---
layout: docs
title: Usage
description: "How to use NUTS — full Caddyfile / JSON configuration reference, JetStream setup, EventSource client code, message replay, auth, metrics, and example scenarios"
permalink: /docs/usage/
---

## Configuration

NUTS is configured through the Caddyfile or Caddy's JSON config.

### Caddyfile Syntax — Full Reference

```caddyfile
nuts {
    # NATS server URL (required): nats://, tls://, ws:// or wss://,
    # or a comma-separated list of servers
    nats_url <url>

    # JetStream stream name (required)
    stream_name <name>

    # NATS authentication (choose exactly one; user/password must be set together)
    nats_credentials <path>             # Path to .creds file
    nats_token <token>                  # Token auth
    nats_user <username>                # User/password auth
    nats_password <password>

    # Optional NATS TLS / mTLS
    nats_tls_ca <path>                  # CA bundle for verifying the server (not with insecure_skip_verify)
    nats_tls_cert <path>                # Client certificate (mTLS)
    nats_tls_key <path>                 # Client key (mTLS)
    nats_tls_insecure_skip_verify       # Disable server verification (DEV ONLY)

    # Subscriber auth (HMAC-signed JWT, optional)
    subscriber_jwt_key <secret>         # Enable JWT subscriber auth and topic claims
    subscriber_jwt_cookie <name>        # Cookie name for browser EventSource clients

    # CORS
    allowed_origins <origins...>        # scheme://host[:port], or *. Default: *
    allowed_headers <headers...>        # Default: Cache-Control Last-Event-ID
    allowed_methods <methods...>        # Only GET / OPTIONS are supported

    # Routing & topic shape
    topic_prefix <prefix>               # Prefix for all subscriptions
    max_topics_per_subscription <count> # Per-request topic cap (0=default 32, -1=unlimited)

    # Streaming behaviour
    heartbeat_interval <seconds>        # SSE keep-alive interval (0=default 30)
    reconnect_wait <seconds>            # NATS reconnect wait (0=default 2)
    max_reconnects <count>              # Max NATS reconnects, 0=none, -1=infinite (default: -1)
    nats_idle_heartbeat <seconds>       # Pull-consumer heartbeat (0=default 10, must be < 15)

    # Per-event / per-connection limits
    max_event_size <bytes>              # Max SSE frame size (0=default 1 MiB, <0=unlimited)
    max_connections <count>             # Global concurrent-stream cap (default: 0 = unlimited)
    client_buffer_size <count>          # Messages prefetched from JetStream per connection (0=default 64)
    shared_subscriptions [true|false]   # Share one consumer per topic set among live connections (default: off)
    write_timeout <seconds>             # Deadline for each SSE write/flush (0=default 30, -1=disabled)

    # Replay bounds (for catch-up after reconnect)
    replay_max_messages <count>         # Cap replayed messages per reconnect (default: 0 = unlimited)
    replay_window <seconds>             # Time-bound replay window (default: 0 = all retained)

    # Health probes
    live_path  <path>                   # Liveness probe (default: /livez)
    ready_path <path>                   # Readiness probe (default: /readyz)
    health_path <path>                  # Legacy combined probe (default: /healthz)

    # Hub discovery
    hub_url <url>                       # URL emitted in the Link header (rel="nuts")
}
```

