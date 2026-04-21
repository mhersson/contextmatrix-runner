// Package metrics defines the Prometheus metric set exposed by the runner.
//
// All metrics are registered on a dedicated prometheus.Registerer (shared
// within a process, but constructed by the caller) rather than the default
// global registry. This keeps tests hermetic — each test can create its own
// *Metrics with a fresh registry and assert against that.
//
// Label cardinality is kept finite on purpose: we never label by card_id or
// project. Broadcaster drops use a static "all" label so the series count
// stays O(1). Panic-recovered counts are bucketed by a small set of goroutine
// names defined as constants below.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Known goroutine labels for the panic_recovered counter. Using constants
// prevents accidental cardinality blowup from ad-hoc strings at call sites.
const (
	GoroutineRun          = "run"
	GoroutineStreamStdout = "stream_stdout"
	GoroutineStreamStderr = "stream_stderr"
	GoroutineLogparser    = "logparser"
)

// Container-exit outcomes for cmr_container_duration_seconds.
const (
	OutcomeSuccess     = "success"
	OutcomeFailure     = "failure"
	OutcomeTimeout     = "timeout"
	OutcomeKilled      = "killed"
	OutcomeIdleTimeout = "idle_timeout"
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
	ContainerDuration       *prometheus.HistogramVec
	RunningContainers       prometheus.Gauge
	CallbackRetriesTotal    *prometheus.CounterVec
	BroadcasterDropsTotal   *prometheus.CounterVec
	PanicRecoveredTotal     *prometheus.CounterVec
	ReplayCacheHitsTotal    prometheus.Counter
	PreflightLastSuccessSec prometheus.Gauge
	// DNSLookupTimeoutsTotal counts buildExtraHosts() MCP-hostname lookups
	// that exceeded the 2s deadline. Label-free so cardinality stays O(1);
	// no per-host label (attacker-influenced input would explode series).
	DNSLookupTimeoutsTotal prometheus.Counter
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

		ContainerDuration: factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "cmr_container_duration_seconds",
				Help: "Wall-clock container lifetime from start to exit, in seconds.",
				Buckets: []float64{
					1, 5, 15, 30, 60,
					300, 600, 1800, 3600, 7200,
				},
			},
			[]string{"outcome"},
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

		PanicRecoveredTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "cmr_panic_recovered_total",
				Help: "Total recovered panics, labelled by the goroutine in which the panic occurred.",
			},
			[]string{"goroutine"},
		),

		ReplayCacheHitsTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "cmr_replay_cache_hits_total",
			Help: "Total webhook replay cache hits (duplicate message_id rejected).",
		}),

		PreflightLastSuccessSec: factory.NewGauge(prometheus.GaugeOpts{
			Name: "cmr_preflight_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful preflight check.",
		}),

		DNSLookupTimeoutsTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "cmr_dns_lookup_timeouts_total",
			Help: "Total MCP-hostname DNS lookups that exceeded the spawn-path deadline.",
		}),
	}
}
