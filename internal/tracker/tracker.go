// Package tracker maintains a thread-safe mapping of running containers.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// stdinCloseTimeout is the maximum time Remove waits for a synchronous
// stdin.Close + onClose to complete before backgrounding the work. Declared
// as a var so tests can shrink it without a real 2-second wait.
var stdinCloseTimeout = 2 * time.Second

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
// Concurrency model:
//
//   - info.stdin (the *stdinState pointer) is written by SetStdin under
//     tracker.mu (held for write).
//   - Readers (WriteStdin, CloseStdin, MarkStdinClosed, and the chat
//     variants) capture info.stdin into a local while holding tracker.mu
//     (RLock), THEN release tracker.mu BEFORE touching stdin.mu. This
//     synchronises the field access (silencing the race detector — see
//     Fix W8 in REVIEW.md) without holding tracker.mu across the
//     potentially-blocking Write/Close on a hijacked TCP socket.
//   - Once info.stdin is allocated it is never reset to nil for that
//     entry's lifetime; only the writer field (info.stdin.stdin) is
//     nil'd on close. The pointer is therefore "set once, then stable"
//     once visible to readers — they only need to nil-check the pointer
//     to distinguish "container was never interactive" from "container
//     was interactive at some point".
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
	SessionID   string // chat-mode containers; mutually exclusive with CardID
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
	SessionID   string
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
		SessionID:   ci.SessionID,
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

// Cancel invokes the stored context.CancelFunc for the tracked entry.
// Returns false if no container is tracked; returns true if an entry exists
// (the call is a no-op if Cancel is nil).
//
// Lock discipline: the Cancel function pointer is captured under tracker.mu
// (read), then the lock is released BEFORE the cancel func is invoked. This
// mirrors Remove and prevents deadlock-by-self if the registered cancel
// func reaches back into the tracker (e.g. a CancelFunc whose closure ends
// up calling Snapshot or Has on the same Tracker). Invoking it under RLock
// would block any concurrent writer the cancel func transitively waits on.
func (t *Tracker) Cancel(project, cardID string) bool {
	t.mu.RLock()

	info, ok := t.containers[key(project, cardID)]
	if !ok {
		t.mu.RUnlock()

		return false
	}

	cancel := info.Cancel

	t.mu.RUnlock()

	if cancel != nil {
		cancel()
	}

	return true
}

// SetStdin attaches a writable stdin handle to a tracked container.
// onClose is an optional callback invoked by Remove after the writer is closed
// (e.g. to release the underlying network connection for a HijackedResponse).
//
// Behaviour and locking:
//   - Common path (entry present): tracker.mu is held for write while we
//     allocate the stdinState (if needed) and assign the writer / onClose
//     under stdin.mu nested inside tracker.mu. This matches the package-wide
//     lock order and prevents a concurrent Remove from observing a partially-
//     attached stdin.
//   - Late-arrival path (entry already Removed): tracker.mu is released
//     BEFORE we touch w/onClose, then closeWriterAsync runs the close +
//     onClose dispatch on a goroutine under stdinCloseTimeout. This is the
//     H20 + late-arrival watchdog fix: a wedged hijacked TCP socket whose
//     Close() blocks on kernel-buffer pressure or a slow peer must not be
//     able to freeze tracker.mu for the rest of the runner.
func (t *Tracker) SetStdin(project, cardID string, w io.WriteCloser, onClose func()) {
	k := key(project, cardID)

	t.mu.Lock()

	info, ok := t.containers[k]
	if !ok {
		// The entry was already Removed. Release tracker.mu BEFORE closing
		// the writer / invoking onClose so a wedged hijacked TCP socket
		// (Close blocks on kernel-buffer pressure or a slow peer) cannot
		// freeze the entire tracker for concurrent operations. We close
		// asynchronously under stdinCloseTimeout, mirroring the
		// Remove → closeStdinAsync path.
		t.mu.Unlock()
		closeWriterAsync(w, onClose, k)

		return
	}

	if info.stdin == nil {
		info.stdin = &stdinState{}
	}

	info.stdin.mu.Lock()
	info.stdin.stdin = w
	info.stdin.onClose = onClose
	info.stdin.mu.Unlock()

	t.mu.Unlock()
}

