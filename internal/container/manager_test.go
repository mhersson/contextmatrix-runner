package container

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/callback"
	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/github"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

var (
	cachedKey     *rsa.PrivateKey
	cachedKeyOnce sync.Once
)

func testRSAKey() *rsa.PrivateKey {
	cachedKeyOnce.Do(func() {
		var err error
		cachedKey, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
	})
	return cachedKey
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig() *config.Config {
	cfg := &config.Config{
		BaseImage:        "test-image:latest",
		ContainerTimeout: "1h",
		AnthropicAPIKey:  "sk-test",
	}
	// Parse the container timeout duration without full validation.
	cfg.ParseContainerTimeout()
	return cfg
}

// testTokenProvider creates a mock GitHub token server and TokenProvider.
func testTokenProvider(t *testing.T) github.TokenGenerator {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ghs_test_token",
			"expires_at": "2030-01-01T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)

	tp, err := github.NewTokenProviderWithKey(12345, 67890, testRSAKey(), srv.URL)
	require.NoError(t, err)
	return tp
}

func testPATProvider(t *testing.T) github.TokenGenerator {
	t.Helper()
	p, err := github.NewPATProvider("ghp_test_pat")
	require.NoError(t, err)
	return p
}

func testPayload() RunConfig {
	return RunConfig{
		CardID:  "PROJ-042",
		Project: "my-project",
		RepoURL: "git@github.com:org/repo.git",
		MCPURL:  "http://cm:8080/mcp",
	}
}

func TestRun_Success(t *testing.T) {
	var createdEnv []string
	var createdLabels map[string]string
	var reportedStatuses []string

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
			assert.Equal(t, "test-image:latest", ref)
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerCreateFn: func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
			createdEnv = cfg.Env
			createdLabels = cfg.Labels
			assert.Contains(t, name, "cmr-")
			return container.CreateResponse{ID: "test-ctr-123"}, nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}
			return ch, make(chan error)
		},
	}

	// Track reported statuses.
	origCbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ RunnerStatus string `json:"runner_status"` }
		_ = json.Unmarshal(body, &req)
		reportedStatuses = append(reportedStatuses, req.RunnerStatus)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer origCbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(origCbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:    payload.CardID,
		Project:   payload.Project,
		StartedAt: time.Now(),
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	// Container should have been removed from tracker.
	assert.Equal(t, 0, tr.Count())

	// Verify env vars.
	assert.Contains(t, createdEnv, "CM_CARD_ID=PROJ-042")
	assert.Contains(t, createdEnv, "CM_PROJECT=my-project")
	assert.Contains(t, createdEnv, "CM_MCP_URL=http://cm:8080/mcp")
	assert.Contains(t, createdEnv, "CM_REPO_URL=https://github.com/org/repo.git")
	assert.Contains(t, createdEnv, "ANTHROPIC_API_KEY=sk-test")

	// Verify labels.
	assert.Equal(t, "true", createdLabels[LabelRunner])
	assert.Equal(t, "PROJ-042", createdLabels[LabelCardID])
	assert.Equal(t, "my-project", createdLabels[LabelProject])

	// Should have reported "running".
	assert.Contains(t, reportedStatuses, "running")
}

func TestRun_PATProvider(t *testing.T) {
	var createdEnv []string

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerCreateFn: func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			createdEnv = cfg.Env
			return container.CreateResponse{ID: "pat-test-ctr"}, nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}
			return ch, make(chan error)
		},
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	assert.Contains(t, createdEnv, "CM_GIT_TOKEN=ghp_test_pat")
}

func TestRun_NonZeroExit(t *testing.T) {
	var reportedStatuses []string

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ RunnerStatus string `json:"runner_status"` }
		_ = json.Unmarshal(body, &req)
		reportedStatuses = append(reportedStatuses, req.RunnerStatus)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 1}
			return ch, make(chan error)
		},
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	assert.Contains(t, reportedStatuses, "failed")
	assert.Equal(t, 0, tr.Count())
}

func TestRun_ImagePullFailure(t *testing.T) {
	var failureReported atomic.Bool

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ RunnerStatus string `json:"runner_status"` }
		_ = json.Unmarshal(body, &req)
		if req.RunnerStatus == "failed" {
			failureReported.Store(true)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return nil, fmt.Errorf("image not found")
		},
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	assert.True(t, failureReported.Load(), "should report failure on image pull error")
	assert.Equal(t, 0, tr.Count())
}

