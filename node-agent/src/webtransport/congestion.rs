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

/// §Feedback: `count` is at most 8192; arrivals are in 50 µs units, `0xFFFF` not received.
const MAX_COUNT: usize = 8192;
const UNIT_US: u64 = 50;
const NOT_RECEIVED: u16 = 0xFFFF;
/// The client's cadence (§Feedback).
const FEEDBACK_INTERVAL: Duration = Duration::from_millis(50);

// The blackout classifier's thresholds (#1433), untested on hardware: the lab campaign tunes them.
/// A solid run of loss over at least this much send time and `BLACKOUT_MIN_DATAGRAMS`, with
/// the arrivals on both sides at the path floor, is a blackout: no loss, no reading, no FEC.
const BLACKOUT_MIN_US: u64 = 30_000;
const BLACKOUT_MIN_DATAGRAMS: usize = 4;
/// A run of loss a report ends inside (the browser stops at `MAX_COUNT` or before an open gap)
/// waits for the next report's arrival this long, then is loss.
const RUN_MAX_WAIT: Duration = Duration::from_millis(500);
/// An arrival under this much one-way delay over the path floor is at the floor.
const AT_FLOOR_MS: f64 = 30.0;
/// The path floor is the least one-way delay over `FLOOR_SPAN_US` of send time, read as the
/// least of each `DELAY_BUCKET_US`: a delay step ages in within the span.
const DELAY_BUCKET_US: u64 = 500_000;
const FLOOR_SPAN_US: u64 = 10_000_000;

/// Send records kept, by `pkt_seq`: over the estimator's 0.5 s rate window at a 400 Mbit/s
/// grant, 25 MB of datagrams, ~21 800 of them (#1430). Sized for PyroWave's grant; the
/// standard codec's 100 Mbit/s left the 0.5 s window at 400 Mbit/s under-read, so windows
/// read as application-limited.
const LOG: usize = 1 << 15;

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
/// v2's hold (#1433, lab rows R3/R4): after a window that loses over `LOSS_HIGH` while the encoder
/// fills its setpoint, or a second shortfall reading in a row, nothing the sender emits goes over
/// `BETA` × what that window delivered until this long has passed with no busy report losing
/// `LOSS_LOW` over the loss floor. A hold set by loss ends when a later burst that lost as much at
/// a lower rate is over with the sender still at that rate (`Estimator::apply`, the per-burst
/// discriminator), and while it holds the pacer keeps a keyframe
/// paced (`PACE_MAX_WAIT_HELD`). The probe that ends a hold on a link still shaped meets its
/// edge, and that can freeze (`a_shallow_buffer_keeps_its_frames_and_reaches_540p_in_15_s`, 180 s
/// shaped: 30 s, two probe freezes; 60 s, none); the lab's R4 climb came 70 s after its last step. Longer only delays
/// recovery after a cap lifts (`a_shallow_cap_then_a_lift_bounds_the_abr_and_recovers_it`: 8 s at
/// 30 s, 36 s at 60 s), which costs resolution, not frames.
const HOLD: Duration = Duration::from_secs(60);
/// After the hold its line probes up this much a second until it passes the most the link
/// delivered while dropping, then at the estimate's 8 % (the link has lifted). Slow enough that
/// the loss that ends a probe on a link still shaped is a few percent, which FEC and NACK repair.
const PROBE_PER_S: f64 = 1.02;
/// With FEC negotiated (`Estimator::probes`) the hold probes the link with extra repair datagrams
/// instead (`Estimator::probe_kbps`): video stays at the line and losing a repair costs no frame. A
/// probe starts this long after the line last stepped down or a probe stopped; a link that lifts is
/// found within it. The first probe 1-3 s after a step met the line still settling and bursts that
/// a Wi-Fi release would have ended (sim: second step to 2500, Wi-Fi rows 2.5 and 4 s apart).
const PROBE_WAIT: Duration = Duration::from_secs(5);
/// One probe step: a GOP of the lab encoder (60 frames at 60 fps), so a keyframe crosses each step,
/// and over 200 datagrams at 2.4 Mbit/s to judge 2 % loss on.
const PROBE_STEP: Duration = Duration::from_secs(1);
/// The probe's total, a multiple of the line: on a link still shaped (a line at 0.7-0.85 × it) the
/// probe is far over it, so its loss shows in the first report and it stops at once. A probe just
/// over a policer's rate hides behind its bucket for a step and then drains it under a keyframe
/// (sim R4, 1.25 ×: a freeze per few crossings). A step carried under `LOSS_LOW` over the loss
/// floor, with no reading and no overuse, raises the line to `BETA`² × the probe and the next step
/// doubles again: a probe is smooth and video is not, and a line raised to 0.82 × a policer's rate
/// overflowed it on the lab encoder's keyframes (sim, 5000 → 3000: 22 freezes at `BETA`).
const PROBE_GAIN: f64 = 2.0;
/// Per report: the loss seen with no probe running is judged over about 2.5 s.
const UNPROBED_DECAY: f64 = 0.98;
/// A probe is stopped by loss only once it lost this many datagrams over the base.
const PROBE_LOST_MIN: f64 = 4.0;
/// A step counts only once this many of the probe's own datagrams are in it.
const PROBE_MIN: i64 = 20;
/// The probe's bytes go in one burst right behind a frame's (`SendLog::probe_room`): what a frame
/// interval at the probe's rate leaves, at most, within this long of the frame's last datagram. A
/// policer refills over the whole interval and a queue drains over it, and the frame takes that
/// room before the probe does, so only repair overflows. Spread out, the probe took the refill as
/// it came and the next frame met an empty bucket (sim, second step to 3800: 2-3 % video loss and a
/// keyframe asked at each crossing).
const PROBE_BANK: Duration = Duration::from_micros(16_667);
const PROBE_TAIL: Duration = Duration::from_millis(2);
const LOSS_WINDOW: Duration = Duration::from_millis(500);
/// An encoder sending this share of its setpoint or more is busy once heavy loss has lasted
/// `LOSS_WINDOW` (`Estimator::apply`); a still screen sends far less.
const UNDERFILL: f64 = 0.5;
/// v2's loss floor (`Estimator::floor_bin_closed`) is judged over the last `FLOOR_BINS` bins of
/// at least `FLOOR_BIN` and `FLOOR_BIN_SAMPLES` datagrams each (16 s), from bins sent at
/// `FLOOR_SPREAD` or less of the busiest one's rate.
const FLOOR_BIN: Duration = Duration::from_secs(2);
const FLOOR_BINS: usize = 8;
const FLOOR_BIN_SAMPLES: i64 = 200;
const FLOOR_SPREAD: f64 = 0.9;
/// Loss at the lower rate this share of the higher rate's or more did not fall with it.
const FLOOR_FLAT: f64 = 0.8;
/// Fewer datagrams than this in the window decide nothing about loss.
const LOSS_MIN_SAMPLES: i64 = 20;
const MAX_LOSS_ENTRIES: usize = 64;

/// The receive rate is measured over this much of the client's arrival clock.
const RATE_WINDOW_US: u64 = 500_000;
/// A rate measured over less than this is noise; the last good one stands in.
const MIN_RATE_SPAN_US: u64 = 100_000;
/// Arrival gaps whose excess over their send gap rule 2 takes out of a rate window: two steps of a
/// path's delay can land in one window.
const GAPS_TAKEN_OUT: usize = 2;
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
/// Datagrams that arrived at under this share of the rate they were sent at: the link held
/// them back (a queue or loss), so the report is a reading of the link ([`Estimator::measured`]).
const LINK_LIMITED: f64 = 0.95;
/// Arrivals kept for the rate window, by receive order, one per datagram: the same 0.5 s at a
/// 400 Mbit/s grant, ~21 800 of them (#1430).
const MAX_ARRIVALS: usize = 1 << 15;

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
    /// A keyframe's, or sent `KEY_SPILL` or less after one (`Run::key`).
    key: bool,
    /// A capacity probe's repair datagram (`SendLog::record_probe`): out of the loss count.
    probe: bool,
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
    /// The stamp of the last keyframe datagram, µs.
    key_us: Option<u64>,
    /// The last stamp, µs: stamps never go back in `pkt_seq` (send) order, whatever stamped
    /// them (a paced release, a queue time, a resend), or the delay detector reads a send
    /// that went back in time.
    last_us: u64,
    /// The probe's bytes still to send (`Self::probe_room`), less every datagram logged since,
    /// and when the last datagram that was not the probe's went.
    probe_tokens: f64,
    probe_at: Option<Instant>,
    video_at: Option<Instant>,
}

