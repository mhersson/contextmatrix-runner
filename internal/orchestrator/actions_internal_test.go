package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
	"github.com/mhersson/contextmatrix-runner/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDrainPendingChatInput verifies the helper concatenates the first
// message with any messages that were buffered on ChatInputCh before the
// chat loop was ready to read. This preserves user intent when the human
// types multiple messages in succession before the agent has prompted.
func TestDrainPendingChatInput(t *testing.T) {
	t.Run("returns first unchanged when channel is empty", func(t *testing.T) {
		ch := make(chan string, 4)
		got := drainPendingChatInput("approve", ch)
		assert.Equal(t, "approve", got)
	})

	t.Run("appends a single queued message", func(t *testing.T) {
		ch := make(chan string, 4)
		ch <- "and change x to y"

		got := drainPendingChatInput("GO", ch)
		assert.Equal(t, "GO\n\nand change x to y", got)
	})

	t.Run("appends multiple queued messages preserving order", func(t *testing.T) {
		ch := make(chan string, 4)
		ch <- "first follow-up"

		ch <- "second follow-up"

		got := drainPendingChatInput("GO", ch)
		assert.Equal(t, "GO\n\nfirst follow-up\n\nsecond follow-up", got)
	})

	t.Run("nil channel is treated as no buffered messages", func(t *testing.T) {
		got := drainPendingChatInput("solo", nil)
		assert.Equal(t, "solo", got)
	})
}

// newTestFSM creates a vectorsigma FSM with all dependencies mocked.
func newTestFSM(t *testing.T) *ContextMatrixOrchestrator {
	t.Helper()

	sm := New()
	sm.Context.Logger = slog.Default()
	sm.Context.MCP = &mockMCP{}
	sm.Context.GitTokens = &mockGitTokens{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sm.ExtendedState.RunCtx = ctx
	sm.ExtendedState.RunCancel = cancel

	return sm
}

func TestInitializeAction(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"

	err := fsm.InitializeAction()
	require.NoError(t, err)
	require.NotNil(t, fsm.ExtendedState.StopCh)
	require.NotNil(t, fsm.ExtendedState.ChatInputCh)
	require.NoError(t, fsm.ExtendedState.Error)

	// GitTokens.Mint was called as the InitializeAction probe.
	mock, ok := fsm.Context.GitTokens.(*mockGitTokens)
	require.True(t, ok)
	require.Equal(t, 1, mock.mintCalls)
}

func TestClaimCardActionPopulatesCard(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"

	err := fsm.ClaimCardAction()
	require.NoError(t, err)
	require.NoError(t, fsm.ExtendedState.Error)
	require.NotNil(t, fsm.ExtendedState.Card)
	require.Equal(t, "ABC-1", fsm.ExtendedState.Card.ID)
	require.Equal(t, "p1", fsm.ExtendedState.Card.Project)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Contains(t, mock.claimCalls, "ABC-1")
}

func TestDecideStartingPhaseAction(t *testing.T) {
	fsm := newTestFSM(t)

	err := fsm.DecideStartingPhaseAction()
	require.NoError(t, err)
}

func TestRunPlanPhaseActionParsesMarker(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "ABC-1", Description: "Add login flow"}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	// Drive fake CC stdout: system_init + text containing PLAN_DRAFTED + system_end.
	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"init","session_id":"sess_1"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"PLAN_DRAFTED\ncard_id: ABC-1\nstatus: drafted\nplan_summary: ok\nsubtask_count: 2\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":1000,"output_tokens":500}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	err := fsm.RunPlanPhaseAction()
	require.NoError(t, err)
	require.NoError(t, fsm.ExtendedState.Error)
	require.NotNil(t, fsm.ExtendedState.Plan)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Equal(t, 1, mock.reportUsageCalls, "ReportUsage should be called once")
}

func TestRunPlanPhaseActionParsesStructuredPayload(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "ABC-1", Description: "Add login flow"}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	// Plan agent emits a fenced JSON payload following the marker.
	planJSON := "PLAN_DRAFTED\n```json\n" + `{
  "card_id": "ABC-1",
  "plan_summary": "two-step login flow",
  "chosen_repos": ["auth-svc"],
  "subtasks": [
    {"title": "Implement signer", "description": "Create jwt.go", "repos": ["auth-svc"], "priority": "high", "depends_on": []},
    {"title": "Wire login handler", "description": "Hook signer into POST /login", "repos": ["auth-svc"], "priority": "medium", "depends_on": ["OTHER-9"]}
  ]
}
` + "```"

	// stream-json wraps the text in a JSON envelope; embed the planJSON
	// safely by JSON-escaping it.
	envelope, err := json.Marshal(map[string]any{"type": "text", "text": planJSON})
	require.NoError(t, err)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"init","session_id":"sess_1"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write(envelope)
		_, _ = fakeAPI.stdoutWrite.Write([]byte("\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":1000,"output_tokens":500}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunPlanPhaseAction())
	require.NoError(t, fsm.ExtendedState.Error)
	require.NotNil(t, fsm.ExtendedState.Plan)

	plan := fsm.ExtendedState.Plan
	require.Equal(t, "two-step login flow", plan.Summary)
	require.Equal(t, []string{"auth-svc"}, plan.ChosenRepos)
	require.Len(t, plan.Subtasks, 2)
	require.Equal(t, "Implement signer", plan.Subtasks[0].Title)
	require.Equal(t, "high", plan.Subtasks[0].Priority)
	require.Empty(t, plan.Subtasks[0].DependsOn)
	require.Equal(t, []string{"OTHER-9"}, plan.Subtasks[1].DependsOn)
	// IDs are not set yet — CreateSubtaskCardsAction populates them.
	require.Empty(t, plan.Subtasks[0].ID)
}

func TestRunPlanPhaseActionParsesMarkerSplitAcrossTextBlocks(t *testing.T) {
	// Regression: when Claude emits prose and the structured marker as
	// distinct text content blocks (with a tool_use intervening), the
	// concatenation in runEphemeralPhase must insert a newline so the
	// line-anchored marker regex still matches.
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "ABC-1"}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	// Two text frames: a prelude paragraph WITHOUT a trailing newline,
	// then the structured marker block.
	preludeEnv, err := json.Marshal(map[string]any{
		"type": "text",
		"text": "I'll plan two subtasks.",
	})
	require.NoError(t, err)

	markerEnv, err := json.Marshal(map[string]any{
		"type": "text",
		"text": "PLAN_DRAFTED\ncard_id: ABC-1\nstatus: drafted\nplan_summary: ok\nsubtask_count: 1\n",
	})
	require.NoError(t, err)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write(preludeEnv)
		_, _ = fakeAPI.stdoutWrite.Write([]byte("\n"))
		_, _ = fakeAPI.stdoutWrite.Write(markerEnv)
		_, _ = fakeAPI.stdoutWrite.Write([]byte("\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":50,"output_tokens":25}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunPlanPhaseAction())
	require.NoError(t, fsm.ExtendedState.Error)
	require.NotNil(t, fsm.ExtendedState.Plan)
}

func TestRunPlanPhaseActionErrorsWithoutMarker(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = &Card{ID: "ABC-1"}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"no marker here"}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	err := fsm.RunPlanPhaseAction()
	require.NoError(t, err) // action returns nil; sets ExtendedState.Error.
	require.Error(t, fsm.ExtendedState.Error)
	require.Contains(t, fsm.ExtendedState.Error.Error(), "parse marker")
}

// TestRunPlanPhaseActionRecoversFromMissingMarkerViaCardBody pins the
// real-Claude flakiness recovery: in autonomous mode the agent
// occasionally calls `update_card` with a complete `## Plan` section
// (markdown + fenced ```json) but stops without emitting the
// PLAN_DRAFTED text marker the runner expects. Before the fallback,
// this turned every such run into a hard FSM error ("plan: parse
// marker: no marker found in text") and the card's promotion / brand
// new autonomous run was lost. The body IS the spec — so when the
// text marker is missing and the body has the canonical fenced JSON,
// the runner recovers by parsing from the body and the FSM advances
// normally to subtask creation.
func TestRunPlanPhaseActionRecoversFromMissingMarkerViaCardBody(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "ABC-1", Description: "Add login flow"}

	// Pre-populate the mock's card body with a canonical `## Plan`
	// section — the agent's `update_card` call would have written this
	// in real life. The fallback reads it via GetTaskContext.
	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)

	mock.cardBody = `Some prior description.

## Design

REST API for auth. Open questions: token format.

## Plan

stub: single-subtask plan recovered from body fallback.

` + "```json\n" + `{
  "card_id": "ABC-1",
  "plan_summary": "single-subtask login flow recovered from body",
  "chosen_repos": ["auth-svc"],
  "subtasks": [
    {"title": "Implement signer", "description": "Create jwt.go", "repos": ["auth-svc"], "priority": "high", "depends_on": []}
  ]
}
` + "```\n"

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	// Simulate the failure mode exactly: the agent emits prose ending
	// with "Let me write the plan to the card body." and stops after
	// its update_card tool call WITHOUT printing the PLAN_DRAFTED
	// marker. ParseMarker(text) finds no marker in this stream.
	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"init","session_id":"sess_recover"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"Let me write the plan to the card body."}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":100,"output_tokens":40}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunPlanPhaseAction())
	require.NoError(t, fsm.ExtendedState.Error,
		"missing PLAN_DRAFTED must NOT fail the FSM when the card body has the canonical `## Plan` JSON")
	require.NotNil(t, fsm.ExtendedState.Plan,
		"the plan must be populated from the card body's fenced json block")

	plan := fsm.ExtendedState.Plan
	require.Equal(t, "single-subtask login flow recovered from body", plan.Summary)
	require.Equal(t, []string{"auth-svc"}, plan.ChosenRepos)
	require.Len(t, plan.Subtasks, 1)
	require.Equal(t, "Implement signer", plan.Subtasks[0].Title)
}

