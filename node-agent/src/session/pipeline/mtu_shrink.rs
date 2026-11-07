//! #1134: shrink the video packets mid-session when the path drops the full-size ones.
//!
//! The signal is the peer's generic NACKs (RFC 4585): each names a sequence number, and
//! the size every packet left at is recorded where `rtpbin` hands it to the transport.
//! Loss on the packets a step would shrink, with little loss below them, is a path MTU
//! under ours. Loss on every size is not, and never shrinks. Steps only go down.

use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use gstreamer as gst;
use gstreamer::prelude::*;

use super::codec_chain::{rtp_mtu_of, set_rtp_mtu};
use crate::session::RTP_MTU_RANGE;

/// One step, in bytes of `QUASAR_RTP_MTU`.
const STEP: u32 = 100;
const WINDOW: Duration = Duration::from_secs(1);
/// Size-correlated windows needed in a row before a step.
const WINDOWS_IN_A_ROW: u32 = 2;
/// The least time between two steps: the keyframe after a step lands well inside it.
const COOLDOWN: Duration = Duration::from_secs(3);
/// Packets of each class a window needs before it says anything.
const MIN_SENT: u32 = 20;
const MIN_NACKED_LARGE: u32 = 10;
/// Share of the large packets NACKed. Low because Chrome stops NACKing past 1000
/// missing packets and asks for a keyframe instead, so a path dropping every large
/// packet can show well under half of them NACKed.
const LARGE_LOSS_MIN: f64 = 0.25;
/// Small packets' loss as a share of the large packets' loss, at most.
const SMALL_LOSS_MAX_OF_LARGE: f64 = 0.2;
/// Sequence numbers remembered per SSRC. A NACK for an older one is not counted.
const HISTORY: usize = 4096;
/// SSRCs tracked: the media stream and its RTX stream, with room to spare.
const MAX_STREAMS: usize = 4;

const RTCP_RTPFB: u32 = 205;
const RTPFB_GENERIC_NACK: u32 = 1;

/// What one stream sent and had NACKed in one window. "Large" is a packet a step would
/// shrink: bigger than the step below the current size.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(super) struct Window {
    pub sent_large: u32,
    pub sent_small: u32,
    pub nacked_large: u32,
    pub nacked_small: u32,
}

impl Window {
    /// Loss concentrated on the large packets.
    fn size_correlated(&self) -> bool {
        if self.sent_large < MIN_SENT
            || self.sent_small < MIN_SENT
            || self.nacked_large < MIN_NACKED_LARGE
        {
            return false;
        }
        let large = f64::from(self.nacked_large) / f64::from(self.sent_large);
        let small = f64::from(self.nacked_small) / f64::from(self.sent_small);
        large >= LARGE_LOSS_MIN && small <= large * SMALL_LOSS_MAX_OF_LARGE
    }
}

/// The step down from `rtp_mtu`, or `None` at the floor.
fn next_step(rtp_mtu: u32) -> Option<u32> {
    let floor = *RTP_MTU_RANGE.start();
    (rtp_mtu > floor).then(|| rtp_mtu.saturating_sub(STEP).max(floor))
}

/// The decision, fed one window at a time with the instant it closed.
pub(super) struct Decision {
    rtp_mtu: u32,
    in_a_row: u32,
    last_step: Option<Instant>,
}

impl Decision {
    pub(super) fn new(rtp_mtu: u32) -> Self {
        Decision {
            rtp_mtu,
            in_a_row: 0,
            last_step: None,
        }
    }

    fn large(&self, size: usize) -> bool {
        size > self.rtp_mtu.saturating_sub(STEP) as usize
    }

