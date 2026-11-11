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
//! - **AIMD** on that signal: overuse cuts to 0.85 × the link's throughput; normal grows
//!   8 % a second, never past 1.5 × the receive rate. The throughput is the highest receive
//!   rate of the last 20 feedback windows in which the sender was not application-limited
//!   (as BBR's delivery-rate filter): a quiet scene, or the gap a delay spike leaves in the
//!   arrivals, measures the content or the stall, not the link (#1316). Overuse that
//!   outlasts a spike with no such window cuts from the receive rate, so a quiet sender
//!   still follows a link that carries less.
//! - **Loss.** Over 10 % scales by (1 − loss/2); under 2 % lets it grow.
//! - The estimate is the least of the two and of the ceiling. QUIC's own window follows it
//!   (`Window`), so it is no bound here.
//! - Feedback that stops while media is being sent decays it.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use base64::Engine;
use quinn_proto::RttEstimator;
use serde_json::Value;
use web_transport_quinn::quinn::congestion::{Controller, ControllerFactory, CubicConfig};

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
/// No group grows past this much arrival time (libwebrtc `kMaxBurstDuration`).
const MAX_BURST_US: u64 = 100_000;

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
/// The overuse cut's base is the highest of this many throughput samples: 0.4–2 s of busy
/// feedback (§Feedback's 20–100 ms). An app-limited stretch ages none of them.
const THROUGHPUT_SAMPLES: usize = 20;
/// Overuse held this long, with no throughput sample for as long, cuts from the receive
/// rate: longer than a delay spike's stall, shorter than a full queue takes to fill.
const SUSTAINED_OVERUSE: Duration = Duration::from_millis(500);
/// Overusing messages this close together are one episode: at the browser's 50 ms cadence
/// (`FEEDBACK_MS` in `web/src/webtransport/feedback.ts`) a `Normal` of up to 200 ms does not
/// restart the hold. Half the hold, so a detector that flickers cannot hide a link that
/// dropped.
const OVERUSE_GAP: Duration = Duration::from_millis(250);
/// A window whose datagrams were sent under this share of the estimate is
/// application-limited, and is no throughput sample.
const APP_LIMITED: f64 = 0.8;
const MAX_ARRIVALS: usize = 1 << 14;

/// Feedback silence while media is sent: past max(this, 2 intervals + RTT), the estimate
/// decays by `SILENCE_DECAY` every `SILENCE_STEP`.
const SILENCE_MIN: Duration = Duration::from_millis(500);
const SILENCE_DECAY: f64 = 0.8;
const SILENCE_STEP: Duration = Duration::from_millis(500);

/// No estimate below this: a floor the governor's own floor always sits above.
const MIN_KBPS: f64 = 150.0;
/// The ABR is fed at most this often, about as often as `rtpgccbwe` notifies.
pub(crate) const REPORT_EVERY: Duration = Duration::from_millis(100);
/// A bound flip or a silence decay is logged at most this often: a bound can flip per
/// message, a decay steps per tick.
const LOG_EVERY: Duration = Duration::from_secs(1);

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

/// When, and how big, each recent agent → client video or FEC datagram was sent, by its `pkt_seq`,
/// and what feedback has said of it: each is counted once, whatever is reported again. A
/// `pkt_seq` never recorded (audio shares the counter) is skipped when feedback names it.
pub(crate) struct SendLog {
    start: Instant,
    slots: Vec<Slot>,
    /// The first send since the last feedback: the silence clock starts here, not at the
    /// feedback, or a frame after a still screen would be judged before its answer can come.
    unanswered_since: Option<Instant>,
}

impl SendLog {
    pub(crate) fn new(start: Instant) -> Self {
        Self {
            start,
            slots: vec![Slot::default(); LOG],
            unanswered_since: None,
        }
    }

