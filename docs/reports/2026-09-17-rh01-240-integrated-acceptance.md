# RH-01 #240 integrated acceptance

**Result: PASS for the agreed Docker / AMD / NVIDIA scope. Promotion remains
subject to explicit owner approval.** No promotion to develop/main is performed.

## Candidate and baseline

The runtime and control-plane images were built from reviewed source
`71e77f8d02acf6dfbf51abaee40efb16dd349910`. Its production node-agent tree is
identical to accepted #239 (`fc5505806d4f5f37d3af455b4ebf14e7692b18fb`); its
control-plane tree is identical to the reviewed #251 implementation. This ticket
adds an opt-in Docker interface test, recovery instructions and acceptance evidence;
it does not change production code or frozen contracts. The final delivery commit
therefore identifies the tests/report, while the source above identifies the deployed
binaries. Do not infer a different image source from the delivery commit.

Both targets were inspected live before deployment: stack identity, control-plane
source/image/schema, agent source/image, engine/client versions, settings and sessions.
Both were idle, online and already at schema 84; there was no schema mismatch to repair.
The baseline used #239's known-good agent and #251's control plane (`64083f2`).
Private records retain full Compose/environment/mount identities; public evidence
uses target roles and omits credentials, network addresses and host paths.

| Component | Exact image ID |
| --- | --- |
| Candidate agent/Pulse, both targets | `sha256:9f6eef5b8dc56abcc5c659a987d58bacf64a14691849731727fa777f33394779` |
| Known-good #239 agent | `sha256:2d6455dd0d37ae553ccd5ca65f0e520884d20263b9f5ca4f50cfc4012bc7012b` |
| Deployed AMD control plane, schema 84 | `sha256:991f025fed053ab6e4ac855060a5ec8983ba2d593593cd7f38207cac4103e1d1` |
| Deployed NVIDIA control plane, schema 84 | `sha256:6de704f00930354d3549ac45acdb6effefb471da79df82d92d5e410387628acf` |
| Fresh-stack production control image, schema 84 | `sha256:f7b4726c197c7d87dd906e7ee81384ca38bfe4c4bc05b5dbc2941d6fddf1ee65` |

Agent tag: `quasar-node-agent:20260917-0813-rh01-240`. All candidate control images
also report source `71e77f8d02acf6dfbf51abaee40efb16dd349910`. The two existing
development stacks use the canonical control-only deployment path; the fresh stack
uses `quasar-control-plane:20260917-0819-rh01-240` from the production Dockerfile.

| Target | Docker server / CLI | Server maximum API | Bollard / negotiated API |
| --- | --- | --- | --- |
| Isolated AMD | 29.8.0 / 29.8.0 | 1.56 | 0.21.1 / 1.53 |
| gpu-test NVIDIA | 29.7.2 / 29.7.2 | 1.55 | 0.21.1 / 1.53 |

Images were built locally through `deploy/build-images.sh`, with pinned published
base digest `sha256:ee45e721c7f7310daad9d2dbb3ab7831385155d081b3da460a679dbadc0f4b6a`,
no pruning, no latest/legacy aliases and no Actions build. The candidate was
transferred to NVIDIA and its immutable ID/source checked before deployment.
The unchanged runtime contract passed: no-GPU 148/0 (GPU elements explicitly
skipped), AMD 146/0, NVIDIA 145/0. These are the actual conditional assertion counts,
not a fabricated 150/150. Agent images contain neither Docker nor Podman executables.

## Repository and real-engine checks

| Check | Result |
| --- | --- |
| `make verify` | PASS, 428 checks, zero warnings/failures |
| `make test-rust` | PASS, 1,497 library tests plus integration suites; opt-in hardware tests run separately below |
| `make test-go` | PASS |
| `make test-db` | PASS, fresh PostgreSQL, uncached DB tests actually executed |
| `make preflight` | PASS on source `71e77f8` plus the final reviewed Docker test; same 428-check verification |
| #251 coordinator/store regression and registry race repetition | PASS |
| Existing opt-in runtime Docker library tests | 5 PASS: discovery, host-path nonce, application lifecycle, classic build success/failure/arguments, pull/offline reuse/in-use refusal/removal |
| `runtime_helpers_docker` | PASS: bind/log/exit/cancel/stop/restart recovery |
| `runtime_inspection_docker` | PASS: metadata, foreign live/stopped mounts, unavailable engine, poisoned local liveness, both managed-home reapers |
| New `runtime_application_docker` | PASS, 10.43 s: successful/nonzero exit and final logs, observation cancellation without rollback, explicit stop, cleanup, home contents, rejected missing bind without creating a host path |

