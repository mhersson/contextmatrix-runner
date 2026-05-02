package workspace

import (
	"context"
	"fmt"
)

// Worktree describes one per-(subtask, repo) git worktree on a
// dedicated cm/<subtask-id> branch.
type Worktree struct {
	Repo   string // slug
	Path   string // /workspace/<slug>/.wt-<subtask>
	Branch string // cm/<subtask>
}

// CreateWorktree creates a per-(subtask, repo) git worktree on
// branch cm/<subtask-id>. The repo must have been cloned first via
// CloneRepo; otherwise an error is returned.
//
// If the repo has no commits yet (a freshly-initialised empty GitHub
// project), CreateWorktree first lands an empty initial commit so
// `git worktree add -b` has a base to branch from. The injected commit
// is harmless on a brand-new repo and keeps the bootstrap-a-new-project
// flow working without forcing the user to seed the repo by hand.
func (m *Manager) CreateWorktree(ctx context.Context, repoSlug, subtaskID string) (Worktree, error) {
	m.mu.Lock()
	if !m.cloned[repoSlug] {
		m.mu.Unlock()

		return Worktree{}, fmt.Errorf("repo %q not cloned; cannot create worktree", repoSlug)
	}
	m.mu.Unlock()

	repoPath := "/workspace/" + repoSlug
	wtPath := repoPath + "/.wt-" + subtaskID
	branch := "cm/" + subtaskID

	if _, _, err := m.exec.Exec(ctx, []string{"git", "-C", repoPath, "rev-parse", "--verify", "HEAD"}); err != nil {
		if _, stderr, initErr := m.exec.Exec(ctx, []string{"git", "-C", repoPath, "commit", "--allow-empty", "-m", "Initial commit"}); initErr != nil {
			return Worktree{}, fmt.Errorf("seed empty repo %s: %w (stderr: %s)", repoSlug, initErr, stderr)
		}
	}

	cmd := []string{"git", "-C", repoPath, "worktree", "add", wtPath, "-b", branch}

	_, stderr, err := m.exec.Exec(ctx, cmd)
	if err != nil {
		return Worktree{}, fmt.Errorf("worktree add %s/%s: %w (stderr: %s)", repoSlug, subtaskID, err, stderr)
	}

	return Worktree{Repo: repoSlug, Path: wtPath, Branch: branch}, nil
}

// RemoveWorktree tears down a worktree (e.g., on subtask cleanup).
// Uses --force to handle dirty state from killed subprocesses.
func (m *Manager) RemoveWorktree(ctx context.Context, repoSlug, subtaskID string) error {
	repoPath := "/workspace/" + repoSlug
	wtPath := repoPath + "/.wt-" + subtaskID
	cmd := []string{"git", "-C", repoPath, "worktree", "remove", "--force", wtPath}

	_, stderr, err := m.exec.Exec(ctx, cmd)
	if err != nil {
		return fmt.Errorf("worktree remove %s/%s: %w (stderr: %s)", repoSlug, subtaskID, err, stderr)
	}

	return nil
}
