.PHONY: build test test-race test-integration lint lint-sh vuln verify-unit clean docker-worker

# Pinned worker toolchain versions. Override on the command line
# if a newer version has been vetted, e.g.
#   make docker-worker GO_VERSION=1.26.3
# These values are passed into the Dockerfile as --build-args so the build is
# reproducible from CI and local shells alike.
GO_VERSION              ?= 1.26.3
GO_SHA256_AMD64         ?= 2b2cfc7148493da5e73981bffbf3353af381d5f93e789c82c79aff64962eb556
GO_SHA256_ARM64         ?= 9d89a3ea57d141c2b22d70083f2c8459ba3890f2d9e818e7e933b75614936565
GOPLS_VERSION           ?= v0.22.0
GOLANGCI_LINT_VERSION   ?= v2.12.2
CLAUDE_CODE_VERSION     ?= 2.1.156
TYPESCRIPT_LSP_VERSION  ?= 5.3.0
TYPESCRIPT_VERSION      ?= 6.0.3

# Image tag components. SHORT_SHA defaults to the current HEAD short hash but
# CI can pin it explicitly (e.g. to the commit that produced the build).
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
SHORT_SHA  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
WORKER_IMAGE_NAME ?= contextmatrix/worker
WORKER_IMAGE_TAG  ?= $(VERSION)-$(SHORT_SHA)

build:
	go build -o contextmatrix-runner ./cmd/contextmatrix-runner

install:
	go install ./cmd/contextmatrix-runner

test:
	go test ./...

test-race:
	CGO_ENABLED=1 go test -race ./...

# test-integration runs the integration build tag against a real Docker
# daemon. Slower (~30s) but exercises ContainerStart / ContainerWait /
# ContainerAttach / stdcopy against the real SDK so a Docker upgrade
# can't silently break the runner.
test-integration:
	@if ! docker info >/dev/null 2>&1; then \
		echo "docker daemon not reachable; skipping integration tests"; \
		exit 1; \
	fi
	CGO_ENABLED=1 go test -tags integration -race -count=1 ./internal/container/...

lint:
	golangci-lint run

lint-sh:
	shellcheck svc.sh docker/entrypoint.sh

# Run the same supply-chain scan CI runs so developers can catch
# vulnerabilities locally before pushing.
vuln:
	@if ! command -v govulncheck >/dev/null 2>&1; then \
		echo "installing govulncheck..."; \
		go install golang.org/x/vuln/cmd/govulncheck@latest; \
	fi
	govulncheck ./...

# verify-unit grep-asserts that the generated systemd unit contains the
# expected hardening directives, and runs `systemd-analyze --user
# verify` if available. No Go build required.
verify-unit:
	./svc.sh verify

docker-worker:
	docker build \
		-f docker/Dockerfile.worker \
		--build-arg GO_VERSION=$(GO_VERSION) \
		--build-arg GO_SHA256_AMD64=$(GO_SHA256_AMD64) \
		--build-arg GO_SHA256_ARM64=$(GO_SHA256_ARM64) \
		--build-arg GOPLS_VERSION=$(GOPLS_VERSION) \
		--build-arg GOLANGCI_LINT_VERSION=$(GOLANGCI_LINT_VERSION) \
		--build-arg CLAUDE_CODE_VERSION=$(CLAUDE_CODE_VERSION) \
		--build-arg TYPESCRIPT_LSP_VERSION=$(TYPESCRIPT_LSP_VERSION) \
		--build-arg TYPESCRIPT_VERSION=$(TYPESCRIPT_VERSION) \
		-t $(WORKER_IMAGE_NAME):$(WORKER_IMAGE_TAG) \
		-t $(WORKER_IMAGE_NAME):latest \
		docker/

clean:
	rm -f contextmatrix-runner
