package webhook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/callback"
	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/container"
	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/mhersson/contextmatrix-runner/internal/streammsg"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

func testManager(tr *tracker.Tracker) *container.Manager {
	cfg := &config.Config{
		ContainerTimeout: "1h",
		ImagePullPolicy:  config.PullNever,
	}
	cfg.ParseContainerTimeout()

	return container.NewManager(nil, tr, nil, nil, nil, cfg, nil)
}

const testAPIKey = "test-api-key-that-is-at-least-32-chars"

const testMCPURL = "https://cm.example.com/mcp"

func signedRequest(t *testing.T, path string, payload any) *http.Request {
	t.Helper()

	body, err := json.Marshal(payload)
	require.NoError(t, err)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, path, body, ts)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)

	return req
}

// TestHmacAuth_ContentLengthExceedsCap pins Fix W1/W10: a request whose
// Content-Length header exceeds the 1 MiB read cap must be rejected with
// 413 (CodeTooLarge) BEFORE the io.LimitReader silently truncates it.
// Without the precheck, validators that allow oversized payloads (e.g.
// chatResumeMaxTurns × chatResumeMaxContentBytes can exceed 1 MiB) would
// produce a misleading 401 because the signature was computed over the
// full body but only the truncated body reaches verification.
func TestHmacAuth_ContentLengthExceedsCap(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}
	handler := h.hmacAuth(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called: 413 must short-circuit")
	})

	w := httptest.NewRecorder()
	// Body itself can be tiny; only the declared Content-Length is checked.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/trigger",
		strings.NewReader("{}"))
	req.ContentLength = int64(maxRequestBodyBytes) + 1
	req.Header.Set(cmhmac.SignatureHeader, "sha256=abc")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))

	handler(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code,
		"oversized Content-Length must produce 413, not 401")

	var resp ErrorResponse

	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, CodeTooLarge, resp.Code)
	assert.False(t, resp.OK)
}

// TestHmacAuth_ContentLengthAtCap verifies a Content-Length exactly at the
// cap is allowed (still rejected with 401 because the signature is fake,
// but NOT rejected with 413 first). Boundary case for W1.
func TestHmacAuth_ContentLengthAtCap(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}
	handler := h.hmacAuth(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called: bad signature must 401")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/trigger",
		strings.NewReader("{}"))
	req.ContentLength = int64(maxRequestBodyBytes) // exactly at the cap
	req.Header.Set(cmhmac.SignatureHeader, "sha256=abc")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))

	handler(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"Content-Length exactly at the cap must NOT be rejected as too large")
}

// TestHmacAuth_NoContentLengthOversizeStreamingBody pins the fallback path
// of Fix W1: a chunked-encoding request whose Content-Length is -1 cannot
// be prechecked, so the LimitReader inside hmacAuth still bounds the read.
// The signature was computed over the FULL body; the truncated body fails
// verification, producing 401. This is the documented fallback behavior:
// we cannot return 413 for streaming bodies because we'd have to read them
// first to know they were oversized.
func TestHmacAuth_NoContentLengthOversizeStreamingBody(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}
	handler := h.hmacAuth(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called: truncated body must 401")
	})

	// Build a request without Content-Length by reading from a body that
	// produces more bytes than the cap. httptest.NewRequest sets
	// ContentLength based on the io.Reader's length when known; using a
	// LimitReader+strings.NewReader gives us len-unknown stream.
	body := strings.Repeat("x", maxRequestBodyBytes+10)

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/trigger",
		io.NopCloser(strings.NewReader(body)))
	req.ContentLength = -1 // simulate unknown / chunked
	req.Header.Set(cmhmac.SignatureHeader, "sha256=abc")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))

	handler(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"unknown Content-Length must fall back to LimitReader → 401, not panic")
}

