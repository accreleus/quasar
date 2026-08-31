//! `quasar-node-agent probe-encoder` — ask the encoder branch what it negotiates, through
//! the code production uses.
//!
//! A hand-typed gst-launch probe is a different graph than production and can lie (see
//! `.claude/rules/gstreamer-gotchas.md` "Standalone Vulkan HEVC" — that's exactly how the
//! 2026-08-22 `main-444` false regression happened). So this probe shares the production
//! encoder resolution ([`pipeline::resolve_effective_encoder`]), builder, and bitstream
//! chain ([`codec_chain::build_bitstream_chain`], which carries `profile=main`). The only
//! difference is the source: `videotestsrc` stands in for the compositor, feeding the same
//! `caps::encoder_input_caps_for` the scale stage emits.

use anyhow::{Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;

use super::{Codec, EncoderChoice, SessionConfig};

/// What the probe found. Serialized verbatim by `--json`.
#[derive(Debug, serde::Serialize)]
pub struct ProbeReport {
    pub codec: String,
    /// The element `resolve_effective_encoder` chose, including a vendor-HW fallback —
    /// `QUASAR_VULKAN_HEVC=0` reporting `nvcudah265enc` here is the answer, not a failure.
    pub encoder_factory: Option<String>,
    pub configured_encoder: String,
    pub effective_encoder: String,
    pub negotiated_sink_caps: Option<String>,
    pub negotiated_src_caps: Option<String>,
    /// The live profile. `main` for h265 on every production path; `null` for av1,
    /// which carries no profile caps field.
    pub profile: Option<String>,
    pub level: Option<String>,
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

fn caps_field(caps: Option<&gst::Caps>, field: &str) -> Option<String> {
    caps?.structure(0)?.get::<String>(field).ok()
}

/// Gets `videotestsrc`'s system-memory frames into the memory the encoder wants.
/// Production gets these from the compositor already in GPU memory, so this stage has
/// no production twin — it exists only to satisfy the shared `caps::encoder_input_caps_for`.
fn upload_element(choice: EncoderChoice) -> Result<Option<gst::Element>> {
    let factory = match choice {
        EncoderChoice::Vulkan => "vulkanupload",
        EncoderChoice::Nvenc => "cudaupload",
        EncoderChoice::Va => "vapostproc",
        EncoderChoice::Openh264 => return Ok(None),
    };
    gst::ElementFactory::make(factory)
        .build()
        .map(Some)
        .with_context(|| format!("{factory} not found — is this the right image?"))
}

/// Build `videotestsrc ! … ! enc ! <profile capsfilter> ! parser ! fakesink`, run it for
/// `seconds`, and report what the encoder actually negotiated.
pub fn run(codec: Codec, width: i32, height: i32, fps: i32, seconds: u64) -> ProbeReport {
    let mut report = ProbeReport {
        codec: codec.as_str().to_string(),
        encoder_factory: None,
        configured_encoder: String::new(),
        effective_encoder: String::new(),
        negotiated_sink_caps: None,
        negotiated_src_caps: None,
        profile: None,
        level: None,
        ok: false,
        error: None,
    };
    match build_and_run(codec, width, height, fps, seconds, &mut report) {
        Ok(()) => report.ok = true,
        Err(e) => report.error = Some(format!("{e:#}")),
    }
    report
}

fn build_and_run(
    codec: Codec,
    width: i32,
    height: i32,
    fps: i32,
    seconds: u64,
    report: &mut ProbeReport,
) -> Result<()> {
    // Same env-derived config production builds; only codec and size are substituted.
    let mut cfg = SessionConfig::from_env(true, None);
    cfg.stream.codec = codec;
    cfg.stream.width = width;
    cfg.stream.height = height;
    cfg.stream.fps = fps;
    report.configured_encoder = format!("{:?}", cfg.encoder).to_lowercase();

    let resolved = super::pipeline::resolve_effective_encoder(&mut cfg)?;
    report.effective_encoder = format!("{:?}", cfg.encoder).to_lowercase();
    report.encoder_factory = Some(resolved.factory.clone());

    let pipeline = gst::Pipeline::new();
    let src = gst::ElementFactory::make("videotestsrc")
        .property("num-buffers", (fps as u64 * seconds).max(1) as i32)
        .property_from_str("pattern", "smpte")
        .build()
        .context("videotestsrc not found")?;
    let convert = gst::ElementFactory::make("videoconvert")
        .build()
        .context("videoconvert not found")?;
    let raw_caps = gst::ElementFactory::make("capsfilter")
        .property(
            "caps",
            gst::Caps::builder("video/x-raw")
                .field("format", "NV12")
                .field("width", width)
                .field("height", height)
                .field("framerate", gst::Fraction::new(fps, 1))
                .build(),
        )
        .build()
        .context("capsfilter not found")?;
    let upload = upload_element(cfg.encoder)?;
    let enc_in_caps = gst::ElementFactory::make("capsfilter")
        .property(
            "caps",
            super::pipeline::encoder_input_caps_for(cfg.encoder, width, height, fps),
        )
        .build()
        .context("capsfilter not found")?;
    let encoder = super::pipeline::build_encoder_element_for_probe(&cfg, codec, &resolved.factory)?;
    // Production bitstream chain: its capsfilter is what pins `profile=main` for h265.
    let (profile_caps, parser) = super::pipeline::build_bitstream_chain_for_probe(codec, &cfg)?;
    let sink = gst::ElementFactory::make("fakesink")
        .property("sync", false)
        .build()
        .context("fakesink not found")?;

    let mut elements: Vec<&gst::Element> = vec![&src, &raw_caps, &convert];
    if let Some(u) = upload.as_ref() {
        elements.push(u);
    }
    elements.push(&enc_in_caps);
    elements.push(&encoder);
    elements.push(&profile_caps);
    elements.push(&parser);
    elements.push(&sink);
    pipeline.add_many(&elements)?;
    gst::Element::link_many(&elements)?;

    pipeline
        .set_state(gst::State::Playing)
        .context("probe pipeline failed to reach PLAYING")?;

    // Bounded: the requested run plus generous slack for device init, then read whatever
    // was negotiated. A probe that hangs is a probe nobody runs.
    let bus = pipeline.bus().context("probe pipeline has no bus")?;
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(seconds + 20);
    let mut failure: Option<String> = None;
    while std::time::Instant::now() < deadline {
        let Some(msg) = bus.timed_pop(gst::ClockTime::from_mseconds(200)) else {
            continue;
        };
        match msg.view() {
            gst::MessageView::Eos(_) => break,
            gst::MessageView::Error(e) => {
                failure = Some(format!("{} ({:?})", e.error(), e.debug()));
                break;
            }
            _ => {}
        }
    }

    let sink_caps = encoder.static_pad("sink").and_then(|p| p.current_caps());
    let src_caps = encoder.static_pad("src").and_then(|p| p.current_caps());
    report.negotiated_sink_caps = sink_caps.as_ref().map(|c| c.to_string());
    report.negotiated_src_caps = src_caps.as_ref().map(|c| c.to_string());
    report.profile = caps_field(src_caps.as_ref(), "profile");
    report.level = caps_field(src_caps.as_ref(), "level");
    pipeline.set_state(gst::State::Null).ok();

    match failure {
        Some(e) => Err(anyhow::anyhow!("probe pipeline error: {e}")),
        None if report.negotiated_src_caps.is_none() => Err(anyhow::anyhow!(
            "the encoder never negotiated output caps within the probe window"
        )),
        None => Ok(()),
    }
}
