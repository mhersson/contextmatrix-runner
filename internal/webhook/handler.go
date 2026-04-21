package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/mhersson/contextmatrix-runner/internal/callback"
	"github.com/mhersson/contextmatrix-runner/internal/container"
	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/mhersson/contextmatrix-runner/internal/metrics"
	"github.com/mhersson/contextmatrix-runner/internal/streammsg"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// CTXRUN-060 note: threading the raw body through context.Value is an
// accepted anti-pattern (context-as-data rather than request-scoped
// cancellation/deadline). A future refactor should pass the body to each
// handler via an explicit argument or a bodyHandler wrapper type. Deferring
// here because it touches every handler signature (six handlers) and the
// polish-sweep scope is supposed to stay low-risk.
type bodyKey struct{}

// ContainerRunner is the subset of container.Manager used by the webhook handler.
// Using an interface here enables handler tests to inject fakes without needing
// the Docker daemon or the full Manager dependency graph.
type ContainerRunner interface {
	Run(ctx context.Context, payload container.RunConfig)
	Kill(project, cardID string) error
}

// Handler processes incoming webhooks from ContextMatrix.
type Handler struct {
	manager         ContainerRunner
	tracker         *tracker.Tracker
	broadcaster     *logbroadcast.Broadcaster
	cmClient        *callback.Client // contextmatrix callback client for promote API call
	apiKey          string
	maxConcurrent   int
	allowedMCPHosts []string
	logger          *slog.Logger
	metrics         *metrics.Metrics

	// replayCache rejects previously-seen HMAC signatures; messageDedup
	// returns the original ack for a repeated (project, card_id, message_id).
	// Both are optional — if nil the corresponding protection is disabled,
	// which keeps existing handler tests compiling without a cache fixture.
	replayCache  *ReplayCache
	messageDedup *MessageDedupCache

	// health drives the /readyz endpoint. Optional for tests; nil means
	// /readyz reports ready unconditionally (see handleReadyz).
	health *HealthState
}

// NewHandler creates a webhook handler.
//
// health is optional — pass nil in tests that do not exercise /readyz. In
// production wiring (cmd/contextmatrix-runner/main.go), the same
// *HealthState is also held by the preflight retry loop and by the
// shutdown sequence so all three components observe and/or flip the same
// atomic flags.
func NewHandler(
	manager ContainerRunner,
	tracker *tracker.Tracker,
	broadcaster *logbroadcast.Broadcaster,
	cmClient *callback.Client,
	apiKey string,
	maxConcurrent int,
	allowedMCPHosts []string,
	logger *slog.Logger,
	health *HealthState,
) *Handler {
	return &Handler{
		manager:         manager,
		tracker:         tracker,
		broadcaster:     broadcaster,
		cmClient:        cmClient,
		apiKey:          apiKey,
		maxConcurrent:   maxConcurrent,
		allowedMCPHosts: allowedMCPHosts,
		logger:          logger,
		health:          health,
	}
}

// SetReplayCache wires in the signature-replay cache used by the HMAC
// middleware. Pass nil to disable replay protection (used by tests).
// Must be called before Register so every route gets the protected
// middleware wrapper.
func (h *Handler) SetReplayCache(c *ReplayCache) {
	h.replayCache = c
}

// SetMessageDedupCache wires in the /message idempotency cache. Pass
// nil to disable dedup (used by tests).
func (h *Handler) SetMessageDedupCache(c *MessageDedupCache) {
	h.messageDedup = c
}

// WithMetrics attaches a metrics bundle used by the /trigger saturation log
// and by the request middleware in Register.
func (h *Handler) WithMetrics(m *metrics.Metrics) *Handler {
	h.metrics = m

	return h
}

// isDraining reports whether a graceful shutdown has started. Handlers that
// begin or extend long-running work (/trigger, /message, /promote,
// /end-session) check this first and return 503 so the shutdown sequence
// can finish without CM pushing more work onto a draining runner. /kill,
// /stop-all, /logs, /health, /readyz intentionally remain reachable during
// drain — they either finish quickly or surface state we want operators to
// be able to read.
func (h *Handler) isDraining() bool {
	return h.health != nil && h.health.Draining.Load()
}

