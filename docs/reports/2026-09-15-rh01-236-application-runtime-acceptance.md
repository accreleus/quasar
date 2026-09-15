# RH-01 #236 — application runtime acceptance

**Passed: implementation, review, baseline/candidate acceptance and owned cleanup.**

## Change and scope

Ordinary application launch, observation, stop and removal now use Quasar-owned
runtime types backed by Bollard. Realized image defaults, environment, mounts,
devices, GPU access and security requirements are checked before accepting launch.
The application has one lifecycle owner; unmigrated callers retain their existing
CLI paths, without automatic fallback for API-owned containers.

A durable operation identity survives uncertain replies. Final exit/OOM evidence
and bounded stdout/stderr tails are collected before explicit removal. Failed
cleanup retains its obligation and protects writable homes across session IDs.
Startup retires prior owned applications before admission; periodic maintenance
retries pending cleanup. Cancelling observation does not roll back owned work.

This stage includes the minimal stop-error propagation needed to protect affected
callers. Generation-sensitive switching/warmup work remains in #237. Updater
redesign, BuildKit, running-session adoption and full Podman/rootless certification
remain outside scope. Frozen wire contracts are unchanged.

## Reviewed and deployed candidate

| | Source commit | Image |
|---|---|---|
| Baseline, both vendors | `7fc0b58142415c79c2cdb641628c5902a00c9a7a` | `quasar-node-agent:20260915-1205-rh01-235` |
| Candidate, both vendors | `8e2ec2761bba9a450cbf6facbff50b4f87d860a4` | `quasar-node-agent:20260915-1615-rh01-236` |

Baseline image ID:
`sha256:5add43038cb90b0c800134496e422c2df1f9cc8fcaa8b229c987291092e2b7f9`.
Candidate image ID:
`sha256:55a1bed3f9293f5cbb1a6f34d5aef25e95a8eac4c46611e92729b00ecf4e1540`.

The canonical local build took 195 seconds from the reviewed source commit,
using the same pinned base and GStreamer toolchain as #235. No Actions image build
ran. The unchanged contract passed 147 applicable non-GPU assertions and 144
applicable GPU assertions on each vendor, with zero failures. Counts are
conditional; skipped GPU checks in the first run are not claimed as passes.

Each target was idle before its agent was recreated. Only the isolated AMD agent
and the dedicated `gpu-test` agent changed. Agent identity storage, environment,
mounts, devices and service settings were preserved. The locally built image was
transferred to NVIDIA and its exact ID and source label verified. Application
fixture images were held constant for the baseline/candidate comparison.

The AMD deployment verifier initially rejected reordered `Binds` entries after
its realized-mount and environment comparisons passed. The original compose
configuration differed only in the intended agent/Pulse image fields; remaining
command, user, entrypoint and network checks passed. The verifier now compares
bind entries without treating list order as a behavior change. NVIDIA passed the
corrected deployment checks. No deployment rollback or host-wide change was needed.

Bollard is pinned at `0.21.1`. AMD ran Docker 29.8.0 / API 1.56 on
Linux 7.0.0-31; NVIDIA ran Docker 29.7.2 / API 1.55 on Linux 7.1.7-200.
The exact version strings are retained in the sanitized acceptance data.

## Actual session results

The baseline and candidate each passed all 22 cells. The sanitized
[acceptance data](rh01-236/acceptance.json) records the comparison and checks.

Browser screenshots show application-generated content, including native Vulkan
cubes and visible terminal input results. Audio checks require advancing received
packets, duration and energy, plus a live, unmuted playing browser track. Encoder
selection is recorded from session-specific agent logs; device listing or an open
input channel is not used as session proof.

| Session | Actual encoder | Baseline teardown | Candidate teardown |
|---|---|---|---|
| AMD media / VA | `vah264enc` | 10.941 s | 10.708 s |
| AMD media / Vulkan | `vulkanh264enc` | 13.065 s | 10.719 s |
| NVIDIA media | `vulkanh264enc` | 11.309 s | 10.829 s |
| AMD native Vulkan app / VA | `vah264enc` | 12.853 s | 11.144 s |
| AMD native Vulkan app / Vulkan | `vulkanh264enc` | 11.292 s | 11.601 s |
| NVIDIA native Vulkan app | `vulkanh264enc` | 11.439 s | 11.226 s |
| NVIDIA desktop sandbox + native app | `vulkanh264enc` | 11.480 s | 11.222 s |

Times run from stop acknowledgment to terminal state, disappearance of the exact
owned containers, and disappearance of recorded session paths. The bound is
35 seconds. These are bounded acceptance runs, not performance benchmarks.

Additional checks on both vendors:

- **Application-visible input:** mouse dragging selected terminal text; its
  visual delta exceeded idle-frame noise. Keyboard input produced exactly
  `quasar` in the application's readback and visible success message.
- **Identity and managed homes:** the preexisting disposable marker survived
  candidate deployment in the same home source. UID 12345/GID 12346 and saved-file
  ownership were preserved. A concurrent request for that home was rejected with
  `409 home_in_use`.
