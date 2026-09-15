# RH-01 #237 — application switching, warm-up and teardown acceptance

**Passed: implementation, review, baseline/candidate acceptance on both vendors and owned cleanup.**

## Change and scope

Application switching, warm-up and their teardown paths are now generation-safe on the
owned runtime API. Each app-container launch attempt owns its own observation record
(exit slot and log ring), installed before the engine is asked for a container and handed
only to that generation's observer thread. A previous generation's late exit or dying log
lines can never be reported for the replacement, including on a rolled-back swap that
relaunches the previous app into the same source; an exit is also discarded at read time
once the agent's own teardown of that generation has begun. A replacement that exits
before presenting fails the swap immediately with its exit status instead of sitting out
the 45 s readiness budget as "no frame".

The Steam warm-up resolves the image's container home through one runtime metadata read
(`Config.Env HOME`, then `Config.WorkingDir`) instead of a `docker image inspect`
subprocess. Stopping a warm-up session is fallible: after the runner joins, the exact
pending application operation for the scratch home is retried a bounded number of times
and, if still unresolved, the job fails as *teardown unproven*. Nothing is snapshotted,
verified or published; the previous template is untouched; the staging tree with the
scratch home is retained rather than removed under a container that may still hold it.
Unreadable pending state is unproven, never proof. Restart-durable recovery, the startup
name-prefix sweep and the remaining dead CLI helpers stay in #239. Frozen wire contracts
are unchanged.

## Reviewed and deployed candidate

