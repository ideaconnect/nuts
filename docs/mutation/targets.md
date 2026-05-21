# Mutation testing MSI targets

This document defines the per-package Mutation Score Indicator (MSI) targets
that mutation runs gate against. Targets are tiered by **risk** (security
impact, blast radius if a bug ships) rather than by coverage achievability.
A high-risk file with a hard-to-test branch gets a higher bar, not a lower
one — that's a signal to invest in better tests, not lower expectations.

MSI is defined as gremlins defines it:

```
MSI = killed / (killed + lived)
```

`NOT_COVERED` and `TIMED_OUT` mutants are excluded from the denominator.

## Per-file targets

| File              | Target MSI | Tier  | Why this tier                                                                                          |
| ----------------- | ---------- | ----- | ------------------------------------------------------------------------------------------------------ |
| `auth.go`         | **≥ 85%**  | A     | Subscriber JWT verification + `subscribe` claim parsing. A surviving mutant here is an auth bypass.    |
| `helpers.go`      | **≥ 85%**  | A     | Topic validation (`isValidTopic`), URL redaction, cookie name validation — all security-adjacent.      |
| `handler.go`      | **≥ 80%**  | B     | Module registration + interface guards. Smaller surface but mistakes here break Caddy integration.     |
| `serve.go`        | **≥ 80%**  | B     | `ServeHTTP`, replay planning, slow-client backpressure, CORS path. Largest behaviour surface.          |
| `caddyfile.go`    | **≥ 75%**  | C     | Directive parsing. Bugs are usually caught by parse-time validation; user-facing but lower blast.      |
| `provision.go`    | **≥ 75%**  | C     | Provision + Validate + NATS dial + TLS. Init-only code; reviewed by `Validate()` warnings on misuse.   |
| `metrics.go`      | **≥ 60%**  | D     | Prometheus collector registration via `promauto`. Most mutants are equivalent (no behavioural change). |

## Tier rationale

**Tier A — security-critical (85%).** Code where a surviving mutant
corresponds to a real auth bypass, validation bypass, or information
disclosure. We invest until we cannot kill more without writing tests that
overspecify implementation (in which case the survivor is documented as
equivalent, not accepted as a gap).

**Tier B — request path (80%).** High blast radius (every request touches
this) but failures are typically observable in functional tests too. 80% is
"the test suite enforces the behaviour we care about" without forcing
property-based tests on every helper.

**Tier C — configuration (75%).** Parse-and-validate code. Defects here are
loud (Caddy refuses to start) and rarely reach production. 75% catches
boundary errors without chasing every default-value mutation.

**Tier D — instrumentation (60%).** Metrics registration is mostly
declarative; many mutants don't change observable behaviour. Setting a high
bar here would force us to write tests against Prometheus internals.

## How these are enforced

1. **`make mutate-pkg PKG=<file>`** runs gremlins scoped to one file and
   prints the per-file MSI. CI agents read this number for PR gating
   (see [AGENTS.md](../../AGENTS.md) §Mutation testing requirement once that
   policy lands).
2. **`make mutate`** (full module) is run by the scheduled weekly GitHub
   Action and produces a JSON report under `docs/mutation/runs/`.
3. Once the baseline is recorded in [baseline.md](baseline.md), the
   `unleash.threshold.efficacy` field in `.gremlins.yaml` will be set to the
   weighted-average target so the weekly run fails on regressions. PR-time
   gating relies on the per-file target read from this document, not from
   the global threshold.

## Reviewing a survivor

When a survivor refuses to die, decide:

- **Kill it.** Add a test that exercises the affected behaviour with the
  right boundary or branch. This is the default path.
- **Document as equivalent.** The mutated code is behaviourally identical
  to the original (e.g. dead branch, logging-only difference,
  micro-optimisation). Record in
  [equivalents.md](equivalents.md) with file:line, mutator, original→mutated
  diff, and a one-paragraph justification.
- **Accept the gap.** Rare. Requires written justification (cost,
  third-party constraint) and a follow-up tracking issue.

The order matters: try to kill before you accept.

## Revisiting targets

Targets are revisited:

- After the first baseline lands (initial calibration).
- When a major refactor changes the testability profile of a file.
- When the weekly workflow has shown ≥ 4 consecutive runs hitting the
  current target — raise the bar; the codebase has earned it.

Lowering a target requires a PR description that explains why the new bar
is honest, not convenient.
