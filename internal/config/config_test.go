package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDigestImage is a placeholder digest-pinned reference that satisfies
// Validate()'s CTXRUN-044 digest-pinning check. Tests that do not exercise
// the pinning rule itself reuse this constant so the unrelated Validate
// paths stay readable.
const testDigestImage = "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestLogFormat_JSON_EmitsValidJSON verifies that configuring log_format: json
// produces parseable JSON log lines. (Belongs to CTXRUN-053.)
func TestLogFormat_JSON_EmitsValidJSON(t *testing.T) {
	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("hello", "k", "v", "n", 7)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded), "JSON handler output must parse cleanly")

	assert.Equal(t, "hello", decoded["msg"])
	assert.Equal(t, "v", decoded["k"])
}

// TestLogFormat_ValidationRejectsUnknown verifies that invalid log_format
// values are rejected at Validate-time.
func TestLogFormat_ValidationRejectsUnknown(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	claudeDir := dir

	yaml := validConfig(pemPath, claudeDir) + "\nlog_format: yaml\n"
	path := writeConfig(t, dir, yaml)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log_format")
}

// TestAdminPort_DefaultsAnd_ValidationRange verifies the default admin_port
// value and that out-of-range values fail validation.
func TestAdminPort_DefaultsAnd_ValidationRange(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	claudeDir := dir

	// Default should be 9091 when unset.
	path := writeConfig(t, dir, validConfig(pemPath, claudeDir))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 9091, cfg.AdminPort)

	// Out-of-range should fail.
	yaml := validConfig(pemPath, claudeDir) + "\nadmin_port: 70000\n"
	path = writeConfig(t, dir, yaml)
	_, err = Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin_port")
}

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()

	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func writePEM(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, "app.pem")
	require.NoError(t, os.WriteFile(path, []byte("fake-key"), 0o600))

	return path
}

func validConfig(pemPath, claudeDir string) string {
	return `
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
claude_auth_dir: "` + claudeDir + `"
github_app:
  app_id: 12345
  installation_id: 67890
  private_key_path: "` + pemPath + `"
`
}

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	claudeDir := dir // reuse temp dir as stand-in

	path := writeConfig(t, dir, validConfig(pemPath, claudeDir))
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 9090, cfg.Port)
	assert.Equal(t, "http://localhost:8080", cfg.ContextMatrixURL)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", cfg.APIKey)
	assert.Equal(t, "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", cfg.BaseImage)
	assert.Equal(t, 3, cfg.MaxConcurrent)
	assert.Equal(t, "2h", cfg.ContainerTimeout)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, int64(12345), cfg.GitHubApp.AppID)
	assert.Equal(t, int64(67890), cfg.GitHubApp.InstallationID)
	assert.Equal(t, pemPath, cfg.GitHubApp.PrivateKeyPath)
}

func TestLoad_Defaults(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	path := writeConfig(t, dir, validConfig(pemPath, dir))
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 9090, cfg.Port)
	assert.Equal(t, 3, cfg.MaxConcurrent)
	assert.Equal(t, "2h", cfg.ContainerTimeout)
	assert.Equal(t, "info", cfg.LogLevel)
	// CTXRUN-048: default auth mode for VerifyAutonomous is HMAC.
	assert.True(t, cfg.UseHMACForVerifyAutonomous,
		"use_hmac_for_verify_autonomous must default to true")
}

func TestLoad_EnvOverrides(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	path := writeConfig(t, dir, validConfig(pemPath, dir))

	t.Setenv("CMR_PORT", "7070")
	t.Setenv("CMR_MAX_CONCURRENT", "5")
	t.Setenv("CMR_LOG_LEVEL", "debug")

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 7070, cfg.Port)
	assert.Equal(t, 5, cfg.MaxConcurrent)
	assert.Equal(t, "debug", cfg.LogLevel)
}

func TestValidate_MissingContextMatrixURL(t *testing.T) {
	cfg := &Config{
		APIKey:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage: testDigestImage,
	}
	err := cfg.Validate()
	assert.ErrorContains(t, err, "contextmatrix_url is required")
}