| | Source commit | Image |
|---|---|---|
| Baseline, both vendors | `8e2ec2761bba9a450cbf6facbff50b4f87d860a4` (#236) | `quasar-node-agent:20260915-1615-rh01-236` |
| Candidate, both vendors | `e1d551c9f5595bdfb275a62c03187a21f18a85ec` | `quasar-node-agent:20260915-2256-rh01-237` |

Baseline image ID `sha256:55a1bed3f9293f5cbb1a6f34d5aef25e95a8eac4c46611e92729b00ecf4e1540`.
Candidate image ID `sha256:1b862aa3c5b67baf0e79b2fe5f77702d112ec6b574729e6015e54f528c893d68`.

The canonical local build (`deploy/build-images.sh runtime --git-ref <commit>`) took
204 seconds from the reviewed commit on the same pinned base and GStreamer toolchain as
#235/#236. No Actions image build ran. The unchanged contract passed 147 non-GPU
assertions at build time, 145 GPU-attached assertions on AMD (with `vah264enc` required)
and 141 GPU-attached assertions on NVIDIA, zero failures. Each target was idle before its
agent was recreated; only the isolated AMD agent and the dedicated `gpu-test` agent
changed, with mounts, environment and service settings preserved. The image was
transferred to NVIDIA and its exact ID and source label verified there.

Bollard is pinned at `0.21.1`. AMD ran Docker 29.8.0 / API 1.56 on Linux 7.0.0-31;
NVIDIA ran Docker 29.7.2 / API 1.55 on Linux 7.1.7-200.

## Actual session results

The baseline and candidate each passed all 14 cells (seven per vendor: media, terminal
input, native Vulkan rendering, two concurrent sessions, a three-step switch, a failed
replacement, and a Steam warm-up). The sanitized [acceptance data](rh01-237/acceptance.json)
records every cell. Browser screenshots show application-generated content: the SMPTE
pattern, native Vulkan cubes rendered by RADV RENOIR on AMD and the RTX 5090 on NVIDIA,
and visible terminal text selection. Audio checks require advancing received packets,
duration and energy plus a live, unmuted playing track. Encoder selection is read from
session-scoped agent logs.

| Session | Actual encoder | Baseline teardown | Candidate teardown |
|---|---|---|---|
| AMD media / VA | `vah264enc` | 12.777 s | 12.664 s |
| NVIDIA media | `vulkanh264enc` | 11.088 s | 10.827 s |
| AMD native Vulkan app / VA | `vah264enc` | 11.549 s | 12.636 s |
| NVIDIA native Vulkan app | `vulkanh264enc` | 11.215 s | 11.134 s |
| AMD terminal app with input | `vah264enc` | 12.157 s | 12.628 s |
| NVIDIA terminal app with input | `vulkanh264enc` | 11.101 s | 11.062 s |

Times run from stop acknowledgment to terminal state, disappearance of the exact owned
containers and of the recorded session paths; the bound is 35 seconds.

**Switching.** On both vendors the same-home generation switch, the switch to the
terminal app and the switch back passed on baseline and candidate: no sampled overlap of
running app containers, the old generation's exact container absent when the replacement
was accepted, the same managed-home source across generations, and one browser decoding
and playing audio across all three switches (candidate teardown 11.355 s AMD,
4.484 s NVIDIA).

**Failed replacement.** Switching to an app that exits 37 immediately rolled back on
both vendors, on baseline and candidate: the session stayed `running` throughout, the
previous app was relaunched as a fresh container under its own generation name with the
same managed home, the failed generation's container was gone, the browser kept decoding
and playing audio, and the only exit line for the session was the replacement's own
generation reporting exit 37. What changed is how the failure is reported and how long
the rollback takes:

| | Baseline | Candidate |
|---|---|---|
| AMD rollback detail | `swap failed; rolled back: replacement app produced no frame within 45s (app_surface_commits=Some(0)); previous app was restarted` | `swap failed; rolled back: replacement app exited before presenting (Code(37)); previous app was restarted` |
| AMD swap request → rollback | 59.1 s | 11.3 s |
| NVIDIA swap request → rollback | 56.7 s | 11.8 s |

The candidate's ~11 s is the previous app's bounded stop plus the replacement's start and
exit; the baseline sat out the 45 s readiness budget and reported "no frame". On NVIDIA
the candidate's replacement lived for less than one 500 ms sample, so its identity is
proven by the agent's generation-1 exit line rather than by sampling.

**Concurrent isolation.** Two browsers rendered content and played audio at once with
distinct session mounts and input nodes; stopping the first preserved the second
(candidate teardowns 10.782, 11.237 s AMD, 10.934, 11.138 s NVIDIA).

**Warm-up.** A real Steam golden-home build ran through the admin job API on both
vendors, on baseline and candidate, from an empty template root (the NVIDIA host's
existing template was removed first so the run was a rebuild, and was restored by it):

| | Presented after | Build | Files | Template | Staging after publish |
|---|---|---|---|---|---|
| AMD baseline | 90.1 s | 251 s | 23696 | 2632 MiB, copy | empty |
| AMD candidate | 84.6 s | 216 s | 23696 | 2632 MiB, copy | empty |
| NVIDIA baseline | 37.3 s | 125 s | 23701 | 2632 MiB, reflink | empty |
| NVIDIA candidate | 39.8 s | 144 s | 23701 | 2632 MiB, reflink | empty |

Each run published `.meta.json` (schema 1, image id, version, byte and file counts that
match the job summary), left the staging tree and scratch home removed, left no warm-up
container and no pending cleanup obligation, and reported `ready` with "Matching
sanitized template published". No teardown-unproven token appeared.

Two environment facts shaped the AMD warm-up. The isolated AMD agent's home and
template roots were on a 3.4 GB tmpfs, below the warm-up's 20 GiB free-space floor, so
before the AMD warm-up cells the agent was recreated once on the baseline image with both
roots moved to disk-backed paths and the two warm-up knobs enabled; that change is
recorded in the acceptance data and is the only environment change. A first AMD baseline
warm-up, run while the candidate image was compiling and the Rust suite was running on
the same box, hit the 300 s boot watchdog while Steam's first-run update was still
extracting; its teardown then returned an uncertain stop, the obligation stayed pending
and periodic maintenance completed it. Re-run on an idle box, Steam presented in 90 s.
The AMD warm-up cells therefore ran with the host's `app_boot_timeout_secs` override set
to 900 through the admin settings API and restored afterwards; NVIDIA needed no override.

## Checks and review

- `make verify`: 428 passed, zero warnings, zero failures.
- `make test-rust`: 1,492 library tests, six binary tests, three logging-convention tests
  and one provision test passed; eight explicitly ignored fixtures; benchmarks passed. An
  earlier full run, taken while the candidate build and the acceptance browsers shared the
  box, had 17 fake-engine fixtures time out with `UnknownOutcome`; every one passed in the
  quiet re-run and in focused runs, and none touches the changed code.
- Focused suites during development: 132 warm-up, container, source and inspection tests.
- Independent specification review used `gpt-5.6-sol` across five passes; the bounded
  warm-up implementation slice used `gpt-5.6-terra`. The primary reviewed the fixed diffs,
  owned every commit, build, deployment and this evidence.

Resolved review defects: a poisoned pending-state lock read as "no writer" (now unproven),
an unproven teardown that still discarded the staging tree (now retained, new
`WarmupError::TeardownUnproven`), the observer's check-then-publish window against the
intentional-stop marker (now re-checked at read time), teardown proof retrying unrelated
pending operations (now only the matching one, bounded), two separate image inspections
for HOME and WorkingDir (now one metadata read and a pure resolver), inconsistent log
tokens, and a swap that consumed a replacement's exit before its readiness gate (now
readiness first, so an app that presented and then exited follows its exit policy). One
review demand was declined with a recorded reason: quarantining host admission after an
unresolved warm-up contradicts the warm-up module's own precedence that a user launch
always wins; the exact operation is retried, the launch-time pending guard blocks any
later writer of that path, and periodic maintenance keeps retrying.

