//! AS-03 — in-session adaptive bitrate (ABR) governor.
//!
//! Closes the congestion-control loop entirely **server-side** (no protocol
//! surface, per AS-00 §Component 3):
//!
//! ```text
//! webrtcbin "request-aux-sender" ─► rtpgccbwe (per send RTP session)
//! rtpgccbwe notify::estimated-bitrate ─► Governor ─► encoder bitrate + cpb retarget
//! ```
//!
//! This module is the **pure** half — the EWMA + asymmetric-hysteresis decision
//! logic, unit-tested without GStreamer. The gst glue lives in [`super::pipeline`].
//!
//! The asymmetry is the **#68 lesson**: a delay-based estimator reading a clean LAN as
//! congested once collapsed the encoder to ~245 kbps. Overreacting down is recoverable;
//! oscillating is not. So the governor steps **down fast** — tracking the raw estimate
//! in ONE step, because the observation stream is notification-driven and goes silent
//! when GCC pins its estimate under sustained loss (a multi-observation descent
//! measurably stalls mid-way, flooding the pipe) — and **up slowly** (EWMA-gated,
//! ≤ +10%/step), retargets at most once per 2 s, only outside a 15% deadband. The raw
//! estimate is safe to track down: already GCC-filtered inside `rtpgccbwe`, not packet
//! noise. ABR only moves the CBR setpoint inside `[floor, ceiling]`; it never exceeds
//! the operator's configured ceiling.

use std::time::{Duration, Instant};

/// The governor's down-path policy. `Protective`/`Off` map to [`DownPolicy::Protective`]
/// (byte-identical to AS-03 — `Off` never applies a retarget at the glue layer, but the
/// pure state machine is shared), `Smooth` maps to [`DownPolicy::Smooth`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DownPolicy {
    /// Track the raw GCC estimate down in ONE step (stall-proof). The #68 posture.
    Protective,
    /// Smoothness-biased: cap a non-emergency downshift to a small step with a longer
    /// inter-step dwell, smooth the estimate when the encoder is freshly saturated, and
    /// refuse a >50%-cliff while over budget — but still drop FAST on CONFIRMED
    /// network congestion.
    Smooth,
}

/// The per-window adaptation hint the smooth down-path consumes, derived from the
/// [`adaptation`](super::adaptation) classifier + a rolling encode-time baseline,
/// computed once per ~5 s drain and pushed into the governor.
///
/// Default (all `false`) is the safe posture — plain capped-down step.
/// `Protective`/`Off` never read this.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct AdaptationHint {
    /// Confirmed network congestion this window (`NetworkCongested`: GCC fell
    /// meaningfully below the setpoint while the encoder sent AT the cap, no
    /// encode-time spike to blame). The only signal authorising the FAST protective
    /// descent, bypassing the smooth cap + dwell — the #68 emergency guard.
    pub network_congested: bool,
    /// The encoder is over its per-frame budget AND that is a FRESH/RISING condition,
    /// not steady-state (#328 carry-forward) — a steady host at its ceiling is
    /// saturated every window and must not be treated as fresh, or it would
    /// permanently suppress legit network downshifts. When set, the smooth path uses
    /// the EWMA (not raw) and applies the no->50%-cliff guard.
    pub encoder_saturation_fresh: bool,
}

/// Governor tuning constants. All rates in **kbit/s**.
#[derive(Debug, Clone, Copy)]
pub struct AbrConfig {
    /// Lower bound — ABR never starves the encoder below this (tier `abr_floor`).
    pub floor_kbps: u32,
    /// Upper bound — the operator's configured tier bitrate. ABR never exceeds it.
    pub ceiling_kbps: u32,
    /// Session frame rate — recomputes the single-frame VBV/CPB cap on every
    /// retarget (`cpb = kbps / fps`).
    pub fps: u32,
    /// EWMA smoothing factor (0,1]; higher tracks faster. Gates the UP path only —
    /// down-moves act on the raw (already GCC-filtered) estimate so a single
    /// notification completes the protective step (stall-proof).
    pub ewma_alpha: f64,
    /// Minimum wall-time between retargets (both directions). The anti-thrash gate.
    pub min_interval: Duration,
    /// Relative departure from the setpoint that must be exceeded before any
    /// retarget (suppresses micro-adjustment churn).
    pub deadband: f64,
    /// Maximum fractional increase per up-step (slow-up asymmetry).
    pub max_up_step: f64,
    /// Which down-path policy this governor runs. Constructed via [`AbrConfig::new`]
    /// (Protective) or [`AbrConfig::new_smooth`].
    pub down_policy: DownPolicy,
    /// `Smooth` only: max fractional DOWN step per non-emergency retarget. 0.125 ⇒
    /// −12.5% (the −10…−15% band). Ignored by `Protective`.
    pub max_down_step: f64,
    /// `Smooth` only: minimum wall-time between two non-emergency downshifts.
    /// Emergency drops on confirmed congestion bypass this.
    pub down_dwell: Duration,
    /// `Smooth` only: when the encoder is freshly saturated, GCC alone must not drive
    /// the setpoint below `cliff_guard_frac × setpoint` in one move (bitrate-down
    /// won't fix encoder saturation). 0.50 ⇒ no >50% cliff. Ignored by `Protective`.
    pub cliff_guard_frac: f64,
    /// `Some` on a WebTransport session in `Smooth` (#1376): the per-report loop
    /// ([`Governor::observe_report`]) instead of windows, the value being the least time
    /// between two encoder writes (zero where a write costs no keyframe).
    pub per_report: Option<Duration>,
}

impl AbrConfig {
    pub const DEFAULT_ALPHA: f64 = 0.3;
    pub const DEFAULT_DEADBAND: f64 = 0.15;
    /// 0.10 → 0.25 (with `DEFAULT_MIN_INTERVAL_MS` 2000 → 1000) cut the post-impairment
    /// setpoint ramp 37.8s → 14.0s and client-present drops 12.2% → 1.7%, zero
    /// oscillations: `docs/reports/2026-08-18-overnight-optimisation/t3-ladder-step-recovery.md`.
    pub const DEFAULT_MAX_UP_STEP: f64 = 0.25;
    pub const DEFAULT_MIN_INTERVAL: Duration = Duration::from_millis(1000);
    /// Smooth-mode defaults (down-path only; up-path is unchanged).
    pub const DEFAULT_MAX_DOWN_STEP: f64 = 0.125; // −12.5% per non-emergency retarget
    pub const DEFAULT_DOWN_DWELL: Duration = Duration::from_secs(7); // 5–10 s band
    pub const DEFAULT_CLIFF_GUARD_FRAC: f64 = 0.50; // never a >50% cliff under saturation
    /// Wire unit (ms) of `QUASAR_ABR_MIN_INTERVAL_MS` / `QUASAR_ABR_DOWN_DWELL_MS`.
    pub const DEFAULT_MIN_INTERVAL_MS: u64 = 1000;
    pub const DEFAULT_DOWN_DWELL_MS: u64 = 7000;

    /// Construct with the default hysteresis constants and the `Protective` policy.
    /// `floor`/`ceiling`/`fps` come from the session; the rest are tuned defaults.
    pub fn new(floor_kbps: u32, ceiling_kbps: u32, fps: u32) -> Self {
        Self {
            floor_kbps,
            ceiling_kbps,
            fps,
            ewma_alpha: Self::DEFAULT_ALPHA,
            min_interval: Self::DEFAULT_MIN_INTERVAL,
            deadband: Self::DEFAULT_DEADBAND,
            max_up_step: Self::DEFAULT_MAX_UP_STEP,
            down_policy: DownPolicy::Protective,
            max_down_step: Self::DEFAULT_MAX_DOWN_STEP,
            down_dwell: Self::DEFAULT_DOWN_DWELL,
            cliff_guard_frac: Self::DEFAULT_CLIFF_GUARD_FRAC,
            per_report: None,
        }
    }

