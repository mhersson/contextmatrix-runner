package container

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
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
	"github.com/mhersson/contextmatrix-runner/internal/chatproto"
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
	// LabelSessionID stores the chat session ID on chat-mode containers.
	LabelSessionID = "contextmatrix.session_id"

	imagePullTimeout = 5 * time.Minute
	// imageInspectTimeout bounds the synchronous ImageInspect call in
	// pullImage's PullIfNotPresent fast-path. A wedged dockerd otherwise
	// blocks the spawn indefinitely because the caller's parent ctx has no
	// deadline at this point. 5s matches the preflight's image_inspect
	// probe budget.
	imageInspectTimeout = 5 * time.Second
	stopGracePeriod     = 10 // seconds
	// callbackTimeout bounds the detached context used to deliver a status
	// callback after the parent ctx has already been cancelled (e.g. on a
	// Kill or start failure race).
	callbackTimeout = 10 * time.Second

	// dockerCleanupTimeout bounds the detached contexts used for
	// best-effort Docker cleanup (Stop / Remove / Kill) that must run
	// even when the parent ctx has already been cancelled. A hung
	// dockerd would otherwise stall shutdown forever; this cap bounds
	// every such call.
	dockerCleanupTimeout = 5 * time.Second

	// runningStatusCallbackTimeout bounds the detached context used by
	// runningCallbackAsync to deliver the "running" status callback. 30s
	// is generous enough to absorb the full 3-attempt retry ladder
	// (1s + 2s + 4s backoff + per-request 10s timeout each) while still
	// bounding the goroutine's lifetime if CM is wedged.
	runningStatusCallbackTimeout = 30 * time.Second

	// taskSkillsPullTimeout bounds the synchronous `git pull` invoked by
	// pullSkillsRepo on the spawn path. Without this, a slow or wedged
	// GitHub remote could stall every spawn indefinitely because the
	// caller's parent ctx has no deadline. 60s is generous for a small
	// skills repo over a healthy network while still capping the worst
	// case before workers visibly slow down.
	taskSkillsPullTimeout = 60 * time.Second
)

// primingWriteTimeout bounds the priming WriteStdin call made right after
// ContainerAttach. A wedged hijacked socket would otherwise hang the Run
// goroutine indefinitely; with this deadline the goroutine gives up and
// continues into waitAndCleanup so normal cancellation paths work.
//
// Declared as a package var (not a const) so tests can shrink it to keep
// synthetic-wedge cases fast.
var primingWriteTimeout = 5 * time.Second

// logDrainTimeout bounds the <-logDone wait on the cancel, timeout, and
// error branches of waitAndCleanup. A hung log-streaming goroutine (wedged
// docker daemon, stuck hijacked socket, or stdcopy/scanner stall) would
// otherwise stall those branches indefinitely, preventing the cleanup
// defers from running and leaking the container until container_timeout
// (2h default).
// Declared as a package var so tests can shrink it without waiting 5s of
// wall time per synthetic-wedge scenario.
var logDrainTimeout = 5 * time.Second

// pullSkillsRepo runs `git pull --ff-only` in dir, authenticating against the
// configured upstream with a freshly minted GitHub token when the remote is
// HTTPS. Returns nil if dir is not a git repo (operator may have a non-tracked
// local clone) or has no `origin` remote configured. Returns the git error
// otherwise — caller should log and continue, not abort.
//
// The git invocation is bounded by taskSkillsPullTimeout so a slow or
// wedged remote cannot stall the spawn path. Credential prompting is
// suppressed via GIT_TERMINAL_PROMPT=0 and GIT_ASKPASS=true so a rejected
// injected token does not leave git blocked waiting on /dev/tty.
var pullSkillsRepo = func(ctx context.Context, dir, token string) error {
	gitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		if os.IsNotExist(err) {
			return nil // not tracked; skip
		}

		return fmt.Errorf("stat %s: %w", gitDir, err)
	}

	pullCtx, cancel := context.WithTimeout(ctx, taskSkillsPullTimeout)
	defer cancel()

	cmd := exec.CommandContext(pullCtx, "git", "-C", dir, "pull", "--ff-only")
	cmd.Env = pullSkillsEnv(pullCtx, dir, token)

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
//
// GIT_TERMINAL_PROMPT=0 and GIT_ASKPASS=true are appended in every case
// so a rejected token (or any other auth failure) cannot leave git
// blocked on /dev/tty waiting for credential input. taskSkillsPullTimeout
// is the secondary bound, but suppressing the prompt converts the failure
// into a fast error rather than a 60s timeout-and-retry.
func pullSkillsEnv(ctx context.Context, dir, token string) []string {
	parent := os.Environ()

	// Suppress interactive credential prompting. GIT_TERMINAL_PROMPT=0
	// makes git fail-fast on auth instead of blocking on /dev/tty;
	// GIT_ASKPASS=true neutralises any system-installed askpass helper
	// that would otherwise be invoked when the terminal prompt is off.
	parent = append(parent,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
	)

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
// points the card's MCP URL at a slow-responding authoritative server could
// otherwise stall the spawn path indefinitely; the deadline caps
// exposure at 2s and then falls back to running the container without the
// ExtraHosts entry. The container can still reach the MCP server via the
// normal host-gateway route if DNS inside the container itself works.
const dnsLookupTimeout = 2 * time.Second

// secretMode describes how secrets are delivered to a container.
type secretMode int

const (
	// secretModeFile delivers secrets via the singleton shared dir
	// ($SecretsDir/shared), bind-mounted read-only at /run/cm-secrets/.
	// The entrypoint sources /run/cm-secrets/env which the tokenRefresher writes.
	secretModeFile secretMode = iota
	// secretModeEnvVar delivers secrets directly via HostConfig.Env. Used when
	// SecretsDir is empty (unit tests without a shared dir).
	secretModeEnvVar
)

// secretDelivery describes how secrets reach a container. In file mode,
// FilePath is the host-side shared dir to bind-mount. In env-var mode, EnvVars
// is the slice of KEY=VALUE strings to append to the container env.
type secretDelivery struct {
	Mode     secretMode
	FilePath string   // set when Mode == secretModeFile: host path of shared dir
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

	// chatCleanup stashes the resources that WaitAndCleanupChat must
	// release after the container exits. Populated by StartChat (keyed
	// by container ID) and consumed by WaitAndCleanupChat.
	// Ownership of the wait goroutine sits with the webhook handler so
	// the tracker entry is registered before an instant container exit
	// can race RemoveChat against AddChat.
	chatCleanupMu sync.Mutex
	chatCleanup   map[string]chatResumeDelivery

	// chatSecrets stashes the per-container redaction inputs collected
	// during StartChat so StreamChatLogs can build a Redactor without
	// changing the webhook-facing interface. Populated under StartChat
	// (keyed by container ID) and consumed by StreamChatLogs; cleared
	// once consumed so a long-lived process does not accumulate entries.
	chatSecretsMu sync.Mutex
	chatSecrets   map[string][]string

	// skillsPullMu serialises concurrent pullSkillsRepo calls against the
	// shared TaskSkillsDir. `git pull` writes to .git/index.lock, which is
	// not safe to share across concurrent invocations against the same
	// working tree; without this two simultaneous spawns can fail with
	// "Another git process seems to be running" or worse, leave a stale
	// lock behind. The mutex is acquired around the synchronous pull only —
	// it does not cover the mount-construction step, so a slow pull cannot
	// stall a different consumer that has already cached the on-disk state.
	skillsPullMu sync.Mutex
}

// WithMetrics attaches a metrics bundle to the manager. A nil bundle disables
// metric observation (useful in tests that do not care about Prometheus).
func (m *Manager) WithMetrics(mx *metrics.Metrics) *Manager {
	m.metrics = mx

	return m
}

// NewManager creates a container manager.
//
// token (githubauth.TokenGenerator) is expected to be non-nil for any
// deployment that actually spawns containers — every spawn-path branch
// (card mode startContainer, chat mode StartChat / BuildChatAuthEnv, and the
// background tokenRefresher) calls GenerateToken conditionally. Production
// wiring in cmd/contextmatrix-runner/main.go always passes a real provider.
//
// A nil token is permitted here so that test code which only exercises
// non-spawn methods (Kill, CleanupOrphans, ListManaged, PruneImages, etc.)
// can construct a Manager without spinning up a fake GitHub token server.
// The four spawn-path call sites (startContainer, buildSecretDelivery,
// StartTokenRefresher, BuildChatAuthEnv) each guard with a nil-check or a
// TaskSkillsDir-guard that skips the mint when the token has no consumer.
//
// cb and broadcaster may also be nil; the manager guards those at every call site.
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
		chatCleanup: map[string]chatResumeDelivery{},
		chatSecrets: map[string][]string{},
	}
}

func (m *Manager) InitSharedSecrets() error {
	if m.cfg.SecretsDir == "" {
		return nil
	}

	dir := filepath.Join(m.cfg.SecretsDir, sharedSecretsSubdir)
	if err := m.mkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create shared secrets dir: %w", err)
	}

	return nil
}

// buildStaticAuthEnv selects the Claude credential to fold into the
// shared secrets file. Empty map when ClaudeAuthDir is configured (the
// worker reads creds from the bind-mounted dir, not env).
func buildStaticAuthEnv(cfg *config.Config) map[string]string {
	static := map[string]string{}
	if cfg.ClaudeAuthDir != "" {
		return static
	}

	switch {
	case cfg.ClaudeOAuthToken != "":
		static["CLAUDE_CODE_OAUTH_TOKEN"] = cfg.ClaudeOAuthToken
	case cfg.AnthropicAPIKey != "":
		static["ANTHROPIC_API_KEY"] = cfg.AnthropicAPIKey
	}

	return static
}

// StartTokenRefresher initialises the shared secrets directory via
// InitSharedSecrets, performs the first token mint synchronously, and then
// spawns a background goroutine that re-mints on expiry. The initial mint is
// fail-closed: if it fails, StartTokenRefresher returns the error immediately
// and the caller should treat it as fatal at boot time. Subsequent mint
// failures in the goroutine loop are retried with exponential backoff and do
// not propagate. In env-var fallback mode (cfg.SecretsDir empty),
// InitSharedSecrets is a no-op, the initial mint is skipped, and the
// spawned goroutine logs and returns immediately.
//
// Returns an error if m.token is nil (a deployment that bind-mounts a
// shared secrets dir but has no token provider configured is a misconfig).
func (m *Manager) StartTokenRefresher(ctx context.Context) error {
	if m.token == nil {
		return fmt.Errorf("StartTokenRefresher: token provider is required (m.token is nil)")
	}

	if err := m.InitSharedSecrets(); err != nil {
		return err
	}

	r := newTokenRefresher(tokenRefresherConfig{
		Token:      m.token,
		SecretsDir: m.cfg.SecretsDir,
		StaticEnv:  buildStaticAuthEnv(m.cfg),
		Logger:     m.logger,
		Metrics:    m.metrics,
	})

	// Synchronous initial mint+write so the HTTP listener can't accept a
	// trigger before /run/cm-secrets/env exists. Skipped in disabled mode
	// (env-var fallback) where there's no file to write.
	if !r.disabled {
		if _, _, err := r.doOneRefresh(ctx); err != nil {
			return fmt.Errorf("initial github token refresh: %w", err)
		}
	}

	m.wg.Add(1)

	go func() {
		defer m.wg.Done()

		r.Run(ctx)
	}()

	return nil
}

