//! The session media pipeline + WebRTC negotiation.
//!
//! The agent is the OFFERER (`protocol/signaling.md`): it owns the media, creates the
//! `webrtcbin`, adds the video + audio tracks, creates the `"input"` DataChannel, and
//! drives offer/answer/trickle-ICE. Transport-agnostic — outbound signaling goes on an
//! mpsc channel ([`OutTx`]) that `server.rs` or the control-plane relay pumps.
//!
//! Pipeline (parameterized by [`SessionConfig`]):
//!   <source> -> videoscale -> videoconvert -> {I420|NV12}(WxH@fps)
//!     -> {openh264enc|vah264enc} -> [profile caps] -> h264parse -> rtph264pay
//!     -> application/x-rtp,media=video,encoding-name=H264,payload=96 -> webrtcbin
//!   <audio> -> audioconvert -> audioresample -> opusenc -> rtpopuspay -> webrtcbin
//!
//! Load-bearing invariants, all preserved: the single-offer guard (some browsers re-fire
//! negotiation-needed after the answer, and re-offering SIGABRTs), a real ICE `sdpMid`
//! (strict browsers reject a bare-index mid), NACK/RTX on the video transceiver, and the
//! input DataChannel created before the first offer.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use anyhow::{Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;
use tokio::sync::mpsc::UnboundedSender;

use super::input::InputSink;
use super::metrics::SessionMetrics;
use super::signaling::{PcId, SignalMsg};
use super::virtual_input::VirtualDevices;
use super::{Codec, EncoderChoice, SessionConfig};
use crate::messages::VideoTopology;

mod caps;
pub(crate) use caps::vulkan_image_transport;
use caps::{caps_profile, raw_video_caps};
mod codec_chain;
use codec_chain::{build_bitstream_chain, BitstreamChain};
mod rtp_ext;
use rtp_ext::{
    attach_abs_capture_time_egress_probe, attach_abs_capture_time_probe,
    attach_abs_capture_time_verification_probe,
};
mod audio_branch;
use audio_branch::add_audio_chain;
mod mic_branch;
use mic_branch::add_mic_branch;
mod probes;
use probes::{
    attach_bitstream_capture, attach_encode_probes, attach_encoder_pts_trace,
    attach_rtp_marker_trace, attach_rtp_ts_trace, attach_stage_probes,
    attach_vulkan_parent_meta_probe,
};
mod encoders;
pub(crate) use encoders::{
    describe_codec_plan, probe_codec_support, rearm_vulkan_rc, EncoderKnobs, ResolvedEncoder,
    VULKAN_ENCODER_NAME,
};
use encoders::{
    effective_encoder, make_nvenc_encoder, make_openh264_encoder, make_va_encoder,
    make_vulkan_encoder,
};
mod resize;
// Adaptive external resolution: the keyframe half of a live resize, exported alongside
// `ScaleStage` so the runner's `session_display_update.stream_*` handler has both halves.
pub use resize::{arm_on_next_caps, force_idr, force_idr_from_pad, force_key_unit_event};
/// Stable name for the selected encoder. Runtime diagnostics inspect the element that
/// actually reached PLAYING rather than echoing requested settings.
pub(crate) const VIDEO_ENCODER_NAME: &str = "quasar-video-encoder";
/// Stable name for the RTP payloader, for the same reason: `caps.negotiated` reports the
/// caps the payloader actually agreed, and `by_name` is the only way to reach it from
/// the runner without threading a handle through every builder.
pub(crate) const VIDEO_PAYLOADER_NAME: &str = "quasar-video-payloader";
mod source_branch;
use source_branch::{build_gpu_convert_stage, build_video_source};
mod scale_stage;
use scale_stage::build_scale_stage;
pub use scale_stage::ScaleStage;
mod abr_glue;
use abr_glue::enable_abr;
// The interface the ABR ladder's resolution rung drives, and the runner's manual
// `session_display_update` path with it.
pub(crate) use abr_glue::resolution_lever_with_echo;
pub use abr_glue::{trigger, EncodeResolutionLever, ResolutionLever};
mod webrtc;
pub(crate) use webrtc::restart_ice;
pub(crate) use webrtc::{
    apply_remote_description, build_answerer_pipeline, remote_description_failure_reason,
    RemoteDescriptionApplied, RemoteDescriptionFailure, SrdFailTx, SrdOkTx,
};
use webrtc::{
    connect_data_channel_open, connect_ice_candidate, connect_negotiation_needed,
    connect_state_logging, enable_video_fec, enable_video_rtx, make_webrtcbin,
    set_audio_receive_latency, spawn_fec_controller,
};

/// Outbound signaling messages produced by GStreamer callback threads, drained
/// by whatever transport is driving this session (WS server / control relay).
pub type OutTx = UnboundedSender<SignalMsg>;

/// Bounded, non-blocking trace lane. Critical lifecycle/signaling events do not share
/// this sender.
pub type TraceTx = super::runner::DiagnosticEventTx;

fn input_data_channel_init() -> (gst::Structure, &'static str) {
    let mode = std::env::var("QUASAR_INPUT_CHANNEL_MODE").unwrap_or_default();
    if matches!(
        mode.as_str(),
        "legacy" | "unreliable" | "unordered" | "unreliable-unordered"
    ) {
        return (
            gst::Structure::builder("application/data-channel")
                .field("ordered", false)
                .field("max-retransmits", 0i32)
                .build(),
            "unreliable+unordered",
        );
    }

    (
        gst::Structure::builder("application/data-channel")
            .field("ordered", true)
            .build(),
        "reliable+ordered",
    )
}

/// Whether audio is disabled (`QUASAR_AUDIO_DISABLED`). Strips the audio m-line from the
/// offer so the browser's A/V sync has no audio track to couple the video jitter buffer
/// against (#304 diagnostic). `pub(crate)` so `effective_media.audio` reports from the
/// same source of truth the pipeline builds from rather than re-reading the env.
pub(crate) fn audio_disabled() -> bool {
    matches!(
        std::env::var("QUASAR_AUDIO_DISABLED").ok().as_deref(),
        Some("1") | Some("true") | Some("TRUE")
    )
}

/// Host-level microphone kill switch (`QUASAR_MIC_DISABLED`), flippable without touching
/// the control plane. When set, every session on this host behaves as if `stream.mic`
/// were false: no recvonly transceiver, no mic m-line, no receive bin. Mirrors
/// [`audio_disabled`], including the accepted values. `pub(crate)` for the same
/// single-source-of-truth reason.
pub(crate) fn mic_disabled() -> bool {
    matches!(
        std::env::var("QUASAR_MIC_DISABLED").ok().as_deref(),
        Some("1") | Some("true") | Some("TRUE")
    )
}

/// Resolve the encoder this session's pipeline builds as for its codec, mutating
/// `cfg.encoder` in place (leaving `cfg.configured_encoder` alone) and returning the
/// [`ResolvedEncoder`] — vendor choice plus the exact registered factory name — to thread
/// down to [`build_encoder_element`] rather than have it re-scan.
///
/// The only per-session indirection where the pipeline may build as a different vendor
/// than configured: the Vulkan×AV1 vendor-HW fallback, taken when `QUASAR_VULKAN_AV1` is
/// off or `vulkanav1enc` is absent from the registry. An unsupported (encoder, codec)
/// pair is a hard error: the agent must never silently produce a codec other than
/// `sessions.codec`. Requires `gst::init` (registry probe).
///
/// Reads the Vulkan knobs from env here, at a session's build entry, so they hold for
/// the whole build. `cfg.render_node` (already resolved to the assigned GPU) goes
/// straight to [`effective_encoder`], so a VA factory name is device-pinned.
pub(crate) fn resolve_effective_encoder(cfg: &mut SessionConfig) -> Result<ResolvedEncoder> {
    let configured = cfg.encoder;
    let codec = cfg.stream.codec;
    let knobs = EncoderKnobs::from_env();
    match effective_encoder(configured, codec, knobs, cfg.render_node.as_str()) {
        Some(resolved) => {
            if resolved.choice != configured {
                // Two situations reach the same fallback: a knob the operator set to 0
                // is intent (INFO); a knob left on whose vulkan element the image lacks
                // is a broken image (WARN), which must not scroll past as chatter.
                if configured == EncoderChoice::Vulkan && knobs.allows(codec) {
                    tracing::warn!(
                        token = "encoder-element-missing-fallback",
                        "{} not registered on this image, falling back to {}; check the image \
                         contract (deploy/image-contract.json) — this host is configured for the \
                         vulkan encoder but is not running it for codec={}",
                        encoders::vulkan_element(codec),
                        resolved.factory,
                        codec.as_str()
                    );
                } else {
                    tracing::info!(
                        "codec fallback: configured={configured:?} codec={} → effective={:?} \
                         (element={}; {}=0 disables the vulkan {} encoder on this host, so this \
                         session builds the vendor HW path)",
                        codec.as_str(),
                        resolved.choice,
                        resolved.factory,
                        EncoderKnobs::var_name(codec),
                        codec.as_str()
                    );
                }
                cfg.encoder = resolved.choice;
            }
            Ok(resolved)
        }
        None => Err(anyhow::anyhow!(
            "no encoder on this host can produce codec={} for the configured {configured:?} \
             encoder path — session cannot launch (the control plane should not have assigned \
             an unsupported codec for this host)",
            codec.as_str()
        )),
    }
}

