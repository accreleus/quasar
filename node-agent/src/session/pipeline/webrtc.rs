//! WebRTC negotiation + signaling glue: offer/answer creation, SDP munging, ICE
//! trickle, DataChannel input wiring, connection-state observation, the webrtcbin
//! factory, and the browserless answerer test pipeline. Transport implementation #1
//! (the `[transport slot]` of the media path).
//!
//! Wire contract: `protocol/signaling.md`. The TWCC / abs-capture-time RTP-extension
//! constants live in `super`, shared with the payloader caps in
//! `super::build_encode_pipeline` and `super::rtp_ext`.

use std::sync::Arc;

use anyhow::{anyhow, Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;

use crate::session::fec::{self, FecAction, FecController, FecControllerConfig};
use crate::session::input::InputSink;
use crate::session::metrics::SessionMetrics;
use crate::session::runner::TraceEvent;
use crate::session::signaling::{IceCandidate, PcId, SignalMsg};
use crate::session::virtual_input::VirtualDevices;

use super::{OutTx, TraceTx};

/// On `on-negotiation-needed`, create the offer (agent only).
///
/// MUST negotiate exactly once: the offer already carries the video m-line, the audio
/// m-line and the "input" DataChannel, and the pipeline is static. Chrome ≥149 answers
/// in a way that re-fires negotiation-needed right after set-remote-description, and
/// re-offering trips an internal assertion (`sdp_media_from_transceiver: mline`) that
/// SIGABRTs the agent. Multi-session means a new webrtcbin per peer, each negotiating
/// once, so this is an invariant, not a workaround.
///
/// `session_id` is empty on the demo/test-src path (no control-plane WS), same convention
/// as `connect_state_logging`. It reaches the "offer created" line, which the qses
/// "exactly one offer per SID" log assertion greps.
pub(super) fn connect_negotiation_needed(
    webrtc: &gst::Element,
    out_tx: OutTx,
    pc: PcId,
    session_id: String,
) {
    let weak = webrtc.downgrade();
    let offered = std::sync::atomic::AtomicBool::new(false);
    webrtc.connect("on-negotiation-needed", false, move |_values| {
        let webrtc = weak.upgrade()?;
        if offered.swap(true, std::sync::atomic::Ordering::SeqCst) {
            tracing::info!("ignoring renegotiation request ({pc} PC negotiates once)");
            return None;
        }
        tracing::info!("negotiation needed ({pc} PC) — creating offer");
        create_offer(&webrtc, out_tx.clone(), pc, session_id.clone());
        None
    });
}

/// Add two fmtp parameters webrtcbin does not emit. Only the transmitted text is munged,
/// not the local description: both are receiver-side parameters the answerer acts on, not
/// transport state webrtcbin must agree with.
/// - H.264 `profile-level-id` line: `level-asymmetry-allowed=1`, so a browser may answer
///   with a different H.264 level than it receives instead of rejecting/transcoding.
/// - RTX `apt=` lines: `rtx-time=125` bounds the receiver's retransmit buffer to 125 ms;
///   a retransmit older than ~2 frame intervals only delays recovery.
fn munge_offer_sdp(sdp: &str) -> String {
    sdp.split("\r\n")
        .map(|line| {
            if line.starts_with("a=fmtp:") {
                if line.contains("profile-level-id") && !line.contains("level-asymmetry-allowed") {
                    return format!("{line};level-asymmetry-allowed=1");
                }
                if line.contains("apt=") && !line.contains("rtx-time") {
                    return format!("{line};rtx-time=125");
                }
            }
            line.to_string()
        })
        .collect::<Vec<_>>()
        .join("\r\n")
}

fn create_offer(webrtc: &gst::Element, out_tx: OutTx, pc: PcId, session_id: String) {
    create_offer_with_options(webrtc, out_tx, pc, None, session_id);
}

/// Generate a new offer with fresh ICE credentials while keeping the live
/// pipeline and transceivers intact.
pub(crate) fn restart_ice(webrtc: &gst::Element, out_tx: OutTx, pc: PcId, session_id: String) {
    let options = gst::Structure::builder("offer-options")
        .field("ice-restart", true)
        .build();
    create_offer_with_options(webrtc, out_tx, pc, Some(options), session_id);
}

fn create_offer_with_options(
    webrtc: &gst::Element,
    out_tx: OutTx,
    pc: PcId,
    options: Option<gst::Structure>,
    session_id: String,
) {
    let webrtc2 = webrtc.clone();
    let promise = gst::Promise::with_change_func(move |reply| {
        let sdp = match session_description_from_reply(reply, "offer") {
            Ok(desc) => desc,
            Err(e) => {
                tracing::error!(
                    token = "create-offer-failed",
                    "create-offer ({pc}) failed: {e}"
                );
                return;
            }
        };
        webrtc2.emit_by_name::<()>("set-local-description", &[&sdp, &None::<gst::Promise>]);
        let sdp_text = match sdp.sdp().as_text() {
            Ok(t) => t,
            Err(e) => {
                tracing::error!(
                    token = "offer-serialize-failed",
                    "offer ({pc}) SDP failed to serialize: {e}"
                );
                return;
            }
        };
        let text = munge_offer_sdp(&sdp_text);
        // The TWCC negotiation surface (extmap + rtcp-fb): both absent is normal with ABR
        // off, but with ABR armed either one missing means rtpgccbwe is blind. The
        // abs-capture-time extmap must always be present (always-on extension). The
        // `session=<id>` field is what the qses "one offer per SID" assertion greps.
        tracing::info!(
            "offer created ({pc} PC, {} bytes) session={session_id} — twcc extmap={} \
             rtcp-fb-transport-cc={} abs-capture-time extmap={} — sending to peer",
            text.len(),
            text.contains(super::RTP_TWCC_URI),
            text.contains("transport-cc"),
            text.contains(super::ABS_CAPTURE_TIME_URI),
        );
        let _ = out_tx.send(SignalMsg::Offer {
            sdp: text,
            pc: Some(pc),
        });
    });
    webrtc.emit_by_name::<()>("create-offer", &[&options, &promise]);
}

// ─────────────────────────────────────────────────────────────────────────────
// Answerer pipeline (in-container loopback test peer — see server::run_answerer)
// ─────────────────────────────────────────────────────────────────────────────

/// A minimal answerer pipeline: a `webrtcbin` consuming incoming media into fakesinks.
/// A deterministic, browserless negotiation proof (reaches ICE connected, receives the
/// video+audio tracks + DataChannel) so the P1-5 acceptance runs in-container. Not a real
/// client — the real client is the browser.
pub(crate) fn build_answerer_pipeline(out_tx: OutTx) -> Result<(gst::Pipeline, gst::Element)> {
    let pipeline = gst::Pipeline::new();
    let webrtc = make_webrtcbin(None)?;
    pipeline.add(&webrtc)?;

    connect_ice_candidate(&webrtc, out_tx, PcId::Video);
    // A loopback test peer: no virtual input devices, no control-plane WS.
    connect_state_logging(&webrtc, None, None, String::new());

    // Consume incoming media so the transport negotiates and the pipeline doesn't stall on
    // an unlinked pad.
    let pipeline_weak = pipeline.downgrade();
    webrtc.connect_pad_added(move |_webrtc, pad| {
        if pad.direction() != gst::PadDirection::Src {
            return;
        }
        let Some(pipeline) = pipeline_weak.upgrade() else {
            return;
        };
        let sink = match gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .property("async", false)
            .build()
        {
            Ok(s) => s,
            Err(e) => {
                tracing::warn!(
                    token = "answerer-fakesink-build-failed",
                    "answerer: fakesink build failed: {e}"
                );
                return;
            }
        };
        if pipeline.add(&sink).is_err() {
            return;
        }
        let _ = sink.sync_state_with_parent();
        if let Some(sink_pad) = sink.static_pad("sink") {
            if let Err(e) = pad.link(&sink_pad) {
                tracing::warn!(
                    token = "answerer-pad-link-failed",
                    "answerer: failed to link incoming pad: {e:?}"
                );
            } else {
                tracing::info!("answerer: incoming media linked to fakesink");
            }
        }
    });

    // Proves the agent's DataChannel arrived in the offer.
    webrtc.connect("on-data-channel", false, |values| {
        if let Ok(dc) = values[1].get::<glib::Object>() {
            let label: String = dc.property("label");
            tracing::info!("answerer: received DataChannel '{label}'");
        }
        None
    });

    Ok((pipeline, webrtc))
}

fn create_answer(webrtc: &gst::Element, out_tx: OutTx) {
    let webrtc2 = webrtc.clone();
    let promise = gst::Promise::with_change_func(move |reply| {
        let sdp = match session_description_from_reply(reply, "answer") {
            Ok(desc) => desc,
            Err(e) => {
                tracing::error!(token = "create-answer-failed", "create-answer failed: {e}");
                return;
            }
        };
        webrtc2.emit_by_name::<()>("set-local-description", &[&sdp, &None::<gst::Promise>]);
        let text = match sdp.sdp().as_text() {
            Ok(t) => t,
            Err(e) => {
                tracing::error!(
                    token = "answer-serialize-failed",
                    "answer SDP failed to serialize: {e}"
                );
                return;
            }
        };
        tracing::info!("answer created ({} bytes) — sending to agent", text.len());
        let _ = out_tx.send(SignalMsg::Answer {
            sdp: text,
            pc: None,
        });
    });
    webrtc.emit_by_name::<()>("create-answer", &[&None::<gst::Structure>, &promise]);
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared element builders + callbacks
// ─────────────────────────────────────────────────────────────────────────────

pub(super) fn make_webrtcbin(stun: Option<&str>) -> Result<gst::Element> {
    let webrtc = gst::ElementFactory::make("webrtcbin")
        .name("sendrecv")
        // max-bundle: one transport for everything (video + audio + DataChannel).
        .property_from_str("bundle-policy", "max-bundle")
        .build()
        .context("webrtcbin not found — is gstreamer1.0-plugins-bad installed?")?;
    if let Some(stun) = stun {
        webrtc.set_property("stun-server", stun);
        tracing::info!("using STUN server: {stun}");
    }
    Ok(webrtc)
}

/// #425: set the rtpbin jitter-buffer target (ms) on the AUDIO webrtcbin. NEVER call it on
/// the video webrtcbin — the video PC's buffering is untouched by design. Knob:
/// `QUASAR_MIC_JITTER_MS`.
///
/// The audio PC's only receive direction is the microphone, so this is the mic receive-leg
/// jitter buffer; webrtcbin's 200 ms default dominated the ~250 ms round-trip measured on
/// the mic loopback test. `find_property`-guarded so a future binding regression warns
/// instead of panicking.
pub(super) fn set_audio_receive_latency(webrtc: &gst::Element, latency_ms: u32) {
    if webrtc.find_property("latency").is_none() {
        tracing::warn!(
            token = "mic-jitter-latency-unavailable",
            "audio webrtcbin has no 'latency' property on this build — \
             QUASAR_MIC_JITTER_MS ({latency_ms} ms) not applied, mic receive \
             latency stays at the webrtcbin default (200 ms)"
        );
        return;
    }
    webrtc.set_property("latency", latency_ms);
    // Grep this token post-deploy to confirm the audio PC took the configured target.
    tracing::info!(
        token = "mic-jitter-latency-applied",
        "audio webrtcbin latency = {latency_ms} ms (QUASAR_MIC_JITTER_MS; mic receive \
         leg only — video PC unaffected)"
    );
}

/// The real `a=mid` of the m-line at `index` in webrtcbin's local description. webrtcbin's
/// mids are not the bare index (it emits `video0`, `audio1`), and Chrome ≥149 rejects an
/// ICE candidate whose `sdpMid` matches no mid in the remote description, so mirroring the
/// index silently breaks ICE there.
fn mid_for_mline(webrtc: &gst::Element, index: u32) -> Option<String> {
    let desc = webrtc
        .property::<Option<gstreamer_webrtc::WebRTCSessionDescription>>("local-description")?;
    let media = desc.sdp().media(index)?;
    media.attribute_val("mid").map(|s| s.to_string())
}

/// Enable NACK-driven RTX on the video transceiver (index 0). Fails open: RTX is a
/// resilience optimisation, so a missing transceiver or `do-nack` logs and continues.
pub(super) fn enable_video_rtx(webrtc: &gst::Element) {
    let transceiver = webrtc.emit_by_name::<Option<gstreamer_webrtc::WebRTCRTPTransceiver>>(
        "get-transceiver",
        &[&0i32],
    );
    match transceiver {
        Some(t) if t.find_property("do-nack").is_some() => {
            t.set_property("do-nack", true);
            tracing::info!("enabled NACK/RTX on the video transceiver (do-nack=true)");
        }
        Some(_) => tracing::warn!(
            token = "rtx-no-do-nack",
            "video transceiver has no 'do-nack' — RTX not enabled"
        ),
        None => tracing::warn!(
            token = "rtx-transceiver-missing",
            "could not fetch video transceiver — RTX not enabled"
        ),
    }
}

/// Enable send-side ULPFEC/RED on the video transceiver (index 0), so a receiver on a
/// lossy link reconstructs immediately rather than waiting on a NACK/RTX round-trip or the
/// next keyframe. Knob: `QUASAR_FEC_PERCENTAGE`. Fails open like [`enable_video_rtx`].
///
/// `negotiate == false` leaves the transceiver untouched (no `red`/`ulpfec` in the offer).
/// `negotiate == true` sets `fec-type=ulp-red` and `fec-percentage=initial_pct` EVEN AT 0:
/// the `auto` premise is to negotiate those lines up front while no repair data flows, so
/// [`spawn_fec_controller`] can ramp 0→N→0 with no SDP renegotiation.
pub(super) fn enable_video_fec(webrtc: &gst::Element, negotiate: bool, initial_pct: u32) {
    if !negotiate {
        return;
    }
    let transceiver = webrtc.emit_by_name::<Option<gstreamer_webrtc::WebRTCRTPTransceiver>>(
        "get-transceiver",
        &[&0i32],
    );
    match transceiver {
        Some(t)
            if t.find_property("fec-type").is_some()
                && t.find_property("fec-percentage").is_some() =>
        {
            // Enum property, so `set_property_from_str` (.claude/rules/gstreamer-gotchas.md);
            // the typed `WebRTCFECType` is feature-gated off in gstreamer-webrtc 0.23.
            t.set_property_from_str("fec-type", "ulp-red");
            t.set_property("fec-percentage", initial_pct);
            tracing::info!(
                "enabled ULPFEC/RED on the video transceiver (fec-type=ulp-red, fec-percentage={initial_pct})"
            );
        }
        Some(_) => {
            tracing::warn!(
                token = "fec-properties-missing",
                "video transceiver has no 'fec-type'/'fec-percentage' — FEC not enabled"
            )
        }
        None => tracing::warn!(
            token = "fec-transceiver-missing",
            "could not fetch video transceiver — FEC not enabled"
        ),
    }
}

/// Cumulative `(packets_lost, packets_sent)` for auto FEC, from a synchronous `get-stats`
/// walked for `remote-inbound-rtp packets-lost` (RTCP RR-derived) and `outbound-rtp
/// packets-sent`. This webrtcbin carries VIDEO ONLY (audio is a separate pipeline/PC,
/// #304), so no SSRC/kind disambiguation is needed. `None` until a counter exists.
fn poll_video_loss(webrtc: &gst::Element) -> Option<(i64, u64)> {
    let promise = gst::Promise::new();
    webrtc.emit_by_name::<()>("get-stats", &[&None::<gst::Pad>, &promise]);
    // Blocks this thread until the stats are gathered. Not on the latency path: this is a
    // dedicated 0.2 Hz poll thread.
    let _ = promise.wait();
    let reply = promise.get_reply()?;

    let mut lost: Option<i64> = None;
    let mut sent: Option<u64> = None;
    for (_field, value) in reply.iter() {
        let Ok(stat) = value.get::<gst::Structure>() else {
            continue;
        };
        // `type` is a GstWebRTCStatsType enum; read its nick string.
        let ty = stat
            .value("type")
            .ok()
            .and_then(|v| v.serialize().ok())
            .map(|s| s.to_string());
        match ty.as_deref() {
            Some("remote-inbound-rtp") => {
                // A signed cumulative counter (gint / gint64).
                let v = stat
                    .get::<i64>("packets-lost")
                    .or_else(|_| stat.get::<i32>("packets-lost").map(i64::from));
                if let Ok(v) = v {
                    lost = Some(v);
                }
            }
            Some("outbound-rtp") => {
                if let Ok(v) = stat
                    .get::<u64>("packets-sent")
                    .or_else(|_| stat.get::<u32>("packets-sent").map(u64::from))
                {
                    sent = Some(v);
                }
            }
            _ => {}
        }
    }
    match (lost, sent) {
        (None, None) => None,
        // One counter missing (no RR yet) reads as zero for that side: a clean window,
        // never a spurious arm.
        (l, s) => Some((l.unwrap_or(0), s.unwrap_or(0))),
    }
}

/// Spawn the auto-FEC controller: a `window_s` (default 5 s) poll thread reading the
/// agent-local loss signal off `get-stats`, feeding the pure hysteresis [`FecController`],
/// and writing `fec-percentage` on each transition — 0→N in one step on sustained loss,
/// N→0 after a clean run.
///
/// Lifetime: it holds a `std::sync::Weak<SessionMetrics>` for liveness (the glib `WeakRef`
/// is `!Send`) and exits within one poll tick of teardown dropping it, so no stop plumbing
/// is needed. The transceiver + webrtcbin are strong glib handles, released on exit.
pub(super) fn spawn_fec_controller(
    webrtc: &gst::Element,
    cfg: FecControllerConfig,
    metrics: std::sync::Weak<SessionMetrics>,
    trace_tx: TraceTx,
    session_id: String,
) {
    let transceiver = webrtc.emit_by_name::<Option<gstreamer_webrtc::WebRTCRTPTransceiver>>(
        "get-transceiver",
        &[&0i32],
    );
    let transceiver = match transceiver {
        Some(t) if t.find_property("fec-percentage").is_some() => t,
        _ => {
            tracing::warn!(
                token = "fec-auto-no-fec-percentage",
                "auto FEC: video transceiver has no 'fec-percentage' — controller not started"
            );
            return;
        }
    };
    let webrtc = webrtc.clone();
    let window = std::time::Duration::from_secs(cfg.window_s.max(1));
    let arm_secs = cfg.arm_windows as u64 * cfg.window_s;
    let disarm_secs = cfg.disarm_windows as u64 * cfg.window_s;
    tracing::info!(
        "auto FEC controller armed: window={}s, arm={} windows (≈{}s ≥{}% loss), \
         disarm={} windows (≈{}s clean), max_flaps={}, armed_pct={}%",
        cfg.window_s,
        cfg.arm_windows,
        arm_secs,
        cfg.arm_loss_pct,
        cfg.disarm_windows,
        disarm_secs,
        cfg.max_flaps,
        cfg.armed_pct,
    );

    let log_span = tracing::Span::current();
    let spawn = std::thread::Builder::new()
        .name("quasar-fec-ctl".into())
        .spawn(move || {
            // Re-enter the session span so this thread's lines carry session=<id>.
            let _log_span = log_span.enter();
            let mut controller = FecController::new(cfg);
            let mut last: Option<(i64, u64)> = None;
            // Liveness granularity, bounding teardown latency; the loss window is `window`.
            let tick = std::time::Duration::from_millis(500);
            let mut elapsed = std::time::Duration::ZERO;
            loop {
                std::thread::sleep(tick);
                if metrics.upgrade().is_none() {
                    break; // session gone — inherit teardown
                }
                elapsed += tick;
                if elapsed < window {
                    continue;
                }
                elapsed = std::time::Duration::ZERO;

                let Some((lost, sent)) = poll_video_loss(&webrtc) else {
                    continue; // no stats yet
                };
                let (dlost, dsent) = match last {
                    Some((pl, ps)) => (lost - pl, sent.saturating_sub(ps)),
                    None => {
                        // First sample seeds the baseline; no window yet.
                        last = Some((lost, sent));
                        continue;
                    }
                };
                last = Some((lost, sent));
                let loss_pct = fec::window_loss_pct(dlost, dsent);

                match controller.observe(loss_pct) {
                    Some(FecAction::Arm(pct)) => {
                        transceiver.set_property("fec-percentage", pct);
                        tracing::info!(
                            "fec auto-armed: loss={loss_pct:.2}% over {arm_secs}s → {pct}% \
                             (armed FEC overhead counts against the tier ceiling)"
                        );
                        emit_fec_event(&trace_tx, &session_id, "fec_armed", loss_pct, pct);
                    }
                    Some(FecAction::Disarm) => {
                        transceiver.set_property("fec-percentage", 0u32);
                        tracing::info!("fec auto-disarmed after {disarm_secs}s clean");
                        emit_fec_event(&trace_tx, &session_id, "fec_disarmed", loss_pct, 0);
                    }
                    None => {}
                }
            }
            tracing::debug!("auto FEC controller thread exiting (session gone)");
        });
    if let Err(e) = spawn {
        tracing::warn!(
            token = "fec-controller-spawn-failed",
            "auto FEC: failed to spawn controller thread: {e} — FEC stays at 0%"
        );
    }
}

/// Emit a `fec_armed` / `fec_disarmed` session-tracer event so transitions show up in the
/// diagnostic bundle and report time-series. Additive rows, no contract change.
fn emit_fec_event(
    trace_tx: &TraceTx,
    session_id: &str,
    event: &'static str,
    loss_pct: f64,
    pct: u32,
) {
    let ts = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as i64;
    let payload = serde_json::json!({
        "loss_pct": (loss_pct * 100.0).round() / 100.0,
        "fec_percentage": pct,
    });
    trace_tx.try_emit(
        session_id.to_string(),
        TraceEvent {
            ts_unix_ms: ts,
            event,
            payload,
        },
    );
}

/// Trickle local ICE candidates to the peer (both roles).
pub(super) fn connect_ice_candidate(webrtc: &gst::Element, out_tx: OutTx, pc: PcId) {
    let weak = webrtc.downgrade();
    webrtc.connect("on-ice-candidate", false, move |values| {
        let mline_index = match values[1].get::<u32>() {
            Ok(v) => v,
            Err(e) => {
                tracing::error!(
                    token = "ice-candidate-mline-failed","on-ice-candidate ({pc}): mline index extraction failed: {e} — skipping candidate");
                return None;
            }
        };
        let candidate = match values[2].get::<String>() {
            Ok(v) => v,
            Err(e) => {
                tracing::error!(
                    token = "ice-candidate-string-failed","on-ice-candidate ({pc}): candidate string extraction failed: {e} — skipping candidate");
                return None;
            }
        };
        // The real mid from the local description, so strict browsers accept the
        // candidate; the index string is only a fallback.
        let sdp_mid = weak
            .upgrade()
            .and_then(|w| mid_for_mline(&w, mline_index))
            .or_else(|| Some(mline_index.to_string()));
        tracing::info!("ICE candidate ({pc} PC, mline={mline_index} mid={sdp_mid:?}): {candidate}");
        let msg = SignalMsg::Ice {
            candidate: IceCandidate {
                candidate,
                sdp_mid,
                sdp_m_line_index: Some(mline_index),
            },
            pc: Some(pc),
        };
        let _ = out_tx.send(msg);
        None
    });
}

/// Log ICE / peer-connection transitions and release held inputs on disconnect (Wolf
/// #302). The `Connected`/`Completed` line is the transport-established acceptance signal.
///
/// `devices` is `None` on the test-src pipeline and the answerer. When present,
/// `Failed`/`Closed`/`Disconnected` triggers `release_all()` so a key held at disconnect
/// does not stay stuck in the compositor and game; releasing on a transient `Disconnected`
/// is intentional, since a stuck key is worse than a dropped hold. `trace_tx` /
/// `session_id` are empty on the demo path (no control-plane WS).
pub(super) fn connect_state_logging(
    webrtc: &gst::Element,
    devices: Option<Arc<VirtualDevices>>,
    trace_tx: Option<TraceTx>,
    session_id: String,
) {
    let ice_trace = trace_tx.clone();
    let ice_sid = session_id.clone();
    webrtc.connect_notify(Some("ice-connection-state"), move |webrtc, _pspec| {
        let state =
            webrtc.property::<gstreamer_webrtc::WebRTCICEConnectionState>("ice-connection-state");
        tracing::info!("ICE connection state: {state:?}");
        // ICE may jump Checking → Completed, skipping `Connected`. Both mean established.
        if matches!(
            state,
            gstreamer_webrtc::WebRTCICEConnectionState::Connected
                | gstreamer_webrtc::WebRTCICEConnectionState::Completed
        ) {
            tracing::info!("✅ ICE CONNECTED — peer reachable, transport established");
        }
        if let Some(ref tx) = ice_trace {
            let ts = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis() as i64;
            let payload = serde_json::json!({
                "kind": "ice",
                "to": format!("{state:?}").to_lowercase(),
            });
            tx.try_emit(
                ice_sid.clone(),
                TraceEvent {
                    ts_unix_ms: ts,
                    event: "webrtc.state_changed",
                    payload,
                },
            );
        }
    });
    webrtc.connect_notify(Some("connection-state"), move |webrtc, _pspec| {
        let state =
            webrtc.property::<gstreamer_webrtc::WebRTCPeerConnectionState>("connection-state");
        tracing::info!("peer connection state: {state:?}");
        if state == gstreamer_webrtc::WebRTCPeerConnectionState::Connected {
            tracing::info!("✅ PEER CONNECTION CONNECTED");
        }
        if matches!(
            state,
            gstreamer_webrtc::WebRTCPeerConnectionState::Failed
                | gstreamer_webrtc::WebRTCPeerConnectionState::Closed
                | gstreamer_webrtc::WebRTCPeerConnectionState::Disconnected
        ) {
            if let Some(ref d) = devices {
                if let Err(e) = d.release_all() {
                    tracing::warn!(
                        token = "disconnect-release-all-failed",
                        "release_all on disconnect failed: {e:#}"
                    );
                }
            }
        }
        if let Some(ref tx) = trace_tx {
            let ts = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis() as i64;
            let payload = serde_json::json!({
                "kind": "connection",
                "to": format!("{state:?}").to_lowercase(),
            });
            tx.try_emit(
                session_id.clone(),
                TraceEvent {
                    ts_unix_ms: ts,
                    event: "webrtc.state_changed",
                    payload,
                },
            );
        }
    });
}

/// Wire the "input" DataChannel: log open, and translate each inbound message into device
/// input via `sink` (`None` under `use_test_src`).
pub(super) fn connect_data_channel_open(
    data_channel: &glib::Object,
    sink: InputSink,
    input_state: std::sync::Arc<crate::session::input::InputState>,
    width: i32,
    height: i32,
    transport_mode: &'static str,
) {
    let label: String = data_channel.property("label");

    let lbl = label.clone();
    data_channel.connect("on-open", false, move |_| {
        tracing::info!("DataChannel '{lbl}' open ({transport_mode})");
        None
    });

    // A clock-sync ping or an input event. Ping `{"t":"ping","id":<n>,"tc":<epoch_ms>}`
    // gets `{"t":"pong","id":<n>,"ts":<host unix epoch ms>}` back on the same channel;
    // `ts` is in the host's `ts_unix_ms` domain, so the client's
    // `offset = hostTs − epochMidpoint` satisfies trace-format.md §4. Everything else
    // routes to `input::handle`.
    //
    // GstWebRTCDataChannel is an untyped `glib::Object` whose `WeakRef` is not `Send`, so
    // it CANNOT be captured here — take it off `values[0]`, the signal emitter
    // (.claude/rules/gstreamer-gotchas.md).
    data_channel.connect("on-message-string", false, move |values| {
        let Ok(msg) = values[1].get::<String>() else {
            return None;
        };

        // Best-effort: any parse failure or non-ping type falls through to input handling.
        if let Ok(v) = serde_json::from_str::<serde_json::Value>(&msg) {
            if v.get("t").and_then(|t| t.as_str()) == Some("ping") {
                if let Some(id) = v.get("id").and_then(|i| i.as_i64()) {
                    let host_ts = std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .unwrap_or_default()
                        .as_millis() as i64;
                    let pong = serde_json::json!({ "t": "pong", "id": id, "ts": host_ts });
                    let pong_str = pong.to_string();
                    if let Ok(ch) = values[0].get::<glib::Object>() {
                        ch.emit_by_name::<()>("send-string", &[&pong_str]);
                    }
                    return None; // never pass a ping to input::handle
                }
            }
        }

        crate::session::input::handle(&msg, &sink, &input_state, width, height);
        None
    });
}

/// Extract a `WebRTCSessionDescription` ("offer" or "answer") from a create-offer /
/// create-answer promise reply.
fn session_description_from_reply(
    reply: Result<Option<&gst::StructureRef>, gst::PromiseError>,
    field: &str,
) -> Result<gstreamer_webrtc::WebRTCSessionDescription> {
    let structure = reply
        .map_err(|e| anyhow!("promise error: {e:?}"))?
        .ok_or_else(|| anyhow!("promise returned no reply"))?;
    let desc = structure
        .value(field)
        .map_err(|e| anyhow!("reply has no '{field}': {e}"))?
        .get::<gstreamer_webrtc::WebRTCSessionDescription>()
        .map_err(|e| anyhow!("'{field}' is not a session description: {e}"))?;
    Ok(desc)
}

/// Why a `set-remote-description` did not apply, and whether that ends the session (#503).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum SrdFailureKind {
    /// webrtcbin refused a description it could have accepted; the PC will never carry
    /// media. Fatal.
    Rejected,
    /// A second answer for an already-settled negotiation (the PC is back in `stable`).
    /// Benign, and normal on the ICE-restart path — see [`answer_is_duplicate`].
    DuplicateAnswer,
}

impl SrdFailureKind {
    /// Does this end the session?
    pub(crate) fn is_benign(self) -> bool {
        matches!(self, SrdFailureKind::DuplicateAnswer)
    }

    /// Stable string for the trace payload's `kind` field.
    pub(crate) fn as_str(self) -> &'static str {
        match self {
            SrdFailureKind::Rejected => "rejected",
            SrdFailureKind::DuplicateAnswer => "duplicate_answer",
        }
    }
}

