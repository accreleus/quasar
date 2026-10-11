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
use crate::session::runner::{DiagnosticEventTx, SessionEvent, SessionTransports};
use crate::session::signaling::{PcId, SignalMsg};
use crate::session::{Codec, EncoderChoice, SessionConfig, StreamParams};

/// The dev/runtime image has libnice, gst-interpipe and openh264; the devtools image
/// `make test-rust` runs in and the CI runner have none of the first two. A test that
/// returns early counts as passed, so the skip is written past libtest's capture, once,
/// and `scripts/verify/agent.sh` restates it.
fn live_webrtc_available() -> bool {
    gst::init().unwrap();
    let missing: Vec<&str> = [
        "interpipesink",
        "interpipesrc",
        "nicesrc",
        "openh264enc",
        "dtlsdec",
        "sctpenc",
    ]
    .into_iter()
    .filter(|f| gst::ElementFactory::find(f).is_none())
    .collect();
    if !missing.is_empty() {
        static SAID: std::sync::Once = std::sync::Once::new();
        SAID.call_once(|| {
            use std::io::Write;
            let _ = writeln!(
                std::io::stderr(),
                "SKIP — session::pipeline::takeover_tests (#535): the live two-peer WebRTC \
                 tests did NOT run, {missing:?} missing from this image; they run in the \
                 quasar-agent-dev container"
            );
        });
    }
    missing.is_empty()
}

fn host_cfg(mic: bool) -> SessionConfig {
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
        mic,
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
    /// Every offer the host sent and every answer it was handed, in order.
    wire: Vec<(&'static str, PcId, String)>,
    /// Rewrites an answer before the host sees it (evidence runs).
    munge_answer: Option<fn(String) -> String>,
    sig_tx: OutTx,
    sig_rx: mpsc::UnboundedReceiver<SignalMsg>,
    bus: gst::Bus,
    /// Every error the encode bus posted, message and debug text joined.
    bus_errors: Vec<String>,
    encoder: gst::Element,
    /// When the encoder emitted each keyframe.
    keyframes: Arc<Mutex<Vec<Instant>>>,
    /// RECONFIGURE events that reached the encoder from downstream.
    reconfigures: Arc<AtomicU64>,
    _metrics: Arc<SessionMetrics>,
    _trace_rx: mpsc::Receiver<(String, crate::session::runner::TraceEvent)>,
}

impl Host {
    fn start(tag: &str) -> Host {
        Host::start_with(tag, false)
    }

    /// `mic`: the session was launched with the microphone granted, so the audio PC
    /// offers a second, recvonly m-line (decoded into a fakesink here: no sidecar).
    fn start_with(tag: &str, mic: bool) -> Host {
        let mut cfg = host_cfg(mic);
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
        let reconfigures = Arc::new(AtomicU64::new(0));
        let count = reconfigures.clone();
        encoder.static_pad("src").unwrap().add_probe(
            gst::PadProbeType::EVENT_UPSTREAM,
            move |_, info| {
                if info.event().map(|e| e.type_()) == Some(gst::EventType::Reconfigure) {
                    count.fetch_add(1, Ordering::Relaxed);
                }
                gst::PadProbeReturn::Ok
            },
        );
        Host {
            source,
            pipeline,
            transports,
            replaced: Vec::new(),
            wire: Vec::new(),
            munge_answer: None,
            sig_tx,
            sig_rx,
            bus,
            bus_errors: Vec::new(),
            encoder,
            keyframes,
            reconfigures,
            _metrics: metrics,
            _trace_rx: trace_rx,
        }
    }

    fn inbound(&mut self, msg: SignalMsg) {
        let msg = match (msg, self.munge_answer) {
            (SignalMsg::Answer { sdp, pc }, Some(munge)) => SignalMsg::Answer {
                sdp: munge(sdp),
                pc,
            },
            (msg, _) => msg,
        };
        if let SignalMsg::Answer { sdp, pc } = &msg {
            self.wire
                .push(("answer", pc.unwrap_or_default(), sdp.clone()));
        }
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
        // The runner's order (`runner::stop_source`).
        self.transports.video.release();
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
                SignalMsg::Offer { pc, sdp } => {
                    host.wire
                        .push(("offer", pc.unwrap_or_default(), sdp.clone()));
                    *pc
                }
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

/// The first value of an SDP attribute or line prefix.
fn sdp_value<'a>(sdp: &'a str, prefix: &str) -> &'a str {
    sdp.lines()
        .find_map(|l| l.trim().strip_prefix(prefix))
        .unwrap_or("-")
}

fn sdp_values<'a>(sdp: &'a str, prefix: &str) -> Vec<&'a str> {
    sdp.lines()
        .filter_map(|l| l.trim().strip_prefix(prefix))
        .collect()
}

