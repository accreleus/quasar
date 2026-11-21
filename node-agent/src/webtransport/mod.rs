//! Transport #2: the agent's WebTransport listener (`protocol/transport-webtransport.md`,
//! amendment 54). One QUIC listener per agent serves every session on the host; a
//! session's client authenticates with a single-use token the control plane minted and
//! then gets the session's video as datagrams.

mod audio;
mod cert;
mod congestion;
mod fec;
mod media;
mod mic_jitter;
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

pub(crate) use audio::Audio;
pub use cert::CertReport;
use cert::{Cert, CertSet};
pub use media::PyroWaveGrant;
use media::{Attached, Gone, Out, Refused};
pub(crate) use media::{Media, WebRtcSide};

const WT_PROTO: u32 = 1;
const HELLO_DEADLINE: Duration = Duration::from_secs(5);
const MAX_CONTROL_MESSAGE: usize = 65_536;
/// Unauthenticated connections held at once (§Before `hello`).
const MAX_UNAUTHENTICATED: usize = 64;
/// Of those, from one source address; `QUASAR_WT_UNAUTH_PER_SOURCE` overrides it.
const DEFAULT_PER_SOURCE: usize = 4;
/// Below this the 1200-byte datagram budget cannot be met (§Channels, "Datagram size").
const MIN_DATAGRAM: usize = 1100;
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
/// quinn's own default; `congestion::Backlog` reads the queue's fill against it.
const DATAGRAM_BUFFER: usize = 1 << 20;
/// One transmit (up to ten packets where the socket batches them) per 100 ms, only behind a
/// stalled queue on a path that acknowledges nothing: at most about 145 kB/s.
const STARVED_PROBE_INTERVAL: Duration = Duration::from_millis(100);
/// The probe's datagram: channel `0x00`, which no receiver knows and so drops (§Channels). Never
/// empty: Chrome (156, 157) delivers no datagram to the page after an empty one (#1333).
const STARVED_PROBE: &[u8] = &[0x00];
/// How often held video is offered to QUIC again (`congestion::VideoQueue`).
const DRAIN_EVERY: Duration = Duration::from_millis(1);

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

/// Where a client's rate estimate goes: the session's ABR, as `rtpgccbwe`'s estimate does on
/// WebRTC (§Feedback: there is no second bitrate controller).
pub(crate) struct Rate {
    /// The estimate never goes above this: the ABR's ceiling.
    pub(crate) ceiling_kbps: f64,
    /// The floor the ABR's setpoint is clamped to now; it moves with the ladder's rung.
    pub(crate) floor_kbps: Box<dyn Fn() -> f64 + Send + Sync>,
    /// The ABR's setpoint now, what the encoder is asked to send (`congestion::governed_kbps`).
    pub(crate) setpoint_kbps: Box<dyn Fn() -> f64 + Send + Sync>,
    /// `QUASAR_WT_CAPACITY`: v2, or 0.6.3's sender and reading (`legacy`).
    pub(crate) capacity: crate::session::settings::WtCapacity,
    /// False with the ABR off: the estimate is tracked and never written to the encoder,
    /// so nothing would pay for FEC.
    pub(crate) retargets: bool,
    /// A client's first estimate: its loop takes the encoder over (one writer per encoder).
    pub(crate) start: Box<dyn Fn() + Send + Sync>,
    /// Some ABR loop owns the session's encoder right now (`SessionMetrics::abr_owner`).
    /// Ownership outlives the transport that started the loop: a WebRTC peer whose PC fails
    /// takes its hold with it (`Holder::None`) and leaves its loop owning the encoder.
    pub(crate) owns: Box<dyn Fn() -> bool + Send + Sync>,
    /// Displace a loop that still owns the encoder, at the takeover itself (#1294): the
    /// encoder back at its launch point, no owner — not at this client's first estimate,
    /// which a client that sends no feedback (§Feedback) never sends.
    pub(crate) displaced: Box<dyn Fn() + Send + Sync>,
    pub(crate) observe: Box<dyn Fn(f64) + Send + Sync>,
    /// After `observe`, for every estimate that came from a receiver report, never for the
    /// silence decay: what the report showed the link carrying when the link held the
    /// sender back (`congestion::Estimator::measured`), `None` when it did not.
    pub(crate) measured: Box<dyn Fn(Option<f64>) + Send + Sync>,
    /// Amendment 85: FEC telemetry — whether this connection's `hello_ack.fec` named a
    /// scheme, and per protected frame the video and repair datagram payload bytes sent.
    /// FEC exists only where this estimate writes the encoder, so it is reported here.
    pub(crate) fec: Box<dyn Fn(FecEvent) + Send + Sync>,
}

/// What the FEC telemetry sink is told (`Rate::fec`).
pub(crate) enum FecEvent {
    /// Client `client` is now the session's attached connection, and its `hello_ack.fec`
    /// named a scheme (`true`) or did not (`false`). Replaces whatever the last one said.
    Attached { client: u64, fec: bool },
    /// Client `client`'s connection ended. A no-op unless it is still the attached one: a
    /// displaced connection's teardown must not clear its successor's telemetry.
    Detached { client: u64 },
    /// Video datagram payload bytes sent (a frame, or a retransmit) and the repair datagram
    /// payload bytes that followed them (0 for a frame or retransmit with none).
    Sent { video: u64, repair: u64 },
}

/// One client's congestion control: its feedback and the agent's silence clock into an
/// estimate, and the estimate into the session's ABR. Built only for a session whose ABR
/// is armed, so one without it keeps no send log.
struct Control<'a> {
    rate: &'a Rate,
    estimator: congestion::Estimator,
    log: congestion::SendLog,
    started: bool,
    refused: u64,
    /// Set once the client is displaced or the session ended.
    gone: Arc<OnceLock<Gone>>,
    /// §FEC: `Some` when `hello_ack.fec` named `xor`.
    fec: Option<fec::Policy>,
    /// The FEC overhead the ABR was last charged for, which is all the repairs may spend.
    charged: f64,
    governor: congestion::Governor,
}

