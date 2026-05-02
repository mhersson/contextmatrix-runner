package sessions

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
)

// Registry holds active sessions for a single card, keyed by purpose.
type Registry struct {
	cardID string

	mu       sync.Mutex
	sessions map[string]*sessionImpl
}

// NewRegistry constructs an empty Registry for the given card.
func NewRegistry(cardID string) *Registry {
	return &Registry{cardID: cardID, sessions: make(map[string]*sessionImpl)}
}

// Acquire constructs or refreshes a session for the given purpose,
// applying tier 1 -> 2 -> 3 fallback. Tier 1 reuses a live cached
// session if it responds to a probe; tier 2 spawns CC with --resume
// using the persisted session_id; tier 3 spawns a fresh CC with the
// primer text appended to the system prompt.
func (r *Registry) Acquire(
	ctx context.Context,
	purpose string,
	opts AcquireOptions,
	spawner Spawner,
	cards CardStore,
	logger *slog.Logger,
) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Tier 1: live cached.
	if s := r.tier1(ctx, purpose); s != nil {
		return s, nil
	}

	// Tier 2: resume by stored session_id.
	if s, err := r.tier2(ctx, purpose, opts, spawner, cards, logger); err == nil && s != nil {
		return s, nil
	}

	// Tier 3: primer fallback.
	return r.tier3(ctx, purpose, opts, spawner, cards, logger)
}

// tier1 returns an existing live session if it still responds, or nil.
// Sessions in StateActive or StateIdle are eligible — idle ones revive
// to active on reuse. Unresponsive sessions are killed and removed
// before returning nil so the caller falls through to tier-2 cleanly.
func (r *Registry) tier1(ctx context.Context, purpose string) *sessionImpl {
	existing, ok := r.sessions[purpose]
	if !ok {
		return nil
	}

	state := existing.getState()
	if state != StateActive && state != StateIdle {
		return nil
	}

	if probeAlive(ctx, existing.cc) {
		existing.setState(StateActive)

		return existing
	}

	_ = existing.cc.Kill()
	existing.setState(StateDead)

	delete(r.sessions, purpose)

	return nil
}

// tier2 attempts to spawn CC with --resume using a persisted session_id.
// Returns (nil, nil) if no session_id is persisted, (session, nil) on
// success, or (nil, err) when resume spawn fails so the caller can fall
// through to tier-3.
func (r *Registry) tier2(
	ctx context.Context,
	purpose string,
	opts AcquireOptions,
	spawner Spawner,
	cards CardStore,
	logger *slog.Logger,
) (*sessionImpl, error) {
	storedID, err := cards.GetSessionID(ctx, r.cardID, purpose)
	if err != nil {
		logger.Warn("session card-store lookup failed; falling through to tier-3",
			"card", r.cardID, "purpose", purpose, "err", err)

		return nil, err
	}

	if storedID == "" {
		return nil, nil
	}

	proc, err := spawner.Spawn(ctx, claudeclient.SpawnOptions{
		Container:    opts.Container,
		SystemPrompt: opts.SystemPrompt,
		Model:        opts.Model,
		AllowedTools: opts.AllowedTools,
		Resume:       storedID,
	})
	if err != nil {
		logger.Warn("session resume failed, falling back to primer",
			"card", r.cardID, "purpose", purpose, "session_id", storedID, "err", err)

		return nil, err
	}

	s := r.adopt(purpose, proc, logger)

	if id := sessionIDFromProcess(proc); id != "" {
		_ = cards.SaveSessionID(ctx, r.cardID, purpose, id)
	}

	return s, nil
}

// tier3 spawns a fresh CC with the primer text appended to the system
// prompt. Returns the resulting session or wraps the spawn error.
func (r *Registry) tier3(
	ctx context.Context,
	purpose string,
	opts AcquireOptions,
	spawner Spawner,
	cards CardStore,
	logger *slog.Logger,
) (Session, error) {
	primerPrompt := opts.SystemPrompt
	if opts.Primer != "" {
		primerPrompt = primerPrompt + "\n\nResuming from saved state:\n\n" + opts.Primer
	}

	proc, err := spawner.Spawn(ctx, claudeclient.SpawnOptions{
		Container:    opts.Container,
		SystemPrompt: primerPrompt,
		Model:        opts.Model,
		AllowedTools: opts.AllowedTools,
	})
	if err != nil {
		return nil, fmt.Errorf("acquire %s/%s: tier-3 spawn: %w", r.cardID, purpose, err)
	}

	s := r.adopt(purpose, proc, logger)

	if id := sessionIDFromProcess(proc); id != "" {
		_ = cards.SaveSessionID(ctx, r.cardID, purpose, id)
	}

	return s, nil
}

// adopt wraps a freshly spawned process in a sessionImpl, marks it
// active, and stores it under purpose. Caller must hold r.mu.
func (r *Registry) adopt(purpose string, proc claudeclient.Process, logger *slog.Logger) *sessionImpl {
	s := &sessionImpl{
		purpose: purpose,
		cardID:  r.cardID,
		cc:      proc,
		state:   StateActive,
		logger:  logger,
	}
	r.sessions[purpose] = s

	return s
}

// Release marks the session as idle. The session remains in the registry
// and can still be revived via tier-1 on a subsequent Acquire.
func (r *Registry) Release(purpose string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if s, ok := r.sessions[purpose]; ok {
		s.setState(StateIdle)
	}
}

// Terminate closes a session and clears its persisted session_id.
func (r *Registry) Terminate(ctx context.Context, purpose, reason string, cards CardStore) error {
	r.mu.Lock()
	s := r.sessions[purpose]
	delete(r.sessions, purpose)
	r.mu.Unlock()

	if s != nil {
		_ = s.Close(ctx, reason)
	}

	return cards.ClearSessionID(ctx, r.cardID, purpose)
}

// TerminateAll closes every session for the card and clears every
// persisted session_id. Called by Manager.TerminateAll when the card
// reaches a terminal status.
func (r *Registry) TerminateAll(ctx context.Context, cards CardStore) {
	r.mu.Lock()

	purposes := make([]string, 0, len(r.sessions))
	for p, s := range r.sessions {
		purposes = append(purposes, p)
		_ = s.Close(ctx, "card terminal")
	}

	r.sessions = make(map[string]*sessionImpl)
	r.mu.Unlock()

	for _, p := range purposes {
		_ = cards.ClearSessionID(ctx, r.cardID, p)
	}
}
