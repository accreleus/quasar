//! `quasar-node-agent media-probe`, the child half of the media host probe (spec #252
//! "Host probes"): composite and encode a few frames on one GPU, answer in one line.
//!
//! Built from the production source, convert, encoder and bitstream builders. Anything a
//! probe adds that a session lacks is a failure mode sessions do not have.
//! Parent half: `crate::host_probe::media`.
//!
//! #282 adds a pixel-verification leg: the probe decodes its own h264 output with
//! `openh264dec` and scores the decoded picture against the compositor's known flat
//! clear-colour field, so the check verifies the picture survived, not just that frames
//! arrived. Design: `docs/superpowers/plans/2026-09-20-282-media-probe-pixel-check.md`.
//!
//! A non-h264 request is a codec probe (#300): no pixel check, so reaching PLAYING and
//! producing the frames without a bus error is its pass.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_app as gst_app;
use gstreamer_video as gst_video;
use gstreamer_video::prelude::*;

use super::{Codec, SessionConfig};

pub const DEFAULT_FRAMES: u64 = 30;
pub const DEFAULT_BUDGET_SECS: u64 = 15;
/// A codec probe proves the encoder starts and produces frames; it scores no pixels, so
/// it needs fewer frames than the H.264 media probe.
pub const CODEC_PROBE_FRAMES: u64 = 10;
pub const CODEC_PROBE_BUDGET_SECS: u64 = 10;

/// What the child was asked to prove. Every field arrives on argv; the binding and
/// encoder selection arrive as env (see `host_probe::media`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MediaProbeRequest {
    pub gpu: i32,
    pub codec: String,
    pub width: i32,
    pub height: i32,
    pub fps: i32,
    pub frames: u64,
    pub budget: Duration,
}

impl Default for MediaProbeRequest {
    /// h264 is the floor codec every host must produce, so it is what a probe asks for.
    fn default() -> Self {
        MediaProbeRequest {
            gpu: 0,
            codec: "h264".into(),
            width: 1280,
            height: 720,
            fps: 60,
            frames: DEFAULT_FRAMES,
            budget: Duration::from_secs(DEFAULT_BUDGET_SECS),
        }
    }
}

impl MediaProbeRequest {
    /// The codec probe for `codec` on `gpu` (CONTEXT.md "Codec probe").
    pub fn codec_probe(gpu: i32, codec: Codec) -> Self {
        MediaProbeRequest {
            gpu,
            codec: codec.as_str().into(),
            frames: CODEC_PROBE_FRAMES,
            budget: Duration::from_secs(CODEC_PROBE_BUDGET_SECS),
            ..MediaProbeRequest::default()
        }
    }
}

/// The child's single stdout line plus its exit code.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ProbeVerdict {
    Pass(String),
    Fail(String),
    /// The GPU encoded fine but the decoded picture is wrong: same line shape
    /// and exit code as `Fail`, kept distinct so `remediation()` can hand back
    /// [`PIXEL_MISMATCH_REMEDIATION`] without sniffing the line text.
    Mismatch(String),
    /// Bad argv — distinct from a fail, so a caller cannot read it as "this GPU cannot
    /// encode".
    Usage(String),
    /// The run could not be concluded — the pixel verification could not reach a verdict,
    /// so the run says nothing about this host. The parent retains the last definitive
    /// result (`host_probe::outcome::record`), which is what amendment 11 requires: an
    /// indeterminate probe neither sets nor clears a block.
    Indeterminate(String),
}

/// Must not contain "WOLF_": it names QUASAR_ENCODER as a way to isolate the fault,
/// never as the fix.
const PIXEL_MISMATCH_REMEDIATION: &str = "The GPU is working — it composited and encoded \
    every probe frame — but the picture that came out of the encoder is wrong, so this GPU \
    is blocked for sessions. To isolate the fault, set QUASAR_ENCODER for this host to \
    another encoder (va, nvenc or openh264) and recreate the agent: if this check then \
    passes, the default encode path is at fault on this GPU and driver. That is a \
    diagnostic step, not a fix. Please report the GPU model, the driver version and the \
    planes named in the summary to the Quasar issue tracker.";

impl ProbeVerdict {
    pub fn line(&self) -> &str {
        match self {
            ProbeVerdict::Pass(s)
            | ProbeVerdict::Fail(s)
            | ProbeVerdict::Mismatch(s)
            | ProbeVerdict::Usage(s)
            | ProbeVerdict::Indeterminate(s) => s,
        }
    }

    pub fn exit_code(&self) -> i32 {
        match self {
            ProbeVerdict::Pass(_) => 0,
            ProbeVerdict::Fail(_) | ProbeVerdict::Mismatch(_) => 1,
            ProbeVerdict::Usage(_) => 2,
            ProbeVerdict::Indeterminate(_) => 3,
        }
    }

    /// A remediation more specific than the per-kind default, printed as a
    /// `quasar-probe-remediation:` line ahead of [`line`](Self::line).
    pub fn remediation(&self) -> Option<&'static str> {
        match self {
            ProbeVerdict::Mismatch(_) => Some(PIXEL_MISMATCH_REMEDIATION),
            _ => None,
        }
    }
}

/// The pixel-verification's contribution to the verdict (design §6). `off` is the worst
/// (max) per-plane disagreement fraction seen over the scored frames.
#[derive(Debug, Clone, PartialEq)]
enum PixelCheck {
    /// The verification could not reach a conclusion; the reason names why (§7). The
    /// encode verdict this run already has still stands — this only ever demotes the
    /// overall result to Indeterminate, never to Fail.
    NotRun(String),
    /// The pixel check verifies h264 only. For another codec the encode verdict alone
    /// decides: frames arriving without a bus error is the codec probe's pass.
    NotCovered(Codec),
    Ok {
        off: f64,
        frames: usize,
    },
    Mismatch {
        off: f64,
        frames: usize,
        /// Comma-joined plane names that exceeded `pixel::OFF_FLOOR` on the worst frame —
        /// chroma-only names a plane-offset/stride defect (#272), luma a layout defect.
        planes: String,
    },
}

