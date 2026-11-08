//! Receiver feedback into an available-rate estimate
//! (`protocol/transport-webtransport.md` §Feedback). The estimate goes to the session's
//! ABR exactly as `rtpgccbwe`'s does on WebRTC; this module is the estimator only.
//!
//! A delay- and loss-based estimator after GCC (draft-ietf-rmcat-gcc): queueing delay is
//! the one-way delay above its 10 s minimum; overuse cuts the estimate to 0.85 × the
//! measured receive rate, moderate loss scales it down, a clear path grows it 8 % a second.

use std::collections::VecDeque;
use std::time::{Duration, Instant};

use base64::Engine;
use serde_json::Value;

/// §Feedback: `count` is at most 8192; arrivals are in 50 µs units, `0xFFFF` not received.
const MAX_COUNT: usize = 8192;
const UNIT_US: i64 = 50;
const NOT_RECEIVED: u16 = 0xFFFF;

/// Send records kept, by `pkt_seq`. At 1148-byte datagrams that is about 18 MB of
/// history, over a second at 100 Mbit/s; feedback arrives within 50 ms plus a round trip.
const LOG: usize = 1 << 14;

/// The receive rate is measured over this much of the client's arrival clock, and loss
/// over this much of the agent's.
const RATE_WINDOW_US: u64 = 500_000;
const LOSS_WINDOW: Duration = Duration::from_millis(500);
/// A rate measured over less than this is noise.
const MIN_RATE_SPAN_US: u64 = 100_000;
/// The base delay is the minimum over this many one-second buckets.
const BASE_BUCKETS: usize = 10;

/// Queueing delay above which the path is overused, and below which it is clear.
const OVERUSE_US: i64 = 20_000;
const CLEAR_US: i64 = 8_000;
/// Loss fractions: above `HIGH` is overuse, between `LOW` and `HIGH` scales the estimate.
const LOSS_LOW: f64 = 0.02;
const LOSS_HIGH: f64 = 0.10;
/// GCC's β (the share of the receive rate kept on overuse) and η (growth per second).
const BETA: f64 = 0.85;
const GROWTH_PER_S: f64 = 1.08;
/// No estimate below this: a floor the governor's own floor always sits above.
const MIN_KBPS: f64 = 150.0;
/// The ABR is fed at most this often, about as often as `rtpgccbwe` notifies.
pub(crate) const REPORT_EVERY: Duration = Duration::from_millis(100);

/// One `feedback` message, validated.
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct Feedback {
    pub(crate) first_pkt_seq: u32,
    pub(crate) ref_us: u64,
    pub(crate) arrivals: Vec<u16>,
}

/// `None` for a message the agent cannot use: a field missing or out of range, or
/// `arrivals_b64` not exactly `count` u16 values.
pub(crate) fn feedback_of(message: &Value) -> Option<Feedback> {
    let first_pkt_seq = u32::try_from(message.get("first_pkt_seq")?.as_u64()?).ok()?;
    let count = usize::try_from(message.get("count")?.as_u64()?).ok()?;
    let ref_us = message.get("ref_us")?.as_u64()?;
    if count == 0 || count > MAX_COUNT {
        return None;
    }
    let bytes = base64::engine::general_purpose::STANDARD
        .decode(message.get("arrivals_b64")?.as_str()?)
        .ok()?;
    if bytes.len() != count * 2 {
        return None;
    }
    let arrivals = bytes
        .chunks_exact(2)
        .map(|b| u16::from_le_bytes([b[0], b[1]]))
        .collect();
    Some(Feedback {
        first_pkt_seq,
        ref_us,
        arrivals,
    })
}

/// When, and how big, each recent agent → client datagram was sent, by its `pkt_seq`.
pub(crate) struct SendLog {
    start: Instant,
    /// (`pkt_seq`, µs after `start`, bytes); `bytes == 0` is an empty slot.
    slots: Vec<(u32, u64, u16)>,
}

impl SendLog {
    pub(crate) fn new(start: Instant) -> Self {
        Self {
            start,
            slots: vec![(0, 0, 0); LOG],
        }
    }

