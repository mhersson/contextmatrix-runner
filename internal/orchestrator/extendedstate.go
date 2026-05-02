package orchestrator

import (
	"context"
	"log/slog"
	"sync/atomic"

	githubauth "github.com/mhersson/contextmatrix-githubauth"
	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
	"github.com/mhersson/contextmatrix-runner/internal/workspace"
)

// Context holds stable infrastructure dependencies. Set once when the
// per-card driver constructs the FSM; never mutated by the FSM.
type Context struct {
	Logger    *slog.Logger // Required by vectorsigma generated code
	MCP       MCPClient
	Claude    *claudeclient.Wrapper
	Workspace *workspace.Manager
	GitTokens githubauth.TokenGenerator
	Notifier  Notifier

	// ClaudeAuthEnv is the static auth env injected on every Claude
	// docker-exec (CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY). It is
	// per-exec rather than spawn-time because the worker's PID 1 is
	// `sleep infinity` and `docker exec` does not inherit the
	// entrypoint shell's env — only Container.Config.Env plus per-exec
	// env. Empty when Claude auth is provided via a mounted
	// claude_auth_dir on disk.
	ClaudeAuthEnv map[string]string

	// SkillIndex is the curated task-skill catalog loaded from
	// cfg.TaskSkillsDir at dispatch time. Empty when task-skills are
	// disabled. Priming builders render the per-card subset selected
	// via ExtendedState.TaskSkills.
	SkillIndex []SkillInfo

	// Worker container handle (created by the driver at start-of-card).
	WorkerContainerID string
}

// Notifier publishes orchestrator-level status messages into the
// runner's chat/log broadcast stream so the UI shows progress between
// agent turns. The legacy runner sent "cloning ... into workspace" at
// startup; this interface is the modern equivalent — the orchestrator
// calls Notify with phase-start announcements ("Starting plan
// phase...") so the user sees activity immediately rather than staring
// at "No messages yet" while the first agent invocation thinks for 90
// seconds before emitting any text.
type Notifier interface {
	// Notify publishes a system-typed message to the broadcaster.
	// kind is the LogEntry type (e.g. "system", "text"); message is
	// the human-readable content. Implementations must be safe to
	// call from any FSM goroutine.
	Notify(kind, message string)
}

// MCPClient is the small surface of the runner's MCP client used by
// orchestrator actions. The production implementation talks to CM
// over HTTPS with Bearer auth.
type MCPClient interface {
	ClaimCard(ctx context.Context, project, cardID, agentID string) error
	GetTaskContext(ctx context.Context, project, cardID, agentID string) (*CardContext, error)
	UpdateCardBody(ctx context.Context, project, cardID, sectionName, content string) error
	UpdateCardField(ctx context.Context, project, cardID string, fields map[string]any) error
	Heartbeat(ctx context.Context, project, cardID, agentID string) error
	CompleteTask(ctx context.Context, project, cardID, agentID, summary string) error
	ReleaseCard(ctx context.Context, project, cardID, agentID string) error
	AddLog(ctx context.Context, project, cardID, agentID, action, message string) error
	ReportUsage(ctx context.Context, project, cardID, agentID string, prompt, completion int, model string) error
	ReportPush(ctx context.Context, project, cardID, agentID, repo, branch, prURL string) error
	TransitionCard(ctx context.Context, project, cardID, agentID, toState string) error
	CreateCard(ctx context.Context, project string, in CreateCardInput) (string, error)
	GetProjectKB(ctx context.Context, project string, repoSlug ...string) (ProjectKB, error)
}

// Mode controls HITL vs autonomous behavior. Stored as int32 inside
// ExtendedState so it can be flipped atomically when promotion fires from
// the driver dispatch goroutine while the FSM goroutine reads it.
type Mode int32

const (
	ModeAutonomous Mode = 0
	ModeHITL       Mode = 1
)

// ExtendedState holds the per-card FSM working state. The driver
// initializes RunCtx/RunCancel and the chat/stop channels at construction;
// actions populate phase outputs and read channels.
type ExtendedState struct {
	Error error // checked by IsError guard

	//nolint:containedctx // FSM action signatures cannot accept ctx params;
	// the per-card driver pre-builds a cancellable run context the actions
	// honor for I/O and subprocess lifetimes.
	RunCtx    context.Context
	RunCancel context.CancelFunc

	Card *Card

	// Phase outputs (populated by phase actions, read by guards).
	Plan           *Plan
	SubtaskResults []ExecuteResult
	DocsResult     *DocumentResult
	ReviewResult   *ReviewResult

	// The currently-active CC subprocess for the current phase. Set by
	// ephemeral phase actions; cleared on phase completion.
	ActiveCC claudeclient.Process

	// StopCh aborts any in-flight phase action; the driver closes it on
	// shutdown / external stop.
	StopCh chan struct{}

	// ChatInputCh receives user chat messages during brainstorming and
	// the HITL plan/review chat-loops.
	ChatInputCh chan string

	// Identity / routing context the driver passes to the FSM.
	AgentID string
	Project string
	CardID  string

	// TaskSkills mirrors the webhook payload field. Selection rules:
	//   nil       → mount the full curated set (no card-level override)
	//   &[]       → explicit no-skills selection
	//   &names    → mount only the named subset
	// Priming builders pass this to FilterSkills(Context.SkillIndex, …).
	TaskSkills *[]string

	// mode is read by the FSM goroutine (in actions and guards) and
	// written by the driver's dispatch goroutine on a /promote event.
	// Use LoadMode() / StoreMode() rather than touching the field directly.
	mode atomic.Int32
}

// LoadMode returns the current FSM mode (HITL or autonomous).
func (e *ExtendedState) LoadMode() Mode { return Mode(e.mode.Load()) }

// StoreMode sets the FSM mode atomically. Driver dispatch calls this on
// promotion mid-run; the per-card driver also seeds it once at startup.
func (e *ExtendedState) StoreMode(m Mode) { e.mode.Store(int32(m)) }
