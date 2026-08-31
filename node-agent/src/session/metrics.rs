//! P4-03 — host-side per-session telemetry.
//!
//! Encode pad probes time each frame sink→src (FIFO-paired) and count frames
//! in/out + encoded bytes; drained once per heartbeat into a `session_metrics`
//! window (`agent-api.md`). Glass-to-glass is computed client-side from the
//! `abs-capture-time` RTP header extension, not here.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use crate::session::adaptation::{self, AdaptationSignals, AdaptationState};
use crate::session::echo::LiveEcho;

/// Bounds the encode-in FIFO so a never-emitted input cannot grow it without bound.
const PENDING_CAP: usize = 64;
const STAGE_SAMPLE_CAP: usize = 2048;
/// Compositor-emit ring depth (PTS → emit instant): 128 ≈ 2.1 s at 60 fps, far past
/// any real compositor→encoder transit, so an aged-out entry is a dropped frame, not
/// one in flight. Allocated only under `QUASAR_LATENCY_PROBE`.
const PROBE_RING_CAP: usize = 128;
/// Encoder→egress pairing queue depth. That hop is sub-frame, so 8 (≈133 ms at 60 fps)
/// is already generous; deeper only lengthens the interval over which a desynced
/// pairing reports a confident wrong number.
const PROBE_SEND_FIFO_CAP: usize = 8;
/// Max age (in frame periods) of an unpaired encoder-src entry before the pairing is
/// treated as desynced and the entry dropped.
///
/// Must stay 1.0. A depth bound alone only catches a runaway pairing; a persistent
/// off-by-N sits below the cap forever and inflates every sample by exactly N frame
/// periods. At 60 fps an off-by-one entry is ~20 ms old on pop vs ~3.4 ms (p95 ~5.7)
/// for a correct pair, so one frame period (16.7 ms) separates them and 1.5 (25 ms)
/// would not. Cost: a genuine enc→send above a frame period is dropped from S3;
/// acceptable only here, because `probe_pay_to_send_*` measures the same span keyed on
/// RTP timestamp (cannot desync) and still reports the untruncated tail.
const PROBE_SEND_MAX_AGE_FRAMES: f64 = 1.0;
/// Consecutive unmatched encoder-input PTS, after at least one match, that mean the
/// S1 correlation key has stopped surviving to the encoder mid-session.
const PROBE_UNMATCHED_STREAK_WARN: u64 = 120;

/// A bounded sample buffer for one window of latency samples.
///
/// Percentiles sort their input, so sample order carries no information — hence a flat
/// ring with a write cursor rather than a `Vec` + `remove(0)`, which would be an O(n)
/// memmove of up to [`STAGE_SAMPLE_CAP`] `f64`s on every push, on the streaming and RTP
/// pacer threads.
#[derive(Default)]
struct SampleRing {
    buf: Vec<f64>,
    cursor: usize,
}

impl SampleRing {
    fn push(&mut self, x: f64) {
        if self.buf.len() < STAGE_SAMPLE_CAP {
            self.buf.push(x);
        } else {
            self.buf[self.cursor] = x;
            self.cursor = (self.cursor + 1) % STAGE_SAMPLE_CAP;
        }
    }

    /// Take the window's samples, leaving the ring empty for the next window.
    fn take(&mut self) -> Vec<f64> {
        self.cursor = 0;
        std::mem::take(&mut self.buf)
    }
}

/// How long a pending FIFO entry may sit before it counts as a genuine encoder drop.
/// vah264enc/openh264enc expose no dropped-frame property, so timeout is the only
/// signal. 500 ms ≈ 30 frame periods at 60 fps.
const DROP_TIMEOUT: std::time::Duration = std::time::Duration::from_millis(500);

// ── Min/mean/percentile summary ────────────────────────────────────────────────

/// Mean/percentile summary of an encode-latency sample set.
#[derive(Debug, Clone, Copy)]
pub struct Summary {
    pub mean: f64,
    pub p50: f64,
    pub p95: f64,
    pub max: f64,
}

impl Summary {
    pub fn of(samples: &[f64]) -> Option<Summary> {
        if samples.is_empty() {
            return None;
        }
        let mut s = samples.to_vec();
        // total_cmp, never partial_cmp().unwrap() (#149): a NaN sample would panic
        // inside the heartbeat drain and take the agent down.
        s.sort_by(f64::total_cmp);
        let pct = |p: f64| {
            let idx = ((p * (s.len() - 1) as f64).round() as usize).min(s.len() - 1);
            s[idx]
        };
        Some(Summary {
            mean: s.iter().sum::<f64>() / s.len() as f64,
            p50: pct(0.50),
            p95: pct(0.95),
            max: s[s.len() - 1],
        })
    }
}

// ── A drained telemetry window ──────────────────────────────────────────────────

/// A non-destructive point read for the `burst_stats` capture
/// ([`SessionMetrics::snapshot_burst`]). Not a `MetricsWindow`: producing one of those
/// resets the state it summarizes. Raw and cumulative; the caller diffs two.
#[derive(Debug, Clone)]
pub struct BurstSnapshot {
    /// Caller-supplied so tests are deterministic.
    pub at: Instant,
    /// Lifetime counters, the same ones the drain diffs.
    pub frames_out: u64,
    pub bytes_out: u64,
    pub overflow_drops: u64,
    /// Instantaneous, not a delta: deepest since the last heartbeat drain reset it.
    pub queue_level_max: u64,
    pub abr_setpoint_kbps: Option<f64>,
    pub gcc_estimate_kbps: Option<f64>,
    /// Samples since the last heartbeat drain — a COPY; the originals stay for it.
    pub encode_ms: Vec<f64>,
    pub queue_dwell_ms: Vec<f64>,
}

/// Per-window telemetry. Built by [`SessionMetrics::drain_window`], serialized into a
/// `session_metrics` message (`agent-api.md`).
///
/// `Default` is test-only: a default window has `abr_mode: ""` / `adaptation_state: ""`,
/// values the drain can never produce.
#[derive(Debug, Clone)]
#[cfg_attr(test, derive(Default))]
pub struct MetricsWindow {
    pub window_ms: u64,
    pub fps: f64,
    pub bitrate_kbps: f64,
    /// Mean encode time over the window, or `None` if no frame was encoded.
    pub encode_ms: Option<f64>,
    pub encode_ms_p50: Option<f64>,
    pub encode_ms_p95: Option<f64>,
    /// Worst encode in the window (ms); absence is "not measured", the p50/p95 rule, not
    /// the echo's. The only key that sees a 200 ms hiccup: over a 5 s window at 60 fps
    /// one frame is the 99.7th percentile, so such a stall barely moves p95.
    pub encode_ms_max: Option<f64>,
    pub source_fps: f64,
    pub compositor_fps: f64,
    pub compositor_pts_delta_p50_ms: Option<f64>,
    pub compositor_pts_delta_p95_ms: Option<f64>,
    pub interpipe_queue_level_max: u64,
    pub interpipe_queue_dwell_p50_ms: Option<f64>,
    pub interpipe_queue_dwell_p95_ms: Option<f64>,
    pub interpipe_queue_drops: u64,
    pub rtp_fps: f64,
    pub rtp_bitrate_kbps: f64,
    pub frames_encoded: u64,
    pub frames_dropped: u64,
    /// AS-03: the ABR governor's current CBR setpoint (kbit/s); `None` when ABR is
    /// disabled/disarmed.
    pub abr_setpoint_kbps: Option<f64>,
    // ── The live echo ([`crate::session::echo`]) ────────────────────────────────
    // Each field below is `None` when what it describes is at its default; absence means
    // exactly that, never "unknown" or "zero". Rule and edges:
    // [`Reported`](crate::session::echo::Reported).
    /// The compositor's app-facing `wl_output` logical mode and the sole authoritative
    /// readback of the live render resolution. Default: the pinned stream size; moved
    /// only by an applied `session_display_update`.
    pub render_width: Option<i32>,
    pub render_height: Option<i32>,
    /// The compositor's `preferred_scale`. Default `1.0`.
    pub ui_scale: Option<f64>,
    /// The EXTERNAL (encoded) size: the frame size on the wire. Default: the launch
    /// size. Independent of `render_*`, the app-facing mode inside the compositor.
    pub stream_width: Option<i32>,
    pub stream_height: Option<i32>,
    /// Whether this session can change the external resolution live. The documented
    /// exception to the rule above: always `Some` in practice (both runner variants set
    /// it before the first drain), so `None` does mean unknown.
    pub external_resize_supported: Option<bool>,
    /// SPT-08 (D6): the ladder's encoder speed-bias rung. Default: rung 0.
    pub ladder_speed_bias: Option<i32>,
    /// The ladder's external-resolution rung index. Default: rung 0, the launch size.
    pub ladder_res_rung: Option<i32>,
    /// SPT-08 (D7): the rate the fps rung asks for. Default: the LAUNCH rate. Only
    /// 120 fps sessions can carry it.
    pub ladder_fps: Option<i32>,
    /// Who owns the external size, `"auto"` (the ladder) or `"pinned"` (a user/admin
    /// PATCH). Rides the size echo: at the launch size there is nothing to own.
    pub external_owner: Option<&'static str>,
    /// SPT-01: the raw rtpgccbwe estimate (kbit/s) BEFORE the governor's EWMA / deadband
    /// / step logic; its delta against `abr_setpoint_kbps` is the governor's smoothing
    /// contribution. `None` when ABR is disarmed or no estimate has arrived.
    pub gcc_estimate_kbps: Option<f64>,
    /// Amendment 5 (abr-ladder): the governor's live floor (kbit/s) while the ladder has
    /// it off the launch floor. Without it, a setpoint pinned at the floor is
    /// indistinguishable from a chosen one.
    pub abr_floor_kbps: Option<f64>,
    /// SPT-02: the active ABR mode. Always present, so the bundle reads it rather than
    /// deriving it from setpoint presence (wrong for `off`: rtpgccbwe stays attached, so
    /// setpoints can appear).
    pub abr_mode: &'static str,
    /// SPT-03: this window's classifier label ("healthy" | "network_congested" |
    /// "encoder_saturated" | "unknown"). Always present, signal-only: SPT-04 consumes it,
    /// it changes no ABR behaviour. `client_presentation_limited` is not among the
    /// values — it needs browser data the agent never sees (invariant #1) and is
    /// detected control-plane-side (classifier.go).
    pub adaptation_state: &'static str,

