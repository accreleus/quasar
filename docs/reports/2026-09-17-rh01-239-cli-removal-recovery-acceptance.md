# RH-01 #239 — engine-CLI removal and recovery coverage acceptance

**Passed: implementation, two independent review rounds, and live acceptance on both vendors
(twelve AMD cells, nine NVIDIA cells) on the final candidate, with owned cleanup.**

## Change and scope

Every node-agent runtime caller now reaches the container engine only through the
Quasar-owned runtime API (Bollard over the configured Unix socket). The last caller on the
CLI, the boot-time sweep of pre-API siblings, is `RuntimeClient::retire_legacy_containers`:
it lists containers by this agent's owner label, re-inspects each by immutable ID, removes
one only when the exact owner label and an allowed name prefix both hold, preserves and
counts everything else (a foreign owner, an unlabelled container, an API-owned application,
an audio sidecar), and leaves a removal it cannot prove for the next boot instead of retrying
it in the same pass. The dead CLI paths (`run_raw`, `spawn_log_follower`, `graceful_remove`,
`force_remove`, `wait_for_exit`) and their bounded-exec plumbing are gone, as are the vestigial
`docker: &str` parameters threaded through the NVIDIA driver-volume and readiness code.
`QUASAR_CONTAINER_RUNTIME` is retired: it selects nothing and produces one
`runtime-cli-knob-retired` warning. `DOCKER_HOST` (a `unix://` absolute path) is the only
endpoint knob. A source-level guard (`node-agent/tests/engine_subprocess_convention.rs`) fails
the build on any reintroduced `docker`/`podman` shell-out and on any child process in the
launch path or the runtime module other than the documented registry credential helpers.

The application observer loop is extracted as `observe_until_exit` with its engine calls
injected, so an engine gap is provably "ask again", never an exit, and the true exit publishes
exactly once per generation. Observation is inspect-polling by exact container identity; there
is no event stream and no subscription is treated as a lifecycle ledger.

The runtime image no longer installs the static Docker CLI. `deploy/image-contract.json` drops
`docker` from the runtime role's required binaries and forbids `docker` and `podman` there;
every other assertion is unchanged. The dev image keeps the CLI as contributor tooling and the
updater keeps its own image, CLI and Compose workflow. `docs/configuration.md` documents the
endpoint, the exact boot recovery order, the endpoint-change semantics and the known-good
rollback path. Frozen wire contracts are unchanged.

### Caller inventory (issue #225), final disposition

| Caller | Runtime API operation |
|---|---|
| Startup cleanup (pre-API `quasar-sess-*` siblings) | `retire_legacy_containers` (this issue) |
| Startup retirement of API-owned applications, audio sidecars, diagnostics | `retire_applications`, `retire_audio_sidecars`, `recover_diagnostics` |
| Application launch / observe / log tail / stop / cleanup | `start_application`, `observe_application`, `application_log_tail`, `stop_application`, `cleanup_application`, `abandon_application` |
| Audio sidecar | `run_audio_sidecar`, `observe_audio_sidecar`, `stop_audio_sidecar`, `cleanup_audio_sidecar` |
| GPU/driver helpers (host-path probe, sibling EGL, lib32 probe) | `run_diagnostic`, `run_nvidia_gpu_diagnostic` |
| Managed images (presence, pull, build, removal, disk) | `image_present`, `ensure_image`, pull/build/remove operations, `engine_storage` |
| Diagnostic metadata (install facts, Compose labels) | `inspect_container`, `inspect_image_metadata` |
| Warm-up (container home resolution, launch, teardown proof) | `inspect_image_metadata` + the application operations |
| Home-liveness discovery (tracked-home GC, throwaway sweep) | `live_containers`, `inspect_container` |

