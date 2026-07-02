package tracker

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func info(project, cardID string) *ContainerInfo {
	return &ContainerInfo{
		ContainerID: "ctr-" + cardID,
		CardID:      cardID,
		Project:     project,
		Image:       "test:latest",
		StartedAt:   time.Now(),
	}
}

func TestAdd_And_Snapshot(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	got, ok := tr.Snapshot("proj", "PROJ-001")
	assert.True(t, ok)
	assert.Equal(t, "ctr-PROJ-001", got.ContainerID)
}

func TestAdd_Duplicate(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	err := tr.Add(info("proj", "PROJ-001"))
	assert.ErrorContains(t, err, "already tracked")
}

func TestAdd_SameCardDifferentProject(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj-a", "TASK-001")))
	require.NoError(t, tr.Add(info("proj-b", "TASK-001")))
	assert.Equal(t, 2, tr.Count())
}

func TestSnapshot_NotFound(t *testing.T) {
	tr := New()
	_, ok := tr.Snapshot("proj", "PROJ-999")
	assert.False(t, ok)
}

func TestUpdateContainerID(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	tr.UpdateContainerID("proj", "PROJ-001", "new-ctr-id")

	got, ok := tr.Snapshot("proj", "PROJ-001")
	require.True(t, ok)
	assert.Equal(t, "new-ctr-id", got.ContainerID)
}

func TestUpdateContainerID_NotFound(_ *testing.T) {
	tr := New()
	tr.UpdateContainerID("proj", "PROJ-999", "ctr-id") // should not panic
}

func TestRemove(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))
	assert.Equal(t, 1, tr.Count())

	tr.Remove("proj", "PROJ-001")
	assert.Equal(t, 0, tr.Count())

	_, ok := tr.Snapshot("proj", "PROJ-001")
	assert.False(t, ok)
}

func TestRemove_NonExistent(t *testing.T) {
	tr := New()
	tr.Remove("proj", "PROJ-999") // should not panic
	assert.Equal(t, 0, tr.Count())
}

func TestCount(t *testing.T) {
	tr := New()
	assert.Equal(t, 0, tr.Count())

	require.NoError(t, tr.Add(info("proj", "PROJ-001")))
	assert.Equal(t, 1, tr.Count())

	require.NoError(t, tr.Add(info("proj", "PROJ-002")))
	assert.Equal(t, 2, tr.Count())
}

func TestListSnapshotsByProject(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("alpha", "A-001")))
	require.NoError(t, tr.Add(info("alpha", "A-002")))
	require.NoError(t, tr.Add(info("beta", "B-001")))

	alpha := tr.ListSnapshotsByProject("alpha")
	assert.Len(t, alpha, 2)

	beta := tr.ListSnapshotsByProject("beta")
	assert.Len(t, beta, 1)

	empty := tr.ListSnapshotsByProject("gamma")
	assert.Empty(t, empty)
}

func TestAllSnapshots(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))
	require.NoError(t, tr.Add(info("proj", "PROJ-002")))

	all := tr.AllSnapshots()
	assert.Len(t, all, 2)
}

func TestConcurrentAccess(t *testing.T) {
	tr := New()

	var wg sync.WaitGroup

	// Concurrent adds
	for i := range 50 {
		wg.Go(func() {
			cardID := "PROJ-" + string(rune('A'+i%26)) + string(rune('0'+i/26)) //nolint:gosec
			_ = tr.Add(info("proj", cardID))
		})
	}

	wg.Wait()

	// Concurrent reads
	for range 50 {
		wg.Go(func() {
			_ = tr.Count()
			_ = tr.AllSnapshots()
			_ = tr.ListSnapshotsByProject("proj")
		})
	}

	wg.Wait()

	// Concurrent removes
	for _, ci := range tr.AllSnapshots() {
		wg.Go(func() {
			tr.Remove(ci.Project, ci.CardID)
		})
	}

	wg.Wait()

	assert.Equal(t, 0, tr.Count())
}

// TestWriteStdin_NoStdinAttached verifies WriteStdin returns an error when no
// stdin has been set for the key.
func TestWriteStdin_NoStdinAttached(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	err := tr.WriteStdin("proj", "PROJ-001", []byte("hello\n"))
	assert.ErrorContains(t, err, "no stdin attached")
}

// TestWriteStdin_ErrNoStdinAttached verifies that errors.Is matches
// ErrNoStdinAttached when WriteStdin is called without stdin set.
func TestWriteStdin_ErrNoStdinAttached(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	err := tr.WriteStdin("proj", "PROJ-001", []byte("hello\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoStdinAttached,
		"expected errors.Is(err, ErrNoStdinAttached) to be true, got: %v", err)
}

// TestWriteStdin_NotTracked verifies WriteStdin returns an error when the
// container is not tracked at all.
func TestWriteStdin_NotTracked(t *testing.T) {
	tr := New()

	err := tr.WriteStdin("proj", "PROJ-999", []byte("hello\n"))
	assert.ErrorContains(t, err, "no container tracked")
}

// TestWriteStdin_AfterSetStdin verifies that WriteStdin succeeds after SetStdin
// has been called.
func TestWriteStdin_AfterSetStdin(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	pr, pw := io.Pipe()
	tr.SetStdin("proj", "PROJ-001", pw, nil)

	// Write in a goroutine so the pipe doesn't block.
	done := make(chan error, 1)

	go func() {
		done <- tr.WriteStdin("proj", "PROJ-001", []byte("hello\n"))

		_ = pw.Close()
	}()

	got, err := io.ReadAll(pr)
	require.NoError(t, err)
	require.NoError(t, <-done)
	assert.Equal(t, "hello\n", string(got))
}

// TestWriteStdin_ConcurrentNoInterleave verifies that concurrent writes from
// multiple goroutines do not interleave lines (each write is a complete line).
func TestWriteStdin_ConcurrentNoInterleave(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	pr, pw := io.Pipe()
	tr.SetStdin("proj", "PROJ-001", pw, nil)

	const (
		writers = 20
		line    = "this-is-a-whole-line\n"
	)

	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			_ = tr.WriteStdin("proj", "PROJ-001", []byte(line))
		})
	}

	// Close the write end after all goroutines finish so ReadAll terminates.
	go func() {
		wg.Wait()

		_ = pw.Close()
	}()

	got, err := io.ReadAll(pr)
	require.NoError(t, err)

	// Each scanned line must be exactly the expected line (no partial writes).
	scanner := bufio.NewScanner(bytes.NewReader(got))

	count := 0
	for scanner.Scan() {
		assert.Equal(t, "this-is-a-whole-line", scanner.Text(),
			"line %d was interleaved or truncated", count)
		count++
	}

	assert.Equal(t, writers, count, "expected %d lines, got %d", writers, count)
}