func TestRunBrainstormingDialogueCompletesOnDiscovery(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "ABC-1"}

	fsm.ExtendedState.ChatInputCh = make(chan string, 4)
	fsm.ExtendedState.ChatInputCh <- "approved, let's go"

	fsm.ExtendedState.StopCh = make(chan struct{}, 1)

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"init","session_id":"sess_brain"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__contextmatrix__discovery_complete","input":{"design_summary":"REST API for auth"}}]}}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":50,"output_tokens":20}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	err := fsm.RunBrainstormingDialogueAction()
	require.NoError(t, err)
	require.NoError(t, fsm.ExtendedState.Error)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.True(t, mock.cardFieldUpdates["discovery_complete"].(bool))

	// Regression: discovery_complete must NOT rewrite the card body's
	// `## Design` section. The brainstorm agent has already populated
	// the section via update_card with the full multi-section design;
	// overwriting it with the one-paragraph design_summary destroys
	// the spec. The summary surfaces as an activity-log entry instead.
	require.NotContains(t, mock.bodyUpdates, "Design",
		"discovery_complete must not rewrite the Design section of the body")

	require.Contains(t, mock.addLogCalls, "discovery_complete",
		"discovery_complete should append an activity log entry")
	require.Contains(t, strings.Join(mock.addLogMessages, "|"), "REST API for auth",
		"the activity log entry should carry the design_summary")
}

func TestRunBrainstormingDialogueStopsOnStopCh(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = &Card{ID: "ABC-1"}
	fsm.ExtendedState.ChatInputCh = make(chan string, 4)
	fsm.ExtendedState.StopCh = make(chan struct{}, 1)

	// Pre-pulse stop so the chat-loop returns before any spawn would
	// fire. Claude can stay nil — the select picks up the stop first.
	fsm.ExtendedState.StopCh <- struct{}{}

	err := fsm.RunBrainstormingDialogueAction()
	require.NoError(t, err)
	require.ErrorIs(t, fsm.ExtendedState.Error, ErrStopped)
}

func TestRunChatLoopReturnsErrPromotedOnAutonomousFlip(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "ABC-1"}

	fsm.ExtendedState.ChatInputCh = make(chan string, 4)
	fsm.ExtendedState.StopCh = make(chan struct{}, 1)

	// Mode = Autonomous simulates the post-promotion state: the driver
	// has already flipped Mode and (in real runs) injected the canned
	// promotion chat message. The agent's turn produces no terminal
	// marker. Without the mode-flip exit the loop would re-enter the
	// select on ChatInputCh and hang until the heartbeat timeout.
	fsm.ExtendedState.StoreMode(ModeAutonomous)

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	// Drive a single fake turn: system_init + system_end with no tool_use.
	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"init","session_id":"sess_p"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":10,"output_tokens":5}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	err := fsm.runChatLoop(fsm.ExtendedState.RunCtx, chatLoopConfig{
		phase:        "brainstorm",
		systemPrompt: "test prompt",
		primer:       "test primer",
		model:        "claude-sonnet-4-6",
		allowedTools: mcpAllowedTools("Read"),
		onTerminalMarker: func(_ context.Context, _ claudeclient.Marker) (bool, error) {
			return false, nil
		},
	})
	require.ErrorIs(t, err, ErrPromoted)

	// The phase-awaiting log must NOT be emitted on promotion exit;
	// it would mislead observers into thinking the loop is still active.
	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.NotContains(t, mock.addLogCalls, "phase",
		"phase-awaiting log must not fire on ErrPromoted exit")
}

func TestRunBrainstormingDialogueExitsCleanlyOnPromotion(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "ABC-1"}

	fsm.ExtendedState.ChatInputCh = make(chan string, 4)
	fsm.ExtendedState.StopCh = make(chan struct{}, 1)

	// Mode = Autonomous before the action runs; the chat loop's first
	// turn produces no terminal marker, mode-flip check fires,
	// runChatLoop returns ErrPromoted, the action handles it gracefully.
	fsm.ExtendedState.StoreMode(ModeAutonomous)

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"init","session_id":"sess_brain"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":10,"output_tokens":5}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunBrainstormingDialogueAction())
	require.NoError(t, fsm.ExtendedState.Error,
		"promotion mid-brainstorm must not stash an error — the FSM advances normally")

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.True(t, mock.cardFieldUpdates["discovery_complete"].(bool),
		"brainstorm action must stamp discovery_complete: true on promotion")
	require.NotContains(t, mock.bodyUpdates, "Design",
		"brainstorm action must NOT write a synthesized Design section on promotion")
}

// TestRunEphemeralPhaseInjectsGitTokenEnv pins the regression that
// agent-driven git/gh inside Claude's Bash tool needs CM_GIT_TOKEN /
// GH_TOKEN on the Claude `docker exec` env. The credential helper in
// docker/entrypoint-orchestrated.sh reads CM_GIT_TOKEN from the per-
// exec env at credential-request time; without these env vars on the
// Claude exec, every agent-driven `git clone` of a private repo
// fails with an auth error. Before the fix, only workspaceExec
// (runner-driven git ops like clone-into-worktree, push, pr-create)
// got the per-exec injection. This test asserts the autonomous
// ephemeral-phase Spawn now carries the env too — exercising
// runEphemeralPhase via RunPlanPhaseAction which is the one-shot
// path the autonomous plan agent runs on.
func TestRunEphemeralPhaseInjectsGitTokenEnv(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "ABC-1"}
	fsm.Context.GitTokens = &mockGitTokens{}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"init","session_id":"sess_1"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"PLAN_DRAFTED\ncard_id: ABC-1\nstatus: drafted\nplan_summary: ok\nsubtask_count: 0\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":1,"output_tokens":1}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunPlanPhaseAction())
	require.NoError(t, fsm.ExtendedState.Error)

	require.Contains(t, fakeAPI.lastEnv, "CM_GIT_TOKEN=stub-token",
		"ephemeral phase Spawn must inject CM_GIT_TOKEN so the worker's git "+
			"credential helper can authenticate; without this, agent-driven "+
			"`git clone` of private repos fails with an auth error")
	require.Contains(t, fakeAPI.lastEnv, "GH_TOKEN=stub-token",
		"ephemeral phase Spawn must inject GH_TOKEN so the agent's `gh` CLI "+
			"calls are authenticated alongside git")
}

// TestRunChatLoopInjectsGitTokenEnv is the chat-loop counterpart:
// HITL phases (brainstorm, plan-HITL, review-HITL, replan-HITL) all
// spawn Claude via runOneChatTurn. The same per-exec token env is
// required for the agent's git/gh calls inside those turns to
// authenticate against private repos.
func TestRunChatLoopInjectsGitTokenEnv(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "ABC-1"}
	fsm.Context.GitTokens = &mockGitTokens{}

	fsm.ExtendedState.ChatInputCh = make(chan string, 4)
	fsm.ExtendedState.StopCh = make(chan struct{}, 1)

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"init","session_id":"sess_brain"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__contextmatrix__discovery_complete","input":{"design_summary":"ok"}}]}}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":1,"output_tokens":1}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunBrainstormingDialogueAction())
	require.NoError(t, fsm.ExtendedState.Error)

	require.Contains(t, fakeAPI.lastEnv, "CM_GIT_TOKEN=stub-token",
		"chat-loop Spawn must inject CM_GIT_TOKEN on the per-turn Claude exec")
	require.Contains(t, fakeAPI.lastEnv, "GH_TOKEN=stub-token",
		"chat-loop Spawn must inject GH_TOKEN on the per-turn Claude exec")
}

