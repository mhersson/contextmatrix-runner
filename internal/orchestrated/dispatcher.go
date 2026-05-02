// Package orchestrated bootstraps the orchestrated worker dependencies and
// dispatches one driver.Driver per /trigger payload. Every webhook /trigger
// is routed through this Dispatcher.
//
// Known follow-ups:
//   - The sessions.CardStore is in-memory: session IDs are lost across
//     runner restarts and the FSM falls back to tier-3 (fresh primer)
//     after a restart. A future iteration backs the store with MCP
//     update_card agent_sessions.
//   - GitTokens remains the runner-level provider via the existing
//     GitTokensAdapter; per-card token threading into the workspace
//     is unchanged.
package orchestrated

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	githubauth "github.com/mhersson/contextmatrix-githubauth"

	"github.com/mhersson/contextmatrix-runner/internal/callback"
	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
	"github.com/mhersson/contextmatrix-runner/internal/config"
	cmcontainer "github.com/mhersson/contextmatrix-runner/internal/container"
	"github.com/mhersson/contextmatrix-runner/internal/driver"
	"github.com/mhersson/contextmatrix-runner/internal/events"
	"github.com/mhersson/contextmatrix-runner/internal/logbroadcast"
	"github.com/mhersson/contextmatrix-runner/internal/mcpclient"
	"github.com/mhersson/contextmatrix-runner/internal/orchestrator"
	"github.com/mhersson/contextmatrix-runner/internal/spawn"
	spawndocker "github.com/mhersson/contextmatrix-runner/internal/spawn/docker"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
	"github.com/mhersson/contextmatrix-runner/internal/webhook"
	"github.com/mhersson/contextmatrix-runner/internal/workspace"
)

// Deps are the constructor inputs.
type Deps struct {
	Cfg       *config.Config
	Logger    *slog.Logger
	GitTokens githubauth.TokenGenerator
	// Callback drives the CM-side runner status (queued → running →
	// completed/failed). Optional: a nil client means the runner card
	// stays in its default state, but the orchestrator otherwise works.
	Callback *callback.Client
	// Broadcaster receives CC stream events as LogEntry values so CM's
	// SSE /logs subscribers can show live agent output in the console.
	// Optional: nil disables log streaming.
	Broadcaster *logbroadcast.Broadcaster
	// Tracker, if set, is updated with the spawned worker's container ID
	// so shutdown force-cleanup and operator endpoints can reach
	// orchestrated containers. Optional in tests; production wiring
	// always passes the same tracker the webhook handler uses.
	Tracker *tracker.Tracker
}

// Dispatcher implements webhook.OrchestratedDispatcher and starts a
// per-card driver.Driver for every /trigger payload it receives.
type Dispatcher struct {
	cfg         *config.Config
	logger      *slog.Logger
	spawner     spawn.Spawner
	gitTokens   githubauth.TokenGenerator
	callback    *callback.Client
	broadcaster *logbroadcast.Broadcaster
	tracker     *tracker.Tracker
	// dnsCache memoises buildExtraHosts() resolver calls so a spawn burst
	// against the same MCP hostname doesn't pay N DNS RTTs. Lifetime is
	// process-long; TTL is dnsCacheTTL.
	dnsCache *dnsCache
	// resolver is the DNS resolver used by buildExtraHosts. Swappable for
	// tests; nil is treated as net.DefaultResolver.
	resolver hostResolver
}

// dnsLookupTimeout bounds buildExtraHosts' resolver call. An attacker who
// points the card's MCP URL at a slow-responding authoritative server could
// otherwise stall the spawn path indefinitely; the deadline caps exposure
// at 2s and then falls back to running the container without the ExtraHosts
// entry.
const dnsLookupTimeout = 2 * time.Second

// New wires up the spawner and logger. Returns an error when essential
// config is missing so callers fail loudly during bootstrap rather than
// silently routing payloads into a half-initialised dispatcher.
func New(d Deps) (*Dispatcher, error) {
	if d.Cfg == nil {
		return nil, fmt.Errorf("orchestrated: cfg is required")
	}

	if d.Cfg.AgentImage == "" {
		return nil, fmt.Errorf("agent_image is required")
	}

	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}

	spawner, err := spawndocker.NewSpawner(logger)
	if err != nil {
		return nil, fmt.Errorf("docker spawner: %w", err)
	}

	return &Dispatcher{
		cfg:         d.Cfg,
		logger:      logger,
		spawner:     spawner,
		gitTokens:   d.GitTokens,
		callback:    d.Callback,
		broadcaster: d.Broadcaster,
		tracker:     d.Tracker,
		dnsCache:    newDNSCache(dnsCacheTTL, dnsCacheCapacity),
	}, nil
}

