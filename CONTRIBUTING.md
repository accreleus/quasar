# Contributing to Quasar

## This is early, dev-mode software

Quasar is under active development; tagged releases exist (see `CHANGELOG.md`, and
`deploy/README.md` "Publishing a platform release" for how one is cut), but interfaces move,
migrations are one-way (see `CLAUDE.md`), and some documented invariants exist
specifically because a workaround caused real pain earlier. Read `CLAUDE.md` and
`AGENTS.md` before making non-trivial changes; they cover the architecture
invariants, the frozen interfaces (`protocol/`), and the day-to-day build/test
workflow in more depth than fits here.

## Branching and pull requests

- `main` is production and is sign-off gated. Pull requests target `develop`,
  the persistent integration branch.
- Branch off `develop`, do the work, open a PR back into `develop`.
- Keep PRs scoped to one change; unrelated fixes belong in their own PR.
- Conventional-commits style for commit messages (`feat:`, `fix:`, `docs:`,
  `chore:`, etc.).

## Before opening a PR

Run the gates for whatever you touched:

- Rust (`node-agent/`): `cargo build`, `cargo test`, `cargo fmt`, and
  `cargo clippy -- -D warnings` clean, run inside the `quasar-agent-dev` container
  (see `AGENTS.md`), not on the host.
- Go (`control-plane/`): `go build ./...`, `go test ./...`, `gofmt`, `go vet`
  clean. DB-backed tests need a real Postgres; `make test-db` provisions one
  automatically. A green `go-check` alone does not mean the DB tests ran.
- Web (`web/`): `npm run build`, `npm test`, and `npx tsc -b --noEmit` for
  type-checking (not `tsc --noEmit -p tsconfig.json`, which is a silent no-op
  on this repo's solution-style tsconfig).
- `make config-check` if you touched `deploy/` compose files or `.env.example`.
- `scripts/dev/leak-scan.sh` — see below. CI runs it on every push and PR.

## No infrastructure fingerprints

This repository is mirrored publicly, so tracked content must not carry the
details of anyone's private network: LAN addresses, ssh key names or paths,
absolute `/Users/<name>/…` paths, or personal DNS names. Once published, they
are archived permanently.

Real host addresses, ssh aliases and key paths belong in
`.claude/skills/_shared/hosts.json`, which is **gitignored**;
`hosts.example.json` documents its schema, and every skill and DX script reads
hosts by ROLE (`gpu-test`, `aux-infra`, `deploy-only`) rather than by name. In
prose and examples use a role name, an RFC 5737 documentation address
(`192.0.2.x`, `198.51.100.x`, `203.0.113.x`), or a `<placeholder>`.

`scripts/dev/leak-scan.sh --issues` applies the same patterns to the GitHub
issue tracker — titles, bodies and comments — because an issue is as public and
as permanently archived as a commit, and issues arrive from agents working in
other repos that have no such guard. It runs daily in CI; run it by hand after
filing anything built from real host output.

`scripts/dev/leak-scan.sh` enforces this over git-tracked content. **The
authority is `.github/workflows/leak-scan.yml`**, which runs it on every push and
pull request and is the one gate a contributor branch cannot rewrite. Everything
below is a local convenience that fails earlier; it is not the enforcement.

To get that early failure, COPY the hook into `.git/hooks/`, once per clone:

```
cp scripts/dev/hooks/pre-push .git/hooks/pre-push && chmod +x .git/hooks/pre-push
```

**Do not use `git config core.hooksPath scripts/dev/hooks`.** That path is inside
the worktree, so the hook git executes is whatever the *currently checked-out
branch* contains. Checking out an untrusted contributor branch to review it, and
then pushing anything, would run that branch's `pre-push` — and the
`leak-scan.sh` it invokes — as you, on your machine, with your credentials. A
reviewer's checkout must never be able to change which code their own git runs.
`.git/hooks/` is outside the worktree and a checkout cannot swap it, so the copy
above stays the version you reviewed until you deliberately re-copy it.

Re-copy after pulling a change to `scripts/dev/hooks/pre-push`; read the diff
first, exactly as you would any other code you are about to execute.

If the scan fires, fix the file. Do not relax the script or widen its exclusions.

`make help` lists the full set of developer verbs; a change is not done until
its build and tests pass.

## Frozen interfaces

`protocol/` (a submodule of `quasar-protocol`), `control-api.md`, and `schema.md`
are frozen; client and host code depends on their stability. Don't change them
in a feature PR. If a change seems necessary, open an issue describing why
first.

## Reporting bugs and requesting features

Use the issue templates under `.github/ISSUE_TEMPLATE/`. Security
vulnerabilities should NOT be filed as public issues; see `SECURITY.md`.

## Questions

Open a GitHub issue with the `needs-triage` label, or start a discussion if the
repository has Discussions enabled.