func TestRun_CustomImage(t *testing.T) {
	var pulledImage string

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
			pulledImage = ref
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}
			return ch, make(chan error)
		},
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	cfg := testConfig()
	cfg.AllowedImages = []string{"test-image:latest", "custom/image:v2"}
	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	payload := testPayload()
	payload.RunnerImage = "custom/image:v2"
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	assert.Equal(t, "custom/image:v2", pulledImage)
}

func TestKill(t *testing.T) {
	mock := &MockDockerClient{}
	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())

	canceled := false
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:      "PROJ-001",
		Project:     "proj",
		ContainerID: "ctr-123",
		Cancel:      func() { canceled = true },
	}))

	err := mgr.Kill("proj", "PROJ-001")
	require.NoError(t, err)
	assert.True(t, canceled)
}

func TestKill_NotFound(t *testing.T) {
	mock := &MockDockerClient{}
	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())

	err := mgr.Kill("proj", "PROJ-999")
	assert.ErrorContains(t, err, "no container tracked")
}

func TestCleanupOrphans(t *testing.T) {
	var removedIDs []string
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
			return []DockerContainer{
				{ID: "orphan-1", Labels: map[string]string{LabelCardID: "A-001", LabelProject: "proj"}},
				{ID: "orphan-2", Labels: map[string]string{LabelCardID: "A-002", LabelProject: "proj"}},
			}, nil
		},
		ContainerRemoveFn: func(_ context.Context, id string, _ container.RemoveOptions) error {
			removedIDs = append(removedIDs, id)
			return nil
		},
	}

	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())

	err := mgr.CleanupOrphans(context.Background())
	require.NoError(t, err)
	assert.Len(t, removedIDs, 2)
	assert.Contains(t, removedIDs, "orphan-1")
	assert.Contains(t, removedIDs, "orphan-2")
}

func TestStreamLogs_WithLogData(t *testing.T) {
	// Sample stream-json lines that logparser would process.
	// We pass them as raw bytes (not Docker multiplexed format).
	// stdcopy.StdCopy will fail to demux them (no valid header), so it will
	// return without writing anything to the pipe — logparser will then see
	// an empty stream. The test verifies the pipeline does not panic or hang.
	sampleJSON := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n"

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerLogsFn: func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(sampleJSON)), nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}
			return ch, make(chan error)
		},
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	// Should complete without hanging or panicking.
	mgr.Run(context.Background(), payload)
	mgr.Wait()

	assert.Equal(t, 0, tr.Count())
}

// buildAuthTestManager creates a manager with a mock that captures env and mounts,
// runs a container, and returns the captured values. The cfg argument controls auth.
func buildAuthTestManager(t *testing.T, cfg *config.Config) (env []string, mounts []string) {
	t.Helper()

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerCreateFn: func(_ context.Context, c *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			env = c.Env
			for _, m := range hc.Mounts {
				mounts = append(mounts, m.Target)
			}
			return container.CreateResponse{ID: "auth-test-ctr"}, nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}
			return ch, make(chan error)
		},
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(cbSrv.Close)

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())
	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	return env, mounts
}

