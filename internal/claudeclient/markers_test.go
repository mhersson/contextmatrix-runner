package claudeclient

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecognizePlanDrafted(t *testing.T) {
	text := `Some preamble.

PLAN_DRAFTED
card_id: ABC-1
status: drafted
plan_summary: Add multi-repo workspace.
subtask_count: 3
`
	m, ok := RecognizeMarker(text)
	require.True(t, ok)
	require.Equal(t, MarkerPlanDrafted, m.Kind)
	require.Equal(t, "ABC-1", m.Fields["card_id"])
	require.Equal(t, "3", m.Fields["subtask_count"])
}

func TestRecognizeTaskBlocked(t *testing.T) {
	text := `TASK_BLOCKED
card_id: SUB-2
status: blocked
reason: needs api change in OTHER-9
blocker_cards: [OTHER-9]
needs_human: true
`
	m, ok := RecognizeMarker(text)
	require.True(t, ok)
	require.Equal(t, MarkerTaskBlocked, m.Kind)
	require.Contains(t, m.Fields["reason"], "OTHER-9")
}

func TestRecognizeDiscoveryComplete(t *testing.T) {
	raw := json.RawMessage(`{"design_summary":"agreed plan: build X with Y"}`)
	ev := StreamEvent{
		Kind:      EventToolUse,
		ToolName:  "discovery_complete",
		ToolInput: raw,
	}
	m, ok := RecognizeFromToolUse(ev)
	require.True(t, ok)
	require.Equal(t, MarkerDiscoveryComplete, m.Kind)
	require.Contains(t, m.Fields["design_summary"], "agreed plan")
}

func TestRecognizePlanComplete(t *testing.T) {
	raw := json.RawMessage(`{"card_id":"PROJ-1","plan_summary":"two subtasks"}`)
	ev := StreamEvent{Kind: EventToolUse, ToolName: "plan_complete", ToolInput: raw}
	m, ok := RecognizeFromToolUse(ev)
	require.True(t, ok)
	require.Equal(t, MarkerPlanComplete, m.Kind)
	require.Equal(t, "PROJ-1", m.Fields["card_id"])
	require.Equal(t, "two subtasks", m.Fields["plan_summary"])
}

func TestRecognizeReviewApprove(t *testing.T) {
	raw := json.RawMessage(`{"card_id":"PROJ-1","summary":"looks good"}`)
	ev := StreamEvent{Kind: EventToolUse, ToolName: "review_approve", ToolInput: raw}
	m, ok := RecognizeFromToolUse(ev)
	require.True(t, ok)
	require.Equal(t, MarkerReviewApprove, m.Kind)
	require.Equal(t, "looks good", m.Fields["summary"])
}

func TestRecognizeReviewRevise(t *testing.T) {
	raw := json.RawMessage(`{"card_id":"PROJ-1","summary":"needs work","feedback":"use REST"}`)
	ev := StreamEvent{Kind: EventToolUse, ToolName: "review_revise", ToolInput: raw}
	m, ok := RecognizeFromToolUse(ev)
	require.True(t, ok)
	require.Equal(t, MarkerReviewRevise, m.Kind)
	require.Equal(t, "use REST", m.Fields["feedback"])
}

func TestRecognizeFromToolUseRejectsOtherTools(t *testing.T) {
	ev := StreamEvent{Kind: EventToolUse, ToolName: "Read", ToolInput: json.RawMessage(`{}`)}
	_, ok := RecognizeFromToolUse(ev)
	require.False(t, ok)
}

// Real Claude emits MCP-registered tools with the server-prefixed name.
// The recognizer must strip the prefix so prefixed and bare forms map
// to the same MarkerKind.
func TestRecognizeFromToolUseAcceptsMCPPrefixedNames(t *testing.T) {
	cases := []struct {
		toolName string
		want     MarkerKind
	}{
		{"mcp__contextmatrix__discovery_complete", MarkerDiscoveryComplete},
		{"mcp__contextmatrix__plan_complete", MarkerPlanComplete},
		{"mcp__contextmatrix__review_approve", MarkerReviewApprove},
		{"mcp__contextmatrix__review_revise", MarkerReviewRevise},
	}
	for _, tc := range cases {
		t.Run(tc.toolName, func(t *testing.T) {
			ev := StreamEvent{Kind: EventToolUse, ToolName: tc.toolName, ToolInput: json.RawMessage(`{"card_id":"PROJ-1"}`)}
			m, ok := RecognizeFromToolUse(ev)
			require.True(t, ok)
			require.Equal(t, tc.want, m.Kind)
		})
	}
}

