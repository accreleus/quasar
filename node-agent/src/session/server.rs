//! Signaling transport for a session.
//!
//! [`serve_direct`] is the **direct** WebSocket signaling server: the agent binds
//! a socket, and a browser (or the loopback answerer) connects straight to it —
//! the demo / dev path that exercises a node-agent session end-to-end without a
//! control plane. In production the control plane owns signaling and relays each
//! message over the agent-API connection (agent-api.md §Signaling relay), pumping
//! the same [`pipeline::OutTx`] channel / calling the same [`handle_inbound`] —
//! the media + negotiation core in `pipeline.rs` is unaffected.
//!
//! [`run_answerer`] is a deterministic, in-container loopback peer used by
//! acceptance to prove negotiation reaches ICE-connected at a given resolution
//! without a browser. A test affordance, not a real client.

use anyhow::{anyhow, Context, Result};
use futures_util::{SinkExt, StreamExt};
use gstreamer as gst;
use gstreamer::prelude::*;
use tokio::sync::mpsc;
use tokio_tungstenite::tungstenite::Message;

use super::host::SessionHost;
use super::pipeline::{self, OutTx, SrdFailTx, SrdOkTx};
use super::signaling::{PcId, SignalMsg};
use super::SessionConfig;

/// Which side of the handshake we are. Drives inbound message dispatch.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Role {
    /// Agent: creates the offer, expects an answer back.
    Offerer,
    /// Loopback test peer: expects an offer, produces an answer.
    Answerer,
}

// ─────────────────────────────────────────────────────────────────────────────
// Direct WebSocket signaling server (P1-5 demo / dev path)
// ─────────────────────────────────────────────────────────────────────────────

/// Run the direct signaling server. Binds the WebSocket, then serves one peer at
/// a time (single-session — a new peer rebuilds the pipeline). This is the
/// browser's entry point for the P1-5 demo.
pub async fn serve_direct(addr: &str, cfg: SessionConfig) -> Result<()> {
    let listener = tokio::net::TcpListener::bind(addr)
        .await
        .with_context(|| format!("failed to bind signaling socket on {addr}"))?;
    tracing::info!("session signaling server listening on ws://{addr} (agent is the offerer)");

    loop {
        let (stream, peer) = listener.accept().await.context("accept failed")?;
        tracing::info!("client connected from {peer}");
        if let Err(e) = handle_peer(stream, &cfg).await {
            tracing::error!(
                token = "session-server-ended-with-error",
                "session ended with error: {e:#}"
            );
        } else {
            tracing::info!("session ended cleanly");
        }
        tracing::info!("waiting for next connection (one session at a time)");
    }
}

