// Command contextmatrix-runner receives webhooks from ContextMatrix and
// spawns disposable Docker containers to execute autonomous tasks.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	githubauth "github.com/mhersson/contextmatrix-githubauth"

	"github.com/mhersson/contextmatrix-runner/internal/callback"
	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/container"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/mhersson/contextmatrix-runner/internal/metrics"
	"github.com/mhersson/contextmatrix-runner/internal/preflight"
	"github.com/mhersson/contextmatrix-runner/internal/tracing"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
	"github.com/mhersson/contextmatrix-runner/internal/webhook"
)

// Shutdown timeouts (CTXRUN-040). Declared as package vars so
// main_shutdown_test.go can shrink them to keep the test under 45 s while
// production retains generous margins.
var (
	// httpShutdownTimeout bounds srv.Shutdown: how long the HTTP listener
	// is given to finish draining in-flight requests after we stop
	// accepting new ones.
	httpShutdownTimeout = 10 * time.Second
	// managerDrainTimeout bounds mgr.Wait: how long we wait for container
	// goroutines to finish on their own after we've already asked them to
	// stop. Beyond this deadline we proceed to the force-cleanup phase.
	managerDrainTimeout = 30 * time.Second
	// forceCleanupTimeout bounds each per-container kill during the
	// force-cleanup pass, so one wedged Docker call can't stall shutdown.
	forceCleanupTimeout = 5 * time.Second
)

// broadcasterDropAdapter bridges logbroadcast.DropObserver to the Prometheus
// counter without forcing logbroadcast to import Prometheus.
type broadcasterDropAdapter struct {
	m *metrics.Metrics
}