/// The MediaStream ids an SDP names, from its `a=ssrc:<id> msid:<stream> <track>` lines.
fn sdp_msids(sdp: &str) -> std::collections::BTreeSet<&str> {
    sdp_values(sdp, "a=ssrc:")
        .into_iter()
        .filter_map(|v| v.split_once(" msid:"))
        .filter_map(|(_, msid)| msid.split(' ').next())
        .collect()
}

/// What identifies an SDP's session and transport, one line.
fn sdp_facts(sdp: &str) -> String {
    let origin: Vec<&str> = sdp_value(sdp, "o=").split(' ').collect();
    let ssrcs: std::collections::BTreeSet<&str> = sdp_values(sdp, "a=ssrc:")
        .into_iter()
        .filter_map(|v| v.split(' ').next())
        .collect();
    format!(
        "o=(id {} v {}) ufrag={} fp={} setup={} mids={:?} bundle={} sctp-port={} ssrc={:?} msid={:?} m={:?}",
        origin.get(1).unwrap_or(&"-"),
        origin.get(2).unwrap_or(&"-"),
        sdp_value(sdp, "a=ice-ufrag:").chars().take(8).collect::<String>(),
        sdp_value(sdp, "a=fingerprint:").chars().take(24).collect::<String>(),
        sdp_value(sdp, "a=setup:"),
        sdp_values(sdp, "a=mid:"),
        sdp_value(sdp, "a=group:BUNDLE "),
        sdp_value(sdp, "a=sctp-port:"),
        ssrcs,
        sdp_msids(sdp),
        sdp_values(sdp, "m=")
            .iter()
            .map(|m| m.split(' ').take(2).collect::<Vec<_>>().join(" "))
            .collect::<Vec<_>>(),
    )
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
    request_offers(host);
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
    let reconfigures_before = host.reconfigures.load(Ordering::Relaxed);

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
    assert_eq!(
        host.reconfigures.load(Ordering::Relaxed),
        reconfigures_before,
        "the replacement made the encoder renegotiate"
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

/// The first page answered but its transport never connected (its loader is still up),
/// and a second page attaches.
fn takeover_while_connecting(tag: &str, another_browser: bool) -> (Host, Peer) {
    let mut host = Host::start(tag);
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
    if another_browser {
        host.munge_answer = Some(with_another_certificate);
    }
    let mut b = attach(&mut host);
    assert!(input_reaches_host(&mut host, &mut b));
    assert_eq!(host.bus_errors, Vec::<String>::new());
    (host, b)
}

/// The encode chain is parked inside the transport that never connected, which is the
/// hard teardown.
#[test]
fn a_takeover_while_the_first_peer_is_still_connecting_reaches_the_new_peer() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (host, _b) = takeover_while_connecting("connecting", true);
    let mut replaced = host.replaced.clone();
    replaced.sort_by_key(|pc| *pc as usize);
    assert_eq!(replaced, [PcId::Video, PcId::Audio]);
}

/// The same from a peer with the first one's certificate, as two peer connections of one
/// process have: no DTLS has started on the transports, so it takes them as they are.
#[test]
fn a_peer_with_the_same_certificate_takes_a_transport_that_never_connected_in_place() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (host, _b) = takeover_while_connecting("inplace", false);
    assert_eq!(host.replaced, Vec::<PcId>::new());
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

    request_offers(&mut host);
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

/// The `n`th SDP of one kind the wire carried for the video PC.
fn video_sdp<'a>(host: &'a Host, what: &str, n: usize) -> &'a str {
    host.wire
        .iter()
        .filter(|(w, pc, _)| *w == what && *pc == PcId::Video)
        .nth(n)
        .map(|(_, _, sdp)| sdp.as_str())
        .unwrap_or_else(|| panic!("no video {what} #{n} on the wire"))
}

fn request_offers(host: &mut Host) {
    host.inbound(SignalMsg::RestartIce {
        pc: Some(PcId::Video),
    });
    host.inbound(SignalMsg::RestartIce {
        pc: Some(PcId::Audio),
    });
}

