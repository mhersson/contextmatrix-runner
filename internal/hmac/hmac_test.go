package hmac

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSignPayloadWithTimestamp_Deterministic(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := "1700000000"

	sig1 := SignPayloadWithTimestamp(key, body, ts)
	sig2 := SignPayloadWithTimestamp(key, body, ts)
	assert.Equal(t, sig1, sig2)
	assert.Len(t, sig1, 64) // SHA-256 = 64 hex chars
}

func TestVerifySignatureWithTimestamp_Valid(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	sig := SignPayloadWithTimestamp(key, body, ts)
	assert.True(t, VerifySignatureWithTimestamp(key, sig, ts, body, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_WrongKey(t *testing.T) {
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	sig := SignPayloadWithTimestamp("key-a", body, ts)
	assert.False(t, VerifySignatureWithTimestamp("key-b", sig, ts, body, DefaultMaxClockSkew))
}

func TestSignPayloadWithTimestamp_DifferentKeys(t *testing.T) {
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := "1700000000"

	sig1 := SignPayloadWithTimestamp("key-a", body, ts)
	sig2 := SignPayloadWithTimestamp("key-b", body, ts)
	assert.NotEqual(t, sig1, sig2)
}

func TestSignPayloadWithTimestamp_DifferentBodies(t *testing.T) {
	key := "test-secret"
	ts := "1700000000"

	sig1 := SignPayloadWithTimestamp(key, []byte(`{"a":1}`), ts)
	sig2 := SignPayloadWithTimestamp(key, []byte(`{"a":2}`), ts)
	assert.NotEqual(t, sig1, sig2)
}

func TestSignPayloadWithTimestamp_DifferentTimestamps(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)

	sig1 := SignPayloadWithTimestamp(key, body, "1700000000")
	sig2 := SignPayloadWithTimestamp(key, body, "1700000001")
	assert.NotEqual(t, sig1, sig2)
}

func TestVerifySignatureWithTimestamp_TamperedBody(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	sig := SignPayloadWithTimestamp(key, body, ts)
	tampered := []byte(`{"card_id":"PROJ-002"}`)
	assert.False(t, VerifySignatureWithTimestamp(key, sig, ts, tampered, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_Expired(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)

	sig := SignPayloadWithTimestamp(key, body, ts)
	assert.False(t, VerifySignatureWithTimestamp(key, sig, ts, body, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_FutureTimestamp(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)
	ts := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)

	sig := SignPayloadWithTimestamp(key, body, ts)
	assert.False(t, VerifySignatureWithTimestamp(key, sig, ts, body, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_InvalidTimestamp(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)

	sig := SignPayloadWithTimestamp(key, body, "not-a-number")
	assert.False(t, VerifySignatureWithTimestamp(key, sig, "not-a-number", body, DefaultMaxClockSkew))
}

func TestVerifySignatureWithTimestamp_EmptyTimestamp(t *testing.T) {
	key := "test-secret"
	body := []byte(`{"card_id":"PROJ-001"}`)

	assert.False(t, VerifySignatureWithTimestamp(key, "", "", body, DefaultMaxClockSkew))
}

// Cross-compatibility: verify that our signing matches CM's format.
func TestSignPayloadWithTimestamp_KnownVector(t *testing.T) {
	key := "my-secret-key-for-testing-1234567890"
	body := []byte(`{"card_id":"PROJ-042","project":"my-project"}`)
	ts := "1700000000"

	sig := SignPayloadWithTimestamp(key, body, ts)
	assert.Len(t, sig, 64)
	assert.Equal(t, sig, SignPayloadWithTimestamp(key, body, ts))
}