func (a broadcasterDropAdapter) ObserveDrop() {
	if a.m == nil {
		return
	}

	a.m.BroadcasterDropsTotal.WithLabelValues("all").Inc()
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to configuration file")

	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	logger := newLogger(cfg)

	for _, ref := range cfg.UnpinnedImageRefs {
		logger.Warn("dev profile: accepting unpinned image reference", "field", ref.Field, "image", ref.Image)
	}

	if len(cfg.AppliedDevDefaults) > 0 {
		logger.Info("dev profile: applied defaults", "defaults", cfg.AppliedDevDefaults)
	}

	// Initialize OpenTelemetry. When OTEL_EXPORTER_OTLP_ENDPOINT is unset the
	// provider runs in no-op mode, so local dev needs no collector.
	shutdownTracer, err := tracing.Init(context.Background(), logger)
	if err != nil {
		logger.Error("failed to init tracing", "error", err)
		os.Exit(1)
	}

	// Prometheus metrics bundle. A dedicated registry avoids the global
	// default registry that tests cannot safely share.
	mx := metrics.New()

	// Docker client.
	docker, err := container.NewRealDockerClient()
	if err != nil {
		logger.Error("failed to create Docker client", "error", err)
		os.Exit(1)
	}

	// Select GitHub auth provider based on config.
	var tokenProvider githubauth.TokenGenerator

	switch cfg.GitHub.AuthMode {
	case "app":
		tp, err := githubauth.NewAppProvider(
			cfg.GitHub.App.AppID,
			cfg.GitHub.App.InstallationID,
			cfg.GitHub.App.PrivateKeyPath,
			githubauth.WithAPIBaseURL(cfg.GitHub.APIBaseURL),
		)
		if err != nil {
			slog.Error("failed to construct GitHub App provider", "error", err)
			os.Exit(1)
		}

		tokenProvider = tp
	case "pat":
		tp, err := githubauth.NewPATProvider(cfg.GitHub.PAT.Token)
		if err != nil {
			slog.Error("failed to construct GitHub PAT provider", "error", err)
			os.Exit(1)
		}

		tokenProvider = tp
	default:
		slog.Error("unreachable: invalid auth_mode after Validate()", "value", cfg.GitHub.AuthMode)
		os.Exit(1)
	}

	slog.Info("github token provider initialized", "auth_mode", cfg.GitHub.AuthMode)
	// NOTE: NOT wrapped in CachingProvider — runner mints fresh per spawn
	// (tokens hand off to long-lived worker containers; freshness at delivery matters).

	// Core components.
	defer func() { _ = docker.Close() }()

	trk := tracker.New()
	cb := callback.NewClient(cfg.ContextMatrixURL, cfg.APIKey, logger).WithMetrics(mx)
	cb.SetUseHMACForVerifyAutonomous(cfg.UseHMACForVerifyAutonomous)

	if !cfg.UseHMACForVerifyAutonomous {
		// CTXRUN-048 cross-repo transition knob. Log loudly at startup so
		// operators can't silently run the deprecated Bearer mode forever.
		logger.Warn(
			"Bearer fallback for VerifyAutonomous is deprecated; " +
				"ContextMatrix server must accept HMAC by the next release — " +
				"remove use_hmac_for_verify_autonomous: false once the server is upgraded",
		)
	}

	broadcaster := logbroadcast.NewBroadcaster(logger, broadcasterDropAdapter{m: mx})

	defer func() { _ = broadcaster.Close(context.Background()) }()

	mgr := container.NewManager(docker, trk, cb, tokenProvider, broadcaster, cfg, logger).WithMetrics(mx)

	// HealthState is the shared view of whether preflight has passed and
	// whether a graceful shutdown has started. /readyz reads both flags;
	// the preflight retry loop flips PreflightPassed; CTXRUN-040 will
	// flip Draining during shutdown. It lives in main.go (not as a
	// package-level global) so the whole wiring graph stays trivially
	// swappable.
	health := webhook.NewHealthState()

	// monitorCtx drives the two background loops (preflight retry and
	// dockerd health monitor). It is cancelled on shutdown so the
	// goroutines exit before the HTTP server finishes draining.
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	defer monitorCancel()

	// Run preflight. It runs synchronously for the first attempt so the
	// initial /readyz state reflects reality within a few seconds; on
	// failure it kicks off a background retrier. The runner keeps
	// serving /health and /readyz throughout, so operators can probe
	// what is broken without a restart.
	probes := buildProbes(cfg, docker, tokenProvider, cb)
	preflight.Loop(monitorCtx, probes, &health.PreflightPassed, preflight.DefaultRetryInterval, logger,
		func() { mx.PreflightLastSuccessSec.SetToCurrentTime() })

	// Clean up any orphan containers from a previous crash.
	if err := mgr.CleanupOrphans(context.Background()); err != nil {
		logger.Warn("orphan cleanup failed", "error", err)
	}

	// Background dockerd health monitor. Three consecutive ping failures
	// (90s) call os.Exit(1) so systemd restarts the runner with a fresh
	// Docker SDK client. The docker SDK auto-reconnects for most
	// operations but not all; this is the escape hatch.
	go container.MonitorDockerd(monitorCtx, docker, logger)

	// Background maintenance loop: periodically sweeps orphaned worker
	// containers and prunes dangling images. Closes the M12 gap where
	// CleanupOrphans only ran once at startup and the image cache grew
	// unbounded across worker-image upgrades. See CTXRUN-058.
	go runMaintenanceLoop(monitorCtx, mgr, cfg.MaintenanceInterval, health, logger)

	// Webhook handler.
	webhookSkew := time.Duration(cfg.WebhookReplaySkewSeconds) * time.Second
	wh := webhook.NewHandler(mgr, trk, broadcaster, cb, cfg.APIKey, cfg.MaxConcurrent, cfg.ContainerContextMatrixURL+"/mcp", logger, webhookSkew, health).WithMetrics(mx)

	// Signature-replay and /message idempotency caches. Both run
	// eviction goroutines tied to the main process context so they
	// shut down cleanly alongside the HTTP server.
	replayCtx, replayCancel := context.WithCancel(context.Background())
	defer replayCancel()

	replayCache := webhook.NewReplayCache(
		time.Duration(cfg.WebhookReplaySkewSeconds)*time.Second,
		cfg.WebhookReplayCacheSize,
	)
	messageDedup := webhook.NewMessageDedupCache(
		time.Duration(cfg.MessageDedupTTLSeconds)*time.Second,
		cfg.MessageDedupCacheSize,
	)

	wh.SetReplayCache(replayCache)
	wh.SetMessageDedupCache(messageDedup)

	go replayCache.Run(replayCtx)
	go messageDedup.Run(replayCtx)

	mux := http.NewServeMux()
	wh.Register(mux)

	// HTTP server. otelhttp wraps the whole mux so every request gets a span.
	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           otelhttp.NewHandler(mux, "cmr"),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Admin server: Prometheus /metrics (HMAC-protected) + /ready probe.
	// Bound to 127.0.0.1 only — never exposed publicly.
	adminSrv := buildAdminServer(cfg, wh, mx, trk, logger)

	// Running-containers gauge refresher — don't poll per-add/remove, that
	// would be too chatty and couples the tracker to Prometheus.
	stopGauge := startRunningContainersGauge(trk, mx, 30*time.Second)
	defer stopGauge()

	// Start the main server.
	go func() {
		logger.Info("runner started", "addr", addr)

		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Start the admin server.
	if adminSrv != nil {
		go func() {
			logger.Info("admin server started", "addr", adminSrv.Addr)

			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("admin server error", "error", err)
			}
		}()
	}

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig)

	shutdown(shutdownDeps{
		logger:        logger,
		srv:           srv,
		health:        health,
		tracker:       trk,
		manager:       mgr,
		callback:      cb,
		monitorCancel: monitorCancel,
		replayCancel:  replayCancel,
	})

	// Admin server and tracer shut down on a fresh bounded ctx so a
	// misbehaving collector or admin handler cannot stall the runner exit.
	tearDownCtx, tearDownCancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer tearDownCancel()

	if adminSrv != nil {
		if err := adminSrv.Shutdown(tearDownCtx); err != nil {
			logger.Error("admin server shutdown error", "error", err)
		}
	}

	if err := shutdownTracer(tearDownCtx); err != nil {
		logger.Error("tracer shutdown error", "error", err)
	}

	logger.Info("runner stopped")
}