async fn handle_peer(stream: tokio::net::TcpStream, cfg: &SessionConfig) -> Result<()> {
    let ws = tokio_tungstenite::accept_async(stream)
        .await
        .context("websocket handshake failed")?;
    let (mut ws_sink, mut ws_stream) = ws.split();

    let (out_tx, mut out_rx) = mpsc::unbounded_channel::<SignalMsg>();

    // Virtual input devices + PulseAudio sidecar for the demo session. The host
    // launches the app container when the compositor's socket appears on the bus,
    // and tears everything down on return.
    let (mut host, pulse_server) =
        SessionHost::prepare("demo", cfg).context("prepare session host")?;

    // Build an effective cfg with the pulse socket threaded in (same pattern as
    // runner.rs). We don't mutate the caller's cfg; clone for the pipeline call.
    let mut cfg_eff = cfg.clone();
    cfg_eff.pulse_server = pulse_server;
    if cfg_eff.pulse_server.is_none() && !cfg_eff.use_test_audio {
        // Same policy as runner.rs: record WHY and honour QUASAR_AUDIO_REQUIRED
        // rather than silently streaming silence. No diagnostic lane here, so the
        // reason lands on cfg + the log only.
        let reason = host
            .audio_degraded_reason()
            .unwrap_or("PulseAudio sidecar unavailable")
            .to_string();
        if cfg_eff.audio_required {
            anyhow::bail!("audio required but unavailable: {reason}");
        }
        tracing::warn!(
            token = "audio-fallback-silent",
            "{reason} — switching to silent audio fallback"
        );
        cfg_eff.audio_degraded_reason = Some(reason);
        cfg_eff.use_test_audio = true;
    }

    let (pipeline, webrtc, _data_channel, audio_webrtc) =
        pipeline::build_session_pipeline(&cfg_eff, out_tx.clone(), host.devices.clone())
            .context("failed to build session pipeline")?;

    // Going to PLAYING triggers on-negotiation-needed, which creates the offer
    // (with the video + audio m-lines and the "input" DataChannel present).
    pipeline
        .set_state(gst::State::Playing)
        .context("session pipeline failed to reach PLAYING")?;

    // VK-05: same post-PLAYING rearm as runner.rs — gst 1.28.4 drops a
    // pre-PLAYING rate-control set on vulkanh264enc (CQP fallback otherwise).
    if cfg_eff.encoder == crate::session::EncoderChoice::Vulkan {
        if let Some(enc) = pipeline.by_name(pipeline::VULKAN_ENCODER_NAME) {
            pipeline::rearm_vulkan_rc(&enc, cfg_eff.stream.bitrate_kbps);
        } else {
            tracing::warn!(
                token = "vulkan-encoder-rearm-not-found",
                "VK-05: Vulkan encoder ({}) not found in demo pipeline for rearm — \
                 encoder may be stuck at CQP",
                pipeline::VULKAN_ENCODER_NAME
            );
        }
    }

    let bus = pipeline.bus().context("pipeline has no bus")?;
    let mut bus_stream = bus.stream();

    let result = signaling_loop(
        &mut ws_sink,
        &mut ws_stream,
        &mut out_rx,
        &mut bus_stream,
        &webrtc,
        audio_webrtc.as_ref(),
        Role::Offerer,
        &out_tx,
        Some(&mut host),
    )
    .await;

    host.teardown();
    pipeline.set_state(gst::State::Null).ok();
    result
}

// ─────────────────────────────────────────────────────────────────────────────
// Loopback answerer (in-container negotiation proof for the P1-5 acceptance)
// ─────────────────────────────────────────────────────────────────────────────

