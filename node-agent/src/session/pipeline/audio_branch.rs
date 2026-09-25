//! The GStreamer audio encode branch (pulsesrc/silent fallback → Opus → RTP).
//!
//! Distinct from `super::super::audio`, which owns the per-session PulseAudio sidecar
//! process lifecycle; this builds the in-pipeline capture/encode elements.

use anyhow::{Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;

use crate::session::audio::QUASAR_MONITOR_SOURCE_NAME;
use crate::session::SessionConfig;

/// Add + link the audio encode chain into `pipeline`, returning its tail capsfilter for
/// linking into webrtcbin. Shared by the single and split pipelines, so the audio path is
/// identical in both.
pub(super) fn add_audio_chain(
    pipeline: &gst::Pipeline,
    cfg: &SessionConfig,
) -> Result<gst::Element> {
    let audio_src = if cfg.use_test_audio {
        // Pulse is optional, so keep the m-line alive without it. NEVER substitute a
        // synthetic tone for an application's audio: that makes the recovery path an
        // audible product defect.
        tracing::info!("audio source: audiotestsrc wave=silence (fallback)");
        gst::ElementFactory::make("audiotestsrc")
            .property("is-live", true)
            .property_from_str("wave", "silence")
            .build()
            .context("audiotestsrc not found")?
    } else {
        let mut builder = gst::ElementFactory::make("pulsesrc");
        let device = QUASAR_MONITOR_SOURCE_NAME;
        if let Some(server) = &cfg.pulse_server {
            tracing::info!("audio source: pulsesrc server={server} device={device}");
            builder = builder.property("server", server.as_str());
        } else {
            tracing::info!("audio source: pulsesrc (default server) device={device}");
        }
        // Pin the capture device EXPLICITLY, never the daemon's default source: the
        // sidecar bakes a second sink (`quasar_mic`) and a remap-source, either of which
        // could move the default and silently redirect host-audio capture at the client's
        // own microphone feed.
        builder = builder.property("device", device);
        if audio_no_clock() {
            tracing::info!("pulsesrc provide-clock=false, do-timestamp=false (QUASAR_AUDIO_NO_CLOCK=1 — #304 Test 2)");
            builder = builder.property("provide-clock", false);
            builder = builder.property("do-timestamp", false);
        }
        builder
            .build()
            .context("pulsesrc not found — is pulseaudio / gst-plugins-good installed?")?
    };
    let audio_convert = gst::ElementFactory::make("audioconvert")
        .build()
        .context("audioconvert not found")?;
    let audio_resample = gst::ElementFactory::make("audioresample")
        .build()
        .context("audioresample not found")?;
    // The Opus wire format (`opus/48000/2`), not opusenc's own caps fixation. A passthrough
    // while the capture sink stays pinned to the same format (`audio::pulse_command`).
    let audio_raw_caps = gst::ElementFactory::make("capsfilter")
        .property(
            "caps",
            gst::Caps::builder("audio/x-raw")
                .field("rate", 48_000_i32)
                .field("channels", 2_i32)
                .build(),
        )
        .build()
        .context("capsfilter not found")?;
    // Low-delay Opus: 10 ms frames halve per-packet audio latency vs the 20 ms default,
    // restricted-lowdelay drops the codec's algorithmic look-ahead (CELT-only), and CBR
    // keeps the bitrate envelope flat for the congestion controller. Enum-typed properties
    // MUST use `property_from_str` (.claude/rules/gstreamer-gotchas.md).
    let opus_enc = gst::ElementFactory::make("opusenc")
        .property_from_str("frame-size", "10")
        .property_from_str("audio-type", "restricted-lowdelay")
        .property_from_str("bitrate-type", "cbr")
        .build()
        .context("opusenc not found — is the GStreamer Opus plugin installed?")?;
    let rtp_opus_pay = gst::ElementFactory::make("rtpopuspay")
        .property("pt", 111_u32)
        .build()
        .context("rtpopuspay not found")?;
    let audio_rtp_caps = gst::Caps::builder("application/x-rtp")
        .field("media", "audio")
        .field("encoding-name", "OPUS")
        .field("payload", 111_i32)
        .field("clock-rate", 48_000_i32)
        .build();
    let audio_rtp_capsfilter = gst::ElementFactory::make("capsfilter")
        .property("caps", &audio_rtp_caps)
        .build()
        .context("capsfilter not found")?;

    pipeline.add_many([
        &audio_src,
        &audio_convert,
        &audio_resample,
        &audio_raw_caps,
        &opus_enc,
        &rtp_opus_pay,
        &audio_rtp_capsfilter,
    ])?;
    gst::Element::link_many([
        &audio_src,
        &audio_convert,
        &audio_resample,
        &audio_raw_caps,
        &opus_enc,
        &rtp_opus_pay,
        &audio_rtp_capsfilter,
    ])
    .context("failed to link audio encode chain")?;
    Ok(audio_rtp_capsfilter)
}

/// Knob: `QUASAR_AUDIO_NO_CLOCK`. #304: sets `provide-clock=false` +
/// `do-timestamp=false` on pulsesrc so the audio source cannot drive the pipeline clock
/// and skew video RTP timestamps.
fn audio_no_clock() -> bool {
    matches!(
        std::env::var("QUASAR_AUDIO_NO_CLOCK").ok().as_deref(),
        Some("1") | Some("true") | Some("TRUE")
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::settings::RuntimeSettings;
    use crate::session::StreamParams;

    fn test_audio_cfg() -> SessionConfig {
        let settings = RuntimeSettings::baseline_with(&|_| None);
        let stream = StreamParams {
            width: 1280,
            height: 720,
            fps: 30,
            bitrate_kbps: 4000,
            h264_profile: "constrained-baseline".to_string(),
            codec: crate::session::Codec::H264,
            abr_floor_kbps: 0,
            mic: false,
        };
        let mut cfg = SessionConfig::for_assignment_with(&settings, stream, None);
        cfg.use_test_audio = true;
        cfg
    }

    #[test]
    fn opus_encoder_input_is_the_wire_format() {
        gst::init().unwrap();
        let pipeline = gst::Pipeline::new();
        let tail = add_audio_chain(&pipeline, &test_audio_cfg()).expect("audio chain builds");
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        pipeline.add(&sink).unwrap();
        tail.link(&sink).unwrap();
        let opus_sink_pad = pipeline
            .iterate_elements()
            .into_iter()
            .filter_map(Result::ok)
            .find(|e| e.factory().is_some_and(|f| f.name() == "opusenc"))
            .and_then(|e| e.static_pad("sink"))
            .expect("opusenc in the chain");
        pipeline.set_state(gst::State::Playing).unwrap();
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        let caps = loop {
            if let Some(caps) = opus_sink_pad.current_caps() {
                break caps;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "opusenc never negotiated"
            );
            std::thread::sleep(std::time::Duration::from_millis(20));
        };
        pipeline.set_state(gst::State::Null).unwrap();
        let s = caps.structure(0).unwrap();
        assert_eq!(s.get::<i32>("rate").unwrap(), 48_000);
        assert_eq!(s.get::<i32>("channels").unwrap(), 2);
    }
}