// Dispatch starts a per-card driver in a goroutine. The goroutine owns
// the cancellable ctx and is responsible for invoking cancel and
// onComplete on every exit path (success, error, panic). onComplete is
// the webhook handler's tracker.Remove hook — without it the tracker
// entry leaks and the same card cannot be re-triggered without runner
// restart.
func (dp *Dispatcher) Dispatch(ctx context.Context, p webhook.TriggerPayload, cancel context.CancelFunc, onComplete func()) error {
	// containerMCPURL is what spawned worker containers use to reach CM
	// (e.g. http://host.docker.internal:8080). Used as the MCP_URL env
	// passed into the worker.
	containerMCPURL := dp.cfg.ContainerContextMatrixURL
	if containerMCPURL == "" {
		containerMCPURL = dp.cfg.ContextMatrixURL
	}

	// runnerMCPURL is what the runner itself uses to reach CM. host.docker.internal
	// only resolves from inside Docker containers — the runner runs on the host,
	// so it must use ContextMatrixURL.
	runnerMCPURL := dp.cfg.ContextMatrixURL

	// Construct per-card MCP client. The bearer is the per-card MCP API
	// key delivered with the trigger payload, NOT the runner's webhook
	// HMAC secret.
	mcp, err := mcpclient.New(ctx, runnerMCPURL+"/mcp", p.MCPAPIKey)
	if err != nil {
		return fmt.Errorf("mcp client: %w", err)
	}

	sseBase := dp.cfg.ContextMatrixURL + "/api/runner/events"
	sse := events.NewSSEClient(sseBase, p.CardID, p.MCPAPIKey)

	// agentID identifies the runner's claim on the card. Using a
	// runner-prefixed agent ID makes log entries on CM clearly
	// attributable to the orchestrated runner path.
	agentID := "runner:" + p.CardID

	// Resolve the project's repo registry up-front so the same set of
	// repos drives both the worker's git credential helper (CM_GIT_TOKEN
	// env, baked in at spawn time) and the workspace manager
	// (CloneRepo/CreateWorktree, used per-subtask in the execute phase).
	// If the project has no registry configured on CM but the trigger
	// payload carries a RepoURL, synthesize a one-entry registry — this
	// keeps legacy single-repo cards working without forcing every
	// project to migrate to a multi-repo .board.yaml.
	registry, card, regErr := fetchTaskContext(ctx, mcp, p.Project, p.CardID, agentID)
	if regErr != nil {
		dp.logger.Warn("task context fetch failed",
			"err", regErr,
			"project", p.Project,
			"card_id", p.CardID,
		)
	}

	mode := orchestrator.ModeAutonomous
	if card != nil && !card.Autonomous {
		mode = orchestrator.ModeHITL
	}

	dp.logger.Info("orchestrator mode selected",
		"project", p.Project,
		"card_id", p.CardID,
		"mode", modeLabel(mode),
		"autonomous_flag", card != nil && card.Autonomous,
	)

	if len(registry) == 0 && p.RepoURL != "" {
		slug := slugFromRepoURL(p.RepoURL)
		if slug == "" {
			slug = p.Project
		}

		registry = []workspace.RepoSpec{{Slug: slug, URL: p.RepoURL}}
		dp.logger.Info("project registry synthesized from trigger payload",
			"project", p.Project,
			"slug", slug,
			"url", p.RepoURL,
		)
	} else {
		dp.logger.Info("project registry fetched",
			"project", p.Project,
			"repos", len(registry),
		)
	}

	// Stage MCP_API_KEY in a tmpfs-backed file the entrypoint sources at
	// startup so it never appears in `docker inspect`. The entrypoint
	// uses it to write $HOME/.claude.json (Claude reads the bearer from
	// disk, not env, on every exec) and then unsets it.
	//
	// CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY are NOT staged here:
	// docker exec ignores PID 1's env, so anything sourced into the
	// entrypoint shell is invisible to subsequent execs. Those land
	// per-exec via driver.Config.ClaudeAuthEnv (claudeAuthEnv()).
	//
	// GitHub tokens are likewise per-exec — minted on demand and
	// injected by workspaceExec — so every git/gh subprocess inside the
	// worker sees a freshly-minted, non-expired token regardless of how
	// long the card runs.
	//
	// SecretsDir empty disables the file path (e.g. unit tests);
	// buildWorkerSpec then folds the remaining secrets back into Env.
	secretsFilePath := ""
	if dp.cfg.SecretsDir != "" {
		secretsFilePath = filepath.Join(dp.cfg.SecretsDir, workerContainerName(p)+".env")
	}

	workerSpec, secretEnv := dp.buildWorkerSpec(ctx, p, containerMCPURL, secretsFilePath)

	if secretsFilePath != "" {
		if err := writeSecretsFile(secretsFilePath, secretEnv); err != nil {
			return fmt.Errorf("stage worker secrets: %w", err)
		}
	}

	// Load the curated task-skill catalog from the host directory the
	// dispatcher already mounted into /host-skills. The orchestrator uses
	// the index to render a `Specialist skills mounted` block in each
	// phase's priming message; the per-card subset selector lives on
	// p.TaskSkills. Failures degrade silently — a missing index just
	// means priming omits the skills block.
	skillIndex, err := orchestrator.LoadSkillIndex(dp.cfg.TaskSkillsDir)
	if err != nil {
		dp.logger.Warn("load skill index failed; priming will omit specialist skills",
			"err", err,
			"dir", dp.cfg.TaskSkillsDir,
		)

		skillIndex = nil
	}

	drv := driver.New(driver.Config{
		Project: p.Project,
		CardID:  p.CardID,
		AgentID: agentID,
		Mode:    mode,

		SkillIndex: skillIndex,
		TaskSkills: p.TaskSkills,

		Spawner:    dp.spawner,
		WorkerSpec: workerSpec,

		// Claude/Workspace are constructed once the worker exists; see
		// BuildDepsFromWorker below.
		Claude:    nil,
		Workspace: nil,

		// Notifier publishes orchestrator-level status messages into the
		// runner's broadcast stream so the chat UI sees activity between
		// agent turns.
		Notifier: newBroadcasterNotifier(dp.broadcaster, p.Project, p.CardID),

		BuildDepsFromWorker: func(w spawn.Worker) (driver.DepsFromWorker, error) {
			// Record the spawned container ID on the tracker entry so the
			// shutdown force-cleanup pass and operator endpoints can reach
			// the orchestrated container. Tracker is optional in tests.
			if dp.tracker != nil {
				dp.tracker.UpdateContainerID(p.Project, p.CardID, w.ID())
			}

			// Wait for the entrypoint to finish staging credentials before
			// any docker-exec touches Claude. Without this the first phase
			// races against `cp -r /claude-auth/.` (which can be hundreds
			// of megabytes on an active developer machine), so claude
			// reads a half-populated $HOME/.claude.json and 401s with
			// "OAuth authentication is currently not supported".
			if waitErr := waitForEntrypointReady(ctx, w, dp.logger); waitErr != nil {
				dp.logger.Warn("worker entrypoint readiness wait failed",
					"err", waitErr,
					"worker", w.ID(),
				)
			}

			// Smoke test the worker: run `claude --version` so we know the
			// binary is on PATH and the exec bridge produces output. This
			// is what we should be seeing for every phase invocation;
			// anything different here is a bug in our exec pipeline rather
			// than something Claude did or didn't do.
			res, smokeErr := w.Exec(ctx, spawn.ExecOptions{
				Cmd:          []string{"claude", "--version"},
				AttachStdout: true,
				AttachStderr: true,
			})
			if smokeErr != nil {
				dp.logger.Warn("worker smoke test (claude --version) failed",
					"err", smokeErr,
					"worker", w.ID(),
				)
			} else {
				dp.logger.Info("worker smoke test (claude --version) ok",
					"worker", w.ID(),
					"exit_code", res.ExitCode,
					"stdout", res.Stdout,
					"stderr", res.Stderr,
				)
			}

			execAPI := newWorkerExecAPI(w, dp.logger)
			wrapper := claudeclient.NewWrapperWithExecAPI(execAPI, dp.logger)

			// Bridge CC stream events to the runner-side log broadcaster
			// so CM's /logs SSE clients see live agent output in the
			// console (matching the legacy path's container-stdout
			// stream). Each text/thinking/tool_use event becomes one
			// LogEntry on the broadcaster's channel.
			//
			// Skill engagements are intentionally NOT detected here. The
			// runner-side callback used to post /api/runner/skill-engaged
			// to the parent card with the parent's agent_id, which
			// misattributed every subtask engagement to the parent. The
			// agent's `add_log(action="skill_engaged", ...)` call (instructed
			// by the priming) is the single source of truth, and CM rolls
			// the subtask entry up to the parent with the subtask's actor.
			bcastProject := p.Project
			bcastCardID := p.CardID

			wrapper.SetEventCallback(func(ev claudeclient.StreamEvent) {
				if dp.broadcaster == nil {
					return
				}

				entry := streamEventToLogEntry(ev, bcastProject, bcastCardID)
				if entry.Type == "" {
					return
				}

				dp.broadcaster.Publish(entry)
			})

			// Workspace manager seeded with the registry resolved above.
			// The exec adapter mints a fresh GitHub token before every
			// docker-exec and injects it as CM_GIT_TOKEN / GH_TOKEN env so
			// the worker's credential helper and gh CLI both see a
			// non-expired token at the moment each subprocess runs.
			wsExec := newWorkspaceExec(w, mintToken(dp.gitTokens))
			ws := workspace.NewManager(wsExec, registry)

			return driver.DepsFromWorker{
				Claude:    wrapper,
				Workspace: ws,
			}, nil
		},

		MCP:           mcp,
		GitTokens:     dp.gitTokens,
		ClaudeAuthEnv: claudeAuthEnv(dp.cfg),

		SSE:               sse,
		Logger:            dp.logger,
		HeartbeatInterval: 5 * time.Minute,
	})

	go func() {
		// Defers run LIFO: onComplete fires last so the tracker entry
		// is dropped after the run ctx is cancelled and the MCP client
		// is closed. A panic-recover sits at the top so an unexpected
		// crash inside the driver still releases the tracker entry —
		// otherwise the operator must restart the runner to re-trigger
		// the card.
		defer func() {
			if onComplete != nil {
				onComplete()
			}
		}()
		defer cancel()
		defer func() { _ = mcp.Close() }()
		defer func() {
			// Remove the per-container secrets file from the host
			// regardless of how the run exited. The file is
			// tmpfs-backed in production so this is a hot-path delete,
			// but we still defer it explicitly so a runner restart that
			// observes a stale file in SecretsDir is a real bug rather
			// than a routine cleanup miss.
			if secretsFilePath != "" {
				if err := os.Remove(secretsFilePath); err != nil && !os.IsNotExist(err) {
					dp.logger.Warn("failed to remove worker secrets file",
						"path", secretsFilePath,
						"err", err,
					)
				}
			}
		}()
		defer func() {
			if r := recover(); r != nil {
				dp.logger.Error("orchestrated driver panic",
					"card_id", p.CardID,
					"project", p.Project,
					"panic", r,
				)
				dp.reportStatus(p, "failed", fmt.Sprintf("driver panic: %v", r))
			}
		}()

		// Tell CM the runner is now actively working on this card so
		// the runner pane flips from "queued" to "running". Best-effort:
		// if the callback path is unconfigured or fails the workflow
		// still proceeds.
		dp.reportStatus(p, "running", "orchestrated worker started")

		if err := drv.Drive(ctx); err != nil {
			dp.logger.Warn("orchestrated driver exited with error",
				"card_id", p.CardID,
				"project", p.Project,
				"err", err,
			)
			dp.reportStatus(p, "failed", err.Error())

			return
		}

		dp.logger.Info("orchestrated driver completed",
			"card_id", p.CardID,
			"project", p.Project,
		)
		dp.reportStatus(p, "completed", "orchestrated worker completed")
	}()

	return nil
}

