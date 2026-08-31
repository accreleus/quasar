## What

<!-- One or two sentences. What does this change do? -->

## Why

<!-- The problem, the issue number, or the decision this implements. -->

## Gates run

<!-- Tick what you actually ran, and paste the RESULT line where there is one.
     `make verify` is the baseline for any change; see AGENTS.md
     "Verification levels" for what your change additionally requires. -->

- [ ] `make verify`
- [ ] `make test-go` · `make test-db` (required for DB-touching Go changes — a green
      `test-go` alone means the DB tests were SKIPPED)
- [ ] `make test-rust`
- [ ] `make test-web`
- [ ] Remote validation on the `gpu-test` host (required for pipeline / encoder /
      streaming changes — a compiling pipeline is not a working pipeline)

## Screenshots

<!-- Required for any change to a user-visible surface, alongside the
     design-handoff visual check (CLAUDE.md "UI work"). Delete if not a UI change. -->