func TestHmacAuth_MissingSignature(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}
	handler := h.hmacAuth(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/test", strings.NewReader("{}"))
	handler(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestHmacAuth_MissingTimestamp(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}
	handler := h.hmacAuth(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/test", strings.NewReader("{}"))
	req.Header.Set(cmhmac.SignatureHeader, "sha256=abc")
	handler(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestHmacAuth_InvalidSignature(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}
	handler := h.hmacAuth(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/test", strings.NewReader("{}"))
	req.Header.Set(cmhmac.SignatureHeader, "sha256=invalid")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))
	handler(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestHmacAuth_ValidSignature(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}

	var called bool

	handler := h.hmacAuth(func(w http.ResponseWriter, _ *http.Request) {
		called = true

		w.WriteHeader(http.StatusOK)
	})

	body := []byte(`{"test":true}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/test", body, ts)

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/test", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)
	handler(w, req)

	assert.True(t, called)
}

// TestHmacAuth_SkewWindow_Accepts verifies that a signed request whose
// timestamp is within the configured skew window is accepted (200 OK).
func TestHmacAuth_SkewWindow_Accepts(t *testing.T) {
	// 8-minute-old request; skew window is 10 minutes → should pass.
	skew := 10 * time.Minute
	h := &Handler{apiKey: testAPIKey, webhookReplaySkew: skew}

	var called bool

	handler := h.hmacAuth(func(w http.ResponseWriter, _ *http.Request) {
		called = true

		w.WriteHeader(http.StatusOK)
	})

	body := []byte(`{"test":true}`)
	// Timestamp 8 minutes in the past.
	oldTS := time.Now().Add(-8 * time.Minute).Unix()
	ts := strconv.FormatInt(oldTS, 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/test", body, ts)

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/test", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)
	handler(w, req)

	assert.True(t, called, "handler should have been called: 8-min-old request within 10-min skew window")
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestHmacAuth_SkewWindow_Rejects verifies that a signed request whose
// timestamp exceeds the configured skew window is rejected with 401.
func TestHmacAuth_SkewWindow_Rejects(t *testing.T) {
	// Same 8-minute-old request; skew window is 1 minute → should fail.
	skew := 1 * time.Minute
	h := &Handler{apiKey: testAPIKey, webhookReplaySkew: skew}

	handler := h.hmacAuth(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called: timestamp outside skew window")
	})

	body := []byte(`{"test":true}`)
	// Timestamp 8 minutes in the past.
	oldTS := time.Now().Add(-8 * time.Minute).Unix()
	ts := strconv.FormatInt(oldTS, 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/test", body, ts)

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/test", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)
	handler(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestHandleTrigger_MissingFields(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", map[string]string{"card_id": "A-001"})
	h.hmacAuth(h.handleTrigger)(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
}

func TestHandleTrigger_ConcurrencyLimit(t *testing.T) {
	tr := tracker.New()
	// Fill up the tracker.
	for i := range 3 {
		_ = tr.Add(&tracker.ContainerInfo{
			CardID:  "EXIST-" + strconv.Itoa(i),
			Project: "proj",
		})
	}

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
		CardID:  "NEW-001",
		Project: "proj",
		RepoURL: "https://github.com/org/repo.git",
	})
	h.hmacAuth(h.handleTrigger)(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestHandleTrigger_Duplicate(t *testing.T) {
	tr := tracker.New()
	_ = tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-042",
		Project: "my-project",
	})

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
		CardID:     "PROJ-042",
		Project:    "my-project",
		RepoURL:    "https://github.com/org/repo.git",
		BaseBranch: "main",
	})
	h.hmacAuth(h.handleTrigger)(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

// TestHandleTrigger_BaseBranchAccepted verifies that a payload containing
// base_branch is parsed without error (no 400 Bad Request response).
// The request conflicts with the pre-seeded duplicate so manager.Run is never
// called, but the handler must successfully decode base_branch first.
func TestHandleTrigger_BaseBranchAccepted(t *testing.T) {
	tr := tracker.New()
	// Seed a duplicate so the handler returns 409 before calling manager.Run,
	// allowing manager to be nil while still exercising JSON decoding.
	_ = tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-200",
		Project: "my-project",
	})

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
		CardID:     "PROJ-200",
		Project:    "my-project",
		RepoURL:    "https://github.com/org/repo.git",
		BaseBranch: "develop",
	})
	h.hmacAuth(h.handleTrigger)(w, req)

	// 409 Conflict means the payload was parsed correctly (base_branch did not
	// cause a 400) and the duplicate check fired as expected.
	assert.Equal(t, http.StatusConflict, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.NotContains(t, resp.Message, "invalid JSON")
}

// TestHandleKill_IdempotentWhenAlreadyStopped verifies that /kill on a card
// with no tracked container and no matching labeled Docker container returns
// 200 OK. The old behaviour was 404, which made retry logic
// in CM harder (a legitimate not-yet-started tracker miss was indistinguishable
// from a hard failure). ForceRemoveByLabels returns 0 for this case so the
// handler falls through to the no-op branch.
func TestHandleKill_IdempotentWhenAlreadyStopped(t *testing.T) {
	fake := &reconcileFakeRunner{forceRet: 0}
	h := NewHandler(fake, tracker.New(), nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/kill", KillPayload{
		CardID:  "PROJ-999",
		Project: "proj",
	})
	h.hmacAuth(h.handleKill)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp SuccessResponse

	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Contains(t, resp.Message, "no-op")
}

// reconcileFakeRunner is a ContainerRunner double that records
// ForceRemoveByLabels calls and returns a configurable ListManaged response.
// Used by the /containers and /kill-fallback tests where neither Run nor Kill
// should fire.
type reconcileFakeRunner struct {
	mu sync.Mutex

	listed    []container.ManagedContainer
	listErr   error
	forceCh   chan struct{ project, cardID string }
	forceRet  int
	forceErr  error
	forceCard string
}

func (f *reconcileFakeRunner) Run(_ context.Context, _ container.RunConfig) {
	panic("reconcileFakeRunner.Run must not be called")
}

func (f *reconcileFakeRunner) Kill(_, _ string) error {
	panic("reconcileFakeRunner.Kill must not be called")
}

func (f *reconcileFakeRunner) ListManaged(_ context.Context) ([]container.ManagedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.listErr != nil {
		return nil, f.listErr
	}

	out := make([]container.ManagedContainer, len(f.listed))
	copy(out, f.listed)

	return out, nil
}

func (f *reconcileFakeRunner) ForceRemoveByLabels(_ context.Context, project, cardID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.forceCard = project + "/" + cardID

	if f.forceCh != nil {
		f.forceCh <- struct{ project, cardID string }{project, cardID}
	}

	return f.forceRet, f.forceErr
}

func (f *reconcileFakeRunner) StartChat(_ context.Context, _ container.StartChatOpts) (string, error) {
	return "", nil
}

func (f *reconcileFakeRunner) Stop(_ context.Context, _ string) error { return nil }

func (f *reconcileFakeRunner) WorkerImage() string { return "" }

func (f *reconcileFakeRunner) BuildChatAuthEnv(_ context.Context) string { return "" }

func (f *reconcileFakeRunner) AttachChatStdin(_ context.Context, _, _ string) error { return nil }

func (f *reconcileFakeRunner) StreamChatLogs(_ context.Context, _, _, _ string) {}

func (f *reconcileFakeRunner) WaitAndCleanupChat(_, _, _ string) {}

func (f *reconcileFakeRunner) DeleteChatCleanup(_ string) {}

func (f *reconcileFakeRunner) KillChat(_ context.Context, _ string) error { return nil }

// TestHandleListContainers_ReturnsDockerAuthoritativeList confirms that the
// endpoint surfaces every ManagedContainer returned by the manager, including
// the tracked/untracked split. The tracker state is reflected on each entry so
// CM's sweep can see tracker/Docker divergence in a single round-trip.
func TestHandleListContainers_ReturnsDockerAuthoritativeList(t *testing.T) {
	tr := tracker.New()

	started := time.Now().Add(-45 * time.Minute).UTC().Truncate(time.Second)
	fake := &reconcileFakeRunner{
		listed: []container.ManagedContainer{
			{
				ContainerID:   "abc123",
				ContainerName: "cmr-contextmatrix-ctxmax-436",
				CardID:        "ctxmax-436",
				Project:       "contextmatrix",
				State:         "running",
				StartedAt:     started,
				Tracked:       false,
			},
			{
				ContainerID:   "def456",
				ContainerName: "cmr-proj-alpha-001",
				CardID:        "alpha-001",
				Project:       "proj",
				State:         "exited",
				StartedAt:     started,
				Tracked:       true,
			},
		},
	}

	h := NewHandler(fake, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedGETRequest(t, "/containers")
	h.hmacAuth(h.handleListContainers)(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp ListContainersResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)
	require.Len(t, resp.Containers, 2)

	assert.Equal(t, "ctxmax-436", resp.Containers[0].CardID)
	assert.Equal(t, "contextmatrix", resp.Containers[0].Project)
	assert.Equal(t, "running", resp.Containers[0].State)
	assert.False(t, resp.Containers[0].Tracked)
	assert.Equal(t, started.Format(time.RFC3339), resp.Containers[0].StartedAt)

	assert.Equal(t, "alpha-001", resp.Containers[1].CardID)
	assert.True(t, resp.Containers[1].Tracked)
}

// TestHandleListContainers_DockerError returns 502 so CM distinguishes a
// runner misbehaving upstream from a legitimate empty list.
func TestHandleListContainers_DockerError(t *testing.T) {
	fake := &reconcileFakeRunner{listErr: fmt.Errorf("docker daemon unreachable")}
	h := NewHandler(fake, tracker.New(), nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedGETRequest(t, "/containers")
	h.hmacAuth(h.handleListContainers)(w, req)

	assert.Equal(t, http.StatusBadGateway, w.Code)
}

// TestHandleKill_ForceRemoveFallbackOnTrackerMiss confirms the class of leak
// fix: tracker has no entry for (project, card_id) but Docker still holds a
// labeled container — the /kill handler must reach past the tracker via
// ForceRemoveByLabels and return 200 with "force-removed" rather than the
// old 200 "no-op" that let the container leak to the 2h timer.
func TestHandleKill_ForceRemoveFallbackOnTrackerMiss(t *testing.T) {
	fake := &reconcileFakeRunner{forceRet: 1}
	h := NewHandler(fake, tracker.New(), nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/kill", KillPayload{
		CardID:  "ctxmax-436",
		Project: "contextmatrix",
	})
	h.hmacAuth(h.handleKill)(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp SuccessResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "force-removed", resp.Message)
	assert.Equal(t, "contextmatrix/ctxmax-436", fake.forceCard)
}

// TestHandleKill_NoOpWhenNeitherTrackerNorDockerHasContainer keeps the
// idempotent branch: if neither the tracker nor Docker has the card, /kill
// still returns 200 OK with a no-op message so CM's retry loop stays simple.
func TestHandleKill_NoOpWhenNeitherTrackerNorDockerHasContainer(t *testing.T) {
	fake := &reconcileFakeRunner{forceRet: 0}
	h := NewHandler(fake, tracker.New(), nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/kill", KillPayload{
		CardID:  "UNKNOWN-001",
		Project: "proj",
	})
	h.hmacAuth(h.handleKill)(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp SuccessResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Contains(t, resp.Message, "no-op")
}

func TestHandleHealth(t *testing.T) {
	tr := tracker.New()
	_ = tr.Add(&tracker.ContainerInfo{CardID: "A-001", Project: "proj"})

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/health", nil)
	h.handleHealth(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, true, resp["ok"])
	assert.InDelta(t, float64(1), resp["running_containers"], 1e-9)
	assert.InDelta(t, float64(3), resp["max_concurrent"], 1e-9)
}

// signedGETRequest builds a signed GET request with an empty body.
// The HMAC is computed over method+path+timestamp with an empty body.
func signedGETRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()

	parsed, err := neturl.Parse(rawURL)
	require.NoError(t, err)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodGet, parsed.Path, []byte{}, ts)

	req := httptest.NewRequestWithContext(context.Background(), "GET", rawURL, nil)
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)

	return req
}

// flushRecorder wraps httptest.ResponseRecorder to implement http.Flusher and
// signals each flush via the flushed channel so tests can synchronise.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		flushed:          make(chan struct{}, 64),
	}
}

func (f *flushRecorder) Flush() {
	f.ResponseRecorder.Flush()

	select {
	case f.flushed <- struct{}{}:
	default:
	}
}

// fakeRunner is a test double for ContainerRunner that records the RunConfig
// passed to Run so tests can assert on the propagated fields.
type fakeRunner struct {
	runCfg container.RunConfig
	runCh  chan container.RunConfig
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{runCh: make(chan container.RunConfig, 1)}
}

func (f *fakeRunner) Run(_ context.Context, cfg container.RunConfig) {
	f.runCfg = cfg
	f.runCh <- cfg
}

func (f *fakeRunner) Kill(_, _ string) error { return nil }

func (f *fakeRunner) ListManaged(_ context.Context) ([]container.ManagedContainer, error) {
	return nil, nil
}

func (f *fakeRunner) ForceRemoveByLabels(_ context.Context, _, _ string) (int, error) {
	return 0, nil
}

func (f *fakeRunner) StartChat(_ context.Context, _ container.StartChatOpts) (string, error) {
	return "", nil
}

func (f *fakeRunner) Stop(_ context.Context, _ string) error { return nil }

func (f *fakeRunner) WorkerImage() string { return "" }

func (f *fakeRunner) BuildChatAuthEnv(_ context.Context) string { return "" }

func (f *fakeRunner) AttachChatStdin(_ context.Context, _, _ string) error { return nil }

func (f *fakeRunner) StreamChatLogs(_ context.Context, _, _, _ string) {}

func (f *fakeRunner) WaitAndCleanupChat(_, _, _ string) {}

func (f *fakeRunner) DeleteChatCleanup(_ string) {}

func (f *fakeRunner) KillChat(_ context.Context, _ string) error { return nil }

// TestHandleTrigger_InteractivePropagated verifies that the Interactive field from the
// JSON trigger body is correctly propagated into the RunConfig passed to the manager.
func TestHandleTrigger_InteractivePropagated(t *testing.T) {
	tests := []struct {
		name        string
		interactive bool
	}{
		{"interactive true", true},
		{"interactive false (default)", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := tracker.New()
			fake := newFakeRunner()
			h := NewHandler(fake, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

			payload := TriggerPayload{
				CardID:      "PROJ-100",
				Project:     "my-project",
				RepoURL:     "https://github.com/org/repo.git",
				Interactive: tt.interactive,
			}
			w := httptest.NewRecorder()
			req := signedRequest(t, "/trigger", payload)
			h.hmacAuth(h.handleTrigger)(w, req)

			require.Equal(t, http.StatusAccepted, w.Code)

			select {
			case cfg := <-fake.runCh:
				assert.Equal(t, tt.interactive, cfg.Interactive)
				assert.Equal(t, testMCPURL, cfg.MCPURL, "handler must inject config-derived MCP URL, not payload")
				assert.Equal(t, container.ModeTask, cfg.Mode, "trigger handler must set Mode=ModeTask explicitly")
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for Run to be called")
			}
		})
	}
}

func TestHandleLogs_SSEHeaders(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	// Cancel the context immediately so handleLogs exits after setup.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := signedGETRequest(t, "/logs")
	req = req.WithContext(ctx)

	w := newFlushRecorder()
	h.hmacAuth(h.handleLogs)(w, req)

	assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", w.Header().Get("Cache-Control"))
	assert.Equal(t, "keep-alive", w.Header().Get("Connection"))
}

func TestHandleLogs_InitialConnectedKeepalive(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := signedGETRequest(t, "/logs")
	req = req.WithContext(ctx)

	w := newFlushRecorder()
	h.hmacAuth(h.handleLogs)(w, req)

	body := w.Body.String()
	assert.Contains(t, body, ": connected\n\n")
}

func TestHandleLogs_EventStreamed(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	// Use an httptest.Server so we have a real connection with proper flushing.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", h.hmacAuth(h.handleLogs))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodGet, "/logs", []byte{}, ts)

	req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/logs", nil)
	require.NoError(t, err)
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)

	client := &http.Client{}
	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// Read the initial ": connected\n\n" line.
	scanner := bufio.NewScanner(resp.Body)

	var firstLine string

	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			firstLine = line

			break
		}
	}

	assert.Equal(t, ": connected", firstLine)

	// Publish a log entry and verify it arrives as data: {json}\n\n.
	entry := logbroadcast.LogEntry{
		CardID:  "PROJ-001",
		Project: "my-project",
		Type:    "text",
		Content: "hello from runner",
	}
	b.Publish(entry)

	var dataLine string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			dataLine = line

			break
		}
	}

	require.NotEmpty(t, dataLine, "expected a data: line from SSE stream")

	jsonPart := strings.TrimPrefix(dataLine, "data: ")

	var got logbroadcast.LogEntry
	require.NoError(t, json.Unmarshal([]byte(jsonPart), &got))
	assert.Equal(t, entry.CardID, got.CardID)
	assert.Equal(t, entry.Project, got.Project)
	assert.Equal(t, entry.Type, got.Type)
	assert.Equal(t, entry.Content, got.Content)
}

func TestHandleLogs_ProjectFilter(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", h.hmacAuth(h.handleLogs))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodGet, "/logs?project=alpha", []byte{}, ts)

	// Subscribe only to "alpha" project.
	req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/logs?project=alpha", nil)
	require.NoError(t, err)
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)

	client := &http.Client{}
	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	scanner := bufio.NewScanner(resp.Body)

	// Consume the initial connected comment.
	for scanner.Scan() {
		if scanner.Text() == ": connected" {
			break
		}
	}

	// Publish to a non-matching project first, then to "alpha".
	b.Publish(logbroadcast.LogEntry{CardID: "BETA-001", Project: "beta", Type: "text", Content: "should not arrive"})
	b.Publish(logbroadcast.LogEntry{CardID: "ALPHA-001", Project: "alpha", Type: "text", Content: "should arrive"})

	// Collect lines until we get a data line or timeout.
	lineCh := make(chan string, 16)

	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				lineCh <- line

				return
			}
		}
	}()

	select {
	case dataLine := <-lineCh:
		var got logbroadcast.LogEntry
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(dataLine, "data: ")), &got))
		assert.Equal(t, "ALPHA-001", got.CardID, "expected only the alpha entry to arrive")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for filtered SSE event")
	}
}

func TestHandleLogs_ClientDisconnect(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", h.hmacAuth(h.handleLogs))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodGet, "/logs", []byte{}, ts)

	req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/logs", nil)
	require.NoError(t, err)
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)

	client := &http.Client{}
	resp, err := client.Do(req)
	require.NoError(t, err)

	// Wait for the initial connected comment, then close the connection.
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if scanner.Text() == ": connected" {
			break
		}
	}

	assert.Equal(t, 1, b.SubscriberCount(), "should have 1 subscriber while connected")

	_ = resp.Body.Close()

	// After disconnect the broadcaster should eventually have 0 subscribers
	// (the handler goroutine detects context cancellation and calls unsubscribe).
	require.Eventually(t, func() bool {
		return b.SubscriberCount() == 0
	}, 2*time.Second, 50*time.Millisecond, "subscriber should be removed after client disconnect")
}

// --- /message handler tests ---

// fakeWriteCloser is a simple io.WriteCloser that records all written bytes.
type fakeWriteCloser struct {
	buf    []byte
	closed bool
}

func (f *fakeWriteCloser) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)

	return len(p), nil
}

func (f *fakeWriteCloser) Close() error {
	f.closed = true

	return nil
}

// setupMessageHandler builds a Handler with a tracker that has a container
// registered (optionally with stdin attached) and a broadcaster.
// Returns handler, broadcaster, and the fake stdin.
func setupMessageHandler(t *testing.T, withStdin bool) (*Handler, *logbroadcast.Broadcaster, *fakeWriteCloser) {
	t.Helper()

	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	var fw *fakeWriteCloser
	if withStdin {
		fw = &fakeWriteCloser{}
		tr.SetStdin("my-project", "PROJ-001", fw, nil)
	}

	return h, b, fw
}

func TestHandleMessage_HappyPath(t *testing.T) {
	h, b, fw := setupMessageHandler(t, true)

	ch, unsub := b.Subscribe("")
	defer unsub()

	payload := MessagePayload{
		CardID:    "PROJ-001",
		Project:   "my-project",
		Content:   "hello from user",
		MessageID: "msg-abc-123",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	var resp SuccessResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "msg-abc-123", resp.MessageID)

	// Verify exactly one write arrived on the fake stdin and is valid stream-json.
	require.NotNil(t, fw)
	require.NotEmpty(t, fw.buf, "expected a write to stdin")

	line := fw.buf
	assert.Equal(t, byte('\n'), line[len(line)-1], "stream-json line must end with newline")

	var got streammsg.UserMessage
	require.NoError(t, json.Unmarshal(line[:len(line)-1], &got))
	assert.Equal(t, "user", got.Type)
	assert.Equal(t, "user", got.Message.Role)
	require.Len(t, got.Message.Content, 1)
	assert.Equal(t, "text", got.Message.Content[0].Type)
	assert.Equal(t, "hello from user", got.Message.Content[0].Text)

	// Verify broadcaster received a "user" LogEntry.
	select {
	case entry := <-ch:
		assert.Equal(t, "user", entry.Type)
		assert.Equal(t, "PROJ-001", entry.CardID)
		assert.Equal(t, "my-project", entry.Project)
		assert.Equal(t, "hello from user", entry.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for broadcaster entry")
	}
}

func TestHandleMessage_400_MissingFields(t *testing.T) {
	cases := []struct {
		name    string
		payload any
	}{
		{"invalid JSON", "not-json"},
		{"missing card_id", MessagePayload{Project: "p", Content: "c"}},
		{"missing project", MessagePayload{CardID: "C-1", Content: "c"}},
		{"missing content", MessagePayload{CardID: "C-1", Project: "p"}},
	}

	h, _, _ := setupMessageHandler(t, false)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()

			var req *http.Request

			if s, ok := tc.payload.(string); ok {
				// Invalid JSON — sign a raw string body.
				body := []byte(s)
				ts := strconv.FormatInt(time.Now().Unix(), 10)
				sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/message", body, ts)
				req = httptest.NewRequestWithContext(context.Background(), "POST", "/message", strings.NewReader(s))
				req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
				req.Header.Set(cmhmac.TimestampHeader, ts)
			} else {
				req = signedRequest(t, "/message", tc.payload)
			}

			h.hmacAuth(h.handleMessage)(w, req)
			assert.Equal(t, http.StatusBadRequest, w.Code, tc.name)
		})
	}
}

func TestHandleMessage_404_NotTracked(t *testing.T) {
	h, _, _ := setupMessageHandler(t, false)

	payload := MessagePayload{
		CardID:  "NONEXISTENT",
		Project: "my-project",
		Content: "hello",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandleMessage_409_NoStdin(t *testing.T) {
	// Container tracked but stdin not attached (non-interactive mode).
	h, b, _ := setupMessageHandler(t, false)

	ch, unsub := b.Subscribe("")
	defer unsub()

	payload := MessagePayload{
		CardID:  "PROJ-001",
		Project: "my-project",
		Content: "hello",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)

	// No phantom echo: the broadcaster must NOT have received a user LogEntry.
	select {
	case entry := <-ch:
		t.Fatalf("expected no broadcast on 409, got entry: %+v", entry)
	case <-time.After(100 * time.Millisecond):
		// correct — nothing published
	}
}

func TestHandleMessage_413_ContentTooLarge(t *testing.T) {
	h, _, _ := setupMessageHandler(t, true)

	payload := MessagePayload{
		CardID:  "PROJ-001",
		Project: "my-project",
		Content: strings.Repeat("x", 8193),
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestHandleMessage_401_InvalidHMAC(t *testing.T) {
	h, _, _ := setupMessageHandler(t, true)

	body, _ := json.Marshal(MessagePayload{CardID: "PROJ-001", Project: "my-project", Content: "hi"})
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/message", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256=badhash")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))

	w := httptest.NewRecorder()
	h.hmacAuth(h.handleMessage)(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- /promote handler tests ---

// autonomousTrueServer starts an httptest server that always returns
// autonomous=true so /promote happy-path tests can satisfy the W5 fail-closed
// gate (cmClient is required; nil cmClient now produces a 502). The server is
// torn down via t.Cleanup so callers don't need an extra defer.
func autonomousTrueServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"autonomous":true}`))
	}))
	t.Cleanup(srv.Close)

	return srv
}

