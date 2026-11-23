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
    /// The chance a datagram sent at this time is dropped on the way, seeded (`rng`).
    loss_at: Box<dyn Fn(u64) -> f64>,
    /// The path carries and acknowledges nothing (queued mode): an outage QUIC sees.
    blackhole: bool,
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
    /// As production's `Control`: feedback governs at `governed_kbps` (the ABR's setpoint and FEC
    /// share when the estimate is under them, never under the ABR floor); silence releases it.
    governor: Governor,
    floor_kbps: f64,
    /// (seconds, µs the oldest held video had waited), each queued-mode tick.
    held_waits: Vec<(f64, u64)>,
    /// The session's ABR, as production feeds it (`Control::feed`): the encoder follows its
    /// setpoint instead of the estimate.
    abr: Option<crate::session::abr::Governor>,
    /// Production's stall guard on each frame (`admit`), skipping to a keyframe after a stale
    /// drop, with the encoder answering a keyframe ask at its next frame.
    guard: Option<Backlog>,
    key_asked: bool,
    /// (seconds, a keyframe was handed to QUIC), each queued-mode tick that handed one.
    keys_handed: Vec<f64>,
    /// Datagrams handed to QUIC older than `HOLD`, but for the rest of a keyframe whose first
    /// datagram went within it; and when each keyframe's first datagram went.
    past_hold: usize,
    key_first: std::collections::HashMap<Instant, Duration>,
    /// (seconds, the ABR's setpoint, the ABR's rate) after each message.
    setpoints: Vec<(f64, u32)>,
    rates: Vec<(f64, f64)>,
    /// Seeded one-way jitter, ±µs per datagram, first in first out (xorshift, `rng`).
    jitter_us: u64,
    rng: u64,
    /// A policer in front of the path: a token bucket of this many bytes refilled at
    /// `cap_kbps`, dropping what finds it empty, with no queue behind it (`tc police`).
    bucket_bytes: Option<f64>,
    tokens: f64,
    tokens_at: u64,
    /// Repair datagrams sent with each frame's video, as a share of it (§FEC), charged to the
    /// ABR as `Control::feed` does.
    fec_share: f64,
    fec_carry: f64,
    /// (agent µs, the path dropped it), every datagram put on the path.
    tx: Vec<(u64, bool)>,
    /// Seconds a frame was the first the stall guard skipped in a row.
    stalls: Vec<f64>,
    stalled: bool,
    /// (seconds, the ABR's reading, the window's loss) after each message.
    readings: Vec<(f64, Option<f64>)>,
    losses: Vec<(f64, Option<f64>)>,
    /// (seconds, the estimate's loss bound) after each message.
    loss_bounds: Vec<(f64, f64)>,
    /// (seconds, the hold's line) after each message.
    lines: Vec<(f64, Option<f64>)>,
    /// Lab facts (#1433, R3/R4 diagnosis), all off by default. The production resolution ladder
    /// on the ABR's rate, as `abr_glue::step_rung` drives it: on a step the floor follows the
    /// rung, the setpoint is cut to the rate and the encoder restarts with a keyframe.
    ladder: Option<(
        crate::session::ladder::LadderPlanner,
        crate::session::ladder::FloorFollow,
    )>,
    /// (seconds, rung) at launch and at each step.
    rungs: Vec<(f64, usize)>,
    /// A keyframe is this many average frames at the encoder's rate.
    key_ratio: f64,
    /// The lab encoder's keyframes (`Sim::lab_hw`): one `HW_GOP` frames after the last keyframe,
    /// sized by the rung (`hw_key_ratio`), with P-frames smaller so a GOP averages the rate.
    hw_keys: bool,
    since_key: u32,
    /// Production's FEC policy (`Control::feed`): the share follows the loss, and the ABR is charged
    /// at most what the estimate leaves above its floor. `None`: `fec_share` fixed.
    fec_policy: Option<super::super::fec::Policy>,
    hold_fec: f64,
    /// The share of the encoder's rate a P-frame takes at a send time (content; CBR averages 1).
    content_gain: Box<dyn Fn(u64) -> f64>,
    /// The browser client: FEC, one NACK resend per lost datagram, keyframe requests, freezes.
    client: Option<Client>,
    /// 128 kbit/s of audio, 320 bytes every 20 ms, onto the path ahead of video (not reported).
    audio: bool,
    next_audio: u64,
    /// `tbf burst`: a token bucket of this many bytes at `cap_kbps` in front of the `queue_bytes`
    /// queue (the lab's `tbf rate 5mbit burst 15kb latency 10ms`).
    tbf_burst: Option<f64>,
    /// The capacity probe's repair datagrams (`Estimator::probe_kbps`): the client drops them (a
    /// frame already whole) and never asks for one again (§FEC: repairs are never retransmitted).
    probe_seqs: std::collections::HashSet<u32>,
    /// (seconds, the probe's total) after each message.
    probes: Vec<(f64, Option<f64>)>,
    /// Production's `probe_frame`: the last frame's video datagrams, and the pair repaired next.
    probe_frame: Option<(Vec<bytes::Bytes>, usize)>,
    /// The `pkt_seq`s whose NACK reached the agent: answered through the agent's queue, as
    /// production answers a `nack`.
    nacked: Vec<u32>,
}

/// The browser's reassembly (`web/src/webtransport/reassembly.ts`) and decoder, simulated: a
/// frame is whole once every video datagram arrives, or its strided XOR repair rebuilds the one
/// missing from its group, or its one NACK resend arrives through the same path. Frames are shown
/// in order; one that cannot be made whole, or is not whole `PENDING_US` after it was sent, is
/// lost: nothing more is shown until a keyframe is, and one is asked for at once
/// (`KEYFRAME_SPACING_MS` apart) and again every `KEY_REASK_MS` while waiting.
#[derive(Default)]
struct Client {
    /// (queued µs, keyframe, video datagrams, repairs), per frame in send order.
    frames: Vec<(u64, bool, u32, u32)>,
    /// queued µs → the wire `pkt_seq`s handed to QUIC, in order.
    handed: std::collections::HashMap<u64, Vec<u32>>,
    /// Arrival µs of each first send and of its resend (`None`: dropped).
    arrived: std::collections::HashMap<u32, Option<u64>>,
    resent: std::collections::HashMap<u32, Option<u64>>,
    /// The wire `pkt_seq` of each resend handed to QUIC → the `pkt_seq` it resends.
    resend_of: std::collections::HashMap<u32, u32>,
    /// The `pkt_seq`s NACKed a second time.
    renacked: std::collections::HashSet<u32>,
    /// (due µs, `pkt_seq`): resends the agent sends then; due µs: keyframe requests reaching it.
    nacks: VecDeque<(u64, u32)>,
    asks: VecDeque<u64>,
    /// The decoder: the next frame to show, when the last was shown, waiting for a keyframe since
    /// (and last asked), and the times a frame was shown over `FREEZE_US` after the one before.
    next: usize,
    shown: Option<u64>,
    waiting: Option<u64>,
    asked: Option<u64>,
    freezes: Vec<f64>,
    /// The longest time between two frames shown, µs, and when it ended.
    longest: (u64, u64),
}

/// A keyframe's size in average frames at the encoder's rate; the lab rows also run at 10 × and
/// print 15 ×. The agent logs the real ratio per keyframe (`webtransport-keyframe-sent`).
const LAB_KEY_RATIO: f64 = 5.0;

/// The lab encoder's GOP (`Sim::hw_keys`): a keyframe a second at 60 fps, and after any forced one.
const HW_GOP: u32 = 60;

/// Keyframe bytes over an average frame at the setpoint, p50 per height, measured on the NVIDIA lab
/// host (`webtransport-keyframe-sent`, 2026-11-22 capacity campaign, all rows).
fn hw_key_ratio(height: i32) -> f64 {
    match height {
        h if h >= 1080 => 5.3,
        h if h >= 900 => 7.3,
        h if h >= 720 => 7.6,
        h if h >= 540 => 8.3,
        _ => 10.7,
    }
}

/// Steam UI content under CBR: 3.7 s of light frames, then 300 ms of busy ones (mean 1.0).
fn lab_content(t: u64) -> f64 {
    match t % 4_000_000 < 3_700_000 {
        true => 0.975,
        false => 1.3,
    }
}

/// A frame shown more than this after the one before is a freeze (`isFreeze` in
/// `web/src/webtransport/telemetry.ts`: over max(3 × typical, typical + 150 ms) at 60 fps).
const FREEZE_US: u64 = 166_667;
/// A lost datagram's resend leaves the agent this long after it was sent (the gap seen at the
/// client, one way back); a keyframe request takes as long to reach it.
const NACK_US: u64 = 25_000;
/// `MAX_PENDING_AGE_MS`, `KEYFRAME_SPACING_MS` and `KEY_REASK_MS` of the web client.
const PENDING_US: u64 = 500_000;
const KEY_SPACING_US: u64 = 150_000;
const KEY_REASK_US: u64 = 1_000_000;

impl Client {
    /// Datagram `seq` by `now`: `Some(Some(at))` arrived, `Some(None)` lost for good, `None` not
    /// known yet.
    fn datagram(&self, seq: u32, now: u64) -> Option<Option<u64>> {
        let by = |a: Option<u64>| a.filter(|&t| t <= now);
        match (self.arrived.get(&seq), self.resent.get(&seq)) {
            (Some(&Some(t)), _) | (_, Some(&Some(t))) if t <= now => Some(Some(t)),
            (Some(&None), Some(&None)) => Some(None),
            (Some(a), r) => by(*a).or(r.and_then(|r| by(*r))).map(Some),
            (None, _) => None,
        }
    }

    /// Frame `q` by `now`: `Some(Some(at))` whole at `at`, `Some(None)` lost, `None` pending.
    fn whole(&self, q: u64, video: u32, repairs: u32, now: u64) -> Option<Option<u64>> {
        let late = now >= q + PENDING_US;
        let Some(seqs) = self.handed.get(&q).filter(|s| s.len() >= video as usize) else {
            return late.then_some(None);
        };
        let (mut done, mut pending) = (0, false);
        for g in 0..repairs.max(1) {
            let members: Vec<Option<Option<u64>>> = (g..video)
                .step_by(repairs.max(1) as usize)
                .chain((repairs > 0).then_some(video + g))
                .filter_map(|i| seqs.get(i as usize))
                .map(|&s| self.datagram(s, now))
                .collect();
            let lost = members.iter().filter(|d| matches!(d, Some(None))).count();
            let open = members.iter().filter(|d| d.is_none()).count();
            if lost > usize::from(repairs > 0) {
                return Some(None);
            }
            pending |= lost + open > usize::from(repairs > 0);
            done = members
                .iter()
                .flatten()
                .flatten()
                .fold(done, |a, &t| a.max(t));
        }
        match (pending, late) {
            (false, _) => Some(Some(done)),
            (true, true) => Some(None),
            (true, false) => None,
        }
    }

    /// Show what can be shown by `now`; `true` when a keyframe is to be asked for now.
    fn decode(&mut self, now: u64) -> bool {
        while let Some(&(q, key, video, repairs)) = self.frames.get(self.next) {
            if self.waiting.is_some() && !key {
                self.next += 1;
                continue;
            }
            let Some(fate) = self.whole(q, video, repairs, now) else {
                break;
            };
            self.next += 1;
            let Some(at) = fate else {
                self.waiting.get_or_insert(now);
                continue;
            };
            self.waiting = None;
            if self.shown.is_some_and(|l| at.max(l) - l > FREEZE_US) {
                self.freezes.push(at as f64 / 1e6);
            }
            if let Some(l) = self.shown.filter(|&l| at.max(l) - l > self.longest.0) {
                self.longest = (at.max(l) - l, at);
            }
            self.shown = Some(self.shown.map_or(at, |l| at.max(l)));
        }
        let due = match (self.waiting, self.asked) {
            (None, _) => false,
            (Some(_), None) => true,
            (Some(since), Some(a)) => {
                (a < since && now >= a + KEY_SPACING_US) || now >= a + KEY_REASK_US
            }
        };
        if due {
            self.asked = Some(now);
        }
        due
    }

    /// The times (seconds) a frame was shown over `FREEZE_US` after the last one shown.
    fn freezes(&self) -> Vec<f64> {
        self.freezes.clone()
    }
}

/// The agent's [`VideoQueue`] and QUIC behind it: a datagram leaves QUIC's queue once the
/// governed window (`Window`: 2 × estimate × max(the 10 s RTT floor, `WINDOW_RTT_MIN`)) has room
/// for it.
#[derive(Default)]
struct Quic {
    video: VideoQueue,
    queue: VecDeque<bytes::Bytes>,
    queued: usize,
    /// (sent µs, acknowledged or declared lost µs, bytes, arrived).
    flight: VecDeque<(u64, u64, usize, bool)>,
    in_flight: usize,
    floor: RttFloor,
    min_rtt: Duration,
    /// Datagrams acknowledged: the guard's ACK count.
    acks: u64,
}

/// web-transport-quinn's session-id header, 1 byte for the first session: counted in front of
/// every queued datagram and once more in `Session::datagram_send_buffer_space`.
const SESSION_HEADER: usize = 1;

