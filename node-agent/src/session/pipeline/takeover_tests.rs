//! #535: the production encode pipeline against real `webrtcbin` peers, in process.
//!
//! A peer here is what a browser page is to the host: its own ICE agent, DTLS
//! certificate and SCTP association. Signaling is carried by hand, the way the
//! control-plane relay carries it: host frames reach only the attached peer.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use gstreamer as gst;
use gstreamer::prelude::*;
use tokio::sync::mpsc;

use super::{
    apply_remote_description, build_answerer_pipeline, build_encode_pipeline,
    build_source_pipeline, resolve_effective_encoder, EncodePipeline, OutTx,
};
use crate::session::metrics::SessionMetrics;
use crate::session::runner::DiagnosticEventTx;
use crate::session::signaling::{PcId, SignalMsg};
use crate::session::{server, Codec, EncoderChoice, SessionConfig, StreamParams};

/// The dev/runtime image has libnice, gst-interpipe and openh264; the devtools image
/// `make test-rust` runs in has none of the first two. Same loud-SKIP rule as
/// [`super::interpipe_available`].
fn live_webrtc_available() -> bool {
    if !super::interpipe_available() {
        return false;
    }
    let missing: Vec<&str> = ["nicesrc", "openh264enc", "dtlsdec", "sctpenc"]
        .into_iter()
        .filter(|f| gst::ElementFactory::find(f).is_none())
        .collect();
    if !missing.is_empty() {
        eprintln!(
            "SKIP — {missing:?} not installed in this image; the two-peer takeover tests did \
             not run (they do run in the quasar-agent-dev container)"
        );
    }
    missing.is_empty()
}

fn host_cfg() -> SessionConfig {
    let mut settings = crate::session::settings::RuntimeSettings::baseline_with(&|_| None);
    settings.encoder = EncoderChoice::Openh264;
    let stream = StreamParams {
        width: 320,
        height: 240,
        fps: 30,
        bitrate_kbps: 2000,
        h264_profile: "constrained-baseline".to_string(),
        codec: Codec::H264,
        abr_floor_kbps: 0,
        mic: false,
    };
    let mut cfg = SessionConfig::for_assignment_with(&settings, stream, None);
    cfg.use_test_src = true;
    cfg.use_test_audio = true;
    cfg
}

/// The host half of a session: a test source feeding the production encode pipeline.
struct Host {
    source: gst::Pipeline,
    enc: EncodePipeline,
    sig_tx: OutTx,
    sig_rx: mpsc::UnboundedReceiver<SignalMsg>,
    bus: gst::Bus,
    /// Every error the encode bus posted, message and debug text joined.
    bus_errors: Vec<String>,
    _metrics: Arc<SessionMetrics>,
    _trace_rx: mpsc::Receiver<(String, crate::session::runner::TraceEvent)>,
}

impl Host {
    fn start(tag: &str) -> Host {
        let mut cfg = host_cfg();
        let resolved = resolve_effective_encoder(&mut cfg).expect("resolve encoder");
        let sink = format!("takeover-{tag}-{}", std::process::id());
        let source = build_source_pipeline(&cfg, None, &sink).expect("source pipeline");
        let (sig_tx, sig_rx) = mpsc::unbounded_channel();
        let (trace_tx, trace_rx) = mpsc::channel(256);
        let trace = DiagnosticEventTx::new(
            trace_tx,
            Arc::new(AtomicU64::new(0)),
            Arc::new(AtomicU64::new(0)),
        );
        let metrics = Arc::new(SessionMetrics::new("smooth", cfg.stream.fps as u32));
        let enc = build_encode_pipeline(
            &cfg,
            sig_tx.clone(),
            None,
            Arc::new(crate::session::input::InputState::new()),
            &sink,
            metrics.clone(),
            None,
            trace,
            format!("takeover-{tag}"),
            &resolved.factory,
        )
        .expect("encode pipeline");

        // One clock and base time across source and encode, as `runner::run_blocking`.
        let clock = gst::SystemClock::obtain();
        let base = clock.time();
        for p in [&source, &enc.pipeline] {
            p.set_start_time(None::<gst::ClockTime>);
            p.use_clock(Some(&clock));
            p.set_base_time(base);
        }
        source.set_state(gst::State::Playing).expect("source PLAYING");
        enc.pipeline
            .set_state(gst::State::Playing)
            .expect("encode PLAYING");
        if let Some(audio) = &enc.audio_pipeline {
            audio.set_state(gst::State::Playing).expect("audio PLAYING");
        }
        let bus = enc.pipeline.bus().expect("encode bus");
        Host {
            source,
            enc,
            sig_tx,
            sig_rx,
            bus,
            bus_errors: Vec::new(),
            _metrics: metrics,
            _trace_rx: trace_rx,
        }
    }