/// The encoder-output `profile` caps field for this codec (`Some` for h264/h265, `None`
/// for av1), warning on the h264 openh264 constrained-baseline downgrade. Shared by both
/// pipeline builders so the warning stays identical.
fn resolve_codec_profile(cfg: &SessionConfig, codec: Codec) -> Result<Option<&'static str>> {
    let profile = caps_profile(codec, &cfg.stream.h264_profile, cfg.encoder)?;
    if codec == Codec::H264 {
        if let Some(effective) = profile {
            if effective != cfg.stream.h264_profile {
                tracing::warn!(
                    token = "h264-profile-downgraded",
                    "H.264 profile '{}' downgraded to '{}' for the {:?} encoder \
                     (openh264enc is constrained-baseline-class; use QUASAR_ENCODER=va for main/high)",
                    cfg.stream.h264_profile,
                    effective,
                    cfg.encoder
                );
            }
        }
    }
    Ok(profile)
}

/// Which vendor builder [`build_encoder_element`] calls for `choice`: the
/// codec-independent half of the vendor×codec dispatch, as a pure mapping. The match is
/// total over `EncoderChoice`, so it cannot drift per-codec the way the other three
/// matrix sites can; `encoders::matrix_tests` exercises it anyway, so a refactor that
/// folds these arms behind a wildcard `_` (silently swallowing a 5th vendor) is caught in
/// test rather than at a live launch. `build_encoder_element` matches on this rather than
/// `cfg.encoder` so the two can never diverge.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum EncoderBuilderKind {
    Va,
    Nvenc,
    Vulkan,
    Openh264,
}

pub(crate) fn encoder_builder_kind(choice: EncoderChoice) -> EncoderBuilderKind {
    match choice {
        EncoderChoice::Va => EncoderBuilderKind::Va,
        EncoderChoice::Nvenc => EncoderBuilderKind::Nvenc,
        EncoderChoice::Vulkan => EncoderBuilderKind::Vulkan,
        EncoderChoice::Openh264 => EncoderBuilderKind::Openh264,
    }
}

/// Build the video encoder element for `cfg`'s (already effective) encoder and `codec`,
/// instantiating `factory` — the exact name [`resolve_effective_encoder`] already
/// resolved, so the hardware builders never re-scan the registry. `va_ctx` is the shared
/// VA display, which must be injected at VA-element creation (`None` on the demo path).
///
/// Vendor×codec matrix: one of four sites edited together when a vendor/codec is added
/// (see [`encoders::encoder_candidates`]; `encoders::matrix_tests` pins them agreeing).
/// This match is total over [`EncoderChoice`] and forwards `codec` unchanged, so adding a
/// codec never touches this function — only `make_openh264_encoder`'s own gate
/// ([`encoders::openh264_supports`]) and the other three sites.
fn build_encoder_element(
    cfg: &SessionConfig,
    codec: Codec,
    va_ctx: Option<&gst::Context>,
    factory: &str,
) -> Result<gst::Element> {
    // The host `gop` is a keyframe interval in frames at the 60 fps REFERENCE; scale it
    // to the session fps so keyframe cadence in TIME is constant (gop=60 ⇒ one keyframe
    // per second at every fps). Unscaled, 120 fps halves the period: a 45 KB AV1 keyframe
    // burst every 500 ms under tight CBR reads as a 2 Hz micro-glitch.
    let gop = encoders::effective_gop(cfg.gop, cfg.stream.fps as u32);
    match encoder_builder_kind(cfg.encoder) {
        EncoderBuilderKind::Va => make_va_encoder(
            codec,
            cfg.stream.bitrate_kbps,
            cfg.stream.fps as u32,
            gop,
            cfg.num_slices,
            cfg.target_usage,
            cfg.render_node.as_str(),
            va_ctx,
            factory,
        ),
        EncoderBuilderKind::Nvenc => make_nvenc_encoder(
            codec,
            cfg.stream.bitrate_kbps,
            cfg.stream.fps as u32,
            gop,
            cfg.cuda_device_id,
            factory,
        ),
        EncoderBuilderKind::Vulkan => make_vulkan_encoder(
            codec,
            cfg.stream.bitrate_kbps,
            cfg.stream.fps as u32,
            gop,
            cfg.num_slices,
            cfg.intra_refresh,
            cfg.intra_refresh_period,
            factory,
        ),
        EncoderBuilderKind::Openh264 => make_openh264_encoder(codec, cfg.stream.bitrate_kbps, gop),
    }
}

/// Harness parity (`probe-encoder`): the production encoder builder, reachable from
/// outside this module. A thin re-export on purpose — `probe-encoder` exists because a
/// hand-typed `gst-launch` probe negotiated a profile production never uses, so the
/// harness must instantiate the same elements with the same properties and the same
/// output capsfilter. A second constructor for the probe would reintroduce that
/// divergence.
pub fn build_encoder_element_for_probe(
    cfg: &SessionConfig,
    codec: Codec,
    factory: &str,
) -> Result<gst::Element> {
    build_encoder_element(cfg, codec, None, factory)
}

/// The production encoder-output capsfilter + parser for `codec`, profile resolved
/// exactly as a session resolves it (`profile=main` for h265 — the field the `main-444`
/// trap turns on). No payloader: the probe has no RTP session, and an unused one would
/// only add a failure mode.
pub fn build_bitstream_chain_for_probe(
    codec: Codec,
    cfg: &SessionConfig,
) -> Result<(gst::Element, gst::Element)> {
    let profile = resolve_codec_profile(cfg, codec)?;
    let chain = build_bitstream_chain(codec, profile)?;
    Ok((chain.encoder_capsfilter, chain.parser))
}

/// The encoder-input caps the scale stage emits, for the probe's stand-in source.
pub fn encoder_input_caps_for(
    choice: EncoderChoice,
    width: i32,
    height: i32,
    fps: i32,
) -> gst::Caps {
    caps::encoder_input_caps_for(choice, width, height, fps)
}

// ─────────────────────────────────────────────────────────────────────────────
// Offerer pipeline (the real session)
// ─────────────────────────────────────────────────────────────────────────────

