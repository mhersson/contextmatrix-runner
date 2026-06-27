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
// Validate()'s digest-pinning check. Tests that do not exercise the pinning
// rule itself reuse this constant so the unrelated Validate paths stay
// readable.
const testDigestImage = "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestLogFormat_JSON_EmitsValidJSON verifies that configuring log_format: json
// produces parseable JSON log lines.
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
// value and that out-of-range or invalid values fail validation.
func TestAdminPort_DefaultsAnd_ValidationRange(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	claudeDir := dir

	// (a) Default should be 0 (disabled) when unset.
	path := writeConfig(t, dir, validConfig(pemPath, claudeDir))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.AdminPort)

	// (b) Explicit admin_port: 0 must pass Validate().
	yaml := validConfig(pemPath, claudeDir) + "\nadmin_port: 0\n"
	path = writeConfig(t, dir, yaml)
	cfg, err = Load(path)
	require.NoError(t, err)
	assert.NoError(t, cfg.Validate())

	// (c) Negative value must be rejected with an error mentioning "admin_port".
	yaml = validConfig(pemPath, claudeDir) + "\nadmin_port: -1\n"
	path = writeConfig(t, dir, yaml)
	_, err = Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin_port")

	// (d) Out-of-range value should fail.
	yaml = validConfig(pemPath, claudeDir) + "\nadmin_port: 70000\n"
	path = writeConfig(t, dir, yaml)
	_, err = Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin_port")
}

