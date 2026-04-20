package callback

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
		assert.True(t, cmhmac.VerifySignatureWithTimestamp(apiKey, sig, tsHeader, body, cmhmac.DefaultMaxClockSkew))

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
		assert.True(t, cmhmac.VerifySignatureWithTimestamp(apiKey, hexSig, ts, body, cmhmac.DefaultMaxClockSkew))

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
		_, _ = w.Write([]byte(`{"id":"PROJ-001","autonomous":true}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "any-key", testLogger())
	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.NoError(t, err)
	assert.True(t, autonomous)

	// Must use GET, not POST, so CM does not re-trigger the promote webhook.
	assert.Equal(t, http.MethodGet, receivedMethod)
	assert.Equal(t, "/api/projects/my-project/cards/PROJ-001", receivedPath)
}

func TestVerifyAutonomous_False(t *testing.T) {
	// autonomous=false means the card has not been promoted yet — caller should
	// refuse to write stdin.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"PROJ-001","autonomous":false}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "any-key", testLogger())
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

	client := NewClient(srv.URL, "any-key", testLogger())
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

	client := NewClient(srv.URL, "any-key", testLogger())
	autonomous, err := client.VerifyAutonomous(context.Background(), "my-project", "PROJ-001")
	require.Error(t, err)
	assert.False(t, autonomous)
	assert.Contains(t, err.Error(), "404")
}
