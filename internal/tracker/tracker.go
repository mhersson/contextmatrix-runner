// Package tracker maintains a thread-safe mapping of running containers.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ErrNoStdinAttached is returned by WriteStdin when the container is tracked
// but has no interactive stdin handle attached (i.e. non-interactive mode).
var ErrNoStdinAttached = errors.New("no stdin attached")

// ErrStdinClosed is returned by WriteStdin when the container was once
// interactive (stdin was attached via SetStdin) but the writer has since
// been closed (by CloseStdin or the Remove fallback). Lets callers return
// 410 Gone instead of 409 Conflict on a post-end-session write.
var ErrStdinClosed = errors.New("stdin closed")

// ErrNotTracked is returned by CloseStdin when the lookup key is unknown.
// Lets callers distinguish the "never tracked / already Removed" case from
// "no stdin attached", since the HTTP mapping differs (404 vs 409).
var ErrNotTracked = errors.New("no container tracked")

// ErrLimitReached is returned by AddIfUnderLimit when the tracker already
// holds `limit` entries. Exposed as a sentinel so handlers can branch on
// errors.Is rather than string-matching the error text.
var ErrLimitReached = errors.New("container limit reached")

// ErrAlreadyTracked is returned by Add and AddIfUnderLimit when an entry
// for the same (project, card_id) is already present. Exposed as a sentinel
// so handlers can return a generic 409 without revealing whether the
// conflict is the same card or a different one.
var ErrAlreadyTracked = errors.New("container already tracked")

// stdinState holds the stdin writer and its mutex as a shared pointer,
// so that the live writer/onClose pair is always reachable from the tracker
// entry even while a concurrent SetStdin/Remove is racing for it.
//
// Invariant: once SetStdin allocates a stdinState for a ContainerInfo, the
// pointer is never reset to nil for that entry's lifetime. Only the writer
// field (stdin.stdin) is nil'd on close. Callers that have already acquired
// tracker mu can therefore nil-check info.stdin as an early-out, and still
// re-check the writer under stdin.mu.
//
// Locking order: tracker.mu (read or write) MUST be acquired before stdin.mu.
// The reverse order is not permitted in this package and would deadlock
// against SetStdin, which holds both locks at once.
type stdinState struct {
	mu      sync.Mutex
	stdin   io.WriteCloser
	onClose func() // optional: invoked by Remove after the writer is closed
}

// ContainerInfo holds metadata about a running container.
//
// ContainerInfo values stored inside the tracker are mutated in-place only
// through Tracker methods, so the concurrency-sensitive fields (stdin and
// Cancel) are reachable by every method that needs them. External callers
// MUST NOT construct a ContainerInfo and mutate its stdin field directly;
// stdin is unexported for that reason. External callers obtain a read-only
// view via Snapshot or ListSnapshotsByProject / AllSnapshots.
type ContainerInfo struct {
	ContainerID string
	CardID      string
	Project     string
	Image       string
	StartedAt   time.Time
	Cancel      context.CancelFunc

	// stdin is a shared pointer so the live writer is always reachable from
	// the tracker entry. Access is mediated by WriteStdin/CloseStdin/SetStdin
	// on the Tracker; callers must never touch it directly.
	stdin *stdinState
}

// ContainerSnapshot is a read-only view of a tracked container. It omits
// the concurrency-sensitive fields (stdin, Cancel) of ContainerInfo so a
// caller cannot accidentally race with a concurrent Remove or SetStdin by
// dereferencing a stale writer or cancel func. Mutations go through
// Tracker methods (Kill via Cancel, WriteStdin, CloseStdin, Remove).
type ContainerSnapshot struct {
	ContainerID string
	CardID      string
	Project     string
	Image       string
	StartedAt   time.Time
}

// snapshotLocked copies the bookkeeping-only fields from an internal
// ContainerInfo. The caller MUST hold tracker.mu.
func snapshotLocked(ci *ContainerInfo) ContainerSnapshot {
	return ContainerSnapshot{
		ContainerID: ci.ContainerID,
		CardID:      ci.CardID,
		Project:     ci.Project,
		Image:       ci.Image,
		StartedAt:   ci.StartedAt,
	}
}

// Tracker maps (project, card_id) pairs to running container info.
type Tracker struct {
	mu         sync.RWMutex
	containers map[string]*ContainerInfo
}

// New creates an empty Tracker.
func New() *Tracker {
	return &Tracker{
		containers: make(map[string]*ContainerInfo),
	}
}

