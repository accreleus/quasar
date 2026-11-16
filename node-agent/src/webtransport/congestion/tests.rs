use std::collections::VecDeque;

use super::*;
use crate::webtransport::tests::Captured;

const SIZE: usize = 1148;
const CEILING: f64 = 8000.0;
const RTT: Duration = Duration::from_millis(20);
const NO_QUIC: QuicPath = QuicPath {
    rtt: RTT,
    cwnd_bytes: 0,
    congestion_events: 0,
    queued_bytes: 0,
};

/// A one-hop path for the estimator, simulated: 60 fps frames sent as bursts into a
/// bottleneck (`cap_kbps`, drop-tail `queue_bytes`), plus a one-way delay that may change,
/// reported by a client whose clock has an offset and drifts. With `send_kbps` unset the
/// sender follows the estimate, as the encoder follows the ABR.
struct Sim {
    start: Instant,
    log: SendLog,
    est: Estimator,
    /// The agent's clock, µs after `start`.
    t: u64,
    seq: u32,
    free_at: u64,
    last_arrival: u64,
    cap_kbps: f64,
    queue_bytes: f64,
    send_kbps: Option<f64>,
    /// The most the content gives the encoder at a send time, kbit/s: a still screen sends
    /// little whatever the estimate.
    content_kbps: Box<dyn Fn(u64) -> f64>,
    /// Extra one-way delay at a send time, µs.
    extra_us: Box<dyn Fn(u64) -> u64>,
    offset_us: u64,
    drift: f64,
    /// Extra one-way delay of each `pkt_seq` on its own, µs: a path that reorders.
    reorder_us: Box<dyn Fn(u32) -> u64>,
    /// The client reports a gap only once a later datagram has been in this long (µs), and
    /// counts a datagram as received when it comes within it: the browser's reorder window.
    hold_us: u64,
    /// Every `n`th datagram is dropped on the way (0: none).
    drop_every: u32,
    /// Datagrams the full bottleneck queue dropped.
    queue_drops: u32,
    /// (`pkt_seq`, arrival on the agent's clock, or dropped), in send order.
    pending: VecDeque<(u32, Option<u64>)>,
    /// (seconds, estimate) after each message.
    trace: Vec<(f64, f64)>,
    /// Video goes through the agent's queue and QUIC's governed window, not straight out.
    quic: Option<Quic>,
    /// The most video the queue held, bytes, and when, seconds.
    held_max: (usize, f64),
    /// Every `n`th frame is a keyframe of five frames' bytes (0: none).
    keyframe_every: u32,
    frames: u32,
    /// (seconds, µs a datagram entering the bottleneck now would wait), each queued-mode tick.
    waits: Vec<(f64, u64)>,
}

/// The agent's [`VideoQueue`] and QUIC behind it: a datagram leaves QUIC's queue once the
/// governed window (`Window`: 2 × estimate × max(sRTT, 100 ms)) has room for it.
#[derive(Default)]
struct Quic {
    video: VideoQueue,
    queue: VecDeque<bytes::Bytes>,
    queued: usize,
    /// (sent µs, acknowledged or declared lost µs, bytes, arrived).
    flight: VecDeque<(u64, u64, usize, bool)>,
    in_flight: usize,
    srtt_us: f64,
}

impl Sim {
    fn new(cap_kbps: f64) -> Self {
        Self::at_seq(cap_kbps, 0)
    }

    fn at_seq(cap_kbps: f64, seq: u32) -> Self {
        let start = Instant::now();
        Self {
            start,
            log: SendLog::new(start),
            est: Estimator::new(CEILING, start),
            t: 0,
            seq,
            free_at: 0,
            last_arrival: 0,
            cap_kbps,
            queue_bytes: 256.0 * 1024.0,
            send_kbps: None,
            content_kbps: Box::new(|_| f64::MAX),
            extra_us: Box::new(|_| 0),
            offset_us: 0,
            drift: 0.0,
            reorder_us: Box::new(|_| 0),
            hold_us: 0,
            drop_every: 0,
            queue_drops: 0,
            pending: VecDeque::new(),
            trace: Vec::new(),
            quic: None,
            held_max: (0, 0.0),
            keyframe_every: 0,
            frames: 0,
            waits: Vec::new(),
        }
    }

    fn run(&mut self, secs: f64) {
        let end = self.t + (secs * 1e6) as u64;
        let mut next_frame = self.t;
        let mut next_feedback = self.t + 50_000;
        let mut next_tick = self.t + 1000;
        while self.t < end {
            if self.quic.is_some() && next_tick < next_frame.min(next_feedback) {
                self.t = next_tick;
                self.drain();
                next_tick += 1000;
            } else if next_frame <= next_feedback {
                self.t = next_frame;
                self.frame();
                next_frame += 16_667;
            } else {
                self.t = next_feedback;
                self.feedback();
                next_feedback += 50_000;
            }
        }
    }

    /// `secs` of the agent sending and nothing coming back, as an outage looks to the
    /// estimator: every frame still goes out, no feedback arrives, and what was sent is lost
    /// with the path rather than reported late.
    fn outage(&mut self, secs: f64) {
        let from = self.t;
        let first = self.seq;
        for step in 1..=(secs * 10.0) as u64 {
            self.t = from + step * 100_000;
            self.frame();
            let at = self.start + Duration::from_micros(self.t);
            self.est.on_tick(at, &self.log, RTT);
        }
        // The packets sent during the outage stay pending as lost, as a real client's first
        // report after it names them NOT_RECEIVED (#1557). Earlier packets keep their arrival.
        for (seq, arrival) in self.pending.iter_mut() {
            if *seq >= first {
                *arrival = None;
            }
        }
    }

    fn frame(&mut self) {
        let kbps = self
            .send_kbps
            .unwrap_or(self.est.estimate_kbps())
            .min((self.content_kbps)(self.t));
        let key = self.keyframe_every > 0 && self.frames.is_multiple_of(self.keyframe_every);
        self.frames += 1;
        let bytes = kbps * 1000.0 / 8.0 / 60.0 * if key { 5.0 } else { 1.0 };
        let n = (bytes / SIZE as f64).ceil().max(1.0) as u32;
        if let Some(quic) = self.quic.as_mut() {
            let queued_at = self.start + Duration::from_micros(self.t);
            for _ in 0..n {
                let datagram = video_datagram(quic.video.next_seq());
                let held = Held {
                    datagram,
                    logged: true,
                    resend: false,
                    queued_at,
                };
                quic.video.push(held, BUFFER);
            }
            self.drain();
            return;
        }
        for _ in 0..n {
            let mut d = vec![0u8; SIZE];
            d[1..5].copy_from_slice(&self.seq.to_le_bytes());
            self.log
                .record(&d, self.start + Duration::from_micros(self.t), false);
            self.transmit(self.seq);
            self.seq = self.seq.wrapping_add(1);
        }
    }

    /// One datagram onto the path now; its arrival on the agent's clock, or `None` dropped.
    fn transmit(&mut self, seq: u32) -> Option<u64> {
        let service = (SIZE as f64 * 8.0 * 1000.0 / self.cap_kbps) as u64;
        let backlog = self.free_at.saturating_sub(self.t) as f64 / service as f64 * SIZE as f64;
        let dropped = self.drop_every > 0 && seq.is_multiple_of(self.drop_every);
        let full = backlog + SIZE as f64 > self.queue_bytes;
        self.queue_drops += u32::from(full);
        let arrival = (!dropped && !full).then(|| {
            self.free_at = self.free_at.max(self.t) + service;
            // One path, first in first out: a delay spike holds back what follows it.
            self.last_arrival = self
                .last_arrival
                .max(self.free_at + 10_000 + (self.extra_us)(self.t));
            self.last_arrival + (self.reorder_us)(seq)
        });
        self.pending.push_back((seq, arrival));
        arrival
    }

    /// QUIC sends what its window allows, the queue hands it what fits in the room the
    /// production `feed` gives it, and QUIC sends again.
    fn drain(&mut self) {
        let mut quic = self.quic.take().expect("queued mode");
        let est = self.est.estimate_kbps();
        self.send_window(&mut quic, est);
        let room = quic
            .video
            .room(Some(est as u64), BUFFER)
            .saturating_sub(quic.queued);
        // As the production `feed`: paced once the estimate has found the link.
        let paced = Some(est as u64).filter(|_| self.est.found_link());
        let at = self.start + Duration::from_micros(self.t);
        let room = room.min(quic.video.pace(paced, at));
        let (queue, queued) = (&mut quic.queue, &mut quic.queued);
        quic.video.feed(room, Some(&mut self.log), |held| {
            *queued += held.datagram.len();
            queue.push_back(held.datagram);
            Handed::Sent
        });
        if quic.video.bytes() > self.held_max.0 {
            self.held_max = (quic.video.bytes(), self.t as f64 / 1e6);
        }
        self.send_window(&mut quic, est);
        self.waits
            .push((self.t as f64 / 1e6, self.free_at.saturating_sub(self.t)));
        self.quic = Some(quic);
    }

    fn send_window(&mut self, quic: &mut Quic, est_kbps: f64) {
        let now = self.t;
        while let Some(&(sent, done, _, arrived)) = quic.flight.front() {
            if done > now {
                break;
            }
            if arrived {
                let rtt = (done - sent) as f64;
                quic.srtt_us = if quic.srtt_us == 0.0 {
                    rtt
                } else {
                    0.875 * quic.srtt_us + 0.125 * rtt
                };
            }
            quic.in_flight -= quic.flight.pop_front().unwrap().2;
        }
        let srtt = quic.srtt_us.max(100_000.0) / 1e6;
        let window = (2.0 * est_kbps * 1000.0 / 8.0 * srtt) as usize;
        while let Some(d) = quic.queue.front() {
            if quic.in_flight + d.len() > window {
                break;
            }
            let d = quic.queue.pop_front().unwrap();
            quic.queued -= d.len();
            quic.in_flight += d.len();
            let arrival = self.transmit(wire_seq(&d));
            // The ACK returns over the same 10 ms; a loss is declared about 200 ms on.
            // Kept in completion order: arrivals are first in first out already.
            let last = quic.flight.back().map_or(0, |f| f.1);
            let done = arrival.map_or(now + 200_000, |a| a + 10_000).max(last);
            quic.flight
                .push_back((now, done, d.len(), arrival.is_some()));
        }
    }

    fn client_us(&self, agent_us: u64) -> u64 {
        self.offset_us + (agent_us as f64 * (1.0 + self.drift)) as u64
    }

    fn feedback(&mut self) {
        let now = self.t;
        let Some(mut last) = self
            .pending
            .iter()
            .rposition(|&(_, a)| a.is_some_and(|a| a <= now))
        else {
            return;
        };
        // A gap is reported only once the first datagram behind it has been in for `hold_us`.
        let mut exposed = u64::MAX;
        for i in (0..=last).rev() {
            match self.pending[i].1.filter(|&a| a <= now) {
                Some(a) => exposed = exposed.min(a),
                None if exposed.saturating_add(self.hold_us) > now => last = i.wrapping_sub(1),
                None => {}
            }
        }
        if last == usize::MAX {
            return;
        }
        let batch: Vec<_> = self
            .pending
            .drain(..=last)
            .map(|(s, a)| (s, a.filter(|&a| a <= now)))
            .collect();
        let ref_us = batch
            .iter()
            .filter_map(|&(_, a)| a)
            .map(|a| self.client_us(a))
            .min()
            .unwrap_or(self.client_us(now));
        let arrivals = batch
            .iter()
            .map(|&(_, a)| match a {
                Some(a) => ((self.client_us(a) - ref_us) / UNIT_US).min(0xFFFE) as u16,
                None => NOT_RECEIVED,
            })
            .collect();
        let fb = Feedback {
            first_pkt_seq: batch[0].0,
            ref_us,
            arrivals,
        };
        let at = self.start + Duration::from_micros(now);
        self.est.on_feedback(&fb, &mut self.log, at, NO_QUIC);
        self.trace
            .push((now as f64 / 1e6, self.est.estimate_kbps()));
    }

    fn min_after(&self, secs: f64) -> f64 {
        self.trace
            .iter()
            .filter(|&&(t, _)| t >= secs)
            .map(|&(_, e)| e)
            .fold(f64::MAX, f64::min)
    }

    fn mean_after(&self, secs: f64) -> f64 {
        let v: Vec<f64> = self
            .trace
            .iter()
            .filter(|&&(t, _)| t >= secs)
            .map(|&(_, e)| e)
            .collect();
        v.iter().sum::<f64>() / v.len() as f64
    }
}

