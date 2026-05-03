package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReportStatus_Success(t *testing.T) {
	apiKey := "test-secret-key-that-is-long-enough"

	var (
		received            statusRequest
		sigHeader, tsHeader string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigHeader = r.Header.Get(cmhmac.SignatureHeader)
		tsHeader = r.Header.Get(cmhmac.TimestampHeader)

		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)

		// Verify HMAC
		sig := strings.TrimPrefix(sigHeader, "sha256=")
		assert.True(t, cmhmac.VerifySignatureWithTimestamp(apiKey, r.Method, r.URL.RequestURI(), sig, tsHeader, body, cmhmac.DefaultMaxClockSkew))

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, apiKey, testLogger())
	err := client.ReportStatus(context.Background(), "PROJ-042", "my-project", "running", "container started")
	require.NoError(t, err)

	assert.Equal(t, "PROJ-042", received.CardID)
	assert.Equal(t, "my-project", received.Project)
	assert.Equal(t, "running", received.RunnerStatus)
	assert.Equal(t, "container started", received.Message)
	assert.True(t, strings.HasPrefix(sigHeader, "sha256="))
	assert.NotEmpty(t, tsHeader)
}

func TestReportStatus_ClientError_NoRetry(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid status"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	err := client.ReportStatus(context.Background(), "PROJ-042", "my-project", "bad", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "422")
	assert.Equal(t, int32(1), calls.Load(), "should not retry on 4xx")
}

