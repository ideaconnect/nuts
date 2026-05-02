# Deployment Examples

These examples are intended as copy-paste starting points. Replace image tags,
domains, secrets, storage classes, and NATS addresses before using them in a
real environment.

## Production Compose

`compose.yaml`:

```yaml
services:
  nats:
    image: nats:2.12-alpine
    command: ["--jetstream", "--store_dir=/data", "-m", "8222"]
    ports:
      - "4222:4222"
      - "8222:8222"
    volumes:
      - nats-data:/data
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:8222/healthz"]
      interval: 5s
      timeout: 3s
      retries: 12

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
          --max-msgs 100000 \
          --max-age 24h \
          --discard old \
          --defaults
    restart: "no"

  nuts:
    image: idcttech/nuts:v0.0.0 # replace with a real release tag
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
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:8080/events/readyz"]
      interval: 5s
      timeout: 3s
      retries: 12

volumes:
  nats-data:
```

`Caddyfile`:

```caddyfile
{
    admin off
    order nuts before respond
}

:8080 {
    route /metrics {
        metrics
    }

    route /events* {
        uri strip_prefix /events
        nuts {
            nats_url {$NATS_URL:nats://nats:4222}
            stream_name {$STREAM_NAME:EVENTS}
            topic_prefix {$TOPIC_PREFIX:events.}
            allowed_origins https://app.example.com
            max_connections 1000
            max_event_size 65536
            client_buffer_size 8
            replay_max_messages 1000
            replay_window 300
        }
    }
}
```

## Kubernetes With External NATS

This example assumes a NATS cluster already exists and that the `EVENTS` stream
has been created with subjects matching `events.>`.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: nuts-caddyfile
data:
  Caddyfile: |
    {
        admin off
        order nuts before respond
    }

    :8080 {
        route /metrics {
            metrics
        }

        route /events* {
            uri strip_prefix /events
            nuts {
                nats_url {$NATS_URL}
                stream_name EVENTS
                topic_prefix events.
                allowed_origins https://app.example.com
                max_connections 1000
                max_event_size 65536
                client_buffer_size 8
                replay_max_messages 1000
                replay_window 300
                live_path /livez
                ready_path /readyz
            }
        }
    }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nuts
spec:
  replicas: 2
  selector:
    matchLabels:
      app: nuts
  template:
    metadata:
      labels:
        app: nuts
    spec:
      containers:
        - name: nuts
          image: idcttech/nuts:v0.0.0 # replace with a real release tag
          imagePullPolicy: IfNotPresent
          ports:
            - name: http
              containerPort: 8080
          env:
            - name: NATS_URL
              value: nats://nats.default.svc.cluster.local:4222
          volumeMounts:
            - name: caddyfile
              mountPath: /app/Caddyfile
              subPath: Caddyfile
              readOnly: true
          livenessProbe:
            httpGet:
              path: /events/livez
              port: http
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /events/readyz
              port: http
            periodSeconds: 5
            failureThreshold: 3
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              memory: 512Mi
      volumes:
        - name: caddyfile
          configMap:
            name: nuts-caddyfile
---
apiVersion: v1
kind: Service
metadata:
  name: nuts
spec:
  selector:
    app: nuts
  ports:
    - name: http
      port: 80
      targetPort: http
```

## Reverse-Proxy-Protected Route

Put subscriber authentication in front of NUTS. This example uses a public edge
Caddy instance that verifies the request with an auth service before proxying to
an internal NUTS instance.

Public edge Caddyfile:

```caddyfile
events.example.com {
    route /events* {
        forward_auth https://auth.example.com {
            uri /verify
            copy_headers X-User X-Tenant
        }
        reverse_proxy http://nuts-internal:8080
    }
}
```

Internal NUTS Caddyfile:

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
            topic_prefix tenants.a.
            allowed_origins https://app.example.com
            max_connections 500
            replay_max_messages 1000
            replay_window 300
        }
    }
}
```

For simple operator-controlled access, `basic_auth` can live in the same Caddy
route before `nuts`. For tenant isolation, prefer separate route blocks with
separate streams or prefixes rather than one broad public prefix.
