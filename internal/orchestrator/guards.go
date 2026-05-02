package orchestrator

// maxAutonomousRevisionAttempts caps how many times an autonomous run will
// loop through the revise → replan → execute cycle before halting. The cap
// is intentionally hard-coded: HITL runs do not consult this guard.
const maxAutonomousRevisionAttempts = 3

// +vectorsigma:guard:AllRemainingBlocked
//
// True when every recorded subtask result reports Status == "blocked".
// Empty result sets return false so an unstarted execute phase does not look
// like an all-blocked terminal state.
func (fsm *ContextMatrixOrchestrator) AllRemainingBlockedGuard(_ ...string) bool {
	res := fsm.ExtendedState.SubtaskResults
	if len(res) == 0 {
		return false
	}

	for _, r := range res {
		if r.Status != "blocked" {
			return false
		}
	}

	return true
}

// +vectorsigma:guard:HasNewSubtasksFromDecomposition
//
// True when the Plan still contains a subtask without a CM card ID —
// the signal that the executor proposed decomposition and the new
// proposals have not yet flowed through CreatingSubtasks. The FSM
// routes Executing → CreatingSubtasks → Executing on this guard so the
// proposed children get IDs before the next execute pass runs them.
func (fsm *ContextMatrixOrchestrator) HasNewSubtasksFromDecompositionGuard(_ ...string) bool {
	plan := fsm.ExtendedState.Plan
	if plan == nil {
		return false
	}

	for _, st := range plan.Subtasks {
		if st.ID == "" {
			return true
		}
	}

	return false
}

// +vectorsigma:guard:HasUnfinishedSubtasks
//
// HEURISTIC: True when a Plan exists and not every Subtask in the plan has a
// matching ExecuteResult yet. Refine in integration if a more precise mapping
// (e.g. by SubtaskID) becomes necessary.
func (fsm *ContextMatrixOrchestrator) HasUnfinishedSubtasksGuard(_ ...string) bool {
	p := fsm.ExtendedState.Plan
	if p == nil {
		return false
	}

	return len(fsm.ExtendedState.SubtaskResults) < len(p.Subtasks)
}

// +vectorsigma:guard:IsRevise
//
// True when the most recent review recommendation is "revise". Mode-agnostic:
// HITL chat-loops set the recommendation directly via the review_revise tool
// call; autonomous reviewers set it from their devil's-advocate analysis.
// The autonomous max-attempts cap is enforced separately by
// IsAutonomousAndMaxAttemptsExceeded in CheckingRevisionBudget.
func (fsm *ContextMatrixOrchestrator) IsReviseGuard(_ ...string) bool {
	r := fsm.ExtendedState.ReviewResult

	return r != nil && r.Recommendation == "revise"
}

// +vectorsigma:guard:IsAutonomousAndMaxAttemptsExceeded
//
// True in autonomous mode when Card.RevisionAttempts has reached
// maxAutonomousRevisionAttempts. HITL runs always return false; humans
// decide when to stop iterating.
func (fsm *ContextMatrixOrchestrator) IsAutonomousAndMaxAttemptsExceededGuard(_ ...string) bool {
	if fsm.ExtendedState.LoadMode() != ModeAutonomous {
		return false
	}

	c := fsm.ExtendedState.Card
	if c == nil {
		return false
	}

	return c.RevisionAttempts >= maxAutonomousRevisionAttempts
}

// +vectorsigma:guard:IsError
//
// True when ExtendedState.Error is non-nil. Phase actions never return errors
// directly; they record them on ExtendedState so this guard can route the
// FSM to HandlingError.
func (fsm *ContextMatrixOrchestrator) IsErrorGuard(_ ...string) bool {
	return fsm.ExtendedState.Error != nil
}

// +vectorsigma:guard:NeedsBrainstorm
//
// HEURISTIC: True for feature-type cards whose discovery has not been marked
// complete. Refine in integration if other card types adopt brainstorming or
// the discovery flag is renamed.
//
// Brainstorming requires a live human channel — the brainstorm prompt
// drives a multi-turn dialogue where the user answers clarifying
// questions and confirms the design. Autonomous runs have no such
// channel, so the guard returns false in autonomous mode regardless
// of card type. Autonomous feature cards skip straight to planning.
func (fsm *ContextMatrixOrchestrator) NeedsBrainstormGuard(_ ...string) bool {
	c := fsm.ExtendedState.Card
	if c == nil {
		return false
	}

	if fsm.ExtendedState.LoadMode() != ModeHITL {
		return false
	}

	return c.Type == "feature" && !c.DiscoveryComplete
}

// +vectorsigma:guard:NeedsDiagnosis
//
// HEURISTIC: True for bug-type cards. Plan 3 follow-up tasks may extend this
// to skip diagnosis when a diagnosis result is already captured.
func (fsm *ContextMatrixOrchestrator) NeedsDiagnosisGuard(_ ...string) bool {
	c := fsm.ExtendedState.Card
	if c == nil {
		return false
	}

	return c.Type == "bug"
}

// +vectorsigma:guard:NeedsReview
//
// True when documentation has been written, the reviewer has not yet
// approved, and the card is currently in the "review" state on CM.
func (fsm *ContextMatrixOrchestrator) NeedsReviewGuard(_ ...string) bool {
	c := fsm.ExtendedState.Card
	if c == nil {
		return false
	}

	return c.DocsWritten && !c.ReviewApproved && c.State == "review"
}

// +vectorsigma:guard:SubtasksDoneNoDocs
//
// HEURISTIC: True when every planned subtask has a result and the card has
// not yet had docs written. Mirrors HasUnfinishedSubtasks but inverted.
func (fsm *ContextMatrixOrchestrator) SubtasksDoneNoDocsGuard(_ ...string) bool {
	c := fsm.ExtendedState.Card

	p := fsm.ExtendedState.Plan
	if c == nil || p == nil {
		return false
	}

	return len(fsm.ExtendedState.SubtaskResults) >= len(p.Subtasks) && !c.DocsWritten
}
