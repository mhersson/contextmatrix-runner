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
internal/webhook/                 → HTTP handlers (/trigger, /kill, /stop-all, /message,
                                    /promote, /end-session, /chat/start, /chat/end,
                                    /logs, /containers, /health, /readyz)
internal/container/               → Docker SDK abstraction, container lifecycle
internal/logparser/               → Parses Claude Code stream-json output, logs relevant events
internal/logbroadcast/            → In-process LogEntry pub/sub for the /logs SSE stream
                                    (keyed by card_id OR session_id)
internal/tracker/                 → Thread-safe (card_id|session_id) → container mapping
internal/callback/                → HMAC-signed status callbacks to CM
internal/metrics/                 → Prometheus metric vars + Register()
internal/preflight/               → Startup readiness checks (dockerd reachability, etc.)
internal/streammsg/               → Builders for Claude Code stream-json user messages
internal/tracing/                 → OpenTelemetry tracer wiring (OTLP HTTP exporter)
docker/                           → Dockerfile.worker + entrypoint.sh
```

GitHub authentication is not an internal package: the runner imports the
shared `github.com/mhersson/contextmatrix-githubauth` module from
`cmd/contextmatrix-runner/main.go`. The same module is consumed by the
contextmatrix server, so the App (JWT → installation token) and PAT
providers stay in lockstep across the two repos.

## Tech stack

- **Go 1.26+** — backend
- **net/http** — stdlib HTTP router
- **Docker SDK** (`github.com/docker/docker`) — container management
- **`github.com/mhersson/contextmatrix-githubauth`** — shared GitHub auth
  module (App + PAT + caching). GitHub App JWT signing now lives inside
  this module; `golang-jwt/jwt/v5` is only an indirect dependency of
  the runner via the auth module.
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
- `mcp__contextmatrix__chat_rehydration_complete` lives in
  `ALLOWED_TOOLS_COMMON`. Chat mode resolves `ALLOWED_TOOLS_CHAT =
  COMMON + AUTO_EXTRAS`, so the tool is callable from rehydrating agents.
  Removing or renaming it silently breaks the rehydration flow — the agent
  gets blocked by the permission gate and the "Restoring workspace…" banner
  in the UI never lifts.

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
sides sign as
`HMAC-SHA256(key, method + "\n" + uri + "\n" + timestamp + "." + body)`,
hex-encoded, where `uri` is the request-target form (path plus `?rawquery`
when present, matching `r.URL.RequestURI()` on the receiver). Binding the
HTTP method and URI into the signature prevents a valid signature for one
endpoint from being replayed against another, and prevents two same-second
GETs to the same path with different query strings from colliding in the
replay cache. Headers: `X-Signature-256: sha256=<hex>`,
`X-Webhook-Timestamp: <unix-ts>`.

### Endpoints

| Method | Path           | Auth | Description                                                                                                                                                                                                                                                          |
| ------ | -------------- | ---- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| POST   | `/trigger`     | HMAC | Start a container. Payload includes `card_id`, `project`, `repo_url`, and optional `interactive: bool`.                                                                                                                                                               |
| POST   | `/kill`        | HMAC | Stop a specific container. Payload: `{card_id, project}`.                                                                                                                                                                                                            |
| POST   | `/stop-all`    | HMAC | Stop all containers (optionally filtered by project).                                                                                                                                                                                                                |
| POST   | `/message`     | HMAC | Send a user message to a running session. Card-mode payload: `{card_id, project, content, message_id}`. Chat-mode payload: `{session_id, content, message_id}` (mutually exclusive with `card_id`/`project`). `content` must be ≤8192 bytes (413 on overflow). Returns 404 if no container, 409 if not interactive, 202 `{ok:true, message_id}` on success. |
| POST   | `/promote`     | HMAC | Promote an interactive session to autonomous mode. Payload: `{card_id, project}`. Returns 404/409 on error, 202 `{ok:true}` on success.                                                                                                                              |
| POST   | `/end-session` | HMAC | Close the stdin of an interactive container so claude exits on EOF. Payload: `{card_id, project}`. Returns 404 if no container, 409 if not interactive (or stdin already closed), 202 `{ok:true}` on success. Safe to call more than once (second call returns 409). |
| POST   | `/refresh-knowledge` | HMAC | Spawn a containerised knowledge-base refresh for the given repo. Synthetic tracker key `kb-refresh:<repo>`. |
| POST   | `/chat/start`  | HMAC | Start a long-lived chat container for a global chat session. Payload: `{session_id, project?, repo_url?, mcp_api_key?, model, resume?}`. `model` is forwarded as `CM_ORCHESTRATOR_MODEL` (entrypoint passes it as `--model`). `resume` is an optional array of `{seq, role, content}` turns; when present, the runner writes `resume.jsonl` + `resume.meta.json` into a per-container host dir and bind-mounts it read-only at `/run/cm-chat/`, sets `CM_CHAT_RESUME=1`, and writes a stream-json user envelope to stdin priming the agent to read the resume file and call `chat_rehydration_complete`. File-prep failures are logged and the container starts without rehydration. Returns 202 `{ok:true, container_id}` on success, 409 if the session already has a container, 429 when the chat concurrency cap is reached. |
| POST   | `/chat/end`    | HMAC | End a tracked chat container: closes stdin, force-stops, removes tracker entry. Payload: `{session_id}`. Returns 200 on success, 404 if no container is tracked. A second call returns 404.                                                                          |
| GET    | `/logs`        | HMAC | SSE stream of `LogEntry` events. Query: `?project=<name>` for card-mode / project-scoped, or `?session_id=<id>` for chat-mode. The two filters are mutually exclusive. Browser EventSource cannot send headers, so consumers must proxy through a server that attaches the HMAC signature. |
| GET    | `/containers`  | HMAC | List currently-running worker containers (`ListContainersResponse`). Used by CM to age-cap runaway containers and detect tracker drift (`Tracked=false` while `State="running"`).                                                                                    |
| GET    | `/health`      | none | Health probe; returns 200.                                                                                                                                                                                                                                           |
| GET    | `/readyz`      | none | Readiness probe. Returns 200 only when preflight has passed and the runner is not draining; 503 otherwise. Unauthenticated so orchestrators / load balancers can poll without HMAC credentials.                                                                       |

### HITL (interactive) mode

When `interactive: true` is set in the `/trigger` payload:

- The runner sets `CM_INTERACTIVE=1` in the container environment and attaches
  to the container's stdin.
- `entrypoint.sh` branches on `CM_INTERACTIVE`: instead of the one-shot
  `run-autonomous` invocation, it runs `claude` with
  `--input-format stream-json --output-format stream-json` and a minimal
  system-context hint as the `-p` prompt. After registering the stdin writer via
  `tracker.SetStdin`, the runner writes a priming stream-json user message
  (built via `streammsg.BuildUserMessage`) that instructs Claude to call
  `get_skill(skill_name='create-plan', ...)` immediately — so plan drafting
  begins without waiting for a human to type anything. The user provides
  approval at the skill's built-in gates (plan approval, execution decision,
  review) via stream-json input.
- The tracker stashes the stdin writer; `tracker.WriteStdin` serialises
  concurrent writes with a per-entry mutex.
- Operators interact with the running session via:
  - `POST /message` — writes a stream-json user message to the container stdin
    and echoes it as a `user`-typed `LogEntry`.
  - `POST /promote` — writes a canned autonomous-mode instruction to stdin and
    emits a `system` LogEntry `"promoted to autonomous mode"`.
  - `POST /end-session` — closes the container's stdin via `tracker.CloseStdin`.
    claude receives EOF on stdin, exits the stream-json loop, and the container
    terminates through the normal `waitAndCleanup` path. Emits a `system`
    LogEntry `"session ended (stdin closed)"`. Used by ContextMatrix when a
    released card reaches a terminal state.
- `tracker.Remove` closes the stdin writer when the container exits (no-op if
  `/end-session` already closed it).

## LogEntry types

`logbroadcast.LogEntry.Type` is a free-form string. Known values:

| Type        | Source                                    | Redacted? |
| ----------- | ----------------------------------------- | --------- |
| `text`      | Claude assistant text block (stdout)      | yes       |
| `thinking`  | Claude thinking block (stdout)            | yes       |
| `tool_call` | Claude tool_use block (non-MCP, stdout)   | yes       |
| `stderr`    | Container stderr line                     | yes       |
| `system`    | Runner lifecycle event (start/stop/error) | no        |
| `user`      | HITL chat message via /message webhook    | no        |
| `usage`     | per-turn token usage / cost reports       | no        |

`logparser.Redact` is applied to `text`, `thinking`, `stderr`, and `tool_call`
entries. It is never called on `user` or `system` entries.

`tool_call` content is formatted as `Name: <summary>` (e.g. `Bash: git status`,
`Read: /tmp/foo.go`). The summary is a per-tool extract of the most relevant
argument: first line of `command` for Bash; `file_path` for
Read/Edit/Write/MultiEdit/NotebookEdit; `pattern [in <path>]` for Glob/Grep;
`url` for WebFetch; `query` for WebSearch; `description` for Task/Agent;
`N todos` for TodoWrite; compact JSON fallback for unknown tools. The result is
whitespace-collapsed and truncated at 200 runes with a trailing `…`.

## Verification

```bash
make test    # must pass before every commit
make lint    # must be clean
make build   # must compile
```
