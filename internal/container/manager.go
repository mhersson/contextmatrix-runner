package container

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/pkg/stdcopy"

	githubauth "github.com/mhersson/contextmatrix-githubauth"

	"github.com/mhersson/contextmatrix-runner/internal/callback"
	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/mhersson/contextmatrix-runner/internal/logparser"
	"github.com/mhersson/contextmatrix-runner/internal/metrics"
	"github.com/mhersson/contextmatrix-runner/internal/streammsg"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// RunConfig holds the parameters needed to start a container.
// This mirrors the webhook TriggerPayload but avoids an import cycle.
type RunConfig struct {
	CardID        string
	Project       string
	RepoURL       string
	MCPURL        string
	MCPAPIKey     string
	BaseBranch    string
	RunnerImage   string
	Interactive   bool
	Model         string
	CorrelationID string
	// TaskSkills is the optional set of skill names to activate in the container.
	// nil means "no constraint" (all skills); non-nil (even empty) means the
	// set is explicit. The entrypoint uses CM_TASK_SKILLS_SET to distinguish
	// the two cases.
	TaskSkills *[]string
}

const (
	// LabelRunner marks containers managed by contextmatrix-runner.
	LabelRunner = "contextmatrix.runner"
	// LabelCardID stores the card ID on the container.
	LabelCardID = "contextmatrix.card_id"
	// LabelProject stores the project name on the container.
	LabelProject = "contextmatrix.project"

	imagePullTimeout = 5 * time.Minute
	stopGracePeriod  = 10 // seconds
	// callbackTimeout bounds the detached context used to deliver a status
	// callback after the parent ctx has already been cancelled (e.g. on a Kill
	// or start failure race). See CTXRUN-050.
	callbackTimeout = 10 * time.Second

	// dockerCleanupTimeout bounds the detached contexts used for
	// best-effort Docker cleanup (Stop / Remove / Kill) that must run
	// even when the parent ctx has already been cancelled. A hung
	// dockerd used to stall shutdown forever; now every such call has
	// a hard cap. See CTXRUN-040.
	dockerCleanupTimeout = 5 * time.Second
)

// primingWriteTimeout bounds the priming WriteStdin call made right after
// ContainerAttach. A wedged hijacked socket used to hang the Run goroutine
// indefinitely; now the goroutine gives up after this deadline and continues
// into waitAndCleanup so normal cancellation paths work. See CTXRUN-040 (C11).
//
// Declared as a package var (not a const) so tests can shrink it to keep
// synthetic-wedge cases fast.
var primingWriteTimeout = 5 * time.Second

// logDrainTimeout bounds the <-logDone wait on the cancel, timeout, and
// error branches of waitAndCleanup. A hung log-streaming goroutine (wedged
// docker daemon, stuck hijacked socket, or stdcopy/scanner stall) used to
// stall those branches indefinitely, preventing the cleanup defers from
// running and leaking the container until container_timeout (2h default).
// Declared as a package var so tests can shrink it without waiting 5s of
// wall time per synthetic-wedge scenario.
var logDrainTimeout = 5 * time.Second

// pullSkillsRepo runs `git pull --ff-only` in dir, authenticating against the
// configured upstream with a freshly minted GitHub token when the remote is
// HTTPS. Returns nil if dir is not a git repo (operator may have a non-tracked
// local clone) or has no `origin` remote configured. Returns the git error
// otherwise — caller should log and continue, not abort.
var pullSkillsRepo = func(ctx context.Context, dir, token string) error {
	gitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		if os.IsNotExist(err) {
			return nil // not tracked; skip
		}

		return fmt.Errorf("stat %s: %w", gitDir, err)
	}

	cmd := exec.CommandContext(ctx, "git", "-C", dir, "pull", "--ff-only")
	cmd.Env = pullSkillsEnv(ctx, dir, token)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git pull: %w (output: %s)", err, out)
	}

	return nil
}

// pullSkillsEnv returns the env for the `git pull` invocation. When the
// repo's `origin` remote is HTTPS, it injects an HTTP Basic authorization
// header with username `x-access-token` (GitHub's documented pattern for
// both App installation tokens and PATs) via GIT_CONFIG_COUNT/KEY_0/VALUE_0
// so the token never appears in the process command line. For any other
// remote, or if the URL cannot be read, the parent env is returned
// unchanged and `git pull` runs with no injected auth.
func pullSkillsEnv(ctx context.Context, dir, token string) []string {
	parent := os.Environ()

	out, err := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		// `git config --get` exits 1 when the key is unset; either way we
		// just run plain git pull and let the caller's WARN handle any
		// failure.
		return parent
	}

	remoteURL := strings.TrimSpace(string(out))
	if remoteURL == "" {
		return parent
	}

	u, err := url.Parse(remoteURL)
	if err != nil || strings.ToLower(u.Scheme) != "https" || u.Host == "" {
		return parent
	}

	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))

	return append(parent,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://"+u.Host+"/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth,
	)
}

// imagePruneMaxAge is the "until" filter passed to ImagesPrune so the
// maintenance loop only reclaims images older than this. Keeping it at 24h
// means freshly-pulled worker images are never a prune target mid-run.
const imagePruneMaxAge = "24h"

// dnsLookupTimeout bounds buildExtraHosts' resolver call. An attacker who
// points the card's MCP URL at a slow-responding authoritative server used
// to be able to stall the spawn path indefinitely (H24); the deadline caps
// exposure at 2s and then falls back to running the container without the
// ExtraHosts entry. The container can still reach the MCP server via the
// normal host-gateway route if DNS inside the container itself works.
const dnsLookupTimeout = 2 * time.Second

// secretMode describes how secrets are delivered to a container.
type secretMode int

const (
	// secretModeFile delivers secrets via a host-side tmpfs file that is
	// bind-mounted read-only into the container at /run/cm-secrets/env.
	secretModeFile secretMode = iota
	// secretModeEnvVar delivers secrets directly via HostConfig.Env. Used as a
	// dev-mode fallback when secrets_dir is not writable.
	secretModeEnvVar
)

// secretDelivery describes the result of prepareSecrets: either a host-side
// file path (Mode == secretModeFile) or a slice of KEY=VALUE strings ready to
// be appended to the container env (Mode == secretModeEnvVar).
type secretDelivery struct {
	Mode     secretMode
	FilePath string   // set when Mode == secretModeFile
	EnvVars  []string // set when Mode == secretModeEnvVar
}

// Manager handles the lifecycle of Docker containers for task execution.
type Manager struct {
	docker      DockerClient
	tracker     *tracker.Tracker
	callback    *callback.Client
	token       githubauth.TokenGenerator
	broadcaster *logbroadcast.Broadcaster
	cfg         *config.Config
	logger      *slog.Logger
	metrics     *metrics.Metrics
	// dnsCache memoises buildExtraHosts() resolver calls so a spawn burst
	// against the same MCP hostname doesn't pay N DNS RTTs. Lifetime is
	// process-long; TTL is dnsCacheTTL.
	dnsCache *dnsCache
	// resolver is the DNS resolver used by buildExtraHosts. Swappable for
	// tests; nil is treated as net.DefaultResolver.
	resolver hostResolver
	wg       sync.WaitGroup

	// mkdirAll is os.MkdirAll by default; swappable in tests to inject
	// filesystem errors without touching the real filesystem.
	mkdirAll func(path string, perm os.FileMode) error
	// createFile is os.OpenFile(path, O_CREATE|O_WRONLY|O_EXCL|O_TRUNC, 0o600)
	// by default; swappable in tests to inject file-creation errors.
	createFile func(path string) (*os.File, error)
}

