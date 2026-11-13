---
status: accepted
date: 2026-11-13
---
# On WebTransport, resolution moves inside a fixed coded size

On a WebTransport session, a resolution change stops being an encoder restart. The encoder keeps
coding frames at the launch size for the whole session, and the scale stage draws the game picture
into a smaller **active rectangle** inside that frame, padded with its edge pixels. The rectangle
grows and shrinks in steps of at most 3 % of each side. Every frame carries its rectangle in a
small metadata unit inside the access unit, and the presenter crops to it. This is Design 4 of
the research (#1372), chosen over AV1 reference scaling (Design 3) and over keeping coarser rung
steps. It covers H.264, HEVC and AV1 on both lab vendors, and it needs nothing new from the
browser's decoder. Spec #1375, ticket #1381; the build is CA5 (#1383).

## Evidence

- **Encoder capabilities** ([#1378 result](https://github.com/accreleus/quasar/issues/1378#issuecomment-6854339213),
  [review](https://github.com/accreleus/quasar/issues/1378#issuecomment-6855812856)). On the NVIDIA
  lab host (RTX 5090, driver 615.78.08), AV1 encode has `FRAME_SIZE_OVERRIDE` but **not**
  `MOTION_VECTOR_SCALING`, so a frame of a new size cannot predict from frames of another size,
  and each size change is in effect an intra frame. The AMD lab host (Raphael iGPU, RADV on
  Mesa 25.3.6) has **no AV1 encode**: H.264 and HEVC only. The bound on one picture's references
  is the per-codec L0/L1 count. On AMD these are HEVC P-L0 1 with no L1, and H.264 P-L0 1,
  B-L0 1, L1 1. On NVIDIA they are AV1 single-reference 7, HEVC P-L0 4 and L1 2, and H.264
  P-L0 7 and L1 1. Every codec on both hosts can code ordinary P frames, which is all Design 4
  asks of the encoder.
- **Browser decode of an AV1 size change on inter frames**
  ([#1379 result](https://github.com/accreleus/quasar/issues/1379#issuecomment-6854334513), issue
  still open). Chrome's software decoder (dav1d, Linux) decodes these streams with no error and no
  `configure()`, and it reports the size of each frame. Any `configure()` at the change makes the
  decoder demand a keyframe. **Hardware decode (NVIDIA and AMD on Linux) and the Mac test client
  were not tested.** Those are the clients a 1440p120 session runs on.
- **Bit cost of a zoom step against a restart**
  ([#1380 result](https://github.com/accreleus/quasar/issues/1380#issuecomment-6855892036)). The
  content was synthetic. On NVIDIA AV1 at CBR 1–10 Mbit/s, a 3 % step inside a fixed 1440p frame
  dips 0.5–0.8 dB of PSNR and is back within 4–7 frames. A restart with a keyframe dips 3–5 dB
  and takes 11–26 frames. At fixed QP the step costs +0.3 to +0.8 frame-equivalents of bytes, and
  the restart +1.7 to +5.9, on AV1 and on H.264 on both hosts. H.264 keyframes are not held to
  the one-frame budget: a restart puts 5–14 times a normal frame on the wire. That latency is what
  Design 4 removes. The 5 % steps cost 1/3 to 1/4 of a restart and pile up at low rates.
  A one-seat method review recomputed the tables from the raw data and found that every cell
  matches. It accepted the result with five corrections that this record applies:
  - The AV1 zoom dips are about one standard deviation of the frame-to-frame noise (0.3–0.7 dB).
    The restart's 3–5 dB is not.
  - The H.264 keyframe's 216 ms (NVIDIA) and 483 ms (AMD) of link time at 1 Mbit/s are an upper
    bound. They come from content that the encoder could not rate-control that low.
  - The Vulkan encoders have no one-frame VBV, in the spike or in production. AV1 keyframes ran at
    2.5–3.8 times the one-frame budget.
  - The spike ran with no periodic keyframe, where production has one per second. The ratio
    survives that, and the restart's cost is slightly overstated if anything.
  - The ramp figures are approximate.
  All of these ratios hold for this content only.

## What each option covers

| | NVIDIA AV1 | NVIDIA H.264 / HEVC | AMD H.264 / HEVC | Linux software decode | Linux hardware decode, Mac |
|---|---|---|---|---|---|
| **Design 4, fixed coded size** (chosen) | yes, measured | H.264 measured; HEVC by inference | H.264 measured; HEVC by inference | nothing new to decode | nothing new to decode, *if* the decoder ignores the metadata unit (to check) |
| **Design 3, AV1 reference scaling** | only as an intra frame per size change | no (a new size needs an IDR) | no (no AV1 encode) | yes | unknown (#1379) |
| **Coarser rungs** (CA1 alone) | yes | yes | yes | yes | yes, with a reconfigure and a keyframe per step |

Design 3 on the hardware we have amounts to an intra frame at each size change. That frame costs
what a restart's keyframe costs (3–5 dB over 11–26 frames). The only saving is the decoder
reconfigure, the saving covers AV1 alone, and the decode side is unproven on the clients that
matter. Coarser rungs keep the freeze and the keyframe stall that the spec's cause 3 sets out to
remove.

## Step size and slew

- **At most 3 % of each side per step, and the rectangle 8-pixel aligned**, as measured. At 5 %
  the steps start to pile up at low rates.
- **One step every 6 frames at most at 60 fps (100 ms).** At that cadence a full 1 → 0.5 → 1
  ramp cost roughly 0.3 dB of mean PSNR at most (approximate, per the review). A full-size to half-size softening takes about 1.7 s. An
  isolated step on AV1 at 1–2.6 Mbit/s needs up to 8 frames to recover fully, so a controller that
  is not chasing a collapse spaces its steps 8 frames apart. A 120 fps session keeps the same
  spacing in time (12 to 16 frames) until it is measured.
- **The band is 1 down to 0.5 of the launch side**, the range measured. The owner approved going
  below 720p (#1375). A size below the band is still one rung restart into a smaller coded size,
  and the rectangle moves inside that size from there.
- **The way up is the same step, no faster than the rate estimate grows.**
- A collapse that #1374 answers with one jump may still take the rectangle there in one large
  step. That is no restart and no reconfigure, but the frame costs about a keyframe. Not measured.

## Where the rectangle travels

**It travels in the access unit**, as a metadata unit the agent writes into each frame's payload:
an AV1 metadata OBU, or an H.264/HEVC SEI `user_data_unregistered`. The client reads it from the
chunk bytes before decoding. It rides every frame, so a lost frame, a NACK resend or FEC needs
no extra state.

We did not put it in a datagram header field. `transport-webtransport.md` §Versioning says a
change to a binary header needs a new `wt_proto`. That cost buys nothing over a few bytes in the
payload, and a header field would repeat on every fragment.

The coded size never changes, so a rectangle change is **not** a configuration change: no new
`config_id`, no `hello_ack` re-send and no keyframe. `hello_ack.video.width`/`height` keep their
meaning, the coded size.

**Contract: CA5 needs the "active rectangle" amendment to `protocol/transport-webtransport.md`**
(additive, inside `wt_proto` 1, needs sign-off). It covers four points: a `hello` field by which
a client says it crops to an active rectangle; the agent using a fixed coded size only for such a
client; the metadata unit's layout per codec; and the rule that a rectangle change is not a
configuration change. A client that did not ask keeps CA1's rungs. This record does not write the
amendment.

## What would reopen the question

- **#1379's hardware and Mac rows pass, and a Vulkan AV1 encoder we deploy on gains
  `MOTION_VECTOR_SCALING`.** Examples are a new NVIDIA driver or an AMD host with VCN AV1 encode.
  Then Design 3 could take over for AV1 and save the full-size encode and the padding. Design 4
  stays for H.264 and HEVC either way.
- **A hardware decoder or VideoToolbox rejects or strips the metadata unit.** The rectangle then
  moves to a datagram field under a new `wt_proto`.
- **The owner sees 3 % steps on the Mac test client**, or real game content (not the synthetic
  clip) makes a zoom step cost close to a keyframe. Then the step grows or the design falls back
  to coarser rungs.
- **At the bottom of the band, H.264 at a fixed coded size cannot get under the link.** On the
  synthetic clip, full-frame 1440p H.264 sat at its QP ceiling at about 2.6 (NVIDIA) and
  3.1 (AMD) Mbit/s. Then the band narrows and the rung restart takes over sooner.

## Left for CA5 (#1383)

The scale stage draws into a fixed canvas at a variable rectangle with edge padding: Vulkan
`vulkanscale` first, in the gst-wayland-display fork, where the fork-bump policy applies, then the
CUDA arm. The agent writes the metadata unit. The amendment above. The client parses the rectangle
and the presenter crops to it, with CA2's sharpen strength following the rectangle. The CA1
controller drives the rectangle under the step and slew above, with the ABR floor following the
rectangle's area. Before HEVC is turned on, a measurement on HEVC and on real game content. The
owner's step check on the Mac test client. The throttle round, and `make bench-check`. WebRTC
(`<video>` cannot crop per frame) and PyroWave keep what they have.
