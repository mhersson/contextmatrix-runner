package webhook

import (
	"bytes"
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

	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
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

	var resp Response
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

// TestHandleMessage_DedupReturnsOriginalAck verifies that a repeat
// /message call with the same (project, card_id, message_id) returns
// the cached 202 ack without invoking tracker.WriteStdin a second time.
func TestHandleMessage_DedupReturnsOriginalAck(t *testing.T) {
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)
	h.SetMessageDedupCache(NewMessageDedupCache(10*time.Minute, 100))

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	payload := MessagePayload{
		CardID:    "PROJ-001",
		Project:   "my-project",
		Content:   "hello from user",
		MessageID: "stable-msg-id-42",
	}

	// First call — writes to stdin, caches ack.
	w1 := httptest.NewRecorder()
	req1 := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w1, req1)

	require.Equal(t, http.StatusAccepted, w1.Code)

	var resp1 Response
	require.NoError(t, json.NewDecoder(w1.Body).Decode(&resp1))
	assert.True(t, resp1.OK)
	assert.Equal(t, "stable-msg-id-42", resp1.MessageID)

	firstWriteLen := len(fw.buf)
	require.NotZero(t, firstWriteLen, "first request must write to stdin")

	// Second call — same (project, card_id, message_id) — must hit cache.
	// We use a fresh timestamp so the HMAC signature differs, bypassing
	// the replay cache; the dedup is solely driven by message_id.
	w2 := httptest.NewRecorder()

	// Sleep 1s to guarantee a different HMAC timestamp, hence a
	// different signature, so the replay cache (which isn't even wired
	// here) can't accidentally be the reason for a hit.
	time.Sleep(1100 * time.Millisecond)

	req2 := signedRequest(t, "/message", payload)
	h.hmacAuth(h.handleMessage)(w2, req2)

	assert.Equal(t, http.StatusAccepted, w2.Code, "dedup hit must still return 202")

	var resp2 Response
	require.NoError(t, json.NewDecoder(w2.Body).Decode(&resp2))
	assert.True(t, resp2.OK)
	assert.Equal(t, "stable-msg-id-42", resp2.MessageID, "dedup'd response must echo original message_id")

	assert.Len(t, fw.buf, firstWriteLen, "second call must NOT write to stdin again")
}

// TestHandleMessage_DedupTTLExpires verifies that after the dedup TTL
// elapses, an identical (project, card_id, message_id) request is
// serviced from scratch — including a second stdin write.
func TestHandleMessage_DedupTTLExpires(t *testing.T) {
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	clock := &mockClock{now: time.Unix(1_700_000_000, 0)}
	dedup := NewMessageDedupCache(5*time.Minute, 100, WithMessageDedupNow(clock.Now))
	h.SetMessageDedupCache(dedup)

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	payload := MessagePayload{
		CardID:    "PROJ-001",
		Project:   "my-project",
		Content:   "hello",
		MessageID: "msg-ttl-test",
	}

	// First call populates the dedup cache.
	w1 := httptest.NewRecorder()
	h.hmacAuth(h.handleMessage)(w1, signedRequest(t, "/message", payload))
	require.Equal(t, http.StatusAccepted, w1.Code)

	firstLen := len(fw.buf)
	require.NotZero(t, firstLen)

	// Advance the cache clock past the TTL.
	clock.advance(6 * time.Minute)

	// Need a new HMAC signature (different timestamp) — the real test
	// is about dedup TTL, not HMAC skew.
	time.Sleep(1100 * time.Millisecond)

	w2 := httptest.NewRecorder()
	h.hmacAuth(h.handleMessage)(w2, signedRequest(t, "/message", payload))
	assert.Equal(t, http.StatusAccepted, w2.Code)

	assert.Greater(t, len(fw.buf), firstLen, "after TTL, second request must re-invoke stdin write")
}

// TestHandleMessage_DedupDisabledNoOp verifies that when the dedup
// cache is nil, repeat calls still write stdin every time (baseline —
// this is the pre-047 behaviour).
func TestHandleMessage_DedupDisabledNoOp(t *testing.T) {
	tr := tracker.New()
	b := logbroadcast.NewBroadcaster(nil, nil)
	h := NewHandler(nil, tr, b, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)
	// intentionally no dedup cache

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  "PROJ-001",
		Project: "my-project",
	}))

	fw := &fakeWriteCloser{}
	tr.SetStdin("my-project", "PROJ-001", fw, nil)

	payload := MessagePayload{
		CardID:    "PROJ-001",
		Project:   "my-project",
		Content:   "hello",
		MessageID: "msg-1",
	}

	w1 := httptest.NewRecorder()
	h.hmacAuth(h.handleMessage)(w1, signedRequest(t, "/message", payload))
	require.Equal(t, http.StatusAccepted, w1.Code)

	time.Sleep(1100 * time.Millisecond)

	w2 := httptest.NewRecorder()
	h.hmacAuth(h.handleMessage)(w2, signedRequest(t, "/message", payload))
	require.Equal(t, http.StatusAccepted, w2.Code)

	// Each request writes a stream-json line ending in \n; expect at
	// least two newlines when dedup is disabled.
	assert.GreaterOrEqual(t, strings.Count(string(fw.buf), "\n"), 2, "dedup disabled must write both times")
}
