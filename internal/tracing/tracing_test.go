package tracing_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/mhersson/contextmatrix-runner/internal/tracing"
)

// TestInit_NoOpModeWhenNoEndpoint verifies that Init succeeds and returns a
// working Shutdown when no OTLP endpoint is configured. Callers create spans
// safely even with no exporter.
func TestInit_NoOpModeWhenNoEndpoint(t *testing.T) {
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