// TestAuthPriority_ClaudeAuthDir verifies that when ClaudeAuthDir is set alongside
// oauth token and API key, only the directory mount is used — no auth env vars injected.
func TestAuthPriority_ClaudeAuthDir(t *testing.T) {
	// Create a temporary directory to use as ClaudeAuthDir so validation passes.
	dir := t.TempDir()

	cfg := testConfig()
	cfg.ClaudeAuthDir = dir
	cfg.ClaudeOAuthToken = "oauth-tok"
	cfg.AnthropicAPIKey = "sk-api"

	env, mounts := buildAuthTestManager(t, cfg)

	// Mount should be present.
	assert.Contains(t, mounts, "/claude-auth", "claude-auth mount should be present")

	// No auth env vars should be injected.
	for _, e := range env {
		assert.False(t, strings.HasPrefix(e, "ANTHROPIC_API_KEY="), "ANTHROPIC_API_KEY must not be set when ClaudeAuthDir is highest priority")
		assert.False(t, strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN="), "CLAUDE_CODE_OAUTH_TOKEN must not be set when ClaudeAuthDir is highest priority")
	}
}

// TestAuthPriority_OAuthToken verifies that when ClaudeAuthDir is unset but
// ClaudeOAuthToken and AnthropicAPIKey are both set, only the OAuth token env var is injected.
func TestAuthPriority_OAuthToken(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = "oauth-tok"
	cfg.AnthropicAPIKey = "sk-api"

	env, mounts := buildAuthTestManager(t, cfg)

	// No mount should be present.
	assert.Empty(t, mounts, "no mounts should be present when using oauth token")

	// Only CLAUDE_CODE_OAUTH_TOKEN should be injected.
	assert.Contains(t, env, "CLAUDE_CODE_OAUTH_TOKEN=oauth-tok")
	for _, e := range env {
		assert.False(t, strings.HasPrefix(e, "ANTHROPIC_API_KEY="), "ANTHROPIC_API_KEY must not be set when OAuth token takes priority")
	}
}

// TestAuthPriority_APIKeyOnly verifies that when only AnthropicAPIKey is set,
// only ANTHROPIC_API_KEY is injected and no mounts are added.
func TestAuthPriority_APIKeyOnly(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = ""
	cfg.AnthropicAPIKey = "sk-only"

	env, mounts := buildAuthTestManager(t, cfg)

	assert.Empty(t, mounts, "no mounts should be present when using API key only")
	assert.Contains(t, env, "ANTHROPIC_API_KEY=sk-only")
	for _, e := range env {
		assert.False(t, strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN="), "CLAUDE_CODE_OAUTH_TOKEN must not be set when only API key is configured")
	}
}

// TestAuthPriority_OAuthTokenOnly verifies that when only ClaudeOAuthToken is set,
// only CLAUDE_CODE_OAUTH_TOKEN is injected and no mounts are added.
func TestAuthPriority_OAuthTokenOnly(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = "oauth-only"
	cfg.AnthropicAPIKey = ""

	env, mounts := buildAuthTestManager(t, cfg)

	assert.Empty(t, mounts, "no mounts should be present when using OAuth token only")
	assert.Contains(t, env, "CLAUDE_CODE_OAUTH_TOKEN=oauth-only")
	for _, e := range env {
		assert.False(t, strings.HasPrefix(e, "ANTHROPIC_API_KEY="), "ANTHROPIC_API_KEY must not be set when only OAuth token is configured")
	}
}

// TestClaudeSettings_EnvVarPresentWhenSet verifies that CM_CLAUDE_SETTINGS is
// injected into the container env when cfg.ClaudeSettings is non-empty.
func TestClaudeSettings_EnvVarPresentWhenSet(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeSettings = `{"enabledTools":["Bash","Edit"]}`

	env, _ := buildAuthTestManager(t, cfg)

	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"enabledTools":["Bash","Edit"]}`)
}

// TestClaudeSettings_EnvVarAbsentWhenEmpty verifies that CM_CLAUDE_SETTINGS is
// not injected when cfg.ClaudeSettings is empty.
func TestClaudeSettings_EnvVarAbsentWhenEmpty(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeSettings = ""

	env, _ := buildAuthTestManager(t, cfg)

	for _, e := range env {
		assert.False(t, strings.HasPrefix(e, "CM_CLAUDE_SETTINGS="), "CM_CLAUDE_SETTINGS must not be set when ClaudeSettings is empty")
	}
}

// TestClaudeSettings_WithClaudeAuthDir verifies that CM_CLAUDE_SETTINGS is
// injected alongside the claude-auth directory mount.
func TestClaudeSettings_WithClaudeAuthDir(t *testing.T) {
	dir := t.TempDir()

	cfg := testConfig()
	cfg.ClaudeAuthDir = dir
	cfg.ClaudeSettings = `{"model":"claude-sonnet-4-6"}`

	env, mounts := buildAuthTestManager(t, cfg)

	assert.Contains(t, mounts, "/claude-auth", "claude-auth mount should be present")
	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"model":"claude-sonnet-4-6"}`)
}

// TestClaudeSettings_WithOAuthToken verifies that CM_CLAUDE_SETTINGS is
// injected alongside the OAuth token env var.
func TestClaudeSettings_WithOAuthToken(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = "oauth-tok"
	cfg.AnthropicAPIKey = ""
	cfg.ClaudeSettings = `{"theme":"dark"}`

	env, _ := buildAuthTestManager(t, cfg)

	assert.Contains(t, env, "CLAUDE_CODE_OAUTH_TOKEN=oauth-tok")
	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"theme":"dark"}`)
}

// TestClaudeSettings_WithAPIKey verifies that CM_CLAUDE_SETTINGS is
// injected alongside the Anthropic API key env var.
func TestClaudeSettings_WithAPIKey(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = ""
	cfg.AnthropicAPIKey = "sk-test-key"
	cfg.ClaudeSettings = `{"permissions":{"allow":["Bash"]}}`

	env, _ := buildAuthTestManager(t, cfg)

	assert.Contains(t, env, "ANTHROPIC_API_KEY=sk-test-key")
	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"permissions":{"allow":["Bash"]}}`)
}