// TestRemove_ClosesStdin verifies that Remove closes the stdin writer exactly
// once, and subsequent writes via WriteStdin return an error.
func TestRemove_ClosesStdin(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	closeCount := 0

	var mu sync.Mutex

	w := &countingWriteCloser{
		closeFn: func() error {
			mu.Lock()
			defer mu.Unlock()

			closeCount++

			return nil
		},
	}
	tr.SetStdin("proj", "PROJ-001", w, nil)

	tr.Remove("proj", "PROJ-001")

	mu.Lock()
	assert.Equal(t, 1, closeCount, "stdin should be closed exactly once on Remove")
	mu.Unlock()

	// Subsequent WriteStdin must fail because the container is no longer tracked.
	err := tr.WriteStdin("proj", "PROJ-001", []byte("x"))
	assert.Error(t, err)
}

// countingWriteCloser is a WriteCloser that counts Close calls for testing.
type countingWriteCloser struct {
	closeFn func() error
}

func (c *countingWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (c *countingWriteCloser) Close() error {
	if c.closeFn != nil {
		return c.closeFn()
	}

	return nil
}

// TestWriteStdin_AfterRemoveReturnsError verifies that WriteStdin returns an
// error after Remove closes the stdin and removes the entry.
func TestWriteStdin_AfterRemoveReturnsError(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	pr, pw := io.Pipe()

	defer func() { _ = pr.Close() }()

	tr.SetStdin("proj", "PROJ-001", pw, nil)

	tr.Remove("proj", "PROJ-001")

	err := tr.WriteStdin("proj", "PROJ-001", []byte("hello"))
	assert.Error(t, err)
}

// TestRemove_InvokesOnClose verifies that Remove calls the onClose callback
// exactly once when a stdin with an onClose is registered.
func TestRemove_InvokesOnClose(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	var mu sync.Mutex

	closeCount := 0
	onClose := func() {
		mu.Lock()
		defer mu.Unlock()

		closeCount++
	}

	fw := &fakeWriteCloserSimple{}
	tr.SetStdin("proj", "PROJ-001", fw, onClose)

	tr.Remove("proj", "PROJ-001")

	mu.Lock()
	assert.Equal(t, 1, closeCount, "onClose should be called exactly once on Remove")
	mu.Unlock()
}

// TestRemove_NoOnClose verifies that Remove does not panic when no onClose
// callback was provided (nil onClose).
func TestRemove_NoOnClose(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	fw := &fakeWriteCloserSimple{}
	tr.SetStdin("proj", "PROJ-001", fw, nil)

	// Must not panic.
	assert.NotPanics(t, func() {
		tr.Remove("proj", "PROJ-001")
	})
}

// TestRemove_NoStdin verifies that Remove does not panic when no stdin was set.
func TestRemove_NoStdin(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	// Must not panic when no stdin was registered.
	assert.NotPanics(t, func() {
		tr.Remove("proj", "PROJ-001")
	})
}

// TestCloseStdin_ClosesWriter verifies CloseStdin closes the writer exactly
// once and leaves the tracker entry in place.
func TestCloseStdin_ClosesWriter(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	var mu sync.Mutex

	closeCount := 0
	w := &countingWriteCloser{
		closeFn: func() error {
			mu.Lock()
			defer mu.Unlock()

			closeCount++

			return nil
		},
	}
	tr.SetStdin("proj", "PROJ-001", w, nil)

	require.NoError(t, tr.CloseStdin("proj", "PROJ-001"))

	mu.Lock()
	assert.Equal(t, 1, closeCount, "stdin should be closed exactly once")
	mu.Unlock()

	// Tracker entry must still be present.
	_, ok := tr.Snapshot("proj", "PROJ-001")
	assert.True(t, ok, "tracker entry should remain after CloseStdin")

	// Subsequent WriteStdin should fail with ErrStdinClosed because the
	// writer was attached and then nil'd on close. The 410-vs-409 split in
	// the /message handler depends on this discrimination.
	err := tr.WriteStdin("proj", "PROJ-001", []byte("hi"))
	require.ErrorIs(t, err, ErrStdinClosed)
	require.NotErrorIs(t, err, ErrNoStdinAttached,
		"WriteStdin after CloseStdin must return ErrStdinClosed, not ErrNoStdinAttached")
}

// TestCloseStdin_Idempotent verifies the second call returns ErrStdinClosed
// (idempotent retry surfaces the "was attached, then closed" state
// so handlers can map to 410 Gone instead of 409 Conflict) and does not
// close the writer again.
func TestCloseStdin_Idempotent(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	var mu sync.Mutex

	closeCount := 0
	w := &countingWriteCloser{
		closeFn: func() error {
			mu.Lock()
			defer mu.Unlock()

			closeCount++

			return nil
		},
	}
	tr.SetStdin("proj", "PROJ-001", w, nil)

	require.NoError(t, tr.CloseStdin("proj", "PROJ-001"))

	err := tr.CloseStdin("proj", "PROJ-001")
	require.ErrorIs(t, err, ErrStdinClosed,
		"second CloseStdin must return ErrStdinClosed so handlers can map to 410 Gone")
	require.NotErrorIs(t, err, ErrNoStdinAttached,
		"second CloseStdin must NOT return ErrNoStdinAttached (would falsely imply non-interactive)")

	mu.Lock()
	assert.Equal(t, 1, closeCount, "second CloseStdin must not re-close the writer")
	mu.Unlock()
}

// TestCloseStdin_NoStdin verifies CloseStdin returns ErrNoStdinAttached when
// no stdin was set.
func TestCloseStdin_NoStdin(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	err := tr.CloseStdin("proj", "PROJ-001")
	assert.ErrorIs(t, err, ErrNoStdinAttached)
}

// TestMarkStdinClosed_FlipsState verifies that MarkStdinClosed nils the
// tracker's writer reference without invoking Close, so subsequent
// WriteStdin / CloseStdin calls return ErrStdinClosed. Used by the priming-
// write timeout path to align the tracker's state with a writer that was
// force-closed directly (bypassing CloseStdin to avoid deadlocking against
// an in-flight Write).
func TestMarkStdinClosed_FlipsState(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	var closeCount atomic.Int32

	w := &countingWriteCloser{
		closeFn: func() error {
			closeCount.Add(1)

			return nil
		},
	}
	tr.SetStdin("proj", "PROJ-001", w, nil)

	// MarkStdinClosed must succeed and must NOT invoke Close on the writer
	// — the caller (writePrimingWithTimeout) closed the underlying writer
	// itself before calling MarkStdinClosed.
	require.NoError(t, tr.MarkStdinClosed("proj", "PROJ-001"))
	assert.EqualValues(t, 0, closeCount.Load(),
		"MarkStdinClosed must not call Close on the writer")

	// Subsequent WriteStdin should return ErrStdinClosed (mapped to 410 by
	// the webhook /message handler).
	err := tr.WriteStdin("proj", "PROJ-001", []byte("hi"))
	require.ErrorIs(t, err, ErrStdinClosed)

	// CloseStdin on a Mark'd entry behaves like a normal post-close call:
	// returns ErrStdinClosed.
	err = tr.CloseStdin("proj", "PROJ-001")
	require.ErrorIs(t, err, ErrStdinClosed)

	// Idempotent: a second Mark on a closed stdin is harmless.
	require.NoError(t, tr.MarkStdinClosed("proj", "PROJ-001"))
}

// TestMarkStdinClosed_NotTracked verifies MarkStdinClosed returns
// ErrNotTracked when the lookup key is unknown — the priming-timeout caller
// uses this branch to decide between log-and-continue (entry already
// Removed by a racing cleanup) and a louder warning.
func TestMarkStdinClosed_NotTracked(t *testing.T) {
	tr := New()
	err := tr.MarkStdinClosed("proj", "MISSING")
	assert.ErrorIs(t, err, ErrNotTracked)
}

// TestMarkStdinClosed_NoStdin verifies MarkStdinClosed returns
// ErrNoStdinAttached when the entry exists but SetStdin was never called.
func TestMarkStdinClosed_NoStdin(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	err := tr.MarkStdinClosed("proj", "PROJ-001")
	assert.ErrorIs(t, err, ErrNoStdinAttached)
}

// TestMarkStdinClosedChat_FlipsState mirrors TestMarkStdinClosed_FlipsState
// for the chat-mode counterpart.
func TestMarkStdinClosedChat_FlipsState(t *testing.T) {
	tr := New()
	require.NoError(t, tr.AddChat(&ContainerInfo{
		ContainerID: "ctr-1",
		SessionID:   "01SESS",
		Image:       "test:latest",
		StartedAt:   time.Now(),
	}))

	var closeCount atomic.Int32

	w := &countingWriteCloser{
		closeFn: func() error {
			closeCount.Add(1)

			return nil
		},
	}
	tr.SetStdinChat("01SESS", w, nil)

	require.NoError(t, tr.MarkStdinClosedChat("01SESS"))
	assert.EqualValues(t, 0, closeCount.Load(),
		"MarkStdinClosedChat must not call Close on the writer")

	err := tr.WriteStdinChat("01SESS", []byte("hi"))
	require.ErrorIs(t, err, ErrStdinClosed)

	err = tr.CloseStdinChat("01SESS")
	require.ErrorIs(t, err, ErrStdinClosed)
}

// TestCloseStdin_NotTracked verifies CloseStdin returns ErrNotTracked when
// the container is not tracked.
func TestCloseStdin_NotTracked(t *testing.T) {
	tr := New()

	err := tr.CloseStdin("proj", "PROJ-999")
	assert.ErrorIs(t, err, ErrNotTracked)
}

// TestCloseStdin_ConcurrentWithRemove exercises the race between a CloseStdin
// call and a concurrent Remove. Under -race, this verifies the stdin writer is
// closed at most once across both calls and neither path panics or deadlocks.
func TestCloseStdin_ConcurrentWithRemove(t *testing.T) {
	for range 50 {
		tr := New()
		require.NoError(t, tr.Add(info("proj", "PROJ-001")))

		var mu sync.Mutex

		closeCount := 0
		w := &countingWriteCloser{
			closeFn: func() error {
				mu.Lock()
				defer mu.Unlock()

				closeCount++

				return nil
			},
		}
		tr.SetStdin("proj", "PROJ-001", w, nil)

		var wg sync.WaitGroup

		wg.Go(func() {
			_ = tr.CloseStdin("proj", "PROJ-001")
		})

		wg.Go(func() {
			tr.Remove("proj", "PROJ-001")
		})

		wg.Wait()

		mu.Lock()
		assert.Equal(t, 1, closeCount, "writer must be closed exactly once across concurrent CloseStdin+Remove")
		mu.Unlock()
	}
}

// TestCloseStdin_ConcurrentWithWrite runs many WriteStdin calls racing with a
// CloseStdin. Under -race, this verifies no use-after-close and no deadlock.
func TestCloseStdin_ConcurrentWithWrite(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	fw := &fakeWriteCloserSimple{}
	tr.SetStdin("proj", "PROJ-001", fw, nil)

	var wg sync.WaitGroup

	for range 50 {
		wg.Go(func() {
			_ = tr.WriteStdin("proj", "PROJ-001", []byte("x\n"))
		})
	}

	wg.Go(func() {
		_ = tr.CloseStdin("proj", "PROJ-001")
	})

	wg.Wait()

	// Any write after the close must now return ErrStdinClosed (the writer
	// was attached then nil'd), not ErrNoStdinAttached (never attached).
	err := tr.WriteStdin("proj", "PROJ-001", []byte("post\n"))
	assert.ErrorIs(t, err, ErrStdinClosed)
}

// TestCloseStdin_ThenRemove verifies Remove after CloseStdin does not
// double-close the writer but still invokes onClose.
func TestCloseStdin_ThenRemove(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	var mu sync.Mutex

	closeCount := 0
	onCloseCount := 0
	w := &countingWriteCloser{
		closeFn: func() error {
			mu.Lock()
			defer mu.Unlock()

			closeCount++

			return nil
		},
	}
	tr.SetStdin("proj", "PROJ-001", w, func() {
		mu.Lock()
		defer mu.Unlock()

		onCloseCount++
	})

	require.NoError(t, tr.CloseStdin("proj", "PROJ-001"))
	tr.Remove("proj", "PROJ-001")

	mu.Lock()
	assert.Equal(t, 1, closeCount, "writer must be closed exactly once across CloseStdin+Remove")
	assert.Equal(t, 1, onCloseCount, "onClose must still run from Remove")
	mu.Unlock()

	assert.Equal(t, 0, tr.Count())
}

// fakeWriteCloserSimple is a minimal WriteCloser for tests that don't need
// to inspect what was written.
type fakeWriteCloserSimple struct{}

func (f *fakeWriteCloserSimple) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeWriteCloserSimple) Close() error                { return nil }

