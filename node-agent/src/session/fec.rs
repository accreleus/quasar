//! FEC auto-arm — loss-triggered ULPFEC ramp.
//!
//! `QUASAR_FEC_PERCENTAGE=N` was static: 0 (off, default) or a fixed N% overhead for the
//! whole session. This module is the **pure** half of a third mode, `auto`, that negotiates
//! `ulp-red` up front at 0% (zero overhead, no SDP renegotiation) and ramps
//! `fec-percentage` 0→N→0 mid-stream based on an agent-local loss signal.
//!
//! Premise (`docs/design/plans/2026-07-22-fec-auto-arm.md` Gate 0): `ulp-red` cannot be
//! added mid-session (one `offer created` per session, never renegotiated), but negotiating
//! `fec-type=ulp-red`/`fec-percentage=0` before the offer puts the `red`/`ulpfec` lines in
//! the SDP while no repair data flows, and `fec-percentage` can then be ramped live.
//!
//! Split, mirroring [`super::abr`]: this file is mode derivation ([`FecMode`] /
//! [`derive_plan`]) and the hysteresis state machine ([`FecController`]), unit-tested
//! without GStreamer. The gst glue lives in [`super::pipeline`]'s `webrtc` submodule.
//!
//! The controller reads loss only; ABR reads the GCC estimate — no shared state, no
//! cross-writes. Arming adds ~N% wire bitrate and GCC pulling the setpoint down in
//! response is correct, not a bug to special-case.

/// The three FEC operating modes. Selected via `QUASAR_FEC_MODE`, or derived from
/// the legacy `QUASAR_FEC_PERCENTAGE` knob for back-compat.
///
/// Precedence (highest first):
///   1. `QUASAR_FEC_MODE` (explicit: `"off"` | `"fixed"` | `"auto"`)
///   2. Derived: `QUASAR_FEC_PERCENTAGE=0`/unset → `Off`; `>0` → `Fixed`
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FecMode {
    /// No FEC: `fec-type`/`fec-percentage` untouched, no `red`/`ulpfec` lines. Default.
    Off,
    /// Static `ulp-red` at `QUASAR_FEC_PERCENTAGE`% for the whole session.
    Fixed,
    /// Negotiate `ulp-red` at 0%, then ramp `fec-percentage` 0→armed→0 based on the
    /// agent-local per-window loss signal (the hysteresis [`FecController`]).
    Auto,
}

/// Classifying a raw `QUASAR_FEC_MODE` value: separates "unset or empty" (silent
/// fall-through to the percentage-derived mode) from "unrecognised" (warns) — mirrors
/// [`super::AbrMode`]'s `classify_env`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FecModeEnv {
    Recognised(FecMode),
    UnsetOrEmpty,
    Unrecognised,
}

impl FecMode {
    /// Parse from a string (case-insensitive); `None` on unknown values. Named
    /// `parse_env` to avoid clippy's `should_implement_trait`, matching
    /// [`super::AbrMode::parse_env`].
    pub fn parse_env(s: &str) -> Option<Self> {
        match s.to_ascii_lowercase().as_str() {
            "off" => Some(FecMode::Off),
            "fixed" => Some(FecMode::Fixed),
            "auto" => Some(FecMode::Auto),
            _ => None,
        }
    }

    /// The canonical wire string for this mode.
    pub fn as_str(self) -> &'static str {
        match self {
            FecMode::Off => "off",
            FecMode::Fixed => "fixed",
            FecMode::Auto => "auto",
        }
    }

    /// Classify a raw `QUASAR_FEC_MODE` value. A trimmed-empty value is treated exactly as
    /// unset — docker-compose forwards an unset host var as `QUASAR_FEC_MODE=""`, and that
    /// must fall through silently, never as unrecognised.
    pub fn classify_env(raw: Option<&str>) -> FecModeEnv {
        match raw.map(str::trim) {
            None | Some("") => FecModeEnv::UnsetOrEmpty,
            Some(s) => match Self::parse_env(s) {
                Some(m) => FecModeEnv::Recognised(m),
                None => FecModeEnv::Unrecognised,
            },
        }
    }
}

