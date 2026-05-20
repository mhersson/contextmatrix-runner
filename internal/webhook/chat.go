package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/mhersson/contextmatrix-runner/internal/container"
	"github.com/mhersson/contextmatrix-runner/internal/streammsg"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// chatPrimingWriteTimeout bounds a single chat-mode priming stdin write.
// Mirrors container/manager.go's primingWriteTimeout: a wedged hijacked
// socket whose Write blocks on kernel-buffer pressure must not freeze the
// /chat/start HTTP request thread. On timeout the chat container's stdin
// is force-closed via tracker.CloseStdinChat so the in-flight Write inside
// tracker.WriteStdinChat returns and releases stdin.mu.
//
// Declared as a package var so tests can shrink it without waiting for the
// 5s wall-clock budget. Fix W1 in REVIEW.md.
var chatPrimingWriteTimeout = 5 * time.Second

// handleChatStart starts a long-lived chat-mode container. The container runs
// claude in stream-json interactive mode; stdin is left open so subsequent
// /message calls can inject user turns.
func (h *Handler) handleChatStart(w http.ResponseWriter, r *http.Request) {
	// Refuse new chat starts during graceful shutdown — the shutdown sequence
	// SIGTERM/SIGKILLs running containers anyway, so spawning one here just
	// to kill it 100ms later is wasted work and noisy in logs.
	if h.isDraining() {
		writeError(w, http.StatusServiceUnavailable, CodeDraining, "runner is draining")

		return
	}

	body := r.Context().Value(bodyKey{}).([]byte)

	var p ChatStartPayload
	if err := json.Unmarshal(body, &p); err != nil {
		h.logDebug("chat/start: invalid JSON", "error", err,
			"correlation_id", correlationIDFromContext(r.Context()))
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")

		return
	}

	if err := ValidatePayload(&p); err != nil {
		h.logDebug("chat/start: validation failed", "error", err.Error(),
			"correlation_id", correlationIDFromContext(r.Context()))
		writeValidationError(w, err)

		return
	}

	hostOnly := ""
	if u, err := url.Parse(p.RepoURL); err == nil {
		hostOnly = u.Host
	}

	h.logInfo("chat/start: received",
		"session_id", p.SessionID, "project", p.Project, "repo_host", hostOnly,
		"correlation_id", correlationIDFromContext(r.Context()))

	if h.tracker.HasChat(p.SessionID) {
		writeError(w, http.StatusConflict, CodeConflict, "session already has a container")

		return
	}

	// Fail-fast capacity check: avoid a docker round-trip when the runner is
	// already saturated. The authoritative reservation happens via
	// AddChatIfUnderLimit after StartChat returns; the post-check still
	// catches racing /chat/start callers that slip through this gate.
	if h.tracker.Count() >= h.maxConcurrent {
		h.logWarn("chat/start: runner saturated",
			"session_id", p.SessionID, "limit", h.maxConcurrent,
			"correlation_id", correlationIDFromContext(r.Context()))
		writeError(w, http.StatusTooManyRequests, CodeLimitReached, "concurrency limit reached")

		return
	}

	gitToken := h.manager.BuildChatAuthEnv(r.Context())

	var resume *container.ChatResume

	if p.Resume != nil {
		turns := make([]container.ChatResumeTurn, len(p.Resume.Turns))
		for i, t := range p.Resume.Turns {
			turns[i] = container.ChatResumeTurn{Seq: t.Seq, Role: t.Role, Content: t.Content}
		}

		resume = &container.ChatResume{
			Turns:   turns,
			Clipped: p.Resume.Clipped,
			OrigSeq: p.Resume.OrigSeq,
		}
	}

	containerID, err := h.manager.StartChat(r.Context(), container.StartChatOpts{
		SessionID: p.SessionID,
		Project:   p.Project,
		RepoURL:   p.RepoURL,
		MCPURL:    h.mcpURL,
		MCPAPIKey: p.MCPAPIKey,
		GitToken:  gitToken,
		Model:     p.Model,
		Resume:    resume,
	})
	if err != nil {
		h.logWarn("chat/start: StartChat failed", "session_id", p.SessionID, "error", err.Error(),
			"correlation_id", correlationIDFromContext(r.Context()))
		writeError(w, http.StatusBadGateway, CodeUpstreamFailure, "start failed")

		return
	}

	// Detached ctx so the log-streaming goroutine outlives the HTTP request
	// that started the chat. Stored in the tracker so handleChatEnd / shutdown
	// can signal the streamer to stop.
	streamCtx, streamCancel := context.WithCancel(context.Background())

	addErr := h.tracker.AddChatIfUnderLimit(&tracker.ContainerInfo{
		ContainerID: containerID,
		SessionID:   p.SessionID,
		Project:     p.Project,
		Image:       h.manager.WorkerImage(),
		StartedAt:   time.Now(),
		Cancel:      streamCancel,
	}, h.maxConcurrent)
	if addErr != nil {
		streamCancel()

		// Detached ctx: r.Context() may already be cancelled here; manager.Stop
		// short-circuits on a cancelled parent and the container would leak
		// until the 2h sweep. 5s matches container.dockerCleanupTimeout (which
		// is unexported in that package).
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if stopErr := h.manager.Stop(stopCtx, containerID); stopErr != nil {
			// Surface as a metric in addition to the log so dashboards can
			// alarm on orphaned chat containers without parsing log lines.
			// Fix W7 in REVIEW.md.
			if h.metrics != nil {
				h.metrics.ChatRollbackFailuresTotal.Inc()
			}

			h.logWarn("chat/start: rollback stop failed",
				"container_id", containerID, "error", stopErr.Error(),
				"correlation_id", correlationIDFromContext(r.Context()))
		}

		stopCancel()

		// Discard the cleanup state stashed by StartChat; WaitAndCleanupChat
		// has not been called yet so nothing else will pop it.
		h.manager.DeleteChatCleanup(containerID)

		switch {
		case errors.Is(addErr, tracker.ErrLimitReached):
			h.logWarn("chat/start: runner saturated at reservation",
				"session_id", p.SessionID, "limit", h.maxConcurrent,
				"correlation_id", correlationIDFromContext(r.Context()))
			writeError(w, http.StatusTooManyRequests, CodeLimitReached, "concurrency limit reached")
		default:
			writeError(w, http.StatusConflict, CodeConflict, "track failed")
		}

		return
	}

	// Start the implicit-exit cleanup goroutine now that the tracker holds
	// the entry. If the container dies in the next microsecond (bad image,
	// exec failure) RemoveChat will find and clear the entry we just
	// registered — closing the race window that existed when StartChat
	// spawned this goroutine internally.
	h.manager.WaitAndCleanupChat(p.SessionID, containerID, p.Project)

	// Attach stdin so subsequent /message turns can stream into claude. If
	// the attach fails we tear the whole thing down and surface a 502; a
	// running container without stdin is dead weight (every /message call
	// would 409 with "container is not in interactive mode").
	//
	// WaitAndCleanupChat is already running by this point, so we rely on
	// Stop to make ContainerWait return; the goroutine then pops the
	// cleanup state and removes the chat resume dir. No explicit
	// DeleteChatCleanup is needed here (deleting it would race the
	// goroutine and silently drop the resume-dir cleanup).
	if err := h.manager.AttachChatStdin(r.Context(), p.SessionID, containerID); err != nil {
		// RemoveChat invokes the stored CancelFunc (streamCancel) before
		// returning, so we deliberately do NOT call streamCancel() again
		// here — a second invocation would be a redundant no-op (Go's
		// context.CancelFunc is idempotent) and obscured the lifecycle.
		// Fix W9 in REVIEW.md.
		h.tracker.RemoveChat(p.SessionID)

		// Detached ctx: AttachChatStdin failure is often the consequence of
		// r.Context() being cancelled (client disconnect, deadline elapsed).
		// Reusing the request ctx for Stop would make this a no-op and leak
		// the container. 5s matches container.dockerCleanupTimeout.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if stopErr := h.manager.Stop(stopCtx, containerID); stopErr != nil {
			// Surface as a metric in addition to the log so dashboards can
			// alarm on orphaned chat containers without parsing log lines.
			// Fix W7 in REVIEW.md.
			if h.metrics != nil {
				h.metrics.ChatRollbackFailuresTotal.Inc()
			}

			h.logWarn("chat/start: rollback stop failed",
				"container_id", containerID, "error", stopErr.Error(),
				"correlation_id", correlationIDFromContext(r.Context()))
		}

		stopCancel()

		h.logWarn("chat/start: AttachChatStdin failed",
			"session_id", p.SessionID, "container_id", containerID, "error", err.Error(),
			"correlation_id", correlationIDFromContext(r.Context()))
		writeError(w, http.StatusBadGateway, CodeUpstreamFailure, "attach stdin failed")

		return
	}

	// If CM shipped a chat-mode primer, write it to stdin BEFORE any
	// rehydration priming so the agent learns the MCP tool surface and
	// CM concepts before being asked to re-establish workspace state.
	if p.Primer != "" {
		h.writeChatPrimingEnvelope(r.Context(), p.SessionID, "primer", p.Primer)
	}

	// If CM signalled a rehydration phase, prime the agent via stdin with the
	// rehydration instructions. The -p positional prompt is ignored by Claude
	// when --input-format stream-json is set (the model treats it as system
	// context and does not auto-execute), so the rehydration instructions
	// have to arrive as a stream-json user message to actually trigger work.
	// The priming text is NOT persisted to the CM transcript (it doesn't go
	// through /message); only the agent's response — tool_calls and the
	// eventual chat_rehydration_complete call — is recorded.
	if p.Resume != nil {
		h.writeChatPrimingEnvelope(
			r.Context(),
			p.SessionID, "rehydration priming",
			buildChatRehydrationPriming(p.SessionID),
		)
	}

	h.manager.StreamChatLogs(streamCtx, p.SessionID, containerID, p.Project)

	h.logInfo("chat/start: container ready",
		"session_id", p.SessionID, "container_id", containerID,
		"correlation_id", correlationIDFromContext(r.Context()))

	writeJSON(w, http.StatusAccepted, ChatStartResponse{
		OK:          true,
		ContainerID: containerID,
	})
}

