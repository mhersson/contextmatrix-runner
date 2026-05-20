package webhook

import (
	"hash/fnv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/chatproto"
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

// TestBuildChatRehydrationPriming_PromptVersionGolden pins the priming text
// shape to chatproto.PromptVersion. Each released PromptVersion has a known
// (length, FNV-1a hash) over the output for a fixed session ID. If the
// priming template changes without bumping PromptVersion — or vice versa —
// this test must fail so CM and the runner stay in lock-step.
//
// To intentionally roll PromptVersion:
//  1. Edit buildChatRehydrationPriming to the new template.
//  2. Bump chatproto.PromptVersion by one.
//  3. Run this test, copy the printed (len, hash) into the new case below,
//     and add the old version's row above it. The case block is append-only —
//     we keep the historical entries so a regression to an older template
//     also fails.
func TestBuildChatRehydrationPriming_PromptVersionGolden(t *testing.T) {
	t.Parallel()

	const goldenSessionID = "GOLDEN-SESSION-V1"

	text := buildChatRehydrationPriming(goldenSessionID)

	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	gotHash := h.Sum64()
	gotLen := len(text)

	type golden struct {
		length int
		hash   uint64
	}

	versions := map[int]golden{
		// PromptVersion 1: initial chat rehydration priming.
		1: {length: 2540, hash: 0xeac203e9b259d0a4},
	}

	want, ok := versions[chatproto.PromptVersion]
	require.True(t, ok,
		"no golden entry for chatproto.PromptVersion=%d; if you bumped the constant, "+
			"add a new (length, hash) row to the versions map", chatproto.PromptVersion)

	assert.Equal(t, want.length, gotLen,
		"priming text length changed without bumping chatproto.PromptVersion "+
			"(or the new version's golden length entry is wrong) — got len=%d hash=0x%x",
		gotLen, gotHash)
	assert.Equal(t, want.hash, gotHash,
		"priming text content changed without bumping chatproto.PromptVersion "+
			"(or the new version's golden hash entry is wrong) — got len=%d hash=0x%x",
		gotLen, gotHash)
}
