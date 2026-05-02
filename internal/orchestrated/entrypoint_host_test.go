package orchestrated

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// extractEntrypointBlock pulls the shell fragment between two named
// sentinel comments out of docker/entrypoint-orchestrated.sh so a test
// can exec just that fragment without inheriting the rest of the
// entrypoint (which assumes /workspace, /run/cm-secrets, MCP config,
// etc.).
func extractEntrypointBlock(t *testing.T, startMarker, endMarker string) string {
	t.Helper()

	// The test runs from internal/orchestrated/. The repo root is two dirs up.
	repoRoot, err := filepath.Abs("../../")
	require.NoError(t, err)

	path := filepath.Join(repoRoot, "docker", "entrypoint-orchestrated.sh")

	data, err := os.ReadFile(path)
	require.NoError(t, err, "read %s", path)

	src := string(data)

	startIdx := strings.Index(src, startMarker)
	require.GreaterOrEqual(t, startIdx, 0, "start marker %q missing in entrypoint", startMarker)

	endIdx := strings.Index(src, endMarker)
	require.Greater(t, endIdx, startIdx, "end marker %q missing or before start", endMarker)

	return src[startIdx : endIdx+len(endMarker)]
}

func extractTaskSkillsBlock(t *testing.T) string {
	t.Helper()

	return extractEntrypointBlock(t,
		"# ----- Task skills (start) -----",
		"# ----- Task skills (end) -----",
	)
}

func extractCredHelperBlock(t *testing.T) string {
	t.Helper()

	return extractEntrypointBlock(t,
		"# ----- Git credential helper (start) -----",
		"# ----- Git credential helper (end) -----",
	)
}

func TestEntrypointTaskSkillsBlock(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	block := extractTaskSkillsBlock(t)

	cases := []struct {
		name        string
		setEnv      bool
		csv         string
		populateDir bool
		wantSkills  []string
		wantWarns   []string
	}{
		{
			name:        "no constraint copies every subdir",
			setEnv:      false,
			populateDir: true,
			wantSkills:  []string{"alpha", "beta"},
		},
		{
			name:        "explicit list copies only requested skills",
			setEnv:      true,
			csv:         "alpha",
			populateDir: true,
			wantSkills:  []string{"alpha"},
		},
		{
			name:        "explicit empty copies nothing",
			setEnv:      true,
			csv:         "",
			populateDir: true,
			wantSkills:  nil,
		},
		{
			name:        "missing skill warns and continues",
			setEnv:      true,
			csv:         "alpha,gamma",
			populateDir: true,
			wantSkills:  []string{"alpha"},
			wantWarns:   []string{"gamma"},
		},
		{
			name:        "invalid name is skipped silently",
			setEnv:      true,
			csv:         "alpha,..,bad/name",
			populateDir: true,
			wantSkills:  []string{"alpha"},
		},
		{
			name:        "missing host dir is a no-op",
			setEnv:      false,
			populateDir: false,
			wantSkills:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o750))

			var hostSkills string

			if tc.populateDir {
				hostSkills = filepath.Join(t.TempDir(), "host-skills")
				for _, s := range []string{"alpha", "beta"} {
					sd := filepath.Join(hostSkills, s)
					require.NoError(t, os.MkdirAll(sd, 0o750))
					require.NoError(t, os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte("# "+s), 0o600))
				}
			} else {
				hostSkills = filepath.Join(t.TempDir(), "does-not-exist")
			}

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, "sh", "-eu", "-c", block)

			cmd.Env = append(os.Environ(),
				"HOME="+home,
				"CM_HOST_SKILLS_DIR="+hostSkills,
			)

			if tc.setEnv {
				cmd.Env = append(cmd.Env,
					"CM_TASK_SKILLS_SET=1",
					"CM_TASK_SKILLS="+tc.csv,
				)
			}

			var stderr bytes.Buffer

			cmd.Stderr = &stderr

			require.NoError(t, cmd.Run(), "block must succeed; stderr=%s", stderr.String())

			entries, err := os.ReadDir(filepath.Join(home, ".claude", "skills"))
			require.NoError(t, err)

			gotSkills := make([]string, 0, len(entries))
			for _, e := range entries {
				if e.IsDir() {
					gotSkills = append(gotSkills, e.Name())
				}
			}

			assert.ElementsMatch(t, tc.wantSkills, gotSkills)

			for _, w := range tc.wantWarns {
				assert.Contains(t, stderr.String(), w, "expected warning to mention %q", w)
			}
		})
	}
}

