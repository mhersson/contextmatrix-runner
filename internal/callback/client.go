// Package callback sends HMAC-signed status callbacks to ContextMatrix.
package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-runner/internal/metrics"
)

const (
	maxRetries     = 3
	requestTimeout = 10 * time.Second
	// bearerFallbackLogInterval is the minimum gap between Error-level logs
	// emitted from the deprecated Bearer fallback path in VerifyAutonomous.
	// The first call always logs; subsequent calls within the window are
	// suppressed so a hot-loop verifier does not drown the log aggregator,
	// but the cmr_callback_bearer_fallback_total counter still ticks on
	// every call so dashboards see the full rate.
	bearerFallbackLogInterval = 5 * time.Minute
)

// Callback request bodies are defined in contextmatrix-protocol; aliased
// so call sites and tests keep compiling unchanged.
type (
	statusRequest       = protocol.StatusCallbackPayload
	skillEngagedRequest = protocol.SkillEngagedPayload
)

// Client sends signed status callbacks to ContextMatrix.
//
// NOTE on apiKey usage: apiKey is the shared HMAC-SHA256 secret used for
// both inbound webhook verification and outbound callback signing. It MUST
// NEVER be sent to ContextMatrix as a raw `Authorization: Bearer` token —
// doing so would leak the HMAC secret and let anyone who saw a single
// Authorization header forge signed callbacks in either direction.
// The one transitional exception is VerifyAutonomous when
// useHMACForVerifyAutonomous is false: the runner falls back to Bearer
// until the ContextMatrix server accepts HMAC on that GET endpoint
// The fallback is deprecated and logs a WARN at startup.
type Client struct {
	httpClient                 *http.Client
	contextMatrixURL           string
	apiKey                     string
	logger                     *slog.Logger
	useHMACForVerifyAutonomous bool
	metrics                    *metrics.Metrics
	// bearerFallbackLastLogUnix is the unix-seconds timestamp of the most
	// recent Error-level log emitted from the Bearer fallback path. Updated
	// via atomic CAS so concurrent VerifyAutonomous calls rate-limit to one
	// Error log per bearerFallbackLogInterval window without blocking on a
	// mutex. The companion cmr_callback_bearer_fallback_total counter is
	// incremented on every call, so dashboards still see the full rate
	// even when logs are throttled.
	bearerFallbackLastLogUnix atomic.Int64
}

// NewClient creates a new callback client. By default VerifyAutonomous is
// HMAC-signed; use SetUseHMACForVerifyAutonomous(false) during the
// cross-repo transition if the ContextMatrix server still expects Bearer.
// The HTTP transport is wrapped with otelhttp so every outgoing request
// becomes a child span of whatever caller context the request is made in.
//
// A nil logger is replaced with slog.Default() so retry / fallback paths
// can always log without a nil-deref panic.
func NewClient(cmURL, apiKey string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}

	return &Client{
		httpClient: &http.Client{
			Timeout:   requestTimeout,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		contextMatrixURL:           cmURL,
		apiKey:                     apiKey,
		logger:                     logger,
		useHMACForVerifyAutonomous: true,
	}
}

// SetUseHMACForVerifyAutonomous toggles whether VerifyAutonomous signs its
// GET request with HMAC (true, the default and secure mode) or falls back
// to sending `Authorization: Bearer <apiKey>` (false, the deprecated
// cross-repo transition mode).
func (c *Client) SetUseHMACForVerifyAutonomous(useHMAC bool) {
	c.useHMACForVerifyAutonomous = useHMAC
}

// WithMetrics attaches a metrics bundle so retry attempts are counted.
// Passing nil disables metric observation.
func (c *Client) WithMetrics(m *metrics.Metrics) *Client {
	c.metrics = m

	return c
}

