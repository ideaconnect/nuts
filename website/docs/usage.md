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
    # NATS server URL (required)
    nats_url <url>

    # JetStream stream name (required)
    stream_name <name>

    # NATS authentication (choose exactly one; user/password must be set together)
    nats_credentials <path>             # Path to .creds file
    nats_token <token>                  # Token auth
    nats_user <username>                # User/password auth
    nats_password <password>

    # Optional NATS TLS / mTLS
    nats_tls_ca <path>                  # CA bundle for verifying the server
    nats_tls_cert <path>                # Client certificate (mTLS)
    nats_tls_key <path>                 # Client key (mTLS)
    nats_tls_insecure_skip_verify       # Disable server verification (DEV ONLY)

    # Subscriber auth (HMAC-signed JWT, optional)
    subscriber_jwt_key <secret>         # Enable JWT subscriber auth and topic claims
    subscriber_jwt_cookie <name>        # Cookie name for browser EventSource clients

    # CORS
    allowed_origins <origins...>        # Default: *
    allowed_headers <headers...>        # Default: Cache-Control Last-Event-ID
    allowed_methods <methods...>        # Only GET / OPTIONS are supported

    # Routing & topic shape
    topic_prefix <prefix>               # Prefix for all subscriptions
    max_topics_per_subscription <count> # Per-request topic cap (0=default 32, <0=unlimited)

    # Streaming behaviour
    heartbeat_interval <seconds>        # SSE keep-alive interval (default: 30)
    reconnect_wait <seconds>            # NATS reconnect wait (default: 2)
    max_reconnects <count>              # Max NATS reconnects, 0=none, -1=infinite (default: -1)
    nats_idle_heartbeat <seconds>       # JetStream consumer idle heartbeat (default: 10, 0=default, -1=disable)

    # Per-event / per-connection limits
    max_event_size <bytes>              # Max SSE frame size (0=default 1 MiB, <0=unlimited)
    max_connections <count>             # Global concurrent-stream cap (default: 0 = unlimited)
    client_buffer_size <count>          # Per-connection send buffer (0=default 64)
    dispatch_timeout <seconds>          # Cap slow-client signal wait in NATS callbacks
    write_timeout <seconds>             # Cap each SSE write/flush

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

Limits the total size of a single SSE event frame — `id:`, `event:`, and `data:`
lines plus the JSON-encoded payload. Frames exceeding the limit are silently
dropped and logged as warnings.

Typical SSE overhead (id, event type, topic, timestamp) is ~120–150 bytes, so a
1000-byte cap leaves ~850 bytes for the raw payload. Use `0` for the 1 MiB
default, or a negative value to disable the limit entirely.

### `max_connections`

Caps the number of concurrent SSE streams per NUTS instance. When the cap is
reached, new clients receive `429 Too Many Requests` (RFC 6585) with
`Retry-After: 5` and `nuts_connections_rejected_total{reason="max_connections"}`
increments. A client-side concurrency cap is deliberately distinct from the
`503` returned when NATS or JetStream is genuinely unavailable.

Buffered-message footprint is bounded by
`max_connections × client_buffer_size × max_event_size`. With defaults that's
up to 64 MiB per connection — size accordingly.

### `dispatch_timeout` and `write_timeout`

Optional guards against slow downstream connections:

- `dispatch_timeout <seconds>` — caps how long a NATS callback waits to notify
  the streaming loop after the client's queue is already full. `0` preserves
  the original unbounded wait.
- `write_timeout <seconds>` — sets a per-frame write deadline before each SSE
  frame is written and flushed. `0` defers entirely to Caddy / HTTP server
  config.

### `nats_idle_heartbeat`

Sets the server-side `IdleHeartbeat` interval on the ephemeral JetStream push
consumer NUTS creates for each stream. When the stream is quiet, the JetStream
server still emits periodic heartbeat frames so the NATS client can tell a live
consumer from a wedged push path — for example one whose server-side consumer
was reaped during a network blip or lost across a leafnode failover. Without it,
NUTS could stay attached to a zombie consumer until the client reconnects for
unrelated reasons.

