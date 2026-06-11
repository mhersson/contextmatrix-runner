// Package config handles loading and validation of runner configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mhersson/contextmatrix-runner/internal/secretenv"
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
	Port                      int      `yaml:"port"`
	AdminPort                 int      `yaml:"admin_port"`
	ContextMatrixURL          string   `yaml:"contextmatrix_url"`
	ContainerContextMatrixURL string   `yaml:"container_contextmatrix_url"`
	APIKey                    string   `yaml:"api_key"`
	BaseImage                 string   `yaml:"base_image"`
	AllowedImages             []string `yaml:"allowed_images"`
	ImagePullPolicy           string   `yaml:"image_pull_policy"`
	MaxConcurrent             int      `yaml:"max_concurrent"`
	ContainerTimeout          string   `yaml:"container_timeout"`
	ContainerMemoryLimit      int64    `yaml:"container_memory_limit"`
	ContainerPidsLimit        int64    `yaml:"container_pids_limit"`
	ClaudeAuthDir             string   `yaml:"claude_auth_dir"`
	ClaudeOAuthToken          string   `yaml:"claude_oauth_token"`
	AnthropicAPIKey           string   `yaml:"anthropic_api_key"`
	// CACertFile is an optional host path to a PEM file holding extra CA
	// certificate(s) to trust inside worker containers. When set, the runner
	// bind-mounts the file read-only at /run/cm-ca/ca.crt and sets
	// NODE_EXTRA_CA_CERTS to that path so Claude Code's Node TLS trusts the
	// chain. Only needed behind a TLS-inspecting corporate proxy (e.g. a
	// Zscaler MITM that re-signs api.anthropic.com / the configured
	// ANTHROPIC_BASE_URL with a private root). Empty disables the feature.
	// Env: CMR_CA_CERT_FILE.
	CACertFile     string       `yaml:"ca_cert_file"`
	ClaudeSettings string       `yaml:"claude_settings"`
	GitHub         GitHubConfig `yaml:"github"`
	LogLevel       string       `yaml:"log_level"`
	LogFormat      string       `yaml:"log_format"`
	// SecretsDir is the host directory where the runner stages the shared
	// secrets file. The runner writes $SecretsDir/shared/env (created at
	// boot, rotated by tokenRefresher) and bind-mounts $SecretsDir/shared/
	// read-only into every worker at /run/cm-secrets/. Should be on tmpfs.
	//
	// When empty, the container manager falls back to env-var delivery and
	// folds secrets into Container.Env directly. The fallback is reachable
	// from any deployment (not just tests) but is unsupported in production:
	// secrets land in HostConfig.Env and `docker inspect` will surface them
	// to anyone with read access to the daemon. ApplyDefaults seeds the
	// default path (defaultSecretsDir) so a real production config never
	// hits the fallback unless explicitly cleared.
	SecretsDir string `yaml:"secrets_dir"`
	// TaskSkillsDir is the host path to the curated task skills repo, bind-mounted
	// read-only into worker containers at /host-skills. The entrypoint copies the
	// resolved subset (per CM_TASK_SKILLS env var) into ~/.claude/skills. Empty
	// disables the feature.
	TaskSkillsDir string `yaml:"task_skills_dir"`

	// Webhook replay-protection tunables.
	//
	// WebhookReplaySkewSeconds bounds how stale an incoming X-Webhook-Timestamp
	// may be before the runner rejects the request. Zero is NOT a disable
	// switch: ApplyDefaults overwrites an explicit zero with
	// defaultWebhookReplaySkew so an absent / unset value falls back to the
	// documented default — replay protection is security-critical and must
	// never be silently disabled by a YAML typo or operator mistake. Negative
	// values are rejected by Validate; values above maxWebhookReplaySkewSec
	// (1 week) are rejected as a sanity ceiling. Operators who want to
	// "disable" the check should narrow the window (e.g. 30s) rather than
	// asking for zero.
	WebhookReplayCacheSize   int `yaml:"webhook_replay_cache_size"`
	WebhookReplaySkewSeconds int `yaml:"webhook_replay_skew_seconds"`
	MessageDedupCacheSize    int `yaml:"message_dedup_cache_size"`
	MessageDedupTTLSeconds   int `yaml:"message_dedup_ttl_seconds"`

	// Idle-output watchdog. If a container's logparser
	// has not published any event in this many seconds, the watchdog kills
	// the container with an "idle timeout" reason. Explicit zero disables
	// the watchdog; negative values are rejected by Validate. Unset
	// (i.e. the YAML key is absent and no env override applies) yields the
	// documented default — see defaultIdleOutputTimeout.
	IdleOutputTimeout time.Duration `yaml:"idle_output_timeout"`

	// IdleWatchdogInterval is the poll cadence for the per-container
	// idle-output watchdog. Default 30s (applied when the YAML key is
	// absent and no env override applies). Explicit zero is preserved
	// (mirroring idle_output_timeout / maintenance_interval semantics) so
	// operators can suppress the documented default. Test harnesses shrink
	// to milliseconds to drive fast synthetic idle scenarios without
	// waiting wall-clock seconds. Negative values are rejected by
	// Validate. Env: CMR_IDLE_WATCHDOG_INTERVAL (an explicit "0s"
	// preserves zero).
	IdleWatchdogInterval time.Duration `yaml:"idle_watchdog_interval"`

	// WorkerExtraEnv is a deployment-wide map of additional env vars
	// injected into every spawned worker container. Use sparingly:
	// production deployments shouldn't need these (the entrypoint sets
	// CM_* vars; rotating secrets reach the worker via the shared dir
	// mount at /run/cm-secrets/, sourced from the env file inside).
	// Useful for non-sensitive flags the worker reads (e.g. application-
	// level feature toggles, CI=true).
	//
	// Values are passed verbatim to the container. Keys must be valid
	// shell env-var names (`A-Za-z_` start, `A-Za-z0-9_` continuation).
	// The runner-managed secret-injection keys listed in
	// secretenv.Keys (CM_GIT_TOKEN, CM_MCP_API_KEY,
	// CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY) and a blocklist of
	// dangerous keys (GIT_SSL_NO_VERIFY, LD_PRELOAD, *_PROXY, etc.) are
	// rejected at config-load time — see Validate().
	WorkerExtraEnv map[string]string `yaml:"worker_extra_env"`

	// MaintenanceInterval is the tick interval for the background
	// reconcile-and-prune loop. Each tick runs CleanupOrphans and
	// PruneImages. Explicit zero disables the loop entirely (no ticker is
	// started); negative values are rejected by Validate. Unset (i.e. the
	// YAML key is absent and no env override applies) yields the documented
	// default — see defaultMaintenanceInterval.
	MaintenanceInterval time.Duration `yaml:"maintenance_interval"`

	// UseHMACForVerifyAutonomous toggles whether the VerifyAutonomous
	// callback to CM is HMAC-signed (true, default) or falls back to
	// `Authorization: Bearer <api_key>` (false, deprecated transition
	// mode). Set false ONLY while the ContextMatrix server is being
	// upgraded to accept HMAC on that GET endpoint.
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