// setupPromoteHandler builds a Handler with a tracker that has a container
// registered (optionally with stdin attached), a broadcaster, and a real
// callback.Client pointed at a fake CM that returns autonomous=true. After
// fix W5, /promote refuses to write to stdin unless cmClient.VerifyAutonomous
// returns true — so happy-path tests need a server, not a nil client.
func setupPromoteHandler(t *testing.T, withStdin bool) (*Handler, *logbroadcast.Broadcaster, *fakeWriteCloser) {
	t.Helper()

	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	srv := autonomousTrueServer(t)
	cmClient := callback.NewClient(srv.URL, "key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testMCPURL, nil, 0, nil)

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	var fw *fakeWriteCloser
	if withStdin {
		fw = &fakeWriteCloser{}
		tr.SetStdin("my-project", "PROJ-001", fw, nil)
	}

	return h, b, fw
}

func TestHandlePromote_HappyPath(t *testing.T) {
	h, b, fw := setupPromoteHandler(t, true)

	ch, unsub := b.Subscribe("")
	defer unsub()

	payload := PromotePayload{
		CardID:  "PROJ-001",
		Project: "my-project",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	var resp SuccessResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)

	// Verify system LogEntry published.
	select {
	case entry := <-ch:
		assert.Equal(t, "system", entry.Type)
		assert.Equal(t, "PROJ-001", entry.CardID)
		assert.Equal(t, "my-project", entry.Project)
		assert.Equal(t, "promoted to autonomous mode", entry.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for broadcaster entry")
	}

	// Verify stdin received a valid stream-json line with the canned content.
	require.NotNil(t, fw)
	require.NotEmpty(t, fw.buf)

	line := fw.buf
	assert.Equal(t, byte('\n'), line[len(line)-1], "stream-json line must end with newline")

	var got streammsg.UserMessage
	require.NoError(t, json.Unmarshal(line[:len(line)-1], &got))
	assert.Equal(t, "user", got.Type)
	assert.Equal(t, "user", got.Message.Role)
	require.Len(t, got.Message.Content, 1)
	assert.Equal(t, "text", got.Message.Content[0].Type)
	assert.Equal(t, autonomousContent, got.Message.Content[0].Text)
}

func TestHandlePromote_400_MissingFields(t *testing.T) {
	cases := []struct {
		name    string
		payload any
	}{
		{"invalid JSON", "not-json"},
		{"missing card_id", PromotePayload{Project: "my-project"}},
		{"missing project", PromotePayload{CardID: "PROJ-001"}},
	}

	h, _, _ := setupPromoteHandler(t, false)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()

			var req *http.Request

			if s, ok := tc.payload.(string); ok {
				body := []byte(s)
				ts := strconv.FormatInt(time.Now().Unix(), 10)
				sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/promote", body, ts)
				req = httptest.NewRequestWithContext(context.Background(), "POST", "/promote", strings.NewReader(s))
				req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
				req.Header.Set(cmhmac.TimestampHeader, ts)
			} else {
				req = signedRequest(t, "/promote", tc.payload)
			}

			h.hmacAuth(h.handlePromote)(w, req)
			assert.Equal(t, http.StatusBadRequest, w.Code, tc.name)
		})
	}
}