    /// Same bounds/fps but with the smoothness-biased [`DownPolicy::Smooth`] down
    /// path. Up-path constants are identical to `new`.
    pub fn new_smooth(floor_kbps: u32, ceiling_kbps: u32, fps: u32) -> Self {
        Self {
            down_policy: DownPolicy::Smooth,
            ..Self::new(floor_kbps, ceiling_kbps, fps)
        }
    }

    /// Overlay the operator's hysteresis-tuning env knobs onto this config, in place.
    /// Every default equals the value it replaced, so all-vars-unset is a no-op.
    /// `floor`/`ceiling`/`fps`/`down_policy` are untouched. Knobs: `QUASAR_ABR_EWMA_ALPHA`
    /// (0,1], `QUASAR_ABR_DEADBAND` (0,1), `QUASAR_ABR_MAX_UP_STEP` (0,∞),
    /// `QUASAR_ABR_MIN_INTERVAL_MS` [1,∞), `QUASAR_ABR_MAX_DOWN_STEP` (0,1, Smooth only),
    /// `QUASAR_ABR_DOWN_DWELL_MS` [0,∞) (Smooth only), `QUASAR_ABR_CLIFF_GUARD_FRAC`
    /// (0,1, Smooth only). Each is validated; an invalid value warns once and falls back.
    pub fn with_env_overrides(self) -> Self {
        // Delegates to AbrGovernorSettings::from_env() so there's exactly one place
        // that reads+validates these vars.
        AbrGovernorSettings::from_env().apply_to(self)
    }

    /// Pure core of [`AbrConfig::with_env_overrides`]: `lookup` supplies each var's raw
    /// value instead of reading process env directly.
    pub fn with_overrides_from(self, lookup: &dyn Fn(&str) -> Option<String>) -> Self {
        AbrGovernorSettings::from_lookup(lookup).apply_to(self)
    }
}

/// The host's ABR governor hysteresis knobs (`QUASAR_ABR_EWMA_ALPHA`,
/// `QUASAR_ABR_DEADBAND`, `QUASAR_ABR_MAX_UP_STEP`, `QUASAR_ABR_MIN_INTERVAL_MS`,
/// `QUASAR_ABR_MAX_DOWN_STEP`, `QUASAR_ABR_DOWN_DWELL_MS`,
/// `QUASAR_ABR_CLIFF_GUARD_FRAC`), resolved `env ← config_update` like every other
/// [`crate::session::settings::RuntimeSettings`] field. Snapshotted into `SessionConfig`
/// at assign time, so a live-class push applies to the NEXT session — mirrors
/// [`super::ladder::LadderSettings`].
///
/// Closes a gap (`docs/reports/2026-08-18-overnight-optimisation/t3-ladder-step-recovery.md`):
/// these knobs were documented as operator-settable, but reading the raw process env at
/// session-build time was never wired into `deploy/docker-compose.yml`'s passthrough and
/// was only settable via a container recreate, never live via `config_update`.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct AbrGovernorSettings {
    pub ewma_alpha: f64,
    pub deadband: f64,
    pub max_up_step: f64,
    pub min_interval_ms: u64,
    pub max_down_step: f64,
    pub down_dwell_ms: u64,
    pub cliff_guard_frac: f64,
}

impl AbrGovernorSettings {
    pub fn from_env() -> Self {
        Self::from_lookup(&|k| std::env::var(k).ok())
    }

    /// Pure core of [`AbrGovernorSettings::from_env`]: `lookup` supplies each var's raw
    /// value instead of reading process env directly.
    pub fn from_lookup(lookup: &dyn Fn(&str) -> Option<String>) -> Self {
        Self {
            ewma_alpha: env_frac_open_unit_incl(
                "QUASAR_ABR_EWMA_ALPHA",
                AbrConfig::DEFAULT_ALPHA,
                lookup,
            ),
            deadband: env_frac_open_unit_excl(
                "QUASAR_ABR_DEADBAND",
                AbrConfig::DEFAULT_DEADBAND,
                lookup,
            ),
            max_up_step: env_pos_f64(
                "QUASAR_ABR_MAX_UP_STEP",
                AbrConfig::DEFAULT_MAX_UP_STEP,
                lookup,
            ),
            min_interval_ms: env_ms_min1(
                "QUASAR_ABR_MIN_INTERVAL_MS",
                AbrConfig::DEFAULT_MIN_INTERVAL_MS,
                lookup,
            ),
            max_down_step: env_frac_open_unit_excl(
                "QUASAR_ABR_MAX_DOWN_STEP",
                AbrConfig::DEFAULT_MAX_DOWN_STEP,
                lookup,
            ),
            down_dwell_ms: env_ms_ge0(
                "QUASAR_ABR_DOWN_DWELL_MS",
                AbrConfig::DEFAULT_DOWN_DWELL_MS,
                lookup,
            ),
            cliff_guard_frac: env_frac_open_unit_excl(
                "QUASAR_ABR_CLIFF_GUARD_FRAC",
                AbrConfig::DEFAULT_CLIFF_GUARD_FRAC,
                lookup,
            ),
        }
    }

    /// Sparse overlay from a `config_update` `settings` block. Unknown/out-of-range
    /// values are ignored defensively (the control plane already validated via
    /// `hostcfg.Catalog()`), matching `LadderSettings::apply_json`.
    pub fn apply_json(&mut self, v: &serde_json::Value) {
        if let Some(x) = v.get("abr_ewma_alpha").and_then(|x| x.as_f64()) {
            if x.is_finite() && x > 0.0 && x <= 1.0 {
                self.ewma_alpha = x;
            }
        }
        if let Some(x) = v.get("abr_deadband").and_then(|x| x.as_f64()) {
            if x.is_finite() && x > 0.0 && x < 1.0 {
                self.deadband = x;
            }
        }
        if let Some(x) = v.get("abr_max_up_step").and_then(|x| x.as_f64()) {
            if x.is_finite() && x > 0.0 {
                self.max_up_step = x;
            }
        }
        if let Some(x) = v.get("abr_min_interval_ms").and_then(|x| x.as_u64()) {
            if x >= 1 {
                self.min_interval_ms = x;
            }
        }
        if let Some(x) = v.get("abr_max_down_step").and_then(|x| x.as_f64()) {
            if x.is_finite() && x > 0.0 && x < 1.0 {
                self.max_down_step = x;
            }
        }
        if let Some(x) = v.get("abr_down_dwell_ms").and_then(|x| x.as_u64()) {
            self.down_dwell_ms = x;
        }
        if let Some(x) = v.get("abr_cliff_guard_frac").and_then(|x| x.as_f64()) {
            if x.is_finite() && x > 0.0 && x < 1.0 {
                self.cliff_guard_frac = x;
            }
        }
    }

    /// Report every knob into the host-observability effective map, keyed by the
    /// hostcfg catalog names so the admin UI can join it against `resolved`.
    pub fn write_effective(&self, m: &mut std::collections::BTreeMap<String, String>) {
        m.insert("abr_ewma_alpha".into(), self.ewma_alpha.to_string());
        m.insert("abr_deadband".into(), self.deadband.to_string());
        m.insert("abr_max_up_step".into(), self.max_up_step.to_string());
        m.insert(
            "abr_min_interval_ms".into(),
            self.min_interval_ms.to_string(),
        );
        m.insert("abr_max_down_step".into(), self.max_down_step.to_string());
        m.insert("abr_down_dwell_ms".into(), self.down_dwell_ms.to_string());
        m.insert(
            "abr_cliff_guard_frac".into(),
            self.cliff_guard_frac.to_string(),
        );
    }

