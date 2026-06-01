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

// MaxMessageContentBytes is the maximum permitted byte length of a
// MessagePayload Content field. Exported so the /message handler can apply
// the same cap before structural validation runs, ensuring the wire-level
// 413 response matches the validator-level 400 boundary.
const MaxMessageContentBytes = 8192

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

	// Reject embedded userinfo (e.g. https://user:token@host/r). The full URL
	// flows downstream into CM_REPO_URL and is git-cloned inside the worker;
	// allowing userinfo would let an attacker-controlled CM inject credentials
	// into the worker's git config and bypass the App-installation-token
	// credential helper.
	if u.User != nil {
		return &ValidationError{Field: "repo_url", Reason: "must not contain userinfo"}
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

	if len(v) > MaxMessageContentBytes {
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

// maxMCPAPIKeyBytes caps the MCP API key length. The key is forwarded
// verbatim into the container env (CM_MCP_API_KEY); we cap the byte length
// to keep oversize inputs from bloating Docker create payloads and reject
// control bytes / invalid UTF-8 so a malformed value cannot smuggle in
// newlines or NUL bytes via the Authorization header.
const maxMCPAPIKeyBytes = 1024

// validateMCPAPIKey is the shared check used by /trigger, /chat/start and
// /refresh-knowledge. Empty is allowed — CM may run without MCP auth in
// loopback dev mode.
func validateMCPAPIKey(v string) error {
	if v == "" {
		return nil
	}

	if len(v) > maxMCPAPIKeyBytes {
		return &ValidationError{
			Field:  "mcp_api_key",
			Reason: fmt.Sprintf("exceeds %d bytes", maxMCPAPIKeyBytes),
		}
	}

	if !utf8.ValidString(v) {
		return &ValidationError{Field: "mcp_api_key", Reason: "not valid UTF-8"}
	}

	if containsCtlBytes(v) {
		return &ValidationError{Field: "mcp_api_key", Reason: "control bytes not allowed"}
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

// maxTaskSkills caps the number of skill names per /trigger payload. Real
// payloads carry a handful of entries; the cap is defence-in-depth against
// an attacker-controlled CM shipping a million-entry slice that would still
// pass the per-entry charset check.
const maxTaskSkills = 64

// ValidateTaskSkills checks every skill name in the slice against the
// allowlist pattern. Empty slice is valid (means "no skills"). Rejections
// are returned as *ValidationError so writeValidationError surfaces the
// "invalid task_skills" wire shape — a raw fmt.Errorf would degrade to
// "invalid payload".
func ValidateTaskSkills(skills []string) error {
	if len(skills) > maxTaskSkills {
		return &ValidationError{
			Field:  "task_skills",
			Reason: fmt.Sprintf("too many entries: %d > %d", len(skills), maxTaskSkills),
		}
	}

	for _, s := range skills {
		if !taskSkillNamePattern.MatchString(s) {
			return &ValidationError{Field: "task_skills", Reason: "name must match " + taskSkillNamePattern.String()}
		}
	}

	return nil
}

// ValidatePayload type-switches on the known webhook payload structs and
// validates every field. It returns a *ValidationError on failure so callers
// can test with errors.As, or a nil error on success.
//
// The caller must pass a pointer to or value of one of the supported payload
// types. Unknown types now return a non-nil error rather than silently
// passing — a registration mistake (a new payload struct decoded by a
// handler but never added to the dispatch below) fails the request rather
// than bypassing the validation gate. Fix W8 in REVIEW.md.
//
// New payload types MUST be added to this type switch even if they have a
// dedicated Validate<Foo> helper. Calling the helper directly from the
// handler bypasses ValidatePayload, which is the single entry point for the
// payload-validation surface — bypassing it splits the dispatch into two
// inconsistent code paths and is the class of mistake that hid a missing
// validator on /refresh-knowledge for one release cycle (Fix W2 in REVIEW.md).
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

	case *RefreshKnowledgePayload:
		if v == nil {
			return nil
		}

		return ValidateRefreshKnowledge(v)
	case RefreshKnowledgePayload:
		return ValidateRefreshKnowledge(&v)

	default:
		// Loud failure on unknown payload types so a registration mistake
		// (handler decodes a new struct but forgets to wire its dispatch
		// branch above) fails the request rather than silently bypassing
		// the validation gate. Fix W8 in REVIEW.md.
		return fmt.Errorf("unknown payload type %T", p)
	}
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

	// Model is optional; when present, must match the chat-model allowlist
	// pattern. Value flows into `claude --model "${CM_ORCHESTRATOR_MODEL}"` in
	// docker/entrypoint.sh; entrypoint quoting blocks argv-injection but
	// validation here is the primary defence and keeps the surface in lockstep
	// with /chat/start.
	if p.Model != "" && !chatModelPattern.MatchString(p.Model) {
		return &ValidationError{
			Field:  "model",
			Reason: "must match " + chatModelPattern.String(),
		}
	}

	if err := validateMCPAPIKey(p.MCPAPIKey); err != nil {
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

// chatResumeRolePattern restricts ResumeTurn.Role to the shapes the
// transcript builder emits. `user_question` is retained for backward
// compatibility with transcripts persisted before AskUserQuestion was
// suppressed — the current runner no longer produces that role, but old
// sessions may still carry it across rehydration.
var chatResumeRolePattern = regexp.MustCompile(`^(user|assistant_text|tool_call|tool_result_summary|user_question)$`)

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

	// chatPrimerMaxBytes caps the chat-mode orientation primer text. The
	// current shipped primer (CM's workflow-skills/chat-mode.md) is ~4 KiB;
	// 64 KiB leaves ample room for operator edits while bounding the stdin
	// write well below hmacAuth's 1 MiB body cap. Hitting this cap returns
	// 400 so CM gets a clear "primer too large" signal rather than a 401
	// from a truncated body.
	chatPrimerMaxBytes = 64 * 1024
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

	if err := validateMCPAPIKey(p.MCPAPIKey); err != nil {
		return err
	}

	if p.Resume != nil {
		if err := validateChatResume(p.Resume); err != nil {
			return err
		}
	}

	if p.Primer != "" {
		if len(p.Primer) > chatPrimerMaxBytes {
			return &ValidationError{
				Field: "primer",
				Reason: fmt.Sprintf("too long: %d > %d bytes",
					len(p.Primer), chatPrimerMaxBytes),
			}
		}

		if !utf8.ValidString(p.Primer) {
			return &ValidationError{
				Field:  "primer",
				Reason: "must be valid UTF-8",
			}
		}

		if strings.ContainsRune(p.Primer, '\x00') {
			return &ValidationError{
				Field:  "primer",
				Reason: "NUL byte not allowed",
			}
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

		// Mirror validateContent: free-form transcript text may contain
		// newlines but NUL bytes would smuggle through any downstream
		// C-style consumer (libgit2, exec arg lists). Fix W9 in REVIEW.md.
		if strings.ContainsRune(t.Content, '\x00') {
			return &ValidationError{
				Field:  fmt.Sprintf("resume.turns[%d].content", i),
				Reason: "NUL byte not allowed",
			}
		}
	}

	return nil
}

func validateChatEnd(p *ChatEndPayload) error {
	return validateIdent("session_id", p.SessionID)
}

// agentIDSuffixRE bounds the suffix after the "human:" prefix on agent_id so
// downstream env injection cannot pass arbitrary control bytes or argv flags.
// Same shape as identRE; documented separately for clarity at the call site.
var agentIDSuffixRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ValidateRefreshKnowledge mirrors ValidatePayload's rules for the refresh
// webhook: project + repo + https repo URL + human-prefixed agent_id.
// overwrite_docs is optional; if present, every entry must be a known KB
// doc filename to prevent the runner-side allowlist from being bypassed.
//
// All rejections are returned as *ValidationError so writeValidationError can
// surface the field-name diagnostic ("invalid project", etc.). A raw
// fmt.Errorf would degrade to "invalid payload" through the type assertion.
func ValidateRefreshKnowledge(p *RefreshKnowledgePayload) error {
	if p == nil {
		return &ValidationError{Field: "payload", Reason: "required"}
	}

	if err := validateIdent("project", p.Project); err != nil {
		return err
	}

	// Repo follows the same shape as project/card_id: alphanumeric plus
	// '.', '_', '-'. It flows through to CM_KB_REPO and into the synthetic
	// tracker key "kb-refresh:<repo>", so a charset escape would leak into
	// container env and tracker keys.
	if err := validateIdent("repo", p.Repo); err != nil {
		return err
	}

	if p.RepoURL == "" {
		return &ValidationError{Field: "repo_url", Reason: "required"}
	}

	if !strings.HasPrefix(p.RepoURL, "https://") {
		return &ValidationError{Field: "repo_url", Reason: "must be https://"}
	}

	if err := validateRepoURL(p.RepoURL); err != nil {
		return err
	}

	if err := validateBaseBranch(p.BaseBranch); err != nil {
		return err
	}

	if !strings.HasPrefix(p.AgentID, "human:") {
		return &ValidationError{Field: "agent_id", Reason: `must start with "human:"`}
	}

	// Tighten past the prefix: reject control bytes and limit charset on the
	// suffix so the full agent_id is safe as a container env value.
	suffix := strings.TrimPrefix(p.AgentID, "human:")
	if suffix == "" {
		return &ValidationError{Field: "agent_id", Reason: "suffix required after human:"}
	}

	if containsCtlBytes(suffix) {
		return &ValidationError{Field: "agent_id", Reason: "control bytes not allowed"}
	}

	if !agentIDSuffixRE.MatchString(suffix) {
		return &ValidationError{Field: "agent_id", Reason: "suffix must match " + agentIDSuffixRE.String()}
	}

	for _, d := range p.OverwriteDocs {
		if !isKnownKBDoc(d) {
			return &ValidationError{Field: "overwrite_docs", Reason: "entry is not a known KB doc"}
		}
	}

	// Model is optional; when present, must match the chat-model allowlist
	// pattern. Value flows into `claude --model "${CM_ORCHESTRATOR_MODEL}"`
	// inside the worker container. Keep the validator in lockstep with the
	// /trigger and /chat/start surface.
	if p.Model != "" && !chatModelPattern.MatchString(p.Model) {
		return &ValidationError{
			Field:  "model",
			Reason: "must match " + chatModelPattern.String(),
		}
	}

	if err := validateMCPAPIKey(p.MCPAPIKey); err != nil {
		return err
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
