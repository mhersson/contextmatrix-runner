package claudeclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Process is the running CC subprocess. Output streams parsed events
// until the underlying stream ends or Kill is called.
type Process interface {
	SendMessage(ctx context.Context, msg StreamMessage) error
	CloseStdin() error // signal EOF to the running Claude
	Output() <-chan StreamEvent
	SessionID() string
	Wait(ctx context.Context) (Usage, error)
	Kill() error
}

// StreamMessage is one stream-json frame written to Claude Code's stdin.
// Matches Claude's --input-format stream-json shape:
//
//	{"type":"user","message":{"role":"user","content":[{"type":"text","text":"..."}]}}
type StreamMessage struct {
	Type    string            `json:"type"` // "user"
	Message StreamUserContent `json:"message"`
}

// StreamUserContent is the nested message body of a stream-json user turn.
type StreamUserContent struct {
	Role    string            `json:"role"` // "user"
	Content []StreamTextBlock `json:"content"`
}

// StreamTextBlock is one content block within a stream-json user turn.
type StreamTextBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// NewUserMessage builds a single-text-block user message — the most
// common shape phase actions use to prime an ephemeral CC subprocess.
func NewUserMessage(text string) StreamMessage {
	return StreamMessage{
		Type: "user",
		Message: StreamUserContent{
			Role:    "user",
			Content: []StreamTextBlock{{Type: "text", Text: text}},
		},
	}
}

// procImpl is the default Process implementation backed by a stream-json
// parser plus a writable stdin pipe.
type procImpl struct {
	execID string
	parser *StreamJSONParser
	stdout io.ReadCloser
	stdin  io.WriteCloser

	out chan StreamEvent

	cancel context.CancelFunc

	mu        sync.Mutex
	sessionID string
	usage     Usage
	closed    bool

	writeMu sync.Mutex
}

// newProcess wires the parser, pump goroutine, and Kill plumbing.
func newProcess(ctx context.Context, w *Wrapper, execID string, stdout io.ReadCloser, stdin io.WriteCloser) *procImpl {
	pctx, cancel := context.WithCancel(ctx)

	p := &procImpl{
		execID: execID,
		parser: NewStreamJSONParser(stdout),
		stdout: stdout,
		stdin:  stdin,
		out:    make(chan StreamEvent, 16),
		cancel: cancel,
	}

	go p.pump(pctx, w)

	return p
}

// pump reads parsed events, captures session_id / usage, fans them out
// to the Output channel, and invokes the wrapper's registered event
// callback if set.
//
// On context cancellation it closes stdin/stdout to unblock the parser
// and exits. The parser goroutine may still be blocked on Read for fakes
// that wrap stdout with io.NopCloser, but the pump itself never blocks
// past cancellation.
func (p *procImpl) pump(ctx context.Context, w *Wrapper) {
	defer close(p.out)

	events := p.parser.Events()

	for {
		select {
		case <-ctx.Done():
			p.closeStdinSafe()
			_ = p.stdout.Close()

			return
		case ev, ok := <-events:
			if !ok {
				return
			}

			p.handleEvent(ev, w)

			select {
			case p.out <- ev:
			case <-ctx.Done():
				p.closeStdinSafe()
				_ = p.stdout.Close()

				return
			}
		}
	}
}

// closeStdinSafe closes the stdin pipe under writeMu so it interleaves
// safely with SendMessage / CloseStdin callers. Sets stdin to nil after
// close so later writes return an error rather than panicking.
func (p *procImpl) closeStdinSafe() {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	if p.stdin == nil {
		return
	}

	_ = p.stdin.Close()
	p.stdin = nil
}

// handleEvent records session_id / usage and fires the wrapper's
// registered event callback.
func (p *procImpl) handleEvent(ev StreamEvent, w *Wrapper) {
	switch ev.Kind {
	case EventSystemInit:
		p.mu.Lock()
		p.sessionID = ev.SessionID
		p.mu.Unlock()
	case EventSystemEnd:
		p.mu.Lock()
		p.usage = ev.Usage
		p.mu.Unlock()
	}

	if w != nil {
		if cb := w.callbackForCurrentRun(); cb != nil {
			cb(ev)
		}
	}
}

// SendMessage writes a JSON-encoded stream-json frame plus newline to stdin.
// Calls are serialized so concurrent SendMessage callers don't interleave.
func (p *procImpl) SendMessage(_ context.Context, msg StreamMessage) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	if p.stdin == nil {
		return fmt.Errorf("stdin is closed")
	}

	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal stream message: %w", err)
	}

	b = append(b, '\n')
	if _, err := p.stdin.Write(b); err != nil {
		return fmt.Errorf("write stream message: %w", err)
	}

	return nil
}

// CloseStdin signals EOF to Claude Code so a single-turn invocation can exit
// cleanly. Idempotent: subsequent SendMessage attempts return an error.
func (p *procImpl) CloseStdin() error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	if p.stdin == nil {
		return nil
	}

	err := p.stdin.Close()
	p.stdin = nil // prevent later SendMessage attempts on a closed pipe

	return err
}

// Output returns the channel of parsed events. Closes when the underlying
// stream ends or Kill is called.
func (p *procImpl) Output() <-chan StreamEvent { return p.out }

// SessionID returns the session_id captured from EventSystemInit, or "" if
// not yet observed.
func (p *procImpl) SessionID() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.sessionID
}

// Wait drains Output until close, returning the Usage captured from
// EventSystemEnd. Blocking on Output (not on the done signal) ensures
// Wait does not return while events are still buffered in the channel.
func (p *procImpl) Wait(ctx context.Context) (Usage, error) {
	for {
		select {
		case <-ctx.Done():
			return Usage{}, ctx.Err()
		case _, ok := <-p.out:
			if !ok {
				p.mu.Lock()
				u := p.usage
				p.mu.Unlock()

				return u, nil
			}
		}
	}
}

// Kill cancels the pump context and closes both ends of the pipe. Idempotent.
func (p *procImpl) Kill() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()

		return nil
	}

	p.closed = true
	p.mu.Unlock()

	p.cancel()
	p.closeStdinSafe()
	_ = p.stdout.Close()

	return nil
}
