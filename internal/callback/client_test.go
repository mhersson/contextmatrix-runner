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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/metrics"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// counterValue returns the float64 value of a single-series, label-free
// counter on m's registry. Returns 0 if the family is absent. Fails the
// test on multi-series families because the counters it inspects are
// intentionally label-free.
func counterValue(t *testing.T, m *metrics.Metrics, name string) float64 {
	t.Helper()

	families, err := m.Registry.Gather()
	require.NoError(t, err)

	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}

		require.Len(t, fam.Metric, 1, "counter %q must be label-free / single-series", name)

		return fam.Metric[0].GetCounter().GetValue()
	}

	return 0
}

// labelledCounterValue returns the value of the series of a CounterVec
// that matches all provided label values, in declaration order. Returns
// 0 if the family is absent or no series matches.
func labelledCounterValue(t *testing.T, m *metrics.Metrics, name string, want ...string) float64 {
	t.Helper()

	families, err := m.Registry.Gather()
	require.NoError(t, err)

	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}

		for _, series := range fam.Metric {
			if matchLabels(series.Label, want) {
				return series.GetCounter().GetValue()
			}
		}
	}

	return 0
}

func matchLabels(have []*dto.LabelPair, want []string) bool {
	if len(have) != len(want) {
		return false
	}

	for i, lp := range have {
		if lp.GetValue() != want[i] {
			return false
		}
	}

	return true
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
	// 5xx → (false, err); caller must not write stdin. After Fix W2 the
	// VerifyAutonomous call retries transient 5xxs maxRetries times before
	// failing; this test asserts every attempt fires (so a regression to
	// a single-shot path would also fail here) and that the final error
	// still carries the upstream status.
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.Error(t, err)
	assert.False(t, autonomous)
	assert.Contains(t, err.Error(), "500")
	assert.Equal(t, int32(maxRetries), calls.Load(),
		"5xx must be retried maxRetries times before fail-closed")
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
// (apiKey-triple-purpose Bearer leakage).
func TestVerifyAutonomous_HMACSigned(t *testing.T) {
	apiKey := "test-secret-key-that-is-long-enough"

	var (
		sigHeader, tsHeader, authHeader string
		receivedMethod, receivedPath    string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigHeader = r.Header.Get(cmhmac.SignatureHeader)
		tsHeader = r.Header.Get(cmhmac.TimestampHeader)
		authHeader = r.Header.Get("Authorization")
		receivedMethod = r.Method
		receivedPath = r.URL.Path

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
	assert.True(t, cmhmac.VerifySignatureWithTimestamp(apiKey, receivedMethod, receivedPath, hexSig, tsHeader, nil, cmhmac.DefaultMaxClockSkew))
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

// TestVerifyAutonomous_BearerFallbackWhenDisabled verifies that disabling
// the HMAC flag restores the legacy Bearer behaviour so the runner stays
// compatible with a CM server that has not yet rolled the HMAC change.
// No HMAC headers must be sent in this mode.
func TestVerifyAutonomous_BearerFallbackWhenDisabled(t *testing.T) {
	apiKey := "test-secret-key-that-is-long-enough"

	var sigHeader, tsHeader, authHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigHeader = r.Header.Get(cmhmac.SignatureHeader)
		tsHeader = r.Header.Get(cmhmac.TimestampHeader)
		authHeader = r.Header.Get("Authorization")

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":true}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, apiKey, testLogger())
	client.SetUseHMACForVerifyAutonomous(false)

	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.NoError(t, err)
	assert.True(t, autonomous)

	assert.Equal(t, "Bearer "+apiKey, authHeader, "Bearer fallback must send the apiKey")
	assert.Empty(t, sigHeader, "HMAC signature header must not be present in Bearer mode")
	assert.Empty(t, tsHeader, "HMAC timestamp header must not be present in Bearer mode")
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
// cancelled callbacks would pile up unreachable Timers.
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
	require.NoError(t, c.ReportSkillEngaged(context.Background(), "ALPHA-001", "alpha", "go-development"))

	assert.Contains(t, string(receivedBody), `"card_id":"ALPHA-001"`)
	assert.Contains(t, string(receivedBody), `"skill_name":"go-development"`)
}

func TestClient_KnowledgeStatus_PostsSignedRequest(t *testing.T) {
	var (
		received KnowledgeStatusRequest
		rawBody  []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/runner/knowledge-status", r.URL.Path)
		assert.NotEmpty(t, r.Header.Get(cmhmac.SignatureHeader))
		assert.NotEmpty(t, r.Header.Get(cmhmac.TimestampHeader))

		rawBody, _ = io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(rawBody, &received))

		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "secret-key-that-is-at-least-32-chars-long", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, c.KnowledgeStatus(context.Background(), KnowledgeStatusRequest{
		Project: "p", Repo: "r", State: "succeeded",
	}))
	assert.Equal(t, "succeeded", received.State)
	assert.Equal(t, "p", received.Project)
	assert.Equal(t, "r", received.Repo)
	assert.NotContains(t, string(rawBody), "commit_sha",
		"marshalled body must not contain commit_sha field")
}

// TestReportSkillEngaged_ServerError_IncrementsRetryMetric pins the
// cmr_callback_retries_total counter to the skill-engaged endpoint label
// on retries. Asymmetric metric coverage across the three callback
// methods previously left skill-engaged and knowledge-status invisible to
// dashboards.
func TestReportSkillEngaged_ServerError_IncrementsRetryMetric(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	mx := metrics.New()
	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger()).WithMetrics(mx)

	require.NoError(t, client.ReportSkillEngaged(context.Background(), "P-1", "proj", "go-development"))
	assert.Equal(t, int32(3), calls.Load(), "should retry on 5xx")

	got := labelledCounterValue(t, mx, "cmr_callback_retries_total", "skill_engaged")
	assert.InDelta(t, 2.0, got, 0, "two retries (first two attempts failed) must increment the skill_engaged label")
}

// TestKnowledgeStatus_ServerError_IncrementsRetryMetric mirrors the
// skill-engaged assertion above against the knowledge-status endpoint
// label.
func TestKnowledgeStatus_ServerError_IncrementsRetryMetric(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	mx := metrics.New()
	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger()).WithMetrics(mx)

	require.NoError(t, client.KnowledgeStatus(context.Background(), KnowledgeStatusRequest{
		Project: "p", Repo: "r", State: "succeeded",
	}))
	assert.Equal(t, int32(3), calls.Load(), "should retry on 5xx")

	got := labelledCounterValue(t, mx, "cmr_callback_retries_total", "knowledge_status")
	assert.InDelta(t, 2.0, got, 0, "two retries must increment the knowledge_status label")
}

