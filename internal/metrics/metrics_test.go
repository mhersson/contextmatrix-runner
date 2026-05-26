package metrics_test

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/metrics"
)

func TestNew_RegistersAllMetrics(t *testing.T) {
	m := metrics.New()
	require.NotNil(t, m)

	// Touch each counter/histogram so it appears in the registry output.
	m.WebhookRequestsTotal.WithLabelValues("trigger", "200", "success").Inc()
	m.WebhookRequestDuration.WithLabelValues("trigger").Observe(0.1)
	m.ContainerDuration.WithLabelValues(metrics.OutcomeSuccess).Observe(30)
	m.RunningContainers.Set(1)
	m.CallbackRetriesTotal.WithLabelValues("status").Inc()
	m.CallbackBearerFallbackTotal.Inc()
	m.BroadcasterDropsTotal.Inc()
	m.PanicRecoveredTotal.WithLabelValues(metrics.GoroutineRun).Inc()
	m.ReplayCacheHitsTotal.Inc()
	m.PreflightLastSuccessSec.Set(1_700_000_000)
	m.DNSLookupTimeoutsTotal.Inc()
	m.ChatRollbackFailuresTotal.Inc()

	families, err := m.Registry.Gather()
	require.NoError(t, err)

	got := make(map[string]bool, len(families))
	for _, f := range families {
		got[f.GetName()] = true
	}

	want := []string{
		"cmr_webhook_requests_total",
		"cmr_webhook_request_duration_seconds",
		"cmr_container_duration_seconds",
		"cmr_running_containers",
		"cmr_callback_retries_total",
		"cmr_callback_bearer_fallback_total",
		"cmr_broadcaster_drops_total",
		"cmr_panic_recovered_total",
		"cmr_replay_cache_hits_total",
		"cmr_preflight_last_success_timestamp_seconds",
		"cmr_dns_lookup_timeouts_total",
		"cmr_chat_rollback_failures_total",
	}

	for _, name := range want {
		assert.True(t, got[name], "metric %q not registered", name)
	}
}

// TestNew_RegistersGoAndProcessCollectors verifies that the standard
// runtime + process collectors are wired onto the dedicated registry. The
// runner never registers on the default global registry, so a missing
// MustRegister call would silently drop go_* / process_* series from
// /metrics — and operators alerting on goroutine-leak or memory growth
// would lose every signal without any error surfaced at boot.
func TestNew_RegistersGoAndProcessCollectors(t *testing.T) {
	m := metrics.New()
	require.NotNil(t, m)

	families, err := m.Registry.Gather()
	require.NoError(t, err)

	want := map[string]bool{
		"go_goroutines":           false,
		"go_memstats_alloc_bytes": false,
	}

	// ProcessCollector reads /proc/self/stat and only emits process_*
	// series on Linux; on macOS / Windows the collector registers but
	// Collect() returns no families, so Gather() never sees them.
	// Production deployments are Linux containers, so we still assert
	// the process_* series there; on dev machines we just verify the
	// go_* collectors. (The map value is "has this been observed by
	// Gather?" — the loop below flips it to true when found.)
	if runtime.GOOS == "linux" {
		want["process_cpu_seconds_total"] = false
		want["process_resident_memory_bytes"] = false
	}

	for _, f := range families {
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
	}

	for name, seen := range want {
		assert.True(t, seen, "expected runtime/process series %q to be registered", name)
	}
}

func TestNew_MultipleCallsUseIsolatedRegistries(t *testing.T) {
	// Two independent registries must not conflict — the whole point of
	// avoiding the default global registry.
	m1 := metrics.New()
	m2 := metrics.New()

	assert.NotSame(t, m1.Registry, m2.Registry)

	m1.ReplayCacheHitsTotal.Inc()

	// Gather should succeed on both without duplicate-registration panics.
	_, err := m1.Registry.Gather()
	require.NoError(t, err)
	_, err = m2.Registry.Gather()
	require.NoError(t, err)
}

// TestNormalizeEndpoint_Allowlist verifies that every well-known path round
// trips, and that paths outside the allowlist collapse to "other". This is
// the cardinality firewall for both metric labels and trace span names —
// any drift between the registered routes in webhook.Register and this
// allowlist must surface in tests.
func TestNormalizeEndpoint_Allowlist(t *testing.T) {
	allowed := []string{
		"/trigger",
		"/kill",
		"/stop-all",
		"/message",
		"/promote",
		"/end-session",
		"/refresh-knowledge",
		"/chat/start",
		"/chat/end",
		"/logs",
		"/containers",
		"/health",
		"/readyz",
		"/metrics",
		"/ready",
	}

	for _, p := range allowed {
		assert.Equal(t, p, metrics.NormalizeEndpoint(p), "allowlisted path %q must round-trip", p)
	}
}

func TestNormalizeEndpoint_UnknownPathsCollapse(t *testing.T) {
	unknown := []string{
		"/nonexistent",
		"/admin/secret",
		"/../../etc/passwd",
		"/trigger/extra/path",
		"",
		"/",
		"/TRIGGER", // case-sensitive: known paths are lowercase
	}

	for _, p := range unknown {
		assert.Equal(t, "other", metrics.NormalizeEndpoint(p), "unknown path %q must collapse to 'other'", p)
	}
}

func TestBroadcasterDropsIsBoundedCounter(t *testing.T) {
	m := metrics.New()

	// BroadcasterDropsTotal is an unlabeled Counter. Emit many drops — the
	// series count must remain 1 by construction (no labels to vary).
	for range 1000 {
		m.BroadcasterDropsTotal.Inc()
	}

	families, err := m.Registry.Gather()
	require.NoError(t, err)

	for _, f := range families {
		if f.GetName() != "cmr_broadcaster_drops_total" {
			continue
		}

		assert.Len(t, f.Metric, 1, "broadcaster_drops_total must have a single series, got %d", len(f.Metric))

		return
	}

	t.Fatal("cmr_broadcaster_drops_total not found in registry")
}
