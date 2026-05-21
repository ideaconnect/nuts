# Uncovered mutants — 2026-05-21 baseline

The 2026-05-21 baseline produced 59 `NOT COVERED` mutants. This document
classifies them by **root cause**, not just by file, because the right
intervention differs by category.

Source data: [`runs/baseline-20260521T123714Z.json`](runs/baseline-20260521T123714Z.json).

> **Update — 2026-05-21 post-refactor (Section 3):** Category 1 (53
> mutants) was eliminated by the predicate-helper refactor in
> `helpers.go` and `auth.go`. Module-wide mutation coverage 88.08% →
> **98.81%**. MSI held at 100%. Report:
> [`runs/postrefactor-20260521T135733Z.json`](runs/postrefactor-20260521T135733Z.json).
>
> **Update — 2026-05-21 post-serve (Section 4):** Extracted
> `streamMetadataReader` interface + `finalizeStreamedMessage` helper.
> Three serve.go mutants closed (former 602, 609, 891). Module coverage
> 98.81% → **99.40%**. The 3 remaining uncovered mutants are:
> `provision.go:202` (Section 5), `serve.go:351`, and `serve.go:883`.
> The latter two are documented as accepted gaps in
> [equivalents.md](equivalents.md). Report:
> [`runs/postserve-20260521T142809Z.json`](runs/postserve-20260521T142809Z.json).
>
> **Update — 2026-05-21 post-provision (Section 5):** Added
> `TestHandler_ConnectNATS_TLSConfigErrorPropagates` exercising the
> `buildTLSConfig` error return in `connectNATS`. provision.go reached
> **100% mutation coverage**. Module coverage 99.40% → **99.60%**. The
> only remaining uncovered mutants are the 2 accepted gaps in
> `serve.go`. Report:
> [`runs/postprovision-20260521T144604Z.json`](runs/postprovision-20260521T144604Z.json).

## Headline

The 59 uncovered mutants split into **two very different categories**:

| Category                                | Count | Real test gap? | Action |
| --------------------------------------- | ----- | -------------- | ------ |
| Empty `case` bodies in `switch` (Go coverage blind spot) | 53    | **No**         | Refactor source so coverage instruments these branches |
| Error / defensive branches genuinely not exercised | 6     | **Yes**        | Write targeted tests |

The headline mutation-coverage number of 88.08% **understates the test
suite's actual strength**. Once the 53 coverage-tool blind spots are
refactored away, mutation coverage will jump to ~99% with no additional
tests needed.

## Counts by file × mutator

| File           | COND_BOUNDARY | COND_NEGATION | INVERT_LOGICAL | INVERT_LOOPCTRL | Total |
| -------------- | ------------- | ------------- | -------------- | --------------- | ----- |
| `helpers.go`   | 12            | 15            | 8              | 0               | 35    |
| `auth.go`      | 6             | 8             | 4              | 0               | 18    |
| `serve.go`     | 0             | 3             | 0              | 2               | 5     |
| `provision.go` | 0             | 1             | 0              | 0               | 1     |
| **Total**      | **18**        | **27**        | **12**         | **2**           | **59** |

## Category 1 — Go coverage blind spot (53 mutants)

Go's coverage tool counts statements, not branches. An empty case body has
no statement to instrument:

```go
switch {
case c >= 'a' && c <= 'z':              // empty body → counted as "uncovered"
case c >= 'A' && c <= 'Z':              // even though tests definitely hit here
case c >= '0' && c <= '9':
case c == '.' || c == '-' || c == '_':
default:
    return false                         // only this is instrumented
}
```

The branch condition itself (`c >= 'a' && c <= 'z'`) carries the mutant
operators (CONDITIONALS_BOUNDARY, CONDITIONALS_NEGATION, INVERT_LOGICAL),
so gremlins sees the line as a mutation target. But because the case body
is empty, Go's coverage profile shows 0 hits, and gremlins skips it as
NOT_COVERED.

**Tests already exercise these branches.** Evidence:

- `helpers_test.go:10-29` — `TestIsValidCookieName_Cases` includes
  `"Session_Id-2"` which hits uppercase (S, I), lowercase, digit, and
  special chars in a single input.
- `nats_test.go:1883` — `TestIsValidTopic_Cases` (49 cases) covers the
  full ASCII range.
- `auth_test.go:127` — `TestIsValidTopicFilter_Cases` covers the
  subscribe-claim character class.

