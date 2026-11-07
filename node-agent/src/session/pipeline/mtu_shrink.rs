//! #1134: shrink the video packets mid-session when the path drops the full-size ones.
//!
//! The signal is the peer's generic NACKs (RFC 4585): each names a sequence number, and
//! the size every packet left at is recorded where `rtpbin` hands it to the transport.
//! A packet counts as lost to the path only once its retransmissions are lost too
//! ([`MTU_LOST_NACKS`]). Such loss on the packets a step would shrink, with little below
//! them, is a path MTU under ours. Loss on every size is not, and never shrinks. Steps
//! only go down.

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
/// How long windows with too few packets may hold a run open. Longer, and a run would
/// join loss from moments of motion minutes apart on an idle desktop.
const HOLD: Duration = Duration::from_secs(3);
/// NACKs that make a packet lost to the path. Chrome (`NackRequester`) NACKs a gap at
/// once, then again each time an RTT has passed since the last NACK, until the packet or
/// its RTX arrives. The RTX is itself about an RTT away, so a second NACK races the first
/// RTX and can mean only a late one; a third means two RTX in a row missed. An MTU drop
/// kills every RTX (the same size plus 2 bytes), so each lost large packet gets there
/// within about 2 RTT; a keyframe burst overflowing a queue or policer lets its RTX by.
const MTU_LOST_NACKS: u8 = 3;
/// Packets of each class a window needs before it says anything. A window short of
/// either holds the run of size-correlated windows rather than breaking it: a static
/// desktop or a 24 fps stream sends few small packets.
const MIN_SENT_LARGE: u32 = 20;
const MIN_SENT_SMALL: u32 = 10;
/// A path MTU under ours drops almost every large packet and no small one. A token bucket
/// a few packets deep drops large packets more often than small ones, but passes many of
/// them: it must not shrink (4 Mbit/s with a 4 KB bucket stepped twice on the lab). Shares
/// of first transmissions, i.e. of packets NACKed at all. Small loss is bounded relative
/// to large loss, so a path MTU on a link that also loses some packets still shrinks.
const LARGE_NACKED_MIN: f64 = 0.80;
const SMALL_NACKED_MAX_OF_LARGE: f64 = 0.25;
/// Every step after the first also needs near-zero small loss, the clean shape of a path
/// MTU, so a shaper that passes the bars above costs at most one step.
const SMALL_NACKED_MAX_AFTER_A_STEP: f64 = 0.05;
/// Large packets lost to the path ([`MTU_LOST_NACKS`]), at least this many and this share.
/// The share is low because Chrome clears its whole NACK list when it would pass 1000
/// packets and asks for a keyframe instead, so at high bitrates few lost packets live to a
/// third NACK; it is what keeps a keyframe burst whose RTX gets by from passing.
const MIN_LOST_LARGE: u32 = 10;
const LARGE_LOST_MIN: f64 = 0.10;
/// Sequence numbers remembered per SSRC: ~0.5 s at 80 Mbps. A later NACK is not counted,
/// which errs toward not shrinking.
const HISTORY: usize = 4096;
/// Sequence numbers read from one NACK packet. Its FCI can name ~276k, all walked under
/// the lock the send path takes.
const MAX_NACKED_PER_PACKET: usize = 1024;
/// SSRCs tracked: the media stream and its RTX stream, with room to spare.
const MAX_STREAMS: usize = 4;

const RTCP_RTPFB: u32 = 205;
const RTPFB_GENERIC_NACK: u32 = 1;

/// What one stream sent, had NACKed (at least once) and lost to the path ([`MTU_LOST_NACKS`])
/// in one window. "Large" is a packet a step would
/// shrink: bigger than the step below the current size. "Small" is under the floor, so no
/// hop we could shrink to drops it. Sizes between count as neither: under a raised cap a
/// low hop drops them too, and counted as small they would veto the shrink.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(super) struct Window {
    pub sent_large: u32,
    pub sent_small: u32,
    pub nacked_large: u32,
    pub nacked_small: u32,
    pub lost_large: u32,
}

