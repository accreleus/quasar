//! The WebRTC implementation of the transport slot (`super::transport`). After the
//! encoded-frame boundary it owns the RTP payloader, the RTP caps and their header
//! extensions (TWCC, abs-capture-time), and the `webrtcbin` transports, which are built
//! once at launch and replaced under the running encode chain when a different peer
//! attaches (#535). The payloader is the session's and outlives every replacement. The
//! encoder element is kept; a replacement sends it no event, and writes to it only what
//! `abr_glue::reset_to_launch` logs.
//!
//! `webrtcbin` cannot serve a second peer on a transport a first one negotiated: it
//! ignores the `ice-restart` offer option, so a re-offer carries the same ICE credentials
//! and the same DTLS association. A new peer therefore gets a new `webrtcbin`.

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use anyhow::{Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;

use super::abr_glue::{self, EncodeResolutionLever, ResolutionLever};
use super::audio_branch::add_audio_chain;
use super::codec_chain::build_video_payloader;
use super::mic_branch::add_mic_branch;
use super::probes::{attach_rtp_marker_trace, attach_rtp_out_probe, attach_rtp_ts_trace};
use super::rtp_ext::{
    attach_abs_capture_time_egress_probe, attach_abs_capture_time_probe,
    attach_abs_capture_time_verification_probe, AbsCaptureTimeExtension,
};
use super::transport::EncodedSession;
use super::webrtc::{
    connect_data_channel_open, connect_ice_candidate, connect_negotiation_needed,
    connect_state_logging, enable_video_fec, enable_video_rtx, make_webrtcbin,
    set_audio_receive_latency, spawn_fec_controller, warn_on_fingerprint_mismatch,
};
use super::{
    audio_disabled, audio_health, input_data_channel_init, resize, OutTx, TraceTx,
    ABS_CAPTURE_TIME_EXT_ID, ABS_CAPTURE_TIME_URI, RTP_TWCC_URI, TWCC_EXT_ID,
};
use crate::session::input::{InputSink, InputState};
use crate::session::metrics::SessionMetrics;
use crate::session::signaling::PcId;
use crate::session::virtual_input::VirtualDevices;
use crate::session::SessionConfig;

/// Ids for video transports, unique in the process: the session's holder names one.
static NEXT_TRANSPORT: AtomicU64 = AtomicU64::new(1);

/// One video transport: the `webrtcbin` carrying video and the `"input"` DataChannel.
pub struct VideoTransport {
    pub id: u64,
    pub webrtc: gst::Element,
    /// Held alive with the transport.
    pub data_channel: Option<glib::Object>,
    bin: gst::Bin,
    retired: Arc<AtomicBool>,
    /// The probe dropping RTP at the tail until this transport's peer has connected. An
    /// unconnected `webrtcbin` holds what is pushed into it, which parks the encode
    /// chain's streaming thread: anything serialized behind it (the allocation query of a
    /// size step or an app swap) then waits for a peer that may never come.
    gate: Arc<Mutex<Option<gst::PadProbeId>>>,
    /// The probe dropping RTP at the tail since a WebTransport client took the session
    /// from this transport's peer. Kept apart from `gate`, which the peer reconnecting
    /// would lift.
    muted: Mutex<Option<gst::PadProbeId>>,
}

/// What it takes to put a transport on the RTP tail, kept for the session so it can be
/// done again.
pub struct WebRtcSlot {
    /// Its stream is the launch point a new transport's ABR starts from; an adoption moves
    /// it (amendment 59, [`Self::restream`]).
    cfg: Mutex<SessionConfig>,
    pipeline: gst::Pipeline,
    /// The RTP capsfilter's src pad, where each `webrtcbin` transport is linked.
    tail: gst::Pad,
    /// Outlives every transport, so a size [`super::mtu_shrink`] stepped down to does too: a
    /// viewer reconnecting over the same path would black out again at the configured size.
    payloader: gst::Element,
    encoder: gst::Element,
    out_tx: OutTx,
    devices: Option<Arc<VirtualDevices>>,
    input_state: Arc<InputState>,
    metrics: Arc<SessionMetrics>,
    trace_tx: TraceTx,
    session_id: String,
    capture_times: AbsCaptureTimeExtension,
    lever: Arc<EncodeResolutionLever>,
    /// `QUASAR_DIAG_NO_OBS` unset.
    observe: bool,
    /// The session's WebTransport media client, when it has one: each WebRTC transport's
    /// input and releases go through it, keyed by that transport (§Takeover).
    webtransport: Option<Arc<crate::webtransport::Media>>,
}

/// Give a `webrtcbin` sink pad the MediaStream id this PC always uses. Left unset,
/// webrtcbin invents one per instance, and a client then sees a replaced transport's
/// offer as a new stream: a second `track` event on a peer connection it already wired.
fn pin_msid(pad: &gst::Pad, pc: PcId) {
    if pad.find_property("msid").is_some() {
        pad.set_property("msid", format!("quasar-{pc}"));
    }
}

/// Test hook: the session id of every `build_audio` call, in order.
#[cfg(test)]
pub(super) static AUDIO_BUILDS: std::sync::Mutex<Vec<String>> = std::sync::Mutex::new(Vec::new());

/// Test hook: how long a replacement's attach stands still before its transport is
/// brought up, so the encode chain pushes into whatever state the slot is in.
#[cfg(test)]
pub(super) static ATTACH_PAUSE_MS: std::sync::atomic::AtomicU64 =
    std::sync::atomic::AtomicU64::new(0);

/// Wrap `webrtc` in a bin whose sink pad reports success for whatever a retired
/// transport returns. A push parked inside a transport being torn down comes back
/// FLUSHING, and FLUSHING reaching the encode chain pauses its streaming task for good.
fn transport_bin(webrtc: &gst::Element, retired: &Arc<AtomicBool>) -> Result<gst::Bin> {
    let bin = gst::Bin::new();
    bin.add(webrtc)?;
    let target = webrtc
        .request_pad_simple("sink_%u")
        .context("webrtcbin refused a sink pad request (video)")?;
    pin_msid(&target, PcId::Video);
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

/// How long a transport whose peer has connected waits for the keyframe it asked for
/// before it sends whatever the encoder produces.
const KEYFRAME_WAIT: Duration = Duration::from_secs(1);

/// Stop dropping RTP at the tail from the encoder's next keyframe on. The frames dropped
/// before it leave a GOP the peer has none of: what follows them in that GOP would reach
/// it undecodable.
fn open_at_keyframe(
    encoder: &gst::Element,
    tail: &glib::WeakRef<gst::Pad>,
    gate: &Arc<Mutex<Option<gst::PadProbeId>>>,
    session_id: &str,
) {
    let (tail, gate, session_id) = (tail.clone(), gate.clone(), session_id.to_string());
    let open = move |keyframe: bool| {
        let Some(gate) = gate.lock().unwrap().take() else {
            return; // retired meanwhile, or opened already
        };
        if let Some(tail) = tail.upgrade() {
            tail.remove_probe(gate);
        }
        if !keyframe {
            tracing::warn!(
                token = "transport-keyframe-missing",
                session_id = %session_id,
                "video PC: no keyframe {} ms after the peer connected; sending what the \
                 encoder produces, which shows nothing until its next keyframe",
                KEYFRAME_WAIT.as_millis()
            );
        }
    };
    let Some(src) = encoder.static_pad("src") else {
        return open(false);
    };
    let deadline = std::time::Instant::now() + KEYFRAME_WAIT;
    src.add_probe(gst::PadProbeType::BUFFER, move |_, info| {
        let keyframe = info
            .buffer()
            .is_some_and(|b| !b.flags().contains(gst::BufferFlags::DELTA_UNIT));
        if !keyframe && std::time::Instant::now() < deadline {
            return gst::PadProbeReturn::Ok;
        }
        open(keyframe);
        gst::PadProbeReturn::Remove
    });
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
    /// Let go of whatever the transport holds, on a session's way out. Must run before
    /// the source is stopped: a transport whose peer never connected parks the encode
    /// chain's push, and a source waiting on that chain never reaches NULL.
    pub fn release(&self) {
        self.retired.store(true, Ordering::SeqCst);
        let _ = self.webrtc.set_state(gst::State::Null);
    }

    /// Tear the transport down under a live encode chain. The caller holds RTP at `tail`.
    fn retire(&self, tail: &gst::Pad, pipeline: &gst::Pipeline) {
        self.retired.store(true, Ordering::SeqCst);
        // Its replacement brings its own.
        let probes = [
            self.gate.lock().unwrap().take(),
            self.muted.lock().unwrap().take(),
        ];
        for probe in probes.into_iter().flatten() {
            tail.remove_probe(probe);
        }
        // Out of the pipeline first: the dying SCTP association must not reach the
        // session's bus, where it reads as the peer leaving and ends the session.
        let _ = pipeline.remove(&self.bin);
        // Releases a push parked inside (a transport that never connected holds RTP).
        let _ = self.webrtc.set_state(gst::State::Null);
        wait_idle(tail);
        let _ = self.bin.set_state(gst::State::Null);
    }
}

impl WebRtcSlot {
    /// Payload the session's encoded video: parser → payloader → RTP caps, in the encode
    /// pipeline, with the RTP probes. Transports go on the RTP caps' src pad.
    pub(crate) fn new(session: EncodedSession) -> Result<Self> {
        let EncodedSession {
            cfg,
            pipeline,
            video,
            encoder,
            lever,
            metrics,
            out_tx,
            devices,
            input_state,
            trace_tx,
            session_id,
            observe,
        } = session;
        let codec = cfg.stream.codec;
        let pay = build_video_payloader(codec, cfg.rtp_mtu)?;
        // TWCC must be declared HERE, in the capsfilter, never via `add-extension` on the
        // payloader. webrtcbin builds its offer from these caps, the payloader's
        // auto-header-extension instantiates the matching `rtphdrexttwcc` writer from the
        // negotiated caps, and webrtcbin's rtpsession learns the id the same way, so the
        // extmap id in the SDP and on the wire cannot diverge. Bolting the extension on
        // out-of-band (webrtcsink's approach) breaks here because this graph pins
        // fully-fixed caps between payloader and webrtcbin: the extension writes header
        // bytes the offer never declared and Chrome drops every packet.
        // `rtcp-fb-transport-cc` puts `a=rtcp-fb:96 transport-cc` in the offer; without it
        // the browser never sends the TWCC feedback rtpgccbwe consumes.
        //
        // abs-capture-time is always on, ABR or not: declaring it in the caps makes
        // webrtcbin emit `a=extmap:6 <URI>` (same mechanism as TWCC extmap-5), and the
        // extension registered on `pay` below records the per-AU NTP time.
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
        let rtp_capsfilter = gst::ElementFactory::make("capsfilter")
            .property("caps", rtp_caps_builder.build())
            .build()
            .context("capsfilter not found")?;
        pipeline.add_many([&pay, &rtp_capsfilter])?;
        let pay_sink = pay
            .static_pad("sink")
            .context("payloader has no sink pad")?;
        video
            .link(&pay_sink)
            .context("failed to link the encoded video into the payloader")?;
        pay.link(&rtp_capsfilter)
            .context("failed to link the payloader into the RTP caps")?;

        if observe {
            attach_rtp_out_probe(&pay, metrics.clone());
        }
        // Registering the extension records each AU's capture instant; the transport
        // stamps it at its egress seam (`attach_video`).
        let capture_times = attach_abs_capture_time_probe(&pay);
        if observe {
            attach_abs_capture_time_verification_probe(&rtp_capsfilter);
        }
        // Diagnostic: per-AU RTP marker-layout trace on the live wire. The synthetic
        // videotestsrc path cannot reproduce rtph264pay's marker logic, so a vulkan-vs-VA
        // marker-bit difference is only decidable on real content. Knob:
        // QUASAR_TRACE_RTP_MARKER.
        if env_flag("QUASAR_TRACE_RTP_MARKER") {
            attach_rtp_marker_trace(&pay);
        }
        // Diagnostic: per-AU RTP timestamp-continuity + seq-contiguity trace, the two
        // surviving libwebrtc PacketBuffer frame-completion keys. Flags dup-TS-across-AU
        // (the every-other-frame merge suspect), non-1500 TS deltas, and seq gaps. Knob:
        // QUASAR_TRACE_RTP_TS.
        if env_flag("QUASAR_TRACE_RTP_TS") {
            attach_rtp_ts_trace(&pay);
        }
        Ok(Self {
            cfg: Mutex::new(cfg),
            pipeline,
            tail: rtp_capsfilter
                .static_pad("src")
                .context("rtp capsfilter has no src pad")?,
            payloader: pay,
            encoder,
            out_tx,
            devices,
            input_state,
            metrics,
            trace_tx,
            session_id,
            capture_times,
            lever,
            observe,
            webtransport: None,
        })
    }

    /// Route this session's WebRTC input and releases through its WebTransport side. Set
    /// before the first transport is attached.
    pub(crate) fn share_input(&mut self, media: Arc<crate::webtransport::Media>) {
        self.webtransport = Some(media);
    }

    /// A WebTransport client took the session: `video`'s peer gets no more media. The
    /// transport stays until a new WebRTC peer replaces it.
    pub(crate) fn mute(&self, video: &VideoTransport) {
        let mut muted = video.muted.lock().unwrap();
        if muted.is_none() {
            *muted = self.hold_rtp();
        }
    }

    /// Build a video transport on the tail. `replacement` is false for the session's
    /// first one, built while the pipeline is still NULL.
    ///
    /// `gate` is the probe already holding RTP back at the tail during a replacement.
    /// Either way the transport keeps one until its peer connects.
    pub(crate) fn attach_video(
        &self,
        replacement: bool,
        gate: Option<gst::PadProbeId>,
    ) -> Result<VideoTransport> {
        let cfg = &self.cfg.lock().unwrap().clone();
        let gate = Arc::new(Mutex::new(gate.or_else(|| self.hold_rtp())));
        let id = NEXT_TRANSPORT.fetch_add(1, Ordering::Relaxed);
        let side = self
            .webtransport
            .clone()
            .map(|media| crate::webtransport::WebRtcSide {
                media,
                transport: Some(id),
            });
        let webrtc = make_webrtcbin(cfg.stun.as_deref())?;
        // Signals webrtcbin emits while it builds its send session: connect them first.
        if cfg.rtp_mtu_shrink {
            super::mtu_shrink::attach(
                &webrtc,
                &self.payloader,
                &self.encoder,
                self.session_id.clone(),
            );
        }
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
        if replacement {
            // Nothing a transport added mid-stream does may make the chain upstream
            // renegotiate: unfenced, two RECONFIGURE events reach the encoder at every
            // replacement.
            sink.add_probe(gst::PadProbeType::EVENT_UPSTREAM, |_, info| {
                match info.event().map(|e| e.type_()) {
                    Some(gst::EventType::Reconfigure) => gst::PadProbeReturn::Drop,
                    _ => gst::PadProbeReturn::Ok,
                }
            });
        } else {
            self.tail
                .link(&sink)
                .context("failed to link video into webrtcbin")?;
        }
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
            side.clone(),
            Some(self.trace_tx.clone()),
            self.session_id.clone(),
        );
        connect_negotiation_needed(
            &webrtc,
            self.out_tx.clone(),
            PcId::Video,
            self.session_id.clone(),
        );
        warn_on_fingerprint_mismatch(&webrtc, PcId::Video, self.session_id.clone());
        // Its peer connected: the encoder is mid-GOP, so RTP flows from a keyframe on.
        // Once per transport; the same peer reconnecting needs neither.
        let encoder = self.encoder.downgrade();
        let session_id = self.session_id.clone();
        let (tail, held) = (self.tail.downgrade(), gate.clone());
        webrtc.connect_notify(Some("connection-state"), move |webrtc, _| {
            let state =
                webrtc.property::<gstreamer_webrtc::WebRTCPeerConnectionState>("connection-state");
            if state != gstreamer_webrtc::WebRTCPeerConnectionState::Connected
                || held.lock().unwrap().is_none()
            {
                return;
            }
            if let Some(encoder) = encoder.upgrade() {
                tracing::info!(
                    token = "transport-peer-connected",
                    session_id = %session_id,
                    "video PC: the transport's peer connected; forcing a keyframe and \
                     sending from it"
                );
                open_at_keyframe(&encoder, &tail, &held, &session_id);
                resize::force_idr(&encoder);
            }
        });

        // webrtcbin must be at least READY before create-data-channel, and the channel
        // must exist before the offer.
        if replacement {
            #[cfg(test)]
            std::thread::sleep(Duration::from_millis(
                ATTACH_PAUSE_MS.load(Ordering::Relaxed),
            ));
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
                    side,
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
            // Linked only now, with its pads active. The link makes the tail resend its
            // sticky events on the next push, and an inactive pad refuses them FLUSHING:
            // that fails the push before any probe or chain function sees it, and
            // pauses the encode chain's streaming task for good.
            self.tail
                .link_full(
                    &sink,
                    gst::PadLinkCheck::DEFAULT | gst::PadLinkCheck::NO_RECONFIGURE,
                )
                .context("failed to link video into the replacement webrtcbin")?;
        }
        Ok(VideoTransport {
            id,
            webrtc,
            data_channel,
            bin,
            retired,
            gate,
            muted: Mutex::new(None),
        })
    }

    /// Drop RTP at the tail until the returned probe is removed.
    fn hold_rtp(&self) -> Option<gst::PadProbeId> {
        self.tail.add_probe(
            gst::PadProbeType::BUFFER | gst::PadProbeType::BUFFER_LIST,
            |_, _| gst::PadProbeReturn::Drop,
        )
    }

    /// Give the video PC a new transport and tear the old one down, with the encode chain
    /// live. The new transport offers as soon as it is up, and takes RTP once its peer
    /// has connected. On `Err` the old one is gone.
    pub fn replace_video(&self, video: &mut VideoTransport) -> Result<()> {
        // Also while the slot is empty: an unlinked push is not-linked, which errors the
        // encode pipeline.
        let gate = self.hold_rtp();
        video.retire(&self.tail, &self.pipeline);
        // A key the last peer held must not stay down for the next one. With a WebTransport
        // side the caller's handover released it (`Media::webrtc_replace`, `webrtc_retired`).
        if let Some(devices) = self
            .devices
            .as_ref()
            .filter(|_| self.webtransport.is_none())
        {
            match devices.release_all() {
                Ok(()) => tracing::info!(
                    token = "transport-replaced-input-released",
                    "video PC: the previous transport is down; keys and buttons its peer \
                     held are released"
                ),
                Err(e) => tracing::warn!(
                    token = "transport-replace-release-all-failed",
                    "release_all on transport replacement failed: {e:#}"
                ),
            }
        }
        let cfg = self.cfg.lock().unwrap().clone();
        abr_glue::reset_to_launch(&self.encoder, &cfg, &self.lever, &self.metrics);
        *video = self.attach_video(true, gate)?;
        // The pipeline handed its latency to the first transport on the way to PLAYING;
        // one added later has to be told.
        let _ = self.pipeline.recalculate_latency();
        Ok(())
    }

    /// Amendment 59 (a)–(b): make `to`'s bitrate, fps and size the launch point, then give
    /// the video PC a new transport, whose ABR starts from there and which offers at once.
    /// On `Err`, the step that failed (`fps`, `bitrate` or `transport`).
    pub fn restream(
        &self,
        video: &mut VideoTransport,
        to: &crate::session::StreamParams,
    ) -> std::result::Result<(), (&'static str, anyhow::Error)> {
        self.move_launch_point(to)?;
        self.replace_video(video).map_err(|e| ("transport", e))
    }

    /// [`Self::restream`] without the new transport: `to`'s bitrate and fps written live,
    /// and its bitrate, fps and size the launch point a later transport starts from.
    pub fn move_launch_point(
        &self,
        to: &crate::session::StreamParams,
    ) -> std::result::Result<(), (&'static str, anyhow::Error)> {
        let mut cfg = self.cfg.lock().unwrap().clone();
        if to.fps != cfg.stream.fps {
            self.lever.set_fps(to.fps).map_err(|e| ("fps", e))?;
        }
        // The CPB is sized kbps / fps, so a new fps rewrites it too.
        let rate_moves = to.bitrate_kbps != cfg.stream.bitrate_kbps || to.fps != cfg.stream.fps;
        if rate_moves && !abr_glue::write_bitrate_live(&self.encoder, &cfg, to.bitrate_kbps, to.fps)
        {
            return Err((
                "bitrate",
                anyhow::anyhow!("the encoder's bitrate is not writable live"),
            ));
        }
        cfg.stream.bitrate_kbps = to.bitrate_kbps;
        cfg.stream.fps = to.fps;
        cfg.stream.width = to.width;
        cfg.stream.height = to.height;
        *self.cfg.lock().unwrap() = cfg;
        Ok(())
    }

    /// Build the audio PC: its own pipeline and `webrtcbin`, left READY. `(None, None)`
    /// when stream audio is off.
    ///
    /// #304: the audio webrtcbin MUST live in its own gst::Pipeline. Two webrtcbins in one
    /// pipeline never reach PLAYING (the aggregate state transition stalls), and the split
    /// also stops the browser's A/V sync from inflating video playout.
    pub fn build_audio(&self) -> Result<(Option<gst::Element>, Option<gst::Pipeline>)> {
        #[cfg(test)]
        AUDIO_BUILDS.lock().unwrap().push(self.session_id.clone());
        let cfg = &self.cfg.lock().unwrap().clone();
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
        pin_msid(&audio_sink_pad, PcId::Audio);
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
        connect_state_logging(
            &audio_webrtc,
            self.devices.clone(),
            self.webtransport
                .clone()
                .map(|media| crate::webtransport::WebRtcSide {
                    media,
                    transport: None,
                }),
            None,
            String::new(),
        );
        // There is one `offer created` per PC (video + audio), so a harness asserting
        // "exactly one offer" must count the VIDEO PC's line, not grep the session. Both
        // lines carry the session id.
        connect_negotiation_needed(
            &audio_webrtc,
            self.out_tx.clone(),
            PcId::Audio,
            self.session_id.clone(),
        );
        warn_on_fingerprint_mismatch(&audio_webrtc, PcId::Audio, self.session_id.clone());
        audio_pipe
            .set_state(gst::State::Ready)
            .context("audio pipeline failed to reach READY")?;
        Ok((Some(audio_webrtc), Some(audio_pipe)))
    }
}

/// A diagnostic knob that is on unless unset, empty or `0`.
fn env_flag(name: &str) -> bool {
    std::env::var(name)
        .map(|v| v != "0" && !v.is_empty())
        .unwrap_or(false)
}
