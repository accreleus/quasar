//! A session's video as the WebTransport client gets it: access units taken off the
//! encoded-frame boundary, and their packetizing into `0x01` datagrams
//! (`protocol/transport-webtransport.md` §Video).

use std::borrow::Cow;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant, SystemTime};

use bytes::{BufMut, Bytes, BytesMut};
use gstreamer as gst;
use gstreamer::prelude::*;
use tokio::sync::mpsc;

use super::audio::{self, Audio};

pub(crate) const HEADER: usize = 25;
/// The most a datagram may carry at the 1200-byte UDP budget (§Channels, "Datagram size").
pub(crate) const MAX_DATAGRAM: usize = 1148;
const CHANNEL_VIDEO: u8 = 0x01;
const FLAG_KEYFRAME: u8 = 1;
const FLAG_DISCARDABLE: u8 = 2;
/// Units queued for a client before it is resynced with a keyframe.
const CLIENT_QUEUE: usize = 32;
/// Forced keyframes closer together than this are coalesced into the last one.
const KEYFRAME_SPACING: Duration = Duration::from_millis(150);
/// Keyframes in a row that `wire_unit` refuses before the session gives up on this transport:
/// about 2 s of forced keyframes at `KEYFRAME_SPACING`.
const UNUSABLE_KEYFRAMES: u32 = 13;

/// The decoder configuration a `hello_ack` names.
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct VideoConfig {
    pub(crate) config_id: u8,
    pub(crate) codec: &'static str,
    /// `avcC`, `hvcC` or `av1C` (empty for AV1 when the parser gave none), or PyroWave's
    /// `PWV1Header`.
    pub(crate) description: Vec<u8>,
    pub(crate) width: i32,
    pub(crate) height: i32,
    pub(crate) fps: i32,
    /// PyroWave only: the `color_space.full_range` its `hello_ack` must state.
    pub(crate) full_range: bool,
}

/// `session_assign.pyrowave` (agent-api.md, amendment 55): the grant to send this session's
/// WebTransport client PyroWave.
#[derive(Debug, Clone, Copy, PartialEq, serde::Deserialize)]
pub struct PyroWaveGrant {
    pub wanted: bool,
    pub target_kbps: u32,
    pub floor_kbps: u32,
    pub max_rtt_ms: u32,
}

/// The session's PyroWave state (codec-pyrowave.md §Switching codec).
struct Pyro {
    grant: PyroWaveGrant,
    /// The per-session wanted flag: cleared by a switch off that is not `webrtc`.
    wanted: bool,
    /// The encode branch is up and has not failed.
    usable: bool,
    /// A client was sent PyroWave this session: the next switch on is a `reconnect`.
    launched: bool,
    width: i32,
    height: i32,
    fps: i32,
}

#[derive(Debug, Clone)]
pub(crate) struct Unit {
    pub(crate) data: Bytes,
    pub(crate) keyframe: bool,
    pub(crate) config_id: u8,
    pub(crate) frame_seq: u16,
    pub(crate) ts_90khz: u32,
    pub(crate) capture_us: u64,
    /// A PyroWave frame: its payload is packed by `pyrowave::pack`, never cut into ranges.
    pub(crate) pyrowave: bool,
}

