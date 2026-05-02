# CLAUDE.md — contextmatrix-runner

## What is this project?

contextmatrix-runner is a self-hosted runner that receives HMAC-signed webhooks
from [ContextMatrix](https://github.com/mhersson/contextmatrix) and spawns
disposable Docker containers running Claude Code to execute autonomous tasks.

It is a **separate binary in its own repository**, not part of ContextMatrix.
The CM-side webhook client, callback endpoint, and UI are in the main
ContextMatrix repo.

## Architecture

```
cmd/contextmatrix-runner/main.go  → entrypoint, wires dependencies
internal/config/                  → YAML config + env overrides + validation
internal/hmac/                    → HMAC-SHA256 signing/verification (shared)
internal/webhook/                 → HTTP handlers (/trigger, /kill, /stop-all, /health)
internal/container/               → Docker SDK abstraction, container lifecycle
internal/tracker/                 → Thread-safe card_id → container mapping
internal/callback/                → HMAC-signed status callbacks to CM
internal/github/                  → TokenGenerator interface; App (JWT → installation token) and PAT providers
docker/                           → Dockerfile.worker + entrypoint.sh
```

## Tech stack

- **Go 1.26+** — backend
- **net/http** — stdlib HTTP router
- **Docker SDK** (`github.com/docker/docker`) — container management
- **golang-jwt** (`github.com/golang-jwt/jwt/v5`) — GitHub App JWT signing
- **go-yaml v3** — config parsing
- **testify** — test assertions

## Coding conventions

Same as the main ContextMatrix repo:

- `internal/` for all packages
- Error handling: `fmt.Errorf("operation: %w", err)`
- `context.Context` first parameter for I/O functions
- No global state; dependencies injected via struct fields, wired in `main.go`
- Tests next to code, table-driven, `t.Helper()` in helpers
- `testify/assert` for assertions, `testify/require` for fatal checks
- `log/slog` for structured logging
- No `init()` functions

## Key interfaces

- `container.DockerClient` — abstracts Docker SDK for testability. Real impl in
  `RealDockerClient`, mock in tests via function fields.

## Container tool permissions

- Worker containers run `claude --allowed-tools` with an explicit allowlist
  instead of `--dangerously-skip-permissions`. See the
  `ALLOWED_TOOLS_COMMON` and `ALLOWED_TOOLS_AUTO_EXTRAS` arrays in
  `docker/entrypoint.sh`. HITL mode uses `COMMON` only; autonomous mode
  appends `Task` so sub-agents can spawn.

## Commit discipline

```bash
make test   # must be clean before every commit
make lint   # must be clean before every commit
make build  # must build
```

**NEVER** commit code without manual approval from the user. No exceptions.

**NEVER** reference the plan phase or task number in commit messages. Use
conventional commits:

**ALWAYS** keep the commit messages short, clear and focues. Use bullet points
in the message body to explain the "what" and "why" of the change, but avoid
long paragraphs.

**ALWAYS** write conventional commit messages with a type, scope, and concise
description. For example:

```
feat(mcp): Add MCP server with Streamable HTTP transport and tool definitions
feat(mcp): Add prompts capability for Claude Code slash commands
feat(skills): Add execute-task skill with heartbeat discipline
```

## Running

```bash
make build               # builds binary
make test                # runs all tests
make test-race           # with race detector
make lint                # golangci-lint
make docker-worker       # builds worker Docker image
```

## Testing

Tests use mocks — no Docker daemon required for unit tests. The
`MockDockerClient` in `container/docker_test.go` has function fields that tests
override per-method.

The GitHub token tests use `httptest.Server` as a fake GitHub API and in-memory
RSA keys generated per test.

## Webhook contract

The runner must produce and verify HMAC signatures identical to ContextMatrix's
`internal/runner/hmac.go`. The `internal/hmac/` package mirrors that code. Both
sides sign as `HMAC-SHA256(key, timestamp + "." + body)`, hex-encoded. Headers:
`X-Signature-256: sha256=<hex>`, `X-Webhook-Timestamp: <unix-ts>`.

### Endpoints

| Method | Path          | Auth | Description                                                                                                                                                                    |
| ------ | ------------- | ---- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| POST   | `/trigger`    | HMAC | Start a container. Payload includes `card_id`, `project`, `repo_url`, and optional `interactive: bool`.                                                                         |
| POST   | `/kill`       | HMAC | Stop a specific container. Payload: `{card_id, project}`.                                                                                                                      |
| POST   | `/stop-all`   | HMAC | Stop all containers (optionally filtered by project).                                                                                                                          |
| GET    | `/logs`       | HMAC | SSE stream of `LogEntry` events for all active containers. Browser EventSource cannot send headers, so consumers must proxy through a server that attaches the HMAC signature. |
| GET    | `/containers` | HMAC | List currently tracked containers.                                                                                                                                             |
| GET    | `/health`     | none | Liveness probe; returns 200 unconditionally.                                                                                                                                   |
| GET    | `/readyz`     | none | Readiness probe; returns 200 only when preflight has passed and the runner is not draining.                                                                                    |

### HITL and message dispatch

HITL chat messages, autonomous promotion, and session termination are not
handled via runner-side webhooks. They are coordinated through the
ContextMatrix server:

- The UI POSTs to `POST /api/projects/{p}/cards/{id}/message` / `/promote` /
  `/stop` on CM. CM appends a typed event to its `RunnerEventBuffer` and the
  per-card session log.
- The runner subscribes to `GET /api/runner/events` (SSE, Bearer-auth). The
  per-card driver dispatches each event into the orchestrator FSM's gate
  channels (chat input, promote, stop).
- HITL turns spawn `claude --resume` per message via the orchestrator FSM
  rather than holding a long-lived stdin-attached process. Status flows back
  to CM via `POST /api/runner/status`; skill engagements via
  `POST /api/runner/skill-engaged`.

## LogEntry types

`logbroadcast.LogEntry.Type` is a free-form string. Known values:

| Type        | Source                                    |
| ----------- | ----------------------------------------- |
| `text`      | Claude assistant text block (stdout)      |
| `thinking`  | Claude thinking block (stdout)            |
| `tool_call` | Claude tool_use block (non-MCP, stdout)   |
| `stderr`    | Container stderr line                     |
| `system`    | Runner lifecycle event (start/stop/error) |
| `user`      | HITL chat message routed via SSE          |

## Verification

```bash
make test    # must pass before every commit
make lint    # must be clean
make build   # must compile
```
