// Package logbroadcast provides a thread-safe fan-out broadcaster for runner
// log entries. Subscribers register with an optional project filter and receive
// a buffered channel of LogEntry values. Slow subscribers are non-blocking:
// entries are dropped when a subscriber's buffer is full and an aggregated
// warning is logged at most once per [dropReportInterval].
package logbroadcast

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// subscriberBufferSize is the channel buffer size for each subscriber.
	// Entries are dropped if a subscriber's buffer is full.
	subscriberBufferSize = 256

	// dropReportInterval bounds how often aggregated drop warnings are logged.
	// Without this bound a chronically-slow SSE client would flood the log
	// with one warn line per dropped entry. Default matches the REVIEW.md ask.
	dropReportInterval = 10 * time.Second
)

// LogEntry represents a single log entry emitted by a runner container.
type LogEntry struct {
	Timestamp time.Time `json:"ts"`
	CardID    string    `json:"card_id"`
	Project   string    `json:"project"`
	// Type is one of: text, thinking, tool_call, stderr, system, user.
	// "user" is a message submitted via the HITL chat input.
	Type    string `json:"type"`
	Content string `json:"content"`
}

// subscriber represents a single log subscriber with a buffered channel and
// an optional project filter.
//
// Concurrency: once Publish releases the broadcaster's RWMutex (see below)
// it sends to sub.ch without the broadcaster lock. A concurrent Unsubscribe
// must therefore not close the channel out from under an inflight send.
// We serialise closure with a per-subscriber mutex (closeMu) and a closed
// flag — Unsubscribe takes the write lock, sets closed, then closes the
// channel; Publish takes the read lock, checks !closed, and sends. Because
// the critical section around the actual send is tiny (one non-blocking
// channel op) the per-subscriber mutex does not reintroduce the starvation
// the broadcaster-wide lock caused. See CTXRUN-059 (H25).
type subscriber struct {
	ch      chan LogEntry
	project string // empty means "all projects"

	closeMu sync.RWMutex
	closed  bool
}

// matches reports whether this subscriber should receive the given entry.
func (s *subscriber) matches(entry LogEntry) bool {
	return s.project == "" || s.project == entry.Project
}

// DropObserver is notified once per dropped entry. Implementations MUST be
// non-blocking and safe for concurrent use. Typical implementations increment
// a Prometheus counter; a nil observer is a no-op.
type DropObserver interface {
	ObserveDrop()
}

// Broadcaster fans out published LogEntry values to all registered subscribers.
// It is safe for concurrent use.
type Broadcaster struct {
	mu          sync.RWMutex
	subscribers map[*subscriber]struct{}

	logger     *slog.Logger
	dropCount  atomic.Uint64
	dropTicker *time.Ticker
	dropObs    DropObserver
	closeOnce  sync.Once
	closed     chan struct{}
}

// NewBroadcaster creates a new, ready-to-use Broadcaster. A nil logger is
// tolerated (drops are silently counted).
func NewBroadcaster(logger *slog.Logger, dropObs DropObserver) *Broadcaster {
	b := &Broadcaster{
		subscribers: make(map[*subscriber]struct{}),
		logger:      logger,
		dropObs:     dropObs,
		closed:      make(chan struct{}),
	}

	b.startDropReporter(dropReportInterval)

	return b
}

// startDropReporter launches the background ticker that flushes the drop
// counter into a single aggregated log line per interval. Exposed for tests
// via NewBroadcasterWithInterval.
func (b *Broadcaster) startDropReporter(interval time.Duration) {
	b.dropTicker = time.NewTicker(interval)

	go func() {
		for {
			select {
			case <-b.closed:
				b.dropTicker.Stop()
				b.flushDropsLocked()

				return
			case <-b.dropTicker.C:
				b.flushDropsLocked()
			}
		}
	}()
}

// NewBroadcasterWithInterval constructs a Broadcaster with a custom drop-log
// interval. Intended for tests that want tighter timing.
func NewBroadcasterWithInterval(logger *slog.Logger, dropObs DropObserver, interval time.Duration) *Broadcaster {
	b := &Broadcaster{
		subscribers: make(map[*subscriber]struct{}),
		logger:      logger,
		dropObs:     dropObs,
		closed:      make(chan struct{}),
	}

	b.startDropReporter(interval)

	return b
}