// TestAdminPort_RejectsCollisionWithPort verifies that admin_port == port is
// rejected at config-load time. Two HTTP listeners cannot share the same
// TCP port; without this guard the second ListenAndServe would fail with
// "address already in use" and the runner would crash via os.Exit(1) deep in
// startup instead of surfacing the typo before bind.
func TestAdminPort_RejectsCollisionWithPort(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	claudeDir := dir

	// Explicit collision: both ports set to 9090 (the default port).
	yaml := validConfig(pemPath, claudeDir) + "\nport: 9090\nadmin_port: 9090\n"
	path := writeConfig(t, dir, yaml)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin_port")
	assert.Contains(t, err.Error(), "port")

	// admin_port == 0 (disabled) must not trigger the collision check even
	// if port also happens to be 0 in a malformed Config{} literal.
	cfg := &Config{Port: 0, AdminPort: 0}
	ApplyDefaults(cfg)
	// After defaults Port=9090, AdminPort=0 — no collision.
	assert.NotEqual(t, cfg.Port, cfg.AdminPort)
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
github:
  auth_mode: "app"
  app:
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
	assert.Equal(t, "app", cfg.GitHub.AuthMode)
	assert.Equal(t, int64(12345), cfg.GitHub.App.AppID)
	assert.Equal(t, int64(67890), cfg.GitHub.App.InstallationID)
	assert.Equal(t, pemPath, cfg.GitHub.App.PrivateKeyPath)
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
	// Default auth mode for VerifyAutonomous is HMAC.
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

func TestValidate_ContainerContextMatrixURL_DefaultsToContextMatrixURL(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := &Config{
		ContextMatrixURL: "http://cm.lan:8080",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        testDigestImage,
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    1,
		ContainerTimeout: "1h",
		AnthropicAPIKey:  "sk-ant-test",
		GitHub: GitHubConfig{
			AuthMode: "app",
			App: GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
		},
	}
	require.NoError(t, cfg.Validate())
	assert.Equal(t, "http://cm.lan:8080", cfg.ContainerContextMatrixURL)
}

func TestValidate_ContainerContextMatrixURL_ExplicitValuePreserved(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := &Config{
		ContextMatrixURL:          "http://cm.lan:8080",
		ContainerContextMatrixURL: "http://host.docker.internal:8080",
		APIKey:                    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:                 testDigestImage,
		ImagePullPolicy:           PullAlways,
		MaxConcurrent:             1,
		ContainerTimeout:          "1h",
		AnthropicAPIKey:           "sk-ant-test",
		GitHub: GitHubConfig{
			AuthMode: "app",
			App: GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
		},
	}
	require.NoError(t, cfg.Validate())
	assert.Equal(t, "http://host.docker.internal:8080", cfg.ContainerContextMatrixURL)
}

func TestValidate_ServiceURLValidation(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr string
	}{
		{"valid https", "https://cm.example.com:8080", ""},
		{"valid http", "http://localhost:8080", ""},
		{"valid http no port", "http://cm.lan", ""},
		{"missing scheme", "cm.example.com:8080", "scheme must be http or https"},
		{"ftp scheme", "ftp://cm.example.com", "scheme must be http or https"},
		{"file scheme", "file:///etc/passwd", "scheme must be http or https"},
		{"empty host", "http://", "host is required"},
		{"unparseable", "://bad", "invalid URL"},
		{"embedded userinfo", "http://user:pass@cm.example.com", "must not embed userinfo credentials"},
		{"embedded user only", "http://user@cm.example.com", "must not embed userinfo credentials"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			pemPath := writePEM(t, dir)

			cfg := &Config{
				ContextMatrixURL: tt.url,
				APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				BaseImage:        testDigestImage,
				ImagePullPolicy:  PullAlways,
				MaxConcurrent:    1,
				ContainerTimeout: "1h",
				AnthropicAPIKey:  "sk-ant-test",
				GitHub: GitHubConfig{
					AuthMode: "app",
					App: GitHubAppConfig{
						AppID:          1,
						InstallationID: 1,
						PrivateKeyPath: pemPath,
					},
				},
			}

			err := cfg.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidate_ContainerContextMatrixURL_InvalidExplicitValue(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := &Config{
		ContextMatrixURL:          "http://cm.lan:8080",
		ContainerContextMatrixURL: "not-a-url",
		APIKey:                    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:                 testDigestImage,
		ImagePullPolicy:           PullAlways,
		MaxConcurrent:             1,
		ContainerTimeout:          "1h",
		AnthropicAPIKey:           "sk-ant-test",
		GitHub: GitHubConfig{
			AuthMode: "app",
			App: GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
		},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "container_contextmatrix_url")
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
		GitHub: GitHubConfig{
			AuthMode: "app",
			App: GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
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
		GitHub: GitHubConfig{
			AuthMode: "app",
			App: GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
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
		GitHub: GitHubConfig{
			AuthMode: "app",
			App: GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
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
			GitHub: GitHubConfig{
				AuthMode: "app",
				App: GitHubAppConfig{
					AppID:          1,
					InstallationID: 1,
					PrivateKeyPath: pemPath,
				},
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
github:
  auth_mode: "app"
  app:
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

	// auth_mode "app" with missing fields — each field required.
	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        testDigestImage,
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    1,
		ContainerTimeout: "1h",
		AnthropicAPIKey:  "sk-ant-test",
		GitHub:           GitHubConfig{AuthMode: "app"},
	}
	err := cfg.Validate()
	require.ErrorContains(t, err, "github.app.app_id is required")

	cfg.GitHub.App.AppID = 1
	err = cfg.Validate()
	require.ErrorContains(t, err, "github.app.installation_id is required")

	cfg.GitHub.App.InstallationID = 1
	err = cfg.Validate()
	require.ErrorContains(t, err, "github.app.private_key_path is required")

	cfg.GitHub.App.PrivateKeyPath = filepath.Join(dir, "nonexistent.pem")
	err = cfg.Validate()
	assert.ErrorContains(t, err, "github.app.private_key_path does not exist")
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
		GitHub: GitHubConfig{
			AuthMode: "app",
			App: GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
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
			GitHub: GitHubConfig{
				AuthMode: "app",
				App: GitHubAppConfig{
					AppID:          1,
					InstallationID: 1,
					PrivateKeyPath: pemPath,
				},
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
github:
  auth_mode: "app"
  api_base_url: "https://api.acme.ghe.com"
  app:
    app_id: 12345
    installation_id: 67890
    private_key_path: "` + pemPath + `"
`
	path := writeConfig(t, dir, content)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "https://api.acme.ghe.com", cfg.GitHub.APIBaseURL)
}

func TestLoad_GitHubApp_APIBaseURL_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	content := `
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
claude_auth_dir: "` + dir + `"
github:
  auth_mode: "app"
  api_base_url: "https://api.yaml.example"
  app:
    app_id: 12345
    installation_id: 67890
    private_key_path: "` + pemPath + `"
`
	path := writeConfig(t, dir, content)

	t.Setenv("CMR_GITHUB_API_BASE_URL", "https://api.env.example")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "https://api.env.example", cfg.GitHub.APIBaseURL)
}

func TestLoad_GitHubApp_APIBaseURL_Default(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	// No api_base_url in YAML, no env var set.
	path := writeConfig(t, dir, validConfig(pemPath, dir))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.GitHub.APIBaseURL)
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
		github      GitHubConfig
		wantErr     bool
		errContains string
	}{
		{
			name: "app-only configured is valid",
			github: GitHubConfig{
				AuthMode: "app",
				App: GitHubAppConfig{
					AppID:          1,
					InstallationID: 1,
					PrivateKeyPath: pemPath,
				},
			},
			wantErr: false,
		},
		{
			name: "pat-only configured is valid",
			github: GitHubConfig{
				AuthMode: "pat",
				PAT:      GitHubPATConfig{Token: "ghp_testtoken"},
			},
			wantErr: false,
		},
		{
			name: "app mode with pat.token set is an error",
			github: GitHubConfig{
				AuthMode: "app",
				App: GitHubAppConfig{
					AppID:          1,
					InstallationID: 1,
					PrivateKeyPath: pemPath,
				},
				PAT: GitHubPATConfig{Token: "ghp_testtoken"},
			},
			wantErr:     true,
			errContains: "github.pat.token must be empty when github.auth_mode is \"app\"",
		},
		{
			name:        "empty auth_mode is an error",
			github:      GitHubConfig{},
			wantErr:     true,
			errContains: "github.auth_mode is required",
		},
		{
			name: "pat mode with app fields set is an error",
			github: GitHubConfig{
				AuthMode: "pat",
				PAT:      GitHubPATConfig{Token: "ghp_patonly"},
				App:      GitHubAppConfig{AppID: 1},
			},
			wantErr:     true,
			errContains: "github.app.* must be empty when github.auth_mode is \"pat\"",
		},
		{
			name: "app mode missing installation_id",
			github: GitHubConfig{
				AuthMode: "app",
				App:      GitHubAppConfig{AppID: 1},
			},
			wantErr:     true,
			errContains: "github.app.installation_id is required",
		},
		{
			name: "app mode missing private_key_path",
			github: GitHubConfig{
				AuthMode: "app",
				App:      GitHubAppConfig{AppID: 1, InstallationID: 1},
			},
			wantErr:     true,
			errContains: "github.app.private_key_path is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseValidConfigNoGitHub(t)
			cfg.GitHub = tt.github

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
	// A Config literal that calls ApplyDefaults before Validate must
	// receive the documented defaults. Validate does not inject defaults
	// on its own; ApplyDefaults is the single source of truth.
	cfg := baseValidConfigNoGitHub(t)
	cfg.GitHub = GitHubConfig{AuthMode: "pat", PAT: GitHubPATConfig{Token: "ghp_patonly"}}

	ApplyDefaults(cfg)
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
			want: "webhook_replay_cache_size must not be negative",
		},
		{
			name: "negative skew seconds",
			mutate: func(c *Config) {
				c.WebhookReplaySkewSeconds = -1
			},
			want: "webhook_replay_skew_seconds must not be negative",
		},
		{
			name: "negative dedup cache size",
			mutate: func(c *Config) {
				c.MessageDedupCacheSize = -1
			},
			want: "message_dedup_cache_size must not be negative",
		},
		{
			name: "negative dedup ttl",
			mutate: func(c *Config) {
				c.MessageDedupTTLSeconds = -1
			},
			want: "message_dedup_ttl_seconds must not be negative",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValidConfigNoGitHub(t)
			cfg.GitHub = GitHubConfig{AuthMode: "pat", PAT: GitHubPATConfig{Token: "ghp_patonly"}}
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

	t.Setenv("CMR_GITHUB_AUTH_MODE", "pat")
	t.Setenv("CMR_GITHUB_PAT_TOKEN", "ghp_envtoken123")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "pat", cfg.GitHub.AuthMode)
	assert.Equal(t, "ghp_envtoken123", cfg.GitHub.PAT.Token)
}

func TestLoad_GitHubPAT_YAMLOverriddenByEnv(t *testing.T) {
	dir := t.TempDir()

	content := `
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
anthropic_api_key: "sk-ant-test"
github:
  auth_mode: "pat"
  pat:
    token: "ghp_fromyaml"
`
	path := writeConfig(t, dir, content)

	t.Setenv("CMR_GITHUB_PAT_TOKEN", "ghp_fromenv")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "ghp_fromenv", cfg.GitHub.PAT.Token)
}

// TestValidate_BaseImageDigestPin covers the requirement that base_image be
// an @sha256:... reference. Mutable tags and malformed digests must fail
// validation so a rebuilt upstream image can never silently ship.
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
			cfg.GitHub = GitHubConfig{AuthMode: "pat", PAT: GitHubPATConfig{Token: "ghp_patonly"}}
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

// TestDeploymentProfile covers the deployment_profile field: defaults, accepted
// values, rejection of unknown values, env override, and the IsDev() helper.
func TestDeploymentProfile(t *testing.T) {
	t.Run("default is production when unset in YAML", func(t *testing.T) {
		dir := t.TempDir()
		pemPath := writePEM(t, dir)

		path := writeConfig(t, dir, validConfig(pemPath, dir))
		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, ProfileProduction, cfg.DeploymentProfile)
	})

	t.Run("explicit production is accepted", func(t *testing.T) {
		dir := t.TempDir()
		pemPath := writePEM(t, dir)

		content := validConfig(pemPath, dir) + "\ndeployment_profile: production\n"
		path := writeConfig(t, dir, content)
		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, ProfileProduction, cfg.DeploymentProfile)
		assert.False(t, cfg.IsDev())
	})

	t.Run("explicit dev is accepted and IsDev returns true", func(t *testing.T) {
		dir := t.TempDir()
		pemPath := writePEM(t, dir)

		content := validConfig(pemPath, dir) + "\ndeployment_profile: dev\n"
		path := writeConfig(t, dir, content)
		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, ProfileDev, cfg.DeploymentProfile)
		assert.True(t, cfg.IsDev())
	})

	t.Run("unknown value staging is rejected", func(t *testing.T) {
		dir := t.TempDir()
		pemPath := writePEM(t, dir)

		content := validConfig(pemPath, dir) + "\ndeployment_profile: staging\n"
		path := writeConfig(t, dir, content)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "deployment_profile must be one of: production, dev")
	})

	t.Run("env override CMR_DEPLOYMENT_PROFILE=dev takes precedence over YAML production", func(t *testing.T) {
		dir := t.TempDir()
		pemPath := writePEM(t, dir)

		content := validConfig(pemPath, dir) + "\ndeployment_profile: production\n"
		path := writeConfig(t, dir, content)

		t.Setenv("CMR_DEPLOYMENT_PROFILE", "dev")

		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, ProfileDev, cfg.DeploymentProfile)
		assert.True(t, cfg.IsDev())
	})
}

// TestIsDev verifies that IsDev returns true only for the "dev" profile.
func TestIsDev(t *testing.T) {
	tests := []struct {
		profile string
		want    bool
	}{
		{profile: "", want: false},
		{profile: ProfileProduction, want: false},
		{profile: ProfileDev, want: true},
	}

	for _, tt := range tests {
		t.Run("profile="+tt.profile, func(t *testing.T) {
			cfg := &Config{DeploymentProfile: tt.profile}
			assert.Equal(t, tt.want, cfg.IsDev())
		})
	}
}

// baseDevConfig returns a minimal valid config in dev mode without GitHub auth.
// Tests that exercise dev-mode digest-pin relaxation set their own GitHub auth.
func baseDevConfig(t *testing.T, pemPath string) *Config {
	t.Helper()

	return &Config{
		ContextMatrixURL:  "http://localhost",
		APIKey:            "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:         testDigestImage,
		ImagePullPolicy:   PullAlways,
		MaxConcurrent:     1,
		ContainerTimeout:  "1h",
		AnthropicAPIKey:   "sk-ant-test",
		DeploymentProfile: ProfileDev,
		GitHub: GitHubConfig{
			AuthMode: "app",
			App: GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
		},
	}
}

// TestValidate_DevMode_UnpinnedBaseImage verifies that in dev mode an unpinned
// base_image does not cause Validate to return an error, and the reference is
// collected in UnpinnedImageRefs.
func TestValidate_DevMode_UnpinnedBaseImage(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := baseDevConfig(t, pemPath)
	cfg.BaseImage = "contextmatrix/worker:latest"

	err := cfg.Validate()
	require.NoError(t, err)

	require.Len(t, cfg.UnpinnedImageRefs, 1)
	assert.Equal(t, "base_image", cfg.UnpinnedImageRefs[0].Field)
	assert.Equal(t, "contextmatrix/worker:latest", cfg.UnpinnedImageRefs[0].Image)
}

// TestValidate_DevMode_MultipleUnpinnedAllowedImages verifies that all unpinned
// allowed_images entries are collected in dev mode with their indexed field names.
func TestValidate_DevMode_MultipleUnpinnedAllowedImages(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := baseDevConfig(t, pemPath)
	cfg.AllowedImages = []string{
		"contextmatrix/worker:v1",
		"contextmatrix/worker:v2",
	}

	err := cfg.Validate()
	require.NoError(t, err)

	require.Len(t, cfg.UnpinnedImageRefs, 2)
	assert.Equal(t, "allowed_images[0]", cfg.UnpinnedImageRefs[0].Field)
	assert.Equal(t, "contextmatrix/worker:v1", cfg.UnpinnedImageRefs[0].Image)
	assert.Equal(t, "allowed_images[1]", cfg.UnpinnedImageRefs[1].Field)
	assert.Equal(t, "contextmatrix/worker:v2", cfg.UnpinnedImageRefs[1].Image)
}

// TestValidate_Production_UnpinnedBaseImageFails verifies that production mode
// keeps the existing fail-closed behaviour for unpinned base_image.
func TestValidate_Production_UnpinnedBaseImageFails(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := baseValidConfigNoGitHub(t)
	cfg.GitHub = GitHubConfig{
		AuthMode: "app",
		App: GitHubAppConfig{
			AppID:          1,
			InstallationID: 1,
			PrivateKeyPath: pemPath,
		},
	}
	cfg.BaseImage = "contextmatrix/worker:latest"
	// DeploymentProfile is zero-value ("") which Validate normalises to production.

	err := cfg.Validate()
	require.ErrorContains(t, err, "base_image must be @sha256:... pinned")
	assert.Nil(t, cfg.UnpinnedImageRefs)
}

// TestValidate_DevMode_FullyPinned verifies that when all images are digest-pinned
// in dev mode, UnpinnedImageRefs is empty (no spurious WARNs on startup).
func TestValidate_DevMode_FullyPinned(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	pinnedA := "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinnedB := "contextmatrix/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	cfg := baseDevConfig(t, pemPath)
	cfg.BaseImage = pinnedA
	cfg.AllowedImages = []string{pinnedB}

	err := cfg.Validate()
	require.NoError(t, err)
	assert.Empty(t, cfg.UnpinnedImageRefs)
}

// TestValidate_DevMode_MixedPinning verifies that only unpinned entries appear
// in UnpinnedImageRefs when some allowed_images are pinned and some are not.
func TestValidate_DevMode_MixedPinning(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	pinnedB := "contextmatrix/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	cfg := baseDevConfig(t, pemPath)
	cfg.AllowedImages = []string{
		pinnedB,
		"contextmatrix/worker:unpinned",
	}

	err := cfg.Validate()
	require.NoError(t, err)

	require.Len(t, cfg.UnpinnedImageRefs, 1)
	assert.Equal(t, "allowed_images[1]", cfg.UnpinnedImageRefs[0].Field)
	assert.Equal(t, "contextmatrix/worker:unpinned", cfg.UnpinnedImageRefs[0].Image)
}

// TestValidate_AllowedImagesDigestPin ensures every entry in the
// allowed_images allowlist is digest-pinned, not just base_image. A single
// tag-only entry must fail validation so the "allowlist matches strings
// not digests" gap stays closed.
func TestValidate_AllowedImagesDigestPin(t *testing.T) {
	validDigest := "contextmatrix/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	t.Run("all digest-pinned entries pass", func(t *testing.T) {
		cfg := baseValidConfigNoGitHub(t)
		cfg.GitHub = GitHubConfig{AuthMode: "pat", PAT: GitHubPATConfig{Token: "ghp_patonly"}}
		cfg.AllowedImages = []string{testDigestImage, validDigest}
		assert.NoError(t, cfg.Validate())
	})

	t.Run("one tag-only entry fails", func(t *testing.T) {
		cfg := baseValidConfigNoGitHub(t)
		cfg.GitHub = GitHubConfig{AuthMode: "pat", PAT: GitHubPATConfig{Token: "ghp_patonly"}}
		cfg.AllowedImages = []string{testDigestImage, "contextmatrix/worker:latest"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.ErrorContains(t, err, "allowed_images[1] must be @sha256:... pinned")
	})

	t.Run("empty list is accepted", func(t *testing.T) {
		cfg := baseValidConfigNoGitHub(t)
		cfg.GitHub = GitHubConfig{AuthMode: "pat", PAT: GitHubPATConfig{Token: "ghp_patonly"}}
		cfg.AllowedImages = nil
		assert.NoError(t, cfg.Validate())
	})
}

// validConfigDev returns a YAML string for a dev-profile config that is
// otherwise identical to validConfig. Tests that need the dev profile use
// this instead of setting the env var after the fact.
func validConfigDev(pemPath, claudeDir string) string {
	return validConfig(pemPath, claudeDir) + "\ndeployment_profile: dev\n"
}

// TestLoad_DevDefaults_UnsetValues verifies that a dev-profile config
// with no explicit skew or pull policy receives the dev defaults and that
// AppliedDevDefaults records them both.
func TestLoad_DevDefaults_UnsetValues(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	path := writeConfig(t, dir, validConfigDev(pemPath, dir))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 86400, cfg.WebhookReplaySkewSeconds, "dev mode: skew should default to 86400")
	assert.Equal(t, PullIfNotPresent, cfg.ImagePullPolicy, "dev mode: pull policy should default to if-not-present")
	assert.ElementsMatch(t, []string{"webhook_replay_skew_seconds=86400", "image_pull_policy=if-not-present"}, cfg.AppliedDevDefaults)
}

// TestLoad_DevDefaults_ExplicitSkew verifies that an explicitly-set
// webhook_replay_skew_seconds is NOT overridden in dev mode and does NOT
// appear in AppliedDevDefaults.
func TestLoad_DevDefaults_ExplicitSkew(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	yaml := validConfigDev(pemPath, dir) + "webhook_replay_skew_seconds: 60\n"
	path := writeConfig(t, dir, yaml)

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 60, cfg.WebhookReplaySkewSeconds, "explicit skew must not be overridden in dev mode")
	assert.NotContains(t, cfg.AppliedDevDefaults, "webhook_replay_skew_seconds=86400")
}

// TestLoad_DevDefaults_ExplicitPullPolicy verifies that an explicitly-set
// image_pull_policy is NOT overridden in dev mode. Covers both "always" (trivially
// distinct from the dev default) and "never" (shares a value with the
// production default, a regression-prone overlap).
func TestLoad_DevDefaults_ExplicitPullPolicy(t *testing.T) {
	cases := []struct {
		name   string
		policy string
	}{
		{name: "always", policy: PullAlways},
		{name: "never", policy: PullNever},
		{name: "if-not-present", policy: PullIfNotPresent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pemPath := writePEM(t, dir)

			yaml := validConfigDev(pemPath, dir) + "image_pull_policy: " + tc.policy + "\n"
			path := writeConfig(t, dir, yaml)

			cfg, err := Load(path)
			require.NoError(t, err)

			assert.Equal(t, tc.policy, cfg.ImagePullPolicy,
				"explicit pull policy %q must not be overridden in dev mode", tc.policy)
			assert.NotContains(t, cfg.AppliedDevDefaults, "image_pull_policy=if-not-present",
				"explicit pull policy must not appear in AppliedDevDefaults")
		})
	}
}

// TestLoad_ProductionDefaults_UnsetValues verifies that the production profile
// (the default) yields 330 for skew and "never" for pull policy, and that
// AppliedDevDefaults is empty.
func TestLoad_ProductionDefaults_UnsetValues(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	path := writeConfig(t, dir, validConfig(pemPath, dir))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 330, cfg.WebhookReplaySkewSeconds, "production: skew must default to 330")
	assert.Equal(t, PullNever, cfg.ImagePullPolicy, "production: pull policy must default to never")
	assert.Empty(t, cfg.AppliedDevDefaults, "production mode must not populate AppliedDevDefaults")
}

func TestConfig_TaskSkillsDir(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
claude_auth_dir: "`+dir+`"
github:
  auth_mode: "app"
  app:
    app_id: 12345
    installation_id: 67890
    private_key_path: "`+pemPath+`"
task_skills_dir: /var/lib/contextmatrix/task-skills
`), 0o600))

	cfg, err := Load(configPath)
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/contextmatrix/task-skills", cfg.TaskSkillsDir)
}

func TestConfig_TaskSkillsDirDefault(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
claude_auth_dir: "`+dir+`"
github:
  auth_mode: "app"
  app:
    app_id: 12345
    installation_id: 67890
    private_key_path: "`+pemPath+`"
`), 0o600))

	cfg, err := Load(configPath)
	require.NoError(t, err)
	assert.Empty(t, cfg.TaskSkillsDir,
		"unset task_skills_dir is allowed; feature simply disabled")
}

// writeConfigFile is an alias for writeConfig used by the new unified-github tests.
func writeConfigFile(t *testing.T, dir, content string) string {
	t.Helper()

	return writeConfig(t, dir, content)
}

// minimalValidRunnerConfig returns a minimal valid Config without GitHub auth so
// tests can set the GitHub block themselves.
func minimalValidRunnerConfig(t *testing.T) *Config {
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

func TestLoad_GitHubAuthModeApp(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	yamlContent := `
github:
  auth_mode: "app"
  app:
    app_id: 123
    installation_id: 456
    private_key_path: ` + pemPath + `
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
anthropic_api_key: "sk-ant-test"
secrets_dir: ` + dir + `
`
	path := writeConfigFile(t, dir, yamlContent)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "app", cfg.GitHub.AuthMode)
	assert.Equal(t, int64(123), cfg.GitHub.App.AppID)
	assert.Equal(t, int64(456), cfg.GitHub.App.InstallationID)
	assert.Equal(t, pemPath, cfg.GitHub.App.PrivateKeyPath)
}

func TestLoad_GitHubAuthModePAT(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
github:
  auth_mode: "pat"
  pat:
    token: "ghp_runner"
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
anthropic_api_key: "sk-ant-test"
secrets_dir: ` + dir + `
`
	path := writeConfigFile(t, dir, yamlContent)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "pat", cfg.GitHub.AuthMode)
	assert.Equal(t, "ghp_runner", cfg.GitHub.PAT.Token)
}

func TestEnvOverrides_GitHubAuthMode(t *testing.T) {
	t.Setenv("CMR_GITHUB_AUTH_MODE", "app")
	t.Setenv("CMR_GITHUB_APP_ID", "999")
	t.Setenv("CMR_GITHUB_INSTALLATION_ID", "888")

	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	t.Setenv("CMR_GITHUB_PRIVATE_KEY_PATH", pemPath)
	t.Setenv("CMR_ANTHROPIC_API_KEY", "sk-ant-test")

	path := writeConfigFile(t, dir, `
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
base_image: "contextmatrix/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
anthropic_api_key: "sk-ant-test"
secrets_dir: `+dir)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "app", cfg.GitHub.AuthMode)
	assert.Equal(t, int64(999), cfg.GitHub.App.AppID)
	assert.Equal(t, int64(888), cfg.GitHub.App.InstallationID)
}

func TestValidate_AuthModeRequired(t *testing.T) {
	cfg := minimalValidRunnerConfig(t)
	cfg.GitHub.AuthMode = ""
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github.auth_mode")
}

// TestValidate_WorkerExtraEnv covers the new WorkerExtraEnv field's
// validation rules: invalid env-var keys are rejected, secrets-file
// var collisions are rejected, well-formed entries pass.
func TestValidate_WorkerExtraEnv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	baseCfg := func() *Config {
		return &Config{
			ContextMatrixURL: "http://cm.lan:8080",
			APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			BaseImage:        testDigestImage,
			ImagePullPolicy:  PullAlways,
			MaxConcurrent:    1,
			ContainerTimeout: "1h",
			AnthropicAPIKey:  "sk-ant-test",
			GitHub: GitHubConfig{
				AuthMode: "app",
				App: GitHubAppConfig{
					AppID:          1,
					InstallationID: 1,
					PrivateKeyPath: pemPath,
				},
			},
		}
	}

	t.Run("valid keys accepted", func(t *testing.T) {
		t.Parallel()

		cfg := baseCfg()
		cfg.WorkerExtraEnv = map[string]string{
			"NPM_CONFIG_REGISTRY": "https://npm.internal/",
			"_LEADING_UNDERSCORE": "ok",
			"WITH_DIGITS_123":     "v",
		}
		require.NoError(t, cfg.Validate())
	})

	t.Run("invalid key shape rejected", func(t *testing.T) {
		t.Parallel()

		cases := []string{
			"",          // empty
			"1STARTS",   // leading digit
			"WITH-DASH", // hyphen
			"WITH SPACE",
			"WITH=EQ",
		}
		for _, k := range cases {
			cfg := baseCfg()
			cfg.WorkerExtraEnv = map[string]string{k: "v"}

			err := cfg.Validate()
			assert.ErrorContains(t, err, "worker_extra_env",
				"key %q should be rejected", k)
		}
	})

	t.Run("secrets-file collisions rejected", func(t *testing.T) {
		t.Parallel()

		secrets := []string{
			"CM_GIT_TOKEN",
			"CLAUDE_CODE_OAUTH_TOKEN",
			"ANTHROPIC_API_KEY",
		}
		for _, k := range secrets {
			cfg := baseCfg()
			cfg.WorkerExtraEnv = map[string]string{k: "leak"}

			err := cfg.Validate()
			assert.ErrorContains(t, err, "collides with a secrets-file var",
				"key %q should collide with secrets file", k)
		}
	})

	t.Run("per-container env collision rejected", func(t *testing.T) {
		t.Parallel()

		cfg := baseCfg()
		cfg.WorkerExtraEnv = map[string]string{"CM_MCP_API_KEY": "leak"}

		err := cfg.Validate()
		assert.ErrorContains(t, err, "collides with a per-container env var",
			"key CM_MCP_API_KEY should collide with per-container env var")
	})
}

// minimalValidConfig returns a fully-populated minimal Config that passes
// Validate(). Tests that exercise a single field mutate the returned value.
func minimalValidConfig(t *testing.T) *Config {
	t.Helper()

	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	return &Config{
		ContextMatrixURL: "http://cm.lan:8080",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        testDigestImage,
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    1,
		ContainerTimeout: "1h",
		AnthropicAPIKey:  "sk-ant-test",
		GitHub: GitHubConfig{
			AuthMode: "app",
			App: GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
		},
	}
}

func TestValidate_WorkerExtraEnv_RejectsDangerousKeys(t *testing.T) {
	dangerous := []string{
		"GIT_SSL_NO_VERIFY",
		"GIT_TERMINAL_PROMPT",
		"LD_PRELOAD",
		"LD_LIBRARY_PATH",
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"NO_PROXY",
		"NODE_OPTIONS",
		"NODE_EXTRA_CA_CERTS",
		"PATH",
		"GOPROXY",
		"GOSUMDB",
		"GOFLAGS",
		"PYTHONPATH",
	}

	for _, key := range dangerous {
		t.Run(key, func(t *testing.T) {
			c := minimalValidConfig(t)
			c.WorkerExtraEnv = map[string]string{key: "1"}

			err := c.Validate()
			require.Error(t, err, "Validate must reject worker_extra_env[%q]", key)
			require.Contains(t, err.Error(), "worker_extra_env")
		})
	}
}

func TestValidate_WorkerExtraEnv_AllowsBenignKeys(t *testing.T) {
	c := minimalValidConfig(t)
	c.WorkerExtraEnv = map[string]string{
		"MY_APP_FEATURE_FLAG": "on",
		"CI":                  "false",
	}
	require.NoError(t, c.Validate())
}

func TestValidate_CACertFile(t *testing.T) {
	t.Run("empty is allowed", func(t *testing.T) {
		c := minimalValidConfig(t)
		c.CACertFile = ""
		require.NoError(t, c.Validate())
	})

	t.Run("existing file is allowed", func(t *testing.T) {
		c := minimalValidConfig(t)
		f := filepath.Join(t.TempDir(), "ca.pem")
		require.NoError(t, os.WriteFile(f, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600))
		c.CACertFile = f
		require.NoError(t, c.Validate())
	})

	t.Run("missing file is rejected", func(t *testing.T) {
		c := minimalValidConfig(t)
		c.CACertFile = filepath.Join(t.TempDir(), "does-not-exist.pem")
		err := c.Validate()
		require.Error(t, err)
		require.ErrorContains(t, err, "ca_cert_file")
	})
}

// TestLoad_UnknownYAMLField verifies that yaml.NewDecoder().KnownFields(true)
// surfaces typos in security-relevant fields instead of silently accepting
// them. A misspelled `webhook_replay_skew_secods` (without the "n") would
// quietly disable replay protection without KnownFields.
func TestLoad_UnknownYAMLField(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	yaml := validConfig(pemPath, dir) + "\nwebhook_replay_skew_secods: 60\n"
	path := writeConfig(t, dir, yaml)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "webhook_replay_skew_secods")
}

// TestLoad_EnvParseError_Surfaces verifies that a malformed integer env var
// causes Load to fail loudly instead of silently falling back to the YAML
// default. Operators typo CMR_PORT=70x0 once and lose ten hours debugging
// without this check.
func TestLoad_EnvParseError_Surfaces(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	path := writeConfig(t, dir, validConfig(pemPath, dir))

	t.Setenv("CMR_PORT", "not-a-number")

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CMR_PORT")
}

// TestLoad_EnvOverrides_ExtraFields covers the CMR_* overrides. Each override
// is exercised through Load() so the env wiring stays
// in lockstep with the documented YAML fields.
func TestLoad_EnvOverrides_ExtraFields(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	path := writeConfig(t, dir, validConfig(pemPath, dir))

	t.Setenv("CMR_WEBHOOK_REPLAY_CACHE_SIZE", "999")
	t.Setenv("CMR_WEBHOOK_REPLAY_SKEW_SECONDS", "111")
	t.Setenv("CMR_MESSAGE_DEDUP_CACHE_SIZE", "888")
	t.Setenv("CMR_MESSAGE_DEDUP_TTL_SECONDS", "222")
	t.Setenv("CMR_IDLE_OUTPUT_TIMEOUT", "5m")
	t.Setenv("CMR_IDLE_WATCHDOG_INTERVAL", "10s")
	t.Setenv("CMR_MAINTENANCE_INTERVAL", "20m")
	t.Setenv("CMR_USE_HMAC_FOR_VERIFY_AUTONOMOUS", "false")
	t.Setenv("CMR_CONTAINER_MEMORY_LIMIT", "12345")
	t.Setenv("CMR_CONTAINER_PIDS_LIMIT", "256")
	t.Setenv("CMR_TASK_SKILLS_DIR", "/srv/skills")
	t.Setenv("CMR_WORKER_EXTRA_ENV", "MY_FLAG=on,CI=true")

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 999, cfg.WebhookReplayCacheSize)
	assert.Equal(t, 111, cfg.WebhookReplaySkewSeconds)
	assert.Equal(t, 888, cfg.MessageDedupCacheSize)
	assert.Equal(t, 222, cfg.MessageDedupTTLSeconds)
	assert.Equal(t, 5*time.Minute, cfg.IdleOutputTimeout)
	assert.Equal(t, 10*time.Second, cfg.IdleWatchdogInterval)
	assert.Equal(t, 20*time.Minute, cfg.MaintenanceInterval)
	assert.False(t, cfg.UseHMACForVerifyAutonomous)
	assert.Equal(t, int64(12345), cfg.ContainerMemoryLimit)
	assert.Equal(t, int64(256), cfg.ContainerPidsLimit)
	assert.Equal(t, "/srv/skills", cfg.TaskSkillsDir)
	assert.Equal(t, "on", cfg.WorkerExtraEnv["MY_FLAG"])
	assert.Equal(t, "true", cfg.WorkerExtraEnv["CI"])
}

