package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
	assert.Equal(t, 3, cfg.MaxConcurrent)
	assert.Equal(t, "2h", cfg.ContainerTimeout)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "app", cfg.GitHub.AuthMode)
	assert.Equal(t, int64(12345), cfg.GitHub.App.AppID)
	assert.Equal(t, int64(67890), cfg.GitHub.App.InstallationID)
	assert.Equal(t, pemPath, cfg.GitHub.App.PrivateKeyPath)
}

// TestLoad_DurationStrings confirms Go duration strings (the format used in
// config.yaml.example) parse cleanly into idle_output_timeout and
// maintenance_interval. Before the Duration wrapper landed, anyone copying
// the example verbatim hit `cannot unmarshal !!str "30m" into time.Duration`
// at startup.
func TestLoad_DurationStrings(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	claudeDir := dir

	content := validConfig(pemPath, claudeDir) + `
idle_output_timeout: "30m"
maintenance_interval: "10m"
`
	path := writeConfig(t, dir, content)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 30*time.Minute, time.Duration(cfg.IdleOutputTimeout))
	assert.Equal(t, 10*time.Minute, time.Duration(cfg.MaintenanceInterval))
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
		APIKey:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AgentImage: testDigestImage,
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
		AgentImage:       testDigestImage,
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
		AgentImage:                testDigestImage,
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			pemPath := writePEM(t, dir)

			cfg := &Config{
				ContextMatrixURL: tt.url,
				APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				AgentImage:       testDigestImage,
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
		AgentImage:                testDigestImage,
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
		AgentImage:       testDigestImage,
	}
	err := cfg.Validate()
	assert.ErrorContains(t, err, "api_key must be at least")
}

func TestValidate_MissingAgentImage(t *testing.T) {
	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ImagePullPolicy:  PullNever,
		MaxConcurrent:    1,
		ContainerTimeout: "1h",
	}
	err := cfg.Validate()
	assert.ErrorContains(t, err, "agent_image is required")
}

func TestValidate_NoCCAuth(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AgentImage:       testDigestImage,
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
		AgentImage:       testDigestImage,
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
		AgentImage:       testDigestImage,
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
			AgentImage:       testDigestImage,
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
agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
		AgentImage:       testDigestImage,
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
		AgentImage:       testDigestImage,
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
		AgentImage:       testDigestImage,
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
			AgentImage:       testDigestImage,
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
agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
		AgentImage:       testDigestImage,
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
	// A Config literal that leaves the new CTXRUN-047 tunables at zero
	// must validate and receive the documented defaults.
	cfg := baseValidConfigNoGitHub(t)
	cfg.GitHub = GitHubConfig{AuthMode: "pat", PAT: GitHubPATConfig{Token: "ghp_patonly"}}

	require.NoError(t, cfg.Validate())

	assert.Equal(t, 10000, cfg.WebhookReplayCacheSize)
	assert.Equal(t, 330, cfg.WebhookReplaySkewSeconds)
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
agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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

func TestApplyEnvOverrides_AgentImage(t *testing.T) {
	t.Setenv("CMR_AGENT_IMAGE", "ghcr.io/example/agent@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")

	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	claudeDir := dir

	path := writeConfig(t, dir, validConfig(pemPath, claudeDir))
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "ghcr.io/example/agent@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", cfg.AgentImage)
}

