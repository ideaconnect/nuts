# NUTS - NATS to SSE for Caddy

NUTS is a Caddy HTTP handler module that streams NATS JetStream messages to
browsers as Server-Sent Events (SSE). Producers publish directly to NATS;
NUTS is a read-only bridge for browser and HTTP clients.

Development, issues, pull requests, full documentation, and source releases
happen on GitHub:

https://github.com/ideaconnect/nuts

## Image

```bash
docker pull idcttech/nuts:latest
```

The image contains Caddy with the `http.handlers.nuts` module linked in. It
runs as the non-root `nuts` user, listens on port `8080`, and starts with:

```bash
/app/caddy run --config /app/Caddyfile
```

For production, pin a versioned tag instead of `latest` when one is available.
The `latest` tag may move on every default-branch push.

## Requirements

NUTS expects an existing NATS JetStream stream. It does not create streams and
does not publish messages.

Example stream setup with the NATS CLI:

```bash
nats stream add EVENTS \
  --subjects "events.>" \
  --storage file \
  --retention limits \
  --max-msgs 10000 \
  --max-age 24h \
  --discard old \
  --defaults
```

## Quick Run

Run NUTS with the shipped Caddyfile and point it at a reachable NATS server:

```bash
docker run -d \
  --name nuts \
  -p 8080:8080 \
  -e NATS_URL=nats://host.docker.internal:4222 \
  --add-host=host.docker.internal:host-gateway \
  idcttech/nuts:latest
```

The default route is `/events`. Subscribe from a browser or curl-compatible SSE
client:

```javascript
const events = new EventSource('/events?topic=my-topic');
events.addEventListener('message', (event) => {
  console.log(event.lastEventId, JSON.parse(event.data));
});
```

Publish to NATS:

```bash
nats pub events.my-topic '{"hello":"world"}'
```

## Environment Variables

The shipped `/app/Caddyfile` reads these Caddy placeholders:

| Variable | Default | Caddyfile directive |
| --- | --- | --- |
| `NATS_URL` | `nats://localhost:4222` | `nats_url` |
| `STREAM_NAME` | `EVENTS` | `stream_name` |
| `TOPIC_PREFIX` | `events.` | `topic_prefix` |

Only these three variables are consumed by the default image configuration.
For `allowed_origins`, subscriber JWT auth, TLS, replay caps, connection caps,
or other NUTS directives, mount your own Caddyfile.

## Volumes

The image expects its active Caddyfile at `/app/Caddyfile`.

Common mounts:

| Host path | Container path | Purpose |
| --- | --- | --- |
| `./Caddyfile` | `/app/Caddyfile:ro` | Replace the default NUTS/Caddy config. |
| `./nats.creds` | `/run/secrets/nats.creds:ro` | NATS credentials file for `nats_credentials`. |
| `./tls/` | `/etc/nuts/tls:ro` | NATS CA/client certs for `nats_tls_*`. |

Example with a custom Caddyfile:

```bash
docker run -d \
  --name nuts \
  -p 8080:8080 \
  -v ./Caddyfile:/app/Caddyfile:ro \
  -v ./nats.creds:/run/secrets/nats.creds:ro \
  idcttech/nuts:latest
```

Minimal custom Caddyfile:

```caddyfile
{
  admin off
  order nuts before respond
}

:8080 {
  route /events* {
    uri strip_prefix /events
    nuts {
      nats_url nats://nats:4222
      stream_name EVENTS
      topic_prefix events.
      allowed_origins https://app.example.com
      max_connections 1000
      replay_max_messages 1000
      replay_window 300
    }
  }

  route /metrics {
    metrics
  }
}
```

## Docker Compose

This example starts NATS, creates the `EVENTS` stream, then starts NUTS.

```yaml
services:
  nats:
    image: nats:2.12-alpine
    command: ["--jetstream", "--store_dir=/data"]
    volumes:
      - nats-data:/data
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:8222/healthz"]
      interval: 2s
      timeout: 3s
      retries: 10

  nats-init:
    image: natsio/nats-box:0.19.0
    depends_on:
      nats:
        condition: service_healthy
    entrypoint: ["/bin/sh", "-c"]
    command:
      - |
        nats -s nats://nats:4222 stream add EVENTS \
          --subjects "events.>" \
          --storage file \
          --retention limits \
          --max-msgs 10000 \
          --max-age 24h \
          --discard old \
          --defaults
    restart: "no"

  nuts:
    image: idcttech/nuts:latest
    ports:
      - "8080:8080"
    environment:
      NATS_URL: nats://nats:4222
      STREAM_NAME: EVENTS
      TOPIC_PREFIX: events.
    volumes:
      - ./Caddyfile:/app/Caddyfile:ro
    depends_on:
      nats-init:
        condition: service_completed_successfully

volumes:
  nats-data:
```

## Security Notes

- NATS auth settings authenticate NUTS to NATS. They do not authenticate
  browser subscribers.
- Protect public routes with Caddy, a reverse proxy, or NUTS subscriber JWT
  auth using `subscriber_jwt_key` and optional `subscriber_jwt_cookie`.
- Use explicit `allowed_origins` for credentialed browser access.
- Set `max_connections`, `max_event_size`, `replay_max_messages`, and
  `replay_window` for public or multi-tenant deployments.
- Use NATS TLS/mTLS (`nats_tls_ca`, `nats_tls_cert`, `nats_tls_key`) when NATS
  is not on a trusted private network.

Full configuration, deployment examples, operations guidance, and security
details are maintained on GitHub:

https://github.com/ideaconnect/nuts