impl Quic {
    /// QUIC's datagram queue as production reads it (`DATAGRAM_BUFFER - datagram_send_buffer_space`):
    /// never 0, even idle.
    fn read(&self) -> usize {
        self.queued + (self.queue.len() + 1) * SESSION_HEADER
    }
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
            loss_at: Box::new(|_| 0.0),
            blackhole: false,
            queue_drops: 0,
            pending: VecDeque::new(),
            trace: Vec::new(),
            quic: None,
            held_max: (0, 0.0),
            keyframe_every: 0,
            frames: 0,
            waits: Vec::new(),
            governor: Governor::default(),
            floor_kbps: 0.0,
            held_waits: Vec::new(),
            abr: None,
            guard: None,
            key_asked: false,
            keys_handed: Vec::new(),
            past_hold: 0,
            key_first: std::collections::HashMap::new(),
            setpoints: Vec::new(),
            rates: Vec::new(),
            jitter_us: 0,
            rng: 0x9E37_79B9_7F4A_7C15,
            bucket_bytes: None,
            tokens: f64::MAX,
            tokens_at: 0,
            fec_share: 0.0,
            fec_carry: 0.0,
            tx: Vec::new(),
            stalls: Vec::new(),
            stalled: false,
            readings: Vec::new(),
            losses: Vec::new(),
            loss_bounds: Vec::new(),
            lines: Vec::new(),
            ladder: None,
            rungs: Vec::new(),
            key_ratio: 5.0,
            hw_keys: false,
            since_key: 0,
            fec_policy: None,
            hold_fec: 0.0,
            content_gain: Box::new(|_| 1.0),
            client: None,
            audio: false,
            next_audio: 0,
            tbf_burst: None,
            probe_seqs: std::collections::HashSet::new(),
            probes: Vec::new(),
            probe_frame: None,
            nacked: Vec::new(),
        }
    }

    /// With production's stall guard and stale drop around the agent's queue.
    fn with_guard(mut self) -> Self {
        self.guard = Some(Backlog::default());
        self
    }

    /// The lab's 1080p60 WebTransport session at 8000 kbit/s (#1433, R3/R4 diagnosis): the ABR
    /// with production's launch floor (`abr_floor_ratio` 0.3) and per-report loop, the resolution
    /// ladder on its rate with the floor following the rung, the stall guard, FEC at the share the
    /// lab measured (20-25 %), audio, and the client. Keyframes only on request and at rung steps,
    /// `LAB_KEY_RATIO` average frames; P-frames follow the content (`lab_content`). On a 50 Mbit/s
    /// link until the test shapes it. `legacy`: `QUASAR_WT_CAPACITY=legacy` (0.6.3, lab side A).
    fn lab(legacy: bool) -> Self {
        use crate::session::ladder::{FloorFollow, LadderConfig, LadderPlanner};
        const FLOOR: u32 = 2400;
        let mut s = Self::new(50_000.0).with_guard();
        s.est.legacy = legacy;
        let cfg = crate::session::abr::AbrConfig {
            per_report: Some(Duration::ZERO),
            legacy_readings: legacy,
            ..crate::session::abr::AbrConfig::new_smooth(FLOOR, CEILING as u32, 60)
        };
        s.abr = Some(crate::session::abr::Governor::new(cfg, s.start));
        let rungs = crate::session::rungs::available_rungs(1920, 1080);
        let mut planner =
            LadderPlanner::new(&LadderConfig::new(), rungs, 60, CEILING as u32, s.start);
        planner.set_per_report();
        let floors = FloorFollow {
            enabled: true,
            launch_floor_kbps: FLOOR,
            operator_floor_kbps: None,
            ceiling_kbps: CEILING as u32,
            launch_px: 1920 * 1080,
            exponent: crate::session::ladder::ResolutionPolicy::DEFAULT_EXPONENT,
        };
        s.ladder = Some((planner, floors));
        s.rungs.push((0.0, 0));
        s.floor_kbps = f64::from(FLOOR);
        s.quic = Some(Quic::default());
        s.client = Some(Client::default());
        s.audio = true;
        s.fec_share = 0.24;
        s.est.probes = !legacy;
        s.key_ratio = LAB_KEY_RATIO;
        s.content_gain = Box::new(lab_content);
        s
    }

    /// `Sim::lab` with the lab encoder's keyframes (`hw_keys`).
    fn lab_hw(legacy: bool) -> Self {
        let mut s = Self::lab(legacy);
        s.hw_keys = true;
        s.fec_policy = Some(Default::default());
        (s.fec_share, s.key_ratio) = (0.0, 0.0);
        s
    }

    /// Ends a lab row's shaping: the clean 50 Mbit/s link again.
    fn clear(&mut self) {
        self.cap_kbps = 50_000.0;
        self.queue_bytes = 256.0 * 1024.0;
        (self.tbf_burst, self.bucket_bytes) = (None, None);
    }

    /// The rung at `secs`.
    fn rung_at(&self, secs: f64) -> usize {
        self.rungs
            .iter()
            .rev()
            .find(|r| r.0 <= secs)
            .map_or(0, |r| r.1)
    }

    /// With the WebTransport ABR (per report, `Smooth`) between the estimate and the encoder.
    fn with_abr(mut self) -> Self {
        let cfg = crate::session::abr::AbrConfig {
            per_report: Some(Duration::ZERO),
            ..crate::session::abr::AbrConfig::new_smooth(500, CEILING as u32, 60)
        };
        self.abr = Some(crate::session::abr::Governor::new(cfg, self.start));
        self
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
            if self.est.on_tick(at, &self.log, RTT).is_some() {
                self.governor.release();
            }
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
        let encoder = match &self.abr {
            Some(abr) => f64::from(abr.setpoint_kbps()),
            None => self.est.estimate_kbps(),
        };
        let kbps = self
            .send_kbps
            .unwrap_or(encoder.max(self.floor_kbps))
            .min((self.content_kbps)(self.t));
        let key = self.keyframe_every > 0 && self.frames.is_multiple_of(self.keyframe_every)
            || std::mem::take(&mut self.key_asked)
            || self.hw_keys && self.since_key + 1 >= HW_GOP;
        self.frames += 1;
        if let (Some(guard), Some(quic)) = (self.guard.as_mut(), self.quic.as_ref()) {
            let at = self.start + Duration::from_micros(self.t);
            guard.acknowledge(quic.acks);
            let admit = guard.admit(quic.read(), key, at, HOLD);
            if admit != Admit::Send && !std::mem::replace(&mut self.stalled, true) {
                self.stalls.push(self.t as f64 / 1e6);
            }
            match admit {
                Admit::Send => self.stalled = false,
                Admit::SkipForKeyframe => {
                    self.key_asked = true;
                    return;
                }
                Admit::Skip => return,
                Admit::GiveUp => panic!("the guard gave up at {:.2} s", self.t as f64 / 1e6),
            }
        }
        let ratio = match (self.hw_keys, self.ladder.as_ref()) {
            (true, Some((planner, _))) => hw_key_ratio(planner.res_size().1),
            (true, None) => hw_key_ratio(1080),
            (false, _) => self.key_ratio,
        };
        self.since_key = if key { 0 } else { self.since_key + 1 };
        let gain = match (key, self.hw_keys) {
            (true, _) => ratio,
            (false, true) => {
                (self.content_gain)(self.t) * (f64::from(HW_GOP) - ratio) / f64::from(HW_GOP - 1)
            }
            (false, false) => (self.content_gain)(self.t),
        };
        let bytes = kbps * 1000.0 / 8.0 / 60.0 * gain;
        let video = (bytes / SIZE as f64).ceil().max(1.0) as u32;
        // The lab sim sends exact bytes, as production does: the last datagram is partial (whole
        // datagrams put a frame at 1813 kbit/s 1.23 × over its rate, over the pacer's 1.25 × at
        // some rates once FEC is on).
        let exact = self.client.is_some();
        let last = match exact {
            true => (bytes as usize - (video as usize - 1) * SIZE).clamp(64, SIZE),
            false => SIZE,
        };
        // Repairs spend the share in bytes and carry the rest to the next frame (`fec::layout`).
        let spent = match exact {
            true => bytes / SIZE as f64,
            false => f64::from(video),
        };
        let repairs = spent * self.fec_share + self.fec_carry;
        self.fec_carry = repairs.fract();
        let n = video + repairs as u32;
        if let Some(client) = self.client.as_mut() {
            client.frames.push((self.t, key, video, n - video));
        }
        if let Some(quic) = self.quic.as_mut() {
            let queued_at = self.start + Duration::from_micros(self.t);
            let mut fragments = Vec::new();
            for i in 0..n {
                let mut datagram = video_datagram(quic.video.next_seq());
                if i + 1 == video {
                    datagram.truncate(last);
                }
                if i < video {
                    fragments.push(datagram.clone());
                }
                let held = Held {
                    datagram,
                    logged: true,
                    resend: false,
                    queued_at,
                    keyframe: key,
                };
                quic.video.push(held, BUFFER);
            }
            self.probe_frame = Some((fragments, 0));
            self.drain();
            return;
        }
        for _ in 0..n {
            let mut d = vec![0u8; SIZE];
            d[1..5].copy_from_slice(&self.seq.to_le_bytes());
            self.log
                .record(&d, self.start + Duration::from_micros(self.t), false);
            self.transmit(self.seq, SIZE);
            self.seq = self.seq.wrapping_add(1);
        }
    }

    /// One datagram onto the path now; its arrival on the agent's clock, or `None` dropped.
    fn transmit(&mut self, seq: u32, bytes: usize) -> Option<u64> {
        let mut dropped =
            self.blackhole || self.drop_every > 0 && seq.is_multiple_of(self.drop_every);
        let chance = (self.loss_at)(self.t);
        dropped |= chance > 0.0 && ((self.rand() % 1_000_000) as f64) < chance * 1e6;
        let arrival = self.path(bytes, dropped, seq);
        self.tx.push((self.t, arrival.is_none()));
        self.pending.push_back((seq, arrival));
        if let Some(client) = self.client.as_mut() {
            match client.resend_of.remove(&seq) {
                // The web client NACKs a fragment twice at most: a lost first resend is asked for
                // again once it is unanswered for a round trip.
                Some(of) if arrival.is_none() && client.renacked.insert(of) => {
                    client.nacks.push_back((self.t + 2 * NACK_US, of));
                }
                Some(of) => drop(client.resent.insert(of, arrival)),
                None => {
                    client.arrived.insert(seq, arrival);
                    if arrival.is_none() && !self.probe_seqs.contains(&seq) {
                        client.nacks.push_back((self.t + NACK_US, seq));
                    }
                }
            }
        }
        arrival
    }

    /// `bytes` onto the bottleneck now (unless `dropped` on the way); arrival on the agent's
    /// clock, or `None`.
    fn path(&mut self, bytes: usize, dropped: bool, seq: u32) -> Option<u64> {
        let service = (SIZE as f64 * 8.0 * 1000.0 / self.cap_kbps) as u64;
        let backlog = self.free_at.saturating_sub(self.t) as f64 / service as f64 * SIZE as f64;
        let service = (bytes as f64 * 8.0 * 1000.0 / self.cap_kbps) as u64;
        let refill = |tokens: f64, since: u64, bucket: f64, t: u64, cap: f64| {
            (tokens + (t - since) as f64 * cap / 8000.0).min(bucket)
        };
        // `tbf`: with its queue empty, what the bucket holds passes at once.
        let mut passes = false;
        if let (Some(burst), true) = (self.tbf_burst, self.free_at <= self.t) {
            self.tokens = refill(self.tokens, self.tokens_at, burst, self.t, self.cap_kbps);
            self.tokens_at = self.t;
            passes = self.tokens >= bytes as f64;
        }
        let full = match self.bucket_bytes {
            Some(bucket) => {
                self.tokens = refill(self.tokens, self.tokens_at, bucket, self.t, self.cap_kbps);
                self.tokens_at = self.t;
                let empty = self.tokens < bytes as f64;
                self.tokens -= if empty { 0.0 } else { bytes as f64 };
                empty
            }
            None => !passes && backlog + bytes as f64 > self.queue_bytes,
        };
        self.queue_drops += u32::from(full);
        let jitter = match self.jitter_us {
            0 => 0,
            j => (self.rand() % (2 * j + 1)) as i64 - j as i64,
        };
        (!dropped && !full).then(|| {
            // A policer has no queue: the path behind it is as fast as the sender.
            let queued = if self.bucket_bytes.is_some() || passes {
                self.tokens -= if passes { bytes as f64 } else { 0.0 };
                self.t
            } else {
                // `tbf` behind its bucket: what is left of it pays for part of this datagram.
                let paid = match self.tbf_burst {
                    Some(_) if self.free_at <= self.t => {
                        let part = (self.tokens / bytes as f64).clamp(0.0, 1.0);
                        (service as f64 * part) as u64
                    }
                    _ => 0,
                };
                self.free_at = self.free_at.max(self.t) + service - paid;
                if self.tbf_burst.is_some() {
                    (self.tokens, self.tokens_at) = (0.0, self.free_at);
                }
                self.free_at
            };
            // One path, first in first out: a delay spike holds back what follows it.
            let at = queued as i64 + 10_000 + (self.extra_us)(self.t) as i64 + jitter;
            self.last_arrival = self.last_arrival.max(at.max(0) as u64);
            self.last_arrival + (self.reorder_us)(seq)
        })
    }

    fn rand(&mut self) -> u64 {
        self.rng ^= self.rng << 13;
        self.rng ^= self.rng >> 7;
        self.rng ^= self.rng << 17;
        self.rng
    }

    /// The share of datagrams put on the path from `secs` on that it dropped.
    fn loss_after(&self, secs: f64) -> f64 {
        let from = (secs * 1e6) as u64;
        let sent = self.tx.iter().filter(|&&(t, _)| t >= from);
        sent.clone().filter(|&&(_, d)| d).count() as f64 / sent.count().max(1) as f64
    }

    /// QUIC sends what its window allows, the queue hands it what fits in the room the
    /// production `feed` gives it, and QUIC sends again.
    fn drain(&mut self) {
        self.client_due();
        let mut quic = self.quic.take().expect("queued mode");
        let est = if self.governor.governs() {
            self.governor.kbps() as f64
        } else {
            self.est.estimate_kbps()
        };
        self.send_window(&mut quic, est);
        // The production `feed`'s rates.
        let rates = video_rates(&self.governor, &self.est, false);
        if let Some(guard) = self.guard.as_mut() {
            guard.paced = rates.0.is_some();
        }
        quic.video.on_hold = self.est.drop_held();
        let at = self.start + Duration::from_micros(self.t);
        // A `nack` answer as production's: admitted by the stall guard, then ahead of held video.
        for of in std::mem::take(&mut self.nacked) {
            if let Some(guard) = self.guard.as_mut() {
                guard.acknowledge(quic.acks);
                match guard.admit(quic.read(), false, at, HOLD) {
                    Admit::Send => {}
                    Admit::SkipForKeyframe => {
                        self.key_asked = true;
                        continue;
                    }
                    Admit::Skip => continue,
                    Admit::GiveUp => panic!("the guard gave up at {:.2} s", self.t as f64 / 1e6),
                }
            }
            let mut datagram = video_datagram(0).to_vec();
            datagram[5..9].copy_from_slice(&of.to_le_bytes());
            let held = Held {
                datagram: datagram.into(),
                logged: true,
                resend: true,
                queued_at: at,
                keyframe: false,
            };
            quic.video.push(held, BUFFER);
        }
        let (room, frame) = quic.video.budget(rates, quic.read(), BUFFER, at);
        let (queue, queued) = (&mut quic.queue, &mut quic.queued);
        let mut key = false;
        let (past_hold, key_first) = (&mut self.past_hold, &mut self.key_first);
        let (client, start) = (&mut self.client, self.start);
        quic.video
            .feed(room, frame, (at, HOLD), Some(&mut self.log), |held| {
                if let Some(c) = client.as_mut() {
                    let q = held.queued_at.saturating_duration_since(start).as_micros() as u64;
                    let seq = wire_seq(&held.datagram);
                    match held.resend {
                        true => drop(c.resend_of.insert(seq, wire_seq(&held.datagram[4..]))),
                        false => c.handed.entry(q).or_default().push(seq),
                    }
                }
                let age = at.saturating_duration_since(held.queued_at);
                let judged = match held.keyframe && !held.resend {
                    true => *key_first.entry(held.queued_at).or_insert(age),
                    false => age,
                };
                *past_hold += usize::from(judged > HOLD);
                *queued += held.datagram.len();
                key |= held.keyframe && !held.resend;
                queue.push_back(held.datagram);
                Handed::Sent
            });
        if let (true, Some(guard)) = (quic.video.orphaned(), self.guard.as_mut()) {
            guard.skip_to_keyframe(quic.acks);
        }
        if key {
            self.keys_handed.push(self.t as f64 / 1e6);
        }
        // The production probe arm, on QUIC's queue as production reads it.
        let probe = self.est.probe_kbps();
        if probe.is_none() {
            self.log.probe_room(None, at);
        } else {
            let read = quic.read();
            let (queue, queued, seqs) = (&mut quic.queue, &mut quic.queued, &mut self.probe_seqs);
            let frame = &mut self.probe_frame;
            super::super::fill_probe(
                &mut self.log,
                probe,
                frame,
                &mut quic.video,
                read,
                at,
                |d| {
                    seqs.insert(wire_seq(&d));
                    *queued += d.len();
                    queue.push_back(d);
                    Handed::Sent
                },
            );
        }
        if quic.video.bytes() > self.held_max.0 {
            self.held_max = (quic.video.bytes(), self.t as f64 / 1e6);
        }
        self.send_window(&mut quic, est);
        self.waits
            .push((self.t as f64 / 1e6, self.free_at.saturating_sub(self.t)));
        let oldest = quic
            .video
            .oldest()
            .map_or(0, |q| at.saturating_duration_since(q).as_micros() as u64);
        self.held_waits.push((self.t as f64 / 1e6, oldest));
        self.quic = Some(quic);
    }

    /// Audio, the resends, the client's decoder and the keyframe requests due now.
    fn client_due(&mut self) {
        if self.audio && self.t >= self.next_audio {
            self.path(320, false, 0);
            self.next_audio = self.t + 20_000;
        }
        let Some(mut c) = self.client.take() else {
            return;
        };
        while c.nacks.front().is_some_and(|&(due, _)| due <= self.t) {
            let (_, seq) = c.nacks.pop_front().unwrap();
            self.nacked.push(seq);
        }
        if c.decode(self.t) {
            c.asks.push_back(self.t + NACK_US);
        }
        while c.asks.front().is_some_and(|&due| due <= self.t) {
            c.asks.pop_front();
            self.key_asked = true;
        }
        self.client = Some(c);
    }

    fn send_window(&mut self, quic: &mut Quic, est_kbps: f64) {
        let now = self.t;
        while let Some(&(sent, done, _, arrived)) = quic.flight.front() {
            if done > now {
                break;
            }
            if arrived {
                quic.acks += 1;
                let at = self.start + Duration::from_micros(done);
                // Idle: nothing waits behind the window.
                let idle = quic.queue.is_empty();
                quic.min_rtt = quic
                    .floor
                    .sample(at, Duration::from_micros(done - sent), idle);
            }
            quic.in_flight -= quic.flight.pop_front().unwrap().2;
        }
        let rtt = quic.min_rtt.max(WINDOW_RTT_MIN).as_secs_f64();
        // Never under Cubic's first window at a 1200-byte MTU, as `Window::window`.
        let window = ((2.0 * est_kbps * 1000.0 / 8.0 * rtt) as usize).max(12_000);
        while let Some(d) = quic.queue.front() {
            if quic.in_flight + d.len() > window {
                break;
            }
            let d = quic.queue.pop_front().unwrap();
            quic.queued -= d.len();
            quic.in_flight += d.len();
            let arrival = self.transmit(wire_seq(&d), d.len());
            // The ACK returns over the same 10 ms; a loss among arrivals is declared at quinn's
            // 9/8 RTT, 25 ms on the 20 ms path. Kept in completion order: arrivals are first in
            // first out already.
            let last = quic.flight.back().map_or(0, |f| f.1);
            let done = match self.blackhole {
                true => u64::MAX,
                false => arrival.map_or(now + 25_000, |a| a + 10_000).max(last),
            };
            quic.flight
                .push_back((now, done, d.len(), arrival.is_some()));
        }
    }

    /// The path is back: what went into the blackhole is declared lost a PTO on.
    fn unblock(&mut self) {
        self.blackhole = false;
        let lost = self.t + 200_000;
        for f in self.quic.as_mut().expect("queued mode").flight.iter_mut() {
            f.1 = f.1.min(lost);
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
        // As `Control::feedback`: what the encoder is asked to send, setpoint plus FEC.
        self.est.encoder_kbps = self
            .abr
            .as_ref()
            .map(|a| f64::from(a.setpoint_kbps()) * (1.0 + self.fec_share));
        if let Some(kbps) = self.est.on_feedback(&fb, &mut self.log, at, NO_QUIC) {
            // The hold acts through the ABR; with none the sender follows the estimate itself.
            let kbps = match self.abr {
                Some(_) => self.est.hold(kbps),
                None => kbps,
            };
            if let Some(policy) = self.fec_policy.as_mut() {
                if let Some(loss) = self.est.loss() {
                    policy.observe(loss, at);
                }
                let room = (kbps / self.floor_kbps - 1.0).max(0.0);
                self.fec_share = policy.overhead().min(room);
            }
            let legacy = self.est.legacy;
            if let Some(abr) = self.abr.as_mut() {
                let share = 1.0 + self.fec_share;
                // `Control::feed`: the line under the FEC its loss called for, then
                // `abr_glue::hold` and `abr_glue::report`.
                if let Some(policy) = self.fec_policy.as_ref() {
                    let fec = policy.overhead();
                    self.hold_fec = self.est.held_kbps().map_or(0.0, |_| self.hold_fec.max(fec));
                }
                let line = self
                    .est
                    .held_kbps()
                    .map(|l| l / share.max(1.0 + self.hold_fec));
                match line {
                    Some(line) => drop(abr.hold_to(line, at)),
                    None => abr.hold_off(at),
                }
                if let Some((planner, _)) = self.ladder.as_mut() {
                    planner.set_held(line.is_some(), at);
                }
                // No reading counts while the rung's step settles.
                let settling = self.ladder.as_ref().is_some_and(|l| l.0.settling(at));
                let reading = self.est.measured().filter(|_| !settling);
                abr.observe_report(reading.map(|r| r / share), at);
            }
            self.step_rung(at);
            if let Some(abr) = self.abr.as_mut() {
                abr.observe(abr_estimate(kbps, self.fec_share), at);
            }
            let setpoint = self
                .abr
                .as_ref()
                .map_or(0.0, |a| f64::from(a.setpoint_kbps()));
            self.governor.govern(match legacy {
                true => kbps.max(self.floor_kbps),
                false => governed_kbps(kbps, setpoint, self.fec_share, self.floor_kbps),
            });
        }
        self.trace
            .push((now as f64 / 1e6, self.est.estimate_kbps()));
        self.readings.push((now as f64 / 1e6, reading(&self.est)));
        self.losses.push((now as f64 / 1e6, window_loss(&self.est)));
        self.loss_bounds
            .push((now as f64 / 1e6, self.est.loss_kbps));
        self.lines.push((now as f64 / 1e6, self.est.held_kbps()));
        self.probes.push((now as f64 / 1e6, self.est.probe_kbps()));
        if let Some(abr) = self.abr.as_ref() {
            self.setpoints.push((now as f64 / 1e6, abr.setpoint_kbps()));
            self.rates.push((now as f64 / 1e6, abr.rate_kbps()));
        }
    }

    /// `abr_glue::step_rung`: the rung from the ABR's rate; a step moves the floor with the rung,
    /// cuts the setpoint to the rate and restarts the encoder (a keyframe at the new size).
    fn step_rung(&mut self, at: Instant) {
        let (Some((planner, floors)), Some(abr)) = (self.ladder.as_mut(), self.abr.as_mut()) else {
            return;
        };
        let rate = abr.rate_kbps();
        let Some(crate::session::ladder::PlannedStep::Resolution(d)) =
            planner.observe_rate(rate, at)
        else {
            return;
        };
        let (w, h) = planner.res_size();
        let floor = floors.floor_for(i64::from(w) * i64::from(h), false);
        abr.set_floor_kbps(floor);
        abr.cut_to(rate, at);
        planner.mark_resized(at);
        self.floor_kbps = f64::from(floor);
        self.key_asked = true;
        self.rungs.push((self.t as f64 / 1e6, d.to));
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
    let loss = s.est.loss();
    assert!(loss.is_some_and(|l| l > 0.15), "FEC reads it: {loss:?}");
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
    let loss = window_loss(&s.est);
    assert!(
        loss.is_none_or(|l| l < LOSS_LOW),
        "the outage's own losses measured {loss:?}"
    );
    assert!(
        s.est.recover_to.is_some(),
        "the outage's own losses ended the regrow"
    );
}

/// #1496 r3: video held in the agent reaches QUIC after the last report still stamped with
/// the time it was queued, before that report. Marked by time, the outage began before the
/// last datagrams the client got ahead of it, the first report after it ended the exemption
/// on one of those, and the outage's own losses cut the estimate to 2.9 Mbit/s (CI, 7bb8e1b1):
/// the recovery keyframe was then paced under the ABR floor.
#[test]
fn video_held_into_an_outage_is_the_outages_loss_not_congestion() {
    let start = Instant::now();
    let ms = |ms: u64| start + Duration::from_millis(ms);
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    let send = |log: &mut SendLog, seq: u32, at: Instant| {
        let mut d = vec![0u8; SIZE];
        d[1..5].copy_from_slice(&seq.to_le_bytes());
        log.record(&d, at, false);
    };
    let sent = |seq: u32| {
        if seq < 100 {
            u64::from(seq) * 10
        } else {
            2500 + u64::from(seq - 300) * 5
        }
    };
    let report = |first: u32, last: u32, got: &dyn Fn(u32) -> bool| Feedback {
        first_pkt_seq: first,
        ref_us: 0,
        arrivals: (first..=last)
            .map(|seq| match got(seq) {
                true => ((sent(seq) + 5) * 1000 / UNIT_US) as u16,
                false => NOT_RECEIVED,
            })
            .collect(),
    };
    for seq in 0..100 {
        send(&mut log, seq, ms(sent(seq)));
    }
    // The last report before the outage: seq 90..100 were still on their way.
    est.on_feedback(&report(0, 89, &|_| true), &mut log, ms(1000), NO_QUIC);
    // Queued at 990 ms, handed to QUIC after that report, lost with the path.
    for seq in 100..300 {
        send(&mut log, seq, ms(990));
    }
    for t in (1100..=2500).step_by(100) {
        est.on_tick(ms(t), &log, RTT);
    }
    assert!(
        est.recover_to.is_some(),
        "the silence was not read as an outage"
    );
    for seq in 300..340 {
        send(&mut log, seq, ms(sent(seq)));
    }
    let first_after = report(90, 339, &|seq| !(100..300).contains(&seq));
    est.on_feedback(&first_after, &mut log, ms(2800), NO_QUIC);
    let loss = window_loss(&est);
    assert!(
        loss.is_none_or(|l| l < LOSS_LOW),
        "the outage's own losses measured {loss:?}"
    );
    assert!(
        est.recover_to.is_some(),
        "the outage's own losses ended the regrow"
    );
}

/// The last window's loss fraction.
fn window_loss(est: &Estimator) -> Option<f64> {
    est.loss
}

/// What the ABR reads from the last message, kbit/s.
fn reading(est: &Estimator) -> Option<f64> {
    est.measured()
}

/// One datagram `i` after `base` in the send log, sent at `sent_ms`.
fn send_nth(log: &mut SendLog, start: Instant, base: u32, i: u32, sent_ms: u64) {
    let mut d = vec![0u8; SIZE];
    d[1..5].copy_from_slice(&base.wrapping_add(i).to_le_bytes());
    log.record(&d, start + Duration::from_millis(sent_ms), false);
}

/// A report of datagrams `first..=last` after `base`: each arrived 1 ms after `sent_ms`, or not
/// at all.
fn report_nth(
    base: u32,
    first: u32,
    last: u32,
    sent_ms: impl Fn(u32) -> u64,
    got: impl Fn(u32) -> bool,
) -> Feedback {
    Feedback {
        first_pkt_seq: base.wrapping_add(first),
        ref_us: 0,
        arrivals: (first..=last)
            .map(|i| match got(i) {
                true => ((sent_ms(i) + 1) * 1000 / UNIT_US) as u16,
                false => NOT_RECEIVED,
            })
            .collect(),
    }
}

/// #1496 lab round: the outage dropped only host → client UDP, so the client's report of what
/// arrived before the cut reached the agent after the cut, and what the agent sent in between
/// (19 and 48 ms of video on the NVIDIA lab host) sat below a mark taken at the send after
/// that report: the first report after the path returned read it as 31 % and 100 % loss
/// (`cut="silence+loss"`) and ended the regrow. Across the `pkt_seq` wrap too.
#[test]
fn video_sent_between_the_cut_and_the_last_report_is_the_outages_loss() {
    for base in [0, u32::MAX - 150] {
        let start = Instant::now();
        let ms = |ms: u64| start + Duration::from_millis(ms);
        let mut log = SendLog::new(start);
        let mut est = Estimator::new(CEILING, start);
        // One a millisecond; the path drops from 100 ms until 1000 ms, when 400.. go.
        let sent = |i: u32| {
            if i < 400 {
                u64::from(i)
            } else {
                u64::from(i) + 600
            }
        };
        let got = |i: u32| !(100..400).contains(&i);
        for i in 0..140 {
            send_nth(&mut log, start, base, i, sent(i));
        }
        // What arrived before the cut, reported after it.
        est.on_feedback(
            &report_nth(base, 0, 99, sent, got),
            &mut log,
            ms(140),
            NO_QUIC,
        );
        for i in 140..400 {
            send_nth(&mut log, start, base, i, sent(i));
        }
        for t in (600..=900).step_by(100) {
            est.on_tick(ms(t), &log, RTT);
        }
        assert!(
            est.recover_to.is_some(),
            "the silence was not read as an outage"
        );
        for i in 400..440 {
            send_nth(&mut log, start, base, i, sent(i));
        }
        est.on_feedback(
            &report_nth(base, 100, 439, sent, got),
            &mut log,
            ms(1100),
            NO_QUIC,
        );
        let loss = window_loss(&est);
        assert!(
            loss.is_none_or(|l| l < LOSS_LOW),
            "base {base}: the outage's own losses measured {loss:?}"
        );
        assert_ne!(
            est.bound,
            Bound::Loss,
            "base {base}: they cut the loss bound"
        );
        assert!(
            est.outage.is_none(),
            "base {base}: the link answered, the mark is spent"
        );
    }
}

/// With both directions down, the client's report of what arrived before the cut comes after
/// the path returns (QUIC resends it), ahead of the report of what came after: it must not
/// spend the mark, or the next report reads the outage's losses as congestion.
#[test]
fn a_late_report_of_what_arrived_before_the_cut_does_not_spend_the_outage_mark() {
    let start = Instant::now();
    let ms = |ms: u64| start + Duration::from_millis(ms);
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    let sent = |i: u32| {
        if i < 300 {
            u64::from(i)
        } else {
            u64::from(i) + 700
        }
    };
    let got = |i: u32| !(100..300).contains(&i);
    for i in 0..60 {
        send_nth(&mut log, start, 0, i, sent(i));
    }
    est.on_feedback(&report_nth(0, 0, 49, sent, got), &mut log, ms(60), NO_QUIC);
    for i in 60..300 {
        send_nth(&mut log, start, 0, i, sent(i));
    }
    for t in (600..=900).step_by(100) {
        est.on_tick(ms(t), &log, RTT);
    }
    for i in 300..340 {
        send_nth(&mut log, start, 0, i, sent(i));
    }
    est.on_feedback(
        &report_nth(0, 50, 99, sent, got),
        &mut log,
        ms(1100),
        NO_QUIC,
    );
    est.on_feedback(
        &report_nth(0, 100, 339, sent, got),
        &mut log,
        ms(1150),
        NO_QUIC,
    );
    let loss = window_loss(&est);
    assert!(
        loss.is_none_or(|l| l < LOSS_LOW),
        "the outage's own losses measured {loss:?}"
    );
    assert_ne!(est.bound, Bound::Loss, "they cut the loss bound");
}

/// #1496 review: a hole in the late report of what arrived before the cut (a datagram
/// reordered behind it, on a Wi-Fi peer) is no sign the link answered. Ending the mark at the
/// next datagram after it read the outage's own losses as congestion in the next report.
#[test]
fn a_reordered_hole_in_the_late_report_does_not_spend_the_outage_mark() {
    let start = Instant::now();
    let ms = |ms: u64| start + Duration::from_millis(ms);
    let mut log = SendLog::new(start);
    let mut est = Estimator::new(CEILING, start);
    let sent = |i: u32| {
        if i < 300 {
            u64::from(i)
        } else {
            u64::from(i) + 700
        }
    };
    let got = |i: u32| !(100..300).contains(&i) && i != 70;
    for i in 0..60 {
        send_nth(&mut log, start, 0, i, sent(i));
    }
    est.on_feedback(&report_nth(0, 0, 49, sent, got), &mut log, ms(60), NO_QUIC);
    for i in 60..300 {
        send_nth(&mut log, start, 0, i, sent(i));
    }
    for t in (600..=900).step_by(100) {
        est.on_tick(ms(t), &log, RTT);
    }
    for i in 300..340 {
        send_nth(&mut log, start, 0, i, sent(i));
    }
    est.on_feedback(
        &report_nth(0, 50, 99, sent, got),
        &mut log,
        ms(1100),
        NO_QUIC,
    );
    assert!(est.outage.is_some(), "a reordered hole spent the mark");
    est.on_feedback(
        &report_nth(0, 100, 339, sent, got),
        &mut log,
        ms(1150),
        NO_QUIC,
    );
    let loss = window_loss(&est);
    assert!(
        loss.is_none_or(|l| l < LOSS_LOW),
        "the outage's own losses measured {loss:?}"
    );
    assert_ne!(est.bound, Bound::Loss, "they cut the loss bound");
    assert!(est.outage.is_none(), "the link answered, the mark is spent");
}

/// #1557 review A2: the exemption ends at the first packet the report shows received at or
/// after the outage's `by` (sent after detection). A burst sent after the link came back is
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
    // own); seq 21..40 are received but sent before `by` (41), so none ends the exemption.
    let sent_us = |seq: u32| u64::from(seq) * 10_000;
    let arrived = |seq: u32| ((sent_us(seq) + 5_000) / UNIT_US) as u16;
    for seq in 0..41u32 {
        let mut d = vec![0u8; SIZE];
        d[1..5].copy_from_slice(&seq.to_le_bytes());
        let at = start + Duration::from_micros(sent_us(seq));
        log.record(&d, at, false);
    }
    est.outage = Some(Outage { from: 1, by: 41 });
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
        window_loss(&est),
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
        delivered_kbps: Some(20_000.0),
        gap_us: 0,
        sent_kbps: None,
        arrived_sent_kbps: None,
        probe_sent_kbps: 0.0,
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
        delivered_kbps: Some(300.0),
        gap_us: 0,
        sent_kbps: None,
        arrived_sent_kbps: None,
        probe_sent_kbps: 0.0,
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

/// #1446 (row G2, design §5): Chrome's reorder discards are loss to the agent, and v2 reads
/// them as 0.6.3 does: the estimate steps down.
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

/// Lab V3 (#1433): paced, QUIC's queue stands at the pacer's room on a path its window limits. The
/// guard reads QUIC's queue only, so a queue standing while ACKs arrive is that path, not a dead
/// one; with no ACK it is still a stall.
#[test]
fn paced_a_queue_standing_with_acks_is_no_stall_and_without_them_is_one() {
    let start = Instant::now();
    let standing = |acking: bool| {
        let mut b = Backlog {
            paced: true,
            ..Default::default()
        };
        (1..60u32).find(|&n| {
            b.acknowledge(if acking { u64::from(n) } else { 0 });
            b.admit(4 * SIZE, false, start + FRAME * n, HOLD) != Admit::Send
        })
    };
    assert_eq!(standing(true), None, "a path that acknowledges");
    assert!(standing(false).is_some(), "a silent path");
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
                delivered_kbps: None,
                gap_us: 0,
                sent_kbps: None,
                arrived_sent_kbps: None,
                probe_sent_kbps: 0.0,
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
            delivered_kbps: None,
            gap_us: 0,
            sent_kbps: None,
            arrived_sent_kbps: None,
            probe_sent_kbps: 0.0,
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

/// The verdict `secs` after a cap at `kbps` let go (#1430): the budget has climbed from it.
fn climbed(kbps: f64, secs: f64) -> PyroVerdict {
    PyroVerdict::Budget(budget_of(kbps * PYRO_CLIMB_PER_S.powf(secs)))
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

/// Feedback for 39 of the 40 datagrams from `first`, every other one lost, ending at an arrival as a
/// browser's report does (`feedback.ts` `take`).
fn half_lost(first: u32) -> Feedback {
    let arrivals = (0..39)
        .map(|i| if i % 2 == 0 { 0 } else { NOT_RECEIVED })
        .collect();
    Feedback {
        first_pkt_seq: first,
        ref_us: 0,
        arrivals,
    }
}

#[test]
fn pyrowave_budget_starts_at_the_target_and_follows_a_lower_estimate() {
    let on = Instant::now();
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
    let open = quic_at(8_000_000.0);
    assert_eq!(
        rate.on_tick(on, open),
        PyroVerdict::Budget(budget_of(f64::from(PYRO_TARGET))),
        "seeded at the target, less the datagram overhead"
    );
    let mut seq = 0;
    let first = pyro_send(&mut rate, &mut seq, on + Duration::from_millis(10));
    let at = on + Duration::from_millis(60);
    let verdict = rate.on_feedback(&half_lost(first), at, open);
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
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
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
        climbed(150_000.0, 0.1),
        "a wider window lets the cap go: the budget climbs from it, never past the target"
    );
    assert_eq!(
        {
            fill(&mut rate, 100);
            rate.on_tick(at + Duration::from_millis(200), quic_at(150_000.0))
        },
        climbed(150_000.0, 0.2),
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
        climbed(150_000.0, 0.1),
        "no new congestion and an empty queue: the cap lets go, and the budget climbs from it"
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
        climbed(150_000.0, 0.2),
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
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
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
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
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
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
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
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
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

/// #1430: the send log is sized for the largest PyroWave grant: the estimator reads the sent
/// rate over its 0.5 s window, and at the 1 Gbit/s a profile can allow (4K120's default is 800)
/// that is 62.5 MB, ~54 400 datagrams, more than a log sized for 400 Mbit/s held. A window the
/// log cannot cover reads as application-limited, so the log holds it all.
#[test]
fn pyrowave_send_log_spans_the_rate_window_at_a_1_gbit_grant() {
    let on = Instant::now();
    let mut log = SendLog::new(on);
    // 1 Gbit/s is 125 000 B/ms; a whole datagram is 1 148 B, so 54 439 of them in 0.5 s.
    let count = 500 * 125_000 / SIZE + 1;
    assert!(count > 54_000, "{count}");
    for seq in 0..count {
        let mut d = vec![0u8; SIZE];
        d[1..5].copy_from_slice(&(seq as u32).to_le_bytes());
        log.record(&d, on + Duration::from_micros(seq as u64 * 9), false);
    }
    assert!(
        LOG >= count,
        "the {count} datagrams of 0.5 s at 1 Gbit/s outgrow the log"
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
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
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
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
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
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
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

/// #1430: on a link between the floor and the target the cap held the budget at the link, the
/// queue drained, and the release handed back the whole target: the budget swung between the two
/// every ~400 ms, 200 ms of each a third over the link. It climbs back instead, so it stays
/// within +13 % of the rate it was capped at (the sim shows +12.9 %).
#[test]
fn pyrowave_on_a_link_between_the_floor_and_the_target_the_budget_stays_near_the_link() {
    for link in [150_000.0, 120_000.0] {
        let on = Instant::now();
        let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
        let verdicts = QueuedPath::new(link).run(&mut rate, on, 12, |_| f64::MAX, None, false);
        assert_eq!(steps_down(&verdicts), None, "link {link}: {verdicts:?}");
        let budgets: Vec<u32> = verdicts
            .iter()
            .filter_map(|(_, v)| match v {
                PyroVerdict::Budget(b) => Some(*b),
                _ => None,
            })
            .collect();
        let capped = budgets.iter().copied().skip(1).min().expect("a cap");
        let after_cap = &budgets[1..];
        let peak = after_cap.iter().copied().max().unwrap();
        assert!(
            f64::from(peak) <= f64::from(capped) * 1.13,
            "link {link}: cap {capped}, peak {peak}: {verdicts:?}"
        );
        assert!(
            f64::from(peak) < f64::from(pyro_budget(f64::from(PYRO_TARGET))) * 0.9,
            "link {link}: the budget went back to the target: {verdicts:?}"
        );
    }
}

/// #1430: once the link has lifted the budget still reaches the target, in a few seconds.
#[test]
fn pyrowave_a_released_cap_climbs_back_to_the_target_not_in_one_step() {
    let on = Instant::now();
    let at = |ms: u64| on + Duration::from_millis(ms);
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
    let target = budget_of(f64::from(PYRO_TARGET));
    let capped = budget_of(150_000.0);
    // Two judgements 100 ms apart with more than a window waiting: the cap, at 150 Mbit/s.
    rate.on_tick(at(1_000), backed_up(150_000.0));
    assert_eq!(
        rate.on_tick(at(1_100), backed_up(150_000.0)),
        PyroVerdict::Budget(capped)
    );
    // The queue drained, the window is wide open: the budget rises from the cap.
    let mut last = capped;
    let mut ms = 1_100;
    while last < target {
        ms += 100;
        fill(&mut rate, 100);
        match rate.on_tick(at(ms), quic_at(8_000_000.0)) {
            PyroVerdict::Budget(b) => {
                assert!(
                    b > last && f64::from(b) <= f64::from(last) * 1.1,
                    "{last} -> {b}"
                );
                last = b;
            }
            v => panic!("{v:?} at {ms}"),
        }
        assert!(
            ms < 1_100 + 3_000,
            "not back at the target after 3 s: {last}"
        );
    }
    assert_eq!(last, target);
}

/// #1430: a still picture after a cap is no proof the link lifted: the budget stays at the cap
/// while the encoder gives less than it, and climbs once the picture fills it again.
#[test]
fn pyrowave_a_still_picture_after_a_cap_keeps_the_budget_at_the_cap() {
    let on = Instant::now();
    let at = |ms: u64| on + Duration::from_millis(ms);
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
    let capped = budget_of(150_000.0);
    rate.on_tick(at(1_000), backed_up(150_000.0));
    assert_eq!(
        rate.on_tick(at(1_100), backed_up(150_000.0)),
        PyroVerdict::Budget(capped)
    );
    // A second of nothing offered, the window wide open.
    for ms in (1_200..=2_100).step_by(100) {
        assert_eq!(
            rate.on_tick(at(ms), quic_at(8_000_000.0)),
            PyroVerdict::Hold,
            "{ms} ms"
        );
    }
    assert_eq!(rate.budget, capped);
    fill(&mut rate, 100);
    assert!(matches!(
        rate.on_tick(at(2_200), quic_at(8_000_000.0)),
        // One climb step from the cap, not the target in one jump (a frozen climb keeping its
        // old start would hand back the whole target here).
        PyroVerdict::Budget(b) if b > capped && b <= capped * 11 / 10
    ));
}

/// #1633: video the agent holds for room in QUIC's queue waits on QUIC too.
#[test]
fn pyrowave_video_held_for_quic_counts_as_a_queue_waiting_on_the_window() {
    let on = Instant::now();
    let at = |ms: u64| on + Duration::from_millis(ms);
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
    let target = budget_of(f64::from(PYRO_TARGET));
    let window_full = quic_at(150_000.0);
    assert!(window_full.cwnd_bytes < 64 * 1024);
    // Nothing in quinn's queue, 64 KiB held here, over two judgements: the cap.
    rate.on_tick(at(1_000), window_full.with_held(64 * 1024));
    assert_ne!(
        rate.on_tick(at(1_100), window_full.with_held(64 * 1024)),
        PyroVerdict::Hold
    );
    assert!(rate.budget < target);
    // The same path with nothing held is not capped.
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
    rate.on_tick(at(1_000), window_full);
    rate.on_tick(at(1_100), window_full);
    assert_eq!(rate.budget, target);
}

#[test]
fn pyrowave_through_a_responsive_path_a_dark_screen_and_a_single_loss_never_step_down() {
    for loss_ms in [None, Some(3000)] {
        let on = Instant::now();
        let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
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
    let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
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
        let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, on, false);
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
        keyframe: false,
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
    assert!(
        video.feed(room, 0, (Instant::now(), Duration::MAX), None, |h| quic(
            &mut wire, h
        ))
    );
    // QUIC drains nothing while three audio datagrams arrive.
    let mut audio = Vec::new();
    for _ in 0..3 {
        audio.push(video.take_seq());
        assert!(
            video.feed(0, 0, (Instant::now(), Duration::MAX), None, |h| quic(
                &mut wire, h
            ))
        );
    }
    assert_eq!(
        wire,
        [0, 1, 2, 3],
        "only the budget's worth of video is with QUIC"
    );
    assert_eq!(audio, [4, 5, 6], "audio goes next, ahead of the 36 held");
    assert_eq!(video.bytes(), 36 * SIZE);

    // The path drains: the rest follows, numbered after the audio, with no gap.
    assert!(
        video.feed(usize::MAX, 0, (Instant::now(), Duration::MAX), None, |h| {
            quic(&mut wire, h)
        })
    );
    assert_eq!(wire[4..], (7..43).collect::<Vec<_>>());
    assert!(video.is_empty());
}

/// The clean-link racer regression (#1433): a frame over the room went out across 1 ms drain
/// ticks. A frame that finds the agent's queue empty goes whole; the room gates its start.
#[test]
fn a_frame_arriving_to_an_empty_queue_goes_to_quic_whole() {
    let mut video = VideoQueue::default();
    let now = Instant::now();
    let first = video.next_seq();
    for i in 0..40 {
        video.push(
            Held {
                queued_at: now,
                ..held(first + i)
            },
            BUFFER,
        );
    }
    // 1 Mbit/s, unpaced: the room is four datagrams, the frame forty.
    let (room, frame) = video.budget((Some(1000), None), 0, BUFFER, now);
    assert_eq!(room, 5000);
    let mut wire = Vec::new();
    assert!(video.feed(
        room,
        frame,
        (Instant::now(), Duration::MAX),
        None,
        |h| quic(&mut wire, h)
    ));
    assert_eq!(wire.len(), 40, "the frame was split by the room");
    assert!(video.is_empty());
}

#[test]
fn a_frame_behind_held_video_is_still_split_by_the_room() {
    let mut video = VideoQueue::default();
    let t0 = Instant::now();
    let t1 = t0 + Duration::from_millis(16);
    let first = video.next_seq();
    for i in 0..10 {
        video.push(
            Held {
                queued_at: t0,
                ..held(first + i)
            },
            BUFFER,
        );
    }
    // QUIC's queue is full: nothing goes, and a second frame arrives behind the first (a stall).
    let mut wire = Vec::new();
    let (room, frame) = video.budget((Some(1000), None), 5000, BUFFER, t0);
    assert!(video.feed(
        room,
        frame,
        (Instant::now(), Duration::MAX),
        None,
        |h| quic(&mut wire, h)
    ));
    assert!(wire.is_empty());
    for i in 0..40 {
        video.push(
            Held {
                queued_at: t1,
                ..held(first + 10 + i)
            },
            BUFFER,
        );
    }
    let (room, frame) = video.budget((Some(1000), None), 0, BUFFER, t1);
    assert!(video.feed(
        room,
        frame,
        (Instant::now(), Duration::MAX),
        None,
        |h| quic(&mut wire, h)
    ));
    assert_eq!(wire.len(), 4, "video already held goes by the room");
    assert_eq!(video.bytes(), 46 * SIZE);
}

/// #1591 point 2: unpaced held video is stamped with its queue time, but the silence clock
/// runs from the hand-off to QUIC: a frame held 600 ms under backpressure is not 600 ms
/// unanswered the moment it leaves.
#[test]
fn the_silence_clock_starts_when_held_unpaced_video_is_handed_to_quic() {
    let start = Instant::now();
    let ms = |n: u64| start + Duration::from_millis(n);
    let mut log = SendLog::new(start);
    let mut video = VideoQueue::default();
    video.push(
        Held {
            queued_at: ms(400),
            ..held(0)
        },
        BUFFER,
    );
    let mut wire = Vec::new();
    // Handed to QUIC at 1000 ms, after a report that cleared the clock.
    assert!(video.feed(
        usize::MAX,
        0,
        (ms(1000), Duration::MAX),
        Some(&mut log),
        |h| quic(&mut wire, h)
    ));
    assert_eq!(wire.len(), 1);
    assert_eq!(log.unanswered_since, Some(ms(1000)));
}

#[test]
fn held_video_over_the_cap_drops_oldest_first_and_leaves_no_gap() {
    let mut video = VideoQueue::default();
    for i in 0..5 {
        video.push(held(i), 3 * SIZE);
    }
    assert_eq!((video.bytes(), video.dropped), (3 * SIZE, 2));
    let mut wire = Vec::new();
    assert!(
        video.feed(usize::MAX, 0, (Instant::now(), Duration::MAX), None, |h| {
            quic(&mut wire, h)
        })
    );
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
    assert!(
        video.feed(usize::MAX, 0, (Instant::now(), Duration::MAX), None, |h| {
            quic(&mut wire, h)
        })
    );
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
    assert!(
        video.feed(SIZE, 0, (Instant::now(), Duration::MAX), None, |h| quic(
            &mut wire, h
        ))
    );
    video.clear();
    assert!(video.is_empty() && video.bytes() == 0);
    assert_eq!(video.room(None, BUFFER), BUFFER);
    video.push(held(9), BUFFER);
    assert!(
        video.feed(usize::MAX, 0, (Instant::now(), Duration::MAX), None, |h| {
            quic(&mut wire, h)
        })
    );
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
    assert!(video.feed(
        usize::MAX,
        0,
        (Instant::now(), Duration::MAX),
        None,
        |h| match verdicts.next().unwrap() {
            Handed::Sent => quic(&mut wire, h),
            other => other,
        }
    ));
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
/// then; QUIC unpaced put each keyframe there at once (110-190 ms late on the lab host). The
/// encoder follows the ABR: the start-up overrun (the 8 Mbit/s ceiling into 5) waits in the
/// agent's queue behind the window on the RTT floor and drains at 15 % of the link once it is
/// a reading, gone by about 17 s. Production's stall guard (`Backlog::admit`) skips it, which
/// this sim does not model.
#[test]
fn a_keyframe_waits_in_the_agent_not_in_the_links_queue() {
    let mut s = Sim::new(5000.0).with_abr().with_guard();
    s.quic = Some(Quic::default());
    s.keyframe_every = 60;
    s.run(40.0);
    assert!(s.est.found_link(), "the estimate found the 5 Mbit/s link");
    let mut waits: Vec<u64> = s
        .waits
        .iter()
        .filter(|&&(t, _)| t >= 20.0)
        .map(|&(_, w)| w)
        .collect();
    waits.sort_unstable();
    let p95 = waits[waits.len() * 95 / 100] / 1000;
    let max = waits[waits.len() - 1] / 1000;
    let estimate = s.mean_after(20.0);
    eprintln!("link queue after 20 s: p95 {p95} ms, max {max} ms; estimate {estimate:.0} kbps");
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
        assert!(
            video.feed(room, 0, (Instant::now(), Duration::MAX), None, |h| quic(
                &mut wire, h
            ))
        );
        40 - video.bytes() / SIZE
    };
    // 40 datagrams queued at `t0` exactly.
    let keyframe = || {
        let mut video = VideoQueue::default();
        let first = video.next_seq();
        for i in 0..40 {
            video.push(
                Held {
                    queued_at: t0,
                    ..held(first + i)
                },
                BUFFER,
            );
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
        send(&mut video, Some(8000), ms(99)),
        13,
        "idle time saves one burst"
    );
    assert_eq!(
        send(&mut video, None, ms(99)),
        40,
        "not governed: not paced"
    );

    let mut video = keyframe();
    assert_eq!(send(&mut video, Some(8000), ms(0)), 5);
    assert_eq!(
        send(&mut video, Some(8000), ms(99)),
        10,
        "waited 99 ms: still paced"
    );
    assert_eq!(
        send(&mut video, Some(8000), ms(100)),
        40,
        "waited 100 ms: goes at once"
    );
}

/// #1496 review A M1: the rates `feed` uses. The pacer holds video to the governed rate, never
/// under the ABR floor the encoder is held to: an outage that decayed the estimate to 500 under
/// a 2500 floor still leaves the encoder at 2500, and pacing that at 1.25 × 500 held it until
/// it was stale.
#[test]
fn video_is_paced_at_the_governed_rate_not_at_an_estimate_under_the_floor() {
    let mut est = Estimator::new(CEILING, Instant::now());
    let governor = Governor::default();
    assert_eq!(
        video_rates(&governor, &est, false),
        (None, None),
        "not governed"
    );
    governor.govern(CEILING);
    assert_eq!(
        video_rates(&governor, &est, false),
        (Some(8000), None),
        "at the ceiling: not paced"
    );
    est.resume_at(500.0);
    governor.govern(2500.0);
    assert_eq!(video_rates(&governor, &est, false), (Some(500), Some(2500)));
    assert_eq!(video_rates(&governor, &est, true), (None, None), "PyroWave");
}

/// The same in the sim: an outage decays the estimate under the ABR floor and the encoder keeps
/// sending the floor. Paced at the governed rate, held video never waits long.
#[test]
fn after_an_outage_under_the_floor_held_video_does_not_go_stale() {
    let mut s = Sim::new(6000.0);
    s.quic = Some(Quic::default());
    s.floor_kbps = 4000.0;
    s.run(10.0);
    s.outage(5.0);
    let resumed = s.t as f64 / 1e6;
    s.run(5.0);
    let worst = s
        .held_waits
        .iter()
        .filter(|&&(t, _)| t >= resumed)
        .map(|&(_, w)| w)
        .max()
        .unwrap()
        / 1000;
    eprintln!("after the outage: oldest held video waited at most {worst} ms");
    assert!(worst < 50, "held video waited {worst} ms");
}

/// #1496 review B M1: a cap that lifts. The pacer's wait is not path delay; logged as it, each
/// paced keyframe read as overuse on the faster link and cut the estimate, which took about
/// 45 s and 32 delay cuts to get back to the ceiling instead of about 10 s.
#[test]
fn after_a_cap_lifts_the_estimate_climbs_back_to_the_ceiling_promptly() {
    let mut s = Sim::new(5000.0);
    s.quic = Some(Quic::default());
    s.keyframe_every = 60;
    s.run(30.0);
    assert!(s.est.found_link(), "the 5 Mbit/s cap was found");
    s.cap_kbps = 50_000.0;
    let lifted = s.t as f64 / 1e6;
    let lines = logged(|| s.run(30.0));
    let back = s
        .trace
        .iter()
        .find(|&&(t, e)| t >= lifted && e >= CEILING)
        .map(|&(t, _)| t - lifted);
    let delay_cuts = cuts(&lines)
        .iter()
        .filter(|c| c.0.contains("delay"))
        .count();
    eprintln!("cap lifted: back at the ceiling after {back:?} s, {delay_cuts} delay cuts");
    assert!(
        back.is_some_and(|b| b <= 15.0),
        "{back:?} s, {delay_cuts} delay cuts"
    );
}

/// #1417 × #1433: a silence decay leaves the estimate under the ceiling, but the link has not
/// said it carries less, so the pacer stays off while the outage regrow runs: a recovery
/// keyframe paced at the decayed rate came late (and, logged at its queue time, read as a
/// delay cut that ended the regrow).
#[test]
fn the_pacer_stays_off_while_the_outage_regrow_runs() {
    let mut s = Sim::new(50_000.0);
    s.quic = Some(Quic::default());
    s.keyframe_every = 60;
    s.run(10.0);
    assert_eq!(s.est.estimate_kbps(), CEILING);
    s.outage(10.0);
    assert!(s.est.estimate_kbps() < CEILING && s.est.recover_to.is_some());
    let resumed = s.t as f64 / 1e6;
    let lines = logged(|| s.run(0.5));
    assert!(s.governor.governs(), "feedback governs again");
    assert_eq!(
        video_rates(&s.governor, &s.est, false).1,
        None,
        "paced during the regrow"
    );
    let lines = [lines, logged(|| s.run(14.5))].concat();
    let back = s
        .trace
        .iter()
        .find(|&&(t, e)| t >= resumed && e >= CEILING)
        .map(|&(t, _)| t - resumed);
    let ended: Vec<&String> = lines
        .iter()
        .filter(|l| l.contains("regrow") && l.contains("ended"))
        .collect();
    eprintln!("after the outage: back at the ceiling after {back:?} s; {ended:?}");
    assert!(ended.iter().all(|l| l.contains("reached")), "{ended:?}");
    assert!(back.is_some_and(|b| b <= 10.0), "{back:?} s");
}

/// #1496 r2 M1: a second step down while the pacer is on (5 Mbit/s with a keyframe each
/// second, then 2 Mbit/s). Paced datagrams are logged at their release and unpaced ones at
/// their queue time; the send log keeps every stamp at or after the one before it in
/// `pkt_seq` order, or the delay detector reads a send that went back in time.
#[test]
fn send_stamps_never_go_back_through_a_second_step_down_while_paced() {
    let mut s = Sim::new(5000.0);
    s.quic = Some(Quic::default());
    s.keyframe_every = 60;
    s.run(30.0);
    assert!(s.est.found_link(), "paced before the second step");
    s.cap_kbps = 2000.0;
    s.run(30.0);
    let mut slots: Vec<&Slot> = s.log.slots.iter().filter(|s| s.bytes > 0).collect();
    slots.sort_by_key(|s| s.seq);
    let back = slots
        .windows(2)
        .filter(|w| w[1].sent_us < w[0].sent_us)
        .count();
    assert_eq!(back, 0, "{back} stamps went back in pkt_seq order");
    assert!(
        s.est.estimate_kbps() < 2000.0,
        "{:.0}",
        s.est.estimate_kbps()
    );
}

/// A group sent no later than the one before it gives no delay sample, and no panic.
#[test]
fn a_send_group_that_went_back_in_time_is_no_delay_sample() {
    // A group whose last send is before the previous group's.
    let mut ia = InterArrival::default();
    ia.push(100_000, 110_000);
    ia.push(200_000, 210_000);
    ia.current.as_mut().unwrap().last_send = 50_000;
    assert_eq!(ia.push(300_000, 310_000), None);
}

/// #1496 r2 S2: a `nack` answer goes ahead of held video, after earlier answers, and keeps
/// the wire `pkt_seq` +1 per datagram.
#[test]
fn a_resend_goes_ahead_of_held_video() {
    let mut video = VideoQueue::default();
    let first = video.next_seq();
    for i in 0..40 {
        video.push(held(first + i), BUFFER);
    }
    let resend = |seq| Held {
        resend: true,
        ..held(seq)
    };
    video.push(resend(100), BUFFER);
    video.push(resend(101), BUFFER);
    let mut wire = Vec::new();
    let mut order = Vec::new();
    assert!(
        video.feed(3 * SIZE, 0, (Instant::now(), Duration::MAX), None, |h| {
            order.push(h.resend);
            quic(&mut wire, h)
        })
    );
    assert_eq!(
        order,
        [true, true, false],
        "the two answers first, then video"
    );
    assert_eq!(wire, [0, 1, 2]);
}

fn resend(pkt_seq: u32) -> Held {
    Held {
        resend: true,
        ..held(pkt_seq)
    }
}

/// #1496 r3 S1: a `nack` answer goes ahead of held video, so the pacer's 100 ms bound reads
/// the oldest video, not the answer in front of it.
#[test]
fn the_pacer_bound_reads_the_oldest_video_not_a_resend_ahead_of_it() {
    let t0 = Instant::now();
    let at = |ms| t0 + Duration::from_millis(ms);
    let mut video = VideoQueue::default();
    video.push(
        Held {
            queued_at: at(0),
            ..held(0)
        },
        BUFFER,
    );
    video.push(
        Held {
            queued_at: at(150),
            ..resend(1)
        },
        BUFFER,
    );
    assert_eq!(
        video.pace(Some(8000), at(150)),
        usize::MAX,
        "video waited 150 ms"
    );

    let mut video = VideoQueue::default();
    video.push(
        Held {
            queued_at: at(0),
            ..resend(0)
        },
        BUFFER,
    );
    video.push(
        Held {
            queued_at: at(150),
            ..held(1)
        },
        BUFFER,
    );
    assert!(
        video.pace(Some(8000), at(150)) < usize::MAX,
        "fresh video is paced"
    );
}

/// #1496 r3 S2: past the cap the oldest video goes, not the `nack` answers ahead of it.
#[test]
fn held_video_over_the_cap_is_dropped_before_a_resend() {
    let mut video = VideoQueue::default();
    video.push(resend(9), 4 * SIZE);
    for i in 0..4 {
        video.push(held(i), 4 * SIZE);
    }
    assert_eq!(video.dropped, 1);
    let held: Vec<_> = video
        .held
        .iter()
        .map(|h| (h.resend, wire_seq(&h.datagram)))
        .collect();
    assert_eq!(held, [(true, 9), (false, 1), (false, 2), (false, 3)]);
}

// The capacity detector (#1433 design §2): what moves the ABR down, and what does not.

/// R8: a lasting one-way delay step (20 → 80 ms) is no reading, and the path floor takes the new
/// delay within `FLOOR_SPAN_US`, so a blackout after it is still one.
#[test]
fn a_delay_step_is_no_reading_and_ages_into_the_floor() {
    let mut s = Sim::new(50_000.0);
    s.send_kbps = Some(6000.0);
    s.extra_us = Box::new(|t| if t >= 5_000_000 { 60_000 } else { 0 });
    for _ in 0..600 {
        s.run(0.05);
        assert_eq!(s.est.measured(), None, "at {:.2} s", s.t as f64 / 1e6);
    }
    assert!(s.est.evidence.at_floor, "the floor did not take the step");
}

/// One datagram a millisecond from `base`: `count` of them, the run `lost` reported lost and the
/// rest received `delay_ms(i)` after they were sent, in one report.
fn blackout_report(
    est: &mut Estimator,
    log: &mut SendLog,
    start: Instant,
    lost: std::ops::Range<u32>,
    delay_ms: impl Fn(u32) -> u64,
) -> Measured {
    let count = 1000;
    for i in 0..count {
        send_nth(log, start, 0, i, u64::from(i));
    }
    let fb = Feedback {
        first_pkt_seq: 0,
        ref_us: 0,
        arrivals: (0..count)
            .map(|i| match lost.contains(&i) {
                true => NOT_RECEIVED,
                false => ((u64::from(i) + delay_ms(i)) * 1000 / UNIT_US) as u16,
            })
            .collect(),
    };
    est.measure(&fb, log, start + Duration::from_millis(1100))
}

/// R5–R7, review r5 B: a 60, 150 or 300 ms total drop with no queue either side is a blackout.
/// Its losses are out of loss, delivered and sent alike: no loss cut, no reading, no FEC level.
/// A run as long behind a queue, or a short one, is loss. Old code: 15 % loss cut the estimate
/// to 0.93 × and the report was a reading.
#[test]
fn a_short_blackout_is_no_loss_no_cut_and_no_reading() {
    for ms in [60, 150, 300] {
        let start = Instant::now();
        let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
        // Inside the 500 ms rate window, with arrivals on both sides of it.
        let m = blackout_report(&mut est, &mut log, start, 600..600 + ms, |_| 1);
        assert_eq!(m.loss, Some(0.0), "{ms} ms");
        let (r, sent) = (m.rate_kbps.unwrap(), m.sent_kbps.unwrap());
        assert!(
            (r - sent).abs() < 0.02 * sent,
            "{ms} ms: delivered {r:.0}, sent {sent:.0}"
        );
        est.apply(m, start + Duration::from_millis(1100), NO_QUIC);
        assert_eq!(est.estimate_kbps(), CEILING, "{ms} ms");
        assert_eq!(est.measured(), None, "{ms} ms");
        assert_eq!(est.loss(), Some(0.0), "{ms} ms: FEC reads no loss");
    }
    // Behind a queue: the arrival before the run waited 50 ms.
    let start = Instant::now();
    let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
    let m = blackout_report(&mut est, &mut log, start, 400..550, |i| {
        if i < 400 {
            1 + u64::from(i) / 8
        } else {
            1
        }
    });
    assert!(m.loss.is_some_and(|l| l > 0.1), "{:?}", m.loss);
    // Under 30 ms of send time.
    let start = Instant::now();
    let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
    let m = blackout_report(&mut est, &mut log, start, 400..425, |_| 1);
    assert!(m.loss.is_some_and(|l| l > 0.02), "{:?}", m.loss);
}

/// Rule 2 (#1433): a report is a reading only when the link delivered under 0.95 × what was sent
/// by a sender that was not application-limited, and then it is `BETA` × what was delivered,
/// never the estimate (#1503: an estimate that floated over the link held the sender over it).
/// Old code (0.6.3): the higher of the estimate and `BETA` × the receive rate.
#[test]
fn a_report_is_a_reading_only_when_the_link_delivered_under_what_was_sent() {
    let start = Instant::now();
    let at = |ms| start + Duration::from_millis(ms);
    let report = |delivered_kbps, sent_kbps| Measured {
        usage: Usage::Normal,
        rate_kbps: delivered_kbps,
        delivered_kbps,
        gap_us: 0,
        sent_kbps,
        arrived_sent_kbps: sent_kbps,
        probe_sent_kbps: 0.0,
        fresh: true,
        loss: None,
    };
    let mut est = Estimator::new(CEILING, start);
    est.apply(report(Some(7900.0), Some(8000.0)), at(100), NO_QUIC);
    assert_eq!(est.measured(), None, "a clean link: arrived as sent");
    est.apply(report(Some(1000.0), Some(3000.0)), at(200), NO_QUIC);
    assert_eq!(est.measured(), None, "an idle sender (application-limited)");
    est.apply(report(None, Some(8000.0)), at(300), NO_QUIC);
    assert_eq!(est.measured(), None, "no delivered rate");
    est.apply(report(Some(6000.0), Some(8000.0)), at(400), NO_QUIC);
    assert_eq!(est.measured(), Some(BETA * 6000.0), "never the estimate");
}

/// A reading moves the ABR only: at the ceiling no frame is paced, whatever the readings say.
#[test]
fn capacity_evidence_paces_nothing_at_the_ceiling() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    let governor = Governor::default();
    governor.govern(CEILING);
    let m = Measured {
        usage: Usage::Normal,
        rate_kbps: Some(5000.0),
        delivered_kbps: Some(5000.0),
        gap_us: 0,
        sent_kbps: Some(8000.0),
        arrived_sent_kbps: Some(8000.0),
        probe_sent_kbps: 0.0,
        fresh: true,
        loss: None,
    };
    est.apply(m, start + Duration::from_millis(100), NO_QUIC);
    assert!(est.measured().is_some());
    assert_eq!(video_rates(&governor, &est, false).1, None);
}

/// Review r3 B, #1503 (the backstop): the governed window is sized on the path's RTT floor, not
/// on the smoothed RTT a queue at the link inflates. Old code: 2 × 8 Mbit/s × a 2 s smoothed
/// RTT, 4 MB.
#[test]
fn the_governed_window_is_sized_on_the_rtt_floor() {
    let start = Instant::now();
    let governor = Governor::default();
    let mut w = window(&governor, start);
    governor.govern(8000.0);
    acked(&mut w, Duration::from_secs(2), Duration::from_millis(150));
    assert_eq!(w.window(), 300_000, "2 × 8 Mbit/s × 150 ms");
}

/// #1503 guard (design test 3): a standing queue (srtt 1.2 s) over a 20 ms RTT floor does not
/// grow the window past 2 × the rate × `WINDOW_RTT_MIN`.
#[test]
fn the_window_does_not_grow_with_a_standing_queue() {
    let start = Instant::now();
    let governor = Governor::default();
    let mut w = window(&governor, start);
    governor.govern(8000.0);
    acked(
        &mut w,
        Duration::from_millis(1200),
        Duration::from_millis(20),
    );
    assert_eq!(w.window(), 200_000, "2 × 8 Mbit/s × 100 ms");
}

// Review rounds 2 and 3 of the capacity detector.

/// I1 (review M1): nothing held past the NACK hold reaches QUIC, except the rest of a keyframe
/// whose first datagram went within it, whatever the stall guard, the ACKs or the encoder are
/// doing, and the picture comes back at a keyframe. At start-up (the 8 Mbit/s ceiling into 5),
/// at a cap's onset, with the encoder held at 4.8 Mbit/s by its floor into 5 (the queue shrinks
/// between keyframes, which resets the guard's timer), and through a 2 s outage the guard skips
/// that ends with ACKs coming back. Old code: a stalled queue drained stale once the ACKs
/// resumed, and the floor case handed QUIC video seconds old.
#[test]
fn no_video_past_the_hold_reaches_quic_on_any_path() {
    for case in ["start-up", "onset", "floor", "stall"] {
        let mut s = Sim::new(match case {
            "start-up" | "floor" => 5000.0,
            _ => 50_000.0,
        })
        .with_abr()
        .with_guard();
        s.quic = Some(Quic::default());
        s.keyframe_every = 60;
        if case == "floor" {
            s.send_kbps = Some(4800.0);
        }
        if matches!(case, "onset" | "stall") {
            s.run(10.0);
        }
        match case {
            "onset" => s.cap_kbps = 5000.0,
            "stall" => {
                s.blackhole = true;
                s.run(2.0);
                assert!(
                    s.guard.as_ref().unwrap().skipped > 0,
                    "the guard never skipped"
                );
                s.unblock();
            }
            _ => {}
        }
        let from = s.t as f64 / 1e6;
        s.run(20.0);
        let keys = s
            .keys_handed
            .windows(2)
            .filter(|k| k[0] >= from)
            .map(|k| k[1] - k[0])
            .fold(0.0, f64::max);
        let stale = s.quic.as_ref().unwrap().video.stale;
        eprintln!(
            "{case}: {} handed past the hold; longest gap between keyframes {keys:.2} s; {stale} \
             datagrams stale",
            s.past_hold
        );
        assert_eq!(s.past_hold, 0, "{case}: handed past the hold");
        assert!(keys < 2.0, "{case}: no keyframe for {keys:.2} s");
    }
}

/// I1: the rule at the hand-off itself, from each kind of datagram: a resend, video with a
/// keyframe behind it, the rest of a keyframe that started within the hold, video with nothing to
/// resume on, and a keyframe whose first datagram is already past it.
#[test]
fn the_hold_is_enforced_where_datagrams_reach_quic() {
    let t0 = Instant::now();
    let ms = |n: u64| t0 + Duration::from_millis(n);
    let unit = |q: &mut VideoQueue, at: u64, key: bool, n: u32| {
        for _ in 0..n {
            let seq = q.next_seq();
            let held = Held {
                queued_at: ms(at),
                keyframe: key,
                ..held(seq)
            };
            q.push(held, BUFFER);
        }
    };
    // (ms queued, keyframe) of each datagram handed at `now` within `room`.
    let feed = |q: &mut VideoQueue, now: u64, room: usize| {
        let mut out = Vec::new();
        q.feed(room, 0, (ms(now), HOLD), None, |h| {
            out.push(((h.queued_at - t0).as_millis() as u64, h.keyframe, h.resend));
            Handed::Sent
        });
        out
    };
    let mut q = VideoQueue::default();
    q.push(
        Held {
            queued_at: ms(0),
            ..resend(900)
        },
        BUFFER,
    );
    unit(&mut q, 0, false, 3);
    unit(&mut q, 100, true, 4);
    unit(&mut q, 117, false, 2);
    let out = feed(&mut q, 251, usize::MAX);
    assert_eq!(out.len(), 6, "{out:?}");
    assert!(out.iter().all(|&(at, _, resend)| at >= 100 && !resend));
    assert_eq!(
        (q.stale, q.orphaned()),
        (4, false),
        "the resend and the video before the keyframe"
    );

    // A keyframe whose first datagram goes within the hold goes whole, however late the rest.
    unit(&mut q, 300, true, 4);
    assert_eq!(feed(&mut q, 500, SIZE).len(), 1);
    assert_eq!(
        feed(&mut q, 600, usize::MAX).len(),
        3,
        "the rest of the keyframe"
    );
    // Video past the hold with no keyframe held: dropped, and units skip to a keyframe.
    unit(&mut q, 610, false, 2);
    assert!(feed(&mut q, 861, usize::MAX).is_empty());
    assert!(q.orphaned());
    // A keyframe past the hold before any of it went: dropped whole, with what follows it.
    unit(&mut q, 1000, true, 3);
    unit(&mut q, 1017, false, 1);
    assert!(feed(&mut q, 1251, usize::MAX).is_empty());
    assert!(q.orphaned());
    assert_eq!(q.bytes(), 0);
}

/// M2: a browser report can end inside a run of loss (at `MAX_COUNT`, or before an open gap):
/// the run waits for the next report's arrival, so a 150 ms blackout split across two reports
/// is still a blackout. Old code: the first report counted its part as loss at once.
#[test]
fn a_blackout_split_across_two_reports_is_still_a_blackout() {
    let start = Instant::now();
    let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
    for i in 0..1000 {
        send_nth(&mut log, start, 0, i, u64::from(i));
    }
    let arrival = |i: u32| match (600..750).contains(&i) {
        true => NOT_RECEIVED,
        false => ((u64::from(i) + 1) * 1000 / UNIT_US) as u16,
    };
    let report = |from: u32, to: u32| Feedback {
        first_pkt_seq: from,
        ref_us: 0,
        arrivals: (from..to).map(arrival).collect(),
    };
    let at = start + Duration::from_millis(1100);
    est.measure(&report(0, 700), &mut log, at);
    let m = est.measure(&report(700, 1000), &mut log, at + Duration::from_millis(50));
    assert_eq!(m.loss, Some(0.0));
    assert!((600..750).all(|i| log.peek(i).unwrap().status == Status::Outage));
}

/// The 150 ms blackout of the test above (600..750 lost), its first report ending at 700, then
/// `reports` as `(from, to, ms after the first)`: the last report's loss, and how many of the
/// run's 150 datagrams ended `Lost` and `Outage`.
fn split_run(reports: &[(u32, u32, u64)]) -> (Option<f64>, usize, usize) {
    let arrival = |i: u32| match (600..750).contains(&i) {
        true => NOT_RECEIVED,
        false => ((u64::from(i) + 1) * 1000 / UNIT_US) as u16,
    };
    let report = |from: u32, to: u32| Feedback {
        first_pkt_seq: from,
        ref_us: 0,
        arrivals: (from..to).map(arrival).collect(),
    };
    let start = Instant::now();
    let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
    for i in 0..1000 {
        send_nth(&mut log, start, 0, i, u64::from(i));
    }
    let at = start + Duration::from_millis(1100);
    est.measure(&report(0, 700), &mut log, at);
    let mut loss = None;
    for &(from, to, after) in reports {
        loss = est
            .measure(
                &report(from, to),
                &mut log,
                at + Duration::from_millis(after),
            )
            .loss;
    }
    let count = |s| {
        (600..750)
            .filter(|&i| log.peek(i).unwrap().status == s)
            .count()
    };
    (loss, count(Status::Lost), count(Status::Outage))
}

/// I4 (review N1, M2, R3-1): a run of loss is decided once, at its closing report when that
/// comes within `RUN_MAX_WAIT`, otherwise as loss at expiry, and that decision covers the whole
/// contiguous run. Closed on time, closed late (and at exactly `RUN_MAX_WAIT`), expired by a
/// report that only extends it, then closed, and by a report that does not continue it. Old code: a late
/// closing report counted 600..700 as loss and then found 700..750 a blackout.
#[test]
fn a_loss_run_is_decided_once_on_time_or_as_loss() {
    let wait = RUN_MAX_WAIT.as_millis() as u64;
    assert_eq!(
        split_run(&[(700, 1000, wait - 100)]),
        (Some(0.0), 0, 150),
        "on time"
    );
    for (name, reports) in [
        ("late", &[(700, 1000, wait + 100)][..]),
        ("at the wait", &[(700, 1000, wait)]),
        ("expired", &[(700, 720, wait), (720, 1000, wait + 50)]),
    ] {
        let (loss, lost, outage) = split_run(reports);
        assert!(
            loss.is_some_and(|l| l > 0.1) && (lost, outage) == (150, 0),
            "{name}: {loss:?} lost {lost} outage {outage}"
        );
    }
    // 700 never reported: a discontinuity ends the run, and 701..750 is a run of its own.
    let (loss, lost, outage) = split_run(&[(701, 1000, 100)]);
    assert!(
        loss.is_some_and(|l| l > 0.1) && (lost, outage) == (100, 49),
        "not continued: {loss:?} lost {lost} outage {outage}"
    );
}

/// I4 (R3-1): a run decided as loss at expiry stays loss while later contiguous reports keep
/// extending it, however many, until an arrival ends it at the floor. Old code: the tail after
/// the expiry began a new run and was found a blackout.
#[test]
fn a_loss_run_growing_after_expiry_stays_loss() {
    let wait = RUN_MAX_WAIT.as_millis() as u64;
    let (loss, lost, outage) = split_run(&[
        (700, 710, wait),
        (710, 720, wait + 10),
        (720, 740, wait + 20),
        (740, 1000, wait + 40),
    ]);
    assert!(
        loss.is_some_and(|l| l > 0.0) && (lost, outage) == (150, 0),
        "{loss:?} lost {lost} outage {outage}"
    );
}

/// M2: audio has no send-log slot, but an audio arrival inside a run of video loss is the link
/// carrying traffic: the run is loss, not a blackout. Old code: the audio was skipped, the video
/// run read solid, and congestion loss was taken for a blackout.
#[test]
fn audio_arriving_through_a_video_loss_burst_makes_it_loss_not_a_blackout() {
    let start = Instant::now();
    let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
    // Every fifth `pkt_seq` is audio.
    let audio = |i: u32| i % 5 == 4;
    for i in (0..1000).filter(|&i| !audio(i)) {
        send_nth(&mut log, start, 0, i, u64::from(i));
    }
    let fb = Feedback {
        first_pkt_seq: 0,
        ref_us: 0,
        arrivals: (0..1000)
            .map(|i| match (600..750).contains(&i) && !audio(i) {
                true => NOT_RECEIVED,
                false => ((u64::from(i) + 1) * 1000 / UNIT_US) as u16,
            })
            .collect(),
    };
    let m = est.measure(&fb, &mut log, start + Duration::from_millis(1100));
    assert!(m.loss.is_some_and(|l| l > 0.1), "{:?}", m.loss);
}

/// M4: a report with no fresh video carries the last window's delivered rate: no reading, even
/// with the queue evidence still standing. Old code: a reading from the stale rate.
#[test]
fn a_report_with_no_fresh_video_is_no_reading() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    let m = Measured {
        usage: Usage::Normal,
        rate_kbps: Some(5000.0),
        delivered_kbps: Some(5000.0),
        gap_us: 0,
        sent_kbps: None,
        arrived_sent_kbps: None,
        probe_sent_kbps: 0.0,
        fresh: false,
        loss: None,
    };
    est.apply(m, start + Duration::from_millis(100), NO_QUIC);
    assert_eq!(est.measured(), None);
}

/// M5: the window's RTT floor is the least sample of the last 10 s, so a longer route on the same
/// path (150 → 400 ms) sizes the window on 400 ms once the old samples are 10 s old: at once when
/// the samples were taken with room in the window, within one more span when every sample was
/// taken with the window full (I3). Old code: quinn's lifetime minimum held the window at
/// 2 × 8 Mbit/s × 100 ms, a quarter of the rate at 400 ms.
#[test]
fn the_rtt_floor_follows_a_longer_route_within_ten_seconds() {
    for idle in [true, false] {
        let start = Instant::now();
        let governor = Governor::default();
        let mut w = window(&governor, start);
        governor.govern(8000.0);
        w.srtt = Duration::from_millis(150);
        for ms in (0..25_000u64).step_by(10) {
            let rtt = Duration::from_millis(if ms < 5_000 { 150 } else { 400 });
            w.rtt_sample(start + Duration::from_millis(ms), rtt, idle || ms < 5_000);
            if ms == 14_000 {
                assert_eq!(
                    w.window(),
                    2 * 8000 * 150 / 8,
                    "inside 10 s of a 150 ms sample"
                );
            }
        }
        assert_eq!(w.window(), 2 * 8000 * 400 / 8, "idle {idle}");
    }
}

/// I3 (review M5): a sender whose window is always full never gives the room-gated sample that
/// may raise the floor, so a whole span without one makes the span's own least sample the floor.
/// A +60 ms route step under a standing queue of 0–10 ms, never idle: the floor follows within
/// 20 s, and to the new path's least sample, not into the queue. Old code: 40 ms for good.
#[test]
fn the_rtt_floor_follows_a_route_step_with_the_sender_never_idle() {
    let start = Instant::now();
    let mut floor = RttFloor::default();
    let step = 5_000u64;
    let mut followed = None;
    for ms in (0..40_000u64).step_by(10) {
        let base = if ms < step { 40 } else { 100 };
        let rtt = Duration::from_millis(base + ms / 10 % 11);
        let now = floor.sample(start + Duration::from_millis(ms), rtt, false);
        if ms >= step && now == Duration::from_millis(100) {
            followed.get_or_insert(ms - step);
        }
        assert!(now <= Duration::from_millis(100), "{now:?} at {ms} ms");
    }
    let followed = followed.expect("the floor never followed the route");
    assert!(followed <= 20_000, "followed after {followed} ms");
    // Through the window: the same step, the window sized on the new floor.
    let governor = Governor::default();
    let mut w = window(&governor, start);
    governor.govern(8000.0);
    w.srtt = Duration::from_millis(40);
    for ms in (0..25_000u64).step_by(10) {
        let base = if ms < step { 40 } else { 100 };
        w.rtt_sample(
            start + Duration::from_millis(ms),
            Duration::from_millis(base + ms / 10 % 11),
            false,
        );
    }
    assert_eq!(w.window(), 2 * 8000 * 100 / 8);
}

/// M6: `feedback_of` accepts any receiver clock that cannot overflow with its arrivals; one that
/// jumps by 2^63 µs must not overflow the one-way delay. Old code: "attempt to subtract with
/// overflow".
#[test]
fn a_receiver_clock_jump_the_parser_accepts_does_not_overflow() {
    let start = Instant::now();
    let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
    for i in 0..40 {
        send_nth(&mut log, start, 0, i, 10 + u64::from(i));
    }
    for (first, ref_us) in [(0u32, 0u64), (20, 1 << 63), (30, 0)] {
        let message = serde_json::json!({
            "type": "feedback", "seq": 1, "first_pkt_seq": first, "count": 10,
            "ref_us": ref_us, "recv_kbps": 0, "lost": 0, "interval_ms": 50,
            "arrivals_b64": base64::engine::general_purpose::STANDARD.encode([0u8; 20]),
        });
        let fb = feedback_of(&message).expect("in contract");
        est.on_feedback(&fb, &mut log, start + Duration::from_millis(100), NO_QUIC);
    }
}

/// M7: a shallow 5 Mbit/s cap for 40 s under the real ABR, the stall guard and production's
/// pacing, then the cap lifts. After a burst the hold keeps the rate under what the link delivered,
/// so the estimate (which never saw the excess) does not drive it straight back into the cap; and
/// both the estimate and the setpoint come back after the lift, once the hold has let go. The rate
/// is what the resolution rung reads: it steps down once the rate stays under
/// `engage_frac` × the rung's comfort for `RATE_CONFIRM` (`LadderPlanner::observe_rate`). Here
/// the loss also cuts the estimate, which caps the up path; with the estimate left high see
/// `abr::tests::per_report_readings_with_a_high_estimate_hold_the_rate_long_enough_for_the_rung`.
#[test]
fn a_shallow_cap_then_a_lift_bounds_the_abr_and_recovers_it() {
    let mut s = Sim::new(5000.0).with_abr().with_guard();
    s.quic = Some(Quic::default());
    s.queue_bytes = 16.0 * SIZE as f64;
    s.keyframe_every = 60;
    s.run(40.0);
    let downs = s
        .setpoints
        .windows(2)
        .filter(|w| w[0].0 >= 10.0 && w[1].1 < w[0].1)
        .count();
    let mean = s
        .setpoints
        .iter()
        .filter(|&&(t, _)| t >= 10.0)
        .map(|&(_, sp)| f64::from(sp))
        .sum::<f64>()
        / s.setpoints.iter().filter(|&&(t, _)| t >= 10.0).count() as f64;
    // At the launch rung the comfort is the ceiling.
    let engage = crate::session::ladder::ResolutionPolicy::DEFAULT_ENGAGE_FRAC * CEILING;
    let (mut under, mut longest) = (None::<f64>, 0.0f64);
    for &(t, r) in s.rates.iter().filter(|&&(t, _)| t >= 5.0) {
        match (r < engage, under) {
            (true, None) => under = Some(t),
            (true, Some(from)) => longest = longest.max(t - from),
            (false, _) => under = None,
        }
    }
    s.cap_kbps = 50_000.0;
    s.run(120.0);
    let back = |v: &[(f64, f64)]| {
        v.iter()
            .find(|&&(t, x)| t >= 40.0 && x >= CEILING)
            .map(|&(t, _)| t - 40.0)
    };
    let sp: Vec<(f64, f64)> = s
        .setpoints
        .iter()
        .map(|&(t, x)| (t, f64::from(x)))
        .collect();
    let (est_back, sp_back) = (back(&s.trace), back(&sp));
    eprintln!(
        "under the cap 10-40 s: {downs} down moves, mean setpoint {mean:.0}, rate under the \
         rung's engage line for at most {longest:.2} s at a time; after the lift: estimate back \
         in {est_back:?} s, setpoint in {sp_back:?} s"
    );
    assert!(
        longest >= crate::session::ladder::LadderPlanner::RATE_CONFIRM.as_secs_f64(),
        "the rung would never step down: under the line {longest:.2} s at most"
    );
    assert!(downs <= 6, "{downs} down moves in 30 s");
    assert!(mean < 5000.0, "mean setpoint {mean:.0} over the cap");
    // The hold's cost (`HOLD`): back in 29 s and 36 s here, 8 s with a 30 s hold.
    assert!(est_back.is_some_and(|b| b <= 35.0), "estimate {est_back:?}");
    assert!(sp_back.is_some_and(|b| b <= 45.0), "setpoint {sp_back:?}");
}

// Capacity detection v2 (#1433): the acceptance tests of the design. Each asserts what v2 does,
// not what this code does, so the ones the old detector fails are red until the product follows.

/// `frames` frames from `from_us`, one every 16 667 µs, each `per` datagrams `pace_us` apart (a
/// frame the pacer lets out in a burst), logged as sent; feedback naming each as received at
/// `arrive(pkt_seq, sent µs)` on the client's clock (the agent's), or lost.
fn frames_report(
    log: &mut SendLog,
    start: Instant,
    first: u32,
    from_us: u64,
    (frames, per, pace_us): (u32, u32, u64),
    arrive: impl Fn(u32, u64) -> Option<u64>,
) -> Feedback {
    let mut sent = Vec::new();
    for f in 0..frames {
        for i in 0..per {
            let seq = first + f * per + i;
            let at = from_us + u64::from(f) * 16_667 + u64::from(i) * pace_us;
            let mut d = vec![0u8; SIZE];
            d[1..5].copy_from_slice(&seq.to_le_bytes());
            log.record(&d, start + Duration::from_micros(at), false);
            sent.push(arrive(seq, at));
        }
    }
    let ref_us = sent.iter().flatten().copied().min().unwrap_or(from_us);
    Feedback {
        first_pkt_seq: first,
        ref_us,
        arrivals: sent
            .iter()
            .map(|a| a.map_or(NOT_RECEIVED, |a| ((a - ref_us) / UNIT_US) as u16))
            .collect(),
    }
}

/// When the last datagram a report names arrived: the agent reads it just after.
fn read_at(start: Instant, fb: &Feedback) -> Instant {
    let last = fb.arrivals.iter().filter(|&&a| a != NOT_RECEIVED).max();
    start + Duration::from_micros(fb.ref_us + u64::from(*last.unwrap_or(&0)) * UNIT_US + 1000)
}

/// Frames of 14 datagrams at 60 fps: 7.7 Mbit/s over the 8 Mbit/s ceiling's 0.8 line, so a
/// window of it is never application-limited.
const BUSY: (u32, u32, u64) = (6, 14, 980);

/// `secs` of `BUSY` reports, 100 ms each, read as they arrive: what each one measured and what
/// the ABR was given.
fn busy_reports(
    secs: u64,
    arrive: impl Fn(u32, u64) -> Option<u64>,
) -> (Estimator, Vec<(Measured, Option<f64>)>) {
    let start = Instant::now();
    let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
    let out = (0..secs * 10)
        .map(|r| {
            let first = r as u32 * BUSY.0 * BUSY.1;
            let from = r * u64::from(BUSY.0) * 16_667;
            let fb = frames_report(&mut log, start, first, from, BUSY, &arrive);
            let now = read_at(start, &fb);
            let m = est.measure(&fb, &mut log, now);
            est.apply(m, now, NO_QUIC);
            (m, reading(&est))
        })
        .collect();
    (est, out)
}

/// R8: a +60 ms one-way step on a link far above the ceiling (50 Mbit/s), with the sender at the
/// ceiling under the real ABR and stall guard, is no evidence of anything. The window must cover
/// the BDP at the new RTT, or the sender is throttled by its own window and what it holds ages in
/// the agent (lab: setpoint 8000 → 2749, five `video-stale`, six freezes).
#[test]
fn a_delay_step_on_an_uncapped_link_moves_nothing() {
    let mut s = Sim::new(50_000.0).with_abr().with_guard();
    s.quic = Some(Quic::default());
    s.jitter_us = 2000;
    s.extra_us = Box::new(|t| if t >= 10_000_000 { 60_000 } else { 0 });
    s.run(40.0);
    let readings: Vec<_> = s.readings.iter().filter(|r| r.1.is_some()).collect();
    assert!(
        readings.is_empty(),
        "the ABR was given readings: {readings:?}"
    );
    let down = s.setpoints.windows(2).find(|w| w[1].1 < w[0].1);
    assert!(down.is_none(), "the setpoint moved down: {down:?}");
    let stale = s.quic.as_ref().map_or(0, |q| q.video.stale);
    assert!(
        s.stalls.is_empty() && stale == 0,
        "the guard stalled at {:?} s, {stale} datagrams went stale",
        s.stalls
    );
}

/// R8: the governed window covers any path up to a 200 ms round trip, whatever the RTT floor
/// the sender last saw (a 5 ms LAN): the BDP at 8 Mbit/s is 80 kB at 80 ms, 200 kB at 200 ms.
/// Old code: 2 × 8 Mbit/s × 30 ms, 60 kB.
#[test]
fn the_governed_window_covers_a_200_ms_round_trip() {
    let start = Instant::now();
    let governor = Governor::default();
    let mut w = window(&governor, start);
    governor.govern(8000.0);
    for (srtt_ms, bdp) in [(80, 80_000), (200, 200_000)] {
        acked(
            &mut w,
            Duration::from_millis(srtt_ms),
            Duration::from_millis(5),
        );
        assert!(w.window() >= bdp, "{srtt_ms} ms: {} < {bdp}", w.window());
    }
}

/// R8, R3, R1: one arrival gap in the 500 ms rate window is no shortfall, a step and a blackout
/// alike; a queue that stretches every gap still is one. The ABR's reading is `BETA` × what the
/// link delivered.
#[test]
fn one_arrival_gap_in_the_rate_window_is_no_shortfall() {
    let (est, step) = busy_reports(5, |_, t| {
        Some(t + 11_000 + if t >= 2_000_000 { 60_000 } else { 0 })
    });
    assert!(
        step.iter().all(|r| r.1.is_none()),
        "a +60 ms step: {:?}",
        step.iter().find(|r| r.1.is_some())
    );
    assert_eq!(est.estimate_kbps(), CEILING, "the step cut the estimate");

    let (est, dark) = busy_reports(5, |_, t| {
        (!(2_000_000..2_300_000).contains(&t)).then_some(t + 11_000)
    });
    assert!(
        dark.iter().all(|r| r.1.is_none()),
        "a 300 ms blackout: {:?}",
        dark.iter().find(|r| r.1.is_some())
    );
    assert_eq!(
        est.estimate_kbps(),
        CEILING,
        "the blackout cut the estimate"
    );

    // A link that carries 85 % of what is sent: every gap is longer, none stands out.
    let (_, deep) = busy_reports(6, |_, t| Some(11_000 + (t as f64 / 0.85) as u64));
    let (m, read) = deep.last().copied().unwrap();
    let delivered = m.rate_kbps.expect("a rate");
    assert!(
        read.is_some_and(|r| (0.9 * BETA * delivered..1.1 * BETA * delivered).contains(&r)),
        "delivered {delivered:.0}, reading {read:?}"
    );
}

/// R3: a full shallow buffer drops whatever arrives while it is full: frame starts, middles and
/// tails alike, 12 % of a paced 7.7 Mbit/s. The loss bound cuts, and the hold keeps the sender
/// under `BETA` × what was delivered. The loss is no reading: what arrived came at its send spacing.
#[test]
fn a_full_shallow_buffer_cuts_the_loss_bound_and_holds_the_sender_under_what_it_delivered() {
    let start = Instant::now();
    let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
    let lost = |seq: u32| {
        matches!(
            (seq / BUSY.1 % 3, seq % BUSY.1),
            (0, 0) | (0, 13) | (1, 0) | (1, 7) | (2, 13)
        )
    };
    let mut readings = Vec::new();
    for r in 0..20u64 {
        let first = r as u32 * BUSY.0 * BUSY.1;
        let from = r * u64::from(BUSY.0) * 16_667;
        let fb = frames_report(&mut log, start, first, from, BUSY, |seq, t| {
            (!lost(seq)).then_some(t + 11_000)
        });
        let now = read_at(start, &fb);
        let m = est.measure(&fb, &mut log, now);
        est.apply(m, now, NO_QUIC);
        readings.push((m.rate_kbps, reading(&est), est.held_kbps()));
    }
    assert!(
        est.loss_kbps <= 0.96 * CEILING,
        "the loss bound is at {:.0} after 2 s of 12 % loss",
        est.loss_kbps
    );
    for (delivered, read, held) in &readings[10..] {
        let delivered = delivered.expect("a rate");
        assert_eq!(*read, None, "a reading on loss");
        assert!(
            held.is_some_and(|h| h <= BETA * delivered * 1.02),
            "delivered {delivered:.0}, held {held:?}"
        );
    }
}

/// Steady loss that is not congestion (Wi-Fi, reorder discards): 3 % at random on a link far over
/// the 8000 kbit/s ceiling, FEC on, under the real ABR and stall guard. Loss under the cut line
/// never lowers the estimate: it stays at the ceiling and FEC keeps its share (`Control::feed`
/// charges at most estimate / floor − 1).
#[test]
fn steady_loss_on_an_uncapped_link_never_ratchets_the_estimate() {
    let mut s = Sim::new(50_000.0).with_abr().with_guard();
    s.quic = Some(Quic::default());
    s.keyframe_every = 60;
    s.fec_share = 0.1;
    s.loss_at = Box::new(|_| 0.03);
    s.run(180.0);
    let low = s.min_after(5.0);
    let floor = f64::from(s.abr.as_ref().unwrap().floor_kbps());
    eprintln!(
        "estimate from 5 s: min {low:.0}, end {:.0}; loss {:.3}; setpoint end {:?}; stalls {:?}",
        s.est.estimate_kbps(),
        s.loss_after(5.0),
        s.setpoints.last(),
        s.stalls
    );
    assert!(low >= 0.95 * CEILING, "the estimate fell to {low:.0}");
    assert!(low / floor - 1.0 >= s.fec_share, "FEC starved at {low:.0}");
    assert!(s.stalls.is_empty(), "stalls {:?}", s.stalls);
}

/// 2.5 % random loss at a 2500 kbit/s session on a link far over it, and one 300 ms burst of
/// 25 % at 20 s. The burst may move the setpoint down; the steady loss under it must not hold it
/// there: the hold it sets ends once backing off shows the loss does not fall with the rate, the
/// setpoint is back at the ceiling by 60 s and the guard never stalls after that. With no such
/// release the hold never ends. The stalls during the burst are the sim's whole-datagram frames
/// (1745 kbit/s rounds to 1.26 × its rate, over the pacer's 1.25 ×).
#[test]
fn random_loss_and_one_burst_recover_to_the_ceiling_without_a_stall() {
    const SESSION: f64 = 2500.0;
    let mut s = Sim::new(50_000.0).with_guard();
    s.est = Estimator::new(SESSION, s.start);
    let cfg = crate::session::abr::AbrConfig {
        per_report: Some(Duration::ZERO),
        ..crate::session::abr::AbrConfig::new_smooth(500, SESSION as u32, 60)
    };
    s.abr = Some(crate::session::abr::Governor::new(cfg, s.start));
    s.quic = Some(Quic::default());
    s.keyframe_every = 60;
    s.loss_at = Box::new(|t| match (20_000_000..20_300_000).contains(&t) {
        true => 0.25,
        false => 0.025,
    });
    s.run(90.0);
    let low = s.setpoints.iter().map(|&(_, sp)| sp).min();
    let back = s
        .setpoints
        .iter()
        .find(|&&(t, sp)| t >= 20.3 && f64::from(sp) >= SESSION)
        .map(|&(t, _)| t);
    let after: Vec<_> = s
        .stalls
        .iter()
        .filter(|&&t| back.is_some_and(|b| t >= b))
        .collect();
    let mut steps: Vec<u32> = Vec::new();
    for &(_, sp) in &s.setpoints {
        if steps.last() != Some(&sp) {
            steps.push(sp);
        }
    }
    eprintln!(
        "setpoint min {low:?}, back at {back:?} s, path {:?}; stalls {:?}; estimate end {:.0}",
        &steps[..steps.len().min(20)],
        s.stalls,
        s.est.estimate_kbps()
    );
    assert!(back.is_some_and(|b| b <= 60.0), "back at {back:?} s");
    assert!(after.is_empty(), "stalls after recovery: {after:?}");
}

/// Rule 1 (#1433), system level: an 800 ms delay spike in a quiet scene cuts the estimate to the
/// quiet receive rate, and the setpoint stays at the ceiling (an idle sender gives no reading).
/// When the scene turns busy the encoder sends at its setpoint while the estimate is far under
/// it and regrows at 8 % a second. The pacer and QUIC's window follow what the encoder sends, so
/// nothing waits in the agent: no stall, no stale drop. Governed at the estimate: the agent holds
/// the excess and the guard stalls.
#[test]
fn a_busy_scene_after_a_cut_in_a_quiet_one_leaves_nothing_waiting_in_the_agent() {
    let mut s = Sim::new(50_000.0).with_abr().with_guard();
    s.quic = Some(Quic::default());
    s.keyframe_every = 60;
    s.content_kbps = Box::new(|t| if t < 6_000_000 { 1500.0 } else { f64::MAX });
    s.extra_us = Box::new(|t| match t {
        3_000_000..3_200_000 => (t - 3_000_000) * 800 / 200,
        3_200_000..3_500_000 => (3_500_000 - t) * 800 / 300,
        _ => 0,
    });
    s.run(6.0);
    let low = s.est.estimate_kbps();
    s.run(14.0);
    let stale = s.quic.as_ref().map_or(0, |q| q.video.stale);
    let late: Vec<_> = s.stalls.iter().filter(|&&t| t >= 6.0).collect();
    eprintln!(
        "estimate at 6 s {low:.0}, setpoint {:?}; stalls {:?}, {stale} stale",
        s.setpoints.last(),
        s.stalls
    );
    assert!(low < 0.5 * CEILING, "no cut in the quiet scene: {low:.0}");
    assert!(
        late.is_empty() && stale == 0,
        "stalls {late:?}, {stale} stale"
    );
}

/// Rule 2's gap correction takes out the largest excesses of an arrival gap over its send gap, not
/// the largest arrival gaps. Unpaced at 30 fps, 29 datagrams a frame 20 µs apart (8 Mbit/s): each
/// gap between frames is 32.8 ms of arrival and as much of sending. Two +30 ms steps land inside
/// frames' bursts in one window, each a gap shorter than a frame gap: both are taken out, and the
/// window is no shortfall. Taking out the largest arrival gap removes a frame gap and leaves the
/// steps: a reading.
#[test]
fn delay_steps_inside_unpaced_frames_are_the_gaps_taken_out() {
    let start = Instant::now();
    let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
    let step = |t: u64| 30_000 * (u64::from(t >= 1_000_300) + u64::from(t >= 1_200_300));
    let mut reports = Vec::new();
    for r in 0..20u32 {
        let mut arrivals = Vec::new();
        for f in r * 3..r * 3 + 3 {
            for i in 0..29u32 {
                let at = u64::from(f) * 33_333 + u64::from(i) * 20;
                let mut d = vec![0u8; SIZE];
                d[1..5].copy_from_slice(&(f * 29 + i).to_le_bytes());
                log.record(&d, start + Duration::from_micros(at), false);
                arrivals.push(at + 11_000 + step(at));
            }
        }
        let ref_us = arrivals[0];
        let fb = Feedback {
            first_pkt_seq: r * 87,
            ref_us,
            arrivals: arrivals
                .iter()
                .map(|a| ((a - ref_us) / UNIT_US) as u16)
                .collect(),
        };
        let now = read_at(start, &fb);
        let m = est.measure(&fb, &mut log, now);
        est.apply(m, now, NO_QUIC);
        reports.push((r, m.gap_us, reading(&est)));
    }
    assert!(
        reports.iter().any(|&(_, gap, _)| gap >= 59_000),
        "both steps not taken out: {reports:?}"
    );
    let read = reports.iter().find(|r| r.2.is_some());
    assert!(read.is_none(), "a reading: {read:?}");
}

/// R4: 11 % of every datagram lost, evenly, never grows the estimate: the loss bound cuts above
/// 10 % and holds there. Old code: a window the loss class did not name took the grow arm.
#[test]
fn ten_percent_loss_never_grows_the_estimate() {
    let mut s = Sim::new(50_000.0);
    s.send_kbps = Some(4000.0);
    s.drop_every = 9;
    s.est.resume_at(4000.0);
    s.run(3.0);
    // From the first report that holds `LOSS_MIN_SAMPLES` datagrams.
    let trace: Vec<_> = s.trace.iter().filter(|&&(t, _)| t >= 0.3).collect();
    let grew = trace.windows(2).find(|w| w[1].1 > w[0].1);
    assert!(grew.is_none(), "the estimate grew: {grew:?}");
    assert!(
        s.est.estimate_kbps() < 4000.0,
        "{:.0}",
        s.est.estimate_kbps()
    );
}

/// #1622: a keyframe burst after a quiet scene on a reordering link (each datagram held 0–40 ms
/// on its own, no loss) is no outage, no loss and no shortfall. The burst, 73 datagrams after
/// 5 s of one a frame, goes out at 5 s; the second after it is judged report by report: none
/// counts a loss, none gives the ABR a reading, the loss bound is never cut and the setpoint
/// never moves down.
#[test]
fn a_keyframe_burst_after_a_quiet_scene_on_a_reordering_link_moves_nothing() {
    let mut s = reordering(0).with_abr();
    s.quic = Some(Quic::default());
    s.content_kbps = Box::new(|t| if t < 5_000_000 { 1.0 } else { f64::MAX });
    s.keyframe_every = 300;
    s.run(8.0);
    let burst = |t: f64| (5.0..6.0).contains(&t);
    assert!(
        s.keys_handed.iter().any(|&t| (5.0..5.1).contains(&t)),
        "no keyframe after the quiet scene: {:?}",
        s.keys_handed
    );
    let judged: Vec<_> = s.losses.iter().filter(|l| burst(l.0)).collect();
    assert!(
        judged.len() >= 10 && judged.iter().all(|l| l.1.is_some_and(|l| l == 0.0)),
        "the burst's reports counted a loss: {judged:?}"
    );
    let read = s.readings.iter().find(|r| burst(r.0) && r.1.is_some());
    assert!(read.is_none(), "the ABR was given a reading: {read:?}");
    // From 3 s: the start-up keyframe is not the burst after a quiet scene.
    let loss_cut = s
        .loss_bounds
        .iter()
        .find(|&&(t, kbps)| t >= 3.0 && kbps < CEILING);
    assert!(loss_cut.is_none(), "the loss bound was cut: {loss_cut:?}");
    let down = s
        .setpoints
        .windows(2)
        .find(|w| w[0].0 >= 3.0 && w[1].1 < w[0].1);
    assert!(down.is_none(), "the setpoint moved down: {down:?}");
}

/// RB (design test 11): 60, 150 and 300 ms of total loss every 10 s on the path to the client,
/// on a link far over the ceiling, are blackouts: no loss cut and no ABR move, and the guard
/// stalls at most once a burst.
#[test]
fn burst_loss_of_60_to_300_ms_moves_nothing() {
    let mut s = Sim::new(50_000.0).with_abr().with_guard();
    s.quic = Some(Quic::default());
    s.keyframe_every = 60;
    s.run(5.0);
    let bursts = [60u64, 150, 300, 60, 150, 300];
    for ms in bursts {
        s.run(10.0 - ms as f64 / 1000.0);
        s.drop_every = 1;
        s.run(ms as f64 / 1000.0);
        s.drop_every = 0;
    }
    s.run(5.0);
    let cut = s
        .loss_bounds
        .iter()
        .find(|&&(t, k)| t >= 5.0 && k < CEILING);
    assert!(cut.is_none(), "a loss cut: {cut:?}");
    let down = s.rates.windows(2).find(|w| w[1].1 < w[0].1);
    assert!(down.is_none(), "the ABR moved down: {down:?}");
    assert!(s.stalls.len() <= bursts.len(), "stalls {:?}", s.stalls);
}

/// 3 s of `BUSY` reports losing every 20th datagram (5 %, under the cut line): the loss bound
/// after them.
fn persistent_loss(est: &mut Estimator, start: Instant) -> f64 {
    let mut log = SendLog::new(start);
    for r in 0..30u64 {
        let first = r as u32 * BUSY.0 * BUSY.1;
        let from = r * u64::from(BUSY.0) * 16_667;
        let fb = frames_report(&mut log, start, first, from, BUSY, |seq, t| {
            (!seq.is_multiple_of(20)).then_some(t + 11_000)
        });
        let now = read_at(start, &fb);
        let m = est.measure(&fb, &mut log, now);
        est.apply(m, now, NO_QUIC);
    }
    est.loss_kbps
}

/// PyroWave (design §5, out of scope): under `legacy` its estimator is 0.6.3's, a blackout counted
/// as loss; under v2 it keeps the blackout exclusion and never holds (`HOLD`): it has its own
/// controller and no keyframes to overflow a link.
#[test]
fn pyrowave_follows_the_capacity_knob_and_never_holds() {
    let start = Instant::now();
    let mut legacy = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, start, true);
    let m = blackout_report(
        &mut legacy.estimator,
        &mut legacy.log,
        start,
        600..750,
        |_| 1,
    );
    assert!(m.loss.is_some_and(|l| l > 0.1), "legacy: {:?}", m.loss);
    let mut v2 = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, start, false);
    let m = blackout_report(&mut v2.estimator, &mut v2.log, start, 600..750, |_| 1);
    assert_eq!(m.loss, Some(0.0), "v2: the blackout is loss");
    for legacy in [false, true] {
        let mut rate = PyroRate::new(PYRO_TARGET, PYRO_FLOOR, start, legacy);
        let bound = persistent_loss(&mut rate.estimator, start);
        assert_eq!(bound, f64::from(PYRO_TARGET), "legacy {legacy}: cut");
        let burst = received(100_000.0, 0.25);
        rate.estimator
            .apply(burst, start + Duration::from_secs(9), NO_QUIC);
        assert_eq!(rate.estimator.held_kbps(), None, "legacy {legacy}: held");
    }
}

/// `QUASAR_WT_CAPACITY=legacy` (design §4): no pacing and no audio room, 0.6.3's reading (the
/// estimate or `BETA` × the receive rate, no gap taken out), blackouts counted as loss, and the
/// window on the smoothed RTT.
#[test]
fn legacy_capacity_is_the_0_6_3_sender() {
    let start = Instant::now();
    let legacy = || Estimator {
        legacy: true,
        ..Estimator::new(CEILING, start)
    };
    let governor = Governor::default();
    governor.govern(5000.0);
    let mut est = legacy();
    est.resume_at(5000.0);
    assert_eq!(video_rates(&governor, &est, false), (None, None), "paced");
    est.resume_at(5000.0);
    est.legacy = false;
    assert_eq!(
        video_rates(&governor, &est, false).1,
        Some(5000),
        "v2 paces"
    );

    // One +60 ms gap in a window that delivered what was sent: v2 takes it out, legacy reads it.
    let shortfall = Measured {
        usage: Usage::Normal,
        rate_kbps: Some(6000.0),
        delivered_kbps: Some(7900.0),
        gap_us: 60_000,
        sent_kbps: Some(8000.0),
        arrived_sent_kbps: Some(8000.0),
        probe_sent_kbps: 0.0,
        fresh: true,
        loss: None,
    };
    let (mut v2, mut old) = (Estimator::new(CEILING, start), legacy());
    v2.apply(shortfall, start + Duration::from_millis(100), NO_QUIC);
    old.apply(shortfall, start + Duration::from_millis(100), NO_QUIC);
    assert_eq!(v2.measured(), None);
    assert_eq!(
        old.measured(),
        Some(CEILING),
        "the estimate, over 0.85 × 6000"
    );

    // The reading is taken after this report's own loss cut, as 0.6.3 did.
    let cut = Measured {
        loss: Some(0.2),
        ..shortfall
    };
    let mut old = legacy();
    old.apply(cut, start + Duration::from_millis(100), NO_QUIC);
    assert_eq!(
        old.measured(),
        Some(0.9 * CEILING),
        "the estimate after its cut"
    );

    // No hold (`HOLD`): neither a burst nor two stretched windows in a row hold the sender.
    let stretched = Measured {
        delivered_kbps: Some(6000.0),
        ..shortfall
    };
    old.apply(stretched, start + Duration::from_millis(200), NO_QUIC);
    old.apply(stretched, start + Duration::from_millis(300), NO_QUIC);
    assert_eq!(old.held_kbps(), None, "legacy held");
    assert_eq!(old.hold(CEILING), CEILING);

    // A 150 ms blackout is loss.
    let (mut log, mut old) = (SendLog::new(start), legacy());
    let m = blackout_report(&mut old, &mut log, start, 600..750, |_| 1);
    assert!(m.loss.is_some_and(|l| l > 0.1), "{:?}", m.loss);

    // Loss under the cut line that persists holds the bound.
    let mut old = legacy();
    assert_eq!(persistent_loss(&mut old, start), CEILING, "legacy cut");

    // The window follows a queue's smoothed RTT.
    let mut w = window(&governor, start);
    governor.govern(8000.0);
    governor.legacy(true);
    acked(&mut w, Duration::from_secs(2), Duration::from_millis(150));
    assert_eq!(w.window(), 4_000_000, "2 × 8 Mbit/s × 2 s");
}

/// R1 (lead's question on rule 1): on a deep buffer (5 Mbit/s, 500 ms of `tbf` queue) the encoder's
/// average output reaches the link, so audio could again wait behind video in the link's queue
/// (0.6.3: ~116 underruns a minute on the lab row). The descent drains it: after convergence an
/// audio datagram entering the link waits a bounded time, at every keyframe too.
#[test]
fn a_deep_buffer_keeps_the_links_queue_short_for_audio() {
    let mut s = Sim::new(5000.0).with_abr().with_guard();
    s.quic = Some(Quic::default());
    s.queue_bytes = 312_500.0;
    s.keyframe_every = 60;
    s.run(60.0);
    let mut waits: Vec<u64> = s
        .waits
        .iter()
        .filter(|&&(t, _)| t >= 15.0)
        .map(|&(_, w)| w / 1000)
        .collect();
    waits.sort_unstable();
    let at = |q: f64| waits[((waits.len() - 1) as f64 * q) as usize];
    eprintln!(
        "link queue from 15 s: p50 {} ms, p95 {} ms, max {} ms; stalls {:?}",
        at(0.5),
        at(0.95),
        at(1.0),
        s.stalls
    );
    // The lab's R1 rule: RTT p95 at most 60 ms over the clean row. Head: the same queue, and six
    // guard stalls in the first 3 s.
    assert!(at(0.95) <= 60 && at(1.0) <= 100, "p95 {} ms", at(0.95));
    assert!(s.stalls.is_empty(), "stalls {:?}", s.stalls);
}

/// One run of random i.i.d. loss `p` on a 50 Mbit/s link, FEC 0.1, the real ABR and stall guard:
/// (setpoint at the end, the estimate's minimum after 5 s, stall times).
fn random_loss_run(p: f64, secs: f64) -> (u32, f64, Vec<f64>) {
    let mut s = Sim::new(50_000.0).with_abr().with_guard();
    s.quic = Some(Quic::default());
    s.keyframe_every = 60;
    s.fec_share = 0.1;
    s.loss_at = Box::new(move |_| p);
    s.run(secs);
    let end = s.setpoints.last().map_or(0, |&(_, sp)| sp);
    (end, s.min_after(5.0), s.stalls)
}

/// Random 3–6 % loss on an uncapped link (Wi-Fi) is no congestion: at the end of 180 s the
/// setpoint is within 0.9 × of what 0.6.3 holds there (the ceiling less the FEC share, 7273), with
/// no stall. Reading every random loss as a shortfall: 6665, 4872, 3864, 1685.
#[test]
fn random_loss_of_3_to_6_percent_on_an_uncapped_link_keeps_the_setpoint() {
    let held = CEILING / 1.1;
    let mut rows = Vec::new();
    for p in [0.03, 0.04, 0.05, 0.06] {
        let (end, low, stalls) = random_loss_run(p, 180.0);
        eprintln!("loss {p}: setpoint end {end}, estimate min {low:.0}, stalls {stalls:?}");
        rows.push((p, end, low, stalls));
    }
    for (p, end, _, stalls) in &rows {
        assert!(f64::from(*end) >= 0.9 * held, "loss {p}: setpoint {end}");
        assert!(stalls.is_empty(), "loss {p}: stalls {stalls:?}");
    }
}

/// 90 s on a 50 Mbit/s link losing `p` at random, FEC 0.1, the real ABR and stall guard, then a
/// 5 Mbit/s cap (256 kB buffer) for 60 s: (when video plus FEC first fit the cap, stall times
/// after the cap).
fn random_loss_then_cap(p: f64) -> (Option<f64>, Vec<f64>) {
    const CAP: f64 = 5000.0;
    let mut s = Sim::new(50_000.0).with_abr().with_guard();
    s.quic = Some(Quic::default());
    s.keyframe_every = 60;
    s.fec_share = 0.1;
    s.loss_at = Box::new(move |_| p);
    s.run(90.0);
    s.cap_kbps = CAP;
    s.run(60.0);
    let under = s
        .setpoints
        .iter()
        .find(|&&(t, sp)| t >= 90.0 && f64::from(sp) * (1.0 + s.fec_share) <= CAP)
        .map(|&(t, _)| t);
    let stalls = s.stalls.into_iter().filter(|&t| t >= 90.0).collect();
    (under, stalls)
}

/// Random 3 % loss, then a real cap: what the random loss did does not hide the cap. The sender
/// fits under it as fast as on a link with no random loss (within 1 s), and stalls no more than
/// that link's cap step does.
#[test]
fn random_loss_then_a_real_cap_still_steps_down_to_the_cap() {
    let (under, stalls) = random_loss_then_cap(0.03);
    let (clean_under, clean_stalls) = random_loss_then_cap(0.0);
    eprintln!(
        "3 %: under the cap at {under:?} s, stalls {stalls:?}; no random loss: under at \
         {clean_under:?} s, stalls {clean_stalls:?}"
    );
    assert!(
        under.is_some_and(|t| t <= 91.0),
        "under the cap at {under:?} s"
    );
    assert!(under <= clean_under, "{under:?} after {clean_under:?}");
    assert!(stalls.len() <= clean_stalls.len(), "stalls {stalls:?}");
}

// The lab rows R3 and R4 (#1433 lab 2026-11-21, NVIDIA host): side V (capacity v2 at c2a2a0dd)
// froze 6-7 times per R3 run and 4-13 per R4 run and held 720p for 70-140 s; side A (0.6.3)
// stepped to 540p in ~10 s on R3 and to 360p on R4 with 1-3 freezes. These rows carry the facts
// the earlier sim lacked: the rung ladder, keyframes on request and at steps, the client's
// freezes, audio, FEC at the measured share and the `tbf` bucket.

/// A lab row: 30 s on a clean 50 Mbit/s link, then `shape` from 30 s (the onset) for 180 s, with
/// keyframes of `key_ratio` average frames.
fn lab_row(legacy: bool, shape: fn(&mut Sim), key_ratio: f64) -> Sim {
    let mut s = Sim::lab(legacy);
    s.key_ratio = key_ratio;
    s.run(30.0);
    shape(&mut s);
    (s.tokens, s.tokens_at) = (15_000.0, s.t);
    s.run(180.0);
    s
}

/// R3: `tbf rate 5mbit burst 15kb latency 10ms`, a 21 kB queue behind a 15 kB bucket.
fn shallow(s: &mut Sim) {
    s.cap_kbps = 5000.0;
    s.queue_bytes = 21_250.0;
    s.tbf_burst = Some(15_000.0);
}

/// R4: `police rate 5mbit burst 15k drop`, no queue.
fn policer(s: &mut Sim) {
    s.cap_kbps = 5000.0;
    s.bucket_bytes = Some(15_000.0);
}

/// (freezes after the onset, the rung path) of a lab row; printed with the stalls, the setpoint's
/// minimum after the onset and the loss after onset + 20 s.
fn lab_summary(name: &str, s: &Sim) -> (Vec<f64>, Vec<(f64, usize)>) {
    let freezes: Vec<f64> = s.client.as_ref().unwrap().freezes();
    let freezes: Vec<f64> = freezes.into_iter().filter(|&t| t >= 30.0).collect();
    let rungs: Vec<(f64, usize)> = s
        .rungs
        .iter()
        .map(|&(t, r)| ((t * 10.0).round() / 10.0, r))
        .collect();
    let low = s
        .setpoints
        .iter()
        .filter(|&&(t, _)| t >= 30.0)
        .map(|&(_, sp)| sp)
        .min()
        .unwrap_or(0);
    let loss = s.loss_after(50.0);
    eprintln!(
        "{name}: freezes {} at {:?}; rungs {rungs:?}; stalls {:?}; setpoint min {low}; loss after \
         onset + 20 s {:.3}",
        freezes.len(),
        freezes
            .iter()
            .map(|t| (t * 10.0).round() / 10.0)
            .collect::<Vec<_>>(),
        s.stalls,
        loss
    );
    (freezes, rungs)
}

/// The rung steps up after the onset of a lab row: (seconds, rung).
fn ups_while_shaped(rungs: &[(f64, usize)]) -> Vec<(f64, usize)> {
    rungs
        .windows(2)
        .filter(|w| w[1].1 < w[0].1 && w[1].0 > 30.0)
        .map(|w| w[1])
        .collect()
}

/// R3: the sender reaches 540p within 15 s of the onset, the client freezes at most twice, the
/// rung never steps up while shaped and the guard never stalls after onset + 15 s (the lab's R3
/// rule), with keyframes of 5 and 10 average frames (seat B, hold round: 10 × froze 6 times
/// when a keyframe over `PACE_MAX_WAIT` of pace went into the link unpaced). 15 × is printed.
#[test]
fn a_shallow_buffer_keeps_its_frames_and_reaches_540p_in_15_s() {
    for ratio in [LAB_KEY_RATIO, 10.0, 15.0] {
        eprintln!("keyframes {ratio} x:");
        lab_summary("A (legacy)", &lab_row(true, shallow, ratio));
        let s = lab_row(false, shallow, ratio);
        let (freezes, rungs) = lab_summary("v2", &s);
        if ratio > 10.0 {
            continue;
        }
        let late: Vec<_> = s.stalls.iter().filter(|&&t| t >= 45.0).collect();
        assert!(
            s.rung_at(45.0) >= 3,
            "{ratio} x: rung {} at onset + 15 s",
            s.rung_at(45.0)
        );
        assert!(freezes.len() <= 2, "{ratio} x: {} freezes", freezes.len());
        assert!(
            late.is_empty(),
            "{ratio} x: stalls after onset + 15 s: {late:?}"
        );
        let ups = ups_while_shaped(&rungs);
        assert!(ups.is_empty(), "{ratio} x: rung up while shaped: {ups:?}");
    }
}

/// R4: the sender reaches A's rung (360p) or loses at most 2 % after onset + 20 s, freezes at most
/// once more than A, the rung never steps up while the policer is on (the lab's R4 rule), and
/// inside the hold the setpoint plus FEC never goes over its line, `BETA` × the least a burst
/// window delivered, one ABR feed late at most. Keyframes of 5 and 10 average frames; 15 × is printed.
#[test]
fn a_policer_reaches_its_rung_and_never_climbs_back() {
    for ratio in [LAB_KEY_RATIO, 10.0, 15.0] {
        eprintln!("keyframes {ratio} x:");
        let (a, _) = lab_summary("A (legacy)", &lab_row(true, policer, ratio));
        let s = lab_row(false, policer, ratio);
        let (freezes, rungs) = lab_summary("v2", &s);
        if ratio > 10.0 {
            continue;
        }
        assert!(
            s.rung_at(210.0) >= 4 || s.loss_after(50.0) <= 0.02,
            "{ratio} x: rung {}, loss {:.3}",
            s.rung_at(210.0),
            s.loss_after(50.0)
        );
        assert!(
            freezes.len() <= a.len() + 1,
            "{ratio} x: {} freezes, A {}",
            freezes.len(),
            a.len()
        );
        let ups = ups_while_shaped(&rungs);
        assert!(ups.is_empty(), "{ratio} x: rung up while shaped: {ups:?}");
        // The ABR is fed every `REPORT_EVERY` (two messages), so it follows the line one late.
        let over: Vec<_> = s
            .lines
            .windows(3)
            .zip(&s.setpoints[2..])
            .filter_map(|(w, &(t, sp))| {
                let line = w.iter().map(|l| l.1).collect::<Option<Vec<f64>>>()?;
                let line = line.into_iter().fold(0.0, f64::max);
                // The ABR never goes under 1080p's floor; the rung waits `HELD_DOWN_WAIT` to step.
                let floor = sp <= 2400 && s.rung_at(t) == 0;
                let over = f64::from(sp) * (1.0 + s.fec_share) > line + 1.0;
                (t > 30.0 && over && !floor).then_some((t, sp))
            })
            .take(5)
            .collect();
        assert!(
            over.is_empty(),
            "{ratio} x: setpoint over the line: {over:?}"
        );
    }
}

/// W2e's deep-buffer cap step (#1433 round 3): 90 s clean, then 5 Mbit/s behind a 256 kB buffer.
/// The readings step the sender under the cap; once the queue drains they stop, and the ABR must
/// not climb back toward an estimate over the cap. Head: 6 guard stalls (90.5-132.3 s).
#[test]
fn a_cap_step_into_a_deep_buffer_does_not_climb_back_over_it() {
    let (under, stalls) = random_loss_then_cap(0.0);
    eprintln!("under the cap at {under:?} s, stalls {stalls:?}");
    assert!(
        under.is_some_and(|t| t <= 91.0),
        "under the cap at {under:?} s"
    );
    assert!(stalls.len() <= 1, "stalls {stalls:?}");
}

/// A report of `kbps` received (and sent at that spacing) with window loss `loss`.
fn received(kbps: f64, loss: f64) -> Measured {
    Measured {
        usage: Usage::Normal,
        rate_kbps: Some(kbps),
        delivered_kbps: Some(kbps),
        gap_us: 0,
        sent_kbps: Some(kbps / (1.0 - loss)),
        arrived_sent_kbps: Some(kbps),
        probe_sent_kbps: 0.0,
        fresh: true,
        loss: Some(loss),
    }
}

/// The hold (`HOLD`): one window losing 25 % while the link delivered 4400 holds the sender under
/// 0.85 × 4400 for `HOLD` of clean reports, a lossy report renews it, and then the line probes up
/// at `PROBE_PER_S` toward 4400. Old code: the bound grew 8 % a second from the next clean report.
#[test]
fn after_a_burst_the_sender_stays_under_what_the_link_delivered() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    let at = |ms: u64| start + Duration::from_millis(ms);
    est.apply(received(4400.0, 0.25), at(100), NO_QUIC);
    assert_eq!(est.held_kbps(), Some(BETA * 4400.0));
    assert_eq!(est.measured(), None, "loss is no reading");
    for ms in (200..=30_000).step_by(100) {
        est.apply(received(4400.0, 0.0), at(ms), NO_QUIC);
    }
    est.apply(received(4400.0, 0.03), at(30_100), NO_QUIC);
    for ms in (30_200..=90_000).step_by(100) {
        est.apply(received(4400.0, 0.0), at(ms), NO_QUIC);
        assert_eq!(est.held_kbps(), Some(3740.0), "at {ms} ms");
        assert_eq!(est.hold(CEILING), 3740.0);
    }
    for ms in (90_200..=95_100).step_by(100) {
        est.apply(received(4400.0, 0.0), at(ms), NO_QUIC);
    }
    let probed = est.held_kbps().unwrap();
    let want = 3740.0 * PROBE_PER_S.powf(5.0);
    assert!(
        (probed - want).abs() < 0.01 * want,
        "probed to {probed:.0}, want {want:.0}"
    );
}

/// A congested window `HOLD_STEP_GAP` after the line last came down, from a sender under what the
/// link then delivered, shows the line did not cure it: the line steps down by `BETA` again; one
/// inside the gap is the same burst. Seat A (hold round): a window delivering as much as before
/// from a sender over the line (its floor) compounds nothing (old code: 3740 → 3179).
#[test]
fn a_burst_that_outlasts_the_line_steps_it_down_again() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    let at = |ms: u64| start + Duration::from_millis(ms);
    est.apply(received(4400.0, 0.25), at(100), NO_QUIC);
    est.apply(received(4400.0, 0.25), at(600), NO_QUIC);
    assert_eq!(est.held_kbps(), Some(3740.0), "the same burst");
    est.apply(received(4400.0, 0.25), at(1200), NO_QUIC);
    assert_eq!(est.held_kbps(), Some(3740.0), "the sender over the line");
    est.apply(received(3800.0, 0.12), at(2400), NO_QUIC);
    assert_eq!(est.held_kbps(), Some(BETA * 3740.0));
}

/// Seat B's hold round finding (#1433): a later burst losing as much at a send rate under the one
/// that set the line did not fall with the rate (Wi-Fi): once it is over, the sender still at its
/// rate, the hold ends, and bursts set no line for `HOLD` after it. The ratchet stepped the line
/// down on every such burst instead.
#[test]
fn a_burst_losing_as_much_at_a_lower_rate_releases_the_hold() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    let at = |ms: u64| start + Duration::from_millis(ms);
    est.apply(received(7000.0, 0.3), at(100), NO_QUIC);
    assert_eq!(est.held_kbps(), Some(BETA * 7000.0));
    for ms in (200..4000).step_by(100) {
        est.apply(received(5000.0, 0.0), at(ms), NO_QUIC);
    }
    est.apply(received(4000.0, 0.3), at(4000), NO_QUIC);
    assert!(
        est.held_kbps().is_some(),
        "released before the burst is over"
    );
    est.apply(received(5700.0, 0.0), at(4100), NO_QUIC);
    assert_eq!(est.held_kbps(), None, "released");
    for ms in (4200..8000).step_by(100) {
        est.apply(received(7000.0, 0.0), at(ms), NO_QUIC);
    }
    est.apply(received(7000.0, 0.3), at(8000), NO_QUIC);
    assert_eq!(
        est.held_kbps(),
        None,
        "a burst inside the memory sets nothing"
    );
    let mut capped = Estimator::new(CEILING, start);
    capped.apply(received(7000.0, 0.3), at(100), NO_QUIC);
    for ms in (200..4000).step_by(100) {
        capped.apply(received(5000.0, 0.0), at(ms), NO_QUIC);
    }
    capped.apply(received(4500.0, 0.1), at(4000), NO_QUIC);
    capped.apply(received(5000.0, 0.0), at(4100), NO_QUIC);
    assert!(
        capped.held_kbps().is_some(),
        "loss that fell with the rate keeps it"
    );
}

