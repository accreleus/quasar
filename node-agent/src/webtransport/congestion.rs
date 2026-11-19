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
//! - **Outage regrow.** An estimate a silence decay took under what the link carried before
//!   it grows back toward that rate at `RECOVERY_GROWTH_PER_S`, so a session that lost ten
//!   seconds of feedback gets its bitrate back in seconds rather than AIMD's minute. The
//!   memory is the stretch after an outage only: it is dropped when the rate is reached,
//!   `RECOVERY_HOLD` after the last decay, on a codec switch, and at the first delay or loss
//!   cut — the link has the last word, and the estimate is already above what it delivers by
//!   the time that cut comes.
//! - **Loss.** Over 10 % scales by (1 − loss/2); under 2 % lets it grow.
//! - The estimate is the least of the two and of the ceiling. QUIC's own window follows it
//!   (`Window`), so it is no bound here.
//! - Feedback that stops while media is being sent decays it.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use base64::Engine;
use quinn_proto::RttEstimator;
use serde_json::Value;
use web_transport_quinn::quinn::congestion::{Controller, ControllerFactory, CubicConfig};

use crate::session::abr;

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
/// Growth while the estimate climbs back toward what the link carried before a silence decay
/// (#1417). AIMD's 8 % a second needs about a minute to regain a 20 Mbit/s session from the
/// floor after a 10 s outage; this reaches the same rate in a few seconds.
const RECOVERY_GROWTH_PER_S: f64 = 2.0;
/// How long the pre-outage rate is remembered after the last silence decay of an episode.
/// It is a memory of the stretch right after an outage, not a standing target: a still screen
/// that never gets back to it leaves nothing behind for busy content minutes later (#1417).
const RECOVERY_HOLD: Duration = Duration::from_secs(20);
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
/// The least one-way delay in the rate window this far over the least of the last
/// [`FLOOR_WINDOW_US`] is a standing queue: the link holds the sender back
/// ([`Estimator::measured`]). A rate gate cannot tell a link a wire 1 % over fills from a
/// clean one, which measures within 1 %, and that queue grows for good (#1496); a queue
/// that keeps growing crosses any line.
const QUEUE_HELD_US: i64 = 30_000;
/// The delay floor is the least one-way delay over this much send time, in steps of
/// [`FLOOR_STEP_US`]: long enough that a wire 0.4 % over the link crosses
/// [`QUEUE_HELD_US`] before its queue ages into the floor, short enough that a path whose
/// delay rose (a roam) stops reading as a queue.
/// shortcut: a wire under 0.4 % over the link is never seen; raise this if one shows.
const FLOOR_WINDOW_US: u64 = 10_000_000;
const FLOOR_STEP_US: u64 = 1_000_000;
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
    /// Reported not received while an outage's own losses were exempt: no loss was counted,
    /// so a late arrival of it has none to undo (#1557).
    Outage,
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
    /// That send's `pkt_seq`. The outage mark is taken in send order, not by stamp: held
    /// video goes out stamped with its queue time, before a report it was sent after.
    unanswered_seq: Option<u32>,
    /// The `pkt_seq` after the last one recorded.
    next_seq: u32,
    /// The last stamp, µs: stamps never go back in `pkt_seq` (send) order, whatever stamped
    /// them (a paced release, a queue time, a resend), or the delay detector reads a send
    /// that went back in time.
    last_us: u64,
}

impl SendLog {
    pub(crate) fn new(start: Instant) -> Self {
        Self {
            start,
            slots: vec![Slot::default(); LOG],
            unanswered_since: None,
            unanswered_seq: None,
            next_seq: 0,
            last_us: 0,
        }
    }

