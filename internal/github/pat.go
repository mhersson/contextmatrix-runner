package github

import (
	"context"
	"errors"
)

// PATProvider returns a static personal access token. Use this when the user
// cannot create a GitHub App (common in GitHub Enterprise).
type PATProvider struct {
	token string
}

var _ TokenGenerator = (*PATProvider)(nil)

// NewPATProvider returns a PATProvider wrapping the given token.
// Returns an error if token is empty.
func NewPATProvider(token string) (*PATProvider, error) {
	if token == "" {
		return nil, errors.New("pat token is required")
	}

	return &PATProvider{token: token}, nil
}

// GenerateToken returns the configured PAT. The context is accepted for
// interface compatibility but unused.
func (p *PATProvider) GenerateToken(_ context.Context) (string, error) {
	return p.token, nil
}