func TestValidate_APIKeyTooShort(t *testing.T) {
	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "short",
		BaseImage:        testDigestImage,
	}
	err := cfg.Validate()
	assert.ErrorContains(t, err, "api_key must be at least")
}

func TestValidate_MissingBaseImage(t *testing.T) {
	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MaxConcurrent:    1,
		ContainerTimeout: "1h",
	}
	err := cfg.Validate()
	assert.ErrorContains(t, err, "base_image is required")
}

func TestValidate_NoCCAuth(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        testDigestImage,
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    1,
		ContainerTimeout: "1h",
		GitHubApp: GitHubApp{
			AppID:          1,
			InstallationID: 1,
			PrivateKeyPath: pemPath,
		},
	}
	err := cfg.Validate()
	assert.ErrorContains(t, err, "at least one of claude_auth_dir, claude_oauth_token, or anthropic_api_key is required")
}

func TestValidate_AnthropicAPIKeyAlone(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        testDigestImage,
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    1,
		ContainerTimeout: "1h",
		AnthropicAPIKey:  "sk-ant-test",
		GitHubApp: GitHubApp{
			AppID:          1,
			InstallationID: 1,
			PrivateKeyPath: pemPath,
		},
	}
	err := cfg.Validate()
	assert.NoError(t, err)
}

func TestValidate_ClaudeOAuthTokenAlone(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        testDigestImage,
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    1,
		ContainerTimeout: "1h",
		ClaudeOAuthToken: "oauth-token-value",
		GitHubApp: GitHubApp{
			AppID:          1,
			InstallationID: 1,
			PrivateKeyPath: pemPath,
		},
	}
	err := cfg.Validate()
	assert.NoError(t, err)
}