    /// `pkt_seq` is bytes 1..5 of every datagram (§Channels). A `resend` (a NACK answer)
    /// counts toward loss, not toward the receive rate.
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
        self.unanswered_since.get_or_insert(at);
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

/// Which bound the estimate sits at; silence decays the delay and loss bounds.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Bound {
    Ceiling,
    Delay,
    Loss,
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
    first_arrival: u64,
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
                first_arrival: arrival,
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
            && (arrival - cur.last_arrival) < send.saturating_sub(cur.last_send)
            && arrival.saturating_sub(cur.first_arrival) < MAX_BURST_US;
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
            first_arrival: arrival,
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
    /// Received datagrams that are not resends: (client arrival µs, bytes, agent send µs).
    arrivals: VecDeque<(u64, u32, u64)>,
    newest_arrival: u64,
    /// (agent instant, receive rate) of recent windows that were not application-limited.
    throughput: VecDeque<(Instant, f64)>,
    overusing_since: Option<Instant>,
    last_overusing: Option<Instant>,
    /// Per message: (agent instant, received, lost); a late arrival after a loss is a
    /// correction (+1, −1).
    losses: VecDeque<(Instant, i64, i64)>,
    last_update: Instant,
    last_decrease: Option<Instant>,
    last_loss_decrease: Option<Instant>,
    last_report: Option<Instant>,
    last_feedback: Option<Instant>,
    last_decay: Option<Instant>,
    loss: Option<f64>,
    bound: Bound,
    /// The bound the log last named, and when.
    logged: (Bound, Option<Instant>),
    /// The estimate before a silence decay not logged yet; delay and loss cuts always log.
    unlogged: Option<f64>,
}