#[test]
fn a_path_with_room_keeps_the_estimate_at_the_ceiling() {
    let mut s = Sim::new(20_000.0);
    s.send_kbps = Some(6000.0);
    s.run(10.0);
    assert_eq!(s.min_after(0.0), CEILING);
}

#[test]
fn a_rate_cap_holds_the_estimate_around_the_cap() {
    let mut s = Sim::new(3000.0);
    s.run(20.0);
    let mean = s.mean_after(10.0);
    assert!(
        (2000.0..3300.0).contains(&mean),
        "the sender follows the estimate around a 3000 kbps cap, mean {mean:.0}"
    );
}

#[test]
fn a_shallow_queue_that_only_drops_is_read_from_the_loss() {
    let mut s = Sim::new(3000.0);
    s.queue_bytes = 2.0 * SIZE as f64;
    s.run(20.0);
    let mean = s.mean_after(10.0);
    assert!(
        (1000.0..3500.0).contains(&mean),
        "a two-datagram queue shows no delay, only loss; mean {mean:.0}"
    );
}

#[test]
fn the_estimate_recovers_when_the_cap_lifts() {
    let mut s = Sim::new(3000.0);
    s.run(10.0);
    let low = s.est.estimate_kbps();
    assert!(low < 3500.0, "capped at {low:.0}");
    s.cap_kbps = 50_000.0;
    s.run(25.0);
    assert!(
        s.est.estimate_kbps() > 7000.0,
        "{low:.0} → {:.0}",
        s.est.estimate_kbps()
    );
}

#[test]
fn a_clock_offset_and_500_ppm_drift_are_not_congestion() {
    let mut s = Sim::new(50_000.0);
    s.send_kbps = Some(6000.0);
    s.offset_us = 3_600_000_000;
    s.drift = 500e-6;
    s.run(60.0);
    assert_eq!(s.min_after(0.0), CEILING, "the estimate never moved");
}

#[test]
fn a_40_ms_route_step_settles_without_walking_to_the_floor() {
    let mut s = Sim::new(50_000.0);
    s.extra_us = Box::new(|t| if t >= 5_000_000 { 40_000 } else { 0 });
    s.run(40.0);
    let low = s.min_after(0.0);
    assert!(
        low > 0.5 * CEILING,
        "the step cost at most a cut or two; low {low:.0}"
    );
    assert!(
        s.est.estimate_kbps() > 0.9 * CEILING,
        "settled back at {:.0}",
        s.est.estimate_kbps()
    );
}

#[test]
fn a_standing_queue_from_a_competing_flow_does_not_starve_the_estimate() {
    let mut s = Sim::new(50_000.0);
    // A competing flow fills 30 ms of the bottleneck's buffer over a second, and keeps it.
    s.extra_us = Box::new(|t| (t.saturating_sub(5_000_000) * 30 / 1000).min(30_000));
    s.run(40.0);
    let low = s.min_after(0.0);
    assert!(low > 0.5 * CEILING, "low {low:.0}");
    assert!(
        s.est.estimate_kbps() > 0.9 * CEILING,
        "settled at {:.0}",
        s.est.estimate_kbps()
    );
}

#[test]
fn growth_never_passes_one_and_a_half_times_the_receive_rate() {
    let mut s = Sim::new(50_000.0);
    // Two 1148-byte datagrams a frame at 60 fps: 1102 kbit/s on the wire.
    s.send_kbps = Some(1000.0);
    s.est.delay_kbps = 1000.0;
    s.est.settle();
    s.run(30.0);
    let e = s.est.estimate_kbps();
    let wire = 2.0 * SIZE as f64 * 8.0 * 60.0 / 1000.0;
    assert!(
        e <= RECV_CAP * wire * 1.02 + RECV_CAP_ADD_KBPS,
        "an app-limited {wire:.0} kbps grew to {e:.0}"
    );
}

#[test]
fn heavy_loss_scales_the_estimate_by_one_minus_half_the_loss() {
    let mut s = Sim::new(50_000.0);
    s.send_kbps = Some(6000.0);
    s.drop_every = 5;
    // One decision: the next waits 300 ms + RTT for losses it has not counted yet.
    s.run(0.3);
    let e = s.est.estimate_kbps();
    assert!(
        (7000.0..7400.0).contains(&e),
        "20 % loss: 8000 × 0.9, got {e:.0}"
    );
}

#[test]
fn overuse_with_sparse_arrivals_still_cuts() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    est.trendline.usage = Usage::Overusing;
    log.record(&[1, 0, 0, 0, 0], start, false);
    let fb = Feedback {
        first_pkt_seq: 0,
        ref_us: 0,
        arrivals: vec![0],
    };
    est.on_feedback(&fb, &mut log, start + Duration::from_millis(50), NO_QUIC);
    assert_eq!(
        est.estimate_kbps(),
        BETA * CEILING,
        "no throughput yet: the ceiling is the base"
    );
}

#[test]
fn feedback_silence_while_media_is_sent_decays_the_estimate() {
    let mut s = Sim::new(50_000.0);
    s.send_kbps = Some(6000.0);
    s.run(2.0);
    let fed = s.est.estimate_kbps();
    let sent_until = s.t;
    // The agent keeps sending; nothing comes back.
    let mut fed_abr = Vec::new();
    let lines = logged(|| {
        for step in 1..=30u64 {
            s.t = sent_until + step * 100_000;
            s.frame();
            let at = s.start + Duration::from_micros(s.t);
            if let Some(k) = s.est.on_tick(at, &s.log, RTT) {
                fed_abr.push((step, k));
            }
        }
    });
    assert_eq!(
        fed_abr.first().map(|&(step, _)| step),
        Some(7),
        "500 ms past the first unanswered send (step 1): {fed_abr:?}"
    );
    let decays = lines
        .iter()
        .filter(|l| l.contains("cut=\"silence\""))
        .count();
    // 24 decays from step 7 to 30, a line at most once a second: steps 7, 17 and 27.
    assert_eq!(decays, 3, "{} decays: {lines:?}", fed_abr.len());
    let e = s.est.estimate_kbps();
    // Step 30 is 2.4 s past that timeout: 0.8 every 500 ms.
    let decayed = fed * 0.8f64.powf(4.8);
    assert!(
        (e - decayed).abs() < 0.05 * fed,
        "3 s of silence: {fed:.0} → {e:.0}"
    );
}

/// #1417: a 10 s outage decays the estimate to the floor; a 20 Mbit/s session then needs
/// about a minute of busy content to get it back at AIMD's 8 % a second. Regrown toward what
/// the link carried before the outage, it is back in a few seconds. Twice: the estimator on
/// its own (a sender fixed at 6 Mbit/s) and the real loop, where the encoder follows the
/// estimate and the receive-rate cap paces the climb.
#[test]
fn an_outage_costs_the_estimate_seconds_not_a_minute() {
    for send_kbps in [Some(6000.0), None] {
        let mut s = Sim::new(20_000.0);
        s.send_kbps = send_kbps;
        s.run(5.0);
        let carried = s.est.estimate_kbps();
        assert!(
            carried > 0.99 * CEILING,
            "{send_kbps:?}: the link carried {carried:.0}"
        );
        s.outage(10.0);
        let low = s.est.estimate_kbps();
        assert!(
            low < 0.05 * carried,
            "{send_kbps:?}: the outage took it to {low:.0}"
        );
        s.run(10.0);
        let e = s.est.estimate_kbps();
        assert!(
            e > 0.9 * carried,
            "{send_kbps:?}: {carried:.0} → {low:.0} → {e:.0} in 10 s of feedback"
        );
    }
}

/// #1557: the first report after a long outage names the packets sent during it lost. Those
/// losses are the outage, not congestion: they must not cut the loss bound, and the memory of
/// what the link carried before the outage must survive so the estimate can regrow fast.
#[test]
fn the_first_report_after_an_outage_counts_no_outage_loss() {
    let mut s = Sim::new(20_000.0);
    s.send_kbps = Some(6000.0);
    s.run(5.0);
    s.outage(10.0);
    assert!(s.est.recover_to.is_some(), "the outage left no memory");
    s.run(0.05);
    let loss = s.est.loss();
    assert!(
        loss.is_none_or(|l| l < LOSS_LOW),
        "the outage's own losses measured {loss:?}"
    );
    assert!(
        s.est.recover_to.is_some(),
        "the outage's own losses ended the regrow"
    );
}

/// #1557 review A2: the exemption ends at the send time of the first packet the report
/// shows received after the outage's mark. A burst sent after the link came back is
/// congestion, even when it falls in the feedback interval before the first report: it
/// must cut, and the exemption must not swallow it for the whole report.
#[test]
fn a_congestion_burst_right_after_recovery_still_cuts() {
    let mut s = Sim::new(20_000.0);
    s.send_kbps = Some(6000.0);
    s.run(5.0);
    s.outage(10.0);
    assert!(s.est.recover_to.is_some(), "the outage left no memory");
    // The link is back, but the first frames after it lose a burst: the report shows a
    // received packet behind the gap, so the mark ends before those losses.
    s.drop_every = 4;
    let lines = logged(|| s.run(0.05));
    let labels: Vec<String> = cuts(&lines).into_iter().map(|c| c.0).collect();
    assert!(
        labels.iter().any(|l| l.contains("loss")),
        "the burst right after recovery was exempted: {labels:?}"
    );
    assert!(
        s.est.recover_to.is_none(),
        "the pre-outage rate outlived the loss cut: {labels:?}"
    );
}

/// #1557 review A1: a packet exempted as the outage's own loss was never counted, so its
/// late arrival must not undo a counted loss. It used to pass through the late-loss path
/// and subtract one it never added.
#[test]
fn a_late_exempted_outage_loss_does_not_undo_a_counted_loss() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    // seq 0 sent before the mark (counted when lost), seq 1..20 after it (the outage's
    // own), seq 21..40 received after the mark: the first of those ends the exemption.
    let sent_us = |seq: u32| u64::from(seq) * 10_000;
    let arrived = |seq: u32| ((sent_us(seq) + 5_000) / UNIT_US) as u16;
    for seq in 0..41u32 {
        let mut d = vec![0u8; SIZE];
        d[1..5].copy_from_slice(&seq.to_le_bytes());
        let at = start + Duration::from_micros(sent_us(seq));
        log.record(&d, at, false);
    }
    est.outage_from_us = Some(10_000);
    let first: Vec<u16> = (0..41u32)
        .map(|seq| {
            if seq <= 20 {
                NOT_RECEIVED
            } else {
                arrived(seq)
            }
        })
        .collect();
    let fb = Feedback {
        first_pkt_seq: 0,
        ref_us: 0,
        arrivals: first,
    };
    est.on_feedback(&fb, &mut log, start + Duration::from_millis(1_000), NO_QUIC);
    assert_eq!(
        est.loss(),
        Some(1.0 / 21.0),
        "the one counted loss was measured, the outage's own twenty were not"
    );
    // seq 1, exempted, arrives late in a later report.
    let late = Feedback {
        first_pkt_seq: 1,
        ref_us: 0,
        arrivals: vec![arrived(1)],
    };
    est.on_feedback(
        &late,
        &mut log,
        start + Duration::from_millis(1_050),
        NO_QUIC,
    );
    let loss = est.loss();
    assert!(
        loss.is_some_and(|l| (l - 1.0 / 22.0).abs() < 1e-12),
        "the exempted packet's late arrival subtracted a loss never counted: {loss:?}"
    );
}

/// #1417: the regrow stops at the rate the link carried before the outage instead of crossing
/// it, so the estimate is never driven above a rate that was proved. The rate is under the
/// ceiling and the encoder follows the estimate, so neither the ceiling clamp nor the
/// receive-rate cap is what stops it: only the memory's own bound is.
#[test]
fn the_regrow_stops_at_the_rate_the_link_carried() {
    let mut s = Sim::new(4000.0);
    s.send_kbps = None;
    s.run(15.0);
    let carried = s.est.estimate_kbps();
    assert!(
        (2000.0..0.95 * CEILING).contains(&carried),
        "the link carried {carried:.0}, which the ceiling cannot hide"
    );
    s.outage(10.0);
    let resumed = s.t;
    // The same link comes back.
    s.run(10.0);
    let crossing = s
        .trace
        .iter()
        .filter(|&&(t, _)| t >= resumed as f64 / 1e6)
        .map(|&(_, e)| e)
        .find(|&e| e >= carried)
        .unwrap_or(f64::NAN);
    assert!(
        crossing.is_finite(),
        "the estimate never got back to {carried:.0}"
    );
    assert!(
        (crossing - carried).abs() < 1e-9 * carried,
        "the regrow crossed to {crossing:.0} from a target of {carried:.0}"
    );
}