func TestRecognizeFromToolUseRejectsNonToolUse(t *testing.T) {
	ev := StreamEvent{Kind: EventText, Text: "hello"}
	_, ok := RecognizeFromToolUse(ev)
	require.False(t, ok)
}

func TestRecognizeNothing(t *testing.T) {
	_, ok := RecognizeMarker("just some prose with no marker")
	require.False(t, ok)
}

func TestRecognizeMarkerLastWins(t *testing.T) {
	// When multiple markers appear, the last one is returned.
	text := `TASK_COMPLETE
card_id: SUB-1
status: done
summary: first

later changed mind

TASK_BLOCKED
card_id: SUB-1
status: blocked
reason: hit a snag
`
	m, ok := RecognizeMarker(text)
	require.True(t, ok)
	require.Equal(t, MarkerTaskBlocked, m.Kind)
	require.Contains(t, m.Fields["reason"], "snag")
}

func TestParseTypedPlan(t *testing.T) {
	text := `PLAN_DRAFTED
card_id: ABC-1
plan_summary: Build it
subtask_count: 2
`

	var plan PlanDraftedPayload

	err := ParseMarker(text, &plan)
	require.NoError(t, err)
	require.Equal(t, "ABC-1", plan.CardID)
	require.Equal(t, 2, plan.SubtaskCount)
}

func TestParseTypedPlanFromStructuredJSON(t *testing.T) {
	text := "Some preamble.\n\nPLAN_DRAFTED\n```json\n" + `{
  "card_id": "ABC-1",
  "plan_summary": "Build the auth flow.",
  "chosen_repos": ["auth-svc", "shared"],
  "subtasks": [
    {
      "title": "Implement JWT signing",
      "description": "Create jwt.go with Sign() and Verify().",
      "repos": ["auth-svc"],
      "priority": "high",
      "depends_on": []
    },
    {
      "title": "Wire login handler",
      "description": "Hook the new signer into POST /login.",
      "repos": ["auth-svc"],
      "priority": "medium",
      "depends_on": ["OTHER-9"]
    }
  ]
}
` + "```\n"

	var p PlanDraftedPayload
	require.NoError(t, ParseMarker(text, &p))
	require.Equal(t, "ABC-1", p.CardID)
	require.Equal(t, "Build the auth flow.", p.PlanSummary)
	require.Equal(t, []string{"auth-svc", "shared"}, p.ChosenRepos)
	require.Len(t, p.Subtasks, 2)
	require.Equal(t, "Implement JWT signing", p.Subtasks[0].Title)
	require.Equal(t, []string{"auth-svc"}, p.Subtasks[0].Repos)
	require.Equal(t, "high", p.Subtasks[0].Priority)
	require.Empty(t, p.Subtasks[0].DependsOn)
	require.Equal(t, []string{"OTHER-9"}, p.Subtasks[1].DependsOn)
	require.Equal(t, 2, p.SubtaskCount)
	require.Equal(t, "drafted", p.Status)
}

func TestParseTypedPlanEmptySubtasksJSON(t *testing.T) {
	// Plan agent signals "blocked on missing design" via empty subtasks.
	text := "PLAN_DRAFTED\n```json\n" + `{
  "card_id": "ABC-1",
  "plan_summary": "Spec is too vague to plan.",
  "chosen_repos": [],
  "subtasks": []
}
` + "```\n"

	var p PlanDraftedPayload
	require.NoError(t, ParseMarker(text, &p))
	require.Equal(t, "ABC-1", p.CardID)
	require.Empty(t, p.Subtasks)
	require.Equal(t, 0, p.SubtaskCount)
}

func TestParseTypedTaskComplete(t *testing.T) {
	text := `TASK_COMPLETE
card_id: SUB-9
status: done
summary: All passing
`

	var p TaskCompletePayload
	require.NoError(t, ParseMarker(text, &p))
	require.Equal(t, "SUB-9", p.CardID)
	require.Equal(t, "done", p.Status)
	require.Equal(t, "All passing", p.Summary)
}