// WriteStdin writes b to the container's attached stdin. Returns:
//   - ErrNotTracked if no container is tracked for (project, cardID);
//   - ErrNoStdinAttached if the container is non-interactive (SetStdin was
//     never called);
//   - ErrStdinClosed if SetStdin WAS called but the writer has since been
//     closed (CloseStdin, Remove, or a prior /end-session).
//
// Callers map these to 404, 409, and 410 Gone respectively.
//
// Lock discipline: both the map lookup and the snapshot of info.stdin
// happen under tracker.mu (RLock). The lock is released BEFORE acquiring
// stdin.mu so a slow writer's Close on stdin.mu cannot block readers on
// tracker.mu. This is the W8 fix — the older code read info.stdin
// without synchronisation, which the race detector flagged on
// concurrent SetStdin + WriteStdin even though the access pattern is
// logically safe (a missed write returns the benign
// ErrNoStdinAttached). Capturing under RLock makes the read happens-
// before the write in SetStdin and silences the detector without
// changing the lock ordering.
func (t *Tracker) WriteStdin(project, cardID string, b []byte) error {
	t.mu.RLock()

	info, ok := t.containers[key(project, cardID)]
	if !ok {
		t.mu.RUnlock()

		return fmt.Errorf("%s/%s: %w", project, cardID, ErrNotTracked)
	}

	stdin := info.stdin

	t.mu.RUnlock()

	if stdin == nil {
		return fmt.Errorf("no stdin attached for %s/%s: %w", project, cardID, ErrNoStdinAttached)
	}

	stdin.mu.Lock()
	defer stdin.mu.Unlock()

	if stdin.stdin == nil {
		// stdin pointer exists (SetStdin was called at some point) but the
		// writer has been nil'd, meaning CloseStdin/Remove already closed
		// it. This is the /end-session-then-/message path.
		return fmt.Errorf("stdin closed for %s/%s: %w", project, cardID, ErrStdinClosed)
	}

	_, err := stdin.stdin.Write(b)

	return err
}

// MarkStdinClosed flips the tracker's view of the stdin writer to "closed"
// without invoking Close on it. Used by the priming-write timeout path,
// which force-closes the underlying writer directly (bypassing the
// tracker's lock-ordered close to avoid a deadlock against the in-flight
// Write that holds stdin.mu). After the force-close the writer is no
// longer usable, but tracker.WriteStdin would still attempt to write to
// it and surface the I/O error as a generic 500 rather than the more
// accurate 410 ErrStdinClosed. This call leaves info.stdin non-nil (so
// the entry stays addressable for Remove) but sets info.stdin.stdin to
// nil under stdin.mu, so future WriteStdin / CloseStdin return
// ErrStdinClosed.
//
// Returns ErrNotTracked if the key is unknown, ErrNoStdinAttached if
// SetStdin was never called. Safe to call multiple times.
func (t *Tracker) MarkStdinClosed(project, cardID string) error {
	t.mu.RLock()

	info, ok := t.containers[key(project, cardID)]
	if !ok {
		t.mu.RUnlock()

		return fmt.Errorf("%s/%s: %w", project, cardID, ErrNotTracked)
	}

	stdin := info.stdin

	t.mu.RUnlock()

	if stdin == nil {
		return fmt.Errorf("no stdin attached for %s/%s: %w", project, cardID, ErrNoStdinAttached)
	}

	stdin.mu.Lock()
	stdin.stdin = nil
	stdin.mu.Unlock()

	return nil
}

// MarkStdinClosedChat is the chat-mode counterpart of MarkStdinClosed. See
// MarkStdinClosed for the use case (priming-write timeout) and semantics.
func (t *Tracker) MarkStdinClosedChat(sessionID string) error {
	t.mu.RLock()

	info, ok := t.containers[chatKey(sessionID)]
	if !ok {
		t.mu.RUnlock()

		return fmt.Errorf("%s: %w", chatKey(sessionID), ErrNotTracked)
	}

	stdin := info.stdin

	t.mu.RUnlock()

	if stdin == nil {
		return fmt.Errorf("no stdin attached for %s: %w", chatKey(sessionID), ErrNoStdinAttached)
	}

	stdin.mu.Lock()
	stdin.stdin = nil
	stdin.mu.Unlock()

	return nil
}

