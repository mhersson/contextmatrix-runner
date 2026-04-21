// Package config handles loading and validation of runner configuration.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
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

// DeploymentProfile constants define the two supported operational modes.
const (
	ProfileProduction = "production"
	ProfileDev        = "dev"
)

// UnpinnedImageRef records an image reference that failed the digest-pin check
// but was accepted because the runner is in dev mode. Populated during
// Validate(); never serialised to YAML.
type UnpinnedImageRef struct {
	Field string
	Image string
}

// Config holds all runner configuration.
type Config struct {
	Port                 int       `yaml:"port"`
	AdminPort            int       `yaml:"admin_port"`
	ContextMatrixURL     string    `yaml:"contextmatrix_url"`
	APIKey               string    `yaml:"api_key"`
	BaseImage            string    `yaml:"base_image"`
	AllowedImages        []string  `yaml:"allowed_images"`
	AllowedMCPHosts      []string  `yaml:"allowed_mcp_hosts"`
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
	LogFormat            string    `yaml:"log_format"`
	// SecretsDir is the host directory where per-container secrets files
	// are written. Each file is bind-mounted read-only into its container
	// at /run/cm-secrets/env so the values never appear in HostConfig.Env
	// (and therefore not in `docker inspect`). Should be on tmpfs.
	SecretsDir string `yaml:"secrets_dir"`

	// Webhook replay-protection tunables. See CTXRUN-047.
	WebhookReplayCacheSize   int `yaml:"webhook_replay_cache_size"`
	WebhookReplaySkewSeconds int `yaml:"webhook_replay_skew_seconds"`
	MessageDedupCacheSize    int `yaml:"message_dedup_cache_size"`
	MessageDedupTTLSeconds   int `yaml:"message_dedup_ttl_seconds"`

	// Idle-output watchdog (CTXRUN-058, H15). If a container's logparser
	// has not published any event in this many seconds, the watchdog kills
	// the container with an "idle timeout" reason. Zero or negative
	// disables the watchdog.
	IdleOutputTimeout time.Duration `yaml:"idle_output_timeout"`

	// MaintenanceInterval is the tick interval for the background
	// reconcile-and-prune loop (CTXRUN-058, M12). Each tick runs
	// CleanupOrphans and PruneImages. Must be positive.
	MaintenanceInterval time.Duration `yaml:"maintenance_interval"`

	// UseHMACForVerifyAutonomous toggles whether the VerifyAutonomous
	// callback to CM is HMAC-signed (true, default) or falls back to
	// `Authorization: Bearer <api_key>` (false, deprecated transition
	// mode). See CTXRUN-048. Set false ONLY while the ContextMatrix
	// server is being upgraded to accept HMAC on that GET endpoint.
	UseHMACForVerifyAutonomous bool `yaml:"use_hmac_for_verify_autonomous"`

	// DeploymentProfile selects the operational mode: "production" (default,
	// strict) or "dev" (loosens validators for local single-box setups).
	// Follow-up subtasks document which specific validators are relaxed in
	// dev mode. Env: CMR_DEPLOYMENT_PROFILE.
	DeploymentProfile string `yaml:"deployment_profile"`

	// UnpinnedImageRefs is populated during Validate() when IsDev() is true
	// and one or more image references are not digest-pinned. Callers (main.go)
	// log a WARN per entry. Never serialised to YAML.
	UnpinnedImageRefs []UnpinnedImageRef `yaml:"-"`

	containerTimeoutDuration time.Duration
}

