package webhook

import (
	"bufio"
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

	"github.com/mhersson/contextmatrix-runner/internal/container"
	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

func testManager(tr *tracker.Tracker) *container.Manager {
	return container.NewManager(nil, tr, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

// TestHmacAuth_BodyOverCapReturns413 verifies the runner-side body cap
// returns 413 with CodeTooLarge when ContentLength exceeds the limit, rather
// than silently truncating and surfacing as a 401 signature mismatch (which
// would be indistinguishable from a wrong key from the caller's POV).
func TestHmacAuth_BodyOverCapReturns413(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}
	handler := h.hmacAuth(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called when body exceeds cap")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/trigger", strings.NewReader(""))
	req.Header.Set(cmhmac.SignatureHeader, "sha256=anything")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))
	req.ContentLength = (1 << 20) + 1 // 1 MiB + 1 byte

	handler(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)

	var resp ErrorResponse

	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, CodeTooLarge, resp.Code)
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
// 200 OK (CTXRUN-056 / C8). The old behaviour was 404, which made retry logic
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
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodGet, "/logs", []byte{}, ts)

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

// --- /stop-all handler tests ---

// stopAllFakeRunner is a ContainerOps double that records every Kill call
// and can be configured to fail for a specific card ID. Safe for concurrent
// use.
type stopAllFakeRunner struct {
	mu      sync.Mutex
	killed  []string
	failFor map[string]bool
	killErr error
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

	// CTXRUN-056 / M40: 207 Multi-Status when any per-card kill fails.
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

type fakeDispatcher struct {
	called   chan TriggerPayload
	wantErr  error
	complete chan struct{}
}

func newFakeDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		called:   make(chan TriggerPayload, 1),
		complete: make(chan struct{}, 1),
	}
}

func (d *fakeDispatcher) Dispatch(_ context.Context, p TriggerPayload, cancel context.CancelFunc, onComplete func()) error {
	d.called <- p

	if d.wantErr != nil {
		// Caller is responsible for invoking cancel when an error is
		// returned from Dispatch (handler does this), so we don't
		// invoke it here.
		return d.wantErr
	}
	// Background "driver"; cancel the ctx and fire the completion hook
	// so the tracker entry is dropped — mirrors the real dispatcher's
	// goroutine contract.
	go func() {
		cancel()

		if onComplete != nil {
			onComplete()
		}

		select {
		case d.complete <- struct{}{}:
		default:
		}
	}()

	return nil
}

func TestHandleTrigger_RoutesToOrchestrated(t *testing.T) {
	tr := tracker.New()
	disp := newFakeDispatcher()

	h := NewHandler(&noopRunner{}, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)
	h.SetOrchestratedDispatcher(disp)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
		CardID:    "PROJ-501",
		Project:   "my-project",
		RepoURL:   "https://github.com/org/repo.git",
		MCPAPIKey: "k",
	})
	h.hmacAuth(h.handleTrigger)(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	select {
	case got := <-disp.called:
		assert.Equal(t, "PROJ-501", got.CardID)
		assert.Equal(t, "my-project", got.Project)
	case <-time.After(2 * time.Second):
		t.Fatal("orchestrated Dispatch was not called")
	}
}

// TestHandleTrigger_OrchestratedCompletionClearsTracker exercises the
// full goroutine lifecycle: after the dispatcher's onComplete fires the
// tracker entry must be dropped so the same card can be re-triggered
// without a runner restart.
func TestHandleTrigger_OrchestratedCompletionClearsTracker(t *testing.T) {
	tr := tracker.New()
	disp := newFakeDispatcher()

	h := NewHandler(&noopRunner{}, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)
	h.SetOrchestratedDispatcher(disp)

	// First trigger.
	w1 := httptest.NewRecorder()
	req1 := signedRequest(t, "/trigger", TriggerPayload{
		CardID:    "PROJ-503",
		Project:   "my-project",
		RepoURL:   "https://github.com/org/repo.git",
		MCPAPIKey: "k",
	})
	h.hmacAuth(h.handleTrigger)(w1, req1)
	require.Equal(t, http.StatusAccepted, w1.Code)

	// Drain the called channel; the dispatcher's goroutine is blocked
	// on its send until we do.
	select {
	case <-disp.called:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher Dispatch was not called for first trigger")
	}

	// Wait for the background "driver" to fire onComplete so the
	// tracker entry has been dropped.
	select {
	case <-disp.complete:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher onComplete did not fire")
	}

	require.Equal(t, 0, tr.Count(), "tracker entry must be dropped after completion")

	// Second trigger of the same card must NOT 409.
	w2 := httptest.NewRecorder()
	req2 := signedRequest(t, "/trigger", TriggerPayload{
		CardID:    "PROJ-503",
		Project:   "my-project",
		RepoURL:   "https://github.com/org/repo.git",
		MCPAPIKey: "k",
	})
	h.hmacAuth(h.handleTrigger)(w2, req2)
	require.Equal(t, http.StatusAccepted, w2.Code)
}

func TestHandleTrigger_OrchestratedDispatchFailureClearsTracker(t *testing.T) {
	tr := tracker.New()
	disp := newFakeDispatcher()
	disp.wantErr = fmt.Errorf("docker daemon down")

	h := NewHandler(&noopRunner{}, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)
	h.SetOrchestratedDispatcher(disp)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
		CardID:    "PROJ-502",
		Project:   "my-project",
		RepoURL:   "https://github.com/org/repo.git",
		MCPAPIKey: "k",
	})
	h.hmacAuth(h.handleTrigger)(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	// Tracker must be cleared so a retry is not blocked by 409 conflict.
	assert.Equal(t, 0, tr.Count())
}
