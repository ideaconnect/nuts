# Operations Runbook

This runbook covers the common production incidents for a NUTS route and ties
them to probes, metrics, dashboard panels, and structured log fields.

## Probes

- `live_path` defaults to `/livez` and returns process liveness only. Use it
  for Kubernetes liveness probes so a temporary NATS outage does not restart a
  healthy Caddy process.
- `ready_path` defaults to `/readyz` and checks both the NATS connection and
  the configured JetStream stream. Use it for readiness probes and load
  balancer target health.
- `health_path` defaults to `/healthz` and remains a backward-compatible
  readiness-style probe with the same NATS and stream checks as `ready_path`.

Example Kubernetes split:

```yaml
livenessProbe:
  httpGet:
    path: /events/livez
    port: http
readinessProbe:
  httpGet:
    path: /events/readyz
    port: http
```

## Metrics And Dashboards

- Example alert rules: [ops/prometheus-alerts.yml](../ops/prometheus-alerts.yml)
- Example Grafana dashboard: [ops/grafana-dashboard.json](../ops/grafana-dashboard.json)

The dashboard expects Caddy's Prometheus metrics scrape to include the `nuts_*`
series. The NATS connectivity panel is designed for a blackbox-exporter scrape
of the NUTS readiness endpoint, because readiness is exposed as JSON rather
than a package-level Prometheus gauge.

## Structured Log Fields

Streaming logs consistently include these fields when a request plan exists:

| Field | Meaning |
| --- | --- |
| `topics` | Browser-requested topic names after de-duplication |
| `subjects` | Full NATS subjects after applying `topic_prefix` |
| `subject_label` | Comma-joined subject label for compact filtering |
| `replay_mode` | `deliver_new`, `start_sequence`, `fallback_deliver_all`, or `fallback_start_time` |
| `replay_start_sequence` | JetStream sequence requested by `last-id` / `Last-Event-ID`, when present |
| `replay_fallback_reason` | Why NUTS fell back from a requested sequence, when present |
| `disconnect_reason` | Why an SSE stream closed, such as `slow_client`, `replay_cap_reached`, `handler_shutdown`, or `write_error` |

Use `subject_label`, `replay_mode`, and `disconnect_reason` as the first
filters when correlating logs with alerts.

## Incident: NATS Down

**Signals**

- `/readyz` returns `503` with `"nats":"disconnected"`.
- Blackbox readiness panel drops to `0` while `/livez` remains `200`.
- Logs contain `disconnected from NATS`; later recovery logs contain
  `reconnected to NATS`.

**Actions**

1. Confirm Caddy is live with `/livez`; do not restart solely because NATS is
   temporarily unavailable.
2. Check NATS server or cluster health, credentials, TLS configuration, and
   network policy between Caddy and NATS.
3. Watch for `reconnected to NATS` and confirm `/readyz` returns `200` before
   putting the instance back behind the load balancer.

## Incident: Stream Missing

**Signals**

- Provisioning fails with `JetStream stream '<name>' not found`.
- `/readyz` returns `503` with `"stream":"unavailable"` if the stream is
  deleted after startup.
- `nuts_subscription_errors_total` may increase for affected requests.

**Actions**

1. Confirm the stream name in Caddy config matches the NATS stream.
2. Run `nats stream info <STREAM>` and verify the configured subject filters
   cover the expected `topic_prefix`.
3. Recreate or restore the stream before routing traffic to the NUTS instance.

## Incident: Oversized messages dropped

**Signals**

- `nuts_messages_dropped_total{reason="raw_payload"}` increases —
  inbound NATS payload exceeded `max_event_size`.
- `nuts_messages_dropped_total{reason="formatted_sse_message"}` increases
  — payload fit but the SSE envelope (JSON wrap + `id`/`event`/`data`
  lines) pushed the frame over `max_event_size`.
- Logs at Warn level with `dropping oversized NATS payload` or
  `dropping oversized SSE event` carry the offending topic and size.

**Actions**

1. `raw_payload` drops point at producer-side: a NATS subject is
   carrying messages larger than NUTS is configured to deliver.
   Either fix the producer or raise `max_event_size` after checking
   the per-connection memory budget in [PERFORMANCE.md](PERFORMANCE.md).
