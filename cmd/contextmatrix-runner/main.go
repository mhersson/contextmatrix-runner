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
	"runtime/debug"
	"sync"
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

// Shutdown timeouts. Declared as package vars so main_shutdown_test.go can
// shrink them to keep the test under 45 s while production retains generous
// margins.
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
	// tracerShutdownTimeout bounds the span-flush + exporter shutdown at
	// process exit. Smaller than httpShutdownTimeout because the BSP only
	// needs to drain in-memory buffers and the exporter only needs to flush
	// the last batch over the wire — a slow collector should not be
	// allowed to extend total process lifetime by 10 s.
	tracerShutdownTimeout = 5 * time.Second
	// callbackShutdownTimeout bounds each per-container Kill / KillChat /
	// ReportStatus call during the shutdown sweep. A dedicated per-call
	// budget means one slow ContextMatrix callback cannot starve the
	// remaining containers — the previous shape shared one 30 s
	// managerDrainTimeout across every container in a single loop.
	callbackShutdownTimeout = 10 * time.Second
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

	a.m.BroadcasterDropsTotal.Inc()
}

// broadcasterPanicAdapter bridges logbroadcast.PanicObserver to the runner's
// panic_recovered counter so a panic in the broadcaster's drop-reporter
// goroutine bumps the same metric as the main.go-managed background
// goroutines. logbroadcast is a leaf package; the adapter keeps it free of
// Prometheus / metrics imports.
type broadcasterPanicAdapter struct {
	m *metrics.Metrics
}

func (a broadcasterPanicAdapter) ObservePanic(goroutine string) {
	if a.m == nil {
		return
	}

	a.m.PanicRecoveredTotal.WithLabelValues(goroutine).Inc()
}

