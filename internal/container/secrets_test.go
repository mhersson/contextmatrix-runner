package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// testSecretsManager returns a Manager with the filesystem hooks replaced by
// the provided functions. All Docker-related fields are wired to no-ops
// because prepareSecrets does not invoke the Docker client.
func testSecretsManager(t *testing.T, cfg *config.Config, mkdirAll func(string, os.FileMode) error, createFile func(string) (*os.File, error)) *Manager {
	t.Helper()

	m := &Manager{
		cfg:      cfg,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		dnsCache: newDNSCache(dnsCacheTTL, dnsCacheCapacity),
		resolver: net.DefaultResolver,
	}

	if mkdirAll != nil {
		m.mkdirAll = mkdirAll
	} else {
		m.mkdirAll = os.MkdirAll
	}

	if createFile != nil {
		m.createFile = createFile
	} else {
		m.createFile = func(path string) (*os.File, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL|os.O_TRUNC, 0o600)
		}
	}

	return m
}

// devConfig returns a minimal config in dev mode, pointing SecretsDir at a
// temp directory the test owns.
func devConfig(t *testing.T) *config.Config {
	t.Helper()

	return &config.Config{
		DeploymentProfile: config.ProfileDev,
		SecretsDir:        t.TempDir() + "/secrets",
		AnthropicAPIKey:   "sk-test",
	}
}

// prodConfig returns a minimal config in production mode.
func prodConfig(t *testing.T) *config.Config {
	t.Helper()

	return &config.Config{
		DeploymentProfile: config.ProfileProduction,
		SecretsDir:        t.TempDir() + "/secrets",
		AnthropicAPIKey:   "sk-test",
	}
}

// testSecrets is a small set of k/v pairs used across multiple sub-tests.
var testSecrets = map[string]string{
	"GITHUB_TOKEN": "ghp_abc123",
	"API_KEY":      "super-secret",
}

func TestPrepareSecrets(t *testing.T) {
	permErr := os.ErrPermission

	tests := []struct {
		name       string
		cfg        func(*testing.T) *config.Config
		mkdirAll   func(string, os.FileMode) error
		createFile func(string) (*os.File, error)
		wantMode   secretMode
		wantErr    bool
		// wantEnvVars are KEY=VALUE strings that must appear in EnvVars
		// when Mode == secretModeEnvVar.
		wantEnvVars []string
		// wantNoFile asserts that no bind-mount file was created.
		wantNoFile bool
	}{
		{
			name: "dev + writable: uses file mode",
			cfg:  devConfig,
			// Use real filesystem (mkdirAll nil -> os.MkdirAll). SecretsDir
			// points at a TempDir the test already owns, so it is writable.
			mkdirAll:   nil,
			createFile: nil,
			wantMode:   secretModeFile,
		},
		{
			name: "dev + unwritable mkdir: falls back to env-var",
			cfg:  devConfig,
			mkdirAll: func(_ string, _ os.FileMode) error {
				return permErr
			},
			wantMode:    secretModeEnvVar,
			wantEnvVars: []string{"API_KEY=super-secret", "GITHUB_TOKEN=ghp_abc123"},
			wantNoFile:  true,
		},
		{
			name: "dev + unwritable createFile: falls back to env-var",
			cfg:  devConfig,
			// mkdirAll succeeds (real os.MkdirAll), but createFile fails.
			mkdirAll: os.MkdirAll,
			createFile: func(_ string) (*os.File, error) {
				return nil, permErr
			},
			wantMode:    secretModeEnvVar,
			wantEnvVars: []string{"API_KEY=super-secret", "GITHUB_TOKEN=ghp_abc123"},
			wantNoFile:  true,
		},
		{
			name: "production + unwritable mkdir: returns error, no fallback",
			cfg:  prodConfig,
			mkdirAll: func(_ string, _ os.FileMode) error {
				return permErr
			},
			wantErr: true,
		},
		{
			name:     "production + unwritable createFile: returns error, no fallback",
			cfg:      prodConfig,
			mkdirAll: os.MkdirAll,
			createFile: func(_ string) (*os.File, error) {
				return nil, permErr
			},
			wantErr: true,
		},
		{
			name: "non-permission error in dev mode: returns error unchanged",
			cfg:  devConfig,
			mkdirAll: func(_ string, _ os.FileMode) error {
				return errors.New("no space left on device")
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg(t)
			m := testSecretsManager(t, cfg, tc.mkdirAll, tc.createFile)

			payload := RunConfig{CardID: "TEST-001", Project: "proj"}
			delivery, err := m.prepareSecrets(payload, testSecrets)

			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantMode, delivery.Mode)

			switch delivery.Mode {
			case secretModeFile:
				assert.NotEmpty(t, delivery.FilePath)
				assert.Empty(t, delivery.EnvVars)

				// File must exist and be readable.
				b, readErr := os.ReadFile(delivery.FilePath)
				require.NoError(t, readErr)
				assert.Contains(t, string(b), "GITHUB_TOKEN")

				// Clean up.
				_ = os.Remove(delivery.FilePath)

			case secretModeEnvVar:
				assert.Empty(t, delivery.FilePath)
				assert.NotEmpty(t, delivery.EnvVars)

				for _, want := range tc.wantEnvVars {
					assert.Contains(t, delivery.EnvVars, want,
						"env-var delivery must include %s", want)
				}

				if tc.wantNoFile {
					// Confirm no file was left on disk.
					dir := cfg.SecretsDir
					if dir == "" {
						dir = "/var/run/cm-runner/secrets"
					}

					entries, _ := os.ReadDir(dir)
					assert.Empty(t, entries, "no file should have been written in env-var mode")
				}
			}
		})
	}
}

