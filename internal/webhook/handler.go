package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-runner/internal/container"
	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

type bodyKey struct{}

// Handler processes incoming webhooks from ContextMatrix.
type Handler struct {
	manager       *container.Manager
	tracker       *tracker.Tracker
	broadcaster   *logbroadcast.Broadcaster
	apiKey        string
	maxConcurrent int
	logger        *slog.Logger
}

// NewHandler creates a webhook handler.
func NewHandler(
	manager *container.Manager,
	tracker *tracker.Tracker,
	broadcaster *logbroadcast.Broadcaster,
	apiKey string,
	maxConcurrent int,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		manager:       manager,
		tracker:       tracker,
		broadcaster:   broadcaster,
		apiKey:        apiKey,
		maxConcurrent: maxConcurrent,
		logger:        logger,
	}
}

// Register adds all webhook routes to the mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /trigger", h.hmacAuth(h.handleTrigger))
	mux.HandleFunc("POST /kill", h.hmacAuth(h.handleKill))
	mux.HandleFunc("POST /stop-all", h.hmacAuth(h.handleStopAll))
	mux.HandleFunc("GET /logs", h.hmacAuth(h.handleLogs))
	mux.HandleFunc("GET /health", h.handleHealth)
}

func (h *Handler) handleTrigger(w http.ResponseWriter, r *http.Request) {
	body := r.Context().Value(bodyKey{}).([]byte)

	var payload TriggerPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if payload.CardID == "" || payload.Project == "" || payload.RepoURL == "" || payload.MCPURL == "" {
		writeError(w, http.StatusBadRequest, "card_id, project, repo_url, and mcp_url are required")
		return
	}

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
		if strings.Contains(err.Error(), "container limit reached") {
			writeError(w, http.StatusTooManyRequests, err.Error())
		} else {
			writeError(w, http.StatusConflict, "task already running: "+payload.CardID)
		}
		return
	}

	h.manager.Run(ctx, container.RunConfig{
		CardID:      payload.CardID,
		Project:     payload.Project,
		RepoURL:     payload.RepoURL,
		MCPURL:      payload.MCPURL,
		MCPAPIKey:   payload.MCPAPIKey,
		RunnerImage: payload.RunnerImage,
	})

	writeJSON(w, http.StatusAccepted, Response{OK: true, Message: "container starting"})
}

func (h *Handler) handleKill(w http.ResponseWriter, r *http.Request) {
	body := r.Context().Value(bodyKey{}).([]byte)

	var payload KillPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if payload.CardID == "" || payload.Project == "" {
		writeError(w, http.StatusBadRequest, "card_id and project are required")
		return
	}

	if err := h.manager.Kill(payload.Project, payload.CardID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, Response{OK: true, Message: "container killed"})
}

func (h *Handler) handleStopAll(w http.ResponseWriter, r *http.Request) {
	body := r.Context().Value(bodyKey{}).([]byte)

	var payload StopAllPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	var containers []*tracker.ContainerInfo
	if payload.Project != "" {
		containers = h.tracker.ListByProject(payload.Project)
	} else {
		containers = h.tracker.All()
	}

	killed := 0
	for _, info := range containers {
		if err := h.manager.Kill(info.Project, info.CardID); err != nil {
			h.logger.Warn("failed to kill container during stop-all",
				"card_id", info.CardID, "error", err)
			continue
		}
		killed++
	}

	writeJSON(w, http.StatusOK, Response{
		OK:      true,
		Message: "stopped " + itoa(killed) + " containers",
	})
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"running_containers": h.tracker.Count(),
	})
}

// handleLogs streams log entries via Server-Sent Events (SSE).
// An optional ?project= query parameter filters entries by project name.
func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	project := r.URL.Query().Get("project")

	// Clear the write deadline — the server has a 30s WriteTimeout that would
	// otherwise terminate the long-lived SSE connection.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		if h.logger != nil {
			h.logger.Debug("SSE could not clear write deadline", "error", err)
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Flush headers and send initial keepalive to trigger client onopen.
	flusher.Flush()
	if _, err := fmt.Fprintf(w, ": connected\n\n"); err != nil {
		if h.logger != nil {
			h.logger.Debug("SSE initial write failed", "error", err)
		}
		return
	}
	flusher.Flush()

	ch, unsubscribe := h.broadcaster.Subscribe(project)
	defer unsubscribe()

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

// hmacAuth is middleware that verifies HMAC signatures on incoming requests.
func (h *Handler) hmacAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sigHeader := r.Header.Get(cmhmac.SignatureHeader)
		if sigHeader == "" {
			writeError(w, http.StatusForbidden, "missing signature")
			return
		}
		tsHeader := r.Header.Get(cmhmac.TimestampHeader)
		if tsHeader == "" {
			writeError(w, http.StatusForbidden, "missing timestamp")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read body")
			return
		}

		sig := strings.TrimPrefix(sigHeader, "sha256=")
		if !cmhmac.VerifySignatureWithTimestamp(h.apiKey, sig, tsHeader, body, cmhmac.DefaultMaxClockSkew) {
			if h.logger != nil {
				h.logger.Warn("webhook authentication failed", "remote_addr", r.RemoteAddr)
			}
			writeError(w, http.StatusForbidden, "invalid signature")
			return
		}

		ctx := context.WithValue(r.Context(), bodyKey{}, body)
		next(w, r.WithContext(ctx))
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, Response{OK: false, Error: msg})
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