/// What the tap sends a connected client, in pad order: a configuration always precedes
/// the first unit encoded under it.
#[derive(Debug)]
pub(crate) enum Out {
    /// With the `codec_switch_reason` its `hello_ack` carries.
    Config(Arc<VideoConfig>, Option<&'static str>),
    Unit(Unit),
}

/// Why a client's queue ended.
#[derive(Debug, Clone, Copy, PartialEq)]
pub(crate) enum Gone {
    TakenOver,
    SessionEnded,
    /// The encoder never produced a keyframe this transport can carry.
    Unusable,
}

struct Client {
    id: u64,
    tx: mpsc::Sender<Out>,
    gone: Arc<OnceLock<Gone>>,
    /// Its queue was full at a configuration change: the configuration goes ahead of
    /// the next keyframe instead, with `resync_reason`, the switch's `codec_switch_reason`.
    resync: bool,
    resync_reason: Option<&'static str>,
    /// It is being sent PyroWave, and none of the standard codec's units.
    pyrowave: Option<Arc<VideoConfig>>,
}

#[derive(Default)]
struct State {
    /// The standard codec's configuration.
    config: Option<Arc<VideoConfig>>,
    /// Every configuration takes the next id, whichever codec it is for: one sequence, so a
    /// client never sees two configurations under one `config_id` across a codec switch.
    next_id: u8,
    pyro: Option<Pyro>,
    client: Option<Client>,
    next_client: u64,
    /// Skip delta units until a keyframe: a client joined, its queue overflowed, or the
    /// configuration changed.
    want_key: bool,
    frame_seq: u16,
    last_ts: u32,
    warned_format: bool,
    warned_keyframe: bool,
    /// Keyframes the parser flagged that `wire_unit` refused, since the last one sent.
    refused_keyframes: u32,
    session_id: String,
    /// Set by `end`: no client attaches after it.
    ended: bool,
    last_forced: Option<Instant>,
    /// A request was coalesced into the last: the next unit past the spacing sends it.
    pending_key: bool,
}

/// The Gamepad API exposes at most four slots; a `gp` for a higher `i` is dropped, so a client
/// cannot grow the per-pad table.
const MAX_PADS: u64 = 4;

/// The client whose input the devices take: none (`0`) or its id, and the buttons of its last
/// reliable `gp` per pad, since a datagram `gp` carries axes only.
#[derive(Default)]
struct Typing {
    client: u64,
    pads: std::collections::HashMap<u64, serde_json::Value>,
}

/// The tap's side of a session: fed by the GStreamer streaming thread, read by the
/// connection task of the one client attached.
pub(crate) struct Media {
    state: Mutex<State>,
    audio: Arc<Audio>,
    input: super::Input,
    /// Whose input reaches the devices. Held across the injection, so a client that stopped
    /// being the attached one cannot slip an event in behind the release that ended it.
    typing: Mutex<Typing>,
    encoder: glib::WeakRef<gst::Element>,
    /// Where `codec.switched` goes, and the session it names.
    trace: Option<(crate::session::pipeline::TraceTx, String)>,
}

/// The client half `Media::attach` hands back.
pub(crate) struct Attached {
    pub(crate) id: u64,
    pub(crate) config: Arc<VideoConfig>,
    /// The first `hello_ack`'s `codec_switch_reason`.
    pub(crate) reason: Option<&'static str>,
    pub(crate) rx: mpsc::Receiver<Out>,
    pub(crate) audio: mpsc::Receiver<audio::Packet>,
    pub(crate) gone: Arc<OnceLock<Gone>>,
}

impl State {
    fn take_id(&mut self) -> u8 {
        let id = self.next_id;
        self.next_id = id.wrapping_add(1);
        id
    }
}

impl Media {
    pub(crate) fn new(
        encoder: &gst::Element,
        input: super::Input,
        audio: Arc<Audio>,
        trace: Option<(crate::session::pipeline::TraceTx, String)>,
    ) -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::default(),
            audio,
            input,
            typing: Mutex::default(),
            encoder: encoder.downgrade(),
            trace,
        })
    }

    /// One `input.md` event from `client` to the devices; dropped unless it is the attached one.
    /// A datagram can arrive after a reliable event that followed it, so a datagram `gp` takes
    /// its buttons from the last reliable one and never changes them; one for a pad with none
    /// is dropped. Everything else passes through as sent.
    pub(crate) fn input_event(&self, client: u64, json: &str, datagram: bool) {
        let mut typing = self.typing.lock().unwrap_or_else(|e| e.into_inner());
        if typing.client != client {
            return;
        }
        if !json.contains("\"gp\"") {
            return (self.input.event)(json);
        }
        let Ok(mut event) = serde_json::from_str::<serde_json::Value>(json) else {
            return (self.input.event)(json);
        };
        if event.get("t").and_then(|t| t.as_str()) == Some("gp") {
            let pad = event.get("i").and_then(|i| i.as_u64()).unwrap_or(0);
            if pad >= MAX_PADS {
                return;
            }
            if datagram {
                let Some(buttons) = typing.pads.get(&pad) else {
                    return;
                };
                event["buttons"] = buttons.clone();
            } else {
                typing
                    .pads
                    .insert(pad, event.get("buttons").cloned().unwrap_or_default());
            }
        }
        (self.input.event)(&event.to_string());
    }

    #[cfg(test)]
    pub(crate) fn remembered_pads(&self) -> usize {
        self.typing.lock().unwrap().pads.len()
    }

    /// Lift everything the devices hold and forget the pads. Called with `typing` locked.
    fn release(&self, typing: &mut Typing) {
        typing.pads.clear();
        (self.input.release_all)();
    }

    /// The session may send PyroWave: `grant`, at `width`x`height`@`fps`, the size its
    /// encode branch runs at. `usable` false: the branch could not be built.
    pub(crate) fn grant_pyrowave(
        &self,
        grant: PyroWaveGrant,
        (width, height, fps): (i32, i32, i32),
        usable: bool,
    ) {
        self.lock().pyro = Some(Pyro {
            grant,
            wanted: grant.wanted,
            usable,
            launched: false,
            width,
            height,
            fps,
        });
    }

    /// `codec.switched` (agent-api.md, amendment 55).
    fn switched(
        &self,
        from: Option<&str>,
        to: &str,
        reason: &str,
        config_id: u8,
        kbps: Option<u32>,
    ) {
        tracing::info!(
            token = "webtransport-codec-switched",
            "WebTransport codec {} -> {to} (reason {reason}, config {config_id})",
            from.unwrap_or("none")
        );
        if let Some((tx, session_id)) = &self.trace {
            tx.try_emit(
                session_id.clone(),
                crate::session::runner::TraceEvent {
                    ts_unix_ms: SystemTime::now()
                        .duration_since(SystemTime::UNIX_EPOCH)
                        .map_or(0, |d| d.as_millis() as i64),
                    event: "codec.switched",
                    payload: serde_json::json!({
                        "from": from, "to": to, "reason": reason,
                        "config_id": config_id, "target_kbps": kbps,
                    }),
                },
            );
        }
    }

    /// A PyroWave client is attached: the encode branch has work.
    pub(crate) fn pyrowave_active(&self) -> bool {
        self.lock()
            .client
            .as_ref()
            .is_some_and(|c| c.pyrowave.is_some())
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    pub(crate) fn audio(&self) -> &Arc<Audio> {
        &self.audio
    }

    /// A configuration exists, so a `hello_ack` can be sent.
    pub(crate) fn ready(&self) -> bool {
        self.lock().config.is_some()
    }

    /// Make `self` stream to a new client, displacing the one before it. The new one
    /// starts at a keyframe, in PyroWave when the session wants it, the client `offer`ed it
    /// (`hello.pyrowave`) and the connection's round trip `rtt` passes the grant's gate
    /// (codec-pyrowave.md §Who gets PyroWave, §Link).
    pub(crate) fn attach(&self, offer: bool, rtt: Duration) -> Option<Attached> {
        let (tx, rx) = mpsc::channel(CLIENT_QUEUE);
        let gone = Arc::new(OnceLock::new());
        let mut st = self.lock();
        if st.ended {
            return None;
        }
        let standard = st.config.clone()?;
        let mut decided = None;
        if let Some(pyro) = st.pyro.as_mut().filter(|p| p.wanted) {
            let reason = if !offer {
                Err("client_no_decoder")
            } else if !pyro.usable {
                Err("encoder_error")
            } else if rtt > Duration::from_millis(u64::from(pyro.grant.max_rtt_ms)) {
                Err("rtt_too_high")
            } else if pyro.launched {
                Ok("reconnect")
            } else {
                Ok("launch")
            };
            // client_no_decoder is this connection's lack, not the session's: it keeps wanting.
            if matches!(reason, Err("rtt_too_high" | "encoder_error")) {
                pyro.wanted = false;
            }
            pyro.launched |= reason.is_ok();
            decided = Some(reason);
        }
        let (config, pyrowave, reason) = match decided {
            Some(Ok(reason)) => {
                let pyro = st.pyro.as_ref().expect("decided from it");
                let (w, h, fps) = (pyro.width, pyro.height, pyro.fps);
                let config = Arc::new(VideoConfig {
                    config_id: st.take_id(),
                    codec: "pyrowave",
                    description: super::pyrowave::pwv1_header(
                        w as u32, h as u32, fps as u32, false,
                    )
                    .to_vec(),
                    width: w,
                    height: h,
                    fps,
                    full_range: false,
                });
                (config.clone(), Some(config), Some(reason))
            }
            Some(Err(reason)) => (standard, None, Some(reason)),
            None => (standard, None, None),
        };
        st.next_client += 1;
        let id = st.next_client;
        let displaced = st.client.replace(Client {
            id,
            tx,
            gone: gone.clone(),
            resync: false,
            resync_reason: None,
            pyrowave,
        });
        if let Some(old) = displaced {
            let _ = old.gone.set(Gone::TakenOver);
        }
        st.want_key = true;
        st.refused_keyframes = 0;
        let kbps = st.pyro.as_ref().map(|p| p.grant.target_kbps);
        drop(st);
        let audio = self.audio.attach(id);
        if let Some(reason) = reason {
            let kbps = (config.codec == "pyrowave").then_some(kbps).flatten();
            self.switched(None, config.codec, reason, config.config_id, kbps);
        }
        {
            // The new client's input starts from nothing held: what its predecessor held is
            // lifted now, before the new client can send, and its late input is dropped.
            let mut typing = self.typing.lock().unwrap_or_else(|e| e.into_inner());
            // A predecessor that has not finished leaving (`detach` clears the client, then
            // takes this lock) still owns the devices: whatever it holds is lifted either way.
            if typing.client != 0 {
                self.release(&mut typing);
            }
            typing.client = id;
        }
        if config.codec != "pyrowave" {
            self.keyframe();
        }
        Some(Attached {
            id,
            config,
            reason,
            rx,
            audio,
            gone,
        })
    }

    pub(crate) fn attached(&self) -> bool {
        self.lock().client.is_some()
    }

    /// Move client `id` off PyroWave onto the standard codec (codec-pyrowave.md §Switching
    /// codec): `client_decode_error` from its `codec_fallback`, or `encoder_error`. Either
    /// clears the session's wanted flag. With no PyroWave client, an `encoder_error` only
    /// marks the branch unusable for the next `hello`.
    pub(crate) fn switch_off(&self, id: Option<u64>, reason: &'static str) {
        let mut st = self.lock();
        if let Some(pyro) = st.pyro.as_mut().filter(|_| reason == "encoder_error") {
            // The next hello reads `encoder_error` and clears the wanted flag then.
            pyro.usable = false;
        }
        let Some(standard) = st.config.clone() else {
            return;
        };
        if !st
            .client
            .as_ref()
            .is_some_and(|c| c.pyrowave.is_some() && id.is_none_or(|id| id == c.id))
        {
            return;
        }
        if let Some(pyro) = st.pyro.as_mut() {
            pyro.wanted = false;
        }
        let config = Arc::new(VideoConfig {
            config_id: st.take_id(),
            ..(*standard).clone()
        });
        st.config = Some(config.clone());
        st.want_key = true;
        let client = st.client.as_mut().expect("checked above");
        client.pyrowave = None;
        match client
            .tx
            .try_send(Out::Config(config.clone(), Some(reason)))
        {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(_)) => {
                client.resync = true;
                client.resync_reason = Some(reason);
            }
            Err(mpsc::error::TrySendError::Closed(_)) => st.client = None,
        }
        drop(st);
        self.switched(
            Some("pyrowave"),
            config.codec,
            reason,
            config.config_id,
            None,
        );
        self.keyframe();
    }

    /// The client is gone. If it is still the one the devices take input from, what it held is
    /// lifted; a displaced client's was lifted when its successor attached, and the successor's
    /// keys are not its to release.
    pub(crate) fn detach(&self, id: u64) {
        {
            let mut st = self.lock();
            if st.client.as_ref().is_some_and(|c| c.id == id) {
                st.client = None;
            }
        }
        #[cfg(test)]
        crate::webtransport::tests::detach_gap();
        let mut typing = self.typing.lock().unwrap_or_else(|e| e.into_inner());
        if typing.client == id {
            typing.client = 0;
            self.release(&mut typing);
        }
        self.audio.detach(id);
    }

    pub(crate) fn end(&self) {
        let mut st = self.lock();
        st.ended = true;
        if let Some(old) = st.client.take() {
            let _ = old.gone.set(Gone::SessionEnded);
        }
        drop(st);
        self.audio.end();
    }

    /// The keyframe request: the force-key-unit event PLI sends. A client drives this,
    /// so a request inside `KEYFRAME_SPACING` of the last waits for the next unit past
    /// it (`on_unit`), and it logs at debug.
    pub(crate) fn keyframe(&self) {
        {
            let mut st = self.lock();
            let now = Instant::now();
            if st
                .last_forced
                .is_some_and(|at| now.duration_since(at) < KEYFRAME_SPACING)
            {
                st.pending_key = true;
                return;
            }
            st.last_forced = Some(now);
            st.pending_key = false;
        }
        let Some(pad) = self.encoder.upgrade().and_then(|e| e.static_pad("src")) else {
            return;
        };
        let accepted = pad.send_event(crate::session::pipeline::force_key_unit_event());
        tracing::debug!(
            accepted,
            "WebTransport keyframe request sent to the encoder"
        );
    }

    pub(super) fn on_caps(&self, caps: &gst::CapsRef, session_id: &str) {
        let mut st = self.lock();
        if st.session_id.is_empty() {
            st.session_id = session_id.to_string();
        }
        let Some(mut config) = video_config(caps) else {
            if !st.warned_format {
                st.warned_format = true;
                tracing::warn!(
                    token = "webtransport-video-format-unsupported",
                    session_id = %session_id,
                    "WebTransport carries no video for these caps: {caps}"
                );
            }
            return;
        };
        if let Some(old) = &st.config {
            if (VideoConfig {
                config_id: old.config_id,
                ..config.clone()
            }) == **old
            {
                return;
            }
        }
        config.config_id = st.take_id();
        let config = Arc::new(config);
        st.config = Some(config.clone());
        // Never send a delta across a change. The encoder normally restarts on a keyframe,
        // and the rung step forces one; `on_unit` asks only if a delta comes first.
        st.want_key = true;
        // A PyroWave client hears of the standard codec only when it is switched back to it.
        let sent = st
            .client
            .as_ref()
            .filter(|c| c.pyrowave.is_none())
            .map(|c| c.tx.try_send(Out::Config(config, None)));
        match (sent, st.client.as_mut()) {
            (Some(Err(mpsc::error::TrySendError::Full(_))), Some(client)) => client.resync = true,
            (Some(Err(mpsc::error::TrySendError::Closed(_))), _) => st.client = None,
            _ => {}
        }
    }

    /// A keyframe the parser flagged that cannot go out. True once there have been
    /// `UNUSABLE_KEYFRAMES` in a row: the client is let go with `Gone::Unusable`.
    fn refuse_keyframe(&self, st: &mut State) -> bool {
        st.refused_keyframes += 1;
        let codec = st.config.as_ref().map_or("", |c| c.codec);
        if !st.warned_keyframe {
            st.warned_keyframe = true;
            tracing::warn!(
                token = "webtransport-keyframe-refused",
                session_id = %st.session_id,
                codec,
                "a keyframe the encoder flagged is not one WebTransport can carry (H.265: no IRAP slice; AV1: no sequence header, or a malformed OBU)"
            );
        }
        if st.refused_keyframes < UNUSABLE_KEYFRAMES {
            return false;
        }
        tracing::error!(
            token = "webtransport-keyframe-unusable",
            session_id = %st.session_id,
            codec,
            refused = st.refused_keyframes,
            "no usable keyframe from the encoder; ending the WebTransport session so the client falls back"
        );
        if let Some(client) = st.client.take() {
            let _ = client.gone.set(Gone::Unusable);
        }
        true
    }

    fn on_unit(&self, buffer: &gst::BufferRef) {
        let flagged = !buffer.flags().contains(gst::BufferFlags::DELTA_UNIT);
        let mut st = self.lock();
        // openh264's second unit carries no PTS: it keeps the one before.
        let ts_90khz = buffer.pts().map_or(st.last_ts, |pts| {
            (pts.nseconds() as u128 * 90_000 / 1_000_000_000) as u32
        });
        st.last_ts = ts_90khz;
        // With a client attached a keyframe is the unit as it would be sent: flagged and, for
        // H.265 and AV1, holding a random access point. With none, the parser's flag is all
        // there is.
        // A PyroWave client is sent no standard unit, so its standard keyframes are not judged.
        let attached = st
            .config
            .clone()
            .filter(|_| st.client.as_ref().is_some_and(|c| c.pyrowave.is_none()));
        let map = attached.as_ref().and_then(|_| buffer.map_readable().ok());
        let wire = attached
            .as_ref()
            .zip(map.as_ref())
            .and_then(|(c, m)| wire_unit(c.codec, m, flagged));
        let keyframe = if attached.is_some() {
            wire.as_ref().is_some_and(|(_, key)| *key)
        } else {
            flagged
        };
        if keyframe {
            st.pending_key = false;
            st.refused_keyframes = 0;
        } else if flagged && attached.is_some() && self.refuse_keyframe(&mut st) {
            return;
        }
        let due = st.pending_key
            && st
                .last_forced
                .is_none_or(|at| at.elapsed() >= KEYFRAME_SPACING);
        if due {
            drop(st);
            self.keyframe();
            st = self.lock();
        }
        let (Some(config), Some(client)) = (st.config.clone(), st.client.as_ref()) else {
            return;
        };
        if client.pyrowave.is_some() {
            return;
        }
        let Some((data, _)) = wire else {
            // The client never gets this unit: the next one it is sent must not need it.
            st.want_key = true;
            return;
        };
        if st.want_key && !keyframe {
            // Only an attached client waiting on a keyframe asks for one, so the WebRTC
            // stream gets no IDR of ours at a caps change nobody here needs.
            drop(st);
            self.keyframe();
            return;
        }
        if st.client.as_ref().is_some_and(|c| c.resync) {
            let sent = st
                .client
                .as_ref()
                .map(|c| c.tx.try_send(Out::Config(config.clone(), c.resync_reason)));
            match sent {
                Some(Ok(())) => {
                    if let Some(client) = st.client.as_mut() {
                        client.resync = false;
                        client.resync_reason = None;
                    }
                }
                Some(Err(mpsc::error::TrySendError::Full(_))) => {
                    st.want_key = true;
                    drop(st);
                    self.keyframe();
                    return;
                }
                _ => {
                    st.client = None;
                    return;
                }
            }
        }
        st.want_key = false;
        // Only a unit the client is sent takes a number: a skipped one would read as a
        // loss to NACK (§Loss recovery). A unit the full queue refuses gives it back.
        let frame_seq = st.frame_seq;
        st.frame_seq = frame_seq.wrapping_add(1);
        let unit = Unit {
            data: Bytes::copy_from_slice(&data),
            keyframe,
            config_id: config.config_id,
            frame_seq,
            ts_90khz,
            capture_us: now_us(),
            pyrowave: false,
        };
        let sent = st.client.as_ref().map(|c| c.tx.try_send(Out::Unit(unit)));
        match sent {
            Some(Err(mpsc::error::TrySendError::Full(_))) => {
                st.frame_seq = frame_seq;
                st.want_key = true;
                drop(st);
                self.keyframe();
            }
            Some(Err(mpsc::error::TrySendError::Closed(_))) => st.client = None,
            _ => {}
        }
    }
}