func TestReportStatus_ServerError_Retries(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false,"error":"internal"}`))

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	err := client.ReportStatus(context.Background(), "PROJ-042", "my-project", "running", "")
	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load(), "should retry on 5xx")
}

func TestReportStatus_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	err := client.ReportStatus(ctx, "PROJ-042", "my-project", "running", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestReportStatus_HMACFormat(t *testing.T) {
	apiKey := "my-super-long-api-key-for-hmac-testing"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig := r.Header.Get(cmhmac.SignatureHeader)
		ts := r.Header.Get(cmhmac.TimestampHeader)

		assert.True(t, strings.HasPrefix(sig, "sha256="), "signature must start with sha256=")
		assert.NotEmpty(t, ts, "timestamp header must be set")

		// Verify the signature is valid
		body, _ := io.ReadAll(r.Body)
		hexSig := strings.TrimPrefix(sig, "sha256=")
		assert.True(t, cmhmac.VerifySignatureWithTimestamp(apiKey, r.Method, r.URL.RequestURI(), hexSig, ts, body, cmhmac.DefaultMaxClockSkew))

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, apiKey, testLogger())
	err := client.ReportStatus(context.Background(), "TEST-001", "proj", "failed", "crash")
	require.NoError(t, err)
}

func TestVerifyAutonomous_True(t *testing.T) {
	var receivedMethod, receivedPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":true}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.NoError(t, err)
	assert.True(t, autonomous)

	// Must use GET, not POST, so CM does not re-trigger the promote webhook.
	assert.Equal(t, http.MethodGet, receivedMethod)
	assert.Equal(t, "/api/v1/cards/my-project/PROJ-001/autonomous", receivedPath)
}

func TestVerifyAutonomous_False(t *testing.T) {
	// autonomous=false means the card has not been promoted yet — caller should
	// refuse to write stdin.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":false}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.NoError(t, err)
	assert.False(t, autonomous)
}

func TestVerifyAutonomous_ServerError(t *testing.T) {
	// 5xx → (false, err); caller must not write stdin.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.Error(t, err)
	assert.False(t, autonomous)
	assert.Contains(t, err.Error(), "500")
}

func TestVerifyAutonomous_NotFound(t *testing.T) {
	// 404 → (false, err); caller must not write stdin.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.Error(t, err)
	assert.False(t, autonomous)
	assert.Contains(t, err.Error(), "404")
}

// TestVerifyAutonomous_HMACSigned confirms the default auth mode: the
// request carries HMAC headers (X-Signature-256 + X-Webhook-Timestamp)
// and NO `Authorization: Bearer`. This is the fix for H10
// (apiKey-triple-purpose Bearer leakage) under CTXRUN-048.
func TestVerifyAutonomous_HMACSigned(t *testing.T) {
	apiKey := "test-secret-key-that-is-long-enough"

	var (
		sigHeader, tsHeader, authHeader string
		receivedMethod, receivedURI     string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigHeader = r.Header.Get(cmhmac.SignatureHeader)
		tsHeader = r.Header.Get(cmhmac.TimestampHeader)
		authHeader = r.Header.Get("Authorization")
		receivedMethod = r.Method
		receivedURI = r.URL.RequestURI()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":true}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, apiKey, testLogger())
	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.NoError(t, err)
	assert.True(t, autonomous)

	// HMAC headers present, no Bearer leakage.
	assert.True(t, strings.HasPrefix(sigHeader, "sha256="), "signature header must be set")
	assert.NotEmpty(t, tsHeader, "timestamp header must be set")
	assert.Empty(t, authHeader, "must not send Authorization: Bearer when HMAC is enabled")

	// Signature must verify against an empty body (GET carries no body).
	hexSig := strings.TrimPrefix(sigHeader, "sha256=")
	assert.True(t, cmhmac.VerifySignatureWithTimestamp(apiKey, receivedMethod, receivedURI, hexSig, tsHeader, nil, cmhmac.DefaultMaxClockSkew))
}

// TestVerifyAutonomous_HMACSigned_RejectsMissingHeaders ensures that a
// server reply of 401 (e.g. HMAC rejected upstream) propagates as an
// error to the caller — the runner must stay fail-closed.
func TestVerifyAutonomous_HMACSigned_RejectsMissingHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing signature"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.Error(t, err)
	assert.False(t, autonomous)
	assert.Contains(t, err.Error(), "401")
}

// TestVerifyAutonomous_PathEscaping verifies that project and cardID are
// url.PathEscape'd unconditionally (REVIEW.md M27). The project contains
// a space and the cardID contains a slash, both of which would otherwise
// produce a malformed URL or a path-traversal vector.
func TestVerifyAutonomous_PathEscaping(t *testing.T) {
	var rawRequestURI string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawRequestURI = r.RequestURI

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":false}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	_, err := client.VerifyAutonomous(context.Background(), "my project", "CARD/42")
	require.NoError(t, err)

	assert.Contains(t, rawRequestURI, "%20", "space in project must be escaped")
	assert.Contains(t, rawRequestURI, "%2F", "slash in cardID must be escaped")
	assert.Contains(t, rawRequestURI, "/api/v1/cards/my%20project/CARD%2F42/autonomous")
}

// TestVerifyAutonomous_URLBuildsCorrectly nails down the exact URL path
// the runner hits: /api/v1/cards/<project>/<cardID>/autonomous.
func TestVerifyAutonomous_URLBuildsCorrectly(t *testing.T) {
	var receivedPath, receivedMethod string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedMethod = r.Method

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":true}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	_, err := client.VerifyAutonomous(context.Background(), "acme", "CARD-7")
	require.NoError(t, err)

	assert.Equal(t, http.MethodGet, receivedMethod)
	assert.Equal(t, "/api/v1/cards/acme/CARD-7/autonomous", receivedPath)
}

// TestCallbackError_ErrorShortForm verifies that Error() returns a body-free
// form safe to surface to external callers (no upstream response body, no
// query string).
func TestCallbackError_ErrorShortForm(t *testing.T) {
	ce := newError(
		"https://example.com/api/runner/status?token=secret-value#frag",
		502,
		[]byte(`{"error":"ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa leak"}`),
	)

	got := ce.Error()
	assert.Equal(t, "callback to https://example.com/api/runner/status returned status 502", got)
	assert.NotContains(t, got, "secret-value", "query string must be stripped")
	assert.NotContains(t, got, "token=", "query key must be stripped")
	assert.NotContains(t, got, "frag", "fragment must be stripped")
	assert.NotContains(t, got, "ghp_", "upstream body must not appear in Error()")
}

// TestCallbackError_DetailForLog preserves the upstream body for server-side
// debug logging, truncated to a sane bound.
func TestCallbackError_DetailForLog(t *testing.T) {
	body := []byte("upstream said boom")
	ce := newError("https://cm.example/api/runner/status", 500, body)

	assert.Equal(t, "upstream said boom", ce.DetailForLog())
	assert.Equal(t, 500, ce.StatusCode())
}

// TestCallbackError_DetailTruncated caps DetailForLog at maxDetailBytes so a
// pathological upstream cannot pin huge buffers via retained error values.
func TestCallbackError_DetailTruncated(t *testing.T) {
	huge := bytes.Repeat([]byte("a"), maxDetailBytes*3)
	ce := newError("https://cm.example/api/runner/status", 500, huge)

	assert.Len(t, ce.DetailForLog(), maxDetailBytes)
}

// TestCallbackError_InvalidURL falls back to "<invalid-url>" so a malformed
// input still produces a safe, secret-free error string.
func TestCallbackError_InvalidURL(t *testing.T) {
	ce := newError("::not a url::", 500, []byte("boom"))

	assert.Equal(t, "callback to <invalid-url> returned status 500", ce.Error())
}

// TestPing_Success confirms Ping returns nil when the CM host is reachable
// at the URL's host:port. httptest.NewServer hands us a live listener, so
// a TCP dial against that URL must succeed.
func TestPing_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	require.NoError(t, client.Ping(context.Background()))
}

// TestPing_Unreachable targets a port nothing listens on (127.0.0.1:1 is a
// well-known closed-port choice on Linux). The dial must fail promptly,
// giving the preflight a real failure signal rather than a silent pass.
func TestPing_Unreachable(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", "test-secret-key-that-is-long-enough", testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.Error(t, client.Ping(ctx))
}

// TestPing_InvalidURL surfaces a parse error rather than a dial error when
// the configured URL is malformed, so operators see the real cause in the
// preflight log.
func TestPing_InvalidURL(t *testing.T) {
	client := NewClient("://not a url", "test-secret-key-that-is-long-enough", testLogger())

	err := client.Ping(context.Background())
	require.Error(t, err)
	// The error must not be a dial error (the URL never reached the dialer).
	assert.Contains(t, err.Error(), "contextmatrix_url")
}

// TestRetryLoop_TimerStopOnCtx verifies that cancelling ctx mid-backoff
// returns promptly and does not leak the per-attempt Timer. Under the old
// time.After-based loop the Timer kept a reference into the runtime heap
// until it fired (up to 4s later on the last attempt) so a burst of
// cancelled callbacks would pile up unreachable Timers. CTXRUN-059 (M19).
//
// The test kicks off a ReportStatus against a server that always 500s,
// then cancels the ctx just as the first backoff starts. We assert two
// things:
//  1. The call returns within a tight envelope after cancellation.
//  2. Goroutine count does not grow meaningfully across many iterations
//     (slack is generous — this is a leak detector, not a strict bound).
func TestRetryLoop_TimerStopOnCtx(t *testing.T) {
	// Always 500 so every attempt burns a full backoff.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())

	// Warm up the HTTP transport so first-run connection setup doesn't
	// skew the goroutine count baseline.
	{
		warmCtx, warmCancel := context.WithCancel(context.Background())
		warmCancel()

		_ = client.ReportStatus(warmCtx, "warm", "warm", "running", "warm")
	}

	// Let any background dial bookkeeping settle.
	time.Sleep(50 * time.Millisecond)

	baseline := runtime.NumGoroutine()

	// Run many cancelled ReportStatus calls. If Timer leaks, each iteration
	// would pin a runtime timer bucket entry and (transitively) the
	// goroutine servicing it; the net growth would dwarf the slack.
	const iterations = 32

	for range iterations {
		ctx, cancel := context.WithCancel(context.Background())

		// Cancel while the first backoff is running (backoff = 1s).
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		err := client.ReportStatus(ctx, "PROJ", "proj", "running", "msg")
		elapsed := time.Since(start)

		// Must return very quickly after cancellation — not wait out the
		// full 1s backoff.
		require.ErrorIs(t, err, context.Canceled)
		require.Less(t, elapsed, 500*time.Millisecond,
			"ReportStatus must return promptly after ctx cancel; got %s", elapsed)
	}

	// Give the runtime a moment to reap any torn-down goroutines.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	after := runtime.NumGoroutine()

	// Generous slack: on a noisy CI runner the http transport may keep a
	// handful of idle conns, but it should not grow linearly with
	// iterations.
	assert.Less(t, after-baseline, 10,
		"goroutine count should not grow with cancelled iterations; baseline=%d after=%d", baseline, after)
}

func TestClient_ReportSkillEngaged(t *testing.T) {
	var receivedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/runner/skill-engaged", r.URL.Path)
		assert.NotEmpty(t, r.Header.Get(cmhmac.SignatureHeader))

		receivedBody, _ = io.ReadAll(r.Body)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "at-least-thirty-two-characters-long-key", slog.Default())
	require.NoError(t, c.ReportSkillEngaged(context.Background(), "ALPHA-001", "alpha", "runner:ALPHA-001", "go-development"))

	assert.Contains(t, string(receivedBody), `"card_id":"ALPHA-001"`)
	assert.Contains(t, string(receivedBody), `"skill_name":"go-development"`)
	assert.Contains(t, string(receivedBody), `"agent_id":"runner:ALPHA-001"`,
		"runner must propagate agent_id so CM can attribute the skill engagement")
}
