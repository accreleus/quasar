//! Encoder selection and construction: software (openh264), VA (AMD/Intel), NVENC
//! (NVIDIA CUDA) and Vulkan Video, plus VA element-name resolution and the
//! single-frame VBV/CPB sizing shared across them. Transport (webrtcbin, SDP/ICE)
//! stays in `super`; live-resolution-change machinery is in `super::resize`.
//!
//! Resolution returns [`ResolvedEncoder`] (vendor choice + the exact factory name the
//! registry scan found), so the hardware builders instantiate that name rather than
//! re-scanning. Property sets go through [`GuardedProps`]. The three Vulkan per-codec
//! knobs are [`EncoderKnobs`], read from env only at a session's ambient edges
//! (`EncoderKnobs::from_env`) and threaded as data — the resolution functions never
//! read env themselves.

use std::sync::atomic::{AtomicBool, Ordering};

use anyhow::{anyhow, Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;

use crate::session::{Codec, EncoderChoice};

/// One Vulkan per-codec knob. Default ON: the env vars only ever DISABLE a codec
/// on the Vulkan encoder. `explicit` only names the source (`default`/`env`) in the
/// startup plan line; it never changes a resolution.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct CodecKnob {
    pub enabled: bool,
    pub explicit: bool,
}

impl Default for CodecKnob {
    fn default() -> Self {
        Self {
            enabled: true,
            explicit: false,
        }
    }
}

impl CodecKnob {
    /// Parse one knob's raw env value (case-insensitive, trimmed). An unrecognised
    /// value must warn and stay enabled — a typo must never silently drop a codec
    /// off a host.
    pub(crate) fn parse(var: &str, raw: Option<&str>) -> Self {
        let Some(v) = raw.map(str::trim) else {
            return Self::default();
        };
        match v.to_ascii_lowercase().as_str() {
            "" => Self::default(),
            "1" | "true" | "on" => Self {
                enabled: true,
                explicit: true,
            },
            "0" | "false" | "off" => Self {
                enabled: false,
                explicit: true,
            },
            _ => {
                tracing::warn!(
                    token = "knob-invalid-vulkan-codec",
                    "{var}='{v}' is not a recognised value (expected one of 1/true/on/0/false/off) \
                     — ignoring it and keeping the codec enabled on the Vulkan encoder"
                );
                Self::default()
            }
        }
    }

    /// `"env"` when an operator set this knob explicitly, else `"default"`.
    pub(crate) fn source(self) -> &'static str {
        if self.explicit {
            "env"
        } else {
            "default"
        }
    }
}

/// The three Vulkan per-codec knobs (`QUASAR_VULKAN_H264` / `QUASAR_VULKAN_HEVC` /
/// `QUASAR_VULKAN_AV1`), read from env once per ambient edge via
/// [`EncoderKnobs::from_env`] and threaded as data from there. The ambient edges:
/// `agent::probe_host_codecs`, `pipeline::resolve_effective_encoder` (once per
/// session), and `source_branch::pin_vulkan_encode_ring` (via
/// [`required_source_ring_depth`]).
///
/// All three default ON. Disabling one makes [`effective_encoder`] borrow the vendor
/// HW encoder (NVENC/VA) per session, keeping the codec advertised; with no vendor
/// element the codec drops off the host. H.264 can never drop — see
/// [`effective_encoder_with`].
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub(crate) struct EncoderKnobs {
    pub h264: CodecKnob,
    pub hevc: CodecKnob,
    pub av1: CodecKnob,
}

impl EncoderKnobs {
    /// Read all three knobs from the current process env.
    pub(crate) fn from_env() -> Self {
        fn knob(var: &str) -> CodecKnob {
            CodecKnob::parse(var, std::env::var(var).ok().as_deref())
        }
        Self {
            h264: knob("QUASAR_VULKAN_H264"),
            hevc: knob("QUASAR_VULKAN_HEVC"),
            av1: knob("QUASAR_VULKAN_AV1"),
        }
    }

    /// The knob governing `codec`.
    pub(crate) fn knob(self, codec: Codec) -> CodecKnob {
        match codec {
            Codec::H264 => self.h264,
            Codec::H265 => self.hevc,
            Codec::Av1 => self.av1,
        }
    }

    /// Whether the Vulkan encoder may produce `codec` on this host.
    pub(crate) fn allows(self, codec: Codec) -> bool {
        self.knob(codec).enabled
    }

    /// The env-var name of `codec`'s knob (for operator-facing log lines).
    pub(crate) fn var_name(codec: Codec) -> &'static str {
        match codec {
            Codec::H264 => "QUASAR_VULKAN_H264",
            Codec::H265 => "QUASAR_VULKAN_HEVC",
            Codec::Av1 => "QUASAR_VULKAN_AV1",
        }
    }
}

/// The GStreamer encoder-element factory-name suffix per codec, shared by the VA
/// (`va{dev}<suffix>enc` / `va<suffix>lpenc`) and NVENC (`nvcuda<suffix>enc`)
/// candidate builders.
fn codec_element_suffix(codec: Codec) -> &'static str {
    match codec {
        Codec::H264 => "h264",
        Codec::H265 => "h265",
        Codec::Av1 => "av1",
    }
}

/// The Vulkan Video encoder factory name a vulkan host is expected to have for
/// `codec`, independent of the knob or the registry — the missing-element warnings
/// use it to name what is absent.
pub(crate) fn vulkan_element(codec: Codec) -> &'static str {
    match codec {
        Codec::H264 => "vulkanh264enc",
        Codec::H265 => "vulkanh265enc",
        Codec::Av1 => "vulkanav1enc",
    }
}

/// Sustained encode throughput in **Mpix/s** for one encoder factory — the
/// `capacity.codec_throughput` hint of agent-api.md (#506). Keyed by ELEMENT (what
/// [`effective_encoder`] resolves to), so the value describes the element a session
/// actually builds, including the Vulkan→vendor fallback.
///
/// Why it exists: throughput is not uniform across one GPU. 1440p120 needs 442 and
/// 2160p60 needs 498, so an h265 session at either tier runs below tier SILENTLY —
/// the encoder back-pressures the compositor rather than dropping (a live
/// 2560x1440@120 h265 session delivered fps(recv)=96, zero drops, zero freezes).
///
/// Unmeasured elements must return `None`: a guessed number gates real launches, an
/// absent one gates nothing. Callers treat `None` as "unknown, do not gate", so a
/// future measured probe replaces this function without touching them.
///
/// The numbers are RTX-5090-class and the table is NOT GPU-aware (all measured on
/// one box, driver 610.57.04). Slower silicon over-states (under-rejects, today's
/// behaviour everywhere); faster silicon under-states and may reject a tier the host
/// could sustain. Revisit per GPU generation; never average two generations.
///
/// Keyed on the GENERIC factory name: `probe_codec_support` resolves with
/// `render_node = "software"`, yielding `vah265enc`, while a real session on
/// `/dev/dri/renderD129` builds `varenderD129h265enc`. Whoever adds the first VA
/// number must handle both spellings (match the codec suffix, or normalise the
/// device prefix away) or the hint silently vanishes on multi-GPU hosts. NVENC and
/// Vulkan names carry no device prefix.
pub(crate) fn element_pixel_rate_mpix_s(factory: &str) -> Option<f64> {
    match factory {
        // Vulkan Video, measured on an RTX 5090 (Mpix/s).
        "vulkanh264enc" => Some(1400.0),
        "vulkanh265enc" => Some(395.0),
        "vulkanav1enc" => Some(1215.0),
        // One NVENC HEVC engine under two factory names (gst 1.24→1.26 rename): the
        // upload path differs, the encode silicon does not, so they share the number.
        "nvh265enc" | "nvcudah265enc" => Some(790.0),
        // Every VA element, NVENC h264/av1, openh264: unmeasured. Unknown, not slow.
        _ => None,
    }
}

/// Whether [`make_openh264_encoder`] accepts `codec` — H.264 only. The one vendor
/// builder with its own codec allow-list (VA/NVENC/Vulkan trust the resolved factory
/// name); it must agree with [`encoder_candidates`]'s `Openh264` arm for every
/// [`Codec`], asserted by `matrix_tests`.
pub(super) fn openh264_supports(codec: Codec) -> bool {
    codec == Codec::H264
}

/// Build the software H.264 encoder (`openh264enc`). Low-latency posture
/// (constrained-baseline, no B-frames) — exactly what browsers decode.
/// `bitrate_kbps` is converted to openh264enc's native **bit/s** unit.
pub(super) fn make_openh264_encoder(
    codec: Codec,
    bitrate_kbps: u32,
    gop: u32,
) -> Result<gst::Element> {
    // Defence in depth: effective_encoder already rejects (Openh264, non-h264).
    if !openh264_supports(codec) {
        return Err(anyhow!(
            "openh264 (software) only encodes H.264, not {} — a non-h264 codec must \
             resolve to a hardware encoder (VA/NVENC/Vulkan)",
            codec.as_str()
        ));
    }
    // rate-control and complexity are GObject enum properties: must be set by name
    // (the Rust setter rejects an i32 and panics, unlike gst-launch).
    let bitrate_bps = bitrate_kbps.saturating_mul(1000);
    let enc = gst::ElementFactory::make("openh264enc")
        .name(super::VIDEO_ENCODER_NAME)
        .property("bitrate", bitrate_bps)
        .property_from_str("rate-control", "bitrate") // CBR
        .property_from_str("complexity", "low") // fastest
        .property("gop-size", gop)
        .build()
        .context("openh264enc not found — is libopenh264 installed?")?;
    tracing::info!(
        "software encoder: openh264enc ({bitrate_kbps} kbps / {bitrate_bps} bps, I420 in)"
    );
    Ok(enc)
}

