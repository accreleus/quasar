# Quasar documentation map

Start with **[`architecture-and-plan.md`](architecture-and-plan.md)** (the master rationale + roadmap)
and **[`../CLAUDE.md`](../CLAUDE.md)** (agent operating context: invariants, frozen interfaces,
conventions, gotchas). This file is the index to everything else.

**Organizing principle (historical):** anything **complete** used to live under `completed/`,
with `design/` holding live implementation plans and `research/` holding performance/latency
write-ups. **None of `completed/`, `design/`, `research/`, `tech-debt/`, or the `phase6/`–`phase9/`
scope stubs exist in this repo** — they were deliberately not carried over to the public repository. What follows
records what each of those held, for context; going forward, plans and specs are filed under
[`superpowers/plans/`](superpowers/plans/) and `superpowers/specs/`, and dated write-ups under
[`reports/`](reports/).

## What works today

A user can register → log in → launch → play a real container-launched app over the
control-plane → node-agent → WebRTC path, with:

- **Single-host MVP**: register/launch/play, Postgres-backed scheduled+reserved sessions (Phase 1).
- **Multi-user + resource governance**: per-user quotas, overcommit rejection (encode slots + VRAM), session-lifecycle hardening, and the launcher↔game swap without tearing down the WebRTC transport (Phase 2).
- **Multi-host scheduling**: N>1 GPU hosts with per-GPU reservation, a pluggable placement policy, host drain/cordon, and host-offline failover (Phase 3).
- **Performance & observability**: adaptive streaming (rtpgccbwe ABR + adaptive playout), GPU zero-copy encode (AMD VA + Nvidia NVENC, opt-in Vulkan), and standing per-session metrics + an always-on client-side deep glass-to-glass trace (Phase 4 + the Optimization/Adaptive-Streaming spike).
- **Per-user persistent storage**: a managed home directory mounted into the game container, behind a storage-provider abstraction, with single-writer guarantees and launch-time quotas (Phase 5).
- A unified **web client** with role-separated user (`/app`) and admin (`/admin`) areas; admin is server-enforced, never UI-gated.
- **Security**: invite-gated registration (off by default) and device token-binding/revocation (W1).

The browser transport is WebRTC (`webrtcbin`); a native UDP client is the planned transport #2.

## Roadmap of record

The numbered-phase framing below is historical. Current work follows the
**integrated roadmap spec v2** (a library-provider model + wave ladder). Its source
document, `design/plans/2026-07-06-roadmap-spec-v2.html`, was deliberately not carried over to the public repository. What's on record:
W0 (image consolidation) and W1 (security wave) are merged; W2 is active: the
console-mode work (local-display sessions on a host's attached monitor) in parallel
with Phase 9 closure (the native client).

## Phases

Per-phase archive directories (`completed/phaseN-*`, `phase6/`–`phase9/`) do not exist
in this repo — see the note above. Status is on record here and in
`architecture-and-plan.md`; per-phase detail is not public.

| Phase | Status |
|---|---|
| 0 — Instrumented spike (transport/input/latency) | ✅ complete |
| 1 — Single-host MVP | ✅ complete |
| 2 — Multi-user + resource governance | ✅ complete |
| 3 — Multi-host scheduling (N>1) | ✅ complete |
| 4 — Performance & observability | ✅ complete |
| 5 — Per-user storage & state | ✅ complete |
| 6 — Library & content management | 📋 scope stub |
| 7 — User management & integrations | 📋 scope stub |
| 8 — Networking edge (TURN/WAN) | 📋 scope stub |
| 9 — Native client | 🔬 design + spike (in progress) — included native-client architecture/perf/macOS research |

## Completed cross-cutting workstreams

These were archived under `completed/`, which does not exist in this repo (see the note
above) — listed here for the record, with no working link:

| Workstream | Was at |
|---|---|
| Adaptive streaming (ABR, playout, stream profiles, AS-10 milestone) | `completed/adaptive-streaming/` |
| GPU zero-copy encode (milestone #10: ZC-01/02/03 + in-compositor Vulkan NV12 / PR #37) | `completed/zero-copy-gpu-memory/` |
| Optimization spike | `completed/spike-optimization/` |
| Web UI overhaul (shipped #270) | `completed/ui-overhaul/` |
| Stream perf tuning (SPT-01..10, `abr_mode=smooth` default) | `completed/stream-perf/` |
| UI polish (2026-06-24 pass) | `completed/ui-polish/` |
| W1 security wave (invite gating + device binding, PR #374) | `completed/w1-security/` |
| Observability v2 session tracer (ST-00..08; the live format spec still stays at [`session-trace/trace-format.md`](session-trace/trace-format.md)) | `completed/session-trace/` |
| Tech-debt refactors TD-01/TD-02 (the open-backlog doc, `tech-debt/REVIEW-REMAINING.md`, is also gone) | `completed/tech-debt/` |
| 2026-07-04 stabilisation audit inputs | `completed/audit-2026-07-04/` |
| Executed implementation plans & kickoff prompts (live plans now go to [`superpowers/plans/`](superpowers/plans/)) | `completed/plans/` |

## Reference (read these to operate the system)

| Doc | What |
|---|---|
| [`../deploy/README.md`](../deploy/README.md) | Deploy / run the stack (the Phase-0 dev-env doc, `completed/phase0-setup.md`, was deliberately not carried over to the public repository) |
| [`configuration.md`](configuration.md) | Every env var — default + accepted values |
| [`third-party-pins.md`](third-party-pins.md) | The pinned `gst-wayland-display` / `gst-interpipe` commits and how to flip them to a release |
| [`reports/`](reports/) | Dated investigation/validation write-ups (current archive; the non-public `research/` directory — perf summary, input-latency analysis — does not exist) |
| [`superpowers/plans/`](superpowers/plans/) | Implementation plans (current location; the non-public `design/` directory does not exist) |
| `../protocol/` | Frozen wire contracts (signaling, input, agent/control/native-client APIs) — a `quasar-protocol` submodule |

## Future / deferred

[`future/`](future/) — `kubernetes-native.md`, `networking-edge.md` (deferred design).
