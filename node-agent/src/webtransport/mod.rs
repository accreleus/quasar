//! Transport #2: the agent's WebTransport listener (`protocol/transport-webtransport.md`,
//! amendment 54). One QUIC listener per agent serves every session on the host; a
//! session's client authenticates with a single-use token the control plane minted and
//! then gets the session's video as datagrams.
//!
//! Not here yet, each a later ticket: audio, microphone and input datagrams, ping and
//! pong, NACK and its retransmit buffer, FEC, receiver feedback and congestion control,
//! and takeover across transports (a new client replaces the last one; nothing bounds it).

mod cert;
mod media;
#[cfg(test)]
mod tests;

use std::collections::HashMap;
use std::net::{IpAddr, SocketAddr};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, SystemTime};

use anyhow::{Context, Result};
use base64::Engine;
use gstreamer as gst;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use tokio::time::{timeout_at, Instant};
use web_transport_quinn::{quinn, RecvStream, Request, SendStream, Session};

pub use cert::CertReport;
use cert::{Cert, CertSet};
use media::{Attached, Gone, Media, Out};

const WT_PROTO: u32 = 1;
const HELLO_DEADLINE: Duration = Duration::from_secs(5);
const MAX_CONTROL_MESSAGE: usize = 65_536;
/// Unauthenticated connections held at once (§Before `hello`).
const MAX_UNAUTHENTICATED: usize = 64;
const TOKEN_SKEW: Duration = Duration::from_secs(15);
const TOKEN_TTL: Duration = Duration::from_secs(60);
const ROTATION_CHECK: Duration = Duration::from_secs(600);
const DEFAULT_PORT: u16 = 4433;

const CLOSE_NORMAL: u32 = 0;
const CLOSE_PROTOCOL: u32 = 4400;
const CLOSE_AUTH: u32 = 4401;
const CLOSE_ENDED: u32 = 4404;
const CLOSE_TAKEN_OVER: u32 = 4410;

/// `capacity.transports` and `capacity.webtransport` (`agent-api.md`, amendment 54).
#[derive(Serialize, Debug, Clone, PartialEq)]
pub struct Transports {
    pub transports: Vec<&'static str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub webtransport: Option<Endpoint>,
}

#[derive(Serialize, Debug, Clone, PartialEq)]
pub struct Endpoint {
    pub port: u16,
    pub addresses: Vec<String>,
    pub protos: Vec<u32>,
    pub certificates: Vec<CertReport>,
}

struct Token {
    hash: String,
    expires_at: SystemTime,
}

#[derive(Default)]
struct Entry {
    tokens: Vec<Token>,
    media: Option<Arc<Media>>,
    /// Kept for a token's lifetime after the session ends, so a late `hello` reads
    /// `session_ended` rather than `session_not_ready`.
    ended_at: Option<SystemTime>,
}

/// The WebTransport sessions this agent holds: their unredeemed token hashes and, once
/// the encode chain exists, their video.
#[derive(Default)]
pub struct Registry {
    sessions: Mutex<HashMap<String, Entry>>,
}

/// Why a `hello` was refused: its `error.code`, and the close code that follows.
#[derive(Debug, Clone, Copy, PartialEq)]
enum Refusal {
    AuthFailed,
    NotReady,
    SessionEnded,
    UnsupportedVersion,
}

impl Refusal {
    fn code(self) -> (&'static str, u32) {
        match self {
            Self::AuthFailed => ("auth_failed", CLOSE_AUTH),
            Self::NotReady => ("session_not_ready", CLOSE_NORMAL),
            Self::SessionEnded => ("session_ended", CLOSE_ENDED),
            Self::UnsupportedVersion => ("unsupported_version", CLOSE_NORMAL),
        }
    }
}

#[derive(Deserialize)]
struct Hello {
    #[serde(rename = "type")]
    kind: String,
    wt_proto: u32,
    token: String,
    session_id: String,
}

