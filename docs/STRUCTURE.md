# Project Structure

## Go Source Files

### handler.go

Module registration and type definitions. Contains:

- `init()` — registers the Caddy module and the `nuts` Caddyfile directive.
- `Handler` struct — all configuration fields (NATS URL, NATS auth, optional subscriber JWT auth, topics, tuning, TLS, CORS, connection caps, health/liveness/readiness paths, client buffer size, dispatch/write timeouts, replay caps `replay_max_messages` / `replay_window`, optional `hub_url`) plus runtime state (connection, JetStream context, logger, mutex, live-connection counter, and a `shutdown` channel that `Cleanup()` closes to wake in-flight SSE handlers).
- `messageEventPayload` struct — the JSON shape sent to SSE clients (`topic`, `payload`, `time`).
- `CaddyModule()` — returns the module ID `http.handlers.nuts`.
- Interface guards ensuring `Handler` satisfies `caddy.Module`, `caddy.Provisioner`, `caddy.Validator`, `caddy.CleanerUpper`, `caddyhttp.MiddlewareHandler`, and `caddyfile.Unmarshaler`.

### provision.go

Caddy lifecycle management — connecting to NATS and tearing down on shutdown. Contains:

- `validateRequiredFields()` — fast pre-flight check run at the top of `Provision()` so bad config never opens a socket.
- `Provision()` — applies defaults (heartbeat, reconnect, max-event-size, client buffer, health/liveness/readiness paths, allowed headers/methods; `dispatch_timeout`, `write_timeout`, `replay_max_messages`, and `replay_window` stay at `0` meaning "off"), creates the `shutdown` channel, calls `connectNATS()`, creates a JetStream context, verifies the configured stream exists, and rolls back on failure.
- `connectNATS()` — builds NATS connection options (reconnect, auth, TLS, lifecycle callbacks) and dials the server.
- `buildTLSConfig()` — assembles a `*tls.Config` (TLS 1.2 minimum) from `nats_tls_ca`, `nats_tls_cert`, `nats_tls_key`, `nats_tls_insecure_skip_verify`.
- `Cleanup()` — mutex-protected teardown: closes the `shutdown` channel (waking any in-flight SSE handlers), closes the NATS connection and nils cached state. Idempotent.
- `Validate()` — checks required fields, rejects conflicting auth methods and invalid subscriber JWT cookie config, validates non-negative timeout/replay/buffer values, warns about wildcard CORS with credentials, cleartext `nats://` with auth, and `insecure_skip_verify`.

### auth.go

Subscriber JWT authorization helpers. Contains:

- `authorizeStreamRequest()` — enforces `subscriber_jwt_key` before any JetStream subscription is created.
- `verifySubscriberJWT()` — verifies HMAC-signed JWTs (`HS256`, `HS384`, `HS512`) and time claims.
- `parseSubscribeClaim()` — reads exact and wildcard topic filters from the JWT `subscribe` claim.

### serve.go

HTTP/SSE request handling — the core streaming loop. Contains:

- `ServeHTTP()` — handles the configurable liveness (`live_path`, default `/livez`), readiness (`ready_path`, default `/readyz`), and legacy health (`health_path`, default `/healthz`) endpoints, OPTIONS (CORS preflight), non-GET passthrough, topic extraction and validation, subscriber JWT authorization, path-shorthand (`/a/b` → topic `a.b`), `Last-Event-ID` / `last-id` replay parsing (bad header falls back to `DeliverNew`; bad `?last-id=` returns 400), the global `max_connections` slot reservation with `Retry-After` rejection, JetStream subscription creation with sequence/window fallback, dispatch/write timeout enforcement, the SSE streaming `select` loop (message delivery, slow-client disconnect, heartbeat, context cancellation), and oversized-event guards that drop before JSON parse.
- `reserveConnSlot()` / `releaseConnSlot()` — atomic counter used to enforce `max_connections`.
- `matchesHealthPath()` / `matchesLivePath()` / `matchesReadyPath()` — exact-or-suffix match against the configured probe paths.
- `setCORSHeaders()` — matches the request `Origin` against `AllowedOrigins` and echoes it back, along with `allowed_headers` and `allowed_methods`.

