package workspace

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeExec records git commands and returns canned results.
//
// `responses` lets a test stage per-call return values keyed by argv shape.
// When a call has no staged response the fake falls back to (`stdout`,
// `stderr`, `err`) (the defaults set on the struct).
type fakeExec struct {
	mu        sync.Mutex
	cmds      [][]string
	stdout    string
	stderr    string
	err       error
	responses []fakeExecResponse
}

type fakeExecResponse struct {
	match  func(cmd []string) bool
	stdout string
	stderr string
	err    error
}

func (f *fakeExec) Exec(_ context.Context, cmd []string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.cmds = append(f.cmds, cmd)

	for _, r := range f.responses {
		if r.match(cmd) {
			return r.stdout, r.stderr, r.err
		}
	}

	return f.stdout, f.stderr, f.err
}

func (f *fakeExec) Cmds() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([][]string, len(f.cmds))
	copy(out, f.cmds)

	return out
}

func TestCloneRepoValidatesRegistry(t *testing.T) {
	mgr := NewManager(&fakeExec{}, []RepoSpec{
		{Slug: "auth-svc", URL: "https://github.com/acme/auth-svc.git"},
	})
	err := mgr.CloneRepo(context.Background(), "unauthorized-slug")
	require.Error(t, err, "rejects slug not in registry")
	require.Contains(t, err.Error(), "not in project registry")
}

func TestCloneRepoRunsGitClone(t *testing.T) {
	fake := &fakeExec{}
	mgr := NewManager(fake, []RepoSpec{
		{Slug: "auth-svc", URL: "https://github.com/acme/auth-svc.git"},
	})
	err := mgr.CloneRepo(context.Background(), "auth-svc")
	require.NoError(t, err)

	// CloneRepo first probes for an existing clone, then runs git clone
	// when the probe confirms the target is empty.
	cmds := fake.Cmds()
	require.Len(t, cmds, 2)
	require.Contains(t, cmds[0], "rev-parse", "first call probes the target dir")
	require.Equal(t, "git", cmds[1][0])
	require.Contains(t, cmds[1], "clone")
	require.Contains(t, cmds[1], "https://github.com/acme/auth-svc.git")
	require.Contains(t, cmds[1], "/workspace/auth-svc")
}

func TestCloneRepoIdempotent(t *testing.T) {
	fake := &fakeExec{}
	mgr := NewManager(fake, []RepoSpec{{Slug: "a", URL: "x"}})
	require.NoError(t, mgr.CloneRepo(context.Background(), "a"))
	require.NoError(t, mgr.CloneRepo(context.Background(), "a"))
	require.Len(t, fake.Cmds(), 2, "first call probes + clones; second call is a no-op")
}

func TestCloneRepoAdoptsExistingClone(t *testing.T) {
	// The plan agent may have run `git clone` via Bash before the
	// orchestrator's first CloneRepo call. The probe then succeeds, and
	// CloneRepo skips the clone but verifies the remote URL.
	fake := &fakeExec{
		responses: []fakeExecResponse{
			{
				match:  func(cmd []string) bool { return contains(cmd, "rev-parse") },
				stdout: "true\n",
			},
			{
				match:  func(cmd []string) bool { return contains(cmd, "remote") && contains(cmd, "get-url") },
				stdout: "https://github.com/acme/auth-svc.git\n",
			},
		},
	}
	mgr := NewManager(fake, []RepoSpec{
		{Slug: "auth-svc", URL: "https://github.com/acme/auth-svc.git"},
	})

	require.NoError(t, mgr.CloneRepo(context.Background(), "auth-svc"))

	cmds := fake.Cmds()
	require.Len(t, cmds, 2, "probe + remote check; no git clone")

	for _, cmd := range cmds {
		require.NotContains(t, cmd, "clone", "must not re-clone an adopted repo")
	}

	require.ElementsMatch(t, []string{"auth-svc"}, mgr.ClonedRepos())
}

func TestCloneRepoRejectsAdoptedCloneWithWrongRemote(t *testing.T) {
	// An adopted clone with a remote URL that doesn't match the registry
	// indicates a stale clone of the wrong fork. Hard error.
	fake := &fakeExec{
		responses: []fakeExecResponse{
			{
				match:  func(cmd []string) bool { return contains(cmd, "rev-parse") },
				stdout: "true\n",
			},
			{
				match:  func(cmd []string) bool { return contains(cmd, "remote") && contains(cmd, "get-url") },
				stdout: "https://github.com/attacker/fork.git\n",
			},
		},
	}
	mgr := NewManager(fake, []RepoSpec{
		{Slug: "auth-svc", URL: "https://github.com/acme/auth-svc.git"},
	})

	err := mgr.CloneRepo(context.Background(), "auth-svc")
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected")
	require.Empty(t, mgr.ClonedRepos(), "wrong-remote clone must not be marked cloned")
}

// contains reports whether the slice has the given element. Avoids
// importing slices for one helper.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}

func TestCloneRepoSurfacesExecError(t *testing.T) {
	fake := &fakeExec{err: errors.New("network down")}
	mgr := NewManager(fake, []RepoSpec{{Slug: "a", URL: "x"}})
	err := mgr.CloneRepo(context.Background(), "a")
	require.Error(t, err)
	require.Contains(t, err.Error(), "git clone a")
	require.Contains(t, err.Error(), "network down")
	// Failed clone should not be marked as cloned.
	require.Empty(t, mgr.ClonedRepos())
}

