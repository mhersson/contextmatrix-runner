package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/container"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// chatFakeRunner is a ContainerRunner double for the /chat/* handler tests.
type chatFakeRunner struct {
	// startChatFn is called by StartChat. Return an empty string + nil for success.
	startChatFn func(ctx context.Context, opts container.StartChatOpts) (string, error)
	// attachChatStdinFn is called by AttachChatStdin (post-AddChat).
	attachChatStdinFn func(ctx context.Context, sessionID, containerID string) error
	// stopFn is called by Stop (rollback path).
	stopFn func(ctx context.Context, containerID string) error
	// workerImage is returned by WorkerImage.
	workerImage string
}

func (f *chatFakeRunner) Run(_ context.Context, _ container.RunConfig) {}

func (f *chatFakeRunner) Kill(_, _ string) error { return nil }

func (f *chatFakeRunner) ListManaged(_ context.Context) ([]container.ManagedContainer, error) {
	return nil, nil
}

func (f *chatFakeRunner) ForceRemoveByLabels(_ context.Context, _, _ string) (int, error) {
	return 0, nil
}

func (f *chatFakeRunner) StartChat(ctx context.Context, opts container.StartChatOpts) (string, error) {
	if f.startChatFn != nil {
		return f.startChatFn(ctx, opts)
	}

	return "container-abc123", nil
}

func (f *chatFakeRunner) Stop(ctx context.Context, containerID string) error {
	if f.stopFn != nil {
		return f.stopFn(ctx, containerID)
	}

	return nil
}

func (f *chatFakeRunner) WorkerImage() string { return f.workerImage }

func (f *chatFakeRunner) BuildChatAuthEnv(_ context.Context) map[string]string { return nil }

func (f *chatFakeRunner) AttachChatStdin(ctx context.Context, sessionID, containerID string) error {
	if f.attachChatStdinFn != nil {
		return f.attachChatStdinFn(ctx, sessionID, containerID)
	}

	return nil
}

func (f *chatFakeRunner) StreamChatLogs(_ context.Context, _, _, _ string) {}

// TestChatStart_Success verifies that a valid /chat/start request returns 202
// and records the container in the tracker.
func TestChatStart_Success(t *testing.T) {
	tr := tracker.New()
	fake := &chatFakeRunner{workerImage: "worker:latest"}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/start", ChatStartPayload{
		SessionID: "sess-001",
		Project:   "my-proj",
	})
	h.hmacAuth(h.handleChatStart)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, true, resp["ok"])
	assert.Equal(t, "container-abc123", resp["container_id"])

	// Tracker must have the chat entry.
	assert.True(t, tr.HasChat("sess-001"))
}

// TestChatStart_Conflict_WhenAlreadyTracked verifies that a duplicate
// /chat/start returns 409 when the session already has a container.
func TestChatStart_Conflict_WhenAlreadyTracked(t *testing.T) {
	tr := tracker.New()
	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{SessionID: "sess-dup"}))

	fake := &chatFakeRunner{}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/start", ChatStartPayload{SessionID: "sess-dup"})
	h.hmacAuth(h.handleChatStart)(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Equal(t, CodeConflict, resp.Code)
}

// TestChatStart_LimitReached_429 verifies that /chat/start refuses to start
// a new chat container when the runner's shared concurrency limit is reached,
// and that StartChat is NOT called in the rejection path.
func TestChatStart_LimitReached_429(t *testing.T) {
	tr := tracker.New()
	// Pre-fill the tracker to the limit using card-mode entries; the chat
	// path must share the same total capacity.
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "C-1", Project: "p"}))
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "C-2", Project: "p"}))

	fake := &chatFakeRunner{
		startChatFn: func(_ context.Context, _ container.StartChatOpts) (string, error) {
			t.Fatalf("StartChat must not be called when limit is reached")

			return "", nil
		},
	}
	// limit = 2; tracker already has 2 entries → next chat must 429.
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 2, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/start", ChatStartPayload{SessionID: "sess-cap"})
	h.hmacAuth(h.handleChatStart)(w, req)

	require.Equal(t, http.StatusTooManyRequests, w.Code)

	var resp ErrorResponse

	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Equal(t, CodeLimitReached, resp.Code)
	assert.False(t, tr.HasChat("sess-cap"))
}

