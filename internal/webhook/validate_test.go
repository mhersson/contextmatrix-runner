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

	"github.com/mhersson/contextmatrix-runner/internal/container"
	cmhmac "github.com/mhersson/contextmatrix-runner/internal/hmac"
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
		{"https with userinfo", "https://user@github.com/org/repo.git", false},
		{"ssh", "ssh://git@github.com/org/repo.git", false},
		{"https with port", "https://gitlab.example.com:8443/org/repo.git", false},

		// scheme rejections
		{"empty", "", true},
		{"http scheme", "http://github.com/org/repo.git", true},
		{"file scheme", "file:///etc/passwd", true},
		{"ftp scheme", "ftp://example.com/repo", true},
		{"no scheme", "github.com/org/repo", true},
		// git SCP-style path (no URL scheme — passes url.Parse but scheme is empty)
		{"scp-style git URL", "git@github.com:org/repo.git", true},

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
		{"at size cap", strings.Repeat("a", maxContentBytes), false},

		{"empty", "", true},
		{"over size cap", strings.Repeat("a", maxContentBytes+1), true},
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

func TestValidatePayload_ByValue(t *testing.T) {
	// Pass-by-value should work as well as pass-by-pointer.
	require.NoError(t, ValidatePayload(KillPayload{CardID: "C-1", Project: "p"}))
	require.NoError(t, ValidatePayload(StopAllPayload{}))
	require.NoError(t, ValidatePayload(PromotePayload{CardID: "C-1", Project: "p"}))
	require.NoError(t, ValidatePayload(EndSessionPayload{CardID: "C-1", Project: "p"}))
	require.NoError(t, ValidatePayload(
		MessagePayload{CardID: "C-1", Project: "p", Content: "hi"},
	))
}

func TestValidatePayload_UnknownTypeNoop(t *testing.T) {
	// Unknown payload type returns nil (handlers only pass known types).
	require.NoError(t, ValidatePayload(struct{ X int }{X: 1}))
	require.NoError(t, ValidatePayload(nil))
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
	sig := cmhmac.SignPayloadWithTimestamp(testAPIKey, body, ts)

	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodPost, "/trigger", strings.NewReader(string(body)),
	)
	req.Header.Set(cmhmac.SignatureHeader, "sha256="+sig)
	req.Header.Set(cmhmac.TimestampHeader, ts)

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