Non-engine subprocesses (`tar`, `ldconfig`, `cp`, `sh` probe scripts, `seatd`, `weston`,
`ddcutil`, `hostname`, `nvidia-smi`, `vulkaninfo`, `nft`/`iptables`) are unchanged and out of
scope. The Docker credential helpers (`docker-credential-<helper>`) are registry tooling, not the
engine, and stay.

## Reviewed and deployed candidate

| | Source commit | Image |
|---|---|---|
| Final candidate, both vendors | `fc5505806d4f5f37d3af455b4ebf14e7692b18fb` | `quasar-node-agent:20260916-1638-rh01-239e` |
| Known-good image for the rollback cell | `e1d551c9f5595bdfb275a62c03187a21f18a85ec` (#237) | `quasar-node-agent:20260915-2256-rh01-237` |

Candidate image ID `sha256:2d6455dd0d37ae553ccd5ca65f0e520884d20263b9f5ca4f50cfc4012bc7012b`.
Built with `deploy/build-images.sh runtime --toolchain registry --no-prune --git-ref <commit>`
on the published toolchain artefact; no Actions image build ran. The build-time contract passed
146 assertions (the set differs from #237 exactly by the intended change: `binary.docker`
required is gone, `binary-absent.docker` and `binary-absent.podman` are new). The GPU-attached
contract passed 146 assertions on AMD (`vah264enc` required) and 145 on NVIDIA, zero failures,
using the new validator and contract copied to each host. Each target was idle before its agent
was recreated; only the isolated AMD agent and the dedicated `gpu-test` agent changed, with
mounts, environment and service settings preserved. The image was transferred to NVIDIA and its
exact ID and source label verified there.

Bollard stays pinned at `0.21.1`. AMD ran Docker 29.8.0 / negotiated API 1.53 on Linux
7.0.0-31; NVIDIA ran Docker 29.7.2 on Linux 7.1.7-200.

Three earlier candidates were built and two of them deployed during this acceptance; each was
superseded by a fix for a defect the live exercise found (below). Every cell reported here ran
on the final candidate.

## Actual results

The sanitized [acceptance data](rh01-239/acceptance.json) records every cell. Screenshots show
application-generated content on both vendors: the SMPTE pattern, native Vulkan cubes rendered by
RADV RENOIR on AMD and the RTX 5090 on NVIDIA, and visible terminal text selection. Audio checks
require advancing received packets, duration and energy plus a live, unmuted playing track.

**No engine executable.** On both targets, `command -v docker` and `command -v podman` inside
the running agent find nothing, `/usr/local/bin/docker` and `/usr/bin/podman` are absent, the
agent has logged `runtime-engine-discovered`, and the host is online on the candidate commit.

**Legacy sweep.** On both targets five containers were created with the host's own engine CLI:
an owner-labelled running `quasar-sess-legacy239-*`, an owner-labelled stopped one, a same-prefix
container with a foreign owner, an unlabelled one, and an owner-labelled one carrying the API
application label. After an agent restart (same container, same node secret) the two owned legacy
containers were gone and the other three were present; the agent logged
`legacy-container-sweep-summary removed=2 preserved=1 unresolved=0` (the foreign and unlabelled
containers never match the owner-label listing, so only the API-labelled one is counted).

**Streaming, input, rendering, isolation, switching.**

| Cell | AMD | NVIDIA |
|---|---|---|
| media (SMPTE, encoder / teardown) | `vah264enc`, luma 127.5, audio pass, 11.375 s | `vulkanh264enc`, luma 127.3, audio pass, 11.068 s |
| input (terminal, mouse selection) | selection pass, 10.955 s | selection pass, 11.091 s |
| native Vulkan app (renderer) | RADV RENOIR, 10.815 s | RTX 5090, 11.027 s |
| two concurrent sessions | distinct mounts, second survives first stop; 10.824 / 10.731 s | 11.186 / 11.102 s |
| three-step switch | browser decoding across all switches; 10.969 s | 10.826 s |
| failed replacement (exit 37) | rolled back in 11.785 s, `replacement app exited before presenting (Code(37))`; 10.795 s | rolled back in 11.636 s; 11.429 s |
| Steam warm-up | 23696 files, 2632 MiB, 190 s, presented after 49.2 s, copy, staging empty | 23701 files, 2632 MiB, 145 s, presented after 45.5 s, reflink, staging empty |

Teardown times run from stop acknowledgment to terminal state, disappearance of the exact owned
containers and of the recorded session paths; the bound is 35 seconds.

**Observation gaps (AMD, disposable socket proxy).** The isolated agent was recreated with
`DOCKER_HOST` pointing at an owned Unix-socket proxy in front of the same daemon (the only
environment change, restored afterwards). Cutting the proxy for 12 s during a session left the
session and the application running and the browser decoding with audio across the cut
(luma 127.5, audio pass); the ordinary teardown through the proxied endpoint took 10.938 s. With
the proxy cut, the application was SIGKILLed: the session stayed `running` while the agent could
not observe, reached `failed` with `app exited with code 137` 1.5 s after the engine returned,
and its containers and paths were gone 0.8 s later. A stop requested during a cut went durable
(`application-stop-pending`, `application-drop-cleanup-pending`, `audio-pulse-cleanup-pending`)
and completed 34.5 s after the request once the engine returned, with every journal `Completed`.

**Agent interruption (AMD).** SIGKILL 0.97 s after a launch was requested, and again during a
requested teardown of a session that had written a marker file into its managed home. After each
restart of the same container: every journal terminal, no owned container left, a foreign
same-prefix container preserved, the marker preserved, and the node secret, owner token and
agent container identity unchanged. The control plane failed the first session itself
(`host_lost`, "agent no longer running this session") within the reconnect window; the second
was reaped on reconnect ("host agent connection lost").

**Return to a known-good image (AMD).** After removing fourteen `Completed` journals recorded
against the proxy endpoint (the documented pre-rollback step, see limitations), the agent was
recreated on the #237 image: it came online with the same host id and node secret, the marker
file was still present, a session on that image mounted the same managed home and read the
marker (teardown 10.763 s), and the agent was returned to the candidate with identity intact.

## Defects found by the live exercise and fixed

1. **A changed engine endpoint refused every boot** (`runtime-application-retirement-pending`,
   UnknownOutcome): `abandon`, `abandon_audio`, `retire_audio` and `recover_profile` checked the
   recorded socket before their `Completed` short-circuit, so sixty proven-terminal records became
   uncertain on the new endpoint. Terminal records are now accepted before any endpoint check; a
   non-terminal record from another endpoint still refuses startup fail-closed.
2. **An audio sidecar whose stop went durable during an engine outage ran until the next agent
   restart**: only the application journal had a periodic retry. The 30 s maintenance pass now
   runs routine audio and diagnostic recovery as well, each independently.
3. **One unreadable or unreconcilable helper journal starved every later one in its profile**
   (`recover_profile` aborted on the first failing entry). Entries are now reconciled
   independently in sorted order and the first failure is reported after the scan.

Each fix was written test-first at the fake-engine seam, mutation-checked, reviewed, rebuilt
through the canonical tooling and redeployed; the full matrix was rerun on the final image.

## Checks and review

- `make verify`: 428 passed, zero failures.
- `make test-rust` on the final commit: 1514 tests passed across 11 targets, 11 explicitly
  ignored fixtures, zero failures; fmt and clippy clean. One earlier full run had a single
  unrelated flake (`artifact::tests::lock_is_exclusive_and_released_on_drop`, a shared kernel
  lock held by a concurrent test binary) that passed in isolation and in every later full run.
- `make test-go`: 41 packages ok (the compose file's comment edits and the enroll-host drift
  guards).
- `scripts/dev/leak-scan.sh`: clean on every commit and on the exported evidence.
- Independent specification review used `gpt-5.6-sol` in three passes (the slice, the
  endpoint fix, the maintenance fix): eight findings, all resolved — two test-strength majors
  (a real observer-loop test; a vacuous identity assertion), a starvation medium in
  `recover_profile`, doc-accuracy items, and comment/assertion nits. The deploy/docs slice was
  implemented by `gpt-5.6-terra`; the primary reviewed every diff and owned every commit, build,
  deployment and this evidence.

Regression tests added at Quasar-owned seams (24 new test functions) cover: the legacy sweep's
ownership decisions, a lost remove reply left unresolved and retried next boot, a listing failure
with no mutation; the observer riding out an engine gap and publishing the true exit once, and
publishing nothing once our own teardown began; boot finishing exactly its own obligation next to
a foreign sibling and a managed home; Completed records from another endpoint accepted at boot
while non-terminal ones stay uncertain (applications and helpers); the maintenance pass running
every journal past a failure; routine audio recovery finishing a lost stop with exactly one
sidecar ever created and no live sidecar touched; the source-tree guards.

## Cleanup, evidence and limitations

All ten owned catalog fixtures and their two managed homes were removed after acceptance; the
unrelated catalog entries on both stacks (two AMD, five NVIDIA) were checked and preserved. The
Steam image, its preparation setting and its template were installed on the isolated AMD stack
only by this acceptance and were removed through the uninstall API, which the agent confirmed by
removing the template; the NVIDIA host keeps the Steam install and the template it rebuilt. The
proxy overlay was removed and the isolated agent runs on the default socket again. Both targets
are idle, registered on the candidate commit, hold no owned application or audio container, and
every application journal on each (91 AMD, 60 NVIDIA) is `Completed`. Superseded candidate
images were removed from both hosts; the #237 image was kept on both as the known-good image.
User identity storage, unrelated data, volumes and workloads were preserved; no Docker restart,
driver change, prune or host-wide fault was applied.

Evidence: [acceptance data](rh01-239/acceptance.json) and candidate screenshots
([AMD media](rh01-239/candidate-media-amd-stream.png),
[NVIDIA media](rh01-239/candidate-media-nvidia-stream.png),
[AMD native rendering](rh01-239/candidate-nativegpu-amd-stream.png),
[NVIDIA native rendering](rh01-239/candidate-nativegpu-nvidia-stream.png),
[AMD input](rh01-239/candidate-input-amd-mouse-selection.png),
[NVIDIA input](rh01-239/candidate-input-nvidia-mouse-selection.png),
[AMD across an engine gap](rh01-239/candidate-gap-amd-stream.png),
failed-replacement and switch screenshots alongside). Screenshots were visually inspected;
credentials, addresses, operator paths and raw engine configuration are excluded.

Limitations and findings outside this issue:

- **No running-session adoption.** A session live when the agent process dies is retired at
  boot, never resumed; its runtime directories under `XDG_RUNTIME_DIR` (`udev-<sid>`,
  `wayland-N`) are not reclaimed by boot retirement and were removed by hand here.
- **Rolling back to an agent older than this fix after an endpoint change** requires first
  removing the `Completed` journals recorded against the other endpoint; the older agent refuses
  to boot on them (defect 1). Documented in `docs/configuration.md`.
- **Control plane:** a user stop that lands between an agent reconnect and its first heartbeat
  leaves the session `stopping` until the next reconnect (`AgentHeartbeat` skips `stopping`
  rows). Filed as #251; no wire change needed.
- The isolated AMD stack's host override `app_boot_timeout_secs=900` predates this acceptance
  and was left as found.
- Intel hardware is unavailable; the existing Intel paths and the
  [external validation procedure](rh01-intel-external-validation.md) are unchanged and no Intel
  certification is claimed. Docker is the validated engine; Podman and rootless remain
  evidence-gated in their own milestone.

Work remains on `feature/rh-01-runtime-api`, integrated into
`initiative/resilient-host-architecture`. No merge into develop/main, release tag or promotion
occurred. #240 is not started by this report.