// Register adds all webhook routes to the mux.
//
// Every registered route is wrapped in correlation-ID + metrics middleware so
// the full chain is: correlation → metrics → hmacAuth → handler. The SSE
// /logs endpoint is wrapped too (its metric just shows one long request).
func (h *Handler) Register(mux *http.ServeMux) {
	wrap := func(handler http.HandlerFunc, authed bool) http.Handler {
		var chained http.Handler = handler
		if authed {
			chained = h.hmacAuth(handler)
		}

		chained = withMetrics(h.metrics, chained)
		chained = withCorrelation(chained)

		return chained
	}

	mux.Handle("POST /trigger", wrap(h.handleTrigger, true))
	mux.Handle("POST /kill", wrap(h.handleKill, true))
	mux.Handle("POST /stop-all", wrap(h.handleStopAll, true))
	mux.Handle("POST /message", wrap(h.handleMessage, true))
	mux.Handle("POST /promote", wrap(h.handlePromote, true))
	mux.Handle("POST /end-session", wrap(h.handleEndSession, true))
	mux.Handle("GET /logs", wrap(h.handleLogs, true))
	mux.Handle("GET /health", wrap(h.handleHealth, false))
	// /readyz is deliberately unauthenticated: it is a readiness probe
	// consumed by orchestrators / load balancers, not by CM. It returns
	// 200 only when preflight has passed and the runner is not draining.
	mux.Handle("GET /readyz", wrap(h.handleReadyz, false))
}

func (h *Handler) handleTrigger(w http.ResponseWriter, r *http.Request) {
	// CTXRUN-040: short-circuit during graceful shutdown. The readiness
	// probe flipped to 503 in step 1 of the shutdown sequence, but a
	// /trigger that raced signal delivery and the load-balancer pull
	// could still land here before we stop accepting new HTTP traffic.
	// Refuse it explicitly so we don't start a container we're about to
	// kill.
	if h.isDraining() {
		writeError(w, http.StatusServiceUnavailable, CodeDraining, "runner is draining")

		return
	}

	body := r.Context().Value(bodyKey{}).([]byte)

	var payload TriggerPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logDebug("trigger: invalid JSON", "error", err)
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")

		return
	}

	if err := ValidatePayload(&payload, h.allowedMCPHosts); err != nil {
		writeValidationError(w, err)

		return
	}

	// Start a span that covers the synchronous half of /trigger — admission
	// check + Manager.Run kickoff. The detached ctx below deliberately does
	// not inherit the request cancellation (the container must outlive the
	// HTTP request), so the span is ended explicitly at the end of this
	// handler.
	_, span := otel.Tracer("cmr").Start(r.Context(), "webhook.trigger")
	span.SetAttributes(
		attribute.String("card_id", payload.CardID),
		attribute.String("project", payload.Project),
		attribute.Bool("interactive", payload.Interactive),
	)

	defer span.End()

	ctx, cancel := context.WithCancel(context.Background())

	err := h.tracker.AddIfUnderLimit(&tracker.ContainerInfo{
		CardID:    payload.CardID,
		Project:   payload.Project,
		Image:     payload.RunnerImage,
		StartedAt: time.Now(),
		Cancel:    cancel,
	}, h.maxConcurrent)
	if err != nil {
		cancel()

		switch {
		case errors.Is(err, tracker.ErrLimitReached):
			// M13: emit a visible saturation signal so operators notice
			// repeated 429s and can scale concurrency or capacity.
			if h.logger != nil {
				h.logger.Warn("trigger rejected: runner saturated",
					"card_id", payload.CardID,
					"project", payload.Project,
					"limit", h.maxConcurrent,
					"correlation_id", correlationIDFromContext(r.Context()),
				)
			}

			writeError(w, http.StatusTooManyRequests, CodeLimitReached, "concurrency limit reached")
		case errors.Is(err, tracker.ErrAlreadyTracked):
			// Generic 409 — do NOT reveal whether the collision is the same
			// card_id or a different one (M21 in REVIEW.md: the old
			// "task already running: <card_id>" leaked tracker state).
			h.logDebug("trigger: card already tracked",
				"card_id", payload.CardID, "project", payload.Project)
			writeError(w, http.StatusConflict, CodeConflict, "conflicting container state")
		default:
			h.logWarn("trigger: tracker add failed", "error", err.Error())
			writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")
		}

		return
	}

	h.manager.Run(ctx, container.RunConfig{
		CardID:        payload.CardID,
		Project:       payload.Project,
		RepoURL:       payload.RepoURL,
		MCPURL:        payload.MCPURL,
		MCPAPIKey:     payload.MCPAPIKey,
		BaseBranch:    payload.BaseBranch,
		RunnerImage:   payload.RunnerImage,
		Interactive:   payload.Interactive,
		Model:         payload.Model,
		CorrelationID: correlationIDFromContext(r.Context()),
	})

	writeSuccess(w, http.StatusAccepted, "container starting")
}

