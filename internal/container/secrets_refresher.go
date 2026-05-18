package container

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	githubauth "github.com/mhersson/contextmatrix-githubauth"
)

// Kept off the root of SecretsDir so other artefacts there (chat-resume
// dirs, etc.) stay out of the bind mount.
const sharedSecretsSubdir = "shared"

const sharedSecretsFile = "env"

// tokenExpiryBuffer is how long before the token expiry we schedule a refresh.
const tokenExpiryBuffer = 10 * time.Minute

var errInvalidTokenChars = errors.New("github token contains invalid characters")

var errStaticCMGitTokenForbidden = errors.New("static env must not contain CM_GIT_TOKEN: refresher owns that key")

type tokenRefresherConfig struct {
	Token      githubauth.TokenGenerator
	SecretsDir string
	StaticEnv  map[string]string
	Logger     *slog.Logger
}

// tokenRefresher is a background loop that rewrites the shared secrets
// file before the current GitHub token expires.
type tokenRefresher struct {
	token       githubauth.TokenGenerator
	secretsPath string
	staticEnv   map[string]string
	logger      *slog.Logger
	disabled    bool
}

func newTokenRefresher(c tokenRefresherConfig) *tokenRefresher {
	r := &tokenRefresher{
		token:     c.Token,
		staticEnv: c.StaticEnv,
		logger:    c.Logger.With("component", "token_refresher"),
		disabled:  c.SecretsDir == "",
	}
	if !r.disabled {
		r.secretsPath = filepath.Join(c.SecretsDir, sharedSecretsSubdir, sharedSecretsFile)
	}

	return r
}

// doOneRefresh performs a single mint-and-conditional-rewrite cycle.
// Returns (changed, expiresAt, err). changed is false when the minted
// token bytes match the file's current content.
func (r *tokenRefresher) doOneRefresh(ctx context.Context) (changed bool, expiresAt time.Time, err error) {
	token, exp, err := r.token.GenerateToken(ctx)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("mint github token: %w", err)
	}

	body, err := renderSecretsFile(token, r.staticEnv)
	if err != nil {
		return false, exp, err
	}

	// Size-check before reading to avoid loading an arbitrarily large file
	// into RAM (e.g. a large file pinned at the bind-mount path).
	// Only attempt a byte-for-byte compare when sizes already match.
	if st, statErr := os.Stat(r.secretsPath); statErr == nil && st.Size() == int64(len(body)) {
		existing, readErr := os.ReadFile(r.secretsPath)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			r.logger.Warn("github token: unexpected read error before rewrite",
				"path", r.secretsPath, "error", readErr)
		}

		if readErr == nil && string(existing) == body {
			return false, exp, nil
		}
	}

	if err := writeSecretsFileAtomic(r.secretsPath, body); err != nil {
		return false, exp, fmt.Errorf("write secrets: %w", err)
	}

	return true, exp, nil
}

func (r *tokenRefresher) refreshOnce(ctx context.Context) error {
	_, _, err := r.doOneRefresh(ctx)

	return err
}

func renderSecretsFile(gitToken string, static map[string]string) (string, error) {
	if _, exists := static["CM_GIT_TOKEN"]; exists {
		return "", errStaticCMGitTokenForbidden
	}

	// Reject any byte outside [A-Za-z0-9_-]; defends the credential helper
	// printf against newline injection that would emit a second
	// credential-protocol line that git parses.
	for i := 0; i < len(gitToken); i++ {
		c := gitToken[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return "", errInvalidTokenChars
		}
	}

	combined := maps.Clone(static)
	if combined == nil {
		combined = map[string]string{}
	}

	combined["CM_GIT_TOKEN"] = gitToken

	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(combined)) {
		fmt.Fprintf(&b, "export %s='%s'\n", k, shellSingleQuoteEscape(combined[k]))
	}

	return b.String(), nil
}

// Prevents partial reads from the bind-mounted worker.
func writeSecretsFileAtomic(path, body string) error {
	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", tmp, err)
	}

	// Force mode even if O_CREATE reused a stale inode with looser bits.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)

		return fmt.Errorf("chmod %s: %w", tmp, err)
	}

	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)

		return fmt.Errorf("write %s: %w", tmp, err)
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("close %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("rename %s: %w", path, err)
	}

	return nil
}

// nextWake returns the duration until the next refresh and whether the duration
// was clamped to the minimum. clamped is true only when the real diff fell
// below 1s; a natural 1s result (expiry exactly 1s after the buffer) returns
// (time.Second, false). PAT mode (zero expiry) returns (time.Hour, false).
func (r *tokenRefresher) nextWake(now, expiresAt time.Time) (time.Duration, bool) {
	if expiresAt.IsZero() {
		return time.Hour, false
	}

	d := expiresAt.Sub(now) - tokenExpiryBuffer
	if d < time.Second {
		return time.Second, true
	}

	return d, false
}

func (r *tokenRefresher) Run(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("github token refresher panicked", "panic", rec)
		}
	}()

	if r.disabled {
		r.logger.Info("github token refresh disabled: env-var delivery mode")

		return
	}

	r.logger.Info("github token refresher started", "path", r.secretsPath)

	backoff := time.Second

	const maxBackoff = 5 * time.Minute

	for {
		changed, expiresAt, err := r.doOneRefresh(ctx)
		if err != nil {
			switch {
			case errors.Is(err, errInvalidTokenChars):
				// Treat invalid token as a mint failure for backoff; retry
				// rather than exiting — could be a transient upstream bug.
				r.logger.Error("github token refresh rejected invalid bytes",
					"next_retry_in", backoff)
			case errors.Is(err, errStaticCMGitTokenForbidden):
				// Config bug: static env must not contain CM_GIT_TOKEN.
				// Keep retrying so the operator sees repeated errors, but
				// the loop will never succeed until config is fixed.
				r.logger.Error("github token refresh misconfigured",
					"error", err, "next_retry_in", backoff)
			default:
				r.logger.Warn("github token mint failed",
					"next_retry_in", backoff, "error", err)
			}

			if !sleep(ctx, backoff) {
				r.logger.Info("github token refresher stopped")

				return
			}

			backoff = nextBackoff(backoff, maxBackoff)

			continue
		}

		backoff = time.Second

		if changed {
			r.logger.Info("github token refreshed",
				"expires_at", expiresAt, "mode", tokenMode(expiresAt))
		} else {
			r.logger.Info("github token unchanged",
				"expires_at", expiresAt, "mode", tokenMode(expiresAt))
		}

		wake, clamped := r.nextWake(time.Now(), expiresAt)

		// Surface clock-skew hot loops: if nextWake clamped to the minimum
		// (real diff < 1s), the token may be expiring immediately.
		if clamped {
			r.logger.Warn("github token: expiry clamped to minimum",
				"expires_at", expiresAt, "now", time.Now(), "wake", wake)
		}

		if !sleep(ctx, wake) {
			r.logger.Info("github token refresher stopped")

			return
		}
	}
}

func nextBackoff(d, limit time.Duration) time.Duration {
	d *= 2
	if d > limit {
		return limit
	}

	return d
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func tokenMode(expiresAt time.Time) string {
	if expiresAt.IsZero() {
		return "pat"
	}

	return "app"
}
