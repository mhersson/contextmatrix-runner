package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/container"
	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// TestHMACAuth_AllFailuresReturn401Generic verifies M8 in REVIEW.md: every
// authentication failure collapses to a byte-identical 401 body so a scanner
// cannot fingerprint the specific reason (missing header vs bad signature vs
// expired timestamp vs unreadable body).
func TestHMACAuth_AllFailuresReturn401Generic(t *testing.T) {
	h := &Handler{apiKey: testAPIKey}
	downstream := func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream handler must not run on failed auth")
	}
	handler := h.hmacAuth(downstream)

	// errReader returns an error on the first Read so we can exercise the
	// "body read failed" branch inside hmacAuth.
	errReader := io.NopCloser(&failingReader{err: io.ErrUnexpectedEOF})

	cases := []struct {
		name  string
		build func() *http.Request
	}{
		{
			name: "missing signature header",
			build: func() *http.Request {
				return httptest.NewRequestWithContext(context.Background(),
					http.MethodPost, "/trigger", strings.NewReader("{}"))
			},
		},
		{
			name: "missing timestamp header",
			build: func() *http.Request {
				r := httptest.NewRequestWithContext(context.Background(),
					http.MethodPost, "/trigger", strings.NewReader("{}"))
				r.Header.Set(cmhmac.SignatureHeader, "sha256=abc")

				return r
			},
		},
		{
			name: "bad signature",
			build: func() *http.Request {
				r := httptest.NewRequestWithContext(context.Background(),
					http.MethodPost, "/trigger", strings.NewReader("{}"))
				r.Header.Set(cmhmac.SignatureHeader, "sha256=deadbeef")
				r.Header.Set(cmhmac.TimestampHeader,
					strconv.FormatInt(time.Now().Unix(), 10))

				return r
			},
		},
		{
			name: "expired timestamp",
			build: func() *http.Request {
				// Sign correctly but with a timestamp outside the skew window.
				body := []byte(`{}`)
				oldTS := strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10)
				sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, oldTS)
				r := httptest.NewRequestWithContext(context.Background(),
					http.MethodPost, "/trigger", bytes.NewReader(body))
				r.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
				r.Header.Set(cmhmac.TimestampHeader, oldTS)

				return r
			},
		},
		{
			name: "unreadable body",
			build: func() *http.Request {
				r := httptest.NewRequestWithContext(context.Background(),
					http.MethodPost, "/trigger", errReader)
				r.Header.Set(cmhmac.SignatureHeader, "sha256=abc")
				r.Header.Set(cmhmac.TimestampHeader,
					strconv.FormatInt(time.Now().Unix(), 10))

				return r
			},
		},
	}

	var canonical []byte

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler(w, tc.build())

			require.Equal(t, http.StatusUnauthorized, w.Code,
				"every auth failure must return 401, got %d for %q", w.Code, tc.name)

			body := w.Body.Bytes()

			// Every failure must parse to the same ErrorResponse shape.
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(body, &resp))
			assert.False(t, resp.OK)
			assert.Equal(t, CodeUnauthorized, resp.Code,
				"every auth failure must use %q, got %q", CodeUnauthorized, resp.Code)
			assert.Equal(t, "unauthorized", resp.Message,
				"message must be the fixed 'unauthorized' literal, not a specific reason")

			// M8 fingerprint protection: the raw response bytes must be
			// byte-identical across every failure mode.
			if i == 0 {
				canonical = append(canonical[:0], body...)

				return
			}

			assert.Equal(t, canonical, body,
				"auth failure bodies must be byte-identical (fingerprint protection)")
		})
	}
}

// failingReader implements io.Reader and always returns the same error.
type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

