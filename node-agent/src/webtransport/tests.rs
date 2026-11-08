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
    let media = Media::new(&encoder, input);
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
        registry.redeem(&hello_for("other", TOKEN, 1), now).err(),
        Some(Refusal::NotReady),
        "a session this agent does not hold yet"
    );
    assert_eq!(
        registry.redeem(&hello_for(SESSION, TOKEN, 1), now).err(),
        Some(Refusal::NotReady),
        "no encode chain yet"
    );
    assert_eq!(
        registry.redeem(&hello_for(SESSION, TOKEN, 0), now).err(),
        Some(Refusal::UnsupportedVersion)
    );
    assert!(registry.set_media(SESSION, ready_media()));
    // Kind separation: a signaling token's hash never reaches the agent.
    assert_eq!(
        registry
            .redeem(&hello_for(SESSION, "a-signaling-token", 1), now)
            .err(),
        Some(Refusal::AuthFailed)
    );
    // The token refused as not ready was kept, and is good until expiry + 15 s.
    assert!(registry
        .redeem(&hello_for(SESSION, TOKEN, 2), at(15))
        .is_ok());
    assert_eq!(
        registry.redeem(&hello_for(SESSION, TOKEN, 1), at(15)).err(),
        Some(Refusal::AuthFailed),
        "consumed"
    );

    assert!(registry.add_token(SESSION, &token_hash("second"), now));
    assert_eq!(
        registry
            .redeem(&hello_for(SESSION, "second", 1), at(16))
            .err(),
        Some(Refusal::AuthFailed),
        "expired past the skew"
    );
    assert!(!registry.add_token("other", &token_hash("x"), now));

    registry.assign("b", &token_hash("token-of-b"), at(60));
    assert!(registry.set_media("b", ready_media()));
    assert_eq!(
        registry
            .redeem(&hello_for(SESSION, "token-of-b", 1), now)
            .err(),
        Some(Refusal::AuthFailed),
        "another session's token"
    );

    registry.end("b");
    assert_eq!(
        registry.redeem(&hello_for("b", "token-of-b", 1), now).err(),
        Some(Refusal::SessionEnded)
    );
    assert!(!registry.add_token("b", &token_hash("late"), at(60)));
}

/// An agent with one WebTransport session, encoding a live test picture through the
/// same parser → payloader boundary the WebRTC transport builds.
struct Agent {
    listener: Arc<Listener>,
    url: url::Url,
    hash: Vec<u8>,
    pipeline: gst::Pipeline,
    tap: Option<Tap>,
    /// Every input event the agent's devices were given, as JSON text.
    input: Arc<Mutex<Vec<String>>>,
    /// How many times a departing client's inputs were lifted.
    released: Arc<std::sync::atomic::AtomicUsize>,
}

impl Drop for Agent {
    fn drop(&mut self) {
        self.tap.take();
        let _ = self.pipeline.set_state(gst::State::Null);
    }
}

async fn agent() -> Agent {
    agent_with(Limits::default()).await
}

async fn agent_with(limits: Limits) -> Agent {
    gst::init().unwrap();
    assert!(
        gst::ElementFactory::find("openh264enc").is_some(),
        "the conformance test needs openh264enc (gstreamer1.0-plugins-bad)"
    );
    let registry = Arc::new(Registry::default());
    registry.assign(SESSION, &token_hash(TOKEN), SystemTime::now() + TOKEN_TTL);
    let pipeline = gst::parse::launch(
        "videotestsrc is-live=true pattern=ball ! video/x-raw,width=640,height=360,framerate=30/1 \
         ! openh264enc name=enc gop-size=1000 bitrate=4000000 \
         ! video/x-h264,profile=constrained-baseline ! h264parse name=parse \
         ! rtph264pay config-interval=-1 pt=96 ! fakesink sync=false",
    )
    .unwrap()
    .downcast::<gst::Pipeline>()
    .unwrap();
    let parse = pipeline.by_name("parse").unwrap();
    let encoder = pipeline.by_name("enc").unwrap();
    let (input, events, released) = recorded();
    let tap = Tap::attach(
        &registry,
        &parse.static_pad("src").unwrap(),
        &encoder,
        SESSION,
        input,
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
        input: events,
        released,
    }
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
        let session = self.connect().await;
        let (mut send, mut recv) = session.open_bi().await.unwrap();
        let hello = json!({"type": "hello", "wt_proto": 1, "token": token,
                           "session_id": SESSION, "client": "browser", "fec": []});
        write_message(&mut send, &hello).await.unwrap();
        let reply = read_message(&mut recv).await.unwrap();
        (session, send, recv, reply)
    }
}

thread_local! {
    /// Run once by `Media::detach` between clearing the client and taking the input lock.
    static DETACH_GAP: RefCell<Option<Box<dyn FnOnce()>>> = const { RefCell::new(None) };
}

/// `Media::detach`'s seam: the moment a test can attach a successor into the gap.
pub(super) fn detach_gap() {
    if let Some(run) = DETACH_GAP.with(|g| g.borrow_mut().take()) {
        run();
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

    let old = media.attach().unwrap();
    media.input_event(old.id, &key(30, true), false);
    assert_eq!(events.lock().unwrap().len(), 1);

    // A takeover lifts what the old client held as the new one attaches, before it can send.
    let new = media.attach().unwrap();
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
fn a_client_attaching_while_its_predecessor_is_still_leaving_starts_from_clean_devices() {
    use std::cell::RefCell;
    use std::rc::Rc;
    use std::sync::atomic::Ordering::SeqCst;
    let (input, events, released) = recorded();
    let media = ready_media_with(input);
    let key =
        |code: u32, pressed: bool| json!({"t": "k", "code": code, "pressed": pressed}).to_string();

    let a = media.attach().unwrap();
    media.input_event(a.id, &key(30, true), false);

    // B attaches after A's detach cleared the client and before it took the input lock.
    let b: Rc<RefCell<Option<media::Attached>>> = Rc::default();
    let (m, slot) = (media.clone(), b.clone());
    DETACH_GAP.with(|g| *g.borrow_mut() = Some(Box::new(move || *slot.borrow_mut() = m.attach())));
    media.detach(a.id);
    let b = b.borrow_mut().take().expect("B attached in the gap");
    assert_eq!(
        released.load(SeqCst),
        1,
        "A's held key was lifted when B attached"
    );

    // A's late detach step lifts nothing of B's.
    media.input_event(b.id, &key(32, true), false);
    assert_eq!(events.lock().unwrap().len(), 2);
    media.detach(b.id);
    assert_eq!(released.load(SeqCst), 2);
}

#[test]
fn a_gamepad_slot_beyond_the_api_limit_is_dropped() {
    let (input, events, _) = recorded();
    let media = ready_media_with(input);
    let id = media.attach().unwrap().id;
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
                        .redeem(&hello_for(SESSION, TOKEN, 1), SystemTime::now())
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
struct Captured(Arc<Mutex<Vec<u8>>>);

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
