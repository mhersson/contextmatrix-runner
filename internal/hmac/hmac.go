// Package hmac provides HMAC-SHA256 signing and verification for webhooks.
// This is a standalone package to avoid import cycles between webhook and callback.
package hmac

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

const (
	// DefaultMaxClockSkew is the maximum allowed age for webhook timestamps.
	DefaultMaxClockSkew = 5 * time.Minute

	// SignatureHeader carries the HMAC-SHA256 signature.
	SignatureHeader = "X-Signature-256"

	// TimestampHeader carries the Unix timestamp used in HMAC computation.
	TimestampHeader = "X-Webhook-Timestamp"
)

// SignPayloadWithTimestamp computes an HMAC-SHA256 signature bound to the
// HTTP method, request path, timestamp, and body. The signed content is:
//
//	method + "\n" + path + "\n" + timestamp + "." + body
//
// Including method and path prevents a valid signature for one endpoint from
// being replayed against another endpoint with an identical body — important
// because /kill and /stop-all share an overlapping {card_id, project} payload
// shape and would otherwise produce colliding signatures when issued
// back-to-back in the same Unix second.
func SignPayloadWithTimestamp(key, method, path string, body []byte, ts string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(method))
	mac.Write([]byte("\n"))
	mac.Write([]byte(path))
	mac.Write([]byte("\n"))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)

	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignatureWithTimestamp checks the HMAC-SHA256 signature against the
// expected value computed over method/path/timestamp/body, and rejects
// payloads with timestamps outside the allowed clock-skew window.
func VerifySignatureWithTimestamp(key, method, path, signature, timestamp string, body []byte, maxSkew time.Duration) bool {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}

	age := time.Since(time.Unix(ts, 0))
	if age < -maxSkew || age > maxSkew {
		return false
	}

	expected := SignPayloadWithTimestamp(key, method, path, body, timestamp)

	return hmac.Equal([]byte(expected), []byte(signature))
}
