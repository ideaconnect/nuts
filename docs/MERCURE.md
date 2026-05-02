# Inspired by Mercure

NUTS was inspired by [Mercure.rocks](https://mercure.rocks). The familiar
patterns — a Caddy module configured via a `nuts { ... }` block, browser
`EventSource` subscriptions, `Last-Event-ID` replay, and a `Link` header for
hub discovery — were shaped by Mercure's prior art, and we're grateful for the
groundwork they laid.

NUTS is a separate project with a different scope: it's a read-only bridge that
delegates persistence, clustering, and authentication to NATS JetStream rather
than implementing its own transport. We respect Mercure's work and recommend it
on its own terms — NUTS is not aiming to replace it.
