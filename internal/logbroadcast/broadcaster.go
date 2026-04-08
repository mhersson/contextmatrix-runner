// Package logbroadcast provides a thread-safe fan-out broadcaster for runner
// log entries. Subscribers register with an optional project filter and receive
// a buffered channel of LogEntry values. Slow subscribers are non-blocking:
// entries are dropped when a subscriber's buffer is full and a warning is logged.
package logbroadcast

import (
	"log/slog"
	"sync"
	"time"
)

const (
	// subscriberBufferSize is the channel buffer size for each subscriber.
	// Entries are dropped if a subscriber's buffer is full.
	subscriberBufferSize = 256
)

// LogEntry represents a single log entry emitted by a runner container.
type LogEntry struct {
	Timestamp time.Time `json:"ts"`
	CardID    string    `json:"card_id"`
	Project   string    `json:"project"`
	// Type is one of: text, thinking, tool_call, stderr, system.
	Type    string `json:"type"`
	Content string `json:"content"`
}

// subscriber represents a single log subscriber with a buffered channel and
// an optional project filter.
type subscriber struct {
	ch      chan LogEntry
	project string // empty means "all projects"
}

// matches reports whether this subscriber should receive the given entry.
func (s *subscriber) matches(entry LogEntry) bool {
	return s.project == "" || s.project == entry.Project
}

// Broadcaster fans out published LogEntry values to all registered subscribers.
// It is safe for concurrent use.
type Broadcaster struct {
	mu          sync.RWMutex
	subscribers map[*subscriber]struct{}
}

// NewBroadcaster creates a new, ready-to-use Broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		subscribers: make(map[*subscriber]struct{}),
	}
}

// Subscribe registers a new subscriber and returns a receive-only channel and
// an unsubscribe function. The channel has a buffer of 256 entries.
//
// project filters entries by project name. An empty string means "all
// projects" — the subscriber will receive every published entry.
//
// The caller must call the returned unsubscribe function when done to prevent
// resource leaks. After calling unsubscribe, the returned channel is closed.
// Calling unsubscribe more than once is safe and has no effect.
func (b *Broadcaster) Subscribe(project string) (<-chan LogEntry, func()) {
	sub := &subscriber{
		ch:      make(chan LogEntry, subscriberBufferSize),
		project: project,
	}

	b.mu.Lock()
	b.subscribers[sub] = struct{}{}
	b.mu.Unlock()

	return sub.ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		if _, ok := b.subscribers[sub]; ok {
			delete(b.subscribers, sub)
			close(sub.ch)
		}
	}
}

// Publish sends entry to all subscribers whose project filter matches. This
// method never blocks: if a subscriber's buffer is full, the entry is dropped
// for that subscriber and a warning is logged via slog.
func (b *Broadcaster) Publish(entry LogEntry) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for sub := range b.subscribers {
		if !sub.matches(entry) {
			continue
		}
		select {
		case sub.ch <- entry:
			// Entry delivered.
		default:
			// Buffer full — drop entry for this slow subscriber.
			slog.Warn("log entry dropped for slow subscriber",
				"card_id", entry.CardID,
				"project", entry.Project,
				"type", entry.Type,
			)
		}
	}
}

// SubscriberCount returns the current number of active subscribers.
// Useful for testing and monitoring.
func (b *Broadcaster) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}
