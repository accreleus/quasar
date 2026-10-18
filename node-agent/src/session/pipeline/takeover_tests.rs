//! #535: the production encode pipeline and the runner's transport state against real
//! `webrtcbin` peers, in process.
//!
//! A peer here is what a browser page is to the host: its own ICE agent and DTLS and
//! SCTP associations. Signaling is carried by hand, the way the control-plane relay
//! carries it: host frames reach only the attached peer. What a browser adds (an SCTP
//! abort when the page goes) is `scripts/harness/run-takeover-peers.sh`.

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
/// `make test-rust` runs in has no gst-interpipe, and a CI runner may lack more. A test
/// that returns early counts as passed, so the skip is written past libtest's capture,
/// once, and `scripts/verify/agent.sh` restates it. With `QUASAR_REQUIRE_LIVE_WEBRTC`
/// set the skip is a failure (#571).
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
        if std::env::var_os("QUASAR_REQUIRE_LIVE_WEBRTC").is_some() {
            panic!(
                "the live two-peer WebRTC tests cannot run: {missing:?} missing from this \
                 image, and QUASAR_REQUIRE_LIVE_WEBRTC is set"
            );
        }
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
    host_cfg_with(mic, false)
}

/// `fps_rung`: the scale stage also carries the frame-rate lever (`abr_ladder_fps`).
fn host_cfg_with(mic: bool, fps_rung: bool) -> SessionConfig {
    let mut settings = crate::session::settings::RuntimeSettings::baseline_with(&|_| None);
    settings.encoder = EncoderChoice::Openh264;
    settings.ladder.fps_enabled = fps_rung;
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
    /// The longest the runner's inbound dispatch took over one message.
    slowest_inbound: Duration,
    /// Rewrites an answer before the host sees it.
    munge_answer: Option<fn(String) -> String>,
    sig_tx: OutTx,
    sig_rx: mpsc::UnboundedReceiver<SignalMsg>,
    bus: gst::Bus,
    /// Every error the encode bus posted, message and debug text joined.
    bus_errors: Vec<String>,
    encoder: gst::Element,
    cfg: SessionConfig,
    interpipesrc: gst::Element,
    lever: Arc<super::EncodeResolutionLever>,
    /// When the encoder emitted each keyframe.
    keyframes: Arc<Mutex<Vec<Instant>>>,
    /// RECONFIGURE events that reached the encoder from downstream.
    reconfigures: Arc<AtomicU64>,
    metrics: Arc<SessionMetrics>,
    _trace_rx: mpsc::Receiver<(String, crate::session::runner::TraceEvent)>,
}

impl Host {
    fn start(tag: &str) -> Host {
        Host::start_with(tag, false)
    }

    /// `mic`: the session was launched with the microphone granted, so the audio PC
    /// offers a second, recvonly m-line (decoded into a fakesink here: no sidecar).
    fn start_with(tag: &str, mic: bool) -> Host {
        Host::start_cfg(tag, host_cfg(mic))
    }

    fn start_cfg(tag: &str, mut cfg: SessionConfig) -> Host {
        trace_to_stderr();
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
            interpipesrc,
            resolution_lever: lever,
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
            slowest_inbound: Duration::ZERO,
            munge_answer: None,
            sig_tx,
            sig_rx,
            bus,
            bus_errors: Vec::new(),
            encoder,
            cfg,
            interpipesrc,
            lever,
            keyframes,
            reconfigures,
            metrics,
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
        let started = Instant::now();
        let replaced = self
            .transports
            .inbound(msg, &self.sig_tx, None, None)
            .expect("replace the video transport");
        self.slowest_inbound = self.slowest_inbound.max(started.elapsed());
        self.replaced.extend(replaced);
    }

    /// What a size or frame-rate step does to the chain: new caps at the scale stage and
    /// a RECONFIGURE from there up to the source. Forced where the rig's encode path has
    /// no stage.
    fn step_the_size(&self) {
        use super::ResolutionLever;
        if self.lever.set_rung(160, 120).is_err() {
            let sink = self.encoder.static_pad("sink").unwrap();
            sink.push_event(gst::event::Reconfigure::new());
        }
    }

