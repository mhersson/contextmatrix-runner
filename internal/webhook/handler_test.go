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

// testAllowedMCPHosts is the default MCP host allowlist used by handler
// tests. Keep in sync with the `MCPURL` literals used in request payloads.
var testAllowedMCPHosts = []string{"cm.example.com"}

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
	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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
			CardID:  "EXIST-" + strconv.Itoa(i),
			Project: "proj",
		})
	}

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
		CardID:  "NEW-001",
		Project: "proj",
		RepoURL: "https://github.com/org/repo.git",
		MCPURL:  "https://cm.example.com/mcp",
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

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
		CardID:     "PROJ-042",
		Project:    "my-project",
		RepoURL:    "https://github.com/org/repo.git",
		MCPURL:     "https://cm.example.com/mcp",
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

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
		CardID:     "PROJ-200",
		Project:    "my-project",
		RepoURL:    "https://github.com/org/repo.git",
		MCPURL:     "https://cm.example.com/mcp",
		BaseBranch: "develop",
	})
	h.hmacAuth(h.handleTrigger)(w, req)

	// 409 Conflict means the payload was parsed correctly (base_branch did not
	// cause a 400) and the duplicate check fired as expected.
	assert.Equal(t, http.StatusConflict, w.Code)

	var resp Response
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.NotContains(t, resp.Message, "invalid JSON")
}

// TestHandleKill_IdempotentWhenAlreadyStopped verifies that /kill on a card
// with no tracked container returns 200 OK (CTXRUN-056 / C8). The old
// behaviour was 404, which made retry logic in CM harder (a legitimate
// not-yet-started tracker miss was indistinguishable from a hard failure).
func TestHandleKill_IdempotentWhenAlreadyStopped(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(testManager(tr), tr, nil, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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

func TestHandleHealth(t *testing.T) {
	tr := tracker.New()
	_ = tr.Add(&tracker.ContainerInfo{CardID: "A-001", Project: "proj"})

	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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
			h := NewHandler(fake, tr, nil, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

			payload := TriggerPayload{
				CardID:      "PROJ-100",
				Project:     "my-project",
				RepoURL:     "https://github.com/org/repo.git",
				MCPURL:      "https://cm.example.com/mcp",
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
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tracker.New(), b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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

// setupPromoteHandler builds a Handler with a tracker that has a container
// registered (optionally with stdin attached) and a broadcaster.
func setupPromoteHandler(t *testing.T, withStdin bool) (*Handler, *logbroadcast.Broadcaster, *fakeWriteCloser) {
	t.Helper()

	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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

func TestHandlePromote_OrderingSystemBeforeStdin(t *testing.T) {
	// Verify that the system LogEntry is published before the stdin write occurs.
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)

	// A controlled stdin that signals when Write is called.
	stdinWritten := make(chan struct{}, 1)
	controlled := &controlledWriteCloser{writeCh: stdinWritten}

	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)
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
	b := logbroadcast.NewBroadcaster(nil, nil)
	cmClient := callback.NewClient(cmServer.URL, "key", nil)

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)
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

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)
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

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)
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

	h := NewHandler(nil, tr, b, cmClient, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)
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

	var resp Response
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
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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

	// /end-session on already-closed stdin returns 409 (idempotent, not a panic).
	endPayload := EndSessionPayload{CardID: "PROJ-001", Project: "my-project"}
	we := httptest.NewRecorder()
	h.hmacAuth(h.handleEndSession)(we, signedRequest(t, "/end-session", endPayload))
	assert.Equal(t, http.StatusConflict, we.Code, "/end-session after /promote must return 409")
}

func TestHandlePromote_WriteFailure_StdinNotClosed(t *testing.T) {
	// When the canned-message stdin write returns an error, /promote must NOT
	// close stdin and must return the existing error response.
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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

	var resp Response
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
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testAllowedMCPHosts, nil, nil, false)

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
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testAllowedMCPHosts, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, false)

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
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testAllowedMCPHosts, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, false)

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
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testAllowedMCPHosts, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, false)

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
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, testAllowedMCPHosts, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, false)

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
	h := NewHandler(&stopAllFakeRunner{}, tr, nil, nil, testAPIKey, 10, testAllowedMCPHosts, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, false)

	body := []byte("not-json")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, ts)
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
	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testAllowedMCPHosts, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, false)

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
			sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, ts)

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

// --- Drain / 503 tests (CTXRUN-040) ---
//
// When the shutdown sequence flips health.Draining to true, every handler
// that starts or extends long-running work must short-circuit to 503 so we
// don't start containers or stdin writes we're about to tear down.

