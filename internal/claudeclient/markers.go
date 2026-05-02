package claudeclient

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MarkerKind identifies a structured terminal marker emitted by a CC
// phase action.
type MarkerKind string

const (
	MarkerPlanDrafted        MarkerKind = "PLAN_DRAFTED"
	MarkerTaskComplete       MarkerKind = "TASK_COMPLETE"
	MarkerTaskBlocked        MarkerKind = "TASK_BLOCKED"
	MarkerTaskNeedsDecomp    MarkerKind = "TASK_NEEDS_DECOMPOSITION"
	MarkerReviewFindings     MarkerKind = "REVIEW_FINDINGS"
	MarkerDocsWritten        MarkerKind = "DOCS_WRITTEN"
	MarkerConflictResolved   MarkerKind = "CONFLICT_RESOLVED"
	MarkerConflictUnresolved MarkerKind = "CONFLICT_UNRESOLVED"
	MarkerDiagnosisComplete  MarkerKind = "DIAGNOSIS_COMPLETE"
	MarkerDiscoveryComplete  MarkerKind = "discovery_complete" // tool_use, not text
	MarkerPlanComplete       MarkerKind = "plan_complete"      // tool_use, not text
	MarkerReviewApprove      MarkerKind = "review_approve"     // tool_use, not text
	MarkerReviewRevise       MarkerKind = "review_revise"      // tool_use, not text
	MarkerRunComplete        MarkerKind = "RUN_COMPLETE"
	MarkerAutonomousHalted   MarkerKind = "AUTONOMOUS_HALTED"
)

// Marker is one recognized terminal marker with its key:value fields and an
// optional raw JSON payload extracted from a fenced ```json ... ``` block
// following the marker header.
type Marker struct {
	Kind   MarkerKind
	Fields map[string]string
	JSON   []byte // populated when the body contains a fenced JSON block
	Raw    string
}

var markerHeaderRe = regexp.MustCompile(
	`(?m)^(PLAN_DRAFTED|TASK_COMPLETE|TASK_BLOCKED|TASK_NEEDS_DECOMPOSITION|REVIEW_FINDINGS|DOCS_WRITTEN|CONFLICT_RESOLVED|CONFLICT_UNRESOLVED|DIAGNOSIS_COMPLETE|RUN_COMPLETE|AUTONOMOUS_HALTED)\s*$`,
)

// fencedJSONRe matches a fenced JSON code block. The opening fence may be
// ```json or just ``` (we accept both because LLMs often omit the language
// hint). Captures the inner content.
var fencedJSONRe = regexp.MustCompile("(?s)```(?:json)?\\s*\\n(.*?)\\n```")

// RecognizeMarker scans free-text output for a structured marker.
// When multiple markers appear, the last one is returned (markers are
// emitted at end of phase by convention).
func RecognizeMarker(text string) (Marker, bool) {
	matches := markerHeaderRe.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return Marker{}, false
	}

	last := matches[len(matches)-1]
	headerStart := last[0]
	headerEnd := last[1]
	kind := MarkerKind(strings.TrimSpace(text[headerStart:headerEnd]))
	body := text[headerEnd:]

	return Marker{
		Kind:   kind,
		Fields: parseMarkerBody(body),
		JSON:   extractFencedJSON(body),
		Raw:    text[headerStart:],
	}, true
}

// extractFencedJSON returns the contents of the FIRST fenced code block in
// body that parses as a JSON object or array, or nil if none is found.
func extractFencedJSON(body string) []byte {
	for _, m := range fencedJSONRe.FindAllStringSubmatch(body, -1) {
		if len(m) < 2 {
			continue
		}

		candidate := []byte(strings.TrimSpace(m[1]))
		if len(candidate) == 0 {
			continue
		}

		switch candidate[0] {
		case '{', '[':
			// Validate it actually parses; otherwise keep looking.
			var probe interface{}
			if err := json.Unmarshal(candidate, &probe); err == nil {
				return candidate
			}
		}
	}

	return nil
}

