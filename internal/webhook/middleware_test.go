package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/metrics"
)

// TestCorrelationMiddleware_GeneratesIDWhenMissing verifies that a request
// without an X-Correlation-ID header gets a fresh UUID attached to the
// response and to the context.
func TestCorrelationMiddleware_GeneratesIDWhenMissing(t *testing.T) {
	var seen string

	handler := withCorrelation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = correlationIDFromContext(r.Context())

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/trigger", strings.NewReader(""))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	got := w.Header().Get(CorrelationHeader)
	require.NotEmpty(t, got, "response must echo a correlation ID")
	assert.Equal(t, got, seen, "ctx value must match response header")
}

// TestCorrelationMiddleware_EchoesIncomingID verifies that a supplied
// X-Correlation-ID is preserved verbatim.
func TestCorrelationMiddleware_EchoesIncomingID(t *testing.T) {
	const id = "abc-123"

	handler := withCorrelation(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		assert.Equal(t, id, correlationIDFromContext(r.Context()))
	}))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/trigger", strings.NewReader(""))
	req.Header.Set(CorrelationHeader, id)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, id, w.Header().Get(CorrelationHeader))
}

// TestMetricsMiddleware_ObservesEndpointAndStatus verifies the request
// counter + histogram get a sample with the right labels.
func TestMetricsMiddleware_ObservesEndpointAndStatus(t *testing.T) {
	mx := metrics.New()

	handler := withMetrics(mx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/trigger", http.NoBody)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	families, err := mx.Registry.Gather()
	require.NoError(t, err)

	var foundCounter, foundHist bool

	for _, f := range families {
		switch f.GetName() {
		case "cmr_webhook_requests_total":
			for _, m := range f.Metric {
				labels := labelMap(m.Label)
				if labels["endpoint"] == "trigger" && labels["status"] == "202" && labels["code"] == "success" {
					foundCounter = true
				}
			}
		case "cmr_webhook_request_duration_seconds":
			for _, m := range f.Metric {
				labels := labelMap(m.Label)
				if labels["endpoint"] == "trigger" {
					foundHist = true
				}
			}
		}
	}

	assert.True(t, foundCounter, "request counter label set not found")
	assert.True(t, foundHist, "request histogram label set not found")
}

// TestMetricsMiddleware_RateLimitedCode verifies that 429s produce the
// rate_limited code bucket (CTXRUN-056 shape).
func TestMetricsMiddleware_RateLimitedCode(t *testing.T) {
	mx := metrics.New()

	handler := withMetrics(mx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/trigger", http.NoBody)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	families, err := mx.Registry.Gather()
	require.NoError(t, err)

	var got bool

	for _, f := range families {
		if f.GetName() != "cmr_webhook_requests_total" {
			continue
		}

		for _, m := range f.Metric {
			l := labelMap(m.Label)
			if l["status"] == "429" && l["code"] == "rate_limited" {
				got = true
			}
		}
	}

	assert.True(t, got, "rate_limited code bucket not recorded")
}

// TestStatusRecorder_UnwrapExposesUnderlyingWriter verifies statusRecorder
// exposes Unwrap() so http.NewResponseController can walk through it to the
// underlying conn. Without this, the SSE /logs handler's SetWriteDeadline
// call silently fails and the server's 30s WriteTimeout drops every
// long-lived connection.
func TestStatusRecorder_UnwrapExposesUnderlyingWriter(t *testing.T) {
	inner := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: inner}

	unwrapper, ok := any(sr).(interface{ Unwrap() http.ResponseWriter })
	require.True(t, ok, "statusRecorder must implement Unwrap() http.ResponseWriter")
	assert.Same(t, http.ResponseWriter(inner), unwrapper.Unwrap(),
		"Unwrap must return the wrapped ResponseWriter so ResponseController can reach the conn")
}

func labelMap(pairs []*dto.LabelPair) map[string]string {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		out[p.GetName()] = p.GetValue()
	}

	return out
}