func TestParseTypedTaskBlocked(t *testing.T) {
	text := `TASK_BLOCKED
card_id: SUB-2
reason: missing api
blocker_cards: [OTHER-9, OTHER-10]
needs_human: true
`

	var p TaskBlockedPayload
	require.NoError(t, ParseMarker(text, &p))
	require.Equal(t, "SUB-2", p.CardID)
	require.Equal(t, []string{"OTHER-9", "OTHER-10"}, p.BlockerCards)
	require.True(t, p.NeedsHuman)
}

func TestParseTypedReviewFindings(t *testing.T) {
	text := `REVIEW_FINDINGS
card_id: PARENT-1
recommendation: approve_with_notes
summary: Looks good with minor concerns
`

	var p ReviewFindingsPayload
	require.NoError(t, ParseMarker(text, &p))
	require.Equal(t, "PARENT-1", p.CardID)
	require.Equal(t, "approve_with_notes", p.Recommendation)
}

func TestParseTypedDocsWritten(t *testing.T) {
	text := `DOCS_WRITTEN
card_id: PARENT-1
status: written
files_written: [README.md, docs/migration.md]
`

	var p DocsWrittenPayload
	require.NoError(t, ParseMarker(text, &p))
	require.Equal(t, []string{"README.md", "docs/migration.md"}, p.FilesWritten)
}

func TestParseTypedDiagnosisComplete(t *testing.T) {
	text := `DIAGNOSIS_COMPLETE
card_id: BUG-1
root_cause: race in writeBuf
`

	var p DiagnosisCompletePayload
	require.NoError(t, ParseMarker(text, &p))
	require.Equal(t, "race in writeBuf", p.RootCause)
}

func TestParseTypedDiscoveryComplete_FromText_Errors(t *testing.T) {
	// discovery_complete is only emitted via tool_use, never as text.
	text := "no marker here"

	var p DiscoveryCompletePayload
	require.Error(t, ParseMarker(text, &p))
}

func TestParseListFieldEmpty(t *testing.T) {
	// Verify list parser handles empty / brackets-only / whitespace.
	text := `DOCS_WRITTEN
card_id: PARENT-1
files_written: []
`

	var p DocsWrittenPayload
	require.NoError(t, ParseMarker(text, &p))
	require.Empty(t, p.FilesWritten)
}

func TestParseTypedTaskNeedsDecomposition(t *testing.T) {
	text := `TASK_NEEDS_DECOMPOSITION
card_id: SUB-3
subtasks: [add migration, update docs, regenerate clients]
`

	var p TaskNeedsDecompositionPayload
	require.NoError(t, ParseMarker(text, &p))
	require.Equal(t, "SUB-3", p.CardID)
	require.Equal(t, []string{"add migration", "update docs", "regenerate clients"}, p.Subtasks)
}

