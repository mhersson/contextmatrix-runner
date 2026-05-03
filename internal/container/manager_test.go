package container

import (
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
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	githubauth "github.com/mhersson/contextmatrix-githubauth"

	"github.com/mhersson/contextmatrix-runner/internal/callback"
	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
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
		// Explicit ImagePullPolicy: tests assert ImagePullFn is invoked, so
		// PullAlways preserves the behavior that previously came from the
		// now-removed empty-string fallback in Manager.pullImage.
		ImagePullPolicy: config.PullAlways,
		// Use the OS temp dir instead of the production default
		// (/var/run/cm-runner/secrets) so tests work without root.
		SecretsDir: os.TempDir() + "/cm-runner-tests-secrets",
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
		// callback now fires on a detached goroutine (CTXRUN-059 H23) so
		// the handler can still be writing it after mgr.Wait() returns.
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

	// Secrets must NOT be in HostConfig.Env — they are written to a tmpfs
	// bind-mounted file at /run/cm-secrets/env instead (CTXRUN-043).
	for _, e := range createdEnv {
		assert.False(t, strings.HasPrefix(e, "CM_GIT_TOKEN="), "CM_GIT_TOKEN must not be in Env")
		assert.False(t, strings.HasPrefix(e, "ANTHROPIC_API_KEY="), "ANTHROPIC_API_KEY must not be in Env")
		assert.False(t, strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN="), "CLAUDE_CODE_OAUTH_TOKEN must not be in Env")
		assert.False(t, strings.HasPrefix(e, "CM_MCP_API_KEY="), "CM_MCP_API_KEY must not be in Env")
	}

	// Verify labels.
	assert.Equal(t, "true", createdLabels[LabelRunner])
	assert.Equal(t, "PROJ-042", createdLabels[LabelCardID])
	assert.Equal(t, "my-project", createdLabels[LabelProject])

	// Should have reported "running". The running-status callback runs on a
	// detached goroutine (CTXRUN-059 H23) so it may land after mgr.Wait()
	// returns — poll briefly with the mutex held.
	require.Eventually(t, func() bool {
		statusMu.Lock()
		defer statusMu.Unlock()

		for _, s := range reportedStatuses {
			if s == "running" {
				return true
			}
		}

		return false
	}, 2*time.Second, 10*time.Millisecond, "running status must be reported")
}

func TestRun_PATProvider(t *testing.T) {
	var (
		createdEnv     []string
		createdMounts  []mount.Mount
		secretsOnDisk  string
		secretsSource  string
		secretsRdOnly  bool
		secretsMntType mount.Type
	)

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		createdEnv = cfg.Env
		createdMounts = hc.Mounts

		// Capture the file contents now — the cleanup defer will
		// unlink it before this test gets to make assertions.
		for _, m := range hc.Mounts {
			if m.Target == secretsMountTarget {
				secretsSource = m.Source
				secretsRdOnly = m.ReadOnly
				secretsMntType = m.Type
				b, err := os.ReadFile(m.Source)
				require.NoError(t, err, "reading on-host secrets file during ContainerCreate")

				secretsOnDisk = string(b)
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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	// PAT token must NOT be in container env.
	for _, e := range createdEnv {
		assert.False(t, strings.HasPrefix(e, "CM_GIT_TOKEN="),
			"CM_GIT_TOKEN must not be in container env; it is delivered via /run/cm-secrets/env")
	}

	// The per-container secrets mount must exist, be a read-only bind.
	var sawSecrets bool

	for _, m := range createdMounts {
		if m.Target == secretsMountTarget {
			sawSecrets = true
		}
	}

	require.True(t, sawSecrets, "secrets mount must be present")
	assert.Equal(t, mount.TypeBind, secretsMntType)
	assert.True(t, secretsRdOnly, "secrets mount must be read-only")
	assert.NotEmpty(t, secretsSource, "secrets mount must have a source path")

	// The file content (captured before cleanup) must contain the PAT token.
	assert.Contains(t, secretsOnDisk, "CM_GIT_TOKEN='ghp_test_pat'",
		"secrets file must contain the PAT token")

	// And it must be deleted by the cleanup path.
	_, err := os.Stat(secretsSource)
	assert.True(t, os.IsNotExist(err),
		"secrets file must be removed after the container exits; stat err=%v", err)
}

// TestStartContainer_SecretsWrittenToTmpfsBindMount verifies the happy-path
// wiring: a secrets file is written, bind-mounted read-only at
// /run/cm-secrets/env, and contains the expected keys with values that are
// safe to embed inside single-quoted shell strings.
func TestStartContainer_SecretsWrittenToTmpfsBindMount(t *testing.T) {
	var (
		createdEnv    []string
		createdMounts []mount.Mount
		secretsText   string
	)

	mock := &MockDockerClient{
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
		ContainerCreateFn: func(_ context.Context, cfg *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			createdEnv = cfg.Env
			createdMounts = hc.Mounts

			for _, m := range hc.Mounts {
				if m.Target == secretsMountTarget {
					b, err := os.ReadFile(m.Source)
					require.NoError(t, err)

					secretsText = string(b)
				}
			}

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

	cfg := testConfig()
	cfg.AnthropicAPIKey = "sk-weird'quote"

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	payload := testPayload()
	payload.MCPAPIKey = "mcp-secret"
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	// No secrets in Env.
	for _, e := range createdEnv {
		assert.False(t, strings.HasPrefix(e, "CM_GIT_TOKEN="))
		assert.False(t, strings.HasPrefix(e, "CM_MCP_API_KEY="))
		assert.False(t, strings.HasPrefix(e, "ANTHROPIC_API_KEY="))
		assert.False(t, strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN="))
	}

	// Secrets mount present and read-only.
	var found bool

	for _, m := range createdMounts {
		if m.Target == secretsMountTarget {
			found = true

			assert.Equal(t, mount.TypeBind, m.Type)
			assert.True(t, m.ReadOnly)
		}
	}

	require.True(t, found, "secrets mount must be present")

	// File content has the expected keys and the quote in the API key is
	// escaped as \'.
	assert.Contains(t, secretsText, "CM_GIT_TOKEN='ghs_test_token'")
	assert.Contains(t, secretsText, "CM_MCP_API_KEY='mcp-secret'")
	assert.Contains(t, secretsText, `ANTHROPIC_API_KEY='sk-weird'\''quote'`,
		"single quote in secret must be escaped as '\\''")
}

// TestSecretsFileCleanedUpOnContainerExit verifies the host-side secrets
// file is unlinked by the waitAndCleanup path.
func TestSecretsFileCleanedUpOnContainerExit(t *testing.T) {
	var secretsPath string

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, _ *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		for _, m := range hc.Mounts {
			if m.Target == secretsMountTarget {
				secretsPath = m.Source
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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	require.NotEmpty(t, secretsPath, "expected secrets mount source path")

	_, err := os.Stat(secretsPath)
	assert.True(t, os.IsNotExist(err),
		"secrets file must be gone after waitAndCleanup; stat err=%v", err)
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
		// goroutine (CTXRUN-059 H23) concurrently with the failed
		// callback, so plain-slice append would race under -race.
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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

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
	mock := successfulMock()
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
	mock := successfulMock()
	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())

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
	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())

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

	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())

	err := mgr.CleanupOrphans(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"orphan-1"}, removedIDs,
		"only the untracked container must be removed; tracked live-1 must survive")
}

// TestCleanupOrphans_PartialFailure verifies that a per-container Stop failure
// does not short-circuit cleanup of the remaining orphans and that the
// returned error wraps the failure via errors.Join so callers can still see
// which container failed. See CTXRUN-050 (M20).
func TestCleanupOrphans_PartialFailure(t *testing.T) {
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
			{ID: "orphan-2-bad", Labels: map[string]string{LabelCardID: "A-002", LabelProject: "proj"}},
			{ID: "orphan-3", Labels: map[string]string{LabelCardID: "A-003", LabelProject: "proj"}},
		}, nil
	}
	mock.ContainerStopFn = func(_ context.Context, id string, _ container.StopOptions) error {
		muStop.Lock()

		stopIDs = append(stopIDs, id)
		muStop.Unlock()

		if id == "orphan-2-bad" {
			return fmt.Errorf("docker stop failed for %s", id)
		}

		return nil
	}
	mock.ContainerRemoveFn = func(_ context.Context, id string, _ container.RemoveOptions) error {
		muRemove.Lock()

		removeIDs = append(removeIDs, id)
		muRemove.Unlock()

		return nil
	}

	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())

	err := mgr.CleanupOrphans(context.Background())
	require.Error(t, err, "CleanupOrphans must return the joined per-container failures")
	assert.Contains(t, err.Error(), "orphan-2-bad",
		"joined error must identify the failing container")
	assert.Contains(t, err.Error(), "stop orphan",
		"joined error must describe which operation failed")

	// All three containers must have been attempted regardless of the middle
	// failure: the bug this test guards against was aborting on first error.
	assert.ElementsMatch(t, []string{"orphan-1", "orphan-2-bad", "orphan-3"}, stopIDs,
		"every orphan must have ContainerStop attempted")
	assert.ElementsMatch(t, []string{"orphan-1", "orphan-2-bad", "orphan-3"}, removeIDs,
		"every orphan must have ContainerRemove attempted even after a Stop failure")
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

// buildAuthTestManager creates a manager with a mock that captures env and
// mounts, runs a container, and returns the captured values. The cfg argument
// controls auth. The returned `mountTargets` contains all Mount.Target paths
// for tests that only care about presence/absence. `secretsContent` is the
// body of the per-container secrets file captured at ContainerCreate time
// (the file is deleted during cleanup before this function returns).
func buildAuthTestManager(t *testing.T, cfg *config.Config) (env []string, mountTargets []string, secretsContent string) {
	t.Helper()

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, c *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		env = c.Env

		for _, m := range hc.Mounts {
			mountTargets = append(mountTargets, m.Target)

			if m.Target == secretsMountTarget {
				b, err := os.ReadFile(m.Source)
				require.NoError(t, err, "reading secrets file at create time")

				secretsContent = string(b)
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

	return env, mountTargets, secretsContent
}

// assertNoAuthEnv fails if any of the secret env vars appear in env.
func assertNoAuthEnv(t *testing.T, env []string) {
	t.Helper()

	for _, e := range env {
		assert.False(t, strings.HasPrefix(e, "ANTHROPIC_API_KEY="), "ANTHROPIC_API_KEY must not be in Env")
		assert.False(t, strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN="), "CLAUDE_CODE_OAUTH_TOKEN must not be in Env")
		assert.False(t, strings.HasPrefix(e, "CM_GIT_TOKEN="), "CM_GIT_TOKEN must not be in Env")
		assert.False(t, strings.HasPrefix(e, "CM_MCP_API_KEY="), "CM_MCP_API_KEY must not be in Env")
	}
}

// TestAuthPriority_ClaudeAuthDir verifies that when ClaudeAuthDir is set alongside
// oauth token and API key, only the directory mount is used — no auth env vars,
// and no Claude token in the secrets file.
func TestAuthPriority_ClaudeAuthDir(t *testing.T) {
	// Create a temporary directory to use as ClaudeAuthDir so validation passes.
	dir := t.TempDir()

	cfg := testConfig()
	cfg.ClaudeAuthDir = dir
	cfg.ClaudeOAuthToken = "oauth-tok"
	cfg.AnthropicAPIKey = "sk-api"

	env, targets, secrets := buildAuthTestManager(t, cfg)

	// The claude-auth mount is present; so is the secrets mount (for CM_GIT_TOKEN).
	assert.Contains(t, targets, "/claude-auth", "claude-auth mount should be present")
	assert.Contains(t, targets, secretsMountTarget, "secrets mount should be present")

	assertNoAuthEnv(t, env)

	// When ClaudeAuthDir takes priority, the Claude auth tokens must NOT
	// appear in the secrets file either — the container reads them from
	// the mounted $HOME/.claude directory.
	assert.NotContains(t, secrets, "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN must not be in secrets file when ClaudeAuthDir is highest priority")
	assert.NotContains(t, secrets, "ANTHROPIC_API_KEY",
		"ANTHROPIC_API_KEY must not be in secrets file when ClaudeAuthDir is highest priority")
}

// TestAuthPriority_OAuthToken verifies that when ClaudeAuthDir is unset but
// ClaudeOAuthToken and AnthropicAPIKey are both set, only the OAuth token
// appears in the secrets file.
func TestAuthPriority_OAuthToken(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = "oauth-tok"
	cfg.AnthropicAPIKey = "sk-api"

	env, targets, secrets := buildAuthTestManager(t, cfg)

	// The only mount must be the secrets tmpfs bind mount.
	assert.Equal(t, []string{secretsMountTarget}, targets,
		"only the secrets mount should be present when using oauth token")

	assertNoAuthEnv(t, env)

	assert.Contains(t, secrets, "CLAUDE_CODE_OAUTH_TOKEN='oauth-tok'")
	assert.NotContains(t, secrets, "ANTHROPIC_API_KEY",
		"ANTHROPIC_API_KEY must not appear in secrets file when OAuth token takes priority")
}

// TestAuthPriority_APIKeyOnly verifies that when only AnthropicAPIKey is set,
// only ANTHROPIC_API_KEY is written to the secrets file.
func TestAuthPriority_APIKeyOnly(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = ""
	cfg.AnthropicAPIKey = "sk-only"

	env, targets, secrets := buildAuthTestManager(t, cfg)

	assert.Equal(t, []string{secretsMountTarget}, targets,
		"only the secrets mount should be present when using API key only")
	assertNoAuthEnv(t, env)

	assert.Contains(t, secrets, "ANTHROPIC_API_KEY='sk-only'")
	assert.NotContains(t, secrets, "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN must not appear in secrets file when only API key is configured")
}

// TestAuthPriority_OAuthTokenOnly verifies that when only ClaudeOAuthToken is
// set, only CLAUDE_CODE_OAUTH_TOKEN is written to the secrets file.
func TestAuthPriority_OAuthTokenOnly(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = "oauth-only"
	cfg.AnthropicAPIKey = ""

	env, targets, secrets := buildAuthTestManager(t, cfg)

	assert.Equal(t, []string{secretsMountTarget}, targets,
		"only the secrets mount should be present when using OAuth token only")
	assertNoAuthEnv(t, env)

	assert.Contains(t, secrets, "CLAUDE_CODE_OAUTH_TOKEN='oauth-only'")
	assert.NotContains(t, secrets, "ANTHROPIC_API_KEY",
		"ANTHROPIC_API_KEY must not appear in secrets file when only OAuth token is configured")
}

// TestClaudeSettings_EnvVarPresentWhenSet verifies that CM_CLAUDE_SETTINGS is
// injected into the container env when cfg.ClaudeSettings is non-empty.
func TestClaudeSettings_EnvVarPresentWhenSet(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeSettings = `{"enabledTools":["Bash","Edit"]}`

	env, _, _ := buildAuthTestManager(t, cfg)

	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"enabledTools":["Bash","Edit"]}`)
}

// TestClaudeSettings_EnvVarAbsentWhenEmpty verifies that CM_CLAUDE_SETTINGS is
// not injected when cfg.ClaudeSettings is empty.
func TestClaudeSettings_EnvVarAbsentWhenEmpty(t *testing.T) {
	cfg := testConfig()
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

	cfg := testConfig()
	cfg.ClaudeAuthDir = dir
	cfg.ClaudeSettings = `{"model":"claude-sonnet-4-6"}`

	env, targets, _ := buildAuthTestManager(t, cfg)

	assert.Contains(t, targets, "/claude-auth", "claude-auth mount should be present")
	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"model":"claude-sonnet-4-6"}`)
}

// TestClaudeSettings_WithOAuthToken verifies that CM_CLAUDE_SETTINGS is
// set alongside the OAuth token being written to the secrets file.
func TestClaudeSettings_WithOAuthToken(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = "oauth-tok"
	cfg.AnthropicAPIKey = ""
	cfg.ClaudeSettings = `{"theme":"dark"}`

	env, _, secrets := buildAuthTestManager(t, cfg)

	assertNoAuthEnv(t, env)
	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"theme":"dark"}`)
	assert.Contains(t, secrets, "CLAUDE_CODE_OAUTH_TOKEN='oauth-tok'")
}

// TestClaudeSettings_WithAPIKey verifies that CM_CLAUDE_SETTINGS is set
// alongside the Anthropic API key being written to the secrets file.
func TestClaudeSettings_WithAPIKey(t *testing.T) {
	cfg := testConfig()
	cfg.ClaudeAuthDir = ""
	cfg.ClaudeOAuthToken = ""
	cfg.AnthropicAPIKey = "sk-test-key"
	cfg.ClaudeSettings = `{"permissions":{"allow":["Bash"]}}`

	env, _, secrets := buildAuthTestManager(t, cfg)

	assertNoAuthEnv(t, env)
	assert.Contains(t, env, `CM_CLAUDE_SETTINGS={"permissions":{"allow":["Bash"]}}`)
	assert.Contains(t, secrets, "ANTHROPIC_API_KEY='sk-test-key'")
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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

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
// could fire. CTXRUN-040 (C11).
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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

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

// TestWaitAndCleanup_ParentContextCanceled verifies that when the parent ctx
// is canceled mid-wait, the manager takes the kill path: the container is
// stopped and removed, the tracker slot is freed, a "container canceled"
// system event is emitted, AND reportFailure is invoked with a "killed by
// operator" message so ContextMatrix can transition the card out of
// `running`. See CTXRUN-050.
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
	mgr := NewManager(mock, tr, cb, tp, b.Broadcaster(), testConfig(), testLogger())

	payload := testPayload()

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  cancel,
	}))

	mgr.Run(ctx, payload)

	// Let the container get started and enter waitAndCleanup, then cancel.
	time.Sleep(50 * time.Millisecond)
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

	// CTXRUN-050: reportFailure must fire via a detached context so CM sees
	// the terminal status even though the parent ctx is already cancelled.
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
// getting stuck in `running`. See CTXRUN-050 (C12).
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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

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

	cfg := testConfig()
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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

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

	mgr := NewManager(mock, tracker.New(), nil, nil, nil, testConfig(), testLogger())
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

	mgr := NewManager(mock, tracker.New(), nil, nil, nil, testConfig(), testLogger())
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

	mgr := NewManager(mock, tracker.New(), nil, nil, nil, testConfig(), testLogger())

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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

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

// TestStreamLogs_StderrScannerPanic asserts that CTXRUN-040's per-goroutine
// recover inside streamLogs isolates a panic in one of the three child
// goroutines (here: stdcopy, which calls Read on the injected panicReader).
// Before the fix a panic in these goroutines unwound the entire runner
// process; now the runner must continue, the panic is surfaced as a
// `system` LogEntry so operators see it, and waitAndCleanup must still run
// to completion so the tracker entry is removed.
func TestStreamLogs_StderrScannerPanic(t *testing.T) {
	mock := successfulMock()
	mock.ContainerLogsFn = func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
		// panicReader's Read panics on first call. stdcopy.StdCopy will
		// panic trying to read from it; the recover() installed by
		// CTXRUN-040 must catch it and emit a system event.
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

	mgr := NewManager(mock, tr, cb, tp, broadcaster.inner, testConfig(), testLogger())

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
	panic("synthetic read panic for CTXRUN-040")
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
// falling back to PullAlways. See CTXRUN-051.
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

	mgr := NewManager(mock, tracker.New(), nil, nil, nil, cfg, testLogger())

	err := mgr.pullImage(context.Background(), "test-image:latest")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image_pull_policy is unset")
	assert.Contains(t, err.Error(), "programming error")
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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

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

// TestIdleWatchdog_KillsOnSilence verifies the CTXRUN-058 idle-output watchdog
// kills the container and emits a "idle timeout" system event when no output
// has been observed for longer than IdleOutputTimeout. Exercises the goroutine
// directly (rather than via streamLogs + a fake stream) so the test is
// deterministic and does not depend on docker multiplexed framing.
func TestIdleWatchdog_KillsOnSilence(t *testing.T) {
	cfg := testConfig()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	cfg := testConfig()
	cfg.IdleOutputTimeout = 50 * time.Millisecond
	// Shrink the poll tick so the watchdog reacts promptly inside the test.
	cfg.IdleWatchdogInterval = 5 * time.Millisecond

	mock := successfulMock()

	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, nil, nil, cfg, testLogger())

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

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

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
	cfg := testConfig()
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

	var cancelled atomic.Bool

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
		Cancel:  func() { cancelled.Store(true) },
	}))

	cb := callback.NewClient(cbSrv.URL, "test-secret-key-that-is-long-enough", testLogger())
	tp := testTokenProvider(t)

	mgr := NewManager(mock, tr, cb, tp, nil, cfg, testLogger())

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	assert.False(t, cancelled.Load(), "watchdog must not fire when IdleOutputTimeout is 0")
}

// TestPruneImages_CallsDockerWithCorrectFilters verifies PruneImages forwards
// the CTXRUN-058 filters (dangling=true, until=24h) to dockerd and surfaces
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
	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())

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
	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())

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
// CTXRUN-059 (H24).
func TestBuildExtraHosts_DNSTimeout(t *testing.T) {
	mock := successfulMock()
	tr := tracker.New()
	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())
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
	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())

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
// sleeps 500ms before responding; the whole Run must still complete well
// under 1s because the callback is fired on a detached goroutine. CTXRUN-059
// (H23).
func TestRun_RunningCallbackAsync(t *testing.T) {
	var runningSeen atomic.Bool

	cbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			RunnerStatus string `json:"runner_status"`
		}

		_ = json.Unmarshal(body, &req)

		if req.RunnerStatus == "running" {
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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

	payload := testPayload()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	start := time.Now()

	mgr.Run(context.Background(), payload)
	mgr.Wait()

	elapsed := time.Since(start)

	// The container's Wait path returns immediately via the mock, so the
	// whole Run should complete in well under 1s. If the running callback
	// were synchronous, the 500ms CM sleep would push us past that mark.
	assert.Less(t, elapsed, 500*time.Millisecond,
		"Run must not block on the running-status callback; got %s", elapsed)

	// Give the async callback a chance to land so we don't leak a goroutine
	// into subsequent tests. We don't assert runningSeen inside the <500ms
	// window because the goroutine may not have raced ahead of mgr.Wait().
	require.Eventually(t, runningSeen.Load, 2*time.Second, 10*time.Millisecond,
		"running callback must eventually fire on its own goroutine")
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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

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

	mgr := NewManager(mock, tr, cb, tp, nil, testConfig(), testLogger())

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

	mgr := NewManager(mock, tr, nil, nil, nil, testConfig(), testLogger())

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

	mgr := NewManager(mock, tracker.New(), nil, nil, nil, testConfig(), testLogger())

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

	mgr := NewManager(mock, tracker.New(), nil, nil, nil, testConfig(), testLogger())

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

	mgr := NewManager(mock, tracker.New(), nil, nil, nil, testConfig(), testLogger())

	n, err := mgr.ForceRemoveByLabels(context.Background(), "proj", "A-001")
	require.NoError(t, err)
	assert.Zero(t, n)
}

// TestForceRemoveByLabels_RequiresProjectAndCard guards against an empty
// call that would otherwise list every runner-managed container and remove
// them all — which would happen if the label filter were silently ignored
// by Docker when the value is empty.
func TestForceRemoveByLabels_RequiresProjectAndCard(t *testing.T) {
	mgr := NewManager(nil, tracker.New(), nil, nil, nil, testConfig(), testLogger())

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
		cfg := testConfig()
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
		cfg := testConfig()
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
		cfg := testConfig()
		cfg.TaskSkillsDir = "/x"

		payload := testPayload()
		skills := []string{"go-development", "docs"}
		payload.TaskSkills = &skills

		_, env := captureCreateArgs(t, cfg, payload)

		assert.Contains(t, env, "CM_TASK_SKILLS_SET=1")
		assert.Contains(t, env, "CM_TASK_SKILLS=go-development,docs")
	})

	t.Run("empty list still emits SET=1 with empty value", func(t *testing.T) {
		cfg := testConfig()
		cfg.TaskSkillsDir = "/x"

		payload := testPayload()
		skills := []string{}
		payload.TaskSkills = &skills

		_, env := captureCreateArgs(t, cfg, payload)

		assert.Contains(t, env, "CM_TASK_SKILLS_SET=1")
		assert.Contains(t, env, "CM_TASK_SKILLS=")
	})

	t.Run("nil TaskSkills emits no env vars even with mount", func(t *testing.T) {
		cfg := testConfig()
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
		cfg := testConfig()
		cfg.WorkerExtraEnv = nil

		env := captureEnv(t, cfg)
		for _, e := range env {
			assert.False(t, strings.HasPrefix(e, "GIT_SSL_NO_VERIFY="),
				"no extra env should be present when WorkerExtraEnv is nil")
		}
	})

	t.Run("entries appear in env list", func(t *testing.T) {
		cfg := testConfig()
		cfg.WorkerExtraEnv = map[string]string{
			"GIT_SSL_NO_VERIFY":   "1",
			"NPM_CONFIG_REGISTRY": "https://npm.internal/",
		}

		env := captureEnv(t, cfg)
		assert.Contains(t, env, "GIT_SSL_NO_VERIFY=1")
		assert.Contains(t, env, "NPM_CONFIG_REGISTRY=https://npm.internal/")
	})

	t.Run("entries sorted for deterministic output", func(t *testing.T) {
		cfg := testConfig()
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
		cfg := testConfig()
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
		cfg := testConfig()
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
		cfg := testConfig()
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
