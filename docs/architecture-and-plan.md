# Quasar — Architecture & Plan of Attack

> **Quasar** is a working placeholder name. Pick a final name and trademark/domain-check it before committing. Other candidates: Lattice, Tessera, Conduit.

A self-hostable, multi-host cloud-gaming platform — conceptually a self-hosted GeForce Now. A clean-room successor to Wolf that builds on Wolf's genuinely strong components, leaves behind everything coupled to discontinued protocols, and is architected from day one for the end state (multi-user, multi-host, Kubernetes, resource-governed) rather than reaching it incrementally.

---

## Guiding principle

The observation that motivated this effort is structural: Wolf grew from a single-host, single-user core, so capabilities beyond that were necessarily added on top of constraints inherited largely from the Moonlight protocol, rather than designed in from the start. K8s support has to work around a design where one process assumes it owns the host; WolfUI works around Moonlight's one-app-per-session model; WolfManager exists because Moonlight clients can't manage a host. These are reasonable responses to those constraints — Quasar's bet is simply to remove the constraints up front and design for the end state instead.

**Quasar's rule: if a feature is on the long-term roadmap, the architecture must anticipate it from the first commit, even if it isn't built yet.** That is the difference between this project and forking Wolf.

---

## Decided technical direction

These are settled based on prior analysis; they are the foundation everything else builds on.

1. **Drop Moonlight / NVIDIA GameStream compatibility.** NVIDIA has discontinued GameStream; the protocol caps the experience and drives much of the UI fragmentation above. The ecosystem is already moving on (Apollo/Artemis are deliberately shedding OG compatibility).
2. **Browser client first, native clients later, over WebRTC.** The browser proves the stack end-to-end against well-trodden ground (Selkies). Native clients come later to reclaim latency the browser costs (see Transport).
3. **GStreamer stays for the media path.** The compositor *is* a GStreamer source element and the encode path's hard zero-copy work is done. The framework was never the bottleneck — pipeline construction was. For a WebRTC/browser target, `webrtcbin` is an asset, not a liability (FFmpeg's WebRTC support is publish-only/minimal and has no bidirectional data channel for input).
4. **The transport is an interface, not a hard dependency.** `compositor → encode → [transport slot]`. `webrtcbin` is implementation #1. This is what makes the framework question safe to defer and native clients possible later.

---

## Architecture: control plane + node agent

The single most important structural decision, and the one that makes the entire roadmap (K8s, resource governance, multi-host) *native* rather than retrofitted, is to split Wolf's single-process design into two services:

**Control plane** (one logical service, horizontally scalable, backed by a real database):
- Accounts, authentication, authorization.
- The public/control API and WebRTC signaling.
- The session scheduler — decides which node runs a given session, governs resources.
- Serves / backs the unified web client.
- Holds *no* per-host GPU state itself; it directs nodes.

**Node agent** (runs on every GPU host):
- Owns the host's GPUs and reports their capacity to the control plane.
- Launches/stops session containers, runs the compositor + encode + transport for sessions assigned to it.
- Plugs virtual input devices (inputtino + fake-udev path, reused).
- Stateless beyond the sessions currently assigned to it.

Why this split is the whole game:
- **Multi-host** is the default, not a feature: the control plane already talks to N node agents; one node is just N=1.
- **Kubernetes** becomes a *packaging* exercise, not a re-architecture: node agent → DaemonSet on GPU nodes, control plane → Deployment, sessions → scheduled workloads. Wolf-on-K8s is awkward because a single process fills both the node-agent and control-plane roles at once; separating those roles on day one is what makes K8s fall out cleanly.
- **Resource governance** has a natural home: the scheduler in the control plane, fed by capacity reports from node agents.
- **State survives restarts and scales**: external Postgres instead of Wolf's in-memory immer structures serialized to TOML.

---

## Fork vs build vs discard

| Component | Decision | Notes |
|---|---|---|
| `gst-wayland-display` (compositor) | **Reuse / light fork** | Crown jewel. Smithay-based, zero-copy CUDA/DMABuf done well, actively maintained. Vendor and track upstream; contribute back where possible. |
| `inputtino` (virtual input) | **Reuse** | Standalone, clean. Vendor it. |
| Smithay | **Upstream dependency** | Foundation of the compositor. Not forked. |
| GStreamer encode + `webrtcbin` tail | **Build (assemble)** | New, tuned pipeline using stock elements. No custom RTP plugins. |
| Orchestration core / session server | **Build new** | The heart. Wolf's is single-host, TOML, GameStream-coupled. Rebuilt as control-plane + node-agent. |
| Signaling server | **Build new** | WebSocket; part of the control plane. |
| Unified web client | **Build new** | Replaces WolfUI + WolfManager + WolfDen with one app. |
| Node agent | **Build new** | The other half of the architectural split. |
| GameStream HTTP/RTSP/ENet, Moonlight RTP payloaders, nanors FEC, `_nvstream` mDNS | **Discard** | Dies with Moonlight. |
| WolfUI (Unity) | **Discard** | Replaced by the web client. The launcher↔game swap becomes a first-class server operation rather than an in-stream Unity flow. |
| WolfManager / WolfDen | **Discard** | Subsumed by the control plane + web client. |

