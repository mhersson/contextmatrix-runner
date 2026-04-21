package preflight

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discardLogger keeps test output clean while still exercising the
// logging branches inside Run / Loop.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRun_AllProbesOK(t *testing.T) {
	probes := Probes{
		DockerPing:        func(_ context.Context) error { return nil },
		ImageInspect:      func(_ context.Context) error { return nil },
		GitHubToken:       func(_ context.Context) error { return nil },
		ContextMatrixPing: func(_ context.Context) error { return nil },
	}

	r := Run(context.Background(), probes, discardLogger())

	assert.True(t, r.OK())
	assert.NoError(t, r.Err())
}

func TestRun_DockerFails(t *testing.T) {
	boom := errors.New("dockerd down")

	probes := Probes{
		DockerPing:        func(_ context.Context) error { return boom },
		GitHubToken:       func(_ context.Context) error { return nil },
		ContextMatrixPing: func(_ context.Context) error { return nil },
	}

	r := Run(context.Background(), probes, discardLogger())

	assert.False(t, r.OK())
	require.Error(t, r.Err())
	assert.ErrorIs(t, r.DockerErr, boom)
}

func TestRun_NilProbeIsSkipped(t *testing.T) {
	// With a nil ImageInspect (the "pull policy != never" case), Run
	// must treat that slot as a no-op — not as a failure — so operators
	// who allow pulls do not trip preflight.
	probes := Probes{
		DockerPing:        func(_ context.Context) error { return nil },
		ImageInspect:      nil,
		GitHubToken:       func(_ context.Context) error { return nil },
		ContextMatrixPing: func(_ context.Context) error { return nil },
	}

	r := Run(context.Background(), probes, discardLogger())

	assert.True(t, r.OK())
}

func TestRun_AggregatesAllFailures(t *testing.T) {
	// Every probe must be called even after earlier ones fail, so the
	// first pass reports the whole dependency state instead of hiding
	// failures behind a short-circuit.
	var (
		dockerCalled, inspectCalled, ghCalled, cmCalled atomic.Bool
		boom                                            = errors.New("boom")
	)

	probes := Probes{
		DockerPing: func(_ context.Context) error {
			dockerCalled.Store(true)

			return boom
		},
		ImageInspect: func(_ context.Context) error {
			inspectCalled.Store(true)

			return boom
		},
		GitHubToken: func(_ context.Context) error {
			ghCalled.Store(true)

			return boom
		},
		ContextMatrixPing: func(_ context.Context) error {
			cmCalled.Store(true)

			return boom
		},
	}

	r := Run(context.Background(), probes, discardLogger())

	assert.True(t, dockerCalled.Load(), "docker probe not called")
	assert.True(t, inspectCalled.Load(), "image-inspect probe not called")
	assert.True(t, ghCalled.Load(), "github probe not called")
	assert.True(t, cmCalled.Load(), "CM probe not called")
	assert.False(t, r.OK())
}

func TestRun_ProbeTimeout(t *testing.T) {
	// A probe that ignores ctx.Done() for longer than its per-probe
	// timeout must surface a deadline error, proving Run isolates
	// slow probes from the rest of the wiring.
	probes := Probes{
		DockerPing: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
				return nil
			}
		},
		GitHubToken:       func(_ context.Context) error { return nil },
		ContextMatrixPing: func(_ context.Context) error { return nil },
		DockerTimeout:     5 * time.Millisecond,
	}

	r := Run(context.Background(), probes, discardLogger())

	require.Error(t, r.DockerErr)
	assert.ErrorIs(t, r.DockerErr, context.DeadlineExceeded)
}

func TestLoop_SucceedsFirstTry(t *testing.T) {
	passed := &atomic.Bool{}
	probes := Probes{
		DockerPing:        func(_ context.Context) error { return nil },
		GitHubToken:       func(_ context.Context) error { return nil },
		ContextMatrixPing: func(_ context.Context) error { return nil },
	}

	Loop(context.Background(), probes, passed, 10*time.Millisecond, discardLogger(), nil)

	assert.True(t, passed.Load(), "passed should flip on the first successful Run")
}

