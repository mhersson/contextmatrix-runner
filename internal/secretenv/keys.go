// Package secretenv is the single source of truth for the list of
// environment-variable names whose values are runner-managed secrets.
//
// The runner injects these names into worker containers; logparser must
// redact their values whenever they appear in container output; and the
// config validator must reject operator-supplied worker_extra_env keys
// that collide with them (otherwise a leaked YAML would silently overwrite
// a freshly-rotated secret).
//
// Adding a new injected secret requires extending only this slice — every
// consumer reads through it.
package secretenv

// keys is the canonical list of environment-variable names treated as
// runner-managed secrets. Kept unexported so consumers cannot mutate the
// backing slice (e.g. a test appending an entry would leak across packages
// because slices share backing arrays). Use [Keys] to obtain a defensive
// copy, or [Contains] for a membership check.
var keys = []string{
	"CLAUDE_CODE_OAUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"CM_MCP_API_KEY",
	"CM_GIT_TOKEN",
}

// Keys returns a fresh copy of the canonical secret env-var names. Callers
// that only need a membership check should use [Contains] to avoid the
// allocation.
func Keys() []string {
	out := make([]string, len(keys))
	copy(out, keys)

	return out
}

// Contains reports whether key is one of the runner-managed secret env
// var names. Cheaper than [Keys] when you only need a membership check.
func Contains(key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}

	return false
}
