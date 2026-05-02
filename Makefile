.PHONY: build test test-race lint lint-sh vuln verify-unit clean docker-orchestrated gen-fsm cm-integration-test

# Pinned worker toolchain versions (CTXRUN-044). Override on the command line
# if a newer version has been vetted, e.g.
#   make docker-orchestrated GO_VERSION=1.26.3
# These values are passed into the Dockerfile as --build-args so the build is
# reproducible from CI and local shells alike.
GO_VERSION              ?= 1.26.2
GO_SHA256_AMD64         ?= 990e6b4bbba816dc3ee129eaeaf4b42f17c2800b88a2166c265ac1a200262282
GO_SHA256_ARM64         ?= c958a1fe1b361391db163a485e21f5f228142d6f8b584f6bef89b26f66dc5b23
GOPLS_VERSION           ?= v0.21.1
GOLANGCI_LINT_VERSION   ?= v2.11.4
CLAUDE_CODE_VERSION     ?= 2.1.116

# Image tag components. SHORT_SHA defaults to the current HEAD short hash but
# CI can pin it explicitly (e.g. to the commit that produced the build).
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
SHORT_SHA  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
AGENT_IMAGE_NAME  ?= contextmatrix/agent
AGENT_IMAGE_TAG   ?= $(VERSION)-$(SHORT_SHA)

build:
	go build -o contextmatrix-runner ./cmd/contextmatrix-runner

test:
	go test ./...

test-race:
	CGO_ENABLED=1 go test -race ./...

lint:
	golangci-lint run

lint-sh:
	shellcheck svc.sh docker/entrypoint-orchestrated.sh

# Run the same supply-chain scan CI runs so developers can catch
# vulnerabilities locally before pushing.
vuln:
	@if ! command -v govulncheck >/dev/null 2>&1; then \
		echo "installing govulncheck..."; \
		go install golang.org/x/vuln/cmd/govulncheck@latest; \
	fi
	govulncheck ./...

# verify-unit grep-asserts that the generated systemd unit contains the
# CTXRUN-052 hardening directives, and runs `systemd-analyze --user
# verify` if available. No Go build required.
verify-unit:
	./svc.sh verify

# Orchestrated worker image (long-lived shell entrypoint).
# Build context is the repo root so Dockerfile.orchestrated's
# COPY docker/entrypoint-orchestrated.sh ... resolves correctly.
docker-orchestrated:
	docker build \
		-f docker/Dockerfile.orchestrated \
		--build-arg GO_VERSION=$(GO_VERSION) \
		--build-arg GO_SHA256_AMD64=$(GO_SHA256_AMD64) \
		--build-arg GO_SHA256_ARM64=$(GO_SHA256_ARM64) \
		--build-arg GOPLS_VERSION=$(GOPLS_VERSION) \
		--build-arg GOLANGCI_LINT_VERSION=$(GOLANGCI_LINT_VERSION) \
		--build-arg CLAUDE_CODE_VERSION=$(CLAUDE_CODE_VERSION) \
		-t $(AGENT_IMAGE_NAME):$(AGENT_IMAGE_TAG) \
		-t $(AGENT_IMAGE_NAME):latest \
		.

# Regenerate the orchestrator FSM runtime from internal/orchestrator/orchestrator.md.
#
# Vectorsigma rejects relative paths leading with `./` or `..` and requires
# the output to be a sub-directory of CWD, so the directive must run from
# the repo root with -o internal -p orchestrator (writes to internal/orchestrator/).
#
# IMPORTANT: vectorsigma may overwrite hand-edited actions.go / guards.go /
# extendedstate.go stubs. The implementations there carry vectorsigma
# marker comments (// +vectorsigma:action:Foo) so it should leave customized
# functions alone, but verify with `git diff` before committing the regeneration.
gen-fsm:
	@if ! command -v vectorsigma >/dev/null 2>&1; then \
		echo "installing vectorsigma..."; \
		go install github.com/mhersson/vectorsigma@latest; \
	fi
	vectorsigma -i internal/orchestrator/orchestrator.md -o internal -p orchestrator

cm-integration-test:
	$(MAKE) -C ../contextmatrix integration-test

clean:
	rm -f contextmatrix-runner