/// What one message measured, for the tests.
#[derive(Debug, Clone, Copy, PartialEq)]
pub(crate) struct Measured {
    pub(crate) usage: Usage,
    pub(crate) rate_kbps: Option<f64>,
    /// The rate every video datagram sent over the same span was sent at, lost ones
    /// included, on the agent's clock; `None` when the message added no fresh arrival.
    pub(crate) sent_kbps: Option<f64>,
    /// A video datagram (not a resend) first reported here is in the rate window.
    pub(crate) fresh: bool,
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
            throughput: VecDeque::new(),
            overusing_since: None,
            last_overusing: None,
            losses: VecDeque::new(),
            last_update: now,
            last_decrease: None,
            last_loss_decrease: None,
            last_report: None,
            last_feedback: None,
            last_decay: None,
            loss: None,
            bound: Bound::Ceiling,
            logged: (Bound::Ceiling, None),
            unlogged: None,
        }
    }

    #[cfg(test)]
    pub(crate) fn estimate_kbps(&self) -> f64 {
        self.estimate_kbps
    }

    /// The loss fraction the last message's window measured; `None` on too few datagrams.
    /// Lost repair datagrams count: they crossed the same path.
    pub(crate) fn loss(&self) -> Option<f64> {
        self.loss
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
        log.unanswered_since = None;
        let m = self.measure(fb, log, now);
        self.apply(m, now, path)
    }

    /// Act on what one message measured.
    fn apply(&mut self, m: Measured, now: Instant, path: QuicPath) -> Option<f64> {
        self.loss = m.loss;
        let dt = now
            .saturating_duration_since(self.last_update)
            .as_secs_f64()
            .min(1.0);
        self.last_update = now;
        let app_limited = m
            .sent_kbps
            .is_none_or(|s| s < APP_LIMITED * self.estimate_kbps);
        if let Some(r) = m.rate_kbps.filter(|_| !app_limited) {
            self.throughput.push_back((now, r));
            if self.throughput.len() > THROUGHPUT_SAMPLES {
                self.throughput.pop_front();
            }
        }
        let in_episode = self
            .last_overusing
            .is_some_and(|at| now.saturating_duration_since(at) <= OVERUSE_GAP);
        if !in_episode || m.usage == Usage::Underusing {
            self.overusing_since = None;
        }
        let overusing_for = match m.usage {
            Usage::Overusing => {
                self.last_overusing = Some(now);
                now.saturating_duration_since(*self.overusing_since.get_or_insert(now))
            }
            _ => Duration::ZERO,
        };
        // Overuse that outlasts a spike while the sender is quiet: what the link delivers.
        let quiet_overuse = overusing_for >= SUSTAINED_OVERUSE
            && self
                .throughput
                .back()
                .is_none_or(|&(at, _)| now.saturating_duration_since(at) >= SUSTAINED_OVERUSE);
        // Nothing measured while busy: one cut from the ceiling, never compounding.
        let base = m
            .rate_kbps
            .filter(|_| quiet_overuse && m.fresh)
            .or(self.throughput.iter().map(|&(_, r)| r).reduce(f64::max))
            .unwrap_or(self.ceiling_kbps);
        let mut cut_by = Vec::new();

        match m.usage {
            Usage::Overusing => {
                let reduce_every = path.rtt.clamp(REDUCE_MIN, REDUCE_MAX);
                if self
                    .last_decrease
                    .is_none_or(|at| now.saturating_duration_since(at) >= reduce_every)
                {
                    let cut = BETA * base;
                    if cut < self.delay_kbps {
                        self.delay_kbps = cut;
                        cut_by.push("delay");
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
                    cut_by.push("loss");
                    // These losses are spent; the next decision reads new ones.
                    self.losses.clear();
                }
            }
            Some(loss) if loss >= LOSS_LOW => {}
            _ => self.loss_kbps *= GROWTH_PER_S.powf(dt),
        }

        let was = self.estimate_kbps;
        self.settle();
        let due = self.log_due(now) && (self.unlogged.is_some() || self.bound != self.logged.0);
        if !cut_by.is_empty() || due {
            // A silence fall not logged yet is part of this line's "from → to".
            let from = match self.unlogged.take() {
                Some(from) => {
                    cut_by.insert(0, "silence");
                    from
                }
                None => was,
            };
            self.logged = (self.bound, Some(now));
            let cut = if cut_by.is_empty() {
                "none".to_string()
            } else {
                cut_by.join("+")
            };
            tracing::info!(
                token = "webtransport-cc-bound",
                bound = ?self.bound,
                cut = cut.as_str(),
                usage = ?m.usage,
                recv_kbps = m.rate_kbps.unwrap_or(0.0) as u64,
                sent_kbps = m.sent_kbps.unwrap_or(0.0) as u64,
                app_limited,
                base_kbps = base as u64,
                loss_pct = m.loss.unwrap_or(0.0) * 100.0,
                delay_kbps = self.delay_kbps as u64,
                loss_kbps = self.loss_kbps as u64,
                rtt_ms = path.rtt.as_millis() as u64,
                cwnd_bytes = path.cwnd_bytes,
                quic_congestion_events = path.congestion_events,
                "WebTransport estimate {from:.0} → {:.0} kbps",
                self.estimate_kbps
            );
        }
        self.due(now)
    }

    /// Called on the agent's own clock between messages. Media sent and no feedback for
    /// longer than the client's interval allows, counted from the first send it has not
    /// answered, is congestion, and decays the estimate.
    /// A client that never sent feedback is not judged (§Feedback lets it omit it).
    pub(crate) fn on_tick(&mut self, now: Instant, log: &SendLog, rtt: Duration) -> Option<f64> {
        self.last_feedback?;
        let sent = log.unanswered_since?;
        let timeout = SILENCE_MIN.max(FEEDBACK_INTERVAL * 2 + rtt);
        if now.saturating_duration_since(sent) <= timeout {
            return None;
        }
        let from = self.last_decay.unwrap_or(sent + timeout);
        let steps = now.saturating_duration_since(from).as_secs_f64() / SILENCE_STEP.as_secs_f64();
        let factor = SILENCE_DECAY.powf(steps);
        self.delay_kbps *= factor;
        self.loss_kbps *= factor;
        let was = self.estimate_kbps;
        self.settle();
        self.last_decay = Some(now);
        if self.estimate_kbps < was {
            self.unlogged.get_or_insert(was);
        }
        if let Some(from) = self.unlogged.filter(|_| self.log_due(now)) {
            self.unlogged = None;
            self.logged = (self.bound, Some(now));
            tracing::info!(
                token = "webtransport-cc-bound",
                bound = ?self.bound,
                cut = "silence",
                silent_ms = now.saturating_duration_since(sent).as_millis() as u64,
                rtt_ms = rtt.as_millis() as u64,
                "WebTransport estimate {from:.0} → {:.0} kbps: no feedback for media sent",
                self.estimate_kbps
            );
        }
        self.due(now)
    }

    fn log_due(&self, now: Instant) -> bool {
        self.logged
            .1
            .is_none_or(|at| now.saturating_duration_since(at) >= LOG_EVERY)
    }

    fn settle(&mut self) {
        let clamp = |k: f64| k.clamp(MIN_KBPS, self.ceiling_kbps);
        self.delay_kbps = clamp(self.delay_kbps);
        self.loss_kbps = clamp(self.loss_kbps);
        let (kbps, bound) = [
            (self.delay_kbps, Bound::Delay),
            (self.loss_kbps, Bound::Loss),
        ]
        .into_iter()
        .fold((self.ceiling_kbps, Bound::Ceiling), |a, b| {
            if b.0 < a.0 {
                b
            } else {
                a
            }
        });
        self.estimate_kbps = clamp(kbps);
        self.bound = bound;
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
        let mut newest_fresh = None;
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
                self.arrivals
                    .push_back((at, u32::from(slot.bytes), slot.sent_us));
                self.newest_arrival = self.newest_arrival.max(at);
                newest_fresh = newest_fresh.max(Some(at));
                if self.arrivals.len() > MAX_ARRIVALS {
                    self.arrivals.pop_front();
                }
            }
        }

        let newest = self.newest_arrival;
        let in_window = |t: u64| t.saturating_add(RATE_WINDOW_US) >= newest;
        self.arrivals.retain(|&(t, _, _)| in_window(t));
        let fresh = newest_fresh.is_some_and(in_window);
        let bytes: u64 = self.arrivals.iter().map(|&(_, b, _)| u64::from(b)).sum();
        // N datagrams span N − 1 gaps: the first one's bytes came before the span began.
        let rate = |key: fn(&(u64, u32, u64)) -> u64, min_span: u64| {
            let first = self.arrivals.iter().min_by_key(|a| key(a))?;
            let last = self.arrivals.iter().map(key).max()?;
            let span = last - key(first);
            (span >= min_span)
                .then(|| (bytes - u64::from(first.1)) as f64 * 8.0 * 1000.0 / span as f64)
        };
        let rate_kbps = rate(|a| a.0, MIN_RATE_SPAN_US);
        // Over every datagram sent in the span, lost ones too: a busy sender under loss is
        // still busy. Datagrams all sent in one instant give no send rate.
        let first_sent = self.arrivals.iter().map(|a| a.2).min();
        let last_sent = self.arrivals.iter().map(|a| a.2).max();
        let sent_kbps = match (fresh, rate_kbps, first_sent, last_sent) {
            (true, Some(_), Some(lo), Some(hi)) if hi > lo => {
                let sent: u64 = log
                    .slots
                    .iter()
                    .filter(|s| s.bytes > 0 && !s.resend && s.sent_us > lo && s.sent_us <= hi)
                    .map(|s| u64::from(s.bytes))
                    .sum();
                Some(sent as f64 * 8.0 * 1000.0 / (hi - lo) as f64)
            }
            _ => None,
        };

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
            sent_kbps,
            fresh,
            loss,
        }
    }
}

