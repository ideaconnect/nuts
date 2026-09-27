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
paused VM or container), NUTS learns that it is disconnected once two pings
go unanswered: within two to three `nats_ping_interval`s, 40 to 60 seconds by
default, when it logs `disconnected from NATS` with `stale connection`. Until
then each new request waits for its JetStream calls to time out, about 7 to
10 seconds, before it gets one of the answers above; lower
`nats_ping_interval` to shorten that window. Streams that are already open
stay open and continue without a gap once NATS answers again.

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

## Streams Close After The Stream Was Recreated Or Restored

A stream deleted and created again numbers its messages from 1 again. An open
SSE stream's consumer would resume after the last sequence it delivered and
wait there, silently skipping the new stream's messages until it passed that
position. NUTS therefore reads the stream's info on every stream request and
every 10 seconds, and closes the SSE streams positioned on the old stream
(#133):

- **Recreated**: the stream's creation time changed. It was deleted and
  created again, or restored with `nats stream restore` on nats-server 2.15,
  which gives a restored stream a new creation time. The SSE streams close
  with `disconnect_reason=stream_recreated`. Their last frame is a `reset`
  event that sets the client's last event ID to `0`, so EventSource
  reconnects and replays the stream from its start. Pages that keep the last
  event ID across reloads must take it from `reset` events too.
- **Rewound**: the creation time is the same, but the stream went back to an
  earlier sequence. It was restored with `nats stream restore` on
  nats-server 2.14 or earlier, which keep the creation time, or its store
  directory was restored from a copy. NUTS notices it in either of two ways:
  - The last sequence is below the highest one seen, and a read at least 5
    seconds later still finds it back.
  - The restored stream has already passed the sequences NUTS saw. Every 10
    seconds NUTS checks that the message it last saw at a sequence is still
    the one stored there. Sequences are never reused, so another message
    there means the stream went back.

  The SSE streams close with `disconnect_reason=stream_rewound` and the same
  `reset` event, and clients replay the stream from its start. The restored
  stream gives the sequences after the backup's end to new messages, so no
  cursor from before the restore is safe to resume from (#137).

Each is logged once as a warning: `JetStream stream was recreated`,
`JetStream stream went back to an earlier sequence`, or, for a rewound stream
that already caught up, `… and has since passed it`. Each closed SSE stream
counts in `nuts_consumer_invalidated_total{reason}`. The reconnects are spread
over 2.5 to 7.5 seconds, and each replay is bounded by `replay_max_messages`
and `replay_window`. Operations that keep the stream, such as
`nats stream purge` or a config update, do not reset its sequence numbers,
and NUTS leaves its streams alone.

Limits:

- Until NUTS notices a recreated or rewound stream, its open SSE streams
  receive nothing new from it. That lasts at most about 20 seconds, or 10
  seconds when a request reads the stream first. They receive everything
  once they replay.
- NUTS notices a rewound stream that already caught up only while the stream
  still holds the message it last checked, which is at most 10 seconds old.
  If the stream's limits (`max_msgs`, `max_age`, `max_bytes`) remove messages
  within 10 seconds, such a rewind can go unnoticed.
- A client that was disconnected while the stream was recreated or restored
  reconnects with a cursor from the old stream. While the cursor is ahead of
  the new stream it gets the retained replay; once the new stream has passed
  it, the client resumes after it and misses the new stream's messages before
  it. With `event_id_format sequence_time` NUTS notices such a cursor and
  replays the stream from its start instead (#138): see README
  "`event_id_format`".

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