func TestValidate_AuthMethodsSatisfyRequirement(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	baseConfig := func() *Config {
		return &Config{
			ContextMatrixURL: "http://localhost",
			APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			BaseImage:        testDigestImage,
			ImagePullPolicy:  PullAlways,
			MaxConcurrent:    1,
			ContainerTimeout: "1h",
			GitHubApp: GitHubApp{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
		}
	}

	tests := []struct {
		name    string
		setup   func(cfg *Config)
		wantErr bool
	}{
		{
			name: "claude_auth_dir alone satisfies requirement",
			setup: func(cfg *Config) {
				cfg.ClaudeAuthDir = dir
			},
			wantErr: false,
		},
		{
			name: "claude_oauth_token alone satisfies requirement",
			setup: func(cfg *Config) {
				cfg.ClaudeOAuthToken = "oauth-token-value"
			},
			wantErr: false,
		},
		{
			name: "anthropic_api_key alone satisfies requirement",
			setup: func(cfg *Config) {
				cfg.AnthropicAPIKey = "sk-ant-test"
			},
			wantErr: false,
		},
		{
			name:    "none set fails validation",
			setup:   func(_ *Config) {},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			tt.setup(cfg)

			err := cfg.Validate()
			if tt.wantErr {
				assert.ErrorContains(t, err, "at least one of claude_auth_dir, claude_oauth_token, or anthropic_api_key is required")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestLoad_ClaudeOAuthTokenEnvOverride(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	// Config without any auth set — env override will provide the token.
	content := `
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
github_app:
  app_id: 12345
  installation_id: 67890
  private_key_path: "` + pemPath + `"
`
	path := writeConfig(t, dir, content)

	t.Setenv("CMR_CLAUDE_OAUTH_TOKEN", "my-oauth-token-value")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "my-oauth-token-value", cfg.ClaudeOAuthToken)
}

func TestValidate_InvalidContainerTimeout(t *testing.T) {
	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        testDigestImage,
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    1,
		ContainerTimeout: "not-a-duration",
		AnthropicAPIKey:  "sk-ant-test",
	}
	err := cfg.Validate()
	assert.ErrorContains(t, err, "container_timeout is invalid")
}

func TestValidate_GitHubAppMissingFields(t *testing.T) {
	dir := t.TempDir()

	// Neither App nor PAT configured → "either ... is required".
	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        testDigestImage,
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    1,
		ContainerTimeout: "1h",
		AnthropicAPIKey:  "sk-ant-test",
		GitHubApp:        GitHubApp{},
	}
	err := cfg.Validate()
	require.ErrorContains(t, err, "either github_app or github_pat is required")

	// Once any App field is set the App path is taken — missing fields surface.
	cfg.GitHubApp.AppID = 1
	err = cfg.Validate()
	require.ErrorContains(t, err, "github_app: installation_id is required")

	cfg.GitHubApp.InstallationID = 1
	err = cfg.Validate()
	require.ErrorContains(t, err, "github_app: private_key_path is required")

	cfg.GitHubApp.PrivateKeyPath = filepath.Join(dir, "nonexistent.pem")
	err = cfg.Validate()
	assert.ErrorContains(t, err, "private_key_path does not exist")
}

func TestContainerTimeoutDuration(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        testDigestImage,
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    1,
		ContainerTimeout: "2h",
		AnthropicAPIKey:  "sk-ant-test",
		GitHubApp: GitHubApp{
			AppID:          1,
			InstallationID: 1,
			PrivateKeyPath: pemPath,
		},
	}
	require.NoError(t, cfg.Validate())
	assert.Equal(t, 2*time.Hour, cfg.ContainerTimeoutDuration())
}

func TestValidate_ClaudeSettings(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	baseConfig := func() *Config {
		return &Config{
			ContextMatrixURL: "http://localhost",
			APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			BaseImage:        testDigestImage,
			ImagePullPolicy:  PullAlways,
			MaxConcurrent:    1,
			ContainerTimeout: "1h",
			AnthropicAPIKey:  "sk-ant-test",
			GitHubApp: GitHubApp{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
		}
	}

	tests := []struct {
		name     string
		settings string
		wantErr  bool
	}{
		{
			name:     "empty string is valid",
			settings: "",
			wantErr:  false,
		},
		{
			name:     "valid JSON object passes",
			settings: `{"includeCoAuthoredBy":false,"enabledPlugins":{"gopls-lsp@claude-plugins-official":true}}`,
			wantErr:  false,
		},
		{
			name:     "invalid JSON fails",
			settings: `{not valid json`,
			wantErr:  true,
		},
		{
			name:     "plain string is invalid JSON",
			settings: "hello",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.ClaudeSettings = tt.settings

			err := cfg.Validate()
			if tt.wantErr {
				assert.ErrorContains(t, err, "claude_settings is not valid JSON")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestLoad_ClaudeSettingsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	path := writeConfig(t, dir, validConfig(pemPath, dir))

	validJSON := `{"includeCoAuthoredBy":false}`
	t.Setenv("CMR_CLAUDE_SETTINGS", validJSON)

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.JSONEq(t, validJSON, cfg.ClaudeSettings)
}

func TestLoad_ClaudeSettingsEnvOverride_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	path := writeConfig(t, dir, validConfig(pemPath, dir))

	t.Setenv("CMR_CLAUDE_SETTINGS", "{invalid json")

	_, err := Load(path)
	require.Error(t, err)
	assert.ErrorContains(t, err, "claude_settings is not valid JSON")
}

func TestLoad_GitHubApp_APIBaseURL_YAML(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	content := `
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
claude_auth_dir: "` + dir + `"
github_app:
  app_id: 12345
  installation_id: 67890
  private_key_path: "` + pemPath + `"
  api_base_url: "https://api.acme.ghe.com"
`
	path := writeConfig(t, dir, content)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "https://api.acme.ghe.com", cfg.GitHubApp.APIBaseURL)
}

func TestLoad_GitHubApp_APIBaseURL_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	content := `
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
claude_auth_dir: "` + dir + `"
github_app:
  app_id: 12345
  installation_id: 67890
  private_key_path: "` + pemPath + `"
  api_base_url: "https://api.yaml.example"
`
	path := writeConfig(t, dir, content)

	t.Setenv("CMR_GITHUB_API_BASE_URL", "https://api.env.example")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "https://api.env.example", cfg.GitHubApp.APIBaseURL)
}

func TestLoad_GitHubApp_APIBaseURL_Default(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	// No api_base_url in YAML, no env var set.
	path := writeConfig(t, dir, validConfig(pemPath, dir))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.GitHubApp.APIBaseURL)
}