    // ── Host-stage latency probe (knob: QUASAR_LATENCY_PROBE) ──────────────────
    // All `None` unless the probe is on AND this window sampled that stage; absent is
    // "not measured", never zero. Design:
    // docs/superpowers/specs/2026-08-18-latency-probe-design.md.
    /// S1 — compositor src pad push → encoder sink pad, correlated on buffer PTS.
    /// Spans the interpipe boundary, the leaky queue and the GPU convert/scale stage:
    /// the largest single agent-side gap in the T1 budget.
    pub probe_capture_to_enc_in_p50_ms: Option<f64>,
    pub probe_capture_to_enc_in_p95_ms: Option<f64>,
    /// S3 — encoder src pad → that access unit's marker packet at the post-`rtpbin`
    /// egress seam (capsfilter + parser + payloader + `rtpbin`/TWCC + pacer). Closed at
    /// the egress, not the payloader src pad, because `rtph264pay` pushes buffer LISTS
    /// that a `PadProbeType::BUFFER` probe never sees — also why `rtp_fps` /
    /// `rtp_bitrate_kbps` on that pad have always been near-dead (2026-08-18: 0.2 fps /
    /// 0.5 kbps against a real 60 fps / 7000 kbps). Subtract `probe_pay_to_send_*` for
    /// parser + payloader alone.
    pub probe_enc_out_to_send_p50_ms: Option<f64>,
    pub probe_enc_out_to_send_p95_ms: Option<f64>,
    /// S4 — an access unit's first RTP packet at the payloader → its marker packet at
    /// the egress seam (pre-SRTP): RTP session, TWCC insertion, pacer.
    pub probe_pay_to_send_p50_ms: Option<f64>,
    pub probe_pay_to_send_p95_ms: Option<f64>,
    /// S0 — the compositor's internal hold: running time at src-pad push minus PTS.
    pub probe_pts_to_emit_p50_ms: Option<f64>,
    pub probe_pts_to_emit_p95_ms: Option<f64>,
    /// Realized wall-clock arrival cadence at the compositor src pad (p95). Unlike
    /// `compositor_pts_delta_p95_ms`, which is PTS-derived and reports the nominal
    /// interval (a flat 16.668 ms at 60 fps) however bursty delivery is.
    pub probe_compositor_frame_interval_p95_ms: Option<f64>,
    /// Encoder-src entries dropped without a pair (depth cap or age bound). Present
    /// whenever the probe is armed, including as `0`, which distinguishes "S3 sparse
    /// because the pairing desynced" from "sparse for some other reason".
    pub probe_send_desyncs: Option<f64>,
    /// Encoder-input buffers whose PTS the compositor ring did not hold; `0` means the
    /// S1 key is healthy.
    pub probe_pts_unmatched: Option<f64>,
}

// ── Live per-session state ──────────────────────────────────────────────────────

/// Shared, thread-safe per-session telemetry state. Lives behind an `Arc`: written by
/// the encoder pad probes (streaming threads), drained by the agent loop.
pub struct SessionMetrics {
    start: Instant,
    /// AS-03: the governor's CBR setpoint in kbit/s, 0 when ABR is disabled/disarmed.
    abr_setpoint_kbps: AtomicU64,
    /// Amendment 5 (abr-ladder): the governor's floor in kbit/s once the ladder has moved
    /// it off the launch floor, else 0 (⇒ the drain reports `None`).
    abr_floor_kbps: AtomicU64,
    /// SPT-01: the raw rtpgccbwe estimate in kbit/s as `f64::to_bits` (0.0 ⇒ never
    /// received), stored on `notify::estimated-bitrate` before the governor sees it.
    gcc_estimate_kbps_bits: AtomicU64,
    /// SPT-02: the active ABR mode, set once at session start. `&'static str`
    /// (`AbrMode::as_str()`) so a drain allocates nothing.
    abr_mode: &'static str,
    /// The live display / external-size / ladder echo ([`crate::session::echo`]). Feeds
    /// no counter, ring or classifier here, and owns its own lock (one mutex over the
    /// whole echo, so a drain can never mix a new width with an old height).
    display: LiveEcho,
    /// SPT-03: the frame rate the classifier should expect, seeded from the launch fps.
    /// Drives the per-frame encode budget (`1000 / target_fps`) so the saturation trip
    /// scales with the tier instead of assuming 60.
    ///
    /// Atomic, not a constructor constant, because the D7 fps rung moves it: the
    /// classifier's host-fps-steady guard trips `Unknown` below `target × 0.83`, so
    /// pinning this at the launch rate made every window after a 120 → 60 step classify
    /// `Unknown`, resetting both rungs' dwell counters — the ladder could engage but
    /// never recover (2026-08-16, stuck at 60 through a recovered 11500 kbps setpoint).
    target_fps: AtomicU64,
    /// SPT-03: the setpoint (kbps) at the end of the previous drain, for the
    /// same-window downshift flag. 0 ⇒ no prior setpoint. Drain-only access.
    last_setpoint_kbps: Mutex<u64>,
    /// SPT-03: classifier thresholds, resolved once at construction from
    /// `QUASAR_ADAPT_*`. An all-unset env is byte-identical to the old constants.
    classifier_cfg: adaptation::ClassifierConfig,
    /// Lifetime counters (the drain diffs these against its last snapshot).
    frames_out: AtomicU64,
    bytes_out: AtomicU64,
    source_commits: AtomicU64,
    compositor_frames: AtomicU64,
    rtp_frames: AtomicU64,
    rtp_bytes: AtomicU64,
    /// `encoder.stall` (#270): the last instant a raw frame entered / an encoded buffer
    /// left the encoder, in ms since [`Self::start`] **plus one** (0 ⇒ never). Two
    /// relaxed stores per frame on probes that already fire; no lock, no allocation.
    /// They answer "is anything coming out right now" for the 100 ms supervision tick;
    /// the FIFO drop-timeout is per-frame and only reports at drain.
    last_encode_in_ms: AtomicU64,
    last_encode_out_ms: AtomicU64,
    /// Lifetime count of FIFO entries discarded by the overflow resync in
    /// `record_encode_in` (SO-08). Genuine encoder drops, cleared before the drop-timeout
    /// scan sees them, so the drain folds them into `frames_dropped`.
    overflow_drops: AtomicU64,
    /// Encoder-input instants FIFO-paired with the next encoded buffer (b-frames=0 ⇒
    /// 1-in-1-out; hardware encoders often emit no PTS, so order beats a PTS key), plus
    /// this window's encode-latency samples — under ONE lock (SO-03) so the src probe
    /// takes a single acquisition per frame. The instant is both the pairing key
    /// (out − in = encode_ms) and the drop-timeout key `drain_window` evicts on.
    pending: Mutex<Pending>,
    stage: Mutex<StagePending>,
    /// AS-03: per-drain hook (~5 s heartbeat), the governor's stall-rescue tick.
    /// rtpgccbwe only notifies when its estimate CHANGES and GCC pins it constant under
    /// sustained loss and at `max-bitrate`, so a purely notification-driven governor
    /// stalls mid-descent (overdriving the pipe) or mid-recovery. Riding the heartbeat
    /// avoids a per-session thread and dies with this struct.
    on_drain: Mutex<Option<Box<dyn Fn() + Send + Sync>>>,
    /// SPT-04: post-classification hook, invoked at the END of `drain_window` with the
    /// window's [`AdaptationState`] and encode-time p95 (ms), from which the smooth
    /// governor derives its [`AdaptationHint`](crate::session::abr::AdaptationHint) for
    /// the next observe. Runs after classification (unlike `on_drain`) so it sees this
    /// window's label. Registered only in `Smooth` mode; dies with this struct.
    #[allow(clippy::type_complexity)]
    on_window: Mutex<Option<Box<dyn Fn(AdaptationState, Option<f64>) + Send + Sync>>>,
    /// Drain bookkeeping: the counter snapshot + instant of the last drain.
    last: Mutex<DrainSnapshot>,
}

/// Encode-pairing FIFO + the window's encode-latency samples, behind one lock.
#[derive(Default)]
struct Pending {
    queue: VecDeque<Instant>,
    samples: Vec<f64>,
}

#[derive(Default)]
struct StagePending {
    queue: VecDeque<Instant>,
    queue_dwell_ms: Vec<f64>,
    queue_level_max: u64,
    queue_drops: u64,
    last_pts_ns: Option<u64>,
    pts_delta_ms: Vec<f64>,
    // ── latency probe (inert unless QUASAR_LATENCY_PROBE is on) ────────────────
    /// (buffer PTS ns, emit instant) recorded at the compositor src pad, matched at the
    /// encoder sink pad. Keyed on PTS, not FIFO order: the leaky queue between the two
    /// may drop a frame, desyncing a positional pairing for the rest of the session.
    probe_capture_ring: VecDeque<(u64, Instant)>,
    /// Encoder-src instants awaiting their access unit's marker packet at the egress
    /// seam. Order-paired (b-frames=0 ⇒ 1-in-1-out), age-bounded on pop by
    /// [`PROBE_SEND_MAX_AGE_FRAMES`] so a persistent off-by-N cannot inflate samples.
    probe_enc_out_fifo: VecDeque<Instant>,
    probe_cap_to_enc_ms: SampleRing,
    probe_enc_out_to_send_ms: SampleRing,
    probe_pay_to_send_ms: SampleRing,
    probe_pts_to_emit_ms: SampleRing,
    probe_frame_interval_ms: SampleRing,
    probe_last_emit: Option<Instant>,
    /// Set by any probe recorder. Distinguishes "probe on, nothing to say" from "probe
    /// off", which is what makes the counters below a reason for a sparse stage.
    probe_active: bool,
    // ── diagnostic counters ────────────────────────────────────────────────────
    // `*_window` are per-window deltas, reset by the drain and published; the lifetime
    // totals drive the once-only warnings and are never published.
    /// Encoder-sink buffers whose PTS the emit ring did not hold. A re-stamping element
    /// upstream (a `videorate` in the scale stage) makes `probe_capture_to_enc_in_*`
    /// absent for a whole run; without this the operator has no idea why.
    probe_unmatched_window: u64,
    probe_unmatched_total: u64,
    probe_matched_total: u64,
    /// Unmatched buffers since the last match — catches the key going bad mid-session,
    /// which a "never matched at all" gate cannot see.
    probe_unmatched_streak: u64,
    probe_warned_unmatched: bool,
    /// Encoder-src entries dropped without a pair, by the depth cap or the age bound.
    /// Non-zero means S3 is sparse and says so, rather than reporting a desynced pairing.
    probe_send_desyncs_window: u64,
    probe_send_desyncs_total: u64,
    probe_warned_desync: bool,
}

impl StagePending {
    /// Count encoder-src entries dropped without a pair, and warn once. Both the depth
    /// cap and the age bound land here, so the counter means one thing: "S3 lost this
    /// many frames to a pairing that was not 1-in-1-out".
    fn note_send_desync(&mut self, dropped: u64) {
        self.probe_send_desyncs_window += dropped;
        self.probe_send_desyncs_total += dropped;
        if !self.probe_warned_desync && self.probe_send_desyncs_total >= 4 {
            self.probe_warned_desync = true;
            tracing::warn!(
                token = "probe-unpaired-encoder-src",
                "latency probe: dropped {} unpaired encoder-src entries — the egress seam is \
                 not closing one pair per encoded frame (a marker packet on an unexpected \
                 payload type, or an encoder buffer that never becomes an access unit). \
                 probe_enc_out_to_send_* will be sparse and probe_send_desyncs reports it; \
                 probe_pay_to_send_* is keyed on the RTP timestamp and is unaffected.",
                self.probe_send_desyncs_total
            );
        }
    }
}

struct DrainSnapshot {
    at: Instant,
    frames_out: u64,
    bytes_out: u64,
    source_commits: u64,
    compositor_frames: u64,
    rtp_frames: u64,
    rtp_bytes: u64,
    overflow_drops: u64,
}

