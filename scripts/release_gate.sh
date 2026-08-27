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

echo "[8/9] Validating Compose and image build..."
docker compose config >/dev/null
docker compose build

echo "[9/9] Running product smoke against the Compose stack..."
cleanup() { docker compose down >/dev/null 2>&1 || true; }
trap cleanup EXIT
docker compose up -d
for attempt in $(seq 1 30); do
    if curl --fail --silent http://127.0.0.1:8080/healthz >/dev/null; then break; fi
    if [ "$attempt" -eq 30 ]; then echo "ERROR: API health check timed out"; exit 1; fi
    sleep 2
done
demo_response=$(curl --fail --silent -X POST -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:8080/api/v1/demo/trigger)
echo "$demo_response" | grep -q 'diagnosis_id'
echo "✓ Product health and real Demo smoke passed"

echo "================================================================="
echo "ALL GATES PASSED: RepoLens is verified and ready for release!"
echo "================================================================="