impl<'a> Control<'a> {
    fn new(
        rate: &'a Rate,
        gone: Arc<OnceLock<Gone>>,
        fec: bool,
        governor: congestion::Governor,
    ) -> Self {
        let now = std::time::Instant::now();
        let legacy = rate.capacity == crate::session::settings::WtCapacity::Legacy;
        governor.legacy(legacy);
        let mut estimator = congestion::Estimator::new(rate.ceiling_kbps, now);
        estimator.legacy = legacy;
        Self {
            rate,
            estimator,
            log: congestion::SendLog::new(now),
            started: false,
            refused: 0,
            gone,
            fec: fec.then(fec::Policy::default),
            charged: 0.0,
            governor,
        }
    }

    fn feedback(&mut self, message: &Value, path: congestion::QuicPath, now: std::time::Instant) {
        let fb = match congestion::feedback_of(message) {
            Ok(fb) => fb,
            Err(field) => {
                if self.refused == 0 {
                    tracing::warn!(
                        token = "webtransport-feedback-invalid",
                        field,
                        "WebTransport feedback out of contract, ignored (§Feedback); later ones \
                         are counted, not logged"
                    );
                }
                self.refused += 1;
                return;
            }
        };
        let estimate = self.estimator.on_feedback(&fb, &mut self.log, now, path);
        // Before the estimate is fed: the encoder's share follows the level this loss sets.
        if let (Some(fec), Some(loss)) = (self.fec.as_mut(), self.estimator.loss()) {
            let was = fec.overhead();
            fec.observe(loss, now);
            if fec.overhead() != was {
                tracing::debug!(
                    token = "webtransport-fec-overhead",
                    "WebTransport FEC overhead {was:.2} → {:.2} at {:.1} % loss",
                    fec.overhead(),
                    loss * 100.0
                );
            }
        }
        if let Some(kbps) = estimate {
            self.feed(kbps, true);
            // QUIC's loss response is given up only while the estimate drives the encoder
            // from live feedback.
            if self.rate.retargets && self.gone.get().is_none() {
                let floor = (self.rate.floor_kbps)();
                self.governor.govern(match self.estimator.legacy {
                    true => kbps.max(floor),
                    false => congestion::governed_kbps(
                        kbps,
                        (self.rate.setpoint_kbps)(),
                        self.charged,
                        floor,
                    ),
                });
            }
        }
    }

    fn tick(&mut self, rtt: Duration, now: std::time::Instant) {
        if let Some(kbps) = self.estimator.on_tick(now, &self.log, rtt) {
            // Feedback has gone silent: the decay holds the encoder at the floor at best, and
            // that may still overrun the path, so Cubic takes it back until feedback returns.
            self.governor.release();
            self.feed(kbps, false);
        }
    }

    /// `report`: a receiver report's estimate, not the silence decay's; its reading
    /// (`Rate::measured`) goes to the ABR first, so the governor judges the estimate with
    /// this report stamped (#1376 review N1).
    fn feed(&mut self, kbps: f64, report: bool) {
        // A client that no longer has the session must never start or re-take the ABR: the
        // encoder is its successor's, and after the session ended the hooks a start
        // registers would leak the encoder (`SessionMetrics::clear_hooks`).
        if self.gone.get().is_some() {
            return;
        }
        if !std::mem::replace(&mut self.started, true) {
            (self.rate.start)();
        }
        // Fed estimate / (1 + share). The governor holds while that is at least (1 − deadband)
        // × its setpoint, so a share inside the deadband moves nothing and video plus FEC is
        // at most estimate / (1 − deadband), the bound video alone has (tests: `governed`).
        // The ABR never goes under its floor: the share is at most what the estimate leaves
        // above that.
        self.charged = self.fec.as_ref().map_or(0.0, |fec| {
            let room = (kbps / (self.rate.floor_kbps)() - 1.0).max(0.0);
            fec.overhead().min(room)
        });
        if report {
            (self.rate.measured)(
                self.estimator
                    .measured()
                    .map(|link| link / (1.0 + self.charged)),
            );
        }
        let loss = self
            .estimator
            .excess_loss()
            .filter(|_| report && !self.estimator.legacy);
        let setpoint = (self.rate.setpoint_kbps)();
        (self.rate.observe)(congestion::abr_estimate(kbps, self.charged, loss, setpoint));
    }

