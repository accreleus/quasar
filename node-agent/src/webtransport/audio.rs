//! A session's audio and microphone for its WebTransport client
//! (`protocol/transport-webtransport.md` §Audio, §Microphone).
//!
//! Audio is the WebRTC audio pipeline's own Opus encode, tapped at the encoder's src pad:
//! the same capture, 10 ms frames and low-delay settings (`audio_branch::add_opus_encode`),
//! with the transport slot after the encoder as for video. The microphone is bare Opus
//! packets into the WebRTC mic's decode chain and sink (`mic_branch::build_stream_mic`).

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use bytes::{BufMut, Bytes, BytesMut};
use gstreamer as gst;
use gstreamer::prelude::*;
use serde_json::{json, Value};
use tokio::sync::mpsc;

use super::mic_jitter::{Jitter, Out};
use crate::session::pipeline::mic_branch;
use crate::session::SessionConfig;

pub(crate) const CHANNEL_AUDIO: u8 = 0x02;
pub(crate) const CHANNEL_MIC: u8 = 0x05;
/// The copy of the previous audio packet (§Audio redundancy, amendment 67).
pub(crate) const CHANNEL_AUDIO_RED: u8 = 0x06;
pub(crate) const AUDIO_HEADER: usize = 19;
pub(crate) const MIC_HEADER: usize = 7;
/// Packets queued for the client before new ones are dropped: 160 ms of 10 ms packets.
const CLIENT_QUEUE: usize = 16;
/// `ts_48khz` ticks in one 10 ms packet.
const PACKET_TICKS: u32 = 480;
/// The most a packet is believed to differ from the clock when the encoder outputs it.
const MAX_AGE_US: i64 = 1_000_000;
/// The longest gap between two packets across which the earlier one is still worth repeating
/// (§Audio redundancy): past it the stream paused, and the copy would arrive where its own
/// packet's time has already gone.
pub(crate) const REPEAT_MAX_GAP_US: u64 = 100_000;
/// The longest Opus packet (RFC 6716).
const MAX_OPUS_PACKET: usize = 1275;

/// What a session offers its WebTransport client.
#[derive(Clone, Debug, Default)]
pub(crate) struct Plan {
    pub(crate) audio: bool,
    pub(crate) mic: bool,
    pub(crate) pulse_server: Option<String>,
    /// The microphone's jitter buffer depth: `QUASAR_MIC_JITTER_MS`.
    pub(crate) jitter_ms: u32,
}

impl Plan {
    /// The microphone, like WebRTC's, needs stream audio on and a grant for the session.
    pub(crate) fn for_session(cfg: &SessionConfig) -> Self {
        let audio = !crate::session::pipeline::audio_disabled();
        Self {
            audio,
            mic: audio && cfg.mic_enabled(),
            pulse_server: cfg.pulse_server.clone(),
            jitter_ms: cfg.mic_jitter_ms,
        }
    }
}

/// One audio packet as the encoder made it, before `pkt_seq` is assigned at send.
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct Packet {
    pub(crate) data: Bytes,
    pub(crate) packet_seq: u16,
    pub(crate) ts_48khz: u32,
    pub(crate) capture_us: u64,
}

/// The `0x02` datagram (§Audio).
pub(crate) fn datagram(p: &Packet, pkt_seq: u32) -> Bytes {
    onto(CHANNEL_AUDIO, p, pkt_seq)
}

/// The `0x06` copy of `p`, for a client that takes channel `0x06` (§Audio redundancy): the same
/// bytes as its `0x02` datagram but for the channel, with a `pkt_seq` of its own.
pub(crate) fn repeated(p: &Packet, pkt_seq: u32) -> Bytes {
    onto(CHANNEL_AUDIO_RED, p, pkt_seq)
}

fn onto(channel: u8, p: &Packet, pkt_seq: u32) -> Bytes {
    let mut d = BytesMut::with_capacity(AUDIO_HEADER + p.data.len());
    d.put_u8(channel);
    d.put_u32_le(pkt_seq);
    d.put_u16_le(p.packet_seq);
    d.put_u32_le(p.ts_48khz);
    d.put_u64_le(p.capture_us);
    d.put_slice(&p.data);
    d.freeze()
}

/// Whether `next`'s datagram repeats `prev`: false across a pause (§Audio redundancy).
pub(crate) fn repeats(prev: &Packet, next: &Packet) -> bool {
    next.capture_us.saturating_sub(prev.capture_us) <= REPEAT_MAX_GAP_US
}

