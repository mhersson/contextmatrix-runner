package container

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

func newTestManager(t *testing.T, mock *MockDockerClient, trk *tracker.Tracker) *Manager {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	return NewManager(mock, trk, logger)
}

func newTrackedEntry(t *testing.T, project, cardID string) *tracker.Tracker {
	t.Helper()

	trk := tracker.New()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	_ = ctx

	require.NoError(t, trk.Add(&tracker.ContainerInfo{
		Project:   project,
		CardID:    cardID,
		StartedAt: time.Now(),
		Cancel:    cancel,
	}))

	return trk
}

func TestKill_Tracked(t *testing.T) {
	trk := newTrackedEntry(t, "p1", "C-1")
	mgr := newTestManager(t, &MockDockerClient{}, trk)

	require.NoError(t, mgr.Kill("p1", "C-1"))
}

func TestKill_NotTracked(t *testing.T) {
	mgr := newTestManager(t, &MockDockerClient{}, tracker.New())

	err := mgr.Kill("p1", "C-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no container tracked")
}

func TestListManaged_FlagsTrackerDivergence(t *testing.T) {
	trk := newTrackedEntry(t, "p1", "C-1")
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
			return []DockerContainer{
				{
					ID:      "abcdef0123456789",
					Names:   []string{"/p1-C-1"},
					Labels:  map[string]string{LabelRunner: "true", LabelProject: "p1", LabelCardID: "C-1"},
					State:   "running",
					Created: time.Now().Unix(),
				},
				{
					ID:      "fedcba9876543210",
					Names:   []string{"/p1-C-2-orphan"},
					Labels:  map[string]string{LabelRunner: "true", LabelProject: "p1", LabelCardID: "C-2"},
					State:   "exited",
					Created: time.Now().Unix(),
				},
			}, nil
		},
	}
	mgr := newTestManager(t, mock, trk)

	got, err := mgr.ListManaged(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2)

	byCard := map[string]ManagedContainer{}
	for _, c := range got {
		byCard[c.CardID] = c
	}

	assert.True(t, byCard["C-1"].Tracked, "C-1 is in tracker; Tracked must be true")
	assert.False(t, byCard["C-2"].Tracked, "C-2 is not tracked; Tracked must be false")
	assert.Equal(t, "p1-C-1", byCard["C-1"].ContainerName, "leading slash on docker name must be stripped")
}

func TestListManaged_SkipsContainersWithoutLabels(t *testing.T) {
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
			return []DockerContainer{
				{
					ID:     "abc",
					Labels: map[string]string{LabelRunner: "true"}, // missing project / card_id
				},
				{
					ID:     "def",
					Labels: map[string]string{LabelRunner: "true", LabelProject: "p1", LabelCardID: "C-1"},
				},
			}, nil
		},
	}
	mgr := newTestManager(t, mock, tracker.New())

	got, err := mgr.ListManaged(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "C-1", got[0].CardID)
}

func TestListManaged_DockerError(t *testing.T) {
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
			return nil, errors.New("daemon unreachable")
		},
	}
	mgr := newTestManager(t, mock, tracker.New())

	_, err := mgr.ListManaged(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list managed containers")
}

func TestForceRemoveByLabels_RemovesEveryMatch(t *testing.T) {
	removed := map[string]bool{}
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
			return []DockerContainer{
				{ID: "id1", Labels: map[string]string{LabelRunner: "true", LabelProject: "p1", LabelCardID: "C-1"}},
				{ID: "id2", Labels: map[string]string{LabelRunner: "true", LabelProject: "p1", LabelCardID: "C-1"}},
			}, nil
		},
		ContainerRemoveFn: func(_ context.Context, id string, _ container.RemoveOptions) error {
			removed[id] = true

			return nil
		},
	}
	mgr := newTestManager(t, mock, tracker.New())

	count, err := mgr.ForceRemoveByLabels(context.Background(), "p1", "C-1")
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.True(t, removed["id1"])
	assert.True(t, removed["id2"])
}

