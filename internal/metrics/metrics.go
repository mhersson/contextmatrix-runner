// Package metrics defines the Prometheus metric set exposed by the runner.
//
// All metrics are registered on a dedicated prometheus.Registerer (shared
// within a process, but constructed by the caller) rather than the default
// global registry. This keeps tests hermetic — each test can create its own
// *Metrics with a fresh registry and assert against that.
//
// Label cardinality is kept finite on purpose: we never label by card_id or
// project. Broadcaster drops are an unlabeled Counter so the series count
// stays O(1). Panic-recovered counts are bucketed by a small set of
// goroutine names defined as constants below.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Known goroutine labels for the panic_recovered counter. Using constants
// prevents accidental cardinality blowup from ad-hoc strings at call sites.
//
// The streamLogs / StreamChatLogs *outer* goroutines drive logparser
// synchronously, so their panic-recovery defers bucket into GoroutineLogparser
// rather than introducing a separate outer-goroutine label. The stdcopy and
// stderr_scanner children run on their own goroutines and use the
// GoroutineStreamStdout / GoroutineStreamStderr labels.
const (
	GoroutineRun                   = "run"
	GoroutineStreamStdout          = "stream_stdout"
	GoroutineStreamStderr          = "stream_stderr"
	GoroutineLogparser             = "logparser"
	GoroutineMonitorDockerd        = "monitor_dockerd"
	GoroutineMaintenanceLoop       = "maintenance_loop"
	GoroutineRunningContainers     = "running_containers_gauge"
	GoroutineRunningStatusCallback = "running_status_callback"
	GoroutineSkillEngagedCallback  = "skill_engaged_callback"
	GoroutinePrimingWrite          = "priming_write"
	// GoroutineWaitAndCleanupChat buckets the wg-tracked goroutine spawned
	// by Manager.WaitAndCleanupChat. A panic there used to crash the runner
	// because the function was the only manager background goroutine without
	// the shared handlePanic defer.
	GoroutineWaitAndCleanupChat = "wait_and_cleanup_chat"
	// GoroutineIdleWatchdog buckets panics from Manager.runIdleWatchdog
	// (per-container idle-output watchdog).
	GoroutineIdleWatchdog = "idle_watchdog"
	// GoroutineDockerdMonitor buckets panics from the per-iteration body of
	// the dockerd health-monitor loop. The label is distinct from
	// GoroutineMonitorDockerd to avoid retroactively changing the meaning
	// of an existing series.
	GoroutineDockerdMonitor = "dockerd_monitor"
	// GoroutineTokenRefresher buckets panics recovered by the deferred
	// recover in tokenRefresher.Run. Pre-fix the recovered panic was logged
	// but never counted, so a wedged GitHub-token mint path could panic on
	// every iteration without showing up in cmr_panic_recovered_total.
	GoroutineTokenRefresher = "token_refresher"
)

// Container-exit outcomes for cmr_container_duration_seconds.
const (
	OutcomeSuccess     = "success"
	OutcomeFailure     = "failure"
	OutcomeTimeout     = "timeout"
	OutcomeKilled      = "killed"
	OutcomeIdleTimeout = "idle_timeout"
)

// endpointAllowlist enumerates the request paths the runner serves. Any
// inbound path outside this set collapses to "other" so a malicious or
// stray probe cannot inflate metric / span cardinality by spraying unique
// URLs at the listener.
//
// Keep this list in lockstep with the routes registered in
// internal/webhook/handler.go's Register() — if you add a new endpoint
// there, add it here too or it will show up as "other" in metrics and
// traces.
var endpointAllowlist = map[string]struct{}{
	"/trigger":           {},
	"/kill":              {},
	"/stop-all":          {},
	"/message":           {},
	"/promote":           {},
	"/end-session":       {},
	"/refresh-knowledge": {},
	"/chat/start":        {},
	"/chat/end":          {},
	"/logs":              {},
	"/containers":        {},
	"/health":            {},
	"/readyz":            {},
	"/metrics":           {},
	"/ready":             {},
}