    /// Swap the source as `runner::perform_swap` does: a second one comes up, the encoder
    /// is re-pointed at it, the first goes down. Whether the first stopped in time.
    fn swap_source(&mut self, tag: &str) -> bool {
        let sink = format!("takeover-{tag}-{}", std::process::id());
        let next = build_source_pipeline(&self.cfg, None, &sink).expect("second source");
        next.set_start_time(None::<gst::ClockTime>);
        next.use_clock(Some(&gst::SystemClock::obtain()));
        next.set_base_time(self.pipeline.base_time().expect("encode base time"));
        next.set_state(gst::State::Playing)
            .expect("second source PLAYING");
        self.interpipesrc.set_property("listen-to", sink.as_str());
        let old = std::mem::replace(&mut self.source, next);
        stops_within(&old, Duration::from_secs(2))
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
    /// The H.264 NAL unit type the first RTP packet out of webrtcbin starts with.
    first_nal: Arc<Mutex<Option<u8>>>,
    /// The `a=fingerprint` value of this PC's own certificate, when it was given one.
    fingerprint: Option<String>,
}

/// The NAL unit type an H.264 RTP packet starts with, looking inside a STAP-A or FU-A.
fn first_nal_type(info: &gst::PadProbeInfo) -> Option<u8> {
    let buffer: &gst::BufferRef = match info.buffer() {
        Some(buffer) => buffer.as_ref(),
        None => info.buffer_list()?.get(0)?,
    };
    let rtp = gstreamer_rtp::RTPBuffer::from_buffer_readable(buffer).ok()?;
    let payload = rtp.payload().ok()?;
    Some(match payload.first()? & 0x1f {
        24 => payload.get(3)? & 0x1f,
        28 => payload.get(1)? & 0x1f,
        nal => nal,
    })
}

/// A certificate of a peer's own, as each browser has. Every `webrtcbin` in a process
/// otherwise presents the one certificate GStreamer generates, and the host checks an
/// answer's `a=fingerprint` against the certificate it is shown in DTLS, so a peer that
/// stands for another browser needs a real one.
struct Certificate {
    /// Certificate and key, PEM.
    pem: String,
    /// What an SDP says of it: `sha-256 AB:CD:…`.
    fingerprint: String,
}

impl Certificate {
    fn mint() -> Certificate {
        use sha2::Digest;
        let rcgen::CertifiedKey { cert, key_pair } =
            rcgen::generate_simple_self_signed(vec!["peer".to_string()]).expect("certificate");
        let digest: Vec<String> = sha2::Sha256::digest(cert.der())
            .iter()
            .map(|byte| format!("{byte:02X}"))
            .collect();
        Certificate {
            pem: cert.pem() + &key_pair.serialize_pem(),
            fingerprint: format!("sha-256 {}", digest.join(":")),
        }
    }
}

impl PeerPc {
    fn new(id: PcId, certificate: Option<&Certificate>) -> PeerPc {
        let (tx, rx) = mpsc::unbounded_channel();
        let (pipeline, webrtc) = build_answerer_pipeline(tx.clone()).expect("answerer");
        // A certificate must be set while the DTLS elements are in NULL: `dtlsenc` takes
        // its connection on the way to READY, and set after that the peer advertises this
        // certificate and presents the process's one. webrtcbin builds a transport and
        // starts it in one call, so the only moment is as the element is added.
        if let Some(pem) = certificate.map(|c| c.pem.clone()) {
            webrtc
                .downcast_ref::<gst::Bin>()
                .expect("webrtcbin is a bin")
                .connect_deep_element_added(move |_, _, element| {
                    if element.factory().is_some_and(|f| f.name() == "dtlsdec") {
                        element.set_property("pem", &pem);
                    }
                });
        }
        let rtp = Arc::new(AtomicU64::new(0));
        let counter = rtp.clone();
        let first_nal = Arc::new(Mutex::new(None));
        let first = first_nal.clone();
        webrtc.connect_pad_added(move |_, pad| {
            if pad.direction() != gst::PadDirection::Src {
                return;
            }
            let (counter, first) = (counter.clone(), first.clone());
            pad.add_probe(
                gst::PadProbeType::BUFFER | gst::PadProbeType::BUFFER_LIST,
                move |_, info| {
                    if counter.fetch_add(1, Ordering::Relaxed) == 0 {
                        *first.lock().unwrap() = first_nal_type(info);
                    }
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
            first_nal,
            fingerprint: certificate.map(|c| c.fingerprint.clone()),
        }
    }

    /// The next frame this PC has for the host. webrtcbin misstates the fingerprint of a
    /// certificate it was given (it hashes the key along with it,
    /// `_add_fingerprint_to_media`), so an answer is made to name the certificate's own.
    fn outbound(&mut self) -> Option<SignalMsg> {
        Some(match (self.rx.try_recv().ok()?, &self.fingerprint) {
            (SignalMsg::Answer { sdp, pc }, Some(own)) => SignalMsg::Answer {
                sdp: with_fingerprint(&sdp, own),
                pc,
            },
            (msg, _) => msg,
        })
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
    /// A page of the one browser this process is: the certificate every peer here shares.
    fn new() -> Peer {
        Peer::with(None)
    }

    /// A page in another browser: a certificate of its own.
    fn another_browser() -> Peer {
        Peer::with(Some(&Certificate::mint()))
    }

    fn with(certificate: Option<&Certificate>) -> Peer {
        Peer {
            video: PeerPc::new(PcId::Video, certificate),
            audio: PeerPc::new(PcId::Audio, certificate),
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
            while let Some(msg) = attached.pc_mut(id).outbound() {
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

/// Every WARN and ERROR line this process logged, as `field=value ` pairs.
static WARNINGS: Mutex<Vec<String>> = Mutex::new(Vec::new());

struct WarningLog;

impl<S: tracing::Subscriber> tracing_subscriber::Layer<S> for WarningLog {
    fn on_event(&self, event: &tracing::Event<'_>, _: tracing_subscriber::layer::Context<'_, S>) {
        struct Line(String);
        impl tracing::field::Visit for Line {
            fn record_debug(&mut self, field: &tracing::field::Field, value: &dyn std::fmt::Debug) {
                self.0 += &format!("{}={value:?} ", field.name());
            }
        }
        if *event.metadata().level() <= tracing::Level::WARN {
            let mut line = Line(String::new());
            event.record(&mut line);
            WARNINGS.lock().unwrap().push(line.0);
        }
    }
}

/// Agent log lines on stderr, for the browser host, and its warnings kept in
/// [`WARNINGS`]. Process-wide: `webrtcbin`'s threads carry no span, so a subscriber set
/// for the test's thread never sees what they log.
fn trace_to_stderr() {
    use tracing_subscriber::prelude::*;
    let _ = tracing_subscriber::registry()
        .with(
            tracing_subscriber::fmt::layer()
                .with_test_writer()
                .with_filter(tracing_subscriber::EnvFilter::new("quasar_node_agent=info")),
        )
        .with(WarningLog)
        .try_init();
}

/// The PCs of session `tag` that warned their peer failed the fingerprint check, one
/// entry per line logged.
fn fingerprint_warnings(tag: &str) -> Vec<&'static str> {
    let session = format!("session_id=takeover-{tag} ");
    let lines = WARNINGS.lock().unwrap();
    let said = |pc: &str| {
        lines
            .iter()
            .filter(|l| l.contains(r#"token="transport-peer-fingerprint-mismatch""#))
            .filter(|l| l.contains(&session) && l.contains(&format!("{pc} PC:")))
            .count()
    };
    ["video", "audio"]
        .into_iter()
        .flat_map(|pc| std::iter::repeat_n(pc, said(pc)))
        .collect()
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
    attach_as(host, Peer::new())
}

fn attach_as(host: &mut Host, mut peer: Peer) -> Peer {
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
    assert_eq!(
        fingerprint_warnings("midstream"),
        Vec::<&str>::new(),
        "neither honest peer failed the fingerprint check"
    );
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
    let b = if another_browser {
        Peer::another_browser()
    } else {
        Peer::new()
    };
    let mut b = attach_as(&mut host, b);
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

    // The window clears: the refused peer's next request is served like any other.
    host.transports.forget_replacements();
    request_offers(&mut host);
    let up = pump(&mut host, &mut c, Duration::from_secs(20), |_, p| {
        streaming(p)
    });
    assert!(
        up,
        "the refused peer was not served once the window cleared: {}",
        describe(&c)
    );
    assert_eq!(host.replaced.len(), 4);
    assert!(input_reaches_host(&mut host, &mut c));
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

/// #564: a session a peer connected to gives back the contexts its encode pipeline was
/// handed. `webrtcbin`'s `get-stats`, asked for each time ICE settles, kept the video PC's
/// rtpsession alive past teardown, and an element keeps its contexts until it is
/// disposed: on a Vulkan session, the `GstVulkanDevice`.
#[test]
fn a_streamed_session_gives_back_the_encode_pipelines_contexts() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("contexts");
    // Stands in for the producer's device (`runner::install_vulkan_context_bridge`).
    let device = gst::Bin::new();
    let held = device.downgrade();
    let mut context = gst::Context::new("gst.vulkan.device", true);
    context
        .get_mut()
        .unwrap()
        .structure_mut()
        .set("device", device);
    host.pipeline.set_context(&context);
    drop(context);
    let peer = first_attach(&mut host);
    // A replaced transport too: a second browser takes the session over.
    let taker = attach_as(&mut host, Peer::another_browser());
    drop((peer, taker));
    // The runner's `MetricsHookGuard`.
    host.metrics.clear_hooks();
    drop(host);
    let deadline = Instant::now() + Duration::from_secs(2);
    while held.upgrade().is_some() && Instant::now() < deadline {
        std::thread::sleep(Duration::from_millis(20));
    }
    assert!(
        held.upgrade().is_none(),
        "an element of the ended session still holds the encode pipeline's contexts"
    );
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

/// A second browser takes a streaming session over. Its certificate is its own, and the
/// host hears it only because its answer's fingerprint is that certificate's.
#[test]
fn a_takeover_by_a_peer_with_its_own_certificate_reaches_the_host() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("own-cert");
    let _a = first_attach(&mut host);

    let mut b = attach_as(&mut host, Peer::another_browser());

    assert_ne!(
        sdp_value(&last_answer(&host, PcId::Video), "a=fingerprint:"),
        sdp_value(video_sdp(&host, "answer", 0), "a=fingerprint:"),
        "the second peer answered with the first one's certificate"
    );
    let mut replaced = host.replaced.clone();
    replaced.sort_by_key(|pc| *pc as usize);
    assert_eq!(replaced, [PcId::Video, PcId::Audio]);
    assert!(
        input_reaches_host(&mut host, &mut b),
        "a peer with a certificate of its own, truthfully advertised, is not heard"
    );
    assert_eq!(host.bus_errors, Vec::<String>::new());
    assert_eq!(fingerprint_warnings("own-cert"), Vec::<&str>::new());
}

/// The host checks an answer's `a=fingerprint` against the certificate the peer presents
/// in DTLS (webrtcbin, GStreamer 1.28.5 on). A peer that fails it is still sent media, and
/// nothing it sends is heard: no input channel, no RTCP. If this fails the check is gone,
/// and the fingerprint `webrtc::answer_from` tells peers apart by authenticates nothing.
#[test]
fn a_peer_whose_fingerprint_is_not_its_certificates_gets_video_and_no_input_channel() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("forged");
    host.munge_answer = Some(with_another_certificate);
    let mut peer = Peer::new();
    let video = pump(&mut host, &mut peer, Duration::from_secs(20), |_, p| {
        rtp(&p.video) > 30
    });
    assert!(
        video,
        "outbound media must not wait on the fingerprint: {}",
        describe(&peer)
    );
    let reported = pump(&mut host, &mut peer, Duration::from_secs(20), |_, _| {
        fingerprint_warnings("forged").len() >= 2
    });
    assert!(reported, "no warning: {}", describe(&peer));
    assert!(
        !input_reaches_host(&mut host, &mut peer),
        "the host heard a peer whose answer names a certificate it did not present"
    );
    assert!(
        peer.video.channel.lock().unwrap().is_none(),
        "the input channel opened to a peer whose answer names a certificate it did not present"
    );
    assert_eq!(
        fingerprint_warnings("forged"),
        ["video", "audio"],
        "each transport says once that its peer failed the check"
    );
}

/// A transport built for a takeover reports a failed check as the launch one does, and
/// the honest peer it displaced was never reported (#577).
#[test]
fn a_takeover_by_a_peer_that_fails_the_fingerprint_check_is_reported() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("forged-takeover");
    let _a = first_attach(&mut host);

    host.munge_answer = Some(with_another_certificate);
    request_offers(&mut host);
    let mut b = Peer::new();
    let reported = pump(&mut host, &mut b, Duration::from_secs(20), |_, _| {
        fingerprint_warnings("forged-takeover").len() >= 2
    });
    assert!(reported, "no warning: {}", describe(&b));
    pump(&mut host, &mut b, Duration::from_secs(1), |_, _| false);

    let mut replaced = host.replaced.clone();
    replaced.sort_by_key(|pc| *pc as usize);
    assert_eq!(replaced, [PcId::Video, PcId::Audio]);
    assert_eq!(
        fingerprint_warnings("forged-takeover"),
        ["video", "audio"],
        "each replacement transport says once that its peer failed the check"
    );
    assert_eq!(host.bus_errors, Vec::<String>::new());
}

/// An answer naming a certificate nobody holds. With no peer behind it, it stands for
/// another browser's answer; a peer that connects behind it is shut out by the host.
fn with_another_certificate(sdp: String) -> String {
    with_fingerprint(&sdp, &format!("sha-256 {}", ["AB"; 32].join(":")))
}

/// `sdp` with every `a=fingerprint` naming `fingerprint`.
fn with_fingerprint(sdp: &str, fingerprint: &str) -> String {
    sdp.lines()
        .map(|l| match l.starts_with("a=fingerprint:") {
            true => format!("a=fingerprint:{fingerprint}"),
            false => l.to_string(),
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

/// The session's audio capture, once it runs.
fn audio_capture(host: &Host) -> gst::Element {
    let capture = host
        .transports
        .audio_pipeline
        .as_ref()
        .expect("audio pipeline")
        .iterate_elements()
        .into_iter()
        .filter_map(Result::ok)
        .find(|e| e.factory().is_some_and(|f| f.name() == "audiotestsrc"))
        .expect("the audio capture");
    let (_, state, _) = capture.state(gst::ClockTime::from_seconds(5));
    assert_eq!(state, gst::State::Playing, "the audio capture never ran");
    capture
}

/// What `pulsesrc` posts when its server goes away under it.
fn sound_server_lost(capture: &gst::Element) {
    gst::element_error!(
        capture,
        gst::ResourceError::Failed,
        ("Disconnected: Connection terminated")
    );
}

/// What `f` logs on this thread. The audio bus handler logs on the thread that posts.
fn logged(f: impl FnOnce()) -> String {
    #[derive(Clone, Default)]
    struct Lines(Arc<Mutex<Vec<u8>>>);
    impl std::io::Write for Lines {
        fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(bytes);
            Ok(bytes.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }
    let lines = Lines::default();
    let writer = lines.clone();
    let subscriber = tracing_subscriber::fmt()
        .with_writer(move || writer.clone())
        .with_ansi(false)
        .finish();
    tracing::subscriber::with_default(subscriber, f);
    let bytes = lines.0.lock().unwrap().clone();
    String::from_utf8_lossy(&bytes).into_owned()
}

/// The session's sound server as its end sees it. Nothing here can count on a
/// PulseAudio-protocol server, so this one keeps what it found when it was stopped, and a
/// capture still running then reports the loss as `pulsesrc` does.
struct SoundServer {
    capture: gst::Element,
    /// The capture's state, and whether the audio bus still took a message.
    at_stop: Arc<Mutex<Option<(gst::State, bool)>>>,
}

impl crate::session::teardown::Sidecar for SoundServer {
    fn server_uri(&self) -> String {
        "unix:/nonexistent/native".into()
    }
    fn socket_dir(&self) -> std::path::PathBuf {
        "/nonexistent".into()
    }
    fn adopt_budget(&mut self, _: crate::session::teardown::RetryBudget) {}
    fn stop(&mut self) {
        let mut seen = self.at_stop.lock().unwrap();
        if seen.is_some() {
            return;
        }
        let state = self.capture.current_state();
        // A flushing bus refuses a post. This message is no warning or error, so the
        // handler drops it unlogged: its 60 s rate limit, which can hide the error below
        // on a bus that does report, has no part in the answer.
        let probe = gst::message::Application::new(gst::Structure::new_empty("probe"));
        let reporting = self
            .capture
            .parent()
            .and_downcast::<gst::Pipeline>()
            .and_then(|audio| audio.bus())
            .is_some_and(|bus| bus.post(probe).is_ok());
        *seen = Some((state, reporting));
        if matches!(state, gst::State::Paused | gst::State::Playing) {
            sound_server_lost(&self.capture);
        }
    }
}

/// #588: every session end stops the sound server under a capture that is still PLAYING
/// (`session::teardown`, "Audio ordering"), and the capture's report of that read as an
/// audio fault.
#[test]
fn a_session_end_does_not_report_its_sound_server_stopping() {
    use crate::session::source::{AppSource, SessionResources};
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let host = Host::start("audio-end");
    let at_stop = Arc::new(Mutex::new(None));
    let res = SessionResources::with_sidecar(
        "takeover-audio-end",
        Box::new(SoundServer {
            capture: audio_capture(&host),
            at_stop: at_stop.clone(),
        }),
    );
    let sink = format!("takeover-audio-end-app-{}", std::process::id());
    let mut source = AppSource::new(&host.cfg, "takeover-audio-end", &sink, &sink, &res, None)
        .expect("app source");

    let log = logged(|| crate::session::runner::stop_source(&mut source, &host.transports));

    // Read out first: a failed assertion that held this lock would poison it for the
    // stops the session's destructors still make.
    let at_stop = *at_stop.lock().unwrap();
    assert!(
        !log.contains("audio-pipeline-message"),
        "the session end logged an audio fault:\n{log}"
    );
    // The log alone could be quiet for another reason: the handler's rate limit, had
    // this pipeline logged anything in the minute before.
    assert_eq!(
        at_stop,
        Some((gst::State::Playing, false)),
        "when the sound server was stopped (capture state, audio bus still reporting): \
         the capture is still up, because taking it down first waits on that server, and \
         its bus reports nothing any more"
    );
}

/// The report itself is as it was: a sound server lost under a live session (#392, #351)
/// still reads as one.
#[test]
fn a_sound_server_lost_under_a_live_session_is_still_reported() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let host = Host::start("audio-lost");
    let capture = audio_capture(&host);

    let log = logged(|| sound_server_lost(&capture));

    assert!(
        log.contains("WARN")
            && log.contains("audio-pipeline-message")
            && log.contains("Disconnected: Connection terminated"),
        "a sound server lost under a live session was not reported:\n{log}"
    );
}

/// The last thing a source stop does is stop its sound server, after the app container
/// (`AppSource::teardown`); this one notes what the session end had done by then.
struct SourceStopWitness {
    emitted: Arc<Mutex<Vec<SessionEvent>>>,
    video: gst::Element,
    /// `(bye already emitted, the video transport's state)`.
    at_stop: Arc<Mutex<Option<(bool, gst::State)>>>,
}

impl crate::session::teardown::Sidecar for SourceStopWitness {
    fn server_uri(&self) -> String {
        "unix:/nonexistent/native".into()
    }
    fn socket_dir(&self) -> std::path::PathBuf {
        "/nonexistent".into()
    }
    fn adopt_budget(&mut self, _: crate::session::teardown::RetryBudget) {}
    fn stop(&mut self) {
        let said_bye = self
            .emitted
            .lock()
            .unwrap()
            .iter()
            .any(|e| matches!(e, SessionEvent::Signaling(SignalMsg::Bye)));
        self.at_stop
            .lock()
            .unwrap()
            .get_or_insert((said_bye, self.video.current_state()));
    }
}

/// #753: on a game exit the browser hears `bye`, after the `stopping` row it reads, and the
/// video transport is gone before the source stop, which waits on Steam's exit.
#[test]
fn a_game_exit_says_bye_and_drops_the_video_before_the_source_stops() {
    use crate::session::source::{AppSource, SessionResources};
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let host = Host::start("game-exit");
    let emitted = Arc::new(Mutex::new(Vec::new()));
    let at_stop = Arc::new(Mutex::new(None));
    let res = SessionResources::with_sidecar(
        "takeover-game-exit",
        Box::new(SourceStopWitness {
            emitted: emitted.clone(),
            video: host.transports.video.webrtc.clone(),
            at_stop: at_stop.clone(),
        }),
    );
    let sink = format!("takeover-game-exit-app-{}", std::process::id());
    let mut source = AppSource::new(&host.cfg, "takeover-game-exit", &sink, &sink, &res, None)
        .expect("app source");

    let emit = |e: SessionEvent| emitted.lock().unwrap().push(e);
    crate::session::runner::stop_for_game_exit(&emit, &mut source, &host.transports);

    let at_stop = *at_stop.lock().unwrap();
    assert_eq!(at_stop, Some((true, gst::State::Null)), "(bye sent, video state) at the source stop");
    let emitted = emitted.lock().unwrap();
    assert!(
        matches!(
            emitted.as_slice(),
            [
                SessionEvent::StoppingWith("game exited"),
                SessionEvent::Signaling(SignalMsg::Bye)
            ]
        ),
        "emitted {emitted:?}"
    );
}

/// A sound server with nothing to stop.
struct NoSoundServer;

impl crate::session::teardown::Sidecar for NoSoundServer {
    fn server_uri(&self) -> String {
        "unix:/nonexistent/native".into()
    }
    fn socket_dir(&self) -> std::path::PathBuf {
        "/nonexistent".into()
    }
    fn adopt_budget(&mut self, _: crate::session::teardown::RetryBudget) {}
    fn stop(&mut self) {}
}

/// Is the thread that replaces the audio transport (`runner::replace_audio`) alive?
fn audio_replacement_running() -> bool {
    std::fs::read_dir("/proc/self/task").is_ok_and(|tasks| {
        tasks.filter_map(Result::ok).any(|task| {
            std::fs::read_to_string(task.path().join("comm"))
                .is_ok_and(|name| name.trim_end() == "quasar-aud-repl")
        })
    })
}

/// #613: a takeover takes the old audio pipeline to NULL, and `pulsesrc` waits on its
/// sound server on the way: 30 s when that server is frozen. The runner must not wait
/// with it, so a stop is still served; and the stop the worker sees must neither be
/// reported (#588) nor be followed by another build.
#[test]
fn a_takeover_does_not_hold_the_runner_on_an_audio_pipeline_that_will_not_stop() {
    use crate::session::source::{AppSource, SessionResources};
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("audio-held");
    let _first = first_attach(&mut host);
    // Parks the capture's streaming thread, so its pipeline cannot reach NULL until
    // `release` drops: after 10 s at most, so a runner that waits fails and does not hang.
    let (release, held) = std::sync::mpsc::channel::<()>();
    let (parked_tx, parked) = std::sync::mpsc::channel::<()>();
    let probe = Mutex::new((held, parked_tx));
    audio_capture(&host).static_pad("src").unwrap().add_probe(
        gst::PadProbeType::BUFFER,
        move |_, _| {
            let probe = probe.lock().unwrap();
            let _ = probe.1.send(());
            let _ = probe.0.recv_timeout(Duration::from_secs(10));
            gst::PadProbeReturn::Ok
        },
    );
    parked
        .recv_timeout(Duration::from_secs(5))
        .expect("the audio capture never reached the probe");
    let old_bus = host
        .transports
        .audio_pipeline
        .as_ref()
        .unwrap()
        .bus()
        .unwrap();

    request_offers(&mut host);
    let mut next = Peer::new();
    let up = pump(&mut host, &mut next, Duration::from_secs(20), |h, p| {
        rtp(&p.video) > 30 && h.replaced.contains(&PcId::Audio)
    });
    assert!(
        up,
        "the peer that took over never got video, or its audio transport was not \
         replaced: {} (host replaced {:?})",
        describe(&next),
        host.replaced
    );
    assert!(
        host.slowest_inbound < Duration::from_secs(3),
        "the takeover held the runner {:?} on the old audio pipeline",
        host.slowest_inbound
    );
    assert!(
        host.transports.audio_webrtc.is_none(),
        "past the bound the new peer gets no audio"
    );
    let probe = gst::message::Application::new(gst::Structure::new_empty("probe"));
    assert!(
        old_bus.post(probe).is_err(),
        "the old audio pipeline still reports while it stops: a session end would log the \
         sound server's stop as a fault (#588)"
    );

    let res = SessionResources::with_sidecar("takeover-audio-held", Box::new(NoSoundServer));
    let sink = format!("takeover-audio-held-app-{}", std::process::id());
    let mut source = AppSource::new(&host.cfg, "takeover-audio-held", &sink, &sink, &res, None)
        .expect("app source");
    let stopping = Instant::now();
    crate::session::runner::stop_source(&mut source, &host.transports);
    assert!(
        stopping.elapsed() < Duration::from_secs(3),
        "the stop took {:?}",
        stopping.elapsed()
    );
    assert!(
        audio_replacement_running(),
        "the old audio pipeline was not held through the stop"
    );
    let builds = || {
        let all = super::transport::AUDIO_BUILDS.lock().unwrap();
        all.iter().filter(|id| *id == "takeover-audio-held").count()
    };
    let built_before_release = builds();

    drop(release);
    let deadline = Instant::now() + Duration::from_secs(10);
    while audio_replacement_running() && Instant::now() < deadline {
        std::thread::sleep(Duration::from_millis(50));
    }
    assert!(
        !audio_replacement_running(),
        "the old audio pipeline never stopped once released"
    );
    assert_eq!(
        builds(),
        built_before_release,
        "the takeover's worker built an audio pipeline after the session stopped"
    );
}

/// The session's first transport holds no RTP either while nobody has answered: a source
/// that renegotiates then keeps running and can be stopped. (Held RTP is what
/// `runner::stop_source` releases a transport for first; none is held any more.)
#[test]
fn a_source_that_renegotiates_before_anyone_answered_keeps_running() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let host = Host::start("unanswered-reneg");
    let frames = source_frames(&host);
    std::thread::sleep(Duration::from_millis(800));
    let sink = host.source.iterate_sinks().into_iter().next();
    let sink = sink.and_then(Result::ok).expect("interpipesink");
    sink.static_pad("sink")
        .unwrap()
        .push_event(gst::event::Reconfigure::new());
    std::thread::sleep(Duration::from_millis(500));
    let before = frames.load(Ordering::Relaxed);
    std::thread::sleep(Duration::from_secs(1));
    assert!(
        frames.load(Ordering::Relaxed) > before + 15,
        "the source stalled behind a transport nobody answered"
    );
    assert!(
        stops_within(&host.source, Duration::from_secs(2)),
        "the source did not stop"
    );
}

/// Hand `peer` the video offer the host has waiting and return its answer, undelivered.
fn answer_to_the_pending_video_offer(host: &mut Host, peer: &mut Peer) -> SignalMsg {
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        while let Ok(msg) = host.sig_rx.try_recv() {
            if let SignalMsg::Offer { pc, .. } | SignalMsg::Ice { pc, .. } = &msg {
                if pc.unwrap_or_default() == PcId::Video {
                    peer.video.inbound(msg);
                }
            }
        }
        // Its candidates follow the answer and stay queued for the next pump.
        if let Some(SignalMsg::Answer { sdp, .. }) = peer.video.outbound() {
            return SignalMsg::Answer {
                sdp,
                pc: Some(PcId::Video),
            };
        }
        assert!(Instant::now() < deadline, "the peer never answered");
        std::thread::sleep(Duration::from_millis(5));
    }
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

    // The page itself answers the new transport's offer: other ICE credentials than the
    // answer above, the same certificate. Its two answers reach the host together, the
    // late one first as it sent them, with no time for the first to finish applying.
    let mut b = Peer::new();
    let to_new_offer = answer_to_the_pending_video_offer(&mut host, &mut b);
    host.inbound(to_old_offer);
    host.inbound(to_new_offer);

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

/// A size step and an app swap while the page that took the session over is still
/// connecting. Neither may wait on its transport: RTP held there parks the encode chain,
/// and the renegotiation both cause queues behind it with the source's streaming thread.
#[test]
fn a_size_step_and_a_swap_do_not_wait_on_a_transport_nobody_answered() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("midway");
    let _a = first_attach(&mut host);
    abandon_a_takeover(&mut host);
    std::thread::sleep(Duration::from_millis(500));

    host.step_the_size();
    let frames = source_frames(&host);
    std::thread::sleep(Duration::from_secs(1));
    assert!(
        frames.load(Ordering::Relaxed) > 15,
        "the source stalled after a size step with nobody connected"
    );

    assert!(
        host.swap_source("midway-2"),
        "the swapped-out source did not stop with nobody connected"
    );
    let frames = source_frames(&host);
    std::thread::sleep(Duration::from_secs(1));
    assert!(
        frames.load(Ordering::Relaxed) > 15,
        "the swapped-in source is not running"
    );

    // The page arrives after all, and answers the offer that was waiting for it. (No
    // re-offer: after a size step it names another H.264 level, which a webrtcbin peer
    // refuses and a browser does not.)
    let mut b = first_attach(&mut host);
    assert!(input_reaches_host(&mut host, &mut b));
    assert_eq!(host.bus_errors, Vec::<String>::new());
}

/// The same two once the new peer is connected: nothing is left waiting on what the
/// replacement skipped.
#[test]
fn a_size_step_and_a_swap_still_work_on_a_replaced_transport() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("after");
    let _a = first_attach(&mut host);
    let mut b = attach(&mut host);

    host.step_the_size();
    let before = rtp(&b.video);
    pump(&mut host, &mut b, Duration::from_secs(1), |_, _| false);
    assert!(
        rtp(&b.video) > before + 15,
        "the stream stopped at a size step"
    );

    assert!(
        host.swap_source("after-2"),
        "the swapped-out source did not stop"
    );
    let before = rtp(&b.video);
    pump(&mut host, &mut b, Duration::from_secs(1), |_, _| false);
    assert!(rtp(&b.video) > before + 15, "the stream stopped at a swap");
    assert!(input_reaches_host(&mut host, &mut b));
    assert_eq!(host.bus_errors, Vec::<String>::new());
}

/// An answer that is not SDP is handed to nothing. It must not stand for a peer: not on a
/// transport nobody has answered, where the real first answer would then read as a second
/// peer, and not on one that is streaming, where it would cost the peer its transport.
#[test]
fn a_malformed_answer_costs_no_transport() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("malformed");
    let malformed = |sdp: &str| SignalMsg::Answer {
        sdp: sdp.to_string(),
        pc: Some(PcId::Video),
    };
    // Once the transport is waiting for its answer: an empty one then fails to parse.
    let offered = Instant::now() + Duration::from_secs(10);
    while !super::has_offered(&host.transports.video.webrtc) {
        assert!(Instant::now() < offered, "the transport never offered");
        std::thread::sleep(Duration::from_millis(10));
    }
    host.inbound(malformed(""));

    let mut a = first_attach(&mut host);
    assert_eq!(
        host.replaced,
        Vec::<PcId>::new(),
        "the first real answer was taken for a second peer"
    );

    host.inbound(malformed("not sdp"));
    let before = rtp(&a.video);
    pump(&mut host, &mut a, Duration::from_secs(1), |_, _| false);
    assert_eq!(
        host.replaced,
        Vec::<PcId>::new(),
        "a malformed answer replaced a streaming peer's transport"
    );
    assert!(rtp(&a.video) > before + 15, "the stream stopped");
    assert!(input_reaches_host(&mut host, &mut a));
}

/// Holds every replacement's attach still for a while, so the encode chain is certain to
/// push while the new transport is half built. Without it that takes a frame landing in
/// under a millisecond: about one replacement in fifty.
struct AttachPause;

impl AttachPause {
    fn of(ms: u64) -> AttachPause {
        super::transport::ATTACH_PAUSE_MS.store(ms, Ordering::Relaxed);
        AttachPause
    }
}

impl Drop for AttachPause {
    fn drop(&mut self) {
        super::transport::ATTACH_PAUSE_MS.store(0, Ordering::Relaxed);
    }
}

/// A frame pushed while a replacement transport is being attached must not stop the
/// encode chain. It did when the tail was linked to a transport whose pads were not
/// active yet: the stream ended there, and the new transport never got caps, so it never
/// made its offer either. `QUASAR_REPLACE_ROUNDS` / `QUASAR_ATTACH_PAUSE_MS` for a soak.
#[test]
fn every_replacement_transport_makes_its_offer() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let env = |name: &str| std::env::var(name).ok().and_then(|s| s.parse::<u64>().ok());
    let rounds = env("QUASAR_REPLACE_ROUNDS").unwrap_or(6) as usize;
    let _pause = AttachPause::of(env("QUASAR_ATTACH_PAUSE_MS").unwrap_or(50));
    let mut host = Host::start("offers");
    host.transports.set_replacement_limit(usize::MAX);
    let _a = first_attach(&mut host);
    let answer = last_answer(&host, PcId::Video);
    let encoded = Arc::new(AtomicU64::new(0));
    let count = encoded.clone();
    host.encoder
        .static_pad("src")
        .unwrap()
        .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
            count.fetch_add(1, Ordering::Relaxed);
            gst::PadProbeReturn::Ok
        });
    let mut silent = Vec::new();
    for round in 0..rounds {
        // Another browser each round: its own certificate and ICE credentials.
        let sdp = answer
            .replace(
                "a=fingerprint:sha-256 ",
                &format!("a=fingerprint:sha-256 {:02X}:", round % 256),
            )
            .replace("a=ice-ufrag:", &format!("a=ice-ufrag:r{round}"));
        host.inbound(SignalMsg::Answer {
            sdp: sdp.clone(),
            pc: Some(PcId::Video),
        });
        assert_eq!(
            host.replaced.len(),
            round + 1,
            "round {round} did not replace"
        );
        let before = encoded.load(Ordering::Relaxed);
        let deadline = Instant::now() + Duration::from_secs(2);
        while !super::has_offered(&host.transports.video.webrtc) && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(2));
        }
        if !super::has_offered(&host.transports.video.webrtc) {
            let sink = host.transports.video.webrtc.sink_pads();
            silent.push(format!(
                "round {round}: no offer in 2 s; encoder emitted {} buffers meanwhile; \
                 transport sink caps {:?}; state {:?}",
                encoded.load(Ordering::Relaxed) - before,
                sink.first().and_then(|p| p.current_caps()).is_some(),
                host.transports.video.webrtc.current_state(),
            ));
            break;
        }
        // The new transport's own peer answers, so the next round reads as a new one.
        host.inbound(SignalMsg::Answer {
            sdp,
            pc: Some(PcId::Video),
        });
        let applied = |h: &Host| {
            h.transports
                .video
                .webrtc
                .property::<Option<gstreamer_webrtc::WebRTCSessionDescription>>(
                    "remote-description",
                )
                .is_some()
        };
        let deadline = Instant::now() + Duration::from_secs(2);
        while !applied(&host) && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(2));
        }
        assert!(applied(&host), "round {round}: the answer never applied");
    }
    assert_eq!(silent, Vec::<String>::new());
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
    /// The runner reported `SwapDone`.
    swapped: bool,
    swap: std::sync::mpsc::Sender<crate::session::runner::SwapRequest>,
    display: std::sync::mpsc::Sender<crate::session::runner::DisplayUpdateRequest>,
}

