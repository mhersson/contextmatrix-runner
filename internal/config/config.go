// Package config handles loading and validation of runner configuration.
package config

import (
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
	Port             int       `yaml:"port"`
	ContextMatrixURL string    `yaml:"contextmatrix_url"`
	APIKey           string    `yaml:"api_key"`
	BaseImage        string    `yaml:"base_image"`
	ImagePullPolicy  string    `yaml:"image_pull_policy"`
	MaxConcurrent    int       `yaml:"max_concurrent"`
	ContainerTimeout string    `yaml:"container_timeout"`
	ClaudeAuthDir    string    `yaml:"claude_auth_dir"`
	AnthropicAPIKey  string    `yaml:"anthropic_api_key"`
	GitHubApp        GitHubApp `yaml:"github_app"`
	LogLevel         string    `yaml:"log_level"`
}

// GitHubApp holds GitHub App credentials for generating installation tokens.
type GitHubApp struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
}

// Load reads a YAML config file and applies environment variable overrides.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		Port:             9090,
		ImagePullPolicy:  PullAlways,
		MaxConcurrent:    3,
		ContainerTimeout: "2h",
		LogLevel:         "info",
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

// ContainerTimeoutDuration parses ContainerTimeout as a time.Duration.
// Panics if the value is invalid; call Validate() first.
func (c *Config) ContainerTimeoutDuration() time.Duration {
	d, err := time.ParseDuration(c.ContainerTimeout)
	if err != nil {
		panic(fmt.Sprintf("invalid container_timeout %q: %v", c.ContainerTimeout, err))
	}
	return d
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
	if _, err := time.ParseDuration(c.ContainerTimeout); err != nil {
		return fmt.Errorf("container_timeout is invalid: %w", err)
	}
	if c.ClaudeAuthDir == "" && c.AnthropicAPIKey == "" {
		return fmt.Errorf("at least one of claude_auth_dir or anthropic_api_key is required")
	}
	if c.ClaudeAuthDir != "" {
		if _, err := os.Stat(c.ClaudeAuthDir); err != nil {
			return fmt.Errorf("claude_auth_dir does not exist: %w", err)
		}
	}
	if err := c.GitHubApp.validate(); err != nil {
		return fmt.Errorf("github_app: %w", err)
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
	if v := os.Getenv("CMR_CLAUDE_AUTH_DIR"); v != "" {
		cfg.ClaudeAuthDir = v
	}
	if v := os.Getenv("CMR_ANTHROPIC_API_KEY"); v != "" {
		cfg.AnthropicAPIKey = v
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
	if v := os.Getenv("CMR_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
}
