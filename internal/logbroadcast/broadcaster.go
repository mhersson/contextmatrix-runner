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

	protocol "github.com/mhersson/contextmatrix-protocol"
)

const (
	// subscriberBufferSize is the channel buffer size for each subscriber.
	// Entries are dropped if a subscriber's buffer is full.
	subscriberBufferSize = 256

	// dropReportInterval bounds how often aggregated drop warnings are logged.
	// Without this bound a chronically-slow SSE client would flood the log
	// with one warn line per dropped entry. Default matches the REVIEW.md ask.
	dropReportInterval = 10 * time.Second

	// maxLogEntryContentBytes caps Content bytes per LogEntry. The logparser
	// allows stream-json lines up to 1 MiB, but fanning a 1 MiB entry to N
	// subscribers each with a 256-slot buffer pins ~N * 256 MiB across
	// subscribers. Worst-case content is the redacted JSON of a single
	// thinking / text block, so a 64 KiB cap leaves all realistic
	// human-readable payloads intact while bounding pinned memory at
	// O(64 KiB) per buffered entry.
	maxLogEntryContentBytes = 64 * 1024
)

// truncatedMarker is appended when LogEntry.Content is truncated at publish
// time so SSE consumers can detect the truncation in the wire shape.
const truncatedMarker = "…[truncated]"

// LogEntry represents a single log entry emitted by a runner container.
// The wire shape is owned by contextmatrix-protocol (this is the /logs SSE
// frame CM consumes); aliased so the rest of the runner keeps compiling
// unchanged. See protocol.LogEntry for the field/type documentation.
type LogEntry = protocol.LogEntry

// TokenUsage carries the per-turn context-window accounting reported by
// Claude in its stream-json output. Aliased to the protocol wire shape.
type TokenUsage = protocol.LogTokenUsage

// subscriber represents a single log subscriber with a buffered channel and
// an optional project or session filter.
//
// Concurrency: once Publish releases the broadcaster's RWMutex (see below)
// it sends to sub.ch without the broadcaster lock. A concurrent Unsubscribe
// must therefore not close the channel out from under an inflight send.
// We serialise closure with a per-subscriber mutex (closeMu) and a closed
// flag — Unsubscribe takes the write lock, sets closed, then closes the
// channel; Publish takes the read lock, checks !closed, and sends. Because
// the critical section around the actual send is tiny (one non-blocking
// channel op) the per-subscriber mutex does not reintroduce the starvation
// the broadcaster-wide lock caused.
type subscriber struct {
	ch        chan LogEntry
	project   string // empty means "all projects" (when sessionID is also empty)
	sessionID string // non-empty means filter on entry.SessionID

	closeMu sync.RWMutex
	closed  bool
}

// matches reports whether this subscriber should receive the given entry.
// If sessionID is set, only entries with a matching SessionID are delivered.
// If project is set, only entries with a matching Project are delivered.
// If both are empty, all entries are delivered.
func (s *subscriber) matches(entry LogEntry) bool {
	if s.sessionID != "" {
		return entry.SessionID == s.sessionID
	}

	return s.project == "" || s.project == entry.Project
}

// DropObserver is notified once per dropped entry. Implementations MUST be
// non-blocking and safe for concurrent use. Typical implementations increment
// a Prometheus counter; a nil observer is a no-op.
type DropObserver interface {
	ObserveDrop()
}

// PanicObserver is notified when the drop-reporter goroutine recovers from a
// panic. Implementations MUST be non-blocking and safe for concurrent use;
// typical implementations bump the runner's panic_recovered counter. A nil
// observer is a no-op. The string argument is a stable label identifying the
// goroutine (e.g. "broadcaster_drop_reporter") so the implementation can
// attribute the panic to a known bucket without introducing per-broadcaster
// cardinality.
type PanicObserver interface {
	ObservePanic(goroutine string)
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
	panicObs   PanicObserver
	closeOnce  sync.Once
	closed     chan struct{}
	// reporterDone is closed by the drop-reporter goroutine once it has
	// flushed the final summary and returned. Close() blocks on this so the
	// caller's "Close has returned" guarantee actually matches the docstring.
	reporterDone chan struct{}
}

// dropReporterLabel is the goroutine label reported to PanicObserver. Defined
// as a const so callers can pin their PanicRecoveredTotal label assertions
// against the same value the broadcaster uses.
const dropReporterLabel = "broadcaster_drop_reporter"

// NewBroadcaster creates a new, ready-to-use Broadcaster. A nil logger is
// tolerated (drops are silently counted). A nil dropObs is a no-op.
func NewBroadcaster(logger *slog.Logger, dropObs DropObserver) *Broadcaster {
	return NewBroadcasterWithPanicObserver(logger, dropObs, nil)
}