// defaultSecretsDir is a filesystem PATH, not a credential. It is named
// via a const to avoid the gosec G101 false-positive on struct literals.
const defaultSecretsDir = "/var/run/cm-runner/secrets" //nolint:gosec // path, not a credential

// Defaults applied by applyDefaults. Promoted to package-level constants so
// the tunables aren't duplicated between Load() and Validate().
const (
	defaultPort                   = 9090
	defaultMaxConcurrent          = 3
	defaultContainerTimeout       = "2h"
	defaultContainerMemoryLimit   = int64(8 * 1024 * 1024 * 1024) // 8 GiB
	defaultContainerPidsLimit     = int64(512)
	defaultLogLevel               = "info"
	defaultWebhookReplayCacheSize = 10000
	defaultWebhookReplaySkew      = 330 // seconds
	defaultMessageDedupCacheSize  = 1000
	defaultMessageDedupTTL        = 600 // seconds
	defaultIdleOutputTimeout      = 30 * time.Minute
	defaultIdleWatchdogInterval   = 30 * time.Second
	defaultMaintenanceInterval    = 10 * time.Minute
)

// Upper bounds for numeric tunables. A typo like
// `max_concurrent: 1000000` would OOM the host at runtime; ranged ceilings
// surface the mistake at config-load time instead of letting operators
// discover it via a crashed runner. Ceilings are picked to comfortably
// exceed any reasonable real-world deployment.
const (
	maxMaxConcurrent          = 256
	maxWebhookReplayCacheSize = 1_000_000
	maxWebhookReplaySkewSec   = 7 * 24 * 60 * 60 // 1 week in seconds
	maxMessageDedupCacheSize  = 1_000_000
	maxMessageDedupTTLSec     = 7 * 24 * 60 * 60               // 1 week in seconds
	maxContainerMemoryLimit   = int64(64 * 1024 * 1024 * 1024) // 64 GiB
	maxContainerPidsLimit     = int64(1 << 16)                 // 65536
	maxIdleOutputTimeout      = 24 * time.Hour
	maxIdleWatchdogInterval   = time.Hour
	maxMaintenanceInterval    = 24 * time.Hour
)

