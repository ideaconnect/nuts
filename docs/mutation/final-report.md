# Mutation testing — final report (2026-05-21)

End-of-initiative summary for the NUTS Mutation project tracked in Asana.
Consolidates [`baseline.md`](baseline.md), [`uncovered.md`](uncovered.md),
[`equivalents.md`](equivalents.md), and the three intermediate run
reports under [`runs/`](runs/).

## Headline

| Metric | Baseline | Final | Δ |
| --- | ---: | ---: | ---: |
| **Test efficacy (MSI)** | 100.00% | **100.00%** | — |
| **Mutation coverage** | 88.08% | **99.60%** | **+11.52 pp** |
| Total mutants | 436 | 503 | +67 |
| Killed | 436 | **501** | +65 |
| Lived | 0 | 0 | — |
| Not covered | 59 | **2** | −57 |

MSI was already at 100% in the baseline. The work raised mutation
coverage from 88.08% to 99.60% — the residual 0.40% is two
explicitly-accepted defensive guards in `serve.go`.

## Per-file results

| File | Baseline cov. | Final cov. | MSI | Target | Status |
| ---- | ---: | ---: | ---: | --- | --- |
| `auth.go`      | 77.50% | **100.00%** | 100% | ≥ 85% | exceeded |
| `caddyfile.go` | 100.00% | 100.00% | 100% | ≥ 75% | met (baseline) |
| `helpers.go`   | 40.68% | **100.00%** | 100% | ≥ 85% | exceeded |
| `provision.go` | 99.13% | **100.00%** | 100% | ≥ 75% | exceeded |
| `serve.go`     | 97.65% | **99.06%** | 100% | ≥ 80% | exceeded |
| `handler.go`   | n/a (0 mutants) | n/a | n/a | ≥ 80% | n/a (no logic) |
| `metrics.go`   | n/a (0 mutants) | n/a | n/a | ≥ 60% | n/a (declarative) |
| **Module**     | **88.08%** | **99.60%** | **100%** | — | — |

## What shipped

### Infrastructure

- [`.gremlins.yaml`](../../.gremlins.yaml) — pinned mutator set, calibrated
  thresholds (`efficacy: 95`, `mutant-coverage: 80`), `cmd/caddy/`
  excluded from mutation.
- [`Makefile`](../../Makefile) — `mutate`, `mutate-pkg PKG=...`, `mutate-tools`
  targets. Gremlins version pinned via `GREMLINS_VERSION`.
- [`.github/workflows/mutation.yml`](../../.github/workflows/mutation.yml) —
  Sunday 03:00 UTC + manual dispatch. Pinned-version read from Makefile.
  Docker NATS up/down. Artifact upload (90-day retention). Δ MSI computed
  from the previous run's artifact; fails the workflow on a drop > 2 pp.
  No `pull_request` trigger (PRs are gated per-file, not module-wide).

### Policy

- [`AGENTS.md`](../../AGENTS.md) — mandatory pre-PR `make mutate-pkg` for
  changes touching `auth.go`, `helpers.go`, `handler.go`, `serve.go`,
  `caddyfile.go`, `provision.go`. New survivors must be killed,
  documented as equivalent, or flagged for human review.
- [`CONTRIBUTING.md`](../../CONTRIBUTING.md) — same policy mirrored for
  humans with an explicit waiver process (refactor / docs-only /
  dep-bump). Contribution checklist gains a mutation-coverage line.
- [`.github/pull_request_template.md`](../../.github/pull_request_template.md) —
  Mutation-coverage section visible at PR creation.

### Code

| File | Change |
| ---- | ------ |
| [`helpers.go`](../../helpers.go) | Extracted `isAllowedTopicByte` and `isAllowedCookieNameByte` predicate helpers. Switch statements simplified. |
| [`auth.go`](../../auth.go) | Extracted `isAllowedFilterTokenByte` predicate helper. Subscribe-claim filter switch simplified. |
| [`serve.go`](../../serve.go) | Added `streamMetadataReader` interface (subset of `nats.JetStreamContext`). Added `(*Handler).finalizeStreamedMessage` helper. `readStreamSnapshot` signature now uses the narrower interface. |

### Tests

| File | Tests added |
| ---- | ----------- |
| [`serve_test.go`](../../serve_test.go) | `TestHandler_ReadStreamSnapshot_StreamInfoErrorReturnsEmptySnapshot`, `TestHandler_ReadStreamSnapshot_GetMsgErrorKeepsSnapshotWithoutStartTime`, `TestHandler_FinalizeStreamedMessage` (table-driven, 3 sub-cases) |
| [`handler_integration_test.go`](../../handler_integration_test.go) | `TestHandler_ConnectNATS_TLSConfigErrorPropagates` |

No new tests were written for `auth.go` or `helpers.go`. The character-class
refactor surfaced the coverage that was already there from existing tests.

## Accepted survivors

Two NOT_COVERED mutants remain, both in `serve.go`, both documented in
[`equivalents.md`](equivalents.md) with explicit re-evaluation criteria.

