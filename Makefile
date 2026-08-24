.PHONY: all build test test-race test-integration eval verify clean fmt lint

all: build

build:
	go build -o bin/repolens-api ./cmd/api
	go build -o bin/repolens-relay ./cmd/relay
	go build -o bin/repolens-worker ./cmd/worker
	go build -o bin/repolens-eval ./cmd/eval

fmt:
	gofmt -s -w .

lint:
	go vet ./...

test:
	GOFLAGS=-mod=readonly go test ./...

test-race:
	GOFLAGS=-mod=readonly go test -race ./...

test-integration:
	GOFLAGS=-mod=readonly go test -v -race ./tests/integration/...

eval:
	go run cmd/eval/main.go

verify: fmt lint test test-race test-integration eval
	@echo "========================================================"
	@echo "All RepoLens validation gates passed successfully!"
	@echo "========================================================"

clean:
	rm -rf bin/ /tmp/repolens_*