// recoverBackgroundGoroutine logs and counts a panic that escapes a
// top-level goroutine spawned by main(). Callers must `defer
// recoverBackgroundGoroutine(...)` so a panic in a third-party library
// (Docker SDK, prometheus collector, etc.) does not unwind the runner
// process; an isolated background goroutine should not be able to bring
// down /trigger, /logs, or the admin server.
func recoverBackgroundGoroutine(logger *slog.Logger, mx *metrics.Metrics, label string) {
	r := recover()
	if r == nil {
		return
	}

	if mx != nil {
		mx.PanicRecoveredTotal.WithLabelValues(label).Inc()
	}

	if logger != nil {
		logger.Error("background goroutine panicked",
			"goroutine", label,
			"panic", r,
			"stack", string(debug.Stack()),
		)
	}
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
			logger.Error("failed to construct GitHub App provider", "error", err)
			os.Exit(1)
		}

		tokenProvider = tp
	case "pat":
		tp, err := githubauth.NewPATProvider(cfg.GitHub.PAT.Token)
		if err != nil {
			logger.Error("failed to construct GitHub PAT provider", "error", err)
			os.Exit(1)
		}

		tokenProvider = tp
	default:
		logger.Error("unreachable: invalid auth_mode after Validate()", "value", cfg.GitHub.AuthMode)
		os.Exit(1)
	}

	logger.Info("github token provider initialized", "auth_mode", cfg.GitHub.AuthMode)
	// NOTE: NOT wrapped in CachingProvider — runner mints fresh per spawn
	// (tokens hand off to long-lived worker containers; freshness at delivery matters).

	// Core components.
	trk := tracker.New()
	cb := callback.NewClient(cfg.ContextMatrixURL, cfg.APIKey, logger).WithMetrics(mx)
	cb.SetUseHMACForVerifyAutonomous(cfg.UseHMACForVerifyAutonomous)

	if !cfg.UseHMACForVerifyAutonomous {
		// Cross-repo transition knob. Log at Error level so operators
		// can't silently run the deprecated Bearer mode forever — the
		// fallback ships the shared HMAC secret in an Authorization
		// header, which anyone with access to a single request log can
		// then use to forge signed callbacks in either direction.
		logger.Error(
			"DEPRECATED — secret leaks via Authorization header — set use_hmac_for_verify_autonomous=true; " +
				"the Bearer fallback ships the shared HMAC secret to ContextMatrix as `Authorization: Bearer <apiKey>` " +
				"and will be removed once every deployed CM server accepts HMAC on the autonomous-verify endpoint",
		)
	}

	broadcaster := logbroadcast.NewBroadcasterWithPanicObserver(
		logger,
		broadcasterDropAdapter{m: mx},
		broadcasterPanicAdapter{m: mx},
	)
	mgr := container.NewManager(docker, trk, cb, tokenProvider, broadcaster, cfg, logger).WithMetrics(mx)

	// HealthState is the shared view of whether preflight has passed and
	// whether a graceful shutdown has started. /readyz reads both flags;
	// the preflight retry loop flips PreflightPassed; the shutdown sequence
	// flips Draining. It lives in main.go (not as a package-level global)
	// so the whole wiring graph stays trivially swappable.
	health := webhook.NewHealthState()

	// monitorCtx drives the two background loops (preflight retry and
	// dockerd health monitor). It is cancelled on shutdown so the
	// goroutines exit before the HTTP server finishes draining.
	monitorCtx, monitorCancel := context.WithCancel(context.Background())

	if err := mgr.StartTokenRefresher(monitorCtx); err != nil {
		logger.Error("failed to start token refresher", "error", err)
		monitorCancel()
		os.Exit(1)
	}

	// Deferred cleanups are registered after fatal startup checks to keep
	// all os.Exit calls in main above any defer, satisfying the
	// exitAfterDefer linter rule.
	defer func() { _ = docker.Close() }()
	defer func() {
		// Bound the broadcaster drain. Without a deadline a wedged
		// reporter goroutine (e.g. a misbehaving slog handler) would
		// pin process exit indefinitely. We reuse the tracer-shutdown
		// budget here as a convenient "tail of shutdown" duration —
		// not because the two operations are equivalent (the tracer
		// DOES make a final OTLP/HTTP flush over the network; the
		// broadcaster only drains an in-memory ticker / logger). 5 s is
		// generous for the broadcaster's purely-local work and tight
		// enough that a stuck logger handler cannot extend total
		// process lifetime by more than the tracer's own ceiling. If
		// the broadcaster's needs ever diverge meaningfully, split this
		// off into its own constant rather than reshaping
		// tracerShutdownTimeout's semantics.
		bcCtx, bcCancel := context.WithTimeout(context.Background(), tracerShutdownTimeout)
		defer bcCancel()

		_ = broadcaster.Close(bcCtx)
	}()
	defer monitorCancel()

	// Run preflight. It runs synchronously for the first attempt so the
	// initial /readyz state reflects reality within a few seconds; on
	// failure it kicks off a background retrier. The runner keeps
	// serving /health and /readyz throughout, so operators can probe
	// what is broken without a restart.
	probes := buildProbes(cfg, docker, tokenProvider, cb)
	preflight.Loop(monitorCtx, probes, &health.PreflightPassed, preflight.DefaultRetryInterval, logger,
		func() { mx.PreflightLastSuccessSec.SetToCurrentTime() })

	// Clean up any orphan containers from a previous crash. Bound the
	// boot-time sweep with the same per-tick budget the maintenance loop
	// uses so a wedged dockerd cannot stall boot indefinitely — without a
	// deadline a hung CleanupOrphans call would pin the runner before the
	// HTTP listener ever opens.
	bootCleanupCtx, bootCleanupCancel := context.WithTimeout(context.Background(), maintenanceCleanupTimeout)
	if err := mgr.CleanupOrphans(bootCleanupCtx); err != nil {
		logger.Warn("orphan cleanup failed", "error", err)
	}

	bootCleanupCancel()

	// Background dockerd health monitor. Three consecutive ping failures
	// (90s) call os.Exit(1) so systemd restarts the runner with a fresh
	// Docker SDK client. The docker SDK auto-reconnects for most
	// operations but not all; this is the escape hatch.
	//
	// Wrapped in a recover loop so a panic inside the Docker SDK can never
	// unwind the whole runner. On panic we wait a short backoff and
	// re-spawn the monitor — dockerd health monitoring must stay alive for
	// the lifetime of the process.
	go func() {
		const monitorRespawnBackoff = 5 * time.Second

		for {
			func() {
				defer recoverBackgroundGoroutine(logger, mx, metrics.GoroutineMonitorDockerd)

				container.MonitorDockerdWithMetrics(monitorCtx, docker, logger, mx)
			}()

			// Clean exit on ctx cancel — don't respawn.
			if monitorCtx.Err() != nil {
				return
			}

			// Panic path: pause briefly then respawn so we don't hot-loop.
			select {
			case <-monitorCtx.Done():
				return
			case <-time.After(monitorRespawnBackoff):
			}
		}
	}()

	// Background maintenance loop: periodically sweeps orphaned worker
	// containers and prunes dangling images. Running it periodically (not
	// just once at startup) keeps the image cache from growing unbounded
	// across worker-image upgrades.
	//
	// Same respawn-on-panic discipline as the dockerd monitor — the
	// maintenance loop must keep ticking even if a single Docker SDK call
	// panics inside CleanupOrphans / PruneImages.
	go func() {
		const maintenanceRespawnBackoff = 5 * time.Second

		for {
			func() {
				defer recoverBackgroundGoroutine(logger, mx, metrics.GoroutineMaintenanceLoop)

				runMaintenanceLoop(monitorCtx, mgr, cfg.MaintenanceInterval, health, logger)
			}()

			if monitorCtx.Err() != nil {
				return
			}

			select {
			case <-monitorCtx.Done():
				return
			case <-time.After(maintenanceRespawnBackoff):
			}
		}
	}()

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

	// HTTP server. otelhttp wraps the whole mux so every request gets a
	// span. The default "operation" name "cmr" makes every span opaque in
	// the trace UI; switching to "METHOD /allowed-path" gives operators an
	// actual breakdown by route. metrics.NormalizeEndpoint bounds path
	// cardinality to the same allowlist used by webhook metrics so a probe
	// spamming /nonexistent paths cannot explode trace series.
	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr: addr,
		Handler: otelhttp.NewHandler(mux, "cmr",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + metrics.NormalizeEndpoint(r.URL.Path)
			}),
		),
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
	stopGauge := startRunningContainersGauge(trk, mx, logger, 30*time.Second)
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

	// Install the force-exit watcher BEFORE detaching the first one so SIGINT
	// / SIGTERM is continuously registered. If we called signal.Stop(sigCh)
	// before signal.Notify(forceCh, ...), a signal arriving in the gap would
	// fall back to the default disposition and the process would be killed
	// silently — operators would lose the "second signal forces exit" branch
	// without any log line explaining what happened. The runtime accepts
	// overlapping registrations safely, so layering forceCh on top of sigCh
	// is the safe order.
	forceCh := make(chan os.Signal, 1)
	signal.Notify(forceCh, syscall.SIGTERM, syscall.SIGINT)
	signal.Stop(sigCh)

	// forceDone unblocks the force-exit watcher when shutdown finishes
	// cleanly, so it doesn't outlive the process.
	forceDone := make(chan struct{})
	defer close(forceDone)
	defer signal.Stop(forceCh)

	go func() {
		select {
		case sig := <-forceCh:
			logger.Error("received second signal during shutdown, forcing exit", "signal", sig)
			os.Exit(1)
		case <-forceDone:
		}
	}()

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

	// Allocate a fresh bounded context for each remaining tear-down step.
	// Sharing one budget between adminSrv.Shutdown and shutdownTracer is a
	// silent foot-gun: a slow admin drain leaves the tracer flush with
	// near-zero budget and span batches get dropped without a log line.
	adminCtx, adminCancel := context.WithTimeout(context.Background(), httpShutdownTimeout)

	if adminSrv != nil {
		if err := adminSrv.Shutdown(adminCtx); err != nil {
			logger.Error("admin server shutdown error", "error", err)
		}
	}

	adminCancel()

	// Tracer flush gets its own, smaller budget. 5s is generous for an
	// in-memory BSP flush + a single OTLP/HTTP batch; if the collector is
	// unreachable the exporter will time out internally and the runner
	// proceeds rather than waiting the full 10s of httpShutdownTimeout.
	tracerCtx, tracerCancel := context.WithTimeout(context.Background(), tracerShutdownTimeout)

	if err := shutdownTracer(tracerCtx); err != nil {
		logger.Error("tracer shutdown error", "error", err)
	}

	tracerCancel()

	logger.Info("runner stopped")
}