impl SessionMetrics {
    /// `abr_mode` is the SPT-02/04 mode ("protective" | "off" | "smooth"), included in
    /// every drain window so the bundle surfaces the real mode. `target_fps` is the
    /// session's configured rate, scaling the SPT-03 classifier's per-frame budget.
    pub fn new(abr_mode: &'static str, target_fps: u32) -> Self {
        let now = Instant::now();
        Self {
            start: now,
            abr_setpoint_kbps: AtomicU64::new(0),
            abr_floor_kbps: AtomicU64::new(0),
            gcc_estimate_kbps_bits: AtomicU64::new(0),
            abr_mode,
            display: LiveEcho::default(),
            target_fps: AtomicU64::new(target_fps as u64),
            last_setpoint_kbps: Mutex::new(0),
            last_encode_in_ms: AtomicU64::new(0),
            last_encode_out_ms: AtomicU64::new(0),
            classifier_cfg: adaptation::ClassifierConfig::from_env(),
            frames_out: AtomicU64::new(0),
            bytes_out: AtomicU64::new(0),
            source_commits: AtomicU64::new(0),
            compositor_frames: AtomicU64::new(0),
            rtp_frames: AtomicU64::new(0),
            rtp_bytes: AtomicU64::new(0),
            overflow_drops: AtomicU64::new(0),
            pending: Mutex::new(Pending::default()),
            stage: Mutex::new(StagePending::default()),
            on_drain: Mutex::new(None),
            on_window: Mutex::new(None),
            last: Mutex::new(DrainSnapshot {
                at: now,
                frames_out: 0,
                bytes_out: 0,
                source_commits: 0,
                compositor_frames: 0,
                rtp_frames: 0,
                rtp_bytes: 0,
                overflow_drops: 0,
            }),
        }
    }

    /// Host monotonic milliseconds since session start.
    pub fn now_ms(&self) -> f64 {
        self.start.elapsed().as_secs_f64() * 1000.0
    }

    /// Host monotonic ms for an already-sampled instant (same epoch as `now_ms`), so a
    /// probe reads the clock once per frame instead of twice (SO-03).
    pub fn ms_at(&self, t: Instant) -> f64 {
        t.saturating_duration_since(self.start).as_secs_f64() * 1000.0
    }

    /// Milliseconds since [`Self::start`], **plus one**, so that 0 can mean "never".
    fn flow_stamp(&self, t: Instant) -> u64 {
        t.saturating_duration_since(self.start).as_millis() as u64 + 1
    }

    /// How long the encoder's input and output have each been silent, as of `now`.
    /// `None` ⇒ that side has never fired (pre-roll, or an encoder that has produced
    /// nothing). Read by the runner's supervision tick; two relaxed loads, no lock.
    pub fn encode_flow(&self, now: Instant) -> (Option<Duration>, Option<Duration>) {
        let since = |stamp: u64| {
            (stamp != 0).then(|| {
                let at = self.start + Duration::from_millis(stamp - 1);
                now.saturating_duration_since(at)
            })
        };
        (
            since(self.last_encode_in_ms.load(Ordering::Relaxed)),
            since(self.last_encode_out_ms.load(Ordering::Relaxed)),
        )
    }

    /// Record one raw frame entering the encoder. `t` is the single per-frame clock
    /// read (SO-03): it FIFO-pairs with the next encoded buffer and is the drop-timeout
    /// key `drain_window` scans.
    pub fn record_encode_in(&self, t: Instant) {
        self.last_encode_in_ms
            .store(self.flow_stamp(t), Ordering::Relaxed);
        let mut p = self.pending.lock().unwrap();
        // Overflow means 1-in-1-out broke. Dropping only the oldest would desync the
        // order-based pairing for the rest of the session (every later output pairs
        // against a newer input, silently skewing encode_ms), so clear the whole FIFO
        // to resync: a brief gap beats a permanently wrong number.
        if p.queue.len() >= PENDING_CAP {
            // Cleared inputs never produced output — count them as genuine drops
            // (SO-08). They vanish before the drop-timeout scan, so this is the only
            // place to do it.
            let cleared = p.queue.len() as u64;
            p.queue.clear();
            self.overflow_drops.fetch_add(cleared, Ordering::Relaxed);
        }
        p.queue.push_back(t);
    }

    /// Count one encoded buffer, add its bytes, and pair it FIFO with the oldest
    /// pending input for an encode-time sample. One lock for the pop + push.
    pub fn record_encode_out(&self, t_out: Instant, bytes: u64) {
        self.last_encode_out_ms
            .store(self.flow_stamp(t_out), Ordering::Relaxed);
        self.frames_out.fetch_add(1, Ordering::Relaxed);
        self.bytes_out.fetch_add(bytes, Ordering::Relaxed);
        let mut p = self.pending.lock().unwrap();
        if let Some(t_in) = p.queue.pop_front() {
            let ms = t_out.saturating_duration_since(t_in).as_secs_f64() * 1000.0;
            p.samples.push(ms);
        }
    }

    pub fn record_source_commits(&self, count: u64) {
        self.source_commits.fetch_add(count, Ordering::Relaxed);
    }

    pub fn record_compositor_frame(&self, pts_ns: Option<u64>) {
        self.compositor_frames.fetch_add(1, Ordering::Relaxed);
        let Some(pts) = pts_ns else { return };
        let mut s = self.stage();
        if let Some(last) = s.last_pts_ns {
            if pts >= last {
                s.pts_delta_ms.push((pts - last) as f64 / 1_000_000.0);
                if s.pts_delta_ms.len() > STAGE_SAMPLE_CAP {
                    s.pts_delta_ms.remove(0);
                }
            }
        }
        s.last_pts_ns = Some(pts);
    }

    pub fn record_queue_in(&self, now: Instant, level: u64) {
        let mut s = self.stage();
        s.queue.push_back(now);
        s.queue_level_max = s.queue_level_max.max(level);
        if s.queue.len() > PENDING_CAP {
            s.queue.pop_front();
            s.queue_drops += 1;
        }
    }

    pub fn record_queue_out(&self, now: Instant) {
        let mut s = self.stage();
        if let Some(start) = s.queue.pop_front() {
            s.queue_dwell_ms
                .push(now.saturating_duration_since(start).as_secs_f64() * 1000.0);
            if s.queue_dwell_ms.len() > STAGE_SAMPLE_CAP {
                s.queue_dwell_ms.remove(0);
            }
        }
    }

    pub fn record_queue_overrun(&self) {
        let mut s = self.stage();
        s.queue_drops += 1;
        s.queue.clear();
    }

    // ── Host-stage latency probe recorders (knob: QUASAR_LATENCY_PROBE) ────────
    //
    // Called only from inside an already-existing pad probe, and only when the knob is
    // on — the gate is at probe-attach time, so a probe-off session pays nothing.
    // Design: docs/superpowers/specs/2026-08-18-latency-probe-design.md.

    /// Compositor src pad: record the emit instant against the buffer's PTS (the S1 key
    /// matched at the encoder sink), the realized wall-clock frame interval, and — when
    /// the caller could read it — the compositor's own PTS→emit hold (S0).
    /// `running_now_ns` is `Element::current_running_time`, on the same timebase as the
    /// buffer PTS (shared clock + base time, #68).
    pub fn probe_record_compositor_emit(
        &self,
        pts_ns: Option<u64>,
        now: Instant,
        running_now_ns: Option<u64>,
    ) {
        let mut s = self.stage();
        s.probe_active = true;
        if let Some(prev) = s.probe_last_emit {
            let ms = now.saturating_duration_since(prev).as_secs_f64() * 1000.0;
            s.probe_frame_interval_ms.push(ms);
        }
        s.probe_last_emit = Some(now);
        if let Some(pts) = pts_ns {
            if let Some(run) = running_now_ns {
                // A PTS ahead of the running time (clock skew at pipeline start) would
                // underflow the subtraction.
                if run >= pts {
                    s.probe_pts_to_emit_ms
                        .push((run - pts) as f64 / 1_000_000.0);
                }
            }
            s.probe_capture_ring.push_back((pts, now));
            if s.probe_capture_ring.len() > PROBE_RING_CAP {
                s.probe_capture_ring.pop_front();
            }
        }
    }

    /// Forget the compositor-side probe state. Must be called on a launcher↔game swap:
    /// the outgoing compositor's ring entries can never match, and the first emit after
    /// the swap would otherwise fold the whole swap gap into
    /// `probe_compositor_frame_interval_p95_ms` as one frame interval.
    pub fn probe_reset_source(&self) {
        let mut s = self.stage();
        s.probe_last_emit = None;
        s.probe_capture_ring.clear();
    }

    /// Encoder sink pad: close the S1 pair for this buffer's PTS.
    ///
    /// Searches the ring for the exact PTS and drops only entries older than the match
    /// (frames the leaky queue dropped). On a miss the ring must be left untouched:
    /// popping the older prefix anyway let one out-of-order or re-stamped PTS destroy
    /// every pending entry behind it. An unheld PTS yields no sample, never a mispair.
    pub fn probe_record_encode_in(&self, pts_ns: Option<u64>, now: Instant) {
        let Some(pts) = pts_ns else { return };
        let mut s = self.stage();
        s.probe_active = true;
        // Index 0 in steady state; the scan only walks past frames dropped between the
        // compositor and the encoder, and the ring is bounded at PROBE_RING_CAP.
        let hit = s.probe_capture_ring.iter().position(|&(p, _)| p == pts);
        match hit {
            Some(i) => {
                let (_, t) = s.probe_capture_ring[i];
                s.probe_capture_ring.drain(..=i);
                s.probe_matched_total += 1;
                s.probe_unmatched_streak = 0;
                let ms = now.saturating_duration_since(t).as_secs_f64() * 1000.0;
                s.probe_cap_to_enc_ms.push(ms);
            }
            None => {
                s.probe_unmatched_window += 1;
                s.probe_unmatched_total += 1;
                s.probe_unmatched_streak += 1;
                // Two cases, one warning: the key never worked (a re-stamping element
                // upstream), or it worked and stopped (a mid-session scale-stage
                // rebuild), which a "never matched at all" gate cannot see.
                let never = s.probe_matched_total == 0
                    && s.probe_unmatched_total >= PROBE_UNMATCHED_STREAK_WARN;
                let stopped = s.probe_matched_total > 0
                    && s.probe_unmatched_streak >= PROBE_UNMATCHED_STREAK_WARN;
                if !s.probe_warned_unmatched && (never || stopped) {
                    s.probe_warned_unmatched = true;
                    let (total, streak, matched) = (
                        s.probe_unmatched_total,
                        s.probe_unmatched_streak,
                        s.probe_matched_total,
                    );
                    if never {
                        tracing::warn!(
                            token = "probe-capture-pts-never-matched",
                            "latency probe: {total} encoder-input buffers and NOT ONE matched a \
                             compositor PTS — an element between the compositor and the encoder \
                             is re-stamping buffers, so probe_capture_to_enc_in_* will stay \
                             absent. Every other probe stage is unaffected."
                        );
                    } else {
                        tracing::warn!(
                            token = "probe-capture-pts-stopped-matching",
                            "latency probe: the compositor→encoder PTS key STOPPED matching \
                             mid-session ({matched} matched, then {streak} consecutive misses) — \
                             an element between the compositor and the encoder began re-stamping \
                             buffers. probe_capture_to_enc_in_* is now sparse or absent; every \
                             other probe stage is unaffected."
                        );
                    }
                }
            }
        }
    }

