//! SPT-08 — the smoothness-first adaptation ladder (pure decision half).
//!
//! When a degradation is detected the cheapest lever should be pulled first, escalating
//! to a more disruptive one only when the cheaper one can't stabilise the stream. Rungs,
//! least-disruptive first:
//!
//!  1. **Bitrate trim** — handled entirely by the smooth governor ([`crate::session::abr`]).
//!     The ladder defers to it and never touches bitrate.
//!  2. **Encoder speed bias** — on `EncoderSaturated`, bias the encoder faster via a live
//!     property write (VA `target-usage` up, NVENC `preset` faster): no caps change, no
//!     graph change, no webrtcbin renegotiation. The only rung this module actuates,
//!     safe default-on.
//!  3. **Framerate drop** (60→45→30) — DEFERRED. fps is pinned as fixed caps on the
//!     `allow-renegotiation=false` interpipesrc boundary (the P2-07 swap invariant), so
//!     changing it via caps would fault that boundary; the renegotiation-free
//!     alternative (frame decimation) invalidates the per-frame VBV/CPB budget
//!     (`cpb = kbps/fps`). Gated off behind [`LadderConfig::fps_enabled`] (default off).
//!  4. **Resolution drop** — the lever exists (adaptive-external-resolution adds a
//!     mutable scale stage in the ENCODE pipeline, downstream of interpipe, so a step
//!     doesn't touch compositor/interpipe caps or trigger renegotiation), implemented
//!     here as [`ResolutionRung`] with its own hysteresis/dwell/settle/emergency policy
//!     ([`ResolutionPolicy`]), actuated through `ResolutionLever` in
//!     [`crate::session::pipeline::abr_glue`]. Ships DARK behind
//!     [`LadderConfig::resolution_enabled`] (`abr_ladder_resolution`, default false)
//!     until the per-host netem soak signs it off.
//!  5. **Playout increase** — `client_presentation_limited` is a control-plane/browser-trace
//!     signal the agent never sees (invariant #1); stays with the AS-05 client-side
//!     playout controller.
//!
//! ## Why a separate pure module
//! Mirrors [`crate::session::abr`]: unit-testable decision logic here, gst glue in
//! [`crate::session::pipeline::abr_glue`]. Runs once per ~5 s metrics drain, never
//! per-frame, never on the latency path.
//!
//! ## Hysteresis (dwell + return-up)
//! A degradation must persist for `engage_dwell` consecutive windows before stepping up,
//! and the stream must be healthy for `recover_dwell` consecutive windows before
//! stepping back down. Recovery is one rung per eligible window: the encoder speed bias
//! is the "sticky" lever, unwound last, only after bitrate (the cheapest lever,
//! restored faster by the governor's up-ramp) has recovered the path.

use crate::session::adaptation::AdaptationState;

/// Phase-2 rung ordering (D7). `hybrid` = resolution down to 1080p, then fps 120→60,
/// then the deeper resolution rungs; the two pure orders are for A/B.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LadderOrder {
    Hybrid,
    ResFirst,
    FpsFirst,
}

impl LadderOrder {
    pub fn parse(s: &str) -> Option<Self> {
        match s.trim().to_ascii_lowercase().as_str() {
            "hybrid" => Some(LadderOrder::Hybrid),
            "res_first" => Some(LadderOrder::ResFirst),
            "fps_first" => Some(LadderOrder::FpsFirst),
            _ => None,
        }
    }
    pub fn as_str(self) -> &'static str {
        match self {
            LadderOrder::Hybrid => "hybrid",
            LadderOrder::ResFirst => "res_first",
            LadderOrder::FpsFirst => "fps_first",
        }
    }
}

/// The host's ladder knobs, resolved `env ← config_update` like every other
/// [`crate::session::settings::RuntimeSettings`] field. Snapshotted into
/// `SessionConfig` at assign time, so a live-class push applies to the NEXT session.
/// Replaces the previous env-only path, which made the ladder unreachable from the
/// admin console.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct LadderSettings {
    pub enabled: bool,
    pub max_bias: u8,
    pub engage_dwell: u8,
    pub recover_dwell: u8,
    pub resolution_enabled: bool,
    pub fps_enabled: bool,
    /// Amendment 5: whether the ABR floor follows the engaged rung ([`FloorFollow`]).
    /// Default **true** — with the ladder itself shipping dark this is dark too, and when
    /// the ladder IS on this is the behaviour that makes it useful.
    pub floor_follows_rung: bool,
    pub order: LadderOrder,
    pub res: ResolutionPolicy,
}

impl LadderSettings {
    pub fn from_env() -> Self {
        let base = LadderConfig::new().with_env_overrides();
        let p = ResolutionPolicy::new();
        Self {
            enabled: !matches!(
                std::env::var("QUASAR_ABR_LADDER").ok().as_deref(),
                Some("0") | Some("false") | Some("FALSE")
            ),
            max_bias: base.max_bias,
            engage_dwell: base.engage_dwell,
            recover_dwell: base.recover_dwell,
            resolution_enabled: env_flag("QUASAR_ABR_LADDER_RESOLUTION"),
            fps_enabled: env_flag("QUASAR_ABR_LADDER_FPS"),
            floor_follows_rung: env_flag_default_true("QUASAR_ABR_LADDER_FLOOR_FOLLOWS_RUNG"),
            order: std::env::var("QUASAR_ABR_LADDER_ORDER")
                .ok()
                .and_then(|s| LadderOrder::parse(&s))
                .unwrap_or(LadderOrder::Hybrid),
            res: ResolutionPolicy {
                exponent: env_f64("QUASAR_ABR_LADDER_RES_EXPONENT", p.exponent, 0.5, 1.0),
                engage_frac: env_f64(
                    "QUASAR_ABR_LADDER_RES_ENGAGE_FRAC",
                    p.engage_frac,
                    0.2,
                    0.95,
                ),
                recover_frac: env_f64(
                    "QUASAR_ABR_LADDER_RES_RECOVER_FRAC",
                    p.recover_frac,
                    0.3,
                    1.0,
                ),
                engage_dwell: env_u8("QUASAR_ABR_LADDER_RES_ENGAGE_DWELL", p.engage_dwell, 1),
                recover_dwell: env_u8("QUASAR_ABR_LADDER_RES_RECOVER_DWELL", p.recover_dwell, 1),
                min_step_s: env_u64("QUASAR_ABR_LADDER_RES_MIN_STEP_S", p.min_step_s, 5, 120),
                min_height: env_i32("QUASAR_ABR_LADDER_RES_MIN_HEIGHT", p.min_height, 360, 2160),
                settle_windows: p.settle_windows,
            },
        }
    }

    /// Sparse overlay from a `config_update` `settings` block. Unknown keys and
    /// type-mismatched values are ignored (the control plane already validated —
    /// this is defensive), matching `RuntimeSettings::apply_json`.
    pub fn apply_json(&mut self, v: &serde_json::Value) {
        if let Some(x) = v.get("abr_ladder").and_then(|x| x.as_bool()) {
            self.enabled = x;
        }
        if let Some(x) = v.get("abr_ladder_max_bias").and_then(|x| x.as_u64()) {
            self.max_bias = x.min(255) as u8;
        }
        if let Some(x) = v.get("abr_ladder_engage_dwell").and_then(|x| x.as_u64()) {
            self.engage_dwell = x.clamp(1, 255) as u8;
        }
        if let Some(x) = v.get("abr_ladder_recover_dwell").and_then(|x| x.as_u64()) {
            self.recover_dwell = x.clamp(1, 255) as u8;
        }
        if let Some(x) = v.get("abr_ladder_resolution").and_then(|x| x.as_bool()) {
            self.resolution_enabled = x;
        }
        if let Some(x) = v.get("abr_ladder_fps").and_then(|x| x.as_bool()) {
            self.fps_enabled = x;
        }
        if let Some(x) = v
            .get("abr_ladder_floor_follows_rung")
            .and_then(|x| x.as_bool())
        {
            self.floor_follows_rung = x;
        }
        if let Some(x) = v.get("abr_ladder_order").and_then(|x| x.as_str()) {
            if let Some(o) = LadderOrder::parse(x) {
                self.order = o;
            }
        }
        if let Some(x) = v.get("abr_ladder_res_exponent").and_then(|x| x.as_f64()) {
            if x.is_finite() && (0.5..=1.0).contains(&x) {
                self.res.exponent = x;
            }
        }
        if let Some(x) = v.get("abr_ladder_res_engage_frac").and_then(|x| x.as_f64()) {
            if x.is_finite() && (0.2..=0.95).contains(&x) {
                self.res.engage_frac = x;
            }
        }
        if let Some(x) = v
            .get("abr_ladder_res_recover_frac")
            .and_then(|x| x.as_f64())
        {
            if x.is_finite() && (0.3..=1.0).contains(&x) {
                self.res.recover_frac = x;
            }
        }
        if let Some(x) = v
            .get("abr_ladder_res_engage_dwell")
            .and_then(|x| x.as_u64())
        {
            self.res.engage_dwell = x.clamp(1, 60) as u8;
        }
        if let Some(x) = v
            .get("abr_ladder_res_recover_dwell")
            .and_then(|x| x.as_u64())
        {
            self.res.recover_dwell = x.clamp(1, 60) as u8;
        }
        if let Some(x) = v.get("abr_ladder_res_min_step_s").and_then(|x| x.as_u64()) {
            self.res.min_step_s = x.clamp(5, 120);
        }
        if let Some(x) = v.get("abr_ladder_res_min_height").and_then(|x| x.as_i64()) {
            self.res.min_height = x.clamp(360, 2160) as i32;
        }
    }

    /// Report every knob into the host-observability effective map, keyed by the
    /// hostcfg catalog names so the admin UI can join it against `resolved`.
    pub fn write_effective(&self, m: &mut std::collections::BTreeMap<String, String>) {
        m.insert("abr_ladder".into(), self.enabled.to_string());
        m.insert("abr_ladder_max_bias".into(), self.max_bias.to_string());
        m.insert(
            "abr_ladder_engage_dwell".into(),
            self.engage_dwell.to_string(),
        );
        m.insert(
            "abr_ladder_recover_dwell".into(),
            self.recover_dwell.to_string(),
        );
        m.insert(
            "abr_ladder_resolution".into(),
            self.resolution_enabled.to_string(),
        );
        m.insert("abr_ladder_fps".into(), self.fps_enabled.to_string());
        m.insert(
            "abr_ladder_floor_follows_rung".into(),
            self.floor_follows_rung.to_string(),
        );
        m.insert("abr_ladder_order".into(), self.order.as_str().to_string());
        m.insert(
            "abr_ladder_res_exponent".into(),
            self.res.exponent.to_string(),
        );
        m.insert(
            "abr_ladder_res_engage_frac".into(),
            self.res.engage_frac.to_string(),
        );
        m.insert(
            "abr_ladder_res_recover_frac".into(),
            self.res.recover_frac.to_string(),
        );
        m.insert(
            "abr_ladder_res_engage_dwell".into(),
            self.res.engage_dwell.to_string(),
        );
        m.insert(
            "abr_ladder_res_recover_dwell".into(),
            self.res.recover_dwell.to_string(),
        );
        m.insert(
            "abr_ladder_res_min_step_s".into(),
            self.res.min_step_s.to_string(),
        );
        m.insert(
            "abr_ladder_res_min_height".into(),
            self.res.min_height.to_string(),
        );
    }

    pub fn to_config(&self) -> LadderConfig {
        LadderConfig {
            max_bias: self.max_bias,
            engage_dwell: self.engage_dwell,
            recover_dwell: self.recover_dwell,
            fps_enabled: self.fps_enabled,
            resolution_enabled: self.resolution_enabled,
            floor_follows_rung: self.floor_follows_rung,
            res: self.res,
            order: self.order,
        }
    }
}

impl Default for LadderSettings {
    fn default() -> Self {
        Self::from_env()
    }
}

fn env_flag(var: &str) -> bool {
    matches!(
        std::env::var(var).ok().as_deref(),
        Some("1") | Some("true") | Some("TRUE")
    )
}

