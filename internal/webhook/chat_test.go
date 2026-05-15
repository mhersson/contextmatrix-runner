package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

	// tracker is consulted by the fake's WaitAndCleanupChat to drop the entry
	// when exitContainer fires. Optional; only tests that exercise the
	// lifecycle-ownership transfer need to set it.
	tracker TrackerService

	// Captured state for test assertions.
	mu                   sync.Mutex
	LastStartOpts        container.StartChatOpts
	PrimingWrites        [][]byte
	WaitCleanupSessions  []string
	DeleteCleanupCalled  []string
	waitExitChans        map[string]chan struct{}
	waitGoroutinesActive sync.WaitGroup
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
	f.mu.Lock()
	f.LastStartOpts = opts
	f.mu.Unlock()

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

// WaitAndCleanupChat models the production hook: spawn a goroutine that waits
// for an external "container exited" signal and then removes the tracker
// entry. Tests trigger the exit via exitContainer.
func (f *chatFakeRunner) WaitAndCleanupChat(sessionID, containerID, _ string) {
	f.mu.Lock()
	f.WaitCleanupSessions = append(f.WaitCleanupSessions, sessionID)

	if f.waitExitChans == nil {
		f.waitExitChans = map[string]chan struct{}{}
	}

	ch, ok := f.waitExitChans[containerID]
	if !ok {
		ch = make(chan struct{})
		f.waitExitChans[containerID] = ch
	}

	f.mu.Unlock()

	f.waitGoroutinesActive.Add(1)

	go func() {
		defer f.waitGoroutinesActive.Done()

		<-ch

		if f.tracker != nil {
			f.tracker.RemoveChat(sessionID)
		}
	}()
}

// DeleteChatCleanup records that the rollback path discarded the stashed
// cleanup state for containerID.
func (f *chatFakeRunner) DeleteChatCleanup(containerID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.DeleteCleanupCalled = append(f.DeleteCleanupCalled, containerID)
}

// exitContainer signals the wait goroutine that the container has exited so
// the fake's WaitAndCleanupChat can drop the tracker entry.
func (f *chatFakeRunner) exitContainer(containerID string) {
	f.mu.Lock()

	if f.waitExitChans == nil {
		f.waitExitChans = map[string]chan struct{}{}
	}

	ch, ok := f.waitExitChans[containerID]
	if !ok {
		ch = make(chan struct{})
		f.waitExitChans[containerID] = ch
	}

	f.mu.Unlock()

	close(ch)
}

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

// TestChatStart_TrackerRegisteredBeforeWaitGoroutine verifies that the wait-and-
// cleanup goroutine is owned by the webhook handler and only spawned AFTER the
// tracker entry has been registered. If the goroutine fired earlier (inside
// StartChat, as it used to) a microsecond-scale container exit could call
// RemoveChat before AddChat, leaving an orphan entry behind.
func TestChatStart_TrackerRegisteredBeforeWaitGoroutine(t *testing.T) {
	tr := tracker.New()
	fake := &chatFakeRunner{
		workerImage: "worker:latest",
		tracker:     tr,
		startChatFn: func(_ context.Context, _ container.StartChatOpts) (string, error) {
			return "container-xyz", nil
		},
	}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/start", ChatStartPayload{
		SessionID: "sess-1",
		Project:   "alpha",
		Model:     "claude-sonnet-4-5",
	})
	h.hmacAuth(h.handleChatStart)(w, req)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.True(t, tr.HasChat("sess-1"),
		"tracker entry must exist immediately after a successful /chat/start")

	fake.mu.Lock()
	require.Contains(t, fake.WaitCleanupSessions, "sess-1",
		"webhook handler must launch WaitAndCleanupChat after registering the tracker entry")
	fake.mu.Unlock()

	fake.exitContainer("container-xyz")

	require.Eventually(t, func() bool {
		return !tr.HasChat("sess-1")
	}, time.Second, 10*time.Millisecond, "tracker entry must be cleared after container exit")
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