// TestChatStart_MissingSessionID_400 verifies that an empty session_id returns
// 400 Bad Request before any container is started.
func TestChatStart_MissingSessionID_400(t *testing.T) {
	tr := tracker.New()
	fake := &chatFakeRunner{
		startChatFn: func(_ context.Context, _ container.StartChatOpts) (string, error) {
			panic("StartChat must not be called when session_id is missing")
		},
	}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/start", ChatStartPayload{}) // empty SessionID
	h.hmacAuth(h.handleChatStart)(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
}

// TestChatStart_RollbackOnTrackerFailure verifies that if AddChat fails (race
// condition: another caller added the same session_id between HasChat and
// AddChat), the handler calls Stop to tear down the container it started and
// returns 409.
func TestChatStart_RollbackOnTrackerFailure(t *testing.T) {
	tr := tracker.New()

	// Pre-seed so that AddChat will fail with ErrAlreadyTracked.
	// We do NOT pre-seed before the handler's HasChat check to avoid the 409
	// at the HasChat gate — we seed it after the handler starts.
	// The simplest approach: seed after the handler's HasChat check but before
	// AddChat. In practice we cannot inject this race in a unit test, so we
	// instead make StartChat add the entry to trigger a genuine AddChat failure.
	var stopCalled bool

	fake := &chatFakeRunner{
		startChatFn: func(_ context.Context, _ container.StartChatOpts) (string, error) {
			// After StartChat succeeds, seed the tracker so AddChat fails.
			_ = tr.AddChat(&tracker.ContainerInfo{SessionID: "sess-race"})

			return "container-race", nil
		},
		stopFn: func(_ context.Context, containerID string) error {
			assert.Equal(t, "container-race", containerID)

			stopCalled = true

			return nil
		},
	}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/start", ChatStartPayload{SessionID: "sess-race"})
	h.hmacAuth(h.handleChatStart)(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.True(t, stopCalled, "Stop must be called to roll back the container on tracker failure")
}

// TestChatEnd_Success verifies that /chat/end closes stdin, stops the
// container, removes the tracker entry, and returns 200. A second call
// returns 404 because the entry is gone.
func TestChatEnd_Success(t *testing.T) {
	tr := tracker.New()
	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{
		SessionID:   "sess-end",
		ContainerID: "ctr-end-1",
	}))

	// Attach a stdin writer so CloseStdinChat succeeds.
	wc := &nopWriteCloser{}
	tr.SetStdinChat("sess-end", wc, nil)

	var stoppedID string

	fake := &chatFakeRunner{
		stopFn: func(_ context.Context, containerID string) error {
			stoppedID = containerID

			return nil
		},
	}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/end", ChatEndPayload{SessionID: "sess-end"})
	h.hmacAuth(h.handleChatEnd)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp SuccessResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)

	assert.Equal(t, "ctr-end-1", stoppedID, "Stop must be called with the tracked container_id")
	assert.False(t, tr.HasChat("sess-end"), "tracker entry must be removed after /chat/end success")

	// Second call: tracker entry is gone → 404.
	w2 := httptest.NewRecorder()
	req2 := signedRequest(t, "/chat/end", ChatEndPayload{SessionID: "sess-end"})
	h.hmacAuth(h.handleChatEnd)(w2, req2)
	assert.Equal(t, http.StatusNotFound, w2.Code)
}

// TestChatEnd_StopsEvenWhenStdinAlreadyClosed verifies that ErrNoStdinAttached
// from CloseStdinChat is non-fatal: the handler still issues Stop and removes
// the tracker entry. This covers the case where stdin was never attached or
// was closed by a prior failed /chat/end.
func TestChatEnd_StopsEvenWhenStdinAlreadyClosed(t *testing.T) {
	tr := tracker.New()
	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{
		SessionID:   "sess-no-stdin",
		ContainerID: "ctr-no-stdin",
	}))
	// No SetStdinChat — CloseStdinChat will return ErrNoStdinAttached.

	stopCalled := false

	fake := &chatFakeRunner{
		stopFn: func(_ context.Context, _ string) error {
			stopCalled = true

			return nil
		},
	}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/end", ChatEndPayload{SessionID: "sess-no-stdin"})
	h.hmacAuth(h.handleChatEnd)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, stopCalled, "Stop must be called even when stdin was not attached")
	assert.False(t, tr.HasChat("sess-no-stdin"))
}

// TestChatEnd_NotFound_404 verifies that /chat/end returns 404 when no
// container is tracked for the given session_id.
func TestChatEnd_NotFound_404(t *testing.T) {
	fake := &chatFakeRunner{}
	h := NewHandler(fake, tracker.New(), nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/end", ChatEndPayload{SessionID: "sess-unknown"})
	h.hmacAuth(h.handleChatEnd)(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Equal(t, CodeNotFound, resp.Code)
}

// TestChatStart_Draining_503 verifies that /chat/start refuses new work during
// graceful shutdown so a chat container is not spawned just to be SIGKILLed
// 100ms later by the shutdown sequence.
func TestChatStart_Draining_503(t *testing.T) {
	hs := NewHealthState()
	hs.Draining.Store(true)
	h := NewHandler(&chatFakeRunner{}, tracker.New(), nil, nil, testAPIKey, 10, testMCPURL, nil, 0, hs)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/start", ChatStartPayload{SessionID: "sess-drain"})
	h.hmacAuth(h.handleChatStart)(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, CodeDraining, resp.Code)
}

// TestChatEnd_Draining_503 mirrors the /chat/start drain check. End is also
// gated so the shutdown sequence's own teardown isn't fighting with concurrent
// /chat/end callers.
func TestChatEnd_Draining_503(t *testing.T) {
	hs := NewHealthState()
	hs.Draining.Store(true)
	h := NewHandler(&chatFakeRunner{}, tracker.New(), nil, nil, testAPIKey, 10, testMCPURL, nil, 0, hs)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/end", ChatEndPayload{SessionID: "sess-drain"})
	h.hmacAuth(h.handleChatEnd)(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, CodeDraining, resp.Code)
}

// nopWriteCloser is a no-op WriteCloser used to attach an interactive stdin
// to a tracker entry in tests.
type nopWriteCloser struct{}

func (n *nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (n *nopWriteCloser) Close() error                { return nil }
