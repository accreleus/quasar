//! The microphone RECEIVE branch (client → host): a `recvonly` Opus m-line on the
//! audio PeerConnection, decoded into the session's PulseAudio sidecar.
//!
//! The only inbound-media path in the agent (everything else is send-only), so its two
//! mechanisms are isolated here:
//!
//! 1. an explicit `add-transceiver(RECVONLY, caps)` on the AUDIO `webrtcbin`, emitted
//!    BEFORE the first offer: `protocol/signaling.md` §Microphone m-line makes the mic
//!    m-line an up-front, never-renegotiated part of the single audio offer, leaving the
//!    single-offer guard in `webrtc.rs` untouched. Mid-session mute/unmute is a
//!    client-local `replaceTrack`, with no signaling.
//! 2. a `pad-added` handler linking the inbound RTP pad into a pre-built bin:
//!
//! ```text
//! rtpopusdepay → opusdec(plc=true) → audioconvert → audioresample
//!   → pulsesink server=<sidecar> device=quasar_mic sync=false async=false
//! ```
//!
//! The bin MUST live in the same `gst::Pipeline` as the audio webrtcbin: a receive branch
//! stranded in a pipeline of its own is the inverse of the #304 two-webrtcbins rule. It is
//! added and linked up-front, so only the one pad link is left when mic packets arrive.
//!
//! Audio lands in the sidecar's `quasar_mic` null sink; the app container records its
//! remapped monitor (`quasar_mic_src`, injected as `PULSE_SOURCE`). The agent never records
//! or persists mic audio — the path is decode → pulsesink only.

use anyhow::{Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_webrtc::WebRTCRTPTransceiverDirection;

use crate::session::audio::QUASAR_MIC_SINK_NAME;

/// The mic receive bin's element name. The `pad-added` handler looks the bin up by name
/// rather than capturing it, so the closure on `webrtcbin` holds no strong element handles
/// (the ref-cycle rule, `.claude/rules/gstreamer-gotchas.md`).
pub(super) const MIC_BIN_NAME: &str = "quasar-mic-branch";

/// The terminal `pulsesink`'s name, for `verify_mic_routing` to find post-link.
const MIC_SINK_NAME: &str = "quasar-mic-sink";

/// The mic m-line's RTP payload type. Matches the send-side `rtpopuspay pt=111`
/// (`audio_branch.rs`) and the browser's default, so the answer never has to remap.
const MIC_OPUS_PT: i32 = 111;

/// The `recvonly` transceiver's offered caps. Static, so the m-line is fully described in
/// the first offer.
fn mic_rtp_caps() -> gst::Caps {
    // `encoding-params = "2"` emits the rtpmap channel count. Chrome's payload matcher
    // only knows `opus/48000/2`; a bare `OPUS/48000` finds no common codec and the m-line
    // comes back rejected (port 0) — unrecoverable, since each PC offers exactly once.
    gst::Caps::builder("application/x-rtp")
        .field("media", "audio")
        .field("encoding-name", "OPUS")
        .field("clock-rate", 48_000_i32)
        .field("encoding-params", "2")
        .field("payload", MIC_OPUS_PT)
        .build()
}

/// The mic receive chain's factories, in link order. No `pulse_server` degrades the
/// terminal to `fakesink`: the m-line still negotiates and the client's mic is discarded
/// rather than failing the session. Pure, so the chain shape is testable with no registry.
fn mic_chain_factories(has_pulse: bool) -> Vec<&'static str> {
    vec![
        "rtpopusdepay",
        "opusdec",
        "audioconvert",
        "audioresample",
        if has_pulse { "pulsesink" } else { "fakesink" },
    ]
}

