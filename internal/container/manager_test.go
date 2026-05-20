package container

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	githubauth "github.com/mhersson/contextmatrix-githubauth"

	"github.com/mhersson/contextmatrix-runner/internal/callback"
	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/mhersson/contextmatrix-runner/internal/metrics"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

var testRSAKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	return k
})

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()

	cfg := &config.Config{
		BaseImage:        "test-image:latest",
		ContainerTimeout: "1h",
		AnthropicAPIKey:  "sk-test",
		// Explicit ImagePullPolicy: tests assert ImagePullFn is invoked, so
		// PullAlways preserves the behavior that previously came from the
		// now-removed empty-string fallback in Manager.pullImage.
		ImagePullPolicy: config.PullAlways,
		// Per-test isolation: each test gets its own temp dir so parallel
		// tests (and cleanup) can't step on each other's secrets state.
		SecretsDir: t.TempDir(),
	}
	// Parse the container timeout duration without full validation.
	cfg.ParseContainerTimeout()

	return cfg
}

// testTokenProvider creates a mock GitHub token server and AppProvider.
func testTokenProvider(t *testing.T) githubauth.TokenGenerator {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ghs_test_token",
			"expires_at": "2030-01-01T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)

	tp, err := githubauth.NewAppProviderWithKey(12345, 67890, testRSAKey(), srv.URL)
	require.NoError(t, err)

	return tp
}

func testPATProvider(t *testing.T) githubauth.TokenGenerator {
	t.Helper()

	p, err := githubauth.NewPATProvider("ghp_test_pat")
	require.NoError(t, err)

	return p
}

func testPayload() RunConfig {
	return RunConfig{
		Mode:    ModeTask,
		CardID:  "PROJ-042",
		Project: "my-project",
		RepoURL: "https://github.com/org/repo.git",
		MCPURL:  "http://cm:8080/mcp",
	}
}

func TestRun_Success(t *testing.T) {
	var (
		createdEnv    []string
		createdLabels map[string]string
		statusMu      sync.Mutex
		// reportedStatuses is mutex-protected because the running-status
		// callback now fires on a detached goroutine so the handler can
		// still be writing it after mgr.Wait() returns.
		reportedStatuses []string
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	mock.ImagePullFn = func(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
		assert.Equal(t, "test-image:latest", ref)

		return io.NopCloser(strings.NewReader("")), nil
	}
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
		createdEnv = cfg.Env
		createdLabels = cfg.Labels

		assert.Contains(t, name, "cmr-")

		return container.CreateResponse{ID: "test-ctr-123"}, nil
	}

	// Track reported statuses.
	origCbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			RunnerStatus string `json:"runner_status"`
		}

		_ = json.Unmarshal(body, &req)

		statusMu.Lock()

		reportedStatuses = append(reportedStatuses, req.RunnerStatus)
		statusMu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer origCbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(origCbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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

	// CM_GIT_TOKEN is delivered via the shared dir mount (not env).
	// Claude auth secrets are in the shared dir too.
	// CM_MCP_API_KEY is per-card and must appear directly in Env.
	for _, e := range createdEnv {
		assert.False(t, strings.HasPrefix(e, "CM_GIT_TOKEN="), "CM_GIT_TOKEN must not be in Env; it rides the shared dir mount")
		assert.False(t, strings.HasPrefix(e, "ANTHROPIC_API_KEY="), "ANTHROPIC_API_KEY must not be in Env")
		assert.False(t, strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN="), "CLAUDE_CODE_OAUTH_TOKEN must not be in Env")
	}

	// Verify labels.
	assert.Equal(t, "true", createdLabels[LabelRunner])
	assert.Equal(t, "PROJ-042", createdLabels[LabelCardID])
	assert.Equal(t, "my-project", createdLabels[LabelProject])

	// Should have reported "running". The running-status callback runs on a
	// detached goroutine so it may land after mgr.Wait() returns — poll
	// briefly with the mutex held.
	require.Eventually(t, func() bool {
		statusMu.Lock()
		defer statusMu.Unlock()

		return slices.Contains(reportedStatuses, "running")
	}, 2*time.Second, 10*time.Millisecond, "running status must be reported")
}

func TestRun_PATProvider(t *testing.T) {
	var (
		createdEnv     []string
		createdMounts  []mount.Mount
		secretsSource  string
		secretsRdOnly  bool
		secretsMntType mount.Type
	)

	cfg := testConfig(t)

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, c *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		createdEnv = c.Env
		createdMounts = hc.Mounts

		for _, m := range hc.Mounts {
			if m.Target == secretsMountTarget {
				secretsSource = m.Source
				secretsRdOnly = m.ReadOnly
				secretsMntType = m.Type
			}
		}

		return container.CreateResponse{ID: "pat-test-ctr"}, nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())
	require.NoError(t, mgr.InitSharedSecrets())

	sharedDir := filepath.Join(cfg.SecretsDir, "shared")

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	// CM_GIT_TOKEN must NOT be in container env; it rides the shared dir mount.
	for _, e := range createdEnv {
		assert.False(t, strings.HasPrefix(e, "CM_GIT_TOKEN="),
			"CM_GIT_TOKEN must not be in container env; it rides the shared dir mount")
	}

	// The secrets dir-mount must exist, be a read-only bind.
	var sawSecrets bool

	for _, m := range createdMounts {
		if m.Target == secretsMountTarget {
			sawSecrets = true
		}
	}

	require.True(t, sawSecrets, "secrets mount must be present")
	assert.Equal(t, mount.TypeBind, secretsMntType)
	assert.True(t, secretsRdOnly, "secrets mount must be read-only")

	// The mount source must be the shared directory (not a per-container file).
	assert.Equal(t, sharedDir, secretsSource, "secrets mount source must be the shared dir")

	// The shared dir must NOT be removed after container exit.
	_, err := os.Stat(secretsSource)
	assert.False(t, os.IsNotExist(err),
		"shared secrets dir must NOT be removed after container exits; stat err=%v", err)
}

// TestStartContainer_SecretsSharedDirMountAndMCPKeyInEnv verifies the new
// delivery model: the shared dir is bind-mounted read-only at secretsMountTarget
// and CM_MCP_API_KEY rides directly in Container.Env (per-card, doesn't rotate).
func TestStartContainer_SecretsSharedDirMountAndMCPKeyInEnv(t *testing.T) {
	var (
		createdEnv    []string
		createdMounts []mount.Mount
	)

	cfg := testConfig(t)
	cfg.AnthropicAPIKey = "sk-api-key"

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerCreateFn: func(_ context.Context, c *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			createdEnv = c.Env
			createdMounts = hc.Mounts

			return container.CreateResponse{ID: "secret-test-ctr"}, nil
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

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	payload := testPayload()
	payload.MCPAPIKey = "mcp-secret"
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	// CM_MCP_API_KEY is per-card and must appear in Env.
	assert.Contains(t, createdEnv, "CM_MCP_API_KEY=mcp-secret",
		"CM_MCP_API_KEY must be in container Env for per-card delivery")

	// Rotating secrets (CM_GIT_TOKEN, Claude auth) must NOT be in Env.
	for _, e := range createdEnv {
		assert.False(t, strings.HasPrefix(e, "CM_GIT_TOKEN="),
			"CM_GIT_TOKEN must not be in Env; it rides the shared dir mount")
		assert.False(t, strings.HasPrefix(e, "ANTHROPIC_API_KEY="),
			"ANTHROPIC_API_KEY must not be in Env; it rides the shared dir mount")
		assert.False(t, strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN="),
			"CLAUDE_CODE_OAUTH_TOKEN must not be in Env; it rides the shared dir mount")
	}

	// Shared dir mount present, read-only bind at secretsMountTarget.
	var found bool

	for _, m := range createdMounts {
		if m.Target == secretsMountTarget {
			found = true

			assert.Equal(t, mount.TypeBind, m.Type)
			assert.True(t, m.ReadOnly)
			assert.Equal(t, filepath.Join(cfg.SecretsDir, "shared"), m.Source,
				"mount source must be the shared dir, not a per-container file")
		}
	}

	require.True(t, found, "secrets mount must be present at %s", secretsMountTarget)
}

// TestSecretsSharedDirNotRemovedOnContainerExit verifies the shared secrets
// directory is NOT removed when a container exits — it is owned by the
// singleton tokenRefresher, not per-container lifecycle.
func TestSecretsSharedDirNotRemovedOnContainerExit(t *testing.T) {
	var secretsSource string

	cfg := testConfig(t)

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, _ *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		for _, m := range hc.Mounts {
			if m.Target == secretsMountTarget {
				secretsSource = m.Source
			}
		}

		return container.CreateResponse{ID: "cleanup-test-ctr"}, nil
	}
	mock.ContainerWaitFn = func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		ch := make(chan container.WaitResponse, 1)
		ch <- container.WaitResponse{StatusCode: 0}

		return ch, make(chan error)
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())
	require.NoError(t, mgr.InitSharedSecrets())

	sharedDir := filepath.Join(cfg.SecretsDir, "shared")

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	require.NotEmpty(t, secretsSource, "expected secrets mount source path")
	assert.Equal(t, sharedDir, secretsSource, "mount source must be the shared dir")

	// Shared dir must still exist after container exit.
	_, err := os.Stat(secretsSource)
	assert.NoError(t, err, "shared secrets dir must still exist after container exits")
}

func TestRun_NonZeroExit(t *testing.T) {
	var (
		statusMu         sync.Mutex
		reportedStatuses []string
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			RunnerStatus string `json:"runner_status"`
		}

		_ = json.Unmarshal(body, &req)

		// Mutex-protected: the running callback now fires on a detached
		// goroutine concurrently with the failed callback, so plain-slice
		// append would race under -race.
		statusMu.Lock()

		reportedStatuses = append(reportedStatuses, req.RunnerStatus)
		statusMu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	mock.ContainerWaitFn = func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		ch := make(chan container.WaitResponse, 1)
		ch <- container.WaitResponse{StatusCode: 1}

		return ch, make(chan error)
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	statusMu.Lock()
	defer statusMu.Unlock()

	assert.Contains(t, reportedStatuses, "failed")
	assert.Equal(t, 0, tr.Count())
}

func TestRun_ImagePullFailure(t *testing.T) {
	var failureReported atomic.Bool

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			RunnerStatus string `json:"runner_status"`
		}

		_ = json.Unmarshal(body, &req)
		if req.RunnerStatus == "failed" {
			failureReported.Store(true)
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	mock.ImagePullFn = func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
		return nil, fmt.Errorf("image not found")
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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

	mock := successfulMock()
	mock.ImagePullFn = func(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
		pulledImage = ref

		return io.NopCloser(strings.NewReader("")), nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	cfg := testConfig(t)
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
	mock := successfulMock()
	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

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
	mock := successfulMock()
	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	err := mgr.Kill("proj", "PROJ-999")
	assert.ErrorContains(t, err, "no container tracked")
}

func TestCleanupOrphans(t *testing.T) {
	var removedIDs []string

	mock := successfulMock()
	mock.ContainerListFn = func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
		return []DockerContainer{
			{ID: "orphan-1", Labels: map[string]string{LabelCardID: "A-001", LabelProject: "proj"}},
			{ID: "orphan-2", Labels: map[string]string{LabelCardID: "A-002", LabelProject: "proj"}},
		}, nil
	}
	mock.ContainerRemoveFn = func(_ context.Context, id string, _ container.RemoveOptions) error {
		removedIDs = append(removedIDs, id)

		return nil
	}

	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	err := mgr.CleanupOrphans(context.Background())
	require.NoError(t, err)
	assert.Len(t, removedIDs, 2)
	assert.Contains(t, removedIDs, "orphan-1")
	assert.Contains(t, removedIDs, "orphan-2")
}

// TestCleanupOrphans_SkipsTrackedContainers guards the regression where the
// maintenance loop killed every active worker container on every tick because
// CleanupOrphans did not filter the Docker list against the in-memory tracker.
// A container labeled with (project, card_id) that is currently in the tracker
// must be left alone; only containers present in Docker AND absent from the
// tracker are true orphans.
func TestCleanupOrphans_SkipsTrackedContainers(t *testing.T) {
	var removedIDs []string

	mock := successfulMock()
	mock.ContainerListFn = func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
		return []DockerContainer{
			{ID: "live-1", Labels: map[string]string{LabelCardID: "A-001", LabelProject: "proj"}},
			{ID: "orphan-1", Labels: map[string]string{LabelCardID: "A-002", LabelProject: "proj"}},
		}, nil
	}
	mock.ContainerRemoveFn = func(_ context.Context, id string, _ container.RemoveOptions) error {
		removedIDs = append(removedIDs, id)

		return nil
	}

	tr := tracker.New()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		Project: "proj", CardID: "A-001", ContainerID: "live-1",
	}))

	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	err := mgr.CleanupOrphans(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"orphan-1"}, removedIDs,
		"only the untracked container must be removed; tracked live-1 must survive")
}

// TestCleanupOrphans_StopFailureWithSuccessfulRemoveIsNotAnError asserts that
// a per-container Stop failure that ends in a successful force-Remove is
// logged as a warning but NOT returned as an error. The container is gone,
// which is the only outcome that matters. Returning Stop failures as hard
// errors used to mislead callers into thinking cleanup did not complete,
// even when every orphan was ultimately destroyed.
func TestCleanupOrphans_StopFailureWithSuccessfulRemoveIsNotAnError(t *testing.T) {
	var (
		stopIDs   []string
		removeIDs []string
		muStop    sync.Mutex
		muRemove  sync.Mutex
	)

	mock := successfulMock()
	mock.ContainerListFn = func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
		return []DockerContainer{
			{ID: "orphan-1", Labels: map[string]string{LabelCardID: "A-001", LabelProject: "proj"}},
			{ID: "orphan-2-bad-stop", Labels: map[string]string{LabelCardID: "A-002", LabelProject: "proj"}},
			{ID: "orphan-3", Labels: map[string]string{LabelCardID: "A-003", LabelProject: "proj"}},
		}, nil
	}
	mock.ContainerStopFn = func(_ context.Context, id string, _ container.StopOptions) error {
		muStop.Lock()

		stopIDs = append(stopIDs, id)
		muStop.Unlock()

		if id == "orphan-2-bad-stop" {
			return fmt.Errorf("docker stop failed for %s", id)
		}

		return nil
	}
	mock.ContainerRemoveFn = func(_ context.Context, id string, _ container.RemoveOptions) error {
		muRemove.Lock()

		removeIDs = append(removeIDs, id)
		muRemove.Unlock()

		// Every Remove succeeds — including the one whose Stop failed.
		return nil
	}

	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	err := mgr.CleanupOrphans(context.Background())
	require.NoError(t, err,
		"CleanupOrphans must NOT return an error when every container was ultimately removed, "+
			"even if Stop failed for some of them")

	// Every orphan must have been Stop-attempted (cleanup does not abort on
	// the first failure) and every orphan must end up Remove-attempted.
	assert.ElementsMatch(t, []string{"orphan-1", "orphan-2-bad-stop", "orphan-3"}, stopIDs,
		"every orphan must have ContainerStop attempted")
	assert.ElementsMatch(t, []string{"orphan-1", "orphan-2-bad-stop", "orphan-3"}, removeIDs,
		"every orphan must have ContainerRemove attempted even after a Stop failure")
}

// TestCleanupOrphans_RemoveFailureIsAnError verifies that a Remove failure
// (the container is genuinely still present after best-effort cleanup) IS
// surfaced as an error so callers can alarm on it. Distinct from a Stop
// failure, which is a warning rather than an error.
func TestCleanupOrphans_RemoveFailureIsAnError(t *testing.T) {
	mock := successfulMock()
	mock.ContainerListFn = func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
		return []DockerContainer{
			{ID: "orphan-1", Labels: map[string]string{LabelCardID: "A-001", LabelProject: "proj"}},
			{ID: "orphan-stuck", Labels: map[string]string{LabelCardID: "A-002", LabelProject: "proj"}},
		}, nil
	}
	mock.ContainerStopFn = func(_ context.Context, _ string, _ container.StopOptions) error {
		return nil
	}
	mock.ContainerRemoveFn = func(_ context.Context, id string, _ container.RemoveOptions) error {
		if id == "orphan-stuck" {
			return fmt.Errorf("docker remove failed for %s", id)
		}

		return nil
	}

	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	err := mgr.CleanupOrphans(context.Background())
	require.Error(t, err, "CleanupOrphans must return an error when a container could not be removed")
	assert.Contains(t, err.Error(), "orphan-stuck",
		"error must identify the container that failed to remove")
	assert.Contains(t, err.Error(), "remove orphan",
		"error must describe which operation failed")
}

func TestStreamLogs_WithLogData(t *testing.T) {
	// Sample stream-json lines that logparser would process.
	// We pass them as raw bytes (not Docker multiplexed format).
	// stdcopy.StdCopy will fail to demux them (no valid header), so it will
	// return without writing anything to the pipe — logparser will then see
	// an empty stream. The test verifies the pipeline does not panic or hang.
	sampleJSON := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n"

	mock := successfulMock()
	mock.ContainerLogsFn = func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(sampleJSON)), nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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

// buildAuthTestManager creates a manager with a mock that captures env and
// mounts, runs a container, and returns the captured values. The cfg argument
// controls auth. The returned `mountTargets` contains all Mount.Target paths
// for tests that only care about presence/absence. `secretsMountSource` is the
// source path of the secrets mount (the shared dir in file-mode delivery).
func buildAuthTestManager(t *testing.T, cfg *config.Config) (env []string, mountTargets []string, secretsMountSource string) {
	t.Helper()

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, c *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		env = c.Env

		for _, m := range hc.Mounts {
			mountTargets = append(mountTargets, m.Target)

			if m.Target == secretsMountTarget {
				secretsMountSource = m.Source
			}
		}

		return container.CreateResponse{ID: "auth-test-ctr"}, nil
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

	return env, mountTargets, secretsMountSource
}

