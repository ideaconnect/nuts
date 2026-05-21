# Mutation testing baseline — 2026-05-21

First mutation testing run on `github.com/ideaconnect/nuts`. Treat this as
the **floor**: regressions are measured against this snapshot until the next
recorded baseline.

Run artefact:
[`runs/baseline-20260521T123714Z.json`](runs/baseline-20260521T123714Z.json).
Live log:
[`runs/baseline-20260521T123714Z.log`](runs/baseline-20260521T123714Z.log).

## Headline numbers

| Metric                | Value      |
| --------------------- | ---------- |
| **Test efficacy (MSI)** | **100.00%** |
| Mutation coverage     | 88.08%     |
| Mutants total         | 436        |
| Mutants killed        | 436        |
| Mutants lived         | 0          |
| Mutants not covered   | 59         |
| Mutants not viable    | 0          |
| Elapsed time          | ~8 minutes |

**Zero surviving mutants.** Every mutant that the test suite could observe
was killed. This is unusually strong for a first run — credit to the
existing combination of unit tests, hardening tests, and integration
tests under embedded NATS.

## Per-file breakdown

| File           | Total | Killed | Lived | Not covered | Runnable | MSI     | Mutation cov. |
| -------------- | ----- | ------ | ----- | ----------- | -------- | ------- | ------------- |
| `caddyfile.go` | 28    | 28     | 0     | 0           | 28       | 100.00% | 100.00%       |
| `provision.go` | 115   | 114    | 0     | 1           | 114      | 100.00% | 99.13%        |
| `serve.go`     | 213   | 208    | 0     | 5           | 208      | 100.00% | 97.65%        |
| `auth.go`      | 80    | 62     | 0     | 18          | 62       | 100.00% | 77.50%        |
| `helpers.go`   | 59    | 24     | 0     | 35          | 24       | 100.00% | 40.68%        |

Files with **zero mutants** in the report (no covered logic that gremlins
considered worth mutating):

- `handler.go` — module registration + interface guards, no
  conditional/arithmetic logic to mutate.
- `metrics.go` — Prometheus collector registration via `promauto`, declarative.

This matches the rationale for putting `metrics.go` in tier D in
[targets.md](targets.md).

## Mutator-type distribution

| Mutator                | Count |
| ---------------------- | ----- |
| `conditionals_negation`  | 288 |
| `conditionals_boundary`  | 82  |
| `invert_logical`         | 76  |
| `arithmetic_base`        | 24  |
| `invert_loop_ctrl`       | 9   |
| `increment_decrement`    | 8   |
| `invert_negatives`       | 8   |

Heavy weighting toward `conditionals_negation` and `invert_logical` —
expected for a request-path / auth-validation codebase. The fact that all
288 negation mutants were killed says the test suite genuinely exercises
both branches of conditionals, not just the happy path.

## What this means for the plan

The original Section 2–5 plan assumed we'd be hunting surviving mutants and
strengthening tests file-by-file. **There are no survivors.** The remaining
work pivots from *kill surviving mutants* to *raise mutation coverage*:

| File         | Why coverage is low | Where to invest                                                                     |
| ------------ | ------------------- | ----------------------------------------------------------------------------------- |
| `helpers.go` | 40.68% covered      | 35 uncovered mutants. Likely URL-redaction / cookie-name edge cases without tests.  |
| `auth.go`    | 77.50% covered      | 18 uncovered mutants. Likely error-path branches not exercised by current tests.    |
| `serve.go`   | 97.65% covered      | 5 uncovered mutants. Marginal — investigate but don't over-invest.                  |
| `provision.go` | 99.13% covered    | 1 uncovered mutant. Probably worth a one-test fix.                                  |

This is a healthier starting point than expected. Section 2 (survivor
triage) becomes "uncovered-mutant triage" — same workflow, different label.

## Calibration of `.gremlins.yaml` thresholds

Now that we have a baseline, the `unleash.threshold.*` fields in
`.gremlins.yaml` should be populated:

- `efficacy: 95` — current MSI is 100%; 95% leaves a 5pp buffer for honest
  test churn without ratcheting the requirement so high that one
  legitimately-equivalent mutant breaks CI. The weekly workflow has its
  own ±2pp regression check on top.
- `mutant-coverage: 80` — current is 88.08%; 80% guards against
  regressions while leaving room for the `helpers.go` coverage gap to be
  addressed without immediate CI fallout.

Apply these in a follow-up commit alongside the calibrated targets.

## Reproduce locally

```bash
make mutate-tools          # installs pinned gremlins
docker compose up -d --wait  # gremlins runs ./... for coverage
gremlins unleash --output docs/mutation/runs/local-$(date -u +%Y%m%dT%H%M%SZ).json .
docker compose down -v
```

Or, for a single file:

```bash
make mutate-pkg PKG=auth.go
```
