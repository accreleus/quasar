# RH-02 #257 and #259: host probes, acceptance and handoff

Both tickets are closed. This is the record for whoever picks up the next RH-02 slice,
possibly on another machine: what landed, where it deviates from the spec and the brief,
what was and was not verified, and how to repeat the hardware runs.

Spec: #252. ADR 0005. Glossary: `CONTEXT.md` "Host readiness".

## State

| | |
| --- | --- |
| Branch | `feature/rh-02-probe-first` = `initiative/resilient-host-architecture` |
| Commits | `cbc584f` `f22b699` `edb9fc6` (#257), `baaca59` `d9d2a7e` `ddeff97` (#259), over `1c12362` |
| Not on | `develop`, `main`. No tag, no image publication, no Actions build. Promotion to `develop` needs the owner's separate approval after RH-02 acceptance. |
| Gates at `edb9fc6` | Rust (fmt, clippy `-D warnings`, `cargo test --all-targets`): 1693 passed, 0 failed, 9 ignored. At `baaca59`: web gate 3030 tests + typecheck + schema drift + build, `make verify` 428 pass / 0 fail; only Rust files changed after that. |
| Image | `quasar-node-agent:<date>-rh02-final`, built by `deploy/build-images.sh runtime --toolchain registry --no-prune`, contract 146 pass / 0 fail, source `edb9fc6`. Local only, on the AMD development host and `gpu-test`. Rebuild from the branch on any other machine. |
| Deployed | The isolated AMD acceptance stack and `gpu-test` both run that image, agent only. Earlier candidate tags and the `rh01-240` image stay on both as rollback targets. |
| Follow-up filed | #267 (below) |

## Where things live

`node-agent/src/host_probe/`

| File | Role |
| --- | --- |
| `decision.rs` | `Scheduler`: when probes run. A state machine with no I/O and no clock, table-tested. Change scheduling behaviour here, never in the orchestrator. |
| `orchestrator.rs` | One task owns the scheduler and carries out its actions. Probes run in their own tasks. `ProbeHandle` methods are non-blocking sends; they are what the launch path calls. |
| `runner.rs` | The production `ProbeRunner`: one target to one bounded run. |
| `child.rs` | Bounded child process: own process group, deadline kill, pre-emption, bounded stdout drain. |
| `media.rs`, `session/probe_media.rs` | Parent and child halves of the media probe. |
| `app_gpu.rs`, `audio.rs`, `container.rs` | The two container probes and their shared result shape. |
| `outcome.rs` | Outcome to readiness check. `indeterminate_status()` is the one function #261 flips from `warn` to `unknown`. |
| `launch_failure.rs` | Which probes a failed launch could explain. |

Also: `session/container.rs` `AppGpuAccess` (the one value a session's docker arguments and
the probe's `GpuProbeRun` are both realized from), `nvidia_volume.rs` `egl-selftest
--open-device [--render-node N]`, `runtime.rs` `Operation::wait_with_cancel`,
`session/warmup/gate.rs` `try_acquire_probe` and `set_release_listener`,
`readiness/report.rs` `forget` and `retained`.

Check ids: `input_probe`, `audio_probe`, `media_probe_gpu<N>`, `application_gpu_probe_gpu<N>`.
The bare `media_probe` and `application_gpu_probe` appear as `skip` on a host with no GPU.
The console groups per-GPU ids under their base id (`baseCheckId` in
`web/src/lib/readiness/groups.ts`).

## Deviations from the spec and the brief

Each is a decision, not an accident. The first one still wants the owner's yes or no.

1. **No capacity reservation for the media probe.** The brief says a probe "takes the same
   local encode reservation a session takes". A session takes none locally: the control
   plane counts encode slots and the agent only logs them. The nearest mechanism is the
   warm-up gate, which is a host-wide lock plus a reported "one fewer encode slot". The
   media probe takes the lock (`try_acquire_probe`), so it and a warm-up exclude each other
   and a deferred probe runs the moment the gate is released. It does not report the missing
   slot: a probe lasts 1 to 3 s and a launch pre-empts it anyway, so the only effect would be
   the control plane turning a launch away in that window, worst on a single-slot GPU. To
   follow the spec literally, call `set_reserved(true)` in `try_acquire_probe` and flip
   `a_media_probe_never_reports_a_reserved_encode_slot`.
2. **Pre-emption is host-wide and covers every probe kind.** Any launch pre-empts whichever
   probe is running, on any GPU. The spec only requires it for the GPU concerned.
3. **Nothing starts while a launch is in flight.** Between a session's assignment and its
   `running` report (or its rejection) the scheduler starts no probe at all, including
   host-wide ones. Added after a live run showed a pre-empted audio probe restarting during
   the launch and competing for the container runtime. Not in the spec.
4. **A lost control-plane connection pre-empts the running probe**, and nothing starts until
   the next registration. This is how "never inside the registration handshake window" holds
   across reconnects.
5. **Host-wide probes (input, audio) may run while a session is live.** Only per-GPU probes
   wait for the GPU. The spec forbids probing a GPU with a live session and is silent on the
   others.
6. **The "agent image" probe input is the build identity**, not the container image id. A new
   image is a new process, which re-runs every probe anyway, and this costs no engine call.
7. **A GPU the host is pinned away from is `skip`.** With `QUASAR_RENDER_NODE` naming another
   GPU the scheduler never places a session there, so a bind failure would be a false alarm.
8. **The launch-path sibling EGL gate is untouched.** #259 replaces the `nvidia_sibling_egl`
   readiness check (which launched a container from the refresh path) on every vendor. It
   does not replace `probe_sibling_egl()` in the session launch gate: removing a refusal is a
   behaviour change these advisory slices must not make. Both go through `run_gpu_probe`, and
   the runtime's one-probe-at-a-time gate covers both.
9. **The EGL self-test gained `--open-device`.** The existing test reads the client extension
   string and never touches a device, so with the GPU withheld it still passes on Mesa's
   software device. The application-GPU probe needs a device enumerated and initialized. The
   no-flag output is byte-identical to before.
10. **Only some endings are verdicts.** Exit 1 fails. Any other non-zero exit (2 is bad argv),
    a kill from outside (SIGKILL from the OOM killer, SIGTERM), a deadline, a pre-emption, a
    spawn failure and a device EGL names differently from the capacity report are all
    indeterminate. Only a fault signal (SIGSEGV, SIGABRT, SIGBUS, SIGILL, SIGFPE, SIGSYS,
    SIGTRAP) is a crash that fails. Indeterminate never replaces a retained pass, fail or skip.
11. **A launch failure is matched to probes by the runner's failure-text prefix**
    (`launch_failure.rs`), because there is no launch-error type. A missed prefix costs a
    missed re-run, nothing else.
12. **The audio probe identity is `quasar-pulse-probe-<nonce>`**, not the `quasar-probe-`
    prefix: the runtime's closed audio profile only accepts `quasar-pulse-<id>` with a socket
    dir named `pulse-<id>`. Boot retirement and ownership checks cover it as they cover a
    session's sidecar.

## Known limits

- The closed #258 probe profile carries the NVIDIA device request only together with the
  Quasar driver volume. On an NVIDIA host with a host-installed driver the probe container
  gets DRM access only. No such host was available.
- The probe container runs as `0:0`, so the DRM group grants are carried but not exercised.
- `recover_diagnostics()` on a still-running earlier GPU probe fails with `UnknownOutcome`
  before `run_gpu_probe` can answer `Busy`. The effect is the same (indeterminate,
  unreconciled, no second create); the `Busy` arm is reached only in unit tests.
- A probe observing a container holds one of the runtime executor's four in-flight permits.
- A deadline-killed input child may leave `/dev/input/eventN` nodes made by the fake-udev
  path, as an interrupted session does today.
- The `explains()` prefixes are coupled to the emit sites in `session/runner.rs` by
  convention only.

## Hardware evidence

Preflight on both hosts before any mutation: agent `rh01-240` (source `71e77f8`), schema 84,
0 active sessions; rechecked immediately before every recreate. Agent-only change, no
migration. Results below were read from the control plane's stored readiness unless marked.

| Host | `input_probe` | `audio_probe` | `media_probe_gpu0` | `application_gpu_probe_gpu0` |
| --- | --- | --- | --- | --- |
| AMD (Renoir, VA) | pass | pass | pass, 30 frames, `vah264enc` | pass, opened `/dev/dri/renderD128` |
| NVIDIA (RTX 5090, 610.57.04, driver volume, Vulkan) | pass | pass | pass, 31 frames, `vulkanh264enc` | pass, opened `/dev/dri/renderD128` |

Injected failures:

| Host | Fixture | Result |
| --- | --- | --- |
| AMD, #257 | acceptance agent recreated with only `/dev/dri/card0` | `media_probe_gpu0` fail with its fix; restored to pass |
| AMD, #259 | throwaway nested engine whose `/dev/dri` held inert nodes, throwaway agent inside it, registered as a second host | `application_gpu_probe_gpu0` fail: "no hardware EGL device (only software rendering is available)"; engine, volume and host row removed |
| NVIDIA, both | disposable containers with the probe's exact access and environment, GPU request withheld | media: "no encoder on this host can produce codec=h264", exit 1; EGL: `DEVICE_ERROR=no hardware EGL device` |

The NVIDIA failures were injected at container level, not through a registered agent.

Live on NVIDIA: a real session launches on the driver-volume path with this agent; 14
launches made during the start-up probes each pre-empted one (60 to 120 µs after the assign
in the two read from the log); 13 reached `running`; a GPU's probes stayed pending for a
session's whole life and ran within seconds of its end. No `quasar-probe-*`,
`quasar-pulse-probe-*` or runtime-dir entry was left after any run.

### Found only on hardware

- `waylanddisplaysrc` needs `XDG_RUNTIME_DIR` and leaves `wayland-N` behind on SIGKILL. The
  media child now gets a private runtime dir, removed on every path (`f22b699`).
- The compositor plugin logs to stdout, so a child's verdict must be its last stdout line.
- On NVIDIA two EGL devices name `/dev/dri/renderD128`, NVIDIA's and Mesa's, and Mesa's
  cannot initialize. The probe passed only because NVIDIA was listed first. Every device
  naming the node is now tried (`ddeff97`).
- The probe restart during a launch, deviation 3 (`edb9fc6`).
- #267: the 14th launch failed because the agent exited 255. GStreamer's segtrap fired while
  the session loaded `libgstvulkan.so`, in the first session after a process start. No probe
  was running and probes never encode inside the agent. Once in 14; the baseline image was
  not run through the same sequence, so whether the rate differs is unknown.

## Not verified

- Intel: not validated, nothing claimed (`docs/reports/rh01-intel-external-validation.md`).
- Restart recovery of an interrupted probe container on hardware (covered on the scripted
  Engine API double).
- A deadline kill of a hung media child on hardware (covered by the child-runner tests).
- Host-injected NVIDIA driver precedence (unit test only).
- The ignored real-Docker tests were not re-run in a `docker:dind` engine; the nested-engine
  fixture exercised the same profile through the agent.

## Repeating the runs

Hosts are addressed by role; addresses live in the operator-local `hosts.json`.

- **Gates.** Run `make verify`, `make test-rust`, `make test-web` serially, with the 5-minute
  load below 10 and nothing else running: `runtime::helper_tests` reports false
  `UnknownOutcome` failures under load. Use a per-worktree `CARGO_TARGET_DIR`. A fresh
  worktree needs `.claude/skills/quasar-session` linked from the main checkout for
  `make verify`, and `git submodule update --init protocol`.
- **Image.** `QUASAR_BASE_DIGEST=$(docker buildx imagetools inspect --format
  '{{.Manifest.Digest}}' ghcr.io/accreleus/quasar-base:latest) deploy/build-images.sh runtime
  --toolchain registry --no-prune --git-ref <sha> --tag-suffix <slug>`. About four minutes.
  Copy to another host with `docker save <tag> | ssh <host> docker load`.
- **Fast probe smoke without an image.** `cargo build` in `quasar-agent-dev` with a named
  volume as `CARGO_TARGET_DIR`, then run `quasar-node-agent media-probe --gpu 0` in the same
  image with `--device /dev/dri`, `QUASAR_ENCODER`, `QUASAR_RENDER_NODE` and
  `XDG_RUNTIME_DIR` set.
- **Reading results.** `select readiness::text from hosts` in the stack's Postgres, filter
  ids ending `_probe` or containing `_probe_gpu`. Agent log lines carry
  `token="host-probe-started|finished|preempted|forgotten"`. Never print a whole agent log:
  the compositor dumps multi-kilobyte GL extension lines. Grep and cut.
- **Injecting #257 on AMD.** Recreate the fixture agent with `devices: [/dev/dri/card0,
  /dev/uinput]`. With `/dev/dri` gone entirely the VA sanity check exits the agent instead.
- **Injecting #259 on AMD.** A probe container's `/dev/dri` comes from the engine's host, not
  the agent's container. Start a throwaway `docker:dind` on the bridge network (never
  `--network host`: a nested dockerd would rewrite the host's iptables), `mount -t tmpfs
  tmpfs /dev/dri` and `mknod` inert nodes inside it, load the image into it, and run a second
  agent there with `CONTROL_PLANE_URL` pointing at the fixture control plane's LAN address,
  `QUASAR_ALLOW_PLAINTEXT_AGENT=1` and the fixture's enrollment token. Delete the host row
  afterwards with `DELETE /v1/hosts/{id}` over the stack's HTTPS port; the HTTP port
  answers 308.
- **NVIDIA.** `--gpus all` injects `/dev/dri/card1` and `renderD128` by itself, so
  withholding the render node means withholding the GPU request too. A hand-built probe
  environment must use `<volume>/glvnd/egl_vendor.d`; with `egl_vendor.d` the NVIDIA vendor
  never registers and the run silently tests Mesa. The authoritative list is `nvidia_env` in
  `runtime/docker/helpers.rs`.
- **Driving sessions on `gpu-test`.** Do it on the host against its local API with the
  per-boot dev key (`POST /v1/dev/agent-session`), and read session state from the database.
  Launching right after an agent restart lands inside the start-up probes, which is how
  pre-emption was exercised.
- **Shared hosts.** Do the AGENTS.md version preflight, recheck immediately before any
  mutation, and ask the owner before deploying a candidate or switching the appliance between
  its own stack and the `gpu-test` VM.

## Next

#261 flips `indeterminate_status()` once the #260 amendment is signed, and brings
`observed_at` to the wire: the orchestrator already stamps every result, and
`ReadinessReport` keeps it. #256 (diagnostic registration) must withhold host probes in
diagnostic mode: gate the `registered` call in `connect_and_run`.

## Models

Fable 5.1: the decision model, test design, review of every diff, gates, hardware runs, the
hardware-found fixes. Opus: media probe, child runner, gate interlock, shared GPU-access
function, EGL device-open FFI, `wait_with_cancel`, all four reviews. Sonnet: outcome mapping,
orchestrator loop, production runner and agent wiring, the two container probes. Haiku:
console groups, changelog lines. No substitutions.
