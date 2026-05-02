package events

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSSEClientReceivesEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		fmt.Fprintf(w, "id: 1\nevent: chat_input\ndata: {\"type\":\"chat_input\",\"data\":\"hello\"}\n\n")
		f.Flush()
		// Hold the connection so the client has time to parse before EOF.
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	client := NewSSEClient(srv.URL, "card1", "bearer-token")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, _ := client.Subscribe(ctx)
	select {
	case ev := <-ch:
		require.Equal(t, "chat_input", ev.Type)
		require.Contains(t, ev.Data, "hello")
		require.Equal(t, uint64(1), ev.EventID)
	case <-ctx.Done():
		t.Fatal("timeout")
	}
}

func TestSSEClientSendsLastEventID(t *testing.T) {
	var receivedHeader atomic.Value
	receivedHeader.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeader.Store(r.Header.Get("Last-Event-ID"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Close immediately — header capture is what we want to verify.
	}))
	defer srv.Close()

	client := NewSSEClient(srv.URL, "card1", "bearer-token")
	client.LastEventID = 42

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, _ = client.Subscribe(ctx)

	require.Eventually(t, func() bool {
		return receivedHeader.Load().(string) == "42"
	}, 500*time.Millisecond, 10*time.Millisecond)
}

func TestSSEClientSendsBearerAuth(t *testing.T) {
	var authHeader atomic.Value
	authHeader.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewSSEClient(srv.URL, "c1", "tok123")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, _ = client.Subscribe(ctx)

	require.Eventually(t, func() bool {
		return authHeader.Load().(string) == "Bearer tok123"
	}, 500*time.Millisecond, 10*time.Millisecond)
}

func TestSSEClientReconnects(t *testing.T) {
	var requests int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&requests, 1)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()

		if n == 1 {
			// First connection: close immediately to trigger reconnect.
			return
		}
		// Second connection: hold open.
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	client := NewSSEClient(srv.URL, "c1", "")

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, _ = client.Subscribe(ctx)

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&requests) >= 2
	}, time.Second, 20*time.Millisecond, "client should reconnect")
}

func TestSSEClientStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		// Hold open forever.
		select {}
	}))
	defer srv.Close()

	client := NewSSEClient(srv.URL, "c1", "")
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := client.Subscribe(ctx)

	cancel()

	// Wait for events channel to close.
	select {
	case _, ok := <-ch:
		require.False(t, ok, "events channel should close on cancel")
	case <-time.After(time.Second):
		t.Fatal("subscribe goroutine didn't exit on cancel")
	}
}

// TestSSEClientReconstructsMultiLineData locks in the runner-side half of
// the multi-line data contract: when CM emits one `data:` line per \n
// fragment, the parser must join them back with \n so the orchestrator's
// chat-input handlers see the original message intact.
func TestSSEClientReconstructsMultiLineData(t *testing.T) {
	const want = "line one\nline two\n\nline four after blank"

	body := strings.NewReader(
		"id: 1\n" +
			"event: chat_input\n" +
			"data: line one\n" +
			"data: line two\n" +
			"data: \n" +
			"data: line four after blank\n" +
			"\n",
	)

	c := &SSEClient{}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	events := make(chan RunnerEvent, 1)

	errCh := make(chan error, 1)

	go func() { errCh <- c.parseStream(ctx, body, events) }()

	select {
	case ev := <-events:
		if ev.Type != "chat_input" {
			t.Fatalf("Type: got %q, want chat_input", ev.Type)
		}

		if ev.Data != want {
			t.Fatalf("Data round-trip:\n got: %q\nwant: %q", ev.Data, want)
		}
	case err := <-errCh:
		t.Fatalf("parseStream returned before event: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for event")
	}
}