/// #1417: the regrow ends the moment the link says it no longer carries the old rate. A link
/// that came back a quarter as fast cuts the delay bound, and the memory goes with it; a
/// still-held memory would keep the estimate racing at 2 %/50 ms toward a rate the link has
/// already refused.
#[test]
fn a_delay_cut_ends_the_regrow() {
    let mut s = Sim::new(6000.0);
    s.send_kbps = None;
    s.run(10.0);
    let carried = s.est.estimate_kbps();
    assert!(carried > 2000.0, "the link carried {carried:.0}");
    s.outage(10.0);
    assert!(s.est.recover_to.is_some(), "the outage left no memory");
    // Back a quarter as fast, with the encoder following the estimate, on a buffer deep
    // enough that it never drops: the delay signal is the only one this link gives.
    s.cap_kbps = carried / 4.0;
    s.queue_bytes = 4.0 * 1024.0 * 1024.0;
    let lines = logged(|| s.run(15.0));
    let labels: Vec<String> = cuts(&lines).into_iter().map(|c| c.0).collect();
    assert!(
        labels.iter().any(|l| l.contains("delay")),
        "no delay cut: {labels:?}"
    );
    assert!(
        s.est.recover_to.is_none(),
        "the pre-outage rate outlived the delay cut: {labels:?}"
    );
    let e = s.est.estimate_kbps();
    assert!(
        e < carried / 2.0,
        "{e:.0} still climbing toward {carried:.0}"
    );
}

/// #1417: and a loss cut ends it too, on a link that only drops (a queue of two datagrams
/// shows no delay), so the loss arm is the one that decides.
#[test]
fn a_loss_cut_ends_the_regrow() {
    let mut s = Sim::new(12_000.0);
    s.send_kbps = Some(6000.0);
    s.run(10.0);
    let carried = s.est.estimate_kbps();
    assert!(carried > 2000.0, "the link carried {carried:.0}");
    s.outage(10.0);
    assert!(s.est.recover_to.is_some(), "the outage left no memory");
    // Back an eighth as fast, with a two-datagram buffer that drops what overruns it and no
    // delay to measure, and the encoder following the estimate.
    s.cap_kbps = 1500.0;
    s.queue_bytes = 2.0 * SIZE as f64;
    s.send_kbps = None;
    let lines = logged(|| s.run(15.0));
    let labels: Vec<String> = cuts(&lines).into_iter().map(|c| c.0).collect();
    let first = labels
        .iter()
        .find(|l| l.contains("delay") || l.contains("loss"));
    assert!(
        first.is_some_and(|l| l.contains("loss")),
        "the first cut was not a loss cut: {labels:?}"
    );
    assert!(
        s.est.recover_to.is_none(),
        "the pre-outage rate outlived the loss cut: {labels:?}"
    );
    let e = s.est.estimate_kbps();
    assert!(
        e < 0.75 * carried,
        "{e:.0} still climbing toward {carried:.0}"
    );
}

/// #1417: the memory of the pre-outage rate is for the stretch after an outage, not for good.
/// A still screen never gets back to it, and minutes later busy content must not race at 2x a
/// second toward a rate measured before the outage.
#[test]
fn the_pre_outage_memory_expires_after_a_still_screen() {
    let mut s = Sim::new(20_000.0);
    s.send_kbps = None;
    s.run(5.0);
    let carried = s.est.estimate_kbps();
    s.outage(10.0);
    assert!(s.est.recover_to.is_some(), "the outage left no memory");
    // A still screen: a datagram a frame, far under the estimate, so the link's rate is
    // never measured and the regrow stalls under the receive-rate cap.
    s.content_kbps = Box::new(|_| 50.0);
    s.run(RECOVERY_HOLD.as_secs_f64() + 5.0);
    assert!(
        s.est.recover_to.is_none(),
        "the memory outlived the still screen"
    );
    assert!(
        s.est.estimate_kbps() < 0.25 * carried,
        "the still screen carried it to {:.0} of {carried:.0}",
        s.est.estimate_kbps()
    );
    // Busy content afterwards climbs at AIMD's 8 % a second: nowhere near the old rate.
    s.content_kbps = Box::new(|_| f64::MAX);
    s.run(5.0);
    let e = s.est.estimate_kbps();
    assert!(
        e < 0.5 * carried,
        "{e:.0} after 5 s of busy content, from a rate of {carried:.0}"
    );
}

/// #1417 review A2/B1: the regrow's bound is on each bound, not only on the one the estimate
/// happens to sit on. A short outage on a delay-bound session leaves the loss bound above the
/// target and the delay bound under it: the delay bound may climb to the target, and the loss
/// bound, already over it, must not grow at all.
#[test]
fn the_regrow_caps_each_bound_at_the_pre_outage_rate() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    est.delay_kbps = 2000.0;
    est.loss_kbps = 5120.0;
    est.settle();
    est.recover_to = Some((3000.0, start + RECOVERY_HOLD));
    // A second between messages would double a bound that is not held.
    let m = Measured {
        usage: Usage::Normal,
        rate_kbps: Some(20_000.0),
        sent_kbps: None,
        fresh: true,
        loss: None,
    };
    est.apply(m, start + Duration::from_secs(1), NO_QUIC);
    assert_eq!(
        est.delay_kbps, 3000.0,
        "the delay bound climbed to {} past a target of 3000",
        est.delay_kbps
    );
    assert_eq!(
        est.loss_kbps, 5120.0,
        "the loss bound grew to {} past a target of 3000",
        est.loss_kbps
    );
}

/// #1417 review A1: what the link delivers caps BOTH bounds, not only the delay one. With the
/// loss bound the binding one and an application-limited sender, the loss bound holds at the
/// receive-rate cap instead of climbing toward the pre-outage rate unmeasured.
#[test]
fn the_loss_bound_grows_within_the_receive_rate_cap() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    est.delay_kbps = 5000.0;
    est.loss_kbps = 1000.0;
    est.settle();
    est.recover_to = Some((5000.0, start + RECOVERY_HOLD));
    // A second later, 300 kbit/s arriving with no send rate: the sender is application
    // limited, so 1.5 x 300 + 10 kbps is all the link is known to carry.
    let m = Measured {
        usage: Usage::Normal,
        rate_kbps: Some(300.0),
        sent_kbps: None,
        fresh: true,
        loss: None,
    };
    est.apply(m, start + Duration::from_secs(1), NO_QUIC);
    let cap = RECV_CAP * 300.0 + RECV_CAP_ADD_KBPS;
    assert!(
        est.loss_kbps <= 1000.0 + 1e-9,
        "the loss bound grew to {:.0} past a cap of {cap:.0}",
        est.loss_kbps
    );
    assert_eq!(
        est.recover_to.map(|(to, _)| to),
        Some(5000.0),
        "still recovering"
    );
}

#[test]
fn a_frame_after_a_still_screen_is_not_judged_before_its_feedback_can_return() {
    let mut s = Sim::new(50_000.0);
    s.send_kbps = Some(6000.0);
    s.run(2.0);
    // 5 s of a still screen: nothing sent, so nothing to answer.
    let still = s.t;
    for step in 1..=50u64 {
        let at = s.start + Duration::from_micros(still + step * 100_000);
        assert_eq!(s.est.on_tick(at, &s.log, RTT), None);
    }
    s.t = still + 5_000_000;
    s.frame();
    let sent = s.t;
    for (after_ms, answered) in [(30, false), (60, true), (100, false), (700, false)] {
        s.t = sent + after_ms * 1000;
        if answered {
            s.feedback();
        }
        let at = s.start + Duration::from_micros(s.t);
        s.est.on_tick(at, &s.log, RTT);
    }
    assert_eq!(
        s.est.estimate_kbps(),
        CEILING,
        "the frame was answered in 60 ms"
    );
}

#[test]
fn no_feedback_ever_or_no_media_sent_is_no_silence() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    log.record(&[1, 0, 0, 0, 0], start, false);
    let later = start + Duration::from_secs(5);
    assert_eq!(
        est.on_tick(later, &log, RTT),
        None,
        "a client that omits feedback"
    );
    let fb = Feedback {
        first_pkt_seq: 0,
        ref_us: 0,
        arrivals: vec![0],
    };
    est.on_feedback(&fb, &mut log, later, NO_QUIC);
    let quiet = later + Duration::from_secs(5);
    assert_eq!(
        est.on_tick(quiet, &log, RTT),
        None,
        "nothing was sent since"
    );
    assert_eq!(est.estimate_kbps(), CEILING);
}

#[test]
fn a_replayed_or_re_reported_range_is_counted_once_and_memory_stays_bounded() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    for seq in 0..100u32 {
        let mut d = vec![0u8; SIZE];
        d[1..5].copy_from_slice(&seq.to_le_bytes());
        log.record(&d, start + Duration::from_micros(seq as u64 * 1000), false);
    }
    let mut arrivals: Vec<u16> = (0..100).map(|i| i * 20).collect();
    arrivals[50] = NOT_RECEIVED;
    let fb = Feedback {
        first_pkt_seq: 0,
        ref_us: 1_000_000,
        arrivals,
    };
    let at = start + Duration::from_millis(100);
    let first = est.measure(&fb, &mut log, at);
    assert_eq!(first.loss, Some(0.01));
    for _ in 0..10_000 {
        est.measure(&fb, &mut log, at);
    }
    assert_eq!(est.arrivals.len(), 99, "a replay adds no arrival");
    assert!(est.losses.len() <= MAX_LOSS_ENTRIES);
    assert_eq!(
        est.measure(&fb, &mut log, at).loss,
        Some(0.01),
        "nor a loss"
    );
    // The latest report of the lost one says it arrived after all.
    let late = Feedback {
        first_pkt_seq: 50,
        ref_us: 1_002_000,
        arrivals: vec![0],
    };
    assert_eq!(est.measure(&late, &mut log, at).loss, Some(0.0));
}

#[test]
fn sequence_numbers_wrap() {
    let mut s = Sim::at_seq(3000.0, u32::MAX - 100);
    s.run(10.0);
    assert!(s.est.estimate_kbps() < 3500.0);
}

#[test]
fn the_abr_is_fed_at_most_every_100_ms() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    let fb = Feedback {
        first_pkt_seq: 0,
        ref_us: 0,
        arrivals: vec![0],
    };
    let fed: Vec<bool> = (0..6)
        .map(|i| {
            est.on_feedback(
                &fb,
                &mut log,
                start + Duration::from_millis(i * 50),
                NO_QUIC,
            )
            .is_some()
        })
        .collect();
    assert_eq!(fed, [true, false, true, false, true, false]);
}

#[test]
fn a_message_out_of_contract_is_refused_naming_the_field() {
    let b64 = |n: usize| base64::engine::general_purpose::STANDARD.encode(vec![0u8; n]);
    let ok = serde_json::json!({"type": "feedback", "seq": 1, "first_pkt_seq": 7, "count": 2,
        "ref_us": 9, "arrivals_b64": b64(4), "recv_kbps": 1, "lost": 0, "interval_ms": 50});
    assert_eq!(
        feedback_of(&ok),
        Ok(Feedback {
            first_pkt_seq: 7,
            ref_us: 9,
            arrivals: vec![0, 0]
        })
    );
    for (field, value, named) in [
        ("count", serde_json::json!(3), "arrivals_b64"),
        ("count", serde_json::json!(0), "count"),
        ("count", serde_json::json!(8193), "count"),
        (
            "first_pkt_seq",
            serde_json::json!(1u64 << 32),
            "first_pkt_seq",
        ),
        (
            "arrivals_b64",
            serde_json::json!("not base64!"),
            "arrivals_b64",
        ),
        ("ref_us", serde_json::json!(-1), "ref_us"),
        ("ref_us", serde_json::json!(u64::MAX), "ref_us"),
        ("lost", serde_json::json!(3), "lost"),
        ("seq", serde_json::Value::Null, "seq"),
        ("interval_ms", serde_json::json!("50"), "interval_ms"),
    ] {
        let mut bad = ok.clone();
        bad[field] = value.clone();
        assert_eq!(feedback_of(&bad), Err(named), "{field} = {value}");
    }
}

