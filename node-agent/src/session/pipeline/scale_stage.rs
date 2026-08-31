//! The mutable encode-side scale stage — the single moving part of adaptive external
//! resolution (`docs/superpowers/specs/2026-08-16-adaptive-external-resolution-design.md`, D7).
//!
//! ```text
//!  SOURCE pipe:  waylanddisplaysrc → … → capsfilter(raw_video_caps) → interpipesink
//!  ENCODE pipe:  interpipesrc(caps FIXED, allow-renegotiation=false) → queue
//!                  → [ SCALE STAGE ] → encoder → capsfilter(codec) → parse → pay → webrtcbin
//! ```
//!
//! Everything there is pinned at the LAUNCH size for the session's life: the source tail
//! caps, the interpipe caps (the swap machinery needs both sides matching exactly), and
//! `webrtcbin`, which must never renegotiate (the one-offer invariant, P2-02). The only
//! size that may move is the capsfilter at the tail of the convert stage: re-setting its
//! `caps` renegotiates exactly the `convert/scale → encoder` link, leaving the compositor,
//! the interpipe and the transport untouched. [`ScaleStage`] is that capsfilter plus the
//! elements feeding it.
//!
//! | encoder    | elements                                  | `supported` |
//! |------------|-------------------------------------------|-------------|
//! | VA         | `[videorate !] vapostproc ! capsfilter`      | yes — `vapostproc` scales |
//! | NVENC N-A  | `[videorate !] cudaconvertscale ! capsfilter` | yes |
//! | NVENC N-B  | `[videorate !] cudaupload ! cudaconvertscale ! capsfilter` | yes |
//! | openh264   | `[videorate !] videoscale ! capsfilter`      | yes (software) |
//! | Vulkan     | `[videorate !] vulkanscale ! capsfilter`     | iff `vulkanscale` registers and [`vulkan_resize_validated_for_codec`], else *(none)* / no |
//!
//! `videorate` is the SPT-08 D7 fps rung's rate adjuster, present only when
//! `abr_ladder_fps` is on for the session (the rung ships dark: knob off ⇒ the graph is
//! byte-identical to the pre-rung one) and absent on an image built without
//! `gst-plugins-base:videorate` (then `fps_lever` is false). When present it must LEAD the
//! stage — see [`build_scale_stage`] for the measurement.
//!
//! The openh264 arm's `videoscale` + capsfilter exists only to give the software path the
//! same lever; at the launch size it is a passthrough. The Vulkan arm's scaler
//! `vulkanscale` (#501) lives in the Quasar `gst-wayland-display` fork and is looked up at
//! build time: an image whose pin predates the element falls back to a zero-element
//! topology with `supported = false`, so metrics never claim a resize the host cannot serve.
//! Origin: `docs/design/research/2026-08-16-vulkan-encode-scale-spike.md` (recommendation A,
//! after finding no in-tree Vulkan scaler in gst 1.28.4).

use std::sync::atomic::{AtomicI32, Ordering};