// ReportStatus sends a runner status update to ContextMatrix.
// Valid statuses: "running", "failed", "completed".
func (c *Client) ReportStatus(ctx context.Context, cardID, project, status, message string) error {
	body, err := json.Marshal(statusRequest{
		CardID:       cardID,
		Project:      project,
		RunnerStatus: status,
		Message:      message,
	})
	if err != nil {
		return fmt.Errorf("marshal callback: %w", err)
	}

	var lastErr error

	statusURI, err := callbackStatusURI(c.contextMatrixURL)
	if err != nil {
		return err
	}

	for attempt := range maxRetries {
		// Each retry uses a fresh ts (and therefore a fresh HMAC
		// signature), so the receiver's replay cache — which keys on the
		// (method, uri, timestamp, signature) tuple — will treat each
		// attempt as a distinct request and will not self-409 the retry.
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		signature := protocol.SignPayloadWithTimestamp(c.apiKey, http.MethodPost, statusURI, body, ts)

		lastErr = c.doRequest(ctx, body, signature, ts)
		if lastErr == nil {
			return nil
		}

		if isClientError(lastErr) {
			return lastErr
		}

		// Log the short, body-free error at Warn level (safe for shared log
		// aggregators) and the full upstream body at Debug level for operators
		// who opt into verbose logging.
		c.logger.Warn("callback failed, retrying",
			"attempt", attempt+1,
			"card_id", cardID,
			"error", lastErr.Error(),
		)

		if ce, ok := errors.AsType[*Error](lastErr); ok {
			c.logger.Debug("callback failed, upstream body",
				"attempt", attempt+1,
				"card_id", cardID,
				"detail", ce.DetailForLog(),
			)
		}

		if c.metrics != nil {
			c.metrics.CallbackRetriesTotal.WithLabelValues(endpointLabel(status)).Inc()
		}

		// Skip the backoff on the final attempt — the loop is about to exit
		// regardless of how long we wait. Without this, a maxRetries=3 run
		// burns ~4s on a stopped timer before returning the failure.
		if attempt == maxRetries-1 {
			break
		}

		// Explicit Timer + Stop on ctx-cancel so ctx cancellation does not
		// leak the timer (time.After drops its reference only when it
		// fires). backoff is per-attempt, so declaring the timer inside
		// the loop body is correct — each attempt gets a fresh timer.
		// Under Go 1.23+ the runtime GC's a stopped timer even if its
		// channel was not drained after Stop returns false, so no manual
		// drain is needed.
		backoff := time.Duration(1<<uint(attempt)) * time.Second
		timer := time.NewTimer(backoff)

		select {
		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()
		case <-timer.C:
		}
	}

	return fmt.Errorf("callback failed after %d attempts: %w", maxRetries, lastErr)
}

// ReportSkillEngaged sends a skill-engagement notification to ContextMatrix.
// The notification is HMAC-signed in the same scheme as ReportStatus and is
// retried up to maxRetries times on server errors.
func (c *Client) ReportSkillEngaged(ctx context.Context, cardID, project, skillName string) error {
	body, err := json.Marshal(skillEngagedRequest{
		CardID:    cardID,
		Project:   project,
		SkillName: skillName,
	})
	if err != nil {
		return fmt.Errorf("marshal skill-engaged callback: %w", err)
	}

	skillURI, err := callbackSkillEngagedURI(c.contextMatrixURL)
	if err != nil {
		return err
	}

	var lastErr error

	for attempt := range maxRetries {
		// Each retry uses a fresh ts (and a fresh HMAC signature), so the
		// receiver's replay cache treats each attempt as a distinct
		// request and does not self-409 the retry.
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		signature := protocol.SignPayloadWithTimestamp(c.apiKey, http.MethodPost, skillURI, body, ts)

		reqURL := c.contextMatrixURL + "/api/runner/skill-engaged"

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create skill-engaged request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(protocol.SignatureHeader, "sha256="+signature)
		req.Header.Set(protocol.TimestampHeader, ts)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("send skill-engaged request: %w", err)
		} else {
			func() {
				defer func() { _ = resp.Body.Close() }()

				respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				if readErr != nil {
					lastErr = fmt.Errorf("read skill-engaged response: %w", readErr)

					return
				}

				if resp.StatusCode >= 400 {
					lastErr = newError(reqURL, resp.StatusCode, respBody)
				} else {
					lastErr = nil
				}
			}()
		}

		if lastErr == nil {
			return nil
		}

		if isClientError(lastErr) {
			return lastErr
		}

		c.logger.Warn("skill-engaged callback failed, retrying",
			"attempt", attempt+1,
			"card_id", cardID,
			"error", lastErr.Error(),
		)

		if c.metrics != nil {
			c.metrics.CallbackRetriesTotal.WithLabelValues(endpointSkillEngaged).Inc()
		}

		// Skip the backoff on the final attempt — the loop is about to exit
		// regardless of how long we wait. Without this, a maxRetries=3 run
		// burns ~4s on a stopped timer before returning the failure.
		if attempt == maxRetries-1 {
			break
		}

		// Explicit Timer + Stop on ctx-cancel so ctx cancellation does
		// not leak the timer. backoff is per-attempt, so declaring the
		// timer inside the loop body is correct — each attempt gets a
		// fresh timer. Under Go 1.23+ the runtime GC's a stopped timer
		// even if its channel was not drained after Stop returns false,
		// so no manual drain is needed.
		backoff := time.Duration(1<<uint(attempt)) * time.Second
		timer := time.NewTimer(backoff)

		select {
		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()
		case <-timer.C:
		}
	}

	return fmt.Errorf("skill-engaged callback failed after %d attempts: %w", maxRetries, lastErr)
}

