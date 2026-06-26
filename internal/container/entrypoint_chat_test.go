package container

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEntrypoint_ChatModeBranch verifies the chat-mode dispatch is present
// in entrypoint.sh and validates stream-json, allowlist composition, model
// passthrough, and CM_CHAT_RESUME conditional.
func TestEntrypoint_ChatModeBranch(t *testing.T) {
	path := entrypointPath(t)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	src := string(raw)

	// Dispatch branch must exist.
	assert.Contains(t, src, `[ -n "${CM_CHAT_SESSION:-}" ]`,
		"entrypoint.sh must dispatch on CM_CHAT_SESSION")
	// Workspace mkdir — chat reuses the shared /home/user/workspace dir
	// that the entrypoint prepares before the dispatch (chat containers
	// run as non-root and can't write to a top-level /workspace).
	assert.Contains(t, src, "mkdir -p /home/user/workspace",
		"entrypoint must create the home workspace before dispatch")

	// Extract the entire chat-mode dispatch branch (the Three-way dispatch one,
	// not the early MCP header setup). Search for the dispatch comment marker
	// to disambiguate the multiple CM_CHAT_SESSION checks.
	dispatchMarker := `# Three-way dispatch:`
	dispatchIdx := strings.Index(src, dispatchMarker)
	require.NotEqual(t, -1, dispatchIdx, "entrypoint.sh must contain the Three-way dispatch comment")

	// Extract from the dispatch section onward (avoiding the early MCP header setup).
	srcFromDispatch := src[dispatchIdx:]
	chatBranch := extractIfBlock(t, srcFromDispatch, `if [ -n "${CM_CHAT_SESSION:-}" ]; then`)

	// Stream-json execution.
	assert.Contains(t, chatBranch, "--input-format stream-json")
	assert.Contains(t, chatBranch, "--output-format stream-json")

	// Model passthrough: --model "${CM_ORCHESTRATOR_MODEL:-claude-sonnet-4-6}"
	assert.Contains(t, chatBranch, `--model "${CM_ORCHESTRATOR_MODEL:-claude-sonnet-4-6}"`,
		"chat branch must forward CM_ORCHESTRATOR_MODEL to claude --model")

	// ALLOWED_TOOLS_CHAT composition: COMMON + AUTO_EXTRAS
	assert.Contains(t, chatBranch, `ALLOWED_TOOLS_CHAT=("${ALLOWED_TOOLS_COMMON[@]}" "${ALLOWED_TOOLS_AUTO_EXTRAS[@]}")`,
		"chat branch must resolve ALLOWED_TOOLS_CHAT as COMMON + AUTO_EXTRAS")
	assert.Contains(t, chatBranch, `"${ALLOWED_TOOLS_CHAT[*]}"`,
		"chat branch must expand ALLOWED_TOOLS_CHAT via space-separated [*] form")

	// CM_CHAT_RESUME conditional: references resume.jsonl
	resumeBlock := extractIfBlock(t, chatBranch, `if [ "${CM_CHAT_RESUME:-0}" = "1" ]; then`)
	assert.Contains(t, resumeBlock, "/run/cm-chat/resume.jsonl",
		"CM_CHAT_RESUME branch must reference the resume payload file")

	// The extracted chat block must be correctly scoped: it must NOT leak into
	// the sibling elif branch that handles HITL (interactive) mode.
	require.NotContains(t, chatBranch, "ALLOWED_TOOLS_HITL",
		"extracted chat block must not leak into the sibling HITL elif branch")
}

func TestEntrypoint_ChatModeValidation(t *testing.T) {
	path := entrypointPath(t)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	src := string(raw)
	assert.Contains(t, src, "ERROR: invalid CM_CHAT_SESSION",
		"must validate CM_CHAT_SESSION")
	assert.Contains(t, src, "ERROR: invalid CM_CHAT_PROJECT",
		"must validate CM_CHAT_PROJECT")
	assert.Contains(t, src, "ERROR: CM_CHAT_REPO_URL must be https://",
		"must validate CM_CHAT_REPO_URL scheme")
}