// TestLoad_EnvOverrides_WorkerExtraEnv_MalformedEntry asserts that a
// missing '=' in CMR_WORKER_EXTRA_ENV surfaces as an error rather than
// being silently ignored.
func TestLoad_EnvOverrides_WorkerExtraEnv_MalformedEntry(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	path := writeConfig(t, dir, validConfig(pemPath, dir))

	t.Setenv("CMR_WORKER_EXTRA_ENV", "NO_EQUALS_HERE")

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CMR_WORKER_EXTRA_ENV")
}

// TestValidate_GitHubURLValidation verifies Fix 6: GitHub.APIBaseURL and
// GitHub.Host are URL-validated when set. Malformed overrides flow into
// the shared githubauth module today and only surface as opaque connect
// errors at runtime; we want them to fail Validate() instead.
func TestValidate_GitHubURLValidation(t *testing.T) {
	tests := []struct {
		name       string
		apiBaseURL string
		host       string
		wantErr    string
	}{
		{
			name:       "valid api_base_url is accepted",
			apiBaseURL: "https://api.github.example",
		},
		{
			name:       "ftp scheme rejected on api_base_url",
			apiBaseURL: "ftp://api.github.example",
			wantErr:    "github.api_base_url",
		},
		{
			name:       "empty host on api_base_url rejected",
			apiBaseURL: "https://",
			wantErr:    "github.api_base_url",
		},
		{
			name: "bare hostname on github.host accepted",
			host: "ghe.example.com",
		},
		{
			name: "url with scheme on github.host accepted",
			host: "https://ghe.example.com",
		},
		{
			name:    "ftp scheme on github.host rejected",
			host:    "ftp://ghe.example.com",
			wantErr: "github.host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minimalValidConfig(t)
			cfg.GitHub.APIBaseURL = tt.apiBaseURL
			cfg.GitHub.Host = tt.host

			err := cfg.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

// TestValidate_ContainerLimitsRejectNegative covers Fix 10: negative
// container_memory_limit / container_pids_limit values must fail Validate
// instead of being passed through to Docker.
func TestValidate_ContainerLimitsRejectNegative(t *testing.T) {
	t.Run("negative memory limit rejected", func(t *testing.T) {
		cfg := minimalValidConfig(t)
		cfg.ContainerMemoryLimit = -1
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "container_memory_limit")
	})

	t.Run("negative pids limit rejected", func(t *testing.T) {
		cfg := minimalValidConfig(t)
		cfg.ContainerPidsLimit = -1
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "container_pids_limit")
	})
}

// TestApplyDefaults_FillsZeroFields exercises ApplyDefaults directly so the
// behaviour is pinned for test-only callers that build Config{} literals.
func TestApplyDefaults_FillsZeroFields(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	assert.Equal(t, 9090, cfg.Port)
	assert.Equal(t, 3, cfg.MaxConcurrent)
	assert.Equal(t, "2h", cfg.ContainerTimeout)
	assert.Equal(t, 30*time.Minute, cfg.IdleOutputTimeout)
	assert.Equal(t, 30*time.Second, cfg.IdleWatchdogInterval)
	assert.Equal(t, 10*time.Minute, cfg.MaintenanceInterval)
	assert.Equal(t, 10000, cfg.WebhookReplayCacheSize)
	assert.Equal(t, 330, cfg.WebhookReplaySkewSeconds)
	assert.Equal(t, ProfileProduction, cfg.DeploymentProfile)
}

// TestApplyDefaults_PreservesExplicitValues verifies that fields already set
// in the input Config are not overwritten by ApplyDefaults.
func TestApplyDefaults_PreservesExplicitValues(t *testing.T) {
	cfg := &Config{
		Port:                   1234,
		MaxConcurrent:          7,
		ContainerTimeout:       "5h",
		WebhookReplayCacheSize: 555,
		IdleOutputTimeout:      99 * time.Second,
	}
	ApplyDefaults(cfg)

	assert.Equal(t, 1234, cfg.Port)
	assert.Equal(t, 7, cfg.MaxConcurrent)
	assert.Equal(t, "5h", cfg.ContainerTimeout)
	assert.Equal(t, 555, cfg.WebhookReplayCacheSize)
	assert.Equal(t, 99*time.Second, cfg.IdleOutputTimeout)
}

// TestLoad_IdleOutputTimeout_ExplicitZeroDisables verifies that
// `idle_output_timeout: 0s` in YAML survives Load() without being clobbered
// by ApplyDefaults' default of 30m. The "zero disables the watchdog"
// contract is load-bearing for operators that explicitly want long-running
// containers to never be killed by the idle watchdog.
func TestLoad_IdleOutputTimeout_ExplicitZeroDisables(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	yaml := validConfig(pemPath, dir) + "\nidle_output_timeout: 0s\n"
	path := writeConfig(t, dir, yaml)

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, time.Duration(0), cfg.IdleOutputTimeout,
		"explicit `idle_output_timeout: 0s` must survive Load and disable the watchdog")
}