// Ping checks that ContextMatrix is reachable at the configured URL via a
// TCP dial to host:port. The runner does not assume CM exposes a dedicated
// readiness endpoint (and several deployments rewrite /api/* paths at an
// ingress), so a plain transport-level probe is the least-assumption smoke
// test. A nil error means the TCP handshake completed; any application-layer
// misconfiguration (wrong API key, broken routing, etc.) will still surface
// on the first real webhook callback — preflight only covers the "is CM
// reachable at all" failure mode.
func (c *Client) Ping(ctx context.Context) error {
	u, err := url.Parse(c.contextMatrixURL)
	if err != nil {
		return fmt.Errorf("parse contextmatrix_url: %w", err)
	}

	host := u.Host
	if host == "" {
		return fmt.Errorf("contextmatrix_url has empty host: %q", c.contextMatrixURL)
	}

	// net.JoinHostPort demands a port, so fill in the scheme default if
	// the URL omitted it. Matches http/https behavior.
	if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		switch u.Scheme {
		case "https":
			host = net.JoinHostPort(host, "443")
		default:
			host = net.JoinHostPort(host, "80")
		}
	}

	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return fmt.Errorf("dial contextmatrix %s: %w", host, err)
	}

	_ = conn.Close()

	return nil
}

// cardResponse is the minimal subset of a CM card needed to verify the
// autonomous flag. Only the fields used by VerifyAutonomous are decoded.
type cardResponse struct {
	Autonomous bool `json:"autonomous"`
}

// VerifyAutonomous fetches the card from ContextMatrix via a read-only GET and
// reports whether its autonomous flag is set. It returns (false, err) on any
// non-2xx response so callers can remain fail-closed without issuing any
// state-changing request back to CM (which would trigger an infinite loop).
//
// The request is HMAC-signed by default. The signature covers the
// timestamp concatenated with an empty body, identical to every other
// runner<->CM webhook so the CM handler uses one verification path.
// During the cross-repo transition, SetUseHMACForVerifyAutonomous(false)
// switches back to `Authorization: Bearer <apiKey>` so the runner keeps
// working against an older CM server that does not yet accept HMAC on
// this endpoint.
//
// project and cardID are url.PathEscape'd unconditionally so values
// like "my project" or "CARD/42" produce a well-formed URL in either mode.
//
// Transient errors (5xx, connection reset, etc.) are retried up to
// maxRetries times with exponential backoff, mirroring ReportStatus /
// ReportSkillEngaged. A 4xx response is non-retryable
// (isClientError short-circuits) and returns immediately. Without the
// retry, a single transient error fails closed as a 502 to CM even when
// a retry would have succeeded.
func (c *Client) VerifyAutonomous(ctx context.Context, project, cardID string) (bool, error) {
	reqURL := fmt.Sprintf("%s/api/v1/cards/%s/%s/autonomous",
		c.contextMatrixURL,
		url.PathEscape(project),
		url.PathEscape(cardID),
	)

	var lastErr error

	for attempt := range maxRetries {
		autonomous, err := c.doVerifyAutonomous(ctx, reqURL)
		if err == nil {
			return autonomous, nil
		}

		lastErr = err

		if isClientError(err) {
			return false, err
		}

		c.logger.Warn("verify-autonomous callback failed, retrying",
			"attempt", attempt+1,
			"project", project,
			"card_id", cardID,
			"error", err.Error(),
		)

		if ce, ok := errors.AsType[*Error](err); ok {
			c.logger.Debug("verify-autonomous callback failed, upstream body",
				"attempt", attempt+1,
				"project", project,
				"card_id", cardID,
				"detail", ce.DetailForLog(),
			)
		}

		if c.metrics != nil {
			c.metrics.CallbackRetriesTotal.WithLabelValues(endpointVerifyAutonomous).Inc()
		}

		// Skip the backoff on the final attempt — the loop is about to exit
		// regardless of how long we wait. Mirrors the same skip in the
		// POST callbacks.
		if attempt == maxRetries-1 {
			break
		}

		// Explicit Timer + Stop on ctx-cancel so ctx cancellation does
		// not leak the timer. backoff is per-attempt; under Go 1.23+ the
		// runtime GC's a stopped timer even if its channel was not drained
		// after Stop returns false, so no manual drain is needed.
		backoff := time.Duration(1<<uint(attempt)) * time.Second
		timer := time.NewTimer(backoff)

		select {
		case <-ctx.Done():
			timer.Stop()

			return false, ctx.Err()
		case <-timer.C:
		}
	}

	return false, fmt.Errorf("verify-autonomous callback failed after %d attempts: %w", maxRetries, lastErr)
}

