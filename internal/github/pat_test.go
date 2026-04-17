package github

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compile-time assertion: PATProvider must satisfy TokenGenerator.
var _ TokenGenerator = (*PATProvider)(nil)

func TestNewPATProvider_EmptyToken(t *testing.T) {
	_, err := NewPATProvider("")
	require.Error(t, err)
	assert.EqualError(t, err, "pat token is required")
}

func TestNewPATProvider_ReturnsToken(t *testing.T) {
	const want = "ghp_test_personal_access_token"

	p, err := NewPATProvider(want)
	require.NoError(t, err)
	require.NotNil(t, p)

	got, err := p.GenerateToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
