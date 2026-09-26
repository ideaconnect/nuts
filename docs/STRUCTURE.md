# Project Structure

## Go Source Files

### handler.go

Module registration and type definitions. Contains:

- `init()` — registers the Caddy module and the `nuts` Caddyfile directive.
- `Handler` struct — all configuration fields (NATS URL, NATS auth, optional subscriber JWT auth, topics, tuning, TLS, CORS, connection caps, health/liveness/readiness paths, client buffer size, write timeout, the deprecated dispatch timeout, replay caps `replay_max_messages` / `replay_window`, optional `hub_url`) plus runtime state (connection, JetStream context, logger, mutex, live-connection counter, a `shutdown` channel that `Cleanup()` closes to wake in-flight SSE handlers, and the stream counter `Cleanup()` waits on).
- `messageEventPayload` struct — the JSON shape sent to SSE clients (`topic`, `payload`, `time`).
- `CaddyModule()` — returns the module ID `http.handlers.nuts`.
- Interface guards ensuring `Handler` satisfies `caddy.Module`, `caddy.Provisioner`, `caddy.Validator`, `caddy.CleanerUpper`, `caddyhttp.MiddlewareHandler`, and `caddyfile.Unmarshaler`.

### provision.go

Caddy lifecycle management — connecting to NATS and tearing down on shutdown. Contains:

- `validateRequiredFields()` — fast pre-flight check run at the top of `Provision()` so bad config never opens a socket.
- `Provision()` — validates the config and logs the transport-security warnings before dialling, applies defaults (heartbeat, reconnect, max-event-size, client buffer, `write_timeout` 30 s, probe paths, allowed headers/methods), creates the `shutdown` channel, calls `connectNATS()`, warns about servers older than 2.14.7, creates a JetStream context with a default API timeout, verifies the configured stream exists within 2 s and logs its limiting settings, and rolls back on failure.
- `connectNATS()` — builds NATS connection options (reconnect, auth, TLS, lifecycle and lame-duck callbacks, no callbacks after Cleanup closes the connection) and dials the server.
- `buildTLSConfig()` — assembles a `*tls.Config` (TLS 1.2 minimum) from `nats_tls_ca`, `nats_tls_cert`, `nats_tls_key`, `nats_tls_insecure_skip_verify`.
- `Cleanup()` — teardown: closes the `shutdown` channel (waking any in-flight SSE handlers), refuses new streams, waits up to 3 s for ended streams to delete their consumers, then closes the NATS connection. Idempotent.
- `Validate()` — re-runs the checks Provision applies before dialling (required fields, one auth mode, numeric ranges and sentinels, `nats_url` schemes, `allowed_origins` format, `nats_tls_ca` without `nats_tls_insecure_skip_verify`, subscriber JWT cookie config), and warns about wildcard CORS, the deprecated `dispatch_timeout`, `nats_idle_heartbeat -1` and short JWT keys. The transport warnings (credentials sent unencrypted, `insecure_skip_verify`) come from Provision before it connects.

### auth.go

Subscriber JWT authorization helpers. Contains:

- `authorizeStreamRequest()` — enforces `subscriber_jwt_key` before any JetStream subscription is created.
- `verifySubscriberJWT()` — verifies HMAC-signed JWTs (`HS256`, `HS384`, `HS512`) and time claims.
- `parseSubscribeClaim()` — reads exact and wildcard topic filters from the JWT `subscribe` claim.

### serve.go

HTTP/SSE request handling — the core streaming loop. Contains:

