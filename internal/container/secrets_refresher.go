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
	"sync/atomic"
	"time"

	githubauth "github.com/mhersson/contextmatrix-githubauth"

	"github.com/mhersson/contextmatrix-runner/internal/metrics"
)

// Kept off the root of SecretsDir so other artefacts there (chat-resume
// dirs, etc.) stay out of the bind mount.
const sharedSecretsSubdir = "shared"

const sharedSecretsFile = "env"

// tokenExpiryBuffer is how long before the token expiry we schedule a refresh.
const tokenExpiryBuffer = 10 * time.Minute

// misconfigLogInterval throttles the static-CM_GIT_TOKEN error log so a
// surviving misconfig (initial mint already fail-fasted, but the loop can
// hit it again on env mutation) does not spam every backoff tick.
const misconfigLogInterval = 5 * time.Minute

var errInvalidTokenChars = errors.New("github token contains invalid characters")

var errStaticCMGitTokenForbidden = errors.New("static env must not contain CM_GIT_TOKEN: refresher owns that key")

// errInvalidStaticSecretChars is returned by renderSecretsFile when a value
// in the static map (CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY) contains
// a byte that would break the sourced `export KEY='value'` shell line —
// NUL, control bytes, or any byte outside printable ASCII. The static
// map flows through shellSingleQuoteEscape, but escaping does not stop
// a literal newline from terminating the export line and turning the
// trailing bytes into a separate shell statement.
//
// This charset check is also mirrored in config.Config.Validate() via
// validatePrintableASCIISecret for ClaudeOAuthToken and AnthropicAPIKey,
// giving misconfigured operators a boot-time error. This runtime check
// here is defence-in-depth during token refresh.
var errInvalidStaticSecretChars = errors.New("static secret value contains invalid characters")

// IsValidStaticSecretByte reports whether b is acceptable inside the
// shell-sourced /run/cm-secrets/env value of a static secret. Real
// OAuth tokens and API keys live in the printable-ASCII range; rejecting
// everything outside that range catches the dangerous bytes (NUL, \n,
// \r, \t, escape sequences, raw UTF-8 multi-byte starts) without
// over-fitting to current token shapes.
func IsValidStaticSecretByte(b byte) bool {
	return b >= 0x20 && b <= 0x7e
}

// validateStaticSecretValue returns errInvalidStaticSecretChars if v
// contains any byte rejected by IsValidStaticSecretByte. An empty string
// is acceptable — upstream config validation rejects empty configured
// values; the static map only contains keys whose values were non-empty
// at buildStaticAuthEnv time.
func validateStaticSecretValue(v string) error {
	for i := 0; i < len(v); i++ {
		if !IsValidStaticSecretByte(v[i]) {
			return errInvalidStaticSecretChars
		}
	}

	return nil
}

type tokenRefresherConfig struct {
	Token      githubauth.TokenGenerator
	SecretsDir string
	StaticEnv  map[string]string
	Logger     *slog.Logger
	// Metrics is optional. When non-nil the refresher increments
	// PanicRecoveredTotal{goroutine=GoroutineTokenRefresher} from its
	// deferred recover so a panic in mint / file-write paths is visible to
	// operators even though Run() swallows the panic.
	Metrics *metrics.Metrics
}

// tokenRefresher is a background loop that rewrites the shared secrets
// file before the current GitHub token expires.
type tokenRefresher struct {
	token       githubauth.TokenGenerator
	secretsPath string
	staticEnv   map[string]string
	logger      *slog.Logger
	disabled    bool
	metrics     *metrics.Metrics

	// lastMisconfigLog is the unix-nano timestamp of the last
	// errStaticCMGitTokenForbidden log emission. Used to rate-limit a noisy
	// misconfig path so the loop logs at most once per misconfigLogInterval.
	lastMisconfigLog atomic.Int64

	// clampedStreakCount tracks consecutive refresh iterations whose
	// nextWake duration was clamped to the 1s minimum. Sustained clamping
	// means the local clock is past the (expiry - buffer) cutoff already,
	// either because dockerd is too slow to mint a fresh token or because
	// the local clock is skewed forward of GitHub's. Once the streak
	// exceeds clampBackoffStreakThreshold the loop applies exponential
	// backoff (capped at clampBackoffCeiling) instead of minting every
	// second.
	clampedStreakCount int
}

// Clamp-streak backoff bounds. clampBackoffStreakThreshold is the number of
// consecutive clamps before we start backing off; clampBackoffFloor /
// clampBackoffCeiling bracket the exponential schedule (1s → 2s → 4s → …
// → 30s).
const (
	clampBackoffStreakThreshold = 3
	clampBackoffFloor           = time.Second
	clampBackoffCeiling         = 30 * time.Second
)

