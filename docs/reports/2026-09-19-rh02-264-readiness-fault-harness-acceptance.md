# RH-02 #264: fault-injection acceptance harness for host readiness — acceptance

| | |
|---|---|
| Ticket | #264, parent #211, specification #252, protocol amendment 11 (#260), ADR 0005 |
| Source | `feature/rh-02-probe-first` from `bc3ffe5`: matrix `61a4cc6`, fixture `a4c5f0b`, harness and guards `651217e`, review fixes `a9d02ac`, `75d17d9`, `5ae325a`. Every report below is from the script at `5ae325a`. |
| Not on | `develop`, `main`. No tag, no image publication, no Actions build, no shared stack touched. No production behaviour change, no contract change, no styling change: nothing under `node-agent/`, `control-plane/`, `web/` or `protocol/` is modified. |
| Protocol pin | `7ecc596` (amendment 11), unchanged. |
| Images | Current: control plane built from `901058f`, agent from `97fd0d9` (the last commits to touch each tree; `git diff` of `node-agent/`, `control-plane/` and `deploy/` from those to the branch head is empty). Baseline: both from `b134085` through `deploy/build-images.sh all --git-ref b134085 --no-prune --no-latest`, image contract passed for all four artefacts. Nothing was published; the test hosts pulled from the lab's local registry by content-addressed tag. |
| Schema | 85 on the current stack, 84 on the baseline stack. Each stack had its own disposable Postgres; no existing database was touched, so no rollback target was needed. |

## What was built

- `scripts/harness/run-readiness-faults.sh`. It runs on a docker host, creates a disposable
  stack it owns (compose project and label `quasar.harness.owner=<run id>`, every host path
  under `/var/lib/<run id>`, every host row named `<run id>-*`), injects each fault, asserts
  through the control-plane API, clears the fault, asserts recovery, tears down, and proves
  the teardown. The scenario matrix, written before the code, is
  `docs/superpowers/plans/2026-09-19-rh02-264-harness-matrix.md`.
- `scripts/harness/readiness-fixture/`, a test-only Go module with two subcommands. `relay`
  sits between the real agent and the control plane for the whole run, forwards every
  WebSocket frame and every plain HTTP call untouched, and can rewrite the `readiness` array
  of a `capacity` report on the wire. `host` is a scripted agent for the two-host and
  classification cases.
- Five `readiness-faults:*` guards in `make verify`.
- The catalogue rows in `docs/developer-tooling.md` and the changelog line.

Results are `pass`, `fail` or `unperformed`. A fault that cannot be injected on a host is
`unperformed`, which is never a pass: the run's verdict is then `incomplete` and its exit
code is 4. Under `scripts/dev/dev.sh run readiness-faults`, which has no docker socket,
every row is `unperformed` and the exit code is 3.

## Results

Each cell counts assertions. The reports are in `docs/reports/rh02-264/`: sanitized JSON plus
a Markdown summary per run, with roles only.