// reportStatus is a small wrapper around callback.ReportStatus that
// no-ops cleanly when no callback client is configured. The 5-second
// budget caps how long a failing CM keeps the goroutine waiting on
// status reporting (the underlying client also retries on its own).
//
// Uses a fresh context.Background-derived ctx rather than the dispatch
// ctx because terminal status reports (completed/failed) often run
// after the run ctx has been cancelled — CM emits a stop event when
// the card hits a terminal state, the runner self-cancels, and
// piggybacking on that ctx makes ReportStatus fail to post the very
// status update the operator needs to see.
func (dp *Dispatcher) reportStatus(p webhook.TriggerPayload, status, message string) {
	if dp.callback == nil {
		return
	}

	cbCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := dp.callback.ReportStatus(cbCtx, p.CardID, p.Project, status, message); err != nil {
		dp.logger.Warn("orchestrated callback ReportStatus failed",
			"card_id", p.CardID,
			"project", p.Project,
			"status", status,
			"err", err,
		)
	}
}

// streamEventToLogEntry converts a Claude stream-json StreamEvent into
// a LogEntry the runner-side broadcaster can publish. Returns a zero
// entry (Type == "") for events that should not appear in the user-
// facing log stream — system_init, system_end, tool_result, etc.
func streamEventToLogEntry(ev claudeclient.StreamEvent, project, cardID string) logbroadcast.LogEntry {
	now := time.Now().UTC()

	switch ev.Kind {
	case claudeclient.EventText:
		return logbroadcast.LogEntry{Timestamp: now, CardID: cardID, Project: project, Type: "text", Content: ev.Text}
	case claudeclient.EventThinking:
		return logbroadcast.LogEntry{Timestamp: now, CardID: cardID, Project: project, Type: "thinking", Content: ev.Text}
	case claudeclient.EventToolUse:
		content := ev.ToolName
		if len(ev.ToolInput) > 0 {
			content = ev.ToolName + " " + string(ev.ToolInput)
		}

		return logbroadcast.LogEntry{Timestamp: now, CardID: cardID, Project: project, Type: "tool_call", Content: content}
	case claudeclient.EventError:
		return logbroadcast.LogEntry{Timestamp: now, CardID: cardID, Project: project, Type: "stderr", Content: ev.Text}
	}

	return logbroadcast.LogEntry{}
}