/// Connect to a running session's signaling WebSocket as the answerer and reach
/// ICE "connected" over loopback. Used by the acceptance test to prove the
/// graduated negotiation works at a given resolution without a browser.
pub async fn run_answerer(url: &str) -> Result<()> {
    tracing::info!("answerer connecting to {url}");
    let (ws, _resp) = tokio_tungstenite::connect_async(url)
        .await
        .with_context(|| format!("failed to connect to {url}"))?;
    let (mut ws_sink, mut ws_stream) = ws.split();

    let (out_tx, mut out_rx) = mpsc::unbounded_channel::<SignalMsg>();
    let (pipeline, webrtc) = pipeline::build_answerer_pipeline(out_tx.clone())
        .context("failed to build answerer pipeline")?;

    pipeline
        .set_state(gst::State::Playing)
        .context("answerer pipeline failed to reach PLAYING")?;

    let bus = pipeline.bus().context("pipeline has no bus")?;
    let mut bus_stream = bus.stream();

    let result = signaling_loop(
        &mut ws_sink,
        &mut ws_stream,
        &mut out_rx,
        &mut bus_stream,
        &webrtc,
        None,
        Role::Answerer,
        &out_tx,
        None,
    )
    .await;

    pipeline.set_state(gst::State::Null).ok();
    result
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared signaling loop + inbound dispatch
// ─────────────────────────────────────────────────────────────────────────────

/// Dispatch one inbound signaling message to the correct `webrtcbin` based on
/// the `pc` field (#304). P1-6's relay transport can reuse this verbatim once it
/// lands (it is transport-agnostic). `audio_webrtc` is `None` when audio is
/// disabled or the demo path didn't create one; `pc: "audio"` messages are then
/// logged + dropped (the browser shouldn't send them if no audio offer went out).
fn handle_inbound(
    msg: SignalMsg,
    webrtc: &gst::Element,
    audio_webrtc: Option<&gst::Element>,
    role: Role,
    out_tx: &OutTx,
    srd_fail_tx: Option<&SrdFailTx>,
    srd_ok_tx: Option<&SrdOkTx>,
) -> Result<()> {
    match msg {
        SignalMsg::Answer { sdp, pc } => {
            if role != Role::Offerer {
                tracing::warn!(
                    token = "signaling-unexpected-answer",
                    "answerer received an unexpected answer — ignoring"
                );
                return Ok(());
            }
            let Some(target) = webrtc_for_pc(pc, webrtc, audio_webrtc) else {
                tracing::warn!(
                    token = "audio-pc-absent-answer-dropped",
                    "dropping answer addressed to the audio PC — no audio webrtcbin exists on this session (audio disabled); NOT retargeting video"
                );
                return Ok(());
            };
            tracing::info!("received answer ({} bytes, pc={})", sdp.len(), pc_name(pc));
            pipeline::apply_remote_description(
                target,
                &sdp,
                gstreamer_webrtc::WebRTCSDPType::Answer,
                None,
                pc.unwrap_or_default(),
                srd_fail_tx.cloned(),
                srd_ok_tx.cloned(),
            )?;
        }
        SignalMsg::Offer { sdp, pc } => {
            if role != Role::Answerer {
                tracing::warn!(
                    token = "signaling-unexpected-offer",
                    "offerer received an unexpected offer — ignoring"
                );
                return Ok(());
            }
            let Some(target) = webrtc_for_pc(pc, webrtc, audio_webrtc) else {
                tracing::warn!(
                    token = "audio-pc-absent-offer-dropped",
                    "dropping offer addressed to the audio PC — no audio webrtcbin exists on this session (audio disabled); NOT retargeting video"
                );
                return Ok(());
            };
            tracing::info!("received offer ({} bytes, pc={})", sdp.len(), pc_name(pc));
            pipeline::apply_remote_description(
                target,
                &sdp,
                gstreamer_webrtc::WebRTCSDPType::Offer,
                Some(out_tx.clone()),
                pc.unwrap_or_default(),
                srd_fail_tx.cloned(),
                // An inbound OFFER never carries an answer's m-line disposition — the
                // agent is the offerer in production, and the demo answerer's own answer
                // is created below, not received.
                None,
            )?;
        }
        SignalMsg::Ice { candidate, pc } => {
            let Some(target) = webrtc_for_pc(pc, webrtc, audio_webrtc) else {
                tracing::warn!(
                    token = "audio-pc-absent-ice-dropped",
                    "dropping ICE candidate addressed to the audio PC — no audio webrtcbin exists on this session (audio disabled); NOT retargeting video"
                );
                return Ok(());
            };
            let mline = candidate.sdp_m_line_index.unwrap_or(0);
            tracing::info!(
                "adding remote ICE candidate (pc={}, mline={mline}): {}",
                pc_name(pc),
                candidate.candidate
            );
            target.emit_by_name::<()>("add-ice-candidate", &[&mline, &candidate.candidate]);
        }
        SignalMsg::RestartIce { pc } => {
            if role != Role::Offerer {
                tracing::warn!(
                    token = "signaling-ice-restart-ignored",
                    "answerer received an ICE restart request — ignoring"
                );
                return Ok(());
            }
            let Some(target) = webrtc_for_pc(pc, webrtc, audio_webrtc) else {
                // #523: a relayed pc:audio restart_ice on an audio-disabled
                // session must be a no-op, never a silent retarget onto the
                // video webrtcbin (that ICE-restarts video and regenerates a
                // second video offer, partially re-opening the #505
                // double-offer shape).
                tracing::warn!(
                    token = "audio-pc-absent-restart-dropped",
                    "dropping ICE restart addressed to the audio PC — no audio webrtcbin exists on this session (audio disabled); NOT retargeting video"
                );
                return Ok(());
            };
            let pc = pc.unwrap_or_default();
            tracing::info!("ICE restart requested ({pc} PC) — creating fresh offer");
            // This demo/relay-shared transport never threads a session id through
            // to here; a rare ICE-restart offer's "offer created" line stays
            // untagged (session="").
            pipeline::restart_ice(target, out_tx.clone(), pc, String::new());
        }
        SignalMsg::Error { message } => tracing::warn!(
            token = "signaling-peer-error",
            "peer signaled error: {message}"
        ),
        SignalMsg::Bye => tracing::info!("peer signaled bye"),
    }
    Ok(())
}

/// Pure routing decision for a `pc`-addressed signaling message (#523): which
/// physical webrtcbin (if any) it targets. Video is always available. Audio is
/// only a target when `audio_present` — an audio-disabled session
/// (`QUASAR_AUDIO_DISABLED=1`) has no audio webrtcbin, and `pc: "audio"` must
/// resolve to `AudioAbsent` rather than silently falling back to video.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum PcTarget {
    Video,
    Audio,
    /// `pc: "audio"` was requested but this session has no audio webrtcbin.
    AudioAbsent,
}

