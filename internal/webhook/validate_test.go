package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-runner/internal/container"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// -----------------------------------------------------------------------------
// card_id / project (identRE)
// -----------------------------------------------------------------------------

func TestValidateIdent(t *testing.T) {
	cases := []struct {
		name    string
		field   string
		val     string
		wantErr bool
	}{
		// happy paths
		{"simple alnum", "card_id", "ABC-123", false},
		{"underscores and dots", "card_id", "my.card_id-1", false},
		{"max length 64", "card_id", strings.Repeat("a", 64), false},
		{"project simple", "project", "proj-1", false},

		// rejects
		{"empty", "card_id", "", true},
		{"length 65", "card_id", strings.Repeat("a", 65), true},
		{"leading hyphen", "card_id", "-rm-rf", true},
		{"newline embedded", "card_id", "abc\ndef", true},
		{"carriage return", "card_id", "abc\rdef", true},
		{"NUL byte", "card_id", "abc\x00def", true},
		{"space", "card_id", "abc def", true},
		{"tab", "card_id", "abc\tdef", true},
		{"double quote", "card_id", "abc\"def", true},
		{"semicolon", "card_id", "abc;def", true},
		{"backtick", "card_id", "abc`def", true},
		{"dollar", "card_id", "abc$def", true},
		{"backslash", "card_id", "abc\\def", true},
		{"slash", "card_id", "abc/def", true},
		{"pipe", "card_id", "abc|def", true},
		{"ampersand", "card_id", "abc&def", true},
		{"unicode non-ascii", "card_id", "café", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIdent(tc.field, tc.val)
			if tc.wantErr {
				require.Error(t, err)

				var ve *ValidationError

				require.ErrorAs(t, err, &ve)
				assert.Equal(t, tc.field, ve.Field)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// base_branch
// -----------------------------------------------------------------------------

func TestValidateBaseBranch(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		wantErr bool
	}{
		// happy paths
		{"empty allowed", "", false},
		{"simple main", "main", false},
		{"slash feature", "feature/foo-bar", false},
		{"release/1.2.3 dots and slash", "release/1.2.3", false},
		{"refs/heads style", "refs/heads/main", false},
		{"max length 200", strings.Repeat("a", 200), false},

		// rejects
		{"length 201", strings.Repeat("a", 201), true},
		{"leading hyphen flag", "-delete", true},
		{"newline in name", "main\nrm", true},
		{"carriage return", "main\r", true},
		{"NUL byte", "main\x00", true},
		{"space", "feature branch", true},
		{"double quote", "main\"", true},
		{"semicolon", "main;ls", true},
		{"backtick", "main`x`", true},
		{"backslash", "main\\x", true},
		{"colon", "main:master", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBaseBranch(tc.val)
			if tc.wantErr {
				require.Error(t, err)

				var ve *ValidationError

				require.ErrorAs(t, err, &ve)
				assert.Equal(t, "base_branch", ve.Field)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// repo_url
// -----------------------------------------------------------------------------

func TestValidateRepoURL(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		wantErr bool
	}{
		// happy paths
		{"https", "https://github.com/org/repo.git", false},
		{"ssh without userinfo", "ssh://github.com/org/repo.git", false},
		{"https with port", "https://gitlab.example.com:8443/org/repo.git", false},

		// scheme rejections
		{"empty", "", true},
		{"http scheme", "http://github.com/org/repo.git", true},
		{"file scheme", "file:///etc/passwd", true},
		{"ftp scheme", "ftp://example.com/repo", true},
		{"no scheme", "github.com/org/repo", true},
		// git SCP-style path (no URL scheme — passes url.Parse but scheme is empty)
		{"scp-style git URL", "git@github.com:org/repo.git", true},

		// userinfo rejections (Fix W2): a CM-supplied URL must not embed
		// credentials, otherwise an attacker-controlled CM could leak tokens
		// into the worker's git config.
		{"https with userinfo", "https://user@github.com/org/repo.git", true},
		{"https with userinfo+password", "https://x:token@github.com/org/repo.git", true},
		{"ssh with git@ userinfo", "ssh://git@github.com/org/repo.git", true},

		// control-byte / injection rejections
		{"newline in raw", "https://github.com\n/org", true},
		{"carriage return in raw", "https://github.com\r/org", true},
		{"NUL byte in raw", "https://github.com\x00/org", true},

		// host rejections
		{"host with space encoded", "https://ex ample.com/repo", true},
		{"host with quote", "https://ex\"ample.com/repo", true},
		// empty host
		{"empty host https", "https:///path", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRepoURL(tc.val)
			if tc.wantErr {
				require.Error(t, err, "expected error for %q", tc.val)

				var ve *ValidationError

				require.ErrorAs(t, err, &ve)
				assert.Equal(t, "repo_url", ve.Field)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// content (MessagePayload)
// -----------------------------------------------------------------------------

func TestValidateContent(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		wantErr bool
	}{
		{"simple text", "hello", false},
		{"with newline allowed", "hello\nworld", false},
		{"unicode ok", "café 🚀", false},
		{"at size cap", strings.Repeat("a", MaxMessageContentBytes), false},

		{"empty", "", true},
		{"over size cap", strings.Repeat("a", MaxMessageContentBytes+1), true},
		{"NUL byte rejected", "hello\x00world", true},
		// 0xff alone is not valid UTF-8.
		{"invalid utf8", string([]byte{0xff, 0xfe, 0xfd}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateContent(tc.val)
			if tc.wantErr {
				require.Error(t, err)

				var ve *ValidationError

				require.ErrorAs(t, err, &ve)
				assert.Equal(t, "content", ve.Field)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// message_id
// -----------------------------------------------------------------------------

func TestValidateMessageID(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		wantErr bool
	}{
		{"empty allowed", "", false},
		{"uuid v4", "550e8400-e29b-41d4-a716-446655440000", false},
		{"prefixed", "msg_abc.123", false},
		{"max length 128", strings.Repeat("a", 128), false},

		{"length 129", strings.Repeat("a", 129), true},
		{"newline", "msg\nfoo", true},
		{"NUL byte", "msg\x00foo", true},
		{"space", "msg foo", true},
		{"slash", "msg/foo", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMessageID(tc.val)
			if tc.wantErr {
				require.Error(t, err)

				var ve *ValidationError

				require.ErrorAs(t, err, &ve)
				assert.Equal(t, "message_id", ve.Field)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// ValidatePayload dispatch table
// -----------------------------------------------------------------------------

func TestValidatePayload_TriggerHappy(t *testing.T) {
	p := &TriggerPayload{
		CardID:     "CARD-001",
		Project:    "proj",
		RepoURL:    "https://github.com/org/repo.git",
		BaseBranch: "main",
	}
	require.NoError(t, ValidatePayload(p))
}

func TestValidatePayload_TriggerRejectsEachField(t *testing.T) {
	mk := func() *TriggerPayload {
		return &TriggerPayload{
			CardID:  "CARD-001",
			Project: "proj",
			RepoURL: "https://github.com/org/repo.git",
		}
	}

	cases := []struct {
		name      string
		mutate    func(*TriggerPayload)
		wantField string
	}{
		{"bad card_id", func(p *TriggerPayload) { p.CardID = "a b" }, "card_id"},
		{"bad project", func(p *TriggerPayload) { p.Project = "-evil" }, "project"},
		{"bad repo_url", func(p *TriggerPayload) { p.RepoURL = "http://example.com/" }, "repo_url"},
		{"bad base_branch", func(p *TriggerPayload) { p.BaseBranch = "main\n" }, "base_branch"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := mk()
			tc.mutate(p)

			err := ValidatePayload(p)
			require.Error(t, err)

			var ve *ValidationError

			require.ErrorAs(t, err, &ve)
			assert.Equal(t, tc.wantField, ve.Field)
		})
	}
}

func TestValidatePayload_Kill(t *testing.T) {
	require.NoError(t, ValidatePayload(&KillPayload{CardID: "C-1", Project: "p"}))

	err := ValidatePayload(&KillPayload{CardID: "-evil", Project: "p"})
	require.Error(t, err)

	var ve *ValidationError

	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "card_id", ve.Field)
}

func TestValidatePayload_StopAll(t *testing.T) {
	// Empty project allowed
	require.NoError(t, ValidatePayload(&StopAllPayload{}))

	// Valid project allowed
	require.NoError(t, ValidatePayload(&StopAllPayload{Project: "proj"}))

	// Bad project rejected
	err := ValidatePayload(&StopAllPayload{Project: "a b"})
	require.Error(t, err)
}

func TestValidatePayload_Message(t *testing.T) {
	p := &MessagePayload{
		CardID:    "C-1",
		Project:   "p",
		Content:   "hello",
		MessageID: "msg-1",
	}
	require.NoError(t, ValidatePayload(p))

	// Empty content
	p2 := *p
	p2.Content = ""
	err := ValidatePayload(&p2)
	require.Error(t, err)

	var ve *ValidationError

	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "content", ve.Field)

	// Bad message_id
	p3 := *p
	p3.MessageID = "msg id with space"
	err = ValidatePayload(&p3)
	require.Error(t, err)
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "message_id", ve.Field)
}

func TestValidatePayload_Promote(t *testing.T) {
	require.NoError(t, ValidatePayload(&PromotePayload{CardID: "C-1", Project: "p"}))
	require.Error(t, ValidatePayload(&PromotePayload{CardID: "", Project: "p"}))
}

func TestValidatePayload_EndSession(t *testing.T) {
	require.NoError(t, ValidatePayload(&EndSessionPayload{CardID: "C-1", Project: "p"}))
	require.Error(t, ValidatePayload(&EndSessionPayload{CardID: "C-1", Project: "-bad"}))
}

func TestValidatePayload_ChatStart(t *testing.T) {
	// Happy paths.
	require.NoError(t, ValidatePayload(&ChatStartPayload{SessionID: "sess-1"}))
	require.NoError(t, ValidatePayload(&ChatStartPayload{
		SessionID: "sess-1",
		Project:   "proj",
		RepoURL:   "https://github.com/org/repo.git",
	}))

	cases := []struct {
		name      string
		mutate    func(*ChatStartPayload)
		wantField string
	}{
		{"empty session_id", func(p *ChatStartPayload) { p.SessionID = "" }, "session_id"},
		{"leading dash session_id", func(p *ChatStartPayload) { p.SessionID = "-evil" }, "session_id"},
		{"control bytes session_id", func(p *ChatStartPayload) { p.SessionID = "sess\nfoo" }, "session_id"},
		{"overlong session_id", func(p *ChatStartPayload) { p.SessionID = strings.Repeat("a", 65) }, "session_id"},
		{"bad project", func(p *ChatStartPayload) { p.Project = "a b" }, "project"},
		{"leading dash project", func(p *ChatStartPayload) { p.Project = "-evil" }, "project"},
		{"non-url repo_url", func(p *ChatStartPayload) { p.RepoURL = "not a url" }, "repo_url"},
		{"http scheme repo_url", func(p *ChatStartPayload) { p.RepoURL = "http://example.com/r" }, "repo_url"},
		{"control byte repo_url", func(p *ChatStartPayload) { p.RepoURL = "https://github.com\n/x" }, "repo_url"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &ChatStartPayload{
				SessionID: "sess-1",
				Project:   "proj",
				RepoURL:   "https://github.com/org/repo.git",
			}
			tc.mutate(p)

			err := ValidatePayload(p)
			require.Error(t, err)

			var ve *ValidationError

			require.ErrorAs(t, err, &ve)
			assert.Equal(t, tc.wantField, ve.Field)
		})
	}
}

func TestChatStartPayload_PrimerUnmarshal(t *testing.T) {
	// Field present → populated.
	const withPrimer = `{"session_id":"S1","primer":"ORIENT"}`

	var p1 ChatStartPayload
	require.NoError(t, json.Unmarshal([]byte(withPrimer), &p1))
	assert.Equal(t, "ORIENT", p1.Primer)
	assert.Equal(t, "S1", p1.SessionID)

	// Field absent → empty string (zero value).
	const withoutPrimer = `{"session_id":"S2"}`

	var p2 ChatStartPayload
	require.NoError(t, json.Unmarshal([]byte(withoutPrimer), &p2))
	assert.Empty(t, p2.Primer, "missing primer field must decode to empty string")
}

func TestValidatePayload_ChatStart_Primer(t *testing.T) {
	// Happy paths: empty (back-compat), short, near-cap.
	require.NoError(t, ValidatePayload(&ChatStartPayload{SessionID: "sess-1"}))
	require.NoError(t, ValidatePayload(&ChatStartPayload{SessionID: "sess-1", Primer: "ORIENT"}))
	require.NoError(t, ValidatePayload(&ChatStartPayload{
		SessionID: "sess-1",
		Primer:    strings.Repeat("a", 64*1024),
	}))

	// Over-cap → rejected with field="primer".
	t.Run("reject:over-cap", func(t *testing.T) {
		err := ValidatePayload(&ChatStartPayload{
			SessionID: "sess-1",
			Primer:    strings.Repeat("a", 64*1024+1),
		})
		require.Error(t, err)

		var ve *ValidationError

		require.ErrorAs(t, err, &ve)
		assert.Equal(t, "primer", ve.Field)
		assert.Contains(t, ve.Reason, "too long")
	})

	// Invalid UTF-8 → rejected with field="primer".
	t.Run("reject:invalid-utf8", func(t *testing.T) {
		err := ValidatePayload(&ChatStartPayload{
			SessionID: "sess-1",
			Primer:    "\xff\xfe not utf-8",
		})
		require.Error(t, err)

		var ve *ValidationError

		require.ErrorAs(t, err, &ve)
		assert.Equal(t, "primer", ve.Field)
		assert.Contains(t, ve.Reason, "UTF-8")
	})
}

func TestValidatePayload_ChatStart_Model(t *testing.T) {
	// Happy paths.
	require.NoError(t, ValidatePayload(&ChatStartPayload{SessionID: "sess-1", Model: ""}))
	require.NoError(t, ValidatePayload(&ChatStartPayload{SessionID: "sess-1", Model: "claude-sonnet-4-6"}))
	require.NoError(t, ValidatePayload(&ChatStartPayload{SessionID: "sess-1", Model: "claude-opus-4-7"}))
	require.NoError(t, ValidatePayload(&ChatStartPayload{SessionID: "sess-1", Model: "claude-haiku-4-5-20251001"}))

	rejects := []string{
		"gpt-4",
		"sonnet-4-6",                        // missing claude- prefix
		"claude-",                           // empty suffix
		"claude-EVIL$",                      // invalid char
		"claude-" + strings.Repeat("a", 65), // too long
	}

	for _, m := range rejects {
		t.Run("reject:"+m, func(t *testing.T) {
			err := ValidatePayload(&ChatStartPayload{SessionID: "sess-1", Model: m})
			require.Error(t, err)

			var ve *ValidationError

			require.ErrorAs(t, err, &ve)
			assert.Equal(t, "model", ve.Field)
		})
	}
}

func TestValidatePayload_ChatStart_Resume(t *testing.T) {
	// Happy: small resume.
	require.NoError(t, ValidatePayload(&ChatStartPayload{
		SessionID: "sess-1",
		Resume: &ChatResumeContext{
			Turns: []ChatResumeTurn{
				{Seq: 1, Role: "user", Content: "hi"},
				{Seq: 2, Role: "assistant_text", Content: "hello"},
				{Seq: 3, Role: "tool_call", Content: "Bash: ls"},
				{Seq: 4, Role: "tool_result_summary", Content: "→ ok"},
			},
		},
	}))

	// Too many turns.
	manyTurns := make([]ChatResumeTurn, chatResumeMaxTurns+1)
	for i := range manyTurns {
		manyTurns[i] = ChatResumeTurn{Seq: int64(i + 1), Role: "user", Content: "x"}
	}

	err := ValidatePayload(&ChatStartPayload{
		SessionID: "sess-1",
		Resume:    &ChatResumeContext{Turns: manyTurns},
	})
	require.Error(t, err)

	var ve *ValidationError

	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "resume.turns", ve.Field)

	// Bad role.
	err = ValidatePayload(&ChatStartPayload{
		SessionID: "sess-1",
		Resume: &ChatResumeContext{Turns: []ChatResumeTurn{
			{Seq: 1, Role: "stderr", Content: "noise"},
		}},
	})
	require.Error(t, err)
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "resume.turns[0].role", ve.Field)

	// Oversized content.
	err = ValidatePayload(&ChatStartPayload{
		SessionID: "sess-1",
		Resume: &ChatResumeContext{Turns: []ChatResumeTurn{
			{Seq: 1, Role: "user", Content: strings.Repeat("x", chatResumeMaxContentBytes+1)},
		}},
	})
	require.Error(t, err)
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "resume.turns[0].content", ve.Field)
}

func TestValidatePayload_ChatEnd(t *testing.T) {
	require.NoError(t, ValidatePayload(&ChatEndPayload{SessionID: "sess-1"}))
	require.Error(t, ValidatePayload(&ChatEndPayload{SessionID: ""}))
	require.Error(t, ValidatePayload(&ChatEndPayload{SessionID: "-evil"}))
	require.Error(t, ValidatePayload(&ChatEndPayload{SessionID: "sess\x00null"}))
}

func TestValidatePayload_ByValue(t *testing.T) {
	// Pass-by-value should work as well as pass-by-pointer.
	require.NoError(t, ValidatePayload(KillPayload{CardID: "C-1", Project: "p"}))
	require.NoError(t, ValidatePayload(StopAllPayload{}))
	require.NoError(t, ValidatePayload(PromotePayload{CardID: "C-1", Project: "p"}))
	require.NoError(t, ValidatePayload(EndSessionPayload{CardID: "C-1", Project: "p"}))
	require.NoError(t, ValidatePayload(
		MessagePayload{CardID: "C-1", Project: "p", Content: "hi"},
	))
	require.NoError(t, ValidatePayload(ChatStartPayload{SessionID: "sess-1"}))
	require.NoError(t, ValidatePayload(ChatEndPayload{SessionID: "sess-1"}))
}

// TestValidatePayload_UnknownTypeRejected verifies the W8 fix: unknown
// payload types now fail loudly with a clear error rather than silently
// returning nil. A handler that registers a new struct without wiring its
// dispatch branch in ValidatePayload must surface the mistake immediately
// rather than bypassing the validation gate.
func TestValidatePayload_UnknownTypeRejected(t *testing.T) {
	err := ValidatePayload(struct{ X int }{X: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown payload type")

	// nil is also unknown (no concrete type) so it fails the same way.
	err = ValidatePayload(nil)
	require.Error(t, err)
}

// -----------------------------------------------------------------------------
// task_skills
// -----------------------------------------------------------------------------

func TestValidateTaskSkills(t *testing.T) {
	cases := []struct {
		name    string
		input   []string
		wantErr bool
	}{
		{"empty list ok", []string{}, false},
		{"single valid", []string{"go-development"}, false},
		{"multiple valid", []string{"go-development", "typescript-react", "code-review"}, false},
		{"alphanumeric ok", []string{"abc123"}, false},
		{"underscore ok", []string{"go_development"}, false},
		{"dot ok", []string{"v1.0"}, false},
		{"empty string rejected", []string{""}, true},
		{"leading dash rejected", []string{"-go-development"}, true},
		{"leading dot rejected", []string{".secret"}, true},
		{"slash rejected (path traversal)", []string{"a/b"}, true},
		{"dotdot rejected", []string{".."}, true},
		{"space rejected", []string{"go development"}, true},
		{"uppercase rejected", []string{"Go-Development"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateTaskSkills(c.input)
			if c.wantErr {
				assert.Error(t, err, "input %v", c.input)
			} else {
				assert.NoError(t, err, "input %v", c.input)
			}
		})
	}
}

func TestValidationError_Message(t *testing.T) {
	e := &ValidationError{Field: "card_id", Reason: "required"}
	assert.Equal(t, "invalid card_id: required", e.Error())
}

// -----------------------------------------------------------------------------
// Handler-level integration: invalid card_id in /trigger returns 400 and does
// NOT touch the tracker or container runner.
// -----------------------------------------------------------------------------

// strictRunner fails the test if Run or Kill is called — the handler must
// reject an invalid payload before dispatching to the manager.
type strictRunner struct{ t *testing.T }

func (r *strictRunner) Run(_ context.Context, _ container.RunConfig) {
	r.t.Fatalf("manager.Run must not be called on invalid payload")
}

func (r *strictRunner) Kill(_, _ string) error {
	r.t.Fatalf("manager.Kill must not be called on invalid payload")

	return nil
}

func (r *strictRunner) ListManaged(_ context.Context) ([]container.ManagedContainer, error) {
	r.t.Fatalf("manager.ListManaged must not be called on invalid payload")

	return nil, nil
}

func (r *strictRunner) ForceRemoveByLabels(_ context.Context, _, _ string) (int, error) {
	r.t.Fatalf("manager.ForceRemoveByLabels must not be called on invalid payload")

	return 0, nil
}

func (r *strictRunner) StartChat(_ context.Context, _ container.StartChatOpts) (string, error) {
	r.t.Fatalf("manager.StartChat must not be called on invalid payload")

	return "", nil
}

func (r *strictRunner) Stop(_ context.Context, _ string) error {
	r.t.Fatalf("manager.Stop must not be called on invalid payload")

	return nil
}

func (r *strictRunner) WorkerImage() string { return "" }

func (r *strictRunner) BuildChatAuthEnv(_ context.Context) string { return "" }

func (r *strictRunner) AttachChatStdin(_ context.Context, _, _ string) error { return nil }

func (r *strictRunner) StreamChatLogs(_ context.Context, _, _, _ string) {}

func (r *strictRunner) WaitAndCleanupChat(_, _, _ string) {
	r.t.Fatalf("manager.WaitAndCleanupChat must not be called on invalid payload")
}

func (r *strictRunner) DeleteChatCleanup(_ string) {
	r.t.Fatalf("manager.DeleteChatCleanup must not be called on invalid payload")
}

func (r *strictRunner) KillChat(_ context.Context, _ string) error {
	r.t.Fatalf("manager.KillChat must not be called on invalid payload")

	return nil
}

// TestValidateTaskSkills_ReturnsValidationError ensures rejections in the
// task_skills validator surface as a *ValidationError so the standard
// writeValidationError helper produces "invalid task_skills" rather than
// degrading to "invalid payload".
func TestValidateTaskSkills_ReturnsValidationError(t *testing.T) {
	err := ValidateTaskSkills([]string{"GoodOne", "BadOne"})
	require.Error(t, err)

	var ve *ValidationError

	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "task_skills", ve.Field)
}

// TestValidateTaskSkills_LengthCap verifies the W10 fix: an attacker
// shipping a million skill entries that all individually pass the charset
// check is rejected by the slice-length cap.
func TestValidateTaskSkills_LengthCap(t *testing.T) {
	// At the cap = ok.
	good := make([]string, maxTaskSkills)
	for i := range good {
		good[i] = "skill-x"
	}

	require.NoError(t, ValidateTaskSkills(good))

	// One over the cap = rejected with task_skills field.
	tooMany := make([]string, maxTaskSkills+1)
	for i := range tooMany {
		tooMany[i] = "skill-x"
	}

	err := ValidateTaskSkills(tooMany)
	require.Error(t, err)

	var ve *ValidationError

	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "task_skills", ve.Field)
	assert.Contains(t, ve.Reason, "too many entries")
}

// TestValidatePayload_TriggerModel verifies the W1 fix: TriggerPayload.Model
// must be validated against chatModelPattern when non-empty, and pass through
// when empty.
func TestValidatePayload_TriggerModel(t *testing.T) {
	base := func() *TriggerPayload {
		return &TriggerPayload{
			CardID:  "C-1",
			Project: "p",
			RepoURL: "https://github.com/org/repo.git",
		}
	}

	// Empty model is allowed (optional field).
	require.NoError(t, ValidatePayload(base()))

	// Allowlisted models accepted.
	for _, m := range []string{"claude-sonnet-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-haiku-4-5-20251001"} {
		p := base()
		p.Model = m
		require.NoError(t, ValidatePayload(p), "model %q must be accepted", m)
	}

	// Disallowed shapes rejected with field="model".
	for _, m := range []string{"gpt-4", "sonnet-4-6", "claude-", "claude-EVIL$"} {
		p := base()
		p.Model = m
		err := ValidatePayload(p)
		require.Error(t, err, "model %q must be rejected", m)

		var ve *ValidationError

		require.ErrorAs(t, err, &ve)
		assert.Equal(t, "model", ve.Field)
	}
}

// TestValidateMCPAPIKey verifies the W6 fix: the MCP API key (which is
// forwarded into the worker env unchanged) must be validated for length,
// UTF-8, and control bytes when non-empty.
func TestValidateMCPAPIKey(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		wantErr bool
	}{
		{"empty allowed", "", false},
		{"simple key", "secret-key", false},
		{"max length", strings.Repeat("a", maxMCPAPIKeyBytes), false},
		{"over length", strings.Repeat("a", maxMCPAPIKeyBytes+1), true},
		{"invalid utf8", string([]byte{0xff, 0xfe, 0xfd}), true},
		{"newline", "key\nfoo", true},
		{"carriage return", "key\rfoo", true},
		{"NUL byte", "key\x00foo", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMCPAPIKey(tc.val)
			if tc.wantErr {
				require.Error(t, err)

				var ve *ValidationError

				require.ErrorAs(t, err, &ve)
				assert.Equal(t, "mcp_api_key", ve.Field)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestValidatePayload_TriggerMCPAPIKey verifies the W6 fix at the dispatch
// level: an invalid MCPAPIKey on /trigger surfaces as field="mcp_api_key".
func TestValidatePayload_TriggerMCPAPIKey(t *testing.T) {
	p := &TriggerPayload{
		CardID:    "C-1",
		Project:   "p",
		RepoURL:   "https://github.com/org/repo.git",
		MCPAPIKey: "key\x00null",
	}
	err := ValidatePayload(p)
	require.Error(t, err)

	var ve *ValidationError

	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "mcp_api_key", ve.Field)
}

// TestValidatePayload_ChatStartMCPAPIKey verifies the W6 fix at the chat
// dispatch level.
func TestValidatePayload_ChatStartMCPAPIKey(t *testing.T) {
	p := &ChatStartPayload{
		SessionID: "sess-1",
		MCPAPIKey: "key\nwithnewline",
	}
	err := ValidatePayload(p)
	require.Error(t, err)

	var ve *ValidationError

	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "mcp_api_key", ve.Field)
}

// TestValidatePayload_ChatStartPrimer_NUL verifies the W9 fix on Primer:
// a NUL byte in the orientation text is rejected.
func TestValidatePayload_ChatStartPrimer_NUL(t *testing.T) {
	err := ValidatePayload(&ChatStartPayload{
		SessionID: "sess-1",
		Primer:    "ORIENT\x00null",
	})
	require.Error(t, err)

	var ve *ValidationError

	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "primer", ve.Field)
	assert.Contains(t, ve.Reason, "NUL")
}

// TestValidatePayload_ChatStartResumeContent_NUL verifies the W9 fix on
// resume turn content.
func TestValidatePayload_ChatStartResumeContent_NUL(t *testing.T) {
	err := ValidatePayload(&ChatStartPayload{
		SessionID: "sess-1",
		Resume: &ChatResumeContext{
			Turns: []ChatResumeTurn{
				{Seq: 1, Role: "user", Content: "hi\x00null"},
			},
		},
	})
	require.Error(t, err)

	var ve *ValidationError

	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "resume.turns[0].content", ve.Field)
	assert.Contains(t, ve.Reason, "NUL")
}

func TestHandleTrigger_InvalidCardID_NoTrackerOrRun(t *testing.T) {
	tr := tracker.New()
	// maxConcurrent=3 so concurrency limit never fires; tracker must remain empty.
	h := NewHandler(&strictRunner{t: t}, tr, nil, nil, testAPIKey, 3, testMCPURL, nil, 0, nil)

	badPayload := TriggerPayload{
		CardID:  "-rm -rf",
		Project: "proj",
		RepoURL: "https://github.com/org/repo.git",
	}

	body, err := json.Marshal(badPayload)
	require.NoError(t, err)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := protocol.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/trigger", body, ts)

	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodPost, "/trigger", strings.NewReader(string(body)),
	)
	req.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	req.Header.Set(protocol.TimestampHeader, ts)

	w := httptest.NewRecorder()
	h.hmacAuth(h.handleTrigger)(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, 0, tr.Count(), "invalid payload must not land in tracker")

	var resp ErrorResponse

	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.OK)
	assert.Equal(t, CodeInvalidField, resp.Code)
	// Message should identify the rejected field but NOT echo the raw value.
	assert.Contains(t, resp.Message, "card_id")
	assert.NotContains(t, resp.Message, "-rm -rf")
}
