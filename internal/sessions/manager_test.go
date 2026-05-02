package sessions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
)

// stubProcess implements claudeclient.Process for tests. Kill is
// idempotent via closeOnce so repeated termination (Close + reaper) is
// safe.
type stubProcess struct {
	sessionID string
	out       chan claudeclient.StreamEvent
	closeOnce sync.Once
}

func newStubProcess(sessionID string) *stubProcess {
	out := make(chan claudeclient.StreamEvent, 4)
	out <- claudeclient.StreamEvent{Kind: claudeclient.EventSystemInit, SessionID: sessionID}

	return &stubProcess{sessionID: sessionID, out: out}
}

func (s *stubProcess) SendMessage(_ context.Context, _ claudeclient.StreamMessage) error {
	return nil
}

func (s *stubProcess) CloseStdin() error { return nil }

func (s *stubProcess) Output() <-chan claudeclient.StreamEvent { return s.out }

func (s *stubProcess) SessionID() string { return s.sessionID }

func (s *stubProcess) Wait(_ context.Context) (claudeclient.Usage, error) {
	return claudeclient.Usage{}, nil
}

func (s *stubProcess) Kill() error {
	s.closeOnce.Do(func() { close(s.out) })

	return nil
}

// stubSpawner counts calls and records the most recent SpawnOptions so
// tests can assert tier-2 passed Resume through to the spawner.
type stubSpawner struct {
	mu        sync.Mutex
	callCount int
	nextID    int
	lastOpts  claudeclient.SpawnOptions
}

func (s *stubSpawner) Spawn(_ context.Context, opts claudeclient.SpawnOptions) (claudeclient.Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.callCount++
	s.nextID++
	s.lastOpts = opts

	return newStubProcess(fmt.Sprintf("sess_%d", s.nextID)), nil
}

func (s *stubSpawner) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.callCount
}

// failingResumeSpawner fails the first Spawn call when Resume != "" and
// succeeds afterward, modelling a stale session_id that triggers tier-3.
type failingResumeSpawner struct {
	mu        sync.Mutex
	callCount int
	failed    bool
}

func (s *failingResumeSpawner) Spawn(_ context.Context, opts claudeclient.SpawnOptions) (claudeclient.Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.callCount++

	if opts.Resume != "" && !s.failed {
		s.failed = true

		return nil, errors.New("resume failed: session expired")
	}

	return newStubProcess(fmt.Sprintf("sess_fresh_%d", s.callCount)), nil
}

func (s *failingResumeSpawner) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.callCount
}

// fakeCardStore records sessions persistence calls in-memory.
type fakeCardStore struct {
	mu    sync.Mutex
	saved map[string]map[string]string
}

func (f *fakeCardStore) GetSessionID(_ context.Context, cardID, purpose string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.saved == nil {
		return "", nil
	}

	return f.saved[cardID][purpose], nil
}

func (f *fakeCardStore) SaveSessionID(_ context.Context, cardID, purpose, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.saved == nil {
		f.saved = make(map[string]map[string]string)
	}

	if f.saved[cardID] == nil {
		f.saved[cardID] = make(map[string]string)
	}

	f.saved[cardID][purpose] = sessionID

	return nil
}

func (f *fakeCardStore) ClearSessionID(_ context.Context, cardID, purpose string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.saved != nil && f.saved[cardID] != nil {
		delete(f.saved[cardID], purpose)
	}

	return nil
}

func TestManagerAcquireTier1ReusesLiveSession(t *testing.T) {
	spawner := &stubSpawner{}
	mgr := NewManager(spawner, &fakeCardStore{}, nil)

	s1, err := mgr.Acquire(context.Background(), "card-1", "brainstorm", AcquireOptions{
		SystemPrompt: "be helpful",
		Model:        "claude-sonnet-4-6",
	})
	require.NoError(t, err)

	s2, err := mgr.Acquire(context.Background(), "card-1", "brainstorm", AcquireOptions{})
	require.NoError(t, err)
	require.Equal(t, s1.SessionID(), s2.SessionID(), "tier-1: same session returned")
	require.Equal(t, 1, spawner.Calls(), "tier-1: no new Spawn")
}