    fn inbound(&self, msg: SignalMsg) {
        server::apply_inbound_as_offerer(
            msg,
            &self.enc.webrtc,
            self.enc.audio_webrtc.as_ref(),
            &self.sig_tx,
            None,
            None,
        )
        .expect("apply inbound signaling");
    }

    fn drain_bus(&mut self) {
        while let Some(m) = self.bus.pop() {
            if let gst::MessageView::Error(e) = m.view() {
                self.bus_errors
                    .push(format!("{} {:?}", e.error(), e.debug()));
            }
        }
    }

    fn stop(self) {
        let _ = self.source.set_state(gst::State::Null);
        let _ = self.enc.pipeline.set_state(gst::State::Null);
        if let Some(audio) = &self.enc.audio_pipeline {
            let _ = audio.set_state(gst::State::Null);
        }
    }
}

/// A data channel shared between the webrtcbin thread that announces it and the test
/// thread that sends on it.
struct Channel(glib::Object);
// SAFETY: GObject reference counting and signal emission are thread-safe; only the
// binding is conservative, because GstWebRTCDataChannel has no typed wrapper.
unsafe impl Send for Channel {}
unsafe impl Sync for Channel {}

/// One of a peer's two peer connections.
struct PeerPc {
    id: PcId,
    pipeline: gst::Pipeline,
    webrtc: gst::Element,
    tx: OutTx,
    rx: mpsc::UnboundedReceiver<SignalMsg>,
    /// RTP buffers that came out of webrtcbin, so past SRTP: media this peer can decode.
    rtp: Arc<AtomicU64>,
    channel: Arc<Mutex<Option<Channel>>>,
    pongs: Arc<AtomicU64>,
}

impl PeerPc {
    fn new(id: PcId) -> PeerPc {
        let (tx, rx) = mpsc::unbounded_channel();
        let (pipeline, webrtc) = build_answerer_pipeline(tx.clone()).expect("answerer");
        let rtp = Arc::new(AtomicU64::new(0));
        let counter = rtp.clone();
        webrtc.connect_pad_added(move |_, pad| {
            if pad.direction() != gst::PadDirection::Src {
                return;
            }
            let counter = counter.clone();
            pad.add_probe(
                gst::PadProbeType::BUFFER | gst::PadProbeType::BUFFER_LIST,
                move |_, _| {
                    counter.fetch_add(1, Ordering::Relaxed);
                    gst::PadProbeReturn::Ok
                },
            );
        });
        let channel = Arc::new(Mutex::new(None));
        let pongs = Arc::new(AtomicU64::new(0));
        let (slot, pong_count) = (channel.clone(), pongs.clone());
        webrtc.connect("on-data-channel", false, move |values| {
            let dc = values[1].get::<glib::Object>().ok()?;
            let pong_count = pong_count.clone();
            dc.connect("on-message-string", false, move |values| {
                let text = values[1].get::<String>().ok()?;
                if text.contains("\"pong\"") {
                    pong_count.fetch_add(1, Ordering::Relaxed);
                }
                None
            });
            *slot.lock().unwrap() = Some(Channel(dc));
            None
        });
        pipeline.set_state(gst::State::Playing).expect("peer PLAYING");
        PeerPc {
            id,
            pipeline,
            webrtc,
            tx,
            rx,
            rtp,
            channel,
            pongs,
        }
    }

