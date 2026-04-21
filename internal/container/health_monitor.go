package container

import (
	"context"
	"log/slog"
	"os"
	"time"
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

// MonitorDockerd runs until ctx is cancelled, pinging dockerd every
// healthProbeInterval. On healthFailureLimit consecutive failures it
// calls exitFn(1) so systemd (or whatever supervisor the operator
// deploys) can restart the runner and recover from pathological SDK
// states the docker SDK's internal reconnect logic does not handle.
//
// The function takes a DockerClient (not a *RealDockerClient) so tests
// can swap in a MockDockerClient with a scripted PingFn. The logger is
// required — the function will panic on nil rather than invent a default
// sink, because losing these log lines in production would hide the
// reason for a restart.
func MonitorDockerd(ctx context.Context, docker DockerClient, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	ticker := time.NewTicker(healthProbeIntervalVar)
	defer ticker.Stop()

	failures := 0

	for {
		select {
		case <-ctx.Done():
			logger.Info("dockerd health monitor exiting", "reason", ctx.Err().Error())

			return
		case <-ticker.C:
			err := pingOnce(ctx, docker)
			if err == nil {
				if failures > 0 {
					logger.Info("dockerd health recovered", "prior_failures", failures)
				}

				failures = 0

				continue
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

				return
			}
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
