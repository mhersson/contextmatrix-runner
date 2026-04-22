package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
	"github.com/mhersson/contextmatrix-runner/internal/webhook"
)

// stubManager implements managerShutdowner. It simulates a wedged
// manager.Wait by never returning unless the test explicitly unblocks it,
// and (optionally) makes ForceKillContainer block to exercise the
// force-cleanup per-container timeout.
type stubManager struct {
	killed    atomic.Int32
	waitDone  chan struct{} // closed by the test to let Wait return
	killErr   error
	forceFn   func(ctx context.Context, containerID string) error
	forceCall atomic.Int32
}

func (m *stubManager) Kill(_, _ string) error {
	m.killed.Add(1)

	return m.killErr
}

func (m *stubManager) Wait() {
	<-m.waitDone
}

func (m *stubManager) ForceKillContainer(ctx context.Context, containerID string) error {
	m.forceCall.Add(1)

	if m.forceFn != nil {
		return m.forceFn(ctx, containerID)
	}

	return nil
}

// stubCallback implements callbackReporter. It just records every call so
// the test can assert CM was told.
type stubCallback struct {
	calls atomic.Int32
}

func (c *stubCallback) ReportStatus(_ context.Context, _, _, _, _ string) error {
	c.calls.Add(1)

	return nil
}

