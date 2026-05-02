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
- Security: **do not open a public issue**. See [SECURITY.md](SECURITY.md).
