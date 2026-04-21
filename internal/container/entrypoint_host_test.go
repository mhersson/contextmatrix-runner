package container

import (
	"context"
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
// We use parameter expansion + a `case` allowlist rather than sed because
// sed is line-oriented: a newline in the input would yield a multi-line
// value that is catastrophic when interpolated into a .netrc/credential
// helper. See CTXRUN-043.
const hostExtractionSnippet = `
GIT_HOST=""
case "${CM_REPO_URL:-}" in
    https://*)
        _rest="${CM_REPO_URL#https://}"
        GIT_HOST="${_rest%%/*}"
        ;;
esac
case "$GIT_HOST" in
    -*|*[!A-Za-z0-9.-]*)
        GIT_HOST=""
        ;;
esac
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

	cmd := exec.CommandContext(context.Background(), "sh", "-c", hostExtractionSnippet)

	cmd.Env = append(os.Environ(), "CM_REPO_URL="+cmRepoURL)
	out, err := cmd.Output()
	require.NoError(t, err, "shell snippet failed for CM_REPO_URL=%q", cmRepoURL)

	return string(out)
}

func TestEntrypointGitHostExtraction(t *testing.T) {
	cases := []struct {
		name      string
		cmRepoURL string
		wantHost  string
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
		{
			// CTXRUN-043: a newline in the host would inject a second
			// `machine` clause into .netrc / a second line into the
			// credential helper. The case-based extractor must reject
			// any such value and fall back to github.com.
			name:      "newline in host rejects and falls back",
			cmRepoURL: "https://evil\nhost/org/repo.git",
			wantHost:  "github.com",
		},
		{
			name:      "host with ; injection rejects and falls back",
			cmRepoURL: "https://host;rm -rf/.git/",
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

// branchValidatorSnippet mirrors the case-based CM_BASE_BRANCH validator in
// docker/entrypoint.sh. It exits 0 on accept, 1 on reject.
const branchValidatorSnippet = `
case "${CM_BASE_BRANCH:-}" in
    "") exit 0 ;;
    -*|*[!A-Za-z0-9._/-]*) exit 1 ;;