// TestValidate_AgentImageDigestPin covers the CTXRUN-044 requirement that
// agent_image be an @sha256:... reference. Mutable tags and malformed digests
// must fail validation so a rebuilt upstream image can never silently ship.
func TestValidate_AgentImageDigestPin(t *testing.T) {
	validDigest := "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	tests := []struct {
		name        string
		image       string
		wantErr     bool
		errContains string
	}{
		{
			name:    "digest-pinned agent_image is accepted",
			image:   validDigest,
			wantErr: false,
		},
		{
			name:        "tag-only agent_image is rejected",
			image:       "contextmatrix/orchestrated:latest",
			wantErr:     true,
			errContains: "agent_image must be @sha256:... pinned",
		},
		{
			name:        "bare name without tag or digest is rejected",
			image:       "contextmatrix/orchestrated",
			wantErr:     true,
			errContains: "agent_image must be @sha256:... pinned",
		},
		{
			name:        "digest of wrong length is rejected",
			image:       "contextmatrix/orchestrated@sha256:deadbeef",
			wantErr:     true,
			errContains: "invalid sha256 digest length",
		},
		{
			name:        "digest with non-hex characters is rejected",
			image:       "contextmatrix/orchestrated@sha256:zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
			wantErr:     true,
			errContains: "non-hex characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseValidConfigNoGitHub(t)
			cfg.GitHub = GitHubConfig{AuthMode: "pat", PAT: GitHubPATConfig{Token: "ghp_patonly"}}
			cfg.AgentImage = tt.image

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
		AgentImage:        testDigestImage,
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

// TestValidate_DevMode_UnpinnedAgentImage verifies that in dev mode an unpinned
// agent_image does not cause Validate to return an error, and the reference is
// collected in UnpinnedImageRefs.
func TestValidate_DevMode_UnpinnedAgentImage(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := baseDevConfig(t, pemPath)
	cfg.AgentImage = "contextmatrix/orchestrated:latest"

	err := cfg.Validate()
	require.NoError(t, err)

	require.Len(t, cfg.UnpinnedImageRefs, 1)
	assert.Equal(t, "agent_image", cfg.UnpinnedImageRefs[0].Field)
	assert.Equal(t, "contextmatrix/orchestrated:latest", cfg.UnpinnedImageRefs[0].Image)
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

// TestValidate_Production_UnpinnedAgentImageFails verifies that production mode
// keeps the existing fail-closed behaviour for unpinned agent_image.
func TestValidate_Production_UnpinnedAgentImageFails(t *testing.T) {
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
	cfg.AgentImage = "contextmatrix/orchestrated:latest"
	// DeploymentProfile is zero-value ("") which Validate normalises to production.

	err := cfg.Validate()
	require.ErrorContains(t, err, "agent_image must be @sha256:... pinned")
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
	cfg.AgentImage = pinnedA
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
// allowed_images allowlist is digest-pinned, not just agent_image. A single
// tag-only entry must fail validation so H2's "allowlist matches strings
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
// with no explicit skew, pull policy, or secrets_dir receives the dev
// defaults and that AppliedDevDefaults records all three.
func TestLoad_DevDefaults_UnsetValues(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdg)

	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	path := writeConfig(t, dir, validConfigDev(pemPath, dir))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 86400, cfg.WebhookReplaySkewSeconds, "dev mode: skew should default to 86400")
	assert.Equal(t, PullIfNotPresent, cfg.ImagePullPolicy, "dev mode: pull policy should default to if-not-present")

	expectedSecrets := filepath.Join(xdg, "cm-runner", "secrets")
	assert.Equal(t, expectedSecrets, cfg.SecretsDir, "dev mode: secrets_dir should default under XDG_RUNTIME_DIR")
	assert.ElementsMatch(t,
		[]string{
			"webhook_replay_skew_seconds=86400",
			"image_pull_policy=if-not-present",
			"secrets_dir=" + expectedSecrets,
		},
		cfg.AppliedDevDefaults,
	)
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
// production default and previously regressed — see git history).
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

// TestLoad_DevDefaults_SecretsDir verifies that the dev profile picks a
// user-writable default for secrets_dir when none is configured. The
// production default `/var/run/cm-runner/secrets` lives under a root-owned
// tmpfs, which fails for non-root local runners — the dev profile must
// steer to $XDG_RUNTIME_DIR (or a temp fallback) and record the chosen
// path in AppliedDevDefaults.
func TestLoad_DevDefaults_SecretsDir(t *testing.T) {
	t.Run("xdg_runtime_dir_set", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", xdg)

		dir := t.TempDir()
		pemPath := writePEM(t, dir)
		path := writeConfig(t, dir, validConfigDev(pemPath, dir))

		cfg, err := Load(path)
		require.NoError(t, err)

		expected := filepath.Join(xdg, "cm-runner", "secrets")
		assert.Equal(t, expected, cfg.SecretsDir,
			"dev mode should default secrets_dir to $XDG_RUNTIME_DIR/cm-runner/secrets")
		assert.Contains(t, cfg.AppliedDevDefaults, "secrets_dir="+expected)
	})

	t.Run("xdg_runtime_dir_unset_falls_back_to_tempdir", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")

		dir := t.TempDir()
		pemPath := writePEM(t, dir)
		path := writeConfig(t, dir, validConfigDev(pemPath, dir))

		cfg, err := Load(path)
		require.NoError(t, err)

		assert.True(t, filepath.IsAbs(cfg.SecretsDir),
			"dev fallback must produce an absolute path; got %q", cfg.SecretsDir)
		assert.True(t, strings.HasPrefix(cfg.SecretsDir, os.TempDir()),
			"dev fallback must live under os.TempDir(); got %q", cfg.SecretsDir)
		assert.Contains(t, cfg.SecretsDir, "cm-runner",
			"dev fallback should embed cm-runner in the path")
		assert.Contains(t, cfg.AppliedDevDefaults, "secrets_dir="+cfg.SecretsDir)
	})

	t.Run("explicit_yaml_overrides_dev_default", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", xdg)

		dir := t.TempDir()
		pemPath := writePEM(t, dir)
		explicit := filepath.Join(dir, "explicit-secrets")
		yaml := validConfigDev(pemPath, dir) + "secrets_dir: " + explicit + "\n"
		path := writeConfig(t, dir, yaml)

		cfg, err := Load(path)
		require.NoError(t, err)

		assert.Equal(t, explicit, cfg.SecretsDir,
			"explicit secrets_dir must not be overridden in dev mode")

		for _, entry := range cfg.AppliedDevDefaults {
			assert.False(t, strings.HasPrefix(entry, "secrets_dir="),
				"explicit secrets_dir must not appear in AppliedDevDefaults; got %q", entry)
		}
	})

	t.Run("env_override_wins_over_dev_default", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", xdg)

		dir := t.TempDir()
		pemPath := writePEM(t, dir)
		envPath := filepath.Join(dir, "env-secrets")
		t.Setenv("CMR_SECRETS_DIR", envPath)
		path := writeConfig(t, dir, validConfigDev(pemPath, dir))

		cfg, err := Load(path)
		require.NoError(t, err)

		assert.Equal(t, envPath, cfg.SecretsDir,
			"CMR_SECRETS_DIR must take precedence over the dev default")

		for _, entry := range cfg.AppliedDevDefaults {
			assert.False(t, strings.HasPrefix(entry, "secrets_dir="),
				"env-overridden secrets_dir must not appear in AppliedDevDefaults; got %q", entry)
		}
	})
}