// TestBaseBranch_EnvVarPresentWhenSet verifies that CM_BASE_BRANCH is injected
// into the container env when RunConfig.BaseBranch is non-empty.
func TestBaseBranch_EnvVarPresentWhenSet(t *testing.T) {
	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerCreateFn: func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			assert.Contains(t, cfg.Env, "CM_BASE_BRANCH=main")
			return container.CreateResponse{ID: "bb-test-ctr"}, nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}
			return ch, make(chan error)
		},
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	payload.BaseBranch = "main"
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()
}

// TestBaseBranch_EnvVarAbsentWhenEmpty verifies that CM_BASE_BRANCH is not
// injected into the container env when RunConfig.BaseBranch is empty.
// TestInteractive_EnvVarPresentWhenTrue verifies that CM_INTERACTIVE=1 is injected
// into the container env when RunConfig.Interactive is true.
func TestInteractive_EnvVarPresentWhenTrue(t *testing.T) {
	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerCreateFn: func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			assert.Contains(t, cfg.Env, "CM_INTERACTIVE=1")
			return container.CreateResponse{ID: "interactive-test-ctr"}, nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}
			return ch, make(chan error)
		},
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	payload.Interactive = true
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()
}

// TestInteractive_EnvVarAbsentWhenFalse verifies that CM_INTERACTIVE is not injected
// into the container env when RunConfig.Interactive is false (the default).
func TestInteractive_EnvVarAbsentWhenFalse(t *testing.T) {
	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerCreateFn: func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			for _, e := range cfg.Env {
				assert.False(t, strings.HasPrefix(e, "CM_INTERACTIVE="), "CM_INTERACTIVE must not be set when Interactive is false")
			}
			return container.CreateResponse{ID: "non-interactive-test-ctr"}, nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}
			return ch, make(chan error)
		},
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	// Interactive is intentionally left false (zero value).
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()
}

func TestBaseBranch_EnvVarAbsentWhenEmpty(t *testing.T) {
	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerCreateFn: func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			for _, e := range cfg.Env {
				assert.False(t, strings.HasPrefix(e, "CM_BASE_BRANCH="), "CM_BASE_BRANCH must not be set when BaseBranch is empty")
			}
			return container.CreateResponse{ID: "bb-test-ctr"}, nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}
			return ch, make(chan error)
		},
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	// BaseBranch is intentionally left empty (zero value).
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()
}

// TestInteractive_StdinConfigFlags verifies that ContainerCreate receives
// OpenStdin=true, AttachStdin=true, Tty=false, StdinOnce=false when Interactive=true.
func TestInteractive_StdinConfigFlags(t *testing.T) {
	var capturedCfg *container.Config
	var attachCalled int

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerCreateFn: func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			capturedCfg = cfg
			return container.CreateResponse{ID: "stdin-test-ctr"}, nil
		},
		ContainerAttachFn: func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
			attachCalled++
			// Use a discarding writer so the priming write does not block.
			return &HijackedResponse{Conn: nopWriteCloser{}}, nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}
			return ch, make(chan error)
		},
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	payload.Interactive = true
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	require.NotNil(t, capturedCfg)
	assert.True(t, capturedCfg.OpenStdin, "OpenStdin must be true when Interactive=true")
	assert.True(t, capturedCfg.AttachStdin, "AttachStdin must be true when Interactive=true")
	assert.False(t, capturedCfg.Tty, "Tty must be false when Interactive=true")
	assert.False(t, capturedCfg.StdinOnce, "StdinOnce must be false when Interactive=true")
	assert.Equal(t, 1, attachCalled, "ContainerAttach must be called exactly once when Interactive=true")
}