/// A `set-remote-description` that did not apply, on one PeerConnection (#503). `reason` is
/// webrtcbin's own message; for [`SrdFailureKind::Rejected`] the runner turns it into the
/// session's `error_message` via [`remote_description_failure_reason`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct RemoteDescriptionFailure {
    pub pc: PcId,
    pub reason: String,
    pub kind: SrdFailureKind,
}

/// A remote ANSWER webrtcbin accepted, carried out of the promise's change-func so the
/// runner can read what the peer agreed to. An answer that APPLIES can still reject an
/// m-line inside it (port 0 / `inactive`), which fires nothing on the
/// [`RemoteDescriptionFailure`] path. See `session::sdp_answer`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct RemoteDescriptionApplied {
    pub pc: PcId,
    /// The answer text, verbatim. Parsed by the runner on its own tick — never on the
    /// GStreamer thread this is sent from.
    pub sdp: String,
}

/// Where an applied answer is reported; unbounded for the same reason as [`SrdFailTx`].
pub(crate) type SrdOkTx = tokio::sync::mpsc::UnboundedSender<RemoteDescriptionApplied>;

/// Where a rejected `set-remote-description` is reported. Unbounded tokio: `send` never
/// blocks and needs no runtime context, so the promise's change-func can call it from a
/// GStreamer thread, while the runner drains it with `try_recv` on its 100 ms tick and
/// `server::signaling_loop` awaits it as a `select!` arm.
pub(crate) type SrdFailTx = tokio::sync::mpsc::UnboundedSender<RemoteDescriptionFailure>;