impl SendLog {
    pub(crate) fn new(start: Instant) -> Self {
        Self {
            start,
            slots: vec![Slot::default(); LOG],
            unanswered_since: None,
            unanswered_seq: None,
            next_seq: 0,
            key_us: None,
            last_us: 0,
            probe_tokens: 0.0,
            probe_at: None,
            video_at: None,
        }
    }

    /// [`Self::record_handed`] for a datagram stamped when it was handed to QUIC.
    #[cfg(test)]
    pub(crate) fn record(&mut self, datagram: &[u8], at: Instant, resend: bool) {
        self.record_handed(datagram, at, at, resend, false);
    }

    /// `pkt_seq` is bytes 1..5 of every datagram (§Channels). A `resend` (a NACK answer)
    /// counts toward loss, not toward the receive rate. `at` stamps the delay detector;
    /// `handed` is when QUIC got it, and starts the silence clock.
    pub(crate) fn record_handed(
        &mut self,
        datagram: &[u8],
        at: Instant,
        handed: Instant,
        resend: bool,
        key: bool,
    ) {
        let Some(seq) = datagram.get(1..5) else {
            return;
        };
        let seq = u32::from_le_bytes([seq[0], seq[1], seq[2], seq[3]]);
        self.last_us = self
            .last_us
            .max(at.saturating_duration_since(self.start).as_micros() as u64);
        if key {
            self.key_us = Some(self.last_us);
        }
        let key = self
            .key_us
            .is_some_and(|k| self.last_us - k <= KEY_SPILL.as_micros() as u64);
        self.slots[seq as usize % LOG] = Slot {
            seq,
            sent_us: self.last_us,
            bytes: datagram.len().min(u16::MAX as usize) as u16,
            resend,
            key,
            probe: false,
            status: Status::Unreported,
        };
        self.probe_tokens -= datagram.len() as f64;
        self.video_at = Some(handed);
        // The hand-off, not the stamp: unpaced held video is stamped with its queue time,
        // before a report it is handed to QUIC after (#1591).
        self.unanswered_since.get_or_insert(handed);
        self.unanswered_seq.get_or_insert(seq);
        self.next_seq = seq.wrapping_add(1);
    }

    /// A capacity probe's datagram handed to QUIC now.
    pub(crate) fn record_probe(&mut self, datagram: &[u8], now: Instant) {
        let video_at = self.video_at;
        self.record_handed(datagram, now, now, false, false);
        self.video_at = video_at;
        if let Some(slot) = datagram
            .get(1..5)
            .and_then(|s| self.slot(u32::from_le_bytes([s[0], s[1], s[2], s[3]])))
        {
            slot.probe = true;
        }
    }

    /// Bytes the capacity probe may send now: `kbps` (`Estimator::probe_kbps`) on the wire, less
    /// every datagram logged, so video plus probe is `kbps` whatever the encoder sends, banked over
    /// `PROBE_BANK` and spent within `PROBE_TAIL` of a frame. Audio is not logged: 128 kbit/s over
    /// it.
    pub(crate) fn probe_room(&mut self, kbps: Option<f64>, now: Instant) -> f64 {
        let Some(kbps) = kbps else {
            (self.probe_tokens, self.probe_at) = (0.0, None);
            return 0.0;
        };
        self.probe_tokens = match self.probe_at {
            Some(at) => {
                self.probe_tokens + kbps * 125.0 * now.saturating_duration_since(at).as_secs_f64()
            }
            None => 0.0,
        }
        .min(kbps * 125.0 * PROBE_BANK.as_secs_f64());
        self.probe_at = Some(now);
        match self
            .video_at
            .is_some_and(|at| now.saturating_duration_since(at) <= PROBE_TAIL)
        {
            true => self.probe_tokens,
            false => 0.0,
        }
    }

    fn slot(&mut self, seq: u32) -> Option<&mut Slot> {
        Some(&mut self.slots[seq as usize % LOG]).filter(|s| s.seq == seq && s.bytes > 0)
    }