    /// `pkt_seq` is bytes 1..5 of every datagram (§Channels). A `resend` (a NACK answer)
    /// counts toward loss, not toward the receive rate.
    pub(crate) fn record(&mut self, datagram: &[u8], at: Instant, resend: bool) {
        let Some(seq) = datagram.get(1..5) else {
            return;
        };
        let seq = u32::from_le_bytes([seq[0], seq[1], seq[2], seq[3]]);
        self.last_us = self
            .last_us
            .max(at.saturating_duration_since(self.start).as_micros() as u64);
        self.slots[seq as usize % LOG] = Slot {
            seq,
            sent_us: self.last_us,
            bytes: datagram.len().min(u16::MAX as usize) as u16,
            resend,
            status: Status::Unreported,
        };
        self.unanswered_since.get_or_insert(at);
        self.unanswered_seq.get_or_insert(seq);
        self.next_seq = seq.wrapping_add(1);
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
    /// Bytes waiting in quinn's datagram queue: QUIC is holding the sender back.
    pub(crate) queued_bytes: u64,
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
        // A group sent before the one before it is no delay sample. An equal send is one: a
        // frame's datagrams share a stamp, and a delay spike can split them across groups.
        let out = self.previous.and_then(|prev| {
            let send_d = done.last_send.checked_sub(prev.last_send)?;
            (done.last_arrival >= prev.last_arrival).then(|| {
                (
                    send_d as f64 / 1000.0,
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
    /// What the last message showed, see [`Self::measured`].
    measured: Option<abr::Held>,
    /// (start of a [`FLOOR_STEP_US`] of send time, the least one-way delay sent in it, µs).
    floor: VecDeque<(u64, i64)>,
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
    /// What the link carried when the current silence decay began, and when that memory stops
    /// being used (`RECOVERY_HOLD` after the last decay): the estimate grows back toward it
    /// faster than AIMD. Dropped when it is reached, when it expires, when the link disagrees
    /// with a delay or loss cut, and on a codec switch (#1417).
    recover_to: Option<(f64, Instant)>,
    /// While a detected silence is an outage: its losses are not congestion (#1557).
    outage: Option<Outage>,
    /// The `pkt_seq` after the newest any report has covered.
    reported_to: Option<u32>,
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
    /// The received datagrams' arrival rate over the rate they were sent at: under 1, the link
    /// queued them; loss does not lower it.
    pub(crate) delivery: Option<f64>,
    /// How far the least one-way delay in the rate window is over the delay floor, µs.
    pub(crate) queue_us: Option<i64>,
    /// When the window's first datagram was sent.
    pub(crate) since: Option<Instant>,
    /// A video datagram (not a resend) first reported here is in the rate window.
    pub(crate) fresh: bool,
    pub(crate) loss: Option<f64>,
}

/// A detected outage's losses (#1557): those reported not received from `from`, the first
/// datagram no report had covered, until one sent after detection (`by`) is received. Not
/// from the first send after the last report: when only host → client drops, the client's
/// report of what came before the cut arrives after it (#1496 lab round). Not ended by a
/// datagram received after a hole either: reordering puts holes in that late report.
#[derive(Debug, Clone, Copy)]
struct Outage {
    from: u32,
    by: u32,
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
            recover_to: None,
            outage: None,
            reported_to: None,
            logged: (Bound::Ceiling, None),
            unlogged: None,
            measured: None,
            floor: VecDeque::new(),
        }
    }

    pub(crate) fn estimate_kbps(&self) -> f64 {
        self.estimate_kbps
    }

    /// The link set the estimate: a delay or loss cut holds it under the ceiling. A silence
    /// decay does not count while its regrow memory is held (#1417): the link has not said it
    /// carries less, and pacing a recovery keyframe at a decayed rate only delays it.
    pub(crate) fn found_link(&self) -> bool {
        self.bound != Bound::Ceiling && self.recover_to.is_none()
    }

    /// Start again from `kbps` (never above the ceiling): codec-pyrowave.md §Rate, the ABR
    /// resumes from the estimate after a switch back.
    pub(crate) fn resume_at(&mut self, kbps: f64) {
        self.delay_kbps = kbps;
        self.loss_kbps = kbps;
        self.stop_regrow("codec switch");
        self.settle();
    }

    /// The loss fraction the last message's window measured; `None` on too few datagrams.
    /// Lost repair datagrams count: they crossed the same path.
    pub(crate) fn loss(&self) -> Option<f64> {
        self.loss
    }

    /// What the last message showed, when the link held the sender back (a standing queue,
    /// [`QUEUE_HELD_US`], and datagrams arriving slower than they were sent): `BETA` (the
    /// delay cut's own drain margin) × that share of the send rate, of the video sent from
    /// `since`. `None` on a clean link, whatever the delay detector says (#68), under random
    /// loss, which leaves the share at 1, and when no rate was measured. A sender under its
    /// estimate counts: a wire 1 % over the link fires no delay cut, so the estimate stays at
    /// the ceiling while the queue grows (#1496). The ABR's down path reads only this (#1376).
    pub(crate) fn measured(&self) -> Option<abr::Held> {
        self.measured
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
        log.unanswered_seq = None;
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
        if self.recover_to.is_some_and(|(_, until)| now > until) {
            self.stop_regrow("expired");
        }
        // Under what the link carried before a silence decay: grow back toward it at
        // `RECOVERY_GROWTH_PER_S`, and no further than it (#1417).
        let recover = self
            .recover_to
            .filter(|&(to, _)| to > self.estimate_kbps)
            .map(|(to, _)| to);
        let growth = if recover.is_some() {
            RECOVERY_GROWTH_PER_S
        } else {
            GROWTH_PER_S
        }
        .powf(dt);
        // What the link is delivering bounds the growth of BOTH bounds, as it does AIMD's
        // (libwebrtc's 1.5 x the receive rate): a link that came back slower holds the bound
        // that is not binding it as well as the one that is.
        let cap = m.rate_kbps.map(|r| RECV_CAP * r + RECV_CAP_ADD_KBPS);
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
                        // The link no longer carries what it did before the outage.
                        self.stop_regrow("delay cut");
                    }
                    self.last_decrease = Some(now);
                }
            }
            Usage::Underusing => {}
            Usage::Normal => {
                let mut grown = self.delay_kbps * growth;
                if let Some(to) = recover {
                    grown = grown.min(to.max(self.delay_kbps));
                }
                if let Some(cap) = cap {
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
                    // The link no longer carries what it did before the outage.
                    self.stop_regrow("loss cut");
                    // These losses are spent; the next decision reads new ones.
                    self.losses.clear();
                }
            }
            Some(loss) if loss >= LOSS_LOW => {}
            _ => {
                let mut grown = self.loss_kbps * growth;
                if let Some(to) = recover {
                    grown = grown.min(to.max(self.loss_kbps));
                }
                if let Some(cap) = cap {
                    if grown > cap {
                        grown = self.loss_kbps.max(cap);
                    }
                }
                self.loss_kbps = grown;
            }
        }

        let was = self.estimate_kbps;
        self.settle();
        if self
            .recover_to
            .is_some_and(|(to, _)| self.estimate_kbps >= to)
        {
            self.stop_regrow("reached");
        }
        self.measured = m
            .delivery
            .zip(m.queue_us)
            .zip(m.since)
            .filter(|&((d, q), _)| d < 1.0 && q > QUEUE_HELD_US)
            .map(|((d, _), since)| abr::Held {
                share: BETA * d,
                since,
            });
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
        // The silence is an outage (#1557).
        if let Some(seq) = log.unanswered_seq {
            self.outage.get_or_insert(Outage {
                from: self.reported_to.unwrap_or(seq),
                by: log.next_seq,
            });
        }
        let from = self.last_decay.unwrap_or(sent + timeout);
        let steps = now.saturating_duration_since(from).as_secs_f64() / SILENCE_STEP.as_secs_f64();
        let factor = SILENCE_DECAY.powf(steps);
        // The first decay of an episode remembers what the link carried; the memory is good
        // for `RECOVERY_HOLD` after the last decay, so an outage of any length keeps it and
        // a still screen afterwards lets it go (#1417).
        let to = match self.recover_to {
            Some((to, _)) => to,
            None => {
                let to = self.estimate_kbps;
                tracing::info!(
                    token = "webtransport-cc-regrow",
                    to_kbps = to as u64,
                    "WebTransport estimate will regrow toward {to:.0} kbps once feedback returns"
                );
                to
            }
        };
        self.recover_to = Some((to, now + RECOVERY_HOLD));
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

    /// Drop the pre-outage memory, with the one log line that closes the fast regrow: it was
    /// reached, it expired, a codec switch replaced it, or the link said it no longer carries
    /// that rate.
    fn stop_regrow(&mut self, reason: &'static str) {
        if let Some((to, _)) = self.recover_to.take() {
            tracing::info!(
                token = "webtransport-cc-regrow",
                to_kbps = to as u64,
                reason,
                "WebTransport estimate regrow toward {to:.0} kbps ended: {reason}"
            );
        }
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
        // At or after `from`, in u32 serial arithmetic.
        let after = |seq: u32, from: u32| seq.wrapping_sub(from) < 1 << 31;
        let end = fb.first_pkt_seq.wrapping_add(fb.arrivals.len() as u32);
        if self.reported_to.is_none_or(|to| after(end, to)) {
            self.reported_to = Some(end);
        }
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
                (Status::Received, _) | (Status::Lost, None) | (Status::Outage, None) => continue,
                (Status::Outage, Some(_)) => {
                    // Late: no loss was counted for it, so there is none to undo; it is no
                    // delay sample either.
                    slot.status = Status::Received;
                    received += 1;
                }
                (Status::Unreported, None) => {
                    if self.outage.is_some_and(|o| after(seq, o.from)) {
                        slot.status = Status::Outage;
                    } else {
                        slot.status = Status::Lost;
                        lost += 1;
                    }
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
                    // The link answered: a loss sent after this is congestion (#1557).
                    if self.outage.is_some_and(|o| after(seq, o.by)) {
                        self.outage = None;
                    }
                    if let Some((send_d, arrival_d, arrival_ms)) =
                        self.inter_arrival.push(slot.sent_us, at)
                    {
                        self.trendline.update(send_d, arrival_d, arrival_ms);
                    }
                }
            }
            if let (false, Some(at)) = (slot.resend, at_us) {
                let delay = at as i64 - slot.sent_us as i64;
                match self.floor.back_mut() {
                    Some((from, least)) if slot.sent_us < *from + FLOOR_STEP_US => {
                        *least = (*least).min(delay);
                    }
                    _ => self.floor.push_back((slot.sent_us, delay)),
                }
                while self
                    .floor
                    .front()
                    .is_some_and(|&(from, _)| from + FLOOR_WINDOW_US < slot.sent_us)
                {
                    self.floor.pop_front();
                }
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
        let delivery = rate_kbps.zip(rate(|a| a.2, 1)).map(|(r, s)| r / s);
        let floor = self.floor.iter().map(|&(_, d)| d).min();
        let queue_us = self
            .arrivals
            .iter()
            .map(|&(at, _, sent)| at as i64 - sent as i64)
            .min()
            .zip(floor)
            .map(|(window, floor)| window - floor);
        // Over every datagram sent in the span, lost ones too: a busy sender under loss is
        // still busy. Datagrams all sent in one instant give no send rate.
        let first_sent = self.arrivals.iter().map(|a| a.2).min();
        let since = first_sent.map(|us| log.start + Duration::from_micros(us));
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
            delivery,
            queue_us,
            since,
            fresh,
            loss,
        }
    }
}