/// Bound on a webrtcbin message carried into a log line, a trace payload, and the
/// session's `error_message`. The `serialize()` fallback below runs over an arbitrary
/// GValue: one malformed reply must not write an unbounded string to the control plane.
const MAX_WEBRTCBIN_MESSAGE_BYTES: usize = 512;

/// Truncate on a char boundary, marking that it happened.
fn bound_message(mut text: String) -> String {
    if text.len() <= MAX_WEBRTCBIN_MESSAGE_BYTES {
        return text;
    }
    let mut end = MAX_WEBRTCBIN_MESSAGE_BYTES;
    while end > 0 && !text.is_char_boundary(end) {
        end -= 1;
    }
    text.truncate(end);
    text.push_str("… (truncated)");
    text
}

/// Classify a `set-remote-description` promise reply. `_set_description_task` replies with
/// a structure carrying an `error` GError when it refuses and with NO structure on success,
/// so `Ok(None)` is the happy path and `error` is the only failure signal short of logs.
///
/// `PromiseError::Interrupted` is NOT a rejection but GStreamer cancellation, posted when
/// a pending promise is abandoned (teardown, source swap, renegotiation); failing on it
/// turns every shutdown racing an in-flight call into a spurious `failed`. `Expired` stays
/// fatal — never applied, and nobody cancelled it.
fn remote_description_error(
    reply: Result<Option<&gst::StructureRef>, gst::PromiseError>,
) -> Option<String> {
    let structure = match reply {
        Ok(Some(s)) => s,
        // No reply structure ⇒ webrtcbin accepted the description.
        Ok(None) => return None,
        Err(gst::PromiseError::Interrupted) => {
            tracing::debug!(
                "set-remote-description promise interrupted (cancelled by teardown or \
                 renegotiation) — not a rejection"
            );
            return None;
        }
        Err(e) => return Some(bound_message(format!("promise error: {e:?}"))),
    };
    if !structure.has_field("error") {
        return None;
    }
    // Typed first (webrtcbin uses G_TYPE_ERROR), then a serialized fallback so a
    // differently-typed `error` field is never silently treated as success.
    if let Ok(err) = structure.get::<glib::Error>("error") {
        return Some(bound_message(err.to_string()));
    }
    if let Ok(text) = structure.get::<String>("error") {
        return Some(bound_message(text));
    }
    let text = structure
        .value("error")
        .ok()
        .and_then(|v| v.serialize().ok())
        .map(|s| s.to_string())
        .unwrap_or_else(|| "unknown error".to_string());
    Some(bound_message(text))
}