- `ServeHTTP()` — handles the configurable liveness (`live_path`, default `/livez`), readiness (`ready_path`, default `/readyz`), and legacy health (`health_path`, default `/healthz`) endpoints (one trailing slash ignored), OPTIONS (CORS preflight), non-GET passthrough, topic extraction and validation, subscriber JWT authorization, path-shorthand (`/a/b` → topic `a.b`), `Last-Event-ID` / `last-id` replay parsing (a valid header wins over the query; a bad header is logged and ignored; a bad `?last-id=` returns 400), retryable rejections (`rejectTransient`: a `retry:` stream for EventSource, `503`/`429` with `Retry-After` otherwise) for NATS outages, `max_connections` and consumer failures, stream-info planning (start position, replay fallbacks, topic checks), and then hands off to `openConsumerStream()` and `serveStream()`.
- `serveStream()` — the SSE writer loop: the `connected` event, message frames with replay caps and the history-only `replay_window` filter, heartbeats, write deadlines, and every termination path with its `disconnect_reason`.
- `formatMessageEvent()` — renders one SSE frame, dropping control messages and oversized payloads or frames first.
- `reserveConnSlot()` / `releaseConnSlot()` — atomic counter used to enforce `max_connections`.
- `matchesHealthPath()` / `matchesLivePath()` / `matchesReadyPath()` — exact-or-suffix match against the configured probe paths.
- `setCORSHeaders()` — matches the request `Origin` against `AllowedOrigins` and echoes it back, along with `allowed_headers` and `allowed_methods`.

### consumer.go

The JetStream side of one SSE stream. Contains:

- `openConsumerStream()` — creates the request's ordered pull consumer (`orderedConsumerConfig()`: filters, explicit start position, inactive threshold capped by the stream's consumer limit) and starts pulling with `pullOptions()` (`client_buffer_size` prefetch, `nats_idle_heartbeat`).
- `startStreamFeed()` — the feed goroutine: pulls one message at a time, formats it, drops what cannot be sent, and hands frames to the writer; the hand-off blocks while the writer is busy, which is what stops pulling.
- `consumerStream.close()` / `deleteConsumer()` — stops the feed and deletes the consumer in the background, logging failures and telling `Cleanup()` when it is gone.

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

Prometheus counters and gauges registered via `promauto`. The README metrics table describes each one; `docs_test.go` fails when a document or the ops files name a metric that is not registered here, or when the README or website leave one out.

### nats_test.go

Core unit and integration tests. Uses an embedded NATS server with JetStream to test validation, Caddyfile parsing, topic filtering, SSE streaming, replay, slow-client disconnect, oversized-event dropping, and cleanup.

### hardening_test.go

Focused tests for security hardening and related correctness items: configurable CORS headers/methods, subscriber JWT authorization, oversized-raw-payload drop, `max_connections` rejection + metric increment, write timeout behavior, config validation (URL schemes, origins, sentinels), cleartext-auth and insecure-TLS warnings, TLS cert/key pairing, `MaxReconnects=0` vs default, integer-directive junk-suffix rejection, custom `health_path`, distinct `live_path` / `ready_path` probe behavior, negative `MaxEventSize` disabling the limit, `replay_max_messages` capping retained replay, `replay_window` switching old cursors to `StartTime`, pre-flight config rejection, and `Cleanup()` waking in-flight SSE handlers.

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
3. **JetStream consumer** — `serve.go` reads the stream's info and plans the start position; `consumer.go` creates an ordered pull consumer bound to the configured stream. New clients start at the stream's `LastSeq + 1`; reconnecting clients at `lastID + 1`. If the requested sequence has been purged, is ahead of the stream, or is older than `replay_window`, NUTS falls back to `DeliverAll()` or `StartTime(now - replay_window)`. `replay_max_messages` closes retained replay after the configured historical event count.
4. **Feed** — a goroutine in `consumer.go` pulls up to `client_buffer_size` messages ahead, formats each one (JetStream sequence as the SSE `id:`, `max_event_size` checks, control messages skipped) and hands frames to the writer one at a time. While the writer is busy, pulling stops.
5. **SSE writer loop** — a single `select` in `serve.go` multiplexes:
   - **`feed.frames`** — formatted frames, written and flushed with a `write_timeout` deadline; a missed deadline disconnects the client as slow.
   - **`feed.errs`** — the consumer could not be recreated; the stream closes so the client resumes elsewhere.
   - **`heartbeat.C`** — periodic SSE comment (`: heartbeat <timestamp>`) keeps the connection alive through proxies/load balancers.
   - **`ctx.Done()`** — client disconnect; the consumer is deleted in the background.
   - **`shutdown`** — `Cleanup()` closes this channel on module teardown so in-flight handlers return promptly instead of waiting for the next heartbeat or NATS-side error.
6. **Client reconnect** — the browser's `EventSource` automatically reconnects and sends the last received `id:` as `Last-Event-ID`, resuming from where it left off.