/// Build the offerer pipeline for `cfg`. Returns the pipeline, the `webrtcbin` (for
/// [`super::server::handle_inbound`] to apply remote SDP/ICE to), and the `"input"`
/// DataChannel (held alive by the caller for the session).
///
/// With `devices`, the compositor element is pointed at the mouse + keyboard evdev nodes
/// (so libinput forwards them to Wayland clients) and DataChannel input is routed to all
/// three devices. `None` ⇒ test-src, input is parse-only.
#[allow(clippy::type_complexity)]
pub fn build_session_pipeline(
    cfg: &SessionConfig,
    out_tx: OutTx,
    devices: Option<Arc<VirtualDevices>>,
) -> Result<(
    gst::Pipeline,
    gst::Element,
    Option<glib::Object>,
    Option<gst::Element>,
)> {
    // Resolve on a local copy so the whole pipeline (source caps, converters, encoder)
    // builds as the effective encoder. Hard-errors on an unsupported (encoder, codec).
    let mut cfg_local = cfg.clone();
    let resolved_encoder = resolve_effective_encoder(&mut cfg_local)?;
    let cfg = &cfg_local;
    let codec = cfg.stream.codec;

    let pipeline = gst::Pipeline::new();
    let (width, height, fps) = (cfg.stream.width, cfg.stream.height, cfg.stream.fps);
    tracing::info!(
        "session pipeline: {width}x{height}@{fps}, {} kbps, codec={}, profile={}, encoder={:?}, source={}",
        cfg.stream.bitrate_kbps,
        codec.as_str(),
        cfg.stream.h264_profile,
        cfg.encoder,
        if cfg.use_test_src {
            "videotestsrc"
        } else {
            "waylanddisplaysrc"
        }
    );
    let effective_profile = resolve_codec_profile(cfg, codec)?;

    // The source chain is added + linked into `pipeline`; `video_tail` is its tail
    // capsfilter. On the VA path that tail is system RGBx, and the GPU `vapostproc`
    // convert to memory:VAMemory NV12 is the `gpu_convert` stage below, linked between
    // the tail and the encoder so it shares the encoder's VADisplay.
    let video_tail = build_video_source(&pipeline, cfg, devices.as_deref())?;
    let gpu_convert = build_gpu_convert_stage(cfg, None)?;

    let encoder = build_encoder_element(cfg, codec, None, &resolved_encoder.factory)?;

    // The profile pinned in the caps between encoder and parser drives the VA/NVENC/
    // Vulkan encoder (none has a `profile` property) and lands verbatim in the SDP offer,
    // so it must equal the real bitstream profile.
    let BitstreamChain {
        encoder_capsfilter: h264_capsfilter,
        parser,
        payloader: pay,
    } = build_bitstream_chain(codec, effective_profile)?;

    // The caps webrtcbin advertises the video m-line from. Static, so the offer can be
    // generated before the first frame flows.
    let rtp_caps = gst::Caps::builder("application/x-rtp")
        .field("media", "video")
        .field("encoding-name", codec.rtp_encoding_name())
        .field("payload", 96_i32)
        .field("clock-rate", 90_000_i32)
        .build();
    let rtp_capsfilter = gst::ElementFactory::make("capsfilter")
        .property("caps", &rtp_caps)
        .build()
        .context("capsfilter not found")?;

    let webrtc = make_webrtcbin(cfg.stun.as_deref())?;

    pipeline.add_many([
        &encoder,
        &h264_capsfilter,
        &parser,
        &pay,
        &rtp_capsfilter,
        &webrtc,
    ])?;
    if !gpu_convert.is_empty() {
        pipeline.add_many(gpu_convert.iter())?;
    }
    // Link video_tail → [GPU convert stage →] encoder → … → rtp.
    let mut chain: Vec<&gst::Element> = vec![&video_tail];
    chain.extend(gpu_convert.iter());
    chain.extend_from_slice(&[&encoder, &h264_capsfilter, &parser, &pay, &rtp_capsfilter]);
    gst::Element::link_many(chain).context("failed to link video encode chain")?;

    // Audio: pulsesrc from the per-session PulseAudio sidecar, or audiotestsrc under
    // `use_test_audio` (audio must never kill video). The helper builds, adds, links,
    // and returns the tail capsfilter. Skipped under QUASAR_AUDIO_DISABLED.
    let audio_disabled = audio_disabled();
    let audio_rtp_capsfilter = if !audio_disabled {
        Some(add_audio_chain(&pipeline, cfg)?)
    } else {
        tracing::info!(
            "audio disabled (QUASAR_AUDIO_DISABLED=1) — video-only offer for #304 validation"
        );
        None
    };

    // Link the video RTP tail into a freshly requested webrtcbin sink pad.
    let src_pad = rtp_capsfilter
        .static_pad("src")
        .context("rtp capsfilter has no src pad")?;
    let sink_pad = webrtc
        .request_pad_simple("sink_%u")
        .context("webrtcbin refused a sink pad request (video)")?;
    src_pad
        .link(&sink_pad)
        .context("failed to link video into webrtcbin")?;

    // NACK + RTX on the video transceiver (index 0): recover a lost packet by
    // retransmission instead of freezing until the next keyframe. Must be set before
    // PLAYING so it lands in the offer.
    enable_video_rtx(&webrtc);
    // ULPFEC/RED proactive burst-loss repair, no RTX round-trip. Demo path negotiates per
    // the plan; the loss-triggered auto ramp rides the encode pipeline instead.
    let fec_plan = cfg.fec_plan();
    enable_video_fec(&webrtc, fec_plan.negotiate, fec_plan.initial_pct);

    // #304: audio belongs in its OWN gst::Pipeline + webrtcbin — two webrtcbins in one
    // pipeline never reach PLAYING (the aggregate state transition stalls), and splitting
    // them keeps the browser's A/V sync from inflating video playout. The demo path does
    // not implement that split: `add_audio_chain` already linked the audio chain into
    // `pipeline`, so this falls back to single-webrtcbin audio. `build_encode_pipeline`
    // is the real path and builds the audio chain into the audio pipe from the start.
    let audio_webrtc = if let Some(audio_tail) = &audio_rtp_capsfilter {
        let audio_pipe = gst::Pipeline::new();
        let audio_webrtc =
            make_webrtcbin(cfg.stun.as_deref()).context("failed to create audio webrtcbin")?;
        audio_pipe.add(&audio_webrtc)?;
        drop(audio_pipe);
        let audio_src_pad = audio_tail
            .static_pad("src")
            .context("audio rtp capsfilter has no src pad")?;
        let audio_sink_pad = webrtc
            .request_pad_simple("sink_%u")
            .context("webrtcbin refused a sink pad request (audio)")?;
        audio_src_pad
            .link(&audio_sink_pad)
            .context("failed to link audio into webrtcbin")?;
        None // single-webrtcbin fallback on this path
    } else {
        None
    };

    // Wire the video callbacks before any state change, or negotiation-needed is missed.
    connect_ice_candidate(&webrtc, out_tx.clone(), PcId::Video);
    // Demo path: no virtual devices and no trace_tx (no control-plane WS to relay to).
    connect_state_logging(&webrtc, None, None, String::new());
    connect_negotiation_needed(&webrtc, out_tx, PcId::Video, String::new());

    // webrtcbin must be at least READY before create-data-channel (it is "closed" in
    // NULL). READY does not fire negotiation-needed (that waits for PLAYING), so the
    // DataChannel still lands in the first offer.
    pipeline
        .set_state(gst::State::Ready)
        .context("session pipeline failed to reach READY")?;

    // Create the "input" DataChannel now so it rides the first (and only) offer instead
    // of triggering a renegotiation later. Reliable + ordered by default;
    // QUASAR_INPUT_CHANNEL_MODE=legacy restores unreliable+unordered for A/B.
    let (dc_init, input_channel_mode) = input_data_channel_init();
    let data_channel = webrtc
        .emit_by_name_with_values(
            "create-data-channel",
            &["input".to_value(), Some(dc_init).to_value()],
        )
        .and_then(|v| v.get::<glib::Object>().ok());
    // With test-src there are no devices, so input is parsed + logged only.
    let input_sink = match devices {
        Some(d) => InputSink::Virtual(d),
        None => InputSink::None,
    };
    match &data_channel {
        Some(dc) => {
            // No launcher↔game swap on this path, so a fresh per-channel InputState
            // (never reset) is sufficient.
            let input_state = Arc::new(crate::session::input::InputState::new());
            connect_data_channel_open(
                dc,
                input_sink,
                input_state,
                width,
                height,
                input_channel_mode,
            );
            tracing::info!("created 'input' DataChannel ({input_channel_mode}, agent side)");
        }
        None => tracing::warn!(
            token = "datachannel-create-returned-nothing",
            "create-data-channel returned no object"
        ),
    }

    Ok((pipeline, webrtc, data_channel, audio_webrtc))
}

// Split pipeline: source ⇢ interpipe ⇢ encode (the launcher↔game swap path).
//
//   • a per-app SOURCE pipeline  : <source> → [scale → convert (sw)] → caps → interpipesink
//   • the persistent ENCODE pipe : interpipesrc → queue → [vapostproc (VA)] → encoder
//                                   → … → webrtcbin (+ audio)
//
// The VA GPU convert must live in the ENCODE pipe, not the source: there it is paced by
// the encoder and shares the encoder's VADisplay. The source just carries the
// compositor's system RGBx across the interpipe.
//
// The encode pipeline, and so the WebRTC transport, is NEVER structurally touched on a
// swap — only `interpipesrc.listen-to` is re-pointed at the new source's interpipesink,
// which is what keeps webrtcbin from renegotiating. Verify with the agent logging exactly
// one `offer created` for the whole session, across every swap.
// (`.claude/rules/gstreamer-gotchas.md`, gst-interpipe entry.)

