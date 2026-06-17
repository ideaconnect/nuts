# Contributing to NUTS

Thanks for your interest in improving NUTS! This document summarizes how to
get a development environment running and the expectations for contributions.

## Development environment

Requirements:

- Go 1.26.2+
- Docker and Docker Compose (for the functional/BDD suite)
- `xcaddy` if you want to build a custom Caddy binary with additional modules

Clone and build:

```bash
git clone https://github.com/ideaconnect/nuts.git
cd nuts
go build ./...
```

Run the unit tests:

```bash
make test-unit
# or
go test -timeout 120s .
```

Run the functional (Godog) suite:

```bash
make test-functional
```

## Coding guidelines

- Run `gofmt -s -w .` and `go vet ./...` before committing.
- Run `make lint` locally; it uses the pinned golangci-lint container that
  matches CI (see `.golangci.yml`).
- Keep exported symbols documented. New Caddyfile directives must be documented
  in [README.md](README.md) and the handler struct fields in
  [handler.go](handler.go).
- Prefer small, focused PRs. One logical change per PR.
- Include unit tests for new behavior. If the change affects the HTTP surface,
  also add a Godog scenario under [features/](features/).

### Test response recorders

Pick the right recorder for the test pattern, otherwise `-race` (run on
the full unit suite in CI) will flag a data race:

- **`safeFlushRecorder` / `newSafeRecorder()`** (in
  [handler_integration_test.go](handler_integration_test.go)) — use when
  the test reads `rr.Body()` **while** the handler goroutine is still
  writing. Internally serialises Write/Body via a mutex. This is the
  right choice for polling-style tests that wait for SSE output to
  appear before cancelling.
- **`flushRecorder`** (a thin `*httptest.ResponseRecorder` wrapper, in
  [nats_test.go](nats_test.go)) — use when the test waits for the
  handler goroutine to finish (`<-done`) **before** reading
  `rr.Body.String()`. The wait happens-before any read, so there's no
  race even though `httptest.ResponseRecorder` is not internally
  synchronised.
- **`failingFlushRecorder`** / **`newFailingFlushRecorder(allowedWrites
  int)`** — use to exercise write-error disconnect paths (allow N
  writes, then return an error on every subsequent write). The
  underlying state is mutex-protected so concurrent reads of `Body()`
  during a goroutine write are safe.

If you change the synchronisation contract of a streaming test (e.g.
remove a `<-done` wait, switch to polling), upgrade the recorder
accordingly. CI runs `go test -race -timeout 240s .` on every PR and
will catch a regression here.

### Synchronisation primitives over `time.Sleep`

Prefer channels, `Eventually`-style polling helpers (e.g.
`waitForSSEBody`, `waitForConsumerCount`), or the existing test helpers
over bare `time.Sleep(...)` for synchronisation. Sleeps make the suite
slower at best and flaky under CI load at worst. Sleeps as
*assertions* (e.g. asserting a heartbeat fires after exactly one
heartbeat interval) are fine — document why timing is the assertion.

### Subtest fixture isolation

For tests with multiple `t.Run(...)` subtests that share a single
`*Handler` and `defer h.Cleanup()`, prefer constructing one handler per
subtest (or use `t.Cleanup` per-subtest). Shared fixtures couple
subtest order; a regression in subtest A can corrupt subtest B in ways
that are hard to bisect. The existing pattern is being phased out — new
tests should be order-independent.

## Mutation testing

We use [gremlins](https://github.com/go-gremlins/gremlins) to measure test
**strength**, not just presence. Coverage tells you a line was touched;
mutation testing tells you a regression on that line would have been caught.

Targets per file and the survivor-review policy are in
[docs/mutation/targets.md](docs/mutation/targets.md).

### When mutation testing is required

Any PR that adds or modifies code in `auth.go`, `helpers.go`, `handler.go`,
`serve.go`, `caddyfile.go`, or `provision.go` must:

1. Run `make mutate-pkg PKG=<changed-file>` locally and report the MSI in
   the PR description.
2. For each new surviving mutant, either kill it with a test, document it
   as equivalent in
   [docs/mutation/equivalents.md](docs/mutation/equivalents.md), or flag it
   for human review in the PR. Don't silently ignore.

The full-module run (`make mutate`) is too slow to gate every PR — that's
why it runs weekly on a schedule (see the `mutation.yml` workflow). Per-PR
gating uses the scoped `mutate-pkg` form.

### Waiver process

A maintainer can waive the mutation-coverage requirement for a specific PR
when:

- The change is a pure refactor with no behaviour change (test suite
  proves equivalence).
- The change is doc-only.
- The change is dependency bumps with no logic edits.

The waiver must be stated explicitly in the PR description so the reason is
durable.

### Setup

Install the binary once: `make mutate-tools`. The version is pinned in the
Makefile so contributors and CI agree.

## Contribution checklist

Use this checklist before opening a PR. It mirrors the main CI gates; run the
subset that matches the change when a full local pass would be unreasonable.

- [ ] `gofmt -l .` returns no files.
- [ ] `go test -timeout 120s .` passes.
- [ ] `go test -run '^$' ./...` passes.
- [ ] `go test -race -timeout 180s .` passes, or the PR explains why the
  focused CI race policy is sufficient for the change.
- [ ] `go vet ./...` passes.
- [ ] `make lint` passes locally, matching the GitHub Actions lint version.
- [ ] `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` reports no
  reachable vulnerabilities.
- [ ] `make test-functional` passes for HTTP, CORS, replay, and Docker/NATS
  behavior changes.
- [ ] `make test-functional-matrix` passes for NATS compatibility changes.
- [ ] `make test-performance` passes for formatter, buffering, replay, or
  concurrency changes.
- [ ] Production Docker image changes build and pass
  `caddy adapt --config /app/Caddyfile` inside the image.
- [ ] Release tooling changes pass `make release-check` and a GoReleaser
  snapshot dry run.
- [ ] Documentation, changelog, and examples are updated for new directives,
  operational behavior, or upgrade-impacting changes.
- [ ] `make mutate-pkg PKG=<changed-file>` ran for each modified file in
  `auth.go`, `helpers.go`, `handler.go`, `serve.go`, `caddyfile.go`,
  `provision.go`. PR description states the resulting MSI and how new
  survivors were handled (killed / documented as equivalent / flagged).
  See [Mutation testing](#mutation-testing) for the full policy.

## Commit messages

Follow the conventional form:

```
<type>(<scope>): <short summary>

<optional body>

<optional footer>
```

`type` is typically one of `feat`, `fix`, `docs`, `refactor`, `test`, `chore`.

## Releasing

Tags of the form `vX.Y.Z` trigger the Docker image publish workflow and the
GoReleaser GitHub release workflow. Update [CHANGELOG.md](CHANGELOG.md) in the
same commit that creates the tag, and run `make release-check` before tagging.
See [docs/RELEASE.md](docs/RELEASE.md) for the release gates, SBOMs, vulnerability scans,
and signing policy.

## Reporting issues

- Bugs: open a GitHub issue with reproduction steps and your Caddy/NUTS version.
- Security: **do not open a public issue**. See [docs/SECURITY.md](docs/SECURITY.md).
