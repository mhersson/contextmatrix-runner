package tracker

import (
	"sync"
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

func TestAdd_And_Get(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	got, ok := tr.Get("proj", "PROJ-001")
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

func TestGet_NotFound(t *testing.T) {
	tr := New()
	_, ok := tr.Get("proj", "PROJ-999")
	assert.False(t, ok)
}

func TestUpdateContainerID(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))

	tr.UpdateContainerID("proj", "PROJ-001", "new-ctr-id")

	got, ok := tr.Get("proj", "PROJ-001")
	require.True(t, ok)
	assert.Equal(t, "new-ctr-id", got.ContainerID)
}

func TestUpdateContainerID_NotFound(t *testing.T) {
	tr := New()
	tr.UpdateContainerID("proj", "PROJ-999", "ctr-id") // should not panic
}

func TestRemove(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))
	assert.Equal(t, 1, tr.Count())

	tr.Remove("proj", "PROJ-001")
	assert.Equal(t, 0, tr.Count())

	_, ok := tr.Get("proj", "PROJ-001")
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

func TestListByProject(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("alpha", "A-001")))
	require.NoError(t, tr.Add(info("alpha", "A-002")))
	require.NoError(t, tr.Add(info("beta", "B-001")))

	alpha := tr.ListByProject("alpha")
	assert.Len(t, alpha, 2)

	beta := tr.ListByProject("beta")
	assert.Len(t, beta, 1)

	empty := tr.ListByProject("gamma")
	assert.Empty(t, empty)
}

func TestAll(t *testing.T) {
	tr := New()
	require.NoError(t, tr.Add(info("proj", "PROJ-001")))
	require.NoError(t, tr.Add(info("proj", "PROJ-002")))

	all := tr.All()
	assert.Len(t, all, 2)
}

func TestConcurrentAccess(t *testing.T) {
	tr := New()
	var wg sync.WaitGroup

	// Concurrent adds
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cardID := "PROJ-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
			_ = tr.Add(info("proj", cardID))
		}(i)
	}
	wg.Wait()

	// Concurrent reads
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tr.Count()
			_ = tr.All()
			_ = tr.ListByProject("proj")
		}()
	}
	wg.Wait()

	// Concurrent removes
	for _, ci := range tr.All() {
		wg.Add(1)
		go func(ci *ContainerInfo) {
			defer wg.Done()
			tr.Remove(ci.Project, ci.CardID)
		}(ci)
	}
	wg.Wait()

	assert.Equal(t, 0, tr.Count())
}