/// A boolean env knob whose default is **on** — only an explicit `0`/`false` turns it off
/// (the `QUASAR_ABR_LADDER` convention, not `env_flag`'s opt-in one).
fn env_flag_default_true(var: &str) -> bool {
    !matches!(
        std::env::var(var).ok().as_deref(),
        Some("0") | Some("false") | Some("FALSE")
    )
}

fn env_f64(var: &str, default: f64, lo: f64, hi: f64) -> f64 {
    match std::env::var(var).ok().as_deref().map(str::trim) {
        None | Some("") => default,
        Some(raw) => match raw.parse::<f64>() {
            Ok(v) if v.is_finite() && (lo..=hi).contains(&v) => v,
            _ => {
                tracing::warn!(
                    token = "knob-invalid-ladder-f64",
                    "{var}={raw:?} is not a valid value (f64 in {lo}..={hi}); using {default}"
                );
                default
            }
        },
    }
}

fn env_u64(var: &str, default: u64, lo: u64, hi: u64) -> u64 {
    match std::env::var(var).ok().as_deref().map(str::trim) {
        None | Some("") => default,
        Some(raw) => match raw.parse::<u64>() {
            Ok(v) if (lo..=hi).contains(&v) => v,
            _ => {
                tracing::warn!(
                    token = "knob-invalid-ladder-u64",
                    "{var}={raw:?} is not a valid value (u64 in {lo}..={hi}); using {default}"
                );
                default
            }
        },
    }
}

fn env_i32(var: &str, default: i32, lo: i32, hi: i32) -> i32 {
    match std::env::var(var).ok().as_deref().map(str::trim) {
        None | Some("") => default,
        Some(raw) => match raw.parse::<i32>() {
            Ok(v) if (lo..=hi).contains(&v) => v,
            _ => {
                tracing::warn!(
                    token = "knob-invalid-ladder-i32",
                    "{var}={raw:?} is not a valid value (i32 in {lo}..={hi}); using {default}"
                );
                default
            }
        },
    }
}

/// Ladder tuning. Default is the safe shippable posture: the encoder-speed-bias
/// rung is active (the only renegotiation-free escalation), fps and resolution
/// rungs are gated OFF and not actuated.
#[derive(Debug, Clone, Copy)]
pub struct LadderConfig {
    /// Max speed-bias rungs. Each rung biases the encoder one step faster. 0 ⇒ the
    /// preset rung is disabled (the ladder is inert). Default 2 (two escalation
    /// steps before the encoder is at its fastest realistic setting).
    pub max_bias: u8,
    /// Consecutive `EncoderSaturated` windows required before stepping a rung UP.
    /// >1 so a single blip doesn't bias the encoder.
    pub engage_dwell: u8,
    /// Consecutive `Healthy` windows required before stepping a rung DOWN (restoring
    /// quality). Longer than `engage_dwell` so recovery is conservative (no flap).
    pub recover_dwell: u8,
    /// DEFERRED rung 3: framerate drop. Default false — not actuated (see module docs).
    pub fps_enabled: bool,
    /// DEFERRED rung 4: resolution drop. Default false — not actuated (see module docs).
    pub resolution_enabled: bool,
    /// Amendment 5: whether the ABR floor follows the engaged rung ([`FloorFollow`]).
    /// Inert unless a rung is actually engaged, so `true` is safe as the default.
    pub floor_follows_rung: bool,
    /// D2 policy for the resolution rung (rung 4). Inert unless `resolution_enabled`.
    pub res: ResolutionPolicy,
    /// D7 rung ordering. Only meaningful once the fps rung exists (phase 2).
    pub order: LadderOrder,
}

impl LadderConfig {
    pub const DEFAULT_MAX_BIAS: u8 = 2;
    pub const DEFAULT_ENGAGE_DWELL: u8 = 2;
    /// 4 → 2 (with `ResolutionPolicy::DEFAULT_RECOVER_DWELL`) cut time-to-launch-rung
    /// 55.2s → 37.6s, 0 oscillations, roughly halved recovery-window client-present
    /// drops (12.2% → 5.7%): `docs/reports/2026-08-18-overnight-optimisation/t3-ladder-step-recovery.md`.
    pub const DEFAULT_RECOVER_DWELL: u8 = 2;

    /// The safe default: speed-bias rung on (max 2 steps), fps + resolution off.
    pub fn new() -> Self {
        Self {
            max_bias: Self::DEFAULT_MAX_BIAS,
            engage_dwell: Self::DEFAULT_ENGAGE_DWELL,
            recover_dwell: Self::DEFAULT_RECOVER_DWELL,
            fps_enabled: false,
            resolution_enabled: false,
            floor_follows_rung: true,
            res: ResolutionPolicy::new(),
            order: LadderOrder::Hybrid,
        }
    }

    /// Overlay the operator's ladder-hysteresis env knobs onto this config, in place.
    /// Every default equals the value it replaced, so all-vars-unset is a no-op.
    /// `fps_enabled`/`resolution_enabled` are set by the caller and untouched here.
    /// Knobs: `QUASAR_ABR_LADDER_MAX_BIAS` [0,255] (0 = inert),
    /// `QUASAR_ABR_LADDER_ENGAGE_DWELL` [1,255], `QUASAR_ABR_LADDER_RECOVER_DWELL` [1,255].
    /// An unparseable value warns once and falls back to the default.
    pub fn with_env_overrides(mut self) -> Self {
        // max_bias may be 0 (a valid disable of the rung); engage/recover must be >= 1
        // (a 0 dwell would step every window, defeating the hysteresis).
        self.max_bias = env_u8("QUASAR_ABR_LADDER_MAX_BIAS", self.max_bias, 0);
        self.engage_dwell = env_u8("QUASAR_ABR_LADDER_ENGAGE_DWELL", self.engage_dwell, 1);
        self.recover_dwell = env_u8("QUASAR_ABR_LADDER_RECOVER_DWELL", self.recover_dwell, 1);
        self
    }
}

/// Parse a `u8` env var that must be `>= min`. Junk / out-of-range (incl. > 255) WARNs
/// once and returns `default`. A trimmed-empty value is treated as unset (silent).
fn env_u8(var: &str, default: u8, min: u8) -> u8 {
    match std::env::var(var).ok().as_deref().map(str::trim) {
        None | Some("") => default,
        Some(raw) => match raw.parse::<u8>() {
            Ok(v) if v >= min => v,
            _ => {
                tracing::warn!(
                    token = "knob-invalid-ladder-u8",
                    "{var}={raw:?} is not a valid value (u8 >= {min}); using default {default}"
                );
                default
            }
        },
    }
}

impl Default for LadderConfig {
    fn default() -> Self {
        Self::new()
    }
}

/// The ladder decision machine. Fed one [`AdaptationState`] per metrics window;
/// emits a new speed-bias level when (and only when) a rung change is warranted.
///
/// Bias level 0 = baseline (the configured encoder quality posture). Each level up
/// biases the encoder one step faster (the glue maps the level onto the encoder's
/// native property — VA `target-usage`, NVENC `preset`). The glue is responsible
/// for the actual live write; this module only decides the level.
#[derive(Debug)]
pub struct Ladder {
    cfg: LadderConfig,
    /// Current speed-bias rung (0..=cfg.max_bias).
    bias: u8,
    /// Consecutive windows the encoder has been saturated (reset by any non-saturated window).
    saturated_run: u8,
    /// Consecutive windows the stream has been healthy (reset by any non-healthy window).
    healthy_run: u8,
}

impl Ladder {
    pub fn new(cfg: LadderConfig) -> Self {
        Self {
            cfg,
            bias: 0,
            saturated_run: 0,
            healthy_run: 0,
        }
    }

    /// The current speed-bias rung (0 = baseline). Exposed for telemetry/tests.
    pub fn bias_level(&self) -> u8 {
        self.bias
    }

    /// Observe one window's [`AdaptationState`]. Returns `Some(new_bias_level)` when
    /// the speed-bias rung changed this window (the glue should apply it), else
    /// `None` (hold the current rung). Pure, cheap arithmetic — runs once per drain.
    ///
    /// Rung selection:
    /// - `EncoderSaturated`: count toward `engage_dwell`; once reached (and below
    ///   `max_bias`), step the bias UP one rung and reset the engage counter so the
    ///   next step needs another full dwell (escalation is paced, not a burst).
    /// - `Healthy`: count toward `recover_dwell`; once reached (and above 0), step
    ///   the bias DOWN one rung and reset the recover counter (restore is paced too).
    /// - `NetworkCongested` / `Unknown`: neither escalate nor recover — hold the rung
    ///   and reset BOTH dwell counters. Congestion is the governor's job (rung 1);
    ///   the ladder must not unwind an encoder bias just because the *network* dipped,
    ///   nor escalate the encoder for a network problem.
    pub fn observe(&mut self, state: AdaptationState) -> Option<u8> {
        // The speed-bias rung is inert if max_bias is 0 (operator disabled it).
        if self.cfg.max_bias == 0 {
            return None;
        }
        match state {
            AdaptationState::EncoderSaturated => {
                self.healthy_run = 0;
                self.saturated_run = self.saturated_run.saturating_add(1);
                if self.saturated_run >= self.cfg.engage_dwell && self.bias < self.cfg.max_bias {
                    self.bias += 1;
                    self.saturated_run = 0; // pace the next escalation
                    return Some(self.bias);
                }
                None
            }
            AdaptationState::Healthy => {
                self.saturated_run = 0;
                self.healthy_run = self.healthy_run.saturating_add(1);
                if self.healthy_run >= self.cfg.recover_dwell && self.bias > 0 {
                    self.bias -= 1;
                    self.healthy_run = 0; // pace the next recovery step
                    return Some(self.bias);
                }
                None
            }
            // Network congestion or an unclassifiable window: hold the encoder rung
            // and reset both runs so a saturated/healthy *streak* must be contiguous.
            AdaptationState::NetworkCongested | AdaptationState::Unknown => {
                self.saturated_run = 0;
                self.healthy_run = 0;
                None
            }
        }
    }
}

use std::time::Instant;

/// D1: the comfort bitrate `B(r)` — the bitrate below which rung `r` starts to look
/// bad. **Not a bandwidth cap**: bandwidth is the governor's lever (rung 1). This is
/// the bits-per-pixel power law, `B(r) = ceiling × (px_r / px_0)^k`, so `B(0) =
/// ceiling` exactly. `k` (`res_exponent`, default 0.75) is the empirical exponent for
/// H.264/HEVC/AV1 at fixed fps; it is an operator knob because the true value moves
/// with content and codec.
pub fn comfort_kbps(ceiling_kbps: u32, launch_px: i64, rung_px: i64, k: f64) -> f64 {
    if launch_px <= 0 || rung_px <= 0 {
        return ceiling_kbps as f64;
    }
    let ratio = rung_px as f64 / launch_px as f64;
    ceiling_kbps as f64 * ratio.powf(k)
}

