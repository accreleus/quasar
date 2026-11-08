//! The video retransmit buffer: what the agent answers a `nack` from
//! (`protocol/transport-webtransport.md` §NACK, §Loss recovery).

use std::collections::VecDeque;
use std::time::{Duration, Instant};

use bytes::{Bytes, BytesMut};

/// `hello_ack.retransmit_window_ms`: the least time a datagram is held (§Loss recovery).
pub(crate) const WINDOW: Duration = Duration::from_millis(250);
/// The memory bound on what is held; past it the oldest frames go first, window or not.
const MAX_BYTES: usize = 16 * 1024 * 1024;
/// Resends per fragment: the client's first NACK and its one repeat (§Loss recovery).
/// Past it the fragment is a `nack_miss`, so a client that never receives it gives up.
const MAX_RESENDS: u8 = 2;
/// §NACK: the agent ignores ranges past the 32nd.
const MAX_RANGES: usize = 32;
const FLAG_RETRANSMIT: u8 = 4;

/// `nack`s a client may send in a burst, and per second after it. A frame takes one
/// `nack` and one repeat; 120 fps with every frame lossy is 240 a second.
const NACK_BURST: f64 = 100.0;
const NACK_RATE: f64 = 500.0;

/// Bounds the `nack`s a connection gets answered: each costs a parse, a lookup and up to
/// a frame of resends, so a flooding client is capped. One past the cap goes unanswered.
pub(crate) struct NackBudget {
    tokens: f64,
    at: Instant,
}

impl NackBudget {
    pub(crate) fn new(now: Instant) -> Self {
        Self {
            tokens: NACK_BURST,
            at: now,
        }
    }

    pub(crate) fn take(&mut self, now: Instant) -> bool {
        let refill = now.saturating_duration_since(self.at).as_secs_f64() * NACK_RATE;
        self.tokens = (self.tokens + refill).min(NACK_BURST);
        self.at = now;
        if self.tokens < 1.0 {
            return false;
        }
        self.tokens -= 1.0;
        true
    }
}

struct Held {
    at: Instant,
    frame_seq: u16,
    frags: Vec<Bytes>,
    resent: Vec<u8>,
}

#[derive(Default)]
pub(crate) struct Retransmit {
    frames: VecDeque<Held>,
    bytes: usize,
}

/// A `nack`'s answer: the datagrams to resend and the ranges for `nack_miss`.
#[derive(Debug, Default, PartialEq)]
pub(crate) struct Answer {
    pub(crate) resend: Vec<Bytes>,
    pub(crate) missed: Vec<[u16; 2]>,
}

impl Retransmit {
    /// Hold a frame's datagrams, as sent. `window` is the hold time now: at least
    /// [`WINDOW`] and twice the smoothed RTT.
    pub(crate) fn keep(&mut self, frame_seq: u16, frags: &[Bytes], now: Instant, window: Duration) {
        self.expire(now, window);
        if frags.is_empty() {
            return;
        }
        let bytes = frags.iter().map(Bytes::len).sum::<usize>();
        self.bytes += bytes;
        self.frames.push_back(Held {
            at: now,
            frame_seq,
            frags: frags.to_vec(),
            resent: vec![0; frags.len()],
        });
        while self.bytes > MAX_BYTES {
            let Some(old) = self.frames.pop_front() else {
                break;
            };
            self.bytes -= old.frags.iter().map(Bytes::len).sum::<usize>();
        }
    }

