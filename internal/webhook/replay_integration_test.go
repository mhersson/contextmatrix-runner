package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// TestHMACMiddleware_RejectsReplay verifies that after a successful
// signed request lands, an identical second request (same signature)
// is rejected with 409 by the replay cache before reaching the
// downstream handler.
func TestHMACMiddleware_RejectsReplay(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)
	h.SetReplayCache(NewReplayCache(5*time.Minute, 100))

	var called int

	handler := h.hmacAuth(func(w http.ResponseWriter, _ *http.Request) {
		called++

		w.WriteHeader(http.StatusOK)
	})

	// Build a single signed payload and replay it byte-identically.
	body := []byte(`{"test":true}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/test", body, ts)

	mkReq := func() *http.Request {
		req := httptest.NewRequestWithContext(context.Background(), "POST", "/test", bytes.NewReader(body))
		req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
		req.Header.Set(cmhmac.TimestampHeader, ts)

		return req
	}

	w1 := httptest.NewRecorder()
	handler(w1, mkReq())
	assert.Equal(t, http.StatusOK, w1.Code, "first request must succeed")

	w2 := httptest.NewRecorder()
	handler(w2, mkReq())
	assert.Equal(t, http.StatusConflict, w2.Code, "replay must be rejected")

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w2.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Message, "duplicate")

	assert.Equal(t, 1, called, "downstream handler must only run once")
}

// TestHMACMiddleware_DifferentSignaturesAllBypass_Replay verifies the
// obvious negative: distinct signatures (e.g. new timestamp) never
// collide in the cache.
func TestHMACMiddleware_DifferentSignaturesAllBypass_Replay(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(nil, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)
	h.SetReplayCache(NewReplayCache(5*time.Minute, 100))

	handler := h.hmacAuth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	body := []byte(`{"ping":1}`)

	for i := range 3 {
		ts := strconv.FormatInt(time.Now().Unix()-int64(i), 10) // distinct ts ensures distinct sig
		sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/test", body, ts)

		req := httptest.NewRequestWithContext(context.Background(), "POST", "/test", bytes.NewReader(body))
		req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
		req.Header.Set(cmhmac.TimestampHeader, ts)

		w := httptest.NewRecorder()
		handler(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "request %d with distinct sig must pass", i)
	}
}