// TestRemove_BlockingStdinClose_DoesNotBlockReturn verifies that Remove returns
// within stdinCloseTimeout even when stdin.Close() blocks indefinitely. This
// guards against the HITL 2h leak hypothesis: a wedged hijacked socket in
// Close would otherwise stall Remove and starve the removeSecretsFile /
// removeContainer defers that follow it in waitAndCleanup.
func TestRemove_BlockingStdinClose_DoesNotBlockReturn(t *testing.T) {
	// Shrink the timeout so the test is fast.
	origTimeout := stdinCloseTimeout
	stdinCloseTimeout = 50 * time.Millisecond

	t.Cleanup(func() { stdinCloseTimeout = origTimeout })

	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	// blockClose is held open until t.Cleanup releases it, simulating a
	// wedged hijacked TCP connection that never returns from Close.
	blockClose := make(chan struct{})

	t.Cleanup(func() { close(blockClose) })

	blocking := &blockingWriteCloser{block: blockClose}
	tr.SetStdin("proj", "PROJ-001", blocking, nil)

	// Remove must return within stdinCloseTimeout + generous buffer (500ms).
	removeDone := make(chan struct{})

	go func() {
		defer close(removeDone)

		tr.Remove("proj", "PROJ-001")
	}()

	deadline := stdinCloseTimeout + 500*time.Millisecond
	select {
	case <-removeDone:
		// good — Remove returned within the deadline
	case <-time.After(deadline):
		t.Fatalf("Remove blocked for more than %v; stdinCloseTimeout watchdog did not fire", deadline)
	}

	// The tracker entry must be gone regardless of whether Close completed.
	assert.False(t, tr.Has("proj", "PROJ-001"), "tracker entry must be removed even if stdin Close blocks")
}

