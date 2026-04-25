package container

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEntrypointAllowlist verifies that docker/entrypoint.sh uses
// --allowed-tools with an explicit allowlist instead of
// --dangerously-skip-permissions (CTXRUN-045). The assertions are performed
// against the file contents so the test runs without a shell or Docker.
func TestEntrypointAllowlist(t *testing.T) {
	path := entrypointPath(t)
	content, err := os.ReadFile(path)
	require.NoError(t, err, "reading entrypoint.sh")

	src := string(content)

	// 1. --dangerously-skip-permissions must be gone entirely.
	assert.NotContains(t, src, "--dangerously-skip-permissions",
		"entrypoint.sh must not pass --dangerously-skip-permissions")

	// 2. --allowed-tools must appear in both branches (autonomous + HITL).
	// Each branch builds its own exec line, so we expect two occurrences.
	assert.GreaterOrEqual(t, strings.Count(src, "--allowed-tools"), 2,
		"entrypoint.sh must pass --allowed-tools in both autonomous and HITL claude invocations")

	// 3. The allowlist is split between a common array (used by both modes)
	// and an autonomous-only extras array. Both must be declared.
	assert.Contains(t, src, "ALLOWED_TOOLS_COMMON=(",
		"entrypoint.sh must define ALLOWED_TOOLS_COMMON as a bash array")
	assert.Contains(t, src, "ALLOWED_TOOLS_AUTO_EXTRAS=(",
		"entrypoint.sh must define ALLOWED_TOOLS_AUTO_EXTRAS for autonomous-only tools")
	assert.Contains(t, src, `"${ALLOWED_TOOLS_HITL[*]}"`,
		"HITL branch must expand ALLOWED_TOOLS_HITL via space-separated [*] form")
	assert.Contains(t, src, `"${ALLOWED_TOOLS_AUTO[*]}"`,
		"autonomous branch must expand ALLOWED_TOOLS_AUTO via space-separated [*] form")

	// 4. Core must-have tools must be on the common allowlist.
	mustHave := []string{
		`"Read"`,
		`"Edit"`,
		`"Write"`,
		`"Skill"`,
		`"Bash(git:*)"`,
		// Text-processing utilities Claude uses in pipelines.
		`"Bash(sed:*)"`,
		`"Bash(awk:*)"`,
		`"Bash(grep:*)"`,
		`"Bash(find:*)"`,
		`"Bash(sort:*)"`,
		`"Bash(diff:*)"`,
		`"Bash(xargs:*)"`,
		`"Bash(printenv:*)"`,
		`"mcp__contextmatrix__transition_card"`,
	}
	for _, tool := range mustHave {
		assert.Contains(t, src, tool,
			"entrypoint.sh ALLOWED_TOOLS_COMMON must contain %s", tool)
	}

	// 5. Destructive ContextMatrix RPCs must not be present anywhere.
	mustNotHave := []string{
		"mcp__contextmatrix__delete_project",
		"mcp__contextmatrix__update_project",
	}
	for _, tool := range mustNotHave {
		assert.NotContains(t, src, tool,
			"entrypoint.sh must not allowlist %s", tool)
	}

	// 6. Task (sub-agent spawning) must appear — but only in the autonomous-
	// only extras array, not in the common one. The per-branch assertion
	// lives in TestEntrypointAllowlistInBothBranches; here we just verify
	// it is declared in the extras block.
	assert.Contains(t, src, `"Task"`,
		"ALLOWED_TOOLS_AUTO_EXTRAS must include Task for autonomous sub-agent spawning")

	// 7. The rationale comment from the CTXRUN-045 spec must be present.
	assert.Contains(t, src, "CTXRUN-045",
		"entrypoint.sh must reference CTXRUN-045 in the allowlist rationale comment")
}

