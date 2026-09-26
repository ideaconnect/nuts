# Configuration Matrix

This matrix lists every supported `nuts` Caddyfile directive, its matching JSON
field, default, validation rules, and operational notes. Defaults are applied
during Caddy provisioning after semantic validation.

## Required Connection

| Caddyfile directive | JSON field | Default | Valid values | Notes |
| --- | --- | --- | --- | --- |
| `nats_url <url>` | `nats_url` | Required | `nats://`, `tls://`, `ws://` or `wss://` URL, `host:port` (dialled as `nats://`), or a comma-separated list of these | Connects NUTS to NATS. Other schemes are rejected: nats.go would dial a typo such as `tsl://` as plaintext `nats://`. Credentials over `nats://` or `ws://` without `nats_tls_*` directives are allowed but log a warning before NUTS connects. Credentials in the URL are redacted in logs. |
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
| `nats_tls_ca <path>` | `nats_tls_ca` | Empty | PEM CA bundle path | Verifies the NATS server certificate with this CA bundle. Rejected together with `nats_tls_insecure_skip_verify`, which would silently ignore it. |
| `nats_tls_cert <path>` | `nats_tls_cert` | Empty | PEM client certificate path | Must be paired with `nats_tls_key` for mTLS. |
| `nats_tls_key <path>` | `nats_tls_key` | Empty | PEM client key path | Must be paired with `nats_tls_cert`. |
| `nats_tls_insecure_skip_verify [bool]` | `nats_tls_insecure_skip_verify` | `false` | No argument means `true`; optional boolean accepted | Disables server certificate verification. Development only; logs a warning before NUTS connects. Cannot be combined with `nats_tls_ca`. |

## Topics, CORS, And HTTP Behavior

| Caddyfile directive | JSON field | Default | Valid values | Notes |
| --- | --- | --- | --- | --- |
| `topic_prefix <prefix>` | `topic_prefix` | Empty | NATS subject prefix | Prepended to every requested topic. Include the trailing `.` when needed, for example `events.`. Validated at config load: must use the topic alphabet (`[A-Za-z0-9._-]`), may not contain NATS wildcards (`*`, `>`), may not start with `.` or `$`, may not contain consecutive dots, and is capped at 256 bytes. A wildcard slip would silently broaden every client's subscription. |
| `allowed_origins <origins...>` | `allowed_origins` | `*` | `*` or one or more origins written as browsers send them: lowercase `scheme://host[:port]`, no path or trailing slash, one per argument | Explicit origins allow credentialed CORS. Wildcard allows anonymous browser reads but does not advertise credentials. Entries that could never match (empty, comma-joined, with a path, uppercase) are rejected. |
| `allowed_headers <headers...>` | `allowed_headers` | `Cache-Control Last-Event-ID` | One or more request header names | Used for CORS preflight responses. Add custom headers only if a non-native SSE client sends them. |
| `allowed_methods <methods...>` | `allowed_methods` | `GET OPTIONS` | Only `GET` and `OPTIONS` | Other methods are rejected during validation because NUTS only serves SSE and preflight requests. |
| `subscriber_jwt_key <secret>` | `subscriber_jwt_key` | Empty | HMAC secret for HS256/HS384/HS512 JWTs | Enables first-party subscriber auth. Tokens must include a `subscribe` claim with allowed topic filters. |
| `subscriber_jwt_cookie <name>` | `subscriber_jwt_cookie` | Empty | Valid HTTP cookie name | Optional cookie source for browser EventSource clients. Requires `subscriber_jwt_key`; `Authorization: Bearer` is always accepted when JWT auth is enabled. |
| `health_path <path>` | `health_path` | `/healthz` | Path with or without leading `/` | Legacy readiness-style endpoint. Checks NATS and stream availability. |
| `live_path <path>` | `live_path` | `/livez` | Path with or without leading `/` | Process liveness only; does not check NATS or JetStream. |
| `ready_path <path>` | `ready_path` | `/readyz` | Path with or without leading `/` | Readiness endpoint. Checks NATS connection and configured stream. |
| `hub_url <url>` | `hub_url` | Empty | Hub URL | Adds `Link: <url>; rel="nuts"` to SSE responses when set. |

Probe paths match exactly or by suffix within the configured route, ignoring
one trailing slash. For example, with a public `/events` route that strips its
prefix, `/events/readyz` and `/events/readyz/` reach NUTS as a readiness
probe.

