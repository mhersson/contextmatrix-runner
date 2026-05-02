package logbroadcast_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingObs struct{ n atomic.Uint64 }

func (c *countingObs) ObserveDrop()  { c.n.Add(1) }
func (c *countingObs) Count() uint64 { return c.n.Load() }

// TestDropLogIsRateLimited verifies that a flood of drops does NOT produce
// one warn line per drop — only an aggregated line per interval.
func TestDropLogIsRateLimited(t *testing.T) {
	var buf safeBuf

	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(h)

	obs := &countingObs{}

	// Tight 50ms window so the test runs quickly.
	b := logbroadcast.NewBroadcasterWithInterval(logger, obs, 50*time.Millisecond)

	defer func() { _ = b.Close(context.Background()) }()

	// Slow subscriber we never drain.
	_, unsub := b.Subscribe("")
	defer unsub()

	// Burst 300 entries (> 256 buffer) so every published entry after the
	// buffer fills gets dropped.
	for range 300 {
		b.Publish(logbroadcast.LogEntry{
			Timestamp: time.Now(),
			Project:   "p",
			CardID:    "c",
			Type:      "text",
			Content:   "x",
		})
	}

	// Give the reporter a couple of ticks to flush.
	time.Sleep(160 * time.Millisecond)

	// The observer counts every drop.
	assert.Positive(t, obs.Count(), "drop observer should see every drop")

	// The logger output should contain at most a handful of warn lines — far
	// fewer than the drop count.
	logLines := strings.Count(buf.String(), "dropped for slow subscribers")
	assert.LessOrEqual(t, logLines, 5, "drop log lines should be rate-limited; got %d lines for %d drops", logLines, obs.Count())
}

type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func makeEntry(project string) logbroadcast.LogEntry {
	return logbroadcast.LogEntry{
		Timestamp: time.Now(),
		CardID:    "CARD-1",
		Project:   project,
		Type:      "text",
		Content:   "hello from " + project,
	}
}

// TestSubscribeUnsubscribeLifecycle verifies that a subscriber receives entries
// before unsubscribing and that its channel is closed afterwards.
func TestSubscribeUnsubscribeLifecycle(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	ch, unsub := b.Subscribe("")
	require.Equal(t, 1, b.SubscriberCount())

	entry := makeEntry("proj-a")
	b.Publish(entry)

	select {
	case got := <-ch:
		assert.Equal(t, entry.Project, got.Project)
		assert.Equal(t, entry.Content, got.Content)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for entry")
	}

	unsub()
	assert.Equal(t, 0, b.SubscriberCount())

	// Channel must be closed after unsubscribe.
	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel should be closed after unsubscribe")
	case <-time.After(time.Second):
		t.Fatal("channel was not closed after unsubscribe")
	}
}

// TestDoubleUnsubscribe verifies that calling unsubscribe twice does not panic.
func TestDoubleUnsubscribe(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)
	_, unsub := b.Subscribe("")
	unsub()
	assert.NotPanics(t, unsub)
}

// TestFanOutToMultipleSubscribers verifies that a published entry is delivered
// to all active subscribers.
func TestFanOutToMultipleSubscribers(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	ch1, unsub1 := b.Subscribe("")
	ch2, unsub2 := b.Subscribe("")
	ch3, unsub3 := b.Subscribe("")

	defer unsub1()
	defer unsub2()
	defer unsub3()

	require.Equal(t, 3, b.SubscriberCount())

	entry := makeEntry("proj-b")
	b.Publish(entry)

	for i, ch := range []<-chan logbroadcast.LogEntry{ch1, ch2, ch3} {
		select {
		case got := <-ch:
			assert.Equal(t, entry.Content, got.Content, "subscriber %d", i+1)
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d timed out", i+1)
		}
	}
}

