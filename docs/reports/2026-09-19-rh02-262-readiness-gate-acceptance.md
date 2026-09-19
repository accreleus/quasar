# RH-02 #262: the readiness admission gate and `host_not_ready` — acceptance

| | |
|---|---|
| Ticket | #262, specification #252, protocol amendment 11 (#260), ADR 0005 |
| Source | `feature/rh-02-gate` at `43d4411`, on top of #268 (`b116637`) and #261 (`97fd0d9`, `820373d`). |
| Not on | `develop`, `main`. No tag, no image publication, no Actions build, no shared stack touched. |
| Protocol pin | `128488d` (unchanged by this slice). |
| Migration | `0085_evidence_gated_readiness`. `0084` was the latest on this branch and on `origin/develop` when it was written. Up, down and up again were run on a throwaway Postgres: three columns and the table appear, vanish and reappear. |
| Images | `quasar-control-plane` from `43d4411` through `deploy/build-images.sh control --git-ref 43d4411 --no-prune --no-latest` (contract 23 passed, 0 failed; labels `source.commit=43d4411…`, `schema.version=85`). The agent image is #261's, from `97fd0d9`: this slice changes no agent code. |

## Two rulings

**Where the specification and the contract differ, the contract wins.** #252 says the totals
query includes the gate; the contract says it must not. The gate is not in `totalsQuery`.

**The contract disagrees with itself about one case, and the outcome rule was implemented.**
Its mechanism sentence says `host_not_ready` is "the candidate query finds nothing, and the
same query with only the readiness filter removed would have found a GPU". Its outcome rule
says a ready host that is full or VRAM-vetoed is `capacity_exhausted` "also when some other
host is blocked by readiness, because the caller's remedy is then still to retry". With one
host blocked but roomy and another ready but full, the first sentence alone gives
`host_not_ready` and the second gives `capacity_exhausted`. The ticket's own test list asks
for the second. So `host_not_ready` needs both: the gate excluded a GPU that would otherwise
have been picked, and no GPU the gate leaves eligible could serve the request on totals
(the host pin included, or a launch pinned to a blocked host would be answered by a host it
cannot use). The implementing model reviewed the ruling and had no objection. The mechanism
sentence in `control-api.md` wants one more clause; that is a wording fix for
`quasar-protocol`, not a behaviour change, and is raised on the ticket.

## What changed

- `internal/readiness`: the verdict, a pure function of the stored report and the host's
  override ids. No database, no internal imports. It decodes field by field, so one odd
  entry is inert on its own and cannot hide a well-formed failing check beside it.
- Migration 0085: the three derived columns and `host_readiness_overrides`, which nothing
  writes until #263.
- The write path: `recomputeReadinessVerdict` locks the host row, re-reads the stored report
  and the overrides, and writes the columns, inside the transaction that stores the report
  and again after every GPU-set write. The GPU update touches only rows whose value changes.
- Admission: one clause in the shared renderer beside the free-VRAM veto, in the candidate
  query's `WHERE` and the recheck's projected boolean. It can never evaluate to NULL, so it
  cannot turn fail-open into fail-closed. The homes term is rendered only for a launch that
  mounts a managed home. The veto diagnostic carries the gate too, so a readiness-blocked GPU
  is never reported as VRAM-vetoed.
- `QUASAR_READINESS_STALE_SECS`, default 60, reaches both the session store and the host read
  path. Unparseable or not positive keeps the default with a startup warning; there is no
  off switch. The pre-refactor SQL anchors stay reachable from a zero-value candidacy that
  renders no clause, which `NewStore` cannot produce.
- The host body serves `readiness_gate`, judged against the database clock, and
  `readiness_overrides` as an always-present empty array.
- `503 host_not_ready`, no `Retry-After`, a message that names no check. The web launch toast
  has its own wording and ignores the server message.

Lock order, as argued by the implementing model and checked in review: the write path takes
the host row, then that host's GPU rows, which is the order the capacity transaction always
took, so this slice adds no edge to the lock graph, and it takes no advisory lock. The
recheck's `FOR UPDATE OF h, g` locks in range-table order, GPU then host, which is inverted
against the capacity transaction. That inversion predates this work; the no-op-skipping GPU
update keeps this slice from widening it. Removing it means reordering the recheck's `FROM`,
which the captured anchors pin; left as a follow-up.

## Tests, written first

