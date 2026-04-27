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
go test -run '^$' -bench 'Benchmark(FormatMessageEvent|TryParseJSON|IsValidTopic|CommonSubjectFilter|MultiTopicRequestedMessageHandler)' -benchmem .
```

Or run both through Make:

```bash
make test-performance
```

## CI Budgets

| Area | Budget | Coverage |
| --- | --- | --- |
| Concurrent live delivery | 16 SSE clients, 40 JSON messages at 100 messages/second, final message visible to every client within 5 seconds | `TestPerformance_ConcurrentSSEClientsReceiveRealisticMessageRate` |
| Slow readers | A replaying client with a saturated 4-message queue disconnects within 3 seconds and handler goroutines return to baseline plus 20 | `TestPerformance_SlowReaderDisconnectsWithoutGoroutineLeak` |
| Replay without fallback caps | 160 retained messages replay within 5 seconds when the queue is sized for that replay | `TestPerformance_ReplayLoadWithAndWithoutFallbackCaps` |
| Replay with fallback caps | `replay_max_messages 25` closes the fallback stream after exactly 25 message events within 5 seconds | `TestPerformance_ReplayLoadWithAndWithoutFallbackCaps` |
| Large payload memory | Repeated 64 KiB payload formatting retains less than 32 MiB of extra heap after GC | `TestPerformance_MemoryGrowthLargePayloadFormattingWithinBudget` |
| Replay memory | Large retained replay scenarios grow heap by less than 32 MiB during the CI-sized run | `TestPerformance_ReplayLoadWithAndWithoutFallbackCaps` |

The benchmarks cover SSE event formatting, JSON compaction, topic validation,
common wildcard calculation for multi-topic subscriptions, and in-process
filtering used when NATS multi-filter consumers are unavailable.

## Production Targets

Use these as release gates before increasing traffic or connection limits:

- **Latency:** p95 server-side publish-to-SSE visibility should stay below
  250 ms for the expected message rate, payload size, NATS distance, and
  browser count. The CI budget is deliberately looser because it uses an
  embedded NATS server and shared test runner resources.
- **Memory per connection:** keep the configured queued-payload ceiling within
  the instance memory budget:
  `client_buffer_size * max_event_size + 256 KiB connection overhead`.
  Across an instance, keep
  `max_connections * (client_buffer_size * max_event_size + 256 KiB)` below
  70% of the container or VM memory limit. `max_event_size -1` removes this
  bound and should only be used behind trusted producers and separate memory
  controls.
- **Maximum sustainable clients per instance:** the default production target
  is 1,000 concurrent light-traffic SSE clients per instance when
  `max_event_size <= 64 KiB`, `client_buffer_size <= 8`, and the memory formula
  leaves headroom. Lower the cap when payloads, replay windows, or edge
  authentication costs are higher; raise it only after an environment-specific
  load run meets the latency and memory budgets above.
- **Replay safety:** configure at least one of `replay_max_messages` or
  `replay_window` for public or multi-tenant routes. Size `client_buffer_size`
  for the largest replay burst you are willing to serve without treating the
  client as slow.

Record benchmark output and load-test parameters with release artifacts when
changing formatter code, replay behavior, client buffering, or NATS versions.
