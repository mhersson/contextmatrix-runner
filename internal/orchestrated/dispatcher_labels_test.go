package orchestrated

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/container"
	"github.com/mhersson/contextmatrix-runner/internal/webhook"
)

// TestBuildWorkerSpec_Labels confirms the dispatcher tags every spawned
// worker container with the canonical runner labels. Without these the
// label-aware sweeps (ListManaged, ForceRemoveByLabels, CleanupOrphans)
// silently no-op and orphaned containers leak across runner restarts.
func TestBuildWorkerSpec_Labels(t *testing.T) {
	t.Parallel()

	dp := newTestDispatcher()
	dp.cfg = &config.Config{
		AgentImage: "contextmatrix/orchestrated@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}

	payload := webhook.TriggerPayload{
		CardID:    "ALPHA-007",
		Project:   "alpha",
		MCPAPIKey: "k",
	}

	spec, _ := dp.buildWorkerSpec(t.Context(), payload, "http://cm:8080", "")

	assert.Equal(t, "true", spec.Labels[container.LabelRunner])
	assert.Equal(t, "alpha", spec.Labels[container.LabelProject])
	assert.Equal(t, "ALPHA-007", spec.Labels[container.LabelCardID])
}