Default-on at `10` seconds. Use `0` for the default, or `-1` to disable. Values
must be under `InactiveThreshold / 2` (currently 15s) so two missed heartbeats
are detectable before the server reaps the consumer. Missed heartbeats surface
as `nuts_nats_async_errors_total{kind="consumer_invalidated"}`.

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
| `nuts_messages_dropped_total{reason}` | Counter | Messages dropped during SSE formatting (`reason`: `raw_payload`, `formatted_sse_message` — exceeded `max_event_size`) |
| `nuts_wildcard_filter_drops_total` | Counter | Messages filtered client-side by the multi-topic wildcard fallback (NATS < 2.10 delivering unrequested subjects) |
| `nuts_slow_client_disconnects_total` | Counter | Clients disconnected due to slow consumption (full per-connection buffer) |
| `nuts_replay_requests_total` | Counter | Connections requesting message replay |
| `nuts_replay_fallbacks_total` | Counter | Replay requests that fell back (sequence purged, or older than `replay_window`) |
| `nuts_replay_cap_reached_total` | Counter | Replaying connections closed after `replay_max_messages` |
| `nuts_subscription_errors_total` | Counter | Failed JetStream subscription attempts |
| `nuts_connections_rejected_total{reason}` | Counter | Connections rejected before streaming (`reason`: `max_connections`, `auth_missing_token`, `auth_invalid_token`, `auth_topic_forbidden`) |
| `nuts_dispatch_timeout_total` | Counter | NATS callbacks that timed out signalling a slow SSE client (`dispatch_timeout`) |
| `nuts_write_disconnects_total{site}` | Counter | SSE streams ended by a response-writer write error (`site`: `connected`, `message`, `heartbeat`) |
| `nuts_nats_async_errors_total{kind}` | Counter | Asynchronous NATS client errors (`kind`: `slow_consumer`, `timeout`, `connection_state`, `consumer_invalidated`, `other`) |
| `nuts_consumer_invalidated_total{reason}` | Counter | JetStream push-consumer invalidation events (`reason`: `heartbeat_missed`, `slow_consumer`) — detection-only today, always `0` until M9 Batch B |
| `nuts_readiness_failures_total{cause}` | Counter | `/readyz` responses returning `503` (`cause`: `nats_disconnected`, `jetstream_missing`, `stream_info_error`) |
| `nuts_nats_connection_events_total{event}` | Counter | NATS connection-state transitions (`event`: `disconnect`, `reconnect`, `closed`) |

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
id: 12345
event: message
data: {"topic":"my-topic","payload":{"your":"data"},"time":"2024-01-01T12:00:00Z"}
```

The `id` field is the JetStream sequence number, used for replay.

### Message Replay

Clients can resume from where they left off using `last-id` or the standard
`Last-Event-ID` header:

```javascript
const lastId = localStorage.getItem('lastEventId') || '';
const events = new EventSource(`/events?topic=notifications&last-id=${lastId}`);
```

**Replay behavior:**

- Messages with sequence numbers greater than `last-id` are delivered
- If the sequence no longer exists (expired or deleted), all available
  messages are replayed
- Without `last-id`, only new messages are delivered
- Standard `EventSource` reconnects send `Last-Event-ID` automatically

> **Replay storm caveat:** when the fallback fires, all retained messages are
> replayed. Cap with `replay_max_messages` and/or `replay_window` for public
> or multi-tenant routes.

### Slow Clients

NUTS does not silently drop messages for active clients. If a client falls
behind and its per-connection queue fills, NUTS disconnects the SSE session.
The client can then reconnect and resume from the last delivered event ID —
no data is lost silently.

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
nats stream add CHAT --subjects "chat.>" --storage file --max-age 7d
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
nats stream add METRICS --subjects "metrics.>" --storage memory --max-age 1h
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
