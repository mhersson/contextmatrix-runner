// Package config handles loading and validation of runner configuration.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
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
	Port                      int    `yaml:"port"`
	AdminPort                 int    `yaml:"admin_port"`
	ContextMatrixURL          string `yaml:"contextmatrix_url"`
	ContainerContextMatrixURL string `yaml:"container_contextmatrix_url"`
	APIKey                    string `yaml:"api_key"`
	// AgentImage is the image used for orchestrated worker containers
	// (long-lived shell; runner spawns CC per phase). Required; must be
	// digest-pinned (@sha256:...) to prevent silent upstream rebuilds.
	AgentImage           string   `yaml:"agent_image"`
	AllowedImages        []string `yaml:"allowed_images"`
	ImagePullPolicy      string   `yaml:"image_pull_policy"`
	MaxConcurrent        int      `yaml:"max_concurrent"`
	ContainerTimeout     string   `yaml:"container_timeout"`
	ContainerMemoryLimit int64    `yaml:"container_memory_limit"`
	ContainerPidsLimit   int64    `yaml:"container_pids_limit"`
	ClaudeAuthDir        string   `yaml:"claude_auth_dir"`
	ClaudeOAuthToken     string   `yaml:"claude_oauth_token"`
	AnthropicAPIKey      string   `yaml:"anthropic_api_key"`
	ClaudeSettings       string   `yaml:"claude_settings"`
	// WorkerExtraEnv is a generic env-passthrough into spawned worker
	// containers. Keys reserved by the runner (MCP_URL, MCP_API_KEY,
	// CM_CARD_ID, CM_PROJECT, CM_GIT_TOKEN, GH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN,
	// ANTHROPIC_API_KEY) are filtered out — the dispatcher sets those itself
	// from per-trigger payload + runner config. Intended for operator-supplied
	// env like GIT_SSL_NO_VERIFY=1 against a self-signed local git server,
	// proxy hostnames, etc. Use sparingly; values become visible inside the
	// container.
	WorkerExtraEnv map[string]string `yaml:"worker_extra_env,omitempty"`
	GitHub         GitHubConfig      `yaml:"github"`
	LogLevel       string            `yaml:"log_level"`
	LogFormat      string            `yaml:"log_format"`
	// SecretsDir is the host directory where per-container secrets files
	// are written. Each file is bind-mounted read-only into its container
	// at /run/cm-secrets/env so the values never appear in HostConfig.Env
	// (and therefore not in `docker inspect`). Should be on tmpfs.
	SecretsDir string `yaml:"secrets_dir"`
	// TaskSkillsDir is the host path to the curated task skills repo, bind-mounted
	// read-only into worker containers at /host-skills. The entrypoint copies the
	// resolved subset (per CM_TASK_SKILLS env var) into ~/.claude/skills. Empty
	// disables the feature.
	TaskSkillsDir string `yaml:"task_skills_dir"`

	// Webhook replay-protection tunables. See CTXRUN-047.
	WebhookReplayCacheSize   int `yaml:"webhook_replay_cache_size"`
	WebhookReplaySkewSeconds int `yaml:"webhook_replay_skew_seconds"`

	// Idle-output watchdog. If a container has published no log entry in
	// this many seconds, the watchdog kills the container with an "idle
	// timeout" reason. Zero or negative disables the watchdog. Accepts Go
	// duration strings ("30m") in YAML.
	IdleOutputTimeout Duration `yaml:"idle_output_timeout"`

	// MaintenanceInterval is the tick interval for the background
	// reconcile-and-prune loop (CTXRUN-058, M12). Each tick runs
	// CleanupOrphans and PruneImages. Must be positive. Accepts Go
	// duration strings ("10m") in YAML.
	MaintenanceInterval Duration `yaml:"maintenance_interval"`

	// DeploymentProfile selects the operational mode: "production" (default,
	// strict) or "dev" (loosens validators for local single-box setups).
	// Follow-up subtasks document which specific validators are relaxed in
	// dev mode. Env: CMR_DEPLOYMENT_PROFILE.
	DeploymentProfile string `yaml:"deployment_profile"`

	// UnpinnedImageRefs is populated during Validate() when IsDev() is true
	// and one or more image references are not digest-pinned. Callers (main.go)
	// log a WARN per entry. Never serialised to YAML.
	UnpinnedImageRefs []UnpinnedImageRef `yaml:"-"`

	// AppliedDevDefaults records which defaults were automatically applied
	// because DeploymentProfile == ProfileDev and the value was unset.
	// Read-only after Load returns. Empty in production mode.
	AppliedDevDefaults []string `yaml:"-"`

	containerTimeoutDuration time.Duration
}