// managerShutdowner is the subset of *container.Manager the shutdown
// sequence relies on. Factoring it out lets main_shutdown_test.go plug in
// a fake with scripted Kill / Wait / ForceKillContainer behaviour without
// needing the full Manager dependency graph.
type managerShutdowner interface {
	Kill(project, cardID string) error
	Wait()
	ForceKillContainer(ctx context.Context, containerID string) error
}

// callbackReporter is the subset of *callback.Client the shutdown sequence
// invokes. Same rationale as managerShutdowner.
type callbackReporter interface {
	ReportStatus(ctx context.Context, cardID, project, status, message string) error
}

// shutdownDeps bundles everything shutdown() needs. Letting the test
// construct this directly avoids re-running all of main() in tests.
type shutdownDeps struct {
	logger        *slog.Logger
	srv           *http.Server
	health        *webhook.HealthState
	tracker       *tracker.Tracker
	manager       managerShutdowner
	callback      callbackReporter
	monitorCancel context.CancelFunc
	replayCancel  context.CancelFunc
}

// shutdown runs the CTXRUN-040 graceful-shutdown sequence. The order
// matters: we must stop accepting new work before we wait for in-flight
// work to finish, and both must be bounded so one wedged goroutine can't
// stall shutdown until systemd SIGKILLs us (at which point callbacks never
// run).
//
//  1. Flip Draining so /readyz flips to 503 immediately and the load
//     balancer removes us from rotation before step 2 finishes. Our own
//     /trigger/message/promote/end-session handlers also short-circuit to
//     503 on this flag so a request that raced signal delivery doesn't
//     start a container we're about to kill.
//  2. Stop the HTTP listener with a bounded deadline. ListenAndServe
//     returns ErrServerClosed; in-flight handlers finish within
//     httpShutdownTimeout or are forcibly dropped.
//  3. Stop the dockerd monitor and preflight retry loops so an
//     in-flight Ping failure does not os.Exit(1) during shutdown.
//  4. Ask every tracked container to stop (Kill) and report its
//     shutdown status to CM on a detached, bounded ctx.
//  5. Wait for manager goroutines with a deadline. If the deadline fires
//     we log and proceed — the force-cleanup pass below is the backstop
//     for wedged Docker state.
//  6. Force-cleanup: for any container still tracked, kill it directly via
//     a bounded ctx so we don't inherit a hung parent.
func shutdown(d shutdownDeps) {
	// Step 1: flip drain flag.
	if d.health != nil {
		d.health.Draining.Store(true)
		d.logger.Info("draining: /readyz will return 503")
	}

	// Step 2: stop HTTP listener.
	if d.srv != nil {
		httpCtx, httpCancel := context.WithTimeout(context.Background(), httpShutdownTimeout)

		if err := d.srv.Shutdown(httpCtx); err != nil {
			d.logger.Error("server shutdown error", "error", err)
		}

		httpCancel()
	}

	// Step 3: stop background monitors.
	if d.monitorCancel != nil {
		d.monitorCancel()
	}

	// Step 4: ask every container to stop, and tell CM.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), managerDrainTimeout)
	defer shutdownCancel()

	if d.tracker != nil && d.manager != nil {
		for _, info := range d.tracker.AllSnapshots() {
			d.logger.Info("killing container on shutdown", "card_id", info.CardID, "project", info.Project)

			if err := d.manager.Kill(info.Project, info.CardID); err != nil {
				d.logger.Warn("failed to kill container", "card_id", info.CardID, "error", err)
			}

			if d.callback != nil {
				if err := d.callback.ReportStatus(shutdownCtx, info.CardID, info.Project, "failed", "runner shutting down"); err != nil {
					d.logger.Warn("failed to report shutdown status", "card_id", info.CardID, "error", err)
				}
			}
		}
	}

	// Step 5: wait for manager goroutines with a deadline.
	if d.manager != nil {
		drainDone := make(chan struct{})

		go func() {
			d.manager.Wait()
			close(drainDone)
		}()

		select {
		case <-drainDone:
			d.logger.Info("manager goroutines drained cleanly")
		case <-shutdownCtx.Done():
			d.logger.Warn("manager drain timeout reached, proceeding to force-cleanup", "timeout", managerDrainTimeout)
		}
	}

	// Step 6: force-cleanup any container still tracked.
	if d.tracker != nil && d.manager != nil {
		for _, info := range d.tracker.AllSnapshots() {
			if info.ContainerID == "" {
				continue
			}

			forceCtx, forceCancel := context.WithTimeout(context.Background(), forceCleanupTimeout)
			if err := d.manager.ForceKillContainer(forceCtx, info.ContainerID); err != nil {
				d.logger.Warn("force-cleanup failed",
					"container_id", info.ContainerID,
					"card_id", info.CardID,
					"error", err)
			}

			forceCancel()
		}
	}

	// Stop the cache eviction goroutines explicitly.
	if d.replayCancel != nil {
		d.replayCancel()
	}
}