// WithMetrics attaches a metrics bundle to the manager. A nil bundle disables
// metric observation (useful in tests that do not care about Prometheus).
func (m *Manager) WithMetrics(mx *metrics.Metrics) *Manager {
	m.metrics = mx

	return m
}

// NewManager creates a container manager.
func NewManager(
	docker DockerClient,
	tracker *tracker.Tracker,
	cb *callback.Client,
	token githubauth.TokenGenerator,
	broadcaster *logbroadcast.Broadcaster,
	cfg *config.Config,
	logger *slog.Logger,
) *Manager {
	return &Manager{
		docker:      docker,
		tracker:     tracker,
		callback:    cb,
		token:       token,
		broadcaster: broadcaster,
		cfg:         cfg,
		logger:      logger,
		dnsCache:    newDNSCache(dnsCacheTTL, dnsCacheCapacity),
		resolver:    net.DefaultResolver,
		mkdirAll:    os.MkdirAll,
		createFile: func(path string) (*os.File, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL|os.O_TRUNC, 0o600)
		},
	}
}

// withCleanupTimeout returns a fresh, parent-detached context bounded by
// callbackTimeout. Used by all cleanup / callback code paths that MUST run
// even when the caller's ctx has already been cancelled (e.g. shutdown,
// container-kill). The parent is intentionally not plumbed in: inheriting a
// cancelled parent would turn every callback into a no-op, which is exactly
// the hang CTXRUN-040 is fixing.
//
// Callers must defer the returned CancelFunc.
func withCleanupTimeout(_ context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), callbackTimeout)
}

// withDockerCleanupTimeout is like withCleanupTimeout but bounded by the
// shorter dockerCleanupTimeout. Used for Docker Stop/Remove/Kill calls where
// we'd rather give up quickly and move on than wait for a hung daemon.
func withDockerCleanupTimeout(_ context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dockerCleanupTimeout)
}

// Run launches the full container lifecycle for a triggered task in a goroutine.
// Use Wait to block until all launched goroutines have finished.
func (m *Manager) Run(ctx context.Context, payload RunConfig) {
	started := time.Now()

	m.wg.Go(func() {
		outcome := metrics.OutcomeSuccess

		defer func() {
			if r := recover(); r != nil {
				outcome = metrics.OutcomeFailure

				if m.metrics != nil {
					m.metrics.PanicRecoveredTotal.WithLabelValues(metrics.GoroutineRun).Inc()
				}

				m.logger.Error("container run panicked",
					"panic", r, "card_id", payload.CardID, "project", payload.Project,
					"stack", string(debug.Stack()))

				// H22: close the partial-failure window. If startContainer had
				// already returned a container ID and then something downstream
				// panicked before waitAndCleanup installed its defers, the
				// Docker container would leak because tracker.Remove alone only
				// clears the in-memory entry. Look up the ID from the tracker
				// (UpdateContainerID was called between startContainer and
				// waitAndCleanup) and Force-remove the container on a fresh,
				// bounded ctx.
				if snap, ok := m.tracker.Snapshot(payload.Project, payload.CardID); ok && snap.ContainerID != "" {
					rmCtx, rmCancel := withDockerCleanupTimeout(context.Background())
					if rmErr := m.docker.ContainerRemove(rmCtx, snap.ContainerID, container.RemoveOptions{Force: true}); rmErr != nil {
						m.logger.Warn("panic recovery: docker remove failed",
							"container_id", snap.ContainerID,
							"card_id", payload.CardID,
							"error", rmErr)
					}

					rmCancel()
				}

				m.tracker.Remove(payload.Project, payload.CardID)

				cbCtx, cbCancel := withCleanupTimeout(context.Background())
				m.reportFailure(cbCtx, payload, fmt.Sprintf("internal error: %v", r))
				cbCancel()
			}

			if m.metrics != nil {
				m.metrics.ContainerDuration.WithLabelValues(outcome).Observe(time.Since(started).Seconds())
			}
		}()

		outcome = m.run(ctx, payload)
	})
}

// Wait blocks until all container goroutines launched by Run have finished.
func (m *Manager) Wait() {
	m.wg.Wait()
}

func (m *Manager) run(ctx context.Context, payload RunConfig) string {
	log := m.logger.With("card_id", payload.CardID, "project", payload.Project)

	containerID, delivery, secretValues, err := m.startContainer(ctx, payload)
	if err != nil {
		log.Error("failed to start container", "error", err)
		m.removeSecretsFile(delivery, log)

		// Use a detached context for the callback: if start raced with a Kill
		// (or the runner is shutting down), ctx is already cancelled and the
		// HTTP request would be a no-op, leaving CM blind to the failure.
		// The outer flow still honours parent cancellation — only the
		// "tell CM we failed" side-effect gets a fresh deadline. See CTXRUN-050.
		cbCtx, cancel := withCleanupTimeout(ctx)
		defer cancel()

		m.reportFailure(cbCtx, payload, fmt.Sprintf("start failed: %v", err))
		m.tracker.Remove(payload.Project, payload.CardID)

		return metrics.OutcomeFailure
	}

	m.tracker.UpdateContainerID(payload.Project, payload.CardID, containerID)

	// Emit system event: container started.
	if m.broadcaster != nil {
		m.broadcaster.Publish(logbroadcast.LogEntry{
			Timestamp: time.Now(),
			CardID:    payload.CardID,
			Project:   payload.Project,
			Type:      "system",
			Content:   "container started",
		})
	}

	// Report running status to CM asynchronously. The callback can take up
	// to ~37s on sustained 5xx/backoff, and blocking the spawn path on it
	// prevents streamLogs from draining stdout so the container stalls on
	// kernel buffer pressure. CTXRUN-059 (H23): fire and forget on a
	// detached 30s context with a panic-safe wrapper.
	m.runningCallbackAsync(payload, log)

	// Wait for container to finish and return its outcome so the caller's
	// duration histogram carries the right label.
	return m.waitAndCleanup(ctx, containerID, delivery, payload, secretValues, log)
}

// runningCallbackAsync fires ReportStatus("running") on a background
// goroutine with a fresh 30s ctx. The parent ctx is intentionally NOT
// inherited: if the parent is already near-cancel (e.g. rapid Kill after
// spawn), the callback would become a no-op and CM would see the card
// stuck. Panics are recovered and counted so one bad callback cannot take
// down the runner.
func (m *Manager) runningCallbackAsync(payload RunConfig, log *slog.Logger) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if m.metrics != nil {
					m.metrics.PanicRecoveredTotal.WithLabelValues(metrics.GoroutineRun).Inc()
				}

				m.logger.Error("running-status callback panicked",
					"panic", r,
					"card_id", payload.CardID,
					"project", payload.Project,
					"stack", string(debug.Stack()))
			}
		}()

		// 30s is generous enough to absorb the full 3-attempt retry ladder
		// (1s + 2s + 4s backoff + per-request 10s timeout each) while
		// still bounding the goroutine's lifetime.
		cbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := m.callback.ReportStatus(cbCtx, payload.CardID, payload.Project, "running", "container started"); err != nil {
			log.Warn("failed to report running status (async)", "error", err)
		}
	}()
}

// secretsMountTarget is the in-container path where the per-container
// secrets file is bind-mounted read-only. It is a PATH, not a credential;
// the gosec G101 flag is a false positive.
const secretsMountTarget = "/run/cm-secrets/env" //nolint:gosec // path, not a credential