| Location | Mutator | Branch | Why accepted |
| -------- | ------- | ------ | ------------ |
| `serve.go:351` | CONDITIONALS_NEGATION | `if h.logger != nil` inside the "failed to subscribe to any requested topics" error branch | Outer error requires every topic in a multi-topic request to fail subscription. Test setup cost is disproportionate to the one-mutant payoff. |
| `serve.go:883` | INVERT_LOOPCTRL | `if msg == nil { continue }` guard against a closed subscription channel | Removing the guard risks nil-pointer deref two lines below. Testing requires racing channel-close against the streaming loop. |

Both will be re-examined if (a) `ServeHTTP` is refactored into more
testable pieces, or (b) a real bug surfaces in either branch.

## Run history

| Date | Report | MSI | Mut. coverage | Notes |
| ---- | ------ | --: | ---: | ----- |
| 2026-05-21 | [`baseline-20260521T123714Z.json`](runs/baseline-20260521T123714Z.json) | 100.00% | 88.08% | First run |
| 2026-05-21 | [`postrefactor-20260521T135733Z.json`](runs/postrefactor-20260521T135733Z.json) | 100.00% | 98.81% | Section 3 — predicate-helper refactor |
| 2026-05-21 | [`postserve-20260521T142809Z.json`](runs/postserve-20260521T142809Z.json) | 100.00% | 99.40% | Section 4 — serve.go extractions |
| 2026-05-21 | [`postprovision-20260521T144604Z.json`](runs/postprovision-20260521T144604Z.json) | 100.00% | 99.60% | Section 5 — connectNATS TLS-error test |

## Time

| Phase | Effort |
| ----- | ------ |
| Infrastructure (Section 1)         | ~30 min |
| Policy + workflow (Section 6 part) | ~30 min |
| Triage + analysis (Section 2)      | ~30 min |
| `helpers.go` / `auth.go` refactor (Section 3) | ~15 min |
| `serve.go` extractions + tests (Section 4) | ~45 min |
| `provision.go` test (Section 5)    | ~15 min |
| Documentation                      | continuous |

Mutation runs themselves: ~8 min baseline, ~9 min for each re-run. Four
full-module runs total.

## Lessons learned

1. **Coverage tools have blind spots that look like test gaps.** 53 of 59
   "uncovered" mutants were on empty `case` bodies in character-class
   switches — Go's coverage tool doesn't instrument them. Tests *did*
   exercise the branches; gremlins reported NOT_COVERED because the
   profile said 0 hits. The fix is a source refactor (predicate helper),
   not new tests. Worth checking for this pattern before writing tests
   to chase a coverage gap.

2. **Pivot the plan when the baseline says so.** The original Section
   2–5 plan assumed surviving mutants. The baseline showed zero
   survivors and the gap was elsewhere. Rebuilding Sections 2–5 around
   the actual data (uncovered-mutant triage instead of survivor triage)
   was a one-message refactor that saved days of mis-targeted work.

3. **Tier targets by risk, not by ease.** `targets.md` uses 85% for
   security-critical files and 60% for instrumentation. A flat target
   would either over-invest in `metrics.go` or under-invest in `auth.go`.

4. **Per-PR mutation is per-file, not module-wide.** A module-wide run
   takes ~9 minutes. Gating every PR on that would be brutal. The policy
   in CONTRIBUTING.md scopes per-PR enforcement to `make mutate-pkg
   PKG=<file>` and reserves the module-wide run for the weekly workflow.

5. **Defensive code is a mutation-testing tax.** `if h.logger != nil`,
   `if msg == nil { continue }` — defensive guards exist for good
   reasons but show up as uncovered branches that resist testing. The
   pragmatic call is to document them as accepted with re-evaluation
   criteria rather than contrive tests that pin defensive behaviour.

6. **`equivalents.md` is load-bearing.** Without a published policy
   ("kill / document as equivalent / accept the gap") and a canonical
   log, an "accepted" mutant becomes "ignored." The doc is explicit
   that it isn't a junk drawer — every entry needs file:line, mutator,
   and a justification.

## Where the policy goes from here

- Weekly scheduled mutation run continues. The workflow fails on a > 2
  pp regression in MSI vs. the prior week.
- Per-PR `make mutate-pkg` is enforced via the PR template + reviewer
  checklist for changes to the security-critical files.
- The next mutation initiative is reactive: a real bug, a major
  refactor of `ServeHTTP`, or the addition of a new directive will
  trigger re-triage of the accepted survivors.
- Target updates happen when ≥ 4 consecutive weekly runs sit at the
  current level — at that point the bar has been earned and can be
  raised; see [`targets.md`](targets.md) § "Revisiting targets."

## Asana

All 26 tasks across 6 sections closed. Two closed as moot (handler.go
and caddyfile.go had nothing to address), two as not-needed (auth-error
negative-path tests, helpers property-based tests — both subsumed by
the Section 3 refactor), the rest as completed work with per-task
comments tracking the actual approach.
