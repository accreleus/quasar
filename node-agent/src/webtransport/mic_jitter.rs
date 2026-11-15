//! The WebTransport microphone's jitter buffer (`protocol/transport-webtransport.md`
//! §Microphone). Datagrams reorder and bunch up on a mobile link; the Opus decoder needs
//! them in order and the sink needs them evenly. Packets wait here until `target` has passed
//! since the first one, then leave one per 10 ms tick in `packet_seq` order. A packet that
//! is still missing when later ones are queued is reported lost, once. The depth is
//! `QUASAR_MIC_JITTER_MS`, the same knob as the WebRTC microphone's buffer.

use std::time::{Duration, Instant};

use bytes::Bytes;

/// Packets held at most (one second of audio): a client further ahead than this has restarted.
const MAX_QUEUED: usize = 100;
/// Ticks with nothing queued before the buffer forgets its position (a muted microphone, or
/// a client that restarted its numbering): the next packet starts it afresh.
const STARVED_TICKS: u32 = 5;
/// The shortest depth that still reorders one packet.
const MIN_TARGET: Duration = Duration::from_millis(20);

/// What one 10 ms tick takes out of the buffer.
#[derive(Debug, PartialEq)]
pub(crate) enum Out {
    /// The next packet; `lost` packets before it never arrived.
    Packet(Bytes),
    /// The next packet is lost: the decoder conceals 10 ms.
    Lost,
    /// Nothing to play yet, or no more.
    Wait,
}

/// `seq` is not before `next` in serial-number arithmetic.
fn not_before(seq: u16, next: u16) -> bool {
    seq.wrapping_sub(next) < 0x8000
}

pub(crate) struct Jitter {
    target: Duration,
    /// Sorted by `packet_seq` from `next`.
    queue: Vec<(u16, Bytes)>,
    /// The packet the next tick wants; `None` before the first of a run.
    next: Option<u16>,
    first_at: Option<Instant>,
    primed: bool,
    starved: u32,
}

impl Jitter {
    pub(crate) fn new(target_ms: u32) -> Self {
        Self {
            target: Duration::from_millis(u64::from(target_ms)).max(MIN_TARGET),
            queue: Vec::new(),
            next: None,
            first_at: None,
            primed: false,
            starved: 0,
        }
    }

    /// A new client: its numbering has nothing to do with the last one's.
    pub(crate) fn reset(&mut self) {
        self.queue.clear();
        self.next = None;
        self.first_at = None;
        self.primed = false;
        self.starved = 0;
    }

    /// `false` when the packet was dropped: a duplicate, or already played or given up on.
    pub(crate) fn push(&mut self, seq: u16, data: Bytes, now: Instant) -> bool {
        let next = match self.next {
            Some(next) => next,
            None => {
                self.next = Some(seq);
                self.first_at = Some(now);
                seq
            }
        };
        let ahead = usize::from(seq.wrapping_sub(next));
        if !not_before(seq, next) || self.queue.iter().any(|(s, _)| *s == seq) {
            return false;
        }
        if ahead > MAX_QUEUED {
            // Not a reorder: the client's numbering jumped. Start again from here.
            self.reset();
            return self.push(seq, data, now);
        }
        let at = self
            .queue
            .partition_point(|(s, _)| s.wrapping_sub(next) < seq.wrapping_sub(next));
        self.queue.insert(at, (seq, data));
        true
    }

