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
func testTokenProvider(t *testing.T) *github.TokenProvider {
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

	mgr := NewManager(mock, tr, cb, tp, testConfig(), testLogger())

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
	assert.Contains(t, createdEnv, "CM_REPO_URL=git@github.com:org/repo.git")
	assert.Contains(t, createdEnv, "ANTHROPIC_API_KEY=sk-test")

	// Verify labels.
	assert.Equal(t, "true", createdLabels[LabelRunner])
	assert.Equal(t, "PROJ-042", createdLabels[LabelCardID])
	assert.Equal(t, "my-project", createdLabels[LabelProject])

	// Should have reported "running".
	assert.Contains(t, reportedStatuses, "running")
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

	mgr := NewManager(mock, tr, cb, tp, testConfig(), testLogger())

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

	mgr := NewManager(mock, tr, cb, tp, testConfig(), testLogger())

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
	mgr := NewManager(mock, tr, cb, tp, cfg, testLogger())

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
	mgr := NewManager(mock, tr, nil, nil, testConfig(), testLogger())

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
	mgr := NewManager(mock, tr, nil, nil, testConfig(), testLogger())

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
	mgr := NewManager(mock, tr, nil, nil, testConfig(), testLogger())

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

	mgr := NewManager(mock, tr, cb, tp, testConfig(), testLogger())

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
