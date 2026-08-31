//! SPT-03 — in-agent live adaptation classifier (signal-only).
//!
//! Computes a lightweight [`AdaptationState`] once per metrics window from signals the
//! agent already has (encode_ms p50/p95, realized fps, FIFO drops, the raw GCC estimate,
//! the governor setpoint, whether the setpoint just stepped down). Labels *which*
//! bottleneck a degradation is, so a future smooth governor (SPT-04) can react
//! differently to "encoder can't keep up" vs "network congested".
//!
//! **Signal-only**: changes no ABR retargeting behaviour ([`abr::Governor`]
//! (crate::session::abr) is untouched). Only adds a telemetry label.
//!
//! ## Relationship to the control-plane classifier (`internal/session/classifier.go`)
//! classifier.go is an observational, post-hoc Go classifier over the joined
//! browser+agent trace; this module is its live in-agent twin. Rule SHAPE is shared
//! deliberately (divergence between the two is a diagnostic footgun); threshold
//! differences are principled, documented at each constant:
//!
//! - **Encoder saturation.** classifier.go trips post-hoc at `encode_ms p95 >= 16.0ms`
//!   (~96% of the 60fps budget) AND fps not steady. This classifier trips EARLIER
//!   (~70% of budget) so SPT-04 can react before frames are already dropped; the budget
//!   derives from the session's target fps, never hardcoded to 60.
//! - **Host-fps-steady guard.** Mirrors classifier.go's `classifierMinHostFps` idea but
//!   relative to the session's target fps (`fps < target × FPS_STEADY_FRAC`) instead of
//!   a fixed 50.0.
//! - **Network congestion.** classifier.go uses browser loss/rtt, which the agent never
//!   sees (invariant #1). In-agent proxy: a GCC estimate meaningfully below the setpoint
//!   while the encoder sends at the cap — rtpgccbwe's estimate is the loss/rtt/jitter
//!   evidence, pre-fused.
//!
//! ## `client_presentation_limited` is intentionally out of scope here.
//! It needs browser-sourced present-σ/freeze data the node-agent never sees (invariant
//! #1). classifier.go detects it control-plane-side from the joined trace. This
//! classifier must never compute it or invent a path for browser data to reach the
//! agent — the closest it gets is [`AdaptationState::Healthy`], which the control plane
//! upgrades to a presentation-limit verdict when the browser trace shows judder with a
//! steady host.

/// The live per-window adaptation label. Emitted into `session_metrics` as
/// `adaptation_state`; consumed by SPT-04 (no consumer in SPT-03 — signal only).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AdaptationState {
    /// Fps at target, encode time well under budget, no FIFO drops, GCC not dragging
    /// the setpoint down.
    Healthy,
    /// GCC estimate meaningfully below the setpoint while the encoder sends at the cap:
    /// the downstream pipe, not the encoder, is the limit.
    NetworkCongested,
    /// Encode time near/over the per-frame budget AND fps below target and/or FIFO
    /// dropping — typically with a GCC downshift trailing the encode-time spike (the
    /// encoder flooding the pipe forces GCC down, the #325 coupled-loop shape).
    EncoderSaturated,
    /// Signals insufficient or contradictory (ABR disarmed, no encode samples, or
    /// degraded with no clear cause).
    Unknown,
}

impl AdaptationState {
    /// Canonical wire string (matches the control-plane bundle / classifier.go taxonomy).
    pub fn as_str(self) -> &'static str {
        match self {
            AdaptationState::Healthy => "healthy",
            AdaptationState::NetworkCongested => "network_congested",
            AdaptationState::EncoderSaturated => "encoder_saturated",
            AdaptationState::Unknown => "unknown",
        }
    }
}

/// One metrics window's agent-local signals, already measured by
/// `SessionMetrics::drain_window` / the ABR glue.
#[derive(Debug, Clone, Copy)]
pub struct AdaptationSignals {
    /// Session target frame rate (fps); per-frame encode budget is `1000 / target_fps`.
    pub target_fps: u32,
    /// Realized output fps this window.
    pub fps: f64,
    /// Encode-time p50 (ms), or `None` if no frame was encoded.
    pub encode_ms_p50: Option<f64>,
    /// Encode-time p95 (ms), or `None` if no frame was encoded.
    pub encode_ms_p95: Option<f64>,
    /// Encoder FIFO drops/timeouts this window.
    pub frames_dropped: u64,
    /// Raw rtpgccbwe estimate (kbps), or `None` when ABR is disarmed / none received yet.
    pub gcc_estimate_kbps: Option<f64>,
    /// Current governor CBR setpoint (kbps), or `None` when ABR is disarmed.
    pub setpoint_kbps: Option<f64>,
    /// Realized send bitrate (kbps): confirms the encoder is pushing at the cap —
    /// congestion requires sending AT the limit, not merely a low estimate while idle.
    pub bitrate_kbps: f64,
    /// True when the governor setpoint stepped DOWN vs the previous window. Reinforces
    /// both congestion and encoder-saturation (a downshift trailing an encode-time spike).
    pub gcc_downshifted: bool,
}

