# syntax=docker/dockerfile:1.7
# Build stage
FROM golang:1.26.8-alpine AS builder

ENV GOTOOLCHAIN=auto CGO_ENABLED=0

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files first for better layer caching
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source code
COPY . .

# Build the server binary
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -ldflags="-s -w" -o /app/caddy ./cmd/caddy

# Runtime stage
FROM alpine:3.24

RUN apk add --no-cache ca-certificates wget \
    && adduser -D -H -u 10001 nuts \
    && mkdir -p /config/caddy /data/caddy \
    && chown -R nuts:nuts /config /data

# The nuts user has no home directory. Caddy keeps its autosaved config and
# its data (certificates, locks) here instead, as the official Caddy image
# does.
ENV XDG_CONFIG_HOME=/config \
    XDG_DATA_HOME=/data

WORKDIR /app

COPY --from=builder --chown=nuts:nuts /app/caddy /app/caddy
COPY --chown=nuts:nuts Caddyfile /app/Caddyfile

USER nuts:nuts

EXPOSE 8080

CMD ["/app/caddy", "run", "--config", "/app/Caddyfile"]
