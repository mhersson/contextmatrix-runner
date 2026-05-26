package logparser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
)

// recordHandler is a minimal slog.Handler that captures records for assertions.
type recordHandler struct {
	records *[]slog.Record
}

func (h *recordHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)

	return nil
}
func (h *recordHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(_ string) slog.Handler      { return h }

// newTestLogger returns a logger and a pointer to the slice of captured records.
func newTestLogger() (*slog.Logger, *[]slog.Record) {
	records := make([]slog.Record, 0)
	h := &recordHandler{records: &records}

	return slog.New(h), &records
}

// attr returns the value of the named attribute from a record, or empty string.
func attr(r slog.Record, key string) string {
	var val string

	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val = a.Value.String()

			return false
		}

		return true
	})

	return val
}

func TestProcessStream(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantLevel     slog.Level
		wantKey       string
		wantValue     string
		wantRecords   int
		wantEmitType  string
		wantEmitCount int
	}{
		{
			name:          "text block is logged",
			input:         `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}`,
			wantLevel:     slog.LevelInfo,
			wantKey:       "claude_text",
			wantValue:     "Hello world",
			wantRecords:   1,
			wantEmitType:  "text",
			wantEmitCount: 1,
		},
		{
			name:          "thinking block is logged",
			input:         `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"deep thoughts"}]}}`,
			wantLevel:     slog.LevelInfo,
			wantKey:       "claude_thinking",
			wantValue:     "deep thoughts",
			wantRecords:   1,
			wantEmitType:  "thinking",
			wantEmitCount: 1,
		},
		{
			name:          "non-MCP tool_use is logged",
			input:         `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/foo.go"}}]}}`,
			wantLevel:     slog.LevelInfo,
			wantKey:       "claude_tool",
			wantValue:     "Read: /tmp/foo.go",
			wantRecords:   1,
			wantEmitType:  "tool_call",
			wantEmitCount: 1,
		},
		{
			name:          "Bash tool is logged",
			input:         `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git status"}}]}}`,
			wantLevel:     slog.LevelInfo,
			wantKey:       "claude_tool",
			wantValue:     "Bash: git status",
			wantRecords:   1,
			wantEmitType:  "tool_call",
			wantEmitCount: 1,
		},
		{
			name:          "MCP tool_use is skipped",
			input:         `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__contextmatrix__heartbeat","input":{"card_id":"CTXMAX-1"}}]}}`,
			wantRecords:   0,
			wantEmitCount: 0,
		},
		{
			name:          "system event is skipped",
			input:         `{"type":"system","subtype":"init","tools":["Read","Write"]}`,
			wantRecords:   0,
			wantEmitCount: 0,
		},
		{
			name:          "user event is skipped",
			input:         `{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
			wantRecords:   0,
			wantEmitCount: 0,
		},
		{
			name:          "rate_limit_event is skipped",
			input:         `{"type":"rate_limit_event","rate_limit_info":{}}`,
			wantRecords:   0,
			wantEmitCount: 0,
		},
		{
			name:          "empty line is skipped",
			input:         "",
			wantRecords:   0,
			wantEmitCount: 0,
		},
		{
			name:          "whitespace-only line is skipped",
			input:         "   ",
			wantRecords:   0,
			wantEmitCount: 0,
		},
		{
			// Signature-only thinking blocks have an empty thinking field.
			// The guard must drop them silently: 0 emits, 0 log records.
			name:          "empty thinking with signature is skipped",
			input:         `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"","signature":"abc"}]}}`,
			wantRecords:   0,
			wantEmitCount: 0,
		},
		{
			// A thinking block with actual content alongside a signature must
			// still emit exactly one thinking entry.
			name:          "non-empty thinking with signature is emitted",
			input:         `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"deep thoughts","signature":"abc"}]}}`,
			wantLevel:     slog.LevelInfo,
			wantKey:       "claude_thinking",
			wantValue:     "deep thoughts",
			wantRecords:   1,
			wantEmitType:  "thinking",
			wantEmitCount: 1,
		},
		{
			// A thinking block whose content matches a redact pattern must emit
			// once with the value replaced by [REDACTED].
			name:          "thinking matching redact pattern emits redacted content",
			input:         `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn"}]}}`,
			wantLevel:     slog.LevelInfo,
			wantKey:       "claude_thinking",
			wantValue:     "[REDACTED]",
			wantRecords:   1,
			wantEmitType:  "thinking",
			wantEmitCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, records := newTestLogger()

			var emitted []logbroadcast.LogEntry

			emit := func(e logbroadcast.LogEntry) {
				emitted = append(emitted, e)
			}

			ProcessStream(strings.NewReader(tc.input), logger, emit)

			assert.Len(t, *records, tc.wantRecords)
			assert.Len(t, emitted, tc.wantEmitCount)

			if tc.wantRecords > 0 && len(*records) > 0 {
				r := (*records)[0]
				assert.Equal(t, tc.wantLevel, r.Level)
				assert.Equal(t, tc.wantValue, attr(r, tc.wantKey))
			}

			if tc.wantEmitCount > 0 && len(emitted) > 0 {
				assert.Equal(t, tc.wantEmitType, emitted[0].Type)
			}
		})
	}
}