// NewBroadcasterWithPanicObserver constructs a Broadcaster that reports any
// recovered panic in the drop-reporter goroutine to panicObs. Production
// callers pass an adapter that bumps the runner's panic_recovered counter so
// background-goroutine panics surface as alerts instead of being silently
// swallowed.
func NewBroadcasterWithPanicObserver(logger *slog.Logger, dropObs DropObserver, panicObs PanicObserver) *Broadcaster {
	b := &Broadcaster{
		subscribers:  make(map[*subscriber]struct{}),
		logger:       logger,
		dropObs:      dropObs,
		panicObs:     panicObs,
		closed:       make(chan struct{}),
		reporterDone: make(chan struct{}),
	}

	b.startDropReporter(dropReportInterval)

	return b
}

// dropReporterRespawnBackoff bounds how fast the drop reporter is allowed to
// re-enter the flush loop after a recovered panic. Matches the cadence of the
// main.go-managed respawn wrappers (dockerd monitor, maintenance loop, gauge)
// so a chronically panicking slog handler doesn't pin a CPU.
const dropReporterRespawnBackoff = 5 * time.Second

// startDropReporter launches the background ticker that flushes the drop
// counter into a single aggregated log line per interval. Exposed for tests
// via NewBroadcasterWithInterval.
//
// Lifecycle: the outer goroutine is a respawn-with-backoff loop that mirrors
// the pattern used by main.go's dockerd-monitor / maintenance / gauge
// goroutines. The inner runOne function does the actual ticker work and is
// recover-wrapped so a slog handler panic (or any other unexpected fault in
// flushDrops) does not unwind the main process. A non-nil PanicObserver
// receives the goroutine label so operators see the recovery in
// cmr_panic_recovered_total. After a recovered panic the outer loop waits
// dropReporterRespawnBackoff (respecting b.closed so a Close during the
// backoff still exits promptly) and restarts the flush loop with a fresh
// ticker. This keeps the broadcaster's panic discipline consistent with the
// peer background goroutines in main.go — previously a single panic would
// silently disable drop reporting for the lifetime of the broadcaster while
// the peer pattern would have respawned.
//
// reporterDone is closed exactly once when the goroutine returns for the
// final time (after b.closed is signalled), so Close()'s wait semantics are
// preserved across respawns.
func (b *Broadcaster) startDropReporter(interval time.Duration) {
	go func() {
		defer close(b.reporterDone)

		for {
			// runOne returns true iff b.closed was observed (clean exit
			// path) and false iff the inner loop panicked. In either
			// case the ticker the call constructed has been stopped via
			// the inner defer.
			exited := b.runDropReporterOnce(interval)
			if exited {
				return
			}

			// Panic path: wait briefly so a persistent fault does not
			// hot-loop and respect b.closed so a shutdown landing during
			// the backoff still exits promptly.
			select {
			case <-b.closed:
				return
			case <-time.After(dropReporterRespawnBackoff):
			}
		}
	}()
}

// runDropReporterOnce runs the drop-flush ticker loop until either b.closed
// fires (returns true) or the body panics and the deferred recover swallows
// the panic (returns false). Splitting this out of startDropReporter keeps the
// outer respawn-with-backoff loop free of inline defers, so a recovered panic
// in the inner body cleanly bubbles to the outer for-loop.
func (b *Broadcaster) runDropReporterOnce(interval time.Duration) (exited bool) {
	ticker := time.NewTicker(interval)
	// Publish the ticker on the receiver so test introspection
	// (PendingDropCount and friends) keeps working unchanged.
	b.dropTicker = ticker

	defer ticker.Stop()
	defer func() {
		r := recover()
		if r == nil {
			return
		}

		if b.panicObs != nil {
			b.panicObs.ObservePanic(dropReporterLabel)
		}

		// The original panic likely came from b.logger itself (a
		// misbehaving slog handler is the realistic failure mode).
		// Logging the recovery via the same handler would re-panic;
		// wrap the Error call so the goroutine still exits cleanly
		// instead of unwinding the recover.
		func() {
			defer func() { _ = recover() }()

			if b.logger != nil {
				b.logger.Error("broadcaster drop reporter panicked; will respawn",
					"goroutine", dropReporterLabel,
					"backoff", dropReporterRespawnBackoff.String(),
					"panic", r,
				)
			}
		}()
	}()

	for {
		select {
		case <-b.closed:
			b.flushDrops()

			return true
		case <-ticker.C:
			b.flushDrops()
		}
	}
}

// NewBroadcasterWithInterval constructs a Broadcaster with a custom drop-log
// interval. Intended for tests that want tighter timing.
func NewBroadcasterWithInterval(logger *slog.Logger, dropObs DropObserver, interval time.Duration) *Broadcaster {
	return NewBroadcasterWithIntervalAndPanicObserver(logger, dropObs, nil, interval)
}