fn token_hash(token: &str) -> String {
    Sha256::digest(token.as_bytes())
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

/// `wt_token_expires_at`, RFC 3339.
pub fn parse_expiry(at: &str) -> Option<SystemTime> {
    time::OffsetDateTime::parse(at, &time::format_description::well_known::Rfc3339)
        .ok()
        .map(SystemTime::from)
}

impl Registry {
    fn lock(&self) -> std::sync::MutexGuard<'_, HashMap<String, Entry>> {
        self.sessions.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// `session_assign` with `transport: "webtransport"`: the session and its first token.
    pub fn assign(&self, session_id: &str, hash: &str, expires_at: SystemTime) {
        let mut sessions = self.lock();
        let now = SystemTime::now();
        sessions.retain(|_, e| {
            e.ended_at
                .is_none_or(|at| now < at + TOKEN_TTL + TOKEN_SKEW)
        });
        sessions.insert(
            session_id.to_string(),
            Entry {
                tokens: vec![Token {
                    hash: hash.to_ascii_lowercase(),
                    expires_at,
                }],
                ..Entry::default()
            },
        );
    }

    /// `session_transport_token`: false for a session this agent does not hold.
    pub fn add_token(&self, session_id: &str, hash: &str, expires_at: SystemTime) -> bool {
        let mut sessions = self.lock();
        let Some(entry) = sessions
            .get_mut(session_id)
            .filter(|e| e.ended_at.is_none())
        else {
            return false;
        };
        let now = SystemTime::now();
        entry.tokens.retain(|t| now <= t.expires_at + TOKEN_SKEW);
        entry.tokens.push(Token {
            hash: hash.to_ascii_lowercase(),
            expires_at,
        });
        true
    }

    /// The session's encode chain exists: its client can be served. False when the
    /// session was never assigned WebTransport.
    pub(crate) fn set_media(&self, session_id: &str, media: Arc<Media>) -> bool {
        match self
            .lock()
            .get_mut(session_id)
            .filter(|e| e.ended_at.is_none())
        {
            Some(entry) => {
                entry.media = Some(media);
                true
            }
            None => false,
        }
    }

    /// The session is stopping: its tokens go, and its client gets `bye session_ended`.
    pub fn end(&self, session_id: &str) {
        let media = self.lock().get_mut(session_id).and_then(|entry| {
            entry.tokens.clear();
            entry.ended_at.get_or_insert_with(SystemTime::now);
            entry.media.take()
        });
        if let Some(media) = media {
            media.end();
        }
    }

    /// Check `hello` and consume its token. A refusal for a session not ready keeps the
    /// token, so the client may retry with it.
    fn redeem(&self, hello: &Hello, now: SystemTime) -> Result<Arc<Media>, Refusal> {
        if hello.wt_proto < WT_PROTO {
            return Err(Refusal::UnsupportedVersion);
        }
        let mut sessions = self.lock();
        let Some(entry) = sessions.get_mut(&hello.session_id) else {
            return Err(Refusal::NotReady);
        };
        if entry.ended_at.is_some() {
            return Err(Refusal::SessionEnded);
        }
        entry.tokens.retain(|t| now <= t.expires_at + TOKEN_SKEW);
        let hash = token_hash(&hello.token);
        let Some(index) = entry.tokens.iter().position(|t| t.hash == hash) else {
            return Err(Refusal::AuthFailed);
        };
        let Some(media) = entry.media.clone().filter(|m| m.ready()) else {
            return Err(Refusal::NotReady);
        };
        entry.tokens.remove(index);
        Ok(media)
    }
}

/// Takes the session's video off the encoded-frame boundary for its WebTransport
/// client, and forgets the session when dropped with the transport.
pub(crate) struct Tap {
    registry: Arc<Registry>,
    session_id: String,
}

impl Tap {
    /// `None` when no listener runs or the session was not assigned WebTransport.
    pub(crate) fn attach(
        registry: &Arc<Registry>,
        video: &gst::Pad,
        encoder: &gst::Element,
        session_id: &str,
    ) -> Option<Self> {
        let media = Media::new(encoder);
        if !registry.set_media(session_id, media.clone()) {
            return None;
        }
        media::tap(video, &media, session_id);
        Some(Self {
            registry: registry.clone(),
            session_id: session_id.to_string(),
        })
    }
}

impl Drop for Tap {
    fn drop(&mut self) {
        self.registry.end(&self.session_id);
    }
}

/// The running listener.
pub struct Listener {
    registry: Arc<Registry>,
    certs: Mutex<CertSet>,
    quic: quinn::Endpoint,
    port: u16,
}

static LISTENER: OnceLock<Arc<Listener>> = OnceLock::new();

/// The process's listener, when one runs.
pub fn listener() -> Option<&'static Arc<Listener>> {
    LISTENER.get()
}

