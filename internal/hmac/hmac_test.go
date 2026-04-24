package hmac

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const (
	testMethodPOST = http.MethodPost
	testPath       = "/kill"
)

func TestSignPayloadWithTimestamp_Deterministic(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := "1700000000"

	sig1 := SignPayloadWithTimestamp(key, testMethodPOST, testPath, body, ts)
	sig2 := SignPayloadWithTimestamp(key, testMethodPOST, testPath, body, ts)
	assert.Equal(t, sig1, sig2)
	assert.Len(t, sig1, 64) // SHA-256 = 64 hex chars
}

func TestVerifySignatureWithTimestamp_Valid(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	sig := SignPayloadWithTimestamp(key, testMethodPOST, testPath, body, ts)
	assert.True(t, VerifySignatureWithTimestamp(key, testMethodPOST, testPath, sig, ts, body, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_WrongKey(t *testing.T) {
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	sig := SignPayloadWithTimestamp("key-a", testMethodPOST, testPath, body, ts)
	assert.False(t, VerifySignatureWithTimestamp("key-b", testMethodPOST, testPath, sig, ts, body, DefaultMaxClockSkew))
}

func TestSignPayloadWithTimestamp_DifferentKeys(t *testing.T) {
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := "1700000000"

	sig1 := SignPayloadWithTimestamp("key-a", testMethodPOST, testPath, body, ts)
	sig2 := SignPayloadWithTimestamp("key-b", testMethodPOST, testPath, body, ts)
	assert.NotEqual(t, sig1, sig2)
}

func TestSignPayloadWithTimestamp_DifferentBodies(t *testing.T) {
	key := "test-secret"
	ts := "1700000000"

	sig1 := SignPayloadWithTimestamp(key, testMethodPOST, testPath, []byte(`{"a":1}`), ts)
	sig2 := SignPayloadWithTimestamp(key, testMethodPOST, testPath, []byte(`{"a":2}`), ts)
	assert.NotEqual(t, sig1, sig2)
}

func TestSignPayloadWithTimestamp_DifferentTimestamps(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)

	sig1 := SignPayloadWithTimestamp(key, testMethodPOST, testPath, body, "1700000000")
	sig2 := SignPayloadWithTimestamp(key, testMethodPOST, testPath, body, "1700000001")
	assert.NotEqual(t, sig1, sig2)
}

// TestSignPayloadWithTimestamp_DifferentPath is the regression guard for the
// /end-session ↔ /kill replay-cache collision: identical body + ts + method
// signed under two different paths MUST produce distinct signatures.
func TestSignPayloadWithTimestamp_DifferentPath(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001","project":"p"}`)
	ts := "1700000000"

	sigEndSession := SignPayloadWithTimestamp(key, testMethodPOST, "/end-session", body, ts)
	sigKill := SignPayloadWithTimestamp(key, testMethodPOST, "/kill", body, ts)
	assert.NotEqual(t, sigEndSession, sigKill)
}

func TestSignPayloadWithTimestamp_DifferentMethod(t *testing.T) {
	key := "test-secret"
	body := []byte{}
	ts := "1700000000"

	sigGet := SignPayloadWithTimestamp(key, http.MethodGet, "/containers", body, ts)
	sigPost := SignPayloadWithTimestamp(key, http.MethodPost, "/containers", body, ts)
	assert.NotEqual(t, sigGet, sigPost)
}

func TestVerifySignatureWithTimestamp_TamperedBody(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	sig := SignPayloadWithTimestamp(key, testMethodPOST, testPath, body, ts)
	tampered := []byte(`{"card_id":"PROJ-002"}`)
	assert.False(t, VerifySignatureWithTimestamp(key, testMethodPOST, testPath, sig, ts, tampered, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_TamperedPath(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	sig := SignPayloadWithTimestamp(key, testMethodPOST, "/end-session", body, ts)
	assert.False(t, VerifySignatureWithTimestamp(key, testMethodPOST, "/kill", sig, ts, body, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_TamperedMethod(t *testing.T) {
	key := "test-secret"
	body := []byte{}
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	sig := SignPayloadWithTimestamp(key, http.MethodGet, "/containers", body, ts)
	assert.False(t, VerifySignatureWithTimestamp(key, http.MethodPost, "/containers", sig, ts, body, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_Expired(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)

	sig := SignPayloadWithTimestamp(key, testMethodPOST, testPath, body, ts)
	assert.False(t, VerifySignatureWithTimestamp(key, testMethodPOST, testPath, sig, ts, body, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_FutureTimestamp(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)

	sig := SignPayloadWithTimestamp(key, testMethodPOST, testPath, body, ts)
	assert.False(t, VerifySignatureWithTimestamp(key, testMethodPOST, testPath, sig, ts, body, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_InvalidTimestamp(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)

	sig := SignPayloadWithTimestamp(key, testMethodPOST, testPath, body, "not-a-number")
	assert.False(t, VerifySignatureWithTimestamp(key, testMethodPOST, testPath, sig, "not-a-number", body, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_EmptyTimestamp(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)

	assert.False(t, VerifySignatureWithTimestamp(key, testMethodPOST, testPath, "", "", body, DefaultMaxClockSkew))
}

// Cross-compatibility: verify that our signing is deterministic and produces
// a known-vector shape.
func TestSignPayloadWithTimestamp_KnownVector(t *testing.T) {
	key := "my-secret-key-for-testing-1234567890"
	body := []byte(`{"card_id":"PROJ-042","project":"my-project"}`)
	ts := "1700000000"

	sig := SignPayloadWithTimestamp(key, testMethodPOST, testPath, body, ts)
	assert.Len(t, sig, 64)
	assert.Equal(t, sig, SignPayloadWithTimestamp(key, testMethodPOST, testPath, body, ts))
}