func TestHandleTrigger_503WhenDraining(t *testing.T) {
	tr := tracker.New()
	health := NewHealthState()
	health.Draining.Store(true)

	h := NewHandler(testManager(tr), tr, nil, nil, testAPIKey, 3, testAllowedMCPHosts, nil, health, false)

	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", TriggerPayload{
		CardID:  "PROJ-777",
		Project: "my-project",
		RepoURL: "https://github.com/org/repo.git",
		MCPURL:  "https://cm.example.com/mcp",
	})
	h.hmacAuth(h.handleTrigger)(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp Response

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

	h := NewHandler(nil, tr, logbroadcast.NewBroadcaster(nil, nil), nil, testAPIKey, 3, testAllowedMCPHosts, nil, health, false)

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

	h := NewHandler(nil, tr, logbroadcast.NewBroadcaster(nil, nil), nil, testAPIKey, 3, testAllowedMCPHosts, nil, health, false)

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

	h := NewHandler(nil, tr, logbroadcast.NewBroadcaster(nil, nil), nil, testAPIKey, 3, testAllowedMCPHosts, nil, health, false)

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

// captureHandler is a minimal slog.Handler that records every INFO record's
// "host" attribute. It is used by the dev-mode host-logging tests so they can
// assert that INFO fires exactly once per new host, without relying on the
// global slog logger (which is not test-safe to mutate).
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *captureHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.records = append(c.records, r)

	return nil
}

func (c *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(_ string) slog.Handler      { return c }

// countDevMCPHostSeen returns the number of "dev profile: mcp host seen" INFO
// records that carry the given host value.
func (c *captureHandler) countDevMCPHostSeen(host string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	n := 0

	for _, r := range c.records {
		if r.Level != slog.LevelInfo || r.Message != "dev profile: mcp host seen" {
			continue
		}

		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "host" && a.Value.String() == host {
				n++
			}

			return true
		})
	}

	return n
}

// TestHandleTrigger_DevMode_MCPHostLogging verifies per-new-host INFO logging
// in dev mode. The handler must:
// - emit INFO for a host the first time it is seen
// - NOT emit INFO for the same host on subsequent requests
// - emit INFO again when a different host appears for the first time.
func TestHandleTrigger_DevMode_MCPHostLogging(t *testing.T) {
	ch := &captureHandler{}
	logger := slog.New(ch)

	// Use an empty allowlist and devMode=true so any https host is accepted.
	tr := tracker.New()
	fake := newFakeRunner()
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, nil, logger, nil, true)

	sendTrigger := func(t *testing.T, cardID, mcpURL string) {
		t.Helper()

		payload := TriggerPayload{
			CardID:  cardID,
			Project: "proj",
			RepoURL: "https://github.com/org/repo.git",
			MCPURL:  mcpURL,
		}
		w := httptest.NewRecorder()
		req := signedRequest(t, "/trigger", payload)
		h.hmacAuth(h.handleTrigger)(w, req)

		// Drain the run channel so the goroutine doesn't block.
		select {
		case <-fake.runCh:
		default:
		}
	}

	// First request for example.com — should log.
	sendTrigger(t, "CARD-1", "https://example.com/mcp")
	assert.Equal(t, 1, ch.countDevMCPHostSeen("example.com"),
		"first trigger for example.com must emit one INFO log")

	// Second request for the same host — must NOT log again.
	sendTrigger(t, "CARD-2", "https://example.com/mcp")
	assert.Equal(t, 1, ch.countDevMCPHostSeen("example.com"),
		"second trigger for same host must not emit duplicate INFO log")

	// Third request for a different host — should log once.
	sendTrigger(t, "CARD-3", "https://other.example.com/mcp")
	assert.Equal(t, 1, ch.countDevMCPHostSeen("other.example.com"),
		"first trigger for other.example.com must emit one INFO log")

	// example.com count is still 1 (not bumped by other host).
	assert.Equal(t, 1, ch.countDevMCPHostSeen("example.com"),
		"example.com count must remain 1 after other.example.com trigger")
}

// TestHandleTrigger_DevMode_EmptyAllowlist verifies that in dev mode with an
// empty allowed_mcp_hosts the /trigger handler accepts any valid https URL.
func TestHandleTrigger_DevMode_EmptyAllowlist(t *testing.T) {
	tr := tracker.New()
	fake := newFakeRunner()
	h := NewHandler(fake, tr, nil, nil, testAPIKey, 10, nil, nil, nil, true)

	payload := TriggerPayload{
		CardID:  "CARD-1",
		Project: "proj",
		RepoURL: "https://github.com/org/repo.git",
		MCPURL:  "https://example.com/mcp",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", payload)
	h.hmacAuth(h.handleTrigger)(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code,
		"dev mode + empty allowlist must accept a valid https mcp_url")
}

// TestHandleTrigger_ProductionMode_EmptyAllowlistRejectsAll verifies that in
// production mode (devMode=false) an empty allowlist still rejects every URL.
func TestHandleTrigger_ProductionMode_EmptyAllowlistRejectsAll(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(&strictRunner{t: t}, tr, nil, nil, testAPIKey, 10, nil, nil, nil, false)

	payload := TriggerPayload{
		CardID:  "CARD-1",
		Project: "proj",
		RepoURL: "https://github.com/org/repo.git",
		MCPURL:  "https://example.com/mcp",
	}
	w := httptest.NewRecorder()
	req := signedRequest(t, "/trigger", payload)
	h.hmacAuth(h.handleTrigger)(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code,
		"production mode + empty allowlist must reject any mcp_url")
}