/// codec-pyrowave.md §Rate: the 1 s the estimate may sit under `floor_kbps`, and the latest
/// the QUIC cap and that timer start after a switch on.
const PYRO_STEP_DOWN_AFTER: Duration = Duration::from_secs(1);
/// The shortest span the encoder's fill of its budget is read over (6 frames at 60 fps), and
/// that a waiting queue must last to count by itself.
const PYRO_FILL_WINDOW: Duration = Duration::from_millis(100);
/// What of a full PyroWave datagram is the encoder's bytes: §Rate's estimate is "less the
/// agent's datagram overhead".
const PYRO_PAYLOAD_SHARE: f64 =
    (super::media::MAX_DATAGRAM - super::media::HEADER - super::pyrowave::HEADER) as f64
        / super::media::MAX_DATAGRAM as f64;

/// codec-pyrowave.md §Rate: the encoder's `bitrate` for an estimate, kbit/s, less the
/// agent's datagram overhead. The one place the budget is computed, the launch value too.
pub(crate) fn pyro_budget(kbps: f64) -> u32 {
    (kbps * PYRO_PAYLOAD_SHARE) as u32
}

/// What [`PyroRate`] asks of the session.
#[derive(Debug, Clone, Copy, PartialEq)]
pub(crate) enum PyroVerdict {
    Hold,
    /// The encoder's new `bitrate`, kbit/s; it budgets each frame at this over the fps.
    Budget(u32),
    /// The estimate stayed under the floor for 1 s: switch off, reason `congestion`.
    StepDown {
        estimate_kbps: f64,
    },
}

