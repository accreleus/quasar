# RH-02 #264 — readiness fault-injection harness: scenario matrix and fixture design

Written before any harness code. The harness (`scripts/harness/run-readiness-faults.sh`)
and its fixture (`scripts/harness/readiness-fixture/`) are implemented against this table.
Contract of record: `protocol/control-api.md` "Evidence-gated readiness", `protocol/agent-api.md`
`readiness`, ADR 0005. Where this file and the contract differ, the contract wins.

## Result vocabulary

Every assertion ends in exactly one of:

- `pass` — the fault was injected, observed, and the assertion held.
- `fail` — the assertion did not hold.
- `unperformed` — the fault could not be injected or observed on this host (missing device,
  vendor mismatch, fixture could not start). **Never counted as a pass.** The run exits
  non-zero when any assertion is `fail`; `unperformed` is reported in its own bucket and makes
  the run's overall verdict `incomplete`, not `pass`.

`lib/harness.sh` has pass/fail/skip. The harness adds `unperformed` locally as a thin wrapper
over `skip` that prefixes the message with `UNPERFORMED` and counts separately in the report's
`notes`; `lib/harness.sh` itself is not changed.

## Fixtures (all disposable, all owned)

Run id `RID` = `rh02h-<8 hex>`. Every container, volume and network carries the label
`quasar.harness.owner=<RID>`; every host path lives under one root `/var/lib/<RID>`; every
host row's `node_name` starts with `<RID>-`. Cleanup is verified by those three keys.

| Fixture | What it is | Used by |
| --- | --- | --- |
| **stack** | compose project `<RID>`: Postgres, control plane, the real node agent, from `deploy/docker-compose.yml` plus generated overrides; images named by `QUASAR_CONTROL_IMAGE` / `QUASAR_AGENT_IMAGE` (caller supplies content-addressed tags) | 2, 3, 4, 5, 8 and the baseline launch |
| **relay** | `readiness-fixture relay`: a WebSocket relay between the real agent and the control plane. It forwards every frame verbatim in both directions, except that it may rewrite the `readiness` array of an upstream `capacity` message according to its current rule (append a synthetic check, set its status, rename it). Rules are changed over a loopback HTTP control port. | 7 |
| **scripted host** | `readiness-fixture host`: a scripted agent. Enrolls, registers, reports one GPU with a given slot count and a given `readiness` array, heartbeats, acks `session_assign`/`session_start`/`session_stop`, and reports `starting → running` / `stopping → stopped`. No media. | 4 (the other host), 6 |
| **nested engine** | `docker:dind` on the project's bridge network (never `--network host`), `live-restore` on, dockerd not PID 1, with a second real agent inside it enrolled as `<RID>-nested` | 1 |

The relay and the scripted host are one Go program in its own module under
`scripts/harness/readiness-fixture/`. It runs in a `golang` container, as `run-apitest.sh`
runs the apitest module.

## Why the synthetic check cannot reach production

The synthetic failing check in scenario 7 is produced **outside the agent**, by the relay
rewriting a report on the wire. The shipped agent gains no code, no feature flag and no env
knob. Proof, enforced on every `make verify`:

1. The `readiness-faults:*` guards in `scripts/dx/tests/run.sh`: no file under
   `node-agent/`, `control-plane/`, `web/` or `deploy/` mentions `readiness-fixture`, the
   synthetic check id prefix `harness_synthetic_`, or the fixture's env names
   (`RH02_FIXTURE_*`); and no Dockerfile under `deploy/` has a `COPY`/`ADD` whose source can
   include `scripts/` or the whole build context.
2. The fixture is a separate Go module (`module quasar-readiness-fixture`); the control-plane
   and agent builds cannot import it.
3. The harness asserts at run time that the agent image under test contains no
   `readiness-fixture` path and that the agent binary has no `harness_synthetic_` bytes
   (`grep -a`, so the check does not depend on `strings` being in the image; an unreadable
   binary is `unperformed`).

## Scenario matrix

Common steps. *Fresh report*: poll `GET /v1/hosts/{id}` (bound 120 s, 2 s interval) until
`readiness_gate.state == "active"` and the expected check state is visible; assertions that
need the gate run within the same poll iteration's freshness. *Launch*: `POST /v1/sessions`
with the fixture app; a placed session is polled to `running` (bound 90 s) and then stopped
and polled to `stopped` before the next step. *Baseline*: before any fault, wait for
`blocking` to be empty (bound 180 s — first-boot NVIDIA provisions for ~20 s), then one
launch must reach `running`. The baseline also requires `input_probe` and `audio_probe` to
have run and passed: a healthy pre-state is what gives each later fault its meaning. After
any agent recreate or restart the harness waits for the new process to register before it
judges a report fresh, so the outgoing agent's last report cannot satisfy a wait.