func TestHandlePromote_404_NotTracked(t *testing.T) {
	h, _, _ := setupPromoteHandler(t, false)

	payload := PromotePayload{
		CardID:  "NONEXISTENT",
		Project: "my-project",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandlePromote_409_NoStdin(t *testing.T) {
	// Container tracked but stdin not attached (non-interactive mode).
	h, _, _ := setupPromoteHandler(t, false)

	payload := PromotePayload{
		CardID:  "PROJ-001",
		Project: "my-project",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
}

func TestHandlePromote_401_InvalidHMAC(t *testing.T) {
	h, _, _ := setupPromoteHandler(t, true)

	body, _ := json.Marshal(PromotePayload{CardID: "PROJ-001", Project: "my-project"})
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/promote", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256=badhash")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))

	w := httptest.NewRecorder()
	h.hmacAuth(h.handlePromote)(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestHandlePromote_PublishAfterStdinSuccess(t *testing.T) {
	// W4: the system LogEntry must be published only AFTER the stdin write
	// succeeds — never before — so a failed write does not leave a phantom
	// "promoted to autonomous mode" line in the UI. The previous ordering
	// asserted "publish first, then write"; that was the bug W4 fixed.
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)

	// A controlled stdin that signals when Write is called.
	stdinWritten := make(chan struct{}, 1)
	controlled := &controlledWriteCloser{writeCh: stdinWritten}

	srv := autonomousTrueServer(t)
	cmClient := callback.NewClient(srv.URL, "key", slog.New(slog.NewTextHandler(io.Discard, nil)))

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testMCPURL, nil, 0, nil)
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))
	tr.SetStdin("my-project", "PROJ-001", controlled, nil)

	ch, unsub := b.Subscribe("")
	defer unsub()

	payload := PromotePayload{CardID: "PROJ-001", Project: "my-project"}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	// The stdin write must have happened (signal sent before handler returned).
	select {
	case <-stdinWritten:
		// good — write occurred
	default:
		t.Fatal("stdin was not written")
	}

	// The system LogEntry must arrive on the subscriber channel after the
	// successful stdin write.
	select {
	case entry := <-ch:
		assert.Equal(t, "system", entry.Type)
		assert.Equal(t, "promoted to autonomous mode", entry.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for system LogEntry")
	}
}

// TestHandlePromote_NoPublishOnStdinFailure verifies the W4 invariant from the
// other direction: when WriteStdin fails (here: no stdin attached → 409), the
// handler must NOT publish the "promoted to autonomous mode" system LogEntry.
// Previously the broadcaster.Publish call ran before WriteStdin, so a phantom
// promotion line appeared in the UI for a promote that actually errored out.
func TestHandlePromote_NoPublishOnStdinFailure(t *testing.T) {
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)

	srv := autonomousTrueServer(t)
	cmClient := callback.NewClient(srv.URL, "key", slog.New(slog.NewTextHandler(io.Discard, nil)))

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testMCPURL, nil, 0, nil)
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))
	// Deliberately NO SetStdin — WriteStdin will return ErrNoStdinAttached.

	ch, unsub := b.Subscribe("")
	defer unsub()

	payload := PromotePayload{CardID: "PROJ-001", Project: "my-project"}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	require.Equal(t, http.StatusConflict, w.Code,
		"non-interactive container must produce 409 from /promote")

	// No system LogEntry must arrive: the failed promote must not echo into
	// the broadcaster. Give the handler enough time to publish if it would
	// erroneously do so (Publish is synchronous so this is generous).
	select {
	case entry := <-ch:
		t.Fatalf("no LogEntry should have been published, but got: type=%q content=%q",
			entry.Type, entry.Content)
	case <-time.After(100 * time.Millisecond):
		// good — nothing published
	}
}

func TestHandlePromote_APICallBeforeStdin(t *testing.T) {
	// When cmClient is set, the contextmatrix verify-autonomous GET must be called
	// BEFORE stdin write. On autonomous=true, stdin write proceeds normally.
	var (
		apiCalled      bool
		receivedMethod string
	)

	mu := &sync.Mutex{}

	cmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		apiCalled = true
		receivedMethod = r.Method
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"PROJ-001","autonomous":true}`))
	}))
	defer cmServer.Close()

	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	cmClient := callback.NewClient(cmServer.URL, "key", nil)

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testMCPURL, nil, 0, nil)
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	payload := PromotePayload{CardID: "PROJ-001", Project: "my-project"}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.True(t, apiCalled, "contextmatrix verify-autonomous GET must be called")
	// Must use GET to avoid re-triggering the promote webhook loop.
	assert.Equal(t, http.MethodGet, receivedMethod, "must use GET, not POST")
	assert.NotEmpty(t, fw.buf, "stdin must be written after autonomous=true verification")
}

func TestHandlePromote_APIFailure_FailClosed(t *testing.T) {
	// When the contextmatrix GET returns a server error, the handler returns 502
	// and must NOT write anything to stdin (fail closed — card stays in HITL mode).
	cmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer cmServer.Close()

	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	cmClient := callback.NewClient(cmServer.URL, "key", nil)

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testMCPURL, nil, 0, nil)
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	payload := PromotePayload{CardID: "PROJ-001", Project: "my-project"}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	assert.Equal(t, http.StatusBadGateway, w.Code, "CM error must produce 502")
	assert.Empty(t, fw.buf, "stdin must NOT be written when CM returns error")

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
}

// TestHandlePromote_APIFailure_GenericErrorBody verifies that the 502 body
// returned on upstream CM failure is a fixed, generic shape — it must NOT
// contain the upstream response body (which may leak tokens or other secrets
// if CM is misconfigured).
func TestHandlePromote_APIFailure_GenericErrorBody(t *testing.T) {
	// Upstream CM response body contains a secret-like substring to ensure it
	// cannot leak into our response body.
	upstreamBody := `{"error":"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn leaked"}`

	cmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer cmServer.Close()

	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	cmClient := callback.NewClient(cmServer.URL, "key", nil)

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testMCPURL, nil, 0, nil)
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	payload := PromotePayload{CardID: "PROJ-001", Project: "my-project"}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	assert.Equal(t, http.StatusBadGateway, w.Code)

	// The response body must be the fixed generic shape — no "error":"..."
	// echoing of the upstream body, no leaked token substring.
	raw := w.Body.String()
	assert.NotContains(t, raw, "ghp_", "upstream token must not leak into 502 response")
	assert.NotContains(t, raw, "leaked", "upstream body text must not leak into 502 response")

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.OK)
	assert.Equal(t, CodeUpstreamFailure, resp.Code)
	assert.Equal(t, "upstream verification failed", resp.Message)

	// stdin must NOT have been written.
	assert.Empty(t, fw.buf)
}

func TestHandlePromote_AutonomousFalse_FailClosed(t *testing.T) {
	// When CM returns autonomous=false the card has not been promoted yet.
	// The handler must return 403 and NOT write to stdin.
	cmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"PROJ-001","autonomous":false}`))
	}))
	defer cmServer.Close()

	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	cmClient := callback.NewClient(cmServer.URL, "key", nil)

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testMCPURL, nil, 0, nil)
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	payload := PromotePayload{CardID: "PROJ-001", Project: "my-project"}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "autonomous=false must produce 403")
	assert.Empty(t, fw.buf, "stdin must NOT be written when autonomous=false")

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Equal(t, CodeForbidden, resp.Code, "autonomous=false must use CodeForbidden, not CodeConflict")
	assert.Contains(t, resp.Message, "autonomous flag is not set")
}

