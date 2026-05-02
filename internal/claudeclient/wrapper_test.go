package claudeclient

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// discardCloser is a no-op io.WriteCloser used by the fake exec when the
// test doesn't care about stdin.
type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardCloser) Close() error                { return nil }

type fakeDockerExec struct {
	mu          sync.Mutex
	lastCmd     []string
	lastEnv     []string
	stdoutWrite *io.PipeWriter
	stdoutRead  *io.PipeReader
}

func newFakeDockerExec() *fakeDockerExec {
	pr, pw := io.Pipe()

	return &fakeDockerExec{stdoutWrite: pw, stdoutRead: pr}
}

func (f *fakeDockerExec) ExecCreate(_ context.Context, _ string, cfg ExecConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastCmd = cfg.Cmd
	f.lastEnv = cfg.Env

	return "exec-id-1", nil
}

func (f *fakeDockerExec) ExecAttach(_ context.Context, _ string) (io.ReadCloser, io.WriteCloser, error) {
	return io.NopCloser(f.stdoutRead), discardCloser{}, nil
}

func TestWrapperSpawnsClaudeWithCorrectFlags(t *testing.T) {
	fake := newFakeDockerExec()
	w := NewWrapperWithExecAPI(fake, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proc, err := w.Spawn(ctx, SpawnOptions{
		Container:    "wc-test",
		SystemPrompt: "you are a planner",
		Model:        "claude-opus-4-7",
		AllowedTools: []string{"Read", "Glob"},
		WorkingDir:   "/workspace",
	})
	require.NoError(t, err)
	require.NotNil(t, proc)

	require.Contains(t, fake.lastCmd, "claude")
	require.Contains(t, fake.lastCmd, "--print")
	require.Contains(t, fake.lastCmd, "--input-format")
	require.Contains(t, fake.lastCmd, "stream-json")
	require.Contains(t, fake.lastCmd, "--output-format")
	require.Contains(t, fake.lastCmd, "--append-system-prompt")
	require.Contains(t, fake.lastCmd, "you are a planner")
	require.Contains(t, fake.lastCmd, "--model")
	require.Contains(t, fake.lastCmd, "claude-opus-4-7")
	require.Contains(t, fake.lastCmd, "--allowed-tools")
	require.Contains(t, fake.lastCmd, "Read,Glob")

	go func() {
		_, _ = io.WriteString(fake.stdoutWrite, `{"type":"system","subtype":"init","session_id":"sess_xyz","model":"claude-opus-4-7"}`+"\n")
		_ = fake.stdoutWrite.Close()
	}()

	var got []StreamEvent
	for ev := range proc.Output() {
		got = append(got, ev)
	}

	require.GreaterOrEqual(t, len(got), 1)
	require.Equal(t, "sess_xyz", proc.SessionID())
}

func TestWrapperPropagatesResume(t *testing.T) {
	fake := newFakeDockerExec()
	w := NewWrapperWithExecAPI(fake, nil)

	proc, err := w.Spawn(context.Background(), SpawnOptions{
		Container: "wc-test",
		Resume:    "sess_existing",
	})
	require.NoError(t, err)

	defer func() { _ = proc.Kill() }()

	require.Contains(t, fake.lastCmd, "--resume")
	require.Contains(t, fake.lastCmd, "sess_existing")
}

func TestWrapperKillCancelsContext(t *testing.T) {
	fake := newFakeDockerExec()
	w := NewWrapperWithExecAPI(fake, nil)

	ctx, cancel := context.WithCancel(context.Background())
	proc, err := w.Spawn(ctx, SpawnOptions{Container: "wc-test"})
	require.NoError(t, err)

	cancel()

	select {
	case <-proc.Output():
		// ok — channel closed by cancellation
	case <-time.After(time.Second):
		t.Fatal("Output channel didn't close on context cancel")
	}
}

func TestWrapperEventCallback(t *testing.T) {
	fake := newFakeDockerExec()

	var (
		captured []StreamEvent
		muCb     sync.Mutex
	)

	w := NewWrapperWithExecAPI(fake, nil)
	w.SetEventCallback(func(ev StreamEvent) {
		muCb.Lock()

		captured = append(captured, ev)
		muCb.Unlock()
	})

	proc, err := w.Spawn(context.Background(), SpawnOptions{Container: "wc-test"})
	require.NoError(t, err)

	go func() {
		_, _ = io.WriteString(fake.stdoutWrite, `{"type":"text","text":"hello"}`+"\n")
		_ = fake.stdoutWrite.Close()
	}()

	drained := 0
	for range proc.Output() {
		drained++
	}

	require.GreaterOrEqual(t, drained, 1)

	muCb.Lock()
	defer muCb.Unlock()

	require.GreaterOrEqual(t, len(captured), 1)
	require.Equal(t, EventText, captured[0].Kind)
}

func TestWrapperSendMessage(t *testing.T) {
	// Send a user-frame message to a captured stdin pipe.
	fake := newFakeDockerExec()

	var (
		mu        sync.Mutex
		sentBytes []byte
	)

	fakeStdinWriter := &capturingWriter{onWrite: func(p []byte) {
		mu.Lock()

		sentBytes = append(sentBytes, p...)
		mu.Unlock()
	}}
	// We need the ExecAttach to return our capturing writer.
	fake2 := &fakeDockerExecWithStdinCapture{
		fakeDockerExec: fake,
		stdin:          fakeStdinWriter,
	}
	w := NewWrapperWithExecAPI(fake2, nil)

	proc, err := w.Spawn(context.Background(), SpawnOptions{Container: "wc-test"})
	require.NoError(t, err)
	require.NoError(t, proc.SendMessage(context.Background(), NewUserMessage("hello")))

	mu.Lock()
	defer mu.Unlock()

	require.Contains(t, string(sentBytes), `"text":"hello"`)
	require.Contains(t, string(sentBytes), `"type":"user"`)
	require.Contains(t, string(sentBytes), `"role":"user"`)
	require.Contains(t, string(sentBytes), "\n", "trailing newline required for stream-json")
}

type capturingWriter struct {
	onWrite func(p []byte)
}

func (c *capturingWriter) Write(p []byte) (int, error) {
	c.onWrite(p)

	return len(p), nil
}

func (c *capturingWriter) Close() error { return nil }

type fakeDockerExecWithStdinCapture struct {
	*fakeDockerExec
	stdin io.WriteCloser
}

func (f *fakeDockerExecWithStdinCapture) ExecAttach(_ context.Context, _ string) (io.ReadCloser, io.WriteCloser, error) {
	return io.NopCloser(f.stdoutRead), f.stdin, nil
}
