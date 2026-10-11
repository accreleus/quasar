//! The transport slot of the media path (`compositor -> encode -> [transport slot]`): the
//! WebRTC transports as units that are built once at launch and replaced, with the
//! encoder left running, when a different peer attaches (#535).
//!
//! `webrtcbin` cannot serve a second peer on a transport a first one negotiated: it
//! ignores the `ice-restart` offer option, so a re-offer carries the same ICE credentials
//! and the same DTLS association. A new peer therefore gets a new `webrtcbin`.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;

use super::abr_glue::{self, EncodeResolutionLever};
use super::audio_branch::add_audio_chain;
use super::mic_branch::add_mic_branch;
use super::rtp_ext::{attach_abs_capture_time_egress_probe, AbsCaptureTimeExtension};
use super::webrtc::{
    connect_data_channel_open, connect_ice_candidate, connect_negotiation_needed,
    connect_state_logging, enable_video_fec, enable_video_rtx, make_webrtcbin,
    set_audio_receive_latency, spawn_fec_controller,
};
use super::{audio_disabled, audio_health, input_data_channel_init, resize, OutTx, TraceTx};
use crate::session::input::{InputSink, InputState};
use crate::session::metrics::SessionMetrics;
use crate::session::signaling::PcId;
use crate::session::virtual_input::VirtualDevices;
use crate::session::SessionConfig;

/// One video transport: the `webrtcbin` carrying video and the `"input"` DataChannel.
pub struct VideoTransport {
    pub webrtc: gst::Element,
    /// Held alive with the transport.
    pub data_channel: Option<glib::Object>,
    bin: gst::Bin,
    retired: Arc<AtomicBool>,
}

/// What it takes to put a transport on the encode chain's tail, kept for the session so
/// it can be done again.
pub struct TransportSlot {
    pub(super) cfg: SessionConfig,
    pub(super) pipeline: gst::Pipeline,
    /// The RTP capsfilter's src pad: the last pad of the encode chain.
    pub(super) tail: gst::Pad,
    pub(super) encoder: gst::Element,
    pub(super) out_tx: OutTx,
    pub(super) devices: Option<Arc<VirtualDevices>>,
    pub(super) input_state: Arc<InputState>,
    pub(super) metrics: Arc<SessionMetrics>,
    pub(super) trace_tx: TraceTx,
    pub(super) session_id: String,
    pub(super) capture_times: AbsCaptureTimeExtension,
    pub(super) lever: Arc<EncodeResolutionLever>,
    /// `QUASAR_DIAG_NO_OBS` unset.
    pub(super) observe: bool,
}

/// Wrap `webrtc` in a bin whose sink pad reports success for whatever a retired
/// transport returns. A push parked inside a transport being torn down comes back
/// FLUSHING, and FLUSHING reaching the encode chain pauses its streaming task for good.
fn transport_bin(webrtc: &gst::Element, retired: &Arc<AtomicBool>) -> Result<gst::Bin> {
    let bin = gst::Bin::new();
    bin.add(webrtc)?;
    let target = webrtc
        .request_pad_simple("sink_%u")
        .context("webrtcbin refused a sink pad request (video)")?;
    let (for_buffer, for_list) = (retired.clone(), retired.clone());
    let sink = gst::GhostPad::builder_with_target(&target)?
        .name("sink")
        .chain_function(move |pad, parent, buffer| {
            let flow = gst::ProxyPad::chain_default(pad, parent, buffer);
            if for_buffer.load(Ordering::SeqCst) {
                return Ok(gst::FlowSuccess::Ok);
            }
            flow
        })
        .chain_list_function(move |pad, parent, list| {
            let flow = gst::ProxyPad::chain_list_default(pad, parent, list);
            if for_list.load(Ordering::SeqCst) {
                return Ok(gst::FlowSuccess::Ok);
            }
            flow
        })
        .build();
    bin.add_pad(&sink)?;
    Ok(bin)
}

/// Return once no push from `pad` is in progress.
fn wait_idle(pad: &gst::Pad) {
    let (tx, rx) = std::sync::mpsc::channel();
    pad.add_probe(gst::PadProbeType::IDLE, move |_, _| {
        let _ = tx.send(());
        gst::PadProbeReturn::Remove
    });
    if rx.recv_timeout(Duration::from_secs(2)).is_err() {
        tracing::warn!(
            token = "transport-retire-not-idle",
            "the encode tail was still pushing into a retired transport after 2 s"
        );
    }
}

