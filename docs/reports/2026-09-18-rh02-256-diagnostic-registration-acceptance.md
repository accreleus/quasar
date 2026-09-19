# RH-02 #256: diagnostic registration — acceptance and handoff

| | |
|---|---|
| Ticket | #256, specification #252 "Diagnostic registration", ADR 0005 |
| Source | `initiative/resilient-host-architecture` and `feature/rh-02-probe-first`, which are the same commit. The first two runs below used the image built from `e7da24c`; the managed-home run and the gates used `4ca2433`. |
| Not on | `develop`, `main`. No tag, no image publication, no Actions build, no deployed stack touched. |
| Images | `quasar-node-agent` from `e7da24c` (`deploy/build-images.sh runtime --toolchain registry --no-prune`, contract 148 pass / 0 fail / 2 GPU-gated skips) and `quasar-control-plane` from `dd2c97b` (contract 23 / 0; no control-plane source changed). Local to the development host; rebuild elsewhere. |

## What changed

An unresolved startup cleanup, or a container runtime that does not answer, used to end the
agent process. It now enters diagnostic mode (`node-agent/src/diagnostic.rs`):

- The decision is pure. `CleanupAttempt::fault` needs the engine to answer discovery and the
  retirement of the previous agent's applications to finish in the same pass. An unreachable
  engine with an empty journal is still a fault. `Startup` resumes at most once per process.
- `agent::run` holds the process in `run_diagnostic_mode` until the resume. Homes GC, both
  provisioners, the `ImageManager`, `HostSessions` (and with it the probe orchestrator) and
  every per-connection task are constructed after that call, so they are withheld by
  construction; the three spawns are also gated on `diagnostic::may_start`.
- The diagnostic connection is its own minimal loop, not `connect_and_run`: it registers with
  the image list read from the state file, reports capacity with the retained `startup_cleanup`
  check, heartbeats, and nacks `session_assign` and every other command that carries an ack
  id. `restart` still works. It never initialises GStreamer, so the driver volume is still
  adopted before anything touches EGL once startup resumes.
- The retry is a separate task that reads no connection: 5 s doubling to 60 s, replaying only
  journalled obligations through the runtime API.
- `/health` answers 503 `{"status":"diagnostic","ready":false,...}`. `deploy/lib/agent-readiness.sh`
  classifies `boot-diagnostic-mode` as cause `diagnostic`, state FAILED, severity fail; the
  normal startup's later verdict supersedes it after a resume.
- `diagnostic::host_probes_withheld()` is the flag for the probe orchestrator's owner. The
  orchestrator is not constructed in diagnostic mode, so nothing reads it yet.

## Gates

Run serially on the development container at `4ca2433` (the merge of this work into
`feature/rh-02-probe-first`, plus the two fixes below), 5-minute load below 4 throughout,
per-worktree `CARGO_TARGET_DIR`.

| Gate | Result |
|---|---|
| `make verify` | 432 pass, 0 warn, 0 fail |
| `make test-rust` (fmt, clippy `-D warnings`, `cargo test --all-targets`) | 1704 passed, 0 failed, 9 ignored |
| `make test-go` | pass, 41 packages, including `TestEnrollHostComposeMatchesBase` |
| `make test-web` | pass: schema drift, typecheck, 236 test files, production build |

Two fixes made the integration branch green:

- `make test-go` failed on `TestEnrollHostComposeMatchesBase`: #253 added
  `QUASAR_HOMES_FREE_SPACE_FLOOR_GIB` to `deploy/docker-compose.yml` and not to the copy
  `deploy/enroll-host.sh` prints. The line is added. A key-by-key comparison of the whole
  agent service, the updater service and the NVIDIA overlay found no other gap, and #256 added
  nothing to any compose file.
- The 15 earlier `make verify` failures were the development container, not the code: 14 bench
  checks shell into the git-excluded `quasar-session` skill, which a fresh machine lacks, and
  `release:signature-contract` needs `openssl`. With both provided locally, untracked, every
  check passes. No assertion was changed.

Two lock-lease tests each failed once in earlier runs and passed on every rerun:
`artifact::tests::abandoned_kernel_lock_is_recovered_without_waiting_for_age` and
`container_ownership::tests::ownership_survives_restart_and_distinct_agents_are_isolated`
(`EAGAIN` on a lease in a fresh temp dir). Neither file is touched here, and the cause is not
established. The suite forks a child that deliberately segfaults, which is a plausible holder
of an inherited lock fd; it also leaves a root-owned `node-agent/core` behind.

## Real-Docker evidence