/// Single-frame VBV/CPB cap in kilobits — one frame's share of the CBR budget.
/// The VA encoders' `cpb-size` is "max CPB size in Kb"; one frame (not the ~1 s
/// default) stops an IDR/complex-scene frame spiking 2-5x the per-frame budget and
/// bursting the bitstream into WebRTC congestion control (the #68 family). `fps=0`
/// ⇒ full bitrate (no div-by-zero); never returns 0, which the encoder reads as
/// "auto-calculate".
pub(super) fn cpb_size_kbits(bitrate_kbps: u32, fps: u32) -> u32 {
    if fps == 0 {
        return bitrate_kbps;
    }
    (bitrate_kbps / fps).max(1)
}

/// VA element-name prefix for a DRM render node, e.g. `/dev/dri/renderD129` →
/// `Some("varenderD129")`; `None` for a non-DRM value like `"software"`. The VA
/// plugin registers a device-specific element per GPU; selecting it pins encode to
/// the assigned card instead of silently using card 0 (cross-GPU contention / wrong
/// VRAM accounting, Wolf #298).
pub(super) fn va_device_element_prefix(render_node: &str) -> Option<String> {
    let base = render_node.rsplit('/').next().unwrap_or(render_node);
    base.starts_with("renderD").then(|| format!("va{base}"))
}

/// VA encoder factory names to try for `render_node`, most-specific first:
/// device-pinned LP then full, then the generic LP/full names. The VA plugin gives
/// the DEFAULT device generic names and only suffixes additional devices, so
/// `/dev/dri/renderD128` commonly has no `varenderD128*` factory. The generic
/// fallback stays fail-closed: the element either adopts the shared display context
/// or has its read-only `device-path` checked against `render_node`.
pub(super) fn va_encoder_candidates(codec: Codec, render_node: &str) -> Vec<String> {
    let sfx = codec_element_suffix(codec);
    let mut candidates = Vec::new();
    if let Some(prefix) = va_device_element_prefix(render_node) {
        candidates.push(format!("{prefix}{sfx}lpenc"));
        candidates.push(format!("{prefix}{sfx}enc"));
    }
    candidates.push(format!("va{sfx}lpenc"));
    candidates.push(format!("va{sfx}enc"));
    candidates
}

/// The H.264 candidate list, kept for that path's unit tests; production calls
/// [`va_encoder_candidates`] directly.
#[cfg(test)]
pub(super) fn va_h264_encoder_candidates(render_node: &str) -> Vec<String> {
    va_encoder_candidates(Codec::H264, render_node)
}

/// NVENC H.264 factory names, OLD name first. Order is load-bearing: gst 1.26
/// renamed the CUDA element `nvcudah264enc` → `nvh264enc`, but gst 1.24 also ships a
/// LEGACY `nvh264enc` with a different property surface, so new-name-first would
/// silently select the legacy element on 1.24 images. Kept for the H.264 unit tests;
/// production calls [`nvenc_encoder_candidates`].
#[cfg(test)]
pub(super) fn nvenc_h264_encoder_candidates() -> [&'static str; 2] {
    ["nvcudah264enc", "nvh264enc"]
}

/// NVENC candidates for `codec`, CUDA element first (ordering rationale:
/// [`nvenc_h264_encoder_candidates`]).
pub(super) fn nvenc_encoder_candidates(codec: Codec) -> [String; 2] {
    let sfx = codec_element_suffix(codec);
    [format!("nvcuda{sfx}enc"), format!("nv{sfx}enc")]
}

/// Encoder factory candidates for the (vendor `choice`, `codec`) pair,
/// most-specific first — the source of truth the encoder factories, the codec probe
/// and [`effective_encoder`] all consult. An EMPTY list means this vendor cannot
/// produce this codec (`Openh264 × H265`, or a `Vulkan × codec` whose knob is
/// disabled — that codec then resolves via the vendor-HW fallback in
/// [`effective_encoder`], not this list). `knobs` is data, not an env read.
///
/// Vendor×codec matrix: four sites must be edited together whenever a vendor or
/// codec is added — this function, [`openh264_supports`],
/// `pipeline::encoder_builder_kind`/`build_encoder_element`, and
/// `caps::caps_profile`/`caps::raw_video_caps_for`. `matrix_tests` asserts all four
/// agree for every `(EncoderChoice, Codec)` pair.
pub(crate) fn encoder_candidates(
    choice: EncoderChoice,
    codec: Codec,
    knobs: EncoderKnobs,
    render_node: &str,
) -> Vec<String> {
    match choice {
        EncoderChoice::Va => va_encoder_candidates(codec, render_node),
        EncoderChoice::Nvenc => nvenc_encoder_candidates(codec).to_vec(),
        EncoderChoice::Openh264 => match codec {
            Codec::H264 => vec!["openh264enc".to_string()],
            // Software HEVC/AV1 encoders are out of scope (§11).
            Codec::H265 | Codec::Av1 => Vec::new(),
        },
        EncoderChoice::Vulkan if !knobs.allows(codec) => {
            // Knob disabled: the vendor-HW fallback (and the H.264 floor) is
            // resolved in `effective_encoder_with`, not here.
            Vec::new()
        }
        EncoderChoice::Vulkan => vec![vulkan_element(codec).to_string()],
    }
}

/// True when at least one of `names` is registered in the (runtime-GPU) GStreamer
/// registry.
fn any_registered(names: &[String]) -> bool {
    names.iter().any(|n| gst::ElementFactory::find(n).is_some())
}

/// Keyframe interval in frames for a session at `fps`. The host `gop` setting is
/// defined at the 60 fps REFERENCE (gop=60 ⇒ one keyframe/second); scaling keeps the
/// cadence in TIME fps-invariant. Unscaled, 120 fps keyframes twice as often, and
/// AV1's whole-frame intra under tight CBR makes that a 7.6x bitrate burst at 2 Hz
/// (a recurring micro-glitch, measured on Redout @1440p120). `fps == 0` falls back
/// to the raw setting; floored at 1.
pub(crate) fn effective_gop(gop: u32, fps: u32) -> u32 {
    if fps == 0 {
        return gop.max(1);
    }
    ((gop as u64 * fps as u64) / 60).max(1) as u32
}

/// The first of `names`, in preference order, that `registered` accepts. Called
/// per-candidate (a one-element slice) so an "is ANY registered" predicate still
/// yields a specific factory name, which is what lets [`effective_encoder_with`]
/// report the exact element the registry scan found.
fn first_registered(names: &[String], registered: &impl Fn(&[String]) -> bool) -> Option<String> {
    names
        .iter()
        .find(|n| registered(std::slice::from_ref(n)))
        .cloned()
}

/// A session's (vendor `choice`, `codec`) pair resolved against the runtime
/// GStreamer registry: which vendor builds the pipeline, and the exact factory name
/// found registered. The builders instantiate `factory` rather than re-deriving
/// candidates and re-scanning.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct ResolvedEncoder {
    pub choice: EncoderChoice,
    pub factory: String,
}

/// Resolve the encoder that builds this session's pipeline for (vendor `choice`,
/// `codec`) against the runtime registry. `None` ⇒ unsupported on this host, a hard
/// launch error: the pipeline must never silently fall back to a different codec,
/// which would desync `sessions.codec`.
///
/// The one place a session may build as a DIFFERENT vendor than configured: a
/// `Vulkan × codec` whose Vulkan candidate is unavailable (knob disabled, or element
/// absent from an unpatched image) borrows the vendor HW encoder — NVENC if
/// registered, else VA, else unsupported. Every other pair resolves to itself iff a
/// candidate registers.
///
/// `render_node` selects the returned factory NAME: pass `"software"` for a
/// support-only question (the codec probe, `external_resize_supported`), and the
/// session's real render node when the result will build an element, so VA returns
/// the device-pinned name.
pub(crate) fn effective_encoder(
    choice: EncoderChoice,
    codec: Codec,
    knobs: EncoderKnobs,
    render_node: &str,
) -> Option<ResolvedEncoder> {
    effective_encoder_with(choice, codec, knobs, render_node, any_registered)
}

/// The registry-independent core of [`effective_encoder`], parameterized on the
/// "is any of these factories registered?" predicate so the resolution policy is
/// unit-testable without a GPU, a registry, or `gst::init`.
pub(crate) fn effective_encoder_with(
    choice: EncoderChoice,
    codec: Codec,
    knobs: EncoderKnobs,
    render_node: &str,
    registered: impl Fn(&[String]) -> bool,
) -> Option<ResolvedEncoder> {
    if choice == EncoderChoice::Vulkan {
        // 1. The Vulkan element itself: knob allows it and it is registered.
        if let Some(factory) = first_registered(
            &encoder_candidates(choice, codec, knobs, render_node),
            &registered,
        ) {
            return Some(ResolvedEncoder {
                choice: EncoderChoice::Vulkan,
                factory,
            });
        }
        // 2. Per-session vendor HW fallback: a disabled knob, or a Vulkan element
        //    missing from an unpatched image, borrows the vendor encoder instead of
        //    failing the session; the codec stays advertised.
        for vendor in [EncoderChoice::Nvenc, EncoderChoice::Va] {
            if let Some(factory) = first_registered(
                &encoder_candidates(vendor, codec, knobs, render_node),
                &registered,
            ) {
                return Some(ResolvedEncoder {
                    choice: vendor,
                    factory,
                });
            }
        }
        // 3. The H.264 floor: a host with no H.264 can serve no client, so
        //    disabling the knob on a host with no vendor H.264 encoder overrides
        //    the knob, loudly.
        if codec == Codec::H264 && !knobs.allows(codec) {
            if let Some(factory) = first_registered(
                &encoder_candidates(choice, codec, EncoderKnobs::default(), render_node),
                &registered,
            ) {
                tracing::error!(
                    token = "h264-floor-no-fallback",
                    "{}=0 disables H.264 on the Vulkan encoder, but this host has no vendor \
                     (NVENC/VA) H.264 encoder element to fall back to — keeping H.264 on \
                     vulkanh264enc anyway. A host with no H.264 cannot serve any client.",
                    EncoderKnobs::var_name(Codec::H264)
                );
                return Some(ResolvedEncoder {
                    choice: EncoderChoice::Vulkan,
                    factory,
                });
            }
        }
        return None;
    }
    let cands = encoder_candidates(choice, codec, knobs, render_node);
    first_registered(&cands, &registered).map(|factory| ResolvedEncoder { choice, factory })
}

