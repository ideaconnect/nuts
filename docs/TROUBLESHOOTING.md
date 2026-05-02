# Troubleshooting

Start with the probe split:

```bash
curl -i http://localhost:8080/events/livez
curl -i http://localhost:8080/events/readyz
```

`/livez` proves the Caddy process can serve HTTP. `/readyz` also verifies the
NATS connection and configured JetStream stream.

## Browser Connects But Receives No Messages

Check the effective NATS subject first:

1. Confirm the client topic and `topic_prefix` combine into the stream subject.
   `topic=orders` plus `topic_prefix events.` subscribes to `events.orders`.
2. Confirm the route strips its public prefix before `nuts` sees the path. A
   request to `/events/orders` should reach NUTS as `/orders`, not
   `/events/orders`.
3. Verify the stream captures the subject:

   ```bash
   nats stream info EVENTS
   nats pub events.orders '{"hello":"world"}'
   ```

4. Watch logs for `failed to subscribe` or `subject_label` fields that reveal
   the subject NUTS actually requested.

## Browser Reports CORS Errors

Native `EventSource` can send cookies with `withCredentials: true`, but it
cannot set custom `Authorization` headers. If a browser needs credentialed CORS,
configure explicit origins rather than `*`:

```caddyfile
allowed_origins https://app.example.com https://admin.example.com
```

Then compare the browser request with a direct preflight-style check:

```bash
curl -i \
  -H 'Origin: https://app.example.com' \
  -H 'Access-Control-Request-Method: GET' \
  -X OPTIONS \
  http://localhost:8080/events?topic=orders
```

Expected successful responses include `Access-Control-Allow-Origin` echoing the
request origin. `Access-Control-Allow-Credentials: true` appears only for
explicitly allow-listed origins.

## Requests Return 400

Common causes:

- No `?topic=` and no path shorthand topic after route-prefix stripping.
- Invalid topic characters or empty topic tokens.
- Bad `?last-id=` value. Query `last-id` must parse as an unsigned integer.

Use a minimal request while debugging:

```bash
curl -i -N 'http://localhost:8080/events?topic=orders'
```

## Requests Return 503

Check the response body and logs:

- `JetStream not available`: the handler is not provisioned or NATS setup
  failed.
- `Too many concurrent connections`: `max_connections` has been reached.
- `Failed to subscribe`: the stream subjects, NATS permissions, or NATS server
  compatibility need attention.
- `/readyz` degraded: NATS is disconnected or the stream is unavailable.

Metrics that help narrow this down include
`nuts_connections_rejected_total{reason="max_connections"}` and
`nuts_subscription_errors_total`.

## Replay Re-sends Too Many Events

When `last-id` or `Last-Event-ID` points below JetStream retention, NUTS falls
back to retained replay. On large streams, set at least one bound:

```caddyfile
replay_max_messages 1000
replay_window 300
```

Look for `replay_mode`, `replay_start_sequence`, and
`replay_fallback_reason` in structured logs. If fallback happens often,
increase JetStream retention, reduce client outage windows, or tune replay
bounds to the largest recovery burst you are willing to serve.

## Slow Clients Disconnect

This is intentional backpressure. When a client's queue fills, NUTS disconnects
the SSE stream instead of silently dropping live messages. The browser should
reconnect and use `Last-Event-ID` to resume.

Useful checks:

- `nuts_slow_client_disconnects_total`
- `disconnect_reason="slow_client"` in logs
- `client_buffer_size`, `max_event_size`, and the memory formula in
  [PERFORMANCE.md](PERFORMANCE.md)
- `write_timeout` for blocked downstream writes and `dispatch_timeout` for
  saturated slow-client signal waits
- Proxy buffering between Caddy and the browser

## Docker Image Starts But Config Looks Wrong

The shipped Caddyfile reads only these environment variables:

| Variable | Directive |
| --- | --- |
| `NATS_URL` | `nats_url` |
| `STREAM_NAME` | `stream_name` |
| `TOPIC_PREFIX` | `topic_prefix` |

Other directives must be added to the mounted Caddyfile explicitly. Validate the
production image config with:

```bash
docker run --rm idcttech/nuts:<version> /app/caddy adapt --config /app/Caddyfile
```

## Functional Tests Fail Locally

Use Make so Docker services are started, waited on, and cleaned up consistently:

```bash
make test-functional
```

If a run fails, service logs are printed automatically. For repeated flake
checks, run:

```bash
make test-functional-stress FUNCTIONAL_TEST_STRESS_COUNT=3
```