### caddyfile.go

Caddyfile configuration parsing. Contains:

- `UnmarshalCaddyfile()` — maps every supported directive to the corresponding `Handler` field. Uses `strconv.Atoi` for integer directives so values like `123abc` are rejected. Recognised directives: `nats_url`, `stream_name`, `nats_credentials`, `nats_token`, `nats_user`, `nats_password`, `subscriber_jwt_key`, `subscriber_jwt_cookie`, `nats_tls_ca`, `nats_tls_cert`, `nats_tls_key`, `nats_tls_insecure_skip_verify`, `topic_prefix`, `allowed_origins`, `allowed_headers`, `allowed_methods`, `heartbeat_interval`, `reconnect_wait`, `max_reconnects`, `max_event_size`, `max_connections`, `client_buffer_size`, `dispatch_timeout`, `write_timeout`, `replay_max_messages`, `replay_window`, `health_path`, `live_path`, `ready_path`, `hub_url`.
- `parseCaddyfile()` — adapter that Caddy calls to turn a Caddyfile block into a `Handler` instance.

### helpers.go

Standalone utility functions used across the module. Contains:

- `toJSON()` — marshals any value to a JSON string.
- `tryParseJSON()` — attempts to unmarshal `[]byte` as JSON; falls back to a plain string. Callers are expected to bound input length first.
- `writeSSEChunk()` / `writeSSEChunkWithTimeout()` — writes one SSE frame to the `http.ResponseWriter`, optionally applying a per-frame write deadline before flushing.
- `isValidTopic()` — accepts only `[A-Za-z0-9._-]`; rejects empty, overlength (>256), wildcard (`*`, `>`), system (`$`-prefix), control-char, leading/trailing dot, and consecutive-dot topics.
- `isValidCookieName()` — validates `subscriber_jwt_cookie` against the HTTP token character set.
- `redactURL()` — strips embedded credentials from a URL before it is logged.

### cmd/caddy/main.go

Build entry point. Imports the standard Caddy modules plus this module (`github.com/ideaconnect/nuts`) so `go build` produces a Caddy binary with NUTS baked in.

### metrics.go

Prometheus counters and gauges registered via `promauto`. Exposes `nuts_active_connections`, `nuts_messages_delivered_total`, `nuts_messages_dropped_total{reason}` (labelled `raw_payload` or `formatted_sse_message`), `nuts_wildcard_filter_drops_total` (multi-topic wildcard fallback client-side filter), `nuts_slow_client_disconnects_total`, `nuts_replay_requests_total`, `nuts_replay_fallbacks_total`, `nuts_subscription_errors_total`, `nuts_connections_rejected_total{reason}` (incremented when `max_connections` rejects a request), `nuts_replay_cap_reached_total` (incremented when retained replay is cut short by `replay_max_messages`), `nuts_dispatch_timeout_total`, `nuts_nats_async_errors_total{kind}` (incremented by the registered `nats.ErrorHandler` on slow-consumer drops and other async failures), and `nuts_write_disconnects_total{site}` (incremented when an SSE write fails at the `connected`, `message`, or `heartbeat` site).

### nats_test.go

Core unit and integration tests. Uses an embedded NATS server with JetStream to test validation, Caddyfile parsing, topic filtering, SSE streaming, replay, slow-client disconnect, oversized-event dropping, and cleanup.

### hardening_test.go

Focused tests for security hardening and related correctness items: configurable CORS headers/methods, subscriber JWT authorization, oversized-raw-payload drop, `max_connections` rejection + metric increment, dispatch/write timeout behavior, cleartext-auth and insecure-TLS warnings, TLS cert/key pairing, `MaxReconnects=0` vs default, integer-directive junk-suffix rejection, custom `health_path`, distinct `live_path` / `ready_path` probe behavior, negative `MaxEventSize` disabling the limit, `replay_max_messages` capping retained replay, `replay_window` switching old cursors to `StartTime`, pre-flight config rejection, and `Cleanup()` waking in-flight SSE handlers.

### handler_integration_test.go

Runtime integration tests for handler behavior that depends on embedded NATS, JetStream state, metrics, TLS material, SSE heartbeat framing, topic-prefix translation, NATS reconnects, retained-message persistence, and multi-topic subscription paths.