func (m *Manager) startContainer(ctx context.Context, payload RunConfig) (string, secretDelivery, []string, error) {
	img := payload.RunnerImage
	if img == "" {
		img = m.cfg.BaseImage
	}

	// Validate image against allowlist.
	if len(m.cfg.AllowedImages) > 0 {
		if !slices.Contains(m.cfg.AllowedImages, img) {
			return "", secretDelivery{}, nil, fmt.Errorf("image %q not in allowed_images list", img)
		}
	} else if img != m.cfg.BaseImage {
		return "", secretDelivery{}, nil, fmt.Errorf("image %q not allowed (only base_image %q permitted when allowed_images is empty)", img, m.cfg.BaseImage)
	}

	// Pull image according to policy.
	if err := m.pullImage(ctx, img); err != nil {
		return "", secretDelivery{}, nil, err
	}

	// Generate GitHub App token. Expiry is discarded for now — the runner
	// mints fresh per spawn and hands the token off to the container.
	gitToken, _, err := m.token.GenerateToken(ctx)
	if err != nil {
		return "", secretDelivery{}, nil, fmt.Errorf("generate git token: %w", err)
	}

	// Secrets (CM_GIT_TOKEN, CM_MCP_API_KEY, CLAUDE_CODE_OAUTH_TOKEN,
	// ANTHROPIC_API_KEY) are written to a mode-0600 file on a tmpfs dir
	// and bind-mounted read-only into the container at
	// /run/cm-secrets/env. This keeps them out of HostConfig.Env (so
	// `docker inspect` cannot leak them) and out of PID 1's initial
	// /proc/1/environ. The same values are also collected into
	// secretValues so the per-container Redactor can mask their literal
	// forms in output (CTXRUN-049).
	secrets := map[string]string{
		"CM_GIT_TOKEN": gitToken,
	}
	if payload.MCPAPIKey != "" {
		secrets["CM_MCP_API_KEY"] = payload.MCPAPIKey
	}

	// Build environment variables — NON-SECRET ONLY.
	env := []string{
		"CM_CARD_ID=" + payload.CardID,
		"CM_PROJECT=" + payload.Project,
		"CM_MCP_URL=" + payload.MCPURL,
		"CM_REPO_URL=" + normalizeRepoURL(payload.RepoURL),
	}

	if payload.CorrelationID != "" {
		env = append(env, "CM_CORRELATION_ID="+payload.CorrelationID)
	}

	if payload.BaseBranch != "" {
		env = append(env, "CM_BASE_BRANCH="+payload.BaseBranch)
	}

	if payload.Interactive {
		env = append(env, "CM_INTERACTIVE=1")
	}

	if payload.Model != "" {
		env = append(env, "CM_ORCHESTRATOR_MODEL="+payload.Model)
	}

	if m.cfg.ClaudeSettings != "" {
		env = append(env, "CM_CLAUDE_SETTINGS="+m.cfg.ClaudeSettings)
	}

	if m.cfg.TaskSkillsDir != "" && payload.TaskSkills != nil {
		env = append(env, "CM_TASK_SKILLS_SET=1")
		env = append(env, "CM_TASK_SKILLS="+strings.Join(*payload.TaskSkills, ","))
	}

	// Worker-extra env vars (deployment-wide, see Config.WorkerExtraEnv).
	// Sorted for deterministic ordering — useful in tests and reproducible
	// `docker inspect` output. Validation in config.Validate ensures keys
	// are well-formed and don't shadow secrets-file vars.
	if len(m.cfg.WorkerExtraEnv) > 0 {
		extraKeys := make([]string, 0, len(m.cfg.WorkerExtraEnv))
		for k := range m.cfg.WorkerExtraEnv {
			extraKeys = append(extraKeys, k)
		}

		sort.Strings(extraKeys)

		for _, k := range extraKeys {
			env = append(env, k+"="+m.cfg.WorkerExtraEnv[k])
		}
	}

	// Apply highest-priority Claude auth method only.
	// Priority: claude_auth_dir > claude_oauth_token > anthropic_api_key.
	var mounts []mount.Mount

	switch {
	case m.cfg.ClaudeAuthDir != "":
		// Mount the auth directory; no auth env vars injected.
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.cfg.ClaudeAuthDir,
			Target:   "/claude-auth",
			ReadOnly: true,
		})
	case m.cfg.ClaudeOAuthToken != "":
		secrets["CLAUDE_CODE_OAUTH_TOKEN"] = m.cfg.ClaudeOAuthToken
	case m.cfg.AnthropicAPIKey != "":
		secrets["ANTHROPIC_API_KEY"] = m.cfg.AnthropicAPIKey
	}

	if m.cfg.TaskSkillsDir != "" {
		if err := pullSkillsRepo(ctx, m.cfg.TaskSkillsDir, gitToken); err != nil {
			slog.Warn("task skills pull failed; using existing local clone",
				"task_skills_dir", m.cfg.TaskSkillsDir,
				"error", err,
			)
		}

		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.cfg.TaskSkillsDir,
			Target:   "/host-skills",
			ReadOnly: true,
		})
	}

	// Collect secret values (deterministic order, sorted by key) for the
	// per-container Redactor so output redacts literal values in addition
	// to the static KEY=... patterns.
	secretKeys := make([]string, 0, len(secrets))
	for k := range secrets {
		secretKeys = append(secretKeys, k)
	}

	sort.Strings(secretKeys)

	secretValues := make([]string, 0, len(secrets))

	for _, k := range secretKeys {
		secretValues = append(secretValues, secrets[k])
	}

	// Prepare secret delivery: try writing to a tmpfs file first. In dev mode,
	// fall back to env-var delivery if the secrets dir is not writable.
	delivery, err := m.prepareSecrets(payload, secrets)
	if err != nil {
		return "", secretDelivery{}, nil, fmt.Errorf("prepare secrets: %w", err)
	}

	// Wire the delivery into the container configuration.
	switch delivery.Mode {
	case secretModeFile:
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   delivery.FilePath,
			Target:   secretsMountTarget,
			ReadOnly: true,
		})
	case secretModeEnvVar:
		// Secrets land directly in env; no bind-mount added.
		env = append(env, delivery.EnvVars...)
	}

	name := sanitizeContainerName(payload.Project, payload.CardID)

	containerCfg := &container.Config{
		Image: img,
		Env:   env,
		Labels: map[string]string{
			LabelRunner:  "true",
			LabelCardID:  payload.CardID,
			LabelProject: payload.Project,
		},
	}
	if payload.Interactive {
		containerCfg.OpenStdin = true
		containerCfg.AttachStdin = true
		// Tty and StdinOnce default to false; leaving them zero-valued.
	}

	resp, err := m.docker.ContainerCreate(ctx,
		containerCfg,
		&container.HostConfig{
			Mounts:      mounts,
			ExtraHosts:  m.buildExtraHosts(ctx, payload.MCPURL),
			CapDrop:     strslice.StrSlice{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			Resources: container.Resources{
				Memory:    m.cfg.ContainerMemoryLimit,
				PidsLimit: &m.cfg.ContainerPidsLimit,
			},
		},
		nil, nil, name,
	)
	if err != nil {
		return "", delivery, nil, fmt.Errorf("create container: %w", err)
	}

	if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up the created-but-not-started container.
		if rmErr := m.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true}); rmErr != nil {
			m.logger.Warn("failed to remove container after start failure", "container_id", resp.ID, "error", rmErr)
		}

		return "", delivery, nil, fmt.Errorf("start container: %w", err)
	}

	if payload.Interactive {
		attached, err := m.docker.ContainerAttach(ctx, resp.ID, container.AttachOptions{
			Stream: true,
			Stdin:  true,
			Stdout: false,
			Stderr: false,
		})
		if err != nil {
			m.logger.Warn("failed to attach stdin to container", "container_id", resp.ID, "error", err)
		} else {
			m.tracker.SetStdin(payload.Project, payload.CardID, attached.Conn, attached.Close)

			// Write the priming stream-json user message so Claude begins work
			// immediately without waiting for a human to type something first.
			content := buildPrimingContent(payload)

			b, buildErr := streammsg.BuildUserMessage(content)
			if buildErr != nil {
				m.logger.Warn("failed to build priming message",
					"container_id", truncateID(resp.ID),
					"card_id", payload.CardID,
					"project", payload.Project,
					"error", buildErr)
			} else {
				// CTXRUN-040 (C11): wrap the priming WriteStdin with a
				// deadline. The hijacked net.Conn can wedge on kernel
				// buffer pressure, a slow container, or a misbehaving
				// proxy, and a synchronous write used to block the Run
				// goroutine forever. We can't reach through
				// tracker.WriteStdin to set a net.Conn write deadline (the
				// writer is behind an io.WriteCloser interface and mocks
				// in tests wouldn't honour it anyway). Instead: spawn the
				// write, time out after primingWriteTimeout, and on
				// timeout close the underlying writer directly — this
				// unblocks the wedged Write inside tracker.WriteStdin so
				// stdin.mu gets released and the normal cleanup path can
				// make progress.
				m.writePrimingWithTimeout(payload, resp.ID, b, attached.Conn)
			}
		}
	}

	return resp.ID, delivery, secretValues, nil
}

