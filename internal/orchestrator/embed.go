package orchestrator

import _ "embed"

// Phase prompts are embedded into the binary so worker containers don't
// need a writable filesystem to access them. Add new prompts here as
// each phase action lands.

//go:embed prompts/plan.md
var promptPlan string

//go:embed prompts/replan.md
var promptReplan string

//go:embed prompts/brainstorm.md
var promptBrainstorm string

//go:embed prompts/diagnose.md
var promptDiagnose string

//go:embed prompts/document.md
var promptDocument string

//go:embed prompts/execute.md
var promptExecute string

//go:embed prompts/resolve_conflict.md
var promptResolveConflict string

//go:embed prompts/review.md
var promptReview string