// TestLoad_IdleOutputTimeout_AbsentKeyApplyDefault verifies that an absent
// idle_output_timeout in YAML still receives the documented default. This is
// the counterpart to TestLoad_IdleOutputTimeout_ExplicitZeroDisables: only
// the explicit zero case must survive Load.
func TestLoad_IdleOutputTimeout_AbsentKeyAppliesDefault(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	// validConfig() does not set idle_output_timeout.
	path := writeConfig(t, dir, validConfig(pemPath, dir))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 30*time.Minute, cfg.IdleOutputTimeout,
		"absent idle_output_timeout must default to 30m")
}

// TestLoad_MaintenanceInterval_ExplicitZeroDisables verifies the parallel
// behaviour for maintenance_interval. The runMaintenanceLoop function in
// main.go treats non-positive intervals as a no-op, so an explicit 0s in
// YAML must propagate through Load instead of being silently replaced with
// the 10-minute default.
func TestLoad_MaintenanceInterval_ExplicitZeroDisables(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	yaml := validConfig(pemPath, dir) + "\nmaintenance_interval: 0s\n"
	path := writeConfig(t, dir, yaml)

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, time.Duration(0), cfg.MaintenanceInterval,
		"explicit `maintenance_interval: 0s` must survive Load and disable the loop")
}