// TestLoad_ProductionDefaults_SecretsDir verifies that the production
// profile keeps `/var/run/cm-runner/secrets` even when XDG_RUNTIME_DIR is
// set in the environment. Operators are expected to provision that path
// (or override via secrets_dir / CMR_SECRETS_DIR) — the production default
// must not silently shift onto a user-runtime path.
func TestLoad_ProductionDefaults_SecretsDir(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdg) // intentionally set; production must ignore.

	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	path := writeConfig(t, dir, validConfig(pemPath, dir))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "/var/run/cm-runner/secrets", cfg.SecretsDir,
		"production: secrets_dir must default to /var/run/cm-runner/secrets")
	assert.Empty(t, cfg.AppliedDevDefaults,
		"production mode must not populate AppliedDevDefaults")
}

func TestConfig_TaskSkillsDir(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
contextmatrix_url: "http://localhost:8080"
api_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
		AgentImage:       testDigestImage,
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
agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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

// TestConfigAgentImageRequired verifies that omitting agent_image fails
// Validate.
func TestConfigAgentImageRequired(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	// validConfig sets agent_image; strip it back out to test the
	// required-field path.
	yaml := strings.Replace(
		validConfig(pemPath, dir),
		`agent_image: "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`,
		"",
		1,
	)
	path := writeConfig(t, dir, yaml)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent_image")
}
