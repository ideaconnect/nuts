# Configuration Matrix

This matrix lists every supported `nuts` Caddyfile directive, its matching JSON
field, default, validation rules, and operational notes. Defaults are applied
during Caddy provisioning after semantic validation.

## Required Connection

| Caddyfile directive | JSON field | Default | Valid values | Notes |
| --- | --- | --- | --- | --- |
| `nats_url <url>` | `nats_url` | Required | NATS URL such as `nats://nats:4222` or `tls://nats:4222` | Connects NUTS to NATS. Plain `nats://` with credentials is allowed but logs a warning. |
| `stream_name <name>` | `stream_name` | Required | Existing JetStream stream name | The stream must exist before Caddy provisions the handler. NUTS does not create streams. |

## NATS Authentication

Choose at most one authentication mode. `nats_user` and `nats_password` count as
one mode and must be configured together.

| Caddyfile directive | JSON field | Default | Valid values | Notes |
| --- | --- | --- | --- | --- |
| `nats_credentials <path>` | `nats_credentials` | Empty | Path to a NATS `.creds` file | Preferred for NATS account-based security. |
| `nats_token <token>` | `nats_token` | Empty | NATS token string | Avoid plaintext `nats://` for production token auth. |
| `nats_user <username>` | `nats_user` | Empty | Username | Must be paired with `nats_password`. |
| `nats_password <password>` | `nats_password` | Empty | Password | Must be paired with `nats_user`. |

## NATS TLS

| Caddyfile directive | JSON field | Default | Valid values | Notes |
| --- | --- | --- | --- | --- |
| `nats_tls_ca <path>` | `nats_tls_ca` | Empty | PEM CA bundle path | Verifies the NATS server certificate with this CA bundle. |
| `nats_tls_cert <path>` | `nats_tls_cert` | Empty | PEM client certificate path | Must be paired with `nats_tls_key` for mTLS. |
| `nats_tls_key <path>` | `nats_tls_key` | Empty | PEM client key path | Must be paired with `nats_tls_cert`. |
| `nats_tls_insecure_skip_verify [bool]` | `nats_tls_insecure_skip_verify` | `false` | No argument means `true`; optional boolean accepted | Disables server certificate verification. Development only; logs a warning. |

## Topics, CORS, And HTTP Behavior

| Caddyfile directive | JSON field | Default | Valid values | Notes |
| --- | --- | --- | --- | --- |
| `topic_prefix <prefix>` | `topic_prefix` | Empty | NATS subject prefix | Prepended to every requested topic. Include the trailing `.` when needed, for example `events.`. Validated at config load: must use the topic alphabet (`[A-Za-z0-9._-]`), may not contain NATS wildcards (`*`, `>`), may not start with `.` or `$`, may not contain consecutive dots, and is capped at 256 bytes. A wildcard slip would silently broaden every client's subscription. |
| `allowed_origins <origins...>` | `allowed_origins` | `*` | One or more origins or `*` | Explicit origins allow credentialed CORS. Wildcard allows anonymous browser reads but does not advertise credentials. |
| `allowed_headers <headers...>` | `allowed_headers` | `Cache-Control Last-Event-ID` | One or more request header names | Used for CORS preflight responses. Add custom headers only if a non-native SSE client sends them. |
| `allowed_methods <methods...>` | `allowed_methods` | `GET OPTIONS` | Only `GET` and `OPTIONS` | Other methods are rejected during validation because NUTS only serves SSE and preflight requests. |
| `subscriber_jwt_key <secret>` | `subscriber_jwt_key` | Empty | HMAC secret for HS256/HS384/HS512 JWTs | Enables first-party subscriber auth. Tokens must include a `subscribe` claim with allowed topic filters. |
| `subscriber_jwt_cookie <name>` | `subscriber_jwt_cookie` | Empty | Valid HTTP cookie name | Optional cookie source for browser EventSource clients. Requires `subscriber_jwt_key`; `Authorization: Bearer` is always accepted when JWT auth is enabled. |
| `health_path <path>` | `health_path` | `/healthz` | Path with or without leading `/` | Legacy readiness-style endpoint. Checks NATS and stream availability. |
| `live_path <path>` | `live_path` | `/livez` | Path with or without leading `/` | Process liveness only; does not check NATS or JetStream. |
| `ready_path <path>` | `ready_path` | `/readyz` | Path with or without leading `/` | Readiness endpoint. Checks NATS connection and configured stream. |
| `hub_url <url>` | `hub_url` | Empty | Hub URL | Adds `Link: <url>; rel="nuts"` to SSE responses when set. |