fn resolve_pc_target(pc: Option<PcId>, audio_present: bool) -> PcTarget {
    match pc {
        Some(PcId::Audio) if audio_present => PcTarget::Audio,
        Some(PcId::Audio) => PcTarget::AudioAbsent,
        _ => PcTarget::Video,
    }
}

/// Resolve a `pc` field to the corresponding webrtcbin element. Defaults to the
/// video webrtcbin when `pc` is absent (backwards-compatible) or `Video`.
/// Returns `None` when `pc: "audio"` was requested but no audio webrtcbin
/// exists for this session — callers must treat that as a drop, never a
/// retarget onto video (#523).
fn webrtc_for_pc<'a>(
    pc: Option<PcId>,
    video: &'a gst::Element,
    audio: Option<&'a gst::Element>,
) -> Option<&'a gst::Element> {
    match resolve_pc_target(pc, audio.is_some()) {
        PcTarget::Video => Some(video),
        PcTarget::Audio => audio,
        PcTarget::AudioAbsent => None,
    }
}

/// Human-readable name for logging.
fn pc_name(pc: Option<PcId>) -> &'static str {
    match pc {
        Some(PcId::Audio) => "audio",
        Some(PcId::Video) => "video",
        None => "video(default)",
    }
}

/// Apply one inbound signaling message to the correct `webrtcbin` as the
/// offerer: accept an answer, add an ICE candidate, or log diagnostics. Used by
/// the P1-7 relay path (runner.rs drains inbound relay messages and calls this).
/// Only the offerer applies answers; offers are ignored (we created the offer).
/// `audio_webrtc` routes `pc: "audio"` messages to the audio webrtcbin (#304).
pub(crate) fn apply_inbound_as_offerer(
    msg: SignalMsg,
    webrtc: &gst::Element,
    audio_webrtc: Option<&gst::Element>,
    out_tx: &OutTx,
    srd_fail_tx: Option<&SrdFailTx>,
    srd_ok_tx: Option<&SrdOkTx>,
) -> anyhow::Result<()> {
    handle_inbound(
        msg,
        webrtc,
        audio_webrtc,
        Role::Offerer,
        out_tx,
        srd_fail_tx,
        srd_ok_tx,
    )
}