// blockingWriteCloser is a WriteCloser whose Close blocks until the provided
// channel is closed. Used to simulate a wedged hijacked TCP connection.
type blockingWriteCloser struct {
	block <-chan struct{}
}

func (b *blockingWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (b *blockingWriteCloser) Close() error {
	<-b.block

	return nil
}

// TestAddIfUnderLimit_Concurrent verifies that under concurrent callers racing
// to reserve a slot against a tight limit, exactly `limit` goroutines succeed
// and every other caller receives the limit-reached error. This exercises the
// single-lock TOCTOU-free path in AddIfUnderLimit.
func TestAddIfUnderLimit_Concurrent(t *testing.T) {
	const (
		limit = 5
		total = 50
	)

	tr := New()

	var (
		wg           sync.WaitGroup
		successes    atomic.Int64
		limitErrors  atomic.Int64
		otherErrors  atomic.Int64
		otherSamples = make(chan error, total)
	)

	for i := range total {
		wg.Go(func() {
			ci := &ContainerInfo{
				ContainerID: "ctr",
				CardID:      "CARD-" + strconv.Itoa(i),
				Project:     "proj",
				StartedAt:   time.Now(),
			}

			err := tr.AddIfUnderLimit(ci, limit)
			switch {
			case err == nil:
				successes.Add(1)
			case strings.Contains(err.Error(), "limit reached"):
				limitErrors.Add(1)
			default:
				otherErrors.Add(1)

				select {
				case otherSamples <- err:
				default:
				}
			}
		})
	}

	wg.Wait()
	close(otherSamples)

	if otherErrors.Load() > 0 {
		for err := range otherSamples {
			t.Logf("unexpected error: %v", err)
		}
	}

	assert.Equal(t, int64(limit), successes.Load(),
		"exactly %d concurrent callers should succeed, got %d", limit, successes.Load())
	assert.Equal(t, int64(total-limit), limitErrors.Load(),
		"the remaining %d callers should receive limit-reached errors", total-limit)
	assert.Equal(t, int64(0), otherErrors.Load(),
		"no other error types are expected from AddIfUnderLimit")
	assert.Equal(t, limit, tr.Count(), "tracker must hold exactly %d containers", limit)
}

// TestErrSentinels verifies every exported tracker sentinel is wired through
// fmt.Errorf("... %w", ...) and reachable via errors.Is from the caller's
// perspective. Handlers branch on these sentinels to
// pick the right HTTP status code; if a future refactor drops the %w wrap the
// handler would silently fall through to the generic internal-error path, so
// this test is the contract that keeps that wiring honest.
func TestErrSentinels(t *testing.T) {
	tr := New()

	// ErrAlreadyTracked is returned by Add and AddIfUnderLimit.
	require.NoError(t, tr.Add(info("proj", "CARD-1")))

	err := tr.Add(info("proj", "CARD-1"))
	require.ErrorIs(t, err, ErrAlreadyTracked,
		"Add on duplicate key must wrap ErrAlreadyTracked")

	err = tr.AddIfUnderLimit(info("proj", "CARD-1"), 10)
	require.ErrorIs(t, err, ErrAlreadyTracked,
		"AddIfUnderLimit on duplicate key must wrap ErrAlreadyTracked")

	// ErrLimitReached is returned by AddIfUnderLimit at capacity.
	err = tr.AddIfUnderLimit(info("proj", "CARD-2"), 1)
	require.ErrorIs(t, err, ErrLimitReached,
		"AddIfUnderLimit at capacity must wrap ErrLimitReached")
	require.NotErrorIs(t, err, ErrAlreadyTracked,
		"limit-reached must NOT also match ErrAlreadyTracked (distinct codes)")

	// ErrNotTracked is returned by WriteStdin and CloseStdin on unknown key.
	err = tr.WriteStdin("proj", "nope", []byte("x"))
	require.ErrorIs(t, err, ErrNotTracked,
		"WriteStdin on unknown key must wrap ErrNotTracked")

	err = tr.CloseStdin("proj", "nope")
	require.ErrorIs(t, err, ErrNotTracked,
		"CloseStdin on unknown key must wrap ErrNotTracked")

	// ErrNoStdinAttached is returned by WriteStdin when SetStdin was never
	// called. (CARD-1 is tracked but has no stdin.)
	err = tr.WriteStdin("proj", "CARD-1", []byte("x"))
	require.ErrorIs(t, err, ErrNoStdinAttached,
		"WriteStdin on non-interactive entry must wrap ErrNoStdinAttached")
	require.NotErrorIs(t, err, ErrStdinClosed,
		"never-attached must NOT match ErrStdinClosed (410 vs 409 split)")

	// ErrStdinClosed is returned by WriteStdin after SetStdin + CloseStdin.
	fw := &fakeWriteCloserSimple{}
	tr.SetStdin("proj", "CARD-1", fw, nil)
	require.NoError(t, tr.CloseStdin("proj", "CARD-1"))

	err = tr.WriteStdin("proj", "CARD-1", []byte("x"))
	require.ErrorIs(t, err, ErrStdinClosed,
		"WriteStdin after CloseStdin must wrap ErrStdinClosed")
	require.NotErrorIs(t, err, ErrNoStdinAttached,
		"closed-after-attach must NOT match ErrNoStdinAttached (distinguishes 410 from 409)")
}

// TestSetStdin_RemoveRace exercises the ordering race where SetStdin and
// Remove fire concurrently on the same tracker entry. If SetStdin released
// the tracker mu before assigning info.stdin.stdin, a concurrent Remove
// could delete the entry and the late SetStdin would then attach a
// writer/onClose to a ContainerInfo no longer reachable from the tracker —
// leaking the hijacked TCP connection because no subsequent Remove would
// ever find it to call Close.
//
// Invariants verified here:
//
//  1. The writer is closed exactly once across the whole race: either by
//     Remove (if SetStdin installed it first) or by SetStdin's late-arrival
//     fallback (if Remove won and SetStdin arrived after the entry was
//     already gone).
//  2. onClose runs exactly once for the same reason — the hijacked TCP
//     connection must always be released.
//  3. No writer escapes Remove: after both goroutines return, there is no
//     tracker entry and the writer is no longer accessible via WriteStdin.
//
// The test runs many iterations with runtime.Gosched() nudges so the
// -race detector has a chance to catch unsynchronised writes to
// stdinState and that specific interleaving.
func TestSetStdin_RemoveRace(t *testing.T) {
	const iterations = 500

	for iter := range iterations {
		tr := New()
		require.NoError(t, tr.Add(info("proj", "PROJ-001")))

		var (
			mu         sync.Mutex
			closeCount int
			onCloseHit int
		)

		w := &countingWriteCloser{
			closeFn: func() error {
				mu.Lock()
				defer mu.Unlock()

				closeCount++

				return nil
			},
		}
		onClose := func() {
			mu.Lock()
			defer mu.Unlock()

			onCloseHit++
		}

		// start gate makes both goroutines wake together so every iteration
		// exercises a fresh interleaving.
		var start sync.WaitGroup

		start.Add(1)

		var wg sync.WaitGroup

		// Goroutine A: register stdin.
		wg.Go(func() {
			start.Wait()
			runtime.Gosched()
			tr.SetStdin("proj", "PROJ-001", w, onClose)
		})

		// Goroutine B: remove the entry.
		wg.Go(func() {
			start.Wait()
			runtime.Gosched()
			tr.Remove("proj", "PROJ-001")
		})

		start.Done()
		wg.Wait()

		mu.Lock()

		// Invariant #1: the writer must be closed EXACTLY once,
		// never zero and never twice. The tracker is responsible for making
		// sure the hijacked TCP conn is always released, regardless of which
		// goroutine won the race.
		assert.Equal(t, 1, closeCount,
			"iter=%d writer must be closed exactly once across SetStdin/Remove race", iter)
		// Invariant #2: onClose must run exactly once.
		assert.Equal(t, 1, onCloseHit,
			"iter=%d onClose must run exactly once across SetStdin/Remove race", iter)
		mu.Unlock()

		// Invariant #3: after the race, no tracker entry remains and the
		// writer is not accessible via WriteStdin.
		_, ok := tr.Snapshot("proj", "PROJ-001")
		assert.False(t, ok, "iter=%d tracker entry must be gone after Remove", iter)

		err := tr.WriteStdin("proj", "PROJ-001", []byte("x"))
		require.Error(t, err, "iter=%d WriteStdin must fail after Remove", iter)
	}
}

func TestTracker_ChatLifecycle(t *testing.T) {
	tr := New()
	info := &ContainerInfo{
		ContainerID: "c1",
		SessionID:   "01HFK0",
		Image:       "img",
		StartedAt:   time.Now(),
	}
	require.NoError(t, tr.AddChat(info))
	assert.True(t, tr.HasChat("01HFK0"))
	snap, ok := tr.SnapshotChat("01HFK0")
	require.True(t, ok)
	assert.Equal(t, "c1", snap.ContainerID)
	assert.Equal(t, "01HFK0", snap.SessionID)

	// Duplicate Add fails.
	err := tr.AddChat(info)
	require.ErrorIs(t, err, ErrAlreadyTracked)

	tr.RemoveChat("01HFK0")
	assert.False(t, tr.HasChat("01HFK0"))
}

func TestTracker_ChatAndCardCoexist(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(&ContainerInfo{ContainerID: "card-1", CardID: "ALPHA-001", Project: "alpha"}))
	require.NoError(t, tr.AddChat(&ContainerInfo{ContainerID: "chat-1", SessionID: "S1"}))
	assert.Equal(t, 2, tr.Count())
	assert.True(t, tr.Has("alpha", "ALPHA-001"))
	assert.True(t, tr.HasChat("S1"))
}