// isPermissionDenied returns true when err is (or wraps) os.ErrPermission or
// syscall.EROFS — the two error values that indicate a read-only or
// unwritable filesystem rather than a transient I/O problem.
func isPermissionDenied(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// prepareSecrets decides how secrets are delivered to the container.
//
//   - Tries to write a mode-0600 file under cfg.SecretsDir (file mode).
//   - If the directory cannot be created or the file cannot be opened due to
//     os.ErrPermission / syscall.EROFS AND the runner is in dev mode, falls
//     back to env-var delivery with a WARN log.
//   - Any other error, or the same permission error in production mode, is
//     returned unchanged so the caller fails closed.
func (m *Manager) prepareSecrets(payload RunConfig, secrets map[string]string) (secretDelivery, error) {
	dir := m.cfg.SecretsDir
	if dir == "" {
		dir = "/var/run/cm-runner/secrets" //nolint:gosec // path, not a credential
	}

	if err := m.mkdirAll(dir, 0o700); err != nil {
		if isPermissionDenied(err) && m.cfg.IsDev() {
			m.logger.Warn("dev profile: secrets_dir not writable, falling back to env-var delivery",
				"dir", dir, "error", err)

			return secretDelivery{Mode: secretModeEnvVar, EnvVars: secretsToEnvVars(secrets)}, nil
		}

		return secretDelivery{}, fmt.Errorf("create secrets dir: %w", err)
	}

	// A 16-byte random nonce avoids collisions if the same card_id is
	// re-triggered while a previous file is still being unlinked.
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return secretDelivery{}, fmt.Errorf("generate secrets nonce: %w", err)
	}

	base := sanitizeContainerName(payload.Project, payload.CardID) + "-" + hex.EncodeToString(nonce[:]) + ".env"
	path := filepath.Join(dir, base)

	f, err := m.createFile(path)
	if err != nil {
		if isPermissionDenied(err) && m.cfg.IsDev() {
			m.logger.Warn("dev profile: secrets_dir not writable, falling back to env-var delivery",
				"dir", dir, "error", err)

			return secretDelivery{Mode: secretModeEnvVar, EnvVars: secretsToEnvVars(secrets)}, nil
		}

		return secretDelivery{}, fmt.Errorf("open secrets file: %w", err)
	}

	// Deterministic iteration for stable tests and reviewable diffs.
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		if _, werr := fmt.Fprintf(f, "export %s='%s'\n", k, shellSingleQuoteEscape(secrets[k])); werr != nil {
			_ = f.Close()
			_ = os.Remove(path)

			return secretDelivery{}, fmt.Errorf("write secret %s: %w", k, werr)
		}
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(path)

		return secretDelivery{}, fmt.Errorf("close secrets file: %w", err)
	}

	return secretDelivery{Mode: secretModeFile, FilePath: path}, nil
}

// secretsToEnvVars converts a secrets map to a sorted slice of KEY=VALUE
// strings suitable for appending to container.Config.Env.
func secretsToEnvVars(secrets map[string]string) []string {
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	vars := make([]string, 0, len(secrets))
	for _, k := range keys {
		vars = append(vars, k+"="+secrets[k])
	}

	return vars
}

// shellSingleQuoteEscape returns s with every `'` replaced by `'\”` so the
// result can be safely embedded inside a single-quoted shell string.
func shellSingleQuoteEscape(s string) string {
	return strings.ReplaceAll(s, `'`, `'\''`)
}

// removeSecretsFile best-effort unlinks the per-container secrets file from
// the host. No-op for env-var delivery (nothing to unlink) or if the file
// path is empty.
func (m *Manager) removeSecretsFile(d secretDelivery, log *slog.Logger) {
	if d.Mode == secretModeEnvVar || d.FilePath == "" {
		return
	}

	if err := os.Remove(d.FilePath); err != nil && !os.IsNotExist(err) {
		log.Warn("failed to remove secrets file", "path", d.FilePath, "error", err)
	}
}

// buildPrimingContent returns the text of the priming stream-json user message
// sent to the container on interactive start. The message instructs Claude to
// begin executing the card immediately via the create-plan skill.
func buildPrimingContent(payload RunConfig) string {
	content := fmt.Sprintf(
		"Begin work on card `%s` now. "+
			"Call `get_skill(skill_name='create-plan', card_id='%s', caller_model='sonnet')` "+
			"via the contextmatrix MCP server and follow the returned skill instructions exactly. "+
			"Use MCP tools only. Never push to main or master. "+
			"Call heartbeat every 5 minutes during idle waits and `report_usage` after each heartbeat. "+
			"On completion, call `release_card` after transitioning to done.",
		payload.CardID, payload.CardID,
	)
	if payload.BaseBranch != "" {
		content += fmt.Sprintf(
			" The base branch for this task is %s; create PRs targeting that branch.",
			payload.BaseBranch,
		)
	}

	return content
}

