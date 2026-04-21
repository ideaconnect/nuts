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
- `CONTRIBUTING.md`, `SECURITY.md`, and this `CHANGELOG.md`.
- Docker image now runs as non-root user `nuts` (uid 10001).

### Changed
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
- Dockerfile uses BuildKit cache mounts for `go mod download` and `go build`.

### Fixed
- `MaxReconnects 0` is now honored as "no reconnects". When the directive is
  omitted, the default `-1` (unlimited) is used.
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