func TestForceRemoveByLabels_RequiresProjectAndCard(t *testing.T) {
	mgr := newTestManager(t, &MockDockerClient{}, tracker.New())

	_, err := mgr.ForceRemoveByLabels(context.Background(), "", "C-1")
	require.Error(t, err)

	_, err = mgr.ForceRemoveByLabels(context.Background(), "p1", "")
	require.Error(t, err)
}

func TestForceRemoveByLabels_PartialFailure(t *testing.T) {
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
			return []DockerContainer{
				{ID: "id1", Labels: map[string]string{LabelRunner: "true", LabelProject: "p1", LabelCardID: "C-1"}},
				{ID: "id2", Labels: map[string]string{LabelRunner: "true", LabelProject: "p1", LabelCardID: "C-1"}},
			}, nil
		},
		ContainerRemoveFn: func(_ context.Context, id string, _ container.RemoveOptions) error {
			if id == "id1" {
				return errors.New("docker says no")
			}

			return nil
		},
	}
	mgr := newTestManager(t, mock, tracker.New())

	count, err := mgr.ForceRemoveByLabels(context.Background(), "p1", "C-1")
	require.Error(t, err)
	assert.Equal(t, 1, count, "the second match still succeeds even after first failure")
}

func TestCleanupOrphans_SkipsTrackedContainers(t *testing.T) {
	trk := newTrackedEntry(t, "p1", "live")
	stopped := map[string]bool{}
	removed := map[string]bool{}
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
			return []DockerContainer{
				{ID: "live-id", Labels: map[string]string{LabelRunner: "true", LabelProject: "p1", LabelCardID: "live"}},
				{ID: "orphan-id", Labels: map[string]string{LabelRunner: "true", LabelProject: "p1", LabelCardID: "orphan"}},
			}, nil
		},
		ContainerStopFn: func(_ context.Context, id string, _ container.StopOptions) error {
			stopped[id] = true

			return nil
		},
		ContainerRemoveFn: func(_ context.Context, id string, _ container.RemoveOptions) error {
			removed[id] = true

			return nil
		},
	}
	mgr := newTestManager(t, mock, trk)

	require.NoError(t, mgr.CleanupOrphans(context.Background()))
	assert.False(t, stopped["live-id"], "tracked container must not be stopped")
	assert.False(t, removed["live-id"], "tracked container must not be removed")
	assert.True(t, stopped["orphan-id"], "untracked orphan must be stopped")
	assert.True(t, removed["orphan-id"], "untracked orphan must be removed")
}

func TestCleanupOrphans_DockerListError(t *testing.T) {
	mock := &MockDockerClient{
		ContainerListFn: func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
			return nil, errors.New("daemon unreachable")
		},
	}
	mgr := newTestManager(t, mock, tracker.New())

	err := mgr.CleanupOrphans(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list orphan containers")
}

func TestPruneImages_PassesDanglingFilter(t *testing.T) {
	var got filters.Args

	mock := &MockDockerClient{
		ImagesPruneFn: func(_ context.Context, args filters.Args) (image.PruneReport, error) {
			got = args

			return image.PruneReport{}, nil
		},
	}
	mgr := newTestManager(t, mock, tracker.New())

	require.NoError(t, mgr.PruneImages(context.Background()))
	assert.True(t, got.ExactMatch("dangling", "true"))
	assert.True(t, got.ExactMatch("until", imagePruneMaxAge))
}

func TestPruneImages_PropagatesDockerError(t *testing.T) {
	mock := &MockDockerClient{
		ImagesPruneFn: func(_ context.Context, _ filters.Args) (image.PruneReport, error) {
			return image.PruneReport{}, errors.New("boom")
		},
	}
	mgr := newTestManager(t, mock, tracker.New())

	err := mgr.PruneImages(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "images prune")
}

func TestWait_NoOp(t *testing.T) {
	// Wait is a no-op retained for backward compatibility with shutdown
	// sequences. It must return promptly even if no goroutines are alive.
	mgr := newTestManager(t, &MockDockerClient{}, tracker.New())

	done := make(chan struct{})

	go func() {
		mgr.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait blocked; expected no-op")
	}
}