/// What the taking page is sent, as `protocol/signaling.md` states it (amendment 27):
/// first the old transport's offer again, then the new transport's, which to the client
/// is an ICE restart on the same DTLS identity.
#[test]
fn the_new_transports_offer_differs_from_the_old_one_only_as_an_ice_restart_does() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start_with("offers", true);
    let _a = first_attach(&mut host);
    let _b = attach(&mut host);

    // The microphone m-line is in every audio offer, under the same mid: the slot a
    // client claimed on the first one is the slot the last one offers.
    let audio: Vec<&str> = host
        .wire
        .iter()
        .filter(|(what, pc, _)| *what == "offer" && *pc == PcId::Audio)
        .map(|(_, _, sdp)| sdp.as_str())
        .collect();
    assert_eq!(audio.len(), 3, "launch, again, and the new transport's");
    for offer in &audio {
        assert_eq!(
            sdp_msids(offer),
            sdp_msids(audio[0]),
            "a new stream to the client"
        );
        assert_eq!(sdp_values(offer, "a=mid:"), sdp_values(audio[0], "a=mid:"));
        assert_eq!(
            sdp_values(offer, "a=mid:").len(),
            2,
            "audio out and microphone in"
        );
        assert!(offer.lines().any(|l| l.trim() == "a=recvonly"));
    }

    let (launch, again, fresh) = (
        video_sdp(&host, "offer", 0),
        video_sdp(&host, "offer", 1),
        video_sdp(&host, "offer", 2),
    );
    let session = |sdp| sdp_value(sdp, "o=").split(' ').nth(1).map(str::to_string);
    let version = |sdp| sdp_value(sdp, "o=").split(' ').nth(2).map(str::to_string);

    // `restart_ice` on a transport: the same session, one version on, nothing restarted.
    assert_eq!(session(again), session(launch));
    assert_ne!(version(again), version(launch));
    assert_eq!(
        sdp_value(again, "a=ice-ufrag:"),
        sdp_value(launch, "a=ice-ufrag:"),
        "webrtcbin started honouring ice-restart: the same peer's answer will now look \
         like a new peer's (see webrtc::answer_from)"
    );

    // The new transport: another SDP session with new ICE credentials...
    assert_ne!(session(fresh), session(launch));
    assert_ne!(
        sdp_value(fresh, "a=ice-ufrag:"),
        sdp_value(launch, "a=ice-ufrag:")
    );
    // ...and nothing else a client has to act on.
    for same in [
        "a=fingerprint:",
        "a=setup:",
        "a=group:BUNDLE ",
        "a=sctp-port:",
    ] {
        assert_eq!(sdp_value(fresh, same), sdp_value(launch, same), "{same}");
    }
    assert_eq!(sdp_values(fresh, "a=mid:"), sdp_values(launch, "a=mid:"));
    assert_eq!(sdp_values(fresh, "m="), sdp_values(launch, "m="));
    // The same MediaStream: a client that wired the first offer's track keeps it.
    assert!(!sdp_msids(launch).is_empty());
    assert_eq!(sdp_msids(fresh), sdp_msids(launch));
}

/// The relay can deliver one frame the displaced page had already sent. Its answer must
/// not bind the transport built for the new page: the new page's own answer would then
/// read as yet another peer and cost a second transport and a third offer.
#[test]
fn an_answer_the_displaced_peer_had_sent_costs_no_transport() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("stale");
    let _a = first_attach(&mut host);
    let stale = SignalMsg::Answer {
        sdp: video_sdp(&host, "answer", 0).to_string(),
        pc: Some(PcId::Video),
    };

    let mut b = Peer::new();
    request_offers(&mut host);
    // Up to the replacement: the new transport's offer has not reached B yet.
    let replaced = pump(&mut host, &mut b, Duration::from_secs(10), |h, _| {
        h.replaced.contains(&PcId::Video)
    });
    assert!(
        replaced,
        "the new peer's answer did not replace the transport"
    );
    host.inbound(stale.clone());
    let up = pump(&mut host, &mut b, Duration::from_secs(20), |_, p| {
        streaming(p)
    });
    assert!(up, "the new peer never streamed: {}", describe(&b));

    // And late, once the new peer streams: not a newer peer either.
    host.inbound(stale);
    pump(&mut host, &mut b, Duration::from_secs(1), |_, _| false);
    let mut replaced = host.replaced.clone();
    replaced.sort_by_key(|pc| *pc as usize);
    assert_eq!(
        replaced,
        [PcId::Video, PcId::Audio],
        "a stale answer cost a transport"
    );
    assert!(input_reaches_host(&mut host, &mut b));
}