    /// Answer `nack {frame_seq, frags}`. Each held fragment goes again as a new datagram
    /// numbered from `*pkt_seq` with `retransmit` set; out-of-range indices are ignored.
    pub(crate) fn answer(
        &mut self,
        frame_seq: u16,
        ranges: &[[u16; 2]],
        pkt_seq: &mut u32,
        now: Instant,
        window: Duration,
    ) -> Answer {
        self.expire(now, window);
        let ranges = ranges.iter().take(MAX_RANGES).filter(|[a, b]| a <= b);
        // Newest first: frame_seq wraps, and only the latest holder of a number is it.
        let Some(held) = self
            .frames
            .iter_mut()
            .rev()
            .find(|h| h.frame_seq == frame_seq)
        else {
            return Answer {
                resend: Vec::new(),
                missed: ranges.copied().collect(),
            };
        };
        let mut answer = Answer::default();
        let last = held.frags.len() - 1;
        for &[a, b] in ranges {
            for i in a as usize..=(b as usize).min(last) {
                if held.resent[i] >= MAX_RESENDS {
                    miss(&mut answer.missed, i as u16);
                    continue;
                }
                held.resent[i] += 1;
                let mut d = BytesMut::from(&held.frags[i][..]);
                d[1..5].copy_from_slice(&pkt_seq.to_le_bytes());
                d[5] |= FLAG_RETRANSMIT;
                *pkt_seq = pkt_seq.wrapping_add(1);
                answer.resend.push(d.freeze());
            }
        }
        answer
    }

    fn expire(&mut self, now: Instant, window: Duration) {
        while let Some(old) = self
            .frames
            .front()
            .filter(|h| now.duration_since(h.at) > window)
        {
            self.bytes -= old.frags.iter().map(Bytes::len).sum::<usize>();
            self.frames.pop_front();
        }
    }
}

/// Add `i` to `ranges`, extending the last range when it is the next index.
fn miss(ranges: &mut Vec<[u16; 2]>, i: u16) {
    if let Some(last) = ranges.last_mut().filter(|l| l[1].checked_add(1) == Some(i)) {
        last[1] = i;
    } else if ranges.len() < MAX_RANGES {
        ranges.push([i, i]);
    }
}

/// PyroWave frame_seqs remembered for ignoring NACKs: well past the retransmit window at 60 fps.
const PYROWAVE_FRAMES_REMEMBERED: usize = 64;

/// The frame_seqs of recent PyroWave frames: a NACK naming one is ignored (codec-pyrowave.md
/// §Loss: never retransmitted, no `nack_miss`). Forgotten when the client leaves PyroWave:
/// frame_seq keeps counting and wraps, and a standard frame reusing an id must be answered.
#[derive(Default)]
pub(crate) struct PyroWaveFrames(VecDeque<u16>);

impl PyroWaveFrames {
    pub(crate) fn remember(&mut self, frame_seq: u16) {
        if self.0.len() == PYROWAVE_FRAMES_REMEMBERED {
            self.0.pop_front();
        }
        self.0.push_back(frame_seq);
    }

    pub(crate) fn ignores(&self, frame_seq: u16) -> bool {
        self.0.contains(&frame_seq)
    }

