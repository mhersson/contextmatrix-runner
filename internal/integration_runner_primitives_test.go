//go:build integration

package internal_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
	"github.com/mhersson/contextmatrix-runner/internal/sessions"
	"github.com/mhersson/contextmatrix-runner/internal/workspace"
)

// --- Fakes (local to this integration test) ---

type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardCloser) Close() error                { return nil }

type fakeExecAPI struct {
	mu          sync.Mutex
	lastCmd     []string
	stdoutWrite *io.PipeWriter
	stdoutRead  *io.PipeReader
}

func newFakeExecAPI() *fakeExecAPI {
	pr, pw := io.Pipe()

	return &fakeExecAPI{stdoutWrite: pw, stdoutRead: pr}
}

func (f *fakeExecAPI) ExecCreate(_ context.Context, _ string, cfg claudeclient.ExecConfig) (string, error) {
	f.mu.Lock()
	f.lastCmd = cfg.Cmd
	f.mu.Unlock()

	return "exec-1", nil
}

func (f *fakeExecAPI) ExecAttach(_ context.Context, _ string) (io.ReadCloser, io.WriteCloser, error) {
	return io.NopCloser(f.stdoutRead), discardCloser{}, nil
}

type fakeCardStore struct {
	mu    sync.Mutex
	saved map[string]map[string]string
}

func (f *fakeCardStore) GetSessionID(_ context.Context, cardID, purpose string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.saved == nil {
		return "", nil
	}

	return f.saved[cardID][purpose], nil
}

func (f *fakeCardStore) SaveSessionID(_ context.Context, cardID, purpose, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.saved == nil {
		f.saved = make(map[string]map[string]string)
	}

	if f.saved[cardID] == nil {
		f.saved[cardID] = make(map[string]string)
	}

	f.saved[cardID][purpose] = sessionID

	return nil
}

func (f *fakeCardStore) ClearSessionID(_ context.Context, cardID, purpose string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.saved != nil && f.saved[cardID] != nil {
		delete(f.saved[cardID], purpose)
	}

	return nil
}

type fakeShellExec struct {
	mu   sync.Mutex
	cmds [][]string
}

func (f *fakeShellExec) Exec(_ context.Context, cmd []string) (string, string, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	f.mu.Unlock()

	return "", "", nil
}

// wrapperAsSpawner adapts *claudeclient.Wrapper to sessions.Spawner.
type wrapperAsSpawner struct{ w *claudeclient.Wrapper }

func (s wrapperAsSpawner) Spawn(ctx context.Context, opts claudeclient.SpawnOptions) (claudeclient.Process, error) {
	return s.w.Spawn(ctx, opts)
}

// --- Integration test ---

// TestPrimitivesIntegrate wires claudeclient + sessions + workspace
// together with mocks to verify the composition's public APIs are
// shaped correctly. No real Docker, no real CC, no live network.
func TestPrimitivesIntegrate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 1. Wrapper with fake exec API.
	fakeExec := newFakeExecAPI()
	wrapper := claudeclient.NewWrapperWithExecAPI(fakeExec, nil)

	// 2. Sessions Manager with the wrapper as Spawner.
	cards := &fakeCardStore{}
	mgr := sessions.NewManager(wrapperAsSpawner{wrapper}, cards, nil)

	// 3. Workspace Manager with a separate fake exec.
	wsExec := &fakeShellExec{}
	ws := workspace.NewManager(wsExec, []workspace.RepoSpec{
		{Slug: "auth-svc", URL: "https://example.com/auth-svc.git"},
	})

	// Drive fake CC stdout with system_init + a text event, then close.
	go func() {
		_, _ = fakeExec.stdoutWrite.Write([]byte(`{"type":"system","subtype":"init","session_id":"sess_1","model":"claude-sonnet-4-6"}` + "\n"))
		_, _ = fakeExec.stdoutWrite.Write([]byte(`{"type":"text","text":"hi"}` + "\n"))
		_ = fakeExec.stdoutWrite.Close()
	}()

	// Acquire a brainstorm session — exercises tier-3 (no prior session_id).
	sess, err := mgr.Acquire(ctx, "card-1", "brainstorm", sessions.AcquireOptions{
		Container:    "wc",
		SystemPrompt: "be helpful",
		Model:        "claude-sonnet-4-6",
	})
	require.NoError(t, err)
	require.NotEmpty(t, sess.SessionID(), "session_id should be populated from system_init")

	// Verify the wrapper passed expected flags through to the fake exec.
	fakeExec.mu.Lock()
	cmd := fakeExec.lastCmd
	fakeExec.mu.Unlock()
	require.Contains(t, cmd, "claude")
	require.Contains(t, cmd, "stream-json")
	require.Contains(t, cmd, "be helpful")
	require.Contains(t, cmd, "claude-sonnet-4-6")

	// Verify session_id was persisted via the card store.
	saved, _ := cards.GetSessionID(ctx, "card-1", "brainstorm")
	require.NotEmpty(t, saved)

	// Workspace: clone + worktree.
	require.NoError(t, ws.CloneRepo(ctx, "auth-svc"))
	wt, err := ws.CreateWorktree(ctx, "auth-svc", "SUB-1")
	require.NoError(t, err)
	require.Equal(t, "cm/SUB-1", wt.Branch)
	require.Equal(t, "/workspace/auth-svc/.wt-SUB-1", wt.Path)

	// Verify the workspace exec recorded both git operations.
	wsExec.mu.Lock()
	require.GreaterOrEqual(t, len(wsExec.cmds), 2, "clone + worktree add")
	wsExec.mu.Unlock()

	// Cleanup.
	mgr.TerminateAll(ctx, "card-1")

	// After TerminateAll the persisted session_id should be cleared.
	saved2, _ := cards.GetSessionID(ctx, "card-1", "brainstorm")
	require.Empty(t, saved2, "TerminateAll should clear persisted session_id")
}