/// QUIC's window over the estimate's rate × RTT: a keyframe burst and RTT jitter fit under
/// it, and quinn paces at 1.25 × window / RTT, 2.5 × the rate.
const WINDOW_GAIN: f64 = 2.0;
/// A LAN's RTT would make the window a few datagrams, and a keyframe wait behind them.
const WINDOW_RTT_MIN: Duration = Duration::from_millis(100);

/// One connection's QUIC congestion window. Until the estimate reaches the ABR, and while
/// PyroWave is active, it is quinn's Cubic: a client that sends no feedback follows QUIC
/// congestion control alone (§Feedback). Once the estimate governs the encoder, the window
/// follows it and no loss shrinks it: Cubic read packets reordered or late from jitter, and
/// 0.1 % random loss, as congestion, and held video to a few Mbit/s while the datagrams over
/// its window waited or were dropped in the send queue (#1330). The estimate is then the
/// only controller, as `rtpgccbwe` is on WebRTC.
pub(crate) struct Window {
    cubic: Box<dyn Controller>,
    /// The rate the encoder is held to, bit/s; 0 while the estimate does not govern it.
    governed: Arc<AtomicU64>,
    srtt: Duration,
}

/// Builds a connection's [`Window`]; its handle is the connection's [`Governor`].
pub(crate) struct WindowFactory(pub(crate) Governor);

/// What sets a connection's window: the estimate, once it governs the encoder.
#[derive(Clone, Default)]
pub(crate) struct Governor(Arc<AtomicU64>);

impl Governor {
    pub(crate) fn govern(&self, kbps: f64) {
        self.0.store((kbps * 1000.0) as u64, Ordering::Relaxed);
    }

    /// Back to Cubic.
    pub(crate) fn release(&self) {
        self.0.store(0, Ordering::Relaxed);
    }
}

impl ControllerFactory for WindowFactory {
    fn build(self: Arc<Self>, now: Instant, current_mtu: u16) -> Box<dyn Controller> {
        Box::new(Window {
            cubic: Arc::new(CubicConfig::default()).build(now, current_mtu),
            governed: self.0 .0.clone(),
            srtt: Duration::ZERO,
        })
    }
}

