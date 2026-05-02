package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	githubauth "github.com/mhersson/contextmatrix-githubauth"
	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
	"github.com/mhersson/contextmatrix-runner/internal/workspace"
)

// logPhase emits a card activity-log entry recording FSM phase entry,
// and — for phases with a noticeable agent-turn delay — ALSO publishes
// a system-typed chat message so the UI surfaces a "we're working on
// it" cue immediately rather than leaving "No messages yet" while the
// first agent invocation thinks for ~90 seconds before emitting any
// text.
//
// Activity-log AddLog is best-effort — errors are intentionally
// ignored so phase progression is observable from the UI even if the
// MCP call momentarily fails. The chat-stream Notify is fire-and-forget
// for the same reason.
func logPhase(fsm *ContextMatrixOrchestrator, name string) {
	if fsm.Context.MCP != nil {
		_ = fsm.Context.MCP.AddLog(
			fsm.ExtendedState.RunCtx,
			fsm.ExtendedState.Project,
			fsm.ExtendedState.CardID,
			fsm.ExtendedState.AgentID,
			"phase",
			name,
		)
	}

	if msg := chatMessageForPhase(name); msg != "" {
		notifyChat(fsm, "system", msg)
	}
}

// chatMessageForPhase returns the chat-friendly announcement for a
// phase name, or "" if no chat announcement should fire. Phases that
// finish quickly (subtasks creation, claim) emit no chat noise; phases
// that drive an agent turn announce themselves so the user sees
// activity before the agent's first text frame arrives.
func chatMessageForPhase(name string) string {
	switch name {
	case "plan":
		return "Starting plan phase — reading the card and drafting subtasks."
	case "replan":
		return "Replanning based on the latest review feedback."
	case "execute":
		return "Starting execute phase — spawning subtask agents."
	case "document":
		return "Analyzing what was built and writing documentation if needed."
	case "review":
		return "Starting review phase — evaluating the work against the plan."
	case "diagnose":
		return "Starting diagnosis — investigating the bug before planning."
	case "brainstorm":
		return "Starting brainstorming — building context, please wait."
	case "commit":
		return "Pushing feature branch and opening PR."
	}

	return ""
}

// notifyChat publishes an orchestrator-level status message into the
// runner's broadcast stream so the chat UI shows activity between
// agent turns. Best-effort: nil notifier is a no-op (tests rarely wire
// one). The legacy runner sent "cloning ... into workspace" at startup;
// this is the modern equivalent — the orchestrator announces phase
// boundaries and other long-running waits so the user sees progress
// instead of "No messages yet".
func notifyChat(fsm *ContextMatrixOrchestrator, kind, message string) {
	if fsm.Context.Notifier == nil {
		return
	}

	fsm.Context.Notifier.Notify(kind, message)
}

// +vectorsigma:action:ClaimCard
//
// Calls MCP.ClaimCard on the card identified by ExtendedState.{Project,CardID,AgentID},
// then fetches the populated card via MCP.GetTaskContext and assigns it to
// ExtendedState.Card. Any error is captured on ExtendedState.Error so the
// IsError guard routes the FSM to HandlingError on the way out of ClaimingCard.
//
// HEURISTIC: This is the minimal implementation needed for the routing guards
// to have a populated Card. Integration follow-ups in Plan 3 may extend it
// (e.g. propagating Parent/Siblings/ProjectRepos from CardContext).
func (fsm *ContextMatrixOrchestrator) ClaimCardAction(_ ...string) error {
	if fsm.Context.MCP == nil {
		fsm.ExtendedState.Error = errors.New("claim card: mcp client not configured")

		return nil
	}

	ctx := fsm.ExtendedState.RunCtx
	if err := fsm.Context.MCP.ClaimCard(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID); err != nil {
		fsm.ExtendedState.Error = fmt.Errorf("claim card: %w", err)

		return nil
	}

	cc, err := fsm.Context.MCP.GetTaskContext(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID)
	if err != nil {
		fsm.ExtendedState.Error = fmt.Errorf("get task context: %w", err)

		return nil
	}

	fsm.ExtendedState.Card = cc.Card

	return nil
}

// +vectorsigma:action:CreateSubtaskCards
//
// Iterates over ExtendedState.Plan.Subtasks and creates a child card on
// CM via MCP.CreateCard for each. The current card is set as the parent
// so the CM-side state machine wires up subtask/parent relationships.
// Errors abort the loop and are captured on ExtendedState.Error so the
// IsError guard routes the FSM to HandlingError.
func (fsm *ContextMatrixOrchestrator) CreateSubtaskCardsAction(_ ...string) error {
	logPhase(fsm, "subtasks")

	ctx := fsm.ExtendedState.RunCtx
	if fsm.ExtendedState.Plan == nil || len(fsm.ExtendedState.Plan.Subtasks) == 0 {
		return nil
	}

	if fsm.Context.MCP == nil {
		fsm.ExtendedState.Error = errors.New("create subtasks: mcp client not configured")

		return nil
	}

	// Track each created subtask so a sibling later in the plan can
	// declare a dependency on it. The plan prompt requires the agent to
	// list subtasks in dependency order: CreateSubtaskCardsAction
	// appends each previous subtask's just-assigned ID to the next
	// subtask's depends_on, so CM renders the blocked / deps-met
	// badges correctly even though the orchestrator currently
	// dispatches them sequentially.
	var prevID string

	for i := range fsm.ExtendedState.Plan.Subtasks {
		st := &fsm.ExtendedState.Plan.Subtasks[i]

		// Skip subtasks that already have an ID (replan paths may rerun
		// this action against a partially-created plan; idempotent reuse
		// also covers the CM-side dedup-by-title guard returning the
		// existing card unchanged).
		if st.ID != "" {
			prevID = st.ID

			continue
		}

		priority := st.Priority
		if priority == "" {
			priority = "medium"
		}

		// Merge the agent-supplied deps with the implicit
		// "previous-subtask" dep, deduplicating so the same ID never
		// shows up twice on a card.
		deps := dedupeStrings(append([]string{}, st.DependsOn...))
		if prevID != "" {
			deps = dedupeStrings(append(deps, prevID))
		}

		id, err := fsm.Context.MCP.CreateCard(ctx, fsm.ExtendedState.Project, CreateCardInput{
			Title:       st.Title,
			Description: st.Description,
			Type:        "task",
			Parent:      fsm.ExtendedState.CardID,
			Repos:       st.Repos,
			DependsOn:   deps,
			Priority:    priority,
		})
		if err != nil {
			fsm.ExtendedState.Error = fmt.Errorf("create subtask %q: %w", st.Title, err)

			return nil
		}

		st.ID = id
		st.DependsOn = deps
		prevID = id
	}

	return nil
}

// dedupeStrings returns s with duplicate entries removed, preserving the
// first occurrence's order. Empty strings are dropped.
func dedupeStrings(s []string) []string {
	seen := make(map[string]struct{}, len(s))
	out := s[:0]

	for _, v := range s {
		if v == "" {
			continue
		}

		if _, ok := seen[v]; ok {
			continue
		}

		seen[v] = struct{}{}
		out = append(out, v)
	}

	return out
}

// +vectorsigma:action:DecideStartingPhase
//
// No-op marker action for the Routing state. The actual routing decision is
// performed by the outgoing transition guards on Routing (NeedsBrainstorm,
// NeedsDiagnosis, HasUnfinishedSubtasks, SubtasksDoneNoDocs, NeedsReview, and
// the Planning fallback).
func (fsm *ContextMatrixOrchestrator) DecideStartingPhaseAction(_ ...string) error {
	return nil
}

// +vectorsigma:action:EmitAutonomousHalted
//
// Records an "halted" activity log entry on the card describing why the
// autonomous loop gave up. Called when max revision attempts is exceeded.
// Best-effort: AddLog errors are intentionally ignored — we are already
// on the way out.
func (fsm *ContextMatrixOrchestrator) EmitAutonomousHaltedAction(_ ...string) error {
	if fsm.Context.MCP == nil {
		return nil
	}

	msg := "autonomous mode halted: max revision attempts exceeded"
	if fsm.ExtendedState.ReviewResult != nil && fsm.ExtendedState.ReviewResult.Summary != "" {
		msg += " — last review: " + fsm.ExtendedState.ReviewResult.Summary
	}

	_ = fsm.Context.MCP.AddLog(fsm.ExtendedState.RunCtx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID, "halted", msg)

	return nil
}

// +vectorsigma:action:HandleError
//
// Terminal action for the HandlingError state. Logs the captured error,
// records it in the card's activity log, and releases the card claim.
// Both MCP calls are best-effort. ExtendedState.Error is preserved so
// the FSM caller can observe the failure.
func (fsm *ContextMatrixOrchestrator) HandleErrorAction(_ ...string) error {
	if fsm.ExtendedState.Error == nil {
		return nil
	}

	if fsm.Context.Logger != nil {
		fsm.Context.Logger.Error("orchestrator error",
			"card", fsm.ExtendedState.CardID,
			"err", fsm.ExtendedState.Error,
		)
	}

	if fsm.Context.MCP != nil {
		_ = fsm.Context.MCP.AddLog(fsm.ExtendedState.RunCtx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID, "error", fsm.ExtendedState.Error.Error())
		_ = fsm.Context.MCP.ReleaseCard(fsm.ExtendedState.RunCtx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID)
	}

	return nil
}

// +vectorsigma:action:IncrementRevisionAttempts
//
// Bumps Card.RevisionAttempts and pushes the new value to CM via
// MCP.UpdateCardField. Used after a "revise" review to track how many
// revision rounds have occurred so the autonomous max-attempts guard
// can fire.
func (fsm *ContextMatrixOrchestrator) IncrementRevisionAttemptsAction(_ ...string) error {
	if fsm.ExtendedState.Card == nil {
		return nil
	}

	fsm.ExtendedState.Card.RevisionAttempts++

	if fsm.Context.MCP != nil {
		_ = fsm.Context.MCP.UpdateCardField(fsm.ExtendedState.RunCtx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, map[string]any{
			"revision_attempts": fsm.ExtendedState.Card.RevisionAttempts,
		})
	}

	return nil
}

// +vectorsigma:action:Initialize
func (fsm *ContextMatrixOrchestrator) InitializeAction(_ ...string) error {
	fsm.ExtendedState.StopCh = make(chan struct{}, 1)
	fsm.ExtendedState.ChatInputCh = make(chan string, 16)

	// Probe GitHub auth at startup so a misconfigured runner fails this
	// card cleanly instead of letting the first git/gh exec inside the
	// worker surface the error mid-phase. Mint is cheap when the
	// underlying provider is a CachingProvider — first call rounds-trips
	// to GitHub, the rest hit the in-process cache.
	if fsm.Context.GitTokens != nil {
		if _, _, err := fsm.Context.GitTokens.GenerateToken(fsm.ExtendedState.RunCtx); err != nil {
			fsm.ExtendedState.Error = fmt.Errorf("initialize: github token mint: %w", err)

			return nil
		}
	}

	return nil
}