// TestRunBrainstormingDialogueDrainsBufferedPromotionMessage exercises
// the regression: when the user clicks Promote DURING an in-flight turn,
// the driver flips Mode to Autonomous AND buffers the canned promotion
// chat message into ChatInputCh. The chat loop must drain that buffered
// message and run ONE more turn so the agent can read the promotion
// trigger and follow the prompt's "Promotion mid-dialogue" handler
// (synthesize design → call update_card → emit discovery_complete).
//
// Before the chat-loop fix the loop returned ErrPromoted immediately on
// the post-turn Mode check, dropping the buffered message and never
// giving the agent a chance to capture the design. This test pins the
// fix: with a buffered message the loop must run the drain turn, the
// agent's discovery_complete must fire normally, and the action must
// return nil (not ErrPromoted) with discovery_complete: true stamped.
func TestRunBrainstormingDialogueDrainsBufferedPromotionMessage(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "ABC-1"}

	fsm.ExtendedState.ChatInputCh = make(chan string, 4)
	fsm.ExtendedState.StopCh = make(chan struct{}, 1)

	// Simulate the post-promotion state: Mode flipped AND a synthetic
	// promotion message buffered in ChatInputCh. This is exactly what
	// driver.dispatch does when /promote fires mid-turn — the buffered
	// message is the agent's only signal to act on the promotion.
	fsm.ExtendedState.StoreMode(ModeAutonomous)

	fsm.ExtendedState.ChatInputCh <- "The user has just promoted this card to autonomous mode. Synthesize and emit the terminal marker."

	// Two-turn fake: primer turn produces no marker (the in-flight turn
	// that ran while the user was clicking Promote); drain turn produces
	// update_card (with a Design body) followed by discovery_complete
	// (the agent reacting to the buffered promotion message).
	turn1 := []byte(
		`{"type":"system","subtype":"init","session_id":"sess_brain"}` + "\n" +
			`{"type":"system","subtype":"end","usage":{"input_tokens":10,"output_tokens":5}}` + "\n",
	)
	turn2 := []byte(
		`{"type":"system","subtype":"init","session_id":"sess_brain"}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__contextmatrix__update_card","input":{"card_id":"ABC-1","body":"## Design\n\nAgreed: REST API.\n\n### Open questions\n\n- Auth scheme."}}]}}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__contextmatrix__discovery_complete","input":{"design_summary":"REST API; auth scheme open"}}]}}` + "\n" +
			`{"type":"system","subtype":"end","usage":{"input_tokens":50,"output_tokens":20}}` + "\n",
	)

	fakeAPI := newMultiTurnFakeExecAPI(turn1, turn2)
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	require.NoError(t, fsm.RunBrainstormingDialogueAction())
	require.NoError(t, fsm.ExtendedState.Error,
		"drain-turn promotion must not stash an error — the FSM advances normally")

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.True(t, mock.cardFieldUpdates["discovery_complete"].(bool),
		"discovery_complete must be stamped after the agent's drain-turn marker")
	require.Contains(t, strings.Join(mock.addLogMessages, "|"), "REST API; auth scheme open",
		"the drain turn's design_summary must reach the activity log")

	require.Equal(t, 2, fakeAPI.attaches,
		"the loop must run TWO turns: the in-flight primer turn AND the drain turn")
}

func TestRunDiagnosisPhaseAction(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "BUG-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "BUG-1"}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"DIAGNOSIS_COMPLETE\ncard_id: BUG-1\nroot_cause: race in writeBuf\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":100,"output_tokens":50}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunDiagnosisPhaseAction())
	require.NoError(t, fsm.ExtendedState.Error)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Equal(t, 1, mock.reportUsageCalls)
}

func TestRunDiagnosisPhaseActionErrorsWithoutMarker(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = &Card{ID: "BUG-1"}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"no marker"}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunDiagnosisPhaseAction())
	require.Error(t, fsm.ExtendedState.Error)
	require.Contains(t, fsm.ExtendedState.Error.Error(), "parse marker")
}

func TestRunDocumentPhaseActionPopulatesResult(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "P-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "P-1"}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"DOCS_WRITTEN\ncard_id: P-1\nstatus: written\nfiles_written: [README.md, CHANGELOG.md]\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":50,"output_tokens":25}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunDocumentPhaseAction())
	require.NoError(t, fsm.ExtendedState.Error)
	require.NotNil(t, fsm.ExtendedState.DocsResult)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Equal(t, true, mock.cardFieldUpdates["docs_written"])
}

func TestRunReviewPhaseActionPopulatesResult(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "P-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "P-1"}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"REVIEW_FINDINGS\ncard_id: P-1\nrecommendation: approve\nsummary: ok\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":80,"output_tokens":40}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunReviewPhaseAction())
	require.NoError(t, fsm.ExtendedState.Error)
	require.NotNil(t, fsm.ExtendedState.ReviewResult)
	require.Equal(t, "approve", fsm.ExtendedState.ReviewResult.Recommendation)
	require.Equal(t, "ok", fsm.ExtendedState.ReviewResult.Summary)
}

func TestRunReplanPhaseDelegatesToPlan(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "P-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "P-1", RevisionRequested: true}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"PLAN_DRAFTED\ncard_id: P-1\nstatus: drafted\nplan_summary: redo\nsubtask_count: 1\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":10,"output_tokens":5}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunReplanPhaseAction())
	require.NoError(t, fsm.ExtendedState.Error)
}

func TestCreateSubtaskCardsAction(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "PARENT-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{
		{Title: "a", Description: "first", Repos: []string{"r1"}, Priority: "high"},
		{Title: "b", Description: "second", Repos: []string{"r2"}, DependsOn: []string{"OTHER-9"}},
	}}

	require.NoError(t, fsm.CreateSubtaskCardsAction())
	require.NoError(t, fsm.ExtendedState.Error)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Equal(t, 2, mock.createCardCalls)

	// Returned IDs are stored on the subtasks so downstream actions can
	// reference real CM card IDs.
	require.Equal(t, "SUB-1", fsm.ExtendedState.Plan.Subtasks[0].ID)
	require.Equal(t, "SUB-2", fsm.ExtendedState.Plan.Subtasks[1].ID)

	// Priority defaults to "medium" when unset; explicit values pass through.
	require.Equal(t, "high", mock.createCardInputs[0].Priority)
	require.Equal(t, "medium", mock.createCardInputs[1].Priority)

	// First subtask has no agent-supplied deps and no previous sibling,
	// so depends_on stays empty. The second subtask combines its own
	// agent-supplied dep ("OTHER-9") with the previous subtask's
	// just-assigned ID, which is what drives the CM blocked badge.
	require.Empty(t, mock.createCardInputs[0].DependsOn)
	require.Equal(t, []string{"OTHER-9", "SUB-1"}, mock.createCardInputs[1].DependsOn)
	require.Equal(t, []string{"OTHER-9", "SUB-1"}, fsm.ExtendedState.Plan.Subtasks[1].DependsOn)
}

func TestCreateSubtaskCardsActionSkipsAlreadyCreated(t *testing.T) {
	// Replan path: some subtasks already have IDs from a prior round.
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "PARENT-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{
		{ID: "SUB-EXISTING", Title: "already created"},
		{Title: "newly added"},
	}}

	require.NoError(t, fsm.CreateSubtaskCardsAction())

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Equal(t, 1, mock.createCardCalls, "should skip already-IDed subtask")
	require.Equal(t, "SUB-EXISTING", fsm.ExtendedState.Plan.Subtasks[0].ID)
	require.Equal(t, "SUB-1", fsm.ExtendedState.Plan.Subtasks[1].ID)
}

func TestCreateSubtaskCardsActionEmptyPlan(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Plan = nil

	require.NoError(t, fsm.CreateSubtaskCardsAction())
	require.NoError(t, fsm.ExtendedState.Error)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Equal(t, 0, mock.createCardCalls)
}

func TestIncrementRevisionAttemptsAction(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "P-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "P-1", RevisionAttempts: 1}

	require.NoError(t, fsm.IncrementRevisionAttemptsAction())
	require.Equal(t, 2, fsm.ExtendedState.Card.RevisionAttempts)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Equal(t, 2, mock.cardFieldUpdates["revision_attempts"])
}

func TestIncrementRevisionAttemptsActionNilCard(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = nil

	require.NoError(t, fsm.IncrementRevisionAttemptsAction())
}

func TestTransitionCardToDoneAction(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "P-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "P-1"}
	fsm.ExtendedState.ReviewResult = &ReviewResult{Summary: "all good"}

	require.NoError(t, fsm.TransitionCardToDoneAction())
	require.NoError(t, fsm.ExtendedState.Error)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)

	// The action does NOT use complete_task — that tool transitions main
	// tasks to "review" on the CM side. Instead it adds a completion log,
	// transitions to "done" directly, and releases the claim.
	require.Equal(t, 0, mock.completeTaskCalls)
	require.Contains(t, mock.transitionCalls, "done")
	require.Contains(t, mock.addLogCalls, "completed")
}

