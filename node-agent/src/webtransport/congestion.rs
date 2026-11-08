//! Receiver feedback into an available-rate estimate
//! (`protocol/transport-webtransport.md` §Feedback), built from the parts of Google
//! Congestion Control (draft-ietf-rmcat-gcc-02) that `rtpgccbwe` runs on WebRTC, with
//! libwebrtc's constants:
//!
//! - **Delay.** Datagrams are grouped by send burst; each group's inter-group delay
//!   variation feeds a trendline (the slope of the smoothed accumulated delay over 20
//!   groups) compared with an adaptive threshold. Only differences on one clock are used,
//!   so a clock offset cancels and drift is a slope far under the threshold; a route step
//!   or a standing queue is a level, not a slope.
//! - **AIMD** on that signal: overuse cuts to 0.85 × the receive rate; normal grows 8 % a
//!   second, never past 1.5 × the receive rate.
//! - **Loss.** Over 10 % scales by (1 − loss/2); under 2 % lets it grow.
//! - The estimate is the least of the two, of 1.5 × QUIC's own cwnd/RTT once QUIC has
//!   seen congestion, and of the ceiling.
//! - Feedback that stops while media is being sent decays it.

use std::collections::VecDeque;
use std::time::{Duration, Instant};

use base64::Engine;
use serde_json::Value;

/// §Feedback: `count` is at most 8192; arrivals are in 50 µs units, `0xFFFF` not received.
const MAX_COUNT: usize = 8192;
const UNIT_US: u64 = 50;
const NOT_RECEIVED: u16 = 0xFFFF;
/// The client's cadence (§Feedback).
const FEEDBACK_INTERVAL: Duration = Duration::from_millis(50);

/// Send records kept, by `pkt_seq`: over a second at 100 Mbit/s.
const LOG: usize = 1 << 14;

// Packet groups (libwebrtc `InterArrival`): one send burst, plus what arrived in a burst
// behind it.
const GROUP_US: u64 = 5_000;
const BURST_US: u64 = 5_000;

// Trendline and adaptive threshold (libwebrtc `TrendlineEstimator`).
const WINDOW: usize = 20;
const SMOOTHING: f64 = 0.9;
const GAIN: f64 = 4.0;
const MIN_DELTAS: f64 = 60.0;
const OVERUSE_TIME_MS: f64 = 10.0;
const K_UP: f64 = 0.0087;
const K_DOWN: f64 = 0.039;
const MAX_ADAPT_OFFSET_MS: f64 = 15.0;
const MAX_ADAPT_STEP_MS: f64 = 100.0;
const THRESHOLD_INIT_MS: f64 = 12.5;
const THRESHOLD_MIN_MS: f64 = 6.0;
const THRESHOLD_MAX_MS: f64 = 600.0;

// AIMD (libwebrtc `AimdRateControl`).
const BETA: f64 = 0.85;
const GROWTH_PER_S: f64 = 1.08;
const RECV_CAP: f64 = 1.5;
const RECV_CAP_ADD_KBPS: f64 = 10.0;
const REDUCE_MIN: Duration = Duration::from_millis(10);
const REDUCE_MAX: Duration = Duration::from_millis(200);

// Loss (libwebrtc `SendSideBandwidthEstimation`).
const LOSS_LOW: f64 = 0.02;
const LOSS_HIGH: f64 = 0.10;
const LOSS_DECREASE_GAP: Duration = Duration::from_millis(300);
const LOSS_WINDOW: Duration = Duration::from_millis(500);
/// Fewer datagrams than this in the window decide nothing about loss.
const LOSS_MIN_SAMPLES: i64 = 20;
const MAX_LOSS_ENTRIES: usize = 64;

/// The receive rate is measured over this much of the client's arrival clock.
const RATE_WINDOW_US: u64 = 500_000;
/// A rate measured over less than this is noise; the last good one stands in.
const MIN_RATE_SPAN_US: u64 = 100_000;
const MAX_ARRIVALS: usize = 1 << 14;