// +vectorsigma:action:PushBranchesAndOpenPRs
//
// Pushes the per-parent feature branch (already advanced through
// subtask integrations during the execute phase) and opens one PR
// per repo:
//
//  1. Determine the feature branch from Card.BranchName, falling back
//     to "cm/<parent-card-id>" when the card omits it.
//  2. Determine the base branch from Card.BaseBranch, falling back to
//     "main".
//  3. For each repo in the parent's chosen_repos:
//     a. Push the feature branch to origin.
//     b. Open one PR via the gh CLI.
//     c. Report a single push per repo via MCP.ReportPush.
//
// Subtask integrations (rebase + ff onto cm/<parent>) happened in the
// execute phase, so there is no merge step here. Failures are logged
// but never fatal: the work is already committed locally on the
// feature branch, and the orchestrator must always be able to reach
// Completing so the parent card transitions to done. Uses a fresh
// background context because CM emits a "stop" SSE event when the
// parent transitions to a terminal state, which would otherwise
// cancel the run ctx mid-push.
func (fsm *ContextMatrixOrchestrator) PushBranchesAndOpenPRsAction(_ ...string) error {
	logPhase(fsm, "commit")

	if fsm.ExtendedState.Card == nil || fsm.ExtendedState.Plan == nil {
		return nil
	}

	if fsm.Context.MCP == nil {
		return nil
	}

	repos := fsm.ExtendedState.Card.ChosenRepos
	if len(repos) == 0 {
		repos = fsm.ExtendedState.Plan.ChosenRepos
	}

	if len(repos) == 0 {
		return nil
	}

	featureBranch := fsm.ExtendedState.Card.BranchName
	if featureBranch == "" {
		featureBranch = "cm/" + fsm.ExtendedState.CardID
	}

	baseBranch := fsm.ExtendedState.Card.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	if fsm.Context.Workspace == nil {
		// No Workspace wired (test-only path): record the intended push
		// per repo so CM still sees a per-parent push entry.
		return fsm.recordPushIntent(repos, featureBranch)
	}

	pushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	title := fmt.Sprintf("%s: %s", fsm.ExtendedState.CardID, fsm.ExtendedState.Card.Title)
	body := fmt.Sprintf(
		"Auto-generated PR aggregating subtask branches for `%s`.\n\n%s\n\n_Generated by ContextMatrix Runner._",
		fsm.ExtendedState.CardID,
		planSummary(fsm.ExtendedState.Plan),
	)

	for _, repo := range repos {
		fsm.finalizeRepo(pushCtx, repo, featureBranch, baseBranch, title, body)
	}

	return nil
}

// finalizeRepo pushes the parent feature branch and opens one PR per
// repo. By this point every successfully-completed subtask has
// already been rebased + fast-forwarded onto featureBranch by the
// execute phase, so there is no merge step here. Per-step failures
// don't fail the action — they're mirrored to the CM activity log
// via add_log so the UI surfaces what got stuck.
func (fsm *ContextMatrixOrchestrator) finalizeRepo(ctx context.Context, repo, featureBranch, baseBranch, title, body string) {
	if err := fsm.Context.Workspace.PushBranch(ctx, repo, featureBranch); err != nil {
		fsm.recordFinalizeFailure(ctx,
			fmt.Sprintf("push %s on %s failed: %v", featureBranch, repo, err),
			"push failed",
			"repo", repo, "branch", featureBranch, "err", err,
		)

		return
	}

	prURL := ""

	if url, err := fsm.Context.Workspace.OpenPR(ctx, repo, featureBranch, baseBranch, title, body); err != nil {
		fsm.recordFinalizeFailure(ctx,
			fmt.Sprintf("open PR for %s on %s failed: %v", featureBranch, repo, err),
			"PR open failed",
			"repo", repo, "branch", featureBranch, "err", err,
		)
	} else {
		prURL = url

		if fsm.Context.Logger != nil {
			fsm.Context.Logger.Info("finalize: PR opened",
				"repo", repo,
				"branch", featureBranch,
				"pr_url", prURL,
			)
		}
	}

	_ = fsm.Context.MCP.ReportPush(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID, repo, featureBranch, prURL)
}

// recordFinalizeFailure mirrors a finalize sub-step failure to both the
// runner's structured log (for operator triage) and the CM activity log
// (for the user — surfaces in the card UI). Best-effort on AddLog: the
// remote call may fail mid-shutdown but the runner log already captured
// the failure.
func (fsm *ContextMatrixOrchestrator) recordFinalizeFailure(ctx context.Context, message, logSummary string, kv ...any) {
	if fsm.Context.Logger != nil {
		fsm.Context.Logger.Warn("finalize: "+logSummary, kv...)
	}

	if fsm.Context.MCP != nil {
		_ = fsm.Context.MCP.AddLog(ctx,
			fsm.ExtendedState.Project,
			fsm.ExtendedState.CardID,
			fsm.ExtendedState.AgentID,
			"finalize_failed",
			message,
		)
	}
}

// recordPushIntent is the no-Workspace fallback path: records one
// push per repo for the parent's feature branch via ReportPush so CM
// at least sees a single intended push entry per repo.
func (fsm *ContextMatrixOrchestrator) recordPushIntent(repos []string, featureBranch string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, repo := range repos {
		_ = fsm.Context.MCP.ReportPush(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID, repo, featureBranch, "")
	}

	return nil
}

// chatLoopConfig configures a HITL chat-loop iteration. Each phase action
// (brainstorm, plan, review) calls runChatLoop with phase-specific values
// for the marker recognition and payload application.
type chatLoopConfig struct {
	phase        string   // for log prefixes
	systemPrompt string   // base system prompt (e.g. promptPlan)
	primer       string   // priming text appended to the system prompt on the first turn only
	model        string   // claude model id
	allowedTools []string // --allowed-tools list, including the terminal-marker MCP tool

	// onTerminalMarker is called when a tool_use event matches a terminal
	// marker for this phase. Returning a non-nil error aborts the loop
	// with that error. Returning (true, nil) terminates cleanly.
	onTerminalMarker func(ctx context.Context, m claudeclient.Marker) (terminate bool, err error)
}

// drainPendingChatInput appends any messages already buffered on ch
// (non-blockingly) onto first, joining them with a blank line so the
// concatenation reads as one combined user turn. It is used by
// runChatLoop to coalesce a back-to-back batch of human messages into a
// single response — typing "GO" + "wait, do X first" while the agent
// hasn't prompted yet would otherwise see "GO" answer the next prompt
// and "wait, do X first" answer the one after that.
func drainPendingChatInput(first string, ch <-chan string) string {
	if ch == nil {
		return first
	}

	parts := []string{first}

	for {
		select {
		case extra, ok := <-ch:
			if !ok {
				return strings.Join(parts, "\n\n")
			}

			parts = append(parts, extra)
		default:
			return strings.Join(parts, "\n\n")
		}
	}
}

// gitTokenEnv mints a fresh GitHub token via g (when configured) and
// returns it as the CM_GIT_TOKEN / GH_TOKEN env pair the worker
// container's git credential helper and gh CLI consume. Returns nil
// (no env injected) when g is nil or returns an empty token; that
// path leaves Claude's git/gh subprocesses unauthenticated, which is
// fine for public-repo clones and surfaces a clear "auth required"
// error from the underlying tool for private ops. Returns an error
// if GenerateToken itself fails so the Spawn call aborts the phase
// rather than launching Claude with stale auth.
//
// The credential helper in docker/entrypoint-orchestrated.sh reads
// CM_GIT_TOKEN from the per-exec env at the moment git asks for
// credentials. Without this helper on the Claude-spawn path, the
// helper sees an empty value and every agent-driven `git clone` of
// a private repo fails — which was the regression introduced when
// tokens moved from worker-spawn env to per-exec env.
func gitTokenEnv(ctx context.Context, g githubauth.TokenGenerator) (map[string]string, error) {
	if g == nil {
		return nil, nil
	}

	tok, _, err := g.GenerateToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("mint github token: %w", err)
	}

	if tok == "" {
		return nil, nil
	}

	return map[string]string{
		"CM_GIT_TOKEN": tok,
		"GH_TOKEN":     tok,
	}, nil
}

// spawnEnv returns the per-exec env for a Claude docker-exec: a
// freshly-minted GitHub token (when configured) merged with the
// runner's static Claude auth env. Without ClaudeAuthEnv on the spawn,
// the worker's `claude` subprocess sees no CLAUDE_CODE_OAUTH_TOKEN /
// ANTHROPIC_API_KEY (the entrypoint shell that sourced the secrets
// file is a different process tree than docker-exec), and Claude
// errors out with "Not logged in".
func spawnEnv(ctx context.Context, fsmCtx *Context) (map[string]string, error) {
	env, err := gitTokenEnv(ctx, fsmCtx.GitTokens)
	if err != nil {
		return nil, err
	}

	for k, v := range fsmCtx.ClaudeAuthEnv {
		if env == nil {
			env = make(map[string]string, len(fsmCtx.ClaudeAuthEnv))
		}

		env[k] = v
	}

	return env, nil
}

