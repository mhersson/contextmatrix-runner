// Package metrics defines the Prometheus metric set exposed by the runner.
//
// All metrics are registered on a dedicated prometheus.Registerer (shared
// within a process, but constructed by the caller) rather than the default
// global registry. This keeps tests hermetic — each test can create its own
// *Metrics with a fresh registry and assert against that.
//
// Label cardinality is kept finite on purpose: we never label by card_id or
// project. Broadcaster drops use a static "all" label so the series count
// stays O(1).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics bundles every Prometheus collector exposed by the runner. It is
// constructed once at process startup and injected into components that need
// to observe. Components never reach for a global.
type Metrics struct {
	// Registry is the registerer these collectors live on. Exposed so the
	// HTTP handler can be wired to the same registry.
	Registry *prometheus.Registry

	WebhookRequestsTotal    *prometheus.CounterVec
	WebhookRequestDuration  *prometheus.HistogramVec
	RunningContainers       prometheus.Gauge
	CallbackRetriesTotal    *prometheus.CounterVec
	BroadcasterDropsTotal   *prometheus.CounterVec
	ReplayCacheHitsTotal    prometheus.Counter
	PreflightLastSuccessSec prometheus.Gauge
}

// New registers every runner metric on a fresh registry and returns the bundle.
// Use this in main and in tests. The registry is isolated from the default
// global registry so repeated calls in tests do not panic on duplicate
// registration.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	factory := promauto.With(reg)

	return &Metrics{
		Registry: reg,

		WebhookRequestsTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "cmr_webhook_requests_total",
				Help: "Total webhook requests processed, labelled by endpoint, HTTP status, and error code.",
			},
			[]string{"endpoint", "status", "code"},
		),

		WebhookRequestDuration: factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "cmr_webhook_request_duration_seconds",
				Help:    "Wall-clock duration of webhook requests, in seconds.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"endpoint"},
		),

		RunningContainers: factory.NewGauge(prometheus.GaugeOpts{
			Name: "cmr_running_containers",
			Help: "Number of containers currently tracked as running.",
		}),

		CallbackRetriesTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "cmr_callback_retries_total",
				Help: "Total ContextMatrix callback retry attempts.",
			},
			[]string{"endpoint"},
		),

		BroadcasterDropsTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "cmr_broadcaster_drops_total",
				Help: "Total log entries dropped for slow SSE subscribers. The card_id_redacted label is always \"all\" to bound cardinality.",
			},
			[]string{"card_id_redacted"},
		),

		ReplayCacheHitsTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "cmr_replay_cache_hits_total",
			Help: "Total webhook replay cache hits (duplicate message_id rejected).",
		}),

		PreflightLastSuccessSec: factory.NewGauge(prometheus.GaugeOpts{
			Name: "cmr_preflight_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful preflight check.",
		}),
	}
}
