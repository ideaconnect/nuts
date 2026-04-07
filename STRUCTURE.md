# Project Structure

## Go Source Files

### handler.go

Module registration and type definitions. Contains:

- `init()` — registers the Caddy module and the `nuts` Caddyfile directive.
- `Handler` struct — all configuration fields (NATS URL, auth, topics, tuning) plus runtime state (connection, JetStream context, logger, mutex).
- `messageEventPayload` struct — the JSON shape sent to SSE clients (`topic`, `payload`, `time`).
- `CaddyModule()` — returns the module ID `http.handlers.nuts`.
- Interface guards ensuring `Handler` satisfies `caddy.Module`, `caddy.Provisioner`, `caddy.Validator`, `caddy.CleanerUpper`, `caddyhttp.MiddlewareHandler`, and `caddyfile.Unmarshaler`.

### provision.go

Caddy lifecycle management — connecting to NATS and tearing down on shutdown. Contains:

- `Provision()` — applies defaults, calls `connectNATS()`, creates a JetStream context, verifies the configured stream exists, and rolls back on failure.
- `connectNATS()` — builds NATS connection options (reconnect, auth, lifecycle callbacks) and dials the server.
- `Cleanup()` — mutex-protected teardown: closes the NATS connection and nils cached state.
- `Validate()` — checks required fields (`nats_url`, `stream_name`), rejects conflicting auth methods, warns about wildcard CORS with credentials.

### serve.go

HTTP/SSE request handling — the core streaming loop. Contains:

- `ServeHTTP()` — handles OPTIONS (CORS preflight), non-GET passthrough, topic extraction and validation, `Last-Event-ID` / `last-id` replay parsing, JetStream subscription creation with sequence-purge fallback, the SSE streaming `select` loop (message delivery, slow-client disconnect, heartbeat, context cancellation), and oversized-event guards.
- `setCORSHeaders()` — matches the request `Origin` against `AllowedOrigins` and echoes it back (or `*`), plus the required `Access-Control-*` headers.

### caddyfile.go

Caddyfile configuration parsing. Contains:

- `UnmarshalCaddyfile()` — maps every supported directive (`nats_url`, `nats_credentials`, `nats_token`, `nats_user`, `nats_password`, `topic_prefix`, `allowed_origins`, `heartbeat_interval`, `reconnect_wait`, `max_reconnects`, `stream_name`, `max_event_size`) to the corresponding `Handler` field.
- `parseCaddyfile()` — adapter that Caddy calls to turn a Caddyfile block into a `Handler` instance.

### helpers.go

Standalone utility functions used across the module. Contains:

- `toJSON()` — marshals any value to a JSON string.
- `tryParseJSON()` — attempts to unmarshal `[]byte` as JSON; falls back to a plain string.
- `writeSSEChunk()` — writes one SSE frame to the `http.ResponseWriter` and flushes.
- `isValidTopic()` — rejects empty, overlength (>256), wildcard (`*`, `>`), system (`$`-prefix), control-char, and double-dot topics.
- `redactURL()` — strips embedded credentials from a URL before it is logged.

### cmd/caddy/main.go

Build entry point. Imports the standard Caddy modules plus this module (`github.com/ideaconnect/nuts`) so `go build` produces a Caddy binary with NUTS baked in.

### nats_test.go

Unit and integration tests. Uses an embedded NATS server with JetStream to test validation, Caddyfile parsing, topic filtering, SSE streaming, replay, slow-client disconnect, oversized-event dropping, and cleanup.

### functional_test/main_test.go & steps_test.go

BDD functional tests driven by [Godog](https://github.com/cucumber/godog). Scenarios in `features/*.feature` are executed against a real Docker Compose stack (NATS + Caddy with NUTS).

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
2. **Topic validation** — `serve.go` validates the topic via `isValidTopic()`, prepends `TopicPrefix`, and checks for a `Last-Event-ID` header or `last-id` query parameter.
3. **JetStream subscription** — `serve.go` creates an ephemeral JetStream consumer bound to the configured stream. New clients get `DeliverNew()`; reconnecting clients get `StartSequence(lastID+1)` with a `DeliverAll()` fallback if the sequence was purged.
4. **SSE streaming loop** — a single `select` in `serve.go` multiplexes:
   - **`msgChan`** — incoming NATS messages are JSON-wrapped into `messageEventPayload`, tagged with the JetStream sequence as the SSE `id:` field, checked against `MaxEventSize`, and flushed to the client via `writeSSEChunk()`.
   - **`slowClient`** — if `msgChan` is full (buffer of 64), the next message triggers an immediate disconnect to protect the server.
   - **`heartbeat.C`** — periodic SSE comment (`: heartbeat <timestamp>`) keeps the connection alive through proxies/load balancers.
   - **`ctx.Done()`** — client disconnect; subscriptions are cleaned up.
5. **Client reconnect** — the browser's `EventSource` automatically reconnects and sends the last received `id:` as `Last-Event-ID`, resuming from where it left off.