func TestProcessStream_MalformedJSON(t *testing.T) {
	// Two lines: one malformed, one valid. Processing should continue after
	// the bad line and still emit a record for the valid one.
	input := "not valid json\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}` + "\n"

	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	// Expect one warn (malformed) + one info (text block).
	assert.Len(t, *records, 2)
	assert.Equal(t, slog.LevelWarn, (*records)[0].Level)
	assert.Equal(t, slog.LevelInfo, (*records)[1].Level)
	assert.Equal(t, "ok", attr((*records)[1], "claude_text"))

	// Valid text block should be emitted.
	assert.Len(t, emitted, 1)
	assert.Equal(t, "text", emitted[0].Type)
	assert.Equal(t, "ok", emitted[0].Content)
}

func TestProcessStream_MultipleContentBlocks(t *testing.T) {
	// One assistant message with both a thinking and a text block.
	input := `{"type":"assistant","message":{"content":[` +
		`{"type":"thinking","thinking":"reasoning"},` +
		`{"type":"text","text":"answer"}` +
		`]}}`

	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	assert.Len(t, *records, 2)
	assert.Equal(t, "reasoning", attr((*records)[0], "claude_thinking"))
	assert.Equal(t, "answer", attr((*records)[1], "claude_text"))

	// Both blocks should be emitted.
	assert.Len(t, emitted, 2)
	assert.Equal(t, "thinking", emitted[0].Type)
	assert.Equal(t, "reasoning", emitted[0].Content)
	assert.Equal(t, "text", emitted[1].Type)
	assert.Equal(t, "answer", emitted[1].Content)
}

func TestProcessStream_LargeThinkingBlock(t *testing.T) {
	// Simulate a thinking block that exceeds the default scanner buffer (64 KiB).
	bigThinking := strings.Repeat("x", 512*1024) // 512 KiB
	line := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"` +
		bigThinking + `"}]}}`

	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(bytes.NewBufferString(line), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	assert.Len(t, *records, 1)
	assert.Equal(t, bigThinking, attr((*records)[0], "claude_thinking"))

	assert.Len(t, emitted, 1)
	assert.Equal(t, "thinking", emitted[0].Type)
	assert.Equal(t, bigThinking, emitted[0].Content)
}

func TestProcessStream_EmptyReader(t *testing.T) {
	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(""), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})
	assert.Empty(t, *records)
	assert.Empty(t, emitted)
}