/// PyroWave's per-session rate controller (codec-pyrowave.md §Rate), one per switch on: the
/// estimate is seeded at the target and never above it, follows `feedback`, and is capped by
/// what the QUIC connection carries from `min(first feedback, switch on + 1 s)`. Feedback
/// silence does not decay it, as on the standard codec it never takes the encoder under
/// the floor; QUIC's cap still applies, so withholding feedback cannot postpone a step-down.
///
/// The cap counts only while QUIC limits the sender. More than a window of datagrams waiting
/// at consecutive judgements over at least 100 ms is the link limiting us, whatever the
/// picture's rate; one judgement of it is a burst in flight. The weaker signals, congestion
/// since the last judgement and holding the latch, count only while the encoder fills the
/// budget asked of it: a picture that gives less (dark, still) sets the rate, not QUIC, and
/// quinn's Cubic does not grow its window for an application-limited sender, so window over
/// RTT is then what was sent, not what the link carries. Once a waiting queue has put the
/// capped estimate under the floor the cap stays latched until the capped estimate is back at
/// or over the floor, or the encoder stops filling its budget: cutting the budget drains the
/// queue, and our own reaction is not the link recovering. Feedback never raises the estimate.
pub(crate) struct PyroRate {
    estimator: Estimator,
    pub(crate) log: SendLog,
    target_kbps: f64,
    floor_kbps: f64,
    on: Instant,
    /// When the QUIC cap and the floor timer started.
    judged_from: Option<Instant>,
    below_since: Option<Instant>,
    /// The `bitrate` last asked for.
    budget: u32,
    /// QUIC's congestion events at the last judgement.
    events: Option<u64>,
    /// A waiting queue put the reading under the floor: the cap holds until it is back over.
    latched: bool,
    /// Since when more than a window has waited, at every judgement.
    waiting_since: Option<Instant>,
    /// Encoder bytes handed to the stream loop, before the stall guard.
    offered: u64,
    /// (when, `offered` then): the older is 100-200 ms back, the window the fill is read over.
    marks: [(Instant, u64); 2],
}

impl PyroRate {
    pub(crate) fn new(target_kbps: u32, floor_kbps: u32, now: Instant) -> Self {
        Self {
            estimator: Estimator::new(f64::from(target_kbps), now),
            log: SendLog::new(now),
            target_kbps: f64::from(target_kbps),
            floor_kbps: f64::from(floor_kbps),
            on: now,
            judged_from: None,
            below_since: None,
            // The first judgement always writes: a previous client may have left a lower one.
            budget: 0,
            events: None,
            latched: false,
            waiting_since: None,
            offered: 0,
            marks: [(now, 0); 2],
        }
    }

    pub(crate) fn on_feedback(
        &mut self,
        fb: &Feedback,
        now: Instant,
        path: QuicPath,
    ) -> PyroVerdict {
        self.judged_from.get_or_insert(now);
        self.estimator.on_feedback(fb, &mut self.log, now, path);
        self.judge(now, path)
    }

    pub(crate) fn on_tick(&mut self, now: Instant, path: QuicPath) -> PyroVerdict {
        let deadline = self.on + PYRO_STEP_DOWN_AFTER;
        if now >= deadline {
            self.judged_from.get_or_insert(deadline);
        }
        self.judge(now, path)
    }

    /// One encoded frame of `bytes`, counted whether or not the stall guard sends it.
    pub(crate) fn offer(&mut self, bytes: usize) {
        self.offered += bytes as u64;
    }

    /// The encoder's output over the last 100-200 ms, kbit/s.
    fn offered_kbps(&mut self, now: Instant) -> f64 {
        if now.saturating_duration_since(self.marks[1].0) >= PYRO_FILL_WINDOW {
            self.marks = [self.marks[1], (now, self.offered)];
        }
        let (at, then) = self.marks[0];
        let secs = now.saturating_duration_since(at).as_secs_f64();
        if secs == 0.0 {
            return f64::INFINITY;
        }
        (self.offered - then) as f64 * 8.0 / 1000.0 / secs
    }

    fn judge(&mut self, now: Instant, path: QuicPath) -> PyroVerdict {
        let mut kbps = self.estimator.estimate_kbps.min(self.target_kbps);
        let events = self.events.replace(path.congestion_events);
        // A frame smaller than the window waits only while quinn flushes it.
        let waiting = path.queued_bytes > path.cwnd_bytes;
        if !waiting {
            self.waiting_since = None;
        }
        let sustained = waiting
            && now.saturating_duration_since(*self.waiting_since.get_or_insert(now))
                >= PYRO_FILL_WINDOW;
        let rose = events.is_some_and(|was| path.congestion_events > was);
        let fills = self.offered_kbps(now) >= APP_LIMITED * f64::from(self.budget);
        if self.judged_from.is_some() {
            if sustained || ((rose || self.latched) && fills) {
                kbps = kbps.min(quic_kbps(path));
            }
            if kbps < self.floor_kbps {
                // A single loss (`rose`) never latches: it buys this judgement only.
                self.latched = sustained || (self.latched && fills);
                let since = *self.below_since.get_or_insert(now);
                if now.saturating_duration_since(since) >= PYRO_STEP_DOWN_AFTER {
                    return PyroVerdict::StepDown {
                        estimate_kbps: kbps,
                    };
                }
            } else {
                self.below_since = None;
                self.latched = false;
            }
        }
        let budget = pyro_budget(kbps);
        if budget == self.budget {
            return PyroVerdict::Hold;
        }
        self.budget = budget;
        PyroVerdict::Budget(budget)
    }
}

