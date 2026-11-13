---
status: accepted
date: 2026-11-13
---
# On WebTransport, resolution moves inside a fixed coded size

On a WebTransport session, a small resolution change stops being an encoder restart. The encoder
keeps its coded size, and the scale stage draws the game picture into a smaller **active
rectangle** inside that frame, padded with its edge pixels. The rectangle moves only within a band
near full size, in steps of at most 3 % of the coded side. Below the band, and for a collapse,
CA1's rung restarts (already shipped) take over, so the coded size is constant only within the
band. Every frame carries its rectangle in a small metadata unit inside the access unit, and the
presenter crops to it. This applies to a client that opts in, on an encoder path whose scale stage
has a canvas mode; everything else keeps CA1's rungs. Coverage: H.264 measured on both lab hosts,
AV1 on the NVIDIA lab host only (the AMD GPU has no AV1 encode), HEVC unmeasured (P-frame
capability only); the metadata unit has been decoded nowhere yet. This is Design 4 of the research
(#1372), chosen over AV1 reference scaling (Design 3) and over coarser rungs alone. Spec #1375,
ticket #1381; the build is CA5 (#1383).

## Evidence

- **Encoder capabilities** ([#1378 result](https://github.com/accreleus/quasar/issues/1378#issuecomment-6854339213),
  [review](https://github.com/accreleus/quasar/issues/1378#issuecomment-6855812856)). On the NVIDIA
  lab host (RTX 5090, driver 615.78.08), AV1 encode has `FRAME_SIZE_OVERRIDE` but **not**
  `MOTION_VECTOR_SCALING`, so a frame of a new size cannot predict from frames of another size.
  The AMD lab host (Raphael iGPU, RADV on Mesa 25.3.6) has **no AV1 encode**: H.264 and HEVC only.
  Every codec on both hosts codes ordinary P frames, which is what Design 4 asks of the encoder.
  That is a capability, not a measured bit cost.
- **Browser decode of an AV1 size change on inter frames**
  ([#1379 result](https://github.com/accreleus/quasar/issues/1379#issuecomment-6854334513), issue
  still open). Chrome's software decoder (dav1d, Linux) decodes these streams with no error and no
  `configure()`, and it reports the size of each frame. Any `configure()` at the change makes the
  decoder demand a keyframe. The streams carried no metadata unit. **Hardware decode (NVIDIA and
  AMD on Linux) and the Mac test client were not tested.**
- **Bit cost of a zoom step against a restart**
  ([#1380 result](https://github.com/accreleus/quasar/issues/1380#issuecomment-6855892036)).
  Synthetic content. On NVIDIA AV1 at CBR 1–10 Mbit/s, a 3 % step inside a fixed 1440p frame dips
  0.5–0.8 dB of PSNR and recovers in 4–7 frames. A restart with a keyframe dips 3–5 dB and takes
  11–26 frames. At fixed QP the step costs +0.3 to +0.8 frame-equivalents of bytes, the restart
  +1.7 to +5.9, on AV1 and on H.264 on both hosts. An H.264 restart puts 5–14 times a normal frame
  on the wire. 5 % steps cost 1/3 to 1/4 of a restart and pile up at low rates. Two costs of the
  fixed size itself:
  - At the same rate, the fixed-size picture settles 0.5–1 dB below a restart at its own size
    (NVIDIA, s ≈ 0.94–0.97, padding counted). The spike also held s = 0.90 on both hosts against
    a restart at the same size, not yet reduced; nothing was measured below 0.90.
  - H.264 at a fixed 1440p coded size sat at its QP ceiling at about 2.6 (NVIDIA) and
    3.1 (AMD) Mbit/s: the 1000 and 1500 kbit/s targets produced the same bytes. A smaller
    rectangle trims that by 7–18 % but cannot bring it down to such a target.

  A one-seat method review recomputed the tables and found every cell matches, with the notes
  that bear on this record: the AV1 zoom dips are about one standard deviation of frame-to-frame
  noise (0.3–0.7 dB), the restart's 3–5 dB is not; the H.264 keyframe's 216 ms (NVIDIA) and
  483 ms (AMD) at 1 Mbit/s are an upper bound; the Vulkan encoders have no one-frame VBV (AV1
  keyframes ran at 2.5–3.8 times the budget); with production's one keyframe per second the
  restart's cost is slightly overstated if anything; the ramp figures are approximate. All ratios
  hold for this content only.

## What each option covers

| | NVIDIA AV1 | NVIDIA H.264 / HEVC | AMD H.264 / HEVC | Browser decode |
|---|---|---|---|---|
| **Design 4, fixed coded size** (chosen) | measured | H.264 measured; HEVC unmeasured | H.264 measured; HEVC unmeasured | an ordinary fixed-size stream; metadata unit not yet decoded anywhere (the client can strip it) |
| **Design 3, AV1 reference scaling** | no MV scaling: a size change cannot predict across sizes | no (a new size needs an IDR) | no (no AV1 encode) | dav1d yes; hardware and Mac unknown (#1379) |
| **Coarser rungs** (CA1 alone) | yes | yes | yes | yes, with a reconfigure and a keyframe per step |

Inter-predicted Design 3 is rejected on #1378: the NVIDIA lab host has no motion-vector scaling and
the AMD lab host has no AV1 encode, so no lab encoder can predict across a size change; #1379 is
still open on the decode side. An intra size change inside one sequence was not measured. Unlike
Design 4 it leaves the encoder at the new coded size; today that role is CA1's rung restart.
Coarser rungs alone keep the freeze and the keyframe stall that the spec's cause 3 sets out to
remove.

## The band

The rectangle moves only within a band near full size. Below the band, and for a collapse that
#1374 answers with one jump, CA1's rung restart takes over: a new coded size, which is a
configuration change as the contract already defines one (new `config_id`, keyframe, `hello_ack`
re-sent), and the rectangle resets to the full new frame. The band then applies to that coded
size. The ABR floor follows from that: the rectangle moves only while the encoder is
rate-controlled at the current coded size, and a target under that floor is a rung restart, not a
smaller rectangle.

The evidence does not fix the band's bottom. CA5 sets it by measurement (CA5f): steady-state rate
and PSNR at a fixed coded size against a restart at the rectangle's own size, per codec, for s from
1 down to 0.5, on real game content. The bottom is where the fixed size stops being
rate-controlled or its quality gap to a restart outgrows a step's cost. Nothing below the lowest
size where that gap was measured (s = 0.90) ships before it.

## Step size and slew

- **A step is at most 3 % of the coded side** (s = 1 − 0.03·k, as the spike stepped), not 3 % of
  the current rectangle, and the rectangle is 8-pixel aligned.
- **Steps go 8 frames apart at 60 fps**, because an isolated 3 % step recovered in 4–7 frames. A
  1 → 0.5 → 1 ramp at one step per 6 frames cost roughly 0.3 dB of mean PSNR (approximate). A
  120 fps session keeps the same spacing in time (16 frames) until it is measured.
- **The way up is the same step, no faster than the rate estimate grows.**

## Where the rectangle travels

**In the access unit**, as a metadata unit the agent writes into each frame's payload: an AV1
metadata OBU, or an H.264/HEVC SEI `user_data_unregistered`. The client reads it from the chunk
bytes before decoding. It rides every frame, so a lost frame, a NACK resend or FEC needs no extra
state. A decoder that rejects the unit changes nothing on the wire: the contract defines the
payload as "the access unit in the form `EncodedVideoChunk` takes: 4-byte length-prefixed NAL
units for H.264 and HEVC …, the low-overhead OBU format for AV1 (each OBU with its size field …)",
so the client removes the unit before `decode()`. If the wire ever had to change, §Versioning
allows "new datagram channels" inside `wt_proto` 1.

Not a datagram header field: §Versioning says "A change to a binary header above needs a new
`wt_proto`", and the field would repeat on every fragment. Its one gain, surviving the loss of the
fragment that holds the unit, does not count, because the client drops an incomplete frame
(§Loss recovery).

A rectangle change inside one coded size is **not** a configuration change: no new `config_id`, no
`hello_ack` re-send and no keyframe. `hello_ack.video.width`/`height` keep their meaning, the coded
size. A rung restart and a PyroWave switch stay configuration changes. The rectangle does not apply
while PyroWave is the active configuration (its payload is not an access unit), and the switch
back starts at the full frame.

The presenter crops to the rectangle and CA2's sharpen runs on it. Either may read past the
rectangle's edge; edge padding makes that read equal a clamp to the edge, up to coding error at
the seam, which is why the padding is edge pixels, not black. Not checked visually.

**Contract: CA5 needs the "active rectangle" amendment to `protocol/transport-webtransport.md`**
(additive, inside `wt_proto` 1, needs sign-off). Its points: a `hello` field by which a client says
it crops to an active rectangle; the agent keeping a fixed coded size only for such a client on an
encoder path with a canvas mode; the metadata unit's layout per codec, and that the client may
strip it before decoding; and that a rectangle change inside one coded size is not a configuration
change, while a rung restart or a PyroWave switch is one and resets the rectangle. This record does
not write the amendment.

## What would reopen the question

- **#1379's hardware and Mac rows pass, and a Vulkan AV1 encoder we deploy on gains
  `MOTION_VECTOR_SCALING`** (a new NVIDIA driver, or an AMD host with VCN AV1 encode). Then
  Design 3 could take over for AV1 and save the full-size encode and the padding. Design 4 stays
  for H.264 and HEVC either way.
- **Decode cost.** The client decodes the coded size for the whole band, however small the
  rectangle; the rectangle saves no decode work. If a weak client cannot hold that decode, the
  band narrows and the rung restart takes over sooner.
- **The owner sees 3 % steps on the Mac test client**, or real game content makes a step cost
  close to a keyframe. Then the step grows or the design falls back to coarser rungs.

## Left for CA5 (#1383)

The scale stage's canvas mode (a fixed canvas, a variable rectangle, edge padding): Vulkan
`vulkanscale` in the gst-wayland-display fork, where the fork-bump policy applies. Any other scale
arm (the CUDA/NVENC fallback until it gets one, VA, openh264) keeps CA1's rungs. The agent writes
the metadata unit. The amendment above. The client parses and strips the unit, and the presenter
crops with CA2's sharpen following the rectangle. The CA1 controller drives the rectangle under
§The band and §Step size and slew. The band measurement (CA5f), which must also run on HEVC before
Design 4 is enabled for HEVC. The owner's step check on the Mac test client, the throttle round,
and `make bench-check`. WebRTC (`<video>` cannot crop per frame) and PyroWave keep what they have.
