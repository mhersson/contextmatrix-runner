// Package tracker maintains a thread-safe mapping of running containers.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNotTracked is returned when the lookup key is unknown.
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

// ContainerInfo holds metadata about a running container.
type ContainerInfo struct {
	ContainerID string
	CardID      string
	Project     string
	Image       string
	StartedAt   time.Time
	Cancel      context.CancelFunc
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

// Remove deletes a container from the tracker.
func (t *Tracker) Remove(project, cardID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.containers, key(project, cardID))
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