/// The hold of `a_burst_losing_as_much_at_a_lower_rate_releases_the_hold` up to its later burst at
/// 4000, with `key` marking that burst's losses as a keyframe's; `asked` is what the encoder is
/// asked through it.
fn later_burst(key: bool, asked: f64) -> (Estimator, impl Fn(u64) -> Instant) {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    let at = move |ms: u64| start + Duration::from_millis(ms);
    est.encoder_kbps = Some(7000.0);
    est.apply(received(7000.0, 0.3), at(100), NO_QUIC);
    for ms in (200..4000).step_by(100) {
        est.apply(received(5000.0, 0.0), at(ms), NO_QUIC);
    }
    est.key_lost = Some(key);
    est.apply(received(4000.0, 0.3), at(4000), NO_QUIC);
    est.encoder_kbps = Some(asked);
    est.apply(received(5700.0, 0.0), at(4100), NO_QUIC);
    (est, at)
}

/// Seat B, hold round 4 (#1433 M1'): a held line just under a policer overflows it only with a
/// keyframe, a short burst at a constant rate that ends on its own, as Wi-Fi's does. A burst that
/// began on a keyframe never releases the hold; the same burst otherwise does.
#[test]
fn a_burst_that_began_on_a_keyframe_never_releases_the_hold() {
    assert!(
        later_burst(true, 7000.0).0.held_kbps().is_some(),
        "released"
    );
    assert_eq!(later_burst(false, 7000.0).0.held_kbps(), None);
}