// TestPrimingMessage_WrittenWhenInteractive verifies that a valid stream-json
// user message is written to the container's stdin exactly once when
// RunConfig.Interactive is true. It also verifies no priming write occurs when
// Interactive is false.
func TestPrimingMessage_WrittenWhenInteractive(t *testing.T) {
	t.Run("interactive=true writes exactly one priming message", func(t *testing.T) {
		var writtenBytes [][]byte
		var writeMu sync.Mutex

		// A WriteCloser that captures all Write calls.
		pr, pw := io.Pipe()
		go func() { _, _ = io.ReadAll(pr) }() // drain so writes don't block

		spyWriter := &spyWriteCloser{
			WriteCloser: pw,
			onWrite: func(b []byte) {
				writeMu.Lock()
				defer writeMu.Unlock()
				// Make a copy — the slice backing b may be reused.
				buf := make([]byte, len(b))
				copy(buf, b)
				writtenBytes = append(writtenBytes, buf)
			},
		}

		mock := &MockDockerClient{
			ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("")), nil
			},
			ContainerAttachFn: func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
				return &HijackedResponse{Conn: spyWriter}, nil
			},
			ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
				ch := make(chan container.WaitResponse, 1)
				ch <- container.WaitResponse{StatusCode: 0}
				return ch, make(chan error)
			},
		}

		cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer cbSrv.Close()

		tr := tracker.New()
		cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
		tp := testTokenProvider(t)

		mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

		payload := testPayload()
		payload.Interactive = true
		payload.CardID = "PROJ-099"
		require.NoError(t, tr.Add(&tracker.ContainerInfo{
			CardID:  payload.CardID,
			Project: payload.Project,
		}))

		mgr.Run(context.Background(), payload)
		mgr.Wait()

		writeMu.Lock()
		defer writeMu.Unlock()

		// Exactly one priming write must have landed.
		require.Len(t, writtenBytes, 1, "expected exactly one priming stdin write")

		// Parse the written bytes as a stream-json user message.
		raw := writtenBytes[0]
		assert.True(t, len(raw) > 0 && raw[len(raw)-1] == '\n', "priming message must be newline-terminated")

		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		require.NoError(t, json.Unmarshal(raw[:len(raw)-1], &msg), "priming bytes must be valid JSON")
		assert.Equal(t, "user", msg.Type)
		assert.Equal(t, "user", msg.Message.Role)
		require.Len(t, msg.Message.Content, 1)
		assert.Equal(t, "text", msg.Message.Content[0].Type)
		assert.Contains(t, msg.Message.Content[0].Text, "get_skill(skill_name='create-plan'")
		assert.Contains(t, msg.Message.Content[0].Text, payload.CardID)
	})

	t.Run("interactive=false writes no priming message", func(t *testing.T) {
		var attachCalled int

		mock := &MockDockerClient{
			ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("")), nil
			},
			ContainerAttachFn: func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
				attachCalled++
				_, pw := io.Pipe()
				return &HijackedResponse{Conn: pw}, nil
			},
			ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
				ch := make(chan container.WaitResponse, 1)
				ch <- container.WaitResponse{StatusCode: 0}
				return ch, make(chan error)
			},
		}

		cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer cbSrv.Close()

		tr := tracker.New()
		cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
		tp := testTokenProvider(t)

		mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

		payload := testPayload()
		// Interactive is false (zero value): no attach, no priming write.
		require.NoError(t, tr.Add(&tracker.ContainerInfo{
			CardID:  payload.CardID,
			Project: payload.Project,
		}))

		mgr.Run(context.Background(), payload)
		mgr.Wait()

		assert.Equal(t, 0, attachCalled, "ContainerAttach must not be called when Interactive=false")
	})

	t.Run("interactive=true with BaseBranch appends branch context", func(t *testing.T) {
		var writtenBytes [][]byte
		var writeMu sync.Mutex

		pr, pw := io.Pipe()
		go func() { _, _ = io.ReadAll(pr) }()

		spyWriter := &spyWriteCloser{
			WriteCloser: pw,
			onWrite: func(b []byte) {
				writeMu.Lock()
				defer writeMu.Unlock()
				buf := make([]byte, len(b))
				copy(buf, b)
				writtenBytes = append(writtenBytes, buf)
			},
		}

		mock := &MockDockerClient{
			ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("")), nil
			},
			ContainerAttachFn: func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
				return &HijackedResponse{Conn: spyWriter}, nil
			},
			ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
				ch := make(chan container.WaitResponse, 1)
				ch <- container.WaitResponse{StatusCode: 0}
				return ch, make(chan error)
			},
		}

		cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer cbSrv.Close()

		tr := tracker.New()
		cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
		tp := testTokenProvider(t)

		mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

		payload := testPayload()
		payload.Interactive = true
		payload.BaseBranch = "feature/my-branch"
		require.NoError(t, tr.Add(&tracker.ContainerInfo{
			CardID:  payload.CardID,
			Project: payload.Project,
		}))

		mgr.Run(context.Background(), payload)
		mgr.Wait()

		writeMu.Lock()
		defer writeMu.Unlock()

		require.Len(t, writtenBytes, 1)
		var msg struct {
			Message struct {
				Content []struct{ Text string `json:"text"` } `json:"content"`
			} `json:"message"`
		}
		raw := writtenBytes[0]
		require.NoError(t, json.Unmarshal(raw[:len(raw)-1], &msg))
		require.Len(t, msg.Message.Content, 1)
		assert.Contains(t, msg.Message.Content[0].Text, "feature/my-branch")
	})
}