// TestRemoveSecretsFile verifies the no-op semantics for env-var delivery.
func TestRemoveSecretsFile(t *testing.T) {
	t.Run("env-var mode: no-op, no error", func(t *testing.T) {
		cfg := devConfig(t)
		m := testSecretsManager(t, cfg, nil, nil)
		log := slog.New(slog.NewTextHandler(io.Discard, nil))

		// Should not panic or error even though FilePath is empty.
		d := secretDelivery{Mode: secretModeEnvVar}
		m.removeSecretsFile(d, log) // no assertion needed; just must not panic
	})

	t.Run("file mode: removes file", func(t *testing.T) {
		cfg := devConfig(t)
		m := testSecretsManager(t, cfg, nil, nil)

		// Create a real temp file to simulate the secrets file.
		f, err := os.CreateTemp(t.TempDir(), "secrets-*.env")
		require.NoError(t, err)

		_ = f.Close()

		d := secretDelivery{Mode: secretModeFile, FilePath: f.Name()}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		m.removeSecretsFile(d, log)

		_, statErr := os.Stat(f.Name())
		assert.True(t, os.IsNotExist(statErr), "file must be removed")
	})

	t.Run("file mode: already removed is a no-op", func(t *testing.T) {
		cfg := devConfig(t)
		m := testSecretsManager(t, cfg, nil, nil)

		d := secretDelivery{Mode: secretModeFile, FilePath: t.TempDir() + "/nonexistent.env"}
		// Must not log a warning or error when the file is already gone.
		var buf bytes.Buffer

		warnLog := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		m.removeSecretsFile(d, warnLog)
		assert.Empty(t, buf.String(), "no warning expected for already-removed file")
	})
}

// TestPrepareSecrets_WarnLogged verifies that the WARN message is emitted
// when dev mode triggers the env-var fallback.
func TestPrepareSecrets_WarnLogged(t *testing.T) {
	cfg := devConfig(t)

	var logBuf bytes.Buffer

	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(handler)

	m := &Manager{
		cfg:      cfg,
		logger:   logger,
		dnsCache: newDNSCache(dnsCacheTTL, dnsCacheCapacity),
		resolver: net.DefaultResolver,
		mkdirAll: func(_ string, _ os.FileMode) error {
			return os.ErrPermission
		},
		createFile: func(_ string) (*os.File, error) {
			return nil, os.ErrPermission
		},
	}

	payload := RunConfig{CardID: "WARN-001", Project: "proj"}
	delivery, err := m.prepareSecrets(payload, testSecrets)

	require.NoError(t, err)
	assert.Equal(t, secretModeEnvVar, delivery.Mode)
	assert.Contains(t, logBuf.String(), "dev profile",
		"expected WARN log containing 'dev profile', got: %s", logBuf.String())
}