func TestClonedRepos(t *testing.T) {
	fake := &fakeExec{}
	mgr := NewManager(fake, []RepoSpec{
		{Slug: "a", URL: "https://example.com/a.git"},
		{Slug: "b", URL: "https://example.com/b.git"},
	})
	require.NoError(t, mgr.CloneRepo(context.Background(), "a"))
	require.NoError(t, mgr.CloneRepo(context.Background(), "b"))
	require.ElementsMatch(t, []string{"a", "b"}, mgr.ClonedRepos())
}

func TestCreateWorktreeRunsGitWorktreeAdd(t *testing.T) {
	fake := &fakeExec{}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth-svc", URL: "x"}})
	require.NoError(t, mgr.CloneRepo(context.Background(), "auth-svc"))
	fake.mu.Lock()
	fake.cmds = nil // discard clone
	fake.mu.Unlock()

	wt, err := mgr.CreateWorktree(context.Background(), "auth-svc", "SUB-1")
	require.NoError(t, err)
	require.Equal(t, "auth-svc", wt.Repo)
	require.Equal(t, "/workspace/auth-svc/.wt-SUB-1", wt.Path)
	require.Equal(t, "cm/SUB-1", wt.Branch)

	// CreateWorktree first probes for HEAD (to detect empty repos), then
	// runs git worktree add.
	cmds := fake.Cmds()
	require.Len(t, cmds, 2)
	require.Contains(t, cmds[0], "rev-parse", "first call probes HEAD")
	require.Contains(t, cmds[1], "worktree")
	require.Contains(t, cmds[1], "add")
	require.Contains(t, cmds[1], "/workspace/auth-svc/.wt-SUB-1")
	require.Contains(t, cmds[1], "cm/SUB-1")
	require.Contains(t, cmds[1], "-C")
	require.Contains(t, cmds[1], "/workspace/auth-svc")
}

func TestCreateWorktreeSeedsEmptyRepo(t *testing.T) {
	// Probe for HEAD fails on empty repo → CreateWorktree lands an
	// initial empty commit before running git worktree add.
	fake := &fakeExec{
		responses: []fakeExecResponse{
			{
				match: func(cmd []string) bool { return contains(cmd, "rev-parse") && contains(cmd, "--verify") },
				err:   errors.New("fatal: Needed a single revision"),
			},
		},
	}
	mgr := NewManager(fake, []RepoSpec{{Slug: "r", URL: "x"}})
	require.NoError(t, mgr.CloneRepo(context.Background(), "r"))
	fake.mu.Lock()
	fake.cmds = nil
	fake.mu.Unlock()

	wt, err := mgr.CreateWorktree(context.Background(), "r", "SUB-1")
	require.NoError(t, err)
	require.Equal(t, "/workspace/r/.wt-SUB-1", wt.Path)

	cmds := fake.Cmds()
	require.Len(t, cmds, 3, "probe + seed commit + worktree add")
	require.Contains(t, cmds[0], "rev-parse")
	require.Contains(t, cmds[1], "commit")
	require.Contains(t, cmds[1], "--allow-empty")
	require.Contains(t, cmds[2], "worktree")
	require.Contains(t, cmds[2], "add")
}

func TestCreateWorktreeRequiresClone(t *testing.T) {
	fake := &fakeExec{}
	mgr := NewManager(fake, []RepoSpec{{Slug: "r", URL: "x"}})
	_, err := mgr.CreateWorktree(context.Background(), "r", "SUB-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not cloned")
}

func TestRemoveWorktree(t *testing.T) {
	fake := &fakeExec{}
	mgr := NewManager(fake, []RepoSpec{{Slug: "r", URL: "x"}})
	require.NoError(t, mgr.CloneRepo(context.Background(), "r"))
	_, err := mgr.CreateWorktree(context.Background(), "r", "S1")
	require.NoError(t, err)
	fake.mu.Lock()
	fake.cmds = nil
	fake.mu.Unlock()

	require.NoError(t, mgr.RemoveWorktree(context.Background(), "r", "S1"))

	cmds := fake.Cmds()
	require.NotEmpty(t, cmds)
	require.Contains(t, cmds[0], "worktree")
	require.Contains(t, cmds[0], "remove")
	require.Contains(t, cmds[0], "--force")
	require.Contains(t, cmds[0], "/workspace/r/.wt-S1")
}

func TestCreateWorktreeSurfacesExecError(t *testing.T) {
	// HEAD probe succeeds (default response), but the worktree add itself
	// errors out — the error message should identify the failing op.
	fake := &fakeExec{
		responses: []fakeExecResponse{
			{
				match: func(cmd []string) bool { return contains(cmd, "rev-parse") && contains(cmd, "--verify") },
			},
			{
				match: func(cmd []string) bool { return contains(cmd, "worktree") && contains(cmd, "add") },
				err:   errors.New("worktree add denied"),
			},
		},
	}
	mgr := NewManager(fake, []RepoSpec{{Slug: "r", URL: "x"}})
	require.NoError(t, mgr.CloneRepo(context.Background(), "r"))

	_, err := mgr.CreateWorktree(context.Background(), "r", "S1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "worktree add r/S1")
	require.Contains(t, err.Error(), "denied")
}