// runChatLoop drives a multi-turn chat with the human via ChatInputCh.
// Each turn is a fresh `claude --print --resume` invocation: send the
// user message, close stdin, drain the response, capture the new
// session_id from system_init for the next turn. claude's --print mode
// is one-shot (it exits after each response per CLI design), so
// continuity is maintained via --resume rather than a long-lived process.
//
// Returns nil on terminal marker, ctx.Err() on context cancellation,
// ErrStopped on stop signal, or a wrapped error on spawn/IO failures.
func (fsm *ContextMatrixOrchestrator) runChatLoop(ctx context.Context, cfg chatLoopConfig) error {
	var sessionID string

	isFirstTurn := true

	for {
		var (
			userMsg    string
			turnSource string
		)

		if isFirstTurn {
			// Agent-first protocol: kick the conversation off with the
			// primer as a synthetic user message. Honour ctx and StopCh
			// non-blockingly before sending the kickoff so a pre-signaled
			// stop wins immediately and we don't spawn claude needlessly.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-fsm.ExtendedState.StopCh:
				return ErrStopped
			default:
			}

			userMsg = cfg.primer
			turnSource = "primer"
			isFirstTurn = false
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-fsm.ExtendedState.StopCh:
				return ErrStopped
			case userMsg = <-fsm.ExtendedState.ChatInputCh:
				// If the user typed multiple messages back-to-back
				// while the agent was working, those messages are
				// still buffered on the channel. Drain them and
				// concatenate so the user's full intent reaches the
				// agent as one response — otherwise the second
				// message gets consumed as the answer to whatever the
				// agent prompts next, which is rarely what the user
				// meant.
				userMsg = drainPendingChatInput(userMsg, fsm.ExtendedState.ChatInputCh)
				turnSource = "human"
			}
		}

		nextSessionID, terminated, terminalErr, err := fsm.runOneChatTurn(ctx, cfg, userMsg, sessionID, turnSource)
		if err != nil {
			return err
		}

		sessionID = nextSessionID

		if terminalErr != nil {
			return terminalErr
		}

		if terminated {
			return nil
		}

		// Promotion mid-run: the driver atomically flipped Mode to Autonomous
		// AND buffered the canned promotion chat message into ChatInputCh.
		// The buffered message is the agent's only signal to act on the
		// promotion (write the synthesized Design via update_card, emit the
		// terminal marker per the prompt's "Promotion mid-dialogue" handler).
		// Drain the channel non-blockingly and run ONE more turn so the
		// agent gets that signal. If the channel is empty (the message was
		// dropped or never delivered) or the agent still doesn't terminate
		// on the drain turn, fall back to ErrPromoted so the phase action's
		// autonomous counterpart takes over.
		if fsm.ExtendedState.LoadMode() == ModeAutonomous {
			select {
			case msg := <-fsm.ExtendedState.ChatInputCh:
				msg = drainPendingChatInput(msg, fsm.ExtendedState.ChatInputCh)

				// The drain turn is the loop's last action: we always
				// return after it. The new session_id and any continued
				// usage are therefore discarded here on purpose.
				_, terminated, terminalErr, err = fsm.runOneChatTurn(ctx, cfg, msg, sessionID, "promotion")
				if err != nil {
					return err
				}

				if terminalErr != nil {
					return terminalErr
				}

				if terminated {
					return nil
				}
			default:
			}

			return ErrPromoted
		}

		// The turn ended without a terminal marker — emit a phase-awaiting
		// activity log entry so observers (UI, tests) know the agent has
		// paused and is waiting for the human's next message.
		if fsm.Context.MCP != nil {
			_ = fsm.Context.MCP.AddLog(ctx,
				fsm.ExtendedState.Project,
				fsm.ExtendedState.CardID,
				fsm.ExtendedState.AgentID,
				"phase",
				cfg.phase+"_awaiting",
			)
		}
	}
}

// runOneChatTurn spawns claude with the configured prompt/model/tools,
// sends userMsg, closes stdin, and drains proc.Output() looking for
// terminal markers. Returns the captured session_id (or the prior one
// if the turn produced none), whether onTerminalMarker terminated the
// loop, the terminalErr from the marker callback (if any), and any
// spawn/send error.
func (fsm *ContextMatrixOrchestrator) runOneChatTurn(
	ctx context.Context,
	cfg chatLoopConfig,
	userMsg string,
	sessionID string,
	turnSource string,
) (string, bool, error, error) {
	if fsm.Context.Claude == nil {
		return sessionID, false, nil, errors.New(cfg.phase + ": claude client not configured")
	}

	if fsm.Context.Logger != nil {
		fsm.Context.Logger.Info(cfg.phase+" HITL turn",
			"card_id", fsm.ExtendedState.CardID,
			"session_id", sessionID,
			"source", turnSource,
			"msg_len", len(userMsg),
			"msg_preview", truncate(userMsg, 200))
	}

	env, err := spawnEnv(ctx, fsm.Context)
	if err != nil {
		return sessionID, false, nil, fmt.Errorf("%s: %w", cfg.phase, err)
	}

	proc, err := fsm.Context.Claude.Spawn(ctx, claudeclient.SpawnOptions{
		Container:    fsm.Context.WorkerContainerID,
		SystemPrompt: cfg.systemPrompt,
		Model:        cfg.model,
		AllowedTools: cfg.allowedTools,
		Resume:       sessionID,
		Env:          env,
	})
	if err != nil {
		return sessionID, false, nil, fmt.Errorf("%s: spawn: %w", cfg.phase, err)
	}

	if err := proc.SendMessage(ctx, claudeclient.NewUserMessage(userMsg)); err != nil {
		_ = proc.Kill()

		return sessionID, false, nil, fmt.Errorf("%s: send: %w", cfg.phase, err)
	}

	// Close stdin so claude knows there are no more user frames in
	// this turn and exits cleanly after responding.
	_ = proc.CloseStdin()

	terminated := false

	var terminalErr error

	for ev := range proc.Output() {
		if ev.Kind != claudeclient.EventToolUse {
			continue
		}

		marker, ok := claudeclient.RecognizeFromToolUse(ev)
		if !ok {
			continue
		}

		done, herr := cfg.onTerminalMarker(ctx, marker)
		if herr != nil {
			terminalErr = herr

			break
		}

		if done {
			terminated = true

			break
		}
	}

	// Capture session_id for the next turn before releasing the proc.
	nextSessionID := sessionID
	if id := proc.SessionID(); id != "" {
		nextSessionID = id
	}

	_ = proc.Kill()

	return nextSessionID, terminated, terminalErr, nil
}

// +vectorsigma:action:RunBrainstormingDialogue
//
// Drives a multi-turn brainstorming dialogue with the user via
// ChatInputCh. Terminates when discovery_complete tool_use signals the
// human approved the design — appends the design_summary to the
// activity log and sets the discovery_complete card field. The body's
// `## Design` section is NOT touched: the brainstorm agent has already
// written the canonical multi-section design there via update_card,
// and overwriting it with the one-paragraph summary would destroy the
// spec the plan phase needs. Stop signals on StopCh and ctx
// cancellation capture the appropriate error on ExtendedState.Error.
//
// On promotion to autonomous mid-dialogue (ErrPromoted), stamps
// discovery_complete: true on the card without writing a synthesized
// Design section and returns nil — the FSM advances to Planning, which
// runs the autonomous planner via LoadMode() routing.
func (fsm *ContextMatrixOrchestrator) RunBrainstormingDialogueAction(_ ...string) error {
	logPhase(fsm, "brainstorm")

	ctx := fsm.ExtendedState.RunCtx

	err := fsm.runChatLoop(ctx, chatLoopConfig{
		phase:        "brainstorm",
		systemPrompt: promptBrainstorm,
		primer:       buildBrainstormPriming(fsm.ExtendedState.Card, fsm.ExtendedState.AgentID),
		model:        modelForPhase("brainstorm", fsm.ExtendedState.Card),
		allowedTools: mcpAllowedTools(
			"Read", "Glob", "Grep",
			"Bash(git:*)",
			"mcp__contextmatrix__get_card",
			"mcp__contextmatrix__get_task_context",
			"mcp__contextmatrix__get_project_kb",
			"mcp__contextmatrix__list_projects",
			"mcp__contextmatrix__update_card",
			"mcp__contextmatrix__add_log",
			"mcp__contextmatrix__report_usage",
			"mcp__contextmatrix__discovery_complete",
		),
		onTerminalMarker: func(ctx context.Context, m claudeclient.Marker) (bool, error) {
			if m.Kind != claudeclient.MarkerDiscoveryComplete {
				return false, nil
			}

			summary := m.Fields["design_summary"]
			_ = fsm.Context.MCP.AddLog(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID, "discovery_complete", summary)
			_ = fsm.Context.MCP.UpdateCardField(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, map[string]any{
				"discovery_complete": true,
			})

			return true, nil
		},
	})
	if errors.Is(err, ErrPromoted) {
		if fsm.Context.MCP != nil {
			_ = fsm.Context.MCP.UpdateCardField(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, map[string]any{
				"discovery_complete": true,
			})
		}

		return nil
	}

	if err != nil {
		fsm.ExtendedState.Error = err
	}

	return nil
}

// runEphemeralPhase spawns a single-turn Claude subprocess for a phase,
// sends a priming user message built from the card context, closes stdin
// to signal end-of-input, and drains the stdout stream-json events into
// a concatenated text blob plus end-of-turn usage. The marker parser
// runs against the returned text.
//
// Returns the full text and usage. If Spawn fails, returns the error.
// The caller is responsible for parsing the marker out of the text.
//
// workingDir is fixed to /workspace today but kept as a parameter so
// future phase actions can override it without reshaping the helper.
//
//nolint:unparam // see workingDir comment above.
func (fsm *ContextMatrixOrchestrator) runEphemeralPhase(
	ctx context.Context,
	sysPrompt string,
	model string,
	allowedTools []string,
	workingDir string,
	primingContent string,
) (string, claudeclient.Usage, error) {
	env, err := spawnEnv(ctx, fsm.Context)
	if err != nil {
		return "", claudeclient.Usage{}, err
	}

	proc, err := fsm.Context.Claude.Spawn(ctx, claudeclient.SpawnOptions{
		Container:    fsm.Context.WorkerContainerID,
		SystemPrompt: sysPrompt,
		Model:        model,
		AllowedTools: allowedTools,
		WorkingDir:   workingDir,
		Env:          env,
	})
	if err != nil {
		return "", claudeclient.Usage{}, fmt.Errorf("spawn: %w", err)
	}

	fsm.ExtendedState.ActiveCC = proc

	defer func() { fsm.ExtendedState.ActiveCC = nil }()

	// Send the priming user message and close stdin so Claude knows
	// there are no more turns and can exit cleanly.
	if primingContent != "" {
		if err := proc.SendMessage(ctx, claudeclient.NewUserMessage(primingContent)); err != nil {
			return "", claudeclient.Usage{}, fmt.Errorf("send priming: %w", err)
		}
	}

	if err := proc.CloseStdin(); err != nil {
		// Non-fatal — Claude may still exit cleanly.
		if fsm.Context.Logger != nil {
			fsm.Context.Logger.Debug("close stdin failed", "err", err)
		}
	}

	var (
		allText  strings.Builder
		usage    claudeclient.Usage
		evCounts = map[claudeclient.EventKind]int{}
	)

	for ev := range proc.Output() {
		evCounts[ev.Kind]++

		switch ev.Kind {
		case claudeclient.EventText:
			// Separate text blocks with a newline. Claude emits prose and
			// the trailing structured marker as distinct content blocks
			// when a tool_use intervenes between them; without a separator
			// the line-anchored marker regex fails to match because the
			// marker keyword sits glued to the previous block's last
			// sentence.
			if allText.Len() > 0 {
				allText.WriteByte('\n')
			}

			allText.WriteString(ev.Text)
		case claudeclient.EventThinking:
			if fsm.Context.Logger != nil {
				fsm.Context.Logger.Debug("claude thinking",
					"card", fsm.ExtendedState.CardID,
					"text", ev.Text,
				)
			}
		case claudeclient.EventToolUse:
			if fsm.Context.Logger != nil {
				fsm.Context.Logger.Debug("claude tool_use",
					"card", fsm.ExtendedState.CardID,
					"tool", ev.ToolName,
					"input", string(ev.ToolInput),
				)
			}
		case claudeclient.EventToolResult:
			if fsm.Context.Logger != nil {
				fsm.Context.Logger.Debug("claude tool_result",
					"card", fsm.ExtendedState.CardID,
					"tool_use_id", ev.ToolUseID,
				)
			}
		case claudeclient.EventError:
			if fsm.Context.Logger != nil {
				fsm.Context.Logger.Warn("claude error event",
					"card", fsm.ExtendedState.CardID,
					"text", ev.Text,
				)
			}
		case claudeclient.EventSystemEnd:
			usage = ev.Usage
		}
	}

	if fsm.Context.Logger != nil {
		fsm.Context.Logger.Info("ephemeral phase output",
			"card", fsm.ExtendedState.CardID,
			"text_len", allText.Len(),
			"events", fmt.Sprintf("%v", evCounts),
			"input_tokens", usage.InputTokens,
			"output_tokens", usage.OutputTokens,
			"text_preview", truncate(allText.String(), 1000),
		)
	}

	return allText.String(), usage, nil
}

