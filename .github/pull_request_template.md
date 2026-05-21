<!--
Thanks for contributing to NUTS! Please fill out the sections below.
Delete what doesn't apply. Don't leave headings empty.
-->

## Summary

<!-- One or two sentences. What does this PR change and why? -->

## Type of change

- [ ] Bug fix
- [ ] New feature / directive
- [ ] Refactor (no behaviour change)
- [ ] Docs / examples / changelog only
- [ ] Build / CI / release tooling
- [ ] Test improvements only

## Test plan

<!-- How did you verify this works? Include commands you ran. -->

## Mutation coverage

<!--
Required when this PR touches auth.go, helpers.go, handler.go, serve.go,
caddyfile.go, or provision.go. Skip the section (or write "N/A — docs only"
etc.) for changes that fall outside that set.

See CONTRIBUTING.md § Mutation testing for the policy and waiver criteria.
-->

- [ ] Ran `make mutate-pkg PKG=<file>` for each changed file in the list above.
- [ ] MSI per file is reported below.
- [ ] New surviving mutants are killed, documented in
      `docs/mutation/equivalents.md`, or flagged in this PR description.

| File | MSI before | MSI after | New survivors handled how |
| ---- | ---------- | --------- | ------------------------- |
|      |            |           |                           |

<!-- If a maintainer is waiving this requirement, paste the justification here. -->

## Checklist

<!-- Mirror of CONTRIBUTING.md § Contribution checklist. Run the subset that
matches your change. -->

- [ ] `gofmt -l .` reports no files.
- [ ] `make test-unit` passes.
- [ ] `go vet ./...` passes.
- [ ] `make lint` passes.
- [ ] `make test-functional` passes (HTTP/CORS/replay/Docker changes).
- [ ] Docs / changelog / examples updated where relevant.
