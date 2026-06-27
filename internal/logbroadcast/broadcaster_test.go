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
	gotAll := drainWithTimeout(t, chAll, 3)
	assert.Len(t, gotAll, 3)

	// proj-alpha subscriber should get exactly 1 entry.
	gotA := drainWithTimeout(t, chA, 1)
	require.Len(t, gotA, 1)
	assert.Equal(t, "proj-alpha", gotA[0].Project)

	// proj-beta subscriber should get exactly 1 entry.
	gotB := drainWithTimeout(t, chB, 1)
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

// drainWithTimeout reads up to n entries from ch within a 1-second budget.
// All callers in this file use the same budget; if a future caller needs a
// different one, restore the timeout parameter.
func drainWithTimeout(t *testing.T, ch <-chan logbroadcast.LogEntry, n int) []logbroadcast.LogEntry {
	t.Helper()

	deadline := time.After(time.Second)

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
// subscribers. The Broadcaster.Publish path does not invoke logparser.Redact;
// redaction only occurs inside logparser.ProcessStream (for assistant text/
// thinking blocks) and the stderr scanner in container/manager.go.
// User-typed secrets are the user's own responsibility.
func TestUserEntryNotRedacted(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

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

// TestLogBroadcaster_SessionIDFilter verifies that a SubscribeWithSessionID
// subscriber only receives entries whose SessionID matches the registered value,
// while a project subscriber and an all-entries subscriber behave normally.
func TestLogBroadcaster_SessionIDFilter(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	chAll, unsubAll := b.Subscribe("")                        // receives all
	chSessA, unsubA := b.SubscribeWithSessionID("sess-alpha") // only sess-alpha
	chSessB, unsubB := b.SubscribeWithSessionID("sess-beta")  // only sess-beta
	chProj, unsubProj := b.Subscribe("proj-card")             // only proj-card entries

	defer unsubAll()
	defer unsubA()
	defer unsubB()
	defer unsubProj()

	// Publish three entries: one for sess-alpha, one for sess-beta, one card entry.
	b.Publish(logbroadcast.LogEntry{SessionID: "sess-alpha", Type: "text", Content: "from alpha"})
	b.Publish(logbroadcast.LogEntry{SessionID: "sess-beta", Type: "text", Content: "from beta"})
	b.Publish(logbroadcast.LogEntry{Project: "proj-card", CardID: "CARD-1", Type: "text", Content: "from card"})

	// All-entries subscriber gets all three.
	gotAll := drainWithTimeout(t, chAll, 3)
	assert.Len(t, gotAll, 3)

	// sess-alpha subscriber gets exactly the alpha entry.
	gotA := drainWithTimeout(t, chSessA, 1)
	require.Len(t, gotA, 1)
	assert.Equal(t, "sess-alpha", gotA[0].SessionID)
	assert.Equal(t, "from alpha", gotA[0].Content)

	// sess-beta subscriber gets exactly the beta entry.
	gotB := drainWithTimeout(t, chSessB, 1)
	require.Len(t, gotB, 1)
	assert.Equal(t, "sess-beta", gotB[0].SessionID)
	assert.Equal(t, "from beta", gotB[0].Content)

	// proj-card subscriber gets only the card entry.
	gotProj := drainWithTimeout(t, chProj, 1)
	require.Len(t, gotProj, 1)
	assert.Equal(t, "proj-card", gotProj[0].Project)

	// Verify no extra entries leaked to session subscribers.
	select {
	case extra := <-chSessA:
		t.Fatalf("sess-alpha subscriber received unexpected entry: %+v", extra)
	default:
	}

	select {
	case extra := <-chSessB:
		t.Fatalf("sess-beta subscriber received unexpected entry: %+v", extra)
	default:
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
// If the RLock were held during send, a subscriber that stayed
// full for the whole test would hold the lock's readers high enough that
// an incoming Subscribe (which acquires a write lock) would stall until
// publishing quieted. This test asserts Subscribe returns promptly even
// while a slow subscriber is backed up.
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

// panicSlogHandler is a slog.Handler that panics on every Handle call.
// Used by TestDropReporter_RecoversFromPanic to inject a deterministic
// panic into the broadcaster's drop-reporter goroutine without racing on
// runtime internals.
type panicSlogHandler struct{}

func (panicSlogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (panicSlogHandler) Handle(_ context.Context, _ slog.Record) error {
	panic("simulated slog handler panic")
}
func (h panicSlogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h panicSlogHandler) WithGroup(_ string) slog.Handler      { return h }

// panicObs records every ObservePanic call. Safe for concurrent use because
// the drop-reporter goroutine is the only producer.
type panicObs struct {
	mu     sync.Mutex
	labels []string
}

func (p *panicObs) ObservePanic(goroutine string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.labels = append(p.labels, goroutine)
}

func (p *panicObs) Labels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]string, len(p.labels))
	copy(out, p.labels)

	return out
}

// TestDropReporter_RecoversFromPanic verifies that a panic inside the
// drop-reporter goroutine (e.g. a misbehaving slog handler) is recovered,
// the PanicObserver is notified, and Close still returns instead of
// deadlocking on reporterDone. Mirrors recoverBackgroundGoroutine semantics
// for the main.go-managed long-lived goroutines.
func TestDropReporter_RecoversFromPanic(t *testing.T) {
	obs := &countingObs{}
	pobs := &panicObs{}

	// A slog logger whose handler panics on every record. flushDrops calls
	// logger.Warn after observing at least one drop, which triggers the panic.
	logger := slog.New(panicSlogHandler{})

	// Tight interval so the reporter ticks quickly.
	b := logbroadcast.NewBroadcasterWithIntervalAndPanicObserver(logger, obs, pobs, 25*time.Millisecond)

	// Slow subscriber we never drain → drops accumulate → reporter calls Warn → panic.
	_, unsub := b.Subscribe("")
	defer unsub()

	// Burst enough entries to ensure the buffer fills and drops accumulate.
	for range 300 {
		b.Publish(logbroadcast.LogEntry{
			Timestamp: time.Now(),
			Project:   "p",
			CardID:    "c",
			Type:      "text",
			Content:   "x",
		})
	}

	// Wait for the reporter to tick and panic-recover.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(pobs.Labels()) > 0 {
			break
		}

		time.Sleep(25 * time.Millisecond)
	}

	labels := pobs.Labels()
	require.NotEmpty(t, labels, "PanicObserver must be notified when the drop-reporter panics")
	assert.Equal(t, "broadcaster_drop_reporter", labels[0], "panic label must match the broadcaster's drop-reporter goroutine")

	// Close must not block on reporterDone — the recover defer is wired
	// before the close-reporterDone defer so the goroutine still exits.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()

	assert.NoError(t, b.Close(closeCtx))
}