// TestLoad_MaintenanceInterval_AbsentKeyAppliesDefault verifies that an
// absent maintenance_interval gets the documented 10m default.
func TestLoad_MaintenanceInterval_AbsentKeyAppliesDefault(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	path := writeConfig(t, dir, validConfig(pemPath, dir))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 10*time.Minute, cfg.MaintenanceInterval,
		"absent maintenance_interval must default to 10m")
}

// TestLoad_IdleOutputTimeout_EnvOverrideZeroDisables verifies that an
// operator setting CMR_IDLE_OUTPUT_TIMEOUT=0s also disables the watchdog,
// even when the YAML has the default 30m value. This matches the
// "explicit zero disables" contract regardless of whether the explicit
// value came from YAML or the environment.
func TestLoad_IdleOutputTimeout_EnvOverrideZeroDisables(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	// YAML sets a positive value; env should override to 0s.
	yaml := validConfig(pemPath, dir) + "\nidle_output_timeout: 30m\n"
	path := writeConfig(t, dir, yaml)

	t.Setenv("CMR_IDLE_OUTPUT_TIMEOUT", "0s")

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, time.Duration(0), cfg.IdleOutputTimeout,
		"explicit CMR_IDLE_OUTPUT_TIMEOUT=0s must disable the watchdog")
}

