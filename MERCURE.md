# Inspired by Mercure

NUTS was inspired by [Mercure.rocks](https://mercure.rocks), which pioneered
the idea of a Caddy-native SSE hub with a clean reconnect/replay story and a
standards-friendly discovery model. A lot of the DX patterns you'll recognise
in NUTS — a `nuts { ... }` Caddyfile block, `EventSource`-based subscriptions,
`Last-Event-ID` replay, a `Link` header for hub discovery — were shaped by
Mercure's prior art, and we're grateful for it.

NUTS is a separate project with a different scope. Rather than implement its
own event transport, authentication model, and clustering, NUTS is a read-only
bridge that delegates those concerns to NATS JetStream: persistence lives in
JetStream, clustering is NATS clustering, and authentication to the NATS
server uses NATS's own mechanisms (credentials file, token, user/password,
mTLS). External producers publish to NATS directly; NUTS only reads.

That split leads to a deliberately different surface area:

- NUTS has no publish endpoint — any NATS client (or anything that can talk
  NATS over its wire protocol) acts as the producer.
- Subscriber authentication and per-topic authorisation are on the roadmap
  (see [ROADMAP.md](ROADMAP.md)) but not implemented today; anonymous
  subscribers are the default.
- NUTS is not aiming for protocol-level interoperability with Mercure clients.
  Clients are expected to speak plain SSE with `Last-Event-ID` (or the
  `?last-id=` query param).

If you're already running NATS JetStream and want an SSE bridge on top of it,
NUTS is designed for that case. If you're not, Mercure is a mature,
well-documented option worth evaluating on its own terms — we're not trying to
position NUTS as a replacement.