func TestParseTypedTaskNeedsDecompositionWrongKind(t *testing.T) {
	text := `TASK_COMPLETE
card_id: SUB-3
status: done
`

	var p TaskNeedsDecompositionPayload

	err := ParseMarker(text, &p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected TASK_NEEDS_DECOMPOSITION")
}

func TestParseMarkerWrongPayloadType(t *testing.T) {
	text := `PLAN_DRAFTED
card_id: A
plan_summary: x
subtask_count: 1
`

	var p TaskCompletePayload

	err := ParseMarker(text, &p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected TASK_COMPLETE")
}

// TestParseTypedPlanTolerantOfStringRepos guards against a real-Opus
// drift mode: emitting `chosen_repos` and subtask `repos` / `depends_on`
// as bare strings instead of arrays, despite the schema. Strict parsing
// crashed the FSM mid-plan; the runner now coerces single strings into
// one-element lists.
func TestParseTypedPlanTolerantOfStringRepos(t *testing.T) {
	text := "PLAN_DRAFTED\n```json\n" + `{
  "card_id": "ABC-1",
  "plan_summary": "Single-repo card; agent emitted scalars instead of arrays.",
  "chosen_repos": "auth-svc",
  "subtasks": [
    {
      "title": "Implement signer",
      "description": "Create jwt.go.",
      "repos": "auth-svc",
      "priority": "high",
      "depends_on": "OTHER-9"
    }
  ]
}
` + "```\n"

	var p PlanDraftedPayload
	require.NoError(t, ParseMarker(text, &p))
	require.Equal(t, []string{"auth-svc"}, p.ChosenRepos)
	require.Len(t, p.Subtasks, 1)
	require.Equal(t, []string{"auth-svc"}, p.Subtasks[0].Repos)
	require.Equal(t, []string{"OTHER-9"}, p.Subtasks[0].DependsOn)
}

// TestParseTypedPlanTolerantOfStringSubtasks mirrors the orchestrator
// HITL fix: if real Opus emits the entire `subtasks` field as a
// JSON-string-encoded array in autonomous-mode PLAN_DRAFTED, the
// runner unwraps once and re-parses rather than crashing the FSM.
func TestParseTypedPlanTolerantOfStringSubtasks(t *testing.T) {
	innerSubtasks := `[{"title":"Implement signer","repos":["auth-svc"],"priority":"high","depends_on":[]}]`
	encoded, err := json.Marshal(innerSubtasks)
	require.NoError(t, err)

	text := "PLAN_DRAFTED\n```json\n" + `{
  "card_id": "ABC-1",
  "plan_summary": "Subtasks string-encoded.",
  "chosen_repos": ["auth-svc"],
  "subtasks": ` + string(encoded) + `
}
` + "```\n"

	var p PlanDraftedPayload
	require.NoError(t, ParseMarker(text, &p))
	require.Equal(t, []string{"auth-svc"}, p.ChosenRepos)
	require.Len(t, p.Subtasks, 1)
	require.Equal(t, "Implement signer", p.Subtasks[0].Title)
	require.Equal(t, []string{"auth-svc"}, p.Subtasks[0].Repos)
}

// TestParseTypedPlanFailureIncludesRawJSON guards diagnostic logging:
// when the parse genuinely cannot recover, the raw block must appear
// in the error so the next drift mode is diagnosable from logs alone.
func TestParseTypedPlanFailureIncludesRawJSON(t *testing.T) {
	text := "PLAN_DRAFTED\n```json\n" + `{
  "card_id": "ABC-1",
  "subtasks": 42
}
` + "```\n"

	var p PlanDraftedPayload

	err := ParseMarker(text, &p)
	require.Error(t, err)
	require.Contains(t, err.Error(), `"subtasks": 42`)
}

// TestParseTypedPlanTolerantOfNullRepos guards against agents that
// emit explicit JSON null for optional list fields. Treat null as
// absent rather than failing.
func TestParseTypedPlanTolerantOfNullRepos(t *testing.T) {
	text := "PLAN_DRAFTED\n```json\n" + `{
  "card_id": "ABC-1",
  "plan_summary": "Pure-spec card with null lists.",
  "chosen_repos": null,
  "subtasks": [
    {
      "title": "Spec-only",
      "description": "Just docs.",
      "repos": null,
      "priority": "low",
      "depends_on": null
    }
  ]
}
` + "```\n"

	var p PlanDraftedPayload
	require.NoError(t, ParseMarker(text, &p))
	require.Empty(t, p.ChosenRepos)
	require.Len(t, p.Subtasks, 1)
	require.Empty(t, p.Subtasks[0].Repos)
	require.Empty(t, p.Subtasks[0].DependsOn)
}

func TestParseMarker_ConflictResolved(t *testing.T) {
	text := `CONFLICT_RESOLVED
card_id: SUB-1
status: resolved
files_resolved: [README.md, docs/api.md]
`

	var got ConflictResolvedPayload

	require.NoError(t, ParseMarker(text, &got))

	require.Equal(t, "SUB-1", got.CardID)
	require.Equal(t, "resolved", got.Status)
	require.Equal(t, []string{"README.md", "docs/api.md"}, got.FilesResolved)
}

func TestParseMarker_ConflictUnresolved(t *testing.T) {
	text := `CONFLICT_UNRESOLVED
card_id: SUB-2
status: unresolved
reason: contradictory intent in README.md — manual review required
`

	var got ConflictUnresolvedPayload

	require.NoError(t, ParseMarker(text, &got))

	require.Equal(t, "SUB-2", got.CardID)
	require.Equal(t, "unresolved", got.Status)
	require.Contains(t, got.Reason, "contradictory")
}

func TestParseMarker_RejectsWrongKindForConflictResolved(t *testing.T) {
	text := `TASK_COMPLETE
card_id: SUB-1
status: done
summary: did stuff
`

	var got ConflictResolvedPayload

	err := ParseMarker(text, &got)
	require.Error(t, err)
}