2. `formatted_sse_message` drops are envelope overhead on small but
   pathological payloads (deeply nested JSON, escape-heavy strings).
   Raising `max_event_size` by a modest amount (~25%) usually resolves
   these without producer-side changes.

## Incident: Replay Storm

**Signals**

- `nuts_replay_fallbacks_total` spikes.
- `nuts_replay_cap_reached_total` rises if `replay_max_messages` is configured.
- Logs may show `replay_mode` as `start_sequence`, `fallback_deliver_all`, or
   `fallback_start_time`; fallback logs include `replay_fallback_reason` such as
   `sequence below retention` or `sequence outside replay window`.

**Actions**

1. Check whether clients are reconnecting with very old `last-id` or
   `Last-Event-ID` values.
2. Set or lower `replay_max_messages` and/or `replay_window` for public or
   multi-tenant routes; these bounds apply to retained replay, not only purged
   cursor fallback.
3. Review JetStream retention. A short retention window increases fallback
   frequency; a very long retained backlog makes each fallback more expensive.

## Incident: Slow Consumers

**Signals**

- `nuts_slow_client_disconnects_total` increases quickly.
- `nuts_nats_async_errors_total{kind="slow_consumer"}` also increases —
  this counts drops at the nats.go-internal per-subscription buffer
  layer (500k msg / 64 MB default), which fire BEFORE NUTS sees the
  message at all. A non-zero rate here means NATS is shedding traffic
  upstream of NUTS, not just slow SSE clients.
- Logs show `disconnect_reason="slow_client"`, `slow_subject`, and
  `buffer_size`.
- Delivered-message rate may remain high while client reconnect churn rises.

**Actions**

1. Identify affected `subject_label` values and client cohorts.
2. Lower `client_buffer_size` to disconnect slow clients sooner, or raise it
   only after checking the memory budget in [PERFORMANCE.md](PERFORMANCE.md).
3. Set `write_timeout` to bound blocked SSE writes and `dispatch_timeout` to
   bound NATS callback waits when a slow-client signal is already pending.
4. Inspect downstream proxy buffering and browser/client processing speed.
5. Confirm clients resume with `Last-Event-ID`; slow disconnects are designed
   to trigger replay rather than silently drop messages.
6. If `nuts_nats_async_errors_total{kind="slow_consumer"}` dominates,
   the bottleneck is between NATS and the NUTS subscription, not
   between NUTS and SSE clients — tune the producer-side rate or scale
   NUTS horizontally.

## Incident: Consumer invalidated mid-stream

**Signals**

- `nuts_nats_async_errors_total{kind="consumer_invalidated"}` ticks
  up. The label covers three nats.go signals that all mean the
  JetStream push consumer is unusable:
  - `nats.ErrConsumerNotActive` — the primary case. nats.go's
    `activityCheck` fires when no `IdleHeartbeat` has arrived within
    the configured tolerance (typically the JetStream server reaped
    the ephemeral via `InactiveThreshold` during a network blip, or
    a leafnode route failover dropped the inbox subscription).
  - `nats.ErrConsumerDeleted` — the consumer was administratively
    deleted while NUTS held the subscription.
  - `*nats.ErrConsumerSequenceMismatch` — heartbeats DID arrive but
    the delivered consumer sequence drifted from what nats.go
    expected (interrupts ordered replay; rarer than the other two).
- After M9 Batch B ships, `nuts_consumer_invalidated_total{reason="heartbeat_missed"}`
  also ticks once per invalidation and the affected SSE handler emits
  `disconnect_reason="consumer_invalidated"`. During the Batch A window,
  only the async-errors counter increments — the affected SSE handler
  stays attached to its zombie consumer until the client reconnects for
  other reasons.

**Actions**

1. **Cross-reference `nuts_nats_connection_events_total{event="disconnect"}`
   FIRST.** A NATS-side connection blip longer than `2 × nats_idle_heartbeat`
   (default 20s with the 10s heartbeat) trips `nats.go`'s `activityCheck`
   on every active push subscription — each one fires
   `ErrConsumerNotActive` and increments `consumer_invalidated` once
   per concurrent SSE client. With N active clients you will see N
   `consumer_invalidated` ticks for what was operationally one
   disconnect, plus the matching `connection_events{event="disconnect"}`
   tick. If the `disconnect` tick correlates in time, treat this
   incident as a connection blip — not a server-side consumer reap —
   and move to the network/cluster troubleshooting flow rather than
   continuing with the steps below.
