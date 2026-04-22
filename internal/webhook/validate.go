package webhook

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ValidationError indicates an incoming webhook payload failed ingress-level
// validation. It is intentionally terse so handlers do not echo user-supplied
// values back into the response body.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Reason)
}

// Per-field regexes. Compiled once at package load (var, not init).
var (
	// card_id / project: alphanumerics, dot, underscore, hyphen. 1..64 runes.
	identRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	// base_branch: adds slash for refs/heads style and longer cap.
	branchRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	// host component: ASCII alphanumerics, dot, hyphen.
	hostRE = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
	// message_id: UUIDs, prefixed ids, etc. 1..128 runes.
	messageIDRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
)

// maxContentBytes is the maximum permitted byte length of a MessagePayload
// Content field. Duplicated here (rather than importing the handler const) so
// this file is self-contained.
const maxContentBytes = 8192

// containsCtlBytes reports whether s contains any of \n, \r, or NUL.
func containsCtlBytes(s string) bool {
	return strings.ContainsAny(s, "\n\r\x00")
}

// validateIdent validates card_id / project: charset regex + no leading '-'.
// containsCtlBytes is redundant with the regex (neither \n, \r, NUL, nor space
// are in the allowed charset) but included explicitly for clarity and for an
// unambiguous error message when those bytes are present.
func validateIdent(field, v string) error {
	if v == "" {
		return &ValidationError{Field: field, Reason: "required"}
	}

	if containsCtlBytes(v) {
		return &ValidationError{Field: field, Reason: "control bytes not allowed"}
	}

	if !identRE.MatchString(v) {
		return &ValidationError{Field: field, Reason: "must match [A-Za-z0-9_.-]{1,64}"}
	}

	if strings.HasPrefix(v, "-") {
		return &ValidationError{Field: field, Reason: "must not start with '-'"}
	}

	return nil
}

// validateBaseBranch validates a git base_branch value. Empty is allowed
// (handlers treat a missing base_branch as "use repo default").
func validateBaseBranch(v string) error {
	if v == "" {
		return nil
	}

	if containsCtlBytes(v) {
		return &ValidationError{Field: "base_branch", Reason: "control bytes not allowed"}
	}

	if !branchRE.MatchString(v) {
		return &ValidationError{Field: "base_branch", Reason: "must match [A-Za-z0-9._/-]{1,200}"}
	}

	if strings.HasPrefix(v, "-") {
		return &ValidationError{Field: "base_branch", Reason: "must not start with '-'"}
	}

	return nil
}

