//! Transport #2: the agent's WebTransport listener (`protocol/transport-webtransport.md`,
//! amendment 54). One QUIC listener per agent serves every session on the host; a
//! session's client authenticates with a single-use token the control plane minted and
//! then gets the session's video as datagrams.
//!
//! Not here yet, each a later ticket: audio, microphone, FEC, receiver feedback and
//! congestion control, and takeover across transports (a new client replaces the last
//! one; nothing bounds it).

mod cert;
mod media;
mod pyrowave;
mod retransmit;
#[cfg(test)]
mod tests;

use std::collections::HashMap;
use std::net::{IpAddr, SocketAddr};
use std::pin::pin;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, SystemTime};

use anyhow::{Context, Result};
use base64::Engine;
use gstreamer as gst;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use tokio::sync::oneshot;
use tokio::time::{timeout_at, Instant};
use web_transport_quinn::{quinn, RecvStream, Request, SendStream, Session};

pub use cert::CertReport;
use cert::{Cert, CertSet};
pub use media::PyroWaveGrant;
use media::{Attached, Gone, Media, Out};

const WT_PROTO: u32 = 1;
const HELLO_DEADLINE: Duration = Duration::from_secs(5);
const MAX_CONTROL_MESSAGE: usize = 65_536;
/// Unauthenticated connections held at once (§Before `hello`).
const MAX_UNAUTHENTICATED: usize = 64;
/// Of those, from one source address; `QUASAR_WT_UNAUTH_PER_SOURCE` overrides it.
const DEFAULT_PER_SOURCE: usize = 4;
/// Below this the 1200-byte datagram budget cannot be met (§Channels, "Datagram size").
const MIN_DATAGRAM: usize = 1100;
/// PyroWave frame_seqs remembered for ignoring NACKs: well past the retransmit window at 60 fps.
const PYROWAVE_FRAMES_REMEMBERED: usize = 64;
/// How long an authenticated client may hold up one write on its control stream.
const CONTROL_WRITE: Duration = Duration::from_secs(2);
/// Nothing received on the control stream for this long means the client is gone
/// (§Bye and error); the client pings once a second.
const CONTROL_IDLE: Duration = Duration::from_secs(10);
/// The input datagram channel (§Channels).
const CHANNEL_INPUT: u8 = 0x03;
/// The longest prefix of a client-supplied id that reaches a log line.
const LOGGED_ID: usize = 64;
const TOKEN_SKEW: Duration = Duration::from_secs(15);
const TOKEN_TTL: Duration = Duration::from_secs(60);
const ROTATION_CHECK: Duration = Duration::from_secs(60);
const DEFAULT_PORT: u16 = 4433;

const CLOSE_NORMAL: u32 = 0;
const CLOSE_PROTOCOL: u32 = 4400;
const CLOSE_AUTH: u32 = 4401;
const CLOSE_ENDED: u32 = 4404;
const CLOSE_TAKEN_OVER: u32 = 4410;

/// Where a session's client input goes: the same `input::handle` the WebRTC data channel
/// feeds, so both transports inject identically. A seam so a test can watch what arrives.
#[derive(Clone)]
pub(crate) struct Input {
    /// One `input.md` event as JSON text, from a datagram or the control stream.
    pub(crate) event: Arc<dyn Fn(&str) + Send + Sync>,
    /// The client left: lift every key, button and stick it held.
    pub(crate) release_all: Arc<dyn Fn() + Send + Sync>,
}

impl Input {
    pub(crate) fn devices(
        devices: Option<Arc<crate::session::virtual_input::VirtualDevices>>,
        state: Arc<crate::session::input::InputState>,
        width: i32,
        height: i32,
    ) -> Self {
        use crate::session::input::{handle, InputSink};
        let sink = devices.clone().map_or(InputSink::None, InputSink::Virtual);
        Self {
            event: Arc::new(move |json| handle(json, &sink, &state, width, height)),
            release_all: Arc::new(move || {
                if let Some(Err(e)) = devices.as_ref().map(|d| d.release_all()) {
                    tracing::warn!(
                        token = "webtransport-release-all-failed",
                        "release_all on disconnect failed: {e:#}"
                    );
                }
            }),
        }
    }
}

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
    /// Amendment 55: this connection can decode PyroWave.
    #[serde(default)]
    pyrowave: bool,
}