/// The ABR floor follows the rung.
///
/// ## Why (measured, run C — `docs/reports/2026-08-16-abr-ladder/VALIDATION.md`)
/// A 1080p120 h264 session at an 11.5 Mbps ceiling had an ABR floor of ~3450 kbps.
/// Behind a 3.5 Mbps netem cap the governor bottomed out at ~4.4 Mbps, above the link,
/// so the encoder flooded the pipe for the whole window (83 packets lost, 107 ms jitter
/// buffer, ~6 fps at the browser). The ladder stepped to 720p60 as designed and it
/// changed nothing: a resolution rung changes bits per pixel, not bandwidth. The rung
/// says "this picture is comfortable on less"; something has to let the governor go there.
///
/// ## What
/// While a rung is engaged the floor scales by the same factor the rung scales the
/// comfort bitrate by: `floor_eff = launch_floor × (B_eff / ceiling)`, `B_eff = B(r) ×
/// (0.6 if fps is down)`, with `B(r)` the [`comfort_kbps`] power law. At the launch rung
/// `B_eff == ceiling`, so the floor is exactly today's — inert with the ladder off or
/// parked at rung 0 (asserted in tests).
///
/// ## Why the launch floor, not `abr_floor_ratio × B_eff`
/// The run-C floor was NOT the ratio-derived value — the `1080p120-h264` profile carries
/// its own `abr_floor_kbps = 4000` on the wire. `ratio × B_eff` would ignore the
/// profile's number; treating that number as an absolute bound would make this mechanism
/// inert on exactly the session it exists to fix. Scaling the launch floor handles both:
/// a profile floor is the floor for the picture the profile describes, so it travels
/// with the rung, and when the floor IS ratio-derived this reduces algebraically to
/// `ratio × B_eff` anyway.
///
/// ## Bounds
/// - `HARD_MIN_KBPS` — never below 300 kbps.
/// - An operator floor (host setting / `QUASAR_ABR_FLOOR_KBPS`) is an absolute lower
///   bound: it always wins.
/// - Never above the launch floor.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct FloorFollow {
    /// `abr_ladder_floor_follows_rung`. `false` ⇒ [`Self::floor_for`] always returns the
    /// launch floor (pre-amendment behaviour).
    pub enabled: bool,
    /// The floor the session launched with — `SessionConfig::abr_config().floor_kbps`.
    pub launch_floor_kbps: u32,
    /// `Some` ONLY for a host/env-level operator floor (`QUASAR_ABR_FLOOR_KBPS` or the
    /// `abr_floor_kbps` host setting). A per-profile wire floor is deliberately NOT this.
    pub operator_floor_kbps: Option<u32>,
    /// The session's configured ceiling (the launch bitrate).
    pub ceiling_kbps: u32,
    /// Pixels at the launch rung — `comfort_kbps`'s `B(0) = ceiling` normaliser.
    pub launch_px: i64,
    /// `abr_ladder_res_exponent` (`k`), shared with the rung's own comfort curve.
    pub exponent: f64,
}

impl FloorFollow {
    /// The floor never goes below this, whatever the rung arithmetic produces. Below a few
    /// hundred kbps the stream is not a stream, and letting GCC drag the encoder there
    /// trades one unusable picture for another.
    pub const HARD_MIN_KBPS: u32 = 300;

    /// The effective floor for a rung of `rung_px` pixels, with the fps rung engaged or
    /// not. Pure arithmetic — no clock, no state.
    pub fn floor_for(&self, rung_px: i64, fps_engaged: bool) -> u32 {
        if !self.enabled || self.ceiling_kbps == 0 {
            return self.launch_floor_kbps;
        }
        let mut b_eff = comfort_kbps(self.ceiling_kbps, self.launch_px, rung_px, self.exponent);
        if fps_engaged {
            b_eff *= FpsRung::HALF_RATE_BITRATE_RATIO;
        }
        // The rung's scale factor, applied to whatever the session's launch floor was.
        let scale = (b_eff / self.ceiling_kbps as f64).clamp(0.0, 1.0);
        let follow = (self.launch_floor_kbps as f64 * scale).round() as u32;
        let mut floor = follow.max(Self::HARD_MIN_KBPS);
        if let Some(operator) = self.operator_floor_kbps {
            floor = floor.max(operator);
        }
        // Never above the launch floor — see the struct doc's third bound.
        floor.min(self.launch_floor_kbps).max(1)
    }
}

/// D2 tuning for the resolution rung. Every default is the spec's.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct ResolutionPolicy {
    /// `k` in `B(r) = ceiling × (px_r/px_0)^k`.
    pub exponent: f64,
    /// Step DOWN when `setpoint < engage_frac × B(current)`.
    pub engage_frac: f64,
    /// Step UP when `setpoint ≥ recover_frac × B(current-1)`.
    pub recover_frac: f64,
    /// Consecutive qualifying windows before a down-step.
    pub engage_dwell: u8,
    /// Consecutive qualifying windows before an up-step (longer — recovery is conservative).
    pub recover_dwell: u8,
    /// Minimum seconds between two resolution steps, in either direction.
    pub min_step_s: u64,
    /// Never step to a rung shorter than this.
    pub min_height: i32,
    /// Windows to ignore entirely after a step (the IDR's σ spike must not feed the
    /// next decision).
    pub settle_windows: u8,
}

impl ResolutionPolicy {
    pub const DEFAULT_EXPONENT: f64 = 0.75;
    pub const DEFAULT_ENGAGE_FRAC: f64 = 0.6;
    pub const DEFAULT_RECOVER_FRAC: f64 = 0.8;
    pub const DEFAULT_ENGAGE_DWELL: u8 = 2;
    /// 4 → 2, alongside `LadderConfig::DEFAULT_RECOVER_DWELL` — see that constant's doc
    /// for the measured gain (this is the resolution-rung half of the same arm).
    pub const DEFAULT_RECOVER_DWELL: u8 = 2;
    pub const DEFAULT_MIN_STEP_S: u64 = 10;
    pub const DEFAULT_MIN_HEIGHT: i32 = 720;
    pub const DEFAULT_SETTLE_WINDOWS: u8 = 1;

    pub fn new() -> Self {
        Self {
            exponent: Self::DEFAULT_EXPONENT,
            engage_frac: Self::DEFAULT_ENGAGE_FRAC,
            recover_frac: Self::DEFAULT_RECOVER_FRAC,
            engage_dwell: Self::DEFAULT_ENGAGE_DWELL,
            recover_dwell: Self::DEFAULT_RECOVER_DWELL,
            min_step_s: Self::DEFAULT_MIN_STEP_S,
            min_height: Self::DEFAULT_MIN_HEIGHT,
            settle_windows: Self::DEFAULT_SETTLE_WINDOWS,
        }
    }

    /// Reject a config that could pump. Field ranges first, then the spec's explicit
    /// hysteresis floor (`recover_frac − engage_frac ≥ 0.05`, checked server-side and
    /// here at ladder build), then the band check for every adjacent pair of this
    /// session's rungs: `engage_frac × B(r) < recover_frac × B(r-1)`.
    ///
    /// With monotonic `B` the per-pair check reduces to `recover_frac > engage_frac`, a
    /// strictly weaker condition than the 0.05 floor, so the floor check is what rejects
    /// a too-tight-but-non-inverted band (e.g. 0.61/0.63). The per-pair check stays to
    /// catch a pathological exponent or non-monotonic rung list here rather than live.
    /// The control plane enforces the same floor (`hostcfg.ValidateResolved`).
    pub fn validate(&self, rungs: &[(i32, i32)], ceiling_kbps: u32) -> Result<(), String> {
        if !(0.5..=1.0).contains(&self.exponent) {
            return Err(format!("exponent {} outside 0.5..=1.0", self.exponent));
        }
        if !(0.2..=0.95).contains(&self.engage_frac) {
            return Err(format!(
                "engage_frac {} outside 0.2..=0.95",
                self.engage_frac
            ));
        }
        if !(0.3..=1.0).contains(&self.recover_frac) {
            return Err(format!(
                "recover_frac {} outside 0.3..=1.0",
                self.recover_frac
            ));
        }
        if self.engage_dwell == 0 {
            return Err("engage_dwell must be >= 1".to_string());
        }
        if self.recover_dwell == 0 {
            return Err("recover_dwell must be >= 1".to_string());
        }
        if self.min_height < 360 {
            return Err(format!("min_height {} below 360", self.min_height));
        }
        // A 0.05 gap is required even when fracs are correctly ordered — a too-tight
        // gap can still pump under noisy setpoints without literally inverting.
        let gap = self.recover_frac - self.engage_frac;
        if gap < 0.05 {
            return Err(format!(
                "recover_frac ({}) − engage_frac ({}) = {:.3} is below the 0.05 hysteresis floor (spec D2)",
                self.recover_frac, self.engage_frac, gap
            ));
        }
        // Engage and recover thresholds are fractions of the same B (the higher rung's
        // comfort bitrate), so the band-collapse condition per pair reduces to
        // `engage_frac >= recover_frac` — written out per-pair, not hoisted to one
        // scalar check, so a degenerate/non-monotonic rung list is still caught here.
        let px = |r: &(i32, i32)| r.0 as i64 * r.1 as i64;
        let launch_px = rungs.first().map(px).unwrap_or(0);
        for pair in rungs.windows(2) {
            let b = comfort_kbps(ceiling_kbps, launch_px, px(&pair[0]), self.exponent);
            if self.engage_frac * b >= self.recover_frac * b {
                return Err(format!(
                    "hysteresis band collapses between {}x{} and {}x{}: \
                     engage_frac ({}) × B = {:.0} >= recover_frac ({}) × B = {:.0}",
                    pair[0].0,
                    pair[0].1,
                    pair[1].0,
                    pair[1].1,
                    self.engage_frac,
                    self.engage_frac * b,
                    self.recover_frac,
                    self.recover_frac * b
                ));
            }
        }
        Ok(())
    }
}

impl Default for ResolutionPolicy {
    fn default() -> Self {
        Self::new()
    }
}

/// Why a step happened — carried into the log line, the trace event, and the report.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StepReason {
    /// Sustained low setpoint (the normal down path).
    Engage,
    /// Sustained recovered setpoint (the normal up path).
    Recover,
    /// The governor is already at the floor and the path is still congested.
    Emergency,
}

impl StepReason {
    pub fn as_str(self) -> &'static str {
        match self {
            StepReason::Engage => "engage",
            StepReason::Recover => "recover",
            StepReason::Emergency => "emergency",
        }
    }
}

/// One resolution step, by rung index into the session's ladder (0 = launch).
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct StepDecision {
    pub from: usize,
    pub to: usize,
    pub reason: StepReason,
}

/// SPT-08 rung 4, actuated: the resolution decision machine. Pure — it decides an
/// index; `pipeline::abr_glue` turns that into one `lever.set_rung(w, h)` call.
#[derive(Debug)]
pub struct ResolutionRung {
    policy: ResolutionPolicy,
    /// The session's ladder, descending, index 0 = launch (mirrors `session::rungs`).
    rungs: Vec<(i32, i32)>,
    /// The deepest index the policy's `min_height` allows.
    floor_rung: usize,
    ceiling_kbps: u32,
    launch_px: i64,
    current: usize,
    engage_run: u8,
    recover_run: u8,
    settle_left: u8,
    last_step: Instant,
    pinned: bool,
}

impl ResolutionRung {
    pub fn new(
        policy: ResolutionPolicy,
        rungs: Vec<(i32, i32)>,
        ceiling_kbps: u32,
        now: Instant,
    ) -> Self {
        let launch_px = rungs.first().map(|r| r.0 as i64 * r.1 as i64).unwrap_or(0);
        // The deepest legal index: the last rung at or above min_height. Always >= 0.
        let floor_rung = rungs
            .iter()
            .enumerate()
            .filter(|(_, (_, h))| *h >= policy.min_height)
            .map(|(i, _)| i)
            .next_back()
            .unwrap_or(0);
        Self {
            policy,
            rungs,
            floor_rung,
            ceiling_kbps,
            launch_px,
            current: 0,
            engage_run: 0,
            recover_run: 0,
            settle_left: 0,
            // Start "already settled" so the first window can act (a session that
            // launches into a congested path must not wait out a phantom interval).
            last_step: now - std::time::Duration::from_secs(policy.min_step_s + 1),
            pinned: false,
        }
    }

    pub fn rung(&self) -> usize {
        self.current
    }

    /// Total: an empty rung list (a malformed/unconfigured ladder) reports `(0, 0)`
    /// rather than panicking on the `len() - 1` underflow a bare index would hit.
    pub fn size(&self) -> (i32, i32) {
        self.rungs
            .get(self.current)
            .or_else(|| self.rungs.last())
            .copied()
            .unwrap_or((0, 0))
    }

    /// D4: while pinned the machine observes (counters keep advancing coherently) but
    /// never emits — a human chose the size and the ladder must not fight them.
    pub fn set_pinned(&mut self, pinned: bool) {
        self.pinned = pinned;
    }

