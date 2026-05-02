package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration wrapper that accepts Go duration strings
// ("30m", "10s", "2h") in YAML. The plain time.Duration type unmarshals
// from int nanoseconds only, so config files written with human-friendly
// strings would silently fail with a parse error — see config.yaml.example
// where idle_output_timeout and maintenance_interval are quoted strings.
//
// Cast back to time.Duration with `time.Duration(d)` when consuming the
// value.
type Duration time.Duration

// UnmarshalYAML accepts both raw integers (legacy nanoseconds) and Go
// duration strings ("30m"). An empty string parses as zero so callers
// can rely on the field's default-handling in Validate().
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	switch value.Tag {
	case "!!int":
		var ns int64
		if err := value.Decode(&ns); err != nil {
			return fmt.Errorf("duration int: %w", err)
		}

		*d = Duration(time.Duration(ns))

		return nil
	case "!!str":
		var s string
		if err := value.Decode(&s); err != nil {
			return fmt.Errorf("duration string: %w", err)
		}

		if s == "" {
			*d = 0

			return nil
		}

		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("duration parse: %w", err)
		}

		*d = Duration(parsed)

		return nil
	default:
		return fmt.Errorf("duration must be a string (\"30m\") or integer nanoseconds, got tag %q", value.Tag)
	}
}

// MarshalYAML emits the Duration as a Go duration string so round-tripping
// produces a human-readable config file.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}