/// Pure scoring of a decoded frame against the compositor's known flat clear-colour
/// field. No GStreamer types cross this boundary, so it unit-tests without a GPU.
mod pixel {
    /// One plane's disagreement with the flat reference the compositor should have
    /// produced.
    pub(crate) struct PlaneScore {
        pub name: &'static str,
        pub off: f64,
    }

    /// The decoded caps' colour range, reduced to what the reference rule (§2) needs.
    #[derive(Debug, Clone, Copy, PartialEq, Eq)]
    pub(crate) enum Range {
        Limited,
        Full,
        Unknown,
    }

    /// A flat field is DC-predicted with zero residual at any QP, so reconstruction
    /// error is 0-1 code; the RGB->YUV of exact black is 16/128 +/-1. 8 is more than 4x
    /// any rounding chain and half the 16-code pedestal, so it cannot be crossed by the
    /// range ambiguity either (design §4).
    pub(crate) const TOL_SAMPLE: u8 = 8;
    /// 1% of a 720p luma plane is 9,216 samples (a 96x96 block) — orders of magnitude
    /// above any rounding artefact, and ~7x below the weakest #272 corruption measured
    /// on the probe pipeline itself (design §4).
    pub(crate) const OFF_FLOOR: f64 = 0.01;
    /// Decoded frames to discard before scoring — decoder warm-up, not rate-control
    /// settle: a flat field has no residual to converge at any QP (design §3).
    pub(crate) const SKIP_FRAMES: usize = 5;
    /// Fewer than this many scored frames and the run says nothing (design §7).
    pub(crate) const MIN_SCORED: usize = 5;

    /// `off(p)` = fraction of `samples_rows` (already trimmed to the visible width by the
    /// caller — reading `stride` bytes per row scores the padding, see
    /// gstreamer-gotchas.md) further than `tol` from `reference`.
    pub(crate) fn score_plane(
        name: &'static str,
        samples_rows: &[&[u8]],
        reference: u8,
        tol: u8,
    ) -> PlaneScore {
        let mut total = 0usize;
        let mut bad = 0usize;
        for row in samples_rows {
            for &sample in *row {
                total += 1;
                if (sample as i16 - reference as i16).abs() > tol as i16 {
                    bad += 1;
                }
            }
        }
        PlaneScore {
            name,
            off: if total == 0 {
                0.0
            } else {
                bad as f64 / total as f64
            },
        }
    }

    /// The plane's own median sample — the reference for the range-agnostic uniformity
    /// cross-check, and what picks a luma reference when the caps carry no range.
    pub(crate) fn median_u8(rows: &[&[u8]]) -> u8 {
        let mut samples: Vec<u8> = rows.iter().flat_map(|row| row.iter().copied()).collect();
        if samples.is_empty() {
            return 0;
        }
        samples.sort_unstable();
        samples[samples.len() / 2]
    }

    /// `ref(Y) = 16` for limited range, `0` for full; an absent/unknown range accepts
    /// whichever of 0/16 the frame's own median luma is nearer (design §2), with 8 (the
    /// midpoint) breaking the tie toward limited, the more common wire convention.
    pub(crate) fn luma_reference(range: Range, median_luma: u8) -> u8 {
        match range {
            Range::Limited => 16,
            Range::Full => 0,
            Range::Unknown => {
                if median_luma < 8 {
                    0
                } else {
                    16
                }
            }
        }
    }

    /// The max `off` across planes, and a comma-joined list of the plane names that
    /// exceeded `OFF_FLOOR` (diagnostic: chroma-only names a plane-offset/stride defect,
    /// luma a layout defect).
    pub(crate) fn frame_off(scores: &[PlaneScore]) -> (f64, String) {
        let off = scores.iter().map(|s| s.off).fold(0.0_f64, f64::max);
        let planes = scores
            .iter()
            .filter(|s| s.off > OFF_FLOOR)
            .map(|s| s.name)
            .collect::<Vec<_>>()
            .join(",");
        (off, planes)
    }

    /// Sustained, not one-off (design §3): true iff at least half the scored frames
    /// exceed `floor`, so a single decoder hiccup does not take a working host out of
    /// service.
    pub(crate) fn sustained(offs: &[f64], floor: f64) -> bool {
        if offs.is_empty() {
            return false;
        }
        let bad = offs.iter().filter(|&&off| off > floor).count();
        bad * 2 >= offs.len()
    }
}

/// One decoded frame's score, kept whole (rather than just the max) so the dump knob can
/// report per-plane detail.
struct ScoredFrame {
    y: f64,
    u: f64,
    v: f64,
    off: f64,
    planes: String,
    range: &'static str,
}