// assertNoSharedSecretEnv fails if any rotating secret env vars appear in env.
// CM_MCP_API_KEY is intentionally excluded: it is per-card and must appear in Env.
func assertNoSharedSecretEnv(t *testing.T, env []string) {
	t.Helper()

	for _, e := range env {
		assert.False(t, strings.HasPrefix(e, "ANTHROPIC_API_KEY="), "ANTHROPIC_API_KEY must not be in Env; it rides the shared dir mount")
		assert.False(t, strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN="), "CLAUDE_CODE_OAUTH_TOKEN must not be in Env; it rides the shared dir mount")
		assert.False(t, strings.HasPrefix(e, "CM_GIT_TOKEN="), "CM_GIT_TOKEN must not be in Env; it rides the shared dir mount")
	}
}

// TestAuthPriority_ClaudeAuthDir verifies that when ClaudeAuthDir is set alongside
// oauth token and API key, only the directory mount is used — no auth env vars.
func TestAuthPriority_ClaudeAuthDir(t *testing.T) {
	// Create a temporary directory to use as ClaudeAuthDir so validation passes.
	dir := t.TempDir()

	cfg := testConfig(t)
	cfg.ClaudeAuthDir = dir
	cfg.ClaudeOAuthToken = "oauth-tok"
	cfg.AnthropicAPIKey = "sk-api"

	env, targets, sharedSrc := buildAuthTestManager(t, cfg)

	// The claude-auth mount is present; so is the shared secrets dir mount.
	assert.Contains(t, targets, "/claude-auth", "claude-auth mount should be present")
	assert.Contains(t, targets, secretsMountTarget, "secrets mount should be present")

	// Shared dir mount source must point at SecretsDir/shared.
	assert.Equal(t, filepath.Join(cfg.SecretsDir, "shared"), sharedSrc,
		"secrets mount source must be the shared dir")

	assertNoSharedSecretEnv(t, env)
}

// TestAuthPriority_OAuthToken verifies that when ClaudeAuthDir is unset but
// ClaudeOAuthToken and AnthropicAPIKey are both set, no auth env vars appear
// in the container env (auth goes via the shared dir).
func TestAuthPriority_OAuthToken(t *testing.T) {
	cfg := testConfig(t)
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = "oauth-tok"
	cfg.AnthropicAPIKey = "sk-api"

	env, targets, sharedSrc := buildAuthTestManager(t, cfg)

	// The only mount must be the shared secrets dir bind mount.
	assert.Equal(t, []string{secretsMountTarget}, targets,
		"only the secrets mount should be present when using oauth token")

	assert.Equal(t, filepath.Join(cfg.SecretsDir, "shared"), sharedSrc,
		"secrets mount source must be the shared dir")

	assertNoSharedSecretEnv(t, env)
}

// TestAuthPriority_APIKeyOnly verifies that when only AnthropicAPIKey is set,
// no auth env vars appear in the container env (auth goes via the shared dir).
func TestAuthPriority_APIKeyOnly(t *testing.T) {
	cfg := testConfig(t)
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = ""
	cfg.AnthropicAPIKey = "sk-only"

	env, targets, sharedSrc := buildAuthTestManager(t, cfg)

	assert.Equal(t, []string{secretsMountTarget}, targets,
		"only the secrets mount should be present when using API key only")

	assert.Equal(t, filepath.Join(cfg.SecretsDir, "shared"), sharedSrc,
		"secrets mount source must be the shared dir")

	assertNoSharedSecretEnv(t, env)
}

// TestAuthPriority_OAuthTokenOnly verifies that when only ClaudeOAuthToken is
// set, no auth env vars appear in the container env (auth goes via the shared dir).
func TestAuthPriority_OAuthTokenOnly(t *testing.T) {
	cfg := testConfig(t)
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = "oauth-only"
	cfg.AnthropicAPIKey = ""

	env, targets, sharedSrc := buildAuthTestManager(t, cfg)

	assert.Equal(t, []string{secretsMountTarget}, targets,
		"only the secrets mount should be present when using OAuth token only")

	assert.Equal(t, filepath.Join(cfg.SecretsDir, "shared"), sharedSrc,
		"secrets mount source must be the shared dir")

	assertNoSharedSecretEnv(t, env)
}

// TestClaudeSettings_EnvVarPresentWhenSet verifies that CM_CLAUDE_SETTINGS is
// injected into the container env when cfg.ClaudeSettings is non-empty.
func TestClaudeSettings_EnvVarPresentWhenSet(t *testing.T) {
	cfg := testConfig(t)
	cfg.ClaudeSettings = `{"enabledTools":["Bash","Edit"]}`

	env, _, _ := buildAuthTestManager(t, cfg)

	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"enabledTools":["Bash","Edit"]}`)
}

// TestClaudeSettings_EnvVarAbsentWhenEmpty verifies that CM_CLAUDE_SETTINGS is
// not injected when cfg.ClaudeSettings is empty.
func TestClaudeSettings_EnvVarAbsentWhenEmpty(t *testing.T) {
	cfg := testConfig(t)
	cfg.ClaudeSettings = ""

	env, _, _ := buildAuthTestManager(t, cfg)

	for _, e := range env {
		assert.False(t, strings.HasPrefix(e, "CM_CLAUDE_SETTINGS="), "CM_CLAUDE_SETTINGS must not be set when ClaudeSettings is empty")
	}
}

// TestClaudeSettings_WithClaudeAuthDir verifies that CM_CLAUDE_SETTINGS is
// injected alongside the claude-auth directory mount.
func TestClaudeSettings_WithClaudeAuthDir(t *testing.T) {
	dir := t.TempDir()

	cfg := testConfig(t)
	cfg.ClaudeAuthDir = dir
	cfg.ClaudeSettings = `{"model":"claude-sonnet-4-6"}`

	env, targets, _ := buildAuthTestManager(t, cfg)

	assert.Contains(t, targets, "/claude-auth", "claude-auth mount should be present")
	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"model":"claude-sonnet-4-6"}`)
}

// TestClaudeSettings_WithOAuthToken verifies that CM_CLAUDE_SETTINGS is
// set and no rotating secrets appear in the container env.
func TestClaudeSettings_WithOAuthToken(t *testing.T) {
	cfg := testConfig(t)
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = "oauth-tok"
	cfg.AnthropicAPIKey = ""
	cfg.ClaudeSettings = `{"theme":"dark"}`

	env, _, _ := buildAuthTestManager(t, cfg)

	assertNoSharedSecretEnv(t, env)
	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"theme":"dark"}`)
}

// TestClaudeSettings_WithAPIKey verifies that CM_CLAUDE_SETTINGS is set
// and no rotating secrets appear in the container env.
func TestClaudeSettings_WithAPIKey(t *testing.T) {
	cfg := testConfig(t)
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = ""
	cfg.AnthropicAPIKey = "sk-test-key"
	cfg.ClaudeSettings = `{"permissions":{"allow":["Bash"]}}`

	env, _, _ := buildAuthTestManager(t, cfg)

	assertNoSharedSecretEnv(t, env)
	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"permissions":{"allow":["Bash"]}}`)
}

// TestOrchestratorModel_EnvVarPresentWhenSet verifies that CM_ORCHESTRATOR_MODEL
// is injected into the container env when RunConfig.Model is non-empty.
func TestOrchestratorModel_EnvVarPresentWhenSet(t *testing.T) {
	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		assert.Contains(t, cfg.Env, "CM_ORCHESTRATOR_MODEL=claude-opus-4-7")

		return container.CreateResponse{ID: "model-test-ctr"}, nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	payload.Model = "claude-opus-4-7"
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()
}

// TestOrchestratorModel_EnvVarAbsentWhenEmpty verifies that CM_ORCHESTRATOR_MODEL
// is not injected into the container env when RunConfig.Model is empty.
func TestOrchestratorModel_EnvVarAbsentWhenEmpty(t *testing.T) {
	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		for _, e := range cfg.Env {
			assert.False(t, strings.HasPrefix(e, "CM_ORCHESTRATOR_MODEL="),
				"CM_ORCHESTRATOR_MODEL must not be set when Model is empty")
		}

		return container.CreateResponse{ID: "no-model-test-ctr"}, nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	// Model is intentionally left empty (zero value).
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()
}

// TestBaseBranch_EnvVarPresentWhenSet verifies that CM_BASE_BRANCH is injected
// into the container env when RunConfig.BaseBranch is non-empty.
func TestBaseBranch_EnvVarPresentWhenSet(t *testing.T) {
	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		assert.Contains(t, cfg.Env, "CM_BASE_BRANCH=main")

		return container.CreateResponse{ID: "bb-test-ctr"}, nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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
	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		assert.Contains(t, cfg.Env, "CM_INTERACTIVE=1")

		return container.CreateResponse{ID: "interactive-test-ctr"}, nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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
	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		for _, e := range cfg.Env {
			assert.False(t, strings.HasPrefix(e, "CM_INTERACTIVE="), "CM_INTERACTIVE must not be set when Interactive is false")
		}

		return container.CreateResponse{ID: "non-interactive-test-ctr"}, nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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
	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		for _, e := range cfg.Env {
			assert.False(t, strings.HasPrefix(e, "CM_BASE_BRANCH="), "CM_BASE_BRANCH must not be set when BaseBranch is empty")
		}

		return container.CreateResponse{ID: "bb-test-ctr"}, nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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
	var (
		capturedCfg  *container.Config
		attachCalled int
	)

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedCfg = cfg

		return container.CreateResponse{ID: "stdin-test-ctr"}, nil
	}
	mock.ContainerAttachFn = func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
		attachCalled++
		// Use a discarding writer so the priming write does not block.
		return &HijackedResponse{Conn: nopWriteCloser{}}, nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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
		var (
			writtenBytes [][]byte
			writeMu      sync.Mutex
		)

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

		mock := successfulMock()
		mock.ContainerAttachFn = func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
			return &HijackedResponse{Conn: spyWriter}, nil
		}

		cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer cbSrv.Close()

		tr := tracker.New()
		cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
		tp := testTokenProvider(t)

		mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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

		mock := successfulMock()
		mock.ContainerAttachFn = func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
			attachCalled++
			_, pw := io.Pipe()

			return &HijackedResponse{Conn: pw}, nil
		}

		cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer cbSrv.Close()

		tr := tracker.New()
		cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
		tp := testTokenProvider(t)

		mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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
		var (
			writtenBytes [][]byte
			writeMu      sync.Mutex
		)

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

		mock := successfulMock()
		mock.ContainerAttachFn = func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
			return &HijackedResponse{Conn: spyWriter}, nil
		}

		cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer cbSrv.Close()

		tr := tracker.New()
		cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
		tp := testTokenProvider(t)

		mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
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
		payload := RunConfig{Mode: ModeTask, CardID: "PROJ-123", Project: "myproj"}
		content := buildPrimingContent(payload)
		assert.Contains(t, content, "PROJ-123")
		assert.Contains(t, content, "get_skill(skill_name='create-plan'")
		assert.Contains(t, content, "card_id='PROJ-123'")
		assert.NotContains(t, content, "base branch")
	})

	t.Run("with base branch", func(t *testing.T) {
		payload := RunConfig{Mode: ModeTask, CardID: "PROJ-456", Project: "myproj", BaseBranch: "main"}
		content := buildPrimingContent(payload)
		assert.Contains(t, content, "PROJ-456")
		assert.Contains(t, content, "get_skill(skill_name='create-plan'")
		assert.Contains(t, content, "main")
		assert.Contains(t, content, "base branch")
	})
}

// blockingWriteCloser blocks every Write until Close is called, at which
// point it returns io.ErrClosedPipe. Used to simulate a wedged hijacked
// socket for TestPrimingWriteStdin_WriteDeadline.
type blockingWriteCloser struct {
	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{done: make(chan struct{})}
}

func (b *blockingWriteCloser) Write(_ []byte) (int, error) {
	<-b.done

	return 0, io.ErrClosedPipe
}

func (b *blockingWriteCloser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}

	b.closed = true

	close(b.done)

	return nil
}

// TestPrimingWriteStdin_WriteDeadline asserts that a priming WriteStdin that
// wedges on the hijacked conn does not stall the Run goroutine: after
// primingWriteTimeout elapses the manager logs and continues into
// waitAndCleanup, and the whole test exits well before any real timeout
// could fire.
func TestPrimingWriteStdin_WriteDeadline(t *testing.T) {
	// Shrink the deadline for the duration of this test so we don't
	// spend 5 s on a synthetic wedge. Snapshot + restore around the run.
	saved := primingWriteTimeout
	primingWriteTimeout = 100 * time.Millisecond

	t.Cleanup(func() { primingWriteTimeout = saved })

	blocker := newBlockingWriteCloser()

	mock := successfulMock()
	mock.ContainerAttachFn = func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
		return &HijackedResponse{Conn: blocker}, nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	payload.Interactive = true
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	start := time.Now()
	done := make(chan struct{})

	go func() {
		mgr.Run(context.Background(), payload)
		mgr.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Run completed without being blocked by the wedged write.
	case <-time.After(5 * time.Second):
		// Unblock any stray goroutine so the test exits cleanly, then fail.
		_ = blocker.Close()

		t.Fatalf("Manager.Run wedged on priming write (elapsed %s)", time.Since(start))
	}

	// With primingWriteTimeout at 100ms, end-to-end Run should take well
	// under a second. A slow CI may add some, so allow 3 s of slack.
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 3*time.Second,
		"priming write deadline did not bound the Run goroutine (elapsed %s)", elapsed)

	// Release the blocker so the detached priming-write goroutine unwinds
	// and doesn't leak into later tests. Safe to call even if already closed.
	_ = blocker.Close()

	// Tracker entry must have been removed via the normal waitAndCleanup
	// path — confirming that the wedge did not short-circuit cleanup.
	assert.Equal(t, 0, tr.Count())
}

// TestWritePrimingTimeoutMarksStdinClosed verifies that when the priming
// WriteStdin times out and we force-close the writer, the tracker's stdin
// state is also flipped to "closed" (via MarkStdinClosed) so a subsequent
// /message call returns ErrStdinClosed (mapped to 410 Gone) instead of
// surfacing the closed-writer I/O error as a generic 500.
func TestWritePrimingTimeoutMarksStdinClosed(t *testing.T) {
	saved := primingWriteTimeout
	primingWriteTimeout = 50 * time.Millisecond

	t.Cleanup(func() { primingWriteTimeout = saved })

	tr := tracker.New()
	mgr := NewManager(successfulMock(), tr, nil, nil, nil, testConfig(t), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	// Attach a blocking writer so the priming WriteStdin wedges on its
	// Write call until the writer is force-closed by the timeout path.
	blocker := newBlockingWriteCloser()
	tr.SetStdin(payload.Project, payload.CardID, blocker, nil)

	// Run writePrimingWithTimeout directly to exercise the timeout branch
	// without spinning up the full Run+attach machinery.
	mgr.writePrimingWithTimeout(payload, "ctr-priming-timeout", []byte("hello"), blocker)

	// Wait for the detached priming-write goroutine to unwind (the wg
	// guards leak detection in other tests; we just need the Mark to
	// have landed by the time we assert).
	mgr.Wait()

	// After the timeout MarkStdinClosed must have been called on the
	// tracker entry; a subsequent WriteStdin must therefore return
	// ErrStdinClosed (NOT a closed-writer I/O error and NOT
	// ErrNoStdinAttached).
	err := tr.WriteStdin(payload.Project, payload.CardID, []byte("after"))
	require.ErrorIs(t, err, tracker.ErrStdinClosed,
		"WriteStdin after priming-timeout must return ErrStdinClosed (-> 410 Gone)")
	require.NotErrorIs(t, err, tracker.ErrNoStdinAttached)
}

func TestInteractive_FalseNoStdinFlagsNoAttach(t *testing.T) {
	var (
		capturedCfg  *container.Config
		attachCalled int
	)

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedCfg = cfg

		return container.CreateResponse{ID: "non-interactive-stdin-ctr"}, nil
	}
	mock.ContainerAttachFn = func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
		attachCalled++
		_, pw := io.Pipe()

		return &HijackedResponse{Conn: pw}, nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

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

// TestWaitAndCleanup_ParentContextCanceled verifies that when the parent ctx
// is canceled mid-wait, the manager takes the kill path: the container is
// stopped and removed, the tracker slot is freed, a "container canceled"
// system event is emitted, AND reportFailure is invoked with a "killed by
// operator" message so ContextMatrix can transition the card out of
// `running`.
func TestWaitAndCleanup_ParentContextCanceled(t *testing.T) {
	type statusReport struct {
		status, message string
	}

	var (
		stopCalled       atomic.Bool
		removeCalled     atomic.Bool
		reportedStatuses = make(chan statusReport, 4)
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			RunnerStatus string `json:"runner_status"`
			Message      string `json:"message"`
		}

		_ = json.Unmarshal(body, &req)
		select {
		case reportedStatuses <- statusReport{req.RunnerStatus, req.Message}:
		default:
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	// ContainerWait blocks forever so the only exit from waitAndCleanup is
	// the `<-ctx.Done()` branch. We deliberately do NOT send on errCh — if we
	// did, the errCh branch plus waitCtx.Err() != nil would classify the
	// shutdown as a timeout instead of a cancel.
	mock.ContainerWaitFn = func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		return make(chan container.WaitResponse), make(chan error)
	}
	mock.ContainerStopFn = func(_ context.Context, _ string, _ container.StopOptions) error {
		stopCalled.Store(true)

		return nil
	}
	mock.ContainerRemoveFn = func(_ context.Context, _ string, _ container.RemoveOptions) error {
		removeCalled.Store(true)

		return nil
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	b := newRecordingBroadcaster()
	mgr := NewManager(mock, tr, cb, tp, b.Broadcaster(), testConfig(t), testLogger())

	payload := testPayload()

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  cancel,
	}))

	mgr.Run(ctx, payload)

	// Wait until startContainer has set the container ID — confirms the
	// run goroutine has entered waitAndCleanup and registered its defers,
	// so cancel() exercises the ctx.Done() branch instead of racing
	// startContainer. Mirrors the pattern used by
	// TestKill_InteractiveContainer_RemovesContainer and friends.
	require.Eventually(t, func() bool {
		snap, ok := tr.Snapshot(payload.Project, payload.CardID)

		return ok && snap.ContainerID != ""
	}, 5*time.Second, 10*time.Millisecond,
		"tracker must have container ID before cancel")

	cancel()
	mgr.Wait()

	// The recording goroutine drains asynchronously — give it a chance.
	require.Eventually(t, func() bool {
		return strings.Contains(joinContents(b.filterType("system")), "container canceled")
	}, 2*time.Second, 10*time.Millisecond,
		"a 'container canceled' system event must be emitted")

	assert.True(t, stopCalled.Load(), "killContainer must stop the container")
	assert.True(t, removeCalled.Load(), "container must be removed")
	assert.Equal(t, 0, tr.Count(), "tracker slot must be freed")

	// reportFailure must fire via a detached context so CM sees the
	// terminal status even though the parent ctx is already cancelled.
	var (
		sawFailed           bool
		sawKilledByOperator bool
	)

drainLoop:
	for {
		select {
		case s := <-reportedStatuses:
			if s.status == "failed" {
				sawFailed = true
			}

			if strings.Contains(s.message, "killed by operator") {
				sawKilledByOperator = true
			}
		default:
			break drainLoop
		}
	}

	assert.True(t, sawFailed, "reportFailure must fire on parent-context cancel")
	assert.True(t, sawKilledByOperator, "failure message must be 'killed by operator'")
}

// TestStartFailure_ReportsFailureDespiteCancelledContext verifies that when
// ContainerStart fails while the parent ctx has already been cancelled (e.g.
// the Kill webhook raced the start goroutine), reportFailure still fires via
// a detached context so CM sees the `failed` status instead of the card
// getting stuck in `running`.
func TestStartFailure_ReportsFailureDespiteCancelledContext(t *testing.T) {
	type statusReport struct {
		status, message string
	}

	reportedStatuses := make(chan statusReport, 4)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			RunnerStatus string `json:"runner_status"`
			Message      string `json:"message"`
		}

		_ = json.Unmarshal(body, &req)
		select {
		case reportedStatuses <- statusReport{req.RunnerStatus, req.Message}:
		default:
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	// We need to cancel the parent ctx BEFORE ContainerStart returns its
	// error so the failure path sees ctx.Err() != nil. The mock blocks on the
	// passed-in context until it is cancelled, then returns the start failure
	// — mimicking a real daemon call that was in flight when the operator
	// hit /kill.
	startEntered := make(chan struct{})

	mock := successfulMock()
	mock.ContainerStartFn = func(ctx context.Context, _ string, _ container.StartOptions) error {
		close(startEntered)
		<-ctx.Done()

		return fmt.Errorf("synthetic start failure")
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  cancel,
	}))

	mgr.Run(ctx, payload)

	// Wait for ContainerStart to be entered, then cancel so its ctx is dead
	// before the start-failure branch runs reportFailure.
	<-startEntered
	cancel()
	mgr.Wait()

	assert.Equal(t, 0, tr.Count(), "tracker slot must be freed after start failure")

	var (
		sawFailed     bool
		sawStartMsg   bool
		sawStartError bool
	)

drainLoop:
	for {
		select {
		case s := <-reportedStatuses:
			if s.status == "failed" {
				sawFailed = true
			}

			if strings.Contains(s.message, "start failed") {
				sawStartMsg = true
			}

			if strings.Contains(s.message, "synthetic start failure") {
				sawStartError = true
			}
		default:
			break drainLoop
		}
	}

	assert.True(t, sawFailed,
		"reportFailure must fire even when the parent ctx was cancelled during start")
	assert.True(t, sawStartMsg, "message must include 'start failed' prefix")
	assert.True(t, sawStartError, "message must include the underlying error")
}