2. Check the NATS server's effective `InactiveThreshold` against NUTS'
   `nats_idle_heartbeat` setting. Two missed heartbeats must be
   detectable before the server reaps; NUTS' `Validate` enforces
   `nats_idle_heartbeat < InactiveThreshold/2` against its in-process
   constant (`defaultConsumerInactiveThreshold = 30s`), but a server-side
   override or a leafnode/cluster configuration can change the effective
   reap interval. If the server reaps faster than NUTS detects, the
   heartbeat path is silently undermined.
3. Inspect NATS server logs for ephemeral consumer creation/deletion
   churn. Leafnode route flaps and replica failovers are common upstream
   causes.
4. After Batch B is deployed, confirm clients reconnect with
   `Last-Event-ID` and resume cleanly; the disconnect is by design and
   the client-side retry is the recovery contract.
5. If the counter ticks during periods of healthy delivery (no
   coincident `connection_events{disconnect}` tick, no server-side
   consumer churn), a real delivery gap (publisher-side issue, stream
   replication divergence) may be present — cross-reference
   `nuts_replay_fallbacks_total` and the NATS server's
   `nats stream report`.

## Incident: Stalled writes

**Signals**

- `nuts_write_disconnects_total{site}` increases, labelled by SSE write
  site: `connected` (initial event), `message` (per-message frame), or
  `heartbeat` (idle keepalive). A burst on `heartbeat` typically means
  proxy buffering or a downstream connection issue; a burst on `message`
  means clients can't keep up with delivery; a burst on `connected`
  points at TLS handshake or Caddy-layer issues before NUTS could send
  its first byte.
- Logs at Warn level with `disconnect_reason="write_error"` or
  `"heartbeat_write_error"` and the matching `write_site` field carry the
  underlying error and elapsed time. (Note: every browser tab-close
  mid-message also produces a Warn-level write_error entry — under high
  client churn these will dominate the log volume; consider sampling
  in your log shipper if this is noisy.)

**Actions**

1. Cross-reference the failing `site` with downstream proxy / load-
   balancer error logs. Heartbeat-site failures are usually idle
   connections being closed by a transparent proxy.
2. Verify `write_timeout` is set to something reasonable for the
   network path (`0` leaves it to Caddy and the underlying HTTP stack).
3. For chronic `message`-site failures, inspect client behaviour —
   browsers under heavy main-thread load can stall their event loop
   long enough to trip `write_timeout`.

## Incident: Wildcard-fallback overhead on pre-2.10 NATS

**Signals**

- `nuts_wildcard_filter_drops_total` increases steadily.
- Connected NATS server reports version `< 2.10` (visible in
  `nats-server -DV` output or the `INFO` line on connect).

**Actions**

1. The wildcard fallback subscribes to the smallest common parent
   subject and filters client-side; every dropped message wasted a
   network round-trip and CPU cycle. The metric measures that waste.
2. Upgrading NATS to ≥ 2.10 enables native multi-filter consumers
   (`ConsumerFilterSubjects`) and removes the fallback entirely.
3. Until you can upgrade, narrow the wildcard by ensuring all topics in
   a single subscription share a deeper common prefix.

## Incident: CORS Misconfiguration

**Signals**

- Browsers report EventSource CORS errors while server-side probes succeed.
- OPTIONS responses do not include the expected reflected
  `Access-Control-Allow-Origin`.
- Credentialed browser flows fail when `allowed_origins *` is used.

**Actions**

1. List explicit origins when cookies or `Authorization` headers are required.
   Wildcard origins intentionally do not advertise
   `Access-Control-Allow-Credentials`.
2. Confirm `allowed_headers` includes any browser-sent custom headers.
3. Verify the route prefix is stripped before NUTS sees the request, so topic
   shorthand and probe paths are evaluated relative to the handler.
4. Use browser devtools for the failing request and compare it with a direct
   `curl -i -H 'Origin: https://example.com'` request against the same path.