// TestChatKeyDoesNotCollideWithProject verifies that the chat-key namespace
// sentinel (NUL byte) cannot collide with any card-mode (project, cardID)
// key. A literal "__chat__" prefix would be a valid project name under the
// webhook validator's [A-Za-z0-9_.-] charset, so a /trigger call for
// project="__chat__", card_id=X would alias to the same map entry as
// /chat/start for session_id=X.
func TestChatKeyDoesNotCollideWithProject(t *testing.T) {
	tr := New()

	// Register a card-mode container using the "__chat__" project name that a
	// string-prefix chat key would collide with. With a "__chat__/<id>" key
	// this would alias to AddChat(SessionID=X).
	require.NoError(t, tr.Add(&ContainerInfo{
		ContainerID: "card-1",
		CardID:      "X",
		Project:     "__chat__",
	}))

	// AddChat(SessionID="X") must succeed: a separate namespace means the
	// two entries coexist; a shared namespace would return ErrAlreadyTracked.
	require.NoError(t, tr.AddChat(&ContainerInfo{
		ContainerID: "chat-1",
		SessionID:   "X",
	}))

	// Both lookups must resolve to their respective entries.
	assert.True(t, tr.Has("__chat__", "X"))
	assert.True(t, tr.HasChat("X"))

	card, ok := tr.Snapshot("__chat__", "X")
	require.True(t, ok)
	assert.Equal(t, "card-1", card.ContainerID)

	chat, ok := tr.SnapshotChat("X")
	require.True(t, ok)
	assert.Equal(t, "chat-1", chat.ContainerID)

	// And removing one must not affect the other.
	tr.RemoveChat("X")
	assert.True(t, tr.Has("__chat__", "X"))
	assert.False(t, tr.HasChat("X"))
}

