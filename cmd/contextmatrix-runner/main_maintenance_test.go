package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// stubMaintenanceTarget records every CleanupOrphans / PruneImages call so
// the loop tests can assert the tick cadence without standing up a real
// Docker mock.
type stubMaintenanceTarget struct {
	cleanupCalls atomic.Int32
	pruneCalls   atomic.Int32
	cleanupErr   error
	pruneErr     error
}

func (s *stubMaintenanceTarget) CleanupOrphans(_ context.Context) error {
	s.cleanupCalls.Add(1)

	return s.cleanupErr
}

func (s *stubMaintenanceTarget) PruneImages(_ context.Context) error {
	s.pruneCalls.Add(1)

	return s.pruneErr
}

// stubMaintenanceHealth is the drain-flag view used by the loop. Using a
// bool pointer the test toggles directly keeps the test free of the webhook
// package.
type stubMaintenanceHealth struct {
	draining atomic.Bool
}

func (s *stubMaintenanceHealth) DrainingLoad() bool {
	return s.draining.Load()
}

// TestMaintenanceLoop_CallsCleanupAndPrune verifies that each ticker tick
// calls both CleanupOrphans and PruneImages, and that errors from either do
// not stop the loop.
func TestMaintenanceLoop_CallsCleanupAndPrune(t *testing.T) {
	target := &stubMaintenanceTarget{
		cleanupErr: errors.New("synthetic cleanup error"),
	}
	health := &stubMaintenanceHealth{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		runMaintenanceLoopWithHealth(ctx, target, 20*time.Millisecond, health, testLogger())
	}()

	// Wait for a handful of ticks.
	assert.Eventually(t, func() bool {
		return target.cleanupCalls.Load() >= 3 && target.pruneCalls.Load() >= 3
	}, 2*time.Second, 10*time.Millisecond,
		"maintenance loop must call both CleanupOrphans and PruneImages on each tick")

	// Cleanup + prune must be called the same number of times — the loop
	// never skips one half after a failure from the other.
	assert.Equal(t, target.cleanupCalls.Load(), target.pruneCalls.Load(),
		"a Cleanup failure must not skip the subsequent Prune")

	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("maintenance loop did not exit on ctx cancel")
	}
}

// TestMaintenanceLoop_ExitsOnDrain verifies that flipping the drain flag
// causes the goroutine to exit on the next tick without running another
// pass.
func TestMaintenanceLoop_ExitsOnDrain(t *testing.T) {
	target := &stubMaintenanceTarget{}
	health := &stubMaintenanceHealth{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		runMaintenanceLoopWithHealth(ctx, target, 20*time.Millisecond, health, testLogger())
	}()

	// Let at least one tick land to prove the loop is actually running.
	assert.Eventually(t, func() bool {
		return target.cleanupCalls.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond, "loop must run at least one tick")

	health.draining.Store(true)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance loop did not exit on drain within 2s")
	}

	// Record the counts at exit. Give a little headroom to show no
	// subsequent ticks land after drain.
	cleanupAtExit := target.cleanupCalls.Load()
	pruneAtExit := target.pruneCalls.Load()

	time.Sleep(80 * time.Millisecond)

	assert.Equal(t, cleanupAtExit, target.cleanupCalls.Load(),
		"no cleanup ticks must fire after drain exit")
	assert.Equal(t, pruneAtExit, target.pruneCalls.Load(),
		"no prune ticks must fire after drain exit")
}

// TestMaintenanceLoop_NonPositiveIntervalIsNoop checks the defensive branch:
// a zero/negative interval should log and return immediately without
// spinning a runaway ticker.
func TestMaintenanceLoop_NonPositiveIntervalIsNoop(t *testing.T) {
	target := &stubMaintenanceTarget{}
	health := &stubMaintenanceHealth{}

	done := make(chan struct{})

	go func() {
		defer close(done)

		runMaintenanceLoopWithHealth(context.Background(), target, 0, health, testLogger())
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("maintenance loop must return immediately on non-positive interval")
	}

	assert.Equal(t, int32(0), target.cleanupCalls.Load(), "no ticks must fire when disabled")
}
