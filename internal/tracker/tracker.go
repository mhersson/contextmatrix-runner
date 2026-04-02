// Package tracker maintains a thread-safe mapping of running containers.
package tracker

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ContainerInfo holds metadata about a running container.
type ContainerInfo struct {
	ContainerID string
	CardID      string
	Project     string
	Image       string
	StartedAt   time.Time
	Cancel      context.CancelFunc
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
func (t *Tracker) Get(project, cardID string) (*ContainerInfo, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	info, ok := t.containers[key(project, cardID)]
	return info, ok
}

// UpdateContainerID atomically sets the container ID for a tracked entry.
func (t *Tracker) UpdateContainerID(project, cardID, containerID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if info, ok := t.containers[key(project, cardID)]; ok {
		info.ContainerID = containerID
	}
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

// ListByProject returns all containers for a given project.
func (t *Tracker) ListByProject(project string) []*ContainerInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var result []*ContainerInfo
	for _, info := range t.containers {
		if info.Project == project {
			result = append(result, info)
		}
	}
	return result
}

// All returns all tracked containers.
func (t *Tracker) All() []*ContainerInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]*ContainerInfo, 0, len(t.containers))
	for _, info := range t.containers {
		result = append(result, info)
	}
	return result
}
