//! A session's video as the WebTransport client gets it: access units taken off the
//! encoded-frame boundary, and their packetizing into `0x01` datagrams
//! (`protocol/transport-webtransport.md` §Video).

use std::sync::atomic::{AtomicBool, Ordering};
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
/// Units queued for a client before it is resynced with a keyframe.
const CLIENT_QUEUE: usize = 32;
/// Forced keyframes closer together than this are coalesced into the last one.
const KEYFRAME_SPACING: Duration = Duration::from_millis(150);

/// The decoder configuration a `hello_ack` names.
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct VideoConfig {
    pub(crate) config_id: u8,
    pub(crate) codec: &'static str,
    /// `avcC`.
    pub(crate) description: Vec<u8>,
    pub(crate) width: i32,
    pub(crate) height: i32,
    pub(crate) fps: i32,
}

#[derive(Debug, Clone)]
pub(crate) struct Unit {
    pub(crate) data: Bytes,
    pub(crate) keyframe: bool,
    pub(crate) config_id: u8,
    pub(crate) frame_seq: u16,
    pub(crate) ts_90khz: u32,
    pub(crate) capture_us: u64,
}

/// What the tap sends a connected client, in pad order: a configuration always precedes
/// the first unit encoded under it.
#[derive(Debug)]
pub(crate) enum Out {
    Config(Arc<VideoConfig>),
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
    /// the next keyframe instead.
    resync: bool,
}

#[derive(Default)]
struct State {
    config: Option<Arc<VideoConfig>>,
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

/// The Gamepad API exposes at most four slots; a `gp` for a higher `i` is dropped, so a client
/// cannot grow the per-pad table.
const MAX_PADS: u64 = 4;

/// `Typing::client` while the session's WebRTC peer holds it (§Takeover: one media client
/// across both transports).
const WEBRTC: u64 = u64::MAX;

/// The client whose input the devices take: none (`0`), a WebTransport client's id or
/// [`WEBRTC`], and the buttons of its last reliable `gp` per pad, since a datagram `gp`
/// carries axes only.
#[derive(Default)]
struct Typing {
    client: u64,
    pads: std::collections::HashMap<u64, serde_json::Value>,
    /// When each recent takeover happened, on either transport.
    takeovers: std::collections::VecDeque<Instant>,
}

/// §Takeover's bound: at most this many takeovers of a session in any [`TAKEOVER_WINDOW`],
/// counted across both transports.
const TAKEOVER_LIMIT: usize = 6;
const TAKEOVER_WINDOW: Duration = Duration::from_secs(60);

impl Typing {
    /// The window holds [`TAKEOVER_LIMIT`] takeovers already.
    fn takeovers_full(&mut self, now: Instant) -> bool {
        while self
            .takeovers
            .front()
            .is_some_and(|at| now.duration_since(*at) >= TAKEOVER_WINDOW)
        {
            self.takeovers.pop_front();
        }
        self.takeovers.len() >= TAKEOVER_LIMIT
    }
}

/// The tap's side of a session: fed by the GStreamer streaming thread, read by the
/// connection task of the one client attached.
pub(crate) struct Media {
    state: Mutex<State>,
    input: super::Input,
    /// Whose input reaches the devices. Held across the injection, so a client that stopped
    /// being the attached one cannot slip an event in behind the release that ended it.
    typing: Mutex<Typing>,
    encoder: glib::WeakRef<gst::Element>,
    /// A WebTransport client took the session from the WebRTC peer; the runner has yet to
    /// stop that peer's media and tell it (`take_webrtc_displaced`).
    webrtc_displaced: AtomicBool,
}

/// The client half `Media::attach` hands back.
pub(crate) struct Attached {
    pub(crate) id: u64,
    pub(crate) config: Arc<VideoConfig>,
    pub(crate) rx: mpsc::Receiver<Out>,
    pub(crate) gone: Arc<OnceLock<Gone>>,
}

impl Media {
    pub(crate) fn new(encoder: &gst::Element, input: super::Input) -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::default(),
            input,
            typing: Mutex::default(),
            encoder: encoder.downgrade(),
            webrtc_displaced: AtomicBool::new(false),
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

    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// A configuration exists, so a `hello_ack` can be sent.
    pub(crate) fn ready(&self) -> bool {
        self.lock().config.is_some()
    }