func TestProcessStream_NilEmit(t *testing.T) {
	// Passing nil for emit should not panic and slog behavior should be unchanged.
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`

	logger, records := newTestLogger()
	ProcessStream(strings.NewReader(input), logger, nil)

	assert.Len(t, *records, 1)
	assert.Equal(t, "hello", attr((*records)[0], "claude_text"))
}

func TestRedact(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "GitHub PAT",
			input: "token: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn",
			want:  "token: [REDACTED]",
		},
		{
			name:  "GitHub server token",
			input: "using ghs_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn for auth",
			want:  "using [REDACTED] for auth",
		},
		{
			name:  "GitHub fine-grained PAT",
			input: "github_pat_ABCDEFGHIJKLMNOPQRSTUV1234",
			want:  "[REDACTED]",
		},
		{
			name:  "Anthropic key",
			input: "key is sk-ant-api03-abcdefghijklmnopqrstuvwxyz",
			want:  "key is [REDACTED]",
		},
		{
			name:  "Bearer token",
			input: "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abcdef",
			want:  "Authorization: [REDACTED]",
		},
		{
			// Regression: base64-padded Bearer tokens used to slip through
			// because '=' was missing from the character class. The redactor
			// must mask the entire opaque token including any '=' padding.
			name:  "Bearer token with base64 padding",
			input: "Authorization: Bearer dGVzdHRlc3R0ZXN0dGVzdHRlc3R0ZXN0==",
			want:  "Authorization: [REDACTED]",
		},
		{
			name:  "GitHub App installation token v1",
			input: "got token v1." + strings.Repeat("a", 40) + " back",
			want:  "got token [REDACTED] back",
		},
		{
			name:  "GitHub App installation token v1 longer hex",
			input: "v1." + strings.Repeat("0123456789abcdef", 4),
			want:  "[REDACTED]",
		},
		{
			name:  "CLAUDE_CODE_OAUTH_TOKEN env assignment preserves key",
			input: "env CLAUDE_CODE_OAUTH_TOKEN=sk-oauth-abc123xyz run",
			want:  "env CLAUDE_CODE_OAUTH_TOKEN=[REDACTED] run",
		},
		{
			name:  "CM_MCP_API_KEY env assignment preserves key",
			input: "CM_MCP_API_KEY=abc123def456 next-arg",
			want:  "CM_MCP_API_KEY=[REDACTED] next-arg",
		},
		{
			name:  "CM_GIT_TOKEN env assignment preserves key",
			input: "dumping CM_GIT_TOKEN=v1.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa now",
			want:  "dumping CM_GIT_TOKEN=[REDACTED] now",
		},
		{
			name:  "ANTHROPIC_API_KEY env assignment preserves key",
			input: "ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz",
			want:  "ANTHROPIC_API_KEY=[REDACTED]",
		},
		{
			name:  "no secrets",
			input: "normal log output with no secrets",
			want:  "normal log output with no secrets",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "short token-like strings not matched",
			input: "ghp_short is fine",
			want:  "ghp_short is fine",
		},
		{
			name:  "short v1.hex not matched (too few hex digits)",
			input: "version v1.abcdef is fine",
			want:  "version v1.abcdef is fine",
		},
		{
			name:  "Slack bot token (xoxb-)",
			input: "secret " + "xoxb" + "-1234567890-1234567890123-abcdefghijklmnopqrstuvwx in env",
			want:  "secret [REDACTED] in env",
		},
		{
			name:  "Slack user token (xoxp-)",
			input: "got " + "xoxp" + "-1234567890-1234567890-abcdefghijklmnopqrstuvwx now",
			want:  "got [REDACTED] now",
		},
		{
			name:  "AWS access key ID",
			input: "AKIAIOSFODNN7EXAMPLE in config",
			want:  "[REDACTED] in config",
		},
		{
			name:  "Google API key",
			input: "key=AIzaSyDdI0hCZtE6vySjMm-WEfRq3CPzqKqqsHI was here",
			want:  "key=[REDACTED] was here",
		},
		{
			name:  "NPM token",
			input: "npm_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ in .npmrc",
			want:  "[REDACTED] in .npmrc",
		},
		{
			name:  "generic JWT",
			input: "jwt eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c shown",
			want:  "jwt [REDACTED] shown",
		},
		{
			name:  "AWS short key not matched (15 chars after AKIA)",
			input: "AKIA1234567890ABCDE", // 14 chars after AKIA (one short)
			want:  "AKIA1234567890ABCDE",
		},
		{
			name:  "CM_MCP_API_KEY quoted value preserves key",
			input: `CM_MCP_API_KEY="abc 123 def" next-arg`,
			want:  `CM_MCP_API_KEY=[REDACTED] next-arg`,
		},
		{
			name:  "ANTHROPIC_API_KEY empty quoted value preserves key",
			input: `ANTHROPIC_API_KEY="" trailing`,
			want:  `ANTHROPIC_API_KEY=[REDACTED] trailing`,
		},
		{
			name:  "CLAUDE_CODE_OAUTH_TOKEN quoted with internal whitespace",
			input: `start CLAUDE_CODE_OAUTH_TOKEN="oauth value with spaces" end`,
			want:  `start CLAUDE_CODE_OAUTH_TOKEN=[REDACTED] end`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Redact(tc.input))
		})
	}
}

// TestRedactor_LiteralValues verifies that a Redactor masks literal secret
// values supplied at construction time in addition to the static patterns.
func TestRedactor_LiteralValues(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
		input   string
		want    string
	}{
		{
			name:    "literal git token masked verbatim",
			secrets: []string{"v1.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			input:   "user pasted v1.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa into log",
			want:    "user pasted [REDACTED] into log",
		},
		{
			name:    "literal MCP API key masked",
			secrets: []string{"my-literal-mcp-key-0123456789"},
			input:   "echo my-literal-mcp-key-0123456789 > /tmp/x",
			want:    "echo [REDACTED] > /tmp/x",
		},
		{
			name:    "empty secret ignored",
			secrets: []string{""},
			input:   "no secret here",
			want:    "no secret here",
		},
		{
			name:    "short secret ignored",
			secrets: []string{"abc"},
			input:   "abc def abc",
			want:    "abc def abc",
		},
		{
			name:    "duplicate secrets deduplicated",
			secrets: []string{"my-literal-mcp-key-0123456789", "my-literal-mcp-key-0123456789"},
			input:   "my-literal-mcp-key-0123456789",
			want:    "[REDACTED]",
		},
		{
			name:    "static pattern still applied via Redactor",
			secrets: []string{"irrelevant-longer-secret-value"},
			input:   "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn",
			want:    "[REDACTED]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRedactor(tc.secrets)
			assert.Equal(t, tc.want, r.Redact(tc.input))
		})
	}
}

// TestRedactor_NilFallback verifies that calling Redact on a nil *Redactor
// falls back to the static Redact (safe default).
func TestRedactor_NilFallback(t *testing.T) {
	var r *Redactor
	assert.Equal(t, "token is [REDACTED]",
		r.Redact("token is ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn"))
}

// TestProcessStreamWithRedactor_LiteralSecrets verifies that
// ProcessStreamWithRedactor masks literal container-injected secrets that
// happen to appear in a text block.
func TestProcessStreamWithRedactor_LiteralSecrets(t *testing.T) {
	liveToken := "live-instance-secret-value-0123456789"
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"oops leaking ` + liveToken + ` sorry"}]}}`

	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	r := NewRedactor([]string{liveToken})
	ProcessStreamWithRedactor(strings.NewReader(input), logger, r, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	}, nil)

	assert.Len(t, *records, 1)
	assert.NotContains(t, attr((*records)[0], "claude_text"), liveToken)
	assert.Contains(t, attr((*records)[0], "claude_text"), "[REDACTED]")

	assert.Len(t, emitted, 1)
	assert.NotContains(t, emitted[0].Content, liveToken)
}

