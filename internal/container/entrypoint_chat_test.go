package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEntrypoint_ChatModeBranch verifies the chat-mode dispatch is present
// in entrypoint.sh and uses stream-json + the autonomous allowlist.
func TestEntrypoint_ChatModeBranch(t *testing.T) {
	// Locate entrypoint.sh relative to repo root.
	wd, err := os.Getwd()
	require.NoError(t, err)
	// wd is .../internal/container; go up two for repo root.
	path := filepath.Join(wd, "..", "..", "docker", "entrypoint.sh")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	src := string(raw)

	// Dispatch branch must exist.
	assert.Contains(t, src, `[ -n "${CM_CHAT_SESSION:-}" ]`,
		"entrypoint.sh must dispatch on CM_CHAT_SESSION")
	// Workspace mkdir — chat reuses the shared /home/user/workspace dir
	// that the entrypoint prepares before the dispatch (chat containers
	// run as non-root and can't write to a top-level /workspace).
	assert.Contains(t, src, "mkdir -p /home/user/workspace",
		"entrypoint must create the home workspace before dispatch")
	// Stream-json execution.
	chatIdx := strings.Index(src, `if [ -n "${CM_CHAT_SESSION:-}" ]; then`)
	require.NotEqual(t, -1, chatIdx)
	// Look at the slice from chat branch up to a few hundred bytes after.
	chatBranch := src[chatIdx:]

	end := strings.Index(chatBranch, "\nelif ")
	if end == -1 {
		end = len(chatBranch)
	}

	chatBranch = chatBranch[:end]
	assert.Contains(t, chatBranch, "--input-format stream-json")
	assert.Contains(t, chatBranch, "--output-format stream-json")
	assert.Contains(t, chatBranch, "${ALLOWED_TOOLS_CHAT[*]}")
}

func TestEntrypoint_ChatModeValidation(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)

	path := filepath.Join(wd, "..", "..", "docker", "entrypoint.sh")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	src := string(raw)
	assert.Contains(t, src, "ERROR: invalid CM_CHAT_SESSION",
		"must validate CM_CHAT_SESSION")
	assert.Contains(t, src, "ERROR: invalid CM_CHAT_PROJECT",
		"must validate CM_CHAT_PROJECT")
	assert.Contains(t, src, "ERROR: CM_CHAT_REPO_URL must be https://",
		"must validate CM_CHAT_REPO_URL scheme")
}