| Owner | Tests |
|---|---|
| Fable, before any implementation | `internal/readiness/verdict_test.go`: every row of the scope table; 22 inert cases (never reported, `[]`, not an array, not JSON, a failing proxy, every proxy failing, pass, `unknown`, `warn`, `skip`, `provisioning`, an unrecognised status, `FAIL` and `failed`, a non-string status, an unrecognised scope, a non-string scope, `blocks` not an object or empty, a gpu scope with no, a non-integer or a negative index); a malformed neighbour; collapse and partial override; the served entry's wire shape and `[]` never `null`; agent-enforced is never lifted; the override lifecycle (lapse only on pass; held on fail, `unknown`, `warn`, `skip`; inert on a vanished id, `[]`, never reported and a malformed report); determinism; the gate state with the boundary at exactly the window. |
| Fable, before any implementation | `internal/session/readiness_admission_test.go`, DB-backed: a blocked host is skipped and another chosen; the classification matrix (11 fleets: only host blocked, only GPU blocked, every host blocked, the other host offline or draining, pinned to the blocked host, nothing online, a request the blocked host could never serve, blocked and full, blocked beside a full ready host, no readiness involved), each asserting the three refusals stay distinct, no retry and no session row; beside the VRAM veto; a blocked GPU on a two-GPU host; the homes scope; abstention on a stale or absent report and the knob widened and narrowed; nine gate shapes each settling on the first attempt; swap ungated. One of these tests was wrong as first written (it left a session live, so the single-writer home guard answered before placement); the implementing model reported it rather than editing it, and it was corrected in review. |
| Opus | The write path (7 DB tests: one call derives the verdict, absent or malformed changes nothing, only the named GPU, a GPU that appears in a later capacity message, an override lifts on recompute and an agent-enforced check ignores it, proxies and `unknown` never derive, homes has its own column); the host body (never reported, fresh and blocked, stale and still populated, overridden, the list, the configured window); the SQL pins (clause text, identical render in pick and recheck, absent from totals, off renders nothing, argument values and positions, placeholder density over veto, gate, pin, image and managed home); the handler (503, the code, no `Retry-After`, a message with no check detail that does name an administrator). |
| Haiku | The launch toast: exact wording, the server message ignored, no session id. |

## Gates at `43d4411`

Run serially on the development container, nothing else running.

| Gate | Result |
|---|---|
| `make verify` | 432 pass, 0 warn, 0 fail |
| `make test-go` | pass, including the OpenAPI route drift test |
| `make test-db` (fresh ephemeral Postgres, `-p 1`) | pass, 4 of 4 stages, DB tests ran |
| `make test-web` | pass: schema drift, typecheck, 236 test files, production build |
| `make preflight` | exit 0; its warnings are this machine's unconfigured optional overlays and the absent host map |
| leak scan (tree, operator patterns) | clean |

Release preflight: no file under `internal/platform` changed, so its results cannot differ.

## Live evidence: AMD test host, 2026-09-19

Shared-host version preflight: zero containers and zero compose projects, read again
immediately before the first mutation. A disposable stack under its own compose project,
fresh database, removed afterwards with its volumes and its home and template roots. The
rollback target for schema 85 is the stack's removal; nothing else on the host has a schema.
Every session created was stopped and seen `stopped` before teardown.

The fault: the agent container recreated with an empty tmpfs over its Vulkan ICD directory
and `LIBVA_DRIVERS_PATH` pointing nowhere. The GPU is still detected and reported; nothing can
composite and encode on it. Hiding Vulkan alone was not a fault: the agent fell back to VA
and the media probe passed, as it should.

| Step | Observed |
|---|---|
| Healthy | Schema 85. `readiness_gate = {state: active, blocking: []}`, `readiness_overrides = []`. Launch `201`, `running`. |
| Fault injected | `media_probe_gpu1` fails and carries `blocks`; `encoder_codecs`, a proxy, also fails and carries none. The gate lists exactly one entry: `media_probe_gpu1`, scope `gpu`, index 1, `control_plane`, not overridden. |
| Derived columns | `hosts.readiness_block_host = f`, `readiness_block_homes = f`, `gpus.readiness_blocked = t` for index 1. |
| Launch | `503`, code `host_not_ready`, no `Retry-After`, message "the host that would run this needs its administrator's attention; try again once they have looked at it". No session row. |
| Operator log | `admission: the readiness gate excluded a GPU that would otherwise have been picked`, with the GPU, its host and which scope did it. None of that is in the response. |
| Card | `rh02-262/blocked-check-amd.png`: the Fail glyph, "Blocks launches" in the existing `chip chip-danger chip-sm`, title "Blocks launches placed on GPU 1", the observed-at line, the fix. `rh02-262/failing-proxy-check-amd.png`: the failing proxy beside it, no marker. "Needs attention" on the card. |
| Fault cleared (agent recreated) | The probe passes, the gate empties, `readiness_blocked = f`, launch `201`, `running`. |
| Two hosts: a second agent enrolled on the same stack, healthy, the first still faulted | The launch is placed on the second host and runs. |
| The ready host then refuses a second launch (its GPU is VRAM-vetoed) while the other host is still blocked | `503 capacity_exhausted` with `Retry-After: 5`, not `host_not_ready`. The veto diagnostic names only the ready host's GPU. This is the ruled case, live. |

