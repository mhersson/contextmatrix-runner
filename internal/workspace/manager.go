// Package workspace coordinates multi-repo clones and per-(subtask × repo)
// git worktrees inside a single worker container. All shell calls go
// through the Exec interface so unit tests can inject fakes; production
// uses a Docker SDK exec backend.
package workspace

import (
	"context"
	"fmt"
	"sync"
)

// Exec abstracts running git/shell commands inside the worker
// container. In production this wraps a Docker SDK exec call.
type Exec interface {
	Exec(ctx context.Context, cmd []string) (stdout, stderr string, err error)
}

// RepoSpec mirrors board.RepoSpec; copied here to avoid CM<->runner
// import cycle. Runner config translation is the caller's concern.
type RepoSpec struct {
	Slug string
	URL  string
}

// Manager coordinates multi-repo workspace operations inside one
// worker container.
type Manager struct {
	exec     Exec
	registry map[string]RepoSpec // slug → spec
	mu       sync.Mutex
	cloned   map[string]bool
}

func NewManager(exec Exec, registry []RepoSpec) *Manager {
	m := &Manager{
		exec:     exec,
		registry: make(map[string]RepoSpec, len(registry)),
		cloned:   make(map[string]bool),
	}
	for _, r := range registry {
		m.registry[r.Slug] = r
	}

	return m
}

// CloneRepo clones a registered repo into /workspace/<slug>.
// Idempotent in two senses:
//
//  1. If CloneRepo has already run for this slug in this Manager, it is a
//     no-op.
//  2. If /workspace/<slug>/.git already exists on disk (e.g., the plan
//     agent ran `git clone` via Bash before the orchestrator's first
//     CloneRepo call for that slug), the existing clone is adopted: the
//     URL is verified against the registry and the slug is marked cloned
//     without re-cloning.
//
// Returns an error if the slug is not in the project's registry, or if an
// adopted clone has a different remote URL than the registry expects.
func (m *Manager) CloneRepo(ctx context.Context, slug string) error {
	m.mu.Lock()
	if m.cloned[slug] {
		m.mu.Unlock()

		return nil
	}

	spec, ok := m.registry[slug]
	if !ok {
		m.mu.Unlock()

		return fmt.Errorf("repo %q not in project registry", slug)
	}
	m.mu.Unlock()

	target := "/workspace/" + slug

	// Adopt an existing clone if one is already on disk. We probe by
	// running `git rev-parse --is-inside-work-tree` from the target dir;
	// the command exits 0 only inside a real git work tree, so this also
	// rejects a stray empty directory at the target path.
	probeStdout, _, probeErr := m.exec.Exec(ctx, []string{"git", "-C", target, "rev-parse", "--is-inside-work-tree"})
	if probeErr == nil && len(probeStdout) > 0 {
		// Verify the remote matches the registry so we don't adopt a
		// stale clone of the wrong fork. A mismatch is a hard error —
		// the orchestrator should not silently work against a different
		// upstream than the planner expects.
		remoteStdout, _, remoteErr := m.exec.Exec(ctx, []string{"git", "-C", target, "remote", "get-url", "origin"})
		if remoteErr == nil {
			actual := trimSpace(remoteStdout)
			if actual != "" && actual != spec.URL {
				return fmt.Errorf("repo %q: existing clone at %s has remote %q, expected %q", slug, target, actual, spec.URL)
			}
		}

		m.mu.Lock()
		m.cloned[slug] = true
		m.mu.Unlock()

		return nil
	}

	cmd := []string{"git", "clone", spec.URL, target}

	_, stderr, err := m.exec.Exec(ctx, cmd)
	if err != nil {
		return fmt.Errorf("git clone %s: %w (stderr: %s)", slug, err, stderr)
	}

	m.mu.Lock()
	m.cloned[slug] = true
	m.mu.Unlock()

	return nil
}

// trimSpace returns s with leading and trailing ASCII whitespace removed.
// Avoids importing strings just for this helper.
func trimSpace(s string) string {
	i, j := 0, len(s)

	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}

	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || s[j-1] == '\r') {
		j--
	}

	return s[i:j]
}

// ClonedRepos returns the slugs that have been cloned.
func (m *Manager) ClonedRepos() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]string, 0, len(m.cloned))
	for s := range m.cloned {
		out = append(out, s)
	}

	return out
}

// RegisteredSlugs returns the slugs in the project's repo registry.
// Used by the orchestrator's plan-phase fallback: when both the agent's
// chosen_repos and a subtask's repos come back empty but exactly one
// repo is registered, the orchestrator can default to it without
// involving the agent.
func (m *Manager) RegisteredSlugs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]string, 0, len(m.registry))
	for s := range m.registry {
		out = append(out, s)
	}

	return out
}