// TestEntrypointAllowlistInBothBranches verifies that the --allowed-tools flag
// appears inside both the if/else branches that invoke claude — not just once
// at the top of the file.
func TestEntrypointAllowlistInBothBranches(t *testing.T) {
	path := entrypointPath(t)
	content, err := os.ReadFile(path)
	require.NoError(t, err, "reading entrypoint.sh")

	src := string(content)

	// Locate the CM_INTERACTIVE branch split.
	ifIdx := strings.Index(src, `[ "${CM_INTERACTIVE:-}" = "1" ]`)
	require.NotEqual(t, -1, ifIdx, "entrypoint.sh must branch on CM_INTERACTIVE")

	elseIdx := strings.Index(src[ifIdx:], "\nelse\n")
	require.NotEqual(t, -1, elseIdx, "entrypoint.sh must have an else clause")
	elseIdx += ifIdx

	fiIdx := strings.LastIndex(src, "\nfi\n")
	require.Greater(t, fiIdx, elseIdx, "entrypoint.sh must terminate the branches with fi")

	interactive := src[ifIdx:elseIdx]
	autonomous := src[elseIdx:fiIdx]

	assert.Contains(t, interactive, "--allowed-tools",
		"HITL (interactive) branch must pass --allowed-tools")
	assert.Contains(t, autonomous, "--allowed-tools",
		"autonomous branch must pass --allowed-tools")

	// Neither branch may re-introduce --dangerously-skip-permissions.
	assert.NotContains(t, interactive, "--dangerously-skip-permissions",
		"HITL branch must not pass --dangerously-skip-permissions")
	assert.NotContains(t, autonomous, "--dangerously-skip-permissions",
		"autonomous branch must not pass --dangerously-skip-permissions")

	// Per-branch allowlist wiring: HITL expands ALLOWED_TOOLS_HITL (common
	// only — no sub-agent spawning), autonomous expands ALLOWED_TOOLS_AUTO
	// (common + Task). The HITL rule that sub-agents must not commit comes
	// from feedback memory; excluding Task in the interactive branch makes
	// it a hard constraint rather than a prompt-level request.
	assert.Contains(t, interactive, `"${ALLOWED_TOOLS_HITL[*]}"`,
		"HITL branch must expand the HITL-only allowlist")
	assert.Contains(t, interactive, `ALLOWED_TOOLS_HITL=("${ALLOWED_TOOLS_COMMON[@]}")`,
		"HITL branch must build ALLOWED_TOOLS_HITL from COMMON only (no Task)")
	assert.Contains(t, autonomous, `"${ALLOWED_TOOLS_AUTO[*]}"`,
		"autonomous branch must expand the autonomous allowlist")
	assert.Contains(t, autonomous, `ALLOWED_TOOLS_AUTO=("${ALLOWED_TOOLS_COMMON[@]}" "${ALLOWED_TOOLS_AUTO_EXTRAS[@]}")`,
		"autonomous branch must append ALLOWED_TOOLS_AUTO_EXTRAS (Task)")

	// Both branches must terminate option parsing with `--` before the
	// positional prompt. Without it, claude's variadic
	// `--allowed-tools <tools...>` greedily consumes the following prompt
	// string as yet another allowed-tool and exits with
	// "Input must be provided either through stdin or as a prompt argument".
	// The `--` must appear AFTER --allowed-tools and BEFORE the "You are
	// running..." prompt in each branch.
	for _, tc := range []struct {
		name   string
		branch string
	}{
		{"HITL", interactive},
		{"autonomous", autonomous},
	} {
		toolsIdx := strings.Index(tc.branch, "--allowed-tools")
		require.NotEqual(t, -1, toolsIdx, "%s branch must have --allowed-tools", tc.name)

		promptIdx := strings.Index(tc.branch, `"You are running`)
		require.NotEqual(t, -1, promptIdx, "%s branch must have the positional prompt", tc.name)
		require.Greater(t, promptIdx, toolsIdx,
			"%s branch: prompt must come after --allowed-tools", tc.name)

		between := tc.branch[toolsIdx:promptIdx]
		assert.Regexp(t, `(^|\s)--(\s|\\\s)`, between,
			"%s branch must place `--` between --allowed-tools and the prompt "+
				"(regression: commander.js variadic swallows the prompt otherwise)", tc.name)
	}
}