// TestWaitAndCleanup_Timeout exercises the timeout path: ContainerWait blocks
// until waitCtx's deadline expires. killContainer must be invoked with the
// correct container ID and a failed status callback issued.
func TestWaitAndCleanup_Timeout(t *testing.T) {
	var (
		stopID           atomic.Value // string
		reportedStatuses = make(chan string, 4)
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			RunnerStatus string `json:"runner_status"`
		}

		_ = json.Unmarshal(body, &req)
		select {
		case reportedStatuses <- req.RunnerStatus:
		default:
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		return container.CreateResponse{ID: "timeout-ctr-id"}, nil
	}
	// ContainerWait blocks until its ctx (waitCtx) expires — then sends the
	// deadline error on errCh so the wait-error branch fires and
	// waitCtx.Err() is non-nil, triggering the timeout path.
	mock.ContainerWaitFn = func(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		errCh := make(chan error, 1)

		go func() {
			<-ctx.Done()

			errCh <- ctx.Err()
		}()

		return make(chan container.WaitResponse), errCh
	}
	mock.ContainerStopFn = func(_ context.Context, id string, _ container.StopOptions) error {
		stopID.Store(id)

		return nil
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	cfg := testConfig(t)
	cfg.ContainerTimeout = "100ms"
	cfg.ParseContainerTimeout()

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	id, _ := stopID.Load().(string)
	assert.Equal(t, "timeout-ctr-id", id, "killContainer must be called with the created container ID")

	var sawFailed bool

	for {
		select {
		case s := <-reportedStatuses:
			if s == "failed" {
				sawFailed = true
			}
		default:
			assert.True(t, sawFailed, "reportFailure must be called on container timeout")

			return
		}
	}
}

// TestWaitAndCleanup_ErrChFromContainerWait exercises the generic errCh path:
// ContainerWait emits a non-timeout error (e.g. daemon disconnect). The
// manager must kill the container, report failure, and clean up the tracker.
func TestWaitAndCleanup_ErrChFromContainerWait(t *testing.T) {
	var (
		stopCalled       atomic.Bool
		removeCalled     atomic.Bool
		reportedStatuses = make(chan string, 4)
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			RunnerStatus string `json:"runner_status"`
		}

		_ = json.Unmarshal(body, &req)
		select {
		case reportedStatuses <- req.RunnerStatus:
		default:
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	mock.ContainerWaitFn = func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		errCh := make(chan error, 1)
		errCh <- fmt.Errorf("docker daemon closed connection")

		return make(chan container.WaitResponse), errCh
	}
	mock.ContainerStopFn = func(_ context.Context, _ string, _ container.StopOptions) error {
		stopCalled.Store(true)

		return nil
	}
	mock.ContainerRemoveFn = func(_ context.Context, _ string, _ container.RemoveOptions) error {
		removeCalled.Store(true)

		return nil
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	assert.True(t, stopCalled.Load(), "killContainer must fire on errCh path")
	assert.True(t, removeCalled.Load(), "container must be removed after errCh path")
	assert.Equal(t, 0, tr.Count(), "tracker slot must be freed")

	var sawFailed bool

	for {
		select {
		case s := <-reportedStatuses:
			if s == "failed" {
				sawFailed = true
			}
		default:
			assert.True(t, sawFailed, "reportFailure must be called on errCh error")

			return
		}
	}
}

// TestWaitAndCleanup_ParentCancelRacingErrCh_ClassifiesAsKilled verifies the
// disambiguation between OutcomeTimeout and OutcomeKilled when the Docker
// SDK's ContainerWait writes a context-canceled error into errCh AT THE SAME
// TIME the parent ctx is canceled.
//
// Go's select picks pseudo-randomly between errCh and ctx.Done(); when errCh
// wins, the cleanup branch used to read `waitCtx.Err() != nil` and classify
// the cancellation as a timeout, silently reporting the wrong terminal state
// to ContextMatrix and the metrics histogram.
//
// The fix gates the timeout branch on
// `errors.Is(waitCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil`
// and routes parent-cancellation to the same path as the ctx.Done() branch.
func TestWaitAndCleanup_ParentCancelRacingErrCh_ClassifiesAsKilled(t *testing.T) {
	type statusReport struct {
		status, message string
	}

	reportedStatuses := make(chan statusReport, 4)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			RunnerStatus string `json:"runner_status"`
			Message      string `json:"message"`
		}

		_ = json.Unmarshal(body, &req)
		select {
		case reportedStatuses <- statusReport{req.RunnerStatus, req.Message}:
		default:
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	// errCh is buffered with a context-canceled error from the start; the
	// mock's ContainerWait returns it unconditionally so the wait-side
	// select sees both errCh and ctx.Done() ready at the same time (the
	// race we are guarding against).
	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		return container.CreateResponse{ID: "racey-ctr-id"}, nil
	}
	mock.ContainerWaitFn = func(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		errCh := make(chan error, 1)

		go func() {
			// Wait for parent ctx (operator /kill) to cancel, THEN emit
			// the context.Canceled into errCh — Docker SDK behaviour.
			<-ctx.Done()

			errCh <- ctx.Err()
		}()

		return make(chan container.WaitResponse), errCh
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	cfg := testConfig(t)
	// Use a comfortably-long container timeout so waitCtx never fires its
	// real deadline; the only way errCh gets populated is via parent ctx.
	cfg.ContainerTimeout = "1h"
	cfg.ParseContainerTimeout()

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	payload := testPayload()

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  cancel,
	}))

	mgr.Run(ctx, payload)

	// Wait until the run goroutine has registered the container ID so the
	// kill exercises the wait path, not the start path.
	require.Eventually(t, func() bool {
		snap, ok := tr.Snapshot(payload.Project, payload.CardID)

		return ok && snap.ContainerID != ""
	}, 5*time.Second, 10*time.Millisecond,
		"tracker must have container ID before cancel")

	cancel()
	mgr.Wait()

	// The failure callback must carry the "killed by operator" message,
	// not the timeout message. Before the fix the message read "container
	// timed out after 1h" because waitCtx.Err() was non-nil.
	var (
		sawFailed  bool
		sawKilled  bool
		sawTimeout bool
	)

drainLoop:
	for {
		select {
		case s := <-reportedStatuses:
			if s.status == "failed" {
				sawFailed = true
			}

			if strings.Contains(s.message, "killed by operator") {
				sawKilled = true
			}

			if strings.Contains(s.message, "timed out") {
				sawTimeout = true
			}
		default:
			break drainLoop
		}
	}

	assert.True(t, sawFailed, "reportFailure must fire even when errCh wins the select race")
	assert.True(t, sawKilled, "race outcome must be classified as killed_by_operator, not timeout")
	assert.False(t, sawTimeout, "parent-ctx-cancel race must NOT be classified as a timeout")
}

// TestKillContainer_Success directly drives the killContainer helper and
// verifies ContainerStop is called with the provided ID and grace period.
func TestKillContainer_Success(t *testing.T) {
	var (
		gotID      string
		gotTimeout int
	)

	mock := successfulMock()
	mock.ContainerStopFn = func(_ context.Context, id string, opts container.StopOptions) error {
		gotID = id

		if opts.Timeout != nil {
			gotTimeout = *opts.Timeout
		}

		return nil
	}

	mgr := NewManager(mock, tracker.New(), nil, testPATProvider(t), nil, testConfig(t), testLogger())
	mgr.killContainer(context.Background(), "target-id", testLogger())

	assert.Equal(t, "target-id", gotID)
	assert.Equal(t, 10, gotTimeout, "killContainer must use the stopGracePeriod")
}

// TestKillContainer_StopError verifies that a ContainerStop failure is logged
// and swallowed — killContainer must not propagate errors because it runs
// inside deferred cleanup where there is no meaningful recovery path.
func TestKillContainer_StopError(t *testing.T) {
	mock := successfulMock()
	mock.ContainerStopFn = func(_ context.Context, _ string, _ container.StopOptions) error {
		return fmt.Errorf("docker not reachable")
	}

	mgr := NewManager(mock, tracker.New(), nil, testPATProvider(t), nil, testConfig(t), testLogger())
	// Must not panic or block.
	assert.NotPanics(t, func() {
		mgr.killContainer(context.Background(), "id", testLogger())
	})
}

// TestRemoveContainer_Failure verifies that a ContainerRemove failure is
// logged and swallowed (same rationale as killContainer).
func TestRemoveContainer_Failure(t *testing.T) {
	mock := successfulMock()
	mock.ContainerRemoveFn = func(_ context.Context, _ string, _ container.RemoveOptions) error {
		return fmt.Errorf("container busy")
	}

	mgr := NewManager(mock, tracker.New(), nil, testPATProvider(t), nil, testConfig(t), testLogger())

	assert.NotPanics(t, func() {
		mgr.removeContainer(context.Background(), "id", testLogger())
	})
}

// TestRun_PanicInStartContainer_RecoveryFreesTrackerAndReports verifies the
// recover() in Manager.Run: when startContainer panics (here, injected via
// ContainerCreate), the deferred recover must free the tracker slot and
// report the failure via the callback.
func TestRun_PanicInStartContainer_RecoveryFreesTrackerAndReports(t *testing.T) {
	reportedStatuses := make(chan string, 4)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			RunnerStatus string `json:"runner_status"`
		}

		_ = json.Unmarshal(body, &req)
		select {
		case reportedStatuses <- req.RunnerStatus:
		default:
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	// Inject a panic deep in the start-container path. Using ContainerStart
	// panics after ContainerCreate succeeds, so the panic fires inside
	// startContainer and exercises the Run() recover path.
	mock.ContainerStartFn = func(_ context.Context, _ string, _ container.StartOptions) error {
		panic("docker sdk exploded")
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	// Must not crash the test binary.
	assert.NotPanics(t, func() {
		mgr.Run(context.Background(), payload)
		mgr.Wait()
	})

	assert.Equal(t, 0, tr.Count(), "tracker slot must be freed after panic recovery")

	var sawFailed bool

	for {
		select {
		case s := <-reportedStatuses:
			if s == "failed" {
				sawFailed = true
			}
		default:
			assert.True(t, sawFailed, "reportFailure must fire from the recover() path")

			return
		}
	}
}

// TestStreamLogs_StderrScannerPanic asserts that the per-goroutine recover
// inside streamLogs isolates a panic in one of the three child goroutines
// (here: stdcopy, which calls Read on the injected panicReader). Before the
// fix a panic in these goroutines unwound the entire runner process; now
// the runner must continue, the panic is surfaced as a `system` LogEntry so
// operators see it, and waitAndCleanup must still run to completion so the
// tracker entry is removed.
func TestStreamLogs_StderrScannerPanic(t *testing.T) {
	mock := successfulMock()
	mock.ContainerLogsFn = func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
		// panicReader's Read panics on first call. stdcopy.StdCopy will
		// panic trying to read from it; the recover() installed by
		// streamLogs must catch it and emit a system event.
		return io.NopCloser(&panicReader{}), nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	broadcaster := newRecordingBroadcaster()
	defer broadcaster.Close()

	mgr := NewManager(mock, tr, cb, tp, broadcaster.inner, testConfig(t), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	assert.NotPanics(t, func() {
		mgr.Run(context.Background(), payload)
		mgr.Wait()
	})
	assert.Equal(t, 0, tr.Count(), "tracker entry must be removed even after child goroutine panic")

	// Assert the system event announcing the recovered panic was published.
	// We don't pin the exact goroutine name (stdcopy vs stderr_scanner vs
	// logparser) because which child catches the panic depends on how
	// stdcopy propagates it — the contract is "a system 'panicked' event
	// shows up", not "this specific goroutine".
	entries := broadcaster.Entries()

	var panicSystemSeen bool

	for _, e := range entries {
		if e.Type == "system" && strings.Contains(e.Content, "panicked") {
			panicSystemSeen = true

			break
		}
	}

	assert.True(t, panicSystemSeen, "expected a system LogEntry containing 'panicked'; got %d entries", len(entries))
}

// panicReader's Read method panics on first call — used only by the skipped
// TestStreamLogs_StderrScannerPanic test body.
type panicReader struct{}

func (p *panicReader) Read(_ []byte) (int, error) {
	panic("synthetic read panic for streamLogs recover test")
}

// recordingBroadcaster wraps logbroadcast.Broadcaster and captures published
// entries so tests can assert on emitted LogEntry types/contents without
// spawning a goroutine to drain a subscriber channel.
type recordingBroadcaster struct {
	inner *logbroadcast.Broadcaster
	ch    <-chan logbroadcast.LogEntry
	unsub func()

	mu      sync.Mutex
	entries []logbroadcast.LogEntry

	done chan struct{}
}

func newRecordingBroadcaster() *recordingBroadcaster {
	b := logbroadcast.NewBroadcaster(nil, nil)
	ch, unsub := b.Subscribe("")

	rec := &recordingBroadcaster{
		inner: b,
		ch:    ch,
		unsub: unsub,
		done:  make(chan struct{}),
	}

	go func() {
		for {
			select {
			case <-rec.done:
				return
			case e, ok := <-ch:
				if !ok {
					return
				}

				rec.mu.Lock()
				rec.entries = append(rec.entries, e)
				rec.mu.Unlock()
			}
		}
	}()

	return rec
}

func (r *recordingBroadcaster) Broadcaster() *logbroadcast.Broadcaster {
	return r.inner
}

// Close stops the drain goroutine and releases the subscription.
func (r *recordingBroadcaster) Close() {
	close(r.done)
	r.unsub()
}

// Entries returns a snapshot of all published LogEntry values so far.
func (r *recordingBroadcaster) Entries() []logbroadcast.LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]logbroadcast.LogEntry, len(r.entries))
	copy(out, r.entries)

	return out
}

func (r *recordingBroadcaster) filterType(typ string) []logbroadcast.LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]logbroadcast.LogEntry, 0, len(r.entries))

	for _, e := range r.entries {
		if e.Type == typ {
			out = append(out, e)
		}
	}

	return out
}

func joinContents(entries []logbroadcast.LogEntry) string {
	var b strings.Builder

	for _, e := range entries {
		b.WriteString(e.Content)
		b.WriteString("\n")
	}

	return b.String()
}

func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
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
			name:     "ssh scheme with non-GitHub host",
			input:    "ssh://git@bitbucket.org/team/project.git",
			expected: "https://bitbucket.org/team/project.git",
		},
		{
			name:     "https passthrough",
			input:    "https://github.com/org/repo.git",
			expected: "https://github.com/org/repo.git",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, normalizeRepoURL(tt.input))
		})
	}
}

// TestPullImage_EmptyPolicyReturnsError asserts that pullImage fails fast with
// a descriptive error when ImagePullPolicy is unset, instead of silently
// falling back to PullAlways.
func TestPullImage_EmptyPolicyReturnsError(t *testing.T) {
	// MockDockerClient with no function fields set: any call into the
	// docker client would fall through to a default that either succeeds
	// or returns a different error. We rely on pullImage short-circuiting
	// before any docker call when the policy is empty.
	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			t.Fatal("ImagePull must not be called when policy is unset")

			return nil, nil
		},
	}

	cfg := &config.Config{
		BaseImage:        "test-image:latest",
		ContainerTimeout: "1h",
		// ImagePullPolicy intentionally left empty.
	}
	cfg.ParseContainerTimeout()

	mgr := NewManager(mock, tracker.New(), nil, testPATProvider(t), nil, cfg, testLogger())

	err := mgr.pullImage(context.Background(), "test-image:latest")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image_pull_policy is unset")
	assert.Contains(t, err.Error(), "programming error")
}

// TestPullImage_RegistryErrorInStream verifies pullImage surfaces a
// registry-side error reported via the ImagePull NDJSON stream. Pre-fix
// these errors were silently swallowed by io.Copy(io.Discard, reader),
// and the failure only manifested later as the much less actionable
// `ContainerCreate: no such image`.
func TestPullImage_RegistryErrorInStream(t *testing.T) {
	tests := []struct {
		name       string
		streamBody string
		wantSubstr string
	}{
		{
			name:       "top-level error field",
			streamBody: `{"status":"Pulling fs layer","id":"abc"}` + "\n" + `{"error":"manifest unknown"}` + "\n",
			wantSubstr: "manifest unknown",
		},
		{
			name:       "errorDetail.message field",
			streamBody: `{"errorDetail":{"message":"unauthorized: authentication required"}}` + "\n",
			wantSubstr: "unauthorized: authentication required",
		},
		{
			name: "error after progress lines",
			streamBody: `{"status":"Pulling fs layer","id":"abc"}` + "\n" +
				`{"status":"Downloading","id":"abc"}` + "\n" +
				`{"error":"pull access denied"}` + "\n",
			wantSubstr: "pull access denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &MockDockerClient{
				ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader(tt.streamBody)), nil
				},
			}

			cfg := &config.Config{
				BaseImage:        "test-image:latest",
				ContainerTimeout: "1h",
				ImagePullPolicy:  config.PullAlways,
			}
			cfg.ParseContainerTimeout()

			mgr := NewManager(mock, tracker.New(), nil, testPATProvider(t), nil, cfg, testLogger())

			err := mgr.pullImage(context.Background(), "test-image:latest")
			require.Error(t, err, "stream containing %q must surface as error", tt.wantSubstr)
			assert.Contains(t, err.Error(), tt.wantSubstr)
		})
	}
}

// TestPullImage_SuccessStreamReturnsNil verifies a clean NDJSON progress
// stream (no error/errorDetail frames) returns nil and does not falsely
// flag a successful pull as failed.
func TestPullImage_SuccessStreamReturnsNil(t *testing.T) {
	body := `{"status":"Pulling from library/test","id":"latest"}` + "\n" +
		`{"status":"Pulling fs layer","id":"abc"}` + "\n" +
		`{"status":"Downloading","id":"abc","progressDetail":{"current":100,"total":100}}` + "\n" +
		`{"status":"Pull complete","id":"abc"}` + "\n"

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(body)), nil
		},
	}

	cfg := &config.Config{
		BaseImage:        "test-image:latest",
		ContainerTimeout: "1h",
		ImagePullPolicy:  config.PullAlways,
	}
	cfg.ParseContainerTimeout()

	mgr := NewManager(mock, tracker.New(), nil, testPATProvider(t), nil, cfg, testLogger())

	err := mgr.pullImage(context.Background(), "test-image:latest")
	require.NoError(t, err)
}

// TestPullImage_MalformedStreamLogsButDoesNotError verifies an undecodable
// progress line stops decoding but does not return an error — partial
// valid prefixes can still represent a successful pull on older daemons.
// (See the inline comment in pullImage for the rationale.)
func TestPullImage_MalformedStreamLogsButDoesNotError(t *testing.T) {
	body := `{"status":"Pulling fs layer"}` + "\n" + `not-json-at-all` + "\n"

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(body)), nil
		},
	}

	cfg := &config.Config{
		BaseImage:        "test-image:latest",
		ContainerTimeout: "1h",
		ImagePullPolicy:  config.PullAlways,
	}
	cfg.ParseContainerTimeout()

	mgr := NewManager(mock, tracker.New(), nil, testPATProvider(t), nil, cfg, testLogger())

	err := mgr.pullImage(context.Background(), "test-image:latest")
	assert.NoError(t, err, "malformed progress line must not be a hard failure")
}

// TestWaitAndCleanup_MessageDuringCleanupGets404 verifies that the tracker
// entry is removed BEFORE the Docker container is removed during cleanup
// (H21 in REVIEW.md). The old defer order left the tracker entry in place
// while the container was already gone, so a /message or /promote arriving
// in that window tried to write stdin to a dead container and produced a
// 500. After the fix, the tracker is unpublished first and the same request
// gets the correct 404.
//
// We exercise the race by blocking ContainerRemove on a channel. Inside
// that blocked window we inspect the tracker state: with the correct LIFO
// ordering (tracker.Remove runs first), the entry must already be gone by
// the time ContainerRemove is entered — so the webhook's "is there a
// container tracked?" check returns false → 404.
func TestWaitAndCleanup_MessageDuringCleanupGets404(t *testing.T) {
	var (
		release           = make(chan struct{})
		trackedDuringRm   atomic.Bool
		containerRemoved  atomic.Bool
		messageAttemptErr atomic.Value // holds error
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		return container.CreateResponse{ID: "ctr-55"}, nil
	}
	mock.ContainerWaitFn = func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		ch := make(chan container.WaitResponse, 1)
		ch <- container.WaitResponse{StatusCode: 0}

		return ch, make(chan error)
	}
	mock.ContainerRemoveFn = func(_ context.Context, _ string, _ container.RemoveOptions) error {
		// The defer LIFO fix means tracker.Remove runs BEFORE
		// removeContainer. Record the tracker state at entry so the
		// assertion below can catch a regression.
		_, stillTracked := tr.Snapshot("my-project", "PROJ-042")
		trackedDuringRm.Store(stillTracked)

		// Simulate a concurrent /message path that would take: Has() check
		// → WriteStdin. Both should now fail with "not tracked" shape,
		// producing a 404 at the webhook layer instead of a 500.
		err := tr.WriteStdin("my-project", "PROJ-042", []byte("late\n"))
		messageAttemptErr.Store(err)

		<-release
		containerRemoved.Store(true)

		return nil
	}

	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:    payload.CardID,
		Project:   payload.Project,
		StartedAt: time.Now(),
	}))

	done := make(chan struct{})

	go func() {
		mgr.Run(context.Background(), payload)
		mgr.Wait()
		close(done)
	}()

	// Let ContainerRemove get called and block.
	require.Eventually(t, func() bool {
		return messageAttemptErr.Load() != nil
	}, 2*time.Second, 10*time.Millisecond,
		"ContainerRemove must be entered so the test can inspect tracker state")

	// H21 assertion: by the time ContainerRemove is entered, the tracker
	// entry must already be gone — tracker.Remove is the last defer (first
	// to execute) in waitAndCleanup.
	assert.False(t, trackedDuringRm.Load(),
		"tracker.Remove must run before ContainerRemove so /message gets 404")

	// The simulated /message write must have failed with the no-container-
	// tracked error (not an ErrNoStdinAttached, not nil). The handler maps
	// that shape to 404.
	errV := messageAttemptErr.Load()
	require.NotNil(t, errV, "WriteStdin must have been attempted during cleanup")

	err, _ := errV.(error)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no container tracked",
		"WriteStdin during cleanup must return the 'no container tracked' shape (404), not a 500")
	require.NotErrorIs(t, err, tracker.ErrNoStdinAttached,
		"must not surface as ErrNoStdinAttached (409); the entry is gone, not just stdin")

	close(release)
	<-done

	assert.True(t, containerRemoved.Load(), "ContainerRemove must complete after release")
	assert.Equal(t, 0, tr.Count(), "tracker must end empty")
}

