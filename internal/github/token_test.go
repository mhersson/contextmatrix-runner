package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func generateTestKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	keyBytes := x509.MarshalPKCS1PrivateKey(key)
	pemBlock := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyBytes})

	path := filepath.Join(t.TempDir(), "test-app.pem")
	require.NoError(t, os.WriteFile(path, pemBlock, 0o600))

	return key, path
}

func TestNewTokenProvider_ValidKey(t *testing.T) {
	_, pemPath := generateTestKey(t)

	tp, err := NewTokenProvider(12345, 67890, pemPath)
	require.NoError(t, err)
	assert.Equal(t, int64(12345), tp.appID)
	assert.Equal(t, int64(67890), tp.installationID)
	assert.NotNil(t, tp.privateKey)
}

func TestNewTokenProvider_InvalidKeyPath(t *testing.T) {
	_, err := NewTokenProvider(1, 1, "/nonexistent/key.pem")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read private key")
}

func TestNewTokenProvider_InvalidKeyData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.pem")
	require.NoError(t, os.WriteFile(path, []byte("not a pem key"), 0o600))

	_, err := NewTokenProvider(1, 1, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse private key")
}

func TestGenerateToken_Success(t *testing.T) {
	key, pemPath := generateTestKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		assert.Equal(t, http.MethodPost, r.Method)
		assert.True(t, strings.HasSuffix(r.URL.Path, "/app/installations/67890/access_tokens"))
		assert.Contains(t, r.Header.Get("Accept"), "github")

		// Verify JWT
		auth := r.Header.Get("Authorization")
		assert.True(t, strings.HasPrefix(auth, "Bearer "))
		jwtStr := strings.TrimPrefix(auth, "Bearer ")

		token, err := jwt.Parse(jwtStr, func(_ *jwt.Token) (any, error) {
			return &key.PublicKey, nil
		})
		assert.NoError(t, err)
		assert.True(t, token.Valid)

		issuer, _ := token.Claims.GetIssuer()
		assert.Equal(t, "12345", issuer)

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ghs_test_token_123",
			"expires_at": "2030-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	tp, err := NewTokenProvider(12345, 67890, pemPath)
	require.NoError(t, err)

	tp.apiBaseURL = srv.URL

	token, err := tp.GenerateToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_test_token_123", token)
}

func TestGenerateToken_GitHubError(t *testing.T) {
	_, pemPath := generateTestKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	tp, err := NewTokenProvider(12345, 67890, pemPath)
	require.NoError(t, err)

	tp.apiBaseURL = srv.URL

	_, err = tp.GenerateToken(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestGenerateToken_EmptyToken(t *testing.T) {
	_, pemPath := generateTestKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"","expires_at":"2030-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	tp, err := NewTokenProvider(12345, 67890, pemPath)
	require.NoError(t, err)

	tp.apiBaseURL = srv.URL

	_, err = tp.GenerateToken(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty token")
}

func TestGenerateToken_ContextCanceled(t *testing.T) {
	_, pemPath := generateTestKey(t)

	tp, err := NewTokenProvider(12345, 67890, pemPath)
	require.NoError(t, err)

	tp.apiBaseURL = "http://localhost:1" // unreachable

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = tp.GenerateToken(ctx)
	require.Error(t, err)
}

func TestNewTokenProvider_DefaultBaseURL(t *testing.T) {
	_, pemPath := generateTestKey(t)

	tp, err := NewTokenProvider(12345, 67890, pemPath)
	require.NoError(t, err)
	assert.Equal(t, "https://api.github.com", tp.apiBaseURL)
}

func TestNewTokenProvider_WithAPIBaseURL(t *testing.T) {
	key, pemPath := generateTestKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.True(t, strings.HasSuffix(r.URL.Path, "/app/installations/67890/access_tokens"))

		// Verify JWT issuer
		auth := r.Header.Get("Authorization")
		assert.True(t, strings.HasPrefix(auth, "Bearer "))
		jwtStr := strings.TrimPrefix(auth, "Bearer ")
		token, err := jwt.Parse(jwtStr, func(_ *jwt.Token) (any, error) {
			return &key.PublicKey, nil
		})
		assert.NoError(t, err)
		assert.True(t, token.Valid)

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ghs_option_token",
			"expires_at": "2030-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	tp, err := NewTokenProvider(12345, 67890, pemPath, WithAPIBaseURL(srv.URL))
	require.NoError(t, err)
	assert.Equal(t, srv.URL, tp.apiBaseURL)

	tok, err := tp.GenerateToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_option_token", tok)
}

func TestWithAPIBaseURL_Empty(t *testing.T) {
	_, pemPath := generateTestKey(t)

	tp, err := NewTokenProvider(1, 1, pemPath, WithAPIBaseURL(""))
	require.NoError(t, err)
	// Empty option must be a no-op; default must be preserved.
	assert.Equal(t, "https://api.github.com", tp.apiBaseURL)
}

func TestWithAPIBaseURL_TrimsTrailingSlash(t *testing.T) {
	_, pemPath := generateTestKey(t)

	tp, err := NewTokenProvider(1, 1, pemPath, WithAPIBaseURL("https://api.acme.ghe.com/"))
	require.NoError(t, err)
	assert.Equal(t, "https://api.acme.ghe.com", tp.apiBaseURL)
}

func TestCreateJWT_Claims(t *testing.T) {
	_, pemPath := generateTestKey(t)

	tp, err := NewTokenProvider(99999, 11111, pemPath)
	require.NoError(t, err)

	jwtStr, err := tp.createJWT()
	require.NoError(t, err)
	assert.NotEmpty(t, jwtStr)

	// Parse without verification to inspect claims
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	token, _, err := parser.ParseUnverified(jwtStr, &jwt.RegisteredClaims{})
	require.NoError(t, err)

	claims := token.Claims.(*jwt.RegisteredClaims)
	assert.Equal(t, "99999", claims.Issuer)
	assert.NotNil(t, claims.IssuedAt)
	assert.NotNil(t, claims.ExpiresAt)
}
