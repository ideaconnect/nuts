# Equivalent and structurally-untestable mutants

Mutants that cannot be killed by any test fall into one of three buckets.
This file is the canonical log; the mutation-coverage policy in
[AGENTS.md](../../AGENTS.md#mutation-testing) and
[CONTRIBUTING.md](../../CONTRIBUTING.md#mutation-testing) requires every
uncovered mutant to be either killed, accepted, or documented here.

## Categories

- **Equivalent** — the mutated code runs but produces behaviour identical
  to the original (e.g. an arithmetic identity, a redundant boolean, a
  logging-only difference).
- **Structurally untestable** — no test could possibly reach the mutated
  line (e.g. defensive guard against an impossible state, dead branch
  preserved for clarity).
- **Coverage-tool blind spot** — tests DO exercise the line, but Go's
  coverage tool fails to record it, so gremlins reports NOT_COVERED.
  Treated as documentation-only until the source can be refactored.

## Current log

### 2026-05-21 — Empty `case` bodies in character-class switches *(resolved)*

| File / lines             | Mutants affected | Resolution |
| ------------------------ | ---------------- | ---------- |
| `helpers.go:92–95`       | 20               | Refactored to `isAllowedTopicByte`. All mutants now KILLED. |
| `helpers.go:121–123`     | 15               | Refactored to `isAllowedCookieNameByte`. All mutants now KILLED. |
| `auth.go:383–386`        | 18               | Refactored to `isAllowedFilterTokenByte`. All mutants now KILLED. |

Original problem: 53 `NOT COVERED` mutants on character-class branches of
`isValidTopic`, `isValidCookieName`, and `isValidTopicFilter`. Go's
coverage tool doesn't instrument empty `case ...:` bodies, so even though
existing tests exercised every branch, gremlins reported them as
uncovered.

**Resolution (PR pending):** each switch was replaced with a small boolean
predicate helper. Go's coverage tool instruments expressions normally, so
the existing tests now show full coverage and gremlins places these
mutants in the runnable+killed bucket. See
[`runs/postrefactor-20260521T135733Z.json`](runs/postrefactor-20260521T135733Z.json):
helpers.go 40.68% → 100% covered, auth.go 77.50% → 100% covered, all
mutants KILLED.

Keeping the entry as historical record of the pattern. Future contributors
who see "empty case bodies show as NOT_COVERED" should reach for the same
refactor before reaching for this file.

### 2026-05-21 — serve.go ServeHTTP edge branches (accepted)

After the Section 4 work (`finalizeStreamedMessage` extraction + the
`streamMetadataReader` interface), serve.go reached **99.06% mutation
coverage with 100% MSI**. Two uncovered mutants remain:

| Location           | Mutator             | Branch                                                                                  | Category |
| ------------------ | ------------------- | --------------------------------------------------------------------------------------- | -------- |
| `serve.go:351:15`  | CONDITIONALS_NEGATION | `if h.logger != nil` inside the "failed to subscribe to any requested topics" branch    | Accepted gap |
| `serve.go:883:5`   | INVERT_LOOPCTRL     | `if msg == nil { continue }` guard against a closed JetStream subscription channel      | Accepted gap |

**Why accepted (line 351):** the outer error path requires constructing a
request whose *every* requested topic fails subscription — a contrived
scenario that needs multi-topic JetStream stubbing far beyond the current
test infrastructure. The inner `h.logger != nil` is a guard for tests
that construct `&Handler{}` directly; it's evaluated correctly in the
common path. Cost of writing the test is disproportionate to the value
(one mutant on one logger-nil guard).

**Why accepted (line 883):** the guard exists because a `select { case
msg := <-msgChan: ... }` can receive nil if `msgChan` is closed
externally. Testing this requires racing the channel close against the
streaming loop. Removing the guard would risk a nil-pointer dereference
on `msg.Subject` two lines below. Keeping the guard is defensive; the
single uncovered LOOPCTRL mutant is the cost.

**Re-evaluate** these decisions if either: (a) serve.go's `ServeHTTP`
gets refactored to extract more testable pieces; (b) a real bug surfaces
in either branch — at that point a targeted test is justified by the bug
report alone.

## How to add an entry

1. Reproduce locally with `make mutate-pkg PKG=<file>`.
2. Add a new dated section below with: file:line, mutator, category,
   evidence (test names that exercise the line, or a clear explanation
   why no test ever could).
3. Link the PR that introduced the entry.

Do not add entries to dodge the policy. The default action on a
surviving / uncovered mutant is **kill it**. This file is for the cases
where killing it would require either a tool fix or a code change
unrelated to the PR.
