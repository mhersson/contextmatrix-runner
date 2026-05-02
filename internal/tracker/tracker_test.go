package tracker

import (
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
// perspective. Handlers branch on these sentinels to pick the right HTTP
// status code; if a future refactor drops the %w wrap the handler would
// silently fall through to the generic internal-error path, so this test is
// the contract that keeps that wiring honest.
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