// TestLoad_MaintenanceInterval_EnvOverrideZeroDisables is the counterpart
// for the maintenance loop.
func TestLoad_MaintenanceInterval_EnvOverrideZeroDisables(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	yaml := validConfig(pemPath, dir) + "\nmaintenance_interval: 10m\n"
	path := writeConfig(t, dir, yaml)

	t.Setenv("CMR_MAINTENANCE_INTERVAL", "0s")

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, time.Duration(0), cfg.MaintenanceInterval,
		"explicit CMR_MAINTENANCE_INTERVAL=0s must disable the loop")
}

// TestLoad_IdleWatchdogInterval_ExplicitZeroPreserved verifies that an
// explicit `idle_watchdog_interval: 0s` in YAML survives Load and is not
// silently replaced with the documented 30s default. Mirrors the
// idle_output_timeout / maintenance_interval semantics so all three knobs
// behave consistently.
func TestLoad_IdleWatchdogInterval_ExplicitZeroPreserved(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	yaml := validConfig(pemPath, dir) + "\nidle_watchdog_interval: 0s\n"
	path := writeConfig(t, dir, yaml)

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, time.Duration(0), cfg.IdleWatchdogInterval,
		"explicit `idle_watchdog_interval: 0s` must survive Load (mirrors idle_output_timeout / maintenance_interval)")
}