    /// Encoder src pad: queue this encoded frame's exit instant for the S3 pair.
    ///
    /// Overflow must CLEAR the queue, never drop the oldest entry — the same resync
    /// discipline as [`Self::record_encode_in`] (SO-08). Dropping one entry per overflow
    /// pinned the queue at its cap whenever the consumer never fired, so every sample
    /// read `cap × frame_period`: a rock-steady 1051 ms that looked like a real
    /// measurement (2026-08-18). A gap in samples is a visible failure; a confidently
    /// wrong number is not.
    ///
    /// The depth cap alone is not sufficient — see [`PROBE_SEND_MAX_AGE_FRAMES`].
    pub fn probe_record_enc_out(&self, now: Instant) {
        let mut s = self.stage();
        s.probe_active = true;
        if s.probe_enc_out_fifo.len() >= PROBE_SEND_FIFO_CAP {
            let dropped = s.probe_enc_out_fifo.len() as u64;
            s.probe_enc_out_fifo.clear();
            s.note_send_desync(dropped);
        }
        s.probe_enc_out_fifo.push_back(now);
    }

    /// Post-`rtpbin` egress seam, marker packet: close S3 (encoder→send) and record S4
    /// (payloader→send) in ONE lock acquisition — this runs on the RTP egress/pacer
    /// thread per marker packet, so the send path must touch the mutex once per frame.
    /// `pay_to_send_ms` is a finished ms value (from the abs-capture-time NTP-64 for this
    /// RTP timestamp), skipped when a `CLOCK_REALTIME` step made it nonsense.
    pub fn probe_record_send(&self, now: Instant, pay_to_send_ms: f64) {
        let max_age = self.probe_send_max_age();
        let mut s = self.stage();
        s.probe_active = true;
        // Age-bound BEFORE pairing: the depth cap only catches a runaway pairing, while
        // a persistent off-by-N sits below the cap forever, inflating every sample.
        let mut stale = 0u64;
        while let Some(&t) = s.probe_enc_out_fifo.front() {
            if now.saturating_duration_since(t) > max_age {
                s.probe_enc_out_fifo.pop_front();
                stale += 1;
            } else {
                break;
            }
        }
        if stale > 0 {
            s.note_send_desync(stale);
        }
        if let Some(t) = s.probe_enc_out_fifo.pop_front() {
            let ms = now.saturating_duration_since(t).as_secs_f64() * 1000.0;
            s.probe_enc_out_to_send_ms.push(ms);
        }
        if pay_to_send_ms.is_finite() && pay_to_send_ms >= 0.0 {
            s.probe_pay_to_send_ms.push(pay_to_send_ms);
        }
    }

    /// Longest an unpaired encoder-src entry may sit before it counts as desynced.
    /// Scales with the session rate — see [`PROBE_SEND_MAX_AGE_FRAMES`].
    fn probe_send_max_age(&self) -> std::time::Duration {
        let fps = self.target_fps.load(Ordering::Relaxed).max(1) as f64;
        std::time::Duration::from_secs_f64(PROBE_SEND_MAX_AGE_FRAMES / fps)
    }