| # | What is asserted | AMD test host | NVIDIA test host | Baseline `b134085` (AMD test host) |
|---|---|---|---|---|
| 1a | Runtime stopped: both checks fail with fix text, `blocks {host, agent}`, listed in `blocking`; recover | pass (10) | pass (10) | **fail** (1 of 2) |
| 1b | Agent health 503 under the fault, 200 after | pass (2) | pass (2) | **fail** (1 of 1) |
| 1c | Refused by the control plane as `host_not_ready`, no `Retry-After`; recovery launch placed on the recovered host and runs | pass (5) | unperformed | **fail** (1 of 1) |
| 1d | Override `PUT` on both agent-enforced checks is `409` | pass (2) | pass (2) | **fail** (1 of 1) |
| 1e | Agent container never restarted; the pid that reported the fault is the pid that resumed | pass (2) | pass (2) | **fail** (1 of 1) |
| 2a | Homes root unwritable: scope `homes`; managed-home launch refused, homeless launch runs; recover | pass (5) | pass (5) | **fail** (5 of 5) |
| 2b | Homes storage exhausted: same, and the managed-home launch runs again after | pass (5) | pass (5) | **fail** (5 of 5) |
| 3 | Input withheld: `input_probe` fails, scope `host`; launch refused; recover | pass (4) | pass (4) | **fail** (4 of 4) |
| 4a | AMD, no Vulkan and no VA driver: GPU probes fail with scope `gpu` and the right index; refused; recover | pass (5) | unperformed | **fail** (6 of 6) |
| 4a' | AMD, Vulkan alone removed: falls back to VA, nothing blocks, launch runs | pass (3) | unperformed | **fail** (3 of 3) |
| 4b | NVIDIA, empty driver volume, provisioning off: as 4a | unperformed | pass (6) | unperformed |
| 4c | Under the GPU fault the launch is placed on the other host | pass (2) | pass (2) | **fail** (1 of 2) |
| 5a | Render node unopenable by the app identity: only the proxy check fails, never in `blocking`, launch runs | pass (3) | pass (3) | **fail** (2 of 3) |
| 5b | Audio sidecar image missing: `unknown`, never in `blocking`, launch is placed; recover | pass (4) | pass (4) | **fail** (1 of 1) |
| 6a | A ready host that is full gives `capacity_exhausted` while another host is blocked | pass (2) | pass (2) | **fail** (1 of 1) |
| 6b | Nothing online gives `no_host_available` | pass (1) | pass (1) | pass (1) |
| 6c | Readiness as the sole reason: `503 host_not_ready`, no `Retry-After`, message names no check | pass (3) | pass (3) | **fail** (1 of 1) |
| 6d | A non-admin sees no check detail; host reads are `403` | pass (3) | pass (3) | **fail** (1 of 1) |
| 7a | Override `PUT` is `200`, idempotent, audited `warn` with `check_id` and `node_name` | pass (5) | pass (5) | **fail** (5 of 5) |
| 7b | The check stays listed, `overridden: true`, still `fail` | pass (1) | pass (1) | **fail** (1 of 1) |
| 7c | **The launch succeeds** under the override: a real session reaches `running` | pass (1) | pass (1) | **fail** (1 of 1) |
| 7d | The check passes and the override lapses; `.lapsed` audit row, actor null | pass (3) | pass (3) | **fail** (1 of 1) |
| 7e | `DELETE` is `204`, idempotent, one `.cleared` row; the launch is refused again | pass (5) | pass (5) | **fail** (5 of 5) |
| 7f | A renamed check makes the override `inert`; the new id blocks; full clear runs | pass (6) | pass (6) | **fail** (3 of 3) |
| 7g | A non-admin `PUT` on an unknown host is `403`, not `404` | pass (1) | pass (1) | **fail** (1 of 1) |
| 7h | A malformed or over-long `check_id` is `400 validation_failed` | pass (2) | pass (2) | **fail** (2 of 2) |
| 8 | No check id, summary or remediation claims browser reachability | pass (1) | pass (1) | pass (1) |
| 9 | Host rows deleted `204`; no labelled asset, no new volume, no session container, no owned path left | pass (4) | pass (4) | pass (4) |
| | **Totals** (every assertion, setup included) | 103 pass, 0 fail, 1 unperformed | 96 pass, 0 fail, 4 unperformed | 19 pass, 54 fail, 1 unperformed |

Every row passes on at least one host, and nothing fails on either. What is `unperformed`:

- **4b on the AMD test host, 4a and 4a′ on the NVIDIA test host.** The other vendor's fault.
- **1c on the NVIDIA test host.** The refusal is `host_not_ready` only when readiness is the
  sole reason, so the harness first proves that the nested host can serve a launch. On that
  host it cannot: the nested engine has no NVIDIA container runtime, and the lab's `/sys` is
  not namespaced, so the nested agent detects a GPU whose device node it does not have. The
  control plane rightly answers `no_host_available`. 1a, 1b, 1d and 1e ran and passed there.
  1c passed in full on the AMD test host, including the recovery launch landing on the
  recovered host and reaching `running`.

