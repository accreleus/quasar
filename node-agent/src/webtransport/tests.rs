//! The conformance client: drives the listener as a browser would (pinning the
//! certificate by hash, a control stream, datagrams) and checks what arrives against
//! `protocol/transport-webtransport.md`.

use std::cell::RefCell;
use std::collections::{BTreeMap, HashMap};

use bytes::Bytes;
use gstreamer::prelude::*;
use web_transport_quinn::{ClientBuilder, SessionError, WebTransportError};

use super::*;

use time::format_description::well_known::Rfc3339;
use time::OffsetDateTime;

const HOUR: Duration = Duration::from_secs(3600);

const SESSION: &str = "0d6c1f0e-4f39-4c55-9b1a-5d2f6b8c7a10";
const TOKEN: &str = "first-transport-token";

fn hello_for(session_id: &str, token: &str, wt_proto: u32) -> Hello {
    Hello {
        kind: "hello".into(),
        wt_proto,
        token: token.into(),
        session_id: session_id.into(),
        pyrowave: false,
        fec: Value::Null,
    }
}

/// An input seam that records what reaches it.
fn recorded() -> (
    Input,
    Arc<Mutex<Vec<String>>>,
    Arc<std::sync::atomic::AtomicUsize>,
) {
    let events = Arc::new(Mutex::new(Vec::new()));
    let released = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let (seen, lifted) = (events.clone(), released.clone());
    let input = Input {
        event: Arc::new(move |json| seen.lock().unwrap().push(json.to_string())),
        release_all: Arc::new(move || {
            lifted.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        }),
    };
    (input, events, released)
}

pub(super) fn no_input() -> Input {
    recorded().0
}

fn ready_media() -> Arc<Media> {
    ready_media_with(no_input())
}

fn ready_media_with(input: Input) -> Arc<Media> {
    gst::init().unwrap();
    let encoder = gst::ElementFactory::make("identity").build().unwrap();
    let media = Media::new(&encoder, input, Audio::new(audio::Plan::default()), None);
    let caps = gst::Caps::builder("video/x-h264")
        .field("stream-format", "avc")
        .field("width", 64)
        .field("height", 64)
        .field(
            "codec_data",
            gst::Buffer::from_slice(vec![1u8, 0x42, 0, 0x1f, 0xff]),
        )
        .build();
    media.on_caps(&caps, SESSION);
    media
}

#[test]
fn a_token_is_single_use_lives_its_expiry_plus_15_s_and_never_crosses() {
    let registry = Registry::default();
    let now = SystemTime::now();
    let at = |s: u64| now + Duration::from_secs(s);
    registry.assign(SESSION, &token_hash(TOKEN), now);

    assert_eq!(
        registry
            .redeem(&hello_for("other", TOKEN, 1), now, Duration::ZERO)
            .err(),
        Some(Refusal::NotReady),
        "a session this agent does not hold yet"
    );
    assert_eq!(
        registry
            .redeem(&hello_for(SESSION, TOKEN, 1), now, Duration::ZERO)
            .err(),
        Some(Refusal::NotReady),
        "no encode chain yet"
    );
    assert_eq!(
        registry
            .redeem(&hello_for(SESSION, TOKEN, 0), now, Duration::ZERO)
            .err(),
        Some(Refusal::UnsupportedVersion)
    );
    assert!(registry.set_media(SESSION, ready_media()));
    // Kind separation: a signaling token's hash never reaches the agent.
    assert_eq!(
        registry
            .redeem(
                &hello_for(SESSION, "a-signaling-token", 1),
                now,
                Duration::ZERO
            )
            .err(),
        Some(Refusal::AuthFailed)
    );
    // The token refused as not ready was kept, and is good until expiry + 15 s.
    assert!(registry
        .redeem(&hello_for(SESSION, TOKEN, 2), at(15), Duration::ZERO)
        .is_ok());
    assert_eq!(
        registry
            .redeem(&hello_for(SESSION, TOKEN, 1), at(15), Duration::ZERO)
            .err(),
        Some(Refusal::AuthFailed),
        "consumed"
    );

    assert!(registry.add_token(SESSION, &token_hash("second"), now));
    assert_eq!(
        registry
            .redeem(&hello_for(SESSION, "second", 1), at(16), Duration::ZERO)
            .err(),
        Some(Refusal::AuthFailed),
        "expired past the skew"
    );
    assert!(!registry.add_token("other", &token_hash("x"), now));

    registry.assign("b", &token_hash("token-of-b"), at(60));
    assert!(registry.set_media("b", ready_media()));
    assert_eq!(
        registry
            .redeem(&hello_for(SESSION, "token-of-b", 1), now, Duration::ZERO)
            .err(),
        Some(Refusal::AuthFailed),
        "another session's token"
    );

    registry.end("b");
    assert_eq!(
        registry
            .redeem(&hello_for("b", "token-of-b", 1), now, Duration::ZERO)
            .err(),
        Some(Refusal::SessionEnded)
    );
    assert!(!registry.add_token("b", &token_hash("late"), at(60)));
}

/// Hand-made access units for the codecs the image has no encoder for. The tap and the
/// framing read NAL and OBU headers only, never slice data.
pub(super) mod streams {
    use super::*;

    pub(in crate::webtransport) fn hevc_caps(width: i32) -> gst::Caps {
        // hvcC: profile 1 (Main), compat 0x60000000, level 93, 4-byte NAL lengths.
        let mut hvcc = vec![0u8; 23];
        hvcc[..2].copy_from_slice(&[1, 0x01]);
        hvcc[2..6].copy_from_slice(&0x6000_0000u32.to_be_bytes());
        hvcc[12] = 93;
        hvcc[21] = 0x03;
        gst::Caps::builder("video/x-h265")
            .field("stream-format", "hvc1")
            .field("alignment", "au")
            .field("width", width)
            .field("height", 360)
            .field("framerate", gst::Fraction::new(30, 1))
            .field("codec_data", gst::Buffer::from_slice(hvcc))
            .build()
    }

    pub(in crate::webtransport) fn av1_caps(width: i32) -> gst::Caps {
        gst::Caps::builder("video/x-av1")
            .field("parsed", true)
            .field("stream-format", "obu-stream")
            .field("alignment", "tu")
            .field("width", width)
            .field("height", 360)
            .field("framerate", gst::Fraction::new(30, 1))
            .field(
                "codec_data",
                gst::Buffer::from_slice(vec![0x81, 0x08, 0x0c, 0]),
            )
            .build()
    }

    fn length_prefixed(nals: &[(u8, usize)]) -> Vec<u8> {
        let mut au = Vec::new();
        for &(header, len) in nals {
            au.extend_from_slice(&(len as u32 + 2).to_be_bytes());
            au.extend_from_slice(&[header << 1, 1]);
            au.extend(std::iter::repeat_n(0x55, len));
        }
        au
    }

    /// An H.265 access unit: with `irap`, VPS, SPS, PPS and an IRAP slice of that NAL type
    /// (19 IDR_W_RADL, 21 CRA), otherwise a TRAIL_R slice.
    pub(in crate::webtransport) fn hevc_au(irap: Option<u8>, body: usize) -> Vec<u8> {
        match irap {
            Some(t) => length_prefixed(&[(32, 10), (33, 20), (34, 5), (t, body)]),
            None => length_prefixed(&[(1, body)]),
        }
    }

    fn obu(kind: u8, len: usize) -> Vec<u8> {
        let mut o = vec![kind << 3 | 0x02];
        let mut n = len;
        loop {
            let byte = (n & 0x7f) as u8;
            n >>= 7;
            o.push(if n > 0 { byte | 0x80 } else { byte });
            if n == 0 {
                break;
            }
        }
        o.extend(std::iter::repeat_n(0x55, len));
        o
    }

    /// An AV1 temporal unit as `av1parse` hands it on: temporal delimiter first, a sequence
    /// header when `sequence_header`, then one frame OBU.
    pub(in crate::webtransport) fn av1_tu(sequence_header: bool, body: usize) -> Vec<u8> {
        let mut tu = obu(2, 0);
        if sequence_header {
            tu.extend(obu(1, 10));
        }
        tu.extend(obu(6, body));
        tu
    }

    /// MSB-first bit writer with Exp-Golomb, for parameter sets a real parser accepts.
    #[derive(Default)]
    struct Bits {
        bytes: Vec<u8>,
        used: u8,
    }

    impl Bits {
        fn u(&mut self, bits: u32, value: u64) -> &mut Self {
            for i in (0..bits).rev() {
                if self.used == 0 {
                    self.bytes.push(0);
                }
                let last = self.bytes.len() - 1;
                self.bytes[last] |= (((value >> i) & 1) as u8) << (7 - self.used);
                self.used = (self.used + 1) % 8;
            }
            self
        }

        fn ue(&mut self, value: u64) -> &mut Self {
            let v = value + 1;
            let len = 64 - v.leading_zeros();
            self.u(len - 1, 0).u(len, v)
        }

        fn se(&mut self, value: i64) -> &mut Self {
            self.ue(if value > 0 {
                value as u64 * 2 - 1
            } else {
                value.unsigned_abs() * 2
            })
        }

        /// rbsp_trailing_bits, then emulation prevention, behind the 2-byte NAL header.
        fn nal(&mut self, kind: u8) -> Vec<u8> {
            self.u(1, 1);
            while self.used != 0 {
                self.u(1, 0);
            }
            let mut nal = vec![kind << 1, 1];
            let mut zeros = 0;
            for &b in &self.bytes {
                if zeros >= 2 && b <= 3 {
                    nal.push(3);
                    zeros = 0;
                }
                zeros = if b == 0 { zeros + 1 } else { 0 };
                nal.push(b);
            }
            nal
        }
    }

    fn profile_tier_level(b: &mut Bits) {
        // Main, compat bit 1 and 2, progressive, frame-only, level 93 (3.1).
        b.u(2, 0)
            .u(1, 0)
            .u(5, 1)
            .u(32, 0x6000_0000)
            .u(1, 1)
            .u(1, 0)
            .u(1, 0)
            .u(1, 1);
        b.u(32, 0).u(12, 0).u(8, 93);
    }

    /// An H.265 Annex-B access unit a real `h265parse` accepts: 64x64 Main, VPS, SPS, PPS, then
    /// (`irap`) an IDR_W_RADL slice or a TRAIL_R slice with POC `poc`. The slice data is junk.
    pub(in crate::webtransport) fn hevc_byte_stream(irap: bool, poc: u8) -> Vec<u8> {
        let mut vps = Bits::default();
        vps.u(4, 0)
            .u(1, 1)
            .u(1, 1)
            .u(6, 0)
            .u(3, 0)
            .u(1, 1)
            .u(16, 0xffff);
        profile_tier_level(&mut vps);
        vps.u(1, 1).ue(0).ue(0).ue(0).u(6, 0).ue(0).u(1, 0).u(1, 0);
        let mut sps = Bits::default();
        sps.u(4, 0).u(3, 0).u(1, 1);
        profile_tier_level(&mut sps);
        sps.ue(0).ue(1).ue(64).ue(64).u(1, 0).ue(0).ue(0).ue(4);
        sps.u(1, 1).ue(0).ue(0).ue(0);
        sps.ue(0).ue(3).ue(0).ue(3).ue(0).ue(0);
        sps.u(1, 0)
            .u(1, 0)
            .u(1, 0)
            .u(1, 0)
            .ue(0)
            .u(1, 0)
            .u(1, 0)
            .u(1, 0)
            .u(1, 0)
            .u(1, 0);
        let mut pps = Bits::default();
        pps.ue(0)
            .ue(0)
            .u(1, 0)
            .u(1, 0)
            .u(3, 0)
            .u(1, 0)
            .u(1, 0)
            .ue(0)
            .ue(0)
            .se(0);
        pps.u(1, 0)
            .u(1, 0)
            .u(1, 0)
            .se(0)
            .se(0)
            .u(1, 0)
            .u(1, 0)
            .u(1, 0)
            .u(1, 0);
        pps.u(1, 0)
            .u(1, 0)
            .u(1, 0)
            .u(1, 0)
            .u(1, 0)
            .u(1, 0)
            .ue(0)
            .u(1, 0)
            .u(1, 0);
        let mut slice = Bits::default();
        slice.u(1, 1);
        if irap {
            slice.u(1, 0);
        }
        slice.ue(0).ue(if irap { 2 } else { 1 });
        if !irap {
            // POC lsb, an explicit short-term set of one picture back, then P-slice fields.
            slice
                .u(8, poc as u64)
                .u(1, 0)
                .ue(1)
                .ue(0)
                .ue(0)
                .u(1, 1)
                .u(1, 0)
                .ue(0);
        }
        slice.se(0);
        let mut au = Vec::new();
        let nals: Vec<Vec<u8>> = if irap {
            vec![vps.nal(32), sps.nal(33), pps.nal(34), slice.nal(19)]
        } else {
            vec![slice.nal(1)]
        };
        for (i, mut nal) in nals.into_iter().enumerate() {
            if i + 1 == if irap { 4 } else { 1 } {
                nal.extend(std::iter::repeat_n(0x55, 40));
            }
            au.extend_from_slice(&[0, 0, 0, 1]);
            au.extend(nal);
        }
        au
    }

    /// An AV1 temporal unit a real `av1parse` accepts: a 64x64 still-picture (reduced header)
    /// sequence header, then one lossless key frame OBU whose tile data is junk.
    pub(in crate::webtransport) fn av1_key_tu() -> Vec<u8> {
        let mut seq = Bits::default();
        seq.u(3, 0)
            .u(1, 1)
            .u(1, 1)
            .u(5, 8)
            .u(4, 7)
            .u(4, 7)
            .u(8, 63)
            .u(8, 63);
        seq.u(1, 0).u(1, 0).u(1, 0); // 128x128 superblocks, filter intra, intra edge
        seq.u(1, 0).u(1, 0).u(1, 0); // superres, cdef, restoration
        seq.u(1, 0).u(1, 0).u(1, 0).u(1, 0).u(2, 0).u(1, 0); // color config
        seq.u(1, 0); // film grain params
        seq.u(1, 1);
        while seq.used != 0 {
            seq.u(1, 0);
        }
        let mut frame = Bits::default();
        frame.u(1, 0).u(1, 0).u(1, 0).u(1, 1); // cdf update, screen content, render size, uniform tiles
        frame.u(8, 0).u(3, 0).u(1, 0).u(1, 0).u(1, 0); // base_q_idx 0, delta q, qmatrix, segmentation, tx set
        let mut obu_frame = frame.bytes.clone();
        obu_frame.extend(std::iter::repeat_n(0x55, 40));
        let mut tu = obu(2, 0);
        tu.extend(wrap(1, &seq.bytes));
        tu.extend(wrap(6, &obu_frame));
        tu
    }

    fn wrap(kind: u8, payload: &[u8]) -> Vec<u8> {
        let mut o = obu(kind, 0);
        o.pop();
        o.push(payload.len() as u8);
        o.extend_from_slice(payload);
        o
    }

    /// The OBU types of a temporal unit, panicking on one without a size field.
    pub(in crate::webtransport) fn obu_types(tu: &[u8]) -> Vec<u8> {
        let mut types = Vec::new();
        let mut at = 0;
        while at < tu.len() {
            let h = tu[at];
            assert!(h & 0x02 != 0, "an OBU without a size field");
            types.push(h >> 3 & 0x0f);
            let (mut size, mut shift) = (0usize, 0);
            at += 1;
            loop {
                let b = tu[at];
                at += 1;
                size |= usize::from(b & 0x7f) << shift;
                shift += 7;
                if b & 0x80 == 0 {
                    break;
                }
            }
            at += size;
        }
        assert_eq!(at, tu.len(), "an OBU past the end");
        types
    }

    /// The NAL types of a length-prefixed H.265 access unit.
    pub(in crate::webtransport) fn hevc_types(au: &[u8]) -> Vec<u8> {
        let mut types = Vec::new();
        let mut rest = au;
        while !rest.is_empty() {
            let len = u32::from_be_bytes(rest[..4].try_into().unwrap()) as usize;
            assert!(len > 0 && 4 + len <= rest.len(), "bad NAL length {len}");
            types.push(rest[4] >> 1 & 0x3f);
            rest = &rest[4 + len..];
        }
        types
    }
}

/// An agent with one WebTransport session, encoding a live test picture through the
/// same parser → payloader boundary the WebRTC transport builds.
struct Agent {
    listener: Arc<Listener>,
    url: url::Url,
    hash: Vec<u8>,
    pipeline: gst::Pipeline,
    tap: Option<Tap>,
    /// The session's audio pipeline, when the test gave it one.
    audio_pipeline: Option<gst::Pipeline>,
    /// Every input event the agent's devices were given, as JSON text.
    input: Arc<Mutex<Vec<String>>>,
    /// How many times a departing client's inputs were lifted.
    released: Arc<std::sync::atomic::AtomicUsize>,
}

impl Drop for Agent {
    fn drop(&mut self) {
        self.tap.take();
        let _ = self.pipeline.set_state(gst::State::Null);
        if let Some(audio) = &self.audio_pipeline {
            let _ = audio.set_state(gst::State::Null);
        }
    }
}

async fn agent() -> Agent {
    agent_with(Limits::default()).await
}

async fn agent_with(limits: Limits) -> Agent {
    agent_for(Stream::H264, limits).await
}

async fn agent_with_plan(limits: Limits, plan: audio::Plan) -> Agent {
    agent_built(Stream::H264, limits, plan, "ball", |_| None).await
}

#[derive(Clone, Copy, Debug, PartialEq)]
enum Stream {
    H264,
    H265,
    Av1,
    /// An AV1 encoder whose keyframes carry no sequence header.
    Av1NoSequenceHeader,
    /// AV1 at about 4 Mbit/s: 16 kB frames.
    Av1Heavy,
}

/// A stand-in for an encoder and parser the image does not have: one unit every 33 ms,
/// a keyframe first and on each force-key-unit request, as the parser's src pad would
/// hand them on (`DELTA_UNIT` on every other).
fn synthetic(stream: Stream) -> gst::Pipeline {
    let pipeline = gst::parse::launch(
        "appsrc name=src is-live=true format=time do-timestamp=true \
         ! identity name=enc ! identity name=parse ! fakesink sync=false",
    )
    .unwrap()
    .downcast::<gst::Pipeline>()
    .unwrap();
    let src = pipeline
        .by_name("src")
        .unwrap()
        .downcast::<gstreamer_app::AppSrc>()
        .unwrap();
    src.set_caps(Some(&match stream {
        Stream::H265 => streams::hevc_caps(640),
        _ => streams::av1_caps(640),
    }));
    let forced = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let seen = forced.clone();
    src.static_pad("src")
        .unwrap()
        .add_probe(gst::PadProbeType::EVENT_UPSTREAM, move |_, _| {
            seen.store(true, std::sync::atomic::Ordering::SeqCst);
            gst::PadProbeReturn::Ok
        });
    std::thread::spawn(move || {
        for n in 0u64.. {
            let key = n == 0 || forced.swap(false, std::sync::atomic::Ordering::SeqCst);
            let body = match (stream, key) {
                (Stream::Av1Heavy, true) => 48_000,
                (Stream::Av1Heavy, false) => 16_000,
                (_, true) => 3000,
                (_, false) => 400,
            };
            let data = match stream {
                Stream::H265 => streams::hevc_au(key.then_some(if n == 0 { 19 } else { 21 }), body),
                Stream::Av1NoSequenceHeader => streams::av1_tu(false, body),
                _ => streams::av1_tu(key, body),
            };
            let mut buffer = gst::Buffer::from_slice(data);
            if !key {
                buffer
                    .get_mut()
                    .unwrap()
                    .set_flags(gst::BufferFlags::DELTA_UNIT);
            }
            if src.push_buffer(buffer).is_err() {
                break;
            }
            std::thread::sleep(Duration::from_millis(33));
        }
    });
    pipeline
}

async fn agent_for(stream: Stream, limits: Limits) -> Agent {
    agent_built(stream, limits, audio::Plan::default(), "ball", |_| None).await
}

