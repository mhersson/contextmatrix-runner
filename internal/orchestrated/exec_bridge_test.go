package orchestrated

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/spawn"
)

// fakeWorker captures the ExecOptions for each Exec call so tests can
// inspect what env was injected.
type fakeWorker struct {
	calls    []spawn.ExecOptions
	exitCode int
	stdout   string
	stderr   string
	err      error
}

func (f *fakeWorker) ID() string                     { return "fake" }
func (f *fakeWorker) Status() spawn.WorkerStatus     { return spawn.WorkerRunning }
func (f *fakeWorker) Stop(_ context.Context) error   { return nil }
func (f *fakeWorker) Remove(_ context.Context) error { return nil }

func (f *fakeWorker) Exec(_ context.Context, opts spawn.ExecOptions) (spawn.ExecResult, error) {
	f.calls = append(f.calls, opts)

	if f.err != nil {
		return spawn.ExecResult{}, f.err
	}

	return spawn.ExecResult{
		ExitCode: f.exitCode,
		Stdout:   f.stdout,
		Stderr:   f.stderr,
	}, nil
}

func (f *fakeWorker) ExecStream(_ context.Context, _ spawn.ExecOptions) (spawn.ExecStream, error) {
	return nil, errors.New("not implemented in fakeWorker")
}

// TestWorkspaceExecInjectsTokenEnv verifies that workspaceExec mints a
// token via tokenSource on every Exec call and passes it as
// CM_GIT_TOKEN / GH_TOKEN in ExecOptions.Env. Without this the worker's
// git credential helper has no token to read and gh CLI has no
// GH_TOKEN, which is the failure mode that broke `gh pr create` in
// production.
func TestWorkspaceExecInjectsTokenEnv(t *testing.T) {
	t.Parallel()

	w := &fakeWorker{}
	mints := 0
	exec := newWorkspaceExec(w, func(_ context.Context) (string, error) {
		mints++

		return "ghs_freshtoken", nil
	})

	_, _, err := exec.Exec(context.Background(), []string{"git", "push"})
	require.NoError(t, err)

	require.Len(t, w.calls, 1)
	assert.Equal(t, "ghs_freshtoken", w.calls[0].Env["CM_GIT_TOKEN"])
	assert.Equal(t, "ghs_freshtoken", w.calls[0].Env["GH_TOKEN"])

	// A second Exec mints again — per-exec freshness is the contract.
	_, _, err = exec.Exec(context.Background(), []string{"gh", "pr", "create"})
	require.NoError(t, err)
	assert.Equal(t, 2, mints, "tokenSource must be called once per Exec")
}

// TestWorkspaceExecPropagatesTokenError surfaces a tokenSource failure
// before invoking the worker, so a misconfigured GitHub auth provider
// fails the git/gh call cleanly instead of silently running unauth'd.
func TestWorkspaceExecPropagatesTokenError(t *testing.T) {
	t.Parallel()

	w := &fakeWorker{}
	exec := newWorkspaceExec(w, func(_ context.Context) (string, error) {
		return "", errors.New("github api 401")
	})

	_, _, err := exec.Exec(context.Background(), []string{"git", "push"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mint github token")
	assert.Empty(t, w.calls, "worker.Exec must not run when token mint fails")
}

// TestWorkspaceExecOmitsEnvWhenNoToken keeps the unit-test path working
// with a nil tokenSource and confirms an empty token (e.g. unconfigured
// generator) does not produce empty CM_GIT_TOKEN / GH_TOKEN env keys
// that could shadow other credential paths.
func TestWorkspaceExecOmitsEnvWhenNoToken(t *testing.T) {
	t.Parallel()

	t.Run("nil tokenSource", func(t *testing.T) {
		t.Parallel()

		w := &fakeWorker{}
		exec := newWorkspaceExec(w, nil)

		_, _, err := exec.Exec(context.Background(), []string{"git", "status"})
		require.NoError(t, err)
		require.Len(t, w.calls, 1)
		assert.Empty(t, w.calls[0].Env)
	})

	t.Run("empty token", func(t *testing.T) {
		t.Parallel()

		w := &fakeWorker{}
		exec := newWorkspaceExec(w, func(_ context.Context) (string, error) {
			return "", nil
		})

		_, _, err := exec.Exec(context.Background(), []string{"git", "status"})
		require.NoError(t, err)
		require.Len(t, w.calls, 1)
		assert.Empty(t, w.calls[0].Env)
	})
}

// Compile-time check: fakeWorker satisfies spawn.Worker.
var _ spawn.Worker = (*fakeWorker)(nil)