func (h *Handler) handleKill(w http.ResponseWriter, r *http.Request) {
	body := r.Context().Value(bodyKey{}).([]byte)

	var payload KillPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logDebug("kill: invalid JSON", "error", err)
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")

		return
	}

	if err := ValidatePayload(&payload, h.allowedMCPHosts); err != nil {
		writeValidationError(w, err)

		return
	}

	// /kill is idempotent: a Kill call on a container that is already stopped
	// (or never existed) is a no-op. M8-era callers fingerprinted the
	// "not found" reason on a 404; we now return 200 with a stable message
	// so CM can retry cleanly and distinguish success from error on status
	// alone.
	if !h.tracker.Has(payload.Project, payload.CardID) {
		h.logDebug("kill: container not tracked (idempotent no-op)",
			"card_id", payload.CardID, "project", payload.Project)
		writeSuccess(w, http.StatusOK, "no-op (already stopped)")

		return
	}

	if err := h.manager.Kill(payload.Project, payload.CardID); err != nil {
		h.logWarn("kill: manager.Kill failed",
			"card_id", payload.CardID, "project", payload.Project, "error", err.Error())
		writeError(w, http.StatusInternalServerError, CodeInternal, "kill failed")

		return
	}

	writeSuccess(w, http.StatusOK, "container killed")
}

func (h *Handler) handleStopAll(w http.ResponseWriter, r *http.Request) {
	body := r.Context().Value(bodyKey{}).([]byte)

	var payload StopAllPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logDebug("stop-all: invalid JSON", "error", err)
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")

		return
	}

	if err := ValidatePayload(&payload, h.allowedMCPHosts); err != nil {
		writeValidationError(w, err)

		return
	}

	var containers []tracker.ContainerSnapshot
	if payload.Project != "" {
		containers = h.tracker.ListSnapshotsByProject(payload.Project)
	} else {
		containers = h.tracker.AllSnapshots()
	}

	results := make([]CardKillResult, 0, len(containers))
	stopped := 0
	failed := 0

	for _, info := range containers {
		if err := h.manager.Kill(info.Project, info.CardID); err != nil {
			h.logWarn("stop-all: kill failed",
				"card_id", info.CardID, "project", info.Project, "error", err.Error())

			results = append(results, CardKillResult{
				CardID:  info.CardID,
				Project: info.Project,
				OK:      false,
				Error:   "kill failed",
			})
			failed++

			continue
		}

		results = append(results, CardKillResult{
			CardID:  info.CardID,
			Project: info.Project,
			OK:      true,
		})
		stopped++
	}

	// 207 Multi-Status if any per-card kill failed; 200 OK otherwise. M40 in
	// REVIEW.md flagged that the old handler returned 200 even when every
	// kill failed — the new shape surfaces the failure in both the status
	// code and the OK flag so callers can branch on either.
	status := http.StatusOK
	ok := true

	if failed > 0 {
		status = http.StatusMultiStatus
		ok = false
	}

	writeJSON(w, status, StopAllResponse{
		OK:      ok,
		Total:   len(containers),
		Stopped: stopped,
		Failed:  failed,
		Results: results,
	})
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"running_containers": h.tracker.Count(),
	})
}

