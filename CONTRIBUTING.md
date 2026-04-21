# Contributing to NUTS

Thanks for your interest in improving NUTS! This document summarizes how to
get a development environment running and the expectations for contributions.

## Development environment

Requirements:

- Go 1.25+
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
go test ./...
```

Run the functional (Godog) suite:

```bash
make test-functional
```

## Coding guidelines

- Run `gofmt -s -w .` and `go vet ./...` before committing.
- Run `golangci-lint run` locally once it is wired up in CI (see `.golangci.yml`).
- Keep exported symbols documented. New Caddyfile directives must be documented
  in [README.md](README.md) and the handler struct fields in
  [handler.go](handler.go).
- Prefer small, focused PRs. One logical change per PR.
- Include unit tests for new behavior. If the change affects the HTTP surface,
  also add a Godog scenario under [features/](features/).

## Commit messages

Follow the conventional form:

```
<type>(<scope>): <short summary>

<optional body>

<optional footer>
```

`type` is typically one of `feat`, `fix`, `docs`, `refactor`, `test`, `chore`.

## Releasing

Tags of the form `vX.Y.Z` trigger the Docker image publish workflow. Update
[CHANGELOG.md](CHANGELOG.md) in the same commit that creates the tag.

## Reporting issues

- Bugs: open a GitHub issue with reproduction steps and your Caddy/NUTS version.
- Security: **do not open a public issue**. See [SECURITY.md](SECURITY.md).