func TestLogLevelSlog(t *testing.T) {
	tests := []struct {
		level    string
		expected int
	}{
		{"debug", -4},
		{"info", 0},
		{"warn", 4},
		{"error", 8},
		{"unknown", 0}, // defaults to info
	}
	for _, tt := range tests {
		cfg := &Config{LogLevel: tt.level}
		assert.Equal(t, tt.expected, int(cfg.LogLevelSlog()), "level: %s", tt.level)
	}
}

// baseValidConfig returns a minimal valid config that satisfies all fields
// except the GitHub auth method, which the test can set.
func baseValidConfigNoGitHub(t *testing.T) *Config {
	t.Helper()

	return &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        testDigestImage,
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    1,
		ContainerTimeout: "1h",
		AnthropicAPIKey:  "sk-ant-test",
	}
}

func TestValidate_GitHubAuthMutualExclusivity(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	tests := []struct {
		name        string
		app         GitHubApp
		pat         GitHubPAT
		wantErr     bool
		errContains string
	}{
		{
			name: "app-only configured is valid",
			app: GitHubApp{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
			pat:     GitHubPAT{},
			wantErr: false,
		},
		{
			name:    "pat-only configured is valid",
			app:     GitHubApp{},
			pat:     GitHubPAT{Token: "ghp_testtoken"},
			wantErr: false,
		},
		{
			name: "both configured is an error",
			app: GitHubApp{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
			pat:         GitHubPAT{Token: "ghp_testtoken"},
			wantErr:     true,
			errContains: "exactly one of github_app or github_pat may be configured",
		},
		{
			name:        "neither configured is an error",
			app:         GitHubApp{},
			pat:         GitHubPAT{},
			wantErr:     true,
			errContains: "either github_app or github_pat is required",
		},
		{
			name:    "pat-only does not trigger GitHubApp.validate errors",
			app:     GitHubApp{}, // missing app_id etc. — should NOT surface
			pat:     GitHubPAT{Token: "ghp_patonly"},
			wantErr: false,
		},
		{
			name:        "any app field set counts as app-configured (app_id only)",
			app:         GitHubApp{AppID: 1}, // partial — triggers validate()
			pat:         GitHubPAT{},
			wantErr:     true,
			errContains: "github_app: installation_id is required",
		},
		{
			name:        "any app field set counts as app-configured (installation_id only)",
			app:         GitHubApp{InstallationID: 1}, // partial — triggers validate()
			pat:         GitHubPAT{},
			wantErr:     true,
			errContains: "github_app: app_id is required",
		},
		{
			name:        "any app field set counts as app-configured (private_key_path only)",
			app:         GitHubApp{PrivateKeyPath: "/tmp/key.pem"}, // partial — triggers validate()
			pat:         GitHubPAT{},
			wantErr:     true,
			errContains: "github_app: app_id is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseValidConfigNoGitHub(t)
			cfg.GitHubApp = tt.app
			cfg.GitHubPAT = tt.pat

			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.errContains)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidate_ReplayCacheDefaultsWhenUnset(t *testing.T) {
	// A Config literal that leaves the new CTXRUN-047 tunables at zero
	// must validate and receive the documented defaults.
	cfg := baseValidConfigNoGitHub(t)
	cfg.GitHubPAT = GitHubPAT{Token: "ghp_patonly"}

	require.NoError(t, cfg.Validate())

	assert.Equal(t, 10000, cfg.WebhookReplayCacheSize)
	assert.Equal(t, 330, cfg.WebhookReplaySkewSeconds)
	assert.Equal(t, 1000, cfg.MessageDedupCacheSize)
	assert.Equal(t, 600, cfg.MessageDedupTTLSeconds)
}

func TestValidate_ReplayCacheRejectsNegative(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name: "negative cache size",
			mutate: func(c *Config) {
				c.WebhookReplayCacheSize = -1
			},
			want: "webhook_replay_cache_size must be positive",
		},
		{
			name: "negative skew seconds",
			mutate: func(c *Config) {
				c.WebhookReplaySkewSeconds = -1
			},
			want: "webhook_replay_skew_seconds must be positive",
		},
		{
			name: "negative dedup cache size",
			mutate: func(c *Config) {
				c.MessageDedupCacheSize = -1
			},
			want: "message_dedup_cache_size must be positive",
		},
		{
			name: "negative dedup ttl",
			mutate: func(c *Config) {
				c.MessageDedupTTLSeconds = -1
			},
			want: "message_dedup_ttl_seconds must be positive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValidConfigNoGitHub(t)
			cfg.GitHubPAT = GitHubPAT{Token: "ghp_patonly"}
			tc.mutate(cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestLoad_GitHubPAT_EnvOverride(t *testing.T) {
	dir := t.TempDir()

	// Config with no GitHub auth at all — env override provides the PAT.
	content := `
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
anthropic_api_key: "sk-ant-test"
`
	path := writeConfig(t, dir, content)

	t.Setenv("CMR_GITHUB_PAT_TOKEN", "ghp_envtoken123")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "ghp_envtoken123", cfg.GitHubPAT.Token)
}

func TestLoad_GitHubPAT_YAMLOverriddenByEnv(t *testing.T) {
	dir := t.TempDir()

	content := `
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
anthropic_api_key: "sk-ant-test"
github_pat:
  token: "ghp_fromyaml"
`
	path := writeConfig(t, dir, content)

	t.Setenv("CMR_GITHUB_PAT_TOKEN", "ghp_fromenv")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "ghp_fromenv", cfg.GitHubPAT.Token)
}

// TestValidate_BaseImageDigestPin covers the CTXRUN-044 requirement that
// base_image be an @sha256:... reference. Mutable tags and malformed digests
// must fail validation so a rebuilt upstream image can never silently ship.
func TestValidate_BaseImageDigestPin(t *testing.T) {
	validDigest := "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	tests := []struct {
		name        string
		image       string
		wantErr     bool
		errContains string
	}{
		{
			name:    "digest-pinned base_image is accepted",
			image:   validDigest,
			wantErr: false,
		},
		{
			name:        "tag-only base_image is rejected",
			image:       "contextmatrix/worker:latest",
			wantErr:     true,
			errContains: "base_image must be @sha256:... pinned",
		},
		{
			name:        "bare name without tag or digest is rejected",
			image:       "contextmatrix/worker",
			wantErr:     true,
			errContains: "base_image must be @sha256:... pinned",
		},
		{
			name:        "digest of wrong length is rejected",
			image:       "contextmatrix/worker@sha256:deadbeef",
			wantErr:     true,
			errContains: "invalid sha256 digest length",
		},
		{
			name:        "digest with non-hex characters is rejected",
			image:       "contextmatrix/worker@sha256:zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
			wantErr:     true,
			errContains: "non-hex characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseValidConfigNoGitHub(t)
			cfg.GitHubPAT = GitHubPAT{Token: "ghp_patonly"}
			cfg.BaseImage = tt.image

			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.errContains)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestValidate_AllowedImagesDigestPin ensures every entry in the
// allowed_images allowlist is digest-pinned, not just base_image. A single
// tag-only entry must fail validation so H2's "allowlist matches strings
// not digests" gap stays closed.
func TestValidate_AllowedImagesDigestPin(t *testing.T) {
	validDigest := "contextmatrix/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	t.Run("all digest-pinned entries pass", func(t *testing.T) {
		cfg := baseValidConfigNoGitHub(t)
		cfg.GitHubPAT = GitHubPAT{Token: "ghp_patonly"}
		cfg.AllowedImages = []string{testDigestImage, validDigest}
		assert.NoError(t, cfg.Validate())
	})

	t.Run("one tag-only entry fails", func(t *testing.T) {
		cfg := baseValidConfigNoGitHub(t)
		cfg.GitHubPAT = GitHubPAT{Token: "ghp_patonly"}
		cfg.AllowedImages = []string{testDigestImage, "contextmatrix/worker:latest"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.ErrorContains(t, err, "allowed_images[1] must be @sha256:... pinned")
	})

	t.Run("empty list is accepted", func(t *testing.T) {
		cfg := baseValidConfigNoGitHub(t)
		cfg.GitHubPAT = GitHubPAT{Token: "ghp_patonly"}
		cfg.AllowedImages = nil
		assert.NoError(t, cfg.Validate())
	})
}