/// The weak link of #1316: 0–20 ms of jitter per frame, and every 7 s a delay spike of
/// `spike_ms` that builds over 200 ms and drains over 300 ms. First in, first out.
fn jitter(spike_ms: u64) -> Box<dyn Fn(u64) -> u64> {
    Box::new(move |t| {
        let jitter = (t / 16_667).wrapping_mul(2_654_435_761) % 20_000;
        let phase = t % 7_000_000;
        let spike = match phase {
            _ if t < 3_000_000 => 0,
            0..200_000 => phase * spike_ms / 200,
            200_000..500_000 => (500_000 - phase) * spike_ms / 300,
            _ => 0,
        };
        jitter + spike
    })
}

/// A second of a quiet scene (1500 kbit/s) every 10 s from 5 s on.
fn quiet_seconds(t: u64) -> f64 {
    if t > 5_000_000 && (t / 1_000_000) % 10 == 5 {
        1500.0
    } else {
        f64::MAX
    }
}

#[test]
fn a_jittery_near_lossless_link_with_quiet_scenes_keeps_the_estimate_up() {
    // The link carries more than the ceiling; one datagram in 20 000 is lost.
    let mut s = Sim::new(15_000.0);
    s.extra_us = jitter(110);
    s.content_kbps = Box::new(quiet_seconds);
    s.drop_every = 20_000;
    s.run(60.0);
    // A spike costs one cut, on the most the link delivered in the last busy second.
    let low = s.min_after(3.0);
    assert!(low > 0.85 * CEILING, "lowest {low:.0}");
}

#[test]
fn a_still_screen_then_a_busy_one_does_not_collapse_the_estimate() {
    let mut s = Sim::new(15_000.0);
    s.extra_us = jitter(110);
    // One datagram a frame for 10 s, then whatever the estimate allows.
    s.content_kbps = Box::new(|t| if t < 10_000_000 { 1.0 } else { f64::MAX });
    s.run(30.0);
    let low = s.min_after(0.0);
    assert!(low > 0.8 * CEILING, "lowest {low:.0}");
    assert!(
        s.mean_after(20.0) > 0.85 * CEILING,
        "busy at {:.0}",
        s.mean_after(20.0)
    );
}

#[test]
fn a_long_delay_spike_costs_one_cut_not_one_per_rtt() {
    let mut s = Sim::new(15_000.0);
    s.extra_us = jitter(400);
    s.run(60.0);
    let low = s.min_after(3.0);
    assert!(low > 0.85 * CEILING, "lowest {low:.0}");
}

#[test]
fn a_capped_link_with_quiet_scenes_still_holds_the_estimate_at_the_cap() {
    let mut s = Sim::new(6000.0);
    s.content_kbps = Box::new(quiet_seconds);
    s.run(40.0);
    let mean = s.mean_after(10.0);
    assert!(
        (4500.0..6300.0).contains(&mean),
        "the sender follows the estimate around a 6000 kbps cap, mean {mean:.0}"
    );
}

#[test]
fn a_link_that_drops_to_a_third_drives_the_estimate_down() {
    let mut s = Sim::new(15_000.0);
    s.run(10.0);
    assert_eq!(s.est.estimate_kbps(), CEILING);
    s.cap_kbps = 3000.0;
    s.run(3.0);
    let e = s.est.estimate_kbps();
    assert!(e < 3000.0, "3 s after the drop: {e:.0}");
    s.run(10.0);
    let mean = s.mean_after(16.0);
    assert!((2000.0..3300.0).contains(&mean), "mean {mean:.0}");
}

/// A link that reorders (#1330): each datagram held 0–40 ms on its own, the netem jitter with
/// no rate the browser still receives (`web/src/webtransport/reorder.test.ts`), one in `drop_every`
/// lost. The client waits out its reorder window before it reports a gap.
fn reordering(drop_every: u32) -> Sim {
    let mut s = Sim::new(15_000.0);
    s.reorder_us = Box::new(|seq| u64::from(seq).wrapping_mul(2_654_435_761) % 40_000);
    s.drop_every = drop_every;
    s.hold_us = 45_000;
    s
}

#[test]
fn a_link_that_reorders_keeps_the_estimate_at_the_link_rate() {
    let mut s = reordering(1000);
    s.run(30.0);
    let (low, mean) = (s.min_after(3.0), s.mean_after(3.0));
    assert!(
        low > 0.85 * CEILING && mean > 0.95 * CEILING,
        "lowest {low:.0}, mean {mean:.0}"
    );
}

#[test]
fn a_link_that_reorders_and_loses_still_cuts_on_the_loss() {
    // Over LOSS_HIGH: 5 % sits in the band that holds the estimate (libwebrtc's rule).
    let mut s = reordering(7);
    s.run(30.0);
    let mean = s.mean_after(10.0);
    assert!(mean < 0.5 * CEILING, "mean {mean:.0}");
}

fn window(governor: &Governor, now: Instant) -> Box<Window> {
    Arc::new(WindowFactory(governor.clone()))
        .build(now, 1200)
        .into_any()
        .downcast()
        .unwrap()
}

/// What `on_ack` takes from quinn's `RttEstimator`, which a test cannot make.
fn acked(w: &mut Window, srtt: Duration, min_rtt: Duration) {
    w.srtt = srtt;
    w.min_rtt = min_rtt;
}

#[test]
fn a_governed_window_follows_the_estimate_and_no_loss_shrinks_it() {
    let start = Instant::now();
    let governor = Governor::default();
    let mut w = window(&governor, start);
    let cubic = w.window();
    w.on_congestion_event(start, start, true, 1200);
    let collapsed = w.window();
    assert!(
        collapsed < cubic,
        "ungoverned, it is Cubic: loss shrinks it"
    );
    governor.govern(8000.0);
    assert_eq!(w.window(), collapsed, "no RTT yet: Cubic until an ack");
    acked(&mut w, Duration::from_millis(5), Duration::from_millis(5));
    // 2 × 8 Mbit/s × the 100 ms RTT floor.
    assert_eq!(w.window(), 200_000);
    w.on_congestion_event(start, start + Duration::from_secs(1), true, 1200);
    assert_eq!(w.window(), 200_000, "a governed window ignores loss");
    governor.govern(100.0);
    assert_eq!(
        w.window(),
        w.initial_window(),
        "never under Cubic's first window"
    );
}

#[test]
fn a_release_starts_cubic_afresh_from_the_paths_bdp() {
    let start = Instant::now();
    let governor = Governor::default();
    let mut w = window(&governor, start);
    // Queueing put the smoothed RTT at 2 s; the path's own is 20 ms.
    acked(&mut w, Duration::from_secs(2), Duration::from_millis(20));
    governor.govern(8000.0);
    w.on_sent(start, 1200, 1);
    // Losses while governed collapse the Cubic underneath to its minimum.
    for n in 1..5 {
        let at = start + Duration::from_secs(n);
        w.on_congestion_event(at, at, true, 1200);
    }
    assert_eq!(w.cubic.window(), 2400);
    governor.release();
    // 8 Mbit/s × 20 ms: neither 2 × MTU nor the governed 2 × rate × 2 s.
    assert_eq!(w.window(), 20_000, "before quinn calls in again");
    let later = start + Duration::from_secs(10);
    w.on_sent(later, 1200, 2);
    assert_eq!(w.window(), 20_000);
    assert_eq!(w.initial_window(), 12_000, "quinn's first window is kept");
    // A real Cubic: the next loss takes it to 0.7 ×.
    w.on_congestion_event(later, later, false, 1200);
    assert_eq!(w.window(), 14_000);
    // A slow path's BDP is under Cubic's first window: that is the floor.
    governor.govern(1000.0);
    w.on_sent(later, 1200, 3);
    governor.release();
    assert_eq!(w.window(), 12_000);
}

#[test]
fn a_starved_probe_allows_one_packet_then_restores_cubics_loss_response() {
    let start = Instant::now();
    let governor = Governor::default();
    let mut w = window(&governor, start);
    let before = w.window();
    governor.probe(true);
    assert_eq!(w.window(), u64::MAX);
    // Even a loss while the probe is pending still reaches Cubic.
    w.on_congestion_event(start, start, true, 1200);
    w.on_sent(start, 1200, 1);
    assert!(w.window() < before);
    assert_eq!(w.window(), w.cubic.window());
    governor.probe(true);
    governor.probe(false);
    assert_eq!(w.window(), w.cubic.window(), "cancelling an unsent probe");
}

const HOLD: Duration = Duration::from_millis(250);
const FRAME: Duration = Duration::from_millis(33);

#[test]
fn a_keyframe_being_paced_out_is_never_read_as_a_stall() {
    let start = Instant::now();
    let mut b = Backlog::default();
    // A 300 KB keyframe drained at 20 KB a frame interval, 10 KB frames behind it.
    let mut queued = 0usize;
    for n in 0..60u32 {
        let at = start + FRAME * n;
        assert_eq!(b.admit(queued, n == 0, at, HOLD), Admit::Send, "unit {n}");
        queued += if n == 0 { 300_000 } else { 10_000 };
        queued = queued.saturating_sub(20_000);
    }
    assert_eq!(b.skipped, 0);
}

#[test]
fn a_queue_that_stops_draining_skips_video_until_it_drains_and_a_keyframe_comes() {
    let start = Instant::now();
    let mut b = Backlog::default();
    assert!(!b.starved(10_000), "a healthy queue needs no probe");
    let mut queued = 0usize;
    let mut n = 0u32;
    let mut admit = |b: &mut Backlog, queued: usize, key: bool| {
        n += 1;
        b.admit(queued, key, start + FRAME * n, HOLD)
    };
    // The window closes: every frame stays in the queue.
    let mut sent = 0;
    while admit(&mut b, queued, false) == Admit::Send {
        queued += 10_000;
        sent += 1;
    }
    // Growing since the second frame; skipping starts once that is longer than the hold.
    assert_eq!(sent, 9, "the queue holds the hold's worth, not a MiB");
    assert!(b.starved(queued));
    assert!(!b.starved(0), "waiting for a keyframe needs no path probe");
    assert_eq!(
        admit(&mut b, queued, true),
        Admit::Skip,
        "not while it is backed up"
    );
    assert_eq!(
        admit(&mut b, 0, false),
        Admit::SkipForKeyframe,
        "drained: ask once"
    );
    assert_eq!(admit(&mut b, 0, false), Admit::Skip);
    assert_eq!(
        admit(&mut b, 0, true),
        Admit::Send,
        "resumes at the keyframe"
    );
    assert_eq!(admit(&mut b, 10_000, false), Admit::Send);
    assert!(!b.starved(10_000), "recovered: stop probing");
    assert_eq!(b.skipped, 4);
}

/// Backed up for more than the hold: skipping from `start + 9 frames`.
fn stalled(b: &mut Backlog, start: Instant) -> Instant {
    let mut queued = 0;
    for n in 0..20u32 {
        let at = start + FRAME * n;
        if b.admit(queued, false, at, HOLD) != Admit::Send {
            return at;
        }
        queued += 10_000;
    }
    panic!("never stalled");
}

#[test]
fn a_path_that_still_acknowledges_is_not_probed_past_its_window() {
    let start = Instant::now();
    let mut b = Backlog::default();
    stalled(&mut b, start);
    let queued = 10 * BACKLOG_EMPTY;
    assert!(
        !b.probe(queued, 5),
        "the first tick has nothing to compare against"
    );
    assert!(
        !b.probe(queued, 6),
        "an ACK arrived since the last tick: QUIC opens its own window"
    );
    assert!(b.probe(queued, 6), "nothing acknowledged for a tick: probe");
    assert!(b.probe(queued, 6));
    assert!(!b.probe(queued, 7), "the probe was answered");
    assert!(
        !b.probe(0, 7),
        "and with the queue drained there is nothing to probe for"
    );
}

#[test]
fn a_probe_emptying_the_queue_does_not_restart_the_give_up_clock() {
    let start = Instant::now();
    let mut b = Backlog::default();
    let at = stalled(&mut b, start);
    b.probed(10);
    b.probed(11); // Later probes must not replace the first one's ACK snapshot.
    b.acknowledge(10);
    assert!(b.starved(0));
    for n in 1..GIVE_UP_HOLDS {
        assert_eq!(b.admit(0, true, at + HOLD * n, HOLD), Admit::Skip);
    }
    assert_eq!(
        b.admit(0, true, at + HOLD * GIVE_UP_HOLDS, HOLD),
        Admit::GiveUp
    );
    b.acknowledge(11);
    assert!(!b.starved(0));
    assert_eq!(b.admit(0, true, at + HOLD * 25, HOLD), Admit::Send);
}