/// QUIC's own estimate bounds ours with this headroom. An exact cap would deadlock: once
/// the ABR sends under cwnd/RTT, QUIC is application-limited and its window stops growing.
const QUIC_CAP: f64 = 1.5;

/// Feedback silence while media is sent: past max(this, 2 intervals + RTT), the estimate
/// decays by `SILENCE_DECAY` every `SILENCE_STEP`.
const SILENCE_MIN: Duration = Duration::from_millis(500);
const SILENCE_DECAY: f64 = 0.8;
const SILENCE_STEP: Duration = Duration::from_millis(500);

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

/// The message, or which field is out of contract (§Feedback).
pub(crate) fn feedback_of(message: &Value) -> Result<Feedback, &'static str> {
    let uint = |name: &'static str| message.get(name).and_then(Value::as_u64).ok_or(name);
    uint("seq")?;
    let first_pkt_seq = u32::try_from(uint("first_pkt_seq")?).map_err(|_| "first_pkt_seq")?;
    let count = uint("count")?;
    if count == 0 || count > MAX_COUNT as u64 {
        return Err("count");
    }
    let ref_us = uint("ref_us")?;
    // The latest arrival a message can name must not overflow the clock.
    ref_us
        .checked_add(u64::from(NOT_RECEIVED - 1) * UNIT_US)
        .ok_or("ref_us")?;
    uint("recv_kbps")?;
    if uint("lost")? > count {
        return Err("lost");
    }
    uint("interval_ms")?;
    let bytes = message
        .get("arrivals_b64")
        .and_then(Value::as_str)
        .and_then(|b| base64::engine::general_purpose::STANDARD.decode(b).ok())
        .filter(|b| b.len() as u64 == count * 2)
        .ok_or("arrivals_b64")?;
    let arrivals = (0..count as usize)
        .map(|i| u16::from_le_bytes([bytes[2 * i], bytes[2 * i + 1]]))
        .collect();
    Ok(Feedback {
        first_pkt_seq,
        ref_us,
        arrivals,
    })
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
enum Status {
    #[default]
    Unreported,
    Lost,
    Received,
}

/// `bytes == 0` is an empty slot.
#[derive(Debug, Clone, Copy, Default)]
struct Slot {
    seq: u32,
    sent_us: u64,
    bytes: u16,
    resend: bool,
    status: Status,
}

/// When, and how big, each recent agent → client datagram was sent, by its `pkt_seq`, and
/// what feedback has said of it: each is counted once, whatever is reported again.
pub(crate) struct SendLog {
    start: Instant,
    slots: Vec<Slot>,
    last_sent: Option<Instant>,
}

impl SendLog {
    pub(crate) fn new(start: Instant) -> Self {
        Self {
            start,
            slots: vec![Slot::default(); LOG],
            last_sent: None,
        }
    }

    /// Any channel's datagram: `pkt_seq` is bytes 1..5 of every one (§Channels). A
    /// `resend` (a NACK answer) counts toward loss, not toward the receive rate.
    pub(crate) fn record(&mut self, datagram: &[u8], at: Instant, resend: bool) {
        let Some(seq) = datagram.get(1..5) else {
            return;
        };
        let seq = u32::from_le_bytes([seq[0], seq[1], seq[2], seq[3]]);
        self.slots[seq as usize % LOG] = Slot {
            seq,
            sent_us: at.saturating_duration_since(self.start).as_micros() as u64,
            bytes: datagram.len().min(u16::MAX as usize) as u16,
            resend,
            status: Status::Unreported,
        };
        self.last_sent = Some(at);
    }

    fn slot(&mut self, seq: u32) -> Option<&mut Slot> {
        Some(&mut self.slots[seq as usize % LOG]).filter(|s| s.seq == seq && s.bytes > 0)
    }
}