func (m *Manager) waitAndCleanup(ctx context.Context, containerID string, delivery secretDelivery, payload RunConfig, secrets []string, log *slog.Logger) string {
	// Defers run LIFO, so the declared order here is the REVERSE of the
	// execution order. We want the tracker entry to disappear first so
	// `/message`, `/promote`, and `/end-session` requests that race with
	// cleanup return 404 (no container tracked) rather than 500 (stdin
	// write against a dead container). H21.
	//
	// Actual execution order:
	//   1. tracker.Remove     — unpublish the entry (also closes stdin).
	//   2. removeSecretsFile  — unlink the host-side per-container secrets file.
	//   3. removeContainer    — delete the Docker container (CTXRUN-040:
	//      bounded ctx so a hung dockerd cannot stall the goroutine).
	defer func() {
		rmCtx, cancel := withDockerCleanupTimeout(context.Background())
		defer cancel()

		m.removeContainer(rmCtx, containerID, log)
	}()
	defer m.removeSecretsFile(delivery, log)
	defer m.tracker.Remove(payload.Project, payload.CardID)

	// Stream container logs in real time.
	logDone := m.streamLogs(ctx, containerID, payload, secrets, log)

	// Apply container timeout.
	timeout := m.cfg.ContainerTimeoutDuration()

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	waitCh, errCh := m.docker.ContainerWait(waitCtx, containerID, container.WaitConditionNotRunning)

	select {
	case result := <-waitCh:
		<-logDone // drain remaining log output

		if result.StatusCode != 0 {
			msg := fmt.Sprintf("container exited with code %d", result.StatusCode)
			log.Warn(msg, "exit_code", result.StatusCode)
			m.emitSystem(payload, "container failed: "+msg)

			cbCtx, cbCancel := withCleanupTimeout(ctx)
			m.reportFailure(cbCtx, payload, msg)
			cbCancel()

			return metrics.OutcomeFailure
		}

		log.Info("container completed successfully")
		m.emitSystem(payload, "container completed")

		cbCtx, cbCancel := withCleanupTimeout(ctx)
		m.reportCompleted(cbCtx, payload)
		cbCancel()

		return metrics.OutcomeSuccess

	case err := <-errCh:
		if waitCtx.Err() != nil {
			// Timeout.
			msg := fmt.Sprintf("container timed out after %s", timeout)
			log.Warn(msg)

			killCtx, killCancel := withDockerCleanupTimeout(ctx)
			m.killContainer(killCtx, containerID, log)
			killCancel()

			select {
			case <-logDone:
			case <-time.After(logDrainTimeout):
				log.Warn("log drain timed out during cleanup",
					"container_id", truncateID(containerID),
					"card_id", payload.CardID,
					"project", payload.Project,
					"timeout", logDrainTimeout)
			}

			m.emitSystem(payload, "container failed: "+msg)

			cbCtx, cbCancel := withCleanupTimeout(ctx)
			m.reportFailure(cbCtx, payload, msg)
			cbCancel()

			return metrics.OutcomeTimeout
		}

		msg := fmt.Sprintf("wait error: %v", err)
		log.Error(msg)

		killCtx, killCancel := withDockerCleanupTimeout(ctx)
		m.killContainer(killCtx, containerID, log)
		killCancel()

		select {
		case <-logDone:
		case <-time.After(logDrainTimeout):
			log.Warn("log drain timed out during cleanup",
				"container_id", truncateID(containerID),
				"card_id", payload.CardID,
				"project", payload.Project,
				"timeout", logDrainTimeout)
		}

		m.emitSystem(payload, "container failed: "+msg)

		cbCtx, cbCancel := withCleanupTimeout(ctx)
		m.reportFailure(cbCtx, payload, msg)
		cbCancel()

		return metrics.OutcomeFailure

	case <-ctx.Done():
		// Parent context canceled (e.g., kill or shutdown).
		log.Info("container canceled")

		killCtx, killCancel := withDockerCleanupTimeout(ctx)
		m.killContainer(killCtx, containerID, log)
		killCancel()

		select {
		case <-logDone:
		case <-time.After(logDrainTimeout):
			log.Warn("log drain timed out during cleanup",
				"container_id", truncateID(containerID),
				"card_id", payload.CardID,
				"project", payload.Project,
				"timeout", logDrainTimeout)
		}

		m.emitSystem(payload, "container canceled")

		// Report failure to CM via a detached context: the parent ctx is
		// already cancelled, so passing it to ReportStatus would turn the
		// callback into a no-op and CM would see the card stuck in
		// `running` forever. See CTXRUN-050.
		cbCtx, cbCancel := withCleanupTimeout(ctx)
		m.reportFailure(cbCtx, payload, "killed by operator")
		cbCancel()

		return metrics.OutcomeKilled
	}
}

// streamLogs follows container stdout/stderr and logs each line. The returned
// channel is closed when the stream ends (container exit or context cancel).
// secrets holds the live secret values injected into this container's
// environment; they are wrapped into a per-container Redactor so literal
// occurrences in container output are masked in addition to the static
// pattern-based redactions.
//
// If the configured IdleOutputTimeout is > 0 a per-container watchdog
// goroutine is spawned that kills the container with an "idle timeout" reason
// when no text/thinking/tool_call/stderr event has been observed for longer
// than the timeout. The watchdog exits when the outer streamLogs goroutine
// finishes (done is closed) or when the parent ctx is cancelled, so it does
// not outlive the container it watches. See CTXRUN-058 (H15).
func (m *Manager) streamLogs(ctx context.Context, containerID string, payload RunConfig, secrets []string, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})

	redactor := logparser.NewRedactor(secrets)

	reader, err := m.docker.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		log.Warn("failed to attach to container logs", "error", err)
		close(done)

		return done
	}

	// Track the last time any output was observed from this container.
	// Updated by both the stderr scanner and the logparser emit callback.
	// Initialised to "now" so a container that exits before producing any
	// output is not flagged idle retroactively.
	var lastOutputAt atomic.Pointer[time.Time]

	now := time.Now()
	lastOutputAt.Store(&now)

	// Start the watchdog if idle-kill is enabled. It closes itself on
	// ctx.Done() or when done is closed (below via `defer close(done)`).
	if idle := m.cfg.IdleOutputTimeout; idle > 0 {
		go m.runIdleWatchdog(ctx, done, containerID, payload, log, &lastOutputAt, idle)
	}

	go func() {
		defer close(done)
		defer func() { _ = reader.Close() }()

		// CTXRUN-040 (C13): the three child goroutines spawned below can
		// each take a panic from third-party input — stdcopy on a malformed
		// docker multiplex frame, bufio.Scanner on bogus UTF-8, the
		// logparser on a bad stream-json line. A panic in any of them used
		// to unwind the whole runner process; the recover() wrappers below
		// isolate each goroutine so one bad container can't crash
		// everything else. The outer goroutine runs logparser
		// synchronously, so it shares the outer's recovery — we recover()
		// there too.
		defer func() {
			if r := recover(); r != nil {
				if m.metrics != nil {
					m.metrics.PanicRecoveredTotal.WithLabelValues(metrics.GoroutineLogparser).Inc()
				}

				m.logger.Error("streamLogs child panicked",
					"goroutine", "logparser",
					"container_id", containerID,
					"card_id", payload.CardID,
					"project", payload.Project,
					"panic", r,
					"stack", string(debug.Stack()))

				if m.broadcaster != nil {
					m.broadcaster.Publish(logbroadcast.LogEntry{
						Timestamp: time.Now(),
						CardID:    payload.CardID,
						Project:   payload.Project,
						Type:      "system",
						Content:   "internal error: logparser panicked",
					})
				}
			}
		}()

		stdoutPr, stdoutPw := io.Pipe()
		stderrPr, stderrPw := io.Pipe()

		go func() {
			defer func() { _ = stdoutPw.Close(); _ = stderrPw.Close() }()
			defer func() {
				if r := recover(); r != nil {
					if m.metrics != nil {
						m.metrics.PanicRecoveredTotal.WithLabelValues(metrics.GoroutineStreamStdout).Inc()
					}

					m.logger.Error("streamLogs child panicked",
						"goroutine", "stdcopy",
						"container_id", containerID,
						"card_id", payload.CardID,
						"project", payload.Project,
						"panic", r,
						"stack", string(debug.Stack()))

					if m.broadcaster != nil {
						m.broadcaster.Publish(logbroadcast.LogEntry{
							Timestamp: time.Now(),
							CardID:    payload.CardID,
							Project:   payload.Project,
							Type:      "system",
							Content:   "internal error: stdcopy panicked",
						})
					}
				}
			}()

			_, _ = stdcopy.StdCopy(stdoutPw, stderrPw, reader)
		}()

		// Log stderr lines as warnings and emit to broadcaster.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					if m.metrics != nil {
						m.metrics.PanicRecoveredTotal.WithLabelValues(metrics.GoroutineStreamStderr).Inc()
					}

					m.logger.Error("streamLogs child panicked",
						"goroutine", "stderr_scanner",
						"container_id", containerID,
						"card_id", payload.CardID,
						"project", payload.Project,
						"panic", r,
						"stack", string(debug.Stack()))

					if m.broadcaster != nil {
						m.broadcaster.Publish(logbroadcast.LogEntry{
							Timestamp: time.Now(),
							CardID:    payload.CardID,
							Project:   payload.Project,
							Type:      "system",
							Content:   "internal error: stderr_scanner panicked",
						})
					}
				}
			}()

			// CTXRUN-059 (H26): start small, grow only when a long line
			// demands it. bufio.Scanner grows geometrically up to max, so
			// a 64KiB initial buffer covers the common case (most stderr
			// lines are a few hundred bytes) while still tolerating up to
			// 1MiB per-line for stack traces. Resident memory per running
			// container drops from ~2MiB of scanner buffers to ~128KiB.
			scanner := bufio.NewScanner(stderrPr)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

			for scanner.Scan() {
				line := scanner.Text()
				// Redact before slog too: a rogue child process echoing the
				// container env through /proc can easily end up on stderr.
				redacted := redactor.Redact(line)
				log.Warn("container stderr", "line", redacted)

				// CTXRUN-058: record the observation so the idle watchdog
				// sees forward progress. Stderr noise counts: a container
				// spewing stack traces is not "idle".
				nowT := time.Now()
				lastOutputAt.Store(&nowT)

				if m.broadcaster != nil {
					m.broadcaster.Publish(logbroadcast.LogEntry{
						Timestamp: nowT,
						CardID:    payload.CardID,
						Project:   payload.Project,
						Type:      "stderr",
						Content:   redacted,
					})
				}
			}
		}()

		// emit is invoked by the logparser for every published assistant
		// text / thinking / tool_use event. We always wire it (even when
		// broadcaster is nil) so the idle watchdog gets fed — that's the
		// whole point of CTXRUN-058 H15.
		emit := func(e logbroadcast.LogEntry) {
			nowT := time.Now()
			lastOutputAt.Store(&nowT)

			if m.broadcaster == nil {
				return
			}

			e.Timestamp = nowT
			e.CardID = payload.CardID
			e.Project = payload.Project
			m.broadcaster.Publish(e)
		}

		onSkillEngaged := func(evt *logparser.SkillEngagedEvent) {
			go func() {
				cbCtx, cancel := context.WithTimeout(context.Background(), callbackTimeout)
				defer cancel()

				if err := m.callback.ReportSkillEngaged(cbCtx, payload.CardID, payload.Project, evt.SkillName); err != nil {
					log.Warn("skill-engaged callback failed", "skill", evt.SkillName, "error", err)
				}
			}()
		}

		logparser.ProcessStreamWithRedactor(stdoutPr, log, redactor, emit, onSkillEngaged)
	}()

	return done
}

