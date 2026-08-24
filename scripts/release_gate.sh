#!/usr/bin/env bash
set -euo pipefail

echo "================================================================="
echo "RepoLens Final Release Gate Validation"
echo "================================================================="

echo "[1/5] Checking code formatting..."
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
    echo "ERROR: Unformatted files detected:"
    echo "$unformatted"
    exit 1
fi
echo "✓ Code formatting clean"

echo "[2/5] Running go vet..."
go vet ./...
echo "✓ go vet passed"

echo "[3/5] Running unit and component tests with race detector..."
GOFLAGS=-mod=readonly go test -race ./internal/...
echo "✓ Unit tests passed"

echo "[4/6] Running unit and component integration tests with race detector..."
GOFLAGS=-mod=readonly go test -v -race ./tests/integration/...
echo "✓ Component integration tests passed"

echo "[5/6] Running real testcontainers integration tests..."
GOFLAGS=-mod=readonly go test -v ./tests/integration_real/...
echo "✓ Real testcontainers integration tests passed"

echo "[6/6] Running offline 32-case eval benchmark..."
go run cmd/eval/main.go
echo "✓ Eval benchmark passed"

echo "================================================================="
echo "ALL GATES PASSED: RepoLens is verified and ready for external final audit!"
echo "================================================================="
