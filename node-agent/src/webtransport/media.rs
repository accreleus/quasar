//! A session's video as the WebTransport client gets it: access units taken off the
//! encoded-frame boundary, and their packetizing into `0x01` datagrams
//! (`protocol/transport-webtransport.md` §Video).

use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant, SystemTime};

use bytes::{BufMut, Bytes, BytesMut};
use gstreamer as gst;
use gstreamer::prelude::*;
use tokio::sync::mpsc;

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

/// The decoder configuration a `hello_ack` names.
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct VideoConfig {
    pub(crate) config_id: u8,
    pub(crate) codec: &'static str,
    /// `avcC`, or PyroWave's `PWV1Header`.
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
    /// Set by `end`: no client attaches after it.
    ended: bool,
    last_forced: Option<Instant>,
    /// A request was coalesced into the last: the next unit past the spacing sends it.
    pending_key: bool,
}

/// The tap's side of a session: fed by the GStreamer streaming thread, read by the
/// connection task of the one client attached.
pub(crate) struct Media {
    state: Mutex<State>,
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
        trace: Option<(crate::session::pipeline::TraceTx, String)>,
    ) -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::default(),
            encoder: encoder.downgrade(),
            trace,
        })
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
        if let Some(old) = st.client.replace(Client {
            id,
            tx,
            gone: gone.clone(),
            resync: false,
            resync_reason: None,
            pyrowave,
        }) {
            let _ = old.gone.set(Gone::TakenOver);
        }
        st.want_key = true;
        let kbps = st.pyro.as_ref().map(|p| p.grant.target_kbps);
        drop(st);
        if let Some(reason) = reason {
            let kbps = (config.codec == "pyrowave").then_some(kbps).flatten();
            self.switched(None, config.codec, reason, config.config_id, kbps);
        }
        if config.codec != "pyrowave" {
            self.keyframe();
        }
        Some(Attached {
            id,
            config,
            reason,
            rx,
            gone,
        })
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

    pub(crate) fn detach(&self, id: u64) {
        let mut st = self.lock();
        if st.client.as_ref().is_some_and(|c| c.id == id) {
            st.client = None;
        }
    }

    pub(crate) fn end(&self) {
        let mut st = self.lock();
        st.ended = true;
        if let Some(old) = st.client.take() {
            let _ = old.gone.set(Gone::SessionEnded);
        }
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

    fn on_unit(&self, buffer: &gst::BufferRef) {
        let keyframe = !buffer.flags().contains(gst::BufferFlags::DELTA_UNIT);
        let mut st = self.lock();
        let frame_seq = st.frame_seq;
        st.frame_seq = frame_seq.wrapping_add(1);
        // openh264's second unit carries no PTS: it keeps the one before.
        let ts_90khz = buffer.pts().map_or(st.last_ts, |pts| {
            (pts.nseconds() as u128 * 90_000 / 1_000_000_000) as u32
        });
        st.last_ts = ts_90khz;
        if keyframe {
            st.pending_key = false;
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
        let Ok(map) = buffer.map_readable() else {
            return;
        };
        let unit = Unit {
            data: Bytes::copy_from_slice(&map),
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

/// H.264 in `stream-format=avc` with 4-byte lengths: the units are then already the
/// payload `EncodedVideoChunk` takes. Other codecs are later tickets.
fn video_config(caps: &gst::CapsRef) -> Option<VideoConfig> {
    let s = caps.structure(0)?;
    if s.name() != "video/x-h264" || s.get::<&str>("stream-format").ok()? != "avc" {
        return None;
    }
    let description = s
        .get::<gst::Buffer>("codec_data")
        .ok()?
        .map_readable()
        .ok()?
        .to_vec();
    // lengthSizeMinusOne in the avcC's fifth byte.
    if description.get(4).map(|b| b & 0x03) != Some(3) {
        return None;
    }
    let fps = s
        .get::<gst::Fraction>("framerate")
        .ok()
        .filter(|f| f.denom() > 0)
        .map_or(0, |f| (f.numer() + f.denom() / 2) / f.denom());
    Some(VideoConfig {
        config_id: 0,
        codec: "h264",
        description,
        width: s.get("width").ok()?,
        height: s.get("height").ok()?,
        fps,
        full_range: false,
    })
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
        (Media::new(&encoder, None), encoder, forced)
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
}
