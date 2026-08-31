# Quasar documentation map

Start with **[`architecture-and-plan.md`](architecture-and-plan.md)** (the master rationale + roadmap)
and **[`../CLAUDE.md`](../CLAUDE.md)** (agent operating context: invariants, frozen interfaces,
conventions, gotchas). This file is the index to everything else.

**Organizing principle:** anything **complete** lives under [`completed/`](completed/); the **current**
phase, **future** stubs, in-progress work, and live reference docs stay at the top level.

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
**integrated roadmap spec v2** ([`design/plans/2026-07-06-roadmap-spec-v2.html`](design/plans/2026-07-06-roadmap-spec-v2.html),
a library-provider model + wave ladder): W0 (image consolidation) and W1 (security
wave) are merged; W2 is active: the console-mode work (local-display sessions on a
host's attached monitor) in parallel with Phase 9 closure (the native client).

## Phases

| Phase | Status | Where |
|---|---|---|
| 0 — Instrumented spike (transport/input/latency) | ✅ complete | [`completed/`](completed/) (`phase0-*`) |
| 1 — Single-host MVP | ✅ complete | [`completed/`](completed/) (`phase1-*`) |
| 2 — Multi-user + resource governance | ✅ complete | [`completed/phase2/`](completed/phase2/) |
| 3 — Multi-host scheduling (N>1) | ✅ complete | [`completed/phase3/`](completed/phase3/) |
| 4 — Performance & observability | ✅ complete | [`completed/phase4/`](completed/phase4/) |
| 5 — Per-user storage & state | ✅ complete | [`completed/phase5/`](completed/phase5/) |
| 6 — Library & content management | 📋 scope stub | [`phase6/`](phase6/) |
| 7 — User management & integrations | 📋 scope stub | [`phase7/`](phase7/) |
| 8 — Networking edge (TURN/WAN) | 📋 scope stub | [`phase8/`](phase8/) |
| 9 — Native client | 🔬 design + spike (in progress) | [`phase9/`](phase9/) (incl. native-client architecture/perf/macOS research) |

## Completed cross-cutting workstreams (all under `completed/`)

| Workstream | Where |
|---|---|
| Adaptive streaming (ABR, playout, stream profiles, AS-10 milestone) | [`completed/adaptive-streaming/`](completed/adaptive-streaming/) |
| GPU zero-copy encode (milestone #10: ZC-01/02/03 + in-compositor Vulkan NV12 / PR #37) | [`completed/zero-copy-gpu-memory/`](completed/zero-copy-gpu-memory/) |
| Optimization spike | [`completed/spike-optimization/`](completed/spike-optimization/) |
| Web UI overhaul (shipped #270) | [`completed/ui-overhaul/`](completed/ui-overhaul/) |
| Stream perf tuning (SPT-01..10, `abr_mode=smooth` default) | [`completed/stream-perf/`](completed/stream-perf/) |
| UI polish (2026-06-24 pass) | [`completed/ui-polish/`](completed/ui-polish/) |
| W1 security wave (invite gating + device binding, PR #374) | [`completed/w1-security/`](completed/w1-security/) |
| Observability v2 session tracer (ST-00..08; live format spec stays at [`session-trace/trace-format.md`](session-trace/trace-format.md)) | [`completed/session-trace/`](completed/session-trace/) |
| Tech-debt refactors TD-01/TD-02 (open backlog stays at [`tech-debt/REVIEW-REMAINING.md`](tech-debt/REVIEW-REMAINING.md)) | [`completed/tech-debt/`](completed/tech-debt/) |
| 2026-07-04 stabilisation audit inputs | [`completed/audit-2026-07-04/`](completed/audit-2026-07-04/) |
| Executed implementation plans & kickoff prompts (live ones stay in [`design/plans/`](design/plans/)) | [`completed/plans/`](completed/plans/) |

## Reference (read these to operate the system)

| Doc | What |
|---|---|
| [`../deploy/README.md`](../deploy/README.md) | Deploy / run the stack (the Phase-0 dev-env doc is archived at [`completed/phase0-setup.md`](completed/phase0-setup.md)) |
| [`configuration.md`](configuration.md) | Every env var — default + accepted values |
| [`third-party-pins.md`](third-party-pins.md) | The pinned `gst-wayland-display` / `gst-interpipe` commits and how to flip them to a release |
| [`research/`](research/) | Performance / latency research (perf summary, input-latency analysis) |
| [`design/`](design/) | Implementation plans & design specs (planning-workflow output) |
| `../protocol/` | Frozen wire contracts (signaling, input, agent/control/native-client APIs) — a `quasar-protocol` submodule |

## Future / deferred

[`future/`](future/) — `kubernetes-native.md`, `networking-edge.md` (deferred design).
