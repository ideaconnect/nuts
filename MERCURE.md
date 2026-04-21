# Mercure vs NUTS — Feature Comparison

Feature-by-feature comparison between [Mercure](https://mercure.rocks) and NUTS.
Use this as a development roadmap — features marked **Planned** are candidates for future work.

## Legend

| Symbol | Meaning |
|--------|---------|
| ✅ | Supported |
| ⚡ | Partial / different approach |
| ❌ | Not supported |
| N/A | Not applicable |

## Core Protocol

| Feature | Mercure | NUTS | Notes |
|---------|---------|------|-------|
| SSE subscribe (EventSource) | ✅ | ✅ | Both use standard Server-Sent Events |
| HTTP POST publish | ✅ | ❌ | Mercure publishes via POST to the hub; NUTS is read-only — external producers publish to NATS |
| Multiple topic subscription | ✅ `?topic=a&topic=b` | ✅ `?topic=a&topic=b` | Same query-parameter pattern |
| Path-based topic shorthand | ❌ | ✅ `GET /events/topic` | NUTS extra convenience |
| Topic prefix | ❌ | ✅ `topic_prefix` | Server-side prefix prepended to all subscriptions |
| URI template topic selectors | ✅ `{id}`, `{+path}` | ❌ | Mercure supports RFC 6570 templates for wildcard matching |
| Protocol specification (IETF draft) | ✅ | ❌ | Mercure is a published Internet-Draft |

## Authentication & Authorization

| Feature | Mercure | NUTS | Notes |
|---------|---------|------|-------|
| Subscriber JWT auth | ✅ `subscriber_jwt` | ❌ | Mercure validates subscriber JWTs with per-topic claims |
| Publisher JWT auth | ✅ `publisher_jwt` | N/A | NUTS has no publish endpoint |
| JWKS URL (identity provider) | ✅ `subscriber_jwks_url` | ❌ | Mercure integrates with Keycloak, AWS Cognito, etc. |
| Anonymous subscribers | ✅ `anonymous` directive | ✅ (default) | NUTS allows all subscribers; no JWT gating |
| Cookie-based auth | ✅ `cookie_name` | ❌ | Mercure supports cookie authorization for browser apps |
| Private topics | ✅ JWT `subscribe` claim | ❌ | Mercure restricts topic access via JWT claims |
| NATS credential file auth | ❌ | ✅ `nats_credentials` | NUTS authenticates to the NATS server, not to HTTP clients |
| NATS token auth | ❌ | ✅ `nats_token` | Backend auth to NATS |
| NATS user/password auth | ❌ | ✅ `nats_user` / `nats_password` | Backend auth to NATS |

## Message Delivery & Replay

| Feature | Mercure | NUTS | Notes |
|---------|---------|------|-------|
| Last-Event-ID reconnect | ✅ | ✅ | Both support the standard header |
| Query-param replay (`last-id`) | ❌ | ✅ | NUTS also accepts `?last-id=N` |
| Event store (history) | ✅ Bolt DB / transports | ✅ JetStream | Mercure uses built-in Bolt; NUTS delegates to NATS JetStream |
| Purged-sequence fallback | ❌ | ✅ `DeliverAll()` | NUTS falls back to all retained messages when the requested sequence is gone |
| Event ID format | ✅ UUID / opaque | ✅ JetStream sequence (uint64) | Different ID schemes |
| Heartbeat / keep-alive | ✅ default 40s | ✅ default 30s | Both send periodic SSE comments |
| Dispatch timeout | ✅ `dispatch_timeout` 5s | ❌ | Mercure caps per-update delivery time |
| Write timeout | ✅ `write_timeout` 600s | ❌ | Mercure closes idle connections after a deadline |
| Max event size guard | ❌ | ✅ `max_event_size` 1 MB | NUTS drops oversized events |

## Slow Client & Back-Pressure

| Feature | Mercure | NUTS | Notes |
|---------|---------|------|-------|
| Slow client handling | ⚡ dispatch timeout | ✅ proactive disconnect | NUTS detects full buffer (64 msgs) and disconnects before dropping |
| Per-connection buffer | ❌ (not documented) | ✅ 64-slot channel | Bounded per-client queue |

## CORS

| Feature | Mercure | NUTS | Notes |
|---------|---------|------|-------|
| Configurable allowed origins | ✅ `cors_origins` | ✅ `allowed_origins` | Same concept |
| Origin echo for credentials | ❌ (not documented) | ✅ | NUTS echoes the request origin for credential-aware CORS |
| Preflight OPTIONS handling | ✅ | ✅ | Both handle OPTIONS with appropriate headers |
| Publish origins whitelist | ✅ `publish_origins` | N/A | NUTS has no publish endpoint |

## Discovery & Observability

| Feature | Mercure | NUTS | Notes |
|---------|---------|------|-------|
| Hub auto-discovery (`Link` header) | ✅ `rel="mercure"` | ✅ `hub_url` | NUTS injects a `Link: <url>; rel="nuts"` header when `hub_url` is configured |
| Active subscriptions API | ✅ `subscriptions` directive | ❌ | Mercure exposes subscription lifecycle events and a REST API |
| Subscription events | ✅ | ❌ | Mercure publishes events when subscriptions are created/closed |
| Prometheus metrics | ✅ `/metrics` | ✅ `nuts_*` | NUTS registers `nuts_*` counters and gauges via promauto; visible on Caddy's metrics endpoint |
| Health check endpoint | ✅ `/healthz` | ✅ `*/healthz` | NUTS responds on any path ending in `/healthz` with NATS + stream status |
| Web UI / demo mode | ✅ `demo` / `ui` | ❌ | Mercure includes a built-in testing UI |

## Encryption & Security

| Feature | Mercure | NUTS | Notes |
|---------|---------|------|-------|
| End-to-end encryption | ✅ | ❌ | Mercure supports E2E payload encryption |
| TLS / HTTPS | ✅ auto Let's Encrypt | ✅ via Caddy | Both leverage Caddy for automatic HTTPS |
| URL credential redaction in logs | ❌ (not documented) | ✅ `redactURL()` | NUTS strips embedded creds before logging |
| Topic name validation | ❌ (not documented) | ✅ `isValidTopic()` | NUTS rejects wildcards, system subjects, control chars |

## Infrastructure & Deployment

| Feature | Mercure | NUTS | Notes |
|---------|---------|------|-------|
| Caddy module | ✅ | ✅ | Both are Caddy middleware modules |
| Caddyfile configuration | ✅ | ✅ | Both support Caddyfile syntax |
| JSON configuration | ✅ | ✅ | Both support Caddy's JSON config |
| Cluster / HA mode | ✅ (commercial) | ⚡ via NATS cluster | Mercure offers a licensed HA version; NUTS inherits HA from NATS clustering |
| Multiple transport backends | ✅ Bolt, local, etc. | ❌ JetStream only | Mercure supports pluggable transports |
| Docker image | ✅ official | ✅ `idcttech/nuts` | Multi-arch (amd64, arm64) image published to Docker Hub |
| Protocol version compat | ✅ `protocol_version_compatibility` | N/A | Mercure maintains backward compatibility with older protocol versions |

## Development Roadmap

Features we should consider adding, ordered roughly by impact:

| Priority | Feature | Rationale |
|----------|---------|-----------|
| High | **Subscriber JWT auth + private topics** | Most requested gap — needed to secure per-topic access without relying solely on NATS ACLs |
| High | **HTTP POST publish endpoint** | Lets any HTTP client publish without a NATS client library; matches Mercure's simplicity |
| Medium | **Active subscriptions API** | REST endpoint listing current subscribers and their topics |
| Medium | **Subscription lifecycle events** | Publish internal NATS events when clients subscribe/unsubscribe |
| Medium | **Cookie-based auth** | Browser EventSource can't set custom headers; cookie auth is the workaround |
| Low | **End-to-end encryption** | Encrypt payloads so the hub cannot read them; useful for sensitive data |
| Low | **Web UI / demo mode** | Built-in testing page for development convenience |
| Low | **Write / dispatch timeout** | Cap maximum connection and per-message delivery durations |
| Low | **URI template topic selectors** | Allow `{id}`-style wildcards in subscription topics |
