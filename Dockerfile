# Stage 0: Build Web UI
FROM node:20-alpine AS web-builder

WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm install
COPY web/ ./
RUN npm run build

# Stage 1: Build Go binaries
FROM golang:1.22-alpine AS builder

WORKDIR /app
RUN apk add --no-cache git gcc musl-dev

COPY go.mod go.sum ./
RUN GOPROXY=https://goproxy.cn,direct go mod download

COPY . .

RUN CGO_ENABLED=1 go build -o /bin/repolens-api ./cmd/api
RUN CGO_ENABLED=1 go build -o /bin/repolens-worker ./cmd/worker
RUN CGO_ENABLED=1 go build -o /bin/repolens-eval ./cmd/eval

# Stage 2: Final minimal runtime
FROM alpine:3.19

RUN apk add --no-cache ca-certificates git tzdata

WORKDIR /app
COPY --from=builder /bin/repolens-api /app/repolens-api
COPY --from=builder /bin/repolens-worker /app/repolens-worker
COPY --from=builder /bin/repolens-eval /app/repolens-eval
COPY --from=web-builder /web/dist /app/web/dist

VOLUME /data/repositories

EXPOSE 8080 9090
CMD ["/app/repolens-api"]
