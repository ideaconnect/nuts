# What mutation testing covers

`gremlins` generates **operator** mutants only: it negates and shifts
comparisons, swaps `&&`/`||`, changes arithmetic, inverts `break`/`continue`
and negates numbers. The MSI in [the current baseline](baseline-20260927.md) is the share of
those mutants the suite kills. It says nothing about statement-level
changes: deleting a call, reverting a fix, reordering guards or removing a
`select` arm. In the 2026-09-26 review, 8 of 11 hand-made statement mutants
passed the whole suite (#128). The 2026-09-27 baseline records the
statement-level mutants run by hand since, and what each survivor led to.

## Blind spots of `gremlins` itself

Go's coverage profile does not instrument some lines, so `gremlins` reports
their mutants as `NOT COVERED` even when tests exercise them:

- the `case` expressions of a tagless `switch` (the case bodies are
  instrumented, the conditions are not);
- constant declarations such as `const timeout = 3 * time.Second`.

## Checking by hand

[`scripts/mutate-by-hand.py`](../../scripts/mutate-by-hand.py) applies one
mutant at a time, runs the tests matching a regex, reports `KILLED`, `LIVED`
or `NOT VIABLE` (rejected by the compiler or `go vet`), and restores the file:

```bash
scripts/mutate-by-hand.py serve.go 'PlanSubscription|DeliveryContract' \
  '644::StartSequence < snapshot.FirstSeq::StartSequence <= snapshot.FirstSeq' \
  '836:: || snapshot.LastSeq < replay.StartSequence {:: {'
```

Every change on the `v1-remediation` branch was checked this way in addition
to `gremlins --diff`:

- the `NOT COVERED` mutants of each diff, by hand;
- **reverse mutants**: each fix reverted on its own (the fix's condition
  forced false, its call removed), to prove the test written for it fails
  without it.

A survivor is either a missing test, which is then written, or an equivalent
mutant. Where an equivalent mutant came from redundant code, the code was
simplified instead of recording the mutant (for example a `FirstSeq > 0`
guard made redundant by an unsigned comparison, and a bounds check made
redundant by `json.Valid`).
