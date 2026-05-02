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
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// CTXRUN-060 note: threading the raw body through context.Value is an
// accepted anti-pattern (context-as-data rather than request-scoped
// cancellation/deadline). A future refactor should pass the body to each
// handler via an explicit argument or a bodyHandler wrapper type. Deferring
// here because it touches every handler signature (six handlers) and the
// polish-sweep scope is supposed to stay low-risk.
type bodyKey struct{}

// ContainerOps is the subset of container.Manager used by the webhook handler
// for kill / list / force-remove operations on already-spawned containers.
// Using an interface here enables handler tests to inject fakes without
// needing the Docker daemon. Container lifecycle (spawn / run) is owned by
// OrchestratedDispatcher, not this interface.
type ContainerOps interface {
	Kill(project, cardID string) error
	ListManaged(ctx context.Context) ([]container.ManagedContainer, error)
	ForceRemoveByLabels(ctx context.Context, project, cardID string) (int, error)
}

// OrchestratedDispatcher routes /trigger payloads to the per-card driver
// (internal/driver). The dispatcher is mandatory: every trigger goes through
// it.
//
// Dispatch must NOT block — it is responsible for kicking off a
// goroutine that runs the driver and returns to the webhook caller
// promptly. The cancel func releases the tracker entry's run context;
// the dispatcher takes ownership and is expected to invoke it on
// completion (success or error). onComplete fires after the driver
// goroutine exits (success, error, or panic) so the caller can drop
// the tracker entry — without it the entry leaks and the same card
// cannot be re-triggered until runner restart.
type OrchestratedDispatcher interface {
	Dispatch(ctx context.Context, payload TriggerPayload, cancel context.CancelFunc, onComplete func()) error
}

// Handler processes incoming webhooks from ContextMatrix.
type Handler struct {
	manager       ContainerOps
	tracker       *tracker.Tracker
	broadcaster   *logbroadcast.Broadcaster
	cmClient      *callback.Client // contextmatrix callback client for promote API call
	apiKey        string
	mcpURL        string // derived from contextmatrix_url + "/mcp" at startup
	maxConcurrent int
	logger        *slog.Logger
	metrics       *metrics.Metrics

	// webhookReplaySkew is the maximum allowed age for webhook timestamps.
	// Defaults to cmhmac.DefaultMaxClockSkew when zero (for backward
	// compatibility with tests that construct Handler literals directly).
	webhookReplaySkew time.Duration

	// replayCache rejects previously-seen HMAC signatures.
	// Optional — if nil the protection is disabled, which keeps existing
	// handler tests compiling without a cache fixture.
	replayCache *ReplayCache

	// health drives the /readyz endpoint. Optional for tests; nil means
	// /readyz reports ready unconditionally (see handleReadyz).
	health *HealthState

	// orchestrated routes /trigger to the per-card driver. Required —
	// every /trigger goes through it. Tests that don't exercise /trigger
	// may leave it nil; production wiring sets it via
	// SetOrchestratedDispatcher before Register is called.
	orchestrated OrchestratedDispatcher
}

// NewHandler creates a webhook handler.
//
// mcpURL is the MCP endpoint URL derived from contextmatrix_url at startup
// (e.g. "http://contextmatrix:8080/mcp"). It is injected into every container
// as CM_MCP_URL so Claude Code can reach the MCP server.
//
// webhookReplaySkew is the maximum allowed age for incoming webhook timestamps.
// Pass time.Duration(cfg.WebhookReplaySkewSeconds)*time.Second from main; in
// tests that construct Handler literals directly the field defaults to zero
// which falls back to cmhmac.DefaultMaxClockSkew inside hmacAuth.
//
// health is optional — pass nil in tests that do not exercise /readyz. In
// production wiring (cmd/contextmatrix-runner/main.go), the same
// *HealthState is also held by the preflight retry loop and by the
// shutdown sequence so all three components observe and/or flip the same
// atomic flags.
func NewHandler(
	manager ContainerOps,
	tracker *tracker.Tracker,
	broadcaster *logbroadcast.Broadcaster,
	cmClient *callback.Client,
	apiKey string,
	maxConcurrent int,
	mcpURL string,
	logger *slog.Logger,
	webhookReplaySkew time.Duration,
	health *HealthState,
) *Handler {
	return &Handler{
		manager:           manager,
		tracker:           tracker,
		broadcaster:       broadcaster,
		cmClient:          cmClient,
		apiKey:            apiKey,
		mcpURL:            mcpURL,
		maxConcurrent:     maxConcurrent,
		logger:            logger,
		webhookReplaySkew: webhookReplaySkew,
		health:            health,
	}
}

// SetReplayCache wires in the signature-replay cache used by the HMAC
// middleware. Pass nil to disable replay protection (used by tests).
// Must be called before Register so every route gets the protected
// middleware wrapper.
func (h *Handler) SetReplayCache(c *ReplayCache) {
	h.replayCache = c
}

// WithMetrics attaches a metrics bundle used by the /trigger saturation log
// and by the request middleware in Register.
func (h *Handler) WithMetrics(m *metrics.Metrics) *Handler {
	h.metrics = m

	return h
}

// SetOrchestratedDispatcher wires the per-card driver dispatcher. Required
// for /trigger handling; must be called before Register.
func (h *Handler) SetOrchestratedDispatcher(d OrchestratedDispatcher) {
	h.orchestrated = d
}

