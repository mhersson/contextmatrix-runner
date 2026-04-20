// Package config handles loading and validation of runner configuration.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// MinAPIKeyLength is the minimum length for the shared HMAC secret.
// Matches ContextMatrix's MinRunnerAPIKeyLength.
const MinAPIKeyLength = 32

// ImagePullPolicy controls when the runner pulls container images.
const (
	PullAlways       = "always"
	PullNever        = "never"
	PullIfNotPresent = "if-not-present"
)

// Config holds all runner configuration.
type Config struct {
	Port                 int       `yaml:"port"`
	ContextMatrixURL     string    `yaml:"contextmatrix_url"`
	APIKey               string    `yaml:"api_key"`
	BaseImage            string    `yaml:"base_image"`
	AllowedImages        []string  `yaml:"allowed_images"`
	ImagePullPolicy      string    `yaml:"image_pull_policy"`
	MaxConcurrent        int       `yaml:"max_concurrent"`
	ContainerTimeout     string    `yaml:"container_timeout"`
	ContainerMemoryLimit int64     `yaml:"container_memory_limit"`
	ContainerPidsLimit   int64     `yaml:"container_pids_limit"`
	ClaudeAuthDir        string    `yaml:"claude_auth_dir"`
	ClaudeOAuthToken     string    `yaml:"claude_oauth_token"`
	AnthropicAPIKey      string    `yaml:"anthropic_api_key"`
	ClaudeSettings       string    `yaml:"claude_settings"`
	GitHubApp            GitHubApp `yaml:"github_app"`
	GitHubPAT            GitHubPAT `yaml:"github_pat"`
	LogLevel             string    `yaml:"log_level"`

	containerTimeoutDuration time.Duration
}

// GitHubApp holds GitHub App credentials for generating installation tokens.
type GitHubApp struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	APIBaseURL     string `yaml:"api_base_url"`
}

// GitHubPAT holds a fine-grained personal access token used instead of a
// GitHub App in enterprise environments where App creation is restricted.
type GitHubPAT struct {
	Token string `yaml:"token"`
}