    /// Overlay these resolved knobs onto an `AbrConfig` already constructed via
    /// `new`/`new_smooth` (which set `floor`/`ceiling`/`fps`/`down_policy`).
    pub fn apply_to(&self, mut cfg: AbrConfig) -> AbrConfig {
        cfg.ewma_alpha = self.ewma_alpha;
        cfg.deadband = self.deadband;
        cfg.max_up_step = self.max_up_step;
        cfg.min_interval = Duration::from_millis(self.min_interval_ms);
        cfg.max_down_step = self.max_down_step;
        cfg.down_dwell = Duration::from_millis(self.down_dwell_ms);
        cfg.cliff_guard_frac = self.cliff_guard_frac;
        cfg
    }
}

impl Default for AbrGovernorSettings {
    fn default() -> Self {
        Self::from_env()
    }
}

/// Parse a fraction in the OPEN-CLOSED unit interval `(0, 1]` (e.g. an EWMA alpha).
/// Junk/out-of-range WARNs once and returns `default`.
fn env_frac_open_unit_incl(
    var: &str,
    default: f64,
    lookup: &dyn Fn(&str) -> Option<String>,
) -> f64 {
    parse_validated_f64(var, default, |v| v > 0.0 && v <= 1.0, "(0, 1]", lookup)
}

/// Parse a fraction in the OPEN unit interval `(0, 1)` (deadband / down-step / cliff-guard).
/// Junk/out-of-range WARNs once and returns `default`.
fn env_frac_open_unit_excl(
    var: &str,
    default: f64,
    lookup: &dyn Fn(&str) -> Option<String>,
) -> f64 {
    parse_validated_f64(var, default, |v| v > 0.0 && v < 1.0, "(0, 1)", lookup)
}

/// Parse a strictly-positive fraction/multiplier `(0, ∞)` (e.g. max up-step).
/// Junk/out-of-range WARNs once and returns `default`.
fn env_pos_f64(var: &str, default: f64, lookup: &dyn Fn(&str) -> Option<String>) -> f64 {
    parse_validated_f64(var, default, |v| v > 0.0, "> 0", lookup)
}

/// Shared f64 env parser with range validation + warn-once-on-bad-value. A trimmed-empty
/// value is treated as unset (silent fall-through), matching `AbrMode::from_env`.
fn parse_validated_f64(
    var: &str,
    default: f64,
    ok: impl Fn(f64) -> bool,
    range: &str,
    lookup: &dyn Fn(&str) -> Option<String>,
) -> f64 {
    match lookup(var).as_deref().map(str::trim) {
        None | Some("") => default,
        Some(raw) => match raw.parse::<f64>() {
            Ok(v) if v.is_finite() && ok(v) => v,
            _ => {
                tracing::warn!(
                    token = "knob-invalid-abr-value",
                    "{var}={raw:?} is not a valid value in {range}; using default {default}"
                );
                default
            }
        },
    }
}

/// Parse a millisecond count that must be `>= 1` (a zero interval would defeat the
/// anti-thrash gate). Junk/zero WARNs once and returns `default`.
fn env_ms_min1(var: &str, default: u64, lookup: &dyn Fn(&str) -> Option<String>) -> u64 {
    parse_validated_ms(var, default, |v| v >= 1, ">= 1", lookup)
}

/// Parse a millisecond count that may be `>= 0` (0 = no dwell, a valid disable).
/// Junk WARNs once and returns `default`.
fn env_ms_ge0(var: &str, default: u64, lookup: &dyn Fn(&str) -> Option<String>) -> u64 {
    parse_validated_ms(var, default, |_| true, ">= 0", lookup)
}

/// Shared millisecond env parser with range validation + warn-once-on-bad-value.
fn parse_validated_ms(
    var: &str,
    default: u64,
    ok: impl Fn(u64) -> bool,
    range: &str,
    lookup: &dyn Fn(&str) -> Option<String>,
) -> u64 {
    match lookup(var).as_deref().map(str::trim) {
        None | Some("") => default,
        Some(raw) => match raw.parse::<u64>() {
            Ok(v) if ok(v) => v,
            _ => {
                tracing::warn!(
                    token = "knob-invalid-abr-duration",
                    "{var}={raw:?} is not a valid millisecond value in {range}; using default {default}"
                );
                default
            }
        },
    }
}

/// Per-report loop: no report for this long is feedback silence, so the estimate fed is
/// the silence decay's.
const REPORT_GAP: Duration = Duration::from_millis(500);

/// The ABR governor state machine. Fed the rtpgccbwe estimate; emits a new CBR
/// setpoint when (and only when) a retarget is warranted.
#[derive(Debug)]
pub struct Governor {
    cfg: AbrConfig,
    /// Smoothed estimate (kbps); `None` until the first observation.
    ewma: Option<f64>,
    /// The current encoder setpoint (kbps) — starts at the ceiling.
    setpoint: u32,
    /// Wall-time of the last retarget (and of construction, so the first interval holds).
    last_retarget: Instant,
    /// `Smooth` only: wall-time of the last non-emergency downshift. `None` until the
    /// first one fires, so the dwell never blocks it — only subsequent ones. Emergency
    /// drops bypass the dwell and don't stamp this.
    last_down: Option<Instant>,
    /// `Smooth` only: the latest per-window adaptation hint, pushed by the glue each
    /// drain. `Protective`/`Off` never read it.
    hint: AdaptationHint,
    /// Per-report loop: what the link allows, not clamped to the floor (the resolution
    /// rung reads it, so a floor cannot hide how far under it the link is).
    rate: f64,
    /// Per-report loop: the last estimate fed to `observe`, and when (the slew's clock).
    fed: Option<Instant>,
    /// Per-report loop: when the last receiver report came.
    last_report: Option<Instant>,
    /// Per-report loop: the first reading of a run under the line.
    under: Option<f64>,
}

impl Governor {
    /// Start at the tier ceiling. `start` seeds the min-interval clock so the governor
    /// holds for the first `min_interval`.
    pub fn new(cfg: AbrConfig, start: Instant) -> Self {
        Self {
            setpoint: cfg.ceiling_kbps,
            cfg,
            ewma: None,
            last_retarget: start,
            last_down: None,
            hint: AdaptationHint::default(),
            rate: cfg.ceiling_kbps as f64,
            fed: None,
            last_report: None,
            under: None,
        }
    }

    /// Per-report loop: what the link allows, unclamped by the floor. The ceiling
    /// otherwise.
    pub fn rate_kbps(&self) -> f64 {
        self.rate
    }

    /// The current CBR setpoint (kbps) — what the encoder bitrate is held at.
    pub fn setpoint_kbps(&self) -> u32 {
        self.setpoint
    }

    /// The governor's current lower bound (kbps). Read by the ladder: its "emergency"
    /// rung condition is *the governor is pinned at its floor*, a moving target since
    /// [`Self::set_floor_kbps`] rather than a launch-time constant.
    pub fn floor_kbps(&self) -> u32 {
        self.cfg.floor_kbps
    }

