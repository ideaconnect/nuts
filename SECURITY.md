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
- Set `max_connections` to a value appropriate for the host.
- Set `max_event_size` to bound memory per event.
- Restrict `allowed_origins` to your trusted front-ends.
- Run the container as a non-root user (already the default).