    /// The `stage` mutex, tolerating poison. The RTP egress/pacer thread takes this lock,
    /// so a panic elsewhere must not panic every later send-path probe call and kill the
    /// media path: only this window's probe data is at risk, and a torn window beats a
    /// dead stream.
    fn stage(&self) -> std::sync::MutexGuard<'_, StagePending> {
        self.stage.lock().unwrap_or_else(|e| e.into_inner())
    }

    pub fn record_rtp_out(&self, bytes: u64, marker: bool) {
        self.rtp_bytes.fetch_add(bytes, Ordering::Relaxed);
        if marker {
            self.rtp_frames.fetch_add(1, Ordering::Relaxed);
        }
    }

    /// AS-03: publish the governor's CBR setpoint (kbit/s) for the next drained window.
    /// A governor that never arms never calls this, so the setpoint stays 0 and the
    /// drain reports `None`.
    pub fn set_abr_setpoint(&self, kbps: u32) {
        self.abr_setpoint_kbps.store(kbps as u64, Ordering::Relaxed);
    }

    /// Amendment 5: publish the governor's floor when the ladder has moved it. `0` ⇒ at
    /// the launch floor, and the drain omits the key, so a run report reads
    /// `abr_floor_kbps` without needing to know the launch value.
    pub fn set_abr_floor(&self, kbps: u32) {
        self.abr_floor_kbps.store(kbps as u64, Ordering::Relaxed);
    }

    /// SPT-01: publish the raw `rtpgccbwe` estimate (kbit/s) BEFORE the governor acts
    /// on it. Called from the `notify::estimated-bitrate` fast path ahead of
    /// `observe()`; non-blocking, one relaxed store.
    pub fn set_gcc_estimate(&self, kbps: f64) {
        self.gcc_estimate_kbps_bits
            .store(kbps.to_bits(), Ordering::Relaxed);
    }

    /// AS-03: register the per-drain hook (see the `on_drain` field doc). Last
    /// registration wins; dropped with the session's `SessionMetrics`.
    pub fn set_on_drain(&self, hook: Box<dyn Fn() + Send + Sync>) {
        *self.on_drain.lock().unwrap() = Some(hook);
    }

    /// SPT-04: register the post-classification hook (see the `on_window` field doc).
    /// Last registration wins; dropped with the session's `SessionMetrics`.
    #[allow(clippy::type_complexity)]
    /// Drop both ABR hooks. The runner MUST call this at session teardown: the hooks hold
    /// strong element refs (encoder + rtpgccbwe, `abr_glue`) while the encoder's pad
    /// probes hold this struct's Arc — a cross ref cycle (encoder → probe → metrics →
    /// hook → encoder) that otherwise outlives the session and retains the encoder's
    /// VkDevice/DPB, the ~505 MiB/session VRAM leak
    /// (`.claude/rules/gstreamer-gotchas.md`). Clearing here breaks it deterministically
    /// on every teardown path.
    ///
    /// This is NOT the sole release path for the `Arc<EncodeResolutionLever>` that
    /// `on_window` also holds (`abr_glue::resolution_lever_with_echo`): the runner and
    /// the webrtcbin `request-aux-sender` closure hold it too, so it is pipeline-scoped.
    pub fn clear_hooks(&self) {
        let had_drain = self.on_drain.lock().unwrap().take().is_some();
        let had_window = self.on_window.lock().unwrap().take().is_some();
        tracing::debug!(had_drain, had_window, "session metrics ABR hooks cleared");
    }

    pub fn set_on_window(&self, hook: Box<dyn Fn(AdaptationState, Option<f64>) + Send + Sync>) {
        *self.on_window.lock().unwrap() = Some(hook);
    }

    /// Record the compositor's live app-facing render size / UI scale for the echo.
    /// `render = None` ⇒ the pinned stream size, so the fields are omitted. Called only
    /// after the compositor actually took the values; restoring the defaults stops the
    /// echo, which is how a consumer sees "back to the pinned stream size".
    pub fn set_display(&self, render: Option<(i32, i32)>, ui_scale: f64) {
        self.display.set_display(render, ui_scale);
    }

    /// Record the session's live EXTERNAL (encoded) size for the echo. `stream = None` ⇒
    /// the launch size, fields omitted. Separate from [`Self::set_display`]: the halves
    /// are written by different parts of the runner at different times (compositor
    /// properties taken vs encode caps negotiated), and neither may clobber the other.
    pub fn set_external(&self, stream: Option<(i32, i32)>) {
        self.display.set_external(stream);
    }

    /// Publish whether this session can be resized live. Called once at session start
    /// (both runner variants) before the first drain, which is what makes
    /// `external_resize_supported` always present.
    pub fn set_external_resize_supported(&self, supported: bool) {
        self.display.set_external_resize_supported(supported);
    }

    /// SPT-08 (D6): publish the ladder's speed-bias rung. Written by the ladder's
    /// `on_window` closure per actuated step; a snapshot, not a counter.
    pub fn set_ladder_bias(&self, bias: u8) {
        self.display.set_ladder_bias(bias);
    }

    /// Publish the ladder's resolution rung index (0 = launch).
    pub fn set_ladder_res_rung(&self, rung: usize) {
        self.display.set_ladder_res_rung(rung);
    }

    /// SPT-08 (D7): publish the fps rung's rate. `0` ⇒ back at the launch rate, this
    /// key's default per [`Reported`](crate::session::echo::Reported).
    pub fn set_ladder_fps(&self, fps: i32) {
        self.display.set_ladder_fps(fps);
    }

    /// SPT-03/D7: tell the classifier what frame rate to expect. Called by the ladder
    /// alongside `set_ladder_fps` on every step; see the `target_fps` field doc for why a
    /// fixed launch rate wedges the ladder. On an up-step the target moves before the
    /// pipeline realizes the rate, so a window or two classifies `Unknown` — the rung's
    /// `settle_windows` swallows exactly those, losing no dwell progress.
    pub fn set_target_fps(&self, fps: u32) {
        self.target_fps.store(fps.max(1) as u64, Ordering::Relaxed);
    }

    /// The frame rate the classifier currently expects (tests/telemetry).
    pub fn target_fps(&self) -> u32 {
        self.target_fps.load(Ordering::Relaxed) as u32
    }

    /// Publish who owns the external size. Written by the runner's manual
    /// `session_display_update` path; the ladder never sets it (auto is the default).
    pub fn set_external_owner_pinned(&self, pinned: bool) {
        self.display.set_external_owner_pinned(pinned);
    }

    /// A non-destructive read of the state [`Self::drain_window`] reports — the
    /// `session_capture` `burst_stats` seam (`session::capture`).
    ///
    /// It must reset nothing: no counter snapshot advanced, no sample vector taken, no
    /// `on_drain`/`on_window` hook fired. A burst samples at 100 ms alongside the
    /// heartbeat drain, and stealing that window's samples would corrupt the telemetry
    /// the operator is watching.
    ///
    /// The caller diffs successive snapshots: counters are lifetime-monotonic, and
    /// sample vectors grow monotonically between drains, so the suffix past the previous
    /// snapshot's length is that sub-window. A drain landing mid-burst shortens one;
    /// `capture::series` handles that instead of indexing off the end.
    ///
    /// Same lock discipline as the drain, including the poison-tolerant [`Self::stage`]
    /// accessor: a capture must never turn a poisoned mutex into a dead media path.
    pub fn snapshot_burst(&self, at: Instant) -> BurstSnapshot {
        let encode_ms = {
            let p = self.pending.lock().unwrap_or_else(|e| e.into_inner());
            p.samples.clone()
        };
        let (queue_dwell_ms, queue_level_max) = {
            let s = self.stage();
            (s.queue_dwell_ms.clone(), s.queue_level_max)
        };
        let gcc = f64::from_bits(self.gcc_estimate_kbps_bits.load(Ordering::Relaxed));
        BurstSnapshot {
            at,
            frames_out: self.frames_out.load(Ordering::Relaxed),
            bytes_out: self.bytes_out.load(Ordering::Relaxed),
            overflow_drops: self.overflow_drops.load(Ordering::Relaxed),
            queue_level_max,
            abr_setpoint_kbps: match self.abr_setpoint_kbps.load(Ordering::Relaxed) {
                0 => None,
                v => Some(v as f64),
            },
            gcc_estimate_kbps: (gcc > 0.0).then_some(gcc),
            encode_ms,
            queue_dwell_ms,
        }
    }

    /// Drain the window into a [`MetricsWindow`], resetting the per-window samples and
    /// advancing the counter snapshot. `now` is passed in so tests are deterministic.
    pub fn drain_window(&self, now: Instant) -> MetricsWindow {
        // Run the governor's stall-rescue tick FIRST, so any retarget it produces lands
        // its setpoint in this window's snapshot.
        if let Some(hook) = self.on_drain.lock().unwrap().as_ref() {
            hook();
        }

        let frames_out = self.frames_out.load(Ordering::Relaxed);
        let bytes_out = self.bytes_out.load(Ordering::Relaxed);
        let source_commits = self.source_commits.load(Ordering::Relaxed);
        let compositor_frames = self.compositor_frames.load(Ordering::Relaxed);
        let rtp_frames = self.rtp_frames.load(Ordering::Relaxed);
        let rtp_bytes = self.rtp_bytes.load(Ordering::Relaxed);
        let overflow = self.overflow_drops.load(Ordering::Relaxed);

        let mut last = self.last.lock().unwrap();
        let window_s = now.saturating_duration_since(last.at).as_secs_f64();
        let d_out = frames_out.saturating_sub(last.frames_out);
        let d_bytes = bytes_out.saturating_sub(last.bytes_out);
        let d_source = source_commits.saturating_sub(last.source_commits);
        let d_compositor = compositor_frames.saturating_sub(last.compositor_frames);
        let d_rtp_frames = rtp_frames.saturating_sub(last.rtp_frames);
        let d_rtp_bytes = rtp_bytes.saturating_sub(last.rtp_bytes);
        // Drops discarded by the FIFO overflow resync this window (SO-08).
        let d_overflow = overflow.saturating_sub(last.overflow_drops);
        *last = DrainSnapshot {
            at: now,
            frames_out,
            bytes_out,
            source_commits,
            compositor_frames,
            rtp_frames,
            rtp_bytes,
            overflow_drops: overflow,
        };
        drop(last);

        // Genuine encoder drops: entries older than DROP_TIMEOUT (500 ms). In-flight
        // frames stay in the FIFO and must NOT count as drops — that kills the ramp-up
        // false positive where d_in.saturating_sub(d_out) over-reported by the pipeline
        // depth. This scan is also why the FIFO cannot be a pure SPSC ring: the drain
        // inspects entries the consumer never popped. `Instant::now()`, not the caller's
        // `now`, so staleness is real elapsed time even when `now` is synthetic (tests).
        let real_now = std::time::Instant::now();
        let drop_cutoff = real_now.checked_sub(DROP_TIMEOUT).unwrap_or(real_now);
        let mut frames_dropped = 0u64;
        // One lock: take the window's samples and evict stale (dropped) entries.
        let window_samples: Vec<f64> = {
            let mut p = self.pending.lock().unwrap();
            let samples = std::mem::take(&mut p.samples);
            p.queue.retain(|t| {
                if *t <= drop_cutoff {
                    frames_dropped += 1;
                    false
                } else {
                    true
                }
            });
            samples
        };
        // Fold in the overflow-resync drops (SO-08): cleared before this scan could see
        // them, genuine drops all the same.
        frames_dropped += d_overflow;

        let fps = if window_s > 0.0 {
            d_out as f64 / window_s
        } else {
            0.0
        };
        let bitrate_kbps = if window_s > 0.0 {
            (d_bytes as f64 * 8.0) / 1000.0 / window_s
        } else {
            0.0
        };
        let summary = Summary::of(&window_samples);
        let encode_ms = summary.map(|s| s.mean);
        // Take the raw samples under the lock; sort them outside it. `Summary::of` sorts
        // a copy of every vector (five probe stages plus two always-on, up to
        // STAGE_SAMPLE_CAP each), and doing that while holding `stage` would block the
        // compositor, encoder and RTP egress/pacer threads for the whole drain.
        let (raw, queue_level_max, queue_drops, probe_counters) = {
            let mut s = self.stage();
            let raw = RawWindowSamples {
                pts_delta_ms: std::mem::take(&mut s.pts_delta_ms),
                queue_dwell_ms: std::mem::take(&mut s.queue_dwell_ms),
                cap_to_enc: s.probe_cap_to_enc_ms.take(),
                enc_out_to_send: s.probe_enc_out_to_send_ms.take(),
                pay_to_send: s.probe_pay_to_send_ms.take(),
                pts_to_emit: s.probe_pts_to_emit_ms.take(),
                frame_interval: s.probe_frame_interval_ms.take(),
            };
            let level = s.queue_level_max;
            let drops = s.queue_drops;
            let counters = ProbeCounters {
                active: s.probe_active,
                send_desyncs: s.probe_send_desyncs_window,
                pts_unmatched: s.probe_unmatched_window,
            };
            s.probe_send_desyncs_window = 0;
            s.probe_unmatched_window = 0;
            s.queue_level_max = s.queue.len() as u64;
            s.queue_drops = 0;
            (raw, level, drops, counters)
        };
        let pts_summary = Summary::of(&raw.pts_delta_ms);
        let dwell_summary = Summary::of(&raw.queue_dwell_ms);
        let probe = ProbeSummaries {
            cap_to_enc: Summary::of(&raw.cap_to_enc),
            enc_out_to_send: Summary::of(&raw.enc_out_to_send),
            pay_to_send: Summary::of(&raw.pay_to_send),
            pts_to_emit: Summary::of(&raw.pts_to_emit),
            frame_interval: Summary::of(&raw.frame_interval),
        };

        // 0 ⇒ ABR inactive ⇒ report None.
        let setpoint_raw = self.abr_setpoint_kbps.load(Ordering::Relaxed);
        let abr_setpoint_kbps = match setpoint_raw {
            0 => None,
            v => Some(v as f64),
        };

        // 0 ⇒ at the launch floor ⇒ None.
        let abr_floor_kbps = match self.abr_floor_kbps.load(Ordering::Relaxed) {
            0 => None,
            v => Some(v as f64),
        };

        // 0.0 bits ⇒ never received ⇒ None.
        let gcc_estimate_kbps = {
            let bits = self.gcc_estimate_kbps_bits.load(Ordering::Relaxed);
            let v = f64::from_bits(bits);
            if v > 0.0 {
                Some(v)
            } else {
                None
            }
        };

        // Same-window downshift (setpoint fell vs last drain), rolling the snapshot
        // forward. The drain is single-consumer (the agent loop), so this read-then-write
        // under one short lock cannot race another drain.
        let gcc_downshifted = {
            let mut last_sp = self.last_setpoint_kbps.lock().unwrap();
            let down = setpoint_raw != 0 && *last_sp != 0 && setpoint_raw < *last_sp;
            *last_sp = setpoint_raw;
            down
        };

        // Classify this window (signal-only, no ABR behaviour change). Uses encode_ms
        // p50/p95, not the mean: the tail is what stalls the frame clock.
        let encode_ms_p95 = summary.map(|s| s.p95);
        let adaptation = adaptation::classify_with(
            &AdaptationSignals {
                target_fps: self.target_fps.load(Ordering::Relaxed) as u32,
                fps,
                encode_ms_p50: summary.map(|s| s.p50),
                encode_ms_p95,
                frames_dropped,
                gcc_estimate_kbps,
                setpoint_kbps: abr_setpoint_kbps,
                bitrate_kbps,
                gcc_downshifted,
            },
            &self.classifier_cfg,
        );
        let adaptation_state = adaptation.as_str();

        // Feed the smooth governor its hint post-classification, so it sees THIS window's
        // label. No-op when no hook is registered (Protective/Off).
        if let Some(hook) = self.on_window.lock().unwrap().as_ref() {
            hook(adaptation, encode_ms_p95);
        }

        // The echo's absent-when-default rule lives on `crate::session::echo::Reported`
        // and is applied once, here.
        let echo = self.display.snapshot();

        MetricsWindow {
            window_ms: (window_s * 1000.0).round() as u64,
            fps,
            bitrate_kbps,
            encode_ms,
            encode_ms_p50: summary.map(|s| s.p50),
            encode_ms_p95: summary.map(|s| s.p95),
            encode_ms_max: summary.map(|s| s.max),
            source_fps: if window_s > 0.0 {
                d_source as f64 / window_s
            } else {
                0.0
            },
            compositor_fps: if window_s > 0.0 {
                d_compositor as f64 / window_s
            } else {
                0.0
            },
            compositor_pts_delta_p50_ms: pts_summary.map(|s| s.p50),
            compositor_pts_delta_p95_ms: pts_summary.map(|s| s.p95),
            interpipe_queue_level_max: queue_level_max,
            interpipe_queue_dwell_p50_ms: dwell_summary.map(|s| s.p50),
            interpipe_queue_dwell_p95_ms: dwell_summary.map(|s| s.p95),
            interpipe_queue_drops: queue_drops,
            rtp_fps: if window_s > 0.0 {
                d_rtp_frames as f64 / window_s
            } else {
                0.0
            },
            rtp_bitrate_kbps: if window_s > 0.0 {
                (d_rtp_bytes as f64 * 8.0) / 1000.0 / window_s
            } else {
                0.0
            },
            frames_encoded: d_out,
            frames_dropped,
            abr_setpoint_kbps,
            abr_floor_kbps,
            render_width: echo.render_width.reported(),
            render_height: echo.render_height.reported(),
            ui_scale: echo.ui_scale.reported(),
            stream_width: echo.stream_width.reported(),
            stream_height: echo.stream_height.reported(),
            external_resize_supported: echo.external_resize_supported.reported(),
            ladder_speed_bias: echo.ladder_speed_bias.reported(),
            ladder_res_rung: echo.ladder_res_rung.reported(),
            ladder_fps: echo.ladder_fps.reported(),
            external_owner: echo.external_owner.reported(),
            gcc_estimate_kbps,
            abr_mode: self.abr_mode,
            adaptation_state,
            probe_capture_to_enc_in_p50_ms: probe.cap_to_enc.map(|s| s.p50),
            probe_capture_to_enc_in_p95_ms: probe.cap_to_enc.map(|s| s.p95),
            probe_enc_out_to_send_p50_ms: probe.enc_out_to_send.map(|s| s.p50),
            probe_enc_out_to_send_p95_ms: probe.enc_out_to_send.map(|s| s.p95),
            probe_pay_to_send_p50_ms: probe.pay_to_send.map(|s| s.p50),
            probe_pay_to_send_p95_ms: probe.pay_to_send.map(|s| s.p95),
            probe_pts_to_emit_p50_ms: probe.pts_to_emit.map(|s| s.p50),
            probe_pts_to_emit_p95_ms: probe.pts_to_emit.map(|s| s.p95),
            probe_compositor_frame_interval_p95_ms: probe.frame_interval.map(|s| s.p95),
            probe_send_desyncs: probe_counters
                .active
                .then_some(probe_counters.send_desyncs as f64),
            probe_pts_unmatched: probe_counters
                .active
                .then_some(probe_counters.pts_unmatched as f64),
        }
    }
}