/// The codecs the active encoder path can produce, probed from the registry at
/// startup — the hostcfg `codecs` report (agent-api.md §3.1.2). A codec is included
/// only when BOTH its encoder resolves (via [`effective_encoder`], so the vendor
/// fallback is reflected) AND its RTP payloader is registered: a missing `rtpav1pay`
/// simply drops AV1 from the set.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct CodecSupport {
    pub codecs: Vec<Codec>,
    /// The factory each codec resolves to, in `codecs` order. Carried rather than
    /// re-derived: [`element_pixel_rate_mpix_s`] is keyed on it, and the hint must
    /// describe the element a session would actually build (including the
    /// Vulkan→vendor fallback), not the nominal encoder choice.
    pub elements: Vec<String>,
}

impl CodecSupport {
    /// The wire-string form (`["h264", ...]`) for the hostcfg `codecs` report.
    pub fn codec_strings(&self) -> Vec<String> {
        self.codecs.iter().map(|c| c.as_str().to_string()).collect()
    }

    /// The `capacity.codec_throughput` hint (#506): wire codec → Mpix/s, for codecs
    /// whose resolved element has a MEASURED rate. An unmeasured codec is absent;
    /// the control plane reads absence as unknown and gates nothing.
    pub fn pixel_rates_mpix_s(&self) -> Vec<(String, f64)> {
        self.codecs
            .iter()
            .zip(self.elements.iter())
            .filter_map(|(codec, element)| {
                element_pixel_rate_mpix_s(element).map(|r| (codec.as_str().to_string(), r))
            })
            .collect()
    }
}

/// Probe the registry for the codecs `choice`'s encoder path can produce. Requires
/// `gst::init` to have run against the runtime GPU's registry — the agent forces a
/// fresh scan for hardware encoders (`session::init_gstreamer`), since a registry
/// baked without a GPU carries no VA/HW encoder factories.
pub fn probe_codec_support(choice: EncoderChoice, knobs: EncoderKnobs) -> CodecSupport {
    let (codecs, elements) = [Codec::H264, Codec::H265, Codec::Av1]
        .into_iter()
        .filter_map(|codec| {
            let resolved = effective_encoder(choice, codec, knobs, "software")?;
            gst::ElementFactory::find(codec.rtp_payloader())?;
            Some((codec, resolved.factory))
        })
        .unzip();
    CodecSupport { codecs, elements }
}

/// Where one codec ends up on a Vulkan-encoder host, for the startup plan line.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum CodecPlan {
    /// Produced by the Vulkan encoder element.
    Vulkan,
    /// The Vulkan element is unavailable (knob disabled, or element absent), so
    /// sessions borrow this vendor HW element instead. The codec stays advertised.
    Fallback(String),
    /// Knob disabled and no vendor element to borrow: the codec is dropped from the
    /// host's advertised set.
    Disabled,
    /// Knob enabled but nothing on this host can produce the codec (element absent
    /// from an unpatched image, or no payloader). Also dropped from the set.
    Unavailable,
}

impl std::fmt::Display for CodecPlan {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            CodecPlan::Vulkan => f.write_str("vulkan"),
            CodecPlan::Fallback(el) => write!(f, "fallback:{el}"),
            CodecPlan::Disabled => f.write_str("disabled"),
            CodecPlan::Unavailable => f.write_str("unavailable"),
        }
    }
}

/// The per-codec plan for a Vulkan-encoder host, parameterized on the registry
/// predicate so it is unit-testable without a GPU.
pub(crate) fn codec_plan_with(
    knobs: EncoderKnobs,
    render_node: &str,
    registered: impl Fn(&[String]) -> bool + Copy,
) -> Vec<(Codec, CodecPlan)> {
    [Codec::H264, Codec::H265, Codec::Av1]
        .into_iter()
        .map(|codec| {
            let plan = match effective_encoder_with(
                EncoderChoice::Vulkan,
                codec,
                knobs,
                render_node,
                registered,
            ) {
                Some(r) if r.choice == EncoderChoice::Vulkan => CodecPlan::Vulkan,
                Some(r) => CodecPlan::Fallback(r.factory),
                None if !knobs.allows(codec) => CodecPlan::Disabled,
                None => CodecPlan::Unavailable,
            };
            (codec, plan)
        })
        .collect()
}

/// True when a codec whose knob is ENABLED has no Vulkan element in the registry —
/// a mis-built image, so the caller logs at WARN. A codec the operator DISABLED is
/// never degraded, however it resolves.
pub(crate) fn plan_is_degraded(knobs: EncoderKnobs, plan: &[(Codec, CodecPlan)]) -> bool {
    plan.iter().any(|(codec, p)| {
        knobs.allows(*codec) && matches!(p, CodecPlan::Fallback(_) | CodecPlan::Unavailable)
    })
}

/// The operator-facing startup line for a Vulkan-encoder host, plus
/// [`plan_is_degraded`] — the caller logs at WARN rather than INFO when degraded.
pub struct CodecPlanReport {
    /// e.g. `h264=vulkan (default), h265=fallback:nvcudah265enc (env), av1=disabled (env)`
    pub line: String,
    pub degraded: bool,
}

/// What each codec resolves to on this Vulkan-encoder host and where its knob value
/// came from. Requires `gst::init` (it consults the registry).
pub fn describe_codec_plan(knobs: EncoderKnobs) -> CodecPlanReport {
    let plan = codec_plan_with(knobs, "software", any_registered);
    let degraded = plan_is_degraded(knobs, &plan);
    let line = plan
        .into_iter()
        .map(|(codec, plan)| format!("{}={plan} ({})", codec.as_str(), knobs.knob(codec).source()))
        .collect::<Vec<_>>()
        .join(", ");
    CodecPlanReport { line, degraded }
}

/// Guarded property-set/read helpers shared by the three hardware encoder builders.
/// Which properties an element exposes differs per instance (LP vs full VA;
/// nvcodec's `rc-mode` vs `rate-control`, `bframes` vs `b-frames` across gst
/// generations; a Vulkan build without the vendored slice/intra-refresh patches), so
/// every set is guarded on presence — an unknown property otherwise panics.
///
/// Always sets via `property_from_str`, never the typed setter: the typed setter
/// panics on any width/signedness mismatch (`expected 'gint', got 'guint'` killed
/// the first live Vulkan session), while the string setter coerces ints, bools and
/// enum nicks.
struct GuardedProps<'a> {
    element: &'a gst::Element,
    factory: &'a str,
}

impl<'a> GuardedProps<'a> {
    fn new(element: &'a gst::Element, factory: &'a str) -> Self {
        Self { element, factory }
    }

    /// Set `name` iff the element exposes it AND it is writable. DEBUG, not WARN:
    /// property sets differ across encoder generations by construction, so a miss is
    /// expected rather than degraded.
    fn set(&self, name: &str, value: &str) {
        match self.element.find_property(name) {
            Some(pspec) if pspec.flags().contains(glib::ParamFlags::WRITABLE) => {
                self.element.set_property_from_str(name, value);
            }
            Some(_) => tracing::debug!(
                token = "encoder-property-readonly",
                "{} '{name}' is read-only — skipping",
                self.factory
            ),
            None => tracing::debug!(
                token = "encoder-property-absent",
                "{} has no '{name}' property — skipping",
                self.factory
            ),
        }
    }

    /// Set the first of `names` the element exposes AND can write (NVENC's
    /// `rc-mode`/`rate-control`, `bframes`/`b-frames` generation aliases). A
    /// read-only first match stops the search rather than falling through to a
    /// later alias.
    fn set_first(&self, names: &[&str], value: &str) {
        for name in names {
            match self.element.find_property(name) {
                Some(pspec) if pspec.flags().contains(glib::ParamFlags::WRITABLE) => {
                    self.element.set_property_from_str(name, value);
                    return;
                }
                Some(_) => {
                    tracing::debug!(
                        token = "encoder-property-readonly-alias",
                        "{} '{name}' is read-only — skipping (set elsewhere)",
                        self.factory
                    );
                    return;
                }
                None => {}
            }
        }
        tracing::debug!(
            token = "encoder-property-no-alias",
            "{} has none of {names:?} — skipping",
            self.factory
        );
    }

    /// Serialized effective value of the first of `names` the element exposes
    /// (`"?"` on serialize failure, `"n/a"` when none exists) — the rate-control
    /// confirmation: a hardware encoder can silently fall back to constant-QP and
    /// ignore `bitrate` entirely (Wolf PR #368).
    fn readback(&self, names: &[&str]) -> String {
        names
            .iter()
            .find(|n| self.element.find_property(n).is_some())
            .map(|n| {
                self.element
                    .property_value(n)
                    .serialize()
                    .map(|s| s.to_string())
                    .unwrap_or_else(|_| "?".into())
            })
            .unwrap_or_else(|| "n/a".into())
    }
}