---

## Resource governance (designed in from Phase 1)

Multi-user-per-host means the scheduler must treat the host's resources as a budget, not assume exclusive use. Scheduling dimensions to model as first-class from the start, even before limits are enforced strictly:

- **GPU encode capacity.** Consumer NVIDIA GPUs cap concurrent NVENC sessions (the limit has risen over driver generations but is finite). This is a hard per-GPU resource — track and govern encode-session count, not just "is there a GPU."
- **VRAM.** Each session's compositor + game consumes VRAM; oversubscription thrashes. Track and reserve.
- **GPU compute / render share.** Per-session render budget; relevant for fair multi-tenancy on one GPU.
- **CPU + memory** for the container and pipeline.
- **Multi-GPU topology.** Preserve Wolf's ability to split work (e.g. encode on iGPU, render on dGPU) — expose it as a scheduling dimension rather than a static config knob.

The scheduler matches a session's resource request against node-agent capacity reports. This is the same shape as Kubernetes' resource model, which is why K8s integration later is natural.

---

## Transport decision rule

Do **not** switch the transport framework until all three are true:

1. The all-GStreamer + `webrtcbin` path is built and working.
2. End-to-end latency is *instrumented and measured* (capture → encode → network → decode → display).
3. Measurement proves `webrtcbin`'s transport — not pipeline tuning, not the client jitter buffer — is the gap to target.

Latency levers to exhaust first, in the existing pipeline: zero-copy GPU path (already done in the compositor), encoder low-latency preset with **no B-frames**, intra-refresh / sliced encoding, tight CBR matched to the link, and reference-frame invalidation for loss recovery (Wolf modeled this as `frames_with_invalid_ref_threshold` — carry the concept forward).

Note on clients: the browser owns its jitter buffer and congestion control (conferencing-tuned), which imposes a latency floor you cannot remove from JavaScript. Native clients are where that latency is reclaimed — so the transport interface should anticipate a **custom UDP protocol for native clients**, with WebRTC as the browser transport and the starting transport, not the permanent one for all clients.

---

## Phased plan of attack

Each phase's architecture anticipates the next, so nothing is a throwaway workaround.

**Phase 0 — De-risk spike (single session, no orchestration).**
One `gst-wayland-display` session → `webrtcbin` → Chrome. One game rendering in-browser with gamepad input over a DataChannel. Instrumented for end-to-end latency from the start. Validates: the transport swap, the input path, the latency budget, and browser-gamepad limits — the riskiest assumptions, cheaply, before any bet is locked.

**Phase 1 — Single-host MVP.**
Node agent + minimal control plane on one host. Web client: library, launch, live stream view. Accounts/auth. Postgres persistence. Establish the transport interface and the *session-as-schedulable-unit* abstraction even though N=1. Introduce resource tracking (encode-session count, VRAM) even if limits are generous — bake the model in.

