//! The conformance client: drives the listener as a browser would (pinning the
//! certificate by hash, a control stream, datagrams) and checks what arrives against
//! `protocol/transport-webtransport.md`.

use std::collections::{BTreeMap, HashMap};

use bytes::Bytes;
use gstreamer::prelude::*;
use web_transport_quinn::{ClientBuilder, SessionError, WebTransportError};

use super::*;

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

fn ready_media() -> Arc<Media> {
    gst::init().unwrap();
    let encoder = gst::ElementFactory::make("identity").build().unwrap();
    let media = Media::new(&encoder);
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
    let tap = Tap::attach(
        &registry,
        &parse.static_pad("src").unwrap(),
        &encoder,
        SESSION,
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

/// The close code the browser's `WebTransport.closed` would resolve with.
async fn close_code(session: &Session) -> Option<u32> {
    match tokio::time::timeout(Duration::from_secs(10), session.closed()).await {
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
    endpoint.set_default_client_config(quinn::ClientConfig::new(Arc::new(crypto)));
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

#[tokio::test]
async fn a_bidi_stream_opened_after_the_control_stream_closed_is_reset() {
    // The agent's stream handling (`streams`), with the test holding the control stream:
    // closed both ways, it gives the peer credit for another bidirectional stream.
    let now = SystemTime::now();
    let certs = CertSet::new(now).unwrap();
    let config = server_config(certs.presented(now)).unwrap();
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