func TestEmitAutonomousHaltedAction(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "P-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "P-1"}
	fsm.ExtendedState.ReviewResult = &ReviewResult{Summary: "still failing tests"}

	require.NoError(t, fsm.EmitAutonomousHaltedAction())

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Contains(t, mock.addLogCalls, "halted")
}

func TestHandleErrorAction(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "P-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "P-1"}
	fsm.ExtendedState.Error = errors.New("boom")

	require.NoError(t, fsm.HandleErrorAction())

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Contains(t, mock.addLogCalls, "error")
	require.Equal(t, 1, mock.releaseCalls)
	// Error is preserved so the FSM caller can observe it.
	require.Error(t, fsm.ExtendedState.Error)
}

func TestHandleErrorActionWithoutError(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Error = nil

	require.NoError(t, fsm.HandleErrorAction())

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.NotContains(t, mock.addLogCalls, "error")
	require.Equal(t, 0, mock.releaseCalls)
}

func TestRunExecutePhaseParallelHappyPath(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = &Card{ID: "P-1"}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{{ID: "SUB-1", Title: "do thing"}}}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"TASK_COMPLETE\ncard_id: SUB-1\nstatus: done\nsummary: ok\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":10,"output_tokens":20}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunExecutePhaseParallelAction())
	require.NoError(t, fsm.ExtendedState.Error)
	require.Len(t, fsm.ExtendedState.SubtaskResults, 1)
	require.Equal(t, "done", fsm.ExtendedState.SubtaskResults[0].Status)
	require.Equal(t, "ok", fsm.ExtendedState.SubtaskResults[0].Summary)
}

func TestRunExecutePhaseParallelHandlesBlocked(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = &Card{ID: "P-1"}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{{ID: "SUB-1", Title: "x"}}}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"TASK_BLOCKED\ncard_id: SUB-1\nreason: needs api in OTHER-9\nblocker_cards: [OTHER-9]\nneeds_human: true\n"}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunExecutePhaseParallelAction())

	// The action records the blocked subtask result …
	require.Len(t, fsm.ExtendedState.SubtaskResults, 1)
	require.Equal(t, "blocked", fsm.ExtendedState.SubtaskResults[0].Status)
	require.Equal(t, []string{"OTHER-9"}, fsm.ExtendedState.SubtaskResults[0].BlockerCards)
	require.True(t, fsm.ExtendedState.SubtaskResults[0].NeedsHuman)

	// … and synthesizes an aggregate Error so HandleError can surface
	// the failure reason via the orchestrator log + card activity log.
	require.Error(t, fsm.ExtendedState.Error)
	require.Contains(t, fsm.ExtendedState.Error.Error(), "all 1 subtask(s) blocked")
	require.Contains(t, fsm.ExtendedState.Error.Error(), "SUB-1")
}

func TestRunExecutePhaseParallelHandlesDecomposition(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = &Card{ID: "P-1"}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{{ID: "SUB-1", Title: "huge"}}}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"TASK_NEEDS_DECOMPOSITION\ncard_id: SUB-1\nsubtasks: [migrate, regen, retest]\n"}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunExecutePhaseParallelAction())
	require.NoError(t, fsm.ExtendedState.Error)
	require.Len(t, fsm.ExtendedState.SubtaskResults, 1)
	require.Equal(t, "needs_decomposition", fsm.ExtendedState.SubtaskResults[0].Status)
	// Plan.Subtasks should now include the proposals (original + 3 new).
	require.GreaterOrEqual(t, len(fsm.ExtendedState.Plan.Subtasks), 4)
}

// TestRunExecutePhaseParallelSkipsCompletedOnReentry exercises the
// re-entry path the FSM takes after a TASK_NEEDS_DECOMPOSITION result
// has been routed through CreatingSubtasks. The newly-IDed subtask runs
// while the original (already-complete) one is skipped — without this
// the action would re-execute every subtask, producing duplicate work
// and corrupting the result list.
func TestRunExecutePhaseParallelSkipsCompletedOnReentry(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = &Card{ID: "P-1"}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{
		{ID: "SUB-1", Title: "first"},
		// SUB-2 was appended via decomposition and IDed by
		// CreatingSubtasks before the FSM came back to Executing.
		{ID: "SUB-2", Title: "decomposed-child"},
	}}
	// Simulate the prior pass: SUB-1 already finished, recorded as done.
	fsm.ExtendedState.SubtaskResults = []ExecuteResult{{
		SubtaskID: "SUB-1",
		Status:    "done",
		Summary:   "first done",
	}}

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	// Only one TASK_COMPLETE frame: if the action incorrectly re-executes
	// SUB-1 the second exec attach reads an already-closed pipe and the
	// result count would be wrong (or the test would block).
	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"TASK_COMPLETE\ncard_id: SUB-2\nstatus: done\nsummary: child done\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":10,"output_tokens":5}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunExecutePhaseParallelAction())
	require.NoError(t, fsm.ExtendedState.Error)

	// Two results: the prior SUB-1 entry preserved + the new SUB-2 done.
	require.Len(t, fsm.ExtendedState.SubtaskResults, 2)
	assert.Equal(t, "SUB-1", fsm.ExtendedState.SubtaskResults[0].SubtaskID)
	assert.Equal(t, "done", fsm.ExtendedState.SubtaskResults[0].Status)
	assert.Equal(t, "first done", fsm.ExtendedState.SubtaskResults[0].Summary)
	assert.Equal(t, "SUB-2", fsm.ExtendedState.SubtaskResults[1].SubtaskID)
	assert.Equal(t, "done", fsm.ExtendedState.SubtaskResults[1].Status)
	assert.Equal(t, "child done", fsm.ExtendedState.SubtaskResults[1].Summary)
}

func TestPushBranchesAndOpenPRsAction(t *testing.T) {
	// With no Workspace configured the action falls back to the
	// recordPushIntent path. The new contract is one push per parent
	// per repo (not one per subtask) — so two repos × one feature
	// branch = two ReportPush calls.
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = &Card{
		ID:          "P-1",
		ChosenRepos: []string{"auth", "billing"},
		BranchName:  "feat/p-1",
		BaseBranch:  "main",
	}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{
		{ID: "SUB-1", Title: "a", Repos: []string{"auth"}},
		{ID: "SUB-2", Title: "b", Repos: []string{"billing"}},
	}}
	fsm.ExtendedState.Project = "proj"
	fsm.ExtendedState.CardID = "P-1"
	fsm.ExtendedState.AgentID = "agent:test"

	require.NoError(t, fsm.PushBranchesAndOpenPRsAction())
	require.NoError(t, fsm.ExtendedState.Error)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Equal(t, 2, mock.reportPushCalls)
}

// tokenFailExec is a workspace.Exec stub that fails any command whose
// argv contains one of the given tokens. The command tokens emitted by
// finalize.go are unique enough ("push", "merge", "switch", "create",
// "fetch") that single-token matching is sufficient to target one of
// the four finalize sub-steps in isolation.
type tokenFailExec struct {
	failTokens []string
	err        error
	stderr     string
}

func (s *tokenFailExec) Exec(_ context.Context, cmd []string) (string, string, error) {
	for _, want := range s.failTokens {
		for _, got := range cmd {
			if got == want {
				return "", s.stderr, s.err
			}
		}
	}
	// Default success: gh pr create needs to print a URL on stdout, but
	// the only test that lets the push pass also lets the gh call pass.
	return "", "", nil
}

// integrateExec is a workspace.Exec stub that records every command
// and can fail the three integrate sub-steps (rebase, rebase --abort,
// fast-forward merge) independently. Used by the integrateSubtaskRepo
// tests to assert which sub-steps ran without conflating rebase with
// rebase --abort the way tokenFailExec would.
type integrateExec struct {
	mu        sync.Mutex
	calls     [][]string
	rebaseErr error // returned for `git -C <wt> rebase <branch>` (sans --abort)
	abortErr  error // returned for `git -C <wt> rebase --abort`
	ffErr     error // returned for `git -C <repo> merge --ff-only ...`
}

func (s *integrateExec) Exec(_ context.Context, cmd []string) (string, string, error) {
	s.mu.Lock()

	cp := append([]string(nil), cmd...)
	s.calls = append(s.calls, cp)
	s.mu.Unlock()

	hasToken := func(tok string) bool {
		for _, a := range cmd {
			if a == tok {
				return true
			}
		}

		return false
	}

	switch {
	case hasToken("rebase") && hasToken("--abort"):
		return "", "", s.abortErr
	case hasToken("rebase"):
		return "", "", s.rebaseErr
	case hasToken("merge") && hasToken("--ff-only"):
		return "", "", s.ffErr
	}

	return "", "", nil
}

