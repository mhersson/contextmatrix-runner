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
		h.logDebug("chat/start: invalid JSON", "error", err)
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")

		return
	}

	if err := ValidatePayload(&p); err != nil {
		h.logDebug("chat/start: validation failed", "error", err.Error())
		writeError(w, http.StatusBadRequest, CodeInvalidField, err.Error())

		return
	}

	hostOnly := ""
	if u, err := url.Parse(p.RepoURL); err == nil {
		hostOnly = u.Host
	}

	h.logInfo("chat/start: received",
		"session_id", p.SessionID, "project", p.Project, "repo_host", hostOnly)

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
			"session_id", p.SessionID, "limit", h.maxConcurrent)
		writeError(w, http.StatusTooManyRequests, CodeLimitReached, "concurrency limit reached")

		return
	}

	authEnv := h.manager.BuildChatAuthEnv(r.Context())

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
		AuthEnv:   authEnv,
		Model:     p.Model,
		Resume:    resume,
	})
	if err != nil {
		h.logWarn("chat/start: StartChat failed", "session_id", p.SessionID, "error", err.Error())
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

		if stopErr := h.manager.Stop(r.Context(), containerID); stopErr != nil {
			h.logWarn("chat/start: rollback stop failed",
				"container_id", containerID, "error", stopErr.Error())
		}

		switch {
		case errors.Is(addErr, tracker.ErrLimitReached):
			h.logWarn("chat/start: runner saturated at reservation",
				"session_id", p.SessionID, "limit", h.maxConcurrent)
			writeError(w, http.StatusTooManyRequests, CodeLimitReached, "concurrency limit reached")
		default:
			writeError(w, http.StatusConflict, CodeConflict, "track failed")
		}

		return
	}

	// Attach stdin so subsequent /message turns can stream into claude. If
	// the attach fails we tear the whole thing down and surface a 502; a
	// running container without stdin is dead weight (every /message call
	// would 409 with "container is not interactive").
	if err := h.manager.AttachChatStdin(r.Context(), p.SessionID, containerID); err != nil {
		h.tracker.RemoveChat(p.SessionID)
		streamCancel()

		if stopErr := h.manager.Stop(r.Context(), containerID); stopErr != nil {
			h.logWarn("chat/start: rollback stop failed",
				"container_id", containerID, "error", stopErr.Error())
		}

		h.logWarn("chat/start: AttachChatStdin failed",
			"session_id", p.SessionID, "container_id", containerID, "error", err.Error())
		writeError(w, http.StatusBadGateway, CodeUpstreamFailure, "attach stdin failed")

		return
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
		text := buildChatRehydrationPriming(p.SessionID)

		envelope, buildErr := streammsg.BuildUserMessage(text)
		if buildErr != nil {
			h.logWarn("chat/start: build rehydration priming failed",
				"session_id", p.SessionID, "error", buildErr.Error())
		} else if writeErr := h.tracker.WriteStdinChat(p.SessionID, envelope); writeErr != nil {
			h.logWarn("chat/start: write rehydration priming failed",
				"session_id", p.SessionID, "error", writeErr.Error())
		} else {
			h.logInfo("chat/start: rehydration priming written",
				"session_id", p.SessionID, "bytes", len(envelope))
		}
	}

	h.manager.StreamChatLogs(streamCtx, p.SessionID, containerID, p.Project)

	h.logInfo("chat/start: container ready",
		"session_id", p.SessionID, "container_id", containerID)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":           true,
		"container_id": containerID,
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
		h.logDebug("chat/end: invalid JSON", "error", err)
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "invalid JSON")

		return
	}

	if err := ValidatePayload(&p); err != nil {
		h.logDebug("chat/end: validation failed", "error", err.Error())
		writeError(w, http.StatusBadRequest, CodeInvalidField, err.Error())

		return
	}

	snap, ok := h.tracker.SnapshotChat(p.SessionID)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound, "no container tracked")

		return
	}

	h.logInfo("chat/end: closing stdin", "session_id", p.SessionID)

	// Best-effort stdin close. ErrNoStdinAttached is non-fatal — a prior
	// /chat/end may already have closed it; we still want to force-stop
	// the container below.
	if err := h.tracker.CloseStdinChat(p.SessionID); err != nil {
		switch {
		case errors.Is(err, tracker.ErrNotTracked):
			writeError(w, http.StatusNotFound, CodeNotFound, "no container tracked")

			return
		case errors.Is(err, tracker.ErrNoStdinAttached):
			// fall through to force-stop
		default:
			h.logWarn("chat/end: CloseStdinChat failed",
				"session_id", p.SessionID, "error", err.Error())
			writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")

			return
		}
	}

	if err := h.manager.Stop(r.Context(), snap.ContainerID); err != nil {
		h.logWarn("chat/end: container stop failed",
			"session_id", p.SessionID, "container_id", snap.ContainerID, "error", err.Error())
	}

	h.tracker.RemoveChat(p.SessionID)

	h.logInfo("chat/end: session ended",
		"session_id", p.SessionID, "container_id", snap.ContainerID)

	writeSuccess(w, http.StatusOK, "")
}