Subscriber JWT `exp` and `nbf` time claims are optional; when present they are
enforced. For public or browser-facing routes, issue short-lived tokens with
`exp`. Compact JWTs over 8 KiB, decoded JWT segments over 6 KiB, and
`subscribe` claims with more than 128 filters are rejected.

## Streaming And Replay Tuning

| Caddyfile directive | JSON field | Default | Valid values | Notes |
| --- | --- | --- | --- | --- |
| `heartbeat_interval <seconds>` | `heartbeat_interval` | `30` | Integer `>= 0`; `0` uses the default | Sends SSE comments to keep idle proxies and clients alive. Negative values are rejected when the Caddyfile is parsed. |
| `reconnect_wait <seconds>` | `reconnect_wait` | `2` | Integer `>= 0`; `0` uses the default | Delay between NATS reconnect attempts. Negative values are rejected when the Caddyfile is parsed. |
| `nats_idle_heartbeat <seconds>` | `nats_idle_heartbeat` | `10` | Integer in `(0, 15)`; `0` uses the default; `-1` accepted for compatibility | Heartbeat interval of every pull request the stream's ordered consumer sends. When heartbeats stop (a dead link, a consumer reaped or deleted on the server), the consumer recreates itself from the last delivered sequence; see README "JetStream consumers". Must stay below 15 s, half the pull request expiry. `-1` no longer disables anything: the library default of 5 s applies and a warning is logged. |
| `max_reconnects <count>` | `max_reconnects` | `-1` when omitted | Integer `>= -1`; `0` means no reconnects, `-1` means unlimited | JSON uses a pointer internally so explicit `0` is preserved. |
| `max_event_size <bytes>` | `max_event_size` | `1048576` when `0` or omitted | Positive cap, `0` for default, negative for unlimited | Caps the formatted SSE frame. Oversized events are dropped and counted. |
| `max_connections <count>` | `max_connections` | `0` | Integer `>= 0` | `0` disables the cap. Rejected clients receive `429` (Too Many Requests, RFC 6585) with a jittered `Retry-After` of 3–8 s; `EventSource` clients get a `200` stream with a `retry:` delay instead (README "Transient failures and EventSource"). Keep it at or below the stream's `max_consumers`: every connection needs its own consumer. |
| `max_topics_per_subscription <count>` | `max_topics_per_subscription` | `32` when `0` or omitted | Positive cap, `0` for the default, `-1` for no limit | Caps the distinct `?topic=` filters allowed per SSE request after deduplication. Requests over the cap receive `400`. Other negative values are rejected as typos; `-1` is logged at startup. |
| `client_buffer_size <count>` | `client_buffer_size` | `64` when `0` or omitted | Integer `>= 0` | How many messages each connection prefetches from JetStream. Pulling pauses while the client is behind, so a full prefetch never disconnects anyone. Per-connection memory is about `client_buffer_size` × the largest message. |
| `shared_subscriptions [bool]` | `shared_subscriptions` | `false` | No argument means `true`; optional boolean accepted | Connections that are caught up with the live stream share one JetStream consumer per topic set, and each message is formatted once. Connections with history to replay, or more than `client_buffer_size` frames behind, use their own consumer until they catch up. See README "`shared_subscriptions`". |
| `dispatch_timeout <seconds>` | `dispatch_timeout` | `0` | Integer `>= 0` | Deprecated, no effect: the pull consumer has no queue hand-off to time out. A positive value logs a warning. It will be removed in the next major release. |
| `write_timeout <seconds>` | `write_timeout` | `30` when `0` or omitted | Integer `>= 0`, or `-1` to disable | Deadline for each SSE write and flush. A client that stops reading is disconnected when a write misses it (`disconnect_reason=slow_client`) and resumes with its last event ID. `-1` leaves write deadlines to Caddy's server config. |
| `replay_max_messages <count>` | `replay_max_messages` | `0` | Integer `>= 0` | `0` is unlimited. When reached during replay, NUTS closes the stream cleanly and the client reconnects from its last event ID. Only history counts: messages published after the request arrived never trip the cap. |
| `replay_window <seconds>` | `replay_window` | `0` | Integer `>= 0` | `0` preserves retained replay. Positive values bound old replay cursors to `StartTime(now - replay_window)` while preserving exact sequence replay inside the window. Also applies when stream info cannot be read or the cursor is ahead of the stream. Live messages are never filtered. |

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
| `write_timeout` | Shorten from 30 s to free stalled connections sooner. |
| `replay_max_messages` / `replay_window` | Bound replay on large retained streams. |
| `nats_tls_*` | Use TLS and mTLS where NATS is not on a trusted private network. |