// handleChatEnd closes the stdin of a tracked chat container, then issues
// a graceful container stop. Stdin close gives claude a clean EOF; the
// follow-up Stop guarantees the container goes away even when claude in
// `-p --input-format stream-json` mode doesn't exit on EOF on its own.
// The tracker entry is removed after Stop returns.
func (h *Handler) handleChatEnd(w http.ResponseWriter, r *http.Request) {
	// Refuse during graceful shutdown — the shutdown sequence force-stops all
	// tracked containers anyway, and a concurrent /chat/end runner-Stop would
	// just race that path.
	if h.isDraining() {
		writeError(w, http.StatusServiceUnavailable, CodeDraining, "runner is draining")

		return
	}

	body := r.Context().Value(bodyKey{}).([]byte)

	var p ChatEndPayload
	if err := json.Unmarshal(body, &p); err != nil {
		h.logDebug("chat/end: invalid JSON", "error", err,
			"correlation_id", correlationIDFromContext(r.Context()))
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")

		return
	}

	if err := ValidatePayload(&p); err != nil {
		h.logDebug("chat/end: validation failed", "error", err.Error(),
			"correlation_id", correlationIDFromContext(r.Context()))
		writeValidationError(w, err)

		return
	}

	snap, ok := h.tracker.SnapshotChat(p.SessionID)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound, MsgNoContainerTracked)

		return
	}

	h.logInfo("chat/end: closing stdin", "session_id", p.SessionID,
		"correlation_id", correlationIDFromContext(r.Context()))

	// Best-effort stdin close. ErrNoStdinAttached and ErrStdinClosed are
	// both non-fatal here — the chat /end path always force-stops the
	// container below, so a missing or already-closed stdin doesn't change
	// what we do next; we only need to short-circuit on a tracker miss
	// (404) or a real I/O failure (500).
	if err := h.tracker.CloseStdinChat(p.SessionID); err != nil {
		switch {
		case errors.Is(err, tracker.ErrNotTracked):
			writeError(w, http.StatusNotFound, CodeNotFound, MsgNoContainerTracked)

			return
		case errors.Is(err, tracker.ErrNoStdinAttached), errors.Is(err, tracker.ErrStdinClosed):
			// fall through to force-stop
		default:
			h.logWarn("chat/end: CloseStdinChat failed",
				"session_id", p.SessionID, "error", err.Error(),
				"correlation_id", correlationIDFromContext(r.Context()))
			writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")

			return
		}
	}

	// Detached ctx: r.Context() may be cancelled if the client disconnected
	// after CloseStdinChat returned. Reusing the request ctx for Stop would
	// short-circuit and leak the container until the 2h sweep, while
	// RemoveChat below would still drop the tracker entry. 5s matches
	// container.dockerCleanupTimeout (unexported) and mirrors the rollback
	// paths in handleChatStart. Fix W3 in REVIEW.md.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()

	if err := h.manager.Stop(stopCtx, snap.ContainerID); err != nil {
		h.logWarn("chat/end: container stop failed",
			"session_id", p.SessionID, "container_id", snap.ContainerID, "error", err.Error(),
			"correlation_id", correlationIDFromContext(r.Context()))
	}

	h.tracker.RemoveChat(p.SessionID)

	h.logInfo("chat/end: session ended",
		"session_id", p.SessionID, "container_id", snap.ContainerID,
		"correlation_id", correlationIDFromContext(r.Context()))

	// 202 Accepted matches /end-session and the wider drain-async pattern:
	// the request is accepted, the stdin has been closed, and the
	// container will exit asynchronously through the
	// WaitAndCleanupChat / Stop path. Returning 200 here would imply the
	// teardown is synchronously complete on return, which is not strictly
	// true. Fix W10 in REVIEW.md.
	writeSuccess(w, http.StatusAccepted, "")
}

