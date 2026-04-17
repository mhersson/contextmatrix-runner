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

## Verification

```bash
make test    # must pass before every commit
make lint    # must be clean
make build   # must compile
```