/// What QUIC knows of the path, read from the connection when feedback arrives.
#[derive(Debug, Clone, Copy)]
pub(crate) struct QuicPath {
    pub(crate) rtt: Duration,
    pub(crate) cwnd_bytes: u64,
    pub(crate) congestion_events: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Usage {
    Normal,
    Overusing,
    Underusing,
}

/// One send burst: µs on the agent's clock and on the client's.
#[derive(Debug, Clone, Copy)]
struct Group {
    first_send: u64,
    last_send: u64,
    last_arrival: u64,
}

/// Groups datagrams by send burst and yields each completed group's deltas from the one
/// before it: (send Δ ms, arrival Δ ms, the group's arrival ms).
#[derive(Default)]
struct InterArrival {
    current: Option<Group>,
    previous: Option<Group>,
}

impl InterArrival {
    fn push(&mut self, send: u64, arrival: u64) -> Option<(f64, f64, f64)> {
        let Some(cur) = self.current.as_mut() else {
            self.current = Some(Group {
                first_send: send,
                last_send: send,
                last_arrival: arrival,
            });
            return None;
        };
        if send < cur.first_send {
            return None; // older than the burst being built: reordered
        }
        let in_send_burst = send - cur.first_send <= GROUP_US;
        // Arrived back to back behind the group, sooner than it was sent: queued behind it.
        let in_arrival_burst = arrival >= cur.last_arrival
            && arrival - cur.last_arrival < BURST_US
            && (arrival - cur.last_arrival) < send.saturating_sub(cur.last_send);
        if in_send_burst || in_arrival_burst {
            cur.last_send = cur.last_send.max(send);
            cur.last_arrival = cur.last_arrival.max(arrival);
            return None;
        }
        let done = *cur;
        let out = self.previous.and_then(|prev| {
            (done.last_arrival >= prev.last_arrival).then(|| {
                (
                    (done.last_send - prev.last_send) as f64 / 1000.0,
                    (done.last_arrival - prev.last_arrival) as f64 / 1000.0,
                    done.last_arrival as f64 / 1000.0,
                )
            })
        });
        self.previous = Some(done);
        self.current = Some(Group {
            first_send: send,
            last_send: send,
            last_arrival: arrival,
        });
        out
    }
}

/// The delay-gradient detector: trendline over smoothed accumulated delay, and the
/// adaptive threshold it is compared with.
struct Trendline {
    history: VecDeque<(f64, f64)>,
    first_arrival_ms: Option<f64>,
    accumulated_ms: f64,
    smoothed_ms: f64,
    deltas: u32,
    prev_trend: f64,
    threshold_ms: f64,
    last_threshold_ms: Option<f64>,
    overuse_ms: Option<f64>,
    overuse_count: u32,
    usage: Usage,
}

impl Default for Trendline {
    fn default() -> Self {
        Self {
            history: VecDeque::with_capacity(WINDOW + 1),
            first_arrival_ms: None,
            accumulated_ms: 0.0,
            smoothed_ms: 0.0,
            deltas: 0,
            prev_trend: 0.0,
            threshold_ms: THRESHOLD_INIT_MS,
            last_threshold_ms: None,
            overuse_ms: None,
            overuse_count: 0,
            usage: Usage::Normal,
        }
    }
}

impl Trendline {
    fn update(&mut self, send_delta_ms: f64, arrival_delta_ms: f64, arrival_ms: f64) {
        self.deltas = (self.deltas + 1).min(1000);
        let first = *self.first_arrival_ms.get_or_insert(arrival_ms);
        self.accumulated_ms += arrival_delta_ms - send_delta_ms;
        self.smoothed_ms = SMOOTHING * self.smoothed_ms + (1.0 - SMOOTHING) * self.accumulated_ms;
        self.history
            .push_back((arrival_ms - first, self.smoothed_ms));
        if self.history.len() > WINDOW {
            self.history.pop_front();
        }
        let trend = if self.history.len() == WINDOW {
            slope(&self.history).unwrap_or(self.prev_trend)
        } else {
            self.prev_trend
        };
        self.detect(trend, send_delta_ms, arrival_ms);
    }