// TestEndpointStatusCodeMatrix is the table-driven check required by
// CTXRUN-056: every (endpoint, input, expected status, expected code) row is
// covered in one place so a future handler tweak that accidentally flips a
// status is caught immediately.
func TestEndpointStatusCodeMatrix(t *testing.T) {
	type setup func(t *testing.T) (*Handler, []byte, string)

	// withTracked seeds a container entry for (proj, card) into a fresh
	// tracker-backed handler. Helper keeps the cases readable.
	withTracked := func(project, card string, withStdin, stdinClosed bool) setup {
		return func(t *testing.T) (*Handler, []byte, string) {
			t.Helper()

			tr := tracker.New()
			b := logbroadcast.NewBroadcaster(nil, nil)
			h := NewHandler(&noopRunner{}, tr, b, nil, testAPIKey, 3, testMCPURL,
				slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)
			require.NoError(t, tr.Add(&tracker.ContainerInfo{
				CardID:  card,
				Project: project,
			}))

			if withStdin {
				fw := &fakeWriteCloser{}
				tr.SetStdin(project, card, fw, nil)

				if stdinClosed {
					require.NoError(t, tr.CloseStdin(project, card))
				}
			}

			return h, nil, ""
		}
	}

	blank := func(t *testing.T) (*Handler, []byte, string) {
		t.Helper()

		tr := tracker.New()
		h := NewHandler(&noopRunner{}, tr, logbroadcast.NewBroadcaster(nil, nil), nil,
			testAPIKey, 3, testMCPURL,
			slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

		return h, nil, ""
	}

	cases := []struct {
		name       string
		setup      setup
		path       string
		handler    func(*Handler) http.HandlerFunc
		payload    any
		rawBody    []byte
		wantStatus int
		wantCode   string // "" means don't assert code (success cases)
	}{
		// /trigger
		{
			name:       "trigger: invalid JSON -> 400 invalid_json",
			setup:      blank,
			path:       "/trigger",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleTrigger },
			rawBody:    []byte("not-json"),
			wantStatus: http.StatusBadRequest,
			wantCode:   CodeInvalidJSON,
		},
		{
			name:       "trigger: missing card_id -> 400 invalid_field",
			setup:      blank,
			path:       "/trigger",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleTrigger },
			wantStatus: http.StatusBadRequest,
			wantCode:   CodeInvalidField,
		},
		{
			name: "trigger: already tracked -> 409 conflict (generic message)",
			setup: func(t *testing.T) (*Handler, []byte, string) {
				t.Helper()

				tr := tracker.New()
				h := NewHandler(&noopRunner{}, tr, logbroadcast.NewBroadcaster(nil, nil), nil,
					testAPIKey, 3, testMCPURL,
					slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)
				require.NoError(t, tr.Add(&tracker.ContainerInfo{
					CardID: "DUPE-1", Project: "proj",
				}))

				return h, nil, ""
			},
			path:       "/trigger",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleTrigger },
			wantStatus: http.StatusConflict,
			wantCode:   CodeConflict,
			payload:    TriggerPayload{CardID: "DUPE-1", Project: "proj", RepoURL: "https://github.com/o/r.git"},
		},
		{
			name: "trigger: over concurrency limit -> 429 limit_reached",
			setup: func(t *testing.T) (*Handler, []byte, string) {
				t.Helper()

				tr := tracker.New()
				h := NewHandler(&noopRunner{}, tr, logbroadcast.NewBroadcaster(nil, nil), nil,
					testAPIKey, 1, testMCPURL,
					slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)
				require.NoError(t, tr.Add(&tracker.ContainerInfo{
					CardID: "BUSY-1", Project: "proj",
				}))

				return h, nil, ""
			},
			path:       "/trigger",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleTrigger },
			wantStatus: http.StatusTooManyRequests,
			wantCode:   CodeLimitReached,
			payload:    TriggerPayload{CardID: "NEW-1", Project: "proj", RepoURL: "https://github.com/o/r.git"},
		},
		{
			name: "trigger: draining -> 503 draining",
			setup: func(t *testing.T) (*Handler, []byte, string) {
				t.Helper()

				tr := tracker.New()
				hs := NewHealthState()
				hs.Draining.Store(true)
				h := NewHandler(&noopRunner{}, tr, logbroadcast.NewBroadcaster(nil, nil), nil,
					testAPIKey, 3, testMCPURL,
					slog.New(slog.NewTextHandler(io.Discard, nil)), 0, hs)

				return h, nil, ""
			},
			path:       "/trigger",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleTrigger },
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   CodeDraining,
		},

		// /kill
		{
			name:       "kill: not tracked -> 200 idempotent",
			setup:      blank,
			path:       "/kill",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleKill },
			payload:    KillPayload{CardID: "GHOST-1", Project: "proj"},
			wantStatus: http.StatusOK,
			wantCode:   "", // success
		},
		{
			name:       "kill: invalid JSON -> 400 invalid_json",
			setup:      blank,
			path:       "/kill",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleKill },
			rawBody:    []byte("garbage"),
			wantStatus: http.StatusBadRequest,
			wantCode:   CodeInvalidJSON,
		},

		// /message
		{
			name:       "message: not tracked -> 404 not_found",
			setup:      blank,
			path:       "/message",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleMessage },
			payload:    MessagePayload{CardID: "NONE", Project: "proj", Content: "hi"},
			wantStatus: http.StatusNotFound,
			wantCode:   CodeNotFound,
		},
		{
			name:       "message: no stdin (non-interactive) -> 409 conflict",
			setup:      withTracked("proj", "CARD-1", false, false),
			path:       "/message",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleMessage },
			payload:    MessagePayload{CardID: "CARD-1", Project: "proj", Content: "hi"},
			wantStatus: http.StatusConflict,
			wantCode:   CodeConflict,
		},
		{
			name:       "message: stdin closed (session ended) -> 410 stdin_closed",
			setup:      withTracked("proj", "CARD-1", true, true),
			path:       "/message",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleMessage },
			payload:    MessagePayload{CardID: "CARD-1", Project: "proj", Content: "hi"},
			wantStatus: http.StatusGone,
			wantCode:   CodeStdinClosed,
		},
		{
			name:       "message: too large -> 413 too_large",
			setup:      withTracked("proj", "CARD-1", true, false),
			path:       "/message",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleMessage },
			payload:    MessagePayload{CardID: "CARD-1", Project: "proj", Content: strings.Repeat("x", maxMessageContent+1)},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   CodeTooLarge,
		},
		{
			name:       "message: invalid JSON -> 400 invalid_json",
			setup:      withTracked("proj", "CARD-1", true, false),
			path:       "/message",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleMessage },
			rawBody:    []byte("not-json"),
			wantStatus: http.StatusBadRequest,
			wantCode:   CodeInvalidJSON,
		},
		{
			name:       "message: missing content -> 400 invalid_field",
			setup:      withTracked("proj", "CARD-1", true, false),
			path:       "/message",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleMessage },
			payload:    MessagePayload{CardID: "CARD-1", Project: "proj"},
			wantStatus: http.StatusBadRequest,
			wantCode:   CodeInvalidField,
		},
		{
			name: "message: draining -> 503 draining",
			setup: func(t *testing.T) (*Handler, []byte, string) {
				t.Helper()

				tr := tracker.New()
				hs := NewHealthState()
				hs.Draining.Store(true)
				h := NewHandler(&noopRunner{}, tr, logbroadcast.NewBroadcaster(nil, nil), nil,
					testAPIKey, 3, testMCPURL,
					slog.New(slog.NewTextHandler(io.Discard, nil)), 0, hs)

				return h, nil, ""
			},
			path:       "/message",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleMessage },
			payload:    MessagePayload{CardID: "C-1", Project: "proj", Content: "hi"},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   CodeDraining,
		},

		// /promote
		{
			name:       "promote: not tracked -> 404 not_found",
			setup:      blank,
			path:       "/promote",
			handler:    func(h *Handler) http.HandlerFunc { return h.handlePromote },
			payload:    PromotePayload{CardID: "GHOST-1", Project: "proj"},
			wantStatus: http.StatusNotFound,
			wantCode:   CodeNotFound,
		},
		{
			name:       "promote: no stdin -> 409 conflict",
			setup:      withTracked("proj", "CARD-1", false, false),
			path:       "/promote",
			handler:    func(h *Handler) http.HandlerFunc { return h.handlePromote },
			payload:    PromotePayload{CardID: "CARD-1", Project: "proj"},
			wantStatus: http.StatusConflict,
			wantCode:   CodeConflict,
		},
		{
			name:       "promote: stdin closed -> 410 stdin_closed",
			setup:      withTracked("proj", "CARD-1", true, true),
			path:       "/promote",
			handler:    func(h *Handler) http.HandlerFunc { return h.handlePromote },
			payload:    PromotePayload{CardID: "CARD-1", Project: "proj"},
			wantStatus: http.StatusGone,
			wantCode:   CodeStdinClosed,
		},

		// /end-session
		{
			name:       "end-session: not tracked -> 404 not_found",
			setup:      blank,
			path:       "/end-session",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleEndSession },
			payload:    EndSessionPayload{CardID: "GHOST-1", Project: "proj"},
			wantStatus: http.StatusNotFound,
			wantCode:   CodeNotFound,
		},
		{
			name:       "end-session: no stdin -> 409 conflict",
			setup:      withTracked("proj", "CARD-1", false, false),
			path:       "/end-session",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleEndSession },
			payload:    EndSessionPayload{CardID: "CARD-1", Project: "proj"},
			wantStatus: http.StatusConflict,
			wantCode:   CodeConflict,
		},

		// /stop-all
		{
			name:       "stop-all: invalid JSON -> 400 invalid_json",
			setup:      blank,
			path:       "/stop-all",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleStopAll },
			rawBody:    []byte("garbage"),
			wantStatus: http.StatusBadRequest,
			wantCode:   CodeInvalidJSON,
		},
		{
			name:       "stop-all: empty tracker -> 200 success",
			setup:      blank,
			path:       "/stop-all",
			handler:    func(h *Handler) http.HandlerFunc { return h.handleStopAll },
			payload:    StopAllPayload{},
			wantStatus: http.StatusOK,
			wantCode:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := tc.setup(t)

			var body []byte

			switch {
			case tc.rawBody != nil:
				body = tc.rawBody
			case tc.payload != nil:
				var err error

				body, err = json.Marshal(tc.payload)
				require.NoError(t, err)
			default:
				body = []byte(`{}`)
			}

			ts := strconv.FormatInt(time.Now().Unix(), 10)
			sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, ts)
			req := httptest.NewRequestWithContext(context.Background(),
				http.MethodPost, tc.path, bytes.NewReader(body))
			req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
			req.Header.Set(cmhmac.TimestampHeader, ts)

			w := httptest.NewRecorder()
			h.hmacAuth(tc.handler(h))(w, req)

			require.Equal(t, tc.wantStatus, w.Code,
				"%s: wrong status; body=%s", tc.name, w.Body.String())

			if tc.wantCode != "" {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp),
					"error body must parse as ErrorResponse: %s", w.Body.String())
				assert.False(t, resp.OK, "error response must have ok=false")
				assert.Equal(t, tc.wantCode, resp.Code,
					"%s: wrong code; message=%q", tc.name, resp.Message)
			}
		})
	}
}