/// Seat B, hold round 4 (#1433 N4): a burst that ended because the encoder was asked for less (a
/// rung step) may have ended because the sender dropped: it releases nothing.
#[test]
fn a_burst_that_ended_with_the_encoder_asked_less_never_releases_the_hold() {
    assert!(
        later_burst(false, 0.8 * 7000.0).0.held_kbps().is_some(),
        "released"
    );
}

/// Seat B, hold round 4 (#1433 M1'): the burst memory never blocks loss lasting `HOLD_STEP_GAP`.
/// A release it got wrong otherwise left a policer unheld for 60 s.
#[test]
fn the_burst_memory_never_blocks_loss_lasting_a_second() {
    let (mut est, at) = later_burst(false, 7000.0);
    assert!(est.random_burst.is_some());
    est.apply(received(7000.0, 0.3), at(4500), NO_QUIC);
    assert_eq!(est.held_kbps(), None, "a burst inside the memory");
    est.apply(received(7000.0, 0.3), at(5000), NO_QUIC);
    est.apply(received(7000.0, 0.3), at(5400), NO_QUIC);
    assert_eq!(est.held_kbps(), None, "under a second");
    est.apply(received(6000.0, 0.3), at(5500), NO_QUIC);
    assert_eq!(est.held_kbps(), Some(BETA * 6000.0));
}

