package metrics_test

import (
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
	m.RunningContainers.Set(1)
	m.CallbackRetriesTotal.WithLabelValues("status").Inc()
	m.BroadcasterDropsTotal.WithLabelValues("all").Inc()
	m.ReplayCacheHitsTotal.Inc()
	m.PreflightLastSuccessSec.Set(1_700_000_000)

	families, err := m.Registry.Gather()
	require.NoError(t, err)

	got := make(map[string]bool, len(families))
	for _, f := range families {
		got[f.GetName()] = true
	}

	want := []string{
		"cmr_webhook_requests_total",
		"cmr_webhook_request_duration_seconds",
		"cmr_running_containers",
		"cmr_callback_retries_total",
		"cmr_broadcaster_drops_total",
		"cmr_replay_cache_hits_total",
		"cmr_preflight_last_success_timestamp_seconds",
	}

	for _, name := range want {
		assert.True(t, got[name], "metric %q not registered", name)
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

func TestBroadcasterDropsLabelIsBounded(t *testing.T) {
	m := metrics.New()

	// The "all" label is a contract: we never use dynamic card_id values.
	// Emit many drops — the series count must remain 1.
	for range 1000 {
		m.BroadcasterDropsTotal.WithLabelValues("all").Inc()
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