// isDraining reports whether a graceful shutdown has started. Handlers that
// begin or extend long-running work (/trigger) check this first and return
// 503 so the shutdown sequence can finish without CM pushing more work onto
// a draining runner. /kill, /stop-all, /logs, /health, /readyz intentionally
// remain reachable during drain — they either finish quickly or surface
// state we want operators to be able to read.
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
	mux.Handle("GET /logs", wrap(h.handleLogs, true))
	mux.Handle("GET /containers", wrap(h.handleListContainers, true))
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

	if err := ValidatePayload(&payload); err != nil {
		writeValidationError(w, err)

		return
	}

	// Start a span that covers the synchronous half of /trigger — admission
	// check + dispatcher kickoff. The detached ctx below deliberately does
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

	// Dispatch to the per-card FSM driver and return immediately. The
	// dispatcher is responsible for spawning a goroutine and invoking
	// cancel on completion. onComplete drops the tracker entry once
	// the driver goroutine exits so the same card can be re-triggered
	// without restarting the runner.
	project := payload.Project
	cardID := payload.CardID
	onComplete := func() {
		h.tracker.Remove(project, cardID)
	}

	if err := h.orchestrated.Dispatch(ctx, payload, cancel, onComplete); err != nil {
		h.tracker.Remove(payload.Project, payload.CardID)
		cancel()
		h.logWarn("orchestrated dispatch failed",
			"card_id", payload.CardID,
			"project", payload.Project,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")

		return
	}

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

	if err := ValidatePayload(&payload); err != nil {
		writeValidationError(w, err)

		return
	}

	// /kill is idempotent. Three cases:
	//
	//  1. Tracker has the entry → normal cancel path (manager.Kill).
	//  2. Tracker is empty but Docker still has a labeled container for
	//     (project, card_id) → tracker/Docker divergence. Go past the
	//     tracker with ForceRemoveByLabels; returning 200 no-op here would
	//     leak the container to container_timeout (2h). This is the class
	//     of leak the Docker-authoritative cleanup is designed to fix.
	//  3. Tracker empty AND no matching Docker container → actual no-op.
	if !h.tracker.Has(payload.Project, payload.CardID) {
		removed, err := h.manager.ForceRemoveByLabels(r.Context(), payload.Project, payload.CardID)
		if err != nil {
			// Errors here are per-container removal failures collected via
			// errors.Join; log and still surface 500 so CM sees the failure
			// and the next sweep can retry.
			h.logWarn("kill: force-remove by labels failed",
				"card_id", payload.CardID, "project", payload.Project, "error", err.Error())
			writeError(w, http.StatusInternalServerError, CodeInternal, "kill failed")

			return
		}

		if removed > 0 {
			h.logInfo("kill: force-removed untracked container(s)",
				"card_id", payload.CardID, "project", payload.Project, "removed", removed)
			writeSuccess(w, http.StatusOK, "force-removed")

			return
		}

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

// handleListContainers returns every Docker container labeled as
// runner-managed, regardless of running/exited state. The response is CM's
// ground truth for "what containers exist on this runner" — the reconcile
// sweep correlates each entry against CM's card store and kills anything
// whose card should not be running. Authoritative from Docker, not from the
// in-memory tracker, so tracker/Docker divergence is visible.
func (h *Handler) handleListContainers(w http.ResponseWriter, r *http.Request) {
	containers, err := h.manager.ListManaged(r.Context())
	if err != nil {
		h.logWarn("list-containers: ListManaged failed", "error", err.Error())
		writeError(w, http.StatusBadGateway, CodeUpstreamFailure, "docker list failed")

		return
	}

	items := make([]ContainerListItem, 0, len(containers))
	for _, c := range containers {
		items = append(items, ContainerListItem{
			ContainerID:   c.ContainerID,
			ContainerName: c.ContainerName,
			CardID:        c.CardID,
			Project:       c.Project,
			State:         c.State,
			StartedAt:     c.StartedAt.UTC().Format(time.RFC3339),
			Tracked:       c.Tracked,
		})
	}

	writeJSON(w, http.StatusOK, ListContainersResponse{OK: true, Containers: items})
}

func (h *Handler) handleStopAll(w http.ResponseWriter, r *http.Request) {
	body := r.Context().Value(bodyKey{}).([]byte)

	var payload StopAllPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logDebug("stop-all: invalid JSON", "error", err)
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")

		return
	}

	if err := ValidatePayload(&payload); err != nil {
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

		// Reject oversize bodies BEFORE reading them so a 5 MiB legitimate
		// payload doesn't get silently truncated to 1 MiB and then fail
		// signature verification — the symptom of which looks identical to
		// "wrong key" and wastes operator time chasing the wrong cause.
		const maxBodyBytes = 1 << 20 // 1 MiB
		if r.ContentLength > maxBodyBytes {
			h.logWarn("webhook body exceeds cap",
				"remote_addr", r.RemoteAddr,
				"path", r.URL.Path,
				"content_length", r.ContentLength,
				"max_bytes", maxBodyBytes,
			)
			writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "request body exceeds cap")

			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			// A truncated / unreadable body is indistinguishable (to a
			// client) from a signature mismatch — both surface as 401.
			h.logDebug("hmac auth: body read failed", "remote_addr", r.RemoteAddr, "error", err.Error())
			writeUnauthorized(w)

			return
		}

		sig := strings.TrimPrefix(sigHeader, "sha256=")

		skew := h.webhookReplaySkew
		if skew == 0 {
			skew = cmhmac.DefaultMaxClockSkew
		}

		if !cmhmac.VerifySignatureWithTimestamp(h.apiKey, r.Method, r.URL.Path, sig, tsHeader, body, skew) {
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

// writeSuccess serialises a SuccessResponse.
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

// logInfo emits an Info log via h.logger, falling back to slog.Default when
// no logger was injected. See logDebug for the taint discussion.
func (h *Handler) logInfo(msg string, args ...any) {
	if h.logger != nil {
		h.logger.Info(msg, args...)

		return
	}

	slog.Info(msg, args...) //nolint:gosec // msg is always a literal at call sites
}
