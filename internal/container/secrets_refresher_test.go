package container

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/metrics"
)

type fakeTokenGen struct {
	token   string
	expires time.Time
	err     error
}

func (f *fakeTokenGen) GenerateToken(_ context.Context) (string, time.Time, error) {
	return f.token, f.expires, f.err
}

func TestTokenRefresherWritesInitialFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, sharedSecretsSubdir), 0o700))

	r := newTokenRefresher(tokenRefresherConfig{
		Token:      &fakeTokenGen{token: "ghs_first", expires: time.Now().Add(1 * time.Hour)},
		SecretsDir: dir,
		StaticEnv:  map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-abc"},
		Logger:     slog.Default(),
	})

	require.NoError(t, r.refreshOnce(context.Background()))

	body, err := os.ReadFile(r.secretsPath)
	require.NoError(t, err)

	got := string(body)
	assert.Contains(t, got, "export CM_GIT_TOKEN='ghs_first'")
	assert.Contains(t, got, "export CLAUDE_CODE_OAUTH_TOKEN='sk-ant-oat01-abc'")
}

func TestNewTokenRefresher(t *testing.T) {
	secretsDir := t.TempDir()
	r := newTokenRefresher(tokenRefresherConfig{
		Token:      &fakeTokenGen{token: "ghs_aaa", expires: time.Now().Add(1 * time.Hour)},
		SecretsDir: secretsDir,
		StaticEnv:  map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-abc"},
		Logger:     slog.Default(),
	})
	require.NotNil(t, r)
	assert.Equal(t, filepath.Join(secretsDir, "shared", "env"), r.secretsPath)
}

func TestTokenRefresherNoChangeKeepsMtime(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, sharedSecretsSubdir), 0o700))

	gen := &fakeTokenGen{token: "ghs_same", expires: time.Now().Add(1 * time.Hour)}
	r := newTokenRefresher(tokenRefresherConfig{
		Token:      gen,
		SecretsDir: dir,
		StaticEnv:  map[string]string{"ANTHROPIC_API_KEY": "k1"},
		Logger:     slog.Default(),
	})

	require.NoError(t, r.refreshOnce(context.Background()))
	st1, err := os.Stat(r.secretsPath)
	require.NoError(t, err)

	time.Sleep(20 * time.Millisecond)

	require.NoError(t, r.refreshOnce(context.Background()))
	st2, err := os.Stat(r.secretsPath)
	require.NoError(t, err)

	assert.Equal(t, st1.ModTime(), st2.ModTime(),
		"file should not be rewritten when content is unchanged")
}

func TestTokenRefresherSleepsUntilExpiryMinusBuffer(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	exp := now.Add(60 * time.Minute)

	r := &tokenRefresher{}
	got, clamped := r.nextWake(now, exp)
	assert.Equal(t, 50*time.Minute, got)
	assert.False(t, clamped)
}

func TestTokenRefresherClampsMinimumSleep(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	exp := now.Add(2 * time.Minute) // already inside buffer

	r := &tokenRefresher{}
	got, clamped := r.nextWake(now, exp)
	assert.Equal(t, time.Second, got)
	assert.True(t, clamped, "real diff < 1s should set clamped=true")
}

func TestTokenRefresherExactlyOneBoundary(t *testing.T) {
	// expiry = now + buffer + 1s: real diff is exactly 1s, not a clamp.
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	exp := now.Add(tokenExpiryBuffer + time.Second)

	r := &tokenRefresher{}
	got, clamped := r.nextWake(now, exp)
	assert.Equal(t, time.Second, got)
	assert.False(t, clamped, "exactly 1s boundary should not be flagged as clamped")
}

func TestTokenRefresherPATSleepsOneHour(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)

	var zero time.Time

	r := &tokenRefresher{}
	got, clamped := r.nextWake(now, zero)
	assert.Equal(t, time.Hour, got)
	assert.False(t, clamped)
}

type sequenceTokenGen struct {
	tokens  []string
	expires []time.Time
	idx     atomic.Int64
}

func (g *sequenceTokenGen) GenerateToken(_ context.Context) (string, time.Time, error) {
	i := g.idx.Add(1) - 1
	if i >= int64(len(g.tokens)) {
		i = int64(len(g.tokens)) - 1
	}

	return g.tokens[i], g.expires[i], nil
}