/// What the connection's congestion controller permits: its window over the smoothed RTT;
/// unbounded when either is unknown (0).
fn quic_kbps(path: QuicPath) -> f64 {
    if path.rtt.is_zero() || path.cwnd_bytes == 0 {
        return f64::INFINITY;
    }
    path.cwnd_bytes as f64 * 8.0 / path.rtt.as_secs_f64() / 1000.0
}

/// QUIC's window over the estimate's rate × RTT: a keyframe burst and RTT jitter fit under
/// it, and quinn paces at 1.25 × window / RTT, 2.5 × the rate.
const WINDOW_GAIN: f64 = 2.0;
/// A LAN's RTT would make the window a few datagrams, and a keyframe wait behind them.
const WINDOW_RTT_MIN: Duration = Duration::from_millis(100);

/// One connection's QUIC congestion window. It is quinn's Cubic unless the estimate is
/// governing the encoder from live feedback (`Governor`): a client that sends no feedback,
/// one whose feedback stopped, a session with the ABR off and a PyroWave session all follow
/// QUIC congestion control alone (§Feedback). While governed the window follows the
/// estimate and no loss shrinks it: Cubic read packets reordered or late from jitter, and
/// 0.1 % random loss, as congestion, and held video to a few Mbit/s while the datagrams over
/// its window waited or were dropped in the send queue (#1330). The estimate is then the
/// only controller, as `rtpgccbwe` is on WebRTC.
pub(crate) struct Window {
    cubic: Box<dyn Controller>,
    governed: Arc<AtomicU64>,
    probe: Arc<AtomicBool>,
    /// The rate it was last governed at, bit/s; 0 while Cubic has it.
    held: u64,
    srtt: Duration,
    min_rtt: Duration,
    mtu: u16,
    /// Cubic's first window, which quinn also reads at a path change.
    first: u64,
}

/// Builds a connection's [`Window`]; its handle is the connection's [`Governor`].
pub(crate) struct WindowFactory(pub(crate) Governor);

/// What sets a connection's window: the estimate, while it governs the encoder.
#[derive(Clone, Default)]
pub(crate) struct Governor(Arc<AtomicU64>, Arc<AtomicBool>);

impl Governor {
    pub(crate) fn probe(&self, due: bool) {
        self.1.store(due, Ordering::Relaxed);
    }

    pub(crate) fn govern(&self, kbps: f64) {
        self.0
            .store((kbps * 1000.0).max(1.0) as u64, Ordering::Relaxed);
    }

    /// Back to Cubic.
    pub(crate) fn release(&self) {
        self.0.store(0, Ordering::Relaxed);
    }

    /// The window follows the estimate, not Cubic.
    pub(crate) fn governs(&self) -> bool {
        self.0.load(Ordering::Relaxed) > 0
    }

    /// The rate the window is held to, kbit/s.
    pub(crate) fn kbps(&self) -> u64 {
        self.0.load(Ordering::Relaxed) / 1000
    }
}

/// The rates `VideoQueue` holds video to, kbit/s: QUIC's room ([`VideoQueue::room`]) at the
/// governed rate or the estimate if lower, and the pacer ([`VideoQueue::pace`]) at the
/// governed rate, never under the ABR floor the encoder is held to, once the estimate is under
/// the ceiling. `None`: the session is not governed (or PyroWave), or nothing to pace.
pub(crate) fn video_rates(
    governor: &Governor,
    estimator: &Estimator,
    pyrowave: bool,
) -> (Option<u64>, Option<u64>) {
    if pyrowave || !governor.governs() {
        return (None, None);
    }
    let room = (governor.kbps() as f64).min(estimator.estimate_kbps()) as u64;
    (Some(room), estimator.found_link().then(|| governor.kbps()))
}

fn cubic(window: u64, now: Instant, mtu: u16) -> Box<dyn Controller> {
    let mut config = CubicConfig::default();
    config.initial_window(window);
    Arc::new(config).build(now, mtu)
}

impl ControllerFactory for WindowFactory {
    fn build(self: Arc<Self>, now: Instant, current_mtu: u16) -> Box<dyn Controller> {
        let cubic = Arc::new(CubicConfig::default()).build(now, current_mtu);
        Box::new(Window {
            first: cubic.initial_window(),
            cubic,
            governed: self.0 .0.clone(),
            probe: self.0 .1.clone(),
            held: 0,
            srtt: Duration::ZERO,
            min_rtt: Duration::ZERO,
            mtu: current_mtu,
        })
    }
}

impl Window {
    /// The governed rate, once an ack has given the window an RTT to size it by.
    fn governing(&self) -> Option<u64> {
        Some(self.governed.load(Ordering::Relaxed)).filter(|&bps| bps > 0 && !self.srtt.is_zero())
    }

    /// Cubic's window on release: the path's delay-free BDP at the rate it last carried, so
    /// it neither starts collapsed (Cubic took every loss while governed) nor keeps the
    /// governed window's headroom. In slow start, as a new connection is.
    fn seed(&self) -> u64 {
        ((self.held as f64 / 8.0 * self.min_rtt.as_secs_f64()) as u64).max(self.first)
    }

    /// Follows the governor: a release starts a new Cubic from `seed`.
    fn sync(&mut self, now: Instant) {
        match self.governing() {
            Some(bps) => self.held = bps,
            None if self.held != 0 => {
                self.cubic = cubic(self.seed(), now, self.mtu);
                self.held = 0;
            }
            None => {}
        }
    }
}