// maxMessageContent is the maximum allowed byte length for a user message.
const maxMessageContent = 8192

// handleMessage accepts a user chat message and writes it to the target
// container's stdin as a Claude Code stream-json user turn.
func (h *Handler) handleMessage(w http.ResponseWriter, r *http.Request) {
	// CTXRUN-040: refuse new work during shutdown so a stdin write doesn't
	// race the /end-session close that shutdown will trigger anyway.
	if h.isDraining() {
		writeError(w, http.StatusServiceUnavailable, CodeDraining, "runner is draining")

		return
	}

	body := r.Context().Value(bodyKey{}).([]byte)

	var payload MessagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logDebug("message: invalid JSON", "error", err)
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")

		return
	}

	// Size cap first: an oversize content produces 413 rather than 400 to
	// match the documented webhook contract. ValidatePayload would otherwise
	// fold this into a 400.
	if len(payload.Content) > maxMessageContent {
		writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "content exceeds 8192 bytes")

		return
	}

	if err := ValidatePayload(&payload, h.allowedMCPHosts); err != nil {
		writeValidationError(w, err)

		return
	}

	// Idempotency: if this (project, card_id, message_id) has already been
	// served successfully, replay the stored ack verbatim. This sits on
	// top of the signature-replay cache: signatures only dedup inside the
	// 5-minute HMAC window, while message_id dedup protects the full
	// message_dedup_ttl (default 10 minutes). A client retrying a
	// network-timed-out POST deserves the original 202, not a 409.
	if h.messageDedup != nil && payload.MessageID != "" {
		if ack, ok := h.messageDedup.Get(payload.Project, payload.CardID, payload.MessageID); ok {
			writeRawAck(w, ack)

			return
		}
	}

	// 404 if no container is tracked for this (project, card_id).
	if !h.tracker.Has(payload.Project, payload.CardID) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no container tracked")

		return
	}

	// Build the Claude Code stream-json user message.
	b, err := streammsg.BuildUserMessage(payload.Content)
	if err != nil {
		h.logWarn("message: BuildUserMessage failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")

		return
	}

	// Write to stdin. Branch on sentinel so we can map to the right status
	// code: 404 TOCTOU, 409 non-interactive, 410 session-ended.
	// Publish only after a successful write so no phantom echo appears in
	// the browser on a non-interactive container.
	if err := h.tracker.WriteStdin(payload.Project, payload.CardID, b); err != nil {
		switch {
		case errors.Is(err, tracker.ErrNotTracked):
			// TOCTOU: entry was Removed between Has and WriteStdin.
			writeError(w, http.StatusNotFound, CodeNotFound, "no container tracked")
		case errors.Is(err, tracker.ErrStdinClosed):
			// M39: session has ended (stdin was attached, then closed by
			// /end-session, CloseStdin, or Remove). 410 Gone tells the
			// caller the resource is permanently unavailable — no point
			// retrying.
			writeError(w, http.StatusGone, CodeStdinClosed, "session ended")
		case errors.Is(err, tracker.ErrNoStdinAttached):
			// Container is tracked but was never interactive. Client
			// requested /message on a non-HITL session.
			writeError(w, http.StatusConflict, CodeConflict, "container is not in interactive mode")
		default:
			h.logWarn("message: stdin write failed",
				"card_id", payload.CardID, "project", payload.Project, "error", err.Error())
			writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")
		}

		return
	}

	if h.broadcaster != nil {
		h.broadcaster.Publish(logbroadcast.LogEntry{
			Timestamp: time.Now(),
			CardID:    payload.CardID,
			Project:   payload.Project,
			Type:      "user",
			Content:   payload.Content,
		})
	}

	// Marshal the success ack once so we can cache it byte-identically
	// for retries. Any marshal error here is an internal bug, not a
	// client problem — fall back to the standard writeJSON path.
	ackBytes, err := json.Marshal(SuccessResponse{OK: true, MessageID: payload.MessageID})
	if err != nil {
		writeJSON(w, http.StatusAccepted, SuccessResponse{OK: true, MessageID: payload.MessageID})

		return
	}

	if h.messageDedup != nil && payload.MessageID != "" {
		h.messageDedup.Put(payload.Project, payload.CardID, payload.MessageID, CachedAck{
			Status: http.StatusAccepted,
			Body:   ackBytes,
		})
	}

	writeRawAck(w, CachedAck{Status: http.StatusAccepted, Body: ackBytes})
}