/// A burst begins on a keyframe (`Run::key`) when its first loss is a keyframe datagram or one sent
/// `KEY_SPILL` or less after one; later, or with no keyframe, it does not.
#[test]
fn a_first_loss_on_or_just_after_a_keyframe_marks_the_report() {
    for (key, lost, want) in [
        (false, 30, false),
        (true, 12, true),
        (true, 30, true),
        (true, 100, false),
    ] {
        let start = Instant::now();
        let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
        for i in 0..120u32 {
            let mut d = vec![0u8; SIZE];
            d[1..5].copy_from_slice(&i.to_le_bytes());
            let t = start + Duration::from_millis(u64::from(i));
            log.record_handed(&d, t, t, false, key && (10..14).contains(&i));
        }
        let fb = report_nth(0, 0, 119, u64::from, |i| i != lost && i != lost + 5);
        est.measure(&fb, &mut log, start + Duration::from_millis(140));
        assert_eq!(est.key_lost, Some(want), "keyframe {key}, lost {lost}");
    }
}

/// A probe that meets loss (`LOSS_LOW` or more) found the link: the line backs off to `BETA` ×
/// what it delivered then, as from a burst.
#[test]
fn a_probe_that_meets_loss_backs_off_from_the_link() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    let at = |ms: u64| start + Duration::from_millis(ms);
    est.apply(received(4000.0, 0.25), at(100), NO_QUIC);
    for ms in (200..=70_000).step_by(100) {
        est.apply(received(4000.0, 0.0), at(ms), NO_QUIC);
    }
    assert!(est.held_kbps().unwrap() > 3400.0, "no probe");
    est.apply(received(3900.0, 0.03), at(70_100), NO_QUIC);
    assert_eq!(est.held_kbps(), Some(BETA * 3900.0));
}