    fn detect(&mut self, trend: f64, send_delta_ms: f64, now_ms: f64) {
        if self.deltas < 2 {
            self.usage = Usage::Normal;
            return;
        }
        let modified = (self.deltas as f64).min(MIN_DELTAS) * trend * GAIN;
        if modified > self.threshold_ms {
            let over = match self.overuse_ms {
                None => send_delta_ms / 2.0,
                Some(t) => t + send_delta_ms,
            };
            self.overuse_ms = Some(over);
            self.overuse_count += 1;
            if over > OVERUSE_TIME_MS && self.overuse_count > 1 && trend >= self.prev_trend {
                self.overuse_ms = Some(0.0);
                self.overuse_count = 0;
                self.usage = Usage::Overusing;
            }
        } else if modified < -self.threshold_ms {
            self.overuse_ms = None;
            self.overuse_count = 0;
            self.usage = Usage::Underusing;
        } else {
            self.overuse_ms = None;
            self.overuse_count = 0;
            self.usage = Usage::Normal;
        }
        self.prev_trend = trend;
        self.adapt_threshold(modified, now_ms);
    }

    fn adapt_threshold(&mut self, modified: f64, now_ms: f64) {
        let last = *self.last_threshold_ms.get_or_insert(now_ms);
        self.last_threshold_ms = Some(now_ms);
        if modified.abs() > self.threshold_ms + MAX_ADAPT_OFFSET_MS {
            return;
        }
        let k = if modified.abs() < self.threshold_ms {
            K_DOWN
        } else {
            K_UP
        };
        let step = (now_ms - last).clamp(0.0, MAX_ADAPT_STEP_MS);
        self.threshold_ms = (self.threshold_ms + k * (modified.abs() - self.threshold_ms) * step)
            .clamp(THRESHOLD_MIN_MS, THRESHOLD_MAX_MS);
    }
}

/// Least-squares slope of `y` over `x`.
fn slope(points: &VecDeque<(f64, f64)>) -> Option<f64> {
    let n = points.len() as f64;
    let (sx, sy) = points
        .iter()
        .fold((0.0, 0.0), |(a, b), &(x, y)| (a + x, b + y));
    let (mx, my) = (sx / n, sy / n);
    let (num, den) = points.iter().fold((0.0, 0.0), |(num, den), &(x, y)| {
        (num + (x - mx) * (y - my), den + (x - mx) * (x - mx))
    });
    (den != 0.0).then(|| num / den)
}

/// The rate estimate one client's feedback drives, in kbit/s.
pub(crate) struct Estimator {
    ceiling_kbps: f64,
    delay_kbps: f64,
    loss_kbps: f64,
    estimate_kbps: f64,
    inter_arrival: InterArrival,
    trendline: Trendline,
    /// Received datagrams that are not resends: (client arrival µs, bytes).
    arrivals: VecDeque<(u64, u32)>,
    newest_arrival: u64,
    last_rate_kbps: Option<f64>,
    /// Per message: (agent instant, received, lost); a late arrival after a loss is a
    /// correction (+1, −1).
    losses: VecDeque<(Instant, i64, i64)>,
    last_update: Instant,
    last_decrease: Option<Instant>,
    last_loss_decrease: Option<Instant>,
    last_report: Option<Instant>,
    last_feedback: Option<Instant>,
    last_decay: Option<Instant>,
    quic_cap_kbps: Option<f64>,
}

/// What one message measured, for the tests.
#[derive(Debug, Clone, Copy, PartialEq)]
pub(crate) struct Measured {
    pub(crate) usage: Usage,
    pub(crate) rate_kbps: Option<f64>,
    pub(crate) loss: Option<f64>,
}

impl Estimator {
    /// Starts at the ceiling, as `rtpgccbwe` is seeded: it only walks down on evidence.
    pub(crate) fn new(ceiling_kbps: f64, now: Instant) -> Self {
        Self {
            ceiling_kbps,
            delay_kbps: ceiling_kbps,
            loss_kbps: ceiling_kbps,
            estimate_kbps: ceiling_kbps,
            inter_arrival: InterArrival::default(),
            trendline: Trendline::default(),
            arrivals: VecDeque::new(),
            newest_arrival: 0,
            last_rate_kbps: None,
            losses: VecDeque::new(),
            last_update: now,
            last_decrease: None,
            last_loss_decrease: None,
            last_report: None,
            last_feedback: None,
            last_decay: None,
            quic_cap_kbps: None,
        }
    }