func TestAddChatIfUnderLimit_Basic(t *testing.T) {
	tr := New()

	require.NoError(t, tr.AddChatIfUnderLimit(&ContainerInfo{SessionID: "S1"}, 3))
	require.NoError(t, tr.AddChatIfUnderLimit(&ContainerInfo{SessionID: "S2"}, 3))
	require.NoError(t, tr.AddChatIfUnderLimit(&ContainerInfo{SessionID: "S3"}, 3))

	// At capacity.
	err := tr.AddChatIfUnderLimit(&ContainerInfo{SessionID: "S4"}, 3)
	require.ErrorIs(t, err, ErrLimitReached)

	// Duplicate session.
	err = tr.AddChatIfUnderLimit(&ContainerInfo{SessionID: "S1"}, 10)
	require.ErrorIs(t, err, ErrAlreadyTracked)
}

// TestAddChatIfUnderLimit_SharesLimitWithCards ensures the chat concurrency
// cap is enforced against the same total container count as card-mode, so a
// runner cannot exceed its declared capacity by mixing both kinds.
func TestAddChatIfUnderLimit_SharesLimitWithCards(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(&ContainerInfo{CardID: "C1", Project: "p"}))
	require.NoError(t, tr.Add(&ContainerInfo{CardID: "C2", Project: "p"}))

	// limit=2, already at capacity from card-mode adds → chat should be rejected.
	err := tr.AddChatIfUnderLimit(&ContainerInfo{SessionID: "S1"}, 2)
	require.ErrorIs(t, err, ErrLimitReached)
}

func TestAddChatIfUnderLimit_Concurrent(t *testing.T) {
	const (
		limit = 5
		total = 50
	)

	tr := New()

	var (
		wg          sync.WaitGroup
		successes   atomic.Int64
		limitErrors atomic.Int64
		otherErrors atomic.Int64
	)

	for i := range total {
		wg.Go(func() {
			ci := &ContainerInfo{
				SessionID: "S-" + strconv.Itoa(i),
				StartedAt: time.Now(),
			}

			err := tr.AddChatIfUnderLimit(ci, limit)
			switch {
			case err == nil:
				successes.Add(1)
			case strings.Contains(err.Error(), "limit reached"):
				limitErrors.Add(1)
			default:
				otherErrors.Add(1)
			}
		})
	}

	wg.Wait()

	assert.Equal(t, int64(limit), successes.Load(),
		"exactly %d concurrent callers should succeed, got %d", limit, successes.Load())
	assert.Equal(t, int64(total-limit), limitErrors.Load())
	assert.Equal(t, int64(0), otherErrors.Load())
	assert.Equal(t, limit, tr.Count())
}

func TestWriteStdinChat_NotTracked(t *testing.T) {
	t.Parallel()

	tr := New()
	err := tr.WriteStdinChat("01ABSENT", []byte("hello"))
	require.ErrorIs(t, err, ErrNotTracked)
}

func TestWriteStdinChat_NoStdinAttached(t *testing.T) {
	t.Parallel()

	tr := New()
	require.NoError(t, tr.AddChat(&ContainerInfo{
		ContainerID: "ctr-1",
		SessionID:   "01SESS",
		Image:       "test:latest",
		StartedAt:   time.Now(),
	}))
	err := tr.WriteStdinChat("01SESS", []byte("hello"))
	require.ErrorIs(t, err, ErrNoStdinAttached)
}

