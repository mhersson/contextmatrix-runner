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
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
	"github.com/mhersson/contextmatrix-runner/internal/metrics"
)

const (
	maxRetries     = 3
	requestTimeout = 10 * time.Second
)

// statusRequest is the JSON body sent to ContextMatrix.
type statusRequest struct {
	CardID       string `json:"card_id"`
	Project      string `json:"project"`
	RunnerStatus string `json:"runner_status"`
	Message      string `json:"message,omitempty"`
}

// skillEngagedRequest is the JSON body sent when the agent engages a skill.
type skillEngagedRequest struct {
	CardID    string `json:"card_id"`
	Project   string `json:"project"`
	SkillName string `json:"skill_name"`
}

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
// (see CTXRUN-048). The fallback is deprecated and logs a WARN at startup.
type Client struct {
	httpClient                 *http.Client
	contextMatrixURL           string
	apiKey                     string
	logger                     *slog.Logger
	useHMACForVerifyAutonomous bool
	metrics                    *metrics.Metrics
}

// NewClient creates a new callback client. By default VerifyAutonomous is
// HMAC-signed; use SetUseHMACForVerifyAutonomous(false) during the
// cross-repo transition if the ContextMatrix server still expects Bearer.
// The HTTP transport is wrapped with otelhttp so every outgoing request
// becomes a child span of whatever caller context the request is made in.
func NewClient(cmURL, apiKey string, logger *slog.Logger) *Client {
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
// cross-repo transition mode). See CTXRUN-048.
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
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		signature := cmhmac.SignPayloadWithTimestamp(c.apiKey, http.MethodPost, statusURI, body, ts)

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

		// CTXRUN-059 (M19): explicit Timer + defer Stop so ctx cancellation
		// does not leak the timer (time.After drops its reference only when
		// it fires). backoff is per-attempt, so declaring the timer inside
		// the loop body is correct — each attempt gets a fresh timer.
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
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		signature := cmhmac.SignPayloadWithTimestamp(c.apiKey, http.MethodPost, skillURI, body, ts)

		reqURL := c.contextMatrixURL + "/api/runner/skill-engaged"

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create skill-engaged request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(cmhmac.SignatureHeader, "sha256="+signature)
		req.Header.Set(cmhmac.TimestampHeader, ts)

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
// The request is HMAC-signed by default (CTXRUN-048). The signature covers
// the timestamp concatenated with an empty body, identical to every other
// runner<->CM webhook so the CM handler uses one verification path.
// During the cross-repo transition, SetUseHMACForVerifyAutonomous(false)
// switches back to `Authorization: Bearer <apiKey>` so the runner keeps
// working against an older CM server that does not yet accept HMAC on
// this endpoint.
//
// project and cardID are url.PathEscape'd unconditionally (M27) so values
// like "my project" or "CARD/42" produce a well-formed URL in either mode.
func (c *Client) VerifyAutonomous(ctx context.Context, project, cardID string) (bool, error) {
	reqURL := fmt.Sprintf("%s/api/v1/cards/%s/%s/autonomous",
		c.contextMatrixURL,
		url.PathEscape(project),
		url.PathEscape(cardID),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, fmt.Errorf("create verify-autonomous request: %w", err)
	}

	if c.useHMACForVerifyAutonomous {
		// HMAC bound to method+URI with an empty body. Binding the URI
		// (path + query) prevents a captured signature from being replayed
		// against a different endpoint, and binding the timestamp prevents
		// replay outside the clock-skew window.
		ts := strconv.FormatInt(time.Now().Unix(), 10)

		uri, perr := verifyAutonomousURI(reqURL)
		if perr != nil {
			return false, perr
		}

		signature := cmhmac.SignPayloadWithTimestamp(c.apiKey, http.MethodGet, uri, nil, ts)
		req.Header.Set(cmhmac.SignatureHeader, "sha256="+signature)
		req.Header.Set(cmhmac.TimestampHeader, ts)
	} else {
		// Deprecated Bearer fallback — retained only so the runner stays
		// compatible with a CM server that has not yet rolled the HMAC
		// change. Leaks the HMAC secret to anyone who can read the
		// Authorization header; remove once the server accepts HMAC.
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+signature)
	req.Header.Set(cmhmac.TimestampHeader, ts)

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
// string and fragment (which can embed credentials or tokens). If the URL
// cannot be parsed it is replaced with "<invalid-url>" so nothing leaks.
func sanitizeURLForError(fullURL string) string {
	u, err := url.Parse(fullURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<invalid-url>"
	}

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