impl Session {
    fn start(tag: &str, idle_timeout: Duration) -> Session {
        trace_to_stderr();
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
        let (display, display_rx) = std::sync::mpsc::channel();
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
            swapped: false,
            swap,
            display,
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
                    SessionEvent::SwapDone => self.swapped = true,
                    _ => {}
                }
            }
            for id in [PcId::Video, PcId::Audio] {
                while let Some(msg) = peer.pc_mut(id).outbound() {
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

/// An answer that refuses its first m-line the way a browser does that cannot decode the
/// session's codec: port 0 and no ICE attributes there. webrtcbin cannot apply it
/// ("media 0 is missing or contains an empty 'ice-ufrag' attribute").
fn refusing_its_first_m_line(answer: &str) -> String {
    let mut section = 0;
    let mut refused_mid = String::new();
    let mut out: Vec<String> = Vec::new();
    for line in answer.lines() {
        if line.starts_with("m=") {
            section += 1;
        }
        if section != 1 {
            out.push(line.to_string());
            continue;
        }
        if let Some(rest) = line.strip_prefix("m=") {
            let mut parts: Vec<&str> = rest.split(' ').collect();
            parts[1] = "0";
            out.push(format!("m={}", parts.join(" ")));
        } else if let Some(mid) = line.strip_prefix("a=mid:") {
            refused_mid = mid.trim().to_string();
            out.push(line.to_string());
        } else if !["a=ice-", "a=fingerprint:", "a=setup:", "a=candidate:"]
            .iter()
            .any(|p| line.starts_with(p))
        {
            out.push(line.to_string());
        }
    }
    let bundle = format!(" {refused_mid}");
    out.iter()
        .map(|l| match l.strip_prefix("a=group:BUNDLE") {
            Some(mids) => format!("a=group:BUNDLE{}", mids.replacen(&bundle, "", 1)),
            None => l.clone(),
        })
        .collect::<Vec<_>>()
        .join("\r\n")
        + "\r\n"
}

impl Session {
    /// A page streams. A second browser takes the session over, and its answer to the new
    /// `refused` transport's offer is one webrtcbin cannot apply.
    fn with_a_takeover_that_cannot_be_applied(tag: &str, refused: PcId) -> Session {
        let mut session = Session::start(tag, Duration::from_secs(600));
        let mut a = Peer::new();
        let up = session.pump(&mut a, Duration::from_secs(20), |_, p| streaming(p));
        assert!(up, "the first peer never streamed: {}", describe(&a));
        let as_b = |sdp: &str| with_another_certificate(with_other_ice(sdp));
        let answers = session.answers.clone();
        for pc in [PcId::Video, PcId::Audio] {
            let sdp = as_b(answers[pc as usize].as_ref().unwrap());
            let _ = session
                .to_host
                .send(SignalMsg::Answer { sdp, pc: Some(pc) });
        }
        let replaced = session.pump(&mut a, Duration::from_secs(10), |s, _| s.offers == [2, 2]);
        assert!(replaced, "offers per PC: {:?}", session.offers);

        let sdp = as_b(&refusing_its_first_m_line(
            answers[refused as usize].as_ref().unwrap(),
        ));
        let _ = session.to_host.send(SignalMsg::Answer {
            sdp,
            pc: Some(refused),
        });
        session.pump(&mut a, Duration::from_secs(2), |s, _| s.ended.is_some());
        session
    }

    /// The session is still there for a peer that can take it: it attaches, streams, and
    /// a stop then ends the session.
    fn serves_the_next_peer_and_stops(&mut self) {
        assert!(
            self.ended.is_none(),
            "an answer the host could not apply ended the session: {:?}",
            self.ended
        );
        let mut next = Peer::new();
        for pc in [PcId::Video, PcId::Audio] {
            let _ = self.to_host.send(SignalMsg::RestartIce { pc: Some(pc) });
        }
        let up = self.pump(&mut next, Duration::from_secs(20), |_, p| streaming(p));
        assert!(up, "the next peer never streamed: {}", describe(&next));

        self.stop.store(true, Ordering::SeqCst);
        let ended = self.ends_within(Duration::from_secs(10));
        assert!(
            matches!(ended, Some(SessionEvent::Stopped { .. })),
            "the session did not stop: {ended:?}"
        );
    }
}

/// A browser that cannot decode the session's codec takes it over: the takeover is lost,
/// the session is not.
#[test]
fn a_takeover_whose_video_answer_cannot_be_applied_leaves_the_session_running() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut session = Session::with_a_takeover_that_cannot_be_applied("refused-video", PcId::Video);
    session.serves_the_next_peer_and_stops();
}

#[test]
fn a_takeover_whose_audio_answer_cannot_be_applied_leaves_the_session_running() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut session = Session::with_a_takeover_that_cannot_be_applied("refused-audio", PcId::Audio);
    session.serves_the_next_peer_and_stops();
}