func TestProcessStream_RedactsSecrets(t *testing.T) {
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"token is ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn"}]}}`

	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	assert.Len(t, *records, 1)
	assert.Equal(t, "token is [REDACTED]", attr((*records)[0], "claude_text"))

	// Emitted content should also be redacted.
	assert.Len(t, emitted, 1)
	assert.Equal(t, "text", emitted[0].Type)
	assert.Equal(t, "token is [REDACTED]", emitted[0].Content)
}

// TestFormatToolCall covers formatToolCall per-tool branches, whitespace
// collapsing, truncation, and fallback behaviour.
func TestFormatToolCall(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		// Bash: single-line command.
		{
			name:  "Bash single-line",
			tool:  "Bash",
			input: `{"command":"git status"}`,
			want:  "Bash: git status",
		},
		// Bash: multi-line command — keeps only first line.
		{
			name:  "Bash multi-line keeps first line",
			tool:  "Bash",
			input: `{"command":"git add .\ngit commit -m \"wip\""}`,
			want:  "Bash: git add .",
		},
		// Read uses file_path.
		{
			name:  "Read with file_path",
			tool:  "Read",
			input: `{"file_path":"/tmp/foo.go"}`,
			want:  "Read: /tmp/foo.go",
		},
		// Edit uses file_path.
		{
			name:  "Edit with file_path",
			tool:  "Edit",
			input: `{"file_path":"/src/main.go","old_string":"a","new_string":"b"}`,
			want:  "Edit: /src/main.go",
		},
		// Write uses file_path.
		{
			name:  "Write with file_path",
			tool:  "Write",
			input: `{"file_path":"/out/result.txt","content":"hello"}`,
			want:  "Write: /out/result.txt",
		},
		// Glob pattern only.
		{
			name:  "Glob pattern only",
			tool:  "Glob",
			input: `{"pattern":"**/*.go"}`,
			want:  "Glob: **/*.go",
		},
		// Glob pattern + path.
		{
			name:  "Glob pattern and path",
			tool:  "Glob",
			input: `{"pattern":"**/*.go","path":"/src"}`,
			want:  "Glob: **/*.go in /src",
		},
		// Grep pattern only.
		{
			name:  "Grep pattern only",
			tool:  "Grep",
			input: `{"pattern":"TODO"}`,
			want:  "Grep: TODO",
		},
		// Grep pattern + path.
		{
			name:  "Grep pattern and path",
			tool:  "Grep",
			input: `{"pattern":"TODO","path":"./internal"}`,
			want:  "Grep: TODO in ./internal",
		},
		// WebFetch uses url.
		{
			name:  "WebFetch url",
			tool:  "WebFetch",
			input: `{"url":"https://example.com/api"}`,
			want:  "WebFetch: https://example.com/api",
		},
		// WebSearch uses query.
		{
			name:  "WebSearch query",
			tool:  "WebSearch",
			input: `{"query":"golang context cancellation"}`,
			want:  "WebSearch: golang context cancellation",
		},
		// Task uses description.
		{
			name:  "Task description",
			tool:  "Task",
			input: `{"description":"implement feature X"}`,
			want:  "Task: implement feature X",
		},
		// Agent uses description.
		{
			name:  "Agent description",
			tool:  "Agent",
			input: `{"description":"run linter on all files"}`,
			want:  "Agent: run linter on all files",
		},
		// TodoWrite: 3-element todos array.
		{
			name:  "TodoWrite 3 todos",
			tool:  "TodoWrite",
			input: `{"todos":[{"id":"1","content":"a"},{"id":"2","content":"b"},{"id":"3","content":"c"}]}`,
			want:  "TodoWrite: 3 todos",
		},
		// Unknown tool falls back to compact JSON.
		{
			name:  "Unknown tool compact JSON fallback",
			tool:  "MyCustomTool",
			input: `{"foo":"bar"}`,
			want:  `MyCustomTool: {"foo":"bar"}`,
		},
		// Empty object → name only.
		{
			name:  "empty object returns name only",
			tool:  "Read",
			input: `{}`,
			want:  "Read",
		},
		// Null input → name only.
		{
			name:  "null input returns name only",
			tool:  "Read",
			input: `null`,
			want:  "Read",
		},
		// Missing input (empty raw message) → name only.
		{
			name:  "absent input returns name only",
			tool:  "Read",
			input: ``,
			want:  "Read",
		},
		// Bash with missing command field → name only (no crash).
		{
			name:  "Bash missing command returns name only",
			tool:  "Bash",
			input: `{"description":"something else"}`,
			want:  `Bash: {"description":"something else"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatToolCall(tc.tool, json.RawMessage(tc.input))
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestFormatToolCall_Truncation verifies that a summary exceeding maxToolCallLen
// runes is truncated to exactly maxToolCallLen runes with a trailing ellipsis.
func TestFormatToolCall_Truncation(t *testing.T) {
	// Build a command that will produce a formatted string > 200 runes.
	// "Bash: " is 6 chars, so the command must be >= 195 chars to trigger truncation.
	longCmd := strings.Repeat("x", 210)
	input := `{"command":"` + longCmd + `"}`

	got := formatToolCall("Bash", json.RawMessage(input))
	runes := []rune(got)

	// Must be exactly maxToolCallLen + 1 rune (the ellipsis character '…').
	assert.Len(t, runes, maxToolCallLen+1, "truncated string should be maxToolCallLen runes + ellipsis")
	assert.Equal(t, '…', runes[len(runes)-1], "last rune should be ellipsis")
}

// TestFormatToolCall_Truncation_InProcessStream ensures the truncated value
// is what ProcessStream logs and emits.
func TestFormatToolCall_Truncation_InProcessStream(t *testing.T) {
	longCmd := strings.Repeat("a", 210)
	input := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"` + longCmd + `"}}]}}`

	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	assert.Len(t, *records, 1)
	logged := attr((*records)[0], "claude_tool")
	loggedRunes := []rune(logged)
	assert.Len(t, loggedRunes, maxToolCallLen+1)
	assert.Equal(t, '…', loggedRunes[len(loggedRunes)-1])

	assert.Len(t, emitted, 1)
	assert.Equal(t, logged, emitted[0].Content)
}

// TestProcessStream_ToolUseRedactsSecrets verifies that secrets in tool
// arguments are redacted in both the slog value and the emitted content.
func TestProcessStream_ToolUseRedactsSecrets(t *testing.T) {
	secret := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn"
	input := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo ` + secret + `"}}]}}`

	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	assert.Len(t, *records, 1)
	logged := attr((*records)[0], "claude_tool")
	assert.NotContains(t, logged, secret, "slog value must not contain the raw secret")
	assert.Contains(t, logged, "[REDACTED]", "slog value must contain [REDACTED]")

	assert.Len(t, emitted, 1)
	assert.NotContains(t, emitted[0].Content, secret, "emitted content must not contain the raw secret")
	assert.Contains(t, emitted[0].Content, "[REDACTED]", "emitted content must contain [REDACTED]")
}