// TestPublish_TruncatesOversizedContent verifies the maxLogEntryContentBytes
// cap: a 1 MiB Content survives publish without panicking and the
// delivered entry is truncated with the documented marker so subscribers
// cannot pin worst-case heap memory.
func TestPublish_TruncatesOversizedContent(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	defer func() { _ = b.Close(context.Background()) }()

	ch, unsub := b.Subscribe("")
	defer unsub()

	// 1 MiB exceeds the documented 64 KiB cap; the truncation must not panic.
	big := strings.Repeat("X", 1<<20)
	b.Publish(logbroadcast.LogEntry{
		Project: "p",
		Type:    "text",
		Content: big,
	})

	select {
	case got := <-ch:
		assert.LessOrEqual(t, len(got.Content), 64*1024,
			"truncated Content must not exceed the documented cap")
		assert.True(t, strings.HasSuffix(got.Content, "…[truncated]"),
			"truncated Content must end with the documented marker; got tail %q",
			got.Content[len(got.Content)-15:])
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the published entry")
	}
}

// TestPublish_PreservesSmallContent verifies that the truncation guard is
// inert for normal-sized entries: anything under the cap must pass through
// byte-for-byte.
func TestPublish_PreservesSmallContent(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	defer func() { _ = b.Close(context.Background()) }()

	ch, unsub := b.Subscribe("")
	defer unsub()

	const payload = "small payload that should survive verbatim"
	b.Publish(logbroadcast.LogEntry{
		Project: "p",
		Type:    "text",
		Content: payload,
	})

	select {
	case got := <-ch:
		assert.Equal(t, payload, got.Content)
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the published entry")
	}
}

// TestPublish_AfterCloseDropsSilently verifies that Publish gates on the
// closed channel: any entry published after Close is discarded without
// fanning out to surviving subscribers. Without the gate, a late Publish
// could race the unsubscribe path and leak a half-sent entry.
func TestPublish_AfterCloseDropsSilently(t *testing.T) {
	b := logbroadcast.NewBroadcaster(nil, nil)

	ch, unsub := b.Subscribe("")
	defer unsub()

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()

	require.NoError(t, b.Close(closeCtx))

	// Publishing post-close must be a no-op; the subscriber's channel
	// already closed (or never receives). Reading drains the closed channel.
	b.Publish(logbroadcast.LogEntry{
		Project: "p",
		Type:    "text",
		Content: "should be ignored",
	})

	// Read at most one entry; we don't care whether we observe the
	// closed-channel zero-value or a timeout — what matters is no
	// post-close entry is delivered.
	select {
	case got, ok := <-ch:
		assert.False(t, ok, "channel should be closed; received entry instead: %+v", got)
	case <-time.After(100 * time.Millisecond):
		// also acceptable
	}
}