// chatTrackerWrapper wraps a tracker and injects errors into WriteStdinChat
// for testing error paths. It also captures priming writes.
type chatTrackerWrapper struct {
	*tracker.Tracker
	mu            sync.Mutex
	PrimingWrites [][]byte
	ForceWriteErr bool
}

func (w *chatTrackerWrapper) WriteStdinChat(sessionID string, b []byte) error {
	w.mu.Lock()
	if w.ForceWriteErr {
		w.mu.Unlock()

		return errors.New("forced write failure")
	}

	w.PrimingWrites = append(w.PrimingWrites, append([]byte(nil), b...))
	w.mu.Unlock()

	return w.Tracker.WriteStdinChat(sessionID, b)
}

// TestChatStart_PrimingWrittenWhenResumeSet verifies that when Resume is
// provided, the handler writes a rehydration priming message to stdin and
// returns 202 Accepted.
func TestChatStart_PrimingWrittenWhenResumeSet(t *testing.T) {
	t.Parallel()

	tr := &chatTrackerWrapper{Tracker: tracker.New()}
	fake := &chatFakeRunner{
		workerImage: "worker:latest",
		attachChatStdinFn: func(_ context.Context, sessionID, _ string) error {
			// Simulate the real manager behavior: attach stdin to the tracker.
			tr.SetStdinChat(sessionID, &nopWriteCloser{}, nil)

			return nil
		},
	}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/start", ChatStartPayload{
		SessionID: "01SESS",
		Project:   "p",
		Resume: &ChatResumeContext{
			Turns: []ChatResumeTurn{
				{Seq: 1, Role: "user", Content: "hi"},
			},
		},
	})
	h.hmacAuth(h.handleChatStart)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)
	require.Len(t, tr.PrimingWrites, 1, "priming write must be captured")
	require.Contains(t, string(tr.PrimingWrites[0]), "chat_rehydration_complete",
		"priming message must contain chat_rehydration_complete")
}

// TestChatStart_NoPrimingWhenResumeNil verifies that when Resume is nil,
// no priming message is written and the handler returns 202 Accepted.
func TestChatStart_NoPrimingWhenResumeNil(t *testing.T) {
	t.Parallel()

	tr := &chatTrackerWrapper{Tracker: tracker.New()}
	fake := &chatFakeRunner{
		workerImage: "worker:latest",
		attachChatStdinFn: func(_ context.Context, sessionID, _ string) error {
			tr.SetStdinChat(sessionID, &nopWriteCloser{}, nil)

			return nil
		},
	}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/start", ChatStartPayload{
		SessionID: "01SESS",
		Project:   "p",
	})
	h.hmacAuth(h.handleChatStart)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)
	require.Empty(t, tr.PrimingWrites, "no priming writes when Resume is nil")
}