/// The hold's other trigger: a second stretched reading in a row (a queue on the link, no loss)
/// sets the line at the lower reading, `BETA` × what that window delivered; one alone does not.
#[test]
fn a_second_stretched_reading_in_a_row_sets_the_hold() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    let at = |ms: u64| start + Duration::from_millis(ms);
    let stretched = |delivered: f64| Measured {
        delivered_kbps: Some(delivered),
        arrived_sent_kbps: Some(7000.0),
        probe_sent_kbps: 0.0,
        sent_kbps: Some(7000.0),
        ..received(delivered, 0.0)
    };
    est.apply(stretched(5000.0), at(100), NO_QUIC);
    assert_eq!(est.measured(), Some(BETA * 5000.0));
    assert_eq!(est.held_kbps(), None, "one reading");
    est.apply(received(7000.0, 0.0), at(150), NO_QUIC);
    est.apply(stretched(5000.0), at(200), NO_QUIC);
    assert_eq!(est.held_kbps(), None, "a break between them");
    est.apply(stretched(4800.0), at(250), NO_QUIC);
    assert_eq!(est.held_kbps(), Some(BETA * 4800.0));
    assert!(!est.drop_held(), "a queue, not a link that drops");
}

/// Seat A (hold round): PyroWave's step-down resumes the standard codec from its estimate with
/// no hold or half-confirmed reading left from before the switch.
#[test]
fn a_codec_switch_back_starts_with_no_hold() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    est.apply(
        received(4400.0, 0.25),
        start + Duration::from_millis(100),
        NO_QUIC,
    );
    est.read_last = true;
    est.resume_at(3000.0);
    assert_eq!((est.held_kbps(), est.read_last), (None, false));
}