/// A client's recovery can ask for an ICE restart in the gap between its answer to the
/// old transport's offer and the new transport's first offer. Forcing an offer out of a
/// transport that has no caps yet gives one with no stream in it, and the real offer
/// then never comes.
#[test]
fn a_restart_request_before_the_new_transport_has_offered_is_absorbed() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("gap");
    let _a = first_attach(&mut host);

    let mut b = Peer::new();
    request_offers(&mut host);
    let replaced = pump(&mut host, &mut b, Duration::from_secs(10), |h, _| {
        h.replaced.contains(&PcId::Video)
    });
    assert!(replaced);
    host.inbound(SignalMsg::RestartIce {
        pc: Some(PcId::Video),
    });
    let up = pump(&mut host, &mut b, Duration::from_secs(20), |_, p| {
        streaming(p)
    });

    assert!(up, "the new peer never streamed: {}", describe(&b));
    let fresh = video_sdp(&host, "offer", 2);
    assert!(
        !sdp_msids(fresh).is_empty(),
        "the new transport's offer names no stream"
    );
    assert!(input_reaches_host(&mut host, &mut b));
}

/// Past the budget a new peer's answer is ignored: no transport, and the peer the
/// session is streaming to keeps its own.
#[test]
fn past_the_replacement_budget_the_current_peer_keeps_the_transport() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("budget");
    host.transports.set_replacement_limit(1);
    let _a = first_attach(&mut host);
    let mut b = attach(&mut host);
    let transport = host.transports.video.webrtc.clone();

    let mut c = Peer::new();
    request_offers(&mut host);
    let b_video = rtp(&b.video);
    pump(&mut host, &mut c, Duration::from_secs(3), |_, _| false);

    assert_eq!(host.replaced.len(), 2, "replaced past the budget");
    assert_eq!(host.transports.video.webrtc, transport);
    assert_eq!(rtp(&c.video), 0);
    assert!(
        rtp(&b.video) > b_video + 30,
        "the current peer lost its stream"
    );
    assert!(input_reaches_host(&mut host, &mut b));
}

/// Twenty takeovers in one session leave one transport's worth of everything.
#[test]
fn twenty_takeovers_leak_nothing() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("leak");
    // Twenty takeovers in half a minute is what the budget exists to refuse.
    host.transports.set_replacement_limit(usize::MAX);
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

/// Evidence, not a gate: what the wire carries across a takeover, one line per SDP.
/// `cargo test wire_evidence -- --ignored --nocapture`
#[test]
#[ignore = "prints the offers and answers of a takeover"]
fn wire_evidence() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("wire");
    let _a = first_attach(&mut host);
    let first_cert = dtls_certificate(&host.transports.video.webrtc);
    let _b = attach(&mut host);
    for (n, (what, pc, sdp)) in host.wire.iter().enumerate() {
        eprintln!("WIRE {n} {what} {pc}: {}", sdp_facts(sdp));
    }
    eprintln!(
        "WIRE the replaced video transport presents the same DTLS certificate: {}",
        first_cert.is_some() && first_cert == dtls_certificate(&host.transports.video.webrtc)
    );
}

/// The PEM a webrtcbin's DTLS transport presents.
fn dtls_certificate(webrtc: &gst::Element) -> Option<String> {
    let t = webrtc.emit_by_name::<Option<glib::Object>>("get-transceiver", &[&0i32])?;
    let sender = t.property::<Option<glib::Object>>("sender")?;
    let transport = sender.property::<Option<glib::Object>>("transport")?;
    transport.property::<Option<String>>("certificate")
}

/// Evidence, not a gate: does the host's webrtcbin check the answer's fingerprint against
/// the certificate the peer presents in DTLS?
#[test]
#[ignore = "prints whether a wrong fingerprint in the answer still connects"]
fn fingerprint_evidence() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("fingerprint");
    host.munge_answer = Some(with_another_certificate);
    let mut peer = Peer::new();
    let up = pump(&mut host, &mut peer, Duration::from_secs(10), |_, p| {
        streaming(p)
    });
    eprintln!(
        "FINGERPRINT a peer whose answer carries a wrong fingerprint streams: {up} ({})",
        describe(&peer)
    );
}