    /// Adopt a size set outside the machine (a manual PATCH, or the runner's echo).
    /// An unknown size is ignored rather than fatal.
    pub fn sync_rung(&mut self, size: (i32, i32)) {
        if let Some(i) = self.rungs.iter().position(|r| *r == size) {
            self.current = i;
            self.engage_run = 0;
            self.recover_run = 0;
        }
    }

    fn comfort(&self, idx: usize) -> f64 {
        let (w, h) = self.rungs[idx];
        comfort_kbps(
            self.ceiling_kbps,
            self.launch_px,
            w as i64 * h as i64,
            self.policy.exponent,
        )
    }

    /// What this window's signal asks for, with no dwell/settle/commit bookkeeping — a
    /// pure read of the thresholds. Extracted so [`Self::observe`] (mutating) and
    /// [`Self::wants`] ([`LadderPlanner`]'s direction-only query) cannot drift.
    fn intent(&self, state: AdaptationState, setpoint_kbps: f64, floor_kbps: f64) -> Intent {
        if self.rungs.len() < 2 || matches!(state, AdaptationState::Unknown) {
            return Intent::Hold;
        }
        // Emergency: bitrate cannot go lower and the path is still congested.
        let at_floor = setpoint_kbps <= floor_kbps * 1.05;
        if matches!(state, AdaptationState::NetworkCongested)
            && at_floor
            && self.current < self.floor_rung
        {
            return Intent::Emergency;
        }
        let engage_at = self.policy.engage_frac * self.comfort(self.current);
        if setpoint_kbps < engage_at && self.current < self.floor_rung {
            return Intent::Engage;
        }
        if self.current > 0 {
            let recover_at = self.policy.recover_frac * self.comfort(self.current - 1);
            if setpoint_kbps >= recover_at {
                return Intent::Recover;
            }
        }
        Intent::Hold
    }

    /// The direction this window would move the rung, ignoring dwell/settle/pin. `None`
    /// ⇒ the rung is in its band (or has no ladder). See [`Self::intent`].
    pub fn wants(
        &self,
        state: AdaptationState,
        setpoint_kbps: f64,
        floor_kbps: f64,
    ) -> Option<StepDir> {
        self.intent(state, setpoint_kbps, floor_kbps).direction()
    }

    /// Whether the rung has anywhere left to go in `dir` (pure — a pin or dwell can
    /// still hold it). [`LadderPlanner`] falls through to the other lever only when
    /// this one is exhausted, never merely because it returned `None` (that would
    /// defeat the dwell pacing).
    pub fn can_step(&self, dir: StepDir) -> bool {
        if self.rungs.len() < 2 {
            return false;
        }
        match dir {
            StepDir::Down => self.current < self.floor_rung,
            StepDir::Up => self.current > 0,
        }
    }

    /// One window. Returns `Some(decision)` when the rung must move.
    ///
    /// Order of gates (each is load-bearing):
    /// 1. **Settle** — swallow the window entirely after a step (the IDR spike).
    /// 2. **Emergency** — congested AND the governor is pinned at its floor ⇒ step now.
    /// 3. **Dwell** — normal down/up paths need contiguous qualifying windows.
    /// 4. **min_step_s** — never two steps closer than the configured interval.
    pub fn observe(
        &mut self,
        state: AdaptationState,
        setpoint_kbps: f64,
        floor_kbps: f64,
        now: Instant,
    ) -> Option<StepDecision> {
        if self.rungs.len() < 2 {
            return None; // no ladder for this session
        }
        if self.settle_left > 0 {
            self.settle_left -= 1;
            return None;
        }
        // `Unknown` carries no information — hold and drop both streaks so a run must
        // be contiguous, even while pinned.
        if matches!(state, AdaptationState::Unknown) {
            self.engage_run = 0;
            self.recover_run = 0;
            return None;
        }

        let can_step = now.duration_since(self.last_step).as_secs() >= self.policy.min_step_s;

        // While pinned, every branch still runs its normal dwell-counter bookkeeping
        // (`self.pinned` is checked only at each commit point) — a human owns the size
        // so the machine must never move it, but counters stay live so a release
        // resumes coherently.
        match self.intent(state, setpoint_kbps, floor_kbps) {
            Intent::Emergency => {
                if self.pinned || !can_step {
                    return None;
                }
                Some(self.step_to(self.current + 1, StepReason::Emergency, now))
            }
            Intent::Engage => {
                self.recover_run = 0;
                self.engage_run = self.engage_run.saturating_add(1);
                if self.engage_run >= self.policy.engage_dwell {
                    if self.pinned || !can_step {
                        return None;
                    }
                    self.engage_run = 0;
                    return Some(self.step_to(self.current + 1, StepReason::Engage, now));
                }
                None
            }
            Intent::Recover => {
                self.engage_run = 0;
                self.recover_run = self.recover_run.saturating_add(1);
                if self.recover_run >= self.policy.recover_dwell {
                    if self.pinned || !can_step {
                        return None;
                    }
                    self.recover_run = 0;
                    return Some(self.step_to(self.current - 1, StepReason::Recover, now));
                }
                None
            }
            // In the band: neither low enough to engage nor high enough to recover.
            Intent::Hold => {
                self.engage_run = 0;
                self.recover_run = 0;
                None
            }
        }
    }

    /// Commit a step, arming the settle window and the min-interval clock. Returns
    /// `None`-equivalent to the caller only via `observe`'s pinned check below.
    fn step_to(&mut self, to: usize, reason: StepReason, now: Instant) -> StepDecision {
        let from = self.current;
        self.current = to;
        self.settle_left = self.policy.settle_windows;
        self.last_step = now;
        StepDecision { from, to, reason }
    }
}

/// Which way a window would move a rung. "Down" is always toward the *cheaper, worse*
/// end (fewer pixels / fewer frames), matching the rung indices.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StepDir {
    Down,
    Up,
}

/// The pure per-window classification shared by [`ResolutionRung`] and [`FpsRung`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Intent {
    Emergency,
    Engage,
    Recover,
    Hold,
}

impl Intent {
    fn direction(self) -> Option<StepDir> {
        match self {
            Intent::Emergency | Intent::Engage => Some(StepDir::Down),
            Intent::Recover => Some(StepDir::Up),
            Intent::Hold => None,
        }
    }
}

/// The **frame-rate** rung — the same decision machine as [`ResolutionRung`], over a
/// two-entry frame-rate ladder instead of a size ladder.
///
/// The ladder is `[launch_fps, launch_fps / 2]` and exists only when launch fps admits a
/// halving (`>= 120`). A 60 fps session has a one-entry ladder and is inert — halving an
/// already-60 fps stream is a bigger perceptual hit than any resolution rung.
///
/// Comfort curve: halving the frame rate cuts bitrate need to ≈0.6× (not 0.5× —
/// inter-frame prediction gets worse as the temporal gap grows). `B_fps(launch) =
/// ceiling`, `B_fps(launch/2) = ceiling × 0.6`. Exponent/fraction/dwell knobs are shared
/// verbatim with [`ResolutionPolicy`] — one hysteresis to tune, not two.
///
/// `StepDecision::from`/`to` carry the frame rates themselves (120/60), not indices —
/// unlike the resolution rung. Lands in the `abr.ladder.step` trace event and the
/// `ladder_fps` metric.
#[derive(Debug)]
pub struct FpsRung {
    policy: ResolutionPolicy,
    /// Descending frame rates, index 0 = launch. Length 1 ⇒ the rung is inert.
    steps: Vec<i32>,
    ceiling_kbps: u32,
    current: usize,
    engage_run: u8,
    recover_run: u8,
    settle_left: u8,
    last_step: Instant,
    pinned: bool,
}

impl FpsRung {
    /// The D7 comfort ratio for a halved frame rate: bitrate need × 0.6.
    pub const HALF_RATE_BITRATE_RATIO: f64 = 0.6;
    /// The lowest launch frame rate that admits an fps rung. Below this there is nothing
    /// to halve to that the ladder is willing to ship.
    pub const MIN_LAUNCH_FPS: i32 = 120;

    pub fn new(policy: ResolutionPolicy, launch_fps: i32, ceiling_kbps: u32, now: Instant) -> Self {
        let steps = if launch_fps >= Self::MIN_LAUNCH_FPS {
            vec![launch_fps, launch_fps / 2]
        } else {
            vec![launch_fps.max(1)]
        };
        Self {
            policy,
            steps,
            ceiling_kbps,
            current: 0,
            engage_run: 0,
            recover_run: 0,
            settle_left: 0,
            // Start "already settled", mirroring ResolutionRung.
            last_step: now - std::time::Duration::from_secs(policy.min_step_s + 1),
            pinned: false,
        }
    }

    /// The frame rate the rung currently asks for.
    pub fn fps(&self) -> i32 {
        self.steps.get(self.current).copied().unwrap_or(0)
    }

    /// The rung index (0 = launch fps).
    pub fn rung(&self) -> usize {
        self.current
    }

    /// Whether this session has an fps rung at all (a 60 fps session does not).
    pub fn is_active(&self) -> bool {
        self.steps.len() >= 2
    }

    pub fn set_pinned(&mut self, pinned: bool) {
        self.pinned = pinned;
    }

    fn comfort(&self, idx: usize) -> f64 {
        let base = self.ceiling_kbps as f64;
        if idx == 0 {
            base
        } else {
            base * Self::HALF_RATE_BITRATE_RATIO
        }
    }

    fn intent(&self, state: AdaptationState, setpoint_kbps: f64, floor_kbps: f64) -> Intent {
        if !self.is_active() || matches!(state, AdaptationState::Unknown) {
            return Intent::Hold;
        }
        let floor_rung = self.steps.len() - 1;
        let at_floor = setpoint_kbps <= floor_kbps * 1.05;
        if matches!(state, AdaptationState::NetworkCongested)
            && at_floor
            && self.current < floor_rung
        {
            return Intent::Emergency;
        }
        if setpoint_kbps < self.policy.engage_frac * self.comfort(self.current)
            && self.current < floor_rung
        {
            return Intent::Engage;
        }
        if self.current > 0
            && setpoint_kbps >= self.policy.recover_frac * self.comfort(self.current - 1)
        {
            return Intent::Recover;
        }
        Intent::Hold
    }

    pub fn wants(
        &self,
        state: AdaptationState,
        setpoint_kbps: f64,
        floor_kbps: f64,
    ) -> Option<StepDir> {
        self.intent(state, setpoint_kbps, floor_kbps).direction()
    }

    pub fn can_step(&self, dir: StepDir) -> bool {
        if !self.is_active() {
            return false;
        }
        match dir {
            StepDir::Down => self.current < self.steps.len() - 1,
            StepDir::Up => self.current > 0,
        }
    }

    /// One window — the same gate order as [`ResolutionRung::observe`].
    pub fn observe(
        &mut self,
        state: AdaptationState,
        setpoint_kbps: f64,
        floor_kbps: f64,
        now: Instant,
    ) -> Option<StepDecision> {
        if !self.is_active() {
            return None;
        }
        if self.settle_left > 0 {
            self.settle_left -= 1;
            return None;
        }
        if matches!(state, AdaptationState::Unknown) {
            self.engage_run = 0;
            self.recover_run = 0;
            return None;
        }
        let can_step = now.duration_since(self.last_step).as_secs() >= self.policy.min_step_s;
        match self.intent(state, setpoint_kbps, floor_kbps) {
            Intent::Emergency => {
                if self.pinned || !can_step {
                    return None;
                }
                Some(self.step_to(self.current + 1, StepReason::Emergency, now))
            }
            Intent::Engage => {
                self.recover_run = 0;
                self.engage_run = self.engage_run.saturating_add(1);
                if self.engage_run >= self.policy.engage_dwell {
                    if self.pinned || !can_step {
                        return None;
                    }
                    self.engage_run = 0;
                    return Some(self.step_to(self.current + 1, StepReason::Engage, now));
                }
                None
            }
            Intent::Recover => {
                self.engage_run = 0;
                self.recover_run = self.recover_run.saturating_add(1);
                if self.recover_run >= self.policy.recover_dwell {
                    if self.pinned || !can_step {
                        return None;
                    }
                    self.recover_run = 0;
                    return Some(self.step_to(self.current - 1, StepReason::Recover, now));
                }
                None
            }
            Intent::Hold => {
                self.engage_run = 0;
                self.recover_run = 0;
                None
            }
        }
    }

