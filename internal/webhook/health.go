package webhook

import (
	"net/http"
	"sync/atomic"
)

// HealthState holds the two flags that control /readyz output.
//
//   - PreflightPassed is flipped by the startup preflight (and its retry
//     loop) once all dependency probes succeed. It starts false and must
//     reach true before the runner reports ready.
//   - Draining is flipped by the shutdown sequence (CTXRUN-040) so the
//     process can continue serving in-flight requests while signalling to
//     the load balancer that no new traffic should be routed. This card
//     only exposes the field; flipping it is CTXRUN-040's job.
//
// Both flags are atomic.Bool so readers (the /readyz handler) do not need
// to coordinate with writers (preflight retry loop, shutdown hook).
type HealthState struct {
	PreflightPassed atomic.Bool
	Draining        atomic.Bool
}

// NewHealthState returns a HealthState with both flags set to false.
func NewHealthState() *HealthState {
	return &HealthState{}
}

// readyResponse matches the /readyz body shape:
//
//	{"ok":true}                       // 200 when preflight passed and not draining
//	{"ok":false,"reason":"preflight"} // 503 when preflight has not passed
//	{"ok":false,"reason":"draining"}  // 503 when a graceful shutdown is in progress
type readyResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// handleReadyz is the runner's readiness probe. Unlike /health (liveness),
// /readyz tells the load balancer whether this instance should receive new
// traffic. It requires no HMAC because it is consumed by infra, not CM.
//
// Draining is checked before PreflightPassed so that a shutdown-in-progress
// runner reports "draining" rather than silently reverting to "preflight"
// if the retry loop were racing the shutdown hook.
func (h *Handler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if h.health == nil {
		// Defensive: if the handler was constructed without a HealthState
		// (older test fixture, or a future caller that forgot to wire
		// it), report ready so we do not break existing tests that only
		// care about authenticated endpoints. Production wiring always
		// supplies a HealthState.
		writeJSON(w, http.StatusOK, readyResponse{OK: true})

		return
	}

	if h.health.Draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, readyResponse{OK: false, Reason: "draining"})

		return
	}

	if !h.health.PreflightPassed.Load() {
		writeJSON(w, http.StatusServiceUnavailable, readyResponse{OK: false, Reason: "preflight"})

		return
	}

	writeJSON(w, http.StatusOK, readyResponse{OK: true})
}