// parseAllToCollect is a test helper that runs ProcessStreamWithRedactor on
// the given stream-json input and collects all emitted events into a []any
// slice. Both logbroadcast.LogEntry values and *SkillEngagedEvent values are
// included so callers can type-assert against whichever they need.
func parseAllToCollect(t *testing.T, input string) []any {
	t.Helper()

	logger, _ := newTestLogger()

	var events []any

	emitLog := func(e logbroadcast.LogEntry) {
		events = append(events, e)
	}

	emitSkill := func(e *SkillEngagedEvent) {
		events = append(events, e)
	}

	ProcessStreamWithRedactor(strings.NewReader(input), logger, nil, emitLog, emitSkill)

	return events
}

func TestParser_DetectsSkillEngagement(t *testing.T) {
	// stream-json fragment with Skill tool_use using the "skill" input key.
	streamJSON := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"abc","name":"Skill","input":{"skill":"go-development"}}]}}` + "\n"

	events := parseAllToCollect(t, streamJSON)

	var found *SkillEngagedEvent

	for _, e := range events {
		if se, ok := e.(*SkillEngagedEvent); ok {
			found = se

			break
		}
	}

	require.NotNil(t, found, "expected a SkillEngagedEvent for Skill tool_use")
	assert.Equal(t, "go-development", found.SkillName)
}

func TestParser_DetectsSkillEngagement_NameKey(t *testing.T) {
	// Fallback: CC may use "name" instead of "skill" as the input key.
	streamJSON := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"abc","name":"Skill","input":{"name":"go-development"}}]}}` + "\n"

	events := parseAllToCollect(t, streamJSON)

	var found *SkillEngagedEvent

	for _, e := range events {
		if se, ok := e.(*SkillEngagedEvent); ok {
			found = se

			break
		}
	}

	require.NotNil(t, found, "expected a SkillEngagedEvent when using 'name' input key")
	assert.Equal(t, "go-development", found.SkillName)
}

func TestParser_IgnoresOtherToolUseEvents(t *testing.T) {
	streamJSON := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Edit","input":{}}]}}` + "\n"

	events := parseAllToCollect(t, streamJSON)

	for _, e := range events {
		_, isSkill := e.(*SkillEngagedEvent)
		assert.False(t, isSkill, "non-Skill tool_use must not produce SkillEngagedEvent")
	}
}

// TestProcessStream_EmitsUsageEntry verifies the wire shape of the "usage"
// LogEntry pinned to CM's chat-manager contract. The JSON tag mapping is
// load-bearing: a typo on `cache_creation_input_tokens` →
// `cache_creation_tokens` would silently zero out the prompt-size column in
// the CM context-window indicator.
func TestProcessStream_EmitsUsageEntry(t *testing.T) {
	// stream-json assistant event with a usage block and a single text
	// content block (so we know the usage entry is emitted in addition to
	// the text entry, not in place of it).
	input := `{"type":"assistant","message":{"model":"claude-opus-4","usage":{` +
		`"input_tokens":11,"output_tokens":22,` +
		`"cache_read_input_tokens":33,"cache_creation_input_tokens":44},` +
		`"content":[{"type":"text","text":"ack"}]}}`

	logger, _ := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	// Two entries expected: one "usage" + one "text".
	require.Len(t, emitted, 2)

	usage := emitted[0]
	assert.Equal(t, "usage", usage.Type)
	assert.Equal(t, "claude-opus-4", usage.Model)
	assert.Empty(t, usage.Content, "usage entry must not carry Content")
	require.NotNil(t, usage.Usage, "usage entry must carry a non-nil Usage block")
	assert.Equal(t, int64(11), usage.Usage.InputTokens)
	assert.Equal(t, int64(22), usage.Usage.OutputTokens)
	assert.Equal(t, int64(33), usage.Usage.CacheReadTokens)
	// Critical mapping: stream-json sends `cache_creation_input_tokens`,
	// wire shape exposes it as `cache_creation_tokens`.
	assert.Equal(t, int64(44), usage.Usage.CacheCreateTokens)

	text := emitted[1]
	assert.Equal(t, "text", text.Type)
	assert.Equal(t, "ack", text.Content)
}

