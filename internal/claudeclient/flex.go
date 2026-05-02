package claudeclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// FlexibleStringSlice is []string that also unmarshals from a bare
// JSON string or null. Real Claude has been observed emitting a single
// string in fields the MCP tool schema declares as an array (e.g.
// `chosen_repos: "only-repo"`); strict json.Unmarshal then fails the
// whole phase. Accepting both shapes at the runner boundary keeps the
// FSM moving when an LLM tool call drifts off-schema.
type FlexibleStringSlice []string

func (s *FlexibleStringSlice) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = nil

		return nil
	}

	switch trimmed[0] {
	case '[':
		var arr []string
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}

		if len(arr) == 0 {
			*s = nil
		} else {
			*s = arr
		}

		return nil
	case '"':
		var single string
		if err := json.Unmarshal(data, &single); err != nil {
			return err
		}

		if single == "" {
			*s = nil
		} else {
			*s = []string{single}
		}

		return nil
	}

	return fmt.Errorf("flexible string slice: expected array or string, got %s", string(trimmed))
}

// FlexibleSubtaskList is []SubtaskSpec that also unmarshals from a
// JSON-string-encoded array or null. Real Claude has been observed
// emitting `subtasks: "<json>"` (a string containing a JSON array)
// instead of the array itself. Strict json.Unmarshal then fails the
// whole phase. We unwrap one level of stringification before giving
// up.
type FlexibleSubtaskList []SubtaskSpec

func (l *FlexibleSubtaskList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*l = nil

		return nil
	}

	switch trimmed[0] {
	case '[':
		var arr []SubtaskSpec
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}

		if len(arr) == 0 {
			*l = nil
		} else {
			*l = arr
		}

		return nil
	case '"':
		var inner string
		if err := json.Unmarshal(data, &inner); err != nil {
			return err
		}

		innerTrimmed := strings.TrimSpace(inner)
		if innerTrimmed == "" {
			*l = nil

			return nil
		}

		if innerTrimmed[0] != '[' {
			return fmt.Errorf("flexible subtask list: string did not contain a JSON array: %q", inner)
		}

		var arr []SubtaskSpec
		if err := json.Unmarshal([]byte(innerTrimmed), &arr); err != nil {
			return fmt.Errorf("flexible subtask list: parse string-encoded JSON: %w (raw=%q)", err, inner)
		}

		if len(arr) == 0 {
			*l = nil
		} else {
			*l = arr
		}

		return nil
	}

	return fmt.Errorf("flexible subtask list: expected array or string, got %s", string(trimmed))
}
