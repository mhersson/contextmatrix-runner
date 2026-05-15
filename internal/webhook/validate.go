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
	// task_skill_name: restricts skill names to a safe charset that cannot
	// reach outside the /host-skills mount via path traversal. Must start with
	// alphanumeric (no leading dash to avoid argv injection, no leading dot to
	// avoid hidden directories), then alphanumeric / dot / underscore / dash.
	taskSkillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
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
// input (defence against .netrc-style injection). ssh URLs are accepted at
// validation but rewritten to https before the container sees them.
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

// ValidateTaskSkills checks every skill name in the slice against the
// allowlist pattern. Empty slice is valid (means "no skills").
func ValidateTaskSkills(skills []string) error {
	for _, s := range skills {
		if !taskSkillNamePattern.MatchString(s) {
			return fmt.Errorf("invalid task skill name: %q", s)
		}
	}

	return nil
}

// ValidatePayload type-switches on the known webhook payload structs and
// validates every field. It returns a *ValidationError on failure so callers
// can test with errors.As, or a nil error on success.
//
// The caller must pass a pointer to or value of one of the supported payload
// types. Unknown types return nil (validation cannot be performed) to avoid
// accidentally blocking extension — this is safe because handlers only ever
// pass the payloads they declared.
func ValidatePayload(p any) error {
	switch v := p.(type) {
	case *TriggerPayload:
		if v == nil {
			return nil
		}

		return validateTrigger(v)
	case TriggerPayload:
		return validateTrigger(&v)

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

	case *ChatStartPayload:
		if v == nil {
			return nil
		}

		return validateChatStart(v)
	case ChatStartPayload:
		return validateChatStart(&v)

	case *ChatEndPayload:
		if v == nil {
			return nil
		}

		return validateChatEnd(v)
	case ChatEndPayload:
		return validateChatEnd(&v)
	}

	return nil
}