// recorded returns the recorded argv slices in invocation order.
func (s *integrateExec) recorded() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([][]string, len(s.calls))
	copy(out, s.calls)

	return out
}

// hasCallContaining reports whether any recorded argv contains all
// the given tokens (in any position).
func (s *integrateExec) hasCallContaining(tokens ...string) bool {
	for _, c := range s.recorded() {
		matched := 0

		for _, want := range tokens {
			for _, got := range c {
				if got == want {
					matched++

					break
				}
			}
		}

		if matched == len(tokens) {
			return true
		}
	}

	return false
}

// TestPushBranchesAndOpenPRsAction_PushFailureSurfacesToActivityLog
// exercises the regression where a `git push` rejection (e.g.
// non-fast-forward, because a feature branch from a prior run is still
// on the remote) was logged only at WARN on the runner stderr. The
// parent card transitioned to done with no PR and no visible failure
// in the CM UI. Per-step failures still don't fail the action — that
// is intentional, see PushBranchesAndOpenPRsAction's docstring — but
// they MUST land in the activity log so the UI surfaces them.
func TestPushBranchesAndOpenPRsAction_PushFailureSurfacesToActivityLog(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "proj"
	fsm.ExtendedState.CardID = "P-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{
		ID:          "P-1",
		ChosenRepos: []string{"auth"},
		BranchName:  "feat/p-1",
		BaseBranch:  "main",
	}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{
		{ID: "SUB-1", Title: "a", Repos: []string{"auth"}},
	}}

	fsm.Context.Workspace = workspace.NewManager(
		&tokenFailExec{
			failTokens: []string{"push"},
			err:        errors.New("non-fast-forward"),
			stderr:     "rejected: non-fast-forward",
		},
		[]workspace.RepoSpec{{Slug: "auth", URL: "https://github.com/acme/auth.git"}},
	)

	require.NoError(t, fsm.PushBranchesAndOpenPRsAction())
	require.NoError(t, fsm.ExtendedState.Error)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Contains(t, mock.addLogCalls, "finalize_failed",
		"push failure must surface as a finalize_failed activity log entry")

	joined := strings.Join(mock.addLogMessages, "|")
	require.Contains(t, joined, "push",
		"log message should name the failed sub-step (push)")
	require.Contains(t, joined, "auth",
		"log message should name the affected repo")
}

// TestPushBranchesAndOpenPRsAction_PRFailureSurfacesToActivityLog
// covers the partner case: push succeeded, opening the PR with `gh pr
// create` failed. Same contract — the activity log records the
// failure even though the action returns nil.
func TestPushBranchesAndOpenPRsAction_PRFailureSurfacesToActivityLog(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "proj"
	fsm.ExtendedState.CardID = "P-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{
		ID:          "P-1",
		ChosenRepos: []string{"auth"},
		BranchName:  "feat/p-1",
		BaseBranch:  "main",
	}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{
		{ID: "SUB-1", Title: "a", Repos: []string{"auth"}},
	}}

	fsm.Context.Workspace = workspace.NewManager(
		&tokenFailExec{
			failTokens: []string{"create"}, // matches `gh pr create`
			err:        errors.New("auth required"),
			stderr:     "gh: not authenticated",
		},
		[]workspace.RepoSpec{{Slug: "auth", URL: "https://github.com/acme/auth.git"}},
	)

	require.NoError(t, fsm.PushBranchesAndOpenPRsAction())
	require.NoError(t, fsm.ExtendedState.Error)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Contains(t, mock.addLogCalls, "finalize_failed",
		"PR-open failure must surface as a finalize_failed activity log entry")

	joined := strings.Join(mock.addLogMessages, "|")
	require.Contains(t, joined, "PR",
		"log message should name the failed sub-step (PR open)")
}

// TestFinalize_PushesFeatureBranchOncePerRepo locks in Task 8's
// contract: finalize is push + open-PR only. Subtasks are integrated
// onto the feature branch at execute time, so finalize must NOT run
// `switch -C` (CheckoutFeatureBranch) or `merge --no-ff` (MergeBranch)
// on any repo. The load-bearing assertion is the per-repo push count;
// the gh CLI is unavailable in tests so the PR-open step will fail
// silently into the activity log — that's accepted.
func TestFinalize_PushesFeatureBranchOncePerRepo(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "proj"
	fsm.ExtendedState.CardID = "CARD-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{
		ID:          "CARD-1",
		Title:       "feature",
		BranchName:  "feat/x",
		BaseBranch:  "main",
		ChosenRepos: []string{"auth", "billing"},
	}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{
		{ID: "SUB-1", Repos: []string{"auth"}},
	}}

	exec := &integrateExec{}
	fsm.Context.Workspace = workspace.NewManager(exec, []workspace.RepoSpec{
		{Slug: "auth", URL: "https://github.com/acme/auth.git"},
		{Slug: "billing", URL: "https://github.com/acme/billing.git"},
	})

	require.NoError(t, fsm.PushBranchesAndOpenPRsAction())

	pushes := 0

	for _, cmd := range exec.recorded() {
		hasPush := false
		hasUpstream := false
		hasSwitchDashC := false
		hasMergeNoFF := false
		sawSwitch := false
		sawDashC := false
		sawMerge := false
		sawNoFF := false

		for _, tok := range cmd {
			switch tok {
			case "push":
				hasPush = true
			case "--set-upstream":
				hasUpstream = true
			case "switch":
				sawSwitch = true
			case "-C":
				sawDashC = true
			case "merge":
				sawMerge = true
			case "--no-ff":
				sawNoFF = true
			}
		}

		if sawSwitch && sawDashC {
			hasSwitchDashC = true
		}

		if sawMerge && sawNoFF {
			hasMergeNoFF = true
		}

		if hasPush && hasUpstream {
			pushes++
		}

		require.False(t, hasSwitchDashC,
			"finalize must not run `switch -C` (no CheckoutFeatureBranch): %v", cmd)
		require.False(t, hasMergeNoFF,
			"finalize must not run `merge --no-ff` (no MergeBranch): %v", cmd)
	}

	require.Equal(t, 2, pushes, "expected one push per repo (auth, billing)")
}

// TestIntegrateSubtaskRepo_CleanRebaseFastForwards covers the happy
// path: the rebase succeeds and the helper fast-forwards the feature
// branch without ever spawning the resolver. Behavioral assertion
// checks that `git rebase feat/x` ran in the worktree and `git merge
// --ff-only cm/SUB-1` ran in the main clone.
func TestIntegrateSubtaskRepo_CleanRebaseFastForwards(t *testing.T) {
	fsm := newTestFSM(t)

	exec := &integrateExec{}
	fsm.Context.Workspace = workspace.NewManager(
		exec,
		[]workspace.RepoSpec{{Slug: "auth", URL: "https://github.com/acme/auth.git"}},
	)

	// No Claude wrapper — if the resolver were spawned it would error.
	require.Nil(t, fsm.Context.Claude)

	err := fsm.integrateSubtaskRepo(context.Background(), "auth", "SUB-1", "Add JWT", "feat/x")
	require.NoError(t, err)

	require.True(t, exec.hasCallContaining("rebase", "feat/x"),
		"RebaseSubtask must run `git rebase feat/x` in the worktree")
	require.True(t, exec.hasCallContaining("merge", "--ff-only", "cm/SUB-1"),
		"FastForwardFeature must run `git merge --ff-only cm/SUB-1` after a clean rebase")
	require.False(t, exec.hasCallContaining("rebase", "--abort"),
		"AbortRebase must NOT run on the clean-rebase happy path")
}

// TestIntegrateSubtaskRepo_ResolverRecoversFromConflict covers the
// recovery path: rebase reports conflict → resolver agent emits
// CONFLICT_RESOLVED → the FF still happens. AbortRebase MUST NOT run
// when the resolver succeeded (the rebase was completed via `git
// rebase --continue` inside the agent's session).
func TestIntegrateSubtaskRepo_ResolverRecoversFromConflict(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = &Card{ID: "P-1"}

	exec := &integrateExec{
		rebaseErr: errors.New("CONFLICT (content): Merge conflict in README.md"),
	}
	fsm.Context.Workspace = workspace.NewManager(
		exec,
		[]workspace.RepoSpec{{Slug: "auth", URL: "https://github.com/acme/auth.git"}},
	)

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"CONFLICT_RESOLVED\nsubtask_id: SUB-1\nbranch: cm/SUB-1\nresolved_files: [a.go]\nsummary: ok\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":10,"output_tokens":20}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	err := fsm.integrateSubtaskRepo(context.Background(), "auth", "SUB-1", "Add JWT", "feat/x")
	require.NoError(t, err)

	require.True(t, exec.hasCallContaining("rebase", "feat/x"),
		"RebaseSubtask must still run on the resolver-recovery path")
	require.True(t, exec.hasCallContaining("merge", "--ff-only", "cm/SUB-1"),
		"FastForwardFeature must run after a successful resolver, not be skipped")
	require.False(t, exec.hasCallContaining("rebase", "--abort"),
		"AbortRebase must NOT run when the resolver succeeded — the rebase was completed via --continue")
}

