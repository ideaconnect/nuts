# Release And Supply Chain

This project publishes two release surfaces:

- GitHub release archives built by GoReleaser from `.goreleaser.yml`.
- The `idcttech/nuts` Docker image built by GitHub Actions.

## Local Release Validation

Run GoReleaser config validation without installing GoReleaser locally:

```bash
make release-check
```

The target runs `goreleaser check` inside `goreleaser/goreleaser:v2.8.2`.
It requires Docker but not a host GoReleaser binary.

## CI Release Gates

Pull requests that touch `.goreleaser.yml`, release workflows, the `Makefile`,
or the production `Dockerfile` run a GoReleaser snapshot dry run:

```bash
goreleaser release --snapshot --clean --skip=publish
```

The snapshot verifies archive builds and release SBOM generation before a tag is
cut. Normal CI still runs unit tests, vet, lint, govulncheck, functional matrix,
and Docker image validation.

## Vulnerability Scanning

Pull requests build the production Docker image locally and scan it with Trivy.
Tag builds scan the pushed multi-platform image by digest. Both scans fail on
unfixed high or critical vulnerabilities:

```bash
trivy image --exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL <image>
```

Go dependency reachability is checked separately by `govulncheck ./...` in CI.

## SBOMs

GoReleaser generates SBOMs for release archives through the `sboms` section in
`.goreleaser.yml`; the release workflow installs Syft before running
GoReleaser. Docker release builds enable BuildKit SBOM attestations with
`docker/build-push-action` (`sbom: true`). Pull request Docker builds also
upload an SPDX JSON SBOM artifact for the local CI image.

## Signing

Container images for version tags are signed keylessly with Cosign using GitHub
OIDC after the multi-platform image is pushed. Verify a release image with:

```bash
cosign verify idcttech/nuts:<version> \
  --certificate-identity-regexp 'https://github.com/ideaconnect/nuts/.github/workflows/ci.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

GitHub release archives are covered by SHA-256 checksums and release SBOMs.
Checksum signing can be added later with Cosign `sign-blob` if downstream
consumers require detached signatures for archive artifacts.

## Dependency Updates

Dependabot is configured for Go modules, the production Dockerfile, and GitHub
Actions. GitHub Actions are intentionally pinned by major version where those
actions publish stable major tags; stricter environments can pin by commit SHA
and let Dependabot propose SHA updates.

## Operator Release Notes Checklist

Release notes should be written for operators first. Include the items below
whenever they changed, even if the code change looks small.

- **Caddy:** minimum supported Caddy version, new Caddyfile directives, changed
  route-ordering expectations, probe path changes, and any required `caddy fmt`
  or `caddy adapt` behavior.
- **Go:** minimum Go toolchain version, build flag changes, race-test policy,
  and any known cross-compilation or container-build impact.
- **NATS:** minimum NATS server version assumptions, JetStream consumer behavior,
  stream subject requirements, auth/TLS changes, and compatibility notes for
  older NATS versions.
- **Replay:** changes to `last-id`, `Last-Event-ID`, fallback replay, retention
  assumptions, `replay_max_messages`, or `replay_window` semantics.
- **Security:** subscriber-auth boundaries, CORS credential behavior, NATS
  credential handling, TLS guidance, and any new deployment hardening steps.
- **Operations:** new or renamed metrics, structured log fields, alert rules,
  dashboard panels, readiness/liveness behavior, and runbook changes.
- **Artifacts:** Docker tags, archive checksums, SBOM availability, vulnerability
  scan status, and image-signing verification instructions.

Suggested release-note shape:

```markdown
## Operator Impact

- Upgrade urgency:
- Config changes:
- Deployment checks:
- Rollback notes:

## Compatibility

- Caddy:
- Go:
- NATS:
- Replay behavior:

## Verification

- Tests:
- Docker image:
- GoReleaser / SBOM / signing:
```
