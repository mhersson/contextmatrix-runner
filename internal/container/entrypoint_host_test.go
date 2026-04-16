package container

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostExtractionSnippet is the shell snippet that extracts the git host from
// CM_REPO_URL. This must match the logic in docker/entrypoint.sh exactly.
const hostExtractionSnippet = `
GIT_HOST=$(printf '%s' "${CM_REPO_URL:-}" | sed -n 's#^https://\([^/]*\)/.*#\1#p')
[ -z "$GIT_HOST" ] && GIT_HOST="github.com"
printf '%s' "$GIT_HOST"
`

// entrypointPath returns the absolute path to docker/entrypoint.sh relative
// to this test file.
func entrypointPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	// internal/container/entrypoint_host_test.go → up two dirs → repo root
	root := filepath.Join(filepath.Dir(filename), "..", "..")
	return filepath.Join(root, "docker", "entrypoint.sh")
}

// extractHost runs the host-extraction snippet with the given CM_REPO_URL and
// returns the resulting GIT_HOST value.
func extractHost(t *testing.T, cmRepoURL string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", hostExtractionSnippet)
	cmd.Env = append(os.Environ(), "CM_REPO_URL="+cmRepoURL)
	out, err := cmd.Output()
	require.NoError(t, err, "shell snippet failed for CM_REPO_URL=%q", cmRepoURL)
	return string(out)
}

func TestEntrypointGitHostExtraction(t *testing.T) {
	cases := []struct {
		name       string
		cmRepoURL  string
		wantHost   string
	}{
		{
			name:      "github.com HTTPS with .git suffix",
			cmRepoURL: "https://github.com/org/repo.git",
			wantHost:  "github.com",
		},
		{
			name:      "acme GHE host",
			cmRepoURL: "https://acme.ghe.com/org/repo.git",
			wantHost:  "acme.ghe.com",
		},
		{
			name:      "deeply nested hostname",
			cmRepoURL: "https://foo.bar.example.com/x/y",
			wantHost:  "foo.bar.example.com",
		},
		{
			name:      "empty CM_REPO_URL defaults to github.com",
			cmRepoURL: "",
			wantHost:  "github.com",
		},
		{
			name:      "SCP-style SSH URL (normalizeRepoURL should prevent this, but default gracefully)",
			cmRepoURL: "git@github.com:org/repo.git",
			wantHost:  "github.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractHost(t, tc.cmRepoURL)
			assert.Equal(t, tc.wantHost, got,
				"CM_REPO_URL=%q: want GIT_HOST=%q, got %q", tc.cmRepoURL, tc.wantHost, got)
		})
	}
}

// TestEntrypointUsesDynamicGitHost verifies that docker/entrypoint.sh no
// longer contains a hardcoded "machine github.com" line and instead derives
// the host dynamically from CM_REPO_URL.
func TestEntrypointUsesDynamicGitHost(t *testing.T) {
	path := entrypointPath(t)
	content, err := os.ReadFile(path)
	require.NoError(t, err, "reading entrypoint.sh")

	src := string(content)

	// Must NOT contain a literal hardcoded machine line.
	assert.NotContains(t, src, "machine github.com",
		"entrypoint.sh must not hardcode 'machine github.com'; it should use $GIT_HOST")

	// Must contain the dynamic host-extraction sed snippet.
	assert.True(t, strings.Contains(src, "sed -n 's#^https://\\([^/]*\\)/.*#\\1#p'"),
		"entrypoint.sh must contain the host-extraction sed snippet")

	// Must export GH_HOST (required by gh CLI for non-github.com hosts).
	assert.Contains(t, src, "export GH_HOST=",
		"entrypoint.sh must export GH_HOST")

	// Must NOT contain hardcoded url.insteadOf for github.com specifically
	// (should use $GIT_HOST variable instead).
	assert.NotContains(t, src, `url."https://github.com/".insteadOf`,
		"entrypoint.sh must not hardcode github.com in url.insteadOf; use $GIT_HOST")
}