// TestIdleWatchdog_KillsOnSilence verifies the idle-output watchdog
// kills the container and emits a "idle timeout" system event when no output
// has been observed for longer than IdleOutputTimeout. Exercises the goroutine
// directly (rather than via streamLogs + a fake stream) so the test is
// deterministic and does not depend on docker multiplexed framing.
func TestIdleWatchdog_KillsOnSilence(t *testing.T) {
	cfg := testConfig(t)
	cfg.IdleOutputTimeout = 50 * time.Millisecond
	// Shrink the poll tick so the watchdog reacts promptly inside the test.
	cfg.IdleWatchdogInterval = 10 * time.Millisecond

	mock := successfulMock()

	tr := tracker.New()
	broadcaster := newRecordingBroadcaster()
	t.Cleanup(broadcaster.Close)

	mgr := NewManager(mock, tr, nil, nil, broadcaster.Broadcaster(), cfg, testLogger())

	// Register a tracker entry whose Cancel is observable. A successful Kill
	// path invokes tracker.Cancel, which flips cancelled.
	var cancelled atomic.Bool

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  func() { cancelled.Store(true) },
	}))

	// Seed lastOutputAt to a time well before now so the very first poll
	// deems the container idle.
	var lastOutputAt atomic.Pointer[time.Time]

	stale := time.Now().Add(-time.Hour)
	lastOutputAt.Store(&stale)

	done := make(chan struct{})

	ctx := t.Context()

	go func() {
		defer close(done)

		mgr.runIdleWatchdog(ctx, make(chan struct{}), "ctr-idle", payload, testLogger(), &lastOutputAt, cfg.IdleOutputTimeout)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire within 2s")
	}

	assert.True(t, cancelled.Load(), "watchdog must Cancel the tracker entry")

	require.Eventually(t, func() bool {
		return strings.Contains(joinContents(broadcaster.filterType("system")), "idle timeout")
	}, 2*time.Second, 10*time.Millisecond,
		"an 'idle timeout' system event must be emitted")
}

// TestIdleWatchdog_DoesNotKillWhileActive verifies the watchdog stays silent
// while the container keeps publishing output faster than the idle timeout.
func TestIdleWatchdog_DoesNotKillWhileActive(t *testing.T) {
	cfg := testConfig(t)
	cfg.IdleOutputTimeout = 50 * time.Millisecond
	// Shrink the poll tick so the watchdog reacts promptly inside the test.
	cfg.IdleWatchdogInterval = 5 * time.Millisecond

	mock := successfulMock()

	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, cfg, testLogger())

	var cancelled atomic.Bool

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  func() { cancelled.Store(true) },
	}))

	var lastOutputAt atomic.Pointer[time.Time]

	now := time.Now()
	lastOutputAt.Store(&now)

	ctx := t.Context()

	stopFeed := make(chan struct{})

	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stopFeed:
				return
			case t := <-ticker.C:
				lastOutputAt.Store(&t)
			}
		}
	}()

	done := make(chan struct{})

	go func() {
		defer close(done)

		mgr.runIdleWatchdog(ctx, stopFeed, "ctr-active", payload, testLogger(), &lastOutputAt, cfg.IdleOutputTimeout)
	}()

	// Feed events for 200 ms; watchdog must remain quiet.
	time.Sleep(200 * time.Millisecond)
	close(stopFeed) // closing stops both the feeder and the watchdog

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("watchdog did not exit after done was closed")
	}

	assert.False(t, cancelled.Load(), "watchdog must not fire while output is flowing")
}

// TestIdleWatchdog_Disabled verifies that IdleOutputTimeout=0 prevents the
// watchdog from being spawned at all: streamLogs runs to completion without a
// kill even though the stream is empty and lastOutputAt never advances.
func TestIdleWatchdog_Disabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.IdleOutputTimeout = 0 // disabled

	mock := successfulMock()

	// Empty log stream: logparser will hit EOF immediately, streamLogs done
	// closes, and since the watchdog is disabled no Kill path fires.
	mock.ContainerLogsFn = func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()

	// Count Cancel invocations. waitAndCleanup invokes tracker.Remove on
	// normal teardown, which now calls the stored Cancel exactly once (H4
	// fix). If the watchdog ALSO fires it would call Kill -> tracker.Cancel
	// -> info.Cancel, bumping the counter past one.
	var cancelCount atomic.Int32

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  func() { cancelCount.Add(1) },
	}))

	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	assert.LessOrEqual(t, cancelCount.Load(), int32(1),
		"watchdog must not fire when IdleOutputTimeout is 0 (only tracker.Remove may invoke Cancel)")
}

// TestIdleWatchdog_PanicRecovery verifies that a panic inside the watchdog's
// inner Kill path (here, injected via a tracker Cancel func that panics) is
// caught by the deferred recover so the goroutine exits cleanly instead of
// unwinding into the runner crash handler. The PanicRecoveredTotal counter
// must be incremented with the GoroutineIdleWatchdog label so operators can
// alert on this specific class of failure.
func TestIdleWatchdog_PanicRecovery(t *testing.T) {
	cfg := testConfig(t)
	cfg.IdleOutputTimeout = 50 * time.Millisecond
	cfg.IdleWatchdogInterval = 10 * time.Millisecond

	mock := successfulMock()

	tr := tracker.New()
	broadcaster := newRecordingBroadcaster()
	t.Cleanup(broadcaster.Close)

	mx := metrics.New()

	mgr := NewManager(mock, tr, nil, nil, broadcaster.Broadcaster(), cfg, testLogger()).WithMetrics(mx)

	// The watchdog fires once, calls m.Kill, which calls tracker.Cancel,
	// which invokes the stored Cancel func. Make Cancel panic so the
	// runIdleWatchdog deferred recover is exercised.
	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  func() { panic("synthetic watchdog Cancel panic") },
	}))

	// Seed lastOutputAt well in the past so the very first poll deems the
	// container idle and triggers the kill path.
	var lastOutputAt atomic.Pointer[time.Time]

	stale := time.Now().Add(-time.Hour)
	lastOutputAt.Store(&stale)

	done := make(chan struct{})

	// Run on a goroutine and assert it does not propagate the panic up to
	// the test goroutine — recovery must keep the program alive.
	go func() {
		defer close(done)

		assert.NotPanics(t, func() {
			mgr.runIdleWatchdog(t.Context(), make(chan struct{}), "ctr-panic",
				payload, testLogger(), &lastOutputAt, cfg.IdleOutputTimeout)
		}, "runIdleWatchdog must recover from a panic in its inner Kill path")
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog goroutine did not exit within 2s after panic")
	}

	// The deferred recover must bump cmr_panic_recovered_total{goroutine=idle_watchdog}.
	got := panicCounterValue(t, mx, metrics.GoroutineIdleWatchdog)
	assert.InDelta(t, 1.0, got, 0.0,
		"PanicRecoveredTotal{goroutine=idle_watchdog} must be 1 after recovery")

	// The system entry surfaced via m.handlePanic should be visible on the
	// broadcaster — operators observe the internal-error event in /logs.
	require.Eventually(t, func() bool {
		for _, e := range broadcaster.Entries() {
			if e.Type == "system" && strings.Contains(e.Content, "idle watchdog panicked") {
				return true
			}
		}

		return false
	}, 2*time.Second, 10*time.Millisecond,
		"a system 'idle watchdog panicked' event must be published after recovery")
}

// panicCounterValue returns the value of cmr_panic_recovered_total for the
// given goroutine label, or 0 if no series exists for that label.
func panicCounterValue(t *testing.T, mx *metrics.Metrics, goroutine string) float64 {
	t.Helper()

	families, err := mx.Registry.Gather()
	require.NoError(t, err)

	for _, fam := range families {
		if fam.GetName() != "cmr_panic_recovered_total" {
			continue
		}

		for _, series := range fam.Metric {
			for _, lp := range series.Label {
				if lp.GetName() == "goroutine" && lp.GetValue() == goroutine {
					return series.GetCounter().GetValue()
				}
			}
		}
	}

	return 0
}

// TestPruneImages_CallsDockerWithCorrectFilters verifies PruneImages forwards
// the prune filters (dangling=true, until=24h) to dockerd and surfaces
// the prune report as a nil error.
func TestPruneImages_CallsDockerWithCorrectFilters(t *testing.T) {
	var capturedFilter filters.Args

	mock := successfulMock()
	mock.ImagesPruneFn = func(_ context.Context, f filters.Args) (image.PruneReport, error) {
		capturedFilter = f

		return image.PruneReport{
			ImagesDeleted:  []image.DeleteResponse{{Deleted: "sha256:aaa"}},
			SpaceReclaimed: 42,
		}, nil
	}

	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	err := mgr.PruneImages(context.Background())
	require.NoError(t, err)

	// dangling=true / until=24h must be present. filters.Args.Get returns
	// the slice of values for the given key.
	danglingVals := capturedFilter.Get("dangling")
	assert.Contains(t, danglingVals, "true", "filter must include dangling=true")

	untilVals := capturedFilter.Get("until")
	assert.Contains(t, untilVals, "24h", "filter must include until=24h")
}

// TestPruneImages_PropagatesDockerError verifies that an ImagesPrune failure
// is wrapped and returned so the maintenance loop can log it.
func TestPruneImages_PropagatesDockerError(t *testing.T) {
	mock := successfulMock()
	mock.ImagesPruneFn = func(_ context.Context, _ filters.Args) (image.PruneReport, error) {
		return image.PruneReport{}, errors.New("dockerd gone")
	}

	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	err := mgr.PruneImages(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "images prune")
	assert.Contains(t, err.Error(), "dockerd gone")
}

// stubResolver is a test double for hostResolver. Calls increments a counter;
// sleep simulates a slow authoritative DNS server; addrs is the canned
// response used when the sleep (if any) completes in time.
type stubResolver struct {
	calls atomic.Int64
	sleep time.Duration
	addrs []string
	err   error
}

func (s *stubResolver) LookupHost(ctx context.Context, _ string) ([]string, error) {
	s.calls.Add(1)

	if s.sleep > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.sleep):
		}
	}

	if s.err != nil {
		return nil, s.err
	}

	return s.addrs, nil
}

// TestBuildExtraHosts_DNSTimeout asserts that a hostile / slow authoritative
// DNS server cannot stall the spawn path indefinitely. The stub sleeps well
// past the 2s cap; buildExtraHosts must return within a small envelope of the
// cap and must return only the default host-gateway entry (no MCP mapping).
func TestBuildExtraHosts_DNSTimeout(t *testing.T) {
	mock := successfulMock()
	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())
	mgr.resolver = &stubResolver{sleep: 5 * time.Second, addrs: []string{"10.0.0.1"}}

	start := time.Now()
	hosts := mgr.buildExtraHosts(context.Background(), "http://slow-dns.example:8080/mcp")
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 3*time.Second,
		"buildExtraHosts must honour the 2s cap; got %s", elapsed)
	assert.Equal(t, []string{"host.docker.internal:host-gateway"}, hosts,
		"on timeout buildExtraHosts must return only the default entry")
}

// TestBuildExtraHosts_DNSCache asserts that a second resolution for the same
// hostname is served from the cache without a second resolver call. This is
// the spawn-burst case: three containers starting in quick succession against
// the same MCP host should pay at most one DNS RTT between them.
func TestBuildExtraHosts_DNSCache(t *testing.T) {
	mock := successfulMock()
	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	stub := &stubResolver{addrs: []string{"192.0.2.17"}}
	mgr.resolver = stub

	// First call populates the cache.
	h1 := mgr.buildExtraHosts(context.Background(), "http://cache-me.example:8080/mcp")
	require.Contains(t, h1, "cache-me.example:192.0.2.17")
	require.Equal(t, int64(1), stub.calls.Load(), "first call must hit the resolver")

	// Second call must be served from the cache.
	h2 := mgr.buildExtraHosts(context.Background(), "http://cache-me.example:8080/mcp")
	require.Contains(t, h2, "cache-me.example:192.0.2.17")
	assert.Equal(t, int64(1), stub.calls.Load(),
		"second call with same hostname must be served from cache; got %d calls", stub.calls.Load())
}

// TestRun_RunningCallbackAsync asserts that startContainer + waitAndCleanup
// do not block on a slow running-status callback. The mock callback server
// sleeps 500ms before responding; the Run path (start + wait + reportTerminal)
// must still complete with the running-status callback still pending. The
// callback is wg-tracked so mgr.Wait() drains it before returning — measure
// only the inner Run path here, not Wait, since wg.Wait blocks on the
// detached goroutine by design.
func TestRun_RunningCallbackAsync(t *testing.T) {
	var (
		runningStartedAt atomic.Int64
		runningSeen      atomic.Bool
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			RunnerStatus string `json:"runner_status"`
		}

		_ = json.Unmarshal(body, &req)

		if req.RunnerStatus == "running" {
			runningStartedAt.Store(time.Now().UnixNano())
			runningSeen.Store(true)
			// Simulate a slow CM by sleeping before responding.
			time.Sleep(500 * time.Millisecond)
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)

	// Wait for the slow callback handler to have started before the inner
	// container path is done. That confirms the goroutine spawned ahead of
	// the spawn-path returning rather than waiting for the callback inline.
	require.Eventually(t, func() bool { return runningStartedAt.Load() != 0 },
		2*time.Second, 10*time.Millisecond,
		"running callback must enter the handler before Wait blocks")

	// mgr.Wait drains the wg-tracked callback goroutine, so it blocks for
	// the remainder of the 500ms sleep. That's expected and acceptable —
	// the assertion above already confirms the callback was non-blocking
	// for the spawn path itself.
	mgr.Wait()

	assert.True(t, runningSeen.Load(),
		"running callback must fire on its own goroutine")
}

// slowCloseWriteCloser is a WriteCloser whose Close() blocks for a fixed
// duration before returning. Used to simulate a hijacked stdin conn that takes
// a little while to close — proving the cleanup path still runs end-to-end
// without a hard wedge.
type slowCloseWriteCloser struct {
	delay time.Duration
}

func (s *slowCloseWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (s *slowCloseWriteCloser) Close() error {
	<-time.After(s.delay)

	return nil
}

// TestKill_InteractiveContainer_RemovesContainer verifies the end-to-end kill
// path for an interactive container: Manager.Kill cancels the context, which
// causes waitAndCleanup to take the ctx.Done() branch, which runs deferred
// cleanup including tracker.Remove and ContainerRemove(Force:true).
func TestKill_InteractiveContainer_RemovesContainer(t *testing.T) {
	var (
		removeCalledWith container.RemoveOptions
		removeCalled     atomic.Bool
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	// ContainerWait never resolves — the only exit is ctx.Done().
	mock.ContainerWaitFn = func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		return make(chan container.WaitResponse), make(chan error)
	}
	mock.ContainerRemoveFn = func(_ context.Context, _ string, opts container.RemoveOptions) error {
		removeCalledWith = opts

		removeCalled.Store(true)

		return nil
	}
	mock.ContainerAttachFn = func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
		return &HijackedResponse{Conn: nopWriteCloser{}}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	payload.CardID = "PROJ-911"
	payload.Interactive = true

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // satisfies gosec G118; Kill triggers cancel via tracker.Cancel

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  cancel,
	}))

	mgr.Run(ctx, payload)

	// Wait until the tracker entry has a container ID — confirms startContainer
	// has returned and waitAndCleanup's defers are installed.
	require.Eventually(t, func() bool {
		snap, ok := tr.Snapshot(payload.Project, payload.CardID)

		return ok && snap.ContainerID != ""
	}, 5*time.Second, 10*time.Millisecond, "tracker must have container ID before Kill")

	require.NoError(t, mgr.Kill(payload.Project, payload.CardID))
	mgr.Wait()

	// ContainerRemove must have been called with Force: true.
	assert.True(t, removeCalled.Load(), "ContainerRemove must be invoked")
	assert.True(t, removeCalledWith.Force, "ContainerRemove must use Force: true")

	// Tracker entry must be gone.
	assert.False(t, tr.Has(payload.Project, payload.CardID), "tracker entry must be removed after Kill")
}

// TestKill_InteractiveContainer_SlowStdinClose_StillRemoves is a companion to
// TestKill_InteractiveContainer_RemovesContainer. It uses a WriteCloser whose
// Close() blocks for 500ms — simulating a hijacked-conn that is slow but not
// wedged — and asserts that cleanup still completes: ContainerRemove(Force:true)
// is called and the tracker entry is gone.
func TestKill_InteractiveContainer_SlowStdinClose_StillRemoves(t *testing.T) {
	var (
		removeCalledWith container.RemoveOptions
		removeCalled     atomic.Bool
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	// ContainerWait never resolves — the only exit is ctx.Done().
	mock.ContainerWaitFn = func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		return make(chan container.WaitResponse), make(chan error)
	}
	mock.ContainerRemoveFn = func(_ context.Context, _ string, opts container.RemoveOptions) error {
		removeCalledWith = opts

		removeCalled.Store(true)

		return nil
	}
	// Attach returns a conn whose Close() blocks 500ms before returning.
	mock.ContainerAttachFn = func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
		return &HijackedResponse{Conn: &slowCloseWriteCloser{delay: 500 * time.Millisecond}}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	payload.CardID = "PROJ-912"
	payload.Interactive = true

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // satisfies gosec G118; Kill triggers cancel via tracker.Cancel

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  cancel,
	}))

	mgr.Run(ctx, payload)

	// Wait until startContainer has set the container ID in the tracker.
	require.Eventually(t, func() bool {
		snap, ok := tr.Snapshot(payload.Project, payload.CardID)

		return ok && snap.ContainerID != ""
	}, 5*time.Second, 10*time.Millisecond, "tracker must have container ID before Kill")

	require.NoError(t, mgr.Kill(payload.Project, payload.CardID))
	mgr.Wait()

	// Even with a 500ms stdin-close delay, removal must still happen.
	assert.True(t, removeCalled.Load(), "ContainerRemove must be invoked despite slow stdin close")
	assert.True(t, removeCalledWith.Force, "ContainerRemove must use Force: true")

	// Tracker entry must be gone.
	assert.False(t, tr.Has(payload.Project, payload.CardID), "tracker entry must be removed after Kill")
}

// TestDNSCache_PutGet exercises the cache primitives directly: a put/get
// pair must hit, and an expired entry must miss.
func TestDNSCache_PutGet(t *testing.T) {
	c := newDNSCache(50*time.Millisecond, 16)

	c.put("h.example", []string{"10.0.0.1"})

	got, ok := c.get("h.example")
	require.True(t, ok)
	assert.Equal(t, []string{"10.0.0.1"}, got)

	// Expire.
	time.Sleep(75 * time.Millisecond)

	_, ok = c.get("h.example")
	assert.False(t, ok, "expired entry must miss")
}

// TestDNSCache_Capacity evicts the oldest entry on overflow.
func TestDNSCache_Capacity(t *testing.T) {
	c := newDNSCache(time.Hour, 2)

	c.put("a", []string{"1"})
	c.put("b", []string{"2"})
	c.put("c", []string{"3"})

	assert.Equal(t, 2, c.len())

	_, ok := c.get("a")
	assert.False(t, ok, "oldest entry must be evicted")

	_, ok = c.get("b")
	assert.True(t, ok)

	_, ok = c.get("c")
	assert.True(t, ok)
}