impl Window {
    /// Whether the loss is a path MTU's: nearly every large packet, few small ones, and the
    /// retransmissions too; none at all of the small ones `after_a_step`. `None` when too
    /// few were sent to say.
    fn size_correlated(&self, after_a_step: bool) -> Option<bool> {
        if self.sent_large < MIN_SENT_LARGE || self.sent_small < MIN_SENT_SMALL {
            return None;
        }
        let share = |n: u32, of: u32| f64::from(n) / f64::from(of);
        let (large, small) = (
            share(self.nacked_large, self.sent_large),
            share(self.nacked_small, self.sent_small),
        );
        Some(
            large >= LARGE_NACKED_MIN
                && small <= large * SMALL_NACKED_MAX_OF_LARGE
                && (!after_a_step || small <= SMALL_NACKED_MAX_AFTER_A_STEP)
                && self.lost_large >= MIN_LOST_LARGE
                && share(self.lost_large, self.sent_large) >= LARGE_LOST_MIN,
        )
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
    /// When the last window with enough packets to say closed.
    last_counted: Option<Instant>,
    last_step: Option<Instant>,
}

impl Decision {
    pub(super) fn new(rtp_mtu: u32) -> Self {
        Decision {
            rtp_mtu,
            in_a_row: 0,
            last_counted: None,
            last_step: None,
        }
    }

    fn large(&self, size: usize) -> bool {
        size > self.rtp_mtu.saturating_sub(STEP) as usize
    }

    fn small(size: usize) -> bool {
        size < *RTP_MTU_RANGE.start() as usize
    }

    /// The size to step down to, or `None`.
    pub(super) fn on_window(&mut self, w: &Window, now: Instant) -> Option<u32> {
        if self
            .last_counted
            .is_some_and(|t| now.duration_since(t) > HOLD)
        {
            self.in_a_row = 0;
        }
        self.in_a_row = match w.size_correlated(self.last_step.is_some()) {
            Some(true) => self.in_a_row + 1,
            Some(false) => 0,
            None => return None,
        };
        self.last_counted = Some(now);
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
    nacks: u8,
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
            nacks: 0,
        };
        if large {
            stream.window.sent_large += 1;
        } else if Decision::small(size) {
            stream.window.sent_small += 1;
        }
        step
    }

    /// The peer NACKed `seqs` of `ssrc`. A packet counts as NACKed at its first NACK and as
    /// lost to the path at its [`MTU_LOST_NACKS`]th, once each.
    pub(super) fn nacked(&mut self, ssrc: u32, seqs: impl IntoIterator<Item = u16>) {
        let Some(stream) = self.streams.iter_mut().find(|s| s.ssrc == ssrc) else {
            return;
        };
        for seq in seqs {
            let sent = &mut stream.sent[usize::from(seq) % HISTORY];
            if sent.size == 0 || sent.seq != seq || sent.nacks >= MTU_LOST_NACKS {
                continue;
            }
            sent.nacks += 1;
            let (first, lost) = (sent.nacks == 1, sent.nacks == MTU_LOST_NACKS);
            let size = usize::from(sent.size);
            let w = &mut stream.window;
            if self.decision.large(size) {
                w.nacked_large += u32::from(first);
                w.lost_large += u32::from(lost);
            } else if Decision::small(size) {
                w.nacked_small += u32::from(first);
            }
        }
    }