    fn peek(&self, seq: u32) -> Option<&Slot> {
        Some(&self.slots[seq as usize % LOG]).filter(|s| s.seq == seq && s.bytes > 0)
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

/// The blackout classifier's state (#1433): a run of loss until an arrival ends it, and the
/// path floor that says whether the arrivals on both sides of it were at the floor.
#[derive(Default)]
struct Evidence {
    /// Lost `pkt_seq`s no arrival has ended yet, whether the arrival before them was at the
    /// floor, and when the run began. Counted, or found a blackout, when an arrival ends it; a
    /// report can end inside it (`web/src/webtransport/feedback.ts` `take`), so it waits for the
    /// next contiguous report, at most `RUN_MAX_WAIT`.
    run: Vec<u32>,
    run_from_floor: bool,
    run_since: Option<Instant>,
    /// An unlogged datagram (audio) arrived after the run's last loss; a loss after that makes
    /// the run no blackout: the link carried something inside it.
    run_heard: bool,
    run_broken: bool,
    /// The run was decided as loss and no arrival or discontinuity has ended it: what follows
    /// it contiguously is the same run, and loss too.
    run_lost: bool,
    at_floor: bool,
    /// (send-time bucket, least one-way delay µs) of each bucket done, oldest first, and the
    /// one being filled. One-way delay is on two clocks: only differences mean anything.
    buckets: VecDeque<(u64, i64)>,
    current: Option<(u64, i64)>,
    floor: Option<i64>,
}

impl Evidence {
    /// One fresh arrival into the floor reading; true when it arrived at the floor.
    fn arrival(&mut self, sent_us: u64, at_us: u64) -> bool {
        // Two clocks, and `feedback_of` accepts any u64 receiver clock: never overflow.
        let delay = (i128::from(at_us) - i128::from(sent_us))
            .clamp(i64::MIN.into(), i64::MAX.into()) as i64;
        let bucket = sent_us / DELAY_BUCKET_US;
        match self.current.as_mut() {
            Some((b, least)) if *b >= bucket => *least = (*least).min(delay),
            _ => {
                if let Some(done) = self.current.replace((bucket, delay)) {
                    self.close(done);
                }
            }
        }
        let floor = self
            .floor
            .unwrap_or_else(|| self.current.map_or(delay, |c| c.1));
        (delay.saturating_sub(floor) as f64) < AT_FLOOR_MS * 1000.0
    }

    fn close(&mut self, (bucket, least): (u64, i64)) {
        self.buckets.push_back((bucket, least));
        while self
            .buckets
            .front()
            .is_some_and(|&(b, _)| (bucket - b) * DELAY_BUCKET_US >= FLOOR_SPAN_US)
        {
            self.buckets.pop_front();
        }
        self.floor = self.buckets.iter().map(|&(_, d)| d).min();
    }
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
    /// What the last message showed the link carrying, see [`Self::measured`].
    measured: Option<f64>,
    overusing_since: Option<Instant>,
    last_overusing: Option<Instant>,
    /// Per message: (agent instant, received, lost); a late arrival after a loss is a
    /// correction (+1, −1).
    losses: VecDeque<(Instant, i64, i64)>,
    last_update: Instant,
    last_decrease: Option<Instant>,
    last_loss_decrease: Option<Instant>,
    /// v2's hold (`HOLD`).
    held: Option<HoldLine>,
    /// The last report gave a shortfall reading: a second in a row sets the hold.
    read_last: bool,
    /// The lossy run going on, and the datagrams the last message counted (`measure`).
    episode: Option<Run>,
    counted: (i64, i64),
    /// The probe's datagrams the last message counted, (received, lost), and when a probe last
    /// met the link's edge (its own datagrams lost, video's not).
    probe_counted: (i64, i64),
    probed_at: Option<Instant>,
    /// What the link lost with no probe running, (received, lost) decayed per report: a probe is
    /// judged on the loss it adds, so random loss under the floor's notice does not stop it.
    unprobed: (f64, f64),
    /// The first loss `counted` was a keyframe's (`Slot::key`).
    key_lost: Option<bool>,
    /// Since when every window with enough datagrams has lost over `LOSS_HIGH`.
    heavy_since: Option<Instant>,
    /// When a burst last showed its loss did not fall with the sender's rate: loss bursts set no
    /// line for `HOLD` after it.
    random_burst: Option<Instant>,
    /// What the encoder is asked to send now, setpoint plus FEC, kbit/s on the wire; `None`
    /// counts every window busy. Only a busy window sets or renews the hold.
    pub(crate) encoder_kbps: Option<f64>,
    /// v2: the open loss-floor bin (start, received, lost), the closed ones (datagrams a
    /// second, loss), and the loss floor ([`Self::loss_floor`]).
    floor_bin: Option<(Instant, i64, i64)>,
    floor_bins: VecDeque<(f64, f64)>,
    floor: f64,
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
    evidence: Evidence,
    /// `QUASAR_WT_CAPACITY=legacy`: 0.6.3's reading, and no blackouts.
    pub(crate) legacy: bool,
    /// v2's hold (`HOLD`) and loss floor; off for PyroWave, which has its own controller and no
    /// keyframes to overflow a link.
    holds: bool,
    /// The sender can probe with repair datagrams (`PROBE_GAIN`): the client decodes FEC.
    pub(crate) probes: bool,
    /// The bound the log last named, and when.
    logged: (Bound, Option<Instant>),
    /// The estimate before a silence decay not logged yet; delay and loss cuts always log.
    unlogged: Option<f64>,
}

/// v2's hold (`HOLD`).
#[derive(Debug, Clone, Copy)]
struct HoldLine {
    /// The line, kbit/s.
    kbps: f64,
    /// The last report that lost `LOSS_LOW` or more, and the last that lowered the line.
    since: Instant,
    stepped: Instant,
    /// The most the link delivered while dropping since the hold began.
    trouble: f64,
    /// The lossy run that last lowered the line: (began, its loss over the run, the send rate it
    /// began at).
    cause: (Instant, f64, f64),
    /// Loss lowered the line (a burst, or a probe meeting loss): the link drops what overflows.
    dropped: bool,
    /// The repair-datagram probe going on (`PROBE_GAIN`), and since when the next one waits
    /// (`PROBE_WAIT`).
    probe: Option<Probe>,
    probe_wait: Instant,
}

/// One probe step: the total on the wire it fills to, since when, and what the reports since then
/// counted: (received, lost) of every datagram, and the probe's own datagrams.
#[derive(Debug, Clone, Copy)]
struct Probe {
    kbps: f64,
    since: Instant,
    counted: (i64, i64),
    probes: i64,
    probe_lost: i64,
    /// The reports' send rates (sum, count): a step proves what was sent, at most its total.
    sent: (f64, u32),
}

/// v2's lossy run: a burst, its ramp and tail (`Estimator::apply`).
#[derive(Debug, Clone, Copy)]
struct Run {
    at: Instant,
    /// Datagrams received and lost in it.
    counted: (i64, i64),
    /// The send rate it began at, and what the encoder was asked then (`Estimator::encoder_kbps`).
    from: f64,
    asked: Option<f64>,
    /// The most its loss so far reached.
    peak: f64,
    /// It began with a keyframe's loss (`Slot::key`): a policer the line sits just under
    /// overflows on a keyframe, a short burst at a constant rate that ends on its own, as Wi-Fi's
    /// does. A Wi-Fi burst does not begin on keyframes.
    key: bool,
}

/// A congested window this long after the line last stepped down shows the step did not cure
/// it: the line steps again. Shorter, it is the same burst (the loss window is 500 ms).
const HOLD_STEP_GAP: Duration = Duration::from_secs(1);
/// A loss this soon after a keyframe datagram was sent is the keyframe's (`Run::key`): seat B's
/// M1' rows lost first 13 and 27 ms after one (3800, 4000 under the lab encoder).
const KEY_SPILL: Duration = Duration::from_millis(50);

/// What one message measured, for the tests.
#[derive(Debug, Clone, Copy, PartialEq)]
pub(crate) struct Measured {
    pub(crate) usage: Usage,
    pub(crate) rate_kbps: Option<f64>,
    /// The receive rate with the `GAPS_TAKEN_OUT` largest excesses of an arrival gap over its send
    /// gap, `gap_us` in all, taken out of the span: what the ABR's reading reads (rule 2).
    pub(crate) delivered_kbps: Option<f64>,
    pub(crate) gap_us: u64,
    /// The rate every video datagram sent over the same span was sent at, lost ones
    /// included, on the agent's clock; `None` when the message added no fresh arrival.
    pub(crate) sent_kbps: Option<f64>,
    /// The capacity probe's datagrams over the same span (`PROBE_GAIN`), apart from `sent_kbps`.
    pub(crate) probe_sent_kbps: f64,
    /// The rate the datagrams in the window were sent at: what `delivered_kbps` is read against.
    pub(crate) arrived_sent_kbps: Option<f64>,
    /// A video datagram (not a resend) first reported here is in the rate window.
    pub(crate) fresh: bool,
    /// Blackouts left out.
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
            held: None,
            read_last: false,
            episode: None,
            counted: (0, 0),
            probe_counted: (0, 0),
            probed_at: None,
            unprobed: (0.0, 0.0),
            key_lost: None,
            heavy_since: None,
            random_burst: None,
            encoder_kbps: None,
            floor_bin: None,
            floor_bins: VecDeque::new(),
            floor: 0.0,
            last_report: None,
            last_feedback: None,
            last_decay: None,
            loss: None,
            bound: Bound::Ceiling,
            recover_to: None,
            outage: None,
            reported_to: None,
            evidence: Evidence::default(),
            legacy: false,
            holds: true,
            probes: false,
            logged: (Bound::Ceiling, None),
            unlogged: None,
            measured: None,
        }
    }

    pub(crate) fn estimate_kbps(&self) -> f64 {
        self.estimate_kbps
    }

    /// The link set the estimate: a delay or loss cut holds it under the ceiling. A silence
    /// decay does not count while its regrow memory is held (#1417): the link has not said it
    /// carries less, and pacing a recovery keyframe at a decayed rate only delays it.
    pub(crate) fn found_link(&self) -> bool {
        (self.bound != Bound::Ceiling || self.held.is_some()) && self.recover_to.is_none()
    }

    /// The hold's line (`HOLD`), kbit/s on the wire, while it is on.
    pub(crate) fn held_kbps(&self) -> Option<f64> {
        self.held.map(|h| h.kbps.max(MIN_KBPS))
    }

    /// The hold is on and the link drops what overflows it: `VideoQueue::on_hold`. A hold from
    /// stretched readings alone is a queue, which keeps a keyframe, and where held video must
    /// still drain past the pace to give the readings that ratchet the line.
    pub(crate) fn drop_held(&self) -> bool {
        self.held.is_some_and(|h| h.dropped)
    }

    /// `kbps`, never over the hold's line (`HOLD`): what governs the sender and feeds the ABR.
    pub(crate) fn hold(&self, kbps: f64) -> f64 {
        self.held.map_or(kbps, |h| kbps.min(h.kbps.max(MIN_KBPS)))
    }

    /// The total the repair-datagram probe fills the wire to (`PROBE_GAIN`), kbit/s, while one runs.
    pub(crate) fn probe_kbps(&self) -> Option<f64> {
        self.held.and_then(|h| h.probe).map(|p| p.kbps)
    }

    /// One report into the repair-datagram probe (`PROBE_GAIN`): what it counted of video and policy
    /// FEC, what was sent with the probe, and `stopped`, a sign the link is full (a line set or
    /// renewed, a reading, overuse). Loss over the base on everything sent in a step, or a stop,
    /// ends the probe at once; a step carried clean raises the line and starts the next.
    fn probe_step(
        &mut self,
        (received, lost): (i64, i64),
        sent: Option<f64>,
        stopped: bool,
        now: Instant,
    ) {
        let probed = std::mem::take(&mut self.probe_counted);
        if self.probe_kbps().is_none() {
            let (r, l) = self.unprobed;
            self.unprobed = (
                r * UNPROBED_DECAY + received as f64,
                l * UNPROBED_DECAY + lost as f64,
            );
        }
        let (r, l) = self.unprobed;
        let floor = self.loss_floor().max(l.max(0.0) / (r + l).max(1.0));
        let Some(h) = self.held.as_mut().filter(|_| self.probes) else {
            return;
        };
        if let Some(mut p) = h.probe.take() {
            p.counted.0 += received + probed.0;
            p.counted.1 += lost + probed.1;
            p.probes += probed.0 + probed.1;
            p.probe_lost += probed.1;
            if let Some(kbps) = sent {
                p.sent = (p.sent.0 + kbps, p.sent.1 + 1);
            }
            let n = p.counted.0 + p.counted.1;
            let loss = p.counted.1.max(0) as f64 / n.max(1) as f64;
            let now_n = received + lost + probed.0 + probed.1;
            let now_loss = (lost + probed.1).max(0) as f64 / now_n.max(1) as f64;
            // Over the base by a share and by `PROBE_LOST_MIN` datagrams: two lost of 40 is 5 %,
            // and random loss does that.
            let excess = |n: i64, loss: f64, share: f64| {
                loss - floor >= share && (loss - floor) * n as f64 >= PROBE_LOST_MIN
            };
            let over = excess(n, loss, LOSS_LOW) || excess(now_n, now_loss, LOSS_HIGH);
            let result = if stopped || over {
                h.probe_wait = now;
                // Behind each frame the probe's own datagrams overflow first; a Wi-Fi burst drops
                // video and probe alike.
                let probe_loss = p.probe_lost.max(0) as f64 / p.probes.max(1) as f64;
                let video = (n - p.probes, p.counted.1 - p.probe_lost);
                let video_loss = video.1.max(0) as f64 / video.0.max(1) as f64;
                if probe_loss >= LOSS_LOW && probe_loss >= 2.0 * video_loss {
                    self.probed_at = Some(now);
                }
                "stopped"
            } else if now.saturating_duration_since(p.since) >= PROBE_STEP && p.probes >= PROBE_MIN
            {
                let carried = p.kbps.min(p.sent.0 / f64::from(p.sent.1.max(1)));
                h.kbps = h.kbps.max(BETA * BETA * carried);
                // Carried at the most a probe tries: nothing left to hold.
                if p.kbps >= 0.999 * self.ceiling_kbps / BETA / BETA {
                    h.kbps = self.ceiling_kbps;
                }
                "raised"
            } else {
                h.probe = Some(p);
                return;
            };
            tracing::info!(
                token = "webtransport-hold-probe",
                result,
                probe_kbps = p.kbps as u64,
                held_kbps = h.kbps as u64,
                loss_pct = loss * 100.0,
                floor_pct = floor * 100.0,
                "WebTransport capacity probe at {:.0} kbps {result}; line {:.0} kbps",
                p.kbps,
                h.kbps
            );
        }
        let since = h.stepped.max(h.probe_wait);
        if !stopped && now.saturating_duration_since(since) >= PROBE_WAIT {
            h.probe = Some(Probe {
                kbps: (h.kbps * PROBE_GAIN).min(self.ceiling_kbps / BETA / BETA),
                since: now,
                counted: (0, 0),
                probes: 0,
                probe_lost: 0,
                sent: (0.0, 0),
            });
        }
        if h.kbps >= self.ceiling_kbps {
            self.end_hold("probe-reached-ceiling");
        }
    }

