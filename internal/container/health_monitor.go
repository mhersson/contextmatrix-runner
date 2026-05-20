package container

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"github.com/mhersson/contextmatrix-runner/internal/metrics"
)

// exitFn is a package-level hook around os.Exit. Tests override it to
// capture the exit code without tearing down the test process. In
// production this is plain os.Exit, so the 3-consecutive-failure path
// terminates the runner and lets systemd restart it from a known-good
// state.
var exitFn = os.Exit

// Health-monitor tunables. 30s probe cadence with a 5s per-probe timeout
// means a single dockerd hiccup cannot exceed one cycle, and three
// failures in a row (90s) are a strong enough signal to justify a
// restart. The counter resets on any success so isolated transient
// failures do not accumulate.
//
// healthProbeIntervalVar and healthProbeTimeoutVar are `var` (not const)
// so tests can shrink them to a few milliseconds without a slow sleep.
// healthFailureLimit stays const — the count is a contract, not a knob.
var (
	healthProbeIntervalVar = 30 * time.Second
	healthProbeTimeoutVar  = 5 * time.Second
)

const healthFailureLimit = 3

// MonitorDockerd runs the health-probe loop without a metrics bundle. Used
// by callers (typically tests) that have no Prometheus registry wired.
// Production callers should prefer MonitorDockerdWithMetrics so the
// per-iteration recover increments cmr_panic_recovered_total.
//
// The function takes a DockerClient (not a *RealDockerClient) so tests
// can swap in a MockDockerClient with a scripted PingFn. A nil logger
// is accepted and replaced with slog.Default() — this is defensive only;
// production wiring should always pass an explicit logger so the restart
// reason is logged with the runner's structured fields.
func MonitorDockerd(ctx context.Context, docker DockerClient, logger *slog.Logger) {
	MonitorDockerdWithMetrics(ctx, docker, logger, nil)
}

// MonitorDockerdWithMetrics runs until ctx is cancelled, pinging dockerd
// every healthProbeInterval. On healthFailureLimit consecutive failures it
// calls exitFn(1) so systemd (or whatever supervisor the operator
// deploys) can restart the runner and recover from pathological SDK
// states the docker SDK's internal reconnect logic does not handle.
//
// A panic inside pingOnce (typically a bug in the Docker SDK or a corrupted
// internal state on a third-party reconnect attempt) used to unwind the
// whole goroutine; main.go's outer recover-and-respawn loop catches that
// but a short window of monitoring is lost. The per-iteration recover
// here keeps the same goroutine alive so the failure counter and ticker
// cadence are preserved across pathological probes. mx may be nil for
// callers that have no Prometheus registry wired (tests).
func MonitorDockerdWithMetrics(ctx context.Context, docker DockerClient, logger *slog.Logger, mx *metrics.Metrics) {
	if logger == nil {
		logger = slog.Default()
	}

	ticker := time.NewTicker(healthProbeIntervalVar)
	defer ticker.Stop()

	failures := 0

	// runOneIteration encapsulates a single select cycle so a panic inside
	// pingOnce (or any Docker SDK callback) is recovered without abandoning
	// the failure counter or the ticker. Returns (exit, exitForce):
	//   - exit=true means MonitorDockerd should return cleanly (ctx done).
	//   - exitForce=true means the failure limit was hit and exitFn(1) was
	//     invoked; the caller returns immediately too.
	//
	// A recovered panic is treated as a probe failure (failures++) and may
	// trip the 3-strike exitFn(1) path. Pre-fix the recover only logged
	// and incremented PanicRecoveredTotal, so a panic-loop in the Docker
	// SDK could panic on every tick forever without supervisor restart.
	runOneIteration := func() (exit bool, exitForce bool) {
		defer func() {
			if r := recover(); r != nil {
				if mx != nil {
					mx.PanicRecoveredTotal.WithLabelValues(metrics.GoroutineDockerdMonitor).Inc()
				}

				failures++

				logger.Error("dockerd monitor iteration panicked; counting as ping failure",
					"panic", r,
					"consecutive_failures", failures,
					"stack", string(debug.Stack()))

				if failures >= healthFailureLimit {
					logger.Error("dockerd monitor panicked repeatedly, exiting for supervisor restart",
						"consecutive_failures", failures)
					exitFn(1)

					exitForce = true
				}
			}
		}()

		select {
		case <-ctx.Done():
			logger.Info("dockerd health monitor exiting", "reason", ctx.Err().Error())

			exit = true

			return exit, exitForce
		case <-ticker.C:
			err := pingOnce(ctx, docker)
			if err == nil {
				if failures > 0 {
					logger.Info("dockerd health recovered", "prior_failures", failures)
				}

				failures = 0

				return exit, exitForce
			}

			// Don't treat a graceful-shutdown ctx.Canceled (or a parent ctx
			// that is already cancelled by the time pingOnce returns) as a
			// dockerd failure. At today's 30s cadence this race is nearly
			// impossible to hit; if anyone lowers the cadence, three
			// canceled probes in a row could otherwise trip exitFn(1) and
			// turn an orderly shutdown into a non-zero exit.
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				logger.Info("dockerd health monitor exiting (ctx canceled mid-probe)")

				exit = true

				return exit, exitForce
			}

			failures++

			logger.Warn("dockerd ping failed",
				"consecutive_failures", failures,
				"error", err.Error(),
			)

			if failures >= healthFailureLimit {
				logger.Error("dockerd ping failed repeatedly, exiting for supervisor restart",
					"consecutive_failures", failures,
					"error", err.Error(),
				)
				exitFn(1)

				exitForce = true

				return exit, exitForce
			}
		}

		return exit, exitForce
	}

	for {
		exit, exitForce := runOneIteration()
		if exit || exitForce {
			return
		}
	}
}

// pingOnce runs a single ping with the health-probe timeout applied.
// Factored out so both the first probe and the confirming probe in the
// failure branch share exact timeout semantics.
func pingOnce(ctx context.Context, docker DockerClient) error {
	probeCtx, cancel := context.WithTimeout(ctx, healthProbeTimeoutVar)
	defer cancel()

	return docker.Ping(probeCtx)
}