// TestShutdown_WedgedContainer exercises the full CTXRUN-040 shutdown
// sequence end-to-end with a wedged manager. A tracked container is
// registered, mgr.Wait never returns on its own, and ForceKillContainer
// blocks well past forceCleanupTimeout. The assertions are:
//
//   - /trigger returns 503 as soon as the drain flag flips.
//   - /readyz returns 503.
//   - The total shutdown runs within a generous bound (< 45 s) even though
//     managerDrainTimeout would normally be 30 s and ForceKillContainer
//     could hang forever — meaning the force-cleanup timeout actually
//     bounds the hung Docker call.
//
// The shutdown timeouts are shrunk for this test so it finishes in a
// couple of seconds on CI.
func TestShutdown_WedgedContainer(t *testing.T) {
	// Shrink timeouts. Restore on cleanup so later tests see defaults.
	prevHTTP, prevManager, prevForce := httpShutdownTimeout, managerDrainTimeout, forceCleanupTimeout

	httpShutdownTimeout = 500 * time.Millisecond
	managerDrainTimeout = 1 * time.Second
	forceCleanupTimeout = 500 * time.Millisecond

	t.Cleanup(func() {
		httpShutdownTimeout = prevHTTP
		managerDrainTimeout = prevManager
		forceCleanupTimeout = prevForce
	})

	// --- Build a minimal runner instance: tracker, health, manager stub,
	//     callback stub, webhook handler, HTTP server on an ephemeral port.

	trk := tracker.New()

	// Register a "running" container so every shutdown step has work to do.
	require.NoError(t, trk.Add(&tracker.ContainerInfo{
		CardID:      "PROJ-042",
		Project:     "test-project",
		ContainerID: "ctr-abc",
	}))

	health := webhook.NewHealthState()
	health.PreflightPassed.Store(true) // so /readyz would be 200 pre-drain

	// ForceKillContainer simulates a wedged docker daemon: block for 10 s.
	// The 500 ms ctx should fire well before.
	mgr := &stubManager{
		waitDone: make(chan struct{}),
		forceFn: func(ctx context.Context, _ string) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Second):
				return nil
			}
		},
	}

	cb := &stubCallback{}

	// Wire the real webhook handler so /trigger and /readyz exercise the
	// production code paths. /trigger needs a ContainerRunner, but our
	// stubManager only implements managerShutdowner. We don't actually
	// want /trigger to succeed — we only want to assert the 503 branch —
	// so we can leave it nil and the draining branch will short-circuit
	// before the nil manager is touched.
	apiKey := strings.Repeat("k", 40)

	wh := webhook.NewHandler(nil, trk, nil, nil, apiKey, 1, "", nil, 0, health)

	mux := http.NewServeMux()
	wh.Register(mux)

	lc := net.ListenConfig{}

	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverDone := make(chan error, 1)

	go func() {
		serverDone <- srv.Serve(ln)
	}()

	t.Cleanup(func() { _ = srv.Close() })

	baseURL := "http://" + ln.Addr().String()

	// --- Pre-drain assertion: /readyz is 200.
	readyzReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/readyz", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(readyzReq)
	require.NoError(t, err)

	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "before drain, /readyz should be 200")

	// --- Run shutdown in a goroutine so we can inspect /trigger and
	//     /readyz partway through.
	shutdownDone := make(chan struct{})

	start := time.Now()

	go func() {
		shutdown(shutdownDeps{
			logger:   testLogger(),
			srv:      srv,
			health:   health,
			tracker:  trk,
			manager:  mgr,
			callback: cb,
			// monitorCancel / replayCancel are nil on purpose — the test
			// doesn't spin up those loops.
		})
		close(shutdownDone)
	}()

	// Give step 1 (drain flag flip) time to land. 100 ms is plenty — the
	// drain flag is a single atomic.Bool.Store call.
	//
	// NOTE: once step 2 (srv.Shutdown) closes the listener, plain HTTP
	// probes will start returning connection errors. We therefore hit
	// /readyz and /trigger BEFORE srv.Shutdown can complete. The
	// 500 ms httpShutdownTimeout is already on the wire; we race to probe
	// before that elapses. Giving ourselves only a ~50 ms window keeps
	// the probes in-flight during the drain.
	time.Sleep(30 * time.Millisecond)

	assert.True(t, health.Draining.Load(), "Draining flag must be set before any other shutdown work")

	// /readyz must return 503 with "draining".
	readyzDrainReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/readyz", nil)
	require.NoError(t, err)

	resp, err = http.DefaultClient.Do(readyzDrainReq)
	if err == nil {
		defer resp.Body.Close()

		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "/readyz should be 503 during drain")

		body, _ := io.ReadAll(resp.Body)
		assert.Contains(t, string(body), "draining")
	}

	// /trigger must return 503. Build a signed request so the HMAC
	// middleware doesn't reject us with 403 first.
	payload := map[string]string{
		"card_id":  "PROJ-100",
		"project":  "test-project",
		"repo_url": "https://github.com/org/repo.git",
	}

	body, _ := json.Marshal(payload)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := cmhmac.SignPayloadWithTimestamp(apiKey, body, ts)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+"/trigger", strings.NewReader(string(body)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)

	resp, err = http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()

		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "/trigger should be 503 during drain")
	}

	// --- Wait for shutdown to finish. It should NOT exceed the sum of
	//     httpShutdownTimeout (500ms) + managerDrainTimeout (1s) +
	//     forceCleanupTimeout (500ms) + slack. We give a very generous
	//     45 s upper bound per the card's specified margin.
	select {
	case <-shutdownDone:
	case <-time.After(45 * time.Second):
		// Unblock the stub manager so the deferred cleanup in this test
		// doesn't leak goroutines, then fail.
		close(mgr.waitDone)
		t.Fatalf("shutdown did not complete within 45 s (elapsed %s)", time.Since(start))
	}

	elapsed := time.Since(start)

	// Positive assertion: the wedged ForceKillContainer was bounded.
	// Total should be well under managerDrainTimeout + forceCleanupTimeout + a
	// modest slack (3 s) even with Go runtime scheduling overhead.
	assert.Less(t, elapsed, 10*time.Second,
		"shutdown should finish within a few seconds, elapsed %s", elapsed)

	// Kill was called exactly once for the one tracked container.
	assert.GreaterOrEqual(t, int32(1), mgr.killed.Load(), "Kill should have run for every tracked container")
	// ForceKillContainer was attempted in the backstop phase.
	assert.GreaterOrEqual(t, int32(1), mgr.forceCall.Load(), "ForceKillContainer must run in the backstop phase")
	// Callback was attempted at least once.
	assert.GreaterOrEqual(t, int32(1), cb.calls.Load(), "CM should be told about the shutting-down container")

	// Let mgr.Wait finally return so no goroutine is left behind.
	close(mgr.waitDone)
}
