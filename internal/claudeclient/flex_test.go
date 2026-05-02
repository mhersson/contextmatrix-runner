package claudeclient

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// FlexibleStringSlice is the runner's defence against LLMs that ignore
// the JSON-array shape declared in MCP tool schemas and emit a bare
// string instead. Real Opus has been observed sending
// `chosen_repos: "single-repo"` despite the schema declaring []string;
// strict json.Unmarshal then crashes the entire phase. The flexible
// type accepts both shapes.
func TestFlexibleStringSliceUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"array of strings", `["a","b","c"]`, []string{"a", "b", "c"}},
		{"empty array", `[]`, nil},
		{"bare string becomes single-element list", `"only-repo"`, []string{"only-repo"}},
		{"empty bare string becomes nil", `""`, nil},
		{"null becomes nil", `null`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got FlexibleStringSlice

			require.NoError(t, json.Unmarshal([]byte(tt.in), &got))

			if tt.want == nil {
				require.Empty(t, got)
			} else {
				require.Equal(t, tt.want, []string(got))
			}
		})
	}
}

func TestFlexibleStringSliceUnmarshalRejectsWrongTypes(t *testing.T) {
	cases := []string{
		`123`,       // number
		`true`,      // bool
		`{"a":1}`,   // object
		`[1, 2, 3]`, // array of non-strings
		`["a", 2]`,  // mixed array
		`not json at all`,
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			var got FlexibleStringSlice
			require.Error(t, json.Unmarshal([]byte(c), &got))
		})
	}
}

func TestFlexibleStringSliceFieldInStruct(t *testing.T) {
	type wrap struct {
		Repos FlexibleStringSlice `json:"repos"`
	}

	t.Run("absent field stays nil", func(t *testing.T) {
		var w wrap
		require.NoError(t, json.Unmarshal([]byte(`{}`), &w))
		require.Empty(t, w.Repos)
	})

	t.Run("string-as-array tolerated", func(t *testing.T) {
		var w wrap
		require.NoError(t, json.Unmarshal([]byte(`{"repos":"single"}`), &w))
		require.Equal(t, []string{"single"}, []string(w.Repos))
	})

	t.Run("real array preserved", func(t *testing.T) {
		var w wrap
		require.NoError(t, json.Unmarshal([]byte(`{"repos":["a","b"]}`), &w))
		require.Equal(t, []string{"a", "b"}, []string(w.Repos))
	})
}

// FlexibleSubtaskList is the second drift mode the runner needs to
// tolerate: real Opus has emitted `subtasks` as a bare string instead
// of the array of objects declared by the schema. Most often this
// string is a JSON-encoded array (Opus collapsing complex types into
// quoted JSON), so we unwrap and re-parse before giving up.
func TestFlexibleSubtaskListUnmarshal(t *testing.T) {
	t.Run("array of objects parses normally", func(t *testing.T) {
		raw := `[
		  {"title": "First", "description": "Do thing", "repos": ["a"], "priority": "high", "depends_on": []},
		  {"title": "Second", "description": "Do other thing", "repos": ["b"], "priority": "medium", "depends_on": ["X-1"]}
		]`

		var got FlexibleSubtaskList
		require.NoError(t, json.Unmarshal([]byte(raw), &got))
		require.Len(t, got, 2)
		require.Equal(t, "First", got[0].Title)
		require.Equal(t, []string{"a"}, got[0].Repos)
		require.Equal(t, []string{"X-1"}, got[1].DependsOn)
	})

	t.Run("empty array becomes nil", func(t *testing.T) {
		var got FlexibleSubtaskList
		require.NoError(t, json.Unmarshal([]byte(`[]`), &got))
		require.Empty(t, got)
	})

	t.Run("null becomes nil", func(t *testing.T) {
		var got FlexibleSubtaskList
		require.NoError(t, json.Unmarshal([]byte(`null`), &got))
		require.Empty(t, got)
	})

	t.Run("string-encoded JSON array unwraps and parses", func(t *testing.T) {
		// What Opus has been observed doing: stringifying the entire
		// array. We unwrap once and re-parse.
		inner := `[{"title":"First","repos":["a"],"priority":"high","depends_on":[]}]`
		raw, err := json.Marshal(inner)
		require.NoError(t, err)

		var got FlexibleSubtaskList
		require.NoError(t, json.Unmarshal(raw, &got))
		require.Len(t, got, 1)
		require.Equal(t, "First", got[0].Title)
		require.Equal(t, []string{"a"}, got[0].Repos)
	})

	t.Run("empty string becomes nil", func(t *testing.T) {
		var got FlexibleSubtaskList
		require.NoError(t, json.Unmarshal([]byte(`""`), &got))
		require.Empty(t, got)
	})

	t.Run("string with non-array content errors with raw included", func(t *testing.T) {
		var got FlexibleSubtaskList

		err := json.Unmarshal([]byte(`"1. First subtask, 2. Second"`), &got)
		require.Error(t, err)
		require.Contains(t, err.Error(), "1. First subtask")
	})

	t.Run("non-array non-string errors", func(t *testing.T) {
		var got FlexibleSubtaskList
		require.Error(t, json.Unmarshal([]byte(`123`), &got))
	})

	t.Run("string-as-array with subtask field drift still tolerated", func(t *testing.T) {
		// Combined drift: outer is string, inner subtask has string repos.
		inner := `[{"title":"First","repos":"only-repo","priority":"high","depends_on":"X-1"}]`
		raw, err := json.Marshal(inner)
		require.NoError(t, err)

		var got FlexibleSubtaskList
		require.NoError(t, json.Unmarshal(raw, &got))
		require.Len(t, got, 1)
		require.Equal(t, []string{"only-repo"}, got[0].Repos)
		require.Equal(t, []string{"X-1"}, got[0].DependsOn)
	})
}