// withCleanupTimeout returns a fresh, parent-detached context bounded by
// callbackTimeout. Used by all cleanup / callback code paths that MUST run
// even when the caller's ctx has already been cancelled (e.g. shutdown,
// container-kill). The parent is intentionally not plumbed in: inheriting a
// cancelled parent would turn every callback into a no-op, which is exactly
// the hang the graceful-shutdown sequence is fixing.
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

// handlePanic is the shared body of the panic-recovery defers attached to
// every manager background goroutine. Must be called from inside the
// deferred function — recover() is invoked by the caller and the value
// passed through `r`.
//
// Steps:
//  1. Increment metrics.PanicRecoveredTotal{goroutine=label} (if metrics set).
//  2. slog.Error logMsg with fields + "panic" + "stack" (appended here so
//     callers don't duplicate the boilerplate).
//  3. If sysEntry is non-nil and the broadcaster is wired, publish a copy
//     with a fresh Timestamp so /logs subscribers see the internal-error event.
//
// Centralising this lets each call site read like:
//
//	defer func() {
//	    if r := recover(); r != nil {
//	        m.handlePanic(r, metrics.GoroutineXxx, "foo panicked",
//	            []any{"card_id", ..., "project", ...},
//	            &logbroadcast.LogEntry{Type: "system", Content: "internal error: foo panicked"})
//	    }
//	}()
//
// Note: Go's recover() returns non-nil only when called directly inside a
// deferred function, so the caller MUST invoke recover() — handlePanic does
// not (and cannot) call it.
func (m *Manager) handlePanic(r any, label, logMsg string, fields []any, sysEntry *logbroadcast.LogEntry) {
	if m.metrics != nil {
		m.metrics.PanicRecoveredTotal.WithLabelValues(label).Inc()
	}

	allFields := make([]any, 0, len(fields)+4)
	allFields = append(allFields, fields...)
	allFields = append(allFields, "panic", r, "stack", string(debug.Stack()))

	if m.logger != nil {
		m.logger.Error(logMsg, allFields...)
	}

	if sysEntry != nil && m.broadcaster != nil {
		entry := *sysEntry
		entry.Timestamp = time.Now()
		m.broadcaster.Publish(entry)
	}
}

// Run launches the full container lifecycle for a triggered task in a goroutine.
// Use Wait to block until all launched goroutines have finished.
func (m *Manager) Run(ctx context.Context, payload RunConfig) {
	started := time.Now()

	m.wg.Go(func() {
		outcome := metrics.OutcomeSuccess

		// containerID is captured locally so the deferred panic recovery can
		// see it even before tracker.UpdateContainerID has published it.
		// Without this, a panic between startContainer's return and
		// UpdateContainerID would leave snap.ContainerID == "" and the
		// Force-remove path would silently no-op, leaking the container.
		var containerID string

		defer func() {
			if r := recover(); r != nil {
				outcome = metrics.OutcomeFailure

				m.handlePanic(r, metrics.GoroutineRun, "container run panicked",
					[]any{"card_id", payload.CardID, "project", payload.Project},
					nil)

				// Close the partial-failure window. If startContainer had
				// already returned a container ID and then something downstream
				// panicked before waitAndCleanup installed its defers, the
				// Docker container would leak because tracker.Remove alone only
				// clears the in-memory entry. Prefer the locally captured ID
				// (set immediately after startContainer returns) over the
				// tracker snapshot, which is only populated after
				// UpdateContainerID has run — there is a window between the
				// two where the snapshot reports ContainerID == "".
				id := containerID
				if id == "" {
					if snap, ok := m.tracker.Snapshot(payload.Project, payload.CardID); ok {
						id = snap.ContainerID
					}
				}

				if id != "" {
					rmCtx, rmCancel := withDockerCleanupTimeout(context.Background())
					if rmErr := m.docker.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true}); rmErr != nil {
						m.logger.Warn("panic recovery: docker remove failed",
							"container_id", id,
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

		outcome = m.run(ctx, payload, &containerID)
	})
}

// Wait blocks until all container goroutines launched by Run have finished.
func (m *Manager) Wait() {
	m.wg.Wait()
}

// run is the inner body of Run. The containerIDOut pointer is populated as
// soon as startContainer succeeds so the outer Run's deferred panic recovery
// can see the ID even before tracker.UpdateContainerID has been called.
func (m *Manager) run(ctx context.Context, payload RunConfig, containerIDOut *string) string {
	log := m.logger.With("card_id", payload.CardID, "project", payload.Project)

	containerID, _, secretValues, err := m.startContainer(ctx, payload)
	if err != nil {
		log.Error("failed to start container", "error", err)

		// Use a detached context for the callback: if start raced with a Kill
		// (or the runner is shutting down), ctx is already cancelled and the
		// HTTP request would be a no-op, leaving CM blind to the failure.
		// The outer flow still honours parent cancellation — only the
		// "tell CM we failed" side-effect gets a fresh deadline.
		cbCtx, cancel := withCleanupTimeout(ctx)
		defer cancel()

		m.reportFailure(cbCtx, payload, fmt.Sprintf("start failed: %v", err))
		m.tracker.Remove(payload.Project, payload.CardID)

		return metrics.OutcomeFailure
	}

	if containerIDOut != nil {
		*containerIDOut = containerID
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
	// kernel buffer pressure. Fire and forget on a detached 30s context with
	// a panic-safe wrapper.
	m.runningCallbackAsync(payload, log)

	// Wait for container to finish and return its outcome so the caller's
	// duration histogram carries the right label.
	return m.waitAndCleanup(ctx, containerID, payload, secretValues, log)
}

// runningCallbackAsync fires ReportStatus("running") on a background
// goroutine with a fresh 30s ctx. The parent ctx is intentionally NOT
// inherited: if the parent is already near-cancel (e.g. rapid Kill after
// spawn), the callback would become a no-op and CM would see the card
// stuck. Panics are recovered and counted so one bad callback cannot take
// down the runner.
func (m *Manager) runningCallbackAsync(payload RunConfig, log *slog.Logger) {
	m.wg.Add(1)

	go func() {
		defer m.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				m.handlePanic(r, metrics.GoroutineRunningStatusCallback, "running-status callback panicked",
					[]any{"card_id", payload.CardID, "project", payload.Project},
					nil)
			}
		}()

		cbCtx, cancel := context.WithTimeout(context.Background(), runningStatusCallbackTimeout)
		defer cancel()

		if err := m.callback.ReportStatus(cbCtx, payload.CardID, payload.Project, "running", "container started"); err != nil {
			log.Warn("failed to report running status (async)", "error", err)
		}
	}()
}

// secretsMountTarget is the in-container directory where the shared secrets
// dir is bind-mounted read-only. The entrypoint sources the `env` file inside
// this directory. It is a PATH, not a credential; the gosec G101 flag is a
// false positive.
const secretsMountTarget = "/run/cm-secrets" //nolint:gosec // path, not a credential

// caCertMountTarget is the in-container path where the optional extra-CA PEM
// (config: ca_cert_file) is bind-mounted read-only. The runner points
// NODE_EXTRA_CA_CERTS at this path so Claude Code's Node TLS trusts the chain.
const caCertMountTarget = "/run/cm-ca/ca.crt"

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

	// Mint a GitHub token ONLY when we'll actually pass it to taskSkillsMount
	// (i.e. a skills clone is configured). Without this guard, deployments that
	// don't bind-mount a skills clone would fail card spawns whenever the
	// GitHub API hiccups even though the token is never used. The token is NOT
	// passed into the container — rotating credentials reach the container via
	// the shared secrets dir that the tokenRefresher maintains.
	//
	// m.token may be nil in unit-test deployments that exercise non-spawn
	// methods only; combine the nil-guard with the TaskSkillsDir guard so a
	// missing provider is detected here instead of crashing inside GenerateToken.
	var gitToken string

	if m.cfg.TaskSkillsDir != "" {
		if m.token == nil {
			return "", secretDelivery{}, nil, fmt.Errorf("generate git token: token provider is required when task_skills_dir is set")
		}

		tok, _, err := m.token.GenerateToken(ctx)
		if err != nil {
			return "", secretDelivery{}, nil, fmt.Errorf("generate git token: %w", err)
		}

		gitToken = tok
	}

	// Build environment variables. CM_MCP_API_KEY is per-card and doesn't
	// rotate, so it rides directly in Env rather than the shared dir.
	env := []string{
		"CM_CARD_ID=" + payload.CardID,
		"CM_PROJECT=" + payload.Project,
		"CM_MCP_URL=" + payload.MCPURL,
		"CM_REPO_URL=" + normalizeRepoURL(payload.RepoURL),
	}

	if payload.MCPAPIKey != "" {
		env = append(env, "CM_MCP_API_KEY="+payload.MCPAPIKey)
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

	env = m.appendCommonEnv(env, payload.TaskSkills)

	var mounts []mount.Mount

	if authMount, ok := m.claudeAuthMount(); ok {
		mounts = append(mounts, authMount)
	}

	if skillsMount, ok := m.taskSkillsMount(ctx, gitToken); ok {
		mounts = append(mounts, skillsMount)
	}

	if caMount, ok := m.caCertMount(); ok {
		mounts = append(mounts, caMount)
	}

	// Determine secret delivery mode and collect secret values for the
	// per-container Redactor. In file mode, the tokenRefresher manages the
	// shared dir contents; in env-var mode (SecretsDir == ""), mint inline.
	delivery, err := m.buildSecretDelivery(ctx)
	if err != nil {
		return "", secretDelivery{}, nil, fmt.Errorf("build secret delivery: %w", err)
	}

	// Collect secret values for the per-container Redactor. See
	// collectSecretValues for the threat-model and CM_GIT_TOKEN-absent
	// rationale.
	secretValues := collectSecretValues(payload.MCPAPIKey, delivery)

	// Wire the delivery into the container configuration.
	env, mounts = applySecretsDelivery(env, mounts, delivery)

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
		m.baseHostConfig(ctx, payload.MCPURL, mounts),
		nil, nil, name,
	)
	if err != nil {
		return "", delivery, nil, fmt.Errorf("create container: %w", err)
	}

	if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up the created-but-not-started container. The caller's ctx may
		// already be cancelled (e.g. Kill raced the start goroutine), so use a
		// fresh detached ctx for the removal — otherwise ContainerRemove turns
		// into a no-op and the container leaks until the 2h container_timeout.
		rmCtx, rmCancel := withDockerCleanupTimeout(ctx)
		if rmErr := m.docker.ContainerRemove(rmCtx, resp.ID, container.RemoveOptions{Force: true}); rmErr != nil {
			m.logger.Warn("failed to remove container after start failure", "container_id", resp.ID, "error", rmErr)
		}

		rmCancel()

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
			// The container is already running with CM_INTERACTIVE=1 and
			// entrypoint.sh blocks forever on stdin. Without an attached
			// writer, tracker.SetStdin is never called, so /message,
			// /promote, and /end-session all reject the session and the
			// container leaks for up to container_timeout (default 2h).
			// Force-remove it via a fresh ctx so a race with parent
			// cancellation can't turn the cleanup into a no-op, then
			// propagate the original attach error so the failure-callback
			// path runs.
			m.logger.Warn("failed to attach stdin to container; removing to avoid leak",
				"container_id", resp.ID, "error", err)

			rmCtx, rmCancel := withDockerCleanupTimeout(ctx)
			if rmErr := m.docker.ContainerRemove(rmCtx, resp.ID, container.RemoveOptions{Force: true}); rmErr != nil {
				m.logger.Warn("failed to remove container after attach failure",
					"container_id", resp.ID, "error", rmErr)
			}

			rmCancel()

			return "", delivery, nil, fmt.Errorf("attach stdin: %w", err)
		}

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
			// Wrap the priming WriteStdin with a deadline. The
			// hijacked net.Conn can wedge on kernel buffer pressure,
			// a slow container, or a misbehaving proxy, and a
			// synchronous write would otherwise block the Run goroutine
			// forever. We can't reach through
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

	return resp.ID, delivery, secretValues, nil
}

