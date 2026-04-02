.PHONY: build test test-race lint clean docker-worker

build:
	go build -o contextmatrix-runner ./cmd/contextmatrix-runner

test:
	go test ./...

test-race:
	CGO_ENABLED=1 go test -race ./...

lint:
	golangci-lint run

docker-worker:
	docker build -f docker/Dockerfile.worker -t contextmatrix/worker:latest docker/

clean:
	rm -f contextmatrix-runner