    /// Move the floor: it is a function of the picture the ladder is currently sending,
    /// not the picture the session launched with.
    ///
    /// Measured failure (`docs/reports/2026-08-16-abr-ladder/VALIDATION.md` run C): a
    /// 1080p120 session on an 11.5 Mbps ceiling has a launch floor of ~3450 kbps. Under
    /// a 3.5 Mbps netem cap the setpoint bottomed out above the link, flooding the pipe
    /// (83 packets lost, 107 ms jitter buffer, ~6 fps at the browser) — resolution
    /// changes bits per pixel, not bandwidth, so the ladder stepping down changed
    /// nothing. Only a lower floor lets the governor stop overdriving the path.
    ///
    /// - New floor clamps to `[1, ceiling]` — at/above ceiling would invert the clamp
    ///   in [`Self::observe`].
    /// - Lowering the floor never moves the setpoint; it only widens the range the
    ///   normal down path may travel (deadband/dwell/step cap still apply).
    /// - Raising the floor above the current setpoint raises the setpoint at once and
    ///   returns `Some(new_setpoint)` — this can't wait for the next retarget, since
    ///   encoding a restored bigger picture at the smaller picture's bitrate is exactly
    ///   the artefact the floor prevents. Deliberately does NOT stamp
    ///   `last_retarget`/`last_down`: a bound change, not a governor decision, must not
    ///   consume the next real observation's anti-thrash budget.
    /// - Returns `None` when the floor didn't move, or moved but the setpoint didn't.
    pub fn set_floor_kbps(&mut self, floor_kbps: u32) -> Option<u32> {
        let floor = floor_kbps.clamp(1, self.cfg.ceiling_kbps);
        if floor == self.cfg.floor_kbps {
            return None;
        }
        self.cfg.floor_kbps = floor;
        if self.setpoint < floor {
            self.setpoint = floor;
            return Some(floor);
        }
        None
    }

    /// The ladder's collapse step (#1320) moves the setpoint with the rung: down to
    /// `kbps` (clamped to the floor) at once, past the interval and the dwell. Never up.
    /// Restarts the interval and the up path's average from the new setpoint, so the
    /// estimate the link carried before the collapse cannot ramp it straight back, and an
    /// estimate under the floor cannot hold the climb back once the link returns.
    pub fn cut_to(&mut self, kbps: f64, now: Instant) -> Option<u32> {
        let target = (kbps.round() as i64)
            .clamp(self.cfg.floor_kbps as i64, self.cfg.ceiling_kbps as i64)
            as u32;
        if target >= self.setpoint {
            return None;
        }
        self.setpoint = target;
        self.last_retarget = now;
        self.ewma = Some(target as f64);
        Some(target)
    }

    /// Push the latest per-window adaptation hint. No-op on `Protective`/`Off` — only
    /// `Smooth` reads it. Called once per ~5 s drain.
    pub fn set_adaptation_hint(&mut self, hint: AdaptationHint) {
        self.hint = hint;
    }

    /// Feed one bandwidth estimate (kbps) observed at `now`. Returns
    /// `Some(new_setpoint_kbps)` (already clamped to `[floor, ceiling]`) when the
    /// encoder should retarget now, else `None`.
    pub fn observe(&mut self, estimate_kbps: f64, now: Instant) -> Option<u32> {
        if self.cfg.per_report.is_some() {
            return self.observe_per_report(estimate_kbps, now);
        }
        self.observe_window(estimate_kbps, now)
    }

    /// Per-report loop, one estimate (each report's, and the silence decay's): up only,
    /// never above the estimate, at most `max_up_step` per `min_interval` (the slew, no
    /// timer), in moves of more than the deadband. Down is [`Self::observe_report`]'s;
    /// only after `REPORT_GAP` with no report (the silence decay) does the window down
    /// path still lower the setpoint, and never the rate: silence is not evidence. That
    /// path is paced by `per_report` too, not only by `min_interval` (#1376 review R3).
    fn observe_per_report(&mut self, estimate: f64, now: Instant) -> Option<u32> {
        let dt = self.fed.map_or(0.0, |at| {
            now.saturating_duration_since(at).as_secs_f64().min(1.0)
        });
        self.fed = Some(now);
        let sp = self.setpoint as f64;
        let silent = self
            .last_report
            .is_none_or(|at| now.saturating_duration_since(at) >= REPORT_GAP);
        if silent && estimate < sp * (1.0 - self.cfg.deadband) {
            return self
                .write_due(now)
                .then(|| self.observe_window(estimate, now))
                .flatten();
        }
        let slew = self.cfg.max_up_step / self.cfg.min_interval.as_secs_f64();
        if estimate > self.rate {
            self.rate = estimate
                .min(self.rate * (1.0 + slew * dt))
                .min(self.cfg.ceiling_kbps as f64);
        }
        let target = self.clamp(self.rate.min(estimate).floor());
        let up = target > self.setpoint
            && (target as f64 >= sp * (1.0 + self.cfg.deadband) || target == self.cfg.ceiling_kbps);
        if !up || !self.write_due(now) {
            return None;
        }
        self.setpoint = target;
        self.last_retarget = now;
        Some(target)
    }

    /// Per-report loop, once per receiver report: `reading` is what the report showed the
    /// link carrying when the link held the sender back (`webtransport::Rate::measured`),
    /// `None` when it did not. A link that delivers what it is sent gives no reading,
    /// whatever its delay detector says: that is the #68 guard. Two readings in a row under
    /// the rate by more than the deadband take the rate and the setpoint to the higher of
    /// them in one move, with no step cap and no dwell.
    pub fn observe_report(&mut self, reading: Option<f64>, now: Instant) -> Option<u32> {
        self.cfg.per_report?;
        self.last_report = Some(now);
        let Some(reading) = reading.filter(|&r| r < self.rate * (1.0 - self.cfg.deadband)) else {
            self.under = None;
            return None;
        };
        let first = self.under.replace(reading)?;
        let target = first.max(reading);
        if !self.write_due(now) {
            self.under = Some(target);
            return None;
        }
        self.under = None;
        self.rate = target;
        let sp = self.clamp(target);
        if sp >= self.setpoint {
            return None;
        }
        self.setpoint = sp;
        self.last_retarget = now;
        Some(sp)
    }

    fn clamp(&self, kbps: f64) -> u32 {
        (kbps.round() as i64).clamp(self.cfg.floor_kbps as i64, self.cfg.ceiling_kbps as i64) as u32
    }

    /// Per-report loop: an encoder whose bitrate write costs a keyframe is written at most
    /// once per `per_report`.
    fn write_due(&self, now: Instant) -> bool {
        now.saturating_duration_since(self.last_retarget) >= self.cfg.per_report.unwrap_or_default()
    }

