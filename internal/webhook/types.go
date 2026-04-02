package webhook

// TriggerPayload is received from ContextMatrix to start a task.
type TriggerPayload struct {
	CardID      string `json:"card_id"`
	Project     string `json:"project"`
	RepoURL     string `json:"repo_url"`
	MCPURL      string `json:"mcp_url"`
	MCPAPIKey   string `json:"mcp_api_key,omitempty"`
	RunnerImage string `json:"runner_image,omitempty"`
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

// Response is the standard webhook response format.
type Response struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}
