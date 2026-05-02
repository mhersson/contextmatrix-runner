// Package sessions owns long-lived Claude Code processes per
// (cardID, purpose) pair. It provides three-tier acquisition (live cache,
// resume by session_id, primer fallback) and per-card lifecycle
// management.
//
// The package is currently unwired from the orchestrator FSM — HITL
// turns spawn `claude --resume` per-message instead of holding a
// long-lived process — but is kept as the named substrate for the
// future contextmatrix-chat (Spec 3) cross-project chat surface. See
// the runner-orchestration design (§2.1, §3.1, §13) for the contract:
// "stateful sessions keyed on user/conversation instead of card/purpose"
// is exactly the second consumer this primitive is shaped for. Do not
// delete during cleanup passes; see CONTRIBUTING/ROADMAP for status.
package sessions

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
)

// Spawner abstracts the CC-spawning capability that the manager
// depends on. In production this is *claudeclient.Wrapper.
type Spawner interface {
	Spawn(ctx context.Context, opts claudeclient.SpawnOptions) (claudeclient.Process, error)
}

// CardStore persists per-card session_id metadata for tier-2 resume
// across runner restarts. In production this is a thin facade over
// MCP update_card / get_task_context.
type CardStore interface {
	GetSessionID(ctx context.Context, cardID, purpose string) (string, error)
	SaveSessionID(ctx context.Context, cardID, purpose, sessionID string) error
	ClearSessionID(ctx context.Context, cardID, purpose string) error
}

// Manager owns per-card session registries.
type Manager struct {
	spawner Spawner
	cards   CardStore
	logger  *slog.Logger

	mu         sync.Mutex
	registries map[string]*Registry
}

// NewManager constructs a Manager. A nil logger is replaced with slog.Default().
func NewManager(spawner Spawner, cards CardStore, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}

	return &Manager{
		spawner:    spawner,
		cards:      cards,
		logger:     logger,
		registries: make(map[string]*Registry),
	}
}

// AcquireOptions configures a session acquisition.
type AcquireOptions struct {
	SystemPrompt string
	Model        string
	AllowedTools []string
	Container    string // worker container name
	Primer       string // tier-3 fallback content
}

// Acquire returns a session for (cardID, purpose), constructing one
// via the appropriate tier (live -> resume -> primer).
func (m *Manager) Acquire(ctx context.Context, cardID, purpose string, opts AcquireOptions) (Session, error) {
	reg := m.registry(cardID)

	return reg.Acquire(ctx, purpose, opts, m.spawner, m.cards, m.logger)
}

// Release marks the session as idle but keeps it in the registry.
func (m *Manager) Release(cardID, purpose string) {
	reg := m.registry(cardID)
	reg.Release(purpose)
}

// Terminate gracefully ends a session.
func (m *Manager) Terminate(ctx context.Context, cardID, purpose, reason string) error {
	reg := m.registry(cardID)

	return reg.Terminate(ctx, purpose, reason, m.cards)
}

// TerminateAll ends every session for a card. Called on card terminal status.
func (m *Manager) TerminateAll(ctx context.Context, cardID string) {
	m.mu.Lock()
	reg := m.registries[cardID]
	delete(m.registries, cardID)
	m.mu.Unlock()

	if reg == nil {
		return
	}

	reg.TerminateAll(ctx, m.cards)
}

// registry returns the Registry for cardID, creating one if absent.
func (m *Manager) registry(cardID string) *Registry {
	m.mu.Lock()
	defer m.mu.Unlock()

	r := m.registries[cardID]
	if r == nil {
		r = NewRegistry(cardID)
		m.registries[cardID] = r
	}

	return r
}

// ----- Tier helpers -----

// probeAlive sends a no-op stream-json frame and treats a non-error send
// as evidence the process is still receiving stdin. CC may not recognize
// the "ping" type, but a successful write proves the pipe is intact;
// once Kill closes stdin SendMessage returns an error and we bail. This
// is approximate by design — the reaper is the durable safety net.
func probeAlive(ctx context.Context, p claudeclient.Process) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	if err := p.SendMessage(probeCtx, claudeclient.StreamMessage{Type: "ping"}); err != nil {
		return false
	}

	return true
}

// sessionIDFromProcess waits up to 500ms for the pump goroutine to
// observe system_init and populate the session_id; returns whatever is
// set (possibly "") at the deadline.
func sessionIDFromProcess(p claudeclient.Process) string {
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if id := p.SessionID(); id != "" {
			return id
		}

		time.Sleep(10 * time.Millisecond)
	}

	return p.SessionID()
}