// TestProcessStream_UsageEntryWireJSONShape pins the JSON marshal shape that
// CM's chat-manager unmarshals. Any rename of a TokenUsage field tag would
// break the cross-repo contract without a corresponding migration; the
// assertion captures the exact key names CM relies on.
func TestProcessStream_UsageEntryWireJSONShape(t *testing.T) {
	input := `{"type":"assistant","message":{"model":"claude-opus-4","usage":{` +
		`"input_tokens":1,"output_tokens":2,` +
		`"cache_read_input_tokens":3,"cache_creation_input_tokens":4},` +
		`"content":[]}}`

	logger, _ := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	require.Len(t, emitted, 1)
	require.Equal(t, "usage", emitted[0].Type)

	raw, err := json.Marshal(emitted[0])
	require.NoError(t, err)

	got := string(raw)
	assert.Contains(t, got, `"input_tokens":1`)
	assert.Contains(t, got, `"output_tokens":2`)
	assert.Contains(t, got, `"cache_read_tokens":3`)
	assert.Contains(t, got, `"cache_creation_tokens":4`)
	assert.Contains(t, got, `"model":"claude-opus-4"`)
	assert.Contains(t, got, `"type":"usage"`)
	// Content must be omitted (omitempty on a usage-only entry).
	assert.NotContains(t, got, `"content":`)
}

// TestProcessStream_OversizedLine_ParsesSuccessfully verifies that a single
// stream-json line larger than the legacy bufio.Scanner cap (1 MiB) is
// processed normally instead of terminating the parser. The previous
// behaviour silently dropped the rest of Claude's output and emitted a
// "scan terminated" system LogEntry; with the bufio.Reader swap a long
// thinking block parses through.
func TestProcessStream_OversizedLine_ParsesSuccessfully(t *testing.T) {
	// 2 MiB > the old 1 MiB scanner cap; with bufio.Scanner this would
	// have surfaced as bufio.ErrTooLong instead of the parsed thinking
	// block we expect.
	longThinking := strings.Repeat("x", 2*1024*1024)
	payload, err := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{"type": "thinking", "thinking": longThinking},
			},
		},
	})
	require.NoError(t, err)

	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(bytes.NewReader(append(payload, '\n')), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	for _, r := range *records {
		assert.Less(t, int(r.Level), int(slog.LevelError),
			"no error-level records expected; got level=%v message=%q", r.Level, r.Message)
	}

	require.Len(t, emitted, 1, "the oversized thinking block should emit exactly one entry")
	assert.Equal(t, "thinking", emitted[0].Type)
	assert.Equal(t, longThinking, emitted[0].Content)
}

// TestProcessStream_ReadError_EmitsSystemEntry verifies that a non-EOF read
// error from the underlying reader still surfaces as a system LogEntry.
// Removing the bufio.Scanner cap eliminated the ErrTooLong diagnostic, but
// the channel for genuine I/O failures must remain.
func TestProcessStream_ReadError_EmitsSystemEntry(t *testing.T) {
	sentinel := errors.New("synthetic stream failure")
	r := &errAfterReader{data: []byte("\n"), err: sentinel}

	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(r, logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	var sawError bool

	for _, rec := range *records {
		if rec.Level >= slog.LevelError {
			sawError = true

			break
		}
	}

	assert.True(t, sawError, "non-EOF read error must surface as Error log record")

	var sysEntry *logbroadcast.LogEntry

	for i := range emitted {
		if emitted[i].Type == "system" {
			sysEntry = &emitted[i]

			break
		}
	}

	require.NotNil(t, sysEntry, "non-EOF read error must emit a system LogEntry")
	assert.Contains(t, sysEntry.Content, "logparser")
	assert.Contains(t, sysEntry.Content, "stream read terminated")
	assert.Contains(t, sysEntry.Content, "synthetic stream failure")
}

// TestProcessStream_ReadError_NilEmit verifies a non-EOF read error logs at
// Error level even when the emit callback is nil — the broadcast path
// must not nil-panic.
func TestProcessStream_ReadError_NilEmit(t *testing.T) {
	sentinel := errors.New("synthetic stream failure")
	r := &errAfterReader{data: []byte("\n"), err: sentinel}

	logger, records := newTestLogger()
	ProcessStream(r, logger, nil)

	var sawError bool

	for _, rec := range *records {
		if rec.Level >= slog.LevelError {
			sawError = true

			break
		}
	}

	assert.True(t, sawError, "non-EOF read error must surface even when emit is nil")
}

// errAfterReader yields data once, then returns err on subsequent reads.
// Used to simulate a non-EOF read failure mid-stream.
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]

		return n, nil
	}

	return 0, r.err
}

// TestProcessStream_EmptyThinkingWithSiblingText verifies that an empty
// thinking block followed by a text block in the same content[] array emits
// only the text entry. This guards against using `break` instead of
// `continue` in the thinking handler — `break` would also skip the text.
func TestProcessStream_EmptyThinkingWithSiblingText(t *testing.T) {
	input := `{"type":"assistant","message":{"content":[` +
		`{"type":"thinking","thinking":"","signature":"sig123"},` +
		`{"type":"text","text":"visible answer"}` +
		`]}}`

	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	// Only the text record must be logged — the empty thinking must not appear.
	assert.Len(t, *records, 1)
	assert.Equal(t, "visible answer", attr((*records)[0], "claude_text"))

	// Only one emit: the text entry.
	assert.Len(t, emitted, 1)
	assert.Equal(t, "text", emitted[0].Type)
	assert.Equal(t, "visible answer", emitted[0].Content)
}

