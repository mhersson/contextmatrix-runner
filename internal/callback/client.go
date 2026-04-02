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
	"net/http"
	"strconv"
	"time"

	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
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

// Client sends signed status callbacks to ContextMatrix.
type Client struct {
	httpClient       *http.Client
	contextMatrixURL string
	apiKey           string
	logger           *slog.Logger
}

// NewClient creates a new callback client.
func NewClient(cmURL, apiKey string, logger *slog.Logger) *Client {
	return &Client{
		httpClient:       &http.Client{Timeout: requestTimeout},
		contextMatrixURL: cmURL,
		apiKey:           apiKey,
		logger:           logger,
	}
}

// ReportStatus sends a runner status update to ContextMatrix.
// Valid statuses: "running", "failed".
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

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	signature := cmhmac.SignPayloadWithTimestamp(c.apiKey, body, ts)

	var lastErr error
	for attempt := range maxRetries {
		lastErr = c.doRequest(ctx, body, signature, ts)
		if lastErr == nil {
			return nil
		}
		if isClientError(lastErr) {
			return lastErr
		}
		c.logger.Warn("callback failed, retrying",
			"attempt", attempt+1,
			"card_id", cardID,
			"error", lastErr,
		)
		backoff := time.Duration(1<<uint(attempt)) * time.Second
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return fmt.Errorf("callback failed after %d attempts: %w", maxRetries, lastErr)
}

func (c *Client) doRequest(ctx context.Context, body []byte, signature, ts string) error {
	url := c.contextMatrixURL + "/api/runner/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
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
		return &callbackError{
			statusCode: resp.StatusCode,
			body:       string(respBody),
		}
	}
	return nil
}

type callbackError struct {
	statusCode int
	body       string
}

func (e *callbackError) Error() string {
	return fmt.Sprintf("callback returned HTTP %d: %s", e.statusCode, e.body)
}

func isClientError(err error) bool {
	var ce *callbackError
	if errors.As(err, &ce) {
		return ce.statusCode >= 400 && ce.statusCode < 500
	}
	return false
}
