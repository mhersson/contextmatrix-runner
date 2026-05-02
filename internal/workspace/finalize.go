package workspace

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrIntegrateConflict is returned by RebaseSubtask when the rebase
// halts on conflict markers. The rebase is left in progress so a
// resolver agent can edit conflicted files and run
// `git rebase --continue` from inside the worktree. Callers that
// don't dispatch a resolver MUST call AbortRebase to clean up.
var ErrIntegrateConflict = errors.New("integrate: rebase conflict")

// PushBranch runs `git push origin <branch>` from the repo's main clone.
// The branch must exist locally — typically a worktree branch created by
// CreateWorktree, into which the execute phase has committed.
func (m *Manager) PushBranch(ctx context.Context, repoSlug, branch string) error {
	repoPath := "/workspace/" + repoSlug
	cmd := []string{"git", "-C", repoPath, "push", "--set-upstream", "origin", branch}

	_, stderr, err := m.exec.Exec(ctx, cmd)
	if err != nil {
		return fmt.Errorf("push %s/%s: %w (stderr: %s)", repoSlug, branch, err, stderr)
	}

	return nil
}

// EnsureFeatureBranch makes the named feature branch the checked-out
// branch in the repo's main clone. Idempotent across calls so the
// orchestrator can call it once per repo at the top of the execute
// phase without worrying about losing integrations on FSM re-entry
// (e.g. after CreatingSubtasks for decomposition).
//
// If the branch already exists locally it is plain-switched to,
// preserving any commits already integrated onto it. If it does not
// exist it is created from origin/<base> (or local <base> as a
// fallback for freshly-seeded empty repos).
func (m *Manager) EnsureFeatureBranch(ctx context.Context, repoSlug, branch, base string) error {
	if base == "" {
		base = "main"
	}

	repoPath := "/workspace/" + repoSlug

	// Branch already exists locally — switch to it without resetting
	// so prior subtask integrations are preserved.
	if _, _, err := m.exec.Exec(ctx, []string{"git", "-C", repoPath, "rev-parse", "--verify", branch}); err == nil {
		if _, stderr, err := m.exec.Exec(ctx, []string{"git", "-C", repoPath, "switch", branch}); err != nil {
			return fmt.Errorf("switch to existing %s on %s: %w (stderr: %s)", branch, repoSlug, err, stderr)
		}

		return nil
	}

	// Best-effort fetch of the base ref so we can branch from origin/<base>.
	_, _, _ = m.exec.Exec(ctx, []string{"git", "-C", repoPath, "fetch", "origin", base})

	startPoint := "origin/" + base

	cmd := []string{"git", "-C", repoPath, "switch", "-C", branch, startPoint}
	if _, stderr, err := m.exec.Exec(ctx, cmd); err != nil {
		// Fall back to a local base branch (rare; freshly-seeded empty repo).
		cmd = []string{"git", "-C", repoPath, "switch", "-C", branch, base}
		if _, stderr2, err2 := m.exec.Exec(ctx, cmd); err2 != nil {
			return fmt.Errorf("create feature branch %s on %s from %s: %w (stderr: %s / %s)", branch, repoSlug, base, err, stderr, stderr2)
		}
	}

	return nil
}

// RebaseSubtask rebases cm/<subtaskID> onto featureBranch in the
// subtask's worktree (/workspace/<repoSlug>/.wt-<subtaskID>). On a
// clean rebase returns nil. On a content conflict returns an error
// that wraps ErrIntegrateConflict and LEAVES THE REBASE IN PROGRESS
// so a resolver agent can edit conflicted files and run
// `git rebase --continue` inside the worktree. Callers that don't
// dispatch a resolver MUST call AbortRebase before reusing the
// worktree.
//
// Structural failures (missing worktree, unknown branch, exec
// errors) return a wrapped error that does NOT match
// ErrIntegrateConflict — those bypass the resolver phase since
// there are no conflict markers to resolve.
func (m *Manager) RebaseSubtask(ctx context.Context, repoSlug, subtaskID, featureBranch string) error {
	wtPath := "/workspace/" + repoSlug + "/.wt-" + subtaskID

	cmd := []string{"git", "-C", wtPath, "rebase", featureBranch}

	stdout, stderr, err := m.exec.Exec(ctx, cmd)
	if err == nil {
		return nil
	}

	// Distinguish content conflicts (resolver-recoverable) from
	// structural failures. Git emits "CONFLICT (...)" or "Merge
	// conflict in" markers in its output when files conflict.
	combined := stdout + "\n" + stderr + "\n" + err.Error()
	if strings.Contains(combined, "CONFLICT") || strings.Contains(combined, "Merge conflict") {
		return fmt.Errorf("rebase %s onto %s in %s: %w (err: %s, stderr: %s)", subtaskID, featureBranch, repoSlug, ErrIntegrateConflict, err, stderr)
	}

	return fmt.Errorf("rebase %s onto %s in %s: %v (stderr: %s)", subtaskID, featureBranch, repoSlug, err, stderr)
}

