package secretenv_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/contextmatrix-runner/internal/secretenv"
)

// TestKeys_ContainsExpectedSecrets verifies that the canonical secret list
// contains each runner-managed key. Adding or removing entries from Keys is
// load-bearing for both logparser redaction and config validation, so we
// pin the expected membership here.
func TestKeys_ContainsExpectedSecrets(t *testing.T) {
	expected := []string{
		"CLAUDE_CODE_OAUTH_TOKEN",
		"ANTHROPIC_API_KEY",
		"CM_MCP_API_KEY",
		"CM_GIT_TOKEN",
	}

	for _, k := range expected {
		assert.True(t, secretenv.Contains(k), "Keys must contain %q", k)
	}
}

// TestContains_UnknownKey verifies that an env var name not in the list is
// reported as non-secret.
func TestContains_UnknownKey(t *testing.T) {
	assert.False(t, secretenv.Contains("HOME"))
	assert.False(t, secretenv.Contains(""))
	assert.False(t, secretenv.Contains("CLAUDE_CODE_OAUTH_TOKE")) // typo
}

// TestKeys_ReturnsFreshCopy verifies that mutating the slice returned by
// Keys() does not affect the canonical list. The previous exported `Keys`
// variable let a malicious or buggy test append to the backing array,
// silently widening the redactor's secret-name allowlist across packages.
func TestKeys_ReturnsFreshCopy(t *testing.T) {
	first := secretenv.Keys()
	original := append([]string(nil), first...)

	// Mutate the returned slice: replace each entry and append.
	for i := range first {
		first[i] = "MUTATED"
	}

	first = append(first, "INJECTED")
	_ = first

	// A second call must return the original canonical list, unaffected.
	second := secretenv.Keys()
	assert.Equal(t, original, second,
		"Keys() must return a defensive copy; callers must not be able to mutate the canonical list")
}
