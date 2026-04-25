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
// can be very long, so we allow up to 1MiB — but the scanner allocates only
// initScannerBuf up front and grows geometrically on demand so the typical
// short-line case doesn't pin the worst-case buffer for the container's
// lifetime. See CTXRUN-059 (H26).
const maxScannerBuf = 1 << 20 // 1 MiB

// initScannerBuf is the initial capacity handed to bufio.Scanner.Buffer.
// bufio.Scanner doubles internally when a line exceeds the current capacity,
// so this only sets the starting allocation — long stream-json lines still
// work up to maxScannerBuf.
const initScannerBuf = 64 * 1024 // 64 KiB

// maxToolCallLen is the maximum number of runes in a formatted tool call
// summary before it is truncated with a trailing ellipsis.
const maxToolCallLen = 200

// whitespaceRun matches one or more whitespace characters for collapsing.
var whitespaceRun = regexp.MustCompile(`\s+`)

// secretPatterns matches common credential formats that should never appear in
// logs. Compiled once at package init time.
//
// The KEY=VALUE patterns (e.g. CLAUDE_CODE_OAUTH_TOKEN=...) use a capturing
// group on the key so the replacement can preserve the key name while
// redacting only the value — useful when the match appears in a docker
// inspect-style env dump, where knowing *which* env var was present is
// helpful for debugging.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`ghp_[A-Za-z0-9_]{36,}`),           // GitHub personal access token
	regexp.MustCompile(`gho_[A-Za-z0-9_]{36,}`),           // GitHub OAuth token
	regexp.MustCompile(`ghu_[A-Za-z0-9_]{36,}`),           // GitHub user-to-server token
	regexp.MustCompile(`ghs_[A-Za-z0-9_]{36,}`),           // GitHub server-to-server token
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`),    // GitHub fine-grained PAT
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),       // Anthropic API key
	regexp.MustCompile(`v1\.[0-9a-f]{40,}`),               // GitHub App installation token
	regexp.MustCompile(`Bearer\s+[A-Za-z0-9._\-/+]{20,}`), // Bearer tokens in output
}

// envSecretKeys are environment-variable names whose values are secrets the
// runner injects into worker containers. When they appear as KEY=value in a
// log line (e.g. a dumped env block or a shell trace), the value is redacted
// but the key name is preserved.
var envSecretKeys = []string{
	"CLAUDE_CODE_OAUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"CM_MCP_API_KEY",
	"CM_GIT_TOKEN",
}

// envSecretPatterns is a derived slice of regexps matching KEY=<non-whitespace>
// for each entry in envSecretKeys. Compiled once at package init time.
var envSecretPatterns = buildEnvSecretPatterns(envSecretKeys)

func buildEnvSecretPatterns(keys []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(keys))
	for _, k := range keys {
		out = append(out, regexp.MustCompile(`(`+regexp.QuoteMeta(k)+`)=\S+`))
	}

	return out
}

// Redact replaces recognized secret patterns with [REDACTED]. For KEY=value
// style env-var secrets it preserves the key name (e.g.
// "CM_MCP_API_KEY=[REDACTED]") so operators can still tell which secret was
// about to be logged.
//
// Redact is called on container output: assistant text/thinking blocks inside
// ProcessStream, stderr lines in container/manager.go, and tool_use summaries.
// It is NOT applied to "user" or "system" LogEntry values, which are published
// directly by webhook handlers without passing through this function.
func Redact(s string) string {
	for _, re := range envSecretPatterns {
		s = re.ReplaceAllString(s, "$1=[REDACTED]")
	}

	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}

	return s
}

// Redactor redacts secrets from a string, including the static Redact
// patterns and an additional set of literal secret values supplied at
// construction time. Redactor is intended to be created per container so
// live tokens (installation tokens, MCP API keys, Anthropic OAuth tokens)
// injected into that container's environment are masked even if they appear
// verbatim in the container's output — a belt-and-suspenders complement to
// the static patterns.
//
// A Redactor with no secrets is equivalent to Redact.
type Redactor struct {
	// secrets is the list of literal strings to mask, sorted longest-first
	// so that substring overlaps redact at the longest match.
	secrets []string
}

// NewRedactor returns a Redactor that masks the static secret patterns and
// each non-empty value in secrets. Duplicate and empty values are discarded;
// values shorter than 8 bytes are also discarded to avoid catastrophic
// over-redaction (e.g. redacting every occurrence of a 4-character token).
func NewRedactor(secrets []string) *Redactor {
	const minSecretLen = 8

	seen := make(map[string]struct{}, len(secrets))
	filtered := make([]string, 0, len(secrets))

	for _, s := range secrets {
		if len(s) < minSecretLen {
			continue
		}

		if _, ok := seen[s]; ok {
			continue
		}

		seen[s] = struct{}{}

		filtered = append(filtered, s)
	}

	// Sort longest-first so overlapping substrings redact at the broader
	// match (e.g. a token that happens to contain a shorter secret as a
	// prefix).
	sortLongestFirst(filtered)

	return &Redactor{secrets: filtered}
}

func sortLongestFirst(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && len(s[j]) > len(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Redact returns input with literal secret values and recognized patterns
// replaced by [REDACTED].
func (r *Redactor) Redact(input string) string {
	if r == nil {
		return Redact(input)
	}

	for _, secret := range r.secrets {
		input = strings.ReplaceAll(input, secret, "[REDACTED]")
	}

	return Redact(input)
}

// SkillEngagedEvent is emitted when the model invokes the Skill tool to
// engage a specialist skill mounted at ~/.claude/skills/. Downstream
// consumers (callback layer) forward this to ContextMatrix so the
// engagement appears in the card's activity log.
type SkillEngagedEvent struct {
	SkillName string
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
//
// ProcessStream applies the static Redact patterns. Callers wanting
// per-container literal-secret redaction should use ProcessStreamWithRedactor.
func ProcessStream(r io.Reader, logger *slog.Logger, emit func(logbroadcast.LogEntry)) {
	ProcessStreamWithRedactor(r, logger, nil, emit, nil)
}

// ProcessStreamWithRedactor is like ProcessStream but applies redactor to
// every text/thinking/tool_use block before logging and emission. If redactor
// is nil the behaviour falls back to the static Redact.
//
// onSkillEngaged is called whenever a Skill tool_use is detected. It may be
// nil when the caller does not need skill-engagement notifications.
func ProcessStreamWithRedactor(r io.Reader, logger *slog.Logger, redactor *Redactor, emit func(logbroadcast.LogEntry), onSkillEngaged func(*SkillEngagedEvent)) {
	redact := func(s string) string {
		if redactor == nil {
			return Redact(s)
		}

		return redactor.Redact(s)
	}

	scanner := bufio.NewScanner(r)
	// Start at initScannerBuf; bufio.Scanner grows as needed up to maxScannerBuf.
	// See CTXRUN-059 (H26) for the memory-footprint rationale.
	scanner.Buffer(make([]byte, 0, initScannerBuf), maxScannerBuf)

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
				content := redact(block.Text)
				logger.Info("claude", "claude_text", content)

				if emit != nil {
					emit(logbroadcast.LogEntry{Type: "text", Content: content})
				}
			case "thinking":
				content := redact(block.Thinking)
				logger.Info("claude", "claude_thinking", content)

				if emit != nil {
					emit(logbroadcast.LogEntry{Type: "thinking", Content: content})
				}
			case "tool_use":
				if strings.HasPrefix(block.Name, "mcp__") {
					continue
				}

				if block.Name == "Skill" {
					handleSkillToolUse(block.Input, logger, onSkillEngaged)

					continue
				}

				content := redact(formatToolCall(block.Name, block.Input))
				logger.Info("claude", "claude_tool", content)

				if emit != nil {
					emit(logbroadcast.LogEntry{Type: "tool_call", Content: content})
				}
			}
		}
	}
}

// handleSkillToolUse parses a Skill tool_use block and calls onSkillEngaged
// if a skill name can be extracted. It accepts both the "skill" and "name"
// input keys so callers can handle either shape that Claude Code may emit.
func handleSkillToolUse(rawInput json.RawMessage, logger *slog.Logger, onSkillEngaged func(*SkillEngagedEvent)) {
	var input struct {
		Skill string `json:"skill"`
		Name  string `json:"name"` // fallback — CC may use either key
	}

	if err := json.Unmarshal(rawInput, &input); err != nil {
		logger.Warn("logparser: failed to parse Skill tool input", "error", err)

		return
	}

	name := input.Skill
	if name == "" {
		name = input.Name
	}

	if name == "" {
		return
	}

	if onSkillEngaged != nil {
		onSkillEngaged(&SkillEngagedEvent{SkillName: name})
	}
}
