package container

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// testBuildSecretDeliveryManager returns a minimal Manager for testing
// buildSecretDelivery. Docker fields are not wired; buildSecretDelivery only
// reads cfg and calls m.token.GenerateToken in env-var mode.
func testBuildSecretDeliveryManager(t *testing.T, cfg *config.Config) *Manager {
	t.Helper()

	return &Manager{
		cfg:      cfg,
		logger:   testLogger(),
		token:    testPATProvider(t),
		dnsCache: newDNSCache(dnsCacheTTL, dnsCacheCapacity),
		resolver: net.DefaultResolver,
		mkdirAll: os.MkdirAll,
		createFile: func(path string) (*os.File, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL|os.O_TRUNC, 0o600)
		},
	}
}

// TestBuildSecretDelivery_FileMode verifies that when SecretsDir is set,
// buildSecretDelivery returns secretModeFile pointing at SecretsDir/shared.
func TestBuildSecretDelivery_FileMode(t *testing.T) {
	cfg := &config.Config{
		SecretsDir: t.TempDir(),
	}

	m := testBuildSecretDeliveryManager(t, cfg)

	delivery, err := m.buildSecretDelivery(context.Background())
	require.NoError(t, err)

	assert.Equal(t, secretModeFile, delivery.Mode)
	assert.Equal(t, filepath.Join(cfg.SecretsDir, "shared"), delivery.FilePath,
		"FilePath must be SecretsDir/shared")
	assert.Empty(t, delivery.EnvVars, "EnvVars must be empty in file mode")
}

// TestBuildSecretDelivery_EnvVarMode verifies that when SecretsDir is empty,
// buildSecretDelivery mints a token and returns secretModeEnvVar with all
// required env vars.
func TestBuildSecretDelivery_EnvVarMode(t *testing.T) {
	cfg := &config.Config{
		SecretsDir:      "",
		AnthropicAPIKey: "sk-test",
	}

	m := testBuildSecretDeliveryManager(t, cfg)

	delivery, err := m.buildSecretDelivery(context.Background())
	require.NoError(t, err)

	assert.Equal(t, secretModeEnvVar, delivery.Mode)
	assert.Empty(t, delivery.FilePath, "FilePath must be empty in env-var mode")
	assert.NotEmpty(t, delivery.EnvVars, "EnvVars must be populated in env-var mode")

	var hasGitToken, hasAnthropicKey bool

	for _, e := range delivery.EnvVars {
		if strings.HasPrefix(e, "CM_GIT_TOKEN=") {
			hasGitToken = true
		}

		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
			hasAnthropicKey = true
		}
	}

	assert.True(t, hasGitToken, "CM_GIT_TOKEN must be in EnvVars for env-var delivery")
	assert.True(t, hasAnthropicKey, "ANTHROPIC_API_KEY must be in EnvVars for env-var delivery")
}

// TestBuildSecretDelivery_EnvVarMode_OAuthToken verifies that OAuth token
// takes priority over Anthropic API key in env-var delivery mode.
func TestBuildSecretDelivery_EnvVarMode_OAuthToken(t *testing.T) {
	cfg := &config.Config{
		SecretsDir:       "",
		ClaudeOAuthToken: "oauth-tok",
		AnthropicAPIKey:  "sk-fallback",
	}

	m := testBuildSecretDeliveryManager(t, cfg)

	delivery, err := m.buildSecretDelivery(context.Background())
	require.NoError(t, err)

	assert.Equal(t, secretModeEnvVar, delivery.Mode)

	var hasOAuth, hasAnthropicKey bool

	for _, e := range delivery.EnvVars {
		if strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			hasOAuth = true
		}

		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
			hasAnthropicKey = true
		}
	}

	assert.True(t, hasOAuth, "CLAUDE_CODE_OAUTH_TOKEN must be in EnvVars when OAuthToken is set")
	assert.False(t, hasAnthropicKey, "ANTHROPIC_API_KEY must not appear when OAuthToken takes priority")
}

// TestSecretsDelivered_EnvVarMode_IntegrationStartContainer verifies that when
// SecretsDir is empty (the env-var fallback path used in tests without a shared
// dir), all secrets appear in container.Config.Env and the secrets bind-mount
// is NOT added to HostConfig.Mounts.
func TestSecretsDelivered_EnvVarMode_IntegrationStartContainer(t *testing.T) {
	var (
		capturedEnv    []string
		capturedMounts []string // Target paths of all mounts
	)

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *dockercontainer.Config, hc *dockercontainer.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (dockercontainer.CreateResponse, error) {
		capturedEnv = cfg.Env

		for _, mn := range hc.Mounts {
			capturedMounts = append(capturedMounts, mn.Target)
		}

		return dockercontainer.CreateResponse{ID: "envvar-ctr"}, nil
	}

	// SecretsDir is empty → env-var delivery path.
	envCfg := &config.Config{
		DeploymentProfile: config.ProfileDev,
		BaseImage:         "test-image:latest",
		SecretsDir:        "", // triggers env-var fallback
		AnthropicAPIKey:   "sk-test-api-key",
		ImagePullPolicy:   config.PullAlways,
		ContainerTimeout:  "1h",
	}
	envCfg.ParseContainerTimeout()

	tr := tracker.New()
	m := NewManager(mock, tr, nil, testPATProvider(t), nil, envCfg, testLogger())

	payload := RunConfig{
		Mode:    ModeTask,
		CardID:  "TEST-042",
		Project: "envvar-proj",
		RepoURL: "https://github.com/org/repo.git",
		MCPURL:  "http://cm:8080/mcp",
	}

	delivery, secretValues, err := startContainerAndCapture(t, m, payload, tr)
	require.NoError(t, err)
	assert.Equal(t, secretModeEnvVar, delivery.Mode)
	assert.NotEmpty(t, secretValues)

	// Secrets must be in Env.
	var foundToken bool

	for _, e := range capturedEnv {
		if strings.HasPrefix(e, "CM_GIT_TOKEN=") {
			foundToken = true
		}
	}

	assert.True(t, foundToken, "CM_GIT_TOKEN must be in container Env for env-var delivery")

	// The secrets bind-mount must NOT be present in env-var mode.
	for _, target := range capturedMounts {
		assert.NotEqual(t, secretsMountTarget, target,
			"secrets bind-mount must not be added in env-var delivery mode")
	}
}

// startContainerAndCapture calls startContainer and returns the secretDelivery
// and secretValues so callers can assert on delivery mode without going through
// the full Run() lifecycle.
func startContainerAndCapture(t *testing.T, m *Manager, payload RunConfig, tr *tracker.Tracker) (secretDelivery, []string, error) {
	t.Helper()

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	_, delivery, secretValues, err := m.startContainer(context.Background(), payload)

	return delivery, secretValues, err
}
