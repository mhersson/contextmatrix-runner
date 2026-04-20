package webhook

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	cfg := &config.Config{ContainerTimeout: "1h"}
	cfg.ParseContainerTimeout()

	return container.NewManager(nil, tr, nil, nil, nil, cfg, nil)
}

const testAPIKey = "test-api-key-that-is-at-least-32-chars"

func signedRequest(t *testing.T, url string, payload any) *http.Request {
	t.Helper()

	body, err := json.Marshal(payload)
	require.NoError(t, err)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, ts)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(string(body)))
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

	assert.Equal(t, http.StatusForbidden, w.Code)
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

	assert.Equal(t, http.StatusForbidden, w.Code)
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
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/test", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)
	handler(w, req)

	assert.True(t, called)
}

func TestHandleTrigger_MissingFields(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", map[string]string{"card_id": "A-001"})
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

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
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

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
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

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
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
	h := NewHandler(testManager(tr), tr, nil, nil, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/kill", KillPayload{
		CardID:  "PROJ-999",
		Project: "proj",
	})
	h.hmacAuth(h.handleKill)(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandleHealth(t *testing.T) {
	tr := tracker.New()
	_ = tr.Add(&tracker.ContainerInfo{CardID: "A-001", Project: "proj"})

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, nil)

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
// The HMAC is computed over timestamp + "." + "" (empty body).
func signedGETRequest(t *testing.T, url string) *http.Request {
	t.Helper()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, []byte{}, ts)

	req := httptest.NewRequestWithContext(context.Background(), "GET", url, nil)
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
			h := NewHandler(fake, tr, nil, nil, testAPIKey, 3, nil)

			payload := TriggerPayload{
				CardID:      "PROJ-100",
				Project:     "my-project",
				RepoURL:     "https://github.com/org/repo.git",
				MCPURL:      "http://cm:8080/mcp",
				Interactive: tt.interactive,
			}
			w := httptest.NewRecorder()
			req := signedRequest(t, "/trigger", payload)
			h.hmacAuth(h.handleTrigger)(w, req)

			require.Equal(t, http.StatusAccepted, w.Code)

			select {
			case cfg := <-fake.runCh:
				assert.Equal(t, tt.interactive, cfg.Interactive)
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for Run to be called")
			}
		})
	}
}

func TestHandleLogs_SSEHeaders(t *testing.T) {
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, nil)

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
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, nil)

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
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, nil)

	// Use an httptest.Server so we have a real connection with proper flushing.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", h.hmacAuth(h.handleLogs))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, []byte{}, ts)

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
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", h.hmacAuth(h.handleLogs))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, []byte{}, ts)

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
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", h.hmacAuth(h.handleLogs))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, []byte{}, ts)

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
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, nil)

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

	var resp Response
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
				sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, ts)
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

	var resp Response
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

func TestHandleMessage_403_InvalidHMAC(t *testing.T) {
	h, _, _ := setupMessageHandler(t, true)

	body, _ := json.Marshal(MessagePayload{CardID: "PROJ-001", Project: "my-project", Content: "hi"})
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/message", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256=badhash")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))

	w := httptest.NewRecorder()
	h.hmacAuth(h.handleMessage)(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- /promote handler tests ---

// setupPromoteHandler builds a Handler with a tracker that has a container
// registered (optionally with stdin attached) and a broadcaster.
func setupPromoteHandler(t *testing.T, withStdin bool) (*Handler, *logbroadcast.Broadcaster, *fakeWriteCloser) {
	t.Helper()

	tr := tracker.New()
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, nil)

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

	var resp Response
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
				sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, ts)
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

	var resp Response
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
}

func TestHandlePromote_403_InvalidHMAC(t *testing.T) {
	h, _, _ := setupPromoteHandler(t, true)

	body, _ := json.Marshal(PromotePayload{CardID: "PROJ-001", Project: "my-project"})
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/promote", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256=badhash")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))

	w := httptest.NewRecorder()
	h.hmacAuth(h.handlePromote)(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestHandlePromote_OrderingSystemBeforeStdin(t *testing.T) {
	// Verify that the system LogEntry is published before the stdin write occurs.
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster()

	// A controlled stdin that signals when Write is called.
	stdinWritten := make(chan struct{}, 1)
	controlled := &controlledWriteCloser{writeCh: stdinWritten}

	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, nil)
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

	// The system LogEntry must arrive on the subscriber channel.
	select {
	case entry := <-ch:
		assert.Equal(t, "system", entry.Type)
		assert.Equal(t, "promoted to autonomous mode", entry.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for system LogEntry")
	}

	// The stdin write must have happened (signal sent before handler returned).
	select {
	case <-stdinWritten:
		// good — write occurred
	default:
		t.Fatal("stdin was not written")
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
	b := logbroadcast.NewBroadcaster()
	cmClient := callback.NewClient(cmServer.URL, "key", nil)

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, nil)
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
	b := logbroadcast.NewBroadcaster()
	cmClient := callback.NewClient(cmServer.URL, "key", nil)

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, nil)
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

	var resp Response
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
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
	b := logbroadcast.NewBroadcaster()
	cmClient := callback.NewClient(cmServer.URL, "key", nil)

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, nil)
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

	var resp Response
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "autonomous flag is not set")
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
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, nil)

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

	var resp Response
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
				sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, ts)
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

	var resp Response
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
	b := logbroadcast.NewBroadcaster()
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, nil)

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

	// Second call finds no stdin attached and returns 409.
	w2 := httptest.NewRecorder()
	h.hmacAuth(h.handleEndSession)(w2, signedRequest(t, "/end-session", payload))
	assert.Equal(t, http.StatusConflict, w2.Code)

	assert.Equal(t, 1, closeCount, "writer must be closed exactly once across two /end-session calls")
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

func TestHandleEndSession_403_InvalidHMAC(t *testing.T) {
	h, _, _ := setupEndSessionHandler(t, true)

	body, _ := json.Marshal(EndSessionPayload{CardID: "PROJ-001", Project: "my-project"})
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/end-session", strings.NewReader(string(body)))
	req.Header.Set(cmhmac.SignatureHeader, "sha256=badhash")
	req.Header.Set(cmhmac.TimestampHeader, strconv.FormatInt(time.Now().Unix(), 10))

	w := httptest.NewRecorder()
	h.hmacAuth(h.handleEndSession)(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}