    /// The size to step down to, or `None`.
    pub(super) fn on_window(&mut self, w: &Window, now: Instant) -> Option<u32> {
        self.in_a_row = if w.size_correlated() {
            self.in_a_row + 1
        } else {
            0
        };
        if self.in_a_row < WINDOWS_IN_A_ROW
            || self
                .last_step
                .is_some_and(|t| now.duration_since(t) < COOLDOWN)
        {
            return None;
        }
        let to = next_step(self.rtp_mtu)?;
        self.rtp_mtu = to;
        self.in_a_row = 0;
        self.last_step = Some(now);
        Some(to)
    }
}

#[derive(Clone, Copy, Default)]
struct Sent {
    seq: u16,
    /// 0: nothing recorded in this slot.
    size: u16,
    nacked: bool,
}

struct Stream {
    ssrc: u32,
    sent: Box<[Sent]>,
    window: Window,
}

/// A step the send path took: from, to, and the window that decided it.
#[derive(Debug, PartialEq)]
pub(super) struct Step {
    pub from: u32,
    pub to: u32,
    pub window: Window,
}

/// The sizes sent per SSRC, the NACKs against them, and the windows they make.
pub(super) struct Shrink {
    decision: Decision,
    streams: Vec<Stream>,
    window_start: Option<Instant>,
}

impl Shrink {
    pub(super) fn new(rtp_mtu: u32) -> Self {
        Shrink {
            decision: Decision::new(rtp_mtu),
            streams: Vec::new(),
            window_start: None,
        }
    }

    /// A packet left. Closes the window when it is due, and returns the step it decided.
    pub(super) fn sent(&mut self, ssrc: u32, seq: u16, size: usize, now: Instant) -> Option<Step> {
        let step = self.close_window_if_due(now);
        let large = self.decision.large(size);
        let at = match self.streams.iter().position(|s| s.ssrc == ssrc) {
            Some(at) => at,
            None if self.streams.len() < MAX_STREAMS => {
                self.streams.push(Stream {
                    ssrc,
                    sent: vec![Sent::default(); HISTORY].into_boxed_slice(),
                    window: Window::default(),
                });
                self.streams.len() - 1
            }
            None => return step,
        };
        let stream = &mut self.streams[at];
        stream.sent[usize::from(seq) % HISTORY] = Sent {
            seq,
            size: size.clamp(1, usize::from(u16::MAX)) as u16,
            nacked: false,
        };
        if large {
            stream.window.sent_large += 1;
        } else {
            stream.window.sent_small += 1;
        }
        step
    }

    /// The peer NACKed `seqs` of `ssrc`. A packet counts once however often it is NACKed.
    pub(super) fn nacked(&mut self, ssrc: u32, seqs: impl IntoIterator<Item = u16>) {
        let Some(stream) = self.streams.iter_mut().find(|s| s.ssrc == ssrc) else {
            return;
        };
        for seq in seqs {
            let sent = &mut stream.sent[usize::from(seq) % HISTORY];
            if sent.size == 0 || sent.seq != seq || sent.nacked {
                continue;
            }
            sent.nacked = true;
            if self.decision.large(usize::from(sent.size)) {
                stream.window.nacked_large += 1;
            } else {
                stream.window.nacked_small += 1;
            }
        }
    }

    fn close_window_if_due(&mut self, now: Instant) -> Option<Step> {
        let start = *self.window_start.get_or_insert(now);
        if now.duration_since(start) < WINDOW {
            return None;
        }
        self.window_start = Some(now);
        // The stream the peer NACKs is the media one; RTX is never NACKed.
        let window = self
            .streams
            .iter()
            .map(|s| s.window)
            .max_by_key(|w| w.nacked_large + w.nacked_small)
            .unwrap_or_default();
        for s in &mut self.streams {
            s.window = Window::default();
        }
        let from = self.decision.rtp_mtu;
        let to = self.decision.on_window(&window, now)?;
        // Packets sent at the old size say nothing about the new one.
        for s in &mut self.streams {
            s.sent.fill(Sent::default());
        }
        Some(Step { from, to, window })
    }
}