// CloseStdin closes the attached stdin writer without removing the tracker
// entry. Used to signal EOF to a containerized claude process so it exits
// cleanly; the normal waitAndCleanup path will later call Remove.
//
// Returns:
//   - ErrNotTracked if the lookup key is unknown (including a TOCTOU where
//     the entry was removed between the caller's Snapshot and this call);
//   - ErrNoStdinAttached if no stdin was ever attached (container ran in
//     non-interactive mode — SetStdin was never called);
//   - ErrStdinClosed if SetStdin WAS called but the writer has since been
//     closed (a prior CloseStdin / /end-session, or Remove's stdin cleanup).
//
// Idempotent: a second successful call returns ErrStdinClosed (not
// ErrNoStdinAttached) so handlers can map a retried /end-session to 410
// Gone instead of 409 Conflict (which would falsely imply the container
// was never interactive). Fix W5 in REVIEW.md.
func (t *Tracker) CloseStdin(project, cardID string) error {
	t.mu.RLock()

	info, ok := t.containers[key(project, cardID)]
	if !ok {
		t.mu.RUnlock()

		return fmt.Errorf("%s/%s: %w", project, cardID, ErrNotTracked)
	}

	stdin := info.stdin

	t.mu.RUnlock()

	if stdin == nil {
		return fmt.Errorf("no stdin attached for %s/%s: %w", project, cardID, ErrNoStdinAttached)
	}

	stdin.mu.Lock()
	defer stdin.mu.Unlock()

	if stdin.stdin == nil {
		// stdin pointer exists (SetStdin was called at some point) but
		// the writer has been nil'd by a prior close. Surface this as
		// ErrStdinClosed so an idempotent /end-session retry maps to
		// 410 Gone rather than 409 Conflict. Fix W5 in REVIEW.md.
		return fmt.Errorf("stdin closed for %s/%s: %w", project, cardID, ErrStdinClosed)
	}

	err := stdin.stdin.Close()
	stdin.stdin = nil

	if err != nil {
		return fmt.Errorf("close stdin for %s/%s: %w", project, cardID, err)
	}

	return nil
}

// closeStdinAsync closes the stdin writer attached to info under a bounded
// watchdog so a wedged hijacked TCP connection cannot stall the caller.
// Must be called after the info has been deleted from the tracker map (so
// tracker.mu is no longer held). label is used only in the warning log.
//
// Lock ordering holds: tracker.mu was released before this function is
// called; stdin.mu is acquired inside the goroutine with no other lock held.
//
// Uses time.NewTimer + explicit Stop instead of time.After so the timer is
// released as soon as the close completes; otherwise the runtime keeps the
// underlying Timer pinned until the deadline elapses, matching the
// callback package's retry-loop discipline. Fix W12 in REVIEW.md.
func closeStdinAsync(info *ContainerInfo, label string) {
	done := make(chan struct{})

	go func() {
		defer close(done)

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
	}()

	timer := time.NewTimer(stdinCloseTimeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		slog.Warn("stdin close timed out in tracker.Remove; backgrounding",
			"key", label, "timeout", stdinCloseTimeout)
	}
}

// closeWriterAsync closes w and invokes onClose under a bounded watchdog.
// Used by SetStdin/SetStdinChat's late-arrival branch (entry already Removed)
// where there is no ContainerInfo to attach the writer to. Without this, a
// wedged hijacked TCP socket's Close() would block tracker.mu (held for write
// by SetStdin), freezing the entire tracker for any concurrent operation.
//
// label is used only in the warning log. Either w or onClose may be nil.
//
// Uses time.NewTimer + explicit Stop instead of time.After for parity with
// closeStdinAsync (see Fix W12 in REVIEW.md).
func closeWriterAsync(w io.Closer, onClose func(), label string) {
	if w == nil && onClose == nil {
		return
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		if w != nil {
			_ = w.Close()
		}

		if onClose != nil {
			onClose()
		}
	}()

	timer := time.NewTimer(stdinCloseTimeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		slog.Warn("stdin close timed out in tracker.SetStdin late-arrival; backgrounding",
			"key", label, "timeout", stdinCloseTimeout)
	}
}