/// Build one per-app SOURCE pipeline: the compositor (or videotestsrc) capturing
/// the app, normalized to [`raw_video_caps`], terminating at an `interpipesink`
/// named `sink_name`. The encode pipeline's `interpipesrc` listens to that name.
///
/// `forward-eos=false` is load-bearing: tearing this source down on a swap must NOT
/// propagate EOS across the interpipe to the live encoder, which would end the encode
/// pipeline and kill the transport. The compositor's Wayland socket is announced on THIS
/// pipeline's bus; the caller (AppSource) catches it and launches the app container.
pub fn build_source_pipeline(
    cfg: &SessionConfig,
    devices: Option<Arc<VirtualDevices>>,
    sink_name: &str,
) -> Result<gst::Pipeline> {
    let pipeline = gst::Pipeline::new();
    let (width, height, fps) = (cfg.stream.width, cfg.stream.height, cfg.stream.fps);
    tracing::info!(
        "source pipeline '{sink_name}': {width}x{height}@{fps}, source={}",
        if cfg.use_test_src {
            "videotestsrc"
        } else {
            "waylanddisplaysrc"
        }
    );

    let video_tail = build_video_source(&pipeline, cfg, devices.as_deref())?;
    let interpipesink = build_source_interpipe_sink(sink_name)?;

    pipeline.add(&interpipesink)?;
    gst::Element::link_many([&video_tail, &interpipesink])
        .context("failed to link source pipeline")?;
    Ok(pipeline)
}

/// The per-app source pipeline's terminating `interpipesink`. Factored out of
/// [`build_source_pipeline`] so its retention/EOS config is unit-testable.
fn build_source_interpipe_sink(sink_name: &str) -> Result<gst::Element> {
    gst::ElementFactory::make("interpipesink")
        .name(sink_name)
        .property("sync", false)
        .property("async", false)
        // GstInterPipeSink subclasses GstAppSink, which by default holds a ref to the last
        // GstSample forever. On the Vulkan path that sample also pins the compositor's
        // ParentBufferMeta child (G1 gate) and so an encode-src ring slot, alive after
        // every real consumer reaches NULL. Under WOLF_VULKAN_RING=2 (auto-pinned for
        // HEVC; RING=1 starves under the same gate, source_branch.rs) that permanently
        // takes one of the two slots, halving convert()'s double-buffering headroom.
        // Quasar never reads `last-sample`. Also unblocks safe teardown.
        .property("enable-last-sample", false)
        // Never forward EOS to listeners: tearing this source down on a swap must not
        // end the live encode pipeline that is (or was) listening.
        .property("forward-eos", false)
        .property("forward-events", true)
        // Drop buffers with no ready listener so a not-yet-switched source never blocks.
        .property("drop", true)
        .build()
        .context("interpipesink not found — is gst-interpipe installed in the image?")
}

/// Build just the scale stage, for tests that exercise the resolution lever against a
/// live pipeline without standing up encode + webrtcbin (`runner`'s metrics-echo test).
#[cfg(test)]
pub(crate) fn build_scale_stage_for_test(cfg: &SessionConfig) -> Result<ScaleStage> {
    build_scale_stage(cfg, None)
}

/// Everything [`build_encode_pipeline`] hands back to the runner. A struct rather than a
/// tuple: a positional `.6`/`.7` at the call site is how two handles get swapped silently.
pub struct EncodePipeline {
    /// The persistent encode pipeline. PLAYING for the whole session, across every swap.
    pub pipeline: gst::Pipeline,
    /// The video `webrtcbin` (inbound SDP/ICE, the one-offer invariant).
    pub webrtc: gst::Element,
    /// The `"input"` DataChannel, held alive for the session.
    pub data_channel: Option<glib::Object>,
    /// The `interpipesrc` whose `listen-to` the runner re-points on a swap.
    pub interpipesrc: gst::Element,
    /// The audio `webrtcbin` (its own pipeline — #304).
    pub audio_webrtc: Option<gst::Element>,
    /// The audio pipeline (created READY here, driven by the runner).
    pub audio_pipeline: Option<gst::Pipeline>,
    /// The encode-side resolution lever, metrics echo and IDR pairing already wired.
    /// Behind an `Arc` because the ABR ladder holds it too.
    pub resolution_lever: Arc<abr_glue::EncodeResolutionLever>,
}

/// Whether a session assigned `cfg` can change its external (encoded) resolution live:
/// the agent publishes this as `external_resize_supported` and validates
/// `session_display_update.stream_*` against it.
///
/// Keys on the EFFECTIVE encoder. A Vulkan host running an AV1 session falls back to the
/// vendor HW encoder, so that session does have a lever even though the host's configured
/// path does not. Agrees with the `ScaleStage` the runner later builds because both go
/// through [`scale_stage::supports_external_resize`].
///
/// Must NOT call [`effective_encoder`]: that probes the registry (needing `gst::init`),
/// while this is answered on the control-message path before any pipeline or session
/// thread exists. The only per-session indirection is Vulkan × AV1, and both landing
/// sites (NVENC, VA) have a scaler, so the answer is `true` without knowing which wins.
/// With neither registered the session cannot launch, so there is nothing to report.
pub fn external_resize_supported(cfg: &SessionConfig) -> bool {
    if cfg.encoder == EncoderChoice::Vulkan && cfg.stream.codec == crate::session::Codec::Av1 {
        return true;
    }
    scale_stage::supports_external_resize(cfg.encoder, cfg.stream.codec)
}

