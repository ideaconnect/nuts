<p align="center">
  <img src="media/nuts-logo.png" alt="NUTS logo" /><br/>
  <a href="https://idct.tech/nuts">https://idct.tech/nuts</a>
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
- **Automatic Reconnection**: Built-in NATS reconnection handling; after a reconnect each stream resumes after the last message it delivered, without gaps
- **[CORS Support](#cors-and-allowed_origins)**: Configurable cross-origin resource sharing
- **Heartbeat**: Keep-alive mechanism to prevent connection timeouts
- **[Backpressure, Not Drops](#slow-clients-and-replay)**: Each stream pulls from JetStream only as fast as its client reads, so bursts and long replays wait in the stream instead of being dropped or disconnecting the client. A client that stops reading is disconnected by `write_timeout` and resumes from its last event ID. Oversized events can still be rejected by `max_event_size`.
- **[NATS Authentication](#with-nats-authentication)**: Credentials file, token, or user/password auth for the NUTS-to-NATS connection
- **NATS TLS / mTLS**: Optional `nats_tls_ca`, `nats_tls_cert`, `nats_tls_key` directives for an encrypted and mutually authenticated NATS connection
- **[Subscriber JWT Authorization](#subscriber-authentication-and-topic-authorization)**: Optional HMAC-signed JWT auth with per-topic `subscribe` claims, accepted from `Authorization: Bearer` or a configurable cookie
- **[Connection Caps](#max_connections)**: `max_connections` bounds concurrent SSE streams; rejected clients receive `429 Too Many Requests` with `Retry-After`, and browser `EventSource` clients are told to [retry](#transient-failures-and-eventsource)
- **[Per-frame Write Bounds](#write_timeout)**: `write_timeout` (default 30 s) bounds every SSE write, so a client that stopped reading cannot tie up a handler indefinitely
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
  - [`write_timeout`](#write_timeout)
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
| Go (build) | 1.26.8 (`go.mod`) | Matches the toolchain `Dockerfile` uses. |
| Caddy | 2.11.x | Embedded via `xcaddy`. Patch bumps tracked in `CHANGELOG.md`. |
| NATS server | 2.10 (required) | Each stream uses an ordered pull consumer with server-side `FilterSubjects`, which needs nats-server 2.10 or newer. **Multi-topic subscriptions need 2.14.7 or newer (2.15 recommended):** older servers, including every 2.10.x and 2.12.x release, can skip messages on one requested subject when another is purged or rolled up (nats-server#8572), and NUTS logs a warning at startup on such servers. On 2.15 and newer, raise the stream's `max_consumers`; see [Consumer limits on nats-server 2.15](#consumer-limits-on-nats-server-215). Functional matrix: see [`Makefile`](Makefile) `test-functional-matrix`. |

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
    image: nats:2.15-alpine
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
          --max-consumers 10000 \
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
   docker run -p 4222:4222 nats:2.15-alpine -js
   ```

2. **Create a JetStream stream** (using NATS CLI):
   ```bash
   # Install NATS CLI: https://github.com/nats-io/natscli
   nats stream add EVENTS \
     --subjects "events.>" \
     --max-consumers 10000 \
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
    # NATS server URL (required): nats://, tls://, ws:// or wss://, or a
    # comma-separated list of servers
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
    allowed_origins <origins...> # CORS origins as scheme://host[:port], or * (default: *)
    allowed_headers <headers...> # CORS request headers (default: Cache-Control Last-Event-ID)
    allowed_methods <methods...> # CORS methods; only GET OPTIONS are supported
    subscriber_jwt_key <secret>  # Enable HMAC JWT subscriber auth and topic claims
    subscriber_jwt_cookie <name> # Optional JWT cookie for browser EventSource clients
    heartbeat_interval <seconds> # SSE keep-alive ticker interval (0=default 30)
    reconnect_wait <seconds>     # Reconnect wait time (0=default 2)
    nats_ping_interval <seconds> # NATS ping interval; two unanswered = stale (0=default 20)
    nats_idle_heartbeat <seconds># Pull-consumer heartbeat: default 10, must be < 15
    max_reconnects <count>       # Max reconnects, 0=none, -1=infinite (default: -1)
    max_event_size <bytes>       # Max SSE event size (0=default 1 MiB, <0=unlimited)
    max_connections <count>      # Global concurrent-stream cap (default: 0 = unlimited)
    max_topics_per_subscription <count>  # Per-request topic cap (0=default 32, -1=unlimited)
    client_buffer_size <count>   # Messages prefetched from JetStream per connection (0=default 64)
    shared_subscriptions [true|false] # Share one consumer per topic set among live connections (default: off)
    write_timeout <seconds>      # Deadline for each SSE write/flush (0=default 30, -1=disabled)
    replay_max_messages <count>  # Cap replayed messages per reconnect (default: 0 = unlimited)
    replay_window <seconds>      # Time-bound replay to the last N seconds (default: 0 = all retained)
    event_id_format <format>     # Event ids: sequence (default) or sequence_time (sequence and message time)
    event_type <source> [name]   # Event names: message (default), topic, or header <name>
    payload_format <format>      # Event data: envelope (default) or raw (the NATS payload itself)
    health_path <path>           # Legacy readiness endpoint (empty/default: /healthz)
    live_path <path>             # Process liveness endpoint (empty/default: /livez)
    ready_path <path>            # NATS/stream readiness endpoint (empty/default: /readyz)
    hub_url <url>                # URL for Link header hub discovery (disabled by default)

    # Optional NATS TLS
    nats_tls_ca <path>                  # CA bundle for verifying the server (not with insecure_skip_verify)
    nats_tls_cert <path>                # Client certificate (mTLS)
    nats_tls_key <path>                 # Client key (mTLS)
    nats_tls_insecure_skip_verify [true|false] # Disable server verification (DEV ONLY; bare flag = true)
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

#### `topic_prefix`

`topic_prefix` is prepended to every requested topic, so include the trailing
dot (`events.`, `tenants.a.`). It is validated at startup, because a mistake
would silently widen every client's subscription: it may only use
`[A-Za-z0-9._-]`, may not contain the NATS wildcards `*` or `>`, may not start
with `.` or `$` (NATS system subjects), may not contain `..`, and is capped at
256 bytes.

#### Zero and negative values

Numeric directives treat `0` and negative values differently. Values not
listed as accepted are rejected when the configuration loads.

| Directive | `0` or omitted | Accepted negative value |
| --- | --- | --- |
| `max_event_size` | 1 MiB default | Any negative: no limit |
| `max_topics_per_subscription` | 32 default | `-1`: no limit |
| `max_reconnects` | Omitted: unlimited; `0`: never reconnect | `-1`: unlimited |
| `write_timeout` | 30 s default | `-1`: no deadline |
| `nats_idle_heartbeat` | 10 s default | `-1`: accepted, but the library's 5 s default applies (warning) |
| `heartbeat_interval`, `reconnect_wait`, `nats_ping_interval` | 30 s, 2 s and 20 s defaults | None |
| `client_buffer_size` | 64 default | None |
| `max_connections` | No cap | None |
| `replay_max_messages`, `replay_window` | No limit | None |
| `dispatch_timeout` | Deprecated, no effect | None |

#### `max_event_size`

Limits the size (in bytes) of a single SSE event, checked in two stages. The
raw NATS payload is checked first, before any JSON parsing, and dropped when
it alone exceeds the limit
(`nuts_messages_dropped_total{reason="raw_payload"}`). The formatted frame —
the `id:`, `event:` and `data:` lines plus the JSON envelope — is checked
next (`reason="formatted_sse_message"`). A dropped event is logged at Warn
with its topic, stream sequence and size; the client never sees it, and its
id never appears.

For example, setting `max_event_size 1000` means that if a NATS message produces an SSE frame larger than 1000 bytes once formatted, that frame is discarded. A typical overhead (id, event type, topic, timestamp) is roughly 120-150 bytes, so a 1000-byte limit leaves ~850 bytes for the raw message payload. Set `max_event_size 0` to fall back to the 1 MiB default, or a negative value to disable the limit entirely. The limit does not bound memory: messages are pulled from JetStream before they are checked (see [docs/PERFORMANCE.md](docs/PERFORMANCE.md)).

#### `max_connections`

Caps the number of concurrent SSE streams per NUTS instance. When the cap
is reached, new clients receive `429 Too Many Requests` (RFC 6585) with a
jittered `Retry-After` of 3–8 seconds, and the
`nuts_connections_rejected_total{reason="max_connections"}` counter is
incremented. Browser `EventSource` clients get a `retry:` stream instead; see
[Transient failures and EventSource](#transient-failures-and-eventsource).
Default `0` disables the cap. The `429` status distinguishes a client-side
concurrency cap from genuine `503` paths (NATS unavailable / subscription
failed / readiness probe degraded), so client-side circuit breakers can keep
retrying rather than opening the circuit on a healthy backend.

**Sizing memory.** Each connection holds at most `client_buffer_size`
messages prefetched from JetStream, plus up to 18 formatted frames on their
way to the client. A client that stops reading reaches that bound and stays
there: its stream stops pulling until it reads again or `write_timeout`
closes it. Prefetched messages are raw NATS messages, bounded by the stream's
`max_msg_size` or the NATS server's `max_payload` (1 MiB by default), not by
`max_event_size`: oversized ones are dropped only once they are read. Frames
are bounded by `max_event_size`. With defaults and 1 MiB messages a
connection can hold about 82 MiB (64 prefetched messages and 18 frames of
1 MiB); the production profile in [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)
(`client_buffer_size 8`, `max_event_size` and the stream's `max_msg_size`
64 KiB) about 2 MiB. [docs/PERFORMANCE.md](docs/PERFORMANCE.md) has the exact
formula, including `shared_subscriptions`.

See [docs/PERFORMANCE.md](docs/PERFORMANCE.md) for latency, memory, and
per-instance client-count budgets plus the load and benchmark commands used to
validate them.

#### `write_timeout`

`write_timeout <seconds>` sets a deadline for writing and flushing each SSE
frame (connected, message and heartbeat). A write that misses it means the
client stopped reading, so NUTS closes the stream with
`disconnect_reason=slow_client` and counts it in
`nuts_slow_client_disconnects_total`; the client resumes from its last event
ID. The default is 30 seconds (`0` or omitted). `-1` disables the deadline and
leaves stalled writes to Caddy and the surrounding HTTP server configuration.

A client that is merely slower than the stream is not disconnected: its
stream pulls from JetStream only as fast as the client reads.

`write_timeout` uses Go's `http.ResponseController`; if a wrapper in front of
NUTS does not support per-response write deadlines, NUTS falls back to the
normal write path.

`dispatch_timeout` is deprecated and has no effect: the pull consumer has no
queue hand-off left to time out. Setting it logs a warning; it will be removed
in the next major release.


#### `nats_ping_interval`

`nats_ping_interval <seconds>` sets how often NUTS pings the NATS server
(20 seconds by default; `0` or omitted uses the default). After two
unanswered pings the connection is stale: NUTS logs `disconnected from NATS`
with `stale connection` and reconnects. A server that stops answering without
closing the connection (packets dropped, a paused VM or container) is
therefore noticed within two to three intervals. From then on new requests
are told to retry at once. Before that, the first request to time out
reading the stream's info (2 seconds) makes NUTS ping the server itself: if
the ping goes unanswered within a second, that request and every new one are
told to retry at once, logged as `NATS server stopped answering`, until the
server answers again (`NATS server answers again`). A server that does answer
the ping is only slow, and the request goes on. Open streams stay open either
way and continue without a gap once NATS answers again. nats.go's own
default, two minutes, took four to six minutes to notice such an outage.
#### JetStream consumers

Every SSE request gets its own ordered pull consumer, named
`nuts_<random id>_<n>`. NUTS deletes it when the stream ends, and a
reconnecting client always gets a new one. The consumer also has an
**`InactiveThreshold` of 30 seconds**, so the server reaps it on its own if
NUTS loses its NATS connection before it can delete it. When the stream sets a
shorter `consumer_limits.inactive_threshold`, NUTS uses that instead, because
the server refuses consumers that ask for more. When the NUTS handler shuts
down or reloads, it waits up to 3 seconds for its streams to delete their
consumers before closing the NATS connection.

The consumer recreates itself from the last delivered sequence after a
delivery gap, a NATS reconnect, or missed heartbeats (`nats_idle_heartbeat`),
for example when the server reaps it during an outage. The client notices
nothing. If recreation keeps failing for about 75 seconds, the stream closes
with `disconnect_reason=consumer_unrecoverable` and the client reconnects with
its last event ID.

#### `shared_subscriptions`

By default every SSE connection owns a JetStream consumer, so a message sent
to a topic with N subscribers is pulled from JetStream, sent over NUTS' NATS
connection and formatted N times. With many subscribers and large messages
this saturates the link: nats-server allows 64 MiB of pending data per client
connection by default, and a burst past it disconnects NUTS. The streams
recover without losing messages, but slowly.

`shared_subscriptions` (off by default) lets connections that are caught up
with the live stream share one consumer per topic set. Each message is pulled
and formatted once and handed to every connection's queue:

- A connection with history to replay reads it from its own consumer, then
  joins the shared subscription. The shared subscription keeps its latest
  frames (up to 1024 frames or 4 MiB), so the hand-off has no gap.
- A connection that falls more than `client_buffer_size` frames behind moves
  back to its own consumer, starting after the last message it was given,
  and rejoins once it has caught up. It is not disconnected.
- The shared consumer recovers from reconnects and deletions like any other.
  If it cannot, its connections move to their own consumers.

With 1000 connections and a burst of 4 × 64 KiB messages, delivery took
0.25 s with sharing and about 14 s without it, spent recovering from
slow-consumer disconnects. Sharing also keeps the stream's consumer count
near the number of topic sets instead of the number of connections. Watch
`nuts_shared_transitions_total{transition="fell_behind"}`: a steady rate means
clients cannot keep up with the shared subscription; raise
`client_buffer_size`. Messages dropped by `max_event_size` or as control
messages are counted once per shared subscription, not once per connection.

#### Consumer limits on nats-server 2.15

From nats-server 2.15, a stream accepts at most 1000 consumers unless
`max_consumers` is set on the stream or the account. Since each SSE
connection owns one consumer, set a **positive** `max_consumers` sized for
peak concurrent connections across all NUTS replicas (for example
`nats stream edit EVENTS --max-consumers 10000`), or set
`default_max_consumers: -1` in the server's JetStream limits. A stream or
account value of `-1` does not lift the default.

When the limit is reached, NUTS rejects the request as retryable (`503` with
`Retry-After`, or a `retry:` stream for `EventSource` clients), logs
`disconnect_reason=stream_consumer_limit` and counts
`nuts_connections_rejected_total{reason="stream_consumer_limit"}`. At startup,
NUTS warns when the stream's `max_consumers` is below `max_connections`.

#### `replay_max_messages` and `replay_window`

Both guard against replay storms — when a client reconnects with an old
`last-id` (or `Last-Event-ID`), NUTS may need to deliver a large retained
backlog before catching up to the live stream. On a stream with long retention
this can be tens of thousands of events.

- `replay_max_messages <count>` closes the SSE connection after the
  configured number of historical replay events have been delivered. The
  client reconnects with a fresher `Last-Event-ID` and continues normally.
  The `nuts_replay_cap_reached_total` counter is incremented each time the cap
  fires. Only history counts: NUTS reads the stream's last sequence when the
  request arrives, and messages published after that are live traffic that
  never trips the cap. When the stream info cannot be read, the backlog
  pending behind the first replayed message is counted instead. A message
  without JetStream metadata counts against the cap during a replay.
- `replay_window <seconds>` time-bounds replay to recent retained messages.
  If the requested cursor is older than the window, NUTS starts replay at
  `now - window`; if the cursor is still inside the window, NUTS preserves
  exact `last-id + 1` cursor semantics. When the stream info cannot be read,
  or the cursor is ahead of the stream (a recreated or restored stream), the
  replay also starts at `now - window`. Replayed messages older than the
  window are dropped and counted as
  `nuts_messages_dropped_total{reason="replay_window"}`; live messages are
  never filtered. To date the resume point, NUTS reads that one message; on
  a stream with `allow_direct` (`nats stream add --allow-direct`) this uses
  Direct Get instead of the JetStream API, which returns the whole message
  base64-encoded.

Both default to `0` (unlimited / all retained) to preserve the original
behaviour. They can be combined: `replay_window` bounds the time range,
`replay_max_messages` bounds the count within that range.

For public or multi-tenant deployments, treat the default `0` values as a
compatibility mode rather than a production recommendation for large retained
streams. Pick bounds that match the largest replay you are willing to serve to
one client, then size JetStream retention, `max_connections`, and edge
rate-limits around that budget.

#### `event_type`

Every message is sent as `event: message` by default, with its topic in the
JSON data, so a page handles all of them in one `message` listener.
`event_type` names each event after something of the message instead:

- `event_type topic`: the topic, without `topic_prefix`. A page subscribed to
  several topics can add one listener per topic:
  `events.addEventListener('orders', …)`.
- `event_type header <name>`: the value of that header of the NATS message,
  for example `event_type header Event-Type` with publishers that set
  `Event-Type: order_created`. Header names are case-sensitive in NATS.

A name must be 1 to 100 characters of `A-Z a-z 0-9 . _ : -`, and it must not
be `connected`, `reset`, `open` or `error`: NUTS and EventSource use those
names themselves, and an event named `error` would reach the page's
`onerror` handler as if the connection had failed. Otherwise, and for a
message without the header, the event is named `message`. The JSON data is
the same in every case. The event name counts towards `max_event_size`.

#### `payload_format`

By default an event's data is the JSON envelope
`{"topic":…,"payload":…,"time":…}`. `payload_format raw` sends the NATS
payload itself instead: a page that only wants the payload need not unwrap
it, plain-text or CSV payloads arrive as they are, and the envelope's 120 to
150 bytes no longer count towards `max_event_size`.

```
event: message
data: first line of the payload
data: second line of the payload
id: 12345
```

A payload with line breaks is sent as several `data:` lines, which
EventSource joins with `\n` again. SSE data is UTF-8 text and a carriage
return ends a line, so a payload that is not valid UTF-8 or holds a carriage
return cannot be sent: it is dropped, logged, and counted as
`nuts_messages_dropped_total{reason="raw_not_text"}`. The envelope's topic
and time are gone in raw mode; pair it with
[`event_type topic`](#event_type) to tell topics apart. The `connected` and
`reset` events keep their JSON data.

#### `event_id_format`

`event_id_format sequence_time` makes each event's id carry the stored time
of its message as well as its stream sequence: `id: 42-1790513389663997004`,
the time in Unix nanoseconds. The default, `sequence`, keeps the bare
sequence (`id: 42`).

A bare sequence cannot tell which stream it came from. When a stream is
deleted and created again, or restored from a backup, it gives the sequences
after its start or its backup's end to other messages. A client that was away
at the time comes back with a cursor from the old stream. Once the new stream
has passed that cursor, NUTS resumes after it, and the client misses the new
stream's messages up to there.

With `sequence_time`, NUTS reads the message the stream holds at the cursor's
sequence. If its time differs, the stream is not the one the cursor came
from, and the client replays the retained stream from its start (bounded by
`replay_max_messages` and `replay_window`), logged as
`replay_fallback_reason="cursor from another stream"`. A cursor whose message
the stream no longer holds, trimmed by its limits or deleted, resumes after
it as before. Connected clients do not need it: NUTS resets their cursors
when it notices the change
([TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md#streams-close-after-the-stream-was-recreated-or-restored)).

Both forms are accepted as `Last-Event-ID` and `?last-id=` whatever the
setting, so it can be changed while clients are connected. Turn it on only if
your clients treat event ids as opaque strings: code that parses them as
integers needs to take the part before the `-`. Each resume from a timed
cursor costs one extra message read (Direct Get on a stream with
`allow_direct`).

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

Write each origin exactly as browsers send it in the `Origin` header:
`scheme://host[:port]` in lowercase, with no path or trailing slash, as
separate arguments. NUTS compares origins literally, so it rejects entries
that could never match: an empty entry, a comma-joined list, a path, or
uppercase letters.

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

Allowed filters use NATS subject syntax for compound values — exact topics
such as `orders.created`, single-token wildcards such as `orders.*`, and tail
wildcards such as `tenant-a.>`. A bare `>` matches every topic on the route
(standard NATS semantics). A bare `*` is also accepted with the same "every
topic" meaning as a NUTS-only convenience alias — note that this is more
permissive than NATS itself, where a bare `*` matches only single-token
subjects. Missing, expired, badly signed, or unauthorized tokens are rejected
before subscription.

Only the HMAC algorithms `HS256`, `HS384` and `HS512` are accepted. Tokens
declaring `none`, RSA, ECDSA or EdDSA are rejected, which rules out the
`alg=none` and "RSA public key used as HMAC secret" confusion attacks, so
tokens from an identity provider that signs with RS256 or ES256 need a
translating layer in front of NUTS. A missing, malformed, expired or badly
signed token gets `401` with `WWW-Authenticate: Bearer realm="nuts"`; a valid
token whose `subscribe` claim does not cover a requested topic gets
`403 Forbidden topic`. Keys shorter than 32 bytes still verify, but log a
startup warning with `key_length` and `recommended_minimum`, because HS256
assumes at least a 32-byte secret.
The `exp` and `nbf` time claims are optional; when present they are enforced.
NUTS requires integer epoch seconds for `exp` and `nbf` — RFC 7519 §2 permits
non-integer NumericDate, but a fractional value (for example `1777392000.5`)
is rejected as malformed rather than truncated. All major JWT issuers emit
integer epoch seconds, so this is documented for completeness rather than as
a real interop hazard.
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

NUTS registers the following metrics with the metrics registry of the Caddy config it is loaded in, so they appear on Caddy's `/metrics` endpoint when the [admin API](https://caddyserver.com/docs/caddyfile/options#admin) or a [metrics handler](https://caddyserver.com/docs/caddyfile/directives/metrics) is enabled. (They are also registered on Prometheus' default registry, for programs that embed NUTS and serve that one.)

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
| `nuts_messages_dropped_total{reason}` | Counter (labeled) | Messages not delivered to a client. `reason` is one of `raw_payload` (inbound NATS payload exceeded `max_event_size`), `formatted_sse_message` (SSE envelope after JSON wrap exceeded `max_event_size`), `replay_window` (a replayed message older than `replay_window`), `control_message` (a subject delete marker or schedule definition; see [Server control messages](#server-control-messages)) or `raw_not_text` (a payload [`payload_format raw`](#payload_format) cannot send: not UTF-8 text, or holding a carriage return). |
| `nuts_wildcard_filter_drops_total` | Counter | Deprecated, always 0: the pre-NATS-2.10 wildcard fallback was removed. |
| `nuts_slow_client_disconnects_total` | Counter | Clients disconnected because a write missed `write_timeout` (the client stopped reading) |
| `nuts_replay_requests_total` | Counter | Connections requesting message replay |
| `nuts_replay_fallbacks_total` | Counter | Replay streams that started in a fallback mode (requested sequence below retention, older than `replay_window`, ahead of the stream, or stream info unavailable), counted once the consumer exists |
| `nuts_subscription_errors_total` | Counter | Stream requests refused because a topic is outside the stream or the JetStream consumer could not be created (consumer-limit refusals are counted in `nuts_connections_rejected_total`) |
| `nuts_connections_rejected_total{reason}` | Counter (labeled) | SSE connections rejected before streaming started. `reason` is one of `max_connections`, `stream_consumer_limit`, `auth_missing_token`, `auth_invalid_token`, `auth_topic_forbidden`. |
| `nuts_replay_cap_reached_total` | Counter | Replaying SSE connections closed after `replay_max_messages` was reached |
| `nuts_dispatch_timeout_total` | Counter | Deprecated, always 0: `dispatch_timeout` has no effect. |
| `nuts_nats_async_errors_total{kind}` | Counter (labeled) | Asynchronous NATS client errors observed by the registered ErrorHandler. `kind` is one of `slow_consumer`, `timeout`, `connection_state`, `consumer_invalidated`, `other`. `consumer_invalidated` only applied to the legacy push subscriptions and stays `0`: consumer health is handled by the ordered consumer itself and counted in `nuts_consumer_invalidated_total`. `slow_consumer` should stay `0` too, since each stream pulls at most `client_buffer_size` messages at a time. |
| `nuts_consumer_invalidated_total{reason}` | Counter (labeled) | JetStream consumer failures under live streams. `reason` is `recreated` (the consumer recovered after a gap, a NATS reconnect or missed heartbeats; the client noticed nothing), `unrecoverable` (recreation kept failing and the stream closed with `disconnect_reason=consumer_unrecoverable`), `stream_recreated` (the JetStream stream was deleted and created again, so the stream closed and the client reconnects from the start of the new one) or `stream_rewound` (the stream's sequence went back, as after a restore that kept its creation time, so the stream closed and the client replays it from its start). |
| `nuts_write_disconnects_total{site}` | Counter (labeled) | SSE streams terminated by a response-writer write error (typically the `write_timeout` deadline firing). `site` is one of `connected`, `message`, `heartbeat`. |
| `nuts_readiness_failures_total{cause}` | Counter (labeled) | `/readyz` probe responses that returned 503 because a dependency was degraded. `cause` is one of `nats_disconnected`, `jetstream_missing`, `stream_info_error`. |
| `nuts_shared_subscriptions` | Gauge | Shared subscriptions (one JetStream consumer per topic set) with `shared_subscriptions` on. |
| `nuts_shared_transitions_total{transition}` | Counter (labeled) | Connections moving onto or off shared subscriptions: `joined` (caught up and attached), `fell_behind` (its queue overflowed; it continues on its own consumer), `shared_failed` (the shared consumer could not be recreated). |
| `nuts_nats_connection_events_total{event}` | Counter (labeled) | NATS connection-state transitions reported by the registered Disconnect/Reconnect/Closed/LameDuckMode handlers. `event` is one of `disconnect`, `reconnect`, `closed`, `lame_duck` (the server announced it is shutting down). `closed` is not counted when NUTS closes the connection itself on shutdown or reload. Use the `reconnect` series to alert on broker flapping (see [ops/prometheus-alerts.yml](ops/prometheus-alerts.yml)). |

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
  --max-consumers 10000 \
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
| `--max-consumers` | Peak concurrent SSE connections across all NUTS instances | Each SSE connection holds one consumer. nats-server 2.15 allows 1000 per stream unless this is set |
| `--max-msg-size` | Your largest message, at most `max_event_size` | Bounds the messages each connection prefetches (see [Sizing memory](#max_connections)); a producer that exceeds it gets an error instead of a message NUTS would drop |

### Example Streams

**Chat application:**
```bash
nats stream add CHAT \
  --subjects "chat.>" \
  --max-consumers 10000 \
  --storage file \
  --max-msgs-per-subject 1000 \
  --max-age 7d
```

**Metrics/Dashboard:**
```bash
nats stream add METRICS \
  --subjects "metrics.>" \
  --max-consumers 10000 \
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

// Resume from a stored ID after a page reload. EventSource resends this URL,
// ?last-id= included, on every auto-reconnect, but its own Last-Event-ID
// header is fresher and takes precedence, so reconnects keep moving forward.
const lastId = localStorage.getItem('lastEventId');
const url = lastId
    ? `/events?topic=notifications&last-id=${encodeURIComponent(lastId)}`
    : '/events?topic=notifications';
const events = new EventSource(url);

// Store the last event ID for replay after a page reload. The handshake and
// every message carry one, and a reset (the stream was recreated) sets it
// back to 0.
function remember(e) {
    if (e.lastEventId) {
        localStorage.setItem('lastEventId', e.lastEventId);
    }
}

// Handle connection
events.addEventListener('connected', (e) => {
    const { topics } = JSON.parse(e.data);
    console.log('Connected to:', topics);
    remember(e);
});

// Handle messages
events.addEventListener('message', (e) => {
    const { topic, payload, time } = JSON.parse(e.data);
    console.log(`[${topic}] at ${time}:`, payload);
    remember(e);
});

events.addEventListener('reset', remember);

// Handle errors and reconnect with replay
events.onerror = (e) => {
    console.error('SSE error:', e);
    // EventSource reconnects on its own and sends Last-Event-ID, including
    // after NUTS answers a transient failure with a retry delay.
    // Custom clients should reconnect with the most recent event ID.
};
```

### Transient failures and EventSource

A browser `EventSource` gives up for good when a connection or reconnection is
answered with anything other than a `200` event stream: it closes and never
retries. NUTS therefore answers transient failures differently for clients
that send `Accept: text/event-stream`, as every `EventSource` does:

| Failure | `EventSource` clients | Other clients |
| --- | --- | --- |
| JetStream not available (NATS outage, handler shutting down) | `200` retry stream | `503` + `Retry-After` |
| `max_connections` reached | `200` retry stream | `429` + `Retry-After` |
| Stream consumer limit reached | `200` retry stream | `503` + `Retry-After` |
| Consumer could not be created | `200` retry stream | `503` + `Retry-After` |

The retry stream holds a comment naming the reason and a `retry:` delay of
2.5–7.5 seconds (jittered so rejected clients do not return together), then
closes:

```
: JetStream not available
retry: 4213

```

The browser waits that long, reconnects and sends its `Last-Event-ID`, so
nothing is lost. `Retry-After` carries the same delay in whole seconds. While
the NATS connection is down, NUTS answers at once rather than waiting for
JetStream calls to time out.
Client errors (`400`, `401`, `403`) and topics outside the stream
(`503 Failed to subscribe to requested topics`) are not retryable: they will
not succeed on their own.

### Slow Clients And Replay

Each stream pulls from JetStream only as fast as its client reads. A client
that falls behind during a burst or a long replay keeps its connection; the
backlog waits in JetStream until the client catches up.

A client that stops reading altogether is disconnected once a write misses
`write_timeout` (30 s by default). It can then resume from the last delivered
SSE `id` using either:

- The browser-managed `Last-Event-ID` header
- The explicit `?last-id=` query parameter for custom clients

This means the delivery policy is effectively:

- No per-client message loss in the live stream path, including across NATS
  reconnects and consumer loss on the server
- Clients that stop reading are disconnected and resume from their last ID
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
- Without `last-id`, only new messages are delivered. The `connected` event
  still carries an `id` (the stream position the subscription starts after),
  so a client that disconnects before its first message resumes without a gap
- Standard `EventSource` reconnects can use the `Last-Event-ID` header automatically
- When a client is disconnected, reconnecting with the last delivered event ID resumes from that point instead of losing messages silently

**Source precedence and malformed-cursor handling:**

When both `?last-id=` and a valid `Last-Event-ID` header are present on
the same request, the **header wins**. An `EventSource` keeps the URL it was
created with, so every auto-reconnect resends the original `?last-id=` next
to the browser's fresher `Last-Event-ID`; letting the query win would replay
from the same old cursor on every reconnect, and with `replay_max_messages`
set the client would never get past the first N messages.

Malformed values are handled asymmetrically on purpose:

- `?last-id=` malformed or above the cursor cap → **400 Bad Request**,
  even when a valid header is also present. An explicit query value is a
  client choice; failing fast surfaces the bug.
- `Last-Event-ID:` malformed or above the cursor cap → **logged at
  Warn and ignored**. The stream resumes from a valid `?last-id=` when one
  is present, otherwise it starts at the live position like a request
  without a cursor. A browser auto-resumes on reconnect with whatever it
  last received; returning 400 would make it loop forever on a single bad
  value. The Warn log carries the offending value so an operator can
  diagnose the producer.

The highest accepted cursor is `2^64 − 3`. NUTS adds `1` to the cursor to
get the JetStream start sequence, and `2^64 − 1` is reserved as a "no
sequence" sentinel, so `2^64 − 2` is rejected too: its start sequence would
land on the sentinel. No real stream comes near these values, but an explicit
cap matters for fuzz tests and for clients that synthesize cursors. A cursor
may also carry a time, `<sequence>-<unix ns>` (see
[`event_id_format`](#event_id_format)); the time must be a positive integer.
A sequence or a time longer than 20 digits (the length of the largest
`uint64`) is rejected before parsing, as a guard against multi-megabyte
numeric strings: it is answered with `"Invalid last-id value: too long"`
(400) for `?last-id=` and logged as `"ignoring oversized Last-Event-ID
header"` for the header.

### Message Format

Messages are sent as SSE events with the following format:

```
event: message
data: {"topic":"my-topic","payload":{"your":"data"},"time":"2024-01-01T12:00:00Z"}
id: 12345
```

The event is named `message` unless [`event_type`](#event_type) names it after its topic or a header, and its data is the JSON envelope unless [`payload_format raw`](#payload_format) sends the payload itself. The `id` field contains the JetStream sequence number, which can be used with `last-id` or `Last-Event-ID` for replay. With [`event_id_format sequence_time`](#event_id_format) it also carries the stored time of the message: `id: 12345-1790513389663997004`. It is the last line of each event: SSE allows the fields in any order, and some clients (for example `@microsoft/fetch-event-source`) record an id as soon as they read it, so with the id last a frame cut off mid-write cannot make them resume after a message they never received.

The first frame of every stream is the handshake event:

```
event: connected
data: {"topics":["my-topic"]}
id: 12344
```

Its `id` is the stream position the subscription starts after: the stream's
last sequence for a request without a cursor, or the requested cursor for a
replay. Treat it like any other event ID. It is omitted when the stream
starts in a fallback replay mode, in which case the client keeps its previous
cursor.

When the JetStream stream is deleted and created again, or restored from a
backup, under an open stream, NUTS ends the stream with a `reset` event:

```
: stream_recreated
retry: 4180
event: reset
data: {"reason":"stream_recreated"}
id: 0
```

The new or restored stream numbers the messages after its start or its
backup's end anew, so the event sets the client's last event ID to `0`, and
EventSource replays the stream from its start after the `retry` delay. The
`reason` is `stream_recreated` or `stream_rewound`. A page that keeps the last event ID across
page reloads must take it from `reset` events too, as the
[JavaScript example](#javascript-eventsource) does. See
[TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md#streams-close-after-the-stream-was-recreated-or-restored).

#### Response headers

Every stream response carries these headers; proxies in between must keep
them:

| Header | Why |
| --- | --- |
| `Content-Type: text/event-stream` | Makes browsers parse the response as SSE. Without it `EventSource` fails the connection. |
| `Cache-Control: no-cache` | Stops browsers and shared caches from caching the stream. |
| `Connection: keep-alive` | Keeps HTTP/1.1 proxies from closing the connection between events. |
| `X-Accel-Buffering: no` | Tells nginx, and proxies that honour it, not to buffer the response. A proxy that buffers delays events until its buffer fills. |

`hub_url` adds `Link: <url>; rel="nuts"`, and allow-listed origins get the
CORS headers described above.

#### Server control messages

Some messages in a stream are written by the server for its own bookkeeping
rather than published by an application. NUTS does not forward them:

- subject delete markers, stored when a subject's last message is removed on
  a stream with `subject_delete_marker_ttl` (header `Nats-Marker-Reason`);
- message schedule definitions (header `Nats-Schedule`). The messages a
  schedule produces are delivered as usual.

They are counted as `nuts_messages_dropped_total{reason="control_message"}`.
Their sequence numbers never appear as event IDs, so IDs can skip them.
Apart from these two headers, NUTS does not look at message headers.

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
nats stream add CHAT --subjects "chat.>" --max-consumers 10000 --storage file --max-age 7d
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
nats stream add METRICS --subjects "metrics.>" --max-consumers 10000 --storage memory --max-age 1h
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

- Go 1.26.8+ (matches the `go` directive in [`go.mod`](go.mod))
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
