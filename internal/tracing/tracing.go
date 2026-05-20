// Package tracing initialises an OpenTelemetry tracer provider.
//
// By default the provider uses a no-op exporter so local development does not
// need a collector. If OTEL_EXPORTER_OTLP_ENDPOINT is set (either the base or
// traces-specific env var), an OTLP/HTTP exporter is configured and spans are
// exported asynchronously. All lookups happen through the standard OTEL env
// vars — nothing is hard-coded here. Operators plug in a real collector by
// setting env vars on the systemd unit.
//
// Sampling: by default the tracer samples 100% of traces
// (ParentBased(AlwaysOn)) for backwards-compatibility. Operators with
// high-rate /logs streams should reduce overhead by setting
// OTEL_TRACES_SAMPLER_ARG to a value between 0.0 and 1.0 — interpreted as the
// TraceIDRatio sampler fraction. Values outside [0.0, 1.0] are clamped to
// 1.0 with a warning, and a non-numeric value is rejected the same way.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
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

// otlpConstructTimeout bounds the time spent constructing the OTLP exporter
// during startup. The exporter itself does not perform a network handshake on
// construction in current SDK versions, but the timeout is a defence-in-depth
// against a future change that adds one (or any blocking DNS resolution
// happening inside the SDK's transport setup).
const otlpConstructTimeout = 10 * time.Second

// defaultSamplerRatio is applied when OTEL_TRACES_SAMPLER_ARG is unset.
// 1.0 matches the previous AlwaysOn behaviour so existing deployments do not
// silently lose spans on upgrade.
const defaultSamplerRatio = 1.0

// Shutdown flushes buffered spans and releases exporter resources. Call it
// from main on shutdown with a short timeout. A nil Shutdown is a no-op.
type Shutdown func(context.Context) error

// Init configures the global tracer provider. When no OTLP endpoint is set in
// the environment, a provider with no exporter is installed — spans still get
// created but are dropped, so instrumentation code paths stay exercised in
// unit tests without requiring a collector.
//
// Endpoint validation: the value of
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT / OTEL_EXPORTER_OTLP_ENDPOINT must parse
// as an http/https URL with a non-empty host and no embedded credentials.
// Invalid endpoints log an error and fall back to no-op mode rather than
// crashing the runner at boot — a misconfigured collector should not stop
// production traffic.
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
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(parseSamplerRatio(logger)))),
	}

	endpoint := firstNonEmpty(
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	)

	var exporter *otlptrace.Exporter

	if endpoint != "" {
		if err := validateOTLPEndpoint(endpoint); err != nil {
			if logger != nil {
				logger.Error("otel tracing disabled: invalid endpoint",
					"endpoint", endpoint, "error", err)
			}
		} else {
			// Construction can in principle do DNS / handshake work in
			// future SDK versions. Bound it so a broken collector cannot
			// stall the runner's boot indefinitely.
			constructCtx, cancel := context.WithTimeout(ctx, otlpConstructTimeout)

			exp, expErr := otlptracehttp.New(constructCtx)

			cancel()

			if expErr != nil {
				// Fail-open: log and continue in no-op mode rather than
				// crashing on a transient collector outage at boot.
				if logger != nil {
					logger.Error("otel tracing disabled: exporter construction failed",
						"endpoint", endpoint, "error", expErr)
				}
			} else {
				exporter = exp
				opts = append(opts, sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(5*time.Second)))

				if logger != nil {
					logger.Info("otel tracing enabled", "endpoint", endpoint)
				}
			}
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

// validateOTLPEndpoint checks that endpoint is a syntactically valid
// http/https URL with a non-empty host and no embedded credentials. Returns
// a descriptive error otherwise. Defence in depth: we don't want an operator
// typo to crash the runner, but we also don't want to silently feed garbage
// into the OTLP transport (which on some collector versions will log nothing
// and just drop every span).
func validateOTLPEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("scheme must be http or https (got %q)", u.Scheme)
	}

	if u.Host == "" {
		return fmt.Errorf("host is required")
	}

	if u.User != nil {
		return fmt.Errorf("endpoint must not embed userinfo credentials")
	}

	return nil
}

// parseSamplerRatio reads OTEL_TRACES_SAMPLER_ARG and returns a value in
// [0.0, 1.0]. Out-of-range or unparseable values fall back to
// defaultSamplerRatio with a warning so a misconfigured collector cannot
// silently disable sampling.
func parseSamplerRatio(logger *slog.Logger) float64 {
	raw := os.Getenv("OTEL_TRACES_SAMPLER_ARG")
	if raw == "" {
		return defaultSamplerRatio
	}

	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		if logger != nil {
			logger.Warn("OTEL_TRACES_SAMPLER_ARG: parse error, falling back to default",
				"value", raw, "error", err, "default", defaultSamplerRatio)
		}

		return defaultSamplerRatio
	}

	// strconv.ParseFloat happily returns NaN / ±Inf for the literal strings
	// "NaN" and "Inf" (and the range check below treats NaN as both
	// "not < 0" and "not > 1"), so a literal "NaN" would slip past every
	// guard and reach TraceIDRatioBased with undefined behaviour. Reject
	// non-finite values explicitly.
	if math.IsNaN(v) || math.IsInf(v, 0) {
		if logger != nil {
			logger.Warn("OTEL_TRACES_SAMPLER_ARG: non-finite value, falling back to default",
				"value", raw, "default", defaultSamplerRatio)
		}

		return defaultSamplerRatio
	}

	if v < 0.0 || v > 1.0 {
		if logger != nil {
			logger.Warn("OTEL_TRACES_SAMPLER_ARG: value out of [0.0, 1.0], falling back to default",
				"value", v, "default", defaultSamplerRatio)
		}

		return defaultSamplerRatio
	}

	return v
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}

	return ""
}
