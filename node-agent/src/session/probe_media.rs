//! `quasar-node-agent media-probe`, the child half of the media host probe (spec #252
//! "Host probes"): composite and encode a few frames on one GPU, answer in one line.
//!
//! Built from the production source, convert, encoder and bitstream builders. Anything a
//! probe adds that a session lacks is a failure mode sessions do not have.
//! Parent half: `crate::host_probe::media`.
//!
//! #282 adds a pixel-verification leg: the probe decodes its own h264 output with
//! `openh264dec` and scores the decoded picture against the compositor's test picture
//! (#289: a fixed grid of four colours, shown only while no client is mapped), so the
//! check verifies the picture survived, moved pixels included, not just that frames
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
    /// A codec probe (#311): the encode pipeline could not reach READY, i.e. the device
    /// has no encoder for this codec. A hardware fact, not a fault — definitive like
    /// `Fail`, but reported as readiness status `unsupported`. The H.264 media probe never
    /// answers this: H.264 is the floor, so its READY failure stays `Fail`.
    Unsupported(String),
}

/// The child's exit code for [`ProbeVerdict::Unsupported`]; the parent reads it in
/// `host_probe::outcome::child_outcome`.
pub const UNSUPPORTED_EXIT: i32 = 4;

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
            | ProbeVerdict::Indeterminate(s)
            | ProbeVerdict::Unsupported(s) => s,
        }
    }

    pub fn exit_code(&self) -> i32 {
        match self {
            ProbeVerdict::Pass(_) => 0,
            ProbeVerdict::Fail(_) | ProbeVerdict::Mismatch(_) => 1,
            ProbeVerdict::Usage(_) => 2,
            ProbeVerdict::Indeterminate(_) => 3,
            ProbeVerdict::Unsupported(_) => UNSUPPORTED_EXIT,
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

/// Pure scoring of a decoded frame against the compositor's test picture. No GStreamer
/// types cross this boundary, so it unit-tests without a GPU.
mod pixel {
    /// One plane's disagreement with the picture the compositor should have produced.
    pub(crate) struct PlaneScore {
        pub name: &'static str,
        pub off: f64,
    }

    pub(crate) const COLS: usize = 16;
    pub(crate) const ROWS: usize = 9;
    /// Class of each cell of the compositor's test picture, rows top to bottom. The other
    /// copy, with the colours, is `comp/test_picture.rs` in the gst-wayland-display fork:
    /// change them together.
    pub(crate) const GRID: [&[u8; COLS]; ROWS] = [
        b"3010231302302312",
        b"1202103130213231",
        b"2321031023030302",
        b"1230212131212010",
        b"0312101302321203",
        b"1231230121203030",
        b"3023021310132312",
        b"0310303203210120",
        b"2023210310303031",
    ];
    /// Classes in ascending sample order on Y, Cb and Cr. The four colours rank this way
    /// under BT.601 and BT.709, limited or full range, at least 30 code values apart, so
    /// the score needs no absolute reference: the encode paths do not share one matrix.
    pub(crate) const ORDER: [[usize; 4]; 3] = [[1, 3, 0, 2], [2, 3, 0, 1], [0, 2, 1, 3]];
    /// Half the palette's smallest gap. Classes closer than this on a plane mean the plane
    /// does not carry the picture at all (a flat field, swapped chroma).
    pub(crate) const MIN_GAP: i16 = 15;
    /// A cell is flat and, at the probe's 1280x720, lies on macroblock boundaries, so its
    /// interior is DC-predicted with no residual and reconstruction error is 0-1 code. 8 is
    /// far above that and about half of `MIN_GAP`, so no class reads as another.
    pub(crate) const TOL_SAMPLE: i16 = 8;
    /// 1% of the scored samples: far above any rounding artefact or the cursor, and below
    /// a displaced 16-column band.
    pub(crate) const OFF_FLOOR: f64 = 0.01;
    /// Decoded frames to discard before scoring: decoder warm-up (design §3).
    pub(crate) const SKIP_FRAMES: usize = 5;
    /// Fewer than this many scored frames and the run says nothing (design §7).
    pub(crate) const MIN_SCORED: usize = 5;

    /// `off` = the fraction of a plane's scored samples further than [`TOL_SAMPLE`] from
    /// the median of their own class, or 1.0 when the class medians are not in `order`
    /// [`MIN_GAP`] apart. `rows` are already trimmed to the visible width: reading `stride`
    /// bytes per row scores the padding (gstreamer-gotchas.md). The outer tenth of each
    /// cell is not scored, which keeps chroma siting and deblocking at the edges out.
    pub(crate) fn score_plane(
        name: &'static str,
        rows: &[&[u8]],
        order: &[usize; 4],
    ) -> PlaneScore {
        let (width, height) = (rows.first().map_or(0, |row| row.len()), rows.len());
        let mut seen = [[0usize; 256]; 4];
        for (row, classes) in GRID.iter().enumerate() {
            let (y0, y1) = (row * height / ROWS, (row + 1) * height / ROWS);
            let margin_y = ((y1 - y0) / 10).max(1);
            for (col, class) in classes.iter().enumerate() {
                let (x0, x1) = (col * width / COLS, (col + 1) * width / COLS);
                let margin_x = ((x1 - x0) / 10).max(1);
                let seen = &mut seen[(class - b'0') as usize];
                for line in rows
                    .iter()
                    .take(y1.saturating_sub(margin_y))
                    .skip(y0 + margin_y)
                {
                    for &sample in line
                        .iter()
                        .take(x1.saturating_sub(margin_x))
                        .skip(x0 + margin_x)
                    {
                        seen[sample as usize] += 1;
                    }
                }
            }
        }

        let median = |class: usize| -> i16 {
            let half = seen[class].iter().sum::<usize>() / 2;
            let mut below = 0;
            for (value, count) in seen[class].iter().enumerate() {
                below += count;
                if below > half {
                    return value as i16;
                }
            }
            0
        };
        let medians = [median(0), median(1), median(2), median(3)];
        let ordered = order
            .windows(2)
            .all(|pair| medians[pair[1]] - medians[pair[0]] >= MIN_GAP);
        let total: usize = seen.iter().flatten().sum();
        if !ordered || total == 0 {
            return PlaneScore { name, off: 1.0 };
        }
        let bad: usize = seen
            .iter()
            .zip(medians)
            .flat_map(|(counts, median)| {
                counts
                    .iter()
                    .enumerate()
                    .filter(move |(value, _)| (*value as i16 - median).abs() > TOL_SAMPLE)
                    .map(|(_, count)| *count)
            })
            .sum();
        PlaneScore {
            name,
            off: bad as f64 / total as f64,
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
}

/// Maps a decoded I420 `gst::Sample` to its [`ScoredFrame`], or `None` if the sample
/// cannot be read (a diagnostic gap, not a probe failure — the caller's `MIN_SCORED`
/// floor is what turns "too few usable frames" into Indeterminate).
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

    let y = pixel::score_plane("y", &y_rows, &pixel::ORDER[0]);
    let u = pixel::score_plane("u", &u_rows, &pixel::ORDER[1]);
    let v = pixel::score_plane("v", &v_rows, &pixel::ORDER[2]);
    let (y_off, u_off, v_off) = (y.off, u.off, v.off);
    let (off, planes) = pixel::frame_off(&[y, u, v]);

    Some(ScoredFrame {
        y: y_off,
        u: u_off,
        v: v_off,
        off,
        planes,
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
    /// The codec asked for: a READY failure is `Unsupported` for a codec probe, `Fail`
    /// for the H.264 floor.
    codec: Codec,
    frames: u64,
    encoder: String,
    render_node: String,
    /// The first bus ERROR not attributed to the decode leg, already formatted with its
    /// debug text.
    error: Option<String>,
    /// `error` was posted by the encoder element itself, not another pipeline element.
    error_from_encoder: bool,
    reached: Reached,
    pixel: PixelCheck,
}

/// The encoder element's own open failure, the signature of a device with no encoder for
/// the codec (AMD VCN 3.x AV1). Deliberately narrow: `unsupported` is sticky (nothing
/// re-probes it until the stack's identity changes), so a READY failure posted by any other
/// element, or with no error, or with other text (a transient device loss, an unopenable
/// render node) must stay `fail`.
const ENCODER_OPEN_FAILURE_SIGNATURES: &[&str] = &["Failed to retrieve vulkan encoder"];

/// #311: the codec probe stopped at NULL→READY because the encoder itself could not open.
fn encoder_could_not_open(seen: &Observed) -> bool {
    seen.reached == Reached::NotReady
        && seen.error_from_encoder
        && seen.error.as_deref().is_some_and(|e| {
            ENCODER_OPEN_FAILURE_SIGNATURES
                .iter()
                .any(|sig| e.contains(sig))
        })
}

/// Pass iff the pipeline reached PLAYING, the wanted frames arrived with no bus error, and
/// (h264 only) the decoded picture matched the compositor's test picture. Pure:
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
        let line = format!(
            "{}: the encode pipeline could not reach {state} on {}: {why}",
            seen.encoder, seen.render_node
        );
        if seen.codec != Codec::H264 && encoder_could_not_open(seen) {
            return ProbeVerdict::Unsupported(line);
        }
        return ProbeVerdict::Fail(line);
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
    match build_and_run(&mut cfg, codec, request, |_| {}) {
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
/// consequence (a not-negotiated from the parser), else the first posted. The flag says
/// whether the chosen one came from the encoder.
fn encoder_error_first(errors: Vec<(bool, String)>) -> Option<(bool, String)> {
    let first = errors.first().cloned();
    errors
        .into_iter()
        .find(|(from_encoder, _)| *from_encoder)
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

/// `tamper` is handed the encoder before the pipeline starts. The probe passes a no-op; a
/// test uses it to damage the picture on its way in.
fn build_and_run(
    cfg: &mut SessionConfig,
    codec: Codec,
    request: &MediaProbeRequest,
    tamper: impl FnOnce(&gst::Element),
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
    // the probe's own output so the picture can be checked against the compositor's test
    // picture (#282, #289).
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
    // With no client mapped the compositor would clear to black, and on a flat field a
    // displaced block is identical to the block it replaced (#289).
    let test_picture = decode.is_some()
        && pipeline.by_name("video-source").is_some_and(|source| {
            let has = source.find_property("test-picture").is_some();
            if has {
                source.set_property("test-picture", true);
            }
            has
        });
    tamper(&encoder);

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
            let (error_from_encoder, error) = match encoder_error_first(errors) {
                Some((from_encoder, text)) => (from_encoder, Some(text)),
                None => (false, None),
            };
            teardown(&pipeline);
            return Ok(Observed {
                codec,
                frames: 0,
                encoder: resolved.factory.clone(),
                render_node: cfg.render_node.clone(),
                error,
                error_from_encoder,
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
            // No further than the frames asked for: when scoring a frame takes longer than
            // the source takes to make one, an unbounded drain never returns to send EOS
            // or to check the budget.
            while (decoded_seen as u64) < request.frames {
                let Some(sample) = leg
                    .appsink
                    .try_pull_sample(Some(gst::ClockTime::from_mseconds(0)))
                else {
                    break;
                };
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
    } else if !test_picture {
        PixelCheck::NotRun("this image's compositor has no test picture".into())
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
        codec,
        frames: frames.load(Ordering::Relaxed),
        encoder: resolved.factory.clone(),
        render_node: cfg.render_node.clone(),
        error: failure,
        // Only read at NotReady.
        error_from_encoder: false,
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
            codec: Codec::H264,
            frames,
            encoder: "vulkanh264enc".into(),
            render_node: "/dev/dri/renderD128".into(),
            error: error.map(str::to_string),
            error_from_encoder: false,
            reached: Reached::Playing,
            pixel: PixelCheck::Ok {
                off: 0.0001,
                frames: 20,
            },
        }
    }

    fn observed_with_pixel(frames: u64, pixel: PixelCheck) -> Observed {
        Observed {
            codec: Codec::H264,
            frames,
            encoder: "vulkanh264enc".into(),
            render_node: "/dev/dri/renderD128".into(),
            error: None,
            error_from_encoder: false,
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
                    codec: Codec::H264,
                    frames: 30,
                    encoder: "vulkanh264enc".into(),
                    render_node: "/dev/dri/renderD128".into(),
                    error: Some("boom".into()),
                    error_from_encoder: false,
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

    // ── #282, #289: pure scoring primitives ───────────────────────────────────────────

    /// Limited-range Y, Cb, Cr of the test picture's four colours under each matrix.
    const BT601: [[u8; 3]; 4] = [
        [147, 163, 82],
        [87, 198, 156],
        [177, 51, 121],
        [117, 86, 195],
    ];
    const BT709: [[u8; 3]; 4] = [
        [153, 159, 84],
        [73, 203, 162],
        [188, 49, 115],
        [108, 92, 193],
    ];

    type Planes = [Vec<Vec<u8>>; 3];

    /// The test picture as a 1280x720 4:2:0 frame: Y, Cb and Cr rows, no padding.
    fn picture(levels: &[[u8; 3]; 4]) -> Planes {
        let plane = |p: usize, width: usize, height: usize| -> Vec<Vec<u8>> {
            (0..height)
                .map(|y| {
                    (0..width)
                        .map(|x| {
                            let cell =
                                pixel::GRID[y * pixel::ROWS / height][x * pixel::COLS / width];
                            levels[(cell - b'0') as usize][p]
                        })
                        .collect()
                })
                .collect()
        };
        [plane(0, 1280, 720), plane(1, 640, 360), plane(2, 640, 360)]
    }

    fn score(frame: &Planes) -> (f64, String) {
        let plane = |p: usize, name| {
            let rows: Vec<&[u8]> = frame[p].iter().map(|row| row.as_slice()).collect();
            pixel::score_plane(name, &rows, &pixel::ORDER[p])
        };
        pixel::frame_off(&[plane(0, "y"), plane(1, "u"), plane(2, "v")])
    }

    /// Swaps two bands of `width` luma columns on `planes`: the displacement a tiling or
    /// stride defect produces, with every sample still a valid picture sample.
    fn swap_columns(frame: &mut Planes, planes: &[usize], a: usize, b: usize, width: usize) {
        for &p in planes {
            let sub = if p == 0 { 1 } else { 2 };
            for row in frame[p].iter_mut() {
                for i in 0..width / sub {
                    row.swap(a / sub + i, b / sub + i);
                }
            }
        }
    }

    #[test]
    fn the_test_picture_scores_zero_under_either_matrix_and_with_noise_inside_the_tolerance() {
        assert_eq!(score(&picture(&BT601)), (0.0, String::new()));
        assert_eq!(score(&picture(&BT709)), (0.0, String::new()));

        let mut noisy = picture(&BT601);
        for plane in noisy.iter_mut() {
            for (y, row) in plane.iter_mut().enumerate() {
                for (x, sample) in row.iter_mut().enumerate() {
                    *sample = sample
                        .wrapping_add(((x * 7 + y * 13) % 13) as u8)
                        .wrapping_sub(6);
                }
            }
        }
        assert_eq!(score(&noisy), (0.0, String::new()));
    }

    #[test]
    fn displaced_columns_fail_the_check_on_every_plane() {
        let mut frame = picture(&BT601);
        swap_columns(&mut frame, &[0, 1, 2], 160, 800, 64);
        let (off, planes) = score(&frame);
        // Two of sixteen cells per row hold another cell's colour over 56 of their 64
        // scored columns.
        assert!(
            (off - 2.0 * 56.0 / 64.0 / 16.0).abs() < 0.001,
            "off = {off}"
        );
        assert_eq!(planes, "y,u,v");
    }

    #[test]
    fn a_displacement_in_chroma_alone_names_only_chroma() {
        let mut frame = picture(&BT709);
        swap_columns(&mut frame, &[1, 2], 160, 800, 64);
        let (off, planes) = score(&frame);
        assert!(off > 10.0 * pixel::OFF_FLOOR, "off = {off}");
        assert_eq!(planes, "u,v");
    }

    #[test]
    fn a_picture_shifted_by_one_macroblock_fails() {
        let mut frame = picture(&BT601);
        frame[0].iter_mut().for_each(|row| row.rotate_right(16));
        frame[1].iter_mut().for_each(|row| row.rotate_right(8));
        frame[2].iter_mut().for_each(|row| row.rotate_right(8));
        let (off, planes) = score(&frame);
        // Each cell's first 8 scored columns of 64 come from its left neighbour, which is
        // another class everywhere but in three cells the rotation wraps onto their own.
        assert!(
            (off - 141.0 / 144.0 * 8.0 / 64.0).abs() < 0.001,
            "off = {off}"
        );
        assert_eq!(planes, "y,u,v");
    }

    #[test]
    fn a_plane_that_does_not_carry_the_picture_is_wholly_off() {
        // The flat black field the compositor shows without the test picture.
        let black = [[16, 128, 128]; 4];
        assert_eq!(score(&picture(&black)), (1.0, "y,u,v".to_string()));

        // The #272 signature: chroma zeroed, luma intact.
        let mut green = picture(&BT601);
        for plane in &mut green[1..] {
            plane.iter_mut().for_each(|row| row.fill(0));
        }
        assert_eq!(score(&green), (1.0, "u,v".to_string()));

        let mut swapped = picture(&BT601);
        swapped.swap(1, 2);
        assert_eq!(score(&swapped), (1.0, "u,v".to_string()));
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

    // ── #300: the codec probe (a non-H.264 request) ──────────────────────────────────

    fn observed_hevc(frames: u64, reached: Reached, error: Option<&str>) -> Observed {
        Observed {
            codec: Codec::H265,
            frames,
            encoder: "vulkanh265enc".into(),
            render_node: "/dev/dri/renderD129".into(),
            error: error.map(str::to_string),
            error_from_encoder: false,
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

    /// The live AMD VCN 3.x AV1 evidence (#300): the encoder's own open failure.
    const AV1_OPEN_FAILURE: &str = "/GstPipeline:pipeline0/GstVulkanAV1Encoder:quasar-vulkan-encoder: Failed to retrieve vulkan encoder (../ext/vulkan/vkav1enc.c(1759): gst_vulkan_av1_encoder_open ())";

    fn av1_not_ready(error: Option<&str>, from_encoder: bool) -> Observed {
        Observed {
            codec: Codec::Av1,
            encoder: "vulkanav1enc".into(),
            error_from_encoder: from_encoder,
            ..observed_hevc(0, Reached::NotReady, error)
        }
    }

    /// #311: the device has no encoder for the codec — a hardware fact, reported as
    /// `unsupported` (exit 4) with the same evidence a fail would carry.
    #[test]
    fn a_codec_probe_that_cannot_reach_ready_is_unsupported_carrying_the_evidence() {
        let v = verdict(10, &av1_not_ready(Some(AV1_OPEN_FAILURE), true));
        assert!(matches!(v, ProbeVerdict::Unsupported(_)), "{v:?}");
        assert_eq!(v.exit_code(), UNSUPPORTED_EXIT);
        assert_eq!(v.exit_code(), 4);
        assert!(v.line().contains("could not reach READY"), "{}", v.line());
        assert!(
            v.line().contains("Failed to retrieve vulkan encoder"),
            "{}",
            v.line()
        );
        assert!(!v.line().contains('\n'));
        assert_eq!(v.remediation(), None);
    }

    /// `unsupported` is sticky, so a READY failure that is not the encoder's own open
    /// failure stays `fail`: another element's error (the compositor source, the upload,
    /// the render node), no error at all, or other text from the encoder.
    #[test]
    fn a_ready_failure_that_is_not_the_encoders_open_failure_stays_a_fail() {
        for seen in [
            av1_not_ready(
                Some("/GstPipeline:pipeline0/GstWaylandDisplaySrc:src: Failed to open render node /dev/dri/renderD129"),
                false,
            ),
            av1_not_ready(
                Some("/GstPipeline:pipeline0/GstVulkanUpload:vulkanupload0: Failed to create vulkan device (VK_ERROR_DEVICE_LOST)"),
                false,
            ),
            // The encoder's text, but posted by another element.
            av1_not_ready(Some(AV1_OPEN_FAILURE), false),
            av1_not_ready(None, false),
            av1_not_ready(None, true),
            av1_not_ready(Some("quasar-vulkan-encoder: device lost (VK_ERROR_DEVICE_LOST)"), true),
        ] {
            let v = verdict(10, &seen);
            assert!(matches!(v, ProbeVerdict::Fail(_)), "{v:?}");
            assert_eq!(v.exit_code(), 1);
        }
    }

    /// H.264 is the floor: its media probe stuck at READY is a real fault that blocks
    /// the GPU, so it stays `Fail` (#311).
    #[test]
    fn the_h264_media_probe_that_cannot_reach_ready_stays_a_fail() {
        let v = verdict(
            30,
            &Observed {
                reached: Reached::NotReady,
                frames: 0,
                pixel: PixelCheck::NotRun("the encode pipeline never started".into()),
                error_from_encoder: true,
                ..observed(
                    0,
                    Some("quasar-vulkan-encoder: Failed to retrieve vulkan encoder"),
                )
            },
        );
        assert!(matches!(v, ProbeVerdict::Fail(_)), "{v:?}");
        assert_eq!(v.exit_code(), 1);
        assert!(v.line().contains("could not reach READY"), "{}", v.line());
    }

    /// A codec probe that got past READY and then failed is a real problem, not a
    /// missing encoder: it stays `Fail` (#311).
    #[test]
    fn a_codec_probe_failing_after_ready_stays_a_fail() {
        for seen in [
            observed_hevc(0, Reached::NotPlaying, Some("vulkanh265enc0: device lost")),
            observed_hevc(10, Reached::Playing, Some("vulkanh265enc0: device lost")),
            observed_hevc(3, Reached::Playing, None),
        ] {
            let v = verdict(10, &seen);
            assert!(matches!(v, ProbeVerdict::Fail(_)), "{v:?}");
            assert_eq!(v.exit_code(), 1);
        }
    }

    #[test]
    fn a_pipeline_that_reached_ready_but_not_playing_is_a_fail_naming_playing() {
        let v = verdict(10, &observed_hevc(0, Reached::NotPlaying, None));
        assert_eq!(v.exit_code(), 1);
        assert!(v.line().contains("could not reach PLAYING"), "{}", v.line());
    }

    /// The exit-code mapping for a non-H.264 request: the pixel check's absence is no
    /// longer indeterminate (3), so the child answers pass (0), fail (1) or, when the
    /// encoder cannot open, unsupported (4).
    #[test]
    fn a_non_h264_request_is_never_indeterminate() {
        for (seen, code) in [
            (observed_hevc(10, Reached::Playing, None), 0),
            (observed_hevc(3, Reached::Playing, None), 1),
            (observed_hevc(10, Reached::Playing, Some("boom")), 1),
            (observed_hevc(0, Reached::NotPlaying, None), 1),
            (observed_hevc(0, Reached::NotReady, None), 1),
            (av1_not_ready(Some(AV1_OPEN_FAILURE), true), 4),
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
            Some((true, "vulkanh265enc0: no encode profile".into()))
        );
        assert_eq!(
            encoder_error_first(vec![(false, "first".into()), (false, "second".into())]),
            Some((false, "first".into()))
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
    // ── #289: the probe path itself, on the software encoder ─────────────────────────

    /// One compositor at a time: each binds a Wayland socket and starts llvmpipe.
    static SERIAL: std::sync::Mutex<()> = std::sync::Mutex::new(());

    /// Runs the probe's pipeline on the software path (llvmpipe compositor, openh264), or
    /// returns `None` where this image cannot: the devtools image has no compositor
    /// element. Like the live WebRTC tests, with `QUASAR_REQUIRE_LIVE_WEBRTC` set a skip is
    /// a failure.
    fn software_probe(tamper: impl FnOnce(&gst::Element)) -> Option<Observed> {
        gst::init().unwrap();
        let mut missing: Vec<&str> = ["waylanddisplaysrc", "openh264enc", "openh264dec"]
            .into_iter()
            .filter(|f| gst::ElementFactory::find(f).is_none())
            .collect();
        // The compositor binds its Wayland socket there and panics without it.
        let has_runtime_dir = std::env::var_os("XDG_RUNTIME_DIR")
            .is_some_and(|dir| std::path::Path::new(&dir).is_dir());
        if !has_runtime_dir {
            missing.push("a directory at $XDG_RUNTIME_DIR");
        }
        if !missing.is_empty() {
            if std::env::var_os("QUASAR_REQUIRE_LIVE_WEBRTC").is_some() {
                panic!(
                    "the media probe's software path cannot run: {missing:?} missing, and \
                     QUASAR_REQUIRE_LIVE_WEBRTC is set"
                );
            }
            // Past libtest's capture, like the live WebRTC tests' skip: a skip nobody sees
            // reads as a pass.
            use std::io::Write;
            let _ = writeln!(
                std::io::stderr(),
                "SKIP — session::probe_media (#289): the media probe's software-path test did \
                 NOT run, {missing:?} missing; it runs in the quasar-agent-dev container"
            );
            return None;
        }

        let _one = SERIAL.lock().unwrap_or_else(|e| e.into_inner());
        let request = MediaProbeRequest::default();
        let mut cfg = SessionConfig::from_env(false, None);
        cfg.encoder = crate::session::EncoderChoice::Openh264;
        cfg.configured_encoder = cfg.encoder;
        cfg.render_node = "software".into();
        cfg.stream.codec = Codec::H264;
        (cfg.stream.width, cfg.stream.height, cfg.stream.fps) =
            (request.width, request.height, request.fps);
        Some(build_and_run(&mut cfg, Codec::H264, &request, tamper).expect("the probe builds"))
    }

    /// Swaps two 64-column bands of every I420 frame on its way into the encoder.
    fn displace_columns(encoder: &gst::Element) {
        let pad = encoder.static_pad("sink").unwrap();
        pad.add_probe(gst::PadProbeType::BUFFER, |pad, info| {
            let caps = pad.current_caps().expect("caps before the first buffer");
            let video = gst_video::VideoInfo::from_caps(&caps).unwrap();
            let buffer = info.buffer_mut().unwrap().make_mut();
            let mut frame =
                gst_video::VideoFrameRef::from_buffer_ref_writable(buffer, &video).unwrap();
            for plane in 0..3u32 {
                let sub = if plane == 0 { 1 } else { 2 };
                let stride = frame.plane_stride()[plane as usize] as usize;
                let data = frame.plane_data_mut(plane).unwrap();
                for line in data.chunks_mut(stride) {
                    for i in 0..64 / sub {
                        line.swap(160 / sub + i, 800 / sub + i);
                    }
                }
            }
            gst::PadProbeReturn::Ok
        });
    }

    #[test]
    fn the_probe_path_passes_untouched_and_fails_with_displaced_columns() {
        let Some(healthy) = software_probe(|_| {}) else {
            return;
        };
        let PixelCheck::Ok { off, frames } = healthy.pixel else {
            panic!("a healthy software path must pass: {:?}", healthy.pixel);
        };
        eprintln!("healthy: worst plane {off:.6} over {frames} frames");
        assert!(off < pixel::OFF_FLOOR / 10.0, "off = {off}");
        assert!(matches!(
            verdict(DEFAULT_FRAMES, &healthy),
            ProbeVerdict::Pass(_)
        ));

        let displaced = software_probe(displace_columns).unwrap();
        let PixelCheck::Mismatch {
            off,
            frames,
            planes,
        } = &displaced.pixel
        else {
            panic!("displaced columns must fail: {:?}", displaced.pixel);
        };
        eprintln!("displaced: worst plane {off:.6} over {frames} frames, planes {planes}");
        assert!(*off > 5.0 * pixel::OFF_FLOOR, "off = {off}");
        assert_eq!(planes, "y,u,v");
        assert!(matches!(
            verdict(DEFAULT_FRAMES, &displaced),
            ProbeVerdict::Mismatch(_)
        ));
    }
}
