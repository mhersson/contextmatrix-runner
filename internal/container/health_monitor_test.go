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

// withFastTicker shrinks the production probe cadence to the supplied
// duration for the duration of a test. The returned cleanup restores the
// original values so tests can run in any order.
func withFastTicker(t *testing.T, interval, timeout time.Duration) {
	t.Helper()

	origInterval := healthProbeIntervalVar
	origTimeout := healthProbeTimeoutVar

	healthProbeIntervalVar = interval
	healthProbeTimeoutVar = timeout

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
	withFastTicker(t, 5*time.Millisecond, 5*time.Millisecond)
	captured := withExitStub(t)

	boom := errors.New("dockerd unreachable")

	var pings atomic.Int32

	mock := &MockDockerClient{
		PingFn: func(_ context.Context) error {
			pings.Add(1)

			return boom
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	withFastTicker(t, 5*time.Millisecond, 5*time.Millisecond)
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
	withFastTicker(t, 5*time.Millisecond, 5*time.Millisecond)
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
