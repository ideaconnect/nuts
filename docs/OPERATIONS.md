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