/// The latency probe's per-window summaries.
#[derive(Default)]
struct ProbeSummaries {
    cap_to_enc: Option<Summary>,
    enc_out_to_send: Option<Summary>,
    pay_to_send: Option<Summary>,
    pts_to_emit: Option<Summary>,
    frame_interval: Option<Summary>,
}

/// One window's raw samples, lifted out from under the `stage` lock so the
/// percentile sorts happen without blocking the streaming and pacer threads.
#[derive(Default)]
struct RawWindowSamples {
    pts_delta_ms: Vec<f64>,
    queue_dwell_ms: Vec<f64>,
    cap_to_enc: Vec<f64>,
    enc_out_to_send: Vec<f64>,
    pay_to_send: Vec<f64>,
    pts_to_emit: Vec<f64>,
    frame_interval: Vec<f64>,
}

/// The probe's per-window diagnostic counters — the machine-readable reason a stage
/// came back sparse or absent.
#[derive(Default)]
struct ProbeCounters {
    /// Whether any probe recorder ran this session. Gates emission: with the probe off
    /// the counters must stay absent (a `0` would claim "measured, none"); with it on a
    /// `0` is real information.
    active: bool,
    send_desyncs: u64,
    pts_unmatched: u64,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn summary_percentiles() {
        let data: Vec<f64> = (1..=100).map(|n| n as f64).collect();
        let s = Summary::of(&data).unwrap();
        assert_eq!(s.max, 100.0);
        assert!((s.mean - 50.5).abs() < 0.01);
        assert!((s.p50 - 50.0).abs() <= 1.0);
        assert!((s.p95 - 95.0).abs() <= 1.0);
    }

    // ── Host-stage latency probe (QUASAR_LATENCY_PROBE) ───────────────────────

    /// A probe-off session (the default) drains every `probe_*` key absent. A zero
    /// would be a lie the bench fold would average.
    #[test]
    fn probe_keys_are_absent_when_the_probe_never_records() {
        let s = SessionMetrics::new("off", 60);
        let at = s.last.lock().unwrap().at + Duration::from_secs(1);
        // Exercise the always-on recorders so the window is not empty for other reasons.
        s.record_compositor_frame(Some(0));
        s.record_encode_in(Instant::now());
        s.record_encode_out(Instant::now(), 1000);
        let w = s.drain_window(at);
        assert!(w.encode_ms_p50.is_some(), "sanity: the always-on path ran");
        assert_eq!(w.probe_capture_to_enc_in_p50_ms, None);
        assert_eq!(w.probe_capture_to_enc_in_p95_ms, None);
        assert_eq!(w.probe_enc_out_to_send_p50_ms, None);
        assert_eq!(w.probe_pay_to_send_p50_ms, None);
        assert_eq!(w.probe_pts_to_emit_p50_ms, None);
        assert_eq!(w.probe_compositor_frame_interval_p95_ms, None);
    }

    /// Keyed on buffer PTS, not FIFO order, so a frame the leaky queue drops cannot
    /// desync every later pair.
    #[test]
    fn capture_to_enc_pairs_on_pts_and_survives_a_dropped_frame() {
        let s = SessionMetrics::new("off", 60);
        let t0 = Instant::now();
        // Four frames, one per ms, PTS 1..4 ms.
        for i in 0..4u64 {
            s.probe_record_compositor_emit(
                Some((i + 1) * 1_000_000),
                t0 + Duration::from_millis(i),
                None,
            );
        }
        // The frame at PTS 2 ms is dropped between the compositor and the encoder.
        s.probe_record_encode_in(Some(1_000_000), t0 + Duration::from_millis(5));
        s.probe_record_encode_in(Some(3_000_000), t0 + Duration::from_millis(9));
        s.probe_record_encode_in(Some(4_000_000), t0 + Duration::from_millis(12));
        let at = s.last.lock().unwrap().at + Duration::from_secs(1);
        let w = s.drain_window(at);
        // 5, 7, 9 ms. A FIFO pairing would have scored the last two 8 and 10.
        assert_eq!(w.probe_capture_to_enc_in_p50_ms, Some(7.0));
        assert_eq!(w.probe_capture_to_enc_in_p95_ms, Some(9.0));
    }

    /// A PTS the ring never saw yields no sample rather than a wrong one (the probe
    /// armed mid-stream, or an upstream element re-stamping).
    #[test]
    fn capture_to_enc_never_mispairs_an_unknown_pts() {
        let s = SessionMetrics::new("off", 60);
        let t0 = Instant::now();
        s.probe_record_compositor_emit(Some(1_000_000), t0, None);
        s.probe_record_encode_in(Some(99_000_000), t0 + Duration::from_millis(5));
        let at = s.last.lock().unwrap().at + Duration::from_secs(1);
        assert_eq!(s.drain_window(at).probe_capture_to_enc_in_p50_ms, None);
    }

    #[test]
    fn capture_ring_is_bounded() {
        let s = SessionMetrics::new("off", 60);
        let t0 = Instant::now();
        for i in 0..(PROBE_RING_CAP as u64 * 4) {
            s.probe_record_compositor_emit(Some(i * 1_000_000), t0, None);
        }
        assert_eq!(s.stage().probe_capture_ring.len(), PROBE_RING_CAP);
    }

    /// b-frames=0 and nothing drops on that span, so the pairing is positional.
    #[test]
    fn enc_out_to_send_pairs_fifo() {
        let s = SessionMetrics::new("off", 60);
        let t0 = Instant::now();
        for i in 0..3u64 {
            s.probe_record_enc_out(t0 + Duration::from_millis(i * 16));
        }
        // Lags of 1, 3 and 5 ms, all inside the one-frame-period age bound.
        s.probe_record_send(t0 + Duration::from_millis(1), 0.5);
        s.probe_record_send(t0 + Duration::from_millis(19), 0.5);
        s.probe_record_send(t0 + Duration::from_millis(37), 0.5);
        let at = s.last.lock().unwrap().at + Duration::from_secs(1);
        let w = s.drain_window(at);
        assert_eq!(w.probe_enc_out_to_send_p50_ms, Some(3.0));
        assert_eq!(w.probe_enc_out_to_send_p95_ms, Some(5.0));
    }

    /// Regression for the 1051 ms confidently-wrong-number bug (2026-08-18). When S3's
    /// consumer at the egress seam never fires, the queue must CLEAR on overflow:
    /// dropping one entry per overflow pinned it at its cap and every sample read
    /// `cap × frame_period`. A stage reporting nothing is a visible failure; a stage
    /// reporting a plausible wrong number is not.
    #[test]
    fn enc_out_queue_clears_on_overflow_instead_of_pinning_at_its_cap() {
        let s = SessionMetrics::new("off", 60);
        let t0 = Instant::now();
        // A consumer that never fires: push far past the cap.
        let pushes = PROBE_SEND_FIFO_CAP as u64 * 10 + 3;
        for i in 0..pushes {
            s.probe_record_enc_out(t0 + Duration::from_millis(i * 16));
        }
        assert!(
            s.stage().probe_enc_out_fifo.len() <= PROBE_SEND_FIFO_CAP,
            "the queue grew past its cap"
        );
        assert!(
            s.stage().probe_send_desyncs_total > 0,
            "a desync must be counted, not silently absorbed"
        );
        // The consumer fires: the oldest survivor is at most `cap` frames old because
        // the queue resyncs. Drop-one-entry would have pinned it there forever.
        let last = t0 + Duration::from_millis((pushes - 1) * 16);
        s.probe_record_send(last + Duration::from_millis(2), 0.5);
        let at = s.last.lock().unwrap().at + Duration::from_secs(1);
        let got = s.drain_window(at).probe_enc_out_to_send_p50_ms.unwrap();
        assert!(
            got < (PROBE_SEND_FIFO_CAP as f64 * 16.0),
            "sample {got} ms is cap-sized — the pairing desynced rather than resyncing"
        );
    }

    /// The case the depth cap cannot catch: one encoder-src entry that never gets a
    /// marker packet (a config/header buffer, or a marker lost to a payload-type filter)
    /// leaves the pairing off by one at depth 1 forever — never overflows, never
    /// resyncs, never warns, every sample inflated by one frame period. The age bound
    /// closes it: the orphan is ~20 ms old when the first marker arrives, so it is
    /// evicted rather than paired.
    #[test]
    fn a_persistent_off_by_one_does_not_inflate_every_send_sample() {
        let s = SessionMetrics::new("off", 60);
        let t0 = Instant::now();
        // The orphan: an encoded buffer whose access unit never reaches the egress.
        s.probe_record_enc_out(t0);
        // Ten real frames at 17 ms, each sent 3 ms after leaving the encoder.
        for i in 1..=10u64 {
            s.probe_record_enc_out(t0 + Duration::from_millis(i * 17));
            s.probe_record_send(t0 + Duration::from_millis(i * 17 + 3), 2.0);
        }
        let at = s.last.lock().unwrap().at + Duration::from_secs(1);
        let w = s.drain_window(at);
        let p50 = w.probe_enc_out_to_send_p50_ms.expect("samples");
        assert!(
            (p50 - 3.0).abs() < 0.5,
            "p50 {p50} ms — a persistent off-by-one inflated every sample by a frame \
             period (would read ~20 ms)"
        );
        // And it is reported, not silently absorbed.
        assert_eq!(
            w.probe_send_desyncs,
            Some(1.0),
            "the evicted orphan must be counted so a sparse stage has a reason"
        );
    }

    /// Armed ⇒ the counters ride every window, including as `0`. Probe off ⇒ they must
    /// be absent, not zero.
    #[test]
    fn probe_counters_are_present_when_armed_and_absent_when_not() {
        let off = SessionMetrics::new("off", 60);
        let at = off.last.lock().unwrap().at + Duration::from_secs(1);
        let w = off.drain_window(at);
        assert_eq!(w.probe_send_desyncs, None);
        assert_eq!(w.probe_pts_unmatched, None);

        let on = SessionMetrics::new("off", 60);
        let t0 = Instant::now();
        on.probe_record_compositor_emit(Some(1_000_000), t0, None);
        on.probe_record_encode_in(Some(1_000_000), t0 + Duration::from_millis(1));
        let at = on.last.lock().unwrap().at + Duration::from_secs(1);
        let w = on.drain_window(at);
        assert_eq!(
            w.probe_send_desyncs,
            Some(0.0),
            "armed ⇒ a healthy 0, not absent"
        );
        assert_eq!(w.probe_pts_unmatched, Some(0.0));

        // An unmatched PTS shows up in the counter for its own window only.
        on.probe_record_encode_in(Some(77_000_000), t0 + Duration::from_millis(2));
        let at = on.last.lock().unwrap().at + Duration::from_secs(2);
        assert_eq!(on.drain_window(at).probe_pts_unmatched, Some(1.0));
        let at = on.last.lock().unwrap().at + Duration::from_secs(3);
        assert_eq!(on.drain_window(at).probe_pts_unmatched, Some(0.0));
    }