// writeRawAck writes the stored ack body verbatim so a retry sees the
// byte-identical response the first call returned.
func writeRawAck(w http.ResponseWriter, ack CachedAck) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ack.Status)
	_, _ = w.Write(ack.Body)
}

const autonomousContent = "Autonomous mode has been enabled (card flag flipped). Check the card with `get_card` at your next gate and continue on the autonomous branch. Do not wait for further user input."

// handlePromote switches a running interactive session to autonomous mode.
// It verifies via a read-only GET that CM has already set the card's autonomous
// flag before writing the canned stdin message. Using GET (not POST) prevents
// re-triggering the webhook and breaking an infinite promote loop. If the flag
// is not confirmed, it returns an error and does NOT write to stdin (fail closed).
func (h *Handler) handlePromote(w http.ResponseWriter, r *http.Request) {
	// CTXRUN-040: refuse new work during shutdown.
	if h.isDraining() {
		writeError(w, http.StatusServiceUnavailable, CodeDraining, "runner is draining")

		return
	}

	body := r.Context().Value(bodyKey{}).([]byte)

	var payload PromotePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logDebug("promote: invalid JSON", "error", err)
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")

		return
	}

	if err := ValidatePayload(&payload, h.allowedMCPHosts); err != nil {
		writeValidationError(w, err)

		return
	}

	// 404 if no container is tracked for this (project, card_id).
	if !h.tracker.Has(payload.Project, payload.CardID) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no container tracked")

		return
	}

	// Verify that CM has already flipped the autonomous flag via a read-only GET.
	// Using GET (not POST) avoids re-triggering the webhook and breaking the
	// infinite promote loop. Fail closed: refuse to write stdin unless CM
	// confirms autonomous=true.
	if h.cmClient != nil {
		autonomous, err := h.cmClient.VerifyAutonomous(r.Context(), payload.Project, payload.CardID)
		if err != nil {
			h.logVerifyAutonomousFailure(payload, err)
			writeUpstreamUnavailable(w)

			return
		}

		if !autonomous {
			if h.logger != nil {
				h.logger.Warn("promote rejected: card autonomous flag is not set on contextmatrix",
					"card_id", payload.CardID,
					"project", payload.Project,
				)
			}

			writeError(w, http.StatusForbidden, CodeConflict, "card autonomous flag is not set on contextmatrix")

			return
		}
	}

	// Publish system LogEntry BEFORE the stdin write so the browser sees the
	// mode switch in the correct order.
	if h.broadcaster != nil {
		h.broadcaster.Publish(logbroadcast.LogEntry{
			Timestamp: time.Now(),
			CardID:    payload.CardID,
			Project:   payload.Project,
			Type:      "system",
			Content:   "promoted to autonomous mode",
		})
	}

	b, err := streammsg.BuildUserMessage(autonomousContent)
	if err != nil {
		h.logWarn("promote: BuildUserMessage failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")

		return
	}

	if err := h.tracker.WriteStdin(payload.Project, payload.CardID, b); err != nil {
		switch {
		case errors.Is(err, tracker.ErrNotTracked):
			writeError(w, http.StatusNotFound, CodeNotFound, "no container tracked")
		case errors.Is(err, tracker.ErrStdinClosed):
			writeError(w, http.StatusGone, CodeStdinClosed, "session ended")
		case errors.Is(err, tracker.ErrNoStdinAttached):
			writeError(w, http.StatusConflict, CodeConflict, "container is not in interactive mode")
		default:
			h.logWarn("promote: stdin write failed",
				"card_id", payload.CardID, "project", payload.Project, "error", err.Error())
			writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")
		}

		return
	}

	// Close stdin so the container's claude process receives EOF and exits
	// cleanly. An already-closed stdin (e.g. a racing /end-session) is not a
	// failure of /promote — log a warning and still return 200.
	if err := h.tracker.CloseStdin(payload.Project, payload.CardID); err != nil {
		h.logWarn("promote: close stdin after write failed (non-fatal)",
			"card_id", payload.CardID, "project", payload.Project, "error", err.Error())
	}

	writeSuccess(w, http.StatusAccepted, "")
}

