# NUTS Roadmap

This document outlines planned features for NUTS, organized into phases.
Completed items are checked off; the rest are candidates for future work.

See [MERCURE.md](MERCURE.md) for a detailed feature-by-feature comparison with Mercure.

## Phase 1: Observability & Operations ✅

No breaking changes, immediately useful for production deployments.

- [x] **Prometheus metrics** — `nuts_*` counters and gauges registered via `promauto`, visible on Caddy's `/metrics` endpoint. Tracks active connections, messages delivered/dropped, slow-client disconnects, replay requests/fallbacks, and subscription errors.
- [x] **Health check endpoint** — Any path ending in `/healthz` within the configured route returns JSON with NATS connectivity and stream availability status (`200` healthy, `503` degraded).
- [x] **Hub discovery (`Link` header)** — Optional `hub_url` directive injects a `Link: <url>; rel="nuts"` header on SSE responses for automatic hub detection by clients.

## Phase 2: Authorization & Access Control

Core security features. Prerequisite for private channels and subscription lifecycle.

- [ ] **Subscriber JWT auth** — Validate JWT on SSE connect, extract allowed topics from claims. Support `subscriber_jwt_key` / `subscriber_jwks_url` Caddyfile directives. Anonymous mode preserved as default (opt-in auth).
- [ ] **Private topics / per-topic access control** — JWT `subscribe` claim restricts which topics a client can subscribe to. Reject unauthorized subscriptions. Depends on subscriber JWT auth.
- [ ] **Cookie-based auth** — Extract JWT from a configurable cookie name for browser `EventSource` (which cannot set custom headers). `cookie_name` Caddyfile directive. Depends on subscriber JWT auth.

## Phase 3: Subscription Lifecycle

Depends on Phase 2 for authenticated subscriber identity.

- [ ] **Subscription notifications** — Publish internal NATS messages when clients subscribe/unsubscribe (e.g. on subject `nuts.subscriptions.{topic}`). Include subscriber identity from JWT when available.
- [ ] **Active subscriptions REST endpoint** — `GET /subscriptions` listing active SSE connections, their topics, connected-since timestamp, and subscriber identity. Optional topic filter: `GET /subscriptions/{topic}`. In-memory connection registry.

## Phase 4: Publishing & Integration

Broadens NUTS from a read-only bridge to a full event hub.

- [ ] **HTTP POST publish endpoint** — `POST /publish` with topic and data fields (form or JSON). Publisher auth via JWT (separate key from subscriber). Publishes directly to NATS JetStream.

## Phase 5: Advanced Features

Lower priority, can be tackled independently.

- [ ] **Dispatch/write timeouts** — Configurable `dispatch_timeout` and `write_timeout` directives to cap how long NUTS waits on slow operations.
- [ ] **URI template topic selectors** — RFC 6570 `{id}` / `{+path}` wildcard matching for topic subscriptions.

## Out of Scope (for now)

- **End-to-end encryption** — Low demand relative to implementation effort.
- **Web UI / demo mode** — The `example/` and `example_docker/` directories serve this purpose.
- **IETF protocol specification** — NUTS is not aiming for Mercure protocol compatibility.
