#!/usr/bin/env bash
set -euo pipefail

echo "================================================================="
echo "RepoLens Final Release Gate Validation"
echo "================================================================="

echo "[1/8] Checking code formatting..."
unformatted=$(gofmt -l cmd/ internal/ tests/)
if [ -n "$unformatted" ]; then
    echo "ERROR: Unformatted files detected:"
    echo "$unformatted"
    exit 1
fi
echo "✓ Code formatting clean"

echo "[2/8] Running go vet..."
go vet ./...
echo "✓ go vet passed"

echo "[3/8] Running all Go tests..."
GOFLAGS=-mod=readonly go test ./...
echo "✓ Unit tests passed"

echo "[4/8] Running Go tests with race detector..."
GOFLAGS=-mod=readonly go test -race ./...
echo "✓ Race tests passed"

echo "[5/8] Running component integration tests..."
GOFLAGS=-mod=readonly go test -v ./tests/integration/...
echo "✓ Component integration tests passed"

echo "[6/8] Running real MySQL integration tests..."
export REPOLENS_REQUIRE_REAL_INTEGRATION=1
GOFLAGS=-mod=readonly go test -v -race ./tests/integration_real/...
echo "✓ Real testcontainers integration tests passed (0 skips, all containers executed)"

echo "[7/8] Building deterministic Web UI and running eval..."
(cd web && npm ci && npm run build)
go run ./cmd/eval
echo "✓ Eval benchmark passed"

echo "[8/8] Validating Compose and image build..."
docker compose config >/dev/null
docker compose build

echo "================================================================="
echo "ALL GATES PASSED: RepoLens is verified and ready for release!"
echo "================================================================="