// handleEndSession closes the stdin of a tracked interactive container so
// claude receives EOF and exits. The tracker entry is left in place; the
// normal waitAndCleanup flow removes it when the container exits.
func (h *Handler) handleEndSession(w http.ResponseWriter, r *http.Request) {
	// CTXRUN-040: refuse new work during shutdown. The shutdown sequence
	// kills every tracked container anyway, so a stray /end-session
	// landing mid-drain would race the container teardown.
	if h.isDraining() {
		writeError(w, http.StatusServiceUnavailable, CodeDraining, "runner is draining")

		return
	}

	body := r.Context().Value(bodyKey{}).([]byte)

	var payload EndSessionPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logDebug("end-session: invalid JSON", "error", err)
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")

		return
	}

	if err := ValidatePayload(&payload, h.allowedMCPHosts); err != nil {
		writeValidationError(w, err)

		return
	}

	if !h.tracker.Has(payload.Project, payload.CardID) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no container tracked")

		return
	}

	if err := h.tracker.CloseStdin(payload.Project, payload.CardID); err != nil {
		switch {
		case errors.Is(err, tracker.ErrNotTracked):
			// TOCTOU: the entry was Removed between our Has and CloseStdin.
			// Same semantic as a 404 — nothing to close.
			writeError(w, http.StatusNotFound, CodeNotFound, "no container tracked")
		case errors.Is(err, tracker.ErrNoStdinAttached):
			writeError(w, http.StatusConflict, CodeConflict, "container is not in interactive mode")
		default:
			h.logWarn("end-session: close stdin failed",
				"card_id", payload.CardID, "project", payload.Project, "error", err.Error())
			writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")
		}

		return
	}

	if h.broadcaster != nil {
		h.broadcaster.Publish(logbroadcast.LogEntry{
			Timestamp: time.Now(),
			CardID:    payload.CardID,
			Project:   payload.Project,
			Type:      "system",
			Content:   "session ended (stdin closed)",
		})
	}

	writeSuccess(w, http.StatusAccepted, "")
}