/// The resolved FEC setup for a session, from the mode + `QUASAR_FEC_PERCENTAGE`.
/// `negotiate` drives whether `enable_video_fec` sets `fec-type=ulp-red`, `initial_pct`
/// is the `fec-percentage` set at build, and `controller_enabled` spawns the auto
/// poll+ramp thread (which arms to `armed_pct`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct FecPlan {
    /// Negotiate `ulp-red` in the offer (set `fec-type` + `fec-percentage`). `false`
    /// leaves the transceiver untouched (off; no `red`/`ulpfec` lines).
    pub negotiate: bool,
    /// `fec-percentage` value set at build time (before the offer). `0` in `Auto`
    /// (armed later by the controller); `armed_pct` in `Fixed`.
    pub initial_pct: u32,
    /// Run the auto controller (poll loss, ramp `fec-percentage`). Only `Auto`.
    pub controller_enabled: bool,
    /// The percentage the controller arms to (also `Fixed`'s static level). In
    /// `Auto` this is the `QUASAR_FEC_PERCENTAGE` knob if set (>0), else the
    /// [`DEFAULT_ARMED_PCT`] default.
    pub armed_pct: u32,
}

/// The default armed percentage in `auto` mode when `QUASAR_FEC_PERCENTAGE` is
/// unset (§3 table: "Armed percentage … `20`"). `Fixed` mode always carries an
/// explicit non-zero percentage, so this default only applies to `Auto`.
pub const DEFAULT_ARMED_PCT: u32 = 20;

/// Derive the [`FecPlan`] from a resolved [`FecMode`] and the `QUASAR_FEC_PERCENTAGE`
/// knob (0 if unset). Pure — env resolution lives in [`super::SessionConfig`].
///
/// - `Off` → don't negotiate, no controller.
/// - `Fixed` → negotiate `ulp-red` at `percentage`; `Fixed` at 0 is degenerate and
///   treated as off (same wire shape as `QUASAR_FEC_PERCENTAGE=0`).
/// - `Auto` → negotiate `ulp-red` at 0, run the controller; arm to `percentage` if
///   set (>0), else [`DEFAULT_ARMED_PCT`].
pub fn derive_plan(mode: FecMode, percentage: u32) -> FecPlan {
    match mode {
        FecMode::Off => FecPlan {
            negotiate: false,
            initial_pct: 0,
            controller_enabled: false,
            armed_pct: 0,
        },
        FecMode::Fixed => {
            // Degenerate fixed-at-0: behave exactly like off (no negotiation).
            let negotiate = percentage > 0;
            FecPlan {
                negotiate,
                initial_pct: percentage,
                controller_enabled: false,
                armed_pct: percentage,
            }
        }
        FecMode::Auto => {
            let armed = if percentage > 0 {
                percentage
            } else {
                DEFAULT_ARMED_PCT
            };
            FecPlan {
                negotiate: true,
                initial_pct: 0,
                controller_enabled: true,
                armed_pct: armed,
            }
        }
    }
}

/// Auto-controller tuning (§3 table). All windows are counts of the evaluation
/// window (`window_s`).
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct FecControllerConfig {
    /// Arm when per-window loss reaches this percentage of packets. `QUASAR_FEC_ARM_LOSS_PCT`.
    pub arm_loss_pct: f64,
    /// Consecutive over-threshold windows required to arm. `QUASAR_FEC_ARM_WINDOWS`.
    pub arm_windows: u32,
    /// Consecutive clean (`< CLEAN_LOSS_PCT`) windows required to disarm. `QUASAR_FEC_DISARM_WINDOWS`.
    pub disarm_windows: u32,
    /// Max arm events per session before latching armed (stop oscillating). `QUASAR_FEC_MAX_FLAPS`.
    pub max_flaps: u32,
    /// The percentage the controller arms to. From the [`FecPlan::armed_pct`].
    pub armed_pct: u32,
    /// Evaluation window length in seconds — carried for logging/tracer context
    /// (the glue's poll cadence). `QUASAR_FEC_WINDOW_S`.
    pub window_s: u64,
}

impl FecControllerConfig {
    /// Arm threshold default (% of packets in the window).
    pub const DEFAULT_ARM_LOSS_PCT: f64 = 0.5;
    /// Windows over threshold to arm (2 × 5 s = 10 s of loss).
    pub const DEFAULT_ARM_WINDOWS: u32 = 2;
    /// Clean windows to disarm (6 × 5 s = 30 s clean).
    pub const DEFAULT_DISARM_WINDOWS: u32 = 6;
    /// Arm/disarm cycles per session before latching armed.
    pub const DEFAULT_MAX_FLAPS: u32 = 4;
    /// Evaluation window (s).
    pub const DEFAULT_WINDOW_S: u64 = 5;
    /// A window with loss below this counts as "clean" for disarm (§3:
    /// "zero-ish loss (< 0.1%)"). A window between this and `arm_loss_pct` is
    /// neither — it breaks both the arm streak and the clean streak.
    pub const CLEAN_LOSS_PCT: f64 = 0.1;
}

