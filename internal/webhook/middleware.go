package webhook

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mhersson/contextmatrix-runner/internal/metrics"
)

// correlationKey is the ctx key under which the correlation ID is stored.
type correlationKey struct{}

// CorrelationHeader is read from incoming webhook requests and echoed on
// every response so clients (and CM) can stitch their traces to runner logs.
const CorrelationHeader = "X-Correlation-ID"

// correlationIDFromContext extracts the correlation ID attached by the
// correlation middleware. Returns "" if none was attached.
func correlationIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(correlationKey{}).(string); ok {
		return v
	}

	return ""
}

// withCorrelation is a middleware that ensures every request has a correlation
// ID. If the client supplied X-Correlation-ID it is echoed; otherwise a fresh
// UUID is generated. The value is attached to ctx and set as a response header
// before the next handler runs, so writeError/writeJSON in the downstream
// handler will carry the header.
func withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationHeader)
		if id == "" {
			id = uuid.NewString()
		}

		w.Header().Set(CorrelationHeader, id)
		ctx := context.WithValue(r.Context(), correlationKey{}, id)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusRecorder is a thin ResponseWriter wrapper that captures the HTTP
// status code written by downstream handlers. Required because metrics
// observation runs after the handler returns.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	if !sr.wrote {
		sr.status = code
		sr.wrote = true
	}

	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if !sr.wrote {
		sr.status = http.StatusOK
		sr.wrote = true
	}

	return sr.ResponseWriter.Write(b)
}

// Flush propagates the flush call to the underlying ResponseWriter when it
// implements http.Flusher (e.g. the SSE /logs handler). Without this the
// middleware would silently swallow the flusher interface.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.NewResponseController walk past this wrapper to reach the
// underlying ResponseWriter. Without it, SetWriteDeadline on the SSE /logs
// handler silently fails and the server's WriteTimeout terminates every
// long-lived connection after ~30s. See handler.handleLogs.
func (sr *statusRecorder) Unwrap() http.ResponseWriter {
	return sr.ResponseWriter
}

// withMetrics wraps every webhook handler and records request count + duration
// on the provided metrics bundle. The endpoint label is the URL path with a
// leading slash stripped; status is the HTTP status code; code is a coarse
// success/error bucket derived from the status (the CTXRUN-056 ErrorResponse
// code-string mapping can be plugged in later without changing the shape).
func withMetrics(m *metrics.Metrics, next http.Handler) http.Handler {
	if m == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)

		endpoint := strings.TrimPrefix(r.URL.Path, "/")
		if endpoint == "" {
			endpoint = "root"
		}

		code := "success"
		if sr.status >= 400 {
			code = "error"
		}

		if sr.status == http.StatusTooManyRequests {
			code = "rate_limited"
		}

		m.WebhookRequestsTotal.WithLabelValues(endpoint, strconv.Itoa(sr.status), code).Inc()
		m.WebhookRequestDuration.WithLabelValues(endpoint).Observe(time.Since(start).Seconds())
	})
}