/// Start the listener on `QUASAR_WT_PORT` (default 4433; `0` turns it off). A listener
/// that cannot start leaves the agent WebRTC-only.
pub async fn start_from_env() {
    let port = match std::env::var("QUASAR_WT_PORT")
        .ok()
        .filter(|v| !v.trim().is_empty())
    {
        None => DEFAULT_PORT,
        Some(v) => match v.trim().parse::<u16>() {
            Ok(0) => return,
            Ok(port) => port,
            Err(_) => {
                tracing::warn!(
                    token = "webtransport-port-invalid",
                    "QUASAR_WT_PORT={v} is not a UDP port; WebTransport stays off"
                );
                return;
            }
        },
    };
    let bound = match Listener::bind(SocketAddr::from(([0u16; 8], port)), Arc::default()).await {
        Err(_) => Listener::bind(SocketAddr::from(([0u8; 4], port)), Arc::default()).await,
        ok => ok,
    };
    match bound {
        Ok(listener) => {
            tracing::info!(
                token = "webtransport-listening",
                "WebTransport listening on UDP {port}"
            );
            let _ = LISTENER.set(listener.clone());
            tokio::spawn(listener.clone().rotate());
            tokio::spawn(listener.serve());
        }
        Err(error) => tracing::warn!(
            token = "webtransport-bind-failed",
            "WebTransport listener on UDP {port} did not start, so this agent is \
             WebRTC-only: {error:#}"
        ),
    }
}

/// What every capacity report says about transports.
pub fn capacity() -> Transports {
    Transports {
        transports: if LISTENER.get().is_some() {
            vec!["webrtc", "webtransport"]
        } else {
            vec!["webrtc"]
        },
        webtransport: LISTENER.get().map(|l| l.endpoint()),
    }
}

fn server_config(cert: &Cert) -> Result<quinn::ServerConfig> {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let mut tls = rustls::ServerConfig::builder_with_provider(provider)
        .with_protocol_versions(&[&rustls::version::TLS13])?
        .with_no_client_auth()
        .with_single_cert(
            vec![cert.der.clone().into()],
            rustls_pki_types::PrivateKeyDer::Pkcs8(cert.key_der.clone().into()),
        )?;
    tls.alpn_protocols = vec![web_transport_quinn::ALPN.as_bytes().to_vec()];
    let crypto = quinn::crypto::rustls::QuicServerConfig::try_from(tls)?;
    Ok(quinn::ServerConfig::with_crypto(Arc::new(crypto)))
}

impl Listener {
    pub(crate) async fn bind(addr: SocketAddr, registry: Arc<Registry>) -> Result<Arc<Self>> {
        let certs = CertSet::new(SystemTime::now())?;
        let config = server_config(certs.presented(SystemTime::now()))?;
        let quic = quinn::Endpoint::server(config, addr).context("bind the QUIC listener")?;
        let port = quic.local_addr()?.port();
        Ok(Arc::new(Self {
            registry,
            certs: Mutex::new(certs),
            quic,
            port,
        }))
    }

    pub fn registry(&self) -> &Arc<Registry> {
        &self.registry
    }

    fn certs(&self) -> std::sync::MutexGuard<'_, CertSet> {
        self.certs.lock().unwrap_or_else(|e| e.into_inner())
    }

    pub(crate) fn endpoint(&self) -> Endpoint {
        Endpoint {
            port: self.port,
            addresses: addresses(),
            protos: vec![WT_PROTO],
            certificates: self.certs().report(SystemTime::now()),
        }
    }

    /// Age the certificates and present the next one on new connections when it is
    /// due. The 15 s capacity refresh carries a changed list to the control plane, well
    /// inside the 48 h between reporting a certificate and presenting it.
    async fn rotate(self: Arc<Self>) {
        let mut presented = self.certs().presented(SystemTime::now()).sha256.clone();
        loop {
            tokio::time::sleep(ROTATION_CHECK).await;
            let now = SystemTime::now();
            let mut certs = self.certs();
            if let Err(error) = certs.tick(now) {
                tracing::warn!(
                    token = "webtransport-cert-rotation-failed",
                    "WebTransport certificate rotation failed: {error:#}"
                );
            }
            let cert = certs.presented(now);
            if cert.sha256 != presented {
                match server_config(cert) {
                    Ok(config) => {
                        self.quic.set_server_config(Some(config));
                        presented = cert.sha256.clone();
                        tracing::info!(
                            token = "webtransport-cert-rotated",
                            "WebTransport presents its next certificate"
                        );
                    }
                    Err(error) => tracing::warn!(
                        token = "webtransport-cert-reload-failed",
                        "WebTransport could not present its next certificate: {error:#}"
                    ),
                }
            }
        }
    }

    pub(crate) async fn serve(self: Arc<Self>) {
        let mut server = web_transport_quinn::Server::new(self.quic.clone());
        let unauthenticated = Arc::new(Semaphore::new(MAX_UNAUTHENTICATED));
        while let Some(request) = server.accept().await {
            match unauthenticated.clone().try_acquire_owned() {
                Ok(permit) => {
                    tokio::spawn(connection(request, self.registry.clone(), permit));
                }
                Err(_) => {
                    tokio::spawn(
                        request.reject(web_transport_quinn::http::StatusCode::TOO_MANY_REQUESTS),
                    );
                }
            }
        }
    }
}