impl Controller for Window {
    fn on_sent(&mut self, now: Instant, bytes: u64, last_packet_number: u64) {
        self.probe.store(false, Ordering::Relaxed);
        self.sync(now);
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
        // Before quinn's first sample this is its initial RTT; by `hello` the handshake gave one.
        self.srtt = rtt.get();
        self.min_rtt = rtt.min();
        self.sync(now);
        self.cubic.on_ack(now, sent, bytes, app_limited, rtt);
    }

    fn on_end_acks(
        &mut self,
        now: Instant,
        in_flight: u64,
        app_limited: bool,
        largest_packet_num_acked: Option<u64>,
    ) {
        self.sync(now);
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
        self.sync(now);
        self.cubic
            .on_congestion_event(now, sent, is_persistent_congestion, lost_bytes);
    }

    fn on_mtu_update(&mut self, new_mtu: u16) {
        self.mtu = new_mtu;
        self.cubic.on_mtu_update(new_mtu);
    }

    fn window(&self) -> u64 {
        // Quinn checks the window before each packet of a transmit and calls on_sent once
        // after it, so this lets one transmit past the window: up to ten packets where the
        // socket batches them. Including after release back to Cubic, which is where an
        // outage leaves the connection (silent feedback releases the estimate).
        if self.probe.load(Ordering::Relaxed) {
            return u64::MAX;
        }
        match self.governing() {
            Some(bps) => {
                let rtt = self.srtt.max(WINDOW_RTT_MIN).as_secs_f64();
                ((WINDOW_GAIN * bps as f64 / 8.0 * rtt) as u64).max(self.first)
            }
            // Released since the last call that could rebuild Cubic.
            None if self.held != 0 => self.seed(),
            None => self.cubic.window(),
        }
    }

    fn clone_box(&self) -> Box<dyn Controller> {
        Box::new(Window {
            cubic: self.cubic.clone_box(),
            governed: self.governed.clone(),
            probe: self.probe.clone(),
            held: self.held,
            srtt: self.srtt,
            min_rtt: self.min_rtt,
            mtu: self.mtu,
            first: self.first,
        })
    }

    fn initial_window(&self) -> u64 {
        self.first
    }

    fn into_any(self: Box<Self>) -> Box<dyn std::any::Any> {
        self
    }
}

/// What to do with the next video unit or `nack` answer while QUIC's datagram queue is
/// backed up.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Admit {
    Send,
    Skip,
    /// Skip, and ask the encoder for a keyframe to resume on.
    SkipForKeyframe,
    /// No keyframe got through for `GIVE_UP_HOLDS` holds: end the WebTransport media. The
    /// browser reconnects over WebTransport once; a second loss falls back to WebRTC
    /// (`stream_lost`).
    GiveUp,
}

/// Bytes in QUIC's datagram queue under one fragment: nothing is waiting.
const BACKLOG_EMPTY: usize = super::media::MAX_DATAGRAM;
/// Holds in which units came while the queue stayed backed up, or keyframe asks went
/// unanswered on a drained one, before the media is given up. About 6 s at the 250 ms
/// hold, past a 2 s outage and quinn's probe backoff after it (about 2 s). The hold is
/// max(250 ms, 2 x RTT), so this scales with RTT, and stays under the 10 s idle timeout
/// only below about 208 ms. An outage longer than this ends the media mid-outage and uses
/// the view's one reconnect, where it used to recover on WebTransport: the accepted trade
/// for not sitting on a frozen picture.
const GIVE_UP_HOLDS: u32 = 24;

/// Holds counted at most once each, and only when a unit comes: a still screen sends
/// nothing, and its silence counts for nothing.
#[derive(Default)]
struct Holds {
    at: Option<Instant>,
    n: u32,
}

impl Holds {
    /// Counts `now` when a hold has passed since the last count; true when it did.
    fn count(&mut self, now: Instant, hold: Duration) -> bool {
        let due = self
            .at
            .is_none_or(|at| now.saturating_duration_since(at) >= hold);
        if due {
            self.at = Some(now);
            self.n += 1;
        }
        due
    }
}

/// Video behind a closed window (a stalled path) is worthless once it is older than the
/// client's NACK hold, and quinn's queue holds a MiB of it, dropping only the oldest
/// datagram when full. A queue that has not shrunk for that long is stalled: video and NACK
/// answers are skipped until it empties and a keyframe comes, so it never holds more than
/// the hold's worth. A keyframe being paced out shrinks it, so it is never mistaken for a
/// stall.
#[derive(Default)]
pub(crate) struct Backlog {
    last: usize,
    probe_acks: Option<u64>,
    /// The ACK count at the last probe tick: unchanged at the next one, the path is silent.
    tick_acks: Option<u64>,
    growing_since: Option<Instant>,
    skipping: bool,
    backed_up: Holds,
    /// Keyframe asks: one each hold while the queue is drained, since a keyframe that came
    /// while it still held a datagram was skipped like the rest.
    asks: Holds,
    /// Video units and `nack` answers skipped over the connection.
    pub(crate) skipped: u64,
}

impl Backlog {
    pub(crate) fn probed(&mut self, acks: u64) {
        self.probe_acks.get_or_insert(acks);
    }

    pub(crate) fn acknowledge(&mut self, acks: u64) {
        if self.probe_acks.is_some_and(|before| before != acks) {
            self.probe_acks = None;
        }
    }

