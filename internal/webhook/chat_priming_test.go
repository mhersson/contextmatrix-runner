package webhook

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBuildChatRehydrationPriming_ContainsContract verifies that the priming
// text contains the canonical MCP tool name and resume file paths. This ensures
// that a silent rename of the MCP tool on the CM side will break the build
// rather than silently breaking rehydration.
func TestBuildChatRehydrationPriming_ContainsContract(t *testing.T) {
	t.Parallel()

	text := buildChatRehydrationPriming("01SESS")

	// Tool name contract: renaming mcp__contextmatrix__chat_rehydration_complete
	// on the CM side must break this test, forcing lock-step updates.
	require.Contains(t, text, "mcp__contextmatrix__chat_rehydration_complete",
		"renaming the MCP tool requires updating chat_priming.go in lock-step")

	// File paths contract: resume data must be read from these paths.
	require.Contains(t, text, "/run/cm-chat/resume.jsonl",
		"priming must reference the resume transcript file path")
	require.Contains(t, text, "/run/cm-chat/resume.meta.json",
		"priming must reference the resume metadata file path")

	// Session-id interpolation contract: session ID must be embedded.
	require.Contains(t, text, "01SESS",
		"session ID must be interpolated into the priming text")

	// Instruction ordering contract: "Before greeting the user" must come
	// before the tool call. This ensures restoration steps are executed
	// in the correct sequence (restore, then call tool to mark complete).
	greetIdx := strings.Index(text, "Before greeting the user")
	toolCallIdx := strings.Index(text, "mcp__contextmatrix__chat_rehydration_complete")
	require.Greater(t, toolCallIdx, greetIdx,
		"tool call must come after the 'before greeting' instruction block")
}

// TestBuildChatRehydrationPriming_NoPanicOnEdgeSessionIDs verifies that the
// function does not panic on edge-case session IDs (empty, very long, etc.).
// This ensures safe string formatting even if session ID validation is relaxed
// in the future.
func TestBuildChatRehydrationPriming_NoPanicOnEdgeSessionIDs(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"", "01A", strings.Repeat("X", 64)} {
		text := buildChatRehydrationPriming(id)
		require.NotEmpty(t, text, "should produce non-empty priming text even for edge session IDs")
	}
}

// TestBuildChatRehydrationPriming_MandatoryCallContract verifies that the
// tool call is correctly framed as MANDATORY. This ensures operators understand
// that the call is required for proper UI state transition.
func TestBuildChatRehydrationPriming_MandatoryCallContract(t *testing.T) {
	t.Parallel()

	text := buildChatRehydrationPriming("SESSION123")

	// The tool call must be marked as MANDATORY in the instructions.
	require.Contains(t, text, "MANDATORY",
		"tool call must be explicitly marked as mandatory in instructions")

	// The tool call must come after restoration, not before.
	toolCallIdx := strings.Index(text, "mcp__contextmatrix__chat_rehydration_complete")
	restorationIdx := strings.Index(text, "restoration is complete")
	require.Greater(t, toolCallIdx, restorationIdx,
		"tool call must come after restoration is marked as complete")
}