/// Build the persistent ENCODE pipeline: `interpipesrc` (listening to
/// `initial_sink_name`) → encoder → parser → payloader → webrtcbin, plus the audio chain
/// and the "input" DataChannel.
///
/// This pipeline stays PLAYING for the whole session, across every swap, so the WebRTC
/// transport never renegotiates. The runner re-points `interpipesrc.listen-to` on a swap.
#[allow(clippy::too_many_arguments)]
pub fn build_encode_pipeline(
    cfg: &SessionConfig,
    out_tx: OutTx,
    devices: Option<Arc<VirtualDevices>>,
    input_state: Arc<crate::session::input::InputState>,
    initial_sink_name: &str,
    session_metrics: Arc<SessionMetrics>,
    va_ctx: Option<&gst::Context>,
    trace_tx: TraceTx,
    session_id: String,
    encoder_factory: &str,
) -> Result<EncodePipeline> {
    // `cfg.encoder` is already the effective encoder (the runner ran
    // `resolve_effective_encoder` first), so every arm below keys on it, and
    // `encoder_factory` is the exact element name that resolution found.
    let pipeline = gst::Pipeline::new();
    let (width, height) = (cfg.stream.width, cfg.stream.height);
    let codec = cfg.stream.codec;
    let effective_profile = resolve_codec_profile(cfg, codec)?;

    // The swap boundary: interpipesrc pulls from whichever source's interpipesink
    // `listen-to` names. Caps are pinned so the offer's video format is known before any
    // frame flows; `allow-renegotiation=false` because the caps never change.
    //
    // `do-timestamp=false` is load-bearing (#68). Source and encode pipelines share one
    // `SystemClock` + base time (`runner::run_blocking`), so the source's PTS are already
    // valid running-time here and pass through untouched — and every source stamps in the
    // same timebase, so re-pointing `listen-to` introduces no PTS jump (`stream-sync`
    // stays at its default; `restart-ts` must never be used). `do-timestamp=true`
    // re-stamps each buffer at its bursty arrival instant, and rtpgccbwe then misreads a
    // clean LAN as congested: the estimate collapses to ~245 kbps and the encoder is
    // backpressured to ~0.5 fps. (`.claude/rules/gstreamer-gotchas.md`.)
    let raw_caps = raw_video_caps(cfg);
    let interpipesrc = gst::ElementFactory::make("interpipesrc")
        .name(format!("{initial_sink_name}-encsrc"))
        .property("listen-to", initial_sink_name)
        .property("is-live", true)
        .property("do-timestamp", false)
        .property_from_str("format", "time")
        .property("allow-renegotiation", false)
        .property("caps", &raw_caps)
        .build()
        .context("interpipesrc not found — is gst-interpipe installed in the image?")?;
    // A short leaky queue decouples the interpipe pull from the encoder thread and drops
    // stale frames across a switch rather than building latency.
    let queue = gst::ElementFactory::make("queue")
        .property("max-size-buffers", cfg.queue_buffers)
        .property("max-size-bytes", 0u32)
        .property("max-size-time", 0u64)
        .property_from_str("leaky", "downstream")
        .build()
        .context("queue not found")?;

    let encoder = build_encoder_element(cfg, codec, va_ctx, encoder_factory)?;
    // VA path: the GPU convert (system RGBx → memory:VAMemory NV12) must sit between the
    // leaky queue and the encoder in THIS pipeline, not the source. Here it is paced by
    // the encoder and shares the encoder's VADisplay through normal GstContext
    // propagation; a `vapostproc` in the decoupled source pipeline instead runs
    // 60fps-ahead and contends for that VADisplay, roughly halving throughput.
    //
    // This stage is also the external-resolution lever: its tail capsfilter is the one
    // size in the graph that may move at runtime (`ScaleStage::set_size`), which is why
    // the whole handle goes back to the runner.
    let scale = build_scale_stage(cfg, va_ctx)?;
    let gpu_convert = scale.elements.clone();

    let BitstreamChain {
        encoder_capsfilter: h264_capsfilter,
        parser,
        payloader: pay,
    } = build_bitstream_chain(codec, effective_profile)?;
    // TWCC must be declared HERE, in the capsfilter, never via `add-extension` on the
    // payloader. webrtcbin builds its offer from these caps, the payloader's
    // auto-header-extension instantiates the matching `rtphdrexttwcc` writer from the
    // negotiated caps, and webrtcbin's rtpsession learns the id the same way, so the
    // extmap id in the SDP and on the wire cannot diverge. Bolting the extension on
    // out-of-band (webrtcsink's approach) breaks here because this graph pins fully-fixed
    // caps between payloader and webrtcbin: the extension writes header bytes the offer
    // never declared and Chrome drops every packet. `rtcp-fb-transport-cc` puts
    // `a=rtcp-fb:96 transport-cc` in the offer; without it the browser never sends the
    // TWCC feedback rtpgccbwe consumes.
    //
    // abs-capture-time is always on, ABR or not: declaring it in the caps makes webrtcbin
    // emit `a=extmap:6 <URI>` (same mechanism as TWCC extmap-5), and the pad probe below
    // on `pay`'s src pad writes the per-packet NTP bytes.
    let mut rtp_caps_builder = gst::Caps::builder("application/x-rtp")
        .field("media", "video")
        .field("encoding-name", codec.rtp_encoding_name())
        .field("payload", 96_i32)
        .field("clock-rate", 90_000_i32)
        .field(
            format!("extmap-{ABS_CAPTURE_TIME_EXT_ID}"),
            ABS_CAPTURE_TIME_URI,
        );
    if cfg.abr_config().is_some() {
        rtp_caps_builder = rtp_caps_builder
            .field(format!("extmap-{TWCC_EXT_ID}"), RTP_TWCC_URI)
            .field("rtcp-fb-transport-cc", true);
    }
    let rtp_caps = rtp_caps_builder.build();
    let rtp_capsfilter = gst::ElementFactory::make("capsfilter")
        .property("caps", &rtp_caps)
        .build()
        .context("capsfilter not found")?;

    let webrtc = make_webrtcbin(cfg.stun.as_deref())?;

    pipeline.add_many([
        &interpipesrc,
        &queue,
        &encoder,
        &h264_capsfilter,
        &parser,
        &pay,
        &rtp_capsfilter,
        &webrtc,
    ])?;
    if !gpu_convert.is_empty() {
        pipeline.add_many(gpu_convert.iter())?;
    }
    // Link interpipesrc → queue → [GPU convert stage →] encoder → … → rtp.
    let mut chain: Vec<&gst::Element> = vec![&interpipesrc, &queue];
    chain.extend(gpu_convert.iter());
    chain.extend_from_slice(&[&encoder, &h264_capsfilter, &parser, &pay, &rtp_capsfilter]);
    gst::Element::link_many(chain).context("failed to link encode chain")?;

    // Time each frame across the encoder + count frames/bytes. Pad probes on existing
    // elements: no graph change, so webrtcbin never renegotiates.
    //
    // Leak-bisection diagnostic: `QUASAR_DIAG_NO_OBS` skips every agent-side probe/signal
    // attachment so a per-session element leak can be attributed to the observability
    // layer or exonerated. Not for production — metrics windows come out empty.
    let diag_no_obs = std::env::var("QUASAR_DIAG_NO_OBS")
        .map(|v| v == "1")
        .unwrap_or(false);
    if !diag_no_obs {
        attach_encode_probes(&encoder, session_metrics.clone(), cfg.latency_probe);
        attach_stage_probes(&queue, &pay, session_metrics.clone());
    }

    // G1 buffer-reuse gate: on the Vulkan-image path, verify the compositor's
    // ParentBufferMeta child survives interpipesink→interpipesrc→queue→encoder. Warns
    // once if a meta-stripping element reopened the gate; otherwise silent.
    if caps::vulkan_image_transport(cfg) {
        attach_vulkan_parent_meta_probe(&encoder);
    }

    // Diagnostic: localise a scrambled-RTP-TS defect to encoder input vs output PTS.
    // Knob: QUASAR_TRACE_ENC_PTS.
    if std::env::var("QUASAR_TRACE_ENC_PTS")
        .map(|v| v != "0" && !v.is_empty())
        .unwrap_or(false)
    {
        attach_encoder_pts_trace(&encoder);
    }

    // Write the abs-capture-time one-byte-header extension (8-byte NTP-64) per RTP
    // packet. Probing `pay`'s src pad means the buffer is already an RTP packet, so the
    // extension can be appended before the capsfilter.
    let capture_times = attach_abs_capture_time_probe(&pay);
    if !diag_no_obs {
        attach_abs_capture_time_egress_probe(
            &webrtc,
            capture_times,
            cfg.latency_probe.then(|| session_metrics.clone()),
        );
        attach_abs_capture_time_verification_probe(&rtp_capsfilter);
    }

    // Diagnostic: per-AU RTP marker-layout trace on the live wire. The synthetic
    // videotestsrc path cannot reproduce rtph264pay's marker logic, so a vulkan-vs-VA
    // marker-bit difference is only decidable on real content. Knob:
    // QUASAR_TRACE_RTP_MARKER.
    if std::env::var("QUASAR_TRACE_RTP_MARKER")
        .map(|v| v != "0" && !v.is_empty())
        .unwrap_or(false)
    {
        attach_rtp_marker_trace(&pay);
    }

    // Diagnostic: per-AU RTP timestamp-continuity + seq-contiguity trace, the two
    // surviving libwebrtc PacketBuffer frame-completion keys. Flags dup-TS-across-AU (the
    // every-other-frame merge suspect), non-1500 TS deltas, and seq gaps. Knob:
    // QUASAR_TRACE_RTP_TS.
    if std::env::var("QUASAR_TRACE_RTP_TS")
        .map(|v| v != "0" && !v.is_empty())
        .unwrap_or(false)
    {
        attach_rtp_ts_trace(&pay);
    }

    // Capture the raw encoded bitstream to a file, to decode offline (content vs
    // profile/colorimetry) or feed the harness's strict ffmpeg decode gate. Knob:
    // QUASAR_CAPTURE_BITSTREAM=<path>, with QUASAR_CAPTURE_H264 as a legacy alias;
    // BITSTREAM wins if both are set.
    let capture_path = std::env::var("QUASAR_CAPTURE_BITSTREAM")
        .ok()
        .or_else(|| std::env::var("QUASAR_CAPTURE_H264").ok());
    if let Some(path) = capture_path {
        if !path.is_empty() {
            // Probe the ENCODER src, not the parser src: the parser src showed 0 buffers
            // despite vaBeginPicture>0.
            attach_bitstream_capture(&encoder, &path);
        }
    }

    let src_pad = rtp_capsfilter
        .static_pad("src")
        .context("rtp capsfilter has no src pad")?;
    let sink_pad = webrtc
        .request_pad_simple("sink_%u")
        .context("webrtcbin refused a sink pad request (video)")?;
    src_pad
        .link(&sink_pad)
        .context("failed to link video into webrtcbin")?;
    enable_video_rtx(&webrtc);
    // ULPFEC/RED proactive burst-loss repair, no RTX round-trip. `fixed` negotiates
    // ulp-red at the static level; `auto` negotiates at 0% and the controller below ramps
    // on loss. Knobs: QUASAR_FEC_MODE / QUASAR_FEC_PERCENTAGE.
    let fec_plan = cfg.fec_plan();
    enable_video_fec(&webrtc, fec_plan.negotiate, fec_plan.initial_pct);
    if fec_plan.controller_enabled {
        // Reads the loss signal off the video webrtcbin get-stats on its own poll thread
        // and writes fec-percentage on each arm/disarm. Clone trace_tx + session_id: both
        // are moved into connect_state_logging further down.
        spawn_fec_controller(
            &webrtc,
            cfg.fec_controller_config(),
            Arc::downgrade(&session_metrics),
            trace_tx.clone(),
            session_id.clone(),
        );
    }
    // Arm the in-session ABR governor on the send path. Must be wired before the offer so
    // rtpgccbwe is part of the single negotiated send session. No-op only when rtpgccbwe
    // is absent or the encoder rejects live writes.
    //
    // The resolution lever is built here, not in the runner, because the ladder needs it
    // at arm time. One `Arc`, two owners: the runner's manual PATCH path and the ladder's
    // `on_window` closure.
    let resolution_lever = resolution_lever_with_echo(scale, encoder.clone(), &session_metrics);
    enable_abr(
        &webrtc,
        &encoder,
        cfg,
        session_metrics.clone(),
        trace_tx.clone(),
        session_id.clone(),
        Some(resolution_lever.clone()).filter(|l| l.stage().supported),
    );

    // Audio: pulsesrc from the session's PulseAudio sidecar (stable across swaps — the
    // new app connects to the same one). Skipped under QUASAR_AUDIO_DISABLED.
    //
    // #304: the audio webrtcbin MUST live in its own gst::Pipeline. Two webrtcbins in one
    // pipeline never reach PLAYING (the aggregate state transition stalls), and the split
    // also stops the browser's A/V sync from inflating video playout. The audio pipeline
    // is returned and set to PLAYING by runner.rs alongside the encode pipeline, sharing
    // its clock + base time.
    let stream_audio_enabled = cfg.video_topology != VideoTopology::DualOutput
        || cfg.console_config.as_ref().is_none_or(|c| c.stream_audio);
    let (audio_webrtc, audio_pipeline) = if !audio_disabled() && stream_audio_enabled {
        let audio_pipe = gst::Pipeline::new();
        let audio_tail = add_audio_chain(&audio_pipe, cfg)?;
        let audio_webrtc = make_webrtcbin(cfg.stun.as_deref())
            .context("failed to create audio webrtcbin (encode pipeline)")?;
        // #425: tune the mic receive-leg jitter buffer on the AUDIO PC only; the video
        // webrtcbin is never touched here.
        set_audio_receive_latency(&audio_webrtc, cfg.mic_jitter_ms);
        audio_pipe.add(&audio_webrtc)?;
        let audio_src_pad = audio_tail
            .static_pad("src")
            .context("audio rtp capsfilter has no src pad")?;
        let audio_sink_pad = audio_webrtc
            .request_pad_simple("sink_%u")
            .context("audio webrtcbin refused a sink pad request")?;
        audio_src_pad
            .link(&audio_sink_pad)
            .context("failed to link audio into audio webrtcbin")?;
        // Microphone capture (client → host): a second, recvonly Opus m-line on this same
        // audio PC, plus the decode bin that plays it into the sidecar's `quasar_mic`
        // sink. Position is load-bearing: after the send transceiver, so the sendonly
        // m-line stays m-line 0 and the wire is unchanged for mic-less sessions; and
        // before READY/negotiation, because the mic m-line must ride the FIRST and only
        // audio offer (`protocol/signaling.md` §Microphone m-line). Never renegotiated —
        // the single-offer guard is untouched and mute/unmute is client-local.
        if cfg.mic_enabled() {
            add_mic_branch(&audio_pipe, &audio_webrtc, cfg.pulse_server.as_deref())?;
        }
        // The audio webrtcbin gets its own negotiation/ICE/state callbacks (PcId::Audio).
        connect_ice_candidate(&audio_webrtc, out_tx.clone(), PcId::Audio);
        // Pass devices so release_all fires on audio-PC disconnect too.
        connect_state_logging(&audio_webrtc, devices.clone(), None, String::new());
        // There is one `offer created` per PC (video + audio), so a harness asserting
        // "exactly one offer" must count the VIDEO PC's line, not grep the session. Both
        // lines carry the session id.
        connect_negotiation_needed(
            &audio_webrtc,
            out_tx.clone(),
            PcId::Audio,
            session_id.clone(),
        );
        audio_pipe
            .set_state(gst::State::Ready)
            .context("audio pipeline failed to reach READY")?;
        (Some(audio_webrtc), Some(audio_pipe))
    } else {
        tracing::info!("stream audio disabled by runtime or session topology — video-only offer");
        if cfg.mic_enabled() {
            // The mic m-line rides the audio PC, so no audio PC means no microphone path.
            // Say so: a mic that silently does nothing is otherwise undiagnosable.
            tracing::warn!(
                token = "mic-no-audio-peerconnection",
                "microphone requested but this session has no audio PeerConnection \
                 (QUASAR_AUDIO_DISABLED or a local-only/stream-audio-off topology) — \
                 no mic m-line will be offered"
            );
        }
        (None, None)
    };

    connect_ice_candidate(&webrtc, out_tx.clone(), PcId::Video);
    // Clone the devices Arc so connect_state_logging can release_all() on disconnect
    // while the original is still available below for the DataChannel input_sink.
    connect_state_logging(&webrtc, devices.clone(), Some(trace_tx), session_id.clone());
    connect_negotiation_needed(&webrtc, out_tx, PcId::Video, session_id);

    pipeline
        .set_state(gst::State::Ready)
        .context("encode pipeline failed to reach READY")?;

    let (dc_init, input_channel_mode) = input_data_channel_init();
    let data_channel = webrtc
        .emit_by_name_with_values(
            "create-data-channel",
            &["input".to_value(), Some(dc_init).to_value()],
        )
        .and_then(|v| v.get::<glib::Object>().ok());
    let input_sink = match devices {
        Some(d) => InputSink::Virtual(d),
        None => InputSink::None,
    };
    match &data_channel {
        Some(dc) => {
            connect_data_channel_open(
                dc,
                input_sink,
                input_state,
                width,
                height,
                input_channel_mode,
            );
            tracing::info!(
                "created 'input' DataChannel ({input_channel_mode}, agent side, encode pipeline)"
            );
        }
        None => tracing::warn!(
            token = "datachannel-create-returned-nothing",
            "create-data-channel returned no object"
        ),
    }

    Ok(EncodePipeline {
        pipeline,
        webrtc,
        data_channel,
        interpipesrc,
        audio_webrtc,
        audio_pipeline,
        resolution_lever,
    })
}