// Log format constants.
const (
	LogFormatText = "text"
	LogFormatJSON = "json"
)

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

	// defaultSecretsDir is a filesystem PATH, not a credential. It is named
	// via a const to avoid the gosec G101 false-positive on struct literals.
	const defaultSecretsDir = "/var/run/cm-runner/secrets" //nolint:gosec // path, not a credential

	cfg := &Config{
		Port:                       9090,
		AdminPort:                  9091,
		ImagePullPolicy:            PullNever,
		MaxConcurrent:              3,
		ContainerTimeout:           "2h",
		ContainerMemoryLimit:       8 * 1024 * 1024 * 1024, // 8 GiB
		ContainerPidsLimit:         512,
		LogLevel:                   "info",
		LogFormat:                  LogFormatText,
		WebhookReplayCacheSize:     10000,
		WebhookReplaySkewSeconds:   330,
		MessageDedupCacheSize:      1000,
		MessageDedupTTLSeconds:     600,
		SecretsDir:                 defaultSecretsDir,
		IdleOutputTimeout:          30 * time.Minute,
		MaintenanceInterval:        10 * time.Minute,
		UseHMACForVerifyAutonomous: true,
		DeploymentProfile:          ProfileProduction,
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

// LogLevelSlog returns the slog.Level for the configured log level. An
// unknown value leaves lvl at its zero value (LevelInfo), matching the
// previous switch-default behaviour; we ignore the UnmarshalText error for
// the same reason.
func (c *Config) LogLevelSlog() slog.Level {
	var lvl slog.Level

	_ = lvl.UnmarshalText([]byte(c.LogLevel))

	return lvl
}

// IsDev returns true when the runner is configured in dev mode.
// Dev mode loosens certain validators for local single-box setups.
func (c *Config) IsDev() bool { return c.DeploymentProfile == ProfileDev }

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

	// Digest-pin base_image and every allowed_images entry (CTXRUN-044).
	// A mutable tag like `:latest` would let a rebuilt upstream image
	// silently ship into production; require `@sha256:...` so operators
	// roll base images intentionally.
	//
	// In dev mode we collect unpinned references instead of failing hard, so
	// local development setups can use mutable tags. The caller (main.go) logs
	// a WARN per entry. Production mode keeps the fail-closed behaviour.
	c.UnpinnedImageRefs = nil

	if err := requireDigestPin("base_image", c.BaseImage); err != nil {
		if !c.IsDev() {
			return err
		}

		c.UnpinnedImageRefs = append(c.UnpinnedImageRefs, UnpinnedImageRef{Field: "base_image", Image: c.BaseImage})
	}

	for i, img := range c.AllowedImages {
		field := fmt.Sprintf("allowed_images[%d]", i)
		if err := requireDigestPin(field, img); err != nil {
			if !c.IsDev() {
				return err
			}

			c.UnpinnedImageRefs = append(c.UnpinnedImageRefs, UnpinnedImageRef{Field: field, Image: img})
		}
	}

	switch c.ImagePullPolicy {
	case PullAlways, PullNever, PullIfNotPresent:
	default:
		return fmt.Errorf("image_pull_policy must be one of: always, never, if-not-present")
	}

	if c.MaxConcurrent < 1 {
		return fmt.Errorf("max_concurrent must be at least 1")
	}

	// Replay-protection tunables default silently when unset so hand-
	// crafted configs (and tests that construct Config literals) don't
	// have to opt in every time. Only explicit negative values are an
	// error. Keep defaults in sync with Load().
	if c.WebhookReplayCacheSize == 0 {
		c.WebhookReplayCacheSize = 10000
	}

	if c.WebhookReplaySkewSeconds == 0 {
		c.WebhookReplaySkewSeconds = 330
	}

	if c.MessageDedupCacheSize == 0 {
		c.MessageDedupCacheSize = 1000
	}

	if c.MessageDedupTTLSeconds == 0 {
		c.MessageDedupTTLSeconds = 600
	}

	if c.WebhookReplayCacheSize < 0 {
		return fmt.Errorf("webhook_replay_cache_size must be positive")
	}

	if c.WebhookReplaySkewSeconds < 0 {
		return fmt.Errorf("webhook_replay_skew_seconds must be positive")
	}

	if c.MessageDedupCacheSize < 0 {
		return fmt.Errorf("message_dedup_cache_size must be positive")
	}

	if c.MessageDedupTTLSeconds < 0 {
		return fmt.Errorf("message_dedup_ttl_seconds must be positive")
	}

	if c.AdminPort != 0 && (c.AdminPort < 1 || c.AdminPort > 65535) {
		return fmt.Errorf("admin_port must be between 1 and 65535")
	}

	switch c.LogFormat {
	case "", LogFormatText, LogFormatJSON:
		// ok
	default:
		return fmt.Errorf("log_format must be one of: text, json")
	}

	switch c.DeploymentProfile {
	case "", ProfileProduction, ProfileDev:
		// Normalise empty to production.
		if c.DeploymentProfile == "" {
			c.DeploymentProfile = ProfileProduction
		}
	default:
		return fmt.Errorf("deployment_profile must be one of: production, dev")
	}

	d, err := time.ParseDuration(c.ContainerTimeout)
	if err != nil {
		return fmt.Errorf("container_timeout is invalid: %w", err)
	}

	c.containerTimeoutDuration = d

	// Idle-output watchdog (CTXRUN-058). Negative values are an error.
	// Zero is legal and disables the watchdog.
	if c.IdleOutputTimeout < 0 {
		return fmt.Errorf("idle_output_timeout must be zero or positive")
	}

	// Maintenance loop interval (CTXRUN-058). Default silently when zero so
	// hand-crafted configs don't have to opt in; negatives are an error.
	if c.MaintenanceInterval == 0 {
		c.MaintenanceInterval = 10 * time.Minute
	}

	if c.MaintenanceInterval < 0 {
		return fmt.Errorf("maintenance_interval must be positive")
	}

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

// requireDigestPin rejects image references that are not @sha256:... pinned.
// A valid digest reference has the form <name>@sha256:<64 hex chars>. This
// closes REVIEW.md H2 (allowlist matches mutable strings) by forcing every
// image reference the runner accepts to name an immutable content hash.
func requireDigestPin(field, image string) error {
	if image == "" {
		return fmt.Errorf("%s must be @sha256:... pinned (got empty string)", field)
	}

	at := strings.Index(image, "@sha256:")
	if at <= 0 {
		return fmt.Errorf("%s must be @sha256:... pinned (got %q)", field, image)
	}

	digest := image[at+len("@sha256:"):]
	if len(digest) != 64 {
		return fmt.Errorf("%s has invalid sha256 digest length: %q", field, image)
	}

	for _, r := range digest {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("%s has non-hex characters in sha256 digest: %q", field, image)
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

	if v := os.Getenv("CMR_SECRETS_DIR"); v != "" {
		cfg.SecretsDir = v
	}

	if v := os.Getenv("CMR_LOG_FORMAT"); v != "" {
		cfg.LogFormat = v
	}

	if v := os.Getenv("CMR_ADMIN_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.AdminPort = n
		}
	}

	if v := os.Getenv("CMR_DEPLOYMENT_PROFILE"); v != "" {
		cfg.DeploymentProfile = v
	}
}