// TestEntrypoint_MCPMergeGuardsEmptyURL verifies the MCP-merge block guards
// the unguarded $CM_MCP_URL expansion behind a non-empty check. Chat-mode
// containers may opt out of MCP wiring (StartChat only sets CM_MCP_URL when
// opts.MCPURL is non-empty). Under `set -euo pipefail` the original
// `jq -n --arg url "$CM_MCP_URL" ...` crashed with "CM_MCP_URL: unbound
// variable" before claude could start.
//
// This is a source-inspection test (not an exec test): we cannot run the
// full entrypoint.sh because it requires bind-mounted /run/cm-secrets/,
// /claude-auth/, /host-skills/, dockerd, etc. The structural check is
// sufficient because the regression we are guarding against is the
// presence of an unguarded `"$CM_MCP_URL"` reference.
func TestEntrypoint_MCPMergeGuardsEmptyURL(t *testing.T) {
	path := entrypointPath(t)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	src := string(raw)

	// The block that builds the MCP entry must live behind a non-empty
	// CM_MCP_URL guard. We check for the guard line itself plus the
	// MCP_ENTRY assignment to ensure the two were not separated by a
	// refactor that left the assignment outside the guard.
	assert.Contains(t, src, `if [ -n "${CM_MCP_URL:-}" ]; then`,
		"entrypoint.sh must guard the MCP-merge block on a non-empty CM_MCP_URL")

	// The jq argument that interpolates the URL must use the
	// `${VAR:-}` defaulting form rather than `"$VAR"`. Without this, the
	// `set -u` flag in the entrypoint blows up on the bare reference
	// even though the surrounding guard would already prevent the
	// expansion at runtime (defence-in-depth + future-proofing).
	assert.NotContains(t, src, `jq -n --arg url "$CM_MCP_URL"`,
		"entrypoint.sh must not reference $CM_MCP_URL bare under set -u; use ${CM_MCP_URL:-}")

	// The original (pre-fix) code wrote both the MCP_ENTRY jq merge and
	// the mv-tmp file outside any conditional. Verify the merge block
	// lives inside the guard by extracting it.
	guardBlock := extractIfBlock(t, src, `if [ -n "${CM_MCP_URL:-}" ]; then`)
	assert.Contains(t, guardBlock, "MCP_ENTRY=$(jq -n",
		"MCP_ENTRY assignment must live inside the CM_MCP_URL guard")
	assert.Contains(t, guardBlock, `.mcpServers`,
		"mcpServers merge must live inside the CM_MCP_URL guard")
}

// TestEntrypoint_MCPMergeEmptyURLDoesNotCrashSetU executes the relevant
// MCP-merge snippet from entrypoint.sh under `set -euo pipefail` with
// CM_MCP_URL unset. Pre-fix this would exit with "CM_MCP_URL: unbound
// variable". Post-fix the guard must skip the merge cleanly.
//
// The snippet is the literal `if [ -n "${CM_MCP_URL:-}" ]; then ... fi`
// block extracted from entrypoint.sh, with the surrounding helpers
// (CLAUDE_JSON init, MCP_HEADERS placeholder) stubbed so the snippet is
// self-contained.
func TestEntrypoint_MCPMergeEmptyURLDoesNotCrashSetU(t *testing.T) {
	path := entrypointPath(t)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	src := string(raw)
	guardBlock := extractIfBlock(t, src, `if [ -n "${CM_MCP_URL:-}" ]; then`)
	require.NotEmpty(t, guardBlock, "guard block extraction failed")

	tmpHome := t.TempDir()

	script := `set -euo pipefail
unset CM_MCP_URL || true
MCP_HEADERS='{}'
CLAUDE_JSON="$HOME/.claude.json"
[ -f "$CLAUDE_JSON" ] || echo '{}' > "$CLAUDE_JSON"
` + guardBlock + `
exit 0
`

	cmd := exec.CommandContext(context.Background(), "bash", "-c", script)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + tmpHome,
	}

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "guard block must not crash with empty CM_MCP_URL: %s", out)

	// And the .claude.json must be left as the trivial init (no MCP
	// servers added when CM_MCP_URL is empty).
	body, err := os.ReadFile(filepath.Join(tmpHome, ".claude.json"))
	require.NoError(t, err)
	assert.Equal(t, "{}\n", string(body),
		".claude.json must remain unchanged when CM_MCP_URL is empty")
}