// Console-mode local-display fan-out: a third interpipe listener → kmssink.
//
// The per-app source's interpipesink already fans out to multiple listeners (drop=true,
// sync=false). This adds a third, in its OWN gst::Pipeline (same reason as the #304 audio
// pipeline: a heavy consumer in the encode pipeline stalls its state transition), which
// downloads the interpipe's encoder-dependent memory to a scannable surface and scans it
// out to a locally-attached display while the session streams to the browser. Knob:
// QUASAR_LOCAL_DISPLAY. Agent-only; no control-plane/schema/protocol change.

/// A local-display fan-out pipeline: `interpipesrc(listen-to=sinkN) → queue →
/// [download → videoconvert] → {waylandsink|kmssink}`. Owns its own `gst::Pipeline`, torn
/// down on Drop (every session exit path).
pub struct LocalDisplay {
    pub pipeline: gst::Pipeline,
    /// The runner re-points `listen-to` on a swap.
    pub interpipesrc: gst::Element,
    sink_frames: Arc<AtomicU64>,
}

impl LocalDisplay {
    pub fn drain_sink_frames(&self) -> u64 {
        self.sink_frames.swap(0, Ordering::AcqRel)
    }
}

impl Drop for LocalDisplay {
    fn drop(&mut self) {
        let _ = self.pipeline.set_state(gst::State::Null);
    }
}