    fn inbound(&self, msg: SignalMsg) {
        match msg {
            SignalMsg::Offer { sdp, .. } => apply_remote_description(
                &self.webrtc,
                &sdp,
                gstreamer_webrtc::WebRTCSDPType::Offer,
                Some(self.tx.clone()),
                self.id,
                None,
                None,
            )
            .expect("peer set-remote-description"),
            SignalMsg::Ice { candidate, .. } => self.webrtc.emit_by_name::<()>(
                "add-ice-candidate",
                &[
                    &candidate.sdp_m_line_index.unwrap_or(0),
                    &candidate.candidate,
                ],
            ),
            _ => {}
        }
    }

    /// The DTLS transport state of this PC's first transceiver, as its enum nick.
    fn dtls_state(&self) -> String {
        let nick = || -> Option<String> {
            let t = self
                .webrtc
                .emit_by_name::<Option<glib::Object>>("get-transceiver", &[&0i32])?;
            let receiver = t.property::<Option<glib::Object>>("receiver")?;
            let transport = receiver.property::<Option<glib::Object>>("transport")?;
            Some(
                transport
                    .property_value("state")
                    .serialize()
                    .ok()?
                    .to_string(),
            )
        };
        nick().unwrap_or_else(|| "none".into())
    }

    fn ice_state(&self) -> String {
        format!(
            "{:?}",
            self.webrtc
                .property::<gstreamer_webrtc::WebRTCICEConnectionState>("ice-connection-state")
        )
    }

