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
    /// Every `n`th datagram is dropped on the way (0: none).
    drop_every: u32,
    /// Datagrams the full bottleneck queue dropped.
    queue_drops: u32,
    /// (`pkt_seq`, arrival on the agent's clock, or dropped), in send order.
    pending: VecDeque<(u32, Option<u64>)>,
    /// (seconds, estimate) after each message.
    trace: Vec<(f64, f64)>,
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
            drop_every: 0,
            queue_drops: 0,
            pending: VecDeque::new(),
            trace: Vec::new(),
        }
    }

    fn run(&mut self, secs: f64) {
        let end = self.t + (secs * 1e6) as u64;
        let mut next_frame = self.t;
        let mut next_feedback = self.t + 50_000;
        while self.t < end {
            if next_frame <= next_feedback {
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

    fn frame(&mut self) {
        let kbps = self
            .send_kbps
            .unwrap_or(self.est.estimate_kbps())
            .min((self.content_kbps)(self.t));
        let bytes = kbps * 1000.0 / 8.0 / 60.0;
        let n = (bytes / SIZE as f64).ceil().max(1.0) as u32;
        let service = (SIZE as f64 * 8.0 * 1000.0 / self.cap_kbps) as u64;
        for _ in 0..n {
            let mut d = vec![0u8; SIZE];
            d[1..5].copy_from_slice(&self.seq.to_le_bytes());
            self.log
                .record(&d, self.start + Duration::from_micros(self.t), false);
            let backlog = self.free_at.saturating_sub(self.t) as f64 / service as f64 * SIZE as f64;
            let dropped = self.drop_every > 0 && self.seq.is_multiple_of(self.drop_every);
            let full = backlog + SIZE as f64 > self.queue_bytes;
            self.queue_drops += u32::from(full);
            let arrival = (!dropped && !full).then(|| {
                self.free_at = self.free_at.max(self.t) + service;
                // One path, first in first out: a delay spike holds back what follows it.
                self.last_arrival = self
                    .last_arrival
                    .max(self.free_at + 10_000 + (self.extra_us)(self.t));
                self.last_arrival
            });
            self.pending.push_back((self.seq, arrival));
            self.seq = self.seq.wrapping_add(1);
        }
    }

    fn client_us(&self, agent_us: u64) -> u64 {
        self.offset_us + (agent_us as f64 * (1.0 + self.drift)) as u64
    }

    fn feedback(&mut self) {
        let now = self.t;
        let Some(last) = self
            .pending
            .iter()
            .rposition(|&(_, a)| a.is_some_and(|a| a <= now))
        else {
            return;
        };
        let batch: Vec<_> = self.pending.drain(..=last).collect();
        let ref_us = batch
            .iter()
            .filter_map(|&(_, a)| a)
            .map(|a| self.client_us(a))
            .min()
            .unwrap();
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
fn quic_bounds_the_estimate_only_once_it_has_seen_congestion() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    let fb = Feedback {
        first_pkt_seq: 0,
        ref_us: 0,
        arrivals: vec![],
    };
    let slow_start = QuicPath {
        rtt: RTT,
        cwnd_bytes: 5000,
        congestion_events: 0,
    };
    est.on_feedback(&fb, &mut log, start, slow_start);
    assert_eq!(
        est.estimate_kbps(),
        CEILING,
        "a window still in slow start is no evidence"
    );
    let congested = QuicPath {
        congestion_events: 1,
        ..slow_start
    };
    est.on_feedback(&fb, &mut log, start, congested);
    // 1.5 × 5000 B × 8 / 20 ms.
    assert_eq!(est.estimate_kbps(), 3000.0);
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
    assert_eq!(decays, fed_abr.len(), "every decay is logged: {lines:?}");
    let e = s.est.estimate_kbps();
    // Step 30 is 2.4 s past that timeout: 0.8 every 500 ms.
    let decayed = fed * 0.8f64.powf(4.8);
    assert!(
        (e - decayed).abs() < 0.05 * fed,
        "3 s of silence: {fed:.0} → {e:.0}"
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
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    est.delay_kbps = 3000.0;
    est.settle();
    let empty = Feedback {
        first_pkt_seq: 0,
        ref_us: 0,
        arrivals: vec![],
    };
    // QUIC's cap alternates between 2880 and 3600 kbps around the delay bound, for 2 s.
    let lines = logged(|| {
        for i in 0..40u32 {
            let path = QuicPath {
                rtt: RTT,
                cwnd_bytes: if i % 2 == 0 { 4800 } else { 6000 },
                congestion_events: 1,
            };
            est.on_feedback(
                &empty,
                &mut log,
                start + 50 * Duration::from_millis(i.into()),
                path,
            );
        }
    });
    assert!((1..=3).contains(&lines.len()), "{} lines", lines.len());
}

#[test]
fn each_quic_congestion_event_that_lowers_the_estimate_is_logged() {
    let start = Instant::now();
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    let empty = Feedback {
        first_pkt_seq: 0,
        ref_us: 0,
        arrivals: vec![],
    };
    let lines = logged(|| {
        for (events, cwnd) in [(1, 5000), (2, 4000), (3, 3000)] {
            let path = QuicPath {
                rtt: RTT,
                cwnd_bytes: cwnd,
                congestion_events: events,
            };
            let at = start + Duration::from_millis(50 * events);
            est.on_feedback(&empty, &mut log, at, path);
        }
    });
    let cuts = lines.iter().filter(|l| l.contains("cut=\"quic\"")).count();
    assert_eq!(cuts, 3, "{lines:?}");
    assert_eq!(est.estimate_kbps(), 1800.0);
}
