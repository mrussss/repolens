#!/usr/bin/env bash
set -euo pipefail

echo "================================================================="
echo "RepoLens Final Release Gate Validation"
echo "================================================================="

echo "[1/10] Checking code formatting..."
unformatted=$(gofmt -l cmd/ internal/ tests/)
if [ -n "$unformatted" ]; then
    echo "ERROR: Unformatted files detected:"
    echo "$unformatted"
    exit 1
fi
echo "✓ Code formatting clean"

echo "[2/10] Running go vet..."
go vet ./...
echo "✓ go vet passed"

echo "[3/10] Running core tests..."
GOFLAGS=-mod=readonly go test ./cmd/... ./internal/...
echo "✓ Core tests passed"

echo "[4/10] Running core tests with race detector..."
GOFLAGS=-mod=readonly go test -race ./cmd/... ./internal/...
echo "✓ Core race tests passed"

echo "[5/10] Running component integration tests with race detector..."
GOFLAGS=-mod=readonly go test -race ./tests/integration/...
echo "✓ Component integration tests passed"

echo "[6/10] Running real MySQL integration tests with race detector..."
REPOLENS_REQUIRE_REAL_INTEGRATION=1 GOFLAGS=-mod=readonly go test -race ./tests/integration_real/...
echo "✓ Real MySQL integration tests passed (required mode, 0 skips)"

echo "[7/10] Running the required real-MySQL Golden Path with race detector..."
REPOLENS_REQUIRE_REAL_INTEGRATION=1 GOFLAGS=-mod=readonly go test -race ./tests/e2e/...
echo "✓ Golden Path passed"

echo "[8/10] Building deterministic Web UI, running Web tests and eval..."
(cd web && npm ci && npm run build && npm test -- --run)
go run ./cmd/eval
echo "✓ Eval benchmark passed"

echo "[9/10] Validating Compose and image build..."
docker compose config >/dev/null
docker compose build

echo "[10/10] Running product smoke against the Compose stack..."
cleanup() { docker compose down >/dev/null 2>&1 || true; }
trap cleanup EXIT
docker compose up -d
for attempt in $(seq 1 30); do
    if curl --noproxy '*' --max-time 5 --fail --silent http://127.0.0.1:8080/healthz >/dev/null; then break; fi
    if [ "$attempt" -eq 30 ]; then echo "ERROR: API health check timed out"; exit 1; fi
    sleep 2
done
demo_response=$(curl --noproxy '*' --max-time 30 --fail --silent -X POST -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:8080/api/v1/demo/trigger)
echo "$demo_response" | grep -q 'diagnosis_id'
echo "✓ Product health and real Demo smoke passed"

echo "================================================================="
echo "ALL GATES PASSED: RepoLens is verified and ready for release!"
echo "================================================================="