/// The addresses a browser may reach this host on, best first: loopback and link-local
/// left out, container bridges last.
fn addresses() -> Vec<String> {
    let mut found: Vec<(bool, bool, IpAddr)> = crate::session::media_evidence::interfaces()
        .into_iter()
        .filter(|(_, ip, _)| reachable(ip))
        .map(|(name, ip, _)| {
            let bridge = ["docker", "br-", "veth", "virbr", "cni", "podman"]
                .iter()
                .any(|p| name.starts_with(p));
            (bridge, ip.is_ipv6(), ip)
        })
        .collect();
    found.sort();
    found.dedup_by_key(|(_, _, ip)| *ip);
    found.into_iter().map(|(_, _, ip)| ip.to_string()).collect()
}

fn reachable(ip: &IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => !(v4.is_loopback() || v4.is_link_local() || v4.is_unspecified()),
        IpAddr::V6(v6) => {
            !(v6.is_loopback() || v6.is_unspecified() || (v6.segments()[0] & 0xffc0) == 0xfe80)
        }
    }
}

async fn read_message(recv: &mut RecvStream) -> Result<Value, Violation> {
    let mut len = [0u8; 4];
    recv.read_exact(&mut len)
        .await
        .map_err(|_| Violation::Gone)?;
    let len = u32::from_le_bytes(len) as usize;
    if len > MAX_CONTROL_MESSAGE {
        return Err(Violation::Protocol("control message over 64 KiB"));
    }
    let mut body = vec![0; len];
    recv.read_exact(&mut body)
        .await
        .map_err(|_| Violation::Gone)?;
    serde_json::from_slice(&body).map_err(|_| Violation::Protocol("malformed control message"))
}

async fn write_message(send: &mut SendStream, message: &Value) -> Result<()> {
    let body = serde_json::to_vec(message)?;
    send.write_all(&(body.len() as u32).to_le_bytes()).await?;
    send.write_all(&body).await?;
    Ok(())
}

