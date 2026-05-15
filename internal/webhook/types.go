package webhook

// TriggerPayload is received from ContextMatrix to start a task.
type TriggerPayload struct {
	CardID      string    `json:"card_id"`
	Project     string    `json:"project"`
	RepoURL     string    `json:"repo_url"`
	MCPAPIKey   string    `json:"mcp_api_key,omitempty"`
	BaseBranch  string    `json:"base_branch,omitempty"`
	RunnerImage string    `json:"runner_image,omitempty"`
	Interactive bool      `json:"interactive,omitempty"`
	Model       string    `json:"model,omitempty"`
	TaskSkills  *[]string `json:"task_skills,omitempty"`
}

// KillPayload is received from ContextMatrix to stop a specific task.
type KillPayload struct {
	CardID  string `json:"card_id"`
	Project string `json:"project"`
}

// StopAllPayload is received from ContextMatrix to stop all tasks.
type StopAllPayload struct {
	Project string `json:"project,omitempty"`
}

// MessagePayload is the body for POST /message. Exactly one of (card_id +
// project) for card-bound HITL or session_id for chat must be set.
type MessagePayload struct {
	CardID    string `json:"card_id,omitempty"`
	Project   string `json:"project,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Content   string `json:"content"`
	MessageID string `json:"message_id,omitempty"`
}

// IsChat reports whether the payload targets a chat session.
func (p MessagePayload) IsChat() bool { return p.SessionID != "" }

// PromotePayload is received from ContextMatrix to switch a running interactive
// session to fully autonomous mode.
type PromotePayload struct {
	CardID  string `json:"card_id"`
	Project string `json:"project"`
}

// EndSessionPayload is received from ContextMatrix to close the stdin of a
// running interactive container so claude exits on EOF. Used when the card
// reaches a terminal state and is released.
type EndSessionPayload struct {
	CardID  string `json:"card_id"`
	Project string `json:"project"`
}

// RefreshKnowledgePayload is received from ContextMatrix to start a KB
// refresh container for (project, repo). Mirrors runner.RefreshKnowledgePayload
// on the CM side; the JSON wire format is the contract. No card_id — the
// (project, repo) pair is the job key.
type RefreshKnowledgePayload struct {
	Project       string   `json:"project"`
	Repo          string   `json:"repo"`
	RepoURL       string   `json:"repo_url"`
	BaseBranch    string   `json:"base_branch,omitempty"`
	AgentID       string   `json:"agent_id"`
	OverwriteDocs []string `json:"overwrite_docs,omitempty"`
	MCPAPIKey     string   `json:"mcp_api_key,omitempty"`
	RunnerImage   string   `json:"runner_image,omitempty"`
	Model         string   `json:"model,omitempty"`
}

// SuccessResponse is the body returned for any 2xx webhook response. `OK` is
// always true; `Message` is a short, free-form human-readable label (never
// derived from user input); `MessageID` is only populated by /message acks so
// CM can correlate the retryable request.
type SuccessResponse struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

// ErrorResponse is the body returned for any non-2xx webhook response (except
// the custom /readyz shape and the SSE /logs stream). `OK` is always false;
// `Code` is a stable enum from codes.go; `Message` is a terse human-readable
// label that never echoes raw err.Error() strings or user-supplied values
// beyond a single field name for validation errors.
type ErrorResponse struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// CardKillResult is one entry in a StopAllResponse: whether the individual
// Kill succeeded for that (project, card_id), and a short reason if not.
type CardKillResult struct {
	CardID  string `json:"card_id"`
	Project string `json:"project"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// ContainerListItem is one entry in a ListContainersResponse. StartedAt is an
// RFC3339 timestamp derived from Docker's container Created field so CM can
// age-cap runaway containers without a second round-trip. Tracked reflects the
// runner's in-memory tracker state at response time; divergence (Tracked=false
// while State="running") is how the sweep detects containers the tracker has
// orphaned.
type ContainerListItem struct {
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name,omitempty"`
	CardID        string `json:"card_id"`
	SessionID     string `json:"session_id,omitempty"`
	Project       string `json:"project"`
	State         string `json:"state"`
	StartedAt     string `json:"started_at"`
	Tracked       bool   `json:"tracked"`
}

// ListContainersResponse is the body returned by GET /containers. OK is always
// true on success (a Docker list error surfaces as a 502 ErrorResponse with
// the upstream-failure code, not a partial success here).
type ListContainersResponse struct {
	OK         bool                `json:"ok"`
	Containers []ContainerListItem `json:"containers"`
}

// StopAllResponse is the body returned by POST /stop-all. `OK` is true iff
// every per-card Kill succeeded; on any failure the status code flips to 207
// and `OK` is false so a single field tells the caller whether they need to
// inspect Results.
type StopAllResponse struct {
	OK      bool             `json:"ok"`
	Total   int              `json:"total"`
	Stopped int              `json:"stopped"`
	Failed  int              `json:"failed"`
	Results []CardKillResult `json:"results"`
}

// ChatStartPayload is received from ContextMatrix to start a chat-mode container.
type ChatStartPayload struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project,omitempty"`
	RepoURL   string `json:"repo_url,omitempty"`
	// MCPAPIKey is forwarded to the container as CM_MCP_API_KEY so the
	// in-container claude can authenticate to CM's MCP endpoint. May be
	// empty when CM's MCP listener has no auth (loopback dev mode); the
	// container then merges an MCP entry with no Authorization header.
	MCPAPIKey string `json:"mcp_api_key,omitempty"`
	// Model is the Claude model the chat container should run. When empty
	// the entrypoint falls back to the historical default
	// (claude-sonnet-4-6). Validated against an allowlist regex; the real
	// allowlist (label, max context tokens) lives on the CM side.
	Model string `json:"model,omitempty"`
	// Resume, when non-nil, is the rehydration payload describing the prior
	// transcript. The runner writes it to /run/cm-chat/resume.jsonl inside
	// the container and sets CM_CHAT_RESUME=1 so the entrypoint switches
	// into the rehydration prompt branch.
	Resume *ChatResumeContext `json:"resume,omitempty"`
	// Primer is the chat-mode orientation text written to the
	// container's stdin as a stream-json user envelope before any
	// rehydration priming. Empty means "no primer" — the handler
	// skips the stdin write. Sourced from CM's
	// workflow-skills/chat-mode.md on each cold open.
	Primer string `json:"primer,omitempty"`
}

// ChatResumeContext mirrors chat.ResumeContext on the CM side. Wire shape
// only — the runner doesn't import CM types.
type ChatResumeContext struct {
	Turns   []ChatResumeTurn `json:"turns"`
	Clipped bool             `json:"clipped"`
	OrigSeq int64            `json:"original_seq"`
}

// ChatResumeTurn is one filtered transcript entry in the rehydration payload.
type ChatResumeTurn struct {
	Seq     int64  `json:"seq"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatEndPayload is received from ContextMatrix to close the stdin of a running
// chat container so claude receives EOF and exits.
type ChatEndPayload struct {
	SessionID string `json:"session_id"`
}

// Response is the legacy polymorphic webhook response body.
//
// Deprecated: new handlers should emit SuccessResponse or ErrorResponse via
// writeSuccess / writeError. This type is retained because existing tests
// decode the shape directly (and a few external consumers may depend on the
// `error` field). It is a union of Success + Error fields and no longer
// produced by the runner — but a JSON unmarshal of a SuccessResponse or an
// ErrorResponse into a Response still populates the matching subset of
// fields so older callers keep working.
type Response struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Error     string `json:"error,omitempty"`
	Code      string `json:"code,omitempty"`
}