#[test]
fn a_keyframe_skipped_behind_a_datagram_is_asked_for_again_every_hold() {
    let start = Instant::now();
    let mut b = Backlog::default();
    let at = stalled(&mut b, start);
    assert_eq!(b.admit(0, false, at + FRAME, HOLD), Admit::SkipForKeyframe);
    // The keyframe comes while an audio datagram and a resend still wait.
    assert_eq!(b.admit(2400, true, at + FRAME * 2, HOLD), Admit::Skip);
    assert_eq!(b.admit(0, false, at + FRAME * 3, HOLD), Admit::Skip);
    assert_eq!(
        b.admit(0, false, at + FRAME + HOLD, HOLD),
        Admit::SkipForKeyframe,
        "asked again a hold later"
    );
    assert_eq!(b.admit(0, true, at + FRAME * 2 + HOLD, HOLD), Admit::Send);
}

#[test]
fn a_queue_that_never_drains_ends_the_media() {
    let start = Instant::now();
    let mut b = Backlog::default();
    let at = stalled(&mut b, start);
    let give_up = HOLD * GIVE_UP_HOLDS;
    // Keyframes keep coming, but the queue never empties.
    let mut t = at;
    while b.admit(400_000, true, t, HOLD) != Admit::GiveUp {
        t += FRAME;
    }
    // One count a hold, each on the first frame past it.
    assert!(
        (give_up..give_up + (FRAME + HOLD) * 2).contains(&(t - at)),
        "{:?}",
        t - at
    );
    // A keyframe admitted before then ends the episode, and the next one starts afresh.
    let mut b = Backlog::default();
    let at = stalled(&mut b, start);
    assert_eq!(b.admit(0, true, at + give_up / 2, HOLD), Admit::Send);
    let again = stalled(&mut b, at + give_up);
    assert_eq!(
        b.admit(400_000, false, again + give_up - FRAME, HOLD),
        Admit::Skip
    );
}

#[test]
fn a_still_screen_after_the_queue_drained_asks_for_a_keyframe_before_giving_up() {
    let start = Instant::now();
    let mut b = Backlog::default();
    let at = stalled(&mut b, start);
    // The path recovered and the queue drained, but nothing came for 7 s.
    let mut t = at + Duration::from_secs(7);
    assert_eq!(b.admit(0, false, t, HOLD), Admit::SkipForKeyframe);
    // Asks that go unanswered end it after the full count, however long the screen was still.
    let mut asks = 1;
    loop {
        t += FRAME;
        match b.admit(0, false, t, HOLD) {
            Admit::SkipForKeyframe => asks += 1,
            Admit::Skip => {}
            Admit::GiveUp => break,
            Admit::Send => panic!("no keyframe was admitted"),
        }
    }
    assert_eq!(asks, GIVE_UP_HOLDS);
}

/// A known limit, not a guarantee: inside the 500 ms hold a quiet sender cannot be told
/// from a link that dropped, so a spike of 800 ms in a quiet scene cuts to what arrived.
#[test]
fn known_limit_an_800_ms_spike_in_a_quiet_scene_cuts_to_the_receive_rate() {
    let mut s = Sim::new(15_000.0);
    s.extra_us = jitter(800);
    s.content_kbps = Box::new(|_| 1500.0);
    s.run(12.0);
    let low = s.min_after(3.0);
    assert!(low < 0.1 * CEILING, "lowest {low:.0}");
}

#[test]
fn a_quiet_sender_follows_a_link_that_drops_below_its_content() {
    // Content at 0.55 × the ceiling: under 80 % of the estimate, so no window is a
    // throughput sample.
    let mut s = Sim::new(15_000.0);
    s.content_kbps = Box::new(|_| 0.55 * CEILING);
    s.run(10.0);
    assert_eq!(s.est.estimate_kbps(), CEILING);
    s.cap_kbps = 3000.0;
    s.run(1.0);
    let e = s.est.estimate_kbps();
    assert!(e <= 3000.0, "1 s after the drop: {e:.0}");
    s.run(5.0);
    assert_eq!(s.queue_drops, 0, "the bottleneck queue never filled");
    let mean = s.mean_after(12.0);
    assert!((2000.0..3300.0).contains(&mean), "mean {mean:.0}");
}

/// `n` video datagrams sent `gap_us` apart, and feedback that names them all as received
/// a millisecond apart, except those `lost` says.
fn busy_window(log: &mut SendLog, start: Instant, gap_us: u64, lost: fn(u16) -> bool) -> Feedback {
    for seq in 0..200u32 {
        let mut d = vec![0u8; SIZE];
        d[1..5].copy_from_slice(&seq.to_le_bytes());
        log.record(
            &d,
            start + Duration::from_micros(u64::from(seq) * gap_us),
            false,
        );
    }
    Feedback {
        first_pkt_seq: 0,
        ref_us: 0,
        arrivals: (0..200u16)
            .map(|i| if lost(i) { NOT_RECEIVED } else { i * 20 })
            .collect(),
    }
}

#[test]
fn feedback_with_no_fresh_video_arrival_adds_no_throughput_sample() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    // 9.2 Mbit/s sent: busy.
    let fb = busy_window(&mut log, start, 1000, |_| false);
    let at = start + Duration::from_millis(250);
    est.on_feedback(&fb, &mut log, at, NO_QUIC);
    assert_eq!(est.throughput.len(), 1);
    // The range reported again, and a message naming only audio (never in the send log).
    let audio = Feedback {
        first_pkt_seq: 5000,
        ref_us: 0,
        arrivals: vec![0; 10],
    };
    for i in 1..=10 {
        let at = at + Duration::from_millis(50 * i);
        est.on_feedback(&fb, &mut log, at, NO_QUIC);
        est.on_feedback(&audio, &mut log, at, NO_QUIC);
    }
    assert_eq!(est.throughput.len(), 1, "no fresh arrival, no sample");

    // Then a fresh application-limited window (918 kbps) under overuse, and overuse held
    // for a second over only that range reported again and audio: no fresh rate to cut
    // from.
    let quiet = paced(&mut log, start, 1000, 50, 1_000_000, 10_000);
    let at = start + Duration::from_millis(1500);
    overusing(&mut est, &quiet, &mut log, at);
    let held = (est.estimate_kbps(), est.delay_kbps);
    for i in 1..=20 {
        let at = at + Duration::from_millis(50 * i);
        overusing(&mut est, &quiet, &mut log, at);
        overusing(&mut est, &audio, &mut log, at);
    }
    assert_eq!((est.estimate_kbps(), est.delay_kbps), held);
}

/// `n` video datagrams from `first`, sent `gap_us` apart from `from_us`, and feedback that
/// names them all as received `gap_us` apart.
fn paced(
    log: &mut SendLog,
    start: Instant,
    first: u32,
    n: u32,
    from_us: u64,
    gap_us: u64,
) -> Feedback {
    for i in 0..n {
        let mut d = vec![0u8; SIZE];
        d[1..5].copy_from_slice(&(first + i).to_le_bytes());
        let at = from_us + u64::from(i) * gap_us;
        log.record(&d, start + Duration::from_micros(at), false);
    }
    Feedback {
        first_pkt_seq: first,
        ref_us: from_us,
        arrivals: (0..n)
            .map(|i| (u64::from(i) * gap_us / UNIT_US) as u16)
            .collect(),
    }
}

/// One message, with the detector made to say `Overusing`.
fn overusing(est: &mut Estimator, fb: &Feedback, log: &mut SendLog, at: Instant) {
    let mut m = est.measure(fb, log, at);
    m.usage = Usage::Overusing;
    est.apply(m, at, NO_QUIC);
}

#[test]
fn a_detector_flickering_to_normal_still_holds_the_overuse() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    // Every 100 ms, ten datagrams sent and received 10 ms apart: 918 kbps, application-
    // limited. The detector says Overusing for 400 ms, then Normal for 100 ms, for 12 s.
    for i in 0..120u32 {
        let from_us = u64::from(i) * 100_000;
        let fb = paced(&mut log, start, i * 10, 10, from_us, 10_000);
        let at = start + Duration::from_micros(from_us + 100_000);
        let mut m = est.measure(&fb, &mut log, at);
        m.usage = if i % 5 == 4 {
            Usage::Normal
        } else {
            Usage::Overusing
        };
        est.apply(m, at, NO_QUIC);
    }
    let e = est.estimate_kbps();
    assert!(e < 0.85 * 918.0 * 1.05, "12 s of overuse: {e:.0}");
}

#[test]
fn datagrams_sent_in_one_instant_are_no_throughput_sample() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    let fb = busy_window(&mut log, start, 0, |_| false);
    est.on_feedback(&fb, &mut log, start + Duration::from_millis(250), NO_QUIC);
    assert!(est.throughput.is_empty(), "no send rate, no sample");
}

#[test]
fn a_busy_sender_under_heavy_loss_is_still_a_throughput_sample() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    // 9.2 Mbit/s sent, 40 % lost: what arrived was sent at 5.5.
    let fb = busy_window(&mut log, start, 1000, |i| i % 5 < 2);
    est.on_feedback(&fb, &mut log, start + Duration::from_millis(250), NO_QUIC);
    assert_eq!(est.throughput.len(), 1, "the lost datagrams were sent too");
}

/// The lines `run` logs at INFO and above, on this thread.
fn logged(run: impl FnOnce()) -> Vec<String> {
    let captured = Captured::default();
    let subscriber = tracing_subscriber::fmt()
        .with_ansi(false)
        .with_writer(captured.clone())
        .finish();
    tracing::subscriber::with_default(subscriber, || {
        tracing::callsite::rebuild_interest_cache();
        run();
    });
    let logs = String::from_utf8(captured.0.lock().unwrap().clone()).unwrap();
    logs.lines().map(String::from).collect()
}

#[test]
fn a_bound_flipping_at_the_feedback_rate_is_logged_at_most_once_a_second() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    est.delay_kbps = 3000.0;
    est.loss_kbps = 3000.0;
    est.settle();
    // For 2 s, a message every 50 ms grows the delay bound and the next the loss bound,
    // so the lower one, the estimate's bound, flips at every message.
    let lines = logged(|| {
        for i in 1..=40u32 {
            let delay_grows = i % 2 == 1;
            let m = Measured {
                usage: if delay_grows {
                    Usage::Normal
                } else {
                    Usage::Underusing
                },
                rate_kbps: None,
                sent_kbps: None,
                fresh: false,
                loss: delay_grows.then_some(0.05),
            };
            est.apply(m, start + 50 * Duration::from_millis(i.into()), NO_QUIC);
        }
    });
    assert!((1..=3).contains(&lines.len()), "{} lines", lines.len());
}

#[test]
fn a_late_report_of_a_datagram_behind_the_rate_window_is_not_fresh() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    // Arrivals at 1.00–1.09 s.
    let fb = paced(&mut log, start, 0, 10, 1_000_000, 10_000);
    assert!(est.measure(&fb, &mut log, start).fresh);
    // First reported now, but it arrived 1.09 s before the newest arrival.
    let late = paced(&mut log, start, 10, 1, 0, 10_000);
    assert!(!est.measure(&late, &mut log, start).fresh);
    // 290 ms behind: in the 500 ms window.
    let recent = paced(&mut log, start, 11, 1, 800_000, 10_000);
    assert!(est.measure(&recent, &mut log, start).fresh);
}

/// Every `webtransport-cc-bound` line as (cut, from, to).
fn cuts(lines: &[String]) -> Vec<(String, f64, f64)> {
    lines
        .iter()
        .filter(|l| l.contains("webtransport-cc-bound"))
        .map(|l| {
            let cut = l.split("cut=\"").nth(1).unwrap().split('"').next().unwrap();
            let span = l.split("estimate ").nth(1).unwrap();
            let mut kbps = span.split(" kbps").next().unwrap().split(" → ");
            let mut next = || kbps.next().unwrap().parse::<f64>().unwrap();
            (cut.to_string(), next(), next())
        })
        .collect()
}