use anyhow::{anyhow, Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;

use crate::session::{Codec, EncoderChoice, SessionConfig};

use super::caps::encoder_input_caps_for;
use super::source_branch::make_vapostproc;

/// The encode-pipeline stage that converts the interpipe's raw frames into the encoder's
/// input format, and is also the resolution lever.
///
/// Construct with [`build_scale_stage`]. `elements` are in link order and the caller links
/// them between the leaky queue and the encoder; `filter` is the tail capsfilter (also in
/// `elements`) and the only element ever mutated after the graph is built.
#[derive(Debug)]
pub struct ScaleStage {
    /// The stage's elements in link order (empty on a Vulkan image without `vulkanscale`).
    pub elements: Vec<gst::Element>,
    /// The mutable tail capsfilter — `None` when the path has no lever.
    pub filter: Option<gst::Element>,
    /// Whether this host+encoder can change the external resolution live. Reported to the
    /// control plane as `external_resize_supported`.
    pub supported: bool,
    /// The launch size: the ceiling for [`ScaleStage::set_size`] and the build size.
    pub launch: (i32, i32),
    /// The LAUNCH framerate: the ceiling for [`ScaleStage::set_fps`] and the build rate.
    /// Every caps write must carry a `framerate` or the negotiation drifts — but the value
    /// re-stamped on a resize is the CURRENT rate, not this one.
    pub fps: i32,
    /// SPT-08 D7: whether the stage can change the encoded frame RATE live. `false` with
    /// no lever at all, and when `videorate` is missing from the image.
    pub fps_lever: bool,
    /// The effective encoder, already resolved for the per-session codec fallback.
    pub choice: EncoderChoice,
    current_w: AtomicI32,
    current_h: AtomicI32,
    current_fps: AtomicI32,
}

impl ScaleStage {
    /// Retarget the encoder input to `width` x `height`, via one
    /// `set_property("caps", …)` on the tail capsfilter. Safe while PLAYING: the
    /// renegotiation is contained to the `convert/scale → encoder` link. `Ok(false)` is
    /// the no-op guard — the control plane re-sends the current rung freely, and a
    /// duplicate must cost neither a renegotiation nor an IDR.
    ///
    /// This only ISSUES the request: the graph renegotiates lazily on the next buffer, so
    /// nothing downstream has changed when this returns. The IDR and the `session_metrics`
    /// echo are therefore driven by the completion probe, which the caller must arm
    /// (`super::resize::arm_on_next_caps`) BEFORE calling this.
    /// `super::abr_glue::EncodeResolutionLever` owns that pairing; prefer it.
    ///
    /// Rejects rather than clamps, so an upstream bug surfaces as an error instead of a
    /// silently wrong frame size: an unsupported encode path, non-positive dimensions, odd
    /// dimensions (4:2:0 chroma), or anything above the launch size (the compositor has no
    /// more pixels than that; upscaling belongs in the client).
    pub fn set_size(&self, width: i32, height: i32) -> Result<bool> {
        if !self.would_change(width, height)? {
            tracing::debug!(
                "external resolution: already at {width}x{height} — no-op (no caps set, no IDR)"
            );
            return Ok(false);
        }
        let filter = self.filter.as_ref().ok_or_else(|| {
            anyhow!(
                "scale stage for {:?} reports supported but has no capsfilter",
                self.choice
            )
        })?;
        let (lw, lh) = self.launch;
        // Re-stamp the CURRENT rate, never `self.fps`: the two levers share one
        // capsfilter, so writing the launch rate back would silently undo an fps step.
        filter.set_property(
            "caps",
            encoder_input_caps_for(self.choice, width, height, self.current_fps()),
        );
        self.current_w.store(width, Ordering::Relaxed);
        self.current_h.store(height, Ordering::Relaxed);
        tracing::info!(
            "external resolution: encoder input retargeted to {width}x{height} \
             (launch {lw}x{lh}, encoder {:?})",
            self.choice
        );
        Ok(true)
    }

    /// SPT-08 D7 phase 2: retarget the encoded frame RATE. One property write and only
    /// one — the tail capsfilter's `framerate`, through the same
    /// [`encoder_input_caps_for`] path as [`Self::set_size`]. `videorate` in front derives
    /// the decimation from its input rate vs. the rate the filter now asks for, and
    /// `drop-only=true` stops it inventing a frame on the way back up.
    ///
    /// NEVER write `videorate`'s `max-rate`: the T9 spike
    /// (`docs/design/research/2026-08-16-fps-rung-spike.md`) measured writing it alone as a
    /// hard `not-negotiated` that kills the source and ends the session, and writing it
    /// before the capsfilter as a race on the same failure. It stays unset for the session.
    /// Equally `videorate` must BE in the graph — without it a framerate-only caps change
    /// is the same `not-negotiated` death, not a no-op.
    ///
    /// Same downstream-only renegotiation as [`Self::set_size`], so `webrtcbin` never
    /// re-offers; `Ok(false)` is the no-op guard.
    ///
    /// The caller must follow a `true` with the CPB rewrite (`write_encoder_bitrate`): the
    /// VBV budget is sized `kbps / fps`, so skipping it leaves the per-frame budget wrong
    /// by 2x. `abr_glue` owns that pairing.
    ///
    /// Rejects rather than clamps: above the launch rate, or below 1.
    pub fn set_fps(&self, fps: i32) -> Result<bool> {
        if !self.would_change_fps(fps)? {
            tracing::debug!("fps rung: already at {fps} fps — no-op (no caps set)");
            return Ok(false);
        }
        let filter = self.filter.as_ref().ok_or_else(|| {
            anyhow!(
                "scale stage for {:?} reports an fps lever but has no capsfilter",
                self.choice
            )
        })?;
        let (w, h) = self.requested();
        filter.set_property("caps", encoder_input_caps_for(self.choice, w, h, fps));
        self.current_fps.store(fps, Ordering::Relaxed);
        tracing::info!(
            "fps rung: encoder input retargeted to {fps} fps (launch {} fps, {w}x{h}, encoder {:?})",
            self.fps,
            self.choice
        );
        Ok(true)
    }

    /// [`Self::would_change`]'s counterpart for the fps lever.
    pub fn would_change_fps(&self, fps: i32) -> Result<bool> {
        if !self.fps_lever {
            return Err(anyhow!(
                "the fps rung is unsupported on the {:?} encode path (or `videorate` is \
                 missing from this image)",
                self.choice
            ));
        }
        if fps < 1 {
            return Err(anyhow!("fps rung: {fps} fps must be at least 1"));
        }
        if fps > self.fps {
            return Err(anyhow!(
                "fps rung: {fps} fps exceeds the launch rate {} fps (the compositor \
                 produces no more frames than this)",
                self.fps
            ));
        }
        Ok(self.current_fps() != fps)
    }

    /// The rate the stage is asking for (the launch rate until the first `set_fps`).
    pub fn current_fps(&self) -> i32 {
        self.current_fps.load(Ordering::Relaxed)
    }

    /// Everything [`Self::set_size`] would reject plus the no-op check, without touching
    /// the graph. `Ok(false)` means legal but already at that size.
    ///
    /// Split out because the completion probe must be armed BEFORE the caps are set (the
    /// streaming thread can push the renegotiating buffer between the two), and arming for
    /// a request about to be rejected leaves a probe waiting for an event that never comes.
    pub fn would_change(&self, width: i32, height: i32) -> Result<bool> {
        if !self.supported {
            return Err(anyhow!(
                "external resize is unsupported on the {:?} encode path",
                self.choice
            ));
        }
        if width <= 0 || height <= 0 {
            return Err(anyhow!(
                "external resize to {width}x{height}: dimensions must be positive"
            ));
        }
        if width % 2 != 0 || height % 2 != 0 {
            return Err(anyhow!(
                "external resize to {width}x{height}: 4:2:0 chroma needs even dimensions"
            ));
        }
        let (lw, lh) = self.launch;
        if width > lw || height > lh {
            return Err(anyhow!(
                "external resize to {width}x{height} exceeds the launch size {lw}x{lh} \
                 (the compositor has no more pixels than this — upscaling belongs in the client)"
            ));
        }
        Ok(self.requested() != (width, height))
    }

    /// The size last asked for, not necessarily what the graph has negotiated: use it for
    /// the no-op guard and [`Self::current`] for anything reported outward.
    pub fn requested(&self) -> (i32, i32) {
        (
            self.current_w.load(Ordering::Relaxed),
            self.current_h.load(Ordering::Relaxed),
        )
    }

    /// The size the encoder input is ACTUALLY negotiated at, read off the tail
    /// capsfilter's src pad; falls back to [`Self::requested`] only when the pad carries no
    /// caps yet. Not the requested size, because a `set_size` could fail to negotiate (a
    /// `cudaconvert` that cannot scale, or an encoder whose `set_format` rejects it).
    ///
    /// Reading this immediately after a `set_size` is deterministically ONE STEP STALE.
    /// The `session_metrics` echo comes from the completion probe
    /// (`super::resize::arm_on_next_caps`), not from here.
    pub fn current(&self) -> (i32, i32) {
        let negotiated = self
            .filter
            .as_ref()
            .and_then(|f| f.static_pad("src"))
            .and_then(|pad| pad.current_caps())
            .and_then(|caps| {
                let s = caps.structure(0)?;
                Some((s.get("width").ok()?, s.get("height").ok()?))
            });
        negotiated.unwrap_or_else(|| self.requested())
    }
}

/// Whether an encode path can change the external resolution live — the source of truth
/// behind both [`ScaleStage::supported`] and the `external_resize_supported` the agent
/// publishes on `session_metrics`.
///
/// Every path but Vulkan always has a lever (its convert element doubles as the scaler).
/// Vulkan's exists iff the `vulkanscale` factory (#501, from the Quasar
/// `gst-wayland-display` fork) is registered AND
/// [`vulkan_resize_validated_for_codec`] passes the session's codec. Either miss returns
/// `false`, so metrics never lie on a stale image or an unvalidated codec.
pub fn supports_external_resize(choice: EncoderChoice, codec: Codec) -> bool {
    match choice {
        EncoderChoice::Vulkan => {
            vulkanscale_factory().is_some() && vulkan_resize_validated_for_codec(codec)
        }
        _ => true,
    }
}

/// The `vulkanscale` factory, if this image's `gst-wayland-display` build carries it.
/// Looked up per call rather than cached: the registry is a process-lifetime singleton and
/// `ElementFactory::find` is cheap.
fn vulkanscale_factory() -> Option<gst::ElementFactory> {
    gst::ElementFactory::find("vulkanscale")
}

/// Whether THIS process's registry carries `vulkanscale`. Shared by every
/// "without vulkanscale" test here, in `abr_glue`, and in the `pipeline` facade (#533):
/// they assert the pre-#501 matrix and must SKIP, not fail, in a registry that already
/// carries the fork's element. Calls `gst::init` itself (cheap and idempotent).
#[cfg(test)]
pub(crate) fn vulkanscale_present() -> bool {
    gst::init().unwrap();
    gst::ElementFactory::find("vulkanscale").is_some()
}

/// #501 validation gate, per codec, for the Vulkan resize lever.
///
/// HARD DEPENDENCY: a mid-stream size GROW on `vulkanh264enc`/`vulkanh265enc` without the
/// vendored `vulkan-enc-output-state-on-resize.patch` is a GPU-level fault (Xid 31), proven
/// live. With it every codec resizes cleanly. That patch and the `vulkanscale` factory land
/// in the same fork/image build, so an image with one is expected to have the other.
///
/// h265 was gated off at first on the belief that `vulkanh265enc` could not start at all
/// (`Video profile format not supported (-1000023003)`); that was a probe artifact — the
/// element advertises `main-444`, which its own `H265ProfileMap[]` cannot map, and a probe
/// leaving `profile` free negotiates it. The production path always pinned
/// `video/x-h265,profile=main` ([`super::caps::caps_profile`]), and with it pinned the
/// fork's GPU test flips 720p→1080p at 59.7 fps with no fault
/// (`docs/reports/2026-08-22-vulkanscale-validation/H265-PROFILE-CAPS.md`).
///
/// Exhaustive rather than `!matches!` so adding a codec is a compile error here.
fn vulkan_resize_validated_for_codec(codec: Codec) -> bool {
    match codec {
        Codec::H264 | Codec::H265 | Codec::Av1 => true,
    }
}

/// The CUDA convert element for the NVENC stage. `cudaconvert` keeps `width`/`height`
/// fixed in its caps transform, so it cannot be the resolution lever; `cudaconvertscale`
/// (same base class, both capabilities) can. Looked up at build time so an image whose
/// `nvcodec` plugin predates the split still builds, degraded: a resize then fails to
/// negotiate, surfacing as a pipeline error rather than a silent wrong-size stream.
fn cuda_convert_factory() -> &'static str {
    if gst::ElementFactory::find("cudaconvertscale").is_some() {
        "cudaconvertscale"
    } else {
        tracing::warn!(
            token = "cudaconvertscale-missing",
            "cudaconvertscale is absent from this image's nvcodec plugin — falling back to \
             cudaconvert, which does not scale; external resolution changes will fail to \
             negotiate on this host"
        );
        "cudaconvert"
    }
}

