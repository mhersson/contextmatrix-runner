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

// stdinState holds the stdin writer and its mutex as a shared pointer,
// so that copies of ContainerInfo still reference the live state.
type stdinState struct {
	mu      sync.Mutex
	stdin   io.WriteCloser
	onClose func() // optional: invoked by Remove after the writer is closed
}

// ContainerInfo holds metadata about a running container.
type ContainerInfo struct {
	ContainerID string
	CardID      string
	Project     string
	Image       string
	StartedAt   time.Time
	Cancel      context.CancelFunc

	// stdin is a shared pointer so copies of ContainerInfo see live state.
	// Accessed only via Tracker.WriteStdin and Tracker.SetStdin; callers must
	// not read or write it through copied ContainerInfo values because the
	// mutex is in the stdinState, not the copy.
	stdin *stdinState
}

// copy returns a shallow copy of the ContainerInfo. The Cancel and stdin
// fields are shared intentionally so callers can still cancel the container
// and write to stdin via the accessor methods on Tracker.
func (ci *ContainerInfo) copy() *ContainerInfo {
	return &ContainerInfo{
		ContainerID: ci.ContainerID,
		CardID:      ci.CardID,
		Project:     ci.Project,
		Image:       ci.Image,
		StartedAt:   ci.StartedAt,
		Cancel:      ci.Cancel, // shared intentionally
		stdin:       ci.stdin,  // shared intentionally; use WriteStdin accessor
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

// Add registers a container. Returns an error if the key already exists.
func (t *Tracker) Add(info *ContainerInfo) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	k := key(info.Project, info.CardID)
	if _, exists := t.containers[k]; exists {
		return fmt.Errorf("container already tracked for %s", k)
	}

	t.containers[k] = info

	return nil
}

// Get looks up a container by project and card ID.
// Returns a copy of the internal state.
func (t *Tracker) Get(project, cardID string) (*ContainerInfo, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	info, ok := t.containers[key(project, cardID)]
	if !ok {
		return nil, false
	}

	return info.copy(), true
}

// UpdateContainerID atomically sets the container ID for a tracked entry.
func (t *Tracker) UpdateContainerID(project, cardID, containerID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if info, ok := t.containers[key(project, cardID)]; ok {
		info.ContainerID = containerID
	}
}

// SetStdin attaches a writable stdin handle to a tracked container.
// onClose is an optional callback invoked by Remove after the writer is closed
// (e.g. to release the underlying network connection for a HijackedResponse).
func (t *Tracker) SetStdin(project, cardID string, w io.WriteCloser, onClose func()) {
	t.mu.Lock()

	info, ok := t.containers[key(project, cardID)]
	if ok && info.stdin == nil {
		info.stdin = &stdinState{}
	}
	t.mu.Unlock()

	if !ok {
		return
	}

	info.stdin.mu.Lock()
	info.stdin.stdin = w
	info.stdin.onClose = onClose
	info.stdin.mu.Unlock()
}

// WriteStdin writes b to the container's attached stdin. Returns an error if
// no stdin handle is attached or the container is not tracked.
func (t *Tracker) WriteStdin(project, cardID string, b []byte) error {
	t.mu.RLock()
	info, ok := t.containers[key(project, cardID)]
	t.mu.RUnlock()

	if !ok {
		return fmt.Errorf("no container tracked for %s/%s", project, cardID)
	}

	if info.stdin == nil {
		return fmt.Errorf("no stdin attached for %s/%s: %w", project, cardID, ErrNoStdinAttached)
	}

	info.stdin.mu.Lock()
	defer info.stdin.mu.Unlock()

	if info.stdin.stdin == nil {
		return fmt.Errorf("no stdin attached for %s/%s: %w", project, cardID, ErrNoStdinAttached)
	}

	_, err := info.stdin.stdin.Write(b)

	return err
}

// Remove deletes a container from the tracker and closes any attached stdin.
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

// ListByProject returns all containers for a given project.
// Returns copies of the internal state.
func (t *Tracker) ListByProject(project string) []*ContainerInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var result []*ContainerInfo

	for _, info := range t.containers {
		if info.Project == project {
			result = append(result, info.copy())
		}
	}

	return result
}

// AddIfUnderLimit atomically checks the concurrency limit and adds the
// container in a single lock acquisition, preventing TOCTOU races.
func (t *Tracker) AddIfUnderLimit(info *ContainerInfo, limit int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.containers) >= limit {
		return fmt.Errorf("container limit reached (%d)", limit)
	}

	k := key(info.Project, info.CardID)
	if _, exists := t.containers[k]; exists {
		return fmt.Errorf("container already tracked for %s/%s", info.Project, info.CardID)
	}

	t.containers[k] = info

	return nil
}

// All returns all tracked containers.
// Returns copies of the internal state.
func (t *Tracker) All() []*ContainerInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]*ContainerInfo, 0, len(t.containers))
	for _, info := range t.containers {
		result = append(result, info.copy())
	}

	return result
}