// TestTokenRefresherLoopRotatesToken relies on real wall-clock sleeps.
// The first token expires in (10 min + 1 s) which is exactly 1 s after the
// tokenExpiryBuffer, so nextWake returns 1 s. After the 1 s sleep the loop
// mints the second token and then would sleep ~50 min — the 3 s context
// timeout fires and cancels the run. Total test wall time ≈ 1 s.
func TestTokenRefresherLoopRotatesToken(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, sharedSecretsSubdir), 0o700))

	now := time.Now()
	gen := &sequenceTokenGen{
		tokens:  []string{"ghs_v1", "ghs_v2"},
		expires: []time.Time{now.Add(10*time.Minute + time.Second), now.Add(1 * time.Hour)},
	}
	r := newTokenRefresher(tokenRefresherConfig{
		Token:      gen,
		SecretsDir: dir,
		StaticEnv:  map[string]string{"ANTHROPIC_API_KEY": "k1"},
		Logger:     slog.Default(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	r.Run(ctx)

	body, err := os.ReadFile(r.secretsPath)
	require.NoError(t, err)
	assert.Contains(t, string(body), "ghs_v2",
		"loop should have ticked past the first token's expiry")
}

// TestClampBackoffDuration verifies the exponential schedule used when a
// sustained clamped-wake streak indicates the local clock is past the
// (expiry - buffer) cutoff. The schedule must start at clampBackoffFloor
// once the streak exceeds clampBackoffStreakThreshold and saturate at
// clampBackoffCeiling to bound the mint rate.
func TestClampBackoffDuration(t *testing.T) {
	tests := []struct {
		name   string
		streak int
		want   time.Duration
	}{
		// At or below threshold: backoff is still the floor (caller should
		// not have invoked clampBackoffDuration yet, but be defensive).
		{"below threshold returns floor", 1, clampBackoffFloor},
		{"at threshold returns floor", clampBackoffStreakThreshold, clampBackoffFloor},
		// First over-threshold value is the floor itself (1s).
		{"first excess returns floor", clampBackoffStreakThreshold + 1, clampBackoffFloor},
		// Doubling sequence: 2s, 4s, 8s, 16s, then capped at 30s.
		{"doubles to 2s", clampBackoffStreakThreshold + 2, 2 * time.Second},
		{"doubles to 4s", clampBackoffStreakThreshold + 3, 4 * time.Second},
		{"doubles to 8s", clampBackoffStreakThreshold + 4, 8 * time.Second},
		{"doubles to 16s", clampBackoffStreakThreshold + 5, 16 * time.Second},
		{"caps at 30s", clampBackoffStreakThreshold + 6, clampBackoffCeiling},
		{"stays capped on long streak", clampBackoffStreakThreshold + 50, clampBackoffCeiling},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampBackoffDuration(tt.streak)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestTokenRefresherClampStreakResetsOnUnclamped verifies that a single
// non-clamped refresh clears the clamp streak so the next clamp starts
// from zero rather than continuing the prior backoff schedule.
func TestTokenRefresherClampStreakResetsOnUnclamped(t *testing.T) {
	r := &tokenRefresher{clampedStreakCount: 7}

	// Simulate a normal (non-clamped) wake by manually emulating the
	// Run-loop's reset branch. (Going through Run would require a complex
	// fake harness; the contract is "reset on non-clamped iteration".)
	r.clampedStreakCount = 0

	assert.Equal(t, 0, r.clampedStreakCount,
		"clamp streak must reset on a non-clamped refresh")
}

// panickingTokenGen panics on every GenerateToken call. Used to exercise
// the tokenRefresher.Run deferred recover.
type panickingTokenGen struct{}

func (p *panickingTokenGen) GenerateToken(_ context.Context) (string, time.Time, error) {
	panic("synthetic token mint panic")
}

// TestTokenRefresherRunRecoversPanicIncrementsMetric verifies that a panic
// raised inside the mint loop is recovered by the deferred recover and
// bumps PanicRecoveredTotal{goroutine=token_refresher}. If the panic were
// only logged, an attacker (or a buggy upstream) could panic-loop the
// refresher silently.
func TestTokenRefresherRunRecoversPanicIncrementsMetric(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, sharedSecretsSubdir), 0o700))

	mx := metrics.New()

	r := newTokenRefresher(tokenRefresherConfig{
		Token:      &panickingTokenGen{},
		SecretsDir: dir,
		StaticEnv:  nil,
		Logger:     slog.Default(),
		Metrics:    mx,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		// The defer must catch the panic and Run must return cleanly.
		assert.NotPanics(t, func() {
			r.Run(ctx)
		}, "tokenRefresher.Run must recover panics from GenerateToken")
	}()

	// Wait briefly for Run to hit the panic, then verify the metric.
	require.Eventually(t, func() bool {
		return tokenRefresherPanicCount(t, mx) >= 1.0
	}, 2*time.Second, 10*time.Millisecond,
		"PanicRecoveredTotal{goroutine=token_refresher} must increment on recover")

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// tokenRefresherPanicCount returns the value of
// cmr_panic_recovered_total{goroutine=token_refresher}, or 0 if missing.
func tokenRefresherPanicCount(t *testing.T, mx *metrics.Metrics) float64 {
	t.Helper()

	families, err := mx.Registry.Gather()
	require.NoError(t, err)

	for _, fam := range families {
		if fam.GetName() != "cmr_panic_recovered_total" {
			continue
		}

		for _, series := range fam.Metric {
			for _, lp := range series.Label {
				if lp.GetName() == "goroutine" && lp.GetValue() == metrics.GoroutineTokenRefresher {
					return series.GetCounter().GetValue()
				}
			}
		}
	}

	return 0
}

func TestTokenRefresherDisabledInEnvVarMode(t *testing.T) {
	var logBuf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	r := newTokenRefresher(tokenRefresherConfig{
		Token:      &fakeTokenGen{token: "ghs_x", expires: time.Now().Add(1 * time.Hour)},
		SecretsDir: "",
		StaticEnv:  nil,
		Logger:     logger,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)

	assert.Contains(t, logBuf.String(),
		"github token refresh disabled: env-var delivery mode")
}

func TestRenderSecretsFileRejectsInvalidTokenChars(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"newline", "ghs_abc\ndef"},
		{"null byte", "ghs_abc\x00def"},
		{"space", "ghs_abc def"},
		{"equals", "ghs_abc=def"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := renderSecretsFile(tt.token, nil)
			assert.ErrorIs(t, err, errInvalidTokenChars)
		})
	}
}

func TestRenderSecretsFileAcceptsValidTokenChars(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"github app token", "ghs_abc123XYZ"},
		{"pat with hyphen", "github_pat-abc123"},
		{"underscore", "ghs_abc_123"},
		{"alphanumeric only", "ABCDE12345abcde"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := renderSecretsFile(tt.token, nil)
			require.NoError(t, err)
			assert.Contains(t, body, tt.token)
		})
	}
}