    #[cfg(test)]
    pub(crate) fn estimate_kbps(&self) -> f64 {
        self.estimate_kbps
    }

    /// Take one message; `Some(estimate)` when it is time to feed the ABR.
    pub(crate) fn on_feedback(
        &mut self,
        fb: &Feedback,
        log: &mut SendLog,
        now: Instant,
        path: QuicPath,
    ) -> Option<f64> {
        self.last_feedback = Some(now);
        self.last_decay = None;
        let m = self.measure(fb, log, now);
        let dt = now
            .saturating_duration_since(self.last_update)
            .as_secs_f64()
            .min(1.0);
        self.last_update = now;
        if let Some(r) = m.rate_kbps {
            self.last_rate_kbps = Some(r);
        }

        match m.usage {
            Usage::Overusing => {
                let reduce_every = path.rtt.clamp(REDUCE_MIN, REDUCE_MAX);
                if self
                    .last_decrease
                    .is_none_or(|at| now.saturating_duration_since(at) >= reduce_every)
                {
                    // Sparse arrivals give no rate: the last one, else the estimate, still cuts.
                    let base = m.rate_kbps.or(self.last_rate_kbps);
                    let cut = BETA * base.unwrap_or(self.estimate_kbps);
                    if cut < self.delay_kbps {
                        tracing::debug!(
                            token = "webtransport-cc-overuse",
                            "WebTransport path overused: delay estimate {:.0} → {cut:.0} kbps",
                            self.delay_kbps
                        );
                        self.delay_kbps = cut;
                    }
                    self.last_decrease = Some(now);
                }
            }
            Usage::Underusing => {}
            Usage::Normal => {
                let mut grown = self.delay_kbps * GROWTH_PER_S.powf(dt);
                if let Some(cap) = m.rate_kbps.map(|r| RECV_CAP * r + RECV_CAP_ADD_KBPS) {
                    if grown > cap {
                        grown = self.delay_kbps.max(cap);
                    }
                }
                self.delay_kbps = grown;
            }
        }

        match m.loss {
            Some(loss) if loss > LOSS_HIGH => {
                let gap = LOSS_DECREASE_GAP + path.rtt;
                if self
                    .last_loss_decrease
                    .is_none_or(|at| now.saturating_duration_since(at) >= gap)
                {
                    self.loss_kbps = self.estimate_kbps * (1.0 - 0.5 * loss);
                    self.last_loss_decrease = Some(now);
                    // These losses are spent; the next decision reads new ones.
                    self.losses.clear();
                }
            }
            Some(loss) if loss >= LOSS_LOW => {}
            _ => self.loss_kbps *= GROWTH_PER_S.powf(dt),
        }

        self.quic_cap_kbps = (path.congestion_events > 0).then(|| {
            let rtt_ms = path.rtt.as_secs_f64().max(0.001) * 1000.0;
            QUIC_CAP * path.cwnd_bytes as f64 * 8.0 / rtt_ms
        });
        self.settle();
        self.due(now)
    }

    /// Called on the agent's own clock between messages. Media sent and no feedback for
    /// longer than the client's interval allows is congestion, and decays the estimate.
    /// A client that never sent feedback is not judged (§Feedback lets it omit it).
    pub(crate) fn on_tick(&mut self, now: Instant, log: &SendLog, rtt: Duration) -> Option<f64> {
        let fb_at = self.last_feedback?;
        let timeout = SILENCE_MIN.max(FEEDBACK_INTERVAL * 2 + rtt);
        let silent = now.saturating_duration_since(fb_at) > timeout;
        if !silent || log.last_sent.is_none_or(|s| s <= fb_at) {
            return None;
        }
        let from = self.last_decay.unwrap_or(fb_at + timeout);
        let steps = now.saturating_duration_since(from).as_secs_f64() / SILENCE_STEP.as_secs_f64();
        let factor = SILENCE_DECAY.powf(steps);
        self.delay_kbps *= factor;
        self.loss_kbps *= factor;
        self.last_decay = Some(now);
        self.settle();
        self.due(now)
    }

