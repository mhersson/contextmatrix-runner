# contextmatrix-runner

A self-hosted runner that receives webhooks from
[ContextMatrix](https://github.com/mhersson/contextmatrix) and spawns disposable
Docker containers to execute autonomous AI tasks using Claude Code.

## Architecture

```text
                               HMAC-signed webhooks
                  ┌──────────────────────────────────────────┐
                  │                                          ▼
┌──────────────┐  │  ┌───────────────────┐    ┌──────────────────────┐
│  Web UI      │──┘  │  contextmatrix    │    │ contextmatrix-runner │
│  (Run Now)   │─────│  (REST API)       │───►│                      │
└──────────────┘     │                   │    │  Docker containers   │
                     │  POST /mcp        │◄───│  (Claude Code)       │
                     │  (MCP tools)      │    │                      │
                     └───────────────────┘    └──────────────────────┘
                            ▲                         │
                            │    MCP (Bearer auth)    │
                            └─────────────────────────┘
```

**ContextMatrix** stores cards, manages state, and sends webhooks. It never
touches code repositories.

**contextmatrix-runner** receives trigger/kill/stop-all webhooks, spawns
disposable Docker containers per task. Each container runs Claude Code in
headless mode, which connects back to ContextMatrix via MCP tools to claim
cards, execute work, and report completion.

## Prerequisites

- Go 1.26+
- Docker (daemon running)
- A running ContextMatrix instance
- A GitHub account with either a GitHub App or a fine-grained personal access token

## Quick Start

```bash
# Build the runner
make build

# Build the worker Docker image
make docker-worker

# Copy the example config
cp config.yaml.example config.yaml

# Edit config.yaml with your settings (see Configuration below)

# Run the runner
./contextmatrix-runner -config config.yaml
```

## Service Management

`svc.sh` manages contextmatrix-runner as a systemd user service. No root access
is required — it uses `systemctl --user`.

**Prerequisite:** Build the binary first (`make build`).

```bash
./svc.sh install    # Generate service file, reload daemon, enable on login
./svc.sh start      # Start the service
./svc.sh stop       # Stop the service
./svc.sh status     # Show service status
./svc.sh uninstall  # Stop, disable, and remove the service file
```

`install` writes the unit file to
`~/.config/systemd/user/contextmatrix-runner.service`. It sets `ExecStart` and
`WorkingDirectory` to the directory containing `svc.sh`, so the script resolves
paths correctly regardless of where it is invoked from. The config file defaults
to `config.yaml` in the same directory.

The unit file is configured for graceful shutdown on stop: `KillMode=mixed` and
`TimeoutStopSec=60` give running containers time to complete before being
force-killed. The service restarts automatically on failure (`Restart=always`,
`RestartSec=10`) and declares `After=docker.service`.

To validate the script with shellcheck:

```bash
make lint-sh
```

## GitHub Setup

The runner requires credentials to generate a git token for each container. Two
methods are supported — configure exactly one.

| Method | When to use |
| ------ | ----------- |
| **GitHub App** | Recommended for most setups. Short-lived, repo-scoped tokens; private key never leaves the host. |
| **Personal Access Token (PAT)** | Use in GitHub Enterprise environments where App creation is restricted, or for quick testing. |

**No inbound connections required.** The runner only makes outbound HTTPS calls
to the GitHub API (`api.github.com` or your enterprise endpoint). It works on a
local LAN with no public domain.

## GitHub App Setup

The runner uses a GitHub App to generate short-lived installation tokens for git
operations inside containers. This is the most secure approach: the App's
private key stays on the runner host, and only ephemeral tokens (valid for 1
hour) enter containers.

### Step 1: Create the GitHub App

1. Go to **GitHub Settings** → **Developer settings** → **GitHub Apps** → **New
   GitHub App**
2. Fill in:
   - **GitHub App name**: e.g., `contextmatrix-runner` (must be globally unique)
   - **Homepage URL**: anything (e.g.,
     `https://github.com/mhersson/contextmatrix-runner`)
   - **Webhook**: uncheck **Active** (we don't receive webhooks from GitHub)
3. Under **Repository permissions**, set:
   - **Contents**: Read & Write (for clone and push)
   - **Pull requests**: Read & Write (for creating PRs)
4. Under **Where can this GitHub App be installed?**: select **Only on this
   account**
5. Click **Create GitHub App**
6. Note the **App ID** shown on the settings page

### Step 2: Generate a Private Key

1. On the App's settings page, scroll to **Private keys**
2. Click **Generate a private key**
3. A `.pem` file will be downloaded — save it securely (e.g.,
   `~/.config/contextmatrix-runner/app.pem`)
4. Set permissions: `chmod 600 ~/.config/contextmatrix-runner/app.pem`

### Step 3: Install the App

1. Go to **GitHub Settings** → **Applications** (or the App's settings →
   **Install App**)
2. Click **Install** next to your account/organization
3. Choose **Only select repositories** and pick the repos you want the runner to
   access
4. Click **Install**
5. Note the **Installation ID** from the URL:
   `https://github.com/settings/installations/{INSTALLATION_ID}`

You can add or remove repositories from the installation at any time through the
GitHub UI — no runner configuration changes needed.

### Step 4: Configure the Runner

Add the App credentials to your `config.yaml`:

```yaml
github:
  auth_mode: "app"
  app:
    app_id: 123456
    installation_id: 78901234
    private_key_path: "/home/you/.config/contextmatrix-runner/app.pem"
  # For GitHub Enterprise Cloud with Data Residency (GHEC-DR) or GHES:
  # host: "acme.ghe.com"           # Env: CMR_GITHUB_HOST
  # api_base_url: "https://api.acme.ghe.com"  # Env: CMR_GITHUB_API_BASE_URL
```

For GitHub Enterprise, `github.host` and optionally `github.api_base_url` must
point to the enterprise instance. Leave both empty for standard `github.com`.
The git host inside containers is derived automatically from the repo URL, so no
extra git configuration is required. Set the matching `github.host` (or
`github.api_base_url`) in ContextMatrix so both sides target the same enterprise
instance.

For end-to-end setup of a GitHub App or PAT covering both the runner
and the contextmatrix server, see
[contextmatrix's topology guide](https://github.com/mhersson/contextmatrix/blob/main/docs/github-auth-recommended-topologies.md).

## GitHub PAT Setup

Use a fine-grained personal access token when you cannot create a GitHub App
(common in GitHub Enterprise environments with restricted App creation).

**Note:** PAT is mutually exclusive with `github.app.*` — configure exactly one
via `github.auth_mode`.

### Step 1: Create a Fine-Grained PAT

1. Go to **GitHub Settings** → **Developer settings** → **Personal access
   tokens** → **Fine-grained tokens** → **Generate new token**
2. Set a descriptive name (e.g., `contextmatrix-runner`) and an expiration
3. Under **Repository access**, select the repositories the runner should access
4. Under **Repository permissions**, set:
   - **Contents**: Read & Write (for clone and push)
   - **Pull requests**: Read & Write (for creating PRs)
5. Click **Generate token** and copy it immediately

### Step 2: Configure the Runner

Add the token to your `config.yaml`:

```yaml
github:
  auth_mode: "pat"
  pat:
    token: "github_pat_..."  # Env: CMR_GITHUB_PAT_TOKEN
```

Or set it via environment variables:

```bash
export CMR_GITHUB_AUTH_MODE="pat"
export CMR_GITHUB_PAT_TOKEN="github_pat_..."
```

Leave the `github.app.*` fields at their defaults (zero/empty) when using a PAT.

## Configuration

All fields can be overridden with environment variables using the `CMR_` prefix.
See `config.yaml.example` for the fully-commented template.

```yaml
# HTTP port for receiving webhooks from ContextMatrix.
# Env: CMR_PORT
port: 9090

# Base URL of the ContextMatrix instance (for status callbacks).
# Env: CMR_CONTEXTMATRIX_URL
contextmatrix_url: "http://localhost:8080"

# URL that spawned containers use to reach ContextMatrix. Injected as CM_MCP_URL.
# Defaults to contextmatrix_url when unset. Override when containers need a
# different address (e.g. host.docker.internal).
# Env: CMR_CONTAINER_CONTEXTMATRIX_URL
# container_contextmatrix_url: "http://host.docker.internal:8080"

# Shared HMAC-SHA256 secret. Must match runner.api_key in ContextMatrix config.
# At least 32 characters.
# Env: CMR_API_KEY
api_key: "your-shared-secret-here-at-least-32-chars"

# Default Docker image for worker containers. MUST be @sha256:... pinned
# (enforced by Validate; mutable tags are rejected). See CTXRUN-044.
# Env: CMR_BASE_IMAGE
base_image: "contextmatrix/worker@sha256:<digest>"

# Allowlist of permitted Docker images. When set, only listed images may be
# used (including runner_image overrides from trigger payloads). When empty,
# only base_image is permitted. Every entry MUST also be @sha256:... pinned.
allowed_images: []

# When to pull the image: always, never, if-not-present.
# Use "never" or "if-not-present" for locally-built images.
# Env: CMR_IMAGE_PULL_POLICY
image_pull_policy: "never"

# Maximum simultaneous containers.
# Env: CMR_MAX_CONCURRENT
max_concurrent: 3

# Force-kill containers after this duration.
# Env: CMR_CONTAINER_TIMEOUT
container_timeout: "2h"

# Memory limit per container in bytes. Default: 8 GiB.
# Env: CMR_CONTAINER_MEMORY_LIMIT
container_memory_limit: 8589934592

# Maximum number of PIDs per container. Default: 512.
# Env: CMR_CONTAINER_PIDS_LIMIT
container_pids_limit: 512

# Claude Code authentication — at least one of the three options below is required.
# Priority order (highest to lowest): claude_auth_dir > claude_oauth_token > anthropic_api_key.
# Only the highest-priority configured method is passed to each container.

# Path to host's ~/.claude/ directory. OAuth tokens are mounted read-only into containers.
# Requires running `claude login` on the host first.
# Env: CMR_CLAUDE_AUTH_DIR
claude_auth_dir: "/home/you/.claude"

# Long-lived OAuth token generated by running `claude setup-token` on the host.
# Injected as CLAUDE_CODE_OAUTH_TOKEN env var in containers. Valid for ~1 year.
# Env: CMR_CLAUDE_OAUTH_TOKEN
claude_oauth_token: ""

# Anthropic API key. Injected as ANTHROPIC_API_KEY env var in containers.
# Note: API key usage incurs additional cost on top of your subscription.
# Env: CMR_ANTHROPIC_API_KEY
anthropic_api_key: ""

# Raw JSON written to /home/user/.claude/settings.json inside each container.
# Use this to configure Claude Code behaviour. Must be valid JSON if set.
# If invalid, the runner exits on startup.
# Env: CMR_CLAUDE_SETTINGS
# claude_settings: '{"includeCoAuthoredBy":false}'

# GitHub authentication. Configure auth_mode and the matching sub-block.
github:
  auth_mode: "app" # CMR_GITHUB_AUTH_MODE — "app" (recommended) or "pat"
  host: ""         # CMR_GITHUB_HOST — GHE/GHEC-DR hostname; empty for github.com
  api_base_url: "" # CMR_GITHUB_API_BASE_URL — override for non-standard enterprise URLs

  # GitHub App credentials (required when auth_mode is "app").
  app:
    app_id: 0           # CMR_GITHUB_APP_ID
    installation_id: 0  # CMR_GITHUB_INSTALLATION_ID
    private_key_path: "" # CMR_GITHUB_PRIVATE_KEY_PATH

  # Fine-grained PAT (required when auth_mode is "pat").
  pat:
    token: "" # CMR_GITHUB_PAT_TOKEN

# Log level: debug, info, warn, error.
# Env: CMR_LOG_LEVEL
log_level: "info"
```

## Task skills

The runner can mount a curated set of Claude Code skills (Go, TypeScript/React, etc.) into worker containers. ContextMatrix server resolves which skills apply per card and sends the list in the `/trigger` payload; the runner copies the resolved subset into `~/.claude/skills/` for Claude Code to discover.

### One-time setup

```bash
# On the runner host (e.g. bumblebee):
sudo mkdir -p /var/lib/contextmatrix/task-skills
sudo chown <runner-user>:<runner-user> /var/lib/contextmatrix/task-skills
git clone <upstream-skills-repo> /var/lib/contextmatrix/task-skills
```

### Config

```yaml
# config.yaml
task_skills_dir: /var/lib/contextmatrix/task-skills
```

### Sync model

The runner runs `git pull --ff-only` on `task_skills_dir` before each `/trigger`. No cron. Pull failures log and continue with the existing local clone — the trigger never aborts because of a sync issue.

### Local-dev / single-host setup

When the runner and the CM server share a host, both `task_skills_dir` configs can point at the same path. No upstream remote needed; edit in place.

## Deployment profiles

The runner operates in one of two modes, set via `deployment_profile` in
`config.yaml` (or `CMR_DEPLOYMENT_PROFILE` env var). The default is
`production`.

| Profile | Behaviour |
| ------- | --------- |
| `production` | All validators enforced at full strength. No change from pre-feature behaviour. |
| `dev` | Five validators relaxed for local single-box setups. Every relaxation logs a startup or per-request line — nothing is silent. |

`deployment_profile: production` (or unset) is byte-identical to the
pre-feature behaviour.

### Dev-mode relaxations

1. **Unpinned image tags accepted** — `base_image`/`allowed_images` may use
   mutable tags (e.g. `contextmatrix/worker:latest`). Each unpinned reference
   logs on startup:
   `level=WARN msg="dev profile: accepting unpinned image reference" field=base_image image=...`

2. **Wider HMAC replay window** — `webhook_replay_skew_seconds` defaults to
   86400 (24 h) instead of 330 s, so a laptop that slept still processes
   webhooks. `image_pull_policy` defaults to `if-not-present` instead of
   `never`. Applied defaults are listed at startup:
   `level=INFO msg="dev profile: applied defaults" defaults=[webhook_replay_skew_seconds=86400 image_pull_policy=if-not-present]`

3. **Configurable HMAC skew** — the skew from `webhook_replay_skew_seconds` is
   now threaded into HMAC verification (previously hardcoded to 5 min), so the
   wider dev-mode window actually takes effect.

4. **`secrets_dir` env-var fallback** — if the runner cannot write to
   `secrets_dir` (e.g. a `systemctl --user` service on a read-only `/var/run`),
   it falls back to delivering secrets as container env vars instead of a bind
   mount, with a WARN:
   `level=WARN msg="dev profile: secrets_dir not writable, falling back to env-var delivery" dir=... error=...`
   Production mode still fails closed on this error.

### Minimal dev-mode config

A first-time user needs only these fields:

```yaml
deployment_profile: dev

contextmatrix_url: "https://your-cm-instance"
api_key: "your-shared-secret-at-least-32-chars"
base_image: "contextmatrix/worker:latest"

anthropic_api_key: "sk-ant-..."   # or claude_auth_dir / claude_oauth_token

github:
  auth_mode: "app"
  app:
    app_id: 123456
    installation_id: 78901234
    private_key_path: "/path/to/app.pem"
```

## ContextMatrix Configuration

On the ContextMatrix side, configure the runner connection in `config.yaml`:

```yaml
# MCP endpoint authentication — bearer token for the /mcp endpoint.
# When set, the runner passes this to containers so Claude Code can
# authenticate with ContextMatrix via MCP tools.
mcp_api_key: "your-mcp-bearer-token"

# Runner integration.
runner:
  enabled: true
  url: "http://localhost:9090" # Runner's address
  api_key: "same-shared-secret-as-runner" # Must match runner's api_key
```

Per-project overrides in `.board.yaml`:

```yaml
remote_execution:
  enabled: true # Enable for this project
  runner_image: "my-org/custom-worker:v2" # Optional custom image
```

## Container Lifecycle

1. User clicks **Run Now** on an autonomous card in the ContextMatrix web UI
2. ContextMatrix sends a signed `/trigger` webhook to the runner
3. Runner generates a git credential token (GitHub App installation token or PAT)
4. Runner pulls the Docker image and starts a hardened container with:
   - Debian bookworm-slim base with Go 1.26, Node.js 25, GitHub CLI, and golangci-lint
   - Claude Code CLI pre-installed
   - MCP config pointing to ContextMatrix
   - Git credential token for clone/push operations
   - Claude Code auth (OAuth dir mount, OAuth token, or API key —
     highest-priority configured method only)
   - All capabilities dropped, no-new-privileges, memory and PID limits
5. Claude Code runs the `run-autonomous` workflow:
   - Claims the card via MCP
   - Clones the repo, plans, executes, reviews, documents
   - Creates a feature branch and PR
   - Completes the card via MCP `complete_task`
6. Container exits, runner cleans up

**On kill**: Container is destroyed immediately. All uncommitted work is
discarded.

## Worker Image

The worker image (`docker/Dockerfile.worker`) is Debian-based and runs
everything as a non-root `user` account (UID 1000). No privilege escalation or
dropping occurs — the Dockerfile sets `USER user` before the entrypoint.

### What happens inside the container

`entrypoint.sh` runs as `user` and performs these steps:

1. **Auth setup** — applies the highest-priority configured auth method:
   - `claude_auth_dir`: copies OAuth tokens from the mounted `/claude-auth` into
     `~/.claude/`
   - `claude_oauth_token`: token is injected as `CLAUDE_CODE_OAUTH_TOKEN` env
     var at container creation time (no entrypoint logic needed)
   - `anthropic_api_key`: injected as `ANTHROPIC_API_KEY` env var at container
     creation time (no entrypoint logic needed)

   If `claude_settings` is configured, writes it to `~/.claude/settings.json`
   after the optional auth-dir copy, so it always takes precedence.

   Also writes `.claude.json` (MCP config), `.netrc` (GitHub token), and
   `.gitconfig`.

2. **Clone** — clones the project repository into `/home/user/workspace`. When
   `base_branch` is set in the trigger payload, the clone uses `-b <branch>` so
   work starts from the correct base. The Claude Code prompt is also extended
   with an instruction to target that branch when creating PRs (`gh pr create
   --base <branch>`).
3. **Execute** — `exec claude` runs Claude Code in headless mode, which connects
   to ContextMatrix via MCP tools to claim the card, execute the work, and
   report completion.

## Webhook Protocol

All webhooks are signed with HMAC-SHA256 using a shared secret.

| Direction   | Endpoint                  | Purpose                         |
| ----------- | ------------------------- | ------------------------------- |
| CM → Runner | `POST /trigger`           | Start a task                    |
| CM → Runner | `POST /kill`              | Stop a specific task            |
| CM → Runner | `POST /stop-all`          | Stop all tasks (or per-project) |
| CM → Runner | `POST /message`           | Send a user message to an interactive session |
| CM → Runner | `POST /promote`           | Promote interactive session to autonomous     |
| CM → Runner | `POST /end-session`       | Close container stdin so claude exits on EOF  |
| Runner → CM | `POST /api/runner/status` | Report container status         |

Signatures: `X-Signature-256: sha256={hex}`, `X-Webhook-Timestamp: {unix-ts}`.
HMAC computed over `timestamp.body`. Max 5-minute clock skew.

Status callback values: `running` (container started), `failed` (error or
non-zero exit), `completed` (clean exit).

### Trigger payload fields

| Field          | Type   | Required | Description                                                                              |
| -------------- | ------ | -------- | ---------------------------------------------------------------------------------------- |
| `card_id`      | string | yes      | Card identifier (e.g. `CTXRUN-019`)                                                     |
| `project`      | string | yes      | Project name                                                                             |
| `repo_url`     | string | yes      | Repository URL. HTTPS and SCP-style SSH (`git@github.com:org/repo`) are both supported. |
| `mcp_api_key`  | string | no       | Bearer token for MCP authentication                                                      |
| `base_branch`  | string | no       | Branch to clone and target for PRs. Defaults to the repo's default branch when omitted. |
| `runner_image`  | string | no       | Docker image override. Must be in `allowed_images` when that list is non-empty.          |
| `interactive`   | bool   | no       | When `true`, runs Claude in stream-json HITL mode and attaches to container stdin. Use `/message` and `/promote` to interact. |
| `model`         | string | no       | Model ID for the orchestrator (e.g. `claude-sonnet-4-6`). Passed through to the container environment.                        |

## API Endpoints

| Method | Path        | Auth | Description                                               |
| ------ | ----------- | ---- | --------------------------------------------------------- |
| POST   | `/trigger`  | HMAC | Start a container for a card                              |
| POST   | `/kill`     | HMAC | Kill a specific container (idempotent; 200 on already-stopped) |
| POST   | `/stop-all` | HMAC | Kill all containers (207 Multi-Status on partial failure)      |
| POST   | `/message`  | HMAC | Send a user message to an interactive (HITL) session      |
| POST   | `/promote`  | HMAC | Promote an interactive session to autonomous mode         |
| POST   | `/end-session` | HMAC | Close stdin of an interactive container; claude exits on EOF |
| GET    | `/logs`     | HMAC | SSE log stream for all active containers. Browser EventSource cannot send headers, so this endpoint must be proxied through a server that attaches the HMAC signature. |
| GET    | `/health`   | none | Health check                                              |
| GET    | `/readyz`   | none | Readiness probe (503 during preflight or drain)           |

### Response envelope

Every 2xx response is a `SuccessResponse`:

```json
{ "ok": true, "message": "...", "message_id": "..." }
```

Every non-2xx response (except `/logs` SSE and the `/readyz` probe shape) is an
`ErrorResponse`:

```json
{ "ok": false, "code": "invalid_field", "message": "invalid card_id" }
```

`code` is a stable enum — clients should branch on `code`, not on `message`
text. `/stop-all` returns a custom `StopAllResponse` with per-card `results`.

### Error codes

| Code                | Status | Endpoint(s)                                 | Meaning                                                        |
| ------------------- | ------ | ------------------------------------------- | -------------------------------------------------------------- |
| `invalid_json`      | 400    | all mutating endpoints                      | request body was not valid JSON                                |
| `invalid_field`     | 400    | all mutating endpoints                      | a field failed validation (`message` names the field)          |
| `unauthorized`      | 401    | all HMAC-guarded endpoints                  | HMAC auth failed (missing header, bad sig, expired, …)         |
| `not_found`         | 404    | `/message`, `/promote`, `/end-session`      | no container tracked for (project, card_id)                    |
| `conflict`          | 409    | `/trigger`, `/message`, `/promote`, `/end-session` | state conflict (already tracked, non-interactive, …)    |
| `duplicate`         | 409    | all HMAC-guarded endpoints                  | HMAC signature replay detected                                 |
| `stdin_closed`      | 410    | `/message`, `/promote`                      | session has ended (stdin was once attached, now closed)        |
| `too_large`         | 413    | `/message`                                  | `content` exceeds 8192 bytes                                   |
| `limit_reached`     | 429    | `/trigger`                                  | `max_concurrent` reached                                       |
| `internal`          | 500    | any                                         | server-side bug; details logged, never echoed                  |
| `upstream_failure`  | 502    | `/promote`                                  | CM verify-autonomous call failed                               |
| `draining`          | 503    | `/trigger`, `/message`, `/promote`, `/end-session` | graceful shutdown in progress                            |

Raw `err.Error()` strings, HMAC-failure reasons, upstream response bodies, and
unmarshal byte offsets are never echoed to clients — they are logged
server-side only.

## Security Model

Running AI agents in containers is a security boundary. The runner enforces
defense-in-depth so that a compromised or misbehaving agent cannot escalate
beyond its container.

### Container hardening

Every container is launched with the following restrictions:

- **All Linux capabilities dropped** (`CapDrop: ALL`). The container process
  runs with zero special privileges — it cannot modify network interfaces, mount
  filesystems, load kernel modules, or perform any other privileged operation,
  even as UID 0.
- **No new privileges** (`no-new-privileges` security option). Prevents
  privilege escalation via setuid/setgid binaries inside the container.
- **Memory limit** (default 8 GiB, configurable via `container_memory_limit`).
  Prevents a runaway process from exhausting host memory.
- **PID limit** (default 512, configurable via `container_pids_limit`). Prevents
  fork bombs from consuming all host PIDs.
- **Image allowlist** (`allowed_images`). When set, only explicitly listed
  images may be used. When empty, only the configured `base_image` is permitted.
  This prevents trigger payloads from requesting execution of arbitrary images.
- **Disposable containers**. Each task gets a fresh environment, destroyed after
  completion. No state persists between runs.

### Authentication and secrets

- **HMAC-SHA256 webhook signing** in both directions (shared secret, never
  transmitted)
- **GitHub App tokens**: short-lived (1 hour), repo-scoped, only ephemeral
  tokens enter containers. **PAT alternative**: the static token is injected
  as an env var per-container — use a token with minimal repository scope and
  set an expiration
- **Read-only mounts**: OAuth token directory mounted read-only when using
  `claude_auth_dir`; long-lived OAuth tokens and API keys injected as env vars
  when using the other auth methods
- **Human-only controls**: only humans can trigger Run/Stop from the CM web UI
- **No inbound connections**: runner only makes outbound calls to GitHub API and
  ContextMatrix

## Troubleshooting

### Container fails with "generate git token" error

**GitHub App (`auth_mode: "app"`):**

- Verify `github.app.private_key_path` points to a valid PEM file
- Verify `github.app.app_id` and `github.app.installation_id` are correct
- Check that the GitHub App is installed on the target repositories

**PAT (`auth_mode: "pat"`):**

- Verify `github.pat.token` (or `CMR_GITHUB_PAT_TOKEN`) is set and non-empty
- Check the token has not expired and has `Contents: Read & Write` and
  `Pull requests: Read & Write` permissions on the target repositories

### Container fails with git clone error

- Verify the repo URL in the ContextMatrix project config matches an installed
  repo
- Both HTTPS (`https://github.com/org/repo`) and SCP-style SSH
  (`git@github.com:org/repo`) URLs are supported — SCP-style URLs are
  automatically normalized to HTTPS before the container clones
- Check that the GitHub App has "Contents: Read & Write" permission
- If the token expired (>1 hour task), retry — the new container gets a fresh
  token

### "container limit reached" (HTTP 429)

- Increase `max_concurrent` in the runner config
- Or wait for running containers to finish

### Runner can't connect to ContextMatrix for callbacks

- Verify `contextmatrix_url` is reachable from the runner host
- Check that `api_key` matches `runner.api_key` in ContextMatrix config

### Containers can't reach ContextMatrix MCP endpoint

- The runner automatically adds a `host.docker.internal` mapping to all
  containers, so this hostname works on both Docker Desktop and Linux
- Verify `container_contextmatrix_url` in the runner config uses
  `host.docker.internal` or the host's LAN IP — not `localhost`
- If it still fails, check Docker networking and firewall rules

### Files in workspace owned by wrong user after container exits

The worker container runs as UID 1000. If the host user running the runner has a
different UID, files created inside bind-mounted volumes will be owned by UID
1000 on the host. This only matters for bind mounts — the default disposable
container filesystem is discarded on exit.

### Orphan containers after runner crash

The runner automatically cleans up orphan containers on startup (identified by
the `contextmatrix.runner=true` label). ContextMatrix will detect the heartbeat
timeout (default 30 minutes) and mark the card as stalled.