/// The same answer from the first peer a session ever had still fails the launch, as it
/// does on `develop`: there is no stream to keep, and the page is told why.
#[test]
fn a_first_answer_that_cannot_be_applied_still_fails_the_launch() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut session = Session::start("refused-launch", Duration::from_secs(600));
    session.mute = true;
    let mut a = Peer::new();
    let answered = session.pump(&mut a, Duration::from_secs(20), |s, _| {
        s.answers[PcId::Video as usize].is_some()
    });
    assert!(answered, "the first peer never answered");
    let sdp = refusing_its_first_m_line(session.answers[PcId::Video as usize].as_ref().unwrap());
    let _ = session.to_host.send(SignalMsg::Answer {
        sdp,
        pc: Some(PcId::Video),
    });

    let ended = session.ends_within(Duration::from_secs(10));
    assert!(
        matches!(&ended, Some(SessionEvent::Failed(reason)) if reason.contains("rejected the remote answer")),
        "the launch did not fail: {ended:?}"
    );
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

impl Session {
    /// A size step, then an app swap, then a stop, with whatever the transports are
    /// waiting for still outstanding. The size step makes the source renegotiate; the
    /// swap re-points the encoder on the runner's own thread. If a transport holds RTP,
    /// the first waits behind it and the second waits on the first: no `SwapDone`, and a
    /// runner that never sees the stop.
    fn resize_swap_and_stop(&mut self) {
        use crate::session::runner::{DisplayUpdateRequest, SwapRequest};
        self.mute = true;
        let mut nobody = Peer::new();
        let step = DisplayUpdateRequest {
            stream: Some((160, 120)),
            ..Default::default()
        };
        self.display.send(step).expect("display lane");
        self.pump(&mut nobody, Duration::from_secs(1), |_, _| false);

        self.swap
            .send(SwapRequest { container: None })
            .expect("swap lane");
        let swapped = self.pump(&mut nobody, Duration::from_secs(10), |s, _| s.swapped);
        assert!(swapped, "the swap never completed");

        self.stop.store(true, Ordering::SeqCst);
        let ended = self.ends_within(Duration::from_secs(10));
        assert!(
            matches!(ended, Some(SessionEvent::Stopped { .. })),
            "the session did not stop: {ended:?}"
        );
    }
}