    /// Ends the hold (`HOLD`), naming the path that ended it for the lab.
    fn end_hold(&mut self, reason: &'static str) {
        if let Some(h) = self.held.take() {
            tracing::info!(
                token = "webtransport-hold-ended",
                reason,
                held_kbps = h.kbps as u64,
                cause_loss_pct = h.cause.1 * 100.0,
                set_by_loss = h.dropped,
                "WebTransport capacity hold at {:.0} kbps ended ({reason})",
                h.kbps
            );
        }
    }

    /// Start again from `kbps` (never above the ceiling): codec-pyrowave.md §Rate, the ABR
    /// resumes from the estimate after a switch back.
    pub(crate) fn resume_at(&mut self, kbps: f64) {
        self.delay_kbps = kbps;
        self.loss_kbps = kbps;
        self.read_last = false;
        self.end_hold("codec-switch");
        self.stop_regrow("codec switch");
        self.settle();
    }

    /// The loss fraction the last message's window measured, blackouts and an outage's own
    /// losses left out; `None` on too few datagrams. The estimate's loss bound and FEC both read
    /// it (XOR cannot rebuild a blackout). Lost repair datagrams count: they crossed the same
    /// path.
    pub(crate) fn loss(&self) -> Option<f64> {
        self.loss
    }

    /// Loss the link shows whatever the sender does (Wi-Fi), which v2 does not read as
    /// congestion; 0 in legacy and for PyroWave. See [`Self::floor_bin_closed`].
    fn loss_floor(&self) -> f64 {
        match !self.legacy && self.holds {
            true => self.floor,
            false => 0.0,
        }
    }

    /// The discriminator: congestion loss falls when the sender's rate falls, random loss does
    /// not. The floor rises to the least loss of the last 16 s only when bins sent at
    /// `FLOOR_SPREAD` or less of the busiest bin's rate lost `FLOOR_FLAT` of what the busier
    /// bins did; any bin losing less lowers it at once. A link the sender overloads loses less
    /// once the readings step it down, so it never grants a floor, and loss above the floor
    /// still cuts the loss bound and gives a reading.
    fn floor_bin_closed(&mut self, rate: f64, loss: f64) {
        self.floor_bins.push_back((rate, loss));
        if self.floor_bins.len() > FLOOR_BINS {
            self.floor_bins.pop_front();
        }
        self.floor = self.floor.min(loss);
        let top = self.floor_bins.iter().map(|b| b.0).fold(0.0, f64::max);
        let mean = |low: bool| {
            let v: Vec<f64> = self
                .floor_bins
                .iter()
                .filter(|b| (b.0 <= FLOOR_SPREAD * top) == low)
                .map(|b| b.1)
                .collect();
            (!v.is_empty()).then(|| v.iter().sum::<f64>() / v.len() as f64)
        };
        if let (FLOOR_BINS, Some(low), Some(high)) =
            (self.floor_bins.len(), mean(true), mean(false))
        {
            if low >= FLOOR_FLAT * high {
                let least = self
                    .floor_bins
                    .iter()
                    .map(|b| b.1)
                    .fold(LOSS_HIGH, f64::min);
                self.floor = self.floor.max(least);
                // Backing off did not stop the loss: it is not the link's capacity. Under half the
                // hold's renewal line it is too little to keep a hold alive anyway.
                // A hold that probes with repair datagrams keeps its line and climbs over the floor
                // instead: putting the pre-hold rate back cost a freeze each time (seat B S2).
                if least >= LOSS_LOW / 2.0 && !self.probes {
                    self.end_hold("loss-floor");
                }
            }
        }
    }

    /// The window's loss over the loss floor: what the estimate's loss bound and the hold read.
    fn excess_loss(&self) -> Option<f64> {
        self.loss.map(|l| (l - self.loss_floor()).max(0.0))
    }