// isPermissionDenied returns true when err is (or wraps) os.ErrPermission or
// syscall.EROFS — the two error values that indicate a read-only or
// unwritable filesystem rather than a transient I/O problem.
func isPermissionDenied(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// buildSecretDelivery decides how secrets are delivered to the container.
// When SecretsDir is set, the singleton tokenRefresher maintains the shared
// dir; workers bind-mount it read-only at /run/cm-secrets/ (file mode).
// When SecretsDir is empty (unit tests without a shared dir), a fresh token is
// minted inline and all secrets are folded into Container.Env (env-var mode).
func (m *Manager) buildSecretDelivery(ctx context.Context) (secretDelivery, error) {
	if m.cfg.SecretsDir == "" {
		if m.token == nil {
			return secretDelivery{}, fmt.Errorf("mint github token: token provider is required in env-var delivery mode")
		}

		token, _, err := m.token.GenerateToken(ctx)
		if err != nil {
			return secretDelivery{}, fmt.Errorf("mint github token: %w", err)
		}

		envVars := []string{"CM_GIT_TOKEN=" + token}

		for k, v := range buildStaticAuthEnv(m.cfg) {
			envVars = append(envVars, k+"="+v)
		}

		return secretDelivery{Mode: secretModeEnvVar, EnvVars: envVars}, nil
	}

	return secretDelivery{
		Mode:     secretModeFile,
		FilePath: filepath.Join(m.cfg.SecretsDir, sharedSecretsSubdir),
	}, nil
}

// collectSecretValues builds the list of literal credential values that the
// per-container Redactor should mask in log output. MCPAPIKey rides directly
// in Env and is always known at spawn time; in env-var delivery mode the
// CM_GIT_TOKEN + Claude auth values are folded into delivery.EnvVars and
// pulled out one KEY=VALUE pair at a time.
//
// CM_GIT_TOKEN is intentionally absent in file mode because the runner
// doesn't know the live value at spawn time — the refresher owns it. Threat
// model: worker logs leaking a token are an unlikely and low-impact path on
// a single-tenant host. If future redaction is needed, the refresher should
// publish via a thread-safe accessor.
//
// Shared by startContainer and StartChat so the two spawn paths stay in lockstep.
func collectSecretValues(mcpAPIKey string, delivery secretDelivery) []string {
	var values []string

	if mcpAPIKey != "" {
		values = append(values, mcpAPIKey)
	}

	if delivery.Mode == secretModeEnvVar {
		for _, e := range delivery.EnvVars {
			if idx := strings.IndexByte(e, '='); idx >= 0 {
				values = append(values, e[idx+1:])
			}
		}
	}

	return values
}

// applySecretsDelivery wires the secret-delivery decision into the container
// spec. In file mode it appends the shared-dir bind-mount at /run/cm-secrets/;
// in env-var fallback mode it appends the secrets to the env slice.
// Both card-mode and chat-mode call this so the two delivery modes stay symmetric.
func applySecretsDelivery(env []string, mounts []mount.Mount, delivery secretDelivery) ([]string, []mount.Mount) {
	switch delivery.Mode {
	case secretModeFile:
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   delivery.FilePath,
			Target:   secretsMountTarget,
			ReadOnly: true,
		})
	case secretModeEnvVar:
		env = append(env, delivery.EnvVars...)
	}

	return env, mounts
}

// chatResumeMountTarget is the in-container path where the rehydration
// payload files are bind-mounted. The entrypoint reads resume.jsonl from
// here when CM_CHAT_RESUME=1 is set.
const chatResumeMountTarget = "/run/cm-chat"

// chatResumeDelivery is the result of prepareChatResume. DirPath is the
// host directory that should be bind-mounted at chatResumeMountTarget;
// empty when rehydration was not requested or could not be materialised.
type chatResumeDelivery struct {
	DirPath string
}

// chatResumeMeta is the JSON payload written to resume.meta.json. The
// entrypoint surfaces this in the rehydration prompt so the agent knows
// whether older turns were clipped.
type chatResumeMeta struct {
	PromptVersion int    `json:"prompt_version"`
	Clipped       bool   `json:"clipped"`
	TurnCount     int    `json:"turn_count"`
	OrigSeqMax    int64  `json:"original_seq_max"`
	SessionID     string `json:"session_id"`
	Project       string `json:"project,omitempty"`
}

// prepareChatResume materialises the rehydration payload on disk so the
// container can bind-mount it at /run/cm-chat/. Returns an empty
// chatResumeDelivery on success when resume is nil. Errors bubble up so
// the caller can degrade to a non-rehydrating start.
//
// Files written:
//   - resume.jsonl: one JSON object per turn (Seq, Role, Content).
//   - resume.meta.json: {prompt_version, clipped, turn_count, ...}
//
// Directory strategy: try SecretsDir (matches the secrets-file approach).
// In dev mode SecretsDir is typically not writable on Linux because the
// default is /var/run/cm-runner/secrets and the runner runs as a regular
// user — fall back to a subdir of os.TempDir() with a WARN log. The
// resume payload is operator-supplied transcript text (not a credential),
// so a tmpfs/tmp-based mount is fine.
func (m *Manager) prepareChatResume(containerName, sessionID, project string, resume *ChatResume) (chatResumeDelivery, error) {
	if resume == nil {
		return chatResumeDelivery{}, nil
	}

	baseDir := m.cfg.SecretsDir
	if baseDir == "" {
		baseDir = "/var/run/cm-runner/secrets" //nolint:gosec // path, not a credential
	}

	if err := m.mkdirAll(baseDir, 0o700); err != nil {
		if isPermissionDenied(err) && m.cfg.IsDev() {
			tmpBase := filepath.Join(os.TempDir(), "cm-runner-chat-resume")
			if mkErr := m.mkdirAll(tmpBase, 0o700); mkErr != nil {
				return chatResumeDelivery{}, fmt.Errorf("create resume base dir (tmp fallback): %w", mkErr)
			}

			m.logger.Warn("dev profile: secrets_dir not writable, falling back to tmp for resume files",
				"secrets_dir", baseDir, "tmp_dir", tmpBase, "error", err)

			baseDir = tmpBase
		} else {
			return chatResumeDelivery{}, fmt.Errorf("create resume base dir: %w", err)
		}
	}

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return chatResumeDelivery{}, fmt.Errorf("generate resume nonce: %w", err)
	}

	dir := filepath.Join(baseDir, "cmr-chat-resume-"+containerName+"-"+hex.EncodeToString(nonce[:]))
	if err := m.mkdirAll(dir, 0o700); err != nil {
		return chatResumeDelivery{}, fmt.Errorf("create resume dir: %w", err)
	}

	jsonlPath := filepath.Join(dir, "resume.jsonl")
	metaPath := filepath.Join(dir, "resume.meta.json")

	jf, err := m.createFile(jsonlPath)
	if err != nil {
		_ = os.RemoveAll(dir)

		return chatResumeDelivery{}, fmt.Errorf("create resume.jsonl: %w", err)
	}

	for _, t := range resume.Turns {
		line, err := json.Marshal(t)
		if err != nil {
			_ = jf.Close()
			_ = os.RemoveAll(dir)

			return chatResumeDelivery{}, fmt.Errorf("marshal resume turn seq=%d: %w", t.Seq, err)
		}

		if _, err := jf.Write(append(line, '\n')); err != nil {
			_ = jf.Close()
			_ = os.RemoveAll(dir)

			return chatResumeDelivery{}, fmt.Errorf("write resume turn seq=%d: %w", t.Seq, err)
		}
	}

	if err := jf.Close(); err != nil {
		_ = os.RemoveAll(dir)

		return chatResumeDelivery{}, fmt.Errorf("close resume.jsonl: %w", err)
	}

	meta := chatResumeMeta{
		PromptVersion: chatproto.PromptVersion,
		Clipped:       resume.Clipped,
		TurnCount:     len(resume.Turns),
		OrigSeqMax:    resume.OrigSeq,
		SessionID:     sessionID,
		Project:       project,
	}

	metaBytes, err := json.Marshal(meta)
	if err != nil {
		_ = os.RemoveAll(dir)

		return chatResumeDelivery{}, fmt.Errorf("marshal resume meta: %w", err)
	}

	mf, err := m.createFile(metaPath)
	if err != nil {
		_ = os.RemoveAll(dir)

		return chatResumeDelivery{}, fmt.Errorf("create resume.meta.json: %w", err)
	}

	if _, err := mf.Write(metaBytes); err != nil {
		_ = mf.Close()
		_ = os.RemoveAll(dir)

		return chatResumeDelivery{}, fmt.Errorf("write resume.meta.json: %w", err)
	}

	if err := mf.Close(); err != nil {
		_ = os.RemoveAll(dir)

		return chatResumeDelivery{}, fmt.Errorf("close resume.meta.json: %w", err)
	}

	return chatResumeDelivery{DirPath: dir}, nil
}