// errorWriteCloser returns an error on every Write call. Used to exercise the
// write-failure path in /promote (and /message) handlers.
type errorWriteCloser struct {
	closed bool
	err    error
}

func (e *errorWriteCloser) Write(_ []byte) (int, error) { return 0, e.err }
func (e *errorWriteCloser) Close() error {
	e.closed = true

	return nil
}

func TestHandlePromote_ClosesStdinOnSuccess(t *testing.T) {
	// A valid /promote must close stdin after writing the canned message.
	h, _, fw := setupPromoteHandler(t, true)

	payload := PromotePayload{
		CardID:  "PROJ-001",
		Project: "my-project",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	var resp SuccessResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)

	require.NotNil(t, fw)
	assert.True(t, fw.closed, "stdin must be closed after successful /promote")
}

func TestHandlePromote_EndSessionIdempotentAfterPromote(t *testing.T) {
	// After /promote closes stdin, a subsequent /end-session must return
	// the idempotent 409 (stdin already closed) without panicking.
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	srv := autonomousTrueServer(t)
	cmClient := callback.NewClient(srv.URL, "key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testMCPURL, nil, 0, nil)

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	promotePayload := PromotePayload{CardID: "PROJ-001", Project: "my-project"}

	// /promote succeeds and closes stdin.
	wp := httptest.NewRecorder()
	h.hmacAuth(h.handlePromote)(wp, signedRequest(t, "/promote", promotePayload))
	require.Equal(t, http.StatusAccepted, wp.Code)
	require.True(t, fw.closed, "stdin must be closed by /promote before /end-session")

	// /end-session on already-closed stdin returns 410 Gone (idempotent
	// retry; the session was active and has been closed). Fix W5: this
	// case used to share the 409 sentinel with "container is non-
	// interactive", which is misleading — the container WAS interactive
	// and is now over.
	endPayload := EndSessionPayload{CardID: "PROJ-001", Project: "my-project"}
	we := httptest.NewRecorder()
	h.hmacAuth(h.handleEndSession)(we, signedRequest(t, "/end-session", endPayload))
	assert.Equal(t, http.StatusGone, we.Code, "/end-session after /promote must return 410")

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(we.Body.Bytes(), &resp))
	assert.Equal(t, CodeStdinClosed, resp.Code, "/end-session idempotent retry must use stdin_closed code")
}

// TestHandlePromote_NilCMClient_500Internal verifies the W3 fix: a /promote
// against a runner whose cmClient was never wired must return 500 with
// CodeInternal, not 502 with CodeUpstreamFailure. The original 502 implied
// "CM is unreachable" but the actual cause is runner-side misconfiguration —
// surface it as such so operator dashboards do not blame CM.
func TestHandlePromote_NilCMClient_500Internal(t *testing.T) {
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	// cmClient = nil deliberately exercises the misconfiguration branch.
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	payload := PromotePayload{CardID: "PROJ-001", Project: "my-project"}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"nil cmClient is a runner-side misconfiguration → 500, not 502")

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.OK)
	assert.Equal(t, CodeInternal, resp.Code,
		"misconfiguration must surface as CodeInternal, not CodeUpstreamFailure")
	assert.Empty(t, fw.buf, "stdin must NOT be written when cmClient is misconfigured")
}

// TestHandleLogs_NilBroadcaster_500Internal verifies the W4 fix: handleLogs
// nil-guards the broadcaster like every other handler that touches it.
// Without the guard a Handler constructed with a nil broadcaster would
// panic on /logs.
func TestHandleLogs_NilBroadcaster_500Internal(t *testing.T) {
	// Broadcaster deliberately nil. Don't subscribe; we just want to confirm
	// the handler returns a structured 500 instead of panicking.
	h := NewHandler(nil, tracker.New(), nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	req := signedGETRequest(t, "/logs")

	w := newFlushRecorder()
	h.hmacAuth(h.handleLogs)(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"nil broadcaster must surface as a structured 500, not a panic")

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, CodeInternal, resp.Code)
}

func TestHandlePromote_WriteFailure_StdinNotClosed(t *testing.T) {
	// When the canned-message stdin write returns an error, /promote must NOT
	// close stdin and must return the existing error response.
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	srv := autonomousTrueServer(t)
	cmClient := callback.NewClient(srv.URL, "key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testMCPURL, nil, 0, nil)

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	ewc := &errorWriteCloser{err: fmt.Errorf("disk full")}
	tr.SetStdin("my-project", "PROJ-001", ewc, nil)

	payload := PromotePayload{CardID: "PROJ-001", Project: "my-project"}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	// The write failure should produce an error response (500 internal error).
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)

	// stdin must NOT have been closed.
	assert.False(t, ewc.closed, "stdin must NOT be closed when the canned-message write fails")
}

// controlledWriteCloser records writes and signals via writeCh.
type controlledWriteCloser struct {
	buf     []byte
	closed  bool
	writeCh chan struct{}
}

func (c *controlledWriteCloser) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	select {
	case c.writeCh <- struct{}{}:
	default:
	}

	return len(p), nil
}

func (c *controlledWriteCloser) Close() error {
	c.closed = true

	return nil
}

func TestHandleMessage_Escaping(t *testing.T) {
	h, _, fw := setupMessageHandler(t, true)

	// Content with embedded quotes, newlines, and non-ASCII.
	content := "say \"hello\"\nand café 🚀"

	payload := MessagePayload{
		CardID:  "PROJ-001",
		Project: "my-project",
		Content: content,
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)
	require.NotEmpty(t, fw.buf)

	line := fw.buf

	var got streammsg.UserMessage
	require.NoError(t, json.Unmarshal(line[:len(line)-1], &got), "captured bytes must be valid JSON")
	require.Len(t, got.Message.Content, 1)
	assert.Equal(t, content, got.Message.Content[0].Text, "text must round-trip byte-for-byte")
}

// --- /end-session handler tests ---

// setupEndSessionHandler builds a Handler with a tracker that has a container
// registered (optionally with stdin attached) and a broadcaster.
func setupEndSessionHandler(t *testing.T, withStdin bool) (*Handler, *logbroadcast.Broadcaster, *fakeWriteCloser) {
	t.Helper()

	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	var fw *fakeWriteCloser
	if withStdin {
		fw = &fakeWriteCloser{}
		tr.SetStdin("my-project", "PROJ-001", fw, nil)
	}

	return h, b, fw
}

func TestHandleEndSession_HappyPath(t *testing.T) {
	h, b, fw := setupEndSessionHandler(t, true)

	ch, unsub := b.Subscribe("")
	defer unsub()

	payload := EndSessionPayload{
		CardID:  "PROJ-001",
		Project: "my-project",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/end-session", payload)
	h.hmacAuth(h.handleEndSession)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	var resp SuccessResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)

	require.NotNil(t, fw)
	assert.True(t, fw.closed, "stdin writer should be closed")

	select {
	case entry := <-ch:
		assert.Equal(t, "system", entry.Type)
		assert.Equal(t, "PROJ-001", entry.CardID)
		assert.Equal(t, "my-project", entry.Project)
		assert.Equal(t, "session ended (stdin closed)", entry.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for broadcaster entry")
	}
}

func TestHandleEndSession_400_MissingFields(t *testing.T) {
	cases := []struct {
		name    string
		payload any
	}{
		{"invalid JSON", "not-json"},
		{"missing card_id", EndSessionPayload{Project: "p"}},
		{"missing project", EndSessionPayload{CardID: "C-1"}},
	}

	h, _, _ := setupEndSessionHandler(t, false)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()

			var req *http.Request

			if s, ok := tc.payload.(string); ok {
				body := []byte(s)
				ts := strconv.FormatInt(time.Now().Unix(), 10)
				sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/end-session", body, ts)
				req = httptest.NewRequestWithContext(context.Background(), "POST", "/end-session", strings.NewReader(s))
				req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
				req.Header.Set(cmhmac.TimestampHeader, ts)
			} else {
				req = signedRequest(t, "/end-session", tc.payload)
			}

			h.hmacAuth(h.handleEndSession)(w, req)
			assert.Equal(t, http.StatusBadRequest, w.Code, tc.name)
		})
	}
}

