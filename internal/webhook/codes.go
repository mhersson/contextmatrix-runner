package webhook

// Stable error codes returned in ErrorResponse.Code. These form a public API
// surface: CM (and any future clients) should branch on Code, not on Message.
// Keep the set small and well-defined; do not add codes for one-off situations.
const (
	// CodeInvalidJSON is returned when the request body cannot be decoded as
	// JSON at all (leading brace missing, truncated, etc.). Status: 400.
	CodeInvalidJSON = "invalid_json"

	// CodeInvalidField is returned for ingress validation failures. The
	// Message includes the field name (not the value). Status: 400.
	CodeInvalidField = "invalid_field"

	// CodeUnauthorized is returned by the HMAC middleware for every
	// authentication failure (missing header, bad signature, expired
	// timestamp, unreadable body). The body is a fixed generic shape — the
	// specific reason is logged server-side only. Status: 401.
	CodeUnauthorized = "unauthorized"

	// CodeConflict is returned for resource-state conflicts: a card is already
	// being tracked for the requested project (on /trigger). Deliberately does
	// NOT reveal whether the card is the SAME card or DIFFERENT — just that
	// the state conflicts. Status: 409.
	CodeConflict = "conflict"

	// CodeLimitReached is returned by /trigger when max_concurrent has been
	// hit. Status: 429.
	CodeLimitReached = "limit_reached"

	// CodeTooLarge is returned when a request field exceeds its size cap.
	// Status: 413.
	CodeTooLarge = "too_large"

	// CodeDuplicate is returned by the HMAC middleware when a signature has
	// already been accepted inside the replay window. Status: 409.
	CodeDuplicate = "duplicate"

	// CodeUpstreamFailure is returned when an upstream Docker call fails in a
	// way that prevents the runner from completing the operation safely. The
	// body is a fixed generic shape so the upstream response cannot leak
	// tokens or other secrets into our response. Status: 502.
	CodeUpstreamFailure = "upstream_failure"

	// CodeDraining is returned by mutating endpoints when graceful shutdown
	// has started. The runner is refusing new work so it can finish existing
	// work before exiting. Status: 503.
	CodeDraining = "draining"

	// CodeInternal is the catch-all for server-side bugs (marshal failures,
	// etc). Message is a fixed string — raw err.Error() is NEVER echoed to
	// the client; the full error is logged server-side. Status: 500.
	CodeInternal = "internal"
)