// NewBroadcasterWithIntervalAndPanicObserver constructs a Broadcaster with a
// custom drop-log interval AND a panic observer for the drop-reporter
// goroutine. Intended for tests that exercise the panic-recovery path with
// tight timing.
func NewBroadcasterWithIntervalAndPanicObserver(logger *slog.Logger, dropObs DropObserver, panicObs PanicObserver, interval time.Duration) *Broadcaster {
	b := &Broadcaster{
		subscribers:  make(map[*subscriber]struct{}),
		logger:       logger,
		dropObs:      dropObs,
		panicObs:     panicObs,
		closed:       make(chan struct{}),
		reporterDone: make(chan struct{}),
	}

	b.startDropReporter(interval)

	return b
}

// Close stops the background drop reporter and flushes any pending counts.
// Calling Close more than once is safe.
//
// Close honours ctx: it signals the reporter to stop and then blocks until
// either the reporter has finished its final flush or ctx is cancelled. A
// cancelled context is reported via the returned error, but the reporter
// will still wind down asynchronously on its own.
func (b *Broadcaster) Close(ctx context.Context) error {
	b.closeOnce.Do(func() { close(b.closed) })

	if ctx == nil {
		<-b.reporterDone

		return nil
	}

	select {
	case <-b.reporterDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// flushDrops emits a single summary log line for dropped entries since
// the previous flush, if any. Despite its name, it holds no broadcaster
// lock — only b.dropCount (atomic) and the immutable b.logger are touched.
func (b *Broadcaster) flushDrops() {
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
//
// Subscribe after Close: the broadcaster rejects new subscriptions once
// Close has been called. The returned channel is already closed and the
// unsubscribe is a no-op — callers see a closed receive immediately and
// must not block on the channel.
func (b *Broadcaster) Subscribe(project string) (<-chan LogEntry, func()) {
	return b.subscribeInternal(project, "")
}

// SubscribeWithSessionID registers a new subscriber filtered to a single chat
// session. Only entries whose SessionID equals sessionID are delivered. The
// returned channel and unsubscribe function follow the same semantics as
// Subscribe, including the post-Close rejection behaviour.
func (b *Broadcaster) SubscribeWithSessionID(sessionID string) (<-chan LogEntry, func()) {
	return b.subscribeInternal("", sessionID)
}

// subscribeInternal is the common implementation behind Subscribe and
// SubscribeWithSessionID. Exactly one of project / sessionID is expected to
// be non-empty; both empty registers an "all entries" subscriber.
func (b *Broadcaster) subscribeInternal(project, sessionID string) (<-chan LogEntry, func()) {
	sub := &subscriber{
		ch:        make(chan LogEntry, subscriberBufferSize),
		project:   project,
		sessionID: sessionID,
	}

	b.mu.Lock()
	// Reject post-close. Once Close has run, the drop reporter has flushed
	// and we no longer accept new subscribers; otherwise a late /logs
	// caller would silently see nothing and never receive a drop summary.
	select {
	case <-b.closed:
		b.mu.Unlock()
		sub.closeMu.Lock()
		sub.closed = true
		close(sub.ch)
		sub.closeMu.Unlock()

		return sub.ch, func() {}
	default:
	}

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
// pattern. The drop-on-full-channel semantics are
// preserved exactly: the select's default branch still increments the
// dropCount and observer.
//
// Because the send runs outside the lock, a concurrent Unsubscribe can close
// a snapshotted subscriber's channel between the snapshot and the send. We
// guard each send with trySend, which acquires the subscriber's per-instance
// closeMu (RLock) and checks the subscriber's `closed` flag before
// performing the non-blocking channel send. Unsubscribe takes closeMu.Lock
// before setting `closed` and closing the channel, so trySend cannot race
// with a close — no recover() is used and no "send on closed channel" panic
// can escape. An entry sent during an in-flight Unsubscribe simply returns
// false from trySend and is counted as a drop, which is consistent with the
// semantics of an unsubscribed client (they asked to stop receiving, so
// losing this in-flight entry is expected and invisible to them).
func (b *Broadcaster) Publish(entry LogEntry) {
	// Closed-broadcaster fast path. Once Close has been called, all
	// surviving subscribers (if any) are about to be torn down by the
	// caller's defers; fanning out more entries to them wastes work and
	// would let a late Publish race with the close path. Subscribe()
	// already rejects post-close registrations, so an in-flight Publish
	// is the only remaining write path.
	select {
	case <-b.closed:
		return
	default:
	}

	// Truncate Content at publish time so a slow subscriber cannot pin
	// O(N * 1 MiB) heap memory in its buffer (logparser permits
	// 1 MiB-per-line stream-json input). System and user entries are
	// rarely large enough to hit the cap, but the truncation runs
	// uniformly to keep the wire-shape contract consistent across types.
	if len(entry.Content) > maxLogEntryContentBytes {
		const head = maxLogEntryContentBytes - len(truncatedMarker)
		// Defence in depth: maxLogEntryContentBytes is larger than the
		// marker by construction, so head is always positive. The check
		// guards against a future tweak that shrinks the cap below the
		// marker length.
		if head > 0 && head < len(entry.Content) {
			entry.Content = entry.Content[:head] + truncatedMarker
		} else {
			entry.Content = truncatedMarker
		}
	}

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