A disposable stack on the development host, which runs nothing else but a local registry:
its own bridge network, Postgres, the control plane, a `docker:dind` engine on that bridge
(never host networking) exporting its socket through a named volume, and the agent with
`DOCKER_HOST` on that socket. Preflight: no other Quasar container on the host, control plane
`dd2c97b` schema 84, zero active sessions. Only the `dind` engine was ever stopped.

| Step | Observed |
|---|---|
| Engine stopped, agent started | One process start. `boot-diagnostic-mode`, fault `runtime_unusable`. Registered: host `online` in `GET /v1/hosts`. |
| Readiness card | `runtime_endpoint` fail ("unreachable: no engine answered at the socket") with its fix; `startup_cleanup` fail ("Every launch on this host is refused") with its fix; the other runtime checks skip. |
| Health | 503, `status: diagnostic`, the reason names the fault. |
| Launch through `POST /v1/sessions` | Session `failed`; `error_message` is "agent rejected assign: host in diagnostic mode (runtime_unusable): … no override lifts this." |
| Withheld work | Zero `homes-gc`, `drvvol-*`, `cudart-*`, `host-probe-*` or `image-*` lines before the resume. |
| Engine restarted | `boot-diagnostic-resumed` on the next retry. Container `StartedAt`, pid and `RestartCount=0` unchanged; one process start in the whole window. Health 200; `runtime_endpoint` pass; `startup_cleanup` gone from the card. |

## Real-Docker evidence on the AMD test host: a live application left behind

Run 2026-09-19 on the AMD test host, image from `e7da24c` pulled through the local registry.
Preflight: no container of any kind on the host, so no stack and no session to disturb. The
stack was disposable: Postgres, the control plane, and a `docker:dind` engine with
`live-restore` on, whose dockerd was not PID 1 so it could be stopped alone. The agent ran
inside that engine, as production does, because a launch is refused unless the agent can
inspect its own mounts through the engine it controls. Its PID 1 was a respawn loop so the
agent process could be SIGKILLed and come back while the engine was down.

| Step | Observed |
|---|---|
| Before | 28 of 28 readiness checks pass. A real session on the AMD GPU (VA encoder) is `running`: application container `c1ba7037…` and audio sidecar `c4dd7917…`, one application journal. |
| dockerd stopped, agent SIGKILLed | Both containers keep running (live-restore). The respawned agent logs `runtime-application-retirement-pending` and `boot-diagnostic-mode`, and registers. |
| Card, health, launch | `runtime_endpoint` fail, `startup_cleanup` fail, each with its fix; health 503; `POST /v1/sessions` ends `failed` with "agent rejected assign: host in diagnostic mode (runtime_unusable)…". |
| While diagnostic | The leftover application's process is untouched. Zero homes-GC, provisioner, host-probe or image lines among the 21 log lines between `boot-diagnostic-mode` and `boot-diagnostic-resumed`. |
| dockerd restarted | Resumed on the next retry, 15 s later, agent pid unchanged (1773), container `RestartCount=0`. Engine events: `stop`, `die`, `destroy` for exactly `c1ba7037…` and `c4dd7917…`. The journal reads `Completed`. No application container was created. |
| After the resume | Homes GC arms 3 ms after `boot-diagnostic-resumed`, then host probes start (their own short-lived probe containers are the only creates). Health 200, runtime checks pass, `startup_cleanup` gone. |

## Real-Docker evidence on the AMD test host: a managed home is preserved

Run 2026-09-19 on the AMD test host with images built from `4ca2433` (agent contract 148 pass,
0 fail, 2 GPU-gated skips; control plane 23 pass). Preflight: no container of any kind on the
host. Same disposable stack as above. The fixture application has `managed_home = true`, so
the live application container mounts `<home root>/admin/<app>` read-write at `/home/quasar`.
Before the fault the home was given a 1 MiB random file, a 64 KiB random file and a text save,
and the application container was shown to read them. An aged throwaway-shaped home
(`agent-deadbeef-cafebabe`, idle 433 h, retention 72 h) stood beside it as a homes-GC
candidate: homes GC never considers a real account's home, so the decoy is what shows whether
GC ran.

The home's manifest is the sorted `sha256sum` of every file under it; its own SHA-256 was
`a4c68904…8767`, 3 files, 1 114 203 bytes, at each of four points:

| Point | Home manifest | Decoy |
|---|---|---|
| Before the fault, application running | `a4c68904…` | present |
| Diagnostic mode, dockerd down, application still mounting the home | `a4c68904…` | present |
| dockerd back, sweep not yet run | `a4c68904…` | present |
| After recovery | `a4c68904…`, all three file hashes identical | deleted |

Homes GC logged nothing between `boot-diagnostic-mode` and `boot-diagnostic-resumed`, a window
of about three minutes in which the unreaped container held the home. The order at recovery,
engine events against agent log lines, UTC:

| Time | Source | Event |
|---|---|---|
| 01:57:54.127 | engine | application `aa3f8404…` `die` |
| 01:57:54.149 | engine | application `aa3f8404…` `destroy` |
| 01:57:54.214 | engine | audio sidecar `269fa077…` `destroy` |
| 01:57:54.224 | agent | `boot-diagnostic-resumed` |
| 01:57:54.228 | agent | `homes-gc: armed` |
| 01:57:54.232 | agent | `homes-gc: deleted …/agent-deadbeef-cafebabe (idle 433 h)` |
| 01:57:54.233 | agent | `homes-gc: sweep … scanned 2, candidates 1, deleted 1` |

So homes GC ran only after the sweep had destroyed the last container that could mount a
home, and it then did real work. The agent pid (2050) and the container's `RestartCount=0`
did not change; the journal reads `Completed`; no application container was created; health
returned to 200 and the runtime checks to pass. As before, the card, the 503 and the refused
launch were captured while diagnostic.

## The console

`startup_cleanup` is in the Container runtime group, first, ahead of `runtime_endpoint`
(`web/src/lib/readiness/groups.ts`, pinned by `groups.test.ts`). No style, label or component
changed. Visual check, 2026-09-19, headless Chromium against the disposable stack's own
console at `4ca2433`: in diagnostic mode the host page shows the card's existing "Container
runtime" heading with two failing tiles in the card's usual tile style, "startup cleanup"
then "runtime endpoint", each with its summary and its fix. After recovery the group shows the
four runtime checks passing and no `startup_cleanup` tile. The same page still has an "Other"
group holding `nvidia_driver_mount` and `host_container_mounts`. Both ids predate RH-02
(`79a41a3`) and are missing from the hand-kept list in `groups.test.ts`, which is how they
escaped the pin. That is left for whoever owns the card.

## Wire shape

#256 changes no message shape. Every readiness check the control plane stored during these
runs, in diagnostic mode and after, has exactly the keys `id`, `status`, `summary` and
`remediation`, and the statuses seen were `pass`, `fail`, `skip` and `warn`. #256 emits no
`blocks`, `observed_at` or `source`, and no `unknown` status. Those arrive with #261, after the
owner signs off `quasar-protocol` PR 22. `startup_cleanup` and `runtime_endpoint` are the
safety checks #261 will mark `blocks {scope: host, enforced_by: agent}`; until then "blocks
launches" is carried by the agent's own refusal, not by a field.

## NVIDIA

Not run, and not required. #256 is vendor-neutral startup logic: the decision, the retry, the
refusal and the health state never look at the GPU vendor, and the diagnostic connection
never initialises GStreamer or EGL, which `capacity.rs` and `readiness.rs` confirm by
reading. The vendor-specific work it touches, the driver-volume and CUDA provisioners, is
withheld entirely until the resume and then runs from the same code path as a normal boot.

## Not verified

- A launch refused this way leaves the session `failed` with `failure_code` null, because the
  existing `session_assign` nack carries only a string. Nothing in #256's acceptance asks for a
  code.

## Handoff notes

- **A separate finding, filed as #268.** With the Vulkan encoder the scheduler admits only GPU
  `index = 0` (`schedulableBindingSQL`). On the AMD test host the one visible GPU is index 1,
  because the kernel enumerates both of the machine's cards, so every launch there is
  `no_host_available` until the encoder is set to `va`. The evidence run used `va`.
- An agent only outlives its engine when dockerd restarts under `live-restore`, or when the
  socket it was given is wrong. Stopping the engine that runs the agent stops the agent.
- The boot sanity gate is unchanged and still runs on the first normal connection, so after a
  resume it can exit for retry exactly as it would have at boot. The development host shows
  this: its containers see the host's GPUs under `/sys/class/drm` with no `/dev/dri`. A fresh
  container there spends the gate's five retries after the resume; the clean no-restart record
  above is from a container whose retries were already spent.
- `deploy/redeploy.sh` recreates the agent with `up --wait`. A diagnostic agent is never
  healthy, so a full redeploy fails at that step, before the verify block prints the
  `diagnostic` arm. That is the intended "not green"; the arm is reached by the verify-only
  paths.
- Diagnostic mode is a startup state. An engine lost after normal startup is reported by the
  runtime checks and does not re-enter it.
- `make homes-gc` runs a separate process and is not gated by the agent's phase. It was not
  before either; it already skips the sweep when live mounts cannot be established.
- A launch refused this way leaves the session `failed` with `failure_code` null, since the
  existing `session_assign` nack carries only a string.