/// The encoder-output wall clock, Unix µs: `capture_us` (erratum quasar-protocol#29 item 13).
fn now_us() -> u64 {
    SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .map_or(0, |d| d.as_micros() as u64)
}

impl Media {
    /// One PyroWave frame off `pyrowaveenc`. A frame the client's queue has no room for is
    /// dropped: the next one repairs everything, so it needs no keyframe.
    pub(crate) fn on_pyrowave_frame(&self, buffer: &gst::BufferRef) {
        let mut st = self.lock();
        let Some(config) = st.client.as_ref().and_then(|c| c.pyrowave.clone()) else {
            return;
        };
        let frame_seq = st.frame_seq;
        st.frame_seq = frame_seq.wrapping_add(1);
        let ts_90khz = buffer.pts().map_or(st.last_ts, |pts| {
            (pts.nseconds() as u128 * 90_000 / 1_000_000_000) as u32
        });
        let Ok(map) = buffer.map_readable() else {
            return;
        };
        let unit = Unit {
            data: Bytes::copy_from_slice(&map),
            keyframe: true,
            config_id: config.config_id,
            frame_seq,
            ts_90khz,
            capture_us: now_us(),
            pyrowave: true,
        };
        let sent = st.client.as_ref().map(|c| c.tx.try_send(Out::Unit(unit)));
        if let Some(Err(mpsc::error::TrySendError::Closed(_))) = sent {
            st.client = None;
        }
    }
}

