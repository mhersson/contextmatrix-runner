package webhook

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestReplayCache_FirstSeenFalseSecondTrue(t *testing.T) {
	c := NewReplayCache(5*time.Minute, 100)

	assert.False(t, c.Seen("abc"), "first sighting must return false")
	assert.True(t, c.Seen("abc"), "second sighting must return true")
	assert.True(t, c.Seen("abc"), "third sighting must still return true")

	// Different signature is independent.
	assert.False(t, c.Seen("def"))
	assert.True(t, c.Seen("def"))
}

func TestReplayCache_EmptySigIsNeverSeen(t *testing.T) {
	c := NewReplayCache(5*time.Minute, 100)

	// Empty signatures short-circuit so callers don't accidentally
	// collapse all missing-header cases into one dedup bucket.
	assert.False(t, c.Seen(""))
	assert.False(t, c.Seen(""))
	assert.Equal(t, 0, c.Len())
}

func TestReplayCache_ExpiredAfterTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := &mockClock{now: now}

	c := NewReplayCache(330*time.Second, 100, WithReplayCacheNow(clock.Now))

	assert.False(t, c.Seen("sig-1"))
	assert.True(t, c.Seen("sig-1"), "within TTL, replay rejected")

	// Advance past the TTL — the next sighting should be treated as fresh.
	clock.advance(400 * time.Second)
	assert.False(t, c.Seen("sig-1"), "after TTL, signature is fresh again")
	assert.True(t, c.Seen("sig-1"))
}

func TestReplayCache_SweepRemovesExpired(t *testing.T) {
	clock := &mockClock{now: time.Unix(1_700_000_000, 0)}
	c := NewReplayCache(60*time.Second, 100, WithReplayCacheNow(clock.Now))

	c.Seen("sig-1")
	c.Seen("sig-2")
	c.Seen("sig-3")
	assert.Equal(t, 3, c.Len())

	clock.advance(90 * time.Second)
	c.sweep()

	assert.Equal(t, 0, c.Len(), "all entries should have been swept")
}

func TestReplayCache_ConcurrentSafe(t *testing.T) {
	c := NewReplayCache(5*time.Minute, 10_000)

	const (
		workers   = 16
		perWorker = 500
	)

	var wg sync.WaitGroup

	wg.Add(workers)

	var dupes atomic.Int64

	for w := range workers {
		go func(w int) {
			defer wg.Done()

			// First half are worker-unique (should all be false).
			for i := range perWorker {
				sig := "w" + strconv.Itoa(w) + "-i" + strconv.Itoa(i)
				if c.Seen(sig) {
					dupes.Add(1)
				}
			}
			// Second half replays the same signatures, must all come back true.
			for i := range perWorker {
				sig := "w" + strconv.Itoa(w) + "-i" + strconv.Itoa(i)
				if !c.Seen(sig) {
					t.Errorf("expected replay hit for %s", sig)

					return
				}
			}
		}(w)
	}

	wg.Wait()

	assert.Equal(t, int64(0), dupes.Load(), "worker-unique signatures must never collide on first call")
}

func TestReplayCache_CapacityEviction(t *testing.T) {
	c := NewReplayCache(0, 3) // ttl disabled, capacity 3

	c.Seen("a")
	c.Seen("b")
	c.Seen("c")
	assert.Equal(t, 3, c.Len())

	// Fourth entry evicts "a".
	c.Seen("d")
	assert.Equal(t, 3, c.Len())
	assert.False(t, c.Seen("a"), "'a' was evicted, must be seen as fresh")

	// Now "a" is back in; adding it pushed out "b".
	assert.Equal(t, 3, c.Len())
	// Confirm "b" was evicted next.
	c.Seen("e") // evicts something
	// At this point only the 3 most recently touched remain.
	// Order of inserts: a,b,c -> d (evict a) -> a (evict b) -> e (evict c).
	// Expected survivors: d, a, e.
	assert.True(t, c.Seen("d"))
	assert.True(t, c.Seen("e"))
	// 'c' was evicted.
	assert.False(t, c.Seen("c"), "'c' was evicted when 'e' was inserted")
}

func TestReplayCache_Run_StopsOnContextCancel(t *testing.T) {
	c := NewReplayCache(time.Second, 10)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		c.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// --- helpers ---

type mockClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *mockClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *mockClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}