    /// Called every 10 ms.
    pub(crate) fn tick(&mut self, now: Instant) -> Out {
        let Some(next) = self.next else {
            return Out::Wait;
        };
        if !self.primed {
            let waited = self
                .first_at
                .is_some_and(|at| now.duration_since(at) >= self.target);
            if !waited || self.queue.is_empty() {
                return Out::Wait;
            }
            self.primed = true;
        }
        if self.queue.is_empty() {
            self.starved += 1;
            if self.starved >= STARVED_TICKS {
                self.reset();
            }
            return Out::Wait;
        }
        self.starved = 0;
        self.next = Some(next.wrapping_add(1));
        if self.queue[0].0 == next {
            Out::Packet(self.queue.remove(0).1)
        } else {
            Out::Lost
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn p(n: u8) -> Bytes {
        Bytes::from(vec![n])
    }

    /// Run ticks of 10 ms from `start`, `n` of them, returning what each gave.
    fn ticks(j: &mut Jitter, start: Instant, from: u32, n: u32) -> Vec<Out> {
        (from..from + n)
            .map(|i| j.tick(start + Duration::from_millis(u64::from(i) * 10)))
            .collect()
    }

    #[test]
    fn nothing_leaves_until_the_depth_has_passed_then_one_packet_per_tick_in_order() {
        let t0 = Instant::now();
        let mut j = Jitter::new(60);
        // Reordered on the way in: 2 before 1.
        for (i, seq) in [0u16, 2, 1, 3].into_iter().enumerate() {
            assert!(j.push(seq, p(seq as u8), t0 + Duration::from_millis(i as u64 * 3)));
        }
        let early = ticks(&mut j, t0, 0, 6);
        assert!(
            early.iter().all(|o| *o == Out::Wait),
            "primed only after 60 ms: {early:?}"
        );
        let out = ticks(&mut j, t0, 6, 4);
        assert_eq!(
            out,
            [
                Out::Packet(p(0)),
                Out::Packet(p(1)),
                Out::Packet(p(2)),
                Out::Packet(p(3))
            ]
        );
    }

    #[test]
    fn a_duplicate_and_a_packet_already_played_are_dropped() {
        let t0 = Instant::now();
        let mut j = Jitter::new(20);
        assert!(j.push(5, p(5), t0));
        assert!(!j.push(5, p(5), t0), "a duplicate");
        ticks(&mut j, t0, 2, 1);
        assert!(!j.push(4, p(4), t0), "behind the first packet of the run");
        assert!(j.push(6, p(6), t0));
        ticks(&mut j, t0, 3, 2);
        assert!(!j.push(5, p(5), t0), "played");
    }

    #[test]
    fn a_lost_packet_is_reported_once_and_the_ones_behind_it_play_on_time() {
        let t0 = Instant::now();
        let mut j = Jitter::new(20);
        for seq in [0u16, 1, 3, 4] {
            j.push(seq, p(seq as u8), t0);
        }
        let out = ticks(&mut j, t0, 2, 4);
        assert_eq!(
            out,
            [
                Out::Packet(p(0)),
                Out::Packet(p(1)),
                Out::Lost,
                Out::Packet(p(3))
            ]
        );
        // 2 turns up late: it was given up on.
        assert!(!j.push(2, p(2), t0));
    }

    #[test]
    fn the_numbering_wraps() {
        let t0 = Instant::now();
        let mut j = Jitter::new(20);
        for seq in [0xfffeu16, 0xffff, 0, 1] {
            j.push(seq, p(seq as u8), t0);
        }
        let out = ticks(&mut j, t0, 2, 4);
        assert_eq!(
            out,
            [
                Out::Packet(p(0xfe)),
                Out::Packet(p(0xff)),
                Out::Packet(p(0)),
                Out::Packet(p(1))
            ]
        );
    }

    #[test]
    fn a_new_run_after_silence_or_a_reset_starts_from_its_own_first_packet() {
        let t0 = Instant::now();
        let mut j = Jitter::new(20);
        for seq in 100u16..110 {
            j.push(seq, p(1), t0);
        }
        ticks(&mut j, t0, 2, 10);
        // Muted: nothing for a while, so the buffer lets go of its place.
        ticks(&mut j, t0, 12, 6);
        assert!(
            j.push(0, p(7), t0 + Duration::from_secs(1)),
            "a client numbering from 0"
        );
        let out = ticks(&mut j, t0 + Duration::from_secs(1), 2, 1);
        assert_eq!(out, [Out::Packet(p(7))]);
        // A new client straight away, before any silence.
        j.push(1, p(1), t0 + Duration::from_secs(2));
        j.reset();
        assert!(j.push(0, p(9), t0 + Duration::from_secs(2)));
    }

    #[test]
    fn the_deepest_accepted_depth_primes_and_plays_before_the_queue_limit() {
        let t0 = Instant::now();
        let depth = crate::session::MIC_JITTER_MAX_MS;
        assert!(
            (depth as usize / 10) * 2 <= MAX_QUEUED,
            "the cap leaves the queue room to spare"
        );
        let mut j = Jitter::new(depth);
        // A client sending one packet per 10 ms through the whole wait.
        let sent = depth / 10;
        for i in 0..sent {
            assert!(j.push(
                i as u16,
                p(1),
                t0 + Duration::from_millis(u64::from(i) * 10)
            ));
        }
        assert_eq!(
            j.tick(t0 + Duration::from_millis(u64::from(depth))),
            Out::Packet(p(1)),
            "primed, and nothing was thrown away"
        );
        assert_eq!(j.queue.len(), sent as usize - 1);
    }

    #[test]
    fn a_depth_past_the_queue_limit_never_primes_which_is_why_the_knob_is_capped() {
        let t0 = Instant::now();
        let mut j = Jitter::new(2_000);
        // 150 packets in 1.5 s: the 102nd is "far ahead" of the one nothing has played yet.
        for i in 0..150u16 {
            j.push(i, p(1), t0 + Duration::from_millis(u64::from(i) * 10));
        }
        assert_eq!(j.tick(t0 + Duration::from_millis(1_500)), Out::Wait);
    }

    #[test]
    fn a_number_far_ahead_is_a_restart_not_a_wait() {
        let t0 = Instant::now();
        let mut j = Jitter::new(20);
        j.push(10, p(10), t0);
        assert!(j.push(10 + 5_000, p(1), t0));
        let out = ticks(&mut j, t0, 2, 1);
        assert_eq!(out, [Out::Packet(p(1))]);
    }
}
