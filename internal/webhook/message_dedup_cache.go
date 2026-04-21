package webhook

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// MessageDedupCache stores the response previously returned for a
// (project, card_id, message_id) tuple so retries of a successful
// /message call see the original ack rather than re-invoking the
// stdin writer.
//
// The value stored is the exact JSON-marshalled response body bytes
// and the HTTP status that should accompany it, so retries see
// byte-identical acks.
//
// Concurrency: all exported methods are safe for concurrent use.
// Lifecycle: see Run — it mirrors ReplayCache.Run.
type MessageDedupCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	now      func() time.Time

	entries *list.List
	index   map[string]*list.Element
}

// CachedAck is the stored ack payload returned on a dedup hit.
type CachedAck struct {
	Status int
	Body   []byte
}

type dedupEntry struct {
	key    string
	stored time.Time
	ack    CachedAck
}

// MessageDedupCacheOption configures a MessageDedupCache.
type MessageDedupCacheOption func(*MessageDedupCache)

// WithMessageDedupNow injects a custom clock for deterministic testing.
func WithMessageDedupNow(now func() time.Time) MessageDedupCacheOption {
	return func(c *MessageDedupCache) {
		if now != nil {
			c.now = now
		}
	}
}

// NewMessageDedupCache constructs a dedup cache with the given TTL
// and capacity. A capacity of <= 0 disables the hard cap; a TTL of
// <= 0 disables time-based expiry.
func NewMessageDedupCache(ttl time.Duration, capacity int, opts ...MessageDedupCacheOption) *MessageDedupCache {
	c := &MessageDedupCache{
		ttl:      ttl,
		capacity: capacity,
		now:      time.Now,
		entries:  list.New(),
		index:    make(map[string]*list.Element),
	}
	for _, opt := range opts {
		opt(c)
	}

	return c
}

// messageDedupKey builds the composite lookup key for a message.
// Uses \x00 as a delimiter to avoid collisions across fields that
// could legitimately contain dashes, slashes, or other punctuation.
func messageDedupKey(project, cardID, messageID string) string {
	return project + "\x00" + cardID + "\x00" + messageID
}

// Get returns the stored ack for the key if present and not expired.
// The second return value is false on miss or on a stale entry (which
// is evicted eagerly).
func (c *MessageDedupCache) Get(project, cardID, messageID string) (CachedAck, bool) {
	if messageID == "" {
		return CachedAck{}, false
	}

	key := messageDedupKey(project, cardID, messageID)

	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.index[key]
	if !ok {
		return CachedAck{}, false
	}

	entry := el.Value.(*dedupEntry)
	if c.ttl > 0 && c.now().Sub(entry.stored) > c.ttl {
		c.entries.Remove(el)
		delete(c.index, key)

		return CachedAck{}, false
	}

	// Mark as recently used — moving to the back keeps LRU eviction
	// order consistent with access.
	c.entries.MoveToBack(el)

	// Return a defensive copy so callers can't mutate the stored bytes.
	bodyCopy := make([]byte, len(entry.ack.Body))
	copy(bodyCopy, entry.ack.Body)

	return CachedAck{Status: entry.ack.Status, Body: bodyCopy}, true
}

// Put stores the ack for (project, cardID, messageID). An empty
// messageID is a no-op — dedup requires the client to supply one.
func (c *MessageDedupCache) Put(project, cardID, messageID string, ack CachedAck) {
	if messageID == "" {
		return
	}

	key := messageDedupKey(project, cardID, messageID)

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()

	// Overwrite existing entry if present; otherwise append.
	if el, ok := c.index[key]; ok {
		entry := el.Value.(*dedupEntry)
		entry.stored = now
		entry.ack = ack

		c.entries.MoveToBack(el)

		return
	}

	// Defensive copy so we own the stored bytes.
	bodyCopy := make([]byte, len(ack.Body))
	copy(bodyCopy, ack.Body)

	el := c.entries.PushBack(&dedupEntry{
		key:    key,
		stored: now,
		ack:    CachedAck{Status: ack.Status, Body: bodyCopy},
	})
	c.index[key] = el

	if c.capacity > 0 {
		for c.entries.Len() > c.capacity {
			oldest := c.entries.Front()
			if oldest == nil {
				break
			}

			c.entries.Remove(oldest)
			delete(c.index, oldest.Value.(*dedupEntry).key)
		}
	}
}

// Len returns the current entry count. Intended for tests.
func (c *MessageDedupCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.entries.Len()
}

// sweep removes entries older than the TTL (if set).
func (c *MessageDedupCache) sweep() {
	if c.ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	cutoff := c.now().Add(-c.ttl)

	for {
		front := c.entries.Front()
		if front == nil {
			return
		}

		entry := front.Value.(*dedupEntry)
		if entry.stored.After(cutoff) {
			return
		}

		c.entries.Remove(front)
		delete(c.index, entry.key)
	}
}

// Run sweeps expired entries every 60s until ctx is cancelled.
// Mirrors ReplayCache.Run; see that method's docstring.
func (c *MessageDedupCache) Run(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweep()
		}
	}
}