func validateTrigger(p *TriggerPayload) error {
	if err := validateIdent("card_id", p.CardID); err != nil {
		return err
	}

	if err := validateIdent("project", p.Project); err != nil {
		return err
	}

	if err := validateRepoURL(p.RepoURL); err != nil {
		return err
	}

	if err := validateBaseBranch(p.BaseBranch); err != nil {
		return err
	}

	if p.TaskSkills != nil {
		if err := ValidateTaskSkills(*p.TaskSkills); err != nil {
			return err
		}
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
	if p.SessionID != "" {
		// Chat mode: session_id only, card_id/project must be empty.
		if p.CardID != "" || p.Project != "" {
			return &ValidationError{Field: "session_id", Reason: "must not coexist with card_id/project"}
		}

		if err := validateIdent("session_id", p.SessionID); err != nil {
			return err
		}
	} else {
		// Card mode: card_id + project required (existing behaviour).
		if err := validateIdent("card_id", p.CardID); err != nil {
			return err
		}

		if err := validateIdent("project", p.Project); err != nil {
			return err
		}
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

// chatModelPattern is the defense-in-depth allowlist for chat model IDs.
// The authoritative allowlist (label, max_tokens) lives in CM config; this
// pattern is the syntactic guardrail on the runner side so a malformed
// model string never reaches docker.ContainerCreate as an env value.
var chatModelPattern = regexp.MustCompile(`^claude-[a-z0-9.-]{1,64}$`)

// chatResumeRolePattern restricts ResumeTurn.Role to the four shapes the
// transcript builder emits.
var chatResumeRolePattern = regexp.MustCompile(`^(user|assistant_text|tool_call|tool_result_summary)$`)

const (
	// chatResumeMaxTurns caps the resume payload at 600 turns, matching CM's
	// maxMessagesForBuild constant — the maximum number of turns buildResume
	// can ever produce before transcript.Build filters down to fit the
	// 40k-token budget. transcript.Build's token budget bounds total content
	// well below hmacAuth's 1 MiB read window in practice (40k tokens ×
	// ~4 chars/token ≈ 160 KB of text, plus JSON overhead), so a legitimately
	// oversized turn-count is rejected with 400 (invalid_field) rather than
	// 401 (signature mismatch on truncated body).
	chatResumeMaxTurns = 600

	// chatResumeMaxContentBytes is the per-turn content cap. 4 KiB is
	// generous for a single conversation turn. In production, transcript.Build's
	// 40k-token budget makes the per-turn average far smaller; this cap is a
	// defense-in-depth ceiling, not the binding factor for payload size.
	chatResumeMaxContentBytes = 4 * 1024
)

func validateChatStart(p *ChatStartPayload) error {
	if err := validateIdent("session_id", p.SessionID); err != nil {
		return err
	}

	// Project and repo_url are optional on /chat/start (the client may open a
	// session without yet binding a project). Validate only when present.
	if p.Project != "" {
		if err := validateIdent("project", p.Project); err != nil {
			return err
		}
	}

	if p.RepoURL != "" {
		if err := validateRepoURL(p.RepoURL); err != nil {
			return err
		}
	}

	if p.Model != "" {
		if !chatModelPattern.MatchString(p.Model) {
			return &ValidationError{
				Field:  "model",
				Reason: "must match " + chatModelPattern.String(),
			}
		}
	}

	if p.Resume != nil {
		if err := validateChatResume(p.Resume); err != nil {
			return err
		}
	}

	return nil
}

func validateChatResume(r *ChatResumeContext) error {
	if len(r.Turns) > chatResumeMaxTurns {
		return &ValidationError{
			Field:  "resume.turns",
			Reason: fmt.Sprintf("too many turns: %d > %d", len(r.Turns), chatResumeMaxTurns),
		}
	}

	for i, t := range r.Turns {
		if !chatResumeRolePattern.MatchString(t.Role) {
			return &ValidationError{
				Field:  fmt.Sprintf("resume.turns[%d].role", i),
				Reason: "must match " + chatResumeRolePattern.String(),
			}
		}

		if len(t.Content) > chatResumeMaxContentBytes {
			return &ValidationError{
				Field: fmt.Sprintf("resume.turns[%d].content", i),
				Reason: fmt.Sprintf("too long: %d > %d bytes",
					len(t.Content), chatResumeMaxContentBytes),
			}
		}

		if !utf8.ValidString(t.Content) {
			return &ValidationError{
				Field:  fmt.Sprintf("resume.turns[%d].content", i),
				Reason: "must be valid UTF-8",
			}
		}
	}

	return nil
}

func validateChatEnd(p *ChatEndPayload) error {
	return validateIdent("session_id", p.SessionID)
}

// ValidateRefreshKnowledge mirrors ValidatePayload's rules for the refresh
// webhook: project + repo + https repo URL + human-prefixed agent_id.
// overwrite_docs is optional; if present, every entry must be a known KB
// doc filename to prevent the runner-side allowlist from being bypassed.
func ValidateRefreshKnowledge(p *RefreshKnowledgePayload) error {
	if p == nil {
		return fmt.Errorf("payload required")
	}

	if p.Project == "" {
		return fmt.Errorf("project required")
	}

	if p.Repo == "" {
		return fmt.Errorf("repo required")
	}

	if p.RepoURL == "" {
		return fmt.Errorf("repo_url required")
	}

	if !strings.HasPrefix(p.RepoURL, "https://") {
		return fmt.Errorf("repo_url must be https://")
	}

	if !strings.HasPrefix(p.AgentID, "human:") {
		return fmt.Errorf(`agent_id must start with "human:"`)
	}

	for _, d := range p.OverwriteDocs {
		if !isKnownKBDoc(d) {
			return fmt.Errorf("overwrite_docs entry %q is not a known KB doc", d)
		}
	}

	return nil
}

// isKnownKBDoc enumerates the v1 doc set. Update if the spec adds docs.
func isKnownKBDoc(name string) bool {
	switch name {
	case "architecture.md", "code-structure.md",
		"api-documentation.md", "glossary.md":
		return true
	}

	return false
}