func key(project, cardID string) string {
	return project + "/" + cardID
}

// Add registers a container. Returns ErrAlreadyTracked if the key already
// exists.
func (t *Tracker) Add(info *ContainerInfo) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	k := key(info.Project, info.CardID)
	if _, exists := t.containers[k]; exists {
		return fmt.Errorf("%s: %w", k, ErrAlreadyTracked)
	}

	t.containers[k] = info

	return nil
}

// Snapshot returns a read-only view of the tracked container for the given
// (project, cardID). The second return value is false if no container is
// tracked. The snapshot never exposes the stdin writer or cancel func; use
// the corresponding Tracker methods (WriteStdin, CloseStdin, Cancel) to
// interact with those concurrency-sensitive fields instead.
func (t *Tracker) Snapshot(project, cardID string) (ContainerSnapshot, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	info, ok := t.containers[key(project, cardID)]
	if !ok {
		return ContainerSnapshot{}, false
	}

	return snapshotLocked(info), true
}

// Has reports whether a container is currently tracked for (project, cardID).
// Cheaper than Snapshot when callers only need the existence check.
func (t *Tracker) Has(project, cardID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	_, ok := t.containers[key(project, cardID)]

	return ok
}

// UpdateContainerID atomically sets the container ID for a tracked entry.
func (t *Tracker) UpdateContainerID(project, cardID, containerID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if info, ok := t.containers[key(project, cardID)]; ok {
		info.ContainerID = containerID
	}
}

// Cancel invokes the stored context.CancelFunc for the tracked entry under
// the tracker mu so the entry cannot be removed mid-call. Returns false if
// no container is tracked; returns true if an entry exists (the call is a
// no-op if Cancel is nil).
func (t *Tracker) Cancel(project, cardID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	info, ok := t.containers[key(project, cardID)]
	if !ok {
		return false
	}

	if info.Cancel != nil {
		info.Cancel()
	}

	return true
}

// SetStdin attaches a writable stdin handle to a tracked container.
// onClose is an optional callback invoked by Remove after the writer is closed
// (e.g. to release the underlying network connection for a HijackedResponse).
//
// The entire operation executes under tracker.mu (held for write) so a
// concurrent Remove cannot observe a partially-attached stdin. stdin.mu is
// acquired nested inside tracker.mu, matching the package-wide lock order.
//
// H20 fix: the old implementation released tracker.mu before assigning
// info.stdin.stdin, creating a window in which a concurrent Remove could
// delete the entry and the late-arriving SetStdin would then orphan the
// writer/onClose on a ContainerInfo no longer reachable from the tracker —
// leaking the hijacked TCP connection. If SetStdin observes the entry has
// already been removed, we close the writer synchronously and invoke
// onClose so the connection still gets released.
func (t *Tracker) SetStdin(project, cardID string, w io.WriteCloser, onClose func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	info, ok := t.containers[key(project, cardID)]
	if !ok {
		// The entry was already Removed. Close the writer synchronously so
		// the underlying hijacked connection does not leak, and invoke
		// onClose so downstream cleanup (e.g. HijackedResponse.Close) still
		// runs exactly once.
		if w != nil {
			_ = w.Close()
		}

		if onClose != nil {
			onClose()
		}

		return
	}

	if info.stdin == nil {
		info.stdin = &stdinState{}
	}

	info.stdin.mu.Lock()
	info.stdin.stdin = w
	info.stdin.onClose = onClose
	info.stdin.mu.Unlock()
}

// WriteStdin writes b to the container's attached stdin. Returns:
//   - ErrNotTracked if no container is tracked for (project, cardID);
//   - ErrNoStdinAttached if the container is non-interactive (SetStdin was
//     never called);
//   - ErrStdinClosed if SetStdin WAS called but the writer has since been
//     closed (CloseStdin, Remove, or a prior /end-session).
//
// Callers map these to 404, 409, and 410 Gone respectively.
func (t *Tracker) WriteStdin(project, cardID string, b []byte) error {
	t.mu.RLock()
	info, ok := t.containers[key(project, cardID)]
	t.mu.RUnlock()

	if !ok {
		return fmt.Errorf("%s/%s: %w", project, cardID, ErrNotTracked)
	}

	if info.stdin == nil {
		return fmt.Errorf("no stdin attached for %s/%s: %w", project, cardID, ErrNoStdinAttached)
	}

	info.stdin.mu.Lock()
	defer info.stdin.mu.Unlock()

	if info.stdin.stdin == nil {
		// stdin pointer exists (SetStdin was called at some point) but the
		// writer has been nil'd, meaning CloseStdin/Remove already closed
		// it. This is the /end-session-then-/message path.
		return fmt.Errorf("stdin closed for %s/%s: %w", project, cardID, ErrStdinClosed)
	}

	_, err := info.stdin.stdin.Write(b)

	return err
}