impl Controller for Window {
    fn on_sent(&mut self, now: Instant, bytes: u64, last_packet_number: u64) {
        self.cubic.on_sent(now, bytes, last_packet_number);
    }

    fn on_ack(
        &mut self,
        now: Instant,
        sent: Instant,
        bytes: u64,
        app_limited: bool,
        rtt: &RttEstimator,
    ) {
        self.srtt = rtt.get();
        self.cubic.on_ack(now, sent, bytes, app_limited, rtt);
    }

    fn on_end_acks(
        &mut self,
        now: Instant,
        in_flight: u64,
        app_limited: bool,
        largest_packet_num_acked: Option<u64>,
    ) {
        self.cubic
            .on_end_acks(now, in_flight, app_limited, largest_packet_num_acked);
    }

    fn on_congestion_event(
        &mut self,
        now: Instant,
        sent: Instant,
        is_persistent_congestion: bool,
        lost_bytes: u64,
    ) {
        self.cubic
            .on_congestion_event(now, sent, is_persistent_congestion, lost_bytes);
    }

    fn on_mtu_update(&mut self, new_mtu: u16) {
        self.cubic.on_mtu_update(new_mtu);
    }

    fn window(&self) -> u64 {
        match self.governed.load(Ordering::Relaxed) {
            0 => self.cubic.window(),
            bps => {
                let rtt = self.srtt.max(WINDOW_RTT_MIN).as_secs_f64();
                ((WINDOW_GAIN * bps as f64 / 8.0 * rtt) as u64).max(self.cubic.initial_window())
            }
        }
    }

    fn clone_box(&self) -> Box<dyn Controller> {
        Box::new(Window {
            cubic: self.cubic.clone_box(),
            governed: self.governed.clone(),
            srtt: self.srtt,
        })
    }

    fn initial_window(&self) -> u64 {
        self.cubic.initial_window()
    }

    fn into_any(self: Box<Self>) -> Box<dyn std::any::Any> {
        self
    }
}

/// What to do with the next video unit while QUIC's datagram queue is backed up.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Admit {
    Send,
    Skip,
    /// Skip, and ask the encoder for a keyframe to resume on.
    SkipForKeyframe,
}

/// Bytes in QUIC's datagram queue under one fragment: nothing is waiting.
const BACKLOG_EMPTY: usize = super::media::MAX_DATAGRAM;

/// Video behind a closed window (a stalled path) is worthless once it is older than the
/// client's NACK hold, and quinn's queue holds a MiB of it, dropping only the oldest
/// datagram when full. A queue that has not shrunk for that long is stalled: units are
/// skipped until it empties and a keyframe comes, so it never holds more than the hold's
/// worth. A keyframe being paced out shrinks it, so it is never mistaken for a stall.
#[derive(Default)]
pub(crate) struct Backlog {
    last: usize,
    growing_since: Option<Instant>,
    skipping: bool,
    asked: bool,
    pub(crate) skipped: u64,
}

impl Backlog {
    /// `queued`: bytes in QUIC's datagram queue now; `keyframe`: the unit can be decoded
    /// alone; `hold`: how long video stays worth sending.
    pub(crate) fn admit(
        &mut self,
        queued: usize,
        keyframe: bool,
        now: Instant,
        hold: Duration,
    ) -> Admit {
        let empty = queued < BACKLOG_EMPTY;
        if empty || queued < self.last {
            self.growing_since = None;
        } else {
            self.growing_since.get_or_insert(now);
        }
        self.last = queued;
        if !self.skipping {
            let since = match self.growing_since {
                Some(at) if now.saturating_duration_since(at) > hold => at,
                _ => return Admit::Send,
            };
            tracing::info!(
                token = "webtransport-video-stalled",
                queued,
                stalled_ms = now.saturating_duration_since(since).as_millis() as u64,
                "WebTransport datagram queue has not drained; video skipped until it does"
            );
            self.skipping = true;
            self.asked = false;
        }
        if empty && keyframe {
            tracing::info!(
                token = "webtransport-video-resumed",
                skipped = self.skipped,
                "WebTransport datagram queue drained; video resumes at a keyframe"
            );
            self.skipping = false;
            return Admit::Send;
        }
        self.skipped += 1;
        if empty && !std::mem::replace(&mut self.asked, true) {
            Admit::SkipForKeyframe
        } else {
            Admit::Skip
        }
    }
}

#[cfg(test)]
mod tests;