/// A transition the controller decided this window. `None` from
/// [`FecController::observe`] means no change (stay as-is).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FecAction {
    /// Ramp `fec-percentage` to this value in one step (burst loss is the target;
    /// ramping slowly defeats the purpose).
    Arm(u32),
    /// Ramp `fec-percentage` back to 0.
    Disarm,
}

/// The hysteresis state machine (§3). Pure: fed one per-window loss percentage at
/// a time via [`observe`](Self::observe), returns the transition (if any). No
/// clock, no I/O — the glue owns the poll cadence and the transceiver write.
#[derive(Debug, Clone)]
pub struct FecController {
    cfg: FecControllerConfig,
    armed: bool,
    /// Consecutive over-threshold windows (the arm streak).
    over_count: u32,
    /// Consecutive clean windows (the disarm streak).
    clean_count: u32,
    /// Arm events so far this session.
    flaps: u32,
    /// After `max_flaps` arm events, stay armed forever (a link that keeps cycling
    /// is lossy; stop oscillating).
    latched: bool,
}

impl FecController {
    pub fn new(cfg: FecControllerConfig) -> Self {
        Self {
            cfg,
            armed: false,
            over_count: 0,
            clean_count: 0,
            flaps: 0,
            latched: false,
        }
    }

    pub fn is_armed(&self) -> bool {
        self.armed
    }

    pub fn is_latched(&self) -> bool {
        self.latched
    }

    /// The configured evaluation window (s) — for the glue's poll cadence + logs.
    pub fn window_s(&self) -> u64 {
        self.cfg.window_s
    }

    /// Feed one window's loss percentage (0..100). Returns the transition to apply,
    /// or `None` to leave `fec-percentage` where it is.
    ///
    /// Streak accounting:
    /// - `loss >= arm_loss_pct` → over-window (breaks the clean streak).
    /// - `loss < CLEAN_LOSS_PCT` → clean window (breaks the arm streak).
    /// - in-between → neither; breaks BOTH streaks.
    pub fn observe(&mut self, loss_pct: f64) -> Option<FecAction> {
        if loss_pct >= self.cfg.arm_loss_pct {
            self.over_count = self.over_count.saturating_add(1);
            self.clean_count = 0;
        } else if loss_pct < FecControllerConfig::CLEAN_LOSS_PCT {
            self.clean_count = self.clean_count.saturating_add(1);
            self.over_count = 0;
        } else {
            // Middle band: not lossy enough to arm, not clean enough to disarm.
            self.over_count = 0;
            self.clean_count = 0;
        }

        if !self.armed {
            // A latched controller is always armed, so this branch only runs
            // pre-latch. Arm on a completed over-streak.
            if self.over_count >= self.cfg.arm_windows {
                self.armed = true;
                self.over_count = 0;
                self.clean_count = 0;
                self.flaps = self.flaps.saturating_add(1);
                if self.flaps >= self.cfg.max_flaps {
                    self.latched = true;
                }
                return Some(FecAction::Arm(self.cfg.armed_pct));
            }
            None
        } else {
            // Armed. Latched links never disarm.
            if !self.latched && self.clean_count >= self.cfg.disarm_windows {
                self.armed = false;
                self.over_count = 0;
                self.clean_count = 0;
                return Some(FecAction::Disarm);
            }
            None
        }
    }
}

