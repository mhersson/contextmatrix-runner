package claudeclient

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	planSectionRe = regexp.MustCompile(`(?m)^##\s+Plan\s*$`)
	nextH2Re      = regexp.MustCompile(`(?m)^##\s+\S`)
)

// ExtractPlanJSON returns the bytes of the last fenced ```json block
// inside the `## Plan` section of a card body, or nil if none. The
// returned bytes are validated as JSON. Returns an error only if a
// fenced block is found but contains invalid JSON (so callers can
// surface the malformed payload for diagnosis).
//
// The `## Plan` section ends at the next `## ` heading or EOF. Fenced
// blocks outside that section are ignored. When multiple blocks exist
// inside the section (e.g. across replan rounds), the last one wins.
//
// Format contract: the runner reads from here; the MCP plan_complete
// tool input is a thin terminal signal. See prompts/plan.md for the
// agent-facing format spec.
func ExtractPlanJSON(body string) (json.RawMessage, error) {
	loc := planSectionRe.FindStringIndex(body)
	if loc == nil {
		return nil, nil
	}

	rest := body[loc[1]:]

	if next := nextH2Re.FindStringIndex(rest); next != nil {
		rest = rest[:next[0]]
	}

	matches := fencedJSONRe.FindAllStringSubmatch(rest, -1)
	if len(matches) == 0 {
		return nil, nil
	}

	last := matches[len(matches)-1][1]

	candidate := strings.TrimSpace(last)
	if candidate == "" {
		return nil, nil
	}

	var probe any
	if err := json.Unmarshal([]byte(candidate), &probe); err != nil {
		return nil, err
	}

	return json.RawMessage(candidate), nil
}