/// Maps a decoded I420 `gst::Sample` to its [`ScoredFrame`], or `None` if the sample
/// cannot be read (a diagnostic gap, not a probe failure — the caller's `MIN_SCORED`
/// floor is what turns "too few usable frames" into Indeterminate). The range comes from
/// the sample's own negotiated caps, never hard-coded (design §2).
fn score_sample(sample: &gst::Sample) -> Option<ScoredFrame> {
    let caps = sample.caps()?;
    let info = gst_video::VideoInfo::from_caps(caps).ok()?;
    let buffer = sample.buffer()?;
    let frame = gst_video::VideoFrameRef::from_buffer_ref_readable(buffer, &info).ok()?;

    // Row-by-row, trimmed to the visible width: reading `stride` bytes per row scores
    // the padding, the single most likely false-fail here (gstreamer-gotchas.md).
    let rows_for = |plane: u32| -> Option<Vec<&[u8]>> {
        let stride = *frame.plane_stride().get(plane as usize)?;
        if stride <= 0 {
            return None;
        }
        let stride = stride as usize;
        let width = frame.comp_width(plane) as usize;
        let height = frame.comp_height(plane) as usize;
        let data = frame.plane_data(plane).ok()?;
        let mut rows = Vec::with_capacity(height);
        for row in 0..height {
            let start = row.checked_mul(stride)?;
            let end = start.checked_add(width)?;
            rows.push(data.get(start..end)?);
        }
        Some(rows)
    };

    let y_rows = rows_for(0)?;
    let u_rows = rows_for(1)?;
    let v_rows = rows_for(2)?;

    let (range, range_name) = match info.colorimetry().range() {
        gst_video::VideoColorRange::Range16_235 => (pixel::Range::Limited, "limited"),
        gst_video::VideoColorRange::Range0_255 => (pixel::Range::Full, "full"),
        _ => (pixel::Range::Unknown, "unknown"),
    };
    let y_ref = pixel::luma_reference(range, pixel::median_u8(&y_rows));

    let y_off = pixel::score_plane("y", &y_rows, y_ref, pixel::TOL_SAMPLE).off;
    let u_off = pixel::score_plane("u", &u_rows, 128, pixel::TOL_SAMPLE).off;
    let v_off = pixel::score_plane("v", &v_rows, 128, pixel::TOL_SAMPLE).off;
    let (off, planes) = pixel::frame_off(&[
        pixel::PlaneScore {
            name: "y",
            off: y_off,
        },
        pixel::PlaneScore {
            name: "u",
            off: u_off,
        },
        pixel::PlaneScore {
            name: "v",
            off: v_off,
        },
    ]);

    Some(ScoredFrame {
        y: y_off,
        u: u_off,
        v: v_off,
        off,
        planes,
        range: range_name,
    })
}

/// `QUASAR_MEDIA_PROBE_DUMP=<dir>`: diagnostic-only per-frame JSON dump. Off by default
/// and costs nothing when unset; an IO error here must never change the verdict
/// (`docs/configuration.md`).
struct DumpSink {
    path: std::path::PathBuf,
}

impl DumpSink {
    fn from_env(gpu: i32) -> Option<Self> {
        let dir = std::env::var("QUASAR_MEDIA_PROBE_DUMP").ok()?;
        if dir.trim().is_empty() {
            return None;
        }
        Some(DumpSink {
            path: std::path::Path::new(&dir).join(format!("media-probe-gpu{gpu}.jsonl")),
        })
    }

    fn write(&self, n: usize, frame: &ScoredFrame) {
        use std::io::Write;
        let line = serde_json::json!({
            "n": n,
            "y": frame.y,
            "u": frame.u,
            "v": frame.v,
            "off": frame.off,
            "range": frame.range,
        });
        if let Ok(mut file) = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.path)
        {
            let _ = writeln!(file, "{line}");
        }
    }
}

/// The decode leg's elements, kept together so a bus error can be attributed to it
/// (design §5) without the main loop knowing GStreamer element identity beyond a name.
struct DecodeLeg {
    appsink: gst_app::AppSink,
    element_names: [&'static str; 4],
}

impl DecodeLeg {
    /// Whether a bus message's source path names one of this leg's elements — used to
    /// route an error to the pixel verification (Indeterminate) rather than the probe's
    /// own encode verdict (Fail).
    fn owns(&self, path: &str) -> bool {
        self.element_names.iter().any(|name| path.contains(name))
    }
}

/// How far the pipeline got. A device whose video engine lacks the codec fails opening
/// the encoder at NULL→READY (the AMD VCN 3.x AV1 signature).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Reached {
    NotReady,
    NotPlaying,
    Playing,
}

/// What the run observed, so the verdict wording is decided in one pure place.
struct Observed {
    frames: u64,
    encoder: String,
    render_node: String,
    /// The first bus ERROR not attributed to the decode leg, already formatted with its
    /// debug text.
    error: Option<String>,
    reached: Reached,
    pixel: PixelCheck,
}

/// Pass iff the pipeline reached PLAYING, the wanted frames arrived with no bus error, and
/// (h264 only) the decoded picture matched the compositor's known flat field. Pure:
/// unit-tested without a GPU.
fn verdict(wanted: u64, seen: &Observed) -> ProbeVerdict {
    let state = match seen.reached {
        Reached::NotReady => Some("READY"),
        Reached::NotPlaying => Some("PLAYING"),
        Reached::Playing => None,
    };
    if let Some(state) = state {
        let why = seen
            .error
            .as_deref()
            .unwrap_or("no error message was posted");
        return ProbeVerdict::Fail(format!(
            "{}: the encode pipeline could not reach {state} on {}: {why}",
            seen.encoder, seen.render_node
        ));
    }
    if let Some(error) = &seen.error {
        return ProbeVerdict::Fail(format!("{}: {error}", seen.encoder));
    }
    if seen.frames < wanted {
        return ProbeVerdict::Fail(format!(
            "{}: only {} of {wanted} frames encoded on {} within the probe budget",
            seen.encoder, seen.frames, seen.render_node
        ));
    }
    match &seen.pixel {
        // Evidence-first, and QUASAR_ENCODER named as the way to ISOLATE which side is
        // at fault, never as "the fix" — #272's cause was the compositor-encoder
        // handoff, not the encoder element itself (design §9).
        PixelCheck::Mismatch {
            off,
            frames,
            planes,
        } => ProbeVerdict::Mismatch(format!(
            "{}: the picture did not survive: {:.2}% of the decoded {planes} samples do not \
             match the picture the compositor fed in on {} (worst plane over {frames} decoded \
             frames). The GPU is working — the frames encoded. The fault is in the \
             compositor-to-encoder handoff or in the encoder itself. Set QUASAR_ENCODER for \
             this host to another encoder (va, nvenc or openh264) and recreate the agent: if \
             the picture is then correct, the encode path is at fault.",
            seen.encoder,
            off * 100.0,
            seen.render_node
        )),
        PixelCheck::NotCovered(codec) => ProbeVerdict::Pass(format!(
            "encoded {} frames with {} on {} ({}; the pixel check covers h264 only)",
            seen.frames,
            seen.encoder,
            seen.render_node,
            codec.as_str()
        )),
        PixelCheck::NotRun(reason) => ProbeVerdict::Indeterminate(format!(
            "{}: encoded {} frames on {}, but the picture could not be verified: {reason}",
            seen.encoder, seen.frames, seen.render_node
        )),
        PixelCheck::Ok { off, frames } => ProbeVerdict::Pass(format!(
            "encoded {} frames with {} on {}; the decoded picture matched what the compositor \
             fed in (worst plane {:.4} over {frames} frames)",
            seen.frames, seen.encoder, seen.render_node, off
        )),
    }
}