// validateRepoURL parses the repo_url and enforces https/ssh scheme, non-empty
// host with a strict host regex, and rejection of control bytes in the raw
// input (defence against .netrc-style injection).
func validateRepoURL(v string) error {
	if v == "" {
		return &ValidationError{Field: "repo_url", Reason: "required"}
	}

	if containsCtlBytes(v) {
		return &ValidationError{Field: "repo_url", Reason: "control bytes not allowed"}
	}

	u, err := url.Parse(v)
	if err != nil {
		return &ValidationError{Field: "repo_url", Reason: "unparseable"}
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "ssh" {
		return &ValidationError{Field: "repo_url", Reason: "scheme must be https or ssh"}
	}

	if u.Host == "" {
		return &ValidationError{Field: "repo_url", Reason: "host is empty"}
	}

	// u.Hostname() strips port/userinfo so the host regex can be strict.
	host := u.Hostname()
	if host == "" {
		return &ValidationError{Field: "repo_url", Reason: "host is empty"}
	}

	if !hostRE.MatchString(host) {
		return &ValidationError{Field: "repo_url", Reason: "host contains disallowed characters"}
	}

	return nil
}

// validateMCPURL parses the mcp_url and enforces https scheme (http also
// accepted in dev mode), non-empty host, absence of control bytes, and
// exact-host membership in the allowlist.
// An empty allowlist is fail-closed in production — every mcp_url is rejected.
// In dev mode (devMode==true) an empty allowlist is relaxed to allow any host,
// so local / ephemeral MCP endpoints can be used without configuring a fixed
// allowlist. When allowedHosts is non-empty the strict check applies regardless
// of devMode.
func validateMCPURL(v string, allowedHosts []string, devMode bool) error {
	if v == "" {
		return &ValidationError{Field: "mcp_url", Reason: "required"}
	}

	if containsCtlBytes(v) {
		return &ValidationError{Field: "mcp_url", Reason: "control bytes not allowed"}
	}

	u, err := url.Parse(v)
	if err != nil {
		return &ValidationError{Field: "mcp_url", Reason: "unparseable"}
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && (!devMode || scheme != "http") {
		return &ValidationError{Field: "mcp_url", Reason: "scheme must be https"}
	}

	host := u.Hostname()
	if host == "" {
		return &ValidationError{Field: "mcp_url", Reason: "host is empty"}
	}

	if !hostRE.MatchString(host) {
		return &ValidationError{Field: "mcp_url", Reason: "host contains disallowed characters"}
	}

	// Empty allowlist: fail-closed in production; relaxed in dev mode.
	if len(allowedHosts) == 0 {
		if devMode {
			return nil
		}

		return &ValidationError{Field: "mcp_url", Reason: "host not in allowlist"}
	}

	lower := strings.ToLower(host)
	for _, allowed := range allowedHosts {
		if strings.ToLower(allowed) == lower {
			return nil
		}
	}

	return &ValidationError{Field: "mcp_url", Reason: "host not in allowlist"}
}

// validateContent enforces byte-size cap, UTF-8 validity and NUL rejection on
// a MessagePayload Content string. Newlines are deliberately permitted — the
// HITL user-message body is free-form prose.
func validateContent(v string) error {
	if v == "" {
		return &ValidationError{Field: "content", Reason: "required"}
	}

	if len(v) > maxContentBytes {
		return &ValidationError{Field: "content", Reason: "exceeds 8192 bytes"}
	}

	if !utf8.ValidString(v) {
		return &ValidationError{Field: "content", Reason: "not valid UTF-8"}
	}

	if strings.ContainsRune(v, '\x00') {
		return &ValidationError{Field: "content", Reason: "NUL byte not allowed"}
	}

	return nil
}

// validateMessageID allows empty (optional field) but restricts charset and
// length when present.
func validateMessageID(v string) error {
	if v == "" {
		return nil
	}

	if containsCtlBytes(v) {
		return &ValidationError{Field: "message_id", Reason: "control bytes not allowed"}
	}

	if !messageIDRE.MatchString(v) {
		return &ValidationError{Field: "message_id", Reason: "must match [A-Za-z0-9_.-]{1,128}"}
	}

	return nil
}

// ValidatePayload type-switches on the known webhook payload structs and
// validates every field. It returns a *ValidationError on failure so callers
// can test with errors.As, or a nil error on success.
//
// devMode is forwarded to validateMCPURL: when true, an empty allowedMCPHosts
// slice permits any well-formed https URL rather than failing closed. It has no
// effect on non-trigger payloads.
//
// The caller must pass a pointer to or value of one of the supported payload
// types. Unknown types return nil (validation cannot be performed) to avoid
// accidentally blocking extension — this is safe because handlers only ever
// pass the payloads they declared.
func ValidatePayload(p any, allowedMCPHosts []string, devMode bool) error {
	switch v := p.(type) {
	case *TriggerPayload:
		if v == nil {
			return nil
		}

		return validateTrigger(v, allowedMCPHosts, devMode)
	case TriggerPayload:
		return validateTrigger(&v, allowedMCPHosts, devMode)

	case *KillPayload:
		if v == nil {
			return nil
		}

		return validateKill(v)
	case KillPayload:
		return validateKill(&v)

	case *StopAllPayload:
		if v == nil {
			return nil
		}

		return validateStopAll(v)
	case StopAllPayload:
		return validateStopAll(&v)

	case *MessagePayload:
		if v == nil {
			return nil
		}

		return validateMessage(v)
	case MessagePayload:
		return validateMessage(&v)

	case *PromotePayload:
		if v == nil {
			return nil
		}

		return validatePromote(v)
	case PromotePayload:
		return validatePromote(&v)

	case *EndSessionPayload:
		if v == nil {
			return nil
		}

		return validateEndSession(v)
	case EndSessionPayload:
		return validateEndSession(&v)
	}

	return nil
}

func validateTrigger(p *TriggerPayload, allowedMCPHosts []string, devMode bool) error {
	if err := validateIdent("card_id", p.CardID); err != nil {
		return err
	}

	if err := validateIdent("project", p.Project); err != nil {
		return err
	}

	if err := validateRepoURL(p.RepoURL); err != nil {
		return err
	}

	if err := validateMCPURL(p.MCPURL, allowedMCPHosts, devMode); err != nil {
		return err
	}

	if err := validateBaseBranch(p.BaseBranch); err != nil {
		return err
	}

	return nil
}

func validateKill(p *KillPayload) error {
	if err := validateIdent("card_id", p.CardID); err != nil {
		return err
	}

	return validateIdent("project", p.Project)
}

func validateStopAll(p *StopAllPayload) error {
	// project is optional on stop-all; only validate when present.
	if p.Project == "" {
		return nil
	}

	return validateIdent("project", p.Project)
}

func validateMessage(p *MessagePayload) error {
	if err := validateIdent("card_id", p.CardID); err != nil {
		return err
	}

	if err := validateIdent("project", p.Project); err != nil {
		return err
	}

	if err := validateContent(p.Content); err != nil {
		return err
	}

	return validateMessageID(p.MessageID)
}

func validatePromote(p *PromotePayload) error {
	if err := validateIdent("card_id", p.CardID); err != nil {
		return err
	}

	return validateIdent("project", p.Project)
}

func validateEndSession(p *EndSessionPayload) error {
	if err := validateIdent("card_id", p.CardID); err != nil {
		return err
	}

	return validateIdent("project", p.Project)
}