// mcpAllowedTools is a tiny convenience that just returns its variadic
// args as a slice. Keeping a single named helper means the (often long)
// per-phase allow-lists read consistently and stay easy to grep for.
func mcpAllowedTools(tools ...string) []string {
	return tools
}

// truncate clamps a string to n runes for log output.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "...[truncated]"
}

// runResolveConflict spawns the focused conflict-resolver phase to
// recover from a rebase conflict during subtask integration. The
// rebase is already in progress in the subtask's worktree — the agent
// edits conflicted files, stages them, and runs `git rebase
// --continue`. Returns nil only on a CONFLICT_RESOLVED marker;
// CONFLICT_UNRESOLVED, parse failures, and spawn errors all return
// non-nil so the caller can clean up via AbortRebase and mark the
// subtask blocked.
func (fsm *ContextMatrixOrchestrator) runResolveConflict(ctx context.Context, repoSlug, subtaskID, subtaskTitle, featureBranch string) error {
	if fsm.Context.Claude == nil {
		return errors.New("resolve_conflict: claude wrapper not configured")
	}

	wtPath := "/workspace/" + repoSlug + "/.wt-" + subtaskID

	priming := fmt.Sprintf(
		"Rebase-conflict resolution for subtask `%s` (%q) on repo `%s`.\n\n"+
			"Worktree path: `%s`\n"+
			"Feature branch (rebase target): `%s`\n\n"+
			"The rebase is paused at one or more conflict markers. "+
			"Inspect with `git -C %s status`, resolve the markers, stage, "+
			"and run `git rebase --continue`. Emit CONFLICT_RESOLVED on "+
			"success or CONFLICT_UNRESOLVED if the conflict is genuinely "+
			"irreconcilable.",
		subtaskID, subtaskTitle, repoSlug, wtPath, featureBranch, wtPath,
	)

	text, _, err := fsm.runEphemeralPhase(ctx, promptResolveConflict,
		modelForPhase("execute", fsm.ExtendedState.Card),
		mcpAllowedTools("Read", "Edit", "Glob", "Grep", "Bash(git:*)"),
		"/workspace",
		priming,
	)
	if err != nil {
		return fmt.Errorf("resolve_conflict: spawn: %w", err)
	}

	var payload claudeclient.ConflictResolvedPayload
	if perr := claudeclient.ParseMarker(text, &payload); perr == nil {
		return nil
	}

	var unres claudeclient.ConflictUnresolvedPayload
	if perr := claudeclient.ParseMarker(text, &unres); perr == nil {
		return fmt.Errorf("resolve_conflict: agent reported unresolved: %s", unres.Reason)
	}

	return fmt.Errorf("resolve_conflict: no recognized marker in agent output")
}

// integrateSubtaskRepo replays the subtask's branch onto the parent
// feature branch in one repo. On a clean rebase it fast-forwards the
// feature branch directly. On a rebase conflict it dispatches the
// resolver phase; if the resolver succeeds the FF still happens. If
// the resolver fails (or the rebase failed for any reason other than
// ErrIntegrateConflict) the rebase is aborted and the error is
// returned so the caller can mark the subtask blocked.
func (fsm *ContextMatrixOrchestrator) integrateSubtaskRepo(ctx context.Context, repoSlug, subtaskID, subtaskTitle, featureBranch string) error {
	if fsm.Context.Workspace == nil {
		return nil
	}

	err := fsm.Context.Workspace.RebaseSubtask(ctx, repoSlug, subtaskID, featureBranch)

	switch {
	case err == nil:
		// Clean rebase, fall through to FF.
	case errors.Is(err, workspace.ErrIntegrateConflict):
		if rerr := fsm.runResolveConflict(ctx, repoSlug, subtaskID, subtaskTitle, featureBranch); rerr != nil {
			_ = fsm.Context.Workspace.AbortRebase(ctx, repoSlug, subtaskID)

			return fmt.Errorf("integrate %s on %s: %w", subtaskID, repoSlug, rerr)
		}
		// Resolver completed the rebase via `git rebase --continue`;
		// fall through to FF.
	default:
		// Some non-conflict error (e.g. worktree missing). Abort
		// defensively in case a partial rebase started.
		_ = fsm.Context.Workspace.AbortRebase(ctx, repoSlug, subtaskID)

		return fmt.Errorf("integrate %s on %s: %w", subtaskID, repoSlug, err)
	}

	if ferr := fsm.Context.Workspace.FastForwardFeature(ctx, repoSlug, subtaskID, featureBranch); ferr != nil {
		// FF failed after rebase completed (clean or via resolver). Defensively
		// abort any leftover rebase state in the worktree — AbortRebase is
		// idempotent (workspace/finalize.go) so the call is a safe no-op when
		// there's nothing to abort, and prevents a half-completed integration
		// from confusing the next attempt.
		_ = fsm.Context.Workspace.AbortRebase(ctx, repoSlug, subtaskID)

		return fmt.Errorf("integrate %s on %s: %w", subtaskID, repoSlug, ferr)
	}

	return nil
}

// buildBrainstormPriming returns the agent-first kickoff message for
// the brainstorming phase. The brainstorm system prompt expects the
// kickoff to name the card and paste the body so the agent can read
// it before opening the design dialogue. Returning an empty string
// here results in Claude Code sending an empty user-message frame to
// Anthropic, which the API rejects with
// "cache_control cannot be set for empty text blocks".
//
// agentID must match the agent_id the runner used to claim the card;
// CM rejects MCP mutations from any other agent.
func buildBrainstormPriming(c *Card, agentID string) string {
	if c == nil {
		return "Begin the design discussion for this card. Read the card body, ask clarifying questions one at a time, and propose 2–3 approaches before settling."
	}

	return fmt.Sprintf(
		"Please brainstorm card `%s` with me.\n\n"+
			"Title: %s\n\n"+
			"Description:\n%s\n\n"+
			"Body:\n%s\n\n"+
			"Use agent_id `%s` for every ContextMatrix MCP call (the runner "+
			"already claimed the card with this id; any other id is rejected).",
		c.ID, c.Title, c.Description, c.Body, agentID,
	)
}

// buildPlanPriming returns the user message that primes the plan phase.
// Includes card identity, title, description, and body so the plan model
// has the full context to decompose work and pick repos.
//
// agentID must match the agent_id the runner used to claim the card; CM
// rejects MCP mutations from any other agent.
//
// In ModeAutonomous the closing tells the agent to emit a PLAN_DRAFTED
// text marker at the end of its single-turn run. In ModeHITL the
// system prompt's `## HITL mode (chat-loop)` section governs the
// turn-by-turn protocol; the primer is the agent-first kickoff message
// and should not carry interpretation guidance.
func buildPlanPriming(c *Card, agentID string, mode Mode) string {
	if c == nil {
		return "Begin planning the work for this card."
	}

	verb := "Begin planning work for"
	closing := "\n\nFollow the system prompt's process. Emit PLAN_DRAFTED at the end."

	if mode == ModeHITL {
		verb = "Please plan"
		closing = ""
	}

	return fmt.Sprintf(
		"%s card `%s`.\n\n"+
			"Title: %s\n\n"+
			"Description:\n%s\n\n"+
			"Body:\n%s\n\n"+
			"Use agent_id `%s` for every ContextMatrix MCP call (the runner "+
			"already claimed the card with this id; any other id is rejected).%s",
		verb, c.ID, c.Title, c.Description, c.Body, agentID, closing,
	)
}

// planSummary renders Plan.Subtasks into a numbered list for use as
// context priming in subsequent phases.
func planSummary(p *Plan) string {
	if p == nil {
		return "(no plan)"
	}

	var b strings.Builder

	fmt.Fprintf(&b, "Subtasks (%d):\n", len(p.Subtasks))

	for i, st := range p.Subtasks {
		fmt.Fprintf(&b, "%d. %s\n", i+1, st.Title)
	}

	return b.String()
}

// subtaskResultsSummary renders SubtaskResults into a textual outline so
// the review phase has the prior outcomes to weigh against.
func subtaskResultsSummary(results []ExecuteResult) string {
	if len(results) == 0 {
		return "(no subtask results)"
	}

	var b strings.Builder

	fmt.Fprintf(&b, "Subtask outcomes (%d):\n", len(results))

	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s [%s]: %s\n", i+1, r.SubtaskID, r.Status, r.Summary)
	}

	return b.String()
}

// +vectorsigma:action:RunDiagnosisPhase
//
// Spawns an ephemeral CC subprocess with prompts/diagnose.md, drains
// the output stream, and parses the DIAGNOSIS_COMPLETE marker. The
// diagnosis itself lives in the card body; this action only validates
// the marker arrived. Token usage is reported.
//
// Errors are captured on ExtendedState.Error so the IsError guard
// routes the FSM to HandlingError. The action itself always returns nil.
func (fsm *ContextMatrixOrchestrator) RunDiagnosisPhaseAction(_ ...string) error {
	logPhase(fsm, "diagnose")

	ctx := fsm.ExtendedState.RunCtx
	if fsm.Context.Claude == nil {
		fsm.ExtendedState.Error = errors.New("diagnose: claude wrapper not configured")

		return nil
	}

	priming := buildDiagnosisPriming(fsm.ExtendedState.Card, fsm.ExtendedState.AgentID)

	text, usage, err := fsm.runEphemeralPhase(ctx, promptDiagnose,
		modelForPhase("diagnose", fsm.ExtendedState.Card),
		mcpAllowedTools("Read", "Glob", "Grep", "Skill",
			"Bash(git:*)",
			"mcp__contextmatrix__get_card",
			"mcp__contextmatrix__get_task_context",
			"mcp__contextmatrix__update_card",
			"mcp__contextmatrix__add_log",
			"mcp__contextmatrix__report_usage",
		),
		"/workspace",
		priming,
	)
	if err != nil {
		fsm.ExtendedState.Error = fmt.Errorf("diagnose: %w", err)

		return nil
	}

	var payload claudeclient.DiagnosisCompletePayload
	if err := claudeclient.ParseMarker(text, &payload); err != nil {
		fsm.ExtendedState.Error = fmt.Errorf("diagnose: parse marker: %w", err)

		return nil
	}

	_ = fsm.Context.MCP.ReportUsage(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID,
		usage.InputTokens, usage.OutputTokens, usage.Model)

	return nil
}