Probe paths match exactly or by suffix within the configured route. For example,
with a public `/events` route that strips its prefix, `/events/readyz` reaches
NUTS as `/readyz`.

Subscriber JWT `exp` and `nbf` time claims are optional; when present they are
enforced. For public or browser-facing routes, issue short-lived tokens with
`exp`. Compact JWTs over 8 KiB, decoded JWT segments over 6 KiB, and
`subscribe` claims with more than 128 filters are rejected.

## Streaming And Replay Tuning

| Caddyfile directive | JSON field | Default | Valid values | Notes |
| --- | --- | --- | --- | --- |
| `heartbeat_interval <seconds>` | `heartbeat_interval` | `30` | Positive integer; `0` or negative uses default | Sends SSE comments to keep idle proxies and clients alive. |
| `reconnect_wait <seconds>` | `reconnect_wait` | `2` | Positive integer; `0` or negative uses default | Delay between NATS reconnect attempts. |
| `max_reconnects <count>` | `max_reconnects` | `-1` when omitted | Integer `>= -1`; `0` means no reconnects, `-1` means unlimited | JSON uses a pointer internally so explicit `0` is preserved. |
| `max_event_size <bytes>` | `max_event_size` | `1048576` when `0` or omitted | Positive cap, `0` for default, negative for unlimited | Caps the formatted SSE frame. Oversized events are dropped and counted. |
| `max_connections <count>` | `max_connections` | `0` | Integer `>= 0` | `0` disables the cap. Rejected clients receive `503` and `Retry-After: 5`. |
| `max_topics_per_subscription <count>` | `max_topics_per_subscription` | `32` when `0` or omitted | Positive cap, `0` for default, negative for unlimited | Caps the distinct `?topic=` filters allowed per SSE request after deduplication. Requests over the cap receive `400`. |
| `client_buffer_size <count>` | `client_buffer_size` | `64` when `0` or omitted | Integer `>= 0` | Per-connection queue length. A full queue disconnects the slow client. |
| `dispatch_timeout <seconds>` | `dispatch_timeout` | `0` | Integer `>= 0` | `0` leaves the wait unbounded — the NATS callback parks until the SSE loop observes the slow-client signal or the connection tears down. Positive values bound how long a NATS callback waits to signal a slow client after its queue is already full; on expiry the slow-client signal is dropped and `nuts_dispatch_timeout_total` is incremented. |
| `write_timeout <seconds>` | `write_timeout` | `0` | Integer `>= 0` | `0` leaves write deadlines to Caddy/server config. Positive values set per-frame SSE write deadlines when the response writer supports them. |
| `replay_max_messages <count>` | `replay_max_messages` | `0` | Integer `>= 0` | `0` is unlimited. When reached during replay, NUTS closes the stream cleanly. |
| `replay_window <seconds>` | `replay_window` | `0` | Integer `>= 0` | `0` preserves retained replay. Positive values bound old replay cursors to `StartTime(now - replay_window)` while preserving exact sequence replay inside the window. |

## Production Defaults To Revisit

The compatibility defaults are intentionally permissive. Production public or
multi-tenant routes should usually set these explicitly:

| Directive | Why revisit it |
| --- | --- |
| `allowed_origins` | Replace `*` with explicit origins when cookies or credentials are used. |
| `subscriber_jwt_key` / `subscriber_jwt_cookie` | Enable first-party subscriber authentication and topic claims when Caddy/upstream policy is not enough. |
| `max_connections` | Bound total concurrent streams per instance. |
| `max_event_size` | Lower from 1 MiB when payloads are known to be smaller. |
| `client_buffer_size` | Lower from 64 when memory per connection matters. |
| `dispatch_timeout` / `write_timeout` | Bound NATS callback waits and blocked SSE writes for slow or stalled downstream connections. |
| `replay_max_messages` / `replay_window` | Bound replay on large retained streams. |
| `nats_tls_*` | Use TLS and mTLS where NATS is not on a trusted private network. |