// managerShutdowner is the subset of *container.Manager the shutdown
// sequence relies on. Factoring it out lets main_shutdown_test.go plug in
// a fake with scripted Kill / Wait / ForceKillContainer behaviour without
// needing the full Manager dependency graph.
type managerShutdowner interface {
	Kill(project, cardID string) error
	KillChat(ctx context.Context, sessionID string) error
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

// shutdown runs the graceful-shutdown sequence. The order matters: we must
// stop accepting new work before we wait for in-flight work to finish, and
// both must be bounded so one wedged goroutine can't stall shutdown until
// systemd SIGKILLs us (at which point callbacks never run).
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
	//
	// We use a top-level shutdownCtx for the manager-drain wait below, but
	// each per-container callback gets its own bounded context so one slow
	// ContextMatrix response cannot drain the budget for the rest of the
	// fleet — without the per-container bound a single 30 s ReportStatus
	// would starve the remaining containers' status callbacks.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), managerDrainTimeout)
	defer shutdownCancel()

	if d.tracker != nil && d.manager != nil {
		for _, info := range d.tracker.AllSnapshots() {
			if info.SessionID != "" {
				d.logger.Info("killing chat container on shutdown", "session_id", info.SessionID)

				killCtx, killCancel := context.WithTimeout(context.Background(), callbackShutdownTimeout)
				if err := d.manager.KillChat(killCtx, info.SessionID); err != nil {
					d.logger.Warn("failed to kill chat container", "session_id", info.SessionID, "error", err)
				}

				killCancel()

				continue
			}

			d.logger.Info("killing container on shutdown", "card_id", info.CardID, "project", info.Project)

			if err := d.manager.Kill(info.Project, info.CardID); err != nil {
				d.logger.Warn("failed to kill container", "card_id", info.CardID, "error", err)
			}

			if d.callback != nil {
				cbCtx, cbCancel := context.WithTimeout(context.Background(), callbackShutdownTimeout)
				if err := d.callback.ReportStatus(cbCtx, info.CardID, info.Project, "failed", "runner shutting down"); err != nil {
					d.logger.Warn("failed to report shutdown status", "card_id", info.CardID, "error", err)
				}

				cbCancel()
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
	SweepStaleChatResumeDirs(maxAge time.Duration)
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
	maintenanceSweepResumeAge = time.Hour
)

// runMaintenanceLoop ticks every interval and runs CleanupOrphans + PruneImages.
// It exits on ctx cancel or on drain. Each tick's Docker call is bounded by a
// fresh per-call timeout so a hung dockerd can't stall the whole loop.
func runMaintenanceLoop(ctx context.Context, mgr maintenanceTarget, interval time.Duration, health *webhook.HealthState, logger *slog.Logger) {
	runMaintenanceLoopWithHealth(ctx, mgr, interval, healthDrainAdapter{h: health}, logger)
}

func runMaintenanceLoopWithHealth(ctx context.Context, mgr maintenanceTarget, interval time.Duration, health maintenanceHealth, logger *slog.Logger) {
	if interval <= 0 {
		// Log once, then block on ctx.Done() so the outer respawn-with-
		// backoff wrapper does not interpret the disabled config as a
		// crash-and-respawn signal. Returning here would spin a fresh
		// "loop disabled" log line every 5 s for the lifetime of the
		// process.
		logger.Warn("maintenance loop disabled: non-positive interval", "interval", interval)

		if ctx != nil {
			<-ctx.Done()
		}

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

// runMaintenanceTick executes one pass of CleanupOrphans + PruneImages +
// SweepStaleChatResumeDirs. Each Docker call gets a fresh bounded child of ctx
// so a wedged daemon cannot stall the loop past the next tick.
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

	mgr.SweepStaleChatResumeDirs(maintenanceSweepResumeAge)
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
		// Parity with the main server so a long-lived keep-alive connection
		// to /metrics (e.g. from a misconfigured Prometheus scraper) does
		// not pin a goroutine indefinitely.
		IdleTimeout: 120 * time.Second,
	}
}

// startRunningContainersGauge polls tracker.Count() on a ticker and publishes
// the result to the running-containers gauge. Returns a stop function the
// caller must invoke on shutdown.
//
// The stop func is idempotent (wrapped in sync.Once) so a double-call from a
// defer plus an explicit shutdown does not panic with "close of closed
// channel".
//
// The ticker is created inside the goroutine so its lifetime is guaranteed to
// match the goroutine's; the previous shape leaked the ticker when the
// goroutine panicked (recoverBackgroundGoroutine recovered and the goroutine
// exited, but `ticker.Stop()` was inside the case body and never ran). The
// poller also wraps itself in a respawn-with-backoff loop matching the
// dockerd / maintenance monitors so a one-off panic inside Prometheus or
// tracker code doesn't permanently stop gauge updates.
func startRunningContainersGauge(trk *tracker.Tracker, mx *metrics.Metrics, logger *slog.Logger, interval time.Duration) func() {
	stop := make(chan struct{})

	// Defensive guard: time.NewTicker panics on a non-positive interval. The
	// outer respawn-with-backoff loop would recover the panic but immediately
	// re-enter the same path and panic again every gaugeRespawnBackoff,
	// pegging a CPU on the recover path for the lifetime of the process.
	// Bail out with a noop stop func instead so a misconfigured caller gets
	// a single warning at boot rather than a continuous panic-respawn spam
	// in the logs.
	if interval <= 0 {
		if logger != nil {
			logger.Warn("running-containers gauge disabled: non-positive interval", "interval", interval)
		}

		return func() {}
	}

	go func() {
		const gaugeRespawnBackoff = 5 * time.Second

		for {
			exited := func() (stopped bool) {
				defer recoverBackgroundGoroutine(logger, mx, metrics.GoroutineRunningContainers)

				ticker := time.NewTicker(interval)
				defer ticker.Stop()

				for {
					select {
					case <-stop:
						return true
					case <-ticker.C:
						mx.RunningContainers.Set(float64(trk.Count()))
					}
				}
			}()

			if exited {
				return
			}

			// Panic path: pause briefly then respawn so we don't hot-loop
			// on a persistent fault. Also respect the stop signal so a
			// shutdown landing during the backoff still exits promptly.
			select {
			case <-stop:
				return
			case <-time.After(gaugeRespawnBackoff):
			}
		}
	}()

	var once sync.Once

	return func() { once.Do(func() { close(stop) }) }
}