/// Rule 2 counts only stretched arrivals: a window that lost 6 % at random, the rest arriving at
/// their send spacing, is no reading. Old code: delivered was read against every datagram sent,
/// lost ones too, so random loss over 5 % was a shortfall.
#[test]
fn random_loss_gives_no_reading() {
    let start = Instant::now();
    let (mut log, mut est) = (SendLog::new(start), Estimator::new(CEILING, start));
    for r in 0..20u64 {
        let first = r as u32 * BUSY.0 * BUSY.1;
        let from = r * u64::from(BUSY.0) * 16_667;
        let fb = frames_report(&mut log, start, first, from, BUSY, |seq, t| {
            (seq % 17 != 3).then_some(t + 11_000)
        });
        let now = read_at(start, &fb);
        let m = est.measure(&fb, &mut log, now);
        est.apply(m, now, NO_QUIC);
        assert!(m.loss.is_none_or(|l| l < LOSS_HIGH));
        assert_eq!(est.measured(), None, "report {r}: {m:?}");
    }
}

/// The lab session on an uncapped 50 Mbit/s link (Wi-Fi) with a 300 ms burst losing `loss` every
/// `every_s` from 10 s, for 240 s: (the deepest rung, the mean setpoint after 30 s, freezes, seconds
/// under 1080p, guard stalls).
fn wifi_bursts(
    sim: fn(bool) -> Sim,
    legacy: bool,
    every_s: f64,
    loss: f64,
) -> (usize, f64, usize, f64, usize) {
    let mut s = sim(legacy);
    let every = (every_s * 1e6) as u64;
    s.loss_at = Box::new(
        move |t| match t >= 10_000_000 && (t - 10_000_000) % every < 300_000 {
            true => loss,
            false => 0.0,
        },
    );
    s.run(240.0);
    let late: Vec<f64> = s
        .setpoints
        .iter()
        .filter(|&&(t, _)| t >= 30.0)
        .map(|&(_, sp)| f64::from(sp))
        .collect();
    let deepest = s.rungs.iter().map(|r| r.1).max().unwrap_or(0);
    let freezes = s.client.as_ref().unwrap().freezes().len();
    let mut under = s.rungs.clone();
    under.push((240.0, 0));
    let under = under
        .windows(2)
        .filter(|w| w[0].1 > 0)
        .map(|w| w[1].0 - w[0].0)
        .sum();
    (
        deepest,
        late.iter().sum::<f64>() / late.len() as f64,
        freezes,
        under,
        s.stalls.len(),
    )
}

/// Seat B's hold round finding (#1433): burst loss on a link the sender does not fill is not its
/// capacity, it does not fall with the sender's rate (Wi-Fi). Bursts every 2.5 or 4 s on an
/// uncapped link keep at least 0.9 × the setpoint 0.6.3 holds there, and under `Sim::lab` keep
/// 1080p. The ratchet took them to 360p (setpoint 462 at 4 s). Under `Sim::lab_hw` (seat B round 4,
/// S3) a burst after the hold that outlasts `LOSS_WINDOW` steps the line down, as a second cap step
/// must, and the release waits for the next burst. Lab V3: no guard stall in any row (the guard read
/// paced video: 17 stalls every 2.5 s), and bursts up to 10 s apart spend at most 10 s under 1080p
/// (the release memory expired 60 s on and the cycle restarted: 720p at +32, +74, +116 s).
#[test]
fn burst_loss_on_an_uncapped_link_keeps_the_rung_and_the_setpoint() {
    let mut bad = Vec::new();
    for (name, sim) in [
        ("lab", Sim::lab as fn(bool) -> Sim),
        ("lab_hw", Sim::lab_hw),
    ] {
        for (every, loss) in [
            (2.5, 0.3),
            (4.0, 0.3),
            (10.0, 0.3),
            (40.0, 0.3),
            (4.0, 0.15),
        ] {
            let (a_rung, a_sp, a_fr, _, a_st) = wifi_bursts(sim, true, every, loss);
            let (rung, sp, fr, under, st) = wifi_bursts(sim, false, every, loss);
            eprintln!(
                "{name} every {every} s at {loss}: legacy deepest rung {a_rung} setpoint {a_sp:.0} \
                 freezes {a_fr} stalls {a_st}; v2 deepest rung {rung} setpoint {sp:.0} freezes {fr} \
                 stalls {st}, {under:.1} s under 1080p"
            );
            let rung_kept = rung == 0 || name == "lab_hw";
            // Freezes: a burst that breaks a frame NACK cannot repair freezes legacy too. v2 also
            // paces the keyframe asked after it (sim: about half its extra freezes; lab check).
            let lossy = st > 0 || fr > 2 * a_fr + 1;
            if every <= 4.0 && (!rung_kept || lossy || sp < 0.9 * a_sp)
                || every <= 10.0 && (st > 0 || under > 10.0)
            {
                bad.push((name, every, loss, rung, fr, sp as u32, a_sp as u32));
            }
        }
    }
    assert!(
        bad.is_empty(),
        "(sim, every, loss, rung, freezes, setpoint, legacy's): {bad:?}"
    );
}

/// Seat A's hold round finding (#1433): a still screen at 400 kbit/s on a link with 2.5 % standing
/// random loss, one window losing over 10 % at 15 s, then a busy scene from 40 s. A quiet sender's
/// rate is not the link's: no line from it, and the setpoint and the rung are back by 60 s. Old
/// code: a line at 0.85 × 400 pinned the setpoint and stepped the rung down, and with no second
/// rate cluster to release it and the standing loss renewing it, the hold never ended.
#[test]
fn a_quiet_scene_sets_no_hold_that_outlives_it() {
    let run = |legacy: bool| {
        let mut s = Sim::lab(legacy);
        s.content_kbps = Box::new(|t| match t < 40_000_000 {
            true => 400.0,
            false => f64::MAX,
        });
        s.loss_at = Box::new(|t| match (15_000_000..15_300_000).contains(&t) {
            true => 0.4,
            false => 0.025,
        });
        s.run(70.0);
        s
    };
    let a = run(true);
    let s = run(false);
    let sp_at = |s: &Sim, t: f64| {
        s.setpoints
            .iter()
            .rev()
            .find(|x| x.0 <= t)
            .map_or(0, |x| x.1)
    };
    eprintln!(
        "legacy: setpoint at 60 s {}, rungs {:?}; v2: setpoint at 60 s {}, rungs {:?}, held at the \
         end {:?}",
        sp_at(&a, 60.0),
        a.rungs,
        sp_at(&s, 60.0),
        s.rungs,
        s.est.held_kbps()
    );
    assert!(
        f64::from(sp_at(&s, 60.0)) >= 0.9 * f64::from(sp_at(&a, 60.0)),
        "setpoint {} at 60 s, legacy {}",
        sp_at(&s, 60.0),
        sp_at(&a, 60.0)
    );
    assert_eq!(s.rung_at(60.0), 0, "rungs {:?}", s.rungs);
}

/// Seat A's hold round finding (#1433): the hold triggers on the window's loss, not on its excess
/// over the random-loss floor. Over a 4 % floor a policer dropping another 8 % loses 12 % a window
/// and sets the hold; read over the floor (8 %) it set nothing, so a 3-6 % floor raised the trigger
/// to 13-16 %. Random bursts are kept out by the per-burst release instead.
#[test]
fn a_policer_over_a_random_loss_floor_still_sets_the_hold() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    est.floor = 0.04;
    est.apply(
        received(6000.0, 0.12),
        start + Duration::from_millis(100),
        NO_QUIC,
    );
    assert_eq!(est.held_kbps(), Some(BETA * 6000.0));
}

/// R4 with the policer stepping down again under the hold at 90 s.
fn second_cap_step(sim: fn(bool) -> Sim, second_kbps: f64, content: f64) -> Sim {
    let mut s = sim(false);
    s.content_gain = Box::new(move |t| content * lab_content(t));
    s.run(30.0);
    policer(&mut s);
    (s.tokens, s.tokens_at) = (15_000.0, s.t);
    s.run(60.0);
    s.cap_kbps = second_kbps;
    s.run(120.0);
    s
}

/// Seat B, hold round 3 (#1433 M1a): a policer stepping down again under the hold (5000 → 2500 or
/// 3000 at 90 s) loses as much at the lower rate as a Wi-Fi burst does, but the loss lasts until
/// the sender drops: the hold stays, the line and the rung step down, and the client is never
/// left 2 s without a frame. Old code: released at 90.6 s, back to 1080p, 69.5 % loss after 95 s
/// and no frame from 90.6 to 156 s (2500); 60 % loss (3000). Round 4 (M1'): with the lab encoder's
/// keyframes, 3800 and 4000 put the line just under the policer, so only keyframes overflow it:
/// that burst ended on its own and released the hold at 90.8 s, then no frame for 61 s.
#[test]
fn a_second_cap_step_under_a_hold_keeps_it_and_steps_down() {
    // The most freezes each row had before the probe round (bdf82c12): a probe crossing a link
    // still shaped must cost none.
    let rows = [
        // 3: one more at the step once NACK answers went through the pacer (lab V3 sim facts).
        ("lab", Sim::lab as fn(bool) -> Sim, 2500.0, 3),
        ("lab", Sim::lab, 3000.0, 2),
        ("lab_hw", Sim::lab_hw, 2500.0, 2),
        ("lab_hw", Sim::lab_hw, 3000.0, 2),
        ("lab_hw", Sim::lab_hw, 3800.0, 4),
        ("lab_hw", Sim::lab_hw, 4000.0, 4),
        ("lab_hw", Sim::lab_hw, 4200.0, 3),
        ("lab_hw", Sim::lab_hw, 4500.0, 1),
    ];
    let mut bad = Vec::new();
    for (name, sim, second, most) in rows {
        let s = second_cap_step(sim, second, 1.0);
        let (freezes, rungs) = lab_summary(&format!("{name} 5000 → {second}"), &s);
        let longest = s.client.as_ref().unwrap().longest;
        let off: Vec<f64> = s
            .lines
            .iter()
            .filter(|&&(t, l)| (89.0..120.0).contains(&t) && l.is_none())
            .map(|&(t, _)| t)
            .take(3)
            .collect();
        eprintln!(
            "  longest gap {:.2} s ending {:.1} s; loss after 95 s {:.3}; rung at 105 s {}; \
             freezes {}; rungs {rungs:?}",
            longest.0 as f64 / 1e6,
            longest.1 as f64 / 1e6,
            s.loss_after(95.0),
            s.rung_at(105.0),
            freezes.len()
        );
        if !off.is_empty() {
            bad.push(format!("{name} {second}: hold off at {off:?}"));
        }
        if s.loss_after(95.0) > 0.05 && s.rung_at(105.0) < 4 {
            bad.push(format!(
                "{name} {second}: loss after 95 s {:.3}, rung {} at 105 s",
                s.loss_after(95.0),
                s.rung_at(105.0)
            ));
        }
        if freezes.len() > most {
            bad.push(format!(
                "{name} {second}: {} freezes, before {most}",
                freezes.len()
            ));
        }
        if longest.0 >= 2_000_000 {
            bad.push(format!(
                "{name} {second}: no frame for {:.2} s up to {:.1} s",
                longest.0 as f64 / 1e6,
                longest.1 as f64 / 1e6
            ));
        }
    }
    assert!(bad.is_empty(), "{bad:#?}");
}

