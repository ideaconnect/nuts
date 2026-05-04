# NUTS 10/10 Quality Burn-List

This plan turns the current quality review into an execution checklist. It is intentionally practical: each item should either remove a known risk, improve maintainability, increase confidence, or make production operation easier.

## Current Baseline

- Overall quality: about 8.3/10.
- Functionality: 8.5/10.
- Code quality: 8.0/10.
- Tests quality: 8.3/10.
- Current strengths: SSE streaming, JetStream replay, multi-topic support, bounded payload handling, slow-client disconnects, health checks, Prometheus metrics, Docker packaging, broad tests, and solid documentation.
- Current gaps: the earlier P0/P1 burn-list items have been addressed; remaining work is optional product polish plus any new review findings tracked in the changelog.

## Status Legend

- `[ ]` Not started.
- `[~]` In progress.
- `[x]` Done.
- `P0` Must fix before calling the app production-grade.
- `P1` High-value hardening for 10/10 quality.
- `P2` Nice-to-have polish or scale work.

## P0: Correctness And Safety

- [x] P0: Run semantic config validation before default normalization and before opening NATS connections in `Provision()`.
- [x] P0: Add lifecycle tests that prove invalid JSON config is rejected through the realistic Caddy `Provision()` / `Validate()` path.
- [x] P0: Preserve documented sentinel semantics while validating config, especially `max_event_size < 0` for unlimited and `client_buffer_size == 0` for default.
- [x] P0: Add regression tests for negative `max_connections`, `client_buffer_size`, `replay_max_messages`, `replay_window`, and unsupported `allowed_methods` through both Caddyfile and JSON-style construction.
- [x] P0: Make fallback replay behavior explicit in tests for both below-retention pre-checks and subscribe-time sequence errors.
- [x] P0: Verify cleanup behavior after every early return path that creates subscriptions or reserves a connection slot.

## P1: Request-Handling Structure

- [x] P1: Split `ServeHTTP` into smaller units: health/preflight handling, request parsing, replay cursor parsing, subscription planning, subscription execution, SSE event formatting, and the stream loop.
- [x] P1: Introduce a small request/stream plan type that captures topics, full subjects, replay mode, start sequence, and fallback strategy.
- [x] P1: Unit-test subscription planning without needing a live NATS server.
- [x] P1: Unit-test SSE event formatting directly, including JSON payloads, raw string payloads, metadata timestamps, and max-size rejection.
- [x] P1: Replace brittle error-string checks for NATS start-sequence failures with typed/structured detection if the NATS client exposes one.
- [x] P1: Keep comments focused on non-obvious behavior after the split; remove comments that merely narrate straightforward code.

## P1: Functional Test Reliability

- [x] P1: Replace fixed sleeps in `functional_test/steps_test.go` with polling helpers that wait for observable conditions.
- [x] P1: Add wait helpers for connected events, target message counts, heartbeat comments, stream availability, and client disconnect completion.
- [x] P1: Run functional tests with `-count=1` in CI or Make targets when validating real Docker/NATS behavior.
- [x] P1: Add retry-safe cleanup for functional test state so failed scenarios do not contaminate later scenarios.
- [x] P1: Add a CI stress pass that runs the functional suite multiple times to detect flakes before release.
- [x] P1: Capture service logs automatically when functional tests fail in CI.

## P1: Compatibility And NATS Behavior

- [x] P1: Add a NATS server version matrix for functional tests, including one version below 2.10 and one current 2.12+ version.
- [x] P1: Add explicit tests that prove NATS 2.10+ uses `ConsumerFilterSubjects` for multi-topic subscriptions.
- [x] P1: Add explicit tests that prove older NATS versions use wildcard subscription plus in-process filtering without duplicate delivery.
- [x] P1: Add tests for stream subject mismatches in multi-topic requests and verify the response is clear and consistent.
- [x] P1: Validate behavior when NATS reconnects while SSE clients are connected.

## P1: Security And Authorization

- [x] P1: Add documented Caddy examples for protecting the NUTS route with authentication middleware or a reverse proxy.
- [x] P1: Add documented examples for per-tenant isolation using separate streams, prefixes, or route blocks.
- [x] P1: Consider an optional first-party subscriber authorization hook if Caddy-only policy is not sufficient for real deployments.
- [x] P1: Add rate-limit guidance for connection attempts, replay-heavy clients, and invalid-topic probes.
- [x] P1: Add security tests for CORS credential behavior, method filtering, invalid topics, and replay bounds to CI-focused suites.
- [x] P1: Document operational guidance for `replay_max_messages` and `replay_window` defaults on large retained streams.