#[derive(Debug)]
enum Violation {
    /// The stream or connection went away.
    Gone,
    Protocol(&'static str),
}

fn hello_ack(config: &media::VideoConfig) -> Value {
    json!({
        "type": "hello_ack",
        "wt_proto": WT_PROTO,
        "config_id": config.config_id,
        "video": {
            "codec": config.codec,
            "description_b64": base64::engine::general_purpose::STANDARD.encode(&config.description),
            "width": config.width,
            "height": config.height,
            "fps": config.fps,
        },
        "audio": null,
        "mic": null,
        "fec": null,
    })
}

/// Refuse every stream past the control stream, before and after `hello_ack`.
async fn refuse_streams(session: Session) {
    loop {
        tokio::select! {
            bi = session.accept_bi() => match bi {
                Ok((mut send, mut recv)) => {
                    let _ = send.reset(0);
                    let _ = recv.stop(0);
                }
                Err(_) => return,
            },
            uni = session.accept_uni() => match uni {
                Ok(mut recv) => {
                    let _ = recv.stop(0);
                }
                Err(_) => return,
            },
        }
    }
}

/// Input (`0x03`) and microphone (`0x05`) are later tickets: every datagram is dropped,
/// which is also the rule before `hello_ack`.
async fn drop_datagrams(session: Session) {
    while session.read_datagram().await.is_ok() {}
}

async fn connection(request: Request, registry: Arc<Registry>, permit: OwnedSemaphorePermit) {
    let Ok(session) = request.ok().await else {
        return;
    };
    let deadline = Instant::now() + HELLO_DEADLINE;
    let datagrams = tokio::spawn(drop_datagrams(session.clone()));
    let control = match timeout_at(deadline, session.accept_bi()).await {
        Ok(Ok(control)) => Some(control),
        Ok(Err(_)) => None,
        Err(_) => {
            session.close(CLOSE_PROTOCOL, b"no hello in 5 s");
            None
        }
    };
    if let Some((send, mut recv)) = control {
        let others = tokio::spawn(refuse_streams(session.clone()));
        match timeout_at(deadline, read_message(&mut recv)).await {
            Err(_) => session.close(CLOSE_PROTOCOL, b"no hello in 5 s"),
            Ok(Err(Violation::Gone)) => {}
            Ok(Err(Violation::Protocol(why))) => session.close(CLOSE_PROTOCOL, why.as_bytes()),
            Ok(Ok(message)) => match serde_json::from_value::<Hello>(message) {
                Ok(hello) if hello.kind == "hello" => {
                    authenticate(&session, send, recv, &registry, hello, permit).await;
                }
                _ => session.close(CLOSE_PROTOCOL, b"the first message is not a hello"),
            },
        }
        others.abort();
    }
    datagrams.abort();
}

async fn authenticate(
    session: &Session,
    mut send: SendStream,
    recv: RecvStream,
    registry: &Registry,
    hello: Hello,
    permit: OwnedSemaphorePermit,
) {
    let media = match registry.redeem(&hello, SystemTime::now()) {
        Ok(media) => media,
        Err(refusal) => {
            let (code, close) = refusal.code();
            tracing::info!(
                token = "webtransport-hello-refused",
                session_id = %hello.session_id,
                "WebTransport hello refused: {code}"
            );
            let _ = write_message(&mut send, &json!({"type": "error", "code": code})).await;
            let _ = send.finish();
            session.close(close, code.as_bytes());
            return;
        }
    };
    drop(permit);
    let Some(client) = media.attach() else {
        session.close(CLOSE_NORMAL, b"internal");
        return;
    };
    tracing::info!(
        token = "webtransport-client-connected",
        session_id = %hello.session_id,
        "WebTransport client connected from {}",
        session.remote_address()
    );
    if write_message(&mut send, &hello_ack(&client.config))
        .await
        .is_err()
    {
        media.detach(client.id);
        return;
    }
    stream(session, send, recv, &media, client).await;
}

/// Hand the client its video until it leaves, is displaced or the session ends.
async fn stream(
    session: &Session,
    mut send: SendStream,
    mut recv: RecvStream,
    media: &Arc<Media>,
    mut client: Attached,
) {
    let (control_tx, mut control_rx) = tokio::sync::mpsc::channel(16);
    let reader = tokio::spawn(async move {
        loop {
            let message = read_message(&mut recv).await;
            let last = message.is_err();
            if control_tx.send(message).await.is_err() || last {
                return;
            }
        }
    });
    let mut pkt_seq = 0u32;
    let close = loop {
        tokio::select! {
            out = client.rx.recv() => match out {
                Some(Out::Unit(unit)) => {
                    let max = session.max_datagram_size().min(media::MAX_DATAGRAM);
                    let sent = media::packetize(&unit, max, &mut pkt_seq)
                        .into_iter()
                        .try_for_each(|d| session.send_datagram(d));
                    if sent.is_err() {
                        break None;
                    }
                }
                Some(Out::Config(config)) => {
                    if write_message(&mut send, &hello_ack(&config)).await.is_err() {
                        break None;
                    }
                }
                None => break Some(match client.gone.get() {
                    Some(Gone::TakenOver) => (json!({"type": "bye", "reason": "taken_over"}), CLOSE_TAKEN_OVER),
                    Some(Gone::Lagged) => (json!({"type": "error", "code": "internal"}), CLOSE_NORMAL),
                    _ => (json!({"type": "bye", "reason": "session_ended"}), CLOSE_ENDED),
                }),
            },
            message = control_rx.recv() => match message {
                Some(Ok(message)) => match message.get("type").and_then(Value::as_str) {
                    Some("keyframe_request") => media.keyframe(),
                    Some("bye") => break None,
                    _ => {}
                },
                Some(Err(Violation::Protocol(why))) => {
                    session.close(CLOSE_PROTOCOL, why.as_bytes());
                    break None;
                }
                Some(Err(Violation::Gone)) | None => break None,
            },
        }
    };
    reader.abort();
    media.detach(client.id);
    if let Some((message, code)) = close {
        let _ = write_message(&mut send, &message).await;
        let _ = send.finish();
        session.close(code, b"");
    } else {
        session.close(CLOSE_NORMAL, b"");
    }
}
