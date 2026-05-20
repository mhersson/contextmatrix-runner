package tracing_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/mhersson/contextmatrix-runner/internal/tracing"
)

// snapshotOTEL captures the global tracer provider and text-map propagator
// and registers a t.Cleanup that restores them. Without this, every test
// that calls tracing.Init leaks into the global OTEL state and bleeds into
// later tests in unpredictable order — the previous shape relied on the
// fact that all tests happened to install the same no-op provider, which
// breaks the moment we add a test that exercises a real exporter or
// propagator.
func snapshotOTEL(t *testing.T) {
	t.Helper()

	prevProvider := otel.GetTracerProvider()
	prevPropagator := otel.GetTextMapPropagator()

	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevPropagator)
	})
}

// TestInit_NoOpModeWhenNoEndpoint verifies that Init succeeds and returns a
// working Shutdown when no OTLP endpoint is configured. Callers create spans
// safely even with no exporter.
func TestInit_NoOpModeWhenNoEndpoint(t *testing.T) {
	snapshotOTEL(t)

	// Make doubly sure the env var is unset for this process.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	shutdown, err := tracing.Init(context.Background(), nil)
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	// Create a span — must not panic even though there's no exporter.
	_, span := otel.Tracer("test").Start(context.Background(), "noop-span")
	assert.NotNil(t, span)
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, shutdown(ctx))
}

// captureLogger returns a logger that buffers JSON-formatted output so tests
// can assert which messages were emitted.
func captureLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return logger, buf
}

// TestInit_InvalidEndpoints verifies that endpoint values which are not
// safe HTTP/HTTPS URLs do NOT crash the runner — instead they log an error
// and fall back to no-op mode. The previous shape passed any string through
// to otlptracehttp.New, leaking misconfiguration into the SDK.
func TestInit_InvalidEndpoints(t *testing.T) {
	snapshotOTEL(t)

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	cases := []struct {
		name     string
		endpoint string
		wantLog  string
	}{
		{
			name:     "scheme missing",
			endpoint: "collector.example.com:4318",
			wantLog:  "scheme must be http or https",
		},
		{
			name:     "ftp scheme rejected",
			endpoint: "ftp://collector.example.com",
			wantLog:  "scheme must be http or https",
		},
		{
			name:     "empty host",
			endpoint: "http://",
			wantLog:  "host is required",
		},
		{
			name:     "embeds userinfo credentials",
			endpoint: "http://user:pass@collector.example.com:4318",
			wantLog:  "must not embed userinfo credentials",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshotOTEL(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tc.endpoint)

			logger, buf := captureLogger(t)
			shutdown, err := tracing.Init(context.Background(), logger)
			// Must NOT crash: misconfigured collector falls back to no-op.
			require.NoError(t, err)
			require.NotNil(t, shutdown)

			assert.Contains(t, buf.String(), "otel tracing disabled")
			assert.Contains(t, buf.String(), tc.wantLog)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			require.NoError(t, shutdown(ctx))
		})
	}
}

// TestInit_ValidEndpointAccepted verifies that a syntactically valid OTLP
// endpoint reaches the exporter construction path. We don't actually run a
// collector — we only assert no validation error and no "disabled" log line.
func TestInit_ValidEndpointAccepted(t *testing.T) {
	snapshotOTEL(t)

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.example.com:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	logger, buf := captureLogger(t)
	shutdown, err := tracing.Init(context.Background(), logger)
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	assert.Contains(t, buf.String(), "otel tracing enabled")
	assert.NotContains(t, buf.String(), "otel tracing disabled")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, shutdown(ctx))
}

// TestInit_SamplerArg_InvalidFallsBack verifies that a non-numeric or
// out-of-range OTEL_TRACES_SAMPLER_ARG falls back to the default ratio and
// logs a warning rather than crashing.
func TestInit_SamplerArg_InvalidFallsBack(t *testing.T) {
	snapshotOTEL(t)

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	cases := []struct {
		name string
		arg  string
	}{
		{"not a number", "abc"},
		{"negative", "-0.5"},
		{"greater than one", "1.5"},
		// strconv.ParseFloat accepts these spellings and returns NaN /
		// ±Inf — both must be rejected, since NaN slips past the
		// "< 0.0 || > 1.0" range check (every comparison with NaN is
		// false) and would produce undefined sampler behaviour.
		{"nan literal", "NaN"},
		{"positive infinity", "+Inf"},
		{"negative infinity", "-Inf"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshotOTEL(t)
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", tc.arg)

			logger, buf := captureLogger(t)
			shutdown, err := tracing.Init(context.Background(), logger)
			require.NoError(t, err)
			require.NotNil(t, shutdown)

			assert.Contains(t, buf.String(), "OTEL_TRACES_SAMPLER_ARG")
			assert.Contains(t, buf.String(), "falling back to default")

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			require.NoError(t, shutdown(ctx))
		})
	}
}

// TestInit_SamplerArg_ValidRatioAccepted verifies that a valid ratio in
// [0.0, 1.0] does NOT emit the fall-back warning and Init succeeds. We can't
// directly inspect the sampler from outside the package, so we assert via
// the absence of warning + a successful init.
func TestInit_SamplerArg_ValidRatioAccepted(t *testing.T) {
	snapshotOTEL(t)

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.25")

	logger, buf := captureLogger(t)
	shutdown, err := tracing.Init(context.Background(), logger)
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	assert.NotContains(t, buf.String(), "falling back to default",
		"valid sampler ratio must not trigger the fall-back warning")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, shutdown(ctx))
}