// TestVerifyAutonomous_BearerFallback_IncrementsCounter pins the
// cmr_callback_bearer_fallback_total counter so dashboards alarm any
// time the deprecated path is taken in prod. Every Bearer call must
// increment by one, independent of the rate-limited log.
func TestVerifyAutonomous_BearerFallback_IncrementsCounter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":true}`))
	}))
	defer srv.Close()

	mx := metrics.New()
	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger()).WithMetrics(mx)
	client.SetUseHMACForVerifyAutonomous(false)

	for i := range 3 {
		_, err := client.VerifyAutonomous(context.Background(), "p", "C-"+strconv.Itoa(i))
		require.NoError(t, err)
	}

	got := counterValue(t, mx, "cmr_callback_bearer_fallback_total")
	assert.InDelta(t, 3.0, got, 0, "every Bearer call must increment the audit counter")
}

// TestVerifyAutonomous_HMACPath_DoesNotIncrementBearerCounter is the
// negative complement: the secure HMAC path must NEVER touch the
// bearer-fallback counter. Regression guard against a future refactor
// that accidentally wires the counter into both branches.
func TestVerifyAutonomous_HMACPath_DoesNotIncrementBearerCounter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":true}`))
	}))
	defer srv.Close()

	mx := metrics.New()
	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger()).WithMetrics(mx)
	// HMAC is the default; do not call SetUseHMACForVerifyAutonomous.

	_, err := client.VerifyAutonomous(context.Background(), "p", "C-1")
	require.NoError(t, err)

	got := counterValue(t, mx, "cmr_callback_bearer_fallback_total")
	assert.InDelta(t, 0.0, got, 0, "HMAC path must not touch the bearer-fallback counter")
}