// runIdleWatchdog polls lastOutputAt and kills the container when no output
// has been seen for longer than idleTimeout. It returns when done is closed
// (the logstream ended), when ctx is cancelled, or after it fires — a single
// kill is enough, and the container's normal cleanup path takes it from
// there.
//
// The watchdog does not attempt drain-awareness: on shutdown the tracker
// entries are cancelled by the outer shutdown sequence anyway, and a race
// between that and this goroutine is harmless (both paths end with the same
// Kill → waitAndCleanup result).
func (m *Manager) runIdleWatchdog(
	ctx context.Context,
	done <-chan struct{},
	containerID string,
	payload RunConfig,
	log *slog.Logger,
	lastOutputAt *atomic.Pointer[time.Time],
	idleTimeout time.Duration,
) {
	tick := m.cfg.IdleWatchdogInterval
	if tick <= 0 {
		tick = 30 * time.Second
	}
	// Never let the poll interval exceed the idle timeout — otherwise a tight
	// idle deadline (e.g. 50ms in tests) would be missed until the next 30s
	// tick. Clamp the poll interval so the watchdog reacts within roughly one
	// timeout window even for small values.
	if tick > idleTimeout && idleTimeout > 0 {
		tick = idleTimeout
	}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case now := <-ticker.C:
			last := lastOutputAt.Load()
			if last == nil {
				continue
			}

			if now.Sub(*last) <= idleTimeout {
				continue
			}

			reason := fmt.Sprintf("idle timeout: no output for %s", idleTimeout)
			log.Warn("container hit idle-output timeout, killing",
				"container_id", truncateID(containerID),
				"idle_timeout", idleTimeout,
				"last_output_at", last.Format(time.RFC3339Nano),
			)

			m.emitSystem(payload, reason)

			// Prefer Kill (which cancels the run's ctx so waitAndCleanup
			// takes the normal cancel path and reports failure to CM).
			// Fall back to a direct stop if the tracker entry has already
			// been removed (race with exit).
			if err := m.Kill(payload.Project, payload.CardID); err != nil {
				log.Warn("idle watchdog Kill failed; attempting direct stop",
					"container_id", truncateID(containerID),
					"error", err,
				)

				stopCtx, cancel := withDockerCleanupTimeout(ctx)
				m.killContainer(stopCtx, containerID, log)
				cancel()
			}

			return
		}
	}
}

// PruneImages asks dockerd to delete dangling/unused images older than
// imagePruneMaxAge. Called on each tick of the maintenance loop. Returning
// an error does not stop the loop — the caller logs and continues.
// See CTXRUN-058 (M12).
func (m *Manager) PruneImages(ctx context.Context) error {
	args := filters.NewArgs()
	args.Add("dangling", "true")
	args.Add("until", imagePruneMaxAge)

	report, err := m.docker.ImagesPrune(ctx, args)
	if err != nil {
		return fmt.Errorf("images prune: %w", err)
	}

	m.logger.Info("image prune complete",
		"deleted", len(report.ImagesDeleted),
		"space_reclaimed_bytes", report.SpaceReclaimed,
	)

	return nil
}

func (m *Manager) killContainer(ctx context.Context, containerID string, log *slog.Logger) {
	grace := stopGracePeriod
	if err := m.docker.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &grace}); err != nil {
		log.Warn("failed to stop container", "error", err)
	}
}

func (m *Manager) removeContainer(ctx context.Context, containerID string, log *slog.Logger) {
	if err := m.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		log.Warn("failed to remove container", "error", err)
	}
}

// ForceKillContainer is the shutdown-path backstop: stop + force-remove a
// container directly by ID, ignoring the tracker. Used by the main shutdown
// sequence's force-cleanup phase, after the normal Kill + mgr.Wait has
// already timed out. The caller must pass a bounded ctx.
func (m *Manager) ForceKillContainer(ctx context.Context, containerID string) error {
	grace := 0

	var errs []error

	if err := m.docker.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &grace}); err != nil {
		errs = append(errs, fmt.Errorf("stop: %w", err))
	}

	if err := m.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		errs = append(errs, fmt.Errorf("remove: %w", err))
	}

	return errors.Join(errs...)
}