// Remove deletes a container from the tracker and closes any attached stdin.
// tracker.mu is released before the stdin work so a slow Close on a
// hijacked connection does not stall readers; by the time we touch stdin.mu
// the entry is unreachable from any other method, so the only contention is
// with an in-flight WriteStdin/CloseStdin that captured the info pointer
// before Remove deleted the map entry — stdin.mu serialises those paths.
func (t *Tracker) Remove(project, cardID string) {
	k := key(project, cardID)

	t.mu.Lock()

	info, ok := t.containers[k]
	if ok {
		delete(t.containers, k)
	}
	t.mu.Unlock()

	if !ok {
		return
	}

	if info.Cancel != nil {
		info.Cancel()
	}

	if info.stdin != nil {
		closeStdinAsync(info, k)
	}
}

// chatKey returns the map key for a chat-mode container. The leading NUL
// byte is the sentinel: project / card_id values are constrained by the
// webhook validator's [A-Za-z0-9_.-] charset (validateIdent), so a NUL
// can never appear in a card-mode key. A literal "__chat__" prefix used
// to alias to a valid project name and could collide with a /trigger
// call for project="__chat__", card_id=<sessionID>. Mirrors the
// chatDedupSentinel pattern in webhook/message_dedup_cache.go.
func chatKey(sessionID string) string {
	return "\x00chat/" + sessionID
}

// AddChat registers a chat-mode container keyed by sessionID. Returns
// ErrAlreadyTracked if an entry for the session already exists.
func (t *Tracker) AddChat(info *ContainerInfo) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	k := chatKey(info.SessionID)
	if _, exists := t.containers[k]; exists {
		return fmt.Errorf("%s: %w", k, ErrAlreadyTracked)
	}

	t.containers[k] = info

	return nil
}

// HasChat reports whether a chat-mode container is currently tracked for
// sessionID.
func (t *Tracker) HasChat(sessionID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	_, ok := t.containers[chatKey(sessionID)]

	return ok
}

// SnapshotChat returns a read-only view of the tracked chat container for
// sessionID. The second return value is false if no container is tracked.
func (t *Tracker) SnapshotChat(sessionID string) (ContainerSnapshot, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	info, ok := t.containers[chatKey(sessionID)]
	if !ok {
		return ContainerSnapshot{}, false
	}

	return snapshotLocked(info), true
}

// RemoveChat deletes a chat-mode container from the tracker and closes any
// attached stdin.
func (t *Tracker) RemoveChat(sessionID string) {
	k := chatKey(sessionID)

	t.mu.Lock()

	info, ok := t.containers[k]
	if ok {
		delete(t.containers, k)
	}
	t.mu.Unlock()

	if !ok {
		return
	}

	if info.Cancel != nil {
		info.Cancel()
	}

	if info.stdin != nil {
		closeStdinAsync(info, k)
	}
}

// WriteStdinChat writes b to the attached stdin of the chat-mode container
// identified by sessionID. Returns ErrNotTracked, ErrNoStdinAttached, or
// ErrStdinClosed as appropriate (matching the semantics of WriteStdin).
//
// Lock discipline mirrors WriteStdin: info.stdin is captured under
// tracker.mu (RLock) so the read is correctly synchronised against
// SetStdinChat's write to that field. Fix W8 in REVIEW.md.
func (t *Tracker) WriteStdinChat(sessionID string, b []byte) error {
	t.mu.RLock()

	info, ok := t.containers[chatKey(sessionID)]
	if !ok {
		t.mu.RUnlock()

		return fmt.Errorf("%s: %w", chatKey(sessionID), ErrNotTracked)
	}

	stdin := info.stdin

	t.mu.RUnlock()

	if stdin == nil {
		return fmt.Errorf("no stdin attached for %s: %w", chatKey(sessionID), ErrNoStdinAttached)
	}

	stdin.mu.Lock()
	defer stdin.mu.Unlock()

	if stdin.stdin == nil {
		return fmt.Errorf("stdin closed for %s: %w", chatKey(sessionID), ErrStdinClosed)
	}

	_, err := stdin.stdin.Write(b)

	return err
}