func TestHandleEndSession_404_NotTracked(t *testing.T) {
	h, _, _ := setupEndSessionHandler(t, false)

	payload := EndSessionPayload{
		CardID:  "NONEXISTENT",
		Project: "my-project",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/end-session", payload)
	h.hmacAuth(h.handleEndSession)(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandleEndSession_409_NoStdin(t *testing.T) {
	h, b, _ := setupEndSessionHandler(t, false)

	ch, unsub := b.Subscribe("")
	defer unsub()

	payload := EndSessionPayload{
		CardID:  "PROJ-001",
		Project: "my-project",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/end-session", payload)
	h.hmacAuth(h.handleEndSession)(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)

	// No broadcast on 409.
	select {
	case entry := <-ch:
		t.Fatalf("expected no broadcast on 409, got entry: %+v", entry)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHandleEndSession_Idempotent(t *testing.T) {
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	closeCount := 0
	w := &countingWriteCloser{
		closeFn: func() error {
			closeCount++

			return nil
		},
	}
	tr.SetStdin("my-project", "PROJ-001", w, nil)

	payload := EndSessionPayload{
		CardID:  "PROJ-001",
		Project: "my-project",
	}

	// First call closes stdin.
	w1 := httptest.NewRecorder()
	h.hmacAuth(h.handleEndSession)(w1, signedRequest(t, "/end-session", payload))
	require.Equal(t, http.StatusAccepted, w1.Code)

	// Second call distinguishes "had stdin, closed" (410 Gone, idempotent
	// retry) from "never had stdin" (409 Conflict, non-interactive). Fix
	// W5: the pre-fix code collapsed both into 409, which made retried
	// /end-session indistinguishable from /end-session against a card
	// that was never interactive — operators couldn't tell whether their
	// retry succeeded or whether the container ran in autonomous mode
	// all along.
	w2 := httptest.NewRecorder()
	h.hmacAuth(h.handleEndSession)(w2, signedRequest(t, "/end-session", payload))
	assert.Equal(t, http.StatusGone, w2.Code, "second /end-session must return 410, not 409")

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w2.Body).Decode(&resp))
	assert.Equal(t, CodeStdinClosed, resp.Code,
		"second /end-session must use stdin_closed code, not conflict")

	assert.Equal(t, 1, closeCount, "writer must be closed exactly once across two /end-session calls")
}

// TestHandleEndSession_NeverInteractive_409 is the negative case of
// TestHandleEndSession_Idempotent: a /end-session against a container
// that was never made interactive (SetStdin never called) must still
// return 409 with CodeConflict / MsgNotInteractive, not 410. This
// preserves the existing semantics of W5 — only "was interactive, now
// closed" maps to 410.
func TestHandleEndSession_NeverInteractive_409(t *testing.T) {
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))
	// Deliberately do NOT call SetStdin so info.stdin is nil → 409.

	payload := EndSessionPayload{CardID: "PROJ-001", Project: "my-project"}

	w := httptest.NewRecorder()
	h.hmacAuth(h.handleEndSession)(w, signedRequest(t, "/end-session", payload))
	require.Equal(t, http.StatusConflict, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, CodeConflict, resp.Code,
		"never-interactive /end-session must keep the 409/conflict shape")
}

// countingWriteCloser counts Close calls. Defined in tracker_test.go of the
// tracker package — redeclare a minimal copy here to keep packages isolated.
type countingWriteCloser struct {
	closeFn func() error
}

func (c *countingWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (c *countingWriteCloser) Close() error {
	if c.closeFn != nil {
		return c.closeFn()
	}

	return nil
}

// --- /stop-all handler tests ---

// stopAllFakeRunner is a ContainerRunner double that records every Kill call
// and can be configured to fail for a specific card ID. Unlike fakeRunner it
// is safe for concurrent use.
type stopAllFakeRunner struct {
	mu       sync.Mutex
	killed   []string
	failFor  map[string]bool
	killErr  error
	runCalls int
}

func (s *stopAllFakeRunner) Run(_ context.Context, _ container.RunConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.runCalls++
}

func (s *stopAllFakeRunner) Kill(project, cardID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failFor[project+"/"+cardID] {
		return s.killErr
	}

	s.killed = append(s.killed, project+"/"+cardID)

	return nil
}

func (s *stopAllFakeRunner) ListManaged(_ context.Context) ([]container.ManagedContainer, error) {
	return nil, nil
}

func (s *stopAllFakeRunner) ForceRemoveByLabels(_ context.Context, _, _ string) (int, error) {
	return 0, nil
}

func (s *stopAllFakeRunner) StartChat(_ context.Context, _ container.StartChatOpts) (string, error) {
	return "", nil
}

func (s *stopAllFakeRunner) Stop(_ context.Context, _ string) error { return nil }

func (s *stopAllFakeRunner) WorkerImage() string { return "" }

func (s *stopAllFakeRunner) BuildChatAuthEnv(_ context.Context) string { return "" }

func (s *stopAllFakeRunner) AttachChatStdin(_ context.Context, _, _ string) error { return nil }

func (s *stopAllFakeRunner) StreamChatLogs(_ context.Context, _, _, _ string) {}

func (s *stopAllFakeRunner) WaitAndCleanupChat(_, _, _ string) {}

func (s *stopAllFakeRunner) DeleteChatCleanup(_ string) {}

// KillChat records a chat-mode kill the same way Kill records a card-mode kill,
// so /stop-all tests can assert which sessions were dispatched to KillChat.
func (s *stopAllFakeRunner) KillChat(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failFor["session:"+sessionID] {
		return s.killErr
	}

	s.killed = append(s.killed, "session:"+sessionID)

	return nil
}

func (s *stopAllFakeRunner) killedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, len(s.killed))
	copy(out, s.killed)

	return out
}

func TestHandleStopAll_Success(t *testing.T) {
	tr := tracker.New()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "A-001", Project: "alpha"}))
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "A-002", Project: "alpha"}))
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "B-001", Project: "beta"}))

	fake := &stopAllFakeRunner{}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/stop-all", StopAllPayload{})
	h.hmacAuth(h.handleStopAll)(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp StopAllResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Equal(t, 3, resp.Total)
	assert.Equal(t, 3, resp.Stopped)
	assert.Equal(t, 0, resp.Failed)
	assert.Len(t, resp.Results, 3)

	killed := fake.killedIDs()
	assert.ElementsMatch(t, []string{"alpha/A-001", "alpha/A-002", "beta/B-001"}, killed)
}

func TestHandleStopAll_ProjectFilter(t *testing.T) {
	tr := tracker.New()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "A-001", Project: "alpha"}))
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "A-002", Project: "alpha"}))
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "B-001", Project: "beta"}))

	fake := &stopAllFakeRunner{}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/stop-all", StopAllPayload{Project: "alpha"})
	h.hmacAuth(h.handleStopAll)(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	killed := fake.killedIDs()
	assert.ElementsMatch(t, []string{"alpha/A-001", "alpha/A-002"}, killed,
		"only alpha project containers must be stopped")

	// Beta container still registered.
	_, ok := tr.Snapshot("beta", "B-001")
	assert.True(t, ok, "beta container must not be affected by project-filtered stop-all")
}

func TestHandleStopAll_NoContainers(t *testing.T) {
	tr := tracker.New()
	fake := &stopAllFakeRunner{}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/stop-all", StopAllPayload{})
	h.hmacAuth(h.handleStopAll)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp StopAllResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Equal(t, 0, resp.Total)
	assert.Equal(t, 0, resp.Stopped)
	assert.Equal(t, 0, resp.Failed)
	assert.Empty(t, resp.Results)
	assert.Empty(t, fake.killedIDs(), "no Kill calls should fire when tracker is empty")
}

func TestHandleStopAll_KillFailureOnOneCard(t *testing.T) {
	tr := tracker.New()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "A-001", Project: "alpha"}))
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "A-002", Project: "alpha"}))
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "A-003", Project: "alpha"}))

	fake := &stopAllFakeRunner{
		failFor: map[string]bool{"alpha/A-002": true},
		killErr: fmt.Errorf("simulated kill failure"),
	}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/stop-all", StopAllPayload{})
	h.hmacAuth(h.handleStopAll)(w, req)

	// 207 Multi-Status when any per-card kill fails.
	require.Equal(t, http.StatusMultiStatus, w.Code)

	var resp StopAllResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK, "OK must be false when any per-card kill failed")
	assert.Equal(t, 3, resp.Total)
	assert.Equal(t, 2, resp.Stopped)
	assert.Equal(t, 1, resp.Failed)
	require.Len(t, resp.Results, 3)

	// Inspect per-card results.
	perCard := make(map[string]CardKillResult, len(resp.Results))
	for _, r := range resp.Results {
		perCard[r.Project+"/"+r.CardID] = r
	}

	assert.True(t, perCard["alpha/A-001"].OK)
	assert.False(t, perCard["alpha/A-002"].OK)
	assert.NotEmpty(t, perCard["alpha/A-002"].Error, "failed entry must carry an error label")
	assert.True(t, perCard["alpha/A-003"].OK)

	killed := fake.killedIDs()
	assert.ElementsMatch(t, []string{"alpha/A-001", "alpha/A-003"}, killed,
		"only non-failing cards should appear in killed list")
}

// TestHandleStopAll_ChatOnly verifies that chat-mode tracker entries are
// dispatched through KillChat (not Kill, which would miss them because the
// chat lookup key is chatKey(sessionID) and CardID is empty).
func TestHandleStopAll_ChatOnly(t *testing.T) {
	tr := tracker.New()
	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{SessionID: "sess-A"}))
	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{SessionID: "sess-B"}))

	fake := &stopAllFakeRunner{}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL,
		slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/stop-all", StopAllPayload{})
	h.hmacAuth(h.handleStopAll)(w, req)

	require.Equal(t, http.StatusOK, w.Code, "all chat kills succeeded; expect 200")

	var resp StopAllResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Equal(t, 2, resp.Total)
	assert.Equal(t, 2, resp.Stopped)
	assert.Equal(t, 0, resp.Failed)

	// stopAllFakeRunner.KillChat records "session:<id>"; Kill records
	// "<project>/<card_id>". Chat-only run must dispatch only via KillChat.
	killed := fake.killedIDs()
	assert.ElementsMatch(t, []string{"session:sess-A", "session:sess-B"}, killed,
		"chat-mode entries must route through KillChat, not Kill")

	// Each result entry must carry SessionID (not CardID).
	for _, r := range resp.Results {
		assert.NotEmpty(t, r.SessionID, "chat-mode result must populate session_id")
		assert.Empty(t, r.CardID, "chat-mode result must not populate card_id")
		assert.True(t, r.OK)
	}
}