// --- thresholds (the v1 hypothesis; mirror classifier.go where applicable) ---------

/// Fraction of the per-frame encode budget at/above which the encoder is "near budget".
/// 0.70 ⇒ ~11.7 ms at 60fps. Deliberately earlier than classifier.go's post-hoc
/// `encoderCeilingMs = 16.0` (~0.96 of budget) so this can flag saturation before
/// frames are dropped.
const ENCODE_BUDGET_FRAC: f64 = 0.70;

/// Fraction of target fps below which realized fps counts as "not steady". Mirrors
/// classifier.go's `classifierMinHostFps` (50/60 ≈ 0.83) but relative to target fps.
const FPS_STEADY_FRAC: f64 = 0.85;

/// Fraction below the setpoint the GCC estimate must fall to count as network
/// congestion. 0.85 matches `abr::AbrConfig::DEFAULT_DEADBAND` so classifier and
/// governor agree on "meaningfully below".
const GCC_BELOW_SETPOINT_FRAC: f64 = 0.85;

/// Fraction of the setpoint the realized send bitrate must reach to count as "at the
/// cap" — congestion is only credible when the encoder is actually pushing the limit.
const SEND_AT_CAP_FRAC: f64 = 0.85;

/// Operator-tunable classifier thresholds. [`Default`] is exactly the constants above,
/// so `from_env` with all vars unset is byte-identical to the pre-exposure behaviour.
#[derive(Debug, Clone, Copy)]
pub struct ClassifierConfig {
    /// `QUASAR_ADAPT_ENCODE_BUDGET_FRAC` — see [`ENCODE_BUDGET_FRAC`]. Range `(0, 1]`.
    pub encode_budget_frac: f64,
    /// `QUASAR_ADAPT_FPS_STEADY_FRAC` — see [`FPS_STEADY_FRAC`]. Range `(0, 1]`.
    pub fps_steady_frac: f64,
    /// `QUASAR_ADAPT_GCC_BELOW_FRAC` — see [`GCC_BELOW_SETPOINT_FRAC`]. Range `(0, 1]`.
    pub gcc_below_setpoint_frac: f64,
    /// `QUASAR_ADAPT_SEND_AT_CAP_FRAC` — see [`SEND_AT_CAP_FRAC`]. Range `(0, 1]`.
    pub send_at_cap_frac: f64,
}

impl Default for ClassifierConfig {
    fn default() -> Self {
        Self {
            encode_budget_frac: ENCODE_BUDGET_FRAC,
            fps_steady_frac: FPS_STEADY_FRAC,
            gcc_below_setpoint_frac: GCC_BELOW_SETPOINT_FRAC,
            send_at_cap_frac: SEND_AT_CAP_FRAC,
        }
    }
}

impl ClassifierConfig {
    /// Read operator overrides from the environment. Each is validated to `(0, 1]`; an
    /// unparseable/out-of-range value warns once and falls back to the default.
    pub fn from_env() -> Self {
        let d = Self::default();
        Self {
            encode_budget_frac: env_frac("QUASAR_ADAPT_ENCODE_BUDGET_FRAC", d.encode_budget_frac),
            fps_steady_frac: env_frac("QUASAR_ADAPT_FPS_STEADY_FRAC", d.fps_steady_frac),
            gcc_below_setpoint_frac: env_frac(
                "QUASAR_ADAPT_GCC_BELOW_FRAC",
                d.gcc_below_setpoint_frac,
            ),
            send_at_cap_frac: env_frac("QUASAR_ADAPT_SEND_AT_CAP_FRAC", d.send_at_cap_frac),
        }
    }
}

