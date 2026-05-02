package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOwnerNameFromURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
		want string
	}{
		{"https with .git", "https://github.com/acme/auth-svc.git", "acme/auth-svc"},
		{"https without .git", "https://github.com/acme/auth-svc", "acme/auth-svc"},
		{"https trailing slash", "https://github.com/acme/auth-svc/", "acme/auth-svc"},
		{"ssh", "git@github.com:acme/auth-svc.git", "acme/auth-svc"},
		{"ssh without .git", "git@github.com:acme/auth-svc", "acme/auth-svc"},
		{"deeper path", "https://gitea.example/owner/group/repo.git", "group/repo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ownerNameFromURL(tc.url)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestOwnerNameFromURL_RejectsMalformed(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "noslash", "https://github.com/", "https://example.com/onlyone"} {
		_, err := ownerNameFromURL(raw)
		require.Error(t, err, "expected error for %q", raw)
	}
}

func TestPushBranchRunsGitPush(t *testing.T) {
	fake := &fakeExec{}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "https://github.com/acme/auth.git"}})

	require.NoError(t, mgr.PushBranch(context.Background(), "auth", "cm/SUB-1"))

	cmds := fake.Cmds()
	require.Len(t, cmds, 1)
	require.Equal(t, "git", cmds[0][0])
	require.Contains(t, cmds[0], "-C")
	require.Contains(t, cmds[0], "/workspace/auth")
	require.Contains(t, cmds[0], "push")
	require.Contains(t, cmds[0], "origin")
	require.Contains(t, cmds[0], "cm/SUB-1")
}

func TestPushBranchSurfacesError(t *testing.T) {
	fake := &fakeExec{err: errors.New("permission denied")}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "https://github.com/acme/auth.git"}})

	err := mgr.PushBranch(context.Background(), "auth", "cm/SUB-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "push auth/cm/SUB-1")
}

func TestOpenPRReturnsURL(t *testing.T) {
	fake := &fakeExec{stdout: "https://github.com/acme/auth/pull/42\n"}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "https://github.com/acme/auth.git"}})

	url, err := mgr.OpenPR(context.Background(), "auth", "cm/SUB-1", "main", "title", "body")
	require.NoError(t, err)
	require.Equal(t, "https://github.com/acme/auth/pull/42", url)

	cmds := fake.Cmds()
	require.Len(t, cmds, 1)
	require.Equal(t, "gh", cmds[0][0])
	require.Contains(t, cmds[0], "--repo")
	require.Contains(t, cmds[0], "acme/auth")
	require.Contains(t, cmds[0], "--head")
	require.Contains(t, cmds[0], "cm/SUB-1")
	require.Contains(t, cmds[0], "--base")
	require.Contains(t, cmds[0], "main")
}

func TestOpenPRDefaultsBaseToMain(t *testing.T) {
	fake := &fakeExec{stdout: "https://github.com/acme/auth/pull/1\n"}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "https://github.com/acme/auth.git"}})

	_, err := mgr.OpenPR(context.Background(), "auth", "cm/SUB-1", "", "title", "body")
	require.NoError(t, err)

	cmds := fake.Cmds()
	require.Len(t, cmds, 1)
	require.Contains(t, cmds[0], "main")
}

func TestOpenPRRejectsUnregisteredSlug(t *testing.T) {
	mgr := NewManager(&fakeExec{}, []RepoSpec{{Slug: "auth", URL: "https://github.com/acme/auth.git"}})

	_, err := mgr.OpenPR(context.Background(), "billing", "cm/SUB-1", "main", "title", "body")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not in project registry")
}

func TestEnsureFeatureBranchCreatesFromOriginBase(t *testing.T) {
	// Branch does not exist yet → fetch origin <base>, then switch -C <branch> origin/<base>.
	fake := &fakeExec{
		responses: []fakeExecResponse{
			{
				match: func(cmd []string) bool {
					return contains(cmd, "rev-parse") && contains(cmd, "--verify") && contains(cmd, "feat/x")
				},
				err: errors.New("unknown ref"),
			},
		},
	}

	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "x"}})

	require.NoError(t, mgr.EnsureFeatureBranch(context.Background(), "auth", "feat/x", "main"))

	cmds := fake.Cmds()
	require.GreaterOrEqual(t, len(cmds), 3, "expected verify + fetch + switch")

	last := cmds[len(cmds)-1]
	require.Contains(t, last, "switch")
	require.Contains(t, last, "-C")
	require.Contains(t, last, "feat/x")
	require.Contains(t, last, "origin/main")
}

func TestEnsureFeatureBranchAdoptsExistingBranch(t *testing.T) {
	// rev-parse --verify succeeds → just switch (no -C, no reset).
	fake := &fakeExec{} // default response: success

	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "x"}})

	require.NoError(t, mgr.EnsureFeatureBranch(context.Background(), "auth", "feat/x", "main"))

	cmds := fake.Cmds()
	// Must NOT include "switch -C" — that would reset and discard prior integrations.
	// The git -C <path> working-dir flag appears before the subcommand, so check
	// for the destructive "-C" flag after "switch" specifically.
	for _, c := range cmds {
		for i, arg := range c {
			if arg == "switch" {
				for _, post := range c[i+1:] {
					require.NotEqual(t, "-C", post, "EnsureFeatureBranch must not reset an existing branch")
				}
			}
		}
	}

	// Must include a plain switch to feat/x.
	last := cmds[len(cmds)-1]
	require.Contains(t, last, "switch")
	require.Contains(t, last, "feat/x")
}