// blockingReadCloser is an io.ReadCloser whose Read() blocks forever on a
// never-closed channel and whose Close() is a no-op. Used to simulate a
// container log stream that never unblocks (wedged docker daemon, stuck
// hijacked socket, or stdcopy/scanner stall).
type blockingReadCloser struct {
	block chan struct{}
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{block: make(chan struct{})}
}

func (b *blockingReadCloser) Read(_ []byte) (int, error) {
	<-b.block // blocks until the channel is closed; Close() never closes it

	return 0, io.EOF
}

func (b *blockingReadCloser) Close() error { return nil }

// TestWaitAndCleanup_LogDone_HangingReader_StillRemovesContainer verifies that
// the cancel path of waitAndCleanup proceeds through cleanup even when the
// log-streaming goroutine never unblocks. The root cause of the HITL container
// leak: a wedged ContainerLogs reader stalls <-logDone indefinitely, preventing
// the tracker.Remove and ContainerRemove defers from running.
func TestWaitAndCleanup_LogDone_HangingReader_StillRemovesContainer(t *testing.T) {
	// Shrink logDrainTimeout so the test runs in ~1s wall time instead of 5s.
	orig := logDrainTimeout
	logDrainTimeout = 50 * time.Millisecond

	t.Cleanup(func() { logDrainTimeout = orig })

	var (
		removeCalledWith container.RemoveOptions
		removeCalled     atomic.Bool
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()

	// ContainerLogs returns a reader that blocks forever in Read() and whose
	// Close() is a no-op — the log-streaming goroutine will never unblock.
	mock.ContainerLogsFn = func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
		return newBlockingReadCloser(), nil
	}

	// ContainerWait never resolves — the only exit from waitAndCleanup is
	// the <-ctx.Done() branch (triggered by Manager.Kill).
	mock.ContainerWaitFn = func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		return make(chan container.WaitResponse), make(chan error)
	}

	mock.ContainerRemoveFn = func(_ context.Context, _ string, opts container.RemoveOptions) error {
		removeCalledWith = opts

		removeCalled.Store(true)

		return nil
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	payload.CardID = "PROJ-426"

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // gosec G118; Kill triggers cancel via tracker.Cancel

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  cancel,
	}))

	mgr.Run(ctx, payload)

	// Wait until startContainer has registered the container ID in the tracker
	// so we know waitAndCleanup's defers are installed.
	require.Eventually(t, func() bool {
		snap, ok := tr.Snapshot(payload.Project, payload.CardID)

		return ok && snap.ContainerID != ""
	}, 5*time.Second, 10*time.Millisecond, "tracker must have container ID before Kill")

	require.NoError(t, mgr.Kill(payload.Project, payload.CardID))

	// Allow logDrainTimeout + dockerCleanupTimeout + 2s slack for the full
	// cleanup sequence to complete despite the hung log reader.
	deadline := logDrainTimeout + dockerCleanupTimeout + 2*time.Second

	mgr.Wait()

	// ContainerRemove must have been called with Force: true even though the
	// log reader never unblocked.
	require.Eventually(t, removeCalled.Load, deadline, 10*time.Millisecond, "ContainerRemove must be invoked despite hung log reader")

	assert.True(t, removeCalledWith.Force, "ContainerRemove must use Force: true")

	// Tracker entry must be gone.
	assert.False(t, tr.Has(payload.Project, payload.CardID), "tracker entry must be removed after Kill")
}

// TestListManaged_ReportsTrackerDivergence is the core guarantee CM's
// Docker-authoritative sweep relies on: for any container labeled as
// runner-managed, ListManaged returns a row whose Tracked flag reflects the
// tracker state at response time. A running container absent from the tracker
// (Tracked=false, State="running") is the signature of the divergence bug the
// sweep is designed to catch.
func TestListManaged_ReportsTrackerDivergence(t *testing.T) {
	mock := successfulMock()
	mock.ContainerListFn = func(_ context.Context, opts container.ListOptions) ([]DockerContainer, error) {
		// The handler is expected to pass the LabelRunner=true filter and All=true.
		assert.True(t, opts.All, "ListManaged must list all containers, including exited ones")
		assert.True(t, opts.Filters.Match("label", LabelRunner+"=true"),
			"ListManaged must filter on the runner-managed label")

		return []DockerContainer{
			{
				ID:      "tracked-abc",
				Names:   []string{"/cmr-proj-a-001"},
				State:   "running",
				Created: time.Now().Add(-10 * time.Minute).Unix(),
				Labels: map[string]string{
					LabelRunner:  "true",
					LabelProject: "proj",
					LabelCardID:  "A-001",
				},
			},
			{
				ID:      "orphan-def",
				Names:   []string{"/cmr-proj-a-002"},
				State:   "running",
				Created: time.Now().Add(-30 * time.Minute).Unix(),
				Labels: map[string]string{
					LabelRunner:  "true",
					LabelProject: "proj",
					LabelCardID:  "A-002",
				},
			},
			{
				// Missing card_id label — must be skipped.
				ID:      "mislabeled-ghi",
				Labels:  map[string]string{LabelRunner: "true", LabelProject: "proj"},
				State:   "running",
				Created: time.Now().Unix(),
			},
		}, nil
	}

	tr := tracker.New()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		Project: "proj", CardID: "A-001", ContainerID: "tracked-abc",
	}))

	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	got, err := mgr.ListManaged(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2, "mislabeled container must be skipped; the other two must be listed")

	byCard := make(map[string]ManagedContainer, len(got))
	for _, c := range got {
		byCard[c.CardID] = c
	}

	require.Contains(t, byCard, "A-001")
	assert.True(t, byCard["A-001"].Tracked, "tracked container must report Tracked=true")
	assert.Equal(t, "cmr-proj-a-001", byCard["A-001"].ContainerName, "Docker's leading / on Names must be stripped")

	require.Contains(t, byCard, "A-002")
	assert.False(t, byCard["A-002"].Tracked, "untracked container (the divergence case) must report Tracked=false")
	assert.Equal(t, "running", byCard["A-002"].State)
}

// TestListManaged_DockerError surfaces the underlying Docker error so the
// webhook handler can translate it into a 502, telling CM "I couldn't ask
// Docker" instead of "I have nothing to report".
func TestListManaged_DockerError(t *testing.T) {
	mock := successfulMock()
	mock.ContainerListFn = func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
		return nil, fmt.Errorf("docker unreachable")
	}

	mgr := NewManager(mock, tracker.New(), nil, testPATProvider(t), nil, testConfig(t), testLogger())

	_, err := mgr.ListManaged(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list managed containers")
}

// TestForceRemoveByLabels_RemovesEveryMatch is the guarantee the /kill
// fallback relies on: when the tracker has no entry but Docker still holds a
// labeled container, ForceRemoveByLabels force-removes it without going
// through the tracker-driven cancel flow. Without this, the container would
// leak to the runner's 2h container_timeout — the exact fail mode the
// Docker-authoritative kill path is closing.
func TestForceRemoveByLabels_RemovesEveryMatch(t *testing.T) {
	var removed []string

	mock := successfulMock()
	mock.ContainerListFn = func(_ context.Context, opts container.ListOptions) ([]DockerContainer, error) {
		assert.True(t, opts.All, "force-remove-by-labels must consider non-running containers too")
		// Verify all three label filters are applied so we never scoop up
		// a container belonging to a different project / card.
		assert.True(t, opts.Filters.Match("label", LabelRunner+"=true"))
		assert.True(t, opts.Filters.Match("label", LabelProject+"=proj"))
		assert.True(t, opts.Filters.Match("label", LabelCardID+"=A-001"))

		return []DockerContainer{
			{ID: "abc-123", Labels: map[string]string{LabelCardID: "A-001", LabelProject: "proj"}},
		}, nil
	}
	mock.ContainerRemoveFn = func(_ context.Context, id string, opts container.RemoveOptions) error {
		assert.True(t, opts.Force, "force-remove must use Force: true")

		removed = append(removed, id)

		return nil
	}

	mgr := NewManager(mock, tracker.New(), nil, testPATProvider(t), nil, testConfig(t), testLogger())

	n, err := mgr.ForceRemoveByLabels(context.Background(), "proj", "A-001")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"abc-123"}, removed)
}

// TestForceRemoveByLabels_NoMatchReturnsZero is the idempotent path consumed
// by /kill: when neither tracker nor Docker knows the card, the handler
// returns 200 no-op.
func TestForceRemoveByLabels_NoMatchReturnsZero(t *testing.T) {
	mock := successfulMock()
	mock.ContainerListFn = func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
		return nil, nil
	}

	mgr := NewManager(mock, tracker.New(), nil, testPATProvider(t), nil, testConfig(t), testLogger())

	n, err := mgr.ForceRemoveByLabels(context.Background(), "proj", "A-001")
	require.NoError(t, err)
	assert.Zero(t, n)
}

// TestForceRemoveByLabels_RequiresProjectAndCard guards against an empty
// call that would otherwise list every runner-managed container and remove
// them all — which would happen if the label filter were silently ignored
// by Docker when the value is empty.
func TestForceRemoveByLabels_RequiresProjectAndCard(t *testing.T) {
	mgr := NewManager(nil, tracker.New(), nil, testPATProvider(t), nil, testConfig(t), testLogger())

	_, err := mgr.ForceRemoveByLabels(context.Background(), "", "A-001")
	require.Error(t, err)

	_, err = mgr.ForceRemoveByLabels(context.Background(), "proj", "")
	require.Error(t, err)
}

func TestContainerCreate_TaskSkillsMount(t *testing.T) {
	// helper builds a manager with the given config, runs a single container
	// creation, and returns the captured mounts and env from ContainerCreate.
	captureCreateArgs := func(t *testing.T, cfg *config.Config, payload RunConfig) ([]mount.Mount, []string) {
		t.Helper()

		var (
			capturedMounts []mount.Mount
			capturedEnv    []string
		)

		mock := successfulMock()
		mock.ContainerCreateFn = func(_ context.Context, ccfg *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			capturedMounts = hc.Mounts
			capturedEnv = ccfg.Env

			return container.CreateResponse{ID: "skills-test-ctr"}, nil
		}

		cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(cbSrv.Close)

		tr := tracker.New()
		cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
		tp := testPATProvider(t)

		mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

		require.NoError(t, tr.Add(&tracker.ContainerInfo{
			CardID:  payload.CardID,
			Project: payload.Project,
		}))

		mgr.Run(context.Background(), payload)
		mgr.Wait()

		return capturedMounts, capturedEnv
	}

	t.Run("mount added when task_skills_dir configured", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.TaskSkillsDir = "/var/lib/cm/task-skills"

		payload := testPayload()
		// TaskSkills nil — only mount should be added, no env vars.

		mounts, env := captureCreateArgs(t, cfg, payload)

		var found bool

		for _, m := range mounts {
			if m.Target == "/host-skills" {
				found = true

				assert.Equal(t, "/var/lib/cm/task-skills", m.Source)
				assert.Equal(t, mount.TypeBind, m.Type)
				assert.True(t, m.ReadOnly)
			}
		}

		assert.True(t, found, "expected /host-skills mount to be present")

		for _, e := range env {
			assert.False(t, strings.HasPrefix(e, "CM_TASK_SKILLS_SET="), "CM_TASK_SKILLS_SET must not be set when TaskSkills is nil")
		}
	})

	t.Run("no mount when task_skills_dir empty", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.TaskSkillsDir = ""

		payload := testPayload()
		skills := []string{"go-development"}
		payload.TaskSkills = &skills

		mounts, env := captureCreateArgs(t, cfg, payload)

		for _, m := range mounts {
			assert.NotEqual(t, "/host-skills", m.Target, "no /host-skills mount expected when TaskSkillsDir is empty")
		}

		for _, e := range env {
			assert.False(t, strings.HasPrefix(e, "CM_TASK_SKILLS_SET="), "CM_TASK_SKILLS_SET must not be set when TaskSkillsDir is empty")
		}
	})

	t.Run("env vars emitted for non-nil TaskSkills", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.TaskSkillsDir = "/x"

		payload := testPayload()
		skills := []string{"go-development", "docs"}
		payload.TaskSkills = &skills

		_, env := captureCreateArgs(t, cfg, payload)

		assert.Contains(t, env, "CM_TASK_SKILLS_SET=1")
		assert.Contains(t, env, "CM_TASK_SKILLS=go-development,docs")
	})

	t.Run("empty list still emits SET=1 with empty value", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.TaskSkillsDir = "/x"

		payload := testPayload()
		skills := []string{}
		payload.TaskSkills = &skills

		_, env := captureCreateArgs(t, cfg, payload)

		assert.Contains(t, env, "CM_TASK_SKILLS_SET=1")
		assert.Contains(t, env, "CM_TASK_SKILLS=")
	})

	t.Run("nil TaskSkills emits no env vars even with mount", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.TaskSkillsDir = "/x"

		payload := testPayload()
		// payload.TaskSkills is nil by default.

		_, env := captureCreateArgs(t, cfg, payload)

		for _, e := range env {
			assert.False(t, strings.HasPrefix(e, "CM_TASK_SKILLS_SET="), "CM_TASK_SKILLS_SET must not be set when TaskSkills is nil")
			assert.False(t, strings.HasPrefix(e, "CM_TASK_SKILLS="), "CM_TASK_SKILLS must not be set when TaskSkills is nil")
		}
	})
}

// TestContainerCreate_WorkerExtraEnv covers the deployment-wide
// extra-env injection. Values land in the container env list verbatim;
// nil/empty maps are no-ops.
func TestContainerCreate_WorkerExtraEnv(t *testing.T) {
	captureEnv := func(t *testing.T, cfg *config.Config) []string {
		t.Helper()

		var capturedEnv []string

		mock := successfulMock()
		mock.ContainerCreateFn = func(_ context.Context, ccfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			capturedEnv = ccfg.Env

			return container.CreateResponse{ID: "extraenv-test-ctr"}, nil
		}

		cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(cbSrv.Close)

		tr := tracker.New()
		cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
		tp := testPATProvider(t)
		mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

		payload := testPayload()
		require.NoError(t, tr.Add(&tracker.ContainerInfo{
			CardID:  payload.CardID,
			Project: payload.Project,
		}))

		mgr.Run(context.Background(), payload)
		mgr.Wait()

		return capturedEnv
	}

	t.Run("nil map adds nothing", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.WorkerExtraEnv = nil

		env := captureEnv(t, cfg)
		for _, e := range env {
			assert.False(t, strings.HasPrefix(e, "GIT_SSL_NO_VERIFY="),
				"no extra env should be present when WorkerExtraEnv is nil")
		}
	})

	t.Run("entries appear in env list", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.WorkerExtraEnv = map[string]string{
			"GIT_SSL_NO_VERIFY":   "1",
			"NPM_CONFIG_REGISTRY": "https://npm.internal/",
		}

		env := captureEnv(t, cfg)
		assert.Contains(t, env, "GIT_SSL_NO_VERIFY=1")
		assert.Contains(t, env, "NPM_CONFIG_REGISTRY=https://npm.internal/")
	})

	t.Run("entries sorted for deterministic output", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.WorkerExtraEnv = map[string]string{
			"ZEBRA": "z",
			"ALPHA": "a",
			"MIKE":  "m",
		}

		env := captureEnv(t, cfg)

		var picked []string

		for _, e := range env {
			switch {
			case strings.HasPrefix(e, "ALPHA="),
				strings.HasPrefix(e, "MIKE="),
				strings.HasPrefix(e, "ZEBRA="):
				picked = append(picked, e)
			}
		}

		assert.Equal(t, []string{"ALPHA=a", "MIKE=m", "ZEBRA=z"}, picked)
	})
}

func TestContainerCreate_TaskSkillsPull(t *testing.T) {
	// runWithPullStub builds a manager, swaps pullSkillsRepo with stub, runs
	// one container creation, and returns whatever stub recorded.
	runWithPullStub := func(t *testing.T, cfg *config.Config, payload RunConfig, stub func(context.Context, string, string) error) (calls []string, tokens []string) {
		t.Helper()

		orig := pullSkillsRepo

		t.Cleanup(func() { pullSkillsRepo = orig })

		pullSkillsRepo = func(ctx context.Context, dir, token string) error {
			calls = append(calls, dir)
			tokens = append(tokens, token)

			return stub(ctx, dir, token)
		}

		mock := successfulMock()

		cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(cbSrv.Close)

		tr := tracker.New()
		cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
		tp := testPATProvider(t)

		mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

		require.NoError(t, tr.Add(&tracker.ContainerInfo{
			CardID:  payload.CardID,
			Project: payload.Project,
		}))

		mgr.Run(context.Background(), payload)
		mgr.Wait()

		return calls, tokens
	}

	t.Run("pull invoked when task_skills_dir configured", func(t *testing.T) {
		dir := t.TempDir()
		// Create a fake .git directory so pullSkillsRepo is not short-circuited
		// by the real implementation (tests stub it, but the dir check is in
		// the stub for this test to exercise the wiring path).
		cfg := testConfig(t)
		cfg.TaskSkillsDir = dir

		calls, tokens := runWithPullStub(t, cfg, testPayload(), func(_ context.Context, _, _ string) error {
			return nil
		})

		require.Len(t, calls, 1, "pull must be called exactly once")
		assert.Equal(t, dir, calls[0])
		require.Len(t, tokens, 1)
		assert.NotEmpty(t, tokens[0], "pullSkillsRepo must receive a non-empty git token")
	})

	t.Run("pull failure does not abort container creation", func(t *testing.T) {
		dir := t.TempDir()
		cfg := testConfig(t)
		cfg.TaskSkillsDir = dir

		// Stub returns an error; container creation must still complete.
		var containerCreated bool

		orig := pullSkillsRepo

		t.Cleanup(func() { pullSkillsRepo = orig })

		pullSkillsRepo = func(_ context.Context, _, _ string) error {
			return fmt.Errorf("git pull: exit status 1")
		}

		mock := successfulMock()
		mock.ContainerCreateFn = func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			containerCreated = true

			return container.CreateResponse{ID: "pull-fail-ctr"}, nil
		}

		cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(cbSrv.Close)

		tr := tracker.New()
		cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
		tp := testPATProvider(t)

		payload := testPayload()
		mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

		require.NoError(t, tr.Add(&tracker.ContainerInfo{
			CardID:  payload.CardID,
			Project: payload.Project,
		}))

		mgr.Run(context.Background(), payload)
		mgr.Wait()

		assert.True(t, containerCreated, "container must still be created even when pull fails")
	})

	t.Run("no pull when task_skills_dir empty", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.TaskSkillsDir = ""

		calls, _ := runWithPullStub(t, cfg, testPayload(), func(_ context.Context, _, _ string) error {
			return nil
		})

		assert.Empty(t, calls, "pull must not be called when TaskSkillsDir is empty")
	})

	t.Run("no pull when dir is not a git repo", func(t *testing.T) {
		// Use the real pullSkillsRepo (not a stub) against a real dir that has
		// no .git entry — the implementation must return nil silently.
		dir := t.TempDir() // exists, but no .git inside

		err := pullSkillsRepo(context.Background(), dir, "any-token")
		assert.NoError(t, err, "pullSkillsRepo must return nil when dir is not a git repo")
	})

	t.Run("concurrent pulls are serialised by skillsPullMu", func(t *testing.T) {
		// Two concurrent taskSkillsMount calls against the same Manager
		// (same TaskSkillsDir) must not overlap inside pullSkillsRepo,
		// because `git pull` writes .git/index.lock and cannot share a
		// working tree. The stub records overlap by tracking the active
		// in-flight count and asserts it never exceeds 1.
		dir := t.TempDir()
		cfg := testConfig(t)
		cfg.TaskSkillsDir = dir

		orig := pullSkillsRepo

		t.Cleanup(func() { pullSkillsRepo = orig })

		var (
			active     atomic.Int32
			maxActive  atomic.Int32
			callsTotal atomic.Int32
		)

		pullSkillsRepo = func(_ context.Context, _, _ string) error {
			n := active.Add(1)

			for {
				cur := maxActive.Load()
				if n <= cur || maxActive.CompareAndSwap(cur, n) {
					break
				}
			}
			// Hold the slot briefly so a concurrent caller has a real
			// chance to overlap if the mutex is missing.
			time.Sleep(20 * time.Millisecond)
			active.Add(-1)
			callsTotal.Add(1)

			return nil
		}

		mgr := NewManager(successfulMock(), tracker.New(), nil, testPATProvider(t), nil, cfg, testLogger())

		const fanout = 8

		var wg sync.WaitGroup

		for range fanout {
			wg.Go(func() {
				_, _ = mgr.taskSkillsMount(context.Background(), "tok")
			})
		}

		wg.Wait()

		assert.EqualValues(t, fanout, callsTotal.Load(),
			"every caller must complete its pull")
		assert.LessOrEqual(t, maxActive.Load(), int32(1),
			"skillsPullMu must keep concurrent pulls strictly serial (max overlap = 1)")
	})
}

