// Package tracing initialises an OpenTelemetry tracer provider.
//
// By default the provider uses a no-op exporter so local development does not
// need a collector. If OTEL_EXPORTER_OTLP_ENDPOINT is set (either the base or
// traces-specific env var), an OTLP/HTTP exporter is configured and spans are
// exported asynchronously. All lookups happen through the standard OTEL env
// vars — nothing is hard-coded here. Operators plug in a real collector by
// setting env vars on the systemd unit.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// ServiceName is published on every span.
const ServiceName = "contextmatrix-runner"

// Shutdown flushes buffered spans and releases exporter resources. Call it
// from main on shutdown with a short timeout. A nil Shutdown is a no-op.
type Shutdown func(context.Context) error

// Init configures the global tracer provider. When no OTLP endpoint is set in
// the environment, a provider with no exporter is installed — spans still get
// created but are dropped, so instrumentation code paths stay exercised in
// unit tests without requiring a collector.
//
// Returns a Shutdown func that must be invoked on process exit to flush spans.
func Init(ctx context.Context, logger *slog.Logger) (Shutdown, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(ServiceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
	}

	endpoint := firstNonEmpty(
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	)

	var exporter *otlptrace.Exporter

	if endpoint != "" {
		// Respect OTEL_EXPORTER_OTLP_* env vars via otlptracehttp defaults.
		exp, expErr := otlptracehttp.New(ctx)
		if expErr != nil {
			return nil, fmt.Errorf("init otlp exporter: %w", expErr)
		}

		exporter = exp
		opts = append(opts, sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(5*time.Second)))

		if logger != nil {
			logger.Info("otel tracing enabled", "endpoint", endpoint)
		}
	} else if logger != nil {
		logger.Info("otel tracing: no exporter configured (local no-op mode)")
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		if err := tp.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutdown tracer provider: %w", err)
		}

		if exporter != nil {
			if err := exporter.Shutdown(ctx); err != nil {
				return fmt.Errorf("shutdown otlp exporter: %w", err)
			}
		}

		return nil
	}, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}

	return ""
}
