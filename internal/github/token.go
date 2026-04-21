// Package github generates short-lived installation tokens from a GitHub App.
package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// jwtExpiry is the lifetime of the JWT used to authenticate as the App.
	jwtExpiry = 10 * time.Minute

	// tokenRequestTimeout is the HTTP timeout for the token exchange request.
	tokenRequestTimeout = 10 * time.Second
)

// TokenGenerator produces a git credential for the current request.
type TokenGenerator interface {
	GenerateToken(ctx context.Context) (string, error)
}

var _ TokenGenerator = (*TokenProvider)(nil)

// Option is a functional option for configuring a TokenProvider.
type Option func(*TokenProvider)

// WithAPIBaseURL returns an Option that overrides the GitHub API base URL.
// Trailing slashes are trimmed. Empty string is a no-op.
func WithAPIBaseURL(u string) Option {
	return func(p *TokenProvider) {
		if u == "" {
			return
		}

		p.apiBaseURL = strings.TrimRight(u, "/")
	}
}

// TokenProvider generates installation access tokens for a GitHub App.
type TokenProvider struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
	apiBaseURL     string // default: https://api.github.com
	httpClient     *http.Client
}

// NewTokenProvider creates a TokenProvider by reading the PEM private key from disk.
// Optional functional options (e.g. WithAPIBaseURL) are applied after the provider
// is constructed with its defaults.
func NewTokenProvider(appID, installationID int64, privateKeyPath string, opts ...Option) (*TokenProvider, error) {
	keyData, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}

	key, err := jwt.ParseRSAPrivateKeyFromPEM(keyData)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	tp := &TokenProvider{
		appID:          appID,
		installationID: installationID,
		privateKey:     key,
		apiBaseURL:     "https://api.github.com",
		httpClient:     &http.Client{Timeout: tokenRequestTimeout},
	}
	for _, opt := range opts {
		opt(tp)
	}

	return tp, nil
}

// NewTokenProviderWithKey creates a TokenProvider from an already-parsed RSA key
// and custom API base URL. Intended for testing.
func NewTokenProviderWithKey(appID, installationID int64, key *rsa.PrivateKey, apiBaseURL string) (*TokenProvider, error) {
	if key == nil {
		return nil, fmt.Errorf("private key is nil")
	}

	return &TokenProvider{
		appID:          appID,
		installationID: installationID,
		privateKey:     key,
		apiBaseURL:     apiBaseURL,
		httpClient:     &http.Client{Timeout: tokenRequestTimeout},
	}, nil
}

// installationToken is the relevant fields from GitHub's token response.
type installationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// GenerateToken creates a short-lived installation access token.
// The token is valid for up to 1 hour and scoped to the App's installation.
func (p *TokenProvider) GenerateToken(ctx context.Context) (string, error) {
	jwtToken, err := p.createJWT()
	if err != nil {
		return "", fmt.Errorf("create JWT: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", p.apiBaseURL, p.installationID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request token: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var token installationToken
	if err := json.Unmarshal(body, &token); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}

	if token.Token == "" {
		return "", fmt.Errorf("empty token in response")
	}

	return token.Token, nil
}

// createJWT builds a signed JWT for authenticating as the GitHub App.
func (p *TokenProvider) createJWT() (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    strconv.FormatInt(p.appID, 10),
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)), // clock skew tolerance
		ExpiresAt: jwt.NewNumericDate(now.Add(jwtExpiry)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

	signed, err := token.SignedString(p.privateKey)
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	return signed, nil
}