/// Add the `recvonly` mic transceiver to `webrtc` and the decode bin to `pipeline`, and
/// wire the `pad-added` link between them.
///
/// MUST be called before the audio pipeline leaves NULL and before any negotiation: the
/// mic m-line has to be in the first (and only) audio offer. `pipeline` must be the one
/// `webrtc` lives in (the #304 audio pipeline).
pub(super) fn add_mic_branch(
    pipeline: &gst::Pipeline,
    webrtc: &gst::Element,
    pulse_server: Option<&str>,
) -> Result<()> {
    let bin = build_mic_bin(pulse_server)?;
    pipeline
        .add(&bin)
        .context("failed to add the microphone bin to the audio pipeline")?;

    // What puts the mic m-line in the offer. The direction enum is NOT feature-gated in
    // gstreamer-webrtc 0.23, so it is set typed. The returned transceiver is dropped:
    // nothing may touch it mid-session, since a direction change is a renegotiation.
    let caps = mic_rtp_caps();
    let transceiver = webrtc.emit_by_name::<Option<gstreamer_webrtc::WebRTCRTPTransceiver>>(
        "add-transceiver",
        &[&WebRTCRTPTransceiverDirection::Recvonly, &caps],
    );
    match transceiver {
        Some(_) => tracing::info!(
            "microphone: added recvonly Opus transceiver (pt={MIC_OPUS_PT}, 48 kHz) \
             to the audio PC"
        ),
        // Not fatal, but without the transceiver there is no mic m-line in the offer, and
        // a silent mic is otherwise only diagnosable from the browser side.
        None => tracing::warn!(
            token = "mic-transceiver-missing",
            "microphone: add-transceiver returned nothing — the offer will carry NO mic m-line"
        ),
    }

    // Link the inbound track when it arrives. Only a weak pipeline ref is captured; the
    // bin is looked up by name inside.
    let pipeline_weak = pipeline.downgrade();
    webrtc.connect_pad_added(move |_webrtc, pad| {
        if pad.direction() != gst::PadDirection::Src {
            return;
        }
        let Some(pipeline) = pipeline_weak.upgrade() else {
            return;
        };
        let caps = pad.current_caps().or_else(|| pad.allowed_caps());
        if !caps_are_audio(caps.as_ref()) {
            tracing::warn!(
                token = "mic-nonaudio-pad",
                "microphone: ignoring non-audio inbound pad '{}' (caps={:?})",
                pad.name(),
                caps
            );
            return;
        }
        let Some(bin) = pipeline.by_name(MIC_BIN_NAME) else {
            tracing::warn!(
                token = "mic-receive-bin-gone",
                "microphone: receive bin '{MIC_BIN_NAME}' is gone — inbound pad dropped"
            );
            return;
        };
        let Some(sink_pad) = bin.static_pad("sink") else {
            tracing::warn!(
                token = "mic-receive-bin-no-sink",
                "microphone: receive bin has no 'sink' pad — inbound pad dropped"
            );
            return;
        };
        if sink_pad.is_linked() {
            // One recvonly m-line means one inbound stream; dropping an unexpected second
            // is safer than tearing the live one down.
            tracing::warn!(
                token = "mic-receive-bin-already-linked",
                "microphone: receive bin already linked — ignoring extra inbound pad '{}'",
                pad.name()
            );
            return;
        }
        match pad.link(&sink_pad) {
            Ok(_) => {
                tracing::info!(
                    "microphone: inbound track linked into the sidecar ({} → {MIC_BIN_NAME})",
                    pad.name()
                );
                verify_mic_routing(&pipeline);
            }
            Err(e) => tracing::warn!(
                token = "mic-inbound-link-failed",
                "microphone: failed to link inbound pad: {e:?}"
            ),
        }
    });

    Ok(())
}

/// Whether an inbound webrtcbin pad's caps describe audio. Absent caps count as audio: the
/// audio PC only offers audio m-lines, so refusing a not-yet-fixated pad would drop the
/// real mic track.
fn caps_are_audio(caps: Option<&gst::Caps>) -> bool {
    let Some(caps) = caps else {
        return true;
    };
    match caps
        .structure(0)
        .and_then(|s| s.get::<String>("media").ok())
    {
        Some(media) => media == "audio",
        // No `media` field: accept, same reasoning as absent caps.
        None => true,
    }
}

