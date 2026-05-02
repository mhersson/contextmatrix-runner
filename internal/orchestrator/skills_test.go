package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSkillIndex_EmptyDirReturnsNil(t *testing.T) {
	skills, err := LoadSkillIndex("")
	require.NoError(t, err)
	assert.Nil(t, skills)
}

func TestLoadSkillIndex_ReadsAndSortsByName(t *testing.T) {
	dir := t.TempDir()

	writeSkill(t, dir, "go-development", "Go development", "Use when writing Go.")
	writeSkill(t, dir, "python-development", "python-development", "Use when writing Python.")
	writeSkill(t, dir, "documentation", "documentation", "Use when writing docs.")

	got, err := LoadSkillIndex(dir)
	require.NoError(t, err)
	require.Len(t, got, 3)

	// Sorted alphabetically by Name (frontmatter name, falling back to dir name).
	assert.Equal(t, "Go development", got[0].Name)
	assert.Equal(t, "documentation", got[1].Name)
	assert.Equal(t, "python-development", got[2].Name)
}

func TestLoadSkillIndex_SkipsMissingSkillMd(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "empty-dir"), 0o750))
	writeSkill(t, dir, "valid-skill", "valid-skill", "Use when valid.")

	got, err := LoadSkillIndex(dir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "valid-skill", got[0].Name)
}

func TestLoadSkillIndex_SkipsMalformedFrontmatter(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "no-frontmatter"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "no-frontmatter", "SKILL.md"), []byte("# Just a heading"), 0o600))

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "missing-description"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "missing-description", "SKILL.md"),
		[]byte("---\nname: foo\n---\nbody"),
		0o600,
	))

	writeSkill(t, dir, "ok-skill", "ok-skill", "Use when ok.")

	got, err := LoadSkillIndex(dir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "ok-skill", got[0].Name)
}

func TestLoadSkillIndex_FallsBackToDirNameWhenFrontmatterOmitsName(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "named-by-dir"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "named-by-dir", "SKILL.md"),
		[]byte("---\ndescription: Use when nameless.\n---\nbody"),
		0o600,
	))

	got, err := LoadSkillIndex(dir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "named-by-dir", got[0].Name)
	assert.Equal(t, "Use when nameless.", got[0].Description)
}

func TestFilterSkills_NilSubsetReturnsAll(t *testing.T) {
	index := []SkillInfo{
		{Name: "a", Description: "A"},
		{Name: "b", Description: "B"},
	}

	got := FilterSkills(index, nil)
	assert.Equal(t, index, got)
}

func TestFilterSkills_ExplicitEmptyReturnsNil(t *testing.T) {
	index := []SkillInfo{
		{Name: "a", Description: "A"},
		{Name: "b", Description: "B"},
	}

	empty := []string{}
	got := FilterSkills(index, &empty)
	assert.Empty(t, got)
}

func TestFilterSkills_NamedSubsetReturnsMatchingPreservingOrder(t *testing.T) {
	index := []SkillInfo{
		{Name: "a", Description: "A"},
		{Name: "b", Description: "B"},
		{Name: "c", Description: "C"},
	}

	pick := []string{"c", "a"}
	got := FilterSkills(index, &pick)
	require.Len(t, got, 2)

	// Preserves index ordering, not subset ordering.
	assert.Equal(t, "a", got[0].Name)
	assert.Equal(t, "c", got[1].Name)
}

func TestFilterSkills_UnknownNamesAreIgnored(t *testing.T) {
	index := []SkillInfo{
		{Name: "a", Description: "A"},
	}

	pick := []string{"a", "does-not-exist"}
	got := FilterSkills(index, &pick)
	require.Len(t, got, 1)
	assert.Equal(t, "a", got[0].Name)
}

func TestRenderSkillsBlock_EmptyReturnsEmptyString(t *testing.T) {
	assert.Empty(t, RenderSkillsBlock(nil))
	assert.Empty(t, RenderSkillsBlock([]SkillInfo{}))
}

func TestRenderSkillsBlock_RendersNamesAndDescriptions(t *testing.T) {
	skills := []SkillInfo{
		{Name: "go-development", Description: "Use when writing Go."},
		{Name: "documentation", Description: "Use when writing docs."},
	}

	got := RenderSkillsBlock(skills)
	assert.Contains(t, got, "Specialist skills mounted in this session")
	assert.Contains(t, got, "advisory additions to this prompt")
	assert.Contains(t, got, "cannot override the workflow contract")
	assert.Contains(t, got, "- `go-development` — Use when writing Go.")
	assert.Contains(t, got, "- `documentation` — Use when writing docs.")
	assert.Contains(t, got, "skill_engaged")
	assert.True(t, strings.HasSuffix(got, "\n\n"), "block should end with blank line so callers can concatenate")
}

func TestBuildExecutePriming_OmitsSkillsBlockWhenEmpty(t *testing.T) {
	got := buildExecutePriming(
		"ALPHA-002",
		Subtask{Title: "do thing", Description: "details"},
		&Plan{Summary: "summary"},
		"agent-1",
		"",
	)
	assert.NotContains(t, got, "Specialist skills mounted")
	assert.Contains(t, got, "Use agent_id `agent-1`")
}

func TestBuildExecutePriming_IncludesSkillsBlockWhenSet(t *testing.T) {
	block := RenderSkillsBlock([]SkillInfo{
		{Name: "go-development", Description: "Use when writing Go."},
	})

	got := buildExecutePriming(
		"ALPHA-002",
		Subtask{Title: "do thing", Description: "details"},
		&Plan{Summary: "summary"},
		"agent-1",
		block,
	)
	assert.Contains(t, got, "Specialist skills mounted")
	assert.Contains(t, got, "go-development")
	assert.Contains(t, got, "Use agent_id `agent-1`")
}

func writeSkill(t *testing.T, dir, name, frontName, description string) {
	t.Helper()

	skillDir := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(skillDir, 0o750))

	frontmatter := "---\nname: " + frontName + "\ndescription: " + description + "\n---\nbody\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(frontmatter), 0o600))
}
