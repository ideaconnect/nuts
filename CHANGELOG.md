# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- New Caddyfile directive `health_path` (default `/healthz`) to customize
  the health-check endpoint.
- New Caddyfile directives `nats_tls_ca`, `nats_tls_cert`, `nats_tls_key`,
  `nats_tls_insecure_skip_verify` for mutual TLS to NATS.
- New Caddyfile directive `allowed_headers` (default
  `Cache-Control, Last-Event-ID`) to configure CORS request headers.
- New Caddyfile directive `allowed_methods` (default `GET, OPTIONS`).
- New Caddyfile directive `max_connections` (default `0`, meaning unlimited)
  with `Retry-After: 5` rejection and a
  `nuts_connections_rejected_total{reason}` Prometheus counter.
- New Caddyfile directive `client_buffer_size` for the per-connection send
  buffer (default `64`).
- New Caddyfile directive `replay_max_messages` (default `0`, unlimited)
  to cap how many events a single client receives on a replay fallback,
  with a new `nuts_replay_cap_reached_total` Prometheus counter.
- New Caddyfile directive `replay_window` (default `0`, all retained) to
  bound the replay fallback to the last N seconds via NATS `StartTime`
  instead of `DeliverAll`.
- Replay fallback now fires when the requested `last-id` is below the
  stream's retained range (previously the JetStream subscribe only
  silently started at `FirstSeq`; this flag-lit fallback enables the cap
  and window directives above and updates `nuts_replay_fallbacks_total`).
- `CONTRIBUTING.md`, `SECURITY.md`, and this `CHANGELOG.md`.
- Docker image now runs as non-root user `nuts` (uid 10001).

### Changed
- `Cleanup()` now signals every in-flight SSE handler via a handler-scoped
  `shutdown` channel, so on Caddy reload or shutdown active clients return
  within milliseconds instead of waiting up to `heartbeat_interval` seconds
  for the next tick to discover the torn-down NATS subscription.
- An unparseable `Last-Event-ID` HTTP *header* is now logged and the stream
  resumes with `DeliverNew` instead of returning `400`. An explicit
  `?last-id=` query parameter still returns `400` when malformed.
- `Provision()` validates required fields before opening a NATS connection.
- Tightened `isValidTopic` to the NATS token charset `[A-Za-z0-9._-]` and
  rejects leading/trailing/consecutive dots.
- Caddyfile integer directives use `strconv.Atoi` so `123abc` is rejected.
- `MaxEventSize < 0` disables the event size limit; `0` uses the 1 MiB
  default; `> 0` is honored as the limit.
- Path-shorthand now converts `/orders/new` to topic `orders.new` instead
  of producing an invalid topic with `/`.
- Default [Caddyfile](Caddyfile) drops the duplicate `route /*` block, adds
  `uri strip_prefix /events`, and raises `heartbeat_interval` to `30`.
- README Caddyfile snippets (Quick Start, Docker Compose, Prometheus metrics)
  now include `uri strip_prefix /events` inside `route /events*` so the
  documented path-shorthand JS example (`new EventSource('/events/my-topic')`)
  produces topic `my-topic` instead of `events.my-topic` once the handler's
  `topic_prefix` is applied.
- Removed the top-level `## Testing` section from the README; the
  `## Development > Running Tests` section covers the same ground in more
  depth and was duplicating the three `make test*` bullets.
- README CORS section splits the wildcard vs. explicit-origins examples
  into two separate fenced blocks and calls out that a second
  `allowed_origins` directive inside the same `nuts { }` block replaces
  the first (previously a single fence showed both forms, inviting a
  copy-paste that silently dropped the wildcard line).
- README Quick Start pins the NATS image to `nats:2.12-alpine` instead
  of `nats:latest` so copy-pasting the snippet on two different days
  can't yield two different NATS versions, matching the Docker Compose
  example already in the same document.
- README "Environment variables" section names the files explicitly
  (`Caddyfile`, `Caddyfile.test`, the root `docker-compose.yml`, and
  `example/docker-compose.yml`) instead of the ambiguous "the
  docker-compose.yml next to it", and notes that `example_docker/`
  leaves the three variables at their defaults.
- `Caddyfile.test` is now indented with tabs to match the root
  `Caddyfile` and Caddy's own `caddy fmt` output, so `caddy adapt`
  no longer logs `Caddyfile input is not formatted` on every run.
- Dockerfile uses BuildKit cache mounts for `go mod download` and `go build`.

### Fixed
- [`Caddyfile`](Caddyfile) and [`Dockerfile.test`](Dockerfile.test) each had
  two and three stale revisions concatenated into a single file, so
  `caddy adapt` refused to load the root Caddyfile (`server block without
  any key is global configuration, and if used, it must be first`) and the
  test image's final stage was the older non-hardened variant that ran as
  root. Both files now contain the single intended revision.
- Slow-client overflow can no longer silently discard the disconnect signal.
  Previously, when both the per-connection send buffer and the `slowClient`
  signal channel were full, further overflows hit a nested `default` that
  dropped the signal on the floor, leaving the session ostensibly connected
  with a saturated buffer. The inner `default` is replaced with a wait on
  the handler's `done` channel so every overflow resolves to either a
  disconnect (after which JetStream replays on reconnect) or a clean
  teardown — never a silent stall.
- `MaxReconnects 0` is now honored as "no reconnects" from both Caddyfile and
  JSON config. The field changed to `*int` so that an explicit `0` in JSON no
  longer collides with Go's zero value and is no longer silently rewritten to
  the default. When the directive is omitted, the default `-1` (unlimited)
  is used.
- CORS: `Access-Control-Allow-Credentials: true` is now only advertised when
  the request `Origin` is explicitly listed in `allowed_origins`. Wildcard
  (`*`) matches no longer attach credentials — browsers would reject a
  credentialed `EventSource` from an unlisted origin anyway, and the old
  behaviour effectively disabled CSRF protection when `allowed_origins *`
  was combined with cookie-based auth at a reverse proxy. Responses that
  echo the request origin now also include `Vary: Origin`.
- Health check path uses a proper suffix match and no longer collides with
  topic shorthand.
- `json.NewEncoder(...).Encode(...)` errors in the health check are logged
  instead of silently discarded.

### Security
- `Validate()` warns when `nats://` is used with credentials (cleartext
  auth over the network) and when `nats_tls_insecure_skip_verify=true`.
- JSON parse is skipped for oversized payloads to avoid unbounded
  allocation for hostile producers.

## [0.x] - prior history

Initial development — see Git history.
