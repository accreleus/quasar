//! #535: the production encode pipeline and the runner's transport state against real
//! `webrtcbin` peers, in process.
//!
//! A peer here is what a browser page is to the host: its own ICE agent and DTLS and
//! SCTP associations. Signaling is carried by hand, the way the control-plane relay
//! carries it: host frames reach only the attached peer. What a browser adds (its own
//! certificate, an SCTP abort when the page goes) is `scripts/harness/run-takeover-peers.sh`.

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
use crate::session::runner::{DiagnosticEventTx, SessionTransports};
use crate::session::signaling::{PcId, SignalMsg};
use crate::session::{Codec, EncoderChoice, SessionConfig, StreamParams};

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
    // No scheduled keyframe inside a test: one that shows up was asked for.
    cfg.gop = 3600;
    cfg
}

/// Each test stands up some forty GStreamer threads and binds UDP ports; one at a time.
static SERIAL: Mutex<()> = Mutex::new(());

fn serial() -> std::sync::MutexGuard<'static, ()> {
    SERIAL.lock().unwrap_or_else(|e| e.into_inner())
}

/// The host half of a session: a test source feeding the production encode pipeline.
struct Host {
    source: gst::Pipeline,
    pipeline: gst::Pipeline,
    /// The runner's own transport state and inbound dispatch.
    transports: SessionTransports,
    /// PCs whose transport was replaced for a new peer, in order.
    replaced: Vec<PcId>,
    sig_tx: OutTx,
    sig_rx: mpsc::UnboundedReceiver<SignalMsg>,
    bus: gst::Bus,
    /// Every error the encode bus posted, message and debug text joined.
    bus_errors: Vec<String>,
    encoder: gst::Element,
    /// When the encoder emitted each keyframe.
    keyframes: Arc<Mutex<Vec<Instant>>>,
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
        source
            .set_state(gst::State::Playing)
            .expect("source PLAYING");
        enc.pipeline
            .set_state(gst::State::Playing)
            .expect("encode PLAYING");
        let EncodePipeline {
            pipeline,
            video,
            transport,
            audio_webrtc,
            audio_pipeline,
            ..
        } = enc;
        let transports = SessionTransports::new(transport, video, audio_webrtc, audio_pipeline);
        transports.audio_pipeline.play();
        let bus = pipeline.bus().expect("encode bus");
        let encoder = pipeline
            .by_name(super::VIDEO_ENCODER_NAME)
            .expect("video encoder");
        let keyframes = Arc::new(Mutex::new(Vec::new()));
        let seen = keyframes.clone();
        encoder
            .static_pad("src")
            .unwrap()
            .add_probe(gst::PadProbeType::BUFFER, move |_, info| {
                if let Some(buffer) = info.buffer() {
                    if !buffer.flags().contains(gst::BufferFlags::DELTA_UNIT) {
                        seen.lock().unwrap().push(Instant::now());
                    }
                }
                gst::PadProbeReturn::Ok
            });
        Host {
            source,
            pipeline,
            transports,
            replaced: Vec::new(),
            sig_tx,
            sig_rx,
            bus,
            bus_errors: Vec::new(),
            encoder,
            keyframes,
            _metrics: metrics,
            _trace_rx: trace_rx,
        }
    }

    fn inbound(&mut self, msg: SignalMsg) {
        let replaced = self
            .transports
            .inbound(msg, &self.sig_tx, None, None)
            .expect("replace the video transport");
        self.replaced.extend(replaced);
    }

    fn drain_bus(&mut self) {
        while let Some(m) = self.bus.pop() {
            if let gst::MessageView::Error(e) = m.view() {
                self.bus_errors
                    .push(format!("{} {:?}", e.error(), e.debug()));
            }
        }
    }
}

