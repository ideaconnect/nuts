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
   the subject NUTS actually requested. A topic outside the stream's subjects
   is refused with `503 Failed to subscribe to requested topics`.
5. For multi-topic requests (`?topic=a&topic=b`), check the server version:
   before 2.14.7, a purge or roll-up of one subject can make the stream skip
   pending messages of the others. NUTS logs a warning at startup on such
   servers.

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
  A bad `Last-Event-ID` header is only logged and ignored, since browsers
  resend it on every reconnect.
- More distinct topics than `max_topics_per_subscription` (32 by default).

Use a minimal request while debugging:

```bash
curl -i -N 'http://localhost:8080/events?topic=orders'
```

## Requests Return 503 Or 429

Check the response body and logs:

- `JetStream not available` (503): NATS is disconnected, or the handler is
  shutting down.
- `Too many concurrent connections` (429): `max_connections` has been reached.
- `Stream consumer limit reached` (503): the stream's `max_consumers` is
  exhausted; every SSE connection holds one consumer. On nats-server 2.15 the
  default is 1000 per stream. See the runbook in
  [OPERATIONS.md](OPERATIONS.md).
- `Failed to subscribe to requested topics` (503): a topic outside the
  stream's subjects, missing NATS permissions, or a JetStream API error; the
  log's `jetstream_error_code` names the server's reason.
- `/readyz` degraded: NATS is disconnected or the stream is unavailable.

Browsers see none of these as errors: requests sent with
`Accept: text/event-stream`, as `EventSource` does, get a `200` stream with a
`retry:` delay for every transient case, and the browser reconnects on its
own. Only the topic error stays a `503`, since retrying cannot fix it.

When NATS stops answering without closing the connection (packets dropped, a
paused VM or container), NUTS only learns that it is disconnected once two
pings go unanswered, which takes several minutes with nats.go's default
two-minute ping interval. Until then each new request waits for its JetStream
calls to time out, about 7 to 10 seconds, before it gets one of the answers
above. Streams that are already open stay open and continue without a gap
once NATS answers again.

Metrics that help narrow this down include
`nuts_connections_rejected_total{reason}` and
`nuts_subscription_errors_total`.

## Replay Re-sends Too Many Events

When `last-id` or `Last-Event-ID` points below JetStream retention, or ahead
of the stream after it was recreated, NUTS falls back to retained replay. On
large streams, set at least one bound:

```caddyfile
replay_max_messages 1000
replay_window 300
```

Look for `replay_mode`, `replay_start_sequence`, and
`replay_fallback_reason` in structured logs. If fallback happens often,
increase JetStream retention, reduce client outage windows, or tune replay
bounds to the largest recovery burst you are willing to serve.

## Slow Clients Disconnect

A client that reads slowly is not disconnected: NUTS stops pulling from
JetStream until it catches up. A client whose writes stop completing is
disconnected once a write misses `write_timeout` (30 s by default), and
resumes from its `Last-Event-ID`.

Useful checks:

- `nuts_slow_client_disconnects_total`
- `disconnect_reason="slow_client"` in logs
- `write_timeout`, and whether a proxy between Caddy and the browser buffers
  or stalls the response (it should honour `X-Accel-Buffering: no`)
- `client_buffer_size` and the memory formula in
  [PERFORMANCE.md](PERFORMANCE.md)

## Messages Missing After A NATS Outage

They should not be: each stream's consumer recreates itself from the last
delivered sequence after a reconnect, and
`nuts_consumer_invalidated_total{reason="recreated"}` counts each recovery. If
a stream instead closes with `disconnect_reason=consumer_unrecoverable`, the
client reconnects with its last event ID. Check that the requested sequence is
still retained (`replay_fallback_reason` in the logs) and, for multi-topic
streams, the nats-server version.

## Streams Stall After The Stream Was Recreated

Deleting a stream and creating it again (or restoring an older backup of it)
while clients are connected can leave their open SSE streams silent. Each
connection's consumer resumes after the last stream sequence it delivered, and
the new stream numbers its messages from 1 again, so those connections receive
nothing until the new stream passes their old position. Heartbeats continue,
so neither the browser nor the readiness probe notices.

Clients that connect afterwards are not affected: a `Last-Event-ID` or
`?last-id=` ahead of the stream falls back to the retained replay
(`replay_window` when configured), as the `replay fallback` log line shows.

After recreating a stream under live traffic, reload Caddy with
`caddy reload --force` (a reload with an unchanged config is skipped) or
restart it. Every stream closes, clients reconnect with their last event ID,
and each one takes the fallback above. Operations that keep the stream,
such as `nats stream purge`, do not reset its sequence numbers and need
nothing.

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
