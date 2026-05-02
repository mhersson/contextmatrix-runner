package orchestrated

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/webhook"
)

// TestBuildWorkerSpec_SecretsRoutedToFile confirms that when a secrets
// file path is supplied, credential-bearing env keys are routed off
// Config.Env (where docker-inspect would expose them) and into the
// returned secrets map for the caller to write to disk.
//
// GitHub tokens (CM_GIT_TOKEN / GH_TOKEN) are NOT spawn-time secrets:
// the runner mints them fresh per docker-exec via workspaceExec, so
// they must appear in neither Config.Env nor the secrets file.
func TestBuildWorkerSpec_SecretsRoutedToFile(t *testing.T) {
	t.Parallel()

	dp := &Dispatcher{
		cfg: &config.Config{
			AgentImage:       "contextmatrix/orchestrated@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			ClaudeOAuthToken: "claude-oauth-secret",
		},
	}
	payload := webhook.TriggerPayload{
		CardID:    "ALPHA-9",
		Project:   "alpha",
		MCPAPIKey: "mcp-secret",
	}

	spec, secrets := dp.buildWorkerSpec(payload, "http://cm:8080", "/tmp/cm-secrets/cm-agent-alpha-alpha-9.env")

	// Spawn-time secret keys must NOT appear in the container env spec.
	for _, key := range []string{"MCP_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		_, present := spec.Env[key]
		assert.False(t, present, "secret %q must not be on Config.Env when secretsFilePath is set", key)
	}

	// GitHub tokens are per-exec, not spawn-time. They must appear
	// in neither Config.Env nor the secrets file.
	for _, key := range []string{"CM_GIT_TOKEN", "GH_TOKEN"} {
		_, envPresent := spec.Env[key]
		_, secretPresent := secrets[key]
		assert.False(t, envPresent, "%q must not be on Config.Env (per-exec only)", key)
		assert.False(t, secretPresent, "%q must not be in the spawn-time secrets file (per-exec only)", key)
	}

	// Non-secret plumbing stays on Env.
	assert.Equal(t, "http://cm:8080/mcp", spec.Env["MCP_URL"])
	assert.Equal(t, "ALPHA-9", spec.Env["CM_CARD_ID"])
	assert.Equal(t, "alpha", spec.Env["CM_PROJECT"])

	// Secrets map carries the spawn-time credentials.
	assert.Equal(t, "mcp-secret", secrets["MCP_API_KEY"])
	assert.Equal(t, "claude-oauth-secret", secrets["CLAUDE_CODE_OAUTH_TOKEN"])

	// Mount targeting /run/cm-secrets/env must be present and read-only.
	var found bool

	for _, m := range spec.Mounts {
		if m.Target == secretsMountTarget {
			found = true

			assert.True(t, m.ReadOnly, "secrets mount must be read-only")
			assert.Equal(t, "/tmp/cm-secrets/cm-agent-alpha-alpha-9.env", m.Source)
		}
	}

	assert.True(t, found, "expected a /run/cm-secrets/env mount")
}

// TestBuildWorkerSpec_SecretsFallbackToEnv confirms the legacy ergonomic
// path: when no secrets file path is provided, secrets fold back into
// Config.Env so the worker still receives them. Used only by unit tests
// that don't stage a real on-disk file.
func TestBuildWorkerSpec_SecretsFallbackToEnv(t *testing.T) {
	t.Parallel()

	dp := &Dispatcher{
		cfg: &config.Config{
			AgentImage:      "contextmatrix/orchestrated@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			AnthropicAPIKey: "anthropic-secret",
		},
	}
	payload := webhook.TriggerPayload{
		CardID:    "BETA-1",
		Project:   "beta",
		MCPAPIKey: "mcp-secret",
	}

	spec, secrets := dp.buildWorkerSpec(payload, "http://cm:8080", "")

	// Without a secrets file path, the credentials live on Env.
	assert.Equal(t, "mcp-secret", spec.Env["MCP_API_KEY"])
	assert.Equal(t, "anthropic-secret", spec.Env["ANTHROPIC_API_KEY"])
	assert.Nil(t, secrets, "secrets map should be nil when folded back into Env")

	for _, m := range spec.Mounts {
		assert.NotEqual(t, secretsMountTarget, m.Target, "no secrets mount when secretsFilePath is empty")
	}

	// GitHub tokens stay per-exec — never on Config.Env, even in the
	// fallback path.
	for _, key := range []string{"CM_GIT_TOKEN", "GH_TOKEN"} {
		_, present := spec.Env[key]
		assert.False(t, present, "%q must not be on Config.Env (per-exec only)", key)
	}
}

// TestWriteSecretsFile confirms the on-disk shape: 0600 perms, sorted
// KEY=VALUE lines using shell-quoted values so the entrypoint can
// `. /run/cm-secrets/env` it without re-quoting.
func TestWriteSecretsFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "cm-secrets", "worker.env")

	require.NoError(t, writeSecretsFile(path, map[string]string{
		"FOO": "bar",
		"BAZ": "value with spaces",
	}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	content, err := os.ReadFile(path)
	require.NoError(t, err)

	got := string(content)
	// Keys should be sorted (BAZ before FOO).
	assert.Contains(t, got, `export BAZ="value with spaces"`)
	assert.Contains(t, got, `export FOO="bar"`)
	assert.Less(t,
		// BAZ line comes before FOO line in sorted order.
		strings.Index(got, "BAZ"),
		strings.Index(got, "FOO"),
	)
}