// removeChatResume best-effort unlinks the per-container resume directory.
// No-op when the delivery is empty (rehydration wasn't requested or prep
// failed before any files were written).
func (m *Manager) removeChatResume(d chatResumeDelivery, log *slog.Logger) {
	if d.DirPath == "" {
		return
	}

	if err := os.RemoveAll(d.DirPath); err != nil && !os.IsNotExist(err) {
		log.Warn("failed to remove resume dir", "path", d.DirPath, "error", err)
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
			"Never push to main or master. "+
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

func (m *Manager) waitAndCleanup(ctx context.Context, containerID string, payload RunConfig, secrets []string, log *slog.Logger) string {
	// Defers run LIFO, so the declared order here is the REVERSE of the
	// execution order. We want the tracker entry to disappear first so
	// `/message`, `/promote`, and `/end-session` requests that race with
	// cleanup return 404 (no container tracked) rather than 500 (stdin
	// write against a dead container).
	//
	// Actual execution order:
	//   1. tracker.Remove  — unpublish the entry (also closes stdin).
	//   2. removeContainer — delete the Docker container (bounded ctx
	//      so a hung dockerd cannot stall the goroutine).
	defer func() {
		rmCtx, cancel := withDockerCleanupTimeout(context.Background())
		defer cancel()

		m.removeContainer(rmCtx, containerID, log)
	}()
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
		m.drainLogs(logDone, containerID, payload, log)

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
		// Disambiguate three race-prone outcomes that all manifest as a write
		// to errCh:
		//   1. waitCtx exceeded its deadline (real container timeout) — parent
		//      ctx is still alive. Classify as OutcomeTimeout.
		//   2. parent ctx is already canceled (operator /kill or shutdown).
		//      The Docker SDK writes a context-canceled error into errCh, and
		//      Go's select picks pseudo-randomly between errCh and ctx.Done(),
		//      so we cannot rely on the ctx.Done() branch winning. Treat this
		//      identically to the ctx.Done() branch: OutcomeKilled.
		//   3. Genuine SDK error (dockerd vanished, container weirdness).
		//      Classify as OutcomeFailure.
		//
		// Before this disambiguation, `waitCtx.Err() != nil` was true for both
		// (1) and (2) and the killed-by-operator case was being silently
		// reported to CM and the metrics histogram as a timeout.
		switch {
		case errors.Is(waitCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
			return m.handleWaitTimeout(ctx, containerID, payload, logDone, timeout, log)
		case ctx.Err() != nil:
			return m.handleCanceled(ctx, containerID, payload, logDone, log)
		default:
			return m.handleWaitError(ctx, containerID, payload, logDone, err, log)
		}

	case <-ctx.Done():
		return m.handleCanceled(ctx, containerID, payload, logDone, log)
	}
}

// handleWaitTimeout is the cleanup path for OutcomeTimeout: kill the container
// (best-effort), drain the log stream under logDrainTimeout, then report
// failure to CM via a detached context so the parent's cancellation does not
// turn the callback into a no-op.
func (m *Manager) handleWaitTimeout(ctx context.Context, containerID string, payload RunConfig, logDone <-chan struct{}, timeout time.Duration, log *slog.Logger) string {
	msg := fmt.Sprintf("container timed out after %s", timeout)
	log.Warn(msg)

	killCtx, killCancel := withDockerCleanupTimeout(ctx)
	m.killContainer(killCtx, containerID, log)
	killCancel()

	m.drainLogs(logDone, containerID, payload, log)

	m.emitSystem(payload, "container failed: "+msg)

	cbCtx, cbCancel := withCleanupTimeout(ctx)
	m.reportFailure(cbCtx, payload, msg)
	cbCancel()

	return metrics.OutcomeTimeout
}

// handleWaitError is the cleanup path for an unexpected ContainerWait error
// (dockerd vanished, malformed response, etc.) that is NOT a timeout and NOT
// a parent-ctx cancellation. Mirrors handleWaitTimeout's structure but
// classifies as OutcomeFailure and surfaces the underlying error in the
// failure message.
func (m *Manager) handleWaitError(ctx context.Context, containerID string, payload RunConfig, logDone <-chan struct{}, err error, log *slog.Logger) string {
	msg := fmt.Sprintf("wait error: %v", err)
	log.Error(msg)

	killCtx, killCancel := withDockerCleanupTimeout(ctx)
	m.killContainer(killCtx, containerID, log)
	killCancel()

	m.drainLogs(logDone, containerID, payload, log)

	m.emitSystem(payload, "container failed: "+msg)

	cbCtx, cbCancel := withCleanupTimeout(ctx)
	m.reportFailure(cbCtx, payload, msg)
	cbCancel()

	return metrics.OutcomeFailure
}

// handleCanceled is the cleanup path for OutcomeKilled. Reached both via the
// ctx.Done() select branch AND via the errCh branch when the parent ctx is
// already canceled (Docker SDK's ContainerWait writes a context-canceled
// error into errCh and Go's select picks pseudo-randomly between them).
func (m *Manager) handleCanceled(ctx context.Context, containerID string, payload RunConfig, logDone <-chan struct{}, log *slog.Logger) string {
	log.Info("container canceled")

	killCtx, killCancel := withDockerCleanupTimeout(ctx)
	m.killContainer(killCtx, containerID, log)
	killCancel()

	m.drainLogs(logDone, containerID, payload, log)

	m.emitSystem(payload, "container canceled")

	// Report failure to CM via a detached context: the parent ctx is
	// already cancelled, so passing it to ReportStatus would turn the
	// callback into a no-op and CM would see the card stuck in
	// `running` forever.
	cbCtx, cbCancel := withCleanupTimeout(ctx)
	m.reportFailure(cbCtx, payload, "killed by operator")
	cbCancel()

	return metrics.OutcomeKilled
}

// drainLogs blocks until logDone fires or logDrainTimeout elapses, whichever
// comes first. Identical body across the three cleanup branches — extracted
// so they share exact semantics.
func (m *Manager) drainLogs(logDone <-chan struct{}, containerID string, payload RunConfig, log *slog.Logger) {
	select {
	case <-logDone:
	case <-time.After(logDrainTimeout):
		log.Warn("log drain timed out during cleanup",
			"container_id", truncateID(containerID),
			"card_id", payload.CardID,
			"project", payload.Project,
			"timeout", logDrainTimeout)
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
// not outlive the container it watches.
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
	//
	// NOTE: this child is intentionally NOT tracked by m.wg. card-mode
	// streamLogs lifecycles are self-bounded by waitAndCleanup's logDrainTimeout
	// (so they cannot stall the caller's Wait), and the stdcopy child below
	// can legitimately block forever on a wedged Docker reader that ignores
	// Close — the hung-reader test relies on Wait() returning so the
	// removeContainer + tracker.Remove defers can fire even when streamLogs
	// has not finished. Chat-mode uses a different lifecycle (external ctx
	// cancellation) and DOES track the equivalents on m.wg; the asymmetry is
	// deliberate.
	if idle := m.cfg.IdleOutputTimeout; idle > 0 {
		go m.runIdleWatchdog(ctx, done, containerID, payload, log, &lastOutputAt, idle)
	}

	go func() {
		defer close(done)
		defer func() { _ = reader.Close() }()

		// The three child goroutines spawned below can each take a panic
		// from third-party input — stdcopy on a malformed docker multiplex
		// frame, bufio.Scanner on bogus UTF-8, the logparser on a bad
		// stream-json line. A panic in any of them would otherwise unwind
		// the whole runner process; the recover() wrappers below
		// isolate each goroutine so one bad container can't crash
		// everything else. The outer goroutine runs logparser
		// synchronously, so it shares the outer's recovery — we recover()
		// there too.
		defer func() {
			if r := recover(); r != nil {
				m.handlePanic(r, metrics.GoroutineLogparser, "streamLogs child panicked",
					[]any{
						"goroutine", "logparser",
						"container_id", containerID,
						"card_id", payload.CardID,
						"project", payload.Project,
					},
					&logbroadcast.LogEntry{
						CardID:  payload.CardID,
						Project: payload.Project,
						Type:    "system",
						Content: "internal error: logparser panicked",
					})
			}
		}()

		stdoutPr, stdoutPw := io.Pipe()
		stderrPr, stderrPw := io.Pipe()

		// Wrap both pipe readers so the idle watchdog gets a tick on every
		// raw byte arrival, not just on completed-line emission. With the
		// 16 MiB soft cap a single long line could still take longer than
		// IdleOutputTimeout to terminate; without this wrapper, the
		// watchdog would Kill a healthy container mid-stream.
		bumpProgress := func() {
			nowT := time.Now()
			lastOutputAt.Store(&nowT)
		}
		stdoutFeed := &progressReader{r: stdoutPr, onProgress: bumpProgress}
		stderrFeed := &progressReader{r: stderrPr, onProgress: bumpProgress}

		// stdcopy and the stderr scanner are intentionally NOT tracked by
		// m.wg — see the runIdleWatchdog comment above. A wedged Docker
		// reader can block stdcopy forever, and waitAndCleanup's log-drain
		// timeout is the bound that lets removeContainer / tracker.Remove
		// run regardless.
		go func() {
			defer func() { _ = stdoutPw.Close(); _ = stderrPw.Close() }()
			defer func() {
				if r := recover(); r != nil {
					m.handlePanic(r, metrics.GoroutineStreamStdout, "streamLogs child panicked",
						[]any{
							"goroutine", "stdcopy",
							"container_id", containerID,
							"card_id", payload.CardID,
							"project", payload.Project,
						},
						&logbroadcast.LogEntry{
							CardID:  payload.CardID,
							Project: payload.Project,
							Type:    "system",
							Content: "internal error: stdcopy panicked",
						})
				}
			}()

			_, _ = stdcopy.StdCopy(stdoutPw, stderrPw, reader)
		}()

		// Log stderr lines as warnings and emit to broadcaster.
		go func() {
			// Always close the stderr pipe reader on exit so the writer side
			// (stdcopy) cannot wedge on a synchronous Write into a buffered
			// io.Pipe that no one is draining. Without this, a bufio.Scanner
			// failure (e.g. bufio.ErrTooLong) would leave stdcopy blocked
			// forever, and the outer streamLogs goroutine would never close
			// its `done` channel — wedging tracker.Remove + removeContainer.
			defer func() { _ = stderrPr.Close() }()
			defer func() {
				if r := recover(); r != nil {
					m.handlePanic(r, metrics.GoroutineStreamStderr, "streamLogs child panicked",
						[]any{
							"goroutine", "stderr_scanner",
							"container_id", containerID,
							"card_id", payload.CardID,
							"project", payload.Project,
						},
						&logbroadcast.LogEntry{
							CardID:  payload.CardID,
							Project: payload.Project,
							Type:    "system",
							Content: "internal error: stderr_scanner panicked",
						})
				}
			}()

			// bufio.Reader avoids bufio.Scanner's 1 MiB cap;
			// logparser.ReadBoundedLine adds a soft per-line cap so a
			// runaway stderr stream can't pin the runner heap. Read
			// from the progressReader wrapper so the idle watchdog
			// gets fed on raw byte arrival.
			br := bufio.NewReaderSize(stderrFeed, 64*1024)

			var readErr error

			for {
				var (
					raw       []byte
					truncated bool
				)

				raw, truncated, readErr = logparser.ReadBoundedLine(br, logparser.MaxLineBytes)
				if len(raw) > 0 || truncated {
					line := strings.TrimRight(string(raw), "\r\n")
					if truncated {
						line += " [...truncated; line exceeded 16 MiB cap]"
					}
					// Redact before slog too: a rogue child process echoing the
					// container env through /proc can easily end up on stderr.
					redacted := redactor.Redact(line)
					log.Warn("container stderr", "line", redacted)

					// Record the observation so the idle watchdog sees forward
					// progress. Stderr noise counts: a container spewing stack
					// traces is not "idle".
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

				if readErr != nil {
					break
				}
			}

			// On non-EOF read failure (e.g. an underlying I/O error) the
			// writer side of stderrPr could still block on its next Write
			// if anything is buffered. Drain to discard so stdcopy makes
			// forward progress; the defer above also closes stderrPr so any
			// already-blocked Write returns io.ErrClosedPipe.
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				log.Warn("container stderr reader aborted; draining pipe to unblock writer",
					"container_id", truncateID(containerID),
					"card_id", payload.CardID,
					"project", payload.Project,
					"error", readErr)

				_, _ = io.Copy(io.Discard, stderrPr)
			}
		}()

		// emit is invoked by the logparser for every published assistant
		// text / thinking / tool_use event. We always wire it (even when
		// broadcaster is nil) so the idle watchdog gets fed — that's the
		// whole point of the idle-output watchdog.
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
			m.wg.Add(1)

			go func() {
				defer m.wg.Done()
				defer func() {
					if r := recover(); r != nil {
						m.handlePanic(r, metrics.GoroutineSkillEngagedCallback, "skill-engaged callback panicked",
							[]any{
								"card_id", payload.CardID,
								"project", payload.Project,
								"skill", evt.SkillName,
							},
							&logbroadcast.LogEntry{
								CardID:  payload.CardID,
								Project: payload.Project,
								Type:    "system",
								Content: "internal error: skill-engaged callback panicked",
							})
					}
				}()

				cbCtx, cancel := withCleanupTimeout(context.Background())
				defer cancel()

				if err := m.callback.ReportSkillEngaged(cbCtx, payload.CardID, payload.Project, evt.SkillName); err != nil {
					log.Warn("skill-engaged callback failed", "skill", evt.SkillName, "error", err)
				}
			}()
		}

		logparser.ProcessStreamWithRedactor(stdoutFeed, log, redactor, emit, onSkillEngaged)
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
	defer func() {
		if r := recover(); r != nil {
			m.handlePanic(r, metrics.GoroutineIdleWatchdog, "idle watchdog panicked",
				[]any{
					"container_id", containerID,
					"card_id", payload.CardID,
					"project", payload.Project,
				},
				&logbroadcast.LogEntry{
					CardID:  payload.CardID,
					Project: payload.Project,
					Type:    "system",
					Content: "internal error: idle watchdog panicked",
				})
		}
	}()

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
// Called once per maintenance tick from the reconcile-and-prune loop.
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
// already-closed WriteCloser.
//
// On the timeout branch we also call tracker.MarkStdinClosed so the
// tracker's view of stdin matches reality. Without this, subsequent
// /message webhooks see info.stdin.stdin non-nil and surface the I/O
// error from the closed writer as a generic 500; the mark flips
// info.stdin.stdin to nil so WriteStdin returns ErrStdinClosed (410 Gone)
// instead.
func (m *Manager) writePrimingWithTimeout(payload RunConfig, containerID string, b []byte, writer io.Closer) {
	done := make(chan error, 1)

	m.wg.Add(1)

	go func() {
		defer m.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				m.handlePanic(r, metrics.GoroutinePrimingWrite, "priming stdin write panicked",
					[]any{
						"container_id", truncateID(containerID),
						"card_id", payload.CardID,
						"project", payload.Project,
					},
					nil)

				// Surface the panic as an error so the outer select sees a
				// completion instead of hanging until primingWriteTimeout.
				select {
				case done <- fmt.Errorf("priming stdin write panicked: %v", r):
				default:
				}
			}
		}()

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

		// Reflect the close in the tracker so subsequent WriteStdin calls
		// return ErrStdinClosed (mapped to 410 Gone by the webhook layer)
		// instead of failing the in-flight Write with a generic I/O error
		// (mapped to 500). The Mark is unconditional: if the tracker entry
		// has already been Removed by a racing cleanup, MarkStdinClosed
		// returns ErrNotTracked and we log-and-continue.
		if err := m.tracker.MarkStdinClosed(payload.Project, payload.CardID); err != nil &&
			!errors.Is(err, tracker.ErrNotTracked) &&
			!errors.Is(err, tracker.ErrNoStdinAttached) {
			m.logger.Warn("priming timeout: failed to mark stdin closed in tracker",
				"container_id", truncateID(containerID),
				"card_id", payload.CardID,
				"project", payload.Project,
				"error", err)
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

// KillChat is the chat-mode equivalent of Kill. It closes the container's
// stdin (so claude has a chance to flush its final stream-json batch) and then
// calls Stop, giving the container its normal SIGTERM grace period before
// SIGKILL. If no stdin has been attached yet (the window between AddChat and
// SetStdinChat) the close is skipped and Stop is still called.
func (m *Manager) KillChat(ctx context.Context, sessionID string) error {
	snap, ok := m.tracker.SnapshotChat(sessionID)
	if !ok {
		return fmt.Errorf("KillChat: no chat container tracked for %s", sessionID)
	}

	if err := m.tracker.CloseStdinChat(sessionID); err != nil &&
		!errors.Is(err, tracker.ErrNoStdinAttached) {
		m.logger.Warn("KillChat: close stdin", "session_id", sessionID, "error", err)
	}

	return m.Stop(ctx, snap.ContainerID)
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
	SessionID     string
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
// Two kinds are surfaced: card-mode containers (LabelCardID + LabelProject)
// and chat-mode containers (LabelSessionID, LabelProject optional — a global
// chat may have no project label). Containers with neither identifier set are
// skipped — they are neither reachable via /kill (which routes by labels) nor
// the sweep's responsibility. Such containers still exist in Docker and are
// caught by CleanupOrphans on the next maintenance tick.
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
		sessionID := ctr.Labels[LabelSessionID]

		// Card containers must carry project + card_id (chat containers may
		// omit project for global chats).
		if cardID != "" && project == "" {
			continue
		}
		// At least one identifier must be present.
		if cardID == "" && sessionID == "" {
			continue
		}

		name := ""
		if len(ctr.Names) > 0 {
			// Docker prefixes container names with "/"; strip it so the
			// wire shape matches what `docker ps` prints.
			name = strings.TrimPrefix(ctr.Names[0], "/")
		}

		tracked := false

		if m.tracker != nil {
			if sessionID != "" {
				tracked = m.tracker.HasChat(sessionID)
			} else {
				tracked = m.tracker.Has(project, cardID)
			}
		}

		result = append(result, ManagedContainer{
			ContainerID:   ctr.ID,
			ContainerName: name,
			CardID:        cardID,
			SessionID:     sessionID,
			Project:       project,
			State:         ctr.State,
			StartedAt:     time.Unix(ctr.Created, 0).UTC(),
			Tracked:       tracked,
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
// Only Remove failures are returned as errors. A Stop failure that ended
// in a successful Remove is logged as a warning but not surfaced to the
// caller — the container is gone, which is the only outcome that matters.
// Returning a Stop failure as a hard error would mislead callers into
// thinking cleanup did not complete, even when every orphan was
// ultimately destroyed.
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
	// on every tick. Chat containers are tracked by LabelSessionID instead
	// of LabelCardID, so the chat path is checked separately.
	orphans := make([]DockerContainer, 0, len(containers))
	skipped := 0

	for _, ctr := range containers {
		project := ctr.Labels[LabelProject]
		cardID := ctr.Labels[LabelCardID]
		sessionID := ctr.Labels[LabelSessionID]

		if m.tracker != nil && project != "" && cardID != "" && m.tracker.Has(project, cardID) {
			skipped++

			continue
		}

		if m.tracker != nil && sessionID != "" && m.tracker.HasChat(sessionID) {
			skipped++

			continue
		}

		orphans = append(orphans, ctr)
	}

	// removeErrs collects only the failures that left a container behind.
	// Stop failures are surfaced through the per-container warning log;
	// they don't go into removeErrs because the subsequent force-Remove
	// is the authoritative "did it actually go away?" check.
	var (
		removeErrs   []error
		stopFailures int
		removed      int
	)

	for _, ctr := range orphans {
		idShort := truncateID(ctr.ID)
		m.logger.Info("cleaning up orphan container",
			"container_id", idShort,
			"card_id", ctr.Labels[LabelCardID],
			"session_id", ctr.Labels[LabelSessionID],
			"project", ctr.Labels[LabelProject],
		)

		// Each per-container Stop/Remove is bounded so one wedged orphan
		// can't stall cleanup of the rest. Using a fresh
		// dockerCleanupTimeout-scoped ctx here (rather than a child of
		// the caller's ctx) matches the semantics we want on shutdown:
		// we'd rather give up quickly and log than wait for a hung
		// daemon.
		stopCtx, stopCancel := withDockerCleanupTimeout(ctx)

		stopTimeout := 5
		if stopErr := m.docker.ContainerStop(stopCtx, ctr.ID, container.StopOptions{Timeout: &stopTimeout}); stopErr != nil {
			// Stop failure is a warning, not an error: the force-Remove
			// below is the authoritative cleanup. If Remove succeeds the
			// container is gone regardless of what Stop reported.
			m.logger.Warn("orphan stop failed (will still attempt force-remove)",
				"container_id", idShort,
				"card_id", ctr.Labels[LabelCardID],
				"session_id", ctr.Labels[LabelSessionID],
				"project", ctr.Labels[LabelProject],
				"error", stopErr,
			)

			stopFailures++
		}

		stopCancel()

		rmCtx, rmCancel := withDockerCleanupTimeout(ctx)

		if rmErr := m.docker.ContainerRemove(rmCtx, ctr.ID, container.RemoveOptions{Force: true}); rmErr != nil {
			m.logger.Warn("orphan remove failed",
				"container_id", idShort,
				"card_id", ctr.Labels[LabelCardID],
				"session_id", ctr.Labels[LabelSessionID],
				"project", ctr.Labels[LabelProject],
				"error", rmErr,
			)
			removeErrs = append(removeErrs, fmt.Errorf("remove orphan %s: %w", idShort, rmErr))
		} else {
			removed++
		}

		rmCancel()
	}

	if len(orphans) > 0 || skipped > 0 {
		m.logger.Info("orphan cleanup complete",
			"removed", removed,
			"attempted", len(orphans),
			"tracked_skipped", skipped,
			"stop_warnings", stopFailures,
			"remove_errors", len(removeErrs),
		)
	}

	return errors.Join(removeErrs...)
}

// SweepStaleChatResumeDirs removes cmr-chat-resume-* host directories that are
// older than maxAge. These directories are created by prepareChatResume and
// cleaned up by WaitAndCleanupChat; any that survive are crash-leak artefacts.
// Candidates are searched under SecretsDir (primary) and os.TempDir() (dev
// fallback). Per-entry removal errors are logged and silently swallowed — this
// is a best-effort janitor.
func (m *Manager) SweepStaleChatResumeDirs(maxAge time.Duration) {
	secretsDir := m.cfg.SecretsDir
	if secretsDir == "" {
		secretsDir = "/var/run/cm-runner/secrets" //nolint:gosec // path, not a credential
	}

	parents := []string{secretsDir, filepath.Join(os.TempDir(), "cm-runner-chat-resume")}

	for _, parent := range parents {
		entries, err := os.ReadDir(parent)
		if err != nil {
			// Directory may not exist; that's fine.
			continue
		}

		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "cmr-chat-resume-") {
				continue
			}

			info, err := e.Info()
			if err != nil {
				// A persistent stat failure would leak the dir forever
				// silently; surface it so an operator can intervene.
				m.logger.Warn("sweep: failed to stat candidate chat resume dir",
					"path", filepath.Join(parent, e.Name()), "error", err)

				continue
			}

			if time.Since(info.ModTime()) <= maxAge {
				continue
			}

			target := filepath.Join(parent, e.Name())
			if err := os.RemoveAll(target); err != nil {
				m.logger.Warn("sweep: failed to remove stale chat resume dir",
					"path", target, "age", time.Since(info.ModTime()).Round(time.Second), "error", err)
			} else {
				m.logger.Info("sweep: removed stale chat resume dir",
					"path", target, "age", time.Since(info.ModTime()).Round(time.Second))
			}
		}
	}
}

// imagePullProgressLineMax caps how many NDJSON status lines pullImage will
// decode from the ImagePull stream. The Docker daemon emits one line per
// pulled layer plus periodic progress updates, so a generous bound of 10000
// covers every realistic registry response while preventing a malicious or
// runaway registry from exhausting the goroutine via an unbounded stream.
const imagePullProgressLineMax = 10_000

// imagePullProgress is the subset of the ImagePull NDJSON status frames we
// care about. The Docker daemon multiplexes layer-progress events with
// terminal error events; both share the same envelope, with the error
// communicated in either the top-level `error` field or
// `errorDetail.message` (depending on daemon version). Other fields are
// ignored.
type imagePullProgress struct {
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
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
		// Bound the inspect so a wedged dockerd cannot stall the spawn
		// path indefinitely. On timeout we fall through to the pull
		// branch — that path has its own (longer) imagePullTimeout cap.
		inspectCtx, cancel := context.WithTimeout(ctx, imageInspectTimeout)

		_, err := m.docker.ImageInspect(inspectCtx, img)

		cancel()

		if err == nil {
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

	// Decode the NDJSON progress stream rather than discarding it. Without
	// this, registry-side errors (manifest unknown, unauthorised, etc.)
	// were silently swallowed and only surfaced later as the much less
	// actionable `ContainerCreate: no such image`. Decoding here lets us
	// fail the pull at the source so the caller's structured error
	// includes the registry's actual reason.
	dec := json.NewDecoder(reader)

	var pullErr error

	for i := 0; i < imagePullProgressLineMax; i++ {
		var msg imagePullProgress

		decErr := dec.Decode(&msg)
		if errors.Is(decErr, io.EOF) {
			break
		}

		if decErr != nil {
			// Malformed or truncated stream. Log and stop reading; rely on
			// any later error from ImageInspect / ContainerCreate to
			// surface a downstream symptom. We do not return this error
			// because a partial valid prefix may have completed the pull
			// successfully on older daemons.
			m.logger.Warn("failed to decode image pull progress",
				"image", img, "error", decErr)

			break
		}

		// Pin the first registry-reported error. Continuing to drain the
		// stream lets the daemon close it cleanly; returning early without
		// draining can leak the underlying HTTP connection on some SDK
		// versions.
		if pullErr == nil {
			switch {
			case msg.Error != "":
				pullErr = fmt.Errorf("pull image %s: registry error: %s", img, msg.Error)
			case msg.ErrorDetail.Message != "":
				pullErr = fmt.Errorf("pull image %s: registry error: %s", img, msg.ErrorDetail.Message)
			}
		}
	}

	if err := reader.Close(); err != nil {
		m.logger.Warn("failed to close image pull reader", "error", err)
	}

	return pullErr
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
// still reach the MCP server if in-container DNS works.
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
		// Distinguish three failure modes so operators know which knob to
		// turn: deadline (DNS latency), lookup error (e.g. NXDOMAIN), and
		// zero-result-no-error (resolver returned an empty slice without
		// reporting an error — surface as a dedicated log line rather than
		// emitting `error=<nil>`, which is misleading).
		switch {
		case errors.Is(err, context.DeadlineExceeded) || lookupCtx.Err() != nil:
			if m.metrics != nil {
				m.metrics.DNSLookupTimeoutsTotal.Inc()
			}

			m.logger.Warn("MCP hostname lookup timed out; container will run without ExtraHosts mapping",
				"hostname", hostname, "timeout", dnsLookupTimeout)
		case err != nil:
			m.logger.Warn("could not resolve MCP hostname for container",
				"hostname", hostname, "error", err)
		default:
			// LookupHost returned (nil, nil): resolver-specific quirk where
			// the hostname has no A/AAAA records but the query did not fail.
			m.logger.Warn("MCP hostname resolved to zero addresses; container will run without ExtraHosts mapping",
				"hostname", hostname)
		}

		return hosts
	}

	m.dnsCache.put(hostname, addrs)

	hosts = append(hosts, hostname+":"+addrs[0])
	m.logger.Info("added MCP host to container", "hostname", hostname, "ip", addrs[0])

	return hosts
}

// StartChatOpts configures a chat-mode container spawn.
type StartChatOpts struct {
	// SessionID is required; the container is labelled and the CM_CHAT_SESSION
	// env var is set to this value.
	SessionID string
	// Project is optional; sets CM_CHAT_PROJECT when non-empty.
	Project string
	// RepoURL is optional; sets CM_CHAT_REPO_URL when non-empty.
	RepoURL string
	// MCPURL is the URL the in-container claude uses to reach CM's MCP
	// endpoint (e.g. http://host.docker.internal:8080/mcp). Sets
	// CM_MCP_URL. Without this, the entrypoint skips the MCP merge and
	// chat sessions cannot call ContextMatrix tools.
	MCPURL string
	// MCPAPIKey authenticates MCP calls. Sets CM_MCP_API_KEY. Empty is
	// permitted when CM's MCP listener has no auth (loopback dev mode).
	MCPAPIKey string
	// GitToken is the GitHub installation token used solely for the runner's
	// own taskSkillsMount git pull and never reaches the container env or
	// shared secrets file.
	GitToken string
	// Model is the Claude model the chat container should run. Sets
	// CM_ORCHESTRATOR_MODEL; the entrypoint passes this to `claude --model`.
	// Empty falls back to the entrypoint default (claude-sonnet-4-6).
	Model string
	// Resume, when non-nil, makes the runner write /run/cm-chat/resume.jsonl
	// and /run/cm-chat/resume.meta.json into the container and set
	// CM_CHAT_RESUME=1. The entrypoint switches to the rehydration prompt
	// branch when this flag is present.
	Resume *ChatResume
	// TaskSkills mirrors RunConfig.TaskSkills for chat-mode workers. nil =
	// no constraint (entrypoint copies the full set from /host-skills);
	// non-nil (even empty) = explicit selection via CM_TASK_SKILLS_SET +
	// CM_TASK_SKILLS. The /host-skills bind-mount is added whenever
	// cfg.TaskSkillsDir is set, regardless of this field.
	TaskSkills *[]string
}

// ChatResume mirrors the wire shape of the rehydration payload. Defined
// here (rather than re-using webhook.ChatResumeContext) so the container
// package has no upward dependency on webhook types.
type ChatResume struct {
	Turns   []ChatResumeTurn
	Clipped bool
	OrigSeq int64
}

// ChatResumeTurn is one filtered, possibly summarized transcript entry.
type ChatResumeTurn struct {
	Seq     int64  `json:"seq"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

// StartChat creates and starts a chat-mode worker container. The container
// runs claude in stream-json mode; the entrypoint dispatches on
// CM_CHAT_SESSION. Returns the container ID. The caller is responsible for
// (1) registering the container in the tracker via AddChat, and (2) attaching
// stdin via AttachChatStdin so /message webhooks can route user turns in.
func (m *Manager) StartChat(ctx context.Context, opts StartChatOpts) (string, error) {
	if opts.SessionID == "" {
		return "", fmt.Errorf("StartChat: SessionID is required")
	}

	img := m.cfg.BaseImage

	m.logger.Info("chat container: starting",
		"session_id", opts.SessionID,
		"project", opts.Project,
		"image", img)

	// Pull image according to policy.
	if err := m.pullImage(ctx, img); err != nil {
		return "", fmt.Errorf("StartChat: pull image: %w", err)
	}

	// Build env — chat-specific values. CM_MCP_API_KEY is per-session and
	// doesn't rotate, so it rides directly in Env. Rotating credentials
	// (CM_GIT_TOKEN, Claude auth) are owned by the tokenRefresher and reach
	// the container via the shared secrets dir mount.
	env := []string{
		"CM_CHAT_SESSION=" + opts.SessionID,
	}

	if opts.MCPAPIKey != "" {
		env = append(env, "CM_MCP_API_KEY="+opts.MCPAPIKey)
	}

	if opts.Project != "" {
		env = append(env, "CM_CHAT_PROJECT="+opts.Project)
	}

	if opts.RepoURL != "" {
		env = append(env, "CM_CHAT_REPO_URL="+normalizeRepoURL(opts.RepoURL))
	}

	if opts.MCPURL != "" {
		env = append(env, "CM_MCP_URL="+opts.MCPURL)
	}

	if opts.Model != "" {
		env = append(env, "CM_ORCHESTRATOR_MODEL="+opts.Model)
	}

	env = m.appendCommonEnv(env, opts.TaskSkills)

	// Sanitise a container name from session ID (may contain arbitrary chars).
	// Truncate at 63 chars — Docker's enforced container name limit.
	name := "cmr-chat-" + containerNameRe.ReplaceAllString(strings.ToLower(opts.SessionID), "-")
	if len(name) > 63 {
		name = name[:63]
	}

	delivery, err := m.buildSecretDelivery(ctx)
	if err != nil {
		return "", fmt.Errorf("StartChat: build secret delivery: %w", err)
	}

	// Materialise the rehydration payload (resume.jsonl + resume.meta.json)
	// into a per-container directory so it can be bind-mounted at
	// /run/cm-chat/. Failure here is non-fatal — the container starts
	// without rehydration and the entrypoint detects the missing file.
	var resumeDelivery chatResumeDelivery

	if opts.Resume != nil {
		var resumeErr error

		resumeDelivery, resumeErr = m.prepareChatResume(name, opts.SessionID, opts.Project, opts.Resume)
		if resumeErr != nil {
			m.logger.Warn("StartChat: rehydration file prep failed; starting fresh agent",
				"session_id", opts.SessionID, "error", resumeErr)
		} else {
			env = append(env, "CM_CHAT_RESUME=1")
		}
	}

	containerCfg := &container.Config{
		Image: img,
		Env:   env,
		Labels: map[string]string{
			LabelRunner:    "true",
			LabelSessionID: opts.SessionID,
		},
		OpenStdin:   true,
		AttachStdin: true,
		// Tty and StdinOnce intentionally left false.
	}
	if opts.Project != "" {
		containerCfg.Labels[LabelProject] = opts.Project
	}

	// Auth-dir bind mount is shared with card-mode.
	var mounts []mount.Mount
	if authMount, ok := m.claudeAuthMount(); ok {
		mounts = append(mounts, authMount)
	}

	// opts.GitToken is used only for the runner's own git pull of the skills
	// repo — it does NOT reach the container env or secrets.
	if skillsMount, ok := m.taskSkillsMount(ctx, opts.GitToken); ok {
		mounts = append(mounts, skillsMount)
	}

	if caMount, ok := m.caCertMount(); ok {
		mounts = append(mounts, caMount)
	}

	// Collect secret values for the per-container Redactor. See
	// collectSecretValues for the threat-model and CM_GIT_TOKEN-absent
	// rationale. Shared with card-mode startContainer so both modes redact
	// the same set of values.
	secretValues := collectSecretValues(opts.MCPAPIKey, delivery)

	env, mounts = applySecretsDelivery(env, mounts, delivery)

	if resumeDelivery.DirPath != "" {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   resumeDelivery.DirPath,
			Target:   chatResumeMountTarget,
			ReadOnly: true,
		})
	}

	// Keep containerCfg.Env in sync with env (in case secretModeEnvVar or
	// CM_CHAT_RESUME mutations happened after the initial assignment).
	containerCfg.Env = env

	resp, err := m.docker.ContainerCreate(ctx, containerCfg, m.baseHostConfig(ctx, opts.MCPURL, mounts), nil, nil, name)
	if err != nil {
		m.removeChatResume(resumeDelivery, m.logger)

		return "", fmt.Errorf("StartChat: create container: %w", err)
	}

	if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up the created-but-not-started container.
		rmCtx, rmCancel := withDockerCleanupTimeout(ctx)
		defer rmCancel()

		if rmErr := m.docker.ContainerRemove(rmCtx, resp.ID, container.RemoveOptions{Force: true}); rmErr != nil {
			m.logger.Warn("StartChat: failed to remove container after start failure",
				"container_id", resp.ID, "error", rmErr)
		}

		m.removeChatResume(resumeDelivery, m.logger)

		return "", fmt.Errorf("StartChat: start container: %w", err)
	}

	m.logger.Info("chat container: started",
		"session_id", opts.SessionID, "container_id", resp.ID, "name", name,
		"resume_active", resumeDelivery.DirPath != "")

	// Lifecycle of the wait-and-cleanup goroutine is owned by the
	// webhook handler. It registers the tracker entry first (so an
	// instant container exit cannot race the entry's creation), then
	// calls WaitAndCleanupChat once registration succeeds. Stash the
	// delivery handles here so the goroutine can release them on exit
	// without needing the secret/resume types in its signature.
	m.chatCleanupMu.Lock()
	m.chatCleanup[resp.ID] = resumeDelivery
	m.chatCleanupMu.Unlock()

	// Stash the per-container secret values so StreamChatLogs can build a
	// Redactor without modifying the webhook-facing interface. The map is
	// keyed by container ID; StreamChatLogs pops the entry once it has
	// constructed its redactor so an idle long-running runner doesn't
	// retain dead entries.
	m.chatSecretsMu.Lock()
	m.chatSecrets[resp.ID] = secretValues
	m.chatSecretsMu.Unlock()

	return resp.ID, nil
}

// DeleteChatCleanup pops the cleanup state stashed by StartChat for the given
// container ID and removes the chat resume dir. Called by webhook rollback
// paths that abort BEFORE invoking WaitAndCleanupChat — in those paths the
// wait goroutine is not running, so no one else will perform the cleanup.
// Safe to call on an unknown ID: returns without touching the filesystem.
// The shared secrets dir is NOT removed — it is owned by the tokenRefresher.
func (m *Manager) DeleteChatCleanup(containerID string) {
	m.chatCleanupMu.Lock()
	rd, ok := m.chatCleanup[containerID]
	delete(m.chatCleanup, containerID)
	m.chatCleanupMu.Unlock()

	// Also drop the stashed redaction inputs so a rollback path doesn't
	// leak entries when StreamChatLogs never gets called to consume them.
	m.chatSecretsMu.Lock()
	delete(m.chatSecrets, containerID)
	m.chatSecretsMu.Unlock()

	if !ok {
		return
	}

	m.removeChatResume(rd, m.logger)
}

// WaitAndCleanupChat launches a wg-tracked goroutine that waits for the chat
// container to exit (any reason — claude crash, OOM, external kill, or a
// /chat/end-triggered Stop) and then unconditionally removes the tracker
// entry and force-removes the container.
//
// /chat/end already performs this teardown explicitly, so the goroutine
// races it; that's fine — every step is idempotent (RemoveChat tolerates
// missing entries; ContainerRemove on a removed container is logged and
// swallowed). The point of this goroutine is to catch the *implicit* exit
// paths where /chat/end is never called: claude crash, OOM, exec failure,
// or `docker kill` from outside the runner. Without it those paths would
// leak the tracker entry until the runner restarts.
//
// Uses context.Background() so the wait survives the request ctx that
// kicked off /chat/start. The associated log-stream goroutine (from
// StreamChatLogs) exits on its own when ContainerLogs EOFs on container
// removal, so no explicit cancel is required here.
//
// The webhook handler owns the lifecycle: StartChat returns the container
// ID after stashing the delivery handles in m.chatCleanup, and the handler
// invokes WaitAndCleanupChat only after registering the tracker entry. The
// secret / resume cleanup handles are popped from the chatCleanup map so
// callers don't need to thread them through the signature.
func (m *Manager) WaitAndCleanupChat(sessionID, containerID, project string) {
	m.wg.Add(1)

	go func() {
		defer m.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				m.handlePanic(r, metrics.GoroutineWaitAndCleanupChat, "chat wait-and-cleanup panicked",
					[]any{"session_id", sessionID, "container_id", containerID, "project", project},
					nil)
			}
		}()

		m.chatCleanupMu.Lock()
		rd, hasResume := m.chatCleanup[containerID]
		delete(m.chatCleanup, containerID)
		m.chatCleanupMu.Unlock()

		log := m.logger.With("session_id", sessionID, "container_id", containerID, "project", project)

		ctx := context.Background()

		waitCh, errCh := m.docker.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)

		select {
		case result := <-waitCh:
			log.Info("chat container: exited",
				"exit_code", result.StatusCode)
		case err := <-errCh:
			log.Warn("chat container: wait error", "error", err)
		}

		m.tracker.RemoveChat(sessionID)

		rmCtx, cancel := withDockerCleanupTimeout(ctx)
		defer cancel()

		m.removeContainer(rmCtx, containerID, log)

		// Remove the per-session chat resume dir. The shared secrets dir is
		// owned by the tokenRefresher and must not be removed here.
		if hasResume {
			m.removeChatResume(rd, log)
		}

		// Drop the stashed secret values regardless of whether
		// StreamChatLogs ever consumed them. The /chat/start rollback path
		// that calls AttachChatStdin and fails routes through
		// RemoveChat + Stop + WaitAndCleanupChat without touching
		// DeleteChatCleanup, which would otherwise leak the entry forever. Idempotent
		// on a key that was already consumed by StreamChatLogs.
		m.chatSecretsMu.Lock()
		delete(m.chatSecrets, containerID)
		m.chatSecretsMu.Unlock()
	}()
}

// AttachChatStdin opens the chat container's stdin and registers the writer
// with the tracker under the given sessionID. Without this, /message webhooks
// cannot route user turns into the running claude process. Must be called
// AFTER tracker.AddChat for the session.
func (m *Manager) AttachChatStdin(ctx context.Context, sessionID, containerID string) error {
	attached, err := m.docker.ContainerAttach(ctx, containerID, container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: false,
		Stderr: false,
	})
	if err != nil {
		return fmt.Errorf("AttachChatStdin: %w", err)
	}

	m.tracker.SetStdinChat(sessionID, attached.Conn, attached.Close)
	m.logger.Info("chat container: stdin attached",
		"session_id", sessionID, "container_id", containerID)

	return nil
}

// StreamChatLogs follows the chat container's stdout/stderr in a background
// goroutine and republishes claude stream-json events as LogEntry values on
// the broadcaster, keyed by sessionID. The goroutine exits when ctx is
// cancelled or the log reader EOFs (container exit). Caller must hold a
// reference to ctx and cancel it on session end or shutdown; the outer
// goroutine plus each of its three children (stdcopy, stderr scanner, and
// logparser) are tracked by m.wg so Wait() drains them.
//
// Secrets stashed by StartChat under containerID are popped here and used
// to construct a per-container Redactor. Without redaction, CM_MCP_API_KEY
// and any env-var-mode secrets could leak into the /logs SSE stream and
// slog output via container stdout/stderr. Each inner goroutine is wrapped
// in a recover() so a malformed stream-json or stdcopy frame in one chat
// container does not crash the whole runner.
func (m *Manager) StreamChatLogs(ctx context.Context, sessionID, containerID, project string) {
	// Pop the redaction inputs stashed by StartChat under containerID. Empty
	// when StartChat wasn't called (tests that exercise StreamChatLogs in
	// isolation) — NewRedactor handles a nil slice safely.
	m.chatSecretsMu.Lock()
	secrets := m.chatSecrets[containerID]
	delete(m.chatSecrets, containerID)
	m.chatSecretsMu.Unlock()

	m.wg.Add(1)

	go func() {
		defer m.wg.Done()

		log := m.logger.With("session_id", sessionID, "container_id", containerID)

		redactor := logparser.NewRedactor(secrets)

		reader, err := m.docker.ContainerLogs(ctx, containerID, container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Follow:     true,
		})
		if err != nil {
			log.Warn("chat container: ContainerLogs failed", "error", err)

			return
		}

		defer func() { _ = reader.Close() }()

		// The three child goroutines spawned below can each take a panic
		// from third-party input — stdcopy on a malformed docker multiplex
		// frame, bufio.Scanner on bogus UTF-8, the logparser on a bad
		// stream-json line. Mirror the card-mode streamLogs structure
		// (lines ~1141-1349) so a panic in any of them is recovered with
		// metric + slog + system LogEntry, and so Wait() drains all three.
		defer func() {
			if r := recover(); r != nil {
				m.handlePanic(r, metrics.GoroutineLogparser, "chat streamLogs child panicked",
					[]any{
						"goroutine", "logparser",
						"container_id", containerID,
						"session_id", sessionID,
						"project", project,
					},
					&logbroadcast.LogEntry{
						SessionID: sessionID,
						Project:   project,
						Type:      "system",
						Content:   "internal error: logparser panicked",
					})
			}
		}()

		stdoutPr, stdoutPw := io.Pipe()
		stderrPr, stderrPw := io.Pipe()

		m.wg.Add(1)

		go func() {
			defer m.wg.Done()
			defer func() { _ = stdoutPw.Close(); _ = stderrPw.Close() }()
			defer func() {
				if r := recover(); r != nil {
					m.handlePanic(r, metrics.GoroutineStreamStdout, "chat streamLogs child panicked",
						[]any{
							"goroutine", "stdcopy",
							"container_id", containerID,
							"session_id", sessionID,
							"project", project,
						},
						&logbroadcast.LogEntry{
							SessionID: sessionID,
							Project:   project,
							Type:      "system",
							Content:   "internal error: stdcopy panicked",
						})
				}
			}()

			_, _ = stdcopy.StdCopy(stdoutPw, stderrPw, reader)
		}()

		m.wg.Add(1)

		go func() {
			defer m.wg.Done()
			// Mirror card-mode: always close the stderr pipe reader so the
			// writer side (stdcopy) cannot wedge on a Write that no one is
			// draining. Without this, a bufio.Scanner failure (e.g.
			// bufio.ErrTooLong) would block stdcopy forever and the outer
			// chat log-stream goroutine would never see EOF.
			defer func() { _ = stderrPr.Close() }()
			defer func() {
				if r := recover(); r != nil {
					m.handlePanic(r, metrics.GoroutineStreamStderr, "chat streamLogs child panicked",
						[]any{
							"goroutine", "stderr_scanner",
							"container_id", containerID,
							"session_id", sessionID,
							"project", project,
						},
						&logbroadcast.LogEntry{
							SessionID: sessionID,
							Project:   project,
							Type:      "system",
							Content:   "internal error: stderr_scanner panicked",
						})
				}
			}()

			// bufio.Reader avoids bufio.Scanner's 1 MiB cap;
			// logparser.ReadBoundedLine adds a soft per-line cap so a
			// runaway stderr stream can't pin the runner heap.
			br := bufio.NewReaderSize(stderrPr, 64*1024)

			var readErr error

			for {
				var (
					raw       []byte
					truncated bool
				)

				raw, truncated, readErr = logparser.ReadBoundedLine(br, logparser.MaxLineBytes)
				if len(raw) > 0 || truncated {
					line := strings.TrimRight(string(raw), "\r\n")
					if truncated {
						line += " [...truncated; line exceeded 16 MiB cap]"
					}
					// Redact before slog too: a rogue child process echoing the
					// container env through /proc can easily end up on stderr.
					redacted := redactor.Redact(line)
					log.Warn("chat container stderr", "line", redacted)

					if m.broadcaster != nil {
						m.broadcaster.Publish(logbroadcast.LogEntry{
							Timestamp: time.Now(),
							SessionID: sessionID,
							Project:   project,
							Type:      "stderr",
							Content:   redacted,
						})
					}
				}

				if readErr != nil {
					break
				}
			}

			// On non-EOF read failure drain to discard so stdcopy makes
			// forward progress; the defer above also closes stderrPr so an
			// already-blocked Write returns io.ErrClosedPipe.
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				log.Warn("chat container stderr reader aborted; draining pipe to unblock writer",
					"container_id", containerID,
					"session_id", sessionID,
					"project", project,
					"error", readErr)

				_, _ = io.Copy(io.Discard, stderrPr)
			}
		}()

		emit := func(e logbroadcast.LogEntry) {
			if m.broadcaster == nil {
				return
			}

			e.Timestamp = time.Now()
			e.SessionID = sessionID
			e.Project = project
			e.CardID = ""
			m.broadcaster.Publish(e)
		}

		logparser.ProcessStreamWithRedactor(stdoutPr, log, redactor, emit, nil)

		log.Info("chat container: log stream ended")
	}()
}

// Stop stops and force-removes a container by ID. It is a thin wrapper used
// for rollback on /chat/start when tracker registration fails after the
// container is already running. Errors are collected but not fatal — the
// caller logs and moves on.
func (m *Manager) Stop(ctx context.Context, containerID string) error {
	stopCtx, stopCancel := withDockerCleanupTimeout(ctx)
	defer stopCancel()

	grace := stopGracePeriod

	var errs []error

	if err := m.docker.ContainerStop(stopCtx, containerID, container.StopOptions{Timeout: &grace}); err != nil {
		errs = append(errs, fmt.Errorf("stop: %w", err))
	}

	rmCtx, rmCancel := withDockerCleanupTimeout(ctx)
	defer rmCancel()

	if err := m.docker.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true}); err != nil {
		errs = append(errs, fmt.Errorf("remove: %w", err))
	}

	return errors.Join(errs...)
}

// WorkerImage returns the configured base image name. Used by the webhook
// handler to record which image a chat container was started from.
func (m *Manager) WorkerImage() string {
	return m.cfg.BaseImage
}

// claudeAuthMount returns the read-only bind for the host's Claude auth dir
// when m.cfg.ClaudeAuthDir is configured, plus true; otherwise the zero
// Mount and false. Both card-mode and chat-mode use this so the auth
// priority cascade (claude_auth_dir > claude_oauth_token > anthropic_api_key)
// stays consistent across modes.
func (m *Manager) claudeAuthMount() (mount.Mount, bool) {
	if m.cfg.ClaudeAuthDir == "" {
		return mount.Mount{}, false
	}

	return mount.Mount{
		Type:     mount.TypeBind,
		Source:   m.cfg.ClaudeAuthDir,
		Target:   "/claude-auth",
		ReadOnly: true,
	}, true
}

// caCertMount returns the read-only bind for the optional extra-CA PEM
// (m.cfg.CACertFile) at caCertMountTarget, plus true; otherwise the zero Mount
// and false. Both card-mode and chat-mode use this so a deployment behind a
// TLS-inspecting proxy trusts the same CA regardless of worker mode. The
// matching NODE_EXTRA_CA_CERTS env var is set in appendCommonEnv.
func (m *Manager) caCertMount() (mount.Mount, bool) {
	if m.cfg.CACertFile == "" {
		return mount.Mount{}, false
	}

	return mount.Mount{
		Type:     mount.TypeBind,
		Source:   m.cfg.CACertFile,
		Target:   caCertMountTarget,
		ReadOnly: true,
	}, true
}

// appendCommonEnv appends deployment-wide env vars consumed by every spawned
// worker — CM_CLAUDE_SETTINGS, CM_TASK_SKILLS_SET/CM_TASK_SKILLS, and the
// WorkerExtraEnv map (sorted by key). Both card-mode startContainer and
// chat-mode StartChat call this so a new deployment-wide knob added here
// automatically lands in every worker, regardless of mode.
func (m *Manager) appendCommonEnv(env []string, taskSkills *[]string) []string {
	if m.cfg.ClaudeSettings != "" {
		env = append(env, "CM_CLAUDE_SETTINGS="+m.cfg.ClaudeSettings)
	}

	if m.cfg.TaskSkillsDir != "" && taskSkills != nil {
		env = append(env, "CM_TASK_SKILLS_SET=1")
		env = append(env, "CM_TASK_SKILLS="+strings.Join(*taskSkills, ","))
	}

	// Point Node (Claude Code) at the bind-mounted extra CA when configured.
	// The matching read-only mount is added by caCertMount() at both
	// startContainer and StartChat. NODE_EXTRA_CA_CERTS is blocked in
	// worker_extra_env (see config.Validate) so this is the only setter.
	if m.cfg.CACertFile != "" {
		env = append(env, "NODE_EXTRA_CA_CERTS="+caCertMountTarget)
	}

	if len(m.cfg.WorkerExtraEnv) > 0 {
		extraKeys := slices.Sorted(maps.Keys(m.cfg.WorkerExtraEnv))
		for _, k := range extraKeys {
			env = append(env, k+"="+m.cfg.WorkerExtraEnv[k])
		}
	}

	return env
}

// taskSkillsMount returns the read-only /host-skills bind-mount when
// m.cfg.TaskSkillsDir is configured, plus true; otherwise the zero Mount and
// false. Best-effort `git pull` of the local clone runs first using gitToken;
// pull failure is logged and the mount is still returned so the container
// starts with the existing on-disk state. Mirrors the claudeAuthMount call
// pattern so card-mode and chat-mode stay symmetric.
//
// Concurrent spawns share the same on-disk TaskSkillsDir, so the pull is
// serialised under m.skillsPullMu. `git pull` writes .git/index.lock and is
// not safe to interleave against itself; without the mutex two simultaneous
// spawns can fail with "Another git process seems to be running" and leave
// a stale lock file behind. The mutex is acquired inside the function (after
// the empty-dir guard) so callers don't need to know about it.
func (m *Manager) taskSkillsMount(ctx context.Context, gitToken string) (mount.Mount, bool) {
	if m.cfg.TaskSkillsDir == "" {
		return mount.Mount{}, false
	}

	m.skillsPullMu.Lock()
	err := pullSkillsRepo(ctx, m.cfg.TaskSkillsDir, gitToken)
	m.skillsPullMu.Unlock()

	if err != nil {
		m.logger.Warn("task skills pull failed; using existing local clone",
			"task_skills_dir", m.cfg.TaskSkillsDir,
			"error", err,
		)
	}

	return mount.Mount{
		Type:     mount.TypeBind,
		Source:   m.cfg.TaskSkillsDir,
		Target:   "/host-skills",
		ReadOnly: true,
	}, true
}

// baseHostConfig assembles the HostConfig pieces every spawned worker shares:
// security (CapDrop ALL, no-new-privileges), resource limits, ExtraHosts
// (host.docker.internal + the resolved MCP host), and the caller-supplied
// mount set. Card-mode and chat-mode both call this so future changes to
// the security/resource posture land in one place.
//
// PidsLimit is copied to a local before taking its address so concurrent
// spawns each get an independent pointer. Sharing &m.cfg.ContainerPidsLimit
// across containers would expose a field-address bug: any code path that
// mutated m.cfg.ContainerPidsLimit would retroactively change the limit
// observed by every running container's HostConfig snapshot, and the
// Docker SDK can keep a reference to that pointer for the lifetime of the
// create request.
func (m *Manager) baseHostConfig(ctx context.Context, mcpURL string, mounts []mount.Mount) *container.HostConfig {
	pidsLimit := m.cfg.ContainerPidsLimit

	return &container.HostConfig{
		Mounts:      mounts,
		ExtraHosts:  m.buildExtraHosts(ctx, mcpURL),
		CapDrop:     strslice.StrSlice{"ALL"},
		SecurityOpt: []string{"no-new-privileges"},
		Resources: container.Resources{
			Memory:    m.cfg.ContainerMemoryLimit,
			PidsLimit: &pidsLimit,
		},
	}
}

// BuildChatAuthEnv mints a GitHub installation token for the runner's own
// taskSkillsMount git pull. Claude auth and CM_GIT_TOKEN for the container
// are owned by the tokenRefresher and reach the worker via the shared secrets
// dir — this token must not be forwarded to the container.
//
// When cfg.TaskSkillsDir is empty the token has no consumer (the skills mount
// is the only caller that uses it), so we skip the GitHub round-trip entirely.
// Card-mode startContainer skips the mint in the same case; this guard keeps
// the two modes symmetric so a deployment without a skills dir pays zero
// GitHub API calls on /chat/start.
func (m *Manager) BuildChatAuthEnv(ctx context.Context) string {
	if m.cfg.TaskSkillsDir == "" {
		return ""
	}

	if m.token == nil {
		return ""
	}

	tok, _, err := m.token.GenerateToken(ctx)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("BuildChatAuthEnv: github token generation failed", "error", err)
		}

		return ""
	}

	return tok
}