// ApplyDefaults fills zero/empty fields in c with their default values.
// Called once from Load() after YAML parse + env overrides. Validate()
// must NOT inject defaults — it only checks ranges.
//
// ApplyDefaults is safe to call on a partially populated Config literal;
// already-set fields are preserved. Test-only callers that construct
// Config{} directly and want defaulted tunables should call this before
// Validate().
func ApplyDefaults(c *Config) {
	if c.Port == 0 {
		c.Port = defaultPort
	}

	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = defaultMaxConcurrent
	}

	if c.ContainerTimeout == "" {
		c.ContainerTimeout = defaultContainerTimeout
	}

	if c.ContainerMemoryLimit == 0 {
		c.ContainerMemoryLimit = defaultContainerMemoryLimit
	}

	if c.ContainerPidsLimit == 0 {
		c.ContainerPidsLimit = defaultContainerPidsLimit
	}

	if c.LogLevel == "" {
		c.LogLevel = defaultLogLevel
	}

	if c.LogFormat == "" {
		c.LogFormat = LogFormatText
	}

	if c.WebhookReplayCacheSize == 0 {
		c.WebhookReplayCacheSize = defaultWebhookReplayCacheSize
	}

	if c.WebhookReplaySkewSeconds == 0 {
		c.WebhookReplaySkewSeconds = defaultWebhookReplaySkew
	}

	if c.MessageDedupCacheSize == 0 {
		c.MessageDedupCacheSize = defaultMessageDedupCacheSize
	}

	if c.MessageDedupTTLSeconds == 0 {
		c.MessageDedupTTLSeconds = defaultMessageDedupTTL
	}

	if c.SecretsDir == "" {
		c.SecretsDir = defaultSecretsDir
	}

	if c.IdleOutputTimeout == 0 {
		c.IdleOutputTimeout = defaultIdleOutputTimeout
	}

	if c.IdleWatchdogInterval == 0 {
		c.IdleWatchdogInterval = defaultIdleWatchdogInterval
	}

	if c.MaintenanceInterval == 0 {
		c.MaintenanceInterval = defaultMaintenanceInterval
	}

	if c.DeploymentProfile == "" {
		c.DeploymentProfile = ProfileProduction
	}

	// Derive APIBaseURL from Host when only Host is set. This wires the
	// otherwise-dead Host field through to the githubauth consumer in
	// main.go. The "/api/v3" suffix is the standard GitHub Enterprise
	// Server pattern. An explicit api_base_url is preserved so operators
	// can still target non-standard layouts (e.g. GHEC-DR
	// "https://api.acme.ghe.com").
	if c.GitHub.Host != "" && c.GitHub.APIBaseURL == "" {
		host := c.GitHub.Host
		// Strip any user-supplied scheme; we always emit https for the
		// derived URL. Operators wanting plain http must set
		// api_base_url explicitly.
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+len("://"):]
		}

		c.GitHub.APIBaseURL = "https://" + host + "/api/v3"
	}
}

