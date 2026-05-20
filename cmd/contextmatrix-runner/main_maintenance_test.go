package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// stubMaintenanceTarget records every CleanupOrphans / PruneImages /
// SweepStaleChatResumeDirs call so the loop tests can assert the tick cadence
// without standing up a real Docker mock.
type stubMaintenanceTarget struct {
	cleanupCalls atomic.Int32
	pruneCalls   atomic.Int32
	sweepCalls   atomic.Int32
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

func (s *stubMaintenanceTarget) SweepStaleChatResumeDirs(_ time.Duration) {
	s.sweepCalls.Add(1)
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
		return target.cleanupCalls.Load() >= 3 && target.pruneCalls.Load() >= 3 && target.sweepCalls.Load() >= 3
	}, 2*time.Second, 10*time.Millisecond,
		"maintenance loop must call CleanupOrphans, PruneImages, and SweepStaleChatResumeDirs on each tick")

	// All three operations must be called the same number of times — the loop
	// never skips one after a failure from another.
	assert.Equal(t, target.cleanupCalls.Load(), target.pruneCalls.Load(),
		"a Cleanup failure must not skip the subsequent Prune")
	assert.Equal(t, target.pruneCalls.Load(), target.sweepCalls.Load(),
		"a Prune failure must not skip the subsequent Sweep")

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

	ctx := t.Context()

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
	sweepAtExit := target.sweepCalls.Load()

	time.Sleep(80 * time.Millisecond)

	assert.Equal(t, cleanupAtExit, target.cleanupCalls.Load(),
		"no cleanup ticks must fire after drain exit")
	assert.Equal(t, pruneAtExit, target.pruneCalls.Load(),
		"no prune ticks must fire after drain exit")
	assert.Equal(t, sweepAtExit, target.sweepCalls.Load(),
		"no sweep ticks must fire after drain exit")
}

// TestMaintenanceLoop_NonPositiveIntervalBlocksUntilCancel verifies the
// defensive branch:
//
//   - A zero/negative interval logs one warning and then blocks on
//     ctx.Done() instead of returning. The outer respawn-with-backoff wrapper
//     in main() would otherwise interpret a clean return as a panic-recover
//     and respawn the goroutine every ~5 s for the process lifetime, spamming
//     the log with "loop disabled" warnings.
//   - No ticks fire while disabled.
//   - The goroutine exits cleanly once ctx is cancelled.
func TestMaintenanceLoop_NonPositiveIntervalBlocksUntilCancel(t *testing.T) {
	target := &stubMaintenanceTarget{}
	health := &stubMaintenanceHealth{}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		defer close(done)

		runMaintenanceLoopWithHealth(ctx, target, 0, health, testLogger())
	}()

	// The loop must NOT return on its own while ctx is alive. Sample for
	// long enough to catch a spurious return that would respawn-loop in
	// production wiring.
	select {
	case <-done:
		t.Fatal("disabled loop returned before ctx cancel — outer respawn wrapper would busy-loop")
	case <-time.After(250 * time.Millisecond):
		// expected: still blocked
	}

	assert.Equal(t, int32(0), target.cleanupCalls.Load(), "no ticks must fire when disabled")
	assert.Equal(t, int32(0), target.sweepCalls.Load(), "no sweep ticks must fire when disabled")

	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("maintenance loop did not exit on ctx cancel after disabled-config block")
	}
}

// TestMaintenanceLoop_DisabledDoesNotProduceRepeatedWarnings verifies the
// disabled path emits exactly one WARN (not one per respawn-backoff tick).
// Without C2 the outer respawn loop in main() would log "maintenance loop
// disabled" every ~5 s for the process lifetime.
func TestMaintenanceLoop_DisabledDoesNotProduceRepeatedWarnings(t *testing.T) {
	target := &stubMaintenanceTarget{}
	health := &stubMaintenanceHealth{}

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		defer close(done)

		runMaintenanceLoopWithHealth(ctx, target, 0, health, logger)
	}()

	time.Sleep(250 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("disabled loop did not exit on ctx cancel")
	}

	got := strings.Count(buf.String(), "maintenance loop disabled")
	assert.Equal(t, 1, got, "disabled config must log exactly one WARN (got %d in %q)", got, buf.String())
}
