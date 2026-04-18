package streammsg

import "encoding/json"

// UserMessage is the Claude Code stream-json shape for a user turn.
type UserMessage struct {
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// BuildUserMessage marshals a Claude Code stream-json user turn containing
// content as a single text block. The returned slice is newline-terminated.
func BuildUserMessage(content string) ([]byte, error) {
	var msg UserMessage
	msg.Type = "user"
	msg.Message.Role = "user"
	msg.Message.Content = []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{Type: "text", Text: content}}

	b, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
