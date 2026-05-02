package claudeclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractPlanJSONFromBody(t *testing.T) {
	t.Run("fenced json block in plan section", func(t *testing.T) {
		body := "# Foo\n\nPreamble.\n\n## Plan\n\nTwo-sentence summary.\n\n### Subtasks\n\n1. **First** ...\n\n```json\n" + `{"plan_summary":"sum","chosen_repos":["r"],"subtasks":[]}` + "\n```\n"

		got, err := ExtractPlanJSON(body)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.JSONEq(t, `{"plan_summary":"sum","chosen_repos":["r"],"subtasks":[]}`, string(got))
	})

	t.Run("no plan section returns nil no error", func(t *testing.T) {
		body := "# Foo\n\nNo plan section here.\n"

		got, err := ExtractPlanJSON(body)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("plan section without fenced block returns nil no error", func(t *testing.T) {
		body := "## Plan\n\nJust prose, no json block.\n"

		got, err := ExtractPlanJSON(body)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("fenced block accepts ``` without language hint", func(t *testing.T) {
		body := "## Plan\n\n```\n" + `{"plan_summary":"sum"}` + "\n```\n"

		got, err := ExtractPlanJSON(body)
		require.NoError(t, err)
		require.JSONEq(t, `{"plan_summary":"sum"}`, string(got))
	})

	t.Run("multiple fenced blocks - last one in section wins", func(t *testing.T) {
		body := "## Plan\n\n```json\n" + `{"plan_summary":"old"}` + "\n```\n\nrevised:\n\n```json\n" + `{"plan_summary":"new"}` + "\n```\n"

		got, err := ExtractPlanJSON(body)
		require.NoError(t, err)
		require.JSONEq(t, `{"plan_summary":"new"}`, string(got))
	})

	t.Run("fenced block outside plan section ignored", func(t *testing.T) {
		body := "# Foo\n\n```json\n" + `{"unrelated":true}` + "\n```\n\n## Other\n\nstuff\n"

		got, err := ExtractPlanJSON(body)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("fenced block stops at next h2", func(t *testing.T) {
		body := "## Plan\n\n```json\n" + `{"plan_summary":"plan"}` + "\n```\n\n## Notes\n\n```json\n" + `{"plan_summary":"notes"}` + "\n```\n"

		got, err := ExtractPlanJSON(body)
		require.NoError(t, err)
		require.JSONEq(t, `{"plan_summary":"plan"}`, string(got))
	})

	t.Run("malformed json returns error with raw", func(t *testing.T) {
		body := "## Plan\n\n```json\n{not valid json\n```\n"

		_, err := ExtractPlanJSON(body)
		require.Error(t, err)
	})

	t.Run("empty body", func(t *testing.T) {
		got, err := ExtractPlanJSON("")
		require.NoError(t, err)
		require.Nil(t, got)
	})
}
