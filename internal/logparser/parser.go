// Package logparser processes Claude Code's stream-json output and logs
// relevant events using structured logging.
package logparser

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
)

// maxScannerBuf is the maximum token size for the scanner. Thinking blocks
// can be very long, so we allocate a generous buffer.
const maxScannerBuf = 1 << 20 // 1 MiB

// maxToolCallLen is the maximum number of runes in a formatted tool call
// summary before it is truncated with a trailing ellipsis.
const maxToolCallLen = 200

// whitespaceRun matches one or more whitespace characters for collapsing.
var whitespaceRun = regexp.MustCompile(`\s+`)

// secretPatterns matches common credential formats that should never appear in
// logs. Compiled once at package init time.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`ghp_[A-Za-z0-9_]{36,}`),           // GitHub personal access token
	regexp.MustCompile(`gho_[A-Za-z0-9_]{36,}`),           // GitHub OAuth token
	regexp.MustCompile(`ghu_[A-Za-z0-9_]{36,}`),           // GitHub user-to-server token
	regexp.MustCompile(`ghs_[A-Za-z0-9_]{36,}`),           // GitHub server-to-server token
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`),    // GitHub fine-grained PAT
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),       // Anthropic API key
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
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

// formatToolCall returns a human-readable summary of a tool call for logging.
// When input is empty, null, or an empty object, only the tool name is returned.
// Otherwise, it returns "name: <summary>" with per-tool argument extraction.
// The result is whitespace-collapsed and truncated to maxToolCallLen runes.
func formatToolCall(name string, input json.RawMessage) string {
	// Return name-only when input is absent, null, or empty object.
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" {
		return name
	}

	summary := extractToolSummary(name, input)
	if summary == "" {
		return name
	}

	result := name + ": " + summary

	// Collapse \r?\n and runs of whitespace to a single space.
	result = collapseWhitespace(result)

	// Truncate to maxToolCallLen runes (UTF-8 safe).
	runes := []rune(result)
	if len(runes) > maxToolCallLen {
		result = string(runes[:maxToolCallLen]) + "…"
	}

	return result
}

// collapseWhitespace replaces newlines and runs of whitespace with a single space.
func collapseWhitespace(s string) string {
	return whitespaceRun.ReplaceAllString(s, " ")
}

// extractToolSummary extracts the relevant argument summary for a known tool.
// Returns empty string when no usable summary can be extracted.
func extractToolSummary(name string, input json.RawMessage) string {
	switch name {
	case "Bash":
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(input, &args); err != nil || args.Command == "" {
			return compactJSON(input)
		}
		// First line only.
		first := strings.SplitN(args.Command, "\n", 2)[0]

		return strings.TrimSpace(first)

	case "Read", "Edit", "MultiEdit", "Write", "NotebookEdit":
		var args struct {
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal(input, &args); err != nil || args.FilePath == "" {
			return compactJSON(input)
		}

		return args.FilePath

	case "Glob":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal(input, &args); err != nil || args.Pattern == "" {
			return compactJSON(input)
		}

		if args.Path != "" {
			return args.Pattern + " in " + args.Path
		}

		return args.Pattern

	case "Grep":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal(input, &args); err != nil || args.Pattern == "" {
			return compactJSON(input)
		}

		if args.Path != "" {
			return args.Pattern + " in " + args.Path
		}

		return args.Pattern

	case "WebFetch":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(input, &args); err != nil || args.URL == "" {
			return compactJSON(input)
		}

		return args.URL

	case "WebSearch":
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(input, &args); err != nil || args.Query == "" {
			return compactJSON(input)
		}

		return args.Query

	case "Task", "Agent":
		var args struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(input, &args); err != nil || args.Description == "" {
			return compactJSON(input)
		}

		return args.Description

	case "TodoWrite":
		var args struct {
			Todos []json.RawMessage `json:"todos"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return compactJSON(input)
		}

		n := len(args.Todos)
		if n == 0 {
			return compactJSON(input)
		}

		return fmt.Sprintf("%d todos", n)

	default:
		return compactJSON(input)
	}
}

// compactJSON returns a compact JSON representation of the input,
// or an empty string if input cannot be compacted.
func compactJSON(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}

	var buf bytes.Buffer
	if err := json.Compact(&buf, input); err != nil {
		return ""
	}

	s := buf.String()
	// If the compact result is just an empty object, return empty.
	if s == "{}" || s == "null" {
		return ""
	}
	// Count runes to avoid returning a huge JSON blob — truncate early.
	if utf8.RuneCountInString(s) > maxToolCallLen {
		runes := []rune(s)

		return string(runes[:maxToolCallLen])
	}

	return s
}

// ProcessStream reads newline-delimited JSON from r and logs Claude Code
// events using logger. It runs until r is exhausted or returns an error.
//
// Logged events:
//   - assistant text blocks    → Info, key "claude_text"
//   - assistant thinking blocks → Info, key "claude_thinking"
//   - assistant non-MCP tool_use → Info, key "claude_tool" with
//     "Name: <summary>" (per-tool argument extract, whitespace-collapsed,
//     redacted, truncated at maxToolCallLen runes with trailing "…")
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

				content := Redact(formatToolCall(block.Name, block.Input))
				logger.Info("claude", "claude_tool", content)

				if emit != nil {
					emit(logbroadcast.LogEntry{Type: "tool_call", Content: content})
				}
			}
		}
	}
}
