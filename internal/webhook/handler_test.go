package webhook

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/container"
	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

func testManager(tr *tracker.Tracker) *container.Manager {
	cfg := &config.Config{ContainerTimeout: "1h"}
	cfg.ParseContainerTimeout()
	return container.NewManager(nil, tr, nil, nil, nil, cfg, nil)
}


const testAPIKey = "test-api-key-that-is-at-least-32-chars"

func signedRequest(t *testing.T, method, url string, payload any) *http.Request {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, ts)

	req := httptest.NewRequest(method, url, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)
	return req
}

func TestHmacAuth_MissingSignature(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}
	handler := h.hmacAuth(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test", strings.NewReader("{}"))
	handler(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestHmacAuth_MissingTimestamp(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}
	handler := h.hmacAuth(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test", strings.NewReader("{}"))
	req.Header.Set(cmhmac.SignatureHeader, "sha256=abc")
	handler(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestHmacAuth_InvalidSignature(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}
	handler := h.hmacAuth(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test", strings.NewReader("{}"))
	req.Header.Set(cmhmac.SignatureHeader, "sha256=invalid")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))
	handler(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
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
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, ts)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)
	handler(w, req)

	assert.True(t, called)
}

func TestHandleTrigger_MissingFields(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(nil, tr, nil, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "POST", "/trigger", map[string]string{"card_id": "A-001"})
	h.hmacAuth(h.handleTrigger)(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var resp Response
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
}

func TestHandleTrigger_ConcurrencyLimit(t *testing.T) {
	tr := tracker.New()
	// Fill up the tracker.
	for i := range 3 {
		_ = tr.Add(&tracker.ContainerInfo{
			CardID:  "EXIST-" + itoa(i),
			Project: "proj",
		})
	}

	h := NewHandler(nil, tr, nil, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "POST", "/trigger", TriggerPayload{
		CardID:  "NEW-001",
		Project: "proj",
		RepoURL: "git@github.com:org/repo.git",
		MCPURL:  "http://cm:8080/mcp",
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

	h := NewHandler(nil, tr, nil, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "POST", "/trigger", TriggerPayload{
		CardID:     "PROJ-042",
		Project:    "my-project",
		RepoURL:    "git@github.com:org/repo.git",
		MCPURL:     "http://cm:8080/mcp",
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

	h := NewHandler(nil, tr, nil, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "POST", "/trigger", TriggerPayload{
		CardID:     "PROJ-200",
		Project:    "my-project",
		RepoURL:    "git@github.com:org/repo.git",
		MCPURL:     "http://cm:8080/mcp",
		BaseBranch: "develop",
	})
	h.hmacAuth(h.handleTrigger)(w, req)

	// 409 Conflict means the payload was parsed correctly (base_branch did not
	// cause a 400) and the duplicate check fired as expected.
	assert.Equal(t, http.StatusConflict, w.Code)
	var resp Response
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.NotContains(t, resp.Error, "invalid JSON")
}

func TestHandleKill_NotFound(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(testManager(tr), tr, nil, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "POST", "/kill", KillPayload{
		CardID:  "PROJ-999",
		Project: "proj",
	})
	h.hmacAuth(h.handleKill)(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandleHealth(t *testing.T) {
	tr := tracker.New()
	_ = tr.Add(&tracker.ContainerInfo{CardID: "A-001", Project: "proj"})

	h := NewHandler(nil, tr, nil, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	h.handleHealth(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, true, resp["ok"])
	assert.Equal(t, float64(1), resp["running_containers"])
}

// signedGETRequest builds a signed GET request with an empty body.
// The HMAC is computed over timestamp + "." + "" (empty body).
func signedGETRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, []byte{}, ts)

	req := httptest.NewRequest("GET", url, nil)
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
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tracker.New(), b, testAPIKey, 3, nil)

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
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tracker.New(), b, testAPIKey, 3, nil)

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
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tracker.New(), b, testAPIKey, 3, nil)

	// Use an httptest.Server so we have a real connection with proper flushing.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", h.hmacAuth(h.handleLogs))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, []byte{}, ts)

	req, err := http.NewRequest("GET", srv.URL+"/logs", nil)
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
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tracker.New(), b, testAPIKey, 3, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", h.hmacAuth(h.handleLogs))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, []byte{}, ts)

	// Subscribe only to "alpha" project.
	req, err := http.NewRequest("GET", srv.URL+"/logs?project=alpha", nil)
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
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tracker.New(), b, testAPIKey, 3, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", h.hmacAuth(h.handleLogs))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, []byte{}, ts)

	req, err := http.NewRequest("GET", srv.URL+"/logs", nil)
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