fn wall_us() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::SystemTime::UNIX_EPOCH)
        .map_or(0, |d| d.as_micros() as u64)
}

#[derive(Default)]
struct State {
    /// The attached client's id (`Media`'s) and its queue.
    client: Option<(u64, mpsc::Sender<Packet>)>,
    packet_seq: u16,
    last_ts: u32,
}

/// The session's audio hub: the Opus tap feeds it, the attached client's connection task
/// drains it, and the client's microphone packets go to the mic sink.
pub(crate) struct Audio {
    plan: Plan,
    state: Mutex<State>,
    mic: Option<Arc<Mic>>,
}

impl Audio {
    pub(crate) fn new(plan: Plan) -> Arc<Self> {
        let mic = plan
            .mic
            .then(|| Mic::new(plan.pulse_server.clone(), plan.jitter_ms));
        Arc::new(Self {
            plan,
            state: Mutex::default(),
            mic,
        })
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// `hello_ack`'s `audio` and `mic` (§Hello ack): `null` for what the session lacks.
    pub(crate) fn describe(&self) -> (Value, Value) {
        let stream = |channels| json!({"codec": "opus", "channels": channels, "sample_rate": 48000, "ptime_ms": 10});
        (
            if self.plan.audio {
                stream(2)
            } else {
                Value::Null
            },
            if self.mic.is_some() {
                stream(1)
            } else {
                Value::Null
            },
        )
    }

    /// `client` is the one connected now: it gets the audio from here on, and the mic starts.
    pub(crate) fn attach(&self, client: u64) -> mpsc::Receiver<Packet> {
        let (tx, rx) = mpsc::channel(CLIENT_QUEUE);
        self.lock().client = Some((client, tx));
        if let Some(mic) = &self.mic {
            mic.attach();
        }
        rx
    }

    pub(crate) fn detach(&self, client: u64) {
        let mut st = self.lock();
        if st.client.as_ref().is_some_and(|(id, _)| *id == client) {
            st.client = None;
        }
    }

    /// The session is over: nobody is sent audio and the mic sink goes.
    pub(crate) fn end(&self) {
        self.lock().client = None;
        if let Some(mic) = &self.mic {
            mic.stop();
        }
    }

    /// Tap `opus`'s src pad: each packet it makes goes to the attached client. Its capture
    /// time is the wall clock less how long the packet has been in the pipeline since its
    /// PTS, the instant its 10 ms began (the pulse source stamps capture time).
    pub(crate) fn tap(self: &Arc<Self>, opus: &gst::Element) {
        let Some(pad) = opus.static_pad("src") else {
            return;
        };
        let audio = self.clone();
        pad.add_probe(gst::PadProbeType::BUFFER, move |pad, info| {
            if let Some(gst::PadProbeData::Buffer(buffer)) = &info.data {
                if !buffer.flags().contains(gst::BufferFlags::HEADER) {
                    // Signed: the encoder stamps a packet a few ms ahead of the clock (its
                    // look-ahead), and clamping that to zero would bend the 10 ms steps.
                    let age_us = pad
                        .parent_element()
                        .and_then(|e| e.current_running_time())
                        .zip(buffer.pts())
                        .map_or(0, |(now, pts)| {
                            (i128::from(now.nseconds()) - i128::from(pts.nseconds())) as i64 / 1000
                        });
                    if let Ok(map) = buffer.map_readable() {
                        audio.on_packet(Bytes::copy_from_slice(&map), buffer.pts(), age_us);
                    }
                }
            }
            gst::PadProbeReturn::Ok
        });
    }

    pub(crate) fn on_packet(&self, data: Bytes, pts: Option<gst::ClockTime>, age_us: i64) {
        let mut st = self.lock();
        let ts_48khz = pts.map_or(st.last_ts.wrapping_add(PACKET_TICKS), |p| {
            (p.nseconds() as u128 * 48_000 / 1_000_000_000) as u32
        });
        st.last_ts = ts_48khz;
        let packet_seq = st.packet_seq;
        st.packet_seq = packet_seq.wrapping_add(1);
        let Some((_, tx)) = st.client.as_ref() else {
            return;
        };
        let packet = Packet {
            data,
            packet_seq,
            ts_48khz,
            capture_us: wall_us().saturating_add_signed(-age_us.clamp(-MAX_AGE_US, MAX_AGE_US)),
        };
        // A client behind by 160 ms is not sent the backlog: realtime audio is dropped, not queued.
        if let Err(mpsc::error::TrySendError::Closed(_)) = tx.try_send(packet) {
            st.client = None;
        }
    }

    /// A `0x05` datagram's body (everything after the channel byte) from `client`.
    pub(crate) fn mic_packet(&self, client: u64, body: &[u8]) {
        let Some(mic) = &self.mic else { return };
        if self
            .lock()
            .client
            .as_ref()
            .is_none_or(|(id, _)| *id != client)
        {
            return;
        }
        mic.push(body);
    }

    #[cfg(test)]
    pub(crate) fn mic_delivered(&self) -> u64 {
        self.mic
            .as_ref()
            .map_or(0, |m| m.delivered.load(Ordering::SeqCst))
    }
}

enum MicState {
    Idle,
    Starting,
    Running {
        pipeline: gst::Pipeline,
        src: gstreamer_app::AppSrc,
        decoder: gst::Element,
    },
    Stopped,
}

/// One 10 ms packet, which is also the pacer's tick.
const MIC_TICK: std::time::Duration = std::time::Duration::from_millis(10);

/// The microphone sink of one session, built when its first client connects.
struct Mic {
    pulse_server: Option<String>,
    state: Mutex<MicState>,
    jitter: Mutex<Jitter>,
    /// Decoded buffers that reached the sink.
    delivered: Arc<AtomicU64>,
}

impl Mic {
    fn new(pulse_server: Option<String>, jitter_ms: u32) -> Arc<Self> {
        Arc::new(Self {
            pulse_server,
            state: Mutex::new(MicState::Idle),
            jitter: Mutex::new(Jitter::new(jitter_ms)),
            delivered: Arc::default(),
        })
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, MicState> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn jitter(&self) -> std::sync::MutexGuard<'_, Jitter> {
        self.jitter.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// A client attached. Its numbering starts where it likes, so nothing of the last
    /// client's stays in the buffer; the sink is built the first time.
    fn attach(self: &Arc<Self>) {
        self.jitter().reset();
        self.start();
    }

    /// Build the sink on a thread of its own: the sound server can take long to answer,
    /// and this runs on the connection task. Packets arriving meanwhile are dropped.
    fn start(self: &Arc<Self>) {
        {
            let mut st = self.lock();
            if !matches!(*st, MicState::Idle) {
                return;
            }
            *st = MicState::Starting;
        }
        let mic = self.clone();
        let log_span = tracing::Span::current();
        let spawned = std::thread::Builder::new()
            .name("quasar-mic-strt".into())
            .spawn(move || {
                let _log_span = log_span.enter();
                mic.build();
            });
        if spawned.is_err() {
            *self.lock() = MicState::Idle;
        }
    }

    fn build(self: &Arc<Self>) {
        let built =
            mic_branch::build_stream_mic(self.pulse_server.as_deref()).and_then(|(p, src)| {
                let delivered = self.delivered.clone();
                if let Some(sink_pad) = p
                    .by_name(mic_branch::MIC_SINK_NAME)
                    .and_then(|s| s.static_pad("sink"))
                {
                    sink_pad.add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                        delivered.fetch_add(1, Ordering::SeqCst);
                        gst::PadProbeReturn::Ok
                    });
                }
                let decoder = p
                    .iterate_recurse()
                    .into_iter()
                    .filter_map(Result::ok)
                    .find(|e| e.factory().is_some_and(|f| f.name() == "opusdec"))
                    .ok_or_else(|| anyhow::anyhow!("no opusdec in the microphone chain"))?;
                p.set_state(gst::State::Playing)?;
                Ok((p, src, decoder))
            });
        let mut st = self.lock();
        match (built, &*st) {
            (Ok((pipeline, src, decoder)), MicState::Starting) => {
                mic_branch::verify_stream_mic(&pipeline);
                *st = MicState::Running {
                    pipeline,
                    src,
                    decoder,
                };
                drop(st);
                let mic = self.clone();
                let log_span = tracing::Span::current();
                let _ = std::thread::Builder::new()
                    .name("quasar-mic-pace".into())
                    .spawn(move || {
                        let _log_span = log_span.enter();
                        mic.pace();
                    });
            }
            (Ok((pipeline, _, _)), _) => {
                drop(st);
                let _ = pipeline.set_state(gst::State::Null);
            }
            (Err(error), _) => {
                tracing::warn!(
                    token = "webtransport-mic-failed",
                    "the WebTransport microphone did not start; its audio is discarded: {error:#}"
                );
                *st = MicState::Stopped;
            }
        }
    }