// TestHandleStopAll_MixedCardAndChat verifies that a /stop-all batch
// containing both card-mode and chat-mode tracker entries dispatches each
// to the right method and reports both kinds in the per-entry results.
func TestHandleStopAll_MixedCardAndChat(t *testing.T) {
	tr := tracker.New()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{CardID: "CARD-A", Project: "proj"}))
	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{SessionID: "sess-A"}))

	fake := &stopAllFakeRunner{}
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testMCPURL,
		slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/stop-all", StopAllPayload{})
	h.hmacAuth(h.handleStopAll)(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp StopAllResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, 2, resp.Stopped)
	assert.Equal(t, 0, resp.Failed)

	killed := fake.killedIDs()
	assert.ElementsMatch(t, []string{"proj/CARD-A", "session:sess-A"}, killed,
		"mixed batch must dispatch card via Kill and chat via KillChat")

	// Confirm result entries carry the right identifier per kind.
	gotCard := false
	gotChat := false

	for _, r := range resp.Results {
		switch {
		case r.CardID != "":
			gotCard = true

			assert.Equal(t, "CARD-A", r.CardID)
			assert.Equal(t, "proj", r.Project)
		case r.SessionID != "":
			gotChat = true

			assert.Equal(t, "sess-A", r.SessionID)
		}
	}

	assert.True(t, gotCard, "expected a card_id entry in Results")
	assert.True(t, gotChat, "expected a session_id entry in Results")
}

func TestHandleStopAll_InvalidJSON(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(&stopAllFakeRunner{}, tr, nil, nil, testAPIKey, 10, testMCPURL, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

	body := []byte("not-json")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/stop-all", body, ts)
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/stop-all", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)

	w := httptest.NewRecorder()
	h.hmacAuth(h.handleStopAll)(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestHandleMessage_ConcurrentNoWriteInterleave fires many concurrent signed
// /message requests against one container's stdin and verifies none of the
// stream-json lines get interleaved or truncated. The tracker's per-entry
// stdin mutex is the serialisation point — if a handler change ever bypassed
// it, the race detector would catch it here.
func TestHandleMessage_ConcurrentNoWriteInterleave(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testMCPURL, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	// A blocking writer that serializes each Write behind a mutex but also
	// records them so we can parse and count whole lines afterwards.
	fw := &concurrentFakeWriter{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /message", h.hmacAuth(h.handleMessage))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	const concurrency = 25

	var wg sync.WaitGroup

	wg.Add(concurrency)

	for i := range concurrency {
		go func(i int) {
			defer wg.Done()

			payload := MessagePayload{
				CardID:    "PROJ-001",
				Project:   "my-project",
				Content:   "message-" + strconv.Itoa(i),
				MessageID: "msg-" + strconv.Itoa(i),
			}

			body, err := json.Marshal(payload)
			if err != nil {
				t.Errorf("marshal: %v", err)

				return
			}

			ts := strconv.FormatInt(time.Now().Unix(), 10)
			sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/message", body, ts)

			req, err := http.NewRequestWithContext(context.Background(), "POST", srv.URL+"/message", strings.NewReader(string(body)))
			if err != nil {
				t.Errorf("new request: %v", err)

				return
			}

			req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
			req.Header.Set(cmhmac.TimestampHeader, ts)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("do: %v", err)

				return
			}

			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusAccepted {
				t.Errorf("status: %d", resp.StatusCode)
			}
		}(i)
	}

	wg.Wait()

	lines := fw.lines()
	require.Len(t, lines, concurrency, "exactly one stream-json line per request")

	// Each captured line must be valid JSON with the expected content prefix.
	seen := make(map[string]bool)

	for _, line := range lines {
		var msg streammsg.UserMessage
		require.NoError(t, json.Unmarshal([]byte(line), &msg), "each captured line must be valid stream-json")
		require.Len(t, msg.Message.Content, 1)

		txt := msg.Message.Content[0].Text
		assert.True(t, strings.HasPrefix(txt, "message-"), "content must be a full 'message-N' payload, got %q", txt)
		seen[txt] = true
	}

	assert.Len(t, seen, concurrency, "every message index must be represented exactly once")
}

// concurrentFakeWriter is an io.WriteCloser that appends each Write to a
// slice under a mutex so callers can inspect the ordering of complete writes.
type concurrentFakeWriter struct {
	mu     sync.Mutex
	writes [][]byte
}

func (c *concurrentFakeWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	buf := make([]byte, len(p))
	copy(buf, p)
	c.writes = append(c.writes, buf)

	return len(p), nil
}

func (c *concurrentFakeWriter) Close() error { return nil }

func (c *concurrentFakeWriter) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]string, 0, len(c.writes))

	for _, w := range c.writes {
		s := string(w)
		// Strip the trailing newline that BuildUserMessage always appends.
		out = append(out, strings.TrimSuffix(s, "\n"))
	}

	return out
}

func TestHandleEndSession_401_InvalidHMAC(t *testing.T) {
	h, _, _ := setupEndSessionHandler(t, true)

	body, _ := json.Marshal(EndSessionPayload{CardID: "PROJ-001", Project: "my-project"})
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/end-session", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256=badhash")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))

	w := httptest.NewRecorder()
	h.hmacAuth(h.handleEndSession)(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Drain / 503 tests ---
//
// When the shutdown sequence flips health.Draining to true, every handler
// that starts or extends long-running work must short-circuit to 503 so we
// don't start containers or stdin writes we're about to tear down.

func TestHandleTrigger_503WhenDraining(t *testing.T) {
	tr := tracker.New()
	health := NewHealthState()
	health.Draining.Store(true)

	h := NewHandler(testManager(tr), tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, health)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
		CardID:  "PROJ-777",
		Project: "my-project",
		RepoURL: "https://github.com/org/repo.git",
	})
	h.hmacAuth(h.handleTrigger)(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp ErrorResponse

	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Message, "draining")

	// Tracker must remain empty — the draining branch runs before AddIfUnderLimit.
	assert.Equal(t, 0, tr.Count())
}

func TestHandleMessage_503WhenDraining(t *testing.T) {
	tr := tracker.New()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	health := NewHealthState()
	health.Draining.Store(true)

	h := NewHandler(nil, tr, logbroadcast.NewBroadcaster(nil, nil), nil, testAPIKey, 3, testMCPURL, nil, 0, health)

	payload := MessagePayload{
		CardID:    "PROJ-001",
		Project:   "my-project",
		Content:   "hello",
		MessageID: "msg-drain-1",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	// No stdin write should have landed on the attached writer.
	assert.Empty(t, fw.buf, "draining branch must short-circuit before any stdin write")
}

func TestHandlePromote_503WhenDraining(t *testing.T) {
	tr := tracker.New()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	health := NewHealthState()
	health.Draining.Store(true)

	h := NewHandler(nil, tr, logbroadcast.NewBroadcaster(nil, nil), nil, testAPIKey, 3, testMCPURL, nil, 0, health)

	payload := PromotePayload{
		CardID:  "PROJ-001",
		Project: "my-project",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/promote", payload)
	h.hmacAuth(h.handlePromote)(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Empty(t, fw.buf, "draining branch must short-circuit before any stdin write")
}

func TestHandleEndSession_503WhenDraining(t *testing.T) {
	tr := tracker.New()
	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	health := NewHealthState()
	health.Draining.Store(true)

	h := NewHandler(nil, tr, logbroadcast.NewBroadcaster(nil, nil), nil, testAPIKey, 3, testMCPURL, nil, 0, health)

	payload := EndSessionPayload{
		CardID:  "PROJ-001",
		Project: "my-project",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/end-session", payload)
	h.hmacAuth(h.handleEndSession)(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.False(t, fw.closed, "draining branch must short-circuit before any stdin close")
}

// --- /refresh-knowledge handler tests ---

func TestHandleRefreshKnowledge_AcceptsValidPayload(t *testing.T) {
	tr := tracker.New()
	fake := newFakeRunner()
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	payload := RefreshKnowledgePayload{
		Project:       "my-project",
		Repo:          "my-repo",
		RepoURL:       "https://github.com/org/my-repo.git",
		BaseBranch:    "main",
		AgentID:       "human:test",
		OverwriteDocs: []string{"api-documentation.md"},
		RunnerImage:   "runner:latest",
		Model:         "claude-opus-4-5",
	}

	w := httptest.NewRecorder()
	req := signedRequest(t, "/refresh-knowledge", payload)
	h.hmacAuth(h.handleRefreshKnowledge)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	select {
	case cfg := <-fake.runCh:
		assert.Equal(t, container.ModeKnowledgeRefresh, cfg.Mode)
		assert.Equal(t, "my-project", cfg.Project)
		assert.Equal(t, "my-repo", cfg.KBRepo)
		assert.Equal(t, "https://github.com/org/my-repo.git", cfg.RepoURL)
		assert.Equal(t, "human:test", cfg.AgentID)
		assert.Equal(t, []string{"api-documentation.md"}, cfg.OverwriteDocs)
		assert.Equal(t, testMCPURL, cfg.MCPURL)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Run to be called")
	}
}

func TestHandleRefreshKnowledge_RejectsInvalidPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload RefreshKnowledgePayload
	}{
		{
			name:    "missing project",
			payload: RefreshKnowledgePayload{Repo: "r", RepoURL: "https://example.com/r.git", AgentID: "human:x"},
		},
		{
			name:    "missing repo",
			payload: RefreshKnowledgePayload{Project: "p", RepoURL: "https://example.com/r.git", AgentID: "human:x"},
		},
		{
			name:    "missing repo_url",
			payload: RefreshKnowledgePayload{Project: "p", Repo: "r", AgentID: "human:x"},
		},
		{
			name:    "non-https repo_url",
			payload: RefreshKnowledgePayload{Project: "p", Repo: "r", RepoURL: "ssh://example.com/r.git", AgentID: "human:x"},
		},
		{
			name:    "missing human: prefix on agent_id",
			payload: RefreshKnowledgePayload{Project: "p", Repo: "r", RepoURL: "https://example.com/r.git", AgentID: "bot:x"},
		},
	}

	tr := tracker.New()
	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := signedRequest(t, "/refresh-knowledge", tc.payload)
			h.hmacAuth(h.handleRefreshKnowledge)(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code, tc.name)

			var resp ErrorResponse
			require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
			assert.False(t, resp.OK)
		})
	}
}

