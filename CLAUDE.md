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
internal/logparser/               → Parses Claude Code stream-json output, logs relevant events
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

The GitHub token tests use `httptest.Server` as a fake GitHub API and
in-memory RSA keys generated per test.

## Webhook contract

The runner must produce and verify HMAC signatures identical to ContextMatrix's
`internal/runner/hmac.go`. The `internal/hmac/` package mirrors that code. Both
sides sign as `HMAC-SHA256(key, timestamp + "." + body)`, hex-encoded. Headers:
`X-Signature-256: sha256=<hex>`, `X-Webhook-Timestamp: <unix-ts>`.

### Endpoints

| Method | Path        | Auth | Description                                                                                  |
|--------|-------------|------|----------------------------------------------------------------------------------------------|
| POST   | `/trigger`  | HMAC | Start a container. Payload includes `card_id`, `project`, `repo_url`, `mcp_url`, and optional `interactive: bool`. |
| POST   | `/kill`     | HMAC | Stop a specific container. Payload: `{card_id, project}`.                                   |
| POST   | `/stop-all` | HMAC | Stop all containers (optionally filtered by project).                                        |
| POST   | `/message`  | HMAC | Send a user message to an interactive session. Payload: `{card_id, project, content, message_id}`. `content` must be ≤8192 bytes (413 on overflow). Returns 404 if no container, 409 if not interactive, 202 `{ok:true, message_id}` on success. |
| POST   | `/promote`  | HMAC | Promote an interactive session to autonomous mode. Payload: `{card_id, project}`. Returns 404/409 on error, 202 `{ok:true}` on success. |
| GET    | `/logs`     | none | SSE stream of `LogEntry` events for all active containers.                                   |
| GET    | `/health`   | none | Health probe; returns 200.                                                                   |

### HITL (interactive) mode

When `interactive: true` is set in the `/trigger` payload:

- The runner sets `CM_INTERACTIVE=1` in the container environment and attaches to the container's stdin.
- `entrypoint.sh` branches on `CM_INTERACTIVE`: instead of the one-shot `run-autonomous` invocation, it runs `claude` with `--input-format stream-json --output-format stream-json` and a prompt instructing Claude to wait for the user's first message.
- The tracker stashes the stdin writer; `tracker.WriteStdin` serialises concurrent writes with a per-entry mutex.
- Operators interact with the running session via:
  - `POST /message` — writes a stream-json user message to the container stdin and echoes it as a `user`-typed `LogEntry`.
  - `POST /promote` — writes a canned autonomous-mode instruction to stdin and emits a `system` LogEntry `"promoted to autonomous mode"`.
- `tracker.Remove` closes the stdin writer when the container exits.

## LogEntry types

`logbroadcast.LogEntry.Type` is a free-form string. Known values:

| Type       | Source                                      | Redacted? |
|------------|---------------------------------------------|-----------|
| `text`     | Claude assistant text block (stdout)        | yes       |
| `thinking` | Claude thinking block (stdout)              | yes       |
| `tool_call`| Claude tool_use block (non-MCP, stdout)     | no        |
| `stderr`   | Container stderr line                       | yes       |
| `system`   | Runner lifecycle event (start/stop/error)   | no        |
| `user`     | HITL chat message via /message webhook      | no        |

`logparser.Redact` is applied only to `text`, `thinking`, and `stderr` entries
(i.e. container output paths). It is never called on `user` or `system` entries.

## Verification

```bash
make test    # must pass before every commit
make lint    # must be clean
make build   # must compile
```
