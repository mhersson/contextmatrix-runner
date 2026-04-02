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
- A GitHub account (for the GitHub App)

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

## GitHub App Setup

The runner uses a GitHub App to generate short-lived installation tokens for git
operations inside containers. This is the most secure approach: the App's
private key stays on the runner host, and only ephemeral tokens (valid for 1
hour) enter containers.

**No inbound connections required.** The runner only makes outbound HTTPS calls
to `api.github.com`. It works on a local LAN with no public domain.

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
github_app:
  app_id: 123456
  installation_id: 78901234
  private_key_path: "/home/you/.config/contextmatrix-runner/app.pem"
```

## Configuration

All fields can be overridden with environment variables using the `CMR_` prefix.

```yaml
# HTTP port for receiving webhooks from ContextMatrix.
# Env: CMR_PORT
port: 9090

# Base URL of the ContextMatrix instance (for status callbacks).
# Env: CMR_CONTEXTMATRIX_URL
contextmatrix_url: "http://localhost:8080"

# Shared HMAC-SHA256 secret. Must match runner.api_key in ContextMatrix config.
# At least 32 characters.
# Env: CMR_API_KEY
api_key: "your-shared-secret-here-at-least-32-chars"

# Default Docker image for worker containers.
# Env: CMR_BASE_IMAGE
base_image: "contextmatrix/worker:latest"

# When to pull the image: always, never, if-not-present.
# Use "never" or "if-not-present" for locally-built images.
# Env: CMR_IMAGE_PULL_POLICY
image_pull_policy: "always"

# Maximum simultaneous containers.
# Env: CMR_MAX_CONCURRENT
max_concurrent: 3

# Force-kill containers after this duration.
# Env: CMR_CONTAINER_TIMEOUT
container_timeout: "2h"

# Path to host's ~/.claude/ directory (OAuth tokens).
# Required unless anthropic_api_key is set.
# Env: CMR_CLAUDE_AUTH_DIR
claude_auth_dir: "/home/you/.claude"

# Alternative: Anthropic API key (instead of OAuth mount).
# Env: CMR_ANTHROPIC_API_KEY
anthropic_api_key: ""

# GitHub App credentials (see setup above).
github_app:
  app_id: 0 # CMR_GITHUB_APP_ID
  installation_id: 0 # CMR_GITHUB_INSTALLATION_ID
  private_key_path: "" # CMR_GITHUB_PRIVATE_KEY_PATH

# Log level: debug, info, warn, error.
# Env: CMR_LOG_LEVEL
log_level: "info"
```

## ContextMatrix Configuration

On the ContextMatrix side, configure the runner connection in `config.yaml`:

```yaml
# MCP endpoint authentication (optional but recommended).
mcp_api_key: "your-mcp-bearer-token"

# Runner integration.
runner:
  enabled: true
  url: "http://localhost:9090" # Runner's address
  api_key: "same-shared-secret-as-runner" # Must match runner's api_key
  public_url: "http://your-host:8080" # URL containers use to reach CM
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
3. Runner generates a short-lived GitHub App token
4. Runner pulls the Docker image and starts a container with:
   - Claude Code CLI pre-installed
   - MCP config pointing to ContextMatrix
   - GitHub App token for git operations
   - Anthropic auth (OAuth tokens or API key)
5. Claude Code runs the `run-autonomous` workflow:
   - Claims the card via MCP
   - Clones the repo, plans, executes, reviews, documents
   - Creates a feature branch and PR
   - Completes the card via MCP `complete_task`
6. Container exits, runner cleans up

**On kill**: Container is destroyed immediately. All uncommitted work is
discarded.

## Webhook Protocol

All webhooks are signed with HMAC-SHA256 using a shared secret.

| Direction   | Endpoint                  | Purpose                         |
| ----------- | ------------------------- | ------------------------------- |
| CM → Runner | `POST /trigger`           | Start a task                    |
| CM → Runner | `POST /kill`              | Stop a specific task            |
| CM → Runner | `POST /stop-all`          | Stop all tasks (or per-project) |
| Runner → CM | `POST /api/runner/status` | Report container status         |

Signatures: `X-Signature-256: sha256={hex}`, `X-Webhook-Timestamp: {unix-ts}`.
HMAC computed over `timestamp.body`. Max 5-minute clock skew.

## API Endpoints

| Method | Path        | Description                                  |
| ------ | ----------- | -------------------------------------------- |
| POST   | `/trigger`  | Start a container for a card (HMAC required) |
| POST   | `/kill`     | Kill a specific container (HMAC required)    |
| POST   | `/stop-all` | Kill all containers (HMAC required)          |
| GET    | `/health`   | Health check (no auth)                       |

## Security Model

- **HMAC-SHA256 webhook signing** in both directions (shared secret, never
  transmitted)
- **GitHub App tokens**: short-lived (1 hour), repo-scoped, only ephemeral
  tokens enter containers
- **Human-only controls**: only humans can trigger Run/Stop from the CM web UI
- **Disposable containers**: fresh environment per task, destroyed after
  completion
- **Read-only mounts**: OAuth tokens mounted read-only into containers
- **No inbound connections**: runner only makes outbound calls to GitHub API and
  ContextMatrix

## Troubleshooting

### Container fails with "generate git token" error

- Verify `github_app.private_key_path` points to a valid PEM file
- Verify `github_app.app_id` and `installation_id` are correct
- Check that the GitHub App is installed on the target repositories

### Container fails with git clone error

- Verify the repo URL in the ContextMatrix project config matches an installed
  repo
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

- Verify `runner.public_url` in ContextMatrix config is reachable from inside
  Docker containers
- For Docker Desktop: use `host.docker.internal` instead of `localhost`
- For Docker on Linux: use the host's LAN IP or configure Docker networking

### Orphan containers after runner crash

The runner automatically cleans up orphan containers on startup (identified by
the `contextmatrix.runner=true` label). ContextMatrix will detect the heartbeat
timeout (default 30 minutes) and mark the card as stalled.