/// Read every access unit and configuration off `video` (the parser's src pad) into
/// `media`. A probe, so the graph is the WebRTC one, unchanged.
pub(crate) fn tap(video: &gst::Pad, media: &Arc<Media>, session_id: &str) {
    let (media, session_id) = (media.clone(), session_id.to_string());
    video.add_probe(
        gst::PadProbeType::BUFFER | gst::PadProbeType::EVENT_DOWNSTREAM,
        move |_, info| {
            match &info.data {
                Some(gst::PadProbeData::Buffer(buffer)) => media.on_unit(buffer),
                Some(gst::PadProbeData::Event(event)) => {
                    if let gst::EventView::Caps(caps) = event.view() {
                        media.on_caps(caps.caps(), &session_id);
                    }
                }
                _ => {}
            }
            gst::PadProbeReturn::Ok
        },
    );
}

/// The caps whose units are already the payload `EncodedVideoChunk` takes: H.264 `avc` and
/// H.265 `hvc1`/`hev1` with 4-byte lengths, and AV1 `obu-stream` temporal units.
fn video_config(caps: &gst::CapsRef) -> Option<VideoConfig> {
    let s = caps.structure(0)?;
    let codec_data = || {
        let buffer = s.get::<gst::Buffer>("codec_data").ok()?;
        let map = buffer.map_readable().ok()?;
        Some(map.to_vec())
    };
    let (codec, description) = match (s.name().as_str(), s.get::<&str>("stream-format").ok()?) {
        ("video/x-h264", "avc") => {
            let d = codec_data()?;
            // lengthSizeMinusOne in the avcC's fifth byte.
            (d.get(4).map(|b| b & 0x03) == Some(3)).then_some(("h264", d))?
        }
        ("video/x-h265", "hvc1" | "hev1") => {
            let d = codec_data()?;
            // lengthSizeMinusOne in the hvcC's 22nd byte.
            (d.get(21).map(|b| b & 0x03) == Some(3)).then_some(("h265", d))?
        }
        ("video/x-av1", "obu-stream") if s.get::<&str>("alignment").ok() == Some("tu") => {
            ("av1", codec_data().unwrap_or_default())
        }
        _ => return None,
    };
    let fps = s
        .get::<gst::Fraction>("framerate")
        .ok()
        .filter(|f| f.denom() > 0)
        .map_or(0, |f| (f.numer() + f.denom() / 2) / f.denom());
    Some(VideoConfig {
        config_id: 0,
        codec,
        description,
        width: s.get("width").ok()?,
        height: s.get("height").ok()?,
        fps,
        full_range: false,
    })
}

/// The `0x01` payload for one parser buffer and whether it is a keyframe. A unit is a
/// keyframe only if the parser said so and the bitstream agrees: a decoder started on
/// anything else fails ("a key frame is required"). H.264 trusts the parser. `None`: not a
/// unit the contract can carry.
fn wire_unit<'a>(codec: &str, data: &'a [u8], flagged: bool) -> Option<(Cow<'a, [u8]>, bool)> {
    match codec {
        // IRAP NAL types, BLA_W_LP through RSV_IRAP_VCL23.
        "h265" => Some((
            data.into(),
            flagged && hevc_nal_types(data).any(|t| (16..=23).contains(&t)),
        )),
        "av1" => {
            let (payload, sequence_header) = av1_unit(data)?;
            Some((payload.into(), flagged && sequence_header))
        }
        _ => Some((data.into(), flagged)),
    }
}

/// The type of each NAL in a 4-byte length-prefixed H.265 access unit, up to the first that
/// does not fit.
fn hevc_nal_types(data: &[u8]) -> impl Iterator<Item = u8> + '_ {
    let mut rest = data;
    std::iter::from_fn(move || {
        let len = u32::from_be_bytes(rest.get(..4)?.try_into().ok()?) as usize;
        let nal = rest
            .get(4..4usize.checked_add(len)?)
            .filter(|n| !n.is_empty())?;
        rest = &rest[4 + len..];
        Some((nal[0] >> 1) & 0x3f)
    })
}

/// A temporal unit without its temporal delimiters (the contract carries none), and whether
/// it holds a sequence header. `None` for an OBU without a size field or one past the end.
fn av1_unit(data: &[u8]) -> Option<(Vec<u8>, bool)> {
    let (mut out, mut sequence_header, mut at) = (Vec::with_capacity(data.len()), false, 0);
    while at < data.len() {
        let start = at;
        let header = data[at];
        if header & 0x02 == 0 {
            return None;
        }
        at += 1 + usize::from(header & 0x04 != 0);
        let mut size = 0usize;
        for i in 0..8 {
            let byte = *data.get(at)?;
            at += 1;
            size |= usize::from(byte & 0x7f) << (7 * i);
            if byte & 0x80 == 0 {
                break;
            }
            if i == 7 {
                return None;
            }
        }
        at = at.checked_add(size).filter(|&end| end <= data.len())?;
        match (header >> 3) & 0x0f {
            2 => {}
            kind => {
                sequence_header |= kind == 1;
                out.extend_from_slice(&data[start..at]);
            }
        }
    }
    (!out.is_empty()).then_some((out, sequence_header))
}

/// Split `unit` into `0x01` datagrams of at most `max` bytes, numbered from `*pkt_seq`.
/// A PyroWave frame is packed by `pyrowave::pack` for `config`'s size; `None` when it
/// cannot be sent (over 65 535 datagrams, or a frame the packing refuses).
pub(crate) fn packetize(
    unit: &Unit,
    config: &VideoConfig,
    max: usize,
    pkt_seq: &mut u32,
) -> Option<Vec<Bytes>> {
    let room = max.saturating_sub(HEADER).max(1);
    if unit.pyrowave {
        let n = super::pyrowave::block_count(config.width as u32, config.height as u32);
        let payloads = super::pyrowave::pack(&unit.data, n, room).ok()?;
        let frags: Vec<&[u8]> = payloads.iter().map(Vec::as_slice).collect();
        return datagrams(unit, FLAG_KEYFRAME | FLAG_DISCARDABLE, &frags, pkt_seq);
    }
    let frags: Vec<&[u8]> = if unit.data.is_empty() {
        vec![&[]]
    } else {
        unit.data.chunks(room).collect()
    };
    let flags = if unit.keyframe { FLAG_KEYFRAME } else { 0 };
    datagrams(unit, flags, &frags, pkt_seq)
}