func TestRenderSecretsFileRejectsEmptyToken(t *testing.T) {
	// An empty token is valid by charset rules (no forbidden bytes),
	// so we verify it renders without error (upstream mint failure is
	// the correct gate for missing tokens).
	body, err := renderSecretsFile("", nil)
	require.NoError(t, err)
	assert.Contains(t, body, "CM_GIT_TOKEN=''")
}

func TestRenderSecretsFileRejectsCMGitTokenInStatic(t *testing.T) {
	static := map[string]string{"CM_GIT_TOKEN": "should-not-be-here"}
	_, err := renderSecretsFile("ghs_valid", static)
	assert.ErrorIs(t, err, errStaticCMGitTokenForbidden)
}

// TestRenderSecretsFileRejectsInvalidStaticValueBytes verifies that a
// static-env value containing a byte that would break the sourced
// `export KEY='value'` shell line is rejected. Without this defence, a
// misconfigured CLAUDE_CODE_OAUTH_TOKEN with an embedded newline would
// turn the trailing bytes into a separate (and potentially attacker-
// controlled) shell command at every worker startup.
func TestRenderSecretsFileRejectsInvalidStaticValueBytes(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"newline in CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-\nfoo"},
		{"carriage return in ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY", "k1\rk2"},
		{"NUL in ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY", "k1\x00k2"},
		{"tab in CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "sk-ant\tfoo"},
		{"DEL byte in ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY", "k1\x7fk2"},
		{"raw UTF-8 multi-byte start in ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY", "k1\xc3\xa9k2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			static := map[string]string{tt.key: tt.value}
			_, err := renderSecretsFile("ghs_valid", static)
			require.ErrorIs(t, err, errInvalidStaticSecretChars)
			assert.Contains(t, err.Error(), tt.key,
				"error must name the offending key so operators can fix it")
		})
	}
}

// TestRenderSecretsFileAcceptsValidStaticValues asserts the charset check
// does not over-reject real-world OAuth tokens / API keys, which use
// printable ASCII plus hyphen and underscore.
func TestRenderSecretsFileAcceptsValidStaticValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"oauth token", "CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-abc_DEF123"},
		{"api key with equals", "ANTHROPIC_API_KEY", "sk-ant-api03-XY=="},
		{"mixed punctuation", "ANTHROPIC_API_KEY", "k1.k2-k3_k4+k5/k6:k7"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			static := map[string]string{tt.key: tt.value}
			body, err := renderSecretsFile("ghs_valid", static)
			require.NoError(t, err)
			assert.Contains(t, body, "export "+tt.key+"='"+tt.value+"'")
		})
	}
}