#[test]
fn each_line_names_every_cause_of_the_fall_it_covers() {
    let start = Instant::now();
    let at = |ms: u64| start + Duration::from_millis(ms);
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    let empty = Feedback {
        first_pkt_seq: 0,
        ref_us: 0,
        arrivals: vec![],
    };
    let send = |log: &mut SendLog, seq: u32, when: Instant| {
        let mut d = vec![0u8; SIZE];
        d[1..5].copy_from_slice(&seq.to_le_bytes());
        log.record(&d, when, false);
    };
    let lines = logged(|| {
        est.on_feedback(&empty, &mut log, at(0), NO_QUIC);
        // Feedback stops: the first decay is logged, the next waits out the second.
        send(&mut log, 0, at(0));
        est.on_tick(at(600), &log, RTT);
        est.on_tick(at(1100), &log, RTT);
        // Feedback for 200 ms with no cut logs nothing yet; then it stops again, and the
        // silence path logs the decay from the last second along with its own.
        for ms in [1150, 1200, 1250, 1300] {
            est.on_feedback(&empty, &mut log, at(ms), NO_QUIC);
        }
        send(&mut log, 1, at(1300));
        est.on_tick(at(1900), &log, RTT);
        // A decay waiting when a loss cut comes: the line names both.
        est.on_tick(at(2400), &log, RTT);
        let m = Measured {
            usage: Usage::Normal,
            rate_kbps: None,
            sent_kbps: None,
            fresh: false,
            loss: Some(0.5),
        };
        est.apply(m, at(2450), NO_QUIC);
    });
    let cuts = cuts(&lines);
    let labels: Vec<_> = cuts.iter().map(|c| c.0.as_str()).collect();
    assert_eq!(labels, ["silence", "silence", "silence+loss"], "{lines:?}");
    assert_eq!(cuts[0].1, CEILING);
    for pair in cuts.windows(2) {
        assert_eq!(pair[1].1, pair[0].2, "no fall left out: {lines:?}");
    }
    assert_eq!(cuts[2].2, est.estimate_kbps().round());
}

/// #1376: a report is evidence of the link (`measured`) only when its datagrams arrived
/// slower than they were sent, and then it is the estimate, never under `BETA` × the
/// receive rate. A clean link, an idle sender and a report with no receive rate give none.
/// Old code: the higher of the estimate and the receive rate for any busy report, a clean
/// link's included, and the estimate alone (a silence decay's included) with no receive rate.
#[test]
fn a_report_is_evidence_only_when_the_link_held_the_sender_back() {
    let start = Instant::now();
    let at = |ms| start + Duration::from_millis(ms);
    let report = |rate_kbps, sent_kbps, loss| Measured {
        usage: Usage::Normal,
        rate_kbps,
        sent_kbps,
        fresh: true,
        loss,
    };
    let mut est = Estimator::new(CEILING, start);
    est.apply(report(Some(7900.0), Some(8000.0), None), at(100), NO_QUIC);
    assert_eq!(est.measured(), None, "a clean link: arrived as sent");
    est.apply(report(Some(1000.0), Some(3000.0), None), at(200), NO_QUIC);
    assert_eq!(est.measured(), None, "an idle sender (application-limited)");
    est.apply(report(None, Some(8000.0), None), at(300), NO_QUIC);
    assert_eq!(est.measured(), None, "no receive rate");
    est.apply(report(Some(6000.0), Some(8000.0), None), at(400), NO_QUIC);
    assert_eq!(
        est.measured(),
        Some(CEILING),
        "held back: the estimate, above 0.85 × 6000"
    );
    // A loss cut leaves the estimate under what arrived: the reading stops at BETA × that.
    est.apply(report(None, None, Some(0.6)), at(500), NO_QUIC);
    assert!(est.estimate_kbps() < BETA * 7000.0);
    est.apply(report(Some(7000.0), Some(7500.0), None), at(600), NO_QUIC);
    assert_eq!(est.measured(), Some(BETA * 7000.0));
}

const PYRO_TARGET: u32 = 200_000;
const PYRO_FLOOR: u32 = 100_000;

/// A LAN path whose QUIC window allows `kbps` at a 1 ms RTT, the sender under it.
fn quic_at(kbps: f64) -> QuicPath {
    QuicPath {
        rtt: Duration::from_millis(1),
        cwnd_bytes: (kbps * 1000.0 / 8.0 * 0.001) as u64,
        congestion_events: 0,
        queued_bytes: 0,
    }
}

/// As [`quic_at`], with datagrams waiting on the window: QUIC limits the sender.
fn backed_up(kbps: f64) -> QuicPath {
    QuicPath {
        queued_bytes: 64 * 1024,
        ..quic_at(kbps)
    }
}

fn budget_of(kbps: f64) -> u32 {
    pyro_budget(kbps)
}

/// The encoder fills its current budget for `ms`.
fn fill(rate: &mut PyroRate, ms: u64) {
    rate.offer((u64::from(rate.budget) * ms / 8) as usize);
}

/// 40 PyroWave datagrams sent at `at`, the next `pkt_seq` after `seq`.
fn pyro_send(rate: &mut PyroRate, seq: &mut u32, at: Instant) -> u32 {
    let first = *seq;
    for _ in 0..40 {
        let mut d = vec![0u8; SIZE];
        d[1..5].copy_from_slice(&seq.to_le_bytes());
        rate.log.record(&d, at, false);
        *seq += 1;
    }
    first
}

/// Feedback for 40 datagrams from `first`, of which only the first arrived.
fn all_but_one_lost(first: u32) -> Feedback {
    let mut arrivals = vec![NOT_RECEIVED; 40];
    arrivals[0] = 0;
    Feedback {
        first_pkt_seq: first,
        ref_us: 0,
        arrivals,
    }
}

#[test]
fn pyrowave_budget_starts_at_the_target_and_follows_a_lower_estimate() {
    let on = Instant::now();
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
    let open = quic_at(8_000_000.0);
    assert_eq!(
        rate.on_tick(on, open),
        PyroVerdict::Budget(budget_of(f64::from(PYRO_TARGET))),
        "seeded at the target, less the datagram overhead"
    );
    let mut seq = 0;
    let first = pyro_send(&mut rate, &mut seq, on + Duration::from_millis(10));
    let at = on + Duration::from_millis(60);
    let verdict = rate.on_feedback(&all_but_one_lost(first), at, open);
    let estimate = rate.estimator.estimate_kbps;
    assert!(
        estimate < f64::from(PYRO_TARGET) && estimate > f64::from(PYRO_FLOOR),
        "the loss cut the estimate under the target: {estimate:.0}"
    );
    assert_eq!(verdict, PyroVerdict::Budget(budget_of(estimate)));
}

#[test]
fn pyrowave_quic_cap_applies_from_the_first_feedback_and_never_lifts_the_target() {
    let on = Instant::now();
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
    let target = budget_of(f64::from(PYRO_TARGET));
    // Before the first feedback, and under 1 s: the window is the standard codec's, not judged.
    assert_eq!(
        rate.on_tick(on + Duration::from_millis(500), backed_up(150_000.0)),
        PyroVerdict::Budget(target)
    );
    let mut seq = 0;
    let first = pyro_send(&mut rate, &mut seq, on + Duration::from_millis(550));
    let fb = Feedback {
        first_pkt_seq: first,
        ref_us: 0,
        arrivals: (0..40).collect(),
    };
    let at = on + Duration::from_millis(600);
    fill(&mut rate, 100);
    assert_eq!(
        rate.on_feedback(&fb, at, backed_up(150_000.0)),
        PyroVerdict::Budget(budget_of(150_000.0)),
        "min(target, estimate) with the estimate capped by QUIC"
    );
    assert_eq!(
        {
            fill(&mut rate, 100);
            rate.on_tick(at + Duration::from_millis(100), backed_up(8_000_000.0))
        },
        PyroVerdict::Budget(target),
        "a wider window never lifts the budget past the target"
    );
    assert_eq!(
        {
            fill(&mut rate, 100);
            rate.on_tick(at + Duration::from_millis(200), quic_at(150_000.0))
        },
        PyroVerdict::Hold,
        "a small window with nothing queued is the sender's own rate, not a cap"
    );
    let lossy = QuicPath {
        congestion_events: 1,
        ..quic_at(150_000.0)
    };
    assert_eq!(
        {
            fill(&mut rate, 100);
            rate.on_tick(at + Duration::from_millis(300), lossy)
        },
        PyroVerdict::Budget(budget_of(150_000.0)),
        "congestion since the last judgement: QUIC limits the sender"
    );
    assert_eq!(
        {
            fill(&mut rate, 100);
            rate.on_tick(at + Duration::from_millis(400), lossy)
        },
        PyroVerdict::Budget(target),
        "no new congestion and an empty queue: the cap lets go"
    );
    let unknown = QuicPath {
        cwnd_bytes: 0,
        ..backed_up(150_000.0)
    };
    assert_eq!(
        {
            fill(&mut rate, 100);
            rate.on_tick(at + Duration::from_millis(500), unknown)
        },
        PyroVerdict::Hold,
        "no window known: no cap"
    );
    // From here the encoder gives less than its budget: a desktop under a LAN target.
    let lossier = QuicPath {
        congestion_events: 2,
        ..quic_at(150_000.0)
    };
    assert_eq!(
        rate.on_tick(at + Duration::from_millis(600), lossier),
        PyroVerdict::Hold,
        "a congestion rise with an encoder under its budget: the picture sets the rate"
    );
    assert_eq!(
        rate.on_tick(at + Duration::from_millis(700), backed_up(150_000.0)),
        PyroVerdict::Hold,
        "one judgement of a waiting queue is a burst in flight"
    );
    assert_eq!(
        rate.on_tick(at + Duration::from_millis(800), backed_up(150_000.0)),
        PyroVerdict::Budget(budget_of(150_000.0)),
        "a window waiting for 100 ms caps, whatever the picture's rate"
    );
}

#[test]
fn pyrowave_under_the_floor_for_1_s_steps_down_and_not_before() {
    let on = Instant::now();
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
    let narrow = backed_up(80_000.0);
    let mut seq = 0;
    let first = pyro_send(&mut rate, &mut seq, on);
    let fb = Feedback {
        first_pkt_seq: first,
        ref_us: 0,
        arrivals: (0..40).collect(),
    };
    // The queue counts once it has waited at judgements 100 ms apart.
    rate.on_tick(on, narrow);
    let start = on + Duration::from_millis(100);
    assert_eq!(
        rate.on_feedback(&fb, start, narrow),
        PyroVerdict::Budget(budget_of(80_000.0)),
        "under the floor the budget is still min(target, estimate)"
    );
    // Back over the floor for a moment: the 1 s starts again.
    fill(&mut rate, 500);
    rate.on_tick(start + Duration::from_millis(500), quic_at(8_000_000.0));
    let again = start + Duration::from_millis(600);
    // The queue waits from `again`, so it counts from `again` + 100 ms.
    for ms in (0..=1000).step_by(100) {
        fill(&mut rate, 100);
        assert!(
            !matches!(
                rate.on_tick(again + Duration::from_millis(ms), narrow),
                PyroVerdict::StepDown { .. }
            ),
            "stepped down after {ms} ms under the floor"
        );
    }
    fill(&mut rate, 100);
    assert!(matches!(
        rate.on_tick(again + Duration::from_millis(1100), narrow),
        PyroVerdict::StepDown { estimate_kbps } if estimate_kbps < f64::from(PYRO_FLOOR)
    ));
}

#[test]
fn pyrowave_feedback_silence_does_not_step_down() {
    let on = Instant::now();
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
    let open = quic_at(8_000_000.0);
    let mut seq = 0;
    let first = pyro_send(&mut rate, &mut seq, on);
    let fb = Feedback {
        first_pkt_seq: first,
        ref_us: 0,
        arrivals: (0..40).collect(),
    };
    rate.on_feedback(&fb, on + Duration::from_millis(50), open);
    // Media goes on; feedback never comes back. The standard codec's silence decay would
    // have cut its estimate by now (`feedback_silence_while_media_is_sent_decays_the_estimate`).
    for step in 1..=50u64 {
        let at = on + Duration::from_millis(50 + step * 100);
        pyro_send(&mut rate, &mut seq, at);
        let verdict = rate.on_tick(at, open);
        assert!(
            matches!(verdict, PyroVerdict::Hold | PyroVerdict::Budget(_)),
            "silence stepped down at step {step}"
        );
    }
    assert_eq!(rate.estimator.estimate_kbps, f64::from(PYRO_TARGET));
    assert_eq!(rate.budget, budget_of(f64::from(PYRO_TARGET)));
}

#[test]
fn pyrowave_withheld_feedback_is_judged_on_quic_from_1_s_after_the_switch_on() {
    let on = Instant::now();
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
    let narrow = backed_up(80_000.0);
    let mut stepped = None;
    for ms in (0..=3000u64).step_by(100) {
        fill(&mut rate, 100);
        if let PyroVerdict::StepDown { .. } = rate.on_tick(on + Duration::from_millis(ms), narrow) {
            stepped = Some(ms);
            break;
        }
    }
    assert_eq!(
        stepped,
        Some(2000),
        "the cap from 1 s, then 1 s under the floor"
    );
}