    /// Every 10 ms the jitter buffer gives one packet to the decoder, or tells it one is lost.
    /// Pushing at an even rate is what keeps a sink with no clock of its own from crackling.
    fn pace(&self) {
        let mut at = std::time::Instant::now();
        loop {
            at += MIC_TICK;
            std::thread::sleep(at.saturating_duration_since(std::time::Instant::now()));
            let out = self.jitter().tick(std::time::Instant::now());
            let (src, decoder) = match &*self.lock() {
                MicState::Running { src, decoder, .. } => (src.clone(), decoder.clone()),
                _ => return,
            };
            match out {
                Out::Packet(opus) => {
                    let _ = src.push_buffer(gst::Buffer::from_slice(opus));
                }
                // Opus conceals a lost packet when the decoder is told 10 ms are missing. Sent
                // into the decoder's own sink pad, so it is ordered with the buffers.
                Out::Lost => {
                    let gap =
                        gst::event::Gap::builder(src.current_running_time().unwrap_or_default())
                            .duration(gst::ClockTime::from_mseconds(10))
                            .build();
                    if let Some(pad) = decoder.static_pad("sink") {
                        pad.send_event(gap);
                    }
                }
                Out::Wait => {}
            }
        }
    }

    /// §Microphone: `u16 packet_seq, u32 ts_48khz, Opus`. The jitter buffer orders them.
    fn push(&self, body: &[u8]) {
        let Some(opus) = body.get(MIC_HEADER - 1..) else {
            return;
        };
        if opus.is_empty() || opus.len() > MAX_OPUS_PACKET {
            return;
        }
        if !matches!(*self.lock(), MicState::Running { .. }) {
            return;
        }
        let seq = u16::from_le_bytes([body[0], body[1]]);
        self.jitter()
            .push(seq, Bytes::copy_from_slice(opus), std::time::Instant::now());
    }