// TestSecretsDelivered_EnvVarMode_IntegrationStartContainer verifies that in
// env-var delivery mode:
//   - secrets appear in container.Config.Env (not hidden via bind-mount)
//   - the /run/cm-secrets/env bind-mount is NOT added to HostConfig.Mounts
//
// It uses a real temp dir for SecretsDir and injects a permission error on
// mkdirAll so the dev-mode fallback is exercised end-to-end through
// prepareSecrets inside startContainer.
func TestSecretsDelivered_EnvVarMode_IntegrationStartContainer(t *testing.T) {
	var (
		capturedEnv    []string
		capturedMounts []string // Target paths of all mounts
	)

	mock := successfulMock()
	mock.ContainerCreateFn = func(_ context.Context, cfg *dockercontainer.Config, hc *dockercontainer.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (dockercontainer.CreateResponse, error) {
		capturedEnv = cfg.Env

		for _, mn := range hc.Mounts {
			capturedMounts = append(capturedMounts, mn.Target)
		}

		return dockercontainer.CreateResponse{ID: "envvar-ctr"}, nil
	}

	devCfg := &config.Config{
		DeploymentProfile: config.ProfileDev,
		BaseImage:         "test-image:latest",
		SecretsDir:        t.TempDir() + "/unwritable",
		AnthropicAPIKey:   "sk-test-api-key",
		ImagePullPolicy:   config.PullAlways,
		ContainerTimeout:  "1h",
	}
	devCfg.ParseContainerTimeout()

	tr := tracker.New()
	m := NewManager(mock, tr, nil, testPATProvider(t), nil, devCfg, testLogger())

	// Override mkdirAll to simulate an unwritable secrets directory.
	m.mkdirAll = func(_ string, _ os.FileMode) error {
		return os.ErrPermission
	}

	payload := RunConfig{
		CardID:  "TEST-042",
		Project: "envvar-proj",
		RepoURL: "https://github.com/org/repo.git",
		MCPURL:  "http://cm:8080/mcp",
	}

	delivery, secretValues, err := startContainerAndCapture(t, m, payload, tr)
	require.NoError(t, err)
	assert.Equal(t, secretModeEnvVar, delivery.Mode)
	assert.NotEmpty(t, secretValues)

	// Secrets must be in Env.
	var foundToken bool

	for _, e := range capturedEnv {
		if strings.HasPrefix(e, "CM_GIT_TOKEN=") {
			foundToken = true
		}
	}

	assert.True(t, foundToken, "CM_GIT_TOKEN must be in container Env for env-var delivery")

	// The /run/cm-secrets/env bind-mount must NOT be present.
	for _, target := range capturedMounts {
		assert.NotEqual(t, secretsMountTarget, target,
			"secrets bind-mount must not be added in env-var delivery mode")
	}
}

// startContainerAndCapture calls startContainer and returns the secretDelivery
// and secretValues so callers can assert on delivery mode without going through
// the full Run() lifecycle.
func startContainerAndCapture(t *testing.T, m *Manager, payload RunConfig, tr *tracker.Tracker) (secretDelivery, []string, error) {
	t.Helper()

	require.NoError(t, tr.Add(&tracker.ContainerInfo{
		CardID:  payload.CardID,
		Project: payload.Project,
	}))

	cid, delivery, secretValues, err := m.startContainer(context.Background(), payload)
	if err == nil {
		// Best-effort cleanup; ignore errors.
		m.removeSecretsFile(delivery, testLogger())

		_ = cid
	}

	return delivery, secretValues, err
}