    /// The repair datagrams that follow one frame's video datagrams. A PyroWave frame gets
    /// none: no loss is measured while it is active (§Feedback), so nothing sizes them.
    fn protect(
        &mut self,
        pyrowave: bool,
        fragments: &[bytes::Bytes],
        pkt_seq: &mut u32,
    ) -> Vec<bytes::Bytes> {
        match self.fec.as_mut().filter(|_| !pyrowave) {
            Some(fec) => fec.protect(fragments, self.charged, pkt_seq),
            None => Vec::new(),
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
    /// §Takeover's bound; the token is kept.
    RateLimited,
}

impl Refusal {
    fn code(self) -> (&'static str, u32) {
        match self {
            Self::AuthFailed => ("auth_failed", CLOSE_AUTH),
            Self::NotReady => ("session_not_ready", CLOSE_NORMAL),
            Self::SessionEnded => ("session_ended", CLOSE_ENDED),
            Self::UnsupportedVersion => ("unsupported_version", CLOSE_NORMAL),
            Self::RateLimited => ("takeover_rate_limited", CLOSE_NORMAL),
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
    /// Amendment 67: this connection takes channel `0x06`, the repeat of the previous audio
    /// packet (§Audio redundancy). Absent, as an older client sends it: no copies.
    #[serde(default)]
    audio_red: bool,
    /// The FEC schemes the client decodes (§Hello). Anything but a list names none: FEC is
    /// optional, so a malformed offer costs the client its repairs, not its session.
    #[serde(default)]
    fec: Value,
}

impl Hello {
    fn decodes(&self, scheme: &str) -> bool {
        self.fec
            .as_array()
            .is_some_and(|schemes| schemes.iter().any(|s| s == scheme))
    }
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

    /// Test hook (#1005): the media the session's tap registered, so a live test can send a
    /// `hello` at the session's own client path.
    #[cfg(test)]
    pub(crate) fn media_of(&self, session_id: &str) -> Option<Arc<Media>> {
        self.lock().get(session_id).and_then(|e| e.media.clone())
    }

    /// The session is stopping: its tokens go, and its client gets `bye session_ended`.
    pub fn end(&self, session_id: &str) {
        self.end_if(session_id, |_| true);
    }

    /// `end`, only when `names` holds for the entry's media, checked under the same lock: a
    /// tap dropped after its successor registered under the same id (#1197) must leave that
    /// successor's entry alone.
    fn end_if(&self, session_id: &str, names: impl FnOnce(Option<&Arc<Media>>) -> bool) {
        let media = self
            .lock()
            .get_mut(session_id)
            .filter(|entry| names(entry.media.as_ref()))
            .and_then(|entry| {
                entry.tokens.clear();
                entry.ended_at.get_or_insert_with(SystemTime::now);
                entry.media.take()
            });
        if let Some(media) = media {
            media.end();
        }
    }

    /// Check `hello`, attach its client and consume its token, in one step: two hellos racing
    /// one token attach once. A refusal for a session not ready, or past the takeover bound,
    /// keeps the token, so the client may retry with it.
    /// `rtt` is the connection's round trip, for the PyroWave gate.
    fn redeem(
        &self,
        hello: &Hello,
        now: SystemTime,
        rtt: Duration,
    ) -> Result<(Arc<Media>, Attached), Refusal> {
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
        let Some(media) = entry.media.clone() else {
            return Err(Refusal::NotReady);
        };
        let attached = media
            .attach(hello.pyrowave, rtt)
            .map_err(|refused| match refused {
                Refused::Ended => Refusal::SessionEnded,
                Refused::NotReady => Refusal::NotReady,
                Refused::RateLimited => Refusal::RateLimited,
            })?;
        entry.tokens.remove(index);
        Ok((media, attached))
    }
}

/// The registry a session's transports use when no listener runs: a test's own.
#[cfg(test)]
pub(crate) static TEST_REGISTRY: Mutex<Option<Arc<Registry>>> = Mutex::new(None);

/// The registry the session's transports attach to: the listener's.
pub(crate) fn registry() -> Option<Arc<Registry>> {
    let registry = listener().map(|l| l.registry().clone());
    #[cfg(test)]
    let registry = registry.or_else(|| TEST_REGISTRY.lock().unwrap().clone());
    registry
}

/// Takes the session's video off the encoded-frame boundary for its WebTransport
/// client, and forgets the session when dropped with the transport.
///
/// One live tap per session id (#1197): `attach` registers its media over any earlier tap's
/// (`set_media`, under the registry lock), and a drop ends the entry only while it still
/// names that tap's media. A rebuild's old tap is dropped after its successor attached, so
/// it never ends the successor's entry; nor does a tap whose id was assigned again.
pub(crate) struct Tap {
    registry: Arc<Registry>,
    session_id: String,
    /// The PyroWave encoder of a granted session; dropped after the session is ended here.
    /// Behind a lock so `release` can stop it through `&self`, ahead of the source teardown.
    pyrowave: Mutex<Option<crate::session::pipeline::pyrowave_branch::Encoder>>,
    media: Arc<Media>,
}

impl Tap {
    /// `None` when no listener runs or the session was not assigned WebTransport.
    /// `pyrowave`: the session's config, its grant and the feed off the encode pipeline.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn attach(
        registry: &Arc<Registry>,
        video: &gst::Pad,
        encoder: &gst::Element,
        session_id: &str,
        input: Input,
        audio: audio::Plan,
        pyrowave: Option<(
            &crate::session::SessionConfig,
            PyroWaveGrant,
            Option<crate::session::pipeline::pyrowave_branch::Feed>,
        )>,
        rate: Option<Rate>,
    ) -> Option<Self> {
        let media = Media::new(encoder, input, Audio::new(audio), rate);
        if !registry.set_media(session_id, media.clone()) {
            return None;
        }
        media::tap(video, &media, session_id);
        let pyrowave =
            pyrowave.and_then(|(cfg, grant, feed)| pyrowave_branch(cfg, grant, feed, &media));
        Some(Self {
            registry: registry.clone(),
            session_id: session_id.to_string(),
            pyrowave: Mutex::new(pyrowave),
            media,
        })
    }

    /// The session's audio, for the audio pipeline to tap at its Opus encoder.
    pub(crate) fn audio(&self) -> Arc<Audio> {
        self.media.audio().clone()
    }

    /// What the runner offers this session's client.
    pub(crate) fn plan(cfg: &crate::session::SessionConfig) -> audio::Plan {
        audio::Plan::for_session(cfg)
    }

    /// A client is attached and being sent video: the session's idle reap counts it.
    pub(crate) fn attached(&self) -> bool {
        self.media.attached()
    }

    /// The session's one media client across both transports, which the WebRTC side
    /// injects and releases through.
    pub(crate) fn media(&self) -> &Arc<Media> {
        &self.media
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
    // Rate: the encoder budgets `bitrate` / fps per frame, starting at the target; each
    // client's stream loop drives it live (`congestion::PyroRate`).
    let started = crate::session::pipeline::pyrowave_branch::start_encoder(
        cfg,
        feed,
        congestion::pyro_budget(f64::from(grant.target_kbps)),
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
        Ok(encoder) => {
            media.pyrowave_encoder(encoder.enc.clone());
            Some(encoder)
        }
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

impl Tap {
    /// Stops the PyroWave encoder now (idempotent; a no-op without one). It shares the GPU
    /// queue and a compositor ring frame with the source, so a session stop calls this before
    /// the source teardown (`Transport::release`, #535, #1228).
    pub(crate) fn release(&self) {
        let encoder = self
            .pyrowave
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take();
        drop(encoder);
    }
}

impl Drop for Tap {
    fn drop(&mut self) {
        let own = &self.media;
        self.registry
            .end_if(&self.session_id, |m| m.is_some_and(|m| Arc::ptr_eq(m, own)));
        // Its own media too, which the entry may no longer name.
        self.media.end();
        self.release();
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
    /// What each connection is accepted with, but for its own transport.
    config: quinn::ServerConfig,
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
    let mut config = quinn::ServerConfig::with_crypto(Arc::new(crypto));
    // A placeholder: a transport's congestion factory, and so its `Governor`, would be shared
    // by every connection, so `serve` accepts each with its own (`accept_with`).
    config
        .transport_config(Arc::new(transport(congestion::Governor::default())))
        .max_incoming(4 * MAX_UNAUTHENTICATED);
    Ok(config)
}

/// One connection's transport: its congestion window is set through `governor`.
fn transport(governor: congestion::Governor) -> quinn::TransportConfig {
    // Bounds on what one peer can make the agent hold. Two bidirectional streams are the
    // CONNECT stream and the control stream; HTTP/3 needs three unidirectional ones.
    let mut transport = quinn::TransportConfig::default();
    transport
        .max_concurrent_bidi_streams(2u32.into())
        .max_concurrent_uni_streams(8u32.into())
        .stream_receive_window(65_536u32.into())
        .receive_window((1u32 << 20).into())
        .max_idle_timeout(Some(quinn::VarInt::from_u32(10_000).into()))
        .keep_alive_interval(Some(Duration::from_secs(3)))
        .datagram_receive_buffer_size(Some(65_536))
        .datagram_send_buffer_size(DATAGRAM_BUFFER)
        .congestion_controller_factory(Arc::new(congestion::WindowFactory(governor)));
    transport
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
        let quic =
            quinn::Endpoint::server(config.clone(), addr).context("bind the QUIC listener")?;
        let port = quic.local_addr()?.port();
        Ok(Arc::new(Self {
            registry,
            certs: Mutex::new(certs),
            served,
            reload_failed: Mutex::default(),
            quic,
            config,
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
                    let governor = congestion::Governor::default();
                    let mut config = self.config.clone();
                    config.transport_config(Arc::new(transport(governor.clone())));
                    tokio::spawn(connection(
                        incoming,
                        Arc::new(config),
                        governor,
                        self.registry.clone(),
                        slot,
                    ));
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

fn hello_ack(
    config: &media::VideoConfig,
    audio: &Audio,
    switch_reason: Option<&str>,
    fec: bool,
    audio_red: bool,
) -> Value {
    let (stream_audio, mic) = audio.describe();
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
        "audio": stream_audio,
        "mic": mic,
        "fec": fec.then_some("xor"),
        "retransmit_window_ms": retransmit::WINDOW.as_millis() as u64,
    });
    // §Audio redundancy: only a client that asked, and only where the session has stream audio.
    if audio_red && ack["audio"].is_object() {
        ack["audio"]["redundancy"] = json!(1);
    }
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

/// The backlog guard's verdict on the next video unit or `nack` answer; a skip that wants a
/// keyframe has asked for it. `held`: video bytes not yet handed to QUIC; `acks`:
/// [`congestion::Governor::acks`].
fn admit(
    backlog: &mut congestion::Backlog,
    session: &Session,
    media: &Media,
    keyframe: bool,
    held: usize,
    acks: u64,
) -> congestion::Admit {
    let queued = DATAGRAM_BUFFER.saturating_sub(session.datagram_send_buffer_space()) + held;
    let hold = retransmit_window(session);
    backlog.acknowledge(acks);
    let admit = backlog.admit(queued, keyframe, std::time::Instant::now(), hold);
    if admit == congestion::Admit::SkipForKeyframe {
        media.keyframe();
    }
    admit
}

/// How a stream ends when no keyframe gets through its datagram queue: as an unusable
/// encoder ends it. The browser ignores the `error` and reconnects over WebTransport once on
/// the close; a second loss falls back to WebRTC (`stream_lost`).
fn stalled() -> (Option<Value>, u32, &'static str) {
    let message = "no keyframe got through the datagram queue";
    (
        Some(json!({"type": "error", "code": "internal", "message": message})),
        CLOSE_NORMAL,
        "",
    )
}

/// The most bytes of one PyroWave frame handed to QUIC back-to-back (#1606). A ~435 KB frame
/// (1080p60 at 200 Mbit/s) sent whole lost thousands of datagrams after the agent's send; a
/// half-size frame (~215 KB) was clean in every lab run. 128 KiB (about 110 datagrams, four
/// slices of that frame) sits well under the clean burst and is a round number.
const PYRO_BURST_BYTES: usize = 128 * 1024;

/// The frame rate the slice spacing assumes when the configuration carries none.
const PACING_FALLBACK_FPS: u32 = 60;

/// The spacing between a frame's slices: a quarter of the frame interval over its gaps. A
/// one-slice frame gets the same quarter, so the next frame's first slice still waits it out.
fn pacing_gap(fps: i32, slices: usize) -> std::time::Duration {
    let fps = u32::try_from(fps)
        .ok()
        .filter(|f| *f > 0)
        .unwrap_or(PACING_FALLBACK_FPS);
    std::time::Duration::from_secs(1) / fps / 4 / (slices.max(2) as u32 - 1)
}

/// The slices a frame's datagrams are handed over in: each a datagram count and its offset from
/// the frame's start. PyroWave is cut so no slice passes `PYRO_BURST_BYTES`, the slices spread
/// evenly over a quarter of the frame interval (a target: `Pacing` keeps the spacing, not the
/// frame-relative offsets). Every other codec is one slice at once.
fn burst_plan(
    datagrams: &[bytes::Bytes],
    pyrowave: bool,
    fps: i32,
) -> Vec<(usize, std::time::Duration)> {
    let mut counts = vec![0usize];
    let mut bytes = 0;
    for d in datagrams {
        if pyrowave && bytes > 0 && bytes + d.len() > PYRO_BURST_BYTES {
            counts.push(0);
            bytes = 0;
        }
        bytes += d.len();
        *counts.last_mut().expect("one slice") += 1;
    }
    if counts.len() == 1 {
        return vec![(counts[0], std::time::Duration::ZERO)];
    }
    let gap = pacing_gap(fps, counts.len());
    counts
        .into_iter()
        .zip(0..)
        .map(|(n, i)| (n, gap * i))
        .collect()
}

/// One slice of a PyroWave frame, with the spacing that follows it.
struct Slice {
    frame: u16,
    gap: Duration,
    datagrams: Vec<bytes::Bytes>,
}

/// The most frames `Pacing` holds: a frame never waits behind more than one older frame.
const PACING_FRAMES: usize = 2;

/// PyroWave slices not yet handed to QUIC, released one per `gap` by the send loop's own
/// deadline arm so audio, input, feedback and pongs run between them. Slices keep their order
/// across frames (a frame arriving mid-way queues behind the older one); a third frame drops the
/// oldest frame's unsent tail, which the next frame repairs (codec-pyrowave.md §Loss).
#[derive(Default)]
struct Pacing {
    queue: std::collections::VecDeque<Slice>,
    /// When the next slice may go: the last hand-off plus its gap, kept across frames and an
    /// empty queue so no two slices are closer than a gap. Past (or `None`) when idle: a slice
    /// goes at once.
    ready: Option<Instant>,
}

impl Pacing {
    fn push(&mut self, frame: u16, gap: Duration, slices: Vec<Vec<bytes::Bytes>>) {
        let frame_slices = slices.into_iter().map(|datagrams| Slice {
            frame,
            gap,
            datagrams,
        });
        self.queue.extend(frame_slices);
        while self
            .queue
            .iter()
            .map(|s| s.frame)
            .collect::<Vec<_>>()
            .chunk_by(|a, b| a == b)
            .count()
            > PACING_FRAMES
        {
            let oldest = self.queue[0].frame;
            while self.queue.front().is_some_and(|s| s.frame == oldest) {
                self.queue.pop_front();
            }
        }
    }

    /// When the next slice is due; `None` with nothing queued.
    fn deadline(&self) -> Option<Instant> {
        (!self.queue.is_empty()).then(|| self.ready.unwrap_or_else(Instant::now))
    }

    /// The next slice if it is due. A late caller gets one slice, and the rest are spaced from
    /// `now`, never from the missed deadline: no catch-up burst. That holds at a frame boundary
    /// too: a frame queued after the last pop waits out the previous slice's gap.
    fn pop(&mut self, now: Instant) -> Option<Vec<bytes::Bytes>> {
        if self.ready.is_some_and(|t| t > now) {
            return None;
        }
        let slice = self.queue.pop_front()?;
        self.ready = Some(now + slice.gap);
        Some(slice.datagrams)
    }
}

/// The payload bytes of video datagrams: what amendment 85's FEC share is measured against.
fn video_payload(datagrams: &[bytes::Bytes]) -> u64 {
    datagrams
        .iter()
        .map(|d| d.len().saturating_sub(media::HEADER) as u64)
        .sum()
}

/// Hand QUIC the held video that fits beside audio (`VideoQueue::room`), sized at the
/// governed rate or, when it is lower, the estimate itself: the governor never goes under
/// the ABR floor, the link may. False when the connection is gone. A resend sends only while
/// its fragment's budget allows; a video datagram the path's maximum datagram size has
/// shrunk below is skipped, and its frame cannot complete, so a keyframe is asked for. Nothing
/// held past the NACK hold is handed over (`VideoQueue::feed`).
#[allow(clippy::too_many_arguments)]
fn feed(
    session: &Session,
    video: &mut congestion::VideoQueue,
    control: Option<&mut Control>,
    pyro: Option<&mut congestion::PyroRate>,
    pyrowave: bool,
    sent: &mut retransmit::Retransmit,
    media: &Media,
    handed: &mut u64,
    fec: bool,
    backlog: &mut congestion::Backlog,
    governor: &congestion::Governor,
) -> bool {
    let rates = control.as_ref().map_or((None, None), |c| {
        congestion::video_rates(&c.governor, &c.estimator, pyrowave)
    });
    let queued = DATAGRAM_BUFFER.saturating_sub(session.datagram_send_buffer_space());
    let now = std::time::Instant::now();
    let (room, frame) = video.budget(rates, queued, DATAGRAM_BUFFER, now);
    let log = match pyro {
        Some(rate) if pyrowave => Some(&mut rate.log),
        _ => control.filter(|_| !pyrowave).map(|c| &mut c.log),
    };
    let too_large = video.too_large;
    let hold = (now, retransmit_window(session));
    let alive = video.feed(room, frame, hold, log, |held| {
        if held.resend && !sent.allows(&held.datagram) {
            return congestion::Handed::Skipped;
        }
        let resend = held.resend.then(|| held.datagram.clone());
        match session.send_datagram(held.datagram) {
            Ok(()) => {
                *handed += 1;
                if let Some(d) = resend {
                    sent.charge(&d);
                    // A retransmit is video sent in the window too (amendment 85), counted
                    // once it is handed over: a resend skipped or dropped never was.
                    if let Some(rate) = media.rate().filter(|_| fec) {
                        (rate.fec)(FecEvent::Sent {
                            video: video_payload(&[d]),
                            repair: 0,
                        });
                    }
                }
                congestion::Handed::Sent
            }
            Err(web_transport_quinn::SessionError::SendDatagramError(
                quinn::SendDatagramError::TooLarge,
            )) => congestion::Handed::TooLarge,
            Err(_) => congestion::Handed::Gone,
        }
    });
    if video.too_large > too_large {
        if too_large == 0 {
            tracing::warn!(
                token = "webtransport-video-too-large",
                max = session.max_datagram_size(),
                "a held video datagram is over the path's datagram size now; skipped, and a \
                 keyframe asked for (later ones are counted)"
            );
        }
        media.keyframe();
    }
    if video.orphaned() {
        backlog.skip_to_keyframe(governor.acks());
    }
    alive
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
/// (`0x03`) goes to the session's devices and the microphone (`0x05`) to its mic sink.
async fn datagrams(session: Session, authed: Arc<OnceLock<(Arc<Media>, u64)>>) {
    while let Ok(datagram) = session.read_datagram().await {
        let Some((media, client)) = authed.get() else {
            continue;
        };
        match datagram.split_first() {
            Some((&CHANNEL_INPUT, body)) => {
                if let Ok(json) = std::str::from_utf8(body) {
                    media.input_event(*client, json, true);
                }
            }
            Some((&audio::CHANNEL_MIC, body)) => media.audio().mic_packet(*client, body),
            _ => {}
        }
    }
}

/// One connection, from QUIC accept. The 5 s hello deadline runs from here, over the
/// QUIC and HTTP/3 handshakes as well as the `hello` itself.
async fn connection(
    incoming: quinn::Incoming,
    config: Arc<quinn::ServerConfig>,
    governor: congestion::Governor,
    registry: Arc<Registry>,
    slot: Slot,
) {
    let deadline = Instant::now() + HELLO_DEADLINE;
    let Ok(connecting) = incoming.accept_with(config) else {
        return;
    };
    let Ok(Ok(conn)) = timeout_at(deadline, connecting).await else {
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
                    &session, send, recv, &registry, hello, slot, deadline, &authed, governor,
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
    governor: congestion::Governor,
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
    let verdict = registry.redeem(&hello, SystemTime::now(), session.rtt());
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
    // #1294: the takeover displaces a loop that still owns the encoder, here rather than
    // under the registry lock `redeem` holds (the reset writes the encoder, and a rung
    // restore restarts its session), and before this client's `hello_ack` lets it feed.
    media.displace_stale_abr();
    // §FEC: repairs are paid for by lowering the encoder's rate, so only an ABR that
    // writes it sends them.
    let fec = media.rate().is_some_and(|rate| rate.retargets) && hello.decodes("xor");
    tracing::info!(
        token = "webtransport-client-connected",
        session_id = %hello.session_id,
        "WebTransport client connected from {}",
        session.remote_address()
    );
    if !matches!(
        timeout_at(
            deadline,
            write_message(
                &mut send,
                &hello_ack(
                    &client.config,
                    media.audio(),
                    client.reason,
                    fec,
                    hello.audio_red
                )
            )
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
    stream(
        session,
        send,
        recv,
        &media,
        client,
        fec,
        hello.audio_red,
        governor,
    )
    .await;
}

fn quic_path(session: &Session) -> congestion::QuicPath {
    let path = quinn::Connection::stats(session).path;
    congestion::QuicPath {
        rtt: path.rtt,
        cwnd_bytes: path.cwnd,
        congestion_events: path.congestion_events,
        queued_bytes: DATAGRAM_BUFFER.saturating_sub(session.datagram_send_buffer_space()) as u64,
    }
}

/// codec-pyrowave.md §Rate: a controller seeded at the target at each switch on, kept while
/// the client stays on PyroWave, gone when it leaves.
fn pyro_rate(
    media: &Media,
    config: &media::VideoConfig,
    kept: Option<congestion::PyroRate>,
) -> Option<congestion::PyroRate> {
    if config.codec != "pyrowave" {
        return None;
    }
    kept.or_else(|| {
        let grant = media.pyrowave_grant()?;
        Some(congestion::PyroRate::new(
            grant.target_kbps,
            grant.floor_kbps,
            std::time::Instant::now(),
            media
                .rate()
                .is_some_and(|r| r.capacity == crate::session::settings::WtCapacity::Legacy),
        ))
    })
}

fn pyro_act(
    media: &Media,
    client: &Attached,
    pyro: &mut Option<congestion::PyroRate>,
    control: Option<&mut Control>,
    verdict: congestion::PyroVerdict,
) {
    // A displaced client's loop must not write its successor's encoder.
    if client.gone.get().is_some() {
        return;
    }
    match verdict {
        congestion::PyroVerdict::Hold => {}
        congestion::PyroVerdict::Budget(kbps) => media.pyrowave_budget(kbps),
        congestion::PyroVerdict::StepDown { estimate_kbps } => {
            tracing::info!(
                token = "pyrowave-congestion-step-down",
                estimate_kbps = estimate_kbps as u64,
                floor_kbps = media.pyrowave_grant().map_or(0, |g| g.floor_kbps),
                "PyroWave estimate under the floor for 1 s; stepping down to the standard codec"
            );
            *pyro = None;
            if let Some(control) = control {
                control.estimator.resume_at(estimate_kbps);
            }
            media.switch_off(Some(client.id), "congestion");
        }
    }
}

/// Hand the client its video until it leaves, is displaced or the session ends.
#[allow(clippy::too_many_arguments)]
async fn stream(
    session: &Session,
    mut send: SendStream,
    mut recv: RecvStream,
    media: &Arc<Media>,
    mut client: Attached,
    fec: bool,
    audio_red: bool,
    governor: congestion::Governor,
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
    let mut pyrowave_frames = retransmit::PyroWaveFrames::default();
    let mut nack_budget = retransmit::NackBudget::new(std::time::Instant::now());
    let mut nacks_capped = 0u64;
    let mut warned_oversized = false;
    // Amendment 85: whether this connection is sent repairs, so the session's windows carry
    // the FEC keys (0 repair) exactly while it is. The publish refuses a client the session
    // no longer has, under the lock that replaces it: a displaced connection's attach lands
    // after the successor's, whether it arrived before the successor's takeover (#1554) or
    // between that check and its publish (#1569), so it must not overwrite the successor's
    // telemetry.
    media.fec_attach(client.id, fec);
    let mut control = media
        .rate()
        .map(|rate| Control::new(rate, client.gone.clone(), fec, governor.clone()));
    let mut backlog = congestion::Backlog::default();
    let mut video = congestion::VideoQueue::default();
    // Re-armed while video is held: QUIC says nothing when its queue drains.
    let mut drain = pin!(tokio::time::sleep(Duration::ZERO));
    // Every datagram given to quinn: what it put on the wire is in its stats.
    let mut handed = 0u64;
    let mut starved_probes = 0u64;
    let mut probe_tick = tokio::time::interval(STARVED_PROBE_INTERVAL);
    probe_tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    let mut control_tick = tokio::time::interval(congestion::REPORT_EVERY);
    control_tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    // Nothing is sent before `hello_ack`: what queued up behind it is stale.
    while client.audio.try_recv().is_ok() {}
    let mut audio_open = true;
    // §Audio redundancy: the packet the next one repeats; a connection starts with none.
    let mut previous_audio: Option<audio::Packet> = None;
    // The configuration the client was last sent: a PyroWave unit packs for its size.
    let mut current = client.config.clone();
    let mut pyro = pyro_rate(media, &current, None);
    let mut pyro_refused = 0u64;
    let mut pacing = Pacing::default();
    // What ends the stream: an optional last message, the close code and its reason.
    const QUIET: (Option<Value>, u32, &str) = (None, CLOSE_NORMAL, "");
    let mut idle = pin!(tokio::time::sleep(CONTROL_IDLE));
    let (message, code, why) = loop {
        tokio::select! {
            _ = probe_tick.tick() => {
                let queued = DATAGRAM_BUFFER.saturating_sub(session.datagram_send_buffer_space())
                    + video.bytes();
                let acks = governor.acks();
                // Only a session the estimate can govern (#1332's invariant): with ABR off (an
                // ABR loop exists then, but retargets nothing), and while PyroWave is active,
                // QUIC's own congestion control stands untouched. The estimate itself lets go
                // during an outage (its feedback is silent), which is when this must still run,
                // so it asks what the session is, not who governs now.
                let probe = media.rate().is_some_and(|r| r.retargets)
                    && current.codec != "pyrowave"
                    && backlog.probe(queued, acks);
                governor.probe(probe);
                if probe {
                    // A datagram wakes quinn; the client drops it. The allowance lets
                    // one transmit of what is queued past a closed window: up to ten packets
                    // where the socket batches them, so at most about 145 kB/s, and only while
                    // nothing at all is being acknowledged.
                    starved_probes += 1;
                    handed += 1;
                    if session.send_datagram(bytes::Bytes::from_static(STARVED_PROBE)).is_err() {
                        break QUIET;
                    }
                }
            }
            // §Feedback: while PyroWave is active its own rate controller stands in for the
            // ABR, so the ABR's estimate is fed nothing then.
            _ = control_tick.tick(), if pyro.is_some() || (control.is_some() && current.codec != "pyrowave") => {
                if let Some(rate) = pyro.as_mut() {
                    let verdict = rate.on_tick(std::time::Instant::now(), quic_path(session));
                    pyro_act(media, &client, &mut pyro, control.as_mut(), verdict);
                } else if let Some(control) = control.as_mut() {
                    control.tick(quinn::Connection::rtt(session), std::time::Instant::now());
                }
            }
            // #1606: the next PyroWave slice, one at a time so the arms around it keep running.
            _ = tokio::time::sleep_until(pacing.deadline().unwrap_or_else(Instant::now)), if pacing.deadline().is_some() => {
                if let Some(slice) = pacing.pop(Instant::now()) {
                    let now = std::time::Instant::now();
                    for datagram in slice {
                        video.push(congestion::Held { datagram, logged: pyro.is_some(), resend: false, queued_at: now, keyframe: true }, DATAGRAM_BUFFER);
                    }
                    if !feed(session, &mut video, control.as_mut(), pyro.as_mut(), current.codec == "pyrowave", &mut sent, media, &mut handed, fec, &mut backlog, &governor) {
                        break QUIET;
                    }
                }
            }
            packet = client.audio.recv(), if audio_open => match packet {
                Some(packet) => {
                    // §Audio redundancy: the packet before this one, immediately before it.
                    // Straight to QUIC, ahead of held video (#1321).
                    if let Some(previous) = previous_audio.as_ref().filter(|p| audio::repeats(p, &packet)) {
                        let seq = video.take_seq();
                        handed += 1;
                        if session.send_datagram(audio::repeated(previous, seq)).is_err() {
                            break QUIET;
                        }
                    }
                    let seq = video.take_seq();
                    // Not in the send log: the estimate is the video ABR's, and on WebRTC
                    // audio is a peer connection GCC never sees. Feedback naming an audio
                    // `pkt_seq` finds no record and counts for nothing. The `0x06` copy above is
                    // an audio datagram too, and is left out for the same reason.
                    handed += 1;
                    if session.send_datagram(audio::datagram(&packet, seq)).is_err() {
                        break QUIET;
                    }
                    if audio_red {
                        previous_audio = Some(packet);
                    }
                }
                // Displaced or ended: the video queue says which, next.
                None => audio_open = false,
            },
            () = &mut drain, if !video.is_empty() => {
                if !feed(session, &mut video, control.as_mut(), pyro.as_mut(), current.codec == "pyrowave", &mut sent, media, &mut handed, fec, &mut backlog, &governor) {
                    break QUIET;
                }
                drain.as_mut().reset(Instant::now() + DRAIN_EVERY);
            }
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
                    if let Some(rate) = pyro.as_mut().filter(|_| unit.pyrowave) {
                        rate.offer(unit.data.len());
                    }
                    match admit(&mut backlog, session, media, unit.keyframe || unit.pyrowave, video.bytes(), governor.acks()) {
                        congestion::Admit::Send => {}
                        congestion::Admit::GiveUp => break stalled(),
                        _ => continue,
                    }
                    let now = std::time::Instant::now();
                    let max = session.max_datagram_size().min(media::MAX_DATAGRAM);
                    pkt_seq = video.next_seq();
                    let Some(mut datagrams) = media::packetize(&unit, &current, max, &mut pkt_seq) else {
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
                        pyrowave_frames.remember(unit.frame_seq);
                    } else {
                        sent.keep(unit.frame_seq, &datagrams, now, retransmit_window(session));
                    }
                    // shortcut: repairs follow the whole frame, so past 48 fragments they can
                    // arrive after the client's NACK hold; send them per block if keyframe
                    // loss shows in the bench.
                    if let Some(control) = control.as_mut() {
                        let repairs = control.protect(unit.pyrowave, &datagrams, &mut pkt_seq);
                        // Every video frame while a scheme is on, one that got no repair too
                        // (a small frame banks its budget; a PyroWave frame gets none): the
                        // share is repair over ALL the video sent (amendment 85).
                        if let Some(rate) = media.rate().filter(|_| fec) {
                            // Payload, not datagram bytes: the header is per-datagram overhead
                            // on both sides and would dilute the ratio differently.
                            let repair: u64 = repairs
                                .iter()
                                .map(|d| d.len().saturating_sub(fec::HEADER) as u64)
                                .sum();
                            (rate.fec)(FecEvent::Sent { video: video_payload(&datagrams), repair });
                        }
                        datagrams.extend(repairs);
                    }
                    let logged = if unit.pyrowave { pyro.is_some() } else { control.is_some() };
                    let keyframe = unit.keyframe || unit.pyrowave;
                    if unit.pyrowave {
                        // #1606: slices go out one per deadline, from the pacing arm.
                        let plan = burst_plan(&datagrams, true, current.fps);
                        let gap = pacing_gap(current.fps, plan.len());
                        let mut rest = datagrams.into_iter();
                        let slices = plan.iter().map(|(n, _)| rest.by_ref().take(*n).collect()).collect();
                        pacing.push(unit.frame_seq, gap, slices);
                        datagrams = pacing.pop(Instant::now()).unwrap_or_default();
                    }
                    for datagram in datagrams {
                        video.push(congestion::Held { datagram, logged, resend: false, queued_at: now, keyframe }, DATAGRAM_BUFFER);
                    }
                    if !feed(session, &mut video, control.as_mut(), pyro.as_mut(), current.codec == "pyrowave", &mut sent, media, &mut handed, fec, &mut backlog, &governor) {
                        break QUIET;
                    }
                    drain.as_mut().reset(Instant::now() + DRAIN_EVERY);
                }
                Some(Out::Config(config, reason)) => {
                    // The client drops a datagram of a configuration it has left: none goes
                    // ahead of the new one's keyframe.
                    video.clear();
                    if config.codec != "pyrowave" {
                        pyrowave_frames.forget();
                    }
                    current = config.clone();
                    pacing.queue.clear();
                    pyro = pyro_rate(media, &current, pyro);
                    let ack = hello_ack(&config, media.audio(), reason, fec, audio_red);
                    let write = write_message(&mut send, &ack);
                    if !matches!(tokio::time::timeout(CONTROL_WRITE, write).await, Ok(Ok(()))) {
                        break QUIET;
                    }
                }
                None => break match client.gone.get() {
                    // A rebuild replaced the encoder: a lost connection, not an end, so the
                    // client mints a fresh token and reconnects onto the new encoder
                    // (§Reconnect, #1197). No `bye` and code 0 is what says "lost link".
                    Some(Gone::Rebuilt) => QUIET,
                    Some(Gone::TakenOver) => (Some(json!({"type": "bye", "reason": "taken_over"})), CLOSE_TAKEN_OVER, ""),
                    Some(Gone::Unusable) => (
                        Some(json!({"type": "error", "code": "internal", "message": "the encoder sent no usable keyframe"})),
                        CLOSE_NORMAL,
                        "",
                    ),
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
                        Some("nack") if nack_of(&message).is_some_and(|(seq, _)| pyrowave_frames.ignores(seq)) => {}
                        Some("nack") if !nack_budget.take(std::time::Instant::now()) => nacks_capped += 1,
                        Some("nack") => match admit(&mut backlog, session, media, false, video.bytes(), governor.acks()) {
                            congestion::Admit::GiveUp => break stalled(),
                            // Behind a stalled queue a resend is as stale as new video.
                            congestion::Admit::Skip | congestion::Admit::SkipForKeyframe => {}
                            congestion::Admit::Send => {
                                let answer = nack_of(&message).map(|(frame_seq, ranges)| {
                                    let window = retransmit_window(session);
                                    let now = std::time::Instant::now();
                                    pkt_seq = video.next_seq();
                                    (frame_seq, sent.answer(frame_seq, &ranges, &mut pkt_seq, now, window))
                                });
                                if let Some((frame_seq, answer)) = answer {
                                    let (logged, now) = (control.is_some(), std::time::Instant::now());
                                    for datagram in answer.resend {
                                        video.push(congestion::Held { datagram, logged, resend: true, queued_at: now, keyframe: false }, DATAGRAM_BUFFER);
                                    }
                                    if !feed(session, &mut video, control.as_mut(), pyro.as_mut(), current.codec == "pyrowave", &mut sent, media, &mut handed, fec, &mut backlog, &governor) {
                                        break QUIET;
                                    }
                                    drain.as_mut().reset(Instant::now() + DRAIN_EVERY);
                                    if !answer.missed.is_empty() {
                                        let miss = json!({"type": "nack_miss", "frame_seq": frame_seq, "frags": answer.missed});
                                        let write = write_message(&mut send, &miss);
                                        if !matches!(tokio::time::timeout(CONTROL_WRITE, write).await, Ok(Ok(()))) {
                                            break QUIET;
                                        }
                                    }
                                }
                            }
                        },
                        Some("feedback") if current.codec == "pyrowave" => {
                            let fb = congestion::feedback_of(&message);
                            if let Err(field) = fb {
                                if pyro_refused == 0 {
                                    tracing::warn!(
                                        token = "pyrowave-feedback-invalid",
                                        field,
                                        "PyroWave feedback out of contract, ignored (§Feedback); later \
                                         ones are counted, not logged"
                                    );
                                }
                                pyro_refused += 1;
                            }
                            if let (Some(rate), Ok(fb)) = (pyro.as_mut(), fb) {
                                let verdict = rate.on_feedback(&fb, std::time::Instant::now(), quic_path(session));
                                pyro_act(media, &client, &mut pyro, control.as_mut(), verdict);
                            }
                        }
                        Some("feedback") => {
                            if let Some(control) = control.as_mut() {
                                let now = std::time::Instant::now();
                                control.feedback(&message, quic_path(session), now);
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
    // Amendment 85: this connection is over, so the session's windows stop claiming FEC until
    // another client negotiates it (a WebRTC peer's windows never carry the keys).
    if let Some(rate) = media.rate() {
        (rate.fec)(FecEvent::Detached { client: client.id });
    }
    let quic = quinn::Connection::stats(session);
    tracing::info!(
        token = "webtransport-client-quic-stats",
        datagrams_handed_incl_audio_and_resends = handed,
        datagrams_sent = quic.frame_tx.datagram,
        units_and_nack_answers_skipped_total = backlog.skipped,
        held_video_dropped = video.dropped,
        held_video_stale = video.stale,
        video_datagrams_too_large = video.too_large,
        starved_probes,
        governed = governor.governs(),
        governed_kbps = governor.kbps(),
        lost_packets = quic.path.lost_packets,
        congestion_events = quic.path.congestion_events,
        cwnd_bytes = quic.path.cwnd,
        rtt_ms = quic.path.rtt.as_millis() as u64,
        "WebTransport client left; what quinn did with its datagrams"
    );
    let resends_too_large = video.resends_too_large;
    if nacks_capped + resends_too_large > 0 {
        tracing::debug!(
            token = "webtransport-nack-dropped",
            nacks_capped,
            resends_too_large,
            "WebTransport left nacks unanswered or resends unsent"
        );
    }
    if pyro_refused > 0 {
        tracing::debug!(
            token = "pyrowave-feedback-refused",
            refused = pyro_refused,
            "PyroWave feedback messages ignored as out of contract"
        );
    }
    if let Some(refused) = control.as_ref().map(|c| c.refused).filter(|&n| n > 0) {
        tracing::debug!(
            token = "webtransport-feedback-refused",
            refused,
            "WebTransport feedback messages ignored as out of contract"
        );
    }
    if let Some(message) = message {
        let _ = tokio::time::timeout(CONTROL_WRITE, write_message(&mut send, &message)).await;
        let _ = send.finish();
    }
    close_session(session, code, why.as_bytes()).await;
}