// secretsMountTarget is where the per-container secrets file lands inside
// the worker. The orchestrated entrypoint sources it on startup and removes
// the in-container view; the runner deletes the host file on container
// teardown. Matches the path the entrypoint already reads.
const secretsMountTarget = "/run/cm-secrets/env" //nolint:gosec // path, not a credential

// buildWorkerSpec assembles env + mounts for one orchestrated worker.
// Claude auth is wired by priority (mirroring the legacy path):
//
//	claude_auth_dir > claude_oauth_token > anthropic_api_key
//
// Only the auth_dir branch lands here (as a bind-mount). The OAuth
// token / API key paths are handled per-exec via
// driver.Config.ClaudeAuthEnv → orchestrator.Context.ClaudeAuthEnv —
// see claudeAuthEnv() below. They cannot live in the spawn-time
// secrets file because the worker's PID 1 is `sleep infinity` and
// `docker exec` does not inherit the entrypoint shell's env.
//
// secretsFilePath, when non-empty, redirects MCP_API_KEY into a
// separate map returned alongside the spec; the caller writes that map
// to disk with 0600 perms and bind-mounts it read-only at
// /run/cm-secrets/env. The MCP key is kept off Config.Env so
// `docker inspect` reveals only non-credential plumbing.
//
// When secretsFilePath is empty the secrets fall back into Env — kept for
// unit-test ergonomics where no real on-disk file is being staged.
//
// GitHub tokens are NOT spawn-time secrets: the runner mints a fresh
// token before each docker-exec via workspaceExec, so neither
// Config.Env nor the secrets file ever holds CM_GIT_TOKEN/GH_TOKEN.
func (dp *Dispatcher) buildWorkerSpec(ctx context.Context, p webhook.TriggerPayload, containerMCPURL, secretsFilePath string) (spawn.WorkerSpec, map[string]string) {
	env := map[string]string{
		"MCP_URL":    containerMCPURL + "/mcp",
		"CM_CARD_ID": p.CardID,
		"CM_PROJECT": p.Project,
	}

	secrets := map[string]string{
		"MCP_API_KEY": p.MCPAPIKey,
	}

	var mounts []spawn.Mount

	if dp.cfg.ClaudeAuthDir != "" {
		// Bind-mount the host's ~/.claude read-only at /claude-auth (legacy
		// path). The entrypoint copies its contents into $HOME/.claude/ so
		// the worker gets the host's credentials without any path inside
		// the worker being able to write back to the host. Mounting
		// read-only at a separate target also leaves $HOME/.claude/ in
		// tmpfs so writes done by the entrypoint (settings.json,
		// .claude.json) and by Claude itself (project history, etc.) never
		// persist to the host.
		mounts = append(mounts, spawn.Mount{
			Source:   dp.cfg.ClaudeAuthDir,
			Target:   "/claude-auth",
			ReadOnly: true,
		})

		// Also mount the host's ~/.claude.json (sibling file, not inside
		// the .claude/ dir). Claude Code stores subscription/account state
		// there — userID, oauthAccount, hasAvailableSubscription,
		// migration flags. Without it Claude considers a fresh worker
		// "first run", backs up our merged file, and rewrites it with a
		// minimal template. Either the rewrite drops mcpServers (worker
		// loses the contextmatrix MCP), or the OAuth flow ends up routing
		// through the API-key endpoint (401 "OAuth authentication is
		// currently not supported"). Skip the mount if the host file does
		// not exist; Docker would otherwise create an empty directory at
		// the source path.
		hostClaudeJSON := filepath.Join(filepath.Dir(dp.cfg.ClaudeAuthDir), ".claude.json")
		if _, err := os.Stat(hostClaudeJSON); err == nil {
			mounts = append(mounts, spawn.Mount{
				Source:   hostClaudeJSON,
				Target:   "/claude-auth.json",
				ReadOnly: true,
			})
		}
	}

	// Operator-supplied claude_settings: written to ~/.claude/settings.json
	// by the entrypoint after the optional /host-claude copy so the
	// operator's settings always win over a host-side settings.json.
	if dp.cfg.ClaudeSettings != "" {
		env["CM_CLAUDE_SETTINGS"] = dp.cfg.ClaudeSettings
	}

	// Task skills: when the runner is configured with a host skills dir,
	// bind-mount it read-only at /host-skills so the entrypoint can copy
	// the requested subset into the worker's ~/.claude/skills. A non-nil
	// payload.TaskSkills (even empty) is treated as "explicit selection";
	// nil means "mount the full set" — matching the legacy contract.
	if dp.cfg.TaskSkillsDir != "" {
		mounts = append(mounts, spawn.Mount{
			Source:   dp.cfg.TaskSkillsDir,
			Target:   "/host-skills",
			ReadOnly: true,
		})

		if p.TaskSkills != nil {
			env["CM_TASK_SKILLS_SET"] = "1"
			env["CM_TASK_SKILLS"] = strings.Join(*p.TaskSkills, ",")
		}
	}

	// Apply operator-supplied passthrough env. Reserved keys set by the
	// dispatcher above are skipped so a misconfigured worker_extra_env
	// can't accidentally clobber MCP wiring or auth tokens.
	for k, v := range dp.cfg.WorkerExtraEnv {
		if _, reserved := env[k]; reserved {
			continue
		}

		if _, reserved := secrets[k]; reserved {
			continue
		}

		env[k] = v
	}

	// When SecretsDir is configured, route the credential-bearing env
	// through a tmpfs-backed file the entrypoint sources at startup, and
	// keep the container's docker-inspect env limited to non-secret
	// plumbing. When SecretsDir is empty (unit tests), fold the secrets
	// back into Env so the worker still receives them.
	if secretsFilePath != "" {
		mounts = append(mounts, spawn.Mount{
			Source:   secretsFilePath,
			Target:   secretsMountTarget,
			ReadOnly: true,
		})
	} else {
		for k, v := range secrets {
			env[k] = v
		}

		secrets = nil
	}

	return spawn.WorkerSpec{
		Image:  dp.cfg.AgentImage,
		Name:   workerContainerName(p),
		Env:    env,
		Mounts: mounts,
		// Labels mark this container as runner-managed so the
		// label-aware sweeps (ListManaged / ForceRemoveByLabels /
		// CleanupOrphans) can find it. Without these the orphan-cleanup
		// pass silently no-ops and worker containers leak across runner
		// restarts.
		Labels: map[string]string{
			cmcontainer.LabelRunner:  "true",
			cmcontainer.LabelProject: p.Project,
			cmcontainer.LabelCardID:  p.CardID,
		},
		// host-gateway lets host.docker.internal resolve from inside the
		// container on Linux. We additionally resolve the MCP URL hostname
		// on the runner host (which sees the host's /etc/hosts) and inject
		// the result so a LAN-only / split-horizon name reaches CM from
		// inside the worker even when the container's resolver can't see
		// it.
		ExtraHosts: dp.buildExtraHosts(ctx, containerMCPURL),
		Resources: spawn.ResourceLimits{
			MemoryBytes: dp.cfg.ContainerMemoryLimit,
			PIDs:        dp.cfg.ContainerPidsLimit,
		},
		// Sandbox the worker so a compromised Claude exec runs without
		// ambient Linux capabilities, cannot escalate via setuid, and
		// cannot mutate the image rootfs. The Tmpfs entries supply the
		// writable scratch space the entrypoint and Claude need on top
		// of that read-only root: /tmp for general scratch,
		// /home/user covers everything the entrypoint writes
		// (~/.gitconfig, ~/.claude.json, ~/.claude/skills/,
		// ~/.cm-git-cred/, plus npm/cache dirs Claude touches),
		// /workspace for clones and worktrees.
		Security: spawn.SecuritySpec{
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			ReadonlyRootfs: true,
		},
		// /tmp gets the standard 1777 sticky-bit world-writable mode
		// (no UID needed). /home/user and /workspace are handed to UID
		// 1000 (= the worker image's user) so the entrypoint can write
		// ~/.gitconfig, ~/.claude.json, ~/.cm-git-cred/, ~/.claude/skills/
		// and clone repos under /workspace. Without uid=1000, the
		// kernel-default root:root tmpfs is unwritable to the non-root
		// container user and the entrypoint dies on the first mkdir.
		//
		// Exec is enabled on all three because the worker builds and
		// runs test binaries (`go test` produces a binary in tmpdir and
		// then execs it; npm-installed CLIs land under ~/.local/bin and
		// must run from there). Docker's tmpfs default is noexec, so
		// the option must be set explicitly.
		Tmpfs: []spawn.TmpfsMount{
			{Target: "/tmp", Mode: "1777", Exec: true},
			{Target: "/home/user", Mode: "0700", UID: workerUID, GID: workerGID, Exec: true},
			{Target: "/workspace", Mode: "0755", UID: workerUID, GID: workerGID, Exec: true},
		},
	}, secrets
}

