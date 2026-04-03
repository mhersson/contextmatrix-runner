// Package logparser processes Claude Code's stream-json output and logs
// relevant events using structured logging.
package logparser

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
)

// maxScannerBuf is the maximum token size for the scanner. Thinking blocks
// can be very long, so we allocate a generous buffer.
const maxScannerBuf = 1 << 20 // 1 MiB

// event is the top-level structure of every stream-json line.
type event struct {
	Type    string  `json:"type"`
	Message message `json:"message"`
}

// message holds the assistant's response content.
type message struct {
	Content []contentBlock `json:"content"`
}

// contentBlock represents a single block within a message.
type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
	Name     string `json:"name"`
}

// ProcessStream reads newline-delimited JSON from r and logs Claude Code
// events using logger. It runs until r is exhausted or returns an error.
//
// Logged events:
//   - assistant text blocks    → Info, key "claude_text"
//   - assistant thinking blocks → Info, key "claude_thinking"
//   - assistant non-MCP tool_use → Info, key "claude_tool" (name only)
//
// All other event types are silently skipped. Malformed JSON lines produce a
// warning and do not interrupt processing.
func ProcessStream(r io.Reader, logger *slog.Logger) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, maxScannerBuf)
	scanner.Buffer(buf, maxScannerBuf)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			logger.Warn("logparser: malformed JSON line", "error", err)
			continue
		}

		if ev.Type != "assistant" {
			continue
		}

		for _, block := range ev.Message.Content {
			switch block.Type {
			case "text":
				logger.Info("claude", "claude_text", block.Text)
			case "thinking":
				logger.Info("claude", "claude_thinking", block.Thinking)
			case "tool_use":
				if strings.HasPrefix(block.Name, "mcp__") {
					continue
				}
				logger.Info("claude", "claude_tool", block.Name)
			}
		}
	}
}