**Phase 2 — Multi-user + resource governance.**
Concurrent sessions on one host. Real resource governor (encode/VRAM/GPU quotas). Session lifecycle hardening. Lobbies / multi-user-per-session if desired. The launcher↔game swap as a first-class control-plane operation (may still use GStreamer interpipe internally — but now it's a feature, not a client workaround).

**Phase 3 — Multi-host + scheduling.**
Multiple node agents. Control plane schedules sessions across nodes and load-balances. Prove the split at N>1. TURN/relay scaled as a separate component for internet-facing NAT traversal.

**Phase 4 — Performance & observability.**
Productize the Phase 0 latency instrument into an end-to-end performance pipeline (host
encode → wire → browser decode/present on one timeline), reproducible test apps, an
automated troubleshooting harness, per-session telemetry surfaced in admin, and a client
connection + decode-capability test at login stored per user/device. Scope stub:
`docs/completed/phase4/`.

**Phase 5 — Storage & state foundation.**
Per-user persistent state (managed home mounted into the container) and the storage
substrate the library needs. Two asymmetric problems — a read-only common content store
and a read-write per-user home — behind a pluggable storage-provider abstraction.
**Single-host now, multi-host-ready by design** (networked/shared storage is a later driver
swap, not a re-model). Complete — records in `docs/completed/phase5/`.

**Phase 6 — Library & content management.**
A real library: common deduplicated content store (overlayfs, write-once), a runtime-image
catalog, Proton management, artwork/metadata (SteamGridDB/Steam/IGDB), entitlements, and
admin-permissioned installs. Multi-source by design (Steam first). An "app" becomes
*(content) + (runtime image) + (launch command)*, not a monolithic image. Scope stub:
`docs/phase6/`.

**Phase 7 — User management & integrations.**
Invite/redemption signup, device management, and **Steam via SteamKit2** — server-side QR
auth + direct depot download into the common store, then direct-launch under Proton (no
in-container Steam client, no per-user re-downloads). Scope stub: `docs/phase7/`.

**Phase 8 — Networking edge (TURN/WAN).**
Internet-facing media relay so GPU hosts stay private (outbound-only). Private hosts +
public TURN edge; `ice_servers` flow through the contract; signaling shapes unchanged.
Design: `docs/future/networking-edge.md`; scope stub: `docs/phase8/`. **Order flexible** —
pull ahead if remote usability becomes the priority.

**Phase 9 — Native clients + client SDK.**
Document the wire protocol; ship a reference native client (custom UDP transport — the lever
that beats the browser's irreducible jitter-buffer floor) + input capture; open the
community-contribution surface. Transparent reconnect/resume lands here. Scope stub:
`docs/phase9/`.

**Deferred — Kubernetes-native.**
Was the original Phase 4; **dropped from the active sequence** while the focus is
single-host → small multi-host. Costs nothing to defer because the control-plane/node-agent
split has existed since Phase 1 — it is packaging, not re-architecture, when revived. See
`docs/future/kubernetes-native.md`.

> **Roadmap note (updated 2026-07-17 — see `docs/README.md` for the live status).**
> Phases 0–5 are complete (records in `docs/completed/`); active work follows the
> roadmap-spec-v2 wave ladder (`docs/design/plans/2026-07-06-roadmap-spec-v2.html`),
> which supersedes the numbered-phase framing. Phases 6–8 remain **scope stubs** —
> each gets thorough exploration + detailed design + tickets **at phase start**, not
> before. The order of Phases 6–9 is provisional; Phase 8 in particular can be
> resequenced. Kubernetes is deferred.

---

## Platform releases and self-update (2026-09-05)

Quasar updates itself. A **platform release** is a matched control-plane + node-agent
image set from one commit (`CONTEXT.md` "Platform releases"). The control plane detects
releases (stable = GitHub Releases + a release manifest; edge = a branch tag's digest and
the identity labels on the image), decides what is offered with a pure function ordered
by **schema version** — the database migration the release embeds — and applies through
a per-host **updater** sidecar that pulls pinned digests and recreates containers, since a
container cannot recreate itself. The shape follows the invariants above: the control
plane holds the decision and the state (`platform_releases`, apply runs and attempts in
Postgres); the agent only relays a `release_apply` to its updater and reports
`release_state`; the updater trusts nothing but an allowlisted registry namespace and a
well-formed digest (ADR 0001). Order is fixed by ADR 0002: control plane first, hosts
after, never below the database's applied migration, so the console can never offer a
downgrade; revert exists only for agents. A source-built host is told about releases but
never given one. The design record is #104 (spec), the ADRs, and
`docs/reports/2026-09-05-self-update-live-gate/`.

## Open decisions to confirm

- **Implementation language(s).** Leading recommendation: a polyglot split that mirrors the architectural split.
  - *Node agent in Rust* — aligns with `gst-wayland-display` + Smithay (both Rust), mature `gstreamer-rs` bindings, `webrtc-rs` available if/when a non-`webrtcbin` transport is wanted.
  - *Control plane in Go* — the Kubernetes ecosystem is Go (client-go, operators, schedulers), fast iteration, Pion available for any WebRTC needs in the control plane.
  - Alternative: all-Rust (simpler to staff one language, tighter integration with the compositor, at the cost of slower control-plane iteration and a less native K8s ecosystem fit).
- **Transport framework beyond `webrtcbin`** — deferred by the transport decision rule; revisit only with latency numbers.
- **License** — Wolf and the GoW components are MIT; confirm compatible licensing for vendored/forked components and choose the project license deliberately.
- **Final name** — and trademark/domain check.

---

## What this buys you

Three separate UIs collapse into one web app. The Moonlight protocol coupling and its associated complexity go away. Multi-host, resource governance, and Kubernetes stop being retrofits and become consequences of the day-one architecture. And the strongest engineering in the existing stack — the compositor — is reused rather than rebuilt. The result is a smaller, more coherent surface despite a far more ambitious goal.