// workerUID / workerGID match the `user` account created in the agent
// image's Dockerfile (`useradd -m -u 1000`). The dispatcher hands
// tmpfs mounts to this UID so the worker process can write into them.
const (
	workerUID = 1000
	workerGID = 1000
)

// buildExtraHosts returns the /etc/hosts entries Docker should add to the
// worker container. host.docker.internal is always included so the
// host-gateway alias resolves on Linux. When containerMCPURL points at a
// hostname (not an IP, localhost, or host.docker.internal), the runner
// resolves it on the host's resolver — which honours /etc/hosts via
// nsswitch — and pins the result into the container so a LAN-only or
// split-horizon name still reaches CM from inside the worker.
//
// Resolution failures (timeout, NXDOMAIN, parse error) degrade silently to
// the default entry; the spawn path must never block on DNS. Successful
// lookups are memoised in dnsCache for dnsCacheTTL so a spawn burst pays
// at most one resolver RTT per hostname.
func (dp *Dispatcher) buildExtraHosts(_ context.Context, mcpURL string) []string {
	hosts := []string{"host.docker.internal:host-gateway"}

	u, err := url.Parse(mcpURL)
	if err != nil || u.Hostname() == "" {
		return hosts
	}

	hostname := u.Hostname()
	if net.ParseIP(hostname) != nil || hostname == "localhost" || hostname == "host.docker.internal" {
		return hosts
	}

	if dp.dnsCache != nil {
		if addrs, ok := dp.dnsCache.get(hostname); ok && len(addrs) > 0 {
			hosts = append(hosts, hostname+":"+addrs[0])

			return hosts
		}
	}

	resolver := dp.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	// Use context.Background as the parent so a nearly-cancelled parent
	// ctx doesn't cut the deadline short — the 2s cap is already tight.
	lookupCtx, cancel := context.WithTimeout(context.Background(), dnsLookupTimeout)
	defer cancel()

	addrs, err := resolver.LookupHost(lookupCtx, hostname)
	if err != nil || len(addrs) == 0 {
		if errors.Is(err, context.DeadlineExceeded) || lookupCtx.Err() != nil {
			dp.logger.Warn("MCP hostname lookup timed out; container will run without ExtraHosts mapping",
				"hostname", hostname, "timeout", dnsLookupTimeout)
		} else {
			dp.logger.Warn("could not resolve MCP hostname for container",
				"hostname", hostname, "error", err)
		}

		return hosts
	}

	if dp.dnsCache != nil {
		dp.dnsCache.put(hostname, addrs)
	}

	hosts = append(hosts, hostname+":"+addrs[0])
	dp.logger.Info("added MCP host to container", "hostname", hostname, "ip", addrs[0])

	return hosts
}

