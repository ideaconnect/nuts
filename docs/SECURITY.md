# Security Policy

## Supported versions

| Version  | Supported          |
| -------- | ------------------ |
| `main`   | :white_check_mark: |
| Latest tagged release | :white_check_mark: |
| Older tags | :x:              |

## Reporting a vulnerability

Please report suspected vulnerabilities privately by emailing
**security@idct.tech** with:

- A description of the issue and its impact.
- Steps to reproduce (PoC where possible).
- Affected versions / commit SHAs.
- Any known mitigations.

We will acknowledge your report within 72 hours and aim to release a fix
within 30 days for high-severity issues. Once a fix is available we will
coordinate public disclosure and credit the reporter (unless anonymity is
requested).

## Scope

In scope:

- The `nuts` Caddy HTTP handler module in this repository.
- The published `idcttech/nuts` Docker image.
- Build tooling and GitHub Actions workflows in this repository.

Out of scope:

- Third-party dependencies (please report upstream instead; notify us if the
  issue manifests specifically through NUTS).
- Denial of service via resource exhaustion that requires administrative
  misconfiguration (e.g. `max_connections` unset in a public deployment).

## Hardening guidance

Production deployments should:

- Enable NATS TLS via `nats_tls_ca` / `nats_tls_cert` / `nats_tls_key`.
- Protect the NUTS HTTP route with Caddy `basic_auth`, `forward_auth`, a
  reverse proxy, application policy, or `subscriber_jwt_key` when subscriber
  authentication or per-topic authorization is needed. NATS auth directives
  only authenticate NUTS to NATS; they do not authenticate browser subscribers.
- Set `max_connections` to a value appropriate for the host.
- Set `max_event_size` to bound memory per event.
- Set `write_timeout` and `dispatch_timeout` where blocked downstream writes
  or saturated slow-client signals should be bounded by the handler itself.
- Set `replay_max_messages` or `replay_window` when a large retained stream
  could make replay too expensive.
- Restrict `allowed_origins` to your trusted front-ends.
- Run the container as a non-root user (already the default).

### Subscriber access boundary

Without `subscriber_jwt_key` or an upstream auth layer, any client that can
reach a NUTS route can request any valid topic under that handler's
`topic_prefix`. CORS is not authorization; it only controls browser read access
by origin.

Use one or more of these controls when tenant or private-topic isolation
matters:

- `subscriber_jwt_key` with a JWT `subscribe` claim for first-party topic
  authorization inside NUTS.
- Caddy route policy, `forward_auth`, a reverse proxy, or application cookies.
- Separate NUTS route blocks with separate streams/prefixes per tenant.

NUTS subscriber JWTs use HMAC signatures (`HS256`, `HS384`, or `HS512`) and
must include a `subscribe` claim. Supported filters are exact topic names,
single-token wildcards such as `orders.*`, and tail wildcards such as
`tenant-a.>`. A bare `>` matches every topic on the route per standard NATS
semantics; a bare `*` is accepted as a NUTS-only alias with the same "every
topic" meaning, which is more permissive than NATS itself (where bare `*`
only matches single-token subjects). Missing, expired, badly signed, or
unauthorized tokens are rejected before an ephemeral JetStream consumer is
created. The `exp` and `nbf` time claims are optional; when present they are
enforced. For public or browser-facing routes, issue short-lived tokens with
`exp`. NUTS rejects compact JWTs over 8 KiB, decoded JWT segments over 6 KiB,
and `subscribe` claims with more than 128 filters.

### Rate limits and replay bounds

Use an edge proxy, CDN, WAF, Caddy rate-limit plugin, or upstream gateway to
limit request churn by IP, user, or tenant. Recommended buckets are:

- SSE connection attempts to the NUTS route.
- Replay-heavy requests with stale `last-id` or `Last-Event-ID` values.
- Repeated invalid-topic requests returning `400`.
- Repeated subscription failures or connection-cap rejections returning `503`.

`max_connections` caps concurrent streams, but it is not a request-rate limit.
On retained streams with a large backlog, set `replay_window` and/or
`replay_max_messages` so one reconnect cannot force an unbounded
replay. The default `0` values preserve compatibility by allowing all retained
replay; production public routes should choose explicit bounds that match their
retention and client recovery budget.