// TestEntrypoint_MCPMergePopulatedURLWritesEntry asserts the merge still
// runs when CM_MCP_URL is non-empty. Tests the positive case so a future
// refactor that accidentally inverts the guard is caught.
func TestEntrypoint_MCPMergePopulatedURLWritesEntry(t *testing.T) {
	path := entrypointPath(t)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	src := string(raw)
	guardBlock := extractIfBlock(t, src, `if [ -n "${CM_MCP_URL:-}" ]; then`)
	require.NotEmpty(t, guardBlock)

	tmpHome := t.TempDir()

	script := `set -euo pipefail
export CM_MCP_URL='https://cm.example.com/mcp/v1'
MCP_HEADERS='{"Authorization": "Bearer test"}'
CLAUDE_JSON="$HOME/.claude.json"
[ -f "$CLAUDE_JSON" ] || echo '{}' > "$CLAUDE_JSON"
` + guardBlock + `
cat "$CLAUDE_JSON"
`

	cmd := exec.CommandContext(context.Background(), "bash", "-c", script)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + tmpHome,
	}

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "guard block failed with non-empty CM_MCP_URL: %s", out)

	body, err := os.ReadFile(filepath.Join(tmpHome, ".claude.json"))
	require.NoError(t, err)
	assert.Contains(t, string(body), `"contextmatrix"`,
		"merged .claude.json must contain the contextmatrix MCP entry")
	assert.Contains(t, string(body), "https://cm.example.com/mcp/v1",
		"merged .claude.json must contain the CM_MCP_URL value")
}

// extractIfBlock returns the substring covering a single `if ... then ... fi`
// conditional block starting at the given header line (e.g.
// `if [ "${CM_CHAT_RESUME:-0}" = "1" ]; then`).
//
// Similar to extractAllowlistBlock, this is depth-aware: it counts nested
// if/then/elif/else/fi keywords and returns when the depth returns to zero,
// correctly handling escaped quotes, comments, and word boundaries. This
// prevents brittle string-slicing that would fail on nested conditionals or
// quoted strings containing "fi" or other keywords.
func extractIfBlock(t *testing.T, src, header string) string {
	t.Helper()

	start := strings.Index(src, header)
	require.NotEqual(t, -1, start, "%s block not found", header)

	// Cursor sits just past the header line. Walk forward, skipping quoted
	// strings and comments, counting if/elif/else/fi keywords.
	cursor := start + len(header)

	// Initialize depth at 1 (the opening "if ... then").
	depth := 1
	inQuote := false
	inComment := false

	for i := cursor; i < len(src); i++ {
		c := src[i]
		switch {
		case inComment:
			// Inside a comment until newline.
			if c == '\n' {
				inComment = false
			}
		case c == '"' && (i == 0 || src[i-1] != '\\'):
			// Toggle quote state (escaped quotes don't toggle).
			inQuote = !inQuote
		case inQuote:
			// Inside a quoted string; skip keywords.
		case c == '#':
			// Start of a comment (not inside a quote).
			inComment = true
		case isKeywordAt(src, i, "if"):
			// Nested "if" keyword increments depth.
			depth++
		case isKeywordAt(src, i, "fi"):
			// "fi" keyword (at word boundary) decrements depth.
			depth--
			if depth == 0 {
				// Found the matching "fi"; include it and return.
				j := i + 2
				for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
					j++
				}

				if j < len(src) && (src[j] == ';' || src[j] == '\n') {
					j++
				}

				return src[start:j]
			}
		case depth == 1 && isKeywordAt(src, i, "elif"):
			// "elif" at the current branch depth closes this branch.
			// Do not increment depth — elif sits at the same depth as the
			// surrounding if.
			return src[start:i]
		case depth == 1 && isKeywordAt(src, i, "else"):
			// "else" at the current branch depth closes this branch.
			return src[start:i]
		}
	}

	require.FailNowf(t, "block extraction failed", "%s closing fi not found", header)

	return ""
}

// isKeywordAt checks if the given keyword appears at position i in src as a
// complete word token (preceded and followed by word boundaries).
func isKeywordAt(src string, i int, keyword string) bool {
	// Check if the keyword appears at this position.
	if !strings.HasPrefix(src[i:], keyword) {
		return false
	}

	// Check the character before (must be whitespace, semicolon, or start of string).
	if i > 0 {
		prev := src[i-1]
		if prev != ' ' && prev != '\t' && prev != '\n' && prev != ';' && prev != '&' && prev != '|' && prev != '(' {
			return false
		}
	}

	// Check the character after (must be whitespace, semicolon, or end of string).
	afterIdx := i + len(keyword)
	if afterIdx < len(src) {
		next := src[afterIdx]
		if next != ' ' && next != '\t' && next != '\n' && next != ';' && next != '&' && next != '|' && next != ')' {
			return false
		}
	}

	return true
}