// TestPullSkillsEnv verifies that pullSkillsEnv injects an Authorization
// header via env-based git config when the upstream is HTTPS, and falls
// back to the parent env unchanged otherwise. The token must never appear
// in argv-visible places — only in the GIT_CONFIG_VALUE_0 env var.
func TestPullSkillsEnv(t *testing.T) {
	gitInit := func(t *testing.T, remote string) string {
		t.Helper()

		dir := t.TempDir()
		mustGit := func(args ...string) {
			//nolint:gosec // args are test-controlled literals
			cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "git %v: %s", args, out)
		}
		mustGit("init", "--quiet")

		if remote != "" {
			mustGit("remote", "add", "origin", remote)
		}

		return dir
	}

	t.Run("https remote injects extraheader", func(t *testing.T) {
		dir := gitInit(t, "https://github.com/org/skills.git")

		env := pullSkillsEnv(context.Background(), dir, "ghs_secret_token_value")

		assert.Contains(t, env, "GIT_CONFIG_COUNT=1")
		assert.Contains(t, env, "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader")
		// "x-access-token:ghs_secret_token_value" base64 == "eC1hY2Nlc3MtdG9rZW46Z2hzX3NlY3JldF90b2tlbl92YWx1ZQ=="
		assert.Contains(t, env,
			"GIT_CONFIG_VALUE_0=Authorization: Basic eC1hY2Nlc3MtdG9rZW46Z2hzX3NlY3JldF90b2tlbl92YWx1ZQ==",
		)
	})

	t.Run("https remote with port scopes header to host:port", func(t *testing.T) {
		dir := gitInit(t, "https://gh.example.com:8443/org/skills.git")

		env := pullSkillsEnv(context.Background(), dir, "tok")

		assert.Contains(t, env, "GIT_CONFIG_KEY_0=http.https://gh.example.com:8443/.extraheader")
	})

	t.Run("no remote returns parent env unchanged", func(t *testing.T) {
		dir := gitInit(t, "")

		env := pullSkillsEnv(context.Background(), dir, "tok")

		for _, e := range env {
			assert.False(t, strings.HasPrefix(e, "GIT_CONFIG_"),
				"no remote should not inject GIT_CONFIG_* (got %q)", e)
		}
	})

	t.Run("non-https remote returns parent env unchanged", func(t *testing.T) {
		// e.g. a local file:// upstream used in dev — pull works without
		// auth injection, and we must not pretend HTTPS scoping applies.
		upstream := t.TempDir()
		dir := gitInit(t, "file://"+upstream)

		env := pullSkillsEnv(context.Background(), dir, "tok")

		for _, e := range env {
			assert.False(t, strings.HasPrefix(e, "GIT_CONFIG_"),
				"non-https remote should not inject GIT_CONFIG_* (got %q)", e)
		}
	})
}

// TestRun_KnowledgeRefreshEnvInjection verifies that ModeKnowledgeRefresh
// injects the four refresh-specific env vars and does not duplicate the
// already-injected CM_PROJECT or CM_REPO_URL.
func TestRun_KnowledgeRefreshEnvInjection(t *testing.T) {
	var createdEnv []string

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		createdEnv = cfg.Env

		return container.CreateResponse{ID: "kb-refresh-ctr"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := RunConfig{
		Mode:          ModeKnowledgeRefresh,
		CardID:        "kb-refresh:my-repo",
		Project:       "my-project",
		KBRepo:        "my-repo",
		RepoURL:       "https://github.com/org/my-repo.git",
		AgentID:       "human:test",
		OverwriteDocs: []string{"api-documentation.md"},
		MCPURL:        "http://cm:8080/mcp",
	}
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	assert.Contains(t, createdEnv, "CM_MODE=knowledge-refresh")
	assert.Contains(t, createdEnv, "CM_KB_REPO=my-repo")
	assert.Contains(t, createdEnv, "CM_AGENT_ID=human:test")
	assert.Contains(t, createdEnv, "CM_KB_OVERWRITE_DOCS=api-documentation.md")

	// CM_PROJECT and CM_REPO_URL are set by the base env block — verify they
	// are present and not duplicated.
	assert.Contains(t, createdEnv, "CM_PROJECT=my-project")
	assert.Contains(t, createdEnv, "CM_REPO_URL=https://github.com/org/my-repo.git")

	var modeCount int

	for _, e := range createdEnv {
		if e == "CM_MODE=knowledge-refresh" {
			modeCount++
		}
	}

	assert.Equal(t, 1, modeCount, "CM_MODE must appear exactly once")
}

// TestRun_KnowledgeRefreshMode_OnSuccess_CallsKnowledgeStatus verifies that
// when Mode==ModeKnowledgeRefresh and the container exits with code 0, the
// manager posts to /api/runner/knowledge-status with state="succeeded" and
// does NOT post to /api/runner/status.
func TestRun_KnowledgeRefreshMode_OnSuccess_CallsKnowledgeStatus(t *testing.T) {
	var (
		mu             sync.Mutex
		knowledgeCalls []string // captured "state" values
		statusCalled   bool
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		switch r.URL.Path {
		case "/api/runner/knowledge-status":
			var req struct {
				State string `json:"state"`
				Repo  string `json:"repo"`
			}

			_ = json.Unmarshal(body, &req)

			mu.Lock()

			knowledgeCalls = append(knowledgeCalls, req.State)
			mu.Unlock()

		case "/api/runner/status":
			var req struct {
				RunnerStatus string `json:"runner_status"`
			}

			_ = json.Unmarshal(body, &req)

			// Neither "running" nor "completed" should be called in refresh mode.
			mu.Lock()
			statusCalled = true
			mu.Unlock()

			_ = req
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := RunConfig{
		Mode:    ModeKnowledgeRefresh,
		CardID:  "kb-refresh:my-repo",
		Project: "my-project",
		KBRepo:  "my-repo",
		RepoURL: "https://github.com/org/my-repo.git",
		AgentID: "human:test",
		MCPURL:  "http://cm:8080/mcp",
	}
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	mu.Lock()
	defer mu.Unlock()

	assert.Contains(t, knowledgeCalls, "succeeded", "knowledge-status must be called with succeeded on exit 0")
	assert.False(t, statusCalled, "reportCompleted must not call ReportStatus in knowledge-refresh mode")
}

// TestRun_KnowledgeRefreshMode_OnFailure_CallsKnowledgeStatusWithFailed
// verifies that when Mode==ModeKnowledgeRefresh and the container exits with a
// non-zero code, the manager posts to /api/runner/knowledge-status with
// state="failed".
func TestRun_KnowledgeRefreshMode_OnFailure_CallsKnowledgeStatusWithFailed(t *testing.T) {
	var (
		mu             sync.Mutex
		knowledgeCalls []string
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		if r.URL.Path == "/api/runner/knowledge-status" {
			var req struct {
				State string `json:"state"`
			}

			_ = json.Unmarshal(body, &req)

			mu.Lock()

			knowledgeCalls = append(knowledgeCalls, req.State)
			mu.Unlock()
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	mock.ContainerWaitFn = func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		ch := make(chan container.WaitResponse, 1)
		ch <- container.WaitResponse{StatusCode: 1}

		return ch, make(chan error)
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := RunConfig{
		Mode:    ModeKnowledgeRefresh,
		CardID:  "kb-refresh:my-repo",
		Project: "my-project",
		KBRepo:  "my-repo",
		RepoURL: "https://github.com/org/my-repo.git",
		AgentID: "human:test",
		MCPURL:  "http://cm:8080/mcp",
	}
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	mu.Lock()
	defer mu.Unlock()

	assert.Contains(t, knowledgeCalls, "failed", "knowledge-status must be called with failed on non-zero exit")
}

// TestRun_KnowledgeRefreshMode_DoesNotCallReportStatusRunning verifies that
// runningCallbackAsync is skipped entirely for refresh-mode containers so the
// runner never POSTs a "running" runner_status to CM (which would 404 because
// "kb-refresh:<repo>" is not a valid card ID).
func TestRun_KnowledgeRefreshMode_DoesNotCallReportStatusRunning(t *testing.T) {
	var (
		mu               sync.Mutex
		statusCallBodies []string
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		if r.URL.Path == "/api/runner/status" {
			mu.Lock()

			statusCallBodies = append(statusCallBodies, string(body))
			mu.Unlock()
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()
	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := RunConfig{
		Mode:    ModeKnowledgeRefresh,
		CardID:  "kb-refresh:my-repo",
		Project: "my-project",
		KBRepo:  "my-repo",
		RepoURL: "https://github.com/org/my-repo.git",
		AgentID: "human:test",
		MCPURL:  "http://cm:8080/mcp",
	}
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	mu.Lock()
	defer mu.Unlock()

	// No call to /api/runner/status must have been made — neither "running"
	// nor any other runner_status. KnowledgeStatus is the only terminal
	// callback for refresh mode.
	assert.Empty(t, statusCallBodies, "ReportStatus must not be called for refresh-mode containers")
}

// TestRun_KnowledgeRefreshMode_DoesNotCallReportSkillEngaged verifies that
// onSkillEngaged is skipped entirely for refresh-mode containers so the runner
// never POSTs to /api/runner/skill-engaged with a synthetic
// "kb-refresh:<repo>" CardID (which CM rejects as 4xx, producing retry log
// spam). Mirrors the running-status guard added in efa9412.
func TestRun_KnowledgeRefreshMode_DoesNotCallReportSkillEngaged(t *testing.T) {
	var (
		mu                     sync.Mutex
		skillEngagedCallBodies []string
	)

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		if r.URL.Path == "/api/runner/skill-engaged" {
			mu.Lock()

			skillEngagedCallBodies = append(skillEngagedCallBodies, string(body))
			mu.Unlock()
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	mock := successfulMock()

	// Inject a Skill tool_use stream-json line into stdout so the logparser
	// fires the onSkillEngaged hook. Docker logs are multiplexed, so we frame
	// the bytes via stdcopy.NewStdWriter — otherwise stdcopy.StdCopy in
	// streamLogs would drop them as unframed garbage.
	mock.ContainerLogsFn = func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
		var buf strings.Builder

		w := stdcopy.NewStdWriter(&buf, stdcopy.Stdout)

		_, _ = w.Write([]byte(
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"abc","name":"Skill","input":{"skill":"refresh-knowledge"}}]}}` + "\n",
		))

		return io.NopCloser(strings.NewReader(buf.String())), nil
	}

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := RunConfig{
		Mode:    ModeKnowledgeRefresh,
		CardID:  "kb-refresh:my-repo",
		Project: "my-project",
		KBRepo:  "my-repo",
		RepoURL: "https://github.com/org/my-repo.git",
		AgentID: "human:test",
		MCPURL:  "http://cm:8080/mcp",
	}
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	// Drain any callback goroutines spawned by onSkillEngaged before the
	// guard fires. The pre-fix closure dispatches via `go func()` so we need
	// a small grace window for the POST to land at cbSrv before we read the
	// counter.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	assert.Empty(t, skillEngagedCallBodies,
		"ReportSkillEngaged must not fire for knowledge-refresh containers")
}

// TestManager_StartChat_SetsEnvAndWorkspace verifies that StartChat injects the
// correct CM_CHAT_* env vars, merges non-secret values, sets OpenStdin, and
// does NOT inject card-mode vars (CM_CARD_ID, CM_INTERACTIVE). Secrets are
// covered by TestManager_StartChat_SecretsViaTmpfs.
func TestManager_StartChat_SetsEnvAndWorkspace(t *testing.T) {
	var capturedCfg *container.Config

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedCfg = cfg

		return container.CreateResponse{ID: "chat-ctr-abc123"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	containerID, err := mgr.StartChat(context.Background(), StartChatOpts{
		SessionID: "S1",
		Project:   "alpha",
		RepoURL:   "https://example.com/alpha.git",
		MCPURL:    "http://host.docker.internal:8080/mcp",
	})
	require.NoError(t, err)
	assert.Equal(t, "chat-ctr-abc123", containerID)
	require.NotNil(t, capturedCfg)

	env := capturedCfg.Env

	// Required chat env vars must be present.
	assert.Contains(t, env, "CM_CHAT_SESSION=S1")
	assert.Contains(t, env, "CM_CHAT_PROJECT=alpha")
	assert.Contains(t, env, "CM_CHAT_REPO_URL=https://example.com/alpha.git")
	assert.Contains(t, env, "CM_MCP_URL=http://host.docker.internal:8080/mcp",
		"CM_MCP_URL must be set in chat mode so claude can reach CM's MCP endpoint")

	// Card-mode vars must NOT be present.
	for _, e := range env {
		assert.False(t, strings.HasPrefix(e, "CM_CARD_ID="), "CM_CARD_ID must not be set in chat mode")
		assert.False(t, strings.HasPrefix(e, "CM_INTERACTIVE="), "CM_INTERACTIVE must not be set in chat mode")
	}

	// Container must be configured for stdin streaming.
	assert.True(t, capturedCfg.OpenStdin, "OpenStdin must be true for chat containers")
	assert.True(t, capturedCfg.AttachStdin, "AttachStdin must be true for chat containers")
	assert.False(t, capturedCfg.Tty, "Tty must be false for chat containers")

	// Label must carry the session ID.
	assert.Equal(t, "S1", capturedCfg.Labels[LabelSessionID])
	assert.Equal(t, "true", capturedCfg.Labels[LabelRunner])
	assert.Equal(t, "alpha", capturedCfg.Labels[LabelProject])
}

// TestManager_StartChat_SecretDelivery verifies that chat-mode secrets are
// delivered correctly: CM_MCP_API_KEY rides in Env (per-card), while rotating
// secrets (CM_GIT_TOKEN, Claude auth) ride the shared dir mount and must not
// appear in container.Config.Env.
func TestManager_StartChat_SecretDelivery(t *testing.T) {
	var (
		capturedCfg     *container.Config
		capturedHostCfg *container.HostConfig
	)

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, hostCfg *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedCfg = cfg
		capturedHostCfg = hostCfg

		return container.CreateResponse{ID: "chat-ctr-secrets"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	cfg := testConfig(t)
	cfg.DeploymentProfile = config.ProfileProduction

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	_, err := mgr.StartChat(context.Background(), StartChatOpts{
		SessionID: "S-secrets",
		Project:   "alpha",
		RepoURL:   "https://example.com/alpha.git",
		MCPAPIKey: "mcp-secret-key",
		GitToken:  "gh-token-secret",
	})
	require.NoError(t, err)
	require.NotNil(t, capturedCfg)
	require.NotNil(t, capturedHostCfg)

	// CM_MCP_API_KEY is per-card and must appear in Env.
	assert.Contains(t, capturedCfg.Env, "CM_MCP_API_KEY=mcp-secret-key",
		"CM_MCP_API_KEY must be in container Env for per-card delivery")

	// Rotating secrets must NOT appear in container.Config.Env.
	for _, e := range capturedCfg.Env {
		assert.False(t, strings.HasPrefix(e, "CM_GIT_TOKEN="),
			"CM_GIT_TOKEN must not be in Env; it rides the shared dir mount")
		assert.False(t, strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN="),
			"CLAUDE_CODE_OAUTH_TOKEN must not be in Env; it rides the shared dir mount")
		assert.False(t, strings.HasPrefix(e, "ANTHROPIC_API_KEY="),
			"ANTHROPIC_API_KEY must not be in Env; it rides the shared dir mount")
	}

	// Mounts must include the shared dir at the canonical target.
	var hasSecretsMount bool

	for _, m := range capturedHostCfg.Mounts {
		if m.Target == secretsMountTarget {
			hasSecretsMount = true

			assert.True(t, m.ReadOnly, "secrets mount must be read-only")
			assert.Equal(t, mount.TypeBind, m.Type, "secrets mount must be a bind")
			assert.Equal(t, filepath.Join(cfg.SecretsDir, "shared"), m.Source,
				"secrets mount source must be the shared dir")
		}
	}

	assert.True(t, hasSecretsMount, "chat container HostConfig must bind-mount the shared secrets dir at %s", secretsMountTarget)
}

// TestManager_StartChat_RequiresSessionID verifies that an empty SessionID
// is rejected before any Docker call is attempted.
func TestManager_StartChat_RequiresSessionID(t *testing.T) {
	mock := &MockDockerClient{} // No Fns set — any call will panic.

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	_, err := mgr.StartChat(context.Background(), StartChatOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SessionID is required")
}

// TestManager_StartChat_UsesBaseImage verifies that chat containers spawn from
// cfg.BaseImage. There is intentionally no per-spawn image override on the
// chat path — the runner's allowlist policy lives behind cfg.BaseImage / the
// startContainer allowlist for card-mode and chat-mode shares it implicitly.
func TestManager_StartChat_UsesBaseImage(t *testing.T) {
	var usedImage string

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		usedImage = cfg.Image

		return container.CreateResponse{ID: "chat-fallback-ctr"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	cfg := testConfig(t) // BaseImage is "test-image:latest"
	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	_, err := mgr.StartChat(context.Background(), StartChatOpts{
		SessionID: "S2",
	})
	require.NoError(t, err)
	assert.Equal(t, "test-image:latest", usedImage, "chat containers must spawn from cfg.BaseImage")
}

// TestManager_StartChat_OptionalFieldsOmitted verifies that when Project and
// RepoURL are empty, their corresponding env vars are absent.
func TestManager_StartChat_OptionalFieldsOmitted(t *testing.T) {
	var capturedEnv []string

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedEnv = cfg.Env

		return container.CreateResponse{ID: "chat-minimal-ctr"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	_, err := mgr.StartChat(context.Background(), StartChatOpts{
		SessionID: "S3",
	})
	require.NoError(t, err)
	assert.Contains(t, capturedEnv, "CM_CHAT_SESSION=S3")

	for _, e := range capturedEnv {
		assert.False(t, strings.HasPrefix(e, "CM_CHAT_PROJECT="), "CM_CHAT_PROJECT must be absent when Project is empty")
		assert.False(t, strings.HasPrefix(e, "CM_CHAT_REPO_URL="), "CM_CHAT_REPO_URL must be absent when RepoURL is empty")
	}
}

// TestManager_StartChat_ClaudeSettings verifies that CM_CLAUDE_SETTINGS is
// injected into chat containers when cfg.ClaudeSettings is non-empty (parity
// with the card-mode startContainer path). The entrypoint reads this to write
// $HOME/.claude/settings.json.
func TestManager_StartChat_ClaudeSettings(t *testing.T) {
	var capturedEnv []string

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedEnv = cfg.Env

		return container.CreateResponse{ID: "chat-ctr-cs"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	cfg := testConfig(t)
	cfg.ClaudeSettings = `{"theme":"dark"}`

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	_, err := mgr.StartChat(context.Background(), StartChatOpts{SessionID: "S-cs"})
	require.NoError(t, err)
	assert.Contains(t, capturedEnv, `CM_CLAUDE_SETTINGS={"theme":"dark"}`)
}

// TestManager_StartChat_ClaudeSettings_AbsentWhenEmpty verifies that
// CM_CLAUDE_SETTINGS is not set on chat containers when cfg.ClaudeSettings is
// empty.
func TestManager_StartChat_ClaudeSettings_AbsentWhenEmpty(t *testing.T) {
	var capturedEnv []string

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedEnv = cfg.Env

		return container.CreateResponse{ID: "chat-ctr-no-cs"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	_, err := mgr.StartChat(context.Background(), StartChatOpts{SessionID: "S-no-cs"})
	require.NoError(t, err)

	for _, e := range capturedEnv {
		assert.False(t, strings.HasPrefix(e, "CM_CLAUDE_SETTINGS="),
			"CM_CLAUDE_SETTINGS must not be set when cfg.ClaudeSettings is empty")
	}
}

// TestManager_StartChat_WorkerExtraEnv verifies that deployment-wide
// WorkerExtraEnv values are propagated into chat containers (parity with the
// card-mode startContainer path) in sorted order.
func TestManager_StartChat_WorkerExtraEnv(t *testing.T) {
	var capturedEnv []string

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedEnv = cfg.Env

		return container.CreateResponse{ID: "chat-ctr-extra"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	cfg := testConfig(t)
	cfg.WorkerExtraEnv = map[string]string{
		"ZEBRA": "z",
		"ALPHA": "a",
		"MIKE":  "m",
	}

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	_, err := mgr.StartChat(context.Background(), StartChatOpts{SessionID: "S-extra"})
	require.NoError(t, err)

	picked := []string{}

	for _, e := range capturedEnv {
		switch {
		case strings.HasPrefix(e, "ALPHA="),
			strings.HasPrefix(e, "MIKE="),
			strings.HasPrefix(e, "ZEBRA="):
			picked = append(picked, e)
		}
	}

	assert.Equal(t, []string{"ALPHA=a", "MIKE=m", "ZEBRA=z"}, picked,
		"WorkerExtraEnv must be appended in sorted order (parity with card-mode)")
}

// TestManager_StartChat_TaskSkillsMount verifies that when cfg.TaskSkillsDir is
// configured, chat containers get the /host-skills bind-mount and
// pullSkillsRepo is invoked with the git token from opts.GitToken.
func TestManager_StartChat_TaskSkillsMount(t *testing.T) {
	var (
		capturedHostCfg *container.HostConfig
		pullCalls       []string
		pullTokens      []string
	)

	orig := pullSkillsRepo

	t.Cleanup(func() { pullSkillsRepo = orig })

	pullSkillsRepo = func(_ context.Context, dir, token string) error {
		pullCalls = append(pullCalls, dir)
		pullTokens = append(pullTokens, token)

		return nil
	}

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, _ *container.Config, hostCfg *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedHostCfg = hostCfg

		return container.CreateResponse{ID: "chat-ctr-skills"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	skillsDir := t.TempDir()

	cfg := testConfig(t)
	cfg.TaskSkillsDir = skillsDir

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	_, err := mgr.StartChat(context.Background(), StartChatOpts{
		SessionID: "S-skills",
		GitToken:  "gh-token-for-pull",
	})
	require.NoError(t, err)
	require.NotNil(t, capturedHostCfg)

	require.Len(t, pullCalls, 1, "pullSkillsRepo must be invoked exactly once")
	assert.Equal(t, skillsDir, pullCalls[0])
	assert.Equal(t, "gh-token-for-pull", pullTokens[0],
		"pullSkillsRepo must receive the git token from GitToken")

	var hasSkillsMount bool

	for _, m := range capturedHostCfg.Mounts {
		if m.Target == "/host-skills" {
			hasSkillsMount = true

			assert.Equal(t, skillsDir, m.Source)
			assert.True(t, m.ReadOnly, "/host-skills mount must be read-only")
			assert.Equal(t, mount.TypeBind, m.Type)
		}
	}

	assert.True(t, hasSkillsMount, "chat container HostConfig must bind-mount /host-skills when TaskSkillsDir is set")
}

// TestManager_StartChat_TaskSkillsExplicitSet verifies that when
// StartChatOpts.TaskSkills is non-nil, CM_TASK_SKILLS_SET=1 and CM_TASK_SKILLS
// (comma-separated) are set on the container env.
func TestManager_StartChat_TaskSkillsExplicitSet(t *testing.T) {
	var capturedEnv []string

	orig := pullSkillsRepo

	t.Cleanup(func() { pullSkillsRepo = orig })

	pullSkillsRepo = func(_ context.Context, _, _ string) error { return nil }

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedEnv = cfg.Env

		return container.CreateResponse{ID: "chat-ctr-skills-set"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	cfg := testConfig(t)
	cfg.TaskSkillsDir = t.TempDir()

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	skills := []string{"create-plan", "review"}

	_, err := mgr.StartChat(context.Background(), StartChatOpts{
		SessionID:  "S-skills-set",
		TaskSkills: &skills,
	})
	require.NoError(t, err)

	assert.Contains(t, capturedEnv, "CM_TASK_SKILLS_SET=1")
	assert.Contains(t, capturedEnv, "CM_TASK_SKILLS=create-plan,review")
}

// TestManager_StartChat_TaskSkillsAbsentWhenNil verifies that with
// TaskSkills==nil the env vars are NOT set, so the entrypoint copies the full
// skills set (parity with the card-mode "no constraint" branch).
func TestManager_StartChat_TaskSkillsAbsentWhenNil(t *testing.T) {
	var capturedEnv []string

	orig := pullSkillsRepo

	t.Cleanup(func() { pullSkillsRepo = orig })

	pullSkillsRepo = func(_ context.Context, _, _ string) error { return nil }

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedEnv = cfg.Env

		return container.CreateResponse{ID: "chat-ctr-skills-nil"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	cfg := testConfig(t)
	cfg.TaskSkillsDir = t.TempDir()

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	_, err := mgr.StartChat(context.Background(), StartChatOpts{SessionID: "S-skills-nil"})
	require.NoError(t, err)

	for _, e := range capturedEnv {
		assert.False(t, strings.HasPrefix(e, "CM_TASK_SKILLS_SET="),
			"CM_TASK_SKILLS_SET must be absent when TaskSkills is nil")
		assert.False(t, strings.HasPrefix(e, "CM_TASK_SKILLS="),
			"CM_TASK_SKILLS must be absent when TaskSkills is nil")
	}
}

// TestManager_StreamChatLogs_PublishesAssistantText verifies that
// StreamChatLogs reads the chat container's stdout, parses claude
// stream-json, and republishes assistant text events to the broadcaster
// with SessionID set. Without this wiring, browser SSE never sees a reply.
func TestManager_StreamChatLogs_PublishesAssistantText(t *testing.T) {
	mock := successfulMock()
	mock.ContainerLogsFn = func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
		var buf strings.Builder

		w := stdcopy.NewStdWriter(&buf, stdcopy.Stdout)
		_, _ = w.Write([]byte(
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello back."}]}}` + "\n",
		))

		return io.NopCloser(strings.NewReader(buf.String())), nil
	}

	rec := newRecordingBroadcaster()
	defer rec.Close()

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	mgr := NewManager(mock, tr, cb, tp, rec.Broadcaster(), testConfig(t), testLogger())

	mgr.StreamChatLogs(context.Background(), "S-stream", "chat-ctr-1", "alpha")
	mgr.Wait()

	// mgr.Wait drains the streaming goroutine, but Broadcaster.Publish is
	// async: events are queued onto a channel that the recording drain
	// goroutine consumes. Poll briefly until the drain catches up.
	var entries []logbroadcast.LogEntry

	require.Eventually(t, func() bool {
		entries = rec.filterType("text")

		return len(entries) > 0
	}, time.Second, 5*time.Millisecond, "expected at least one text LogEntry")

	assert.Equal(t, "S-stream", entries[0].SessionID)
	assert.Empty(t, entries[0].CardID, "chat LogEntry must not carry a CardID")
	assert.Equal(t, "alpha", entries[0].Project)
	assert.Contains(t, entries[0].Content, "Hello back.")
}

// TestListManaged_IncludesChatContainers verifies that chat-mode containers
// (labelled with LabelSessionID but no LabelCardID) are surfaced by the
// /containers operator endpoint. Without this CM cannot reconcile orphan
// chat sessions because it never learns about the containers.
func TestListManaged_IncludesChatContainers(t *testing.T) {
	mock := successfulMock()
	mock.ContainerListFn = func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
		return []DockerContainer{
			{
				ID:      "card-ctr-1",
				Names:   []string{"/cmr-proj-a-001"},
				State:   "running",
				Created: time.Now().Add(-5 * time.Minute).Unix(),
				Labels: map[string]string{
					LabelRunner:  "true",
					LabelProject: "proj",
					LabelCardID:  "A-001",
				},
			},
			{
				ID:      "chat-ctr-tracked",
				Names:   []string{"/cmr-chat-s-known"},
				State:   "running",
				Created: time.Now().Add(-2 * time.Minute).Unix(),
				Labels: map[string]string{
					LabelRunner:    "true",
					LabelProject:   "proj",
					LabelSessionID: "S-known",
				},
			},
			{
				ID:      "chat-ctr-untracked",
				Names:   []string{"/cmr-chat-s-orphan"},
				State:   "running",
				Created: time.Now().Add(-1 * time.Minute).Unix(),
				Labels: map[string]string{
					LabelRunner:    "true",
					LabelSessionID: "S-orphan",
				},
			},
			{
				// Neither cardID nor sessionID — must still be skipped.
				ID:     "mislabeled",
				Labels: map[string]string{LabelRunner: "true", LabelProject: "proj"},
				State:  "running",
			},
		}, nil
	}

	tr := tracker.New()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		Project: "proj", CardID: "A-001", ContainerID: "card-ctr-1",
	}))
	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{
		SessionID: "S-known", Project: "proj", ContainerID: "chat-ctr-tracked",
	}))

	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	got, err := mgr.ListManaged(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 3,
		"card container + tracked chat + orphan chat must be listed; mislabeled must be skipped")

	byID := make(map[string]ManagedContainer, len(got))
	for _, c := range got {
		byID[c.ContainerID] = c
	}

	require.Contains(t, byID, "card-ctr-1")
	assert.Equal(t, "A-001", byID["card-ctr-1"].CardID)
	assert.Empty(t, byID["card-ctr-1"].SessionID, "card container must have empty SessionID")
	assert.True(t, byID["card-ctr-1"].Tracked)

	require.Contains(t, byID, "chat-ctr-tracked")
	assert.Empty(t, byID["chat-ctr-tracked"].CardID, "chat container must have empty CardID")
	assert.Equal(t, "S-known", byID["chat-ctr-tracked"].SessionID)
	assert.True(t, byID["chat-ctr-tracked"].Tracked, "tracked chat must report Tracked=true")

	require.Contains(t, byID, "chat-ctr-untracked")
	assert.Equal(t, "S-orphan", byID["chat-ctr-untracked"].SessionID)
	assert.False(t, byID["chat-ctr-untracked"].Tracked,
		"chat container missing from tracker (divergence) must report Tracked=false")
}

