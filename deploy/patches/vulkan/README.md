# Vulkan encode patches (vendored)

> **The one-off probes cited below are not in the tree.** `vulkan_rc_retarget_probe.c`,
> `intra_refresh_caps_probe.c`, `intra_refresh_bitstream_probe.py`,
> `vkvideoencodeav1cbr.c` and friends were investigation instruments that lived under
> `deploy/diag/` and `deploy/experiments/`, removed 2026-08-27 when `deploy/` became the
> operator front door. They are named here because they are the *provenance* of these
> measurements; recover any of them from git history (`git log --diff-filter=D -- deploy/diag`).

> **BUILD MODEL CHANGED 2026-07-26 — the `gst-wayland-display-*.patch` files in this directory are
> NO LONGER APPLIED AT BUILD TIME.** The image now builds the Quasar fork's `develop` branch
> (`github.com/salty2011/gst-wayland-display`), which is upstream `43d4c25` plus these ten changes
> as real commits, one per former patch. The branch tip was verified byte-identical to the old
> clone-then-apply-ten result, so nothing about the compiled output changed.
>
> These files are retained as (a) the authored record with the rationale in this README, and (b) the
> source for upstream submissions. Each maps 1:1 to a commit on `develop`. **If you change the
> compositor, change it on `develop` and re-diff the file here so the two stay in step** — the
> procedure is in `docs/third-party-pins.md`.
>
> The **GStreamer** patches here (`vkh264enc-*`, `vulkanh265enc`, `gstreamer-vulkan-global-submit-lock`)
> ARE still applied at build time. Only the `gst-wayland-display-*` ones moved into the branch.

These patches split across **two upstreams**: ten apply to a from-source **GStreamer**
`1.28.4` checkout (`gst-plugins-bad`), while the patches named `gst-wayland-display-*` apply to the
**gst-wayland-display** compositor checkout itself (see their dedicated sections below). Both sets
are applied inside `deploy/Dockerfile.vulkan`, each at its own upstream's build step.