### auth_test.go

Unit tests for JWT parsing and validation, subscriber token extraction, and topic-filter validation.

### helpers_test.go

Unit tests for helper edge cases such as cookie-name validation and SSE write deadline behavior.

### serve_test.go

Unit tests for request parsing, replay planning, message formatting, replay cap accounting, subject-filter helpers, readiness responses, and other `serve.go` pure helper behavior.

### functional_test/main_test.go & steps_test.go

BDD functional tests driven by [Godog](https://github.com/cucumber/godog). Scenarios in `features/*.feature` are executed against a real Docker Compose stack (NATS + Caddy with NUTS).

---

## Documentation And Operations Files

- [ARCHITECTURE.md](ARCHITECTURE.md) - system and replay diagrams plus ownership boundaries.
- [CONFIGURATION.md](CONFIGURATION.md) - complete Caddyfile/JSON directive matrix with defaults, valid values, and operational notes.
- [DEPLOYMENT.md](DEPLOYMENT.md) - production Compose, Kubernetes, and reverse-proxy-protected deployment examples.
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) - browser/EventSource, CORS, replay, Docker, and functional-test troubleshooting.
- [OPERATIONS.md](OPERATIONS.md) - probes, metrics, structured log fields, and incident runbooks.
- [PERFORMANCE.md](PERFORMANCE.md) - load-test coverage and production performance budgets.
- [RELEASE.md](RELEASE.md) - release validation, SBOMs, vulnerability scans, signing, and operator release-note guidance.

---

## Message Flow

### Publishing (sending messages into the system)

```
Producer  ──►  NATS Server  ──►  JetStream Stream
```

1. An external producer publishes a message to a NATS subject (e.g. `events.orders.new`).
2. The NATS server persists the message in the JetStream stream whose subject filter matches the subject.

NUTS itself does **not** publish messages. It is a read-only bridge — any NATS client or service acts as the producer.

### Receiving (delivering messages to browsers)

```
Browser (EventSource)  ──►  Caddy + NUTS Handler  ──►  JetStream Consumer  ──►  NATS Server
```

Step by step:

1. **Browser connects** — opens an `EventSource` to e.g. `GET /events?topic=orders.new`.
2. **Topic validation** — `serve.go` validates the topic via `isValidTopic()`, prepends `TopicPrefix`, and checks for a `Last-Event-ID` header or `last-id` query parameter. If the query is absent the request path is used as a shorthand (`/a/b` → `a.b`).
3. **JetStream subscription** — `serve.go` creates an ephemeral JetStream consumer bound to the configured stream. New clients get `DeliverNew()`; reconnecting clients get `StartSequence(lastID+1)`. If the requested sequence has been purged, NUTS falls back to `DeliverAll()` or `StartTime(now - replay_window)`. If the requested sequence is retained but older than `replay_window`, NUTS starts at that time window instead. `replay_max_messages` closes retained replay after the configured historical event count.
4. **SSE streaming loop** — a single `select` in `serve.go` multiplexes:
   - **`msgChan`** — incoming NATS messages are JSON-wrapped into `messageEventPayload`, tagged with the JetStream sequence as the SSE `id:` field, checked against `MaxEventSize`, and flushed to the client via `writeSSEChunkWithTimeout()` when `write_timeout` is configured.
   - **`slowClient`** — if `msgChan` is full (configurable via `client_buffer_size`, default 64), the next message triggers an immediate disconnect to protect the server. `dispatch_timeout` bounds how long the NATS callback waits when that slow-client signal is already blocked.
   - **`heartbeat.C`** — periodic SSE comment (`: heartbeat <timestamp>`) keeps the connection alive through proxies/load balancers.
   - **`ctx.Done()`** — client disconnect; subscriptions are cleaned up.
   - **`shutdown`** — `Cleanup()` closes this channel on module teardown so in-flight handlers return promptly instead of waiting for the next heartbeat or NATS-side error.
5. **Client reconnect** — the browser's `EventSource` automatically reconnects and sends the last received `id:` as `Last-Event-ID`, resuming from where it left off.