/// An answer as another browser would have written it: a certificate of its own. Every
/// peer here shares the process's one; webrtcbin does not check the fingerprint against
/// the handshake, so the peer still connects.
fn with_another_certificate(sdp: String) -> String {
    sdp.lines()
        .map(|l| match l.strip_prefix("a=fingerprint:sha-256 ") {
            Some(_) => format!("a=fingerprint:sha-256 {}", ["AB"; 32].join(":")),
            None => l.to_string(),
        })
        .collect::<Vec<_>>()
        .join("\r\n")
        + "\r\n"
}

/// An answer as another peer connection of the same process would have written it: other
/// ICE credentials, the same certificate.
fn with_other_ice(sdp: &str) -> String {
    sdp.replace("a=ice-ufrag:", "a=ice-ufrag:x")
}

/// The last answer the host was handed for `pc`.
fn last_answer(host: &Host, pc: PcId) -> String {
    host.wire
        .iter()
        .rev()
        .find(|(what, on, _)| *what == "answer" && *on == pc)
        .map(|(_, _, sdp)| sdp.clone())
        .unwrap_or_else(|| panic!("no {pc} answer on the wire"))
}

/// A page answered the old transports' offers and went away: each PC has a new transport
/// whose offer nobody will answer.
fn abandon_a_takeover(host: &mut Host) {
    for pc in [PcId::Video, PcId::Audio] {
        let sdp = with_other_ice(&last_answer(host, pc));
        host.inbound(SignalMsg::Answer { sdp, pc: Some(pc) });
    }
    assert_eq!(host.replaced, [PcId::Video, PcId::Audio]);
}

/// Does `pipeline` reach NULL within `within`? A state change that blocks is left behind
/// on its own thread.
fn stops_within(pipeline: &gst::Pipeline, within: Duration) -> bool {
    let pipeline = pipeline.clone();
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let _ = pipeline.set_state(gst::State::Null);
        let _ = tx.send(());
    });
    rx.recv_timeout(within).is_ok()
}

/// Counts the frames the source hands to the encode pipeline.
fn source_frames(host: &Host) -> Arc<AtomicU64> {
    let frames = Arc::new(AtomicU64::new(0));
    let count = frames.clone();
    let sink = host
        .source
        .iterate_sinks()
        .into_iter()
        .next()
        .and_then(Result::ok)
        .expect("the source's interpipesink");
    sink.static_pad("sink")
        .unwrap()
        .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
            count.fetch_add(1, Ordering::Relaxed);
            gst::PadProbeReturn::Ok
        });
    frames
}

/// A transport nobody answers holds the encode chain's RTP until its peer connects. The
/// source must not end up waiting on that: it could then be neither stopped nor swapped.
#[test]
fn a_new_transport_nobody_answers_does_not_hold_the_source() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("abandoned");
    let _a = first_attach(&mut host);
    let frames = source_frames(&host);

    abandon_a_takeover(&mut host);

    std::thread::sleep(Duration::from_millis(500));
    let before = frames.load(Ordering::Relaxed);
    std::thread::sleep(Duration::from_secs(1));
    assert!(
        frames.load(Ordering::Relaxed) > before + 15,
        "the source stalled behind a transport nobody answered"
    );
    let audio = host.transports.audio_webrtc.clone().expect("audio PC");
    let audio_pipeline = audio
        .parent()
        .and_downcast::<gst::Pipeline>()
        .expect("audio pipeline");
    assert!(
        stops_within(&audio_pipeline, Duration::from_secs(2)),
        "the audio pipeline did not stop behind a transport nobody answered"
    );
    assert!(
        stops_within(&host.source, Duration::from_secs(2)),
        "the source did not stop behind a transport nobody answered"
    );
}