// TestChatStart_LargeBodyReturnsClearError verifies that a resume payload
// exceeding chatResumeMaxTurns is rejected with 400 (CodeInvalidField) and
// that the rejection happens inside validateChatResume rather than as a 401
// from the HMAC middleware truncating an oversized body.
//
// The two sub-cases bracket the new 600-turn cap. The at-cap case uses 1 KiB
// per turn so that 600 × ~1100-byte JSON-encoded turn stays well under
// hmacAuth's 1 MiB read window (chatResumeMaxContentBytes=4 KiB per turn
// would produce ~2.4 MiB at 600 turns, exceeding the HMAC window).
// transcript.Build's 40k-token budget means production payloads are far
// smaller than this worst-case test — the turn-count cap is the binding
// constraint, not bytes.
//
//   - exactly chatResumeMaxTurns turns at 1024 bytes each → body ≈ 660 KiB,
//     fits within hmacAuth's 1 MiB window → 202 Accepted.
//   - chatResumeMaxTurns+1 turns → validation rejects before HMAC can
//     truncate → 400 with CodeInvalidField.
func TestChatStart_LargeBodyReturnsClearError(t *testing.T) {
	t.Parallel()

	// atCapContentSize is chosen so that chatResumeMaxTurns turns of this size
	// produce a JSON body under hmacAuth's 1 MiB read window while still being
	// large enough to constitute a realistic stress payload. 1 KiB per turn ×
	// 600 turns ≈ 614 KiB of content; with JSON framing the total is ~660 KiB.
	const atCapContentSize = 1024

	tr := &chatTrackerWrapper{Tracker: tracker.New()}
	fake := &chatFakeRunner{
		workerImage: "worker:latest",
		attachChatStdinFn: func(_ context.Context, sessionID, _ string) error {
			tr.SetStdinChat(sessionID, &nopWriteCloser{}, nil)

			return nil
		},
	}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	// --- sub-case 1: exactly at the cap → should succeed. ---
	turns := make([]ChatResumeTurn, chatResumeMaxTurns)
	for i := range turns {
		turns[i] = ChatResumeTurn{
			Seq:     int64(i + 1),
			Role:    "user",
			Content: strings.Repeat("x", atCapContentSize),
		}
	}

	body, err := json.Marshal(ChatStartPayload{
		SessionID: "01SESS-AT-CAP",
		Project:   "p",
		Resume:    &ChatResumeContext{Turns: turns},
	})
	require.NoError(t, err)
	require.Less(t, len(body), 1<<20, "at-cap body must stay within hmacAuth's 1 MiB window")

	w := httptest.NewRecorder()
	h.hmacAuth(h.handleChatStart)(w, signedRequest(t, "/chat/start", ChatStartPayload{
		SessionID: "01SESS-AT-CAP",
		Project:   "p",
		Resume:    &ChatResumeContext{Turns: turns},
	}))
	require.Equal(t, http.StatusAccepted, w.Code, "at-cap payload must be accepted")

	// --- sub-case 2: one turn over the cap → clear 400, not 401. ---
	over := make([]ChatResumeTurn, chatResumeMaxTurns+1)
	for i := range over {
		over[i] = ChatResumeTurn{
			Seq:     int64(i + 1),
			Role:    "user",
			Content: strings.Repeat("x", atCapContentSize),
		}
	}

	w2 := httptest.NewRecorder()
	h.hmacAuth(h.handleChatStart)(w2, signedRequest(t, "/chat/start", ChatStartPayload{
		SessionID: "01SESS-OVER",
		Project:   "p",
		Resume:    &ChatResumeContext{Turns: over},
	}))
	require.Equal(t, http.StatusBadRequest, w2.Code,
		"over-cap payload must return 400, not 401 from HMAC truncation")

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w2.Body).Decode(&resp))
	assert.Equal(t, CodeInvalidField, resp.Code)
}

// TestChatStart_WriteStdinFailureDoesNotTearDown verifies that if writing
// the rehydration priming message fails (e.g., stdin write error), the handler
// still returns 202 Accepted and does NOT tear down the container.
func TestChatStart_WriteStdinFailureDoesNotTearDown(t *testing.T) {
	t.Parallel()

	tr := &chatTrackerWrapper{Tracker: tracker.New()}
	tr.ForceWriteErr = true
	fake := &chatFakeRunner{
		workerImage: "worker:latest",
		attachChatStdinFn: func(_ context.Context, sessionID, _ string) error {
			// Simulate the real manager behavior: attach stdin to the tracker.
			tr.SetStdinChat(sessionID, &nopWriteCloser{}, nil)

			return nil
		},
	}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/chat/start", ChatStartPayload{
		SessionID: "01SESS",
		Project:   "p",
		Resume: &ChatResumeContext{
			Turns: []ChatResumeTurn{
				{Seq: 1, Role: "user", Content: "hi"},
			},
		},
	})
	h.hmacAuth(h.handleChatStart)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code,
		"handler returns 202 even when WriteStdinChat fails")
	require.True(t, tr.HasChat("01SESS"),
		"container must still be tracked (write failure is non-fatal)")
}