fn datagrams(unit: &Unit, flags: u8, frags: &[&[u8]], pkt_seq: &mut u32) -> Option<Vec<Bytes>> {
    let count = u16::try_from(frags.len()).ok()?;
    Some(
        frags
            .iter()
            .enumerate()
            .map(|(index, frag)| {
                let mut d = BytesMut::with_capacity(HEADER + frag.len());
                d.put_u8(CHANNEL_VIDEO);
                d.put_u32_le(*pkt_seq);
                d.put_u8(flags);
                d.put_u8(unit.config_id);
                d.put_u16_le(unit.frame_seq);
                d.put_u16_le(index as u16);
                d.put_u16_le(count);
                d.put_u32_le(unit.ts_90khz);
                d.put_u64_le(unit.capture_us);
                d.put_slice(frag);
                *pkt_seq = pkt_seq.wrapping_add(1);
                d.freeze()
            })
            .collect(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn unit(len: usize) -> Unit {
        Unit {
            data: Bytes::from((0..len).map(|i| i as u8).collect::<Vec<_>>()),
            keyframe: true,
            config_id: 7,
            frame_seq: 0xfffe,
            ts_90khz: 0x0102_0304,
            capture_us: 0x1122_3344_5566_7788,
            pyrowave: false,
        }
    }

    fn cfg_for(u: &Unit) -> VideoConfig {
        VideoConfig {
            config_id: u.config_id,
            codec: "h264",
            description: Vec::new(),
            width: 320,
            height: 192,
            fps: 60,
            full_range: false,
        }
    }

    #[test]
    fn a_unit_splits_into_25_byte_headers_and_in_order_byte_ranges() {
        let u = unit(3000);
        let mut pkt_seq = u32::MAX;
        let dgrams = packetize(&u, &cfg_for(&u), MAX_DATAGRAM, &mut pkt_seq).unwrap();
        assert_eq!(dgrams.len(), 3, "1123 + 1123 + 754");
        assert_eq!(pkt_seq, 2, "one pkt_seq per datagram, wrapping");
        let mut joined = Vec::new();
        for (i, d) in dgrams.iter().enumerate() {
            assert!(d.len() <= MAX_DATAGRAM);
            assert_eq!(d[0], 0x01);
            assert_eq!(
                u32::from_le_bytes(d[1..5].try_into().unwrap()),
                u32::MAX.wrapping_add(i as u32)
            );
            assert_eq!(d[5], FLAG_KEYFRAME);
            assert_eq!(d[6], 7);
            assert_eq!(&d[7..9], &0xfffe_u16.to_le_bytes());
            assert_eq!(u16::from_le_bytes([d[9], d[10]]), i as u16);
            assert_eq!(u16::from_le_bytes([d[11], d[12]]), 3);
            assert_eq!(&d[13..17], &0x0102_0304_u32.to_le_bytes());
            assert_eq!(&d[17..25], &0x1122_3344_5566_7788_u64.to_le_bytes());
            joined.extend_from_slice(&d[HEADER..]);
        }
        assert_eq!(dgrams[0].len(), MAX_DATAGRAM);
        assert_eq!(joined, u.data);
    }

    #[test]
    fn a_unit_that_fits_is_one_fragment_and_a_delta_has_no_keyframe_flag() {
        let mut u = unit(MAX_DATAGRAM - HEADER);
        u.keyframe = false;
        let mut pkt_seq = 0;
        let dgrams = packetize(&u, &cfg_for(&u), 1400, &mut pkt_seq).unwrap();
        assert_eq!(dgrams.len(), 1);
        assert_eq!(dgrams[0][5], 0);
        assert_eq!(u16::from_le_bytes([dgrams[0][11], dgrams[0][12]]), 1);
    }

    #[test]
    fn caps_give_the_avcc_and_a_new_configuration_takes_the_next_id() {
        gst::init().unwrap();
        let caps = |w: i32, cd: &[u8]| {
            gst::Caps::builder("video/x-h264")
                .field("stream-format", "avc")
                .field("alignment", "au")
                .field("width", w)
                .field("height", 720)
                .field("framerate", gst::Fraction::new(60, 1))
                .field("codec_data", gst::Buffer::from_slice(cd.to_vec()))
                .build()
        };
        let avcc = [1u8, 0x42, 0xc0, 0x1f, 0xff, 0xe1, 0, 0];
        let media = Arc::new(Media {
            state: Mutex::default(),
            audio: Audio::new(audio::Plan::default()),
            input: crate::webtransport::tests::no_input(),
            typing: Mutex::default(),
            encoder: glib::WeakRef::new(),
            trace: None,
        });
        media.on_caps(&caps(1280, &avcc), "s");
        let mut client = media
            .attach(false, Duration::ZERO)
            .expect("ready after caps");
        assert_eq!(client.config.config_id, 0);
        assert_eq!(client.config.description, avcc);
        assert_eq!((client.config.width, client.config.fps), (1280, 60));

        media.on_caps(&caps(1280, &avcc), "s");
        assert!(
            client.rx.try_recv().is_err(),
            "the same caps are no new config"
        );
        media.on_caps(&caps(960, &avcc), "s");
        let Ok(Out::Config(next, None)) = client.rx.try_recv() else {
            panic!("a resize is a new configuration");
        };
        assert_eq!((next.config_id, next.width), (1, 960));

        let byte_stream = gst::Caps::builder("video/x-h264")
            .field("stream-format", "byte-stream")
            .build();
        assert!(video_config(&byte_stream).is_none());
    }

    fn caps(width: i32) -> gst::Caps {
        gst::Caps::builder("video/x-h264")
            .field("stream-format", "avc")
            .field("width", width)
            .field("height", 720)
            .field("framerate", gst::Fraction::new(60, 1))
            .field(
                "codec_data",
                gst::Buffer::from_slice(vec![1u8, 0x42, 0xc0, 0x1f, 0xff, 0xe1, 0, 0]),
            )
            .build()
    }

    fn buffer(keyframe: bool) -> gst::Buffer {
        let mut b = gst::Buffer::from_slice(vec![0u8, 0, 0, 1, if keyframe { 0x65 } else { 0x41 }]);
        if !keyframe {
            b.get_mut().unwrap().set_flags(gst::BufferFlags::DELTA_UNIT);
        }
        b
    }

    /// Media over a stand-in encoder whose src pad counts the keyframe requests it gets.
    fn counted() -> (
        Arc<Media>,
        gst::Element,
        Arc<std::sync::atomic::AtomicUsize>,
    ) {
        gst::init().unwrap();
        let encoder = gst::ElementFactory::make("identity").build().unwrap();
        let forced = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let seen = forced.clone();
        encoder.static_pad("src").unwrap().add_probe(
            gst::PadProbeType::EVENT_UPSTREAM,
            move |_, _| {
                seen.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                gst::PadProbeReturn::Ok
            },
        );
        // An element in NULL has inactive pads, which refuse every event.
        encoder.set_state(gst::State::Paused).unwrap();
        (
            Media::new(
                &encoder,
                crate::webtransport::tests::no_input(),
                Audio::new(audio::Plan::default()),
                None,
            ),
            encoder,
            forced,
        )
    }

    fn units(rx: &mut mpsc::Receiver<Out>) -> Vec<(bool, u8)> {
        let mut out = Vec::new();
        while let Ok(o) = rx.try_recv() {
            out.push(match o {
                Out::Unit(u) => (u.keyframe, u.config_id),
                Out::Config(c, _) => (false, 100 + c.config_id),
            });
        }
        out
    }

    #[test]
    fn a_new_configuration_forces_a_keyframe_and_no_delta_crosses_it() {
        let (media, _encoder, forced) = counted();
        media.on_caps(&caps(1280), "s");
        let mut client = media.attach(false, Duration::ZERO).unwrap();
        media.on_unit(&buffer(true));
        media.on_unit(&buffer(false));
        assert_eq!(units(&mut client.rx), [(true, 0), (false, 0)]);

        std::thread::sleep(KEYFRAME_SPACING);
        let before = forced.load(std::sync::atomic::Ordering::SeqCst);
        media.on_caps(&caps(960), "s");
        media.on_unit(&buffer(false));
        assert_eq!(
            forced.load(std::sync::atomic::Ordering::SeqCst),
            before + 1,
            "a delta under the new configuration asks for the keyframe"
        );
        media.on_unit(&buffer(true));
        media.on_unit(&buffer(false));
        assert_eq!(
            units(&mut client.rx),
            [(false, 101), (true, 1), (false, 1)],
            "the hello_ack, then the keyframe; the delta in between is skipped"
        );
    }

    #[test]
    fn a_full_queue_at_a_configuration_change_resyncs_instead_of_dropping_the_client() {
        let (media, _encoder, _) = counted();
        media.on_caps(&caps(1280), "s");
        let mut client = media.attach(false, Duration::ZERO).unwrap();
        media.on_unit(&buffer(true));
        for _ in 1..CLIENT_QUEUE {
            media.on_unit(&buffer(false));
        }
        media.on_caps(&caps(960), "s");
        assert_eq!(
            units(&mut client.rx).len(),
            CLIENT_QUEUE,
            "the configuration did not fit"
        );
        media.on_unit(&buffer(false));
        media.on_unit(&buffer(true));
        assert_eq!(
            units(&mut client.rx),
            [(false, 101), (true, 1)],
            "the configuration goes ahead of the next keyframe"
        );
        assert!(client.gone.get().is_none(), "the client is still attached");
    }

    fn seqs(rx: &mut mpsc::Receiver<Out>) -> Vec<u16> {
        let mut out = Vec::new();
        while let Ok(o) = rx.try_recv() {
            if let Out::Unit(u) = o {
                out.push(u.frame_seq);
            }
        }
        out
    }

    #[test]
    fn units_skipped_for_a_keyframe_or_refused_by_a_full_queue_take_no_frame_seq() {
        let (media, _encoder, _) = counted();
        media.on_caps(&caps(1280), "s");
        let mut client = media.attach(false, Duration::ZERO).unwrap();
        media.on_unit(&buffer(false)); // before the first keyframe: skipped
        media.on_unit(&buffer(true));
        media.on_unit(&buffer(false));
        media.on_caps(&caps(960), "s");
        media.on_unit(&buffer(false)); // under the new configuration, before its keyframe
        media.on_unit(&buffer(true));
        assert_eq!(
            seqs(&mut client.rx),
            [0, 1, 2],
            "no gap for a unit never sent"
        );

        for _ in 0..=CLIENT_QUEUE {
            media.on_unit(&buffer(false)); // the last one finds the queue full
        }
        media.on_unit(&buffer(true));
        let got = seqs(&mut client.rx);
        assert_eq!(got.len(), CLIENT_QUEUE, "the queue held its fill");
        assert_eq!(got.last(), Some(&(2 + CLIENT_QUEUE as u16)));
        media.on_unit(&buffer(true));
        assert_eq!(
            seqs(&mut client.rx),
            [3 + CLIENT_QUEUE as u16],
            "the refused unit gave its number back"
        );
    }

    #[test]
    fn keyframe_requests_closer_than_the_spacing_are_one() {
        let (media, _encoder, forced) = counted();
        for _ in 0..10 {
            media.keyframe();
        }
        assert_eq!(forced.load(std::sync::atomic::Ordering::SeqCst), 1);
        std::thread::sleep(KEYFRAME_SPACING);
        media.keyframe();
        assert_eq!(forced.load(std::sync::atomic::Ordering::SeqCst), 2);
    }

    #[test]
    fn no_client_attaches_after_the_session_ended() {
        let (media, _encoder, _) = counted();
        media.on_caps(&caps(1280), "s");
        let first = media.attach(false, Duration::ZERO).unwrap();
        media.end();
        assert_eq!(first.gone.get(), Some(&Gone::SessionEnded));
        assert!(
            media.attach(false, Duration::ZERO).is_none(),
            "a hello racing the end gets nothing"
        );
    }

    #[test]
    fn a_keyframe_request_coalesced_into_a_pli_is_sent_once_the_spacing_passes() {
        let (media, _encoder, forced) = counted();
        media.on_caps(&caps(1280), "s");
        let _client = media.attach(false, Duration::ZERO).unwrap();
        let count = || forced.load(std::sync::atomic::Ordering::SeqCst);
        std::thread::sleep(KEYFRAME_SPACING);
        media.keyframe(); // the PLI
        let after_pli = count();
        std::thread::sleep(Duration::from_millis(50));
        let changed = Instant::now();
        media.on_caps(&caps(960), "s");
        assert_eq!(count(), after_pli, "coalesced into the PLI's keyframe");
        while count() == after_pli {
            assert!(
                changed.elapsed() < Duration::from_millis(400),
                "the coalesced request was never sent: the stream would stall on want_key"
            );
            std::thread::sleep(Duration::from_millis(16));
            media.on_unit(&buffer(false));
        }
        assert!(changed.elapsed() < KEYFRAME_SPACING + Duration::from_millis(60));
    }

    #[test]
    fn a_caps_change_with_no_client_forces_no_keyframe_and_a_keyframe_clears_a_pending_one() {
        let (media, _encoder, forced) = counted();
        let count = || forced.load(std::sync::atomic::Ordering::SeqCst);
        media.on_caps(&caps(1280), "s");
        media.on_caps(&caps(960), "s");
        media.on_unit(&buffer(false));
        media.on_unit(&buffer(false));
        assert_eq!(count(), 0, "nothing of ours reaches the WebRTC stream");

        let _client = media.attach(false, Duration::ZERO).unwrap();
        assert_eq!(count(), 1, "the attach asks once");
        media.keyframe();
        assert!(media.lock().pending_key, "coalesced");
        media.on_unit(&buffer(true));
        assert!(
            !media.lock().pending_key,
            "the keyframe that went by satisfies it"
        );
        std::thread::sleep(KEYFRAME_SPACING);
        media.on_unit(&buffer(false));
        assert_eq!(count(), 1, "no stale request after the keyframe");
    }
    use crate::webtransport::tests::streams;

    #[test]
    fn h265_and_av1_caps_give_their_description_and_other_formats_are_refused() {
        gst::init().unwrap();
        let hevc = video_config(&streams::hevc_caps(640)).expect("hvc1 with an hvcC");
        assert_eq!(
            (hevc.codec, hevc.description.len(), hevc.fps),
            ("h265", 23, 30)
        );
        let av1 = video_config(&streams::av1_caps(640)).expect("obu-stream temporal units");
        assert_eq!(
            (av1.codec, av1.description.as_slice()),
            ("av1", &[0x81, 0x08, 0x0c, 0][..])
        );

        let with = |name: &str, field: &str, value: &str| {
            let mut c = if name == "video/x-h265" {
                streams::hevc_caps(640)
            } else {
                streams::av1_caps(640)
            };
            c.make_mut().structure_mut(0).unwrap().set(field, value);
            c
        };
        assert!(video_config(&with("video/x-h265", "stream-format", "byte-stream")).is_none());
        assert!(video_config(&with("video/x-av1", "stream-format", "annexb")).is_none());
        assert!(video_config(&with("video/x-av1", "alignment", "obu")).is_none());
        let mut short = streams::hevc_caps(640);
        short.make_mut().structure_mut(0).unwrap().set(
            "codec_data",
            gst::Buffer::from_slice(vec![
                1u8, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 93, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01,
            ]),
        );
        assert!(
            video_config(&short).is_none(),
            "2-byte NAL lengths cannot be sent as is"
        );
        let mut no_description = streams::av1_caps(640);
        no_description
            .make_mut()
            .structure_mut(0)
            .unwrap()
            .remove_field("codec_data");
        assert!(
            video_config(&no_description).is_some_and(|c| c.description.is_empty()),
            "av1C is optional for AV1"
        );
    }

    #[test]
    fn an_h265_unit_is_a_keyframe_only_with_an_irap_slice_the_parser_flagged() {
        for (irap, name) in [
            (16, "BLA_W_LP"),
            (19, "IDR_W_RADL"),
            (20, "IDR_N_LP"),
            (21, "CRA"),
            (23, "RSV_IRAP_VCL23"),
        ] {
            let au = streams::hevc_au(Some(irap), 50);
            let (data, key) = wire_unit("h265", &au, true).unwrap();
            assert!(key, "{name} starts a decoder");
            assert_eq!(&*data, &au[..], "the payload goes out as it is");
            assert!(
                !wire_unit("h265", &au, false).unwrap().1,
                "{name} the parser called a delta"
            );
        }
        let trail = streams::hevc_au(None, 50);
        assert!(
            !wire_unit("h265", &trail, true).unwrap().1,
            "TRAIL_R flagged as a keyframe is a delta"
        );
        // Parameter sets alone are not a random access point.
        let mut ps = streams::hevc_au(Some(19), 50);
        ps.truncate(ps.len() - 52 - 4);
        assert!(
            !wire_unit("h265", &ps, true).unwrap().1,
            "VPS, SPS and PPS without a slice"
        );
        // A length past the end ends the scan instead of reading on.
        let mut cut = streams::hevc_au(Some(19), 50);
        cut.truncate(cut.len() - 1);
        assert!(!wire_unit("h265", &cut, true).unwrap().1);
    }

    #[test]
    fn an_av1_unit_loses_its_temporal_delimiter_and_is_a_keyframe_with_a_sequence_header() {
        let key = streams::av1_tu(true, 50);
        let (data, is_key) = wire_unit("av1", &key, true).unwrap();
        assert!(is_key);
        assert_eq!(streams::obu_types(&data), [1, 6], "the delimiter is gone");
        assert!(
            !wire_unit("av1", &key, false).unwrap().1,
            "the parser called it a delta"
        );

        let delta = streams::av1_tu(false, 50);
        let (data, is_key) = wire_unit("av1", &delta, true).unwrap();
        assert!(
            !is_key,
            "a frame without a sequence header cannot start a decoder"
        );
        assert_eq!(streams::obu_types(&data), [6]);

        assert!(
            wire_unit("av1", &[0x10, 0x00], true).is_none(),
            "no size field"
        );
        assert!(
            wire_unit("av1", &[0x32, 0x09, 0x55], true).is_none(),
            "an OBU past the end"
        );
        assert!(
            wire_unit("av1", &[0x12, 0x00], true).is_none(),
            "a delimiter alone is no unit"
        );
        let extended = [0x12u8, 0x00, 0x0e, 0x00, 0x01, 0x55];
        assert_eq!(
            av1_unit(&extended),
            Some((vec![0x0e, 0x00, 0x01, 0x55], true)),
            "an extension byte moves the size field"
        );
    }

    #[test]
    fn h265_units_reach_a_client_under_the_configuration_with_their_flags() {
        gst::init().unwrap();
        let media = Arc::new(Media {
            state: Mutex::default(),
            audio: Audio::new(audio::Plan::default()),
            input: crate::webtransport::tests::no_input(),
            typing: Mutex::default(),
            encoder: glib::WeakRef::new(),
            trace: None,
        });
        media.on_caps(&streams::hevc_caps(640), "s");
        let mut client = media.attach(false, Duration::ZERO).unwrap();
        let unit = |au: Vec<u8>, key: bool| {
            let mut b = gst::Buffer::from_slice(au);
            if !key {
                b.get_mut().unwrap().set_flags(gst::BufferFlags::DELTA_UNIT);
            }
            b
        };
        media.on_unit(&unit(streams::hevc_au(None, 20), false));
        media.on_unit(&unit(streams::hevc_au(Some(19), 20), true));
        media.on_unit(&unit(streams::hevc_au(None, 20), false));
        assert_eq!(
            units(&mut client.rx),
            [(true, 0), (false, 0)],
            "the delta before the first keyframe is skipped"
        );
    }
    /// Starts `description` (an appsrc named `src` into a parser named `parse`, behind the
    /// payloader the session's pipeline has) with a tap on the parser's src pad, and pushes
    /// `first` until the parser has put out caps. `None` where the image lacks an element.
    fn through_real_parser(
        description: &str,
        first: &[u8],
    ) -> Option<(Attached, gstreamer_app::AppSrc, gst::Pipeline)> {
        gst::init().unwrap();
        let pipeline = gst::parse::launch(description)
            .map_err(|e| eprintln!("skipping: {e}"))
            .ok()?
            .downcast::<gst::Pipeline>()
            .unwrap();
        let parse = pipeline.by_name("parse").unwrap();
        let media = Media::new(
            &parse,
            crate::webtransport::tests::no_input(),
            Audio::new(audio::Plan::default()),
            None,
        );
        tap(&parse.static_pad("src").unwrap(), &media, "s");
        pipeline.set_state(gst::State::Playing).unwrap();
        let src = pipeline
            .by_name("src")
            .unwrap()
            .downcast::<gstreamer_app::AppSrc>()
            .unwrap();
        push(&src, first, 0);
        let deadline = Instant::now() + Duration::from_secs(10);
        while !media.ready() {
            assert!(Instant::now() < deadline, "no caps came out of the parser");
            std::thread::sleep(Duration::from_millis(20));
        }
        Some((media.attach(false, Duration::ZERO).unwrap(), src, pipeline))
    }

    /// No flags: the parser decides which units are deltas.
    fn push(src: &gstreamer_app::AppSrc, au: &[u8], n: u64) {
        let mut b = gst::Buffer::from_slice(au.to_vec());
        b.get_mut()
            .unwrap()
            .set_pts(gst::ClockTime::from_mseconds(33 * n));
        src.push_buffer(b).unwrap();
    }

    #[test]
    fn a_real_h265parse_hands_the_tap_hvc1_with_four_byte_lengths_and_irap_keyframes() {
        let key = streams::hevc_byte_stream(true, 0);
        let Some((mut client, src, pipeline)) = through_real_parser(
            "appsrc name=src format=time caps=video/x-h265,stream-format=byte-stream,alignment=au \
             ! h265parse name=parse ! rtph265pay pt=96 ! fakesink sync=false",
            &key,
        ) else {
            return;
        };
        // The client attached after the first unit: it waits for the next keyframe.
        push(&src, &streams::hevc_byte_stream(false, 1), 1);
        push(&src, &key, 2);
        push(&src, &streams::hevc_byte_stream(false, 3), 3);
        let config = client.config.clone();
        assert_eq!(
            (config.codec, config.width, config.height),
            ("h265", 64, 64)
        );
        assert_eq!(config.description[0], 1, "an hvcC record");
        assert_eq!(config.description[21] & 3, 3, "4-byte NAL lengths");
        assert_eq!(config.description[12], 93, "the SPS level reaches the hvcC");

        let mut seen = Vec::new();
        let deadline = Instant::now() + Duration::from_secs(10);
        while seen.len() < 2 && Instant::now() < deadline {
            match client.rx.try_recv() {
                Ok(Out::Unit(u)) => seen.push(u),
                _ => std::thread::sleep(Duration::from_millis(10)),
            }
        }
        pipeline.set_state(gst::State::Null).unwrap();
        assert_eq!(
            seen.len(),
            2,
            "the keyframe and the trail unit that followed it"
        );
        assert!(seen[0].keyframe && !seen[1].keyframe);
        let types = streams::hevc_types(&seen[0].data);
        assert!(types.contains(&19), "the IDR slice: {types:?}");
        assert!(
            streams::hevc_types(&seen[1].data).contains(&1),
            "the trail slice"
        );
    }
    #[test]
    fn a_real_av1parse_hands_the_tap_obu_temporal_units_and_sequence_header_keyframes() {
        let key = streams::av1_key_tu();
        let Some((mut client, src, pipeline)) = through_real_parser(
            "appsrc name=src format=time caps=video/x-av1,stream-format=obu-stream,alignment=tu,parsed=true \
             ! av1parse name=parse ! video/x-av1,parsed=true,stream-format=obu-stream,alignment=tu \
             ! fakesink sync=false",
            &key,
        ) else {
            return;
        };
        let config = client.config.clone();
        assert_eq!((config.codec, config.width, config.height), ("av1", 64, 64));
        assert!(
            config.description.is_empty() || config.description[0] == 0x81,
            "av1parse gives no av1C or a real one: {:02x?}",
            config.description
        );
        push(&src, &key, 1);
        let mut unit = None;
        let deadline = Instant::now() + Duration::from_secs(10);
        while unit.is_none() && Instant::now() < deadline {
            match client.rx.try_recv() {
                Ok(Out::Unit(u)) => unit = Some(u),
                _ => std::thread::sleep(Duration::from_millis(10)),
            }
        }
        pipeline.set_state(gst::State::Null).unwrap();
        let unit = unit.expect("a unit after the client attached");
        assert!(unit.keyframe);
        let types = streams::obu_types(&unit.data);
        assert!(types.contains(&1) && !types.contains(&2), "{types:?}");
    }
    fn av1_unit_buffer(sequence_header: bool, flagged_key: bool) -> gst::Buffer {
        let mut b = gst::Buffer::from_slice(streams::av1_tu(sequence_header, 20));
        if !flagged_key {
            b.get_mut().unwrap().set_flags(gst::BufferFlags::DELTA_UNIT);
        }
        b
    }

    fn bare_media() -> Arc<Media> {
        gst::init().unwrap();
        Arc::new(Media {
            state: Mutex::default(),
            audio: Audio::new(audio::Plan::default()),
            input: crate::webtransport::tests::no_input(),
            typing: Mutex::default(),
            encoder: glib::WeakRef::new(),
            trace: None,
        })
    }

    #[test]
    fn keyframes_the_transport_cannot_carry_end_the_client_after_about_two_seconds() {
        let media = bare_media();
        media.on_caps(&streams::av1_caps(640), "s");
        let mut client = media.attach(false, Duration::ZERO).unwrap();
        // Flagged as keyframes by the parser, with no sequence header in them.
        let refused = |n| {
            for _ in 0..n {
                media.on_unit(&av1_unit_buffer(false, true));
            }
        };
        refused(UNUSABLE_KEYFRAMES - 1);
        assert!(
            media.attached() && client.gone.get().is_none(),
            "one short of the limit"
        );

        media.on_unit(&av1_unit_buffer(true, true));
        assert_eq!(
            units(&mut client.rx),
            [(true, 0)],
            "a usable keyframe goes out"
        );
        refused(UNUSABLE_KEYFRAMES - 1);
        assert!(media.attached(), "a keyframe that went out reset the count");

        refused(1);
        assert_eq!(client.gone.get(), Some(&Gone::Unusable));
        assert!(!media.attached(), "the client is let go, not left black");
        // What was queued before (the refused ones went out as deltas) drains, then the queue ends.
        while let Ok(out) = client.rx.try_recv() {
            assert!(matches!(out, Out::Unit(ref u) if !u.keyframe));
        }
        assert!(matches!(
            client.rx.try_recv(),
            Err(mpsc::error::TryRecvError::Disconnected)
        ));
    }

    #[test]
    fn a_coalesced_keyframe_request_stays_until_a_keyframe_actually_goes_out() {
        let (media, _encoder, _forced) = counted();
        media.on_caps(&streams::av1_caps(640), "s");
        let _client = media.attach(false, Duration::ZERO).unwrap();
        // Past the attach's wait for a keyframe, so a refused unit asks for nothing itself.
        media.on_unit(&av1_unit_buffer(true, true));
        assert!(!media.lock().want_key);
        media.keyframe();
        assert!(media.lock().pending_key, "coalesced into the attach's");
        media.on_unit(&av1_unit_buffer(false, true));
        assert!(
            media.lock().pending_key,
            "a unit refused as a keyframe satisfies nothing"
        );
        media.on_unit(&av1_unit_buffer(true, true));
        assert!(!media.lock().pending_key);
    }

    const GRANT: PyroWaveGrant = PyroWaveGrant {
        wanted: true,
        target_kbps: 200_000,
        floor_kbps: 100_000,
        max_rtt_ms: 5,
    };
    const LAN: Duration = Duration::from_millis(1);

    /// Counted media with a standard configuration and a PyroWave grant at 320x192@60.
    fn granted() -> (
        Arc<Media>,
        gst::Element,
        Arc<std::sync::atomic::AtomicUsize>,
    ) {
        let (media, encoder, forced) = counted();
        media.on_caps(&caps(1280), "s");
        media.grant_pyrowave(GRANT, (320, 192, 60), true);
        (media, encoder, forced)
    }

    fn pyro_frame() -> gst::Buffer {
        gst::Buffer::from_slice(super::super::pyrowave::tests::frame(320, 192, &[(0, 4)]))
    }

    fn sent(rx: &mut mpsc::Receiver<Out>) -> Vec<String> {
        let mut out = Vec::new();
        while let Ok(o) = rx.try_recv() {
            out.push(match o {
                Out::Unit(u) => format!(
                    "unit {} {}{}",
                    u.config_id,
                    if u.pyrowave { "pyrowave" } else { "std" },
                    if u.keyframe { " key" } else { "" }
                ),
                Out::Config(c, r) => {
                    format!("config {} {} {}", c.config_id, c.codec, r.unwrap_or("-"))
                }
            });
        }
        out
    }

    #[test]
    fn a_wanted_session_starts_its_offering_client_in_pyrowave() {
        let (media, _encoder, forced) = granted();
        let mut client = media.attach(true, LAN).unwrap();
        assert_eq!(client.config.codec, "pyrowave");
        assert_eq!(
            client.config.config_id, 1,
            "the standard configuration took 0"
        );
        assert_eq!(client.reason, Some("launch"));
        assert_eq!(
            client.config.description,
            super::super::pyrowave::pwv1_header(320, 192, 60, false)
        );
        assert!(media.pyrowave_active());
        assert_eq!(
            forced.load(std::sync::atomic::Ordering::SeqCst),
            0,
            "every PyroWave frame is a keyframe"
        );

        media.on_unit(&buffer(true));
        media.on_pyrowave_frame(&pyro_frame());
        media.on_caps(&caps(960), "s");
        assert_eq!(
            sent(&mut client.rx),
            ["unit 1 pyrowave key"],
            "no standard unit or configuration reaches a PyroWave client"
        );
    }

    #[test]
    fn a_client_that_does_not_offer_keeps_the_session_wanting_and_the_next_one_launches() {
        let (media, _encoder, _) = granted();
        let first = media.attach(false, LAN).unwrap();
        assert_eq!(
            (first.config.codec, first.reason),
            ("h264", Some("client_no_decoder"))
        );
        assert!(!media.pyrowave_active());
        media.on_pyrowave_frame(&pyro_frame());
        let second = media.attach(true, LAN).unwrap();
        assert_eq!(
            (second.config.codec, second.reason),
            ("pyrowave", Some("launch"))
        );
        let third = media.attach(true, LAN).unwrap();
        assert_eq!(third.reason, Some("reconnect"));
    }

    #[test]
    fn a_round_trip_over_the_gate_clears_wanted_for_the_session() {
        let (media, _encoder, _) = granted();
        let slow = media.attach(true, Duration::from_millis(6)).unwrap();
        assert_eq!(
            (slow.config.codec, slow.reason),
            ("h264", Some("rtt_too_high"))
        );
        let later = media.attach(true, LAN).unwrap();
        assert_eq!(
            (later.config.codec, later.reason),
            ("h264", None),
            "no flap back on"
        );
    }

    #[test]
    fn an_unwanted_or_ungranted_session_never_gets_pyrowave_and_says_nothing() {
        let (media, _encoder, _) = counted();
        media.on_caps(&caps(1280), "s");
        assert_eq!(media.attach(true, LAN).unwrap().reason, None);
        media.grant_pyrowave(
            PyroWaveGrant {
                wanted: false,
                ..GRANT
            },
            (320, 192, 60),
            true,
        );
        let c = media.attach(true, LAN).unwrap();
        assert_eq!((c.config.codec, c.reason), ("h264", None));
    }

    #[test]
    fn codec_fallback_switches_to_the_standard_codec_at_a_new_configuration_and_a_keyframe() {
        let (media, _encoder, forced) = granted();
        let mut client = media.attach(true, LAN).unwrap();
        std::thread::sleep(KEYFRAME_SPACING);
        let before = forced.load(std::sync::atomic::Ordering::SeqCst);
        media.switch_off(Some(client.id + 1), "client_decode_error");
        assert!(
            media.pyrowave_active(),
            "another client's fallback moves nothing"
        );
        media.switch_off(Some(client.id), "client_decode_error");
        assert!(!media.pyrowave_active());
        assert_eq!(forced.load(std::sync::atomic::Ordering::SeqCst), before + 1);
        media.on_pyrowave_frame(&pyro_frame());
        media.on_unit(&buffer(false));
        media.on_unit(&buffer(true));
        assert_eq!(
            sent(&mut client.rx),
            ["config 2 h264 client_decode_error", "unit 2 std key"],
            "the hello_ack first; the delta before the keyframe is skipped"
        );
        let next = media.attach(true, LAN).unwrap();
        assert_eq!(
            (next.config.codec, next.reason),
            ("h264", None),
            "wanted was cleared"
        );
    }

    #[test]
    fn an_encoder_failure_before_any_client_is_the_first_hellos_reason() {
        let (media, _encoder, _) = granted();
        media.switch_off(None, "encoder_error");
        let c = media.attach(true, LAN).unwrap();
        assert_eq!((c.config.codec, c.reason), ("h264", Some("encoder_error")));
        assert_eq!(media.attach(true, LAN).unwrap().reason, None);
    }

    #[test]
    fn a_pyrowave_unit_is_flagged_keyframe_and_discardable_and_packed() {
        let mut u = unit(0);
        u.data = Bytes::from(super::super::pyrowave::tests::frame(
            320,
            192,
            &[(0, 400), (7, 10)],
        ));
        u.pyrowave = true;
        let mut pkt_seq = 0;
        let d = packetize(&u, &cfg_for(&u), MAX_DATAGRAM, &mut pkt_seq).unwrap();
        // 8 + 1600 > 1111: the SOF alone, the oversized block 0 in two parts, then block 7.
        assert_eq!(d.len(), 4);
        for (i, d) in d.iter().enumerate() {
            assert!(d.len() <= MAX_DATAGRAM);
            assert_eq!(d[5], FLAG_KEYFRAME | FLAG_DISCARDABLE);
            assert_eq!(u16::from_le_bytes([d[9], d[10]]), i as u16);
            assert_eq!(u16::from_le_bytes([d[11], d[12]]), 4);
        }
        assert_eq!(&d[1][HEADER..HEADER + 3], &[1, 0, 2], "kind 1, part 0 of 2");
    }

    #[test]
    fn a_switch_whose_hello_ack_found_the_queue_full_resends_it_with_its_reason() {
        let (media, _encoder, _) = granted();
        let mut client = media.attach(true, LAN).unwrap();
        for _ in 0..CLIENT_QUEUE {
            media.on_pyrowave_frame(&pyro_frame());
        }
        media.switch_off(Some(client.id), "client_decode_error");
        assert_eq!(
            sent(&mut client.rx).len(),
            CLIENT_QUEUE,
            "the hello_ack did not fit"
        );
        media.on_unit(&buffer(true));
        assert_eq!(
            sent(&mut client.rx),
            ["config 2 h264 client_decode_error", "unit 2 std key"],
            "the resync carries codec_switch_reason"
        );
    }

    #[test]
    fn standard_keyframes_the_transport_cannot_carry_do_not_end_a_pyrowave_client() {
        let (media, _encoder, _) = counted();
        media.on_caps(&streams::av1_caps(640), "s");
        media.grant_pyrowave(GRANT, (320, 192, 60), true);
        let client = media.attach(true, LAN).unwrap();
        assert_eq!(client.config.codec, "pyrowave");
        for _ in 0..UNUSABLE_KEYFRAMES * 2 {
            media.on_unit(&av1_unit_buffer(false, true));
        }
        assert!(
            media.attached() && client.gone.get().is_none(),
            "the standard stream is not this client's"
        );
    }
}
