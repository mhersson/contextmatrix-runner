package webhook_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/mhersson/contextmatrix-runner/internal/metrics"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
	"github.com/mhersson/contextmatrix-runner/internal/webhook"
)

const adminTestAPIKey = "test-api-key-that-is-at-least-32-chars"

// TestAdminMetrics_RejectsUnauthenticated verifies that the /metrics endpoint
// on the admin port refuses a request with no HMAC signature, and accepts a
// correctly-signed GET. Covers the "401 vs 200" contract from the ticket
// (the runner returns 403 for missing signatures, which is equivalent).
func TestAdminMetrics_RejectsUnauthenticated(t *testing.T) {
	mx := metrics.New()
	b := logbroadcast.NewBroadcaster(nil, nil)

	h := webhook.NewHandler(nil, tracker.New(), b, nil, adminTestAPIKey, 3, nil, nil, nil, false).WithMetrics(mx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", h.AdminAuth(promhttp.HandlerFor(mx.Registry, promhttp.HandlerOpts{}).ServeHTTP))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Unauthenticated: should be rejected.
	{
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/metrics", http.NoBody)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		assert.NotEqual(t, http.StatusOK, resp.StatusCode, "unauthenticated /metrics must not return 200")
		assert.GreaterOrEqual(t, resp.StatusCode, 400, "expected a 4xx rejection")
	}

	// Correctly signed GET: should be accepted.
	{
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		sig := cmhmac.SignPayloadWithTimestamp(adminTestAPIKey, []byte{}, ts)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/metrics", http.NoBody)
		require.NoError(t, err)
		req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
		req.Header.Set(cmhmac.TimestampHeader, ts)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode, "signed /metrics must succeed")
	}
}