// doVerifyAutonomous performs a single VerifyAutonomous HTTP request and
// decodes the response. Extracted from the retry loop so VerifyAutonomous
// can re-issue the request on a transient error without duplicating the
// signing + parsing logic.
func (c *Client) doVerifyAutonomous(ctx context.Context, reqURL string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, fmt.Errorf("create verify-autonomous request: %w", err)
	}

	if c.useHMACForVerifyAutonomous {
		// HMAC bound to method+URI with an empty body. Binding the URI
		// (path + query) prevents a captured signature from being replayed
		// against a different endpoint, and binding the timestamp prevents
		// replay outside the clock-skew window. Each retry uses a fresh
		// timestamp so the receiver's replay cache treats every attempt
		// as a distinct request.
		ts := strconv.FormatInt(time.Now().Unix(), 10)

		uri, perr := verifyAutonomousURI(reqURL)
		if perr != nil {
			return false, perr
		}

		signature := protocol.SignPayloadWithTimestamp(c.apiKey, http.MethodGet, uri, nil, ts)
		req.Header.Set(protocol.SignatureHeader, "sha256="+signature)
		req.Header.Set(protocol.TimestampHeader, ts)
	} else {
		// Deprecated Bearer fallback — retained only so the runner stays
		// compatible with a CM server that has not yet rolled the HMAC
		// change. Leaks the HMAC secret to anyone who can read the
		// Authorization header; remove once the server accepts HMAC.
		//
		// Audit trail: increment the cmr_callback_bearer_fallback_total
		// counter on every call so dashboards alert as soon as this path
		// is taken in prod, and emit a rate-limited Error log so the same
		// signal shows up in centralized log aggregators without spamming
		// them on every VerifyAutonomous request.
		req.Header.Set("Authorization", "Bearer "+c.apiKey)

		if c.metrics != nil {
			c.metrics.CallbackBearerFallbackTotal.Inc()
		}

		c.logBearerFallback()
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("send verify-autonomous request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, fmt.Errorf("read verify-autonomous response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return false, newError(reqURL, resp.StatusCode, respBody)
	}

	var card cardResponse
	if err := json.Unmarshal(respBody, &card); err != nil {
		return false, fmt.Errorf("parse verify-autonomous response: %w", err)
	}

	return card.Autonomous, nil
}

func (c *Client) doRequest(ctx context.Context, body []byte, signature, ts string) error {
	reqURL := c.contextMatrixURL + "/api/runner/status"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.SignatureHeader, "sha256="+signature)
	req.Header.Set(protocol.TimestampHeader, ts)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return newError(reqURL, resp.StatusCode, respBody)
	}

	return nil
}

// callbackStatusURI returns the request-target (path + raw query) of the CM
// status-callback URL. Sender and receiver must agree on the signed value —
// any intermediate proxy that rewrites paths or queries would break HMAC
// auth, so this is derived from the configured contextMatrixURL to keep
// both sides consistent even if the base URL includes a trailing slash or
// a path prefix.
func callbackStatusURI(contextMatrixURL string) (string, error) {
	return deriveURI(contextMatrixURL + "/api/runner/status")
}

// callbackSkillEngagedURI returns the request-target of the CM
// skill-engaged callback URL.
func callbackSkillEngagedURI(contextMatrixURL string) (string, error) {
	return deriveURI(contextMatrixURL + "/api/runner/skill-engaged")
}

// verifyAutonomousURI returns the request-target of the constructed
// /autonomous verify URL.
func verifyAutonomousURI(reqURL string) (string, error) {
	return deriveURI(reqURL)
}

func deriveURI(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", rawURL, err)
	}

	if u.Path == "" {
		u.Path = "/"
	}

	return u.RequestURI(), nil
}

// maxDetailBytes caps the upstream body retained on *Error for
// server-side-only debug logging, so a rogue upstream cannot pin large
// buffers.
const maxDetailBytes = 2048

// Error is returned for non-2xx upstream responses. Its Error() method
// intentionally omits the upstream body (which may contain secrets leaked by a
// misconfigured CM) and returns only a URL + status short form safe for
// propagation to clients. The truncated body is retained on a private field
// and exposed via DetailForLog() for server-side Debug logging only.
type Error struct {
	urlShort   string // scheme://host/path (no query/fragment)
	statusCode int
	detail     string // truncated upstream body for server-side logs only
}