/// Build a VA hardware encoder for `codec` (`QUASAR_ENCODER=va`), instantiating the
/// `factory` [`effective_encoder`] already resolved (device-pinned when
/// `render_node` is a real DRM node). `bitrate_kbps` is in kbit/s, the VA encoders'
/// native unit. Low-latency posture: CBR, no B-frames, single reference. The
/// candidate list is recomputed only, lazily, for the not-found error message.
#[allow(clippy::too_many_arguments)]
pub(super) fn make_va_encoder(
    codec: Codec,
    bitrate_kbps: u32,
    fps: u32,
    gop: u32,
    num_slices: u32,
    target_usage: u32,
    render_node: &str,
    va_ctx: Option<&gst::Context>,
    factory: &str,
) -> Result<gst::Element> {
    let codec_str = codec.as_str();
    let enc = gst::ElementFactory::make(factory)
        .name(super::VIDEO_ENCODER_NAME)
        .build()
        .with_context(|| {
            let candidates = va_encoder_candidates(codec, render_node);
            format!(
                "no VA {codec_str} encoder found (tried {candidates:?}). Most likely \
                 the GPU is not in the container — pass `--device /dev/dri` to \
                 `docker run` — or this GPU's VCN cannot encode {codec_str} (e.g. \
                 Renoir has no AV1 encode). Verify inside the container: `vainfo` \
                 lists a matching encode entrypoint and `gst-inspect-1.0 {}` shows \
                 the element. On AMD this also needs mesa-va-drivers in the image.",
                candidates.last().map(String::as_str).unwrap_or("vah264enc")
            )
        })?;

    // A shared GstVaDisplay must be injected NOW, while the element is fresh: a
    // gst-va element creates its display lazily on first need (the `device-path`
    // read below, or NULL→READY), so a later set_context yields "Can't replace VA
    // display while operating" and a separate display, and the encoder re-imports
    // the compositor's NV12 surface instead of reusing it (crash).
    if let Some(ctx) = va_ctx {
        enc.set_context(ctx);
        tracing::info!("GW-03: injected shared VA display into {factory} before bind");
    }

    // Verify the encoder bound to the assigned card (Wolf #298): a mismatch means a
    // different GPU than the scheduler assigned, so fail rather than mis-account
    // VRAM. Skipped on the shared-display path — the injected context fixes the
    // device, and this read would force a display bind before adoption.
    if va_ctx.is_none()
        && enc.find_property("device-path").is_some()
        && render_node.starts_with("/dev/dri/")
    {
        let bound = enc.property::<String>("device-path");
        if bound == render_node {
            tracing::info!("VA encoder pinned to assigned render node {render_node} ({factory})");
        } else {
            return Err(anyhow!(
                "VA encoder {factory} bound to {bound}, not the scheduled render node {render_node}"
            ));
        }
    }

    let props = GuardedProps::new(&enc, factory);
    props.set("bitrate", &bitrate_kbps.to_string()); // kbit/s
    props.set("rate-control", "cbr");
    props.set("b-frames", "0"); // no reordering — latency + simple decode
    props.set("ref-frames", "1"); // single reference — lower encode complexity/latency
    props.set("key-int-max", &gop.to_string());
    props.set("target-usage", &target_usage.to_string()); // 1=quality … 7=speed/lowest-latency
    props.set("num-slices", &num_slices.to_string()); // N slices → a lost packet damages one strip
    props.set("min-qp", "20"); // quality floor — caps the IDR/scene-cut bit blow-up
    props.set("aud", "false"); // skip AU-delimiter NAL — a few bytes off every frame

    // Single-frame VBV/CPB cap (SO-02): smooth the bitstream into the WebRTC pacer.
    let cpb = cpb_size_kbits(bitrate_kbps, fps);
    props.set("cpb-size", &cpb.to_string());

    // Catch a silent fallback to constant-QP, which ignores `bitrate` (Wolf #368).
    let effective_rc = props.readback(&["rate-control"]);
    tracing::info!(
        "VA hardware encoder: {factory} (codec={codec_str}, {bitrate_kbps} kbps, \
         rate-control={effective_rc}, b-frames=0, ref-frames=1, target-usage={target_usage}, \
         num-slices={num_slices}, min-qp=20, aud=false, cpb-size={cpb} Kb (1-frame VBV), NV12 in)"
    );
    Ok(enc)
}

/// Build the NVENC hardware encoder (`QUASAR_ENCODER=nvenc`, ZC-02), instantiating
/// the `factory` resolution already found. Ingests `memory:CUDAMemory` NV12 from the
/// encode pipeline's `cudaconvert`. Blackwell killed the legacy preset GUIDs, so
/// this uses the modern p1–p7 preset + tune system (verified on the RTX 5090: p4 +
/// ultra-low-latency + cbr). `bitrate` is kbit/s, like the VA encoders.
pub(super) fn make_nvenc_encoder(
    codec: Codec,
    bitrate_kbps: u32,
    fps: u32,
    gop: u32,
    cuda_device_id: i32,
    factory: &str,
) -> Result<gst::Element> {
    let codec_str = codec.as_str();
    let enc = gst::ElementFactory::make(factory)
        .name(super::VIDEO_ENCODER_NAME)
        .build()
        .with_context(|| {
            let candidates = nvenc_encoder_candidates(codec);
            format!(
                "no NVENC {codec_str} encoder found (tried {candidates:?} — the \
                 nvcuda* CUDA element on gst >=1.26, plain nv* on <1.26). The \
                 GPU/driver must be in the container — run with `--runtime=nvidia \
                 --gpus all -e NVIDIA_DRIVER_CAPABILITIES=all`. The universal \
                 `quasar-node-agent` image (deploy/Dockerfile.vulkan) is \
                 CUDA-built for every lineage — there is no separate NVIDIA \
                 image any more (quasar-nv retired, #545). AV1 needs a \
                 gen-9-NVENC GPU (RTX 40/50). Verify: \
                 `gst-inspect-1.0 {}`.",
                candidates[0]
            )
        })?;

    // Guarded on WRITABILITY, not just existence: nvcudah264enc exposes some props
    // read-only (cuda-device-id, set by the input CUDA context) and
    // set_property_from_str on a read-only prop panics. Property names also differ
    // across nvcodec generations — on gst 1.28 the GstNvEncoder-based elements use
    // `rc-mode`/`bframes` where nvcudah264enc used `rate-control`/`b-frames` — so
    // the aliases are tried in order; a wrong single name silently leaves RC at
    // `default` (auto), which is what the read-back below catches.
    let props = GuardedProps::new(&enc, factory);
    props.set("preset", "p4"); // modern preset GUID (legacy ones are dead on Blackwell)
    props.set("tune", "ultra-low-latency"); // low-latency posture
    props.set_first(&["rc-mode", "rate-control"], "cbr");
    props.set("bitrate", &bitrate_kbps.to_string()); // kbit/s
    props.set("gop-size", &gop.to_string()); // keyframe interval
    props.set_first(&["bframes", "b-frames"], "0"); // no reordering — latency + simple decode
    props.set("aud", "false"); // skip AU-delimiter NAL — a few bytes off every frame

    // Single-frame VBV cap (SO-02): smooth the bitstream into the WebRTC pacer so an
    // IDR/scene-cut frame can't spike and collapse congestion control (#68 family).
    let vbv = cpb_size_kbits(bitrate_kbps, fps);
    props.set("vbv-buffer-size", &vbv.to_string()); // kbits

    // AS-07 content-adaptive quantisation. aq-strength only acts when
    // spatial-aq=true; rc-lookahead stays 0 because lookahead adds latency
    // (temporal-aq excluded for the same reason).
    props.set("spatial-aq", "true");
    props.set("aq-strength", "10");
    props.set("rc-lookahead", "0");

    // HEVC/AV1: repeat the parameter sets / sequence header on every IDR so a
    // late-joining receiver decodes without waiting for the next GOP (the analogue
    // of rtph26xpay's `config-interval=-1`). H.264 keeps its default.
    if codec != Codec::H264 {
        props.set("repeat-sequence-header", "true");
    }

    // SO-07 device audit, the CUDA analog of the VA device-path read-back.
    // cuda-device-id is read-only: the device comes from the INPUT CUDA context, so
    // multi-GPU pinning rides on that context. With `--features cuda` the runner
    // injects one app-owned GstCudaContext on cfg.cuda_device_id and this confirms
    // the encoder adopted it; without it, the device is whatever cudaupload defaulted
    // to (0), and real pinning there is a future item.
    if enc.find_property("cuda-device-id").is_some() {
        let bound = enc
            .property_value("cuda-device-id")
            .serialize()
            .map(|s| s.to_string())
            .unwrap_or_else(|_| "?".into());
        if bound == cuda_device_id.to_string() {
            tracing::info!(
                "NVENC effective cuda-device-id={bound} (matches requested {cuda_device_id}); {factory}"
            );
        } else {
            tracing::warn!(
                token = "encoder-cuda-device-mismatch",
                "NVENC effective cuda-device-id={bound} but session requested {cuda_device_id} — \
                 the encoder bound a different GPU than assigned (the shared CUDA context was not \
                 adopted, or cudaupload defaulted elsewhere); possible cross-GPU contention / wrong \
                 VRAM accounting on a multi-GPU host (the CUDA analog of Wolf #298); {factory}"
            );
        }
    }
    // The gst 1.28 nv*enc default is `default` (auto RC), so a silently-dropped
    // `cbr` set would lose explicit bitrate control and re-open the #68 congestion
    // collapse. Same alias order as the set.
    let effective_rc = props.readback(&["rc-mode", "rate-control"]);
    tracing::info!(
        "NVENC hardware encoder: {factory} (codec={codec_str}, {bitrate_kbps} kbps, preset=p4, \
         tune=ultra-low-latency, rate-control={effective_rc}, b-frames=0, gop-size={gop}, aud=false, \
         vbv-buffer-size={vbv} kbits (1-frame), spatial-aq=true, aq-strength=10, rc-lookahead=0, \
         repeat-sequence-header={}, cuda-device-id={cuda_device_id}, CUDAMemory NV12 in)",
        codec != Codec::H264
    );
    Ok(enc)
}

/// Fixed element name so the runner's post-PLAYING rearm (`rearm_vulkan_rc`) finds
/// the encoder via `pipeline.by_name(..)` instead of GStreamer's auto-numbered
/// default, which would break silently on a second instantiation.
pub(crate) const VULKAN_ENCODER_NAME: &str = "quasar-vulkan-encoder";

