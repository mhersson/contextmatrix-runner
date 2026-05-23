package streammsg

import (
	"encoding/json"
	"fmt"
)

// ToolResultBlock is a single tool_result content block within a user turn.
type ToolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
}

// ToolResultMessage is the Claude Code stream-json shape for a tool_result user turn.
type ToolResultMessage struct {
	Type    string `json:"type"`
	Message struct {
		Role    string            `json:"role"`
		Content []ToolResultBlock `json:"content"`
	} `json:"message"`
}

// BuildToolResultMessage marshals a Claude Code stream-json user turn containing
// a single tool_result content block. The returned slice is newline-terminated.
func BuildToolResultMessage(toolUseID, content string) ([]byte, error) {
	if toolUseID == "" {
		return nil, fmt.Errorf("streammsg: BuildToolResultMessage: toolUseID is required")
	}

	var msg ToolResultMessage

	msg.Type = "user"
	msg.Message.Role = "user"
	msg.Message.Content = []ToolResultBlock{{
		Type:      "tool_result",
		ToolUseID: toolUseID,
		Content:   content,
	}}

	b, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("streammsg: BuildToolResultMessage: marshal: %w", err)
	}

	return append(b, '\n'), nil
}