    fn stop(&self) {
        let old = std::mem::replace(&mut *self.lock(), MicState::Stopped);
        if let MicState::Running { pipeline, .. } = old {
            // The sound server may be gone or slow: NULL on its own thread, never the caller's.
            let _ = std::thread::Builder::new()
                .name("quasar-mic-stop".into())
                .spawn(move || {
                    let _ = pipeline.set_state(gst::State::Null);
                });
        }
    }
}

#[cfg(test)]
pub(crate) fn test_opus_packets(n: usize) -> Vec<Vec<u8>> {
    gst::init().unwrap();
    let pipeline = gst::parse::launch(&format!(
            "audiotestsrc num-buffers={n} samplesperbuffer=480 wave=sine ! \
             audio/x-raw,rate=48000,channels=1 ! opusenc frame-size=10 ! appsink name=out sync=false"
        ))
        .unwrap()
        .downcast::<gst::Pipeline>()
        .unwrap();
    let sink = pipeline
        .by_name("out")
        .unwrap()
        .downcast::<gstreamer_app::AppSink>()
        .unwrap();
    pipeline.set_state(gst::State::Playing).unwrap();
    let mut out = Vec::new();
    while let Ok(sample) = sink.pull_sample() {
        let buffer = sample.buffer().unwrap();
        if !buffer.flags().contains(gst::BufferFlags::HEADER) {
            out.push(buffer.map_readable().unwrap().to_vec());
        }
    }
    pipeline.set_state(gst::State::Null).unwrap();
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn mic_body(seq: u16, opus: &[u8]) -> Vec<u8> {
        let mut b = seq.to_le_bytes().to_vec();
        b.extend_from_slice(&(u32::from(seq) * 480).to_le_bytes());
        b.extend_from_slice(opus);
        b
    }

    #[test]
    fn the_audio_datagram_is_the_19_byte_header_and_the_opus_packet() {
        let p = Packet {
            data: Bytes::from_static(&[9, 8, 7]),
            packet_seq: 0xfffe,
            ts_48khz: 0x0102_0304,
            capture_us: 0x1122_3344_5566_7788,
        };
        let d = datagram(&p, u32::MAX);
        assert_eq!(d.len(), AUDIO_HEADER + 3);
        assert_eq!(d[0], 0x02);
        assert_eq!(&d[1..5], &u32::MAX.to_le_bytes());
        assert_eq!(&d[5..7], &0xfffe_u16.to_le_bytes());
        assert_eq!(&d[7..11], &0x0102_0304_u32.to_le_bytes());
        assert_eq!(&d[11..19], &0x1122_3344_5566_7788_u64.to_le_bytes());
        assert_eq!(&d[19..], &[9, 8, 7]);
    }

    /// §Audio redundancy: the copy is the packet, its own `pkt_seq`, and channel `0x06`.
    #[test]
    fn the_redundant_datagram_is_the_same_packet_on_its_own_channel_and_pkt_seq() {
        let p = Packet {
            data: Bytes::from_static(&[9, 8, 7]),
            packet_seq: 0xfffe,
            ts_48khz: 0x0102_0304,
            capture_us: 0x1122_3344_5566_7788,
        };
        let original = datagram(&p, 41);
        let copy = repeated(&p, 40);
        assert_eq!(copy.len(), original.len());
        assert_eq!(copy[0], 0x06);
        assert_eq!(&copy[1..5], &40u32.to_le_bytes());
        assert_eq!(
            &copy[5..],
            &original[5..],
            "packet_seq, ts, capture_us and the Opus packet are the repeated packet's"
        );
    }

    #[test]
    fn a_packet_is_not_repeated_across_a_pause_of_more_than_100_ms() {
        let at = |capture_us| Packet {
            data: Bytes::from_static(&[1]),
            packet_seq: 0,
            ts_48khz: 0,
            capture_us,
        };
        let (first, next) = (at(1_000_000), at(1_010_000));
        assert!(repeats(&first, &next), "10 ms apart: repeated");
        assert!(
            repeats(&first, &at(1_100_000)),
            "exactly 100 ms apart is still repeated"
        );
        assert!(
            !repeats(&first, &at(1_100_001)),
            "a pause over 100 ms is not: its copy would be stale"
        );
    }

    #[test]
    fn packets_go_to_the_attached_client_numbered_and_stamped() {
        let audio = Audio::new(Plan {
            audio: true,
            ..Plan::default()
        });
        audio.on_packet(Bytes::from_static(b"before"), None, 0);
        let mut rx = audio.attach(1);
        let pts = |ms| Some(gst::ClockTime::from_mseconds(ms));
        audio.on_packet(Bytes::from_static(b"a"), pts(1000), 25_000);
        audio.on_packet(Bytes::from_static(b"b"), pts(1010), 0);
        let (a, b) = (rx.try_recv().unwrap(), rx.try_recv().unwrap());
        assert_eq!(
            (a.packet_seq, b.packet_seq),
            (1, 2),
            "numbered across the session, not the client"
        );
        assert_eq!(
            (a.ts_48khz, b.ts_48khz),
            (48_000, 48_480),
            "48 kHz from the PTS"
        );
        assert!(
            b.capture_us - a.capture_us >= 25_000,
            "a's 25 ms age is taken off the wall clock"
        );
        // Signed: a packet stamped ahead of the clock is captured "after" now.
        audio.on_packet(Bytes::from_static(b"c"), None, -5_000);
        assert!(rx.try_recv().unwrap().capture_us > wall_us());
        // A client behind by more than the queue is not sent the backlog.
        for _ in 0..CLIENT_QUEUE + 5 {
            audio.on_packet(Bytes::from_static(b"x"), None, 0);
        }
        let mut queued = 0;
        while rx.try_recv().is_ok() {
            queued += 1;
        }
        assert_eq!(queued, CLIENT_QUEUE);
        while rx.try_recv().is_ok() {}
        audio.detach(2);
        audio.on_packet(Bytes::from_static(b"x"), None, 0);
        assert!(rx.try_recv().is_ok(), "a stale detach changes nothing");
        audio.detach(1);
        audio.on_packet(Bytes::from_static(b"x"), None, 0);
        assert!(rx.try_recv().is_err(), "a detached client gets nothing");
    }

    #[test]
    fn hello_ack_names_only_what_the_session_has() {
        let none = Audio::new(Plan::default()).describe();
        assert_eq!(none, (Value::Null, Value::Null));
        let both = Audio::new(Plan {
            audio: true,
            mic: true,
            pulse_server: None,
            jitter_ms: 40,
        })
        .describe();
        assert_eq!(both.0["channels"], 2);
        assert_eq!(
            both.1,
            json!({"codec": "opus", "channels": 1, "sample_rate": 48000, "ptime_ms": 10})
        );
    }

    #[test]
    fn mic_packets_from_the_attached_client_are_decoded_into_the_sink() {
        let packets = test_opus_packets(30);
        assert!(packets.len() >= 20, "{} packets", packets.len());
        let audio = Audio::new(Plan {
            audio: true,
            mic: true,
            pulse_server: None,
            jitter_ms: 40,
        });
        let _rx = audio.attach(7);
        let mic = audio.mic.clone().unwrap();
        wait_running(&mic);
        // Not the attached client; a bare header; an empty packet; a packet behind: none count.
        audio.mic_packet(8, &mic_body(0, &packets[0]));
        audio.mic_packet(7, &[1, 0, 0]);
        audio.mic_packet(7, &mic_body(0, &[]));
        assert_eq!(audio.mic_delivered(), 0);
        for (i, p) in packets.iter().enumerate().take(20) {
            audio.mic_packet(7, &mic_body(10 + i as u16, p));
            audio.mic_packet(7, &mic_body(10 + i as u16, p));
            std::thread::sleep(std::time::Duration::from_millis(10));
        }
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        while audio.mic_delivered() < 15 {
            assert!(
                std::time::Instant::now() < deadline,
                "only {} decoded buffers reached the sink",
                audio.mic_delivered()
            );
            std::thread::sleep(std::time::Duration::from_millis(20));
        }
        audio.end();
        assert!(matches!(*mic.lock(), MicState::Stopped));
    }

    fn wait_running(mic: &Mic) {
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
        while !matches!(*mic.lock(), MicState::Running { .. }) {
            assert!(
                std::time::Instant::now() < deadline,
                "the mic sink never started"
            );
            std::thread::sleep(std::time::Duration::from_millis(20));
        }
    }

    /// Send packets `from..from + n` of `packets` (cycled) as `client`, 10 ms apart.
    fn send(audio: &Audio, client: u64, packets: &[Vec<u8>], from: u16, n: u16) {
        for i in 0..n {
            audio.mic_packet(
                client,
                &mic_body(from + i, &packets[usize::from(i) % packets.len()]),
            );
            std::thread::sleep(std::time::Duration::from_millis(10));
        }
    }

    /// The delivered count once it has stopped moving for 200 ms (at most 5 s).
    fn settled_delivered(audio: &Audio) -> u64 {
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        let mut last = audio.mic_delivered();
        loop {
            std::thread::sleep(std::time::Duration::from_millis(200));
            let now = audio.mic_delivered();
            if now == last {
                return now;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "the delivered count never settled"
            );
            last = now;
        }
    }

    fn wait_delivered(audio: &Audio, at_least: u64) {
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        while audio.mic_delivered() < at_least {
            assert!(
                std::time::Instant::now() < deadline,
                "only {} of {at_least} decoded buffers reached the sink",
                audio.mic_delivered()
            );
            std::thread::sleep(std::time::Duration::from_millis(20));
        }
    }

    #[test]
    fn a_client_that_takes_over_numbers_from_zero_and_is_heard_from_its_first_packet() {
        let packets = test_opus_packets(30);
        let audio = Audio::new(Plan {
            audio: true,
            mic: true,
            pulse_server: None,
            jitter_ms: 40,
        });
        let _a = audio.attach(7);
        wait_running(audio.mic.as_ref().unwrap());
        send(&audio, 7, &packets, 1_000, 25);
        wait_delivered(&audio, 20);
        let before = audio.mic_delivered();
        // A reload, or a second device: a new client counting from 0, straight away.
        let _b = audio.attach(8);
        send(&audio, 8, &packets, 0, 12);
        wait_delivered(&audio, before + 11);
        // Read the count only once client 8's last packet (and any concealment after it) has
        // drained: reading it mid-flight made this test flaky.
        let after = settled_delivered(&audio);
        // The first client is no longer the one heard.
        send(&audio, 7, &packets, 1_025, 5);
        std::thread::sleep(std::time::Duration::from_millis(150));
        assert_eq!(audio.mic_delivered(), after);
    }

    #[test]
    fn a_lost_microphone_packet_is_concealed_not_skipped() {
        let packets = test_opus_packets(30);
        let audio = Audio::new(Plan {
            audio: true,
            mic: true,
            pulse_server: None,
            jitter_ms: 40,
        });
        let _rx = audio.attach(7);
        wait_running(audio.mic.as_ref().unwrap());
        for seq in (0u16..12).filter(|s| *s != 5) {
            audio.mic_packet(7, &mic_body(seq, &packets[usize::from(seq)]));
            std::thread::sleep(std::time::Duration::from_millis(10));
        }
        // 11 packets arrived; the decoder was told about the 12th and concealed it.
        wait_delivered(&audio, 12);
    }
}