/// Run the probe. Initialises GStreamer exactly as a session does — `GST_REGISTRY` for
/// the VA path included — or the probe answers a different question than the host faces.
pub fn run(request: &MediaProbeRequest) -> ProbeVerdict {
    let codec = match Codec::parse(&request.codec) {
        Ok(c) => c,
        Err(e) => return ProbeVerdict::Usage(format!("{e:#}")),
    };
    if request.frames == 0 || request.width <= 0 || request.height <= 0 || request.fps <= 0 {
        return ProbeVerdict::Usage(format!(
            "media-probe: --frames must be > 0 and --size WxH@FPS positive (got {}x{}@{}, {} frames)",
            request.width, request.height, request.fps, request.frames
        ));
    }

    // The compositor source, not `use_test_src`: the probe exists to prove the compositor
    // arm the session builds.
    let mut cfg = SessionConfig::from_env(false, None);
    cfg.stream.codec = codec;
    cfg.stream.width = request.width;
    cfg.stream.height = request.height;
    cfg.stream.fps = request.fps;
    if let Err(e) = super::init_gstreamer(&cfg) {
        return ProbeVerdict::Fail(format!("gstreamer init failed: {e:#}"));
    }

    tracing::info!(
        "media probe: gpu={} {}x{}@{} codec={} render_node={} encoder={:?}, want {} frames within {:?}",
        request.gpu,
        request.width,
        request.height,
        request.fps,
        codec.as_str(),
        cfg.render_node,
        cfg.encoder,
        request.frames,
        request.budget,
    );

    let render_node = cfg.render_node.clone();
    match build_and_run(&mut cfg, codec, request) {
        Ok(seen) => verdict(request.frames, &seen),
        // A build failure names its element through the production builders' contexts.
        Err(e) => ProbeVerdict::Fail(format!("{render_node}: {e:#}")),
    }
}

/// Builds the decode leg off `tee` — absent (not failed) when `openh264dec` is not
/// registered on this image; the caller treats that as Indeterminate, never a fail
/// (design §7).
fn build_decode_leg(
    pipeline: &gst::Pipeline,
    tee: &gst::Element,
) -> anyhow::Result<Option<DecodeLeg>> {
    use anyhow::Context;

    if gst::ElementFactory::find("openh264dec").is_none() {
        return Ok(None);
    }

    let queue = gst::ElementFactory::make("queue")
        .name("media-probe-decode-queue")
        .build()
        .context("decode-leg queue not found")?;
    let decoder = gst::ElementFactory::make("openh264dec")
        .name("media-probe-decoder")
        .build()
        .context("openh264dec not found")?;
    let decode_caps = gst::Caps::builder("video/x-raw")
        .field("format", "I420")
        .build();
    let capsfilter = gst::ElementFactory::make("capsfilter")
        .name("media-probe-decode-caps")
        .property("caps", &decode_caps)
        .build()
        .context("decode-leg capsfilter not found")?;
    let appsink_elem = gst::ElementFactory::make("appsink")
        .name("media-probe-decode-sink")
        .property("emit-signals", false)
        .property("sync", false)
        .property("max-buffers", 8u32)
        .property("drop", false)
        .build()
        .context("appsink not found")?;
    let appsink = appsink_elem
        .dynamic_cast::<gst_app::AppSink>()
        .map_err(|_| anyhow::anyhow!("the appsink element did not bind as a GstAppSink"))?;

    pipeline
        .add_many([
            &queue,
            &decoder,
            &capsfilter,
            appsink.upcast_ref::<gst::Element>(),
        ])
        .context("adding the decode leg")?;
    gst::Element::link_many([
        tee,
        &queue,
        &decoder,
        &capsfilter,
        appsink.upcast_ref::<gst::Element>(),
    ])
    .context("linking the decode leg")?;

    Ok(Some(DecodeLeg {
        appsink,
        element_names: [
            "media-probe-decode-queue",
            "media-probe-decoder",
            "media-probe-decode-caps",
            "media-probe-decode-sink",
        ],
    }))
}

/// The evidence for a failed state change: the encoder's own error over a downstream
/// consequence (a not-negotiated from the parser), else the first posted.
fn encoder_error_first(errors: Vec<(bool, String)>) -> Option<String> {
    let first = errors.first().map(|(_, text)| text.clone());
    errors
        .into_iter()
        .find(|(from_encoder, _)| *from_encoder)
        .map(|(_, text)| text)
        .or(first)
}

/// `<element path>: <error> (<debug>)` for an ERROR message; `None` for anything else.
fn bus_error_text(msg: &gst::Message) -> Option<String> {
    let gst::MessageView::Error(e) = msg.view() else {
        return None;
    };
    let from = msg
        .src()
        .map(|s| s.path_string().to_string())
        .unwrap_or_else(|| "pipeline".into());
    // The verdict is one stdout line: a newline in the debug text would cut it short.
    Some(format!("{from}: {} ({})", e.error(), e.debug().unwrap_or_default()).replace('\n', " "))
}