/// One replacement per PC, nobody answering the new transports, is inside the budget.
#[test]
fn a_resize_and_a_swap_during_an_abandoned_takeover_leave_the_runner_free() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut session = Session::with_an_abandoned_takeover("resize-swap", Duration::from_secs(600));
    session.resize_swap_and_stop();
}

/// The same on the session's first transport, before anybody answered at all.
#[test]
fn a_resize_and_a_swap_before_anyone_answered_leave_the_runner_free() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut session = Session::start("launch-resize-swap", Duration::from_secs(600));
    session.mute = true;
    let mut nobody = Peer::new();
    let offered = session.pump(&mut nobody, Duration::from_secs(20), |s, _| {
        s.offers == [1, 1]
    });
    assert!(offered, "offers per PC: {:?}", session.offers);
    session.pump(&mut nobody, Duration::from_millis(800), |_, _| false);
    session.resize_swap_and_stop();
}

/// A frame-rate step alone, with the fps rung on, renegotiates like a size step.
#[test]
fn an_fps_step_and_a_swap_do_not_wait_on_a_transport_nobody_answered() {
    if !live_webrtc_available() {
        return;
    }
    use super::ResolutionLever;
    let _serial = serial();
    let mut host = Host::start_cfg("fps", host_cfg_with(false, true));
    let _a = first_attach(&mut host);
    abandon_a_takeover(&mut host);
    std::thread::sleep(Duration::from_millis(500));

    assert_eq!(
        host.lever.set_fps(15).ok(),
        Some(true),
        "this rig has no frame-rate lever"
    );
    let frames = source_frames(&host);
    std::thread::sleep(Duration::from_secs(1));
    assert!(
        frames.load(Ordering::Relaxed) > 15,
        "the source stalled after a frame-rate step with nobody connected"
    );
    assert!(
        host.swap_source("fps-2"),
        "the swapped-out source did not stop with nobody connected"
    );
}