// claudeAuthEnv returns the per-exec env that authenticates Claude
// inside the worker. The runner injects this on every Claude
// docker-exec via orchestrator.Context.ClaudeAuthEnv → spawnEnv. It
// cannot be a spawn-time env or a sourced secrets file because
// `docker exec` does not inherit the entrypoint shell's env: PID 1 is
// `sleep infinity`, and exec'd processes only see Container.Config.Env
// plus the per-exec env passed into ContainerExecCreate.
//
// Priority mirrors buildWorkerSpec's auth-dir mount precedence:
//
//	claude_auth_dir > claude_oauth_token > anthropic_api_key
//
// Returns nil when claude_auth_dir is configured (Claude reads
// credentials from the bind-mounted ~/.claude/.credentials.json
// instead) or when no auth source is configured at all.
func claudeAuthEnv(cfg *config.Config) map[string]string {
	switch {
	case cfg.ClaudeAuthDir != "":
		return nil
	case cfg.ClaudeOAuthToken != "":
		return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": cfg.ClaudeOAuthToken}
	case cfg.AnthropicAPIKey != "":
		return map[string]string{"ANTHROPIC_API_KEY": cfg.AnthropicAPIKey}
	}

	return nil
}

// writeSecretsFile writes the credential map to path with 0600 perms,
// one KEY=VALUE per line, suitable for being sourced by /bin/sh. The
// caller bind-mounts the file at /run/cm-secrets/env. The file lives in
// SecretsDir which is expected to be tmpfs in production so the host
// page cache never persists the contents.
func writeSecretsFile(path string, env map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}

	sort.Strings(keys) // stable order so a re-run produces the same file content.

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%q\n", k, env[k])
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write secrets file: %w", err)
	}

	return nil
}