// TestIntegrateSubtaskRepo_AbortsAndReturnsErrOnResolverFailure
// covers the failure path: rebase conflicts and the resolver gives up
// with CONFLICT_UNRESOLVED. The helper must abort the rebase to clean
// up the worktree, NOT attempt the FF, and return an error so the
// caller can mark the subtask blocked.
func TestIntegrateSubtaskRepo_AbortsAndReturnsErrOnResolverFailure(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = &Card{ID: "P-1"}

	exec := &integrateExec{
		rebaseErr: errors.New("CONFLICT (content): Merge conflict in README.md"),
	}
	fsm.Context.Workspace = workspace.NewManager(
		exec,
		[]workspace.RepoSpec{{Slug: "auth", URL: "https://github.com/acme/auth.git"}},
	)

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"CONFLICT_UNRESOLVED\nsubtask_id: SUB-1\nreason: irreconcilable api change\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":10,"output_tokens":20}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	err := fsm.integrateSubtaskRepo(context.Background(), "auth", "SUB-1", "Add JWT", "feat/x")
	require.Error(t, err)

	require.True(t, exec.hasCallContaining("rebase", "--abort"),
		"AbortRebase must run after an unresolved conflict to clean up the worktree")
	require.False(t, exec.hasCallContaining("merge", "--ff-only", "cm/SUB-1"),
		"FastForwardFeature must NOT run when the resolver gave up")
}

func TestPushBranchesAndOpenPRsActionNoRepos(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Card = &Card{ID: "P-1"}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{{ID: "SUB-1", Title: "a"}}}

	require.NoError(t, fsm.PushBranchesAndOpenPRsAction())
	require.NoError(t, fsm.ExtendedState.Error)

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)
	require.Equal(t, 0, mock.reportPushCalls)
}

// TestRunExecutePhase_CallsEnsureFeatureBranchPerRepo exercises the
// pre-loop feature-branch setup: each UNIQUE repo across subtasks
// must be cloned + EnsureFeatureBranch'd exactly once before any
// subtask runs. The dedupe guards against a multi-subtask plan
// landing N redundant `git switch` invocations on the same clone.
func TestRunExecutePhase_CallsEnsureFeatureBranchPerRepo(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "proj"
	fsm.ExtendedState.CardID = "CARD-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "CARD-1", BranchName: "feat/x", BaseBranch: "main"}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{
		{ID: "SUB-1", Title: "a", Repos: []string{"auth", "billing"}},
		{ID: "SUB-2", Title: "b", Repos: []string{"auth"}},
	}}

	exec := &integrateExec{}
	fsm.Context.Workspace = workspace.NewManager(exec, []workspace.RepoSpec{
		{Slug: "auth", URL: "https://github.com/acme/auth.git"},
		{Slug: "billing", URL: "https://github.com/acme/billing.git"},
	})

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	// Pipe data is read by the FIRST execute spawn; the SECOND spawn
	// hits EOF on the already-closed pipe and returns an empty,
	// unparsable text (which parseExecuteResult treats as blocked).
	// That's fine: this test asserts only on the pre-loop
	// EnsureFeatureBranch count, which fires unconditionally before
	// any subtask spawns.
	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"TASK_COMPLETE\ncard_id: SUB-1\nstatus: done\nsummary: ok\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":10,"output_tokens":20}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunExecutePhaseParallelAction())

	// EnsureFeatureBranch lands a `git -C <repo> switch <branch>` (existing-branch
	// path) per unique repo. Two unique repos → exactly two switch invocations
	// from the pre-loop setup. CreateWorktree does not use `switch`, so the
	// count is unambiguous.
	switchCount := 0

	for _, cmd := range exec.recorded() {
		for _, tok := range cmd {
			if tok == "switch" {
				switchCount++

				break
			}
		}
	}

	require.Equal(t, 2, switchCount,
		"expected EnsureFeatureBranch (`git switch ...`) once per unique repo (auth, billing)")
}

