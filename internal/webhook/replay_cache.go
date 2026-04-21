package webhook

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// ReplayCache is a thread-safe TTL+capacity-bounded set used to reject
// webhook replay attacks. It is keyed on the hex-encoded HMAC signature
// and records the moment the signature was first observed.
//
// Concurrency: all exported methods are safe for concurrent use.
//
// Lifecycle: Run starts a background eviction goroutine that sweeps
// expired entries every 60s. Run returns when the supplied context is
// cancelled — callers that construct a cache must wire it into the
// shutdown context or the goroutine will leak.
type ReplayCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	now      func() time.Time

	// entries is an LRU — the list is ordered oldest-first (front) to
	// newest-last (back). index gives O(1) lookup into the list element
	// holding that signature's entry.
	entries *list.List
	index   map[string]*list.Element
}

type replayEntry struct {
	sig  string
	seen time.Time
}

// ReplayCacheOption configures a ReplayCache.
type ReplayCacheOption func(*ReplayCache)

// WithReplayCacheNow injects a custom clock for deterministic testing.
func WithReplayCacheNow(now func() time.Time) ReplayCacheOption {
	return func(r *ReplayCache) {
		if now != nil {
			r.now = now
		}
	}
}

// NewReplayCache constructs a replay cache with the given TTL and
// capacity. A capacity of <= 0 disables the hard cap; a TTL of <= 0
// disables time-based expiry (entries live until evicted by capacity).
func NewReplayCache(ttl time.Duration, capacity int, opts ...ReplayCacheOption) *ReplayCache {
	r := &ReplayCache{
		ttl:      ttl,
		capacity: capacity,
		now:      time.Now,
		entries:  list.New(),
		index:    make(map[string]*list.Element),
	}
	for _, opt := range opts {
		opt(r)
	}

	return r
}

// Seen atomically checks whether sig has been observed within the TTL
// window and, if not, records it. Returns true when the signature was
// already seen (i.e. the caller should treat the request as a replay),
// false when the signature is new and has now been recorded.
func (r *ReplayCache) Seen(sig string) bool {
	if sig == "" {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()

	if el, ok := r.index[sig]; ok {
		entry := el.Value.(*replayEntry)
		if r.ttl <= 0 || now.Sub(entry.seen) <= r.ttl {
			// Refresh the entry's position so recent repeats stay warm.
			entry.seen = now

			r.entries.MoveToBack(el)

			return true
		}
		// Expired: drop it and treat as fresh.
		r.entries.Remove(el)
		delete(r.index, sig)
	}

	// Record the new signature at the back (newest).
	el := r.entries.PushBack(&replayEntry{sig: sig, seen: now})
	r.index[sig] = el

	// Enforce capacity by evicting the oldest entries (front).
	if r.capacity > 0 {
		for r.entries.Len() > r.capacity {
			oldest := r.entries.Front()
			if oldest == nil {
				break
			}

			r.entries.Remove(oldest)
			delete(r.index, oldest.Value.(*replayEntry).sig)
		}
	}

	return false
}

// Len returns the current number of entries. Intended for tests.
func (r *ReplayCache) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.entries.Len()
}

// sweep removes all entries older than the TTL. Exported variant is
// Run; this is the single-shot form used by both Run and tests.
func (r *ReplayCache) sweep() {
	if r.ttl <= 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := r.now().Add(-r.ttl)

	for {
		front := r.entries.Front()
		if front == nil {
			return
		}

		entry := front.Value.(*replayEntry)
		if entry.seen.After(cutoff) {
			return
		}

		r.entries.Remove(front)
		delete(r.index, entry.sig)
	}
}

// Run sweeps expired entries every 60s until ctx is cancelled. It
// blocks; callers typically run it in a goroutine. Calling Run on a
// cache without time-based expiry (ttl <= 0) still honours context
// cancellation but does no sweep work.
func (r *ReplayCache) Run(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep()
		}
	}
}