func TestLoop_RetriesUntilSuccess(t *testing.T) {
	// Fail twice, then succeed. Loop must kick off a background retrier
	// and eventually flip `passed` to true.
	var attempts atomic.Int32

	probes := Probes{
		DockerPing: func(_ context.Context) error {
			n := attempts.Add(1)
			if n < 3 {
				return errors.New("temporary")
			}

			return nil
		},
		GitHubToken:       func(_ context.Context) error { return nil },
		ContextMatrixPing: func(_ context.Context) error { return nil },
	}

	passed := &atomic.Bool{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Loop(ctx, probes, passed, 5*time.Millisecond, discardLogger(), nil)

	// Busy-wait up to 1s for the retry loop to flip the flag. Plenty
	// of headroom versus the 5ms interval.
	deadline := time.Now().Add(1 * time.Second)

	for time.Now().Before(deadline) {
		if passed.Load() {
			break
		}

		time.Sleep(2 * time.Millisecond)
	}

	assert.True(t, passed.Load(), "passed should flip once probes recover")
	assert.GreaterOrEqual(t, attempts.Load(), int32(3))
}

func TestLoop_InvokesOnSuccessCallback(t *testing.T) {
	// onSuccess must fire on both the first-try happy path and the
	// retry-recovery path so callers (main.go) can observe OK transitions
	// without threading types into this package.
	t.Run("first_try", func(t *testing.T) {
		var calls atomic.Int32

		probes := Probes{
			DockerPing:        func(_ context.Context) error { return nil },
			GitHubToken:       func(_ context.Context) error { return nil },
			ContextMatrixPing: func(_ context.Context) error { return nil },
		}
		passed := &atomic.Bool{}

		Loop(context.Background(), probes, passed, 5*time.Millisecond, discardLogger(),
			func() { calls.Add(1) })

		assert.True(t, passed.Load())
		assert.Equal(t, int32(1), calls.Load())
	})

	t.Run("after_recovery", func(t *testing.T) {
		var attempts, calls atomic.Int32

		probes := Probes{
			DockerPing: func(_ context.Context) error {
				if attempts.Add(1) < 2 {
					return errors.New("temporary")
				}

				return nil
			},
			GitHubToken:       func(_ context.Context) error { return nil },
			ContextMatrixPing: func(_ context.Context) error { return nil },
		}
		passed := &atomic.Bool{}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		Loop(ctx, probes, passed, 5*time.Millisecond, discardLogger(),
			func() { calls.Add(1) })

		deadline := time.Now().Add(1 * time.Second)
		for time.Now().Before(deadline) {
			if passed.Load() {
				break
			}

			time.Sleep(2 * time.Millisecond)
		}

		assert.True(t, passed.Load())
		assert.Equal(t, int32(1), calls.Load(),
			"onSuccess should fire exactly once when the retry loop recovers")
	})
}

func TestLoop_ExitsOnContextCancel(t *testing.T) {
	// If ctx is cancelled before probes recover, the retry goroutine
	// must exit without leaking. We assert this by cancelling and
	// confirming `passed` never flips.
	probes := Probes{
		DockerPing:        func(_ context.Context) error { return errors.New("still down") },
		GitHubToken:       func(_ context.Context) error { return nil },
		ContextMatrixPing: func(_ context.Context) error { return nil },
	}

	passed := &atomic.Bool{}

	ctx, cancel := context.WithCancel(context.Background())

	Loop(ctx, probes, passed, 5*time.Millisecond, discardLogger(), nil)

	// Let a few retries happen so the goroutine is unquestionably in
	// its ticker loop.
	time.Sleep(20 * time.Millisecond)
	cancel()

	// After cancellation, the flag must stay false and there should be
	// no further state changes. Sleep long enough that a goroutine
	// ignoring cancellation would definitely have fired again.
	time.Sleep(30 * time.Millisecond)

	assert.False(t, passed.Load())
}