// Load reads a YAML config file and applies environment variable overrides.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		Port:                 9090,
		ImagePullPolicy:      PullNever,
		MaxConcurrent:        3,
		ContainerTimeout:     "2h",
		ContainerMemoryLimit: 8 * 1024 * 1024 * 1024, // 8 GiB
		ContainerPidsLimit:   512,
		LogLevel:             "info",
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyEnvOverrides(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

// ContainerTimeoutDuration returns the parsed container timeout duration.
// The value is parsed and cached during Validate().
func (c *Config) ContainerTimeoutDuration() time.Duration {
	return c.containerTimeoutDuration
}

// ParseContainerTimeout parses and caches the container timeout duration.
// Intended for tests that create partial configs without calling Validate().
func (c *Config) ParseContainerTimeout() {
	if d, err := time.ParseDuration(c.ContainerTimeout); err == nil {
		c.containerTimeoutDuration = d
	}
}

// LogLevelSlog returns the slog.Level for the configured log level.
func (c *Config) LogLevelSlog() slog.Level {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Validate checks that all required fields are present and valid.
func (c *Config) Validate() error {
	if c.ContextMatrixURL == "" {
		return fmt.Errorf("contextmatrix_url is required")
	}

	if c.APIKey == "" {
		return fmt.Errorf("api_key is required")
	}

	if len(c.APIKey) < MinAPIKeyLength {
		return fmt.Errorf("api_key must be at least %d characters", MinAPIKeyLength)
	}

	if c.BaseImage == "" {
		return fmt.Errorf("base_image is required")
	}

	switch c.ImagePullPolicy {
	case PullAlways, PullNever, PullIfNotPresent:
	default:
		return fmt.Errorf("image_pull_policy must be one of: always, never, if-not-present")
	}

	if c.MaxConcurrent < 1 {
		return fmt.Errorf("max_concurrent must be at least 1")
	}

	d, err := time.ParseDuration(c.ContainerTimeout)
	if err != nil {
		return fmt.Errorf("container_timeout is invalid: %w", err)
	}

	c.containerTimeoutDuration = d
	if c.ClaudeAuthDir == "" && c.ClaudeOAuthToken == "" && c.AnthropicAPIKey == "" {
		return fmt.Errorf("at least one of claude_auth_dir, claude_oauth_token, or anthropic_api_key is required")
	}

	if c.ClaudeAuthDir != "" {
		if _, err := os.Stat(c.ClaudeAuthDir); err != nil {
			return fmt.Errorf("claude_auth_dir does not exist: %w", err)
		}
	}

	if c.ClaudeSettings != "" && !json.Valid([]byte(c.ClaudeSettings)) {
		return fmt.Errorf("claude_settings is not valid JSON")
	}

	appConfigured := c.GitHubApp.AppID != 0 || c.GitHubApp.InstallationID != 0 || c.GitHubApp.PrivateKeyPath != ""

	patConfigured := c.GitHubPAT.Token != ""
	switch {
	case appConfigured && patConfigured:
		return fmt.Errorf("exactly one of github_app or github_pat may be configured")
	case !appConfigured && !patConfigured:
		return fmt.Errorf("either github_app or github_pat is required")
	case appConfigured:
		if err := c.GitHubApp.validate(); err != nil {
			return fmt.Errorf("github_app: %w", err)
		}
	}

	return nil
}

func (g *GitHubApp) validate() error {
	if g.AppID == 0 {
		return fmt.Errorf("app_id is required")
	}

	if g.InstallationID == 0 {
		return fmt.Errorf("installation_id is required")
	}

	if g.PrivateKeyPath == "" {
		return fmt.Errorf("private_key_path is required")
	}

	if _, err := os.Stat(g.PrivateKeyPath); err != nil {
		return fmt.Errorf("private_key_path does not exist: %w", err)
	}

	return nil
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("CMR_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
		}
	}

	if v := os.Getenv("CMR_CONTEXTMATRIX_URL"); v != "" {
		cfg.ContextMatrixURL = v
	}

	if v := os.Getenv("CMR_API_KEY"); v != "" {
		cfg.APIKey = v
	}

	if v := os.Getenv("CMR_BASE_IMAGE"); v != "" {
		cfg.BaseImage = v
	}

	if v := os.Getenv("CMR_IMAGE_PULL_POLICY"); v != "" {
		cfg.ImagePullPolicy = v
	}

	if v := os.Getenv("CMR_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxConcurrent = n
		}
	}

	if v := os.Getenv("CMR_CONTAINER_TIMEOUT"); v != "" {
		cfg.ContainerTimeout = v
	}

	if v := os.Getenv("CMR_CONTAINER_MEMORY_LIMIT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.ContainerMemoryLimit = n
		}
	}

	if v := os.Getenv("CMR_CONTAINER_PIDS_LIMIT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.ContainerPidsLimit = n
		}
	}

	if v := os.Getenv("CMR_CLAUDE_AUTH_DIR"); v != "" {
		cfg.ClaudeAuthDir = v
	}

	if v := os.Getenv("CMR_CLAUDE_OAUTH_TOKEN"); v != "" {
		cfg.ClaudeOAuthToken = v
	}

	if v := os.Getenv("CMR_ANTHROPIC_API_KEY"); v != "" {
		cfg.AnthropicAPIKey = v
	}

	if v := os.Getenv("CMR_CLAUDE_SETTINGS"); v != "" {
		cfg.ClaudeSettings = v
	}

	if v := os.Getenv("CMR_GITHUB_APP_ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.GitHubApp.AppID = n
		}
	}

	if v := os.Getenv("CMR_GITHUB_INSTALLATION_ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.GitHubApp.InstallationID = n
		}
	}

	if v := os.Getenv("CMR_GITHUB_PRIVATE_KEY_PATH"); v != "" {
		cfg.GitHubApp.PrivateKeyPath = v
	}

	if v := os.Getenv("CMR_GITHUB_API_BASE_URL"); v != "" {
		cfg.GitHubApp.APIBaseURL = v
	}

	if v := os.Getenv("CMR_GITHUB_PAT_TOKEN"); v != "" {
		cfg.GitHubPAT.Token = v
	}

	if v := os.Getenv("CMR_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
}
