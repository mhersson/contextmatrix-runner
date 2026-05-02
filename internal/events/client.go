// Package events provides the runner-side client for subscribing to
// CM's per-card runner-events SSE endpoint plus a polling fallback.
// Both share the RunnerEvent shape that mirrors CM's runner ring buffer.
package events

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RunnerEvent mirrors CM's runner-event shape (defined here to avoid a
// CM ↔ runner module dependency).
type RunnerEvent struct {
	EventID uint64    `json:"event_id"`
	Type    string    `json:"type"`
	Data    string    `json:"data,omitempty"`
	At      time.Time `json:"at"`
}

// SSEClient subscribes to CM's per-card runner-events SSE endpoint with
// auto-reconnect and Last-Event-ID resume. It is goroutine-safe.
type SSEClient struct {
	BaseURL    string
	CardID     string
	AuthToken  string
	HTTPClient *http.Client

	mu          sync.Mutex
	LastEventID uint64
}

// NewSSEClient constructs a client. baseURL is the SSE endpoint URL
// (e.g. "http://cm:8080/api/runner/events"); cardID identifies the
// per-card stream; authToken is the bearer for CM's MCP auth.
func NewSSEClient(baseURL, cardID, authToken string) *SSEClient {
	return &SSEClient{
		BaseURL:    baseURL,
		CardID:     cardID,
		AuthToken:  authToken,
		HTTPClient: &http.Client{},
	}
}

// Subscribe opens the SSE stream. The returned event channel emits
// RunnerEvent values in arrival order; the error channel receives a
// signal only when ctx is cancelled and the goroutine exits. Network
// failures are absorbed by exponential-backoff reconnects.
func (c *SSEClient) Subscribe(ctx context.Context) (<-chan RunnerEvent, <-chan error) {
	events := make(chan RunnerEvent, 32)

	errs := make(chan error, 1)
	go c.run(ctx, events, errs)

	return events, errs
}

func (c *SSEClient) run(ctx context.Context, events chan<- RunnerEvent, errs chan<- error) {
	defer close(events)
	defer close(errs)

	backoff := 250 * time.Millisecond
	maxBackoff := 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		if err := c.connectOnce(ctx, events); err != nil {
			// Connection failed — back off and retry. Don't surface as a
			// fatal error; SSE is expected to flap.
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}

			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}

			continue
		}
		// Clean disconnect — reconnect immediately with reset backoff.
		backoff = 250 * time.Millisecond
	}
}

func (c *SSEClient) connectOnce(ctx context.Context, events chan<- RunnerEvent) error {
	url := fmt.Sprintf("%s?card_id=%s", c.BaseURL, c.CardID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}

	c.mu.Lock()
	lastID := c.LastEventID
	c.mu.Unlock()

	if lastID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(lastID, 10))
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sse: status %d", resp.StatusCode)
	}

	return c.parseStream(ctx, resp.Body, events)
}

func (c *SSEClient) parseStream(ctx context.Context, body io.Reader, events chan<- RunnerEvent) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		ev        RunnerEvent
		dataLines []string
	)

	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		line := sc.Text()
		if line == "" {
			// Empty line = end of event. Flush and reset.
			if len(dataLines) > 0 || ev.EventID != 0 || ev.Type != "" {
				ev.Data = strings.Join(dataLines, "\n")
				if ev.EventID > 0 {
					c.mu.Lock()
					c.LastEventID = ev.EventID
					c.mu.Unlock()
				}

				select {
				case events <- ev:
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			ev = RunnerEvent{}
			dataLines = nil

			continue
		}

		if strings.HasPrefix(line, ":") {
			// Comment / keepalive — ignore.
			continue
		}

		switch {
		case strings.HasPrefix(line, "id: "):
			if id, err := strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64); err == nil {
				ev.EventID = id
			}
		case strings.HasPrefix(line, "event: "):
			ev.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}

	if err := sc.Err(); err != nil {
		return err
	}

	return io.EOF
}