// Log format constants.
const (
	LogFormatText = "text"
	LogFormatJSON = "json"
)

// GitHubAppConfig holds GitHub App credentials for generating installation tokens.
type GitHubAppConfig struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
}

// GitHubPATConfig holds a fine-grained personal access token used instead of a
// GitHub App in enterprise environments where App creation is restricted.
type GitHubPATConfig struct {
	Token string `yaml:"token"`
}

// GitHubConfig is the unified GitHub auth block. Set AuthMode to "app" or "pat".
type GitHubConfig struct {
	AuthMode   string          `yaml:"auth_mode"`    // "app" | "pat"
	Host       string          `yaml:"host"`         // optional GHE/GHEC-DR host
	APIBaseURL string          `yaml:"api_base_url"` // optional override
	App        GitHubAppConfig `yaml:"app"`
	PAT        GitHubPATConfig `yaml:"pat"`
}

// ResolvedAPIBaseURL returns the effective GitHub API base URL.
// Precedence: APIBaseURL (trimmed) > "https://api." + Host > "https://api.github.com".
func (g *GitHubConfig) ResolvedAPIBaseURL() string {
	if v := strings.TrimSpace(g.APIBaseURL); v != "" {
		return v
	}

	if g.Host != "" {
		return "https://api." + g.Host
	}

	return "https://api.github.com"
}

// AllowedHosts returns the list of GitHub hostnames that are permitted.
// When Host is empty or "github.com", only ["github.com"] is returned.
// For any other Host value, ["github.com", Host] is returned.
func (g *GitHubConfig) AllowedHosts() []string {
	if g.Host == "" || g.Host == "github.com" {
		return []string{"github.com"}
	}

	return []string{"github.com", g.Host}
}

// Load reads a YAML config file and applies environment variable overrides.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		Port:                   9090,
		AdminPort:              0,
		MaxConcurrent:          3,
		ContainerTimeout:       "2h",
		ContainerMemoryLimit:   8 * 1024 * 1024 * 1024, // 8 GiB
		ContainerPidsLimit:     512,
		LogLevel:               "info",
		LogFormat:              LogFormatText,
		WebhookReplayCacheSize: 10000,
		IdleOutputTimeout:      Duration(30 * time.Minute),
		MaintenanceInterval:    Duration(10 * time.Minute),
		DeploymentProfile:      ProfileProduction,
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyEnvOverrides(cfg)

	// Capture whether the user explicitly set ImagePullPolicy / SecretsDir
	// before we fill in defaults. "never" / the production secrets path are
	// legitimate explicit choices, so after the default-assignment below we
	// can no longer tell the two apart — the dev-profile overrides must
	// gate on these sentinels, not on the final value.
	explicitPullPolicy := cfg.ImagePullPolicy != ""
	if !explicitPullPolicy {
		cfg.ImagePullPolicy = PullNever
	}

	explicitSecretsDir := cfg.SecretsDir != ""

	// Dev-profile defaults: loosen a handful of tunables for local
	// single-box setups. Only applied when the user did NOT explicitly set
	// the value in YAML or via env (zero / empty sentinel check).
	if cfg.DeploymentProfile == ProfileDev {
		if cfg.WebhookReplaySkewSeconds == 0 {
			cfg.WebhookReplaySkewSeconds = 86400 // 24 h
			cfg.AppliedDevDefaults = append(cfg.AppliedDevDefaults, "webhook_replay_skew_seconds=86400")
		}

		if !explicitPullPolicy {
			cfg.ImagePullPolicy = PullIfNotPresent
			cfg.AppliedDevDefaults = append(cfg.AppliedDevDefaults, "image_pull_policy=if-not-present")
		}

		if !explicitSecretsDir {
			cfg.SecretsDir = chooseDevSecretsDir()
			cfg.AppliedDevDefaults = append(cfg.AppliedDevDefaults, "secrets_dir="+cfg.SecretsDir)
		}
	}

	if !explicitSecretsDir && cfg.SecretsDir == "" {
		// Production fallback: keep the historical /var/run path so
		// k8s/systemd manifests that pre-provision it continue to work
		// without changes. Operators that want a different layout set
		// secrets_dir or CMR_SECRETS_DIR.
		cfg.SecretsDir = defaultProductionSecretsDir
	}

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