// TestSlowSubscriberDrop verifies that a full subscriber buffer does not cause
// Publish to block, and that other subscribers are not affected.
func TestSlowSubscriberDrop(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	// Slow subscriber — we intentionally never read from this channel.
	_, unsubSlow := b.Subscribe("")
	defer unsubSlow()

	// Normal subscriber.
	chFast, unsubFast := b.Subscribe("")
	defer unsubFast()

	// Fill the slow subscriber's buffer (256 entries) plus a few extras to
	// trigger drops, while verifying Publish never blocks.
	done := make(chan struct{})

	go func() {
		defer close(done)

		for i := range 270 {
			b.Publish(logbroadcast.LogEntry{
				Timestamp: time.Now(),
				CardID:    "CARD-1",
				Project:   "proj-c",
				Type:      "text",
				Content:   "msg",
			})

			_ = i
		}
	}()

	select {
	case <-done:
		// Good — Publish returned without blocking.
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on slow subscriber")
	}

	// Fast subscriber should have received at least some entries (up to its
	// buffer limit of 256).
	received := 0

drain:
	for {
		select {
		case <-chFast:
			received++
		default:
			break drain
		}
	}

	assert.Positive(t, received, "fast subscriber should have received entries")
}

// TestProjectFiltering verifies that a subscriber with a project filter only
// receives entries for that project, while an all-projects subscriber gets all.
func TestProjectFiltering(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	chAll, unsubAll := b.Subscribe("")       // receives everything
	chA, unsubA := b.Subscribe("proj-alpha") // only proj-alpha
	chB, unsubB := b.Subscribe("proj-beta")  // only proj-beta

	defer unsubAll()
	defer unsubA()
	defer unsubB()

	b.Publish(makeEntry("proj-alpha"))
	b.Publish(makeEntry("proj-beta"))
	b.Publish(makeEntry("proj-gamma"))

	// All-projects subscriber should get 3 entries.
	gotAll := drainWithTimeout(t, chAll, 3, time.Second)
	assert.Len(t, gotAll, 3)

	// proj-alpha subscriber should get exactly 1 entry.
	gotA := drainWithTimeout(t, chA, 1, time.Second)
	require.Len(t, gotA, 1)
	assert.Equal(t, "proj-alpha", gotA[0].Project)

	// proj-beta subscriber should get exactly 1 entry.
	gotB := drainWithTimeout(t, chB, 1, time.Second)
	require.Len(t, gotB, 1)
	assert.Equal(t, "proj-beta", gotB[0].Project)

	// Verify proj-alpha channel has no more entries.
	select {
	case extra := <-chA:
		t.Fatalf("proj-alpha subscriber received unexpected entry: %+v", extra)
	default:
		// Good — channel is empty.
	}
}

// drainWithTimeout reads up to n entries from ch within the given timeout.
func drainWithTimeout(t *testing.T, ch <-chan logbroadcast.LogEntry, n int, timeout time.Duration) []logbroadcast.LogEntry {
	t.Helper()

	deadline := time.After(timeout)

	out := make([]logbroadcast.LogEntry, 0, n)
	for range n {
		select {
		case e := <-ch:
			out = append(out, e)
		case <-deadline:
			return out
		}
	}

	return out
}

// TestUserEntryFanOutVerbatim verifies that a "user"-typed LogEntry is delivered
// to all subscribers exactly as published — no content transformation applied.
func TestUserEntryFanOutVerbatim(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	ch1, unsub1 := b.Subscribe("")
	ch2, unsub2 := b.Subscribe("")

	defer unsub1()
	defer unsub2()

	entry := logbroadcast.LogEntry{
		Timestamp: time.Now(),
		CardID:    "CARD-42",
		Project:   "proj-hitl",
		Type:      "user",
		Content:   "What is the status of the deployment?",
	}
	b.Publish(entry)

	for i, ch := range []<-chan logbroadcast.LogEntry{ch1, ch2} {
		select {
		case got := <-ch:
			assert.Equal(t, "user", got.Type, "subscriber %d: wrong type", i+1)
			assert.Equal(t, entry.Content, got.Content, "subscriber %d: content must be unchanged", i+1)
			assert.Equal(t, entry.CardID, got.CardID, "subscriber %d: card_id must match", i+1)
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d timed out waiting for user entry", i+1)
		}
	}
}