// RecognizeFromToolUse handles markers emitted as tool calls — the HITL
// chat-loop terminal markers (discovery_complete, plan_complete,
// review_approve, review_revise). The runner intercepts the tool_use event
// before the actual MCP call completes, so the server response is
// irrelevant for control flow.
//
// Real Claude emits MCP-registered tools with the full server-prefixed
// name (e.g. "mcp__contextmatrix__plan_complete"). The integration stub
// emits the bare name. Strip the prefix so both shapes resolve to the
// same MarkerKind.
func RecognizeFromToolUse(ev StreamEvent) (Marker, bool) {
	if ev.Kind != EventToolUse {
		return Marker{}, false
	}

	name := strings.TrimPrefix(ev.ToolName, "mcp__contextmatrix__")

	var kind MarkerKind

	switch name {
	case "discovery_complete":
		kind = MarkerDiscoveryComplete
	case "plan_complete":
		kind = MarkerPlanComplete
	case "review_approve":
		kind = MarkerReviewApprove
	case "review_revise":
		kind = MarkerReviewRevise
	default:
		return Marker{}, false
	}

	var input map[string]any

	_ = json.Unmarshal(ev.ToolInput, &input)

	fields := make(map[string]string, len(input))
	for k, v := range input {
		if s, ok := v.(string); ok {
			fields[k] = s
		}
	}

	return Marker{
		Kind:   kind,
		Fields: fields,
		Raw:    string(ev.ToolInput),
	}, true
}

func parseMarkerBody(body string) map[string]string {
	fields := make(map[string]string)

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		idx := strings.Index(line, ":")
		if idx < 0 {
			// End of marker fields when a non-key:value line appears.
			break
		}

		k := strings.TrimSpace(line[:idx])
		v := strings.TrimSpace(line[idx+1:])
		fields[k] = v
	}

	return fields
}

// ----- Typed payloads -----

type PlanDraftedPayload struct {
	CardID       string
	Status       string
	PlanSummary  string
	ChosenRepos  []string
	Subtasks     []SubtaskSpec
	SubtaskCount int
}

// SubtaskSpec is one subtask carried in the structured PLAN_DRAFTED JSON
// payload. The orchestrator translates this into orchestrator.Subtask + a
// CreateCard call per entry.
//
// DependsOn lists OTHER existing CM card IDs that must be in `done`
// state before this subtask is considered ready. The orchestrator
// passes it through to CM's create_card `depends_on` field. The plan
// agent ALSO uses this field to declare cross-subtask deps within the
// same plan (CreateSubtaskCardsAction resolves those references after
// the cards exist).
type SubtaskSpec struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Repos       []string `json:"repos"`
	Priority    string   `json:"priority"`
	DependsOn   []string `json:"depends_on"`
}

// UnmarshalJSON tolerates real-Claude tool calls that emit `repos` or
// `depends_on` as a bare string instead of the schema-declared array.
// See FlexibleStringSlice for the rationale.
func (s *SubtaskSpec) UnmarshalJSON(data []byte) error {
	var shadow struct {
		Title       string              `json:"title"`
		Description string              `json:"description"`
		Repos       FlexibleStringSlice `json:"repos"`
		Priority    string              `json:"priority"`
		DependsOn   FlexibleStringSlice `json:"depends_on"`
	}

	if err := json.Unmarshal(data, &shadow); err != nil {
		return err
	}

	s.Title = shadow.Title
	s.Description = shadow.Description
	s.Repos = []string(shadow.Repos)
	s.Priority = shadow.Priority
	s.DependsOn = []string(shadow.DependsOn)

	return nil
}

type TaskCompletePayload struct {
	CardID  string
	Status  string
	Summary string
}

type TaskBlockedPayload struct {
	CardID       string
	Reason       string
	BlockerCards []string
	NeedsHuman   bool
}

type TaskNeedsDecompositionPayload struct {
	CardID   string
	Subtasks []string
}

type ReviewFindingsPayload struct {
	CardID         string
	Recommendation string
	Summary        string
}

type DocsWrittenPayload struct {
	CardID       string
	FilesWritten []string
}

type ConflictResolvedPayload struct {
	CardID        string
	Status        string
	FilesResolved []string
}

type ConflictUnresolvedPayload struct {
	CardID string
	Status string
	Reason string
}

type DiagnosisCompletePayload struct {
	CardID    string
	RootCause string
}

type DiscoveryCompletePayload struct {
	DesignSummary string
}

// ParseMarker scans text for a marker and unmarshals it into dst.
// Returns an error if no marker is found OR if the marker doesn't match
// the expected kind for the payload type.
func ParseMarker(text string, dst interface{}) error {
	m, ok := RecognizeMarker(text)
	if !ok {
		return fmt.Errorf("no marker found in text")
	}

	return populatePayload(m, dst)
}

