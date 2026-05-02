package orchestrator

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsErrorGuard(t *testing.T) {
	fsm := newTestFSM(t)
	require.False(t, fsm.IsErrorGuard())

	fsm.ExtendedState.Error = errors.New("boom")
	require.True(t, fsm.IsErrorGuard())
}

func TestNeedsBrainstormGuard(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.StoreMode(ModeHITL)
	require.False(t, fsm.NeedsBrainstormGuard(), "no card → false")

	fsm.ExtendedState.Card = &Card{Type: "feature", DiscoveryComplete: false}
	require.True(t, fsm.NeedsBrainstormGuard())

	fsm.ExtendedState.Card.DiscoveryComplete = true
	require.False(t, fsm.NeedsBrainstormGuard(), "discovery complete → false")

	fsm.ExtendedState.Card = &Card{Type: "task"}
	require.False(t, fsm.NeedsBrainstormGuard(), "non-feature type → false")

	// Brainstorming requires a live human channel; autonomous feature
	// cards skip straight to planning.
	fsm.ExtendedState.StoreMode(ModeAutonomous)
	fsm.ExtendedState.Card = &Card{Type: "feature", DiscoveryComplete: false}
	require.False(t, fsm.NeedsBrainstormGuard(), "autonomous mode → false even for feature")
}

func TestNeedsDiagnosisGuard(t *testing.T) {
	fsm := newTestFSM(t)
	require.False(t, fsm.NeedsDiagnosisGuard(), "no card → false")

	fsm.ExtendedState.Card = &Card{Type: "bug"}
	require.True(t, fsm.NeedsDiagnosisGuard())

	fsm.ExtendedState.Card.Type = "task"
	require.False(t, fsm.NeedsDiagnosisGuard())
}

func TestHasUnfinishedSubtasksGuard(t *testing.T) {
	fsm := newTestFSM(t)
	require.False(t, fsm.HasUnfinishedSubtasksGuard(), "no plan → false")

	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{{Title: "a"}, {Title: "b"}}}
	require.True(t, fsm.HasUnfinishedSubtasksGuard(), "plan with no results yet → unfinished")

	fsm.ExtendedState.SubtaskResults = []ExecuteResult{{}, {}}
	require.False(t, fsm.HasUnfinishedSubtasksGuard(), "results match plan length → finished")
}

func TestSubtasksDoneNoDocsGuard(t *testing.T) {
	fsm := newTestFSM(t)
	require.False(t, fsm.SubtasksDoneNoDocsGuard(), "no card/plan → false")

	fsm.ExtendedState.Card = &Card{}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{{Title: "a"}}}
	fsm.ExtendedState.SubtaskResults = []ExecuteResult{{}}
	require.True(t, fsm.SubtasksDoneNoDocsGuard())

	fsm.ExtendedState.Card.DocsWritten = true
	require.False(t, fsm.SubtasksDoneNoDocsGuard(), "docs written → false")
}

func TestNeedsReviewGuard(t *testing.T) {
	fsm := newTestFSM(t)
	require.False(t, fsm.NeedsReviewGuard(), "no card → false")

	fsm.ExtendedState.Card = &Card{State: "review", DocsWritten: true}
	require.True(t, fsm.NeedsReviewGuard())

	fsm.ExtendedState.Card.ReviewApproved = true
	require.False(t, fsm.NeedsReviewGuard(), "review approved → false")
}

func TestIsReviseGuard(t *testing.T) {
	t.Run("nil ReviewResult returns false", func(t *testing.T) {
		fsm := newTestFSM(t)
		require.False(t, fsm.IsReviseGuard())
	})

	t.Run("approve returns false", func(t *testing.T) {
		fsm := newTestFSM(t)
		fsm.ExtendedState.ReviewResult = &ReviewResult{Recommendation: "approve"}
		require.False(t, fsm.IsReviseGuard())
	})

	t.Run("revise returns true regardless of mode", func(t *testing.T) {
		fsm := newTestFSM(t)
		fsm.ExtendedState.ReviewResult = &ReviewResult{Recommendation: "revise"}
		fsm.ExtendedState.StoreMode(ModeHITL)
		require.True(t, fsm.IsReviseGuard())
		fsm.ExtendedState.StoreMode(ModeAutonomous)
		require.True(t, fsm.IsReviseGuard())
	})
}

func TestIsAutonomousAndMaxAttemptsExceededGuard(t *testing.T) {
	fsm := newTestFSM(t)
	require.False(t, fsm.IsAutonomousAndMaxAttemptsExceededGuard(), "no card → false")

	fsm.ExtendedState.StoreMode(ModeAutonomous)
	fsm.ExtendedState.Card = &Card{RevisionAttempts: 2}
	require.False(t, fsm.IsAutonomousAndMaxAttemptsExceededGuard())

	fsm.ExtendedState.Card.RevisionAttempts = maxAutonomousRevisionAttempts
	require.True(t, fsm.IsAutonomousAndMaxAttemptsExceededGuard())

	fsm.ExtendedState.StoreMode(ModeHITL)
	require.False(t, fsm.IsAutonomousAndMaxAttemptsExceededGuard(), "HITL never exceeds")
}

func TestAllRemainingBlockedGuard(t *testing.T) {
	fsm := newTestFSM(t)
	require.False(t, fsm.AllRemainingBlockedGuard(), "empty results → false")

	fsm.ExtendedState.SubtaskResults = []ExecuteResult{{Status: "done"}, {Status: "blocked"}}
	require.False(t, fsm.AllRemainingBlockedGuard(), "mixed results → false")

	fsm.ExtendedState.SubtaskResults = []ExecuteResult{{Status: "blocked"}, {Status: "blocked"}}
	require.True(t, fsm.AllRemainingBlockedGuard())
}

func TestHasNewSubtasksFromDecompositionGuard(t *testing.T) {
	fsm := newTestFSM(t)
	require.False(t, fsm.HasNewSubtasksFromDecompositionGuard(), "no plan → false")

	// All subtasks have IDs (steady state after CreatingSubtasks ran):
	// the guard must NOT fire, otherwise the FSM loops forever between
	// Executing and CreatingSubtasks.
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{
		{ID: "PROJ-1", Title: "a"},
		{ID: "PROJ-2", Title: "b"},
	}}
	require.False(t, fsm.HasNewSubtasksFromDecompositionGuard(),
		"every subtask has an ID → false")

	// Decomposition just appended a proposal without an ID — the guard
	// must fire so the FSM routes through CreatingSubtasks before the
	// next execute pass.
	fsm.ExtendedState.Plan.Subtasks = append(fsm.ExtendedState.Plan.Subtasks,
		Subtask{Title: "proposed-child"},
	)
	require.True(t, fsm.HasNewSubtasksFromDecompositionGuard(),
		"proposed subtask without an ID → true")
}