The first two GStreamer patches originate from
[games-on-whales/gst-wayland-display](https://github.com/games-on-whales/gst-wayland-display)
at commit [`43d4c25`](https://github.com/games-on-whales/gst-wayland-display/commit/43d4c25)
(the PR [#37](https://github.com/games-on-whales/gst-wayland-display/pull/37)
`feat/vulkan-encode` branch), from its `patches/` directory:

- `patches/vkh264enc-dpb-pool-in-new-sequence.patch` — **vendored verbatim**.
- `patches/vulkanh265enc.patch` — **Quasar-modified** (was verbatim from PR #37; a
  bitstream-conformance fix on NVIDIA was folded in 2026-07-24, see its section
  below). Re-fetching it verbatim from a new pin would drop that fix — re-apply it.

The remaining GStreamer patches are **Quasar-authored** (not from gst-wayland-display):

- `vkh264enc-rc-fix.patch`
- `vkh264enc-num-slices.patch`
- `gstreamer-vulkan-global-submit-lock.patch`
- `vkh264enc-intra-refresh.patch`
- `gstreamer-vulkan-rc-retarget-no-reset.patch`
- `vulkanav1enc.patch`
- `vulkan-enc-output-state-on-resize.patch`
- `vkenc-bitstream-buffer-pool.patch`
- `vkh265enc-profile-template.patch` (**candidate — not applied**, see below)

They are applied — **in this order** — to a from-source GStreamer `1.28.4`
checkout (see `deploy/Dockerfile.vulkan`); the rc-fix patch touches the same
`new_sequence` region as the DPB patch, so it must apply **after** it, and the
num-slices patch touches `new_sequence`, `_setup_slice`, `_setup_codec_pic`, and
the property table on top of the rc-fix state, so it applies **fourth**. The global-submit-lock
diagnostic applies **fifth**, and the intra-refresh patch applies **sixth**: it was diffed against the tree with all five earlier patches applied (it shares
`new_sequence`, `_setup_codec_pic`, the frame struct, and the property table with num-slices, and
`gstvkdevice.c` with the submit-lock diagnostic), so it applies **sixth**. The
rc-retarget-no-reset library fix applies **seventh** (it edits
`gst_vulkan_encoder_request_rc_reset`, introduced by the rc-fix patch), and
`vulkanav1enc.patch` applies **eighth**: it was diffed against the tree with all seven
earlier patches applied and shares `ext/vulkan/meson.build` + `gstvulkan.c` registration context
with `vulkanh265enc.patch`. `vulkan-enc-output-state-on-resize.patch` applies **ninth (last)**: it
edits the `new_sequence()` of `vkh264enc.c` and `vkh265enc.c`, which the dpb-pool, rc-fix,
num-slices and intra-refresh patches (h264) and `vulkanh265enc.patch` (h265) all shape first, and
its rationale references the AV1 element the eighth patch introduces.
`vkenc-bitstream-buffer-pool.patch` applies **tenth (last)**: it was diffed against
`gst-libs/gst/vulkan/gstvkencoder-private.c` with all nine predecessors applied, and that file is
shaped by the rc-fix, intra-refresh, rc-retarget and output-state-on-resize patches before it.
None of the ten are upstreamed into GStreamer itself yet.

`vkh265enc-profile-template.patch` is an **eleventh candidate that is NOT wired into
`deploy/Dockerfile.vulkan` and is therefore NOT applied to any image today.** It is a
correctness fix for an upstream defect the Quasar encode path already sidesteps by pinning
`profile=main` in its capsfilter, so it buys nothing for a Quasar image and everything for
anyone driving `vulkanh265enc` by hand. When it is wired up it would apply **eleventh (last)**: it
edits `gst_vulkan_h265_encoder_register()` in `vkh265enc.c`, a region no earlier patch
touches, so its position is a convention rather than a constraint. See its section below.

The gst-wayland-display patches (whose names begin `gst-wayland-display-`) are applied to a *different*
checkout (gst-wayland-display, not GStreamer) at a *different* Dockerfile step, so it has no
ordering relationship to the five above.

## What each does

### `vkh264enc-dpb-pool-in-new-sequence.patch`

**Mandatory.** Moves `gst_vulkan_encoder_create_dpb_pool()` out of
`gst_vulkan_h264_encoder_propose_allocation()` and into
`gst_vulkan_h264_encoder_new_sequence()`.

Stock `vah264enc`/`vulkanh264enc` create the DPB (decoded-picture-buffer) pool
when answering the allocation query, which normally arrives after the encoder's
caps are already set. Behind an `interpipesrc` boundary (Quasar's launcher↔game
swap seam, see `CLAUDE.md`'s gst-interpipe notes), the allocation query can
arrive **before** `set_format`/`new_sequence` has run — the encoder then tries
to build the DPB pool with no sequence info yet and asserts. Deferring pool
creation to `new_sequence` (which always runs after the format is known)
removes the ordering dependency. Without this patch, `vulkanh264enc` is not
safely usable downstream of `interpipesrc`.

### `vulkanh265enc.patch`

**Optional; Quasar-modified (no longer verbatim from PR #37).** Adds a
`vulkanh265enc` element (H.265/HEVC Vulkan video encode) to `gst-plugins-bad`,
mirroring the existing `vulkanh264enc`. The node-agent produces H.265 with it by default
(`QUASAR_VULKAN_HEVC=0` disables it — see `docs/configuration.md`); it is
vendored because the future **native UDP client** is where HEVC matters (native
decoders are not constrained to Chrome's WebRTC receiver) and building it once
alongside the DPB patch keeps the PR #37 set whole rather than cherry-picked.

**Quasar bitstream-conformance fix (2026-07-24).** As vendored from PR #37 the
element emitted a **non-conformant** stream on NVIDIA. It maintains two SPS
representations: the driver-consumed *std* SPS derives CTB geometry from
`VkVideoEncodeH265CapabilitiesKHR.ctbSizes` (NVIDIA advertises only
`CTB_SIZE_32` → the driver encodes 32×32 CTBs), but the *bitstream* SPS is
bit-written from the hand-rolled `GstH265SPS`, whose `gsth265encoder.c`
`sps_init()` hardcodes `log2_diff_max_min_cb=3` → **CtbSize 64**. The decoder
then parsed the coding tree / slice data on a 2×-wrong CTB grid (garbage
`cu_qp_delta`, `alignment_bit_equal_to_one=0`, a cascading RPS/POC failure on
inter frames) — black video on VideoToolbox. RADV advertises `CTB_SIZE_64`, so
it matched the hand-rolled value and appeared to work. The element already
fetched the driver's authoritative SPS/PPS via
`vkGetEncodedVideoSessionParametersKHR` (`writeStdSPS/PPS=VK_TRUE`) but
**force-zeroed** `hasStdSPS/PPSOverrides` and so discarded them. The fix
(`vkh265enc.c`, resolution spec
`docs/design/plans/2026-07-24-vulkanh265enc-conformance-resolution-spec.md`)
landed in two stages:

1. **CTB geometry (Option A1 — makes it decode).** Remove the force-zero so the
   override-gated adopt path runs, and fix a latent return-value bug in the
   never-before-exercised `_h265_parameters_parse()` helper
   (`gst_h265_parser_identify_nalu()` returns `NO_NAL_END` on the final NAL of
   the SPS+PPS blob — a successful full parse the helper mis-reported as
   failure). Gating on the override flags keeps RADV byte-identical (it reports
   no override). This alone got the stream to **decode** on VideoToolbox (driver
   CtbSize 32 == bitstream CtbSize 32).

2. **Robust form — emit the driver's bytes verbatim (kills the flicker).**
   Stage 1 still *re-bit-wrote* the adopted `GstH265SPS` via
   `gst_h265_bit_writer_sps()`, and that parse→rewrite round-trip malformed the
   SPS extension (`sps_extension_present_flag=1`, `sps_extension_4bits=1`, which
   should be 0). ffmpeg tolerates it; macOS VideoToolbox decoded but **flickered
   every frame** (gate-5). The robust form keeps the driver's authoritative
   SPS/PPS NAL bytes (Annex B, from the overrides call) and `_write_headers()`
   emits them **verbatim** instead of bit-writing `self->sps/pps` — round-trip
   free, so `sps_extension_present_flag` is back to 0 and VideoToolbox receives
   the driver's native bytes. `self->sps` is retained only for the coded-size
   estimate and caps. `_setup_slice()` was also changed to source its
   in-loop-filter flags (`slice_sao_*`, `cu_chroma_qp_offset_enabled_flag`,
   `deblocking_filter_override_flag`) from the authoritative SPS/PPS rather than
   the upstream MR #5739 hardcodes — spec-correct hygiene, though it is a no-op
   on the emitted bitstream (the driver treats those std-struct flags as no-ops
   at encode time; verified byte-identical).

3. **Output-caps colorimetry — signal the color-space (kills the brightness
   toggle).** After stage 2 VideoToolbox decoded cleanly but the picture showed
   **whole-image light/dark alternation** (rapid per-frame brightness toggling);
   NVENC HEVC through the identical client/pipeline was clean, and host-side
   ffmpeg decode showed a perfectly flat luma timeline (mean 122.63, stdev 0.03)
   — i.e. the toggle is purely receiver **interpretation**, not the pixels. Root
   cause: `nvh265enc`/`vah265enc` put `colorimetry`/`chroma-site` on their coded
   `video/x-h265` output caps; the vendored `vulkanh265enc` did not. That caps
   field propagates through `h265parse` → `rtph265pay` → `webrtcbin` and is how
   the WebRTC **color-space** is signalled — absent it, Chrome/VideoToolbox has
   no out-of-band color-space and guesses per frame (light/dark toggle) even
   though the in-band VUI is correct (`h265parse` does **not** back-fill
   colorimetry from the VUI — verified). `new_parameters()` now propagates
   `self->in_state->info` colorimetry + chroma-site onto the output caps, exactly
   like the other encoders (verified live: encoder src caps carry
   `colorimetry=bt709, chroma-site=mpeg2`).

Verified on Tower (RTX 5090): all-intra and 60-frame-GOP captures decode 120/120
frames with 0 errors under jellyfin-ffmpeg strict decode (`-err_detect +explode`,
versus 60/120 and 2/120 pre-fix), `sps_extension_present_flag=0`, all SPS NALs
byte-identical, host-decode luma timeline flat, and the live node-agent HEVC path
carries the color-space on its caps. The RADV regression gate and Michael's
browser (VideoToolbox) re-test remain outstanding.

**Quasar live-ABR fix (2026-07-26).** `vulkanh265enc` had the **whole** rate-control
defect set that `vkh264enc-rc-fix.patch` fixes for H.264, and none of the fixes; the
one node-agent diagnostic that should have caught it could not fire, because
`rearm_vulkan_rc` warns when `bitrate` is not `GST_PARAM_MUTABLE_PLAYING` and this
patch *does* set that flag, so everything host-side looked correct. Measured on Tower
with `vulkan_rc_retarget_probe.c` before the fix: an 8000 kbps CBR request
produced **~550 Mbps** at 1080p60 snow (i.e. constant-QP), a retarget to 2000 changed
nothing that tracked (ratio 0.67), and it emitted an extra key frame one frame after
the retarget, realigning the GOP. Three changes, each the H.264 patch's counterpart:

- **Fix A** — push the property-selected rate-control mode into the encoder after
  `gst_vulkan_encoder_start()`, *before* reading it back. Without it the read-back
  overwrites the user's `cbr` with the driver default and the stream is silently
  constant-QP. This is why the element never had working CBR at all, let alone ABR.
- **Fix B1** — a live `bitrate` write on a started encoder updates the rate-control
  values in place instead of calling `gst_h265_encoder_reconfigure()`, whose
  `configure()` drains and resets the GOP (→ an IDR per retarget).
- **Fix B2** — the `new_sequence` same-profile early return calls
  `gst_vulkan_encoder_request_rc_update()`, so a change that *does* go through a
  reconfigure still reaches the driver.

B1/B2 use the API `vkh264enc-rc-fix.patch` introduces, which is why this patch applies
*before* it in the chain: the patches only have to apply in order, not compile in
order. After: 7.49 Mbps against the 8000 kbps setpoint, 1.11 after the retarget to
2000 (ratio 0.15), key frames at exactly `0 60 120 180 240 300 360 420` —
`RETARGET_VERDICT=PASS`. See
`docs/design/plans/2026-07-26-vulkan-abr-retarget-defects-spec.md`.

Because of this the patch is **regenerated against the DPB baseline** (like the
Quasar-authored patches below), not re-fetched: if `GST_WAYLAND_DISPLAY_REF` or
the DPB patch moves, re-diff this against the new baseline (re-apply the fix)
rather than re-fetching it verbatim from the new commit's `patches/` directory.

### `vkh264enc-rc-fix.patch`

**Mandatory for any bitrate-controlled `vulkanh264enc` use** (i.e. the
node-agent's ABR/CBR path). Fixes two rate-control defects in stock GStreamer
`1.28.4` `vulkanh264enc` (both present at upstream HEAD; unreported as of
2026-07-05). It touches `ext/vulkan/vkh264enc.c` plus a small additive API on
the shared encoder library (`gst-libs/gst/vulkan/gstvkencoder-private.{c,h}`).

- **Fix A — `rate-control=cbr` set before PLAYING is lost.** The element's
  `_reset_rc_props()` bails before pushing the property-selected mode to the
  Vulkan encoder (it early-returns while the encoder has no caps yet, which is
  always the case before `gst_vulkan_encoder_start()`). The started encoder is
  then left at the driver default (Renoir reports DISABLED/cqp as supported, so
  the validate step keeps it), and `new_sequence` reads that back and
  **overwrites** the `cbr` property with `cqp` — silently producing a
  constant-QP stream (~141 Mbps for max-entropy 1080p60). The patch pushes the
  requested mode into the encoder immediately after `start`, before the
  read-back, so the user's `cbr`/`vbr` selection actually takes.

- **Fix B — live `bitrate` changes while PLAYING never retarget.** A bitrate
  change triggers a reconfigure, but because the video profile is unchanged,
  `new_sequence` takes its "already started, same profile" early-return and
  never re-applies rate control. The refreshed bitrate reaches the per-frame
  rate-control layer info but is only pushed to the driver by
  `vkCmdControlVideoCoding`, which a bitrate change never issues. The patch adds
  two one-shot request helpers on the shared encoder library —
  `gst_vulkan_encoder_request_rc_reset()` (the pre-existing one) and
  `gst_vulkan_encoder_request_rc_update()` — and calls the **update** one on that
  early-return path, so the driver re-applies the new bitrate on the next encode.

- **Fix B1 (2026-07-26) — the retarget must not reconfigure the element.**
  `set_property(PROP_BITRATE)` called `gst_h264_encoder_reconfigure()`, and the
  base class's `configure()` **drains** the encoder and calls
  `gst_h264_encoder_reset()`, which zeroes `gop.cur_frame_index` — so the very
  next frame is an IDR that restarts the GOP. **This, not the Vulkan session
  reset below, is where the IDR-per-retarget actually came from**; measured on
  Tower with `vulkan_rc_retarget_probe.c`, `idr-period=60` and a
  retarget at frame 245 gave key frames at `0 60 120 180 240 246 306 366 426` —
  an extra key one frame after the retarget, with the GOP realigned to it, so
  under sustained ABR the keyframe interval collapses to the retarget interval.
  A live `bitrate` write on a started encoder now updates `rc.bitrate` /
  `rc.max_bitrate` in place (`_update_live_bitrate()`, mirroring
  `_configure_rate_control()`'s rate half) and requests a reset-free re-apply,
  skipping the reconfigure entirely. A pre-`start()` write is unchanged. After:
  `0 60 120 180 240 300 360 420 480`, `RETARGET_VERDICT=PASS`. Confirmed live on
  Tower under `qnetem` (H.264 1440p60, ABR `smooth`, `idr-period=60`), counting
  `gst_h264_encoder_print_gop_structure` — one per `configure()` — in the agent
  log: **11 GOP regenerations for 10 ABR retargets before, 1 for 17 after**.

- **Fix B2 (2026-07-26) — the retarget must not reset the video session.** As
  first shipped, Fix B used `request_rc_reset()`, which sets the library's
  `session_reset` flag; `gst_vulkan_encoder_encode()` then issues
  `vkCmdControlVideoCoding` with a hardcoded
  `QUALITY_LEVEL | RATE_CONTROL | **RESET**` flag triple. `RESET_BIT` resets the
  video session and invalidates the DPB, so **every live ABR retarget cost a key
  frame** — up to one per 2 s of sustained congestion (the ABR governor's
  `DEFAULT_MIN_INTERVAL`), i.e. a bitrate spike precisely when the path is already
  congested. `request_rc_update()` is the split: a separate `rc_update` flag that
  issues `ENCODE_RATE_CONTROL_BIT` **only** (no `RESET_BIT`, and no
  `QUALITY_LEVEL_BIT` — the quality level is not changing), leaving the DPB
  intact. `session_reset` keeps today's three bits and wins the race if both are
  pending (a reset re-applies rate control anyway). Like the reset path, the
  update frame passes `begin_coding.pNext = NULL` rather than an rc_info that
  does not yet describe the session's state. Spec:
  `docs/design/plans/2026-07-26-vulkan-abr-retarget-defects-spec.md`. **B1 and B2
  are both required**: B2 alone leaves the IDR (verified — the before/after images
  gave an identical `unexpected_keyframes_after_retarget=1`), and B1 alone would
  update `rc.bitrate` with nothing to deliver it to the driver.

  Measured on Tower (RTX 5090, driver 595.80) with
  `vkvideoencodeav1cbr.c`'s `rcupd-*-midgop`
  cases — the reset-free control path is codec-agnostic, so the AV1 harness
  exercises exactly the library code H.264/H.265 use. **The driver accepts it and
  the setpoint takes effect one frame later**: 8→2 Mbps mid-GOP gave
  seg1 8.661 / seg2 2.059 Mbps (+2.97%), 2→8 gave 2.171 / 8.402 (+5.02%), both
  with **0 key frames after the retarget** and 240/240 OBUs parsing. The
  `request_rc_reset()` control needed ~3 frames to converge and, at the element
  level, a key frame. (Same run also fixed a latent harness bug: the sequence
  header's `order_hint_bits_minus_1` was unclamped, illegal for any GOP > 255.)

- **Not fixed (driver limitation, C): CBR overshoot on Renoir VCN with
  max-entropy content.** With A+B in place the correct CBR mode + average/max
  bitrate + session reset are verifiably delivered to the driver
  (`GST_DEBUG=vulkanencoder:ERROR` shows `avg=…max=…`), yet RADV's Renoir VCN
  encoder does **not** honor low CBR setpoints for `videotestsrc pattern=snow`:
  it floors at ~15 Mbps and only tracks the target once the target exceeds that
  floor (measured: target 2000→15017 kbit/s, 8000→15023, 20000→19096). For
  lower-entropy content (`pattern=ball`, closer to real game frames) the same
  build tracks the target cleanly (2000→~1.5 Mbps, 8000→~5.5 Mbps) and the live
  retarget works (pre 8288 → post 2170 kbit/s, ratio 0.26). So C is a
  RADV/hardware constraint, not a pipeline bug, and is left as a known
  limitation.

Evidence: see the **G2 entry in `docs/reports/VULKAN-WORKLOG.md`** (probe:
`g2_bitrate_probe.c`, run on hermes Renoir in
`quasar-vulkan:latest`).

**Upstream status:** unreported as of 2026-07-05 — issue to be filed against
[gstreamer/gstreamer](https://gitlab.freedesktop.org/gstreamer/gstreamer)
(A + B are upstream bugs, present in `1.28.4` and at HEAD).

### `vkh264enc-num-slices.patch`

**Mandatory for lossy (WiFi) clients** — the last blocker before `vulkanh264enc`
can be the AMD default (issue #367). Stock `1.28.4` `vulkanh264enc` emits **one
slice per frame** (the element hardcodes `naluSliceEntryCount = 1`; its own source
carries a `TODO: + support multi-slices`). Over a bursty-loss WiFi path a single
lost packet destroys the whole frame, and with `idr-period=60` that costs up to a
full second of received frames — a real-client A/B on hermes Renoir measured the
single-slice `vulkanh264enc` stream collapsing to **8-13 received fps** while the
agent delivered 54-60, whereas `vah264enc` (which emits `num-slices=8`) rode at
**~50 fps** on the identical path (see the `2026-07-05T14:00Z` SOAK FINDING entry
in `docs/reports/VULKAN-WORKLOG.md`).

This patch adds a `num-slices` `guint` property (default **1**, so the default
output is byte-for-byte the historical single-slice stream and non-Quasar
consumers are unaffected — the patch is upstreamable) that splits each frame into
N macroblock-row-aligned H.264 slices. `mb_rows = ceil(height/16)` rows are
distributed as evenly as possible across the N slices, each getting its own
`StdVideoEncodeH264SliceHeader` (with `first_mb_in_slice = start_row * mb_width`)
and `VkVideoEncodeH264NaluSliceInfoKHR` entry (replicating the CQP `constantQp`);
`naluSliceEntryCount`/`pNaluSliceEntries` on the picture info point at the filled
arrays. The value is clamped at encode time to the number of MB rows **and** to the
driver's `VkVideoEncodeH264CapabilitiesKHR.maxSliceCount` (a `GST_WARNING` is
logged when clamping). It touches only `ext/vulkan/vkh264enc.c` (no encoder-library
change — the slice split lives entirely in the element).

Evidence (hermes Renoir, `quasar-vulkan:latest`, `--device /dev/dri`, mounted
rebuilt `libgstvulkan.so`; probe `slice_count_probe.c` counts VCL NALs
per encoder-src buffer): **Renoir `maxSliceCount = 128`**; `num-slices=1` → 1
slice/frame (default preserved), `num-slices=8` → exactly 8 slices/frame across
120 frames, uneven counts (3/5/7) split without error, and `num-slices=999`
clamps to 68 (the 1080p MB-row count, itself below the 128 driver cap) with the
warning logged. See the `2026-07-05` multi-slice entry in
`docs/reports/VULKAN-WORKLOG.md` for the NAL-count table and the netem loss A/B.

**Upstream status:** unreported as of 2026-07-05 — same filing as the rc-fix; the
`num-slices` property mirrors `vah264enc`/`nvh264enc` naming, so it is directly
upstreamable.

### `gstreamer-vulkan-global-submit-lock.patch`

**Diagnostic A/B arm; not an accepted production fix.** Stock GStreamer `1.28.4` stores the
submission mutex in each `GstVulkanQueue` wrapper. Distinct wrappers can refer to the same raw
`VkQueue`, so their mutexes would not satisfy Vulkan's external-synchronization rule for that
handle. Tower normally selects distinct graphics/compute and VIDEO_ENCODE queue families, however,
so the converter and encoder are expected to use different raw queues. Tower has produced
stochastic Xid 32 failures during session pipeline bring-up, where both queues begin submitting.

This patch changes `gst_vulkan_queue_submit_lock()` / `_unlock()` to use one process-global GLib
mutex for every wrapper and queue. This is deliberately broader than an eventual keyed-by-raw-
queue production design: it tests whether suppressing cross-queue host submission concurrency or
changing its timing removes the failure while preserving the public API and all existing call
sites. The matching gst-wayland-display NVIDIA-sync patch routes both raw submits through that API
when using the shared-device Vulkan encode path; `WOLF_VULKAN_DUMP` is diagnostic-only. Compare
locked and stock builds under the same repeated-session matrix. Passing this arm alone does not
prove a same-queue Vulkan violation, establish root cause, or satisfy Wave acceptance.

The patch also emits one bounded `vulkandevice` INFO record whenever GStreamer creates a queue
wrapper. Enable it with `GST_DEBUG=vulkandevice:6`; the record includes wrapper pointer, raw
`VkDevice`, raw `VkQueue`, family/index, and `global-lock=enabled`. It does not log per frame.

**Authored against:** GStreamer `1.28.4`, after the four encode feature/fix patches. LGPL-2.1+
applies to the resulting modified GStreamer source.

### `vkh264enc-intra-refresh.patch`

**Optional; the browser-reachable half of #227** (Periodic Intra Refresh). Adds rolling intra
refresh to `vulkanh264enc` via `VK_KHR_video_encode_intra_refresh` (Vulkan 1.4.321, July 2025):
instead of a periodic full IDR, each frame carries one region's worth of intra macroblocks and a
full sweep of the picture completes every N frames, bounding loss recovery without an IDR bandwidth
spike and giving GCC/ABR one fewer transient to misread. It complements `num-slices` (spatial damage
bound) with a temporal reference-chain bound. Touches `ext/vulkan/vkh264enc.c` plus the shared
encoder library (`gst-libs/gst/vulkan/gstvkencoder-private.{c,h}`) and the device-extension list
(`gst-libs/gst/vulkan/gstvkdevice.c`).

Two element properties, both **default-off / 0 so the default output is byte-for-byte the historical
stream** (upstreamable, mirroring `num-slices`):

- `intra-refresh` (boolean, default `false`) — enable rolling refresh.
- `intra-refresh-period` (guint, default `0`) — frames between the start of consecutive refresh
  waves; `0` (or any value below the cycle length) means continuous, back-to-back cycles.

**Mode auto-selection (deviates from the spec's per-picture-partition-only plan, justified by the A0
capability probe `intra_refresh_caps_probe.c`):** the two target GPUs expose *different*
mode families, so the shared encoder negotiates in `gst_vulkan_encoder_start()` against
`VkVideoEncodeIntraRefreshCapabilitiesKHR`:

- **Tower RTX 5090 (NVIDIA 595.80):** only `PER_PICTURE_PARTITION` (maxCycle 64). Chosen as the
  primary mode; cycle duration = the coded slice count — i.e. the requested `num-slices` clamped to
  the macroblock-row count and `maxIntraRefreshCycleDuration` (the H.264 VU requires cycle ==
  slice count, so the element clamps the requested region hint to mb-rows before start and additionally
  guards per frame, skipping intra refresh if the two ever diverge). One slice per frame is coded intra
  and the refreshed slice rotates. Verified: with `num-slices=8` the refreshed slice is an **I-slice**
  among seven P-slices whose index rotates `0..7` with period 8; with `num-slices=50` at 720p the cycle
  clamps to 45 (mb-rows) and the I-slice sweeps `0..44` with period 45 — no extra IDRs, no driver
  rejection.
- **hermes Renoir (RADV Mesa 25.3.6):** `BLOCK_BASED|BLOCK_ROW_BASED|BLOCK_COLUMN_BASED` (no
  per-picture-partition), `partitionIndependentIntraRefreshRegions=true`, maxCycle 256. Falls back to
  `BLOCK_ROW_BASED` (legal alongside multi-slice because regions are partition-independent); cycle =
  slice count clamped to mb-rows and `maxIntraRefreshCycleDuration` (or 8 when `num-slices=1`). Verified:
  heavy-slice band sweeps with period 8 (`num-slices=8`) and period 45 (`num-slices=50` at 720p, clamped
  from 50 to mb-rows), no extra IDRs.

If neither usable mode is present (or the extension/header is absent) the encoder logs a
`GST_WARNING` and runs **with intra refresh disabled** — it never fails the session (fail-open, per
spec §2.1/§2.3). IDR cadence (`idr-period`) and forced key units are unchanged; forced key units
still emit a real IDR.

**Reference constraint (`maxIntraRefreshActiveReferencePictures = 1` on both GPUs):** while a frame
is inside a refresh wave the encoder must use at most one active reference. The element guards this
per frame and skips marking a frame as intra-refresh (logging a warning) if the GOP handed it more
references, so a mis-set reference count degrades to plain P frames rather than a driver error.
**Consumers that enable intra refresh must therefore run `num-ref-frames=1`** (the node-agent's
low-latency default) — otherwise the driver's default multi-reference GOP disables intra refresh
every frame (observed on Tower with the stock 3-reference default).

**Known limitation:** on the per-picture-partition path, `intra-refresh` with `num-slices=1` yields a
cycle duration of 1 — i.e. every frame is fully intra (all-I), which is not useful. Pair intra
refresh with `num-slices>1` (the node-agent default is 8). The block-row path does not have this
degeneracy (its cycle defaults to 8 for a single slice).

Evidence (both hosts, `quasar-vulkan:latest`, rebuilt `libgstvulkan*` mounted over the image;
capture via `filesink` + parse with `intra_refresh_bitstream_probe.py`): see the `#227
A1` entry in `docs/reports/VULKAN-WORKLOG.md`.

**Upstream status:** unreported as of 2026-07-20; same filing batch as the rc-fix/num-slices patches.
The `intra-refresh` property naming mirrors `vah264enc`/`nvh264enc`, so it is directly upstreamable.

### `gstreamer-vulkan-rc-retarget-no-reset.patch`

**Library fix, codec-neutral (vulkanh264enc / vulkanh265enc / vulkanav1enc).** Two fixes to
`gst-libs/gst/vulkan/gstvkencoder-private.c`: (A) `gst_vulkan_encoder_request_rc_reset()` raises
only `VK_VIDEO_CODING_CONTROL_ENCODE_RATE_CONTROL_BIT_KHR` — previously it ORed in
`RESET_BIT_KHR` unconditionally, resetting the video session, invalidating the DPB, and forcing an
IDR on every live bitrate retarget (an ABR loop retargeting ~1/s would keyframe ~1/s). Genuine
session (re)starts keep `RESET_BIT` (`gst_vulkan_encoder_start` / `set_rc_mode` set
`session_reset` directly). (B) `gst_vulkan_encoder_picture_clear` released DPB slots with
`slotIndex > 0`, so slot 0 — typically the first frame of a session — leaked for the session's
lifetime; `>= 0` (the unset sentinel is `-1`). Depends on `vkh264enc-rc-fix.patch` (which
introduces `request_rc_reset`). Applies seventh. See the `vulkanav1enc-campaign` record for the
overlap analysis.

### `vulkanav1enc.patch`

**The Vulkan Video AV1 encoder element — see the patch's own header for the full description.**
Seven files: `ext/vulkan/base/gstav1encoder.{c,h}` (AV1 base class, forward-only, property writes
never raise `need_configure` so the reconfigure-IDR class is unrepresentable),
`ext/vulkan/vkav1enc.{c,h}` (driver-emitted sequence header verbatim, TD+concat temporal-unit
assembly, colorimetry on output caps, CDEF/loop-filter/tile quality tuning, 200 ms VBV CBR),
registration in `meson.build`/`gstvulkan.c` (rank NONE, like vulkanh265enc), and an
`av1_encode_caps()` addition to `ext/vulkan/gstvkvideocaps.c` (stock 1.28.4 returns
`FALSE /* unimplemented */` for ENCODE_AV1, vetoing registration on every device). Applies
eighth of nine (`vulkan-enc-output-state-on-resize.patch` is now the last). Gated in Quasar by `QUASAR_VULKAN_AV1`, which is ON by default since
2026-08-22 (setting it to 0, or building an image without this patch, takes the NVENC fallback
instead). Validated 2026-08-21 on RTX 5090 / driver 610.57.04: 120/120 + 600/600 frames
decoded by `vulkanav1dec`, CBR 8 Mbps → 9.55 Mbps over 600 frames, upstream
`libs_vkvideoencodeav1` conformance green, Khronos validation layer clean.

**Upstream status:** GStreamer submissions are restricted (no MR path for us); the offer target is
games-on-whales/gst-wayland-display `patches/`, bundled with its prerequisites
(`vkh264enc-rc-fix.patch`, `gstreamer-vulkan-rc-retarget-no-reset.patch`) — posting gated on
operator sign-off. Regenerate against every GStreamer re-pin (same standing cost as
`vulkanh265enc.patch`).

### `vulkan-enc-output-state-on-resize.patch`

**Mandatory for any live external-resolution change on the Vulkan encode path** (#501, the
`vulkanscale` lever). Three files: `ext/vulkan/vkh264enc.c`, `ext/vulkan/vkh265enc.c`, and one
defensive line in `gst-libs/gst/vulkan/gstvkencoder-private.c`. Applies **ninth (last)**.

Both elements early-returned from `new_sequence()` whenever the video **profile** was unchanged,
which a pure resolution change always is. That early return skipped every size-derived step, and
the two elements lost a different half of the truth:

- `vulkanh264enc` refreshed `self->in_state` only in the tail of the function, and
  `new_parameters()` (which the base class calls immediately after `new_sequence()`) builds the src
  caps width/height from `self->in_state->info`. So the **caps kept the launch size** while the
  level was re-derived correctly and the emitted SPS carried the new resolution. `gsth264parse.c`
  lets upstream caps override the SPS it parses, so downstream caps lied about the resolution.
- `vulkanh265enc` already hoisted its `in_state` refresh above the early return (for a
  colorimetry-only change), but not `self->coded_width`/`coded_height`, so its caps followed the
  new size while the emitted SPS `pic_width_in_luma_samples` and the `GstVideoInfo` handed to
  `gst_vulkan_encoder_encode()` stayed at the launch size.

In both cases the Vulkan video session and its DPB pool also stayed at the launch size
(`gst_vulkan_encoder_create_dpb_pool()` early-returns `TRUE` when a pool exists), so a step back up
to a larger size would have encoded into undersized DPB images.

The fix keys each early return on the input size as well as the profile, so a coded-size change
takes the same path a profile change takes: stop and restart the Vulkan session.
`gst_vulkan_encoder_stop()` clears the DPB pool, so the restart re-runs the whole first-start path
(the driver coded-extent limit check, the coded-extent recompute, DPB pool creation at the new
size, and, through `new_parameters()`, `gst_video_encoder_set_output_state()` with the new
width/height and the re-derived level/profile caps). `vulkanh264enc` additionally hoists its
`in_state` refresh above the early return, matching `vulkanh265enc`, so a renegotiation that keeps
the resolution (framerate, colorimetry) also updates the state the encode path reads.

The library half: `gst_vulkan_encoder_stop()` now wipes `priv->slots[]`. That table holds
**borrowed** pointers to pictures whose DPB images come from the pool the same function destroys.
The base classes drain before they reconfigure, so every picture should already have gone through
`gst_vulkan_encoder_picture_clear()` (which is what NULLs its slot); the wipe only matters if one
survived, in which case it would otherwise keep a slot index occupied across the restart and hand
the new video session a reference backed by an image from the destroyed pool (device-lost). It
owns nothing, so it frees nothing and leaks nothing. Being codec-neutral it also covers
`vulkanav1enc`, whose restart path this patch does not otherwise touch.

**Pre-existing upstream quirk, unchanged by this patch: the profile half of both early-return
tests is dead code.** `self->profile` is assigned from the freshly computed chroma subsampling,
bit depths and std profile idc in the block immediately above the test, so the comparison compares
those values with themselves and is always true. The restart is therefore keyed on the input size
alone, and a mid-stream **bit-depth or chroma-subsampling** change still would not restart the
session. That is deliberately left alone here: it is unreachable from the #501 lever, which only
ever changes width and height, and fixing it is a separate upstream concern.

**`vulkanav1enc` is deliberately not touched** (beyond the codec-neutral library line above)**.** The element added by `vulkanav1enc.patch` already
keys its early return on the input size (its "trap 6" note) and publishes its output state on both
paths, so it was already correct, confirmed live rather than assumed.

Evidence (quasar-devbox, RTX 5090, NVIDIA 610.57.04, rebuilt `libgstvulkan.so` mounted over the
image's; probe `videotestsrc` NV12 1080p60 `! videoscale ! capsfilter ! vulkanupload ! <enc> !
<parse> ! fakesink`, capsfilter flipped 1920x1080 to 1280x720 and back at frames 90 and 180, caps
pad-probes on the encoder src pad and the parser src pad):

| element | frame 90 (request 1280x720) | frame 180 (request 1920x1080) |
|---|---|---|
| `vulkanh264enc` before | `level=3.2, width=1920, height=1080` | `level=4.2, width=1920, height=1080` |
| `vulkanh264enc` after | `level=3.2, width=1280, height=720` | `level=4.2, width=1920, height=1080` |
| `vulkanav1enc` (unpatched) | `width=1280, height=720` | `width=1920, height=1080` |

270 frames, no bus error, both directions.

**Restart-path VRAM soak** (same host and probe, `nvidia-smi --query-gpu=memory.used` sampled
every 10 flips, one process, 60 frames per flip). Each flip is a full session restart, so a leak in
the stop/start path would show as monotonic growth:

| flips | 0 | 10 | 20 | 30 | 40 | 50 | 60 | 70 | 80 | end (84) |
|---|---|---|---|---|---|---|---|---|---|---|
| `vulkanh264enc` MiB | 546 | 695 | 695 | 695 | 695 | 695 | 695 | 695 | 695 | 727 |
| `vulkanav1enc` MiB | 546 | 625 | 625 | 625 | 625 | 625 | 625 | 625 | 625 | 666 |

Flat from the first sample onward, 5100 frames each, no bus error. The `start` figure is taken
before `PLAYING` (nothing allocated yet); the `end` figure is taken mid-restart at the final flip
boundary, and a separate 44-flip h264 run ended at the **same** 727 MiB, so that offset is a
sampling artefact of where the last sample lands, not accumulation.

**`vulkanh265enc` is compile-proven only**: on this host
the element fails to start with `Video profile format not supported ...
vkGetPhysicalDeviceVideoCapabilitiesKHR` (pre-existing on driver 610.57.04, unrelated to this
patch), so its runtime behaviour is unverified.

**Upstream status:** unreported as of 2026-08-22. Both fixes are upstreamable as written (no
Quasar-specific behaviour); same offer bundle as `vulkanav1enc.patch`, gated on operator sign-off.

### `vkenc-bitstream-buffer-pool.patch`

**Quasar-authored, 2026-08-23, issue [#507](https://github.com/accreleus/quasar/issues/507).
Applies tenth (last).** Recycles the Vulkan bitstream output buffer instead of allocating a new
one for every frame. Codec-agnostic: it lives in the shared encoder library
(`gst-libs/gst/vulkan/gstvkencoder-private.c`) and therefore benefits `vulkanh264enc`,
`vulkanh265enc` and `vulkanav1enc` alike.

**The defect.** `gst_vulkan_encoder_picture_init()` called
`gst_vulkan_video_codec_buffer_new()` once per frame. That is a `vkCreateBuffer` +
`vkAllocateMemory` + `vkBindBufferMemory` of a host-visible bitstream buffer (at least 1 MiB, and
`gst_h26x_calculate_coded_size()` a good deal more at high resolutions), with the matching
`vkFreeMemory` in `gst_vulkan_encoder_picture_clear()`. Both run on the streaming thread, so they
serialise with the encode engine rather than overlapping it.

Measured on the devbox RTX 5090 (driver 610.57.04) at 2560x1440 with the agent's own encoder
properties, using temporary timers around each stage of the encode path:

| stage | HEVC | H.264 |
|---|---|---|
| `vkCreateBuffer`+`vkAllocateMemory` (allocate) | 0.70 ms | 0.61 ms |
| `vkFreeMemory` (release) | 0.53 ms | 0.39 ms |
| everything else outside the encode call | 0.08 ms | 0.09 ms |

So the whole non-encode per-frame cost was this one allocation. The rest of the CPU path
(command-buffer begin, command recording, queue submit, query fetch) measured 0.005 / 0.009 /
0.03 / 0.001 ms respectively, i.e. under 0.05 ms combined, and the header bit-writing measured
0.001 ms.

**The fix.** A minimal private `GstBufferPool` subclass whose `alloc_buffer` calls the same
`gst_vulkan_video_codec_buffer_new()`. Four properties of it are load-bearing, and the first two
were found by breaking them first:

- **The pooled buffer never leaves the encoder.** Once the encode completes,
  `gst_vulkan_encoder_encode()` copies the bytes the encoder actually wrote into a plain buffer and
  hands the Vulkan one straight back. Pushing the pooled buffer downstream instead looks like it
  should work (the refcount says when it is free) but does not: `GstBaseParse` keeps the buffers it
  receives, and their `GstMemory`, in an adapter well past the point where the encoder has cleared
  the picture, so a later frame overwrote memory the parser was still describing. That was a
  reproducible `SIGSEGV` in `gst_base_parse_chain()` on the first live resolution change. The copy
  is the encoded frame, kilobytes against a megabyte-scale device allocation, and it gives
  downstream plain system memory to map instead of host-visible device memory.
- **A recycled buffer is zeroed on the way back into the pool.** Freshly allocated Vulkan memory
  reads back as zeroes, and `_write_headers()` in all three codec elements relies on that: it
  advances its write cursor past each NAL by `size + 1`, so a byte between NALs is never written,
  and the filler NAL only pads out to the alignment the encoder needs. Without the zeroing, those
  untouched bytes carry the previous frame's bitstream into the next IDR's parameter-set region: an
  H.264 capture still produced 300 encoded frames but decoded only 240, stopping exactly at the
  second IDR (`idr-period=240`). H.265 and AV1 happened not to trip it, which is precisely why a
  frame-count decode gate is worth more here than eyeballing the stream.

  Only a prefix is cleared, not the multi-megabyte allocation: `gst_vulkan_encoder_encode()` stamps
  the byte count it produced on `GST_BUFFER_OFFSET_END`, and by induction every byte at or above
  that offset is still zero (the buffer starts zeroed, headers go below `pic->offset`, and the
  encoder writes up to `pic->offset + data_size`, which is exactly the stamped value). A 64 KiB
  floor covers the case where the next frame's header region outruns the previous frame's whole
  bitstream, and any padding a driver writes past the byte count its query reports: the elements
  emit at most AUD + VPS + SPS + PPS + one SEI, each bit-written into a 4096-byte scratch NAL and
  then start-code and emulation-prevention expanded (worst case 3/2), plus a filler padding to
  `minBitstreamBufferOffsetAlignment` - about 31 KiB, so the floor is double the worst case. A
  buffer released without the stamp (an encode that failed part way) is cleared in full. Clearing
  the whole allocation instead costs about 5 MiB of memset per frame at 1440p and 13 MiB at 2160p,
  which measurably ate into the win for the cheaper codecs (AV1 1440p 590 fps rather than 657).
- **A buffer that cannot be cleared is dropped, not reused.** If the map for zeroing fails, the
  memory is tagged so `GstBufferPool` frees the buffer and the next acquire allocates a fresh
  (zeroed) one. Silently recycling a dirty buffer would reintroduce exactly the corruption the
  clearing exists to prevent.
- **The pool is configured for the size the buffers really are.**
  `gst_vulkan_video_codec_buffer_new()` floors its allocation at 1 MiB, and
  `default_reset_buffer()` resizes every recycled buffer down to the configured pool size, so
  configuring the pool with the requested size would give frame 1 a different `dstBufferRange` from
  frames 2 onward at any rung whose coded size is below the floor. The pool mirrors the floor and
  `alloc_buffer()` asserts the two agree, tagging the buffer out of the pool if they ever drift.

The pool copies the `GstVulkanVideoProfile` by value and **re-chains its `pNext` into its own copy**,
the same chain-up `gst_vulkan_encoder_start()` does. `GstVulkanVideoProfile` is self-referential, and
this pool is explicitly allowed to outlive the encoder, so a plain assignment would leave the copy
pointing into the encoder's private profile.

The pool is keyed on the aligned bitstream size and rebuilt when that changes, which is what a live
resolution change (the `vulkanscale` lever, `vulkan-enc-output-state-on-resize.patch`) looks like
from here. `gst_vulkan_encoder_stop()` drops the encoder's reference but deliberately does **not**
deactivate the pool: any buffer still out on loan keeps it alive and is freed when it is released,
so a session restart cannot pull memory out from under a picture still in flight.

**Effect** (devbox RTX 5090, compositor decoupled by a `queue`, agent encoder properties):

| codec | resolution | before | after |
|---|---|---|---|
| HEVC | 1280x720 | 371 fps (342 Mpix/s) | 476 fps (438 Mpix/s) |
| HEVC | 1920x1080 | 184 fps (382 Mpix/s) | 225 fps (467 Mpix/s) |
| HEVC | 2560x1440 | 110 fps (404 Mpix/s) | 131 fps (482 Mpix/s) |
| HEVC | 3840x2160 | 49 fps (406 Mpix/s) | 58 fps (484 Mpix/s) |
| H.264 | 1280x720 | 924 fps (851 Mpix/s) | 996 fps (918 Mpix/s) * |
| H.264 | 1920x1080 | 562 fps (1165 Mpix/s) | 984 fps (2040 Mpix/s) * |
| H.264 | 2560x1440 | 373 fps (1375 Mpix/s) | 620 fps (2284 Mpix/s) |
| H.264 | 3840x2160 | 185 fps (1538 Mpix/s) | 240 fps (1991 Mpix/s) * |
| AV1 | 1280x720 | 881 fps (812 Mpix/s) | 996 fps (918 Mpix/s) * |
| AV1 | 1920x1080 | 521 fps (1081 Mpix/s) | 995 fps (2064 Mpix/s) * |
| AV1 | 2560x1440 | 325 fps (1199 Mpix/s) | 657 fps (2423 Mpix/s) |
| AV1 | 3840x2160 | 142 fps (1179 Mpix/s) | 240 fps (1991 Mpix/s) * |

`*` these cells sit at the harness's requested framerate (1000 fps, or 240 at 2160p where a higher
request exceeds the codec level), so they are lower bounds on the encoder: the source ran out of
frames before the encoder ran out of headroom. Every HEVC cell is a real encoder ceiling.

1440p120 HEVC needs 442 Mpix/s, so this is what takes it from out of reach to in reach.

**Correctness evidence.** A fixed 300-frame capture at 2560x1440 with the agent's encoder
properties is **byte-identical** (md5) to the same capture from the unpatched library, for H.264,
H.265 and AV1 alike; the change produces the same bitstream, it just stops allocating to do it.
Both H.264 and H.265 captures strict-decode to 300 of 300 frames through `vulkanh26xdec`. The
gst-wayland-display fork's GPU-gated live-resize suite (`cargo test --test vulkanscale --
--ignored`, which flips 1280x720 to 1920x1080 mid-stream and asserts the new size reaches the
bitstream, including the multi-session case) passes 5 of 5, matching the unpatched baseline.

**Oversize frames.** The buffer size is still chosen by the codec element
(`gst_h26x_calculate_coded_size()`) and this patch does not change it, so a frame that needs more
than the buffer is as (im)possible as it was before. What did change is the failure mode, and it
changed for the better. Forcing the case with a fault-injected build (an 8 KiB request against
frames of about 15 KiB) at 2560x1440: the **unpatched** library reported 60 encoded frames and wrote
61230 bytes with no error at all, because `gst_buffer_set_size()` cannot grow a buffer past its
allocation and the frame was **silently truncated**. The **patched** library refuses: the
`map.size < out_size` guard fires on the first oversize frame, logs `Bitstream buffer is N bytes,
the encoder reported M`, and the pipeline errors out. It never hands on a short frame. In a normal
build the pool requests the same size and mirrors the same 1 MiB floor as the unpatched helper, so
buffer sizing is byte-for-byte what it was.

**What this does not fix.** The remaining HEVC cost is genuine engine time: the fence wait in
`gst_vulkan_encoder_encode()` is 7.4 ms per 1440p frame, it scales linearly with pixel count
(about 2.05 ms/Mpix, intercept about 0.2 ms), and running two, three or four concurrent encode
sessions saturates at roughly 133 fps aggregate at 1440p. Every coding-tool knob we can reach
through the Vulkan API is inert on it: SPS `max_transform_hierarchy_depth_inter/intra`,
`amp_enabled_flag`, `sample_adaptive_offset_enabled_flag`, `sps_temporal_mvp_enabled_flag`,
`strong_intra_smoothing_enabled_flag`, PPS `transform_skip_enabled_flag`,
`sign_data_hiding_enabled_flag`, `cu_qp_delta_enabled_flag`, and the rate-control mode were each
A/B'd and each changed throughput by under 1 percent. `VkVideoEncodeQualityLevelInfoKHR` is
honoured but only in the slow direction: level 0 (what the agent uses) is already the fastest, and
levels 4 and 6 are measurably slower. For comparison, on the same box `nvh265enc` reaches 102 fps
at `preset=p7`, 315 fps at `preset=p4`, and 367 fps at `preset=low-latency-hp`, i.e. NVIDIA's
Vulkan Video HEVC encoder behaves like NVENC's slowest preset at every quality level the API
exposes. That gap is inside the driver and there is no Vulkan knob for it.

### `vkh265enc-profile-template.patch`

**Candidate. Not applied — not referenced by `deploy/Dockerfile.vulkan`.** Kept here as the
authored record and as the basis for an upstream GStreamer submission. It is therefore also
deliberately absent from the `patches` inventory in `deploy/release-manifest.json`, which lists
build inputs only. Origin: commit 5f0d4776 (2026-08-22, #501) — the investigation that showed the
h265 failure was a probe caps-negotiation artifact rather than a driver bug. Do not delete it as
an orphan and do not wire it into the Dockerfile chain without a live exercise; it is neither an
abandoned experiment nor a pending build change.

Makes `vulkanh265enc` stop advertising H.265 profiles it cannot itself translate.

`gst_vulkan_h265_encoder_register()` builds the element's src pad template straight from
`gst_vulkan_physical_device_codec_caps()`, i.e. from whatever the driver reports. On an
RTX 5090 that yields `profile: { main, main-10, main-444 }`. But the element's own
`H265ProfileMap[]` maps only `main`, `main-10` and `main-still-picture` into a
`StdVideoH265ProfileIdc`. `main-444` is
`STD_VIDEO_H265_PROFILE_IDC_FORMAT_RANGE_EXTENSIONS` in `gstvkvideoutils-private.c` and has
no entry in that table.

So when downstream leaves `profile` unconstrained, negotiation is free to settle on
`main-444`, `gst_vulkan_h265_profile_type()` logs `Unsupported profile type '10'` and
returns `STD_VIDEO_H265_PROFILE_IDC_INVALID`, and `gst_vulkan_encoder_start()` then fails:

```
vkh265enc.c:335 WARN  Unsupported profile type '10'
vkh265enc.c:952 error: Unable to start vulkan encoder with error
                Video profile format not supported (0xc464dc25, -1000023003):
                vkGetPhysicalDeviceVideoCapabilitiesKHR
```

The driver is right to refuse; the element asked for a profile it never supported. The
patch intersects the driver-derived caps with the profiles `H265ProfileMap[]` can map,
before they become the pad template. It fails open: if the intersection is empty the
original caps are kept, so an exotic driver can never register an element with empty src
caps.

**Why Quasar does not need it.** The node-agent's encode pipeline already pins
`video/x-h265,profile=main` on the encoder output
(`node-agent/src/session/pipeline/caps.rs`, documented in `codec_chain.rs`), which
constrains the negotiation before it can go wrong. Live HEVC sessions on a Vulkan host have
never hit this. What *did* hit it is every hand-written probe pipeline — the #501 bisect and
the fork's `vulkanscale.rs` GPU test both omitted the capsfilter and both read the resulting
failure as a driver defect. It is not one: the same box encodes HEVC on Vulkan perfectly
with the profile pinned, on NVIDIA 610.57.04.

**Wiring it up** (if a future need appears): add the `patch -p1` line to the GStreamer
build step in `deploy/Dockerfile.vulkan` after
`vulkan-enc-output-state-on-resize.patch`, then rebuild via `deploy/build-images.sh`. It has
not been compiled — a full GStreamer build was out of scope for the session that authored it
(2026-08-22). Treat the first build as the compile check.

### `gst-wayland-display-vulkan-pts.patch`

**Mandatory for the `vulkan=true` (`memory:VulkanImage` → `vulkanh264enc`) encode path.** Unlike
the five patches above, this one patches **gst-wayland-display** (the compositor), not GStreamer —
it is applied at the gst-wayland-display build step in `deploy/Dockerfile.vulkan`, after the
`43d4c25` checkout and before `cargo cinstall`. It touches one file:
`wayland-display-core/src/utils/vulkan_nv12.rs`.

Root cause (VULKAN-WORKLOG `2026-07-06`): the Vulkan NV12 export ring holds one persistent
`GstBuffer` per slot (default `RING = 4`, `WOLF_VULKAN_RING`), and `VulkanNv12::to_gst_buffer()`
returned a bare `.clone()` of the current slot's buffer every frame. `waylanddisplaysrc` runs
`set_do_timestamp(true)`, which only stamps a buffer whose PTS is `GST_CLOCK_TIME_NONE`; a recycled
slot buffer carries the PTS it was stamped with on its *previous* use, so `do_timestamp` skips it.
The exported PTS therefore degenerate to the ring's ~4 recurring values — **non-monotonic and
duplicated across frames** (live trace: dup-TS-across 99.9%, non-monotonic 25%, exactly 4 distinct
RTP timestamps cycling forever). A downstream `rtph264pay` turns those into RTP timestamps that
libwebrtc's `PacketBuffer` cannot assemble, so the browser drops ~60% of frames at decode
(`framesAssembledFromMultiplePackets` 39.7% vs 99.9% on VA) even on a clean LAN with zero packet
loss. The RGBx and DMABuf output paths never hit this because each builds a **fresh** `GstBuffer`
(PTS `NONE`) every frame.

The fix clears PTS/DTS/duration on the recycled buffer before returning it, so `do_timestamp`
re-stamps it with the current running-time each frame — making the vulkan path timestamp exactly
like the RGBx/DMABuf paths. It is **scoped to the `encode_src` case** (`vulkanh264enc`), so the VA
compositor-NV12 dmabuf path (`encode_src == false`, GW-02, `QUASAR_COMPOSITOR_NV12`) is left
byte-identical. The buffer's memory and video meta are untouched — only the timing metadata is
reset.

**Upstream status:** to be reported on
[gst-wayland-display PR #37](https://github.com/games-on-whales/gst-wayland-display/pull/37) (the
`feat/vulkan-encode` branch this image builds from) — the defect is in that PR's vulkan output
path, not in GStreamer.

> **Related, but NOT a patch — the encode-src ring-slot *tiling* defect.** Distinct from the PTS
> issue above (which is about buffer *timestamps*), the same `RING` (`WOLF_VULKAN_RING`, default 4)
> has a *tiling* defect on the **shared-device encode-src path** (`vulkan_share.rs`
> `alloc_encode_src_buffer` / `vulkan_nv12.rs` `create_encode_output`): on NVIDIA the H.265
> (Main-10) encode-src pool gives one of the four per-slot images an incompatible tiling/swizzle, so
> the RGBA→NV12 compute+copy writes it **black** — exactly 1-in-4 encoded frames black (h264's pool
> tiles uniformly). The clean uniform-tiling fix is **infeasible on NVIDIA** (its Vulkan-Video
> encoder rejects a LINEAR encode-src image — verified live on the RTX 5090, 2026-07-24, with
> `WOLF_VULKAN_LINEAR_ENCSRC=1`), and a compositor-side validation probe can't reliably test the
> encoder's tiled read. So this one is fixed **in the node-agent, not here**: it auto-pins the ring
> to a double-buffered depth (`WOLF_VULKAN_RING=2`) whenever HEVC is enabled on the Vulkan
> encoder, which is the default since 2026-08-22 (`QUASAR_VULKAN_HEVC=0` removes the pin)
> (`node-agent/src/session/pipeline/source_branch.rs::pin_vulkan_encode_ring`; resolution
> spec §7 item 4). `RING=2`, not the single stable slot `RING=1`, is pinned — the single slot starves
> under the G1 `ParentBufferMeta` buffer-reuse gate below (multi-session spec §2c; rung-2 validated
> on Tower, 2026-07-25). Uniform encode-src tiling in the compositor would restore full RING
> parallelism and is worth including in the PR #37 upstream report.

### `gst-wayland-display-app-cadence.patch`

Exports `waylanddisplaysrc.app-surface-commits` as a read-only `uint64` lifetime counter. The
compositor increments it only when a mapped xdg-toplevel commits a new buffer; cursor, popup,
subsurface, and configure-only commits are excluded. Quasar delta-samples the counter beside the
separate compositor output-pad cadence, without adding pipeline elements or emitting a bus message
per frame.

The single-app compositor forces mapped xdg-toplevels fullscreen, so the mapped top-level is the
authoritative application surface for current Quasar workloads. An application that renders only
through updating subsurfaces will under-report source cadence because subsurface commits are
deliberately excluded; such a workload needs an explicit aggregation policy before being certified.
The patch applies to the pinned `43d4c25` checkout after the Vulkan PTS patch and before
`cargo cinstall`.

### `gst-wayland-display-vulkan-nvidia-sync.patch`

**Tower stabilization candidate, pending live A/B validation.** The VulkanImage producer and
`vulkanh264enc` share GStreamer Vulkan objects and queues. Two plausible causes of Tower's Xid
13/32 and `VK_ERROR_DEVICE_LOST` are an externally-unsynchronized producer `vkQueueSubmit` and a
replacement compositor creating a different logical Vulkan device during launcher-to-app swaps.
The node agent now enforces device continuity; this patch makes the producer participate in
GStreamer's `GstVulkanQueue` submission lock and validates that the selected queue supports both
graphics and compute.

GStreamer 1.28.4's Vulkan image allocator already supplies the device queue-family list and selects
`CONCURRENT` sharing when multiple families are present, so this patch deliberately does not
override sharing mode or retain its own queue-family array.

PR #37's intentional encode-layout/fan-out behavior is preserved. Its prior Rust byte-offset writes
into `GstVulkanImageMemory` are replaced by a small C bridge compiled against the exact GStreamer
1.28 headers. The bridge destroys the allocator-created timeline semaphore before clearing it,
avoiding a per-image handle leak. Apply this patch after the PTS and app-cadence patches.

### `gst-wayland-display-gles-vulkan-sync.patch`

**Mandatory for every in-compositor Vulkan conversion path.** PR #37 renders the scene into a GLES
RGBA dmabuf and then imports that same allocation into Vulkan for RGBA-to-NV12 conversion. Upstream
waited `render_output_result.sync` only after `State::create_frame()` returned, but
`create_frame()` had already called `to_gs_buffer()` and submitted the Vulkan read. The apparent
wait therefore protected only the later GStreamer hand-off, not the GLES-to-Vulkan ownership seam.

The patch moves the existing render-fence wait immediately after `render_output()` and before any
readback or `to_gs_buffer()` conversion. This gives the external-memory consumer a completed GLES
producer without relying on undocumented driver serialization. It removes the now-redundant late
wait from the caller. This is independent of gst-interpipe and the encoder: an idle compositor's
first conversion can otherwise race its own GLES render. Tower exposed that race on an RTX 5090
with NVIDIA 595.80 as Xid 13/32 followed by `VK_ERROR_DEVICE_LOST`; upstream's cited RTX 5080 tests
used driver 610.43.02 and only about 170 frames, so they do not cover this environment or sustained
operation.

Apply this patch last, after the PTS, app-cadence, and NVIDIA queue/device synchronization patches.

### `gst-wayland-display-initial-pointer-focus.patch`

**Mandatory for any interactive session.** Upstream assigns keyboard focus directly at
toplevel map, but pointer focus only ever comes from `Space::element_under(pointer_location)`
during motion — and that lookup filters on `Window::bbox()`, which stays `(0,0)` until
`Window::on_commit()` runs. The per-commit `on_commit()` call in the commit handler only fires
for surfaces *already* in the space, so a client that never re-commits its root toplevel after
the mapping commit (Steam Big Picture rendering via subsurfaces, an idle `wev`) keeps an empty
bbox forever: `element_under()` returns `None` at every cursor position and the surface never
receives `wl_pointer.enter` — mouse clicks and cursor imagery are dead while keyboard works.
Upstream's own `tests/fixture.rs` documents the same failure ("spent hours … window size kept
at (0,0)") and works around it by calling `on_commit()` manually; this patch applies that fix
at the production map site. One added call: `window.on_commit()` after `space.map_element()`.
Applies after the other gst-wayland-display patches (context-adjacent to app-cadence).
Found 2026-07-19 (Tower Steam kb/mouse regression investigation).

### `gst-wayland-display-fail-closed-renderer.patch`

**Mandatory for multi-session-on-one-GPU correctness and renderer observability** (#378 W1).
Patches gst-wayland-display (the compositor + its gst element), not GStreamer. Applied
after `initial-pointer-focus` (only `linear-encsrc-fallback` applies after it). Four related changes, all traced to #378's
investigation (see `docs/design/plans/2026-07-19-378-multisession-fail-closed-spec.md`):

1. **EGL bind-failure log reworded + demoted to `debug!`**
   (`wayland-display-core/src/comp/mod.rs`). Upstream logs a failed
   `renderer.bind_wl_display()` as `info!("Failed to initialize EGL hardware-acceleration")`
   — dangerously misleading. The bind's ONLY product is the `EGLBufferReader` (legacy
   `wl_drm` / EGL-image import), which is **dead code here**: both client buffer routes go
   through `import_dmabuf` (`handlers/dmabuf.rs`, `handlers/wl_drm.rs` — mesa's `wl_drm` is
   implemented over dmabuf), and smithay's `buffer_type()` dispatch checks dmabuf before EGL.
   A failed bind therefore loses **no client capability**. On NVIDIA the per-device
   `EGLDisplay` is process-shared and permits exactly one `wl_display`, so the **2nd
   concurrent compositor's bind ALWAYS fails** — reading that as loss of hardware
   acceleration cost a full diagnostic detour. Now `debug!` with wording that names the real
   (dead-code) effect. The success arm keeps its `info!("EGL hardware-acceleration enabled")`.

2. **Explicit `unbind_wl_display()` at compositor teardown**
   (`wayland-display-core/src/comp/mod.rs`, end of `comp::init`). smithay unbinds the
   `wl_display` only when the last `EGLBufferReader` Arc drops, which drains asynchronously
   with renderer teardown — so the per-device `EGLDisplay` can stay transiently bound and
   **race the next session's bind** (`OtherEGLDisplayAlreadyBound` on a back-to-back launch).
   The patch calls `renderer.unbind_wl_display()` (smithay `ImportEgl` trait) before the
   `GlesRenderer` drops, running `eglUnbindWaylandDisplayWL` promptly. Gated on a new
   `State.egl_bound` flag (set from change 1's match) so it only unbinds what it bound.

3. **Fail-closed software-fallback: already present at `43d4c25` — DOCUMENTED, no code
   change.** The spec asked for a gst **Error** when a requested GPU `render-node` silently
   falls back to `RenderTarget::Software`. Source review of `utils/target.rs`,
   `utils/renderer/mod.rs`, and `gst-plugin-wayland-display/src/waylandsrc/imp.rs` shows this
   fallback **cannot happen silently at this pin**: `RenderTarget::from_str` maps a GPU path
   to `Hardware(DrmNode::from_path(path)?)` and returns `Err(CreateDrmNodeError)` if the node
   fails to open — which propagates up and makes the element post `gst::error_msg!(
   LibraryError::Failed, …)` from `start()`. `setup_renderer()` `.expect()`-panics (→ the
   catch_unwind'd compositor thread dies → gst EOS/error) if it can't build the GLES renderer
   on a hardware node. `RenderTarget::Software` is reachable **only** via explicit
   `render-node=software`. So a requested GPU that can't be used already fails closed; there
   is no silent-software path to convert. (If `GST_WAYLAND_DISPLAY_REF` moves, re-verify this
   assumption before assuming the fail-closed behavior still holds.)

4. **dmabuf-import failure marker → gst bus WARNING, modeled as a persistent CONDITION**
   (`handlers/dmabuf.rs`, `handlers/wl_drm.rs`, `handlers/compositor.rs`, `comp/mod.rs`,
   `lib.rs`, and the gst element `waylandsrc/imp.rs`). Because the `GstLayer` tracing bridge
   forwards to the GStreamer **debug log**, not the gst **bus** — and the compositor runs on
   its own thread with no bus-posting subscriber — the marker is carried to the element over
   a shared `Arc<AtomicU64>` degradation counter (mirroring the existing
   `app_surface_commits` counter). The element delta-samples the counter in `create()` and,
   on each new event, posts a real bus **WARNING** (`gst::element_warning!`) whose message
   contains **`wolf-renderer-degraded`** (renamed from `quasar-renderer-degraded` for upstreaming; the agent matches the unprefixed `renderer-degraded` stem, so both are recognised). The node-agent's W2 fail-closed hook matches that
   bus WARNING to fail a session that requires hardware rendering.

   **Condition, not a one-shot event.** A first design that bumped the counter once per
   failed import (rate-limited to ≤1/5s) turned out to be insufficient: live T6 testing on
   Tower found that gamescope submits exactly **one** dmabuf, its rejected import makes it
   back off permanently (no retry, ever), so exactly one marker was ever emitted — and the
   node-agent's 2-in-30s debounce then never fired, leaving a pure-black session `running`
   forever. `comp::State` now tracks `renderer_degraded_active: Option<Instant>` as a
   condition, not an event:
   - **Set/refresh** (`State::note_renderer_degraded`, called from both import handlers'
     Err arms and the T6 injection hook below): the first failure since the condition was
     last clear emits immediately; a repeat failure while already active does not re-emit
     (the periodic tick below covers "still broken" without flooding on a fast-retrying
     client).
   - **Clear** (`State::clear_renderer_degraded`): fired the moment any client buffer is
     subsequently handled successfully — a successful `import_dmabuf` in either handler, or
     a newly-attached **non**-dmabuf buffer (SHM, single-pixel) accepted in
     `handlers/compositor.rs::commit`. A client that recovers (e.g. an SHM fallback after a
     rejected dmabuf) is proven alive, so the earlier marker was transient and the node-agent
     debounce correctly ignores it.
   - **Periodic re-emit** (`State::tick_renderer_degraded`, called every frame from the
     per-frame `render` closure in `comp::init` — the encode pipeline pulls frames on its own
     schedule independent of client behavior, making it the one reliable periodic tick
     available): while the condition remains active, bump the counter (→ another bus
     WARNING) every 5s. A client that backs off for good after one rejected import never
     produces another failure to re-trigger `note_renderer_degraded`, so this is what keeps
     the marker alive until the node-agent acts on it.

   **T6 fault-injection hook (review addition, 2c5fc31):** since a bogus `render-node` fails
   unconditionally (independent of the fail-closed knob) and `render-node=software` never
   advertises a dmabuf global, neither exercises the `QUASAR_REQUIRE_HW_RENDER` knob end to
   end — so `comp::debug_fail_dmabuf_import()` reads `WOLF_DEBUG_FAIL_DMABUF_IMPORT` (renamed from `QUASAR_DEBUG_FAIL_DMABUF_IMPORT`)
   (`"1"`/`"true"`, `OnceLock`-cached) and, when set, makes both dmabuf-import handlers skip
   the real `import_dmabuf` call and take the failure branch unconditionally, every time —
   so the condition never clears and the periodic re-emit fires, deterministically driving
   T6's pass condition (session fails after the 2nd marker, ~5-10s). Test-only; **never set
   in production**.

| Field | Value |
|---|---|
| Origin | Quasar (this repo) — not vendored |
| Patches | `games-on-whales/gst-wayland-display` (compositor + gst element): `wayland-display-core/src/{comp/mod.rs,lib.rs,wayland/handlers/dmabuf.rs,wayland/handlers/wl_drm.rs,wayland/handlers/compositor.rs}`, `gst-plugin-wayland-display/src/waylandsrc/imp.rs` |
| Authored against | `43d4c25` (the `GST_WAYLAND_DISPLAY_REF` this image builds), on top of the other five `gst-wayland-display-*` patches |
| Upstream status | Quasar-specific (#378); the bind-log demote + explicit unbind are plausibly upstreamable |

If `GST_WAYLAND_DISPLAY_REF` moves, re-diff this patch against the new commit (verify the
`bind_wl_display` match, the `comp::init` teardown tail, the two import handlers, and the
element `create()` all still apply) and update `docs/third-party-pins.md`. The smithay
`ImportEgl::unbind_wl_display` API and the `gst::element_warning!` macro must still exist in
the pinned `smithay` / `gstreamer` versions.

### `gst-wayland-display-linear-encsrc-fallback.patch`

**Latent-bug fix for the `WOLF_VULKAN_LINEAR_ENCSRC=1` opt-in knob** (the RX 9070 / GFX12
swizzle-copy workaround). Patches gst-wayland-display
(`wayland-display-core/src/utils/vulkan_share.rs::alloc_encode_src_buffer`), not GStreamer.
Applied **last**, after `fail-closed-renderer`.

Upstream's comment promises the knob is safe: "falls back to tiled if the linear alloc
fails". The code did not deliver that — when
`gst_vulkan_image_memory_alloc_with_image_info` returns null for the LINEAR image, the
function just returned `None`, which fails `VulkanNv12::new_on_shared` → "encode-src image
allocation failed" → the session's whole Vulkan output fails to create. Verified live on
the RTX 5090 (2026-07-24): NVIDIA's Vulkan-Video encoder rejects a LINEAR encode-src image,
so setting the knob on an NVIDIA vulkan h265 host hard-failed every session instead of
degrading gracefully.

The patch implements the promised fallback: if the LINEAR alloc returns null while
`WOLF_VULKAN_LINEAR_ENCSRC` is set, log a `tracing::warn!` and retry the identical
allocation with `vk::ImageTiling::OPTIMAL` (the tiled default). The non-knob path is
byte-identical (the retry branch is gated on `linear_encsrc`), and a genuine allocation
failure still returns `None` after the retry. This makes the knob safe to leave on in
mixed-GPU fleets: hosts whose encoder accepts LINEAR use it, hosts that reject it get the
tiled default plus a warning, none hard-fail. Found while integrating the ring-slot tiling
fix (`docs/design/plans/2026-07-24-vulkanh265enc-conformance-resolution-spec.md` §7 item 4).

| Field | Value |
|---|---|
| Origin | Quasar (this repo) — not vendored |
| Patches | `games-on-whales/gst-wayland-display` (compositor), file `wayland-display-core/src/utils/vulkan_share.rs` |
| Authored against | `43d4c25` (the `GST_WAYLAND_DISPLAY_REF` this image builds), on top of the other six `gst-wayland-display-*` patches (the `nvidia-sync` patch touches the same function, so this applies **after** the full stack) |
| Upstream status | to be reported on [gst-wayland-display PR #37](https://github.com/games-on-whales/gst-wayland-display/pull/37) (comment/behavior mismatch is upstream's) |

If `GST_WAYLAND_DISPLAY_REF` moves, first check whether upstream implemented its promised
fallback (then drop this patch); otherwise re-diff against the new commit with the other
six gst-wayland-display patches applied first, and update `docs/third-party-pins.md`.
(Note: as of the 8th patch below, `linear-encsrc-fallback` is no longer the *last*
gst-wayland-display patch — `per-element-vulkan-device` applies after it.)

### `gst-wayland-display-per-element-vulkan-device.patch`

**The 8th gst-wayland-display patch — per-element Vulkan device ownership** (multi-session
Vulkan spec §2a, `docs/design/plans/2026-07-25-vulkan-multisession-spec.md`). Patches
gst-wayland-display (the compositor core + its gst element), not GStreamer. Applied **last of
all**, after `linear-encsrc-fallback` (it touches `vulkan_share.rs`, which `nvidia-sync` **and**
`linear-encsrc-fallback` also touch, so it must land on top of the full stack).

**The blocker it removes.** At `43d4c25`, `wayland-display-core/src/utils/vulkan_share.rs` held
the owned `GstVulkanInstance` + `GstVulkanDevice` in **two process-global `OnceLock` slots**
(`device_slot()` / `instance_slot()`). The first `waylanddisplaysrc` to answer a
`gst.vulkan.device` context query minted the device on *its* `target_minor`, and **every later
session in the process silently reused that exact device** regardless of its render node. For N
concurrent Vulkan-encode sessions on one host that means: one shared `VkDevice` (a single
failure domain — one session's `DEVICE_LOST` corrupts all N), session 2..N's `target_minor`
ignored (wrong device on a future multi-GPU host), and a deliberate per-process device leak
(one leaked device per session at N).

**What it changes.** The owned instance + device move out of the two statics and into a new
per-element `VulkanShare { instance: Mutex<Option<VulkanInstance>>, device:
Mutex<Option<VulkanDevice>> }`, held behind an `Arc`. Each `waylanddisplaysrc` element owns one
`Arc<VulkanShare>` (a field on the gst-element `imp` struct, created in `Default`, living for
the element's whole lifetime) and hands its **own** compositor thread a clone — threaded through
`WaylandDisplay::new_with_channel` → `comp::init` → `comp::State.vulkan_share`, exactly like the
existing `app_surface_commits` / `renderer_degraded` `Arc<AtomicU64>`s. So each element mints,
answers `gst.vulkan.{instance,device}` context queries with, and (on teardown) destroys its
**own** `VkDevice`. N concurrent sessions therefore get **N isolated devices** — a `DEVICE_LOST`
on one retires only that element's device, leaving the other N-1 untouched. The five functions
that read/wrote the global slots (`handle_set_context`, `shared_device`, `ensure_owned_device`,
`provide_context`, `wait_for_shared_device`) become methods on `VulkanShare`; the
device-parameterized helpers (`raw_handles`, `encode_src_pool`, `alloc_encode_src_buffer`,
`recover_vk_image`) never touched the global slot and are unchanged (the caller now supplies the
element's own device). `wait_for_shared_device`'s poll loop is **retained**, scoped to the
element (the encoder-`set_context`-races-compositor-thread condition is per-element).

**Teardown.** The old global slot leaked by design. The element's `stop()` now calls
`VulkanShare::clear()` alongside `drop(state.display)`, destroying that session's instance +
device — so N sessions do not leak N devices, and a retired session takes its `VkDevice` with it
(the gwd half of the per-session failure domain, spec §2b). The `Drop` path calls **no**
`process::exit` (spec §2b's `exit(75)` removal lives on the node-agent side; nothing here aborts
the process).

**RADV byte-identical (by construction).** This is a **storage-location** change only —
`VulkanShare::ensure_owned_device` runs the *identical* device-creation code
(`VulkanInstance::new/open`, `physical_index_for_minor`, the four external-memory extensions,
`VulkanDevice::open`) the process-global path ran, with **no vendor branch**. Only *where the
resulting handle is stored* moved (a `static` slot → `self`). RADV (the untestable-locally path,
spec §5) sees no behavioral change.

**`repr(C)` overlays + C bridge untouched.** The patch does **not** touch the module-header
`repr(C)` overlays or the `wayland_display_vk_*` C bridge (`vulkan_bridge.c`) — no ABI
assumption changes, only the Rust-side owner of the `VulkanDevice`/`VulkanInstance` handles
(spec §7 risk 1 mitigated by construction).

**Files touched:** `wayland-display-core/src/utils/vulkan_share.rs` (the `VulkanShare` struct +
methods + a GPU-free unit test), `wayland-display-core/src/lib.rs`
(`WaylandDisplay::new_with_channel` gains an `Arc<VulkanShare>` param; `new` mints its own),
`wayland-display-core/src/comp/mod.rs` (`init` + `State` carry the share; the
`GstVideoInfo::VULKAN` output-ring build reads the element's share), and
`gst-plugin-wayland-display/src/waylandsrc/imp.rs` (the `imp` struct field + `Default` + the
three call sites + `start()` clone hand-off + `stop()` `clear()`).

**Unit test (spec §4 rung 1, GPU-free half):** `vulkan_share::tests::
shares_are_independent_per_element` asserts two `VulkanShare`s are distinct allocations, each
starts with no device, and clearing one never touches the other. The full "two elements mint
**distinct** `GstVulkanDevice` pointers" assertion needs a real GPU and is a Tower soak item
(spec §4 rung 1 / §7 cross-session distinctness log) — `ensure_owned_device` opens a real
`VkDevice`, so it cannot run headless.

| Field | Value |
|---|---|
| Origin | Quasar (this repo) — not vendored |
| Patches | `games-on-whales/gst-wayland-display` (compositor core + gst element): `wayland-display-core/src/{utils/vulkan_share.rs,lib.rs,comp/mod.rs}`, `gst-plugin-wayland-display/src/waylandsrc/imp.rs` |
| Authored against | `43d4c25`, on top of the other seven `gst-wayland-display-*` patches (applies **8th/last**; `nvidia-sync` + `linear-encsrc-fallback` also touch `vulkan_share.rs`) |
| Verified | `cargo check --workspace` clean + unit test green in `quasar-dev:latest` on hermes (2026-07-25); Tower N-session soak is the spec §4 rung 2-4 gate |
| Upstream status | offer as **contribution #6** on [gst-wayland-display PR #37](https://github.com/games-on-whales/gst-wayland-display/pull/37) (spec §6) — the process-global slot blocks any multi-tenant Vulkan use of the element; vendor-neutral |

If `GST_WAYLAND_DISPLAY_REF` moves, re-diff this against the new commit **with the other seven
gst-wayland-display patches applied first** (confirm the `VulkanShare` struct site in
`vulkan_share.rs`, the `new_with_channel` / `comp::init` / `comp::State` threading, and the four
`imp.rs` call sites all still apply), and update `docs/third-party-pins.md`. Re-verify the
module-header `repr(C)` overlay offsets are still untouched.

### `gst-wayland-display-vulkan-parent-buffer-gate.patch`

**The 9th gst-wayland-display patch — G1 buffer-reuse gate (ParentBufferMeta)** (multi-session
Vulkan spec §2c, `docs/design/plans/2026-07-25-vulkan-multisession-spec.md`; re-authored from
`origin/fix/vulkan-ring-gate`'s G1 pieces onto develop's codec-era tree). Patches
gst-wayland-display (compositor core), one file: `wayland-display-core/src/utils/vulkan_nv12.rs`.
Applied **9th/last**, after `per-element-vulkan-device` — it changes `to_gst_buffer`, which the
`vulkan-pts` patch (applied 1st) also touches, so it re-diffs on top of the full stack.

**The bug it closes.** On the Vulkan encode path the converter keeps a small ring of cached
`GstBuffer`s, one per encode-src slot image. Pre-gate, `to_gst_buffer()` returned a bare
`.clone()` of the slot's cached buffer, and `convert()` gated slot reuse on that cached buffer
being writable (refcount 1). But `waylanddisplaysrc` is a `BaseSrc` that `make_writable`s the
buffer to stamp `do_timestamp`/DISCONT — a **shallow header copy** that releases the cached
header's ref while the underlying `VkImage` **memory** is still held by the encoder. The cached
header then reads as writable even though the memory is in use, so the slot gets recycled under
the encoder — a GPU data hazard in the green-bars / device-loss family. (Michael's intermittent
green bars on h264+h265 Vulkan sessions, which do **not** correlate with any GPU Xid/reset, are a
candidate artifact this gate addresses — spec §2c green-bars hook; validated separately by a
Tower strict-decode before/after capture, not gated on here.)

**What it changes** (`encode_src` path only; VA / RGBx / DMABuf paths byte-identical):

1. **Child-header hand-out.** `to_gst_buffer()` returns a fresh `parent.copy()` **child**
   header (sharing the slot's memory, no deep copy) carrying `GstParentBufferMeta` that refs the
   cached slot buffer. The meta's COPY transform propagates that parent ref across every
   subsequent `make_writable` / shallow copy, so the cached slot buffer stays non-writable until
   the encoder releases the **last** child — making cached-header writability a complete reuse
   sentinel. PTS/DTS/duration are cleared on the fresh child only (never mutating the cached
   buffer), preserving the `vulkan-pts` patch's monotonic-timestamp fix without a `make_writable`
   on the shared buffer.
2. **Drop-and-re-emit instead of "reuse anyway."** When the target slot is still referenced
   after the ~1s wait, `convert()` no longer reuses it — it drops the freshly-rendered frame and
   returns `Ok(())` without advancing `cur`, so `to_gst_buffer()` re-emits the last completed
   frame (rate-limited warn). This is what makes any pinned ring depth (the node-agent auto-pins
   **`WOLF_VULKAN_RING=2`**, double-buffered, when HEVC is armed — `RING=1`'s single slot starves
   under this gate instead, spec §2c/rung-2) safe under an encoder that holds its input buffer
   until its `GstVulkanOperation` completes. If nothing has completed yet, it errors (no safe
   frame to re-emit). Two new `VulkanNv12` fields (`have_output`, `busy_drops`) back this.

**Deliberately NOT ported from the ring-gate branch:** the `require_safe_vulkan_teardown` /
`process::exit(75)`-in-`Drop` teardown safety (that is spec §2b's per-session fail-closed work,
which drops `exit(75)` entirely — a `Drop` must never abort the process at N sessions), the
process-global device-epoch, and the `ConvertOutcome` enum + caller changes (the internal
drop-and-re-emit needs no caller change). This patch is the buffer-reuse gate **only**.

**Node-agent half (not in this patch — in `node-agent/src/session/pipeline*`):** the source
`interpipesink` now sets `enable-last-sample=false` (a retained last-sample would pin the
ParentBufferMeta child → a slot → forever, permanently losing one of the two `RING=2` slots),
and the encoder **sink** pad carries a warn-once probe (`attach_vulkan_parent_meta_probe`) that
flags if the meta was stripped anywhere on `interpipesink → interpipesrc → queue → encoder`
(a silent gate regression otherwise).

**Smoothness (mandatory Tower A/B, spec §4 rung 2):** the per-frame `parent.copy()` child + the
drop-and-re-emit path may cost frame pacing; the gate is accepted only if `present_interval_sd_ms`
and `present_fps` are **not worse** than develop's baseline on identical sessions (#108 present-σ
rule). Runs on Tower.

| Field | Value |
|---|---|
| Origin | Quasar (this repo) — not vendored; re-authored from `origin/fix/vulkan-ring-gate` G1 |
| Patches | `games-on-whales/gst-wayland-display` (compositor core), file `wayland-display-core/src/utils/vulkan_nv12.rs` |
| Authored against | `43d4c25`, on top of the other eight `gst-wayland-display-*` patches (applies **9th/last**; the `vulkan-pts` patch also touches `to_gst_buffer`) |
| Verified | `cargo check -p wayland-display-core` clean + GPU-free unit test `parent_meta_tracks_shallow_header_copies_until_last_release` green in `quasar-dev` on hermes (2026-07-25); Tower smoothness A/B + green-bars before/after are the spec §4 rung 2/5 gates |
| Upstream status | offer alongside the vulkan-pts fix on [gst-wayland-display PR #37](https://github.com/games-on-whales/gst-wayland-display/pull/37) — the bare-clone reuse hazard is in PR #37's vulkan output path |

If `GST_WAYLAND_DISPLAY_REF` or the `vulkan-pts` patch moves, re-diff this against the new commit
**with the other eight gst-wayland-display patches applied first** (confirm the `to_gst_buffer`
body, the `convert_inner` busy-wait block, the two `VulkanNv12` constructors, and the struct
field site all still apply) and update `docs/third-party-pins.md`.

### `gst-wayland-display-cuda-pool-config-leak.patch`

**The 10th gst-wayland-display patch — two reference-counting errors in the CUDA
`decide_allocation` path; together they leaked the injected `GstCudaContext` once per
session.** Two files: `wayland-display-core/src/utils/allocator/cuda/mod.rs` and
`gst-plugin-wayland-display/src/waylandsrc/imp.rs`. Applied **10th/last** — `imp.rs` is also
touched by the `app-cadence`, `initial-pointer-focus` and `per-element-vulkan-device` patches,
so this one is diffed on top of the full nine.

**Bug 1 — leaked pool config (the context leak).** `CUDABufferPool::get_updated_size()` calls
`gst_buffer_pool_get_config()` and never frees the result. That function is **`(transfer full)`**
— a fresh `gst_structure_copy()` of the pool's config (`gstbufferpool.c`) that the caller must
`gst_structure_free()`. The sibling `configure()` makes the same call but hands its copy to
`gst_buffer_pool_set_config()`, which consumes it, so only `get_updated_size()` leaks.

`gst_structure_copy()` `g_value_copy()`s every field, and `configure()` had just installed a
**`cuda-stream`** field (`GST_TYPE_CUDA_STREAM`, whose boxed copy/free are
`gst_mini_object_ref`/`unref`). `gst_cuda_stream_new()` in turn holds
`gst_object_ref (context)` on its `GstCudaContext`, released only in `_gst_cuda_stream_free()`
(`gstcudastream.cpp`). Retaining chain:

```
leaked GstStructure (config copy)  ->  cuda-stream field  ->  GstCudaStream  ->  GstCudaContext
```

`decide_allocation()` negotiates twice per session, so **two config copies leak per session**,
pinning one `GstCudaStream` and — through it — **one `GstCudaContext` reference per session**.

**Bug 2 — over-unreffed caps (why fixing bug 1 alone breaks the pipeline).**
`decide_allocation()` adopted the allocation query's caps with
`gst::Caps::from_glib_full(outcaps.unwrap().as_ptr())`. `query.get()` only **lends** that
pointer, so `from_glib_full` consumes a reference the element never owned, leaving the
negotiated caps one short for the rest of the session. Nothing visibly breaks while bug 1 is
present, because the leaked config copy happens to hold a ref on that same caps. Fix bug 1
alone and the caps drops below its true count: sessions still connect and decode, but buffers
are never released and **every** session's pipelines are retained (Tower: VRAM 843 MiB, ~3000
`GstMemory` and 6 `GstPipeline`s alive after two sessions, plus
`free_priv_data: object finalizing but still has 1 parents`). The two fixes are therefore a
single unit; do not split them.

**Why the leak resisted the earlier hunt.** With an application-injected per-session context
(Quasar's ZC-02 zero-copy NVENC path, `node-agent/src/session/cuda_share.rs`) the leaked
reference meant the session's `GstCudaContext` was never finalized: ~500 MiB VRAM plus one
`cuda-EvtHandlr` driver thread per session, perfectly linear (Tower, measured 34 -> 542 -> 1000
MiB). Two properties hid it:

- **The retaining chain is invisible to the leaks tracer as normally configured.** The repo's
  standard recipe is `leaks(filters=GstObject)`, but a `GstStructure` is not refcounted at all
  and `GstCudaStream` is a `GstMiniObject` — so only the terminal `GstCudaContext` shows up,
  alive at ref-count 1 with **no `GstObject` holder**, which reads as an impossible leak.
  `filters=` also restricts what the tracer *tracks*, so naming types by hand still hides any
  holder you did not think of; the run that cracked this used the **unfiltered** `leaks` tracer.
- **No teardown hook can reclaim it.** The leaked structure is detached from every object.
  gwd's own lifecycle is correct here: instrumented builds confirm `CUDAContext::drop` **does**
  run and the compositor's `GsCUDABuf` **does** drop. So the earlier hypothesis — that gwd was
  missing a CUDA analogue of its per-element Vulkan `VulkanShare::clear()` — is **disproved**,
  and clearing `settings.cuda_context` in `stop()` was correctly observed to change nothing.

**Scope: NVIDIA only.** Both hunks are inside `#[cfg(feature = "cuda")]` code (`allocator/cuda/`
and the cuda-gated `decide_allocation`), and `--features cuda` is enabled only by the `nv`
Dockerfile target (`CUDA_ENABLE=1`). The VA, Vulkan and software paths do not compile either
site, so **RADV / VA / Vulkan images are byte-identical** and no AMD or Intel behaviour changes.

**Relationship to the node-agent singleton.** `cuda_share.rs::shared_cuda_context()` (develop
`3185a4f`) made the app-owned context a process-global singleton per `cuda_device_id`, which
**bounded** this leak (one context leaked once instead of one per session) without fixing it —
post-singleton the symptom is a monotonically growing refcount on one shared object. The
singleton is the right architecture regardless (nvcodec shares one context per device; it saves
~70 ms of session start), so it stays; this patch makes it leak-**free** rather than
leak-**bounded**, and restores correct behaviour for any consumer that does not share one
context process-wide.

| Field | Value |
|---|---|
| Origin | Quasar (this repo) — not vendored |
| Patches | `games-on-whales/gst-wayland-display`: `wayland-display-core/src/utils/allocator/cuda/mod.rs`, `gst-plugin-wayland-display/src/waylandsrc/imp.rs` |
| Authored against | `43d4c25`, on top of the other nine `gst-wayland-display-*` patches (applies **10th/last**; three of them also touch `imp.rs`) |
| Verified | Tower RTX 5090, `quasar-nv` + `QUASAR_ENCODER=nvenc`, unfiltered `leaks` tracer. Before: +1 alive `GstCudaStream` and +1 `GstCudaContext` ref per session (1 session -> ref-count 2, 2 sessions -> 3). After, 3 sequential sessions: **0** alive `GstCudaStream`, `GstCudaContext` ref-count flat at **1** (the agent's own cached `GstContext`), VRAM flat at 544 MiB, zero GStreamer criticals, every session `DECODE OK` at 59-60 fps |
| Upstream status | offer as **finding #8** on [gst-wayland-display PR #37](https://github.com/games-on-whales/gst-wayland-display/pull/37) — matters for any multi-tenant / multi-session CUDA use of `waylanddisplaysrc`, vendor-neutral within the CUDA path |

**Known residual (not addressed here, not caused by this patch):** roughly one `GstCaps` per
one-to-two sessions stays alive both before and after the fix (1 alive after 2 sessions, 2 after
3). Small and unrelated to the context chain; worth a separate look.

If `GST_WAYLAND_DISPLAY_REF` moves, re-check that `CUDABufferPool::get_updated_size()` still
calls `gst_buffer_pool_get_config()` and that `decide_allocation()` still adopts the query caps —
if upstream has fixed either, re-diff only the remaining hunk (or **drop the patch**) rather than
forcing it, and update `docs/third-party-pins.md`.

## License note

These patches modify **GStreamer** (`gst-plugins-bad`), which is licensed
**LGPL-2.1-or-later** — that license, not gst-wayland-display's, governs the
patched files themselves. The vendored DPB + h265 patches are sourced from the
`games-on-whales/gst-wayland-display` repository, which is MIT-licensed, but
vendoring a patch file does not relicense the code it patches; the resulting
built `libgstvulkan*.so` remains an LGPL-2.1+ GStreamer plugin like any other
`gst-plugins-bad` element. The Quasar-authored rc-fix and num-slices patches
likewise modify LGPL-2.1+ GStreamer code and carry that license.

## Provenance pin

The DPB patch (vendored verbatim):

| Field | Value |
|---|---|
| Source repo | `games-on-whales/gst-wayland-display` |
| Source PR | [#37](https://github.com/games-on-whales/gst-wayland-display/pull/37) `feat/vulkan-encode` |
| Vendored at commit | `43d4c25` |
| Applied against | GStreamer `1.28.4` (`gst-plugins-bad`) |

If the pin moves, re-fetch `vkh264enc-dpb-pool-in-new-sequence.patch` from the
new commit's `patches/` directory verbatim (do not hand-edit) and update this
table plus `docs/third-party-pins.md`.

The h265 patch (originally PR #37, now Quasar-modified):

| Field | Value |
|---|---|
| Source PR | [#37](https://github.com/games-on-whales/gst-wayland-display/pull/37) `feat/vulkan-encode` @ `43d4c25` |
| Quasar modification | 2026-07-24 NVIDIA bitstream-conformance fix (`vkh265enc.c`) — resolution spec `docs/design/plans/2026-07-24-vulkanh265enc-conformance-resolution-spec.md`, Option A1 |
| Regenerated against | pristine GStreamer `1.28.4` + the DPB patch (applies second, before rc-fix) |
| Upstream status | fix to be reported on PR #37 / MR !5739 (see resolution spec §5) |

The h265 patch is **no longer verbatim**: it is regenerated against the DPB
baseline (like the Quasar-authored patches below), so if `GST_WAYLAND_DISPLAY_REF`
or the DPB patch moves, re-diff it against the new baseline and re-apply the
conformance fix — do **not** re-fetch it verbatim (that would silently drop the
fix). Update this table plus `docs/third-party-pins.md`.

The rc-fix patch (Quasar-authored):

| Field | Value |
|---|---|
| Origin | Quasar (this repo) — not vendored |
| Authored against | GStreamer `1.28.4` (`gst-plugins-bad`), on top of the DPB patch |
| Upstream status | unreported as of 2026-07-05; issue to be filed |

The rc-fix patch is regenerated against the baseline (DPB + h265 applied), so if
GStreamer or the DPB patch moves, re-diff it against the new baseline rather than
re-fetching it.

The num-slices patch (Quasar-authored):

| Field | Value |
|---|---|
| Origin | Quasar (this repo) — not vendored |
| Authored against | GStreamer `1.28.4` (`gst-plugins-bad`), on top of DPB + h265 + rc-fix |
| Upstream status | unreported as of 2026-07-05; issue to be filed |

The num-slices patch is regenerated against the DPB + h265 + rc-fix baseline
(it applies fourth), so re-diff it against the new baseline rather than
re-fetching it if any earlier patch moves.

The intra-refresh patch (Quasar-authored):

| Field | Value |
|---|---|
| Origin | Quasar (this repo) — not vendored |
| Authored against | GStreamer `1.28.4` (`gst-plugins-bad`), on top of DPB + h265 + rc-fix + num-slices + global-submit-lock (applies **sixth** of nine) |
| Requires | Vulkan headers ≥ 1.4.321 (image ships 1.4.341) and a driver exposing `VK_KHR_video_encode_intra_refresh` (RADV Mesa ≥ 25.3, current NVIDIA) |
| Upstream status | unreported as of 2026-07-20; issue to be filed with the rc-fix/num-slices batch |

The intra-refresh patch is regenerated against the five-patch baseline (it applies
sixth), so re-diff it against the new baseline rather than re-fetching it if any
earlier patch moves.

The vulkan-pts patch (Quasar-authored, patches gst-wayland-display — not GStreamer):

| Field | Value |
|---|---|
| Origin | Quasar (this repo) — not vendored |
| Patches | `games-on-whales/gst-wayland-display` (compositor), file `wayland-display-core/src/utils/vulkan_nv12.rs` |
| Authored against | `43d4c25` (the `GST_WAYLAND_DISPLAY_REF` this image builds) |
| Upstream status | to be reported on [gst-wayland-display PR #37](https://github.com/games-on-whales/gst-wayland-display/pull/37) |

If `GST_WAYLAND_DISPLAY_REF` moves, re-diff this patch against the new commit
(the `to_gst_buffer` locus is stable, but confirm it applies) and update this
table plus `docs/third-party-pins.md`.

The pointer-enter-refocus patch (Quasar-authored, patches gst-wayland-display — not GStreamer):

| Field | Value |
|---|---|
| Origin | Quasar (this repo) — authored record of fork commit `cc07f1b` (branch `fix/432-pointer-enter-race`) |
| Patches | `salty2011/gst-wayland-display` (compositor): `comp/input.rs`, `comp/mod.rs`, `wayland/handlers/compositor.rs`, `tests/test_pointer.rs` |
| Authored against | fork `develop` `b202563` + `fix/keymap-memfd-400` (`ded71c0`) |
| What | Edge-triggered `wl_pointer` refocus (quasar issue #432): smithay emits `enter` only on a focus transition and broadcasts to the resources existing at that instant, so a client calling `wl_seat.get_pointer` after the map-time synthetic motion never receives an enter and ignores all pointer input forever (rootful Xwayland deterministically; gamescope intermittently, self-healing on re-map). The map handler arms a flag AFTER its synthetic motion; the next real motion — guarded on no grab, no active constraint, surface present — forces smithay's focus to `None` so the following motion re-emits a genuine `enter` with a serial newer than the forced leave's. |
| Upstream status | unreported; the root fix (retroactive enter on `GetPointer`) belongs in smithay itself and should be filed there |

Like the other `gst-wayland-display-*` patches this is NOT applied at build time —
the build compiles the fork branch; this file is the authored record and upstream
submission source.