/// Build the Vulkan Video hardware encoder (`QUASAR_ENCODER=vulkan`, VK-05),
/// instantiating the `factory` [`effective_encoder`] resolved. A codec only reaches
/// here when its knob is enabled AND its vulkan element is registered; either miss
/// resolves to the vendor-HW fallback.
///
/// Ingests `memory:VulkanImage` NV12 directly from the interpipe. The source selects
/// its physical device by the assigned DRM render node and publishes that
/// GstVulkanDevice upstream; the patched interpipe forwards the context query and
/// this encoder adopts it, so multi-GPU routing is tied to render-node identity
/// rather than assuming a Vulkan ordinal equals a scheduler index. `bitrate_kbps` is
/// kbit/s. Low-latency posture: CBR, no B-frames, single reference, quality=0
/// (Wolf #450), AUD off.
#[allow(clippy::too_many_arguments)]
pub(super) fn make_vulkan_encoder(
    codec: Codec,
    bitrate_kbps: u32,
    _fps: u32,
    gop: u32,
    num_slices: u32,
    intra_refresh: bool,
    intra_refresh_period: u32,
    factory: &str,
) -> Result<gst::Element> {
    let codec_str = codec.as_str();
    let enc = gst::ElementFactory::make(factory)
        .name(VULKAN_ENCODER_NAME)
        .build()
        .with_context(|| {
            format!(
                "{factory} not found — the GPU/driver must be in the container and support \
                 Vulkan Video {codec_str} encode (Mesa freeworld / RADV on AMD; H.265 needs \
                 QUASAR_VULKAN_HEVC=1, AV1 needs QUASAR_VULKAN_AV1=1 and both are experimental \
                 opt-in paths). Verify inside the container: `gst-inspect-1.0 {factory}`."
            )
        })?;

    let props = GuardedProps::new(&enc, factory);

    // On gst 1.28.4 a rate-control set before PLAYING is silently lost (falls back
    // to CQP), so this is best-effort; the authoritative set is `rearm_vulkan_rc`
    // post-PLAYING.
    props.set("rate-control", "cbr");
    props.set("bitrate", &bitrate_kbps.to_string()); // kbit/s, like the VA encoders
    props.set("quality", "0"); // low-latency posture (Wolf #450 preset)
                               // Single reference: also a hard dependency of intra-refresh (#227 A1) — the
                               // driver's maxIntraRefreshActiveReferencePictures is 1 on both validated GPUs,
                               // so this must be 1 whenever intra-refresh is on.
    props.set("num-ref-frames", "1");
    props.set("b-frames", "0"); // no reordering — latency + simple decode
    props.set("aud", "false"); // skip AU-delimiter NAL
    props.set("idr-period", &gop.to_string()); // keyframe interval, frames
                                               // N slices ⇒ a lost packet damages one strip, not the frame. Needs the
                                               // vendored `vkh264enc-num-slices.patch` (no `num-slices` on stock gst
                                               // 1.28.4); the guard warns instead of panicking on an unpatched build.
                                               // The element clamps to MB rows + driver maxSliceCount.
    props.set("num-slices", &num_slices.to_string());

    // #227 A2: rolling intra refresh, knob QUASAR_INTRA_REFRESH, default off (which
    // keeps output byte-identical). Needs the vendored `vkh264enc-intra-refresh.patch`;
    // the guarded set warns rather than panicking on an unpatched build. The element
    // negotiates per-picture-partition vs block-row mode against driver caps itself.
    // H.264-only: the patch targets vkh264enc and upstream exposes no H.265
    // equivalent, so warn and ignore the knob rather than set an absent property.
    if intra_refresh && codec != Codec::H264 {
        tracing::warn!(
            token = "knob-intra-refresh-unsupported-codec",
            "QUASAR_INTRA_REFRESH is set but intra-refresh is unsupported for codec={codec_str} \
             (H.264-only); ignoring the knob"
        );
    }
    if intra_refresh && codec == Codec::H264 {
        props.set("intra-refresh", "true");
        props.set("intra-refresh-period", &intra_refresh_period.to_string());
        // Per-picture-partition mode's refresh cycle equals num-slices, so
        // num-slices=1 degenerates to all-intra every frame on the NVIDIA-preferred
        // mode. AMD's block-row fallback is unaffected, but there is no vendor
        // signal here, so warn unconditionally.
        if num_slices <= 1 {
            tracing::warn!(
                token = "knob-intra-refresh-degenerate",
                "QUASAR_INTRA_REFRESH=1 with num-slices=1: per-picture-partition mode's \
                 refresh cycle equals num-slices, so this degenerates to all-intra every \
                 frame on the NVIDIA-preferred mode (no bandwidth benefit). Raise \
                 QUASAR_SLICES for a real rolling refresh cycle; AMD's block-row fallback \
                 is unaffected."
            );
        }
    }

    let effective_rc = props.readback(&["rate-control"]);
    // Derived from the element's actual property presence, not the requested flag:
    // on an unpatched build the guarded set silently no-ops, and reporting `true`
    // would claim intra refresh is live when it is not. Live verification greps the
    // `intra-refresh=true(period=N)` form, so keep that format.
    let intra_refresh_summary = match (intra_refresh, enc.find_property("intra-refresh").is_some())
    {
        (true, true) => format!("intra-refresh=true(period={intra_refresh_period})"),
        (true, false) => "intra-refresh=requested-unsupported".to_string(),
        (false, _) => "intra-refresh=false".to_string(),
    };
    tracing::info!(
        "Vulkan hardware encoder: {factory} (codec={codec_str}, {bitrate_kbps} kbps, \
         rate-control={effective_rc}, quality=0, num-ref-frames=1, b-frames=0, aud=false, \
         idr-period={gop}, num-slices={num_slices}, {intra_refresh_summary}, VulkanImage NV12 in)"
    );
    Ok(enc)
}

/// Whether `enc`'s `name` property is live-writable (`MUTABLE_PLAYING`). Callers
/// needing a live write must gate on this, not mere presence.
///
/// The flag proves the element accepts the write, NOT that the driver acts on it:
/// `vulkanh265enc` once set the flag while never issuing `vkCmdControlVideoCoding`,
/// so live ABR was silently inert. That half is covered by
/// `gst_vulkan_encoder_request_rc_update()` (`vkh264enc-rc-fix.patch`) and verified
/// by measurement, never by this flag.
pub(crate) fn is_mutable_playing(enc: &gst::Element, name: &str) -> bool {
    enc.find_property(name)
        .map(|p| p.flags().contains(gst::PARAM_FLAG_MUTABLE_PLAYING))
        .unwrap_or(false)
}

/// VK-05: re-arm `rate-control`/`bitrate` on a live Vulkan encoder once the encode
/// pipeline reaches PLAYING, working around a gst 1.28.4 bug where a pre-PLAYING
/// `rate-control` set is silently dropped to CQP.
///
/// Both vendored Vulkan elements mark these `MUTABLE_PLAYING` and re-apply a live
/// change via `gst_vulkan_encoder_request_rc_update()` — the reset-free
/// `vkCmdControlVideoCoding` that leaves the DPB intact, so a retarget forces no IDR
/// (H.264) and is no longer inert (H.265). Measurements:
/// `docs/design/plans/2026-07-26-vulkan-abr-retarget-defects-spec.md`,
/// `docs/reports/VULKAN-WORKLOG.md` G2.
pub(crate) fn rearm_vulkan_rc(enc: &gst::Element, bitrate_kbps: u32) {
    let factory = enc
        .factory()
        .map(|f| f.name().to_string())
        .unwrap_or_else(|| "vulkan-encoder".into());
    // Guards an UNPATCHED GStreamer: stock 1.28.4 marks neither property
    // MUTABLE_PLAYING, so this rearm and every ABR retarget would be a no-op. It
    // cannot detect a driver-side inert retarget; that is what the spec's §6 live
    // keyframe/bitrate checks are for.
    if !is_mutable_playing(enc, "bitrate") || !is_mutable_playing(enc, "rate-control") {
        static VK_RC_NOT_LIVE: AtomicBool = AtomicBool::new(false);
        if !VK_RC_NOT_LIVE.swap(true, Ordering::Relaxed) {
            tracing::warn!(
                token = "encoder-rc-not-mutable-playing",
                "VK-05: {factory} rate-control/bitrate are not MUTABLE_PLAYING — this image's \
                 GStreamer is missing the vendored vkh264enc-rc-fix patch. Post-PLAYING rearm \
                 and live ABR retargets will be inert; the stream runs at its build-time CBR set"
            );
        }
    }
    if enc.find_property("rate-control").is_some() {
        enc.set_property_from_str("rate-control", "cbr");
    }
    if enc.find_property("bitrate").is_some() {
        // property_from_str, not the typed setter: same signedness-panic guard.
        enc.set_property_from_str("bitrate", &bitrate_kbps.to_string());
    }
    tracing::info!(
        "VK-05: rearmed {factory} rate-control=cbr, bitrate={bitrate_kbps} kbps post-PLAYING \
         (gst 1.28.4 pre-PLAYING rate-control loss workaround, see VULKAN-WORKLOG.md G2)"
    );
}

/// The Vulkan compositor encode-src ring depth `knobs` requires, `None` if the
/// default ring is fine — the WHY half of `source_branch::pin_vulkan_encode_ring`,
/// which only applies it.
///
/// `Some(2)` iff HEVC is enabled on the Vulkan encoder. The 5090's Vulkan H.265
/// encode-src pool gives one of the default `RING=4` slots an incompatible
/// tiling/swizzle, so the RGBA→NV12 compute shader writes it black — exactly 1-in-4
/// encoded frames (`docs/design/plans/2026-07-24-vulkanh265enc-conformance-resolution-spec.md`
/// §7 item 4). `RING=2` eliminates it (0 black frames measured); not `RING=1`, see
/// `source_branch::pin_vulkan_encode_ring` for the starvation finding.
///
/// AV1 must NOT arm the pin on its own: proven live on the RTX 5090 with zero black
/// frames at the stock `RING=4` once the input-buffer-release fix removed the
/// retention pressure the H.265 defect depends on.
pub(super) fn required_source_ring_depth(knobs: EncoderKnobs) -> Option<u32> {
    if knobs.allows(Codec::H265) {
        Some(2)
    } else {
        None
    }
}

