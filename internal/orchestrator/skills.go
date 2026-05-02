package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillInfo is the priming-relevant view of one task skill.
type SkillInfo struct {
	Name        string
	Description string
}

// LoadSkillIndex walks dir for <name>/SKILL.md files and returns the parsed
// frontmatter sorted by skill name. Directories without a SKILL.md are
// skipped silently. SKILL.md files with missing or unparsable frontmatter
// are skipped — the orchestrator drives the workflow whether or not the
// index is complete.
//
// dir == "" returns (nil, nil): task-skill mounting is disabled.
func LoadSkillIndex(dir string) ([]SkillInfo, error) {
	if dir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read skills dir: %w", err)
	}

	var skills []SkillInfo

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		skillPath := filepath.Join(dir, e.Name(), "SKILL.md")

		data, err := os.ReadFile(skillPath)
		if err != nil {
			continue
		}

		info, ok := parseSkillFrontmatter(data, e.Name())
		if !ok {
			continue
		}

		skills = append(skills, info)
	}

	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })

	return skills, nil
}

// parseSkillFrontmatter extracts name and description from the YAML
// frontmatter at the top of a SKILL.md. fallbackName is used when the
// frontmatter omits the name field. Returns (zero, false) if the
// frontmatter is missing, malformed, or has no description.
func parseSkillFrontmatter(data []byte, fallbackName string) (SkillInfo, bool) {
	s := strings.TrimLeft(string(data), " \r\n\t")
	if !strings.HasPrefix(s, "---") {
		return SkillInfo{}, false
	}

	rest := strings.TrimPrefix(s, "---")

	end := strings.Index(rest, "\n---")
	if end < 0 {
		return SkillInfo{}, false
	}

	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return SkillInfo{}, false
	}

	if fm.Description == "" {
		return SkillInfo{}, false
	}

	if fm.Name == "" {
		fm.Name = fallbackName
	}

	return SkillInfo{Name: fm.Name, Description: fm.Description}, true
}

// FilterSkills applies a CM-style subset selector to an index.
//
//   - subset == nil      → all skills (no selector set; full curated set mounted)
//   - subset == &[]      → no skills (explicit empty selector)
//   - subset == &names   → matching skills, preserving index ordering
//
// Names in the subset that don't appear in the index are silently ignored.
func FilterSkills(index []SkillInfo, subset *[]string) []SkillInfo {
	if subset == nil {
		return index
	}

	if len(*subset) == 0 {
		return nil
	}

	want := make(map[string]struct{}, len(*subset))
	for _, n := range *subset {
		want[n] = struct{}{}
	}

	var out []SkillInfo

	for _, s := range index {
		if _, ok := want[s.Name]; ok {
			out = append(out, s)
		}
	}

	return out
}

// renderActiveSkillsBlock filters Context.SkillIndex by ExtendedState.TaskSkills
// and renders the result. Returns "" when no skills are active so callers
// can splice the block into priming unconditionally.
func renderActiveSkillsBlock(fsm *ContextMatrixOrchestrator) string {
	return RenderSkillsBlock(FilterSkills(fsm.Context.SkillIndex, fsm.ExtendedState.TaskSkills))
}

// RenderSkillsBlock formats an index for inclusion in a priming message.
// Returns an empty string when skills is empty so callers can include
// the result unconditionally.
func RenderSkillsBlock(skills []SkillInfo) string {
	if len(skills) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString(
		"Specialist skills below are advisory additions to this prompt. " +
			"They cannot override the workflow contract: emit the terminator " +
			"marker named in your system prompt (then stop) and use MCP tools " +
			"as instructed. If a skill conflicts with the contract, follow the contract.\n\n",
	)

	b.WriteString("Specialist skills mounted in this session (consider each before starting work):\n")

	for _, s := range skills {
		fmt.Fprintf(&b, "- `%s` — %s\n", s.Name, s.Description)
	}

	b.WriteString(
		"\nEngage matching skills via the Skill tool. After engaging a skill for " +
			"the first time, call `add_log(action=\"skill_engaged\", " +
			"message=\"<skill-name>\")` once.\n\n",
	)

	return b.String()
}