    /// Commit. `from`/`to` are frame RATES, not indices (see the struct doc).
    fn step_to(&mut self, to: usize, reason: StepReason, now: Instant) -> StepDecision {
        let from_fps = self.fps();
        self.current = to;
        self.settle_left = self.policy.settle_windows;
        self.last_step = now;
        StepDecision {
            from: from_fps as usize,
            to: self.fps() as usize,
            reason,
        }
    }
}

/// One step, on one lever. The planner never returns two in the same window: a window
/// that moved both levers at once is unreadable in the telemetry and doubles the σ spike.
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum PlannedStep {
    /// `from`/`to` are rung INDICES into the session's size ladder.
    Resolution(StepDecision),
    /// `from`/`to` are frame RATES (120 → 60).
    Fps(StepDecision),
}

/// Which lever a window belongs to.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Lever {
    Res,
    Fps,
}

/// D7: the two rungs plus the ordering policy that decides which one a window belongs to.
///
/// ## The one rule that matters
/// **Recovery unwinds in the exact reverse of the engage order.** If the ladder went
/// `res → fps`, it must come back `fps → res` — otherwise the picture gets *sharper*
/// while the stream is still stuttering, which is precisely the trade the ladder exists
/// to avoid. That is not tracked as a stack of past steps (which an external
/// `session_display_update` would desync); it is DERIVED each window from the two rungs'
/// current positions plus the order, so a manual resize can never leave it lying.
///
/// ## Ordering (`abr_ladder_order`)
/// - `hybrid` (default, Michael's): resolution rungs down to 1080p, **then** fps 120→60,
///   **then** the deeper resolution rungs. 120→60 is the step taken before dropping below
///   1080p, because at 1080p a frame-rate halving is the smaller perceptual cost.
/// - `res_first` / `fps_first`: the two pure orders, for A/B.
///
/// ## One lever per window
/// Each window is classified into a direction first ([`ResolutionRung::wants`] /
/// [`FpsRung::wants`] — pure, no bookkeeping), then exactly ONE rung is fed. The other
/// rung is frozen for that window: its dwell counters do not advance, which is what keeps
/// a step on lever A from silently paying down lever B's dwell. Fall-through to the second
/// lever happens only when the first is EXHAUSTED in that direction
/// ([`ResolutionRung::can_step`]), never merely because it returned `None` (that would
/// defeat the pacing).
#[derive(Debug)]
pub struct LadderPlanner {
    res: ResolutionRung,
    fps: FpsRung,
    order: LadderOrder,
    /// `false` ⇒ `abr_ladder_fps` is off: the planner is exactly the resolution rung.
    fps_enabled: bool,
    /// Set once a lever has refused a step — see [`LadderPlanner::set_res_retired`].
    res_retired: bool,
    fps_retired: bool,
}

impl LadderPlanner {
    /// The hybrid pivot: the height at and below which the fps rung is taken before any
    /// further resolution step. Michael's rule — "120→60 as the step before dropping
    /// below 1080p".
    pub const HYBRID_PIVOT_HEIGHT: i32 = 1080;

    pub fn new(
        cfg: &LadderConfig,
        rungs: Vec<(i32, i32)>,
        launch_fps: i32,
        ceiling_kbps: u32,
        now: Instant,
    ) -> Self {
        Self {
            res: ResolutionRung::new(cfg.res, rungs, ceiling_kbps, now),
            fps: FpsRung::new(cfg.res, launch_fps, ceiling_kbps, now),
            order: cfg.order,
            fps_enabled: cfg.fps_enabled,
            res_retired: false,
            fps_retired: false,
        }
    }

    /// The resolution rung index (0 = launch).
    pub fn res_rung(&self) -> usize {
        self.res.rung()
    }

    /// The external size the resolution rung currently asks for.
    pub fn res_size(&self) -> (i32, i32) {
        self.res.size()
    }

    /// The frame rate the fps rung currently asks for (the launch fps until it steps —
    /// and always the launch fps when the rung is off or the session is 60 fps).
    pub fn fps(&self) -> i32 {
        self.fps.fps()
    }

    /// The fps rung INDEX (0 = the launch rate). Amendment 5 reads it to decide whether
    /// the effective ABR floor carries the half-rate factor.
    pub fn fps_rung(&self) -> usize {
        self.fps.rung()
    }

    /// Whether an fps rung exists for this session at all (enabled AND launch fps ≥ 120).
    pub fn fps_rung_active(&self) -> bool {
        self.fps_enabled && self.fps.is_active()
    }

    /// A human owns the picture — both rungs keep counting but neither emits. A rung
    /// already RETIRED stays pinned regardless (a refusal is permanent).
    ///
    /// Ordering (load-bearing): call this AFTER [`set_res_retired`](Self::set_res_retired)
    /// / [`set_fps_retired`](Self::set_fps_retired) in a window, never before — the
    /// retirement setters write the pin flag from the retirement bit alone, so calling
    /// one after this would clear a human pin. `observe_resolution_rung` in
    /// `pipeline::abr_glue` is the only caller and keeps that order.
    pub fn set_pinned(&mut self, pinned: bool) {
        self.res.set_pinned(pinned || self.res_retired);
        self.fps.set_pinned(pinned || self.fps_retired);
    }

    /// Retire one lever for the rest of the session — the glue calls this when a lever
    /// has REFUSED a step (cannot start working mid-session).
    ///
    /// Distinct from a pin: a pinned rung is still selectable (keeps counting, planner
    /// spends the window on it and emits nothing), a retired rung is ineligible for
    /// selection so the window falls through to the other lever — otherwise a missing
    /// `videorate` would park every `hybrid` window on the dead fps rung at the pivot.
    ///
    /// Call this BEFORE [`set_pinned`](Self::set_pinned) in a window — it writes the pin
    /// flag from the retirement bit alone, so calling it after would clear a human pin.
    pub fn set_res_retired(&mut self, retired: bool) {
        self.res_retired = retired;
        self.res.set_pinned(retired);
    }

    /// Retire the FPS lever. See [`Self::set_res_retired`] for the ordering rule with
    /// [`set_pinned`](Self::set_pinned).
    pub fn set_fps_retired(&mut self, retired: bool) {
        self.fps_retired = retired;
        self.fps.set_pinned(retired);
    }

    /// Adopt an external size (a manual `session_display_update`). Only the resolution
    /// rung has an out-of-band writer; fps is ladder-owned.
    pub fn sync_res(&mut self, size: (i32, i32)) {
        self.res.sync_rung(size);
    }

    /// Whether the fps rung is eligible to move under `hybrid` — i.e. the resolution rung
    /// has already come down to the pivot (1080p or shorter).
    fn at_or_below_pivot(&self) -> bool {
        self.res.size().1 <= Self::HYBRID_PIVOT_HEIGHT
    }

    /// The lever priority for a DOWN (engage) window.
    fn down_priority(&self) -> [Lever; 2] {
        match self.order {
            LadderOrder::ResFirst => [Lever::Res, Lever::Fps],
            LadderOrder::FpsFirst => [Lever::Fps, Lever::Res],
            // Hybrid: resolution until the pivot, then fps, then resolution again.
            LadderOrder::Hybrid => {
                if self.at_or_below_pivot() {
                    [Lever::Fps, Lever::Res]
                } else {
                    [Lever::Res, Lever::Fps]
                }
            }
        }
    }

    /// The lever priority for an UP (recover) window — the reverse of engage order. For
    /// the two pure orders that's literally the reverse of `down_priority`. Hybrid needs
    /// the pivot: the fps step was taken at the pivot, so a resolution rung below it was
    /// engaged after and must come back first; at or above the pivot fps was most recent.
    fn up_priority(&self) -> [Lever; 2] {
        match self.order {
            LadderOrder::ResFirst => [Lever::Fps, Lever::Res],
            LadderOrder::FpsFirst => [Lever::Res, Lever::Fps],
            LadderOrder::Hybrid => {
                let fps_engaged = self.fps.rung() > 0;
                if fps_engaged && self.res.size().1 >= Self::HYBRID_PIVOT_HEIGHT {
                    [Lever::Fps, Lever::Res]
                } else {
                    [Lever::Res, Lever::Fps]
                }
            }
        }
    }

    fn can_step(&self, lever: Lever, dir: StepDir) -> bool {
        match lever {
            Lever::Res => !self.res_retired && self.res.can_step(dir),
            Lever::Fps => !self.fps_retired && self.fps_rung_active() && self.fps.can_step(dir),
        }
    }