// A failed assertion must still take the pipelines to NULL: an interpipe pair dropped
// while PLAYING leaves a dangling listener that segfaults the next test.
impl Drop for Host {
    fn drop(&mut self) {
        let _ = self.source.set_state(gst::State::Null);
        let _ = self.pipeline.set_state(gst::State::Null);
        self.transports.audio_pipeline.finish();
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
        pipeline
            .set_state(gst::State::Playing)
            .expect("peer PLAYING");
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

impl Drop for PeerPc {
    fn drop(&mut self) {
        self.stop();
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

/// Carry signaling between the host and the attached peer until `done` or the deadline.
/// Returns whether `done` held.
fn pump(
    host: &mut Host,
    attached: &mut Peer,
    within: Duration,
    done: impl FnMut(&Host, &Peer) -> bool,
) -> bool {
    pump_with(host, attached, within, true, done)
}

/// [`pump`], optionally losing every ICE candidate: a peer that answers and never connects.
fn pump_with(
    host: &mut Host,
    attached: &mut Peer,
    within: Duration,
    carry_ice: bool,
    mut done: impl FnMut(&Host, &Peer) -> bool,
) -> bool {
    let deadline = Instant::now() + within;
    loop {
        while let Ok(msg) = host.sig_rx.try_recv() {
            let pc = match &msg {
                SignalMsg::Offer { pc, .. } => *pc,
                SignalMsg::Ice { pc, .. } if carry_ice => *pc,
                _ => continue,
            };
            attached.pc(pc).inbound(msg);
        }
        for id in [PcId::Video, PcId::Audio] {
            while let Ok(msg) = attached.pc_mut(id).rx.try_recv() {
                // The answerer speaks the Phase-0 shapes; tag them as the SPA does.
                let msg = match msg {
                    SignalMsg::Answer { sdp, .. } => SignalMsg::Answer { sdp, pc: Some(id) },
                    SignalMsg::Ice { candidate, .. } if carry_ice => SignalMsg::Ice {
                        candidate,
                        pc: Some(id),
                    },
                    _ => continue,
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

/// Agent log lines on stderr, for the browser host.
fn trace_to_stderr() {
    let _ = tracing_subscriber::fmt()
        .with_test_writer()
        .with_env_filter("quasar_node_agent=info")
        .try_init();
}

fn rtp(pc: &PeerPc) -> u64 {
    pc.rtp.load(Ordering::Relaxed)
}

/// Attach a fresh peer the way a page does on its first socket: answer the offers the
/// relay buffered, and wait for media and the input channel.
fn first_attach(host: &mut Host) -> Peer {
    let mut peer = Peer::new();
    let up = pump(host, &mut peer, Duration::from_secs(20), |_, p| {
        streaming(p)
    });
    assert!(
        up,
        "the first peer never received media: {}",
        describe(&peer)
    );
    peer
}

/// What a page that opens `/app/session/<id>` sends when its socket opens
/// (`web/src/webrtc/session.ts`, `requestOfferOnOpen`), then the wait for its media.
fn attach(host: &mut Host) -> Peer {
    let mut peer = Peer::new();
    host.inbound(SignalMsg::RestartIce {
        pc: Some(PcId::Video),
    });
    host.inbound(SignalMsg::RestartIce {
        pc: Some(PcId::Audio),
    });
    let up = pump(host, &mut peer, Duration::from_secs(20), |_, p| {
        streaming(p)
    });
    assert!(
        up,
        "the peer that attached never received media: {} (host replaced {:?})",
        describe(&peer),
        host.replaced
    );
    peer
}

fn streaming(peer: &Peer) -> bool {
    rtp(&peer.video) > 30 && rtp(&peer.audio) > 10 && peer.video.channel.lock().unwrap().is_some()
}

fn describe(peer: &Peer) -> String {
    format!(
        "video rtp={} audio rtp={} ice={} dtls={}",
        rtp(&peer.video),
        rtp(&peer.audio),
        peer.video.ice_state(),
        peer.video.dtls_state()
    )
}

/// Does the host answer this peer's input channel?
fn input_reaches_host(host: &mut Host, peer: &mut Peer) -> bool {
    let before = peer.video.pongs.load(Ordering::Relaxed);
    peer.video.ping();
    pump(host, peer, Duration::from_secs(2), |_, p| {
        p.video.pongs.load(Ordering::Relaxed) > before
    })
}

fn threads() -> usize {
    std::fs::read_dir("/proc/self/task").map_or(0, |d| d.count())
}

/// The issue: a second page opens a session a first page is streaming.
#[test]
fn a_takeover_mid_stream_moves_the_stream_to_the_new_peer() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("midstream");
    let a = first_attach(&mut host);
    let first_transport = host.transports.video.webrtc.downgrade();
    let keyframes_before = host.keyframes.lock().unwrap().len();

    let mut b = attach(&mut host);

    let mut replaced = host.replaced.clone();
    replaced.sort_by_key(|pc| *pc as usize);
    assert_eq!(
        replaced,
        [PcId::Video, PcId::Audio],
        "each PC gets one new transport"
    );
    assert!(
        first_transport.upgrade().is_none(),
        "the displaced peer's transport is torn down by the host"
    );
    assert!(
        input_reaches_host(&mut host, &mut b),
        "the new peer's input channel must reach the host"
    );
    assert!(
        host.keyframes.lock().unwrap().len() > keyframes_before,
        "the new peer needs a keyframe to start decoding"
    );
    assert_eq!(
        host.pipeline.by_name(super::VIDEO_ENCODER_NAME).as_ref(),
        Some(&host.encoder),
        "the encoder is not rebuilt"
    );

    // The displaced page: nothing reaches it any more, and its input is not heard.
    let a_video = rtp(&a.video);
    let a_pongs = a.video.pongs.load(Ordering::Relaxed);
    a.video.ping();
    pump(&mut host, &mut b, Duration::from_secs(1), |_, _| false);
    assert_eq!(
        rtp(&a.video),
        a_video,
        "the displaced peer still receives video"
    );
    assert_eq!(a.video.pongs.load(Ordering::Relaxed), a_pongs);

    // It leaves; the session carries on.
    a.stop();
    let b_video = rtp(&b.video);
    pump(&mut host, &mut b, Duration::from_secs(2), |_, _| false);
    assert!(
        rtp(&b.video) > b_video + 20,
        "the stream stopped when the displaced peer left"
    );
    assert_eq!(host.bus_errors, Vec::<String>::new());
}

/// The first page answered but its transport never connected (its loader is still up).
/// The encode chain is then parked inside that transport, which is the hard teardown.
#[test]
fn a_takeover_while_the_first_peer_is_still_connecting_reaches_the_new_peer() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("connecting");
    let mut a = Peer::new();
    let answered = pump_with(&mut host, &mut a, Duration::from_secs(10), false, |h, _| {
        h.transports
            .video
            .webrtc
            .property::<Option<gstreamer_webrtc::WebRTCSessionDescription>>("remote-description")
            .is_some()
    });
    assert!(answered, "the first peer's answer never applied");
    assert_eq!(rtp(&a.video), 0, "the first peer must not have connected");

    let mut b = attach(&mut host);

    assert!(host.replaced.contains(&PcId::Video));
    assert!(input_reaches_host(&mut host, &mut b));
    assert_eq!(host.bus_errors, Vec::<String>::new());
}

/// The first page is gone before the second attaches (a reload, a resume). The agent
/// cannot tell this from a takeover: the first transport is still up as far as it knows.
#[test]
fn a_peer_that_attaches_after_the_first_is_gone_gets_the_stream() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("reload");
    let a = first_attach(&mut host);
    a.stop();

    let mut b = attach(&mut host);

    assert!(input_reaches_host(&mut host, &mut b));
    assert_eq!(host.bus_errors, Vec::<String>::new());
}

/// `restart_ice` from the peer the transport belongs to: media recovery, and an in-place
/// signaling re-attach that had a retry in flight. Replacing the transport here would
/// strand that peer's DTLS association. (An in-place re-attach with nothing in flight
/// sends the agent nothing at all.)
#[test]
fn a_restart_from_the_same_peer_keeps_the_transport() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("samepeer");
    let mut a = first_attach(&mut host);
    let transport = host.transports.video.webrtc.clone();

    host.inbound(SignalMsg::RestartIce {
        pc: Some(PcId::Video),
    });
    host.inbound(SignalMsg::RestartIce {
        pc: Some(PcId::Audio),
    });
    let before = rtp(&a.video);
    pump(&mut host, &mut a, Duration::from_secs(2), |_, _| false);

    assert_eq!(host.replaced, [], "the same peer must keep its transport");
    assert_eq!(host.transports.video.webrtc, transport);
    assert!(
        rtp(&a.video) > before + 20,
        "media stopped across the re-offer"
    );
    assert!(input_reaches_host(&mut host, &mut a));
}

/// Twenty takeovers in one session leave one transport's worth of everything.
#[test]
fn twenty_takeovers_leak_nothing() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("leak");
    let mut current = first_attach(&mut host);
    let mut retired = Vec::new();
    let (mut elements, mut live_threads) = (0, 0);
    for round in 0..20 {
        retired.push(host.transports.video.webrtc.downgrade());
        retired.extend(host.transports.audio_webrtc.as_ref().map(|w| w.downgrade()));
        let next = attach(&mut host);
        current.stop();
        current = next;
        if round == 0 {
            // Past the first takeover, so both counts include a peer being stopped.
            elements = host.pipeline.children().len();
            live_threads = threads();
        }
    }
    assert!(input_reaches_host(&mut host, &mut current));
    assert_eq!(
        host.replaced.len(),
        40,
        "every takeover replaces both transports once"
    );
    let leaked = retired.iter().filter(|w| w.upgrade().is_some()).count();
    assert_eq!(leaked, 0, "retired webrtcbins still alive");
    assert_eq!(
        host.pipeline.children().len(),
        elements,
        "the encode pipeline grew"
    );
    // A leaked transport keeps its webrtcbin, ICE and RTCP threads: several per takeover.
    assert!(
        threads() < live_threads + 19,
        "threads grew from {live_threads} to {} over 19 takeovers",
        threads()
    );
    assert_eq!(host.bus_errors, Vec::<String>::new());
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
        let (mut seen_errors, mut seen_replaced) = (0, 0);
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
                    for pc in &host.replaced[seen_replaced..] {
                        eprintln!("HOST replaced the {pc} transport");
                    }
                    seen_replaced = host.replaced.len();
                }
            }
        }
    });
}
