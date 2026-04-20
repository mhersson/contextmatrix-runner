package logbroadcast_test

import (
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	b := logbroadcast.NewBroadcaster()

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
	b := logbroadcast.NewBroadcaster()
	_, unsub := b.Subscribe("")
	unsub()
	assert.NotPanics(t, unsub)
}

// TestFanOutToMultipleSubscribers verifies that a published entry is delivered
// to all active subscribers.
func TestFanOutToMultipleSubscribers(t *testing.T) {
	b := logbroadcast.NewBroadcaster()

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
	b := logbroadcast.NewBroadcaster()

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
	b := logbroadcast.NewBroadcaster()

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
	b := logbroadcast.NewBroadcaster()

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
// subscribers. The Broadcaster.Publish path does not invoke logparser.Redact;
// redaction only occurs inside logparser.ProcessStream (for assistant text/
// thinking blocks) and the stderr scanner in container/manager.go.
// User-typed secrets are the user's own responsibility.
func TestUserEntryNotRedacted(t *testing.T) {
	b := logbroadcast.NewBroadcaster()

	ch, unsub := b.Subscribe("")
	defer unsub()

	// A Bearer token that logparser.Redact would normally replace with [REDACTED].
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
	b := logbroadcast.NewBroadcaster()

	const (
		goroutines = 20
		publishes  = 50
	)

	var wg sync.WaitGroup

	// Publishers.
	for range goroutines {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range publishes {
				b.Publish(makeEntry("proj-concurrent"))
			}
		}()
	}

	// Concurrent subscribe/unsubscribe.
	for range goroutines {
		wg.Add(1)

		go func() {
			defer wg.Done()

			ch, unsub := b.Subscribe("")
			// Drain to avoid blocking publishers.
			go func() {
				for range ch { //nolint:revive
					// drain
				}
			}()

			time.Sleep(time.Millisecond)
			unsub()
		}()
	}

	wg.Wait()
	// After all goroutines finish, subscriber count should be 0.
	assert.Equal(t, 0, b.SubscriberCount())
}