func TestHandleRefreshKnowledge_503WhenDraining(t *testing.T) {
	health := NewHealthState()
	health.Draining.Store(true)

	h := NewHandler(nil, tracker.New(), nil, nil, testAPIKey, 3, testMCPURL, nil, 0, health)

	payload := RefreshKnowledgePayload{
		Project: "p",
		Repo:    "r",
		RepoURL: "https://example.com/r.git",
		AgentID: "human:x",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/refresh-knowledge", payload)
	h.hmacAuth(h.handleRefreshKnowledge)(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, CodeDraining, resp.Code)
}

func TestHandleRefreshKnowledge_429WhenAtLimit(t *testing.T) {
	tr := tracker.New()
	_ = tr.Add(&tracker.ContainerInfo{CardID: "BUSY-1", Project: "proj"})

	h := NewHandler(&noopRunner{}, tr, nil, nil, testAPIKey, 1, testMCPURL, nil, 0, nil)

	payload := RefreshKnowledgePayload{
		Project: "proj",
		Repo:    "my-repo",
		RepoURL: "https://github.com/org/my-repo.git",
		AgentID: "human:test",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/refresh-knowledge", payload)
	h.hmacAuth(h.handleRefreshKnowledge)(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestHandleRefreshKnowledge_409WhenAlreadyRunning(t *testing.T) {
	tr := tracker.New()
	_ = tr.Add(&tracker.ContainerInfo{
		CardID:  "kb-refresh:my-repo",
		Project: "proj",
	})

	h := NewHandler(&noopRunner{}, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	payload := RefreshKnowledgePayload{
		Project: "proj",
		Repo:    "my-repo",
		RepoURL: "https://github.com/org/my-repo.git",
		AgentID: "human:test",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/refresh-knowledge", payload)
	h.hmacAuth(h.handleRefreshKnowledge)(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

// --- /message chat path tests ---

// setupChatMessageHandler builds a Handler with a tracker that has a chat
// container registered (optionally with stdin attached) and a broadcaster.
func setupChatMessageHandler(t *testing.T, withStdin bool) (*Handler, *logbroadcast.Broadcaster, *fakeWriteCloser) {
	t.Helper()

	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	require.NoError(t, tr.AddChat(&tracker.ContainerInfo{
		SessionID: "sess-chat-001",
	}))

	var fw *fakeWriteCloser
	if withStdin {
		fw = &fakeWriteCloser{}
		tr.SetStdinChat("sess-chat-001", fw, nil)
	}

	return h, b, fw
}

// TestMessage_ChatPath_Success verifies that a /message with session_id
// (and no card_id/project) returns 202 and writes to the chat container stdin.
func TestMessage_ChatPath_Success(t *testing.T) {
	h, b, fw := setupChatMessageHandler(t, true)

	ch, unsub := b.SubscribeWithSessionID("sess-chat-001")
	defer unsub()

	payload := MessagePayload{
		SessionID: "sess-chat-001",
		Content:   "hello from chat",
		MessageID: "msg-chat-001",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	var resp SuccessResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "msg-chat-001", resp.MessageID)

	// Stdin must have been written.
	require.NotNil(t, fw)
	require.NotEmpty(t, fw.buf)

	// Broadcaster must have a "user" entry with session_id set.
	select {
	case entry := <-ch:
		assert.Equal(t, "user", entry.Type)
		assert.Equal(t, "sess-chat-001", entry.SessionID)
		assert.Equal(t, "hello from chat", entry.Content)
		assert.Empty(t, entry.CardID, "chat entry must not carry card_id")
		assert.Empty(t, entry.Project, "chat entry must not carry project")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for chat broadcaster entry")
	}
}

// TestMessage_ChatPath_NotFound verifies that /message with session_id for an
// untracked session returns 404.
func TestMessage_ChatPath_NotFound(t *testing.T) {
	h, _, _ := setupChatMessageHandler(t, false)

	payload := MessagePayload{
		SessionID: "sess-unknown",
		Content:   "hello",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Equal(t, CodeNotFound, resp.Code)
}

// TestMessage_ChatPath_Validation verifies that /message with both session_id
// and card_id returns 400.
func TestMessage_ChatPath_Validation(t *testing.T) {
	h, _, _ := setupChatMessageHandler(t, false)

	payload := MessagePayload{
		SessionID: "sess-chat-001",
		CardID:    "PROJ-001",
		Content:   "hello",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Equal(t, CodeInvalidField, resp.Code)
}

// TestMessage_ChatPath_NoStdin verifies that /message with a tracked session
// that has no stdin attached returns 409.
func TestMessage_ChatPath_NoStdin(t *testing.T) {
	h, b, _ := setupChatMessageHandler(t, false)

	ch, unsub := b.Subscribe("")
	defer unsub()

	payload := MessagePayload{
		SessionID: "sess-chat-001",
		Content:   "hello",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Equal(t, CodeConflict, resp.Code)

	// No phantom echo.
	select {
	case entry := <-ch:
		t.Fatalf("expected no broadcast on 409, got entry: %+v", entry)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestMessage_ChatPath_UnknownToolUseIDFieldIgnored verifies that a /message
// POST carrying a `tool_use_id` field (left over from older CM deployments)
// is silently dropped — the field was removed when AskUserQuestion routing
// moved to the MCP permission_prompt gate; payloads with it must still
// produce a plain user-text stdin frame so a rolling upgrade does not break
// chat. encoding/json ignores unknown object keys by default, so the
// regression we guard against here is a future contributor re-adding the
// field to MessagePayload without thinking through Phase 2.
func TestMessage_ChatPath_UnknownToolUseIDFieldIgnored(t *testing.T) {
	h, _, fw := setupChatMessageHandler(t, true)

	rawBody := []byte(`{"session_id":"sess-chat-001","content":"plain message","message_id":"msg-unknown-001","tool_use_id":"toolu_legacy"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/message", rawBody, ts)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/message", bytes.NewReader(rawBody))
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)

	w := httptest.NewRecorder()
	h.hmacAuth(h.handleMessage)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	require.NotNil(t, fw)
	require.NotEmpty(t, fw.buf)

	got := string(fw.buf)
	assert.Contains(t, got, `"type":"text"`,
		"stale tool_use_id field must be ignored; stdin frame must be plain user text")
	assert.NotContains(t, got, `"tool_result"`,
		"stale tool_use_id field must NOT resurrect the deleted tool_result injection path")
	assert.NotContains(t, got, `"tool_use_id"`,
		"stdin frame must not echo any tool_use_id back to claude")
}

// TestLogs_SessionIDFilter verifies that a ?session_id= subscription only
// receives entries with a matching SessionID (and not project-keyed entries).
func TestLogs_SessionIDFilter(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", h.hmacAuth(h.handleLogs))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodGet, "/logs?session_id=sess-filter", []byte{}, ts)

	req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/logs?session_id=sess-filter", nil)
	require.NoError(t, err)
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)

	client := &http.Client{}
	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	scanner := bufio.NewScanner(resp.Body)

	// Consume the initial connected comment.
	for scanner.Scan() {
		if scanner.Text() == ": connected" {
			break
		}
	}

	// Publish one entry for a different session and one for the target session.
	b.Publish(logbroadcast.LogEntry{SessionID: "sess-other", Type: "text", Content: "should not arrive"})
	b.Publish(logbroadcast.LogEntry{SessionID: "sess-filter", Type: "text", Content: "should arrive"})

	lineCh := make(chan string, 16)

	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				lineCh <- line

				return
			}
		}
	}()

	select {
	case dataLine := <-lineCh:
		var got logbroadcast.LogEntry
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(dataLine, "data: ")), &got))
		assert.Equal(t, "sess-filter", got.SessionID, "expected only the target session entry")
		assert.Equal(t, "should arrive", got.Content)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for session-filtered SSE event")
	}
}

// TestLogs_SessionIDAndProjectMutuallyExclusive verifies that providing both
// ?session_id= and ?project= returns 400.
func TestLogs_SessionIDAndProjectMutuallyExclusive(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodGet, "/logs?project=p&session_id=s", []byte{}, ts)

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/logs?project=p&session_id=s", nil)
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)

	w := newFlushRecorder()
	h.hmacAuth(h.handleLogs)(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Equal(t, CodeInvalidField, resp.Code)
}