Two fixture apps: `app-nohome` (`managed_home` false) and `app-home` (`managed_home` true).
Both `fedora:43` + `sleep`, `default_vram_mb` 256.

| # | Scenario | Inject | Assert under fault | Clear | Assert recovery |
| --- | --- | --- | --- | --- | --- |
| 1a | Runtime stopped | stop dockerd inside the nested engine (live-restore keeps containers), then kill the agent **process**: its container's PID 1 is a respawn loop, and the agent enters diagnostic mode at process start (the #256 recipe). The engine socket reaches the agent through its directory, because a restarted dockerd recreates the socket | nested host `status == online`; `runtime_endpoint` and `startup_cleanup` are `fail`, each with non-empty `remediation`; each has `blocks == {scope: host, enforced_by: agent}`; both listed in `readiness_gate.blocking` with `enforced_by: agent` | start dockerd | both checks leave `blocking`; `runtime_endpoint` `pass` |
| 1b | Health | same fault | agent health endpoint answers 503 | — | 200 |
| 1c | Refusal | same fault, real agent stopped so the nested host is the only host. **Before** the fault a launch on the nested host must reach `running`; where it cannot (a nested engine with no vendor container runtime), the control plane rightly answers `no_host_available` and 1c is `unperformed` | launch → `503`, `error.code == host_not_ready`, no `Retry-After` | — | launch is placed on the nested host and reaches `running` |
| 1d | No override | same fault | `PUT …/readiness-overrides/runtime_endpoint` → `409`; same for `startup_cleanup` | — | — |
| 1e | No restart | across 1a–1c | agent container `StartedAt` and `RestartCount` unchanged across fault and recovery; the agent pid that reported the fault is the pid that resumed | — | — |
| 2a | Homes root unwritable | remount the stack's homes root read-only (it is a harness-owned tmpfs under `/var/lib/<RID>`) | `homes_root_writable` `fail`, `blocks.scope == homes`; `app-home` launch → `503 host_not_ready`; `app-nohome` launch reaches `running` | remount rw | check `pass`; `app-home` launch reaches `running` |
| 2b | Homes storage exhausted | fill the same tmpfs to 0 bytes free | `homes_free_space` `fail`, `blocks.scope == homes`; `app-home` refused `host_not_ready`; `app-nohome` runs | delete the fill file | check not `fail`; `app-home` runs |
| 3 | Input unavailable | recreate the agent without `/dev/uinput` | `input_probe` `fail`, `blocks.scope == host`; launch → `503 host_not_ready` | recreate with the device | `input_probe` `pass`; launch runs |
| 4a | GPU path broken (AMD) | recreate the agent with an empty tmpfs over the Vulkan ICD dir **and** `LIBVA_DRIVERS_PATH=/nonexistent` | `media_probe_gpu<N>` and `application_gpu_probe_gpu<N>`: each that is `fail` has `blocks == {scope: gpu, gpu_index: N}`; `media_probe_gpu<N>` must be `fail`; `N` equals the GPU's reported index; launch → `host_not_ready` | recreate clean | probes `pass`; launch runs |
| 4a′ | Vulkan alone is not a fault (AMD) | tmpfs over the ICD dir only | `media_probe_gpu<N>` `pass`; nothing in `blocking`; launch runs | recreate clean | — |
| 4b | GPU path broken (NVIDIA) | recreate the agent on a fresh empty driver volume with `QUASAR_NVIDIA_DRIVER_VOLUME=0`, `QUASAR_CUDA_RUNTIME=0` | as 4a | recreate on the original volume | as 4a |
| 4c | Placed elsewhere | 4a/4b fault, plus a ready scripted host with a free slot | launch → `201`, `session.host_id` is the scripted host | — | — |
| 5a | Proxy never blocks | the host's `/dev/dri` nodes are a shared device and are never chmod'ed: the harness creates its own device node (same major:minor, mode 0600 root) under `/var/lib/<RID>` and binds it over the render node path in the fixture agent container only | `dri_node_app_access` `fail` and carries no `blocks`; it is absent from `blocking`; launch reaches `running` | recreate clean | — |
| 5b | Indeterminate never blocks | first require `audio_probe` to be `pass` (an `unknown` that was already there proves nothing), then recreate the agent with `QUASAR_PULSE_IMAGE` naming an image that does not exist | `audio_probe` `unknown`; absent from `blocking`; launch is **placed** (`201`; the session itself may later fail for lack of audio — that is not asserted) | recreate clean | `audio_probe` `pass` |
| 6a | Full beats blocked | real agent stopped. Scripted host A: 1 slot, ready. Scripted host B: failing `host`-scope evidence check. Fill A with one launch | second launch → `503`, `error.code == capacity_exhausted` | stop the session | — |
| 6b | Nothing online | every agent and scripted host stopped | launch → `503 no_host_available` | — | — |
| 6c | Sole reason | only scripted host B (blocked) online | launch → `503`, `error.code == host_not_ready`, no `Retry-After` header, `error.message` contains no check id reported by any host (checked against the full id list) | — | — |
| 6d | Non-admin sees no detail | a registered non-admin user | their `host_not_ready` body names no check; `GET /v1/hosts` and `GET /v1/hosts/{id}` with their token are `403` | — | — |
| 7a | Override set | relay rule: append `harness_synthetic_gate` `fail`, `blocks {scope: host, enforced_by: control_plane}` to the real agent's report; launch first → `host_not_ready` | `PUT` → `200` with the override object; repeat → `200` and no second audit row; audit has `host.readiness_override.set` severity `warn` with `check_id` in detail | — | — |
| 7b | Still visible | — | `blocking` still lists the check, `overridden == true`; `readiness[]` still has it `fail` | — | — |
| 7c | **Launch succeeds** | — | launch → `201`, reaches `running` on the real agent (a real session on the real GPU), then stopped cleanly | — | — |
| 7d | Lapse | relay rule: synthetic check `pass` | override gone from `readiness_overrides`; audit has `.lapsed` with actor null | — | — |
| 7e | Clear | rule back to `fail`, `PUT`, then `DELETE` | `204`; repeat `DELETE` → `204`; audit has `.cleared`; launch refused again | — | — |
| 7f | Rename is inert | `PUT` again, then relay rule renames the check to `harness_synthetic_gate_v2` | override listed `inert == true`; `blocking` lists the new id `overridden == false`; launch → `host_not_ready` | rule off; `DELETE` override | `blocking` empty; launch runs |
| 7g | Authorization first | non-admin token, `PUT` on a **random unknown host id** | `403` (not `404`) | — | — |
| 7h | Validation | admin `PUT` with `check_id` `bad%20id` and one of 129 bytes | `400`, `error.code == validation_failed` | — | — |
| 8 | Host-local honesty | none — read every check every host reported during the run, including each fault state, where the fix text is on the card | no `id`, `summary` or `remediation` matches (case-insensitive) `browser`, `internet`, `external(ly)`, `public(ly)`, `port forward`, `wan`, `remote client`, `reachable from` or `reachability`, unless the sentence is a negation on the harness's allow-list. One exemption, by id only: `media_reachability`, which the RH-02 spec keeps as an id while rewording the check as the host's inbound firewall posture; its summary and remediation are scanned like any other. The bare word "unreachable" is not a claim (it describes the runtime socket). Fewer than 10 collected ids is `unperformed`, never a pass | — | — |
| 9 | Ownership cleanup | after teardown | every `<RID>-*` host row deleted through `DELETE /v1/hosts/{id}` (`204`) with its agent stopped first; zero containers, volumes, networks with the owner label; no volume at all that was not there at preflight (an anonymous volume carries no label); zero `quasar-sess-*` / `quasar-pulse-*` / `quasar-probe-*` containers; `/var/lib/<RID>` gone; and no entry under the agent's fixed `/run/quasar-agent` runtime path that was not there at a root-read preflight snapshot, after removing entries this run can attribute to itself (any new entry when the harness's stack was the engine's only Quasar stack; under `--allow-cohabit`, only `udev-<session id>` entries whose session id is one this run created) — `unperformed`, never a pass, if the path could not be read at preflight or at teardown | — | — |

## Baseline proof

Against images built from `b134085` the control plane has no gate: no `readiness_gate` on the
host body, no `host_not_ready`, no override routes. Each scenario's first gate-dependent
assertion therefore fails (1c, 2a, 2b, 3, 4a/4b, 6a is the exception noted below, 6c, 7a, and
5/8 fail at "gate is `active`" because the field is absent). The harness does not special-case
the baseline: it runs the same assertions and the report shows them failing. Scenario 6a/6b's
refusal codes predate RH-02; their *setup* still asserts `readiness_gate.state == active` on
the scripted hosts first, which is what fails on the baseline.

## Report

`deploy/results/readiness-faults-<ts>.json` (the `lib/harness.sh` report) plus
`readiness-faults-<ts>.md`, a human summary: one row per matrix line with
pass/fail/unperformed, host **role** (`gpu-test-amd` / `gpu-test-nvidia` style role names
given by `--role=`), GPU vendor and index, image source commits, schema version. Sanitizer:
before writing, every string is passed through a filter that replaces the machine's hostname,
any IPv4/IPv6 literal, the invoking username, any `host:port/` registry prefix and the
`RID`-independent absolute home path with role placeholders; the harness then greps its own
output for those shapes and fails the run if any survive.