/// The sequence numbers a generic NACK's FCI names: (PID, BLP) pairs.
pub(super) fn nack_seqs(fci: &[u8]) -> impl Iterator<Item = u16> + '_ {
    fci.chunks_exact(4).flat_map(|c| {
        let pid = u16::from_be_bytes([c[0], c[1]]);
        let blp = u16::from_be_bytes([c[2], c[3]]);
        std::iter::once(pid).chain(
            (0..16u16)
                .filter(move |i| blp & (1 << i) != 0)
                .map(move |i| pid.wrapping_add(i + 1)),
        )
    })
}

/// Watch `webrtc`'s send session and shrink `payloader` on size-correlated loss. Call
/// before the transport negotiates: the hooks go on as `rtpbin` adds its send pad. A
/// payloader already at the floor gets none.
pub(super) fn attach(
    webrtc: &gst::Element,
    payloader: &gst::Element,
    encoder: &gst::Element,
    session_id: String,
) {
    let Some(rtp_mtu) = rtp_mtu_of(payloader).filter(|&m| next_step(m).is_some()) else {
        return;
    };
    let Some(rtpbin) = webrtc
        .downcast_ref::<gst::Bin>()
        .and_then(|b| b.by_name("rtpbin"))
    else {
        tracing::warn!(
            token = "rtp-mtu-shrink-no-rtpbin",
            session_id = %session_id,
            "webrtcbin has no rtpbin: video packets will not shrink on a smaller path MTU"
        );
        return;
    };
    let shrink = Arc::new(Mutex::new(Shrink::new(rtp_mtu)));
    // Weak: these closures live inside the encode pipeline that holds both elements.
    let (payloader, encoder) = (payloader.downgrade(), encoder.downgrade());
    rtpbin.connect_pad_added(move |rtpbin, pad| {
        let Some(id) = pad
            .name()
            .strip_prefix("send_rtp_src_")
            .and_then(|n| n.parse::<u32>().ok())
        else {
            return;
        };
        // rtpbin adds this pad holding its lock, and `get-internal-session` takes it: ask
        // from GStreamer's pool once the lock is released, or negotiation deadlocks.
        let nacks = shrink.clone();
        rtpbin.call_async(move |rtpbin| {
            let Some(session) =
                rtpbin.emit_by_name::<Option<glib::Object>>("get-internal-session", &[&id])
            else {
                return;
            };
            session.connect("on-feedback-rtcp", false, move |v| {
                if v[1].get::<u32>().ok()? != RTCP_RTPFB
                    || v[2].get::<u32>().ok()? != RTPFB_GENERIC_NACK
                {
                    return None;
                }
                let ssrc = v[4].get::<u32>().ok()?;
                let fci = v[5].get::<Option<gst::Buffer>>().ok()??;
                let fci = fci.map_readable().ok()?;
                nacks.lock().unwrap().nacked(ssrc, nack_seqs(&fci));
                None
            });
        });
        let (shrink, payloader, encoder) = (shrink.clone(), payloader.clone(), encoder.clone());
        let session_id = session_id.clone();
        pad.add_probe(
            gst::PadProbeType::BUFFER | gst::PadProbeType::BUFFER_LIST,
            move |_, info| {
                let now = Instant::now();
                let mut step = None;
                {
                    let mut shrink = shrink.lock().unwrap();
                    let mut record = |b: &gst::BufferRef| {
                        if let Ok(rtp) = gstreamer_rtp::RTPBuffer::from_buffer_readable(b) {
                            if let Some(s) = shrink.sent(rtp.ssrc(), rtp.seq(), b.size(), now) {
                                step = Some(s);
                            }
                        }
                    };
                    match &info.data {
                        Some(gst::PadProbeData::Buffer(b)) => record(b),
                        Some(gst::PadProbeData::BufferList(l)) => l.iter().for_each(record),
                        _ => {}
                    }
                }
                if let Some(step) = step {
                    take_step(&step, &payloader, &encoder, &session_id);
                }
                gst::PadProbeReturn::Ok
            },
        );
    });
}

