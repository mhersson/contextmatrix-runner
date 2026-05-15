package container

import (
	"os"
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

	// Extract the entire chat-mode dispatch branch (the Four-way dispatch one,
	// not the early MCP header setup). Search for the dispatch comment marker
	// to disambiguate the multiple CM_CHAT_SESSION checks.
	dispatchMarker := `# Four-way dispatch:`
	dispatchIdx := strings.Index(src, dispatchMarker)
	require.NotEqual(t, -1, dispatchIdx, "entrypoint.sh must contain the Four-way dispatch comment")

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
	// the sibling elif branch that handles knowledge-refresh mode.
	require.NotContains(t, chatBranch, "knowledge-refresh",
		"extracted chat block must not leak into the sibling elif branch")
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