/// Build the encode-pipeline scale stage for `cfg`'s (already effective) encoder.
///
/// `va_ctx` is the shared `GstVaDisplay` context, which MUST be injected at VA element
/// creation (`make_vapostproc` / `va_share.rs`) before any property read binds its own.
pub(super) fn build_scale_stage(
    cfg: &SessionConfig,
    va_ctx: Option<&gst::Context>,
) -> Result<ScaleStage> {
    let (w, h, fps) = (cfg.stream.width, cfg.stream.height, cfg.stream.fps);
    let choice = cfg.encoder;

    let make = |name: &str| -> Result<gst::Element> {
        gst::ElementFactory::make(name)
            .build()
            .with_context(|| format!("{name} not found — is the GPU plugin loaded?"))
    };
    let tail_filter = || -> Result<gst::Element> {
        gst::ElementFactory::make("capsfilter")
            .property("caps", encoder_input_caps_for(choice, w, h, fps))
            .build()
            .context("capsfilter not found")
    };

    // A path is supported iff it gets a lever, and this is the same answer the agent
    // publishes as `external_resize_supported`. Resolved BEFORE the match so the Vulkan
    // arm never builds a `videorate` it would only throw away.
    let supported = supports_external_resize(choice, cfg.stream.codec);

    // SPT-08 D7: the fps rung's rate adjuster, at the HEAD of the stage — upstream of the
    // converter/scaler, downstream of the leaky queue.
    //
    // The position is measured, not assumed, and it must NOT move to where the T9 spike
    // had it (immediately before the tail capsfilter, where the spike had no resolution
    // lever to break). There, a size change must renegotiate THROUGH a non-scaling element
    // up to the converter, forcing a compositor CUDA pool re-alloc: measured live, the fps
    // step and the first resize survived and the second (1600x900 → 1280x720) killed the
    // session 17 ms after the caps write with `GstInterPipeSrc … streaming stopped, reason
    // not-negotiated (-4)`. At the head, the tail capsfilter negotiates directly with the
    // converter and `videorate` only ever sees the interpipe's fixed launch size; that ran
    // 6 ladder steps with zero warnings and one offer.
    //
    // Cost of the head position: on the VA arm `videorate` becomes a party to the
    // interpipe → `vapostproc` link, where ZC-03's DMABuf-modifier negotiation happens. T9
    // confirmed feature preservation on `memory:CUDAMemory` but NOT on
    // `memory:VAMemory`/DMABuf. Re-run that gate on a VA host before enabling the fps rung
    // there — issue #499. Until it closes, the knob gate keeps VA hosts off this path.
    //
    // `drop-only=true` so it can only drop frames, never duplicate one on the way back up;
    // `max-rate` stays UNSET for the session (writing it is a `not-negotiated` stream kill,
    // see `ScaleStage::set_fps`). At the launch rate it is a passthrough.
    //
    // Absent from an older image ⇒ build without it and report `fps_lever = false`;
    // failing the build would take every session on that host down for a rung that ships
    // dark, and the resolution rung keeps working either way.
    //
    // Knob-gated so the rung ships dark: with `abr_ladder_fps` off the graph must be
    // BYTE-IDENTICAL to the pre-rung one. `ladder_config()` is `Some` only in `smooth`
    // mode, and `fps_enabled` tracks `abr_ladder_fps` alone (#502: the `abr_ladder` master
    // flag no longer disarms this rung).
    let fps_rung_wanted = cfg.ladder_config().is_some_and(|lc| lc.fps_enabled);
    let videorate = if supported && fps_rung_wanted {
        match gst::ElementFactory::find("videorate") {
            Some(_) => Some(
                gst::ElementFactory::make("videorate")
                    .property("drop-only", true)
                    .build()
                    .context("videorate is registered but would not build")?,
            ),
            None => {
                tracing::warn!(
                    token = "videorate-missing",
                    "`videorate` is absent from this image's gst-plugins-base — the SPT-08 fps \
                     rung is unavailable on this host (rebuild with \
                     -Dgst-plugins-base:videorate=enabled). The resolution rung is unaffected."
                );
                None
            }
        }
    } else {
        None
    };
    // `[videorate] ! converters ! tail capsfilter` — the rate adjuster leads the stage.
    let tail_chain = |converters: Vec<gst::Element>, f: gst::Element| -> Vec<gst::Element> {
        let mut els = Vec::with_capacity(converters.len() + 2);
        if let Some(vr) = videorate.clone() {
            els.push(vr);
        }
        els.extend(converters);
        els.push(f);
        els
    };

    let (elements, filter): (Vec<gst::Element>, Option<gst::Element>) = match choice {
        EncoderChoice::Va => {
            let pp = make_vapostproc(cfg.render_node.as_str(), va_ctx)?;
            let f = tail_filter()?;
            (tail_chain(vec![pp], f.clone()), Some(f))
        }
        #[cfg(feature = "cuda")]
        EncoderChoice::Nvenc => {
            // N-A: the interpipe carries memory:CUDAMemory BGRA (zero-copy); the convert
            // makes CUDAMemory NV12 in place and scales in the same pass. No cudaupload —
            // there is no system hop. Shares the injected GstCudaContext in-pipe.
            let factory = cuda_convert_factory();
            tracing::info!("CUDA convert/scale (zero-copy CUDAMemory BGRA→NV12): {factory}");
            let f = tail_filter()?;
            (tail_chain(vec![make(factory)?], f.clone()), Some(f))
        }
        #[cfg(not(feature = "cuda"))]
        EncoderChoice::Nvenc => {
            // N-B: the interpipe carries system RGBx; cudaupload copies it to CUDA and the
            // convert produces CUDAMemory NV12. No cross-pipeline context share.
            let factory = cuda_convert_factory();
            tracing::info!("CUDA convert/scale (GPU upload + RGBx→NV12): cudaupload ! {factory}");
            let f = tail_filter()?;
            (
                tail_chain(vec![make("cudaupload")?, make(factory)?], f.clone()),
                Some(f),
            )
        }
        // VK-05 + #501: the interpipe carries memory:VulkanImage NV12. With `vulkanscale`
        // in the image it is the scaler (a passthrough at the launch size); without it the
        // arm falls back to a zero-element topology and `supported` is already false, so
        // no capsfilter is built for a lever that would not work.
        EncoderChoice::Vulkan => {
            if supported {
                let f = tail_filter()?;
                (tail_chain(vec![make("vulkanscale")?], f.clone()), Some(f))
            } else {
                (vec![], None)
            }
        }
        EncoderChoice::Openh264 => {
            // The interpipe carries system I420 at the launch size, so this is a
            // passthrough until something calls set_size. It exists only to give the
            // software path the lever the hardware arms get from their converters.
            let f = tail_filter()?;
            (tail_chain(vec![make("videoscale")?], f.clone()), Some(f))
        }
    };

    debug_assert_eq!(
        filter.is_some(),
        supported,
        "{choice:?}: supports_external_resize disagrees with the built stage"
    );
    // The fps lever needs BOTH a capsfilter to write and `videorate` in front of it.
    let fps_lever = supported && videorate.is_some();

    Ok(ScaleStage {
        elements,
        filter,
        supported,
        launch: (w, h),
        fps,
        fps_lever,
        choice,
        current_w: AtomicI32::new(w),
        current_h: AtomicI32::new(h),
        current_fps: AtomicI32::new(fps),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::StreamParams;

    fn cfg_for(encoder: EncoderChoice, width: i32, height: i32) -> SessionConfig {
        cfg_with_fps_rung(encoder, width, height, false)
    }

    /// `cfg_for` with the codec pinned — `supported` varies by codec, not encoder alone.
    fn cfg_for_codec(
        encoder: EncoderChoice,
        codec: Codec,
        width: i32,
        height: i32,
    ) -> SessionConfig {
        let mut cfg = cfg_for(encoder, width, height);
        cfg.stream.codec = codec;
        cfg
    }

    /// A session config with the fps rung explicitly on or off. `baseline()` reads
    /// `fps_enabled` from env, so both arms pin it rather than inherit it.
    fn cfg_with_fps_rung(
        encoder: EncoderChoice,
        width: i32,
        height: i32,
        fps_enabled: bool,
    ) -> SessionConfig {
        let mut settings = crate::session::settings::RuntimeSettings::baseline();
        settings.encoder = encoder;
        settings.abr_mode = crate::session::AbrMode::Smooth;
        settings.ladder.enabled = true;
        settings.ladder.fps_enabled = fps_enabled;
        let stream = StreamParams {
            width,
            height,
            fps: 60,
            bitrate_kbps: 8000,
            h264_profile: "constrained-baseline".to_string(),
            codec: crate::session::Codec::H264,
            abr_floor_kbps: 0,
            mic: false,
        };
        SessionConfig::for_assignment_with(&settings, stream, None)
    }

    /// A ScaleStage built without GPU element factories, so the pure policy can be tested
    /// on every encoder arm including those whose plugins this image lacks.
    fn stage_with(choice: EncoderChoice, supported: bool, launch: (i32, i32)) -> ScaleStage {
        gst::init().unwrap();
        let filter = supported.then(|| {
            gst::ElementFactory::make("capsfilter")
                .property(
                    "caps",
                    encoder_input_caps_for(choice, launch.0, launch.1, 60),
                )
                .build()
                .unwrap()
        });
        ScaleStage {
            elements: filter.iter().cloned().collect(),
            filter,
            supported,
            launch,
            fps: 60,
            fps_lever: supported,
            choice,
            current_w: AtomicI32::new(launch.0),
            current_h: AtomicI32::new(launch.1),
            current_fps: AtomicI32::new(60),
        }
    }

    fn filter_fps(stage: &ScaleStage) -> i32 {
        let caps = stage.filter.as_ref().unwrap().property::<gst::Caps>("caps");
        let fr: gst::Fraction = caps.structure(0).unwrap().get("framerate").unwrap();
        fr.numer()
    }

    fn filter_size(stage: &ScaleStage) -> (i32, i32) {
        let caps = stage.filter.as_ref().unwrap().property::<gst::Caps>("caps");
        let s = caps.structure(0).unwrap();
        (s.get("width").unwrap(), s.get("height").unwrap())
    }

    // ── #501: covering `vulkanscale` present vs. absent ─────────────────────────
    //
    // Both halves of the matrix need coverage — "old pin" (factory absent, the arm
    // degrades) and "new pin" (factory present) — and neither can be assumed of this
    // process's registry, so both are gated symmetrically on `vulkanscale_present()`
    // (#533). Whichever half does not match the ambient registry is inert, not failing.
    //
    // Gated on the live registry rather than a `gst::Element::register` stub: `cargo test`
    // runs a module's tests in parallel threads of ONE process and the registry is
    // process-global, so a stub is visible to every concurrent test — including the
    // "absent" ones and `pipeline.rs`'s sibling assertion — and removing it races the same
    // way. Same pattern the fps rung uses for `videorate`'s absence.

    // The `supported` matrix is what the control plane publishes as
    // `external_resize_supported`, so it is a contract.
    #[test]
    fn supported_matrix_per_encoder_without_vulkanscale() {
        gst::init().unwrap();
        if vulkanscale_present() {
            eprintln!(
                "SKIP supported_matrix_per_encoder_without_vulkanscale: this registry carries \
                 vulkanscale (#533) — see supported_matrix_flips_vulkan_true_with_vulkanscale"
            );
            return;
        }
        for (choice, want) in [
            (EncoderChoice::Openh264, true),
            (EncoderChoice::Va, true),
            (EncoderChoice::Nvenc, true),
            (EncoderChoice::Vulkan, false),
        ] {
            // Vulkan + openh264 build with no GPU plugin; VA/CUDA need hardware, so assert
            // through the struct rather than the builder.
            if choice == EncoderChoice::Vulkan || choice == EncoderChoice::Openh264 {
                let stage = build_scale_stage(&cfg_for(choice, 1920, 1080), None).unwrap();
                assert_eq!(stage.supported, want, "{choice:?}");
                assert_eq!(stage.filter.is_some(), want, "{choice:?} filter presence");
                assert_eq!(stage.elements.is_empty(), !want, "{choice:?} elements");
                assert_eq!(stage.launch, (1920, 1080));
                assert_eq!(stage.current(), (1920, 1080));
            } else {
                assert!(
                    stage_with(choice, want, (1920, 1080)).supported,
                    "{choice:?}"
                );
            }
        }
    }

    // "New pin": with `vulkanscale` registered, Vulkan flips to `supported = true` with the
    // same `[scaler, capsfilter]` shape as every other hardware arm.
    #[test]
    fn supported_matrix_flips_vulkan_true_with_vulkanscale() {
        gst::init().unwrap();
        if !vulkanscale_present() {
            return;
        }
        let stage = build_scale_stage(&cfg_for(EncoderChoice::Vulkan, 1920, 1080), None).unwrap();
        assert!(stage.supported);
        assert!(stage.filter.is_some());
        let names: Vec<String> = stage
            .elements
            .iter()
            .map(|e| e.factory().unwrap().name().to_string())
            .collect();
        assert_eq!(names, vec!["vulkanscale", "capsfilter"]);
        assert_eq!(stage.launch, (1920, 1080));
        assert_eq!(stage.current(), (1920, 1080));
    }

    // "Old pin": no elements, no filter, and set_size is a hard error — which the control
    // plane turns into `external_resize_unsupported`.
    #[test]
    fn vulkan_stage_is_inert_and_rejects_resize_without_vulkanscale() {
        gst::init().unwrap();
        if vulkanscale_present() {
            eprintln!(
                "SKIP vulkan_stage_is_inert_and_rejects_resize_without_vulkanscale: this \
                 registry carries vulkanscale (#533) — see vulkan_stage_is_scalable_with_vulkanscale"
            );
            return;
        }
        let stage = build_scale_stage(&cfg_for(EncoderChoice::Vulkan, 1920, 1080), None).unwrap();
        assert!(stage.elements.is_empty());
        assert!(stage.filter.is_none());
        let err = stage.set_size(1280, 720).unwrap_err().to_string();
        assert!(err.contains("unsupported"), "got {err}");
        assert_eq!(
            stage.current(),
            (1920, 1080),
            "a rejected resize must not move current()"
        );
    }

    // "New pin": `set_size` mutates the tail capsfilter and reports `true`, as on every
    // other hardware arm.
    #[test]
    fn vulkan_stage_is_scalable_with_vulkanscale() {
        gst::init().unwrap();
        if !vulkanscale_present() {
            return;
        }
        let stage = build_scale_stage(&cfg_for(EncoderChoice::Vulkan, 1920, 1080), None).unwrap();
        assert!(stage.set_size(1280, 720).unwrap());
        assert_eq!(filter_size(&stage), (1280, 720));
        assert_eq!(stage.requested(), (1280, 720));
        assert!(stage.set_size(1920, 1080).unwrap());
        assert_eq!(filter_size(&stage), (1920, 1080));
    }

    #[test]
    fn vulkan_stage_is_scalable_for_av1_with_vulkanscale() {
        gst::init().unwrap();
        if !vulkanscale_present() {
            return;
        }
        let stage = build_scale_stage(
            &cfg_for_codec(EncoderChoice::Vulkan, Codec::Av1, 1920, 1080),
            None,
        )
        .unwrap();
        assert!(stage.supported);
        assert!(stage.set_size(1280, 720).unwrap());
        assert_eq!(filter_size(&stage), (1280, 720));
    }

    // h265 behaves like h264 and av1 on an image carrying `vulkanscale`; the earlier
    // inert-topology expectation was a probe artifact, see
    // `vulkan_resize_validated_for_codec`.
    #[test]
    fn vulkan_stage_is_scalable_for_h265_with_vulkanscale() {
        gst::init().unwrap();
        if !vulkanscale_present() {
            return;
        }
        let stage = build_scale_stage(
            &cfg_for_codec(EncoderChoice::Vulkan, Codec::H265, 1920, 1080),
            None,
        )
        .unwrap();
        assert!(stage.supported);
        assert!(stage.set_size(1280, 720).unwrap());
        assert_eq!(filter_size(&stage), (1280, 720));
        assert!(stage.set_size(1920, 1080).unwrap());
        assert_eq!(filter_size(&stage), (1920, 1080));
    }

    // With no `vulkanscale`, every Vulkan codec falls back to the inert topology, so
    // metrics never claim a lever the image cannot serve.
    #[test]
    fn vulkan_h265_is_inert_without_vulkanscale() {
        gst::init().unwrap();
        if vulkanscale_present() {
            eprintln!(
                "SKIP vulkan_h265_is_inert_without_vulkanscale: this registry carries \
                 vulkanscale (#533) — see vulkan_stage_is_scalable_for_h265_with_vulkanscale"
            );
            return;
        }
        let stage = build_scale_stage(
            &cfg_for_codec(EncoderChoice::Vulkan, Codec::H265, 1920, 1080),
            None,
        )
        .unwrap();
        assert!(!stage.supported);
        assert!(stage.elements.is_empty());
        assert!(stage.filter.is_none());
        let err = stage.set_size(1280, 720).unwrap_err().to_string();
        assert!(err.contains("unsupported"), "got {err}");
    }

    #[test]
    fn set_size_mutates_the_filter_caps() {
        let stage = stage_with(EncoderChoice::Openh264, true, (1920, 1080));
        assert_eq!(filter_size(&stage), (1920, 1080));
        assert!(stage.set_size(1280, 720).unwrap());
        assert_eq!(filter_size(&stage), (1280, 720));
        assert_eq!(stage.requested(), (1280, 720));
        // …and back up to the launch size.
        assert!(stage.set_size(1920, 1080).unwrap());
        assert_eq!(filter_size(&stage), (1920, 1080));
        assert_eq!(stage.requested(), (1920, 1080));
    }

    // The no-op guard: re-requesting the current size sets no property and reports
    // `false`, so the caller skips the IDR. The control plane re-sends the current rung
    // freely, and that must never cost a renegotiation or a visible keyframe hitch.
    #[test]
    fn set_size_to_the_current_size_is_a_no_op() {
        let stage = stage_with(EncoderChoice::Openh264, true, (1920, 1080));
        assert!(!stage.set_size(1920, 1080).unwrap());
        assert_eq!(filter_size(&stage), (1920, 1080));

        assert!(stage.set_size(1280, 720).unwrap());
        assert!(!stage.set_size(1280, 720).unwrap());
        assert_eq!(filter_size(&stage), (1280, 720));
        assert_eq!(stage.requested(), (1280, 720));
    }

    #[test]
    fn current_falls_back_to_the_request_before_negotiation() {
        let stage = stage_with(EncoderChoice::Openh264, true, (1920, 1080));
        assert_eq!(stage.current(), (1920, 1080));
        stage.set_size(1280, 720).unwrap();
        assert_eq!(stage.current(), (1280, 720));
    }

    // The supported matrix is a pure function of the encode path (plus, for Vulkan, the
    // registry and the codec) — the same one the agent answers
    // `external_resize_supported` with before any pipeline exists.
    #[test]
    fn supports_external_resize_matches_the_built_stage_without_vulkanscale() {
        if vulkanscale_present() {
            eprintln!(
                "SKIP supports_external_resize_matches_the_built_stage_without_vulkanscale: \
                 this registry carries vulkanscale (#533) — see \
                 supports_external_resize_true_for_every_vulkan_codec_with_vulkanscale"
            );
            return;
        }
        for (choice, want) in [
            (EncoderChoice::Openh264, true),
            (EncoderChoice::Va, true),
            (EncoderChoice::Nvenc, true),
            (EncoderChoice::Vulkan, false),
        ] {
            assert_eq!(
                supports_external_resize(choice, Codec::H264),
                want,
                "{choice:?}"
            );
        }
    }

    #[test]
    fn supports_external_resize_true_for_every_vulkan_codec_with_vulkanscale() {
        gst::init().unwrap();
        if !vulkanscale_present() {
            return;
        }
        assert!(supports_external_resize(EncoderChoice::Vulkan, Codec::H264));
        assert!(supports_external_resize(EncoderChoice::Vulkan, Codec::H265));
        assert!(supports_external_resize(EncoderChoice::Vulkan, Codec::Av1));
    }

    // With `vulkanscale` absent the answer is `false` for every codec except av1, which is
    // `true` for an unrelated reason: it falls back per session to a vendor HW encoder
    // that always has a scaler.
    #[test]
    fn supports_external_resize_false_for_vulkan_h264_h265_without_vulkanscale() {
        gst::init().unwrap();
        if vulkanscale_present() {
            eprintln!(
                "SKIP supports_external_resize_false_for_vulkan_h264_h265_without_vulkanscale: \
                 this registry carries vulkanscale (#533) — see \
                 supports_external_resize_true_for_every_vulkan_codec_with_vulkanscale"
            );
            return;
        }
        assert!(!supports_external_resize(
            EncoderChoice::Vulkan,
            Codec::H264
        ));
        assert!(!supports_external_resize(
            EncoderChoice::Vulkan,
            Codec::H265
        ));
    }

    // The codec gate isolated from the factory probe: fails first if someone re-gates a
    // codec without updating the doc.
    #[test]
    fn the_codec_validation_gate_passes_every_codec() {
        for codec in [Codec::H264, Codec::H265, Codec::Av1] {
            assert!(
                vulkan_resize_validated_for_codec(codec),
                "{codec:?} must be validated"
            );
        }
    }

    // Every rejected input leaves both the caps and current() untouched.
    #[test]
    fn set_size_rejects_illegal_sizes_without_side_effects() {
        let stage = stage_with(EncoderChoice::Openh264, true, (1920, 1080));
        for (w, h, needle) in [
            (0, 720, "positive"),
            (1280, -1, "positive"),
            (1281, 720, "even"),
            (1280, 721, "even"),
            (2560, 1440, "exceeds the launch size"),
            // Only one dimension over the ceiling is still over the ceiling.
            (1920, 1440, "exceeds the launch size"),
        ] {
            let err = stage.set_size(w, h).unwrap_err().to_string();
            assert!(
                err.contains(needle),
                "{w}x{h}: wanted {needle:?}, got {err}"
            );
        }
        assert_eq!(filter_size(&stage), (1920, 1080));
        assert_eq!(stage.requested(), (1920, 1080));
    }

    // The caps the lever writes must carry the encoder's memory feature, not just a size,
    // or a resize silently drops the zero-copy path to system memory.
    #[test]
    fn set_size_preserves_the_encoder_memory_feature() {
        let stage = stage_with(EncoderChoice::Va, true, (1920, 1080));
        stage.set_size(1280, 720).unwrap();
        let caps = stage.filter.as_ref().unwrap().property::<gst::Caps>("caps");
        assert!(caps
            .features(0)
            .map(|f| f.to_string())
            .unwrap_or_default()
            .contains("memory:VAMemory"));
        assert_eq!(filter_size(&stage), (1280, 720));
    }

    // ── SPT-08 D7: the fps lever ──────────────────────────────────────────────

    // The lever is the tail capsfilter's `framerate` and nothing else: writing
    // videorate.max-rate is a `not-negotiated` stream kill (T9 amendment 1).
    #[test]
    fn set_fps_moves_the_tail_caps_framerate() {
        let stage = stage_with(EncoderChoice::Openh264, true, (1920, 1080));
        assert_eq!(stage.current_fps(), 60);
        assert_eq!(filter_fps(&stage), 60);
        assert!(stage.set_fps(30).unwrap());
        assert_eq!(filter_fps(&stage), 30);
        assert_eq!(stage.current_fps(), 30);
        assert!(!stage.set_fps(30).unwrap(), "repeat is a no-op");
        assert!(stage.set_fps(60).unwrap());
        assert_eq!(filter_fps(&stage), 60);
        assert_eq!(stage.current_fps(), 60);
    }

    #[test]
    fn set_fps_rejects_a_rate_above_the_launch_rate_or_below_1() {
        let stage = stage_with(EncoderChoice::Openh264, true, (1920, 1080));
        assert!(
            stage.set_fps(120).is_err(),
            "the source never produces more than launch fps"
        );
        assert!(stage.set_fps(0).is_err());
        assert!(stage.set_fps(-1).is_err());
        assert_eq!(
            stage.current_fps(),
            60,
            "a rejected set_fps leaves the stage alone"
        );
        assert_eq!(filter_fps(&stage), 60);
    }

    // The one drift bug this pairing invites: the two levers share ONE capsfilter, so a
    // resize that re-stamps the LAUNCH rate would silently undo an fps step.
    #[test]
    fn a_resize_after_an_fps_step_keeps_the_reduced_rate() {
        let stage = stage_with(EncoderChoice::Openh264, true, (1920, 1080));
        stage.set_fps(30).unwrap();
        stage.set_size(1280, 720).unwrap();
        assert_eq!(
            filter_fps(&stage),
            30,
            "the resize must not restore the launch fps"
        );
        assert_eq!(filter_size(&stage), (1280, 720));
        // Symmetrically, an fps step after a resize keeps the reduced SIZE.
        stage.set_fps(60).unwrap();
        assert_eq!(filter_size(&stage), (1280, 720));
        assert_eq!(filter_fps(&stage), 60);
    }

    // No capsfilter ⇒ no fps lever either, and it says so.
    #[test]
    fn vulkan_has_no_fps_lever_without_vulkanscale() {
        gst::init().unwrap();
        if vulkanscale_present() {
            eprintln!(
                "SKIP vulkan_has_no_fps_lever_without_vulkanscale: this registry carries \
                 vulkanscale (#533) — see vulkan_has_fps_lever_with_vulkanscale_and_knob_on"
            );
            return;
        }
        let stage = build_scale_stage(&cfg_for(EncoderChoice::Vulkan, 1920, 1080), None).unwrap();
        assert!(!stage.fps_lever);
        let err = stage.set_fps(30).unwrap_err().to_string();
        assert!(err.contains("unsupported"), "got {err}");
        assert_eq!(stage.current_fps(), 60);
    }

    // Once Vulkan has a capsfilter it gets the fps lever, iff `videorate` is also in the
    // image and the rung knob is on.
    #[test]
    fn vulkan_has_fps_lever_with_vulkanscale_and_knob_on() {
        gst::init().unwrap();
        if !vulkanscale_present() {
            return;
        }
        let stage = build_scale_stage(
            &cfg_with_fps_rung(EncoderChoice::Vulkan, 1920, 1080, true),
            None,
        )
        .unwrap();
        if gst::ElementFactory::find("videorate").is_none() {
            assert!(!stage.fps_lever, "no videorate ⇒ no fps lever");
            return;
        }
        assert!(stage.fps_lever);
        assert!(stage.set_fps(30).unwrap());
        assert_eq!(stage.current_fps(), 30);
    }

    // `videorate` is MANDATORY for the lever to work (T9: without it a framerate-only
    // caps change is a `not-negotiated` stream death), and its position is load-bearing
    // for the OTHER lever: it must LEAD the stage so a size change still negotiates
    // directly with the converter. Guards against restoring the T9 harness order.
    #[test]
    fn videorate_leads_the_stage_so_the_resize_still_negotiates_with_the_converter() {
        gst::init().unwrap();
        let stage = build_scale_stage(
            &cfg_with_fps_rung(EncoderChoice::Openh264, 1920, 1080, true),
            None,
        )
        .unwrap();
        if gst::ElementFactory::find("videorate").is_none() {
            assert!(!stage.fps_lever, "no videorate ⇒ no fps lever");
            assert_eq!(stage.elements.len(), 2, "videoscale ! capsfilter only");
            return;
        }
        assert!(stage.fps_lever);
        let names: Vec<String> = stage
            .elements
            .iter()
            .map(|e| e.factory().unwrap().name().to_string())
            .collect();
        assert_eq!(
            names,
            vec!["videorate", "videoscale", "capsfilter"],
            "videorate leads the stage; between the scaler and the capsfilter it breaks resize"
        );
        assert_eq!(
            stage.elements.last().unwrap(),
            stage.filter.as_ref().unwrap()
        );
        let vr = &stage.elements[0];
        assert!(
            vr.property::<bool>("drop-only"),
            "must never duplicate a frame"
        );
        assert_eq!(
            vr.property::<i32>("max-rate"),
            i32::MAX,
            "max-rate must stay at its default — writing it is a not-negotiated stream kill (T9)"
        );
    }

    // The rung SHIPS DARK: with `abr_ladder_fps` off the graph must be exactly the
    // pre-rung one — no `videorate`, no new party to the interpipe → converter
    // negotiation (the VA/DMABuf gate, #499).
    #[test]
    fn videorate_is_absent_when_the_fps_rung_is_off() {
        gst::init().unwrap();
        let stage = build_scale_stage(&cfg_for(EncoderChoice::Openh264, 1920, 1080), None).unwrap();
        let names: Vec<String> = stage
            .elements
            .iter()
            .map(|e| e.factory().unwrap().name().to_string())
            .collect();
        assert_eq!(
            names,
            vec!["videoscale", "capsfilter"],
            "knob off ⇒ the pre-rung graph, byte for byte"
        );
        assert!(!stage.fps_lever, "no videorate ⇒ no fps lever");
        // The resolution lever is untouched by the gating.
        assert!(stage.supported);
        assert!(stage.set_size(1280, 720).unwrap());
    }

    // With no capsfilter the Vulkan arm has no fps lever, and must not CONSTRUCT a
    // videorate it would only throw away.
    #[test]
    fn the_vulkan_arm_builds_no_videorate_without_vulkanscale() {
        gst::init().unwrap();
        if vulkanscale_present() {
            eprintln!(
                "SKIP the_vulkan_arm_builds_no_videorate_without_vulkanscale: this registry \
                 carries vulkanscale (#533) — see \
                 the_vulkan_arm_with_vulkanscale_leads_with_videorate_when_knob_is_on"
            );
            return;
        }
        // Knob ON: the point is that the Vulkan arm skips it even when asked.
        let stage = build_scale_stage(
            &cfg_with_fps_rung(EncoderChoice::Vulkan, 1920, 1080, true),
            None,
        )
        .unwrap();
        assert!(
            stage.elements.is_empty(),
            "no elements at all, videorate included"
        );
        assert!(!stage.fps_lever);
    }

    #[test]
    fn the_vulkan_arm_with_vulkanscale_leads_with_videorate_when_knob_is_on() {
        gst::init().unwrap();
        if !vulkanscale_present() {
            return;
        }
        let stage = build_scale_stage(
            &cfg_with_fps_rung(EncoderChoice::Vulkan, 1920, 1080, true),
            None,
        )
        .unwrap();
        if gst::ElementFactory::find("videorate").is_none() {
            assert!(!stage.fps_lever, "no videorate ⇒ no fps lever");
            let names: Vec<String> = stage
                .elements
                .iter()
                .map(|e| e.factory().unwrap().name().to_string())
                .collect();
            assert_eq!(names, vec!["vulkanscale", "capsfilter"]);
            return;
        }
        assert!(stage.fps_lever);
        let names: Vec<String> = stage
            .elements
            .iter()
            .map(|e| e.factory().unwrap().name().to_string())
            .collect();
        assert_eq!(
            names,
            vec!["videorate", "vulkanscale", "capsfilter"],
            "videorate leads the stage on Vulkan too"
        );
    }

    // Knob OFF ⇒ byte-identical to the pre-rung graph even with `vulkanscale` present.
    #[test]
    fn the_vulkan_arm_with_vulkanscale_builds_no_videorate_when_knob_is_off() {
        gst::init().unwrap();
        if !vulkanscale_present() {
            return;
        }
        let stage = build_scale_stage(&cfg_for(EncoderChoice::Vulkan, 1920, 1080), None).unwrap();
        let names: Vec<String> = stage
            .elements
            .iter()
            .map(|e| e.factory().unwrap().name().to_string())
            .collect();
        assert_eq!(names, vec!["vulkanscale", "capsfilter"]);
        assert!(!stage.fps_lever);
    }

    // End to end on the software arm: the framerate write really renegotiates downstream.
    #[test]
    fn software_stage_renegotiates_the_framerate_live() {
        gst::init().unwrap();
        if gst::ElementFactory::find("videorate").is_none() {
            return; // pre-videorate image: the lever is reported unsupported, nothing to test
        }
        let mut cfg = cfg_with_fps_rung(EncoderChoice::Openh264, 640, 360, true);
        cfg.stream.fps = 60;
        let stage = build_scale_stage(&cfg, None).unwrap();
        assert!(stage.fps_lever);

        let pipeline = gst::Pipeline::new();
        let src = gst::ElementFactory::make("videotestsrc")
            .property("is-live", true)
            .build()
            .unwrap();
        let convert = gst::ElementFactory::make("videoconvert").build().unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        pipeline.add_many([&src, &convert, &sink]).unwrap();
        pipeline.add_many(stage.elements.iter()).unwrap();
        let mut chain: Vec<&gst::Element> = vec![&src, &convert];
        chain.extend(stage.elements.iter());
        chain.push(&sink);
        gst::Element::link_many(chain).unwrap();
        pipeline.set_state(gst::State::Playing).unwrap();

        let sink_pad = sink.static_pad("sink").unwrap();
        let negotiated_fps = || -> Option<i32> {
            let caps = sink_pad.current_caps()?;
            let fr: gst::Fraction = caps.structure(0)?.get("framerate").ok()?;
            Some(fr.numer())
        };
        let wait_for = |want: i32| {
            for _ in 0..200 {
                if negotiated_fps() == Some(want) {
                    return true;
                }
                std::thread::sleep(std::time::Duration::from_millis(25));
            }
            false
        };
        assert!(wait_for(60), "stage never negotiated the launch rate");
        assert!(stage.set_fps(30).unwrap());
        assert!(
            wait_for(30),
            "set_fps did not renegotiate downstream: sink saw {:?}",
            negotiated_fps()
        );
        assert!(stage.set_fps(60).unwrap());
        assert!(
            wait_for(60),
            "set_fps back to launch did not renegotiate: sink saw {:?}",
            negotiated_fps()
        );
        pipeline.set_state(gst::State::Null).unwrap();
    }

    // The stage live in `videotestsrc → videoconvert → [stage] → fakesink`: set_size
    // really changes the size downstream, not just a property.
    #[test]
    fn software_stage_renegotiates_downstream_caps_live() {
        gst::init().unwrap();
        let cfg = cfg_for(EncoderChoice::Openh264, 1280, 720);
        let stage = build_scale_stage(&cfg, None).unwrap();

        let pipeline = gst::Pipeline::new();
        let src = gst::ElementFactory::make("videotestsrc")
            .property("is-live", true)
            .build()
            .unwrap();
        let convert = gst::ElementFactory::make("videoconvert").build().unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        pipeline.add_many([&src, &convert, &sink]).unwrap();
        pipeline.add_many(stage.elements.iter()).unwrap();
        let mut chain: Vec<&gst::Element> = vec![&src, &convert];
        chain.extend(stage.elements.iter());
        chain.push(&sink);
        gst::Element::link_many(chain).unwrap();

        pipeline.set_state(gst::State::Playing).unwrap();
        let sink_pad = sink.static_pad("sink").unwrap();
        let negotiated = |pad: &gst::Pad| -> Option<(i32, i32)> {
            let caps = pad.current_caps()?;
            let s = caps.structure(0)?;
            Some((s.get("width").ok()?, s.get("height").ok()?))
        };
        let wait_for = |want: (i32, i32)| {
            for _ in 0..200 {
                if negotiated(&sink_pad) == Some(want) {
                    return true;
                }
                std::thread::sleep(std::time::Duration::from_millis(25));
            }
            false
        };

        assert!(
            wait_for((1280, 720)),
            "stage never negotiated the launch size"
        );
        // current() reads the tail capsfilter's own src pad, so it agrees with the sink.
        assert_eq!(stage.current(), (1280, 720));

        assert!(stage.set_size(640, 360).unwrap());
        assert!(
            wait_for((640, 360)),
            "set_size did not renegotiate downstream: sink saw {:?}",
            negotiated(&sink_pad)
        );
        assert_eq!(
            stage.current(),
            (640, 360),
            "current() must report the NEGOTIATED size"
        );

        assert!(stage.set_size(1280, 720).unwrap());
        assert!(
            wait_for((1280, 720)),
            "set_size back to launch did not renegotiate: sink saw {:?}",
            negotiated(&sink_pad)
        );
        assert_eq!(stage.current(), (1280, 720));

        // Live no-op: already at 1280x720, so nothing is set and nothing renegotiates.
        assert!(!stage.set_size(1280, 720).unwrap());
        assert_eq!(stage.current(), (1280, 720));

        pipeline.set_state(gst::State::Null).unwrap();
    }
}