/// The order `runner::stop_source` keeps. A source that renegotiates while a transport
/// holds RTP waits on the encode chain, whichever transport it is; the session's first
/// here, with the renegotiation forced.
#[test]
fn releasing_the_transport_frees_a_source_that_waits_on_it() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let host = Host::start("release");
    let frames = source_frames(&host);
    std::thread::sleep(Duration::from_millis(800));
    let sink = host.source.iterate_sinks().into_iter().next();
    let sink = sink.and_then(Result::ok).expect("interpipesink");
    sink.static_pad("sink")
        .unwrap()
        .push_event(gst::event::Reconfigure::new());
    std::thread::sleep(Duration::from_millis(500));
    let before = frames.load(Ordering::Relaxed);
    std::thread::sleep(Duration::from_millis(500));
    assert_eq!(
        frames.load(Ordering::Relaxed),
        before,
        "the source is not waiting on the transport: this test no longer shows the hazard"
    );

    host.transports.video.release();

    assert!(
        stops_within(&host.source, Duration::from_secs(2)),
        "the source did not stop once the transport was released"
    );
}

/// A page that answered two offers from the old transport has the second answer land on
/// the new one. A browser then answers the new transport's own offer with new ICE
/// credentials and the certificate it had, which must read as that page correcting
/// itself, not as one more peer: one more offer on the same transport, no third.
#[test]
fn a_late_answer_to_the_old_offer_costs_no_second_transport() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("late");
    let _a = first_attach(&mut host);
    // The taking page's two answers to the old transport's offers: the first replaces the
    // transport, the second arrives once the new one has offered.
    let to_old_offer = SignalMsg::Answer {
        sdp: with_other_ice(&last_answer(&host, PcId::Video)),
        pc: Some(PcId::Video),
    };
    host.inbound(to_old_offer.clone());
    assert_eq!(host.replaced, [PcId::Video]);
    let offered = Instant::now() + Duration::from_secs(10);
    while !super::has_offered(&host.transports.video.webrtc) {
        assert!(Instant::now() < offered, "the new transport never offered");
        std::thread::sleep(Duration::from_millis(10));
    }
    host.inbound(to_old_offer);

    // The page itself, answering the new transport's offer: other ICE credentials than
    // the answer above, the same certificate.
    let mut b = Peer::new();
    host.inbound(SignalMsg::RestartIce {
        pc: Some(PcId::Audio),
    });
    let up = pump(&mut host, &mut b, Duration::from_secs(20), |_, p| {
        streaming(p)
    });

    assert!(up, "the taking page never streamed: {}", describe(&b));
    assert_eq!(
        host.replaced,
        [PcId::Video, PcId::Audio],
        "the late answer cost a second video transport"
    );
    assert!(input_reaches_host(&mut host, &mut b));
    assert_eq!(host.bus_errors, Vec::<String>::new());
}

/// A whole session on `runner::run_blocking`, with signaling carried by hand: what the
/// runner does on its way out, which the [`Host`] above does not have.
struct Session {
    stop: Arc<std::sync::atomic::AtomicBool>,
    events: mpsc::Receiver<(String, SessionEvent)>,
    to_host: std::sync::mpsc::Sender<SignalMsg>,
    thread: Option<std::thread::JoinHandle<()>>,
    /// Offers the host sent, per PC.
    offers: [usize; 2],
    /// The last answer the peer gave, per PC.
    answers: [Option<String>; 2],
    /// The runner's last word: `Stopped` or `Failed`.
    ended: Option<SessionEvent>,
    /// Nothing the peer says reaches the host.
    mute: bool,
    // The runner reads a closed swap lane as its owner going away.
    _swap: std::sync::mpsc::Sender<crate::session::runner::SwapRequest>,
}

impl Session {
    fn start(tag: &str, idle_timeout: Duration) -> Session {
        let mut cfg = host_cfg(false);
        cfg.idle_timeout = idle_timeout;
        let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let (evt_tx, events) = mpsc::channel(256);
        let (diag_tx, _) = mpsc::channel(1);
        let diagnostics = DiagnosticEventTx::new(
            diag_tx,
            Arc::new(AtomicU64::new(0)),
            Arc::new(AtomicU64::new(0)),
        );
        let (to_host, sig_rx) = std::sync::mpsc::channel();
        let (swap, swap_rx) = std::sync::mpsc::channel();
        let (_, display_rx) = std::sync::mpsc::channel();
        let (_, capture_rx) = std::sync::mpsc::channel();
        let metrics = Arc::new(SessionMetrics::new("smooth", cfg.stream.fps as u32));
        let (id, flag) = (format!("takeover-{tag}"), stop.clone());
        let thread = std::thread::spawn(move || {
            crate::session::runner::run_blocking(
                id,
                cfg,
                evt_tx,
                diagnostics,
                flag,
                sig_rx,
                swap_rx,
                display_rx,
                capture_rx,
                metrics,
            )
        });
        Session {
            stop,
            events,
            to_host,
            thread: Some(thread),
            offers: [0; 2],
            answers: [None, None],
            ended: None,
            mute: false,
            _swap: swap,
        }
    }

