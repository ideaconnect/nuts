# Performance And Load Confidence

This document defines the current performance confidence suite and the budgets
that deployments should use as starting points. The tests are intentionally
bounded so they can run in normal CI; production load testing should repeat the
same scenarios with the real NATS topology, payload shapes, browser mix, and
container limits.

## How To Run

Run the CI-sized confidence tests:

```bash
go test -run '^TestPerformance_' -timeout 180s .
```

Run the hot-path benchmarks with allocation reporting:

```bash
go test -run '^$' -bench 'Benchmark(FormatMessageEvent|FormatMessageEventLarge|WriteJSONPayload|IsValidTopic|StreamFeed|SharedFanOut)' -benchmem .
```

Or run both through Make:

```bash
make test-performance
```

## CI Budgets

| Area | Budget | Coverage |
| --- | --- | --- |
| Concurrent live delivery | 16 SSE clients, 40 JSON messages at 100 messages/second, final message visible to every client within 5 seconds | `TestPerformance_ConcurrentSSEClientsReceiveRealisticMessageRate` |
| Stalled readers | A client that stops reading while 400 × 60 KiB messages are published is disconnected within 3 seconds of `write_timeout 1`, and goroutines return to the pre-request baseline plus 3 | `TestPerformance_SlowReaderDisconnectsWithoutGoroutineLeak` |
| Replay without caps | 160 retained messages replay within 5 seconds at the default `client_buffer_size` | `TestPerformance_ReplayLoadWithAndWithoutFallbackCaps` |
| Replay with caps | `replay_max_messages 25` closes the replaying stream after exactly 25 historical message events within 5 seconds | `TestPerformance_ReplayLoadWithAndWithoutFallbackCaps` |
| Large payload memory | Repeated 64 KiB payload formatting retains less than 32 MiB of extra heap after GC, and the payload survives formatting | `TestPerformance_MemoryGrowthLargePayloadFormattingWithinBudget` |
| Replay memory | Large retained replay scenarios grow heap by less than 32 MiB during the CI-sized run | `TestPerformance_ReplayLoadWithAndWithoutFallbackCaps` |
| Shared fan-out | With `shared_subscriptions`, 300 connections receive a burst of 4 × 64 KiB messages within 2 seconds through one consumer, without a NATS reconnect | `TestPerformance_SharedFanOutBurst` |

The delivery contract tests (`delivery_contract_test.go`) add correctness
budgets under load: a 3000-message backlog replays on one connection at the
default settings, and a 1000-message burst reaches every connected client
without a slow-client disconnect.

The benchmarks cover SSE event formatting, JSON compaction, topic validation,
the feed that pulls, formats and hands messages to the writer, and handing a
formatted frame to the connections of a shared subscription (about 110 ns and
no allocation per delivery, against about 0.4 µs and 1 allocation to format a
small message once per connection). The formatter builds each frame in one
pass: about 0.4 µs for a small JSON message and 320 µs for 64 KiB, with one
allocation.

## Production Targets

Use these as release gates before increasing traffic or connection limits:

- **Latency:** p95 server-side publish-to-SSE visibility should stay below
  250 ms for the expected message rate, payload size, NATS distance, and
  browser count. The CI budget is deliberately looser because it uses an
  embedded NATS server and shared test runner resources.
- **Memory per connection:** each connection holds at most
  `client_buffer_size` messages prefetched from JetStream, plus two formatted
  frames on their way to the client:
  `client_buffer_size * M + 2 * F + 256 KiB connection overhead`, where `M`
  is the largest message the stream accepts (its `max_msg_size`, or the
  server's `max_payload`, 1 MiB by default) and `F` is `max_event_size`.
  Prefetched messages are raw JetStream messages, so `max_event_size` does not
  bound them: an oversized message is dropped only after it was pulled.
  Across an instance, keep `max_connections` × that figure below 70% of the
  container or VM memory limit.
- **Maximum sustainable clients per instance:** the default production target
  is 1,000 concurrent light-traffic SSE clients per instance when messages are
  at most 64 KiB and `client_buffer_size` is at most 8, and the memory formula
  leaves headroom. Every client also holds one JetStream consumer, so the
  stream's `max_consumers` must cover all instances. Lower the cap when
  payloads, replay windows, or edge authentication costs are higher; raise it
  only after an environment-specific load run meets the latency and memory
  budgets above.
- **Fan-out:** without `shared_subscriptions`, every connection's consumer
  sends its own copy of each message over NUTS' single NATS connection, and
  nats-server disconnects a client whose pending data exceeds 64 MiB
  (`max_pending`). 500 connections receiving 4 × 64 KiB took 2.6 s with one
  such reconnect, 1000 connections about 14 s. With `shared_subscriptions`
  the same bursts took 0.12 s and 0.25 s. Turn it on when many clients
  share topics, especially with large messages, or raise the server's
  `max_pending`.
- **Replay safety:** configure at least one of `replay_max_messages` or
  `replay_window` for public or multi-tenant routes. A long replay no longer
  disconnects the client, but it keeps a consumer and a connection busy for
  as long as the backlog takes to deliver.
- **Downstream stalls:** `write_timeout` (30 s by default) frees the
  connection, its consumer and its memory when a client or proxy stops
  reading. Shorten it on public routes where stalled connections are common.

Record benchmark output and load-test parameters with release artifacts when
changing formatter code, replay behavior, client buffering, or NATS versions.