// TestCleanupOrphans_SkipsTrackedChat guards against the regression where the
// maintenance loop would kill every active chat container on every tick: chat
// containers carry LabelSessionID (not LabelCardID), so the (project, cardID)
// tracker check fell through and classified them as orphans.
func TestCleanupOrphans_SkipsTrackedChat(t *testing.T) {
	var removedIDs []string

	mock := successfulMock()
	mock.ContainerListFn = func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
		return []DockerContainer{
			{ID: "live-chat", Labels: map[string]string{
				LabelRunner: "true", LabelProject: "proj", LabelSessionID: "S-live",
			}},
			{ID: "orphan-chat", Labels: map[string]string{
				LabelRunner: "true", LabelProject: "proj", LabelSessionID: "S-orphan",
			}},
		}, nil
	}
	mock.ContainerRemoveFn = func(_ context.Context, id string, _ container.RemoveOptions) error {
		removedIDs = append(removedIDs, id)

		return nil
	}

	tr := tracker.New()
	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{
		SessionID: "S-live", Project: "proj", ContainerID: "live-chat",
	}))

	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	err := mgr.CleanupOrphans(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"orphan-chat"}, removedIDs,
		"only the untracked chat container must be removed; tracked live-chat must survive")
}

// TestManager_DeleteChatCleanup_UnlinksHostFiles guards the leak in the
// AddChatIfUnderLimit rollback path: after StartChat mounts the shared secrets
// dir and creates the resume directory, the webhook handler calls
// DeleteChatCleanup as part of its rollback. The wait goroutine is NOT yet
// running, so DeleteChatCleanup itself must run the resume cleanup.
// The shared secrets dir must NOT be removed — it is owned by the refresher.
func TestManager_DeleteChatCleanup_UnlinksHostFiles(t *testing.T) {
	var capturedHostCfg *container.HostConfig

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, _ *container.Config, hostCfg *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedHostCfg = hostCfg

		return container.CreateResponse{ID: "chat-ctr-rollback"}, nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	cfg := testConfig(t)
	cfg.DeploymentProfile = config.ProfileProduction

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())
	require.NoError(t, mgr.InitSharedSecrets())

	containerID, err := mgr.StartChat(context.Background(), StartChatOpts{
		SessionID: "S-rollback",
		Project:   "alpha",
		RepoURL:   "https://example.com/alpha.git",
		MCPAPIKey: "mcp-secret-key",
		GitToken:  "gh-token-secret",
		Resume: &ChatResume{
			Turns: []ChatResumeTurn{
				{Seq: 1, Role: "user", Content: "hi"},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "chat-ctr-rollback", containerID)
	require.NotNil(t, capturedHostCfg)

	// Recover the host-side paths from the captured mount config.
	var secretsSource, resumeDir string

	for _, m := range capturedHostCfg.Mounts {
		switch m.Target {
		case secretsMountTarget:
			secretsSource = m.Source
		case chatResumeMountTarget:
			resumeDir = m.Source
		}
	}

	require.NotEmpty(t, secretsSource, "secrets mount must be present")
	require.NotEmpty(t, resumeDir, "resume dir mount must be present")
	// The secrets source is the shared dir (not a per-container file).
	assert.Equal(t, filepath.Join(cfg.SecretsDir, "shared"), secretsSource,
		"secrets mount source must be the shared dir")
	require.DirExists(t, resumeDir, "resume dir must exist after StartChat")

	// Simulate the webhook handler's AddChatIfUnderLimit rollback.
	mgr.DeleteChatCleanup(containerID)

	// The shared secrets dir must NOT be removed by DeleteChatCleanup.
	_, statErr := os.Stat(secretsSource)
	assert.False(t, os.IsNotExist(statErr),
		"DeleteChatCleanup must NOT remove the shared secrets dir")

	assert.NoDirExists(t, resumeDir,
		"DeleteChatCleanup must remove the host-side resume directory")
}

// TestManager_StartChat_WaitCleanupRemovesTrackerOnExit verifies that the
// wait-and-cleanup path waits for the container to exit and then removes the
// tracker entry plus force-removes the container. This covers the implicit-
// exit paths (claude crash, OOM, external kill) where /chat/end is NOT called
// by the user but the container exits anyway.
//
// The wait goroutine is launched by the webhook handler after the tracker
// entry is registered (so an instant container exit cannot race AddChat); the
// test simulates that ordering explicitly by calling WaitAndCleanupChat after
// tr.AddChat.
func TestManager_StartChat_WaitCleanupRemovesTrackerOnExit(t *testing.T) {
	waitCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)

	var (
		removeCalls atomic.Int32
		removedIDs  sync.Map
	)

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		return container.CreateResponse{ID: "chat-ctr-wait"}, nil
	}
	mock.ContainerWaitFn = func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		return waitCh, errCh
	}
	mock.ContainerRemoveFn = func(_ context.Context, id string, _ container.RemoveOptions) error {
		removeCalls.Add(1)
		removedIDs.Store(id, true)

		return nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	containerID, err := mgr.StartChat(context.Background(), StartChatOpts{
		SessionID: "S-wait",
		Project:   "alpha",
	})
	require.NoError(t, err)
	assert.Equal(t, "chat-ctr-wait", containerID)

	// Simulate the webhook handler registering the tracker entry after
	// StartChat returns and then launching the wait goroutine. Without
	// this ordering, the cleanup goroutine has nothing to remove.
	_, streamCancel := context.WithCancel(context.Background())
	t.Cleanup(streamCancel)
	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{
		ContainerID: containerID,
		SessionID:   "S-wait",
		Project:     "alpha",
		StartedAt:   time.Now(),
		Cancel:      streamCancel,
	}))

	mgr.WaitAndCleanupChat("S-wait", containerID, "alpha")

	require.True(t, tr.HasChat("S-wait"), "tracker entry should exist before container exit")

	// Fire the container exit. The wait goroutine must observe this,
	// remove the tracker entry, and force-remove the container.
	waitCh <- container.WaitResponse{StatusCode: 0}

	mgr.Wait()

	assert.False(t, tr.HasChat("S-wait"), "tracker entry must be removed after container exit")
	assert.GreaterOrEqual(t, int(removeCalls.Load()), 1, "ContainerRemove must be called at least once on the chat container")

	if _, ok := removedIDs.Load("chat-ctr-wait"); !ok {
		t.Fatalf("expected ContainerRemove to be called with chat-ctr-wait, removed IDs: %v", &removedIDs)
	}
}

// stdinCloseSpy is a WriteCloser that records whether Close() was called.
type stdinCloseSpy struct {
	closed atomic.Bool
}

func (s *stdinCloseSpy) Write(p []byte) (int, error) { return len(p), nil }
func (s *stdinCloseSpy) Close() error {
	s.closed.Store(true)

	return nil
}

// TestKillChat_ClosesStdinThenStops verifies that KillChat:
//  1. closes the container's stdin so claude can flush its final stream-json batch,
//  2. calls ContainerStop (SIGTERM + grace) on the chat container,
//  3. calls ContainerRemove to clean up.
//
// This mirrors the graceful kill path that card-mode containers already receive
// via Kill + the context-cancel chain; KillChat gives chat containers the same
// treatment.
func TestKillChat_ClosesStdinThenStops(t *testing.T) {
	t.Parallel()

	var (
		stoppedID atomic.Value
		removedID atomic.Value
	)

	mock := successfulMock()
	mock.ContainerStopFn = func(_ context.Context, id string, _ container.StopOptions) error {
		stoppedID.Store(id)

		return nil
	}
	mock.ContainerRemoveFn = func(_ context.Context, id string, _ container.RemoveOptions) error {
		removedID.Store(id)

		return nil
	}

	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	const (
		sessionID   = "01TESTCHAT"
		containerID = "ctr-xyz"
	)

	spy := &stdinCloseSpy{}

	_, streamCancel := context.WithCancel(context.Background())
	t.Cleanup(streamCancel)

	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{
		ContainerID: containerID,
		SessionID:   sessionID,
		Project:     "global",
		StartedAt:   time.Now(),
		Cancel:      streamCancel,
	}))

	// Attach the spy stdin so CloseStdinChat has something to close.
	tr.SetStdinChat(sessionID, spy, nil)

	require.NoError(t, mgr.KillChat(context.Background(), sessionID))

	assert.True(t, spy.closed.Load(), "KillChat must close container stdin")
	assert.Equal(t, containerID, stoppedID.Load(), "KillChat must call ContainerStop on the chat container")
	assert.Equal(t, containerID, removedID.Load(), "KillChat must call ContainerRemove on the chat container")
}

// TestKillChat_NotFound verifies that KillChat returns an error when no chat
// container is tracked for the given session ID.
func TestKillChat_NotFound(t *testing.T) {
	t.Parallel()

	mock := successfulMock()
	mgr := NewManager(mock, tracker.New(), nil, testPATProvider(t), nil, testConfig(t), testLogger())

	err := mgr.KillChat(context.Background(), "no-such-session")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-session")
}

// TestKillChat_NoStdinAttached verifies that KillChat succeeds (still calls
// ContainerStop/Remove) even when no stdin has been attached yet to the chat
// container — covering the race window between tracker.AddChat and
// tracker.SetStdinChat.
func TestKillChat_NoStdinAttached(t *testing.T) {
	t.Parallel()

	var stopped atomic.Bool

	mock := successfulMock()
	mock.ContainerStopFn = func(_ context.Context, _ string, _ container.StopOptions) error {
		stopped.Store(true)

		return nil
	}
	mock.ContainerRemoveFn = func(_ context.Context, _ string, _ container.RemoveOptions) error {
		return nil
	}

	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, testPATProvider(t), nil, testConfig(t), testLogger())

	_, streamCancel := context.WithCancel(context.Background())
	t.Cleanup(streamCancel)

	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{
		ContainerID: "ctr-nostdin",
		SessionID:   "no-stdin-session",
		Project:     "global",
		StartedAt:   time.Now(),
		Cancel:      streamCancel,
	}))

	// No SetStdinChat call — stdin is nil.
	require.NoError(t, mgr.KillChat(context.Background(), "no-stdin-session"))
	assert.True(t, stopped.Load(), "ContainerStop must be called even when stdin is not attached")
}