    /// One window. Returns at most ONE step, on exactly one lever.
    pub fn observe(
        &mut self,
        state: AdaptationState,
        setpoint_kbps: f64,
        floor_kbps: f64,
        now: Instant,
    ) -> Option<PlannedStep> {
        // Read from the resolution rung first, fall back to fps. Pure — neither call
        // advances a counter.
        let dir = self
            .res
            .wants(state, setpoint_kbps, floor_kbps)
            .or_else(|| {
                (self.fps_rung_active() && !self.fps_retired)
                    .then(|| self.fps.wants(state, setpoint_kbps, floor_kbps))
                    .flatten()
            });
        let Some(dir) = dir else {
            // Neither rung is asking for anything: feed BOTH so their dwell counters
            // reset (a `Hold` window can never step, so this cannot move the picture).
            let a = self.res.observe(state, setpoint_kbps, floor_kbps, now);
            let b = (self.fps_rung_active() && !self.fps_retired)
                .then(|| self.fps.observe(state, setpoint_kbps, floor_kbps, now))
                .flatten();
            debug_assert!(
                a.is_none() && b.is_none(),
                "a rung stepped on a window neither wanted to move"
            );
            return a.map(PlannedStep::Resolution).or(b.map(PlannedStep::Fps));
        };
        let priority = match dir {
            StepDir::Down => self.down_priority(),
            StepDir::Up => self.up_priority(),
        };
        // Take the first lever with room left in this direction. Falling through on
        // "exhausted" only — never on a mere `None` (see the struct doc).
        let Some(lever) = priority.into_iter().find(|l| self.can_step(*l, dir)) else {
            // Reachable only when a rung WANTS to move and neither lever has room — i.e.
            // one of them is retired and the other is at its extreme. Never on the happy
            // path: `wants` already required a rung with somewhere to go.
            debug_assert!(
                self.res_retired || self.fps_retired,
                "a direction was derived but neither lever can move it"
            );
            return None;
        };
        match lever {
            Lever::Res => self
                .res
                .observe(state, setpoint_kbps, floor_kbps, now)
                .map(PlannedStep::Resolution),
            Lever::Fps => self
                .fps
                .observe(state, setpoint_kbps, floor_kbps, now)
                .map(PlannedStep::Fps),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ladder() -> Ladder {
        Ladder::new(LadderConfig::new())
    }

    #[test]
    fn defaults_are_the_safe_posture() {
        let c = LadderConfig::new();
        assert_eq!(c.max_bias, 2);
        assert!(!c.fps_enabled, "fps rung must default OFF");
        assert!(!c.resolution_enabled, "resolution rung must default OFF");
        assert!(
            c.floor_follows_rung,
            "the floor must follow the rung by default (inert while both rungs are off)"
        );
    }

    // ---- FloorFollow -----------------------------------------------------------------

    /// The run-C session: `1080p120-h264`, ceiling 11500, profile's own wire floor of
    /// 4000 kbps — not the ratio-derived 3450 the spec draft assumed.
    fn run_c_floor() -> FloorFollow {
        FloorFollow {
            enabled: true,
            launch_floor_kbps: 4000,
            operator_floor_kbps: None,
            ceiling_kbps: 11500,
            launch_px: 1920 * 1080,
            exponent: ResolutionPolicy::DEFAULT_EXPONENT,
        }
    }

    #[test]
    fn floor_at_the_launch_rung_is_exactly_todays_floor() {
        let ff = run_c_floor();
        assert_eq!(ff.floor_for(1920 * 1080, false), 4000);
        assert_eq!(
            FloorFollow {
                launch_floor_kbps: 3450,
                ..ff
            }
            .floor_for(1920 * 1080, false),
            3450
        );
    }

    #[test]
    fn the_ratio_derived_case_reduces_to_the_specs_formula() {
        let ratio = 0.3;
        let ceiling = 11500u32;
        let ff = FloorFollow {
            launch_floor_kbps: (ceiling as f64 * ratio) as u32,
            ..run_c_floor()
        };
        for &(px, fps_down) in &[
            ((1600 * 900) as i64, false),
            ((1280 * 720) as i64, false),
            ((1280 * 720) as i64, true),
        ] {
            let mut b_eff = comfort_kbps(ceiling, 1920 * 1080, px, 0.75);
            if fps_down {
                b_eff *= FpsRung::HALF_RATE_BITRATE_RATIO;
            }
            let spec = (ratio * b_eff).round() as u32;
            let got = ff.floor_for(px, fps_down);
            assert!(
                got.abs_diff(spec) <= 1,
                "px={px} fps_down={fps_down}: got {got}, spec formula {spec}"
            );
        }
    }

    #[test]
    fn floor_drops_with_the_resolution_rung() {
        let ff = run_c_floor();
        // scale(1600x900) = (0.694)^0.75 ≈ 0.760 ⇒ floor ≈ 3039.
        let r900 = ff.floor_for(1600 * 900, false);
        assert!((2950..=3150).contains(&r900), "900p floor = {r900}");
        // scale(1280x720) = (0.444)^0.75 ≈ 0.544 ⇒ floor ≈ 2178.
        let r720 = ff.floor_for(1280 * 720, false);
        assert!((2100..=2250).contains(&r720), "720p floor = {r720}");
        assert!(r720 < r900 && r900 < 4000, "monotone with the rung");
    }

    #[test]
    fn the_fps_rung_takes_the_floor_under_the_link() {
        // The netem cap is 3.5 Mbps, launch floor 4000: any engaged rung must put the
        // floor under the cap.
        let ff = run_c_floor();
        let fps_only = ff.floor_for(1920 * 1080, true);
        assert_eq!(fps_only, 2400);
        assert!(
            fps_only < 3500,
            "even the first step clears the 3.5 Mbps cap"
        );
        let deepest = ff.floor_for(1280 * 720, true);
        assert!((1250..=1400).contains(&deepest), "720p60 floor = {deepest}");
    }

    #[test]
    fn a_host_level_operator_floor_wins_absolutely() {
        let ff = FloorFollow {
            operator_floor_kbps: Some(3000),
            launch_floor_kbps: 3000,
            ..run_c_floor()
        };
        assert_eq!(ff.floor_for(1280 * 720, true), 3000);
        assert_eq!(ff.floor_for(1920 * 1080, false), 3000);
    }

    #[test]
    fn the_knob_off_pins_the_floor_at_launch() {
        let ff = FloorFollow {
            enabled: false,
            ..run_c_floor()
        };
        assert_eq!(ff.floor_for(1280 * 720, true), 4000);
    }

    #[test]
    fn the_floor_never_goes_below_the_hard_minimum() {
        let ff = FloorFollow {
            ceiling_kbps: 1200,
            launch_floor_kbps: 360,
            ..run_c_floor()
        };
        let f = ff.floor_for(640 * 360, true);
        assert!(f >= FloorFollow::HARD_MIN_KBPS, "floor = {f}");
        assert!(f <= 360, "and never above the launch floor");
    }

    #[test]
    fn starts_at_baseline_bias_zero() {
        assert_eq!(ladder().bias_level(), 0);
    }

    #[test]
    fn single_saturated_window_does_not_escalate() {
        let mut l = ladder();
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), None);
        assert_eq!(l.bias_level(), 0);
    }