// Load reads a YAML config file and applies environment variable overrides.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	// UseHMACForVerifyAutonomous defaults to true. We set it before
	// unmarshalling so an absent YAML key keeps the safe default; an
	// explicit "false" in YAML still overrides. ApplyDefaults() can't
	// distinguish "explicitly false" from the bool zero value, so the
	// default lives here.
	cfg := &Config{
		UseHMACForVerifyAutonomous: true,
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Detect "explicit zero" for the duration knobs whose contract is
	// "zero disables / overrides default". yaml.v3 cannot distinguish an
	// absent key from a key set to 0s once it has decoded into
	// time.Duration, so we round-trip the raw YAML through a permissive
	// presence-detecting decode before applyEnvOverrides runs. After
	// ApplyDefaults clobbers 0 with the documented default, we restore the
	// original zero if the key was set explicitly (in YAML or via env).
	explicitIdleOutputTimeout, explicitIdleWatchdogInterval, explicitMaintenanceInterval, err := detectExplicitDurations(data)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := applyEnvOverrides(cfg); err != nil {
		return nil, fmt.Errorf("apply env overrides: %w", err)
	}

	// Env overrides also count as "explicit". An operator setting
	// CMR_IDLE_OUTPUT_TIMEOUT=0s expects the watchdog to be disabled.
	if _, ok := os.LookupEnv("CMR_IDLE_OUTPUT_TIMEOUT"); ok {
		explicitIdleOutputTimeout = true
	}

	if _, ok := os.LookupEnv("CMR_IDLE_WATCHDOG_INTERVAL"); ok {
		explicitIdleWatchdogInterval = true
	}

	if _, ok := os.LookupEnv("CMR_MAINTENANCE_INTERVAL"); ok {
		explicitMaintenanceInterval = true
	}

	// Capture whether the user explicitly set ImagePullPolicy before we fill
	// in a default. "never" is a legitimate explicit choice, so after the
	// default-assignment below we can no longer tell the two apart — the
	// dev-profile override must gate on this sentinel, not on the final value.
	explicitPullPolicy := cfg.ImagePullPolicy != ""
	if !explicitPullPolicy {
		cfg.ImagePullPolicy = PullNever
	}

	// Stash whether WebhookReplaySkewSeconds was set explicitly before
	// ApplyDefaults overwrites the zero value. Dev-mode applies its own
	// default when the user did not opt in.
	explicitSkew := cfg.WebhookReplaySkewSeconds != 0

	// Stash the pre-default values so we can restore an explicit zero after
	// ApplyDefaults overwrites it with the documented default.
	preDefaultIdle := cfg.IdleOutputTimeout
	preDefaultIdleInterval := cfg.IdleWatchdogInterval
	preDefaultMaint := cfg.MaintenanceInterval

	ApplyDefaults(cfg)

	// Restore explicit-zero (the "disables" contract). An explicit positive
	// value survives ApplyDefaults unchanged and needs no restore.
	if explicitIdleOutputTimeout && preDefaultIdle == 0 {
		cfg.IdleOutputTimeout = 0
	}

	if explicitIdleWatchdogInterval && preDefaultIdleInterval == 0 {
		cfg.IdleWatchdogInterval = 0
	}

	if explicitMaintenanceInterval && preDefaultMaint == 0 {
		cfg.MaintenanceInterval = 0
	}

	// Dev-profile defaults: loosen a handful of tunables for local
	// single-box setups. Only applied when the user did NOT explicitly set
	// the value in YAML or via env.
	if cfg.DeploymentProfile == ProfileDev {
		if !explicitSkew {
			cfg.WebhookReplaySkewSeconds = 86400 // 24 h
			cfg.AppliedDevDefaults = append(cfg.AppliedDevDefaults, "webhook_replay_skew_seconds=86400")
		}

		if !explicitPullPolicy {
			cfg.ImagePullPolicy = PullIfNotPresent
			cfg.AppliedDevDefaults = append(cfg.AppliedDevDefaults, "image_pull_policy=if-not-present")
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

// detectExplicitDurations returns whether each of the explicit-zero-honouring
// duration knobs (idle_output_timeout, idle_watchdog_interval,
// maintenance_interval) was present (set to any value, including 0s) in the
// raw YAML body. Used by Load to honour the "explicit zero disables /
// overrides default" contract — see the IdleOutputTimeout,
// IdleWatchdogInterval and MaintenanceInterval field comments. A minimal
// map[string]any decode is sufficient because we only need key presence, not
// value validation.
func detectExplicitDurations(data []byte) (idleSet, idleIntervalSet, maintSet bool, err error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return false, false, false, err
	}

	_, idleSet = raw["idle_output_timeout"]
	_, idleIntervalSet = raw["idle_watchdog_interval"]
	_, maintSet = raw["maintenance_interval"]

	return idleSet, idleIntervalSet, maintSet, nil
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

	// Mirror the tracing endpoint check: userinfo in a service URL leaks
	// credentials into request logs and into any traced span. Reject up
	// front instead of letting it ride through the HTTP client.
	if u.User != nil {
		return fmt.Errorf("%s: must not embed userinfo credentials", field)
	}

	return nil
}

// Validate checks that all required fields are present and valid.
//
// Validate must NOT inject numeric / string defaults — those live in
// ApplyDefaults(), called from Load() after env overrides. Test-only
// callers that construct Config{} literals directly should call
// ApplyDefaults() explicitly if they want defaulted tunables.
//
// As an exception, Validate() does set two derived fields:
//   - ContainerContextMatrixURL falls back to ContextMatrixURL when empty.
//   - containerTimeoutDuration is parsed and cached from ContainerTimeout.
//
// Both are derivations, not numeric defaults.
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

	if c.GitHub.APIBaseURL != "" {
		if err := validateServiceURL("github.api_base_url", c.GitHub.APIBaseURL); err != nil {
			return err
		}
	}

	if c.GitHub.Host != "" {
		// GitHub.Host accepts either a bare hostname or a URL. validateServiceURL
		// requires a scheme; if Host is given without one we synthesise an https
		// URL so the same hostname / scheme checks apply. A bare hostname like
		// "ghe.example.com" is the documented common case.
		hostForCheck := c.GitHub.Host
		if !strings.Contains(hostForCheck, "://") {
			hostForCheck = "https://" + hostForCheck
		}

		if err := validateServiceURL("github.host", hostForCheck); err != nil {
			return err
		}
	}

	if c.APIKey == "" {
		return fmt.Errorf("api_key is required")
	}

	if len(c.APIKey) < MinAPIKeyLength {
		return fmt.Errorf("api_key must be at least %d characters", MinAPIKeyLength)
	}

	// Restrict APIKey to printable ASCII (0x21..0x7E). The deprecated
	// VerifyAutonomous Bearer fallback ships the key as
	// `Authorization: Bearer <apiKey>` over an HTTP header; a stray CR/LF
	// or non-printable character there would corrupt the request and
	// (worse) could be a header-injection vector. Reject early.
	for i, r := range c.APIKey {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("api_key contains invalid character at position %d (must be printable ASCII 0x21-0x7E)", i)
		}
	}

	if c.BaseImage == "" {
		return fmt.Errorf("base_image is required")
	}

	// Digest-pin base_image and every allowed_images entry. A mutable tag
	// like `:latest` would let a rebuilt upstream image silently ship into
	// production; require `@sha256:...` so operators roll base images
	// intentionally.
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

	if c.MaxConcurrent > maxMaxConcurrent {
		return fmt.Errorf("max_concurrent must not exceed %d (got %d)", maxMaxConcurrent, c.MaxConcurrent)
	}

	// Replay-protection tunables and resource limits only need range
	// checks here; ApplyDefaults() (called from Load) has already filled
	// in zero values with the documented defaults. A negative value
	// indicates operator typo and must surface, while a literal zero is
	// only reachable from tests that constructed Config{} directly.
	if c.WebhookReplayCacheSize < 0 {
		return fmt.Errorf("webhook_replay_cache_size must not be negative (zero means apply default)")
	}

	if c.WebhookReplayCacheSize > maxWebhookReplayCacheSize {
		return fmt.Errorf("webhook_replay_cache_size must not exceed %d (got %d)", maxWebhookReplayCacheSize, c.WebhookReplayCacheSize)
	}

	if c.WebhookReplaySkewSeconds < 0 {
		return fmt.Errorf("webhook_replay_skew_seconds must not be negative (zero means apply default)")
	}

	if c.WebhookReplaySkewSeconds > maxWebhookReplaySkewSec {
		return fmt.Errorf("webhook_replay_skew_seconds must not exceed %d (got %d)", maxWebhookReplaySkewSec, c.WebhookReplaySkewSeconds)
	}

	if c.MessageDedupCacheSize < 0 {
		return fmt.Errorf("message_dedup_cache_size must not be negative (zero means apply default)")
	}

	if c.MessageDedupCacheSize > maxMessageDedupCacheSize {
		return fmt.Errorf("message_dedup_cache_size must not exceed %d (got %d)", maxMessageDedupCacheSize, c.MessageDedupCacheSize)
	}

	if c.MessageDedupTTLSeconds < 0 {
		return fmt.Errorf("message_dedup_ttl_seconds must not be negative (zero means apply default)")
	}

	if c.MessageDedupTTLSeconds > maxMessageDedupTTLSec {
		return fmt.Errorf("message_dedup_ttl_seconds must not exceed %d (got %d)", maxMessageDedupTTLSec, c.MessageDedupTTLSeconds)
	}

	if c.ContainerMemoryLimit < 0 {
		return fmt.Errorf("container_memory_limit must not be negative (zero means apply default)")
	}

	if c.ContainerMemoryLimit > maxContainerMemoryLimit {
		return fmt.Errorf("container_memory_limit must not exceed %d bytes / 64 GiB (got %d)", maxContainerMemoryLimit, c.ContainerMemoryLimit)
	}

	if c.ContainerPidsLimit < 0 {
		return fmt.Errorf("container_pids_limit must not be negative (zero means apply default)")
	}

	if c.ContainerPidsLimit > maxContainerPidsLimit {
		return fmt.Errorf("container_pids_limit must not exceed %d (got %d)", maxContainerPidsLimit, c.ContainerPidsLimit)
	}

	// Port range check. A typo like CMR_PORT=-1 or CMR_PORT=70000 must
	// surface here, symmetric with AdminPort below. Zero is tolerated for
	// the same reason as the other "zero means apply default" tunables —
	// test-only callers that construct Config{} literals directly and skip
	// ApplyDefaults would otherwise have to fill Port explicitly.
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535 (got %d; zero means apply default)", c.Port)
	}

	if c.AdminPort != 0 && (c.AdminPort < 1 || c.AdminPort > 65535) {
		return fmt.Errorf("admin_port must be 0 (disabled) or between 1 and 65535")
	}

	// Two listeners cannot share a port. Catch the collision at config-load
	// time so a misconfigured operator sees the error before bind, instead
	// of one of the two HTTP servers failing to ListenAndServe and bringing
	// the runner down via os.Exit(1) deeper in startup.
	if c.AdminPort != 0 && c.AdminPort == c.Port {
		return fmt.Errorf("admin_port must differ from port (both set to %d)", c.Port)
	}

	// Normalise log_format to lowercase before validation so an operator
	// writing "JSON" or "Text" in YAML gets the same treatment as the
	// case-insensitive log_level check below; the field is then stored in
	// canonical lowercase form so newLogger's equality check
	// (LogFormat == LogFormatJSON) routes correctly.
	c.LogFormat = strings.ToLower(c.LogFormat)
	switch c.LogFormat {
	case "", LogFormatText, LogFormatJSON:
		// ok
	default:
		return fmt.Errorf("log_format must be one of: text, json")
	}

	// LogLevelSlog silently swallows UnmarshalText errors and defaults to
	// Info; verifying the allowlist here surfaces the typo at boot rather
	// than running production silently above the operator's intended
	// verbosity. The values match slog.Level's UnmarshalText accept set.
	switch strings.ToLower(c.LogLevel) {
	case "", "debug", "info", "warn", "error":
		// ok (ApplyDefaults normalises empty to "info")
	default:
		return fmt.Errorf("log_level must be one of: debug, info, warn, error (got %q)", c.LogLevel)
	}

	switch c.DeploymentProfile {
	case "", ProfileProduction, ProfileDev:
		// ok (ApplyDefaults normalises empty to production)
	default:
		return fmt.Errorf("deployment_profile must be one of: production, dev")
	}

	d, err := time.ParseDuration(c.ContainerTimeout)
	if err != nil {
		return fmt.Errorf("container_timeout is invalid: %w", err)
	}

	c.containerTimeoutDuration = d

	// Idle-output watchdog. Negative values are an error.
	// Zero is legal and disables the watchdog.
	if c.IdleOutputTimeout < 0 {
		return fmt.Errorf("idle_output_timeout must be zero or positive")
	}

	if c.IdleOutputTimeout > maxIdleOutputTimeout {
		return fmt.Errorf("idle_output_timeout must not exceed %s (got %s)", maxIdleOutputTimeout, c.IdleOutputTimeout)
	}

	if c.IdleWatchdogInterval < 0 {
		return fmt.Errorf("idle_watchdog_interval must not be negative (zero means apply default)")
	}

	if c.IdleWatchdogInterval > maxIdleWatchdogInterval {
		return fmt.Errorf("idle_watchdog_interval must not exceed %s (got %s)", maxIdleWatchdogInterval, c.IdleWatchdogInterval)
	}

	// Worker extra env: validate key shape and reject collisions with
	// secrets-file vars. These restrictions exist so a misconfigured
	// extra-env map can't accidentally shadow the secret-injection path
	// (the entrypoint sources the env file from the shared dir mounted
	// at /run/cm-secrets/). The runner-managed secret list lives in
	// secretenv.Keys so logparser, config and any future consumer stay
	// in lockstep.
	for k := range c.WorkerExtraEnv {
		if !validEnvKey(k) {
			return fmt.Errorf("worker_extra_env key %q is not a valid env var name", k)
		}

		if secretenv.Contains(k) {
			// CM_MCP_API_KEY is per-container; the other entries are
			// sourced from the shared secrets file. Either way a YAML
			// override would shadow runner-managed values, so we reject
			// the whole runner-managed set.
			if k == "CM_MCP_API_KEY" {
				return fmt.Errorf("worker_extra_env key %q collides with a per-container env var; remove it", k)
			}

			return fmt.Errorf("worker_extra_env key %q collides with a secrets-file var; remove it", k)
		}

		switch k {
		case "GIT_SSL_NO_VERIFY", "GIT_TERMINAL_PROMPT",
			"LD_PRELOAD", "LD_LIBRARY_PATH",
			"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
			"NODE_OPTIONS",
			"NODE_EXTRA_CA_CERTS",
			"PATH",
			"GOPROXY", "GOSUMDB", "GOFLAGS",
			"PYTHONPATH":
			return fmt.Errorf("worker_extra_env key %q is unsafe (would let YAML override worker behaviour); remove it", k)
		}
	}

	if c.MaintenanceInterval < 0 {
		return fmt.Errorf("maintenance_interval must not be negative (zero means apply default)")
	}

	if c.MaintenanceInterval > maxMaintenanceInterval {
		return fmt.Errorf("maintenance_interval must not exceed %s (got %s)", maxMaintenanceInterval, c.MaintenanceInterval)
	}

	if c.ClaudeAuthDir == "" && c.ClaudeOAuthToken == "" && c.AnthropicAPIKey == "" {
		return fmt.Errorf("at least one of claude_auth_dir, claude_oauth_token, or anthropic_api_key is required")
	}

	if c.ClaudeAuthDir != "" {
		if _, err := os.Stat(c.ClaudeAuthDir); err != nil {
			return fmt.Errorf("claude_auth_dir does not exist: %w", err)
		}
	}

	if c.CACertFile != "" {
		if _, err := os.Stat(c.CACertFile); err != nil {
			return fmt.Errorf("ca_cert_file does not exist: %w", err)
		}
	}

	// Static-env secrets bypass tokenRefresher's runtime byte filter — they
	// flow directly into the worker's env file. Reject control bytes /
	// NUL / non-printable ASCII here so a typo in YAML cannot inject a
	// stray newline into the credential helper printf path (the same
	// reasoning as secrets_refresher.go's GitHub-token charset check).
	if c.ClaudeOAuthToken != "" {
		if err := validatePrintableASCIISecret("claude_oauth_token", c.ClaudeOAuthToken); err != nil {
			return err
		}
	}

	if c.AnthropicAPIKey != "" {
		if err := validatePrintableASCIISecret("anthropic_api_key", c.AnthropicAPIKey); err != nil {
			return err
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

// applyEnvOverrides applies CMR_* environment variables on top of cfg.
// Parse errors are accumulated and returned so an operator typo (e.g.
// CMR_PORT=70x0) surfaces at startup rather than silently falling back
// to the YAML / default. Each integer-valued override that fails to
// parse contributes one wrapped error.
func applyEnvOverrides(cfg *Config) error {
	var errs []error

	parseInt := func(envName string, dst *int) {
		v := os.Getenv(envName)
		if v == "" {
			return
		}

		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: parse int %q: %w", envName, v, err))

			return
		}

		*dst = n
	}

	parseInt64 := func(envName string, dst *int64) {
		v := os.Getenv(envName)
		if v == "" {
			return
		}

		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: parse int64 %q: %w", envName, v, err))

			return
		}

		*dst = n
	}

	parseDuration := func(envName string, dst *time.Duration) {
		v := os.Getenv(envName)
		if v == "" {
			return
		}

		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: parse duration %q: %w", envName, v, err))

			return
		}

		*dst = d
	}

	parseBool := func(envName string, dst *bool) {
		v := os.Getenv(envName)
		if v == "" {
			return
		}

		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: parse bool %q: %w", envName, v, err))

			return
		}

		*dst = b
	}

	// Core service config.
	parseInt("CMR_PORT", &cfg.Port)
	parseInt("CMR_ADMIN_PORT", &cfg.AdminPort)

	if v := os.Getenv("CMR_CONTEXTMATRIX_URL"); v != "" {
		cfg.ContextMatrixURL = v
	}

	if v := os.Getenv("CMR_CONTAINER_CONTEXTMATRIX_URL"); v != "" {
		cfg.ContainerContextMatrixURL = v
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

	parseInt("CMR_MAX_CONCURRENT", &cfg.MaxConcurrent)

	if v := os.Getenv("CMR_CONTAINER_TIMEOUT"); v != "" {
		cfg.ContainerTimeout = v
	}

	parseInt64("CMR_CONTAINER_MEMORY_LIMIT", &cfg.ContainerMemoryLimit)
	parseInt64("CMR_CONTAINER_PIDS_LIMIT", &cfg.ContainerPidsLimit)

	// Claude auth / settings.
	if v := os.Getenv("CMR_CLAUDE_AUTH_DIR"); v != "" {
		cfg.ClaudeAuthDir = v
	}

	if v := os.Getenv("CMR_CLAUDE_OAUTH_TOKEN"); v != "" {
		cfg.ClaudeOAuthToken = v
	}

	if v := os.Getenv("CMR_ANTHROPIC_API_KEY"); v != "" {
		cfg.AnthropicAPIKey = v
	}

	if v := os.Getenv("CMR_CA_CERT_FILE"); v != "" {
		cfg.CACertFile = v
	}

	if v := os.Getenv("CMR_CLAUDE_SETTINGS"); v != "" {
		cfg.ClaudeSettings = v
	}

	// GitHub.
	if v := os.Getenv("CMR_GITHUB_AUTH_MODE"); v != "" {
		cfg.GitHub.AuthMode = v
	}

	if v := os.Getenv("CMR_GITHUB_HOST"); v != "" {
		cfg.GitHub.Host = v
	}

	if v := os.Getenv("CMR_GITHUB_API_BASE_URL"); v != "" {
		cfg.GitHub.APIBaseURL = v
	}

	parseInt64("CMR_GITHUB_APP_ID", &cfg.GitHub.App.AppID)
	parseInt64("CMR_GITHUB_INSTALLATION_ID", &cfg.GitHub.App.InstallationID)

	if v := os.Getenv("CMR_GITHUB_PRIVATE_KEY_PATH"); v != "" {
		cfg.GitHub.App.PrivateKeyPath = v
	}

	if v := os.Getenv("CMR_GITHUB_PAT_TOKEN"); v != "" {
		cfg.GitHub.PAT.Token = v
	}

	// Logging.
	if v := os.Getenv("CMR_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}

	if v := os.Getenv("CMR_LOG_FORMAT"); v != "" {
		cfg.LogFormat = v
	}

	// Secrets and skills directories.
	if v := os.Getenv("CMR_SECRETS_DIR"); v != "" {
		cfg.SecretsDir = v
	}

	if v := os.Getenv("CMR_TASK_SKILLS_DIR"); v != "" {
		cfg.TaskSkillsDir = v
	}

	// Replay-protection tunables.
	parseInt("CMR_WEBHOOK_REPLAY_CACHE_SIZE", &cfg.WebhookReplayCacheSize)
	parseInt("CMR_WEBHOOK_REPLAY_SKEW_SECONDS", &cfg.WebhookReplaySkewSeconds)
	parseInt("CMR_MESSAGE_DEDUP_CACHE_SIZE", &cfg.MessageDedupCacheSize)
	parseInt("CMR_MESSAGE_DEDUP_TTL_SECONDS", &cfg.MessageDedupTTLSeconds)

	// Idle watchdog / maintenance cadences (Go duration strings, e.g. "30s").
	parseDuration("CMR_IDLE_OUTPUT_TIMEOUT", &cfg.IdleOutputTimeout)
	parseDuration("CMR_IDLE_WATCHDOG_INTERVAL", &cfg.IdleWatchdogInterval)
	parseDuration("CMR_MAINTENANCE_INTERVAL", &cfg.MaintenanceInterval)

	// Bool tunables.
	parseBool("CMR_USE_HMAC_FOR_VERIFY_AUTONOMOUS", &cfg.UseHMACForVerifyAutonomous)

	// Deployment profile.
	if v := os.Getenv("CMR_DEPLOYMENT_PROFILE"); v != "" {
		cfg.DeploymentProfile = v
	}

	// WorkerExtraEnv: comma-separated list of KEY=VALUE pairs. Empty entries
	// are skipped. An entry without '=' is a parse error. The map shape lets
	// operators add overrides via env without rewriting the whole YAML map,
	// at the cost of disallowing values that themselves contain commas.
	if v := os.Getenv("CMR_WORKER_EXTRA_ENV"); v != "" {
		if cfg.WorkerExtraEnv == nil {
			cfg.WorkerExtraEnv = make(map[string]string)
		}

		for _, kv := range strings.Split(v, ",") {
			kv = strings.TrimSpace(kv)
			if kv == "" {
				continue
			}

			k, val, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				errs = append(errs, fmt.Errorf("CMR_WORKER_EXTRA_ENV: entry %q is not KEY=VALUE", kv))

				continue
			}

			cfg.WorkerExtraEnv[k] = val
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

// validatePrintableASCIISecret rejects values that contain bytes outside
// printable ASCII (0x20..0x7E). The runner injects these values into the
// worker's env file as `export KEY='<value>'`; a stray CR/LF, tab, or
// control byte in the value could break out of the single-quoted shell
// context and emit a second shell command. Reject early instead of relying
// on downstream escaping.
//
// The accepted range matches container.IsValidStaticSecretByte exactly so
// the boot-time config check and the runtime secrets-refresher gate accept
// the same character set — a value that passes here must not be rejected
// at refresh time, otherwise an operator sees a green boot followed by a
// silent refresh failure on the first token rotation.
//
// Field is the YAML key the caller used (e.g. "claude_oauth_token") so the
// returned error tells the operator which value is bad without revealing
// the secret itself.
func validatePrintableASCIISecret(field, value string) error {
	for i := range len(value) {
		c := value[i]
		// Reject NUL, ASCII control characters (0x00..0x1F), DEL
		// (0x7F), and high-bit bytes (0x80+) — none of which appear in
		// any Claude / Anthropic token format and any of which could
		// break shell quoting. Space (0x20) is accepted because it
		// survives single-quoted shell context unchanged and the
		// container-side gate accepts it.
		if c < 0x20 || c > 0x7e {
			return fmt.Errorf("%s contains invalid byte at position %d (must be printable ASCII 0x20-0x7E)", field, i)
		}
	}

	return nil
}

// validEnvKey returns true for strings that are valid POSIX env-var
// names: must start with [A-Za-z_], remainder [A-Za-z0-9_]. Empty
// strings are rejected.
func validEnvKey(s string) bool {
	if s == "" {
		return false
	}

	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}

	return true
}