esac
exit 0
`

// TestEntrypointBranchValidator verifies the new case-based branch validator
// rejects injection payloads that the old grep-based one would let through.
// Feeding a literal newline to the subshell via CM_BASE_BRANCH covers C6.
func TestEntrypointBranchValidator(t *testing.T) {
	cases := []struct {
		name      string
		branch    string
		wantAllow bool
	}{
		{"plain main", "main", true},
		{"slash feature branch", "feature/my-branch", true},
		{"dots and underscore", "release_1.2.3", true},
		{"empty allowed", "", true},
		{"leading dash rejected", "-rf", false},
		{"whitespace rejected", "foo bar", false},
		{"newline rejected", "main\n--upload-pack=evil", false},
		{"carriage return rejected", "main\r--upload-pack=evil", false},
		{"NUL rejected", "main\x00", false},
		{"semicolon rejected", "main;id", false},
		{"dollar rejected", "$(whoami)", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), "sh", "-c", branchValidatorSnippet)

			cmd.Env = append(os.Environ(), "CM_BASE_BRANCH="+tc.branch)

			err := cmd.Run()
			if tc.wantAllow {
				assert.NoError(t, err, "branch %q should be allowed", tc.branch)
			} else {
				assert.Error(t, err, "branch %q should be rejected", tc.branch)
			}
		})
	}
}

// TestEntrypointBranchValidatorInSource verifies the case-pattern string is
// present verbatim in docker/entrypoint.sh so refactors cannot silently
// regress the validator to a line-oriented grep.
func TestEntrypointBranchValidatorInSource(t *testing.T) {
	path := entrypointPath(t)
	content, err := os.ReadFile(path)
	require.NoError(t, err, "reading entrypoint.sh")

	src := string(content)

	// The new whole-string case pattern.
	assert.Contains(t, src, `-*|*[!A-Za-z0-9._/-]*`,
		"entrypoint.sh must validate CM_BASE_BRANCH with a whole-string case pattern")

	// The old grep-based validator must be gone.
	assert.NotContains(t, src, `grep -qE '^-|[[:space:]]'`,
		"entrypoint.sh must not use the legacy grep-based branch validator (CTXRUN-043)")
}

// TestEntrypointSecretsFileSourcing verifies the entrypoint reads the tmpfs
// secrets file at /run/cm-secrets/env when present.
func TestEntrypointSecretsFileSourcing(t *testing.T) {
	path := entrypointPath(t)
	content, err := os.ReadFile(path)
	require.NoError(t, err, "reading entrypoint.sh")

	src := string(content)

	assert.Contains(t, src, `CM_SECRETS_FILE="/run/cm-secrets/env"`,
		"entrypoint.sh must define the CM_SECRETS_FILE path")
	assert.Contains(t, src, `. "$CM_SECRETS_FILE"`,
		"entrypoint.sh must source the secrets file")
	assert.Contains(t, src, `unset CM_GIT_TOKEN CM_MCP_API_KEY`,
		"entrypoint.sh must unset transient secrets before exec claude")
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

	// Must NOT use the legacy sed-based host extraction (CTXRUN-043:
	// sed is line-oriented, so a newline in CM_REPO_URL could yield a
	// multi-line value. Parameter expansion + case allowlist is required).
	assert.NotContains(t, src, "sed -n 's#^https://",
		"entrypoint.sh must not use sed for host extraction (CTXRUN-043)")

	// Must derive GIT_HOST via parameter expansion.
	assert.Contains(t, src, `_rest="${CM_REPO_URL#https://}"`,
		"entrypoint.sh must extract host via parameter expansion")
	assert.Contains(t, src, `GIT_HOST="${_rest%%/*}"`,
		"entrypoint.sh must slice GIT_HOST from _rest via parameter expansion")

	// Must export GH_HOST (required by gh CLI for non-github.com hosts).
	assert.Contains(t, src, "export GH_HOST=",
		"entrypoint.sh must export GH_HOST")

	// Must NOT contain hardcoded url.insteadOf for github.com specifically
	// (should use $GIT_HOST variable instead).
	assert.NotContains(t, src, `url."https://github.com/".insteadOf`,
		"entrypoint.sh must not hardcode github.com in url.insteadOf; use $GIT_HOST")
}

// TestEntrypointInteractiveContent verifies the branching structure directly in
// the entrypoint.sh source without executing the script — no filesystem
// dependencies required.
//
// The previous TestEntrypointInteractiveBranching that executed entrypoint.sh
// via bash+mocks was removed as part of CTXRUN-057: it was gated behind
// CM_ENTRYPOINT_HOST_TEST=1, required write access to /home/user/workspace on
// the host, and duplicated the same assertions that this source-inspection
// test already covers. Keeping the dead-gated runner path made the file harder
// to reason about; this source-level check is sufficient.
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
	assert.GreaterOrEqual(t, strings.Count(src, "--output-format stream-json"), 1,
		"entrypoint.sh must include --output-format stream-json")

	// One-shot branch must NOT include --input-format.
	// Verify by checking the one-shot exec line has no input-format flag on its line.
	assert.Contains(t, src, "run-autonomous workflow",
		"one-shot branch must reference run-autonomous workflow")

	// Interactive branch must contain the minimal context hint.
	// Workflow-start instructions now live in the priming stream-json message,
	// not in the -p prompt.
	assert.Contains(t, src, "A human user may send you approval messages",
		"interactive branch must contain the minimal context hint")

	// Interactive branch must NOT include the autonomous steps.
	// (Both must not coexist in same exec block — verified by presence of if/else/fi.)
	assert.Contains(t, src, "else",
		"entrypoint.sh must have an else clause separating the two branches")
}