/// Build the [`LocalDisplay`] fan-out for `cfg`, listening to `initial_sink_name`.
///
/// The interpipesrc config must mirror the encode pipeline's exactly
/// (`do-timestamp=false`, `allow-renegotiation=false`, `caps=raw_video_caps`, `is-live`,
/// `format=time`): the runner shares one clock + base time across all pipelines, so the
/// source's PTS are already valid here and re-stamping breaks pacing (#68).
///
/// The download bridge is encoder-aware: the interpipe carries CUDAMemory / VulkanImage /
/// DMABuf / system memory depending on the encoder (`caps::raw_video_caps`,
/// `source_branch`), but the terminal sink only scans out system or dmabuf memory.
///
/// Terminal sink: `wayland_socket = Some(sock)` → `waylandsink` into a headless weston,
/// the nvidia-drm path (nvidia-drm exposes KMS only via the ATOMIC API, which kmssink
/// cannot drive but weston's drm-backend can, so weston owns DRM master). `None` →
/// `kmssink`, for amdgpu/legacy KMS where kmssink can take DRM master directly.
pub fn build_local_display_pipeline(
    cfg: &SessionConfig,
    initial_sink_name: &str,
    wayland_socket: Option<&str>,
) -> Result<LocalDisplay> {
    let pipeline = gst::Pipeline::new();

    let raw_caps = raw_video_caps(cfg);
    let interpipesrc = gst::ElementFactory::make("interpipesrc")
        .name(format!("{initial_sink_name}-dispsrc"))
        .property("listen-to", initial_sink_name)
        .property("is-live", true)
        .property("do-timestamp", false)
        .property_from_str("format", "time")
        .property("allow-renegotiation", false)
        .property("caps", &raw_caps)
        .build()
        .context("interpipesrc not found — is gst-interpipe installed in the image?")?;
    // Short leaky queue: drop stale frames across a swap rather than build latency.
    let queue = gst::ElementFactory::make("queue")
        .property("max-size-buffers", 3u32)
        .property("max-size-bytes", 0u32)
        .property("max-size-time", 0u64)
        .property_from_str("leaky", "downstream")
        .build()
        .context("queue not found")?;

    // Encoder-aware download bridge: bring the interpipe memory down to a scannable
    // layout. Arms are split per encoder (no unreachable pattern under either cargo
    // feature), mirroring `caps::raw_video_caps_for`.
    let make_el = |name: &'static str| -> Result<gst::Element> {
        gst::ElementFactory::make(name)
            .build()
            .with_context(|| format!("{name} not found — is the plugin in the image?"))
    };
    let mut bridge: Vec<gst::Element> = Vec::new();
    match cfg.encoder {
        // memory:CUDAMemory BGRA → download to system, then convert.
        #[cfg(feature = "cuda")]
        EncoderChoice::Nvenc => {
            bridge.push(make_el("cudadownload")?);
            bridge.push(make_el("videoconvert")?);
        }
        // No cuda feature: the interpipe carries system RGBx, so just convert.
        #[cfg(not(feature = "cuda"))]
        EncoderChoice::Nvenc => bridge.push(make_el("videoconvert")?),
        // memory:VulkanImage NV12 → download to system, then convert.
        EncoderChoice::Vulkan => {
            if caps::vulkan_image_transport(cfg) {
                bridge.push(make_el("vulkandownload")?);
            }
            if !caps::local_dmabuf_transport(cfg) {
                bridge.push(make_el("videoconvert")?);
            }
        }
        // VA: under ZC-03 dmabuf RGB, kmssink imports the dmabuf directly — videoconvert
        // cannot negotiate the DMABuf memory feature. Otherwise system RGBx → convert.
        EncoderChoice::Va => {
            if caps::dmabuf_zerocopy_format(cfg).is_none() {
                bridge.push(make_el("videoconvert")?);
            }
        }
        // openh264: system I420 → convert.
        EncoderChoice::Openh264 => bridge.push(make_el("videoconvert")?),
    }

    let terminal = if let Some(sock) = wayland_socket {
        // waylandsink reads XDG_RUNTIME_DIR from env (the dir weston used); `display` is
        // the socket name. weston owns DRM master.
        //
        // Never set `fullscreen` at construction: it races the sink's wl surface creation
        // (`gst_wl_window_ensure_fullscreen: assertion 'self' failed` — the GstWlWindow
        // does not exist until the sink realizes its surface on the first buffer).
        // Fullscreen is compositor-side instead: `console::spawn_weston_console` launches
        // weston with `--shell=kiosk-shell.so`, which fullscreens every toplevel on map.
        gst::ElementFactory::make("waylandsink")
            .name("local-display")
            .property("display", sock)
            .build()
            .context("waylandsink not found — is the wayland plugin in the image?")?
    } else {
        // force-modesetting=true takes DRM master to drive a headless-attached connector.
        // Pin both the amdgpu driver and the selected card-scoped connector when sysfs
        // exposes its DRM object id: a multi-GPU host must never silently scan out
        // through a different card/connector.
        let mut builder = gst::ElementFactory::make("kmssink")
            .name("local-display")
            .property("sync", true)
            .property("force-modesetting", true)
            .property("driver-name", "amdgpu");
        if let Some(connector_id) =
            selected_connector_id(cfg, std::path::Path::new("/sys/class/drm"))
        {
            builder = builder.property("connector-id", connector_id);
        }
        builder
            .build()
            .context("kmssink not found — is the kms plugin in the image (GPU-less build)?")?
    };

    let sink_frames = Arc::new(AtomicU64::new(0));
    let sink_frames_probe = sink_frames.clone();
    terminal
        .static_pad("sink")
        .context("local-display terminal has no sink pad")?
        .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
            sink_frames_probe.fetch_add(1, Ordering::Relaxed);
            gst::PadProbeReturn::Ok
        });

    let mut elems: Vec<gst::Element> = vec![interpipesrc.clone(), queue];
    elems.extend(bridge);
    elems.push(terminal);
    pipeline.add_many(elems.iter())?;
    gst::Element::link_many(elems.iter()).context("failed to link local-display chain")?;

    Ok(LocalDisplay {
        pipeline,
        interpipesrc,
        sink_frames,
    })
}

fn selected_connector_id(cfg: &SessionConfig, drm_root: &std::path::Path) -> Option<i32> {
    let output_id = cfg.console_config.as_ref()?.output_id.as_deref()?;
    let (card, connector) = output_id.split_once(':')?;
    std::fs::read_to_string(drm_root.join(format!("{card}-{connector}/connector_id")))
        .ok()?
        .trim()
        .parse()
        .ok()
}

// Local-audio output (`pulsesrc` → host ALSA device).
//
// Unlike `LocalDisplay` this does not tap the interpipe fan-out: it is a second,
// independent `pulsesrc` client of the session's PulseAudio sidecar (the same one
// `pipeline/audio_branch.rs` captures from; PulseAudio allows any number of monitor
// clients). So there is no `listen-to` to re-point on an app swap — the sidecar is
// session-scoped, not per-app, so this pipeline is built once and untouched until
// teardown.

/// A local-audio output pipeline: `pulsesrc → audioconvert → audioresample → alsasink`.
/// Owns its own `gst::Pipeline`; `Drop` sets it to `Null`, as [`LocalDisplay`] does.
pub struct LocalAudio {
    pub pipeline: gst::Pipeline,
}

impl Drop for LocalAudio {
    fn drop(&mut self) {
        let _ = self.pipeline.set_state(gst::State::Null);
    }
}

