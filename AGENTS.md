# AGENTS.md

Instructions for AI coding agents (Claude Code, GitHub Copilot, Codex, Cursor,
Aider, etc.) working in this repository. Human contributors should start with
[README.md](README.md) and [CONTRIBUTING.md](CONTRIBUTING.md); this file is the
single source of truth for agent behaviour.

## What NUTS is

NUTS is a [Caddy](https://caddyserver.com) HTTP handler module written in Go
that bridges [NATS JetStream](https://docs.nats.io/nats-concepts/jetstream)
messages into browser-friendly Server-Sent Events. The handler ID is
`http.handlers.nuts` and the Caddyfile directive is `nuts { ... }`.

The design is deliberately small: Caddy owns HTTP routing and edge policy,
NATS owns persistence and fan-out, NUTS bridges the two with one long-lived
NATS connection per handler instance. NUTS is **read-only** — producers
publish directly to NATS; NUTS never publishes. It was inspired by
[Mercure.rocks](https://mercure.rocks); we respect their work and do not aim
to replace it.

### Capabilities the codebase actually exposes

- SSE streaming from JetStream-retained subjects to `EventSource` clients.
- Multi-topic subscriptions via repeated `?topic=` query parameters or path
  shorthand (`/orders/new` → subject `orders.new`, with optional
  `topic_prefix`).
- Replay via `?last-id=` query or browser-managed `Last-Event-ID` header,
  bounded by `replay_max_messages` and/or `replay_window`.
- Slow-client backpressure: when a per-connection queue fills, NUTS
  disconnects the SSE stream rather than dropping queued events; the client
  reconnects and resumes via `Last-Event-ID`.
- NATS authentication (credentials file, token, user/password) and TLS / mTLS
  to the NATS server.
- Optional first-party subscriber JWT auth (`subscriber_jwt_key`,
  `subscriber_jwt_cookie`) with HMAC-signed tokens and a `subscribe` claim
  for per-topic authorisation.
- Configurable CORS (`allowed_origins`, `allowed_headers`, `allowed_methods`).
- Connection cap (`max_connections`) with `429` + `Retry-After` rejection.
- Per-frame write deadlines (`write_timeout`) and slow-client signal bounds
  (`dispatch_timeout`).
- Probes: `live_path` (`/livez`), `ready_path` (`/readyz`), and the legacy
  `health_path` (`/healthz`).
- Hub discovery via `Link: <url>; rel="nuts"` when `hub_url` is set.
- Prometheus metrics (`nuts_*`) registered via `promauto`, surfaced through
  Caddy's `/metrics` handler.

A current and complete directive list with defaults, JSON field names,
validation rules, and operational notes is in
[docs/CONFIGURATION.md](docs/CONFIGURATION.md). Treat that file as
authoritative — do not invent directives.

## Repository structure

Top-level Go source is the Caddy module package itself (`package nuts`):

| File | Responsibility |
| --- | --- |
| [handler.go](handler.go) | Module registration, `Handler` struct, interface guards. |
| [provision.go](provision.go) | `Provision`, `Validate`, `Cleanup`; NATS dial and TLS config. |
| [auth.go](auth.go) | Subscriber JWT verification and `subscribe`-claim parsing. |
| [serve.go](serve.go) | `ServeHTTP`, the SSE streaming loop, replay planning, probes, CORS. |
| [caddyfile.go](caddyfile.go) | Caddyfile parsing (`UnmarshalCaddyfile`, `parseCaddyfile`). |
| [helpers.go](helpers.go) | Pure helpers: SSE writers, JSON helpers, topic/cookie validation, URL redaction. |
| [metrics.go](metrics.go) | Prometheus counters and gauges. |
| [cmd/caddy/main.go](cmd/caddy/main.go) | `go build` entry point that links a Caddy binary with this module. |

Tests live alongside the source:

| File | What it covers |
| --- | --- |
| [nats_test.go](nats_test.go) | Core unit and integration tests against an embedded NATS server. |
| [hardening_test.go](hardening_test.go) | Security hardening tests (CORS, JWT, oversized payloads, max connections, timeouts, TLS, replay caps). |
| [handler_integration_test.go](handler_integration_test.go) | Runtime integration tests for metrics, TLS config, heartbeat, topic prefixing, reconnect, persistence, and multi-topic subscription paths. |
| [auth_test.go](auth_test.go) | Subscriber JWT parsing, verification, token extraction, and topic-filter validation tests. |
| [helpers_test.go](helpers_test.go) | Helper edge-case tests for cookie names and SSE write behavior. |
| [performance_test.go](performance_test.go) | `TestPerformance_*` confidence tests and benchmarks. |
| [serve_test.go](serve_test.go) | Request parsing, replay planning, formatting, and stream helper unit tests without live NATS. |
| [functional_test/](functional_test/) | [Godog](https://github.com/cucumber/godog) BDD tests against a real Docker Compose stack. |
| [features/](features/) | Gherkin `.feature` files driving the Godog suite. |

Operational and deployment assets:

| Path | Purpose |
| --- | --- |
| [Caddyfile](Caddyfile), [Caddyfile.test](Caddyfile.test) | Reference configurations; `Caddyfile.test` powers the functional stack. |
| [Dockerfile](Dockerfile), [Dockerfile.test](Dockerfile.test) | Production and test images (non-root, uid 10001). |
| [docker-compose.yml](docker-compose.yml) | Functional-test stack (NATS + NUTS built from source). |
| [example/](example/), [example_docker/](example_docker/) | Interactive demos (source build vs. published image). |
| [ops/prometheus-alerts.yml](ops/prometheus-alerts.yml), [ops/grafana-dashboard.json](ops/grafana-dashboard.json) | Reference alerts and dashboard. |
| [scripts/](scripts/) | Helper scripts (e.g. `setup-dev.sh`). |
| [docs/](docs/) | All in-depth documentation; see the index in [README.md](README.md#further-documentation). |
| [website/](website/) | Marketing/docs site (Jekyll + Tailwind) published to `https://idct.tech/nuts`. Has its own guidelines in [website/AGENTS.md](website/AGENTS.md). |

## How to run things

Always prefer the `Makefile` targets — they match CI and are documented in
[CONTRIBUTING.md](CONTRIBUTING.md).

### Build

```bash
make build                 # ./caddy with the nuts module linked in
go build ./cmd/caddy       # equivalent
```

### Tests

| Target | What it does |
| --- | --- |
| `make test-unit` | Unit/integration tests with embedded NATS (no Docker). |
| `make test-performance` | `TestPerformance_*` plus the named hot-path benchmarks. |
| `make test-functional` | Godog BDD scenarios against the Docker Compose stack. |
| `make test-functional-stress FUNCTIONAL_TEST_STRESS_COUNT=N` | Repeats the functional suite N times to catch flakes. |
| `make test-functional-matrix` | Runs functional tests against `nats:2.9-alpine` (pre-multi-filter), `nats:2.12-alpine`, and `nats:2.14-alpine` (matches the embedded `nats-server/v2` major.minor pinned in `go.mod`). |
| `make test` | `test-unit` + `test-functional`. |
| `make lint` | golangci-lint via the pinned container image. |
| `make release-check` | GoReleaser config validation in a container. |

Race-test policy is intentionally focused — see the `Run focused race tests`
step in [.github/workflows/ci.yml](.github/workflows/ci.yml). Adding broad
`-race` runs without justification will not be accepted.

### Local single-test loop

```bash
go test -v -run TestHandler_ServeHTTP_Integration -timeout 120s .
```

### Common pre-commit checks

```bash
gofmt -s -w .
go vet ./...
go mod tidy && git diff --exit-code go.mod go.sum
make lint
```

The full PR checklist is [CONTRIBUTING.md § Contribution checklist](CONTRIBUTING.md#contribution-checklist).

### Website

The site under [website/](website/) is a Jekyll + Tailwind project served at
`https://idct.tech/nuts` (the org's `idct.tech` Pages custom domain makes this
project repo resolve at the `/nuts` subpath, so `baseurl` is `/nuts`). Build it
through the containerised toolchain — Docker is the only prerequisite:

```bash
make website-build     # production build → website/_site (mirrors CI)
make website-serve     # livereload dev server at http://localhost:4000/nuts/
make website-clean     # remove generated output
```

Deployment is automatic: [.github/workflows/website.yml](.github/workflows/website.yml)
builds and deploys to GitHub Pages on every `v*` tag. Internal links and assets
must go through `relative_url` (or `{{ site.baseurl }}`) so the `/nuts` prefix is
applied — never hard-code root-absolute paths. Deeper conventions are in
[website/AGENTS.md](website/AGENTS.md).

## Coding conventions

- **Go version:** match `go.mod` (Go 1.26.4 at time of writing). Don't bump
  the toolchain unless asked.
- **Style:** `gofmt -s -w .`; `make lint` must pass; no new `//nolint`
  directives without a one-line justification on the same line.
- **Comments:** keep them focused on non-obvious behaviour and invariants.
  Don't narrate straightforward code; don't restate identifier names.
- **No new dependencies casually.** This module is intended to be small and
  auditable. New imports require justification in the PR description.
- **Caddyfile directives:** every supported directive must be parsed in
  [caddyfile.go](caddyfile.go), validated in [provision.go](provision.go),
  documented in [README.md](README.md) and
  [docs/CONFIGURATION.md](docs/CONFIGURATION.md), and added to
  [CHANGELOG.md](CHANGELOG.md) under `[Unreleased]`.
- **Sentinel semantics:** preserve the documented zero-value meanings —
  notably `max_event_size 0` → 1 MiB default, `max_event_size < 0` →
  unlimited; `client_buffer_size 0` → default 64; `max_reconnects` is
  `*int` so an explicit `0` survives JSON round-trips.
- **Topic validation:** `isValidTopic` accepts only `[A-Za-z0-9._-]` with the
  documented rejections (empty, overlength, wildcards `*`/`>`, `$`-system,
  control chars, leading/trailing/consecutive dots). Don't loosen it.
- **CORS:** echo the request `Origin` only when allow-listed; emit `Vary:
  Origin`; advertise `Access-Control-Allow-Credentials: true` only for
  explicit (non-wildcard) origins.
- **Tests for new behaviour:** unit test under the embedded NATS server when
  possible. Add a Godog scenario under [features/](features/) for changes that
  are observable over HTTP. See [Mutation testing](#mutation-testing) for the
  additional bar on test *strength* (not just presence).
- **Documentation:** when behaviour changes, update the relevant file under
  [docs/](docs/) **and** the matching section of [README.md](README.md).
  Operator-facing impact also belongs in [CHANGELOG.md](CHANGELOG.md).

## Mutation testing

This repository uses [gremlins](https://github.com/go-gremlins/gremlins) to
measure test *strength* — not just whether tests exist, but whether they
would catch a regression. Coverage answers "did the test touch this line?";
mutation testing answers "would the test fail if this line were wrong?"

Per-file MSI (Mutation Score Indicator) targets and the policy on
surviving mutants are in
[docs/mutation/targets.md](docs/mutation/targets.md). The current baseline
is in [docs/mutation/baseline.md](docs/mutation/baseline.md).

### Requirement for agents

Any change that adds or modifies code in **`auth.go`, `helpers.go`,
`handler.go`, `serve.go`, `caddyfile.go`, or `provision.go`** must:

1. Run `make mutate-pkg PKG=<changed-file>` locally before declaring the
   task complete.
2. Report the resulting MSI in the PR description.
3. For every new survivor introduced by the change, take one of three
   actions (in order of preference):
   - **Kill it** — add a test that exercises the affected behaviour with
     the right boundary or branch. This is the default path.
   - **Document as equivalent** — record the survivor in
     [docs/mutation/equivalents.md](docs/mutation/equivalents.md) with
     `file:line`, mutator, original→mutated diff, and a one-paragraph
     justification.
   - **Flag for human review** — call it out explicitly in the PR
     description. Never silently ignore.
4. Adding a new exported function without a test that kills at least the
   boundary and return-value mutants on it is a blocker.

The full module run (`make mutate`) is reserved for the weekly scheduled
GitHub Action, not per-PR — see
[.github/workflows/mutation.yml](.github/workflows/mutation.yml).

### Why this is enforced on agents specifically

An AI agent can produce code that compiles and passes the existing tests
while leaving newly-introduced branches completely unverified. Coverage
masks this; mutation testing surfaces it. The requirement above closes
that gap before review starts, instead of leaking it into review load.

## CI and release surfaces

CI is GitHub Actions ([.github/workflows/ci.yml](.github/workflows/ci.yml),
[.github/workflows/release.yml](.github/workflows/release.yml),
[.github/workflows/website.yml](.github/workflows/website.yml)).

PRs run, in this order:

1. `gofmt`, `go mod tidy` diff check, golangci-lint, unit tests with coverage,
   focused race tests, `go vet`, `govulncheck`.
2. Functional test matrix (`nats:2.9-alpine`, `nats:2.12-alpine`,
   `nats:2.14-alpine`) and a 3× functional stress pass.
3. Coverage upload to Codecov.
4. Production Docker image build + `caddy adapt` validation + Trivy scan +
   SBOM (SPDX JSON) artifact.
5. GoReleaser config check + snapshot dry run when release-relevant files
   change ([.goreleaser.yml](.goreleaser.yml), workflows, `Makefile`,
   `Dockerfile`).

Pushes to `main`/`master` and `v*` tags additionally:

1. Build and push the multi-arch (`linux/amd64`, `linux/arm64`)
   `idcttech/nuts` image with provenance and SBOM attestations.
2. On tag: scan the pushed image by digest with Trivy and sign all tags
   keyless via Cosign + GitHub OIDC.
3. Update the Docker Hub description from [DOCKERHUB_README.md](DOCKERHUB_README.md).
4. GoReleaser publishes archives and SBOMs for the tag.
5. Build the [website/](website/) Jekyll site and deploy it to GitHub Pages at
   `https://idct.tech/nuts` ([website.yml](.github/workflows/website.yml); also
   runnable on demand via `workflow_dispatch`).

Release surfaces and verification commands are documented in
[docs/RELEASE.md](docs/RELEASE.md). Operator-facing release notes follow the
template in that file.

## Deployment shape

The published artefact set:

- `idcttech/nuts:<version>` Docker image (multi-arch, non-root uid 10001,
  Cosign-signed, SBOM-attested for tag releases). `:latest` is mutable — pin
  to a concrete tag in production.
- GitHub release archives with SHA-256 checksums and SBOMs.
- Source build via `go build ./cmd/caddy` or `xcaddy build --with
  github.com/ideaconnect/nuts`.

Reference deployments: production Compose, Kubernetes, and a
reverse-proxy-protected example are in
[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md). Operations guidance (probes,
incident runbooks, structured log fields) is in
[docs/OPERATIONS.md](docs/OPERATIONS.md). Performance budgets and the formula
for sizing memory per connection are in
[docs/PERFORMANCE.md](docs/PERFORMANCE.md).

## Security boundaries

Read [docs/SECURITY.md](docs/SECURITY.md) before changing anything in `auth.go`,
`provision.go`, or the CORS path of `serve.go`. Key invariants:

- NATS auth directives (`nats_credentials`, `nats_token`,
  `nats_user`/`nats_password`) authenticate **NUTS to NATS**. They are not
  subscriber credentials.
- Subscriber identity is enforced either by Caddy/upstream policy (e.g.
  `forward_auth`, `basic_auth`) or by the optional `subscriber_jwt_key` check.
  CORS is not authorisation.
- JWT verification accepts `HS256`, `HS384`, `HS512`. The `subscribe` claim is
  evaluated **before** any JetStream consumer is created.
- `Validate()` warns on cleartext `nats://` with credentials and on
  `nats_tls_insecure_skip_verify=true`. Don't silence those warnings.
- Oversized payloads are rejected before JSON parsing to avoid unbounded
  allocation by hostile producers.

Do not commit secrets, NATS credentials files, JWTs, or `.env` files.

## What you should *not* do

- Don't add a publish endpoint or any code that publishes to NATS from inside
  the handler. Phase 4 of [docs/ROADMAP.md](docs/ROADMAP.md) is the only
  sanctioned path for that and it is not yet started.
- Don't add Mercure protocol compatibility shims. NUTS speaks plain SSE.
- Don't introduce backwards-compat stubs (re-exports, renamed `_`-prefixed
  vars, "removed" comments) for things you delete — just delete them.
- Don't bypass hooks (`--no-verify`, `--no-gpg-sign`) when committing.
- Don't run destructive git operations (`reset --hard`, force-push, branch
  delete) without explicit user instruction.
- Don't fabricate metrics, directives, log fields, or struct fields. If it's
  not in `caddyfile.go` / `metrics.go` / the `Handler` struct, it doesn't
  exist.
- Don't move or rename files in `docs/` without updating every referrer
  (README, CONTRIBUTING, sibling docs, CI workflows, this file).

## Pointers for specific agents

This file is the canonical instruction set. The following per-tool files are
thin pointers and should not be allowed to drift:

- [CLAUDE.md](CLAUDE.md) — read by Claude Code.
- [.github/copilot-instructions.md](.github/copilot-instructions.md) — read by
  GitHub Copilot in VS Code, JetBrains, and on github.com.

If you maintain a different agent (Cursor, Aider, Codex, Continue, Cline,
etc.), point its rules file at `AGENTS.md` rather than copying its contents.

When updating agent guidance, edit **AGENTS.md**. Only touch the pointer files
when their pointer mechanics change.