// workerContainerName builds a stable, predictable name for the worker
// container. Docker container names must be alphanumeric + dashes and
// underscores; we sanitise the inputs by lowercasing and replacing any
// non-conforming character with '-'.
func workerContainerName(p webhook.TriggerPayload) string {
	return "cm-agent-" + sanitiseName(p.Project) + "-" + sanitiseName(p.CardID)
}

func sanitiseName(s string) string {
	s = strings.ToLower(s)

	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-' || c == '_':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}

	return string(out)
}

// slugFromRepoURL extracts a slug from a git URL by stripping the path
// down to its last segment and removing a trailing ".git". Returns "" when
// no useful slug can be derived (caller should fall back to the project
// name). Examples:
//
//	https://github.com/acme/auth-svc.git → "auth-svc"
//	git@github.com:acme/auth-svc.git     → "auth-svc"
//	https://gitea.example/owner/repo     → "repo"
func slugFromRepoURL(url string) string {
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimSuffix(url, "/")

	if i := strings.LastIndexAny(url, "/:"); i >= 0 {
		return sanitiseName(url[i+1:])
	}

	return sanitiseName(url)
}

// modeLabel returns a human-readable label for an orchestrator mode for
// log output. Adding it as a helper keeps the dispatch site readable.
func modeLabel(m orchestrator.Mode) string {
	if m == orchestrator.ModeHITL {
		return "hitl"
	}

	return "autonomous"
}

