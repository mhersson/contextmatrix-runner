package claudeclient

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseStreamJSONEvents(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess_xyz","model":"claude-sonnet-4-6"}`,
		`{"type":"text","text":"hello"}`,
		`{"type":"thinking","text":"considering"}`,
		`{"type":"tool_use","name":"Read","input":{"path":"foo.go"}}`,
		`{"type":"tool_result","tool_use_id":"abc","content":"file contents"}`,
		`{"type":"system","subtype":"end","usage":{"input_tokens":120,"output_tokens":45}}`,
	}, "\n") + "\n"

	parser := NewStreamJSONParser(io.NopCloser(bytes.NewReader([]byte(in))))

	var events []StreamEvent
	for ev := range parser.Events() {
		events = append(events, ev)
	}

	require.NoError(t, parser.Err())
	require.Len(t, events, 6)
	require.Equal(t, EventSystemInit, events[0].Kind)
	require.Equal(t, "sess_xyz", events[0].SessionID)
	require.Equal(t, "claude-sonnet-4-6", events[0].Model)
	require.Equal(t, EventText, events[1].Kind)
	require.Equal(t, "hello", events[1].Text)
	require.Equal(t, EventThinking, events[2].Kind)
	require.Equal(t, EventToolUse, events[3].Kind)
	require.Equal(t, "Read", events[3].ToolName)
	require.NotEmpty(t, events[3].ToolInput)
	require.JSONEq(t, `{"path":"foo.go"}`, string(events[3].ToolInput))
	require.Equal(t, EventToolResult, events[4].Kind)
	require.Equal(t, "abc", events[4].ToolUseID)
	require.Equal(t, "file contents", events[4].Content)
	require.Equal(t, EventSystemEnd, events[5].Kind)
	require.Equal(t, 120, events[5].Usage.InputTokens)
	require.Equal(t, 45, events[5].Usage.OutputTokens)
	require.Equal(t, "claude-sonnet-4-6", events[5].Usage.Model)
}

func TestParseStreamJSONEndEventWithExplicitModel(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s","model":"claude-sonnet-4-6"}`,
		`{"type":"system","subtype":"end","model":"claude-opus-4-7","usage":{"input_tokens":1,"output_tokens":2}}`,
	}, "\n") + "\n"

	parser := NewStreamJSONParser(io.NopCloser(bytes.NewReader([]byte(in))))

	var events []StreamEvent
	for ev := range parser.Events() {
		events = append(events, ev)
	}

	require.NoError(t, parser.Err())
	require.Len(t, events, 2)
	require.Equal(t, "claude-opus-4-7", events[1].Usage.Model)
}

func TestParseStreamJSONHandlesPartialLines(t *testing.T) {
	in := `{"type":"text","text":"a"}` + "\n" + `{"type":"text","text":"b"}`
	parser := NewStreamJSONParser(io.NopCloser(bytes.NewReader([]byte(in))))

	count := 0
	for range parser.Events() {
		count++
	}

	require.NoError(t, parser.Err())
	require.Equal(t, 2, count)
}

func TestParseStreamJSONInvalidLineSetsErr(t *testing.T) {
	in := `{"type":"text","text":"a"}` + "\n" + `not-json` + "\n"

	parser := NewStreamJSONParser(io.NopCloser(bytes.NewReader([]byte(in))))

	count := 0
	for range parser.Events() {
		count++
	}

	require.Error(t, parser.Err())
	require.Equal(t, 1, count, "should emit the valid event before stopping at the invalid one")
}

func TestParseStreamJSONEmptyLines(t *testing.T) {
	// Empty lines between events should be silently skipped.
	in := "\n\n" + `{"type":"text","text":"a"}` + "\n\n" + `{"type":"text","text":"b"}` + "\n"
	parser := NewStreamJSONParser(io.NopCloser(bytes.NewReader([]byte(in))))

	count := 0
	for range parser.Events() {
		count++
	}

	require.NoError(t, parser.Err())
	require.Equal(t, 2, count)
}

func TestParseStreamJSONErrorEvent(t *testing.T) {
	in := `{"type":"error","message":"something broke"}` + "\n"
	parser := NewStreamJSONParser(io.NopCloser(bytes.NewReader([]byte(in))))

	var got StreamEvent
	for ev := range parser.Events() {
		got = ev
	}

	require.NoError(t, parser.Err())
	require.Equal(t, EventError, got.Kind)
	require.Contains(t, got.Text, "something broke")
}

func TestStreamJSONParserClose(t *testing.T) {
	// Close on an idle parser must unblock the run goroutine and close
	// the events channel cleanly, so an abandoned caller does not leak
	// a goroutine blocked in Read.
	pr, pw := io.Pipe()
	parser := NewStreamJSONParser(pr)
	require.NoError(t, parser.Close())

	// Closing the writer side after the parser closes the reader must
	// not panic; PipeWriter tolerates Close after the reader closed.
	_ = pw.Close()

	// Events channel must close in finite time.
	done := make(chan struct{})

	go func() {
		for range parser.Events() { //revive:disable-line:empty-block
		}

		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(time.Second):
		t.Fatal("events channel did not close after parser.Close()")
	}
}
