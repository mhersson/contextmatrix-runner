package webhook

import (
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
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

func testManager(tr *tracker.Tracker) *container.Manager {
	cfg := &config.Config{ContainerTimeout: "1h"}
	cfg.ParseContainerTimeout()
	return container.NewManager(nil, tr, nil, nil, cfg, nil)
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
	h := NewHandler(nil, tr, testAPIKey, 3, nil)

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

	h := NewHandler(nil, tr, testAPIKey, 3, nil)

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

	h := NewHandler(nil, tr, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := signedRequest(t, "POST", "/trigger", TriggerPayload{
		CardID:  "PROJ-042",
		Project: "my-project",
		RepoURL: "git@github.com:org/repo.git",
		MCPURL:  "http://cm:8080/mcp",
	})
	h.hmacAuth(h.handleTrigger)(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestHandleKill_NotFound(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(testManager(tr), tr, testAPIKey, 3, nil)

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

	h := NewHandler(nil, tr, testAPIKey, 3, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	h.handleHealth(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, true, resp["ok"])
	assert.Equal(t, float64(1), resp["running_containers"])
}