// handleLogs streams log entries via Server-Sent Events (SSE).
// An optional ?project= query parameter filters entries by project name.
func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternal, "streaming not supported")

		return
	}

	project := r.URL.Query().Get("project")

	// Clear the write deadline — the server has a 30s WriteTimeout that would
	// otherwise terminate the long-lived SSE connection. This depends on every
	// middleware in the chain exposing Unwrap() so http.NewResponseController
	// can reach the underlying conn; logged at WARN if it ever fails again.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		if h.logger != nil {
			h.logger.Warn("SSE could not clear write deadline; connection will drop on WriteTimeout",
				"error", err)
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe before writing ": connected" so receiving that line is a
	// client-observable guarantee that the subscription is live.
	ch, unsubscribe := h.broadcaster.Subscribe(project)
	defer unsubscribe()

	// Flush headers and send initial keepalive to trigger client onopen.
	flusher.Flush()

	if _, err := fmt.Fprintf(w, ": connected\n\n"); err != nil {
		if h.logger != nil {
			h.logger.Debug("SSE initial write failed", "error", err)
		}

		return
	}

	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	if h.logger != nil {
		h.logger.Info("SSE log client connected",
			"project_filter", project,
			"remote_addr", r.RemoteAddr,
		)
	}

	for {
		select {
		case <-r.Context().Done():
			if h.logger != nil {
				h.logger.Info("SSE log client disconnected",
					"project_filter", project,
					"remote_addr", r.RemoteAddr,
				)
			}

			return

		case <-ticker.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				if h.logger != nil {
					h.logger.Debug("SSE keepalive write failed", "error", err)
				}

				return
			}

			flusher.Flush()

		case entry, ok := <-ch:
			if !ok {
				return
			}

			data, err := json.Marshal(entry)
			if err != nil {
				if h.logger != nil {
					h.logger.Debug("SSE marshal failed", "error", err)
				}

				continue
			}

			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				if h.logger != nil {
					h.logger.Debug("SSE event write failed", "error", err)
				}

				return
			}

			flusher.Flush()
		}
	}
}

// AdminAuth exposes the HMAC-verifier middleware for use outside the webhook
// mux (e.g. the admin /metrics endpoint). Kept as a named method so the
// authentication chain used for the metrics endpoint is the same one used for
// /trigger — making an auth bypass on one impossible without the other.
func (h *Handler) AdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return h.hmacAuth(next)
}