    /// Called every probe tick with QUIC's ACK count. Whether to probe the path now: video is
    /// held behind a stalled queue and no ACK has arrived since the last tick. A path that
    /// still acknowledges, however slowly, opens its own window; probing past the window is
    /// for a path that says nothing, where the only cost is a packet lost.
    pub(crate) fn probe(&mut self, queued: usize, acks: u64) -> bool {
        self.acknowledge(acks);
        let silent = self.tick_acks.replace(acks) == Some(acks);
        let due = silent && self.starved(queued);
        if due {
            self.probed(acks);
        }
        due
    }

    pub(crate) fn starved(&self, queued: usize) -> bool {
        self.skipping && (queued >= BACKLOG_EMPTY || self.probe_acks.is_some())
    }

    /// `queued`: bytes in QUIC's datagram queue now; `keyframe`: the unit can be decoded
    /// alone; `hold`: how long video stays worth sending.
    pub(crate) fn admit(
        &mut self,
        queued: usize,
        keyframe: bool,
        now: Instant,
        hold: Duration,
    ) -> Admit {
        // Sending a probe can empty a small queue even while the path is down. Only
        // an ACK proves recovery; otherwise the original give-up clock must keep running.
        let empty = queued < BACKLOG_EMPTY && self.probe_acks.is_none();
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
            self.backed_up = Holds::default();
            self.asks = Holds::default();
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
        let holds = if empty {
            &mut self.asks
        } else {
            &mut self.backed_up
        };
        let counted = holds.count(now, hold);
        if holds.n > GIVE_UP_HOLDS {
            tracing::warn!(
                token = "webtransport-video-unrecoverable",
                queued,
                "no keyframe got through the WebTransport datagram queue; ending the \
                 WebTransport media"
            );
            return Admit::GiveUp;
        }
        self.skipped += 1;
        if empty && counted {
            Admit::SkipForKeyframe
        } else {
            Admit::Skip
        }
    }
}

/// Video QUIC may hold ahead of the next audio datagram, in time at the governed rate. Over
/// two frames at 60 fps: at 20 ms a 1080p60 frame at 8 Mbit/s plus its FEC filled it, waited a
/// drain tick, and a clean link presented 3 ms later at the median (#1433, lab host).
/// shortcut: at 30 fps a frame is 33 ms of rate, so one over this with its FEC still waits a
/// tick; make it frame-relative if a 30 fps session shows that cost.
const AUDIO_AHEAD_MS: u64 = 40;
/// Video leaves at most this × the governed rate (the estimate, never under the ABR floor)
/// once the estimate is under the ceiling (#1433), so a keyframe waits here, where audio
/// overtakes it, not in the link's queue: right after a delay cut this is 1.06 × what the
/// link carried. quinn alone paces at 1.25 × window / RTT, 2.5 × the rate or more. Must stay
/// over 1 / (1 − the ABR's 0.15 deadband), the most video plus FEC is sent at, or held video
/// never drains.
const PACE_GAIN: f64 = 1.25;
/// The pacer's burst: this long at the pace, at least two datagrams.
const PACE_BURST_MS: f64 = 5.0;
/// Video that has waited this long is not paced: a paced keyframe of five frames' bytes waits
/// at most 5 / (60 fps × `PACE_GAIN`), 67 ms, so this is a path back from an outage, or a
/// source over the pace, and it drains at QUIC's own pace as before #1433.
const PACE_MAX_WAIT: Duration = Duration::from_millis(100);

/// Video, FEC and `nack` answers waiting for room in QUIC's datagram queue, so audio, which
/// goes straight to QUIC, never queues behind more than [`VideoQueue::room`] of video and
/// is never the datagram QUIC drops to make space (#1321). `pkt_seq` is taken as a datagram
/// is handed to QUIC, so it follows send order (§Channels) even when audio overtakes.
#[derive(Default)]
pub(crate) struct VideoQueue {
    held: VecDeque<Held>,
    bytes: usize,
    /// The `pkt_seq` of the next datagram handed to QUIC.
    wire: u32,
    /// The rate the room was last sized at: held video drains at it after a release.
    pace: Option<u64>,
    /// Video datagrams dropped at the cap; they never take a `pkt_seq`, so no feedback
    /// reports them lost.
    pub(crate) dropped: u64,
    /// The pacer: bytes video may still send now, and when they were counted.
    tokens: f64,
    counted: Option<Instant>,
    /// Video and resent datagrams over the path's datagram size, skipped.
    pub(crate) too_large: u64,
    pub(crate) resends_too_large: u64,
}

pub(crate) struct Held {
    pub(crate) datagram: bytes::Bytes,
    /// Goes in the send log, stamped `queued_at`: time held here is delay the estimate
    /// must see, as it saw time in QUIC's queue.
    pub(crate) logged: bool,
    pub(crate) resend: bool,
    pub(crate) queued_at: Instant,
}

/// What became of a datagram handed to QUIC.
pub(crate) enum Handed {
    Sent,
    /// Not sent and its `pkt_seq` not used: a resend the fragment's budget no longer allows.
    Skipped,
    /// Over the path's datagram size: skipped like [`Handed::Skipped`], and counted.
    TooLarge,
    Gone,
}

impl VideoQueue {
    /// Bytes of QUIC's datagram queue video may fill: [`AUDIO_AHEAD_MS`] at `kbps`, at
    /// least two datagrams, at most half the queue so audio always fits beside it. With no
    /// rate (`None`), video held from a governed spell still drains at that spell's last
    /// rate; only an empty queue goes back to QUIC's whole buffer.
    /// shortcut: a session never governed (no feedback, ABR off, PyroWave) gets the whole
    /// buffer, so under congestion audio waits behind QUIC's queue as before #1321.
    pub(crate) fn room(&mut self, kbps: Option<u64>, buffer: usize) -> usize {
        let kbps = kbps.or(self.pace.filter(|_| !self.held.is_empty()));
        self.pace = kbps;
        kbps.map_or(buffer, |kbps| {
            ((kbps * AUDIO_AHEAD_MS / 8) as usize)
                .max(2 * super::media::MAX_DATAGRAM)
                .min(buffer / 2)
        })
    }