// CloseStdinChat closes the attached stdin writer for the chat-mode container
// identified by sessionID without removing the tracker entry.
//
// Mirrors CloseStdin: returns ErrNotTracked if the key is unknown,
// ErrNoStdinAttached if SetStdinChat was never called, and ErrStdinClosed
// if SetStdinChat WAS called but the writer has since been closed (a prior
// CloseStdinChat, or RemoveChat's stdin cleanup). Idempotent — a second
// successful call returns ErrStdinClosed. Fix W5 in REVIEW.md.
func (t *Tracker) CloseStdinChat(sessionID string) error {
	t.mu.RLock()

	info, ok := t.containers[chatKey(sessionID)]
	if !ok {
		t.mu.RUnlock()

		return fmt.Errorf("%s: %w", chatKey(sessionID), ErrNotTracked)
	}

	stdin := info.stdin

	t.mu.RUnlock()

	if stdin == nil {
		return fmt.Errorf("no stdin attached for %s: %w", chatKey(sessionID), ErrNoStdinAttached)
	}

	stdin.mu.Lock()
	defer stdin.mu.Unlock()

	if stdin.stdin == nil {
		// stdin pointer exists (SetStdinChat was called) but writer has
		// been nil'd by a prior close. Mirror CloseStdin: return
		// ErrStdinClosed so the chat /end-session retry path can
		// distinguish "closed" from "never attached".
		return fmt.Errorf("stdin closed for %s: %w", chatKey(sessionID), ErrStdinClosed)
	}

	err := stdin.stdin.Close()
	stdin.stdin = nil

	if err != nil {
		return fmt.Errorf("close stdin for %s: %w", chatKey(sessionID), err)
	}

	return nil
}

// SetStdinChat attaches a writable stdin handle to a tracked chat-mode
// container. Mirrors SetStdin semantics:
//   - Common path (entry present): tracker.mu is held for write while we
//     allocate stdinState and assign w/onClose under stdin.mu (nested).
//   - Late-arrival path (entry already Removed): tracker.mu is released
//     BEFORE w/onClose are touched, then closeWriterAsync runs the close on
//     a goroutine under stdinCloseTimeout so a wedged hijacked TCP socket
//     cannot freeze tracker.mu for concurrent operations.
func (t *Tracker) SetStdinChat(sessionID string, w io.WriteCloser, onClose func()) {
	k := chatKey(sessionID)

	t.mu.Lock()

	info, ok := t.containers[k]
	if !ok {
		// Mirror SetStdin's late-arrival fix: release tracker.mu BEFORE
		// closing the writer / invoking onClose so a wedged hijacked TCP
		// socket cannot freeze the tracker.
		t.mu.Unlock()
		closeWriterAsync(w, onClose, k)

		return
	}

	if info.stdin == nil {
		info.stdin = &stdinState{}
	}

	info.stdin.mu.Lock()
	info.stdin.stdin = w
	info.stdin.onClose = onClose
	info.stdin.mu.Unlock()

	t.mu.Unlock()
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

// AddChatIfUnderLimit is the chat-mode counterpart of AddIfUnderLimit: it
// reserves a chat container slot keyed by sessionID under the same shared
// concurrency cap (chat and card-mode share the runner's total capacity).
// Returns ErrLimitReached or ErrAlreadyTracked, both wrapped for errors.Is.
func (t *Tracker) AddChatIfUnderLimit(info *ContainerInfo, limit int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.containers) >= limit {
		return fmt.Errorf("%w (%d)", ErrLimitReached, limit)
	}

	k := chatKey(info.SessionID)
	if _, exists := t.containers[k]; exists {
		return fmt.Errorf("%s: %w", k, ErrAlreadyTracked)
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
