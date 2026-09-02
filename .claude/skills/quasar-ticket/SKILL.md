---
name: quasar-ticket
description: Use when implementing a Quasar ticket (P1-* or similar) against the frozen protocol/ contracts — encodes the repo's one-ticket-per-session discipline: read the contract, TDD, build/test in the container, run the acceptance script, fmt/vet/clippy clean, branch off develop and merge back into develop (no PR), stop at the acceptance line, escalate to Opus on ambiguity or a frozen-interface/latency/security touch.
arguments:
  - name: ticket
    description: "ticket to orient on — a GitHub issue number (#204 / 204) or a slug (P5-01); if omitted the skill reads context from the current branch, project-memory current-focus, or CLAUDE.md"
    required: false
---

# Implementing a Quasar ticket

Rigid process. Quasar's architecture is designed for the end state and guarded by
frozen contracts; this skill keeps a ticket on the rails (especially on cheaper
tiers). Read `CLAUDE.md` first — it is the source of truth; this skill is the
procedure that applies it.

## 0. Orient (before touching code)
- Read `CLAUDE.md` (invariants), then the ticket's plan doc: live plans are in
  `docs/design/plans/` (roadmap of record: `2026-07-06-roadmap-spec-v2.html`);
  completed phases and executed plans are archived under `docs/completed/`.
  Note the ticket's scope, dependencies, and **tier**.
- **Given a GitHub issue number** (`#N`/`N`): `gh issue view N --json title,body,labels,milestone`
  — the issue body carries the header (`Tier:` / `Depends on:` / `### Scope` /
  `### Acceptance`) and is the spec when no `docs/` ticket file exists. Milestone
  state: `gh` has **no** `milestone` subcommand — use
  `.claude/skills/ship-milestone/scripts/ms-status "<milestone title>"` (one-shot
  status table) or `gh api repos/accreleus/quasar/milestones`.
- **Not told which ticket?** Read project-memory `current-focus` (in the
  auto-loaded memory index) for the active milestone/ticket before asking the
  user to re-explain it.
- Read the relevant `protocol/*.md` contract(s) the ticket implements against —
  `agent-api.md`, `control-api.md`, `schema.md`, `signaling.md`, `input.md`. The
  contract is the spec; your code conforms to it, not vice-versa.
- **Confirm tier.** If the ticket is marked Opus and you are not Opus, stop and say
  so. **Escalate to Opus** when: the ticket is ambiguous, it touches a **frozen
  interface** (`protocol/` contracts), the **latency path**, or **security /
  concurrency** — or a cheaper model has already failed it twice.

## 1. Frozen contracts are off-limits
The `protocol/` contracts are frozen. Implement against them freely; **changing**
a message, column, type, status code, or endpoint requires **Opus + explicit human
sign-off**. If the ticket seems to need a contract change, **stop and escalate** —
do not quietly edit the contract to make your code compile. (Additive, admin-gated
extensions that change no existing shape are the one documented exception, and
still want sign-off — see `control-api.md §Authorization` as the precedent.)

## 2. Branch
Never work on `main` or directly on `develop`. Branch **off `develop`**:
```
git checkout develop && git pull && git checkout -b <type>/<ticket-slug>
```
(`feat/`, `fix/`, `docs/`, `chore/` — conventional commits.)

## 3. TDD (use the test-driven-development skill)
Write the failing test first, watch it fail, then implement to green. Cover **both**
sides of any gate (allow **and** deny), the happy path, and the contract's stated
error cases (status codes / error codes from `control-api.md`). For DB-backed
control-plane work the tests are integration tests — see §4.

## 4. Build & test — everything runs in a container
This workstation has **no** Rust/GStreamer/Wayland and **no** Go. Locally, prefer
`make <target>`; to run on a real host use the **quasar-host** skill
(`qhost test <target>`), which wraps the same `scripts/dev/dev.sh` targets:

- **Rust** (`node-agent/`): `scripts/dev/dev.sh build [dir]`,
  `scripts/dev/dev.sh test [dir]`, `scripts/dev/dev.sh check [dir]` (fmt --check + clippy
  -D warnings). Done = `cargo fmt` + `cargo clippy -- -D warnings` clean.
- **Go** (`control-plane/`): `scripts/dev/dev.sh go-check` is build + vet + test — **but
  it wires no database, so the DB integration tests SILENTLY SKIP.** A green
  `go-check` does NOT mean the DB tests ran.
  - **`scripts/dev/dev.sh go-test-db`** runs the same with Postgres attached so the
    `internal/{auth,crud,session,signal}` tests actually execute (pass extra args
    through to `go test`, e.g. `go-test-db -run TestFoo -v`). **A control-plane
    ticket touching the DB is not DONE until `go-test-db` is green**, fmt + vet
    clean — not just `go-check`.
- **Acceptance scripts**: `scripts/dev/dev.sh run <name>` runs `scripts/harness/run-<name>.sh`
  inside the container (e.g. `run p5-home`, `run st-trace`). The ticket's
  acceptance script is the definition of done for the runtime behaviour.

Watch the **GStreamer-rs / WebRTC / VAAPI gotchas in `CLAUDE.md`** — gst-launch is
more forgiving than the Rust GObject API, and several of them (enum-as-string
props, stale VA registry, softpipe hiding the encoder, mDNS/avahi for Chrome ICE)
have bitten real tickets.

## 5. Stop at the acceptance line
A ticket is DONE only when its build + tests pass **and** its stated acceptance
criteria are met. Do exactly the ticket — do not gold-plate, do not start the next
ticket, do not refactor unrelated code. When the acceptance line is reached: stop.

## 6. Finish: merge back into `develop` — no PR
Commit (conventional-commits; co-author trailer per `CLAUDE.md`), push, then land
the branch **into `develop` directly**:
```
git checkout develop && git pull && git merge <type>/<ticket-slug>
```
Feature-branch → `develop` is **self-serve: no PR, no review gate**. Then **stop**
— report outcomes faithfully (if tests skipped or failed, say so with the output).
Summarize what was implemented, how it was verified (exact commands + results),
and any decisions/escalations, using the structure of `_shared/pr-body.tmpl` as
the checklist for that summary.

**`develop` → `main` is the only sign-off gate**, and it is never yours to take:
`main` is production and merging into it requires explicit human sign-off. If a
ticket genuinely needs to reach `main`, that promotion happens through the
ship-milestone flow with a PR body from `_shared/pr-body.tmpl` — and still stops
for sign-off.

## Quick reference
| need | command |
|---|---|
| Rust build / test / lint | `scripts/dev/dev.sh build` · `… test` · `… check` |
| Go build+vet+test (no DB — tests skip) | `scripts/dev/dev.sh go-check` |
| Go build+vet+test **with** Postgres | `scripts/dev/dev.sh go-test-db [go-test args]` |
| run an acceptance/demo script | `scripts/dev/dev.sh run <name>` |
| interactive container shell | `scripts/dev/dev.sh shell` |