    /// Any channel's datagram: `pkt_seq` is bytes 1..5 of every one (§Channels).
    pub(crate) fn record(&mut self, datagram: &[u8], at: Instant) {
        let Some(seq) = datagram.get(1..5) else {
            return;
        };
        let seq = u32::from_le_bytes(seq.try_into().unwrap());
        let us = at.saturating_duration_since(self.start).as_micros() as u64;
        let bytes = datagram.len().min(u16::MAX as usize) as u16;
        self.slots[seq as usize % LOG] = (seq, us, bytes);
    }

    fn get(&self, seq: u32) -> Option<(u64, u16)> {
        match self.slots[seq as usize % LOG] {
            (s, us, bytes) if s == seq && bytes > 0 => Some((us, bytes)),
            _ => None,
        }
    }
}

/// The rate estimate one client's feedback drives, in kbit/s.
pub(crate) struct Estimator {
    estimate_kbps: f64,
    ceiling_kbps: f64,
    /// Received datagrams: (client arrival µs, bytes), oldest first.
    arrivals: VecDeque<(u64, u32)>,
    /// Per message: (agent instant, reported, lost).
    losses: VecDeque<(Instant, u32, u32)>,
    /// Per one-second bucket of the agent's clock: (bucket start, minimum one-way delay).
    base: VecDeque<(Instant, i64)>,
    last_update: Instant,
    last_decrease: Option<Instant>,
    last_report: Option<Instant>,
}

/// What one message measured, for the log and the tests.
#[derive(Debug, Clone, Copy, PartialEq)]
pub(crate) struct Measured {
    pub(crate) queue_us: Option<i64>,
    pub(crate) rate_kbps: Option<f64>,
    pub(crate) loss: f64,
}

impl Estimator {
    /// Starts at the ceiling, as `rtpgccbwe` is seeded: it only walks down on evidence.
    pub(crate) fn new(ceiling_kbps: f64, now: Instant) -> Self {
        Self {
            estimate_kbps: ceiling_kbps,
            ceiling_kbps,
            arrivals: VecDeque::new(),
            losses: VecDeque::new(),
            base: VecDeque::new(),
            last_update: now,
            last_decrease: None,
            last_report: None,
        }
    }

    #[cfg(test)]
    pub(crate) fn estimate_kbps(&self) -> f64 {
        self.estimate_kbps
    }

    /// Take one message; `Some(estimate)` when it is time to feed the ABR
    /// ([`REPORT_EVERY`]). `rtt` spaces two cuts: the first one's effect is not visible
    /// to the client before a round trip.
    pub(crate) fn on_feedback(
        &mut self,
        fb: &Feedback,
        log: &SendLog,
        now: Instant,
        rtt: Duration,
    ) -> Option<f64> {
        let m = self.measure(fb, log, now);
        let dt = now
            .saturating_duration_since(self.last_update)
            .as_secs_f64()
            .min(1.0);
        self.last_update = now;
        let queue = m.queue_us.unwrap_or(0);
        if queue > OVERUSE_US || m.loss > LOSS_HIGH {
            let spaced = self
                .last_decrease
                .is_none_or(|at| now.saturating_duration_since(at) > rtt + REPORT_EVERY * 2);
            if let (true, Some(rate)) = (spaced, m.rate_kbps) {
                if BETA * rate < self.estimate_kbps {
                    tracing::debug!(
                        token = "webtransport-cc-overuse",
                        queue_us = queue,
                        loss = m.loss,
                        "WebTransport path overused: estimate {:.0} → {:.0} kbps (receive rate {rate:.0})",
                        self.estimate_kbps,
                        BETA * rate
                    );
                    self.estimate_kbps = BETA * rate;
                }
                self.last_decrease = Some(now);
            }
        } else if m.loss > LOSS_LOW {
            self.estimate_kbps *= (1.0 - 0.5 * m.loss).powf(dt);
        } else if queue < CLEAR_US {
            self.estimate_kbps *= GROWTH_PER_S.powf(dt);
        }
        self.estimate_kbps = self.estimate_kbps.clamp(MIN_KBPS, self.ceiling_kbps);
        let due = self
            .last_report
            .is_none_or(|at| now.saturating_duration_since(at) >= REPORT_EVERY);
        due.then(|| {
            self.last_report = Some(now);
            self.estimate_kbps
        })
    }