## P1: Performance And Load Confidence

- [x] P1: Add benchmarks for SSE event formatting, JSON compaction, topic validation, and multi-topic filtering.
- [x] P1: Add load tests for many concurrent SSE clients with a realistic message rate.
- [x] P1: Add slow-reader tests that measure disconnect behavior and ensure no goroutine leaks.
- [x] P1: Add replay-load tests for large retained streams with and without fallback caps.
- [x] P1: Track memory growth during large payload and large replay scenarios.
- [x] P1: Define target performance budgets for latency, memory per connection, and maximum sustainable clients per instance.

## P1: Observability And Operations

- [x] P1: Add example Prometheus alert rules for high disconnect rates, subscription errors, replay fallback spikes, and max-connection rejections.
- [x] P1: Add a dashboard example for active clients, delivered messages, drops, replay activity, and NATS connectivity.
- [x] P1: Add structured log fields consistently for request topics, subject labels, replay mode, and disconnect reason.
- [x] P1: Consider a readiness endpoint distinct from liveness if deployments need separate orchestration semantics.
- [x] P1: Add an operations runbook for common incidents: NATS down, stream missing, replay storm, slow consumers, and CORS misconfiguration.

## P1: CI, Release, And Supply Chain

- [x] P1: Add a containerized local `make release-check` so GoReleaser validation does not depend on a developer-installed binary.
- [x] P1: Add a GoReleaser snapshot dry run in CI for pull requests that touch release configuration.
- [x] P1: Add Docker image vulnerability scanning for pull requests and release builds.
- [x] P1: Add SBOM generation for release artifacts and Docker images.
- [x] P1: Consider signing release artifacts and container images.
- [x] P1: Add Dependabot or Renovate for Go modules, Docker images, and GitHub Actions.
- [x] P1: Pin action versions by major today, and consider pinning by SHA if stricter supply-chain policy is desired.

## P2: Documentation And Developer Experience

- [x] P2: Add an architecture diagram showing Caddy, NUTS, NATS JetStream, producers, subscribers, and replay flow.
- [x] P2: Add a configuration matrix that lists every directive, default, valid values, JSON field name, and operational notes.
- [x] P2: Add a troubleshooting guide for common browser/EventSource and CORS issues.
- [x] P2: Add copy-paste examples for production Compose, Kubernetes, and reverse-proxy-protected deployments.
- [x] P2: Add a contribution checklist that mirrors the CI gates.
- [x] P2: Add release notes guidance for operators upgrading Caddy, Go, NATS, or replay behavior.

## P2: Product Polish

- [ ] P2: Consider optional event-type mapping from topic or metadata.
- [ ] P2: Consider optional payload envelope customization for users that want raw payload-only SSE events.
- [ ] P2: Consider exposing NATS server version and stream metadata in health or debug output, gated appropriately.
- [ ] P2: Consider configurable retry hints in SSE output for browser reconnect behavior.
- [ ] P2: Consider a sample JavaScript client helper for replay-aware subscription setup.

## 10/10 Exit Criteria

- [x] All P0 items are complete.
- [x] High-value P1 items are complete or explicitly rejected with documented rationale.
- [x] `gofmt -l .` returns no files.
- [x] `go test -timeout 120s .` passes.
- [x] `go test -run '^$' ./...` passes.
- [x] `go test -race -timeout 180s .` passes or an intentionally focused race-test policy is documented.
- [x] `go vet ./...` passes.
- [x] `golangci-lint run` passes in CI and in the documented local path.
- [x] `govulncheck ./...` reports no reachable vulnerabilities.
- [x] `make test-functional` passes repeatedly without flakes.
- [x] Docker production image builds and `caddy adapt --config /app/Caddyfile` passes inside the image.
- [x] GoReleaser `check` and a snapshot dry run pass.
- [x] Functional tests cover both old and current NATS server behavior for multi-topic subscriptions.
- [x] Load/stress tests meet documented latency, memory, and concurrency budgets.
- [x] Docs explain subscriber authentication boundaries, replay bounds, operational alerts, and deployment hardening.

## Suggested Execution Order

1. Fix lifecycle validation ordering and add realistic lifecycle regression tests.
2. Replace functional-test sleeps with condition-based polling.
3. Split `ServeHTTP` into smaller units while preserving behavior.
4. Add NATS version compatibility tests for multi-topic subscriptions.
5. Add load/backpressure benchmarks and define production budgets.
6. Add release/supply-chain checks and operational runbooks.
7. Polish docs, examples, and optional product improvements.