/// Parse a classifier fraction in `(0, 1]`. Junk/out-of-range WARNs once and returns
/// `default`. A trimmed-empty value is treated as unset (silent fall-through).
fn env_frac(var: &str, default: f64) -> f64 {
    match std::env::var(var).ok().as_deref().map(str::trim) {
        None | Some("") => default,
        Some(raw) => match raw.parse::<f64>() {
            Ok(v) if v.is_finite() && v > 0.0 && v <= 1.0 => v,
            _ => {
                tracing::warn!(
                    token = "knob-invalid-fraction",
                    "{var}={raw:?} is not a valid fraction in (0, 1]; using default {default}"
                );
                default
            }
        },
    }
}

/// Classify one metrics window into an [`AdaptationState`]. Pure, cheap arithmetic —
/// runs once per drain (~5 s), never per-frame. Priority order:
///
///  1. `EncoderSaturated` — checked first: when the encoder floods the pipe it also
///     drags GCC down (#325 coupled-loop shape), so a naive congestion check would
///     mis-blame the network. The encode-time spike is the discriminator.
///  2. `NetworkCongested` — GCC meaningfully below setpoint with the encoder at the
///     cap and no encode-time spike; the agent-local proxy for classifier.go's
///     browser-sourced congestion window.
///  3. `Healthy` — fps at target, encode under budget, no drops, GCC not dragging.
///  4. `Unknown` — insufficient/contradictory signals.
pub fn classify(s: &AdaptationSignals) -> AdaptationState {
    classify_with(s, &ClassifierConfig::default())
}

