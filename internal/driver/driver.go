// Package driver owns the per-card lifecycle: it spawns a worker
// container, constructs the orchestrator FSM, dispatches inbound SSE
// events into the FSM's gate channels, and runs the FSM to terminal.
package driver

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	githubauth "github.com/mhersson/contextmatrix-githubauth"
	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
	"github.com/mhersson/contextmatrix-runner/internal/events"
	"github.com/mhersson/contextmatrix-runner/internal/orchestrator"
	"github.com/mhersson/contextmatrix-runner/internal/spawn"
	"github.com/mhersson/contextmatrix-runner/internal/workspace"
)

// SSESubscriber abstracts the runner-side SSE client for testability.
// In production this is satisfied by *events.SSEClient; tests inject a
// controllable fake.
type SSESubscriber interface {
	Subscribe(ctx context.Context) (<-chan events.RunnerEvent, <-chan error)
}

// Config holds the dependencies needed to drive one card.
type Config struct {
	Project string
	CardID  string
	AgentID string
	Mode    orchestrator.Mode

	Spawner    spawn.Spawner
	WorkerSpec spawn.WorkerSpec

	Claude    *claudeclient.Wrapper
	Workspace *workspace.Manager
	MCP       orchestrator.MCPClient
	GitTokens githubauth.TokenGenerator
	Notifier  orchestrator.Notifier

	// ClaudeAuthEnv is the static auth env injected on every Claude
	// docker-exec (CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY). Empty
	// when claude_auth_dir is configured (auth comes from the bind-
	// mounted on-disk credentials file instead).
	ClaudeAuthEnv map[string]string

	// SkillIndex is the full task-skill catalog loaded from
	// runner cfg.TaskSkillsDir; nil when task-skills are disabled.
	SkillIndex []orchestrator.SkillInfo

	// TaskSkills is the per-card subset selector mirroring the webhook
	// payload field of the same name. Priming builders apply it via
	// FilterSkills.
	TaskSkills *[]string

	// BuildDepsFromWorker, if set, is invoked AFTER the worker is spawned
	// and BEFORE the FSM is constructed. The returned deps overwrite the
	// matching cfg fields. Used by callers that need a worker reference
	// to build claudeclient/sessions/workspace.
	BuildDepsFromWorker func(spawn.Worker) (DepsFromWorker, error)

	SSE               SSESubscriber
	Logger            *slog.Logger
	HeartbeatInterval time.Duration
}

// DepsFromWorker bundles the worker-dependent dependencies returned by
// Config.BuildDepsFromWorker. Non-nil fields overwrite the corresponding
// Config fields before the FSM is constructed.
type DepsFromWorker struct {
	Claude    *claudeclient.Wrapper
	Workspace *workspace.Manager
}

// Driver owns one per-card run.
type Driver struct {
	cfg Config
}

// New constructs a Driver.
func New(cfg Config) *Driver {
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 5 * time.Minute
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return &Driver{cfg: cfg}
}

// Drive runs the full per-card lifecycle. Returns when the FSM reaches
// a terminal state or parent ctx is cancelled.
func (d *Driver) Drive(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// 1. Spawn worker container (skipped when no Spawner is configured —
	// useful for unit tests that exercise the dispatcher only).
	var (
		workerID string
		worker   spawn.Worker
	)

	if d.cfg.Spawner != nil {
		w, err := d.cfg.Spawner.Spawn(ctx, d.cfg.WorkerSpec)
		if err != nil {
			return err
		}

		worker = w
		workerID = worker.ID()

		defer func() {
			_ = worker.Remove(context.Background())
		}()
	}

	// 1a. Build worker-dependent deps (claudeclient/sessions/workspace).
	// These need the spawn.Worker in hand, so callers thread them in via
	// the BuildDepsFromWorker hook rather than directly populating cfg.
	if d.cfg.BuildDepsFromWorker != nil && worker != nil {
		deps, err := d.cfg.BuildDepsFromWorker(worker)
		if err != nil {
			return fmt.Errorf("build deps from worker: %w", err)
		}

		if deps.Claude != nil {
			d.cfg.Claude = deps.Claude
		}

		if deps.Workspace != nil {
			d.cfg.Workspace = deps.Workspace
		}
	}

	// 2. Construct FSM and populate Context + ExtendedState in place.
	sm := orchestrator.New()
	sm.Context.Logger = d.cfg.Logger
	sm.Context.MCP = d.cfg.MCP
	sm.Context.Claude = d.cfg.Claude
	sm.Context.Workspace = d.cfg.Workspace
	sm.Context.GitTokens = d.cfg.GitTokens
	sm.Context.Notifier = d.cfg.Notifier
	sm.Context.ClaudeAuthEnv = d.cfg.ClaudeAuthEnv
	sm.Context.SkillIndex = d.cfg.SkillIndex
	sm.Context.WorkerContainerID = workerID

	sm.ExtendedState.RunCtx = ctx
	sm.ExtendedState.RunCancel = cancel
	sm.ExtendedState.Project = d.cfg.Project
	sm.ExtendedState.CardID = d.cfg.CardID
	sm.ExtendedState.AgentID = d.cfg.AgentID
	sm.ExtendedState.TaskSkills = d.cfg.TaskSkills
	sm.ExtendedState.StoreMode(d.cfg.Mode)

	// 3. Open SSE subscription and dispatch events into ExtendedState
	// channels. The InitializeAction creates the channels lazily on
	// first run, but the dispatcher tolerates nil channels via the
	// non-blocking select-default below.
	if d.cfg.SSE != nil {
		sseEvents, sseErrs := d.cfg.SSE.Subscribe(ctx)
		go d.dispatchEvents(ctx, sseEvents, sseErrs, sm.ExtendedState)
	}

	// 4. Start heartbeat goroutine.
	if d.cfg.MCP != nil {
		go d.heartbeat(ctx)
	}

	// 5. Run FSM. Vectorsigma's Run blocks until terminal state.
	return sm.Run()
}