    /// A miss must not destroy the ring: popping every older entry behind an
    /// out-of-order or re-stamped PTS cost the pairing for all frames still in flight.
    #[test]
    fn an_unmatched_pts_leaves_the_capture_ring_intact() {
        let s = SessionMetrics::new("off", 60);
        let t0 = Instant::now();
        for i in 1..=3u64 {
            s.probe_record_compositor_emit(
                Some(i * 1_000_000),
                t0 + Duration::from_millis(i),
                None,
            );
        }
        // A PTS from nowhere must consume nothing.
        s.probe_record_encode_in(Some(999_000_000), t0 + Duration::from_millis(5));
        assert_eq!(
            s.stage().probe_capture_ring.len(),
            3,
            "an unmatched PTS drained entries that were still pairable"
        );
        // The real frames still pair, in order.
        s.probe_record_encode_in(Some(1_000_000), t0 + Duration::from_millis(6));
        s.probe_record_encode_in(Some(2_000_000), t0 + Duration::from_millis(8));
        let at = s.last.lock().unwrap().at + Duration::from_secs(1);
        let w = s.drain_window(at);
        assert_eq!(w.probe_capture_to_enc_in_p50_ms, Some(6.0));
    }

    /// The gap between the old compositor's last frame and the new one's first is not a
    /// frame interval and must not enter the p95.
    #[test]
    fn a_source_swap_does_not_report_the_swap_gap_as_a_frame_interval() {
        let s = SessionMetrics::new("off", 60);
        let t0 = Instant::now();
        s.probe_record_compositor_emit(Some(1_000_000), t0, None);
        s.probe_record_compositor_emit(Some(2_000_000), t0 + Duration::from_millis(16), None);
        // Swap: 4 s of dead air, then the replacement compositor starts.
        s.probe_reset_source();
        let t1 = t0 + Duration::from_secs(4);
        s.probe_record_compositor_emit(Some(3_000_000), t1, None);
        s.probe_record_compositor_emit(Some(4_000_000), t1 + Duration::from_millis(16), None);
        let at = s.last.lock().unwrap().at + Duration::from_secs(1);
        let got = s
            .drain_window(at)
            .probe_compositor_frame_interval_p95_ms
            .expect("intervals");
        assert!(
            got < 100.0,
            "frame-interval p95 is {got} ms — the swap gap was folded in as an interval"
        );
    }

    /// S4 arrives as a finished ms value from the NTP-64 egress delta, so a
    /// non-monotonic `CLOCK_REALTIME` step must not inject a negative or NaN sample.
    #[test]
    fn pay_to_send_rejects_nonsense_samples() {
        let s = SessionMetrics::new("off", 60);
        let t0 = Instant::now();
        s.probe_record_send(t0, -1.0);
        s.probe_record_send(t0, f64::NAN);
        let at = s.last.lock().unwrap().at + Duration::from_secs(1);
        assert_eq!(s.drain_window(at).probe_pay_to_send_p50_ms, None);
        s.probe_record_send(t0, 2.5);
        let at = s.last.lock().unwrap().at + Duration::from_secs(2);
        assert_eq!(s.drain_window(at).probe_pay_to_send_p50_ms, Some(2.5));
    }

    /// The drain clears every probe sample vector, so a window never re-reports the
    /// previous one's frames.
    #[test]
    fn probe_samples_do_not_leak_across_windows() {
        let s = SessionMetrics::new("off", 60);
        let t0 = Instant::now();
        s.probe_record_compositor_emit(Some(1_000_000), t0, Some(3_000_000));
        s.probe_record_compositor_emit(Some(2_000_000), t0 + Duration::from_millis(16), None);
        s.probe_record_encode_in(Some(1_000_000), t0 + Duration::from_millis(4));
        s.probe_record_enc_out(t0);
        s.probe_record_send(t0 + Duration::from_millis(1), 3.0);
        let base = s.last.lock().unwrap().at;
        let first = s.drain_window(base + Duration::from_secs(1));
        assert_eq!(first.probe_capture_to_enc_in_p50_ms, Some(4.0));
        assert_eq!(first.probe_pts_to_emit_p50_ms, Some(2.0));
        assert_eq!(first.probe_compositor_frame_interval_p95_ms, Some(16.0));
        assert_eq!(first.probe_pay_to_send_p50_ms, Some(3.0));
        let second = s.drain_window(base + Duration::from_secs(2));
        assert_eq!(second.probe_capture_to_enc_in_p50_ms, None);
        assert_eq!(second.probe_pts_to_emit_p50_ms, None);
        assert_eq!(second.probe_compositor_frame_interval_p95_ms, None);
        assert_eq!(second.probe_pay_to_send_p50_ms, None);
    }

    /// A PTS ahead of the element's running time (clock skew at pipeline start) must be
    /// skipped, never subtracted into an unsigned underflow.
    #[test]
    fn pts_to_emit_skips_a_pts_ahead_of_the_clock() {
        let s = SessionMetrics::new("off", 60);
        s.probe_record_compositor_emit(Some(9_000_000), Instant::now(), Some(1_000_000));
        let at = s.last.lock().unwrap().at + Duration::from_secs(1);
        assert_eq!(s.drain_window(at).probe_pts_to_emit_p50_ms, None);
    }

    // ── the metrics echo (agent-api.md `session_metrics`) ──────────────────────

    #[test]
    fn display_echo_reports_non_default_values_and_stops_at_the_defaults() {
        let s = SessionMetrics::new("off", 60);
        let at = |m: &SessionMetrics| m.last.lock().unwrap().at + Duration::from_secs(1);

        // Untouched session ⇒ nothing echoed (the fields are omitted on the wire).
        let w = s.drain_window(at(&s));
        assert_eq!(
            (w.render_width, w.render_height, w.ui_scale),
            (None, None, None)
        );

        // Echoed verbatim, and on every subsequent window (snapshot, not a counter).
        s.set_display(Some((1280, 720)), 1.5);
        for _ in 0..2 {
            let w = s.drain_window(at(&s));
            assert_eq!(w.render_width, Some(1280));
            assert_eq!(w.render_height, Some(720));
            assert_eq!(w.ui_scale, Some(1.5));
        }

        // Restoring the defaults stops the echo: that absence is how a consumer learns
        // the session is back at its pinned stream size / scale 1.0.
        s.set_display(None, 1.0);
        let w = s.drain_window(at(&s));
        assert_eq!(
            (w.render_width, w.render_height, w.ui_scale),
            (None, None, None)
        );

        // Scale-only and size-only each echo just their own field.
        s.set_display(None, 2.0);
        let w = s.drain_window(at(&s));
        assert_eq!((w.render_width, w.render_height), (None, None));
        assert_eq!(w.ui_scale, Some(2.0));

        s.set_display(Some((960, 540)), 1.0);
        let w = s.drain_window(at(&s));
        assert_eq!((w.render_width, w.render_height), (Some(960), Some(540)));
        assert_eq!(w.ui_scale, None);
    }

    // ── adaptive external resolution: the stream half of the same echo ──────────

    #[test]
    fn external_echo_reports_the_stream_size_only_when_off_the_launch_size() {
        let s = SessionMetrics::new("off", 60);
        let at = |m: &SessionMetrics| m.last.lock().unwrap().at + Duration::from_secs(1);

        // Before the runner reports the capability it is unknown, never a silent
        // `false` (a consumer would read that as "this host cannot resize").
        let w = s.drain_window(at(&s));
        assert_eq!(w.external_resize_supported, None);
        assert_eq!((w.stream_width, w.stream_height), (None, None));

        // Set once at session start ⇒ present on every window thereafter, including at
        // the launch size.
        s.set_external_resize_supported(true);
        for _ in 0..2 {
            let w = s.drain_window(at(&s));
            assert_eq!(w.external_resize_supported, Some(true));
            assert_eq!((w.stream_width, w.stream_height), (None, None));
        }

        // Stepped down ⇒ echoed, and it keeps being echoed.
        s.set_external(Some((1280, 720)));
        for _ in 0..2 {
            let w = s.drain_window(at(&s));
            assert_eq!((w.stream_width, w.stream_height), (Some(1280), Some(720)));
        }

        // Back at the launch size ⇒ the echo stops, which is how a consumer sees it.
        s.set_external(None);
        let w = s.drain_window(at(&s));
        assert_eq!((w.stream_width, w.stream_height), (None, None));
        assert_eq!(w.external_resize_supported, Some(true));
    }

    // The compositor half and the encode half must never clobber each other.
    #[test]
    fn render_and_stream_echoes_are_independent() {
        let s = SessionMetrics::new("off", 60);
        let at = |m: &SessionMetrics| m.last.lock().unwrap().at + Duration::from_secs(1);

        s.set_external_resize_supported(false);
        s.set_external(Some((1280, 720)));
        s.set_display(Some((960, 540)), 1.25);
        let w = s.drain_window(at(&s));
        assert_eq!((w.render_width, w.render_height), (Some(960), Some(540)));
        assert_eq!(w.ui_scale, Some(1.25));
        assert_eq!((w.stream_width, w.stream_height), (Some(1280), Some(720)));
        assert_eq!(w.external_resize_supported, Some(false));

        // Resetting the compositor half leaves the external size alone.
        s.set_display(None, 1.0);
        let w = s.drain_window(at(&s));
        assert_eq!((w.render_width, w.render_height), (None, None));
        assert_eq!((w.stream_width, w.stream_height), (Some(1280), Some(720)));

        // …and vice versa.
        s.set_display(Some((960, 540)), 1.0);
        s.set_external(None);
        let w = s.drain_window(at(&s));
        assert_eq!((w.render_width, w.render_height), (Some(960), Some(540)));
        assert_eq!((w.stream_width, w.stream_height), (None, None));
    }

    // ── SPT-08 D6: the ladder telemetry keys ────────────────────────────────────

    // Ladder telemetry is present-when-non-default, like stream_*.
    #[test]
    fn ladder_keys_are_absent_until_the_ladder_moves() {
        let s = SessionMetrics::new("smooth", 60);
        let w = s.drain_window(Instant::now());
        assert_eq!(w.ladder_speed_bias, None);
        assert_eq!(w.ladder_res_rung, None);
        assert_eq!(
            w.external_owner, None,
            "owner is only reported off the launch size"
        );
    }

    #[test]
    fn ladder_keys_report_once_the_ladder_moves() {
        let s = SessionMetrics::new("smooth", 60);
        s.set_ladder_bias(2);
        s.set_ladder_res_rung(1);
        s.set_external(Some((1920, 1080)));
        let w = s.drain_window(Instant::now());
        assert_eq!(w.ladder_speed_bias, Some(2));
        assert_eq!(w.ladder_res_rung, Some(1));
        assert_eq!(w.external_owner, Some("auto"), "ladder-owned by default");
        s.set_external_owner_pinned(true);
        let w = s.drain_window(Instant::now());
        assert_eq!(w.external_owner, Some("pinned"));
    }

    // Returning to the launch size drops the owner key with the size keys.
    #[test]
    fn external_owner_disappears_with_the_size_echo() {
        let s = SessionMetrics::new("smooth", 60);
        s.set_external(Some((1280, 720)));
        s.set_external_owner_pinned(true);
        assert_eq!(
            s.drain_window(Instant::now()).external_owner,
            Some("pinned")
        );
        s.set_external(None);
        let w = s.drain_window(Instant::now());
        assert_eq!(w.external_owner, None);
        assert_eq!((w.stream_width, w.stream_height), (None, None));
    }

