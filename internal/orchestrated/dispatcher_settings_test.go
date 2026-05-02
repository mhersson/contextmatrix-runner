package orchestrated

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/spawn"
	"github.com/mhersson/contextmatrix-runner/internal/webhook"
)

// TestBuildWorkerSpec_ClaudeAuthDirMount asserts that claude_auth_dir is
// bind-mounted read-only at /claude-auth, the sibling .claude.json file is
// also bind-mounted read-only at /claude-auth.json when present, and that
// nothing is bind-mounted writable into the worker's home.
func TestBuildWorkerSpec_ClaudeAuthDirMount(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	authDir := filepath.Join(tmp, ".claude")
	require.NoError(t, os.MkdirAll(authDir, 0o700))

	siblingJSON := filepath.Join(tmp, ".claude.json")
	require.NoError(t, os.WriteFile(siblingJSON, []byte(`{"userID":"abc"}`), 0o600))

	dp := newTestDispatcher()
	dp.cfg = &config.Config{
		AgentImage:    "contextmatrix/orchestrated@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		ClaudeAuthDir: authDir,
	}
	payload := webhook.TriggerPayload{CardID: "C-1", Project: "p", MCPAPIKey: "k"}

	spec, _ := dp.buildWorkerSpec(t.Context(), payload, "http://cm:8080", "")

	var (
		authMount *spawn.Mount
		jsonMount *spawn.Mount
	)

	for i := range spec.Mounts {
		switch spec.Mounts[i].Source {
		case authDir:
			authMount = &spec.Mounts[i]
		case siblingJSON:
			jsonMount = &spec.Mounts[i]
		}
	}

	require.NotNil(t, authMount, "claude_auth_dir mount must be present")
	assert.Equal(t, "/claude-auth", authMount.Target)
	assert.True(t, authMount.ReadOnly,
		"claude_auth_dir must be read-only so worker writes never reach the host")

	require.NotNil(t, jsonMount, "sibling .claude.json mount must be present when the host file exists")
	assert.Equal(t, "/claude-auth.json", jsonMount.Target)
	assert.True(t, jsonMount.ReadOnly)
}

// TestBuildWorkerSpec_ClaudeAuthDirSiblingJSONAbsent asserts that we do NOT
// emit the sibling .claude.json mount when the host file doesn't exist —
// otherwise Docker would create an empty directory at the source path.
func TestBuildWorkerSpec_ClaudeAuthDirSiblingJSONAbsent(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	authDir := filepath.Join(tmp, ".claude")
	require.NoError(t, os.MkdirAll(authDir, 0o700))
	// Note: no sibling .claude.json file.

	dp := newTestDispatcher()
	dp.cfg = &config.Config{
		AgentImage:    "contextmatrix/orchestrated@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		ClaudeAuthDir: authDir,
	}
	payload := webhook.TriggerPayload{CardID: "C-1", Project: "p", MCPAPIKey: "k"}

	spec, _ := dp.buildWorkerSpec(t.Context(), payload, "http://cm:8080", "")

	for i := range spec.Mounts {
		assert.NotEqual(t, "/claude-auth.json", spec.Mounts[i].Target,
			"sibling .claude.json mount must be absent when the host file does not exist")
	}
}

// TestBuildWorkerSpec_ClaudeSettingsEnv asserts that claude_settings, when
// configured, is plumbed to the worker as CM_CLAUDE_SETTINGS so the
// entrypoint can write $HOME/.claude/settings.json. When unset the env var
// must not appear so the entrypoint skips the write.
func TestBuildWorkerSpec_ClaudeSettingsEnv(t *testing.T) {
	t.Parallel()

	settings := `{"includeCoAuthoredBy":false}`

	tests := []struct {
		name     string
		settings string
		want     string
		present  bool
	}{
		{"present when set", settings, settings, true},
		{"absent when empty", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dp := newTestDispatcher()
			dp.cfg = &config.Config{
				AgentImage:     "contextmatrix/orchestrated@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
				ClaudeSettings: tc.settings,
			}
			payload := webhook.TriggerPayload{CardID: "C-1", Project: "p", MCPAPIKey: "k"}

			spec, _ := dp.buildWorkerSpec(t.Context(), payload, "http://cm:8080", "")

			got, ok := spec.Env["CM_CLAUDE_SETTINGS"]
			assert.Equal(t, tc.present, ok, "CM_CLAUDE_SETTINGS env presence")
			assert.Equal(t, tc.want, got)
		})
	}
}
