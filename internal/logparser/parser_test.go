package logparser

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
		name        string
		input       string
		wantLevel   slog.Level
		wantKey     string
		wantValue   string
		wantRecords int
	}{
		{
			name:        "text block is logged",
			input:       `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}`,
			wantLevel:   slog.LevelInfo,
			wantKey:     "claude_text",
			wantValue:   "Hello world",
			wantRecords: 1,
		},
		{
			name:        "thinking block is logged",
			input:       `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"deep thoughts"}]}}`,
			wantLevel:   slog.LevelInfo,
			wantKey:     "claude_thinking",
			wantValue:   "deep thoughts",
			wantRecords: 1,
		},
		{
			name:        "non-MCP tool_use is logged",
			input:       `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{}}]}}`,
			wantLevel:   slog.LevelInfo,
			wantKey:     "claude_tool",
			wantValue:   "Read",
			wantRecords: 1,
		},
		{
			name:        "Bash tool is logged",
			input:       `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]}}`,
			wantLevel:   slog.LevelInfo,
			wantKey:     "claude_tool",
			wantValue:   "Bash",
			wantRecords: 1,
		},
		{
			name:        "MCP tool_use is skipped",
			input:       `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__contextmatrix__heartbeat","input":{}}]}}`,
			wantRecords: 0,
		},
		{
			name:        "system event is skipped",
			input:       `{"type":"system","subtype":"init","tools":["Read","Write"]}`,
			wantRecords: 0,
		},
		{
			name:        "user event is skipped",
			input:       `{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
			wantRecords: 0,
		},
		{
			name:        "rate_limit_event is skipped",
			input:       `{"type":"rate_limit_event","rate_limit_info":{}}`,
			wantRecords: 0,
		},
		{
			name:        "empty line is skipped",
			input:       "",
			wantRecords: 0,
		},
		{
			name:        "whitespace-only line is skipped",
			input:       "   ",
			wantRecords: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, records := newTestLogger()
			ProcessStream(strings.NewReader(tc.input), logger)

			assert.Len(t, *records, tc.wantRecords)

			if tc.wantRecords > 0 && len(*records) > 0 {
				r := (*records)[0]
				assert.Equal(t, tc.wantLevel, r.Level)
				assert.Equal(t, tc.wantValue, attr(r, tc.wantKey))
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
	ProcessStream(strings.NewReader(input), logger)

	// Expect one warn (malformed) + one info (text block).
	assert.Len(t, *records, 2)
	assert.Equal(t, slog.LevelWarn, (*records)[0].Level)
	assert.Equal(t, slog.LevelInfo, (*records)[1].Level)
	assert.Equal(t, "ok", attr((*records)[1], "claude_text"))
}

func TestProcessStream_MultipleContentBlocks(t *testing.T) {
	// One assistant message with both a thinking and a text block.
	input := `{"type":"assistant","message":{"content":[` +
		`{"type":"thinking","thinking":"reasoning"},` +
		`{"type":"text","text":"answer"}` +
		`]}}`

	logger, records := newTestLogger()
	ProcessStream(strings.NewReader(input), logger)

	assert.Len(t, *records, 2)
	assert.Equal(t, "reasoning", attr((*records)[0], "claude_thinking"))
	assert.Equal(t, "answer", attr((*records)[1], "claude_text"))
}

func TestProcessStream_LargeThinkingBlock(t *testing.T) {
	// Simulate a thinking block that exceeds the default scanner buffer (64 KiB).
	bigThinking := strings.Repeat("x", 512*1024) // 512 KiB
	line := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"` +
		bigThinking + `"}]}}`

	logger, records := newTestLogger()
	ProcessStream(bytes.NewBufferString(line), logger)

	assert.Len(t, *records, 1)
	assert.Equal(t, bigThinking, attr((*records)[0], "claude_thinking"))
}

func TestProcessStream_EmptyReader(t *testing.T) {
	logger, records := newTestLogger()
	ProcessStream(strings.NewReader(""), logger)
	assert.Empty(t, *records)
}