func populatePayload(m Marker, dst interface{}) error {
	switch v := dst.(type) {
	case *PlanDraftedPayload:
		if m.Kind != MarkerPlanDrafted {
			return fmt.Errorf("expected PLAN_DRAFTED, got %s", m.Kind)
		}

		// Prefer the structured JSON payload when present. Fall back to
		// the legacy key:value fields so older formats still parse.
		if len(m.JSON) > 0 {
			var jp struct {
				CardID      string              `json:"card_id"`
				PlanSummary string              `json:"plan_summary"`
				ChosenRepos FlexibleStringSlice `json:"chosen_repos"`
				Subtasks    FlexibleSubtaskList `json:"subtasks"`
			}

			if err := json.Unmarshal(m.JSON, &jp); err != nil {
				return fmt.Errorf("plan_drafted: parse json block: %w (raw=%s)", err, string(m.JSON))
			}

			v.CardID = jp.CardID
			v.PlanSummary = jp.PlanSummary
			v.ChosenRepos = []string(jp.ChosenRepos)
			v.Subtasks = []SubtaskSpec(jp.Subtasks)
			v.SubtaskCount = len(jp.Subtasks)
			v.Status = "drafted"

			return nil
		}

		v.CardID = m.Fields["card_id"]
		v.Status = m.Fields["status"]
		v.PlanSummary = m.Fields["plan_summary"]

		if n, err := strconv.Atoi(m.Fields["subtask_count"]); err == nil {
			v.SubtaskCount = n
		}
	case *TaskCompletePayload:
		if m.Kind != MarkerTaskComplete {
			return fmt.Errorf("expected TASK_COMPLETE, got %s", m.Kind)
		}

		v.CardID = m.Fields["card_id"]
		v.Status = m.Fields["status"]
		v.Summary = m.Fields["summary"]
	case *TaskBlockedPayload:
		if m.Kind != MarkerTaskBlocked {
			return fmt.Errorf("expected TASK_BLOCKED, got %s", m.Kind)
		}

		v.CardID = m.Fields["card_id"]
		v.Reason = m.Fields["reason"]
		v.NeedsHuman = m.Fields["needs_human"] == "true"
		v.BlockerCards = parseListField(m.Fields["blocker_cards"])
	case *TaskNeedsDecompositionPayload:
		if m.Kind != MarkerTaskNeedsDecomp {
			return fmt.Errorf("expected TASK_NEEDS_DECOMPOSITION, got %s", m.Kind)
		}

		v.CardID = m.Fields["card_id"]
		v.Subtasks = parseListField(m.Fields["subtasks"])
	case *ReviewFindingsPayload:
		if m.Kind != MarkerReviewFindings {
			return fmt.Errorf("expected REVIEW_FINDINGS, got %s", m.Kind)
		}

		v.CardID = m.Fields["card_id"]
		v.Recommendation = m.Fields["recommendation"]
		v.Summary = m.Fields["summary"]
	case *DocsWrittenPayload:
		if m.Kind != MarkerDocsWritten {
			return fmt.Errorf("expected DOCS_WRITTEN, got %s", m.Kind)
		}

		v.CardID = m.Fields["card_id"]
		v.FilesWritten = parseListField(m.Fields["files_written"])
	case *ConflictResolvedPayload:
		if m.Kind != MarkerConflictResolved {
			return fmt.Errorf("expected %s, got %s", MarkerConflictResolved, m.Kind)
		}

		v.CardID = m.Fields["card_id"]
		v.Status = m.Fields["status"]
		v.FilesResolved = parseListField(m.Fields["files_resolved"])
	case *ConflictUnresolvedPayload:
		if m.Kind != MarkerConflictUnresolved {
			return fmt.Errorf("expected %s, got %s", MarkerConflictUnresolved, m.Kind)
		}

		v.CardID = m.Fields["card_id"]
		v.Status = m.Fields["status"]
		v.Reason = m.Fields["reason"]
	case *DiagnosisCompletePayload:
		if m.Kind != MarkerDiagnosisComplete {
			return fmt.Errorf("expected DIAGNOSIS_COMPLETE, got %s", m.Kind)
		}

		v.CardID = m.Fields["card_id"]
		v.RootCause = m.Fields["root_cause"]
	case *DiscoveryCompletePayload:
		if m.Kind != MarkerDiscoveryComplete {
			return fmt.Errorf("expected discovery_complete, got %s", m.Kind)
		}

		v.DesignSummary = m.Fields["design_summary"]
	default:
		return fmt.Errorf("unknown payload type %T", dst)
	}

	return nil
}

// parseListField parses "[a, b, c]" into ["a", "b", "c"]. Tolerant of
// empty brackets and missing brackets.
func parseListField(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")

	if s == "" {
		return nil
	}

	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}

	return out
}