For exhaustive defaults, validation rules, and JSON field names, see
[`docs/CONFIGURATION.md`](https://github.com/ideaconnect/nuts/blob/main/docs/CONFIGURATION.md)
in the NUTS repository.

### JSON Configuration

```json
{
    "handler": "nuts",
    "nats_url": "nats://localhost:4222",
    "stream_name": "EVENTS",
    "topic_prefix": "events.",
    "allowed_origins": ["https://example.com"],
    "heartbeat_interval": 30,
    "reconnect_wait": 2,
    "max_reconnects": -1,
    "max_event_size": 1048576,
    "max_connections": 1000,
    "replay_max_messages": 1000,
    "replay_window": 300
}
```

### Path Shorthand and `route`

NUTS derives the NATS subject from `?topic=` (repeatable) **or** from the
request path when the query is absent. Forward slashes in the path are
translated to `.` so `/orders/new` becomes the NATS subject `orders.new`
(plus any `topic_prefix`).

When mounting NUTS behind a route matcher, strip the matcher's prefix:

```caddyfile
route /events* {
    uri strip_prefix /events
    nuts { ... }
}
```

Without `uri strip_prefix /events`, a request for `/events/my-topic` would
subscribe to `events.events.my-topic`.

### `max_event_size`

Limits the size of a single SSE event, checked twice: the raw NATS payload
first (`reason="raw_payload"`), then the formatted frame with its `id:`,
`event:` and `data:` lines and JSON envelope (`reason="formatted_sse_message"`).
Oversized events are dropped, counted in `nuts_messages_dropped_total` and
logged as warnings with their stream sequence.

Typical SSE overhead (id, event type, topic, timestamp) is ~120–150 bytes, so a
1000-byte cap leaves ~850 bytes for the raw payload. Use `0` for the 1 MiB
default, or a negative value to disable the limit entirely.

### `max_connections`

Caps the number of concurrent SSE streams per NUTS instance. When the cap is
reached, new clients receive `429 Too Many Requests` (RFC 6585) with a
jittered `Retry-After` of 3–8 seconds, and
`nuts_connections_rejected_total{reason="max_connections"}` increments.
Browser `EventSource` clients get a `200` stream with a `retry:` delay
instead, so they keep reconnecting. A client-side concurrency cap is
deliberately distinct from the `503` returned when NATS or JetStream is
genuinely unavailable.

Each connection prefetches up to `client_buffer_size` messages (64 by
default) from JetStream, and holds one JetStream consumer, so keep
`max_connections` across all instances within the stream's `max_consumers`
(1000 by default from nats-server 2.15).

### `write_timeout`

The deadline for each SSE write and flush, 30 seconds by default. A client
whose writes stop completing is disconnected with
`disconnect_reason=slow_client` and resumes from its last event ID; a client
that merely reads slowly is not, because NUTS pulls from JetStream only as fast
as it reads. `-1` leaves write deadlines to Caddy's server config.
`dispatch_timeout` is deprecated and has no effect.

### `shared_subscriptions`

Off by default, every connection owns a JetStream consumer, so each message is
pulled, sent over NUTS' NATS connection and formatted once per subscriber.
With `shared_subscriptions`, connections that are caught up with the live
stream share one consumer per topic set and each message is formatted once. A
connection with history to replay catches up on its own consumer first, and
one that falls more than `client_buffer_size` frames behind returns to its own
consumer until it catches up again — without a gap and without being
disconnected. Use it when many clients subscribe to the same topics,
especially with large messages.

### `nats_idle_heartbeat`

The heartbeat interval of every pull request the stream's consumer sends, 10
seconds by default. When heartbeats stop — a dead link, or a consumer reaped
or deleted on the server — the consumer recreates itself from the last
delivered sequence and the stream continues without a gap. Values must stay
under 15 seconds. `-1` is still accepted but no longer disables anything; a
warning is logged.

### `replay_max_messages` and `replay_window`

Both bound the catch-up that fires when a client reconnects with an old
`last-id`. They guard against replay storms when JetStream retention is large.

- `replay_max_messages` — closes the SSE connection after the configured
  number of historical events; the client reconnects with a fresher cursor.
- `replay_window` — caps replay to the last N seconds; if the cursor is older
  than the window, NUTS starts at `now - window`.

Both default to `0` (unlimited) for backward compatibility — set bounds for
public or multi-tenant routes.

### CORS and `allowed_origins`

NUTS never emits a literal `Access-Control-Allow-Origin: *`; it echoes the
request `Origin` when it is allow-listed. A `Vary: Origin` header is added so
shared caches don't leak responses between origins.

`Access-Control-Allow-Credentials: true` is only advertised when the
incoming `Origin` is explicitly listed in `allowed_origins`. With `*`,
requests are accepted but credentials are **not** advertised — browsers will
reject credentialed cross-origin streams.

```caddyfile
# Wildcard — anonymous CORS only
allowed_origins *

# Explicit — credentials allowed for these origins
allowed_origins https://app.example.com https://admin.example.com
```

`allowed_methods` is intentionally limited to `GET` and `OPTIONS`.

### Subscriber Authentication (JWT)

The `nats_credentials`, `nats_token`, and `nats_user` / `nats_password`
directives authenticate the **NUTS process** to NATS. Subscriber access is a
separate concern.

Setting `subscriber_jwt_key` requires an HMAC-signed JWT before NUTS creates a
JetStream consumer. Tokens come from `Authorization: Bearer <jwt>` or, when
`subscriber_jwt_cookie` is set, from that cookie. The token must include a
`subscribe` claim listing allowed topic filters (before `topic_prefix` is
applied):

```json
{
  "sub": "user-123",
  "exp": 1777392000,
  "subscribe": ["orders.*", "tenant-a.>"]
}
```

NATS-style tokens are supported: exact (`orders.created`), single-token
wildcards (`orders.*`), tail wildcards (`tenant-a.>`), or `*` / `>` for any
topic on that route. Browser `EventSource` cannot set custom `Authorization`
headers — use the cookie form for browser clients.

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

### Liveness and Readiness Probes

```bash
curl http://localhost:8080/events/livez   # process only
curl http://localhost:8080/events/readyz  # NATS + stream
```

- `live_path` (default `/livez`) — process liveness only. Use for Kubernetes
  liveness probes.
- `ready_path` (default `/readyz`) — checks NATS connection and stream.
  Use for readiness probes and load-balancer target health.
- `health_path` (default `/healthz`) — backward-compatible readiness check.

### Prometheus Metrics

NUTS registers the following metrics; expose them via Caddy's `metrics` handler:

```caddyfile
:8080 {
    route /metrics { metrics }
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

| Metric | Type | Description |
|--------|------|-------------|
| `nuts_active_connections` | Gauge | Currently connected SSE clients |
| `nuts_messages_delivered_total` | Counter | SSE message events successfully written to clients |
| `nuts_messages_dropped_total{reason}` | Counter | Messages not delivered (`reason`: `raw_payload`, `formatted_sse_message` — exceeded `max_event_size`; `replay_window` — a replayed message older than `replay_window`; `control_message` — a subject delete marker or schedule definition) |
| `nuts_wildcard_filter_drops_total` | Counter | Deprecated, always `0`: the pre-2.10 wildcard fallback was removed |
| `nuts_slow_client_disconnects_total` | Counter | Clients disconnected because a write missed `write_timeout` |
| `nuts_replay_requests_total` | Counter | Connections requesting message replay |
| `nuts_replay_fallbacks_total` | Counter | Replay requests that fell back (sequence purged, older than `replay_window`, ahead of the stream, or stream info unavailable), counted once the consumer exists |
| `nuts_replay_cap_reached_total` | Counter | Replaying connections closed after `replay_max_messages` |
| `nuts_subscription_errors_total` | Counter | Stream requests refused because a topic is outside the stream or the consumer could not be created |
| `nuts_connections_rejected_total{reason}` | Counter | Connections rejected before streaming (`reason`: `max_connections`, `stream_consumer_limit`, `auth_missing_token`, `auth_invalid_token`, `auth_topic_forbidden`) |
| `nuts_dispatch_timeout_total` | Counter | Deprecated, always `0`: `dispatch_timeout` has no effect |
| `nuts_write_disconnects_total{site}` | Counter | SSE streams ended by a response-writer write error (`site`: `connected`, `message`, `heartbeat`) |
| `nuts_nats_async_errors_total{kind}` | Counter | Asynchronous NATS client errors (`kind`: `slow_consumer`, `timeout`, `connection_state`, `consumer_invalidated` — legacy, stays `0`, `other`) |
| `nuts_consumer_invalidated_total{reason}` | Counter | Consumers lost under a live stream (`reason`: `recreated` — recovered from the last delivered sequence; `unrecoverable` — recreation failed and the stream closed) |
| `nuts_readiness_failures_total{cause}` | Counter | `/readyz` responses returning `503` (`cause`: `nats_disconnected`, `jetstream_missing`, `stream_info_error`) |
| `nuts_shared_subscriptions` | Gauge | Shared subscriptions (one consumer per topic set) with `shared_subscriptions` on |
| `nuts_shared_transitions_total{transition}` | Counter | Connections joining shared subscriptions (`joined`) or leaving them because they fell behind (`fell_behind`) or the shared consumer failed (`shared_failed`) |
| `nuts_nats_connection_events_total{event}` | Counter | NATS connection-state transitions (`event`: `disconnect`, `reconnect`, `closed`, `lame_duck`) |

### Hub Discovery

When `hub_url` is configured, every SSE response includes a `Link` header:

```
Link: <https://example.com/events>; rel="nuts"
```

This lets clients discover the event hub URL from any response that carries
the header (NUTS itself, or an upstream API / proxy that re-emits it).

## JetStream Setup

NUTS requires a pre-configured JetStream stream — create it before starting Caddy.

```bash
nats stream add EVENTS \
  --subjects "events.>" \
  --max-consumers 10000 \
  --storage file \
  --retention limits \
  --max-msgs 10000 \
  --max-age 24h \
  --discard old
```

| Option | Recommended | Purpose |
|--------|-------------|---------|
| `--subjects` | Match `topic_prefix` + `>` | Subjects the stream captures |
| `--storage` | `file` | `file` for persistence, `memory` for speed |
| `--retention` | `limits` | How messages are retained |
| `--max-msgs` | `10000` | Maximum messages to keep |
| `--max-age` | `24h` | Maximum age of messages |
| `--discard` | `old` | Discard oldest when limit reached |

## Client-Side Usage

### Connecting with EventSource

```javascript
// Single topic
const events = new EventSource('/events?topic=notifications');

// Multiple topics
const events = new EventSource('/events?topic=notifications&topic=updates');

// Path-based topic
const events = new EventSource('/events/my-topic');
```

### Handling Messages

```javascript
events.addEventListener('connected', (e) => {
    const { topics } = JSON.parse(e.data);
    console.log('Connected to:', topics);
});

events.addEventListener('message', (e) => {
    const { topic, payload, time } = JSON.parse(e.data);
    console.log(`[${topic}] at ${time}:`, payload);

    if (e.lastEventId) {
        localStorage.setItem('lastEventId', e.lastEventId);
    }
});

events.onerror = (e) => {
    console.error('SSE error:', e);
    // EventSource auto-reconnects and sends Last-Event-ID automatically
};
```

### Message Format

```
event: message
data: {"topic":"my-topic","payload":{"your":"data"},"time":"2024-01-01T12:00:00Z"}
id: 12345
```

The `id` field is the JetStream sequence number, used for replay. It comes
last in each event, so clients that record an id as soon as they parse it
cannot skip a message whose frame was cut off.

### Message Replay

Clients can resume from where they left off using `last-id` or the standard
`Last-Event-ID` header:

```javascript
const lastId = localStorage.getItem('lastEventId');
const url = lastId
    ? `/events?topic=notifications&last-id=${encodeURIComponent(lastId)}`
    : '/events?topic=notifications';
const events = new EventSource(url);
```

**Replay behavior:**

- Messages with sequence numbers greater than `last-id` are delivered
- If the sequence no longer exists (expired or deleted), all available
  messages are replayed
- Without `last-id`, only new messages are delivered
- Standard `EventSource` reconnects send `Last-Event-ID` automatically
- When both are present, a valid `Last-Event-ID` header wins over
  `?last-id=`: `EventSource` resends its original URL on every reconnect,
  and the header carries the fresher position. A malformed `?last-id=` is
  rejected with `400`; a malformed header is logged and ignored

> **Replay storm caveat:** when the fallback fires, all retained messages are
> replayed. Cap with `replay_max_messages` and/or `replay_window` for public
> or multi-tenant routes.

### Slow Clients

Each stream pulls from JetStream only as fast as its client reads, so a client
that falls behind during a burst or a long replay keeps its connection while
the backlog waits in JetStream. A client that stops reading altogether is
disconnected once a write misses `write_timeout` (30 s by default), and
resumes from its last event ID.

### Transient Failures

A browser `EventSource` gives up for good when a connection is answered with
anything but a `200` event stream. For requests sent with
`Accept: text/event-stream`, NUTS therefore answers transient failures — NATS
unavailable, `max_connections`, the stream's consumer limit, a consumer that
cannot be created — with a `200` stream holding only a jittered `retry:`
delay. The browser waits, reconnects and keeps its `Last-Event-ID`. Other
clients get `503` or `429` with `Retry-After`.

## Example Scenarios

### Chat Application

```caddyfile
:8080 {
    route /chat/* {
        uri strip_prefix /chat
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
nats stream add CHAT --subjects "chat.>" --max-consumers 10000 --storage file --max-age 7d
```

```javascript
const room = 'room-123';
const events = new EventSource(`/chat/messages?topic=${room}`);
```

### Real-Time Dashboard

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
nats stream add METRICS --subjects "metrics.>" --max-consumers 10000 --storage memory --max-age 1h
```

### Authenticated NATS Connection

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

### Multi-Tenant Routes

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