// writePrimingWithTimeout writes the priming stream-json message to the
// attached container's stdin, bounded by primingWriteTimeout. If the write
// doesn't land in time we force-close the writer directly (bypassing the
// tracker) so the wedged Write inside tracker.WriteStdin returns an error
// and releases stdin.mu — otherwise it would hold the per-entry mutex and
// block a subsequent Remove forever. We close the raw writer rather than
// going through tracker.CloseStdin because CloseStdin also acquires
// stdin.mu, which would deadlock against the in-flight Write. Closing the
// writer directly is safe: tracker.Remove's Close is a no-op on an
// already-closed WriteCloser. See CTXRUN-040 (C11).
func (m *Manager) writePrimingWithTimeout(payload RunConfig, containerID string, b []byte, writer io.Closer) {
	done := make(chan error, 1)

	go func() {
		done <- m.tracker.WriteStdin(payload.Project, payload.CardID, b)
	}()

	select {
	case err := <-done:
		if err != nil {
			m.logger.Warn("failed to write priming message to container stdin",
				"container_id", truncateID(containerID),
				"card_id", payload.CardID,
				"project", payload.Project,
				"error", err)
		}
	case <-time.After(primingWriteTimeout):
		m.logger.Warn("priming stdin write timed out; closing writer to unblock",
			"container_id", truncateID(containerID),
			"card_id", payload.CardID,
			"project", payload.Project,
			"timeout", primingWriteTimeout)

		if writer != nil {
			_ = writer.Close()
		}
	}
}

func (m *Manager) reportFailure(ctx context.Context, payload RunConfig, message string) {
	if err := m.callback.ReportStatus(ctx, payload.CardID, payload.Project, "failed", message); err != nil {
		m.logger.Error("failed to report failure callback", "card_id", payload.CardID, "error", err)
	}
}

// reportCompleted notifies ContextMatrix that the container exited normally.
// This acts as a safety net: if Claude didn't call complete_task, the claim
// is still released server-side when ContextMatrix receives the "completed" status.
func (m *Manager) reportCompleted(ctx context.Context, payload RunConfig) {
	if err := m.callback.ReportStatus(ctx, payload.CardID, payload.Project, "completed", "container exited normally"); err != nil {
		m.logger.Error("failed to report completed callback", "card_id", payload.CardID, "error", err)
	}
}

// emitSystem publishes a system-type LogEntry to the broadcaster if one is set.
func (m *Manager) emitSystem(payload RunConfig, content string) {
	if m.broadcaster == nil {
		return
	}

	m.broadcaster.Publish(logbroadcast.LogEntry{
		Timestamp: time.Now(),
		CardID:    payload.CardID,
		Project:   payload.Project,
		Type:      "system",
		Content:   content,
	})
}

// Kill stops and removes a specific container by project and card ID.
func (m *Manager) Kill(project, cardID string) error {
	// Cancel runs the stored context.CancelFunc under tracker.mu, so an
	// inflight Remove cannot observe a nil Cancel or clear the entry from
	// under us. Returns false iff no entry is tracked.
	if !m.tracker.Cancel(project, cardID) {
		return fmt.Errorf("no container tracked for %s/%s", project, cardID)
	}

	return nil
}

// ManagedContainer describes a Docker container labeled as runner-managed. It
// is the ground-truth unit consumed by CM's reconcile sweep: a container is
// listed here iff docker ps says so, regardless of whether the runner's
// in-memory tracker still knows about it. That divergence is the failure mode
// the Docker-authoritative sweep is designed to catch.
type ManagedContainer struct {
	ContainerID   string
	ContainerName string
	CardID        string
	Project       string
	State         string
	StartedAt     time.Time
	Tracked       bool
}

// ListManaged returns every Docker container labeled LabelRunner=true,
// regardless of running/exited state. Tracked reflects whether the in-memory
// tracker currently has a matching entry; consumers can use the field to
// detect tracker/Docker divergence without needing a second round-trip.
//
// Containers missing the card_id or project label are skipped — they are
// neither reachable via /kill (which routes by labels) nor the sweep's
// responsibility (the sweep correlates against CM cards, not arbitrary
// docker containers). Such containers still exist in Docker and are caught
// by CleanupOrphans on the next maintenance tick.
func (m *Manager) ListManaged(ctx context.Context) ([]ManagedContainer, error) {
	containers, err := m.docker.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelRunner+"=true")),
		All:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("list managed containers: %w", err)
	}

	result := make([]ManagedContainer, 0, len(containers))

	for _, ctr := range containers {
		project := ctr.Labels[LabelProject]
		cardID := ctr.Labels[LabelCardID]

		if project == "" || cardID == "" {
			continue
		}

		name := ""
		if len(ctr.Names) > 0 {
			// Docker prefixes container names with "/"; strip it so the
			// wire shape matches what `docker ps` prints.
			name = strings.TrimPrefix(ctr.Names[0], "/")
		}

		result = append(result, ManagedContainer{
			ContainerID:   ctr.ID,
			ContainerName: name,
			CardID:        cardID,
			Project:       project,
			State:         ctr.State,
			StartedAt:     time.Unix(ctr.Created, 0).UTC(),
			Tracked:       m.tracker != nil && m.tracker.Has(project, cardID),
		})
	}

	return result, nil
}

// ForceRemoveByLabels is the /kill fallback path: when the tracker has no
// entry for (project, cardID) but Docker still holds a labeled container, we
// bypass the tracker-driven cancel flow entirely and go straight to
// docker rm -f. The only sane way to get here is tracker/Docker divergence
// (a prior cleanup returned early with a logged warning before removal
// succeeded) — in which case every additional layer that "properly" cancels
// the missing tracker entry is a no-op, and the container leaks to the 2h
// container_timeout unless we reach past them.
//
// Returns the number of containers removed. An error from any single removal
// is joined into the final error but does not stop the sweep over the rest
// of the matches — removing as many as possible still beats leaving them all
// running.
func (m *Manager) ForceRemoveByLabels(ctx context.Context, project, cardID string) (int, error) {
	if project == "" || cardID == "" {
		return 0, fmt.Errorf("force-remove: project and card_id are both required")
	}

	args := filters.NewArgs()
	args.Add("label", LabelRunner+"=true")
	args.Add("label", LabelProject+"="+project)
	args.Add("label", LabelCardID+"="+cardID)

	containers, err := m.docker.ContainerList(ctx, container.ListOptions{
		Filters: args,
		All:     true,
	})
	if err != nil {
		return 0, fmt.Errorf("list containers by label: %w", err)
	}

	removed := 0

	var errs []error

	for _, ctr := range containers {
		idShort := truncateID(ctr.ID)

		m.logger.Info("force-removing container by label",
			"container_id", idShort,
			"card_id", cardID,
			"project", project,
		)

		rmCtx, cancel := withDockerCleanupTimeout(ctx)
		if err := m.docker.ContainerRemove(rmCtx, ctr.ID, container.RemoveOptions{Force: true}); err != nil {
			m.logger.Warn("force-remove by label failed",
				"container_id", idShort,
				"card_id", cardID,
				"project", project,
				"error", err,
			)

			errs = append(errs, fmt.Errorf("remove %s: %w", idShort, err))

			cancel()

			continue
		}

		cancel()

		removed++
	}

	return removed, errors.Join(errs...)
}