// TestEntrypointCredHelperBlock confirms the credential helper that the
// entrypoint installs reads its token from the per-exec process env at
// the moment git asks for credentials, NOT from a file on disk and NOT
// from the entrypoint shell's env. This is what lets `gh pr create`
// and long-running git ops work past the App installation token's
// 1-hour TTL: the runner injects a fresh CM_GIT_TOKEN on every docker
// exec and the helper picks it up.
func TestEntrypointCredHelperBlock(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	block := extractCredHelperBlock(t)

	cases := []struct {
		name   string
		token  string
		wantPW string
	}{
		{
			name:   "fresh token reaches git via env",
			token:  "ghs_freshtoken123",
			wantPW: "ghs_freshtoken123",
		},
		{
			name:   "rotated token wins over previous (no on-disk staleness)",
			token:  "ghs_rotated456",
			wantPW: "ghs_rotated456",
		},
		{
			name:   "missing token yields empty password (no crash)",
			token:  "",
			wantPW: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			// Step 1: run the credential-helper install block as the
			// entrypoint would. It writes ~/.cm-git-cred-helper.sh and
			// configures git's global credential.helper.
			install := exec.CommandContext(ctx, "sh", "-eu", "-c", block)

			install.Env = append(os.Environ(), "HOME="+home)

			var installErr bytes.Buffer

			install.Stderr = &installErr
			require.NoError(t, install.Run(), "install block must succeed; stderr=%s", installErr.String())

			helperPath := filepath.Join(home, ".cm-git-cred-helper.sh")
			info, err := os.Stat(helperPath)
			require.NoError(t, err, "helper script must be created")
			assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "helper must be 0700")

			// Step 2: drive the helper directly with `get` and the per-exec
			// env the runner would inject. This is exactly what git does
			// when credential.helper is set to "!<script>".
			run := exec.CommandContext(ctx, helperPath, "get")
			run.Env = []string{"HOME=" + home}

			if tc.token != "" {
				run.Env = append(run.Env, "CM_GIT_TOKEN="+tc.token)
			}

			out, err := run.Output()
			require.NoError(t, err, "helper must exit cleanly even with empty token")

			assert.Contains(t, string(out), "username=x-access-token\n",
				"helper must always identify as x-access-token")
			assert.Contains(t, string(out), "password="+tc.wantPW+"\n",
				"helper must echo CM_GIT_TOKEN as the password")
		})
	}
}

// TestEntrypointCredHelperLeavesNoTokenOnDisk guards against a
// regression where someone reintroduces the legacy "stash the token in
// ~/.cm-git-cred/token" pattern. The whole point of the env-reading
// helper is that no token ever sits on the worker filesystem.
func TestEntrypointCredHelperLeavesNoTokenOnDisk(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	block := extractCredHelperBlock(t)
	home := t.TempDir()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-eu", "-c", block)

	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"CM_GIT_TOKEN=ghs_should_not_be_persisted",
	)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "block must succeed; stderr=%s", stderr.String())

	// The only file the entrypoint should write is the helper script
	// itself, and it must NOT contain the literal token (the helper
	// reads from env at call time, it does not bake the token in).
	helper, err := os.ReadFile(filepath.Join(home, ".cm-git-cred-helper.sh"))
	require.NoError(t, err)
	assert.NotContains(t, string(helper), "ghs_should_not_be_persisted",
		"helper script must not bake the token in; it reads from env")

	// Legacy locations from the previous file-based scheme must be absent.
	for _, leftover := range []string{
		filepath.Join(home, ".cm-git-cred"),
		filepath.Join(home, ".cm-git-cred", "token"),
		filepath.Join(home, ".config", "gh", "hosts.yml"),
	} {
		_, err := os.Stat(leftover)
		assert.True(t, os.IsNotExist(err), "no token-on-disk allowed at %s", leftover)
	}
}