// TestBuildChatAuthEnv_ReturnsGitToken verifies that BuildChatAuthEnv returns
// a non-empty token when a token provider is wired AND a skills dir is
// configured. Claude auth (OAuth token, API key) is owned by the tokenRefresher
// and reaches the worker via the shared secrets dir — this function must not
// return those credentials.
func TestBuildChatAuthEnv_ReturnsGitToken(t *testing.T) {
	t.Parallel()

	tp := testPATProvider(t)
	cfg := &config.Config{
		ClaudeOAuthToken: "should-not-appear",
		AnthropicAPIKey:  "neither",
		// TaskSkillsDir non-empty: the mint is gated on a real consumer for
		// the token (the skills bind-mount git pull).
		TaskSkillsDir: t.TempDir(),
	}

	mgr := NewManager(nil, tracker.New(), nil, tp, nil, cfg, testLogger())
	tok := mgr.BuildChatAuthEnv(context.Background())

	require.NotEmpty(t, tok, "BuildChatAuthEnv must return a non-empty token when a provider is wired")
}

// TestBuildChatAuthEnv_SkipsMintWhenNoSkillsDir keeps chat-mode symmetric with
// the card-mode startContainer mint guard: without TaskSkillsDir the token has
// no consumer, so /chat/start must pay zero GitHub API round-trips.
func TestBuildChatAuthEnv_SkipsMintWhenNoSkillsDir(t *testing.T) {
	t.Parallel()

	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ghs_should_not_be_used",
			"expires_at": "2030-01-01T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)

	tp, err := githubauth.NewAppProviderWithKey(12345, 67890, testRSAKey(), srv.URL)
	require.NoError(t, err)

	cfg := &config.Config{
		// TaskSkillsDir intentionally empty.
	}

	mgr := NewManager(nil, tracker.New(), nil, tp, nil, cfg, testLogger())
	tok := mgr.BuildChatAuthEnv(context.Background())

	require.Empty(t, tok, "BuildChatAuthEnv must return empty when TaskSkillsDir is unset")
	require.Equal(t, 0, calls, "BuildChatAuthEnv must not call GenerateToken when TaskSkillsDir is unset")
}

// TestManager_StartChat_ResumeMountReadOnly verifies that StartChat binds
// /run/cm-chat with ReadOnly: true so the container cannot overwrite the
// rehydration payload.
func TestManager_StartChat_ResumeMountReadOnly(t *testing.T) {
	t.Parallel()

	var capturedHostCfg *container.HostConfig

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, _ *container.Config, hostCfg *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedHostCfg = hostCfg

		return container.CreateResponse{ID: "chat-resume-ro"}, nil
	}

	cfg := testConfig(t)

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	_, err := mgr.StartChat(context.Background(), StartChatOpts{
		SessionID: "01CHATRESUMERO",
		Project:   "p",
		Resume: &ChatResume{
			Turns: []ChatResumeTurn{{Seq: 1, Role: "user", Content: "hi"}},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, capturedHostCfg)

	found := false

	for _, m := range capturedHostCfg.Mounts {
		if m.Target == chatResumeMountTarget {
			found = true

			require.True(t, m.ReadOnly, "resume mount must be ReadOnly: true")
			require.Equal(t, mount.TypeBind, m.Type, "resume mount must be a bind mount")
		}
	}

	require.True(t, found, "resume mount (%s) not found in HostConfig.Mounts", chatResumeMountTarget)
}

// TestStartChat_ModelEnvForwarded verifies that StartChatOpts.Model becomes
// CM_ORCHESTRATOR_MODEL=<id> in the container's Config.Env.
func TestStartChat_ModelEnvForwarded(t *testing.T) {
	t.Parallel()

	var capturedEnv []string

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		capturedEnv = cfg.Env

		return container.CreateResponse{ID: "chat-model-test"}, nil
	}

	cfg := testConfig(t)

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	_, err := mgr.StartChat(context.Background(), StartChatOpts{
		SessionID: "01CHATMODEL",
		Project:   "p",
		Model:     "claude-opus-4-7",
	})
	require.NoError(t, err)
	require.NotNil(t, capturedEnv)
	require.Contains(t, capturedEnv, "CM_ORCHESTRATOR_MODEL=claude-opus-4-7")
}

// TestStartChat_ResumeEnvOnlyWhenSucceeded verifies that CM_CHAT_RESUME=1
// is set in the container's Config.Env only when prepareChatResume succeeds.
// When prepareChatResume fails, the container still starts but CM_CHAT_RESUME=1
// is not present in the env.
func TestStartChat_ResumeEnvOnlyWhenSucceeded(t *testing.T) {
	t.Parallel()

	// Success path: CM_CHAT_RESUME=1 is present.
	{
		var capturedEnv []string

		mock := successfulMock()
		mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			capturedEnv = cfg.Env

			return container.CreateResponse{ID: "chat-resume-success"}, nil
		}

		cfg := testConfig(t)

		tr := tracker.New()
		cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
		tp := testPATProvider(t)

		mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

		_, err := mgr.StartChat(context.Background(), StartChatOpts{
			SessionID: "01CHATRESUME",
			Project:   "p",
			Resume: &ChatResume{
				Turns: []ChatResumeTurn{{Seq: 1, Role: "user", Content: "hi"}},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, capturedEnv)
		require.Contains(t, capturedEnv, "CM_CHAT_RESUME=1")
	}

	// Failure path: prepareChatResume fails. Container still starts; CM_CHAT_RESUME=1
	// is not in env. We inject the failure by mocking mkdirAll to reject calls after
	// secrets setup has succeeded (second mkdir call is for the per-container
	// resume directory).
	{
		var (
			capturedEnv []string
			mkdirCount  atomic.Int32
		)

		mock := successfulMock()
		mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			capturedEnv = cfg.Env

			return container.CreateResponse{ID: "chat-resume-fail"}, nil
		}

		cfg := testConfig(t)
		cfg.DeploymentProfile = config.ProfileProduction

		tr := tracker.New()
		cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
		tp := testPATProvider(t)

		mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

		// Mock mkdirAll to succeed for the first couple calls (secrets prep) but
		// fail for the per-container resume directory. This isolates the
		// prepareChatResume failure from secrets prep.
		mgr.mkdirAll = func(path string, perm os.FileMode) error {
			count := mkdirCount.Add(1)
			// buildSecretDelivery makes no mkdirAll calls in file mode;
			// all mkdirAll calls here are for the per-container resume dir.
			if count > 1 {
				return fmt.Errorf("injected mkdir failure for resume dir")
			}

			return os.MkdirAll(path, perm)
		}

		_, err := mgr.StartChat(context.Background(), StartChatOpts{
			SessionID: "01CHATRESUMEFAIL",
			Project:   "p",
			Resume: &ChatResume{
				Turns: []ChatResumeTurn{{Seq: 1, Role: "user", Content: "hi"}},
			},
		})
		require.NoError(t, err, "container still starts despite prepareChatResume failure")
		require.NotNil(t, capturedEnv)
		require.NotContains(t, capturedEnv, "CM_CHAT_RESUME=1", "CM_CHAT_RESUME must not be set when prepareChatResume fails")
	}
}

// TestManager_PrepareChatResume_FilePerms verifies that prepareChatResume
// creates the host directory with 0700 permissions and resume.jsonl with
// 0600 permissions.
func TestManager_PrepareChatResume_FilePerms(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)

	mgr := NewManager(nil, tracker.New(), nil, testPATProvider(t), nil, cfg, testLogger())

	resume := &ChatResume{
		Turns: []ChatResumeTurn{{Seq: 1, Role: "user", Content: "hi"}},
	}

	delivery, err := mgr.PrepareChatResumeForTest("ctr-perms-test", "01SESS", "proj", resume)
	require.NoError(t, err)
	require.NotEmpty(t, delivery.DirPath)

	t.Cleanup(func() { _ = os.RemoveAll(delivery.DirPath) })

	info, err := os.Stat(delivery.DirPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "host dir must have 0700 permissions")

	info, err = os.Stat(filepath.Join(delivery.DirPath, "resume.jsonl"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "resume.jsonl must have 0600 permissions")
}

// TestManager_PrepareChatResume_NonceUniqueness verifies that repeated calls to
// prepareChatResume produce unique host directories even for the same container
// name and session, preventing one call from colliding with or overwriting another.
func TestManager_PrepareChatResume_NonceUniqueness(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)

	mgr := NewManager(nil, tracker.New(), nil, testPATProvider(t), nil, cfg, testLogger())

	resume := &ChatResume{
		Turns: []ChatResumeTurn{{Seq: 1, Role: "user", Content: "hi"}},
	}

	seen := make(map[string]struct{})

	for i := range 50 {
		d, err := mgr.PrepareChatResumeForTest("ctr-nonce-test", "01SESS", "proj", resume)
		require.NoError(t, err)

		_, collision := seen[d.DirPath]
		require.False(t, collision, "host dir nonce collision at iteration %d: %s", i, d.DirPath)

		seen[d.DirPath] = struct{}{}
		_ = os.RemoveAll(d.DirPath)
	}
}

// TestManager_PrepareChatResume_CleansUpOnWriteFailure verifies that when
// createFile fails (e.g., write fails), the host directory is cleaned up and
// not left behind.
func TestManager_PrepareChatResume_CleansUpOnWriteFailure(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)

	mgr := NewManager(nil, tracker.New(), nil, testPATProvider(t), nil, cfg, testLogger())

	// Inject a createFile that fails for resume.jsonl but succeeds for others
	mgr.SetCreateFileForTest(func(path string) (*os.File, error) {
		if strings.HasSuffix(path, "resume.jsonl") {
			return nil, errors.New("forced write failure")
		}

		return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL|os.O_TRUNC, 0o600)
	})

	resume := &ChatResume{
		Turns: []ChatResumeTurn{{Seq: 1, Role: "user", Content: "hello"}},
	}

	_, err := mgr.PrepareChatResumeForTest("ctr-cleanup-test", "01SESS", "proj", resume)
	require.Error(t, err, "prepareChatResume should fail when createFile fails")
	require.ErrorContains(t, err, "forced write failure")

	// Verify that the host directory was cleaned up (no leaked dir)
	matches, err := filepath.Glob(filepath.Join(cfg.SecretsDir, "cmr-chat-resume-ctr-cleanup-test-*"))
	require.NoError(t, err)
	require.Empty(t, matches, "failed write must clean up the host directory, got %v", matches)
}

// TestManager_PrepareChatResume_FallsBackToTmpInDevMode verifies that when the
// configured secrets dir is not writable and the manager is in dev mode, the
// function falls back to writing under os.TempDir().
func TestManager_PrepareChatResume_FallsBackToTmpInDevMode(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	// Use a non-existent directory that will fail on mkdir
	cfg.SecretsDir = "/nonexistent/path/that/cannot/be/created"
	cfg.DeploymentProfile = "dev"

	mgr := NewManager(nil, tracker.New(), nil, testPATProvider(t), nil, cfg, testLogger())

	resume := &ChatResume{
		Turns: []ChatResumeTurn{{Seq: 1, Role: "user", Content: "fallback test"}},
	}

	delivery, err := mgr.PrepareChatResumeForTest("ctr-fallback-test", "01SESS", "proj", resume)
	require.NoError(t, err, "prepareChatResume should succeed with tmp fallback in dev mode")
	require.NotEmpty(t, delivery.DirPath)

	t.Cleanup(func() { _ = os.RemoveAll(delivery.DirPath) })

	// Verify the directory was created under TempDir, not under the configured (inaccessible) secrets dir
	require.True(t,
		strings.HasPrefix(delivery.DirPath, os.TempDir()),
		"dev fallback must write under TempDir, got %s", delivery.DirPath,
	)

	// Verify the directory actually exists and contains resume files
	info, err := os.Stat(delivery.DirPath)
	require.NoError(t, err)
	require.True(t, info.IsDir(), "delivery.DirPath must be a directory")

	_, err = os.Stat(filepath.Join(delivery.DirPath, "resume.jsonl"))
	require.NoError(t, err, "resume.jsonl must exist in the fallback directory")

	_, err = os.Stat(filepath.Join(delivery.DirPath, "resume.meta.json"))
	require.NoError(t, err, "resume.meta.json must exist in the fallback directory")
}

func TestInitSharedSecretsCreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{SecretsDir: dir}

	m := &Manager{
		cfg:      cfg,
		logger:   slog.Default(),
		mkdirAll: os.MkdirAll,
	}

	require.NoError(t, m.InitSharedSecrets())

	info, err := os.Stat(filepath.Join(dir, "shared"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestInitSharedSecretsNoopWhenSecretsDirEmpty(t *testing.T) {
	m := &Manager{
		cfg:      &config.Config{SecretsDir: ""},
		logger:   slog.Default(),
		mkdirAll: os.MkdirAll,
	}
	require.NoError(t, m.InitSharedSecrets())
}

func TestStartTokenRefresherWritesSecretsFile(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.Config{
		SecretsDir:       dir,
		ClaudeOAuthToken: "sk-ant-oat01-abc",
	}
	m := &Manager{
		cfg:      cfg,
		logger:   slog.Default(),
		token:    &fakeTokenGen{token: "ghs_init", expires: time.Now().Add(1 * time.Hour)},
		mkdirAll: os.MkdirAll,
	}

	ctx, cancel := context.WithCancel(context.Background())
	// LIFO: wg.Wait runs after cancel so the goroutine exits before the drain.
	defer m.wg.Wait()
	defer cancel()

	require.NoError(t, m.StartTokenRefresher(ctx))

	// Synchronous initial mint guarantees the file exists on return.
	b, err := os.ReadFile(filepath.Join(dir, "shared", "env"))
	require.NoError(t, err)
	assert.Contains(t, string(b), "ghs_init")
}

// TestStreamLogs_OversizedStderrLine_DoesNotWedge exercises Fix M2: when a
// stderr line exceeds bufio.Scanner's 1 MiB cap, the scanner aborts with
// bufio.ErrTooLong. The pipe writer (stdcopy) would otherwise block forever
// on its next Write into the now-undrained pipe — wedging the outer
// streamLogs goroutine and blocking tracker.Remove + removeContainer.
//
// The fix closes stderrPr on scanner exit AND drains any in-flight bytes, so
// stdcopy completes and the outer goroutine sees EOF on the docker reader.
//
// We verify the cleanup sequence runs within a bounded deadline rather than
// hanging until the test timeout, which is what the pre-fix behaviour would
// look like.
func TestStreamLogs_OversizedStderrLine_DoesNotWedge(t *testing.T) {
	// Build a Docker multiplexed stream with a single stderr "line" larger
	// than the 1 MiB scanner cap, followed by EOF on the underlying reader.
	var buf bytes.Buffer

	stderrW := stdcopy.NewStdWriter(&buf, stdcopy.Stderr)
	// 1.5 MiB of 'a' followed by a single newline — well above the 1 MiB cap
	// so scanner.Scan returns false with bufio.ErrTooLong.
	huge := make([]byte, (3*1024*1024)/2)
	for i := range huge {
		huge[i] = 'a'
	}

	_, err := stderrW.Write(append(huge, '\n'))
	require.NoError(t, err)

	mock := successfulMock()
	mock.ContainerLogsFn = func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
		// io.NopCloser around the buffer EOFs at end-of-bytes, so stdcopy
		// returns and the outer goroutine closes `done`. Without the fix
		// the stdcopy Write into the stderr pipe would block forever
		// because no one is reading after scanner aborts.
		return io.NopCloser(&buf), nil
	}

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cbSrv.Close()

	tr := tracker.New()
	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	// The whole Run + Wait cycle must complete within a bounded window. The
	// pre-fix behaviour would hang stdcopy forever and waitAndCleanup would
	// only release after the configured ContainerTimeout (1h in testConfig)
	// — way past any reasonable test deadline.
	done := make(chan struct{})

	go func() {
		defer close(done)

		mgr.Run(context.Background(), payload)
		mgr.Wait()
	}()

	select {
	case <-done:
		// good — pipeline drained the oversized stderr and returned.
	case <-time.After(15 * time.Second):
		t.Fatal("streamLogs wedged on oversized stderr line; M2 fix is regressed")
	}

	assert.Equal(t, 0, tr.Count(), "tracker entry must be removed after Run completes")
}

// TestWaitAndCleanupChat_RemovesChatSecrets exercises Fix M3: the
// AttachChatStdin-failure rollback path used to invoke RemoveChat + Stop +
// WaitAndCleanupChat without touching DeleteChatCleanup, leaving the
// chatSecrets entry stranded forever. WaitAndCleanupChat must now drop the
// entry regardless of who consumed it.
func TestWaitAndCleanupChat_RemovesChatSecrets(t *testing.T) {
	t.Parallel()

	containerID := "ctr-leak-test"

	waitCh := make(chan container.WaitResponse, 1)
	waitCh <- container.WaitResponse{StatusCode: 0}

	mock := successfulMock()
	mock.ContainerWaitFn = func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		return waitCh, make(chan error)
	}
	mock.ContainerRemoveFn = func(_ context.Context, _ string, _ container.RemoveOptions) error {
		return nil
	}

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())
	tp := testPATProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(t), testLogger())

	// Stash a chat secrets entry directly (simulating StartChat's bookkeeping).
	mgr.chatSecretsMu.Lock()
	mgr.chatSecrets[containerID] = []string{"sk-secret"}
	mgr.chatSecretsMu.Unlock()

	mgr.WaitAndCleanupChat("sess-id", containerID, "proj")
	mgr.Wait()

	mgr.chatSecretsMu.Lock()
	_, leaked := mgr.chatSecrets[containerID]
	count := len(mgr.chatSecrets)
	mgr.chatSecretsMu.Unlock()

	assert.False(t, leaked,
		"chatSecrets entry for %s must be deleted by WaitAndCleanupChat to plug AttachChatStdin-failure leak", containerID)
	assert.Equal(t, 0, count, "chatSecrets must be empty after WaitAndCleanupChat completes")
}

// TestStartContainer_SkipsTokenMintWhenNoSkillsDir exercises Fix M5: when
// TaskSkillsDir is unset, m.token.GenerateToken must not be called at all
// during startContainer. A transient GitHub-API failure on a deployment that
// does not bind-mount a skills clone used to block every card spawn.
func TestStartContainer_SkipsTokenMintWhenNoSkillsDir(t *testing.T) {
	t.Parallel()

	// alwaysFailingTokenGen returns an error for every GenerateToken call.
	// Wired into m.token to prove startContainer does NOT call it when
	// TaskSkillsDir == "" (the production posture for deployments without
	// a skills clone).
	alwaysFail := &failingTokenGen{err: errors.New("synthetic github API failure")}

	cfg := testConfig(t)
	// Default testConfig has TaskSkillsDir == "" already, but be explicit
	// because the test depends on it.
	cfg.TaskSkillsDir = ""
	// SecretsDir != "" means buildSecretDelivery uses file mode and does
	// NOT mint a token of its own (the refresher owns that key). Combined
	// with TaskSkillsDir == "", no GenerateToken call should be made.
	require.NotEmpty(t, cfg.SecretsDir)

	mock := successfulMock()

	tr := tracker.New()
	cb := callback.NewClient("http://unused:9999", "test-secret-key-that-is-long-enough", testLogger())

	mgr := NewManager(mock, tr, cb, alwaysFail, nil, cfg, testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	// startContainer should NOT propagate any token-mint error because it
	// must not call GenerateToken at all in this configuration. Run end-to-end
	// proves the spawn path succeeds; the alwaysFail provider is wired so the
	// test fails loudly if anything mints a token.
	mgr.Run(context.Background(), payload)
	mgr.Wait()

	assert.Zero(t, alwaysFail.calls.Load(),
		"startContainer must not call GenerateToken when TaskSkillsDir is empty")
	assert.Equal(t, 0, tr.Count(), "tracker entry should be released after Run completes")
}

// failingTokenGen is a TokenGenerator that returns err on every call and
// counts invocations. Used by TestStartContainer_SkipsTokenMintWhenNoSkillsDir
// to prove that GenerateToken is never called when no skills clone is bound.
type failingTokenGen struct {
	err   error
	calls atomic.Int32
}

func (f *failingTokenGen) GenerateToken(_ context.Context) (string, time.Time, error) {
	f.calls.Add(1)

	return "", time.Time{}, f.err
}