func TestEnsureFeatureBranchFallsBackToLocalBase(t *testing.T) {
	// Branch absent + origin/main unresolvable → retry against local "main".
	fake := &fakeExec{
		responses: []fakeExecResponse{
			{
				match: func(cmd []string) bool {
					return contains(cmd, "rev-parse") && contains(cmd, "--verify") && contains(cmd, "feat/x")
				},
				err: errors.New("unknown ref"),
			},
			{
				match: func(cmd []string) bool {
					return contains(cmd, "switch") && contains(cmd, "-C") && contains(cmd, "origin/main")
				},
				err: errors.New("origin/main: unknown ref"),
			},
		},
	}

	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "x"}})

	require.NoError(t, mgr.EnsureFeatureBranch(context.Background(), "auth", "feat/x", "main"))

	last := fake.Cmds()[len(fake.Cmds())-1]
	require.Contains(t, last, "switch")
	require.Contains(t, last, "-C")
	require.Contains(t, last, "feat/x")
	require.Contains(t, last, "main")
	require.NotContains(t, last, "origin/main")
}

func TestRebaseSubtaskRunsRebaseInWorktree(t *testing.T) {
	fake := &fakeExec{}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "x"}})

	require.NoError(t, mgr.RebaseSubtask(context.Background(), "auth", "SUB-1", "feat/x"))

	cmds := fake.Cmds()
	require.Len(t, cmds, 1)
	require.Equal(t, "git", cmds[0][0])
	require.Contains(t, cmds[0], "-C")
	require.Contains(t, cmds[0], "/workspace/auth/.wt-SUB-1")
	require.Contains(t, cmds[0], "rebase")
	require.Contains(t, cmds[0], "feat/x")
}

func TestRebaseSubtaskReturnsErrIntegrateConflictOnFailure(t *testing.T) {
	fake := &fakeExec{err: errors.New("CONFLICT (content): Merge conflict in README.md")}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "x"}})

	err := mgr.RebaseSubtask(context.Background(), "auth", "SUB-1", "feat/x")

	require.Error(t, err)
	require.ErrorIs(t, err, ErrIntegrateConflict,
		"RebaseSubtask must wrap ErrIntegrateConflict so callers can dispatch a resolver")
	require.Contains(t, err.Error(), "README.md")

	// Crucially: must NOT have called `rebase --abort`. The rebase is
	// left in progress for the resolver agent to operate on.
	cmds := fake.Cmds()
	for _, c := range cmds {
		require.NotContains(t, c, "--abort", "RebaseSubtask must leave the rebase in progress on conflict")
	}
}

func TestRebaseSubtaskReturnsPlainErrorOnStructuralFailure(t *testing.T) {
	// Structural failures (unknown branch, missing worktree, etc.)
	// must NOT wrap ErrIntegrateConflict — otherwise the orchestrator
	// would waste a resolver-agent spawn on a worktree with no real
	// conflict markers.
	fake := &fakeExec{err: errors.New("fatal: ambiguous argument 'feat/x': unknown revision")}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "x"}})

	err := mgr.RebaseSubtask(context.Background(), "auth", "SUB-1", "feat/x")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrIntegrateConflict,
		"structural failures must NOT wrap ErrIntegrateConflict — caller would waste a resolver spawn")
	require.Contains(t, err.Error(), "ambiguous argument")
}

func TestAbortRebaseRunsRebaseAbortInWorktree(t *testing.T) {
	fake := &fakeExec{}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "x"}})

	require.NoError(t, mgr.AbortRebase(context.Background(), "auth", "SUB-1"))

	cmds := fake.Cmds()
	require.Len(t, cmds, 1)
	require.Contains(t, cmds[0], "-C")
	require.Contains(t, cmds[0], "/workspace/auth/.wt-SUB-1")
	require.Contains(t, cmds[0], "rebase")
	require.Contains(t, cmds[0], "--abort")
}

func TestAbortRebaseIsIdempotent(t *testing.T) {
	// `git rebase --abort` exits non-zero if no rebase is in progress.
	// AbortRebase must swallow that so callers can defer it without
	// branching on whether a rebase actually got started.
	fake := &fakeExec{err: errors.New("fatal: No rebase in progress?")}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "x"}})

	require.NoError(t, mgr.AbortRebase(context.Background(), "auth", "SUB-1"))
}

func TestFastForwardFeatureRunsFFOnlyMerge(t *testing.T) {
	fake := &fakeExec{}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "x"}})

	require.NoError(t, mgr.FastForwardFeature(context.Background(), "auth", "SUB-1", "feat/x"))

	cmds := fake.Cmds()
	require.Len(t, cmds, 1)
	require.Contains(t, cmds[0], "-C")
	require.Contains(t, cmds[0], "/workspace/auth")
	// The cmd argv contains "/workspace/auth" but NOT the worktree subpath.
	for _, arg := range cmds[0] {
		require.NotEqual(t, "/workspace/auth/.wt-SUB-1", arg, "FastForwardFeature must run in the main clone, not the worktree")
	}

	require.Contains(t, cmds[0], "merge")
	require.Contains(t, cmds[0], "--ff-only")
	require.Contains(t, cmds[0], "cm/SUB-1")
}

func TestFastForwardFeatureSurfacesNonFFError(t *testing.T) {
	fake := &fakeExec{err: errors.New("not possible to fast-forward, aborting")}
	mgr := NewManager(fake, []RepoSpec{{Slug: "auth", URL: "x"}})

	err := mgr.FastForwardFeature(context.Background(), "auth", "SUB-1", "feat/x")

	require.Error(t, err)
	require.Contains(t, err.Error(), "fast-forward")
}
