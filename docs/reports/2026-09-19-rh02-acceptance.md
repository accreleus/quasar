# RH-02 probe-first host readiness: acceptance report

| | |
|---|---|
| Ticket | #265. Parents #210 and #211, initiative #207, specification #252, ADR 0005 |
| Source | `feature/rh-02-probe-first` = `initiative/resilient-host-architecture` at `7e5f859` when runs A to C were made, and at `b7afff6` (documentation only) for run D. No production code changed in this slice. |
| Not on | `develop`, `main`. No tag, no image publication, no Actions build. |
| Protocol pin | `7ecc596` (amendment 11). quasar-protocol#23 (wording-only clarifications) was still open, so the pin was left alone. |
| Schema | 85 (`0085_evidence_gated_readiness`), one-way |
| Images | `quasar-control-plane` from `901058f`, `quasar-node-agent` from `97fd0d9` (no source under `control-plane/`, `web/`, `node-agent/` or `deploy/` changed after those commits), `quasar-updater` from `7e5f859`, built for this slice. All through `deploy/build-images.sh … --no-prune` with the image contract passing (updater: 16 passed, 0 failed). Nothing published. |
| Evidence | [`rh02-265/`](rh02-265/): one directory per install. Every file passed a sanitizer that refuses to write while an operator-local name still matches. |

**This report requests the owner's acceptance of RH-02. It does not perform promotion.**
Nothing here merges into `develop`; #210, #211 and #207 stay open until the owner accepts.

## What ran

Four fresh installs, each with a new Compose project, a new database and a new agent
identity. A, B and C were removed afterwards. **Run D was added a day later**, after the owner
confirmed the NVIDIA card was free, and at the owner's request it was left running for
visual inspection when this was written.

| Run | What it is | What it is not |
|---|---|---|
| **A** | A fresh Quasar install on the AMD test host: a Linux system container on the Unraid host, with Docker nested inside it and a standard-Linux userspace, under the Unraid host's kernel and amdgpu kernel driver. | Not a virtual machine, and not a native Unraid install. |
| **B** | The same on the NVIDIA test host, under the Unraid host's kernel and NVIDIA kernel driver. The driver volume was provisioned from empty. | Not a native Unraid/NVIDIA install: Docker here is the container's own, with its own paths and its own NVIDIA container toolkit. |
| **C** | A native install through the Unraid host's own Docker, with plain `docker compose`, on the AMD GPU, beside the owner's existing install, which was stopped throughout and was left byte-for-byte as found. | Not NVIDIA. The #264 harness was not run here: it starts privileged nested engines and shadows device nodes, and this engine also hosts the owner's other services. The owner authorised an isolated install, not fault injection. |
| **D** | A native install through the Unraid host's own Docker with the NVIDIA overlay, on the RTX 5090, with the same isolation as run C. The NVIDIA test host was idle throughout. The host's Docker already had the `nvidia` runtime and a CDI specification; nothing on the host was changed. | The #264 harness was not run here, for the same reason as run C. No agent-recreate check was made. |

