package orchestrated

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// hostResolver is the subset of net.Resolver used by buildExtraHosts. The
// real implementation is &net.Resolver{}; tests override it with a stub that
// sleeps / returns a canned answer. A narrow interface avoids having to
// construct a real resolver in tests and keeps the cache unit-testable
// without touching DNS.
type hostResolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// dnsCacheCapacity bounds the number of distinct hostnames kept warm. 256 is
// generous for the runner workload (one MCP host per deployment, maybe a
// small handful across projects) while keeping memory trivial. The cache is
// LRU so pathological churn from attacker-chosen MCPURLs can't evict
// legitimate entries indefinitely — once the first 256 slots fill, the
// oldest gets evicted per insertion.
const dnsCacheCapacity = 256

// dnsCacheTTL is how long a resolved entry stays valid. 5 minutes balances
// responsiveness to DNS changes (a backend IP churn still propagates within
// a few minutes) against the spawn-path cost we're trying to eliminate.
const dnsCacheTTL = 5 * time.Minute

// dnsCacheEntry is a single cached resolution. addrs is the result returned
// from LookupHost; expiresAt is when the entry becomes stale.
type dnsCacheEntry struct {
	host      string
	addrs     []string
	expiresAt time.Time
}

// dnsCache is a TTL+capacity-bounded LRU of DNS resolutions. Thread-safe.
// A zero-value cache is NOT usable — use newDNSCache to construct one with
// the backing list/map wired up.
//
// Concurrency: all exported methods are safe for concurrent use. The mutex
// protects both the list ordering and the index map; callers never touch
// the underlying list/map directly.
type dnsCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	now      func() time.Time

	entries *list.List
	index   map[string]*list.Element
}

// newDNSCache constructs a cache with the given TTL and capacity.
func newDNSCache(ttl time.Duration, capacity int) *dnsCache {
	return &dnsCache{
		ttl:      ttl,
		capacity: capacity,
		now:      time.Now,
		entries:  list.New(),
		index:    make(map[string]*list.Element),
	}
}

// get returns the cached addresses for host and true when a non-expired
// entry is present; false otherwise. A hit promotes the entry to the back
// of the LRU (newest).
func (c *dnsCache) get(host string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.index[host]
	if !ok {
		return nil, false
	}

	entry, _ := el.Value.(*dnsCacheEntry)
	if c.now().After(entry.expiresAt) {
		c.entries.Remove(el)
		delete(c.index, host)

		return nil, false
	}

	c.entries.MoveToBack(el)

	out := make([]string, len(entry.addrs))
	copy(out, entry.addrs)

	return out, true
}

// put stores addrs for host with the configured TTL, evicting the oldest
// entry if the cache is at capacity. A replacement for an existing host
// key updates in place (keeping the LRU honest).
func (c *dnsCache) put(host string, addrs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	stored := make([]string, len(addrs))
	copy(stored, addrs)

	entry := &dnsCacheEntry{
		host:      host,
		addrs:     stored,
		expiresAt: c.now().Add(c.ttl),
	}

	if el, ok := c.index[host]; ok {
		el.Value = entry
		c.entries.MoveToBack(el)

		return
	}

	el := c.entries.PushBack(entry)
	c.index[host] = el

	for c.capacity > 0 && c.entries.Len() > c.capacity {
		oldest := c.entries.Front()
		if oldest == nil {
			break
		}

		c.entries.Remove(oldest)

		if oldestEntry, ok := oldest.Value.(*dnsCacheEntry); ok {
			delete(c.index, oldestEntry.host)
		}
	}
}