/// RTP is dropped while a replacement waits for its peer, mid-GOP as it comes. What the
/// peer then receives must start at a keyframe: the rest of the GOP it has none of would
/// reach it undecodable. The encoder here acts on the keyframe request 300 ms late, as
/// one with frames in flight does, so frames of the old GOP are there to be sent.
#[test]
fn the_first_frame_a_new_peer_receives_is_a_keyframe() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("first-frame");
    let a = first_attach(&mut host);
    let first = *a.video.first_nal.lock().unwrap();
    assert!(
        matches!(first, Some(5 | 7 | 8)),
        "the first peer's first video packet starts with NAL type {first:?}"
    );
    let lost = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let (lose, again) = (lost.clone(), host.encoder.clone());
    host.encoder.static_pad("src").unwrap().add_probe(
        gst::PadProbeType::EVENT_UPSTREAM,
        move |_, info| {
            let request = info
                .event()
                .is_some_and(|e| e.structure().is_some_and(|s| s.name() == "GstForceKeyUnit"));
            if request && !lose.swap(true, Ordering::SeqCst) {
                return gst::PadProbeReturn::Drop;
            }
            gst::PadProbeReturn::Ok
        },
    );
    std::thread::spawn(move || {
        while !lost.load(Ordering::SeqCst) {
            std::thread::sleep(Duration::from_millis(5));
        }
        std::thread::sleep(Duration::from_millis(300));
        super::resize::force_idr(&again);
    });

    let b = attach(&mut host);

    let nal = *b.video.first_nal.lock().unwrap();
    // 7 and 8 are the parameter sets a keyframe leads with, 5 an IDR slice; 1 is a
    // slice of a frame that depends on earlier ones.
    assert!(
        matches!(nal, Some(5 | 7 | 8)),
        "the new peer's first video packet starts with NAL type {nal:?}"
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
        // Past its budget for this long after the second page attaches: that page's
        // answers are ignored, then the window clears.
        let refuse_secs: Option<u64> = std::env::var("QUASAR_TAKEOVER_REFUSE_SECS")
            .ok()
            .and_then(|s| s.parse().ok());
        let mut refuse_until: Option<tokio::time::Instant> = None;
        let mut host = Host::start_with("browser", mic);
        if refuse_secs.is_some() {
            host.transports.set_replacement_limit(0);
        }
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
                    if let (2, Some(secs)) = (id, refuse_secs) {
                        refuse_until = Some(tokio::time::Instant::now() + Duration::from_secs(secs));
                    }
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
                    if refuse_until.is_some_and(|at| tokio::time::Instant::now() >= at) {
                        refuse_until = None;
                        host.transports.set_replacement_limit(6);
                        host.transports.forget_replacements();
                        eprintln!("HOST the replacement window cleared");
                    }
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
                            // One more offer either way: this request yields it if the
                            // new transport has already offered, else it is absorbed and
                            // the request after that offer does.
                            restart_after_offer =
                                !super::has_offered(&host.transports.video.webrtc);
                            host.inbound(SignalMsg::RestartIce { pc: Some(pc) });
                        }
                        if late_answer && pc == PcId::Video {
                            let answer = SignalMsg::Answer {
                                sdp: last_answer(&host, pc),
                                pc: Some(pc),
                            };
                            // Once the new transport has offered. If that offer is out
                            // already, now: held for the next one, it would land after
                            // the page had connected.
                            if super::has_offered(&host.transports.video.webrtc) {
                                eprintln!(
                                    "HOST replaying the answer that replaced the video transport"
                                );
                                host.inbound(answer);
                            } else {
                                late = Some(answer);
                            }
                        }
                    }
                }
            }
        }
    });
}

/// What the host hands `webrtcbin` per transport, and whether it is given back.
mod aux_senders;