// dispatchEvents routes SSE events into the right ExtendedState channels.
// Exits when ctx is cancelled or the event channel is closed.
func (d *Driver) dispatchEvents(
	ctx context.Context,
	ev <-chan events.RunnerEvent,
	errs <-chan error,
	ext *orchestrator.ExtendedState,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ev:
			if !ok {
				return
			}

			d.dispatch(ctx, e, ext)
		case err, ok := <-errs:
			if !ok {
				continue
			}

			d.cfg.Logger.Warn("sse error", "err", err)
		}
	}
}

// dispatch routes a single inbound RunnerEvent into the matching
// ExtendedState channel. The FSM owns its consumption; the driver
// only fans events in.
//
// promotion is special: flip the FSM mode atomically so any active phase
// guard re-reads as autonomous on the next call, then push a canned
// "you are autonomous now" chat into the active phase agent so a long-
// lived chat session can finish on its own.
func (d *Driver) dispatch(_ context.Context, ev events.RunnerEvent, ext *orchestrator.ExtendedState) {
	switch ev.Type {
	case "chat_input":
		sendNonBlocking(ext.ChatInputCh, ev.Data)
	case "promotion":
		ext.StoreMode(orchestrator.ModeAutonomous)
		sendNonBlocking(ext.ChatInputCh, promotionChatMessage)
	case "stop":
		sendNonBlocking(ext.StopCh, struct{}{})

		if ext.RunCancel != nil {
			ext.RunCancel()
		}
	}
}

// promotionChatMessage is sent into the active phase's chat session when
// the user promotes to autonomous mid-run. The agent reads this and
// finishes on its own by emitting the terminal-marker tool registered
// in the current phase's allowed-tools list — different phases expose
// different markers, so the message names all four and lets the agent
// pick whichever is available. The orchestrator's runChatLoop also
// returns ErrPromoted at end-of-turn if the agent did not emit any
// marker, so a misbehaving model cannot hang the loop.
const promotionChatMessage = "The user has just promoted this card to autonomous mode. " +
	"Finish the current phase on your own — synthesize the best decision based on the " +
	"conversation so far and emit the terminal-marker tool registered in your allowed " +
	"tools (discovery_complete for brainstorming, plan_complete for planning, " +
	"review_approve or review_revise for review). Do not wait for further user input."

// sendNonBlocking attempts a single non-blocking send on ch. If ch is
// nil or full, the value is dropped — the FSM action that owns the
// channel is responsible for treating absence as a wait, not a signal.
func sendNonBlocking[T any](ch chan T, v T) {
	if ch == nil {
		return
	}

	select {
	case ch <- v:
	default:
	}
}

// heartbeat fires MCP.Heartbeat every cfg.HeartbeatInterval until ctx
// is cancelled.
func (d *Driver) heartbeat(ctx context.Context) {
	t := time.NewTicker(d.cfg.HeartbeatInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = d.cfg.MCP.Heartbeat(ctx, d.cfg.Project, d.cfg.CardID, d.cfg.AgentID)
		}
	}
}
