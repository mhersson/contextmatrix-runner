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
// file path is supplied, MCP_API_KEY is routed off Config.Env (where
// docker-inspect would expose it) and into the returned secrets map
// for the caller to write to disk.
//
// CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY do NOT appear in either
// place: docker exec ignores PID 1's env, so anything sourced into the
// entrypoint shell from the secrets file is invisible to the
// subsequent `claude` exec. Those land per-exec via
// driver.Config.ClaudeAuthEnv (covered by TestClaudeAuthEnv).
//
// GitHub tokens (CM_GIT_TOKEN / GH_TOKEN) are likewise per-exec only.
func TestBuildWorkerSpec_SecretsRoutedToFile(t *testing.T) {
	t.Parallel()

	dp := newTestDispatcher()
	dp.cfg = &config.Config{
		AgentImage:       "contextmatrix/orchestrated@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		ClaudeOAuthToken: "claude-oauth-secret",
	}
	payload := webhook.TriggerPayload{
		CardID:    "ALPHA-9",
		Project:   "alpha",
		MCPAPIKey: "mcp-secret",
	}

	spec, secrets := dp.buildWorkerSpec(t.Context(), payload, "http://cm:8080", "/tmp/cm-secrets/cm-agent-alpha-alpha-9.env")

	// MCP_API_KEY is staged via the secrets file, never on Config.Env.
	_, present := spec.Env["MCP_API_KEY"]
	assert.False(t, present, "MCP_API_KEY must not be on Config.Env when secretsFilePath is set")

	// Per-exec creds: never on Config.Env, never in the secrets file.
	// They flow through driver.Config.ClaudeAuthEnv instead.
	for _, key := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "CM_GIT_TOKEN", "GH_TOKEN"} {
		_, envPresent := spec.Env[key]
		_, secretPresent := secrets[key]
		assert.False(t, envPresent, "%q must not be on Config.Env (per-exec only)", key)
		assert.False(t, secretPresent, "%q must not be in the spawn-time secrets file (per-exec only)", key)
	}

	// Non-secret plumbing stays on Env.
	assert.Equal(t, "http://cm:8080/mcp", spec.Env["MCP_URL"])
	assert.Equal(t, "ALPHA-9", spec.Env["CM_CARD_ID"])
	assert.Equal(t, "alpha", spec.Env["CM_PROJECT"])

	// Secrets map carries the MCP key.
	assert.Equal(t, "mcp-secret", secrets["MCP_API_KEY"])

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
// path: when no secrets file path is provided, the MCP key folds back
// into Config.Env so the worker still receives it. Used only by unit
// tests that don't stage a real on-disk file.
//
// Claude auth credentials remain per-exec in both paths (covered by
// TestClaudeAuthEnv); they must never appear on Config.Env.
func TestBuildWorkerSpec_SecretsFallbackToEnv(t *testing.T) {
	t.Parallel()

	dp := newTestDispatcher()
	dp.cfg = &config.Config{
		AgentImage:      "contextmatrix/orchestrated@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		AnthropicAPIKey: "anthropic-secret",
	}
	payload := webhook.TriggerPayload{
		CardID:    "BETA-1",
		Project:   "beta",
		MCPAPIKey: "mcp-secret",
	}

	spec, secrets := dp.buildWorkerSpec(t.Context(), payload, "http://cm:8080", "")

	// Without a secrets file path, the MCP key lives on Env.
	assert.Equal(t, "mcp-secret", spec.Env["MCP_API_KEY"])
	assert.Nil(t, secrets, "secrets map should be nil when folded back into Env")

	// Claude auth credentials are per-exec only — never on Config.Env,
	// even in the fallback path.
	for _, key := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "CM_GIT_TOKEN", "GH_TOKEN"} {
		_, present := spec.Env[key]
		assert.False(t, present, "%q must not be on Config.Env (per-exec only)", key)
	}

	for _, m := range spec.Mounts {
		assert.NotEqual(t, secretsMountTarget, m.Target, "no secrets mount when secretsFilePath is empty")
	}
}

// TestClaudeAuthEnv pins the per-exec env the orchestrator injects on
// every Claude docker-exec for each supported auth mode. The mapping
// must mirror buildWorkerSpec's auth-dir mount precedence:
//
//	claude_auth_dir > claude_oauth_token > anthropic_api_key
//
// claude_auth_dir returns nil because Claude reads
// ~/.claude/.credentials.json from the bind-mount instead of env. The
// other two return the env var name Claude actually reads on startup.
//
// Pre-fix this code path didn't exist: the OAuth token was routed
// through the secrets file, sourced into the entrypoint shell, deleted,
// and then invisible to every subsequent `docker exec claude` —
// surfacing as "Not logged in · Please run /login" inside the worker.
func TestClaudeAuthEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *config.Config
		want map[string]string
	}{
		{
			name: "auth_dir takes priority and yields no env",
			cfg: &config.Config{
				ClaudeAuthDir:    "/host/.claude",
				ClaudeOAuthToken: "ignored-when-auth-dir-set",
				AnthropicAPIKey:  "also-ignored",
			},
			want: nil,
		},
		{
			name: "oauth token wins over anthropic api key",
			cfg: &config.Config{
				ClaudeOAuthToken: "tok-abc",
				AnthropicAPIKey:  "ignored-when-oauth-set",
			},
			want: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-abc"},
		},
		{
			name: "anthropic api key when nothing else set",
			cfg:  &config.Config{AnthropicAPIKey: "key-xyz"},
			want: map[string]string{"ANTHROPIC_API_KEY": "key-xyz"},
		},
		{
			name: "no auth configured returns nil",
			cfg:  &config.Config{},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, claudeAuthEnv(tc.cfg))
		})
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