// CloseStdin closes the attached stdin writer without removing the tracker
// entry. Used to signal EOF to a containerized claude process so it exits
// cleanly; the normal waitAndCleanup path will later call Remove.
//
// Returns ErrNotTracked if the key is unknown (including a TOCTOU where the
// entry was removed between the caller's Snapshot and this call) and
// ErrNoStdinAttached if no stdin has been set or it has already been closed.
// Idempotent: a second call returns ErrNoStdinAttached.
func (t *Tracker) CloseStdin(project, cardID string) error {
	t.mu.RLock()
	info, ok := t.containers[key(project, cardID)]
	t.mu.RUnlock()

	if !ok {
		return fmt.Errorf("%s/%s: %w", project, cardID, ErrNotTracked)
	}

	if info.stdin == nil {
		return fmt.Errorf("no stdin attached for %s/%s: %w", project, cardID, ErrNoStdinAttached)
	}

	info.stdin.mu.Lock()
	defer info.stdin.mu.Unlock()

	if info.stdin.stdin == nil {
		return fmt.Errorf("no stdin attached for %s/%s: %w", project, cardID, ErrNoStdinAttached)
	}

	err := info.stdin.stdin.Close()
	info.stdin.stdin = nil

	if err != nil {
		return fmt.Errorf("close stdin for %s/%s: %w", project, cardID, err)
	}

	return nil
}

// Remove deletes a container from the tracker and closes any attached stdin.
// tracker.mu is released before the stdin work so a slow Close on a
// hijacked connection does not stall readers; by the time we touch stdin.mu
// the entry is unreachable from any other method, so the only contention is
// with an in-flight WriteStdin/CloseStdin that captured the info pointer
// before Remove deleted the map entry — stdin.mu serialises those paths.
func (t *Tracker) Remove(project, cardID string) {
	t.mu.Lock()

	info, ok := t.containers[key(project, cardID)]
	if ok {
		delete(t.containers, key(project, cardID))
	}
	t.mu.Unlock()

	if ok && info.stdin != nil {
		info.stdin.mu.Lock()
		if info.stdin.stdin != nil {
			_ = info.stdin.stdin.Close()
			info.stdin.stdin = nil
		}

		onClose := info.stdin.onClose
		info.stdin.onClose = nil
		info.stdin.mu.Unlock()

		if onClose != nil {
			onClose()
		}
	}
}

// Count returns the number of tracked containers.
func (t *Tracker) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return len(t.containers)
}

// ListSnapshotsByProject returns read-only snapshots for every container in
// the given project. Callers cannot mutate the tracker through the returned
// values; use Cancel / WriteStdin / Remove on the Tracker instead.
func (t *Tracker) ListSnapshotsByProject(project string) []ContainerSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var result []ContainerSnapshot

	for _, info := range t.containers {
		if info.Project == project {
			result = append(result, snapshotLocked(info))
		}
	}

	return result
}

// AddIfUnderLimit atomically checks the concurrency limit and adds the
// container in a single lock acquisition, preventing TOCTOU races. Returns
// ErrLimitReached when the limit is hit, or ErrAlreadyTracked when the key
// already exists. Both errors are wrapped (via fmt.Errorf %w) so callers
// can branch on errors.Is.
func (t *Tracker) AddIfUnderLimit(info *ContainerInfo, limit int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.containers) >= limit {
		return fmt.Errorf("%w (%d)", ErrLimitReached, limit)
	}

	k := key(info.Project, info.CardID)
	if _, exists := t.containers[k]; exists {
		return fmt.Errorf("%s/%s: %w", info.Project, info.CardID, ErrAlreadyTracked)
	}

	t.containers[k] = info

	return nil
}

// AllSnapshots returns read-only snapshots of every tracked container.
func (t *Tracker) AllSnapshots() []ContainerSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]ContainerSnapshot, 0, len(t.containers))
	for _, info := range t.containers {
		result = append(result, snapshotLocked(info))
	}

	return result
}