// newError constructs an *Error, stripping query and fragment from the URL
// and truncating the upstream body to maxDetailBytes.
func newError(fullURL string, statusCode int, body []byte) *Error {
	short := sanitizeURLForError(fullURL)

	detail := string(body)
	if len(detail) > maxDetailBytes {
		detail = detail[:maxDetailBytes]
	}

	return &Error{
		urlShort:   short,
		statusCode: statusCode,
		detail:     detail,
	}
}

// sanitizeURLForError returns scheme://host/path for fullURL, dropping query
// string, fragment, and any embedded userinfo (which can embed credentials or
// tokens). A misconfigured base URL of the form https://user:token@host/path
// would otherwise leak the credentials into error messages propagated to
// clients and log aggregators. If the URL cannot be parsed it is replaced
// with "<invalid-url>" so nothing leaks.
func sanitizeURLForError(fullURL string) string {
	u, err := url.Parse(fullURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<invalid-url>"
	}

	// Strip userinfo before reading u.Host: net/url's Host field does not
	// include userinfo (that lives on u.User), so this guards against a
	// future change that would include it. Clearing the field is the
	// belt-and-braces step on top of building the URL by hand below.
	u.User = nil

	return u.Scheme + "://" + u.Host + u.Path
}

// Error returns a body-free short form safe for propagation to clients and
// third-party log aggregators.
func (e *Error) Error() string {
	return fmt.Sprintf("callback to %s returned status %d", e.urlShort, e.statusCode)
}

// DetailForLog returns the truncated upstream body for server-side-only debug
// logging. Never propagate this value to HTTP responses or to external logs.
func (e *Error) DetailForLog() string {
	return e.detail
}

// StatusCode exposes the upstream HTTP status for callers that need to
// distinguish 4xx from 5xx without string-matching.
func (e *Error) StatusCode() int {
	return e.statusCode
}

func isClientError(err error) bool {
	// Context-cancellation and deadline-exceeded errors are terminal: there
	// is no point burning another attempt's backoff before the next
	// ctx.Done() select wakes up. Treat them as "client" errors so the
	// retry loop returns immediately. Without this short-circuit a cancelled
	// or expired request would log a Warn retry line and sleep one backoff
	// before the next iteration's ctx-select catches the cancellation.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	if ce, ok := errors.AsType[*Error](err); ok {
		return ce.statusCode >= 400 && ce.statusCode < 500
	}

	return false
}

// endpointLabel maps a runner_status to a bounded label for the
// cmr_callback_retries_total counter. We intentionally collapse unknown
// statuses into "status" rather than passing the raw string through — an
// attacker cannot influence status here, but keeping the label set closed
// guards against future callers adding arbitrary values.
func endpointLabel(status string) string {
	switch status {
	case "running", "failed", "completed":
		return status
	default:
		return "status"
	}
}

// Endpoint labels for cmr_callback_retries_total on the skill-engaged and
// verify-autonomous callbacks. Kept as constants so the label set stays
// closed and matches what dashboards key on.
const (
	endpointSkillEngaged = "skill_engaged"
	// endpointVerifyAutonomous labels retries of the read-only
	// VerifyAutonomous GET so transient CM failures (a brief 5xx or a
	// connection reset) show up in the same cmr_callback_retries_total
	// series as the POST callbacks rather than disappearing into a
	// fail-closed 502 response on the runner.
	endpointVerifyAutonomous = "verify_autonomous"
)

// logBearerFallback emits an Error-level log noting that the deprecated
// Bearer fallback for VerifyAutonomous was taken. The log is rate-limited
// to one entry per bearerFallbackLogInterval (5m) across all goroutines on
// this Client; the rate-limit is implemented as an atomic CAS on the
// last-log timestamp so a hot path of VerifyAutonomous calls does not
// drown the log aggregator. The cmr_callback_bearer_fallback_total
// counter is always incremented (in the caller), so dashboards still see
// the full call rate.
func (c *Client) logBearerFallback() {
	now := time.Now().Unix()
	last := c.bearerFallbackLastLogUnix.Load()

	if now-last < int64(bearerFallbackLogInterval/time.Second) {
		return
	}

	// CAS: only the winner of the race logs. Losers fall through silently
	// and rely on the counter for the audit trail.
	if !c.bearerFallbackLastLogUnix.CompareAndSwap(last, now) {
		return
	}

	c.logger.Error(
		"VerifyAutonomous took the deprecated Bearer fallback — the shared HMAC secret is being shipped in the Authorization header; set use_hmac_for_verify_autonomous=true once the ContextMatrix server accepts HMAC on this endpoint",
		"rate_limited_per", bearerFallbackLogInterval.String(),
	)
}