/// Verify the mic `pulsesink` stream landed on the `quasar_mic` sink, and re-assert the
/// device if it did not.
///
/// Live incident 2026-08-02: the stream connected to `quasar_output` despite
/// `device=quasar_mic` on the element, mixing the user's microphone into the outbound
/// stream and starving `quasar_mic_src`. The mechanism is unproven (0/6 repro), so this
/// targets the failure surface instead: gstpulsesink's subscribe callback rewrites `device`
/// to the sink the stream is really on, making the mis-route observable, and a runtime
/// `device` write is `pa_context_move_sink_input_by_name`, the manual fix that healed the
/// live session. A silent, privacy-relevant fault becomes a logged, self-healing one.
fn verify_mic_routing(pipeline: &gst::Pipeline) {
    let Some(sink) = pipeline.by_name(MIC_SINK_NAME) else {
        return; // fakesink degrade path
    };
    if sink.find_property("device").is_none() {
        return;
    }
    let log_span = tracing::Span::current();
    std::thread::spawn(move || {
        // Re-enter the session span so this thread's lines carry session=<id>.
        let _log_span = log_span.enter();
        // Let the pa stream finish connecting and the subscribe callback settle.
        for (attempt, delay_ms) in [(1u8, 2_000u64), (2, 3_000)] {
            std::thread::sleep(std::time::Duration::from_millis(delay_ms));
            let device = sink
                .property::<Option<String>>("device")
                .unwrap_or_default();
            if device == QUASAR_MIC_SINK_NAME {
                if attempt > 1 {
                    tracing::info!("microphone: routing re-verified on '{device}' — healed");
                }
                return;
            }
            tracing::error!(
                token = "mic-routing-wrong-device",
                "microphone: stream landed on '{device}' instead of \
                 '{QUASAR_MIC_SINK_NAME}' — the mic would loop into the outbound \
                 stream; re-asserting the device (attempt {attempt})"
            );
            sink.set_property("device", QUASAR_MIC_SINK_NAME);
        }
        std::thread::sleep(std::time::Duration::from_millis(2_000));
        let device = sink
            .property::<Option<String>>("device")
            .unwrap_or_default();
        if device == QUASAR_MIC_SINK_NAME {
            tracing::info!("microphone: routing re-verified on '{device}' — healed");
        } else {
            tracing::error!(
                token = "mic-routing-unrecoverable",
                "microphone: routing STILL on '{device}' after re-assertion — mic \
                 audio is mixing into the outbound stream; manual fix: \
                 pactl move-sink-input <idx> {QUASAR_MIC_SINK_NAME} in the sidecar"
            );
        }
    });
}

