#!/usr/bin/env bash
set -euo pipefail

# Build the test with the same pinned compiler as the production worker, then
# execute it in the actual Compose worker image, with networking disabled.
worker_image=${1:-repolens-worker:latest}
runtime_tmp=$(mktemp -d)
runtime_container=
cleanup() {
    if [ -n "$runtime_container" ]; then docker rm -f "$runtime_container" >/dev/null; fi
    rm -rf "$runtime_tmp"
}
trap cleanup EXIT

docker build --target codeintel-runtime-test -t repolens-codeintel-runtime-test .
runtime_container=$(docker create repolens-codeintel-runtime-test)
docker cp "$runtime_container:/app/codeintel-runtime.test" "$runtime_tmp/codeintel-runtime.test"
docker rm "$runtime_container" >/dev/null
runtime_container=
docker run --rm --network none \
    --mount "type=bind,src=$runtime_tmp/codeintel-runtime.test,dst=/app/codeintel-runtime.test,readonly" \
    "$worker_image" /app/codeintel-runtime.test -test.run='^TestRuntimeStdlibAndOfflineBoundary$' -test.v