// hmacAuth is middleware that verifies HMAC signatures on incoming requests.
//
// M8 in REVIEW.md: every authentication failure returns a single fixed 401
// body with {code: "unauthorized", message: "unauthorized"}. The specific
// reason (missing header, bad signature, expired timestamp, unreadable body)
// is logged server-side at Debug level but never echoed to the client, so a
// scanner cannot fingerprint which step failed.
//
// Replay rejection (a post-authentication condition) still returns the
// distinct 409/duplicate shape — it is not an authentication failure, so
// collapsing it into the generic 401 would lose diagnostic signal for CM.
func (h *Handler) hmacAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sigHeader := r.Header.Get(cmhmac.SignatureHeader)
		if sigHeader == "" {
			h.logDebug("hmac auth: missing signature header", "remote_addr", r.RemoteAddr)
			writeUnauthorized(w)

			return
		}

		tsHeader := r.Header.Get(cmhmac.TimestampHeader)
		if tsHeader == "" {
			h.logDebug("hmac auth: missing timestamp header", "remote_addr", r.RemoteAddr)
			writeUnauthorized(w)

			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			// A truncated / unreadable body is indistinguishable (to a
			// client) from a signature mismatch — both surface as 401.
			h.logDebug("hmac auth: body read failed", "remote_addr", r.RemoteAddr, "error", err.Error())
			writeUnauthorized(w)

			return
		}

		sig := strings.TrimPrefix(sigHeader, "sha256=")
		if !cmhmac.VerifySignatureWithTimestamp(h.apiKey, sig, tsHeader, body, cmhmac.DefaultMaxClockSkew) {
			h.logWarn("webhook authentication failed", "remote_addr", r.RemoteAddr)
			writeUnauthorized(w)

			return
		}

		// Replay protection: reject any signature we've already seen
		// inside the skew window. 409 is used to signal "I authenticated
		// you but this exact signed request already landed" — distinct
		// from 401 (unauthenticated) so CM can log/metric them apart.
		if h.replayCache != nil && h.replayCache.Seen(sig) {
			if h.metrics != nil {
				h.metrics.ReplayCacheHitsTotal.Inc()
			}

			h.logWarn("webhook replay rejected",
				"remote_addr", r.RemoteAddr,
				"path", r.URL.Path,
			)

			writeError(w, http.StatusConflict, CodeDuplicate, "duplicate request")

			return
		}

		ctx := context.WithValue(r.Context(), bodyKey{}, body)
		next(w, r.WithContext(ctx))
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	// CTXRUN-059 (L13): json.NewEncoder allocates an encoder per call; for
	// the short webhook responses we serialize here, json.Marshal + a
	// single Write is both cheaper and trims the trailing newline that
	// Encoder adds. If Marshal fails we fall back to a fixed error body so
	// the client still gets a well-formed JSON response.
	w.Header().Set("Content-Type", "application/json")

	body, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"code":"internal","message":"response marshal failed"}`))

		return
	}

	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeSuccess serialises a SuccessResponse. /message acks bypass this
// helper: they build a SuccessResponse with MessageID set and then pass the
// marshalled bytes through the dedup cache via writeRawAck so a retry sees
// the byte-identical body.
func writeSuccess(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, SuccessResponse{
		OK:      true,
		Message: msg,
	})
}

// writeError serialises an ErrorResponse. `msg` must NEVER be raw err.Error()
// text — handlers log the full error server-side and pass a fixed, client-safe
// string here. For validation errors use writeValidationError, which reuses
// this helper but allows the field name in the message.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorResponse{
		OK:      false,
		Code:    code,
		Message: msg,
	})
}

// writeValidationError maps a *ValidationError to a 400 ErrorResponse. The
// response message is "invalid <field>" — the field name only, never the
// user-supplied value (M4 in REVIEW.md: the value may itself be injection-
// crafted text and should not be echoed). If err is not a *ValidationError
// it is treated as an opaque 400.
func writeValidationError(w http.ResponseWriter, err error) {
	if ve, ok := errors.AsType[*ValidationError](err); ok {
		writeError(w, http.StatusBadRequest, CodeInvalidField, "invalid "+ve.Field)

		return
	}

	writeError(w, http.StatusBadRequest, CodeInvalidField, "invalid payload")
}

// writeUnauthorized returns the single fixed 401 shape for every HMAC
// authentication failure. See hmacAuth for the rationale.
func writeUnauthorized(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, CodeUnauthorized, "unauthorized")
}

// writeUpstreamUnavailable returns a fixed generic 502 body for upstream
// verification failures. Details (including any callbackError.DetailForLog()
// body) are logged server-side only — never echoed to the client, since the
// upstream response may contain tokens or other secrets leaked by a
// misconfigured CM.
func writeUpstreamUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusBadGateway, CodeUpstreamFailure, "upstream verification failed")
}

// logDebug emits a Debug log via h.logger, falling back to slog.Default
// when no logger was injected (the test-only code path). All call sites
// pass a compile-time-constant msg literal; the variadic args are keyed
// k/v pairs where values come from internal state or sanitised error
// strings — never raw request bodies.
func (h *Handler) logDebug(msg string, args ...any) {
	if h.logger != nil {
		h.logger.Debug(msg, args...)

		return
	}

	slog.Debug(msg, args...) //nolint:gosec // msg is always a literal at call sites
}

// logWarn emits a Warn log via h.logger, falling back to slog.Default
// when no logger was injected. See logDebug for the taint discussion.
func (h *Handler) logWarn(msg string, args ...any) {
	if h.logger != nil {
		h.logger.Warn(msg, args...)

		return
	}

	slog.Warn(msg, args...) //nolint:gosec // msg is always a literal at call sites
}

// logVerifyAutonomousFailure emits a Warn log with the short (body-free)
// error and, when the error is a callback.callbackError, a Debug log with the
// truncated upstream body for operators. Keeps secrets off shared log sinks
// unless Debug is explicitly enabled.
func (h *Handler) logVerifyAutonomousFailure(payload PromotePayload, err error) {
	logger := h.logger
	if logger == nil {
		logger = slog.Default()
	}

	logger.Warn("contextmatrix verify-autonomous request failed",
		"card_id", payload.CardID,
		"project", payload.Project,
		"error", err.Error(),
	)

	if ce, ok := errors.AsType[*callback.Error](err); ok {
		logger.Debug("contextmatrix verify-autonomous upstream body",
			"card_id", payload.CardID,
			"project", payload.Project,
			"status", ce.StatusCode(),
			"detail", ce.DetailForLog(),
		)
	}
}