// defaultProductionSecretsDir is a filesystem PATH (not a credential),
// hoisted to a const to keep gosec G101 from flagging the literal in the
// default-assignment site.
const defaultProductionSecretsDir = "/var/run/cm-runner/secrets" //nolint:gosec // path, not a credential

// chooseDevSecretsDir returns a user-writable default for SecretsDir in
// dev mode. /var/run is root-owned on Linux and breaks `make run`-style
// local setups, so dev defaults to $XDG_RUNTIME_DIR/cm-runner/secrets
// when XDG_RUNTIME_DIR is set (the standard per-user tmpfs). Without it
// — e.g. a non-systemd daemon user — fall back to a stable path under
// os.TempDir() keyed by uid so concurrent runs by different users don't
// collide.
func chooseDevSecretsDir() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "cm-runner", "secrets")
	}

	return filepath.Join(os.TempDir(), fmt.Sprintf("cm-runner-%d", os.Getuid()), "secrets")
}

func validateServiceURL(field, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%s: invalid URL: %w", field, err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%s: scheme must be http or https", field)
	}

	if u.Hostname() == "" {
		return fmt.Errorf("%s: host is required", field)
	}

	return nil
}

// Validate checks that all required fields are present and valid.
func (c *Config) Validate() error {
	if c.ContextMatrixURL == "" {
		return fmt.Errorf("contextmatrix_url is required")
	}

	if c.ContainerContextMatrixURL == "" {
		c.ContainerContextMatrixURL = c.ContextMatrixURL
	}

	if err := validateServiceURL("contextmatrix_url", c.ContextMatrixURL); err != nil {
		return err
	}

	if err := validateServiceURL("container_contextmatrix_url", c.ContainerContextMatrixURL); err != nil {
		return err
	}

	if c.APIKey == "" {
		return fmt.Errorf("api_key is required")
	}

	if len(c.APIKey) < MinAPIKeyLength {
		return fmt.Errorf("api_key must be at least %d characters", MinAPIKeyLength)
	}

	switch c.ImagePullPolicy {
	case PullAlways, PullNever, PullIfNotPresent:
	default:
		return fmt.Errorf("image_pull_policy must be one of: always, never, if-not-present")
	}

	if c.AgentImage == "" {
		return fmt.Errorf("agent_image is required")
	}

	// Digest-pin agent_image and every allowed_images entry (CTXRUN-044).
	// A mutable tag like `:latest` would let a rebuilt upstream image
	// silently ship into production; require `@sha256:...` so operators
	// roll the agent image intentionally.
	//
	// In dev mode we collect unpinned references instead of failing hard, so
	// local development setups can use mutable tags. The caller (main.go) logs
	// a WARN per entry. Production mode keeps the fail-closed behaviour.
	c.UnpinnedImageRefs = nil

	if err := requireDigestPin("agent_image", c.AgentImage); err != nil {
		if !c.IsDev() {
			return err
		}

		c.UnpinnedImageRefs = append(c.UnpinnedImageRefs, UnpinnedImageRef{Field: "agent_image", Image: c.AgentImage})
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

	if c.WebhookReplayCacheSize < 0 {
		return fmt.Errorf("webhook_replay_cache_size must be positive")
	}

	if c.WebhookReplaySkewSeconds < 0 {
		return fmt.Errorf("webhook_replay_skew_seconds must be positive")
	}

	if c.AdminPort != 0 && (c.AdminPort < 1 || c.AdminPort > 65535) {
		return fmt.Errorf("admin_port must be 0 (disabled) or between 1 and 65535")
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
		c.MaintenanceInterval = Duration(10 * time.Minute)
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

	switch c.GitHub.AuthMode {
	case "app":
		if c.GitHub.App.AppID == 0 {
			return fmt.Errorf("github.app.app_id is required when github.auth_mode is \"app\"")
		}

		if c.GitHub.App.InstallationID == 0 {
			return fmt.Errorf("github.app.installation_id is required when github.auth_mode is \"app\"")
		}

		if c.GitHub.App.PrivateKeyPath == "" {
			return fmt.Errorf("github.app.private_key_path is required when github.auth_mode is \"app\"")
		}

		if _, err := os.Stat(c.GitHub.App.PrivateKeyPath); err != nil {
			return fmt.Errorf("github.app.private_key_path does not exist: %w", err)
		}

		if c.GitHub.PAT.Token != "" {
			return fmt.Errorf("github.pat.token must be empty when github.auth_mode is \"app\"")
		}
	case "pat":
		if c.GitHub.PAT.Token == "" {
			return fmt.Errorf("github.pat.token is required when github.auth_mode is \"pat\"")
		}

		if c.GitHub.App.AppID != 0 || c.GitHub.App.InstallationID != 0 || c.GitHub.App.PrivateKeyPath != "" {
			return fmt.Errorf("github.app.* must be empty when github.auth_mode is \"pat\"")
		}
	default:
		return fmt.Errorf("github.auth_mode is required: must be \"app\" or \"pat\" (got %q)", c.GitHub.AuthMode)
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

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("CMR_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
		}
	}

	if v := os.Getenv("CMR_CONTEXTMATRIX_URL"); v != "" {
		cfg.ContextMatrixURL = v
	}

	if v := os.Getenv("CMR_CONTAINER_CONTEXTMATRIX_URL"); v != "" {
		cfg.ContainerContextMatrixURL = v
	}

	if v := os.Getenv("CMR_API_KEY"); v != "" {
		cfg.APIKey = v
	}

	if v := os.Getenv("CMR_AGENT_IMAGE"); v != "" {
		cfg.AgentImage = v
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

	if v := os.Getenv("CMR_GITHUB_AUTH_MODE"); v != "" {
		cfg.GitHub.AuthMode = v
	}

	if v := os.Getenv("CMR_GITHUB_HOST"); v != "" {
		cfg.GitHub.Host = v
	}

	if v := os.Getenv("CMR_GITHUB_API_BASE_URL"); v != "" {
		cfg.GitHub.APIBaseURL = v
	}

	if v := os.Getenv("CMR_GITHUB_APP_ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.GitHub.App.AppID = n
		}
	}

	if v := os.Getenv("CMR_GITHUB_INSTALLATION_ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.GitHub.App.InstallationID = n
		}
	}

	if v := os.Getenv("CMR_GITHUB_PRIVATE_KEY_PATH"); v != "" {
		cfg.GitHub.App.PrivateKeyPath = v
	}

	if v := os.Getenv("CMR_GITHUB_PAT_TOKEN"); v != "" {
		cfg.GitHub.PAT.Token = v
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