// writeChatPrimingEnvelope builds a stream-json user envelope from text and
// writes it to the chat container's stdin. Used by handleChatStart for both
// the chat-mode primer and the rehydration priming. Build / write failures
// are fail-open — a WARN is logged and the function returns without raising;
// the /chat/start request must still return 202 to avoid leaving an
// orphaned container the operator can't end through normal channels.
//
// The actual stdin write is bounded by chatPrimingWriteTimeout. A wedged
// hijacked socket whose Write blocks on kernel-buffer pressure used to
// freeze the /chat/start HTTP request thread indefinitely; now the write
// is run on a worker goroutine and we force-close the chat container's
// stdin via tracker.CloseStdinChat on timeout so the in-flight Write
// returns and releases stdin.mu. Card-mode uses an equivalent pattern in
// container.Manager.writePrimingWithTimeout; this is its chat-mode
// counterpart. Fix W1 in REVIEW.md.
//
// ctx carries the correlation_id used in log lines so a primer-write failure
// can be traced back to the originating /chat/start request. Fix W7 in
// REVIEW.md.
//
// kind is a human-readable label (e.g. "primer", "rehydration priming")
// never persisted to the transcript. Pass it as a structured slog field
// instead of concatenating into the message so log aggregators bucket by
// the constant "chat/start: build envelope failed" rather than splitting
// the series per kind. Fix W11 in REVIEW.md.
func (h *Handler) writeChatPrimingEnvelope(ctx context.Context, sessionID, kind, text string) {
	envelope, buildErr := streammsg.BuildUserMessage(text)
	if buildErr != nil {
		h.logWarn("chat/start: build envelope failed",
			"kind", kind,
			"session_id", sessionID, "error", buildErr.Error(),
			"correlation_id", correlationIDFromContext(ctx))

		return
	}

	h.writeChatStdinWithTimeout(ctx, sessionID, kind, envelope)
}

