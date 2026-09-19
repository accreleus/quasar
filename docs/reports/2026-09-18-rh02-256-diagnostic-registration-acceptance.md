# RH-02 #256: diagnostic registration — acceptance and handoff

| | |
|---|---|
| Ticket | #256, specification #252 "Diagnostic registration", ADR 0005 |
| Source | `initiative/resilient-host-architecture`. Every run below used the image built from `e7da24c`; the one later code change rewords a log line. |
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

Run serially with the 5-minute load below 2, per-worktree `CARGO_TARGET_DIR`.

| Gate | Result |
|---|---|
| `make test-rust` (fmt, clippy `-D warnings`, `cargo test --all-targets`) | 1704 passed, 0 failed, 9 ignored at `e7da24c`, twice in a row |
| `make test-go` | one failure, present before this work: `TestEnrollHostComposeMatchesBase` — `QUASAR_HOMES_FREE_SPACE_FLOOR_GIB` (#253, `a5e1ea2`) is in `deploy/docker-compose.yml` and missing from `deploy/enroll-host.sh`. Every other package passes. Not fixed here: the file belongs to #253. |
| `make verify` | 408 pass, 2 warn, 15 fail. All 15 are host tooling gaps: 14 need the operator-local `quasar-session` skill script, which does not exist on this machine, and `release:signature-contract` needs `openssl`. Every `redeploy:*` check passes, including the new `diagnostic` pins. |

Two lock-lease tests failed once each and passed on the reruns:
`artifact::tests::abandoned_kernel_lock_is_recovered_without_waiting_for_age` and
`container_ownership::tests::ownership_survives_restart_and_distinct_agents_are_isolated`
(`EAGAIN` on a lease in a fresh temp dir). Neither file is touched here. The suite forks a
child that deliberately segfaults, which is a plausible holder of an inherited lock fd; it
also leaves a root-owned `node-agent/core` behind.

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

Run 2026-09-19 on `amd-test`, image from `e7da24c` pulled through the local registry.
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

## Not verified

- **A managed home on disk.** The fixture application mounted no home, so "the home is
  preserved" was shown only as "the container that could hold one was never touched, and
  homes GC did not arm". The double's retirement tests cover the journal side.
- **NVIDIA.** Nothing ran on `nvidia-test`. The ordering argument (no GStreamer or EGL
  in-process before the driver volume is adopted) rests on reading `capacity.rs` and
  `readiness.rs`.
- **The console.** `web/` was out of scope. `startup_cleanup` is not in
  `web/src/lib/readiness/groups.ts`, so the card shows it under "Other" with no "blocks
  launches" marker.

## Handoff notes

- **A separate finding, not #256.** With the Vulkan encoder the scheduler admits only GPU
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