// CleanupOrphans removes any leftover containers from a previous runner crash.
// A container is "orphan" iff it is labeled LabelRunner=true in Docker AND
// has no corresponding entry in the in-memory tracker — i.e. the current
// runner process does not know about it. Containers that are actively
// tracked (a card is assigned, the runner is still managing them) are
// skipped so the periodic maintenance sweep does not kill live work.
//
// Per-container Stop/Remove failures are logged individually and collected
// into the returned error via errors.Join so that callers can see which
// containers failed without aborting cleanup of the rest. A nil return means
// every orphan was successfully stopped and removed.
func (m *Manager) CleanupOrphans(ctx context.Context) error {
	containers, err := m.docker.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelRunner+"=true")),
		All:     true,
	})
	if err != nil {
		return fmt.Errorf("list orphan containers: %w", err)
	}

	// Filter out containers still present in the in-memory tracker. Without
	// this, the maintenance loop would kill every active worker container
	// on every tick.
	orphans := make([]DockerContainer, 0, len(containers))
	skipped := 0

	for _, ctr := range containers {
		project := ctr.Labels[LabelProject]
		cardID := ctr.Labels[LabelCardID]

		if m.tracker != nil && project != "" && cardID != "" && m.tracker.Has(project, cardID) {
			skipped++

			continue
		}

		orphans = append(orphans, ctr)
	}

	var errs []error

	for _, ctr := range orphans {
		idShort := truncateID(ctr.ID)
		m.logger.Info("cleaning up orphan container",
			"container_id", idShort,
			"card_id", ctr.Labels[LabelCardID],
			"project", ctr.Labels[LabelProject],
		)

		// CTXRUN-040: each per-container Stop/Remove is bounded so one
		// wedged orphan can't stall cleanup of the rest. Using a fresh
		// dockerCleanupTimeout-scoped ctx here (rather than a child of
		// the caller's ctx) matches the semantics we want on shutdown:
		// we'd rather give up quickly and log than wait for a hung
		// daemon.
		stopCtx, stopCancel := withDockerCleanupTimeout(ctx)

		stopTimeout := 5
		if stopErr := m.docker.ContainerStop(stopCtx, ctr.ID, container.StopOptions{Timeout: &stopTimeout}); stopErr != nil {
			m.logger.Warn("orphan stop failed",
				"container_id", idShort,
				"card_id", ctr.Labels[LabelCardID],
				"project", ctr.Labels[LabelProject],
				"error", stopErr,
			)
			errs = append(errs, fmt.Errorf("stop orphan %s: %w", idShort, stopErr))
		}

		stopCancel()

		rmCtx, rmCancel := withDockerCleanupTimeout(ctx)

		if rmErr := m.docker.ContainerRemove(rmCtx, ctr.ID, container.RemoveOptions{Force: true}); rmErr != nil {
			m.logger.Warn("orphan remove failed",
				"container_id", idShort,
				"card_id", ctr.Labels[LabelCardID],
				"project", ctr.Labels[LabelProject],
				"error", rmErr,
			)
			errs = append(errs, fmt.Errorf("remove orphan %s: %w", idShort, rmErr))
		}

		rmCancel()
	}

	if len(orphans) > 0 || skipped > 0 {
		m.logger.Info("orphan cleanup complete",
			"removed", len(orphans)-len(errs),
			"attempted", len(orphans),
			"tracked_skipped", skipped,
			"errors", len(errs),
		)
	}

	return errors.Join(errs...)
}

func (m *Manager) pullImage(ctx context.Context, img string) error {
	policy := m.cfg.ImagePullPolicy
	if policy == "" {
		return fmt.Errorf("image_pull_policy is unset; this is a programming error, set it explicitly in Config")
	}

	if policy == config.PullNever {
		return nil
	}

	if policy == config.PullIfNotPresent {
		if _, err := m.docker.ImageInspect(ctx, img); err == nil {
			m.logger.Debug("image already present locally, skipping pull", "image", img)

			return nil
		}
	}

	pullCtx, cancel := context.WithTimeout(ctx, imagePullTimeout)
	defer cancel()

	reader, err := m.docker.ImagePull(pullCtx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", img, err)
	}

	if _, err := io.Copy(io.Discard, reader); err != nil {
		m.logger.Warn("failed to drain image pull output", "error", err)
	}

	if err := reader.Close(); err != nil {
		m.logger.Warn("failed to close image pull reader", "error", err)
	}

	return nil
}

var containerNameRe = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// normalizeRepoURL rewrites an ssh:// git remote URL to its https equivalent
// so the container can authenticate with a token rather than an SSH key.
// Other schemes (notably https://) pass through unchanged. Validation upstream
// rejects everything except https:// and ssh://, so this only ever runs on
// those two cases.
//
//	ssh://git@github.com/org/repo.git  → https://github.com/org/repo.git
//	ssh://github.com/org/repo.git      → https://github.com/org/repo.git
//	https://github.com/org/repo.git    → (unchanged)
func normalizeRepoURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	if u.Scheme == "ssh" {
		// Strip user info (e.g. "git@") and rewrite to https.
		u.Scheme = "https"
		u.User = nil

		return u.String()
	}

	return rawURL
}

func sanitizeContainerName(project, cardID string) string {
	name := fmt.Sprintf("cmr-%s-%s", project, cardID)
	name = strings.ToLower(name)

	return containerNameRe.ReplaceAllString(name, "-")
}

func truncateID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
}

// buildExtraHosts returns extra /etc/hosts entries for the container.
// Always includes host.docker.internal. If the MCP URL contains a hostname
// that resolves on the host (e.g. via /etc/hosts), it's added so containers
// can reach it too.
//
// The lookup is bounded by dnsLookupTimeout and memoised via m.dnsCache so
// an attacker-influenced MCPURL hostname can't stall the spawn path. A
// timeout or error returns the default entry set only — the container can
// still reach the MCP server if in-container DNS works. See CTXRUN-059 (H24).
//
// The caller's ctx is intentionally ignored: the 2s cap is already tight
// enough that inheriting a near-cancelled parent ctx would effectively
// disable the cache-miss path. The parameter is retained (as _) so the
// signature stays stable for call sites that pass ctx unconditionally.
func (m *Manager) buildExtraHosts(_ context.Context, mcpURL string) []string {
	hosts := []string{"host.docker.internal:host-gateway"}

	u, err := url.Parse(mcpURL)
	if err != nil || u.Hostname() == "" {
		return hosts
	}

	hostname := u.Hostname()
	// Skip if it's an IP, localhost, or host.docker.internal (already added via host-gateway above)
	if net.ParseIP(hostname) != nil || hostname == "localhost" || hostname == "host.docker.internal" {
		return hosts
	}

	// Cache hit: return the memoised IPs without touching the resolver.
	if addrs, ok := m.dnsCache.get(hostname); ok && len(addrs) > 0 {
		hosts = append(hosts, hostname+":"+addrs[0])

		return hosts
	}

	resolver := m.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	// Bound the lookup so a slow authoritative server can't stall us.
	// Use context.Background as the parent so a nearly-cancelled parent ctx
	// doesn't cut the deadline short (the 2s cap is already tight enough).
	lookupCtx, cancel := context.WithTimeout(context.Background(), dnsLookupTimeout)
	defer cancel()

	addrs, err := resolver.LookupHost(lookupCtx, hostname)
	if err != nil || len(addrs) == 0 {
		// Distinguish timeout from other failures so operators know when
		// to look at DNS latency versus a misconfigured hostname.
		if errors.Is(err, context.DeadlineExceeded) || lookupCtx.Err() != nil {
			if m.metrics != nil {
				m.metrics.DNSLookupTimeoutsTotal.Inc()
			}

			m.logger.Warn("MCP hostname lookup timed out; container will run without ExtraHosts mapping",
				"hostname", hostname, "timeout", dnsLookupTimeout)
		} else {
			m.logger.Warn("could not resolve MCP hostname for container",
				"hostname", hostname, "error", err)
		}

		return hosts
	}

	m.dnsCache.put(hostname, addrs)

	hosts = append(hosts, hostname+":"+addrs[0])
	m.logger.Info("added MCP host to container", "hostname", hostname, "ip", addrs[0])

	return hosts
}