    /// Carry signaling between the runner and `peer` until `done` or the deadline.
    fn pump(
        &mut self,
        peer: &mut Peer,
        within: Duration,
        mut done: impl FnMut(&Session, &Peer) -> bool,
    ) -> bool {
        let deadline = Instant::now() + within;
        loop {
            while let Ok((_, event)) = self.events.try_recv() {
                match event {
                    SessionEvent::Signaling(msg) => {
                        let pc = match &msg {
                            SignalMsg::Offer { pc, .. } => {
                                self.offers[pc.unwrap_or_default() as usize] += 1;
                                *pc
                            }
                            SignalMsg::Ice { pc, .. } => *pc,
                            _ => continue,
                        };
                        peer.pc(pc).inbound(msg);
                    }
                    SessionEvent::Stopped { .. } | SessionEvent::Failed(_) => {
                        self.ended = Some(event)
                    }
                    _ => {}
                }
            }
            for id in [PcId::Video, PcId::Audio] {
                while let Ok(msg) = peer.pc_mut(id).rx.try_recv() {
                    let msg = match msg {
                        SignalMsg::Answer { sdp, .. } => {
                            self.answers[id as usize] = Some(sdp.clone());
                            SignalMsg::Answer { sdp, pc: Some(id) }
                        }
                        SignalMsg::Ice { candidate, .. } => SignalMsg::Ice {
                            candidate,
                            pc: Some(id),
                        },
                        _ => continue,
                    };
                    if self.mute {
                        continue;
                    }
                    let _ = self.to_host.send(msg);
                }
            }
            if done(self, peer) {
                return true;
            }
            if Instant::now() >= deadline {
                return false;
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    /// A page streams; a second one answers the old transports' offers and goes away. Each
    /// PC is left on a new transport whose offer nobody will answer.
    fn with_an_abandoned_takeover(tag: &str, idle_timeout: Duration) -> Session {
        let mut session = Session::start(tag, idle_timeout);
        let mut a = Peer::new();
        let up = session.pump(&mut a, Duration::from_secs(20), |_, p| streaming(p));
        assert!(up, "the first peer never streamed: {}", describe(&a));
        for pc in [PcId::Video, PcId::Audio] {
            let sdp = with_other_ice(session.answers[pc as usize].as_ref().unwrap());
            let _ = session
                .to_host
                .send(SignalMsg::Answer { sdp, pc: Some(pc) });
        }
        let replaced = session.pump(&mut a, Duration::from_secs(10), |s, _| s.offers == [2, 2]);
        assert!(
            replaced,
            "offers per PC after the takeover: {:?}",
            session.offers
        );
        // RTP reaches the new transports and parks there.
        session.pump(&mut a, Duration::from_millis(500), |_, _| false);
        session
    }

    /// Wait for the runner's last event and for its thread, which returns only once the
    /// source, the encode pipeline and the audio pipeline are down.
    fn ends_within(&mut self, within: Duration) -> Option<SessionEvent> {
        let mut nobody = Peer::new();
        self.pump(&mut nobody, within, |s, _| {
            s.ended.is_some() && s.thread.as_ref().is_some_and(|t| t.is_finished())
        });
        if self.thread.as_ref().is_some_and(|t| t.is_finished()) {
            let _ = self.thread.take().map(|t| t.join());
        }
        self.thread.is_none().then(|| self.ended.clone()).flatten()
    }
}

impl Drop for Session {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
    }
}

/// The page that took the session over went away before answering. The operator's stop
/// must still end the session: terminal event, and the runner off its thread.
#[test]
fn an_abandoned_takeover_does_not_keep_the_session_from_stopping() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut session = Session::with_an_abandoned_takeover("stop", Duration::from_secs(600));

    session.stop.store(true, Ordering::SeqCst);

    let ended = session.ends_within(Duration::from_secs(15));
    assert!(
        matches!(ended, Some(SessionEvent::Stopped { .. })),
        "the session did not stop: {ended:?}"
    );
}

/// The same, left alone: the idle reap ends it.
#[test]
fn an_abandoned_takeover_is_reaped_when_idle() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut session = Session::with_an_abandoned_takeover("reap", Duration::from_secs(8));