The new ignored test runs in the development container against an explicit socket,
existing image and daemon-host checkout path using `QUASAR_TEST_RUNTIME_SOCKET`,
`QUASAR_TEST_RUNTIME_IMAGE` and `QUASAR_TEST_RUNTIME_HOST_ROOT`. Its cleanup guard
tracks intent before submission and retains state/home evidence if cleanup is unproven.
Quasar-owned interface tests also cover ownership collisions, rejected device/security
requirements, uncertain outcomes and stable identities; the live cells below establish
actual Docker and GPU behavior. A missing-bind engine rejection alone is not claimed
as complete capability-negotiation coverage.

## Baseline and candidate sessions

Both baseline and candidate displayed actual application content, received browser
audio with advancing packet/duration/energy counters and active playback, and passed
application-visible input. The input fixture received `quasar` plus Enter inside the
application; mouse drag visibly selected terminal text. Evidence includes decoded
screenshots, luma samples, application renderer identity and session-specific encoder
selection. Device enumeration or black-frame decoding was not used as a pass criterion.

| Cell | AMD | NVIDIA |
| --- | --- | --- |
| Actual encoding | `vah264enc` and separately forced `vulkanh264enc` | `vulkanh264enc` |
| Application Vulkan rendering | AMD Radeon Graphics, RADV RENOIR | NVIDIA GeForce RTX 5090 |
| Existing NVIDIA driver volume | N/A | Same read-only driver-volume identity as baseline |
| Concurrent sessions | Two input applications, both content/audio/keyboard/mouse; distinct session paths/devices | Same |
| Stop one concurrent session | Other keeps running/decoding; stops 11.212 / 12.725 s | Other keeps running/decoding; stops 11.125 / 10.711 s |
| Three application switches | No running-generation overlap, correct same/different home selection; browser survives | Same |
| Failed replacement, exit 37 | Rolls back to prior application, home retained | Same; exit also established by application log when polling missed its short lifetime |
| Failed initial launch, exit 37 | Final stdout/stderr retained; container/runtime paths removed | Same |
| Legacy ownership sweep | Owned legacy fixtures removed; foreign, unlabelled and API-owned fixtures preserved | Same |

Settings used the existing 720p60 launch profile and effective H.264 configuration.
AMD retained its 900-second application boot override; NVIDIA retained its existing
home-root and zero-copy overrides. The separate AMD Vulkan cell set `encoder=vulkan`,
verified the effective setting and actual encoder, then restored the exact overrides.
Application fixture image `quasar-node-agent:20260915-2256-rh01-237` was deliberately
held constant across baseline/candidate; the candidate agent and Pulse changed.

A separate fresh AMD Compose project used a new database, control-plane state,
agent identity, journals, runtime paths and managed-home roots. It registered at
schema 84, rendered/encoded content with browser audio, enforced duplicate-home
exclusion and preserved UID/GID 12345/12346. Teardown was 12.161 s. Its containers,
volumes and exact owned paths were removed; the existing AMD stack was preserved.

## Failure and recovery evidence

- **Engine endpoint outage:** a narrowly owned Unix proxy cut access for 12 seconds.
  A running app remained running with rendered content/audio. An app killed during
  the cut was not falsely reported absent: exit 137 became visible 1.0 s after
  endpoint recovery, with cleanup 0.8 s later. Stopping during the cut produced a
  terminal row while its container still existed; durable cleanup completed after
  recovery (30.887 s from stop). The host Docker daemon was never stopped.
