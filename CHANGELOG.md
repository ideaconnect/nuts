# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Planned as the next **MAJOR** release. Each SSE stream is now backed by an
ordered **pull** consumer instead of a push consumer. That removes a class of
silent message loss, and clients that fall behind no longer get disconnected.
Read **Changed** before upgrading. **nats-server 2.10 or newer is required.**

### Added
- **`shared_subscriptions`** (off by default) lets connections that are
  caught up with the live stream share one JetStream consumer per topic set,
  pulling and formatting each message once (#119). A connection with history
  to replay catches up on its own consumer and then joins; one that falls
  more than `client_buffer_size` frames behind returns to its own consumer
  until it catches up. Hand-offs go by stream sequence, so nothing is skipped
  or repeated, and nobody is disconnected for falling behind. Without it,
  every connection's consumer sends its own copy of each message over NUTS'
  single NATS connection: 1000 connections receiving 4 × 64 KiB took about
  14 s, most of it recovering from nats-server's slow-consumer disconnects
  (#118). With it, 0.25 s. New metrics: `nuts_shared_subscriptions` and
  `nuts_shared_transitions_total{transition}`.
- A request refused because the stream reached its consumer limit is counted
  as `nuts_connections_rejected_total{reason="stream_consumer_limit"}` and
  logged with `disconnect_reason=stream_consumer_limit` (#110). NUTS also
  warns at startup when the stream's `max_consumers` is below
  `max_connections`.
- NUTS warns at startup when connected to a nats-server older than 2.14.7,
  which can silently skip messages on multi-topic subscriptions (#111).
- A NATS server entering lame duck mode is logged and counted as
  `nuts_nats_connection_events_total{event="lame_duck"}` (#73).
- Consumer-creation failures log the JetStream error code as
  `jetstream_error_code`.

### Changed
- **Breaking: transient failures tell `EventSource` clients to retry** (#105).
  A browser `EventSource` stops reconnecting for good after any answer other
  than a `200` event stream, so a NATS outage or a connection cap used to
  disconnect browsers permanently. Requests with
  `Accept: text/event-stream` now get a `200` stream holding only a jittered
  `retry:` delay (2.5–7.5 s) when JetStream is unavailable, `max_connections`
  or the stream's consumer limit is reached, or the consumer cannot be
  created. The browser then reconnects with its `Last-Event-ID`. Other
  clients keep `503`/`429`, now always with a jittered `Retry-After`
  (3–8 s; `max_connections` used a fixed 5). While the NATS connection is
  down, these answers come at once instead of after the JetStream timeouts
  (about 7 seconds).
- **Breaking: configurations that could never work as intended are
  rejected at load time.**
  - `nats_tls_ca` together with `nats_tls_insecure_skip_verify` (#78): Go
    ignores the CA bundle when verification is off, so the connection
    accepted any certificate while looking pinned.
  - `nats_url` schemes other than `nats`, `tls`, `ws` and `wss` (#77).
    nats.go dials an unknown scheme, such as a mistyped `tsl://`, as
    plaintext `nats://`.
  - `allowed_origins` entries that can never match a browser's `Origin`
    header (#79): empty, comma-joined, with a path or trailing slash, or with
    uppercase letters.
  - `max_topics_per_subscription` below `-1` (#81). Only `-1` turns the cap
    off, which is now logged at startup; other negatives were typos that
    silently disabled it.
- The warnings about credentials sent unencrypted and about
  `nats_tls_insecure_skip_verify` are logged by Provision before NUTS
  connects, rather than by Validate after the credentials have been sent
  (#82). The unencrypted-credentials warning now also covers `ws://`,
  servers without a scheme and credentials embedded in `nats_url`, and no
  longer fires when `nats_tls_*` directives make nats.go use TLS.
- Negative `heartbeat_interval` and `reconnect_wait` values are rejected when
  the Caddyfile is parsed, with the directive's location (#80).
- A failed consumer delete is logged with the stream's topics and the
  consumer name (#84).
- **Breaking: server control messages are no longer forwarded** (#112).
  Subject delete markers (`Nats-Marker-Reason`) and message schedule
  definitions (`Nats-Schedule`) used to reach clients as ordinary, often
  empty, message events. They are skipped and counted as
  `nuts_messages_dropped_total{reason="control_message"}`. Messages produced
  by a schedule are delivered as before.
- **Breaking: the JetStream delivery pipeline is pull-based** (#116). Every SSE
  request creates an ordered pull consumer (nats.go `jetstream` package), named
  `nuts_<id>_<n>`. It is deleted when the stream ends, instead of lingering
  until `InactiveThreshold`. A feed goroutine pulls up to `client_buffer_size`
  messages ahead and hands them to the SSE writer one at a time. While the
  writer is busy, pulling stops and the backlog waits in JetStream.
- **Breaking: a slow client is now one whose write misses `write_timeout`.**
  A full per-connection queue no longer disconnects the client, because the
  queue can no longer overflow. `nuts_slow_client_disconnects_total` and
  `disconnect_reason=slow_client` now mean "a write hit its deadline".
- **Breaking: `write_timeout` defaults to 30 seconds** (previously 0, meaning
  no deadline). With backpressure, a write that cannot complete is the only
  sign of a client that stopped reading, so it needs a bound by default. `0`
  or omitted now means the default; `-1` disables the deadline (#85).
- **Breaking: `client_buffer_size` is the per-connection prefetch** from
  JetStream (default 64). It bounds per-connection memory at roughly
  `client_buffer_size` × the largest message.
- **Breaking: nats-server 2.10 or newer is required.** The pre-2.10
  multi-topic fallback (subscribe to a common wildcard and filter in NUTS) is
  removed, along with the server-version sniffing that chose it. Multi-topic
  requests always use server-side `FilterSubjects` (#117). nats-server 2.9 has
  been end-of-life since 2024, and keeping the fallback meant keeping a second
  delivery path that nothing in the matrix could exercise alongside the new
  consumer.
- **Breaking: the stream's subjects are checked for every request.** A topic
  outside them now gets `503 Failed to subscribe to requested topics` for
  single-topic requests too. Current nats-servers would otherwise create a
  consumer that silently delivers nothing.
- The `connected` event now carries an `id:` (#101). For requests without a
  cursor it is the stream's last sequence when the consumer was created (`0`
  on an empty stream); for replay requests it is the requested cursor. Those
  requests start at an explicit sequence instead of `DeliverNew`.
- **Breaking: a valid `Last-Event-ID` header wins over `?last-id=`** (#102).
  `EventSource` resends the URL it was created with, `?last-id=` included, on
  every auto-reconnect. Because the query used to win, a client that resumed
  from a stored id replayed from that same id on every reconnect, and with
  `replay_max_messages` it never got past the first N messages. A malformed
  `?last-id=` is still rejected with `400`. A malformed header is logged and
  ignored, and the query cursor is used instead.
- **Breaking: probe paths ignore one trailing slash** (#83). `GET /livez/`,
  `/readyz/` and `/healthz/` (also under a route prefix) now answer as probes.
  Before, they were treated as a subscription to a topic named `livez`, so
  kubelet probes with a trailing slash failed with `503`. Use
  `?topic=livez` to subscribe to such a topic.
- The test matrix covers nats-server 2.10, 2.12, 2.14 and 2.15, and the
  Docker Compose files, CI and examples default to `nats:2.15-alpine` (#109).
  The `nats stream add` examples set `--max-consumers`.
- Metric Help strings list their label values, and the documentation
  describes the pull pipeline throughout: architecture, configuration,
  operations runbooks, troubleshooting, performance budgets and the memory
  formula (#70, #71). A test keeps every metric name in the docs and ops
  files in step with the registered metrics.
- The example alert rules and Grafana dashboard cover the stream consumer
  limit, consumer recoveries, oversized drops by reason, write failures by
  site, NATS slow consumers and lame duck mode (#86).
- `nuts_nats_connection_events_total{event="closed"}` no longer counts the
  connection NUTS closes itself on shutdown or reload (#73).
- `nats_idle_heartbeat` now sets the heartbeat of every pull request. When
  heartbeats stop, the ordered consumer recreates itself. `-1` is still
  accepted but no longer disables anything: the library default (5s) applies,
  and a warning is logged (#74).
- `nuts_consumer_invalidated_total{reason}` now counts `recreated` (the consumer
  recovered on its own) and `unrecoverable` (recreation failed and the stream
  closed with `disconnect_reason=consumer_unrecoverable`). The old
  `heartbeat_missed`/`slow_consumer` labels are gone.
- `nuts_replay_fallbacks_total` counts fallback replays once the consumer
  exists, rather than attempts (#71 D).
- Provision's stream check is bounded to 2 seconds (#72), and every JetStream
  API call has a default deadline.

### Security
- Credentials in `nats_url` were logged in clear when they appeared in a
  server after the first of a comma-separated list, or in a server given
  without a scheme (`user:pass@host:4222`). Every server of the list is now
  redacted, and so are the credentials of a URL that does not parse.

### Deprecated
- `dispatch_timeout` has no effect, because there is no queue hand-off left to
  time out. Setting it logs a warning. It will be removed in the next major
  release.
- `nuts_dispatch_timeout_total` and `nuts_wildcard_filter_drops_total` stay
  registered at zero for dashboard compatibility, and will be removed in the
  next major release.

### Fixed
- **Messages in flight when NUTS' NATS connection dropped were lost for good**,
  and the SSE stream continued with a hole that `Last-Event-ID` could not
  recover (#99). The ordered consumer now resumes after the last message it
  actually delivered.
- **Replays and publish bursts larger than `client_buffer_size` disconnected
  even fast clients** as "slow" (#100). Catching up on a 3000-message backlog
  took about 900 reconnects; it now completes on one connection.
- **A client that reconnected before receiving its first message lost
  everything published in between** (#101), for example after a Caddy reload.
- **A consumer reaped or deleted under a live stream left the stream open and
  silent** (#54). It is now recreated from the last delivered sequence without
  the client noticing (#53).
- **A cursor ahead of the stream parked the client in the future** (#103).
  After a stream was recreated or restored from a backup, a stored
  `Last-Event-ID` could point past the new stream's last sequence. The
  consumer then waited silently until the stream reached that sequence and
  skipped everything before it. Such cursors now fall back to the retained
  replay (`replay_window` when configured).
- **`replay_window` and `replay_max_messages` failed open when the stream info
  could not be read at request time** (#98). A stale cursor then replayed the
  whole stream. The window now falls back to `now - replay_window`, and the
  cap counts the backlog pending behind the first replayed message.
- **The `replay_window` filter also dropped live messages** (#106), for example
  from a mirror catching up or with clock skew between servers. It now applies
  to replayed history only, and each drop is counted in
  `nuts_messages_dropped_total{reason="replay_window"}`.
- **A deleted message at the resume point re-sent history** under
  `replay_window` (#115). Its publish time could not be read, so the request
  fell back to the time window and replayed messages the client already had.
  NUTS now dates the resume point by the next retained message.
- `replay_max_messages` no longer counts live messages published during a
  replay, including after a reconnect to an empty stream (#98).
- **Every request failed on a stream whose `consumer_limits.inactive_threshold`
  is below 30 seconds** (#113), with `503` while the readiness probe stayed
  green: the server refused the consumer's fixed 30-second threshold (error
  10153). NUTS now uses the stream's limit when it is shorter.
- **A reload left every open stream's consumer on the server** until its
  inactive threshold expired (#75). Cleanup now waits up to 3 seconds for the
  streams it ends to delete their consumers before closing the connection.
- Consumer deletes are skipped while the NATS connection is down, instead of
  holding up Cleanup until they time out; the server removes those consumers
  after their inactive threshold.
- The NATS closed callback no longer runs after Cleanup has returned, when
  Caddy has already unloaded the handler (#73).
- A slow-client overflow race could write a message after an earlier one was
  dropped (#104). The NATS callback could also stay parked for good behind a
  stalled writer, with messages accumulating up to nats.go's 64 MiB pending
  limit (#85). Both code paths are gone.
- Oversize payloads are dropped before they are queued for the writer, and the
  drop log now names the stream sequence (#107, #120).

### Removed
- The subscribe-time replay fallback, which could never run: nats-server
  clamps a below-retention start sequence instead of returning an error (#97).
- The unreachable `subscription_empty` rejection branch (#69).

## [0.4.3] - 2026-09-26

Bug-fix and security release. Upgrade urgency: **high** if Caddy runs with
access logging (`log`) or HTTP request metrics, where every SSE request
failed; **medium** otherwise, for the Go standard-library fixes. No directive,
JSON field, metric or response-format changes. This release also ships the
Docker image that v0.4.2 never got.

### Fixed
- **SSE streams returned `500 Streaming not supported` whenever Caddy access
  logging or HTTP request metrics were enabled** (#114). With the `log`
  directive, or the global `metrics` option, Caddy wraps the response writer in
  a recorder that implements `FlushError()` and `Unwrap()` but not
  `http.Flusher`, and the handler's `w.(http.Flusher)` check rejected every
  stream. Flushing and write deadlines now go through
  `http.NewResponseController`, which follows the wrapper chain. The
  functional-test Caddyfile now enables `log`, so the Godog suite runs through
  the wrapper.
- A failed flush is now reported as a write error, so the stream closes and
  `nuts_messages_delivered_total` counts only frames that were flushed.
  Previously the flush error was discarded and the failure surfaced only on
  the next write, up to `heartbeat_interval` later.

### Security
- Go toolchain 1.26.4 → **1.26.8** (`go.mod`, both Dockerfiles). This clears
  the reachable stdlib vulnerabilities `govulncheck` reported in `net/http`
  (GO-2026-6089, GO-2026-5026), `crypto/tls` (GO-2026-6090, GO-2026-5856),
  `net/url`, `html/template`, `encoding/xml`, `encoding/asn1` and `os` (#108).
- Indirect modules bumped past their advisories: `google.golang.org/grpc`
  1.84.0 (GO-2026-6348, GO-2026-6061), `golang.org/x/text` 0.42.0
  (GO-2026-5970), `go.opentelemetry.io/otel` 1.44.0 (GO-2026-5158) (#108).
- Embedded test server `nats-server/v2` 2.14.2 → 2.15.0. This test-only
  dependency carried CVE-2026-58207 and CVE-2026-58210 (high) among others
  (#109).

### Changed
- `github.com/nats-io/nats.go` 1.52.0 → 1.54.0. It fixes a `DecodeHeadersMsg`
  panic on malformed status lines (nats.go#2101). Also bumped:
  `prometheus/client_golang` 1.24.1, `client_model` 0.6.3 and `godog` 0.16.0
  (#109).
- CI runs `govulncheck -test ./...`, so test-only dependencies are scanned too
  (#109).
- The release workflow waits for CI to succeed on the tagged commit before
  GoReleaser publishes anything. v0.4.2 published archives while CI was red,
  and its Docker image was never pushed (#108).

### Notes
- **v0.4.2 has no Docker image.** Its CI run failed `govulncheck`, so the
  image jobs never ran. Its release archives were built with Go 1.26.4.
  Upgrade to this release instead.

## [0.4.2] - 2026-09-23

Project-site release. **No module code changes** — the `nuts` handler
behaviour, Caddyfile directives, JSON fields, and metrics are identical to
0.4.1. Upgrade urgency: none.

### Added
- Website: opt-in cookie consent banner with Google Analytics 4 loaded only
  after the visitor accepts (prior-consent model; nothing contacts Google on
  reject or no choice), withdrawable via the footer's *Cookie preferences*,
  and a new [privacy page](https://idct.tech/nuts/privacy/). Four anonymous
  interaction events (install-command copy, GitHub link, contact submit,
  features-page scroll) fire only under that consent.

### Changed
- Website: sponsor and Buy-Me-a-Coffee header icons sit on white chips, and the
  hero GitHub button is icon-only for a better fit on narrow phones.
- The project website at <https://idct.tech/nuts> is now served from Cloudflare
  instead of GitHub Pages: a static-assets Worker bound to `idct.tech/nuts/*`
  ([website/wrangler.jsonc](website/wrangler.jsonc)), deployed by
  [.github/workflows/website.yml](.github/workflows/website.yml) on the same
  triggers as before, which now also verifies the live URL serves the build.
  The URL is unchanged. The website toolchain moves from Node 20 to Node 22.

## [0.4.1] - 2026-07-05

Documentation and project-site release. **No module code changes** — the `nuts`
handler behaviour, Caddyfile directives, JSON fields, and metrics are identical
to 0.4.0. Upgrade urgency: none.

### Added
- Project marketing/documentation website integrated in-repo under `website/`
  (Jekyll + Tailwind CSS), published to <https://idct.tech/nuts> via GitHub Pages
  on tagged releases ([.github/workflows/website.yml](.github/workflows/website.yml)).
  Containerised local build with no host Ruby/Node required:
  `make website-build` / `make website-serve` / `make website-clean`.

### Fixed
- Website documentation brought in line with the current module: the
  `max_connections` rejection is documented as `429 Too Many Requests` (not the
  pre-0.4.0 `503`), the `nats_idle_heartbeat` directive is added, and the
  Prometheus metrics reference is completed (all 16 `nuts_*` series). Corrected
  the README project-site link to `https://idct.tech/nuts` and the
  `max_connections` status note in `AGENTS.md`.

### Changed
- `.dockerignore` excludes `website/` from the production image build context so
  website-only edits no longer invalidate the Go builder-layer cache. The
  released `idcttech/nuts` image is unaffected (multi-stage build copies only the
  binary).

## [0.4.0] - 2026-06-19

### Added
- **M9 Batch A — JetStream consumer-invalidation detection.** Closes
  the first half of a two-batch milestone (#9) addressing a silent-
  failure mode where a server-side ephemeral consumer reaped by
  InactiveThreshold during a network blip, or one lost across a
  leafnode route failover, would stay attached to its SSE handler
  until the client reconnected for unrelated reasons. The SSE-layer
  heartbeat ticker only proves the HTTP socket is open; it cannot see
  a wedged JetStream push path. Batch A surfaces the failure as
  metrics + structured logs; Batch B (still open) will terminate the
  affected SSE handler with `disconnect_reason=consumer_invalidated`
  so the client reconnects with `Last-Event-ID` against a fresh
  consumer.
  - New `nats_idle_heartbeat <seconds>` Caddyfile / JSON knob (default
    10s, default-on). Sets `nats.IdleHeartbeat(...)` on every
    ephemeral JetStream push consumer so nats.go can detect a wedged
    push path even on a quiet stream. `-1` is the explicit operator-
    disable sentinel; `0` is normalised to 10s by Provision. Validate
    enforces the upper bound `< InactiveThreshold/2` (currently 15s)
    so two missed heartbeats are detectable before the server reaps.
  - New `consumer_invalidated` label value on
    `nuts_nats_async_errors_total{kind}`. Covers three distinct
    nats.go error paths that all indicate the JetStream push
    consumer is unusable: `nats.ErrConsumerNotActive` (sentinel,
    raised by nats.go's `activityCheck` when IdleHeartbeat tolerance
    elapses without a heartbeat arriving — the primary failure mode
    M9 targets), `nats.ErrConsumerDeleted` (sentinel, raised when
    the consumer was administratively deleted), and
    `*nats.ErrConsumerSequenceMismatch` (typed struct, raised when
    heartbeats arrive but the delivered sequence drifted). All
    three previously routed to the generic `"other"` bucket. The
    initial Phase 1 implementation only covered the typed
    `ErrConsumerSequenceMismatch` case; the heavy Batch A test gate
    (#52) uncovered that the primary heartbeat-miss path actually
    surfaces as `ErrConsumerNotActive` and was never reaching the
    metric — without the gate the label would have shipped with
    zero counts in production. Classifier uses both `errors.Is`
    (for the sentinel paths) and `errors.As` (for the typed
    struct); dedicated regression tests pin both contracts.
  - New `nuts_consumer_invalidated_total{reason}` CounterVec
    registered (declaration-only in Batch A; Batch B populates it).
    Reason labels: `heartbeat_missed`, `slow_consumer`. Pre-
    registration so /metrics exposes a zero-counted series before
    dashboards reference it.
  - New `docs/OPERATIONS.md` incident playbook section "Consumer
    invalidated mid-stream" documenting the Batch A signal-only
    contract and the Batch B disconnect contract.
- README `Compatibility` table (Go, Caddy, NATS minimum tested) and a
  `Versioning policy` section documenting semver discipline, deprecation
  and removal cadence, and the `:latest` Docker-tag warning.
- Expanded `.github/dependabot.yml` with labels (`dependencies`, `go`/`ci`/`docker`),
  per-ecosystem `commit-message` prefixes (`deps(go)`, `deps(actions)`,
  `deps(docker)`), and security-vs-version-update group splitting so
  CVE-fix PRs aren't held back by churning minor bumps. Same weekly
  Monday-04:00-UTC cadence so bumps land before the Sunday-03:00-UTC
  mutation workflow re-tests the resulting tree.
- New nightly fuzz workflow (`.github/workflows/fuzz.yml`) — five
  matrix jobs, one per `Fuzz*` target in `fuzz_test.go`
  (`FuzzIsValidTopic`, `FuzzIsValidTopicFilter`, `FuzzIsValidCookieName`,
  `FuzzSubjectMatchesFilter`, `FuzzSubscriberTopicMatches`). Default 5
  min per target; `workflow_dispatch` accepts a custom `fuzztime`
  input. Crashing inputs are uploaded as artifacts so the next
  maintainer can reproduce locally and convert them into seeds.
- New live-handshake mTLS integration test
  (`TestHandler_ConnectNATS_TLS_LiveHandshake`) drives `connectNATS`
  against an embedded TLS-enabled NATS server, asserting both
  positive (handshake succeeds with `InsecureSkipVerify` against a
  self-signed CN-only cert, proving `nats.Secure(tlsCfg)` wires the
  TLS layer) and negative (hostname-mismatch rejection when the same
  cert is loaded as a trust root) paths. Catches regressions where
  `nats.Secure(tlsCfg)` is unwired (e.g. replaced with
  `nats.RootCAs(...)`), which would pass every prior
  `buildTLSConfig`-only test.
- New `codecov.yml` with project (`auto` target, 0.5% threshold) and
  patch (80% target) coverage gates so PR-time line-coverage
  regressions surface as Codecov status checks, complementing the
  weekly MSI gate in `mutation.yml`.
- `Makefile`'s `test-functional` / `test-functional-dev` targets
  honour `FUNCTIONAL_TEST_RACE=1`, which appends `-race` to the
  `go test` invocation. CI runs one functional pass under `-race`
  against `nats:2.12-alpine` to catch broker-timing-dependent races
  the root-package `-race` step can't see.
- `TestHandler_PlanSubscriptionSelectsReplayModes` gains two sub-tests
  pinning the `plan.Replay.HasSnapshot` propagation introduced in
  pass 7. A regression that dropped or inverted the assignment would
  fail at the unit layer before reaching mutation testing or the
  JetStream integration suite.
- New Prometheus counter `nuts_nats_async_errors_total{kind}` populated
  by a registered `nats.ErrorHandler`. Kinds: `slow_consumer`, `timeout`,
  `connection_state`, `other`. Surfaces `nats.ErrSlowConsumer` drops that
  previously hit nats.go's default stderr printer with no metric or
  structured log.
- New Prometheus counter `nuts_write_disconnects_total{site}` labelled
  by SSE write site (`connected`, `message`, `heartbeat`). All three
  write-error sites also upgraded from Debug to Warn so default-level
  log piles surface client-side disconnects driven by `write_timeout`.
- New Prometheus counter `nuts_wildcard_filter_drops_total` for the
  multi-topic wildcard fallback's client-side filter. Non-zero means a
  NATS server older than 2.10 is delivering subjects the client did not
  request and NUTS is filtering them in-process.
- Ephemeral JetStream consumers now set an explicit
  `nats.InactiveThreshold` (default 30s, see
  `defaultConsumerInactiveThreshold`) so server-side consumer state is
  reaped promptly under reconnect churn instead of relying on
  nats-server's 5s default.
- New Prometheus counter `nuts_readiness_failures_total{cause}` labelled
  by readiness-probe degradation cause (`nats_disconnected`,
  `jetstream_missing`, `stream_info_error`). Previously `/readyz`
  silently returned 503 with no log line and no metric, so an
  orchestrator pulling pods out of rotation gave operators no
  Prometheus signal explaining why.
- New Prometheus counter `nuts_nats_connection_events_total{event}`
  incremented from the registered `DisconnectErrHandler`,
  `ReconnectHandler`, and `ClosedHandler`. `event` is one of
  `disconnect`, `reconnect`, `closed`. Closes the flap-detection gap:
  a clean broker-restart cycle never went through the async
  ErrorHandler, so the existing `nuts_nats_async_errors_total{kind=
  connection_state}` did not move on plain Disconnect+Reconnect.
- `nuts_connections_rejected_total{reason}` now also fires for each
  subscriber-JWT rejection path: `auth_missing_token` (no Authorization
  header or malformed shape), `auth_invalid_token` (signature, expiry,
  or claim verification failure), and `auth_topic_forbidden` (token
  valid but the `subscribe` claim does not cover a requested topic).
  An attacker probing the auth surface now leaves a Prometheus
  footprint that `ops/prometheus-alerts.yml` watches via the new
  `NutsAuthRejectionsHigh` alert.
- `ops/prometheus-alerts.yml` gains three alerts:
  `NutsNATSBrokerFlapping`, `NutsReadinessProbeFailing`,
  `NutsAuthRejectionsHigh` — each keyed on the new counters above.

### Changed
- **Breaking metric format.** `nuts_messages_dropped_total` is now a
  labelled counter `nuts_messages_dropped_total{reason}` so operators
  can distinguish raw-NATS oversize (`reason=raw_payload`) from SSE-
  envelope oversize (`reason=formatted_sse_message`). Update PromQL
  queries (`sum(nuts_messages_dropped_total)` continues to work
  unchanged; queries that asserted the unlabelled series specifically
  must add `{reason=~".+"}` or similar).
- CORS headers (`Access-Control-Allow-Origin`, `Access-Control-Allow-
  Methods`, `Access-Control-Allow-Headers`, `Access-Control-Allow-
  Credentials`, `Vary: Origin`) now apply to every response including
  400, 401, 403, 405, and 503 paths. Previously only the SSE stream
  and OPTIONS preflight set them, so browsers translated auth and
  validation failures into opaque CORS errors instead of the real
  status code.
- The readiness probe's `StreamInfo` call now passes
  `nats.MaxWait(1s)` (`defaultReadinessProbeTimeout`) so a partially-
  degraded JetStream cluster cannot stall the probe past the
  orchestrator's readiness budget.
- `Provision()`'s failure-cleanup defer is now registered before
  `connectNATS` so an early connect failure runs `Cleanup()` instead of
  leaking the just-created shutdown channel. `connectNATS` errors are
  promoted to the shared `provisionErr` so the defer fires.
- Error wrapping discipline: `fmt.Errorf` call sites in `provision.go`
  use `%w` instead of `%v` so `errors.Is`/`As` work for callers.
- `interface{}` → `any` in production code (`auth.go`, `helpers.go`,
  `handler.go`).
- README `docker-compose` snippet for NATS now includes `-m 8222` in the
  command so the documented healthcheck on port 8222 actually passes.
  Without it the depends-on health gate blocked forever.
- README build-from-source Go version raised from `1.26.2+` to
  `1.26.4+` to match `go.mod`. `CONTRIBUTING.md` aligned to the same
  floor in the same release.
- README gains a `Ephemeral consumer hygiene` section explaining the
  30 s `InactiveThreshold` operator tradeoff (reconnect-storm protection
  vs the previous nats-server 5 s default).
- README gains a `Source precedence and malformed-cursor handling`
  subsection under the replay docs that documents the contract:
  `?last-id=` query takes precedence over the `Last-Event-ID` header,
  malformed `?last-id=` returns 400, malformed `Last-Event-ID:` logs
  at Warn and falls back to `DeliverNew` so browser auto-reconnect
  doesn't loop forever.
- Versioning policy clarifies the SemVer §4 carve-out for the 0.x
  series — pre-1.0 MINOR releases MAY include breaking changes (the
  `nuts_messages_dropped_total{reason}` labelled-counter migration is
  the current example).
- `docs/OPERATIONS.md` runbook updated with: (1) cross-reference from
  the slow-consumer incident to `nuts_nats_async_errors_total{kind=
  slow_consumer}`; (2) new `Stalled writes` section keyed on
  `nuts_write_disconnects_total{site}`; (3) new `Wildcard-fallback
  overhead on pre-2.10 NATS` section keyed on
  `nuts_wildcard_filter_drops_total`; (4) new `Oversized messages
  dropped` section covering both `nuts_messages_dropped_total{reason}`
  values.

### Fixed
- README documents the probe-path suffix-match semantic and the
  topic-shorthand collision risk for topics ending in the configured
  probe paths. Operators with conflicting topic names should configure
  unique `health_path`, `live_path`, `ready_path`.
- `nuts_readiness_failures_total{cause}` now keeps its documented 1:1
  contract with /readyz 503 responses. Previously the three cause
  branches were independent `if` blocks, so a missing-runtime probe
  bumped two labels and a stale-`js` + disconnected-conn case could
  bump three — operators summing `sum(rate(...))` saw 2–3× the actual
  probe-failure rate during outages. A one-shot guard now records only
  the first matched cause per response. Response body still reports
  every observed degradation (`nats=disconnected`, `stream=unavailable`).
- `topic_prefix` is validated at config load. Previously `Validate()`
  inspected every other field but ignored TopicPrefix, so a one-character
  Caddyfile typo (e.g. `topic_prefix *.`) composed with the per-request
  topic to silently subscribe every client to a wildcard namespace
  (cross-tenant fan-out). The new check rejects NATS wildcards (`*`,
  `>`), leading `.`, system-subject prefix (`$`), consecutive dots,
  disallowed bytes, and lengths over 256.
- `nuts_subscription_errors_total` now also increments on planning-time
  topic rejection (multi-topic request where at least one requested
  full subject is not allowed by the stream's configured subjects).
  Previously only subscribe-time failures fired the counter, so the
  `NutsSubscriptionErrorsHigh` alert missed deployments that changed a
  stream's allowed subjects.
- NATS reconnect log line now passes `nc.ConnectedUrl()` through
  `redactURL()` to match the startup-log convention. Operators using
  credentialed `nats://user:pass@host` URLs no longer leak credentials
  on each reconnect event.
- The max-connections rejection log line now emits
  `disconnect_reason="max_connections"` to match the convention used
  by every other connection-termination log site (10 others); the
  previous `reject_reason` key was the lone outlier and operator
  dashboards keyed on `disconnect_reason` missed it. The
  `metricsConnectionsRejected{reason="max_connections"}` counter is
  unchanged.
- **`max_connections` rejection status code is now `429` (RFC 6585) instead
  of `503`.** A client-side concurrency cap is distinct from a genuine
  backend outage; using `503` collided with the readiness-probe-degraded
  and subscription-failure paths and tripped client-side circuit
  breakers into opening the circuit when the right reaction is to keep
  retrying with `Retry-After`. The `Retry-After: 5` header and the
  `nuts_connections_rejected_total{reason="max_connections"}` counter
  are unchanged. Operators who scripted retry on `503` should add `429`
  to their accepted retryable-status set.
- `replay_max_messages` no longer enforces against live messages when
  the per-request `StreamInfo` snapshot is unavailable. Previously a
  transient broker blip coinciding with a `?last-id=` reconnect would
  leave `CapSequence=0` and the conservative "count it" branch in
  `countsTowardReplayCap` silently retargeted the cap at live traffic,
  closing the SSE session with `disconnect_reason=replay_cap_reached`
  after N live messages of any age. A new `HasSnapshot` flag on
  `streamInfoSnapshot` and `replayPlan` distinguishes "snapshot
  unavailable" from "snapshot says LastSeq=0" so replay accounting only
  runs when the snapshot was actually observed.
- `?last-id=` cursor cap tightened from `parsedID == maxReplayCursor` to
  `parsedID >= maxReplayCursor-1`. The previous check missed the
  off-by-one input where `parsedID+1` (the JetStream StartSequence)
  lands exactly on the reserved sentinel and JetStream silently parks
  the consumer at a sequence that will never arrive, leaving the
  client with only heartbeats. Query rejections still return 400;
  header values still fall back to `DeliverNew` so browser
  EventSource auto-reconnects don't loop.
- `Validate()` now rejects negative `heartbeat_interval` and
  `reconnect_wait`. Previously these silently fell into Provision's
  `<= 0` normalization branch and were rewritten to defaults, so a
  typo like `heartbeat_interval -30` (intended as `30`) passed
  validation green and the keep-alive cadence reverted to the default.
  The `0`-means-default semantic is preserved for forward
  compatibility.
- `handler.go` `MaxConnections` doc now reads `HTTP 429` (matches
  the implementation post-`8d06acd`); `serve.go`'s CORS-rationale
  comment now lists `429 (max_connections)` separately from genuine
  `503` paths; `docs/CONFIGURATION.md`'s `dispatch_timeout` Notes
  cell uses the unambiguous "leaves the wait unbounded" phrasing
  from `handler.go`. Three doc surfaces that the pass-6 sweep missed.
- NATS cert/key load error now cites both file paths
  (`load nats_tls_cert=<path> nats_tls_key=<path>: <cause>`),
  matching the CA-load error shape so operators don't have to bisect
  which file is malformed.
- `docs/CONFIGURATION.md` `max_connections` row updated from `503` to
  `429 (Too Many Requests, RFC 6585)` — the canonical configuration
  table is now consistent with `handler.go`, `serve.go`, `README.md`,
  and `CHANGELOG.md`. (Last surface missed by the pass-6 status-code
  rotation.)
- CI image-scan tightening: Trivy scan in the `docker` job now runs on
  every push-built image (including `:latest` promoted from `main`),
  not only on tagged releases. A new HIGH/CRITICAL CVE in a base
  image (Alpine, Caddy) can land between PR-merge and the next tag,
  and the previous gate (`if: startsWith(github.ref, 'refs/tags/v')`)
  silently shipped unscanned `:latest` images. The PR-build scan is
  unchanged.
- `govulncheck` invocation pinned from `@latest` to `@v1.1.4` to match
  the pinning convention used for the other CI tooling (gremlins,
  golangci-lint, Trivy).
- Mutation workflow's `gh run list` baseline lookup now hard-codes
  `--branch=main` instead of `${{ github.ref_name }}`. A
  `workflow_dispatch` from a feature branch previously silently
  skipped regression detection because the previous-run lookup
  matched the dispatched branch (usually zero successful runs); now
  it always compares against the canonical main baseline.

### Operator notes
- CORS headers are now emitted on every response with an allow-listed
  `Origin`, **including probe paths** (`/healthz`, `/livez`,
  `/readyz`). Previously probes never set CORS headers. If your
  load-balancer or monitoring scraper sends an `Origin` you don't
  allow-list, behaviour is unchanged.
- The Debug → Warn elevation of all three write-disconnect log sites
  (`connected`, `message`, `heartbeat`) means **every browser tab-close
  mid-stream now produces a Warn-level log entry**. Under heavy client
  churn this can dominate aggregated log volume; consider sampling
  `disconnect_reason="write_error"` entries in your log shipper if the
  signal-to-noise ratio degrades.
- PromQL migration for the `nuts_messages_dropped_total` labelled
  counter: bare-metric queries (e.g. `nuts_messages_dropped_total > 0`)
  now return one time series per reason instead of one in total. Alert
  rules that compared the bare metric must wrap with
  `sum without (reason)` or use `{reason=~".+"}` to keep their previous
  semantics; queries that already used `rate()` / `increase()` are
  unaffected because both functions preserve labels.

## [0.3.0] - 2026-05-21

### Added
- Mutation testing pipeline using
  [gremlins](https://github.com/go-gremlins/gremlins): pinned version via
  the Makefile (`make mutate-tools` / `make mutate` /
  `make mutate-pkg PKG=…`), [`.gremlins.yaml`](.gremlins.yaml) tuning the
  mutator set and quality gates, and a weekly
  [`mutation.yml`](.github/workflows/mutation.yml) GitHub Action that
  uploads each run's JSON report as an artifact and fails the run when
  the Mutation Score Indicator drops by more than 2 percentage points
  versus the prior week. Per-PR enforcement for changes touching
  `auth.go`, `helpers.go`, `handler.go`, `serve.go`, `caddyfile.go`, or
  `provision.go` is documented in [`AGENTS.md`](AGENTS.md) and
  [`CONTRIBUTING.md`](CONTRIBUTING.md) (run `make mutate-pkg`, report
  MSI in the PR, kill / document / flag every new survivor).
- Mutation-testing documentation under [`docs/mutation/`](docs/mutation/):
  baseline, per-file MSI targets, accepted-equivalent survivors log,
  uncovered-code notes, final report, and per-run logs. Current state:
  test efficacy (MSI) 100%, mutation coverage 99.60%,
  501 / 503 mutants killed with 2 documented accepted gaps.
- [`.github/pull_request_template.md`](.github/pull_request_template.md)
  reflecting the mutation-testing per-file requirement and the
  refactor / doc-only / dependency-bump waiver path.

### Changed
- Refactored byte-class predicates in [`auth.go`](auth.go) and
  [`helpers.go`](helpers.go) into named helpers
  (`isAllowedFilterTokenByte`, `isAllowedTopicByte`,
  `isAllowedCookieNameByte`) and extracted `serveStream`'s post-format
  branch in [`serve.go`](serve.go) into `finalizeStreamedMessage`. Pure
  refactors driven by mutation-kill targeting — no behaviour change.
- `Handler.readStreamSnapshot` now takes a narrow
  `streamMetadataReader` interface (`StreamInfo` + `GetMsg`) instead of
  the full `nats.JetStreamContext`, so unit tests can stub metadata
  reads independently of a live JetStream connection. The real
  `*nats.js` implementation satisfies it automatically.

### Security
- Bumped `github.com/caddyserver/caddy/v2` from `v2.11.2` to `v2.11.3` to
  fix **CVE-2026-45135** — unsafe Unicode handling in the FastCGI
  `splitPos` logic that could allow execution of non-PHP files. Flagged
  by Trivy on the 0.3.0 release image build (HIGH severity).
- Bumped `go` directive in `go.mod` from `1.26.2` to `1.26.4` to pull in
  upstream Go standard-library fixes for reachable vulnerabilities flagged
  by `govulncheck`:
  - **GO-2026-4982** (`html/template`) — bypass of meta content URL
    escaping leading to XSS.
  - **GO-2026-4980** (`html/template`) — escaper bypass leading to XSS.
  - **GO-2026-4971** (`net`) — `Dial`/`LookupPort` panic on NUL byte on
    Windows.
  - **GO-2026-5039** (`net/textproto`) — arbitrary inputs included in
    errors without escaping (reachable via `nats.Connect` →
    `textproto.Reader.ReadMIMEHeader`).
  - **GO-2026-5037** (`crypto/x509`) — inefficient candidate hostname
    parsing (reachable via `x509.Certificate.Verify` /
    `VerifyHostname` / `HostnameError.Error`).
  - **GO-2026-5038** (`mime`) — quadratic complexity in
    `WordDecoder.DecodeHeader` (present in imports; no reachable call
    site in this module).
- Bumped `golang.org/x/net` from `v0.52.0` to `v0.53.0` to fix
  **GO-2026-4918** — infinite loop in the HTTP/2 transport when given a
  malformed `SETTINGS_MAX_FRAME_SIZE`. Vulnerability was reachable
  transitively through `caddyhttp.HandlerFunc.ServeHTTP`. After all the
  bumps above `govulncheck ./...` reports no vulnerabilities.

## [0.2.0] - 2026-05-05

### Added
- New Caddyfile directive `health_path` (default `/healthz`) to customize
  the health-check endpoint.
- New Caddyfile directives `live_path` (default `/livez`) and `ready_path`
  (default `/readyz`) to split process liveness from NATS/JetStream
  readiness while keeping `health_path` as a backward-compatible readiness
  check.
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
- New Caddyfile directives `dispatch_timeout` and `write_timeout` (default
  `0`, disabled) to bound saturated slow-client signaling and per-frame SSE
  writes when supported by the HTTP response writer.
- New Caddyfile directive `replay_max_messages` (default `0`, unlimited)
  to cap how many historical events a single client receives on replay,
  with a new `nuts_replay_cap_reached_total` Prometheus counter.
- New Caddyfile directive `replay_window` (default `0`, all retained) to
  bound old replay cursors to the last N seconds via NATS `StartTime`.
- New Caddyfile directives `subscriber_jwt_key` and `subscriber_jwt_cookie`
  for optional first-party subscriber JWT auth and per-topic `subscribe` claim
  authorization before any JetStream consumer is created.
- Replay fallback now fires when the requested `last-id` is below the
  stream's retained range (previously the JetStream subscribe only
  silently started at `FirstSeq`; this flag-lit fallback enables the cap
  and window directives above and updates `nuts_replay_fallbacks_total`).
- Performance confidence suite with benchmarks for SSE event formatting,
  JSON compaction, topic validation, and multi-topic filtering, plus bounded
  load, replay, slow-reader, goroutine, and memory-growth tests documented in
  `PERFORMANCE.md`.
- Operations assets: Prometheus alert rules, a Grafana dashboard example, and
  a runbook for NATS outages, missing streams, replay storms, slow consumers,
  and CORS misconfiguration.
- Release and supply-chain hardening: containerized GoReleaser validation,
  PR snapshot dry runs for release config changes, Docker vulnerability scans,
  release/archive and image SBOM generation, Cosign signing for tagged Docker
  images, Dependabot configuration, and release policy documentation.
- Documentation and developer-experience additions: architecture and replay
  diagrams, a complete configuration matrix, troubleshooting guide, production
  deployment examples, a CI-aligned contribution checklist, and operator release
  note guidance.
- Docker Hub description now uses `DOCKERHUB_README.md`, a shorter Docker-focused
  README tailored for Docker Hub's description limits.
- `CONTRIBUTING.md`, `docs/SECURITY.md`, and this `CHANGELOG.md`.
- Docker image now runs as non-root user `nuts` (uid 10001).

### Changed
- Updated vulnerable indirect dependencies embedded in the Caddy binary:
  `github.com/go-jose/go-jose/v3` to `v3.0.5`,
  `github.com/go-jose/go-jose/v4` to `v4.1.4`, `github.com/jackc/pgx/v5`
  to `v5.9.0`, and `go.opentelemetry.io/otel/sdk` to `v1.43.0`.
- Subscriber JWT verification now rejects compact tokens over 8 KiB, decoded
  JWT segments over 6 KiB, and `subscribe` claims with more than 128 filters.
- `Validate()` now rejects `max_reconnects` values below `-1`; `-1` remains
  the unlimited sentinel and `0` still means no reconnects.
- `Validate()` now warns when `nats_credentials` is used over plaintext
  `nats://`, matching the existing warnings for token and user/password auth.
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
- Stream lifecycle logs now include consistent structured fields for requested
  topics, full subjects, replay mode, replay start/fallback context, and
  disconnect reason.
- `replay_max_messages` now caps all retained replay requests, not only
  purged-cursor fallback replay. `replay_window` now also bounds retained
  cursors older than the configured window while preserving exact sequence
  replay for cursors still inside the window.

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
- README and SECURITY now document subscriber-auth boundaries, Caddy
  `basic_auth` / `forward_auth` examples, per-tenant route isolation,
  rate-limit guidance, replay-bound guidance, and the decision to defer a
  first-party subscriber authorization hook to the opt-in JWT/private-topic
  roadmap.

## [0.x] - prior history

Initial development — see Git history.