    let ended = session.ends_within(Duration::from_secs(30));
    assert!(
        matches!(&ended, Some(SessionEvent::Failed(reason)) if reason.starts_with("idle:")),
        "the session was not reaped: {ended:?}"
    );
}

/// The session's first transport, never answered by anyone: a stop ends it on `develop`
/// too, and still does.
#[test]
fn a_session_nobody_answered_stops() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut session = Session::start("unanswered", Duration::from_secs(600));
    session.mute = true;
    let mut nobody = Peer::new();
    let offered = session.pump(&mut nobody, Duration::from_secs(20), |s, _| {
        s.offers == [1, 1]
    });
    assert!(offered, "offers per PC: {:?}", session.offers);
    session.pump(&mut nobody, Duration::from_millis(800), |_, _| false);

    session.stop.store(true, Ordering::SeqCst);

    let ended = session.ends_within(Duration::from_secs(15));
    assert!(
        matches!(ended, Some(SessionEvent::Stopped { .. })),
        "the session did not stop: {ended:?}"
    );
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
        let mic = std::env::var("QUASAR_TAKEOVER_MIC").is_ok();
        // Ask for an ICE restart the moment the video transport is replaced: a client's
        // recovery request landing between the old transport's offer and the new one's.
        // And again once that transport's offer is out and not yet answered.
        let restart_between = std::env::var("QUASAR_TAKEOVER_RESTART_BETWEEN").is_ok();
        let mut restart_after_offer = false;
        // Replay the answer that replaced the video transport once the new transport has
        // offered: the taking page's second answer to the old transport's offer.
        let late_answer = std::env::var("QUASAR_TAKEOVER_LATE_ANSWER").is_ok();
        let mut late: Option<SignalMsg> = None;
        let mut host = Host::start_with("browser", mic);
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
                    let ws = match tokio_tungstenite::accept_async(stream).await {
                        Ok(ws) => ws,
                        Err(e) => {
                            eprintln!("HOST websocket handshake failed: {e}");
                            continue;
                        }
                    };
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
                            Ok(msg) => {
                                if let SignalMsg::Answer { sdp, pc } = &msg {
                                    eprintln!(
                                        "HOST answer {} from socket {id}: {}",
                                        pc.unwrap_or_default(),
                                        sdp_facts(sdp)
                                    );
                                }
                                host.inbound(msg)
                            }
                            Err(e) => eprintln!("HOST bad frame from {id}: {e}"),
                        }
                    }
                }
                _ = tick.tick() => {
                    while let Ok(msg) = host.sig_rx.try_recv() {
                        let mut video_offer = false;
                        if let SignalMsg::Offer { sdp, pc } = &msg {
                            eprintln!("HOST offer {}: {}", pc.unwrap_or_default(), sdp_facts(sdp));
                            video_offer = pc.unwrap_or_default() == PcId::Video;
                        }
                        let frame = Message::Text(msg.to_json().unwrap().into());
                        match &attached {
                            Some((_, tx)) => { let _ = tx.send(frame); }
                            None => buffered.push(frame),
                        }
                        if video_offer && std::mem::take(&mut restart_after_offer) {
                            host.inbound(SignalMsg::RestartIce { pc: Some(PcId::Video) });
                        }
                        if let Some(answer) = late.take_if(|_| video_offer) {
                            eprintln!("HOST replaying the answer that replaced the video transport");
                            host.inbound(answer);
                        }
                    }
                    host.drain_bus();
                    for e in &host.bus_errors[seen_errors..] {
                        eprintln!("HOST bus error: {e}");
                    }
                    seen_errors = host.bus_errors.len();
                    let replaced: Vec<PcId> = host.replaced[seen_replaced..].to_vec();
                    seen_replaced = host.replaced.len();
                    for pc in replaced {
                        eprintln!("HOST replaced the {pc} transport");
                        if restart_between && pc == PcId::Video {
                            host.inbound(SignalMsg::RestartIce { pc: Some(pc) });
                            restart_after_offer = true;
                        }
                        if late_answer && pc == PcId::Video {
                            late = Some(SignalMsg::Answer {
                                sdp: last_answer(&host, pc),
                                pc: Some(pc),
                            });
                        }
                    }
                }
            }
        }
    });
}
