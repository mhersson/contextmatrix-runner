package sessions

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
)

// Session is the consumer-facing handle returned by Manager.Acquire.
// Implementations wrap a claudeclient.Process and gate concurrent sends
// with an internal mutex so callers can drive a session from multiple
// goroutines safely.
type Session interface {
	SendMessage(ctx context.Context, msg claudeclient.StreamMessage) error
	NextEvent(ctx context.Context) (claudeclient.StreamEvent, error)
	SessionID() string
	Close(ctx context.Context, reason string) error
	Kill() error
}

// SessionState represents the lifecycle stage of a Session.
type SessionState int

// SessionState values.
const (
	StateIdle SessionState = iota
	StateActive
	StateTerminating
	StateDead
)

// errSessionClosed is returned by NextEvent once the process output
// channel is closed (Kill or natural end of stream).
var errSessionClosed = errors.New("session output channel closed")

// sessionImpl is the concrete Session backed by a claudeclient.Process.
type sessionImpl struct {
	purpose   string
	cardID    string
	cc        claudeclient.Process
	mu        sync.Mutex // guards state
	state     SessionState
	sendMu    sync.Mutex
	closeOnce sync.Once
	logger    *slog.Logger
}

// setState updates the session lifecycle state under s.mu.
func (s *sessionImpl) setState(state SessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state = state
}

// getState returns the current lifecycle state under s.mu.
func (s *sessionImpl) getState() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.state
}

// SessionID returns the CC-assigned session_id, or "" if system_init
// has not yet been observed by the underlying process.
func (s *sessionImpl) SessionID() string {
	return s.cc.SessionID()
}

// SendMessage forwards the frame to the underlying process under the
// per-session send mutex.
func (s *sessionImpl) SendMessage(ctx context.Context, msg claudeclient.StreamMessage) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	return s.cc.SendMessage(ctx, msg)
}

// NextEvent reads one event from the underlying process or returns
// errSessionClosed when the output channel closes.
func (s *sessionImpl) NextEvent(ctx context.Context) (claudeclient.StreamEvent, error) {
	select {
	case ev, ok := <-s.cc.Output():
		if !ok {
			s.setState(StateDead)

			return claudeclient.StreamEvent{}, errSessionClosed
		}

		return ev, nil
	case <-ctx.Done():
		return claudeclient.StreamEvent{}, ctx.Err()
	}
}

// Close terminates the underlying process. Idempotent — repeated calls
// are no-ops after the first.
func (s *sessionImpl) Close(_ context.Context, reason string) error {
	var err error

	s.closeOnce.Do(func() {
		s.setState(StateTerminating)
		s.logger.Info("session closing", "card", s.cardID, "purpose", s.purpose, "reason", reason)
		err = s.cc.Kill()
		s.setState(StateDead)
	})

	return err
}

// Kill is a convenience wrapper for Close with a fixed reason.
func (s *sessionImpl) Kill() error {
	return s.Close(context.Background(), "kill")
}