    fn settle(&mut self) {
        let clamp = |k: f64| k.clamp(MIN_KBPS, self.ceiling_kbps);
        self.delay_kbps = clamp(self.delay_kbps);
        self.loss_kbps = clamp(self.loss_kbps);
        let quic = self.quic_cap_kbps.unwrap_or(f64::MAX);
        self.estimate_kbps = clamp(self.delay_kbps.min(self.loss_kbps).min(quic));
    }

    fn due(&mut self, now: Instant) -> Option<f64> {
        let due = self
            .last_report
            .is_none_or(|at| now.saturating_duration_since(at) >= REPORT_EVERY);
        due.then(|| {
            self.last_report = Some(now);
            self.estimate_kbps
        })
    }

    /// Count each datagram the message reports for the first time, feed the delay detector
    /// in `pkt_seq` (send) order, and read the windowed receive rate and loss.
    pub(crate) fn measure(&mut self, fb: &Feedback, log: &mut SendLog, now: Instant) -> Measured {
        let (mut received, mut lost) = (0i64, 0i64);
        for (i, &arrival) in fb.arrivals.iter().enumerate() {
            let seq = fb.first_pkt_seq.wrapping_add(i as u32);
            let Some(slot) = log.slot(seq) else {
                continue;
            };
            let at_us = (arrival != NOT_RECEIVED)
                .then(|| fb.ref_us.checked_add(u64::from(arrival) * UNIT_US))
                .flatten();
            match (slot.status, at_us) {
                (Status::Received, _) | (Status::Lost, None) => continue,
                (Status::Unreported, None) => {
                    slot.status = Status::Lost;
                    lost += 1;
                    continue;
                }
                (Status::Lost, Some(_)) => {
                    // Late: the loss is undone, but it is no delay sample.
                    slot.status = Status::Received;
                    lost -= 1;
                    received += 1;
                }
                (Status::Unreported, Some(at)) => {
                    slot.status = Status::Received;
                    received += 1;
                    if let Some((send_d, arrival_d, arrival_ms)) =
                        self.inter_arrival.push(slot.sent_us, at)
                    {
                        self.trendline.update(send_d, arrival_d, arrival_ms);
                    }
                }
            }
            if let (false, Some(at)) = (slot.resend, at_us) {
                self.arrivals.push_back((at, u32::from(slot.bytes)));
                self.newest_arrival = self.newest_arrival.max(at);
                if self.arrivals.len() > MAX_ARRIVALS {
                    self.arrivals.pop_front();
                }
            }
        }

        let newest = self.newest_arrival;
        self.arrivals
            .retain(|&(t, _)| t.saturating_add(RATE_WINDOW_US) >= newest);
        // N datagrams span N − 1 gaps: the oldest one's bytes arrived before the span began.
        let (oldest, first_bytes) = self
            .arrivals
            .iter()
            .min_by_key(|&&(t, _)| t)
            .copied()
            .unwrap_or((newest, 0));
        let span = newest - oldest;
        let rate_kbps = (span >= MIN_RATE_SPAN_US).then(|| {
            let bytes: u64 = self.arrivals.iter().map(|&(_, b)| u64::from(b)).sum();
            (bytes - u64::from(first_bytes)) as f64 * 8.0 * 1000.0 / span as f64
        });

        if received != 0 || lost != 0 {
            self.losses.push_back((now, received, lost));
            if self.losses.len() > MAX_LOSS_ENTRIES {
                self.losses.pop_front();
            }
        }
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
            .fold((0i64, 0i64), |(r, l), &(_, a, b)| (r + a, l + b));
        let (r, l) = (r.max(0), l.max(0));
        let loss = (r + l >= LOSS_MIN_SAMPLES).then(|| l as f64 / (r + l) as f64);

        Measured {
            usage: self.trendline.usage,
            rate_kbps,
            loss,
        }
    }
}

#[cfg(test)]
mod tests;
