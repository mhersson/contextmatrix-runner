package logparser

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

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
		name           string
		input          string
		wantLevel      slog.Level
		wantKey        string
		wantValue      string
		wantRecords    int
		wantEmitType   string
		wantEmitCount  int
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Redact(tc.input))
		})
	}
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
	assert.Equal(t, maxToolCallLen+1, len(runes), "truncated string should be maxToolCallLen runes + ellipsis")
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
	assert.Equal(t, maxToolCallLen+1, len(loggedRunes))
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
