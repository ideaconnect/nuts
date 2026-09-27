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
| `disconnect_reason` | Why an SSE stream closed or was refused: `client_context_done`, `slow_client`, `write_error`, `heartbeat_write_error`, `replay_cap_reached`, `consumer_unrecoverable`, `handler_shutdown`, `jetstream_unavailable`, `max_connections`, `stream_consumer_limit` or `subscription_failed` |

Use `subject_label`, `replay_mode`, and `disconnect_reason` as the first
filters when correlating logs with alerts.

## Incident: NATS Down

**Signals**

- `/readyz` returns `503` with `"nats":"disconnected"`.
- Blackbox readiness panel drops to `0` while `/livez` remains `200`.
- Logs contain `disconnected from NATS`; later recovery logs contain
  `reconnected to NATS`. A server that stopped answering without closing the
  connection shows up as `stale connection`, two to three
  `nats_ping_interval`s (40–60 s by default) after it went quiet; until then
  new requests wait about 7 s for their JetStream timeouts before being
  refused.
- New stream requests are refused at once with
  `disconnect_reason=jetstream_unavailable`: plain clients get `503` with
  `Retry-After`, browsers a `retry:` stream, so `EventSource` keeps
  reconnecting.
- Open streams stay open. After the reconnect their consumers recreate
  themselves from the last delivered sequence
  (`nuts_consumer_invalidated_total{reason="recreated"}`); streams whose
  consumer cannot be recreated close with
  `disconnect_reason=consumer_unrecoverable` and resume from the client's
  last event ID.

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
4. Clients connected while the stream was recreated need nothing: NUTS
   notices the new stream on the next request or within 10 seconds, closes
   their streams with `disconnect_reason=stream_recreated`, and they replay
   the new stream from its start. On nats-server 2.14 and earlier a restore
   keeps the creation time and rewinds the stream instead; NUTS notices that
   too (`disconnect_reason=stream_rewound`), and clients replay the restored
   stream from its start. See
   [TROUBLESHOOTING.md](TROUBLESHOOTING.md#streams-close-after-the-stream-was-recreated-or-restored).

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
   `fallback_start_time`; fallback logs include `replay_fallback_reason`:
   `sequence below retention`, `sequence outside replay window`,
   `cursor ahead of stream` (a recreated or restored stream) or
   `stream info unavailable`.

**Actions**

1. Check whether clients are reconnecting with very old `last-id` or
   `Last-Event-ID` values.
2. Set or lower `replay_max_messages` and/or `replay_window` for public or
   multi-tenant routes; these bounds apply to retained replay, not only purged
   cursor fallback.
3. Review JetStream retention. A short retention window increases fallback
   frequency; a very long retained backlog makes each fallback more expensive.

## Incident: Slow clients

**Signals**

- `nuts_slow_client_disconnects_total` increases.
- Logs show `disconnect_reason="slow_client"`: a write to the client missed
  `write_timeout`.

**Cause**

A client that reads slowly is never disconnected for it: NUTS pulls from
JetStream only as fast as the client reads, and the backlog waits in the
stream. A slow-client disconnect means writes stopped completing altogether
for `write_timeout` (30 s by default), usually a client that went away without
closing the connection, or a proxy that stopped forwarding.

**Actions**

1. Identify affected `subject_label` values and client cohorts.
2. Inspect proxies between Caddy and the browser for response buffering; they
   should honour `X-Accel-Buffering: no`.
3. Confirm clients resume with `Last-Event-ID`; the disconnect is designed to
   end in a resume, not a loss.
4. Shorten `write_timeout` to free stalled connections, and their consumers,
   sooner.
5. `nuts_nats_async_errors_total{kind="slow_consumer"}` counts overflow in
   nats.go's own subscription buffers, before NUTS sees a message. It should
   stay at zero, because each stream pulls at most `client_buffer_size`
   messages at a time; a rising rate points at the NATS connection itself
   (CPU starvation or a saturated link).

## Incident: Consumer recreated or unrecoverable

**Signals**

- `nuts_consumer_invalidated_total{reason="recreated"}` increases: a stream's
  consumer was lost (a NATS reconnect, missed pull heartbeats, a consumer
  deleted or reaped on the server) and recreated itself from the last
  delivered sequence. The client noticed nothing, and the log line
  `JetStream consumer recreated` names the previous consumer.
- `nuts_consumer_invalidated_total{reason="unrecoverable"}` increases and
  streams close with `disconnect_reason="consumer_unrecoverable"`:
  recreation kept failing for about 75 seconds (10 attempts), so the stream
  ended and the client reconnects with its last event ID.
- `nuts_consumer_invalidated_total{reason="stream_recreated"}` or
  `{reason="stream_rewound"}` increases: the JetStream stream itself was
  deleted and created again, or restored from a backup, under open streams.
  Their consumers were positioned on the old stream, so the streams closed
  and their clients reconnect onto the new one; see
  [TROUBLESHOOTING.md](TROUBLESHOOTING.md#streams-close-after-the-stream-was-recreated-or-restored).
  If nobody recreated the stream, look for a job that creates it on startup:
  a memory-storage stream is lost when its server restarts.

**Actions**

1. Cross-reference `nuts_nats_connection_events_total{event="disconnect"}`
   first. A reconnect recreates every open stream's consumer, so one blip
   shows up once per open stream.
2. Without a matching disconnect, look for consumers deleted on the server:
   an operator or tool deleting `nuts_*` consumers, or a stream's
   `consumer_limits.inactive_threshold` reaping consumers of stalled
   streams.
3. For `unrecoverable`, check JetStream availability and the stream's
   consumer limit (see below): recreation needs to create a consumer.

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
2. Check `write_timeout` for the network path (30 s by default; `-1`
   leaves it to Caddy and the underlying HTTP stack).
3. For chronic `message`-site failures, inspect client behaviour —
   browsers under heavy main-thread load can stall their event loop
   long enough to trip `write_timeout`.

## Incident: Connections keep falling off shared subscriptions

**Signals**

- `nuts_shared_transitions_total{transition="fell_behind"}` rises steadily
  (with `shared_subscriptions` on).
- The stream's consumer count stays above `nuts_shared_subscriptions`:
  connections that fell behind catch up on their own consumers.

**Actions**

1. Nothing is lost: a connection that falls behind continues on its own
   consumer and rejoins once caught up. The cost is the extra consumers.
2. Raise `client_buffer_size`, the per-connection queue on a shared
   subscription, if bursts are larger than it.
3. Look for slow clients or proxies, as in "Slow clients" above.
4. `transition="shared_failed"` means a shared consumer could not be
   recreated; see "Consumer recreated or unrecoverable".

## Incident: Multi-topic streams skip messages

**Signals**

- Clients subscribed to several topics (`?topic=a&topic=b`) miss messages on
  one topic, typically after a subject purge, a `Nats-Rollup: sub` publish,
  per-message TTL delete markers or a message schedule firing on another.
- At startup, NUTS logged a warning that the server can skip messages on
  multi-topic subscriptions.

**Actions**

1. Upgrade nats-server to 2.14.7 or newer (2.15 recommended). Older releases,
   including every 2.10.x and 2.12.x, move a multi-filter consumer past
   pending messages when one of its subjects is purged (nats-server#8572).
2. Until then, subscribe to one topic per connection on affected routes.

## Incident: Stream consumer limit reached

**Signals**

- `nuts_connections_rejected_total{reason="stream_consumer_limit"}` increases.
- Logs at Warn level with `disconnect_reason="stream_consumer_limit"`.
- Clients get `503 Stream consumer limit reached` (browsers: a `retry:`
  stream, so they keep reconnecting).
- At startup, a Warn that the stream's `max_consumers` is below
  `max_connections`.

**Cause**

Every SSE connection holds one JetStream consumer, and the stream refuses new
consumers beyond `max_consumers`. From nats-server 2.15 that limit is 1000 per
stream unless the stream or account sets a positive value; `-1` does not
lift it. All NUTS instances and other applications on the stream share it.

**Steps**

1. Check the stream: `nats stream info EVENTS` (`Maximum Consumers`) and the
   current count (`Consumers`).
2. Raise it for peak concurrent connections across every NUTS instance:
   `nats stream edit EVENTS --max-consumers 10000`, or set
   `default_max_consumers: -1` in the server's JetStream limits.
3. Keep `max_connections` × instances at or below the limit, so clients get
   the cheaper `max_connections` rejection first.

## Incident: Streams answer 500 "Streaming not supported"

**Cause**

The response writer NUTS received cannot flush. Before v0.4.3 this happened
whenever Caddy's `log` directive or HTTP request metrics were enabled, because
Caddy wraps the writer in a recorder without `http.Flusher`. NUTS now flushes
through `http.ResponseController`, which follows `Unwrap`.

**Actions**

1. Upgrade to v0.4.3 or newer.
2. If it persists, look for a third-party handler in the route that wraps the
   response writer without `Flush` or `Unwrap` methods.

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