impl VideoTransport {
    /// Tear the transport down under a live encode chain. The caller holds RTP at `tail`.
    fn retire(&self, tail: &gst::Pad, pipeline: &gst::Pipeline) {
        self.retired.store(true, Ordering::SeqCst);
        // Out of the pipeline first: the dying SCTP association must not reach the
        // session's bus, where it reads as the peer leaving and ends the session.
        let _ = pipeline.remove(&self.bin);
        // Releases a push parked inside (a transport that never connected holds RTP).
        let _ = self.webrtc.set_state(gst::State::Null);
        wait_idle(tail);
        let _ = self.bin.set_state(gst::State::Null);
    }
}

impl TransportSlot {
    /// Build a video transport on the tail. `replacement` is false for the session's
    /// first one, built while the pipeline is still NULL.
    pub(super) fn attach_video(&self, replacement: bool) -> Result<VideoTransport> {
        let cfg = &self.cfg;
        let webrtc = make_webrtcbin(cfg.stun.as_deref())?;
        // Signals webrtcbin emits while it builds its send session: connect them first.
        if self.observe {
            attach_abs_capture_time_egress_probe(
                &webrtc,
                self.capture_times.clone(),
                cfg.latency_probe.then(|| self.metrics.clone()),
            );
        }
        let retired = Arc::new(AtomicBool::new(false));
        let bin = transport_bin(&webrtc, &retired)?;
        self.pipeline.add(&bin)?;
        let sink = bin
            .static_pad("sink")
            .context("transport bin has no sink")?;
        self.tail
            .link(&sink)
            .context("failed to link video into webrtcbin")?;
        enable_video_rtx(&webrtc);
        // ULPFEC/RED proactive burst-loss repair, no RTX round-trip. `fixed` negotiates
        // ulp-red at the static level; `auto` negotiates at 0% and the controller ramps
        // on loss. Knobs: QUASAR_FEC_MODE / QUASAR_FEC_PERCENTAGE.
        let fec_plan = cfg.fec_plan();
        enable_video_fec(&webrtc, fec_plan.negotiate, fec_plan.initial_pct);
        if fec_plan.controller_enabled {
            spawn_fec_controller(
                &webrtc,
                cfg.fec_controller_config(),
                Arc::downgrade(&self.metrics),
                retired.clone(),
                self.trace_tx.clone(),
                self.session_id.clone(),
            );
        }
        // Arm the in-session ABR governor on the send path. Must be wired before the
        // offer so rtpgccbwe is part of the negotiated send session. No-op only when
        // rtpgccbwe is absent or the encoder rejects live writes.
        abr_glue::enable_abr(
            &webrtc,
            &self.encoder,
            cfg,
            self.metrics.clone(),
            self.trace_tx.clone(),
            self.session_id.clone(),
            Some(self.lever.clone()).filter(|l| l.stage().supported),
        );
        connect_ice_candidate(&webrtc, self.out_tx.clone(), PcId::Video);
        connect_state_logging(
            &webrtc,
            self.devices.clone(),
            Some(self.trace_tx.clone()),
            self.session_id.clone(),
        );
        connect_negotiation_needed(
            &webrtc,
            self.out_tx.clone(),
            PcId::Video,
            self.session_id.clone(),
        );
        if replacement {
            // The encoder is mid-GOP: the peer that just connected needs a keyframe.
            let encoder = self.encoder.downgrade();
            webrtc.connect_notify(Some("connection-state"), move |webrtc, _| {
                let state = webrtc
                    .property::<gstreamer_webrtc::WebRTCPeerConnectionState>("connection-state");
                if state == gstreamer_webrtc::WebRTCPeerConnectionState::Connected {
                    if let Some(encoder) = encoder.upgrade() {
                        resize::force_idr(&encoder);
                    }
                }
            });
        }

        // webrtcbin must be at least READY before create-data-channel, and the channel
        // must exist before the offer.
        if replacement {
            bin.set_state(gst::State::Ready)
                .context("transport failed to reach READY")?;
        } else {
            self.pipeline
                .set_state(gst::State::Ready)
                .context("encode pipeline failed to reach READY")?;
        }
        let (dc_init, input_channel_mode) = input_data_channel_init();
        let data_channel = webrtc
            .emit_by_name_with_values(
                "create-data-channel",
                &["input".to_value(), Some(dc_init).to_value()],
            )
            .and_then(|v| v.get::<glib::Object>().ok());
        let input_sink = match &self.devices {
            Some(d) => InputSink::Virtual(d.clone()),
            None => InputSink::None,
        };
        match &data_channel {
            Some(dc) => {
                connect_data_channel_open(
                    dc,
                    input_sink,
                    self.input_state.clone(),
                    cfg.stream.width,
                    cfg.stream.height,
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
        if replacement {
            bin.sync_state_with_parent()
                .context("transport failed to follow the encode pipeline's state")?;
        }
        Ok(VideoTransport {
            webrtc,
            data_channel,
            bin,
            retired,
        })
    }

    /// Give the video PC a new transport and tear the old one down, with the encode chain
    /// live. The new transport offers as soon as it is up. On `Err` the old one is gone.
    pub fn replace_video(&self, video: &mut VideoTransport) -> Result<()> {
        // RTP is dropped at the tail while the slot is empty: an unlinked push is
        // not-linked, which errors the encode pipeline.
        let gate = self.tail.add_probe(
            gst::PadProbeType::BUFFER | gst::PadProbeType::BUFFER_LIST,
            |_, _| gst::PadProbeReturn::Drop,
        );
        video.retire(&self.tail, &self.pipeline);
        // A key the last peer held must not stay down for the next one.
        if let Some(devices) = &self.devices {
            if let Err(e) = devices.release_all() {
                tracing::warn!(
                    token = "transport-replace-release-all-failed",
                    "release_all on transport replacement failed: {e:#}"
                );
            }
        }
        abr_glue::reset_to_launch(&self.encoder, &self.cfg, &self.lever, &self.metrics);
        let new = self.attach_video(true);
        if let Some(gate) = gate {
            self.tail.remove_probe(gate);
        }
        *video = new?;
        // The pipeline handed its latency to the first transport on the way to PLAYING;
        // one added later has to be told.
        let _ = self.pipeline.recalculate_latency();
        Ok(())
    }

    /// Build the audio PC: its own pipeline and `webrtcbin`, left READY. `(None, None)`
    /// when stream audio is off.
    ///
    /// #304: the audio webrtcbin MUST live in its own gst::Pipeline. Two webrtcbins in one
    /// pipeline never reach PLAYING (the aggregate state transition stalls), and the split
    /// also stops the browser's A/V sync from inflating video playout.
    pub fn build_audio(&self) -> Result<(Option<gst::Element>, Option<gst::Pipeline>)> {
        let cfg = &self.cfg;
        if audio_disabled() {
            tracing::info!(
                "stream audio disabled by runtime or session topology — video-only offer"
            );
            if cfg.mic_enabled() {
                // The mic m-line rides the audio PC, so no audio PC means no microphone
                // path. Say so: a mic that silently does nothing is otherwise undiagnosable.
                tracing::warn!(
                    token = "mic-no-audio-peerconnection",
                    "microphone requested but this session has no audio PeerConnection \
                     (QUASAR_AUDIO_DISABLED or a local-only/stream-audio-off topology) — \
                     no mic m-line will be offered"
                );
            }
            return Ok((None, None));
        }
        // Audio: pulsesrc from the session's PulseAudio sidecar (stable across swaps — the
        // new app connects to the same one).
        let audio_pipe = gst::Pipeline::new();
        audio_health::watch_bus(&audio_pipe, &self.session_id);
        let audio_tail = add_audio_chain(&audio_pipe, cfg, &self.session_id)?;
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
        // before READY/negotiation, because the mic m-line must ride the audio offer
        // (`protocol/signaling.md` §Microphone m-line). Never renegotiated —
        // the single-offer guard is untouched and mute/unmute is client-local.
        if cfg.mic_enabled() {
            add_mic_branch(&audio_pipe, &audio_webrtc, cfg.pulse_server.as_deref())?;
        }
        // The audio webrtcbin gets its own negotiation/ICE/state callbacks (PcId::Audio).
        connect_ice_candidate(&audio_webrtc, self.out_tx.clone(), PcId::Audio);
        // Pass devices so release_all fires on audio-PC disconnect too.
        connect_state_logging(&audio_webrtc, self.devices.clone(), None, String::new());
        // There is one `offer created` per PC (video + audio), so a harness asserting
        // "exactly one offer" must count the VIDEO PC's line, not grep the session. Both
        // lines carry the session id.
        connect_negotiation_needed(
            &audio_webrtc,
            self.out_tx.clone(),
            PcId::Audio,
            self.session_id.clone(),
        );
        audio_pipe
            .set_state(gst::State::Ready)
            .context("audio pipeline failed to reach READY")?;
        Ok((Some(audio_webrtc), Some(audio_pipe)))
    }
}