// writeChatStdinWithTimeout writes envelope to the chat container's stdin
// on a worker goroutine and bounds the wait by chatPrimingWriteTimeout. On
// timeout the chat container's stdin is closed via tracker.CloseStdinChat
// so the in-flight tracker.WriteStdinChat call inside the worker goroutine
// returns an error and releases stdin.mu (otherwise it would pin the
// per-entry mutex and block a subsequent /chat/end or shutdown forever).
// Build and write failures are fail-open: a WARN log is emitted and the
// function returns, so the /chat/start request still returns 202 rather
// than leaving an orphaned container the operator cannot end through
// normal channels. Mirrors container.Manager.writePrimingWithTimeout for
// card-mode. Fix W1 in REVIEW.md.
func (h *Handler) writeChatStdinWithTimeout(ctx context.Context, sessionID, kind string, envelope []byte) {
	done := make(chan error, 1)

	go func() {
		// We deliberately do NOT recover here — the tracker's WriteStdinChat
		// is internal Go code we control; a panic inside it is a real bug
		// that should crash the runner and not be swallowed.
		done <- h.tracker.WriteStdinChat(sessionID, envelope)
	}()

	select {
	case writeErr := <-done:
		if writeErr != nil {
			h.logWarn("chat/start: write envelope failed",
				"kind", kind,
				"session_id", sessionID, "error", writeErr.Error(),
				"correlation_id", correlationIDFromContext(ctx))

			return
		}

		h.logInfo("chat/start: envelope written",
			"kind", kind,
			"session_id", sessionID, "bytes", len(envelope),
			"correlation_id", correlationIDFromContext(ctx))
	case <-time.After(chatPrimingWriteTimeout):
		h.logWarn("chat/start: priming stdin write timed out; closing stdin to unblock",
			"kind", kind,
			"session_id", sessionID,
			"timeout", chatPrimingWriteTimeout,
			"correlation_id", correlationIDFromContext(ctx))

		// Force-close the chat container's stdin. The pending Write inside
		// the worker goroutine returns with an error and releases stdin.mu;
		// the goroutine then exits, the buffered `done` channel absorbs the
		// late send, and we do not block on it.
		if closeErr := h.tracker.CloseStdinChat(sessionID); closeErr != nil &&
			!errors.Is(closeErr, tracker.ErrNotTracked) &&
			!errors.Is(closeErr, tracker.ErrNoStdinAttached) {
			h.logWarn("chat/start: CloseStdinChat after timeout failed",
				"session_id", sessionID, "error", closeErr.Error(),
				"correlation_id", correlationIDFromContext(ctx))
		}
	}
}