**Locations:**

| File         | Lines exhibiting the pattern                              |
| ------------ | --------------------------------------------------------- |
| `helpers.go` | 92–95 (`isValidTopic`), 121–123 (`isValidCookieName`)     |
| `auth.go`    | 383–386 (subscribe-claim filter validation)               |

**Recommended fix (Section 3):** refactor the character-class switches
into helper predicates returning `bool`. The helper is a single
expression, Go's coverage tool instruments it normally, and gremlins
mutates the same branches — except now they end up in the
runnable+killed bucket because tests already exercise the helper.

Sketch:

```go
func isAllowedTopicByte(c byte) bool {
    return (c >= 'a' && c <= 'z') ||
        (c >= 'A' && c <= 'Z') ||
        (c >= '0' && c <= '9') ||
        c == '.' || c == '-' || c == '_'
}

// isValidTopic uses it:
for i := 0; i < len(topic); i++ {
    if !isAllowedTopicByte(topic[i]) {
        return false
    }
}
```

Expected impact: helpers.go coverage 40.68% → ~98%; auth.go coverage 77.5%
→ ~100%. MSI stays at 100%.

## Category 2 — Genuine test gaps (6 mutants)

Six mutants on code paths that no test exercises today. These need
focused tests:

| File / line          | Mutator             | Code context                                                                             | Test idea |
| -------------------- | ------------------- | ---------------------------------------------------------------------------------------- | --------- |
| `provision.go:202`   | CONDITIONALS_NEGATION | `if err != nil { return err }` after `h.buildTLSConfig()` — TLS config build failure path | Provide an invalid TLS path (e.g., non-existent CA file) and assert provisioning fails with the wrapped error. |
| `serve.go:342`       | CONDITIONALS_NEGATION | `if h.logger != nil` inside "failed to subscribe to any requested topics" branch         | Provision a handler with `h.logger = nil` and trigger the empty-subscriptions path. |
| `serve.go:602`       | CONDITIONALS_NEGATION | `} else if h.logger != nil` for replay-sequence timestamp read failure                   | Force `js.GetMsg` to return an error during replay snapshot. Logger-nil variant. |
| `serve.go:609`       | CONDITIONALS_NEGATION | `} else if h.logger != nil` for StreamInfo failure during pre-check                      | Force `js.StreamInfo` to error. Logger-nil variant. |
| `serve.go:874`       | INVERT_LOOPCTRL     | `if msg == nil { continue }` after `msg := <-msgChan`                                    | Push a `nil` message through `msgChan` and assert the stream loop continues. |
| `serve.go:891`       | INVERT_LOOPCTRL     | `if shouldSkipReplayWindowMessage(plan, formatted) { continue }`                         | Stage a message that's outside the replay window and assert it's skipped, not delivered. |

Three of the five serve.go ones are logger-nil guards. They could be
collapsed into a single test that runs the affected code paths with
`h.logger = nil` and asserts no panic.

## Per-mutant inventory

Full list at file:line:column granularity. Use this to drive Section 3
test additions.

### helpers.go (35)

All on `isValidTopic` (92–95) and `isValidCookieName` (121–123). See
Category 1.

### auth.go (18)

All on the subscribe-claim topic-filter character class (383–386). See
Category 1.

### serve.go (5)

- `serve.go:342:15`  CONDITIONALS_NEGATION  — logger-nil guard
- `serve.go:602:24`  CONDITIONALS_NEGATION  — logger-nil guard
- `serve.go:609:22`  CONDITIONALS_NEGATION  — logger-nil guard
- `serve.go:874:5`   INVERT_LOOPCTRL        — `continue` on nil message
- `serve.go:891:5`   INVERT_LOOPCTRL        — `continue` on replay-window skip

### provision.go (1)

- `provision.go:202:10`  CONDITIONALS_NEGATION  — TLS build error path

## What this enables for Section 3+

1. **Section 3 (helpers.go + auth.go):** primary work is the refactor in
   Category 1 — single source change, zero new tests, fixes 53 of 59
   uncovered. Verify with `make mutate-pkg PKG=helpers.go` and
   `PKG=auth.go`.
2. **Section 4 (serve.go):** 5 targeted tests for the genuine gaps above.
3. **Section 5 (provision.go):** 1 targeted test for the TLS-config
   failure path.

Expected end state after Sections 3–5: **mutation coverage ≥ 99%** with
MSI still at 100%.