    /// The message's queueing delay (its least-delayed datagram, so one late datagram is
    /// not a queue), and the windowed receive rate and loss.
    pub(crate) fn measure(&mut self, fb: &Feedback, log: &SendLog, now: Instant) -> Measured {
        let (mut lost, mut reported) = (0u32, 0u32);
        let mut delays = Vec::new();
        for (i, &arrival) in fb.arrivals.iter().enumerate() {
            let seq = fb.first_pkt_seq.wrapping_add(i as u32);
            let Some((sent_us, bytes)) = log.get(seq) else {
                continue;
            };
            reported += 1;
            if arrival == NOT_RECEIVED {
                lost += 1;
                continue;
            }
            let at_us = fb.ref_us + arrival as u64 * UNIT_US as u64;
            delays.push(at_us as i64 - sent_us as i64);
            self.arrivals.push_back((at_us, bytes as u32));
        }

        if let Some(&min) = delays.iter().min() {
            match self.base.back_mut() {
                Some((start, low))
                    if now.saturating_duration_since(*start) < Duration::from_secs(1) =>
                {
                    *low = (*low).min(min)
                }
                _ => {
                    self.base.push_back((now, min));
                    if self.base.len() > BASE_BUCKETS {
                        self.base.pop_front();
                    }
                }
            }
        }
        let base = self.base.iter().map(|&(_, d)| d).min();
        let queue_us = delays.iter().min().zip(base).map(|(d, b)| d - b);

        // Arrivals come in order of `pkt_seq`, not of time; trim by the newest seen.
        let newest = self.arrivals.iter().map(|&(t, _)| t).max().unwrap_or(0);
        self.arrivals.retain(|&(t, _)| t + RATE_WINDOW_US >= newest);
        let oldest = self
            .arrivals
            .iter()
            .map(|&(t, _)| t)
            .min()
            .unwrap_or(newest);
        let span = newest - oldest;
        let rate_kbps = (span >= MIN_RATE_SPAN_US).then(|| {
            let bits: u64 = self.arrivals.iter().map(|&(_, b)| b as u64 * 8).sum();
            bits as f64 * 1000.0 / span as f64
        });

        self.losses.push_back((now, reported, lost));
        while self
            .losses
            .front()
            .is_some_and(|&(at, _, _)| now.saturating_duration_since(at) > LOSS_WINDOW)
        {
            self.losses.pop_front();
        }
        let (r, l) = self
            .losses
            .iter()
            .fold((0u32, 0u32), |(r, l), &(_, a, b)| (r + a, l + b));
        let loss = if r == 0 { 0.0 } else { l as f64 / r as f64 };

        Measured {
            queue_us,
            rate_kbps,
            loss,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A one-hop path for the estimator: datagrams sent at a constant rate into a
    /// bottleneck of `cap_kbps` with a drop-tail queue of `queue_bytes`, reported by a
    /// client as the contract says. Time is simulated.
    struct Path {
        start: Instant,
        log: SendLog,
        est: Estimator,
        seq: u32,
        /// The simulated clock, µs after `start`.
        clock: u64,
        /// When the bottleneck is next free, µs.
        free_at: u64,
        /// (seq, arrival µs, or `None` dropped) not yet reported.
        pending: Vec<(u32, Option<u64>)>,
        first: u32,
        cap_kbps: f64,
        queue_bytes: u64,
        reported: Vec<f64>,
    }

    const SIZE: usize = 1148;

    impl Path {
        fn new(ceiling: f64, cap_kbps: f64, queue_bytes: u64) -> Self {
            Self::starting_at(ceiling, cap_kbps, queue_bytes, 0)
        }

        fn starting_at(ceiling: f64, cap_kbps: f64, queue_bytes: u64, seq: u32) -> Self {
            let start = Instant::now();
            Self {
                start,
                log: SendLog::new(start),
                est: Estimator::new(ceiling, start),
                seq,
                clock: 0,
                free_at: 0,
                pending: Vec::new(),
                first: seq,
                cap_kbps,
                queue_bytes,
                reported: Vec::new(),
            }
        }

        /// Send at `send_kbps` for `secs`, with feedback every 50 ms.
        fn run(&mut self, send_kbps: f64, secs: f64) {
            let gap_us = SIZE as f64 * 8.0 * 1000.0 / send_kbps;
            let mut t = self.clock;
            let end = t + (secs * 1e6) as u64;
            let mut next_feedback = t + 50_000;
            let service_us = SIZE as f64 * 8.0 * 1000.0 / self.cap_kbps;
            let one_way = 10_000u64;
            while t < end {
                let mut d = vec![0u8; SIZE];
                d[1..5].copy_from_slice(&self.seq.to_le_bytes());
                self.log.record(&d, self.start + Duration::from_micros(t));
                let backlog = self.free_at.saturating_sub(t) as f64 / service_us * SIZE as f64;
                let arrival = (backlog as u64 + SIZE as u64 <= self.queue_bytes).then(|| {
                    self.free_at = self.free_at.max(t) + service_us as u64;
                    self.free_at + one_way
                });
                self.pending.push((self.seq, arrival));
                self.seq = self.seq.wrapping_add(1);
                t += gap_us as u64;
                if t >= next_feedback {
                    self.feedback(next_feedback);
                    next_feedback += 50_000;
                }
            }
            self.clock = t;
        }

        /// What arrived by `at_us` (client clock = agent clock + 1 s, any offset works).
        fn feedback(&mut self, at_us: u64) {
            let arrived = self
                .pending
                .iter()
                .rposition(|&(_, a)| a.is_some_and(|a| a <= at_us));
            let Some(last) = arrived else { return };
            let batch: Vec<_> = self.pending.drain(..=last).collect();
            let offset = 1_000_000u64;
            let ref_us = batch.iter().filter_map(|&(_, a)| a).min().unwrap() + offset;
            let arrivals = batch
                .iter()
                .map(|&(_, a)| match a {
                    Some(a) if a <= at_us => ((a + offset - ref_us) / 50) as u16,
                    _ => NOT_RECEIVED,
                })
                .collect();
            let fb = Feedback {
                first_pkt_seq: self.first,
                ref_us,
                arrivals,
            };
            self.first = self.first.wrapping_add(batch.len() as u32);
            let now = self.start + Duration::from_micros(at_us);
            if let Some(k) = self
                .est
                .on_feedback(&fb, &self.log, now, Duration::from_millis(20))
            {
                self.reported.push(k);
            }
        }
    }

    #[test]
    fn a_path_with_room_keeps_the_estimate_at_the_ceiling() {
        let mut p = Path::new(8000.0, 20_000.0, 64 * 1024);
        p.run(6000.0, 5.0);
        assert_eq!(p.est.estimate_kbps(), 8000.0);
        assert!(!p.reported.is_empty());
    }

    #[test]
    fn a_rate_cap_cuts_the_estimate_to_below_the_cap() {
        let mut p = Path::new(8000.0, 3000.0, 256 * 1024);
        p.run(6000.0, 3.0);
        let est = p.est.estimate_kbps();
        assert!(
            est < 3000.0 && est > 2000.0,
            "β × the 3000 kbps receive rate, got {est:.0}"
        );
    }

    #[test]
    fn a_shallow_queue_that_only_drops_is_read_from_the_loss() {
        // A two-datagram queue: no delay to see, only loss.
        let mut p = Path::new(8000.0, 3000.0, 2 * SIZE as u64);
        p.run(6000.0, 3.0);
        let est = p.est.estimate_kbps();
        assert!(est < 3000.0, "loss over 10% cuts to β × rate, got {est:.0}");
    }

    #[test]
    fn the_estimate_recovers_when_the_cap_lifts() {
        let mut p = Path::new(8000.0, 3000.0, 256 * 1024);
        p.run(6000.0, 3.0);
        let low = p.est.estimate_kbps();
        p.cap_kbps = 50_000.0;
        // Sending under the estimate, as the encoder would at the lowered setpoint. The
        // queue built under the cap drains first, then the estimate grows 8 % a second.
        p.run(2000.0, 8.0);
        assert!(
            p.est.estimate_kbps() > low * 1.2,
            "{low:.0} → {:.0}",
            p.est.estimate_kbps()
        );
    }

    #[test]
    fn moderate_loss_scales_the_estimate_down_gradually() {
        let start = Instant::now();
        let mut log = SendLog::new(start);
        let mut est = Estimator::new(8000.0, start);
        // 20 datagrams per message, one lost: 5 % loss, no queue.
        for m in 0..20u32 {
            let mut arrivals = Vec::new();
            for i in 0..20u32 {
                let seq = m * 20 + i;
                let mut d = vec![0u8; SIZE];
                d[1..5].copy_from_slice(&seq.to_le_bytes());
                let sent =
                    Duration::from_millis(m as u64 * 50) + Duration::from_micros(i as u64 * 2500);
                log.record(&d, start + sent);
                arrivals.push(if i == 7 {
                    NOT_RECEIVED
                } else {
                    (i * 50) as u16
                });
            }
            let fb = Feedback {
                first_pkt_seq: m * 20,
                ref_us: 5_000_000 + m as u64 * 50_000,
                arrivals,
            };
            est.on_feedback(
                &fb,
                &log,
                start + Duration::from_millis((m as u64 + 1) * 50),
                Duration::from_millis(20),
            );
        }
        let e = est.estimate_kbps();
        // (1 − 0.5 × 0.05) a second, for one second.
        assert!(e < 7900.0 && e > 7700.0, "got {e:.0}");
    }

    #[test]
    fn sequence_numbers_wrap() {
        let mut p = Path::starting_at(8000.0, 3000.0, 256 * 1024, u32::MAX - 100);
        p.run(6000.0, 3.0);
        assert!(p.est.estimate_kbps() < 3000.0);
    }

    #[test]
    fn datagrams_the_agent_never_logged_count_for_nothing() {
        let start = Instant::now();
        let log = SendLog::new(start);
        let mut est = Estimator::new(8000.0, start);
        let fb = Feedback {
            first_pkt_seq: 0,
            ref_us: 0,
            arrivals: vec![NOT_RECEIVED; 100],
        };
        let m = est.measure(&fb, &log, start);
        assert_eq!(m.loss, 0.0);
        assert_eq!(m.queue_us, None);
    }

    #[test]
    fn the_abr_is_fed_at_most_every_100_ms() {
        let start = Instant::now();
        let log = SendLog::new(start);
        let mut est = Estimator::new(8000.0, start);
        let fb = Feedback {
            first_pkt_seq: 0,
            ref_us: 0,
            arrivals: vec![0],
        };
        let rtt = Duration::from_millis(20);
        let fed: Vec<bool> = (0..6)
            .map(|i| {
                est.on_feedback(&fb, &log, start + Duration::from_millis(i * 50), rtt)
                    .is_some()
            })
            .collect();
        assert_eq!(fed, [true, false, true, false, true, false]);
    }

    #[test]
    fn a_message_out_of_contract_is_refused() {
        let b64 = |n: usize| base64::engine::general_purpose::STANDARD.encode(vec![0u8; n]);
        let ok = serde_json::json!({"type": "feedback", "seq": 1, "first_pkt_seq": 7, "count": 2,
            "ref_us": 9, "arrivals_b64": b64(4), "recv_kbps": 1, "lost": 0, "interval_ms": 50});
        assert_eq!(
            feedback_of(&ok),
            Some(Feedback {
                first_pkt_seq: 7,
                ref_us: 9,
                arrivals: vec![0, 0]
            })
        );
        for (field, value) in [
            ("count", serde_json::json!(3)),
            ("count", serde_json::json!(0)),
            ("count", serde_json::json!(8193)),
            ("first_pkt_seq", serde_json::json!(1u64 << 32)),
            ("arrivals_b64", serde_json::json!("not base64!")),
            ("ref_us", serde_json::json!(-1)),
        ] {
            let mut bad = ok.clone();
            bad[field] = value.clone();
            assert_eq!(feedback_of(&bad), None, "{field} = {value}");
        }
    }
}