// TestLoad_IdleWatchdogInterval_AbsentKeyAppliesDefault verifies that an
// absent key still gets the 30s default. The explicit-zero handling must
// not regress the default-injection path.
func TestLoad_IdleWatchdogInterval_AbsentKeyAppliesDefault(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	path := writeConfig(t, dir, validConfig(pemPath, dir))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 30*time.Second, cfg.IdleWatchdogInterval,
		"absent idle_watchdog_interval must default to 30s")
}

// TestLoad_IdleWatchdogInterval_EnvOverrideZeroPreserved checks the env
// override path: CMR_IDLE_WATCHDOG_INTERVAL=0s must disable the watchdog
// even when YAML carries a positive value.
func TestLoad_IdleWatchdogInterval_EnvOverrideZeroPreserved(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	yaml := validConfig(pemPath, dir) + "\nidle_watchdog_interval: 30s\n"
	path := writeConfig(t, dir, yaml)

	t.Setenv("CMR_IDLE_WATCHDOG_INTERVAL", "0s")

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, time.Duration(0), cfg.IdleWatchdogInterval,
		"explicit CMR_IDLE_WATCHDOG_INTERVAL=0s must preserve zero")
}

// TestValidate_Port_RangeCheck verifies that out-of-range port values are
// rejected at Validate time. Zero is tolerated (the same "zero means apply
// default" convention used by the replay-cache tunables).
func TestValidate_Port_RangeCheck(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	tests := []struct {
		name    string
		port    int
		wantErr bool
	}{
		{"valid", 9090, false},
		{"min in range", 1, false},
		{"max in range", 65535, false},
		{"zero accepted (apply default)", 0, false},
		{"negative rejected", -1, true},
		{"above 65535 rejected", 70000, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseValidConfigNoGitHub(t)
			cfg.GitHub = GitHubConfig{
				AuthMode: "app",
				App: GitHubAppConfig{
					AppID:          1,
					InstallationID: 1,
					PrivateKeyPath: pemPath,
				},
			}
			cfg.Port = tt.port

			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorContains(t, err, "port")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestLoad_Port_EnvOverrideRejectsOutOfRange verifies that an out-of-range
// env-supplied port reaches Validate (i.e. the override is wired) and
// is rejected. Surfaces operator typos at config-load time.
func TestLoad_Port_EnvOverrideRejectsOutOfRange(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	path := writeConfig(t, dir, validConfig(pemPath, dir))

	t.Setenv("CMR_PORT", "70000")

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "port")
}

// TestValidate_APIKey_CharacterClass verifies that APIKey values containing
// non-printable / non-ASCII characters are rejected. The deprecated
// VerifyAutonomous Bearer fallback ships the key in an Authorization header,
// so a stray CR/LF or control byte must surface as a config error, not as a
// silent header-injection vector.
func TestValidate_APIKey_CharacterClass(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	base := func() *Config {
		cfg := baseValidConfigNoGitHub(t)
		cfg.GitHub = GitHubConfig{
			AuthMode: "app",
			App: GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
		}

		return cfg
	}

	tests := []struct {
		name    string
		apiKey  string
		wantErr bool
	}{
		{"all ASCII printable accepted", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		{"contains CR rejected", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\r", true},
		{"contains LF rejected", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", true},
		{"contains tab rejected", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t", true},
		{"contains DEL rejected", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x7f", true},
		{"contains space rejected", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ", true},
		{"contains non-ASCII rejected", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaá", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			cfg.APIKey = tt.apiKey

			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "api_key")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestValidate_NumericCeilings verifies that the upper bounds on each
// numeric tunable surface as Validate errors. A typo like
// `max_concurrent: 1000000` should be caught at config-load time instead of
// OOM-ing the host at /trigger time.
func TestValidate_NumericCeilings(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	base := func() *Config {
		cfg := baseValidConfigNoGitHub(t)
		cfg.GitHub = GitHubConfig{
			AuthMode: "app",
			App: GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKeyPath: pemPath,
			},
		}

		return cfg
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "max_concurrent over ceiling",
			mutate: func(c *Config) { c.MaxConcurrent = maxMaxConcurrent + 1 },
			want:   "max_concurrent must not exceed",
		},
		{
			name:   "webhook_replay_cache_size over ceiling",
			mutate: func(c *Config) { c.WebhookReplayCacheSize = maxWebhookReplayCacheSize + 1 },
			want:   "webhook_replay_cache_size must not exceed",
		},
		{
			name:   "webhook_replay_skew_seconds over ceiling",
			mutate: func(c *Config) { c.WebhookReplaySkewSeconds = maxWebhookReplaySkewSec + 1 },
			want:   "webhook_replay_skew_seconds must not exceed",
		},
		{
			name:   "message_dedup_cache_size over ceiling",
			mutate: func(c *Config) { c.MessageDedupCacheSize = maxMessageDedupCacheSize + 1 },
			want:   "message_dedup_cache_size must not exceed",
		},
		{
			name:   "message_dedup_ttl_seconds over ceiling",
			mutate: func(c *Config) { c.MessageDedupTTLSeconds = maxMessageDedupTTLSec + 1 },
			want:   "message_dedup_ttl_seconds must not exceed",
		},
		{
			name:   "container_memory_limit over ceiling",
			mutate: func(c *Config) { c.ContainerMemoryLimit = maxContainerMemoryLimit + 1 },
			want:   "container_memory_limit must not exceed",
		},
		{
			name:   "container_pids_limit over ceiling",
			mutate: func(c *Config) { c.ContainerPidsLimit = maxContainerPidsLimit + 1 },
			want:   "container_pids_limit must not exceed",
		},
		{
			name:   "idle_output_timeout over ceiling",
			mutate: func(c *Config) { c.IdleOutputTimeout = maxIdleOutputTimeout + time.Second },
			want:   "idle_output_timeout must not exceed",
		},
		{
			name:   "idle_watchdog_interval over ceiling",
			mutate: func(c *Config) { c.IdleWatchdogInterval = maxIdleWatchdogInterval + time.Second },
			want:   "idle_watchdog_interval must not exceed",
		},
		{
			name:   "maintenance_interval over ceiling",
			mutate: func(c *Config) { c.MaintenanceInterval = maxMaintenanceInterval + time.Second },
			want:   "maintenance_interval must not exceed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(cfg)

			err := cfg.Validate()
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// TestValidate_NumericCeilings_AtCeilingPasses verifies the boundary value
// itself is accepted — only "above the ceiling" should fail.
func TestValidate_NumericCeilings_AtCeilingPasses(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := baseValidConfigNoGitHub(t)
	cfg.GitHub = GitHubConfig{
		AuthMode: "app",
		App: GitHubAppConfig{
			AppID:          1,
			InstallationID: 1,
			PrivateKeyPath: pemPath,
		},
	}
	cfg.MaxConcurrent = maxMaxConcurrent
	cfg.WebhookReplayCacheSize = maxWebhookReplayCacheSize
	cfg.WebhookReplaySkewSeconds = maxWebhookReplaySkewSec
	cfg.MessageDedupCacheSize = maxMessageDedupCacheSize
	cfg.MessageDedupTTLSeconds = maxMessageDedupTTLSec
	cfg.ContainerMemoryLimit = maxContainerMemoryLimit
	cfg.ContainerPidsLimit = maxContainerPidsLimit
	cfg.IdleOutputTimeout = maxIdleOutputTimeout
	cfg.IdleWatchdogInterval = maxIdleWatchdogInterval
	cfg.MaintenanceInterval = maxMaintenanceInterval

	assert.NoError(t, cfg.Validate())
}

// TestApplyDefaults_DerivesAPIBaseURLFromHost verifies that the GitHub
// auth-block "host" field is live config: when only host is set, the runner
// derives a sensible api_base_url so the value flows through to the
// githubauth consumer. Setting github.host alone is enough; the runner does
// not silently accept it as a no-op.
func TestApplyDefaults_DerivesAPIBaseURLFromHost(t *testing.T) {
	cases := []struct {
		name           string
		host           string
		apiBaseURL     string
		wantAPIBaseURL string
	}{
		{
			name:           "bare hostname is wrapped in https",
			host:           "ghe.example.com",
			wantAPIBaseURL: "https://ghe.example.com/api/v3",
		},
		{
			name:           "host with scheme has scheme stripped",
			host:           "https://ghe.example.com",
			wantAPIBaseURL: "https://ghe.example.com/api/v3",
		},
		{
			name:           "host with http scheme still emits https-derived URL",
			host:           "http://ghe.example.com",
			wantAPIBaseURL: "https://ghe.example.com/api/v3",
		},
		{
			name:           "explicit api_base_url is preserved when host also set",
			host:           "ghe.example.com",
			apiBaseURL:     "https://api.acme.ghe.com",
			wantAPIBaseURL: "https://api.acme.ghe.com",
		},
		{
			name:           "host empty leaves api_base_url empty",
			host:           "",
			wantAPIBaseURL: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				GitHub: GitHubConfig{
					Host:       tc.host,
					APIBaseURL: tc.apiBaseURL,
				},
			}
			ApplyDefaults(cfg)
			assert.Equal(t, tc.wantAPIBaseURL, cfg.GitHub.APIBaseURL)
		})
	}
}

// TestValidate_ClaudeOAuthToken_RejectsControlBytes verifies that
// static-env Claude OAuth values containing control bytes / NUL / non-
// printable ASCII are rejected by Validate. These values flow into the
// worker env file as `export KEY='<value>'` — a stray newline would
// terminate the shell-quoted string and inject a second command. Literal
// spaces are intentionally accepted to keep the boot-time check aligned
// with container.IsValidStaticSecretByte's runtime gate (0x20..0x7E);
// space inside single-quoted shell is harmless.
func TestValidate_ClaudeOAuthToken_RejectsControlBytes(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"NUL byte", "valid-prefix\x00trailing"},
		{"newline", "abcdef\nghi"},
		{"carriage return", "abc\rdef"},
		{"tab", "abc\tdef"},
		{"DEL", "abc\x7fdef"},
		{"high bit", "abc\xffdef"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := minimalValidConfig(t)
			c.AnthropicAPIKey = "" // exercise the OAuth-token path
			c.ClaudeOAuthToken = tc.token

			err := c.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "claude_oauth_token")
		})
	}
}

// TestValidate_ClaudeOAuthToken_AcceptsSpace anchors the alignment with
// container.IsValidStaticSecretByte. The config-side check and the
// container-side gate both accept the full 0x20..0x7E range, including 0x20
// (space); a mismatch there would produce a boot-time / refresh-time
// failure. This test pins that contract so a tightening of either side
// surfaces here.
func TestValidate_ClaudeOAuthToken_AcceptsSpace(t *testing.T) {
	c := minimalValidConfig(t)
	c.AnthropicAPIKey = ""
	c.ClaudeOAuthToken = "abc def"

	require.NoError(t, c.Validate(),
		"space (0x20) must be accepted to match container.IsValidStaticSecretByte")
}

// TestValidate_AnthropicAPIKey_RejectsControlBytes mirrors the OAuth-token
// charset check on the api-key path.
func TestValidate_AnthropicAPIKey_RejectsControlBytes(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"NUL byte", "sk-ant-prefix\x00trailing"},
		{"newline", "sk-ant\nfoo"},
		{"carriage return", "sk-ant\rfoo"},
		{"tab", "sk-ant\tfoo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := minimalValidConfig(t)
			c.AnthropicAPIKey = tc.key

			err := c.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "anthropic_api_key")
		})
	}
}

// TestValidate_AnthropicAPIKey_AcceptsValid verifies that the charset
// check leaves normal printable-ASCII tokens unchanged.
func TestValidate_AnthropicAPIKey_AcceptsValid(t *testing.T) {
	c := minimalValidConfig(t)
	c.AnthropicAPIKey = "sk-ant-api03-AaBbCc0123_-456789"

	require.NoError(t, c.Validate())
}

// TestValidate_LogLevel_AllowlistRejectsInvalid verifies that a typo in
// log_level surfaces at Validate-time instead of silently defaulting to
// Info via slog.Level.UnmarshalText's error-swallowing fallback.
func TestValidate_LogLevel_AllowlistRejectsInvalid(t *testing.T) {
	cases := []string{"DEBUG-typo", "trace", "verbose", "fatal", "9"}
	for _, lvl := range cases {
		t.Run(lvl, func(t *testing.T) {
			c := minimalValidConfig(t)
			c.LogLevel = lvl

			err := c.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "log_level")
		})
	}
}

// TestValidate_LogLevel_AllowlistAcceptsValid verifies the allowlist
// accepts the canonical slog level names (case-insensitive) and the
// default-applying empty string.
func TestValidate_LogLevel_AllowlistAcceptsValid(t *testing.T) {
	cases := []string{"", "debug", "info", "warn", "error", "Debug", "INFO", "WARN", "Error"}
	for _, lvl := range cases {
		t.Run(lvl, func(t *testing.T) {
			c := minimalValidConfig(t)
			c.LogLevel = lvl

			require.NoError(t, c.Validate())
		})
	}
}