/// `pattern` is the H.264 test picture's; `rate` arms the session's ABR on its encoder.
async fn agent_built(
    stream: Stream,
    limits: Limits,
    plan: audio::Plan,
    pattern: &str,
    rate: impl FnOnce(&gst::Element) -> Option<Rate>,
) -> Agent {
    gst::init().unwrap();
    let registry = Arc::new(Registry::default());
    registry.assign(SESSION, &token_hash(TOKEN), SystemTime::now() + TOKEN_TTL);
    let pipeline = if stream == Stream::H264 {
        assert!(
            gst::ElementFactory::find("openh264enc").is_some(),
            "the conformance test needs openh264enc (gstreamer1.0-plugins-bad)"
        );
        gst::parse::launch(&format!(
            "videotestsrc is-live=true pattern={pattern} \
             ! video/x-raw,width=640,height=360,framerate=30/1 \
             ! openh264enc name=enc gop-size=1000 bitrate=4000000 \
             ! video/x-h264,profile=constrained-baseline ! h264parse name=parse \
             ! rtph264pay config-interval=-1 pt=96 ! fakesink sync=false",
        ))
        .unwrap()
        .downcast::<gst::Pipeline>()
        .unwrap()
    } else {
        synthetic(stream)
    };
    let parse = pipeline.by_name("parse").unwrap();
    let encoder = pipeline.by_name("enc").unwrap();
    let (input, events, released) = recorded();
    let tap = Tap::attach(
        &registry,
        &parse.static_pad("src").unwrap(),
        &encoder,
        SESSION,
        input,
        plan,
        None,
        rate(&encoder),
    );
    assert!(tap.is_some(), "an assigned session takes the tap");
    pipeline.set_state(gst::State::Playing).unwrap();

    let listener = Listener::bind("127.0.0.1:0".parse().unwrap(), registry, limits)
        .await
        .unwrap();
    tokio::spawn(listener.clone().serve());
    let endpoint = listener.endpoint();
    let hash = base64::engine::general_purpose::STANDARD
        .decode(&endpoint.certificates[0].sha256)
        .unwrap();
    let url = format!("https://127.0.0.1:{}/wt", endpoint.port)
        .parse()
        .unwrap();
    // A config exists once the first caps passed the tap.
    let media = listener.registry().lock()[SESSION].media.clone().unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    while !media.ready() {
        assert!(Instant::now() < deadline, "no caps from the encoder");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    Agent {
        listener,
        url,
        hash,
        pipeline,
        tap,
        audio_pipeline: None,
        input: events,
        released,
    }
}

/// An audio pipeline made the way the session's is (`add_opus_encode`, on silence), tapped
/// at its Opus encoder for the agent's one session.
fn tap_session_audio(agent: &mut Agent) {
    use crate::session::settings::RuntimeSettings;
    use crate::session::StreamParams;
    let stream = StreamParams {
        width: 640,
        height: 360,
        fps: 30,
        bitrate_kbps: 4000,
        h264_profile: "constrained-baseline".to_string(),
        codec: crate::session::Codec::H264,
        abr_floor_kbps: 0,
        mic: true,
    };
    let mut cfg = crate::session::SessionConfig::for_assignment_with(
        &RuntimeSettings::baseline_with(&|_| None),
        stream,
        None,
    );
    cfg.use_test_audio = true;
    let pipeline = gst::Pipeline::new();
    let (_, opus) = crate::session::pipeline::audio_branch::add_opus_encode(&pipeline, &cfg)
        .expect("the session's Opus encode builds");
    let sink = gst::ElementFactory::make("fakesink")
        .property("sync", false)
        .build()
        .unwrap();
    pipeline.add(&sink).unwrap();
    opus.link(&sink).unwrap();
    agent.tap.as_ref().unwrap().audio().tap(&opus);
    pipeline.set_state(gst::State::Playing).unwrap();
    agent.audio_pipeline = Some(pipeline);
}

impl Agent {
    async fn connect(&self) -> Session {
        ClientBuilder::new()
            .with_server_certificate_hashes(vec![self.hash.clone()])
            .unwrap()
            .connect(self.url.clone())
            .await
            .unwrap()
    }

    /// Connect, open the control stream and send `hello`; the first reply.
    async fn hello(&self, token: &str) -> (Session, SendStream, RecvStream, Value) {
        self.hello_with(token, json!({"fec": []})).await
    }

    /// A `hello` with `extra`'s fields beside the required ones.
    async fn hello_with(
        &self,
        token: &str,
        extra: Value,
    ) -> (Session, SendStream, RecvStream, Value) {
        let session = self.connect().await;
        let (mut send, mut recv) = session.open_bi().await.unwrap();
        let mut hello = json!({"type": "hello", "wt_proto": 1, "token": token,
                               "session_id": SESSION, "client": "browser"});
        let extra = extra.as_object().unwrap().clone();
        hello.as_object_mut().unwrap().extend(extra);
        write_message(&mut send, &hello).await.unwrap();
        let reply = read_message(&mut recv).await.unwrap();
        (session, send, recv, reply)
    }
}

thread_local! {
    /// Armed by a test on its own thread. The tests run on current-thread runtimes, so the
    /// next `close_session` there is the one it means: it holds until `release`.
    static CLOSE_HOLD: RefCell<Option<(oneshot::Receiver<()>, oneshot::Sender<()>)>> =
        const { RefCell::new(None) };
}

/// Sent to the test when the held close has run, by dropping it.
pub(super) struct CloseDone(Option<oneshot::Sender<()>>);

impl Drop for CloseDone {
    fn drop(&mut self) {
        if let Some(done) = self.0.take() {
            let _ = done.send(());
        }
    }
}

/// `close_session`'s seam: waits for the test's release when a hold is armed on this thread.
pub(super) async fn hold_close() -> CloseDone {
    let armed = CLOSE_HOLD.with(|h| h.borrow_mut().take());
    match armed {
        Some((release, done)) => {
            let _ = release.await;
            CloseDone(Some(done))
        }
        None => CloseDone(None),
    }
}

/// The close code the browser's `WebTransport.closed` would resolve with.
async fn close_code(session: &Session) -> Option<u32> {
    close_code_within(session, Duration::from_secs(10)).await
}

async fn close_code_within(session: &Session, within: Duration) -> Option<u32> {
    match tokio::time::timeout(within, session.closed()).await {
        Ok(SessionError::WebTransportError(WebTransportError::Closed(code, _))) => Some(code),
        other => panic!("not a WebTransport close: {other:?}"),
    }
}

#[derive(Debug)]
struct Frame {
    keyframe: bool,
    config_id: u8,
    frame_seq: u16,
    ts_90khz: u32,
    capture_us: u64,
    data: Vec<u8>,
}

/// Read video datagrams until `n` access units are whole, reassembling each from its
/// fragments. Returned in completion order. `lossless`: no `pkt_seq` may be missing.
async fn frames(session: &Session, n: usize, lossless: bool) -> Vec<Frame> {
    let mut parts: HashMap<u16, BTreeMap<u16, Bytes>> = HashMap::new();
    let mut done = Vec::new();
    let mut last_pkt: Option<u32> = None;
    while done.len() < n {
        let d = tokio::time::timeout(Duration::from_secs(10), session.read_datagram())
            .await
            .expect("video stalled")
            .unwrap();
        assert!(
            d.len() <= media::MAX_DATAGRAM,
            "a {} byte datagram",
            d.len()
        );
        assert_eq!(d[0], 0x01, "only video is sent");
        let pkt_seq = u32::from_le_bytes(d[1..5].try_into().unwrap());
        if let Some(last) = last_pkt.filter(|_| lossless) {
            assert_eq!(pkt_seq, last.wrapping_add(1), "pkt_seq +1 per datagram");
        }
        last_pkt = Some(pkt_seq);
        let frame_seq = u16::from_le_bytes([d[7], d[8]]);
        let index = u16::from_le_bytes([d[9], d[10]]);
        let count = u16::from_le_bytes([d[11], d[12]]);
        let frags = parts.entry(frame_seq).or_default();
        frags.insert(index, d.slice(media::HEADER..));
        if frags.len() == count as usize {
            let frags = parts.remove(&frame_seq).unwrap();
            done.push(Frame {
                keyframe: d[5] & 1 == 1,
                config_id: d[6],
                frame_seq,
                ts_90khz: u32::from_le_bytes(d[13..17].try_into().unwrap()),
                capture_us: u64::from_le_bytes(d[17..25].try_into().unwrap()),
                data: frags.into_values().flatten().collect(),
            });
        }
    }
    done
}

/// The NAL unit types of an access unit in 4-byte length-prefixed form; panics on
/// anything else (an Annex-B start code, a length past the end).
fn nal_types(au: &[u8]) -> Vec<u8> {
    let mut types = Vec::new();
    let mut rest = au;
    while !rest.is_empty() {
        let len = u32::from_be_bytes(rest[..4].try_into().unwrap()) as usize;
        assert!(len > 0 && 4 + len <= rest.len(), "bad NAL length {len}");
        assert_eq!(rest[4] & 0x80, 0, "forbidden_zero_bit");
        types.push(rest[4] & 0x1f);
        rest = &rest[4 + len..];
    }
    types
}

#[tokio::test]
async fn a_browser_like_client_gets_h264_access_units_after_its_token() {
    let agent = agent().await;

    let (refused, _send, _recv, reply) = agent.hello("not-the-token").await;
    assert_eq!(reply, json!({"type": "error", "code": "auth_failed"}));
    assert_eq!(close_code(&refused).await, Some(CLOSE_AUTH));

    let (session, mut send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack");
    assert_eq!(ack["wt_proto"], 1);
    assert_eq!(ack["video"]["codec"], "h264");
    assert_eq!(
        (
            ack["video"]["width"].clone(),
            ack["video"]["height"].clone()
        ),
        (json!(640), json!(360))
    );
    let avcc = base64::engine::general_purpose::STANDARD
        .decode(ack["video"]["description_b64"].as_str().unwrap())
        .unwrap();
    assert_eq!(
        (avcc[0], avcc[4] & 3),
        (1, 3),
        "avcC with 4-byte NAL lengths"
    );
    let config_id = ack["config_id"].as_u64().unwrap() as u8;

    let started = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap()
        .as_micros() as u64;
    let got = frames(&session, 30, true).await;
    assert!(got[0].keyframe, "the client starts at a keyframe");
    assert!(nal_types(&got[0].data).contains(&5), "an IDR slice");
    for pair in got.windows(2) {
        let (a, b) = (&pair[0], &pair[1]);
        assert_eq!(
            b.frame_seq,
            a.frame_seq.wrapping_add(1),
            "units in order, none lost"
        );
        assert!(
            b.ts_90khz.wrapping_sub(a.ts_90khz) < 1 << 31,
            "ts_90khz rises"
        );
        assert!(b.capture_us >= a.capture_us, "capture time rises");
    }
    for f in &got {
        assert_eq!(f.config_id, config_id);
        assert!(
            f.capture_us.abs_diff(started) < 10_000_000,
            "capture_us is wall-clock µs"
        );
        let types = nal_types(&f.data);
        assert_eq!(
            f.keyframe,
            types.contains(&5),
            "the keyframe flag matches the bitstream"
        );
    }

    write_message(
        &mut send,
        &json!({"type": "keyframe_request", "reason": "startup"}),
    )
    .await
    .unwrap();
    assert!(
        frames(&session, 15, true).await.iter().any(|f| f.keyframe),
        "a keyframe follows the request"
    );

    let (reused, _send, _recv, reply) = agent.hello(TOKEN).await;
    assert_eq!(reply["code"], "auth_failed", "a token is single use");
    assert_eq!(close_code(&reused).await, Some(CLOSE_AUTH));
}

/// An audio datagram as a client parses it (§Audio).
#[derive(Debug)]
struct AudioPacket {
    pkt_seq: u32,
    packet_seq: u16,
    ts_48khz: u32,
    capture_us: u64,
    opus: Vec<u8>,
}

/// Every datagram from `session` for `span`, in arrival order: audio parsed, and the video
/// ones counted. Asserts that one `pkt_seq` counts both channels without a gap.
async fn audio_for(session: &Session, span: Duration) -> (Vec<AudioPacket>, usize) {
    let (mut audio, mut video) = (Vec::new(), 0);
    let mut last: Option<u32> = None;
    let until = Instant::now() + span;
    while let Ok(Ok(d)) = tokio::time::timeout_at(until, session.read_datagram()).await {
        assert!(
            d.len() <= media::MAX_DATAGRAM,
            "a {} byte datagram",
            d.len()
        );
        let pkt_seq = u32::from_le_bytes(d[1..5].try_into().unwrap());
        if let Some(last) = last {
            assert_eq!(
                pkt_seq,
                last.wrapping_add(1),
                "one pkt_seq for video and audio"
            );
        }
        last = Some(pkt_seq);
        match d[0] {
            0x01 => video += 1,
            0x02 => audio.push(AudioPacket {
                pkt_seq,
                packet_seq: u16::from_le_bytes([d[5], d[6]]),
                ts_48khz: u32::from_le_bytes(d[7..11].try_into().unwrap()),
                capture_us: u64::from_le_bytes(d[11..19].try_into().unwrap()),
                opus: d[audio::AUDIO_HEADER..].to_vec(),
            }),
            other => panic!("channel {other:#x} is not sent to a client"),
        }
    }
    (audio, video)
}

#[tokio::test]
async fn a_session_with_audio_and_a_microphone_sends_opus_and_takes_the_microphone() {
    let plan = audio::Plan {
        audio: true,
        mic: true,
        pulse_server: None,
        jitter_ms: 40,
    };
    let mut agent = agent_with_plan(Limits::default(), plan).await;
    tap_session_audio(&mut agent);
    let (session, mut send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(
        ack["audio"],
        json!({"codec": "opus", "channels": 2, "sample_rate": 48000, "ptime_ms": 10})
    );
    assert_eq!(
        ack["mic"],
        json!({"codec": "opus", "channels": 1, "sample_rate": 48000, "ptime_ms": 10})
    );

    let started = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap()
        .as_micros() as u64;
    let (got, video) = audio_for(&session, Duration::from_millis(1500)).await;
    assert!(video > 10, "video still flows beside audio ({video})");
    assert!(got.len() >= 80, "10 ms packets over 1.5 s: {}", got.len());
    for pair in got.windows(2) {
        let (a, b) = (&pair[0], &pair[1]);
        assert_eq!(
            b.packet_seq,
            a.packet_seq.wrapping_add(1),
            "none lost on a clean path"
        );
        assert_eq!(b.ts_48khz.wrapping_sub(a.ts_48khz), 480, "10 ms at 48 kHz");
        assert!(b.pkt_seq != a.pkt_seq);
        assert!(b.capture_us > a.capture_us, "capture time rises");
    }
    // Packets leave the encoder in bursts; capture time follows the audio's own clock.
    for pair in got.windows(2) {
        let step = pair[1].capture_us - pair[0].capture_us;
        assert!(
            step.abs_diff(10_000) < 3_000,
            "capture time steps by 10 ms, not by arrival: {step} µs"
        );
    }
    for p in &got {
        assert!(
            p.capture_us.abs_diff(started) < 10_000_000,
            "capture_us is wall-clock µs"
        );
        // TOC byte: CELT-only, fullband, one 10 ms frame (RFC 6716 §3.1): low-delay, 10 ms.
        assert_eq!(p.opus[0] >> 3, 30, "the session's low-delay 10 ms Opus");
    }

    // The microphone: real 10 ms Opus packets in §Microphone framing reach the sink decoded.
    let packets = audio::test_opus_packets(30);
    let audio_hub = agent.tap.as_ref().unwrap().audio();
    for (i, opus) in packets.iter().enumerate() {
        let mut d = vec![0x05];
        d.extend_from_slice(&(i as u16).to_le_bytes());
        d.extend_from_slice(&(i as u32 * 480).to_le_bytes());
        d.extend_from_slice(opus);
        session.send_datagram(Bytes::from(d)).unwrap();
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let deadline = Instant::now() + Duration::from_secs(5);
    while audio_hub.mic_delivered() < 20 {
        assert!(
            Instant::now() < deadline,
            "{} decoded microphone buffers reached the sink",
            audio_hub.mic_delivered()
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    write_message(
        &mut send,
        &json!({"type": "bye", "reason": "client_closed"}),
    )
    .await
    .unwrap();
}

/// What a codec's access unit must contain for the keyframe flag to be true.
fn carries_random_access(stream: Stream, data: &[u8]) -> bool {
    match stream {
        Stream::H264 => nal_types(data).contains(&5),
        Stream::H265 => streams::hevc_types(data)
            .iter()
            .any(|t| (16..=23).contains(t)),
        Stream::Av1 | Stream::Av1NoSequenceHeader | Stream::Av1Heavy => {
            streams::obu_types(data).contains(&1)
        }
    }
}

async fn codec_session(stream: Stream, codec: &str) {
    let agent = agent_for(stream, Limits::default()).await;
    let (session, mut send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack");
    assert_eq!(ack["video"]["codec"], codec);
    let description = base64::engine::general_purpose::STANDARD
        .decode(ack["video"]["description_b64"].as_str().unwrap())
        .unwrap();
    match stream {
        Stream::H265 => assert_eq!(
            (description[1], description[12], description[21] & 3),
            (1, 93, 3),
            "hvcC: Main, level 93, 4-byte NAL lengths"
        ),
        _ => assert_eq!(description[0], 0x81, "av1C"),
    }

    let got = frames(&session, 40, true).await;
    assert!(got[0].keyframe, "the client starts at a keyframe");
    assert!(got[0].data.len() > 2 * 1123, "a keyframe spans fragments");
    for f in &got {
        assert_eq!(
            f.keyframe,
            carries_random_access(stream, &f.data),
            "the keyframe flag matches the bitstream"
        );
        if stream == Stream::Av1 {
            assert!(
                !streams::obu_types(&f.data).contains(&2),
                "no temporal delimiter"
            );
        }
    }

    write_message(
        &mut send,
        &json!({"type": "keyframe_request", "reason": "startup"}),
    )
    .await
    .unwrap();
    let after = frames(&session, 20, false).await;
    assert!(
        after.iter().any(|f| f.keyframe),
        "a keyframe follows the request"
    );
}

#[tokio::test]
async fn an_h265_session_sends_length_prefixed_access_units_with_irap_keyframes() {
    codec_session(Stream::H265, "h265").await;
}

#[tokio::test]
async fn an_av1_session_sends_obu_temporal_units_without_delimiters_and_sequence_header_keyframes()
{
    codec_session(Stream::Av1, "av1").await;
}

#[tokio::test]
async fn an_encoder_with_no_usable_keyframe_ends_the_session_with_an_internal_error() {
    let agent = agent_for(Stream::Av1NoSequenceHeader, Limits::default()).await;
    let (session, _send, mut recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack");
    let error = tokio::time::timeout(Duration::from_secs(15), async {
        loop {
            let message = read_message(&mut recv).await.unwrap();
            if message["type"] == "error" {
                break message;
            }
        }
    })
    .await
    .expect("no error: the agent kept forcing keyframes for good");
    assert_eq!(error["code"], "internal", "the client falls back to WebRTC");
    assert_eq!(close_code(&session).await, Some(CLOSE_NORMAL));
}

/// The next video datagram, as (frame_seq, frag_index, frag_count, datagram).
async fn datagram(session: &Session) -> (u16, u16, u16, Bytes) {
    let d = tokio::time::timeout(Duration::from_secs(10), session.read_datagram())
        .await
        .expect("video stalled")
        .unwrap();
    let at = |i: usize| u16::from_le_bytes([d[i], d[i + 1]]);
    (at(7), at(9), at(11), d)
}

#[tokio::test]
async fn a_nacked_fragment_is_resent_one_past_the_window_is_a_miss_and_late_gets_a_keyframe() {
    nack_and_late(Stream::H264).await;
}

#[tokio::test]
async fn nack_and_late_keyframe_recovery_work_for_h265() {
    nack_and_late(Stream::H265).await;
}

#[tokio::test]
async fn nack_and_late_keyframe_recovery_work_for_av1() {
    nack_and_late(Stream::Av1).await;
}

async fn nack_and_late(stream: Stream) {
    let agent = agent_for(stream, Limits::default()).await;
    let (session, mut send, mut recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["retransmit_window_ms"], 250);

    let (frame_seq, _, _, original) = datagram(&session).await;
    let index = u16::from_le_bytes([original[9], original[10]]);
    let nack = json!({"type": "nack", "frame_seq": frame_seq, "frags": [[index, index]]});
    write_message(&mut send, &nack).await.unwrap();
    let mut resent = None;
    for _ in 0..1000 {
        let (_, _, _, d) = datagram(&session).await;
        if d[5] & 4 != 0 {
            resent = Some(d);
            break;
        }
    }
    let resent = resent.expect("no retransmission within 1000 datagrams");
    assert_eq!(resent[6..], original[6..], "same header and payload");
    assert_eq!(resent[5], original[5] | 4, "flags plus retransmit");
    let seq = |d: &Bytes| u32::from_le_bytes(d[1..5].try_into().unwrap());
    let ahead = seq(&resent).wrapping_sub(seq(&original));
    assert!(ahead > 0 && ahead < 1 << 31, "a new pkt_seq");

    tokio::time::sleep(Duration::from_millis(400)).await;
    write_message(&mut send, &nack).await.unwrap();
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(5), read_message(&mut recv))
            .await
            .expect("no nack_miss")
            .unwrap(),
        json!({"type": "nack_miss", "frame_seq": frame_seq, "frags": [[index, index]]}),
        "past the 250 ms window the agent no longer holds it"
    );

    write_message(
        &mut send,
        &json!({"type": "keyframe_request", "reason": "late"}),
    )
    .await
    .unwrap();
    assert!(
        frames(&session, 15, false).await.iter().any(|f| f.keyframe),
        "a late frame's keyframe request is answered"
    );
}

/// The receiving side of a `tc tbf` shape, simulated: each datagram is timed through a
/// `cap_kbps` bottleneck with a drop-tail queue, and reported with the contract's
/// `feedback` (§Feedback) as a browser client would.
struct Bottleneck {
    start: std::time::Instant,
    cap_kbps: f64,
    queue_bytes: f64,
    /// When the bottleneck is next free, seconds after `start`.
    free_at: f64,
    /// (`pkt_seq`, arrival in seconds, or `None` dropped), not reported yet.
    pending: Vec<(u32, Option<f64>)>,
    next: Option<u32>,
    seq: u64,
}

impl Bottleneck {
    fn new(cap_kbps: f64) -> Self {
        Self {
            start: std::time::Instant::now(),
            cap_kbps,
            queue_bytes: 64.0 * 1024.0,
            free_at: 0.0,
            pending: Vec::new(),
            next: None,
            seq: 0,
        }
    }

    fn now(&self) -> f64 {
        self.start.elapsed().as_secs_f64()
    }

    fn receive(&mut self, d: &[u8]) {
        let seq = u32::from_le_bytes(d[1..5].try_into().unwrap());
        let next = *self.next.get_or_insert(seq);
        // Missing from loopback: dropped before the bottleneck.
        for gap in 0..seq.wrapping_sub(next).min(8192) {
            self.pending.push((next.wrapping_add(gap), None));
        }
        self.next = Some(seq.wrapping_add(1));
        let (now, service) = (self.now(), d.len() as f64 * 8.0 / (self.cap_kbps * 1000.0));
        let backlog = (self.free_at - now).max(0.0) / service * d.len() as f64;
        let arrival = (backlog + d.len() as f64 <= self.queue_bytes).then(|| {
            self.free_at = self.free_at.max(now) + service;
            self.free_at
        });
        self.pending.push((seq, arrival));
    }

    /// The `feedback` for everything up to the last datagram arrived by now.
    fn feedback(&mut self) -> Option<Value> {
        let now = self.now();
        let last = self
            .pending
            .iter()
            .take(8192)
            .rposition(|&(_, a)| a.is_some_and(|a| a <= now))?;
        let batch: Vec<_> = self.pending.drain(..=last).collect();
        let first_pkt_seq = batch[0].0;
        let ref_s = batch
            .iter()
            .filter_map(|&(_, a)| a)
            .fold(f64::MAX, f64::min);
        let mut bytes = Vec::new();
        let mut lost = 0;
        for &(_, a) in &batch {
            let unit = match a {
                Some(a) if a <= now => (((a - ref_s) * 20_000.0) as u64).min(0xFFFE) as u16,
                _ => {
                    lost += 1;
                    0xFFFF
                }
            };
            bytes.extend_from_slice(&unit.to_le_bytes());
        }
        self.seq += 1;
        Some(json!({
            "type": "feedback", "seq": self.seq,
            "first_pkt_seq": first_pkt_seq, "count": batch.len(),
            "ref_us": (ref_s * 1e6) as u64,
            "arrivals_b64": base64::engine::general_purpose::STANDARD.encode(&bytes),
            "recv_kbps": 0, "lost": lost, "interval_ms": 50,
        }))
    }
}

/// A UDP hop between the test client and the agent. Each datagram the agent sends is held
/// as long as `impair` says, or dropped on `None`; the client's go straight through.
async fn relay(agent: u16, mut impair: impl FnMut() -> Option<Duration> + Send + 'static) -> u16 {
    use tokio::net::UdpSocket;
    let front = Arc::new(UdpSocket::bind("127.0.0.1:0").await.unwrap());
    let back = UdpSocket::bind("127.0.0.1:0").await.unwrap();
    back.connect(("127.0.0.1", agent)).await.unwrap();
    let port = front.local_addr().unwrap().port();
    tokio::spawn(async move {
        let (mut up, mut down) = ([0u8; 2048], [0u8; 2048]);
        let mut client = None;
        loop {
            tokio::select! {
                r = front.recv_from(&mut up) => {
                    let Ok((n, from)) = r else { return };
                    client = Some(from);
                    let _ = back.send(&up[..n]).await;
                }
                r = back.recv(&mut down) => {
                    let Ok(n) = r else { return };
                    let (Some(to), Some(hold)) = (client, impair()) else { continue };
                    let (front, bytes) = (front.clone(), down[..n].to_vec());
                    tokio::spawn(async move {
                        tokio::time::sleep(hold).await;
                        let _ = front.send_to(&bytes, to).await;
                    });
                }
            }
        }
    });
    port
}

/// The client's §Feedback on a path that may reorder: arrivals by `pkt_seq`, each range
/// reported once everything in it has arrived or a later datagram has.
#[derive(Default)]
struct Arrivals {
    start: Option<std::time::Instant>,
    /// `pkt_seq` → µs after `start`.
    seen: BTreeMap<u32, u64>,
    next: Option<u32>,
    seq: u64,
}

impl Arrivals {
    fn receive(&mut self, d: &[u8]) {
        let pkt_seq = u32::from_le_bytes(d[1..5].try_into().unwrap());
        let start = *self.start.get_or_insert_with(std::time::Instant::now);
        self.next.get_or_insert(pkt_seq);
        self.seen
            .insert(pkt_seq, start.elapsed().as_micros() as u64);
    }

    fn feedback(&mut self) -> Option<Value> {
        let first = self.next?;
        let last = *self.seen.keys().next_back()?;
        let count = (last.checked_sub(first)? + 1).min(8192);
        let ref_us = self
            .seen
            .range(first..first + count)
            .map(|(_, &t)| t)
            .min()?;
        let mut bytes = Vec::new();
        let mut lost = 0;
        for s in first..first + count {
            let unit = match self.seen.remove(&s) {
                Some(t) => (((t - ref_us) / 50).min(0xFFFE)) as u16,
                None => {
                    lost += 1;
                    0xFFFF
                }
            };
            bytes.extend_from_slice(&unit.to_le_bytes());
        }
        self.next = Some(first + count);
        self.seen.retain(|&s, _| s >= first + count);
        self.seq += 1;
        Some(json!({
            "type": "feedback", "seq": self.seq,
            "first_pkt_seq": first, "count": count,
            "ref_us": ref_us,
            "arrivals_b64": base64::engine::general_purpose::STANDARD.encode(&bytes),
            "recv_kbps": 0, "lost": lost, "interval_ms": 50,
        }))
    }
}

/// An agent whose ABR is armed with a 4 Mbit/s floor and an encoder that never moves: what
/// reaches the client is the transport's doing.
async fn governed_agent(stream: Stream) -> Agent {
    agent_built(
        stream,
        Limits::default(),
        audio::Plan::default(),
        "snow",
        |_| {
            Some(Rate {
                ceiling_kbps: 8000.0,
                floor_kbps: Box::new(|| 4000.0),
                retargets: true,
                start: Box::new(|| {}),
                observe: Box::new(|_| {}),
            })
        },
    )
    .await
}

/// A client on the end of `impair`ed path, sending its feedback every 50 ms.
async fn through(
    agent: &mut Agent,
    impair: impl FnMut() -> Option<Duration> + Send + 'static,
) -> (Session, SendStream, RecvStream) {
    let port = relay(agent.url.port().unwrap(), impair).await;
    agent.url.set_port(Some(port)).unwrap();
    let (session, send, recv, reply) = agent.hello(TOKEN).await;
    assert_eq!(reply["type"], "hello_ack");
    (session, send, recv)
}

/// #1330: netem's jitter without a rate reorders almost every datagram. QUIC's loss
/// detection read each one overtaken by three as lost, its window collapsed, and the video
/// waited and was dropped behind it: the browser got nothing for minutes.
#[tokio::test]
async fn reordered_delivery_does_not_stall_the_datagram_path() {
    // Snow does not compress: well over 4 Mbit/s.
    let mut agent = governed_agent(Stream::H264).await;
    // Each datagram held 0–40 ms on its own: most are overtaken by several.
    let mut x = 0x2545_f491_4f6c_dd1du64;
    let (session, mut send, _recv) = through(&mut agent, move || {
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        Some(Duration::from_micros(x % 40_000))
    })
    .await;
    let mut path = Arrivals::default();
    let mut report = tokio::time::interval(Duration::from_millis(50));
    let measure_from = Instant::now() + Duration::from_secs(2);
    let deadline = tokio::time::sleep(Duration::from_secs(6));
    tokio::pin!(deadline);
    let mut got: Vec<u32> = Vec::new();
    loop {
        tokio::select! {
            d = session.read_datagram() => {
                let d = d.unwrap();
                if Instant::now() >= measure_from {
                    got.push(u32::from_le_bytes(d[1..5].try_into().unwrap()));
                }
                path.receive(&d);
            }
            _ = report.tick() => {
                if let Some(fb) = path.feedback() {
                    write_message(&mut send, &fb).await.unwrap();
                }
            }
            () = &mut deadline => break,
        }
    }
    let span = got.iter().max().unwrap() - got.iter().min().unwrap() + 1;
    let delivered = got.len() as f64 / span as f64;
    // About 6500 here; plain Cubic delivered about 600 (measured, five runs each).
    assert!(got.len() > 2000, "{} datagrams in 4 s", got.len());
    assert!(
        delivered > 0.95,
        "{:.0} % of what was sent arrived",
        delivered * 100.0
    );
}

/// Logs `webtransport-client-quic-stats` lines into `captured` on this thread.
fn capture_info(captured: &Captured) -> tracing::subscriber::DefaultGuard {
    let subscriber = tracing_subscriber::fmt()
        .with_max_level(tracing::Level::INFO)
        .with_ansi(false)
        .with_writer(captured.clone())
        .finish();
    let guard = tracing::subscriber::set_default(subscriber);
    tracing::callsite::rebuild_interest_cache();
    guard
}

/// Says `bye` and waits for the agent's line on the client's leave: whether QUIC's window
/// followed the estimate, and the rate it was held to, kbit/s.
async fn governed_at_leave(mut send: SendStream, captured: &Captured) -> (bool, u64) {
    write_message(&mut send, &json!({"type": "bye"}))
        .await
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        let logs = String::from_utf8(captured.0.lock().unwrap().clone()).unwrap();
        if let Some(line) = logs
            .lines()
            .find(|l| l.contains("webtransport-client-quic-stats"))
        {
            let field = |name: &str| {
                let value = line.split(name).nth(1).expect("logged");
                value.split(' ').next().unwrap().to_string()
            };
            return (
                field(" governed=").parse().unwrap(),
                field("governed_kbps=").parse().unwrap(),
            );
        }
        assert!(
            Instant::now() < deadline,
            "the client's leave was not logged"
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

/// §Feedback: a client that sends no feedback follows QUIC congestion control alone; one
/// that does hands the window to the estimate.
#[tokio::test]
async fn only_a_client_that_sends_feedback_takes_quic_congestion_control_away() {
    let captured = Captured::default();
    let _logs = capture_info(&captured);
    let agent = governed_agent(Stream::H264).await;
    let (session, send, _recv, reply) = agent.hello(TOKEN).await;
    assert_eq!(reply["type"], "hello_ack");
    for _ in 0..30 {
        session.read_datagram().await.unwrap();
    }
    assert!(
        !governed_at_leave(send, &captured).await.0,
        "no feedback: Cubic"
    );
    drop(agent);

    let captured = Captured::default();
    let _logs = capture_info(&captured);
    let agent = governed_agent(Stream::H264).await;
    let (session, mut send, _recv, reply) = agent.hello(TOKEN).await;
    assert_eq!(reply["type"], "hello_ack");
    let mut path = Arrivals::default();
    for _ in 0..30 {
        path.receive(&session.read_datagram().await.unwrap());
    }
    write_message(&mut send, &path.feedback().unwrap())
        .await
        .unwrap();
    let (governed, kbps) = governed_at_leave(send, &captured).await;
    assert!(governed && kbps >= 4000, "feedback: governed at {kbps}");
}

/// §Feedback: while PyroWave is active its own rate controller stands in for the estimate,
/// so its feedback leaves QUIC congestion control in place.
#[tokio::test]
async fn a_pyrowave_client_keeps_quic_congestion_control() {
    let captured = Captured::default();
    let _logs = capture_info(&captured);
    let agent = governed_agent(Stream::H264).await;
    let media = agent.listener.registry().lock()[SESSION]
        .media
        .clone()
        .unwrap();
    let grant = PyroWaveGrant {
        wanted: true,
        target_kbps: 200_000,
        floor_kbps: 100_000,
        max_rtt_ms: 1_000,
    };
    media.grant_pyrowave(grant, (320, 192, 60), true);
    let raw = std::fs::read(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../testdata/pyrowave/detail_320x192_xhi.pwv1"
    ))
    .unwrap();
    let (session, mut send, _recv, reply) =
        agent.hello_with(TOKEN, json!({"pyrowave": true})).await;
    assert_eq!(reply["video"]["codec"], "pyrowave");
    let mut path = Bottleneck::new(1e6);
    media.on_pyrowave_frame(&gst::Buffer::from_slice(raw[28..].to_vec()));
    path.receive(&session.read_datagram().await.unwrap());
    tokio::time::sleep(Duration::from_millis(20)).await;
    let feedback = path.feedback().expect("a datagram to report");
    write_message(&mut send, &feedback).await.unwrap();
    assert!(!governed_at_leave(send, &captured).await.0);
}

/// #1330: a path that carries nothing for 2 s closes QUIC's window, and quinn's 1 MiB queue
/// holds 2 s of 4 Mbit/s video, dropping only the oldest. Behind a stalled queue the agent
/// stops adding video, so what is still worth nothing when the path returns is the NACK
/// hold's worth, and a keyframe follows it. A NACK behind it is not answered either.
#[tokio::test]
async fn video_behind_a_stalled_path_is_skipped_not_queued() {
    use std::sync::atomic::{AtomicBool, Ordering::Relaxed};
    let mut agent = governed_agent(Stream::Av1Heavy).await;
    let down = Arc::new(AtomicBool::new(false));
    let cut = down.clone();
    let (session, mut send, mut recv) = through(&mut agent, move || {
        (!cut.load(Relaxed)).then_some(Duration::ZERO)
    })
    .await;
    let missed = Arc::new(AtomicBool::new(false));
    let miss = missed.clone();
    tokio::spawn(async move {
        while let Ok(m) = read_message(&mut recv).await {
            miss.fetch_or(m["type"] == "nack_miss", Relaxed);
        }
    });
    let mut path = Arrivals::default();
    let mut report = tokio::time::interval(Duration::from_millis(50));
    let t0 = Instant::now();
    let (outage, up_at, end) = (
        t0 + Duration::from_secs(2),
        t0 + Duration::from_secs(4),
        t0 + Duration::from_secs(9),
    );
    // A frame and when it arrived, before the outage: the synthetic encoder's clock.
    let mut clock = None::<(u16, Instant)>;
    let (mut stale, mut fresh) = (BTreeMap::new(), None);
    // The last first fragment that arrived before the outage, by frame and pkt_seq, and
    // whether it came again after: a resend.
    let (mut last_first, mut nacked, mut resent) = (None::<(u16, u32)>, false, false);
    loop {
        let now = Instant::now();
        if now >= end {
            break;
        }
        down.store((outage..up_at).contains(&now), Relaxed);
        // Video is skipped from about 0.7 s in (QUIC's window fills, then a hold passes). The
        // frame is past the retransmit window by then, so an answer would be a nack_miss.
        if !nacked && now >= outage + Duration::from_millis(1500) {
            nacked = true;
            let (frame_seq, _) = last_first.expect("a frame before the outage");
            let nack = json!({"type": "nack", "frame_seq": frame_seq, "frags": [[0, 0]]});
            write_message(&mut send, &nack).await.unwrap();
        }
        tokio::select! {
            d = session.read_datagram() => {
                let d = d.unwrap();
                path.receive(&d);
                let frame_seq = u16::from_le_bytes([d[7], d[8]]);
                let pkt_seq = u32::from_le_bytes(d[1..5].try_into().unwrap());
                if d[9..11] == [0, 0] {
                    match last_first {
                        _ if now < outage => last_first = Some((frame_seq, pkt_seq)),
                        Some((f, p)) if f == frame_seq && p != pkt_seq => resent = true,
                        _ => {}
                    }
                }
                match clock {
                    _ if now < outage => clock = Some((frame_seq, now)),
                    Some((f0, at)) if now >= up_at => {
                        let made = at + Duration::from_millis(33) * u32::from(frame_seq.wrapping_sub(f0));
                        if now.saturating_duration_since(made) > Duration::from_millis(200) {
                            stale.entry(frame_seq).or_insert(now);
                        } else {
                            fresh.get_or_insert(now);
                        }
                    }
                    _ => {}
                }
            }
            _ = report.tick() => {
                if let Some(fb) = path.feedback() {
                    write_message(&mut send, &fb).await.unwrap();
                }
            }
            _ = tokio::time::sleep(Duration::from_millis(10)) => {}
        }
    }
    // The NACK hold (250 ms) plus the frames the closing window swallowed, at 30 fps.
    assert!(
        stale.len() <= 15,
        "{} frames over 200 ms old arrived after the path came back",
        stale.len()
    );
    // Measured 0.12–0.16 s; QUIC's probe, backed off over the outage, can make it about 2 s.
    // Under the guard's give-up (6 s of skipping from 250 ms into the outage): video resumes
    // on a keyframe and the media is not ended.
    let fresh = fresh.expect("no fresh video after the path came back");
    assert!(
        fresh - up_at < Duration::from_secs(4),
        "fresh video {:?} after",
        fresh - up_at
    );
    assert!(
        !resent,
        "a NACK behind the stalled queue was answered with a resend"
    );
    assert!(
        !missed.load(Relaxed),
        "a NACK behind the stalled queue was answered with a nack_miss"
    );
}

/// #1040: receiver feedback from a rate-capped client drives the session's real ABR
/// (governor, classifier, smooth mode) until the encoder sends below the cap. Below it
/// within 10 s needs the #68 emergency drop: smooth's capped steps (−12.5 % per 7 s
/// dwell) take over 30 s.
#[tokio::test]
async fn a_rate_capped_client_steps_the_encoder_below_the_cap() {
    const CAP_KBPS: f64 = 1500.0;
    let metrics = Arc::new(crate::session::metrics::SessionMetrics::new("smooth", 30));
    let (trace_tx, _trace_rx) = tokio::sync::mpsc::channel(256);
    let trace_tx =
        crate::session::runner::DiagnosticEventTx::new(trace_tx, Arc::default(), Arc::default());
    let cfg = {
        let mut settings = crate::session::settings::RuntimeSettings::baseline_with(&|_| None);
        settings.encoder = crate::session::EncoderChoice::Openh264;
        settings.abr_mode = crate::session::AbrMode::Smooth;
        let stream = crate::session::StreamParams {
            width: 640,
            height: 360,
            fps: 30,
            bitrate_kbps: 4000,
            h264_profile: "constrained-baseline".to_string(),
            codec: crate::session::Codec::H264,
            abr_floor_kbps: 0,
            mic: false,
        };
        crate::session::SessionConfig::for_assignment_with(&settings, stream, None)
    };
    let abr_metrics = metrics.clone();
    // Snow does not compress: the encoder really sends its setpoint, over the cap.
    let agent = agent_built(
        Stream::H264,
        Limits::default(),
        audio::Plan::default(),
        "snow",
        move |encoder| {
            crate::session::pipeline::feedback_abr(
                encoder,
                &cfg,
                abr_metrics,
                trace_tx,
                SESSION.to_string(),
                None,
            )
        },
    )
    .await;
    let encoder = agent.pipeline.by_name("enc").unwrap();
    // What the session's encoder probes feed: the classifier's "sending at the cap".
    let sent = metrics.clone();
    encoder
        .static_pad("src")
        .unwrap()
        .add_probe(gst::PadProbeType::BUFFER, move |_, info| {
            if let Some(b) = info.buffer() {
                sent.record_encode_out(std::time::Instant::now(), b.size() as u64);
            }
            gst::PadProbeReturn::Ok
        });
    let (session, mut send, _recv, _) = agent.hello(TOKEN).await;

    let mut path = Bottleneck::new(CAP_KBPS);
    let mut report = tokio::time::interval(Duration::from_millis(50));
    let mut drain = tokio::time::interval(Duration::from_secs(1));
    let mut congested = false;
    let deadline = tokio::time::sleep(Duration::from_secs(10));
    tokio::pin!(deadline);
    let kbps = loop {
        tokio::select! {
            d = session.read_datagram() => path.receive(&d.unwrap()),
            _ = report.tick() => {
                if let Some(fb) = path.feedback() {
                    write_message(&mut send, &fb).await.unwrap();
                }
            }
            _ = drain.tick() => {
                // The runner's heartbeat drains a window every ~5 s; faster here.
                let window = metrics.drain_window(std::time::Instant::now());
                congested |= window.adaptation_state == "network_congested";
                let kbps = encoder.property::<u32>("bitrate") / 1000;
                if (kbps as f64) < CAP_KBPS {
                    break kbps;
                }
            }
            () = &mut deadline => panic!(
                "the encoder still at {} kbps after 10 s behind a {CAP_KBPS} kbps cap",
                encoder.property::<u32>("bitrate") / 1000
            ),
        }
    };
    assert!(congested, "the drop came through the congestion classifier");
    assert!(kbps >= 500, "never below the ABR floor ({kbps} kbps)");
}

/// §Feedback: while PyroWave is active its own rate controller stands in for the ABR, so a
/// PyroWave client's feedback feeds no estimate; after its fallback to H.264 it does.
#[tokio::test]
async fn feedback_while_pyrowave_is_active_feeds_no_estimate() {
    use std::sync::atomic::{AtomicUsize, Ordering};
    let fed = Arc::new(AtomicUsize::new(0));
    let count = fed.clone();
    let agent = agent_built(
        Stream::H264,
        Limits::default(),
        audio::Plan::default(),
        "ball",
        move |_| {
            Some(Rate {
                ceiling_kbps: 4000.0,
                floor_kbps: Box::new(|| 500.0),
                retargets: true,
                start: Box::new(|| {}),
                observe: Box::new(move |_| {
                    count.fetch_add(1, Ordering::SeqCst);
                }),
            })
        },
    )
    .await;
    let media = agent.listener.registry().lock()[SESSION]
        .media
        .clone()
        .unwrap();
    media.grant_pyrowave(
        PyroWaveGrant {
            wanted: true,
            target_kbps: 200_000,
            floor_kbps: 100_000,
            max_rtt_ms: 1_000,
        },
        (320, 192, 60),
        true,
    );
    let raw = std::fs::read(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../testdata/pyrowave/detail_320x192_xhi.pwv1"
    ))
    .unwrap();
    let session = agent.connect().await;
    let (mut send, mut recv) = session.open_bi().await.unwrap();
    let hello = json!({"type": "hello", "wt_proto": 1, "token": TOKEN, "session_id": SESSION,
                       "client": "browser", "pyrowave": true});
    write_message(&mut send, &hello).await.unwrap();
    assert_eq!(
        read_message(&mut recv).await.unwrap()["video"]["codec"],
        "pyrowave"
    );

    // A path with room: every datagram is reported as arrived, as a browser would.
    let mut path = Bottleneck::new(1e6);
    media.on_pyrowave_frame(&gst::Buffer::from_slice(raw[28..].to_vec()));
    path.receive(&session.read_datagram().await.unwrap());
    tokio::time::sleep(Duration::from_millis(20)).await;
    let feedback = path.feedback().expect("a datagram to report");
    write_message(&mut send, &feedback).await.unwrap();
    // The fallback's hello_ack comes after the agent read that feedback: the control
    // stream is in order.
    let fallback = json!({"type": "codec_fallback", "reason": "decode_error"});
    write_message(&mut send, &fallback).await.unwrap();
    assert_eq!(
        read_message(&mut recv).await.unwrap()["video"]["codec"],
        "h264"
    );
    assert_eq!(
        fed.load(Ordering::SeqCst),
        0,
        "a PyroWave client's feedback fed the ABR"
    );

    let deadline = Instant::now() + Duration::from_secs(10);
    while fed.load(Ordering::SeqCst) == 0 {
        assert!(
            Instant::now() < deadline,
            "H.264 feedback never fed the ABR"
        );
        if let Ok(d) =
            tokio::time::timeout(Duration::from_millis(50), session.read_datagram()).await
        {
            path.receive(&d.unwrap());
        }
        if let Some(feedback) = path.feedback() {
            write_message(&mut send, &feedback).await.unwrap();
        }
    }
}

/// #1040: the estimate is the video ABR's, and on WebRTC audio is a peer connection GCC
/// never sees. A still screen with music: only audio flows, and feedback that reports it
/// arriving later and later, as behind a queue, moves no estimate.
#[tokio::test]
async fn audio_alone_never_moves_the_video_estimate() {
    let fed = Arc::new(Mutex::new(Vec::new()));
    let seen = fed.clone();
    let plan = audio::Plan {
        audio: true,
        mic: false,
        pulse_server: None,
        jitter_ms: 40,
    };
    let mut agent = agent_built(Stream::H264, Limits::default(), plan, "ball", move |_| {
        Some(Rate {
            ceiling_kbps: 4000.0,
            floor_kbps: Box::new(|| 500.0),
            retargets: true,
            start: Box::new(|| {}),
            observe: Box::new(move |kbps| seen.lock().unwrap().push(kbps)),
        })
    })
    .await;
    tap_session_audio(&mut agent);
    let (session, mut send, _recv, _) = agent.hello(TOKEN).await;
    // Video reached the client, then the screen goes still.
    while session.read_datagram().await.unwrap()[0] != 0x01 {}
    agent.pipeline.set_state(gst::State::Paused).unwrap();

    let mut path = Bottleneck::new(1e6);
    let (mut audio, mut late) = (0, 0.0);
    let until = Instant::now() + Duration::from_secs(2);
    while let Ok(d) = tokio::time::timeout_at(until, session.read_datagram()).await {
        let d = d.unwrap();
        // Each audio datagram waits 10 ms longer than the one before it.
        if d[0] == 0x02 {
            audio += 1;
            late += 0.010;
        }
        let pkt_seq = u32::from_le_bytes(d[1..5].try_into().unwrap());
        path.pending.push((pkt_seq, Some(path.now() + late)));
        if let Some(feedback) = path.feedback() {
            write_message(&mut send, &feedback).await.unwrap();
        }
    }
    assert!(audio >= 100, "10 ms audio for 2 s: {audio} datagrams");
    let fed = fed.lock().unwrap();
    assert!(!fed.is_empty(), "the feedback was read");
    assert!(
        fed.iter().all(|&kbps| kbps == 4000.0),
        "audio alone moved the video estimate: {fed:?}"
    );
}

fn rate(retargets: bool) -> Rate {
    Rate {
        ceiling_kbps: 8000.0,
        floor_kbps: Box::new(|| 4000.0),
        retargets,
        start: Box::new(|| {}),
        observe: Box::new(|_| {}),
    }
}

/// 200 video datagrams logged as sent 1 ms apart from `at`, then feedback that names them
/// all as arrived 1 ms apart, read 250 ms after `at`.
fn answered(control: &mut Control, first: u32, at: std::time::Instant) {
    let mut arrivals = Vec::new();
    for i in 0..200u32 {
        let mut d = vec![0u8; media::MAX_DATAGRAM];
        d[1..5].copy_from_slice(&(first + i).to_le_bytes());
        control
            .log
            .record(&d, at + Duration::from_millis(i.into()), false);
        arrivals.extend_from_slice(&((i * 20) as u16).to_le_bytes());
    }
    let fb = json!({
        "type": "feedback", "seq": first / 200 + 1,
        "first_pkt_seq": first, "count": 200,
        "ref_us": u64::from(first) * 1000,
        "arrivals_b64": base64::engine::general_purpose::STANDARD.encode(&arrivals),
        "recv_kbps": 0, "lost": 0, "interval_ms": 50,
    });
    let path = congestion::QuicPath {
        rtt: Duration::from_millis(20),
        cwnd_bytes: 0,
        congestion_events: 0,
    };
    control.feedback(&fb, path, at + Duration::from_millis(250));
}

/// §Feedback: with the ABR off the estimate never reaches the encoder, so QUIC keeps its
/// own congestion control.
#[test]
fn with_the_abr_off_feedback_never_takes_quic_congestion_control_away() {
    let rate = rate(false);
    let governor = congestion::Governor::default();
    let mut control = Control::new(&rate, Arc::default(), false, governor.clone());
    answered(&mut control, 0, std::time::Instant::now());
    assert_eq!(governor.kbps(), 0);
}

/// Feedback that stops hands the window back to Cubic, and the next feedback takes it again.
#[test]
fn silent_feedback_hands_the_window_back_to_cubic_until_feedback_returns() {
    let rate = rate(true);
    let governor = congestion::Governor::default();
    let mut control = Control::new(&rate, Arc::default(), false, governor.clone());
    let t0 = std::time::Instant::now();
    let rtt = Duration::from_millis(20);
    answered(&mut control, 0, t0);
    assert!(governor.kbps() >= 4000, "the estimate governs");
    // Media goes on; no feedback answers it.
    let mut d = vec![0u8; media::MAX_DATAGRAM];
    d[1..5].copy_from_slice(&200u32.to_le_bytes());
    control
        .log
        .record(&d, t0 + Duration::from_millis(300), false);
    control.tick(rtt, t0 + Duration::from_millis(700));
    assert!(governor.kbps() >= 4000, "inside the silence timeout");
    control.tick(rtt, t0 + Duration::from_millis(900));
    assert_eq!(governor.kbps(), 0, "the decay began: Cubic");
    answered(&mut control, 201, t0 + Duration::from_secs(2));
    assert!(governor.kbps() >= 4000, "feedback is back: governed again");
}

/// A client displaced, or told the session ended, neither starts nor feeds the ABR: the
/// encoder is its successor's, and a start after the session ended would register hooks
/// that keep the encoder alive.
#[test]
fn a_client_that_is_gone_neither_starts_nor_feeds_the_abr() {
    use std::sync::atomic::{AtomicUsize, Ordering::SeqCst};
    let calls = Arc::new(AtomicUsize::new(0));
    let (started, fed) = (calls.clone(), calls.clone());
    let rate = Rate {
        ceiling_kbps: 4000.0,
        floor_kbps: Box::new(|| 500.0),
        retargets: true,
        start: Box::new(move || {
            started.fetch_add(1, SeqCst);
        }),
        observe: Box::new(move |_| {
            fed.fetch_add(1, SeqCst);
        }),
    };
    let gone = Arc::new(OnceLock::new());
    let mut control = Control::new(&rate, gone.clone(), false, Default::default());
    control.feed(3000.0);
    assert_eq!(calls.load(SeqCst), 2, "a live client starts and feeds");
    gone.set(Gone::TakenOver).unwrap();
    control.feed(3000.0);
    Control::new(&rate, gone, false, Default::default()).feed(3000.0);
    assert_eq!(calls.load(SeqCst), 2);
}

/// A path with room that loses every 25th datagram (4 %), as the client's feedback
/// reports it (§Feedback). True when `d` is one it lost.
struct Lossy {
    path: Bottleneck,
    reported: Instant,
}

impl Lossy {
    fn new() -> Self {
        Self {
            path: Bottleneck::new(1e6),
            reported: Instant::now(),
        }
    }

    async fn receive(&mut self, d: &[u8], send: &mut SendStream) -> bool {
        let lost = u32::from_le_bytes(d[1..5].try_into().unwrap()) % 25 == 24;
        if !lost {
            // The gap a lost one leaves is what `Bottleneck` reports as not received.
            self.path.receive(d);
        }
        if self.reported.elapsed() >= Duration::from_millis(50) {
            self.reported = Instant::now();
            if let Some(feedback) = self.path.feedback() {
                write_message(send, &feedback).await.unwrap();
            }
        }
        lost
    }
}

/// #1046: repair datagrams are paid for out of the estimate. At 4 % loss they are a fifth
/// on top of the video, so the ABR is fed the estimate over 1.2; a client that takes no FEC
/// leaves it whole.
#[test]
fn the_abr_is_fed_the_estimate_less_the_fec_share() {
    let fed = Arc::new(Mutex::new(Vec::new()));
    let seen = fed.clone();
    let rate = Rate {
        ceiling_kbps: 6000.0,
        floor_kbps: Box::new(|| 500.0),
        retargets: true,
        start: Box::new(|| {}),
        observe: Box::new(move |kbps: f64| seen.lock().unwrap().push(kbps.round())),
    };
    let mut control = Control::new(&rate, Arc::default(), true, Default::default());
    control.feed(4200.0);
    let fec = control.fec.as_mut().unwrap();
    fec.observe(0.04, std::time::Instant::now());
    control.feed(4200.0);
    Control::new(&rate, Arc::default(), false, Default::default()).feed(4200.0);
    assert_eq!(*fed.lock().unwrap(), [4200.0, 3500.0, 4200.0]);
}

/// A session whose frames take several datagrams (snow does not compress), with an ABR
/// that writes the encoder (`retargets`) or only tracks the estimate.
async fn lossy_session(retargets: bool) -> Agent {
    agent_built(
        Stream::H264,
        Limits::default(),
        audio::Plan::default(),
        "snow",
        move |_| {
            Some(Rate {
                ceiling_kbps: 4000.0,
                floor_kbps: Box::new(|| 500.0),
                retargets,
                start: Box::new(|| {}),
                observe: Box::new(|_| {}),
            })
        },
    )
    .await
}

/// #1046, §FEC: a client that offered `xor` and reports loss is sent repair datagrams, and
/// a fragment it lost is rebuilt from one by the contract's rule, with no NACK and so no
/// retransmission.
#[tokio::test]
async fn a_client_that_offers_xor_and_reports_loss_rebuilds_a_fragment_without_a_nack() {
    let agent = lossy_session(true).await;
    let session = agent.connect().await;
    let (mut send, mut recv) = session.open_bi().await.unwrap();
    let hello = json!({"type": "hello", "wt_proto": 1, "token": TOKEN,
                       "session_id": SESSION, "client": "browser", "fec": ["xor"]});
    write_message(&mut send, &hello).await.unwrap();
    assert_eq!(read_message(&mut recv).await.unwrap()["fec"], "xor");

    let mut path = Lossy::new();
    let mut frames: HashMap<u16, BTreeMap<u16, Bytes>> = HashMap::new();
    let mut lost: HashMap<(u16, u16), Bytes> = HashMap::new();
    let (mut repairs, mut rebuilt) = (0, 0);
    let deadline = Instant::now() + Duration::from_secs(20);
    while rebuilt < 3 {
        let d = tokio::time::timeout_at(deadline, session.read_datagram())
            .await
            .unwrap_or_else(|_| panic!("{repairs} repairs and {rebuilt} rebuilt in 20 s"))
            .unwrap();
        let dropped = path.receive(&d, &mut send).await;
        let at = |i: usize| u16::from_le_bytes([d[i], d[i + 1]]);
        if d[0] == 0x01 {
            assert_eq!(d[5] & 4, 0, "no NACK was sent, so nothing is resent");
            if dropped {
                lost.insert((at(7), at(9)), d.slice(media::HEADER..));
            } else {
                frames
                    .entry(at(7))
                    .or_default()
                    .insert(at(9), d.slice(media::HEADER..));
            }
            continue;
        }
        assert_eq!(d[0], 0x04, "video and FEC only");
        repairs += 1;
        if dropped {
            continue;
        }
        assert!(d.len() <= media::MAX_DATAGRAM);
        assert_eq!(d[5], 0x01, "scheme xor");
        let (frame_seq, first, count, stride) = (at(6), at(8), d[10] as u16, d[11] as u16);
        assert!((2..=48).contains(&count) && stride >= 1);
        // §FEC: the one missing fragment is the repair payload XOR every other protected
        // payload, truncated to `length_xor` XOR every other protected length.
        let Some(have) = frames.get(&frame_seq) else {
            continue;
        };
        let group: Vec<u16> = (0..count).map(|i| first + i * stride).collect();
        let missing: Vec<u16> = group
            .iter()
            .copied()
            .filter(|i| !have.contains_key(i))
            .collect();
        let [k] = missing[..] else { continue };
        let mut payload = d[14..].to_vec();
        let mut len = at(12);
        for other in group.iter().filter(|&&i| i != k).map(|i| &have[i]) {
            len ^= other.len() as u16;
            for (out, byte) in payload.iter_mut().zip(other.iter()) {
                *out ^= byte;
            }
        }
        payload.truncate(len as usize);
        // One the loopback itself dropped (a loaded machine) left nothing to compare with.
        let Some(original) = lost.get(&(frame_seq, k)) else {
            continue;
        };
        assert_eq!(payload, original[..], "fragment {k} of frame {frame_seq}");
        rebuilt += 1;
    }
}

/// §Hello: a client that offers no scheme is answered `fec: null` and sent no repair
/// datagram, whatever loss it reports.
#[tokio::test]
async fn a_client_that_offers_no_fec_gets_none_whatever_it_loses() {
    let agent = lossy_session(true).await;
    let (session, mut send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["fec"], Value::Null);
    let mut path = Lossy::new();
    for _ in 0..800 {
        let d = datagram(&session).await.3;
        assert_eq!(d[0], 0x01, "only video is sent");
        path.receive(&d, &mut send).await;
    }
}

/// §Hello ack: a session with no ABR reads no feedback, so it has no loss to size FEC by
/// and names no scheme.
#[tokio::test]
async fn a_session_without_an_abr_names_no_fec_scheme() {
    let agent = agent().await;
    let session = agent.connect().await;
    let (mut send, mut recv) = session.open_bi().await.unwrap();
    let hello = json!({"type": "hello", "wt_proto": 1, "token": TOKEN,
                       "session_id": SESSION, "client": "browser", "fec": ["xor"]});
    write_message(&mut send, &hello).await.unwrap();
    assert_eq!(read_message(&mut recv).await.unwrap()["fec"], Value::Null);
}

/// `QUASAR_ABR_MODE=off` still tracks the estimate and never writes it to the encoder, so
/// nothing would pay for repairs: such a session names no scheme and sends none.
#[tokio::test]
async fn a_session_whose_abr_does_not_retarget_gets_no_fec_whatever_it_loses() {
    let agent = lossy_session(false).await;
    let (session, mut send, _recv, ack) = agent.hello_with(TOKEN, json!({"fec": ["xor"]})).await;
    assert_eq!(ack["fec"], Value::Null);
    let mut path = Lossy::new();
    for _ in 0..800 {
        let d = datagram(&session).await.3;
        assert_eq!(d[0], 0x01, "only video is sent");
        path.receive(&d, &mut send).await;
    }
}

/// §Hello: `fec` is a list. No list, an empty one or one of schemes this agent does not
/// know is a client that takes no FEC, never a refused hello.
#[tokio::test]
async fn a_hello_that_names_no_known_fec_scheme_is_served_without_fec() {
    for offer in [
        json!({}),
        json!({ "fec": null }),
        json!({"fec": []}),
        json!({"fec": ["fec2"]}),
        json!({"fec": "xor"}),
    ] {
        let agent = lossy_session(true).await;
        let (session, _send, _recv, ack) = agent.hello_with(TOKEN, offer.clone()).await;
        assert_eq!(ack["type"], "hello_ack", "{offer}");
        assert_eq!(ack["fec"], Value::Null, "{offer}");
        assert_eq!(datagram(&session).await.3[0], 0x01, "{offer}: video flows");
    }
}

/// 40 full video datagrams: 25 % of them pays for ten repairs, 4 % for one.
fn forty_fragments() -> Vec<Bytes> {
    vec![Bytes::from(vec![1; media::MAX_DATAGRAM]); 40]
}

/// #1046: the ABR's setpoint never goes under its floor, so repairs may spend only what the
/// estimate leaves above it; under that, video alone already fills the path.
#[test]
fn repairs_spend_only_what_the_estimate_leaves_above_the_abr_floor() {
    let fed = Arc::new(Mutex::new(Vec::new()));
    let seen = fed.clone();
    let rate = Rate {
        ceiling_kbps: 6000.0,
        floor_kbps: Box::new(|| 2500.0),
        retargets: true,
        start: Box::new(|| {}),
        observe: Box::new(move |kbps: f64| seen.lock().unwrap().push(kbps.round())),
    };
    let mut control = Control::new(&rate, Arc::default(), true, Default::default());
    let fec = control.fec.as_mut().unwrap();
    fec.observe(0.05, std::time::Instant::now());
    assert_eq!(fec.overhead(), 0.25);
    let frame = forty_fragments();
    let mut repairs = |estimate_kbps| {
        control.feed(estimate_kbps);
        control.protect(false, &frame, &mut 0).len()
    };
    assert_eq!(repairs(2000.0), 0, "under the floor");
    assert_eq!(repairs(2500.0), 0, "at the floor");
    assert_eq!(repairs(2600.0), 1, "4 % above the floor");
    assert_eq!(repairs(5000.0), 10, "room for all 25 %");
    assert_eq!(*fed.lock().unwrap(), [2000.0, 2500.0, 2500.0, 4000.0]);
}

/// What a session on a real smooth governor (2500 to 8000 kbit/s) sends in each of `seconds`
/// at `estimate_kbps` and FEC `level`: the setpoint, and that plus the repairs
/// `Control::protect` makes for a second of 60 fps video at it, in kbit/s. The clock is the
/// test's, one estimate a second.
fn governed(level: u8, estimate_kbps: f64, seconds: u64) -> Vec<(u32, f64)> {
    use crate::session::abr::{AbrConfig, Governor};
    let base = std::time::Instant::now();
    let governor = Arc::new(Mutex::new((
        Governor::new(AbrConfig::new_smooth(2500, 8000, 60), base),
        base,
    )));
    let (floor, observed) = (governor.clone(), governor.clone());
    let rate = Rate {
        ceiling_kbps: 8000.0,
        floor_kbps: Box::new(move || f64::from(floor.lock().unwrap().0.floor_kbps())),
        retargets: true,
        start: Box::new(|| {}),
        observe: Box::new(move |kbps| {
            let mut governor = observed.lock().unwrap();
            let now = governor.1;
            governor.0.observe(kbps, now);
        }),
    };
    let mut control = Control::new(&rate, Arc::default(), true, Default::default());
    let fec = control.fec.as_mut().unwrap();
    fec.observe(f64::from(level) / 100.0, base);
    assert_eq!(fec.overhead(), f64::from(level) * 0.05);
    (1..=seconds)
        .map(|second| {
            governor.lock().unwrap().1 = base + Duration::from_secs(second);
            control.feed(estimate_kbps);
            let setpoint = governor.lock().unwrap().0.setpoint_kbps();
            let fragments = (setpoint as usize * 125 / 60).div_ceil(media::MAX_DATAGRAM);
            let frame = vec![Bytes::from(vec![1; media::MAX_DATAGRAM]); fragments];
            let repairs: usize = (0..60)
                .flat_map(|_| control.protect(false, &frame, &mut 0))
                .map(|repair| repair.len())
                .sum();
            let video = 60 * fragments * media::MAX_DATAGRAM;
            let sent = f64::from(setpoint) * (1.0 + repairs as f64 / video as f64);
            (setpoint, sent)
        })
        .collect()
}

/// What the governor lets video alone reach before it steps down: the estimate over
/// 1 − its deadband. Video plus FEC is held to the same.
fn governor_bound(estimate_kbps: f64) -> f64 {
    estimate_kbps / (1.0 - crate::session::abr::AbrConfig::DEFAULT_DEADBAND)
}

/// #1046: a FEC share inside the governor's deadband (levels 1 to 3, up to 15 %) does not
/// move the encoder. At the ceiling the repairs go on top of it, so the session sends more
/// than its ceiling, and no more than the governor's own bound.
#[test]
fn a_fec_share_inside_the_deadband_rides_on_top_of_the_ceiling() {
    for level in 1..=3 {
        for (setpoint, sent) in governed(level, 8000.0, 30) {
            assert_eq!(setpoint, 8000, "level {level}");
            assert!(
                sent > 8000.0 && sent <= governor_bound(8000.0),
                "level {level}: {sent:.0} kbit/s"
            );
        }
    }
}

/// Levels 4 and 5 are outside the deadband: from the ceiling a smooth governor takes its
/// one 12.5 % step and holds there, the total back inside the bound.
#[test]
fn a_fec_share_outside_the_deadband_steps_the_setpoint_down() {
    for level in 4..=5 {
        for (setpoint, sent) in governed(level, 8000.0, 30) {
            assert_eq!(setpoint, 7000, "level {level}");
            assert!(
                sent > 8000.0 && sent <= governor_bound(8000.0),
                "level {level}: {sent:.0} kbit/s"
            );
        }
    }
}

/// On a link that carries less than the ceiling the governor settles (12.5 % per 7 s) where
/// video plus FEC is inside the bound video alone has (level 0), at every level.
#[test]
fn on_a_constrained_link_video_plus_fec_settles_inside_the_governors_bound() {
    for estimate_kbps in [3000.0, 5000.0, 6500.0] {
        for level in 0..=5 {
            let sent = governed(level, estimate_kbps, 90);
            let settled = &sent[80..];
            let (setpoint, _) = settled[0];
            for &(at, sent) in settled {
                assert_eq!(
                    at, setpoint,
                    "{estimate_kbps} kbit/s, level {level}: settled"
                );
                assert!(
                    sent <= governor_bound(estimate_kbps) && (level == 0 || sent > f64::from(at)),
                    "{estimate_kbps} kbit/s, level {level}: {at} + FEC = {sent:.0}"
                );
            }
        }
    }
}

/// A PyroWave frame gets no repair even at a level an earlier loss left.
#[test]
fn a_pyrowave_frame_gets_no_repair_at_any_overhead() {
    let rate = Rate {
        ceiling_kbps: 6000.0,
        floor_kbps: Box::new(|| 500.0),
        retargets: true,
        start: Box::new(|| {}),
        observe: Box::new(|_| {}),
    };
    let mut control = Control::new(&rate, Arc::default(), true, Default::default());
    let fec = control.fec.as_mut().unwrap();
    fec.observe(0.05, std::time::Instant::now());
    control.feed(6000.0);
    let frame = forty_fragments();
    assert_eq!(control.protect(false, &frame, &mut 0).len(), 10);
    assert!(control.protect(true, &frame, &mut 0).is_empty());
}

/// codec-pyrowave.md §Loss, §Feedback: a client that offered `xor` and is sent PyroWave
/// frames gets no repair datagram, whatever loss it reports. `hello_ack.fec` still names
/// the scheme: the connection may fall back to the standard codec.
#[tokio::test]
async fn a_pyrowave_client_that_offers_xor_is_sent_no_repair_whatever_it_loses() {
    let agent = lossy_session(true).await;
    let media = agent.listener.registry().lock()[SESSION]
        .media
        .clone()
        .unwrap();
    media.grant_pyrowave(
        PyroWaveGrant {
            wanted: true,
            target_kbps: 200_000,
            floor_kbps: 100_000,
            max_rtt_ms: 1_000,
        },
        (320, 192, 60),
        true,
    );
    let raw = std::fs::read(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../testdata/pyrowave/detail_320x192_xhi.pwv1"
    ))
    .unwrap();
    let offer = json!({"pyrowave": true, "fec": ["xor"]});
    let (session, mut send, _recv, ack) = agent.hello_with(TOKEN, offer).await;
    assert_eq!(ack["video"]["codec"], "pyrowave");
    assert_eq!(ack["fec"], "xor");

    // A second of frames: twice the window the estimator measures loss over.
    let mut path = Lossy::new();
    let mut video = 0;
    for _ in 0..60 {
        media.on_pyrowave_frame(&gst::Buffer::from_slice(raw[28..].to_vec()));
        while let Ok(d) =
            tokio::time::timeout(Duration::from_millis(16), session.read_datagram()).await
        {
            let d = d.unwrap();
            assert_eq!(d[0], 0x01, "only video is sent");
            video += 1;
            path.receive(&d, &mut send).await;
        }
    }
    assert!(video > 1000, "the frames arrived ({video} datagrams)");
}

#[tokio::test]
async fn a_client_flooding_nacks_gets_only_its_budget_answered() {
    let agent = agent().await;
    let (_session, mut send, mut recv, _) = agent.hello(TOKEN).await;
    // A frame the agent never held: every answered nack is one nack_miss.
    let nack = json!({"type": "nack", "frame_seq": 40_000, "frags": [[0, 0]]});
    const SENT: usize = 1000;
    let started = std::time::Instant::now();
    let write = async {
        for _ in 0..SENT {
            write_message(&mut send, &nack).await.unwrap();
        }
    };
    let read = async {
        let mut misses = 0;
        while let Ok(Ok(m)) =
            tokio::time::timeout(Duration::from_millis(500), read_message(&mut recv)).await
        {
            misses += usize::from(m["type"] == "nack_miss");
        }
        misses
    };
    let ((), misses) = tokio::join!(write, read);
    // 100 at once, then 500 a second while the flood lasted.
    let budget = 100 + (started.elapsed().as_secs_f64() * 500.0).ceil() as usize;
    assert!(misses >= 100, "the burst is answered ({misses})");
    assert!(
        misses <= budget.min(SENT - 1),
        "{misses} answered of {SENT}, budget {budget}"
    );
}

/// The job `agent-live.yml` runs under `webtransport::`: one session from hello to a clean
/// close, asserting outcomes only (what a browser would see), not the listener's internals.
#[tokio::test]
async fn a_session_runs_from_hello_through_decodable_frames_to_a_clean_close() {
    let agent = agent().await;
    let (session, mut send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack");

    let got = frames(&session, 60, false).await;
    assert!(got[0].keyframe, "the client starts at a keyframe");
    for f in &got {
        let types = nal_types(&f.data);
        assert_eq!(
            f.keyframe,
            types.contains(&5),
            "keyframe flag matches the bitstream"
        );
    }

    write_message(
        &mut send,
        &json!({"type": "bye", "reason": "client_closed"}),
    )
    .await
    .unwrap();
    assert_eq!(close_code(&session).await, Some(CLOSE_NORMAL));
}

#[tokio::test]
async fn a_session_without_audio_or_a_microphone_says_so_and_ignores_both() {
    let agent = agent().await;
    let (session, _send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(
        (ack["audio"].clone(), ack["mic"].clone()),
        (Value::Null, Value::Null)
    );
    session
        .send_datagram(Bytes::from_static(&[0x05, 0, 0, 0, 0, 0, 0, 1, 2, 3]))
        .unwrap();
    // Video alone, so the one-channel `frames` check holds.
    assert_eq!(frames(&session, 5, true).await.len(), 5);
}

#[tokio::test]
async fn before_hello_the_agent_reads_one_stream_and_waits_five_seconds() {
    let agent = agent().await;

    let session = agent.connect().await;
    let (mut control, _recv) = session.open_bi().await.unwrap();
    // Half a length prefix: the control stream is then the first the agent sees.
    let oversized = 65_537u32.to_le_bytes();
    control.write_all(&oversized[..2]).await.unwrap();
    // The CONNECT stream and the control stream are the two bidirectional streams the
    // agent allows: a third cannot be opened at all.
    assert!(
        tokio::time::timeout(Duration::from_secs(1), session.open_bi())
            .await
            .is_err(),
        "no second stream beside the control stream"
    );
    let sent = Instant::now();
    control.write_all(&oversized[2..]).await.unwrap();
    assert_eq!(
        close_code(&session).await,
        Some(CLOSE_PROTOCOL),
        "over 64 KiB"
    );
    assert!(
        sent.elapsed() < Duration::from_secs(2),
        "at the length, not the hello deadline"
    );

    let session = agent.connect().await;
    let (mut control, _recv) = session.open_bi().await.unwrap();
    write_message(&mut control, &json!({"type": "keyframe_request"}))
        .await
        .unwrap();
    assert_eq!(
        close_code(&session).await,
        Some(CLOSE_PROTOCOL),
        "not a hello"
    );

    let session = agent.connect().await;
    let opened = Instant::now();
    session
        .send_datagram(Bytes::from_static(&[0x03, b'{', b'}']))
        .unwrap();
    assert_eq!(close_code(&session).await, Some(CLOSE_PROTOCOL), "no hello");
    assert!(opened.elapsed() >= Duration::from_millis(4900));
}

/// Waits for the agent's devices to have been given `n` events.
async fn input_events(agent: &Agent, n: usize) -> Vec<String> {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        let seen = agent.input.lock().unwrap().clone();
        if seen.len() >= n || Instant::now() > deadline {
            return seen;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

#[tokio::test]
async fn input_arrives_by_datagram_and_control_stream_only_after_hello_ack() {
    let agent = agent().await;
    let session = agent.connect().await;
    // Before `hello_ack` every datagram is dropped.
    session
        .send_datagram(Bytes::from_static(
            b"\x03{\"t\":\"k\",\"code\":1,\"pressed\":true}",
        ))
        .unwrap();
    tokio::time::sleep(Duration::from_millis(200)).await;
    let (mut send, mut recv) = session.open_bi().await.unwrap();
    let hello = json!({"type": "hello", "wt_proto": 1, "token": TOKEN,
                       "session_id": SESSION, "client": "browser", "fec": []});
    write_message(&mut send, &hello).await.unwrap();
    assert_eq!(read_message(&mut recv).await.unwrap()["type"], "hello_ack");

    // The input.md JSON arrives byte for byte, from a datagram and from the control stream.
    let motion = r#"{"t":"mm","dx":4,"dy":-2,"seq":17,"tc":123456.7}"#;
    session
        .send_datagram(Bytes::from([&[0x03u8][..], motion.as_bytes()].concat()))
        .unwrap();
    // Another channel's id, and one nobody knows, are dropped.
    session
        .send_datagram(Bytes::from_static(
            b"\x01{\"t\":\"k\",\"code\":2,\"pressed\":true}",
        ))
        .unwrap();
    session
        .send_datagram(Bytes::from_static(
            b"\x7f{\"t\":\"k\",\"code\":3,\"pressed\":true}",
        ))
        .unwrap();
    let key = json!({"t": "k", "code": 30, "pressed": false});
    write_message(&mut send, &json!({"type": "input", "event": key}))
        .await
        .unwrap();
    let seen = input_events(&agent, 2).await;
    tokio::time::sleep(Duration::from_millis(100)).await;
    let seen_all = agent.input.lock().unwrap().clone();
    assert_eq!(seen_all.len(), 2, "only the two events: {seen_all:?}");
    assert!(seen.contains(&motion.to_string()), "{seen:?}");
    assert!(
        seen_all
            .iter()
            .any(|e| serde_json::from_str::<Value>(e).unwrap() == key),
        "{seen_all:?}"
    );

    // Ping is answered at once with the ping's fields and the agent's clocks.
    let wall = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap()
        .as_secs_f64()
        * 1000.0;
    write_message(
        &mut send,
        &json!({"type": "ping", "id": 17, "tc": 123456.7}),
    )
    .await
    .unwrap();
    let pong = read_message(&mut recv).await.unwrap();
    assert_eq!(
        (&pong["type"], &pong["id"], &pong["tc"]),
        (&json!("pong"), &json!(17), &json!(123456.7))
    );
    assert!(pong["ts"].as_f64().unwrap() >= 0.0);
    assert!(
        (pong["tw"].as_f64().unwrap() - wall).abs() < 5_000.0,
        "tw is Unix ms: {pong}"
    );

    // A departing client's held inputs are lifted.
    assert_eq!(agent.released.load(std::sync::atomic::Ordering::SeqCst), 0);
    write_message(
        &mut send,
        &json!({"type": "bye", "reason": "client_closed"}),
    )
    .await
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while agent.released.load(std::sync::atomic::Ordering::SeqCst) == 0 {
        assert!(Instant::now() < deadline, "release_all never ran");
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

#[tokio::test]
async fn a_datagram_gamepad_state_moves_axes_and_never_changes_buttons() {
    let agent = agent().await;
    let (session, mut send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack");
    let gp = |i: u32, buttons: [u8; 2], axes: [f64; 2]| json!({"t": "gp", "i": i, "buttons": buttons, "axes": axes});
    let datagram =
        |event: &Value| Bytes::from([&[CHANNEL_INPUT][..], event.to_string().as_bytes()].concat());

    // A reliable press, then a stale datagram from before it: old buttons, new stick.
    write_message(
        &mut send,
        &json!({"type": "input", "event": gp(0, [1, 0], [0.0, 0.0])}),
    )
    .await
    .unwrap();
    assert_eq!(input_events(&agent, 1).await.len(), 1);
    session
        .send_datagram(datagram(&gp(0, [0, 0], [0.5, -0.5])))
        .unwrap();
    // A pad no reliable state has established yet has no buttons to keep: dropped.
    session
        .send_datagram(datagram(&gp(1, [1, 1], [0.9, 0.9])))
        .unwrap();
    assert_eq!(input_events(&agent, 2).await.len(), 2);
    // The release is a reliable event and does change them.
    write_message(
        &mut send,
        &json!({"type": "input", "event": gp(0, [0, 0], [0.5, -0.5])}),
    )
    .await
    .unwrap();
    assert_eq!(input_events(&agent, 3).await.len(), 3);
    tokio::time::sleep(Duration::from_millis(100)).await;

    let seen: Vec<Value> = agent
        .input
        .lock()
        .unwrap()
        .iter()
        .map(|e| serde_json::from_str(e).unwrap())
        .collect();
    assert_eq!(
        seen,
        vec![
            gp(0, [1, 0], [0.0, 0.0]),
            gp(0, [1, 0], [0.5, -0.5]),
            gp(0, [0, 0], [0.5, -0.5])
        ],
        "the stale datagram moved the stick and left the button held"
    );
}

#[test]
fn only_the_attached_clients_input_reaches_the_devices() {
    use std::sync::atomic::Ordering::SeqCst;
    let (input, events, released) = recorded();
    let media = ready_media_with(input);
    let key =
        |code: u32, pressed: bool| json!({"t": "k", "code": code, "pressed": pressed}).to_string();
    let neutral = json!({"t": "gp", "i": 0, "buttons": [0, 0], "axes": [0.0, 0.0]}).to_string();

    let old = media.attach(false, Duration::ZERO).unwrap();
    media.input_event(old.id, &key(30, true), false);
    assert_eq!(events.lock().unwrap().len(), 1);

    // A takeover lifts what the old client held as the new one attaches, before it can send.
    let new = media.attach(false, Duration::ZERO).unwrap();
    assert_eq!(released.load(SeqCst), 1);
    // The old tab's late input, its neutral pad state included, reaches nothing.
    media.input_event(old.id, &neutral, true);
    media.input_event(old.id, &key(31, true), false);
    let delivered = events.lock().unwrap().clone();
    assert_eq!(delivered.len(), 1, "{delivered:?}");
    media.input_event(new.id, &key(32, true), false);
    assert_eq!(events.lock().unwrap().len(), 2);

    // The old client's cleanup does not lift the new client's key.
    media.detach(old.id);
    assert_eq!(released.load(SeqCst), 1);
    media.input_event(new.id, &key(32, false), false);
    assert_eq!(events.lock().unwrap().len(), 3);

    // The attached client leaving does, and nothing it sends after that is taken.
    media.detach(new.id);
    assert_eq!(released.load(SeqCst), 2);
    media.input_event(new.id, &key(33, true), false);
    assert_eq!(events.lock().unwrap().len(), 3);
}

#[test]
fn a_gamepad_slot_beyond_the_api_limit_is_dropped() {
    let (input, events, _) = recorded();
    let media = ready_media_with(input);
    let id = media.attach(false, Duration::ZERO).unwrap().id;
    let gp = |i: u32| json!({"t": "gp", "i": i, "buttons": [0], "axes": [0.0]}).to_string();
    for i in [4, 1000, u32::MAX] {
        media.input_event(id, &gp(i), false);
        media.input_event(id, &gp(i), true);
    }
    assert!(events.lock().unwrap().is_empty());
    media.input_event(id, &gp(3), false);
    assert_eq!(events.lock().unwrap().len(), 1);
    assert_eq!(media.remembered_pads(), 1, "one pad remembered");
}

#[tokio::test]
async fn a_takeover_lifts_the_old_clients_keys_once_and_keeps_the_new_clients() {
    use std::sync::atomic::Ordering::SeqCst;
    let agent = agent().await;
    let registry = agent.listener.registry();
    let key =
        |code: u32| json!({"type": "input", "event": {"t": "k", "code": code, "pressed": true}});

    let (first, mut first_send, _first_recv, _) = agent.hello(TOKEN).await;
    write_message(&mut first_send, &key(30)).await.unwrap();
    assert_eq!(input_events(&agent, 1).await.len(), 1);

    assert!(registry.add_token(
        SESSION,
        &token_hash("reconnect"),
        SystemTime::now() + TOKEN_TTL
    ));
    let (_second, mut second_send, _second_recv, ack) = agent.hello("reconnect").await;
    assert_eq!(ack["type"], "hello_ack");
    assert_eq!(
        agent.released.load(SeqCst),
        1,
        "lifted before the new client's ack"
    );

    // The old tab, still connected for a moment, sends more; the new client presses a key.
    let late = Bytes::from_static(b"\x03{\"t\":\"k\",\"code\":31,\"pressed\":true}");
    let _ = first.send_datagram(late);
    let _ = write_message(&mut first_send, &key(31)).await;
    write_message(&mut second_send, &key(32)).await.unwrap();
    assert_eq!(input_events(&agent, 2).await.len(), 2);
    assert_eq!(close_code(&first).await, Some(CLOSE_TAKEN_OVER));
    tokio::time::sleep(Duration::from_millis(300)).await;
    let seen: Vec<Value> = agent
        .input
        .lock()
        .unwrap()
        .iter()
        .map(|e| serde_json::from_str(e).unwrap())
        .collect();
    assert_eq!(
        seen.iter()
            .map(|e| e["code"].as_u64().unwrap())
            .collect::<Vec<_>>(),
        [30, 32],
        "{seen:?}"
    );
    assert_eq!(
        agent.released.load(SeqCst),
        1,
        "the old client's cleanup left the new client's key alone"
    );

    write_message(
        &mut second_send,
        &json!({"type": "bye", "reason": "client_closed"}),
    )
    .await
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while agent.released.load(SeqCst) < 2 {
        assert!(
            Instant::now() < deadline,
            "the new client's keys were never lifted"
        );
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

#[tokio::test]
async fn a_client_silent_on_the_control_stream_for_10_s_is_gone_and_its_inputs_lifted() {
    let agent = agent().await;
    let (session, _send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack");
    let tap = agent.tap.as_ref().unwrap();
    assert!(tap.attached());
    let silent = Instant::now();
    close_code_within(&session, Duration::from_secs(14)).await;
    assert!(
        silent.elapsed() >= Duration::from_millis(9_900),
        "{:?}",
        silent.elapsed()
    );
    assert_eq!(agent.released.load(std::sync::atomic::Ordering::SeqCst), 1);
    // The idle reap reads the same client field this close clears (#1179).
    assert!(
        !tap.attached(),
        "a client closed for silence no longer counts"
    );
}

#[tokio::test]
async fn the_newest_client_takes_over_and_an_ended_session_says_so() {
    let agent = agent().await;
    let registry = agent.listener.registry();
    let expires = SystemTime::now() + TOKEN_TTL;

    let (first, _send, mut first_recv, _) = agent.hello(TOKEN).await;
    assert!(registry.add_token(SESSION, &token_hash("reconnect"), expires));
    let (second, _send2, mut second_recv, ack) = agent.hello("reconnect").await;
    assert_eq!(ack["type"], "hello_ack");
    assert_eq!(
        read_message(&mut first_recv).await.unwrap(),
        json!({"type": "bye", "reason": "taken_over"})
    );
    assert_eq!(close_code(&first).await, Some(CLOSE_TAKEN_OVER));
    assert!(frames(&second, 1, true).await[0].keyframe);

    assert!(registry.add_token(SESSION, &token_hash("too-late"), expires));
    registry.end(SESSION);
    assert_eq!(
        read_message(&mut second_recv).await.unwrap(),
        json!({"type": "bye", "reason": "session_ended"})
    );
    assert_eq!(close_code(&second).await, Some(CLOSE_ENDED));
    let (late, _send, _recv, reply) = agent.hello("too-late").await;
    assert_eq!(reply["code"], "session_ended");
    assert_eq!(close_code(&late).await, Some(CLOSE_ENDED));
}

/// The agent's half of the WebTransport → WebRTC fallback (control-api.md §Transport fallback,
/// "Close first"): the client that falls back says `bye` while it holds a key. The agent closes
/// normally, lifts what it held, counts no client, and the session is still there for the
/// WebRTC peer that follows (and for a later WebTransport client with a fresh token).
#[tokio::test]
async fn a_client_that_falls_back_leaves_no_client_no_held_key_and_a_live_session() {
    let agent = agent().await;
    let tap = agent.tap.as_ref().unwrap();
    let (session, mut send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack");
    frames(&session, 1, true).await;
    session
        .send_datagram(Bytes::from_static(
            b"\x03{\"t\":\"k\",\"code\":17,\"pressed\":true}",
        ))
        .unwrap();
    assert_eq!(input_events(&agent, 1).await.len(), 1, "the key is held");
    assert_eq!(agent.released.load(std::sync::atomic::Ordering::SeqCst), 0);

    write_message(
        &mut send,
        &json!({"type": "bye", "reason": "client_closed"}),
    )
    .await
    .unwrap();
    assert_eq!(close_code(&session).await, Some(CLOSE_NORMAL));
    let deadline = Instant::now() + Duration::from_secs(5);
    while tap.attached() || agent.released.load(std::sync::atomic::Ordering::SeqCst) == 0 {
        assert!(
            Instant::now() < deadline,
            "the client that fell back still counts, or its key is still held"
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }

    // Not ended: a fresh token for the same session still gets a `hello_ack` and frames.
    assert!(agent.listener.registry().add_token(
        SESSION,
        &token_hash("after-fallback"),
        SystemTime::now() + TOKEN_TTL
    ));
    let (again, _send, _recv, ack) = agent.hello("after-fallback").await;
    assert_eq!(ack["type"], "hello_ack");
    assert!(frames(&again, 1, true).await[0].keyframe);
}

/// What the session's idle reap reads (#1179): a client counts from its `hello_ack` while it
/// streams, across a takeover, and stops counting once it leaves.
#[tokio::test]
async fn the_tap_counts_a_streaming_client_until_it_leaves() {
    let agent = agent().await;
    let tap = agent.tap.as_ref().unwrap();
    assert!(!tap.attached(), "no client has said hello");

    let (first, _send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack");
    frames(&first, 1, true).await;
    assert!(tap.attached());

    let expires = SystemTime::now() + TOKEN_TTL;
    assert!(agent
        .listener
        .registry()
        .add_token(SESSION, &token_hash("reconnect"), expires));
    let (second, mut send, _recv2, _) = agent.hello("reconnect").await;
    assert_eq!(close_code(&first).await, Some(CLOSE_TAKEN_OVER));
    frames(&second, 1, true).await;
    assert!(tap.attached(), "the client that took over counts");

    write_message(
        &mut send,
        &json!({"type": "bye", "reason": "client_closed"}),
    )
    .await
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while tap.attached() {
        assert!(Instant::now() < deadline, "a client that left still counts");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

fn key_input(code: u32) -> Value {
    json!({"type": "input", "event": {"t": "k", "code": code, "pressed": true}})
}

/// The video transport ids these tests give the WebRTC side.
const T1: u64 = 101;
const T2: u64 = 102;

fn webrtc(agent: &Agent, transport: u64) -> WebRtcSide {
    WebRtcSide {
        media: agent.tap.as_ref().unwrap().media().clone(),
        transport: Some(transport),
    }
}

/// What `transport`'s data channel hands the devices, through the session's media client.
fn webrtc_key(agent: &Agent, transport: u64, code: u32) {
    let input = agent.input.clone();
    webrtc(agent, transport).input(move || {
        input
            .lock()
            .unwrap()
            .push(json!({"t": "k", "code": code, "pressed": true}).to_string())
    });
}

fn codes_seen(agent: &Agent) -> Vec<u64> {
    agent
        .input
        .lock()
        .unwrap()
        .iter()
        .map(|e| {
            serde_json::from_str::<Value>(e).unwrap()["code"]
                .as_u64()
                .unwrap()
        })
        .collect()
}

/// A WebTransport client takes the session by `token`, which is added first.
async fn take_over(agent: &Agent, token: &str) -> (Session, SendStream, RecvStream, Value) {
    let expires = SystemTime::now() + TOKEN_TTL;
    assert!(agent
        .listener
        .registry()
        .add_token(SESSION, &token_hash(token), expires));
    agent.hello(token).await
}

/// §Takeover, WebRTC takes over WebTransport: the client gets `bye taken_over` and `4410` at
/// once, what it held is lifted then, once, and nothing it sends after reaches the devices.
#[tokio::test]
async fn a_webrtc_peer_takes_over_the_webtransport_client() {
    use std::sync::atomic::Ordering::SeqCst;
    let agent = agent().await;
    let tap = agent.tap.as_ref().unwrap();
    let (session, mut send, mut recv, _) = agent.hello(TOKEN).await;
    write_message(&mut send, &key_input(30)).await.unwrap();
    assert_eq!(input_events(&agent, 1).await.len(), 1);

    assert!(tap.media().webrtc_attached(T1), "a client was attached");
    assert_eq!(agent.released.load(SeqCst), 1, "lifted at the takeover");
    assert!(!tap.attached(), "the idle reap no longer counts it");
    let _ = write_message(&mut send, &key_input(31)).await;
    webrtc_key(&agent, T2, 33);
    webrtc_key(&agent, T1, 32);
    assert_eq!(
        read_message(&mut recv).await.unwrap(),
        json!({"type": "bye", "reason": "taken_over"})
    );
    assert_eq!(close_code(&session).await, Some(CLOSE_TAKEN_OVER));
    assert_eq!(
        codes_seen(&agent),
        [30, 32],
        "the old client's late key and another transport's are dropped"
    );
    assert_eq!(
        agent.released.load(SeqCst),
        1,
        "its own close lifted nothing of the WebRTC peer's"
    );
    assert_eq!(tap.media().take_to_mute(), None, "nothing to stop");
}

/// §Takeover, WebTransport takes over WebRTC: the peer's key is lifted before the client's
/// `hello_ack`, the runner is told once which transport to stop, and that peer's late input
/// and its disconnect reach nothing of the client's.
#[tokio::test]
async fn a_webtransport_client_takes_over_the_webrtc_peer() {
    use std::sync::atomic::Ordering::SeqCst;
    let agent = agent().await;
    let media = agent.tap.as_ref().unwrap().media().clone();
    webrtc_key(&agent, T1, 1);
    assert!(
        agent.input.lock().unwrap().is_empty(),
        "no WebRTC peer accepted yet"
    );
    assert!(!media.webrtc_attached(T1), "no client to displace");
    webrtc_key(&agent, T1, 30);
    assert_eq!(media.take_to_mute(), None);

    let (session, mut send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack");
    assert_eq!(agent.released.load(SeqCst), 1, "lifted before the ack");
    assert_eq!(
        media.take_to_mute(),
        Some(T1),
        "the runner stops that transport"
    );
    assert_eq!(media.take_to_mute(), None, "once");

    webrtc_key(&agent, T1, 31);
    webrtc(&agent, T1).disconnected(true);
    write_message(&mut send, &key_input(32)).await.unwrap();
    assert_eq!(input_events(&agent, 2).await.len(), 2);
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(codes_seen(&agent), [30, 32]);
    assert_eq!(
        agent.released.load(SeqCst),
        1,
        "the peer's disconnect lifted the client's key"
    );
    assert!(frames(&session, 1, true).await[0].keyframe);
}

/// #1193: the host missed the WebTransport close on a fallback. The WebRTC attach displaces the
/// client then, so its control-stream silence 10 s later lifts nothing the WebRTC peer holds.
#[tokio::test]
async fn a_key_held_over_webrtc_after_a_fallback_without_a_webtransport_close_stays_held() {
    use std::sync::atomic::Ordering::SeqCst;
    let agent = agent().await;
    let media = agent.tap.as_ref().unwrap().media().clone();
    // Never closed by the client, and silent from here on.
    let (session, mut send, _recv, _) = agent.hello(TOKEN).await;
    write_message(&mut send, &key_input(30)).await.unwrap();
    assert_eq!(input_events(&agent, 1).await.len(), 1);

    media.webrtc_attached(T1);
    webrtc_key(&agent, T1, 30);
    assert_eq!(codes_seen(&agent), [30, 30], "held again over WebRTC");
    let released = agent.released.load(SeqCst);
    tokio::time::sleep(CONTROL_IDLE + Duration::from_secs(1)).await;
    assert_eq!(
        agent.released.load(SeqCst),
        released,
        "a release after the WebRTC press lifted the key the WebRTC peer holds"
    );
    assert_eq!(
        released, 1,
        "the client's key was lifted once, at the attach"
    );
    assert_eq!(close_code(&session).await, Some(CLOSE_TAKEN_OVER));
}

/// §Authentication and tokens, "Reconnect": a reload's fresh token resumes the same session
/// with nothing restarted (the same decoder configuration, frame numbers carrying on), from a
/// keyframe.
#[tokio::test]
async fn a_reload_reconnects_with_a_fresh_token_and_resumes_from_a_keyframe() {
    let agent = agent().await;
    let tap = agent.tap.as_ref().unwrap();
    let (first, mut send, _recv, ack) = agent.hello(TOKEN).await;
    let last = frames(&first, 5, true).await.last().unwrap().frame_seq;
    write_message(
        &mut send,
        &json!({"type": "bye", "reason": "client_closed"}),
    )
    .await
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while tap.attached() {
        assert!(Instant::now() < deadline, "the reloaded page never left");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }

    let (second, _send, _recv, again) = take_over(&agent, "reload").await;
    assert_eq!(again, ack, "the same session's configuration");
    let next = &frames(&second, 1, true).await[0];
    assert!(next.keyframe, "the reconnected client starts at a keyframe");
    assert!(
        next.frame_seq > last,
        "frame numbers carry on: {} after {last}",
        next.frame_seq
    );
}

/// §Takeover's bound (erratum 28): 6 takeovers of a session in any 60 s, counted across both
/// transports. A 7th WebTransport `hello` is refused, keeping its token, and the connected
/// client streams on. A WebRTC answer while a WebTransport client holds is counted and never
/// refused; a new WebRTC peer displacing the WebRTC holder is refused, on both PCs.
#[tokio::test]
async fn a_seventh_takeover_inside_60_s_is_refused_on_either_transport() {
    let agent = agent().await;
    let registry = agent.listener.registry();
    let tap = agent.tap.as_ref().unwrap();
    let media = tap.media();
    // Nobody to displace: no takeover.
    let mut clients = vec![agent.hello(TOKEN).await];
    for n in 0..6u64 {
        if n % 2 == 0 {
            assert!(media.webrtc_attached(T1 + n), "takeover {n} is WebRTC's");
            continue;
        }
        let client = take_over(&agent, &format!("takeover-{n}")).await;
        assert_eq!(
            client.3["type"], "hello_ack",
            "takeover {n} is WebTransport's"
        );
        clients.push(client);
    }

    let (refused, _send, _recv, reply) = take_over(&agent, "seventh").await;
    assert_eq!(reply["code"], "takeover_rate_limited", "{reply}");
    assert_eq!(close_code(&refused).await, Some(CLOSE_NORMAL));
    assert!(
        registry.lock()[SESSION]
            .tokens
            .iter()
            .any(|t| t.hash == token_hash("seventh")),
        "a refused hello keeps its token"
    );
    let streaming = &clients.last().unwrap().0;
    frames(streaming, 1, false).await;
    assert!(tap.attached(), "the connected client keeps streaming");

    assert!(media.webrtc_attached(T2 + 10), "over the bound, and let in");
    let mut asked = false;
    assert!(
        !media.webrtc_replace(T2 + 10, || {
            asked = true;
            true
        }),
        "a new peer displacing the WebRTC holder is refused"
    );
    assert!(!asked, "the PC's own budget was not spent");
    assert!(media.webrtc_refuses(T2 + 10), "and on its audio PC");
}

/// Seat B F1: a page that streamed over WebRTC, then over WebTransport through six reloads
/// that each displaced the last tab, falls back to WebRTC. Its answer on the old transport is a
/// new peer's, and it displaces a WebTransport client, not the WebRTC holder: let in, given a
/// transport of its own, and counted.
#[tokio::test]
async fn a_fallback_after_six_reloads_is_not_refused() {
    let agent = agent().await;
    let media = agent.tap.as_ref().unwrap().media().clone();
    assert!(
        !media.webrtc_attached(T1),
        "the page first streams over WebRTC"
    );
    let mut clients = Vec::new();
    for n in 0..6 {
        let client = take_over(&agent, &format!("reload-{n}")).await;
        assert_eq!(client.3["type"], "hello_ack");
        clients.push(client);
    }
    let mut asked = false;
    assert!(
        media.webrtc_replace(T1, || {
            asked = true;
            true
        }),
        "the falling-back page gets a transport"
    );
    assert!(asked, "only the PC's own budget decides");
    assert!(
        media.webrtc_attached(T2),
        "its answer displaces the WebTransport client"
    );
    webrtc_key(&agent, T2, 30);
    assert_eq!(
        codes_seen(&agent),
        [30],
        "the falling-back page holds the session"
    );
}

/// Seat B F3: a WebRTC peer whose PC failed or closed holds nothing. A WebTransport client
/// after it displaces nobody: it is not counted, so not refused, and no transport is stopped.
#[tokio::test]
async fn a_webtransport_hello_after_the_webrtc_tab_closed_is_not_counted() {
    let agent = agent().await;
    let media = agent.tap.as_ref().unwrap().media().clone();
    let mut clients = vec![agent.hello(TOKEN).await];
    for n in 0..5 {
        clients.push(take_over(&agent, &format!("reload-{n}")).await);
    }
    assert!(media.webrtc_attached(T1), "the 6th takeover");
    webrtc(&agent, T1).disconnected(false);
    assert_eq!(
        media.holder(),
        media::Holder::WebRtc(T1),
        "a transient disconnect keeps it"
    );
    webrtc(&agent, T1).disconnected(true);

    let (_session, _send, _recv, reply) = take_over(&agent, "after-the-tab").await;
    assert_eq!(reply["type"], "hello_ack", "{reply}");
    assert_eq!(media.take_to_mute(), None, "nobody to stop or tell");
}

/// Two takeovers at once, a WebTransport `hello` and a WebRTC first answer, end with one
/// holder, and exactly one client is displaced: the one that lost.
#[test]
fn a_hello_racing_a_webrtc_first_answer_leaves_one_holder_and_one_displaced() {
    for _ in 0..500 {
        let media = ready_media();
        let start = Arc::new(std::sync::Barrier::new(2));
        let wt = {
            let (media, start) = (media.clone(), start.clone());
            std::thread::spawn(move || {
                start.wait();
                media.attach(false, Duration::ZERO).unwrap()
            })
        };
        let rtc = {
            let (media, start) = (media.clone(), start.clone());
            std::thread::spawn(move || {
                start.wait();
                media.webrtc_attached(T1)
            })
        };
        let wt = wt.join().unwrap();
        let displaced_wt = rtc.join().unwrap();
        let displaced_rtc = media.take_to_mute() == Some(T1);
        assert_eq!(
            displaced_wt,
            wt.gone.get().is_some(),
            "what the WebRTC attach reports is what happened to the client"
        );
        assert!(
            displaced_wt ^ displaced_rtc,
            "exactly one is displaced: client {displaced_wt}, peer {displaced_rtc}"
        );
        let holder = media.holder();
        assert_eq!(
            holder,
            if displaced_wt {
                media::Holder::WebRtc(T1)
            } else {
                media::Holder::Wt(wt.id)
            }
        );
    }
}

#[tokio::test]
async fn past_64_unauthenticated_connections_new_sessions_are_refused() {
    let agent = agent_with(Limits {
        per_source: 0,
        ..Limits::default()
    })
    .await;
    let mut held = Vec::new();
    for _ in 0..MAX_UNAUTHENTICATED {
        held.push(agent.connect().await);
    }
    let refused = ClientBuilder::new()
        .with_server_certificate_hashes(vec![agent.hash.clone()])
        .unwrap()
        .connect(agent.url.clone())
        .await;
    assert!(refused.is_err(), "the 65th session is refused");

    held.pop().unwrap().close(0, b"");
    tokio::time::sleep(Duration::from_millis(200)).await;
    let (_session, _send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack", "a freed slot takes a new client");
}

#[test]
fn capacity_lists_webtransport_only_with_a_listener_and_reports_the_endpoint() {
    let report = Endpoint {
        port: 4433,
        addresses: vec!["192.0.2.10".into()],
        protos: vec![1],
        certificates: Vec::new(),
    };
    let json = serde_json::to_value(Transports {
        transports: vec!["webrtc", "webtransport"],
        webtransport: Some(report),
    })
    .unwrap();
    assert_eq!(
        json,
        json!({"transports": ["webrtc", "webtransport"],
               "webtransport": {"port": 4433, "addresses": ["192.0.2.10"], "protos": [1],
                                "certificates": []}})
    );
    assert_eq!(
        serde_json::to_value(capacity()).unwrap(),
        json!({"transports": ["webrtc"]}),
        "no listener in this process"
    );
    assert!(!reachable(&"127.0.0.1".parse().unwrap()));
    assert!(!reachable(&"169.254.1.1".parse().unwrap()));
    assert!(!reachable(&"fe80::1".parse().unwrap()));
    assert!(reachable(&"192.0.2.10".parse().unwrap()));
    assert!(reachable(&"2001:db8::10".parse().unwrap()));
}

/// The same client against a live agent, with what a launch response hands a browser:
/// `QUASAR_WT_URL`, `QUASAR_WT_TOKEN`, `QUASAR_WT_SESSION` and `QUASAR_WT_CERT_HASHES`
/// (comma-separated base64). Run with `--ignored`.
#[tokio::test]
#[ignore = "needs a live agent and a minted transport token"]
async fn a_live_agent_streams_h264_to_the_conformance_client() {
    let var = |k: &str| std::env::var(k).unwrap_or_else(|_| panic!("{k} is unset"));
    let hashes = var("QUASAR_WT_CERT_HASHES")
        .split(',')
        .map(|h| {
            base64::engine::general_purpose::STANDARD
                .decode(h.trim())
                .unwrap()
        })
        .collect();
    let session = ClientBuilder::new()
        .with_server_certificate_hashes(hashes)
        .unwrap()
        .connect(var("QUASAR_WT_URL").parse::<url::Url>().unwrap())
        .await
        .unwrap();
    let (mut send, mut recv) = session.open_bi().await.unwrap();
    let hello = json!({"type": "hello", "wt_proto": 1, "token": var("QUASAR_WT_TOKEN"),
                       "session_id": var("QUASAR_WT_SESSION"), "client": "native"});
    write_message(&mut send, &hello).await.unwrap();
    let ack = read_message(&mut recv).await.unwrap();
    assert_eq!(ack["type"], "hello_ack", "{ack}");
    let got = frames(&session, 300, false).await;
    assert!(got[0].keyframe && nal_types(&got[0].data).contains(&5));
    for pair in got.windows(2) {
        assert!(
            pair[1].capture_us >= pair[0].capture_us,
            "capture time rises"
        );
    }
    for f in &got {
        nal_types(&f.data);
    }
    let span_us = got.last().unwrap().capture_us - got[0].capture_us;
    eprintln!(
        "live: {}x{}@{} config_id={} {} units over {:.2} s ({:.1} fps), {} keyframes, {} bytes",
        ack["video"]["width"],
        ack["video"]["height"],
        ack["video"]["fps"],
        ack["config_id"],
        got.len(),
        span_us as f64 / 1e6,
        (got.len() - 1) as f64 / (span_us as f64 / 1e6),
        got.iter().filter(|f| f.keyframe).count(),
        got.iter().map(|f| f.data.len()).sum::<usize>()
    );
    write_message(
        &mut send,
        &json!({"type": "bye", "reason": "client_closed"}),
    )
    .await
    .unwrap();
}

/// A QUIC peer that completes the handshake and then sends nothing: no HTTP/3 settings,
/// no CONNECT. It takes any certificate.
#[derive(Debug)]
struct AnyCert(Arc<rustls::crypto::CryptoProvider>);

impl rustls::client::danger::ServerCertVerifier for AnyCert {
    fn verify_server_cert(
        &self,
        _: &rustls_pki_types::CertificateDer<'_>,
        _: &[rustls_pki_types::CertificateDer<'_>],
        _: &rustls_pki_types::ServerName<'_>,
        _: &[u8],
        _: rustls_pki_types::UnixTime,
    ) -> Result<rustls::client::danger::ServerCertVerified, rustls::Error> {
        Ok(rustls::client::danger::ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &rustls_pki_types::CertificateDer<'_>,
        dss: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &self.0.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &rustls_pki_types::CertificateDer<'_>,
        dss: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &self.0.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
        self.0.signature_verification_algorithms.supported_schemes()
    }
}

fn raw_endpoint() -> quinn::Endpoint {
    raw_endpoint_with(|_| {})
}

fn raw_endpoint_with(shape: impl FnOnce(&mut quinn::TransportConfig)) -> quinn::Endpoint {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let mut tls = rustls::ClientConfig::builder_with_provider(provider.clone())
        .with_protocol_versions(&[&rustls::version::TLS13])
        .unwrap()
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(AnyCert(provider)))
        .with_no_client_auth();
    tls.alpn_protocols = vec![web_transport_quinn::ALPN.as_bytes().to_vec()];
    let crypto = quinn::crypto::rustls::QuicClientConfig::try_from(tls).unwrap();
    let mut endpoint = quinn::Endpoint::client("127.0.0.1:0".parse().unwrap()).unwrap();
    let mut transport = quinn::TransportConfig::default();
    shape(&mut transport);
    let mut config = quinn::ClientConfig::new(Arc::new(crypto));
    config.transport_config(Arc::new(transport));
    endpoint.set_default_client_config(config);
    endpoint
}

/// Open `n` raw QUIC connections at once; the ones the agent let through.
async fn raw_connections(
    agent: &Agent,
    endpoint: &quinn::Endpoint,
    n: usize,
) -> Vec<quinn::Connection> {
    let addr = SocketAddr::from(([127, 0, 0, 1], agent.url.port().unwrap()));
    let attempts =
        (0..n).map(|_| async move { endpoint.connect(addr, "quasar-node-agent").unwrap().await });
    futures_util::future::join_all(attempts)
        .await
        .into_iter()
        .filter_map(Result::ok)
        .collect()
}

#[tokio::test]
async fn peers_that_never_send_connect_hold_a_slot_until_the_deadline_and_never_pass_the_cap() {
    let agent = agent_with(Limits {
        per_source: 0,
        ..Limits::default()
    })
    .await;
    let endpoint = raw_endpoint();
    let started = Instant::now();
    let held = raw_connections(&agent, &endpoint, 200).await;
    assert!(
        started.elapsed() < HELLO_DEADLINE,
        "the burst must fit inside one deadline for the count to mean anything"
    );
    assert_eq!(
        held.len(),
        MAX_UNAUTHENTICATED,
        "exactly the cap got through"
    );
    assert!(
        ClientBuilder::new()
            .with_server_certificate_hashes(vec![agent.hash.clone()])
            .unwrap()
            .connect(agent.url.clone())
            .await
            .is_err(),
        "a browser is refused while silent peers hold every slot"
    );
    for conn in &held {
        assert!(matches!(
            tokio::time::timeout(Duration::from_secs(10), conn.closed()).await,
            Ok(quinn::ConnectionError::ApplicationClosed(_))
        ));
    }
    let closed_after = started.elapsed();
    assert!(
        closed_after >= Duration::from_millis(4500) && closed_after < Duration::from_secs(8),
        "closed at the hello deadline, after {closed_after:?}"
    );
    let (_session, _send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack", "the slots came back");
}

#[tokio::test]
async fn one_source_holds_at_most_its_share_of_the_slots() {
    let agent = agent_with(Limits {
        unauthenticated: MAX_UNAUTHENTICATED,
        per_source: 4,
    })
    .await;
    let endpoint = raw_endpoint();
    let held = raw_connections(&agent, &endpoint, 6).await;
    assert_eq!(held.len(), 4);
}

#[tokio::test]
async fn streams_before_the_control_stream_are_stopped_and_the_hello_still_lands() {
    let agent = agent().await;
    let session = agent.connect().await;
    for i in 0..20 {
        let mut uni = tokio::time::timeout(Duration::from_secs(5), session.open_uni())
            .await
            .unwrap_or_else(|_| panic!("stream {i} was never given credit: refused streams leak"))
            .unwrap();
        let _ = uni.write_all(&[0u8; 4096]).await;
        assert!(
            tokio::time::timeout(Duration::from_secs(5), uni.stopped())
                .await
                .is_ok_and(|stopped| matches!(stopped, Ok(Some(_)))),
            "stream {i} got STOP_SENDING"
        );
    }
    let (mut send, mut recv) = session.open_bi().await.unwrap();
    let hello = json!({"type": "hello", "wt_proto": 1, "token": TOKEN, "session_id": SESSION});
    write_message(&mut send, &hello).await.unwrap();
    assert_eq!(read_message(&mut recv).await.unwrap()["type"], "hello_ack");
}

#[test]
fn two_hellos_racing_one_token_redeem_it_once() {
    for _ in 0..200 {
        let registry = Arc::new(Registry::default());
        registry.assign(SESSION, &token_hash(TOKEN), SystemTime::now() + TOKEN_TTL);
        assert!(registry.set_media(SESSION, ready_media()));
        let start = Arc::new(std::sync::Barrier::new(2));
        let racers: Vec<_> = (0..2)
            .map(|_| {
                let (registry, start) = (registry.clone(), start.clone());
                std::thread::spawn(move || {
                    start.wait();
                    registry
                        .redeem(
                            &hello_for(SESSION, TOKEN, 1),
                            SystemTime::now(),
                            Duration::ZERO,
                        )
                        .is_ok()
                })
            })
            .collect();
        let won = racers
            .into_iter()
            .map(|r| r.join().unwrap())
            .filter(|ok| *ok)
            .count();
        assert_eq!(won, 1);
    }
}

#[derive(Clone, Default)]
pub(super) struct Captured(pub(super) Arc<Mutex<Vec<u8>>>);

impl std::io::Write for Captured {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(buf);
        Ok(buf.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

impl<'a> tracing_subscriber::fmt::MakeWriter<'a> for Captured {
    type Writer = Captured;
    fn make_writer(&'a self) -> Captured {
        self.clone()
    }
}

#[tokio::test]
async fn a_64_kib_session_id_is_refused_and_logged_bounded() {
    let agent = agent().await;
    let session = agent.connect().await;
    let (mut send, mut recv) = session.open_bi().await.unwrap();
    let huge = format!("\"\n{}", "x".repeat(65_000));
    let hello = json!({"type": "hello", "wt_proto": 1, "token": TOKEN, "session_id": huge});
    write_message(&mut send, &hello).await.unwrap();
    assert_eq!(
        read_message(&mut recv).await.unwrap()["code"],
        "session_not_ready"
    );

    // The refusal's own log call, captured on this thread (a scoped subscriber does not
    // reach tasks other tests' subscribers race with).
    let captured = Captured::default();
    let subscriber = tracing_subscriber::fmt()
        .with_max_level(tracing::Level::DEBUG)
        .with_ansi(false)
        .with_writer(captured.clone())
        .finish();
    tracing::subscriber::with_default(subscriber, || {
        tracing::callsite::rebuild_interest_cache();
        log_refusal(&huge, "session_not_ready");
    });
    let logs = String::from_utf8(captured.0.lock().unwrap().clone()).unwrap();
    assert_eq!(logs.lines().count(), 1, "one line: {logs}");
    assert!(logs.len() < 400, "a {} byte log line", logs.len());
    assert!(
        logs.contains("\\\"\\n") && logs.contains(&"x".repeat(62)),
        "escaped and cut: {logs}"
    );
    assert!(
        !logs.contains(&"x".repeat(63)),
        "cut at 64 characters: {logs}"
    );
}

/// A WebTransport client on a QUIC transport the test shapes.
fn client_with(shape: impl FnOnce(&mut quinn::TransportConfig)) -> web_transport_quinn::Client {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let mut tls = rustls::ClientConfig::builder_with_provider(provider.clone())
        .with_protocol_versions(&[&rustls::version::TLS13])
        .unwrap()
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(AnyCert(provider)))
        .with_no_client_auth();
    tls.alpn_protocols = vec![web_transport_quinn::ALPN.as_bytes().to_vec()];
    let crypto = quinn::crypto::rustls::QuicClientConfig::try_from(tls).unwrap();
    let mut transport = quinn::TransportConfig::default();
    shape(&mut transport);
    let mut config = quinn::ClientConfig::new(Arc::new(crypto));
    config.transport_config(Arc::new(transport));
    let endpoint = quinn::Endpoint::client("127.0.0.1:0".parse().unwrap()).unwrap();
    web_transport_quinn::Client::new(endpoint, config)
}

/// Every stream gets 8 bytes of credit: the handshake completes (the client reads what
/// that needs, so credit flows) but it never reads its control stream, so a refusal of
/// more than 8 bytes can never be written to it.
fn stalling_client() -> web_transport_quinn::Client {
    client_with(|t| {
        t.stream_receive_window(8u32.into())
            .keep_alive_interval(Some(Duration::from_millis(500)));
    })
}

#[tokio::test]
async fn a_peer_that_grants_no_credit_gives_its_slot_back_by_the_hello_deadline() {
    let agent = agent_with(Limits {
        unauthenticated: MAX_UNAUTHENTICATED,
        per_source: 1,
    })
    .await;
    let stalled = stalling_client().connect(agent.url.clone()).await.unwrap();
    let opened = Instant::now();
    let (mut send, _recv) = stalled.open_bi().await.unwrap();
    let hello =
        json!({"type": "hello", "wt_proto": 1, "token": "not-the-token", "session_id": SESSION});
    write_message(&mut send, &hello).await.unwrap();
    tokio::time::sleep(Duration::from_millis(500)).await;
    let while_stalled = ClientBuilder::new()
        .with_server_certificate_hashes(vec![agent.hash.clone()])
        .unwrap()
        .connect(agent.url.clone())
        .await;
    assert!(
        while_stalled.is_err(),
        "a refused peer stalling its refusal still holds its source's slot"
    );
    assert_eq!(
        close_code(&stalled).await,
        Some(CLOSE_AUTH),
        "closed once the refusal write gave up"
    );
    assert!(opened.elapsed() < HELLO_DEADLINE + Duration::from_secs(2));
    // Its source's one slot is free again.
    let (_session, _send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack");
}

#[tokio::test]
async fn a_peer_without_usable_datagrams_is_closed_4400_and_keeps_its_token() {
    let agent = agent().await;
    let session = client_with(|t| {
        t.datagram_receive_buffer_size(None);
    })
    .connect(agent.url.clone())
    .await
    .unwrap();
    // The agent reads this client's datagram limit as 0.
    let (mut send, _recv) = session.open_bi().await.unwrap();
    let hello = json!({"type": "hello", "wt_proto": 1, "token": TOKEN, "session_id": SESSION});
    write_message(&mut send, &hello).await.unwrap();
    assert_eq!(close_code(&session).await, Some(CLOSE_PROTOCOL));
    let (_session, _send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack", "the token was not spent");
}

#[test]
fn an_ipv6_64_is_one_source_and_ipv4_counts_per_address() {
    let gate = Arc::new(Gate {
        limits: Limits {
            unauthenticated: MAX_UNAUTHENTICATED,
            per_source: 1,
        },
        held: Mutex::default(),
    });
    let ip = |s: &str| s.parse::<IpAddr>().unwrap();
    let first = gate.take(ip("2001:db8:0:1::1")).unwrap();
    assert!(
        gate.take(ip("2001:db8:0:1:ffff::2")).is_none(),
        "the same /64"
    );
    let _other_net = gate.take(ip("2001:db8:0:2::1")).unwrap();
    let _v4 = gate.take(ip("192.0.2.1")).unwrap();
    assert!(
        gate.take(ip("::ffff:192.0.2.1")).is_none(),
        "v4-mapped is that address"
    );
    let _next_v4 = gate.take(ip("192.0.2.2")).unwrap();
    drop(first);
    assert!(
        gate.take(ip("2001:db8:0:1::9")).is_some(),
        "the /64's slot came back"
    );
}

fn hash_of(sha: &str) -> Vec<u8> {
    base64::engine::general_purpose::STANDARD
        .decode(sha)
        .unwrap()
}

impl Agent {
    /// A handshake pinned to `hashes`, as a browser's `serverCertificateHashes` does it.
    /// The quinn client checks the hash only, never the validity dates; the 14-day and
    /// P-256 rules are tested in `cert.rs`.
    async fn pin(&self, hashes: Vec<Vec<u8>>) -> Result<Session, impl std::fmt::Debug> {
        let client = ClientBuilder::new()
            .with_server_certificate_hashes(hashes)
            .unwrap();
        tokio::time::timeout(Duration::from_secs(5), client.connect(self.url.clone()))
            .await
            .expect("the handshake ends")
    }

    fn expires(&self) -> SystemTime {
        let first = &self.listener.endpoint().certificates[0];
        OffsetDateTime::parse(&first.not_after, &Rfc3339)
            .unwrap()
            .into()
    }
}

/// Walks the listener through a certificate's life on a fake clock, with a client pinned
/// to different hashes at each step.
#[tokio::test]
async fn rotation_overlaps_hashes_keeps_live_connections_and_refuses_stale_pins() {
    let agent = agent().await;
    let h1 = agent.hash.clone();
    let expires = agent.expires();

    let (live, _send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack");
    assert_eq!(frames(&live, 3, false).await.len(), 3);

    // 48 h left: both certificates reported, the old one still presented.
    let reported = agent.listener.endpoint_at(expires - 48 * HOUR).certificates;
    assert_eq!(reported.len(), 2);
    assert_eq!(hash_of(&reported[0].sha256), h1, "current first");
    let h2 = hash_of(&reported[1].sha256);
    assert!(agent.pin(vec![h1.clone(), h2.clone()]).await.is_ok());
    assert!(
        agent.pin(vec![h2.clone()]).await.is_err(),
        "not presented yet"
    );

    // 12 h left: new connections get the next certificate; either hash connects.
    let reported = agent.listener.endpoint_at(expires - 12 * HOUR).certificates;
    assert_eq!(hash_of(&reported[0].sha256), h2, "presented first");
    assert!(agent.pin(vec![h1.clone(), h2.clone()]).await.is_ok());
    assert!(agent.pin(vec![h2.clone()]).await.is_ok());
    assert!(
        agent.pin(vec![h1.clone()]).await.is_err(),
        "the old pin alone is stale"
    );
    assert_eq!(
        frames(&live, 3, false).await.len(),
        3,
        "the live session kept its certificate"
    );

    // Expired: the old hash is no longer reported or accepted, the live session goes on.
    let reported = agent
        .listener
        .endpoint_at(expires + Duration::from_secs(1))
        .certificates;
    assert_eq!(reported.len(), 1);
    assert_eq!(hash_of(&reported[0].sha256), h2);
    assert!(agent.pin(vec![h1]).await.is_err());
    assert!(agent.pin(vec![h2]).await.is_ok());
    assert_eq!(frames(&live, 3, false).await.len(), 3);
}

/// A suspend outlasts `rotate`'s monotonic sleep: the next report, with every certificate
/// expired on the wall clock, must already carry a fresh one that is also being served.
#[tokio::test]
async fn a_report_after_every_certificate_expired_names_a_fresh_served_one() {
    let agent = agent().await;
    let expires = agent.expires();
    let both = agent.listener.endpoint_at(expires - 48 * HOUR).certificates;
    let old: Vec<Vec<u8>> = both.iter().map(|c| hash_of(&c.sha256)).collect();

    let resumed = expires + 30 * 24 * HOUR;
    let reported = agent.listener.endpoint_at(resumed).certificates;
    assert_eq!(reported.len(), 1);
    let fresh = hash_of(&reported[0].sha256);
    assert!(!old.contains(&fresh));
    assert!(agent.pin(vec![fresh]).await.is_ok());
    assert!(agent.pin(old).await.is_err());
}

/// A next certificate that cannot be loaded leaves the old one served, and first.
#[tokio::test]
async fn a_failed_swap_keeps_serving_and_listing_the_old_certificate_first() {
    let agent = agent().await;
    let h1 = agent.hash.clone();
    let expires = agent.expires();
    let broken = |now| {
        let mut cert = Cert::make(now)?;
        cert.key_der = vec![0; 8];
        Ok(cert)
    };

    let captured = Captured::default();
    let subscriber = tracing_subscriber::fmt()
        .with_max_level(tracing::Level::DEBUG)
        .with_ansi(false)
        .with_writer(captured.clone())
        .finish();
    tracing::subscriber::with_default(subscriber, || {
        tracing::callsite::rebuild_interest_cache();
        agent.listener.rotate_with(expires - 48 * HOUR, broken);
        agent.listener.rotate_at(expires - 12 * HOUR);
        agent.listener.rotate_at(expires - 11 * HOUR);
    });
    let logs = String::from_utf8(captured.0.lock().unwrap().clone()).unwrap();
    assert_eq!(
        logs.matches("webtransport-cert-reload-failed").count(),
        1,
        "warned once for the one certificate: {logs}"
    );

    let reported = agent.listener.endpoint_at(expires - 12 * HOUR).certificates;
    assert_eq!(reported.len(), 2);
    assert_eq!(hash_of(&reported[0].sha256), h1, "the served one first");
    assert!(agent.pin(vec![h1]).await.is_ok());
}

/// A valid key from another certificate is refused like any other failed swap.
#[tokio::test]
async fn a_key_that_does_not_belong_to_the_certificate_is_refused() {
    let now = SystemTime::now();
    let (old, other) = (Cert::make(now).unwrap(), Cert::make(now).unwrap());
    let mut mismatched = Cert::make(now).unwrap();
    mismatched.key_der = other.key_der.clone();
    assert!(Served::new(&mismatched).is_err());
    let served = Served::new(&old).unwrap();
    assert!(served.set(&mismatched).is_err());
    assert_eq!(served.hash(), old.sha256, "the old certificate stays");
}

/// Resumption would skip the certificate check, so the listener offers none.
#[test]
fn the_tls_config_offers_no_session_resumption() {
    let cert = Cert::make(SystemTime::now()).unwrap();
    let tls = tls_config(Served::new(&cert).unwrap()).unwrap();
    assert!(!tls.session_storage.can_cache());
    assert_eq!(tls.send_tls13_tickets, 0);
}

/// A Retry token issued before a swap redeems after it (the swap never replaces the
/// quinn config, whose token key would change with it).
#[tokio::test]
async fn a_retry_token_issued_before_a_swap_redeems_after_it() {
    let now = SystemTime::now();
    let (old, next) = (Cert::make(now).unwrap(), Cert::make(now).unwrap());
    let served = Served::new(&old).unwrap();
    let quic = quinn::Endpoint::server(
        server_config(served.clone()).unwrap(),
        "127.0.0.1:0".parse().unwrap(),
    )
    .unwrap();
    let url: url::Url = format!("https://127.0.0.1:{}/wt", quic.local_addr().unwrap().port())
        .parse()
        .unwrap();
    let pinned = hash_of(&next.sha256);
    let client = tokio::spawn(async move {
        ClientBuilder::new()
            .with_server_certificate_hashes(vec![pinned])
            .unwrap()
            .connect(url)
            .await
            .is_ok()
    });

    let first = quic.accept().await.unwrap();
    assert!(!first.remote_address_validated());
    first.retry().unwrap();
    served.set(&next).unwrap();
    let second = quic.accept().await.unwrap();
    assert!(second.remote_address_validated(), "the token redeemed");
    let conn = second.await.unwrap();
    Request::accept(conn).await.unwrap().ok().await.unwrap();
    assert!(client.await.unwrap(), "and the next certificate was served");
}

#[tokio::test]
async fn a_bidi_stream_opened_after_the_control_stream_closed_is_reset() {
    // The agent's stream handling (`streams`), with the test holding the control stream:
    // closed both ways, it gives the peer credit for another bidirectional stream.
    let now = SystemTime::now();
    let certs = CertSet::new(now).unwrap();
    let config = server_config(Served::new(certs.presented(now)).unwrap()).unwrap();
    let hash = base64::engine::general_purpose::STANDARD
        .decode(&certs.presented(now).sha256)
        .unwrap();
    let quic = quinn::Endpoint::server(config, "127.0.0.1:0".parse().unwrap()).unwrap();
    let url: url::Url = format!("https://127.0.0.1:{}/wt", quic.local_addr().unwrap().port())
        .parse()
        .unwrap();
    let server = tokio::spawn(async move {
        let conn = quic.accept().await.unwrap().await.unwrap();
        let session = Request::accept(conn).await.unwrap().ok().await.unwrap();
        let (control_tx, control_rx) = oneshot::channel();
        tokio::spawn(streams(session.clone(), control_tx));
        let (mut send, mut recv) = control_rx.await.unwrap();
        recv.read_to_end(64).await.unwrap();
        send.finish().unwrap();
        (session, quic)
    });
    let client = ClientBuilder::new()
        .with_server_certificate_hashes(vec![hash])
        .unwrap()
        .connect(url)
        .await
        .unwrap();
    let (mut send, mut recv) = client.open_bi().await.unwrap();
    send.write_all(b"hello").await.unwrap();
    send.finish().unwrap();
    recv.read_to_end(64).await.unwrap();
    let _held = server.await.unwrap();

    let (mut send, mut recv) = tokio::time::timeout(Duration::from_secs(5), client.open_bi())
        .await
        .expect("credit came back with the closed control stream")
        .unwrap();
    let _ = send.write_all(b"a second control stream").await;
    assert!(
        tokio::time::timeout(Duration::from_secs(5), recv.read(&mut [0; 8]))
            .await
            .unwrap()
            .is_err(),
        "the new stream is reset"
    );
}

/// A client that opens the HTTP/3 streams Chrome opens (control, QPACK encoder and
/// decoder) and keeps them, so it sees a STOP_SENDING on any of them. The library
/// client hides its control stream.
struct Chrome {
    conn: quinn::Connection,
    h3: Vec<quinn::SendStream>,
    _agent_control: quinn::RecvStream,
    connect: quinn::RecvStream,
    _connect_send: quinn::SendStream,
    session_id: web_transport_quinn::proto::VarInt,
    stream: Option<(quinn::SendStream, quinn::RecvStream)>,
}

impl Chrome {
    async fn connect(agent: &Agent, shape: impl FnOnce(&mut quinn::TransportConfig)) -> Self {
        use web_transport_quinn::proto::{
            ConnectRequest, ConnectResponse, Settings, StreamUni, VarInt,
        };
        let endpoint = raw_endpoint_with(shape);
        let addr = SocketAddr::from(([127, 0, 0, 1], agent.url.port().unwrap()));
        let conn = endpoint
            .connect(addr, "quasar-node-agent")
            .unwrap()
            .await
            .unwrap();
        let mut h3 = Vec::new();
        let mut control = conn.open_uni().await.unwrap();
        let mut settings = Settings::default();
        settings.enable_webtransport(1);
        settings.write(&mut control).await.unwrap();
        h3.push(control);
        for kind in [StreamUni::QPACK_ENCODER, StreamUni::QPACK_DECODER] {
            let mut qpack = conn.open_uni().await.unwrap();
            let mut header = Vec::new();
            kind.encode(&mut header);
            qpack.write_all(&header).await.unwrap();
            h3.push(qpack);
        }
        // The agent's control stream: read its SETTINGS so its credit comes back.
        let mut agent_control = conn.accept_uni().await.unwrap();
        Settings::read(&mut agent_control).await.unwrap();
        let (mut connect_send, mut connect) = conn.open_bi().await.unwrap();
        ConnectRequest::new(agent.url.clone())
            .write(&mut connect_send)
            .await
            .unwrap();
        ConnectResponse::read(&mut connect).await.unwrap();
        let session_id = VarInt::from_u64(u64::from(connect_send.id())).unwrap();
        Self {
            conn,
            h3,
            _agent_control: agent_control,
            connect,
            _connect_send: connect_send,
            session_id,
            stream: None,
        }
    }

    /// Open the WebTransport control stream and send `hello`.
    async fn hello(&mut self, wt_proto: u32, token: &str, session_id: &str) {
        let hello = json!({"type": "hello", "wt_proto": wt_proto, "token": token,
                           "session_id": session_id, "client": "browser", "fec": []});
        self.first_message(&serde_json::to_vec(&hello).unwrap())
            .await;
    }

    /// Open the WebTransport control stream and send `body` as its first message.
    async fn first_message(&mut self, body: &[u8]) {
        use web_transport_quinn::proto::Frame;
        let (mut send, recv) = self.conn.open_bi().await.unwrap();
        let mut header = Vec::new();
        Frame::WEBTRANSPORT.encode(&mut header);
        self.session_id.encode(&mut header);
        header.extend_from_slice(&(body.len() as u32).to_le_bytes());
        header.extend_from_slice(body);
        send.write_all(&header).await.unwrap();
        self.stream = Some((send, recv));
    }

    /// The next control message, or `None` if the stream ended without one.
    async fn message(&mut self) -> Option<Value> {
        let (_, recv) = self.stream.as_mut().unwrap();
        let read = async {
            let mut len = [0u8; 4];
            recv.read_exact(&mut len).await.ok()?;
            let mut body = vec![0; u32::from_le_bytes(len) as usize];
            recv.read_exact(&mut body).await.ok()?;
            serde_json::from_slice(&body).ok()
        };
        tokio::time::timeout(Duration::from_secs(10), read)
            .await
            .expect("no control message in 10 s")
    }

    /// The code of the CLOSE_WEBTRANSPORT_SESSION capsule, the way the browser reads it.
    async fn close_code(&mut self) -> u32 {
        use web_transport_quinn::proto::{Capsule, Http3CapsuleReader};
        let mut reader = Http3CapsuleReader::new(&mut self.connect);
        let read = async {
            loop {
                match reader.read().await {
                    Ok(Some(Capsule::CloseWebTransportSession { code, .. })) => return Some(code),
                    Ok(Some(_)) => {}
                    _ => return None,
                }
            }
        };
        tokio::time::timeout(Duration::from_secs(10), read)
            .await
            .expect("no close capsule in 10 s")
            .expect("the CONNECT stream ended without a close capsule")
    }

    /// Chrome closes the connection on a STOP_SENDING for an HTTP/3 control or QPACK
    /// stream; none may arrive, before or after the connection closes. `close_from_client`:
    /// Chrome closes the connection itself once it has the capsule.
    async fn assert_h3_streams_untouched(&self, close_from_client: bool) {
        use futures_util::FutureExt;
        // A closed connection answers `stopped()` at once; the check below still sees a stop.
        let open = self.conn.close_reason().is_none();
        for (n, stream) in self.h3.iter().enumerate().filter(|_| open) {
            assert!(
                stream.stopped().now_or_never().is_none(),
                "the agent stopped HTTP/3 stream {n} before the close reached the client"
            );
        }
        if close_from_client {
            self.conn.close(0u32.into(), b"");
        }
        let _ = tokio::time::timeout(Duration::from_secs(5), self.conn.closed()).await;
        for (n, stream) in self.h3.iter().enumerate() {
            let stopped = tokio::time::timeout(Duration::from_secs(5), stream.stopped())
                .await
                .unwrap_or_else(|_| panic!("stopped() on HTTP/3 stream {n} hung after the close"));
            assert!(
                !matches!(stopped, Ok(Some(_))),
                "the agent stopped HTTP/3 stream {n}"
            );
        }
    }
}

#[tokio::test]
async fn a_refused_hello_is_read_and_closed_without_stopping_chromes_h3_streams() {
    let agent = agent().await;
    // (wt_proto, token, session id, error code, the close code that follows)
    let cases = [
        (1, TOKEN, "not-a-session", "session_not_ready", CLOSE_NORMAL),
        (1, "wrong-token", SESSION, "auth_failed", CLOSE_AUTH),
        (0, TOKEN, SESSION, "unsupported_version", CLOSE_NORMAL),
    ];
    for (n, (proto, token, session, error, close)) in cases.into_iter().enumerate() {
        let mut chrome = Chrome::connect(&agent, |_| {}).await;
        chrome.hello(proto, token, session).await;
        let reply = chrome
            .message()
            .await
            .unwrap_or_else(|| panic!("{error}: no error message"));
        assert_eq!(reply, json!({"type": "error", "code": error}));
        assert_eq!(chrome.close_code().await, close, "{error}");
        chrome.assert_h3_streams_untouched(n == 0).await;
    }

    // A client that cannot take datagrams is closed 4400 with no message, and keeps its token.
    let mut chrome = Chrome::connect(&agent, |t| {
        t.datagram_receive_buffer_size(None);
    })
    .await;
    chrome.hello(1, TOKEN, SESSION).await;
    assert_eq!(chrome.close_code().await, CLOSE_PROTOCOL);
    chrome.assert_h3_streams_untouched(false).await;
    let (_session, _send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(ack["type"], "hello_ack", "the token was not spent");
}

#[tokio::test]
async fn bye_and_takeover_close_without_stopping_chromes_h3_streams() {
    let agent = agent().await;
    let registry = agent.listener.registry();

    // The client says bye.
    let mut leaving = Chrome::connect(&agent, |_| {}).await;
    leaving.hello(1, TOKEN, SESSION).await;
    assert_eq!(leaving.message().await.unwrap()["type"], "hello_ack");
    let (send, _) = leaving.stream.as_mut().unwrap();
    write_message_raw(send, &json!({"type": "bye"})).await;
    assert_eq!(leaving.close_code().await, CLOSE_NORMAL);
    leaving.assert_h3_streams_untouched(true).await;

    // A newer client displaces it, then the session ends.
    let expires = SystemTime::now() + TOKEN_TTL;
    for token in ["first", "second"] {
        assert!(registry.add_token(SESSION, &token_hash(token), expires));
    }
    let mut first = Chrome::connect(&agent, |_| {}).await;
    first.hello(1, "first", SESSION).await;
    assert_eq!(first.message().await.unwrap()["type"], "hello_ack");
    let mut second = Chrome::connect(&agent, |_| {}).await;
    second.hello(1, "second", SESSION).await;
    assert_eq!(second.message().await.unwrap()["type"], "hello_ack");
    assert_eq!(
        first.message().await.unwrap(),
        json!({"type": "bye", "reason": "taken_over"})
    );
    assert_eq!(first.close_code().await, CLOSE_TAKEN_OVER);
    first.assert_h3_streams_untouched(false).await;

    registry.end(SESSION);
    assert_eq!(
        second.message().await.unwrap(),
        json!({"type": "bye", "reason": "session_ended"})
    );
    assert_eq!(second.close_code().await, CLOSE_ENDED);
    second.assert_h3_streams_untouched(false).await;
}

async fn write_message_raw(send: &mut quinn::SendStream, message: &Value) {
    let body = serde_json::to_vec(message).unwrap();
    send.write_all(&(body.len() as u32).to_le_bytes())
        .await
        .unwrap();
    send.write_all(&body).await.unwrap();
}

#[tokio::test]
async fn a_refused_peer_keeps_its_slot_until_its_close_has_run() {
    use futures_util::FutureExt;
    let agent = agent_with(Limits {
        unauthenticated: MAX_UNAUTHENTICATED,
        per_source: 1,
    })
    .await;
    // The refused connection's close holds after its capsule is out, until released.
    let (release, held) = oneshot::channel();
    let (done_tx, done) = oneshot::channel();
    CLOSE_HOLD.with(|h| *h.borrow_mut() = Some((held, done_tx)));
    let mut chrome = Chrome::connect(&agent, |_| {}).await;
    chrome.hello(1, "wrong-token", SESSION).await;
    assert_eq!(chrome.message().await.unwrap()["code"], "auth_failed");
    assert_eq!(chrome.close_code().await, CLOSE_AUTH);
    // `hold_close` takes the armed hold when `close_session` reaches it, before the capsule is read.
    assert!(
        CLOSE_HOLD.with(|h| h.borrow().is_none()),
        "close hold not reached"
    );
    // The capsule is in and the close is held: the agent still holds the source's slot.
    let while_closing = ClientBuilder::new()
        .with_server_certificate_hashes(vec![agent.hash.clone()])
        .unwrap()
        .connect(agent.url.clone())
        .await;
    assert!(
        while_closing.is_err(),
        "a refused connection still holds its source's slot while its close runs"
    );
    for (n, stream) in chrome.h3.iter().enumerate() {
        assert!(
            stream.stopped().now_or_never().is_none(),
            "the agent stopped HTTP/3 stream {n} while the close was held"
        );
    }
    release.send(()).unwrap();
    match tokio::time::timeout(Duration::from_secs(5), done).await {
        Ok(Ok(())) => {}
        Ok(Err(_)) => panic!("the held close was dropped without running"),
        Err(_) => panic!("the held close did not finish within 5 s of its release"),
    }
    chrome.assert_h3_streams_untouched(false).await;
    let (_session, _send, _recv, ack) = agent.hello(TOKEN).await;
    assert_eq!(
        ack["type"], "hello_ack",
        "the slot came back once the close ran"
    );
}

#[tokio::test]
async fn a_malformed_hello_or_none_at_all_is_closed_4400_without_stopping_chromes_h3_streams() {
    let agent = agent().await;
    for body in [&b"{ not json"[..], br#"{"type":"keyframe_request"}"#] {
        let mut chrome = Chrome::connect(&agent, |_| {}).await;
        chrome.first_message(body).await;
        assert_eq!(chrome.close_code().await, CLOSE_PROTOCOL);
        chrome.assert_h3_streams_untouched(false).await;
    }

    // No hello: closed at the 5 s deadline.
    let mut chrome = Chrome::connect(&agent, |_| {}).await;
    let opened = Instant::now();
    assert_eq!(chrome.close_code().await, CLOSE_PROTOCOL);
    assert!(opened.elapsed() >= Duration::from_millis(4900));
    chrome.assert_h3_streams_untouched(false).await;
}

#[tokio::test]
async fn a_stalled_control_stream_is_closed_without_stopping_chromes_h3_streams() {
    use web_transport_quinn::proto::error_to_http3;
    let agent = agent().await;
    // 8 bytes of credit per stream: the handshake completes, the hello_ack cannot be written.
    let mut chrome = Chrome::connect(&agent, |t| {
        t.stream_receive_window(8u32.into())
            .keep_alive_interval(Some(Duration::from_millis(500)));
    })
    .await;
    chrome.hello(1, TOKEN, SESSION).await;
    // The capsule needs credit too, so this close arrives as the connection's.
    let closed = tokio::time::timeout(Duration::from_secs(15), chrome.conn.closed())
        .await
        .expect("never closed");
    assert!(
        matches!(&closed, quinn::ConnectionError::ApplicationClosed(c)
            if u64::from(c.error_code) == error_to_http3(CLOSE_PROTOCOL)),
        "{closed:?}"
    );
    chrome.assert_h3_streams_untouched(false).await;
}

/// The PyroWave datagrams of `n` frames, each frame rejoined from its datagrams' packets
/// (every part of an oversized block in order) after checking the header and coverage rules
/// of codec-pyrowave.md §Video datagrams. Fails on any non-PyroWave datagram.
async fn pyrowave_frames(
    session: &Session,
    n: usize,
    config_id: u8,
    blocks: u32,
) -> Vec<(u16, Vec<u8>)> {
    let mut parts: HashMap<u16, BTreeMap<u16, Bytes>> = HashMap::new();
    let mut done = Vec::new();
    while done.len() < n {
        let d = tokio::time::timeout(Duration::from_secs(10), session.read_datagram())
            .await
            .expect("video stalled")
            .unwrap();
        assert!(d.len() <= media::MAX_DATAGRAM);
        assert_eq!(
            (d[0], d[5], d[6]),
            (0x01, 3, config_id),
            "a PyroWave datagram: keyframe + discardable"
        );
        let frame_seq = u16::from_le_bytes([d[7], d[8]]);
        let count = u16::from_le_bytes([d[11], d[12]]);
        let frags = parts.entry(frame_seq).or_default();
        frags.insert(u16::from_le_bytes([d[9], d[10]]), d.slice(media::HEADER..));
        if frags.len() < count as usize {
            continue;
        }
        let mut frame = Vec::new();
        let mut next = 0u32;
        for p in parts.remove(&frame_seq).unwrap().into_values() {
            let le = |at: usize| u32::from_le_bytes(p[at..at + 4].try_into().unwrap());
            let (first, end) = (le(4), le(8));
            match p[0] {
                0 => {
                    assert_eq!(first, next, "ranges are contiguous");
                    next = end;
                }
                1 if p[1] == 0 => {
                    assert_eq!(first, next);
                    next = end;
                }
                1 => assert_eq!(end, next, "a later part repeats its block's range"),
                kind => panic!("kind {kind}"),
            }
            frame.extend_from_slice(&p[pyrowave::HEADER..]);
        }
        assert_eq!(next, blocks, "the ranges partition [0, N)");
        done.push((frame_seq, frame));
    }
    done
}

#[tokio::test]
async fn a_pyrowave_client_gets_frames_that_rejoin_byte_for_byte_and_can_fall_back() {
    let agent = agent().await;
    let media = agent.listener.registry().lock()[SESSION]
        .media
        .clone()
        .unwrap();
    media.grant_pyrowave(
        PyroWaveGrant {
            wanted: true,
            target_kbps: 200_000,
            floor_kbps: 100_000,
            // The 5 ms gate is `media`'s unit tests'; a loaded test host's loopback can exceed it.
            max_rtt_ms: 1_000,
        },
        (320, 192, 60),
        true,
    );
    let raw = std::fs::read(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../testdata/pyrowave/detail_320x192_xhi.pwv1"
    ))
    .unwrap();
    let golden = &raw[28..];

    let session = agent.connect().await;
    let (mut send, mut recv) = session.open_bi().await.unwrap();
    let hello = json!({"type": "hello", "wt_proto": 1, "token": TOKEN, "session_id": SESSION,
                       "client": "browser", "pyrowave": true});
    write_message(&mut send, &hello).await.unwrap();
    let ack = read_message(&mut recv).await.unwrap();
    assert_eq!(ack["type"], "hello_ack");
    assert_eq!(ack["video"]["codec"], "pyrowave");
    assert_eq!(ack["codec_switch_reason"], "launch");
    assert_eq!(
        ack["video"]["color_space"],
        json!({"primaries": "bt709", "transfer": "bt709", "matrix": "bt709", "full_range": false})
    );
    let description = base64::engine::general_purpose::STANDARD
        .decode(ack["video"]["description_b64"].as_str().unwrap())
        .unwrap();
    assert_eq!(description, raw[..24], "the fixture's own PWV1Header");
    let config_id = ack["config_id"].as_u64().unwrap() as u8;

    // The stand-in encoder's H.264 keeps flowing; none of it reaches this client.
    for _ in 0..3 {
        media.on_pyrowave_frame(&gst::Buffer::from_slice(golden.to_vec()));
    }
    let got = pyrowave_frames(&session, 3, config_id, pyrowave::block_count(320, 192)).await;
    assert!(
        got.iter().all(|(_, f)| f == golden),
        "packed and rejoined, byte for byte"
    );

    // codec-pyrowave.md §Loss: a NACK naming a PyroWave frame is ignored. Nothing was kept for
    // retransmission, so nothing is resent, and no nack_miss comes (the next control message
    // below is the fallback's hello_ack).
    write_message(
        &mut send,
        &json!({"type": "nack", "frame_seq": got[0].0, "frags": [[0, 0]]}),
    )
    .await
    .unwrap();
    assert!(
        tokio::time::timeout(Duration::from_millis(300), session.read_datagram())
            .await
            .is_err(),
        "a PyroWave frame was retransmitted"
    );

    write_message(
        &mut send,
        &json!({"type": "codec_fallback", "reason": "decode_error"}),
    )
    .await
    .unwrap();
    let back = read_message(&mut recv).await.unwrap();
    assert_eq!(back["type"], "hello_ack");
    assert_eq!(back["video"]["codec"], "h264");
    assert_eq!(back["codec_switch_reason"], "client_decode_error");
    let h264 = back["config_id"].as_u64().unwrap() as u8;
    assert_eq!(h264, config_id.wrapping_add(1));
    let first = frames(&session, 1, false).await.remove(0);
    assert_eq!(
        (first.config_id, first.keyframe),
        (h264, true),
        "a keyframe first"
    );
    assert_eq!(nal_types(&first.data).last(), Some(&5));
}

/// A session id assigned again while its tap runs gets a fresh registry entry, which no longer
/// names that tap's media: the tap still ends its own when it drops, so the client on it is
/// told the session ended.
#[test]
fn a_tap_dropped_after_its_session_was_assigned_again_ends_its_own_media() {
    gst::init().unwrap();
    let registry = Arc::new(Registry::default());
    let assign = || registry.assign(SESSION, &token_hash(TOKEN), SystemTime::now() + TOKEN_TTL);
    assign();
    let pipeline = synthetic(Stream::Av1);
    let tap = Tap::attach(
        &registry,
        &pipeline
            .by_name("parse")
            .unwrap()
            .static_pad("src")
            .unwrap(),
        &pipeline.by_name("enc").unwrap(),
        SESSION,
        no_input(),
        audio::Plan::default(),
        None,
        None,
    )
    .unwrap();
    tap.media().on_caps(&streams::av1_caps(640), SESSION);
    let client = tap.media().attach(false, Duration::ZERO).unwrap();

    assign();
    drop(tap);
    assert_eq!(client.gone.get(), Some(&Gone::SessionEnded));
}

/// #1228: a session stop releases the tap's PyroWave encoder before the source goes (it shares
/// the GPU queue and a compositor ring frame), while the tap itself lives on until the
/// transports drop. A second release, and one with no encoder, are no-ops.
#[test]
fn releasing_the_tap_stops_its_pyrowave_encoder_and_keeps_the_tap() {
    use crate::session::pipeline::pyrowave_branch::{feed_at, start_with};
    gst::init().unwrap();
    let registry = Arc::new(Registry::default());
    registry.assign(SESSION, &token_hash(TOKEN), SystemTime::now() + TOKEN_TTL);
    let pipeline = synthetic(Stream::Av1);
    let tap = Tap::attach(
        &registry,
        &pipeline
            .by_name("parse")
            .unwrap()
            .static_pad("src")
            .unwrap(),
        &pipeline.by_name("enc").unwrap(),
        SESSION,
        no_input(),
        audio::Plan::default(),
        None,
        None,
    )
    .unwrap();
    tap.release(); // no encoder yet: nothing to do

    // A live encode pipeline for the PyroWave feed, as the runner's has.
    let raw = gst::Caps::builder("video/x-raw")
        .field("format", "NV12")
        .field("width", 64)
        .field("height", 48)
        .field("framerate", gst::Fraction::new(60, 1))
        .build();
    let main = gst::parse::launch(
        "videotestsrc is-live=true name=src ! capsfilter name=caps ! queue name=q ! fakesink sync=false",
    )
    .unwrap()
    .downcast::<gst::Pipeline>()
    .unwrap();
    main.by_name("caps").unwrap().set_property("caps", &raw);
    let feed = feed_at(main.by_name("q").unwrap().static_pad("src").unwrap(), &raw).unwrap();
    let encoder = start_with(
        feed,
        None,
        &gst::ElementFactory::make("identity").build().unwrap(),
        || true,
        |_| {},
        |_| {},
    )
    .unwrap();
    let side = encoder.pipeline.clone();
    *tap.pyrowave.lock().unwrap() = Some(encoder);
    main.set_state(gst::State::Playing).unwrap();
    let deadline = std::time::Instant::now() + Duration::from_secs(10);
    while side.current_state() != gst::State::Playing {
        assert!(
            std::time::Instant::now() < deadline,
            "the PyroWave pipeline never started"
        );
        std::thread::sleep(Duration::from_millis(20));
    }

    tap.release();
    assert_eq!(
        side.current_state(),
        gst::State::Null,
        "the encoder was stopped"
    );
    assert!(tap.pyrowave.lock().unwrap().is_none(), "and dropped");
    assert!(
        registry.lock().contains_key(SESSION),
        "the tap itself is still alive"
    );
    tap.release();

    let _ = main.set_state(gst::State::Null);
    let _ = pipeline.set_state(gst::State::Null);
}
