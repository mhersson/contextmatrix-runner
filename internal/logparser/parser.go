// Package logparser processes Claude Code's stream-json output and logs
// relevant events using structured logging.
package logparser

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"

	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
)

// maxScannerBuf is the maximum token size for the scanner. Thinking blocks
// can be very long, so we allocate a generous buffer.
const maxScannerBuf = 1 << 20 // 1 MiB

// secretPatterns matches common credential formats that should never appear in
// logs. Compiled once at package init time.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`ghp_[A-Za-z0-9_]{36,}`),          // GitHub personal access token
	regexp.MustCompile(`gho_[A-Za-z0-9_]{36,}`),          // GitHub OAuth token
	regexp.MustCompile(`ghu_[A-Za-z0-9_]{36,}`),          // GitHub user-to-server token
	regexp.MustCompile(`ghs_[A-Za-z0-9_]{36,}`),          // GitHub server-to-server token
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`),   // GitHub fine-grained PAT
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),      // Anthropic API key
	regexp.MustCompile(`Bearer\s+[A-Za-z0-9._\-/+]{20,}`), // Bearer tokens in output
}

// Redact replaces recognized secret patterns with [REDACTED].
//
// Redact is called only on container output: assistant text/thinking blocks
// inside ProcessStream, and stderr lines in container/manager.go. It is NOT
// applied to "user" or "system" LogEntry values, which are published directly
// by webhook handlers without passing through this function.
func Redact(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

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
//
// emit is called for each parsed block after the slog call. If emit is nil,
// it is skipped (backward-compatible with callers that do not need broadcasting).
func ProcessStream(r io.Reader, logger *slog.Logger, emit func(logbroadcast.LogEntry)) {
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
				content := Redact(block.Text)
				logger.Info("claude", "claude_text", content)
				if emit != nil {
					emit(logbroadcast.LogEntry{Type: "text", Content: content})
				}
			case "thinking":
				content := Redact(block.Thinking)
				logger.Info("claude", "claude_thinking", content)
				if emit != nil {
					emit(logbroadcast.LogEntry{Type: "thinking", Content: content})
				}
			case "tool_use":
				if strings.HasPrefix(block.Name, "mcp__") {
					continue
				}
				logger.Info("claude", "claude_tool", block.Name)
				if emit != nil {
					emit(logbroadcast.LogEntry{Type: "tool_call", Content: block.Name})
				}
			}
		}
	}
}
