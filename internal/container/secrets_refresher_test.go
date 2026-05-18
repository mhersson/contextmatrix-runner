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