// maintenanceTarget is the subset of *container.Manager that the
// background maintenance loop drives. Factored out so tests can plug in a
// lightweight stub that counts calls without spinning up the full Docker
// mock stack.
type maintenanceTarget interface {
	CleanupOrphans(ctx context.Context) error
	PruneImages(ctx context.Context) error
}

// maintenanceHealth narrows *webhook.HealthState down to what the loop
// reads. Defined as an interface so tests can drive it without reaching
// into the webhook package.
type maintenanceHealth interface {
	// DrainingLoad returns true once graceful shutdown has begun.
	DrainingLoad() bool
}

// healthDrainAdapter wraps *webhook.HealthState so it satisfies
// maintenanceHealth. Kept inline because the adapter is one line of glue
// and doesn't belong in the webhook package.
type healthDrainAdapter struct {
	h *webhook.HealthState
}

func (a healthDrainAdapter) DrainingLoad() bool {
	return a.h != nil && a.h.Draining.Load()
}

// Tunables for the maintenance loop cleanup + prune calls. Exposed as
// package vars so tests can shrink them to keep synthetic scenarios fast.
var (
	maintenanceCleanupTimeout = 30 * time.Second
	maintenancePruneTimeout   = 60 * time.Second
)

// runMaintenanceLoop ticks every interval and runs CleanupOrphans + PruneImages.
// It exits on ctx cancel or on drain. Each tick's Docker call is bounded by a
// fresh per-call timeout so a hung dockerd can't stall the whole loop.
// See CTXRUN-058 (M12, M35).
func runMaintenanceLoop(ctx context.Context, mgr maintenanceTarget, interval time.Duration, health *webhook.HealthState, logger *slog.Logger) {
	runMaintenanceLoopWithHealth(ctx, mgr, interval, healthDrainAdapter{h: health}, logger)
}