func newTokenRefresher(c tokenRefresherConfig) *tokenRefresher {
	r := &tokenRefresher{
		token:     c.Token,
		staticEnv: c.StaticEnv,
		logger:    c.Logger.With("component", "token_refresher"),
		disabled:  c.SecretsDir == "",
		metrics:   c.Metrics,
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

// shellSingleQuoteEscape returns s with every `'` replaced by `'\”` so the
// result can be safely embedded inside a single-quoted shell string. Lives
// here (rather than in manager.go) because renderSecretsFile is its only
// caller — keeping helper and consumer in the same file makes the
// secrets-rendering surface easier to audit.
func shellSingleQuoteEscape(s string) string {
	return strings.ReplaceAll(s, `'`, `'\''`)
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

		isAllowed := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !isAllowed {
			return "", errInvalidTokenChars
		}
	}

	// Reject any static-value byte that would break the sourced shell
	// `export KEY='value'` line. shellSingleQuoteEscape neutralises a
	// literal `'` but cannot stop a literal newline from terminating the
	// export statement and turning trailing bytes into a separate (and
	// possibly attacker-controlled) shell command. Apply the check
	// before clone+escape so callers fail-fast at the refresh attempt
	// rather than at the worker's `set -a; . /run/cm-secrets/env`.
	for _, k := range slices.Sorted(maps.Keys(static)) {
		if err := validateStaticSecretValue(static[k]); err != nil {
			return "", fmt.Errorf("static env %q: %w", k, err)
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

	// fsync before rename so a host crash during rotation can't leave a
	// torn (truncated or zero-length) file behind on persistent storage.
	// On tmpfs this is effectively a no-op; on a real fs this is the
	// difference between losing a token rotation and losing the file.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)

		return fmt.Errorf("fsync %s: %w", tmp, err)
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
			if r.metrics != nil {
				r.metrics.PanicRecoveredTotal.WithLabelValues(metrics.GoroutineTokenRefresher).Inc()
			}

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
					"error", err, "next_retry_in", backoff)
			case errors.Is(err, errStaticCMGitTokenForbidden):
				// Config bug: static env must not contain CM_GIT_TOKEN.
				// The initial synchronous mint in StartTokenRefresher already
				// fail-fasts on this; if we still reach the loop branch the
				// misconfig is unrecoverable until config is fixed. Rate-limit
				// the log to misconfigLogInterval so a wedged config does not
				// spam every backoff tick.
				now := time.Now().UnixNano()
				prev := r.lastMisconfigLog.Load()

				if prev == 0 || now-prev >= misconfigLogInterval.Nanoseconds() {
					if r.lastMisconfigLog.CompareAndSwap(prev, now) {
						r.logger.Error("github token refresh misconfigured",
							"error", err, "next_retry_in", backoff)
					}
				}
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
		// (real diff < 1s), the token may be expiring immediately. Bound
		// the mint rate under sustained clamping so a forward-skewed clock
		// (or a token whose expiry is permanently inside the buffer) does
		// not pin GitHub mint at one per second forever.
		if clamped {
			r.clampedStreakCount++

			if r.clampedStreakCount > clampBackoffStreakThreshold {
				wake = clampBackoffDuration(r.clampedStreakCount)

				r.logger.Warn("github token: sustained clamped streak; applying backoff",
					"expires_at", expiresAt,
					"now", time.Now(),
					"streak", r.clampedStreakCount,
					"wake", wake)
			} else {
				r.logger.Warn("github token: expiry clamped to minimum",
					"expires_at", expiresAt, "now", time.Now(), "wake", wake)
			}
		} else {
			r.clampedStreakCount = 0
		}

		if !sleep(ctx, wake) {
			r.logger.Info("github token refresher stopped")

			return
		}
	}
}

// clampBackoffDuration returns the exponential backoff for a clamped-wake
// streak, capped at clampBackoffCeiling. streak is the total number of
// consecutive clamps; backoff starts once it exceeds
// clampBackoffStreakThreshold.
//
//	streak=4  → 2s
//	streak=5  → 4s
//	streak=6  → 8s
//	streak=7  → 16s
//	streak>=8 → 30s (capped)
func clampBackoffDuration(streak int) time.Duration {
	excess := streak - clampBackoffStreakThreshold
	if excess < 1 {
		return clampBackoffFloor
	}

	// 1 << (excess-1) doubles each iteration above the floor. Cap the
	// shift well before overflow — anything >= 30s saturates at
	// clampBackoffCeiling.
	const maxShift = 30
	if excess-1 >= maxShift {
		return clampBackoffCeiling
	}

	d := clampBackoffFloor << (excess - 1)
	if d > clampBackoffCeiling {
		return clampBackoffCeiling
	}

	return d
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