func TestManagerAcquireTier2ResumesBySessionID(t *testing.T) {
	spawner := &stubSpawner{}
	cards := &fakeCardStore{saved: map[string]map[string]string{
		"card-1": {"brainstorm": "sess_persisted"},
	}}
	mgr := NewManager(spawner, cards, nil)

	sess, err := mgr.Acquire(context.Background(), "card-1", "brainstorm", AcquireOptions{
		Container: "wc",
	})
	require.NoError(t, err)
	require.Equal(t, 1, spawner.Calls())
	require.NotNil(t, sess)

	spawner.mu.Lock()
	require.Equal(t, "sess_persisted", spawner.lastOpts.Resume, "tier-2 must pass Resume to Spawn")
	spawner.mu.Unlock()
}

func TestManagerAcquireTier3WhenResumeFails(t *testing.T) {
	spawner := &failingResumeSpawner{}
	cards := &fakeCardStore{saved: map[string]map[string]string{
		"card-1": {"brainstorm": "sess_dead"},
	}}
	mgr := NewManager(spawner, cards, nil)

	sess, err := mgr.Acquire(context.Background(), "card-1", "brainstorm", AcquireOptions{
		Container:    "wc",
		SystemPrompt: "be helpful",
		Primer:       "saved design content here",
	})
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 2, spawner.Calls(), "tier-2 attempt + tier-3 fallback")
}

func TestManagerTerminateClearsSessionID(t *testing.T) {
	spawner := &stubSpawner{}
	cards := &fakeCardStore{}
	mgr := NewManager(spawner, cards, nil)

	_, err := mgr.Acquire(context.Background(), "card-1", "brainstorm", AcquireOptions{})
	require.NoError(t, err)

	require.NoError(t, mgr.Terminate(context.Background(), "card-1", "brainstorm", "test"))

	id, _ := cards.GetSessionID(context.Background(), "card-1", "brainstorm")
	require.Empty(t, id, "Terminate must clear persisted session_id")
}

func TestManagerTerminateAllClearsAllPurposes(t *testing.T) {
	spawner := &stubSpawner{}
	cards := &fakeCardStore{}
	mgr := NewManager(spawner, cards, nil)

	_, err := mgr.Acquire(context.Background(), "card-1", "brainstorm", AcquireOptions{})
	require.NoError(t, err)

	_, err = mgr.Acquire(context.Background(), "card-1", "diagnose", AcquireOptions{})
	require.NoError(t, err)

	mgr.TerminateAll(context.Background(), "card-1")

	id1, _ := cards.GetSessionID(context.Background(), "card-1", "brainstorm")
	id2, _ := cards.GetSessionID(context.Background(), "card-1", "diagnose")

	require.Empty(t, id1)
	require.Empty(t, id2)
}

func TestManagerAcquireSeparateCardsAreIsolated(t *testing.T) {
	spawner := &stubSpawner{}
	mgr := NewManager(spawner, &fakeCardStore{}, nil)

	s1, err := mgr.Acquire(context.Background(), "card-1", "brainstorm", AcquireOptions{})
	require.NoError(t, err)

	s2, err := mgr.Acquire(context.Background(), "card-2", "brainstorm", AcquireOptions{})
	require.NoError(t, err)

	require.NotEqual(t, s1.SessionID(), s2.SessionID())
	require.Equal(t, 2, spawner.Calls())
}

// erroringCardStore returns a fixed error from GetSessionID so tests
// can verify that tier-2 logs and falls through to tier-3 instead of
// silently swallowing the error.
type erroringCardStore struct {
	getErr error
}

func (e *erroringCardStore) GetSessionID(_ context.Context, _, _ string) (string, error) {
	return "", e.getErr
}

func (e *erroringCardStore) SaveSessionID(_ context.Context, _, _, _ string) error { return nil }
func (e *erroringCardStore) ClearSessionID(_ context.Context, _, _ string) error   { return nil }

func TestManagerAcquireLogsCardStoreErrorBeforeFallthrough(t *testing.T) {
	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	spawner := &stubSpawner{}
	cards := &erroringCardStore{getErr: errors.New("boom: card-store unreachable")}

	mgr := NewManager(spawner, cards, logger)

	sess, err := mgr.Acquire(context.Background(), "card-1", "brainstorm", AcquireOptions{
		Container: "wc",
	})
	require.NoError(t, err)
	require.NotNil(t, sess, "tier-3 fallback should still produce a session")

	logs := buf.String()
	require.Contains(t, logs, "session card-store lookup failed",
		"tier-2 must log the card-store error before falling through")
	require.Contains(t, logs, "boom: card-store unreachable",
		"tier-2 must include the underlying error in the log line")
}