fn take_step(
    step: &Step,
    payloader: &glib::WeakRef<gst::Element>,
    encoder: &glib::WeakRef<gst::Element>,
    session_id: &str,
) {
    let Some(payloader) = payloader.upgrade() else {
        return;
    };
    set_rtp_mtu(&payloader, step.to);
    let w = &step.window;
    tracing::warn!(
        token = "rtp-mtu-shrunk",
        session_id = %session_id,
        from = step.from,
        to = step.to,
        nacked_large = w.nacked_large,
        sent_large = w.sent_large,
        nacked_small = w.nacked_small,
        sent_small = w.sent_small,
        "the path drops full-size video packets: RTP packets shrunk from {} to {} bytes",
        step.from,
        step.to
    );
    // The peer is missing frames: a keyframe at the new size now, not at its next PLI.
    if let Some(encoder) = encoder.upgrade() {
        super::resize::force_idr(&encoder);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn w(sent_large: u32, nacked_large: u32, sent_small: u32, nacked_small: u32) -> Window {
        Window {
            sent_large,
            sent_small,
            nacked_large,
            nacked_small,
        }
    }

    #[test]
    fn a_window_is_size_correlated_only_when_large_packets_carry_the_loss() {
        let table = [
            ("clean", w(200, 0, 60, 0), false),
            ("every large packet lost", w(200, 200, 60, 0), true),
            ("a quarter of them NACKed", w(200, 50, 60, 0), true),
            ("just under a quarter", w(200, 49, 60, 0), false),
            (
                "a deeper hop also takes some small ones",
                w(200, 180, 60, 9),
                true,
            ),
            ("uniform 30%", w(200, 60, 60, 18), false),
            ("uniform 80%", w(200, 160, 60, 48), false),
            (
                "small loss at a fifth of large loss",
                w(200, 100, 60, 6),
                true,
            ),
            ("small loss just over a fifth", w(200, 100, 60, 7), false),
            ("too few large packets sent", w(19, 19, 60, 0), false),
            ("too few small packets sent", w(200, 200, 19, 0), false),
            ("too few NACKs", w(30, 9, 60, 0), false),
        ];
        for (what, window, want) in table {
            assert_eq!(window.size_correlated(), want, "{what}: {window:?}");
        }
    }

    #[test]
    fn steps_go_down_by_a_hundred_to_the_floor() {
        let chain = |mut m: u32| {
            let mut out = vec![m];
            while let Some(n) = next_step(m) {
                out.push(n);
                m = n;
            }
            out
        };
        assert_eq!(chain(1200), [1200, 1100, 1000, 900]);
        assert_eq!(chain(1400), [1400, 1300, 1200, 1100, 1000, 900]);
        assert_eq!(chain(1436), [1436, 1336, 1236, 1136, 1036, 936, 900]);
        assert_eq!(chain(900), [900]);
    }

    #[test]
    fn the_decision_wants_two_windows_in_a_row_and_a_cooldown_between_steps() {
        let lossy = w(200, 200, 60, 0);
        let uniform = w(200, 60, 60, 18);
        let clean = Window::default();
        let t0 = Instant::now();
        let at = |s: u64| t0 + Duration::from_secs(s);
        // (window, second it closes, the step wanted)
        let table: &[(&str, Window, u64, Option<u32>)] = &[
            ("one lossy window is not enough", lossy, 1, None),
            ("a clean one resets the run", clean, 2, None),
            ("lossy again", lossy, 3, None),
            ("two in a row step", lossy, 4, Some(1100)),
            ("inside the cooldown, first", lossy, 5, None),
            ("inside the cooldown, second", lossy, 6, None),
            ("cooldown over", lossy, 7, Some(1000)),
            ("uniform loss never counts", uniform, 8, None),
            ("uniform loss never counts, again", uniform, 9, None),
            ("lossy", lossy, 10, None),
            ("lossy, to the floor", lossy, 11, Some(900)),
            ("at the floor", lossy, 15, None),
            ("still at the floor", lossy, 20, None),
        ];
        let mut d = Decision::new(1200);
        for (what, window, sec, want) in table {
            assert_eq!(d.on_window(window, at(*sec)), *want, "{what} (t={sec}s)");
        }
    }

    #[test]
    fn a_uniformly_lossy_path_never_shrinks() {
        let mut d = Decision::new(1200);
        let t0 = Instant::now();
        for (i, loss) in [0.1, 0.3, 0.5, 0.8, 0.95]
            .iter()
            .cycle()
            .take(100)
            .enumerate()
        {
            let window = w(200, (200.0 * loss) as u32, 60, (60.0 * loss) as u32);
            let now = t0 + Duration::from_secs(i as u64);
            assert_eq!(d.on_window(&window, now), None, "loss {loss} at {i}s");
        }
    }

    #[test]
    fn nack_fci_names_each_pid_and_its_bitmask() {
        // PID 10 with BLP bits 0 and 15 (11, 26); PID 65535 with bit 0 (wraps to 0).
        let fci = [0, 10, 0x80, 0x01, 0xff, 0xff, 0, 1];
        assert_eq!(nack_seqs(&fci).collect::<Vec<_>>(), [10, 11, 26, 65535, 0]);
        assert_eq!(
            nack_seqs(&[0, 1, 0]).count(),
            0,
            "a short FCI names nothing"
        );
    }

    #[test]
    fn the_send_history_counts_each_nack_once_against_its_own_stream() {
        const MEDIA: u32 = 0xaaaa;
        const RTX: u32 = 0xbbbb;
        let t0 = Instant::now();
        let mut s = Shrink::new(1200);
        for (step, seq) in (0..40u16).enumerate() {
            let now = t0 + Duration::from_millis(step as u64);
            assert!(s.sent(MEDIA, seq * 2, 1180, now).is_none());
            assert!(s.sent(MEDIA, seq * 2 + 1, 300, now).is_none());
        }
        assert!(s.sent(RTX, 0, 1180, t0).is_none());
        // Every large packet, twice over, plus seqs never sent and the RTX stream.
        let large: Vec<u16> = (0..40).map(|i| i * 2).collect();
        s.nacked(MEDIA, large.iter().copied());
        s.nacked(MEDIA, large.iter().copied());
        s.nacked(MEDIA, [1000, 2000]);
        s.nacked(RTX, [0]);
        let media = s.streams.iter().find(|st| st.ssrc == MEDIA).unwrap();
        assert_eq!(media.window, w(40, 40, 40, 0));
        // A window that late closes and is the first of a run: no step yet.
        assert_eq!(s.sent(MEDIA, 80, 300, t0 + WINDOW), None);
        assert!(s.streams.iter().all(|st| st.window.sent_large == 0));
    }

    #[test]
    fn a_step_forgets_what_was_sent_at_the_old_size() {
        const MEDIA: u32 = 1;
        let t0 = Instant::now();
        let mut s = Shrink::new(1200);
        let mut seq = 0u16;
        let mut steps = Vec::new();
        for window in 0..3u64 {
            for i in 0..60u64 {
                let now = t0 + WINDOW * window as u32 + Duration::from_millis(i);
                for size in [1180, 1180, 300] {
                    if let Some(step) = s.sent(MEDIA, seq, size, now) {
                        steps.push(step);
                    }
                    if size > 1100 {
                        s.nacked(MEDIA, [seq]);
                    }
                    seq = seq.wrapping_add(1);
                }
            }
        }
        assert_eq!(steps.len(), 1, "{steps:?}");
        assert_eq!((steps[0].from, steps[0].to), (1200, 1100));
        // A late NACK for a packet sent before the step counts for nothing now.
        let before = s.streams[0].window;
        s.nacked(MEDIA, [0, 1]);
        assert_eq!(s.streams[0].window, before);
    }
}