/// The core select loop shared by both roles: pump outbound signaling to the
/// WebSocket, dispatch inbound signaling to `webrtcbin`, surface pipeline bus
/// errors, and (#503) end the session when webrtcbin refuses a remote
/// description.
///
/// The #503 arm matters to both roles: a refused description means that
/// PeerConnection never carries media. For the answerer it is doubly
/// load-bearing — `apply_remote_description` skips `create_answer` on a rejected
/// offer, so without this arm `run_answerer` would loop forever having sent none.
#[allow(clippy::too_many_arguments)]
async fn signaling_loop<S, R>(
    ws_sink: &mut S,
    ws_stream: &mut R,
    out_rx: &mut mpsc::UnboundedReceiver<SignalMsg>,
    bus_stream: &mut gst::bus::BusStream,
    webrtc: &gst::Element,
    audio_webrtc: Option<&gst::Element>,
    role: Role,
    out_tx: &OutTx,
    mut host: Option<&mut SessionHost>,
) -> Result<()>
where
    S: SinkExt<Message> + Unpin,
    <S as futures_util::Sink<Message>>::Error: std::error::Error + Send + Sync + 'static,
    R: StreamExt<Item = Result<Message, tokio_tungstenite::tungstenite::Error>> + Unpin,
{
    let (srd_fail_tx, mut srd_fail_rx) = tokio::sync::mpsc::unbounded_channel();
    loop {
        tokio::select! {
            outbound = out_rx.recv() => {
                match outbound {
                    Some(msg) => {
                        let json = msg.to_json().context("failed to serialize signaling message")?;
                        ws_sink.send(Message::Text(json)).await
                            .map_err(|e| anyhow!("websocket send failed: {e}"))?;
                    }
                    None => break, // all senders dropped (pipeline torn down)
                }
            }
            inbound = ws_stream.next() => {
                match inbound {
                    Some(Ok(Message::Text(txt))) => {
                        match SignalMsg::from_json(txt.as_str()) {
                            Ok(msg) => handle_inbound(
                                msg, webrtc, audio_webrtc, role, out_tx, Some(&srd_fail_tx),
                                // The P1-5 demo/loopback path emits no trace events (it
                                // has no session id and no control plane to send to).
                                None,
                            )?,
                            Err(e) => tracing::warn!(
                                token = "signaling-frame-malformed","ignoring malformed signaling frame: {e}"),
                        }
                    }
                    Some(Ok(Message::Close(_))) | None => {
                        tracing::info!("signaling websocket closed");
                        break;
                    }
                    Some(Ok(_)) => {} // ignore binary / ping / pong
                    Some(Err(e)) => {
                        tracing::warn!(
                            token = "signaling-websocket-error","websocket error: {e}");
                        break;
                    }
                }
            }
            failure = srd_fail_rx.recv() => {
                match failure {
                    // A duplicate answer settles nothing and breaks nothing (same
                    // disposition as the runner's srd_fail_rx lane) — never end
                    // the demo/loopback session over it.
                    Some(f) if f.kind.is_benign() => {
                        tracing::warn!(
                            token = "webrtc-duplicate-answer",
                            "ignoring a duplicate remote answer on the {} PC: {}",
                            f.pc, f.reason,
                        );
                    }
                    Some(f) => {
                        let reason =
                            pipeline::remote_description_failure_reason(f.pc, &f.reason);
                        tracing::error!(
                            token = "session-server-fatal","{reason}");
                        return Err(anyhow!("{reason}"));
                    }
                    // srd_fail_tx outlives this loop, so recv() cannot return None
                    // while it runs — break anyway, or a stuck None spins the
                    // select at 100% CPU.
                    None => break,
                }
            }
            bus_msg = bus_stream.next() => {
                if let Some(bus_msg) = bus_msg {
                    // Launch the app container once the compositor announces its
                    // Wayland socket (demo path mirrors the agent runner).
                    if let Some(h) = host.as_deref_mut() {
                        h.on_bus_message(&bus_msg);
                    }
                    match bus_msg.view() {
                        gst::MessageView::Error(err) => {
                            return Err(anyhow!(
                                "pipeline error: {} ({:?})",
                                err.error(),
                                err.debug()
                            ));
                        }
                        gst::MessageView::Eos(_) => {
                            tracing::info!("pipeline reached EOS");
                            break;
                        }
                        _ => {}
                    }
                }
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod pc_routing_tests {
    //! #523: the routing decision for pc-addressed signaling messages, in
    //! isolation from `gst::Element` construction (no GStreamer init needed).

    use super::{resolve_pc_target, PcTarget};
    use crate::session::signaling::PcId;

    #[test]
    fn video_target_when_pc_absent() {
        assert_eq!(resolve_pc_target(None, true), PcTarget::Video);
        assert_eq!(resolve_pc_target(None, false), PcTarget::Video);
    }

    #[test]
    fn video_target_when_pc_is_video() {
        assert_eq!(resolve_pc_target(Some(PcId::Video), true), PcTarget::Video);
        assert_eq!(resolve_pc_target(Some(PcId::Video), false), PcTarget::Video);
    }

    #[test]
    fn audio_target_when_pc_is_audio_and_present() {
        assert_eq!(resolve_pc_target(Some(PcId::Audio), true), PcTarget::Audio);
    }

    #[test]
    fn audio_absent_never_falls_back_to_video() {
        // The #523 regression: pc:audio on an audio-disabled session (no
        // audio webrtcbin) must resolve to AudioAbsent, never Video.
        assert_eq!(
            resolve_pc_target(Some(PcId::Audio), false),
            PcTarget::AudioAbsent
        );
    }
}