// ZC-02 topology, per build:
// - `--features cuda`: full zero-copy. The compositor emits memory:CUDAMemory BGRA
//   across the interpipe; cudaconvert → CUDAMemory NV12 → nvcuda*enc, all sharing one
//   app-owned GstCudaContext the runner injects into both pipelines
//   (session::cuda_share). Capturing the compositor's context off the bus does not
//   work — it never posts one.
// - without it: the interpipe carries system RGBx, cudaupload creates the context and
//   the downstream elements inherit it in-pipe. Costs the compositor→encode system hop.

#[cfg(test)]
mod tests {
    use super::{
        codec_plan_with, cpb_size_kbits, effective_encoder_with, effective_gop, encoder_candidates,
        nvenc_encoder_candidates, nvenc_h264_encoder_candidates, openh264_supports,
        plan_is_degraded, required_source_ring_depth, va_device_element_prefix,
        va_encoder_candidates, va_h264_encoder_candidates, vulkan_element, CodecKnob, CodecPlan,
        EncoderKnobs, ResolvedEncoder,
    };
    use crate::session::{Codec, EncoderChoice};

    /// Knobs with exactly `codec` disabled (`QUASAR_VULKAN_*=0`), the rest default.
    fn disabled(codec: Codec) -> EncoderKnobs {
        let off = CodecKnob {
            enabled: false,
            explicit: true,
        };
        let mut k = EncoderKnobs::default();
        match codec {
            Codec::H264 => k.h264 = off,
            Codec::H265 => k.hevc = off,
            Codec::Av1 => k.av1 = off,
        }
        k
    }

    // ---- multi-codec candidate matrix (encoder_candidates / per-vendor helpers) ----

    #[test]
    fn va_encoder_candidates_are_codec_aware() {
        assert_eq!(
            va_encoder_candidates(Codec::H265, "/dev/dri/renderD129"),
            [
                "varenderD129h265lpenc",
                "varenderD129h265enc",
                "vah265lpenc",
                "vah265enc"
            ]
        );
        assert_eq!(
            va_encoder_candidates(Codec::Av1, "software"),
            ["vaav1lpenc", "vaav1enc"]
        );
        assert_eq!(
            va_encoder_candidates(Codec::H264, "/dev/dri/renderD129"),
            va_h264_encoder_candidates("/dev/dri/renderD129")
        );
    }

    #[test]
    fn nvenc_encoder_candidates_are_codec_aware() {
        assert_eq!(
            nvenc_encoder_candidates(Codec::H265),
            ["nvcudah265enc", "nvh265enc"]
        );
        assert_eq!(
            nvenc_encoder_candidates(Codec::Av1),
            ["nvcudaav1enc", "nvav1enc"]
        );
        assert_eq!(
            nvenc_encoder_candidates(Codec::H264).to_vec(),
            nvenc_h264_encoder_candidates().to_vec()
        );
    }

    // openh264 is H.264-only; the knobs are irrelevant to this vendor.
    #[test]
    fn openh264_candidates_h264_only() {
        let knobs = EncoderKnobs::default();
        assert_eq!(
            encoder_candidates(EncoderChoice::Openh264, Codec::H264, knobs, "software"),
            ["openh264enc"]
        );
        assert!(
            encoder_candidates(EncoderChoice::Openh264, Codec::H265, knobs, "software").is_empty()
        );
        assert!(
            encoder_candidates(EncoderChoice::Openh264, Codec::Av1, knobs, "software").is_empty()
        );
    }

    // Default knobs list all three vulkan elements. `encoder_candidates` takes
    // `EncoderKnobs` as data, so this needs no env.
    #[test]
    fn vulkan_candidates_default_to_all_three_codecs() {
        let knobs = EncoderKnobs::default();
        assert_eq!(
            encoder_candidates(EncoderChoice::Vulkan, Codec::H264, knobs, "software"),
            ["vulkanh264enc"]
        );
        assert_eq!(
            encoder_candidates(EncoderChoice::Vulkan, Codec::H265, knobs, "software"),
            ["vulkanh265enc"]
        );
        assert_eq!(
            encoder_candidates(EncoderChoice::Vulkan, Codec::Av1, knobs, "software"),
            ["vulkanav1enc"]
        );
    }

    #[test]
    fn vulkan_candidates_empty_only_for_the_disabled_codec() {
        for codec in [Codec::H264, Codec::H265, Codec::Av1] {
            let knobs = disabled(codec);
            assert!(
                encoder_candidates(EncoderChoice::Vulkan, codec, knobs, "software").is_empty(),
                "{} disabled ⇒ no vulkan candidate",
                codec.as_str()
            );
            for other in [Codec::H264, Codec::H265, Codec::Av1] {
                if other != codec {
                    assert!(
                        !encoder_candidates(EncoderChoice::Vulkan, other, knobs, "software")
                            .is_empty(),
                        "disabling {} must not affect {}",
                        codec.as_str(),
                        other.as_str()
                    );
                }
            }
        }
    }

    // Vulkan-default NVIDIA host: the NVIDIA compose overlay defaults
    // QUASAR_ENCODER=vulkan, so the vendor fallback is the ONLY way an AV1 session
    // encodes there. A fake registry keeps these GPU-free.