// spyWriteCloser wraps an io.WriteCloser and calls onWrite for every Write call.
type spyWriteCloser struct {
	io.WriteCloser
	onWrite func([]byte)
}

func (s *spyWriteCloser) Write(p []byte) (int, error) {
	if s.onWrite != nil {
		s.onWrite(p)
	}
	return s.WriteCloser.Write(p)
}

// TestBuildPrimingContent verifies the priming content helper directly.
func TestBuildPrimingContent(t *testing.T) {
	t.Run("without base branch", func(t *testing.T) {
		payload := RunConfig{CardID: "PROJ-123", Project: "myproj"}
		content := buildPrimingContent(payload)
		assert.Contains(t, content, "PROJ-123")
		assert.Contains(t, content, "get_skill(skill_name='create-plan'")
		assert.Contains(t, content, "card_id='PROJ-123'")
		assert.NotContains(t, content, "base branch")
	})

	t.Run("with base branch", func(t *testing.T) {
		payload := RunConfig{CardID: "PROJ-456", Project: "myproj", BaseBranch: "main"}
		content := buildPrimingContent(payload)
		assert.Contains(t, content, "PROJ-456")
		assert.Contains(t, content, "get_skill(skill_name='create-plan'")
		assert.Contains(t, content, "main")
		assert.Contains(t, content, "base branch")
	})
}

func TestInteractive_FalseNoStdinFlagsNoAttach(t *testing.T) {
	var capturedCfg *container.Config
	var attachCalled int

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerCreateFn: func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			capturedCfg = cfg
			return container.CreateResponse{ID: "non-interactive-stdin-ctr"}, nil
		},
		ContainerAttachFn: func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
			attachCalled++
			_, pw := io.Pipe()
			return &HijackedResponse{Conn: pw}, nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}
			return ch, make(chan error)
		},
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	// Interactive is intentionally left false (zero value).
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	require.NotNil(t, capturedCfg)
	assert.False(t, capturedCfg.OpenStdin, "OpenStdin must be false when Interactive=false")
	assert.False(t, capturedCfg.AttachStdin, "AttachStdin must be false when Interactive=false")
	assert.Equal(t, 0, attachCalled, "ContainerAttach must not be called when Interactive=false")
}

func TestSanitizeContainerName(t *testing.T) {
	tests := []struct {
		project  string
		cardID   string
		expected string
	}{
		{"my-project", "PROJ-042", "cmr-my-project-proj-042"},
		{"alpha", "A-001", "cmr-alpha-a-001"},
		{"with spaces", "B-002", "cmr-with-spaces-b-002"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expected, sanitizeContainerName(tt.project, tt.cardID))
	}
}

func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "SCP-style with user",
			input:    "git@github.com:org/repo.git",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "SCP-style without user",
			input:    "github.com:org/repo.git",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "ssh scheme with user",
			input:    "ssh://git@github.com/org/repo.git",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "ssh scheme without user",
			input:    "ssh://github.com/org/repo.git",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "https passthrough",
			input:    "https://github.com/org/repo.git",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "http passthrough",
			input:    "http://github.com/org/repo.git",
			expected: "http://github.com/org/repo.git",
		},
		{
			name:     "SCP-style with non-GitHub host",
			input:    "git@gitlab.com:mygroup/myrepo.git",
			expected: "https://gitlab.com/mygroup/myrepo.git",
		},
		{
			name:     "ssh scheme with non-GitHub host",
			input:    "ssh://git@bitbucket.org/team/project.git",
			expected: "https://bitbucket.org/team/project.git",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, normalizeRepoURL(tt.input))
		})
	}
}