- **Concurrent isolation:** two browsers rendered content and played audio with
  distinct session sockets/input nodes. Stopping one preserved the other stream.
- **Switching:** same-home generation switch, terminal switch and return passed;
  old container IDs were absent when replacements were accepted and audio
  continued. Sampling cannot exclude sub-sample overlap; interface tests enforce
  cleanup-before-replacement ordering.
- **GPU realization:** DRI access and supplementary groups were preserved.
  NVIDIA retained the exact existing driver-volume identity, destination and
  read-only mode. Native application rendering selected RADV RENOIR on AMD and
  the RTX 5090 on NVIDIA, with software renderers excluded.
- **NVIDIA desktop compatibility:** with the existing desktop security policy,
  the application observed the NVIDIA proc-params mount removed, ran a nonroot
  bubblewrap sandbox, and rendered its native cube with browser audio. Realized
  masked/read-only system-path lists were empty as required.
- **Failed launches:** exit 37, missing executable/exit 127 and a forbidden mount
  failed as expected and left no owned app containers or recorded session paths.
  The candidate preserved both `RH01-236-final-stdout` and
  `RH01-236-final-stderr`; the baseline had lost them to its removal race.

Readiness uses the unchanged 300-second watchdog. Baseline totals were 316.198 s
on AMD and 314.350 s on NVIDIA, including cleanup, with the final readiness marker
retained. Candidate totals were 311.831 s on AMD and 313.108 s on NVIDIA,
with `app_never_presented`, the final marker retained, and no remaining owned
containers or session paths.

## Checks and review

- `make verify`: 428 passed, zero warnings/failures.
- `make test-rust`: 1,483 library tests, six binary tests, three logging convention
  tests, one provision test and benchmarks passed. Docker/hardware fixtures are
  explicitly opt-in. Two existing build fixtures initially failed under full-suite
  contention, passed a focused 13-test run, and passed subsequent full runs.
- The guarded real-Docker application smoke passed create/start, exit 0, final-log
  retention and exact owned cleanup using an existing local Alpine image. No
  owned container or pending journal remained. GPU sessions above are separate
  evidence.
- Independent specification review used `gpt-5.6-sol`; the bounded implementation
  worker used `gpt-5.6-terra`. The primary reviewed standards/correctness and owned
  all commits, builds, deployment and evidence. Reviews used frozen snapshots.

Resolved review defects included image/default fidelity, realized requirement
verification, safe retirement after rejected requirements, lost-reply ownership,
monotonic cleanup, bounded final evidence, NVIDIA repair reconciliation, retryable
stop failures, cross-session home protection, startup ordering and periodic
recovery. Regression tests cover ownership collisions, cancellation during owned
mutations, uncertain create/remove, cleanup recovery, readiness/observer behavior,
pre-submission `Busy`/unavailable outcomes and CLI exclusion of API-owned apps.
A missing warning token was fixed before the final passing check. No source review
finding remains open.

## Cleanup, evidence and limitations

All 18 uniquely owned catalog fixtures and both disposable managed homes were
removed after acceptance. Unrelated catalog entries, including the preexisting
AMD app, were checked and preserved. Temporary AMD Vulkan settings were restored;
owned remote contract-test files were removed. The candidate compose overlays
remain because the deployed agents use them. Both targets are idle and registered
on the candidate. No owned application/audio container remains; all 17 AMD and
15 NVIDIA application journal records are `Completed`. No cleanup obligation is
pending. User identity storage and unrelated data/workloads were preserved.

Evidence:

- [Baseline/candidate data and cleanup proof](rh01-236/acceptance.json)
- [AMD VA content](rh01-236/candidate-amd-va.png), [AMD Vulkan content](rh01-236/candidate-amd-vulkan.png), [NVIDIA content](rh01-236/candidate-nvidia.png)
- [AMD native rendering](rh01-236/candidate-nativegpu-amd.png), [AMD native rendering with Vulkan encoding](rh01-236/candidate-nativegpu-amd-vulkan.png), [NVIDIA native rendering](rh01-236/candidate-nativegpu-nvidia.png)
- [NVIDIA desktop sandbox rendering](rh01-236/candidate-nativegpu-desktop-nvidia.png)
- [AMD application input](rh01-236/candidate-input-amd.png), [NVIDIA application input](rh01-236/candidate-input-nvidia.png)

Corresponding baseline screenshots are stored alongside these files. Screenshots
were visually inspected; the JSON carries the encoder, audio, application input,
GPU-realization and teardown evidence. Private credentials, host addresses,
operator paths and raw engine configuration are excluded.

Intel hardware is unavailable. Existing Intel paths and the
[external validation procedure](rh01-intel-external-validation.md) are unchanged;
no Intel hardware certification is claimed. The agreed AMD/NVIDIA scope is
unaffected by this limitation.

Work remains on `feature/rh-01-runtime-api`. No merge into develop/main, release
tag or promotion occurred. The next implementation issue after acceptance is
#237; it is not started by this report.