    /// Send a clock-sync ping on the input channel; the host answers `pong` on the same
    /// channel, which is how a peer proves its input path reaches the host.
    fn ping(&self) {
        if let Some(dc) = self.channel.lock().unwrap().as_ref() {
            dc.0.emit_by_name::<()>("send-string", &[&r#"{"t":"ping","id":1,"tc":0}"#]);
        }
    }

    fn stop(&self) {
        let _ = self.pipeline.set_state(gst::State::Null);
    }
}

/// A browser page: a video PC (media + input channel) and an audio PC.
struct Peer {
    video: PeerPc,
    audio: PeerPc,
}

impl Peer {
    fn new() -> Peer {
        Peer {
            video: PeerPc::new(PcId::Video),
            audio: PeerPc::new(PcId::Audio),
        }
    }

    fn pc(&self, id: Option<PcId>) -> &PeerPc {
        match id {
            Some(PcId::Audio) => &self.audio,
            _ => &self.video,
        }
    }

    fn pc_mut(&mut self, id: PcId) -> &mut PeerPc {
        match id {
            PcId::Audio => &mut self.audio,
            PcId::Video => &mut self.video,
        }
    }

    fn stop(&self) {
        self.video.stop();
        self.audio.stop();
    }
}

/// Everything the wire carried, for the evidence dump.
#[derive(Default)]
struct Wire {
    offers: Vec<(PcId, String)>,
    answers: Vec<(PcId, String)>,
}

/// Carry signaling between the host and the attached peer until `done` or the deadline.
/// Returns whether `done` held.
fn pump(
    host: &mut Host,
    attached: &mut Peer,
    wire: &mut Wire,
    within: Duration,
    mut done: impl FnMut(&Host, &Peer) -> bool,
) -> bool {
    let deadline = Instant::now() + within;
    loop {
        while let Ok(msg) = host.sig_rx.try_recv() {
            let pc = match &msg {
                SignalMsg::Offer { sdp, pc } => {
                    wire.offers.push((pc.unwrap_or_default(), sdp.clone()));
                    *pc
                }
                SignalMsg::Ice { pc, .. } => *pc,
                _ => None,
            };
            attached.pc(pc).inbound(msg);
        }
        for id in [PcId::Video, PcId::Audio] {
            while let Ok(msg) = attached.pc_mut(id).rx.try_recv() {
                // The answerer speaks the Phase-0 shapes; tag them as the SPA does.
                let msg = match msg {
                    SignalMsg::Answer { sdp, .. } => {
                        wire.answers.push((id, sdp.clone()));
                        SignalMsg::Answer { sdp, pc: Some(id) }
                    }
                    SignalMsg::Ice { candidate, .. } => SignalMsg::Ice {
                        candidate,
                        pc: Some(id),
                    },
                    other => other,
                };
                host.inbound(msg);
            }
        }
        host.drain_bus();
        if done(host, attached) {
            return true;
        }
        if Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}

/// Agent log lines on stderr for the evidence runs.
fn trace_to_stderr() {
    let _ = tracing_subscriber::fmt()
        .with_test_writer()
        .with_env_filter("quasar_node_agent=info")
        .try_init();
}

fn host_states(host: &Host) -> String {
    let w = &host.enc.webrtc;
    format!(
        "ice={:?} conn={:?} signaling={}",
        w.property::<gstreamer_webrtc::WebRTCICEConnectionState>("ice-connection-state"),
        w.property::<gstreamer_webrtc::WebRTCPeerConnectionState>("connection-state"),
        w.property_value("signaling-state")
            .serialize()
            .map(|s| s.to_string())
            .unwrap_or_default(),
    )
}

fn sdp_attr(sdp: &str, name: &str) -> String {
    let prefix = format!("a={name}:");
    sdp.lines()
        .find_map(|l| l.trim().strip_prefix(&prefix).map(str::to_string))
        .unwrap_or_else(|| "-".into())
}

fn rtp(pc: &PeerPc) -> u64 {
    pc.rtp.load(Ordering::Relaxed)
}

/// Attach a fresh peer the way a page does on its first socket: answer the offers the
/// relay buffered, and wait for media and the input channel.
fn first_attach(host: &mut Host, wire: &mut Wire) -> Peer {
    let mut peer = Peer::new();
    let up = pump(host, &mut peer, wire, Duration::from_secs(20), |_, p| {
        rtp(&p.video) > 30 && rtp(&p.audio) > 10 && p.video.channel.lock().unwrap().is_some()
    });
    assert!(
        up,
        "first peer never received media: video rtp={} audio rtp={} ice={} dtls={}",
        rtp(&peer.video),
        rtp(&peer.audio),
        peer.video.ice_state(),
        peer.video.dtls_state()
    );
    peer
}

/// What a page that opens `/app/session/<id>` sends when its socket opens
/// (`web/src/webrtc/session.ts`, `requestOfferOnOpen`).
fn request_offers(host: &Host) {
    host.inbound(SignalMsg::RestartIce {
        pc: Some(PcId::Video),
    });
    host.inbound(SignalMsg::RestartIce {
        pc: Some(PcId::Audio),
    });
}

/// Evidence run for #535, not a gate: prints what the wire and the two peers show when a
/// second peer attaches to a session a first peer is streaming.
/// `cargo test takeover_evidence -- --ignored --nocapture`
#[test]
#[ignore = "evidence dump for #535; prints, asserts nothing about the outcome"]
fn takeover_evidence() {
    if !live_webrtc_available() {
        return;
    }
    trace_to_stderr();
    let mut host = Host::start("evidence");
    let mut wire = Wire::default();
    let mut a = first_attach(&mut host, &mut wire);
    eprintln!(
        "EVIDENCE A up: video rtp={} audio rtp={} ice={} dtls={}",
        rtp(&a.video),
        rtp(&a.audio),
        a.video.ice_state(),
        a.video.dtls_state()
    );

    // B attaches. The relay now delivers host frames to B only; A keeps its transport.
    let mut b = Peer::new();
    request_offers(&host);
    let a_before = rtp(&a.video);
    let started = Instant::now();
    let b_got_media = pump(&mut host, &mut b, &mut wire, Duration::from_secs(15), |_, p| {
        rtp(&p.video) > 30
    });
    eprintln!(
        "EVIDENCE takeover after {:?}: B decodes={b_got_media} B video rtp={} B audio rtp={} \
         B ice={} B dtls={} | A video rtp {a_before} -> {} A ice={} A dtls={}",
        started.elapsed(),
        rtp(&b.video),
        rtp(&b.audio),
        b.video.ice_state(),
        b.video.dtls_state(),
        rtp(&a.video),
        a.video.ice_state(),
        a.video.dtls_state()
    );
    eprintln!("EVIDENCE host video PC after takeover: {}", host_states(&host));
    for (pc, sdp) in &wire.offers {
        eprintln!(
            "EVIDENCE offer  pc={pc} ufrag={} setup={} candidates-in-sdp={} fingerprint={}",
            sdp_attr(sdp, "ice-ufrag"),
            sdp_attr(sdp, "setup"),
            sdp.matches("a=candidate:").count(),
            sdp_attr(sdp, "fingerprint")
        );
    }
    for (pc, sdp) in &wire.answers {
        eprintln!(
            "EVIDENCE answer pc={pc} ufrag={} setup={} fingerprint={}",
            sdp_attr(sdp, "ice-ufrag"),
            sdp_attr(sdp, "setup"),
            sdp_attr(sdp, "fingerprint")
        );
    }

    // The displaced page leaves.
    a.stop();
    pump(&mut host, &mut b, &mut wire, Duration::from_secs(8), |h, _| {
        !h.bus_errors.is_empty()
    });
    eprintln!(
        "EVIDENCE after A left: host bus errors={:?} B video rtp={}",
        host.bus_errors,
        rtp(&b.video)
    );
    let _ = &mut a;
    b.stop();
    host.stop();
}

/// Evidence run for the plain reload: the first page is gone before the second attaches.
#[test]
#[ignore = "evidence dump for #535; prints, asserts nothing about the outcome"]
fn reload_evidence() {
    if !live_webrtc_available() {
        return;
    }
    trace_to_stderr();
    let mut host = Host::start("reload");
    let mut wire = Wire::default();
    let a = first_attach(&mut host, &mut wire);
    a.stop();
    let mut b = Peer::new();
    let gone = pump(&mut host, &mut b, &mut wire, Duration::from_secs(5), |h, _| {
        !h.bus_errors.is_empty()
    });
    eprintln!(
        "EVIDENCE reload: host saw the first peer leave={gone} errors={:?}",
        host.bus_errors
    );
    request_offers(&host);
    let b_got_media = pump(&mut host, &mut b, &mut wire, Duration::from_secs(15), |_, p| {
        rtp(&p.video) > 30
    });
    b.video.ping();
    pump(&mut host, &mut b, &mut wire, Duration::from_secs(2), |_, p| {
        p.video.pongs.load(Ordering::Relaxed) > 0
    });
    eprintln!(
        "EVIDENCE reload: B decodes={b_got_media} B video rtp={} B ice={} B dtls={} B pongs={} \
         host errors={:?}",
        rtp(&b.video),
        b.video.ice_state(),
        b.video.dtls_state(),
        b.video.pongs.load(Ordering::Relaxed),
        host.bus_errors
    );
    b.stop();
    host.stop();
}

/// A stand-in for the control-plane relay, for real browsers: one signaling WebSocket per
/// page, the last attach wins and the displaced socket is closed 4410, host frames reach
/// only the attached socket and buffer until the first one (`protocol/signaling.md`).
/// Driven by `scripts/harness/run-takeover-peers.sh`; not a gate.
#[test]
#[ignore = "serves the encode pipeline to real browsers; needs QUASAR_TAKEOVER_WS"]
fn browser_host() {
    use futures_util::{SinkExt, StreamExt};
    use tokio_tungstenite::tungstenite::protocol::{frame::coding::CloseCode, CloseFrame};
    use tokio_tungstenite::tungstenite::Message;

    let Ok(addr) = std::env::var("QUASAR_TAKEOVER_WS") else {
        eprintln!("SKIP — QUASAR_TAKEOVER_WS not set");
        return;
    };
    if !live_webrtc_available() {
        return;
    }
    trace_to_stderr();
    let secs: u64 = std::env::var("QUASAR_TAKEOVER_SECS")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(120);
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .unwrap();
    rt.block_on(async move {
        let mut host = Host::start("browser");
        let listener = tokio::net::TcpListener::bind(&addr).await.expect("bind");
        eprintln!("HOST listening on ws://{addr} for {secs}s");
        let (in_tx, mut in_rx) = mpsc::unbounded_channel::<(u64, String)>();
        let mut attached: Option<(u64, mpsc::UnboundedSender<Message>)> = None;
        let mut buffered: Vec<Message> = Vec::new();
        let mut next_id = 0u64;
        let mut seen_errors = 0usize;
        let mut tick = tokio::time::interval(Duration::from_millis(10));
        let deadline = tokio::time::Instant::now() + Duration::from_secs(secs);
        loop {
            tokio::select! {
                _ = tokio::time::sleep_until(deadline) => break,
                accepted = listener.accept() => {
                    let Ok((stream, _)) = accepted else { continue };
                    let Ok(ws) = tokio_tungstenite::accept_async(stream).await else { continue };
                    next_id += 1;
                    let id = next_id;
                    let (mut sink, mut source) = ws.split();
                    let (out_tx, mut out_rx) = mpsc::unbounded_channel::<Message>();
                    tokio::spawn(async move {
                        while let Some(m) = out_rx.recv().await {
                            let closing = matches!(m, Message::Close(_));
                            if sink.send(m).await.is_err() || closing {
                                break;
                            }
                        }
                    });
                    let in_tx = in_tx.clone();
                    tokio::spawn(async move {
                        while let Some(Ok(m)) = source.next().await {
                            if let Message::Text(t) = m {
                                let _ = in_tx.send((id, t.as_str().to_string()));
                            }
                        }
                    });
                    eprintln!("HOST socket {id} attached");
                    for m in buffered.drain(..) {
                        let _ = out_tx.send(m);
                    }
                    if let Some((old, old_tx)) = attached.replace((id, out_tx)) {
                        eprintln!("HOST socket {old} displaced (4410)");
                        let _ = old_tx.send(Message::Close(Some(CloseFrame {
                            code: CloseCode::from(4410),
                            reason: "session attached from another client".into(),
                        })));
                    }
                }
                Some((id, text)) = in_rx.recv() => {
                    if attached.as_ref().is_some_and(|(a, _)| *a == id) {
                        match SignalMsg::from_json(&text) {
                            Ok(msg) => host.inbound(msg),
                            Err(e) => eprintln!("HOST bad frame from {id}: {e}"),
                        }
                    }
                }
                _ = tick.tick() => {
                    while let Ok(msg) = host.sig_rx.try_recv() {
                        let frame = Message::Text(msg.to_json().unwrap().into());
                        match &attached {
                            Some((_, tx)) => { let _ = tx.send(frame); }
                            None => buffered.push(frame),
                        }
                    }
                    host.drain_bus();
                    for e in &host.bus_errors[seen_errors..] {
                        eprintln!("HOST bus error: {e}");
                    }
                    seen_errors = host.bus_errors.len();
                }
            }
        }
        host.stop();
    });
}
