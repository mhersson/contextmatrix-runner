package webhook

import protocol "github.com/mhersson/contextmatrix-protocol"

// Stable error codes are defined in contextmatrix-protocol; re-exported
// so handler call sites keep reading webhook.Code*.
const (
	CodeInvalidJSON     = protocol.CodeInvalidJSON
	CodeInvalidField    = protocol.CodeInvalidField
	CodeUnauthorized    = protocol.CodeUnauthorized
	CodeNotFound        = protocol.CodeNotFound
	CodeConflict        = protocol.CodeConflict
	CodeForbidden       = protocol.CodeForbidden
	CodeLimitReached    = protocol.CodeLimitReached
	CodeTooLarge        = protocol.CodeTooLarge
	CodeDuplicate       = protocol.CodeDuplicate
	CodeStdinClosed     = protocol.CodeStdinClosed
	CodeUpstreamFailure = protocol.CodeUpstreamFailure
	CodeDraining        = protocol.CodeDraining
	CodeInternal        = protocol.CodeInternal
)

// Stable error messages reused across handlers so the wire-level shape stays
// consistent for the same condition regardless of whether the request came in
// through the card-mode or chat-mode code path. Clients should branch on
// Code, not on Message, but the messages still need to match so operator
// dashboards and log aggregators are not split by an irrelevant string
// difference. See Fix W1 in REVIEW.md.
const (
	// MsgNotInteractive is the wire message returned with CodeConflict when a
	// container exists but stdin is not attached (the container was started
	// in non-interactive mode). Used by /message, /promote, /end-session for
	// both card-mode and chat-mode requests.
	MsgNotInteractive = "container is not in interactive mode"

	// MsgNoContainerTracked is the wire message returned with CodeNotFound
	// when no container is tracked for the lookup key (card_id+project, or
	// session_id). Used by /message, /promote, /end-session, /chat/end.
	MsgNoContainerTracked = "no container tracked"
)
