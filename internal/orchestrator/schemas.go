package orchestrator

// Card mirrors the CM-side card data the orchestrator needs.
type Card struct {
	ID                string
	Project           string
	Title             string
	Description       string
	Type              string
	State             string
	Autonomous        bool
	Body              string   // raw markdown body with sections
	Repos             []string // hint
	ChosenRepos       []string // authoritative after plan
	BlockerCards      []string // runtime blockers (distinct from DependsOn)
	BranchName        string   // parent's feature branch (one PR per parent)
	BaseBranch        string   // base branch the PR targets; defaults to "main"
	RevisionAttempts  int
	RevisionRequested bool
	PlanApproved      bool
	ReviewApproved    bool
	DiscoveryComplete bool
	AgentSessions     map[string]string // purpose → session_id
	DocsWritten       bool
}

// CardContext bundles a card with its parent + siblings + project config.
type CardContext struct {
	Card         *Card
	Parent       *Card
	Siblings     []*Card
	ProjectRepos []RepoSpec
}

// RepoSpec mirrors board.RepoSpec from CM (slug + URL + description).
type RepoSpec struct {
	Slug        string
	URL         string
	Description string
}

// ProjectKB is the tiered project knowledge base returned by MCP.
type ProjectKB struct {
	Repos       map[string]string
	JiraProject string
	Project     string
}

// Plan is the typed output of the plan phase.
type Plan struct {
	Subtasks    []Subtask
	ChosenRepos []string
	Reasoning   string
	Summary     string
}

// Subtask is one entry in the plan.
//
// ID is empty when the plan first lands and is populated by
// CreateSubtaskCardsAction with the CM-assigned card ID. Downstream actions
// (Executing, Reviewing, etc.) reference subtasks by this ID.
//
// DependsOn is the list of card IDs this subtask depends on — passed to
// CM as the create_card `depends_on` field, which drives the visible
// blocked / deps-met badges and the get_ready_tasks dispatch order.
// CreateSubtaskCardsAction appends each previous subtask's assigned ID
// here so sequential subtasks render correctly in the CM UI even
// though the orchestrator currently dispatches them one at a time.
type Subtask struct {
	ID          string
	Title       string
	Description string
	Repos       []string
	Priority    string
	DependsOn   []string
}

// ExecuteResult is the typed output of one execute sub-agent.
type ExecuteResult struct {
	SubtaskID        string
	Status           string // done | blocked | needs_decomposition
	Summary          string
	Commits          []CommitInfo
	BlockerCards     []string // present when blocked
	NeedsHuman       bool
	ProposedSubtasks []Subtask // present when needs_decomposition
	Usage            TokenUsage
}

// CommitInfo references one commit produced during execute.
type CommitInfo struct {
	Repo string
	SHA  string
}

// TokenUsage is per-phase token accounting.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	Model            string
}

// DocumentResult is the typed output of the documentation phase.
type DocumentResult struct {
	FilesWritten []string
	Usage        TokenUsage
}

// ReviewResult is the typed output of the review phase.
type ReviewResult struct {
	Recommendation string // approve | approve_with_notes | revise
	Summary        string
	// Feedback is the detailed change request set by the HITL review_revise
	// tool call; primes the next replan round.
	Feedback string
	Usage    TokenUsage
}

// CreateCardInput captures the args for creating a subtask card via MCP.
//
// DependsOn maps to CM's create_card `depends_on` field — the list of
// card IDs that must be in `done` state before this card is considered
// ready. Driving this from the orchestrator means CM's web UI shows
// the correct blocked / deps-met badges on freshly-created subtasks.
type CreateCardInput struct {
	Title       string
	Description string
	Type        string
	Parent      string
	Repos       []string
	DependsOn   []string
	Priority    string
}
