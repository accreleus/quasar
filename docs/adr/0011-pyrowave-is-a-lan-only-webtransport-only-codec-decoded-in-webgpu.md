---
status: accepted
date: 2026-11-06
---
# PyroWave is a LAN-only, WebTransport-only codec decoded in WebGPU

**PyroWave** is a wavelet codec: MIT licensed, intra-only, run in Vulkan compute. Quasar offers it
as the user-facing **Ultra-low latency (LAN)** switch (`GLOSSARY.md`), and we decided that it
rides **only** the WebTransport transport (ADR 0010), is decoded **in the browser by a WGSL port
running in WebGPU**, and is **never chosen automatically over the internet** (spec #1048, wire
contract `protocol/codec-pyrowave.md`, amendment 55). Decided with the owner on 2026-11-06.

## Why this codec, and why it is bounded this way

H.264, HEVC and AV1 trade latency for compression: frames depend on earlier frames, rate control
looks ahead, and the browser's decoder adds its own queue. A user on the same LAN as the host has
bandwidth to spare and wants the least delay between the game drawing a frame and seeing it.
PyroWave has no inter-frame state, so a lost packet costs only the blocks it carried (*block
concealment*) and never a freeze or a keyframe wait. As measured in the B00 spike (#1049): at most 0.23 ms
encode at 4K on the RTX 5090, and a WGSL decoder that matches the C reference within one code value
on every feature path, at native speed on that GPU.

The price is bitrate: about 200 Mbit/s at 1080p, 300 at 1440p, 400 at 4K (the contract's defaults,
from #1049). That decides the shape
of the decision:

- **Never over WebRTC.** No browser decodes PyroWave natively and WebRTC has no payload or
  congestion response for 200 Mbit/s of independent blocks. The browser must decode it itself, so
  it needs a transport that hands the page raw datagrams.
- **Never over the internet.** The rate rules out a WAN. The contract's gate is `max_rtt_ms` (5 ms,
  amendment 55 §Link, a guess until benched): the agent starts PyroWave on a connection, or accepts
  a switch on to it, only while the session's own QUIC round trip is at or under it; past that the
  session streams a standard codec over WebTransport and says why. If the congestion estimate falls
  below a floor, the agent switches the connection back to the standard codec the same way. The
  device probe's bandwidth and RTT are not inputs: they measure the client to control-plane path.
- **The control plane grants; the agent applies.** The control plane grants PyroWave, at launch or
  through the user's toggle, after checking the profile, the transport, the host's GPU codec set
  and the device's WebGPU. The agent only applies a grant, subject to its own round-trip check, and
  may move a session off PyroWave (congestion, a client decode error, a fallback to WebRTC). It
  never grants itself PyroWave, and a switch off for loss or congestion is not undone by the agent.
- **Never automatic over the internet, and not automatic at all in 0.6.** The user turns it on, on
  a profile an admin allowed; it is off everywhere until an admin enables it. Automatic selection, with no
  toggle, is planned for 0.6.1 (otherwise 0.7) only for a qualifying LAN: a round trip under about
  2 ms (a different figure from `max_rtt_ms`: the automatic-selection criterion, not the gate
  every PyroWave session passes), at least 1.5 times its bitrate free, and a bench-proven latency win. A fast-looking link
  alone never switches it on; a session that fails the LAN test always gets a standard codec.
- **Only when proven.** The hosts need a passing PyroWave codec probe, the browser needs WebGPU,
  and the headline claim has a gate: at least 15 ms median glass-to-glass win over WebRTC with
  H.264 on the NVIDIA lab host. Failing the A00 carriage test (#1025: about 250 Mbit/s with under 1% loss
  on the lab LAN) removes PyroWave from 0.6 and WebTransport ships without it.

## What the decision commits us to

- **A hand-ported WGSL decoder** with two feature paths: `subgroups` plus `shader-f16` where the
  browser exposes them, and workgroup-memory scans with fp32 storage otherwise. Chrome exposes no
  `shader-f16` on NVIDIA/Vulkan, so the fp32 path is a production path, not a degraded one. It
  renders straight into the shared WebGPU presenter, with no copy back to the CPU.
- **A pinned upstream.** PyroWave's C API is unstable, so the exact commit is pinned (and the
  parts of Granite it needs), recorded beside the other third-party pins, and bumped only together
  with regenerated golden fixtures. The WGSL decoder is checked against the reference decode on
  every change.
- **Encode stays in GStreamer** (invariant 4): a `pyrowaveenc` element wraps the C API and takes
  Vulkan images from the compositor's Vulkan path, so nothing upstream of the encoded-frame
  boundary changes.
- **A codec that exists on one transport only.** The session's codec is otherwise identical on
  both transports (ADR 0010); PyroWave is the single exception, and a WebRTC peer always gets the
  session's standard codec.
- **4:2:0 chroma; no entropy coder.** 4:4:4 is a later amendment; upstream declares a proper
  entropy coder out of scope.

## Considered options

- **PyroWave over WebRTC data channels.** Reintroduces the browser's congestion control and an
  ordering/reliability model the codec does not want, for a rate WebRTC is not built for.
- **A WebAssembly or CPU decoder.** Cannot hold 4K60 at this bitrate; the work is GPU-shaped.
- **A low-latency intra-only profile of H.264 or AV1.** Still goes through the browser's decoder
  queue and a lookahead-free encoder is not what those encoders are tuned for, so the floor this
  work exists to remove stays.
- **Choosing it automatically in 0.6.** A false positive puts a 200 Mbit/s stream on a link that
  cannot hold it, so selection waits for bench evidence.

## Amendment (2026-11-07, amendment 58, #1059): chosen at launch, `prefer` lets Auto pick it

Decided with the owner on 2026-11-07, recorded on #1059 and in `protocol/codec-pyrowave.md`. This
replaces "not automatic at all in 0.6" above; every other bound in this ADR stands (WebTransport
only, LAN only, the agent's round-trip gate, the control plane grants and the agent applies).

- **A profile's `pyrowave_mode` is `off`, `allow` or `prefer`**, replacing `pyrowave_allowed`.
  `allow` gives PyroWave only to a user who picks it in the Launch options Codec list. `prefer`
  also lets a launch whose codec is Auto get it, **at launch**, when the control plane's Auto rule
  holds. A link that does not qualify always gets a standard codec.
- **Mid-session switching is withdrawn from 0.6 and moves to 0.6.1**
  (`PATCH /v1/sessions/{id}/pyrowave` and `session_pyrowave` are gone).
- **There is no user transport choice.** PyroWave implies WebTransport; the transport follows the
  stream plan.
- **A per-GPU ceiling, `pyrowave_max_per_gpu`**, bounds PyroWave sessions on one GPU
  (`sessions.pyrowave_slot`).