// TestRunExecutePhase_IntegratesEachDoneSubtask exercises the
// post-spawn integrate hook: a TASK_COMPLETE result must trigger
// integrateSubtaskRepo, which lands rebase + ff-only merge per repo
// the subtask touched.
func TestRunExecutePhase_IntegratesEachDoneSubtask(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "proj"
	fsm.ExtendedState.CardID = "CARD-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "CARD-1", BranchName: "feat/x", BaseBranch: "main"}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{
		{ID: "SUB-1", Title: "a", Repos: []string{"auth"}},
	}}

	exec := &integrateExec{}
	fsm.Context.Workspace = workspace.NewManager(exec, []workspace.RepoSpec{
		{Slug: "auth", URL: "https://github.com/acme/auth.git"},
	})

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"TASK_COMPLETE\ncard_id: SUB-1\nstatus: done\nsummary: ok\n"}` + "\n"))
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"system","subtype":"end","usage":{"input_tokens":10,"output_tokens":20}}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunExecutePhaseParallelAction())

	require.True(t, exec.hasCallContaining("rebase", "feat/x"),
		"expected RebaseSubtask (`git rebase feat/x`) after a successful subtask")
	require.True(t, exec.hasCallContaining("merge", "--ff-only", "cm/SUB-1"),
		"expected FastForwardFeature (`git merge --ff-only cm/SUB-1`) after a clean rebase")
}

// TestRunExecutePhase_DoesNotIntegrateBlockedSubtasks ensures the
// integrate hook is gated on Status=="done". A blocked result must
// not rebase or fast-forward — the worktree branch is unfit to
// merge into the parent's feature branch.
func TestRunExecutePhase_DoesNotIntegrateBlockedSubtasks(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "proj"
	fsm.ExtendedState.CardID = "CARD-1"
	fsm.ExtendedState.AgentID = "agent:test"
	fsm.ExtendedState.Card = &Card{ID: "CARD-1", BranchName: "feat/x", BaseBranch: "main"}
	fsm.ExtendedState.Plan = &Plan{Subtasks: []Subtask{
		{ID: "SUB-1", Title: "a", Repos: []string{"auth"}},
	}}

	exec := &integrateExec{}
	fsm.Context.Workspace = workspace.NewManager(exec, []workspace.RepoSpec{
		{Slug: "auth", URL: "https://github.com/acme/auth.git"},
	})

	fakeAPI := newFakeExecAPI()
	fsm.Context.Claude = claudeclient.NewWrapperWithExecAPI(fakeAPI, nil)

	go func() {
		_, _ = fakeAPI.stdoutWrite.Write([]byte(`{"type":"text","text":"TASK_BLOCKED\ncard_id: SUB-1\nreason: needs api in OTHER-9\nblocker_cards: [OTHER-9]\nneeds_human: true\n"}` + "\n"))
		_ = fakeAPI.stdoutWrite.Close()
	}()

	require.NoError(t, fsm.RunExecutePhaseParallelAction())

	require.False(t, exec.hasCallContaining("rebase", "feat/x"),
		"blocked subtask must not trigger a rebase")
	require.False(t, exec.hasCallContaining("merge", "--ff-only", "cm/SUB-1"),
		"blocked subtask must not trigger a fast-forward merge")
}

// TestPlanFromPayloadSubtaskRepoFallback exercises the defence-in-depth
// fallback that copies chosen_repos into a subtask's Repos when the agent
// leaves it empty. Without this fallback the execute phase iterates an
// empty slice and skips CloneRepo/CreateWorktree entirely, which surfaces
// to the subagent as an empty /workspace and a hard "blocked" status.
func TestPlanFromPayloadSubtaskRepoFallback(t *testing.T) {
	t.Run("fills empty subtask repos from chosen_repos", func(t *testing.T) {
		payload := claudeclient.PlanDraftedPayload{
			PlanSummary: "test",
			ChosenRepos: []string{"only-repo"},
			Subtasks: []claudeclient.SubtaskSpec{
				{Title: "first", Repos: nil},
				{Title: "second", Repos: []string{}},
				{Title: "third", Repos: []string{"explicit"}},
			},
		}

		plan := planFromPayload(payload)

		require.Len(t, plan.Subtasks, 3)
		require.Equal(t, []string{"only-repo"}, plan.Subtasks[0].Repos,
			"empty Repos should fall back to ChosenRepos")
		require.Equal(t, []string{"only-repo"}, plan.Subtasks[1].Repos,
			"zero-length Repos should also fall back")
		require.Equal(t, []string{"explicit"}, plan.Subtasks[2].Repos,
			"explicit Repos must be preserved")
	})

	t.Run("leaves empty when chosen_repos is also empty", func(t *testing.T) {
		payload := claudeclient.PlanDraftedPayload{
			PlanSummary: "pure-spec card",
			ChosenRepos: []string{},
			Subtasks: []claudeclient.SubtaskSpec{
				{Title: "spec-only", Repos: nil},
			},
		}

		plan := planFromPayload(payload)

		require.Len(t, plan.Subtasks, 1)
		require.Empty(t, plan.Subtasks[0].Repos,
			"pure-spec card must keep empty Repos")
	})

	t.Run("each subtask gets its own backing array", func(t *testing.T) {
		payload := claudeclient.PlanDraftedPayload{
			ChosenRepos: []string{"shared"},
			Subtasks: []claudeclient.SubtaskSpec{
				{Title: "first"},
				{Title: "second"},
			},
		}

		plan := planFromPayload(payload)

		require.Len(t, plan.Subtasks, 2)
		// Mutating one subtask's Repos must not leak into the other or
		// back into the source ChosenRepos slice.
		plan.Subtasks[0].Repos[0] = "mutated"
		require.Equal(t, []string{"shared"}, plan.Subtasks[1].Repos)
		require.Equal(t, []string{"shared"}, payload.ChosenRepos)
	})
}

// TestApplyRegisteredRepoFallback exercises the deeper defence-in-depth
// fallback that fires when both subtask.Repos AND chosen_repos come back
// empty (the agent classified a coding card as "pure spec") but the
// worker has exactly one registered repo. Without this an entire run
// blocks on an empty workspace just because the agent demurred.
func TestApplyRegisteredRepoFallback(t *testing.T) {
	t.Run("populates ChosenRepos and subtask Repos when registry has exactly one slug", func(t *testing.T) {
		mgr := workspace.NewManager(nil, []workspace.RepoSpec{
			{Slug: "only-repo", URL: "https://example.test/only.git"},
		})

		fsm := &ContextMatrixOrchestrator{
			Context: &Context{Workspace: mgr},
			ExtendedState: &ExtendedState{
				Plan: &Plan{
					Subtasks: []Subtask{
						{Title: "first"},
						{Title: "second", Repos: []string{"explicit"}},
					},
				},
			},
		}

		applyRegisteredRepoFallback(fsm)

		require.Equal(t, []string{"only-repo"}, fsm.ExtendedState.Plan.ChosenRepos)
		require.Equal(t, []string{"only-repo"}, fsm.ExtendedState.Plan.Subtasks[0].Repos)
		require.Equal(t, []string{"explicit"}, fsm.ExtendedState.Plan.Subtasks[1].Repos,
			"explicit subtask repos must be preserved")
	})

	t.Run("noop when ChosenRepos already populated", func(t *testing.T) {
		mgr := workspace.NewManager(nil, []workspace.RepoSpec{
			{Slug: "only-repo", URL: "https://example.test/only.git"},
		})
		fsm := &ContextMatrixOrchestrator{
			Context: &Context{Workspace: mgr},
			ExtendedState: &ExtendedState{
				Plan: &Plan{
					ChosenRepos: []string{"agent-chosen"},
					Subtasks:    []Subtask{{Title: "first"}},
				},
			},
		}

		applyRegisteredRepoFallback(fsm)

		require.Equal(t, []string{"agent-chosen"}, fsm.ExtendedState.Plan.ChosenRepos)
		// Note: planFromPayload's per-subtask fallback would have copied
		// agent-chosen onto Subtasks[0].Repos before this fallback runs;
		// applyRegisteredRepoFallback itself doesn't touch subtasks when
		// ChosenRepos is already set.
	})

	t.Run("noop when registry has multiple repos", func(t *testing.T) {
		mgr := workspace.NewManager(nil, []workspace.RepoSpec{
			{Slug: "repo-a", URL: "https://example.test/a.git"},
			{Slug: "repo-b", URL: "https://example.test/b.git"},
		})
		fsm := &ContextMatrixOrchestrator{
			Context: &Context{Workspace: mgr},
			ExtendedState: &ExtendedState{
				Plan: &Plan{
					Subtasks: []Subtask{{Title: "ambiguous"}},
				},
			},
		}

		applyRegisteredRepoFallback(fsm)

		// With two repos, the orchestrator can't safely guess; leave
		// empty so AllRemainingBlocked surfaces the genuine failure.
		require.Empty(t, fsm.ExtendedState.Plan.ChosenRepos)
		require.Empty(t, fsm.ExtendedState.Plan.Subtasks[0].Repos)
	})

	t.Run("noop when no Workspace wired", func(t *testing.T) {
		fsm := &ContextMatrixOrchestrator{
			Context: &Context{Workspace: nil},
			ExtendedState: &ExtendedState{
				Plan: &Plan{Subtasks: []Subtask{{Title: "no-ws"}}},
			},
		}

		applyRegisteredRepoFallback(fsm)

		require.Empty(t, fsm.ExtendedState.Plan.ChosenRepos)
	})
}

// TestApplyPlanCompletePayloadTolerantOfStringRepos guards the
// HITL-mode plan_complete tool_use parser against the same drift mode
// fixed in TestParseTypedPlanTolerantOfStringRepos: real Opus calling
// the tool with `chosen_repos`, `repos`, or `depends_on` as bare
// strings instead of arrays. Strict parsing crashed the FSM mid-plan
// in production (see HOMELAND-001 incident); the runner now coerces.
func TestApplyPlanCompletePayloadTolerantOfStringRepos(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"

	raw := `{
  "card_id": "ABC-1",
  "plan_summary": "single-repo card",
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
}`

	marker := claudeclient.Marker{Kind: claudeclient.MarkerPlanComplete, Raw: raw}

	require.NoError(t, fsm.applyPlanCompletePayload(context.Background(), marker))
	require.NotNil(t, fsm.ExtendedState.Plan)

	plan := fsm.ExtendedState.Plan
	require.Equal(t, "single-repo card", plan.Summary)
	require.Equal(t, []string{"auth-svc"}, plan.ChosenRepos)
	require.Len(t, plan.Subtasks, 1)
	require.Equal(t, []string{"auth-svc"}, plan.Subtasks[0].Repos)
	require.Equal(t, []string{"OTHER-9"}, plan.Subtasks[0].DependsOn)
}

// TestApplyPlanCompletePayloadTolerantOfStringSubtasks covers a second
// observed real-Opus drift mode (post-HOMELAND-001 follow-up incident):
// the entire `subtasks` field comes back as a JSON-string-encoded
// array instead of an array of objects. Strict parsing crashed the
// FSM mid-plan again; the runner now unwraps one level.
func TestApplyPlanCompletePayloadTolerantOfStringSubtasks(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"

	innerSubtasks := `[
	  {"title":"Implement signer","description":"Create jwt.go.","repos":["auth-svc"],"priority":"high","depends_on":[]},
	  {"title":"Wire login handler","description":"Hook signer.","repos":["auth-svc"],"priority":"medium","depends_on":["OTHER-9"]}
	]`
	encodedSubtasks, err := json.Marshal(innerSubtasks)
	require.NoError(t, err)

	raw := fmt.Sprintf(`{
  "card_id": "ABC-1",
  "plan_summary": "Subtasks came back string-encoded.",
  "chosen_repos": ["auth-svc"],
  "subtasks": %s
}`, string(encodedSubtasks))

	marker := claudeclient.Marker{Kind: claudeclient.MarkerPlanComplete, Raw: raw}

	require.NoError(t, fsm.applyPlanCompletePayload(context.Background(), marker))
	require.NotNil(t, fsm.ExtendedState.Plan)

	plan := fsm.ExtendedState.Plan
	require.Equal(t, "Subtasks came back string-encoded.", plan.Summary)
	require.Equal(t, []string{"auth-svc"}, plan.ChosenRepos)
	require.Len(t, plan.Subtasks, 2)
	require.Equal(t, "Implement signer", plan.Subtasks[0].Title)
	require.Equal(t, []string{"auth-svc"}, plan.Subtasks[0].Repos)
	require.Equal(t, "Wire login handler", plan.Subtasks[1].Title)
	require.Equal(t, []string{"OTHER-9"}, plan.Subtasks[1].DependsOn)
}

// TestApplyPlanCompletePayloadLogsRawOnParseFailure verifies that when
// the parser truly cannot recover, the raw payload is included in the
// error so we can diagnose the next drift mode without instrumenting
// production. Without this, total_bytes alone in stream-json previews
// is useless.
func TestApplyPlanCompletePayloadLogsRawOnParseFailure(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"

	// `subtasks: 42` is non-recoverable (not array, not string).
	raw := `{"card_id":"ABC-1","plan_summary":"x","chosen_repos":[],"subtasks":42}`

	marker := claudeclient.Marker{Kind: claudeclient.MarkerPlanComplete, Raw: raw}

	err := fsm.applyPlanCompletePayload(context.Background(), marker)
	require.Error(t, err)
	require.Contains(t, err.Error(), "plan: parse plan json")
	require.Contains(t, err.Error(), `"subtasks":42`,
		"raw payload must appear in error so the next drift mode is diagnosable")
}

// TestApplyPlanCompletePayloadTolerantOfNullRepos covers agents that
// emit explicit JSON null for optional list fields. Treat null as
// absent rather than failing.
func TestApplyPlanCompletePayloadTolerantOfNullRepos(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"

	raw := `{
  "card_id": "ABC-1",
  "plan_summary": "pure-spec card",
  "chosen_repos": null,
  "subtasks": [
    {
      "title": "Spec only",
      "description": "Just docs.",
      "repos": null,
      "priority": "low",
      "depends_on": null
    }
  ]
}`

	marker := claudeclient.Marker{Kind: claudeclient.MarkerPlanComplete, Raw: raw}

	require.NoError(t, fsm.applyPlanCompletePayload(context.Background(), marker))
	require.NotNil(t, fsm.ExtendedState.Plan)

	plan := fsm.ExtendedState.Plan
	require.Empty(t, plan.ChosenRepos)
	require.Len(t, plan.Subtasks, 1)
	require.Empty(t, plan.Subtasks[0].Repos)
	require.Empty(t, plan.Subtasks[0].DependsOn)
}

// TestApplyPlanCompletePayloadReadsCardBody verifies the new card-body
// path: the runner fetches the card via MCP, extracts the fenced JSON
// from the ## Plan section, and parses it. The marker.Raw is now a
// thin signal (just card_id) and is no longer the source of plan data.
func TestApplyPlanCompletePayloadReadsCardBody(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"

	body := "## Plan\n\nTwo-step plan.\n\n### Subtasks\n\n1. **Sign**\n\n```json\n" + `{
  "plan_summary": "two-step",
  "chosen_repos": ["auth-svc"],
  "subtasks": [
    {"title": "Sign", "description": "Create jwt.go.", "repos": ["auth-svc"], "priority": "high", "depends_on": []}
  ]
}` + "\n```\n"

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)

	mock.cardBody = body

	marker := claudeclient.Marker{Kind: claudeclient.MarkerPlanComplete, Raw: `{"card_id":"ABC-1"}`}

	require.NoError(t, fsm.applyPlanCompletePayload(context.Background(), marker))
	require.NotNil(t, fsm.ExtendedState.Plan)
	require.Equal(t, "two-step", fsm.ExtendedState.Plan.Summary)
	require.Equal(t, []string{"auth-svc"}, fsm.ExtendedState.Plan.ChosenRepos)
	require.Len(t, fsm.ExtendedState.Plan.Subtasks, 1)
	require.Equal(t, "Sign", fsm.ExtendedState.Plan.Subtasks[0].Title)
}