    /// Make `self` stream to a new client, displacing the one before it. The new one
    /// starts at a keyframe.
    pub(crate) fn attach(&self) -> Option<Attached> {
        let (tx, rx) = mpsc::channel(CLIENT_QUEUE);
        let gone = Arc::new(OnceLock::new());
        let mut st = self.lock();
        if st.ended {
            return None;
        }
        let config = st.config.clone()?;
        st.next_client += 1;
        let id = st.next_client;
        let displaced = st.client.replace(Client {
            id,
            tx,
            gone: gone.clone(),
            resync: false,
        });
        if let Some(old) = displaced {
            let _ = old.gone.set(Gone::TakenOver);
        }
        st.want_key = true;
        drop(st);
        {
            // The new client's input starts from nothing held: what its predecessor held is
            // lifted now, before the new client can send, and its late input is dropped.
            let mut typing = self.typing.lock().unwrap_or_else(|e| e.into_inner());
            // A predecessor that has not finished leaving (`detach` clears the client, then
            // takes this lock) still owns the devices: whatever it holds is lifted either way.
            if typing.client != 0 {
                self.release(&mut typing);
            }
            if typing.client == WEBRTC {
                self.webrtc_displaced.store(true, Ordering::SeqCst);
            }
            typing.client = id;
        }
        self.keyframe();
        Some(Attached {
            id,
            config,
            rx,
            gone,
        })
    }

    pub(crate) fn attached(&self) -> bool {
        self.lock().client.is_some()
    }

    /// The host accepted a WebRTC peer (`transport-webtransport.md` §Takeover): it displaces
    /// the WebTransport client, and what that client held is lifted now, before the peer can
    /// send. That client's later detach, its 10 s silence included, then lifts nothing.
    /// True when a client was attached. Displacing anyone, the WebRTC peer before it
    /// included, counts as a takeover.
    pub(crate) fn webrtc_attached(&self) -> bool {
        let displaced = self.lock().client.take();
        if let Some(old) = &displaced {
            let _ = old.gone.set(Gone::TakenOver);
        }
        let mut typing = self.typing.lock().unwrap_or_else(|e| e.into_inner());
        if typing.client != 0 {
            typing.takeovers.push_back(Instant::now());
        }
        if typing.client != 0 && typing.client != WEBRTC {
            self.release(&mut typing);
        }
        typing.client = WEBRTC;
        displaced.is_some()
    }

    /// A WebTransport `hello` may take the session: false past §Takeover's bound. One that
    /// displaces a client is counted here, before its token is spent.
    pub(crate) fn admit_hello(&self) -> bool {
        let mut typing = self.typing.lock().unwrap_or_else(|e| e.into_inner());
        if typing.client == 0 {
            return true;
        }
        let now = Instant::now();
        if typing.takeovers_full(now) {
            return false;
        }
        typing.takeovers.push_back(now);
        true
    }

    /// A new WebRTC peer would be past §Takeover's bound. It is counted only when its answer
    /// is applied (`webrtc_attached`), since both PCs ask before either is answered.
    /// shortcut: a takeover in that round trip can pass the bound by one; reserve a slot
    /// here if that ever matters.
    pub(crate) fn takeovers_full(&self) -> bool {
        let mut typing = self.typing.lock().unwrap_or_else(|e| e.into_inner());
        typing.takeovers_full(Instant::now())
    }

    /// One WebRTC input event, `inject`ed only while the WebRTC peer holds the session.
    pub(crate) fn webrtc_input(&self, inject: impl FnOnce()) {
        let typing = self.typing.lock().unwrap_or_else(|e| e.into_inner());
        if typing.client == WEBRTC {
            inject();
        }
    }

    /// The WebRTC peer disconnected or its transport was retired: what it held is lifted,
    /// unless a WebTransport client holds the session. The peer keeps it: a transient
    /// disconnect can recover.
    pub(crate) fn release_webrtc(&self) {
        let mut typing = self.typing.lock().unwrap_or_else(|e| e.into_inner());
        if typing.client == WEBRTC {
            self.release(&mut typing);
        }
    }

    /// Whether a WebTransport client took the session from the WebRTC peer since the last
    /// call.
    pub(crate) fn take_webrtc_displaced(&self) -> bool {
        self.webrtc_displaced.swap(false, Ordering::SeqCst)
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
            config.config_id = old.config_id.wrapping_add(1);
        }
        let config = Arc::new(config);
        st.config = Some(config.clone());
        // Never send a delta across a change. The encoder normally restarts on a keyframe,
        // and the rung step forces one; `on_unit` asks only if a delta comes first.
        st.want_key = true;
        let sent = st
            .client
            .as_ref()
            .map(|c| c.tx.try_send(Out::Config(config)));
        match (sent, st.client.as_mut()) {
            (Some(Err(mpsc::error::TrySendError::Full(_))), Some(client)) => client.resync = true,
            (Some(Err(mpsc::error::TrySendError::Closed(_))), _) => st.client = None,
            _ => {}
        }
    }

