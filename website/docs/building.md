---
layout: docs
title: Building & Running
description: "How to build and run NUTS — with or without Docker, including xcaddy, source builds, the prebuilt Docker image, and Compose"
permalink: /docs/building/
---

NUTS can be run in three main ways:

1. **As a standalone Caddy binary** built with `xcaddy` or from source.
2. **As a prebuilt Docker image** (`idcttech/nuts`) you pull and run.
3. **Via Docker Compose**, which brings up NATS + NUTS together.

Pick whichever matches your environment — they all serve the same SSE endpoints.

## Prerequisites

- **Go 1.26.4+** — only needed if you build the binary yourself ([download](https://go.dev/dl/))
- **[xcaddy](https://github.com/caddyserver/xcaddy)** — recommended for binary builds
- **Docker** _(optional)_ — for image / Compose-based runs and the BDD test stack
- **[NATS CLI](https://github.com/nats-io/natscli)** _(optional)_ — useful for stream management and manual testing

---

## Running Without Docker

Use this path when you already run Caddy on the host or want a single static binary.

### 1. Build Caddy with NUTS

The simplest way is `xcaddy`:

```bash
xcaddy build --with github.com/ideaconnect/nuts
```

This produces a `caddy` binary in the current directory with NUTS compiled in.

Or build from source:

```bash
git clone https://github.com/ideaconnect/nuts.git
cd nuts
go build -o caddy ./cmd/caddy
```

For development with the race detector:

```bash
CGO_ENABLED=1 go build -race -o caddy ./cmd/caddy
```

### 2. Start NATS with JetStream

```bash
nats-server -js -p 4222
```

### 3. Create the JetStream stream

```bash
nats stream add EVENTS \
  --subjects "events.>" \
  --storage file \
  --retention limits \
  --max-msgs 10000 \
  --max-age 24h \
  --discard old
```

### 4. Run Caddy

```bash
./caddy run --config Caddyfile
```

Verify NUTS is loaded:

```bash
./caddy list-modules | grep nuts
# expected: http.handlers.nuts
```

---

## Running With Docker

### Option A — Prebuilt image

A multi-architecture image (`amd64` / `arm64`) is published to Docker Hub at
[`idcttech/nuts`](https://hub.docker.com/r/idcttech/nuts).

```bash
docker pull idcttech/nuts:latest
```

> **Pin in production.** `:latest` is updated on every default-branch push.
> Pin a specific `idcttech/nuts:<version>` tag instead — versioned releases are
> published to [GitHub](https://github.com/ideaconnect/nuts/releases) and Docker Hub.

The image expects a Caddyfile mounted at `/app/Caddyfile` and exposes port `8080`:

```bash
docker run -d \
  -p 8080:8080 \
  -e NATS_URL=nats://host.docker.internal:4222 \
  --add-host=host.docker.internal:host-gateway \
  -v ./Caddyfile:/app/Caddyfile:ro \
  idcttech/nuts:latest
```

If NATS runs in the same Docker network, point `NATS_URL` at its service name
(e.g. `nats://nats:4222`) instead of `host.docker.internal`.

### Option B — NATS in Docker, Caddy locally

Quickly spin up just NATS while keeping a local Caddy binary:

```bash
docker run --rm -p 4222:4222 nats:2.12-alpine -js
```

### Option C — Full stack with Docker Compose

The NUTS repository ships a `docker-compose.yml` that brings up NATS (with JetStream)
and a Caddy server with NUTS, ready for development:

```bash
git clone https://github.com/ideaconnect/nuts.git
cd nuts

docker compose up -d --build      # start all services
docker compose logs -f            # tail logs
docker compose down -v            # stop and clean up
```

A production-style Compose file using the prebuilt image looks like:

```yaml
services:
  nats:
    image: nats:2.12-alpine
    command: ["--jetstream", "--store_dir=/data"]
    volumes:
      - nats-data:/data
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:8222/healthz"]
      interval: 2s
      timeout: 3s
      retries: 10

  nats-init:
    image: natsio/nats-box:0.19.0
    depends_on:
      nats:
        condition: service_healthy
    entrypoint: ["/bin/sh", "-c"]
    command:
      - |
        nats -s nats://nats:4222 stream add EVENTS \
          --subjects "events.>" --storage file --retention limits \
          --max-msgs 10000 --max-age 24h --discard old --defaults
    restart: "no"

  nuts:
    image: idcttech/nuts:latest    # pin to a version in production
    ports:
      - "8080:8080"
    volumes:
      - ./Caddyfile:/app/Caddyfile:ro
    depends_on:
      nats-init:
        condition: service_completed_successfully

volumes:
  nats-data:
```

---

## Container Environment Variables

The Caddyfile shipped in the image uses Caddy's `{$NAME:default}` substitution
for three commonly-overridden values:

| Variable | Default | Caddyfile directive |
|---|---|---|
| `NATS_URL` | `nats://localhost:4222` | `nats_url` |
| `STREAM_NAME` | `EVENTS` | `stream_name` |
| `TOPIC_PREFIX` | `events.` | `topic_prefix` |

To expose other directives (`allowed_origins`, `max_connections`, etc.) through
the environment, add matching `{$NAME:default}` placeholders yourself — NUTS
itself does not read environment variables directly.

---

## Using Make

The repository ships a Makefile for common development tasks:

```bash
make build           # Build the Caddy binary
make test            # Run all tests (unit + functional)
make test-unit       # Run unit tests with embedded NATS
make test-functional # Run BDD tests with Docker
make docker-up       # Start Docker services
make docker-down     # Stop Docker services
make clean           # Clean build artifacts
make help            # Show all available commands
```

A convenience script sets up the development environment in one step:

```bash
./scripts/setup-dev.sh
```

This starts NATS with JetStream and creates the test stream automatically.