    pub(crate) fn forget(&mut self) {
        self.0.clear();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::webtransport::media::{packetize, Unit, VideoConfig, MAX_DATAGRAM};

    fn frame(frame_seq: u16, len: usize, pkt_seq: &mut u32) -> Vec<Bytes> {
        let unit = Unit {
            data: Bytes::from(vec![7u8; len]),
            keyframe: false,
            config_id: 0,
            frame_seq,
            ts_90khz: 0,
            capture_us: 0,
            pyrowave: false,
        };
        let config = VideoConfig {
            config_id: 0,
            codec: "h264",
            description: Vec::new(),
            width: 64,
            height: 64,
            fps: 60,
            full_range: false,
        };
        packetize(&unit, &config, MAX_DATAGRAM, pkt_seq).unwrap()
    }

    #[test]
    fn a_held_fragment_is_resent_with_a_new_pkt_seq_and_the_retransmit_flag() {
        let (now, mut pkt_seq) = (Instant::now(), 0);
        let mut buf = Retransmit::default();
        let sent = frame(9, 5000, &mut pkt_seq); // 5 fragments, pkt_seq 0..=4
        buf.keep(9, &sent, now, WINDOW);

        let a = buf.answer(9, &[[1, 2], [4, 70], [3, 1]], &mut pkt_seq, now, WINDOW);
        assert!(a.missed.is_empty(), "out-of-range and reversed are ignored");
        assert_eq!(a.resend.len(), 3);
        for (d, (i, seq)) in a.resend.iter().zip([(1, 5u32), (2, 6), (4, 7)]) {
            assert_eq!(u32::from_le_bytes(d[1..5].try_into().unwrap()), seq);
            assert_eq!(d[5], FLAG_RETRANSMIT);
            assert_eq!(d[6..], sent[i][6..], "same header and payload otherwise");
        }
        assert_eq!(pkt_seq, 8);
    }

    #[test]
    fn past_the_window_or_the_resend_budget_or_unknown_is_a_miss() {
        let (now, mut pkt_seq) = (Instant::now(), 0);
        let mut buf = Retransmit::default();
        buf.keep(1, &frame(1, 3000, &mut pkt_seq), now, WINDOW);
        buf.keep(2, &frame(2, 3000, &mut pkt_seq), now + WINDOW, WINDOW);

        for _ in 0..MAX_RESENDS {
            assert_eq!(
                buf.answer(2, &[[0, 2]], &mut pkt_seq, now + WINDOW, WINDOW)
                    .resend
                    .len(),
                3
            );
        }
        let spent = buf.answer(2, &[[0, 0], [2, 2]], &mut pkt_seq, now + WINDOW, WINDOW);
        assert_eq!(
            spent,
            Answer {
                resend: vec![],
                missed: vec![[0, 0], [2, 2]]
            }
        );

        let later = now + WINDOW + Duration::from_millis(1);
        let gone = buf.answer(1, &[[0, 1]], &mut pkt_seq, later, WINDOW);
        assert_eq!(gone.missed, [[0, 1]], "frame 1 aged out");
        assert_eq!(
            buf.answer(40, &[[0, 0]], &mut pkt_seq, later, WINDOW)
                .missed,
            [[0, 0]]
        );
    }

    #[test]
    fn the_nack_budget_caps_a_burst_and_refills_at_its_rate() {
        let now = Instant::now();
        let mut budget = NackBudget::new(now);
        let burst = (0..1000).take_while(|_| budget.take(now)).count();
        assert_eq!(burst, NACK_BURST as usize);
        assert!(!budget.take(now));
        let later = now + Duration::from_millis(100);
        let refilled = (0..1000).take_while(|_| budget.take(later)).count();
        assert_eq!(refilled, (NACK_RATE / 10.0) as usize);
    }

    #[test]
    fn the_memory_bound_drops_the_oldest_frames() {
        let (now, mut pkt_seq) = (Instant::now(), 0);
        let mut buf = Retransmit::default();
        let big = MAX_BYTES / 3;
        for seq in 0..4 {
            buf.keep(seq, &frame(seq, big, &mut pkt_seq), now, WINDOW);
        }
        assert!(buf.bytes <= MAX_BYTES);
        assert!(!buf
            .answer(0, &[[0, 0]], &mut pkt_seq, now, WINDOW)
            .missed
            .is_empty());
        assert_eq!(
            buf.answer(3, &[[0, 0]], &mut pkt_seq, now, WINDOW)
                .resend
                .len(),
            1
        );
    }

    /// Both seats: after a step-down, a standard frame that reuses a remembered PyroWave id
    /// (frame_seq wraps) is NACKable again and gets its resend.
    #[test]
    fn a_standard_frame_reusing_a_pyrowave_id_after_a_step_down_is_resent() {
        let now = Instant::now();
        let mut pyrowave = PyroWaveFrames::default();
        for seq in 10..20 {
            pyrowave.remember(seq);
        }
        assert!(pyrowave.ignores(12), "a PyroWave frame's NACK is ignored");
        pyrowave.forget(); // the switch to the standard codec

        let mut sent = Retransmit::default();
        let mut pkt_seq = 0;
        sent.keep(12, &frame(12, 3000, &mut pkt_seq), now, WINDOW);
        assert!(
            !pyrowave.ignores(12),
            "the reused id is a standard frame now"
        );
        let answer = sent.answer(12, &[[1, 1]], &mut pkt_seq, now, WINDOW);
        assert_eq!(answer.resend.len(), 1, "its NACK gets a resend");
        assert!(answer.missed.is_empty());
    }

    #[test]
    fn only_the_last_64_pyrowave_frames_are_remembered() {
        let mut pyrowave = PyroWaveFrames::default();
        for seq in 0..65 {
            pyrowave.remember(seq);
        }
        assert!(!pyrowave.ignores(0));
        assert!(pyrowave.ignores(1) && pyrowave.ignores(64));
    }
}
