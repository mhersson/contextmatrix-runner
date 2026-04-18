package streammsg_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/streammsg"
)

func TestBuildUserMessage_Shape(t *testing.T) {
	b, err := streammsg.BuildUserMessage("hello world")
	require.NoError(t, err)

	// Strip the trailing newline before unmarshaling.
	require.Equal(t, byte('\n'), b[len(b)-1], "output must be newline-terminated")

	var got streammsg.UserMessage
	require.NoError(t, json.Unmarshal(b[:len(b)-1], &got))

	assert.Equal(t, "user", got.Type)
	assert.Equal(t, "user", got.Message.Role)
	require.Len(t, got.Message.Content, 1)
	assert.Equal(t, "text", got.Message.Content[0].Type)
	assert.Equal(t, "hello world", got.Message.Content[0].Text)
}

func TestBuildUserMessage_NewlineTerminated(t *testing.T) {
	b, err := streammsg.BuildUserMessage("ping")
	require.NoError(t, err)
	assert.Equal(t, byte('\n'), b[len(b)-1])
}

func TestBuildUserMessage_EmptyContent(t *testing.T) {
	b, err := streammsg.BuildUserMessage("")
	require.NoError(t, err)
	require.Equal(t, byte('\n'), b[len(b)-1], "output must be newline-terminated")

	var got streammsg.UserMessage
	require.NoError(t, json.Unmarshal(b[:len(b)-1], &got))

	assert.Equal(t, "user", got.Type)
	assert.Equal(t, "user", got.Message.Role)
	require.Len(t, got.Message.Content, 1)
	assert.Equal(t, "text", got.Message.Content[0].Type)
	assert.Equal(t, "", got.Message.Content[0].Text)
}