// TestVerifyAutonomous_BearerFallback_LogRateLimited drives the
// rate-limited Error log: many sequential Bearer calls must produce
// exactly one log entry per bearerFallbackLogInterval window. The
// counter still increments on every call.
func TestVerifyAutonomous_BearerFallback_LogRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":true}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mx := metrics.New()
	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", logger).WithMetrics(mx)
	client.SetUseHMACForVerifyAutonomous(false)

	const calls = 5

	for i := range calls {
		_, err := client.VerifyAutonomous(context.Background(), "p", "C-"+strconv.Itoa(i))
		require.NoError(t, err)
	}

	logged := strings.Count(buf.String(), "deprecated Bearer fallback")
	assert.Equal(t, 1, logged, "Bearer-fallback log must be rate-limited to one entry per window; got %d", logged)
	assert.Contains(t, buf.String(), "level=ERROR", "Bearer-fallback log must be at ERROR level")

	got := counterValue(t, mx, "cmr_callback_bearer_fallback_total")
	assert.InDelta(t, float64(calls), got, 0, "counter must increment on every Bearer call, independent of log rate-limit")
}

// TestVerifyAutonomous_BearerFallback_LogResetsAfterWindow confirms the
// rate-limiter releases after bearerFallbackLogInterval. We exercise it
// by directly rewinding the internal timestamp into the past, then
// making a single Bearer call and asserting a fresh Error log appears.
func TestVerifyAutonomous_BearerFallback_LogResetsAfterWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":true}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", logger)
	client.SetUseHMACForVerifyAutonomous(false)

	_, err := client.VerifyAutonomous(context.Background(), "p", "C-1")
	require.NoError(t, err)

	// Force the rate-limit window to expire by rewinding the last-log
	// timestamp far enough back that the next call's (now - last) check
	// clears the window.
	client.bearerFallbackLastLogUnix.Store(time.Now().Unix() - int64(2*bearerFallbackLogInterval/time.Second))

	_, err = client.VerifyAutonomous(context.Background(), "p", "C-2")
	require.NoError(t, err)

	logged := strings.Count(buf.String(), "deprecated Bearer fallback")
	assert.Equal(t, 2, logged, "rate-limiter must release after the window; expected two log entries, got %d", logged)
}

// TestVerifyAutonomous_BearerFallback_NilMetricsSafe ensures the Bearer
// audit path tolerates a Client constructed without WithMetrics. A nil
// metrics bundle must not panic.
func TestVerifyAutonomous_BearerFallback_NilMetricsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":false}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())
	client.SetUseHMACForVerifyAutonomous(false)

	autonomous, err := client.VerifyAutonomous(context.Background(), "p", "C-1")
	require.NoError(t, err)
	assert.False(t, autonomous)
}

// TestSanitizeURLForError_StripsUserinfo pins Fix W4: a misconfigured base
// URL with embedded credentials (https://user:token@host/path) must NEVER
// leak the userinfo into error messages. The sanitised form keeps only
// scheme + host + path; query, fragment and userinfo are dropped.
func TestSanitizeURLForError_StripsUserinfo(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "userinfo with password",
			in:   "https://user:secret-token@contextmatrix.example.com/api/runner/status",
			want: "https://contextmatrix.example.com/api/runner/status",
		},
		{
			name: "userinfo without password",
			in:   "https://bare-user@contextmatrix.example.com/api/v1/cards/p/c",
			want: "https://contextmatrix.example.com/api/v1/cards/p/c",
		},
		{
			name: "userinfo with query and fragment",
			in:   "https://u:p@host.example/api/x?q=token#frag",
			want: "https://host.example/api/x",
		},
		{
			name: "no userinfo, query stripped",
			in:   "https://host.example/api/x?token=leaked",
			want: "https://host.example/api/x",
		},
		{
			name: "unparseable",
			in:   "::::nope",
			want: "<invalid-url>",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeURLForError(c.in)
			assert.Equal(t, c.want, got, "sanitised URL must not leak userinfo / query / fragment")
			assert.NotContains(t, got, "secret-token", "secret must never appear in sanitised URL")
			assert.NotContains(t, got, "bare-user", "userinfo must never appear in sanitised URL")
			assert.NotContains(t, got, "token=leaked", "query must never appear in sanitised URL")
		})
	}
}

// TestIsClientError_TreatsCtxErrorsAsTerminal pins Fix W3: errors wrapping
// context.Canceled or context.DeadlineExceeded must short-circuit the retry
// loop so the next attempt does not burn a backoff before the ctx.Done()
// select catches the cancellation.
func TestIsClientError_TreatsCtxErrorsAsTerminal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"raw context.Canceled", context.Canceled, true},
		{"raw context.DeadlineExceeded", context.DeadlineExceeded, true},
		{"wrapped context.Canceled",
			&wrappedErr{inner: context.Canceled}, true},
		{"wrapped context.DeadlineExceeded",
			&wrappedErr{inner: context.DeadlineExceeded}, true},
		{"4xx Error stays a client error",
			newError("https://host.example/x", http.StatusBadRequest, []byte("bad")), true},
		{"5xx Error is not a client error",
			newError("https://host.example/x", http.StatusInternalServerError, []byte("oops")), false},
		{"unrelated error",
			io.ErrUnexpectedEOF, false},
		{"nil",
			nil, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isClientError(c.err))
		})
	}
}