/// Build the mic decode bin with a `sink` ghost pad over the depayloader's sink.
fn build_mic_bin(pulse_server: Option<&str>) -> Result<gst::Bin> {
    let bin = gst::Bin::builder().name(MIC_BIN_NAME).build();

    let mut elements: Vec<gst::Element> = Vec::new();
    for factory in mic_chain_factories(pulse_server.is_some()) {
        let mut builder = gst::ElementFactory::make(factory);
        if factory == "pulsesink" {
            // `verify_mic_routing` finds it by name (`by_name` recurses into bins).
            builder = builder.name(MIC_SINK_NAME);
        }
        let element = builder
            .build()
            .with_context(|| format!("{factory} not found — is it in the image?"))?;
        elements.push(element);
    }

    // Packet-loss concealment: the mic rides the same lossy client uplink, and a concealed
    // 20 ms gap beats a hard dropout in voice. Guarded — an unknown property panics.
    let decoder = &elements[1];
    if decoder.find_property("plc").is_some() {
        decoder.set_property("plc", true);
    } else {
        tracing::warn!(
            token = "mic-opusdec-no-plc",
            "microphone: opusdec has no 'plc' — packet-loss concealment not enabled"
        );
    }

    let terminal = elements.last().expect("chain is never empty");
    // Play out as soon as decoded: webrtcbin's jitter buffer already did the
    // reordering/timing, and a second timing stage only adds voice-path latency.
    terminal.set_property("sync", false);
    // `async=false` is load-bearing: an unfed sink holds the state change ASYNC until it
    // prerolls, and this one stays unfed until the browser attaches a mic track, which the
    // client may never do. Without it the whole AUDIO pipeline sits below PLAYING and the
    // session has no audio at all.
    terminal.set_property("async", false);
    if let Some(server) = pulse_server {
        let device = QUASAR_MIC_SINK_NAME;
        terminal.set_property("server", server);
        terminal.set_property("device", device);
        // The mic sink must never become the pipeline clock: the runner pins one shared
        // SystemClock across every session pipeline (#68).
        if terminal.find_property("provide-clock").is_some() {
            terminal.set_property("provide-clock", false);
        }
        tracing::info!("microphone: sink = pulsesink server={server} device={device}");
    } else {
        // Negotiate the m-line, discard the media: a session must not fail because the
        // sidecar is unavailable.
        tracing::warn!(
            token = "mic-no-pulse-sidecar",
            "microphone: no PulseAudio sidecar for this session — mic media will be \
             received and DISCARDED (fakesink)"
        );
    }

    bin.add_many(&elements)
        .context("failed to add microphone elements to the bin")?;
    gst::Element::link_many(&elements).context("failed to link the microphone chain")?;

    let depay_sink = elements[0]
        .static_pad("sink")
        .context("rtpopusdepay has no sink pad")?;
    let ghost = gst::GhostPad::with_target(&depay_sink)
        .context("failed to ghost the microphone bin's sink pad")?;
    ghost
        .set_active(true)
        .context("failed to activate the microphone bin's ghost pad")?;
    bin.add_pad(&ghost)
        .context("failed to add the microphone bin's ghost pad")?;

    Ok(bin)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn chain_terminates_in_pulsesink_only_with_a_sidecar() {
        assert_eq!(
            mic_chain_factories(true),
            [
                "rtpopusdepay",
                "opusdec",
                "audioconvert",
                "audioresample",
                "pulsesink"
            ]
        );
        // No sidecar: received and discarded, never a build failure.
        assert_eq!(*mic_chain_factories(false).last().unwrap(), "fakesink");
        assert_eq!(
            mic_chain_factories(true)[..4],
            mic_chain_factories(false)[..4]
        );
    }

    #[test]
    fn offered_caps_match_the_send_side_opus_payload() {
        gst::init().unwrap();
        let caps = mic_rtp_caps();
        let s = caps.structure(0).unwrap();
        assert_eq!(s.name(), "application/x-rtp");
        assert_eq!(s.get::<String>("media").unwrap(), "audio");
        assert_eq!(s.get::<String>("encoding-name").unwrap(), "OPUS");
        assert_eq!(s.get::<i32>("clock-rate").unwrap(), 48_000);
        // Chrome requires the rtpmap channel count (`opus/48000/2`); absent, the browser
        // rejects the m-line with port 0 (proven live 2026-08-02).
        assert_eq!(s.get::<String>("encoding-params").unwrap(), "2");
        assert_eq!(s.get::<i32>("payload").unwrap(), MIC_OPUS_PT);
    }

    #[test]
    fn audio_pad_caps_are_accepted_and_video_rejected() {
        gst::init().unwrap();
        let audio = gst::Caps::builder("application/x-rtp")
            .field("media", "audio")
            .build();
        let video = gst::Caps::builder("application/x-rtp")
            .field("media", "video")
            .build();
        let bare = gst::Caps::builder("application/x-rtp").build();
        assert!(caps_are_audio(Some(&audio)));
        assert!(!caps_are_audio(Some(&video)));
        // Unfixated / absent caps must not drop the real mic track.
        assert!(caps_are_audio(Some(&bare)));
        assert!(caps_are_audio(None));
    }
}