// buildDiagnosisPriming returns the user message that primes the
// diagnosis phase with the bug-card identity and body.
func buildDiagnosisPriming(c *Card, agentID string) string {
	if c == nil {
		return "Begin diagnosing this card."
	}

	return fmt.Sprintf(
		"Diagnose card `%s`. Title: %s\n\n%s\n\n"+
			"Use agent_id `%s` for every ContextMatrix MCP call.\n\n"+
			"Follow the system prompt and emit DIAGNOSIS_COMPLETE at the end.",
		c.ID, c.Title, c.Body, agentID,
	)
}

// +vectorsigma:action:RunDocumentPhase
//
// Spawns an ephemeral CC subprocess with prompts/document.md, drains
// the output stream, and parses the DOCS_WRITTEN marker. Populates
// ExtendedState.DocsResult with the list of files written and pushes
// docs_written:true to the card via MCP. Token usage is reported.
//
// Errors are captured on ExtendedState.Error so the IsError guard
// routes the FSM to HandlingError.
func (fsm *ContextMatrixOrchestrator) RunDocumentPhaseAction(_ ...string) error {
	logPhase(fsm, "document")

	ctx := fsm.ExtendedState.RunCtx
	if fsm.Context.Claude == nil {
		fsm.ExtendedState.Error = errors.New("document: claude wrapper not configured")

		return nil
	}

	priming := fmt.Sprintf(
		"Documentation phase for parent card `%s`.\n\nPlan summary:\n%s\n"+
			"What the execute phase built:\n%s\n"+
			"%s"+
			"Use agent_id `%s` for every ContextMatrix MCP call (the orchestrator "+
			"already claimed the parent card with this id; any other id is rejected).\n\n"+
			"Follow the system prompt and emit DOCS_WRITTEN at the end.",
		fsm.ExtendedState.CardID,
		planSummary(fsm.ExtendedState.Plan),
		subtaskResultsSummary(fsm.ExtendedState.SubtaskResults),
		renderActiveSkillsBlock(fsm),
		fsm.ExtendedState.AgentID,
	)

	text, usage, err := fsm.runEphemeralPhase(ctx, promptDocument,
		modelForPhase("document", fsm.ExtendedState.Card),
		mcpAllowedTools("Read", "Glob", "Grep", "Skill", "Write", "Edit",
			"Bash(git:*)",
			"mcp__contextmatrix__get_card",
			"mcp__contextmatrix__get_task_context",
			"mcp__contextmatrix__update_card",
			"mcp__contextmatrix__report_usage",
			"mcp__contextmatrix__add_log",
		),
		"/workspace",
		priming,
	)
	if err != nil {
		fsm.ExtendedState.Error = fmt.Errorf("document: %w", err)

		return nil
	}

	var payload claudeclient.DocsWrittenPayload
	if err := claudeclient.ParseMarker(text, &payload); err != nil {
		fsm.ExtendedState.Error = fmt.Errorf("document: parse marker: %w", err)

		return nil
	}

	fsm.ExtendedState.DocsResult = &DocumentResult{FilesWritten: payload.FilesWritten}

	if fsm.Context.MCP != nil {
		_ = fsm.Context.MCP.UpdateCardField(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, map[string]any{
			"docs_written": true,
		})
		_ = fsm.Context.MCP.ReportUsage(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID,
			usage.InputTokens, usage.OutputTokens, usage.Model)
	}

	return nil
}

// +vectorsigma:action:RunExecutePhaseParallel
//
// HEURISTIC v1: Sequential execution; true parallel fan-out via errgroup
// is a follow-up task. Each subtask gets its own worktree per repo.
//
// For each subtask in ExtendedState.Plan.Subtasks the action:
//  1. Best-effort creates a per-repo worktree on cm/<subtask-id>.
//  2. Spawns an ephemeral CC subprocess with prompts/execute.md.
//  3. Drains the output stream and inspects the trailing marker for
//     TASK_COMPLETE / TASK_BLOCKED / TASK_NEEDS_DECOMPOSITION.
//  4. Records the parsed ExecuteResult on ExtendedState.SubtaskResults.
//
// On TASK_NEEDS_DECOMPOSITION the proposed sub-subtasks are appended to
// ExtendedState.Plan.Subtasks; the FSM then routes
// Executing → CreatingSubtasks → Executing so each new proposal gets a
// CM card ID before this action runs again. On re-entry we keep the
// existing SubtaskResults and skip any subtask whose ID is already
// recorded — only newly-created decomposed subtasks execute on the
// second pass.
func (fsm *ContextMatrixOrchestrator) RunExecutePhaseParallelAction(_ ...string) error {
	logPhase(fsm, "execute")

	ctx := fsm.ExtendedState.RunCtx
	if fsm.ExtendedState.Plan == nil || len(fsm.ExtendedState.Plan.Subtasks) == 0 {
		return nil
	}

	if fsm.Context.Claude == nil {
		fsm.ExtendedState.Error = errors.New("execute: claude wrapper not configured")

		return nil
	}

	// Resolve the parent feature/base branch up front so we can both
	// pre-position each repo's main clone on cm/<parent> (so the doc
	// phase commits land there) and pass the branch into
	// integrateSubtaskRepo after each subtask completes.
	featureBranch := ""
	baseBranch := "main"

	if fsm.ExtendedState.Card != nil {
		featureBranch = fsm.ExtendedState.Card.BranchName
		if fsm.ExtendedState.Card.BaseBranch != "" {
			baseBranch = fsm.ExtendedState.Card.BaseBranch
		}
	}

	if featureBranch == "" {
		featureBranch = "cm/" + fsm.ExtendedState.CardID
	}

	// Set up each unique repo BEFORE the subtask loop: clone (idempotent)
	// then EnsureFeatureBranch so that subsequent integrate steps
	// fast-forward onto the parent's feature branch rather than colliding
	// with a stale main checkout. EnsureFeatureBranch is also idempotent
	// across FSM re-entries (e.g. after CreatingSubtasks for
	// decomposition).
	if fsm.Context.Workspace != nil {
		seen := map[string]struct{}{}

		for _, st := range fsm.ExtendedState.Plan.Subtasks {
			for _, repo := range st.Repos {
				if _, ok := seen[repo]; ok {
					continue
				}

				seen[repo] = struct{}{}

				if err := fsm.Context.Workspace.CloneRepo(ctx, repo); err != nil {
					if fsm.Context.Logger != nil {
						fsm.Context.Logger.Warn("execute: clone failed during feature-branch setup",
							"repo", repo, "err", err)
					}

					continue
				}

				if err := fsm.Context.Workspace.EnsureFeatureBranch(ctx, repo, featureBranch, baseBranch); err != nil {
					if fsm.Context.Logger != nil {
						fsm.Context.Logger.Warn("execute: EnsureFeatureBranch failed",
							"repo", repo, "branch", featureBranch, "base", baseBranch, "err", err)
					}
				}
			}
		}
	}

	// Build the set of subtask IDs that already have a result so the
	// re-entry pass after CreatingSubtasks (decomposition) skips them.
	// On a first-pass entry the set is empty and every subtask runs as
	// before.
	completed := make(map[string]struct{}, len(fsm.ExtendedState.SubtaskResults))
	for _, r := range fsm.ExtendedState.SubtaskResults {
		completed[r.SubtaskID] = struct{}{}
	}

	// Snapshot the slice length at the start so a TASK_NEEDS_DECOMPOSITION
	// result that appends new proposals does NOT cause the same pass to
	// iterate into them. The HasNewSubtasksFromDecomposition guard then
	// routes the FSM through CreatingSubtasks (which populates the new
	// IDs) before this action runs again — which on re-entry sees the
	// new subtasks and runs only those (the original ones are already
	// in `completed`).
	originalLen := len(fsm.ExtendedState.Plan.Subtasks)
	for i := 0; i < originalLen; i++ {
		st := fsm.ExtendedState.Plan.Subtasks[i]

		subtaskID := st.ID
		if subtaskID == "" {
			// CreateSubtaskCardsAction must populate the ID before this
			// action runs. If we hit this path it's a programming error,
			// not an agent error; fail fast rather than fan out an
			// uncoordinated execute call against a phantom card.
			fsm.ExtendedState.Error = fmt.Errorf("execute: subtask %d (%q) has no ID; CreateSubtaskCards must run first", i, st.Title)

			return nil
		}

		if _, done := completed[subtaskID]; done {
			continue
		}

		// Ensure each repo this subtask touches is cloned, then create
		// the per-(subtask, repo) worktree. CloneRepo is idempotent — if
		// the plan agent already cloned via Bash, the existing clone is
		// adopted.
		worktreeFailed := false

		for _, repo := range st.Repos {
			if fsm.Context.Workspace == nil {
				continue
			}

			if err := fsm.Context.Workspace.CloneRepo(ctx, repo); err != nil {
				summary := fmt.Sprintf("clone %s: %v", repo, err)
				if fsm.Context.Logger != nil {
					fsm.Context.Logger.Error("execute: subtask blocked at clone",
						"subtask_id", subtaskID,
						"repo", repo,
						"err", err,
					)
				}

				fsm.ExtendedState.SubtaskResults = append(fsm.ExtendedState.SubtaskResults, ExecuteResult{
					SubtaskID: subtaskID,
					Status:    "blocked",
					Summary:   summary,
				})

				worktreeFailed = true

				break
			}

			if _, err := fsm.Context.Workspace.CreateWorktree(ctx, repo, subtaskID); err != nil {
				summary := fmt.Sprintf("worktree create %s: %v", repo, err)
				if fsm.Context.Logger != nil {
					fsm.Context.Logger.Error("execute: subtask blocked at worktree",
						"subtask_id", subtaskID,
						"repo", repo,
						"err", err,
					)
				}

				fsm.ExtendedState.SubtaskResults = append(fsm.ExtendedState.SubtaskResults, ExecuteResult{
					SubtaskID: subtaskID,
					Status:    "blocked",
					Summary:   summary,
				})

				worktreeFailed = true

				break
			}
		}

		if worktreeFailed {
			continue
		}

		priming := buildExecutePriming(subtaskID, st, fsm.ExtendedState.Plan, fsm.ExtendedState.AgentID, renderActiveSkillsBlock(fsm))

		text, usage, err := fsm.runEphemeralPhase(ctx, promptExecute,
			modelForPhase("execute", fsm.ExtendedState.Card),
			mcpAllowedTools("Read", "Glob", "Grep", "Skill", "Edit", "Write",
				"Bash",
				"mcp__contextmatrix__claim_card",
				"mcp__contextmatrix__get_card",
				"mcp__contextmatrix__get_task_context",
				"mcp__contextmatrix__update_card",
				"mcp__contextmatrix__heartbeat",
				"mcp__contextmatrix__report_usage",
				"mcp__contextmatrix__add_log",
				"mcp__contextmatrix__complete_task",
				"mcp__contextmatrix__transition_card",
			),
			"/workspace",
			priming,
		)
		if err != nil {
			fsm.ExtendedState.SubtaskResults = append(fsm.ExtendedState.SubtaskResults, ExecuteResult{
				SubtaskID: subtaskID,
				Status:    "blocked",
				Summary:   fmt.Sprintf("spawn: %v", err),
			})

			continue
		}

		result := parseExecuteResult(text, subtaskID, usage)
		fsm.ExtendedState.SubtaskResults = append(fsm.ExtendedState.SubtaskResults, result)

		// Successful subtask: rebase the worktree branch onto the
		// parent's feature branch and fast-forward the feature branch
		// onto it, per repo. Failures demote the subtask to blocked so
		// the finalize phase doesn't push a feature branch missing this
		// work.
		if result.Status == "done" {
			for _, repo := range st.Repos {
				if err := fsm.integrateSubtaskRepo(ctx, repo, subtaskID, st.Title, featureBranch); err != nil {
					idx := len(fsm.ExtendedState.SubtaskResults) - 1
					fsm.ExtendedState.SubtaskResults[idx].Status = "blocked"
					fsm.ExtendedState.SubtaskResults[idx].Summary = fmt.Sprintf(
						"integrate failed: %v (executor summary was: %s)",
						err, fsm.ExtendedState.SubtaskResults[idx].Summary,
					)

					if fsm.Context.Logger != nil {
						fsm.Context.Logger.Warn("execute: integrate failed",
							"subtask_id", subtaskID, "repo", repo, "err", err)
					}

					if fsm.Context.MCP != nil {
						_ = fsm.Context.MCP.AddLog(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID,
							fsm.ExtendedState.AgentID, "integrate_failed",
							fmt.Sprintf("subtask %s on %s: %v", subtaskID, repo, err))
					}

					break
				}
			}
		}

		// Decomposition: append the proposed subtasks to Plan.Subtasks
		// for the FSM to pick up via HasNewSubtasksFromDecomposition guard.
		if result.Status == "needs_decomposition" {
			fsm.ExtendedState.Plan.Subtasks = append(fsm.ExtendedState.Plan.Subtasks, result.ProposedSubtasks...)
		}
	}

	// If every subtask result is blocked, the FSM's AllRemainingBlocked
	// guard will route to HandlingError. HandleError only surfaces
	// fsm.ExtendedState.Error — without aggregating the per-subtask
	// summaries here, the operator sees a graceful exit with no clue why.
	// Synthesize an aggregate Error AND mirror each blocked summary onto
	// the parent card's activity log via MCP.AddLog so it shows up in the
	// CM UI.
	if allBlocked(fsm.ExtendedState.SubtaskResults) {
		var b strings.Builder

		fmt.Fprintf(&b, "execute: all %d subtask(s) blocked", len(fsm.ExtendedState.SubtaskResults))

		for _, r := range fsm.ExtendedState.SubtaskResults {
			fmt.Fprintf(&b, "; %s: %s", r.SubtaskID, r.Summary)

			if fsm.Context.MCP != nil {
				_ = fsm.Context.MCP.AddLog(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID,
					"execute_blocked", fmt.Sprintf("subtask %s: %s", r.SubtaskID, r.Summary))
			}
		}

		fsm.ExtendedState.Error = errors.New(b.String())
	}

	return nil
}