/// Two stretched readings in a row at a 7000 send rate: a hold set by readings (a queue).
fn read_hold(est: &mut Estimator, at: impl Fn(u64) -> Instant) {
    let stretched = |delivered: f64| Measured {
        delivered_kbps: Some(delivered),
        arrived_sent_kbps: Some(7000.0),
        probe_sent_kbps: 0.0,
        sent_kbps: Some(7000.0),
        ..received(delivered, 0.0)
    };
    est.apply(stretched(5000.0), at(100), NO_QUIC);
    est.apply(stretched(4800.0), at(150), NO_QUIC);
    assert_eq!(est.held_kbps(), Some(BETA * 4800.0));
}

/// Seat B, hold round 3 (#1433 M1b): a hold set by readings lost nothing, so "as much loss at a
/// lower rate" is any loss: a later lossy window must not release it. Old code: one 15 % window
/// at 3000 released it and set the 60 s burst memory.
#[test]
fn a_hold_set_by_readings_is_not_released_by_a_later_burst() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    let at = |ms: u64| start + Duration::from_millis(ms);
    read_hold(&mut est, at);
    for ms in (200..3000).step_by(50) {
        est.apply(received(4000.0, 0.0), at(ms), NO_QUIC);
    }
    est.apply(received(3000.0, 0.15), at(3000), NO_QUIC);
    est.apply(received(3000.0, 0.0), at(3050), NO_QUIC);
    assert!(est.held_kbps().is_some(), "released");
    assert_eq!(est.random_burst, None, "burst memory set");
}

/// Seat B, hold round 3 (#1433 M1b): a probe on a hold set by readings that meets loss backs off
/// from the link (`met`). Old code: the release ran first and ended the hold.
#[test]
fn a_probe_on_a_hold_set_by_readings_backs_off_when_it_meets_loss() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    let at = |ms: u64| start + Duration::from_millis(ms);
    read_hold(&mut est, at);
    for ms in (200..=62_000).step_by(100) {
        est.apply(received(4000.0, 0.0), at(ms), NO_QUIC);
    }
    est.apply(received(3900.0, 0.03), at(62_100), NO_QUIC);
    assert_eq!(est.held_kbps(), Some(BETA * 3900.0));
}

/// Seat B, hold round 3 (#1433 S1): an encoder sending 0.6-0.7 × its setpoint into the R4
/// policer still overloads it: the sustained loss sets the hold and the sender steps down, and
/// it loses no more than legacy after onset + 20 s. Old code (0.7 ×): no hold, 1080p, 14.2 % loss
/// and a late freeze; legacy 540p, 6.4 %. The quiet scene (0.05 ×) is
/// `a_quiet_scene_sets_no_hold_that_outlives_it`.
#[test]
fn an_underfilling_encoder_on_a_policer_still_sets_the_hold() {
    for fill in [0.6, 0.7] {
        let run = |legacy: bool| {
            let mut s = Sim::lab(legacy);
            s.content_gain = Box::new(move |t| fill * lab_content(t));
            s.run(30.0);
            policer(&mut s);
            (s.tokens, s.tokens_at) = (15_000.0, s.t);
            s.run(90.0);
            s
        };
        let a = run(true);
        let s = run(false);
        lab_summary(&format!("{fill} x legacy"), &a);
        let (freezes, _) = lab_summary(&format!("{fill} x v2"), &s);
        let held = s.lines.iter().any(|&(t, l)| t > 30.0 && l.is_some());
        assert!(held, "{fill} x: no hold");
        assert!(
            s.loss_after(50.0) <= a.loss_after(50.0).max(0.02),
            "{fill} x: loss {:.3}, legacy {:.3}",
            s.loss_after(50.0),
            a.loss_after(50.0)
        );
        let late: Vec<_> = freezes.iter().filter(|&&t| t >= 60.0).collect();
        assert!(late.is_empty(), "{fill} x: freezes after 60 s {late:?}");
    }
}

/// A lab row with the lab encoder's keyframes and FEC (`Sim::lab_hw`): 30 s clean, `shape` for
/// 180 s, then clean again for `after` seconds.
fn lab_hw_row(legacy: bool, shape: impl Fn(&mut Sim), after: f64) -> Sim {
    let mut s = Sim::lab_hw(legacy);
    s.run(30.0);
    shape(&mut s);
    (s.tokens, s.tokens_at) = (15_000.0, s.t);
    s.run(180.0);
    s.clear();
    s.run(after);
    s
}

/// Seconds after the clear at 210 s that the rung is back at 1080p for good.
fn back_after_clear(s: &Sim) -> Option<f64> {
    match s.rungs.last() {
        Some(&(t, 0)) => Some((t - 210.0).max(0.0)),
        _ => None,
    }
}

/// The guard's stalls after onset + 15 s.
fn late_stalls(s: &Sim) -> Vec<f64> {
    s.stalls.iter().copied().filter(|&t| t >= 45.0).collect()
}

/// A hardware lab row's summary, printed with its recovery and stalls.
fn hw_summary(name: &str, s: &Sim) -> (Vec<f64>, Vec<(f64, usize)>) {
    let summary = lab_summary(name, s);
    eprintln!(
        "  back to 1080p {:?} s after the clear; stalls after onset + 15 s {:?}",
        back_after_clear(s),
        late_stalls(s)
    );
    summary
}

fn loss_between(s: &Sim, from: f64, to: f64) -> f64 {
    let (from, to) = ((from * 1e6) as u64, (to * 1e6) as u64);
    let sent = s.tx.iter().filter(|&&(t, _)| (from..to).contains(&t));
    sent.clone().filter(|&&(_, d)| d).count() as f64 / sent.count().max(1) as f64
}

/// R3 and R4 with the lab encoder (NVIDIA lab host, 2026-11-22): a keyframe a second at the
/// measured per-rung ratios and production's adaptive FEC. R3 reaches 540p within 15 s of the onset
/// (lab run 1: 720p until onset + 37 s, the FEC charge capped at 3 % by 1080p's floor), R4 reaches
/// 360p or loses at most 2 % while shaped, neither freezes over twice or steps the rung up while
/// shaped, the guard never stalls after onset + 15 s, and both are back at 1080p within 30 s of the
/// clear (probe round 3: three 5.2 s proving steps and the ladder's 4 s; with the video line probing
/// after a 60 s hold, 26 and 69 s). Legacy is printed.
#[test]
fn lab_rows_with_the_lab_encoders_keyframes_hold_and_recover() {
    for (name, shape) in [("R3", shallow as fn(&mut Sim)), ("R4", policer)] {
        hw_summary(
            &format!("{name} hw legacy"),
            &lab_hw_row(true, shape, 120.0),
        );
        let s = lab_hw_row(false, shape, 120.0);
        let (freezes, rungs) = hw_summary(&format!("{name} hw v2"), &s);
        let (back, stalls) = (back_after_clear(&s), late_stalls(&s));
        let shaped: Vec<_> = freezes.iter().filter(|&&t| t < 210.0).collect();
        assert!(shaped.len() <= 2, "{name}: freezes {freezes:?}");
        let ups: Vec<_> = ups_while_shaped(&rungs)
            .into_iter()
            .filter(|u| u.0 < 210.0)
            .collect();
        assert!(ups.is_empty(), "{name}: rung up while shaped {ups:?}");
        assert!(stalls.is_empty(), "{name}: stalls {stalls:?}");
        // 30 s: NACK answers paced and reported as production sends them (lab V3) put R3 at 26.7 s.
        assert!(back.is_some_and(|b| b <= 30.0), "{name}: back {back:?}");
        match name {
            "R3" => assert!(s.rung_at(45.0) >= 3, "R3: rung {} at 45 s", s.rung_at(45.0)),
            _ => assert!(
                s.rung_at(209.0) >= 4 || loss_between(&s, 50.0, 210.0) <= 0.02,
                "R4: rung {}, loss {:.3}",
                s.rung_at(209.0),
                loss_between(&s, 50.0, 210.0)
            ),
        }
    }
}

/// R3 and R4 over a random-loss floor (a Wi-Fi viewer behind the lab's shaper): 2 % and 4 % from
/// the start.
fn shallow_2(s: &mut Sim) {
    shallow(s);
    s.loss_at = Box::new(|_| 0.02);
}
fn policer_2(s: &mut Sim) {
    policer(s);
    s.loss_at = Box::new(|_| 0.02);
}
fn policer_4(s: &mut Sim) {
    policer(s);
    s.loss_at = Box::new(|_| 0.04);
}

/// Seat B, hold round 4 (#1433 M1'): R4 with the lab encoder over a 4 % random-loss floor freezes
/// at most 3 times while shaped. Old code: 42 freezes, the burst memory blocking every hold for
/// 60 s after a release. Seat B S2 (probe round): the 2 % rows freeze at most once while shaped
/// (R3 twice, lab V3 sim facts), and are back at 1080p within 35 s of the clear (the random loss
/// stays; R4 + 4 % within 60 s). bdf82c12: 7
/// (R3) and 4 (R4) freezes, each `loss-floor` end putting the pre-hold rate back; R4 + 4 % never
/// back.
#[test]
fn lab_rows_over_a_random_loss_floor_keep_the_hold() {
    for (name, shape) in [
        ("R3 + 2 %", shallow_2 as fn(&mut Sim)),
        ("R4 + 2 %", policer_2),
        ("R4 + 4 %", policer_4),
    ] {
        hw_summary(&format!("{name} legacy"), &lab_hw_row(true, shape, 120.0));
        let s = lab_hw_row(false, shape, 120.0);
        let (freezes, _) = hw_summary(&format!("{name} v2"), &s);
        let shaped: Vec<_> = freezes.iter().filter(|&&t| t < 210.0).collect();
        // R3 + 2 %: 2 since NACK answers are paced and reported (lab V3 sim facts): a
        // `burst-not-capacity` release 5 s before the clear, then a stale drop. Lab check.
        let most = match name {
            "R4 + 4 %" => 3,
            "R3 + 2 %" => 2,
            _ => 1,
        };
        assert!(
            shaped.len() <= most,
            "{name}: freezes while shaped {shaped:?}"
        );
        // A random loss that stops a 5.2 s proving step costs `PROBE_WAIT` and the step again.
        // R4 + 4 %: 60 s since NACK answers are paced and reported (lab V3 sim facts): 53.6 s,
        // legacy never back.
        let most_back = if name == "R4 + 4 %" { 60.0 } else { 35.0 };
        let back = back_after_clear(&s);
        assert!(
            back.is_some_and(|b| b <= most_back),
            "{name}: back {back:?}"
        );
    }
}

/// Seat B, probe rounds 2 and 3 (#1433 M1, M1'): a policer's bucket passes a probe step clean until
/// it drains, and a raise past the policer's rate then drains it under video and freezes. Under
/// `Sim::lab_hw`, a 5000 kbit/s policer with buckets to 2 MB (3.2 s at its rate), the same after 60 s
/// at 2500, and Wi-Fi bursts over a 1 MB bucket: no line over the policer 20 s after the onset or
/// the switch. Past the onset's cut, a plain policer freezes at most once and the switch twice, and
/// both are back at 1080p within 25 s of the clear. d463755b: 1 MB froze 14 times; 0bf22755: 1.25 MB
/// 12, the switch 11 at 750 kB. The Wi-Fi row's freezes follow its Wi-Fi releases (the hold ends and
/// the estimate climbs into the bucket), which no probe governs: printed.
#[test]
fn a_deep_bucket_policer_never_lets_the_probe_raise_the_line_over_it() {
    let mut failed = Vec::new();
    // (name, bucket, 2500 for 60 s first, Wi-Fi bursts, most freezes past the onset's cut)
    for (name, bytes, switch, wifi, most) in [
        ("15 kB", 15_000.0, false, false, Some(1)),
        ("150 kB", 150_000.0, false, false, Some(1)),
        ("500 kB", 500_000.0, false, false, Some(1)),
        ("750 kB", 750_000.0, false, false, Some(1)),
        ("1 MB", 1_000_000.0, false, false, Some(1)),
        ("1.25 MB", 1_250_000.0, false, false, Some(1)),
        ("1.5 MB", 1_500_000.0, false, false, Some(1)),
        ("2 MB", 2_000_000.0, false, false, Some(1)),
        ("2500 then 750 kB", 750_000.0, true, false, Some(2)),
        ("2500 then 1 MB", 1_000_000.0, true, false, Some(2)),
        ("2500 then 1.5 MB", 1_500_000.0, true, false, Some(2)),
        ("1 MB + Wi-Fi", 1_000_000.0, false, true, None),
    ] {
        let mut s = Sim::lab_hw(false);
        if wifi {
            // 40 % for 300 ms every 7 s from 10 s (`wifi_bursts`).
            s.loss_at =
                Box::new(
                    |t| match t >= 10_000_000 && (t - 10_000_000) % 7_000_000 < 300_000 {
                        true => 0.4,
                        false => 0.0,
                    },
                );
        }
        s.run(30.0);
        policer(&mut s);
        s.bucket_bytes = Some(bytes);
        (s.tokens, s.tokens_at) = (15_000.0, s.t);
        let caps: &[(f64, f64)] = match switch {
            true => {
                s.cap_kbps = 2500.0;
                s.run(60.0);
                s.cap_kbps = 5000.0;
                s.run(120.0);
                &[(30.0, 2500.0), (90.0, 5000.0)]
            }
            false => {
                s.run(180.0);
                &[(30.0, 5000.0)]
            }
        };
        s.clear();
        s.loss_at = Box::new(|_| 0.0);
        s.run(120.0);
        let (freezes, _) = hw_summary(&format!("R4 {name} v2"), &s);
        // The cut at the onset (three freezes to 2500, before any probe) is not the probe's.
        let shaped: Vec<_> = freezes
            .iter()
            .filter(|&&t| (35.0..210.0).contains(&t))
            .collect();
        let mut top = 0.0_f64;
        let mut over = Vec::new();
        for (i, &(from, cap)) in caps.iter().enumerate() {
            let to = caps.get(i + 1).map_or(210.0, |c| c.0);
            // A later, higher cap is watched from the switch: a line coming up from under the
            // earlier cap has no reason to pass it, so no settling time is owed there.
            let start = if i == 0 { from + 20.0 } else { from };
            for &(t, l) in &s.lines {
                if let Some(l) = l.filter(|_| (start..to).contains(&t)) {
                    top = top.max(l / cap);
                    if l > cap && over.len() < 5 {
                        over.push((t, l));
                    }
                }
            }
        }
        let back = back_after_clear(&s);
        eprintln!(
            "  loss while shaped {:.3}, top line 20 s after a cap {:.2} x it",
            loss_between(&s, 30.0, 210.0),
            top
        );
        let late = match most {
            Some(most) => shaped.len() > most || back.is_none_or(|b| b > 25.0),
            None => false,
        };
        if late || !over.is_empty() {
            failed.push(format!(
                "{name}: freezes while shaped {shaped:?}, lines over the policer {over:?}, back \
                 {back:?}"
            ));
        }
    }
    assert!(failed.is_empty(), "{failed:#?}");
}

/// Probe round (#1433): a hold that can probe sends repair datagrams at `PROBE_GAIN` × the line
/// `PROBE_WAIT` after the line stepped down. A step carried clean raises the line to `BETA`² × what
/// it carried and doubles again; a report that loses the probe's datagrams stops it at once and the
/// line stays. Probe round 3 (seat B M1'): every raise first sends `PROBE_BUCKET_S` of its landing
/// past it, 5.2 s at `PROBE_GAIN`.
#[test]
fn a_probe_raises_the_line_only_on_a_step_carried_clean() {
    let start = Instant::now();
    let mut est = Estimator::new(4.0 * CEILING, start);
    est.probes = true;
    let at = |ms: u64| start + Duration::from_millis(ms);
    est.apply(received(6000.0, 0.3), at(100), NO_QUIC);
    let mut line = BETA * 6000.0;
    for ms in (200..5100).step_by(50) {
        est.apply(received(5000.0, 0.0), at(ms), NO_QUIC);
        assert_eq!(est.probe_kbps(), None, "probing at {ms} ms");
    }
    est.apply(received(5000.0, 0.0), at(5100), NO_QUIC);
    let mut since = 5100;
    for step in 0..2 {
        assert_eq!(est.probe_kbps(), Some(PROBE_GAIN * line), "step {step}");
        let carried = Measured {
            sent_kbps: Some(line),
            probe_sent_kbps: line,
            ..received(2.0 * line, 0.0)
        };
        // Raised once (2 × line − landing) × t ≥ 2 s × landing: 5.207 s.
        for ms in (since + 50..=since + 5200).step_by(50) {
            est.probe_counted = (10, 0);
            est.apply(carried, at(ms), NO_QUIC);
            assert!(
                est.held_kbps() == Some(line),
                "step {step} raised at {ms} ms"
            );
        }
        since += 5250;
        est.probe_counted = (10, 0);
        est.apply(carried, at(since), NO_QUIC);
        line *= BETA * BETA * PROBE_GAIN;
        assert!(
            est.held_kbps().is_some_and(|l| (l - line).abs() < 1.0),
            "step {step}: line {:?}, want {line:.0}",
            est.held_kbps()
        );
    }
    est.probe_counted = (5, 10);
    est.apply(received(line, 0.0), at(since + 50), NO_QUIC);
    assert_eq!(
        est.probe_kbps(),
        None,
        "a probe losing its datagrams goes on"
    );
    assert!(est.held_kbps().is_some_and(|l| (l - line).abs() < 1.0));
}

/// Lab V3 (#1433): a probe that never gets its datagrams out (on hardware the session header kept
/// QUIC's queue from reading empty) must not hold the line for good. After `PROBE_STARVED` it is
/// starved, and the hold climbs as one with no FEC does, at `PROBE_PER_S` once `HOLD` has passed.
#[test]
fn a_starved_probe_falls_back_to_the_slow_climb() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    est.probes = true;
    let at = |ms: u64| start + Duration::from_millis(ms);
    est.apply(received(6000.0, 0.3), at(100), NO_QUIC);
    let line = est.held_kbps().expect("a hold");
    for ms in (150..=100_000).step_by(50) {
        est.apply(received(line, 0.0), at(ms), NO_QUIC);
    }
    assert!(
        est.held_kbps().is_none_or(|l| l > 1.5 * line),
        "line {:?} from {line:.0}",
        est.held_kbps()
    );
}

/// Probe round (#1433): the probe's datagrams are counted apart. Losing them is no video loss (the
/// hold, the loss bound and FEC read video's), and their bytes are out of the encoder's send rate.
#[test]
fn a_probes_lost_datagrams_are_not_video_loss() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    let mut log = SendLog::new(start);
    for i in 0..70u32 {
        let mut d = vec![0u8; SIZE];
        d[1..5].copy_from_slice(&i.to_le_bytes());
        let sent = start + Duration::from_millis(2 * u64::from(i));
        match (40..60).contains(&i) {
            true => log.record_probe(&d, sent),
            false => log.record(&d, sent, false),
        }
    }
    let fb = report_nth(0, 0, 69, |i| 2 * u64::from(i), |i| !(40..50).contains(&i));
    let m = est.measure(&fb, &mut log, start + Duration::from_millis(200));
    assert_eq!(m.loss, Some(0.0));
    assert_eq!(est.probe_counted, (10, 10));
    assert!(m.probe_sent_kbps > 0.0);
    let all = m.sent_kbps.unwrap() + m.probe_sent_kbps;
    assert!(
        (m.probe_sent_kbps / all - 20.0 / 70.0).abs() < 0.02,
        "probe {:.0} of {all:.0} kbps",
        m.probe_sent_kbps
    );
}

/// Probe round (#1433): the probe's bytes go in one burst right behind a frame's, never between
/// frames, at the probe's rate less what the frame sent.
#[test]
fn the_probe_fills_right_behind_a_frame_less_what_it_sent() {
    let start = Instant::now();
    let at = |ms: u64| start + Duration::from_millis(ms);
    let mut log = SendLog::new(start);
    assert_eq!(log.probe_room(Some(4800.0), at(0)), 0.0);
    send_nth(&mut log, start, 0, 0, 10);
    let room = log.probe_room(Some(4800.0), at(10));
    assert!(
        (room - (4800.0 * 125.0 * 0.010 - SIZE as f64)).abs() < 1.0,
        "room {room:.0}"
    );
    assert_eq!(log.probe_room(Some(4800.0), at(13)), 0.0, "between frames");
    assert_eq!(log.probe_room(None, at(14)), 0.0);
}

/// Probe round (#1433): a probe that met the link's edge (its own datagrams lost, video's not) keeps
/// a burst near it from releasing the hold as Wi-Fi (sim, second step to 3800 under the lab encoder:
/// the line just over the policer was released 2 s after a probe, back to 1080p).
#[test]
fn a_probe_at_the_links_edge_keeps_a_burst_near_it_from_releasing_the_hold() {
    let start = Instant::now();
    let mut est = Estimator::new(CEILING, start);
    est.probes = true;
    let at = |ms: u64| start + Duration::from_millis(ms);
    est.apply(received(7000.0, 0.3), at(100), NO_QUIC);
    for ms in (200..=5100).step_by(100) {
        est.apply(received(5000.0, 0.0), at(ms), NO_QUIC);
    }
    assert!(est.probe_kbps().is_some());
    est.probe_counted = (5, 10);
    est.apply(received(5000.0, 0.0), at(5150), NO_QUIC);
    assert_eq!(est.probe_kbps(), None);
    est.apply(received(4000.0, 0.3), at(6000), NO_QUIC);
    est.apply(received(5700.0, 0.0), at(6100), NO_QUIC);
    assert!(est.held_kbps().is_some(), "released");
}

/// Probe round (#1433, seat B S2): with repair-datagram probes the loss-floor exit keeps the hold's
/// line and raises the floor; it no longer puts the pre-hold rate back (a freeze each time on R4 +
/// 2 %).
#[test]
fn a_loss_floor_under_a_probing_hold_keeps_the_line() {
    let start = Instant::now();
    for probes in [false, true] {
        let mut est = Estimator::new(CEILING, start);
        est.probes = probes;
        est.apply(received(6000.0, 0.3), start, NO_QUIC);
        for rate in [400.0, 400.0, 400.0, 400.0, 300.0, 300.0, 300.0, 300.0] {
            est.floor_bin_closed(rate, 0.02);
        }
        assert_eq!(est.floor, 0.02);
        assert_eq!(est.held_kbps().is_some(), probes, "probes {probes}");
    }
}