// TestApplyPlanCompletePayloadCardBodyMissingPlan returns an error so
// the operator can fix the agent's missing block. Without this, a thin
// signal with no card-body data would silently produce an empty plan.
func TestApplyPlanCompletePayloadCardBodyMissingPlan(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)

	mock.cardBody = "## Plan\n\nProse only, no JSON.\n"

	// Marker carries no plan data either — both sources are empty, so
	// we expect a clear error.
	marker := claudeclient.Marker{Kind: claudeclient.MarkerPlanComplete, Raw: ""}

	err := fsm.applyPlanCompletePayload(context.Background(), marker)
	require.Error(t, err)
	require.Contains(t, err.Error(), "## Plan")
}

// TestApplyPlanCompletePayloadFallsBackToToolInput preserves backwards
// compat for transitional cases. If the card body has no plan JSON but
// the tool input does, parse from the tool input.
func TestApplyPlanCompletePayloadFallsBackToToolInput(t *testing.T) {
	fsm := newTestFSM(t)
	fsm.ExtendedState.Project = "p1"
	fsm.ExtendedState.CardID = "ABC-1"
	fsm.ExtendedState.AgentID = "agent:test"

	mock, ok := fsm.Context.MCP.(*mockMCP)
	require.True(t, ok)

	mock.cardBody = ""

	raw := `{"card_id":"ABC-1","plan_summary":"from-tool","chosen_repos":["x"],"subtasks":[{"title":"t","repos":["x"],"priority":"high","depends_on":[]}]}`

	marker := claudeclient.Marker{Kind: claudeclient.MarkerPlanComplete, Raw: raw}
	require.NoError(t, fsm.applyPlanCompletePayload(context.Background(), marker))
	require.Equal(t, "from-tool", fsm.ExtendedState.Plan.Summary)
}

// TestSkillInAllowedToolsForSkillPhases verifies that each of the five
// phase sites that mention skills in their prompts (execute, review
// autonomous, review HITL, document, diagnose) include "Skill" in their
// mcpAllowedTools call. The three phases that do NOT mention skills
// (brainstorm, plan autonomous, plan HITL) must NOT have "Skill" added.
//
// This is a source-level check: it reads actions.go, extracts the body
// of each named function (or the chatLoopConfig block for HITL phases),
// and asserts the "Skill" substring appears/is absent as required. The
// approach avoids the full invocation overhead while still catching
// accidental regressions in the tool list.
func TestSkillInAllowedToolsForSkillPhases(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("actions.go")
	require.NoError(t, err, "read actions.go")

	content := string(src)

	// extractFuncBody returns the substring of content starting at the
	// first occurrence of funcMarker and extending to the matching closing
	// brace. It is a simple heuristic: it counts '{' and '}' characters
	// from the marker position to find the function boundary.
	extractFuncBody := func(funcMarker string) string {
		start := strings.Index(content, funcMarker)
		if start < 0 {
			return ""
		}

		depth := 0
		began := false

		for i := start; i < len(content); i++ {
			switch content[i] {
			case '{':
				depth++
				began = true
			case '}':
				depth--
			}

			if began && depth == 0 {
				return content[start : i+1]
			}
		}

		return content[start:]
	}

	phasesWithSkill := []struct {
		phase  string
		marker string
	}{
		{"RunDiagnosisPhaseAction", "func (fsm *ContextMatrixOrchestrator) RunDiagnosisPhaseAction"},
		{"RunDocumentPhaseAction", "func (fsm *ContextMatrixOrchestrator) RunDocumentPhaseAction"},
		{"RunExecutePhaseParallelAction", "func (fsm *ContextMatrixOrchestrator) RunExecutePhaseParallelAction"},
		// autonomous review lives inside runReviewPhaseAutonomous
		{"runReviewPhaseAutonomous (review autonomous)", "func (fsm *ContextMatrixOrchestrator) runReviewPhaseAutonomous"},
		// HITL review uses runReviewPhaseHITL
		{"runReviewPhaseHITL (review HITL)", "func (fsm *ContextMatrixOrchestrator) runReviewPhaseHITL"},
	}

	for _, tc := range phasesWithSkill {
		t.Run(tc.phase, func(t *testing.T) {
			t.Parallel()

			body := extractFuncBody(tc.marker)
			require.NotEmpty(t, body, "function body not found: %s", tc.marker)
			require.Contains(t, body, `"Skill"`,
				"phase %q must include Skill in its mcpAllowedTools call", tc.phase)
		})
	}

	phasesWithoutSkill := []struct {
		phase  string
		marker string
	}{
		{"RunBrainstormingDialogueAction (brainstorm)", "func (fsm *ContextMatrixOrchestrator) RunBrainstormingDialogueAction"},
		{"RunPlanPhaseAction (plan autonomous)", "func (fsm *ContextMatrixOrchestrator) RunPlanPhaseAction"},
		{"runPlanPhaseHITL (plan HITL)", "func (fsm *ContextMatrixOrchestrator) runPlanPhaseHITL"},
	}

	for _, tc := range phasesWithoutSkill {
		t.Run(tc.phase, func(t *testing.T) {
			t.Parallel()

			body := extractFuncBody(tc.marker)
			require.NotEmpty(t, body, "function body not found: %s", tc.marker)
			require.NotContains(t, body, `"Skill"`,
				"phase %q must NOT include Skill in its mcpAllowedTools call", tc.phase)
		})
	}
}
