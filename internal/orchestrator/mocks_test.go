package orchestrator

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
)

type mockMCP struct {
	mu                sync.Mutex
	claimCalls        []string
	cardFieldUpdates  map[string]any
	bodyUpdates       map[string]string
	cardBody          string
	reportUsageCalls  int
	addLogCalls       []string
	addLogMessages    []string
	transitionCalls   []string
	completeTaskCalls int
	releaseCalls      int
	reportPushCalls   int
	createCardCalls   int
	createCardInputs  []CreateCardInput
}

func (m *mockMCP) ClaimCard(_ context.Context, _, cardID, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.claimCalls = append(m.claimCalls, cardID)

	return nil
}

func (m *mockMCP) GetTaskContext(_ context.Context, project, cardID, _ string) (*CardContext, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return &CardContext{Card: &Card{ID: cardID, Project: project, Body: m.cardBody}}, nil
}

func (m *mockMCP) UpdateCardBody(_ context.Context, _, _, sectionName, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.bodyUpdates == nil {
		m.bodyUpdates = make(map[string]string)
	}

	m.bodyUpdates[sectionName] = content

	return nil
}

func (m *mockMCP) UpdateCardField(_ context.Context, _, _ string, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cardFieldUpdates == nil {
		m.cardFieldUpdates = make(map[string]any)
	}

	for k, v := range fields {
		m.cardFieldUpdates[k] = v
	}

	return nil
}

func (m *mockMCP) Heartbeat(_ context.Context, _, _, _ string) error { return nil }

func (m *mockMCP) CompleteTask(_ context.Context, _, _, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.completeTaskCalls++

	return nil
}

func (m *mockMCP) ReleaseCard(_ context.Context, _, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.releaseCalls++

	return nil
}

func (m *mockMCP) AddLog(_ context.Context, _, _, _, action, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.addLogCalls = append(m.addLogCalls, action)
	m.addLogMessages = append(m.addLogMessages, message)

	return nil
}

func (m *mockMCP) ReportUsage(_ context.Context, _, _, _ string, _, _ int, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.reportUsageCalls++

	return nil
}

func (m *mockMCP) ReportPush(_ context.Context, _, _, _, _, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.reportPushCalls++

	return nil
}

func (m *mockMCP) TransitionCard(_ context.Context, _, _, _, toState string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.transitionCalls = append(m.transitionCalls, toState)

	return nil
}

func (m *mockMCP) CreateCard(_ context.Context, _ string, in CreateCardInput) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.createCardCalls++
	m.createCardInputs = append(m.createCardInputs, in)

	return fmt.Sprintf("SUB-%d", m.createCardCalls), nil
}

func (m *mockMCP) GetProjectKB(_ context.Context, _ string, _ ...string) (ProjectKB, error) {
	return ProjectKB{}, nil
}

type mockGitTokens struct {
	mu        sync.Mutex
	mintCalls int
}

func (m *mockGitTokens) GenerateToken(_ context.Context) (string, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.mintCalls++

	return "stub-token", time.Time{}, nil
}

// fakeExecAPI implements claudeclient.ExecAPI for orchestrator tests.
// Tests drive a fake CC stdout by writing JSON-per-line frames to
// stdoutWrite; the wrapper reads them via stdoutRead.
type fakeExecAPI struct {
	mu          sync.Mutex
	lastCmd     []string
	lastEnv     []string
	stdoutWrite *io.PipeWriter
	stdoutRead  *io.PipeReader
}

func newFakeExecAPI() *fakeExecAPI {
	pr, pw := io.Pipe()

	return &fakeExecAPI{stdoutWrite: pw, stdoutRead: pr}
}

func (f *fakeExecAPI) ExecCreate(_ context.Context, _ string, cfg claudeclient.ExecConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastCmd = cfg.Cmd
	f.lastEnv = cfg.Env

	return "exec-1", nil
}

func (f *fakeExecAPI) ExecAttach(_ context.Context, _ string) (io.ReadCloser, io.WriteCloser, error) {
	return io.NopCloser(f.stdoutRead), discardCloser{}, nil
}

// discardCloser swallows all writes — used as a fake stdin for CC.
type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardCloser) Close() error                { return nil }

// multiTurnFakeExecAPI implements claudeclient.ExecAPI with one fresh
// pipe per ExecAttach call so chat-loop tests that need multiple turns
// can supply distinct stream-json output for each spawn. Tests preload
// `turns` with one byte slice per expected attach; ExecAttach pops the
// next entry, writes it on a goroutine, and closes the writer to mark
// EOF for that turn. Out-of-turns attaches return an immediately-closed
// pipe (EOF) so the consumer's `for ev := range proc.Output()` loop
// drains and exits cleanly.
type multiTurnFakeExecAPI struct {
	mu       sync.Mutex
	turns    [][]byte
	attaches int
	lastCmd  []string
}

func newMultiTurnFakeExecAPI(turns ...[]byte) *multiTurnFakeExecAPI {
	return &multiTurnFakeExecAPI{turns: turns}
}

func (f *multiTurnFakeExecAPI) ExecCreate(_ context.Context, _ string, cfg claudeclient.ExecConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastCmd = cfg.Cmd

	return fmt.Sprintf("exec-%d", f.attaches+1), nil
}

func (f *multiTurnFakeExecAPI) ExecAttach(_ context.Context, _ string) (io.ReadCloser, io.WriteCloser, error) {
	f.mu.Lock()

	idx := f.attaches
	f.attaches++

	var data []byte
	if idx < len(f.turns) {
		data = f.turns[idx]
	}

	f.mu.Unlock()

	pr, pw := io.Pipe()

	go func() {
		if len(data) > 0 {
			_, _ = pw.Write(data)
		}

		_ = pw.Close()
	}()

	return io.NopCloser(pr), discardCloser{}, nil
}