/// Per-window loss percentage from cumulative RTCP counters. `delta_lost` is the
/// increase in `remote-inbound-rtp.packets-lost`, `delta_sent` the increase in
/// `outbound-rtp.packets-sent`, both over the window. Zero packets sent (or a
/// non-positive lost delta from RTCP duplicate accounting) yields 0.0 — a clean
/// window, no div-by-zero.
pub fn window_loss_pct(delta_lost: i64, delta_sent: u64) -> f64 {
    if delta_lost <= 0 {
        return 0.0;
    }
    let denom = delta_sent.max(1) as f64;
    100.0 * delta_lost as f64 / denom
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg() -> FecControllerConfig {
        FecControllerConfig {
            arm_loss_pct: FecControllerConfig::DEFAULT_ARM_LOSS_PCT,
            arm_windows: FecControllerConfig::DEFAULT_ARM_WINDOWS,
            disarm_windows: FecControllerConfig::DEFAULT_DISARM_WINDOWS,
            max_flaps: FecControllerConfig::DEFAULT_MAX_FLAPS,
            armed_pct: DEFAULT_ARMED_PCT,
            window_s: FecControllerConfig::DEFAULT_WINDOW_S,
        }
    }

    // ── U1: controller state machine ────────────────────────────────────────

    #[test]
    fn arms_after_exactly_arm_windows_not_before() {
        let mut c = FecController::new(cfg());
        // First over-threshold window: no arm yet (arm_windows = 2).
        assert_eq!(c.observe(1.0), None, "one over-window must not arm");
        assert!(!c.is_armed());
        // Second consecutive over-window: arms.
        assert_eq!(c.observe(1.0), Some(FecAction::Arm(20)));
        assert!(c.is_armed());
    }

    #[test]
    fn over_streak_must_be_consecutive() {
        let mut c = FecController::new(cfg());
        assert_eq!(c.observe(1.0), None, "over #1");
        // A clean window breaks the arm streak.
        assert_eq!(c.observe(0.0), None, "clean resets over_count");
        assert_eq!(c.observe(1.0), None, "over #1 again — still not armed");
        assert_eq!(c.observe(1.0), Some(FecAction::Arm(20)), "over #2 arms");
    }

    #[test]
    fn middle_band_breaks_the_arm_streak() {
        let mut c = FecController::new(cfg());
        assert_eq!(c.observe(1.0), None, "over #1");
        // 0.3% is between CLEAN (0.1) and ARM (0.5): neither — breaks the streak.
        assert_eq!(c.observe(0.3), None, "middle band resets over_count");
        assert_eq!(c.observe(1.0), None, "over #1 again");
        assert_eq!(c.observe(1.0), Some(FecAction::Arm(20)));
    }

    #[test]
    fn threshold_boundary_loss_equal_threshold_arms() {
        let mut c = FecController::new(cfg());
        // loss == threshold (0.5) counts as over.
        assert_eq!(c.observe(0.5), None);
        assert_eq!(c.observe(0.5), Some(FecAction::Arm(20)));
    }

    #[test]
    fn just_below_clean_threshold_counts_as_clean() {
        // 0.09% counts as clean (< 0.1); disarm after DISARM_WINDOWS clean windows.
        let mut c = FecController::new(cfg());
        c.observe(1.0);
        assert_eq!(c.observe(1.0), Some(FecAction::Arm(20)));
        // Five clean windows: not yet (disarm_windows = 6).
        for i in 0..5 {
            assert_eq!(
                c.observe(0.09),
                None,
                "clean window {i} must not disarm yet"
            );
        }
        assert_eq!(
            c.observe(0.09),
            Some(FecAction::Disarm),
            "6th clean disarms"
        );
        assert!(!c.is_armed());
    }

    #[test]
    fn disarm_streak_must_be_consecutive() {
        let mut c = FecController::new(cfg());
        c.observe(1.0);
        c.observe(1.0); // armed
        for _ in 0..5 {
            assert_eq!(c.observe(0.0), None);
        }
        // A lossy window breaks the disarm streak.
        assert_eq!(c.observe(1.0), None, "loss resets clean_count while armed");
        // Now a full DISARM_WINDOWS run is needed again.
        for _ in 0..5 {
            assert_eq!(c.observe(0.0), None);
        }
        assert_eq!(c.observe(0.0), Some(FecAction::Disarm));
    }

    #[test]
    fn flap_latch_at_max_flaps() {
        let mut c = FecController::new(cfg());
        // Drive MAX_FLAPS (4) full arm/disarm cycles. The 4th arm latches.
        for cycle in 0..4 {
            assert_eq!(c.observe(1.0), None, "cycle {cycle}: over #1");
            let armed = c.observe(1.0);
            assert_eq!(armed, Some(FecAction::Arm(20)), "cycle {cycle}: arm");
            if cycle < 3 {
                // Not yet latched — disarm normally.
                assert!(!c.is_latched(), "cycle {cycle}: not latched before 4th arm");
                for _ in 0..5 {
                    assert_eq!(c.observe(0.0), None);
                }
                assert_eq!(
                    c.observe(0.0),
                    Some(FecAction::Disarm),
                    "cycle {cycle}: disarm"
                );
            }
        }
        // After the 4th arm the controller is latched: no amount of clean disarms it.
        assert!(c.is_latched(), "latched after MAX_FLAPS arm events");
        assert!(c.is_armed());
        for _ in 0..20 {
            assert_eq!(c.observe(0.0), None, "latched controller never disarms");
        }
        assert!(c.is_armed());
    }

    #[test]
    fn window_loss_with_zero_packets_sent_is_clean_no_div_by_zero() {
        assert_eq!(window_loss_pct(0, 0), 0.0);
        assert_eq!(window_loss_pct(0, 100), 0.0);
        // Negative lost delta (RTCP duplicate accounting) clamps to clean.
        assert_eq!(window_loss_pct(-3, 100), 0.0);
        // A zero-sent window that somehow reports loss stays finite (denom = 1).
        assert!(window_loss_pct(5, 0).is_finite());
    }

    #[test]
    fn window_loss_basic_arithmetic() {
        // 5 lost of 1000 sent = 0.5%.
        assert!((window_loss_pct(5, 1000) - 0.5).abs() < 1e-9);
        // 30 lost of 1000 = 3%.
        assert!((window_loss_pct(30, 1000) - 3.0).abs() < 1e-9);
    }

    // ── U2: mode derivation ─────────────────────────────────────────────────

    #[test]
    fn mode_parse_env() {
        assert_eq!(FecMode::parse_env("off"), Some(FecMode::Off));
        assert_eq!(FecMode::parse_env("FIXED"), Some(FecMode::Fixed));
        assert_eq!(FecMode::parse_env("Auto"), Some(FecMode::Auto));
        assert_eq!(FecMode::parse_env("bogus"), None);
    }

    #[test]
    fn mode_classify_env() {
        assert_eq!(FecMode::classify_env(None), FecModeEnv::UnsetOrEmpty);
        assert_eq!(FecMode::classify_env(Some("")), FecModeEnv::UnsetOrEmpty);
        assert_eq!(FecMode::classify_env(Some("  ")), FecModeEnv::UnsetOrEmpty);
        assert_eq!(
            FecMode::classify_env(Some("auto")),
            FecModeEnv::Recognised(FecMode::Auto)
        );
        assert_eq!(
            FecMode::classify_env(Some("nope")),
            FecModeEnv::Unrecognised
        );
    }

    /// The (negotiate, initial_pct, controller_enabled) matrix over
    /// unset/0/N/explicit-off/fixed/auto.
    #[test]
    fn plan_derivation_matrix() {
        // Off (unset ⇒ Off derived, or explicit off): no negotiation, no controller.
        let off = derive_plan(FecMode::Off, 0);
        assert_eq!(
            (off.negotiate, off.initial_pct, off.controller_enabled),
            (false, 0, false)
        );
        // Explicit off ignores a stray percentage.
        let off_n = derive_plan(FecMode::Off, 20);
        assert_eq!(
            (off_n.negotiate, off_n.initial_pct, off_n.controller_enabled),
            (false, 0, false)
        );

        // Fixed at N (the QUASAR_FEC_PERCENTAGE>0 back-compat path): negotiate at N,
        // no controller.
        let fixed = derive_plan(FecMode::Fixed, 20);
        assert_eq!(
            (fixed.negotiate, fixed.initial_pct, fixed.controller_enabled),
            (true, 20, false)
        );
        assert_eq!(fixed.armed_pct, 20);

        // Degenerate fixed-at-0 ⇒ off wire shape.
        let fixed0 = derive_plan(FecMode::Fixed, 0);
        assert_eq!(
            (
                fixed0.negotiate,
                fixed0.initial_pct,
                fixed0.controller_enabled
            ),
            (false, 0, false)
        );

        // Auto with an explicit armed level: negotiate at 0, controller on, arm to N.
        let auto_n = derive_plan(FecMode::Auto, 30);
        assert_eq!(
            (
                auto_n.negotiate,
                auto_n.initial_pct,
                auto_n.controller_enabled
            ),
            (true, 0, true)
        );
        assert_eq!(auto_n.armed_pct, 30);

        // Auto with no percentage set (0): armed level defaults to 20.
        let auto0 = derive_plan(FecMode::Auto, 0);
        assert_eq!(
            (auto0.negotiate, auto0.initial_pct, auto0.controller_enabled),
            (true, 0, true)
        );
        assert_eq!(auto0.armed_pct, DEFAULT_ARMED_PCT);
    }
}