/// Clean feedback for `secs` on a path that carries everything at once, 60 fps frames of
/// `frame_datagrams` each; the verdicts, in order.
fn pyro_clean_run(
    rate: &mut PyroRate,
    on: Instant,
    path: QuicPath,
    frame_datagrams: u32,
    secs: u64,
) -> Vec<PyroVerdict> {
    let mut verdicts = Vec::new();
    let (mut seq, mut unreported) = (0u32, Vec::new());
    let (mut next_frame, mut next_fb, mut next_tick) = (0u64, 50_000u64, 100_000u64);
    let end = secs * 1_000_000;
    while next_frame.min(next_fb).min(next_tick) < end {
        let t = next_frame.min(next_fb).min(next_tick);
        let at = on + Duration::from_micros(t);
        if t == next_frame {
            for i in 0..frame_datagrams {
                let mut d = vec![0u8; SIZE];
                d[1..5].copy_from_slice(&seq.to_le_bytes());
                rate.log.record(&d, at, false);
                // 1 ms one way, then 1 Gbit/s.
                unreported.push((seq, t + 1_000 + u64::from(i) * 9));
                seq += 1;
            }
            next_frame += 16_667;
        } else if t == next_fb {
            next_fb += 50_000;
            let arrived: Vec<_> = unreported
                .iter()
                .filter(|&&(_, a)| a <= t)
                .copied()
                .collect();
            unreported.retain(|&(_, a)| a > t);
            let Some(&(first, ref_us)) = arrived.first() else {
                continue;
            };
            let fb = Feedback {
                first_pkt_seq: first,
                ref_us,
                arrivals: arrived
                    .iter()
                    .map(|&(_, a)| ((a - ref_us) / UNIT_US) as u16)
                    .collect(),
            };
            verdicts.push(rate.on_feedback(&fb, at, path));
        } else {
            next_tick += 100_000;
            verdicts.push(rate.on_tick(at, path));
        }
    }
    verdicts
}

#[test]
fn pyrowave_an_application_limited_sender_with_a_small_window_never_steps_down() {
    let on = Instant::now();
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
    // Quinn's first window (12 000 B) at a 1 ms RTT: 96 Mbit/s, under the floor. A dark
    // screen's frames fit it, so nothing queues and Cubic never grows it.
    let window = QuicPath {
        cwnd_bytes: 12_000,
        ..quic_at(0.0)
    };
    let verdicts = pyro_clean_run(&mut rate, on, window, 8, 10);
    assert!(
        !verdicts
            .iter()
            .any(|v| matches!(v, PyroVerdict::StepDown { .. })),
        "stepped down: {:?}",
        verdicts
            .iter()
            .find(|v| matches!(v, PyroVerdict::StepDown { .. }))
    );
    assert_eq!(rate.budget, budget_of(f64::from(PYRO_TARGET)));
}

/// #1430: the send log is sized for PyroWave's grant: the estimator reads the sent rate over
/// its 0.5 s window, and at 400 Mbit/s that is 25 MB, ~21 800 datagrams, more than a log
/// sized for the standard codec's 100 Mbit/s. A window the log cannot cover reads as
/// application-limited, so the log holds it all.
#[test]
fn pyrowave_send_log_spans_the_rate_window_at_a_400_mbit_grant() {
    let on = Instant::now();
    let mut log = SendLog::new(on);
    // 400 Mbit/s is 50 000 B/ms; a whole datagram is 1 148 B, so 21 778 of them in 0.5 s.
    let count = 21_778usize;
    for seq in 0..count {
        let mut d = vec![0u8; SIZE];
        d[1..5].copy_from_slice(&(seq as u32).to_le_bytes());
        log.record(&d, on + Duration::from_micros(seq as u64 * 23), false);
    }
    assert!(
        LOG >= count,
        "the {count} datagrams of 0.5 s at 400 Mbit/s outgrow the log"
    );
    assert!(
        MAX_ARRIVALS >= count,
        "the rate window's {count} arrivals outgrow the arrivals deque"
    );
    assert_eq!(log.slots.len(), LOG);
    // The whole window is held: the first record was never overwritten by a later one.
    assert!(
        log.slot(0).is_some(),
        "the oldest record of the rate window was overwritten"
    );
}

/// #1430 review: a whole frame arrives as a burst over a window smaller than it, and the
/// 100 ms tick lands on the burst every time. The queue waits at each tick, but the report
/// between ticks shows the window drained it, so it is not the standing queue that caps:
/// the budget must not be cut and the session must not step down.
#[test]
fn pyrowave_a_burst_phase_locked_to_the_tick_is_not_a_standing_queue() {
    let on = Instant::now();
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
    // Quinn's first window at a 1 ms RTT: 12 000 B, 96 Mbit/s, 1/35 of one whole frame at
    // the target. The report 50 ms after a tick sees it drained; the tick sees the next burst.
    let small = quic_at(96_000.0);
    let burst = QuicPath {
        queued_bytes: 407_000, // one whole 60 fps frame at the 200 Mbit/s target
        ..small
    };
    for step in 0..40u64 {
        let at = on + Duration::from_millis(50 + step * 100);
        fill(&mut rate, 50);
        let report = rate.on_feedback(
            &Feedback {
                first_pkt_seq: u32::MAX / 2, // names nothing the log holds: no rate sample
                ref_us: 0,
                arrivals: vec![0],
            },
            at,
            small,
        );
        assert!(
            !matches!(report, PyroVerdict::StepDown { .. }),
            "stepped down from an oscillating queue at step {step}: {report:?}"
        );
        fill(&mut rate, 50);
        // The tick is phase-locked to the burst (a whole frame every 100 ms): waiting now, but
        // the report between ticks showed the queue drained, so this is one burst in flight.
        let tick = rate.on_tick(at + Duration::from_millis(100), burst);
        assert!(
            !matches!(tick, PyroVerdict::StepDown { .. }),
            "the burst at the tick capped and stepped down at step {step}: {tick:?}"
        );
    }
    assert_eq!(rate.budget, budget_of(f64::from(PYRO_TARGET)));
}

#[test]
fn pyrowave_clean_feedback_at_the_target_for_10_s_never_steps_down() {
    let on = Instant::now();
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
    // 200 Mbit/s at 60 fps: 363 full datagrams a frame.
    let verdicts = pyro_clean_run(&mut rate, on, quic_at(8_000_000.0), 363, 10);
    assert!(
        !verdicts
            .iter()
            .any(|v| matches!(v, PyroVerdict::StepDown { .. })),
        "stepped down on a clean link"
    );
    assert!(
        rate.estimator.estimate_kbps >= f64::from(PYRO_FLOOR),
        "estimate {:.0}",
        rate.estimator.estimate_kbps
    );
}

#[test]
fn the_abr_resumes_from_the_estimate_it_is_handed_never_above_its_ceiling() {
    let now = Instant::now();
    let mut est = Estimator::new(CEILING, now);
    est.resume_at(3000.0);
    assert_eq!(est.estimate_kbps(), 3000.0);
    est.resume_at(2.0 * CEILING);
    assert_eq!(est.estimate_kbps(), CEILING);
}

/// A LAN path whose QUIC state answers the budget, judged by ticks alone (feedback withheld),
/// 1 ms steps, 60 fps. Quinn's datagram queue (1 MiB) drains at min(window, link). While more
/// than a window waits, Cubic grows the window 5 % a millisecond up to the link and holds it
/// there with no loss (the link's queue is quinn's own: no delay or loss signal reaches
/// feedback). Otherwise the sender is application-limited and the window holds. The stall
/// guard (`Backlog`) skips frames once the queue has been non-empty for 250 ms, until it
/// empties.
struct QueuedPath {
    link_kbps: f64,
    window_kbps: f64,
    queue: f64,
    events: u64,
    nonempty_since: Option<u64>,
    skipping: bool,
}

impl QueuedPath {
    fn new(link_kbps: f64) -> Self {
        Self {
            link_kbps,
            // Quinn's first window, 12 000 B, at a 1 ms RTT.
            window_kbps: 96_000.0,
            queue: 0.0,
            events: 0,
            nonempty_since: None,
            skipping: false,
        }
    }

    fn cwnd_bytes(&self) -> f64 {
        self.window_kbps * 1000.0 / 8.0 * 0.001
    }

    /// Judge every 100 ms for `secs`; `content_kbps(ms)` is the most the picture gives the
    /// encoder, and a loss at `loss_ms` is one congestion event. With `feedback`, a report every
    /// 50 ms that says nothing (on time, no loss: the link's queue is quinn's own).
    /// (ms, verdict) for each change.
    fn run(
        &mut self,
        rate: &mut PyroRate,
        on: Instant,
        secs: u64,
        content_kbps: impl Fn(u64) -> f64,
        loss_ms: Option<u64>,
        feedback: bool,
    ) -> Vec<(u64, PyroVerdict)> {
        let mut out = Vec::new();
        let mut budget = f64::from(pyro_budget(f64::from(PYRO_TARGET)));
        let mut next_frame_us = 0u64;
        for ms in 0..secs * 1000 {
            if loss_ms == Some(ms) {
                self.events += 1;
                self.window_kbps *= 0.7;
            }
            let reports = feedback && ms % 50 == 25;
            if ms % 100 == 0 || reports {
                let path = QuicPath {
                    rtt: Duration::from_millis(1),
                    cwnd_bytes: self.cwnd_bytes() as u64,
                    congestion_events: self.events,
                    queued_bytes: self.queue as u64,
                };
                let at = on + Duration::from_millis(ms);
                let verdict = if reports {
                    // Names nothing the log holds: no delay, no loss, no rate.
                    let fb = Feedback {
                        first_pkt_seq: u32::MAX / 2,
                        ref_us: 0,
                        arrivals: vec![0],
                    };
                    rate.on_feedback(&fb, at, path)
                } else {
                    rate.on_tick(at, path)
                };
                match verdict {
                    PyroVerdict::Hold => {}
                    v => {
                        if let PyroVerdict::Budget(b) = v {
                            budget = f64::from(b);
                        }
                        out.push((ms, v));
                        if matches!(v, PyroVerdict::StepDown { .. }) {
                            return out;
                        }
                    }
                }
            }
            if next_frame_us < (ms + 1) * 1000 {
                next_frame_us += 16_667;
                let bytes = content_kbps(ms).min(budget) * 1000.0 / 8.0 / 60.0;
                rate.offer(bytes as usize);
                if !self.skipping {
                    self.queue = (self.queue + bytes).min(1024.0 * 1024.0);
                }
            }
            let blocked = self.queue > self.cwnd_bytes();
            self.queue = (self.queue - self.window_kbps.min(self.link_kbps) / 8.0).max(0.0);
            if blocked {
                self.window_kbps = (self.window_kbps * 1.05).min(self.link_kbps);
            }
            if self.queue > 0.0 {
                let since = *self.nonempty_since.get_or_insert(ms);
                self.skipping |= ms - since >= 250;
            } else {
                self.nonempty_since = None;
                self.skipping = false;
            }
        }
        out
    }
}

fn steps_down(verdicts: &[(u64, PyroVerdict)]) -> Option<u64> {
    verdicts
        .iter()
        .find(|(_, v)| matches!(v, PyroVerdict::StepDown { .. }))
        .map(|&(ms, _)| ms)
}

#[test]
fn pyrowave_a_link_under_the_floor_steps_down_with_feedback_withheld_and_the_budget_holds() {
    let on = Instant::now();
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
    let verdicts = QueuedPath::new(80_000.0).run(&mut rate, on, 4, |_| f64::MAX, None, false);
    let stepped = steps_down(&verdicts);
    assert!(
        stepped.is_some_and(|ms| (1900..=2500).contains(&ms)),
        "the cap from 1 s, then 1 s under the floor: {verdicts:?}"
    );
    let floor = pyro_budget(f64::from(PYRO_FLOOR));
    let first_cut = verdicts
        .iter()
        .position(|(_, v)| matches!(v, PyroVerdict::Budget(b) if *b < floor))
        .expect("a budget under the floor");
    assert!(
        !verdicts[first_cut..]
            .iter()
            .any(|(_, v)| matches!(v, PyroVerdict::Budget(b) if *b >= floor)),
        "the budget bounced back once the cut drained the queue: {verdicts:?}"
    );
}