    fn on_unit(&self, buffer: &gst::BufferRef) {
        let keyframe = !buffer.flags().contains(gst::BufferFlags::DELTA_UNIT);
        let mut st = self.lock();
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
        let (Some(config), Some(_)) = (st.config.clone(), st.client.as_ref()) else {
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
                .map(|c| c.tx.try_send(Out::Config(config.clone())));
            match sent {
                Some(Ok(())) => {
                    if let Some(client) = st.client.as_mut() {
                        client.resync = false;
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
        let Ok(map) = buffer.map_readable() else {
            // The client never gets this unit: the next one it is sent must not need it.
            st.want_key = true;
            return;
        };
        st.want_key = false;
        // Only a unit the client is sent takes a number: a skipped one would read as a
        // loss to NACK (§Loss recovery). A unit the full queue refuses gives it back.
        let frame_seq = st.frame_seq;
        st.frame_seq = frame_seq.wrapping_add(1);
        let unit = Unit {
            data: Bytes::copy_from_slice(&map),
            keyframe,
            config_id: config.config_id,
            frame_seq,
            ts_90khz,
            capture_us: SystemTime::now()
                .duration_since(SystemTime::UNIX_EPOCH)
                .map_or(0, |d| d.as_micros() as u64),
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
    })
}

/// Split `unit` into `0x01` datagrams of at most `max` bytes, numbered from `*pkt_seq`.
pub(crate) fn packetize(unit: &Unit, max: usize, pkt_seq: &mut u32) -> Vec<Bytes> {
    let room = max.saturating_sub(HEADER).max(1);
    let frags: Vec<&[u8]> = if unit.data.is_empty() {
        vec![&[]]
    } else {
        unit.data.chunks(room).collect()
    };
    let Ok(count) = u16::try_from(frags.len()) else {
        return Vec::new();
    };
    let flags = if unit.keyframe { FLAG_KEYFRAME } else { 0 };
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
        .collect()
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
        }
    }

    #[test]
    fn a_unit_splits_into_25_byte_headers_and_in_order_byte_ranges() {
        let u = unit(3000);
        let mut pkt_seq = u32::MAX;
        let dgrams = packetize(&u, MAX_DATAGRAM, &mut pkt_seq);
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
        let dgrams = packetize(&u, 1400, &mut pkt_seq);
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
            input: crate::webtransport::tests::no_input(),
            typing: Mutex::default(),
            encoder: glib::WeakRef::new(),
            webrtc_displaced: AtomicBool::new(false),
        });
        media.on_caps(&caps(1280, &avcc), "s");
        let mut client = media.attach().expect("ready after caps");
        assert_eq!(client.config.config_id, 0);
        assert_eq!(client.config.description, avcc);
        assert_eq!((client.config.width, client.config.fps), (1280, 60));

        media.on_caps(&caps(1280, &avcc), "s");
        assert!(
            client.rx.try_recv().is_err(),
            "the same caps are no new config"
        );
        media.on_caps(&caps(960, &avcc), "s");
        let Ok(Out::Config(next)) = client.rx.try_recv() else {
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
            Media::new(&encoder, crate::webtransport::tests::no_input()),
            encoder,
            forced,
        )
    }

    fn units(rx: &mut mpsc::Receiver<Out>) -> Vec<(bool, u8)> {
        let mut out = Vec::new();
        while let Ok(o) = rx.try_recv() {
            out.push(match o {
                Out::Unit(u) => (u.keyframe, u.config_id),
                Out::Config(c) => (false, 100 + c.config_id),
            });
        }
        out
    }

    #[test]
    fn a_new_configuration_forces_a_keyframe_and_no_delta_crosses_it() {
        let (media, _encoder, forced) = counted();
        media.on_caps(&caps(1280), "s");
        let mut client = media.attach().unwrap();
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
        let mut client = media.attach().unwrap();
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
        let mut client = media.attach().unwrap();
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
        let first = media.attach().unwrap();
        media.end();
        assert_eq!(first.gone.get(), Some(&Gone::SessionEnded));
        assert!(
            media.attach().is_none(),
            "a hello racing the end gets nothing"
        );
    }

    #[test]
    fn a_keyframe_request_coalesced_into_a_pli_is_sent_once_the_spacing_passes() {
        let (media, _encoder, forced) = counted();
        media.on_caps(&caps(1280), "s");
        let _client = media.attach().unwrap();
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

        let _client = media.attach().unwrap();
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
}
