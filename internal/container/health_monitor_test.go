package container

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fastTickerInterval is the per-test probe cadence; tight enough to run the
// failure-counter logic within ~50ms of wall time while still leaving a
// few-ms cushion above scheduling jitter. Single var so the unparam
// linter does not complain about a helper parameter that never varies.
const fastTickerInterval = 5 * time.Millisecond

// withFastTicker shrinks the production probe cadence to fastTickerInterval
// for the duration of a test. The returned cleanup restores the original
// values so tests can run in any order.
func withFastTicker(t *testing.T) {
	t.Helper()

	origInterval := healthProbeIntervalVar
	origTimeout := healthProbeTimeoutVar

	healthProbeIntervalVar = fastTickerInterval
	healthProbeTimeoutVar = fastTickerInterval

	t.Cleanup(func() {
		healthProbeIntervalVar = origInterval
		healthProbeTimeoutVar = origTimeout
	})
}

// withExitStub swaps exitFn for a recorder so MonitorDockerd's os.Exit
// branch is observable without killing the test process. Returns a
// pointer whose int value is the captured exit code (0 if never called).
func withExitStub(t *testing.T) *atomic.Int32 {
	t.Helper()

	var captured atomic.Int32

	origExit := exitFn
	exitFn = func(code int) {
		// Clamp to int32 so gosec is happy; no real exit code
		// exceeds this range.
		c := int32(math.MinInt32)
		switch {
		case code > math.MaxInt32:
			c = math.MaxInt32
		case code >= math.MinInt32:
			c = int32(code) //nolint:gosec // guarded by the switch above
		}

		captured.Store(c)
	}

	t.Cleanup(func() { exitFn = origExit })

	return &captured
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMonitorDockerd_ExitsOnThreeConsecutiveFailures(t *testing.T) {
	withFastTicker(t)
	captured := withExitStub(t)

	boom := errors.New("dockerd unreachable")

	var pings atomic.Int32

	mock := &MockDockerClient{
		PingFn: func(_ context.Context) error {
			pings.Add(1)

			return boom
		},
	}

	ctx := t.Context()

	done := make(chan struct{})

	go func() {
		MonitorDockerd(ctx, mock, discardLogger())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("MonitorDockerd did not exit within 1s of 3 failures")
	}

	assert.EqualValues(t, 1, captured.Load(), "exitFn should be called with code 1")
	assert.GreaterOrEqual(t, pings.Load(), int32(healthFailureLimit),
		"expected at least %d pings before exit", healthFailureLimit)
}

func TestMonitorDockerd_CounterResetsOnSuccess(t *testing.T) {
	withFastTicker(t)
	captured := withExitStub(t)

	// Sequence: fail, fail, succeed, fail, fail. Never 3 in a row.
	// exitFn must not fire.
	sequence := []error{
		errors.New("1"),
		errors.New("2"),
		nil,
		errors.New("3"),
		errors.New("4"),
	}

	var idx atomic.Int32

	mock := &MockDockerClient{
		PingFn: func(_ context.Context) error {
			i := int(idx.Add(1)) - 1
			if i >= len(sequence) {
				// After the scripted sequence, stay healthy so we
				// can cancel the loop without triggering exit.
				return nil
			}

			return sequence[i]
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		MonitorDockerd(ctx, mock, discardLogger())
		close(done)
	}()

	// Wait until at least the scripted sequence has been consumed.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && int(idx.Load()) < len(sequence) {
		time.Sleep(2 * time.Millisecond)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("MonitorDockerd did not exit after context cancellation")
	}

	assert.EqualValues(t, 0, captured.Load(),
		"exitFn must not be called: never 3 consecutive failures")
}

func TestMonitorDockerd_ExitsCleanlyOnContextCancel(t *testing.T) {
	withFastTicker(t)
	captured := withExitStub(t)

	mock := &MockDockerClient{
		PingFn: func(_ context.Context) error { return nil },
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		MonitorDockerd(ctx, mock, discardLogger())
		close(done)
	}()

	time.Sleep(15 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("MonitorDockerd did not exit after context cancellation")
	}

	assert.EqualValues(t, 0, captured.Load(),
		"exitFn must not be called when shutdown is via ctx cancel")
}

// TestMonitorDockerd_PanicInPingDoesNotKillLoop verifies the per-iteration
// defer recovers a panic inside PingFn (typically a bug in the Docker SDK
// or a corrupted internal state on a reconnect attempt) so the monitor
// keeps its ticker cadence and failure counter alive. Without it a single
// panic unwinds the whole goroutine; main.go's outer recover-and-respawn
// loop catches that but the fresh failure counter starts from zero, masking
// crashlooping dockerd behind a fresh counter.
func TestMonitorDockerd_PanicInPingDoesNotKillLoop(t *testing.T) {
	withFastTicker(t)
	captured := withExitStub(t)

	var pings atomic.Int32

	mock := &MockDockerClient{
		PingFn: func(_ context.Context) error {
			n := pings.Add(1)
			if n == 1 {
				panic("simulated docker SDK panic")
			}

			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		MonitorDockerd(ctx, mock, discardLogger())
		close(done)
	}()

	// Wait for at least 3 pings to confirm the loop survived the panic on
	// the first iteration and produced subsequent successes.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && pings.Load() < 3 {
		time.Sleep(2 * time.Millisecond)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("MonitorDockerd did not exit after context cancellation")
	}

	assert.GreaterOrEqual(t, pings.Load(), int32(3),
		"loop must keep ticking after a recovered panic")
	assert.EqualValues(t, 0, captured.Load(),
		"a recovered panic must not trigger exitFn(1)")
}

// TestMonitorDockerd_PanicLoopTripsExit verifies that a panic on every
// iteration is counted as a probe failure and trips the 3-strike
// exitFn(1) path just like consecutive regular ping errors do. A recover
// that only logged would let a panic-loop in the Docker SDK panic forever
// without supervisor restart.
func TestMonitorDockerd_PanicLoopTripsExit(t *testing.T) {
	withFastTicker(t)
	captured := withExitStub(t)

	var pings atomic.Int32

	mock := &MockDockerClient{
		PingFn: func(_ context.Context) error {
			pings.Add(1)

			panic("simulated unrecoverable docker SDK panic")
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan struct{})

	go func() {
		MonitorDockerd(ctx, mock, discardLogger())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("MonitorDockerd did not exit within 1s of 3 panic-loop iterations")
	}

	assert.EqualValues(t, 1, captured.Load(),
		"a panic-loop must trigger exitFn(1) after healthFailureLimit consecutive panics")
	assert.GreaterOrEqual(t, pings.Load(), int32(healthFailureLimit),
		"loop must have ticked at least healthFailureLimit times before exit")
}
