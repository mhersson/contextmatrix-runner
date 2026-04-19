package webhook

// TriggerPayload is received from ContextMatrix to start a task.
type TriggerPayload struct {
	CardID      string `json:"card_id"`
	Project     string `json:"project"`
	RepoURL     string `json:"repo_url"`
	MCPURL      string `json:"mcp_url"`
	MCPAPIKey   string `json:"mcp_api_key,omitempty"`
	BaseBranch  string `json:"base_branch,omitempty"`
	RunnerImage string `json:"runner_image,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
	Model       string `json:"model,omitempty"`
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

// MessagePayload is received from ContextMatrix to deliver a user chat message
// to a running interactive container's stdin.
type MessagePayload struct {
	CardID    string `json:"card_id"`
	Project   string `json:"project"`
	Content   string `json:"content"`
	MessageID string `json:"message_id,omitempty"`
}

// PromotePayload is received from ContextMatrix to switch a running interactive
// session to fully autonomous mode.
type PromotePayload struct {
	CardID  string `json:"card_id"`
	Project string `json:"project"`
}

// Response is the standard webhook response format.
type Response struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Error     string `json:"error,omitempty"`
}
