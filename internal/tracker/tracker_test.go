package tracker

import (
	"bufio"
	"bytes"
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

// TestCloseStdin_Idempotent verifies the second call returns ErrNoStdinAttached
// and does not close the writer again.
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
	require.ErrorIs(t, err, ErrNoStdinAttached)

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
// perspective (CTXRUN-056 C8 / M21). Handlers branch on these sentinels to
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
// Remove fire concurrently on the same tracker entry. H20 in REVIEW.md
// flagged that the old SetStdin released the tracker mu before assigning
// info.stdin.stdin, so a concurrent Remove could delete the entry and the
// late SetStdin would then attach a writer/onClose to a ContainerInfo that
// was no longer reachable from the tracker — leaking the hijacked TCP
// connection because no subsequent Remove would ever find it to call Close.
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
// stdinState and the original H20 interleaving specifically.
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

		// Invariant #1 (H20 fix): the writer must be closed EXACTLY once,
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