// wrappedErr is a minimal errors.Is-friendly wrapper so we can assert that
// isClientError unwraps before checking.
type wrappedErr struct {
	inner error
}

func (e *wrappedErr) Error() string { return "wrapped: " + e.inner.Error() }
func (e *wrappedErr) Unwrap() error { return e.inner }

// TestReportStatus_ContextCanceled_NoRetrySleep verifies that a cancelled
// context returns promptly: the retry loop must short-circuit on the
// first ctx-aware error instead of sleeping out the backoff. Fix W3 in
// REVIEW.md.
//
// Strategy: point the client at an unreachable localhost port so every
// attempt fails fast with a ctx-wrapped dial error. Without the W3 fix the
// loop would sleep one backoff (1s) before the next ctx.Done() select
// caught the cancellation; with W3 the loop returns within ~ctx-timeout.
func TestReportStatus_ContextCanceled_NoRetrySleep(t *testing.T) {
	// 127.0.0.1:1 is a port no listener can bind to; Dial fails immediately
	// with a connection-refused / no-such-host error.
	unreachable := "http://127.0.0.1:1"

	client := NewClient(unreachable, "test-secret-key-that-is-long-enough", testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := client.ReportStatus(ctx, "PROJ-1", "p", "running", "")
	elapsed := time.Since(start)

	require.Error(t, err)
	// Pre-W3, a maxRetries=3 run burns ~3s on backoff after each failed
	// attempt because isClientError returned false on ctx-wrapped errors.
	// Post-W3 the first ctx.Canceled / DeadlineExceeded short-circuits the
	// loop. Allow generous slack (700ms) for CI scheduling.
	assert.Less(t, elapsed, 700*time.Millisecond,
		"ReportStatus must short-circuit on ctx cancellation; took %s", elapsed)
}

// TestVerifyAutonomous_TransientRetrySucceeds verifies Fix W2: the first
// transient 5xx is retried and the eventual 2xx is returned without
// surfacing the prior failure to the caller. The retry metric for the
// verify_autonomous label increments per failed attempt, mirroring the
// POST callbacks.
func TestVerifyAutonomous_TransientRetrySucceeds(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"transient"}`))

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"autonomous":true}`))
	}))
	defer srv.Close()

	mx := metrics.New()
	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger()).WithMetrics(mx)

	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.NoError(t, err, "transient 5xx must be retried and eventually return success")
	assert.True(t, autonomous)
	assert.Equal(t, int32(3), calls.Load(), "expected 2 retries + 1 success = 3 calls")

	got := labelledCounterValue(t, mx, "cmr_callback_retries_total", "verify_autonomous")
	assert.InDelta(t, 2.0, got, 0,
		"two retries (first two attempts failed) must increment the verify_autonomous label")
}

// TestVerifyAutonomous_4xxNoRetry verifies Fix W2: a 4xx response is a
// client error and must NOT be retried — mirrors ReportStatus's
// isClientError short-circuit.
func TestVerifyAutonomous_4xxNoRetry(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())

	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.Error(t, err)
	assert.False(t, autonomous)
	assert.Contains(t, err.Error(), "404")
	assert.Equal(t, int32(1), calls.Load(), "4xx must not be retried")
}

// TestVerifyAutonomous_CtxCancelShortCircuits verifies Fix W2: a cancelled
// context returns promptly without burning a full retry ladder. Without
// the isClientError check for ctx errors a cancelled VerifyAutonomous
// would sleep through 1s + 2s of backoff before exiting.
func TestVerifyAutonomous_CtxCancelShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := NewClient(srv.URL, "test-secret-key-that-is-long-enough", testLogger())

	start := time.Now()
	autonomous, err := client.VerifyAutonomous(ctx, "p", "C-1")
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.False(t, autonomous)
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 500*time.Millisecond,
		"cancelled ctx must short-circuit the retry loop; took %s", elapsed)
}