- **Lost mutation replies:** the proxy forwarded an application create, observed
  Docker's 201, discarded the reply and cut observation; later it did the same for
  a removal acknowledged by Docker with 204. Both remained nonterminal in durable
  journals until reconciliation. Each operation had exactly one create submission;
  cleanup completed under its original identity. The home marker survived. No fresh
  operation identity or CLI fallback was used to resolve uncertainty.
- **Agent interruption:** SIGKILL of only the isolated agent after launch submission
  and during teardown, followed by restart of that same container, retired owned
  work. All application journals completed; node identity, owner state, home marker
  and a foreign sibling remained intact. Launch and interrupted teardown reached
  `failed/host_lost` through the existing disconnect reaper. This is distinct from
  the #251 heartbeat rule below. Recorded leftover per-session boot runtime paths
  were removed only after independent container/journal proof.
- **#251 retained regression, both targets:** running → reconnect → stop before
  first heartbeat → explicit omission reached `stopped/host_lost` in 5.135 s AMD /
  4.868 s NVIDIA, with one registration and no further reconnect or application
  failure fields. Container absence and completed cleanup were checked separately;
  the preserved home was then reused. Slow teardown remained `stopping` across a
  heartbeat, and duplicate-home admission returned 409. Coordinator/store tests
  cover missing versus empty lists, stale connection fencing, concurrent transitions,
  duplicate reconciliation and terminal reporting.
- **Known-good return, both targets:** drained idle candidate → recorded #239 agent
  → candidate. Both launches read the preserved home marker. Host/node identity and
  persistent mounts were unchanged. All four stops took approximately 10.7–11.5 s.
  The schema-84 control plane remained in place throughout; the old schema-80
  control image was never used against the upgraded database.

The [recovery guide](../runtime-api-recovery.md) documents the cutover, drain,
independent cleanup checks and return procedure. Heartbeat absence never proves
container deletion. Agent pending-operation and startup-retirement gates remain intact.

## Review, corrections and final state

One authorized Terra worker supplied the bounded test file. The primary reviewed
fixed diffs for correctness/standards; Sol independently reviewed scope and the
corrected test. Review found three test defects: missing unwind cleanup/state
retention, a non-observing host-path assertion and unbounded raw socket reads.
All were returned to the same worker, corrected, re-reviewed and rerun against Docker.
Final test snapshot SHA-256:
`1d9984605279622a2a0e655360e712ae555ab7e966ffda1c2a176e46915a4614`.

Harness setup failures were retained privately and excluded from passing evidence:
an unpublished default toolchain reference, an overly strict fresh-template assertion,
a proxy Python import-path collision, and inspection fixtures using the wrong image
or competing checkout mounts. Corrected runs passed. One exited container from the
initial test lacking an unwind guard required operator API cleanup after exact ID,
owner, operation and image verification; that action is not claimed as runtime
recovery evidence. No production defect or unresolved review blocker was found.

Both targets finish idle and online on the candidate image and schema-84 control
planes above. All-user active-session queries return zero; application journals are
Completed, as are all helper journals (127 AMD / 2,261 NVIDIA). Ten disposable catalog assets and their two managed homes were removed.
The two AMD and five NVIDIA unrelated catalog entries, persistent agent mounts,
identity and temporary settings were verified preserved/restored. Owned proxies and
the fresh test deployment were removed. No broad prune, driver change, Docker restart
or release tag occurred.

## Evidence and promotion decision

[Sanitized evidence directory](2026-09-17-rh01-240-evidence/) contains baseline and
candidate summaries, browser/audio/input/renderer measurements, screenshots, contracts,
lost-reply events, reconnect/cleanup/recovery results and final preservation checks.
Raw tokens, URLs, private host paths, full engine inspections and journals remain private.

**Recommendation: approve promotion of the integrated RH-01 candidate after reviewing
this evidence.** The next action is explicit owner approval specifying the promotion
target; this ticket does not authorize that promotion. No next issue is started.

Limits: bounded development-host acceptance, not a soak or fleet guarantee. Intel
hardware was unavailable; its existing paths and [external procedure](rh01-intel-external-validation.md)
are preserved without certification. Podman/rootless certification, updater redesign,
running-session adoption, TURN and roaming remain outside this acceptance.
