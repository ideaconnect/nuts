---
layout: docs
title: Documentation
description: "Get started with NUTS — installation, configuration, and usage guide"
permalink: /docs/
---

## Documentation

Welcome to the NUTS documentation. Choose a topic to get started:

- **[Building & Running]({{ '/docs/building/' | relative_url }})** — prerequisites, `xcaddy`, building from source, the prebuilt Docker image, Docker Compose, and Make targets
- **[Usage]({{ '/docs/usage/' | relative_url }})** — full Caddyfile / JSON configuration reference, JetStream setup, EventSource client code, replay, auth, metrics, and example scenarios
- **[Contributing]({{ '/docs/contributing/' | relative_url }})** — development setup, running tests, and submitting pull requests

## Quick Start (Without Docker)

1. **Build Caddy with NUTS:**

   ```bash
   xcaddy build --with github.com/ideaconnect/nuts
   ```

2. **Start NATS** with JetStream enabled:

   ```bash
   nats-server -js -p 4222
   ```

3. **Create a JetStream stream:**

   ```bash
   nats stream add EVENTS \
     --subjects "events.>" \
     --storage file \
     --retention limits \
     --max-msgs 10000 \
     --max-age 24h \
     --discard old
   ```

4. **Create a Caddyfile:**

   ```
   :8080 {
       route /events* {
           uri strip_prefix /events
           nuts {
               nats_url nats://localhost:4222
               stream_name EVENTS
               topic_prefix events.
           }
       }
   }
   ```

5. **Run Caddy:**

   ```bash
   ./caddy run --config Caddyfile
   ```

6. **Connect from JavaScript:**

   ```javascript
   const events = new EventSource('/events?topic=my-topic');

   events.addEventListener('message', (e) => {
       const data = JSON.parse(e.data);
       console.log('Received:', data, 'ID:', e.lastEventId);
   });
   ```

7. **Publish a message** (using the NATS CLI):

   ```bash
   nats pub events.my-topic '{"hello": "world"}'
   ```

## Quick Start (With Docker)

Pick whichever fits your workflow:

- **Just NATS in Docker, Caddy local:**

  ```bash
  docker run --rm -p 4222:4222 nats:2.12-alpine -js
  ```

- **Prebuilt NUTS Docker image** (`idcttech/nuts`):

  ```bash
  docker run -d \
    -p 8080:8080 \
    -e NATS_URL=nats://host.docker.internal:4222 \
    --add-host=host.docker.internal:host-gateway \
    -v ./Caddyfile:/app/Caddyfile:ro \
    idcttech/nuts:latest
  ```

- **Full stack with Docker Compose** (from the NUTS repo):

  ```bash
  docker compose up -d --build
  ```

  This starts NATS (with JetStream) and Caddy with NUTS together.

For detailed configuration, client usage, and example scenarios, see the [Usage]({{ '/docs/usage/' | relative_url }}) page.

For full build and run instructions, see [Building & Running]({{ '/docs/building/' | relative_url }}).
