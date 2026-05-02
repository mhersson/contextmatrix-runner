package driver

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/events"
	"github.com/mhersson/contextmatrix-runner/internal/orchestrator"
)

// fakeSSE is a controllable SSESubscriber used by tests.
type fakeSSE struct {
	ev   chan events.RunnerEvent
	errs chan error
}

func newFakeSSE() *fakeSSE {
	return &fakeSSE{
		ev:   make(chan events.RunnerEvent, 32),
		errs: make(chan error, 16),
	}
}

func (f *fakeSSE) Subscribe(_ context.Context) (<-chan events.RunnerEvent, <-chan error) {
	return f.ev, f.errs
}

func (f *fakeSSE) Inject(ev events.RunnerEvent) { f.ev <- ev }

// fakeMCP records calls for the driver tests.
type fakeMCP struct {
	mu         sync.Mutex
	fields     map[string]any
	heartbeats int32
}

func (m *fakeMCP) ClaimCard(_ context.Context, _, _, _ string) error { return nil }

func (m *fakeMCP) GetTaskContext(_ context.Context, _, cardID, _ string) (*orchestrator.CardContext, error) {
	return &orchestrator.CardContext{Card: &orchestrator.Card{ID: cardID}}, nil
}

func (m *fakeMCP) UpdateCardBody(_ context.Context, _, _, _, _ string) error { return nil }

func (m *fakeMCP) UpdateCardField(_ context.Context, _, _ string, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.fields == nil {
		m.fields = make(map[string]any)
	}

	for k, v := range fields {
		m.fields[k] = v
	}

	return nil
}

func (m *fakeMCP) Heartbeat(_ context.Context, _, _, _ string) error {
	atomic.AddInt32(&m.heartbeats, 1)

	return nil
}

func (m *fakeMCP) CompleteTask(_ context.Context, _, _, _, _ string) error { return nil }
func (m *fakeMCP) ReleaseCard(_ context.Context, _, _, _ string) error     { return nil }
func (m *fakeMCP) AddLog(_ context.Context, _, _, _, _, _ string) error    { return nil }

func (m *fakeMCP) ReportUsage(_ context.Context, _, _, _ string, _, _ int, _ string) error {
	return nil
}

func (m *fakeMCP) ReportPush(_ context.Context, _, _, _, _, _, _ string) error { return nil }
func (m *fakeMCP) TransitionCard(_ context.Context, _, _, _, _ string) error   { return nil }

func (m *fakeMCP) CreateCard(_ context.Context, _ string, _ orchestrator.CreateCardInput) (string, error) {
	return "SUB-1", nil
}

func (m *fakeMCP) GetProjectKB(_ context.Context, _ string, _ ...string) (orchestrator.ProjectKB, error) {
	return orchestrator.ProjectKB{}, nil
}

// fakeGitTokens is a no-op githubauth.TokenGenerator for the driver tests.
type fakeGitTokens struct{}

func (fakeGitTokens) GenerateToken(_ context.Context) (string, time.Time, error) {
	return "", time.Time{}, nil
}

func TestDriverDispatchesChatInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sse := newFakeSSE()
	mcp := &fakeMCP{}

	d := New(Config{
		Project:           "p1",
		CardID:            "C-1",
		AgentID:           "agent",
		MCP:               mcp,
		GitTokens:         fakeGitTokens{},
		SSE:               sse,
		HeartbeatInterval: 100 * time.Millisecond,
	})

	ext := &orchestrator.ExtendedState{
		RunCtx:      ctx,
		ChatInputCh: make(chan string, 4),
	}
	go d.dispatchEvents(ctx, sse.ev, sse.errs, ext)

	sse.Inject(events.RunnerEvent{Type: "chat_input", Data: "hello"})

	select {
	case msg := <-ext.ChatInputCh:
		require.Equal(t, "hello", msg)
	case <-ctx.Done():
		t.Fatal("chat input not dispatched")
	}
}

func TestDriverPromotionFlipsModeAndPushesChat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sse := newFakeSSE()

	d := New(Config{
		Project: "p1",
		CardID:  "C-1",
		AgentID: "agent",
		SSE:     sse,
	})

	ext := &orchestrator.ExtendedState{
		RunCtx:      ctx,
		ChatInputCh: make(chan string, 4),
	}
	ext.StoreMode(orchestrator.ModeHITL)

	go d.dispatchEvents(ctx, sse.ev, sse.errs, ext)

	sse.Inject(events.RunnerEvent{Type: "promotion", Data: "{}"})

	select {
	case chat := <-ext.ChatInputCh:
		require.Contains(t, chat, "autonomous")
		// Every HITL chat-loop phase gets a valid terminal-marker option
		// in the canned message — brainstorming was missing from the
		// original message and would hang the loop on promotion.
		require.Contains(t, chat, "discovery_complete")
		require.Contains(t, chat, "plan_complete")
		require.Contains(t, chat, "review_approve")
		require.Contains(t, chat, "review_revise")
	case <-ctx.Done():
		t.Fatal("promotion did not push canned chat")
	}

	require.Equal(t, orchestrator.ModeAutonomous, ext.LoadMode(),
		"promotion must flip Mode to autonomous")
}

func TestDriverStopCancelsContext(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sse := newFakeSSE()
	d := New(Config{SSE: sse})

	ctx, cancelInner := context.WithCancel(parent)
	defer cancelInner()

	ext := &orchestrator.ExtendedState{
		RunCtx:    ctx,
		RunCancel: cancelInner,
		StopCh:    make(chan struct{}, 1),
	}
	go d.dispatchEvents(ctx, sse.ev, sse.errs, ext)

	sse.Inject(events.RunnerEvent{Type: "stop"})

	require.Eventually(t, func() bool {
		select {
		case <-ctx.Done():
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestDriverHeartbeat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mcp := &fakeMCP{}
	d := New(Config{
		Project:           "p1",
		CardID:            "C-1",
		AgentID:           "agent",
		MCP:               mcp,
		HeartbeatInterval: 30 * time.Millisecond,
	})

	go d.heartbeat(ctx)

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&mcp.heartbeats) >= 2
	}, time.Second, 10*time.Millisecond)
}