// Close stops the background drop reporter and flushes any pending counts.
// Calling Close more than once is safe.
func (b *Broadcaster) Close(_ context.Context) error {
	b.closeOnce.Do(func() { close(b.closed) })

	return nil
}

// flushDropsLocked emits a single summary log line for dropped entries since
// the previous flush, if any.
func (b *Broadcaster) flushDropsLocked() {
	n := b.dropCount.Swap(0)
	if n == 0 {
		return
	}

	if b.logger != nil {
		b.logger.Warn("log entries dropped for slow subscribers", "count", n, "window", dropReportInterval.String())
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
		if _, ok := b.subscribers[sub]; !ok {
			b.mu.Unlock()

			return
		}

		delete(b.subscribers, sub)
		b.mu.Unlock()

		// Close under the per-subscriber write lock so any concurrent
		// Publish (which took a snapshot before our delete and holds only
		// closeMu.RLock before the send) finishes first. This pair of
		// locks is the whole reason the broadcaster-wide RLock can be
		// dropped before the send without re-introducing a send-on-closed
		// race.
		sub.closeMu.Lock()
		sub.closed = true
		close(sub.ch)
		sub.closeMu.Unlock()
	}
}

// Publish sends entry to all subscribers whose project filter matches. This
// method never blocks: if a subscriber's buffer is full the entry is dropped
// for that subscriber and the drop counter is incremented. A summary log line
// is emitted at most once per [dropReportInterval].
//
// Concurrency: the RLock is held only long enough to snapshot the current
// subscriber set. The actual send loop runs WITHOUT the lock, so a slow
// subscriber's full channel buffer (observed by the default-drop branch of
// the select) cannot starve Subscribe / Unsubscribe callers under sustained
// log throughput. This mirrors the standard "copy-under-lock, work-outside"
// pattern. See CTXRUN-059 (H25). The drop-on-full-channel semantics are
// preserved exactly: the select's default branch still increments the
// dropCount and observer.
//
// Because the send runs outside the lock, a concurrent Unsubscribe can close
// a snapshotted subscriber's channel between the snapshot and the send. We
// guard each send with trySend, which serialises against Unsubscribe via the
// per-subscriber closeMu + closed flag so we never attempt a send on a
// closed channel — concurrent unsubscribers are treated as a drop, consistent
// with the semantics of an unsubscribed client (they asked to stop receiving,
// so losing this in-flight entry is expected and invisible to them).
func (b *Broadcaster) Publish(entry LogEntry) {
	b.mu.RLock()

	snap := make([]*subscriber, 0, len(b.subscribers))
	for sub := range b.subscribers {
		snap = append(snap, sub)
	}

	b.mu.RUnlock()

	for _, sub := range snap {
		if !sub.matches(entry) {
			continue
		}

		if !b.trySend(sub, entry) {
			b.dropCount.Add(1)

			if b.dropObs != nil {
				b.dropObs.ObserveDrop()
			}
		}
	}
}

// trySend does the non-blocking channel send under the subscriber's closeMu
// read lock. A concurrent Unsubscribe must first acquire closeMu.Lock (see
// Subscribe's returned closure) so it cannot close the channel while we're
// sending. Returns true iff the entry was delivered (buffered on the
// subscriber's channel); false on a dropped send or a subscriber that has
// already unsubscribed.
func (b *Broadcaster) trySend(sub *subscriber, entry LogEntry) bool {
	sub.closeMu.RLock()
	defer sub.closeMu.RUnlock()

	if sub.closed {
		return false
	}

	select {
	case sub.ch <- entry:
		return true
	default:
		return false
	}
}

// SubscriberCount returns the current number of active subscribers.
// Useful for testing and monitoring.
func (b *Broadcaster) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return len(b.subscribers)
}

// PendingDropCount returns the number of drops that have not yet been flushed
// by the ticker. Exposed for tests; not part of the stable API.
func (b *Broadcaster) PendingDropCount() uint64 {
	return b.dropCount.Load()
}