// AbortRebase cancels an in-progress rebase in the subtask's
// worktree. Idempotent — `git rebase --abort` exits non-zero when
// there's nothing to abort, but that's not an error from the
// caller's point of view, so we swallow it.
func (m *Manager) AbortRebase(ctx context.Context, repoSlug, subtaskID string) error {
	wtPath := "/workspace/" + repoSlug + "/.wt-" + subtaskID

	_, _, _ = m.exec.Exec(ctx, []string{"git", "-C", wtPath, "rebase", "--abort"})

	return nil
}

// FastForwardFeature advances featureBranch in the repo's main clone
// to the tip of cm/<subtaskID>. Use after RebaseSubtask returns nil
// (or after a resolver agent has completed the rebase). Returns an
// error if the merge isn't fast-forwardable — that would indicate a
// caller-side logic bug since the rebase guarantees the subtask
// branch contains featureBranch's tip.
func (m *Manager) FastForwardFeature(ctx context.Context, repoSlug, subtaskID, featureBranch string) error {
	repoPath := "/workspace/" + repoSlug
	subtaskBranch := "cm/" + subtaskID

	cmd := []string{"git", "-C", repoPath, "merge", "--ff-only", subtaskBranch}

	_, stderr, err := m.exec.Exec(ctx, cmd)
	if err != nil {
		return fmt.Errorf("fast-forward %s to %s on %s: %w (stderr: %s)", featureBranch, subtaskBranch, repoSlug, err, stderr)
	}

	return nil
}

// OpenPR creates a GitHub pull request via the gh CLI for an
// already-pushed branch. The head branch must exist on the remote.
// Returns the PR URL on success (the trimmed first line of gh's stdout
// is the URL by gh CLI convention).
//
// Uses the registered repo URL to derive the --repo OWNER/NAME flag, so
// the call works without setting a working directory and without the
// gh CLI needing to be invoked from inside a git work tree.
func (m *Manager) OpenPR(ctx context.Context, repoSlug, head, base, title, body string) (string, error) {
	m.mu.Lock()
	spec, ok := m.registry[repoSlug]
	m.mu.Unlock()

	if !ok {
		return "", fmt.Errorf("open pr %q: repo not in project registry", repoSlug)
	}

	ownerName, err := ownerNameFromURL(spec.URL)
	if err != nil {
		return "", fmt.Errorf("open pr %q: %w", repoSlug, err)
	}

	if base == "" {
		base = "main"
	}

	cmd := []string{
		"gh", "pr", "create",
		"--repo", ownerName,
		"--head", head,
		"--base", base,
		"--title", title,
		"--body", body,
	}

	stdout, stderr, err := m.exec.Exec(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("gh pr create %s %s→%s: %w (stderr: %s)", ownerName, head, base, err, stderr)
	}

	// gh prints the PR URL to stdout. Take the last non-empty line in
	// case any informational lines precede it.
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line, nil
		}
	}

	return "", nil
}

// ownerNameFromURL extracts the GitHub-style "OWNER/NAME" from a clone URL.
// Accepts both https (https://github.com/owner/name[.git]) and ssh
// (git@github.com:owner/name[.git]) shapes. Returns an error if the URL
// shape doesn't match a recognisable owner/name pair.
func ownerNameFromURL(raw string) (string, error) {
	raw = strings.TrimSuffix(raw, ".git")
	raw = strings.TrimSuffix(raw, "/")

	// SSH form: git@host:owner/name
	if !strings.Contains(raw, "://") && strings.Contains(raw, ":") {
		i := strings.Index(raw, ":")
		path := raw[i+1:]

		if owner, name, ok := splitOwnerName(path); ok {
			return owner + "/" + name, nil
		}
	}

	// HTTPS / HTTP form
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse repo url %q: %w", raw, err)
	}

	if owner, name, ok := splitOwnerName(strings.TrimPrefix(u.Path, "/")); ok {
		return owner + "/" + name, nil
	}

	return "", fmt.Errorf("repo url %q does not look like owner/name", raw)
}

func splitOwnerName(path string) (string, string, bool) {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", false
	}

	owner := parts[len(parts)-2]
	name := parts[len(parts)-1]

	if owner == "" || name == "" {
		return "", "", false
	}

	return owner, name, true
}