fn token_hash(token: &str) -> String {
    Sha256::digest(token.as_bytes())
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

fn fixed_time_eq(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len()
        && std::hint::black_box(a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y))) == 0
}

/// A client-supplied id, cut to `LOGGED_ID` characters for a log line.
fn shown(id: &str) -> &str {
    id.char_indices()
        .nth(LOGGED_ID)
        .map_or(id, |(at, _)| &id[..at])
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
        let Some(index) = entry
            .tokens
            .iter()
            .position(|t| fixed_time_eq(t.hash.as_bytes(), hash.as_bytes()))
        else {
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
    /// The PyroWave encoder of a granted session; dropped after the session is ended here.
    pyrowave: Option<crate::session::pipeline::pyrowave_branch::Encoder>,
    media: Arc<Media>,
}

impl Tap {
    /// `None` when no listener runs or the session was not assigned WebTransport.
    /// `pyrowave`: the session's config, its grant and the feed off the encode pipeline.
    pub(crate) fn attach(
        registry: &Arc<Registry>,
        video: &gst::Pad,
        encoder: &gst::Element,
        session_id: &str,
        trace: Option<crate::session::pipeline::TraceTx>,
        input: Input,
        pyrowave: Option<(
            &crate::session::SessionConfig,
            PyroWaveGrant,
            Option<crate::session::pipeline::pyrowave_branch::Feed>,
        )>,
    ) -> Option<Self> {
        let media = Media::new(encoder, input, trace.map(|tx| (tx, session_id.to_string())));
        if !registry.set_media(session_id, media.clone()) {
            return None;
        }
        media::tap(video, &media, session_id);
        let pyrowave =
            pyrowave.and_then(|(cfg, grant, feed)| pyrowave_branch(cfg, grant, feed, &media));
        Some(Self {
            registry: registry.clone(),
            session_id: session_id.to_string(),
            pyrowave,
            media,
        })
    }

    /// A client is attached and being sent video: the session's idle reap counts it.
    pub(crate) fn attached(&self) -> bool {
        self.media.attached()
    }
}

/// Start the PyroWave encode of a granted session, gated on a PyroWave client. With no feed
/// or encoder the grant still stands, unusable, so a `hello` reads `encoder_error`.
fn pyrowave_branch(
    cfg: &crate::session::SessionConfig,
    grant: PyroWaveGrant,
    feed: Option<crate::session::pipeline::pyrowave_branch::Feed>,
    media: &Arc<Media>,
) -> Option<crate::session::pipeline::pyrowave_branch::Encoder> {
    let size = (cfg.stream.width, cfg.stream.height, cfg.stream.fps);
    let Some(feed) = feed else {
        media.grant_pyrowave(grant, size, false);
        return None;
    };
    media.grant_pyrowave(grant, size, true);
    let (gate, frames, errors) = (
        Arc::downgrade(media),
        Arc::downgrade(media),
        Arc::downgrade(media),
    );
    // Rate: the encoder budgets `bitrate` / fps per frame; B09 (#1058) drives it live from
    // the clamp of codec-pyrowave.md §Rate.
    let started = crate::session::pipeline::pyrowave_branch::start_encoder(
        cfg,
        feed,
        grant.target_kbps,
        move || gate.upgrade().is_some_and(|m| m.pyrowave_active()),
        move |buffer| {
            if let Some(media) = frames.upgrade() {
                media.on_pyrowave_frame(buffer);
            }
        },
        move |error| {
            tracing::warn!(
                token = "pyrowave-encoder-failed",
                "PyroWave encoder failed: {error}"
            );
            if let Some(media) = errors.upgrade() {
                media.switch_off(None, "encoder_error");
            }
        },
    );
    match started {
        Ok(encoder) => Some(encoder),
        Err(error) => {
            tracing::warn!(
                token = "pyrowave-encoder-start-failed",
                "PyroWave encoder did not start: {error:#}"
            );
            media.switch_off(None, "encoder_error");
            None
        }
    }
}

impl Drop for Tap {
    fn drop(&mut self) {
        self.registry.end(&self.session_id);
        self.pyrowave.take();
    }
}

/// How many unauthenticated connections the listener holds, in all and per source
/// address (0: no per-source cap, for an edge proxy that every client shares).
#[derive(Clone, Copy, Debug)]
pub(crate) struct Limits {
    pub(crate) unauthenticated: usize,
    pub(crate) per_source: usize,
}

impl Default for Limits {
    fn default() -> Self {
        Self {
            unauthenticated: MAX_UNAUTHENTICATED,
            per_source: DEFAULT_PER_SOURCE,
        }
    }
}

/// The unauthenticated connections held now, by source address.
struct Gate {
    limits: Limits,
    held: Mutex<HashMap<IpAddr, usize>>,
}

/// The per-source key: an IPv4 address, or an IPv6 address's /64, which one host
/// usually holds whole. On a SLAAC LAN every client shares one /64, and `fe80::/64`
/// merges link-local peers across interfaces (`QUASAR_WT_UNAUTH_PER_SOURCE`).
fn source(ip: IpAddr) -> IpAddr {
    match ip.to_canonical() {
        IpAddr::V6(v6) => IpAddr::V6((u128::from(v6) & !u128::from(u64::MAX)).into()),
        v4 => v4,
    }
}

/// One unauthenticated connection's place in the gate, given back on drop.
struct Slot {
    gate: Arc<Gate>,
    ip: IpAddr,
}

impl Gate {
    fn take(self: &Arc<Self>, ip: IpAddr) -> Option<Slot> {
        let ip = source(ip);
        let mut held = self.held.lock().unwrap_or_else(|e| e.into_inner());
        let mine = held.get(&ip).copied().unwrap_or(0);
        if held.values().sum::<usize>() >= self.limits.unauthenticated
            || (self.limits.per_source > 0 && mine >= self.limits.per_source)
        {
            return None;
        }
        held.insert(ip, mine + 1);
        Some(Slot {
            gate: self.clone(),
            ip,
        })
    }
}

impl Drop for Slot {
    fn drop(&mut self) {
        let mut held = self.gate.held.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(n) = held.get_mut(&self.ip) {
            *n -= 1;
            if *n == 0 {
                held.remove(&self.ip);
            }
        }
    }
}

/// The running listener.
pub struct Listener {
    registry: Arc<Registry>,
    certs: Mutex<CertSet>,
    served: Arc<Served>,
    /// The certificate a failed swap last warned about.
    reload_failed: Mutex<Option<String>>,
    quic: quinn::Endpoint,
    port: u16,
    limits: Limits,
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
    let mut limits = Limits::default();
    if let Ok(v) = std::env::var("QUASAR_WT_UNAUTH_PER_SOURCE") {
        match v.trim().parse() {
            Ok(n) => limits.per_source = n,
            Err(_) => tracing::warn!(
                token = "webtransport-per-source-invalid",
                "QUASAR_WT_UNAUTH_PER_SOURCE={v} is not a count; using {DEFAULT_PER_SOURCE}"
            ),
        }
    }
    let any6 = SocketAddr::from(([0u16; 8], port));
    let bound = match Listener::bind(any6, Arc::default(), limits).await {
        Err(_) => Listener::bind(SocketAddr::from(([0u8; 4], port)), Arc::default(), limits).await,
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

/// The certificate new handshakes get. One `quinn::ServerConfig` serves for the listener's
/// life and only this changes, so the Retry token key never does: a token issued just
/// before a swap still redeems after it.
#[derive(Debug)]
struct Served(std::sync::RwLock<(String, Arc<rustls::sign::CertifiedKey>)>);

impl Served {
    fn load(cert: &Cert) -> Result<(String, Arc<rustls::sign::CertifiedKey>)> {
        // `from_der` refuses a key that does not belong to the certificate.
        let certified = rustls::sign::CertifiedKey::from_der(
            vec![cert.der.clone().into()],
            rustls_pki_types::PrivateKeyDer::Pkcs8(cert.key_der.clone().into()),
            &rustls::crypto::ring::default_provider(),
        )?;
        Ok((cert.sha256.clone(), Arc::new(certified)))
    }

    fn new(cert: &Cert) -> Result<Arc<Self>> {
        Ok(Arc::new(Self(std::sync::RwLock::new(Self::load(cert)?))))
    }

    /// Serve `cert` to new handshakes. On `Err` the previous certificate stays.
    fn set(&self, cert: &Cert) -> Result<()> {
        *self.0.write().unwrap_or_else(|e| e.into_inner()) = Self::load(cert)?;
        Ok(())
    }

    fn hash(&self) -> String {
        self.0.read().unwrap_or_else(|e| e.into_inner()).0.clone()
    }
}

impl rustls::server::ResolvesServerCert for Served {
    fn resolve(
        &self,
        _: rustls::server::ClientHello<'_>,
    ) -> Option<Arc<rustls::sign::CertifiedKey>> {
        Some(self.0.read().unwrap_or_else(|e| e.into_inner()).1.clone())
    }
}

fn tls_config(served: Arc<Served>) -> Result<rustls::ServerConfig> {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let mut tls = rustls::ServerConfig::builder_with_provider(provider)
        .with_protocol_versions(&[&rustls::version::TLS13])?
        .with_no_client_auth()
        .with_cert_resolver(served);
    tls.alpn_protocols = vec![web_transport_quinn::ALPN.as_bytes().to_vec()];
    // No resumption: it would skip the certificate, so a client could resume on the
    // certificate of a handshake from before a swap.
    tls.session_storage = Arc::new(rustls::server::NoServerSessionStorage {});
    tls.send_tls13_tickets = 0;
    Ok(tls)
}

fn server_config(served: Arc<Served>) -> Result<quinn::ServerConfig> {
    let tls = tls_config(served)?;
    let crypto = quinn::crypto::rustls::QuicServerConfig::try_from(tls)?;
    // Bounds on what one peer can make the agent hold. Two bidirectional streams are the
    // CONNECT stream and the control stream; HTTP/3 needs three unidirectional ones.
    let mut transport = quinn::TransportConfig::default();
    transport
        .max_concurrent_bidi_streams(2u32.into())
        .max_concurrent_uni_streams(8u32.into())
        .stream_receive_window(65_536u32.into())
        .receive_window((1u32 << 20).into())
        .max_idle_timeout(Some(Duration::from_secs(10).try_into()?))
        .keep_alive_interval(Some(Duration::from_secs(3)))
        .datagram_receive_buffer_size(Some(65_536));
    let mut config = quinn::ServerConfig::with_crypto(Arc::new(crypto));
    config
        .transport_config(Arc::new(transport))
        .max_incoming(4 * MAX_UNAUTHENTICATED);
    Ok(config)
}

impl Listener {
    pub(crate) async fn bind(
        addr: SocketAddr,
        registry: Arc<Registry>,
        limits: Limits,
    ) -> Result<Arc<Self>> {
        let certs = CertSet::new(SystemTime::now())?;
        let served = Served::new(certs.presented(SystemTime::now()))?;
        let config = server_config(served.clone())?;
        let quic = quinn::Endpoint::server(config, addr).context("bind the QUIC listener")?;
        let port = quic.local_addr()?.port();
        Ok(Arc::new(Self {
            registry,
            certs: Mutex::new(certs),
            served,
            reload_failed: Mutex::default(),
            quic,
            port,
            limits,
        }))
    }

    pub fn registry(&self) -> &Arc<Registry> {
        &self.registry
    }

    fn certs(&self) -> std::sync::MutexGuard<'_, CertSet> {
        self.certs.lock().unwrap_or_else(|e| e.into_inner())
    }

    pub(crate) fn endpoint(&self) -> Endpoint {
        self.endpoint_at(SystemTime::now())
    }

    /// Ages the certificates first: `rotate`'s sleep is monotonic, so after a suspend it
    /// can lag the wall clock, and a report must never name an expired certificate.
    fn endpoint_at(&self, now: SystemTime) -> Endpoint {
        self.rotate_at(now);
        Endpoint {
            port: self.port,
            addresses: addresses(),
            protos: vec![WT_PROTO],
            certificates: self.certs().report(now, &self.served.hash()),
        }
    }

    /// Age the certificates and present the next one on new connections when it is
    /// due. The 15 s capacity refresh carries a changed list to the control plane, well
    /// inside the 48 h between reporting a certificate and presenting it.
    async fn rotate(self: Arc<Self>) {
        loop {
            tokio::time::sleep(ROTATION_CHECK).await;
            self.rotate_at(SystemTime::now());
        }
    }

    /// One rotation pass at `now`. The clock is a parameter so a test can walk a
    /// certificate's whole life.
    fn rotate_at(&self, now: SystemTime) {
        self.rotate_with(now, Cert::make);
    }

    fn rotate_with(&self, now: SystemTime, make: impl FnOnce(SystemTime) -> Result<Cert>) {
        let mut certs = self.certs();
        if let Err(error) = certs.tick_with(now, make) {
            tracing::warn!(
                token = "webtransport-cert-rotation-failed",
                "WebTransport certificate rotation failed: {error:#}"
            );
        }
        let cert = certs.presented(now);
        if cert.sha256 != self.served.hash() {
            let mut failed = self.reload_failed.lock().unwrap_or_else(|e| e.into_inner());
            match self.served.set(cert) {
                Ok(()) => {
                    *failed = None;
                    tracing::info!(
                        token = "webtransport-cert-rotated",
                        "WebTransport presents its next certificate"
                    );
                }
                // Every capacity report and rotation tick retries; say it once per certificate.
                Err(error) if failed.as_deref() != Some(&cert.sha256) => {
                    *failed = Some(cert.sha256.clone());
                    tracing::warn!(
                        token = "webtransport-cert-reload-failed",
                        "WebTransport could not present its next certificate: {error:#}"
                    );
                }
                Err(_) => {}
            }
        }
    }

    /// Accept at the QUIC layer: a source address is validated with a Retry before it
    /// may hold a slot, and the slot is taken before the handshake, so nothing a peer
    /// does before `hello` escapes the limits or the hello deadline.
    pub(crate) async fn serve(self: Arc<Self>) {
        let gate = Arc::new(Gate {
            limits: self.limits,
            held: Mutex::default(),
        });
        while let Some(incoming) = self.quic.accept().await {
            if !incoming.remote_address_validated() {
                let _ = incoming.retry();
                continue;
            }
            match gate.take(incoming.remote_address().ip()) {
                Some(slot) => {
                    tokio::spawn(connection(incoming, self.registry.clone(), slot));
                }
                None => incoming.refuse(),
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

/// The answer to a client `ping` (§Ping and pong): its fields echoed, with this agent's
/// monotonic clock and its wall clock at the same instant.
fn pong(ping: &Value) -> Value {
    let wall_ms = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs_f64()
        * 1000.0;
    json!({
        "type": "pong",
        "id": ping.get("id"),
        "tc": ping.get("tc"),
        "ts": crate::session::input::host_monotonic_ms(),
        "tw": wall_ms,
    })
}

fn hello_ack(config: &media::VideoConfig, switch_reason: Option<&str>) -> Value {
    let mut ack = json!({
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
        "retransmit_window_ms": retransmit::WINDOW.as_millis() as u64,
    });
    // codec-pyrowave.md §`hello_ack`: required for PyroWave, and always BT.709.
    if config.codec == "pyrowave" {
        ack["video"]["color_space"] = json!({
            "primaries": "bt709", "transfer": "bt709", "matrix": "bt709",
            "full_range": config.full_range,
        });
    }
    if let Some(reason) = switch_reason {
        ack["codec_switch_reason"] = json!(reason);
    }
    ack
}

/// A `nack`'s `frame_seq` and its first 32 `frags` ranges; `None` when malformed.
fn nack_of(message: &Value) -> Option<(u16, Vec<[u16; 2]>)> {
    let frame_seq = u16::try_from(message.get("frame_seq")?.as_u64()?).ok()?;
    let ranges = message
        .get("frags")?
        .as_array()?
        .iter()
        .take(32)
        .filter_map(|r| {
            let r = r.as_array()?;
            let at = |i: usize| u16::try_from(r.get(i)?.as_u64()?).ok();
            Some([at(0)?, at(1)?])
        })
        .collect();
    Some((frame_seq, ranges))
}

/// How long sent video is held for a `nack`: [`retransmit::WINDOW`], or twice the
/// smoothed RTT when that is longer (§Loss recovery).
fn retransmit_window(session: &Session) -> Duration {
    retransmit::WINDOW.max(quinn::Connection::rtt(session) * 2)
}

/// Send `nack` answers; false when the connection is gone. A resend the path's maximum
/// datagram size has shrunk below is dropped and counted: the client gives that frame up.
fn resend(session: &Session, datagrams: Vec<bytes::Bytes>, too_large: &mut u64) -> bool {
    for d in datagrams {
        match session.send_datagram(d) {
            Ok(()) => {}
            Err(web_transport_quinn::SessionError::SendDatagramError(
                quinn::SendDatagramError::TooLarge,
            )) => *too_large += 1,
            Err(_) => return false,
        }
    }
    true
}

/// Hand over the first bidirectional stream as the control stream and refuse every
/// other stream, from the moment the session exists.
async fn streams(session: Session, control: oneshot::Sender<(SendStream, RecvStream)>) {
    let mut control = Some(control);
    loop {
        tokio::select! {
            bi = session.accept_bi() => match bi {
                Ok(pair) => match control.take() {
                    Some(tx) => {
                        let _ = tx.send(pair);
                    }
                    None => {
                        let (mut send, mut recv) = pair;
                        let _ = send.reset(0);
                        let _ = recv.stop(0);
                    }
                },
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

/// Before `hello_ack` every datagram is dropped (§Before `hello`); after it, input
/// (`0x03`) goes to the session's devices. Microphone (`0x05`) is a later ticket.
async fn datagrams(session: Session, authed: Arc<OnceLock<(Arc<Media>, u64)>>) {
    while let Ok(datagram) = session.read_datagram().await {
        let Some((media, client)) = authed.get() else {
            continue;
        };
        if let Some((&CHANNEL_INPUT, body)) = datagram.split_first() {
            if let Ok(json) = std::str::from_utf8(body) {
                media.input_event(*client, json, true);
            }
        }
    }
}

/// One connection, from QUIC accept. The 5 s hello deadline runs from here, over the
/// QUIC and HTTP/3 handshakes as well as the `hello` itself.
async fn connection(incoming: quinn::Incoming, registry: Arc<Registry>, slot: Slot) {
    let deadline = Instant::now() + HELLO_DEADLINE;
    let Ok(Ok(conn)) = timeout_at(deadline, incoming).await else {
        return;
    };
    let session = async { Request::accept(conn.clone()).await.ok()?.ok().await.ok() };
    let session = match timeout_at(deadline, session).await {
        Ok(Some(session)) => session,
        Ok(None) => return,
        Err(_) => {
            let code = web_transport_quinn::proto::error_to_http3(CLOSE_PROTOCOL);
            conn.close(
                quinn::VarInt::from_u64(code).unwrap_or_default(),
                b"no hello in 5 s",
            );
            return;
        }
    };
    let (control_tx, control_rx) = oneshot::channel();
    let others = tokio::spawn(streams(session.clone(), control_tx));
    let authed = Arc::new(OnceLock::new());
    let datagrams = tokio::spawn(datagrams(session.clone(), authed.clone()));
    let hello = async {
        let (send, mut recv) = control_rx.await.map_err(|_| Violation::Gone)?;
        let message = read_message(&mut recv).await?;
        Ok::<_, Violation>((send, recv, message))
    };
    match timeout_at(deadline, hello).await {
        Err(_) => close_session(&session, CLOSE_PROTOCOL, b"no hello in 5 s").await,
        Ok(Err(Violation::Gone)) => {}
        Ok(Err(Violation::Protocol(why))) => {
            close_session(&session, CLOSE_PROTOCOL, why.as_bytes()).await
        }
        Ok(Ok((send, recv, message))) => match serde_json::from_value::<Hello>(message) {
            Ok(hello) if hello.kind == "hello" => {
                authenticate(
                    &session, send, recv, &registry, hello, slot, deadline, &authed,
                )
                .await;
            }
            _ => {
                close_session(
                    &session,
                    CLOSE_PROTOCOL,
                    b"the first message is not a hello",
                )
                .await
            }
        },
    }
    others.abort();
    datagrams.abort();
}

/// Close the session and hold it until the close has run. A `Session` owns the peer's
/// HTTP/3 control and QPACK streams, and dropping the last handle stops them
/// (STOP_SENDING): Chrome treats that as a fatal connection error and never reads the
/// close capsule. Bounded by `CONTROL_WRITE`; the library force-closes after ~3 RTT, so
/// past an RTT of ~0.7 s that bound fires first: the connection is then closed at the
/// QUIC level with the session's code, before the handles drop, so no frame follows it.
async fn close_session(session: &Session, code: u32, reason: &[u8]) {
    session.close(code, reason);
    // Test seam: a test holds the close here, after the capsule is out, to probe while its slot is held.
    #[cfg(test)]
    let _done = tests::hold_close().await;
    if tokio::time::timeout(CONTROL_WRITE, session.closed())
        .await
        .is_err()
    {
        let code = web_transport_quinn::proto::error_to_http3(code);
        quinn::Connection::close(
            session,
            quinn::VarInt::from_u64(code).unwrap_or_default(),
            reason,
        );
    }
}

fn log_refusal(session_id: &str, code: &str) {
    tracing::debug!(
        token = "webtransport-hello-refused",
        session_id = ?shown(session_id),
        "WebTransport hello refused: {code}"
    );
}

/// Every write here is bounded by `deadline`, the hello deadline: a peer that grants no
/// stream credit must not hold a slot, or an attachment, past it.
#[allow(clippy::too_many_arguments)]
async fn authenticate(
    session: &Session,
    mut send: SendStream,
    recv: RecvStream,
    registry: &Registry,
    hello: Hello,
    slot: Slot,
    deadline: Instant,
    authed: &OnceLock<(Arc<Media>, u64)>,
) {
    // Checked before the token is spent: a peer that cannot take 1200-byte-budget
    // datagrams (or none) could never be sent video.
    if session.max_datagram_size() < MIN_DATAGRAM {
        log_refusal(&hello.session_id, "datagrams_too_small");
        close_session(
            session,
            CLOSE_PROTOCOL,
            b"datagrams unsupported or too small",
        )
        .await;
        return;
    }
    let verdict = registry
        .redeem(&hello, SystemTime::now())
        // `end` may run between the redeem and the attach.
        .and_then(|media| {
            media
                .attach(hello.pyrowave, session.rtt())
                .map(|c| (media, c))
                .ok_or(Refusal::SessionEnded)
        });
    // The slot is held until `hello_ack` is written, or on a refusal until the close has
    // run: a peer stalling either counts against the caps like any unauthenticated one.
    let (media, client) = match verdict {
        Ok(attached) => attached,
        Err(refusal) => {
            let (code, close) = refusal.code();
            log_refusal(&hello.session_id, code);
            let error = json!({"type": "error", "code": code});
            let _ = timeout_at(deadline, write_message(&mut send, &error)).await;
            let _ = send.finish();
            close_session(session, close, code.as_bytes()).await;
            return;
        }
    };
    tracing::info!(
        token = "webtransport-client-connected",
        session_id = %hello.session_id,
        "WebTransport client connected from {}",
        session.remote_address()
    );
    if !matches!(
        timeout_at(
            deadline,
            write_message(&mut send, &hello_ack(&client.config, client.reason))
        )
        .await,
        Ok(Ok(()))
    ) {
        media.detach(client.id);
        close_session(session, CLOSE_PROTOCOL, b"control stream stalled").await;
        return;
    }
    drop(slot);
    let _ = authed.set((media.clone(), client.id));
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
    let mut sent = retransmit::Retransmit::default();
    // The frame_seqs of recent PyroWave frames: a NACK naming one is ignored.
    let mut pyrowave_frames = std::collections::VecDeque::with_capacity(PYROWAVE_FRAMES_REMEMBERED);
    let mut nack_budget = retransmit::NackBudget::new(std::time::Instant::now());
    let (mut nacks_capped, mut resends_too_large) = (0u64, 0u64);
    let mut warned_oversized = false;
    // The configuration the client was last sent: a PyroWave unit packs for its size.
    let mut current = client.config.clone();
    // What ends the stream: an optional last message, the close code and its reason.
    const QUIET: (Option<Value>, u32, &str) = (None, CLOSE_NORMAL, "");
    let mut idle = pin!(tokio::time::sleep(CONTROL_IDLE));
    let (message, code, why) = loop {
        tokio::select! {
            () = &mut idle => {
                tracing::info!(
                    token = "webtransport-client-idle",
                    "WebTransport client sent nothing on the control stream for {CONTROL_IDLE:?}"
                );
                break QUIET;
            }
            out = client.rx.recv() => match out {
                Some(Out::Unit(unit)) if unit.config_id != current.config_id => {}
                Some(Out::Unit(unit)) => {
                    let max = session.max_datagram_size().min(media::MAX_DATAGRAM);
                    let Some(datagrams) = media::packetize(&unit, &current, max, &mut pkt_seq) else {
                        if !warned_oversized {
                            warned_oversized = true;
                            tracing::warn!(
                                token = "webtransport-unit-too-large",
                                "a {} byte {} frame cannot be sent (over 65535 fragments, or a \
                                 PyroWave frame the packing refuses); dropped",
                                unit.data.len(),
                                current.codec
                            );
                        }
                        // A PyroWave frame needs no keyframe: the next one repairs everything.
                        if !unit.pyrowave {
                            media.keyframe();
                        }
                        continue;
                    };
                    // codec-pyrowave.md §Loss: a PyroWave frame is never retransmitted, so it is
                    // never kept; a NACK naming it is ignored (`pyrowave_frames`).
                    if unit.pyrowave {
                        if pyrowave_frames.len() == PYROWAVE_FRAMES_REMEMBERED {
                            pyrowave_frames.pop_front();
                        }
                        pyrowave_frames.push_back(unit.frame_seq);
                    } else {
                        let now = std::time::Instant::now();
                        sent.keep(unit.frame_seq, &datagrams, now, retransmit_window(session));
                    }
                    // shortcut: a PyroWave frame (~380 datagrams at 1080p/200) goes as one burst;
                    // §Rate's SHOULD pace over half a frame interval is B09 (#1058).
                    if datagrams.into_iter().try_for_each(|d| session.send_datagram(d)).is_err() {
                        break QUIET;
                    }
                }
                Some(Out::Config(config, reason)) => {
                    current = config.clone();
                    let ack = hello_ack(&config, reason);
                    let write = write_message(&mut send, &ack);
                    if !matches!(tokio::time::timeout(CONTROL_WRITE, write).await, Ok(Ok(()))) {
                        break QUIET;
                    }
                }
                None => break match client.gone.get() {
                    Some(Gone::TakenOver) => (Some(json!({"type": "bye", "reason": "taken_over"})), CLOSE_TAKEN_OVER, ""),
                    _ => (Some(json!({"type": "bye", "reason": "session_ended"})), CLOSE_ENDED, ""),
                },
            },
            message = control_rx.recv() => match message {
                Some(Ok(message)) => {
                    idle.as_mut().reset(Instant::now() + CONTROL_IDLE);
                    match message.get("type").and_then(Value::as_str) {
                        // codec-pyrowave.md §Loss: no keyframe request for a PyroWave frame.
                        Some("keyframe_request") if current.codec != "pyrowave" => media.keyframe(),
                        Some("codec_fallback") => media.switch_off(Some(client.id), "client_decode_error"),
                        // codec-pyrowave.md §Loss: a NACK for a PyroWave frame is ignored: no
                        // retransmission and no nack_miss.
                        Some("nack") if nack_of(&message).is_some_and(|(seq, _)| pyrowave_frames.contains(&seq)) => {}
                        Some("nack") if !nack_budget.take(std::time::Instant::now()) => nacks_capped += 1,
                        Some("nack") => {
                            let answer = nack_of(&message).map(|(frame_seq, ranges)| {
                                let window = retransmit_window(session);
                                let now = std::time::Instant::now();
                                (frame_seq, sent.answer(frame_seq, &ranges, &mut pkt_seq, now, window))
                            });
                            if let Some((frame_seq, answer)) = answer {
                                if !resend(session, answer.resend, &mut resends_too_large) {
                                    break QUIET;
                                }
                                if !answer.missed.is_empty() {
                                    let miss = json!({"type": "nack_miss", "frame_seq": frame_seq, "frags": answer.missed});
                                    let write = write_message(&mut send, &miss);
                                    if !matches!(tokio::time::timeout(CONTROL_WRITE, write).await, Ok(Ok(()))) {
                                        break QUIET;
                                    }
                                }
                            }
                        }
                        Some("input") => {
                            if let Some(event) = message.get("event") {
                                media.input_event(client.id, &event.to_string(), false);
                            }
                        }
                        Some("ping") => {
                            let pong = pong(&message);
                            let write = write_message(&mut send, &pong);
                            if !matches!(tokio::time::timeout(CONTROL_WRITE, write).await, Ok(Ok(()))) {
                                break QUIET;
                            }
                        }
                        Some("bye") => break QUIET,
                        _ => {}
                    }
                }
                Some(Err(Violation::Protocol(why))) => break (None, CLOSE_PROTOCOL, why),
                Some(Err(Violation::Gone)) | None => break QUIET,
            },
        }
    };
    reader.abort();
    media.detach(client.id);
    if nacks_capped + resends_too_large > 0 {
        tracing::debug!(
            token = "webtransport-nack-dropped",
            nacks_capped,
            resends_too_large,
            "WebTransport left nacks unanswered or resends unsent"
        );
    }
    if let Some(message) = message {
        let _ = tokio::time::timeout(CONTROL_WRITE, write_message(&mut send, &message)).await;
        let _ = send.finish();
    }
    close_session(session, code, why.as_bytes()).await;
}