func runMaintenanceLoopWithHealth(ctx context.Context, mgr maintenanceTarget, interval time.Duration, health maintenanceHealth, logger *slog.Logger) {
	if interval <= 0 {
		logger.Warn("maintenance loop disabled: non-positive interval", "interval", interval)

		return
	}

	logger.Info("maintenance loop started", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("maintenance loop exiting: context cancelled")

			return
		case <-ticker.C:
			if health != nil && health.DrainingLoad() {
				logger.Info("maintenance loop exiting: draining")

				return
			}

			runMaintenanceTick(ctx, mgr, logger)
		}
	}
}

// runMaintenanceTick executes one pass of CleanupOrphans + PruneImages. Each
// Docker call gets a fresh bounded child of ctx so a wedged daemon cannot
// stall the loop past the next tick.
func runMaintenanceTick(ctx context.Context, mgr maintenanceTarget, logger *slog.Logger) {
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, maintenanceCleanupTimeout)

	if err := mgr.CleanupOrphans(cleanupCtx); err != nil {
		logger.Warn("maintenance: CleanupOrphans failed", "error", err)
	}

	cleanupCancel()

	pruneCtx, pruneCancel := context.WithTimeout(ctx, maintenancePruneTimeout)

	if err := mgr.PruneImages(pruneCtx); err != nil {
		logger.Warn("maintenance: PruneImages failed", "error", err)
	}

	pruneCancel()
}

// buildProbes returns the preflight Probes wired to the real
// dependencies. Kept as a small helper so the preflight package stays
// decoupled from config / client types, and so TestMain-style tests can
// substitute their own Probes struct without going through this helper.
func buildProbes(cfg *config.Config, docker container.DockerClient, tokenProvider githubauth.TokenGenerator, cb *callback.Client) preflight.Probes {
	probes := preflight.Probes{
		DockerPing: docker.Ping,
		GitHubToken: func(ctx context.Context) error {
			_, _, err := tokenProvider.GenerateToken(ctx)

			return err
		},
		ContextMatrixPing: cb.Ping,
	}

	// Image inspect is only meaningful when the runner refuses to pull.
	// Under "always" or "if-not-present" the manager handles missing
	// images at /trigger time, so there is nothing to verify up front.
	if cfg.ImagePullPolicy == config.PullNever {
		probes.ImageInspect = func(ctx context.Context) error {
			_, err := docker.ImageInspect(ctx, cfg.BaseImage)

			return err
		}
	}

	return probes
}

// newLogger builds the process-wide slog.Logger. When LogFormat == "json" the
// handler emits newline-delimited JSON so log collectors can ingest it without
// a parser. The default ("text") preserves the human-readable behaviour.
func newLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevelSlog()}

	if cfg.LogFormat == config.LogFormatJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}

	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// buildAdminServer returns the admin HTTP server, bound to 127.0.0.1 so the
// metrics endpoint is never exposed to the public interface even if firewall
// rules are missing. /metrics is protected by the same HMAC middleware used
// for webhooks; /ready is unauthenticated (it's a probe).
func buildAdminServer(
	cfg *config.Config,
	wh *webhook.Handler,
	mx *metrics.Metrics,
	trk *tracker.Tracker,
	logger *slog.Logger,
) *http.Server {
	port := cfg.AdminPort
	if port == 0 {
		logger.Info("admin endpoints disabled (admin_port=0)")

		return nil
	}

	mux := http.NewServeMux()

	metricsHandler := promhttp.HandlerFor(mx.Registry, promhttp.HandlerOpts{})

	mux.HandleFunc("GET /metrics", wh.AdminAuth(func(w http.ResponseWriter, r *http.Request) {
		metricsHandler.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"running_containers":%d}`, trk.Count())
	})

	logger.Info("admin endpoints registered",
		"port", port,
		"metrics_auth", "hmac",
		"ready_auth", "none",
	)

	return &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
}

// startRunningContainersGauge polls tracker.Count() on a ticker and publishes
// the result to the running-containers gauge. Returns a stop function the
// caller must invoke on shutdown.
func startRunningContainersGauge(trk *tracker.Tracker, mx *metrics.Metrics, interval time.Duration) func() {
	stop := make(chan struct{})
	ticker := time.NewTicker(interval)

	go func() {
		for {
			select {
			case <-stop:
				ticker.Stop()

				return
			case <-ticker.C:
				mx.RunningContainers.Set(float64(trk.Count()))
			}
		}
	}()

	return func() { close(stop) }
}