// TestUserEntryNotRedacted is a regression test asserting that user-submitted
// content containing a Bearer token is delivered verbatim to broadcaster
// subscribers. The Broadcaster.Publish path does not redact; user-typed
// secrets are the user's own responsibility.
func TestUserEntryNotRedacted(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	ch, unsub := b.Subscribe("")
	defer unsub()

	// A Bearer token to confirm the broadcaster does not redact it.
	secretContent := "Please use Bearer abcdefghijklmnopqrstuvwxyz1234567890 for auth"

	b.Publish(logbroadcast.LogEntry{
		Timestamp: time.Now(),
		CardID:    "CARD-42",
		Project:   "proj-hitl",
		Type:      "user",
		Content:   secretContent,
	})

	select {
	case got := <-ch:
		assert.Equal(t, "user", got.Type)
		// Content must arrive exactly as published — no [REDACTED] substitution.
		assert.Equal(t, secretContent, got.Content,
			"user entry content must not be redacted by the broadcaster")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for user entry")
	}
}

// TestConcurrentSafety exercises concurrent subscribe/unsubscribe/publish to
// check for data races (run with -race).
func TestConcurrentSafety(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	const (
		goroutines = 20
		publishes  = 50
	)

	var wg sync.WaitGroup

	// Publishers.
	for range goroutines {
		wg.Go(func() {
			for range publishes {
				b.Publish(makeEntry("proj-concurrent"))
			}
		})
	}

	// Concurrent subscribe/unsubscribe.
	for range goroutines {
		wg.Go(func() {
			ch, unsub := b.Subscribe("")
			// Drain to avoid blocking publishers.
			go func() {
				for range ch { //nolint:revive
					// drain
				}
			}()

			time.Sleep(time.Millisecond)
			unsub()
		})
	}

	wg.Wait()
	// After all goroutines finish, subscriber count should be 0.
	assert.Equal(t, 0, b.SubscriberCount())
}

// TestPublish_ReleasesLockBeforeSend verifies that Publish snapshots the
// subscriber set under the lock and then sends outside of it, so a slow
// subscriber's full channel cannot starve Subscribe / Unsubscribe callers.
// Under the old RLock-held-during-send pattern, a subscriber that stayed
// full for the whole test would hold the lock's readers high enough that
// an incoming Subscribe (which acquires a write lock) would stall until
// publishing quieted. This test asserts Subscribe returns promptly even
// while a slow subscriber is backed up. CTXRUN-059 (H25).
func TestPublish_ReleasesLockBeforeSend(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	// A slow subscriber: we never drain its channel, so after 256 entries
	// the select's default branch fires. The send itself must not block
	// because the capacity-256 channel falls back to the drop path; but the
	// Publish iteration itself should still release the lock before looping.
	ch, unsub := b.Subscribe("")
	defer unsub()

	_ = ch // intentionally undrained

	// Start a burst of publishers running for the duration of the test.
	done := make(chan struct{})

	var pubWG sync.WaitGroup

	for range 4 {
		pubWG.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}

				b.Publish(logbroadcast.LogEntry{
					Timestamp: time.Now(),
					Project:   "p",
					CardID:    "c",
					Type:      "text",
					Content:   "x",
				})
			}
		})
	}

	// Measure how long a Subscribe/Unsubscribe round-trip takes while the
	// burst is live. The snapshot-outside-lock design keeps this in the
	// microsecond range; a blocking design would stall for the duration of
	// a full publisher iteration.
	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		start := time.Now()
		_, u := b.Subscribe("")
		u()

		elapsed := time.Since(start)

		// Generous slack for CI; the RLock-free-for-writers version runs in
		// well under 50ms even on the race-detector run.
		require.Less(t, elapsed, 250*time.Millisecond,
			"Subscribe+Unsubscribe must not block on a slow Publish loop; got %s", elapsed)
	}

	close(done)
	pubWG.Wait()
}