Each run followed `deploy/README.md` Part 1, path A ("Install a release") as a self-hoster
would, with the two image lines naming the locally built images instead of a published
release. Every place the documentation did not work is listed under
[Operator documentation findings](#operator-documentation-findings).

## Acceptance text against the evidence

"Met" means shown on hardware in this slice or an earlier RH-02 record. "Partly met" says
what is missing.

### #210 "Build structured host facts and disposable runtime preflight"

| # | Acceptance text | Verdict | Evidence and reason |
|---|---|---|---|
| 1 | "Probe GPU/compositor/encoder, audio, uinput, storage permissions/free space and relevant network prerequisites." | **Met, with defect #280** | All four installs report `media_probe_gpu<N>` (composite and encode on the real render node), `application_gpu_probe_gpu<N>`, `audio_probe`, `input_probe`, `homes_root_writable`, `homes_free_space`, `template_free_space` and `media_reachability`, each with observed-at and source: [`install-a/host-body.json`](rh02-265/install-a/host-body.json), [`install-b/host-body.json`](rh02-265/install-b/host-body.json), [`install-c/host-body.json`](rh02-265/install-c/host-body.json). Built in [#257/#259](2026-09-19-rh02-257-259-host-probes-acceptance.md). Two limits are filed. #272: the media probe counts encoded frames and cannot see a corrupt picture. #280: on run D the application GPU probe fails on a healthy host, because the probe container is not given the GPU the way a real application container is. |
| 2 | "Failure leaves diagnostic registration available but blocks affected workloads." | **Met** | Diagnostic registration: [#256](2026-09-18-rh02-256-diagnostic-registration-acceptance.md). Blocking by scope: [#262](2026-09-19-rh02-262-readiness-gate-acceptance.md) and the #264 matrix below (host, homes and gpu scopes; a launch that mounts no home is not refused by a homes failure; a second host takes the launch). Shown again here on a real install with real faults: a host that stays **online** while `runtime_endpoint` fails and launches are refused ([`1c-real-engine.txt`](rh02-265/install-b/1c-real-engine.txt)), and a first-boot NVIDIA host blocked only while its GPU probes fail ([`tls-real-block.txt`](rh02-265/install-b/tls-real-block.txt)). |
| 3 | "Probe cleanup is ownership-scoped" | **Met, with one harness gap** | Probe containers carry the agent's ownership label and are removed; nine sessions and every probe on install B, including three app crashes, left no container and no socket directory. On run C the new agent left the existing install's four stopped containers untouched ([`install-c/install-log.txt`](rh02-265/install-c/install-log.txt)). The agent logs its legacy sweep only when it preserved or could not resolve something; there was nothing of that kind to consider, so the log is silent and the proof is the before/after comparison. The gap is in the test tooling, not the product: #275. |
| 4 | "missing drivers are reported, not silently installed or bypassed" | **Met** | Run B from an empty driver volume: the card reported the three NVIDIA graphics checks as `provisioning` with the text "no action needed; the agent restarts itself when it finishes", the GPU probes failed and blocked launches meanwhile, nothing was written outside the volume, and the block cleared by itself ([`agent-firstboot-provisioning.txt`](rh02-265/install-b/agent-firstboot-provisioning.txt), [`provision-gate-timeline.txt`](rh02-265/install-b/provision-gate-timeline.txt)). With provisioning off and an empty volume the probes fail and stay failed (#264 row 4b). |
| 5 | Scope text: "Discover CDI where available; preserve tested NVIDIA compatibility paths. Report fact provenance and freshness." | **Met** | `runtime_cdi` on install B reports CDI enabled, the spec directories and the discovered devices, and states that GPU injection does not use CDI. Provenance and freshness: [#261](2026-09-19-rh02-261-readiness-provenance-acceptance.md), visible on every card captured here. |

### #211 "Expose readiness diagnostics and validate fresh self-hoster installs"

| # | Acceptance text | Verdict | Evidence and reason |
|---|---|---|---|
| 1 | "Fresh Unraid/NVIDIA and standard Linux installs have recorded evidence." | **Met as to evidence; the evidence includes a blocking defect** | Standard Linux: runs A (AMD) and B (NVIDIA). Native Unraid: run C on the AMD GPU and **run D on the NVIDIA GPU**. Run D installed cleanly and its card rendered, but `application_gpu_probe_gpu0` failed and the gate refused every launch with `host_not_ready` ([`install-log.txt`](rh02-265/install-d/install-log.txt), [`host-body-blocked.json`](rh02-265/install-d/host-body-blocked.json)). With that one check overridden, a real session was correct in every respect ([`session.txt`](rh02-265/install-d/session.txt)), so the failure is a false positive: #280, cause in [`probe-vs-app-container.txt`](rh02-265/install-d/probe-vs-app-container.txt). The recorded evidence the line asks for exists. What it records is that RH-02, as it stands, blocks a healthy native Unraid/NVIDIA host until an admin overrides a wrong check. |
| 2 | "Missing runtime, invalid storage, unavailable input and GPU failures appear before launch." | **Met** | #264 matrix, rerun in this slice on both test hosts (AMD 103 pass, 0 fail, 1 unperformed; NVIDIA 96, 0, 4). Row 1c, unperformed on NVIDIA in #264 because the nested engine cannot serve a launch there, was performed against install B's own engine and passed. |
| 3 | "Host-local readiness never claims browser reachability; the browser probe supplies that evidence." | **Met** | #264 row 8 on both hosts. Every captured card carries the sentence "It does not show whether a browser can reach the host", and `media_reachability` speaks only of the host's own inbound firewall. |
| 4 | "Reuse #158 and #149/#126 evidence boundaries; do not claim untested Intel support." | **Met** | Nothing is claimed for Intel. [`rh01-intel-external-validation.md`](rh01-intel-external-validation.md) remains the only Intel evidence, and it is external. |
| 5 | Scope text: "Render effective facts, failed checks and corrective actions through existing readiness/wizard patterns. Keep explicit admin overrides visible." | **Met, with one limitation** | The same card renders in the setup wizard's host step and in Fleet ▸ host on all three installs, with seven "can block launches" markers ([`install-a/readiness-card-wizard.txt`](rh02-265/install-a/readiness-card-wizard.txt), [`install-c/readiness-card-wizard.txt`](rh02-265/install-c/readiness-card-wizard.txt), and the three `readiness-card-console.txt`). Overrides: [#263](2026-09-19-rh02-263-readiness-override-acceptance.md) and #264 row 7. Not seen: the user-facing launch message in a real browser (see Limitations). |

## Tested hardware and software matrix

Facts not in a log file were captured live and are in each install's `environment.txt`.

| | Run A | Run B | Run C |
|---|---|---|---|
| What the run is | Fresh install on the AMD test host: a Linux system container (LXC) on the Unraid host, Docker nested inside it, standard-Linux userspace, the Unraid host's kernel and amdgpu kernel driver | Fresh install on the NVIDIA test host: a Linux system container (LXC) on the Unraid host, Docker nested inside it, standard-Linux userspace, the Unraid host's kernel and NVIDIA kernel driver | Native install through the Unraid host's own Docker with plain compose, AMD GPU |
| Source commit of the clone / working tree | `7e5f85998448432f4067eb4a15a188b8effacc51` ([install-a/install-log.txt](rh02-265/install-a/install-log.txt) `01-install.txt`) | `7e5f85998448432f4067eb4a15a188b8effacc51` ([install-b/install-log.txt](rh02-265/install-b/install-log.txt) `01-clone.txt`) | `7e5f85998448432f4067eb4a15a188b8effacc51` ([install-c/install-log.txt](rh02-265/install-c/install-log.txt) `05-install.txt`) — images were `docker load`ed, not pulled from a clone |
| control-plane image source commit | `901058fbb503382eed758c8f718fba5b4cffc912` (`<local-registry>/quasar-control-plane:dev-901058f`) | same | same (`Loaded image: <local-registry>/quasar-control-plane:dev-901058f`) |
| node-agent image source commit | `97fd0d9b93e17933afd4f9e151c3bf44541800bd` (`<local-registry>/quasar-node-agent:dev-97fd0d9`) | same | same |
| updater image source commit | `7e5f85998448432f4067eb4a15a188b8effacc51` (`<local-registry>/quasar-updater:dev-7e5f859`) | same | same |
| schema_migrations version | 85 ([install-a/final-identity.txt](rh02-265/install-a/final-identity.txt): version 85, not dirty) | 85 ([install-b/final-identity.txt](rh02-265/install-b/final-identity.txt): version 85, not dirty) | 85 ([install-c/final-identity.txt](rh02-265/install-c/final-identity.txt): version 85, not dirty) |
| Engine / API version | Docker Engine 29.8.1; engine offers API 1.40–1.56, agent negotiates 1.53 ([install-a/final-identity.txt](rh02-265/install-a/final-identity.txt), [install-a/host-body.json](rh02-265/install-a/host-body.json) `runtime_api_version`) | Docker Engine 29.8.1; engine offers API 1.40–1.56, agent negotiates 1.53 ([install-b/host-body.json](rh02-265/install-b/host-body.json) `runtime_api_version`) | Docker Engine 29.7.1; engine offers API 1.40–1.55, agent negotiates 1.53 ([install-c/final-identity.txt](rh02-265/install-c/final-identity.txt), [install-c/host-body.json](rh02-265/install-c/host-body.json) `runtime_api_version`) |
| Compose version | 5.5.1 ([environment.txt](rh02-265/install-a/environment.txt)) | 5.5.1 ([environment.txt](rh02-265/install-b/environment.txt)) | 2.40.3 ([environment.txt](rh02-265/install-c/environment.txt)) |
| Kernel and userspace | `6.18.47-Unraid`, the Unraid host's kernel; userspace Fedora Linux 43 (Container Image) | `6.18.47-Unraid`, the Unraid host's kernel; userspace Fedora Linux 43 (Container Image) | `6.18.47-Unraid`; Unraid OS 7.4 |
| GPU model / PCI id | AMD "Granite Ridge [Radeon Graphics]", PCI `1002:13C0`, render node `/dev/dri/renderD129` | NVIDIA GeForce RTX 5090, PCI `10de:2b85` (`0x2B8510DE`), render node `/dev/dri/renderD128` | AMD "Granite Ridge [Radeon Graphics]" (same physical card as run A), PCI `1002:13c0`, render node `/dev/dri/renderD129` |
| Driver versions | amdgpu kernel driver (the Unraid host's; no version string captured); in the agent image RADV, Mesa 25.3.6, `mesa-va-drivers` 25.3.6 | NVIDIA kernel module 610.57.04 (the Unraid host's, open kernel module); container toolkit 1.20.1 in the test host; graphics userspace provisioned by Quasar into the driver volume, 610.57.04 | amdgpu kernel driver; the same agent image, so RADV and Mesa 25.3.6. The NVIDIA driver is also present on this host and was not used |
| CDI state (`runtime_cdi`, as reported) | "CDI is enabled (spec dirs: /etc/cdi, /var/run/cdi) and the engine discovered no devices; GPU injection does not use CDI" | "CDI is enabled (spec dirs: /etc/cdi, /var/run/cdi); devices discovered: nvidia.com/gpu=0 (cdi), nvidia.com/gpu=GPU-\<uuid\> (cdi), nvidia.com/gpu=all (cdi); GPU injection does not use CDI" | "CDI is enabled (spec dirs: /etc/cdi, /var/run/cdi); devices discovered: nvidia.com/gpu=0 (cdi), nvidia.com/gpu=GPU-\<uuid\> (cdi), nvidia.com/gpu=all (cdi); GPU injection does not use CDI" |
| Number of readiness checks reported | 30 | 34 | 32 |
| Checks carrying `blocks` | the same seven as run C, as both captured cards show. `host-body.json` lists only the first five because it was read seconds after a restart, before the probes had reported ([NOTE-host-body.txt](rh02-265/install-a/NOTE-host-body.txt)) | `runtime_endpoint`, `homes_root_writable`, `homes_free_space`, `input_probe`, `audio_probe`, `media_probe_gpu0`, `media_probe_gpu1`, `application_gpu_probe_gpu0`, `application_gpu_probe_gpu1` | `runtime_endpoint`, `homes_root_writable`, `homes_free_space`, `input_probe`, `audio_probe`, `media_probe_gpu1`, `application_gpu_probe_gpu1` |
| Effective encoder of the real session | `vah264enc` after `QUASAR_ENCODER=va`. The default `vulkanh264enc` gave a corrupt picture (#272) ([session.txt](rh02-265/install-a/session.txt)) | `vulkanav1enc`, the default ([session.txt](rh02-265/install-b/session.txt)) | `vah264enc` after `QUASAR_ENCODER=va`. The default `vulkanh264enc` gave a corrupt picture (#272) ([session.txt](rh02-265/install-c/session.txt)) |
| Session resolution / fps / codec | 1920x1080, 60 fps, H.264 (`packetization-mode=1;profile-level-id=42e01f`) | 2560x1440, 60 fps, AV1 (`level-idx=5;profile=0;tier=0`) | 1920x1080, 60 fps, H.264 (`packetization-mode=1;profile-level-id=42e01f`) |
| Time from acknowledged stop to containers + socket dirs gone | 1255 ms (delete acked `1789859909547`; last cleanup line `epoch_ms=1789859910802 containers=0 dirs=0`) | 1271 ms (delete acked `1789825868795`; last cleanup line `epoch_ms=1789825870066 containers=0 dirs=0`) | 1376 ms (delete acked `1789861053449`; last cleanup line `epoch_ms=1789861054825 containers=0 dirs=0`) |
| Identity / data persistence across agent recreate | Same `id` and `node_name` (`quasar-node-1`) before and after; only `last_registered_at` advanced ([install-a/agent-recreate.txt](rh02-265/install-a/agent-recreate.txt)) | Same `id` and `node_name` (`quasar-node-1`) before and after; only `last_registered_at` advanced ([install-b/agent-recreate.txt](rh02-265/install-b/agent-recreate.txt)) | Same host id, one host row, marker file in the managed home intact ([environment.txt](rh02-265/install-c/environment.txt)) |

### What the container boundary changes

Kernel facts on a card (kernel version, loaded modules, user-namespace limits, `/sys`) describe the Unraid host. Device nodes and their permissions, the container runtime, CDI and the toolkit describe the container. How the devices reach each system container is the owner's LXC configuration and was not inspected; what was recorded is what each container sees ([install-a](rh02-265/install-a/environment.txt), [install-b](rh02-265/install-b/environment.txt)). Nesting Docker inside a system container cost nothing visible here: no user-namespace remapping, AppArmor or cgroup device denial appeared in any run, and `user_namespaces` passed.

The two test hosts share the Unraid host's kernel, kernel modules and GPU
drivers — only the device nodes and their permissions are each container's
own. Neither test host has `/dev/kmsg`, so the documented compose edit (drop
the device from `devices:` and drop `SYSLOG` from `cap_add:`) was applied on
both before the stack would come up; the native install on the Unraid host
needed no such edit, because `/dev/kmsg` is present there
([install-c/install-log.txt](rh02-265/install-c/install-log.txt) isolation
check: `crw-r--r-- root root /dev/kmsg`).

`/sys` is not namespaced between the two test containers, so the NVIDIA test
host's agent also listed the AMD GPU as a second GPU with 2 encode slots, even
though the AMD device node is not present inside that container — reflected
in run B's `host-body.json` as `media_probe_gpu1` / `application_gpu_probe_gpu1`
both reporting `skip: "This host is pinned to /dev/dri/renderD128; sessions
are never placed on GPU 1"` rather than being absent. Readiness is truthful about it; the capacity display is not, and still counts that GPU's 2 encode slots (#276).

uinput event nodes are static in both system containers, and the agent
fabricates the udev entries itself; input worked in both (`input_probe: pass`
in every run). The native install on the Unraid host, by contrast, faces
Unraid's two path forms (`/mnt/cache/appdata/...` vs `/mnt/user/appdata/...`,
see [install-c/install-log.txt](rh02-265/install-c/install-log.txt)) and has
both GPUs' device nodes present at once (`/dev/dri` lists `card0`/`renderD128`
NVIDIA and `card1`/`renderD129` AMD), a situation neither test host is in.

## Real sessions

One real session per install, driven from Google Chrome 153 through the install's own
self-signed TLS, launched from the library page. Frames are the decoded video element, not
page screenshots.

| | A (AMD test host) | B (NVIDIA test host) | C (native Unraid, AMD) |
|---|---|---|---|
| Visible application content | XFCE desktop, 1920x1080, 60 fps | XFCE desktop, 2560x1440, 60 fps | XFCE desktop, 1920x1080, 60 fps |
| Effective encoder (session log) | `vah264enc` after `QUASAR_ENCODER=va`; the default `vulkanh264enc` gave a corrupt picture (#272) | `vulkanav1enc`, AV1, 7000 kb/s | `vah264enc` after `QUASAR_ENCODER=va`; default corrupt (#272) |
| Mouse | pointer moved, right click opened the desktop menu | the same, then a click opened a file manager window | the same |
| Keyboard | shortcut opened a terminal; a typed command ran | the same | the same |
| Audio | received audio energy 0 while silent, level 0.50 during a 440 Hz tone | the same (energy 0, then 0.51 rising to 2.15) | the same |
| Normal stop | `202`, state `stopped`, no failure code | the same | the same |
| Acknowledged stop to containers and socket directories gone | 1.26 s | 1.27 s | 1.38 s |
| Identity and data across an agent recreate | same host id, one host row, marker file, app and session history intact | the same; the driver volume was adopted, not downloaded again | same host id, one host row, marker file intact |

Evidence: `session.txt`, `agent-recreate.txt` and the `frame-*.png` files in each install
directory.

Two honest notes on method. The desktop image has no audio player, so the tone was played
into the session's own audio server from its sidecar container: that exercises capture,
encode, WebRTC and the browser, and not the application's own socket connection, which
`audio_probe` covers. And the typed text arrives in lower case because the test driver does
not press Shift; the product was not at fault.

## Run D: native Unraid on the NVIDIA GPU

- The agent found the host's own NVIDIA graphics userspace, so it did **not** provision a
  driver volume (`driver_volume_version`: skip, "host driver packages are in use"). The three
  NVIDIA graphics checks and `media_probe_gpu0` passed; the media probe encoded with
  `vulkanh264enc` ([`readiness-card-wizard.txt`](rh02-265/install-d/readiness-card-wizard.txt)).
- `application_gpu_probe_gpu0` failed and never cleared: "the EGL stack loads but no GPU could
  be opened: eglInitialize failed ... (egl error 0x3001)". The gate listed it and a launch was
  refused `503 host_not_ready` ([`gate-timeline.txt`](rh02-265/install-d/gate-timeline.txt)).
- The override was set through the API (`200`). The check stayed listed, marked overridden,
  and the launch then succeeded. That is the #263 behaviour, seen here on a real, non-synthetic
  block for the first time.
- The session: XFCE desktop at 2560x1440 and 60 fps, AV1 on `vulkanav1enc`; the right-click
  menu opened; a shortcut opened a terminal and a typed command ran; received audio energy was
  0 while silent and level 0.50 during the tone; normal stop `202` to `stopped`, and containers
  and socket directories gone 1.58 s after the acknowledgement
  ([`frame-mouse-contextmenu.png`](rh02-265/install-d/frame-mouse-contextmenu.png),
  [`frame-keyboard-terminal.png`](rh02-265/install-d/frame-keyboard-terminal.png)).
- Why the probe is wrong: a real application container is created with an NVIDIA device
  request, so the container toolkit injects the driver; the probe container is created with
  none, and with no driver volume on this host it has no NVIDIA userspace at all. On a host
  that uses the driver volume (run B) the same probe passes.
- The owner's existing install was compared before the run and checked after the session
  work: unchanged. The full before-and-after proof belongs to teardown, which is pending.

## Run B: the driver volume from empty

- First boot: the agent began provisioning one second after start and reported all checks
  passing 91 s after start, including a cold download. No operator action.
- Measured again with the gate polled every second: the two GPU probes were listed as
  blocking for **14 s** (20 s from agent start to clear), then cleared by themselves after
  the agent's self-restart ([`provision-gate-timeline.txt`](rh02-265/install-b/provision-gate-timeline.txt)).
- A launch made during a third such block was refused over HTTPS with `503 host_not_ready`,
  a message naming no check, and no Retry-After; 19 s later the block had cleared and the
  next launch returned `201` and ran ([`tls-real-block.txt`](rh02-265/install-b/tls-real-block.txt)).
- The host's container toolkit injects the GPU through CDI, and the agent still found the
  NVIDIA graphics userspace missing and provisioned it. CDI injection alone does not supply
  the graphics libraries on this host.

Difference from the #262 and #264 records: the NVIDIA test host now has the NVIDIA
container runtime registered with Docker, container toolkit 1.20.1, a generated CDI
specification and the CDI refresh units enabled. #262 recorded it without the toolkit.

## Closing #264's four gaps

| Gap | Result |
|---|---|
| A stack with TLS on | **Partly closed.** The harness cannot target an installed stack: it sets `QUASAR_TLS=off` on a disposable stack of its own, routes its real agent through a plaintext relay it starts before `compose up`, and injects faults by recreating that agent with compose overrides. Pointing it at an operator's install would mean mutating that install's agent and database. Instead, the gate was exercised with real faults on install B over its own HTTPS listener: the refusal, its headers and its message, the `409` on an agent-enforced override, recovery and a successful launch. Rows 6 and 7 (classification and the override lifecycle) have not been run over TLS. |
| Row 1c on NVIDIA hardware | **Closed**, with the owner's go-ahead at the time. Install B was the only stack on that host. The engine was frozen with SIGSTOP and thawed with SIGCONT, because the daemon has no live-restore and stopping it would also have stopped the control plane; a dead-man timer guaranteed the thaw. Host stayed online, `runtime_endpoint` failed with `blocks {host, agent}`, launch refused `503 host_not_ready` with no Retry-After, override PUT `409`, recovery with the agent's restart count still 0, recovery launch `201` and running ([`1c-real-engine.txt`](rh02-265/install-b/1c-real-engine.txt)). It exposed #274. |
| Intel | Not available. Nothing is claimed. |
| A host with more than one GPU | Not available in a usable form. Recorded as untested. (The NVIDIA test host lists a second GPU it cannot open: #276.) |

## #264 scenario matrix per host

Rerun in this slice from the committed harness, each on the install's host while the install itself was paused. The original #264 runs are in [that record](2026-09-19-rh02-264-readiness-fault-harness-acceptance.md) and agree with these.

| Row | AMD test host (this ticket) | NVIDIA test host run 1 | NVIDIA test host run 2 | Note |
|---|---|---|---|---|
| preflight | pass | pass | pass | |
| image-clean | pass | pass | pass | |
| fixture | pass | pass | pass | |
| stack | pass | pass | pass | |
| login | pass | pass | pass | |
| nonadmin | pass | pass | pass | |
| seed | pass | pass | pass | |
| baseline | pass | fail | pass | see failure note below |
| 1a | pass | pass | pass | |
| 1b | pass | pass | pass | |
| 1c | pass | unperformed | unperformed | see unperformed note below |
| 1d | pass | pass | pass | |
| 1e | pass | pass | pass | |
| 1 (nested host row cleanup) | pass | pass | pass | |
| 2a | pass | pass | pass | |
| 2b | pass | pass | pass | |
| 3 | pass | pass | pass | |
| 4a | pass | unperformed | unperformed | AMD-only scenario |
| 4a' | pass | unperformed | unperformed | AMD-only scenario |
| 4b | unperformed | pass | pass | NVIDIA-only scenario |
| 4c | pass | pass | pass | |
| 5a | pass | pass | pass | |
| 5b | pass | pass | pass | |
| 6a | pass | pass | pass | |
| 6b | pass | pass | pass | |
| 6c | pass | pass | pass | |
| 6d | pass | pass | pass | |
| 7a | pass | pass | pass | |
| 7b | pass | pass | pass | |
| 7c | pass | pass | pass | |
| 7d | pass | pass | pass | |
| 7e | pass | pass | pass | |
| 7f | pass | pass | pass | |
| 7g | pass | pass | pass | |
| 7h | pass | pass | pass | |
| 8 | pass | pass | pass | |
| 9 | pass | pass | pass | |

Totals: AMD test host 103 pass / 0 fail / 1 unperformed
([install-a/harness-gpu-test-amd.md](rh02-265/install-a/harness-gpu-test-amd.md)).
NVIDIA test host run 1: 95 pass / 1 fail / 4 unperformed
([install-b/harness-gpu-test-nvidia-run1.md](rh02-265/install-b/harness-gpu-test-nvidia-run1.md)).
NVIDIA test host run 2: 96 pass / 0 fail / 4 unperformed
([install-b/harness-gpu-test-nvidia-run2.md](rh02-265/install-b/harness-gpu-test-nvidia-run2.md)).

Unperformed rows and their reasons, quoted from the reports:

- **Row 4b, AMD test host**: "UNPERFORMED 4b: host GPU vendor is 'amd', not nvidia" — a limit of the test setup, not a defect.
- **Row 4a, both NVIDIA runs**: "UNPERFORMED 4a: host GPU vendor is 'nvidia', not amd" — a limit of the test setup, not a defect.
- **Row 4a', both NVIDIA runs**: "UNPERFORMED 4a': host GPU vendor is 'nvidia', not amd" — a limit of the test setup, not a defect.
- **Row 1c, both NVIDIA runs (pre-fault instance)**: "UNPERFORMED 1c: the nested host could not serve a launch before the fault on this host (a pre-fault launch got HTTP 503 code=no_host_available), so a refusal could not have readiness as its sole reason" — a limit of the test setup, not a defect.
- **Row 1c, both NVIDIA runs (recovery instance)**: "UNPERFORMED 1c: recovery launch — the nested host could not serve a launch before the fault either (a pre-fault launch got HTTP 503 code=no_host_available)" — a limit of the test setup, not a defect.

The single run-1 failure was:

> baseline: launch did not reach running within 90s — {"state":"failed","state_detail":"preparing resources and image","error_message":"start source pipeline: source pipeline failed to reach PLAYING: Element failed to change its state","failure_code":null}

It did not recur in run 2, nor in the two #264 runs, and the agent log covering run 1 was not retained. Recorded on #267 as a possibly related data point. It is carried forward as an unexplained single failure, not as a pass.

### Row 1c on NVIDIA hardware against install B's own engine

Row 1c was additionally performed on the NVIDIA test host against install B's
own engine (not the nested/scripted host used for the rest of the matrix),
recorded in
[install-b/1c-real-engine.txt](rh02-265/install-b/1c-real-engine.txt):

1. `23:28:27` — before state captured: host `online`, `runtime_endpoint` `pass`, nothing blocking.
2. `23:28:27` — the engine is frozen with `SIGSTOP` rather than stopped, because the daemon has no live-restore (a stop would have killed every running container instead of just wedging the daemon); dockerd state becomes `Tsl`.
3. `23:30:22` — the agent reports the fault: `runtime_endpoint` now `fail`, `blocking` now lists it. That is about 115 seconds (`23:28:27` → `23:30:22`) for the agent to notice and report the frozen engine.
4. `23:30:22` — a launch while the engine is frozen is refused with HTTP 503 `host_not_ready`, no `Retry-After` header.
5. `23:30:22` — a `PUT` override on `runtime_endpoint` is refused with HTTP 409 `conflict` ("check \"runtime_endpoint\" is enforced by the agent itself; no override lifts it").
6. `23:30:22` — the agent's own `status` field in the host body still reads `"online"` while blocked; readiness, not `status`, carries the fault.
7. `23:30:22` — the engine is thawed (`THAWED`, dockerd state `Ssl`), and the very next poll (`23:30:22`/`23:30:23`) shows `runtime_endpoint` recovered to `pass` with nothing blocking.
8. `23:30:23` — a launch after the thaw succeeds (HTTP 201); by `23:30:25` the session is `running`, and it is still `running` at `23:30:31`.
9. `23:30:31` — the agent container's restart count stayed `0` throughout (`started=2026-09-19T23:22:39.331245977Z`), confirming the freeze/thaw never took the agent process down.

### The baseline caveat

The pre-RH-02 baseline `b134085` refuses **every** launch on these hosts for the reason
fixed in #268, so a scenario that "fails on the baseline" because its launch was refused
proves nothing about RH-02. The unconfounded must-fail evidence in the
[#264 record](2026-09-19-rh02-264-readiness-fault-harness-acceptance.md) is these four:

- no `readiness_gate` on the host body;
- no evidence checks reported;
- no `host_not_ready` from any launch;
- `404` on both override routes.

Two confirmations asked for on one host, both on install B: a proxy failure that RH-02 did
not make blocking still launches (#264 row 5a, rerun here, passed on both hosts), and Fleet
▸ Releases reads the same stored readiness without applying anything
(the release preflight in [`releases-preflight.json`](rh02-265/install-b/releases-preflight.json) and the card in
[`host-body.json`](rh02-265/install-b/host-body.json) carry the same three checks, `updater_socket`,
`updater_stack_dir` and `updater_overlays`, with the same status and the same facts: the
updater commit, the stack directory and its two compose files. The fourth preflight check,
`image_resolvable`, is `unknown` because no release was listed. Nothing was applied. This
shows the preflight agreeing with the stored readiness; it does not prove the code path.)

## Defects and observations filed

None blocked a fresh install. One, #280, is in RH-02: a probe that is wrong on one kind of host. The gate, the refusal and the override all behaved correctly given that wrong answer.

| Issue | What | Bearing on RH-02 |
|---|---|---|
| #272 | The default Vulkan H.264 encoder gives a corrupt picture on the AMD Granite Ridge iGPU; VA is clean. Reproduced on A and C. | Not an RH-02 change, but the media probe passes while the picture is wrong, and it is the first thing an AMD self-hoster would see. |
| #273 | The XFCE desktop image fails on relaunch into a used home: a crash on NVIDIA, a white desktop on the native AMD install. A fresh home always works. | None. The agent cleaned up every failed session. |
| #274 | A hung engine takes about 115 s to reach the readiness report, longer than the gate's 60 s staleness window. | A real gap in "runtime failures appear before launch" for the hung, as opposed to dead, engine. |
| #275 | The #264 harness leaves directories under the agent runtime path after killed-agent scenarios and does not check that path. The #264 record's "nothing remains" missed them. Removed by hand in this slice. | Test tooling only. |
| #276 | A GPU the agent is pinned away from still counts toward the host's advertised encode slots. | Readiness is truthful about it; the capacity display is not. Low priority, a system-container artefact. |
| **#280** | `application_gpu_probe` fails on a healthy NVIDIA host that gets its driver from the container toolkit and not from the Quasar driver volume, and blocks every launch. Found on run D. | **The one defect here that is in RH-02 itself**, and in the configuration the operator docs recommend for NVIDIA. |
| #267 (comment) | One of three NVIDIA harness runs failed its first launch after agent start; not diagnosable, log not retained. | Unknown. |

Ruled out before filing, in the spirit of #264's false alarm: a "missing" app image that a
listing format simply did not show; input that "did not arrive" because a byte counter was
read before it was written; black frames that were a screenshot artefact under a locked
pointer.

## Operator documentation findings

1. **Filed as #277. `deploy/README.md` Part 1 path A never sets `QUASAR_UPDATER_IMAGE` or `QUASAR_STACK_DIR`.** Step 4 ("Pull and start", `deploy/README.md:134-148`) runs only `docker compose -f deploy/docker-compose.yml pull` / `up -d` (plus the NVIDIA overlay); neither of those two variables is mentioned anywhere in Part 1. On both test hosts the pull fails: `Image quasar-updater:latest Error pull access denied for quasar-updater, repository does not exist or may require 'docker login': denied: requested access to the resource is denied` / `pull rc=1` ([install-a/install-log.txt](rh02-265/install-a/install-log.txt) `01-install.txt`, [install-b/install-log.txt](rh02-265/install-b/install-log.txt) `03-pull.txt`). The actual fix — `QUASAR_STACK_DIR=$(cd deploy && pwd)` and `QUASAR_UPDATER_IMAGE=ghcr.io/accreleus/quasar/quasar-updater:latest` — exists only in `docs/upgrading.md:343-348` under "The updater" → "Adding it to an existing install", which Part 1 of `deploy/README.md` never links to. Suggested fix: either set both variables in the step-2 `.env` snippet of `deploy/README.md`, or add an explicit pointer from step 4 to `docs/upgrading.md` "The updater" before the first `pull`.
2. **Filed as #278. The quick start uses `openssl rand` without listing `openssl` as a prerequisite.** `deploy/README.md:77-79` generates all three secrets with `openssl rand -hex 24` / `-hex 32` / `-base64 32`, but neither "Before you start" (`deploy/README.md:18-35`) nor the "Prerequisites in detail" table (`deploy/README.md:356-370`) lists `openssl`. It was absent on both test hosts (Fedora 43 container image), recorded in [install-a/environment.txt](rh02-265/install-a/environment.txt) and [install-b/environment.txt](rh02-265/install-b/environment.txt); the runs generated the secrets from `/dev/urandom` instead. That substitution was made at the keyboard and is not in a log file. Suggested fix: add `openssl` as a row in the prerequisites table (or fall back to `/dev/urandom` explicitly in the documented snippet, since that is what both hosts effectively required).
3. **Filed as #278. The `/dev/kmsg` edit is documented only in the prerequisites table, not at the point of failure.** `deploy/README.md:365` (the prerequisites table) says: "Optional: on a kernel without it, drop the device and the capability from the node-agent service and the `xid_visibility` readiness check reports `skip`, which fails nothing." Step 4 ("Pull and start", `deploy/README.md:134-148`) does not repeat or link to that note. On the NVIDIA test host, the first `up` attempt fails outright: `Error response from daemon: error gathering device information while adding custom device "/dev/kmsg": no such file or directory` ([install-b/install-log.txt](rh02-265/install-b/install-log.txt) `04-up.txt`), and only a second, edited `up` succeeds (`05-up2.txt`). Suggested fix: add a one-line callout in step 4 pointing back to the `/dev/kmsg` prerequisites row when the `up` fails with that specific error text.
4. **Not a documentation defect: choosing a GPU on a host with two.** `deploy/README.md:939-940` says "`QUASAR_RENDER_NODE` can stay unset — the agent binds to the scheduled GPU. Set it ... only to pin a specific GPU on a multi-GPU host." Run C set it to the AMD render node because the run was scoped to the AMD GPU. Leaving it unset on a host with an NVIDIA and an AMD GPU was **not tested**, so nothing is claimed either way.
5. **Filed as #279. Running a second install beside an existing one on the same engine is not documented in `deploy/README.md`.** A search of `deploy/README.md` for guidance on a second, side-by-side install (distinct `COMPOSE_PROJECT_NAME`, `CONTROL_PORT`, `QUASAR_TLS_PORT`, `QUASAR_HEALTH_ADDR`, and the shared host path `/run/quasar-agent`) found no such section — the closest related text is `docs/upgrading.md:310-312`, which only notes that the TLS-volume name depends on `COMPOSE_PROJECT_NAME` "unless the stack directory's name says otherwise," in the context of a single upgrade, not a second install. Run C had to work out all of these from the compose file itself (`project=quasar-rh02c`, `cp_ports=28080,28443`, `health=<addr>:29091`, `render=/dev/dri/renderD129` — [install-c/install-log.txt](rh02-265/install-c/install-log.txt) `05-install.txt`) with no README section to follow. Suggested fix: add a "Running a second install on one host" section to `deploy/README.md` enumerating the variables that must be changed and the `/run/quasar-agent` host path that is shared and must not collide.

No other Unraid-specific step was needed: a single native install would follow the quick start as written, with the appdata path the docs already describe. Run C was deployed with plain `docker compose`, not Compose Manager and not a template.

## Limitations

- **Intel** is externally validated only. Nothing is claimed for it.
- **A native Unraid/NVIDIA install was run (D) and launches only with an override**, because
  of #280. It had no agent-recreate check and no harness run, and it was still running, not
  torn down, when this was written.
- **A host with more than one usable GPU is untested.**
- **TLS:** the gate's refusal, recovery and the agent-enforced `409` were shown over HTTPS on
  a documented install. The classification rows and the override lifecycle were not.
- **The #264 harness was not run on the Unraid host**, for the reason given under run C.
- **A report going stale while the host stays online** is covered by tests only. #274 shows
  one way it happens in practice.
- **The user-facing launch message has not been seen in a real browser.** The refusals here
  were made through the API; the browser sessions in this slice were all successful
  launches. The message text itself is in the API responses captured above.
- **Run B's wizard host step** was captured live and matches the console card, but its
  screenshot shows a LAN address and is not committed; A and C have the wizard text.
- The AMD sessions that count ran on the **VA** encoder, not the default, because of #272.
- Every test host is a system container, so kernel facts on a card are the Unraid host's
  and device-node facts are the container's.

## Open follow-ups

#266 (review follow-ups from the storage and runtime checks), #267 (agent exits 255 on the
first NVIDIA session after start), #269 (resume test flake), #270 (`make test-db`
`pg_isready` race), #271 (admission recheck lock order), quasar-protocol#23 (amendment 11
wording), and from this slice #272 to #276, #277 to #279 (documentation) and #280.

## For the owner: promotion

**Proposed commit for `develop`:** the tip of `initiative/resilient-host-architecture` once
this record has landed on it (the hash is in the closing comment on #265). RH-02's product
code is unchanged since `901058f`.

**What it changes for an existing install**

- Migration `0085_evidence_gated_readiness` runs at the control plane's first boot and is
  **one-way**: a control plane must never again be rolled back below schema 85, or it
  crash-loops on "no migration found for version 85".
- Hosts begin reporting `blocks` on seven checks, so **the gate becomes active**. A host
  that launched sessions yesterday while one of those checks was failing will refuse them
  tomorrow with `503 host_not_ready`. That is the point of RH-02, and it is a behaviour
  change on upgrade day.
- An agent older than the control plane reports no `blocks`; the gate treats such a host as
  having nothing blocking. Mixed versions are safe in that direction.

**Rollback recipe, and what it loses.** Take the Postgres backup in `docs/upgrading.md`
*before* upgrading. To go back: stop the stack, restore that backup, start the previous
images. Rolling back without the restore is not possible. Restoring loses everything
written after the backup: sessions, audit rows, and any readiness overrides (the table
does not exist below 85).

**The first thing an upgrading operator will see.** On Fleet ▸ host and in the setup
wizard, the readiness card gains "can block launches" markers, an observed-at time and a
source on each check, and an override control on the checks the control plane enforces. If
anything marked is failing, launches to that host are refused until it is fixed or
overridden, and the audit log records each override at warn severity.

**What to watch on first boot**

- The control plane reports healthy only after the migration finishes; wait for it.
- On an NVIDIA host whose driver volume must provision, launches are refused for roughly
  15 to 90 s and then recover without action. Do not intervene.
- Look at the card before telling users the upgrade is done: a red marked check is now a
  refusal, not a warning.
- On an AMD host, check the picture of the first stream (#272).
- **On an NVIDIA host that uses the container toolkit and has no Quasar driver volume, expect
  `application_gpu_probe_gpu<N>` to fail and every launch to be refused (#280).** The owner's
  own install on the Unraid host is of this kind. Until #280 is fixed the way through is the
  override on that check, which the audit log records at warn severity.

## What I need from the owner to accept RH-02

1. **A decision on #280 before promotion.** My recommendation is to fix it first: it turns a
   working NVIDIA host into one that refuses every launch on upgrade day, and the fix is
   contained (create the probe container with the same GPU injection a real session gets).
   The alternative is to promote and tell NVIDIA operators to set the override.
2. A decision on #272 before an AMD self-hoster meets it: flip `AMD_AUTO_DEFAULT` to VA, or
   accept it as known. It is not part of RH-02, but it sits in the first-install path this
   increment is about.
3. A decision on whether #274 must be fixed before promotion or can follow.
4. The separate approval to promote to `develop`. This report does not do it.
5. Optional: a window to show the refusal message in a real browser, to retire that
   limitation.

## Gates

See the closing comment on #265 for the `make verify` and `make preflight` output on the
final commit, and the leak scan of the tree and the tracker.

## Models

Host work, preflights, evidence, judgments, the acceptance tables, the promotion section,
issues and commits: Claude Fable 5.1. Drafting of the hardware matrix, the per-host #264
table and the documentation findings from captured artifacts, reviewed line by line:
Claude Sonnet. The `CHANGELOG.md` line and link checking: Claude Haiku.
