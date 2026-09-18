//! `quasar-node-agent media-probe`, the child half of the media host probe (spec #252
//! "Host probes"): composite and encode a few frames on one GPU, answer in one line.
//!
//! Built from the production source, convert, encoder and bitstream builders. Anything a
//! probe adds that a session lacks is a failure mode sessions do not have.
//! Parent half: `crate::host_probe::media`.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use gstreamer as gst;
use gstreamer::prelude::*;

use super::{Codec, SessionConfig};

pub const DEFAULT_FRAMES: u64 = 30;
pub const DEFAULT_BUDGET_SECS: u64 = 15;

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

/// The child's single stdout line plus its exit code.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ProbeVerdict {
    Pass(String),
    Fail(String),
    /// Bad argv — distinct from a fail, so a caller cannot read it as "this GPU cannot
    /// encode".
    Usage(String),
}

impl ProbeVerdict {
    pub fn line(&self) -> &str {
        match self {
            ProbeVerdict::Pass(s) | ProbeVerdict::Fail(s) | ProbeVerdict::Usage(s) => s,
        }
    }

    pub fn exit_code(&self) -> i32 {
        match self {
            ProbeVerdict::Pass(_) => 0,
            ProbeVerdict::Fail(_) => 1,
            ProbeVerdict::Usage(_) => 2,
        }
    }
}

/// What the run observed, so the verdict wording is decided in one pure place.
struct Observed {
    frames: u64,
    encoder: String,
    render_node: String,
    /// The first bus ERROR, already formatted with its debug text.
    error: Option<String>,
}

/// Pass iff the wanted frames arrived with no bus error. Pure: unit-tested without a GPU.
fn verdict(wanted: u64, seen: &Observed) -> ProbeVerdict {
    if let Some(error) = &seen.error {
        return ProbeVerdict::Fail(format!("{}: {error}", seen.encoder));
    }
    if seen.frames < wanted {
        return ProbeVerdict::Fail(format!(
            "{}: only {} of {wanted} frames encoded on {} within the probe budget",
            seen.encoder, seen.frames, seen.render_node
        ));
    }
    ProbeVerdict::Pass(format!(
        "encoded {} frames with {} on {}",
        seen.frames, seen.encoder, seen.render_node
    ))
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
    let sink = gst::ElementFactory::make("fakesink")
        .property("sync", false)
        .build()
        .context("fakesink not found")?;

    pipeline
        .add_many([&encoder, &profile_caps, &parser, &sink])
        .context("adding the encode chain")?;
    if !convert.is_empty() {
        pipeline.add_many(convert.iter())?;
    }
    let mut chain: Vec<&gst::Element> = vec![&tail];
    chain.extend(convert.iter());
    chain.extend_from_slice(&[&encoder, &profile_caps, &parser, &sink]);
    gst::Element::link_many(chain).context("linking the probe encode chain")?;

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

    let observe = |error: Option<String>| Observed {
        frames: frames.load(Ordering::Relaxed),
        encoder: resolved.factory.clone(),
        render_node: cfg.render_node.clone(),
        error,
    };
    let teardown = |pipeline: &gst::Pipeline| {
        // NULL on every path: a probe that leaves an encoder session open is worse than
        // one that fails.
        let _ = pipeline.set_state(gst::State::Null);
    };

    if let Err(e) = pipeline.set_state(gst::State::Playing) {
        teardown(&pipeline);
        return Err(anyhow::anyhow!(
            "the probe pipeline never reached PLAYING: {e}"
        ));
    }

    let bus = match pipeline.bus() {
        Some(bus) => bus,
        None => {
            teardown(&pipeline);
            anyhow::bail!("the probe pipeline has no bus");
        }
    };
    let deadline = Instant::now() + request.budget;
    let mut failure: Option<String> = None;
    while Instant::now() < deadline {
        if frames.load(Ordering::Relaxed) >= request.frames {
            break;
        }
        let Some(msg) = bus.timed_pop(gst::ClockTime::from_mseconds(100)) else {
            continue;
        };
        match msg.view() {
            gst::MessageView::Eos(_) => break,
            gst::MessageView::Error(e) => {
                let from = msg
                    .src()
                    .map(|s| s.path_string().to_string())
                    .unwrap_or_else(|| "pipeline".into());
                failure = Some(format!(
                    "{from}: {} ({})",
                    e.error(),
                    e.debug().unwrap_or_default()
                ));
                break;
            }
            _ => {}
        }
    }

    let seen = observe(failure);
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
        }
    }

    #[test]
    fn enough_frames_and_no_error_passes_naming_the_encoder_and_node() {
        assert_eq!(
            verdict(30, &observed(30, None)),
            ProbeVerdict::Pass(
                "encoded 30 frames with vulkanh264enc on /dev/dri/renderD128".into()
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
}