func TestWriteStdinChat_StdinClosed(t *testing.T) {
	t.Parallel()

	tr := New()
	require.NoError(t, tr.AddChat(&ContainerInfo{
		ContainerID: "ctr-1",
		SessionID:   "01SESS",
		Image:       "test:latest",
		StartedAt:   time.Now(),
	}))

	// Attach and immediately close stdin.
	pr, pw := io.Pipe()
	_ = pr.Close()

	tr.SetStdinChat("01SESS", pw, nil)
	require.NoError(t, tr.CloseStdinChat("01SESS"))

	err := tr.WriteStdinChat("01SESS", []byte("hello"))
	require.ErrorIs(t, err, ErrStdinClosed)
}

func TestRemove_InvokesCancel(t *testing.T) {
	tr := New()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // belt-and-braces in case Remove regresses

	require.NoError(t, tr.Add(&ContainerInfo{
		Project: "alpha",
		CardID:  "ALPHA-1",
		Cancel:  cancel,
	}))

	tr.Remove("alpha", "ALPHA-1")

	select {
	case <-ctx.Done():
		// Expected: Remove invoked Cancel.
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Remove did not invoke stored Cancel")
	}
}

func TestRemoveChat_InvokesCancel(t *testing.T) {
	tr := New()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	require.NoError(t, tr.AddChat(&ContainerInfo{
		SessionID: "sess-1",
		Cancel:    cancel,
	}))

	tr.RemoveChat("sess-1")

	select {
	case <-ctx.Done():
	case <-time.After(50 * time.Millisecond):
		t.Fatal("RemoveChat did not invoke stored Cancel")
	}
}

// TestCancel_DoesNotHoldRLockDuringCancelFunc pins the rule that tracker.Cancel
// captures info.Cancel under tracker.mu (read) and releases the lock BEFORE
// invoking it. A CancelFunc that reaches back into the tracker (e.g. calls
// Snapshot or Has on the same Tracker) would deadlock-by-self if Cancel held
// the RLock while the cancel func tried to take it again. This matches the
// discipline used in Remove.
//
// The test exercises the reentrant read path explicitly: the cancel func
// calls Has on the same tracker. Without the fix this would still pass
// under Go's RWMutex semantics in the absence of a queued writer, so we
// also run a parallel writer that contends for tracker.mu.Lock(). Under
// Go's RWMutex starvation-avoidance rules, a writer queued after the
// initial RLock will block any subsequent RLock from inside the cancel
// func — and tracker.Cancel would never return.
func TestCancel_DoesNotHoldRLockDuringCancelFunc(t *testing.T) {
	tr := New()

	canceled := make(chan struct{})

	cancelFn := func() {
		// Reach back into the tracker from inside the cancel func. If
		// tracker.Cancel still held tracker.mu (read), and a parallel
		// writer is queued, this Has() call would block — and Cancel
		// would never return.
		_ = tr.Has("alpha", "ALPHA-1")

		close(canceled)
	}

	require.NoError(t, tr.Add(&ContainerInfo{
		Project: "alpha",
		CardID:  "ALPHA-1",
		Cancel:  cancelFn,
	}))

	// Run a parallel writer that wants tracker.mu.Lock(). If Cancel held
	// its RLock during the cancel func, the writer would queue behind that
	// RLock and starve the reentrant Has() inside the cancel func.
	writerStarted := make(chan struct{})
	writerDone := make(chan struct{})

	go func() {
		close(writerStarted)
		// Add a new entry — this needs tracker.mu.Lock().
		_ = tr.Add(&ContainerInfo{Project: "beta", CardID: "BETA-1"})

		close(writerDone)
	}()

	<-writerStarted
	// Tiny pause so the writer has a chance to queue at tracker.mu.
	time.Sleep(5 * time.Millisecond)

	if !tr.Cancel("alpha", "ALPHA-1") {
		t.Fatal("Cancel returned false for a tracked entry")
	}

	select {
	case <-canceled:
		// Good: cancel func ran to completion (which required acquiring
		// the read lock again from inside).
	case <-time.After(time.Second):
		t.Fatal("Cancel func did not complete; tracker.Cancel still holds RLock during invocation")
	}

	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("Parallel writer never completed; tracker.Cancel deadlocked the tracker")
	}
}

// TestCancel_MissingEntryReturnsFalse pins the existing behaviour: Cancel
// on an unknown (project, cardID) returns false and does not invoke any
// cancel func.
func TestCancel_MissingEntryReturnsFalse(t *testing.T) {
	tr := New()

	if got := tr.Cancel("nope", "MISS-1"); got {
		t.Fatal("Cancel must return false for an entry that is not tracked")
	}
}

// TestCancel_NilCancelFuncIsNoOp verifies that a tracked entry whose
// Cancel field is nil still returns true and does not panic.
func TestCancel_NilCancelFuncIsNoOp(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(&ContainerInfo{
		Project: "alpha",
		CardID:  "ALPHA-1",
		Cancel:  nil,
	}))

	if got := tr.Cancel("alpha", "ALPHA-1"); !got {
		t.Fatal("Cancel must return true for a tracked entry even when Cancel is nil")
	}
}

// TestCancelWithReason_RecordsReasonAndCancels verifies that CancelWithReason
// records the reason on the tracked entry before invoking its stored cancel
// func, and that Reason surfaces it afterwards. A missing entry returns false
// and leaves Reason empty.
func TestCancelWithReason_RecordsReasonAndCancels(t *testing.T) {
	tr := New()

	var cancelled atomic.Bool

	require.NoError(t, tr.Add(&ContainerInfo{
		Project: "p", CardID: "C-1",
		Cancel: func() { cancelled.Store(true) },
	}))

	require.True(t, tr.CancelWithReason("p", "C-1", "idle_timeout"))
	assert.True(t, cancelled.Load(), "CancelWithReason must invoke the stored cancel func")
	assert.Equal(t, "idle_timeout", tr.Reason("p", "C-1"), "reason must be readable after cancel")

	assert.False(t, tr.CancelWithReason("p", "missing", "x"), "missing entry returns false")
	assert.Empty(t, tr.Reason("p", "missing"))
}