## Live evidence: NVIDIA test host, 2026-09-19

Run after the first version of this record, at `901058f` (the control-plane image rebuilt
from it, contract 23 passed, 0 failed; the agent image still #261's). The host had no NVIDIA
container toolkit, so the NVIDIA compose overlay's `gpus: all` could not start an agent. With
the owner's go-ahead the toolkit was installed from NVIDIA's repository, the engine's runtime
configured with `nvidia-ctk` and the engine restarted; the previous engine configuration is
kept beside it. No kernel module or driver was touched. Preflight before each mutation: zero
containers, zero compose projects. The stack was disposable and removed afterwards; the
toolkit stays. dnf reported skipping per-package signature checks: NVIDIA's repository file
verifies the signed repository metadata instead of each package.

| Step | Observed |
|---|---|
| First boot, no fault injected | Before the agent had provisioned its NVIDIA userspace, `media_probe_gpu0` and `application_gpu_probe_gpu0` failed definitively and the gate blocked GPU 0. About 20 s later the driver volume was provisioned, the agent restarted itself as designed, both probes passed and the block cleared. A launch in that window is refused cleanly instead of dying on the host. |
| Healthy | Launch `201`, `running` on GPU 0 with `vulkanh264enc` on the RTX 5090. |
| Fault: an empty driver volume with provisioning off | Two proxies fail with no `blocks` (`nvidia_egl_vendor_json`, `nvidia_lib32_gl`) and are absent from the gate. Both evidence checks fail and both name GPU 0. `gpus.readiness_blocked = t` for index 0. |
| Launch | `503 host_not_ready`, no `Retry-After`. The host's second GPU row is unblocked but is not the bound render node, so it is not a candidate and readiness is still the sole reason. |
| One of the two checks overridden | GPU 0 stays blocked and the launch is still `503`: a scope stays blocked until every unoverridden failing check naming it is gone. |
| Both overridden | `readiness_blocked = f`; `blocking` still lists both, `overridden: true`; launch `201`, placed on GPU 0, then failed on the host, as the evidence said it would. |
| Fault cleared | Both probes pass and both overrides lapse by themselves: two `host.readiness_override.lapsed` rows, actor null, `info`, after the two `set` rows (`warn`). Launch `201`, `running`. |

The card was not screenshotted here; it is the same vendor-neutral component captured on the
AMD host. One thing seen and not this work's: the NVIDIA host reports a second GPU row for
the machine's other card, whose device node it does not have, because `/sys/class/drm` is not
namespaced. Its binding never matches, so it takes no launches.

## Not verified

- The launch toast on a live stack: the headless Chromium used for screenshots has no H.264
  decoder, so the client disables Play before any launch is sent (captured). The wording is
  unit-tested and the API response was captured live.
- A report going stale while the host stays online was not staged live; it is covered by the
  DB tests, including the boundary and the knob.
- Intel: not validated, nothing claimed.

## The diagnostic-mode refusal and `failure_code`

A launch an agent refuses in diagnostic mode ends `failed` with `failure_code` null.
`control-api.md` defines one `failure_code`, `app_exited_early`, which does not fit, and a
nacked `session_assign` is the control-plane failure path, where the contract says
`failure_code` does not appear at all. Amendment 11 adds none. No code was invented; this is
a contract gap for the owner. With this slice the control plane excludes such a host before
dispatch, because `runtime_endpoint` and `startup_cleanup` carry `blocks` and fail there, so
the launch is refused with `host_not_ready` instead. The null-code path remains only for the
window before the host's first readiness report. #264's harness asserts refusal reasons
through the API and should expect `host_not_ready` after the first report.

## Models

The verdict truth table and the admission matrix, every diff review, the gates, the evidence
and this record: Fable 5.1. The control-plane implementation and its tests: Opus 5. Launch
wording, the configuration row and the changelog: Haiku 4.5. Review edits by Fable: the 503
wording (it said "hardware"; the blocked scope can be homes storage or audio), the error-code
block's alignment, a stronger assertion in the handler test, the corrected homes test, the
glossary entry, and comments cut to the repo's bar.