/// Build the [`LocalAudio`] leg for `cfg`, playing out `audio_output`.
///
/// `audio_output` of `"auto"` sets no `device` on `alsasink`, so the operator's
/// `/etc/asound.conf` / `default` PCM decides; any other string (`"hw:1,3"`) is set
/// verbatim.
///
/// The `pulsesrc` derivation mirrors `pipeline/audio_branch.rs`'s capture branch: `server`
/// from `cfg.pulse_server` when present, and `device` pinned to
/// [`QUASAR_MONITOR_SOURCE_NAME`](super::audio::QUASAR_MONITOR_SOURCE_NAME) on both paths.
/// The pin is required: once the sidecar started baking a microphone feed sink +
/// remap-source, following its default source stopped being safe. Low-latency by
/// construction — no RTP/jitter-buffer stage, this is a local device fan-out.
pub fn build_local_audio_pipeline(cfg: &SessionConfig, audio_output: &str) -> Result<LocalAudio> {
    let pipeline = gst::Pipeline::new();

    let monitor = super::audio::QUASAR_MONITOR_SOURCE_NAME;
    let mut src_builder = gst::ElementFactory::make("pulsesrc");
    if let Some(server) = &cfg.pulse_server {
        tracing::info!("local audio source: pulsesrc server={server} device={monitor}");
        src_builder = src_builder.property("server", server.as_str());
    } else {
        tracing::info!("local audio source: pulsesrc (default server) device={monitor}");
    }
    // Pin the capture device rather than following the daemon default: the sidecar also
    // carries a microphone feed sink + remap-source, and a moved default would route the
    // client's own mic to the console speakers.
    src_builder = src_builder.property("device", monitor);
    let pulse_src = src_builder.build().context(
        "pulsesrc not found (local audio) — is pulseaudio / gst-plugins-good installed?",
    )?;

    let audio_convert = gst::ElementFactory::make("audioconvert")
        .build()
        .context("audioconvert not found (local audio)")?;
    let audio_resample = gst::ElementFactory::make("audioresample")
        .build()
        .context("audioresample not found (local audio)")?;

    // Console-only playback pre-flight: an explicit `hw:*` sink opens that card, `auto`
    // enumerates the cards visible inside the agent container. Streamed audio uses a
    // separate WebRTC pipeline and never reaches here.
    match super::audio::enable_iec958_playback_switches(audio_output) {
        Ok(n) if n > 0 => {
            tracing::info!(
                "enabled {n} IEC958 playback switch(es) on {audio_output} for local audio"
            );
        }
        Ok(_) => {}
        Err(e) => {
            tracing::warn!(
                token = "audio-iec958-enable-failed-batch",
                "could not enable IEC958 playback switches for {audio_output}: {e:#}"
            );
        }
    }

    let mut sink_builder = gst::ElementFactory::make("alsasink").name("local-audio");
    if audio_output != "auto" {
        tracing::info!("local audio sink: alsasink device={audio_output}");
        sink_builder = sink_builder.property("device", audio_output);
    } else {
        tracing::info!("local audio sink: alsasink (default ALSA device)");
    }
    let alsa_sink = sink_builder
        .build()
        .context("alsasink not found — is the ALSA plugin (gst-plugins-good) in the image?")?;

    pipeline.add_many([&pulse_src, &audio_convert, &audio_resample, &alsa_sink])?;
    gst::Element::link_many([&pulse_src, &audio_convert, &audio_resample, &alsa_sink])
        .context("failed to link local-audio chain")?;

    Ok(LocalAudio { pipeline })
}

/// The transport-wide congestion control RTP header-extension URI Chrome negotiates.
/// Matches webrtcsink's `RTP_TWCC_URI`.
const RTP_TWCC_URI: &str =
    "http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01";

/// The extmap id TWCC is declared under. Any free one-byte id (1–14) works; webrtcbin
/// allocates its own extension ids (mid, rid) around ids already in caps.
const TWCC_EXT_ID: u32 = 5;

/// Absolute capture time RTP header-extension URI (Chrome implements it for end-to-end
/// delay estimation). A UQ32.32 NTP-64 timestamp per packet: the hardware-independent g2g
/// foundation that replaces the pixel-overlay instrument on zero-copy DMABuf paths.
const ABS_CAPTURE_TIME_URI: &str = "http://www.webrtc.org/experiments/rtp-hdrext/abs-capture-time";

/// Extmap id for abs-capture-time. Must be in the one-byte-header range 1–14 and distinct
/// from TWCC's 5. webrtcbin derives `a=extmap:6 <URI>` from the payloader caps.
const ABS_CAPTURE_TIME_EXT_ID: u32 = 6;

/// gst-interpipe is a third-party RidgeRun plugin built from source in the runtime/dev
/// images, absent from the `quasar-devtools` image `make test-rust` runs in. Tests needing
/// a real interpipe element gate on this and print a loud SKIP (which
/// `scripts/verify/agent.sh` restates) rather than failing. Keep it narrow: never widen
/// into a general "skip when any element is missing" — a test that quietly stops running
/// is the hidden-coverage-loss class CLAUDE.md calls out for the DB tests.
#[cfg(test)]
pub(crate) fn interpipe_available() -> bool {
    gstreamer::init().unwrap();
    let ok = gstreamer::ElementFactory::find("interpipesink").is_some()
        && gstreamer::ElementFactory::find("interpipesrc").is_some();
    if !ok {
        eprintln!(
            "SKIP — gst-interpipe is not installed in this image; the interpipe-backed \
             regression test did not run (it does run in the quasar-agent-dev container)"
        );
    }
    ok
}

#[cfg(test)]
mod source_interpipe_tests {
    use super::build_source_interpipe_sink;
    use gstreamer::prelude::*;

    /// The source interpipesink must NOT retain the last GstSample: on the Vulkan path a
    /// retained sample pins the compositor's ParentBufferMeta child (G1 gate) and so an
    /// encode-src ring slot, permanently taking one of the two under WOLF_VULKAN_RING=2.
    /// `drop=true` is also load-bearing: a not-yet-switched source must never block.
    #[test]
    fn source_sink_does_not_retain_last_sample() {
        if !super::interpipe_available() {
            return;
        }
        let sink = build_source_interpipe_sink("g1-retention-test").unwrap();
        assert!(
            !sink.property::<bool>("enable-last-sample"),
            "source interpipesink must disable last-sample retention (G1 gate, spec 2c)"
        );
        assert!(sink.property::<bool>("drop"));
    }

    use super::external_resize_supported;
    use crate::session::{EncoderChoice, SessionConfig};

    fn cfg_for(encoder: EncoderChoice, codec: crate::session::Codec) -> SessionConfig {
        let mut settings = crate::session::settings::RuntimeSettings::baseline();
        settings.encoder = encoder;
        let stream = crate::session::StreamParams {
            width: 1920,
            height: 1080,
            fps: 60,
            bitrate_kbps: 8000,
            h264_profile: "constrained-baseline".to_string(),
            codec,
            abr_floor_kbps: 0,
            mic: false,
        };
        SessionConfig::for_assignment_with(&settings, stream, None)
    }

    // The agent answers this synchronously, before any pipeline exists, and it must agree
    // with the `ScaleStage` the runner later builds. Vulkan h264 and h265 are `false` in
    // THIS environment for one reason only — no `vulkanscale` factory registered (#501);
    // on an image whose gst-wayland-display pin carries it they are `true`, and
    // `vulkan_resize_validated_for_codec` no longer holds any codec back. Vulkan × AV1 is
    // `true` regardless: it falls back per session to a vendor HW encoder that always has
    // a scaler.
    #[test]
    fn external_resize_supported_matches_the_scale_stage_matrix() {
        use crate::session::Codec;
        if super::scale_stage::vulkanscale_present() {
            eprintln!(
                "SKIP external_resize_supported_matches_the_scale_stage_matrix: this registry \
                 carries vulkanscale (#533) — the Vulkan x h264/h265 rows below assume it is \
                 absent; see scale_stage's paired \"_with_vulkanscale\" tests for that case"
            );
            return;
        }
        for (encoder, codec, want) in [
            (EncoderChoice::Openh264, Codec::H264, true),
            (EncoderChoice::Va, Codec::H264, true),
            (EncoderChoice::Nvenc, Codec::H264, true),
            (EncoderChoice::Nvenc, Codec::Av1, true),
            (EncoderChoice::Vulkan, Codec::H264, false),
            (EncoderChoice::Vulkan, Codec::H265, false),
            // Per-session vendor-HW fallback: no vulkanav1enc, so this session encodes on
            // NVENC/VA and can be resized live.
            (EncoderChoice::Vulkan, Codec::Av1, true),
        ] {
            assert_eq!(
                external_resize_supported(&cfg_for(encoder, codec)),
                want,
                "{encoder:?} x {codec:?}"
            );
        }
    }

    // No GStreamer registry, no gst::init: the agent calls this on the control-message
    // path, where a registry probe would panic (and did — three agent tests caught it).
    #[test]
    fn external_resize_supported_needs_no_gstreamer_init() {
        // Deliberately no gst::init() here.
        assert!(external_resize_supported(&cfg_for(
            EncoderChoice::Nvenc,
            crate::session::Codec::H264
        )));
    }
}
