package streammsg_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/streammsg"
)

func TestBuildToolResultMessage_Shape(t *testing.T) {
	b, err := streammsg.BuildToolResultMessage("toolu_abc123", "the answer")
	require.NoError(t, err)

	// Strip trailing newline before unmarshaling.
	require.Equal(t, byte('\n'), b[len(b)-1], "output must be newline-terminated")

	var got streammsg.ToolResultMessage
	require.NoError(t, json.Unmarshal(b[:len(b)-1], &got))

	assert.Equal(t, "user", got.Type)
	assert.Equal(t, "user", got.Message.Role)
	require.Len(t, got.Message.Content, 1)
	assert.Equal(t, "tool_result", got.Message.Content[0].Type)
	assert.Equal(t, "toolu_abc123", got.Message.Content[0].ToolUseID)
	assert.Equal(t, "the answer", got.Message.Content[0].Content)
}

func TestBuildToolResultMessage_NewlineTerminated(t *testing.T) {
	b, err := streammsg.BuildToolResultMessage("toolu_abc123", "ping")
	require.NoError(t, err)
	assert.Equal(t, byte('\n'), b[len(b)-1])
}

func TestBuildToolResultMessage_EmptyToolUseID(t *testing.T) {
	b, err := streammsg.BuildToolResultMessage("", "some content")
	assert.Nil(t, b)
	require.EqualError(t, err, "streammsg: BuildToolResultMessage: toolUseID is required")
}