fn build_and_run(
    cfg: &mut SessionConfig,
    codec: Codec,
    request: &MediaProbeRequest,
) -> anyhow::Result<Observed> {
    use anyhow::Context;

    let resolved = super::pipeline::resolve_effective_encoder(cfg)?;
    let cfg = &*cfg;

    let pipeline = gst::Pipeline::new();
    let tail = super::pipeline::build_video_source(&pipeline, cfg, None)
        .context("compositor source stage")?;
    let convert =
        super::pipeline::build_gpu_convert_stage(cfg, None).context("GPU convert stage")?;
    let encoder = super::pipeline::build_encoder_element_for_probe(cfg, codec, &resolved.factory)
        .with_context(|| format!("encoder stage ({})", resolved.factory))?;
    let (profile_caps, parser) =
        super::pipeline::build_bitstream_chain_for_probe(codec, cfg).context("bitstream stage")?;

    // The encoded-frame count still lands on the fakesink leg, unchanged in position and
    // meaning. A `tee` after the parser adds a second leg to `openh264dec`, which decodes
    // the probe's own output so the picture can be checked against the compositor's known
    // flat clear-colour field (#282).
    let tee = gst::ElementFactory::make("tee")
        .build()
        .context("tee not found")?;
    let count_queue = gst::ElementFactory::make("queue")
        .build()
        .context("count-leg queue not found")?;
    let sink = gst::ElementFactory::make("fakesink")
        .property("sync", false)
        .build()
        .context("fakesink not found")?;

    pipeline
        .add_many([&encoder, &profile_caps, &parser, &tee, &count_queue, &sink])
        .context("adding the encode chain")?;
    if !convert.is_empty() {
        pipeline.add_many(convert.iter())?;
    }
    let mut chain: Vec<&gst::Element> = vec![&tail];
    chain.extend(convert.iter());
    chain.extend_from_slice(&[&encoder, &profile_caps, &parser, &tee]);
    gst::Element::link_many(chain).context("linking the probe encode chain")?;
    gst::Element::link_many([&tee, &count_queue, &sink]).context("linking the count leg")?;

    // Counted at the sink pad, after the parser: a buffer here is an encoded frame that
    // survived parsing. The closure captures only the counter — capturing a strong clone
    // of the element it is attached to is a GObject ref cycle (gstreamer-gotchas).
    let frames = Arc::new(AtomicU64::new(0));
    let counter = frames.clone();
    let sink_pad = sink
        .static_pad("sink")
        .context("fakesink has no sink pad")?;
    sink_pad.add_probe(gst::PadProbeType::BUFFER, move |_, _| {
        counter.fetch_add(1, Ordering::Relaxed);
        gst::PadProbeReturn::Ok
    });

    // Codec coverage is stated, not quietly widened (design §5): the decode leg only
    // ever verifies h264, so it is not built for another codec even if a future caller
    // widens `MediaProbeRequest`.
    let decode = if codec == Codec::H264 {
        build_decode_leg(&pipeline, &tee)?
    } else {
        None
    };

    let teardown = |pipeline: &gst::Pipeline| {
        // NULL on every path: a probe that leaves an encoder session open is worse than
        // one that fails.
        let _ = pipeline.set_state(gst::State::Null);
    };

    let bus = match pipeline.bus() {
        Some(bus) => bus,
        None => {
            teardown(&pipeline);
            anyhow::bail!("the probe pipeline has no bus");
        }
    };

    // Stepped through READY so the verdict names the state the encoder could not reach,
    // with the element's own error message as the evidence.
    for (state, stuck) in [
        (gst::State::Ready, Reached::NotReady),
        (gst::State::Playing, Reached::NotPlaying),
    ] {
        if pipeline.set_state(state).is_err() {
            // A streaming thread may post its ERROR just after the state change returns,
            // so wait briefly for the first, then take whatever else is queued.
            let mut errors = Vec::new();
            let mut next = bus.timed_pop_filtered(
                gst::ClockTime::from_mseconds(200),
                &[gst::MessageType::Error],
            );
            while let Some(msg) = next {
                let from_encoder = msg.src() == Some(encoder.upcast_ref::<gst::Object>());
                if let Some(text) = bus_error_text(&msg) {
                    errors.push((from_encoder, text));
                }
                next = bus.pop_filtered(&[gst::MessageType::Error]);
            }
            let error = encoder_error_first(errors);
            teardown(&pipeline);
            return Ok(Observed {
                frames: 0,
                encoder: resolved.factory.clone(),
                render_node: cfg.render_node.clone(),
                error,
                reached: stuck,
                pixel: PixelCheck::NotRun("the encode pipeline never started".into()),
            });
        }
    }

    let dump = DumpSink::from_env(request.gpu);
    let mut decoded_seen: usize = 0;
    let mut scored_offs: Vec<f64> = Vec::new();
    let mut worst_off = 0.0_f64;
    let mut worst_planes = String::new();
    let mut failure: Option<String> = None;
    let mut pixel_not_run: Option<String> = None;
    let mut eos_sent = false;
    let mut eos_seen = false;

    let deadline = Instant::now() + request.budget;
    'pump: while Instant::now() < deadline {
        // Drain the decode leg every pass, whether or not the encode count has been
        // reached yet: the appsink queue is bounded (`max-buffers=8`, `drop=false`), and
        // a `tee` pushes to its branches from the same streaming thread — an undrained
        // decode leg would eventually backpressure the count leg too.
        if let Some(leg) = &decode {
            while let Some(sample) = leg
                .appsink
                .try_pull_sample(Some(gst::ClockTime::from_mseconds(0)))
            {
                decoded_seen += 1;
                if decoded_seen <= pixel::SKIP_FRAMES {
                    continue;
                }
                if let Some(scored) = score_sample(&sample) {
                    if let Some(dump) = &dump {
                        dump.write(decoded_seen, &scored);
                    }
                    scored_offs.push(scored.off);
                    if scored.off > worst_off {
                        worst_off = scored.off;
                        worst_planes = scored.planes.clone();
                    }
                }
            }
        }

        let encoded_done = frames.load(Ordering::Relaxed) >= request.frames;
        match &decode {
            // No decode leg: the original gate is also the stopping condition — there is
            // nothing left to drain.
            None if encoded_done => break,
            None => {}
            Some(_) => {
                if encoded_done && !eos_sent {
                    // Gate on the decoded count, and drain (design §3/§4): a decoder
                    // holds frames in its DPB, so stopping at the encoded count would
                    // routinely leave too few decoded frames and trip our own
                    // Indeterminate on a healthy host.
                    pipeline.send_event(gst::event::Eos::new());
                    eos_sent = true;
                }
                if eos_sent && (eos_seen || decoded_seen as u64 >= request.frames) {
                    break;
                }
            }
        }

        let Some(msg) = bus.timed_pop(gst::ClockTime::from_mseconds(20)) else {
            continue;
        };
        match msg.view() {
            gst::MessageView::Eos(_) => {
                eos_seen = true;
            }
            gst::MessageView::Error(_) => {
                let from = msg
                    .src()
                    .map(|s| s.path_string().to_string())
                    .unwrap_or_else(|| "pipeline".into());
                let text = bus_error_text(&msg).unwrap_or_default();
                // Attribute by source element (design §5): an error from the decode leg
                // says nothing about the host's encode path, so it must not fail a
                // healthy host.
                if decode.as_ref().is_some_and(|leg| leg.owns(&from)) {
                    pixel_not_run = Some(text);
                } else {
                    failure = Some(text);
                }
                break 'pump;
            }
            _ => {}
        }
    }

    let pixel = if let Some(reason) = pixel_not_run {
        PixelCheck::NotRun(reason)
    } else if codec != Codec::H264 {
        PixelCheck::NotCovered(codec)
    } else if decode.is_none() {
        PixelCheck::NotRun("openh264dec is not registered on this image".into())
    } else if !eos_seen && Instant::now() >= deadline {
        PixelCheck::NotRun(format!(
            "the probe's budget expired before the decode leg drained ({} of {} frames \
             decoded, {} scored)",
            decoded_seen,
            request.frames,
            scored_offs.len()
        ))
    } else if scored_offs.len() < pixel::MIN_SCORED {
        PixelCheck::NotRun(format!(
            "only {} of the required {} decoded frames could be scored within the probe \
             budget",
            scored_offs.len(),
            pixel::MIN_SCORED
        ))
    } else if pixel::sustained(&scored_offs, pixel::OFF_FLOOR) {
        PixelCheck::Mismatch {
            off: worst_off,
            frames: scored_offs.len(),
            planes: worst_planes,
        }
    } else {
        PixelCheck::Ok {
            off: worst_off,
            frames: scored_offs.len(),
        }
    };

    let seen = Observed {
        frames: frames.load(Ordering::Relaxed),
        encoder: resolved.factory.clone(),
        render_node: cfg.render_node.clone(),
        error: failure,
        reached: Reached::Playing,
        pixel,
    };
    teardown(&pipeline);
    Ok(seen)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn observed(frames: u64, error: Option<&str>) -> Observed {
        Observed {
            frames,
            encoder: "vulkanh264enc".into(),
            render_node: "/dev/dri/renderD128".into(),
            error: error.map(str::to_string),
            reached: Reached::Playing,
            pixel: PixelCheck::Ok {
                off: 0.0001,
                frames: 20,
            },
        }
    }

    fn observed_with_pixel(frames: u64, pixel: PixelCheck) -> Observed {
        Observed {
            frames,
            encoder: "vulkanh264enc".into(),
            render_node: "/dev/dri/renderD128".into(),
            error: None,
            reached: Reached::Playing,
            pixel,
        }
    }

    #[test]
    fn enough_frames_and_no_error_passes_naming_the_encoder_and_node() {
        assert_eq!(
            verdict(30, &observed(30, None)),
            ProbeVerdict::Pass(
                "encoded 30 frames with vulkanh264enc on /dev/dri/renderD128; the decoded \
                 picture matched what the compositor fed in (worst plane 0.0001 over 20 \
                 frames)"
                    .into()
            )
        );
        assert_eq!(verdict(30, &observed(31, None)).exit_code(), 0);
    }

    #[test]
    fn a_bus_error_fails_with_one_line_naming_the_encoder() {
        let v = verdict(
            30,
            &observed(0, Some("device lost (vk: ERROR_DEVICE_LOST)")),
        );
        assert_eq!(
            v,
            ProbeVerdict::Fail("vulkanh264enc: device lost (vk: ERROR_DEVICE_LOST)".into())
        );
        assert_eq!(v.exit_code(), 1);
        assert!(!v.line().contains('\n'));
    }

    /// An error outranks the frame count: frames that arrived before a device loss are
    /// not a pass.
    #[test]
    fn a_bus_error_outranks_the_frame_count() {
        assert!(matches!(
            verdict(30, &observed(60, Some("boom"))),
            ProbeVerdict::Fail(_)
        ));
    }

    #[test]
    fn too_few_frames_by_the_budget_fails_with_the_count() {
        let v = verdict(30, &observed(7, None));
        assert_eq!(
            v,
            ProbeVerdict::Fail(
                "vulkanh264enc: only 7 of 30 frames encoded on /dev/dri/renderD128 within \
                 the probe budget"
                    .into()
            )
        );
        assert_eq!(v.exit_code(), 1);
    }

    #[test]
    fn zero_frames_is_a_fail_not_a_pass() {
        assert!(matches!(
            verdict(30, &observed(0, None)),
            ProbeVerdict::Fail(_)
        ));
    }

    #[test]
    fn a_usage_error_is_exit_two() {
        assert_eq!(ProbeVerdict::Usage("bad codec".into()).exit_code(), 2);
    }

    #[test]
    fn the_default_request_is_h264_720p60_thirty_frames() {
        let d = MediaProbeRequest::default();
        assert_eq!(
            (d.codec.as_str(), d.width, d.height, d.fps, d.frames),
            ("h264", 1280, 720, 60, 30)
        );
        assert_eq!(d.budget, Duration::from_secs(15));
    }

    // ── #282: the pixel verification's contribution to the verdict ───────────────────

    #[test]
    fn a_pixel_mismatch_fails_with_evidence_first_wording_naming_quasar_encoder_as_isolation() {
        let v = verdict(
            30,
            &observed_with_pixel(
                30,
                PixelCheck::Mismatch {
                    off: 0.2533,
                    frames: 20,
                    planes: "u,v".into(),
                },
            ),
        );
        assert_eq!(v.exit_code(), 1);
        assert!(matches!(v, ProbeVerdict::Mismatch(_)));
        let line = v.line();
        assert!(line.contains("25.33%"));
        assert!(line.contains("u,v"));
        assert!(line.contains("the picture did not survive"));
        assert!(line.contains("The GPU is working"));
        // QUASAR_ENCODER is named as the way to ISOLATE which side is at fault, never as
        // the fix — the wording must not claim it repairs anything by itself.
        assert!(line.contains("QUASAR_ENCODER"));
        assert!(!line.to_lowercase().contains("will fix"));
    }

    #[test]
    fn a_mismatch_verdict_carries_the_isolation_remediation() {
        let v = verdict(
            30,
            &observed_with_pixel(
                30,
                PixelCheck::Mismatch {
                    off: 0.2533,
                    frames: 20,
                    planes: "u,v".into(),
                },
            ),
        );
        assert_eq!(v.exit_code(), 1);
        let remediation = v.remediation().expect("a mismatch carries remediation");
        assert!(remediation.contains("QUASAR_ENCODER"));
        assert!(remediation.contains("GPU model"));
        assert!(remediation.contains("driver version"));
        assert!(remediation.contains("planes"));
        assert!(!remediation.contains("WOLF_"));
    }

    #[test]
    fn every_other_verdict_carries_no_remediation() {
        assert_eq!(ProbeVerdict::Pass("ok".into()).remediation(), None);
        assert_eq!(ProbeVerdict::Fail("boom".into()).remediation(), None);
        assert_eq!(ProbeVerdict::Usage("bad".into()).remediation(), None);
        assert_eq!(
            ProbeVerdict::Indeterminate("dunno".into()).remediation(),
            None
        );
    }

    #[test]
    fn a_pixel_check_that_could_not_run_is_indeterminate_never_fail() {
        let v = verdict(
            30,
            &observed_with_pixel(
                30,
                PixelCheck::NotRun("openh264dec is not registered on this image".into()),
            ),
        );
        assert_eq!(v.exit_code(), 3);
        assert!(matches!(v, ProbeVerdict::Indeterminate(_)));
        assert!(!matches!(v, ProbeVerdict::Fail(_)));
        assert!(v.line().contains("openh264dec is not registered"));
        assert!(v.line().contains("encoded 30 frames"));
    }

    #[test]
    fn a_bus_error_and_frame_shortfall_both_outrank_the_pixel_check() {
        // The pixel check never even gets consulted when the encode side already failed.
        assert!(matches!(
            verdict(
                30,
                &Observed {
                    frames: 30,
                    encoder: "vulkanh264enc".into(),
                    render_node: "/dev/dri/renderD128".into(),
                    error: Some("boom".into()),
                    reached: Reached::Playing,
                    pixel: PixelCheck::Mismatch {
                        off: 0.9,
                        frames: 20,
                        planes: "y,u,v".into(),
                    },
                }
            ),
            ProbeVerdict::Fail(_)
        ));
    }

    // ── #282: pure scoring primitives ─────────────────────────────────────────────────

    /// 720x720/2 chroma-shaped synthetic rows: `width` bytes per row, no padding, so the
    /// tests exercise `score_plane` directly without a real `VideoFrameRef`.
    fn flat_rows(value: u8, width: usize, height: usize) -> Vec<Vec<u8>> {
        (0..height).map(|_| vec![value; width]).collect()
    }

    fn as_row_refs(rows: &[Vec<u8>]) -> Vec<&[u8]> {
        rows.iter().map(|r| r.as_slice()).collect()
    }

    #[test]
    fn score_plane_orders_identical_below_noise_below_displacement_and_green() {
        let reference = 16u8;
        let identical = flat_rows(reference, 64, 64);
        let identical_off =
            pixel::score_plane("y", &as_row_refs(&identical), reference, pixel::TOL_SAMPLE).off;
        assert_eq!(identical_off, 0.0);

        // Lossy-like noise: every sample within tolerance except a thin sliver — still
        // near zero, but not identically zero.
        let mut noise = flat_rows(reference, 64, 64);
        for row in noise.iter_mut().take(1) {
            row[0] = reference.saturating_add(pixel::TOL_SAMPLE + 1);
        }
        let noise_off =
            pixel::score_plane("y", &as_row_refs(&noise), reference, pixel::TOL_SAMPLE).off;
        assert!(noise_off > identical_off);
        assert!(noise_off < 0.05);

        // A column displacement: a solid block of rows reading badly wrong content.
        let mut displaced = flat_rows(reference, 64, 64);
        for row in displaced.iter_mut().skip(32) {
            row.fill(200);
        }
        let displaced_off =
            pixel::score_plane("y", &as_row_refs(&displaced), reference, pixel::TOL_SAMPLE).off;
        assert!(displaced_off > noise_off);

        // The measured #272 signature: a flat "green" region (zeroed chroma against a
        // reference of 128).
        let green = flat_rows(0, 64, 64);
        let green_off = pixel::score_plane("u", &as_row_refs(&green), 128, pixel::TOL_SAMPLE).off;
        assert_eq!(green_off, 1.0);
        assert!(green_off > noise_off);
    }

    #[test]
    fn score_plane_all_black_and_all_white_against_a_limited_range_reference() {
        let black = flat_rows(16, 16, 16);
        assert_eq!(
            pixel::score_plane("y", &as_row_refs(&black), 16, pixel::TOL_SAMPLE).off,
            0.0
        );
        let white = flat_rows(235, 16, 16);
        assert_eq!(
            pixel::score_plane("y", &as_row_refs(&white), 16, pixel::TOL_SAMPLE).off,
            1.0
        );
    }

    #[test]
    fn median_u8_takes_the_middle_of_the_sorted_samples() {
        let rows = vec![vec![5u8, 1, 3], vec![4, 2]];
        assert_eq!(pixel::median_u8(&as_row_refs(&rows)), 3);
        let empty: &[&[u8]] = &[];
        assert_eq!(pixel::median_u8(empty), 0);
    }

    #[test]
    fn frame_off_is_the_worst_plane_and_names_only_the_planes_over_the_floor() {
        let scores = [
            pixel::PlaneScore {
                name: "y",
                off: 0.005,
            },
            pixel::PlaneScore {
                name: "u",
                off: 0.02,
            },
            pixel::PlaneScore {
                name: "v",
                off: 0.001,
            },
        ];
        let (off, planes) = pixel::frame_off(&scores);
        assert_eq!(off, 0.02);
        assert_eq!(planes, "u");
    }

    #[test]
    fn sustained_is_false_under_half_and_true_at_exactly_half() {
        // 1 of 4 over the floor: under half.
        assert!(!pixel::sustained(
            &[0.02, 0.005, 0.005, 0.005],
            pixel::OFF_FLOOR
        ));
        // 2 of 4 over the floor: exactly half.
        assert!(pixel::sustained(
            &[0.02, 0.02, 0.005, 0.005],
            pixel::OFF_FLOOR
        ));
    }

    #[test]
    fn sustained_is_false_with_no_scored_frames() {
        assert!(!pixel::sustained(&[], pixel::OFF_FLOOR));
    }

    #[test]
    fn luma_reference_is_16_for_limited_0_for_full_and_nearest_median_when_unknown() {
        assert_eq!(pixel::luma_reference(pixel::Range::Limited, 0), 16);
        assert_eq!(pixel::luma_reference(pixel::Range::Limited, 235), 16);
        assert_eq!(pixel::luma_reference(pixel::Range::Full, 16), 0);
        assert_eq!(pixel::luma_reference(pixel::Range::Unknown, 0), 0);
        assert_eq!(pixel::luma_reference(pixel::Range::Unknown, 7), 0);
        assert_eq!(pixel::luma_reference(pixel::Range::Unknown, 8), 16);
        assert_eq!(pixel::luma_reference(pixel::Range::Unknown, 16), 16);
    }

    // ── #300: the codec probe (a non-H.264 request) ──────────────────────────────────

    fn observed_hevc(frames: u64, reached: Reached, error: Option<&str>) -> Observed {
        Observed {
            frames,
            encoder: "vulkanh265enc".into(),
            render_node: "/dev/dri/renderD129".into(),
            error: error.map(str::to_string),
            reached,
            pixel: PixelCheck::NotCovered(Codec::H265),
        }
    }

    #[test]
    fn an_hevc_request_with_a_passing_encode_passes_without_the_pixel_check() {
        let v = verdict(10, &observed_hevc(10, Reached::Playing, None));
        assert!(matches!(v, ProbeVerdict::Pass(_)), "{v:?}");
        assert_eq!(v.exit_code(), 0);
        assert!(v.line().contains("encoded 10 frames with vulkanh265enc"));
        assert!(v.line().contains("h264 only"), "{}", v.line());
    }

    #[test]
    fn a_pipeline_that_cannot_reach_ready_is_a_definitive_fail_carrying_the_evidence() {
        let v = verdict(
            10,
            &Observed {
                encoder: "vulkanav1enc".into(),
                ..observed_hevc(
                    0,
                    Reached::NotReady,
                    Some("vulkanav1enc0: Could not open the encoder (no AV1 encode profile)"),
                )
            },
        );
        assert!(matches!(v, ProbeVerdict::Fail(_)), "{v:?}");
        assert_eq!(v.exit_code(), 1);
        assert!(v.line().contains("could not reach READY"), "{}", v.line());
        assert!(v.line().contains("no AV1 encode profile"), "{}", v.line());
        assert!(!v.line().contains('\n'));
    }

    #[test]
    fn a_pipeline_that_reached_ready_but_not_playing_is_a_fail_naming_playing() {
        let v = verdict(10, &observed_hevc(0, Reached::NotPlaying, None));
        assert_eq!(v.exit_code(), 1);
        assert!(v.line().contains("could not reach PLAYING"), "{}", v.line());
    }

    /// The exit-code mapping for a non-H.264 request: the pixel check's absence is no
    /// longer indeterminate (3), so the child answers pass (0) or fail (1).
    #[test]
    fn a_non_h264_request_exits_zero_or_one_never_indeterminate() {
        for (seen, code) in [
            (observed_hevc(10, Reached::Playing, None), 0),
            (observed_hevc(3, Reached::Playing, None), 1),
            (observed_hevc(10, Reached::Playing, Some("boom")), 1),
            (observed_hevc(0, Reached::NotReady, None), 1),
        ] {
            assert_eq!(verdict(10, &seen).exit_code(), code);
        }
    }

    #[test]
    fn the_encoders_own_error_is_the_evidence_over_a_downstream_one() {
        assert_eq!(
            encoder_error_first(vec![
                (false, "h265parse0: not-negotiated".into()),
                (true, "vulkanh265enc0: no encode profile".into()),
            ]),
            Some("vulkanh265enc0: no encode profile".into())
        );
        assert_eq!(
            encoder_error_first(vec![(false, "first".into()), (false, "second".into())]),
            Some("first".into())
        );
        assert_eq!(encoder_error_first(Vec::new()), None);
    }

    #[test]
    fn a_codec_probe_request_asks_for_ten_frames_within_ten_seconds() {
        let r = MediaProbeRequest::codec_probe(1, Codec::Av1);
        assert_eq!((r.gpu, r.codec.as_str(), r.frames), (1, "av1", 10));
        assert_eq!(r.budget, Duration::from_secs(10));
        assert_eq!((r.width, r.height, r.fps), (1280, 720, 60));
    }

    // ── #282 §8: the check id and its `blocks` are unchanged ─────────────────────────

    #[test]
    fn the_check_id_and_blocks_stay_media_probe_gpu_n_scoped_to_that_gpu() {
        use crate::host_probe::{ProbeKind, ProbeTarget};
        use crate::messages::ReadinessBlocks;

        let target = ProbeTarget::gpu(ProbeKind::Media, 3);
        assert_eq!(target.check_id(), "media_probe_gpu3");
        assert_eq!(
            target.blocks(),
            Some(ReadinessBlocks::gpu(3, "control_plane"))
        );
    }
}
