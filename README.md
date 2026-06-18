<p align="center">
  <img src="media/nuts-logo.png" alt="NUTS logo" /><br/>
  <a href="https://nuts.idct.tech">https://nuts.idct.tech</a>
</p>

# 🥜 NUTS - NATS to SSE for Caddy

A Caddy Server module that bridges NATS.io JetStream messages to Server-Sent Events (SSE), inspired by [Mercure.rocks](https://mercure.rocks).

[![codecov](https://codecov.io/gh/ideaconnect/nuts/graph/badge.svg?token=Z09PBL02A6)](https://codecov.io/gh/ideaconnect/nuts)
[![CI](https://github.com/ideaconnect/nuts/actions/workflows/ci.yml/badge.svg)](https://github.com/ideaconnect/nuts/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/ideaconnect/nuts)](go.mod)
[![Go Report Card](https://goreportcard.com/badge/github.com/ideaconnect/nuts)](https://goreportcard.com/report/github.com/ideaconnect/nuts)
[![Latest release](https://img.shields.io/github/v/release/ideaconnect/nuts)](https://github.com/ideaconnect/nuts/releases/latest)
[![Docker Pulls](https://img.shields.io/docker/pulls/idcttech/nuts)](https://hub.docker.com/r/idcttech/nuts)
[![License](https://img.shields.io/badge/license-BSD%204--Clause-blue.svg)](LICENSE)
[![GitHub Sponsors](https://img.shields.io/github/sponsors/ideaconnect?style=flat&logo=github)](https://github.com/sponsors/ideaconnect)
[![Buy Me a Coffee](https://img.shields.io/badge/Buy%20Me%20a%20Coffee-ffdd00?logo=buy-me-a-coffee&logoColor=black)](https://buymeacoffee.com/idct)

## Features

- **Real-time Updates**: Stream NATS messages to web browsers via SSE/EventSource
- **[JetStream Persistence](#jetstream-setup)**: Messages are persisted in NATS JetStream for replay
- **[Message Replay](#message-replay-with-last-id-or-last-event-id)**: Clients can reconnect and replay messages from a specific ID using `?last-id=` or the standard `Last-Event-ID` header. Replay can be bounded by `replay_max_messages` or `replay_window` when configured.
- **Multiple Topics**: Subscribe to multiple NATS subjects simultaneously
- **Automatic Reconnection**: Built-in NATS reconnection handling
- **[CORS Support](#cors-and-allowed_origins)**: Configurable cross-origin resource sharing
- **Heartbeat**: Keep-alive mechanism to prevent connection timeouts
- **[No Silent Drops For Slow Clients](#slow-clients-and-replay)**: When a client falls behind, NUTS disconnects that SSE session before dropping queued messages so the client can resume from the last delivered event ID. Oversized events can still be rejected by `max_event_size`.
- **[NATS Authentication](#with-nats-authentication)**: Credentials file, token, or user/password auth for the NUTS-to-NATS connection
- **NATS TLS / mTLS**: Optional `nats_tls_ca`, `nats_tls_cert`, `nats_tls_key` directives for an encrypted and mutually authenticated NATS connection
- **[Subscriber JWT Authorization](#subscriber-authentication-and-topic-authorization)**: Optional HMAC-signed JWT auth with per-topic `subscribe` claims, accepted from `Authorization: Bearer` or a configurable cookie
- **[Connection Caps](#max_connections)**: `max_connections` bounds concurrent SSE streams; rejected clients receive `429 Too Many Requests` with `Retry-After`
- **[Per-frame Write Bounds](#dispatch_timeout-and-write_timeout)**: Optional `dispatch_timeout` and `write_timeout` keep slow downstream connections from tying up a handler indefinitely
- **Topic Prefixing**: Optional prefix for all NATS subscriptions
- **[Prometheus Metrics](#prometheus-metrics)**: Built-in `nuts_*` counters and gauges (active connections, messages delivered, slow-client disconnects, replay stats)
- **[Liveness And Readiness Checks](#liveness-and-readiness-checks)**: `/livez`, `/readyz`, and legacy `/healthz` probe endpoints
- **[Hub Discovery](#hub-discovery)**: Optional `Link` header with `rel="nuts"` for automatic hub detection

## Table of Contents

- [Features](#features)
- [Compatibility](#compatibility)
- [Versioning policy](#versioning-policy)
- [Installation](#installation)
  - [Using xcaddy (Recommended)](#using-xcaddy-recommended)
  - [Building from Source](#building-from-source)
  - [Using the Docker Image](#using-the-docker-image)
  - [Docker Compose](#docker-compose)
  - [Environment variables](#environment-variables)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
  - [Caddyfile Syntax](#caddyfile-syntax)
  - [Path-shorthand and `route`](#path-shorthand-and-route)
  - [`max_event_size`](#max_event_size)
  - [`max_connections`](#max_connections)
  - [`dispatch_timeout` and `write_timeout`](#dispatch_timeout-and-write_timeout)
  - [`replay_max_messages` and `replay_window`](#replay_max_messages-and-replay_window)
  - [CORS and `allowed_origins`](#cors-and-allowed_origins)
  - [Subscriber authentication and topic authorization](#subscriber-authentication-and-topic-authorization)
  - [Liveness And Readiness Checks](#liveness-and-readiness-checks)
  - [Prometheus Metrics](#prometheus-metrics)
  - [Hub Discovery](#hub-discovery)
- [JetStream Setup](#jetstream-setup)
- [Client Usage](#client-usage)
  - [JavaScript EventSource](#javascript-eventsource)
  - [Slow Clients And Replay](#slow-clients-and-replay)
  - [Message Replay with `last-id` or `Last-Event-ID`](#message-replay-with-last-id-or-last-event-id)
  - [Message Format](#message-format)
- [Example Scenarios](#example-scenarios)
- [Inspired by Mercure](#inspired-by-mercure)
- [Development](#development)
- [Roadmap](#roadmap)
- [License](#license)
- [Contributing](#contributing)

## Further Documentation

In-depth reference and operations material lives in the [`docs/`](docs/)
directory:

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — system, replay flow, and
  ownership boundaries.
- [docs/CONFIGURATION.md](docs/CONFIGURATION.md) — full directive matrix with
  defaults, JSON field names, valid values, and operational notes.
- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) — copy-paste Compose, Kubernetes,
  and reverse-proxy-protected deployment examples.
- [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md) — common EventSource,
  CORS, replay, Docker, and functional-test issues.
- [docs/OPERATIONS.md](docs/OPERATIONS.md) — probes, metrics, structured log
  fields, and incident runbooks.
- [docs/PERFORMANCE.md](docs/PERFORMANCE.md) — load-test coverage and
  production performance budgets.
- [docs/RELEASE.md](docs/RELEASE.md) — release validation, SBOMs, vulnerability
  scans, and signing policy.
- [docs/ROADMAP.md](docs/ROADMAP.md) — completed milestones and planned
  features.
- [docs/STRUCTURE.md](docs/STRUCTURE.md) — Go source file map.
- [docs/mutation/](docs/mutation/) — mutation testing baseline, per-file
  MSI targets, accepted-survivors log, and run reports.
- [docs/MERCURE.md](docs/MERCURE.md) — short note on Mercure, which inspired
  NUTS.
- [docs/SECURITY.md](docs/SECURITY.md) — security model, auth boundaries,
  reverse-proxy patterns, and the supported disclosure process.

## Compatibility

| Component | Minimum tested | Notes |
| --- | --- | --- |
| Go (build) | 1.26.4 (`go.mod`) | Matches the toolchain `Dockerfile` uses. |
| Caddy | 2.11.x | Embedded via `xcaddy`. Patch bumps tracked in `CHANGELOG.md`. |
| NATS server | 2.9-alpine | Functional matrix covers `nats:2.9-alpine` and `nats:2.12-alpine` (see [`Makefile`](Makefile) `test-functional-matrix`). The in-process unit suite uses the embedded `nats-server/v2` library pinned in `go.mod`. **Recommendation:** run NATS ≥ 2.10. Pre-2.10 servers lack `ConsumerFilterSubjects`, so multi-topic subscriptions fall back to wildcard-subscribe with client-side filtering — observe `nuts_wildcard_filter_drops_total` to size the impact. |

## Versioning policy

NUTS follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

- **MAJOR** — incompatible changes to the Caddyfile directive set, JSON
  config schema, Prometheus metric names or label sets, exported Go API,
  or the HTTP response shape (status codes, headers, SSE framing).
- **MINOR** — additive features, new directives, new metrics, new optional
  behaviour gated on configuration. Existing configs continue to load
  with unchanged semantics.
- **PATCH** — bug fixes, security fixes, performance improvements with no
  observable contract change.

**Pre-1.0 (0.x) carve-out.** Per [SemVer §4](https://semver.org/spec/v2.0.0.html#spec-item-4), the public API is not considered stable until a 1.0 release. While NUTS is on the 0.x line, MINOR releases MAY include changes that would otherwise require a MAJOR bump under the rules above — for example the
`nuts_messages_dropped_total{reason}` labelled-counter migration that
shipped in the next 0.x release after 0.3. Every such break is
explicitly called out in `CHANGELOG.md` under the **Changed** heading
with the phrase "Breaking" so operators reading the changelog can spot
it without diffing the metric set.

Deprecations land in a MINOR release with a warning at provision time
and a note in `CHANGELOG.md`; removals land no sooner than the next
MAJOR release. The `:latest` Docker tag follows `main`; production
deployments should pin a specific tag (`vX.Y.Z`).

## Installation

### Using xcaddy (Recommended)

```bash
xcaddy build --with github.com/ideaconnect/nuts
```

### Building from Source

```bash
# Clone the repository
git clone https://github.com/ideaconnect/nuts.git
cd nuts

# Build custom Caddy with the module
go build -o caddy ./cmd/caddy
```

### Using the Docker Image

A pre-built multi-architecture image (`amd64` / `arm64`) is published to Docker Hub:

```bash
docker pull idcttech/nuts:latest
```

> **Pin in production.** `:latest` is updated on every default-branch push, so pulling it twice on the same host can yield different binaries. Once a versioned release is cut, pin to the concrete tag — `idcttech/nuts:<version>` — so rollouts are reproducible and rollbacks are possible.

The image expects a Caddyfile mounted at `/app/Caddyfile` and exposes port `8080`:

```bash
docker run -d \
  -p 8080:8080 \
  -e NATS_URL=nats://host.docker.internal:4222 \
  --add-host=host.docker.internal:host-gateway \
  -v ./Caddyfile:/app/Caddyfile:ro \
  idcttech/nuts:latest
```

Set `NATS_URL` to a NATS server reachable from inside the container. If NATS
runs in the same Compose network, use its service name instead, for example
`nats://nats:4222`.

#### Docker Compose

A typical production-like stack with NATS and NUTS:

```yaml
services:
  nats:
    image: nats:2.12-alpine
    # -m 8222 enables the HTTP monitoring endpoint that the healthcheck
    # below probes. Without it the healthcheck never passes and any
    # depends_on: { condition: service_healthy } gates block forever.
    command: ["--jetstream", "--store_dir=/data", "-m", "8222"]
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
          --subjects "events.>" \
          --storage file \
          --retention limits \
          --max-msgs 10000 \
          --max-age 24h \
          --discard old \
          --defaults
    restart: "no"

  nuts:
    image: idcttech/nuts:latest  # pin to a concrete version in production
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

With a Caddyfile like:

```caddyfile
:8080 {
    route /events* {
        uri strip_prefix /events
        nuts {
            nats_url  nats://nats:4222
            stream_name EVENTS
            topic_prefix events.
        }
    }
}
```

#### Environment variables

The `Caddyfile` and `Caddyfile.test` at the repository root use Caddy's
`{$NAME:default}` substitution for the three variables below so the same
file works in the test harness, locally, and in a container. The
`docker-compose.yml` at the repository root and the one under `example/`
populate them via the `environment:` block on the `nuts` service; the
compose file under `example_docker/` points at the published Docker
image and leaves the values as their defaults.

| Variable | Default (if unset) | Caddyfile directive |
|---|---|---|
| `NATS_URL` | `nats://localhost:4222` | `nats_url` |
| `STREAM_NAME` | `EVENTS` | `stream_name` |
| `TOPIC_PREFIX` | `events.` | `topic_prefix` |

Equivalent Caddyfile snippet:

```caddyfile
nuts {
    nats_url     {$NATS_URL:nats://localhost:4222}
    stream_name  {$STREAM_NAME:EVENTS}
    topic_prefix {$TOPIC_PREFIX:events.}
}
```

Only these three are consumed by the shipped Caddyfile. To expose other
directives (`allowed_origins`, `max_connections`, etc.) through the
environment, add matching `{$NAME:default}` placeholders yourself — NUTS
itself does not read environment variables directly.

## Quick Start

1. **Start NATS server with JetStream enabled**:
   ```bash
   docker run -p 4222:4222 nats:2.12-alpine -js
   ```

2. **Create a JetStream stream** (using NATS CLI):
   ```bash
   # Install NATS CLI: https://github.com/nats-io/natscli
   nats stream add EVENTS \
     --subjects "events.>" \
     --storage file \
     --retention limits \
     --max-msgs 10000 \
     --max-age 24h \
     --discard old
   ```

3. **Create a Caddyfile**:
   ```caddyfile
   :8080 {
       route /events* {
           uri strip_prefix /events
           nuts {
               nats_url nats://localhost:4222
               stream_name EVENTS
               topic_prefix events.
           }
       }
   }
   ```

   `uri strip_prefix /events` ensures the path-shorthand example below
   (`new EventSource('/events/my-topic')`) sees `/my-topic` inside the
   handler; without it the handler would subscribe to
   `events.events.my-topic`. See
   [Path-shorthand and `route`](#path-shorthand-and-route) for details.

4. **Run Caddy**:
   ```bash
   ./caddy run
   ```

5. **Connect from JavaScript**:
   ```javascript
   const events = new EventSource('/events?topic=my-topic');

   events.addEventListener('message', (e) => {
       const data = JSON.parse(e.data);
       console.log('Received:', data, 'ID:', e.lastEventId);
   });
   ```

6. **Publish a message** (using NATS CLI):
   ```bash
   nats pub events.my-topic '{"hello": "world"}'
   ```

## Configuration

For the complete directive matrix, including JSON field names, defaults,
validation rules, and production notes, see
[docs/CONFIGURATION.md](docs/CONFIGURATION.md).

### Caddyfile Syntax

```caddyfile
nuts {
    # NATS server URL (required)
    nats_url <url>

    # JetStream stream name (required)
    stream_name <name>

    # NATS authentication (choose exactly one; user/password must be set together)
    nats_credentials <path>      # Path to .creds file
    nats_token <token>           # Token auth
    nats_user <username>         # User/password auth
    nats_password <password>

    # Optional settings
    topic_prefix <prefix>        # Prefix for all subscriptions
    allowed_origins <origins...> # CORS origins (default: *)
    allowed_headers <headers...> # CORS request headers (default: Cache-Control Last-Event-ID)
    allowed_methods <methods...> # CORS methods; only GET OPTIONS are supported
    subscriber_jwt_key <secret>  # Enable HMAC JWT subscriber auth and topic claims
    subscriber_jwt_cookie <name> # Optional JWT cookie for browser EventSource clients
    heartbeat_interval <seconds> # Heartbeat interval (default: 30)
    reconnect_wait <seconds>     # Reconnect wait time (default: 2)
    max_reconnects <count>       # Max reconnects, 0=none, -1=infinite (default: -1)
    max_event_size <bytes>       # Max SSE event size (0=default 1 MiB, <0=unlimited)
    max_connections <count>      # Global concurrent-stream cap (default: 0 = unlimited)
    max_topics_per_subscription <count>  # Per-request topic cap (0=default 32, <0=unlimited)
    client_buffer_size <count>   # Per-connection send buffer (0=default 64)
    dispatch_timeout <seconds>   # Cap slow-client signal wait in NATS callbacks (default: 0 = disabled)
    write_timeout <seconds>      # Cap each SSE write/flush when supported (default: 0 = disabled)
    replay_max_messages <count>  # Cap replayed messages per reconnect (default: 0 = unlimited)
    replay_window <seconds>      # Time-bound replay to the last N seconds (default: 0 = all retained)
    health_path <path>           # Legacy readiness endpoint (empty/default: /healthz)
    live_path <path>             # Process liveness endpoint (empty/default: /livez)
    ready_path <path>            # NATS/stream readiness endpoint (empty/default: /readyz)
    hub_url <url>                # URL for Link header hub discovery (disabled by default)

    # Optional NATS TLS
    nats_tls_ca <path>                  # CA bundle for verifying the server
    nats_tls_cert <path>                # Client certificate (mTLS)
    nats_tls_key <path>                 # Client key (mTLS)
    nats_tls_insecure_skip_verify       # Disable server verification (DEV ONLY)
}
```

#### Path-shorthand and `route`

NUTS derives the NATS subject from `?topic=` (repeatable) **or** from the
request path when the query is absent. Forward slashes in the path are
translated to `.` so `/orders/new` becomes the NATS subject `orders.new`
(plus any `topic_prefix`).

If you mount NUTS behind a route matcher, strip the matcher's prefix before
the handler sees the request:

```caddyfile
route /events* {
    uri strip_prefix /events
    nuts { ... }
}
```

#### `max_event_size`

Limits the total size (in bytes) of a single SSE event frame — including the `id:`, `event:`, and `data:` lines plus the JSON-encoded payload. Any event that exceeds the limit is silently dropped and a warning is logged. The client never sees it.

For example, setting `max_event_size 1000` means that if a NATS message produces an SSE frame larger than 1000 bytes once formatted, that frame is discarded. A typical overhead (id, event type, topic, timestamp) is roughly 120-150 bytes, so a 1000-byte limit leaves ~850 bytes for the raw message payload. Set `max_event_size 0` to fall back to the 1 MiB default, or a negative value to disable the limit entirely.

#### `max_connections`

Caps the number of concurrent SSE streams per NUTS instance. When the cap
is reached, new clients receive `429 Too Many Requests` (RFC 6585) with
`Retry-After: 5` and the `nuts_connections_rejected_total{reason="max_connections"}`
counter is incremented. Default `0` disables the cap. The `429` status
distinguishes a client-side concurrency cap from genuine `503` paths
(NATS unavailable / subscription failed / readiness probe degraded), so
client-side circuit breakers can keep retrying rather than opening the
circuit on a healthy backend.

**Sizing memory.** The buffered-message footprint is bounded by
`max_connections × client_buffer_size × max_event_size`. With defaults
(`client_buffer_size 64`, `max_event_size 1048576`) each connection can hold
up to 64 MiB of queued payloads, so `max_connections 1000` implies a ~64 GiB
worst-case ceiling before slow-client disconnects kick in. Lower
`client_buffer_size` or `max_event_size` if that ceiling is unacceptable;
`max_event_size -1` (unlimited) removes the per-event bound entirely and
makes the ceiling unbounded.

See [docs/PERFORMANCE.md](docs/PERFORMANCE.md) for latency, memory, and
per-instance client-count budgets plus the load and benchmark commands used to
validate them.

#### `dispatch_timeout` and `write_timeout`

These optional guards keep slow or blocked downstream connections from tying up
NUTS indefinitely:

- `dispatch_timeout <seconds>` caps how long a NATS callback waits to notify
  the streaming loop after the client's queue is already full. `0` preserves
  the original unbounded wait.
- `write_timeout <seconds>` sets a per-frame write deadline before each SSE
  connected, message, and heartbeat frame is written and flushed. `0` leaves
  write deadlines entirely to Caddy and the surrounding HTTP server config.

`write_timeout` uses Go's `http.ResponseController`; if a wrapper in front of
NUTS does not support per-response write deadlines, NUTS falls back to the
normal write path. Caddy server-level timeouts and proxy buffering policy still
matter, but this directive gives the handler its own protection for supported
HTTP stacks.

#### Ephemeral consumer hygiene

Every SSE request opens its own ephemeral JetStream consumer with an
explicit **`InactiveThreshold` of 30 seconds**. After the SSE subscription
ends (client disconnect, NUTS shutdown, write error), nats-server retains
the consumer for 30 s in case the client reconnects, then reaps the
server-side state. This protects against reconnect-storm accumulation
that would build up with nats-server's 5 s default.

The threshold is not user-configurable today; if 30 s is unsuitable for
your deployment, open an issue with the scenario you need to support.

#### `replay_max_messages` and `replay_window`

Both guard against replay storms — when a client reconnects with an old
`last-id` (or `Last-Event-ID`), NUTS may need to deliver a large retained
backlog before catching up to the live stream. On a stream with long retention
this can be tens of thousands of events.

- `replay_max_messages <count>` closes the SSE connection after the
  configured number of historical replay events have been delivered. The
  client reconnects with a fresher `Last-Event-ID` and continues normally.
  The `nuts_replay_cap_reached_total` counter is incremented each time the cap
  fires.
- `replay_window <seconds>` time-bounds replay to recent retained messages.
  If the requested cursor is older than the window, NUTS starts replay at
  `now - window`; if the cursor is still inside the window, NUTS preserves
  exact `last-id + 1` cursor semantics.

Both default to `0` (unlimited / all retained) to preserve the original
behaviour. They can be combined: `replay_window` bounds the time range,
`replay_max_messages` bounds the count within that range.

For public or multi-tenant deployments, treat the default `0` values as a
compatibility mode rather than a production recommendation for large retained
streams. Pick bounds that match the largest replay you are willing to serve to
one client, then size JetStream retention, `max_connections`, and edge
rate-limits around that budget.

#### CORS and `allowed_origins`

NUTS never emits a literal `Access-Control-Allow-Origin: *`; it echoes the
request `Origin` header whenever the incoming origin is allow-listed. A
`Vary: Origin` header is added so shared caches don't leak one origin's
response to another.

`Access-Control-Allow-Credentials: true` is only advertised when the request
`Origin` is explicitly listed in `allowed_origins`. If `allowed_origins`
contains `*`, the request is accepted but credentials are **not** advertised —
browsers will reject credentialed cross-origin streams. Native browser
`EventSource` can send cookies with `withCredentials: true`, but it cannot set
custom `Authorization` headers; use cookies, a reverse proxy, or a custom SSE
client for header-based subscriber auth. To support credentialed CORS, replace
`*` with the explicit origins that should be trusted:

Pick **one** of the two forms below (a second `allowed_origins` directive
inside the same `nuts { }` block overwrites the first):

Wildcard — anonymous CORS only, no cookies / `Authorization` headers:

```caddyfile
allowed_origins *
```

Explicit — credentials allowed for these origins:

```caddyfile
allowed_origins https://app.example.com https://admin.example.com
```

`allowed_methods` is intentionally limited to `GET` and `OPTIONS`, because
NUTS only serves SSE streams and CORS preflight requests. Subscriber
authentication and topic authorization are separate from CORS: CORS controls
which browser origins may read responses, not who is allowed to subscribe.

#### Subscriber authentication and topic authorization

The `nats_credentials`, `nats_token`, and `nats_user` / `nats_password`
directives authenticate the NUTS process to NATS. Subscriber access is separate
and can be handled either by Caddy/upstream policy or by NUTS' optional
first-party JWT check.

Set `subscriber_jwt_key` to require an HMAC-signed JWT before NUTS creates a
JetStream consumer. Tokens are accepted from `Authorization: Bearer <jwt>` or,
when `subscriber_jwt_cookie` is configured, from that cookie. The token must
include a `subscribe` claim listing allowed topic filters before
`topic_prefix` is applied:

```json
{
  "sub": "user-123",
  "exp": 1777392000,
  "subscribe": ["orders.*", "tenant-a.>"]
}
```

Allowed filters use NATS-style tokens: exact topics such as `orders.created`,
single-token wildcards such as `orders.*`, tail wildcards such as
`tenant-a.>`, or `*` / `>` to allow every topic on that route. Missing,
expired, badly signed, or unauthorized tokens are rejected before subscription.
The `exp` and `nbf` time claims are optional; when present they are enforced.
For public or browser-facing routes, include `exp` and keep tokens compact:
NUTS rejects compact JWTs over 8 KiB, decoded JWT segments over 6 KiB, and
`subscribe` claims with more than 128 filters.

Example:

```caddyfile
:8080 {
  route /events* {
    uri strip_prefix /events
    nuts {
      nats_url nats://nats:4222
      stream_name EVENTS
      topic_prefix events.
      allowed_origins https://app.example.com
      allowed_headers Cache-Control Last-Event-ID Authorization
      subscriber_jwt_key {$SUBSCRIBER_JWT_KEY}
      subscriber_jwt_cookie nuts_session
    }
  }
}
```

Native browser `EventSource` cannot set custom `Authorization` headers, so use
same-site requests or a configured cookie for browser clients. Custom clients
can use the Bearer header directly.

Protect a route with Caddy `basic_auth` when simple operator-controlled access
is enough. Generate the password hash with `caddy hash-password` and keep the
route prefix strip before `nuts`:

```caddyfile
:8080 {
  route /events* {
    basic_auth {
      alice <bcrypt-hash-from-caddy-hash-password>
    }
    uri strip_prefix /events
    nuts {
      nats_url nats://nats:4222
      stream_name EVENTS
      topic_prefix events.
      allowed_origins https://app.example.com
    }
  }
}
```

For application-owned sessions, put an auth service or reverse proxy in front
of NUTS. The auth layer should reject unauthenticated requests before `nuts`
creates a JetStream consumer:

```caddyfile
:8080 {
  route /events* {
    forward_auth https://auth.internal {
      uri /verify
      copy_headers X-User X-Tenant
    }
    uri strip_prefix /events
    nuts {
      nats_url nats://nats:4222
      stream_name EVENTS
      topic_prefix events.
      allowed_origins https://app.example.com
    }
  }
}
```

Use separate route blocks, streams, or prefixes for tenant isolation. A single
public route with only a broad `topic_prefix` is not tenant authorization:

```caddyfile
:8080 {
  route /tenant-a/events* {
    uri strip_prefix /tenant-a/events
    nuts {
      nats_url nats://nats:4222
      stream_name TENANT_A_EVENTS
      topic_prefix tenants.a.
      allowed_origins https://tenant-a.example.com
      max_connections 500
      replay_max_messages 1000
      replay_window 300
    }
  }

  route /tenant-b/events* {
    uri strip_prefix /tenant-b/events
    nuts {
      nats_url nats://nats:4222
      stream_name TENANT_B_EVENTS
      topic_prefix tenants.b.
      allowed_origins https://tenant-b.example.com
      max_connections 500
      replay_max_messages 1000
      replay_window 300
    }
  }
}
```

Apply rate limits at the edge, CDN, WAF, Caddy plugin, or reverse proxy that
already knows the client IP or user identity. Useful buckets are connection
attempts to the SSE route, repeated `400` responses from invalid topics,
replay-heavy requests with very old `last-id` values, and repeated `503`
responses from connection caps or subscription failures. `max_connections`
protects concurrent streams, while rate limiting protects request churn.

### Liveness And Readiness Checks

NUTS exposes separate probe paths within the configured route:

- `live_path` (default `/livez`) returns process liveness only and does not
  check NATS. Use this for Kubernetes liveness probes.
- `ready_path` (default `/readyz`) checks the NATS connection and configured
  JetStream stream. Use this for readiness probes and load balancer target
  health.
- `health_path` (default `/healthz`) remains a backward-compatible
  readiness-style check with the same NATS and stream checks as `ready_path`.

```bash
curl -i http://localhost:8080/events/livez
curl -i http://localhost:8080/events/readyz
```

**Live (200):**
```json
{"status":"ok"}
```

The readiness and legacy health endpoints return NATS connectivity and stream
availability:

**Ready (200):**
```json
{"status":"ok","nats":"connected","stream":"available"}
```

**Not ready (503):**
```json
{"status":"degraded","nats":"disconnected","stream":"unavailable"}
```

Operational runbooks and Kubernetes probe examples are in
[docs/OPERATIONS.md](docs/OPERATIONS.md).

> **Probe path matching:** the configured probe paths match either as an
> exact path or as a path suffix on a `/`-segment boundary. So with the
> default `/healthz`, both `/healthz` and `/events/healthz` are routed
> to the probe handler. A topic-shorthand path that happens to end with
> the configured probe path (e.g. a topic named `orders/healthz` reached
> via path-shorthand) will be intercepted by the probe handler instead of
> opening an SSE stream. If you use path-shorthand with topic names that
> could collide, configure unique probe paths via `health_path`,
> `live_path`, and `ready_path`.

### Prometheus Metrics

NUTS registers the following metrics via `promauto`, which appear automatically on Caddy's `/metrics` endpoint when the [admin API](https://caddyserver.com/docs/caddyfile/options#admin) or a [metrics handler](https://caddyserver.com/docs/caddyfile/directives/metrics) is enabled.

To expose metrics, add a `metrics` handler to your Caddyfile:

```caddyfile
:8080 {
    route /metrics {
        metrics
    }
    route /events* {
        uri strip_prefix /events
        nuts {
            nats_url  nats://localhost:4222
            stream_name EVENTS
            topic_prefix events.
        }
    }
}
```

Then scrape `http://localhost:8080/metrics` from Prometheus. Available metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `nuts_active_connections` | Gauge | Currently connected SSE clients |
| `nuts_messages_delivered_total` | Counter | SSE message events successfully written |
| `nuts_messages_dropped_total{reason}` | Counter (labeled) | Messages dropped during SSE formatting. `reason` is one of `raw_payload` (inbound NATS payload exceeded `max_event_size`) or `formatted_sse_message` (SSE envelope after JSON wrap exceeded `max_event_size`). |
| `nuts_wildcard_filter_drops_total` | Counter | Messages silently filtered client-side by the multi-topic wildcard fallback (subjects not requested by the client). Non-zero means a server older than NATS 2.10 is wasting bandwidth on unrequested subjects. |
| `nuts_slow_client_disconnects_total` | Counter | Clients disconnected due to slow consumption |
| `nuts_replay_requests_total` | Counter | Connections requesting message replay |
| `nuts_replay_fallbacks_total` | Counter | Replay requests that used fallback replay (requested sequence was purged or older than `replay_window`) |
| `nuts_subscription_errors_total` | Counter | Failed JetStream subscription attempts |
| `nuts_connections_rejected_total{reason}` | Counter (labeled) | SSE connections rejected before streaming started. `reason` is one of `max_connections`, `auth_missing_token`, `auth_invalid_token`, `auth_topic_forbidden`. |
| `nuts_replay_cap_reached_total` | Counter | Replaying SSE connections closed after `replay_max_messages` was reached |
| `nuts_dispatch_timeout_total` | Counter | NATS callbacks that timed out signalling a slow SSE client (set when `dispatch_timeout` fires before the SSE loop observes the signal) |
| `nuts_nats_async_errors_total{kind}` | Counter (labeled) | Asynchronous NATS client errors observed by the registered ErrorHandler. `kind` is one of `slow_consumer`, `timeout`, `connection_state`, `other`. `slow_consumer` indicates the nats.go per-subscription buffer overflowed and messages were silently dropped. |
| `nuts_write_disconnects_total{site}` | Counter (labeled) | SSE streams terminated by a response-writer write error (typically the `write_timeout` deadline firing). `site` is one of `connected`, `message`, `heartbeat`. |
| `nuts_readiness_failures_total{cause}` | Counter (labeled) | `/readyz` probe responses that returned 503 because a dependency was degraded. `cause` is one of `nats_disconnected`, `jetstream_missing`, `stream_info_error`. |
| `nuts_nats_connection_events_total{event}` | Counter (labeled) | NATS connection-state transitions reported by the registered Disconnect/Reconnect/Closed handlers. `event` is one of `disconnect`, `reconnect`, `closed`. Use the `reconnect` series to alert on broker flapping (see [ops/prometheus-alerts.yml](ops/prometheus-alerts.yml)). |

Example alert rules and a Grafana dashboard are available in
[ops/prometheus-alerts.yml](ops/prometheus-alerts.yml) and
[ops/grafana-dashboard.json](ops/grafana-dashboard.json).

Streaming logs include structured fields such as `topics`, `subjects`,
`subject_label`, `replay_mode`, `replay_start_sequence`,
`replay_fallback_reason`, and `disconnect_reason`; see
[docs/OPERATIONS.md](docs/OPERATIONS.md) for incident-response guidance.

### Hub Discovery

When `hub_url` is configured, every SSE response includes a `Link` header:

```
Link: <https://example.com/events>; rel="nuts"
```

This lets clients discover the event hub URL from the SSE endpoint. If an upstream API wants clients to discover the hub from normal API responses, that API or a reverse proxy must also emit the same `Link` header. A client can then inspect the header:

```javascript
const resp = await fetch('/api/resource'); // API/proxy must include the Link header
const link = resp.headers.get('Link');
// Parse link header to extract the hub URL, then:
const events = new EventSource(hubUrl + '?topic=updates');
```

To enable hub discovery, add the `hub_url` directive:

```caddyfile
nuts {
    nats_url nats://localhost:4222
    stream_name EVENTS
    hub_url https://example.com/events
}
```

## JetStream Setup

NUTS requires a pre-configured JetStream stream. The stream must be created before starting Caddy.

### Creating a Stream

Using the NATS CLI:

```bash
# Basic stream for events
nats stream add EVENTS \
  --subjects "events.>" \
  --storage file \
  --retention limits \
  --max-msgs 10000 \
  --max-age 24h \
  --discard old

# Or interactively
nats stream add
```

### Stream Configuration Options

| Option | Recommended | Description |
|--------|-------------|-------------|
| `--subjects` | Match your `topic_prefix` + `>` | Subjects the stream captures |
| `--storage` | `file` | Use `file` for persistence, `memory` for speed |
| `--retention` | `limits` | How messages are retained |
| `--max-msgs` | `10000` | Maximum messages to keep |
| `--max-age` | `24h` | Maximum age of messages |
| `--discard` | `old` | Discard oldest messages when limit reached |

### Example Streams

**Chat application:**
```bash
nats stream add CHAT \
  --subjects "chat.>" \
  --storage file \
  --max-msgs-per-subject 1000 \
  --max-age 7d
```

**Metrics/Dashboard:**
```bash
nats stream add METRICS \
  --subjects "metrics.>" \
  --storage memory \
  --max-msgs 5000 \
  --max-age 1h
```

## Client Usage

### JavaScript EventSource

```javascript
// Subscribe to a single topic
const events = new EventSource('/events?topic=notifications');

// Subscribe to multiple topics
const events = new EventSource('/events?topic=notifications&topic=updates');

// Using path-based topic
const events = new EventSource('/events/my-topic');

// Replay messages from a specific ID (e.g., after reconnection)
const lastId = localStorage.getItem('lastEventId') || '';
const events = new EventSource(`/events?topic=notifications&last-id=${lastId}`);

// Handle connection
events.addEventListener('connected', (e) => {
    const { topics } = JSON.parse(e.data);
    console.log('Connected to:', topics);
});

// Handle messages and track last ID for replay
events.addEventListener('message', (e) => {
    const { topic, payload, time } = JSON.parse(e.data);
    console.log(`[${topic}] at ${time}:`, payload);

    // Store last event ID for reconnection replay
    if (e.lastEventId) {
        localStorage.setItem('lastEventId', e.lastEventId);
    }
});

// Handle errors and reconnect with replay
events.onerror = (e) => {
    console.error('SSE error:', e);
    // EventSource will auto-reconnect and send Last-Event-ID automatically.
    // Custom clients should reconnect with the most recent event ID.
};
```

### Slow Clients And Replay

NUTS does not silently drop queued messages merely because an active SSE client is slow.

If a client cannot read fast enough and its per-connection queue fills, NUTS closes that SSE connection instead of discarding queued messages. A reconnecting client can then resume from the last delivered SSE `id` using either:

- The browser-managed `Last-Event-ID` header
- The explicit `?last-id=` query parameter for custom clients

This means the delivery policy is effectively:

- No silent per-client message loss in the live stream path due to slow consumers
- Slow clients must reconnect to continue
- `dispatch_timeout` and `write_timeout` can bound callback waits and blocked
  SSE writes when the downstream connection or proxy stalls
- Replay depends on the requested sequence still being retained in JetStream
- Oversized raw payloads or formatted SSE events are rejected according to `max_event_size`

### Message Replay with `last-id` or `Last-Event-ID`

The `last-id` query parameter and standard `Last-Event-ID` header allow clients to replay messages from a specific point:

```javascript
// Get the last received message ID
const lastId = '12345';

// Reconnect and get all messages after that ID
const events = new EventSource(`/events?topic=updates&last-id=${lastId}`);
```

**Behavior:**
- Messages with sequence numbers greater than `last-id` will be delivered
- If the requested sequence no longer exists (expired/deleted), NUTS falls back to retained replay
- **Replay storm caveat**: old cursors can trigger a large retained backlog. Design your stream retention policy (max age, max messages) accordingly, and cap replay with `replay_max_messages` or `replay_window` for public or multi-tenant routes.
- Without `last-id`, only new messages are delivered
- Standard `EventSource` reconnects can use the `Last-Event-ID` header automatically
- When a slow client is disconnected, reconnecting with the last delivered event ID resumes from that point instead of losing messages silently

**Source precedence and malformed-cursor handling:**

When both `?last-id=` and the `Last-Event-ID` header are present on the
same request, the **query parameter wins** — clients explicitly setting
`?last-id=` override whatever the browser auto-attaches on EventSource
reconnect.

Malformed values are handled asymmetrically on purpose:

- `?last-id=` malformed or above the cursor cap → **400 Bad Request**.
  An explicit query value is a client choice; failing fast surfaces the
  bug.
- `Last-Event-ID:` malformed or above the cursor cap → **logged at
  Warn, stream resumes with `DeliverNew`**. A browser auto-resumes on
  reconnect with whatever it last received; returning 400 would make
  it loop forever on a single bad value. The Warn log carries the
  offending value so an operator can diagnose the producer.

### Message Format

Messages are sent as SSE events with the following format:

```
id: 12345
event: message
data: {"topic":"my-topic","payload":{"your":"data"},"time":"2024-01-01T12:00:00Z"}
```

The `id` field contains the JetStream sequence number, which can be used with `last-id` or `Last-Event-ID` for replay.

## Example Scenarios

### Chat Application

```caddyfile
:8080 {
    route /chat/* {
        nuts {
            nats_url nats://localhost:4222
            stream_name CHAT
            topic_prefix chat.
            allowed_origins https://chat.example.com
        }
    }
}
```

```bash
# Create the stream first
nats stream add CHAT --subjects "chat.>" --storage file --max-age 7d
```

```javascript
// Client subscribes to a room
const room = 'room-123';
const events = new EventSource(`/chat/messages?topic=${room}`);
```

### Real-time Dashboard

```caddyfile
:8080 {
    route /dashboard/events {
        nuts {
            nats_url nats://localhost:4222
            stream_name METRICS
            topic_prefix metrics.
            heartbeat_interval 15
        }
    }
}
```

```bash
# Create the stream first
nats stream add METRICS --subjects "metrics.>" --storage memory --max-age 1h
```

### With NATS Authentication

These settings secure the backend NATS connection used by NUTS. They are not
browser subscriber credentials.

```caddyfile
:8080 {
    route /secure/events {
        nuts {
            nats_url nats://nats.example.com:4222
            stream_name EVENTS
            nats_credentials /etc/nats/user.creds
        }
    }
}
```

## Inspired by Mercure

NUTS was inspired by [Mercure.rocks](https://mercure.rocks); we're grateful for
the groundwork they laid in this space and we respect their work. See
[docs/MERCURE.md](docs/MERCURE.md) for a short note on the inspiration.

## Development

### Prerequisites

- Go 1.26.4+ (matches the `go` directive in [`go.mod`](go.mod))
- Docker (for running NATS server)
- [NATS CLI](https://github.com/nats-io/natscli) (optional, for manual testing)

### Quick Setup

```bash
# Start NATS server with JetStream and create test stream
./scripts/setup-dev.sh

# Or manually with Docker Compose
make docker-up
```

### Running Tests

#### Unit Tests

Unit tests use an embedded NATS server, so no external dependencies are required:

```bash
# Run unit tests
go test -v -timeout 120s .

# Run specific test
go test -v -run TestHandler_ServeHTTP_Integration .
```

#### Performance Confidence

Performance confidence tests also use an embedded NATS server. They cover
concurrent SSE clients, replay-load behavior, slow-reader disconnects,
goroutine cleanup, large-payload memory growth, and hot-path benchmarks:

```bash
make test-performance
```

The current budgets and raw benchmark commands are documented in
[docs/PERFORMANCE.md](docs/PERFORMANCE.md).

#### Functional/BDD Tests

Functional tests use [Godog](https://github.com/cucumber/godog) (Cucumber for Go) with Gherkin syntax.
They require Docker services to be running:

```bash
# Using Make (recommended)
make test-functional

# Or step by step:
docker compose up -d --build
make wait-functional-stack
cd functional_test && go test -v -timeout 120s ./...
docker compose down -v
```

The BDD tests are defined in `features/sse_streaming.feature` using Gherkin syntax:

```gherkin
Feature: SSE Streaming with JetStream
  Scenario: Connect to SSE endpoint and receive messages
    Given I am connected to SSE endpoint "/events?topic=notifications"
    When I publish message '{"alert": "test"}' to subject "events.notifications"
    Then I should receive an SSE event with topic "notifications"
    And the event should have an ID
```

#### All Tests

```bash
# Run both unit and functional tests
make test
```

#### Mutation Testing

NUTS uses [gremlins](https://github.com/go-gremlins/gremlins) to measure
**test strength** in addition to coverage. Coverage tells you a line was
touched; mutation testing tells you a regression on that line would be
caught.

```bash
# One-time install of the pinned gremlins binary into $GOPATH/bin.
make mutate-tools

# Run mutation testing on the whole module (brings the Docker stack up).
make mutate

# Run scoped to a single file or directory (much faster).
make mutate-pkg PKG=auth.go
```

Reports land in [`docs/mutation/runs/`](docs/mutation/runs/) as JSON. The
weekly [`mutation.yml`](.github/workflows/mutation.yml) GitHub Action
runs the full module on Sunday 03:00 UTC and fails the run if the
Mutation Score Indicator (MSI) drops by more than 2 percentage points
versus the prior week.

Per-PR enforcement is documented in [CONTRIBUTING.md § Mutation
testing](CONTRIBUTING.md#mutation-testing): contributors run
`make mutate-pkg` on changed security-critical files and report the MSI
in the PR description. Per-file targets and the survivor-handling policy
are in [docs/mutation/targets.md](docs/mutation/targets.md).

Current state (2026-05-21):

- Test efficacy (MSI): **100%**
- Mutation coverage: **99.60%**
- 501 of 503 mutants killed; 2 documented accepted gaps in
  [docs/mutation/equivalents.md](docs/mutation/equivalents.md).

See [docs/mutation/final-report.md](docs/mutation/final-report.md) for
the end-of-initiative summary.

### Building

```bash
# Build custom Caddy with the module
go build -o caddy ./cmd/caddy

# Build with race detector (requires CGO)
CGO_ENABLED=1 go build -race -o caddy ./cmd/caddy

# Format code
go fmt ./...

# Run the pinned linter container
make lint
```

### Docker Compose

The root `docker-compose.yml` spins up the test environment (NATS + NUTS built from source). The `example/` and `example_docker/` directories each have their own `docker-compose.yml` for the interactive demo:

```bash
# Test environment
docker compose up -d --build

# Interactive demo (built from source)
cd example && ./start.sh

# Interactive demo (pre-built Docker image)
cd example_docker && ./start.sh

# View logs / stop
docker compose logs -f
docker compose down -v
```

### Makefile Commands

```bash
make build              # Build the Caddy binary
make test               # Run all tests (unit + functional)
make test-unit          # Run unit tests with embedded NATS
make test-functional    # Run BDD tests with Docker
make mutate-tools       # Install the pinned gremlins binary
make mutate             # Run mutation testing on the whole module
make mutate-pkg PKG=… # Run mutation testing scoped to one file/dir
make docker-up          # Start Docker services
make docker-down        # Stop Docker services
make clean              # Clean build artifacts
make help               # Show all available commands
```

## Roadmap

See [docs/ROADMAP.md](docs/ROADMAP.md) for completed milestones and planned
features, including subscription lifecycle events, an HTTP publish endpoint,
and more.

## License

BSD 4-Clause License - see [LICENSE](LICENSE) file for details.

## Contributing

Contributions of all kinds are welcome and appreciated! Whether you're fixing a typo, reporting a bug, suggesting a feature, or submitting a pull request — every bit helps make NUTS better.

Here are some ways you can get involved:

- **Report bugs** — Found something broken? [Open an issue](https://github.com/ideaconnect/nuts/issues) with steps to reproduce.
- **Suggest features** — Have an idea for an improvement? Start a discussion or file an issue — we'd love to hear it.
- **Submit pull requests** — Code contributions are always welcome. Feel free to pick up an open issue or propose your own change.
- **Ask questions** — Not sure how something works? Open an issue and ask. There are no silly questions.
- **Share feedback** — If you're using NUTS in a project, let us know how it's going. Your experience helps guide development.

When submitting a pull request, please:

1. Keep changes focused and minimal.
2. Add or update tests when behavior changes.
3. Run `make test` to verify both unit and functional tests pass.
4. Run `go vet ./...` and `go mod tidy && git diff --exit-code go.mod go.sum`.
5. Follow the existing code style (`go fmt ./...`).

---

<p align="center">
  <img src="media/idct-logo.png" alt="IDCT logo" /><br/>
  Created by <a href="https://idct.tech">IDCT</a> Bartosz Pachołek
</p>