/// webrtcbin's `signaling-state` as its enum nick (`stable`, `have-local-offer`, …).
/// Untyped: the typed `WebRTCSignalingState` is feature-gated off in gstreamer-webrtc 0.23.
fn signaling_state_nick(webrtc: &gst::Element) -> Option<String> {
    webrtc
        .property_value("signaling-state")
        .serialize()
        .ok()
        .map(|s| s.to_string())
}

/// Is this answer a duplicate for an already-settled negotiation?
///
/// The ICE-restart double-answer: the control plane BUFFERS the agent's offer for a
/// session whose browser has not attached and drains it on register
/// (`signal/handler.go`), and the SPA fires `restart_ice` on websocket open, so a
/// reconnecting client gets two offers per PC and answers both. webrtcbin does not match
/// an answer to its offer generation — it applies whichever lands first, goes `stable`,
/// and refuses the second. Nothing is broken.
///
/// An answer only applies from `have-local-offer`; any other state means no outstanding
/// offer, so it is duplicate or stale, never a peer rejecting our media. `closed` is NOT
/// folded in — a closed PC takes the normal error path. Checked before the call so the
/// duplicate never reaches webrtcbin; [`error_is_stale_answer`] covers the read-to-call
/// window.
fn answer_is_duplicate(state_nick: Option<&str>) -> bool {
    matches!(state_nick, Some("stable"))
}

