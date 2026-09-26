# NUTS Remediation Plan: post-v0.4.2 review

This plan turns the 2026-09-26 review into an execution checklist. It covers
all 63 open issues in milestones [M9], [M10] and [M11]: 1 critical, 18 major,
34 minor and 10 nit.

The order follows risk and dependencies:

1. Unblock CI and fix the critical bug.
2. Build the test foundation.
3. Fix message delivery in two passes: local cursor fixes, then the pipeline.
4. Fix NATS compatibility, then scalability.
5. Hardening, test hygiene and documentation.

The completed v0.4 "10/10 Quality Burn-List" that used to live in this file is
in git history (`git show e772731:docs/PLAN.md`). Its five open P2 items are
carried over to the [Backlog](#backlog).

## How to use this plan

- Status markers:
  - `[ ]` not started
  - `[~]` in progress
  - `[x]` implemented, tested and committed (work happens on the
    `v1-remediation` branch until it is merged)
- Effort:
  - **S**: half a day or less
  - **M**: 1–3 days
  - **L**: more than 3 days
- Open one PR per issue, or per tightly coupled pair.
- Write `Closes #N` in the PR description. The M9 phases were merged with
  `Refs #N` and stayed open until the review closed them by hand.
- Items tagged **Dn** wait on a decision from [Decisions](#decisions).

### Definition of done (every PR)

- A failing test that reproduces the issue lands together with the fix.
- These pass: `gofmt -s`, `go vet ./...`, `make lint`, `make test-unit`.
- A Godog scenario is added when the change is observable over HTTP.
- `make mutate-pkg PKG=<file>` is run for every touched `auth.go`,
  `helpers.go`, `handler.go`, `serve.go`, `caddyfile.go` or `provision.go`:
  - the PR reports the MSI;
  - every survivor is killed or recorded in `docs/mutation/equivalents.md`.
- There is a CHANGELOG entry under `[Unreleased]`. Contract changes are
  flagged **Breaking** under *Changed*, per the 0.x rule in README
  "Versioning policy".
- Docs are updated in the same PR:
  - README and `docs/CONFIGURATION.md` for directives and defaults;
  - `docs/OPERATIONS.md` for new disconnect reasons and metrics;
  - `website/docs` when client examples change.

## Critical path

D1 → [#108] → [#114] → **v0.4.3** → [#125] + [#127] + test kit → [#97] →
[#101], [#102], [#103], [#98] → **v0.5.0** → D2 spike → [#99], [#100], [#120]
(or [#116]) → [#110], [#105], [#111] → **v0.6.0** → [#119] → **v0.7.0**

Phases 7 and 8 can run in parallel with anything after Phase 1. Phase 9
items are due before each release.

## Decisions

- [x] **D1: Go toolchain bump.** Approved; Go 1.26.8 (e74b5da).
  - Options: approve Go ≥ 1.26.6 in `go.mod` and both Dockerfiles. AGENTS.md
    requires explicit approval for a toolchain bump.
  - Recommendation: approve. CI can't go green without it ([#108]).
  - Blocks: everything.
- [x] **D2: Delivery primitive.** Decided **B** after the spike: the delivery
  kit passes on the ordered pull consumer and fails on the push pipeline
  (3e215e8). There were two options:
  - **A.** Keep the legacy push consumers and fix them in place: [#99], [#100],
    [#120], [#53] and [#54] are all needed.
  - **B.** Use a per-client `jetstream.OrderedConsumer` with a bounded pull
    loop.
  - Recommendation: decide after the Phase 4.1 spike, and prefer B if it passes
    the delivery kit. With B the library handles gaps, missed heartbeats and
    consumer recreation, and pull delivery gives backpressure for free
    ([#116]).
  - Blocks: Phase 4.
- [x] **D3: Server support floor.** Decided: drop 2.9 and ship the result as
  the next MAJOR release.
  - Options: keep nats-server 2.9 (the wildcard fallback), or require ≥ 2.10.
    Either way, set a minimum server version for multi-topic correctness.
  - Recommendation:
    - drop 2.9, which has been end-of-life since 2024, and delete the wildcard
      path;
    - document ≥ 2.14.7 (2.15 recommended) for multi-topic subscriptions.
  - Blocks: [#109] (matrix), [#111], [#117].
- [~] **D4: Framing and response-shape changes.** Following the
  recommendation: a) and b) are implemented; c) and d) are next; e) stays
  opt-in. A 0.x minor may make these with Breaking notes. The options are:
  - a) `id:` on the `connected` event
  - b) the `Last-Event-ID` header wins over `?last-id`
  - c) EventSource requests get `200` + `retry:` on transient failures
  - d) server control messages are skipped
  - e) an explicit gap signal
  - Recommendation: accept a–d, and make e opt-in.
  - Blocks: [#101], [#102], [#105], [#112], [#107].
- [x] **D5: Stall-protection defaults.** `write_timeout` defaults to 30 s
  (5391949). `dispatch_timeout` is deprecated instead of defaulted: the pull
  pipeline has no queue hand-off left to time out.
  - Options: keep `dispatch_timeout 0` and `write_timeout 0`, or ship non-zero
    defaults.
  - Recommendation: non-zero defaults (for example a 5 s no-progress budget and
    a 30 s write deadline), with a Breaking note.
  - Blocks: [#100], [#120], [#85].
- [x] **D6: Replay cap when no snapshot is available.** Implemented as
  recommended.
  - Options: fail with a retryable response, or derive the history boundary
    from the first delivered message (`StreamSequence + NumPending`).
  - Recommendation: the `NumPending` boundary for the cap, and the window
    fallback for `replay_window`.
  - Blocks: [#98].

## Release checkpoints

| Release | Scope | Contents |
| --- | --- | --- |
| v0.4.3 | Phase 1 | Patch release: [#114] fix, toolchain and module security bumps, and a published Docker image again |
| v0.5.0 | Phases 2–3 | Replay and cursor correctness; Breaking notes for [#102] and [#83] |
| v0.6.0 | Phases 4–5 | Delivery pipeline, NATS 2.15 compatibility, new defaults (D5), response shapes (D4c) |
| v0.7.0 | Phase 6 | Shared fan-out, opt-in first |

**Revised:** with D2 = B and 2.9 dropped (D3), Phases 2–9 ship together as the
next MAJOR release, v1.0.0, from the `v1-remediation` branch instead of as
v0.5–v0.7. v0.4.3 is stamped at commit 114632f and waits for its tag.

## Phase 1: Unblock CI, fix the critical bug, release v0.4.3

Goal: green CI, streaming that works behind Caddy access logs and HTTP
metrics, and a published Docker image.

- [x] **D1 approved.**
- [~] **[#108] Get CI green again.** *major · M*
  - [x] Bump Go to the latest 1.26.x patch (≥ 1.26.6):
    - `go.mod` (`go` / `toolchain`), `Dockerfile` and `Dockerfile.test`;
    - every "1.26.4" mention (AGENTS.md, README "Compatibility",
      CONTRIBUTING).
  - [x] Clear the flagged modules (grpc, x/text, otel), preferably through a
    Caddy patch bump, otherwise with explicit `require` lines; then run
    `go mod tidy`.
  - [~] `govulncheck ./...` is clean locally, and CI is green on `main`.
    Clean locally; CI runs once the branch is pushed.
  - [x] Gate `release.yml` on CI success for the tag commit, so a red tag
    can't publish archives.
  - [ ] Rebase or supersede Dependabot [#92] and [#96]. Superseded by
    e74b5da; close them after the push.
- [x] **[#114] Streaming behind Caddy access logs and HTTP metrics.**
  *critical · S*
  - [x] Replace the `w.(http.Flusher)` check in `ServeHTTP` with
    `http.NewResponseController(w)`. Drop the `flusher` parameter from
    `serveStream` and `writeSSEChunk*`.
  - [x] Make `writeSSEChunk` return the flush error, so
    `nuts_messages_delivered_total` counts only frames that were actually
    flushed.
  - [x] Unit test with a writer that exposes only `Write`, `Header`,
    `WriteHeader`, `FlushError` and `Unwrap`.
  - [x] `caddy.Load` integration tests with `"logs": {}` and with
    `"metrics": {}`.
  - [x] Add `log` to `Caddyfile.test`, so the Godog suite runs with access
    logging on.
- [x] **[#109] Module bumps.** *minor · S*
  - [x] nats.go → v1.54.0; embedded nats-server → v2.15.0 (at least 2.14.7).
  - [x] Run `govulncheck -test ./...` in CI, or add a separate test-scope job.
- [~] **Release v0.4.3.** *S*
  - [x] CHANGELOG:
    - *Fixed*: [#114];
    - *Security*: the toolchain and module bumps;
    - a note that v0.4.2 has no Docker image and was built with Go 1.26.4.
  - [ ] Walk the `docs/RELEASE.md` checklist: `idcttech/nuts:v0.4.3` on Docker
    Hub, cosign signature, SBOM, release archives. Waits for the tag.

## Phase 2: Test foundation

Goal: tests run the real lifecycle, fail instead of hanging, and can prove the
delivery contract. Phases 3–6 rely on this kit.

- [x] **[#125] Streaming tests go through `Provision`.** *major · M*
  - [x] Make `newProvisionedHandler` call `Provision`. Add `newConnectedHandler`
    for the few tests that need a half-initialised handler. (Not needed:
    those tests call `connectNATS` directly.)
  - [x] Route `TestHandler_Cleanup_WakesInFlightHandlers`,
    `TestHandler_Cleanup_IsIdempotent` and the other lifecycle tests through
    `Provision` + `Cleanup`, and confirm mutants mA and mB are killed.
  - [x] **[#58]** Delete `TestHandler_IdleHeartbeat_DefaultOnAfterProvision`
    (`_PropagatesThroughProvisionDefault` already covers it), or rewrite it on
    the new helper. *minor · S*
- [ ] **[#127] Test timing.** *major · M*
  - [ ] Replace sleep-as-sync with waiting for `event: connected` or with
    `waitForConsumerCount`. Drop sleeps that follow an already-acknowledged
    publish.
  - [ ] Poll, then cancel, instead of waiting out context deadlines. Merge the
    duplicate dispatch-timeout tests. Target: 20 s or less for the unit
    package.
  - [ ] Give every streaming `ServeHTTP` call in tests a context deadline, so a
    guard regression fails its test instead of hanging the package.
- [x] **[#126] Slow-client release path.** *major · M* The pull migration
  removed `signalSlowClient` and the enqueue path, so two items became
  obsolete.
  - [x] Take the goroutine baseline before the server starts (or measure before
    `ns.Shutdown`), with a slack of 3 or less. Confirm mutant mI is killed.
  - [x] ~~Add deterministic tests for both `<-done` arms of `signalSlowClient`.~~
    Confirm mutant mF is killed.
  - [x] ~~Make the `BenchmarkEnqueueMessageSteadyState` drainer also read~~
    ~~`slowClient`~~; replaced by `BenchmarkStreamFeed`, which is in
    `make test-performance`.
- [x] **Delivery-contract test kit** (shared helpers, e.g. `testutil_test.go`).
  *M*
  - [x] `assertContiguousIDs`: ids strictly increasing, with no gaps and no
    duplicates.
  - [x] A black-hole TCP proxy between NUTS and NATS (discard, then cut) for
    link-loss tests.
  - [x] A burst and replay harness: `PublishAsync` N messages, then run an
    EventSource-style reconnect loop that resends `Last-Event-ID` and counts
    connections.
  - [x] A `caddy.Load` helper for Caddy-in-the-loop tests (logs, metrics,
    routes).
- [ ] **[#128] Pin the unpinned contracts.** *minor · M*
  - [ ] Assert all four SSE response headers, in a unit test and one Godog
    step. This kills mE.
  - [ ] Replay-window "caught-up" guard: add a unit case where
    `StartSequence = LastSeq + 1` must not fall back, plus an integration case.
    This kills mW.
  - [ ] Replace the `len(opts) < 3` check with an `OptStartTime` assertion.
    Drive the MaxReconnects-0 and RejectsBeforeDialing tests through
    `Provision`.
  - [ ] Document in `docs/mutation` that the MSI covers operator mutations only.
    Optionally add a statement-deletion script for the six hot files.

## Phase 3: Replay and cursor correctness → v0.5.0

Goal: every cursor path resumes exactly where the client left off. The changes
are local to request parsing and planning in `serve.go`, and don't depend on
D2.

- [x] **[#97] Remove the dead subscribe-time fallback** (do this first; it
  simplifies the rest). *minor · S*
  - [x] Delete the retry in `executeSubscriptionPlan`,
    `isReplayStartSequenceError`, `jsErrCodeSequenceNotFound`, their test, and
    the reference in the `helpers.go` godoc.
  - [x] Add a comment in `planSubscription` explaining that the server clamps a
    below-retention start sequence to `FirstSeq`. Add an integration test that
    pins that server behaviour.
- [x] **[#103] Cursor ahead of the stream.** *major · S*
  - [x] When a snapshot exists and `StartSequence > LastSeq + 1`, fall back with
    reason `cursor ahead of stream`, log a Warn, and count it in
    `nuts_replay_fallbacks_total`.
  - [x] Test by recreating the stream.
- [x] **[#98] Replay protections fail closed.** *major · M* (D6)
  - [x] With `replay_window` set and a failed `StreamInfo`, use
    `fallback_start_time` with reason `stream info unavailable`.
  - [x] With only a cap configured, derive the history boundary from the first
    delivered message (`StreamSequence + NumPending`) instead of disabling the
    cap.
  - [x] Add tests with a failing `streamMetadataReader` stub, and a README note
    ([#70] item I).
- [x] **[#115] Deleted resume message.** *minor · S* Implemented by dating
  the resume point with the next retained message
  (`GetMsg` with `next_by_subj`) instead of a client-side window.
  - [x] ~~When `GetMsg` returns not-found (10037) inside `[FirstSeq, LastSeq]`,~~
    keep `start_sequence` and apply the window client-side to the historical
    part.
  - [x] The test expects `[4 5]`, not `[1 2 4 5]`.
- [x] **[#106] Window filter applies to history only.** *minor · S*
  - [x] Skip messages only when `StreamSequence <= CapSequence`.
  - [x] Count skips as `nuts_messages_dropped_total{reason="replay_window"}`.
- [x] **[#101] Cursor on the `connected` event.** *major · M* (D4a)
  - [x] Requests without a cursor:
    - read `StreamInfo` with a bounded wait, subscribe with
      `StartSequence(LastSeq + 1)`, and emit `id: <LastSeq>` on `connected`
      (`id: 0` for an empty stream);
    - if `StreamInfo` fails, keep today's `DeliverNew` path and log it.
  - [x] Replay requests emit `id: <last-id>` on `connected`.
  - [x] Tests:
    - cancel before the first message, publish during the gap, reconnect with
      the received id, and assert everything arrives;
    - a Godog scenario for a reload before the first message.
  - [x] Update README "Message Format". After [#116], reuse the consumer's
    cached info instead of the extra `StreamInfo`.
- [x] **[#102] Cursor precedence.** *major · S* (D4b)
  - [x] A valid `Last-Event-ID` header wins over `?last-id=` (or take the
    larger of the two). A malformed explicit query still returns 400.
  - [x] Update the README precedence section, the README JS example and
    `website/docs/usage.md`. Mark the CHANGELOG entry **Breaking**.
  - [x] Tests:
    - `?last-id=2` plus `Last-Event-ID: 7` starts at 8;
    - a Godog reconnect that keeps the original URL.
- [x] **[#104] Sticky overflow.** *minor · S* Obsolete: the pull pipeline
  has no overflow path (3e215e8).
  - [x] ~~Set an atomic flag on the first overflow. The callback never enqueues~~
    after that, and the writer checks the flag before each write.
  - [x] ~~Race test with buffer sizes 1–4 over many iterations, using~~
    `assertContiguousIDs`.
- [x] **[#83] Probe paths with a trailing slash.** *minor · S*
  - [x] Normalise one trailing `/` before matching.
  - [x] Assert `Content-Type: application/json` for `/livez/`, `/readyz/` and
    `/healthz/`.
- [x] **Release v0.5.0.** *S* Folded into the MAJOR release; the Breaking
  entries are in the CHANGELOG.
  - [x] The CHANGELOG marks as **Breaking**:
    - [#102]: cursor precedence;
    - [#101]: the id on `connected`;
    - [#83]: probe routing.

## Phase 4: Delivery pipeline (backpressure, gap detection, invalidation, memory)

Goal: no silent holes, no spurious slow-client disconnects, and bounded memory
per connection, proven with the Phase 2 kit.

- [x] **4.1 D2 spike.** *M*
  - [x] Prototype a per-client `jetstream.OrderedConsumer` behind `serveStream`:
    - consumer config: `FilterSubjects`, `DeliverPolicy` / `OptStartSeq` /
      `OptStartTime`, `InactiveThreshold`;
    - a pull loop feeding a bounded channel (`PullMaxMessages` ≈
      `client_buffer_size`);
    - heartbeats stay in the writer's select loop.
  - [x] Run the kit's link-loss ([#99]), backlog and burst ([#100]),
    overflow-race ([#104]) and consumer-delete ([#54]) tests against both
    pipelines.
  - [x] Record the results in [#116] and decide D2.

**If D2 = B (migrate):**

- [x] **[#116] Move the subscription path to the `jetstream` package.**
  *minor · L*
  - [x] Per-client ordered consumer plus the pull loop.
  - [x] A slow client is detected by `write_timeout` expiry, with the D5
    default.
  - [x] Provision, the readiness probe and the snapshot read go through
    `jetstream.Stream`.
  - [x] Regression test: a reset before the first message must not replay
    history (the legacy `nats.OrderedConsumer` + `DeliverNew` trap).
  - [x] Verify with the kit, then:
    - close [#99], [#100], [#104], [#120], [#85] and [#54];
    - close [#53] as superseded.

**If D2 = A (keep push consumers):** not taken; D2 = B. [#99], [#100], [#104],
[#85], [#53] and [#54] were closed by the migration, and [#120] is covered by
the pull prefetch bound.

- [ ] **[#99] Consumer-sequence continuity check.** *major · M*
  - [ ] Check continuity in the callback, before the wildcard filter. On a
    gap, stop enqueueing and end the stream with
    `disconnect_reason=delivery_gap` before writing anything past the gap.
  - [ ] Add a metric and a proxy test.
- [ ] **[#100] Backpressure instead of disconnect.** *major · L* (D5)
  - [ ] Block the callback while the writer is making progress. A client is
    slow only after no progress for `dispatch_timeout` (non-zero default).
  - [ ] Enable `nats.EnableFlowControl()`. On a slow signal, flush the frames
    already queued before closing.
  - [ ] Tests:
    - a 3000-message backlog replays on one connection;
    - a 1000-message live burst causes no disconnect;
    - a stalled reader is still disconnected.
- [ ] **[#120] Bounded memory per connection.** *major · M*
  - [ ] Enforce `max_event_size` at enqueue.
  - [ ] Call `sub.SetPendingLimits(...)` with values derived from
    `client_buffer_size` and `max_event_size`.
  - [ ] Drop immediately once a slow signal is pending.
  - [ ] Memory test with a stalled client. Fix the formula in
    `docs/PERFORMANCE.md` and README.
- [ ] **[#85] No indefinite parking.** *minor · S*
  - [ ] Verify with the new defaults, using the release-path tests from [#126].
- [ ] **[#53] + [#54] M9 Batch B.** *major · L*
  - [ ] Implement as specified in the issues.
  - [ ] Act only when `nc.Status() == CONNECTED`.
  - [ ] Send a jittered `retry:` on disconnect.
  - [ ] Add alerts (see [#86]).

**Both paths:**

- [ ] **[#121] Batch already-queued frames.** *minor · M*
  - [ ] One write, one flush, and one deadline set/clear per batch. Keep the
    clear on HTTP/2.
- [ ] **[#107] Delivery visibility.** *nit · S–M*
  - [ ] Write `id:` last in the frame.
  - [x] Log the stream sequence for oversize drops.
  - [ ] Optional gap signal (D4e).
  - [ ] Attribute slow-consumer errors to the stream and topics.
- [ ] **Godog scenarios.**
  - [ ] A NATS restart mid-stream still produces contiguous ids.
  - [ ] A backlog replays on one connection.
  - [ ] A burst causes no `slow_client`.

## Phase 5: NATS server compatibility (2.14.7 / 2.15)

Goal: correct behaviour on current servers, and clear, retryable errors when a
server-side limit is hit.

- [x] **[#109] Test matrix.** *minor · S* (D3)
  - [x] Add `nats:2.15-alpine`.
  - [x] Move the CI and `docker-compose.yml` defaults off `nats:2.12-alpine`.
  - [x] Keep or drop 2.9 per D3. Update README "Compatibility" and the
    Makefile comment.
- [x] **[#111] Minimum server for multi-topic.** *major · S* A startup warning below 2.14.7; the regression test fails against nats:2.12 and passes on 2.15.
  - [x] Document ≥ 2.14.7 (2.15 recommended) for multi-topic subscriptions.
  - [x] Optionally warn, or fall back to the wildcard path, on older servers.
  - [x] Regression scenario: purge one subject while the other has messages
    pending.
- [x] **[#110] 2.15's 1000-consumer default.** *major · M*
  - [x] Document the requirement in README, DEPLOYMENT and OPERATIONS:
    - a positive `max_consumers` on the stream or account, or the server-wide
      `default_max_consumers: -1`;
    - `--max-consumers` in the `nats stream add` examples.
  - [x] Detect error 10026 (`JSErrCodeMaximumConsumersLimit`) and handle it with:
    - its own `disconnect_reason`;
    - `nuts_connections_rejected_total{reason="stream_consumer_limit"}`;
    - the retryable response from [#105].
  - [x] A 2.15 scenario with a small `max_consumers`. [#119] removes this
    limit structurally.
- [x] **[#112] Server control messages.** *minor · S* (D4d)
  - [x] Skip messages carrying `Nats-Marker-Reason` or `Nats-Schedule*`, and
    count them as `reason="control_message"`.
  - [x] Optionally forward markers as an opt-in `event: deleted`.
  - [x] Test with `AllowMsgTTL` + `SubjectDeleteMarkerTTL`.
- [x] **[#113] `InactiveThreshold` vs stream consumer limits.** *minor · S* Capped per request from the stream info, so a limit changed after startup is honoured too; the heartbeat bound is tied to the pull expiry, not the threshold.
  - [x] Read `ConsumerLimits.InactiveThreshold` in Provision.
  - [x] Clamp (and re-validate `nats_idle_heartbeat`), or fail with an
    actionable error.
  - [x] Map error 10153.
- [x] **[#117] Capability detection.** *minor · S* (D3) The wildcard path was deleted with the pull migration.
  - [x] Retry through the wildcard path on
    `ErrConsumerMultipleFilterSubjectsNotSupported`, or delete the wildcard
    path if 2.9 is dropped.
- [x] **[#105] Retryable responses for EventSource.** *minor · M* (D4c) Also answers at once while NATS is reconnecting.
  - [x] For requests with `Accept: text/event-stream`, answer `200` +
    `retry: <ms>` (jittered), then close. This applies to transient failures:
    JetStream unavailable, subscribe failure, `max_connections`, and the
    consumer limit.
  - [x] Other clients keep 503/429.
  - [x] Update `example/index.html` and the README.
- [x] **[#73] NATS callback hygiene.** *minor · S*
  - [x] Add `nats.NoCallbacksAfterClientClose()`.
  - [x] Add a `LameDuckModeHandler` that logs and counts
    `event="lame_duck"`.
- [x] **[#75] Drain on Cleanup.** *nit · S* Implemented as a bounded wait for the streams' consumer deletes rather than `Drain`.
  - [x] `Cleanup` drains with a bounded `DrainTimeout`, so per-request consumer
    deletes happen.
  - [x] Test that no consumers linger.
- [x] **[#72] Bounded waits.** *minor · S*
  - [x] Add `MaxWait` to the Provision-time `StreamInfo` and to the JetStream
    context (shared with [#123]).
- [x] **[#74] `nats_idle_heartbeat -1` warning.** *minor · S*
  - [x] Log a Warn when set, and add a README operability note.
- [x] **Release v0.6.0** (Phases 4–5). *S* Folded into the MAJOR release; the Breaking entries are in the CHANGELOG.
  - [x] The CHANGELOG marks as **Breaking**:
    - the new defaults (D5);
    - the response shapes ([#105]);
    - control-message filtering ([#112]).

## Phase 6: Scalability and hot-path performance

Goal: per-message cost that doesn't grow with the number of subscribers, and
documented budgets that hold.

- [ ] **[#119] Shared live subscriptions.** *major · L*
  - [ ] Write a design note in `docs/ARCHITECTURE.md`:
    - a registry keyed by the sorted subject set, with one consumer per key;
    - each frame formatted once and pushed to bounded per-client queues;
    - replaying clients hand off by stream sequence, with de-duplication;
    - reference-counted teardown.
  - [ ] Ship behind an opt-in directive first (MINOR). Make it the default in a
    later release, after a soak period.
  - [ ] Add the issue's benchmarks (CPU and allocs per delivery, NATS messages
    per delivery) to `make test-performance`.
- [ ] **[#118] One connection carries N copies.** *major · M*
  - [ ] Resolved by [#119]. If #119 slips, add a small NATS connection pool.
  - [ ] Document server `max_pending` / `write_deadline` sizing.
  - [ ] A burst test at the documented scale must not trigger a slow-consumer
    kick.
- [ ] **[#122] Single-pass formatter.** *minor · M*
  - [ ] Compact the payload straight into a `[]byte` frame, write the envelope
    by hand, and call `w.Write`.
  - [ ] Golden tests pin byte-identical output: HTML escaping, U+2028/9,
    non-JSON input, empty payload.
- [ ] **[#123] Per-connection JetStream cost.** *minor · M*
  - [ ] Cache the stream subjects at Provision, and deduplicate concurrent
    `StreamInfo` calls with `singleflight`.
  - [ ] Avoid `GetMsg` for the window check (use `DirectGet`, or subscribe
    first and check the first message).
  - [ ] Release the connection slot before the consumer delete, and delete
    asynchronously.
- [ ] **[#124] Allocation-free subject matcher.** *minor · S*
  - [ ] Walk the tokens without `strings.Split`.
  - [ ] Fuzz-test equivalence against the old matcher (after [#61]).
- [ ] **Re-measure performance.**
  - [ ] Update the `docs/PERFORMANCE.md` budgets and the per-connection memory
    formula, backed by a test.

## Phase 7: Control path and configuration hardening (parallelisable)

Several of these are good first issues.

- [x] **[#78] Reject CA pinning with insecure-skip-verify.** *major · S*
  - [x] Reject `nats_tls_ca` combined with `nats_tls_insecure_skip_verify` in
    `validateConfigValues`, which runs inside Provision before dialling. Mark
    the CHANGELOG entry **Breaking**.
- [x] **[#82] Earlier insecure-TLS warning.** *minor · S*
  - [x] Emit the warning from Provision, before `connectNATS`.
- [x] **[#77] `nats_url` scheme allowlist.** *minor · S*
  - [x] Allow `nats`, `tls`, `ws` and `wss`, including comma-separated lists.
  - [x] Warn about plaintext credentials for `nats://`, `ws://` and URLs with no
    scheme.
- [x] **[#79] Validate `allowed_origins`.** *minor · S*
  - [x] Accept only `*` or `scheme://host[:port]`: no empty entries,
    whitespace, control characters or commas.
- [x] **[#80] Parse-time negative checks.** *nit · S*
  - [x] Reject negative `heartbeat_interval` and `reconnect_wait` at parse time.
- [x] **[#81] Tighten the topic-cap sentinel.** *nit · S*
  - [x] Only `-1` disables `max_topics_per_subscription`. Mark the CHANGELOG
    entry **Breaking**.
- [x] **[#84] Better cleanup logging.** *nit · S* `cleanupStream` is gone; failed consumer deletes are logged with the topics instead.
  - [x] ~~`cleanupStream` logs the request's topics and subjects instead of
    `sub.Subject`.~~

## Phase 8: Test-suite hardening and hygiene (parallelisable)

- [ ] **[#129] Positive controls and exact metric deltas.** *minor · M*
  - [ ] Give vacuous tests a positive control, and assert exact metric deltas.
  - [ ] Assert the series no test covers yet:
    - `nuts_wildcard_filter_drops_total`;
    - `nuts_write_disconnects_total{site=connected|message}`;
    - `nuts_nats_connection_events_total{event="closed"}`.
  - [ ] Remove the undocumented `"queue_full"` label.
- [ ] **[#130] Godog gaps.** *minor · M*
  - [ ] The invalidation scenario asserts that `consumer_invalidated`
    increments.
  - [ ] Add a quiet window to exact-count steps.
  - [ ] Add a contiguous-ids step.
  - [ ] Expose a metrics endpoint in the functional stack.
- [ ] **[#131] Hygiene.** *nit · M*
  - [ ] Split `nats_test.go` by source file, with a shared `testutil_test.go`.
  - [ ] Keep one server-start helper and one counter reader.
  - [ ] Isolate subtests.
  - [ ] Fix documentation drift in CONTRIBUTING, the AGENTS.md race-test note
    and the `fuzz_test.go` comment.
  - [ ] Stop restart tests from reusing a port after releasing it.
- [ ] **[#61] Fuzz properties.** *minor · S*
  - [ ] Add property assertions to `FuzzSubjectMatchesFilter` and
    `FuzzSubscriberTopicMatches`. This is needed before [#124].
- [ ] **[#63]** `TestTryParseJSON` asserts the parsed content. *minor · S*
- [ ] **[#64]** The large-payload memory test asserts the payload survives
  formatting. *nit · S*
- [ ] **[#65] Readiness failure metric.** *minor · S*
  - [ ] Force a readiness-probe `StreamInfo` failure, and assert that
    `cause="stream_info_error"` increments and is logged.
- [ ] **[#66]** Assert the `disconnect_reason=max_connections` log field.
  *minor · S*
- [ ] **[#67]** Assert CORS headers on 429 (and on 401, 403 and 503).
  *minor · S*
- [ ] **[#68]** Assert `h.shutdown == nil` after a `connectNATS` failure in
  `Provision`. *minor · S*
- [ ] **[#69]** Assert `subscription_failed`, and test or delete the
  unreachable `subscription_empty` branch. *minor · S*
- [ ] **[#87]** With the cookie configured but absent or empty, the request
  gets 401. *nit · S*

## Phase 9: Documentation, metrics and operations (due before each release)

- [x] **[#70] README sweep.** *minor · M*
  - [x] Cover items A–M from the issue, plus N (consumer retention wording).
    Item I no longer applies: #98 keeps the cap without a snapshot.
  - [x] Sync `website/docs`.
- [x] **[#71] Metric Help strings.** *nit · S*
  - [x] Enumerate label values in the Help strings.
  - [x] Split nil-error closes out of `event="disconnect"`. Resolved by #73:
    NUTS's own close no longer reports a disconnect.
  - [x] Fix the `nuts_replay_fallbacks_total` Help text.
- [x] **[#86] Alert rules and dashboard.** *nit · S*
  - [x] Add alerts for:
    - `consumer_invalidated`, `slow_consumer` (`dispatch_timeout` is gone);
    - `write_disconnects`, `messages_dropped` (`wildcard_filter_drops` is
      always 0);
    - the new Phase 3–5 metrics: consumer recoveries, the consumer limit,
      lame duck mode. Control messages are routine and only on the
      dashboard.
  - [x] Add matching Grafana panels.
- [x] **`docs/OPERATIONS.md` runbooks.**
  - [x] Cover NATS link loss and delivery gaps, the 2.15 consumer limit,
    replay storms, and running behind access logs.
- [x] **`docs/CONFIGURATION.md`.**
  - [x] Document every new or changed directive and default.

## Backlog

These are carried over from the v0.4 burn-list (P2 product polish).

- [ ] P2: Optional event-type mapping from topic or metadata.
- [ ] P2: Optional payload envelope customization for raw payload-only events.
- [ ] P2: Expose the NATS server version and stream metadata in health or
  debug output, gated appropriately.
- [ ] P2: Configurable retry hints in SSE output. [#105] and Phase 4 deliver
  part of this.
- [ ] P2: A sample JavaScript client helper for replay-aware subscriptions,
  paired with [#102] and [#105].

## Exit criteria

- [ ] Every critical and major issue in M9, M10 and M11 is closed, or rejected
  with the rationale recorded in the issue.
- [ ] CI is green on `main`.
- [ ] `govulncheck ./...` and `govulncheck -test ./...` are clean.
- [ ] Every release tag has a published, signed Docker image.
- [ ] The delivery-contract suite passes these scenarios:
  - [ ] contiguous ids across a NATS link loss;
  - [ ] a 3000-message backlog replays on one connection at default settings;
  - [ ] a 1000-message burst causes no `slow_client`;
  - [ ] a reload before the first message loses nothing;
  - [ ] an EventSource-style reconnect with `?last-id` in the URL resumes from
    the header cursor;
  - [ ] a recreated stream resumes correctly;
  - [ ] a consumer deleted mid-stream is recovered.
- [ ] The functional matrix is green on every supported server (per D3),
  including 2.15.
- [ ] The unit package runs in 20 s or less, and no fixed sleep is used as
  synchronisation.
- [ ] Statement mutants mA, mB, mE, mF, mI and mW are killed, and the gremlins
  baseline is re-recorded.
- [ ] README, `docs/CONFIGURATION.md`, `docs/OPERATIONS.md` and
  `docs/PERFORMANCE.md` match the shipped behaviour, and a test verifies the
  memory formula.

## Issue index

Every open issue appears in exactly one phase. The exception is [#109], which
is split between Phases 1 and 5.

| Phase | Issues |
| --- | --- |
| 1: CI and critical fix | [#108] [#114] [#109] |
| 2: Test foundation | [#125] [#58] [#127] [#126] [#128] |
| 3: Replay and cursor correctness | [#97] [#103] [#98] [#115] [#106] [#101] [#102] [#104] [#83] |
| 4: Delivery pipeline | [#116] [#99] [#100] [#120] [#85] [#53] [#54] [#121] [#107] |
| 5: NATS compatibility | [#109] [#111] [#110] [#112] [#113] [#117] [#105] [#73] [#75] [#72] [#74] |
| 6: Scalability and performance | [#119] [#118] [#122] [#123] [#124] |
| 7: Control-path hardening | [#78] [#82] [#77] [#79] [#80] [#81] [#84] |
| 8: Test hardening | [#129] [#130] [#131] [#61] [#63] [#64] [#65] [#66] [#67] [#68] [#69] [#87] |
| 9: Docs and operations | [#70] [#71] [#86] |

[M9]: https://github.com/ideaconnect/nuts/milestone/9
[M10]: https://github.com/ideaconnect/nuts/milestone/10
[M11]: https://github.com/ideaconnect/nuts/milestone/11
[#53]: https://github.com/ideaconnect/nuts/issues/53
[#54]: https://github.com/ideaconnect/nuts/issues/54
[#58]: https://github.com/ideaconnect/nuts/issues/58
[#61]: https://github.com/ideaconnect/nuts/issues/61
[#63]: https://github.com/ideaconnect/nuts/issues/63
[#64]: https://github.com/ideaconnect/nuts/issues/64
[#65]: https://github.com/ideaconnect/nuts/issues/65
[#66]: https://github.com/ideaconnect/nuts/issues/66
[#67]: https://github.com/ideaconnect/nuts/issues/67
[#68]: https://github.com/ideaconnect/nuts/issues/68
[#69]: https://github.com/ideaconnect/nuts/issues/69
[#70]: https://github.com/ideaconnect/nuts/issues/70
[#71]: https://github.com/ideaconnect/nuts/issues/71
[#72]: https://github.com/ideaconnect/nuts/issues/72
[#73]: https://github.com/ideaconnect/nuts/issues/73
[#74]: https://github.com/ideaconnect/nuts/issues/74
[#75]: https://github.com/ideaconnect/nuts/issues/75
[#77]: https://github.com/ideaconnect/nuts/issues/77
[#78]: https://github.com/ideaconnect/nuts/issues/78
[#79]: https://github.com/ideaconnect/nuts/issues/79
[#80]: https://github.com/ideaconnect/nuts/issues/80
[#81]: https://github.com/ideaconnect/nuts/issues/81
[#82]: https://github.com/ideaconnect/nuts/issues/82
[#83]: https://github.com/ideaconnect/nuts/issues/83
[#84]: https://github.com/ideaconnect/nuts/issues/84
[#85]: https://github.com/ideaconnect/nuts/issues/85
[#86]: https://github.com/ideaconnect/nuts/issues/86
[#87]: https://github.com/ideaconnect/nuts/issues/87
[#92]: https://github.com/ideaconnect/nuts/pull/92
[#96]: https://github.com/ideaconnect/nuts/pull/96
[#97]: https://github.com/ideaconnect/nuts/issues/97
[#98]: https://github.com/ideaconnect/nuts/issues/98
[#99]: https://github.com/ideaconnect/nuts/issues/99
[#100]: https://github.com/ideaconnect/nuts/issues/100
[#101]: https://github.com/ideaconnect/nuts/issues/101
[#102]: https://github.com/ideaconnect/nuts/issues/102
[#103]: https://github.com/ideaconnect/nuts/issues/103
[#104]: https://github.com/ideaconnect/nuts/issues/104
[#105]: https://github.com/ideaconnect/nuts/issues/105
[#106]: https://github.com/ideaconnect/nuts/issues/106
[#107]: https://github.com/ideaconnect/nuts/issues/107
[#108]: https://github.com/ideaconnect/nuts/issues/108
[#109]: https://github.com/ideaconnect/nuts/issues/109
[#110]: https://github.com/ideaconnect/nuts/issues/110
[#111]: https://github.com/ideaconnect/nuts/issues/111
[#112]: https://github.com/ideaconnect/nuts/issues/112
[#113]: https://github.com/ideaconnect/nuts/issues/113
[#114]: https://github.com/ideaconnect/nuts/issues/114
[#115]: https://github.com/ideaconnect/nuts/issues/115
[#116]: https://github.com/ideaconnect/nuts/issues/116
[#117]: https://github.com/ideaconnect/nuts/issues/117
[#118]: https://github.com/ideaconnect/nuts/issues/118
[#119]: https://github.com/ideaconnect/nuts/issues/119
[#120]: https://github.com/ideaconnect/nuts/issues/120
[#121]: https://github.com/ideaconnect/nuts/issues/121
[#122]: https://github.com/ideaconnect/nuts/issues/122
[#123]: https://github.com/ideaconnect/nuts/issues/123
[#124]: https://github.com/ideaconnect/nuts/issues/124
[#125]: https://github.com/ideaconnect/nuts/issues/125
[#126]: https://github.com/ideaconnect/nuts/issues/126
[#127]: https://github.com/ideaconnect/nuts/issues/127
[#128]: https://github.com/ideaconnect/nuts/issues/128
[#129]: https://github.com/ideaconnect/nuts/issues/129
[#130]: https://github.com/ideaconnect/nuts/issues/130
[#131]: https://github.com/ideaconnect/nuts/issues/131