// allBlocked reports whether every result in res has Status == "blocked".
// Returns false on an empty slice so a zero-subtask plan doesn't
// accidentally get treated as a blocked execute phase.
func allBlocked(res []ExecuteResult) bool {
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

// buildExecutePriming returns the user message that primes the execute
// phase for a single subtask. Includes the subtask identity + description,
// the parent plan summary, the worktree paths the orchestrator
// pre-created (so the agent doesn't have to discover its working
// directory via `pwd`), and the rendered specialist-skills block (empty
// when no task-skills are mounted for this card).
func buildExecutePriming(subtaskID string, st Subtask, plan *Plan, agentID, skillsBlock string) string {
	worktreeNote := buildWorktreeNote(subtaskID, st.Repos)

	return fmt.Sprintf(
		"Execute subtask `%s`.\n\nTitle: %s\n\nDescription:\n%s\n\n"+
			"Parent plan summary:\n%s\n\n"+
			"%s%s"+
			"Use agent_id `%s` for every ContextMatrix MCP call.\n\n"+
			"Follow the system prompt and emit TASK_COMPLETE, TASK_BLOCKED, "+
			"or TASK_NEEDS_DECOMPOSITION at the end.",
		subtaskID, st.Title, st.Description, planSummary(plan), worktreeNote, skillsBlock, agentID,
	)
}

// buildWorktreeNote renders the per-repo worktree paths the orchestrator
// pre-created for a subtask. Returns an empty string when the subtask
// has no repos (pure-spec subtasks); the caller's format string still
// works because the empty interpolation is a no-op.
func buildWorktreeNote(subtaskID string, repos []string) string {
	if len(repos) == 0 {
		return ""
	}

	if len(repos) == 1 {
		path := fmt.Sprintf("/workspace/%s/.wt-%s", repos[0], subtaskID)

		return fmt.Sprintf(
			"Worktree: `%s` — your code changes belong here. Bash calls do "+
				"not persist `cd`, so use `git -C %s ...` and absolute "+
				"paths rather than `cd <dir> && ...`.\n\n",
			path, path,
		)
	}

	var b strings.Builder

	b.WriteString("Worktrees (one per repo, your code changes belong inside these):\n")

	for _, repo := range repos {
		fmt.Fprintf(&b, "- `/workspace/%s/.wt-%s`\n", repo, subtaskID)
	}

	b.WriteString(
		"\nBash calls do not persist `cd`, so use `git -C <worktree> ...` " +
			"and absolute paths rather than `cd <dir> && ...`.\n\n",
	)

	return b.String()
}

// parseExecuteResult inspects the CC output text for one of three
// terminal markers and returns a populated ExecuteResult. If no marker
// is recognized the result is treated as blocked with a parse-error
// summary so the orchestrator can route on it deterministically.
func parseExecuteResult(text, subtaskID string, usage claudeclient.Usage) ExecuteResult {
	res := ExecuteResult{
		SubtaskID: subtaskID,
		Usage: TokenUsage{
			PromptTokens:     usage.InputTokens,
			CompletionTokens: usage.OutputTokens,
			Model:            usage.Model,
		},
	}

	var done claudeclient.TaskCompletePayload
	if err := claudeclient.ParseMarker(text, &done); err == nil {
		res.Status = "done"
		res.Summary = done.Summary

		return res
	}

	var blocked claudeclient.TaskBlockedPayload
	if err := claudeclient.ParseMarker(text, &blocked); err == nil {
		res.Status = "blocked"
		res.Summary = blocked.Reason
		res.BlockerCards = blocked.BlockerCards
		res.NeedsHuman = blocked.NeedsHuman

		return res
	}

	var decomp claudeclient.TaskNeedsDecompositionPayload
	if err := claudeclient.ParseMarker(text, &decomp); err == nil {
		res.Status = "needs_decomposition"
		for _, title := range decomp.Subtasks {
			res.ProposedSubtasks = append(res.ProposedSubtasks, Subtask{Title: title})
		}

		return res
	}

	res.Status = "blocked"
	res.Summary = "no recognized marker in output"

	return res
}

// +vectorsigma:action:RunPlanPhase
//
// Spawns an ephemeral Claude Code subprocess with prompts/plan.md (or
// prompts/replan.md when Card.RevisionRequested), drains the output stream,
// parses the PLAN_DRAFTED marker including its structured JSON payload, and
// populates ExtendedState.Plan from it. The agent also writes a
// human-readable ## Plan section to the card body for HITL reviewers and
// replan rounds.
//
// Errors are captured on ExtendedState.Error so the IsError guard routes
// the FSM to HandlingError. The action itself always returns nil.
//
// In HITL mode the action runs a session-based chat-loop instead: the user
// drives the plan dialogue interactively, and the agent emits a plan_complete
// tool call when both sides agree on the final plan. The structured
// subtasks payload from that tool call drives ExtendedState.Plan; the agent
// is also responsible for writing the human-readable ## Plan section to the
// card body before signaling completion.
func (fsm *ContextMatrixOrchestrator) RunPlanPhaseAction(_ ...string) error {
	logPhase(fsm, "plan")

	if fsm.ExtendedState.LoadMode() == ModeHITL {
		err := fsm.runPlanPhaseHITL()
		if errors.Is(err, ErrPromoted) {
			// Mid-plan promotion: mode is now Autonomous. Run the
			// autonomous planner so the FSM lands with a real Plan
			// instead of advancing with a stale HITL chat-loop state.
			return fsm.runPlanPhaseAutonomous()
		}

		if err != nil {
			fsm.ExtendedState.Error = err
		}

		return nil
	}

	return fsm.runPlanPhaseAutonomous()
}

func (fsm *ContextMatrixOrchestrator) runPlanPhaseAutonomous() error {
	ctx := fsm.ExtendedState.RunCtx
	if fsm.Context.Claude == nil {
		fsm.ExtendedState.Error = errors.New("plan: claude wrapper not configured")

		return nil
	}

	sysPrompt := promptPlan
	if fsm.ExtendedState.Card != nil && fsm.ExtendedState.Card.RevisionRequested {
		sysPrompt = promptReplan
	}

	priming := buildPlanPriming(fsm.ExtendedState.Card, fsm.ExtendedState.AgentID, ModeAutonomous)

	text, usage, err := fsm.runEphemeralPhase(ctx, sysPrompt,
		modelForPhase("plan", fsm.ExtendedState.Card),
		mcpAllowedTools("Read", "Glob", "Grep",
			"Bash(git:*)",
			"mcp__contextmatrix__get_card",
			"mcp__contextmatrix__get_task_context",
			"mcp__contextmatrix__get_project_kb",
			"mcp__contextmatrix__list_projects",
			"mcp__contextmatrix__update_card",
		),
		"/workspace",
		priming,
	)
	if err != nil {
		fsm.ExtendedState.Error = fmt.Errorf("plan: %w", err)

		return nil
	}

	// Parse PLAN_DRAFTED marker (with structured JSON payload).
	var payload claudeclient.PlanDraftedPayload

	markerErr := claudeclient.ParseMarker(text, &payload)

	// Refresh the card so downstream actions see what the agent wrote.
	// We also need the body for the marker-fallback path below: real
	// Claude occasionally stops after calling `update_card` with the
	// plan body (containing `## Plan` + fenced ```json) but never emits
	// the PLAN_DRAFTED text marker. The body IS the canonical spec —
	// the HITL plan path already treats it as source of truth via
	// applyPlanCompletePayload — so the autonomous path falls back to
	// the same body-extract here rather than failing the FSM.
	if fsm.Context.MCP != nil {
		cc, err := fsm.Context.MCP.GetTaskContext(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID)
		if err != nil {
			fsm.ExtendedState.Error = fmt.Errorf("plan: get card: %w", err)

			return nil
		}

		fsm.ExtendedState.Card = cc.Card

		if markerErr != nil && cc.Card != nil {
			if perr := populatePlanFromBody(cc.Card.Body, &payload); perr == nil {
				if fsm.Context.Logger != nil {
					fsm.Context.Logger.Info("plan: recovered missing PLAN_DRAFTED marker via card body",
						"card_id", fsm.ExtendedState.CardID,
						"marker_err", markerErr.Error(),
					)
				}

				markerErr = nil
			}
		}

		// Clear revision flag if it was set on the refreshed card.
		if cc.Card != nil && cc.Card.RevisionRequested {
			_ = fsm.Context.MCP.UpdateCardField(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, map[string]any{
				"revision_requested": false,
			})
		}

		_ = fsm.Context.MCP.ReportUsage(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID,
			usage.InputTokens, usage.OutputTokens, usage.Model)
	}

	if markerErr != nil {
		fsm.ExtendedState.Error = fmt.Errorf("plan: parse marker: %w", markerErr)

		return nil
	}

	fsm.ExtendedState.Plan = planFromPayload(payload)
	applyRegisteredRepoFallback(fsm)

	return nil
}

// populatePlanFromBody extracts the canonical plan from the card body's
// `## Plan` section (the fenced ```json block). Used by the autonomous
// plan phase as a fallback when the agent wrote `update_card` with the
// plan body but stopped without emitting the PLAN_DRAFTED text marker
// — a real-Claude flakiness mode. Mirrors the HITL plan path's
// applyPlanCompletePayload contract: the body IS the spec; the text
// marker / tool call is just the terminal signal.
func populatePlanFromBody(body string, dst *claudeclient.PlanDraftedPayload) error {
	extracted, err := claudeclient.ExtractPlanJSON(body)
	if err != nil {
		return fmt.Errorf("extract plan json: %w", err)
	}

	if len(extracted) == 0 {
		return errors.New("no `## Plan` section with fenced json block in body")
	}

	var jp struct {
		CardID      string                           `json:"card_id"`
		PlanSummary string                           `json:"plan_summary"`
		ChosenRepos claudeclient.FlexibleStringSlice `json:"chosen_repos"`
		Subtasks    claudeclient.FlexibleSubtaskList `json:"subtasks"`
	}

	if err := json.Unmarshal(extracted, &jp); err != nil {
		return fmt.Errorf("parse plan json: %w (raw=%s)", err, string(extracted))
	}

	dst.CardID = jp.CardID
	dst.PlanSummary = jp.PlanSummary
	dst.ChosenRepos = []string(jp.ChosenRepos)
	dst.Subtasks = []claudeclient.SubtaskSpec(jp.Subtasks)
	dst.SubtaskCount = len(jp.Subtasks)
	dst.Status = "drafted"

	return nil
}

// runPlanPhaseHITL drives the planning chat-loop. claude --print is
// one-shot, so each user turn spawns a fresh process with --resume to
// continue the conversation. The loop terminates when plan_complete
// tool_use signals the human approved the final plan.
func (fsm *ContextMatrixOrchestrator) runPlanPhaseHITL() error {
	ctx := fsm.ExtendedState.RunCtx

	sysPrompt := promptPlan
	if fsm.ExtendedState.Card != nil && fsm.ExtendedState.Card.RevisionRequested {
		sysPrompt = promptReplan
	}

	return fsm.runChatLoop(ctx, chatLoopConfig{
		phase:        "plan",
		systemPrompt: sysPrompt,
		primer:       buildPlanPriming(fsm.ExtendedState.Card, fsm.ExtendedState.AgentID, ModeHITL),
		model:        modelForPhase("plan", fsm.ExtendedState.Card),
		allowedTools: mcpAllowedTools("Read", "Glob", "Grep",
			"Bash(git:*)",
			"mcp__contextmatrix__get_card",
			"mcp__contextmatrix__get_task_context",
			"mcp__contextmatrix__get_project_kb",
			"mcp__contextmatrix__list_projects",
			"mcp__contextmatrix__update_card",
			"mcp__contextmatrix__plan_complete",
		),
		onTerminalMarker: func(ctx context.Context, m claudeclient.Marker) (bool, error) {
			if m.Kind != claudeclient.MarkerPlanComplete {
				return false, nil
			}

			if err := fsm.applyPlanCompletePayload(ctx, m); err != nil {
				return false, err
			}

			return true, nil
		},
	})
}

// applyPlanCompletePayload reads the canonical plan from the card body
// (the agent has just written `## Plan` with a fenced JSON block via
// update_card) and populates ExtendedState.Plan from it. The plan_complete
// tool input is now a thin signal (just card_id); structured data lives
// in the card body. Tool input is consulted only as a fallback for stubs
// and transitional cases — the FlexibleStringSlice / FlexibleSubtaskList
// decoders apply on both paths so LLM shape drift never crashes the FSM.
func (fsm *ContextMatrixOrchestrator) applyPlanCompletePayload(ctx context.Context, marker claudeclient.Marker) error {
	var planJSON json.RawMessage

	source := "card_body"

	if fsm.Context.MCP != nil {
		cc, err := fsm.Context.MCP.GetTaskContext(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID)
		if err == nil && cc != nil && cc.Card != nil {
			fsm.ExtendedState.Card = cc.Card

			extracted, exerr := claudeclient.ExtractPlanJSON(cc.Card.Body)
			if exerr != nil {
				if fsm.Context.Logger != nil {
					fsm.Context.Logger.Warn("plan_complete card-body extract failed",
						"card_id", fsm.ExtendedState.CardID,
						"error", exerr.Error(),
					)
				}

				return fmt.Errorf("plan: extract plan json from card body: %w", exerr)
			}

			planJSON = extracted

			if cc.Card.RevisionRequested {
				_ = fsm.Context.MCP.UpdateCardField(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, map[string]any{
					"revision_requested": false,
				})
			}
		}
	}

	if len(planJSON) == 0 {
		if marker.Raw == "" {
			return fmt.Errorf("plan: no plan json in card body's `## Plan` section and no tool input fallback — agent must update_card the body with a fenced ```json block")
		}

		planJSON = json.RawMessage(marker.Raw)
		source = "tool_input"
	}

	var payload struct {
		PlanSummary string                           `json:"plan_summary"`
		ChosenRepos claudeclient.FlexibleStringSlice `json:"chosen_repos"`
		Subtasks    claudeclient.FlexibleSubtaskList `json:"subtasks"`
	}

	if err := json.Unmarshal(planJSON, &payload); err != nil {
		if fsm.Context.Logger != nil {
			fsm.Context.Logger.Warn("plan_complete parse failed",
				"card_id", fsm.ExtendedState.CardID,
				"source", source,
				"error", err.Error(),
				"raw_payload", string(planJSON),
			)
		}

		return fmt.Errorf("plan: parse plan json (source=%s): %w (raw=%s)", source, err, string(planJSON))
	}

	chosenRepos := []string(payload.ChosenRepos)
	plan := &Plan{
		Summary:     payload.PlanSummary,
		ChosenRepos: chosenRepos,
	}

	for _, st := range payload.Subtasks {
		repos := st.Repos
		if len(repos) == 0 && len(chosenRepos) > 0 {
			repos = append([]string(nil), chosenRepos...)
		}

		plan.Subtasks = append(plan.Subtasks, Subtask{
			Title:       st.Title,
			Description: st.Description,
			Repos:       repos,
			Priority:    st.Priority,
			DependsOn:   st.DependsOn,
		})
	}

	fsm.ExtendedState.Plan = plan
	applyRegisteredRepoFallback(fsm)

	return nil
}

// applyRegisteredRepoFallback covers the case where the agent emits an
// empty chosen_repos list (the plan prompt allows this for "pure-spec
// cards") AND the subtasks therefore come back with empty Repos too. If
// the worker has exactly one registered repo, default both Plan.ChosenRepos
// and every empty subtask.Repos to that single slug — the only sensible
// choice when there's no ambiguity. This is the second layer of the
// repo-defaulting cascade; planFromPayload already covers the case where
// chosen_repos is non-empty but a subtask's repos are not.
func applyRegisteredRepoFallback(fsm *ContextMatrixOrchestrator) {
	if fsm.ExtendedState.Plan == nil || fsm.Context.Workspace == nil {
		return
	}

	if len(fsm.ExtendedState.Plan.ChosenRepos) > 0 {
		return
	}

	registered := fsm.Context.Workspace.RegisteredSlugs()
	if len(registered) != 1 {
		return
	}

	defaults := append([]string(nil), registered...)
	fsm.ExtendedState.Plan.ChosenRepos = append([]string(nil), defaults...)

	for i := range fsm.ExtendedState.Plan.Subtasks {
		if len(fsm.ExtendedState.Plan.Subtasks[i].Repos) == 0 {
			fsm.ExtendedState.Plan.Subtasks[i].Repos = append([]string(nil), defaults...)
		}
	}
}

// planFromPayload converts the typed PlanDraftedPayload from claudeclient into
// the orchestrator's Plan type. Subtask IDs are left empty here;
// CreateSubtaskCardsAction populates them after MCP.CreateCard returns.
//
// Subtask repo fallback: when the agent leaves subtasks[].repos empty but
// chosen_repos is populated, copy chosen_repos into the subtask's Repos so
// the execute phase has slugs to clone and worktree against. The plan
// prompt requires a non-empty repos list whenever chosen_repos is
// non-empty (see prompts/plan.md), but this fallback is defence in depth
// against prompt drift — without it the execute loop iterates an empty
// slice and skips CloneRepo/CreateWorktree entirely, leaving the
// subagent to spawn into an empty /workspace.
func planFromPayload(p claudeclient.PlanDraftedPayload) *Plan {
	plan := &Plan{
		Summary:     p.PlanSummary,
		ChosenRepos: p.ChosenRepos,
	}

	for _, st := range p.Subtasks {
		repos := st.Repos
		if len(repos) == 0 && len(p.ChosenRepos) > 0 {
			// Defensive copy: prevent later mutation of one subtask's
			// Repos from leaking into siblings via shared backing array.
			repos = append([]string(nil), p.ChosenRepos...)
		}

		plan.Subtasks = append(plan.Subtasks, Subtask{
			Title:       st.Title,
			Description: st.Description,
			Repos:       repos,
			Priority:    st.Priority,
			DependsOn:   st.DependsOn,
		})
	}

	return plan
}

// modelForPhase returns the configured model for a given phase. v1 uses
// hardcoded mappings; future versions read from runner config.
//
// Opus runs the decision-heavy phases — brainstorm (where the design
// is invented from a fuzzy user prompt and compounds downstream),
// plan / replan (structured decomposition), diagnose (bug
// investigation), and review (critical evaluation). Sonnet runs the
// volume phases — execute fan-out per subtask, document — where good-
// enough output at lower cost is the right tradeoff.
func modelForPhase(phase string, _ *Card) string {
	switch phase {
	case "brainstorm", "plan", "diagnose", "review":
		return "claude-opus-4-7"
	default:
		return "claude-sonnet-4-6"
	}
}

// +vectorsigma:action:RunReplanPhase
//
// Delegates to RunPlanPhaseAction. The plan action already chooses the
// replan prompt when Card.RevisionRequested is set, so the FSM-level
// distinction is purely a state-machine routing concern.
func (fsm *ContextMatrixOrchestrator) RunReplanPhaseAction(_ ...string) error {
	logPhase(fsm, "replan")

	return fsm.RunPlanPhaseAction()
}

// +vectorsigma:action:RunReviewPhase
//
// Spawns an ephemeral CC subprocess with prompts/review.md, drains the
// output stream, and parses the REVIEW_FINDINGS marker. Populates
// ExtendedState.ReviewResult with the recommendation and summary so
// downstream guards can route on approve / approve_with_notes / revise.
// Token usage is reported.
//
// Errors are captured on ExtendedState.Error so the IsError guard
// routes the FSM to HandlingError.
//
// In HITL mode the action drives a session-based chat-loop instead: the
// reviewer (human) and the agent converse, and the agent emits
// review_approve or review_revise tool_use to terminate the loop.
func (fsm *ContextMatrixOrchestrator) RunReviewPhaseAction(_ ...string) error {
	logPhase(fsm, "review")

	ctx := fsm.ExtendedState.RunCtx

	// Transition the parent card to "review" before spawning either path.
	// This mirrors the legacy create-plan workflow's start_review step: the
	// card visibly enters the review state for any UI watchers, and the
	// orchestrator's later transition to "done" goes through the expected
	// in_progress → review → done path.
	if fsm.Context.MCP != nil {
		if err := fsm.Context.MCP.TransitionCard(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID, "review"); err != nil {
			if fsm.Context.Logger != nil {
				fsm.Context.Logger.Warn("review: transition to review failed (continuing)",
					"card_id", fsm.ExtendedState.CardID,
					"err", err,
				)
			}
		}
	}

	if fsm.ExtendedState.LoadMode() == ModeHITL {
		err := fsm.runReviewPhaseHITL()
		if errors.Is(err, ErrPromoted) {
			// Mid-review promotion: mode is now Autonomous. Run the
			// autonomous reviewer so the FSM lands with a real
			// ReviewResult instead of advancing with a stale chat-loop
			// state.
			return fsm.runReviewPhaseAutonomous()
		}

		if err != nil {
			fsm.ExtendedState.Error = err
		}

		return nil
	}

	return fsm.runReviewPhaseAutonomous()
}

func (fsm *ContextMatrixOrchestrator) runReviewPhaseAutonomous() error {
	ctx := fsm.ExtendedState.RunCtx
	if fsm.Context.Claude == nil {
		fsm.ExtendedState.Error = errors.New("review: claude wrapper not configured")

		return nil
	}

	priming := fmt.Sprintf(
		"Review phase for parent card `%s`.\n\nPlan summary:\n%s\n\nSubtask outcomes:\n%s\n\n"+
			"%s"+
			"Use agent_id `%s` for every ContextMatrix MCP call (the orchestrator "+
			"already claimed the parent card with this id; any other id is rejected).\n\n"+
			"Follow the system prompt and emit REVIEW_FINDINGS at the end.",
		fsm.ExtendedState.CardID,
		planSummary(fsm.ExtendedState.Plan),
		subtaskResultsSummary(fsm.ExtendedState.SubtaskResults),
		renderActiveSkillsBlock(fsm),
		fsm.ExtendedState.AgentID,
	)

	text, usage, err := fsm.runEphemeralPhase(ctx, promptReview,
		modelForPhase("review", fsm.ExtendedState.Card),
		mcpAllowedTools("Read", "Glob", "Grep", "Skill",
			"Bash",
			"mcp__contextmatrix__get_card",
			"mcp__contextmatrix__get_task_context",
			"mcp__contextmatrix__update_card",
			"mcp__contextmatrix__report_usage",
			"mcp__contextmatrix__add_log",
		),
		"/workspace",
		priming,
	)
	if err != nil {
		fsm.ExtendedState.Error = fmt.Errorf("review: %w", err)

		return nil
	}

	var payload claudeclient.ReviewFindingsPayload
	if err := claudeclient.ParseMarker(text, &payload); err != nil {
		fsm.ExtendedState.Error = fmt.Errorf("review: parse marker: %w", err)

		return nil
	}

	fsm.ExtendedState.ReviewResult = &ReviewResult{
		Recommendation: payload.Recommendation,
		Summary:        payload.Summary,
	}

	if fsm.Context.MCP != nil {
		_ = fsm.Context.MCP.ReportUsage(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID,
			usage.InputTokens, usage.OutputTokens, usage.Model)
	}

	return nil
}

// runReviewPhaseHITL runs the review chat-loop via runChatLoop. The
// agent emits review_approve or review_revise to terminate; the
// structured fields drive ExtendedState.ReviewResult.Recommendation and
// Summary, with review_revise also carrying detailed Feedback that
// primes the next replan round.
func (fsm *ContextMatrixOrchestrator) runReviewPhaseHITL() error {
	ctx := fsm.ExtendedState.RunCtx

	primer := fmt.Sprintf(
		"Please review parent card `%s`.\n\nPlan summary:\n%s\n\n"+
			"Subtask outcomes:\n%s\n\n"+
			"%s"+
			"Use agent_id `%s` for every ContextMatrix MCP call.",
		fsm.ExtendedState.CardID,
		planSummary(fsm.ExtendedState.Plan),
		subtaskResultsSummary(fsm.ExtendedState.SubtaskResults),
		renderActiveSkillsBlock(fsm),
		fsm.ExtendedState.AgentID,
	)

	return fsm.runChatLoop(ctx, chatLoopConfig{
		phase:        "review",
		systemPrompt: promptReview,
		primer:       primer,
		model:        modelForPhase("review", fsm.ExtendedState.Card),
		allowedTools: mcpAllowedTools("Read", "Glob", "Grep", "Skill",
			"Bash",
			"mcp__contextmatrix__get_card",
			"mcp__contextmatrix__get_task_context",
			"mcp__contextmatrix__update_card",
			"mcp__contextmatrix__report_usage",
			"mcp__contextmatrix__add_log",
			"mcp__contextmatrix__review_approve",
			"mcp__contextmatrix__review_revise",
		),
		onTerminalMarker: func(_ context.Context, m claudeclient.Marker) (bool, error) {
			switch m.Kind {
			case claudeclient.MarkerReviewApprove:
				fsm.ExtendedState.ReviewResult = &ReviewResult{
					Recommendation: "approve",
					Summary:        m.Fields["summary"],
				}

				return true, nil
			case claudeclient.MarkerReviewRevise:
				fsm.ExtendedState.ReviewResult = &ReviewResult{
					Recommendation: "revise",
					Summary:        m.Fields["summary"],
					Feedback:       m.Fields["feedback"],
				}

				return true, nil
			}

			return false, nil
		},
	})
}

// +vectorsigma:action:TransitionCardToDone
//
// Walks the parent card to "done", releases the claim, and records a
// completion log entry. Does NOT use MCP.CompleteTask — CM's
// complete_task tool transitions main tasks to "review" (not "done")
// because that tool is built for the legacy human-review workflow.
// The orchestrator already moved the card to "review" at the start of
// the Reviewing phase, so we do the final "review → done" hop
// ourselves and release.
func (fsm *ContextMatrixOrchestrator) TransitionCardToDoneAction(_ ...string) error {
	ctx := fsm.ExtendedState.RunCtx
	if fsm.Context.MCP == nil {
		fsm.ExtendedState.Error = errors.New("transition done: mcp client not configured")

		return nil
	}

	summary := "completed"
	if fsm.ExtendedState.ReviewResult != nil && fsm.ExtendedState.ReviewResult.Summary != "" {
		summary = fsm.ExtendedState.ReviewResult.Summary
	}

	// Best-effort completion log entry first; if this fails we still try
	// to transition + release so the card doesn't get stuck.
	_ = fsm.Context.MCP.AddLog(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID, "completed", summary)

	if err := fsm.Context.MCP.TransitionCard(ctx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID, "done"); err != nil {
		fsm.ExtendedState.Error = fmt.Errorf("transition card to done: %w", err)

		return nil
	}

	// CM emits a "stop" SSE event when a card transitions to a terminal
	// state, which the runner-side dispatcher routes to RunCancel. By
	// the time we reach ReleaseCard, the run ctx is therefore likely
	// already cancelled — and the claim would silently linger on the
	// card. Use a fresh background context so the release survives the
	// self-cancellation race.
	releaseCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := fsm.Context.MCP.ReleaseCard(releaseCtx, fsm.ExtendedState.Project, fsm.ExtendedState.CardID, fsm.ExtendedState.AgentID); err != nil {
		if fsm.Context.Logger != nil {
			fsm.Context.Logger.Warn("transition done: release failed (card already done)",
				"card_id", fsm.ExtendedState.CardID,
				"err", err,
			)
		}
	}

	return nil
}