// TestNoRawErrLeakIntoResponseBody sanity-checks the error-message hygiene
// required by M4 / M41 in REVIEW.md. It feeds each endpoint a JSON body that,
// if blindly echoed, would surface a recognisable marker into the response.
func TestNoRawErrLeakIntoResponseBody(t *testing.T) {
	tr := tracker.New()
	h := NewHandler(&noopRunner{}, tr, logbroadcast.NewBroadcaster(nil, nil), nil,
		testAPIKey, 3, testMCPURL,
		slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

	// An unmarshal on this produces an error with byte offset; the old code
	// echoed err.Error() which surfaced internal implementation details
	// (M41).
	const marker = "___PEEK_ME___"

	body := []byte("{" + marker + "}")

	endpoints := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/trigger", h.handleTrigger},
		{"/kill", h.handleKill},
		{"/stop-all", h.handleStopAll},
		{"/message", h.handleMessage},
		{"/promote", h.handlePromote},
		{"/end-session", h.handleEndSession},
	}

	for _, ep := range endpoints {
		t.Run(ep.path, func(t *testing.T) {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, ts)
			req := httptest.NewRequestWithContext(context.Background(),
				http.MethodPost, ep.path, bytes.NewReader(body))
			req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
			req.Header.Set(cmhmac.TimestampHeader, ts)

			w := httptest.NewRecorder()
			h.hmacAuth(ep.handler)(w, req)

			raw := w.Body.String()
			assert.NotContains(t, raw, marker,
				"%s must not echo raw request bytes into the response body", ep.path)
			// Byte-offset format from encoding/json.
			assert.NotContains(t, raw, "invalid character",
				"%s must not echo the encoding/json error text", ep.path)
		})
	}
}

// noopRunner is a ContainerRunner that does nothing. Handler tests that do
// not care about what happens after AddIfUnderLimit use this to avoid a real
// container.Manager.
type noopRunner struct{}

func (n *noopRunner) Run(_ context.Context, _ container.RunConfig) {}
func (n *noopRunner) Kill(_, _ string) error                       { return nil }
func (n *noopRunner) ListManaged(_ context.Context) ([]container.ManagedContainer, error) {
	return nil, nil
}

func (n *noopRunner) ForceRemoveByLabels(_ context.Context, _, _ string) (int, error) {
	return 0, nil
}
