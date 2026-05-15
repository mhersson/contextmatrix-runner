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
// --dangerously-skip-permissions. The assertions are performed against the
// file contents so the test runs without a shell or Docker.
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
}

// TestEntrypointKBAllowlistContainsRequiredTools verifies that the
// knowledge-refresh dispatch branch declares ALLOWED_TOOLS_KB with the
// required write/progress tools and the Task tool (for sub-agent fan-out).
func TestEntrypointKBAllowlistContainsRequiredTools(t *testing.T) {
	path := entrypointPath(t)
	content, err := os.ReadFile(path)
	require.NoError(t, err, "reading entrypoint.sh")

	src := string(content)

	assert.Contains(t, src, "ALLOWED_TOOLS_KB=(",
		"entrypoint.sh must define ALLOWED_TOOLS_KB for knowledge-refresh mode")
	assert.Contains(t, src, `"${ALLOWED_TOOLS_KB[*]}"`,
		"knowledge-refresh branch must expand ALLOWED_TOOLS_KB via space-separated [*] form")

	// Required additions over ALLOWED_TOOLS_COMMON.
	required := []string{
		`"Task"`,
		`"mcp__contextmatrix__refresh_knowledge_base"`,
		`"mcp__contextmatrix__commit_knowledge_docs"`,
		`"mcp__contextmatrix__update_refresh_progress"`,
	}
	for _, tool := range required {
		assert.Contains(t, src, tool,
			"ALLOWED_TOOLS_KB must include %s", tool)
	}

	// Inheritance: KB allowlist must spread ALLOWED_TOOLS_COMMON in.
	assert.Contains(t, src, `ALLOWED_TOOLS_KB=("${ALLOWED_TOOLS_COMMON[@]}"`,
		"ALLOWED_TOOLS_KB must inherit ALLOWED_TOOLS_COMMON via array spread")
}

// TestEntrypointKBAllowlistDoesNotContainDestructiveTools verifies the
// effective knowledge-refresh allowlist does not introduce destructive tools
// the refresh skill never needs.
//
// ALLOWED_TOOLS_KB inherits ALLOWED_TOOLS_COMMON via array spread, so the
// guardrail must check both blocks: drift in either one would otherwise leak
// into the KB allowlist undetected.
func TestEntrypointKBAllowlistDoesNotContainDestructiveTools(t *testing.T) {
	path := entrypointPath(t)
	content, err := os.ReadFile(path)
	require.NoError(t, err, "reading entrypoint.sh")

	src := string(content)

	// The KB allowlist is COMMON + KB extras. Extract both blocks (each a
	// bash array literal terminated by `)`) and concatenate so the deny
	// assertion catches drift in either source.
	commonBlock := extractAllowlistBlock(t, src, "ALLOWED_TOOLS_COMMON=(")
	kbBlock := extractAllowlistBlock(t, src, "ALLOWED_TOOLS_KB=(")
	effective := commonBlock + "\n" + kbBlock

	// Each entry below must be verified absent from the current allowlist
	// before being added — the goal is a regression guard, not a
	// false-positive trap. Items that ARE legitimately present (e.g.
	// "Write", "Edit", "Bash(rm:*)", update_card) are deliberately
	// excluded.
	denied := []string{
		// Destructive ContextMatrix RPCs the refresh skill never invokes.
		`"mcp__contextmatrix__delete_project"`,
		`"mcp__contextmatrix__update_project"`,
		`"mcp__contextmatrix__delete_card"`,
		// Bash escapes that defeat per-command prefix matching.
		`"Bash(*)"`,
		`"Bash(curl:*)"`,
		`"Bash(eval:*)"`,
	}
	for _, tool := range denied {
		assert.NotContains(t, effective, tool,
			"effective KB allowlist (COMMON + KB extras) must not include destructive tool %s", tool)
	}
}

// extractAllowlistBlock returns the substring covering a single
// `NAME=( ... )` bash array literal starting at `marker`.
//
// Naive `strings.Index(src, ")")` would stop at the first nested paren —
// e.g. `"Bash(git:*)"` or a `# comment with (parens)` — and silently
// truncate the block, letting drift after that line slip past the deny
// check. We walk the bytes after the opening `(`, skip anything inside a
// double-quoted string or `#`-prefixed comment, count `(`/`)` depth, and
// return when depth returns to zero (the matching outer close).
func extractAllowlistBlock(t *testing.T, src, marker string) string {
	t.Helper()

	start := strings.Index(src, marker)
	require.NotEqual(t, -1, start, "%s block not found", marker)

	// Cursor sits just past the opening `(` — depth starts at 1.
	cursor := start + len(marker)
	depth := 1
	inQuote := false
	inComment := false

	for i := cursor; i < len(src); i++ {
		c := src[i]
		switch {
		case inComment:
			if c == '\n' {
				inComment = false
			}
		case c == '"' && (i == 0 || src[i-1] != '\\'):
			inQuote = !inQuote
		case inQuote:
			// inside a string literal — anything goes
		case c == '#':
			inComment = true
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return src[start:i]
			}
		}
	}

	require.FailNowf(t, "block extraction failed", "%s closing paren not found", marker)

	return ""
}

// TestEntrypointKBDispatchRunsClaude verifies the knowledge-refresh branch
// of the entrypoint runs claude with the KB allowlist (not the autonomous
// or HITL allowlist) and references the refresh-knowledge skill.
func TestEntrypointKBDispatchRunsClaude(t *testing.T) {
	path := entrypointPath(t)
	content, err := os.ReadFile(path)
	require.NoError(t, err, "reading entrypoint.sh")

	src := string(content)

	// The block opens with the elif-test on CM_MODE in the dispatch section.
	// (The validation block also contains an if-test on CM_MODE, so we search
	// for the elif form used in the four-way dispatch.)
	idx := strings.Index(src, `elif [ "${CM_MODE:-}" = "knowledge-refresh" ]; then`)
	require.NotEqual(t, -1, idx, "knowledge-refresh dispatch branch not found")

	// And ends at the next elif (HITL branch) or fi.
	rest := src[idx:]

	endRel := strings.Index(rest, "\nelif ")
	if endRel == -1 {
		endRel = strings.Index(rest, "\nelse")
	}

	require.NotEqual(t, -1, endRel, "knowledge-refresh branch terminator not found")
	branch := rest[:endRel]

	assert.Contains(t, branch, "exec claude",
		"knowledge-refresh branch must exec claude")
	assert.Contains(t, branch, `"${ALLOWED_TOOLS_KB[*]}"`,
		"knowledge-refresh branch must use ALLOWED_TOOLS_KB")
	assert.Contains(t, branch, "get_skill",
		"knowledge-refresh branch must reference get_skill in its prompt")
	assert.Contains(t, branch, "refresh-knowledge",
		"knowledge-refresh branch must reference the refresh-knowledge skill name")
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