// TestProcessStream_AskUserQuestion_SingleQuestion verifies that an
// AskUserQuestion tool_use is emitted as a "user_question" LogEntry with
// the structured payload preserved as compact JSON — not routed through
// formatToolCall's 200-rune-truncated path.
func TestProcessStream_AskUserQuestion_SingleQuestion(t *testing.T) {
	input := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_single01","name":"AskUserQuestion","input":{` +
		`"questions":[{"question":"Which library should we use?","header":"Library","multiSelect":false,` +
		`"options":[{"label":"date-fns","description":"Functional, tree-shakeable"},` +
		`{"label":"luxon","description":"Object-oriented with timezone support"}]}]}}]}}`

	logger, records := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	require.Len(t, *records, 1)
	assert.Equal(t, slog.LevelInfo, (*records)[0].Level)
	assert.NotEmpty(t, attr((*records)[0], "claude_user_question"))

	require.Len(t, emitted, 1)
	assert.Equal(t, "user_question", emitted[0].Type)

	var got askUserQuestionPayload

	require.NoError(t, json.Unmarshal([]byte(emitted[0].Content), &got))
	require.Len(t, got.Questions, 1)
	assert.Equal(t, "Which library should we use?", got.Questions[0].Question)
	assert.Equal(t, "Library", got.Questions[0].Header)
	assert.False(t, got.Questions[0].MultiSelect)
	require.Len(t, got.Questions[0].Options, 2)
	assert.Equal(t, "date-fns", got.Questions[0].Options[0].Label)
	assert.Equal(t, "luxon", got.Questions[0].Options[1].Label)
}

// TestProcessStream_AskUserQuestion_MultiQuestion verifies that multiple
// questions in one AskUserQuestion call are preserved in order, and that
// multiSelect=true round-trips.
func TestProcessStream_AskUserQuestion_MultiQuestion(t *testing.T) {
	input := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_multi01","name":"AskUserQuestion","input":{` +
		`"questions":[` +
		`{"question":"Q1?","header":"H1","multiSelect":false,"options":[{"label":"a"},{"label":"b"}]},` +
		`{"question":"Q2?","header":"H2","multiSelect":true,"options":[{"label":"x"},{"label":"y"},{"label":"z"}]}` +
		`]}}]}}`

	logger, _ := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	require.Len(t, emitted, 1)
	assert.Equal(t, "user_question", emitted[0].Type)

	var got askUserQuestionPayload

	require.NoError(t, json.Unmarshal([]byte(emitted[0].Content), &got))
	require.Len(t, got.Questions, 2)
	assert.Equal(t, "Q1?", got.Questions[0].Question)
	assert.False(t, got.Questions[0].MultiSelect)
	assert.Equal(t, "Q2?", got.Questions[1].Question)
	assert.True(t, got.Questions[1].MultiSelect)
	assert.Len(t, got.Questions[1].Options, 3)
}

// TestProcessStream_AskUserQuestion_PropagatesToolUseID verifies that the
// stream-json tool_use `id` is captured and surfaced on the emitted LogEntry's
// ToolUseID, so the chat layer can later send a `tool_result` frame back to
// Claude that closes the tool call loop. Without this, the answer would
// arrive as plain user text against an open tool_use and Claude would
// rationalise the situation as "the tool isn't available".
func TestProcessStream_AskUserQuestion_PropagatesToolUseID(t *testing.T) {
	const toolUseID = "toolu_01ABCDEFGHIJKLMNOPQRSTUV"

	input := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + toolUseID + `","name":"AskUserQuestion","input":{` +
		`"questions":[{"question":"Pick one","options":[{"label":"a"},{"label":"b"}]}]}}]}}`

	logger, _ := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	require.Len(t, emitted, 1)
	assert.Equal(t, "user_question", emitted[0].Type)
	assert.Equal(t, toolUseID, emitted[0].ToolUseID,
		"AskUserQuestion tool_use id must propagate to the LogEntry's ToolUseID")
}

// TestProcessStream_AskUserQuestion_MissingToolUseIDFallsBack verifies that
// a structurally valid AskUserQuestion tool_use without an "id" field falls
// through to the generic tool_call path rather than being emitted as a
// user_question. Emitting a user_question without an id would surface a
// question card in the chat UI that the runner cannot answer (no tool_use_id
// to reference in a tool_result frame), and the operator's eventual reply
// would arrive as plain user text against an open tool_use — the exact
// failure mode this feature was added to prevent.
func TestProcessStream_AskUserQuestion_MissingToolUseIDFallsBack(t *testing.T) {
	// Structurally valid input, but no "id" on the tool_use block.
	input := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{` +
		`"questions":[{"question":"Pick one","options":[{"label":"a"},{"label":"b"}]}]}}]}}`

	logger, _ := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	require.Len(t, emitted, 1)
	assert.Equal(t, "tool_call", emitted[0].Type,
		"AskUserQuestion without a tool_use id must fall back to the generic tool_call path")
	assert.Empty(t, emitted[0].ToolUseID,
		"fallback tool_call must not carry an empty-string ToolUseID either")
	assert.Contains(t, emitted[0].Content, "AskUserQuestion")
}

// TestProcessStream_AskUserQuestion_MalformedFallsBackToToolCall verifies
// that an AskUserQuestion payload that fails to parse still surfaces — via
// the generic tool_call path — rather than being silently dropped.
func TestProcessStream_AskUserQuestion_MalformedFallsBackToToolCall(t *testing.T) {
	// "questions" is a string instead of an array — Unmarshal will fail.
	input := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":"oops"}}]}}`

	logger, _ := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	require.Len(t, emitted, 1)
	assert.Equal(t, "tool_call", emitted[0].Type,
		"malformed AskUserQuestion must fall back to the generic tool_call path")
	assert.Contains(t, emitted[0].Content, "AskUserQuestion")
}