// TestSetStdin_LateArrivalWedgedClose_DoesNotBlockTracker verifies that when
// SetStdin arrives AFTER Remove (the entry is gone), the late-arrival branch
// does not close the writer SYNCHRONOUSLY under tracker.mu (write). Closing
// it synchronously would let a wedged hijacked TCP socket's Close() freeze
// the entire tracker for any concurrent operation.
//
// The late-arrival branch runs the Close + onClose on a background goroutine
// bounded by stdinCloseTimeout. We verify two invariants:
//  1. SetStdin returns within the timeout even when Close wedges forever.
//  2. Other tracker operations (Has, Add, Count) remain responsive while
//     the close is wedged, proving tracker.mu was released.
func TestSetStdin_LateArrivalWedgedClose_DoesNotBlockTracker(t *testing.T) {
	origTimeout := stdinCloseTimeout
	stdinCloseTimeout = 50 * time.Millisecond

	t.Cleanup(func() { stdinCloseTimeout = origTimeout })

	tr := New()

	// Don't Add the key — SetStdin will hit the late-arrival branch
	// immediately because the entry is missing.
	blockClose := make(chan struct{})

	t.Cleanup(func() { close(blockClose) })

	blocking := &blockingWriteCloser{block: blockClose}
	onClose := func() {
		// Mirror writer behaviour: also wedge to prove the goroutine path
		// supports onClose blockage too.
		<-blockClose
	}

	setStdinReturned := make(chan struct{})

	go func() {
		defer close(setStdinReturned)

		tr.SetStdin("proj", "PROJ-999", blocking, onClose)
	}()

	// SetStdin must return within stdinCloseTimeout + a small slack.
	deadline := stdinCloseTimeout + 500*time.Millisecond
	select {
	case <-setStdinReturned:
		// good — SetStdin completed without waiting on the wedged Close.
	case <-time.After(deadline):
		t.Fatalf("SetStdin blocked for more than %v on wedged Close; M4 fix is regressed", deadline)
	}

	// Concurrent tracker ops must still be responsive — proving tracker.mu
	// was not held during the close.
	require.NoError(t, tr.Add(info("other-proj", "OTHER-1")))
	assert.True(t, tr.Has("other-proj", "OTHER-1"),
		"tracker must remain responsive while wedged Close runs in background")
	assert.Equal(t, 1, tr.Count(),
		"tracker.Count must return promptly after late-arrival SetStdin")
}

// TestSetStdinChat_LateArrivalWedgedClose_DoesNotBlockTracker mirrors the
// card-mode test for the chat-mode late-arrival branch.
func TestSetStdinChat_LateArrivalWedgedClose_DoesNotBlockTracker(t *testing.T) {
	origTimeout := stdinCloseTimeout
	stdinCloseTimeout = 50 * time.Millisecond

	t.Cleanup(func() { stdinCloseTimeout = origTimeout })

	tr := New()

	blockClose := make(chan struct{})

	t.Cleanup(func() { close(blockClose) })

	blocking := &blockingWriteCloser{block: blockClose}

	setStdinReturned := make(chan struct{})

	go func() {
		defer close(setStdinReturned)

		tr.SetStdinChat("01MISSING", blocking, nil)
	}()

	deadline := stdinCloseTimeout + 500*time.Millisecond
	select {
	case <-setStdinReturned:
		// good — SetStdinChat returned without blocking on Close.
	case <-time.After(deadline):
		t.Fatalf("SetStdinChat blocked for more than %v on wedged Close; M4 fix is regressed", deadline)
	}

	// Chat-mode tracker ops must still be responsive.
	require.NoError(t, tr.AddChat(&ContainerInfo{SessionID: "01OTHER"}))
	assert.True(t, tr.HasChat("01OTHER"))
	assert.Equal(t, 1, tr.Count())
}

// TestSetStdin_WriteStdin_RaceUnderRace verifies that concurrent
// SetStdin and WriteStdin must not produce a data race on the
// stdinState field. The invariant under test is that info.stdin is
// "set once under tracker.mu (write), then stable" — WriteStdin
// snapshots info under tracker.mu (RLock), releases the lock, and only
// then nil-checks info.stdin. The race detector (-race) catches any
// unsynchronised write to that field.
//
// A refactor that introduced a "reset to nil" path for info.stdin would race
// the lock-free nil-check inside WriteStdin, and this test would surface the
// regression.
func TestSetStdin_WriteStdin_RaceUnderRace(t *testing.T) {
	const iterations = 200

	for iter := range iterations {
		tr := New()
		require.NoError(t, tr.Add(info("proj", "PROJ-001")))

		var start sync.WaitGroup

		start.Add(1)

		var wg sync.WaitGroup

		// Multiple readers via WriteStdin and CloseStdin: each call
		// snapshots info under tracker.mu (RLock), drops the lock, and
		// then dereferences info.stdin without re-acquiring tracker.mu.
		// The -race detector flags any unsynchronised access to that
		// field if the invariant is regressed.
		for range 4 {
			wg.Go(func() {
				start.Wait()
				runtime.Gosched()

				_ = tr.WriteStdin("proj", "PROJ-001", []byte("x"))
			})
		}

		// Writer: SetStdin under tracker.mu (write).
		wg.Go(func() {
			start.Wait()
			runtime.Gosched()
			tr.SetStdin("proj", "PROJ-001", &countingWriteCloser{}, nil)
		})

		start.Done()
		wg.Wait()

		// Clean up via Remove so each iteration starts with a fresh
		// tracker and no zombie state leaks.
		tr.Remove("proj", "PROJ-001")

		// Tiny yield so the goroutine scheduler exercises different
		// orderings across iterations.
		if iter%32 == 0 {
			runtime.Gosched()
		}
	}
}
