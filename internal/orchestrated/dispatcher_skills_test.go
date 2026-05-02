package orchestrated

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/spawn"
	"github.com/mhersson/contextmatrix-runner/internal/webhook"
)

// TestBuildWorkerSpec_TaskSkills covers every combination of
// cfg.TaskSkillsDir × payload.TaskSkills the dispatcher must handle.
//
// Semantics mirror the legacy container manager (commit c1f82b1):
//
//   - TaskSkillsDir == ""             ⇒ feature disabled; no mount, no env.
//   - TaskSkillsDir != "", nil list   ⇒ mount only; entrypoint copies the
//     full host dir into ~/.claude/skills.
//   - TaskSkillsDir != "", non-nil    ⇒ mount + CM_TASK_SKILLS_SET=1 +
//     CM_TASK_SKILLS=<csv>; entrypoint copies only the listed skills.
//   - TaskSkillsDir != "", empty list ⇒ mount + CM_TASK_SKILLS_SET=1 +
//     CM_TASK_SKILLS=""; entrypoint copies nothing.
func TestBuildWorkerSpec_TaskSkills(t *testing.T) {
	t.Parallel()

	skills := func(s ...string) *[]string {
		out := append([]string{}, s...)

		return &out
	}

	cases := []struct {
		name          string
		taskSkillsDir string
		payloadSkills *[]string
		wantMount     bool
		wantSetEnv    bool
		wantSkillsCSV string
	}{
		{
			name:          "disabled — no dir, nil payload",
			taskSkillsDir: "",
			payloadSkills: nil,
			wantMount:     false,
			wantSetEnv:    false,
		},
		{
			name:          "disabled — no dir, payload set is ignored",
			taskSkillsDir: "",
			payloadSkills: skills("plan"),
			wantMount:     false,
			wantSetEnv:    false,
		},
		{
			name:          "no constraint — dir set, nil payload",
			taskSkillsDir: "/skills",
			payloadSkills: nil,
			wantMount:     true,
			wantSetEnv:    false,
		},
		{
			name:          "explicit list — dir set, two skills",
			taskSkillsDir: "/skills",
			payloadSkills: skills("plan", "execute"),
			wantMount:     true,
			wantSetEnv:    true,
			wantSkillsCSV: "plan,execute",
		},
		{
			name:          "explicit empty — dir set, empty list",
			taskSkillsDir: "/skills",
			payloadSkills: skills(),
			wantMount:     true,
			wantSetEnv:    true,
			wantSkillsCSV: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dp := newTestDispatcher()
			dp.cfg = &config.Config{
				AgentImage:    "contextmatrix/orchestrated@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				TaskSkillsDir: tc.taskSkillsDir,
			}
			payload := webhook.TriggerPayload{
				CardID:     "TEST-1",
				Project:    "p",
				MCPAPIKey:  "k",
				TaskSkills: tc.payloadSkills,
			}

			spec, _ := dp.buildWorkerSpec(t.Context(), payload, "http://cm:8080", "")

			var hostSkillsMount *spawn.Mount

			for i := range spec.Mounts {
				if spec.Mounts[i].Target == "/host-skills" {
					hostSkillsMount = &spec.Mounts[i]
				}
			}

			if tc.wantMount {
				if assert.NotNil(t, hostSkillsMount, "expected /host-skills mount") {
					assert.Equal(t, tc.taskSkillsDir, hostSkillsMount.Source)
					assert.True(t, hostSkillsMount.ReadOnly, "/host-skills must be read-only")
				}
			} else {
				assert.Nil(t, hostSkillsMount, "expected no /host-skills mount")
			}

			gotSet, gotSetOK := spec.Env["CM_TASK_SKILLS_SET"]
			gotCSV, gotCSVOK := spec.Env["CM_TASK_SKILLS"]

			if tc.wantSetEnv {
				assert.True(t, gotSetOK, "CM_TASK_SKILLS_SET should be set")
				assert.Equal(t, "1", gotSet)
				assert.True(t, gotCSVOK, "CM_TASK_SKILLS should be set")
				assert.Equal(t, tc.wantSkillsCSV, gotCSV)
			} else {
				assert.False(t, gotSetOK, "CM_TASK_SKILLS_SET should NOT be set")
				assert.False(t, gotCSVOK, "CM_TASK_SKILLS should NOT be set")
			}
		})
	}
}
