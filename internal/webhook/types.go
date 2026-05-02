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

// SuccessResponse is the body returned for any 2xx webhook response. `OK` is
// always true; `Message` is a short, free-form human-readable label (never
// derived from user input).
type SuccessResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
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