Regression tests at Quasar-owned seams cover: a previous generation's late exit never
reported for the replacement, an exit published just before the stop marker discarded at
read time, previous-generation log lines never in the replacement tail, warm-up home
resolution rules, pending-writer lookup by normalized source, bounded teardown proof that
retries only the matching operation, and a job that never snapshots, publishes or
discards after an unproven teardown. The swap loop itself is not unit-testable without a
compositor; its failed-replacement behaviour is the live cell above.

## Cleanup, evidence and limitations

All eleven owned catalog fixtures (six AMD, five NVIDIA) and their three managed homes
were removed after acceptance; the unrelated catalog entries on both stacks were checked
and preserved. The Steam image, its preparation setting and its template were only ever
installed on the isolated AMD stack by this acceptance and were removed through the
uninstall API, which the agent confirmed by removing the template; the NVIDIA host keeps
the Steam install, preparation setting and rebuilt template it had before. The AMD
boot-timeout override was restored after each warm-up cell. The isolated AMD agent keeps
its disk-backed home and template roots and the candidate overlay, since the deployed
agent uses them. Both targets are idle, registered on the candidate commit, hold no
owned application or audio container, and every application journal record on each
(47 per host) is `Completed`. User identity storage, unrelated data, volumes and
workloads were preserved; no Docker restart, driver change, prune or host-wide fault
was applied.

Evidence:

- [Baseline/candidate data, deployments, contracts, build and the AMD roots change](rh01-237/acceptance.json)
- Candidate screenshots: [AMD media](rh01-237/candidate-media-amd-stream.png), [NVIDIA media](rh01-237/candidate-media-nvidia-stream.png), [AMD native rendering](rh01-237/candidate-nativegpu-amd-stream.png), [NVIDIA native rendering](rh01-237/candidate-nativegpu-nvidia-stream.png), [AMD input](rh01-237/candidate-input-amd-mouse-selection.png), [NVIDIA input](rh01-237/candidate-input-nvidia-mouse-selection.png), [AMD after a failed replacement](rh01-237/candidate-failedswap-amd-stream.png), [NVIDIA after a failed replacement](rh01-237/candidate-failedswap-nvidia-stream.png), [AMD across switches](rh01-237/candidate-swap-amd-stream-0.png), [NVIDIA across switches](rh01-237/candidate-swap-nvidia-stream-0.png)

Corresponding baseline screenshots are stored alongside. Screenshots were visually
inspected; private credentials, host addresses, operator paths and raw engine
configuration are excluded.

Limitations: the pending-operation map is process memory, so restart-durable recovery of
an unproven teardown remains #239 as the issue assigns it; managed-home identity is
matched by lexically normalized path, not by filesystem identity. Intel hardware is
unavailable; the existing Intel paths and the
[external validation procedure](rh01-intel-external-validation.md) are unchanged and no
Intel certification is claimed.

Work remains on `feature/rh-01-runtime-api`, integrated into
`initiative/resilient-host-architecture`. No merge into develop/main, release tag or
promotion occurred. The next implementation issue is #239; it is not started by this
report.