    /// A fake "registered" predicate over an explicit element-name allowlist.
    fn registry(available: &'static [&'static str]) -> impl Fn(&[String]) -> bool {
        move |cands: &[String]| cands.iter().any(|c| available.contains(&c.as_str()))
    }

    fn resolved(choice: EncoderChoice, factory: &str) -> Option<ResolvedEncoder> {
        Some(ResolvedEncoder {
            choice,
            factory: factory.to_string(),
        })
    }

    #[test]
    fn vulkan_host_h264_stays_vulkan() {
        let nvidia_box = registry(&["vulkanh264enc", "nvcudah264enc", "nvcudaav1enc"]);
        assert_eq!(
            effective_encoder_with(
                EncoderChoice::Vulkan,
                Codec::H264,
                EncoderKnobs::default(),
                "software",
                &nvidia_box
            ),
            resolved(EncoderChoice::Vulkan, "vulkanh264enc"),
            "h264 on a vulkan host must build vulkanh264enc, never fall back"
        );
    }

    // An image without vulkanav1enc (a pin lacking the vendored patch) still
    // resolves av1, via the vendor-HW fallback, even with the knob on.
    #[test]
    fn vulkan_host_av1_falls_back_to_nvenc_when_element_absent() {
        let nvidia_box = registry(&["vulkanh264enc", "nvcudah264enc", "nvcudaav1enc"]);
        assert_eq!(
            effective_encoder_with(
                EncoderChoice::Vulkan,
                Codec::Av1,
                EncoderKnobs::default(),
                "software",
                &nvidia_box
            ),
            resolved(EncoderChoice::Nvenc, "nvcudaav1enc"),
            "av1 on a vulkan NVIDIA host with no vulkanav1enc in the registry must \
             fall back to the NVENC AV1 encoder"
        );
    }

    #[test]
    fn vulkan_host_av1_stays_vulkan_by_default_when_element_registered() {
        let nvidia_box = registry(&[
            "vulkanh264enc",
            "vulkanav1enc",
            "nvcudah264enc",
            "nvcudaav1enc",
        ]);
        assert_eq!(
            effective_encoder_with(
                EncoderChoice::Vulkan,
                Codec::Av1,
                EncoderKnobs::default(),
                "software",
                &nvidia_box
            ),
            resolved(EncoderChoice::Vulkan, "vulkanav1enc"),
            "av1 defaults to the vulkan encoder when vulkanav1enc is registered"
        );
    }

    // A disabled knob takes the vendor-HW fallback for every codec, so the codec
    // stays resolvable and stays advertised.
    #[test]
    fn disabled_knob_falls_back_to_vendor_hw_for_every_codec() {
        let nvidia_box = registry(&[
            "vulkanh264enc",
            "vulkanh265enc",
            "vulkanav1enc",
            "nvcudah264enc",
            "nvcudah265enc",
            "nvcudaav1enc",
        ]);
        for (codec, factory) in [
            (Codec::H264, "nvcudah264enc"),
            (Codec::H265, "nvcudah265enc"),
            (Codec::Av1, "nvcudaav1enc"),
        ] {
            assert_eq!(
                effective_encoder_with(
                    EncoderChoice::Vulkan,
                    codec,
                    disabled(codec),
                    "software",
                    &nvidia_box
                ),
                resolved(EncoderChoice::Nvenc, factory),
                "{}=0 must borrow the vendor HW encoder, not drop the codec",
                EncoderKnobs::var_name(codec)
            );
        }
    }

    // A disabled knob with NO vendor element drops the codec (hevc/av1) — but never
    // h264: the floor guarantee keeps h264 on vulkanh264enc regardless.
    #[test]
    fn disabled_knob_without_vendor_element_drops_codec_except_h264() {
        let vulkan_only = registry(&["vulkanh264enc", "vulkanh265enc", "vulkanav1enc"]);
        for codec in [Codec::H265, Codec::Av1] {
            assert_eq!(
                effective_encoder_with(
                    EncoderChoice::Vulkan,
                    codec,
                    disabled(codec),
                    "software",
                    &vulkan_only
                ),
                None,
                "{} disabled with no vendor element ⇒ dropped from the host set",
                codec.as_str()
            );
        }
        assert_eq!(
            effective_encoder_with(
                EncoderChoice::Vulkan,
                Codec::H264,
                disabled(Codec::H264),
                "software",
                &vulkan_only
            ),
            resolved(EncoderChoice::Vulkan, "vulkanh264enc"),
            "h264 is the guaranteed floor: disabling it on a host with no vendor h264 \
             encoder must keep vulkanh264enc, never produce a host with no h264"
        );
    }

    // The plan line's three states on one host: vulkan, vendor fallback, dropped.
    #[test]
    fn codec_plan_reports_vulkan_fallback_and_disabled() {
        let host = registry(&["vulkanh264enc", "vulkanh265enc", "nvcudah265enc"]);
        let off = CodecKnob {
            enabled: false,
            explicit: true,
        };
        let knobs = EncoderKnobs {
            hevc: off,
            av1: off,
            ..Default::default()
        };
        assert_eq!(
            codec_plan_with(knobs, "software", &host),
            vec![
                (Codec::H264, CodecPlan::Vulkan),
                (Codec::H265, CodecPlan::Fallback("nvcudah265enc".into())),
                (Codec::Av1, CodecPlan::Disabled),
            ]
        );
        assert_eq!(knobs.h264.source(), "default");
        assert_eq!(knobs.av1.source(), "env");
        assert!(
            !plan_is_degraded(knobs, &codec_plan_with(knobs, "software", &host)),
            "an operator-disabled codec is the knob working, never a degraded image"
        );
    }

    // A knob left ON whose vulkan element the image lacks is a DEGRADED plan,
    // whether it falls back or loses the codec — that is what makes the plan line
    // log at WARN.
    #[test]
    fn plan_is_degraded_when_an_enabled_codec_lost_its_vulkan_element() {
        let broken_image = registry(&["vulkanh264enc", "nvcudah265enc"]);
        let knobs = EncoderKnobs::default();
        let plan = codec_plan_with(knobs, "software", &broken_image);
        assert_eq!(
            plan,
            vec![
                (Codec::H264, CodecPlan::Vulkan),
                (Codec::H265, CodecPlan::Fallback("nvcudah265enc".into())),
                (Codec::Av1, CodecPlan::Unavailable),
            ]
        );
        assert!(
            plan_is_degraded(knobs, &plan),
            "an enabled codec missing its vulkan element must make the plan loud"
        );

        // Same registry, those two codecs disabled: identical resolutions, opposite
        // operator intent, so NOT degraded.
        let off = CodecKnob {
            enabled: false,
            explicit: true,
        };
        let disabled_knobs = EncoderKnobs {
            hevc: off,
            av1: off,
            ..Default::default()
        };
        assert!(!plan_is_degraded(
            disabled_knobs,
            &codec_plan_with(disabled_knobs, "software", &broken_image)
        ));

        // A fully-patched image is not degraded.
        let good = registry(&["vulkanh264enc", "vulkanh265enc", "vulkanav1enc"]);
        assert!(!plan_is_degraded(
            knobs,
            &codec_plan_with(knobs, "software", &good)
        ));
    }

    // The name the missing-element warnings quote must be the name resolution seeks.
    #[test]
    fn vulkan_element_names_match_the_candidate_list() {
        for codec in [Codec::H264, Codec::H265, Codec::Av1] {
            assert_eq!(
                encoder_candidates(
                    EncoderChoice::Vulkan,
                    codec,
                    EncoderKnobs::default(),
                    "software"
                ),
                [vulkan_element(codec)]
            );
        }
    }

    #[test]
    fn codec_plan_reports_unavailable_when_nothing_registers() {
        let host = registry(&["vulkanh264enc"]);
        let plan = codec_plan_with(EncoderKnobs::default(), "software", &host);
        assert_eq!(plan[1], (Codec::H265, CodecPlan::Unavailable));
        assert_eq!(plan[2], (Codec::Av1, CodecPlan::Unavailable));
    }

    // The fallback prefers NVENC, takes VA when only VA can encode AV1, and yields
    // None when neither can — a hard unsupported, never a silent codec swap.
    #[test]
    fn vulkan_av1_fallback_prefers_nvenc_then_va_then_unsupported() {
        let knobs = EncoderKnobs::default();
        let amd_box = registry(&["vulkanh264enc", "vaav1enc"]);
        assert_eq!(
            effective_encoder_with(
                EncoderChoice::Vulkan,
                Codec::Av1,
                knobs,
                "software",
                &amd_box
            ),
            resolved(EncoderChoice::Va, "vaav1enc")
        );

        let no_av1 = registry(&["vulkanh264enc", "nvcudah264enc"]);
        assert_eq!(
            effective_encoder_with(
                EncoderChoice::Vulkan,
                Codec::Av1,
                knobs,
                "software",
                &no_av1
            ),
            None,
            "no vendor AV1 encoder ⇒ unsupported, not a fallback to another codec"
        );
    }

    // QUASAR_ENCODER=nvenc must keep the whole NVENC path — the documented opt-out.
    #[test]
    fn explicit_nvenc_host_resolves_to_nvenc() {
        let nvidia_box = registry(&["vulkanh264enc", "nvcudah264enc", "nvcudaav1enc"]);
        for (codec, factory) in [(Codec::H264, "nvcudah264enc"), (Codec::Av1, "nvcudaav1enc")] {
            assert_eq!(
                effective_encoder_with(
                    EncoderChoice::Nvenc,
                    codec,
                    EncoderKnobs::default(),
                    "software",
                    &nvidia_box
                ),
                resolved(EncoderChoice::Nvenc, factory),
                "explicit nvenc must stay nvenc for {codec:?}"
            );
        }
    }

    #[test]
    fn vulkan_h265_follows_its_knob() {
        assert_eq!(
            encoder_candidates(
                EncoderChoice::Vulkan,
                Codec::H265,
                EncoderKnobs::default(),
                "software"
            ),
            ["vulkanh265enc"],
            "hevc on the vulkan encoder is the default"
        );
        assert!(
            encoder_candidates(
                EncoderChoice::Vulkan,
                Codec::H265,
                disabled(Codec::H265),
                "software"
            )
            .is_empty(),
            "QUASAR_VULKAN_HEVC=0 removes the vulkan h265 candidate"
        );
    }

    // The knob parsing table; pure, no env involved.
    #[test]
    fn codec_knob_parse_table() {
        const VAR: &str = "QUASAR_VULKAN_HEVC";
        for raw in [None, Some(""), Some("  ")] {
            let k = CodecKnob::parse(VAR, raw);
            assert!(k.enabled, "{raw:?} ⇒ enabled");
            assert!(!k.explicit, "{raw:?} is not an explicit operator setting");
            assert_eq!(k.source(), "default");
        }
        for raw in ["1", "true", "TRUE", "on", "On", " true "] {
            let k = CodecKnob::parse(VAR, Some(raw));
            assert!(k.enabled, "{raw:?} ⇒ enabled");
            assert!(k.explicit, "{raw:?} is explicit");
            assert_eq!(k.source(), "env");
        }
        for raw in ["0", "false", "FALSE", "off", "Off", " 0 "] {
            let k = CodecKnob::parse(VAR, Some(raw));
            assert!(!k.enabled, "{raw:?} ⇒ disabled");
            assert!(k.explicit, "{raw:?} is explicit");
            assert_eq!(k.source(), "env");
        }
        for raw in ["yes", "no", "2", "enabled", "-1"] {
            let k = CodecKnob::parse(VAR, Some(raw));
            assert!(
                k.enabled,
                "{raw:?} is unrecognised — warn and keep the codec enabled, never \
                 silently drop it"
            );
            assert!(
                !k.explicit,
                "{raw:?} was rejected, so the source is the default"
            );
        }
    }

    #[test]
    fn encoder_knobs_default_all_enabled() {
        let k = EncoderKnobs::default();
        for codec in [Codec::H264, Codec::H265, Codec::Av1] {
            assert!(k.allows(codec), "{} defaults on", codec.as_str());
            assert_eq!(k.knob(codec).source(), "default");
        }
    }

    // The only test that touches these env vars, so save/restore needs no lock —
    // every other knob test constructs `EncoderKnobs` directly. Keep it that way.
    #[test]
    fn encoder_knobs_from_env_reads_all_three_vars() {
        const VARS: [&str; 3] = [
            "QUASAR_VULKAN_H264",
            "QUASAR_VULKAN_HEVC",
            "QUASAR_VULKAN_AV1",
        ];
        let prior: Vec<_> = VARS.iter().map(|v| std::env::var(v).ok()).collect();

        for v in VARS {
            std::env::remove_var(v);
        }
        assert_eq!(
            EncoderKnobs::from_env(),
            EncoderKnobs::default(),
            "unset ⇒ all three enabled from the default"
        );

        std::env::set_var(VARS[0], "0");
        std::env::set_var(VARS[1], "off");
        std::env::set_var(VARS[2], "1");
        let k = EncoderKnobs::from_env();
        assert!(!k.allows(Codec::H264));
        assert!(!k.allows(Codec::H265));
        assert!(k.allows(Codec::Av1));
        assert_eq!(k.knob(Codec::Av1).source(), "env");

        for (v, was) in VARS.iter().zip(prior) {
            match was {
                Some(val) => std::env::set_var(v, val),
                None => std::env::remove_var(v),
            }
        }
    }

    #[test]
    fn knob_var_names_match_the_documented_knobs() {
        assert_eq!(EncoderKnobs::var_name(Codec::H264), "QUASAR_VULKAN_H264");
        assert_eq!(EncoderKnobs::var_name(Codec::H265), "QUASAR_VULKAN_HEVC");
        assert_eq!(EncoderKnobs::var_name(Codec::Av1), "QUASAR_VULKAN_AV1");
    }

    // SO-07: the VA element name must derive from the assigned render node so a
    // multi-GPU host pins encode to the scheduler-chosen card (Wolf #298).
    #[test]
    fn va_device_element_prefix_maps_render_nodes() {
        assert_eq!(
            va_device_element_prefix("/dev/dri/renderD129").as_deref(),
            Some("varenderD129")
        );
        assert_eq!(
            va_device_element_prefix("/dev/dri/renderD128").as_deref(),
            Some("varenderD128")
        );
        assert_eq!(va_device_element_prefix("software"), None);
        assert_eq!(va_device_element_prefix("/dev/dri/card0"), None);
    }

    #[test]
    fn va_candidates_keep_verified_generic_fallback_for_default_device() {
        assert_eq!(
            va_h264_encoder_candidates("/dev/dri/renderD129"),
            [
                "varenderD129h264lpenc",
                "varenderD129h264enc",
                "vah264lpenc",
                "vah264enc"
            ]
        );
        assert_eq!(
            va_h264_encoder_candidates("software"),
            ["vah264lpenc", "vah264enc"]
        );
    }

    // SO-02: one frame's share of the CBR budget in kbits (`cpb-size` on the VA
    // encoders is "max CPB in Kb").
    #[test]
    fn cpb_size_is_one_frame_budget() {
        assert_eq!(cpb_size_kbits(6000, 60), 100); // 6 Mbps @ 60 → 100 kbit/frame
        assert_eq!(cpb_size_kbits(3000, 30), 100);
        // fps=0 must not divide-by-zero; fall back to the full bitrate (1s CPB).
        assert_eq!(cpb_size_kbits(6000, 0), 6000);
        // never round down to 0 (a 0 cap would be interpreted as auto-calculate).
        assert_eq!(cpb_size_kbits(10, 60), 1);
    }

    // gst 1.24 also ships a LEGACY nvh264enc, so the old name must be tried first.
    #[test]
    fn nvenc_h264_encoder_candidates_prefer_old_name_first() {
        assert_eq!(
            nvenc_h264_encoder_candidates(),
            ["nvcudah264enc", "nvh264enc"]
        );
    }

    // `gop` is frames at the 60 fps reference; scaling keeps the cadence in TIME
    // constant (the 2 Hz AV1 keyframe-burst glitch at 120 fps).
    #[test]
    fn effective_gop_holds_time_cadence_across_fps() {
        assert_eq!(effective_gop(60, 60), 60, "60 fps reference is unchanged");
        assert_eq!(effective_gop(60, 120), 120, "120 fps keeps the 1 s cadence");
        assert_eq!(effective_gop(60, 30), 30, "30 fps keeps the 1 s cadence");
        assert_eq!(
            effective_gop(120, 120),
            240,
            "gop=120 means 2 s at every fps"
        );
        assert_eq!(
            effective_gop(60, 0),
            60,
            "unknown fps falls back to the raw setting"
        );
        assert_eq!(effective_gop(1, 30), 1, "floored at 1, never 0");
    }

    // HEVC arms the ring pin (the H.265 encode-src pool's per-slot tiling defect);
    // the regression guarded here is that av1 without hevc must NOT inherit it.
    #[test]
    fn required_source_ring_depth_is_hevc_only() {
        assert_eq!(
            required_source_ring_depth(EncoderKnobs::default()),
            Some(2),
            "hevc defaults on ⇒ the ring pin is the default posture"
        );
        assert_eq!(
            required_source_ring_depth(disabled(Codec::H265)),
            None,
            "QUASAR_VULKAN_HEVC=0 ⇒ no pin (av1 alone must not inherit it)"
        );
        assert_eq!(
            required_source_ring_depth(disabled(Codec::Av1)),
            Some(2),
            "hevc still pins when only av1 is disabled"
        );
        let mut none_on = disabled(Codec::H265);
        none_on.av1 = disabled(Codec::Av1).av1;
        assert_eq!(
            required_source_ring_depth(none_on),
            None,
            "hevc disabled ⇒ no pin regardless of the other knobs"
        );
    }

    #[test]
    fn openh264_supports_is_h264_only() {
        assert!(openh264_supports(Codec::H264));
        assert!(!openh264_supports(Codec::H265));
        assert!(!openh264_supports(Codec::Av1));
    }
}

