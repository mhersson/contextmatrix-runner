package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// readyBody decodes the /readyz response; ok + optional reason are the
// only fields the wire contract promises.
type readyBody struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
}

func newReadyzHandler(t *testing.T, state *HealthState) http.Handler {
	t.Helper()

	// NewHandler accepts nil for everything the /readyz path does not
	// touch — /readyz must be usable without CM/callback/manager/etc.
	h := NewHandler(nil, tracker.New(), nil, nil, testAPIKey, 1, testMCPURL, nil, 0, state)

	mux := http.NewServeMux()
	h.Register(mux)

	return mux
}

func doReadyz(t *testing.T, h http.Handler) (int, readyBody) {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var body readyBody

	require.NoError(t, json.NewDecoder(w.Result().Body).Decode(&body))
	_ = w.Result().Body.Close()

	return w.Code, body
}

func TestReadyz_PreflightFailing(t *testing.T) {
	state := NewHealthState()
	// Preflight has not passed; Draining is false.
	h := newReadyzHandler(t, state)

	code, body := doReadyz(t, h)

	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.False(t, body.OK)
	assert.Equal(t, "preflight", body.Reason)
}

func TestReadyz_Draining(t *testing.T) {
	state := NewHealthState()
	// Draining trumps preflight state: a runner that has started
	// shutdown must report "draining" even if preflight previously
	// passed.
	state.PreflightPassed.Store(true)
	state.Draining.Store(true)

	h := newReadyzHandler(t, state)

	code, body := doReadyz(t, h)

	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.False(t, body.OK)
	assert.Equal(t, "draining", body.Reason)
}

func TestReadyz_OK(t *testing.T) {
	state := NewHealthState()
	state.PreflightPassed.Store(true)

	h := newReadyzHandler(t, state)

	code, body := doReadyz(t, h)

	assert.Equal(t, http.StatusOK, code)
	assert.True(t, body.OK)
	assert.Empty(t, body.Reason)
}

// TestReadyz_NoHMAC confirms /readyz is unauthenticated. A request
// missing the HMAC headers must still receive a valid readiness answer
// (not 403) because LBs will not send signed probes.
func TestReadyz_NoHMAC(t *testing.T) {
	state := NewHealthState()
	state.PreflightPassed.Store(true)

	h := newReadyzHandler(t, state)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody)
	// Explicitly no X-Signature-256 / X-Webhook-Timestamp headers.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestReadyz_NilHealthState covers the defensive branch where a handler
// was built with nil state — the response must still be well-formed JSON
// (not a panic) so older test fixtures keep compiling.
func TestReadyz_NilHealthState(t *testing.T) {
	h := newReadyzHandler(t, nil)

	code, body := doReadyz(t, h)

	assert.Equal(t, http.StatusOK, code)
	assert.True(t, body.OK)
}