// TestProcessStream_AskUserQuestion_EmptyQuestionsFallsBack verifies that
// a payload with an empty questions array falls back to tool_call so the
// event still appears in the stream.
func TestProcessStream_AskUserQuestion_EmptyQuestionsFallsBack(t *testing.T) {
	input := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[]}}]}}`

	logger, _ := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	require.Len(t, emitted, 1)
	assert.Equal(t, "tool_call", emitted[0].Type)
}

// TestProcessStream_AskUserQuestion_RedactsSecrets verifies that secret
// strings embedded inside an AskUserQuestion question survive on the wire
// and then get redacted via the standard pipeline before emission. The
// table covers each secretPatterns family that has non-trivial
// JSON-encoding interaction (Bearer/JWT/AWS keys use character classes
// that JSON-string encoding preserves byte-for-byte; env-var-shaped
// secrets are the one path with non-obvious encoding behaviour).
func TestProcessStream_AskUserQuestion_RedactsSecrets(t *testing.T) {
	tests := []struct {
		name   string
		secret string
	}{
		{name: "github personal access token", secret: "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn"},
		{name: "bearer token", secret: "Bearer abc123XYZabc123XYZabc123XYZ"},
		{name: "JWT", secret: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NSJ9.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"},
		{name: "AWS access key id", secret: "AKIAIOSFODNN7EXAMPLE"},
		{name: "CM env-var secret", secret: "CM_MCP_API_KEY=abc123XYZsecretvalue"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_secret01","name":"AskUserQuestion","input":{` +
				`"questions":[{"question":"Use this token: ` + tc.secret + `?","options":[{"label":"yes"},{"label":"no"}]}]}}]}}`

			logger, _ := newTestLogger()

			var emitted []logbroadcast.LogEntry

			ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
				emitted = append(emitted, e)
			})

			require.Len(t, emitted, 1)
			assert.Equal(t, "user_question", emitted[0].Type)
			assert.NotContains(t, emitted[0].Content, tc.secret,
				"raw secret must not appear in the emitted user_question content")
			assert.Contains(t, emitted[0].Content, "[REDACTED]",
				"redactor must replace the secret with [REDACTED]")
		})
	}
}

// TestValidateAskUserQuestion_FallbackContract pins the contract used by
// ProcessStream: nil/empty/invalid inputs must return false so the
// caller routes to the generic tool_call path.
func TestValidateAskUserQuestion_FallbackContract(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "empty input", input: ``, want: false},
		{name: "whitespace only", input: `   `, want: false},
		{name: "invalid JSON", input: `{not json`, want: false},
		{name: "missing questions key", input: `{}`, want: false},
		{name: "empty questions array", input: `{"questions":[]}`, want: false},
		{name: "wrong type for questions", input: `{"questions":"oops"}`, want: false},
		{
			// Per-question shape check: nil/missing options crashes the
			// frontend's options.map(...). Reject upstream so the event
			// surfaces via the tool_call fallback path instead.
			name:  "question with missing options field",
			input: `{"questions":[{"question":"hi"}]}`,
			want:  false,
		},
		{
			// Same shape with an explicit null options — equivalent to missing.
			name:  "question with null options",
			input: `{"questions":[{"question":"hi","options":null}]}`,
			want:  false,
		},
		{
			// Empty options array is intentionally allowed — the model can
			// ask an open question and let the user type free-text.
			name:  "question with empty options array",
			input: `{"questions":[{"question":"hi","options":[]}]}`,
			want:  true,
		},
		{
			name:  "valid single question",
			input: `{"questions":[{"question":"hi","options":[{"label":"a"}]}]}`,
			want:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, validateAskUserQuestion(json.RawMessage(tc.input)))
		})
	}
}

// TestProcessStream_AskUserQuestion_MissingOptionsFallsBack is the
// integration-side counterpart to the contract table above — when a
// payload arrives with a question missing its options field, the runner
// emits the generic tool_call entry rather than dropping or crashing.
func TestProcessStream_AskUserQuestion_MissingOptionsFallsBack(t *testing.T) {
	input := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[{"question":"hi"}]}}]}}`

	logger, _ := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	require.Len(t, emitted, 1)
	assert.Equal(t, "tool_call", emitted[0].Type,
		"missing options must fall back to the generic tool_call path")
}

// TestProcessStream_AskUserQuestion_PreservesUnknownFields verifies that
// unknown fields on the wire survive the runner. We validate via the typed
// struct but emit the original input bytes, so a future Claude Code field
// like `priority` does not get silently dropped.
func TestProcessStream_AskUserQuestion_PreservesUnknownFields(t *testing.T) {
	input := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_unknown01","name":"AskUserQuestion","input":{` +
		`"questions":[{"question":"Q?","priority":"high","options":[{"label":"a","style":"primary"}]}],` +
		`"meta":"experimental"}}]}}`

	logger, _ := newTestLogger()

	var emitted []logbroadcast.LogEntry

	ProcessStream(strings.NewReader(input), logger, func(e logbroadcast.LogEntry) {
		emitted = append(emitted, e)
	})

	require.Len(t, emitted, 1)
	assert.Equal(t, "user_question", emitted[0].Type)
	assert.Contains(t, emitted[0].Content, `"priority":"high"`,
		"unknown per-question field must survive on the wire")
	assert.Contains(t, emitted[0].Content, `"style":"primary"`,
		"unknown per-option field must survive on the wire")
	assert.Contains(t, emitted[0].Content, `"meta":"experimental"`,
		"unknown top-level field must survive on the wire")
}