    /// The window loop (WebRTC, and WebTransport outside `Smooth`).
    ///
    /// The #68 guard: the governor is notification-driven and `rtpgccbwe` only
    /// notifies on estimate CHANGE — under sustained loss GCC pins the estimate and
    /// goes silent, so a down-path needing multiple observations to converge stalls
    /// mid-descent, stranding the encoder flooding the pipe (measured: 30s at 5562
    /// kbps into a 3500 kbit cap). So down acts on the RAW estimate in ONE step (it's
    /// already GCC-filtered; a lone spurious dip costs one recoverable step, preferred
    /// to oscillation/overdrive), while up stays EWMA-gated and capped at
    /// `+max_up_step` per interval — worst case strands the setpoint low, never
    /// overdrives the path.
    fn observe_window(&mut self, estimate_kbps: f64, now: Instant) -> Option<u32> {
        // EWMA updates on every observation — it gates the UP path.
        let smoothed = match self.ewma {
            Some(prev) => self.cfg.ewma_alpha * estimate_kbps + (1.0 - self.cfg.ewma_alpha) * prev,
            None => estimate_kbps,
        };
        self.ewma = Some(smoothed);

        // Anti-thrash: at most one retarget per min_interval (both directions).
        if now.saturating_duration_since(self.last_retarget) < self.cfg.min_interval {
            return None;
        }

        let sp = self.setpoint as f64;
        let clamp = |target: f64| -> u32 {
            (target.round() as i64).clamp(self.cfg.floor_kbps as i64, self.cfg.ceiling_kbps as i64)
                as u32
        };

        // UP-gate ceiling override: once setpoint exceeds ceiling/(1+deadband), the
        // regular gate `smoothed > sp×1.15` sits above the ceiling and can never pass,
        // permanently stranding recovery ~4-13% below the configured rate (measured:
        // stuck at 7645/8000 with the estimate pinned at max). No churn risk finishing
        // the climb: the ceiling clamps further steps and the down path still rules.
        let ceiling = self.cfg.ceiling_kbps as f64;
        let ceiling_headroom = estimate_kbps >= ceiling && sp < ceiling;

        // DOWN: raw estimate below the deadband ⇒ a downshift is warranted.
        let mut is_smooth_nonemergency_down = false;
        let target = if estimate_kbps < sp * (1.0 - self.cfg.deadband) {
            match self.cfg.down_policy {
                // Byte-identical to AS-03: track the raw estimate down in ONE step.
                DownPolicy::Protective => clamp(estimate_kbps),
                DownPolicy::Smooth => {
                    // Confirmed congestion: the protective one-step descent, bypassing
                    // the cap AND dwell — preserves #68's fast drop.
                    if self.hint.network_congested {
                        clamp(estimate_kbps)
                    } else {
                        // Dwell only spaces SUBSEQUENT downshifts; the first (no prior
                        // last_down) is never blocked. Emergency drops never stamp it.
                        if let Some(last_down) = self.last_down {
                            if now.saturating_duration_since(last_down) < self.cfg.down_dwell {
                                return None;
                            }
                        }
                        is_smooth_nonemergency_down = true;
                        // Freshly saturated: the bursty estimate is partly the encoder
                        // being late, not the network — smooth it (EWMA), don't chase the
                        // raw dip. Steady-state saturation (#328) doesn't set this flag.
                        let basis = if self.hint.encoder_saturation_fresh {
                            smoothed
                        } else {
                            estimate_kbps
                        };
                        let capped_floor = sp * (1.0 - self.cfg.max_down_step);
                        let mut want = basis.max(capped_floor);
                        // Cliff guard: bitrate-down won't fix encoder saturation, so GCC
                        // alone must not force a >cliff_guard_frac cliff in one move.
                        if self.hint.encoder_saturation_fresh {
                            want = want.max(sp * self.cfg.cliff_guard_frac);
                        }
                        clamp(want)
                    }
                }
            }
        }
        // UP: identical across policies — the slow ramp is unchanged.
        else if smoothed > sp * (1.0 + self.cfg.deadband) || ceiling_headroom {
            let upper = if ceiling_headroom { ceiling } else { smoothed };
            clamp((sp * (1.0 + self.cfg.max_up_step)).min(upper))
        } else {
            return None; // inside the deadband — hold.
        };

        if target == self.setpoint {
            // Already at floor/ceiling, or the up-step rounded back to the same value.
            return None;
        }
        self.setpoint = target;
        self.last_retarget = now;
        // Stamp the dwell clock ONLY on a non-emergency smooth downshift — emergency
        // drops and up-steps must not extend it.
        if is_smooth_nonemergency_down {
            self.last_down = Some(now);
        }
        Some(target)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg() -> AbrConfig {
        AbrConfig::new(2500, 8000, 60)
    }

    fn at(base: Instant, secs: u64) -> Instant {
        base + Duration::from_secs(secs)
    }

    #[test]
    fn starts_at_ceiling() {
        let base = Instant::now();
        let g = Governor::new(cfg(), base);
        assert_eq!(g.setpoint_kbps(), 8000);
    }

    #[test]
    fn holds_for_the_first_min_interval() {
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        assert_eq!(g.observe(1000.0, base + Duration::from_millis(500)), None);
        assert_eq!(g.setpoint_kbps(), 8000);
    }

    #[test]
    fn clean_lan_never_reduces() {
        // An estimate at/above the ceiling must never throttle below the tier bitrate.
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        for s in 1..30 {
            let out = g.observe(12000.0, at(base, s * 3));
            assert!(out.is_none(), "clean LAN must not retarget, got {out:?}");
        }
        assert_eq!(g.setpoint_kbps(), 8000);
    }

    #[test]
    fn small_departure_inside_deadband_holds() {
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        // 8000 → ~7000 is -12.5%, inside the 15% deadband ⇒ no retarget.
        assert_eq!(g.observe(7000.0, at(base, 3)), None);
        assert_eq!(g.observe(7000.0, at(base, 6)), None);
        assert_eq!(g.setpoint_kbps(), 8000);
    }

    #[test]
    fn steps_down_fast_to_track_the_estimate() {
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        let mut last = None;
        for s in 1..20 {
            last = g.observe(3000.0, at(base, s * 3)).or(last);
        }
        // Converges near 3000 (EWMA settles), never below the 2500 floor.
        assert!(g.setpoint_kbps() <= 4000 && g.setpoint_kbps() >= 2500);
        assert!(
            last.is_some(),
            "a collapse must produce at least one retarget"
        );
    }

    /// #1320: the collapse step moves the setpoint inside the first interval, down only,
    /// clamped to the (just lowered) floor, and the up path starts from the cut.
    #[test]
    fn cut_to_drops_at_once_and_never_raises() {
        let base = Instant::now();
        let mut g = Governor::new(AbrConfig::new_smooth(2500, 8000, 60), base);
        g.set_floor_kbps(900);
        let now = base + Duration::from_millis(100);
        assert_eq!(g.cut_to(1200.0, now), Some(1200), "inside min_interval");
        assert_eq!(g.cut_to(5000.0, now), None, "never up");
        assert_eq!(g.cut_to(100.0, now), Some(900), "never under the floor");
        // The pre-collapse average does not ramp it back: an estimate at the cut holds.
        assert_eq!(g.observe(900.0, at(base, 2)), None);
        // The average restarts at the clamped setpoint, not the raw estimate: seeded with 100
        // it would read 0.3 × 2000 + 0.7 × 100 = 670 and hold; from 900 it reads 1230 and
        // climbs one step.
        let mut g = Governor::new(AbrConfig::new_smooth(2500, 8000, 60), base);
        g.set_floor_kbps(900);
        assert_eq!(g.cut_to(100.0, now), Some(900));
        assert_eq!(g.observe(2000.0, at(base, 2)), Some(1125));
    }

    #[test]
    fn never_below_floor() {
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        for s in 1..40 {
            g.observe(100.0, at(base, s * 3)); // estimate far below the floor
        }
        assert_eq!(g.setpoint_kbps(), 2500, "setpoint must clamp at the floor");
    }

    #[test]
    fn steps_up_slowly_after_recovery() {
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        for s in 1..30 {
            g.observe(100.0, at(base, s * 3));
        }
        assert_eq!(g.setpoint_kbps(), 2500);
        // Each up-step is bounded to +25%, so a single jump can't overshoot the
        // ceiling. Recovery time must start after the down-phase's last observation.
        let before = g.setpoint_kbps();
        let mut t = 90u64;
        let after_one = loop {
            t += 3;
            if let Some(n) = g.observe(12000.0, at(base, t)) {
                break n;
            }
            if t > 120 {
                panic!("recovery never stepped up");
            }
        };
        // First up-step from 2500 is +25% ≈ 3125, NOT a jump to the ceiling.
        assert!(
            after_one > before && after_one <= 3200,
            "up-step must be ≤ +25% ({before} → {after_one})"
        );
    }

    #[test]
    fn recovery_is_monotone_and_bounded_to_ceiling() {
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        for s in 1..20 {
            g.observe(2500.0, at(base, s * 3));
        }
        let mut t = 60u64;
        let mut prev = g.setpoint_kbps();
        for _ in 0..60 {
            t += 3;
            if let Some(n) = g.observe(20000.0, at(base, t)) {
                assert!(n >= prev, "recovery must be monotone up ({prev} → {n})");
                assert!(n <= 8000, "must never exceed the ceiling, got {n}");
                prev = n;
            }
        }
        assert_eq!(g.setpoint_kbps(), 8000, "recovery converges to the ceiling");
    }

    #[test]
    fn recovery_completes_to_ceiling_with_pinned_estimate() {
        // After congestion clears, GCC pins its estimate exactly at the ceiling. The
        // regular up-gate is unreachable once sp > ceiling/1.15 without the
        // ceiling-headroom override (measured: stuck at 7645/8000) — the staircase
        // must still finish all the way to the ceiling.
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        for s in 1..20 {
            g.observe(2500.0, at(base, s * 3));
        }
        assert!(
            g.setpoint_kbps() <= 2904,
            "descent should be near the floor"
        );
        let mut t = 90u64;
        for _ in 0..60 {
            t += 3;
            g.observe(8000.0, at(base, t));
        }
        assert_eq!(
            g.setpoint_kbps(),
            8000,
            "a ceiling-pinned estimate must recover the setpoint fully to the ceiling"
        );
    }

    #[test]
    fn dip_inside_deadband_is_absorbed() {
        // 8000 → 7000 is −12.5%, inside the 15% deadband.
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        assert_eq!(g.observe(8000.0, at(base, 3)), None);
        let out = g.observe(7000.0, at(base, 6));
        assert!(
            out.is_none(),
            "a dip inside the deadband must be absorbed, got {out:?}"
        );
        assert_eq!(g.setpoint_kbps(), 8000);
    }

    #[test]
    fn severe_dip_tracks_down_in_one_step() {
        // One notification completes the protective action (the #68 stall-proof
        // posture: rtpgccbwe goes silent under sustained loss).
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        assert_eq!(g.observe(8000.0, at(base, 3)), None);
        let out = g
            .observe(1000.0, at(base, 6))
            .expect("a severe dip must retarget on the first observation");
        // Raw estimate 1000 clamps to the 2500 floor — the full protective step.
        assert_eq!(
            out, 2500,
            "down must track the raw estimate (clamped) in one step"
        );
        assert_eq!(g.setpoint_kbps(), 2500);
    }

    #[test]
    fn no_oscillation_under_jitter_around_setpoint() {
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        let jitter = [7300.0, 8600.0, 7600.0, 8400.0, 7800.0, 8200.0];
        let mut retargets = 0;
        for (i, &e) in jitter.iter().cycle().take(40).enumerate() {
            if g.observe(e, at(base, (i as u64 + 1) * 3)).is_some() {
                retargets += 1;
            }
        }
        assert_eq!(retargets, 0, "jitter inside the deadband must not retarget");
        assert_eq!(g.setpoint_kbps(), 8000);
    }

    // ---- protective/off parity guard -----------------------------------------------

    #[test]
    fn protective_config_is_the_default_policy() {
        assert_eq!(cfg().down_policy, DownPolicy::Protective);
        assert_eq!(smooth_cfg().down_policy, DownPolicy::Smooth);
    }

    #[test]
    fn protective_still_tracks_raw_estimate_down_in_one_step() {
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        assert_eq!(g.observe(8000.0, at(base, 3)), None);
        assert_eq!(g.observe(1000.0, at(base, 6)), Some(2500));
    }

    // ---- smooth-mode down path -------------------------------------------------

    fn smooth_cfg() -> AbrConfig {
        AbrConfig::new_smooth(2500, 8000, 60)
    }

    /// Build a smooth governor and seed the estimate at the ceiling (settles EWMA at
    /// 8000) so a later down observation is a clean step from the ceiling.
    fn smooth_at_ceiling(base: Instant) -> Governor {
        let mut g = Governor::new(smooth_cfg(), base);
        assert_eq!(g.observe(8000.0, at(base, 1)), None);
        g
    }

    #[test]
    fn smooth_caps_a_normal_down_step_to_max_down_step() {
        let base = Instant::now();
        let mut g = smooth_at_ceiling(base);
        let out = g
            .observe(4000.0, at(base, 4))
            .expect("a dip outside the deadband must retarget");
        // 8000 × (1 − 0.125) = 7000 — the capped step, not 4000.
        assert_eq!(
            out, 7000,
            "smooth down-step must cap to −12.5%, not track raw"
        );
        assert_eq!(g.setpoint_kbps(), 7000);
    }

    #[test]
    fn smooth_dwell_blocks_a_second_nonemergency_downshift_in_the_window() {
        let base = Instant::now();
        let mut g = smooth_at_ceiling(base);
        // first smooth down @ t=4
        assert_eq!(g.observe(4000.0, at(base, 4)), Some(7000));
        // t=7: min_interval (2 s) passed, but dwell (7 s) since the down has NOT.
        assert_eq!(
            g.observe(3000.0, at(base, 7)),
            None,
            "a second non-emergency downshift inside the dwell must hold"
        );
        assert_eq!(g.setpoint_kbps(), 7000);
        // t=12: dwell (7 s since the t=4 down) elapsed ⇒ a further smooth step is allowed.
        let out = g
            .observe(3000.0, at(base, 12))
            .expect("after the dwell a further smooth downshift is allowed");
        assert_eq!(out, (7000.0_f64 * 0.875).round() as u32); // 6125
    }

    #[test]
    fn smooth_confirmed_congestion_emergency_bypasses_cap_and_dwell() {
        let base = Instant::now();
        let mut g = smooth_at_ceiling(base);
        // First a non-emergency smooth step to arm the dwell.
        assert_eq!(g.observe(4000.0, at(base, 4)), Some(7000));
        // Now confirmed congestion within the dwell window: must still drop FAST.
        g.set_adaptation_hint(AdaptationHint {
            network_congested: true,
            encoder_saturation_fresh: false,
        });
        let out = g
            .observe(1000.0, at(base, 7))
            .expect("a confirmed-congestion emergency must bypass cap+dwell");
        // Tracks the raw estimate (1000) clamped to the 2500 floor — the full step.
        assert_eq!(out, 2500, "emergency must track raw down in one step (#68)");
    }

    #[test]
    fn smooth_encoder_saturation_does_not_cliff_more_than_half() {
        let base = Instant::now();
        let mut g = smooth_at_ceiling(base);
        g.set_adaptation_hint(AdaptationHint {
            network_congested: false,
            encoder_saturation_fresh: true,
        });
        let out = g
            .observe(800.0, at(base, 4))
            .expect("a saturation dip outside the deadband still steps (softly)");
        assert!(
            out >= 4000,
            "saturation must never cliff below 50% of the setpoint (got {out})"
        );
        // Concretely it is the −12.5% capped step (7000), well above the 4000 guard.
        assert_eq!(out, 7000);
    }

    #[test]
    fn smooth_steadystate_saturation_does_not_suppress_a_network_downshift() {
        // #328: steady-state saturation must not permanently suppress network adaptation.
        let base = Instant::now();
        let mut g = smooth_at_ceiling(base);
        // Steady-state saturation: NOT fresh. A real congestion dip arrives.
        g.set_adaptation_hint(AdaptationHint {
            network_congested: false,
            encoder_saturation_fresh: false,
        });
        let out = g
            .observe(4000.0, at(base, 4))
            .expect("a network downshift must proceed despite steady-state saturation");
        // It still smooths (caps to −12.5%) but it is NOT suppressed: the setpoint moved.
        assert_eq!(out, 7000);
        assert!(
            out < 8000,
            "steady-state saturation must not block the downshift"
        );
    }

    // ---- config exposure: QUASAR_ABR_* hysteresis knobs -----------------------------

    #[test]
    fn env_overrides_default_to_the_old_constants_and_are_picked_up() {
        // (a) All unset ⇒ overlay is a no-op: every field EXACTLY equals its old constant.
        let d = AbrConfig::new(2500, 8000, 60).with_overrides_from(&crate::test_env::lookup(&[]));
        assert_eq!(d.ewma_alpha, AbrConfig::DEFAULT_ALPHA);
        assert_eq!(d.deadband, AbrConfig::DEFAULT_DEADBAND);
        assert_eq!(d.max_up_step, AbrConfig::DEFAULT_MAX_UP_STEP);
        assert_eq!(d.min_interval, AbrConfig::DEFAULT_MIN_INTERVAL);
        assert_eq!(d.max_down_step, AbrConfig::DEFAULT_MAX_DOWN_STEP);
        assert_eq!(d.down_dwell, AbrConfig::DEFAULT_DOWN_DWELL);
        assert_eq!(d.cliff_guard_frac, AbrConfig::DEFAULT_CLIFF_GUARD_FRAC);
        // The bounds/fps/policy passthrough is untouched.
        assert_eq!(d.floor_kbps, 2500);
        assert_eq!(d.ceiling_kbps, 8000);
        assert_eq!(d.fps, 60);
        assert_eq!(d.down_policy, DownPolicy::Protective);

        // (b) Each var set to a valid value is picked up verbatim.
        let s =
            AbrConfig::new_smooth(2500, 8000, 60).with_overrides_from(&crate::test_env::lookup(&[
                ("QUASAR_ABR_EWMA_ALPHA", "0.5"),
                ("QUASAR_ABR_DEADBAND", "0.2"),
                ("QUASAR_ABR_MAX_UP_STEP", "0.25"),
                ("QUASAR_ABR_MIN_INTERVAL_MS", "3000"),
                ("QUASAR_ABR_MAX_DOWN_STEP", "0.2"),
                ("QUASAR_ABR_DOWN_DWELL_MS", "5000"),
                ("QUASAR_ABR_CLIFF_GUARD_FRAC", "0.4"),
            ]));
        assert_eq!(s.ewma_alpha, 0.5);
        assert_eq!(s.deadband, 0.2);
        assert_eq!(s.max_up_step, 0.25);
        assert_eq!(s.min_interval, Duration::from_millis(3000));
        assert_eq!(s.max_down_step, 0.2);
        assert_eq!(s.down_dwell, Duration::from_millis(5000));
        assert_eq!(s.cliff_guard_frac, 0.4);
        assert_eq!(
            s.down_policy,
            DownPolicy::Smooth,
            "policy is untouched by env"
        );

        // (c) Invalid / out-of-range values fall back to the default (warn + default).
        let f =
            AbrConfig::new_smooth(2500, 8000, 60).with_overrides_from(&crate::test_env::lookup(&[
                ("QUASAR_ABR_EWMA_ALPHA", "1.5"),     // > 1
                ("QUASAR_ABR_DEADBAND", "0"),         // not in (0,1)
                ("QUASAR_ABR_MAX_UP_STEP", "-0.1"),   // negative
                ("QUASAR_ABR_MIN_INTERVAL_MS", "0"),  // must be >= 1
                ("QUASAR_ABR_MAX_DOWN_STEP", "junk"), // unparseable
                ("QUASAR_ABR_CLIFF_GUARD_FRAC", "1"), // not in (0,1)
            ]));
        assert_eq!(f.ewma_alpha, AbrConfig::DEFAULT_ALPHA);
        assert_eq!(f.deadband, AbrConfig::DEFAULT_DEADBAND);
        assert_eq!(f.max_up_step, AbrConfig::DEFAULT_MAX_UP_STEP);
        assert_eq!(f.min_interval, AbrConfig::DEFAULT_MIN_INTERVAL);
        assert_eq!(f.max_down_step, AbrConfig::DEFAULT_MAX_DOWN_STEP);
        assert_eq!(f.cliff_guard_frac, AbrConfig::DEFAULT_CLIFF_GUARD_FRAC);

        // (d) A down-dwell of 0 is VALID (disables the dwell) — not a fall-back.
        let z =
            AbrConfig::new_smooth(2500, 8000, 60).with_overrides_from(&crate::test_env::lookup(&[
                ("QUASAR_ABR_DOWN_DWELL_MS", "0"),
            ]));
        assert_eq!(z.down_dwell, Duration::from_millis(0));
    }

    // ---- the floor follows the ladder rung ------------------------------------------

    #[test]
    fn set_floor_lowers_the_bound_without_moving_the_setpoint() {
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        for s in 1..10 {
            g.observe(1400.0, at(base, s * 3));
        }
        assert_eq!(g.setpoint_kbps(), 2500, "stranded at the launch floor");
        assert_eq!(g.floor_kbps(), 2500);
        assert_eq!(
            g.set_floor_kbps(1050),
            None,
            "lowering must not move the setpoint"
        );
        assert_eq!(g.floor_kbps(), 1050);
        assert_eq!(g.setpoint_kbps(), 2500);
        let out = g
            .observe(1400.0, at(base, 60))
            .expect("with the floor lowered the descent resumes");
        assert_eq!(out, 1400, "protective tracks the raw estimate down");
        assert_eq!(g.setpoint_kbps(), 1400);
    }

    #[test]
    fn set_floor_raises_the_setpoint_when_the_ladder_climbs_back() {
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        g.set_floor_kbps(1050);
        for s in 1..10 {
            g.observe(1100.0, at(base, s * 3));
        }
        assert!(g.setpoint_kbps() < 2500);
        assert_eq!(g.set_floor_kbps(2500), Some(2500));
        assert_eq!(g.setpoint_kbps(), 2500);
        assert_eq!(g.floor_kbps(), 2500);
    }

    #[test]
    fn set_floor_clamps_to_the_ceiling_and_ignores_a_no_op() {
        let base = Instant::now();
        let mut g = Governor::new(cfg(), base);
        assert_eq!(g.set_floor_kbps(99_999), None); // setpoint is already at 8000
        assert_eq!(g.floor_kbps(), 8000);
        // A floor of 0 would starve the encoder to nothing; clamps to 1.
        assert_eq!(g.set_floor_kbps(0), None);
        assert_eq!(g.floor_kbps(), 1);
        assert_eq!(g.set_floor_kbps(1), None);
        assert_eq!(g.floor_kbps(), 1);
    }

    #[test]
    fn an_untouched_floor_leaves_spt04_behaviour_byte_identical() {
        let base = Instant::now();
        let mut g = smooth_at_ceiling(base);
        assert_eq!(g.floor_kbps(), 2500);
        assert_eq!(g.observe(4000.0, at(base, 4)), Some(7000));
        assert_eq!(g.observe(3000.0, at(base, 7)), None);
        assert_eq!(g.observe(3000.0, at(base, 12)), Some(6125));
        assert_eq!(g.floor_kbps(), 2500, "the floor never moves on its own");
    }

    #[test]
    fn smooth_up_path_is_unchanged_from_protective() {
        let base = Instant::now();
        let mut g = Governor::new(smooth_cfg(), base);
        g.set_adaptation_hint(AdaptationHint {
            network_congested: true,
            encoder_saturation_fresh: false,
        });
        for s in 1..20 {
            g.observe(100.0, at(base, s * 3));
        }
        assert_eq!(
            g.setpoint_kbps(),
            2500,
            "emergency descent reaches the floor"
        );
        g.set_adaptation_hint(AdaptationHint::default());
        let before = g.setpoint_kbps();
        let mut t = 90u64;
        let after_one = loop {
            t += 3;
            if let Some(n) = g.observe(12000.0, at(base, t)) {
                break n;
            }
            if t > 130 {
                panic!("recovery never stepped up");
            }
        };
        assert!(
            after_one > before && after_one <= 3200,
            "smooth up-step must be ≤ +25% ({before} → {after_one})"
        );
    }

    // ── #1376: the per-report loop (WebTransport) ─────────────────────────────

    fn per_report(gap_ms: u64) -> AbrConfig {
        AbrConfig {
            per_report: Some(Duration::from_millis(gap_ms)),
            ..AbrConfig::new_smooth(1000, 10_000, 60)
        }
    }

    fn ms(base: Instant, ms: u64) -> Instant {
        base + Duration::from_millis(ms)
    }

    /// Down: two readings in a row under the rate by more than the deadband take the
    /// setpoint to the higher of them at once, with no −12.5 % cap, and a deeper drop right
    /// after is not held by a dwell. The window loop caps a step at −12.5 % and dwells 7 s.
    #[test]
    fn per_report_down_takes_the_reading_in_one_move() {
        let base = Instant::now();
        let mut g = Governor::new(per_report(0), base);
        assert_eq!(g.observe(10_000.0, ms(base, 0)), None);
        assert_eq!(
            g.observe_report(Some(5000.0), ms(base, 0)),
            None,
            "one reading is no evidence"
        );
        assert_eq!(
            g.observe(5000.0, ms(base, 100)),
            None,
            "the estimate alone never goes down"
        );
        assert_eq!(
            g.observe_report(Some(5100.0), ms(base, 100)),
            Some(5100),
            "the higher of the two"
        );
        assert_eq!(g.rate_kbps(), 5100.0);
        g.observe_report(Some(800.0), ms(base, 300));
        assert_eq!(
            g.observe_report(Some(700.0), ms(base, 400)),
            Some(1000),
            "the setpoint stops at the floor"
        );
        assert_eq!(g.rate_kbps(), 800.0, "the rate does not");
    }

    /// A report with no reading between two low ones breaks the run, and a reading inside
    /// the deadband is none.
    #[test]
    fn per_report_a_broken_run_or_a_reading_in_the_deadband_holds() {
        let base = Instant::now();
        let mut g = Governor::new(per_report(0), base);
        for i in 0..20 {
            let r = (i % 2 == 0).then_some(5000.0);
            assert_eq!(g.observe_report(r, ms(base, i * 100)), None);
        }
        for i in 20..40 {
            assert_eq!(
                g.observe_report(Some(8600.0), ms(base, i * 100)),
                None,
                "inside 15 %"
            );
        }
        assert_eq!(g.setpoint_kbps(), 10_000);
    }

    /// #68: with reports flowing, an estimate under the setpoint (a delay detector misreading
    /// a clean link) moves nothing; only a report's reading can. The window loop steps down
    /// 12.5 % on the first such estimate.
    #[test]
    fn per_report_a_low_estimate_without_a_reading_moves_nothing() {
        let base = Instant::now();
        let mut g = Governor::new(per_report(0), base);
        for i in 0..100 {
            let at = ms(base, i * 100);
            assert_eq!(
                g.observe(if i % 3 == 0 { 6000.0 } else { 9800.0 }, at),
                None
            );
            assert_eq!(g.observe_report(None, at), None);
        }
        assert_eq!(g.setpoint_kbps(), 10_000);
        let mut w = Governor::new(AbrConfig::new_smooth(1000, 10_000, 60), base);
        assert_eq!(
            w.observe(6000.0, ms(base, 1500)),
            Some(8750),
            "the window loop moves"
        );
    }

    /// Up: never above the estimate, never faster than `max_up_step` per `min_interval`
    /// (25 %/s, compounded per report here), each move past the deadband, and the ceiling
    /// once the estimate is there. The window loop climbs off an EWMA, one step per
    /// `min_interval`.
    #[test]
    fn per_report_up_is_slew_limited_under_the_estimate_and_past_the_deadband() {
        let base = Instant::now();
        let mut g = Governor::new(per_report(0), base);
        g.observe(10_000.0, ms(base, 0));
        g.observe_report(Some(2000.0), ms(base, 0));
        assert_eq!(g.observe_report(Some(2000.0), ms(base, 100)), Some(2000));
        let (mut est, mut moves) = (2000.0f64, vec![2000u32]);
        for i in 2..300u64 {
            // The estimate climbs 34 %/s here, faster than the slew lets the setpoint.
            est = (est * 1.03).min(10_000.0);
            let at = ms(base, i * 100);
            if let Some(sp) = g.observe(est, at) {
                moves.push(sp);
            }
            g.observe_report(None, at);
            let sp = g.setpoint_kbps() as f64;
            assert!(sp <= est, "never above the estimate: {sp} > {est}");
            assert!(
                sp <= 2000.0 * 1.025f64.powi(i as i32) + 1.0,
                "slew at {i}: {sp}"
            );
        }
        for pair in moves.windows(2) {
            assert!(
                pair[1] as f64 >= pair[0] as f64 * 1.15 || pair[1] == 10_000,
                "a move inside the deadband: {moves:?}"
            );
        }
        assert_eq!(g.setpoint_kbps(), 10_000);
        assert!(moves.len() <= 13, "bounded: {moves:?}");
    }

    /// The silence decay (no report for 500 ms) still lowers the setpoint the window loop's
    /// way, and never the rate the resolution rung reads.
    #[test]
    fn per_report_the_silence_decay_lowers_the_setpoint_never_the_rate() {
        let base = Instant::now();
        let mut g = Governor::new(per_report(0), base);
        g.observe(10_000.0, ms(base, 0));
        g.observe_report(None, ms(base, 0));
        assert_eq!(
            g.observe(4000.0, ms(base, 300)),
            None,
            "a report 300 ms ago: not silence"
        );
        assert_eq!(
            g.observe(4000.0, ms(base, 1500)),
            Some(8750),
            "silence: the capped step"
        );
        assert_eq!(g.rate_kbps(), 10_000.0);
    }

    /// An encoder whose bitrate write costs a keyframe (NVENC, VA) is written at most once per
    /// `per_report`; the run waits for it rather than being lost.
    #[test]
    fn per_report_writes_wait_out_the_gap() {
        let base = Instant::now();
        let mut g = Governor::new(per_report(1000), base);
        g.observe_report(Some(5000.0), ms(base, 500));
        assert_eq!(
            g.observe_report(Some(5000.0), ms(base, 600)),
            None,
            "1 s since the start"
        );
        assert_eq!(g.observe_report(Some(5000.0), ms(base, 1000)), Some(5000));
        g.observe_report(Some(2000.0), ms(base, 1100));
        assert_eq!(g.observe_report(Some(2000.0), ms(base, 1200)), None);
        assert_eq!(g.observe_report(Some(2000.0), ms(base, 2000)), Some(2000));
    }

    /// #1376 review R3: the silence decay is paced by `per_report` as well. With a 100 ms
    /// `min_interval` and confirmed congestion (no dwell), a falling estimate on silence
    /// ticks 100 ms apart is written at most once a second. Old code: every tick.
    #[test]
    fn per_report_the_silence_decay_waits_out_the_gap() {
        let base = Instant::now();
        let mut g = Governor::new(
            AbrConfig {
                min_interval: Duration::from_millis(100),
                ..per_report(1000)
            },
            base,
        );
        g.set_adaptation_hint(AdaptationHint {
            network_congested: true,
            encoder_saturation_fresh: false,
        });
        let writes: Vec<u64> = (10..40u64)
            .filter(|i| {
                g.observe(9000.0 - *i as f64 * 200.0, ms(base, i * 100))
                    .is_some()
            })
            .map(|i| i * 100)
            .collect();
        assert_eq!(writes, vec![1000, 2000, 3000], "{writes:?}");
    }

    /// The window loop (WebRTC, `protective`) never reads a report.
    #[test]
    fn a_window_governor_ignores_reports() {
        let base = Instant::now();
        let mut g = Governor::new(AbrConfig::new_smooth(1000, 10_000, 60), base);
        for i in 0..10 {
            assert_eq!(g.observe_report(Some(2000.0), ms(base, i * 100)), None);
        }
        assert_eq!(g.setpoint_kbps(), 10_000);
    }
}
