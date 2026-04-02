package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
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
base_image: "contextmatrix/worker:latest"
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
	assert.Equal(t, "contextmatrix/worker:latest", cfg.BaseImage)
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
		BaseImage: "img",
	}
	err := cfg.Validate()
	assert.ErrorContains(t, err, "contextmatrix_url is required")
}

func TestValidate_APIKeyTooShort(t *testing.T) {
	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "short",
		BaseImage:        "img",
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
		BaseImage:        "img",
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
	assert.ErrorContains(t, err, "at least one of claude_auth_dir or anthropic_api_key")
}

func TestValidate_AnthropicAPIKeyAlone(t *testing.T) {
	dir := t.TempDir()
	pemPath := writePEM(t, dir)

	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        "img",
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

func TestValidate_InvalidContainerTimeout(t *testing.T) {
	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        "img",
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

	cfg := &Config{
		ContextMatrixURL: "http://localhost",
		APIKey:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseImage:        "img",
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    1,
		ContainerTimeout: "1h",
		AnthropicAPIKey:  "sk-ant-test",
		GitHubApp:        GitHubApp{},
	}
	err := cfg.Validate()
	assert.ErrorContains(t, err, "github_app: app_id is required")

	cfg.GitHubApp.AppID = 1
	err = cfg.Validate()
	assert.ErrorContains(t, err, "github_app: installation_id is required")

	cfg.GitHubApp.InstallationID = 1
	err = cfg.Validate()
	assert.ErrorContains(t, err, "github_app: private_key_path is required")

	cfg.GitHubApp.PrivateKeyPath = filepath.Join(dir, "nonexistent.pem")
	err = cfg.Validate()
	assert.ErrorContains(t, err, "private_key_path does not exist")
}

func TestContainerTimeoutDuration(t *testing.T) {
	cfg := &Config{ContainerTimeout: "2h"}
	assert.Equal(t, 2*60*60*1e9, float64(cfg.ContainerTimeoutDuration()))
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
