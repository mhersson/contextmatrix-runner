// Package preflight runs the dependency smoke-tests the runner performs at
// startup and periodically until they succeed. A failing preflight keeps
// /readyz returning 503 so the load balancer does not route traffic to an
// unhealthy instance; /health continues to return 200 because the process
// itself is alive.
//
// The package is intentionally dependency-light: the probes themselves are
// supplied via the Probes struct so main.go can wire real clients and tests
// can wire fakes without needing a Docker daemon, a GitHub API, or a
// running ContextMatrix.
package preflight

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// Timeouts for individual probes. Each probe is bounded separately so a
// slow dependency does not starve the others. These values match the card
// contract (docker/image 5s, github 10s, CM TCP dial 5s) and are exposed
// via Probes so tests can shrink them.
const (
	defaultDockerPingTimeout    = 5 * time.Second
	defaultImageInspectTimeout  = 5 * time.Second
	defaultGitHubTokenTimeout   = 10 * time.Second
	defaultContextMatrixTimeout = 5 * time.Second

	// DefaultRetryInterval is the cadence at which Loop retries preflight
	// after an initial failure. 30s balances between catching a dockerd
	// or GitHub recovery reasonably quickly and not hammering CM.
	DefaultRetryInterval = 30 * time.Second
)

// Probes bundles the four dependency checks. Each probe accepts a ctx that
// already has the per-probe timeout applied by Run, so implementations do
// not need to add their own deadlines on top.
type Probes struct {
	// DockerPing verifies dockerd connectivity.
	DockerPing func(ctx context.Context) error

	// ImageInspect verifies the base image is present locally. May be nil
	// when ImagePullPolicy is not "never" — Run skips the check in that
	// case. When non-nil the caller should have pre-bound the image
	// reference so Run does not need to know it.
	ImageInspect func(ctx context.Context) error

	// GitHubToken verifies the configured credential (PAT or App) can
	// mint an installation token.
	GitHubToken func(ctx context.Context) error

	// ContextMatrixPing verifies CM is reachable. A TCP dial is the
	// standard implementation (see callback.Client.Ping) so the check
	// makes no assumption about CM's HTTP routing.
	ContextMatrixPing func(ctx context.Context) error

	// Optional per-probe timeouts. Zero means "use the default constant".
	// Tests set very short values; production leaves them unset.
	DockerTimeout        time.Duration
	ImageInspectTimeout  time.Duration
	GitHubTimeout        time.Duration
	ContextMatrixTimeout time.Duration
}

// Result is the accumulated outcome of a single Run invocation. Every probe
// runs even after an earlier one fails so the first log line captures the
// full dependency state; operators should not need to restart the runner
// three times to discover three broken integrations.
type Result struct {
	DockerErr        error
	ImageInspectErr  error
	GitHubErr        error
	ContextMatrixErr error
}

// OK reports whether every configured probe returned nil.
func (r Result) OK() bool {
	return r.DockerErr == nil &&
		r.ImageInspectErr == nil &&
		r.GitHubErr == nil &&
		r.ContextMatrixErr == nil
}

// Err returns the first non-nil probe error, or nil if all passed.
// Mostly useful as a one-line summary when logging failures.
func (r Result) Err() error {
	switch {
	case r.DockerErr != nil:
		return fmt.Errorf("docker ping: %w", r.DockerErr)
	case r.ImageInspectErr != nil:
		return fmt.Errorf("image inspect: %w", r.ImageInspectErr)
	case r.GitHubErr != nil:
		return fmt.Errorf("github token: %w", r.GitHubErr)
	case r.ContextMatrixErr != nil:
		return fmt.Errorf("contextmatrix ping: %w", r.ContextMatrixErr)
	default:
		return nil
	}
}

// Run executes all configured probes once and returns the consolidated
// Result. Each probe runs with its own timeout derived from Probes (or the
// package defaults). A nil probe is skipped and does not contribute to
// Result — this makes the ImageInspect check opt-in based on the caller's
// pull policy.
func Run(ctx context.Context, p Probes, logger *slog.Logger) Result {
	if logger == nil {
		logger = slog.Default()
	}

	var r Result

	r.DockerErr = runProbe(ctx, "docker_ping", p.DockerPing,
		firstNonZero(p.DockerTimeout, defaultDockerPingTimeout), logger)
	r.ImageInspectErr = runProbe(ctx, "image_inspect", p.ImageInspect,
		firstNonZero(p.ImageInspectTimeout, defaultImageInspectTimeout), logger)
	r.GitHubErr = runProbe(ctx, "github_token", p.GitHubToken,
		firstNonZero(p.GitHubTimeout, defaultGitHubTokenTimeout), logger)
	r.ContextMatrixErr = runProbe(ctx, "contextmatrix_ping", p.ContextMatrixPing,
		firstNonZero(p.ContextMatrixTimeout, defaultContextMatrixTimeout), logger)

	if r.OK() {
		logger.Info("preflight passed")
	} else {
		logger.Error("preflight failed", "error", r.Err().Error())
	}

	return r
}

func runProbe(ctx context.Context, name string, fn func(context.Context) error, timeout time.Duration, logger *slog.Logger) error {
	if fn == nil {
		// A nil probe means the caller has nothing to verify for this
		// slot (e.g. image_inspect when pull policy != "never"). Return
		// nil so Result.OK() is not forced false.
		return nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	err := fn(probeCtx)

	elapsed := time.Since(start)
	if err != nil {
		logger.Info("preflight probe failed",
			"probe", name,
			"duration", elapsed.String(),
			"error", err.Error(),
		)

		return err
	}

	logger.Info("preflight probe ok", "probe", name, "duration", elapsed.String())

	return nil
}

// Loop runs preflight once synchronously. If it passes, it flips the
// passed flag to true and returns without starting a goroutine. If it
// fails, it spawns a background goroutine that re-runs preflight every
// interval until it passes or ctx is cancelled.
//
// This matches the card's shape: the caller (main.go) does not block on a
// dependency recovery — /readyz continues to report 503 — but once the
// flag flips the runner is usable again without a process restart.
//
// onSuccess, if non-nil, is invoked every time the OK-transition happens
// (initial pass or retry recovery). Callers use it to observe success for
// metrics/telemetry without threading those types into this package.
func Loop(ctx context.Context, p Probes, passed *atomic.Bool, interval time.Duration, logger *slog.Logger, onSuccess func()) {
	if logger == nil {
		logger = slog.Default()
	}

	if interval <= 0 {
		interval = DefaultRetryInterval
	}

	if Run(ctx, p, logger).OK() {
		passed.Store(true)

		if onSuccess != nil {
			onSuccess()
		}

		return
	}

	// The first run failed. Start a background retrier. We deliberately
	// do not stash the goroutine in a WaitGroup: the caller owns ctx and
	// cancelling it is the only exit path.
	go retryLoop(ctx, p, passed, interval, logger, onSuccess)
}

func retryLoop(ctx context.Context, p Probes, passed *atomic.Bool, interval time.Duration, logger *slog.Logger, onSuccess func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("preflight retry loop exiting", "reason", ctx.Err().Error())

			return
		case <-ticker.C:
			if Run(ctx, p, logger).OK() {
				passed.Store(true)
				logger.Info("preflight recovered; /readyz will report ok")

				if onSuccess != nil {
					onSuccess()
				}

				return
			}
		}
	}
}

func firstNonZero(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}

	return fallback
}