#[test]
fn pyrowave_through_a_responsive_path_a_dark_screen_and_a_single_loss_never_step_down() {
    for loss_ms in [None, Some(3000)] {
        let on = Instant::now();
        let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
        // A dark screen fits quinn's first window: nothing waits on it, and it never grows.
        let verdicts =
            QueuedPath::new(1_000_000.0).run(&mut rate, on, 10, |_| 5_000.0, loss_ms, false);
        assert_eq!(
            steps_down(&verdicts),
            None,
            "loss {loss_ms:?}: {verdicts:?}"
        );
        assert_eq!(rate.budget, pyro_budget(f64::from(PYRO_TARGET)));
    }
}

#[test]
fn pyrowave_a_dark_screen_that_turns_to_full_detail_dips_once_and_does_not_step_down() {
    let on = Instant::now();
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
    let content = |ms: u64| if ms < 3000 { 5_000.0 } else { f64::MAX };
    let verdicts = QueuedPath::new(1_000_000.0).run(&mut rate, on, 10, content, None, false);
    assert_eq!(steps_down(&verdicts), None, "{verdicts:?}");
    let floor = pyro_budget(f64::from(PYRO_FLOOR));
    let dips = verdicts
        .iter()
        .filter(|(_, v)| matches!(v, PyroVerdict::Budget(b) if *b < floor))
        .count();
    assert!(dips <= 1, "{dips} dips: {verdicts:?}");
    assert_eq!(rate.budget, pyro_budget(f64::from(PYRO_TARGET)));
}

/// A desktop gives the encoder 70 Mbit/s under a 200 Mbit/s target on a 60 Mbit/s link: the
/// encoder never fills its budget, yet more than a window waits for good. That is the link.
#[test]
fn pyrowave_a_picture_under_its_budget_on_a_link_under_the_floor_steps_down() {
    // The stall guard gives the media up after this many holds of the queue.
    let give_up_ms = u64::from(GIVE_UP_HOLDS) * super::super::retransmit::WINDOW.as_millis() as u64;
    for feedback in [false, true] {
        let on = Instant::now();
        let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on);
        let verdicts =
            QueuedPath::new(60_000.0).run(&mut rate, on, 6, |_| 70_000.0, None, feedback);
        let stepped = steps_down(&verdicts);
        assert!(
            stepped.is_some_and(|ms| ms <= 2_200 && ms < give_up_ms),
            "feedback {feedback}: {verdicts:?}"
        );
    }
}

/// A video-lane datagram as `packetize` numbers it: channel, then `pkt_seq` (§Channels).
fn video_datagram(pkt_seq: u32) -> bytes::Bytes {
    let mut d = vec![0x01];
    d.extend_from_slice(&pkt_seq.to_le_bytes());
    d.resize(SIZE, 0);
    d.into()
}

fn held(pkt_seq: u32) -> Held {
    Held {
        datagram: video_datagram(pkt_seq),
        logged: true,
        resend: false,
        queued_at: Instant::now(),
    }
}

fn wire_seq(d: &[u8]) -> u32 {
    u32::from_le_bytes(d[1..5].try_into().unwrap())
}

/// QUIC taking a datagram: its wire `pkt_seq` is noted.
fn quic(wire: &mut Vec<u32>, held: Held) -> Handed {
    wire.push(wire_seq(&held.datagram));
    Handed::Sent
}

const BUFFER: usize = 1 << 20;

#[test]
fn audio_leaves_ahead_of_video_over_the_send_budget_and_video_waits() {
    let mut video = VideoQueue::default();
    // 1 Mbit/s: 40 ms of it is 5000 bytes, four video datagrams.
    let room = video.room(Some(1000), BUFFER);
    assert_eq!(room, 5000);
    let mut wire = Vec::new();
    // A keyframe of 40 datagrams lands on a path that has not drained.
    let first = video.next_seq();
    for i in 0..40 {
        video.push(held(first + i), BUFFER);
    }
    assert!(video.feed(room, None, |h| quic(&mut wire, h)));
    // QUIC drains nothing while three audio datagrams arrive.
    let mut audio = Vec::new();
    for _ in 0..3 {
        audio.push(video.take_seq());
        assert!(video.feed(0, None, |h| quic(&mut wire, h)));
    }
    assert_eq!(
        wire,
        [0, 1, 2, 3],
        "only the budget's worth of video is with QUIC"
    );
    assert_eq!(audio, [4, 5, 6], "audio goes next, ahead of the 36 held");
    assert_eq!(video.bytes(), 36 * SIZE);

    // The path drains: the rest follows, numbered after the audio, with no gap.
    assert!(video.feed(usize::MAX, None, |h| quic(&mut wire, h)));
    assert_eq!(wire[4..], (7..43).collect::<Vec<_>>());
    assert!(video.is_empty());
}

#[test]
fn held_video_over_the_cap_drops_oldest_first_and_leaves_no_gap() {
    let mut video = VideoQueue::default();
    for i in 0..5 {
        video.push(held(i), 3 * SIZE);
    }
    assert_eq!((video.bytes(), video.dropped), (3 * SIZE, 2));
    let mut wire = Vec::new();
    assert!(video.feed(usize::MAX, None, |h| quic(&mut wire, h)));
    assert_eq!(
        wire,
        [0, 1, 2],
        "the three newest, numbered from the wire's next"
    );
}

#[test]
fn the_room_is_two_datagrams_at_least_and_half_the_buffer_at_most() {
    let mut video = VideoQueue::default();
    assert_eq!(
        video.room(None, BUFFER),
        BUFFER,
        "never governed: QUIC's whole queue"
    );
    let two = 2 * super::super::media::MAX_DATAGRAM;
    assert_eq!(video.room(Some(100), BUFFER), two);
    assert_eq!(video.room(Some(10_000_000), BUFFER), BUFFER / 2);
    assert_eq!(
        video.room(Some(100), 1000),
        500,
        "a tiny buffer: half, no panic"
    );
}

/// A2/S4: feedback silence releases the governor; what is held then drains at the last
/// governed rate, not into QUIC in one go ahead of audio.
#[test]
fn video_held_when_the_governor_lets_go_drains_at_the_last_governed_rate() {
    let mut video = VideoQueue::default();
    assert_eq!(video.room(Some(1000), BUFFER), 5000);
    for i in 0..40 {
        video.push(held(i), BUFFER);
    }
    assert_eq!(video.room(None, BUFFER), 5000, "released with video held");
    let mut wire = Vec::new();
    assert!(video.feed(usize::MAX, None, |h| quic(&mut wire, h)));
    assert!(video.is_empty());
    assert_eq!(
        video.room(None, BUFFER),
        BUFFER,
        "drained: QUIC's whole queue again"
    );
}

/// A10/S1: a configuration change drops what is held, and the rate it was paced at.
#[test]
fn a_cleared_queue_holds_nothing_and_keeps_its_numbering() {
    let mut video = VideoQueue::default();
    video.room(Some(1000), BUFFER);
    for i in 0..4 {
        video.push(held(i), BUFFER);
    }
    let mut wire = Vec::new();
    assert!(video.feed(SIZE, None, |h| quic(&mut wire, h)));
    video.clear();
    assert!(video.is_empty() && video.bytes() == 0);
    assert_eq!(video.room(None, BUFFER), BUFFER);
    video.push(held(9), BUFFER);
    assert!(video.feed(usize::MAX, None, |h| quic(&mut wire, h)));
    assert_eq!(wire, [0, 1], "no gap across the clear");
}

/// A4: a datagram over the path's size is counted and takes no `pkt_seq`; nor does a resend
/// whose budget is spent.
#[test]
fn a_skipped_datagram_takes_no_pkt_seq_and_too_large_is_counted() {
    let mut video = VideoQueue::default();
    let mut resend = held(1);
    resend.resend = true;
    for h in [held(0), resend, held(2), held(3)] {
        video.push(h, BUFFER);
    }
    let mut verdicts = [
        Handed::TooLarge,
        Handed::TooLarge,
        Handed::Skipped,
        Handed::Sent,
    ]
    .into_iter();
    let mut wire = Vec::new();
    assert!(
        video.feed(usize::MAX, None, |h| match verdicts.next().unwrap() {
            Handed::Sent => quic(&mut wire, h),
            other => other,
        })
    );
    assert_eq!((video.too_large, video.resends_too_large), (1, 1));
    assert_eq!(wire, [0], "the one sent takes the first number");
}

/// M1: a step cut under a deep bottleneck buffer, with video handed to QUIC through the
/// queue and QUIC's governed window (`Window`) in between. The send log stamps the time a
/// datagram was queued, so the estimate comes down as it does with no queue.
#[test]
fn a_step_cut_under_a_deep_buffer_brings_the_estimate_down_through_the_queue() {
    let cut = |queued: bool| {
        let mut s = Sim::new(20_000.0);
        s.queue_bytes = 8.0 * 1024.0 * 1024.0;
        if queued {
            s.quic = Some(Quic::default());
        }
        s.run(10.0);
        assert_eq!(s.est.estimate_kbps(), CEILING, "the ceiling before the cut");
        s.cap_kbps = 2000.0;
        s.run(20.0);
        (s.mean_after(20.0), s.held_max)
    };
    let (direct, _) = cut(false);
    let (queued, held_max) = cut(true);
    eprintln!(
        "estimate 10-20 s after the cut: direct {direct:.0}, through the queue {queued:.0}; \
         most held {} B at {:.2} s",
        held_max.0, held_max.1
    );
    assert!(direct < 2400.0, "no queue: {direct:.0}");
    assert!(queued < 2400.0, "through the queue: {queued:.0}");
}

/// #1433: a 5 Mbit/s link, keyframes once a second, video through the agent's queue and QUIC's
/// governed window. Audio sent at any moment waits behind whatever is in the link's queue
/// then; QUIC unpaced put each keyframe there at once (110-190 ms late on the lab host).
#[test]
fn a_keyframe_waits_in_the_agent_not_in_the_links_queue() {
    let mut s = Sim::new(5000.0);
    s.quic = Some(Quic::default());
    s.keyframe_every = 60;
    s.run(30.0);
    assert!(s.est.found_link(), "the estimate found the 5 Mbit/s link");
    let mut waits: Vec<u64> = s
        .waits
        .iter()
        .filter(|&&(t, _)| t >= 10.0)
        .map(|&(_, w)| w)
        .collect();
    waits.sort_unstable();
    let p95 = waits[waits.len() * 95 / 100] / 1000;
    let max = waits[waits.len() - 1] / 1000;
    let estimate = s.mean_after(10.0);
    eprintln!("link queue after 10 s: p95 {p95} ms, max {max} ms; estimate {estimate:.0} kbps");
    assert!(p95 <= 35 && max <= 45, "p95 {p95} ms, max {max} ms");
    assert!(estimate > 3500.0, "pacing kept the estimate: {estimate:.0}");
}

#[test]
fn the_pacer_lets_video_go_at_its_rate_in_short_bursts_and_never_holds_video_100_ms() {
    let t0 = Instant::now();
    let ms = |n: u64| t0 + Duration::from_millis(n);
    // The datagrams the queue has handed QUIC after `pace(kbps, at)`.
    let send = |video: &mut VideoQueue, kbps, at| {
        let room = video.pace(kbps, at);
        let mut wire = Vec::new();
        assert!(video.feed(room, None, |h| quic(&mut wire, h)));
        40 - video.bytes() / SIZE
    };
    let keyframe = || {
        let mut video = VideoQueue::default();
        let first = video.next_seq();
        for i in 0..40 {
            video.push(held(first + i), BUFFER);
        }
        video
    };
    let mut video = keyframe();
    // 8 Mbit/s × 1.25 is 1250 bytes a millisecond; a burst is 5 ms of it.
    assert_eq!(send(&mut video, Some(8000), ms(0)), 5, "one burst at once");
    assert_eq!(
        send(&mut video, Some(8000), ms(1)),
        6,
        "then 1250 bytes a ms"
    );
    assert_eq!(send(&mut video, Some(8000), ms(3)), 8);
    assert_eq!(
        send(&mut video, Some(8000), ms(100)),
        13,
        "idle time saves one burst"
    );
    assert_eq!(
        send(&mut video, None, ms(101)),
        40,
        "not governed: not paced"
    );

    let mut video = keyframe();
    assert_eq!(send(&mut video, Some(8000), ms(0)), 5);
    assert_eq!(
        send(&mut video, Some(8000), ms(101)),
        40,
        "video that waited 100 ms goes at once"
    );
}