    // D7 fps rung: same present-when-non-default convention, where "default" is the
    // LAUNCH rate (published as the 0 sentinel).
    #[test]
    fn ladder_fps_is_reported_only_below_the_launch_rate() {
        let s = SessionMetrics::new("smooth", 120);
        assert_eq!(s.drain_window(Instant::now()).ladder_fps, None);
        s.set_ladder_fps(60);
        assert_eq!(s.drain_window(Instant::now()).ladder_fps, Some(60));
        // Back at the launch rate ⇒ the 0 sentinel, and the key goes.
        s.set_ladder_fps(0);
        assert_eq!(s.drain_window(Instant::now()).ladder_fps, None);
    }

    // Regression (2026-08-16): the classifier's target fps must FOLLOW the fps rung.
    // Pinned at the launch rate, 60 realized against a 120 target trips the
    // host-fps-steady guard, every window classifies `Unknown`, and an `Unknown` window
    // resets both rungs' dwell counters — the ladder engages and can never recover.
    #[test]
    fn the_classifier_target_fps_follows_the_fps_rung() {
        let s = SessionMetrics::new("smooth", 120);
        assert_eq!(s.target_fps(), 120, "seeded from the launch rate");
        s.set_target_fps(60);
        assert_eq!(s.target_fps(), 60);
        // A 60 fps window is steady against a 60 target, not against 120.
        let steady = |target: u32, fps: f64| {
            adaptation::classify(&adaptation::AdaptationSignals {
                target_fps: target,
                fps,
                encode_ms_p50: Some(1.9),
                encode_ms_p95: Some(2.1),
                frames_dropped: 0,
                gcc_estimate_kbps: Some(11500.0),
                setpoint_kbps: Some(11500.0),
                bitrate_kbps: 9000.0,
                gcc_downshifted: false,
            })
        };
        assert_eq!(steady(120, 60.0), adaptation::AdaptationState::Unknown);
        assert_eq!(steady(60, 60.0), adaptation::AdaptationState::Healthy);
        // 0 is never a legal target (it would divide the frame budget by zero).
        s.set_target_fps(0);
        assert_eq!(s.target_fps(), 1);
    }

    // Bias 0 is the baseline and is not reported.
    #[test]
    fn a_zero_bias_is_not_reported() {
        let s = SessionMetrics::new("smooth", 60);
        s.set_ladder_bias(0);
        s.set_ladder_res_rung(0);
        assert_eq!(s.drain_window(Instant::now()).ladder_speed_bias, None);
        assert_eq!(s.drain_window(Instant::now()).ladder_res_rung, None);
    }

    #[test]
    fn drain_window_computes_fps_bitrate_drops() {
        let s = SessionMetrics::new("protective", 60);
        let base = Instant::now();
        // 6 frames in, 5 out: the 6th is still in the encoder (in-flight, not a drop).
        for _ in 0..6 {
            s.record_encode_in(base);
        }
        for _ in 0..5 {
            s.record_encode_out(base + Duration::from_millis(5), 2000);
        }
        // Drain over a synthetic 1 s window.
        let now = s.last.lock().unwrap().at + Duration::from_secs(1);
        let w = s.drain_window(now);
        assert_eq!(w.frames_encoded, 5);
        // The 6th entry is recent (< DROP_TIMEOUT) — in-flight, not a genuine drop.
        assert_eq!(w.frames_dropped, 0);
        assert!((w.fps - 5.0).abs() < 0.01);
        // 5 * 2000 bytes * 8 / 1000 / 1 s = 80 kbps.
        assert!((w.bitrate_kbps - 80.0).abs() < 0.01);
        // encode_ms mean = 5.0 (each out paired with an in at 0.0).
        assert!((w.encode_ms.unwrap() - 5.0).abs() < 0.01);
    }

    /// The max rides the same window as the mean and percentiles — one `Summary` over
    /// one drained sample set, not a high-water mark that survives into the next window.
    #[test]
    fn drain_window_publishes_the_worst_encode_not_only_the_percentiles() {
        let s = SessionMetrics::new("protective", 60);
        let base = Instant::now();
        // Nineteen ordinary frames and one 200 ms stall: the shape p95 hides.
        for _ in 0..20 {
            s.record_encode_in(base);
        }
        for _ in 0..19 {
            s.record_encode_out(base + Duration::from_millis(4), 2000);
        }
        s.record_encode_out(base + Duration::from_millis(200), 2000);

        let now = s.last.lock().unwrap().at + Duration::from_secs(1);
        let w = s.drain_window(now);
        assert!((w.encode_ms_p50.unwrap() - 4.0).abs() < 0.5);
        // p95 of 20 samples is index 19 here, so compare p50 against max: the stall is
        // invisible in the body and unmissable here.
        assert!(w.encode_ms_max.unwrap() >= 199.0);

        // Drained with the window, so the next one starts clean.
        let now = s.last.lock().unwrap().at + Duration::from_secs(1);
        assert_eq!(s.drain_window(now).encode_ms_max, None);
    }

    #[test]
    fn drain_window_reports_correlated_stage_budget() {
        let s = SessionMetrics::new("protective", 60);
        let base = Instant::now();
        s.record_source_commits(2);
        s.record_compositor_frame(Some(0));
        s.record_compositor_frame(Some(16_666_667));
        s.record_queue_in(base, 1);
        s.record_queue_out(base + Duration::from_millis(2));
        s.record_queue_overrun();
        s.record_rtp_out(1200, false);
        s.record_rtp_out(800, true);

        let now = s.last.lock().unwrap().at + Duration::from_secs(1);
        let w = s.drain_window(now);
        assert!((w.source_fps - 2.0).abs() < 0.01);
        assert!((w.compositor_fps - 2.0).abs() < 0.01);
        assert!((w.compositor_pts_delta_p50_ms.unwrap() - 16.666667).abs() < 0.001);
        assert_eq!(w.interpipe_queue_level_max, 1);
        assert!((w.interpipe_queue_dwell_p50_ms.unwrap() - 2.0).abs() < 0.01);
        assert_eq!(w.interpipe_queue_drops, 1);
        assert!((w.rtp_fps - 1.0).abs() < 0.01);
        assert!((w.rtp_bitrate_kbps - 16.0).abs() < 0.01);
    }

    #[test]
    fn drain_window_resets_between_windows() {
        let s = SessionMetrics::new("protective", 60);
        let base = Instant::now();
        for _ in 0..3 {
            s.record_encode_in(base);
            s.record_encode_out(base + Duration::from_millis(4), 1000);
        }
        let t0 = s.last.lock().unwrap().at;
        let w1 = s.drain_window(t0 + Duration::from_secs(1));
        assert_eq!(w1.frames_encoded, 3);
        // Second drain with no new frames ⇒ a zeroed window, no encode_ms.
        let w2 = s.drain_window(t0 + Duration::from_secs(2));
        assert_eq!(w2.frames_encoded, 0);
        assert_eq!(w2.frames_dropped, 0);
        assert!(w2.encode_ms.is_none());
    }

    #[test]
    fn fifo_stays_bounded_and_resyncs_on_overflow() {
        let s = SessionMetrics::new("protective", 60);
        let base = Instant::now();
        // Flood the FIFO with inputs the encoder never drained (overflow).
        for _ in 0..(PENDING_CAP * 3) {
            s.record_encode_in(base);
        }
        // Cleared on overflow rather than accumulating without limit.
        assert!(s.pending.lock().unwrap().queue.len() <= PENDING_CAP);
        // Drain the residual inputs + the skewed first window and discard it.
        let residual = s.pending.lock().unwrap().queue.len();
        for _ in 0..residual {
            s.record_encode_out(base + Duration::from_millis(2), 100);
        }
        let t0 = s.last.lock().unwrap().at;
        let _ = s.drain_window(t0 + Duration::from_secs(1));
        // Post-resync the FIFO is empty, so a fresh in/out pairs cleanly: 3 ms apart.
        let base2 = Instant::now();
        s.record_encode_in(base2);
        s.record_encode_out(base2 + Duration::from_millis(3), 100);
        let w = s.drain_window(t0 + Duration::from_secs(2));
        assert!((w.encode_ms.unwrap() - 3.0).abs() < 0.01);
    }

    /// A recent FIFO entry (< DROP_TIMEOUT) is in-flight and must NOT count as a drop;
    /// a stale one (> DROP_TIMEOUT, no matching encode_out) must. Both look identical in
    /// the observable counters (1 in, 0 out).
    #[test]
    fn frames_dropped_genuine_drop_vs_inflight() {
        let s = SessionMetrics::new("protective", 60);
        let t0 = s.last.lock().unwrap().at;

        // A — in-flight: entered the encoder just now, no output yet.
        s.record_encode_in(Instant::now());
        let w1 = s.drain_window(t0 + Duration::from_millis(10));
        assert_eq!(
            w1.frames_dropped, 0,
            "recent in-flight frame must not be counted as dropped"
        );
        assert_eq!(w1.frames_encoded, 0);

        // B — genuine drop: entered 2 s ago (past DROP_TIMEOUT = 500 ms), no output, so
        // it is evicted from the FIFO and counted.
        let stale_t = Instant::now() - Duration::from_secs(2);
        s.record_encode_in(stale_t);
        let w2 = s.drain_window(t0 + Duration::from_millis(20));
        assert_eq!(
            w2.frames_dropped, 1,
            "stale FIFO entry (2 s, > DROP_TIMEOUT) must be classified as a genuine drop"
        );
        assert_eq!(w2.frames_encoded, 0);

        // A's still-recent entry remains in the FIFO.
        assert_eq!(s.pending.lock().unwrap().queue.len(), 1);
    }

    /// SO-08: inputs discarded by the FIFO overflow resync are genuine drops and
    /// must be counted, not silently swallowed (they vanish before the timeout scan).
    #[test]
    fn overflow_resync_counts_cleared_as_drops() {
        let s = SessionMetrics::new("protective", 60);
        let base = Instant::now();
        // Flood past PENDING_CAP so the overflow-clear fires. The entries are recent, so
        // the timeout scan contributes 0: every counted drop comes from the overflow.
        for _ in 0..(PENDING_CAP * 2 + 1) {
            s.record_encode_in(base);
        }
        let t0 = s.last.lock().unwrap().at;
        let w = s.drain_window(t0 + Duration::from_secs(1));
        assert!(
            w.frames_dropped >= PENDING_CAP as u64,
            "overflow-cleared inputs must count as drops, got {}",
            w.frames_dropped
        );
        // A second drain with no new overflow reports no further overflow drops.
        let w2 = s.drain_window(t0 + Duration::from_secs(2));
        assert_eq!(w2.frames_dropped, 0);
    }

    #[test]
    fn encode_in_out_pairs_fifo() {
        let s = SessionMetrics::new("protective", 60);
        let base = Instant::now();
        s.record_encode_in(base);
        s.record_encode_in(base);
        s.record_encode_out(base + Duration::from_millis(3), 100); // pairs in #1 → 3.0
        s.record_encode_out(base + Duration::from_millis(4), 100); // pairs in #2 → 4.0
        let now = s.last.lock().unwrap().at + Duration::from_secs(1);
        let w = s.drain_window(now);
        // mean of {3.0, 4.0} = 3.5
        assert!((w.encode_ms.unwrap() - 3.5).abs() < 0.01);
    }
}