Intel: not validated, nothing claimed.

## The baseline fails

The same script against images from `b134085`, the pre-RH-02 baseline, records 54 failures.
Every row fails except three that were already true then and one that does not depend on
the gate:

- **6b**: nothing online was `no_host_available` before RH-02.
- **8**: the baseline's check text makes no browser claim either. The misleading word was on
  the console's card, which this harness does not read.
- **9**: cleanup.
- Within **5a**, one assertion of three holds: the proxy check existed and carried no
  `blocks`. The row fails.

One caveat. On this host the baseline refuses even a plain launch with `no_host_available`,
because the encoder-placement fix for a host whose only GPU is not at index 0 (#268) is
itself part of RH-02. The baseline's launch failures therefore have that second cause. The
clean must-fail evidence is the rest: no `readiness_gate` on the host body, no evidence check
ever reported (`runtime_endpoint`, `homes_root_writable`, `input_probe`, `media_probe_gpu<N>`
are absent), no `host_not_ready`, and `404` on the override routes.

An earlier baseline run is what exposed several assertions that passed with no gate at all
("the override is gone" is true of an override that never existed; a host body with no
`readiness_gate` counted as zero blocking entries). They are fixed in `a9d02ac` and
`75d17d9`; see "Review" below.

## The synthetic check cannot reach production

Scenario 7 needs a failing evidence check on a host that is really healthy, so that the
launch made under the override can succeed. That check is produced **outside the agent**, by
the relay rewriting a report on the wire. The shipped agent gains no code, no feature flag
and no env knob, and the real session in 7c is a real session on the real GPU.

- `readiness-faults:fixture-unreachable`: no file under `node-agent/`, `control-plane/`,
  `web/` or `deploy/` mentions `readiness-fixture`, `harness_synthetic_` or `RH02_FIXTURE_`.
  Negative-tested: a planted reference in `node-agent/tests/` is caught, and removed again.
- `readiness-faults:no-image-copies-scripts`: no `deploy/Dockerfile*` has a `COPY` or `ADD`
  whose source can include `scripts/` or the whole build context. Negative-tested against a
  Dockerfile with a multi-line `COPY … scripts/harness` and an `ADD .`; `--from=` is ignored.
- `readiness-faults:separate-module`: the fixture is `module quasar-readiness-fixture`, so
  neither product build can import it.
- At run time the harness asserts that the agent image under test carries no
  `readiness-fixture` path and that the agent binary has no `harness_synthetic_` bytes. Both
  passed on both hosts.
- The relay refuses any rule whose check id does not start with `harness_synthetic_`.

## Ownership and what remains on the hosts

Before each run: both test hosts had no container, no compose project and no volume, so
there was no deployed stack, no schema and no session to record or protect, and no shared
stack was involved. The count was rechecked immediately before every run.

- The nested engine is on its own bridge network. Nothing uses `--network host` except the
  relay, the scripted hosts and the agent, as the shipped compose file does.
- The homes faults act on a 256 MB tmpfs the harness mounts under its own root. The host's
  `/dev/dri` nodes are never modified: 5a binds a harness-owned device node over the render
  node path inside the fixture agent container only.
- Row 9 passed in every run, the baseline and two runs interrupted with `SIGTERM` included:
  every `<run id>-*` host row deleted through `DELETE /v1/hosts/{id}` with `204`, no labelled
  container, volume or network, no volume that was not there at preflight, no
  `quasar-sess-*`, `quasar-pulse-*` or `quasar-probe-*` container, no owned path or mount.
- One leak was found and fixed during development: `docker:dind` declares an anonymous
  volume, which carries no label. Two such volumes were left on the AMD test host by an early
  run, found by hand and removed. The before-and-after volume comparison in row 9 exists
  because of it.
- **Remaining on each host:** pulled images only (the two current and two baseline Quasar
  images, `postgres:16-alpine`, `fedora:43`, `docker:dind`, `golang:1.25`). No container,
  volume, network, path or file of this work. `/dev/input` existed on both hosts before this
  work and was left alone.

## Gates

On the development container, serially, on the branch:

| Gate | Result |
|---|---|
| `make verify` | `status=ok pass=437 warn=0 fail=0`, the five `readiness-faults:*` guards included. Run again after the guards changed: the same. |
| `make test-go` | pass, Go unit and OpenAPI drift tests |
| `make test-db` | pass, fresh ephemeral Postgres, `-p 1` |
| `make preflight` | exit 0. `verify` ok; `doctor` and `config-check` report `degraded` on four advisories about this machine (no remote role configured in the operator-local host file, three optional compose overlays without their variables), none from this change. |
| Fixture | `gofmt`, `go vet` and `go test` clean natively and in a `golang:1.25` container |
| `make test-rust`, `make test-web` | not run: neither the agent crate nor `web/` changed |
| Leak scan with the operator's patterns | clean on the tree. It caught two of my own mistakes before anything was pushed: a role label and a set of file names that contained a lab ssh alias. |

None of the known flakes (#269, #270, #197) fired.

## Review

Two read-only reviews by Opus 5 of the diff from `bc3ffe5`, one against the repository's
standards and one row by row against the matrix, the ticket and the contract.

- **Standards.** No defect in the `set -euo pipefail` handling, the cleanup trap, leaks or
  the relay's write concurrency. Fixed: private review-checklist tags in about 25 comments;
  `--help` printing into the code; `--only=7` selecting nothing (a bare scenario number now
  selects its rows); a dead helper and an unused parameter. Left as judgement calls:
  `json_get` beside `jq`, and the five `wait_*` loops that could sit on `poll_until`.
- **Spec.** Sixteen findings, the serious ones false passes: 5b went on asserting after its
  fault was not observed; 2b's recovery accepted an absent check and never relaunched the
  managed-home app; 6d and 7e accepted any `503`; the check-id scans of 6c, 6d and 8 passed on
  an empty collection; an absent GPU probe check was skipped in silence. All fixed, along
  with `node_name` in 7a's audit detail, the first launch landing on host A in 6a, and the
  unreachability guard scanning whole trees.

Found by my own review and the live runs before that: a silent exit on an empty engine
(`grep` under `pipefail`), an image-clean check that passed when `strings` was missing, a
freshness wait the outgoing agent's last report could satisfy, a wrong audio sidecar image
that made 5b hollow, a relay that did not pass the agent's plain HTTP calls through, and a
nested agent left on software rendering. The last one first looked like a product defect
(`no_host_available` where `host_not_ready` was expected) and was the fixture's fault.

**No production defect was found.**

## Models

The scenario matrix, the fixture design and its unreachability proof, the review of every
diff, every gate and hardware run, the fixes listed above, the commits and this record:
Fable 5.1. The fixture and the first two passes of the harness: Sonnet 5. The catalogue rows,
the changelog line and the comment cleanup: Haiku 4.5. The two reviews: Opus 5. No model was
substituted. Nothing needed escalating to Opus for the fixture, because it touches nothing
inside the agent.

## Not verified

- Intel.
- 1c on an NVIDIA host, for the reason above. It needs a host whose nested engine has the
  vendor container runtime.
- The harness against a TLS-on stack: it runs the disposable stack with `QUASAR_TLS=off`.
- `--allow-cohabit`, the mode for an engine that already runs another Quasar stack. It was
  never needed and never exercised.
- A host with more than one GPU: 4c's "other host" is the scripted host.