    #[test]
    fn sustained_saturation_escalates_one_rung_per_dwell() {
        let mut l = ladder();
        // Window 1: count = 1, no step.
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), None);
        // Window 2: count = 2 = engage_dwell ⇒ step to bias 1.
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), Some(1));
        assert_eq!(l.bias_level(), 1);
        // Counter reset: the next escalation needs another full dwell.
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), None);
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), Some(2));
        assert_eq!(l.bias_level(), 2);
    }

    #[test]
    fn escalation_caps_at_max_bias() {
        let mut l = ladder();
        let mut steps = Vec::new();
        for _ in 0..20 {
            if let Some(b) = l.observe(AdaptationState::EncoderSaturated) {
                steps.push(b);
            }
        }
        assert_eq!(
            steps,
            vec![1, 2],
            "must escalate to max_bias and no further"
        );
        assert_eq!(l.bias_level(), 2);
    }

    #[test]
    fn healthy_recovers_one_rung_per_recover_dwell() {
        let mut l = ladder();
        for _ in 0..4 {
            l.observe(AdaptationState::EncoderSaturated);
        }
        assert_eq!(l.bias_level(), 2);
        assert_eq!(l.observe(AdaptationState::Healthy), None);
        assert_eq!(l.observe(AdaptationState::Healthy), Some(1));
        assert_eq!(l.bias_level(), 1);
        assert_eq!(l.observe(AdaptationState::Healthy), None);
        assert_eq!(l.observe(AdaptationState::Healthy), Some(0));
        assert_eq!(l.bias_level(), 0);
    }

    #[test]
    fn healthy_at_baseline_is_a_noop() {
        let mut l = ladder();
        for _ in 0..10 {
            assert_eq!(l.observe(AdaptationState::Healthy), None);
        }
        assert_eq!(l.bias_level(), 0);
    }

    #[test]
    fn network_congestion_never_touches_the_encoder_rung() {
        let mut l = ladder();
        for _ in 0..10 {
            assert_eq!(l.observe(AdaptationState::NetworkCongested), None);
        }
        assert_eq!(l.bias_level(), 0);
    }

    #[test]
    fn congestion_does_not_unwind_an_existing_bias() {
        let mut l = ladder();
        l.observe(AdaptationState::EncoderSaturated);
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), Some(1));
        for _ in 0..10 {
            assert_eq!(l.observe(AdaptationState::NetworkCongested), None);
        }
        assert_eq!(
            l.bias_level(),
            1,
            "congestion must not recover the encoder rung"
        );
    }

    #[test]
    fn flap_guard_requires_contiguous_streaks() {
        // A saturated window then a healthy window then saturated must NOT escalate
        // (the run resets), proving hysteresis needs a contiguous streak.
        let mut l = ladder();
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), None); // run=1
        assert_eq!(l.observe(AdaptationState::Healthy), None); // saturated run reset
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), None); // run=1 again, not 2
        assert_eq!(
            l.bias_level(),
            0,
            "non-contiguous saturation must not escalate"
        );
    }

    #[test]
    fn unknown_resets_runs_like_congestion() {
        let mut l = ladder();
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), None); // run=1
        assert_eq!(l.observe(AdaptationState::Unknown), None); // reset
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), None); // run=1, not 2
        assert_eq!(l.bias_level(), 0);
    }

    #[test]
    fn max_bias_zero_disables_the_rung() {
        let cfg = LadderConfig {
            max_bias: 0,
            ..LadderConfig::new()
        };
        let mut l = Ladder::new(cfg);
        for _ in 0..10 {
            assert_eq!(l.observe(AdaptationState::EncoderSaturated), None);
        }
        assert_eq!(l.bias_level(), 0, "max_bias=0 ⇒ inert ladder");
    }

    // ---- config exposure: QUASAR_ABR_LADDER_* hysteresis knobs ----------------------
    // Process-global env vars; all cases in ONE serialized snapshot/restore test (no
    // serial_test dep, no other ladder test touches env vars).

    fn restore(key: &str, prior: Option<String>) {
        match prior {
            Some(v) => std::env::set_var(key, v),
            None => std::env::remove_var(key),
        }
    }

    #[test]
    fn env_overrides_default_to_the_old_constants_and_are_picked_up() {
        let keys = [
            "QUASAR_ABR_LADDER_MAX_BIAS",
            "QUASAR_ABR_LADDER_ENGAGE_DWELL",
            "QUASAR_ABR_LADDER_RECOVER_DWELL",
        ];
        let saved: Vec<(&str, Option<String>)> =
            keys.iter().map(|k| (*k, std::env::var(k).ok())).collect();
        for k in &keys {
            std::env::remove_var(k);
        }

        // (a) All unset ⇒ overlay is a no-op: fields EXACTLY equal the old constants.
        let d = LadderConfig::new().with_env_overrides();
        assert_eq!(d.max_bias, LadderConfig::DEFAULT_MAX_BIAS);
        assert_eq!(d.engage_dwell, LadderConfig::DEFAULT_ENGAGE_DWELL);
        assert_eq!(d.recover_dwell, LadderConfig::DEFAULT_RECOVER_DWELL);
        // The fps/resolution passthrough set by the caller is untouched.
        assert!(!d.fps_enabled);
        assert!(!d.resolution_enabled);

        // (b) Each var set to a valid value is picked up.
        std::env::set_var("QUASAR_ABR_LADDER_MAX_BIAS", "3");
        std::env::set_var("QUASAR_ABR_LADDER_ENGAGE_DWELL", "5");
        std::env::set_var("QUASAR_ABR_LADDER_RECOVER_DWELL", "8");
        let s = LadderConfig::new().with_env_overrides();
        assert_eq!(s.max_bias, 3);
        assert_eq!(s.engage_dwell, 5);
        assert_eq!(s.recover_dwell, 8);

        // (c) max_bias=0 is VALID (disables the rung) — not a fall-back.
        std::env::set_var("QUASAR_ABR_LADDER_MAX_BIAS", "0");
        assert_eq!(LadderConfig::new().with_env_overrides().max_bias, 0);

        // (d) Invalid / out-of-range values fall back to the default.
        std::env::set_var("QUASAR_ABR_LADDER_MAX_BIAS", "999"); // > u8::MAX
        std::env::set_var("QUASAR_ABR_LADDER_ENGAGE_DWELL", "0"); // must be >= 1
        std::env::set_var("QUASAR_ABR_LADDER_RECOVER_DWELL", "junk"); // unparseable
        let f = LadderConfig::new().with_env_overrides();
        assert_eq!(f.max_bias, LadderConfig::DEFAULT_MAX_BIAS);
        assert_eq!(f.engage_dwell, LadderConfig::DEFAULT_ENGAGE_DWELL);
        assert_eq!(f.recover_dwell, LadderConfig::DEFAULT_RECOVER_DWELL);

        for (k, prior) in saved {
            restore(k, prior);
        }
    }

    #[test]
    fn recovery_then_resaturation_escalates_again() {
        let mut l = ladder();
        l.observe(AdaptationState::EncoderSaturated);
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), Some(1));
        for _ in 0..4 {
            l.observe(AdaptationState::Healthy);
        }
        assert_eq!(l.bias_level(), 0);
        l.observe(AdaptationState::EncoderSaturated);
        assert_eq!(l.observe(AdaptationState::EncoderSaturated), Some(1));
        assert_eq!(l.bias_level(), 1);
    }

    // ── D1/D2: the resolution rung ────────────────────────────────────────────
    use std::time::{Duration, Instant};

    const CEILING: u32 = 10_000; // 1440p120 @ 10 Mbps, the spec's worked example
    fn rungs_1440() -> Vec<(i32, i32)> {
        vec![(2560, 1440), (1920, 1080), (1600, 900), (1280, 720)]
    }
    fn rung_at(now: Instant) -> ResolutionRung {
        ResolutionRung::new(ResolutionPolicy::new(), rungs_1440(), CEILING, now)
    }
    /// Advance far enough that `min_step_s` and `settle_windows` never mask a decision.
    fn later(t: Instant, secs: u64) -> Instant {
        t + Duration::from_secs(secs)
    }

    // D1: B(0) = ceiling; B(r) = ceiling * (px_r/px_0)^k. Worked example, k=0.75.
    #[test]
    fn comfort_bitrate_follows_the_power_law() {
        let px0 = 2560i64 * 1440;
        assert!((comfort_kbps(CEILING, px0, px0, 0.75) - 10_000.0).abs() < 1e-6);
        let b1080 = comfort_kbps(CEILING, px0, 1920 * 1080, 0.75);
        let b900 = comfort_kbps(CEILING, px0, 1600 * 900, 0.75);
        let b720 = comfort_kbps(CEILING, px0, 1280 * 720, 0.75);
        // Spec D1: 6.5 / 4.9 / 3.5 Mbps (±0.1 Mbps).
        assert!((b1080 - 6500.0).abs() < 100.0, "1080p B = {b1080}");
        assert!((b900 - 4900.0).abs() < 100.0, "900p B = {b900}");
        assert!((b720 - 3500.0).abs() < 100.0, "720p B = {b720}");
        assert!(b1080 > b900 && b900 > b720, "B must be monotonic in pixels");
    }

    // D2 hysteresis gate: recover_frac must exceed engage_frac, and the resulting
    // bands must not overlap for ANY adjacent pair of this session's rungs.
    #[test]
    fn policy_validate_rejects_an_inverted_band() {
        let good = ResolutionPolicy::new();
        assert!(good.validate(&rungs_1440(), CEILING).is_ok());
        let bad = ResolutionPolicy {
            engage_frac: 0.9,
            recover_frac: 0.8,
            ..ResolutionPolicy::new()
        };
        let err = bad.validate(&rungs_1440(), CEILING).unwrap_err();
        assert!(err.contains("engage_frac"), "got {err}");
        assert!(err.contains("recover_frac"), "got {err}");
    }

    // D2: the spec's explicit 0.05 gap floor rejects a correctly-ORDERED but
    // too-tight band even though it never literally inverts.
    #[test]
    fn policy_validate_rejects_a_too_tight_but_non_inverted_band() {
        let bad = ResolutionPolicy {
            engage_frac: 0.61,
            recover_frac: 0.63,
            ..ResolutionPolicy::new()
        };
        let err = bad.validate(&rungs_1440(), CEILING).unwrap_err();
        assert!(err.contains("engage_frac"), "got {err}");
        assert!(err.contains("recover_frac"), "got {err}");
    }

    // D2: exactly the 0.05 floor is accepted (inclusive boundary).
    #[test]
    fn policy_validate_accepts_the_boundary_gap_of_0_05() {
        let ok = ResolutionPolicy {
            engage_frac: 0.60,
            recover_frac: 0.65,
            ..ResolutionPolicy::new()
        };
        assert!(ok.validate(&rungs_1440(), CEILING).is_ok());
    }

    #[test]
    fn policy_validate_rejects_out_of_range_fields() {
        for (p, needle) in [
            (
                ResolutionPolicy {
                    exponent: 0.0,
                    ..ResolutionPolicy::new()
                },
                "exponent",
            ),
            (
                ResolutionPolicy {
                    engage_dwell: 0,
                    ..ResolutionPolicy::new()
                },
                "engage_dwell",
            ),
            (
                ResolutionPolicy {
                    recover_dwell: 0,
                    ..ResolutionPolicy::new()
                },
                "recover_dwell",
            ),
            (
                ResolutionPolicy {
                    min_height: 0,
                    ..ResolutionPolicy::new()
                },
                "min_height",
            ),
        ] {
            let err = p.validate(&rungs_1440(), CEILING).unwrap_err();
            assert!(err.contains(needle), "wanted {needle}, got {err}");
        }
    }

    // Down: setpoint below engage_frac * B(current) for engage_dwell CONSECUTIVE windows.
    #[test]
    fn sustained_low_setpoint_steps_down_one_rung_after_the_dwell() {
        let t = Instant::now();
        let mut r = rung_at(t);
        // engage threshold at rung 0 = 0.6 * 10000 = 6000 kbps.
        assert_eq!(
            r.observe(AdaptationState::Healthy, 5000.0, 3000.0, later(t, 1)),
            None
        );
        let d = r
            .observe(AdaptationState::Healthy, 5000.0, 3000.0, later(t, 20))
            .expect("second consecutive window must step");
        assert_eq!(
            d,
            StepDecision {
                from: 0,
                to: 1,
                reason: StepReason::Engage
            }
        );
        assert_eq!(r.rung(), 1);
        assert_eq!(r.size(), (1920, 1080));
    }

    // A setpoint ABOVE the engage threshold resets the engage run (contiguity).
    #[test]
    fn a_healthy_window_resets_the_engage_run() {
        let t = Instant::now();
        let mut r = rung_at(t);
        assert_eq!(
            r.observe(AdaptationState::Healthy, 5000.0, 3000.0, later(t, 1)),
            None
        );
        assert_eq!(
            r.observe(AdaptationState::Healthy, 9000.0, 3000.0, later(t, 20)),
            None
        );
        assert_eq!(
            r.observe(AdaptationState::Healthy, 5000.0, 3000.0, later(t, 40)),
            None
        );
        assert_eq!(r.rung(), 0, "a non-contiguous streak must not step");
    }

    // Up: setpoint >= recover_frac * B(r-1) for recover_dwell consecutive windows.
    #[test]
    fn sustained_recovery_steps_up_one_rung_after_the_longer_dwell() {
        let t = Instant::now();
        let mut r = rung_at(t);
        r.sync_rung((1920, 1080));
        assert_eq!(r.rung(), 1);
        // recover threshold to rung 0 = 0.8 * B(0) = 0.8 * 10000 = 8000 kbps.
        // recover_dwell defaults to 2 (2026-08-18 T3 flip): one window holds, the
        // second consecutive window steps.
        assert_eq!(
            r.observe(AdaptationState::Healthy, 8500.0, 3000.0, later(t, 20)),
            None,
            "window 1 must hold"
        );
        let d = r
            .observe(AdaptationState::Healthy, 8500.0, 3000.0, later(t, 40))
            .expect("the 2nd consecutive window must step up");
        assert_eq!(
            d,
            StepDecision {
                from: 1,
                to: 0,
                reason: StepReason::Recover
            }
        );
        assert_eq!(r.size(), (2560, 1440));
    }

    // Emergency: congested AND the setpoint is already at (within 5% of) the floor
    // ⇒ step immediately, dwell bypassed.
    #[test]
    fn emergency_steps_down_immediately_at_the_floor() {
        let t = Instant::now();
        let mut r = rung_at(t);
        let d = r
            .observe(
                AdaptationState::NetworkCongested,
                3100.0,
                3000.0,
                later(t, 1),
            )
            .expect("congested at the floor must step on the FIRST window");
        assert_eq!(d.reason, StepReason::Emergency);
        assert_eq!(
            d,
            StepDecision {
                from: 0,
                to: 1,
                reason: StepReason::Emergency
            }
        );
    }

    // Congestion ABOVE the floor is the governor's job — the rung holds.
    #[test]
    fn congestion_above_the_floor_is_not_an_emergency() {
        let t = Instant::now();
        let mut r = rung_at(t);
        assert_eq!(
            r.observe(
                AdaptationState::NetworkCongested,
                7000.0,
                3000.0,
                later(t, 1)
            ),
            None
        );
        assert_eq!(r.rung(), 0);
    }

    // Settle: the window immediately after a step is IGNORED (the IDR σ spike must
    // not feed the next decision), and no two steps inside min_step_s.
    #[test]
    fn settle_and_min_interval_suppress_a_second_step() {
        let t = Instant::now();
        let mut r = rung_at(t);
        assert!(r
            .observe(AdaptationState::NetworkCongested, 3100.0, 3000.0, t)
            .is_some());
        // settle_windows = 1: the very next window is swallowed whole.
        assert_eq!(
            r.observe(
                AdaptationState::NetworkCongested,
                3100.0,
                3000.0,
                later(t, 5)
            ),
            None
        );
        // min_step_s = 10: still inside the interval ⇒ still no step.
        assert_eq!(
            r.observe(
                AdaptationState::NetworkCongested,
                3100.0,
                3000.0,
                later(t, 9)
            ),
            None
        );
        // Past both gates ⇒ the emergency fires again.
        assert!(r
            .observe(
                AdaptationState::NetworkCongested,
                3100.0,
                3000.0,
                later(t, 30)
            )
            .is_some());
        assert_eq!(r.rung(), 2);
    }

    // Floor rung: never below min_height (720 by default).
    #[test]
    fn never_steps_below_the_min_height() {
        let t = Instant::now();
        let mut r = rung_at(t);
        let mut steps = Vec::new();
        for i in 0..40u64 {
            if let Some(d) = r.observe(
                AdaptationState::NetworkCongested,
                100.0,
                3000.0,
                later(t, 30 * i),
            ) {
                steps.push(d.to);
            }
        }
        assert_eq!(steps, vec![1, 2, 3], "one rung at a time, stopping at 720p");
        assert_eq!(r.size(), (1280, 720));
    }

    #[test]
    fn min_height_can_gate_the_ladder_higher() {
        let t = Instant::now();
        let policy = ResolutionPolicy {
            min_height: 1080,
            ..ResolutionPolicy::new()
        };
        let mut r = ResolutionRung::new(policy, rungs_1440(), CEILING, t);
        let mut steps = Vec::new();
        for i in 0..40u64 {
            if let Some(d) = r.observe(
                AdaptationState::NetworkCongested,
                100.0,
                3000.0,
                later(t, 30 * i),
            ) {
                steps.push(d.to);
            }
        }
        assert_eq!(steps, vec![1], "min_height 1080 ⇒ one rung only");
    }

    // Unknown: hold, and reset BOTH counters (same posture as the bias rung).
    #[test]
    fn unknown_holds_and_resets_the_runs() {
        let t = Instant::now();
        let mut r = rung_at(t);
        assert_eq!(
            r.observe(AdaptationState::Healthy, 5000.0, 3000.0, later(t, 1)),
            None
        );
        assert_eq!(
            r.observe(AdaptationState::Unknown, 5000.0, 3000.0, later(t, 20)),
            None
        );
        assert_eq!(
            r.observe(AdaptationState::Healthy, 5000.0, 3000.0, later(t, 40)),
            None
        );
        assert_eq!(r.rung(), 0, "Unknown must reset the engage run");
    }

    // Pinned (D4): a human owns the size — the machine still OBSERVES (so it resumes
    // coherently) but emits nothing.
    #[test]
    fn pinned_emits_nothing_and_resumes_after_release() {
        let t = Instant::now();
        let mut r = rung_at(t);
        r.set_pinned(true);
        for i in 0..10u64 {
            assert_eq!(
                r.observe(
                    AdaptationState::NetworkCongested,
                    100.0,
                    3000.0,
                    later(t, 30 * i)
                ),
                None
            );
        }
        assert_eq!(r.rung(), 0, "a pinned rung never moves itself");
        r.set_pinned(false);
        assert!(r
            .observe(
                AdaptationState::NetworkCongested,
                100.0,
                3000.0,
                later(t, 400)
            )
            .is_some());
    }

    // Pinned (D4): the dwell counters keep advancing normally while pinned — only the
    // emission (and the actual index move) is suppressed — so a release resumes
    // coherently: no lost dwell progress, no extra windows required after unpin.
    #[test]
    fn pinned_keeps_counting_and_steps_immediately_on_release() {
        let t = Instant::now();
        let mut r = rung_at(t);
        r.set_pinned(true);
        // Two consecutive low-setpoint windows while pinned build the full engage
        // dwell (2) — the rung must not move, but the run counter must not reset.
        assert_eq!(
            r.observe(AdaptationState::Healthy, 5000.0, 3000.0, later(t, 1)),
            None
        );
        assert_eq!(
            r.observe(AdaptationState::Healthy, 5000.0, 3000.0, later(t, 20)),
            None
        );
        assert_eq!(r.rung(), 0, "pinned must never move the rung itself");
        r.set_pinned(false);
        let d = r
            .observe(AdaptationState::Healthy, 5000.0, 3000.0, later(t, 40))
            .expect("the dwell was already satisfied while pinned; must step on release");
        assert_eq!(
            d,
            StepDecision {
                from: 0,
                to: 1,
                reason: StepReason::Engage
            }
        );
    }

    // A session with a single rung (no ladder) is inert, not a panic.
    #[test]
    fn a_single_rung_session_is_inert() {
        let t = Instant::now();
        let mut r = ResolutionRung::new(ResolutionPolicy::new(), vec![(1280, 720)], 8000, t);
        for i in 0..10u64 {
            assert_eq!(
                r.observe(
                    AdaptationState::NetworkCongested,
                    10.0,
                    3000.0,
                    later(t, 30 * i)
                ),
                None
            );
        }
    }

    // sync_rung adopts an externally-set size (a manual PATCH) without emitting.
    #[test]
    fn sync_rung_adopts_an_external_size() {
        let t = Instant::now();
        let mut r = rung_at(t);
        r.sync_rung((1280, 720));
        assert_eq!(r.rung(), 3);
        // An unknown size leaves the index alone rather than panicking.
        r.sync_rung((1234, 567));
        assert_eq!(r.rung(), 3);
    }

    // ── D7: the fps rung + the LadderPlanner ordering ─────────────────────────

    fn planner(
        order: LadderOrder,
        fps_enabled: bool,
        launch_fps: i32,
        t: Instant,
    ) -> LadderPlanner {
        let cfg = LadderConfig {
            resolution_enabled: true,
            fps_enabled,
            order,
            ..LadderConfig::new()
        };
        LadderPlanner::new(&cfg, rungs_1440(), launch_fps, CEILING, t)
    }

    fn label(step: PlannedStep) -> String {
        match step {
            PlannedStep::Resolution(d) => format!("res->{}", d.to),
            PlannedStep::Fps(d) => format!("fps->{}", d.to),
        }
    }

    /// Drive `n` congested-at-the-floor windows 30 s apart and collect the labels.
    fn congested_run(p: &mut LadderPlanner, t: Instant, n: u64) -> Vec<String> {
        let mut seen = Vec::new();
        for i in 1..n {
            if let Some(s) = p.observe(
                AdaptationState::NetworkCongested,
                100.0,
                3000.0,
                later(t, 30 * i),
            ) {
                seen.push(label(s));
            }
        }
        seen
    }

    // D7 `hybrid`: resolution down to 1080p, THEN fps 120→60, THEN the deeper
    // resolution rungs. This ordering is Michael's ("120→60 as the step before
    // dropping below 1080p") and is the default.
    #[test]
    fn hybrid_order_inserts_the_fps_step_at_1080p() {
        let t = Instant::now();
        let mut p = planner(LadderOrder::Hybrid, true, 120, t);
        assert_eq!(
            congested_run(&mut p, t, 12),
            vec!["res->1", "fps->60", "res->2", "res->3"]
        );
        assert_eq!(p.fps(), 60);
        assert_eq!(p.res_rung(), 3);
    }

    #[test]
    fn res_first_never_touches_fps_until_the_floor_rung() {
        let t = Instant::now();
        let mut p = planner(LadderOrder::ResFirst, true, 120, t);
        let mut seen = Vec::new();
        for i in 1..12u64 {
            if let Some(s) = p.observe(
                AdaptationState::NetworkCongested,
                100.0,
                3000.0,
                later(t, 30 * i),
            ) {
                seen.push(matches!(s, PlannedStep::Fps(_)));
            }
        }
        assert_eq!(
            seen,
            vec![false, false, false, true],
            "fps only after every resolution rung"
        );
        assert_eq!(p.fps(), 60);
    }

    #[test]
    fn fps_first_takes_the_frame_rate_step_before_any_resolution_step() {
        let t = Instant::now();
        let mut p = planner(LadderOrder::FpsFirst, true, 120, t);
        let first = p.observe(
            AdaptationState::NetworkCongested,
            100.0,
            3000.0,
            later(t, 30),
        );
        assert!(matches!(first, Some(PlannedStep::Fps(_))));
        assert_eq!(p.res_rung(), 0, "no resolution step may precede it");
    }

    // A 60fps session has NO fps rung at all (there is nothing to halve to).
    #[test]
    fn a_60fps_session_has_no_fps_rung() {
        let t = Instant::now();
        let mut p = planner(LadderOrder::FpsFirst, true, 60, t);
        assert!(!p.fps_rung_active());
        let first = p.observe(
            AdaptationState::NetworkCongested,
            100.0,
            3000.0,
            later(t, 30),
        );
        assert!(
            matches!(first, Some(PlannedStep::Resolution(_))),
            "must fall through to resolution"
        );
        assert_eq!(p.fps(), 60);
    }

    // Recovery unwinds in the REVERSE order (fps comes back before the resolution rung
    // above it), so the picture never gets sharper at a stutter.
    #[test]
    fn hybrid_recovery_unwinds_in_reverse() {
        let t = Instant::now();
        let mut p = planner(LadderOrder::Hybrid, true, 120, t);
        for i in 1..6u64 {
            p.observe(
                AdaptationState::NetworkCongested,
                100.0,
                3000.0,
                later(t, 30 * i),
            );
        }
        assert_eq!((p.res_rung(), p.fps()), (2, 60));
        let mut seen = Vec::new();
        for i in 10..40u64 {
            if let Some(s) = p.observe(AdaptationState::Healthy, 9800.0, 3000.0, later(t, 30 * i)) {
                seen.push(label(s));
            }
        }
        assert_eq!(
            seen[0], "res->1",
            "the deepest resolution rung comes back first"
        );
        assert_eq!(seen[1], "fps->120");
        assert_eq!(seen[2], "res->0");
        assert_eq!((p.res_rung(), p.fps()), (0, 120), "fully unwound");
    }

    // The pure orders unwind in their own reverse too.
    #[test]
    fn res_first_recovery_returns_the_frame_rate_before_any_pixels() {
        let t = Instant::now();
        let mut p = planner(LadderOrder::ResFirst, true, 120, t);
        for i in 1..12u64 {
            p.observe(
                AdaptationState::NetworkCongested,
                100.0,
                3000.0,
                later(t, 30 * i),
            );
        }
        assert_eq!((p.res_rung(), p.fps()), (3, 60));
        let mut seen = Vec::new();
        for i in 20..60u64 {
            if let Some(s) = p.observe(AdaptationState::Healthy, 9800.0, 3000.0, later(t, 30 * i)) {
                seen.push(label(s));
            }
        }
        assert_eq!(
            seen[0], "fps->120",
            "res_first engaged fps last ⇒ it returns first"
        );
        assert_eq!(seen[1], "res->2");
    }

    // fps_enabled=false ⇒ the planner is exactly the resolution rung (phase-1 posture).
    #[test]
    fn the_planner_is_resolution_only_when_the_fps_rung_is_off() {
        let t = Instant::now();
        let mut p = planner(LadderOrder::Hybrid, false, 120, t);
        assert!(!p.fps_rung_active());
        for i in 1..12u64 {
            if let Some(s) = p.observe(
                AdaptationState::NetworkCongested,
                100.0,
                3000.0,
                later(t, 30 * i),
            ) {
                assert!(
                    matches!(s, PlannedStep::Resolution(_)),
                    "no fps step may be planned"
                );
            }
        }
        assert_eq!(p.fps(), 120);
        assert_eq!(p.res_rung(), 3, "the resolution rung is unaffected");
    }

    // A pin freezes BOTH levers (D4: a human owns the picture).
    #[test]
    fn a_pin_freezes_both_levers() {
        let t = Instant::now();
        let mut p = planner(LadderOrder::Hybrid, true, 120, t);
        p.set_pinned(true);
        assert!(congested_run(&mut p, t, 12).is_empty());
        assert_eq!((p.res_rung(), p.fps()), (0, 120));
    }

    // The fps rung's own comfort curve: halving the rate needs 0.6x the bitrate, so the
    // step back UP to 120 needs recover_frac x ceiling, not recover_frac x 0.6 x ceiling.
    #[test]
    fn the_fps_rung_uses_the_0_6_half_rate_comfort_ratio() {
        let t = Instant::now();
        let mut r = FpsRung::new(ResolutionPolicy::new(), 120, CEILING, t);
        assert!(r.is_active());
        // engage at 0.6 x B_fps(120) = 0.6 x 10000 = 6000 kbps.
        assert_eq!(
            r.observe(AdaptationState::Healthy, 5000.0, 3000.0, later(t, 1)),
            None,
            "one window is not the dwell"
        );
        let d = r
            .observe(AdaptationState::Healthy, 5000.0, 3000.0, later(t, 20))
            .expect("the second consecutive low window must halve the rate");
        assert_eq!(
            d,
            StepDecision {
                from: 120,
                to: 60,
                reason: StepReason::Engage
            },
            "from/to are frame RATES, not indices"
        );
        assert_eq!(r.fps(), 60);
        // Recovery threshold is 0.8 x B_fps(120) = 8000 kbps: 7000 must NOT bring it back.
        for i in 2..10u64 {
            assert_eq!(
                r.observe(AdaptationState::Healthy, 7000.0, 3000.0, later(t, 20 * i)),
                None
            );
        }
        assert_eq!(r.fps(), 60, "7000 kbps is inside the band");
        for i in 10..14u64 {
            r.observe(AdaptationState::Healthy, 8500.0, 3000.0, later(t, 20 * i));
        }
        assert_eq!(r.fps(), 120, "8500 kbps clears 0.8 x ceiling");
    }

    // A 60 fps session's rung is inert: no steps, ever, in either direction.
    #[test]
    fn a_60fps_rung_is_inert() {
        let t = Instant::now();
        let mut r = FpsRung::new(ResolutionPolicy::new(), 60, CEILING, t);
        assert!(!r.is_active());
        assert_eq!(r.fps(), 60);
        for i in 0..10u64 {
            assert_eq!(
                r.observe(
                    AdaptationState::NetworkCongested,
                    100.0,
                    3000.0,
                    later(t, 30 * i)
                ),
                None
            );
        }
        assert_eq!(r.fps(), 60);
        assert!(!r.can_step(StepDir::Down) && !r.can_step(StepDir::Up));
    }

    // An external resize (a manual PATCH) moves the resolution rung underneath the
    // planner; the unwind order must follow the NEW position, not a remembered history.
    #[test]
    fn an_external_resize_reorders_the_unwind_without_a_stale_history() {
        let t = Instant::now();
        let mut p = planner(LadderOrder::Hybrid, true, 120, t);
        // Engage down to (res 2, fps 60).
        for i in 1..6u64 {
            p.observe(
                AdaptationState::NetworkCongested,
                100.0,
                3000.0,
                later(t, 30 * i),
            );
        }
        assert_eq!((p.res_rung(), p.fps()), (2, 60));
        // A human PATCHes the size back up to 1080p (rung 1) — now the fps step is the
        // most recent one again, so it must be what comes back first.
        p.sync_res((1920, 1080));
        let mut seen = Vec::new();
        for i in 10..30u64 {
            if let Some(s) = p.observe(AdaptationState::Healthy, 9800.0, 3000.0, later(t, 30 * i)) {
                seen.push(label(s));
            }
        }
        assert_eq!(seen[0], "fps->120");
        assert_eq!(seen[1], "res->0");
    }

    // An empty rung list (a malformed/unconfigured ladder) must never panic — `size()`
    // used to underflow `rungs.len() - 1` at index 0.
    #[test]
    fn an_empty_rung_list_never_panics() {
        let t = Instant::now();
        let mut r = ResolutionRung::new(ResolutionPolicy::new(), vec![], 8000, t);
        assert_eq!(r.rung(), 0);
        assert_eq!(r.size(), (0, 0));
        assert_eq!(
            r.observe(
                AdaptationState::NetworkCongested,
                10.0,
                3000.0,
                later(t, 30)
            ),
            None
        );
    }
}