/// [`classify`] with operator-tunable thresholds. The bare [`classify`] delegates here
/// with [`ClassifierConfig::default`] (the old hardcoded constants). Same rules; only the
/// threshold source differs.
pub fn classify_with(s: &AdaptationSignals, cfg: &ClassifierConfig) -> AdaptationState {
    let target_fps = s.target_fps.max(1) as f64;
    let frame_budget_ms = 1000.0 / target_fps;
    let fps_steady = s.fps >= target_fps * cfg.fps_steady_frac;

    // Prefer p95 (tail stalls the frame clock); None ⇒ nothing encoded this window.
    let encode_ms = s.encode_ms_p95.or(s.encode_ms_p50);
    let encode_near_budget =
        encode_ms.is_some_and(|e| e >= frame_budget_ms * cfg.encode_budget_frac);

    if encode_near_budget && (!fps_steady || s.frames_dropped > 0) {
        return AdaptationState::EncoderSaturated;
    }

    // Requires ABR armed (both estimate + setpoint).
    if let (Some(gcc), Some(sp)) = (s.gcc_estimate_kbps, s.setpoint_kbps) {
        let gcc_below = gcc < sp * cfg.gcc_below_setpoint_frac;
        let sending_at_cap = s.bitrate_kbps >= sp * cfg.send_at_cap_frac;
        if gcc_below && (sending_at_cap || s.gcc_downshifted) && !encode_near_budget {
            return AdaptationState::NetworkCongested;
        }
    }

    let gcc_ok = match (s.gcc_estimate_kbps, s.setpoint_kbps) {
        (Some(gcc), Some(sp)) => gcc >= sp * cfg.gcc_below_setpoint_frac,
        // ABR disarmed: the host path can still be healthy if fps/encode/drops are fine.
        _ => true,
    };
    if fps_steady && !encode_near_budget && s.frames_dropped == 0 && gcc_ok {
        return AdaptationState::Healthy;
    }

    AdaptationState::Unknown
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A clean 60 fps window with ABR armed and the estimate at the ceiling.
    fn healthy() -> AdaptationSignals {
        AdaptationSignals {
            target_fps: 60,
            fps: 59.5,
            encode_ms_p50: Some(2.0),
            encode_ms_p95: Some(3.0),
            frames_dropped: 0,
            gcc_estimate_kbps: Some(8000.0),
            setpoint_kbps: Some(8000.0),
            bitrate_kbps: 7900.0,
            gcc_downshifted: false,
        }
    }

    #[test]
    fn clean_window_is_healthy() {
        assert_eq!(classify(&healthy()), AdaptationState::Healthy);
    }

    #[test]
    fn abr_disarmed_but_host_fine_is_healthy() {
        let s = AdaptationSignals {
            gcc_estimate_kbps: None,
            setpoint_kbps: None,
            ..healthy()
        };
        assert_eq!(classify(&s), AdaptationState::Healthy);
    }

    #[test]
    fn encode_near_budget_with_fps_drop_is_encoder_saturated() {
        let s = AdaptationSignals {
            fps: 45.0,
            encode_ms_p50: Some(19.0),
            encode_ms_p95: Some(20.0),
            ..healthy()
        };
        assert_eq!(classify(&s), AdaptationState::EncoderSaturated);
    }

    #[test]
    fn encode_near_budget_with_drops_is_encoder_saturated() {
        let s = AdaptationSignals {
            fps: 58.0,
            encode_ms_p95: Some(13.0), // > 0.70 × 16.67 ≈ 11.67
            frames_dropped: 4,
            ..healthy()
        };
        assert_eq!(classify(&s), AdaptationState::EncoderSaturated);
    }

    #[test]
    fn encoder_saturation_wins_over_congestion_when_both_present() {
        // #325 coupled loop: the encode spike must win over the GCC drag it also causes.
        let s = AdaptationSignals {
            fps: 44.0,
            encode_ms_p95: Some(21.0),
            frames_dropped: 2,
            gcc_estimate_kbps: Some(5000.0), // dragged below setpoint
            setpoint_kbps: Some(8000.0),
            bitrate_kbps: 7600.0,
            gcc_downshifted: true,
            ..healthy()
        };
        assert_eq!(classify(&s), AdaptationState::EncoderSaturated);
    }

    #[test]
    fn gcc_below_setpoint_at_cap_is_network_congested() {
        let s = AdaptationSignals {
            gcc_estimate_kbps: Some(5000.0), // < 0.85 × 8000 = 6800
            setpoint_kbps: Some(8000.0),
            bitrate_kbps: 7800.0,
            gcc_downshifted: true,
            ..healthy()
        };
        assert_eq!(classify(&s), AdaptationState::NetworkCongested);
    }

    #[test]
    fn gcc_below_setpoint_reinforced_by_downshift_only() {
        let s = AdaptationSignals {
            gcc_estimate_kbps: Some(4000.0),
            setpoint_kbps: Some(8000.0),
            bitrate_kbps: 4200.0, // below cap×0.85 — but gcc_downshifted carries it
            gcc_downshifted: true,
            ..healthy()
        };
        assert_eq!(classify(&s), AdaptationState::NetworkCongested);
    }

    #[test]
    fn gcc_below_setpoint_but_encoder_idle_is_not_congestion() {
        // No downshift and not sending at the cap: no evidence the pipe is constrained.
        let s = AdaptationSignals {
            gcc_estimate_kbps: Some(5000.0),
            setpoint_kbps: Some(8000.0),
            bitrate_kbps: 3000.0, // well below cap
            gcc_downshifted: false,
            ..healthy()
        };
        // gcc_ok is false ⇒ not Healthy, not Congested ⇒ Unknown.
        assert_eq!(classify(&s), AdaptationState::Unknown);
    }

    #[test]
    fn no_encode_samples_with_clean_network_is_healthy() {
        let s = AdaptationSignals {
            encode_ms_p50: None,
            encode_ms_p95: None,
            ..healthy()
        };
        assert_eq!(classify(&s), AdaptationState::Healthy);
    }

    #[test]
    fn fps_collapsed_without_encode_spike_is_unknown() {
        // Must not guess EncoderSaturated without the encode signal (could be a
        // client-side present limit, which is control-plane-only).
        let s = AdaptationSignals {
            fps: 30.0,
            encode_ms_p95: Some(3.0),
            ..healthy()
        };
        assert_eq!(classify(&s), AdaptationState::Unknown);
    }

    #[test]
    fn target_fps_drives_the_budget_not_a_hardcoded_60() {
        // At 30 fps the budget is 33.3 ms; a 13 ms encode is well UNDER 0.70×33.3≈23.3,
        // so the same 13 ms that saturates a 60 fps session is healthy at 30 fps.
        let s = AdaptationSignals {
            target_fps: 30,
            fps: 29.5,
            encode_ms_p95: Some(13.0),
            ..healthy()
        };
        assert_eq!(classify(&s), AdaptationState::Healthy);
    }

    // ---- config exposure: QUASAR_ADAPT_* classifier thresholds ----------------------
    // Process-global env vars; all cases in ONE serialized snapshot/restore test (no
    // serial_test dep, no other adaptation test touches env vars).

    fn restore(key: &str, prior: Option<String>) {
        match prior {
            Some(v) => std::env::set_var(key, v),
            None => std::env::remove_var(key),
        }
    }

    #[test]
    fn classifier_config_env_defaults_and_overrides() {
        let keys = [
            "QUASAR_ADAPT_ENCODE_BUDGET_FRAC",
            "QUASAR_ADAPT_FPS_STEADY_FRAC",
            "QUASAR_ADAPT_GCC_BELOW_FRAC",
            "QUASAR_ADAPT_SEND_AT_CAP_FRAC",
        ];
        let saved: Vec<(&str, Option<String>)> =
            keys.iter().map(|k| (*k, std::env::var(k).ok())).collect();
        for k in &keys {
            std::env::remove_var(k);
        }

        // (a) All unset ⇒ from_env EXACTLY equals the old hardcoded constants.
        let d = ClassifierConfig::from_env();
        assert_eq!(d.encode_budget_frac, ENCODE_BUDGET_FRAC);
        assert_eq!(d.fps_steady_frac, FPS_STEADY_FRAC);
        assert_eq!(d.gcc_below_setpoint_frac, GCC_BELOW_SETPOINT_FRAC);
        assert_eq!(d.send_at_cap_frac, SEND_AT_CAP_FRAC);
        // ...and the bare classify() uses those defaults (byte-identical fence).
        assert_eq!(
            classify(&healthy()),
            classify_with(&healthy(), &ClassifierConfig::default())
        );

        // (b) Each var set to a valid value is picked up.
        std::env::set_var("QUASAR_ADAPT_ENCODE_BUDGET_FRAC", "0.9");
        std::env::set_var("QUASAR_ADAPT_FPS_STEADY_FRAC", "0.75");
        std::env::set_var("QUASAR_ADAPT_GCC_BELOW_FRAC", "0.8");
        std::env::set_var("QUASAR_ADAPT_SEND_AT_CAP_FRAC", "0.95");
        let s = ClassifierConfig::from_env();
        assert_eq!(s.encode_budget_frac, 0.9);
        assert_eq!(s.fps_steady_frac, 0.75);
        assert_eq!(s.gcc_below_setpoint_frac, 0.8);
        assert_eq!(s.send_at_cap_frac, 0.95);

        // (c) Invalid / out-of-range values fall back to the default.
        std::env::set_var("QUASAR_ADAPT_ENCODE_BUDGET_FRAC", "0"); // not in (0,1]
        std::env::set_var("QUASAR_ADAPT_FPS_STEADY_FRAC", "1.5"); // > 1
        std::env::set_var("QUASAR_ADAPT_GCC_BELOW_FRAC", "junk"); // unparseable
        std::env::set_var("QUASAR_ADAPT_SEND_AT_CAP_FRAC", "-0.2"); // negative
        let f = ClassifierConfig::from_env();
        assert_eq!(f.encode_budget_frac, ENCODE_BUDGET_FRAC);
        assert_eq!(f.fps_steady_frac, FPS_STEADY_FRAC);
        assert_eq!(f.gcc_below_setpoint_frac, GCC_BELOW_SETPOINT_FRAC);
        assert_eq!(f.send_at_cap_frac, SEND_AT_CAP_FRAC);

        // 1.0 is a VALID upper bound (inclusive) for these fractions — not a fall-back.
        std::env::set_var("QUASAR_ADAPT_FPS_STEADY_FRAC", "1");
        assert_eq!(ClassifierConfig::from_env().fps_steady_frac, 1.0);

        for (k, prior) in saved {
            restore(k, prior);
        }
    }

    #[test]
    fn tuned_config_changes_the_classification() {
        let s = AdaptationSignals {
            fps: 55.0,                 // 55/60 ≈ 0.917 — steady at 0.85, not at 0.95
            encode_ms_p95: Some(13.0), // > 0.70×16.67 ≈ 11.67 ⇒ near budget
            ..healthy()
        };
        // Default: fps steady (0.917 ≥ 0.85), so not saturated despite near-budget encode.
        assert_ne!(classify(&s), AdaptationState::EncoderSaturated);
        // Raise the fps-steady bar to 0.95 ⇒ 0.917 is now "not steady" ⇒ EncoderSaturated.
        let cfg = ClassifierConfig {
            fps_steady_frac: 0.95,
            ..ClassifierConfig::default()
        };
        assert_eq!(classify_with(&s, &cfg), AdaptationState::EncoderSaturated);
    }

    #[test]
    fn state_strings_are_stable() {
        assert_eq!(AdaptationState::Healthy.as_str(), "healthy");
        assert_eq!(
            AdaptationState::NetworkCongested.as_str(),
            "network_congested"
        );
        assert_eq!(
            AdaptationState::EncoderSaturated.as_str(),
            "encoder_saturated"
        );
        assert_eq!(AdaptationState::Unknown.as_str(), "unknown");
    }
}