    fn close_window_if_due(&mut self, now: Instant) -> Option<Step> {
        let start = *self.window_start.get_or_insert(now);
        if now.duration_since(start) < WINDOW {
            return None;
        }
        self.window_start = Some(now);
        // The stream the peer NACKs is the media one; RTX is never NACKed. With no NACKs,
        // the one that sent the most.
        let window = self
            .streams
            .iter()
            .map(|s| s.window)
            .max_by_key(|w| (w.nacked_large + w.nacked_small, w.sent_large + w.sent_small))
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

/// The sequence numbers a generic NACK's FCI names, (PID, BLP) pairs, at most
/// [`MAX_NACKED_PER_PACKET`].
pub(super) fn nack_seqs(fci: &[u8]) -> impl Iterator<Item = u16> + '_ {
    fci.as_chunks::<4>()
        .0
        .iter()
        .flat_map(|c| {
            let pid = u16::from_be_bytes([c[0], c[1]]);
            let blp = u16::from_be_bytes([c[2], c[3]]);
            std::iter::once(pid).chain(
                (0..16u16)
                    .filter(move |i| blp & (1 << i) != 0)
                    .map(move |i| pid.wrapping_add(i + 1)),
            )
        })
        .take(MAX_NACKED_PER_PACKET)
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
                let seqs: Vec<u16> = nack_seqs(&fci).collect();
                nacks.lock().unwrap().nacked(ssrc, seqs);
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
        lost_large = w.lost_large,
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

    /// A window whose NACKed large packets were all lost to the path, as at a path MTU.
    fn w(sent_large: u32, nacked_large: u32, sent_small: u32, nacked_small: u32) -> Window {
        wl(
            sent_large,
            nacked_large,
            nacked_large,
            sent_small,
            nacked_small,
        )
    }

    fn wl(sent_large: u32, nacked: u32, lost: u32, sent_small: u32, nacked_small: u32) -> Window {
        Window {
            sent_large,
            sent_small,
            nacked_large: nacked,
            nacked_small,
            lost_large: lost,
        }
    }

    #[test]
    fn a_window_is_size_correlated_only_when_large_packets_carry_the_loss() {
        let table = [
            ("clean", w(200, 0, 60, 0), Some(false)),
            (
                "a path MTU: every large packet lost",
                w(200, 200, 60, 0),
                Some(true),
            ),
            (
                "80% of large packets NACKed",
                w(200, 160, 60, 0),
                Some(true),
            ),
            ("just under 80%", w(200, 159, 60, 0), Some(false)),
            (
                "~50 Mbps: 92% NACKed, Chrome's 1000-packet list cap leaves 12% to a third NACK",
                wl(4000, 3680, 480, 600, 0),
                Some(true),
            ),
            (
                "a third NACK on just under a tenth",
                wl(200, 200, 19, 60, 0),
                Some(false),
            ),
            (
                "a keyframe burst: 90% NACKed, the RTX gets by",
                wl(200, 180, 0, 60, 0),
                Some(false),
            ),
            (
                "shallow bucket: 50% of large, 10% of small",
                wl(200, 100, 25, 60, 6),
                Some(false),
            ),
            (
                "shallow bucket: 85% of large, 10% of small (once, before any step)",
                wl(200, 170, 120, 60, 6),
                Some(true),
            ),
            (
                "a path MTU on a link losing 20% of everything",
                w(200, 200, 60, 12),
                Some(true),
            ),
            (
                "small loss at a quarter of large loss",
                w(200, 200, 60, 15),
                Some(true),
            ),
            (
                "small loss over a quarter of large loss",
                w(200, 200, 60, 16),
                Some(false),
            ),
            ("uniform 30%", w(200, 60, 60, 18), Some(false)),
            ("uniform 90%", w(200, 180, 60, 54), Some(false)),
            ("too few large packets sent", w(19, 19, 60, 0), None),
            ("too few small packets sent", w(200, 200, 9, 0), None),
            (
                "ten small packets are enough",
                w(200, 200, 10, 0),
                Some(true),
            ),
            (
                "enough sent, too few lost",
                wl(30, 30, 9, 60, 0),
                Some(false),
            ),
        ];
        for (what, window, want) in table {
            assert_eq!(window.size_correlated(false), want, "{what}: {window:?}");
        }
        // After a step, only the clean shape: small loss at most 5%.
        let after = [
            ("a path MTU, no small loss", w(200, 200, 60, 0), Some(true)),
            ("small loss at 5%", w(200, 200, 60, 3), Some(true)),
            ("small loss just over 5%", w(200, 200, 60, 4), Some(false)),
            (
                "a path MTU on a link losing 20% of everything",
                w(200, 200, 60, 12),
                Some(false),
            ),
            (
                "shallow bucket: 85% of large, 10% of small",
                wl(200, 170, 120, 60, 6),
                Some(false),
            ),
        ];
        for (what, window, want) in after {
            assert_eq!(
                window.size_correlated(true),
                want,
                "after a step, {what}: {window:?}"
            );
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
        let clean = w(200, 0, 60, 0);
        let thin = w(200, 200, 4, 0);
        let t0 = Instant::now();
        let at = |s: u64| t0 + Duration::from_secs(s);
        // (window, second it closes, the step wanted)
        let table: &[(&str, Window, u64, Option<u32>)] = &[
            ("one lossy window is not enough", lossy, 1, None),
            ("a clean one resets the run", clean, 2, None),
            ("lossy again", lossy, 3, None),
            ("too few packets to say", thin, 4, None),
            ("a thin window between holds the run", lossy, 5, Some(1100)),
            ("inside the cooldown, first", lossy, 6, None),
            ("inside the cooldown, second", lossy, 7, None),
            ("cooldown over", lossy, 8, Some(1000)),
            ("uniform loss never counts", uniform, 9, None),
            ("uniform loss never counts, again", uniform, 10, None),
            ("lossy", lossy, 11, None),
            ("lossy, to the floor", lossy, 12, Some(900)),
            ("at the floor", lossy, 15, None),
            ("still at the floor", lossy, 20, None),
        ];
        let mut d = Decision::new(1200);
        for (what, window, sec, want) in table {
            assert_eq!(d.on_window(window, at(*sec)), *want, "{what} (t={sec}s)");
        }
    }

    #[test]
    fn a_run_held_by_thin_windows_lapses_after_a_few_seconds() {
        let lossy = w(200, 200, 60, 0);
        let thin = w(4, 4, 4, 0);
        let t0 = Instant::now();
        let at = |s: u64| t0 + Duration::from_secs(s);
        let table: &[(&str, Window, u64, Option<u32>)] = &[
            ("lossy during motion", lossy, 1, None),
            ("idle", thin, 2, None),
            ("idle", thin, 300, None),
            ("idle, ten minutes on", thin, 600, None),
            ("lossy again, minutes after the first", lossy, 601, None),
            ("lossy, the second in a row", lossy, 602, Some(1100)),
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
    fn one_nack_packet_names_at_most_a_thousand_or_so() {
        // 4096 full pairs name 69632 sequence numbers.
        let fci: Vec<u8> = (0..4096u16)
            .flat_map(|i| {
                let [a, b] = i.wrapping_mul(17).to_be_bytes();
                [a, b, 0xff, 0xff]
            })
            .collect();
        assert_eq!(nack_seqs(&fci).count(), MAX_NACKED_PER_PACKET);
    }

    #[test]
    fn a_packet_counts_as_nacked_once_and_as_lost_at_its_third_nack_against_its_own_stream() {
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
        let media = |s: &Shrink| s.streams.iter().find(|st| st.ssrc == MEDIA).unwrap().window;
        let large: Vec<u16> = (0..40).map(|i| i * 2).collect();
        // NACKed once each; a second NACK can mean only a late RTX, so nothing is lost yet.
        s.nacked(MEDIA, large.iter().copied());
        s.nacked(MEDIA, large.iter().copied());
        assert_eq!(media(&s), wl(40, 40, 0, 40, 0));
        // The third counts, a fourth does not count again, and nor do seqs never sent or
        // the RTX stream.
        s.nacked(MEDIA, large.iter().copied());
        s.nacked(MEDIA, large.iter().copied());
        for _ in 0..3 {
            s.nacked(MEDIA, [1000, 2000]);
            s.nacked(RTX, [0]);
        }
        assert_eq!(media(&s), w(40, 40, 40, 0));
        // A window that late closes and is the first of a run: no step yet.
        assert_eq!(s.sent(MEDIA, 80, 300, t0 + WINDOW), None);
        assert!(s.streams.iter().all(|st| st.window.sent_large == 0));
    }

    #[derive(Clone, Copy)]
    enum Path {
        /// Every packet over this many bytes is dropped, its RTX (2 bytes more) too.
        Hop(usize),
        /// This percentage of every transmission, any size, RTX included.
        Lossy(u64),
        /// This percentage of the large packets' first transmission, and no RTX: a burst
        /// overflowing a queue or a policer. Each lost one is NACKed twice, the second
        /// racing its RTX.
        Burst(u64),
        /// A token bucket only a few packets deep (`tbf` 4 Mbit/s, 4 KB): every
        /// transmission, RTX included, is lost at the first percentage when it is a full
        /// fragment and at the second otherwise.
        Shaper(u64, u64),
        /// A hop, on a link that also loses this percentage of every transmission.
        HopAnd(usize, u64),
    }

    /// Frames as a payloader cuts them at `fps`, five full fragments and a tail of any size
    /// at the size of the moment, for `secs` seconds, NACKed as Chrome would through `path`.
    /// -> every step taken.
    fn steps(cap: u32, path: Path, fps: u64, secs: u64) -> Vec<u32> {
        const MEDIA: u32 = 1;
        let t0 = Instant::now();
        let mut s = Shrink::new(cap);
        let mut taken = Vec::new();
        let mut seq = 0u16;
        // splitmix64: each transmission of each packet an independent draw.
        let hash = |seq: u16, attempt: u64| {
            let mut z = (u64::from(seq) << 2 | attempt).wrapping_add(0x9e37_79b9_7f4a_7c15);
            z = (z ^ (z >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
            z = (z ^ (z >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
            (z ^ (z >> 31)) % 100
        };
        let lossy = |seq: u16, pct: u64| (0..4).take_while(|&a| hash(seq, a) < pct).count();
        for frame in 0..fps * secs {
            let now = t0 + Duration::from_millis(frame * 1000 / fps);
            let full = s.decision.rtp_mtu as usize - 20;
            let tail = 100 + (frame as usize * 37) % (full - 100);
            for size in [full, full, full, full, full, tail] {
                if let Some(step) = s.sent(MEDIA, seq, size, now) {
                    taken.push(step.to);
                }
                let nacks = match path {
                    Path::Hop(hop) if size > hop => 4,
                    Path::Hop(_) => 0,
                    Path::HopAnd(hop, _) if size > hop => 4,
                    Path::HopAnd(_, pct) | Path::Lossy(pct) => lossy(seq, pct),
                    Path::Burst(pct) if size == full && hash(seq, 0) < pct => 2,
                    Path::Burst(_) => 0,
                    Path::Shaper(large, small) => {
                        lossy(seq, if size == full { large } else { small })
                    }
                };
                for _ in 0..nacks {
                    s.nacked(MEDIA, [seq]);
                }
                seq = seq.wrapping_add(1);
            }
        }
        taken
    }

    fn first_step(cap: u32, path: Path, fps: u64) -> Option<u32> {
        steps(cap, path, fps, 6).first().copied()
    }

    #[test]
    fn a_shaper_costs_at_most_one_step_and_a_clean_deep_hop_steps_to_fit() {
        use Path::*;
        assert_eq!(
            steps(1200, Shaper(85, 10), 60, 30),
            [1100],
            "a severe shaper"
        );
        assert_eq!(
            steps(1200, Shaper(50, 10), 60, 30),
            Vec::<u32>::new(),
            "the lab's shallow bucket"
        );
        assert_eq!(
            steps(1436, Hop(1000), 60, 30),
            [1336, 1236, 1136, 1036, 936],
            "a clean hop four steps under a raised cap"
        );
        assert_eq!(
            steps(1200, HopAnd(1000, 20), 60, 30),
            [1100],
            "a deep hop on a lossy link"
        );
    }

    #[test]
    fn a_hop_anywhere_under_the_cap_shrinks_and_other_loss_does_not() {
        use Path::*;
        let table = [
            (
                "default, hop just under it",
                1200,
                Hop(1100),
                60,
                Some(1100),
            ),
            ("default, hop a step lower", 1200, Hop(1000), 60, Some(1100)),
            ("default, no hop", 1200, Hop(1300), 60, None),
            ("default, 30% of every size", 1200, Lossy(30), 60, None),
            ("default, 60% of every size", 1200, Lossy(60), 60, None),
            (
                "raised to 1400, a 1280 hop",
                1400,
                Hop(1280),
                60,
                Some(1300),
            ),
            (
                "raised to 1400, a 1000 hop",
                1400,
                Hop(1000),
                60,
                Some(1300),
            ),
            (
                "raised to 1436, a 1000 hop",
                1436,
                Hop(1000),
                60,
                Some(1336),
            ),
            (
                "raised to 1436, 30% of every size",
                1436,
                Lossy(30),
                60,
                None,
            ),
            (
                "bursts lose 40% of large, RTX gets by",
                1200,
                Burst(40),
                60,
                None,
            ),
            (
                "bursts lose 90% of large, RTX gets by",
                1200,
                Burst(90),
                60,
                None,
            ),
            (
                "24 fps, hop just under the cap",
                1200,
                Hop(1100),
                24,
                Some(1100),
            ),
            (
                "shallow bucket: 30% of large, 10% of small",
                1200,
                Shaper(30, 10),
                60,
                None,
            ),
            (
                "shallow bucket: 40% of large, 10% of small",
                1200,
                Shaper(40, 10),
                60,
                None,
            ),
            (
                "shallow bucket: 50% of large, 10% of small",
                1200,
                Shaper(50, 10),
                60,
                None,
            ),
            (
                "shallow bucket: 60% of large, 10% of small",
                1200,
                Shaper(60, 10),
                60,
                None,
            ),
            (
                "a hop just under the cap on a link losing 20% of everything",
                1200,
                HopAnd(1100, 20),
                60,
                Some(1100),
            ),
            (
                "shallow bucket at 1400: 50% of large, 10% of small",
                1400,
                Shaper(50, 10),
                60,
                None,
            ),
        ];
        for (what, cap, path, fps, want) in table {
            assert_eq!(first_step(cap, path, fps), want, "{what}");
        }
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
                        for _ in 0..MTU_LOST_NACKS {
                            s.nacked(MEDIA, [seq]);
                        }
                    }
                    seq = seq.wrapping_add(1);
                }
            }
        }
        assert_eq!(steps.len(), 1, "{steps:?}");
        assert_eq!((steps[0].from, steps[0].to), (1200, 1100));
        // Late NACKs for a packet sent before the step count for nothing now.
        let before = s.streams[0].window;
        for _ in 0..MTU_LOST_NACKS {
            s.nacked(MEDIA, [0, 1]);
        }
        assert_eq!(s.streams[0].window, before);
    }
}
