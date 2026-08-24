# Stage 1: Build binary
FROM golang:1.22-alpine AS builder

WORKDIR /app
RUN apk add --no-cache git gcc musl-dev

COPY go.mod go.sum ./
RUN GOPROXY=https://goproxy.cn,direct go mod download

COPY . .

RUN CGO_ENABLED=1 go build -o /bin/repolens-api ./cmd/api
RUN CGO_ENABLED=1 go build -o /bin/repolens-relay ./cmd/relay
RUN CGO_ENABLED=1 go build -o /bin/repolens-worker ./cmd/worker
RUN CGO_ENABLED=1 go build -o /bin/repolens-eval ./cmd/eval

# Stage 2: Final minimal runtime
FROM alpine:3.19

RUN apk add --no-cache ca-certificates git tzdata

WORKDIR /app
COPY --from=builder /bin/repolens-api /app/repolens-api
COPY --from=builder /bin/repolens-relay /app/repolens-relay
COPY --from=builder /bin/repolens-worker /app/repolens-worker
COPY --from=builder /bin/repolens-eval /app/repolens-eval

VOLUME /data/repositories

EXPOSE 8080 9090
CMD ["/app/repolens-api"]