    /// The ABR's reading (#1376, rule 2 of #1433): `BETA` × what the link delivered, kbit/s, when
    /// the datagrams that arrived came over the window slower than `LINK_LIMITED` × the rate they
    /// were sent at and the sender was not application-limited. `None` otherwise: a clean link,
    /// whatever its delay detector says (#68), loss (the hold's), a delay step or a blackout (one
    /// gap, taken out of `delivered`). Never the estimate, which may float over the link while the
    /// sender is application-limited (#1503).
    pub(crate) fn measured(&self) -> Option<f64> {
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
        let was_held = self.held.is_some();
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

        let excess = self.excess_loss();
        match excess {
            Some(loss) if loss > LOSS_HIGH => {
                let gap = LOSS_DECREASE_GAP + path.rtt;
                if self
                    .last_loss_decrease
                    .is_none_or(|at| now.saturating_duration_since(at) >= gap)
                {
                    self.loss_kbps = self.estimate_kbps * (1.0 - 0.5 * loss);
                    self.last_loss_decrease = Some(now);
                    if self.loss_kbps < self.estimate_kbps {
                        cut_by.push("loss");
                        // The link no longer carries what it did before the outage.
                        self.stop_regrow("loss cut");
                        // These losses are spent; the next decision reads new ones.
                        self.losses.clear();
                    }
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

        // Rule 2 (#1433): arrivals stretched past their send spacing after the gap correction.
        // Loss is no reading: the datagrams that arrived are measured against their own sending.
        let read = m
            .delivered_kbps
            .zip(m.arrived_sent_kbps)
            .filter(|&(d, s)| m.fresh && !app_limited && d < LINK_LIMITED * s)
            .map(|(d, _)| BETA * d);
        // The hold (`HOLD`): what a congested window delivered, or a confirmed reading. Only while
        // the encoder fills what it is asked: a quiet scene's rate is not the link's.
        let holds = self.holds && !self.legacy;
        self.heavy_since = match m.loss {
            Some(l) if l > LOSS_HIGH => self.heavy_since.or(Some(now)),
            Some(_) => None,
            None => self.heavy_since,
        };
        let fill = self
            .encoder_kbps
            .map(|e| m.sent_kbps.unwrap_or(0.0) / e.max(1.0));
        // An encoder under its setpoint still overloads a link it sends over: heavy loss that lasts
        // a window counts (S1: 0.7 × into the R4 policer). A still screen's one burst does not.
        let sustained = self
            .heavy_since
            .is_some_and(|at| now.saturating_duration_since(at) >= LOSS_WINDOW);
        let busy = fill.is_none_or(|f| f >= APP_LIMITED || (f >= UNDERFILL && sustained));
        let congested = holds && busy && m.loss.is_some_and(|l| l > LOSS_HIGH);
        let sent = m.sent_kbps.unwrap_or(0.0);
        let loss = m.loss.unwrap_or(0.0);
        // The lossy run going on (a burst, ramp and tail): reports with too few datagrams to judge
        // do not end it, and its loss is counted over it (a loss cut restarts the window, so one
        // window's loss is noise).
        let (r, l) = std::mem::take(&mut self.counted);
        let key_lost = std::mem::take(&mut self.key_lost);
        let prev = self.episode;
        self.episode = match self.episode {
            Some(run) if m.loss.is_none_or(|l| l >= LOSS_LOW) => Some(Run {
                counted: (run.counted.0 + r, run.counted.1 + l),
                ..run
            }),
            _ if holds && busy && loss >= LOSS_LOW => Some(Run {
                at: now,
                counted: (r, l),
                from: sent,
                asked: self.encoder_kbps,
                peak: 0.0,
                key: key_lost == Some(true),
            }),
            _ => None,
        };
        // Direct `apply` (tests) counts nothing: the window's loss stands in.
        let run_loss = self.episode.map_or(loss, |run| {
            let (r, l) = run.counted;
            match r + l >= LOSS_MIN_SAMPLES {
                true => l.max(0) as f64 / (r + l) as f64,
                false => loss,
            }
        });
        if let Some(run) = self.episode.as_mut() {
            run.peak = run.peak.max(run_loss);
            if let Some(h) = self.held.as_mut().filter(|h| h.cause.0 == run.at) {
                h.cause.1 = run_loss;
            }
        }
        // A run that began a step after the line last came down is a new burst. A new burst
        // lowers the line only once its window holds it (`LOSS_WINDOW`).
        let later = self
            .episode
            .zip(self.held)
            .filter(|&(run, h)| run.at.saturating_duration_since(h.stepped) >= HOLD_STEP_GAP);
        // W2e's discriminator per burst, once the burst is over: its loss peaked as high as the line's
        // run lost over it, at a rate under the one that run began at, so it did not fall with the
        // rate (a later run's tail is longer: the sender no longer drops inside it), and it ended
        // with the line where it was and the encoder asked as much, so nothing the sender did
        // ended it (Wi-Fi). Capacity loses as much at a lower rate too, but until the sender drops
        // (seat B M1: a second cap step), or only on keyframes (M1', `Run::key`). Only a hold set
        // by loss: readings lost nothing.
        // A probe found the link full this close to the burst (sim, second step to 3800 under the
        // lab encoder: the line just over the policer, released 2 s after a probe, back to 1080p).
        let edge = self.probed_at;
        let released = prev
            .filter(|_| self.episode.is_none())
            .zip(self.held)
            .is_some_and(|(run, h)| {
                h.dropped
                    && !run.key
                    && edge.is_none_or(|at| at + PROBE_WAIT <= run.at)
                    && h.cause.1 >= LOSS_HIGH
                    && run.at.saturating_duration_since(h.stepped) >= HOLD_STEP_GAP
                    && run.peak >= FLOOR_FLAT * h.cause.1
                    && run.from <= FLOOR_SPREAD * h.cause.2
                    && run
                        .asked
                        .zip(self.encoder_kbps)
                        .is_none_or(|(was, now)| now >= FLOOR_SPREAD * was)
            });
        if released {
            self.random_burst = Some(now);
            self.end_hold("burst-not-capacity");
        }
        // The burst memory never blocks loss lasting `HOLD_STEP_GAP`: a release it got wrong (seat B
        // M1': 3800 under the lab encoder's keyframes) otherwise cost 61 s with no frame.
        let random = self
            .random_burst
            .is_some_and(|at| now.saturating_duration_since(at) < HOLD)
            && self
                .episode
                .is_none_or(|run| now.saturating_duration_since(run.at) < HOLD_STEP_GAP);
        let settled =
            later.is_none_or(|(run, _)| now.saturating_duration_since(run.at) >= LOSS_WINDOW);
        let burst = m
            .rate_kbps
            .filter(|_| congested && !random && settled)
            .map(|r| BETA * r);
        // A probe that meets loss found the link: back off from it, as from a burst.
        let probing = !self.probes
            && self
                .held
                .is_some_and(|h| now.saturating_duration_since(h.since) >= HOLD);
        let met = m
            .rate_kbps
            .filter(|_| probing && busy && excess.is_some_and(|l| l >= LOSS_LOW))
            .map(|r| BETA * r);
        let confirmed = read.filter(|_| self.read_last && holds);
        self.read_last = read.is_some();
        if let Some(line) = [burst, met, confirmed]
            .into_iter()
            .flatten()
            .reduce(f64::min)
        {
            let cause = self
                .episode
                .map_or((now, loss, sent), |run| (run.at, run_loss, run.from));
            let mut h = self.held.unwrap_or(HoldLine {
                kbps: f64::INFINITY,
                since: now,
                stepped: now,
                trouble: 0.0,
                cause,
                dropped: false,
                probe: None,
                probe_wait: now,
            });
            // Still congested a step after the line came down, with the sender under what the link
            // then delivered or the link now delivering under the line: that line did not cure it.
            // A sender over the line (its floor) delivering as much as before compounds nothing.
            let again = now.saturating_duration_since(h.stepped) >= HOLD_STEP_GAP
                && (line / BETA < h.kbps || sent <= h.kbps / BETA);
            let to = line.min(if again { BETA * h.kbps } else { h.kbps });
            if to < h.kbps {
                (h.kbps, h.stepped, h.cause) = (to, now, cause);
            }
            (h.since, h.trouble) = (now, h.trouble.max(line / BETA));
            h.dropped |= burst.or(met).is_some();
            self.held = Some(h);
        } else if let Some(h) = self.held.as_mut() {
            if busy && excess.is_some_and(|l| l >= LOSS_LOW) {
                h.since = now;
            } else if probing {
                // Gently up to where the link dropped, at the estimate's pace past it.
                let per_s = if h.kbps < h.trouble {
                    PROBE_PER_S
                } else {
                    GROWTH_PER_S
                };
                h.kbps *= per_s.powf(dt);
                if h.kbps >= self.ceiling_kbps {
                    self.end_hold("probe-reached-ceiling");
                }
            }
        }
        let stopped = burst.or(met).or(confirmed).or(read).is_some() || m.usage == Usage::Overusing;
        let sent = m.sent_kbps.map(|s| s + m.probe_sent_kbps);
        self.probe_step((r, l), sent, stopped, now);

        let was = self.estimate_kbps;
        self.settle();
        if self
            .recover_to
            .is_some_and(|(to, _)| self.estimate_kbps >= to)
        {
            self.stop_regrow("reached");
        }
        // After `settle`: legacy's reading is at least this report's estimate, as 0.6.3's.
        self.measured = match self.legacy {
            false => read,
            true => m
                .rate_kbps
                .zip(m.sent_kbps)
                .filter(|&(r, s)| m.fresh && !app_limited && r < LINK_LIMITED * s)
                .map(|(r, _)| self.estimate_kbps.max(BETA * r)),
        };
        let due = self.log_due(now)
            && (self.unlogged.is_some() || self.bound != self.logged.0 || self.measured.is_some());
        let hold_began = self.held.is_some() && !was_held;
        if !cut_by.is_empty() || due || hold_began {
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
                delivered_kbps = m.delivered_kbps.unwrap_or(0.0) as u64,
                gap_ms = m.gap_us as f64 / 1000.0,
                sent_kbps = m.sent_kbps.unwrap_or(0.0) as u64,
                encoder_kbps = self.encoder_kbps.unwrap_or(0.0) as u64,
                fill = fill.unwrap_or(1.0),
                busy,
                app_limited,
                base_kbps = base as u64,
                loss_pct = m.loss.unwrap_or(0.0) * 100.0,
                reading_kbps = self.measured.unwrap_or(0.0) as u64,
                delay_kbps = self.delay_kbps as u64,
                loss_kbps = self.loss_kbps as u64,
                held_kbps = self.held.map_or(0, |h| h.kbps as u64),
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
        // At or after `from`, in u32 serial arithmetic.
        let after = |seq: u32, from: u32| seq.wrapping_sub(from) < 1 << 31;
        let end = fb.first_pkt_seq.wrapping_add(fb.arrivals.len() as u32);
        // A pending run continues only into the report that starts where its own ended, and
        // only within `RUN_MAX_WAIT`.
        let mut lost = 0i64;
        let continues = self.reported_to == Some(fb.first_pkt_seq);
        if !continues || self.run_expired(now) {
            lost += self.end_run(log, None, now);
        }
        self.evidence.run_lost &= continues;
        if self.reported_to.is_none_or(|to| after(end, to)) {
            self.reported_to = Some(end);
        }
        // (received, lost) video and policy FEC datagrams.
        let mut video = (0i64, lost);
        let mut newest_fresh = None;
        for (i, &arrival) in fb.arrivals.iter().enumerate() {
            let seq = fb.first_pkt_seq.wrapping_add(i as u32);
            let Some(slot) = log.slot(seq) else {
                // Audio shares `pkt_seq` and is not logged, but its arrival is traffic.
                if arrival != NOT_RECEIVED && !self.evidence.run.is_empty() {
                    self.evidence.run_heard = true;
                }
                continue;
            };
            let at_us = (arrival != NOT_RECEIVED)
                .then(|| fb.ref_us.checked_add(u64::from(arrival) * UNIT_US))
                .flatten();
            let (resend, sent_us) = (slot.resend, slot.sent_us);
            // A probe's datagram is counted apart: its loss harms no frame (`PROBE_GAIN`).
            let count = match slot.probe {
                true => &mut self.probe_counted,
                false => &mut video,
            };
            match (slot.status, at_us) {
                (Status::Received, _) | (Status::Lost | Status::Outage, None) => continue,
                (Status::Outage, Some(_)) => {
                    // Late: no loss was counted for it, so there is none to undo; it is no
                    // delay sample either.
                    slot.status = Status::Received;
                    count.0 += 1;
                }
                (Status::Unreported, None) => {
                    if self.outage.is_some_and(|o| after(seq, o.from)) {
                        slot.status = Status::Outage;
                    } else {
                        // Counted, or found a blackout, once an arrival ends the run.
                        // Unreported until then: the client names each `pkt_seq` once.
                        let ev = &mut self.evidence;
                        if ev.run.is_empty() {
                            ev.run_from_floor = ev.at_floor;
                            ev.run_since = Some(now);
                            ev.run_heard = false;
                            ev.run_broken = false;
                        }
                        ev.run_broken |= ev.run_heard;
                        ev.run.push(seq);
                    }
                    continue;
                }
                (Status::Lost, Some(_)) => {
                    // Late: the loss is undone, but it is no delay sample.
                    slot.status = Status::Received;
                    count.1 -= 1;
                    count.0 += 1;
                }
                (Status::Unreported, Some(at)) => {
                    slot.status = Status::Received;
                    count.0 += 1;
                    // The link answered: a loss sent after this is congestion (#1557).
                    if self.outage.is_some_and(|o| after(seq, o.by)) {
                        self.outage = None;
                    }
                    if let Some((send_d, arrival_d, arrival_ms)) =
                        self.inter_arrival.push(sent_us, at)
                    {
                        self.trendline.update(send_d, arrival_d, arrival_ms);
                    }
                    let at_floor = self.evidence.arrival(sent_us, at);
                    video.1 += self.end_run(log, Some(at_floor), now);
                    self.evidence.at_floor = at_floor;
                }
            }
            if let (false, Some(at)) = (resend, at_us) {
                let bytes = log.peek(seq).map_or(0, |s| u32::from(s.bytes));
                self.arrivals.push_back((at, bytes, sent_us));
                self.newest_arrival = self.newest_arrival.max(at);
                newest_fresh = newest_fresh.max(Some(at));
                if self.arrivals.len() > MAX_ARRIVALS {
                    self.arrivals.pop_front();
                }
            }
        }

        // No arrival ended it in time: its losses.
        if self.run_expired(now) {
            video.1 += self.end_run(log, None, now);
        }
        let (received, lost) = video;

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
        // Rule 2's delivered rate (#1433): the arrival span less the `GAPS_TAKEN_OUT` largest
        // excesses of an arrival gap over the send gap between the same two datagrams, not the
        // largest arrival gaps: an unpaced frame's gap can be longer than a step inside a burst.
        // A delay step or a blackout leaves one such gap; a deep buffer stretches every gap, and
        // keeps its shortfall.
        let mut gap_us = 0;
        let delivered_kbps = rate_kbps.and_then(|r| {
            let mut order: Vec<(u64, u64)> =
                self.arrivals.iter().map(|&(t, _, s)| (t, s)).collect();
            order.sort_unstable();
            let mut excess: Vec<u64> = order
                .windows(2)
                .map(|w| (w[1].0 - w[0].0).saturating_sub(w[1].1.saturating_sub(w[0].1)))
                .collect();
            excess.sort_unstable_by(|a, b| b.cmp(a));
            gap_us = excess.iter().take(GAPS_TAKEN_OUT).sum();
            let span = order.last()?.0 - order.first()?.0;
            (span > gap_us).then(|| r * span as f64 / (span - gap_us) as f64)
        });
        // Over every datagram sent in the span, lost ones too: a busy sender under loss is
        // still busy. Datagrams all sent in one instant give no send rate.
        let first_sent = self.arrivals.iter().map(|a| a.2).min();
        let last_sent = self.arrivals.iter().map(|a| a.2).max();
        // The probe's bytes apart (`probe_sent_kbps`): the hold reads what the encoder sends.
        let (sent_kbps, probe_sent_kbps) = match (fresh, rate_kbps, first_sent, last_sent) {
            (true, Some(_), Some(lo), Some(hi)) if hi > lo => {
                // A blackout's datagrams are out of delivered and sent alike.
                let (sent, probe) = log
                    .slots
                    .iter()
                    .filter(|s| s.bytes > 0 && !s.resend && s.status != Status::Outage)
                    .filter(|s| s.sent_us > lo && s.sent_us <= hi)
                    .fold((0u64, 0u64), |(v, p), s| match s.probe {
                        true => (v, p + u64::from(s.bytes)),
                        false => (v + u64::from(s.bytes), p),
                    });
                let kbps = |b: u64| b as f64 * 8.0 * 1000.0 / (hi - lo) as f64;
                (Some(kbps(sent)), kbps(probe))
            }
            _ => (None, 0.0),
        };
        let arrived_sent_kbps = rate(|a| a.2, 1).filter(|_| sent_kbps.is_some());

        if received != 0 || lost != 0 {
            let bin = self.floor_bin.get_or_insert((now, 0, 0));
            bin.1 += received;
            bin.2 += lost;
            let (since, r, l) = *bin;
            let span = now.saturating_duration_since(since);
            if span >= FLOOR_BIN && r + l >= FLOOR_BIN_SAMPLES {
                self.floor_bin = None;
                let n = (r + l) as f64;
                self.floor_bin_closed(n / span.as_secs_f64(), l.max(0) as f64 / n);
            }
            self.counted = (received, lost);
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
            delivered_kbps,
            gap_us,
            sent_kbps,
            probe_sent_kbps,
            arrived_sent_kbps,
            fresh,
            loss,
        }
    }

    fn run_expired(&self, now: Instant) -> bool {
        self.evidence
            .run_since
            .is_some_and(|at| now.saturating_duration_since(at) >= RUN_MAX_WAIT)
    }

    /// The one place a run of loss is decided, once: its datagrams leave the run as lost or
    /// as a blackout here, and nothing later reads them as either. A run decided as loss with
    /// no arrival to end it (`at_floor` `None`) stays loss through its contiguous tail. An
    /// arrival at the floor (`at_floor`) makes it a blackout when the run is solid over
    /// `BLACKOUT_MIN_US` of send time and `BLACKOUT_MIN_DATAGRAMS`, with no queue on either
    /// side (a 60–300 ms drop: the client's NACK and keyframe request repair it). Anything
    /// else is its losses: a run open `RUN_MAX_WAIT` or more is loss whatever closes it, since
    /// hiding congestion as a blackout is the dangerous mistake. Returns the losses counted.
    fn end_run(&mut self, log: &mut SendLog, at_floor: Option<bool>, now: Instant) -> i64 {
        let expired = self.run_expired(now);
        let run = std::mem::take(&mut self.evidence.run);
        let sent = |seq: &u32| log.peek(*seq).map(|s| s.sent_us);
        let span = run
            .last()
            .and_then(sent)
            .zip(run.first().and_then(sent))
            .map_or(0, |(to, from)| to.saturating_sub(from));
        self.evidence.run_since = None;
        let blackout = !self.legacy
            && !expired
            && !self.evidence.run_lost
            && run.len() >= BLACKOUT_MIN_DATAGRAMS
            && span >= BLACKOUT_MIN_US
            && self.evidence.run_from_floor
            && !self.evidence.run_broken
            && at_floor == Some(true);
        self.evidence.run_lost = at_floor.is_none() && (self.evidence.run_lost || !run.is_empty());
        let mut lost = 0;
        for seq in run {
            let Some(slot) = log.slot(seq) else {
                continue;
            };
            if blackout {
                slot.status = Status::Outage;
            } else if slot.probe {
                slot.status = Status::Lost;
                self.probe_counted.1 += 1;
            } else {
                slot.status = Status::Lost;
                lost += 1;
                self.key_lost.get_or_insert(slot.key);
            }
        }
        lost
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
    /// `legacy`: `QUASAR_WT_CAPACITY=legacy`, 0.6.3's estimator.
    pub(crate) fn new(target_kbps: u32, floor_kbps: u32, now: Instant, legacy: bool) -> Self {
        Self {
            estimator: Estimator {
                legacy,
                holds: false,
                ..Estimator::new(f64::from(target_kbps), now)
            },
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

/// QUIC's window over the governed rate × the path's RTT floor: a keyframe burst and RTT
/// jitter fit under it, and quinn paces at 1.25 × window / RTT, 2.5 × the rate.
const WINDOW_GAIN: f64 = 2.0;
/// The window covers the BDP of any path up to a 200 ms round trip, so a route step under that
/// never makes the window the bottleneck: at 30 ms a +60 ms step held the sender to 6 of its
/// 8 Mbit/s and the ABR read its own window as the link (#1433, lab row R8).
/// shortcut: a route step to over 200 ms RTT is window-limited until the RTT floor adopts it
/// (10–20 s); size on srtt with a queue cap if a real path shows it.
const WINDOW_RTT_MIN: Duration = Duration::from_millis(100);
/// The RTT floor is the least sample of this long, as BBR's: quinn's minimum is for the
/// connection's life, so a longer route on the same path kept the window at the old BDP.
const RTT_FLOOR_SPAN: Duration = Duration::from_secs(10);

/// The least RTT sample over the last `RTT_FLOOR_SPAN`, kept as the least of each second.
#[derive(Default, Clone)]
pub(crate) struct RttFloor {
    /// Samples that may set the floor (`Self::sample`).
    gated: VecDeque<(Instant, Duration)>,
    /// Every sample.
    all: VecDeque<(Instant, Duration)>,
}

/// Folds `rtt` into the second it falls in and forgets seconds past `RTT_FLOOR_SPAN`, keeping
/// the newest; the least kept.
fn fold_rtt(seconds: &mut VecDeque<(Instant, Duration)>, now: Instant, rtt: Duration) -> Duration {
    match seconds.back_mut() {
        Some((at, least)) if now.saturating_duration_since(*at) < Duration::from_secs(1) => {
            *least = (*least).min(rtt)
        }
        _ => seconds.push_back((now, rtt)),
    }
    while seconds.len() > 1
        && seconds
            .front()
            .is_some_and(|&(at, _)| now.saturating_duration_since(at) > RTT_FLOOR_SPAN)
    {
        seconds.pop_front();
    }
    seconds.iter().map(|&(_, d)| d).min().unwrap_or(rtt)
}

impl RttFloor {
    /// Takes one sample; the floor after it. `idle`: the sender had room in its window and
    /// nothing to send (quinn's `app_limited`). Only such a sample, or one at or under the
    /// floor, can set it: one taken while the window was full measures the sender's own queue,
    /// and following it grew the window with the queue (a standing 116 ms queue in the sim at
    /// 5 Mbit/s, against 30 ms; `a_keyframe_waits_in_the_agent_not_in_the_links_queue`).
    /// A whole `RTT_FLOOR_SPAN` with none makes the span's own least sample the floor anyway,
    /// BBR's ProbeRTT without its drain, so a sender that is never idle still follows a longer
    /// route within one or two spans: guarded by
    /// `the_rtt_floor_follows_a_route_step_with_the_sender_never_idle`.
    pub(crate) fn sample(&mut self, now: Instant, rtt: Duration, idle: bool) -> Duration {
        fold_rtt(&mut self.all, now, rtt);
        let floor = self.gated.iter().map(|&(_, d)| d).min();
        if idle || floor.is_none_or(|f| rtt <= f) {
            return fold_rtt(&mut self.gated, now, rtt);
        }
        if self
            .gated
            .back()
            .is_some_and(|&(at, _)| now.saturating_duration_since(at) > RTT_FLOOR_SPAN)
        {
            self.gated = self.all.clone();
        }
        self.gated.iter().map(|&(_, d)| d).min().unwrap_or(rtt)
    }
}

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
    acks: Arc<AtomicU64>,
    legacy: Arc<AtomicBool>,
    /// The rate it was last governed at, bit/s; 0 while Cubic has it.
    held: u64,
    srtt: Duration,
    /// [`RttFloor`]'s: the least RTT sample of the last 10 s.
    min_rtt: Duration,
    floor: RttFloor,
    mtu: u16,
    /// Cubic's first window, which quinn also reads at a path change.
    first: u64,
}

/// Builds a connection's [`Window`]; its handle is the connection's [`Governor`].
pub(crate) struct WindowFactory(pub(crate) Governor);

/// What sets a connection's window: the estimate, while it governs the encoder. Also counts
/// the packets QUIC newly acknowledged ([`Self::acks`]).
#[derive(Clone, Default)]
pub(crate) struct Governor(
    Arc<AtomicU64>,
    Arc<AtomicBool>,
    Arc<AtomicU64>,
    Arc<AtomicBool>,
);

impl Governor {
    /// `QUASAR_WT_CAPACITY=legacy`: the window is sized on the smoothed RTT, as in 0.6.3.
    pub(crate) fn legacy(&self, legacy: bool) {
        self.3.store(legacy, Ordering::Relaxed);
    }

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

    /// Packets QUIC newly acknowledged: proof the path carries what is sent. Never quinn's count
    /// of ACK frames: a peer that still sends (a `nack` on the control stream) repeats ACK frames
    /// on a path that carries nothing to it, and one resumed video into an outage
    /// (`video_behind_a_stalled_path_is_skipped_not_queued`).
    pub(crate) fn acks(&self) -> u64 {
        self.2.load(Ordering::Relaxed)
    }
}

/// Rule 1 (#1433): the rate the pacer and QUIC's window are governed at. What the encoder sends,
/// its setpoint plus the FEC share `charged`, when the estimate is under it, and never under the
/// ABR floor: the encoder's average output always leaves the agent, so an overload shows at the
/// link as loss and delay, not as held video the stall guard reads as a dead path (lab row R3).
pub(crate) fn governed_kbps(estimate: f64, setpoint: f64, charged: f64, floor: f64) -> f64 {
    estimate.max(setpoint * (1.0 + charged)).max(floor)
}

/// What the ABR is fed from an estimate, kbit/s of video: the estimate less the FEC share
/// `charged`. The hold (`HOLD`) bounds the estimate, so the ABR cannot climb back over the link.
pub(crate) fn abr_estimate(kbps: f64, charged: f64) -> f64 {
    kbps / (1.0 + charged)
}

/// The rates `VideoQueue` holds video to, kbit/s: QUIC's room ([`VideoQueue::room`]) at the
/// governed rate or the estimate if lower, and the pacer ([`VideoQueue::pace`]) at the
/// governed rate ([`governed_kbps`]) once the estimate is under the ceiling. `None`: the
/// session is not governed (or PyroWave), or nothing to pace.
pub(crate) fn video_rates(
    governor: &Governor,
    estimator: &Estimator,
    pyrowave: bool,
) -> (Option<u64>, Option<u64>) {
    if pyrowave || !governor.governs() || estimator.legacy {
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
            acks: self.0 .2.clone(),
            legacy: self.0 .3.clone(),
            held: 0,
            srtt: Duration::ZERO,
            min_rtt: Duration::ZERO,
            floor: RttFloor::default(),
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

    fn rtt_sample(&mut self, now: Instant, rtt: Duration, idle: bool) {
        self.min_rtt = self.floor.sample(now, rtt, idle);
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
        self.acks.fetch_add(1, Ordering::Relaxed);
        // Before quinn's first sample this is its initial RTT; by `hello` the handshake gave one.
        self.srtt = rtt.get();
        // Per packet: quinn's estimator keeps no latest sample a controller can read.
        self.rtt_sample(now, now.saturating_duration_since(sent), app_limited);
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
                // The RTT floor, not the smoothed RTT, which a queue at the link inflates: the
                // window never grows with a standing queue (#1503).
                let rtt = match self.legacy.load(Ordering::Relaxed) {
                    true => self.srtt,
                    false => self.min_rtt,
                };
                let rtt = rtt.max(WINDOW_RTT_MIN).as_secs_f64();
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
            acks: self.acks.clone(),
            legacy: self.legacy.clone(),
            held: self.held,
            srtt: self.srtt,
            min_rtt: self.min_rtt,
            floor: self.floor.clone(),
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

    /// Called every probe tick with [`Governor::acks`]. Whether to probe the path now: video is
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

    /// Held video went stale with no keyframe held ([`VideoQueue::feed`]): what comes next
    /// depends on it, so units are skipped until the queue drains and a keyframe comes, as
    /// after a stall. The drop emptied the queue, not the path: an ACK after `acks` must prove
    /// the path moves first.
    pub(crate) fn skip_to_keyframe(&mut self, acks: u64) {
        self.probed(acks);
        if !self.skipping {
            tracing::info!(
                token = "webtransport-video-stale",
                "WebTransport held video outlived the NACK hold; dropped, video skipped until a \
                 keyframe"
            );
            self.skipping = true;
            self.backed_up = Holds::default();
            self.asks = Holds::default();
        }
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
/// Video leaves at most this × the governed rate ([`governed_kbps`], never under what the
/// encoder sends) once the estimate is under the ceiling (#1433), so a keyframe waits here,
/// where audio overtakes it, not in the link's queue. quinn alone paces at 1.25 × window / RTT,
/// 2.5 × the rate or more. Must stay over 1, or held video never drains.
const PACE_GAIN: f64 = 1.25;
/// The pacer's burst: this long at the pace, at least two datagrams.
const PACE_BURST_MS: f64 = 5.0;
/// Video that has waited this long is not paced: a paced keyframe of five frames' bytes waits
/// at most 5 / (60 fps × `PACE_GAIN`), 67 ms, so this is a path back from an outage, or a
/// source over the pace, and it drains at QUIC's own pace as before #1433.
const PACE_MAX_WAIT: Duration = Duration::from_millis(100);
/// `PACE_MAX_WAIT` while a link that drops what overflows it holds the sender
/// (`VideoQueue::on_hold`): a keyframe of 15 average frames drains at the pace in 200 ms. Into
/// that link unpaced its tail is lost, and so are the frames held behind it (sim R4 at 10 ×
/// keyframes: 3 freezes, 23 % loss and the rung back up mid-shape, paced 1 freeze; R3 at 15 ×: 9
/// freezes, paced 1). More held than the pace drains in this long (a
/// source over the pace) still goes after `PACE_MAX_WAIT`. Under the NACK hold
/// (`retransmit::WINDOW`, 250 ms), or held video expires instead of draining.
const PACE_MAX_WAIT_HELD: Duration = Duration::from_millis(200);

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
    /// The frame last pushed (by its queue time) and whether it arrived to an empty queue:
    /// that frame's tail is not held by the room.
    frame: Option<(Instant, bool)>,
    /// Video and resent datagrams over the path's datagram size, skipped.
    pub(crate) too_large: u64,
    pub(crate) resends_too_large: u64,
    /// The keyframe (its `queued_at`) whose first datagram was handed within the hold: the
    /// rest of it may still go however old (`Self::feed`).
    keyframe_started: Option<Instant>,
    /// Held datagrams dropped as older than the hold (`Self::feed`).
    pub(crate) stale: u64,
    /// A link that drops what overflows it holds the sender ([`Estimator::drop_held`]): video
    /// the pace drains within `PACE_MAX_WAIT_HELD` waits that long for it.
    pub(crate) on_hold: bool,
    /// Video was dropped as stale with no keyframe held: units are skipped to the next one
    /// ([`Self::orphaned`]).
    orphaned: bool,
}

pub(crate) struct Held {
    pub(crate) datagram: bytes::Bytes,
    /// Goes in the send log, stamped `queued_at`: time held here is delay the estimate
    /// must see, as it saw time in QUIC's queue.
    pub(crate) logged: bool,
    pub(crate) resend: bool,
    pub(crate) queued_at: Instant,
    /// A datagram of a unit that decodes alone (or its repairs).
    pub(crate) keyframe: bool,
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
        // What the pace drains in `PACE_MAX_WAIT_HELD` waits that long; more cannot drain in time.
        let drains = kbps.is_some_and(|k| {
            self.bytes as f64 <= k as f64 * PACE_GAIN / 8.0 * PACE_MAX_WAIT_HELD.as_millis() as f64
        });
        let wait = match self.on_hold && drains {
            true => PACE_MAX_WAIT_HELD,
            false => PACE_MAX_WAIT,
        };
        let stale = self
            .held
            .iter()
            .find(|h| !h.resend)
            .is_some_and(|h| now.saturating_duration_since(h.queued_at) >= wait);
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

    /// What `feed` may hand QUIC now: `(room, frame)`. `room` is the bytes of QUIC's queue
    /// still free to video (`queued` already there), under the pacer's burst; `frame` bounds
    /// a frame that arrived to an empty queue, which `room` gates only at its start: split
    /// across drain ticks, a clean 1080p60 stream presented unevenly (#1433, racer σ 0.90 →
    /// 5.8 ms). Never past half the buffer, so audio still fits beside it.
    pub(crate) fn budget(
        &mut self,
        (governed, paced): (Option<u64>, Option<u64>),
        queued: usize,
        buffer: usize,
        now: Instant,
    ) -> (usize, usize) {
        let pace = self.pace(paced, now);
        let room = self.room(governed, buffer).saturating_sub(queued).min(pace);
        (room, pace.min((buffer / 2).saturating_sub(queued)))
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

    /// Whether `held` is past the hold: older than `hold`, and not the rest of the keyframe
    /// whose first datagram was handed within it.
    fn expired(&self, held: &Held, now: Instant, hold: Duration) -> bool {
        now.saturating_duration_since(held.queued_at) > hold
            && !(held.keyframe && self.keyframe_started == Some(held.queued_at))
    }

    /// Drops the expired head: a resend alone; video with everything held that depends on it,
    /// up to the next keyframe, or all of it, which leaves units to skip to a keyframe.
    fn expire_head(&mut self) {
        let Some(head) = self.held.pop_front() else {
            return;
        };
        self.bytes -= head.datagram.len();
        self.stale += 1;
        if head.resend {
            return;
        }
        let key = self
            .held
            .iter()
            .find(|h| !h.resend && h.keyframe && h.queued_at > head.queued_at)
            .map(|h| h.queued_at);
        let (bytes, stale) = (&mut self.bytes, &mut self.stale);
        self.held.retain(|h| {
            let keep = h.resend || key.is_some_and(|k| h.queued_at >= k);
            if !keep {
                *bytes -= h.datagram.len();
                *stale += 1;
            }
            keep
        });
        self.frame = None;
        self.orphaned |= key.is_none();
    }

    /// Whether video went stale with no keyframe held since the last call: skip units until a
    /// keyframe comes ([`Backlog::skip_to_keyframe`]).
    pub(crate) fn orphaned(&mut self) -> bool {
        std::mem::take(&mut self.orphaned)
    }

    /// Drops everything held: datagrams of a configuration the client has left.
    pub(crate) fn clear(&mut self) {
        self.held.clear();
        self.bytes = 0;
        self.pace = None;
        self.counted = None;
        self.frame = None;
        self.keyframe_started = None;
        self.orphaned = false;
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
        if !held.resend && self.frame.is_none_or(|(at, _)| at != held.queued_at) {
            self.frame = Some((held.queued_at, self.held.is_empty()));
        }
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
    /// wire `pkt_seq`, and logs each sent one at the time it was queued; the log's `Instant` is
    /// `now`, when QUIC gets them. A frame that arrived to an empty queue goes whole, up to
    /// `frame` bytes, once `room` lets it start. False when the connection is gone.
    ///
    /// The one place a datagram reaches QUIC, so the one place the hold is enforced: nothing
    /// queued longer than `hold` (the client's NACK hold) is ever handed over, whatever the
    /// stall guard, the ACKs or the path are doing, except the rest of a keyframe whose first
    /// datagram went within it (`Self::expire_head`).
    pub(crate) fn feed(
        &mut self,
        mut room: usize,
        mut frame: usize,
        (now, hold): (Instant, Duration),
        mut log: Option<&mut SendLog>,
        mut send: impl FnMut(Held) -> Handed,
    ) -> bool {
        let open = room > 0;
        while let Some(next) = self.held.front() {
            if self.expired(next, now, hold) {
                self.expire_head();
                continue;
            }
            let len = next.datagram.len();
            let whole = open && !next.resend && self.frame == Some((next.queued_at, true));
            if len > if whole { room.max(frame) } else { room } {
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
            let started = (held.keyframe && !resend).then_some(held.queued_at);
            let key = started.is_some();
            let logged = log.as_mut().filter(|_| held.logged);
            let record = logged.map(|log| (log, held.datagram.clone()));
            match send(held) {
                Handed::Sent => {
                    if let Some((log, d)) = record {
                        log.record_handed(&d, queued_at, now, resend, key);
                    }
                    if started.is_some() {
                        self.keyframe_started = started;
                    }
                    room = room.saturating_sub(len);
                    frame = frame.saturating_sub(len);
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
