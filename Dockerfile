# Stage 0: Build Web UI
FROM node:20-alpine AS web-builder

WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 1: Build Go binaries
FROM golang:1.22-alpine AS builder

WORKDIR /app
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN GOPROXY=https://goproxy.cn,direct go mod download

# Copy only source required to build production binaries.
# Local RealBench artifacts/cache can be several GB; keep them out of this stage.
COPY cmd ./cmd
COPY internal ./internal
COPY contracts ./contracts

# The production Compose profile uses MySQL. Disable cgo so the Alpine image
# does not depend on musl-specific sqlite3 headers; SQLite remains available
# for host-side unit/integration tests.
RUN CGO_ENABLED=0 go build -o /bin/repolens-api ./cmd/api
RUN CGO_ENABLED=0 go build -o /bin/repolens-worker ./cmd/worker
RUN CGO_ENABLED=0 go build -o /bin/repolens-eval ./cmd/eval

# Stage 2: Production runtime with pinned, offline standard-library resources.
FROM alpine:3.19 AS runtime

RUN apk add --no-cache ca-certificates git tzdata

# Copy only the toolchain, never the builder module/build caches. go/importer
# needs the Go command, source tree and compiler to obtain stdlib export data.
COPY --from=builder /usr/local/go /usr/local/go
ENV GOROOT=/usr/local/go PATH=/usr/local/go/bin:$PATH GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOENV=off

WORKDIR /app
COPY --from=builder /bin/repolens-api /app/repolens-api
COPY --from=builder /bin/repolens-worker /app/repolens-worker
COPY --from=builder /bin/repolens-eval /app/repolens-eval
COPY --from=web-builder /web/dist /app/web/dist
COPY migrations /app/migrations

VOLUME /data/repositories
VOLUME /data/secrets

EXPOSE 8080
CMD ["/app/repolens-api"]

# Exercise the same analyzer in the final runtime, using a compiled Go test.
FROM builder AS codeintel-test-builder
RUN CGO_ENABLED=0 go test -c -o /bin/codeintel-runtime.test ./internal/codeintel

FROM runtime AS codeintel-runtime-test
COPY --from=codeintel-test-builder /bin/codeintel-runtime.test /app/codeintel-runtime.test
CMD ["/app/codeintel-runtime.test", "-test.run=^TestRuntimeStdlibAndOfflineBoundary$", "-test.v"]

# Keep the production stage last so Compose builds the ordinary runtime image.
FROM runtime AS production
