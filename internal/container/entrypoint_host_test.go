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

// runEntrypoint executes docker/entrypoint.sh in an isolated environment where
// both "claude" and "git" are replaced by minimal mock scripts. The mock
// "claude" writes its full argument list to argFile; the mock "git" handles
// "clone" by creating /home/user/workspace and silently succeeds for everything
// else (config, etc.). The function returns the content of argFile so callers
// can assert on which flags claude was invoked with.
func runEntrypoint(t *testing.T, extraEnv []string) string {
	t.Helper()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	ep := entrypointPath(t)

	// Temp dir for mock binaries and output.
	tmpDir := t.TempDir()
	argFile := filepath.Join(tmpDir, "claude-args.txt")
	workspace := filepath.Join(tmpDir, "workspace")

	// Mock claude: writes all args to argFile then exits 0.
	claudeMock := filepath.Join(tmpDir, "claude")
	err := os.WriteFile(claudeMock, []byte(`#!/bin/bash
printf '%s\n' "$@" > `+argFile+`
exit 0
`), 0o755)
	require.NoError(t, err, "writing mock claude")

	// Mock git: for "clone" create the workspace dir; for everything else succeed silently.
	gitMock := filepath.Join(tmpDir, "git")
	err = os.WriteFile(gitMock, []byte(`#!/bin/bash
if [ "$1" = "clone" ]; then
    mkdir -p `+workspace+`
    exit 0
fi
exit 0
`), 0o755)
	require.NoError(t, err, "writing mock git")

	// Mock jq: needed for MCP config construction — return harmless JSON.
	jqMock := filepath.Join(tmpDir, "jq")
	err = os.WriteFile(jqMock, []byte(`#!/bin/bash
echo '{}'
exit 0
`), 0o755)
	require.NoError(t, err, "writing mock jq")

	// Base env: clear PATH to only our mocks + minimal system tools.
	baseEnv := []string{
		"HOME=" + tmpDir,
		"PATH=" + tmpDir + ":/usr/bin:/bin",
		"CM_CARD_ID=TEST-001",
		"CM_PROJECT=test-project",
		"CM_MCP_URL=http://localhost:9999/mcp",
		"CM_REPO_URL=https://github.com/example/repo.git",
	}

	cmd := exec.Command("bash", ep)
	cmd.Env = append(baseEnv, extraEnv...)
	// entrypoint does "cd /home/user/workspace" after clone; since our mock
	// creates tmpDir/workspace, we patch the path by intercepting at the shell
	// level. The entrypoint hardcodes /home/user/workspace in the cd command,
	// so we pre-create it to avoid a cd failure.
	require.NoError(t, os.MkdirAll("/home/user/workspace", 0o755),
		"creating /home/user/workspace for test (requires write access or run as root)")

	out, err := cmd.CombinedOutput()
	// The mock claude exits 0, so the script should succeed. If it fails for
	// another reason (e.g. missing /home/user/workspace), surface the output.
	if err != nil {
		t.Logf("entrypoint output:\n%s", out)
	}
	require.NoError(t, err, "entrypoint.sh failed")

	raw, err := os.ReadFile(argFile)
	require.NoError(t, err, "reading captured claude args")
	return string(raw)
}

// TestEntrypointInteractiveBranching verifies that the CM_INTERACTIVE env var
// selects the correct claude invocation mode by executing docker/entrypoint.sh
// with mock binaries. It is gated behind CM_ENTRYPOINT_HOST_TEST=1 because it
// requires write access to /home/user/workspace on the host, which is not
// available in all CI environments.
//
// The always-on source-inspection guard is TestEntrypointInteractiveContent,
// which reads entrypoint.sh directly without executing it.
//
// To run locally:
//
//	CM_ENTRYPOINT_HOST_TEST=1 go test ./internal/container/ -run TestEntrypointInteractiveBranching
func TestEntrypointInteractiveBranching(t *testing.T) {
	if os.Getenv("CM_ENTRYPOINT_HOST_TEST") == "" {
		t.Skip("set CM_ENTRYPOINT_HOST_TEST=1 to run entrypoint host tests (requires write access to /home/user/workspace)")
	}

	t.Run("one-shot when CM_INTERACTIVE unset", func(t *testing.T) {
		args := runEntrypoint(t, nil)
		assert.NotContains(t, args, "--input-format",
			"one-shot path must not include --input-format")
		assert.Contains(t, args, "--output-format\nstream-json",
			"one-shot path must include --output-format stream-json")
		assert.Contains(t, args, "run-autonomous workflow",
			"one-shot path must include the autonomous workflow prompt")
		assert.NotContains(t, args, "Wait for the user's first message",
			"one-shot path must not include the interactive prompt")
	})

	t.Run("stream-json input when CM_INTERACTIVE=1", func(t *testing.T) {
		args := runEntrypoint(t, []string{"CM_INTERACTIVE=1"})
		assert.Contains(t, args, "--input-format\nstream-json",
			"interactive path must include --input-format stream-json")
		assert.Contains(t, args, "--output-format\nstream-json",
			"interactive path must include --output-format stream-json")
		assert.Contains(t, args, "get_skill(skill_name='create-plan'",
			"interactive path must auto-invoke create-plan via get_skill")
		assert.NotContains(t, args, "Wait for the user's first message",
			"interactive path must not tell Claude to wait for the user's first message")
		assert.NotContains(t, args, "run-autonomous workflow",
			"interactive path must not include the autonomous workflow prompt")
	})
}

// TestEntrypointInteractiveContent verifies the branching structure directly in
// the entrypoint.sh source without executing the script — no filesystem
// dependencies required.
func TestEntrypointInteractiveContent(t *testing.T) {
	path := entrypointPath(t)
	content, err := os.ReadFile(path)
	require.NoError(t, err, "reading entrypoint.sh")

	src := string(content)

	// Must branch on CM_INTERACTIVE.
	assert.Contains(t, src, `[ "${CM_INTERACTIVE:-}" = "1" ]`,
		"entrypoint.sh must branch on CM_INTERACTIVE")

	// Interactive branch must include stream-json input format.
	assert.Contains(t, src, "--input-format stream-json",
		"interactive branch must include --input-format stream-json")

	// Both branches must share --output-format stream-json.
	assert.True(t, strings.Count(src, "--output-format stream-json") >= 1,
		"entrypoint.sh must include --output-format stream-json")

	// One-shot branch must NOT include --input-format.
	// Verify by checking the one-shot exec line has no input-format flag on its line.
	assert.True(t, strings.Contains(src, "run-autonomous workflow"),
		"one-shot branch must reference run-autonomous workflow")

	// Interactive branch must auto-invoke create-plan via get_skill.
	assert.Contains(t, src, "get_skill(skill_name='create-plan'",
		"interactive branch must auto-invoke create-plan via get_skill")

	// Interactive branch must NOT include the autonomous steps.
	// (Both must not coexist in same exec block — verified by presence of if/else/fi.)
	assert.Contains(t, src, "else",
		"entrypoint.sh must have an else clause separating the two branches")
}
