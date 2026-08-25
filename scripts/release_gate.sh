#!/usr/bin/env bash
set -euo pipefail

echo "================================================================="
echo "RepoLens Final Release Gate Validation"
echo "================================================================="

# Prerequisite Check: Docker Daemon for Real Testcontainers
echo "Checking Docker daemon prerequisite..."
if ! docker info >/dev/null 2>&1; then
    echo "FATAL: Docker daemon is not running or not accessible."
    echo "Real infrastructure integration tests require a running Docker daemon."
    exit 1
fi
echo "✓ Docker daemon accessible"

echo "[1/6] Checking code formatting..."
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
    echo "ERROR: Unformatted files detected:"
    echo "$unformatted"
    exit 1
fi
echo "✓ Code formatting clean"

echo "[2/6] Running go vet..."
go vet ./...
echo "✓ go vet passed"

echo "[3/6] Running unit and component tests with race detector..."
GOFLAGS=-mod=readonly go test -race ./internal/...
echo "✓ Unit tests passed"

echo "[4/6] Running component integration tests with race detector..."
GOFLAGS=-mod=readonly go test -v -race ./tests/integration/...
echo "✓ Component integration tests passed"

echo "[5/6] Running real testcontainers integration tests (MySQL, RabbitMQ, Elasticsearch) with race detector..."
export REPOLENS_REQUIRE_REAL_INTEGRATION=1
GOFLAGS=-mod=readonly go test -v -race ./tests/integration_real/...
echo "✓ Real testcontainers integration tests passed (0 skips, all containers executed)"

echo "[6/6] Running offline 32-case eval benchmark with ground truth verification..."
go run cmd/eval/main.go
echo "✓ Eval benchmark passed"

echo "================================================================="
echo "ALL GATES PASSED: RepoLens is verified and ready for external final audit!"
echo "================================================================="