/// Does this webrtcbin message mean "you already answered this negotiation"?
///
/// The state read and the `set-remote-description` emit are not atomic, so a duplicate can
/// still reach webrtcbin. Matched defensively over the lowercased message: the
/// SCTP-association classifier matched one exact string and was dead code for months.
fn error_is_stale_answer(message: &str) -> bool {
    let m = message.to_ascii_lowercase();
    m.contains("not in the correct state") && m.contains("stable")
}

/// The session-level `error_message` for a rejected remote description (#503).
///
/// Must be diagnosable alone — it reaches an operator as a `failed` session row, not the
/// agent log — so it names the PC, quotes webrtcbin, and for video states the likely
/// cause. Before #503 the failure was invisible and the session sat `running` until the
/// 300 s app-never-presented timeout.
pub(crate) fn remote_description_failure_reason(pc: PcId, webrtcbin_message: &str) -> String {
    let hint = match pc {
        PcId::Video => {
            " — the peer most likely rejected the video m-line \
                        (client cannot decode the negotiated codec)"
        }
        PcId::Audio => "",
    };
    format!("webrtc: {pc} PeerConnection rejected the remote answer: {webrtcbin_message}{hint}")
}

/// Apply a remote SDP. For an inbound offer (answerer), `then_answer` runs once the remote
/// description is set, creating the answer. Called by
/// [`crate::session::server::handle_inbound`].
///
/// #503: a refusal is reported on `on_failure` rather than swallowed. Passing no promise
/// left the rejection existing only as a webrtcbin WARN.
#[allow(clippy::too_many_arguments)]
pub(crate) fn apply_remote_description(
    webrtc: &gst::Element,
    sdp_text: &str,
    sdp_type: gstreamer_webrtc::WebRTCSDPType,
    then_answer: Option<OutTx>,
    pc: PcId,
    on_failure: Option<SrdFailTx>,
    on_applied: Option<SrdOkTx>,
) -> Result<()> {
    if sdp_type == gstreamer_webrtc::WebRTCSDPType::Answer {
        tracing::info!(
            "remote answer received ({} bytes) — abs-capture-time extmap={} twcc extmap={}",
            sdp_text.len(),
            sdp_text.contains(super::ABS_CAPTURE_TIME_URI),
            sdp_text.contains(super::RTP_TWCC_URI),
        );
    }
    // Duplicate-answer gate: report and return WITHOUT touching webrtcbin. Handing it a
    // description it must refuse leaves the same benign error to classify later, inside a
    // promise on a GStreamer thread. Offers are unaffected.
    if sdp_type == gstreamer_webrtc::WebRTCSDPType::Answer {
        let state = signaling_state_nick(webrtc);
        if answer_is_duplicate(state.as_deref()) {
            let reason = format!(
                "{pc} PeerConnection is already in signaling-state=stable — \
                 duplicate/stale answer for a settled negotiation, ignored"
            );
            tracing::warn!(token = "webrtc-negotiation-warning", "{reason}");
            if let Some(tx) = &on_failure {
                let _ = tx.send(RemoteDescriptionFailure {
                    pc,
                    reason: bound_message(reason),
                    kind: SrdFailureKind::DuplicateAnswer,
                });
            }
            return Ok(());
        }
    }

    let sdp = gstreamer_sdp::SDPMessage::parse_buffer(sdp_text.as_bytes())
        .map_err(|e| anyhow!("failed to parse SDP: {e:?}"))?;
    let desc = gstreamer_webrtc::WebRTCSessionDescription::new(sdp_type, sdp);

    // One promise for both roles: the only place a webrtcbin refusal is observable.
    let webrtc2 = webrtc.clone();
    let applied_sdp = on_applied
        .as_ref()
        .filter(|_| sdp_type == gstreamer_webrtc::WebRTCSDPType::Answer)
        .map(|_| sdp_text.to_string());
    let promise = gst::Promise::with_change_func(move |reply| {
        if let Some(message) = remote_description_error(reply) {
            let kind = if error_is_stale_answer(&message) {
                SrdFailureKind::DuplicateAnswer
            } else {
                SrdFailureKind::Rejected
            };
            if kind.is_benign() {
                tracing::warn!(
                    token = "webrtc-duplicate-answer",
                    "set-remote-description on the {pc} PeerConnection was a duplicate \
                     answer, ignored: {message}"
                );
            } else {
                tracing::error!(
                    token = "webrtc-set-remote-failed",
                    "set-remote-description failed on the {pc} PeerConnection: {message}"
                );
            }
            if let Some(tx) = on_failure {
                let _ = tx.send(RemoteDescriptionFailure {
                    pc,
                    reason: message,
                    kind,
                });
            }
            return;
        }
        // Applied. Hand the text to the runner; parsing is its job, not this GStreamer
        // thread's.
        if let (Some(tx), Some(sdp)) = (on_applied, applied_sdp) {
            let _ = tx.send(RemoteDescriptionApplied { pc, sdp });
        }
        if let Some(out_tx) = then_answer {
            create_answer(&webrtc2, out_tx);
        }
    });
    webrtc.emit_by_name::<()>("set-remote-description", &[&desc, &promise]);
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{
        answer_is_duplicate, enable_video_fec, error_is_stale_answer, make_webrtcbin,
        munge_offer_sdp, remote_description_error, remote_description_failure_reason,
        set_audio_receive_latency, signaling_state_nick, PcId, SrdFailureKind,
    };
    use gstreamer as gst;
    use gstreamer::prelude::*;

    fn init() {
        gst::init().unwrap();
    }

    /// A standalone webrtcbin with transceiver 0 realized. webrtcbin creates it lazily on
    /// the first sink-pad request, mirroring `build_encode_pipeline`'s
    /// `request_pad_simple("sink_%u")`, so `get-transceiver(0)` resolves as in production.
    fn webrtcbin_with_video_transceiver() -> gst::Element {
        let webrtc = make_webrtcbin(None)
            .expect("webrtcbin not found — is gstreamer1.0-plugins-bad installed?");
        webrtc
            .request_pad_simple("sink_%u")
            .expect("webrtcbin refused a sink pad request");
        webrtc
    }

    fn video_transceiver(webrtc: &gst::Element) -> gstreamer_webrtc::WebRTCRTPTransceiver {
        webrtc
            .emit_by_name::<Option<gstreamer_webrtc::WebRTCRTPTransceiver>>(
                "get-transceiver",
                &[&0i32],
            )
            .expect("video transceiver (index 0) not found")
    }

    #[test]
    fn enable_video_fec_sets_ulp_red_and_percentage() {
        init();
        let webrtc = webrtcbin_with_video_transceiver();
        // `fixed` plan: negotiate ulp-red at the static level N.
        enable_video_fec(&webrtc, true, 20);
        let t = video_transceiver(&webrtc);
        // Read untyped: the typed WebRTCFECType is feature-gated off.
        let fec_type = t.property_value("fec-type");
        assert_eq!(
            fec_type.serialize().expect("serialize fec-type").as_str(),
            "ulp-red"
        );
        assert_eq!(t.property::<u32>("fec-percentage"), 20);
    }

    /// The `auto` path negotiates `ulp-red` at 0% up front: the `red`/`ulpfec` SDP lines
    /// are present while no repair data flows, so the controller can ramp later with no
    /// SDP renegotiation.
    #[test]
    fn enable_video_fec_auto_negotiates_ulp_red_at_zero() {
        init();
        let webrtc = webrtcbin_with_video_transceiver();
        // `auto` plan: negotiate = true, initial_pct = 0.
        enable_video_fec(&webrtc, true, 0);
        let t = video_transceiver(&webrtc);
        assert_eq!(
            t.property_value("fec-type")
                .serialize()
                .expect("serialize fec-type")
                .as_str(),
            "ulp-red",
            "auto must negotiate fec-type=ulp-red even at 0%"
        );
        assert_eq!(
            t.property::<u32>("fec-percentage"),
            0,
            "auto starts at 0% (armed later by the controller)"
        );
    }

    #[test]
    fn enable_video_fec_no_negotiate_is_a_noop() {
        init();
        let webrtc = webrtcbin_with_video_transceiver();
        let t = video_transceiver(&webrtc);
        let default_fec_type = t
            .property_value("fec-type")
            .serialize()
            .expect("serialize fec-type")
            .to_string();
        let default_percentage = t.property::<u32>("fec-percentage");

        // `off` plan: negotiate = false ⇒ transceiver untouched.
        enable_video_fec(&webrtc, false, 0);

        assert_eq!(
            t.property_value("fec-type")
                .serialize()
                .expect("serialize fec-type")
                .to_string(),
            default_fec_type,
            "percentage=0 must leave fec-type untouched"
        );
        assert_eq!(
            t.property::<u32>("fec-percentage"),
            default_percentage,
            "percentage=0 must leave fec-percentage untouched"
        );
        assert_ne!(
            default_fec_type, "ulp-red",
            "sanity: the untouched default must not already be ulp-red"
        );
    }

    // ── #425: mic receive jitter buffer (audio-PC-only `latency`) ──

    #[test]
    fn set_audio_receive_latency_applies_only_to_this_webrtcbin() {
        init();
        let audio_webrtc = make_webrtcbin(None).expect("webrtcbin not found");
        let video_webrtc = make_webrtcbin(None).expect("webrtcbin not found");
        let video_default = video_webrtc.property::<u32>("latency");

        set_audio_receive_latency(&audio_webrtc, 60);

        assert_eq!(
            audio_webrtc.property::<u32>("latency"),
            60,
            "the AUDIO webrtcbin's latency property must reflect the configured value"
        );
        assert_eq!(
            video_webrtc.property::<u32>("latency"),
            video_default,
            "a separate (video) webrtcbin must be completely unaffected"
        );
    }

    #[test]
    fn set_audio_receive_latency_accepts_the_issue_target_range() {
        init();
        // The #425 target is ~50-75 ms; both ends must apply cleanly.
        for ms in [50u32, 60, 75] {
            let webrtc = make_webrtcbin(None).expect("webrtcbin not found");
            set_audio_receive_latency(&webrtc, ms);
            assert_eq!(webrtc.property::<u32>("latency"), ms);
        }
    }

    #[test]
    fn munge_offer_sdp_adds_fmtp_params() {
        let sdp = "v=0\r\n\
                   a=fmtp:96 packetization-mode=1;profile-level-id=42e01f\r\n\
                   a=fmtp:97 apt=96\r\n\
                   a=fmtp:111 minptime=10;useinbandfec=1\r\n";
        let out = munge_offer_sdp(sdp);
        assert!(out.contains(
            "a=fmtp:96 packetization-mode=1;profile-level-id=42e01f;level-asymmetry-allowed=1\r\n"
        ));
        assert!(out.contains("a=fmtp:97 apt=96;rtx-time=125\r\n"));
        // Opus fmtp untouched; idempotent on re-munge.
        assert!(out.contains("a=fmtp:111 minptime=10;useinbandfec=1\r\n"));
        assert_eq!(munge_offer_sdp(&out), out);
    }
    // ── #503: set-remote-description failure classification ──────────────────
    //
    // Promise-reply parsing and reason derivation without a pipeline; the reply shapes are
    // webrtcbin's `_set_description_task`'s.

    /// webrtcbin replies with NO structure when it accepts; treating that as a failure
    /// would fail every session.
    #[test]
    fn remote_description_error_is_none_on_empty_reply() {
        init();
        assert_eq!(remote_description_error(Ok(None)), None);
    }

    /// A reply structure with no `error` field is also success: webrtcbin may grow reply
    /// fields, and only `error` means refused.
    #[test]
    fn remote_description_error_is_none_without_an_error_field() {
        init();
        let s = gst::Structure::builder("application/x-gst-promise")
            .field("something", 1i32)
            .build();
        assert_eq!(remote_description_error(Ok(Some(s.as_ref()))), None);
    }

    /// The #503 signature: `_set_description_task` replies with a GError whose
    /// message is `Invalid bundle id 1, no session found`.
    #[test]
    fn remote_description_error_reads_the_gerror_message() {
        init();
        let err = glib::Error::new(
            gst::CoreError::Failed,
            "Invalid bundle id 1, no session found",
        );
        let s = gst::Structure::builder("application/x-gst-promise")
            .field("error", err)
            .build();
        let got = remote_description_error(Ok(Some(s.as_ref())))
            .expect("an `error` field must classify as a failure");
        assert!(
            got.contains("Invalid bundle id 1, no session found"),
            "reason must carry webrtcbin's own message, got {got}"
        );
    }

    /// A differently-typed `error` field must still fail, with a diagnosable string.
    #[test]
    fn remote_description_error_falls_back_for_a_non_gerror_field() {
        init();
        let s = gst::Structure::builder("application/x-gst-promise")
            .field("error", "something went wrong")
            .build();
        let got = remote_description_error(Ok(Some(s.as_ref())))
            .expect("a non-GError `error` field is still a failure");
        assert!(got.contains("something went wrong"), "got {got}");
    }

    /// Expired: never applied, and nobody cancelled it. A genuine failure.
    #[test]
    fn remote_description_error_reports_an_expired_promise() {
        init();
        let got = remote_description_error(Err(gst::PromiseError::Expired))
            .expect("an expired promise is a failure");
        assert!(got.contains("promise error"), "got {got}");
    }

    /// `Interrupted` is cancellation, not rejection: a teardown or renegotiation that
    /// abandons a pending promise must not fail the session.
    #[test]
    fn remote_description_error_ignores_an_interrupted_promise() {
        init();
        assert_eq!(
            remote_description_error(Err(gst::PromiseError::Interrupted)),
            None,
            "an interrupted (cancelled) promise must never fail a session"
        );
    }

    /// The `serialize()` fallback runs over an arbitrary GValue, so what reaches
    /// `error_message` must be bounded.
    #[test]
    fn remote_description_error_bounds_a_huge_message() {
        init();
        let huge = "x".repeat(50_000);
        let s = gst::Structure::builder("application/x-gst-promise")
            .field("error", huge.as_str())
            .build();
        let got = remote_description_error(Ok(Some(s.as_ref()))).expect("still a failure");
        assert!(
            got.len() < 600,
            "message must be bounded, got {} bytes",
            got.len()
        );
        assert!(
            got.ends_with("… (truncated)"),
            "truncation must be marked: {got}"
        );
    }

    /// The reason reaching the control plane must name the PC, quote webrtcbin, and (video
    /// only) name the likely cause: an operator reads it with no agent log to hand.
    #[test]
    fn failure_reason_names_the_pc_and_quotes_webrtcbin() {
        let reason =
            remote_description_failure_reason(PcId::Video, "Invalid bundle id 1, no session found");
        assert!(reason.contains("video"), "{reason}");
        assert!(reason.contains("Invalid bundle id 1"), "{reason}");
        assert!(
            reason.contains("decode"),
            "video must carry the codec hint: {reason}"
        );

        let audio = remote_description_failure_reason(PcId::Audio, "boom");
        assert!(audio.contains("audio"), "{audio}");
        assert!(audio.contains("boom"), "{audio}");
        assert!(
            !audio.contains("video m-line"),
            "the video-only hint must not leak onto the audio PC: {audio}"
        );
    }
    // ── duplicate-answer classification (ICE-restart double answer) ───────────
    //
    // Root cause: see `answer_is_duplicate`.

    /// Only `have-local-offer` has an outstanding offer for an answer to answer.
    #[test]
    fn answer_is_duplicate_only_in_stable() {
        assert!(
            answer_is_duplicate(Some("stable")),
            "stable means the negotiation already settled — the answer is a duplicate"
        );
        assert!(
            !answer_is_duplicate(Some("have-local-offer")),
            "have-local-offer is the ONLY state an answer applies from"
        );
        // `closed` is a real problem and takes the normal error path.
        assert!(!answer_is_duplicate(Some("closed")));
        assert!(!answer_is_duplicate(Some("have-remote-offer")));
        // Unreadable state: never benign, let webrtcbin decide.
        assert!(!answer_is_duplicate(None));
    }

    /// Defence in depth for the gap between the state read and the emit.
    #[test]
    fn error_is_stale_answer_matches_webrtcbins_wording() {
        assert!(error_is_stale_answer(
            "Not in the correct state (stable) for setting remote answer description"
        ));
        // Case-insensitive contains, never one exact string.
        assert!(error_is_stale_answer(
            "not in the correct state (STABLE) for setting remote answer description"
        ));
        // A genuine rejection must stay fatal.
        assert!(!error_is_stale_answer(
            "Invalid bundle id 1, no session found"
        ));
        // "not in the correct state" for a NON-stable state is not a duplicate.
        assert!(!error_is_stale_answer(
            "Not in the correct state (closed) for setting remote answer description"
        ));
    }

    #[test]
    fn failure_kinds_carry_their_disposition() {
        assert!(SrdFailureKind::DuplicateAnswer.is_benign());
        assert!(!SrdFailureKind::Rejected.is_benign());
        assert_eq!(SrdFailureKind::DuplicateAnswer.as_str(), "duplicate_answer");
        assert_eq!(SrdFailureKind::Rejected.as_str(), "rejected");
    }

    /// The nicks `answer_is_duplicate` matches are webrtcbin's own, asserted against a REAL
    /// element so a rename cannot silently turn every duplicate back into a session kill.
    #[test]
    fn signaling_state_nick_reads_stable_from_a_real_webrtcbin() {
        init();
        let webrtc = make_webrtcbin(None).expect("webrtcbin not found");
        assert_eq!(
            signaling_state_nick(&webrtc).as_deref(),
            Some("stable"),
            "a fresh webrtcbin starts in `stable`, and that nick is what the \
             duplicate-answer gate matches on"
        );
    }
}