/// The vendor×codec support matrix: [`encoder_candidates`], [`openh264_supports`],
/// `pipeline::encoder_builder_kind` and `caps::caps_profile`/`caps::raw_video_caps_for`
/// must be edited together whenever a vendor or codec is added. This iterates every
/// `(EncoderChoice, Codec)` pair and asserts all four agree, so updating three of
/// them fails a fast GPU-free test instead of surfacing on real hardware.
///
/// A merged descriptor type spanning caps.rs and encoders.rs was rejected:
/// `raw_video_caps_for` does not vary by codec, and `build_encoder_element`'s
/// dispatch is already total over `EncoderChoice`.
#[cfg(test)]
mod matrix_tests {
    use super::{encoder_candidates, openh264_supports, EncoderKnobs};
    use crate::session::pipeline::caps::{caps_profile, raw_video_caps_for};
    use crate::session::pipeline::{encoder_builder_kind, EncoderBuilderKind};
    use crate::session::{Codec, EncoderChoice};

    const ALL_CHOICES: [EncoderChoice; 4] = [
        EncoderChoice::Va,
        EncoderChoice::Nvenc,
        EncoderChoice::Vulkan,
        EncoderChoice::Openh264,
    ];
    const ALL_CODECS: [Codec; 3] = [Codec::H264, Codec::H265, Codec::Av1];

    /// Every Vulkan codec knob enabled (the default), so `encoder_candidates`
    /// reports the full structural support matrix.
    fn max_knobs() -> EncoderKnobs {
        EncoderKnobs::default()
    }

    #[test]
    fn candidates_openh264_gate_dispatch_and_caps_all_agree_for_every_pair() {
        gstreamer::init().unwrap();

        for choice in ALL_CHOICES {
            // Pinned literally so a refactor folding these arms behind a wildcard
            // `_` — which would silently swallow a 5th vendor — fails here rather
            // than at a live session launch.
            let expected = match choice {
                EncoderChoice::Va => EncoderBuilderKind::Va,
                EncoderChoice::Nvenc => EncoderBuilderKind::Nvenc,
                EncoderChoice::Vulkan => EncoderBuilderKind::Vulkan,
                EncoderChoice::Openh264 => EncoderBuilderKind::Openh264,
            };
            assert_eq!(
                encoder_builder_kind(choice),
                expected,
                "build_encoder_element's dispatch table diverged for {choice:?}"
            );

            for codec in ALL_CODECS {
                let candidates = encoder_candidates(choice, codec, max_knobs(), "software");
                let candidates_say_supported = !candidates.is_empty();

                // openh264 is the one builder with its own codec allow-list; it must
                // agree with encoder_candidates' Openh264 arm exactly.
                if choice == EncoderChoice::Openh264 {
                    assert_eq!(
                        openh264_supports(codec),
                        candidates_say_supported,
                        "openh264_supports({codec:?}) disagrees with encoder_candidates \
                         for Openh264 — the builder's own guard and the candidate list \
                         must give the same verdict"
                    );
                }

                // caps_profile must resolve for every pair (it depends on codec +
                // vendor identity, never on what this host can build), and its shape
                // must match the codec.
                let profile = caps_profile(codec, "main", choice).unwrap_or_else(|e| {
                    panic!("caps_profile({codec:?}, main, {choice:?}) errored: {e}")
                });
                match codec {
                    Codec::H264 => assert!(
                        profile.is_some(),
                        "h264 must carry a profile for every vendor, got None for {choice:?}"
                    ),
                    Codec::H265 => assert_eq!(
                        profile,
                        Some("main"),
                        "h265 is always Main regardless of vendor, got {profile:?} for {choice:?}"
                    ),
                    Codec::Av1 => assert!(
                        profile.is_none(),
                        "av1 carries no profile field, got {profile:?} for {choice:?}"
                    ),
                }

                // raw_video_caps_for is keyed on EncoderChoice only; this guards
                // against a codec-dependent branch appearing without a matrix update.
                let caps = raw_video_caps_for(choice, 1920, 1080, 60, None);
                assert!(
                    caps.structure(0).is_some(),
                    "raw_video_caps_for({choice:?}) produced caps with no structure \
                     (codec={codec:?})"
                );
            }
        }
    }
}

/// #506 throughput hint: an unmeasured element must yield NOTHING (unknown never
/// gates), and the map must describe the RESOLVED element, not the nominal choice.
#[cfg(test)]
mod pixel_rate_tests {
    use super::{element_pixel_rate_mpix_s, CodecSupport};
    use crate::session::Codec;

    fn support(pairs: &[(Codec, &str)]) -> CodecSupport {
        CodecSupport {
            codecs: pairs.iter().map(|(c, _)| *c).collect(),
            elements: pairs.iter().map(|(_, e)| (*e).to_string()).collect(),
        }
    }

    #[test]
    fn measured_elements_carry_the_issue_506_numbers() {
        assert_eq!(element_pixel_rate_mpix_s("vulkanh265enc"), Some(395.0));
        assert_eq!(element_pixel_rate_mpix_s("vulkanav1enc"), Some(1215.0));
        assert_eq!(element_pixel_rate_mpix_s("vulkanh264enc"), Some(1400.0));
        assert_eq!(element_pixel_rate_mpix_s("nvh265enc"), Some(790.0));
        assert_eq!(element_pixel_rate_mpix_s("nvcudah265enc"), Some(790.0));
    }

    #[test]
    fn unmeasured_elements_are_unknown_not_slow() {
        for factory in [
            "vah264enc",
            "vah265lpenc",
            "varenderD129av1enc",
            "nvcudah264enc",
            "nvav1enc",
            "openh264enc",
            "",
        ] {
            assert_eq!(
                element_pixel_rate_mpix_s(factory),
                None,
                "{factory} must report no hint rather than a guessed one — a guess gates \
                 real launches, an absent value gates nothing"
            );
        }
    }

    /// A Vulkan host whose h265 fell back to NVENC must report NVENC's 790 Mpix/s,
    /// not vulkanh265enc's 395 — why `elements` is carried alongside `codecs`.
    #[test]
    fn rates_follow_the_resolved_element_through_the_vendor_fallback() {
        let s = support(&[
            (Codec::H264, "vulkanh264enc"),
            (Codec::H265, "nvcudah265enc"),
        ]);
        assert_eq!(
            s.pixel_rates_mpix_s(),
            vec![("h264".to_string(), 1400.0), ("h265".to_string(), 790.0)]
        );
    }

    /// An unmeasured codec is absent, so the hint is a SUBSET of the advertised
    /// codec set — what agent-api.md tells the control plane to expect.
    #[test]
    fn unmeasured_codecs_drop_out_of_the_map_entirely() {
        let s = support(&[(Codec::H264, "vah264lpenc"), (Codec::H265, "vah265lpenc")]);
        assert!(s.pixel_rates_mpix_s().is_empty());
        assert_eq!(s.codec_strings(), vec!["h264", "h265"]);
    }
}