// fetchTaskContext pulls the project's repo registry and the card itself
// from CM via MCP in a single call. The registry seeds workspace.Manager;
// the card.Autonomous flag drives the orchestrator's HITL/Autonomous mode
// selection. Nil values are returned on error so the dispatch falls back
// to autonomous mode and a synthesized single-repo registry.
func fetchTaskContext(ctx context.Context, mcp *mcpclient.Client, project, cardID, agentID string) ([]workspace.RepoSpec, *orchestrator.Card, error) {
	cc, err := mcp.GetTaskContext(ctx, project, cardID, agentID)
	if err != nil {
		return nil, nil, fmt.Errorf("get_task_context: %w", err)
	}

	if len(cc.ProjectRepos) == 0 {
		return nil, cc.Card, nil
	}

	out := make([]workspace.RepoSpec, 0, len(cc.ProjectRepos))
	for _, r := range cc.ProjectRepos {
		out = append(out, workspace.RepoSpec{Slug: r.Slug, URL: r.URL})
	}

	return out, cc.Card, nil
}

// Compile-time assertion that *Dispatcher satisfies
// webhook.OrchestratedDispatcher.
var _ webhook.OrchestratedDispatcher = (*Dispatcher)(nil)

// broadcasterNotifier adapts a *logbroadcast.Broadcaster to the
// orchestrator.Notifier interface. The orchestrator publishes
// phase-start announcements and other status messages through this
// path so the chat UI shows activity between agent turns — without it,
// users stare at "No messages yet" while the first agent invocation
// thinks for ~90 seconds before emitting any text.
type broadcasterNotifier struct {
	b       *logbroadcast.Broadcaster
	project string
	cardID  string
}

func newBroadcasterNotifier(b *logbroadcast.Broadcaster, project, cardID string) orchestrator.Notifier {
	if b == nil {
		return nil
	}

	return &broadcasterNotifier{b: b, project: project, cardID: cardID}
}

// mintToken returns a closure that yields a fresh GitHub token, dropping
// the expiry value the workspace exec layer doesn't need. A nil generator
// produces a closure that returns ("", nil) so callers can decide whether
// to require auth (private repos) or skip it (public).
func mintToken(g githubauth.TokenGenerator) func(context.Context) (string, error) {
	if g == nil {
		return func(context.Context) (string, error) { return "", nil }
	}

	return func(ctx context.Context) (string, error) {
		tok, _, err := g.GenerateToken(ctx)

		return tok, err
	}
}

// Notify publishes a single LogEntry of the given type. Empty kind or
// message is dropped. Safe to call from any goroutine.
func (n *broadcasterNotifier) Notify(kind, message string) {
	if n == nil || n.b == nil || kind == "" || message == "" {
		return
	}

	n.b.Publish(logbroadcast.LogEntry{
		Timestamp: time.Now().UTC(),
		CardID:    n.cardID,
		Project:   n.project,
		Type:      kind,
		Content:   message,
	})
}

// entrypointReadyMarker is the path the orchestrated entrypoint touches
// once it has staged every file Claude needs at first-exec time
// (.claude.json, .credentials.json, settings.json). The bulk copy of
// ~/.claude/ runs after this marker, so the marker may exist before the
// container is fully populated — that is fine, only the auth/MCP files
// are critical for the first phase exec.
const entrypointReadyMarker = "/tmp/.cm-entrypoint-ready"

// entrypointReadyTimeout caps how long we wait for the marker. The
// fast operations the marker covers complete in tens of milliseconds
// even on slow disks, so a generous 30s ceiling only ever fires when
// the entrypoint itself crashed before reaching the marker.
const entrypointReadyTimeout = 30 * time.Second

// waitForEntrypointReady polls the worker for the readiness marker the
// entrypoint touches once auth/MCP files are staged. Without this the
// runner's first phase exec races against the entrypoint's slow bulk
// copy and Claude reads a half-populated $HOME/.claude.json.
//
// The poll uses `test -f` via docker-exec rather than ContainerExecCreate
// pre-checks because it costs nothing on a hit and handles the race
// where the worker is "running" per Docker but PID 1 hasn't begun
// executing the script yet.
func waitForEntrypointReady(ctx context.Context, w spawn.Worker, logger *slog.Logger) error {
	deadline := time.Now().Add(entrypointReadyTimeout)

	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	const pollInterval = 50 * time.Millisecond

	for {
		res, err := w.Exec(pollCtx, spawn.ExecOptions{
			Cmd:          []string{"test", "-f", entrypointReadyMarker},
			AttachStdout: true,
			AttachStderr: true,
		})
		if err == nil && res.ExitCode == 0 {
			return nil
		}

		if pollCtx.Err() != nil {
			return fmt.Errorf("entrypoint readiness wait timed out after %s", entrypointReadyTimeout)
		}

		select {
		case <-pollCtx.Done():
			return fmt.Errorf("entrypoint readiness wait timed out after %s", entrypointReadyTimeout)
		case <-time.After(pollInterval):
		}

		_ = logger // suppressed-poll messages would be too noisy; keep the parameter for future use
	}
}
