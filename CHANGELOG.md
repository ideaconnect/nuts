# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