    /// Bytes the pacer lets go now: [`PACE_GAIN`] × `kbps`, in bursts of [`PACE_BURST_MS`];
    /// `None` paces nothing.
    pub(crate) fn pace(&mut self, kbps: Option<u64>, now: Instant) -> usize {
        // Not a `nack` answer: those go ahead of older video (`push`).
        let stale = self
            .held
            .iter()
            .find(|h| !h.resend)
            .is_some_and(|h| now.saturating_duration_since(h.queued_at) >= PACE_MAX_WAIT);
        let Some(kbps) = kbps.filter(|_| !stale) else {
            self.counted = None;
            return usize::MAX;
        };
        let per_s = kbps as f64 * PACE_GAIN * 1000.0 / 8.0;
        let burst = (per_s * PACE_BURST_MS / 1000.0).max(2.0 * super::media::MAX_DATAGRAM as f64);
        self.tokens = match self.counted {
            Some(at) => self.tokens + per_s * now.saturating_duration_since(at).as_secs_f64(),
            None => burst,
        }
        .min(burst);
        self.counted = Some(now);
        self.tokens as usize
    }

    pub(crate) fn is_empty(&self) -> bool {
        self.held.is_empty()
    }

    #[cfg(test)]
    pub(crate) fn oldest(&self) -> Option<Instant> {
        self.held.front().map(|h| h.queued_at)
    }

    pub(crate) fn bytes(&self) -> usize {
        self.bytes
    }

    /// Drops everything held: datagrams of a configuration the client has left.
    pub(crate) fn clear(&mut self) {
        self.held.clear();
        self.bytes = 0;
        self.pace = None;
        self.counted = None;
    }

    /// The `pkt_seq` new video is numbered from, a guess `feed` corrects: after everything
    /// held.
    pub(crate) fn next_seq(&self) -> u32 {
        self.wire.wrapping_add(self.held.len() as u32)
    }

    /// The `pkt_seq` of a datagram handed to QUIC now, ahead of everything held.
    pub(crate) fn take_seq(&mut self) -> u32 {
        let seq = self.wire;
        self.wire = seq.wrapping_add(1);
        seq
    }

    /// Past `cap` held bytes the oldest video is dropped, as QUIC's own queue drops it, and
    /// `nack` answers only once no video is left.
    pub(crate) fn push(&mut self, held: Held, cap: usize) {
        while self.bytes + held.datagram.len() > cap {
            let oldest = self.held.iter().position(|h| !h.resend).unwrap_or(0);
            let Some(old) = self.held.remove(oldest) else {
                break;
            };
            self.bytes -= old.datagram.len();
            self.dropped += 1;
        }
        self.bytes += held.datagram.len();
        // A resend goes ahead of held video (after earlier resends): the client gives a frame
        // up about 2 × (sRTT + 10 ms) after its first NACK, sooner than a paced keyframe drains.
        let at = if held.resend {
            self.held.iter().take_while(|h| h.resend).count()
        } else {
            self.held.len()
        };
        self.held.insert(at, held);
    }

    /// Hands `send` the oldest datagrams that fit in `room` bytes, each carrying the next
    /// wire `pkt_seq`, and logs each sent one at the time it was queued. False when the
    /// connection is gone.
    pub(crate) fn feed(
        &mut self,
        mut room: usize,
        mut log: Option<&mut SendLog>,
        mut send: impl FnMut(Held) -> Handed,
    ) -> bool {
        while let Some(next) = self.held.front() {
            let len = next.datagram.len();
            if len > room {
                break;
            }
            let mut held = self.held.pop_front().expect("front exists");
            self.bytes -= len;
            let seq = self.wire.to_le_bytes();
            if held.datagram.get(1..5) != Some(&seq[..]) {
                // Renumbered only after audio overtook it; a fragment the retransmit store
                // shares is copied, a FEC repair or `nack` answer is not.
                let mut d = held
                    .datagram
                    .try_into_mut()
                    .unwrap_or_else(|d| bytes::BytesMut::from(&d[..]));
                d[1..5].copy_from_slice(&seq);
                held.datagram = d.freeze();
            }
            // While paced, a datagram is logged as sent when the pacer let it go: its wait here
            // is the pacer's, not the path's, and read as delay it cut the estimate at each
            // keyframe on a link faster than the pace. Unpaced, its queue time is logged, as
            // it was in QUIC's queue before #1321. `SendLog::record` keeps the mix monotone.
            let (resend, queued_at) = (held.resend, self.counted.unwrap_or(held.queued_at));
            let logged = log.as_deref_mut().filter(|_| held.logged);
            let record = logged.map(|log| (log, held.datagram.clone()));
            match send(held) {
                Handed::Sent => {
                    if let Some((log, d)) = record {
                        log.record(&d, queued_at, resend);
                    }
                    room -= len;
                    self.tokens -= len as f64;
                    self.wire = self.wire.wrapping_add(1);
                }
                Handed::Skipped => {}
                Handed::TooLarge if resend => self.resends_too_large += 1,
                Handed::TooLarge => self.too_large += 1,
                Handed::Gone => return false,
            }
        }
        true
    }
}

#[cfg(test)]
mod tests;