// NormalizeEndpoint collapses an arbitrary request path to one of the
// runner's well-known endpoints, or "other" for unknown paths. Used by both
// the metrics-recording middleware (to bound label cardinality on
// cmr_webhook_requests_total) and the otelhttp span-name formatter (to
// bound trace cardinality at the same allowlist).
//
// Paths are returned verbatim (no leading-slash stripping) so callers can
// use the result directly as a span suffix; for the existing metric label
// shape ("trigger" rather than "/trigger"), strip the leading "/" at the
// call site.
func NormalizeEndpoint(path string) string {
	if _, ok := endpointAllowlist[path]; ok {
		return path
	}

	return "other"
}

// Metrics bundles every Prometheus collector exposed by the runner. It is
// constructed once at process startup and injected into components that need
// to observe. Components never reach for a global.
type Metrics struct {
	// Registry is the registerer these collectors live on. Exposed so the
	// HTTP handler can be wired to the same registry.
	Registry *prometheus.Registry

	WebhookRequestsTotal   *prometheus.CounterVec
	WebhookRequestDuration *prometheus.HistogramVec
	ContainerDuration      *prometheus.HistogramVec
	RunningContainers      prometheus.Gauge
	CallbackRetriesTotal   *prometheus.CounterVec
	// CallbackBearerFallbackTotal counts every VerifyAutonomous call that
	// took the deprecated `Authorization: Bearer <apiKey>` path instead of
	// HMAC-signing. Label-free so cardinality stays O(1). Operators must
	// alert on any non-zero value: it means the shared HMAC secret is
	// being shipped in clear over the Authorization header.
	CallbackBearerFallbackTotal prometheus.Counter
	BroadcasterDropsTotal       prometheus.Counter
	PanicRecoveredTotal         *prometheus.CounterVec
	ReplayCacheHitsTotal        prometheus.Counter
	PreflightLastSuccessSec     prometheus.Gauge
	// DNSLookupTimeoutsTotal counts buildExtraHosts() MCP-hostname lookups
	// that exceeded the 2s deadline. Label-free so cardinality stays O(1);
	// no per-host label (attacker-influenced input would explode series).
	DNSLookupTimeoutsTotal prometheus.Counter

	// ChatRollbackFailuresTotal counts /chat/start rollback paths where the
	// follow-up container Stop returned an error after a tracker reservation
	// or stdin-attach failure. Operator alarm: a non-zero value means we have
	// orphaned chat containers leaking until the 2h sweep. Label-free so
	// cardinality stays O(1). Fix W7 in REVIEW.md.
	ChatRollbackFailuresTotal prometheus.Counter
}

// New registers every runner metric on a fresh registry and returns the bundle.
// Use this in main and in tests. The registry is isolated from the default
// global registry so repeated calls in tests do not panic on duplicate
// registration.
//
// In addition to the runner-specific cmr_* series, the registry also includes
// the standard Go runtime collector (go_goroutines, go_memstats_*, …) and the
// process collector (process_cpu_seconds_total, process_resident_memory_bytes,
// …). The dedicated-registry shape skipped the default-registry registration
// the prometheus client library does automatically; without these explicit
// adds, /metrics would expose only cmr_* metrics and lose every standard
// runtime / process series operators rely on for goroutine-leak,
// memory-growth and CPU-saturation alerting.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	factory := promauto.With(reg)

	// Register the runtime and process collectors directly on the
	// dedicated registry. promauto would also work but explicit
	// MustRegister keeps the intent obvious next to the cmr_* registrations
	// below.
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

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

		CallbackBearerFallbackTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "cmr_callback_bearer_fallback_total",
			Help: "Total VerifyAutonomous calls that used the deprecated Bearer fallback (apiKey leaked over Authorization header). Any non-zero value is an operator alarm.",
		}),

		BroadcasterDropsTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "cmr_broadcaster_drops_total",
			Help: "Total log entries dropped for slow SSE subscribers. Unlabeled to keep series cardinality at O(1).",
		}),

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

		ChatRollbackFailuresTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "cmr_chat_rollback_failures_total",
			Help: "Total /chat/start rollback Stop failures. Any non-zero value means a chat container is orphaned and will leak until the 2h sweep.",
		}),
	}
}
