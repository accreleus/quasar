//! GStreamer glue for the AS-03 adaptive-bitrate loop: arms `rtpgccbwe` on the
//! webrtcbin send path, runs the pure `abr::Governor` on each estimate, and applies
//! retargets to the live encoder (bitrate + single-frame VBV/CPB). Decision logic is
//! the pure half in `crate::session::abr`.

use std::sync::{Arc, Mutex};

use gstreamer as gst;
use gstreamer::prelude::*;

use crate::session::abr;
use crate::session::ladder;
use crate::session::metrics::SessionMetrics;
use crate::session::runner::TraceEvent;
use crate::session::{AbrMode, EncoderChoice, SessionConfig};

use super::encoders::{cpb_size_kbits, is_mutable_playing};
use super::scale_stage::ScaleStage;
use super::TraceTx;
use crate::session::Codec;

/// The armed ABR control loop: one [`abr::Governor`] plus the retarget side-effects
/// (encoder write + setpoint telemetry), driven from both observation sources — the
/// `notify::estimated-bitrate` handler (fires only while the estimate changes) and the
/// heartbeat-drain tick (stall rescue, ~5 s). `metrics` is weak: the tick hook lives
/// inside `SessionMetrics`, so a strong ref here would be a leak cycle.
///
/// SPT-02 `mode`: `Off` still attaches rtpgccbwe (so `transport.gcc_estimate_kbps`
/// flows for A/B) but discards every retarget — zero `abr.retarget` events.
struct AbrLoop {
    governor: Mutex<abr::Governor>,
    encoder: gst::Element,
    choice: EncoderChoice,
    /// Logging only; retarget property names key on `choice`, not codec.
    codec: Codec,
    /// The rate the encoder is CURRENTLY producing — divisor of the single-frame VBV/CPB
    /// budget (`cpb = kbps / fps`, SO-02). Atomic, not `cfg.stream.fps`: the D7 fps rung
    /// moves it live, and a stale 120 after a 120 → 60 step sizes every CPB wrong by 2x.
    fps: std::sync::atomic::AtomicU32,
    metrics: std::sync::Weak<SessionMetrics>,
    first_estimate: std::sync::atomic::AtomicBool,
    mode: AbrMode,
    trace_tx: Option<TraceTx>,
    session_id: String,
    /// The session's `rtpgccbwe`, so a floor move can lower the ESTIMATOR's `min-bitrate`
    /// too. Set once inside the `request-aux-sender` closure.
    ///
    /// Weak, and load-bearing: that element's notify closure holds a strong
    /// `Arc<AbrLoop>`, so a strong handle back is a GObject ref cycle GLib never collects
    /// (the ~505 MiB/session leak — `.claude/rules/gstreamer-gotchas.md`).
    bwe: Mutex<Option<glib::WeakRef<gst::Element>>>,
    /// One-shot latch for the "estimator handle is gone" warning.
    bwe_warned: std::sync::atomic::AtomicBool,
}

impl AbrLoop {
    fn fps(&self) -> u32 {
        self.fps.load(std::sync::atomic::Ordering::Relaxed)
    }

    /// D7: adopt a new encoded frame rate and re-derive the CPB at the current setpoint;
    /// the rate change alone leaves `cpb = kbps / fps` stale.
    ///
    /// Runs BEFORE the caps event reaches the encoder, so for one frame the CPB is sized
    /// for the new rate while the old rate is still emitting — the safe side of the race
    /// in both directions. Waiting for the caps event instead means real frames at the new
    /// rate against a CPB wrong by 2x.
    fn retune_for_fps(&self, fps: u32) {
        self.fps
            .store(fps.max(1), std::sync::atomic::Ordering::Relaxed);
        if self.mode == AbrMode::Off {
            return;
        }
        let kbps = self.governor.lock().unwrap().setpoint_kbps();
        write_encoder_bitrate(&self.encoder, self.choice, kbps, self.fps());
        tracing::info!(
            "SPT-08 fps rung: CPB re-derived for {} fps at {} kbps (cpb {} kbit)",
            self.fps(),
            kbps,
            cpb_size_kbits(kbps, self.fps())
        );
    }

    /// Amendment 5: move the governor's floor to follow the ladder rung; returns the floor
    /// now in effect. When the raise pushed the setpoint up the encoder is written
    /// immediately — deferring to the next observation would leave it at the smaller
    /// picture's bitrate while the ladder has already restored the bigger one. No-op in
    /// `Off` mode.
    fn set_floor(&self, floor_kbps: u32) -> u32 {
        if self.mode == AbrMode::Off {
            return floor_kbps;
        }
        let (old, raised, now) = {
            let mut gov = self.governor.lock().unwrap();
            let old = gov.floor_kbps();
            let raised = gov.set_floor_kbps(floor_kbps);
            (old, raised, gov.floor_kbps())
        };
        if now != old {
            tracing::info!(
                "SPT-08 ladder: ABR floor {old} → {now} kbps (following the rung; \
                 the floor is a function of the picture being sent, not of the launch size)"
            );
        }
        if let Some(setpoint) = raised {
            write_encoder_bitrate(&self.encoder, self.choice, setpoint, self.fps());
            if let Some(m) = self.metrics.upgrade() {
                m.set_abr_setpoint(setpoint);
            }
            tracing::info!(
                "SPT-08 ladder: the setpoint was below the restored floor — raised to \
                 {setpoint} kbps (cpb {} kbit)",
                cpb_size_kbits(setpoint, self.fps())
            );
        }
        // …and move the ESTIMATOR's own lower bound with it: the governor can only be as
        // free as the estimate it is fed. Run D (2026-08-17) without this walked the
        // governor floor 4000 → 1306 kbps while GCC stayed pinned at exactly the 4000 kbps
        // `min-bitrate` set at arm time — a null result.
        if now != old {
            match self.bwe.lock().unwrap().as_ref().and_then(|w| w.upgrade()) {
                Some(bwe) => {
                    if bwe.find_property("min-bitrate").is_some() {
                        bwe.set_property("min-bitrate", now.saturating_mul(1000));
                    }
                }
                // Warn once per session: without the estimator bound this degrades to the
                // run-D null result, indistinguishable in a report from a dead ladder.
                None => {
                    if !self
                        .bwe_warned
                        .swap(true, std::sync::atomic::Ordering::Relaxed)
                    {
                        tracing::warn!(
                            token = "abr-estimator-handle-gone",
                            "SPT-08 ladder: the ABR floor moved to {now} kbps but the \
                             rtpgccbwe handle is gone — the ESTIMATOR's min-bitrate stays at \
                             its arm-time value, so the setpoint cannot actually follow the \
                             rung below it (the amendment-5 mechanism is degraded for this \
                             session)"
                        );
                    }
                }
            }
        }
        now
    }

    /// Feed one estimate (kbps) through the governor; apply any retarget to the encoder
    /// and publish it. `from_notify` marks real-feedback observations vs tick re-observes.
    fn observe(&self, est_kbps: f64, from_notify: bool) {
        if from_notify
            && !self
                .first_estimate
                .swap(true, std::sync::atomic::Ordering::Relaxed)
        {
            tracing::info!(
                "AS-03 ABR live: first rtpgccbwe estimate {est_kbps:.0} kbps (TWCC feedback loop closed)"
            );
        }
        // SPT-01: the raw pre-smoothing, pre-deadband estimate; its gap to the setpoint is
        // the governor's contribution.
        if from_notify {
            if let Some(m) = self.metrics.upgrade() {
                m.set_gcc_estimate(est_kbps);
            }
        }
        let (prev_setpoint, retarget) = {
            let mut gov = self.governor.lock().unwrap();
            let prev = gov.setpoint_kbps();
            let r = gov.observe(est_kbps, std::time::Instant::now());
            (prev, r)
        };
        // In `Off` the governor still tracks, but its output is discarded. `Protective`
        // and `Smooth` both retarget; they differ only in the pure governor's down policy.
        if let Some(new_kbps) = retarget.filter(|_| self.mode != AbrMode::Off) {
            write_encoder_bitrate(&self.encoder, self.choice, new_kbps, self.fps());
            if let Some(m) = self.metrics.upgrade() {
                m.set_abr_setpoint(new_kbps);
            }
            tracing::info!(
                "AS-03 ABR retarget ({:?}/{}): estimate {:.0} kbps → setpoint {} kbps (cpb {} kbit)",
                self.choice,
                self.codec.as_str(),
                est_kbps,
                new_kbps,
                cpb_size_kbits(new_kbps, self.fps())
            );
            if let Some(ref tx) = self.trace_tx {
                let ts = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap_or_default()
                    .as_millis() as i64;
                let reason = if new_kbps < prev_setpoint {
                    "gcc_downshift"
                } else {
                    "recover"
                };
                let payload = serde_json::json!({
                    "from_kbps": prev_setpoint,
                    "to_kbps": new_kbps,
                    "reason": reason,
                });
                tx.try_emit(
                    self.session_id.clone(),
                    TraceEvent {
                        ts_unix_ms: ts,
                        event: "abr.retarget",
                        payload,
                    },
                );
            }
        }
    }
}

/// Whether `encoder` accepts the live writes a retarget needs (bitrate, plus the
/// single-frame VBV/CPB cap on hardware). AS-00: a retarget that cannot also retune CPB
/// must not be applied, so a missing or read-only property disarms the governor and the
/// encoder stays at static CBR. VA drivers vary; this guard is load-bearing.
fn encoder_abr_writable(encoder: &gst::Element, choice: EncoderChoice) -> bool {
    let writable = |name: &str| {
        encoder
            .find_property(name)
            .map(|p| p.flags().contains(glib::ParamFlags::WRITABLE))
            .unwrap_or(false)
    };
    match choice {
        // Software CBR has no separate VBV property to desync: bitrate-only is safe.
        EncoderChoice::Openh264 => writable("bitrate"),
        EncoderChoice::Va => writable("bitrate") && writable("cpb-size"),
        EncoderChoice::Nvenc => writable("bitrate") && writable("vbv-buffer-size"),
        // VK-05: vulkanh264enc has no VBV/CPB property (gst-inspect 1.28.4), so
        // bitrate-only. The live-bitrate patch is H.264-only, so vulkanh265enc's `bitrate`
        // can be present but not MUTABLE_PLAYING — require live writability, not presence,
        // or an h265 Vulkan session writes an inert setpoint instead of disarming.
        EncoderChoice::Vulkan => is_mutable_playing(encoder, "bitrate"),
    }
}

/// Apply a retarget: the new CBR `kbps` plus the single-frame VBV/CPB cap
/// (`cpb = kbps / fps`, SO-02), in the encoder's native unit. Every write is
/// writability-guarded so a read-only property never panics. The rate-control mode is
/// never touched — CBR stays CBR.
fn write_encoder_bitrate(encoder: &gst::Element, choice: EncoderChoice, kbps: u32, fps: u32) {
    let set_u32 = |name: &str, v: u32| {
        if encoder
            .find_property(name)
            .map(|p| p.flags().contains(glib::ParamFlags::WRITABLE))
            .unwrap_or(false)
        {
            encoder.set_property(name, v);
        }
    };
    let set_str = |name: &str, v: &str| {
        if encoder
            .find_property(name)
            .map(|p| p.flags().contains(glib::ParamFlags::WRITABLE))
            .unwrap_or(false)
        {
            encoder.set_property_from_str(name, v);
        }
    };
    let cpb = cpb_size_kbits(kbps, fps);
    match choice {
        EncoderChoice::Openh264 => set_u32("bitrate", kbps.saturating_mul(1000)), // bit/s
        EncoderChoice::Va => {
            set_u32("bitrate", kbps); // kbit/s
            set_u32("cpb-size", cpb); // kbits — one-frame VBV
        }
        EncoderChoice::Nvenc => {
            // nvcudah264enc wants property_from_str, matching make_nvenc_encoder.
            set_str("bitrate", &kbps.to_string()); // kbit/s
            set_str("vbv-buffer-size", &cpb.to_string()); // kbits — one-frame VBV
        }
        // No VBV/CPB property to retune.
        EncoderChoice::Vulkan => set_u32("bitrate", kbps),
    }
}

/// SPT-08 rung 2: whether `encoder` accepts the live speed-bias write (VA `target-usage`,
/// NVENC `preset`). Probed once at engage time so a read-only property never reaches the
/// panicking `set_property_from_str` path.
fn encoder_speed_bias_writable(encoder: &gst::Element, choice: EncoderChoice) -> bool {
    let writable = |name: &str| {
        encoder
            .find_property(name)
            .map(|p| p.flags().contains(glib::ParamFlags::WRITABLE))
            .unwrap_or(false)
    };
    match choice {
        EncoderChoice::Va => writable("target-usage"),
        EncoderChoice::Nvenc => writable("preset"),
        // No live speed lever: the rung is inert and the ladder defers to bitrate.
        // openh264's `complexity` is fixed "low" at build; no Vulkan equivalent verified.
        EncoderChoice::Openh264 => false,
        EncoderChoice::Vulkan => false,
    }
}

/// SPT-08 rung 2: bias the live encoder one step faster per rung (`bias` 0 = baseline).
/// A plain property write — no caps, graph, or renegotiation change; writability-guarded,
/// and the rate-control mode is never touched.
/// - VA `target-usage` 1 (quality) … 7 (speed): `base_target_usage` + rung, clamped to 7.
/// - NVENC `preset` p1 (fastest) … p7: p4 − rung, clamped to p1.
fn write_encoder_speed_bias(
    encoder: &gst::Element,
    choice: EncoderChoice,
    bias: u8,
    base_target_usage: u32,
) {
    let set_u32 = |name: &str, v: u32| {
        if encoder
            .find_property(name)
            .map(|p| p.flags().contains(glib::ParamFlags::WRITABLE))
            .unwrap_or(false)
        {
            encoder.set_property(name, v);
        }
    };
    let set_str = |name: &str, v: &str| {
        if encoder
            .find_property(name)
            .map(|p| p.flags().contains(glib::ParamFlags::WRITABLE))
            .unwrap_or(false)
        {
            encoder.set_property_from_str(name, v);
        }
    };
    match choice {
        EncoderChoice::Va => {
            let tu = (base_target_usage + bias as u32).clamp(1, 7);
            set_u32("target-usage", tu);
            tracing::info!("SPT-08 ladder: VA target-usage → {tu} (speed-bias rung {bias})");
        }
        EncoderChoice::Nvenc => {
            const NVENC_BASE_PRESET: u8 = 4;
            let p = NVENC_BASE_PRESET.saturating_sub(bias).max(1);
            set_str("preset", &format!("p{p}"));
            tracing::info!("SPT-08 ladder: NVENC preset → p{p} (speed-bias rung {bias})");
        }
        EncoderChoice::Openh264 => { /* inert */ }
        EncoderChoice::Vulkan => { /* inert */ }
    }
}

/// SPT-04 / #328: is this window's encoder saturation FRESH (chase the raw dip) or steady
/// state (smooth it)? `state` is `(was-saturated-last-window, EWMA baseline of encode p95
/// ms)`. Fresh requires saturated plus either a transition into saturation or a p95 rising
/// past the baseline by the up-ramp's 10% margin; the EWMA (alpha 0.3) always advances, so
/// steady-state saturation settles the baseline and stops reading as rising.
fn encoder_saturation_fresh(
    saturated: bool,
    encode_p95: Option<f64>,
    state: (bool, Option<f64>),
) -> (bool, (bool, Option<f64>)) {
    let (prev_saturated, baseline) = state;
    let rising = match (encode_p95, baseline) {
        (Some(p95), Some(b)) => p95 > b * 1.10,
        _ => false,
    };
    let new_baseline = match (encode_p95, baseline) {
        (Some(p95), Some(b)) => Some(0.3 * p95 + 0.7 * b),
        (Some(p95), None) => Some(p95),
        (None, b) => b,
    };
    let transition = saturated && !prev_saturated;
    let fresh = saturated && (transition || rising);
    (fresh, (saturated, new_baseline))
}

/// Per-session state of the SPT-08 resolution/fps rungs, carried between windows.
/// Bundled so the decide-and-actuate step is a free function testable against a fake
/// lever; the `on_window` closure itself needs a live webrtcbin.
struct ResRungState {
    /// Both rungs plus the ordering policy (`fps_enabled = false` makes it exactly the
    /// resolution rung).
    planner: ladder::LadderPlanner,
    /// Sticky: the RESOLUTION lever refused a step, so that rung is retired for the
    /// session (a refusing lever cannot start working mid-session). Not the lever's
    /// `pinned` flag, which belongs to the human — the per-window `set_pinned` would clear
    /// it and telemetry would mislabel an agent failure as a user pin.
    res_disabled: bool,
    /// The same for the FPS lever, and a SEPARATE flag: the two fail independently (an
    /// image without `videorate` has a working resolution lever and no rate lever), and
    /// one shared flag silently took the whole ladder down with it.
    fps_disabled: bool,
    /// Set for exactly one window after a step: `set_rung` is asynchronous, so
    /// `lever.current()` can still report the previous size and a resync would drag the
    /// machine back onto the rung it just left. Mirrors `ResolutionPolicy::settle_windows`.
    in_flight: bool,
}

/// One window of the resolution rung: resync to the live size, decide, actuate. `None`
/// when the machine held, was pinned/disabled, or the lever refused.
///
/// The resync is load-bearing: a manual `session_display_update` (D4) moves the size
/// underneath the machine, and without it a user who PATCHes back to the launch size gets
/// the next window stepping from the stale index. Sync ONLY when the live size differs —
/// `sync_rung` resets both dwell counters, so an unconditional call makes a contiguous
/// dwell impossible and the rung never fires.
fn observe_resolution_rung(
    st: &mut ResRungState,
    lever: &dyn ResolutionLever,
    human_pinned: bool,
    state: crate::session::adaptation::AdaptationState,
    setpoint_kbps: f64,
    floor_kbps: f64,
    now: std::time::Instant,
) -> Option<ladder::PlannedStep> {
    if st.in_flight {
        // Last window's step may not have renegotiated yet; skip the resync rather than
        // adopt the pre-step size.
        st.in_flight = false;
    } else {
        let live = lever.current();
        if st.planner.res_size() != live {
            tracing::info!(
                "SPT-08 resolution rung: the external size moved outside the ladder \
                 (now {}x{}) — resyncing the machine",
                live.0,
                live.1
            );
            st.planner.sync_res(live);
        }
    }
    // A human pin freezes the whole picture; a refused lever is retired individually so
    // the other rung keeps stepping.
    st.planner.set_res_retired(st.res_disabled);
    st.planner.set_fps_retired(st.fps_disabled);
    st.planner.set_pinned(human_pinned);
    let planned = st.planner.observe(state, setpoint_kbps, floor_kbps, now)?;
    let (what, actuated) = match planned {
        ladder::PlannedStep::Resolution(_) => {
            let (w, h) = st.planner.res_size();
            (format!("{w}x{h}"), lever.set_rung(w, h))
        }
        ladder::PlannedStep::Fps(_) => {
            let fps = st.planner.fps();
            (format!("{fps} fps"), lever.set_fps(fps))
        }
    };
    match actuated {
        Ok(_) => {
            st.in_flight = true;
            Some(planned)
        }
        Err(e) => {
            // A refusing lever never gets a second chance (retrying every window spams the
            // log and desyncs the machine). Retire ONLY the lever that refused.
            let which = match planned {
                ladder::PlannedStep::Resolution(_) => {
                    st.res_disabled = true;
                    "resolution"
                }
                ladder::PlannedStep::Fps(_) => {
                    st.fps_disabled = true;
                    "fps"
                }
            };
            tracing::warn!(
                token = "abr-rung-retired",
                "SPT-08 ladder: the {which} lever refused {what} ({e:#}) — the {which} rung is \
                 retired for this session; the other rung is unaffected"
            );
            None
        }
    }
}

/// Amendment 5: the absolute floor for this session. Only a host/env floor qualifies
/// (`QUASAR_ABR_FLOOR_KBPS` / the `abr_floor_kbps` host setting); the profile's
/// `stream.abr_floor_kbps` describes the picture that profile renders, so it is the launch
/// rung's floor and travels with the rung ([`ladder::FloorFollow`]).
///
/// MUST read the two inputs separately, never the merged
/// [`SessionConfig::abr_floor_kbps`] in which a non-zero wire floor wins — that made the
/// host floor unreachable on every profile-launched session. Guarded by
/// `only_a_host_floor_is_absolute`.
fn operator_floor(cfg: &SessionConfig) -> Option<u32> {
    cfg.host_abr_floor_kbps.filter(|&n| n > 0)
}

/// D6 `abr.ladder.step` context: the governor's bitrate at an actuated rung change.
/// Both numbers are needed — a setpoint that did not move after a step is a choice above
/// the floor and a pin at it, and since amendment 5 the floor is not a session constant.
#[derive(Debug, Clone, Copy)]
struct StepRates {
    setpoint_kbps: f64,
    floor_kbps: f64,
}

fn emit_ladder_step(
    trace_tx: &TraceTx,
    session_id: &str,
    rung: &str,
    from: i64,
    to: i64,
    reason: &str,
    rates: StepRates,
) {
    let StepRates {
        setpoint_kbps,
        floor_kbps,
    } = rates;
    let ts = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as i64;
    trace_tx.try_emit(
        session_id.to_string(),
        TraceEvent {
            ts_unix_ms: ts,
            event: "abr.ladder.step",
            payload: serde_json::json!({
                "rung": rung, "from": from, "to": to,
                "reason": reason, "setpoint_kbps": setpoint_kbps.round() as i64,
                // The floor IN EFFECT after this step (see `StepRates`).
                "floor_kbps": floor_kbps.round() as i64,
            }),
        },
    );
}

/// Arm the AS-03 ABR governor on `webrtc`'s send path, retargeting `encoder`. Knobs:
/// `QUASAR_ABR`, `QUASAR_ABR_MODE`.
///
/// No-op (static CBR) when the mode selects disarm, when `rtpgccbwe` is absent from the
/// image, or when the encoder won't take live bitrate+CPB writes — each disarm logs once.
/// Otherwise `request-aux-sender` hands webrtcbin a per-session `rtpgccbwe` bound to
/// `[floor, ceiling]` and each `estimated-bitrate` notification runs the governor.
pub(super) fn enable_abr(
    webrtc: &gst::Element,
    encoder: &gst::Element,
    cfg: &SessionConfig,
    session_metrics: Arc<SessionMetrics>,
    trace_tx: TraceTx,
    session_id: String,
    // The session's external-resolution lever, or `None` when this encode path has no
    // live-resize (Vulkan) — then the resolution rung is inert and says so once.
    lever: Option<Arc<EncodeResolutionLever>>,
) {
    let mode = cfg.abr_mode;
    // #502: a rung armed on this host that this session's mode cannot run is the last
    // silent ladder gate — say it before any early return, in the log of the session that
    // was expected to step.
    if let Some(why) = cfg.ladder_gate_warning() {
        tracing::warn!(token = "abr-disarmed", "{why}");
    }
    // Both modes need ceiling/floor/fps: `Off` still bounds the rtpgccbwe estimator and
    // still runs a Governor so the tick stall-rescue can re-observe (its output is
    // discarded in `AbrLoop::observe`).
    let abr_cfg = match cfg.abr_config() {
        Some(c) => c,
        None => {
            let ceiling = cfg.stream.bitrate_kbps;
            let fps = cfg.stream.fps.max(1) as u32;
            abr::AbrConfig::new(ceiling.saturating_sub(1).max(500), ceiling, fps)
        }
    };

    if gst::ElementFactory::find("rtpgccbwe").is_none() {
        tracing::warn!(
            token = "abr-rtpgccbwe-missing",
            "AS-03 ABR ({}) requested but `rtpgccbwe` is not registered (gst-plugins-rs \
             `rtp` plugin missing from the image) — GCC telemetry unavailable; encoder \
             stays at static CBR",
            mode.as_str()
        );
        return;
    }
    if mode != AbrMode::Off && !encoder_abr_writable(encoder, cfg.encoder) {
        tracing::warn!(
            token = "abr-encoder-not-retargetable",
            "AS-03 ABR ({}) requested but the {:?} encoder will not accept live \
             bitrate+CPB writes — disarming; encoder stays at static CBR",
            mode.as_str(),
            cfg.encoder
        );
        return;
    }

    let choice = cfg.encoder;
    let fps = abr_cfg.fps;
    let floor_bps = (abr_cfg.floor_kbps as u64).saturating_mul(1000);
    let ceiling_bps = (abr_cfg.ceiling_kbps as u64).saturating_mul(1000);
    // SPT-08 ladder config: `Some` in Smooth mode whenever any rung is armed (#502;
    // `abr_ladder=false` only retires the speed-bias rung, via `max_bias = 0`).
    let ladder_cfg = cfg.ladder_config();
    // Amendment 5 floor policy, built once so it is a `Copy` value the `on_window` closure
    // can hold and is anchored on the same launch floor the governor was built with. With
    // no ladder config `FloorFollow::floor_for` returns the launch floor verbatim.
    let launch_floor_kbps = abr_cfg.floor_kbps;
    let floor_follow = ladder::FloorFollow {
        enabled: ladder_cfg.map(|lc| lc.floor_follows_rung).unwrap_or(false),
        launch_floor_kbps,
        operator_floor_kbps: operator_floor(cfg),
        ceiling_kbps: abr_cfg.ceiling_kbps,
        launch_px: cfg.stream.width as i64 * cfg.stream.height as i64,
        exponent: ladder_cfg
            .map(|lc| lc.res.exponent)
            .unwrap_or(ladder::ResolutionPolicy::DEFAULT_EXPONENT),
    };
    // D7: the fps rung's rung-0 and ceiling. Not `abr_cfg.fps`, which is the governor's
    // CPB divisor and moves with the rung.
    let launch_fps = cfg.stream.fps;
    let ladder_encoder = encoder.clone();
    let base_target_usage = cfg.target_usage;
    // SPT-08 rung 4: resolve once here, so an invalid policy or a lever-less path is
    // reported at arm time rather than silently per-window.
    //
    // Either rung is enough to build it: the planner carries both levers, and gating on
    // `resolution_enabled` alone made `abr_ladder_fps=1` a silent no-op. Fps-only seeds
    // the resolution lever RETIRED (`res_disabled`) so every window falls through to fps.
    let res_rung_seed = ladder_cfg
        .filter(|lc| lc.resolution_enabled || lc.fps_enabled)
        .and_then(|lc| match lever.clone() {
            None => {
                tracing::info!(
                    "SPT-08 resolution/fps rungs requested but this session has no live-resize \
                     lever ({:?}) — both rungs inert for the session",
                    cfg.encoder
                );
                None
            }
            Some(l) => {
                let rungs = l.available_rungs();
                // The resolution policy also feeds the fps rung (same comfort table and
                // dwell thresholds), so an invalid policy disables both, fps-only included.
                match lc.res.validate(&rungs, abr_cfg.ceiling_kbps) {
                    Ok(()) => Some((l, lc, rungs)),
                    Err(why) => {
                        tracing::warn!(
                            token = "abr-resolution-policy-invalid",
                            "SPT-08 resolution + fps rungs DISABLED for this session: invalid \
                             resolution policy ({why})"
                        );
                        None
                    }
                }
            }
        });
    // Cloned before `trace_tx` / `session_id` move into the `AbrLoop` below.
    let trace_tx_for_ladder = trace_tx.clone();
    let session_id_for_ladder = session_id.clone();
    // Publish the ceiling as the initial setpoint so the admin chart shows ABR state from
    // the first window, before any retarget.
    session_metrics.set_abr_setpoint(abr_cfg.ceiling_kbps);
    let codec = cfg.stream.codec;
    let abr_loop = Arc::new(AbrLoop {
        governor: Mutex::new(abr::Governor::new(abr_cfg, std::time::Instant::now())),
        encoder: encoder.clone(),
        choice,
        codec,
        fps: std::sync::atomic::AtomicU32::new(fps),
        metrics: Arc::downgrade(&session_metrics),
        first_estimate: std::sync::atomic::AtomicBool::new(false),
        mode,
        trace_tx: Some(trace_tx),
        session_id,
        bwe: Mutex::new(None),
        bwe_warned: std::sync::atomic::AtomicBool::new(false),
    });

    // webrtcbin emits request-aux-sender once per send RTP session (one transport under
    // max-bundle). Guard to exactly one rtpgccbwe so two estimators can never fight over
    // the same encoder.
    let created = Arc::new(std::sync::atomic::AtomicBool::new(false));
    webrtc.connect("request-aux-sender", false, move |_values| {
        if created.swap(true, std::sync::atomic::Ordering::SeqCst) {
            return None; // already wired — let webrtcbin use its default for any extra session
        }
        let bwe = match gst::ElementFactory::make("rtpgccbwe").build() {
            Ok(e) => e,
            Err(e) => {
                tracing::warn!(
                    token = "abr-rtpgccbwe-build-failed","AS-03: rtpgccbwe build failed: {e}; ABR disarmed for this session");
                return None;
            }
        };
        // Keep the estimator's own search within the tier bounds (bps); guarded in case a
        // future plugin rev renames these.
        if bwe.find_property("min-bitrate").is_some() {
            bwe.set_property("min-bitrate", floor_bps as u32);
        }
        if bwe.find_property("max-bitrate").is_some() {
            bwe.set_property("max-bitrate", ceiling_bps as u32);
        }
        // Seed the initial estimate AT THE CEILING. Unseeded, rtpgccbwe starts at
        // min-bitrate and ramps, throttling every session to the floor for its first ~30 s
        // on a clean LAN (measured). Seeded, it only walks down on real congestion.
        if bwe.find_property("estimated-bitrate").is_some() {
            bwe.set_property("estimated-bitrate", ceiling_bps as u32);
        }

        // Weak handle, so a ladder-driven floor move can lower `min-bitrate` too (see
        // `AbrLoop::set_floor`). Weak because the notify closure below holds a strong
        // `Arc<AbrLoop>` — a strong handle back is a ref cycle.
        *abr_loop.bwe.lock().unwrap() = Some(bwe.downgrade());

        // Fast path: react within the 2 s hysteresis while estimates are changing.
        let notify_loop = abr_loop.clone();
        bwe.connect_notify(Some("estimated-bitrate"), move |bwe, _pspec| {
            // `estimated-bitrate` is bits/s; the governor works in kbps.
            let est_bps: u32 = bwe.property("estimated-bitrate");
            notify_loop.observe(est_bps as f64 / 1000.0, true);
        });
        // Stall rescue: rtpgccbwe notifies only on CHANGE, and GCC pins its estimate under
        // sustained loss and at max-bitrate, so a notification-only governor strands
        // mid-descent or mid-recovery (both measured). Re-observe on the existing
        // heartbeat-drain (~5 s); the hook dies with `SessionMetrics`, so there is no
        // per-session thread and no teardown plumbing.
        let tick_loop = abr_loop.clone();
        let tick_bwe = bwe.clone();
        if let Some(m) = tick_loop.metrics.upgrade() {
            m.set_on_drain(Box::new(move || {
                let est_bps: u32 = tick_bwe.property("estimated-bitrate");
                tick_loop.observe(est_bps as f64 / 1000.0, false);
            }));
        }

        // SPT-04: in Smooth mode derive the governor's per-window AdaptationHint from the
        // SPT-03 classifier label plus a rolling encode-time baseline, for the NEXT
        // observe. #328: a steady Renoir at its ceiling trips `encoder_saturated` every
        // window, so saturation counts as fresh only on a transition or a rising p95.
        if mode == AbrMode::Smooth {
            let hint_loop = abr_loop.clone();
            // SPT-08: the ladder MUST share this same `on_window` closure — the slot is
            // single-registration, last wins, so a second hook would silently replace this
            // one. The speed-bias rung engages only when the rung is armed (`max_bias > 0`)
            // and the encoder takes the live write; bitrate stays the governor's, and the
            // resolution/fps rungs are seeded below independently of this rung.
            let ladder_state = ladder_cfg
                .filter(|lc| lc.max_bias > 0)
                .filter(|_| encoder_speed_bias_writable(&ladder_encoder, choice))
                .map(|lc| {
                    tracing::info!(
                        "SPT-08 ladder engaged ({:?}): encoder speed-bias rung active \
                         (max {} steps); fps rung {}, resolution rung {} (deferred — not actuated)",
                        choice,
                        lc.max_bias,
                        if lc.fps_enabled { "requested" } else { "off" },
                        if lc.resolution_enabled {
                            "requested"
                        } else {
                            "off"
                        },
                    );
                    Mutex::new(ladder::Ladder::new(lc))
                });
            let res_state = res_rung_seed.clone().map(|(lever, lc, rungs)| {
                let policy = lc.res;
                let planner = ladder::LadderPlanner::new(
                    &lc,
                    rungs.clone(),
                    launch_fps,
                    abr_cfg.ceiling_kbps,
                    std::time::Instant::now(),
                );
                tracing::info!(
                    "SPT-08 ladder rungs ENGAGED: resolution rung {}, {} rungs {:?}, k={}, \
                     engage {}×B for {} windows, recover {}×B for {} windows, min step {}s, \
                     floor {}p; fps rung {} (order {})",
                    if lc.resolution_enabled { "on" } else { "OFF (retired — fps-only)" },
                    rungs.len(), rungs, policy.exponent, policy.engage_frac, policy.engage_dwell,
                    policy.recover_frac, policy.recover_dwell, policy.min_step_s, policy.min_height,
                    if planner.fps_rung_active() {
                        format!("ENGAGED ({launch_fps} → {})", launch_fps / 2)
                    } else if lc.fps_enabled {
                        format!("inert ({launch_fps} fps admits no halving)")
                    } else {
                        "off".to_string()
                    },
                    lc.order.as_str(),
                );
                tracing::info!(
                    "SPT-08 ladder: ABR floor follows the rung = {} (launch floor {} kbps, \
                     operator floor {:?})",
                    floor_follow.enabled,
                    floor_follow.launch_floor_kbps,
                    floor_follow.operator_floor_kbps,
                );
                (
                    Mutex::new(ResRungState {
                        planner,
                        // Retirement, not a pin, is what makes a rung ineligible for
                        // selection, so an fps-only `hybrid` window falls through to fps.
                        res_disabled: !lc.resolution_enabled,
                        fps_disabled: false,
                        in_flight: false,
                    }),
                    lever,
                )
            });
            // The last actuated speed-bias rung: `Ladder` returns only the new level, and
            // the trace event needs a real `from` plus a direction-derived reason.
            let last_bias = std::sync::atomic::AtomicU8::new(0);
            let res_trace_tx = trace_tx_for_ladder.clone();
            let res_session_id = session_id_for_ladder.clone();
            if let Some(m) = hint_loop.metrics.upgrade() {
                // (was-saturated-last-window, EWMA baseline of encode p95 ms).
                let roll = Arc::new(Mutex::new((false, None::<f64>)));
                // The outer request-aux-sender closure is `Fn` and must keep its own
                // `ladder_encoder`.
                let window_encoder = ladder_encoder.clone();
                m.set_on_window(Box::new(move |state, encode_p95| {
                    use crate::session::adaptation::AdaptationState;
                    let saturated = matches!(state, AdaptationState::EncoderSaturated);
                    let fresh = {
                        let mut st = roll.lock().unwrap();
                        let (f, new_state) = encoder_saturation_fresh(saturated, encode_p95, *st);
                        *st = new_state;
                        f
                    };
                    let hint = abr::AdaptationHint {
                        network_congested: matches!(state, AdaptationState::NetworkCongested),
                        encoder_saturation_fresh: fresh,
                    };
                    hint_loop.governor.lock().unwrap().set_adaptation_hint(hint);
                    // Read once for both rungs: the resolution rung's input signal and both
                    // rungs' trace context. Neither rung ever writes bitrate.
                    let setpoint = hint_loop.governor.lock().unwrap().setpoint_kbps() as f64;

                    // Runs after the hint feed so the governor's bitrate lever is set
                    // first. The bias rung escalates only on `EncoderSaturated`, never on
                    // a network dip.
                    if let Some(ref lad) = ladder_state {
                        if let Some(bias) = lad.lock().unwrap().observe(state) {
                            write_encoder_speed_bias(
                                &window_encoder,
                                choice,
                                bias,
                                base_target_usage,
                            );
                            if let Some(m) = hint_loop.metrics.upgrade() {
                                m.set_ladder_bias(bias);
                            }
                            let prev =
                                last_bias.swap(bias, std::sync::atomic::Ordering::Relaxed);
                            emit_ladder_step(
                                &res_trace_tx,
                                &res_session_id,
                                "bias",
                                prev as i64,
                                bias as i64,
                                if bias > prev { "engage" } else { "recover" },
                                StepRates {
                                    setpoint_kbps: setpoint,
                                    floor_kbps: hint_loop
                                        .governor
                                        .lock()
                                        .unwrap()
                                        .floor_kbps()
                                        as f64,
                                },
                            );
                        }
                    }

                    // Rung 4: pixels, decided off the governor's setpoint. After the bias
                    // rung; the two are independent signals (encoder saturation vs
                    // bandwidth) and may both engage in one window.
                    if let Some((ref rung, ref lever)) = res_state {
                        let mut st = rung.lock().unwrap();
                        // The rung's "governor pinned at its floor" emergency test must
                        // read the LIVE floor: it moves with the rung, and a stale higher
                        // value fires the emergency on a setpoint nowhere near pinned.
                        let live_floor = hint_loop.governor.lock().unwrap().floor_kbps() as f64;
                        let decision = observe_resolution_rung(
                            &mut st,
                            &**lever,
                            lever.pinned(),
                            state,
                            setpoint,
                            live_floor,
                            std::time::Instant::now(),
                        );
                        // Re-derive the floor from wherever the rung ENDED this window,
                        // unconditionally: a manual `session_display_update` moves the size
                        // underneath the machine and the resync above adopts it, a rung
                        // change with no `PlannedStep` to hang the update off.
                        let (rung_w, rung_h) = st.planner.res_size();
                        let floor_now = hint_loop.set_floor(floor_follow.floor_for(
                            rung_w as i64 * rung_h as i64,
                            st.planner.fps_rung() > 0,
                        )) as f64;
                        if let Some(m) = hint_loop.metrics.upgrade() {
                            // Omit-when-default: 0 ⇒ "at the launch floor".
                            m.set_abr_floor(if floor_now as u32 == launch_floor_kbps {
                                0
                            } else {
                                floor_now as u32
                            });
                        }
                        // Every window, not only on a step — same resync reason as above:
                        // otherwise the echo reports the stale rung until the next step.
                        if let Some(m) = hint_loop.metrics.upgrade() {
                            m.set_ladder_res_rung(st.planner.res_rung());
                        }
                        match decision {
                            Some(ladder::PlannedStep::Resolution(d)) => {
                                let (w, h) = st.planner.res_size();
                                drop(st);
                                tracing::info!(
                                    "SPT-08 ladder step: resolution rung {} → {} ({}), \
                                     {w}x{h}, setpoint {setpoint:.0} kbps",
                                    d.from,
                                    d.to,
                                    d.reason.as_str()
                                );
                                if let Some(m) = hint_loop.metrics.upgrade() {
                                    m.set_ladder_res_rung(d.to);
                                }
                                emit_ladder_step(
                                    &res_trace_tx,
                                    &res_session_id,
                                    "resolution",
                                    d.from as i64,
                                    d.to as i64,
                                    d.reason.as_str(),
                                    StepRates {
                                        setpoint_kbps: setpoint,
                                        floor_kbps: floor_now,
                                    },
                                );
                            }
                            // D7: `from`/`to` are frame RATES here, not indices.
                            Some(ladder::PlannedStep::Fps(d)) => {
                                drop(st);
                                tracing::info!(
                                    "SPT-08 ladder step: fps rung {} → {} fps ({}), \
                                     setpoint {setpoint:.0} kbps",
                                    d.from,
                                    d.to,
                                    d.reason.as_str()
                                );
                                // `cpb = kbps / fps`: a rate write alone leaves the
                                // per-frame VBV budget wrong by 2x.
                                hint_loop.retune_for_fps(d.to as u32);
                                if let Some(m) = hint_loop.metrics.upgrade() {
                                    // Omit-when-default: 0 ⇒ "at the launch rate".
                                    m.set_ladder_fps(if d.to as i32 == launch_fps {
                                        0
                                    } else {
                                        d.to as i32
                                    });
                                    // Move the CLASSIFIER's expectation with it: its
                                    // host-fps-steady guard trips `Unknown` below
                                    // `target × 0.83`, an `Unknown` window resets both
                                    // rungs' dwell counters, and a stale target therefore
                                    // freezes the ladder at the rung it just took
                                    // (measured live 2026-08-16).
                                    m.set_target_fps(d.to as u32);
                                }
                                emit_ladder_step(
                                    &res_trace_tx,
                                    &res_session_id,
                                    "fps",
                                    d.from as i64,
                                    d.to as i64,
                                    d.reason.as_str(),
                                    StepRates {
                                        setpoint_kbps: setpoint,
                                        floor_kbps: floor_now,
                                    },
                                );
                            }
                            None => {}
                        }
                    }
                }));
            }
        }
        tracing::info!(
            "AS-03 ABR armed (mode={}): rtpgccbwe → {:?} encoder, codec={}, bounds [{}, {}] kbps @ {} fps",
            mode.as_str(),
            choice,
            codec.as_str(),
            floor_bps / 1000,
            ceiling_bps / 1000,
            fps
        );
        // The aux-sender return MUST be handed back FLOATING: rtpbin ref_sinks it on
        // bin-add expecting a C handler's floating ref, and gstreamer-rs already sank one
        // at construction, so a plain strong handle leaks the rtpgccbwe (and its
        // AbrLoop → encoder chain) per session (.claude/rules/gstreamer-gotchas.md).
        unsafe {
            gst::glib::gobject_ffi::g_object_force_floating(
                bwe.as_ptr() as *mut gst::glib::gobject_ffi::GObject
            );
        }
        Some(bwe.to_value())
    });
}

// ─────────────────────────────────────────────────────────────────────────────
// Adaptive external resolution: the resolution lever, as an interface
// ─────────────────────────────────────────────────────────────────────────────

/// The second knob a congestion controller can turn: frame size, not bitrate.
/// `LadderConfig::resolution_enabled` is `false` by default; the other caller is the
/// runner's `session_display_update.stream_*` path (an explicit operator/user request).
pub trait ResolutionLever: Send + Sync {
    /// The external sizes this session may be stepped to, descending, launch size first.
    fn available_rungs(&self) -> Vec<(i32, i32)>;
    /// The size the encoder input is negotiated at right now.
    fn current(&self) -> (i32, i32);
    /// Step to `w` x `h`. `Ok(false)` ⇒ already there (a no-op, no IDR, no renegotiation).
    /// `Err` ⇒ refused (unsupported encode path, or not a legal size).
    fn set_rung(&self, w: i32, h: i32) -> anyhow::Result<bool>;
    /// D7: step the encoded frame RATE (120 → 60 and back). `Ok(false)` ⇒ already there.
    /// Defaults to a refusal; the ladder retires the rung for the session on the first
    /// `Err`, so an implementor with no rate lever needs no change.
    fn set_fps(&self, _fps: i32) -> anyhow::Result<bool> {
        Err(anyhow::anyhow!("this lever has no fps rung"))
    }
}

/// The one implementation: the encode-side [`ScaleStage`], the IDR that must follow a real
/// change, and the completion callback reporting the size the graph negotiated.
///
/// The resize completes asynchronously, so all three hang off one CAPS probe: a `set_size`
/// without the paired keyframe leaves the receiver decoding at the wrong size until the
/// next scheduled IDR, and a result read back immediately reports the PREVIOUS size.
pub struct EncodeResolutionLever {
    stage: ScaleStage,
    /// A strong ref is safe: nothing on the encoder holds this struct, so there is no
    /// GObject ref cycle, and the one-shot probe captures only `on_resize`.
    encoder: gst::Element,
    /// Called from the streaming thread once a resize has landed, with the size off the
    /// CAPS event; the runner writes the `session_metrics` echo from it.
    ///
    /// MUST NOT hold a strong `Arc<SessionMetrics>`: the probe calling it is on the
    /// encoder's pad and `SessionMetrics` holds ABR hooks that hold the encoder — the
    /// cross ref cycle behind the ~505 MiB/session VRAM leak. The runner passes a `Weak`.
    on_resize: Arc<dyn Fn((i32, i32)) + Send + Sync>,
    /// D4 ownership: set once a human has chosen a non-launch size via
    /// `PATCH /v1/sessions/{id}/display`. The resolution rung emits nothing while it is
    /// set. Released by a PATCH back to the launch size; there is no separate wire field.
    /// Atomic because the runner thread writes it and the heartbeat/drain task reads it.
    pinned: std::sync::atomic::AtomicBool,
    /// Serialises `would_change → arm probe → set_size` so the two writers (the runner's
    /// PATCH path and the ladder's `on_window` closure) can never interleave.
    /// `ScaleStage::set_size` is safe while PLAYING but carries no lock, and a non-atomic
    /// arm/set pairs one step's completion probe with the other's caps event.
    step_lock: Mutex<()>,
    /// The completion probe runs on a STREAMING thread and must not read the graph or emit
    /// there (#270); it parks the trigger here for the runner's 100 ms tick.
    renegotiated: Arc<RenegotiationSignal>,
}

/// The `caps.negotiated` `trigger` values (`protocol/agent-api.md`) — a closed set on the
/// wire, so they live in one place.
pub mod trigger {
    /// First negotiation of the session, with the first offer.
    pub const LAUNCH: &str = "launch";
    /// The ABR ladder's resolution rung stepped the external size.
    pub const RUNG_STEP: &str = "rung_step";
    /// An explicit `session_display_update.stream_*` resize.
    pub const SCALE_REBUILD: &str = "scale_rebuild";
    /// A launcher↔game source swap completed.
    pub const SOURCE_SWAP: &str = "source_swap";
}

/// A completed encode-branch renegotiation the runner has not reported yet.
///
/// One coalescing slot: two steps between runner ticks report the LATER trigger and one
/// event. `caps.negotiated` states what the branch agreed now and the runner reads the
/// live caps when it emits, so a queue of stale triggers would describe caps that are gone.
#[derive(Default)]
pub struct RenegotiationSignal {
    pending: Mutex<Option<&'static str>>,
}

impl RenegotiationSignal {
    /// Called from the streaming thread (the resize-completion probe): stores a
    /// `&'static str` and nothing else — no allocation, no graph read, no emit.
    pub fn record(&self, trigger: &'static str) {
        *self.pending.lock().unwrap() = Some(trigger);
    }

    /// Called from the runner's supervision tick. `Some(trigger)` ⇒ emit one
    /// `caps.negotiated`.
    pub fn take(&self) -> Option<&'static str> {
        self.pending.lock().unwrap().take()
    }
}

impl EncodeResolutionLever {
    pub fn new<F>(stage: ScaleStage, encoder: gst::Element, on_resize: F) -> Self
    where
        F: Fn((i32, i32)) + Send + Sync + 'static,
    {
        Self {
            stage,
            encoder,
            on_resize: Arc::new(on_resize),
            pinned: std::sync::atomic::AtomicBool::new(false),
            step_lock: Mutex::new(()),
            renegotiated: Arc::new(RenegotiationSignal::default()),
        }
    }

    /// The `caps.negotiated` hand-off slot; the runner polls it on its 100 ms tick.
    pub fn renegotiation(&self) -> &RenegotiationSignal {
        &self.renegotiated
    }

    /// [`ResolutionLever::set_rung`] with an explicit `caps.negotiated` trigger. The
    /// trigger is captured inside the `step_lock`, in this step's completion closure, so it
    /// can never be paired with another stepper's caps event. The runner's PATCH path
    /// passes [`trigger::SCALE_REBUILD`]; the ladder-facing `set_rung` defaults to
    /// [`trigger::RUNG_STEP`].
    pub fn set_rung_tagged(&self, w: i32, h: i32, trigger: &'static str) -> anyhow::Result<bool> {
        self.step(w, h, trigger)
    }

    fn step(&self, w: i32, h: i32, trigger: &'static str) -> anyhow::Result<bool> {
        let _step = self.step_lock.lock().unwrap();
        // Validate and no-op-check BEFORE arming: a probe armed for a rejected or no-op
        // request sits on the pad until teardown and then fires on an unrelated later
        // renegotiation.
        if !self.stage.would_change(w, h)? {
            return Ok(false);
        }
        // Arm BEFORE setting the caps. The streaming thread can push the renegotiating
        // buffer between the property set and `add_probe`, and a probe armed afterwards
        // waits forever: no IDR, no echo, and a leaked probe.
        let on_resize = self.on_resize.clone();
        let renegotiated = self.renegotiated.clone();
        let armed = super::resize::arm_on_next_caps(&self.encoder, move |pad, size| {
            super::resize::force_idr_from_pad(pad);
            // Streaming thread: park the trigger, the runner reads the graph and emits
            // `caps.negotiated` on its own tick (#270).
            renegotiated.record(trigger);
            match size {
                Some(size) => on_resize(size),
                // Should not happen on a raw video pad; skip the echo, don't guess.
                None => tracing::warn!(
                    token = "abr-retarget-caps-no-size",
                    "external resolution: the renegotiated caps carried no width/height — \
                     the metrics echo keeps its previous value"
                ),
            }
        });
        if armed.is_none() {
            tracing::warn!(
                token = "abr-retarget-probe-unarmed",
                "external resolution: could not arm the resize-completion probe — forcing an \
                 IDR now (it may land on a frame at the previous size) and leaving the echo"
            );
            super::resize::force_idr(&self.encoder);
        }

        match self.stage.set_size(w, h) {
            Ok(true) => Ok(true),
            // Unreachable after `would_change` said yes (this is the only writer), but if
            // it ever happens, take the probe back off the pad.
            other => {
                if let Some(armed) = armed {
                    armed.disarm();
                }
                other
            }
        }
    }

    /// The stage itself — the runner reads `supported` / `launch` off it.
    pub fn stage(&self) -> &ScaleStage {
        &self.stage
    }

    /// Mark/clear the manual pin; see the `pinned` field.
    pub fn set_pinned(&self, pinned: bool) {
        self.pinned
            .store(pinned, std::sync::atomic::Ordering::Relaxed);
    }

    pub fn pinned(&self) -> bool {
        self.pinned.load(std::sync::atomic::Ordering::Relaxed)
    }
}

/// The session's resolution lever with its `session_metrics` echo wired in. Must be
/// constructed inside `build_encode_pipeline`: `enable_abr` needs it and runs before the
/// pipeline is handed back. `Arc` because the runner's PATCH path and the ladder's
/// `on_window` closure both hold it.
///
/// The echo runs on the STREAMING thread from the one-shot CAPS probe, once the
/// renegotiation has landed, so `session_metrics.stream_*` only reports a size the encoder
/// really takes; `None` at the launch size ([`crate::session::echo::Reported`]).
///
/// Holds the metrics WEAKLY: the callback is reached from a probe on the encoder's pad and
/// `SessionMetrics` holds ABR hooks that hold the encoder, so a strong ref closes that
/// cycle (the ~505 MiB/session VRAM leak).
pub(crate) fn resolution_lever_with_echo(
    scale: ScaleStage,
    encoder: gst::Element,
    metrics: &Arc<SessionMetrics>,
) -> Arc<EncodeResolutionLever> {
    let launch = scale.launch;
    let weak = Arc::downgrade(metrics);
    Arc::new(EncodeResolutionLever::new(scale, encoder, move |size| {
        if let Some(metrics) = weak.upgrade() {
            metrics.set_external((size != launch).then_some(size));
            tracing::info!("external resolution: renegotiated to {}x{}", size.0, size.1);
        }
    }))
}

impl ResolutionLever for EncodeResolutionLever {
    fn available_rungs(&self) -> Vec<(i32, i32)> {
        let (w, h) = self.stage.launch;
        crate::session::rungs::available_rungs(w, h)
    }

    fn current(&self) -> (i32, i32) {
        self.stage.current()
    }

    fn set_rung(&self, w: i32, h: i32) -> anyhow::Result<bool> {
        self.step(w, h, trigger::RUNG_STEP)
    }

    /// D7: the frame-rate step. One capsfilter write under the same `step_lock` as
    /// `set_rung`, so the two levers and the runner's PATCH path never interleave.
    ///
    /// No completion probe and no forced IDR, unlike `set_rung`: the frame SIZE does not
    /// change, so the receiver re-learns nothing and an IDR would be a pure σ spike (the
    /// encoder arms its own keyframe on the CAPS event; T9 measured it landing cleanly, 0
    /// warnings, no restart). `ladder_fps` is published from the decision in `on_window`.
    fn set_fps(&self, fps: i32) -> anyhow::Result<bool> {
        let _step = self.step_lock.lock().unwrap();
        self.stage.set_fps(fps)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn init() {
        gst::init().unwrap();
    }

    // ---- amendment 5: which floor is ABSOLUTE ----

    /// A config with a given WIRE floor (the profile's `stream.abr_floor_kbps`, 0 = none)
    /// and HOST floor (`abr_floor_kbps` / `QUASAR_ABR_FLOOR_KBPS`).
    fn cfg_with_floors(wire: u32, host: Option<u32>) -> SessionConfig {
        let mut settings = crate::session::settings::RuntimeSettings::baseline();
        settings.encoder = EncoderChoice::Openh264;
        settings.abr_floor_kbps = host;
        let stream = crate::session::StreamParams {
            width: 1920,
            height: 1080,
            fps: 120,
            bitrate_kbps: 11500,
            h264_profile: "constrained-baseline".to_string(),
            codec: crate::session::Codec::H264,
            abr_floor_kbps: wire,
            mic: false,
        };
        SessionConfig::for_assignment_with(&settings, stream, None)
    }

    #[test]
    fn only_a_host_floor_is_absolute() {
        // The four combinations. Reading the MERGED `cfg.abr_floor_kbps` gets the "both"
        // row wrong either way: it returns the profile's number (which then refuses to
        // scale with the rung), or, guarded on `wire == 0`, no bound at all.

        // wire only ⇒ nothing absolute; the profile's floor is the launch floor and scales.
        let c = cfg_with_floors(4000, None);
        assert_eq!(c.abr_floor_kbps, Some(4000), "merged: the wire value wins");
        assert_eq!(operator_floor(&c), None);

        // host only ⇒ absolute, and it is also the launch floor.
        let c = cfg_with_floors(0, Some(3000));
        assert_eq!(c.abr_floor_kbps, Some(3000));
        assert_eq!(operator_floor(&c), Some(3000));

        // BOTH ⇒ the wire value still wins the LAUNCH floor (AS10-04 precedence), but the
        // host floor is what the ladder may never go below. The common case: every
        // profile-launched session on a host that set `abr_floor_kbps`.
        let c = cfg_with_floors(4000, Some(3000));
        assert_eq!(
            c.abr_floor_kbps,
            Some(4000),
            "AS10-04 precedence is untouched"
        );
        assert_eq!(
            operator_floor(&c),
            Some(3000),
            "a host floor must survive a profile floor"
        );

        // neither ⇒ the ratio-derived floor; nothing absolute.
        let c = cfg_with_floors(0, None);
        assert_eq!(c.abr_floor_kbps, None);
        assert_eq!(operator_floor(&c), None);
    }

    #[test]
    fn the_host_floor_actually_bounds_the_followed_floor() {
        // End to end through `FloorFollow`: run-C (wire floor 4000 kbps) on a host that
        // also set 3000. The deepest rung must stop at 3000, not the unbounded ~1307.
        let c = cfg_with_floors(4000, Some(3000));
        let ff = ladder::FloorFollow {
            enabled: true,
            launch_floor_kbps: c.abr_config().unwrap().floor_kbps,
            operator_floor_kbps: operator_floor(&c),
            ceiling_kbps: 11500,
            launch_px: 1920 * 1080,
            exponent: ladder::ResolutionPolicy::DEFAULT_EXPONENT,
        };
        assert_eq!(ff.floor_for(1920 * 1080, false), 4000, "launch rung");
        assert_eq!(
            ff.floor_for(1280 * 720, true),
            3000,
            "the host floor bounds the deepest rung"
        );
        // …and with no host floor the same session scales all the way down.
        let c = cfg_with_floors(4000, None);
        let ff = ladder::FloorFollow {
            operator_floor_kbps: operator_floor(&c),
            ..ff
        };
        assert!(ff.floor_for(1280 * 720, true) < 1400);
    }

    // ---- ResolutionLever ----

    fn software_lever(launch: (i32, i32)) -> EncodeResolutionLever {
        init();
        let mut settings = crate::session::settings::RuntimeSettings::baseline();
        settings.encoder = EncoderChoice::Openh264;
        let stream = crate::session::StreamParams {
            width: launch.0,
            height: launch.1,
            fps: 60,
            bitrate_kbps: 8000,
            h264_profile: "constrained-baseline".to_string(),
            codec: crate::session::Codec::H264,
            abr_floor_kbps: 0,
            mic: false,
        };
        let cfg = SessionConfig::for_assignment_with(&settings, stream, None);
        let stage = super::super::scale_stage::build_scale_stage(&cfg, None).unwrap();
        let encoder = gst::ElementFactory::make("identity").build().unwrap();
        EncodeResolutionLever::new(stage, encoder, |_| {})
    }

    // The lever's rungs are the session's rung family — the same table the compositor's
    // mode-ladder and the control plane's validator use.
    #[test]
    fn lever_exposes_the_sessions_rungs() {
        let lever = software_lever((1920, 1080));
        assert_eq!(
            lever.available_rungs(),
            vec![(1920, 1080), (1600, 900), (1280, 720)]
        );
        assert_eq!(lever.current(), (1920, 1080));
    }

    // set_rung reports whether anything moved, and refuses an illegal size, never clamps.
    #[test]
    fn lever_set_rung_reports_change_and_refuses_illegal_sizes() {
        let lever = software_lever((1920, 1080));
        assert!(lever.set_rung(1280, 720).unwrap());
        assert!(
            !lever.set_rung(1280, 720).unwrap(),
            "repeat must be a no-op"
        );
        assert_eq!(lever.current(), (1280, 720));
        // Above the launch size: the compositor has no more pixels than this.
        assert!(lever.set_rung(2560, 1440).is_err());
        assert_eq!(lever.current(), (1280, 720));
    }

    // The lever starts AUTO: the ladder owns the size until a human takes it.
    #[test]
    fn lever_starts_auto_and_records_a_pin() {
        let lever = software_lever((1920, 1080));
        assert!(!lever.pinned(), "a fresh session is ladder-owned (auto)");
        lever.set_pinned(true);
        assert!(lever.pinned());
        lever.set_pinned(false);
        assert!(!lever.pinned());
    }

    // Vulkan has no in-graph scaler when `vulkanscale` (#501) is absent, so its lever is
    // inert. A registry that carries `vulkanscale` makes the pair supported and voids the
    // premise, so skip rather than fail (#533), on the shared
    // `scale_stage::vulkanscale_present()` gate.
    #[test]
    fn lever_on_an_unsupported_path_errors() {
        init();
        if super::super::scale_stage::vulkanscale_present() {
            eprintln!(
                "SKIP lever_on_an_unsupported_path_errors: this registry carries vulkanscale (#533)"
            );
            return;
        }
        let mut settings = crate::session::settings::RuntimeSettings::baseline();
        settings.encoder = EncoderChoice::Vulkan;
        let stream = crate::session::StreamParams {
            width: 1920,
            height: 1080,
            fps: 60,
            bitrate_kbps: 8000,
            h264_profile: "constrained-baseline".to_string(),
            codec: crate::session::Codec::H264,
            abr_floor_kbps: 0,
            mic: false,
        };
        let cfg = SessionConfig::for_assignment_with(&settings, stream, None);
        let stage = super::super::scale_stage::build_scale_stage(&cfg, None).unwrap();
        assert!(!stage.supported);
        let lever = EncodeResolutionLever::new(
            stage,
            gst::ElementFactory::make("identity").build().unwrap(),
            |_| {},
        );
        let err = lever.set_rung(1280, 720).unwrap_err().to_string();
        assert!(err.contains("unsupported"), "got {err}");
    }

    // ---- observe_resolution_rung: the rung-4 decide-and-actuate step ----
    //
    // Against a FAKE lever, so resync-to-live, the sticky disable on a refusal, and the
    // one-window in-flight grace are testable without a live encode pipeline. The pure
    // rung arithmetic is covered in `session::ladder`.

    use crate::session::adaptation::AdaptationState;
    use std::sync::atomic::{AtomicUsize, Ordering};

    struct FakeLever {
        rungs: Vec<(i32, i32)>,
        current: Mutex<(i32, i32)>,
        /// The rate `set_fps` last accepted, and whether the lever has one at all.
        fps: Mutex<i32>,
        fps_lever: bool,
        fps_calls: AtomicUsize,
        /// Refuse every `set_rung` (the "this encode path cannot resize" lever).
        refuse: bool,
        /// Don't move `current` on a successful step: the renegotiation has not landed.
        lag: bool,
        calls: AtomicUsize,
    }

    impl FakeLever {
        fn new(rungs: Vec<(i32, i32)>) -> Self {
            let current = Mutex::new(rungs[0]);
            Self {
                rungs,
                current,
                fps: Mutex::new(120),
                fps_lever: true,
                fps_calls: AtomicUsize::new(0),
                refuse: false,
                lag: false,
                calls: AtomicUsize::new(0),
            }
        }
        /// A lever with no rate lever at all (an old image with no `videorate`).
        fn without_fps(rungs: Vec<(i32, i32)>) -> Self {
            Self {
                fps_lever: false,
                ..Self::new(rungs)
            }
        }
        fn fps(&self) -> i32 {
            *self.fps.lock().unwrap()
        }
        fn fps_calls(&self) -> usize {
            self.fps_calls.load(Ordering::Relaxed)
        }
        fn refusing(rungs: Vec<(i32, i32)>) -> Self {
            Self {
                refuse: true,
                ..Self::new(rungs)
            }
        }
        fn calls(&self) -> usize {
            self.calls.load(Ordering::Relaxed)
        }
        /// A manual `session_display_update` landing outside the ladder.
        fn set_externally(&self, size: (i32, i32)) {
            *self.current.lock().unwrap() = size;
        }
    }

    impl ResolutionLever for FakeLever {
        fn available_rungs(&self) -> Vec<(i32, i32)> {
            self.rungs.clone()
        }
        fn current(&self) -> (i32, i32) {
            *self.current.lock().unwrap()
        }
        fn set_rung(&self, w: i32, h: i32) -> anyhow::Result<bool> {
            self.calls.fetch_add(1, Ordering::Relaxed);
            if self.refuse {
                return Err(anyhow::anyhow!("external resize is unsupported"));
            }
            if !self.lag {
                *self.current.lock().unwrap() = (w, h);
            }
            Ok(true)
        }
        fn set_fps(&self, fps: i32) -> anyhow::Result<bool> {
            self.fps_calls.fetch_add(1, Ordering::Relaxed);
            if !self.fps_lever {
                return Err(anyhow::anyhow!("this lever has no fps rung"));
            }
            *self.fps.lock().unwrap() = fps;
            Ok(true)
        }
    }

    const RES_CEILING: u32 = 8000;
    const RES_FLOOR: f64 = 3000.0;
    fn res_rungs() -> Vec<(i32, i32)> {
        vec![(1920, 1080), (1600, 900), (1280, 720)]
    }
    /// A planner-backed state; the defaults (60 fps, fps rung off) make the planner
    /// exactly the resolution rung.
    fn res_state(now: std::time::Instant) -> ResRungState {
        res_state_with(now, false, 60)
    }
    fn res_state_with(now: std::time::Instant, fps_enabled: bool, launch_fps: i32) -> ResRungState {
        let cfg = ladder::LadderConfig {
            resolution_enabled: true,
            fps_enabled,
            order: ladder::LadderOrder::Hybrid,
            ..ladder::LadderConfig::new()
        };
        ResRungState {
            planner: ladder::LadderPlanner::new(&cfg, res_rungs(), launch_fps, RES_CEILING, now),
            res_disabled: false,
            fps_disabled: false,
            in_flight: false,
        }
    }
    /// One congested-at-the-floor window: the emergency path steps on the first qualifying
    /// window, so a test needs no dwell bookkeeping.
    fn emergency_step(
        st: &mut ResRungState,
        lever: &FakeLever,
        now: std::time::Instant,
    ) -> Option<ladder::PlannedStep> {
        observe_resolution_rung(
            st,
            lever,
            false,
            AdaptationState::NetworkCongested,
            3100.0,
            RES_FLOOR,
            now,
        )
    }
    /// The same window projected onto the RESOLUTION lever. The tests using it run an
    /// fps-less planner, so an fps step there is a bug, not an alternative.
    fn emergency_window(
        st: &mut ResRungState,
        lever: &FakeLever,
        now: std::time::Instant,
    ) -> Option<ladder::StepDecision> {
        match emergency_step(st, lever, now) {
            Some(ladder::PlannedStep::Resolution(d)) => Some(d),
            Some(other) => panic!("expected a resolution step, got {other:?}"),
            None => None,
        }
    }

    // A step is decided, actuated, and the machine and lever agree on the new size.
    #[test]
    fn the_rung_steps_and_actuates_on_the_lever() {
        let t = std::time::Instant::now();
        let lever = FakeLever::new(res_rungs());
        let mut st = res_state(t);
        let d = emergency_window(&mut st, &lever, t).expect("congested at the floor must step");
        assert_eq!((d.from, d.to), (0, 1));
        assert_eq!(lever.calls(), 1);
        assert_eq!(lever.current(), (1600, 900));
        assert_eq!(st.planner.res_size(), (1600, 900));
        assert!(
            !st.res_disabled && !st.fps_disabled,
            "a working lever must never retire a rung"
        );
    }

    // A lever that REFUSES retires the rung for the session and is never called again.
    #[test]
    fn a_refusing_lever_disables_the_rung_and_is_never_retried() {
        let t = std::time::Instant::now();
        let lever = FakeLever::refusing(res_rungs());
        let mut st = res_state(t);
        assert_eq!(
            emergency_window(&mut st, &lever, t),
            None,
            "a refused step must not be reported as actuated"
        );
        assert!(
            st.res_disabled,
            "the RESOLUTION rung is the one that refused"
        );
        for i in 1..10u64 {
            assert_eq!(
                emergency_window(&mut st, &lever, t + std::time::Duration::from_secs(30 * i)),
                None
            );
        }
        assert_eq!(lever.calls(), 1, "the lever must be tried exactly once");
    }

    // The size can move underneath the machine (a manual PATCH back to the launch size,
    // which also releases the pin). Without the resync the machine still believes it is at
    // rung 1 and the next window steps to rung 2, shrinking what the user just restored.
    #[test]
    fn an_external_size_change_resyncs_the_machine_before_it_steps_again() {
        let t = std::time::Instant::now();
        let lever = FakeLever::new(res_rungs());
        let mut st = res_state(t);
        // The ladder steps 0 → 1 on its own.
        assert_eq!(emergency_window(&mut st, &lever, t).map(|d| d.to), Some(1));
        // The window right after a step is the in-flight grace; it is also swallowed by
        // the machine's own settle window.
        let t1 = t + std::time::Duration::from_secs(30);
        assert_eq!(emergency_window(&mut st, &lever, t1), None);
        // A human PATCHes back to the launch size (the pin is released by that same
        // request, so the ladder is free to act again).
        lever.set_externally((1920, 1080));
        let t2 = t + std::time::Duration::from_secs(60);
        let d = emergency_window(&mut st, &lever, t2).expect("must step again");
        assert_eq!(
            (d.from, d.to),
            (0, 1),
            "the step must start from the size the user restored, not the stale rung"
        );
        assert_eq!(lever.current(), (1600, 900));
    }

    // The in-flight grace: `set_rung` is asynchronous, so the window right after a step
    // can still read the PREVIOUS size off the lever. That must NOT drag the machine back
    // onto the rung it just left.
    #[test]
    fn a_not_yet_negotiated_step_does_not_resync_the_machine_backwards() {
        let t = std::time::Instant::now();
        let mut lever = FakeLever::new(res_rungs());
        lever.lag = true; // the resize never lands
        let mut st = res_state(t);
        assert_eq!(emergency_window(&mut st, &lever, t).map(|d| d.to), Some(1));
        assert_eq!(lever.current(), (1920, 1080), "the fake never renegotiated");
        // The very next window is the grace window: no resync, so the machine holds rung 1.
        let t1 = t + std::time::Duration::from_secs(30);
        let _ = emergency_window(&mut st, &lever, t1);
        assert_eq!(
            st.planner.res_rung(),
            1,
            "the in-flight grace must hold the index"
        );
    }

    // A human pin freezes the rung: the machine still observes (so a release resumes
    // coherently) but the lever is never touched.
    #[test]
    fn a_human_pin_freezes_the_rung() {
        let t = std::time::Instant::now();
        let lever = FakeLever::new(res_rungs());
        let mut st = res_state(t);
        for i in 0..5u64 {
            assert_eq!(
                observe_resolution_rung(
                    &mut st,
                    &lever,
                    true, // human_pinned
                    AdaptationState::NetworkCongested,
                    3100.0,
                    RES_FLOOR,
                    t + std::time::Duration::from_secs(30 * i),
                ),
                None
            );
        }
        assert_eq!(
            lever.calls(),
            0,
            "a pinned session's lever is never touched"
        );
        assert_eq!(st.planner.res_rung(), 0);
    }

    // ---- D7: an fps step dispatches onto the lever's rate verb ----

    // Hybrid at a 1080p120 launch is already at the pivot, so the first step is the frame
    // rate and it lands on `set_fps`, not `set_rung`.
    #[test]
    fn an_fps_step_actuates_the_rate_lever_not_the_size_lever() {
        let t = std::time::Instant::now();
        let lever = FakeLever::new(res_rungs());
        let mut st = res_state_with(t, true, 120);
        let step = emergency_step(&mut st, &lever, t).expect("congested at the floor must step");
        assert!(
            matches!(step, ladder::PlannedStep::Fps(_)),
            "1080p launch is at the hybrid pivot ⇒ fps goes first, got {step:?}"
        );
        assert_eq!(lever.fps(), 60);
        assert_eq!(lever.fps_calls(), 1);
        assert_eq!(lever.calls(), 0, "the size lever must not be touched");
        assert_eq!(lever.current(), (1920, 1080));
        assert_eq!(st.planner.fps(), 60);
    }

    // The fps-only posture must step the frame rate and never touch the size lever: the
    // seed retires the resolution rung rather than pinning it, so `hybrid` falls through.
    #[test]
    fn an_fps_only_config_steps_fps_and_never_resolution() {
        let t = std::time::Instant::now();
        let lever = FakeLever::new(res_rungs());
        // The shape `enable_abr` seeds for `resolution_enabled = false`.
        let mut st = res_state_with(t, true, 120);
        st.res_disabled = true;

        let mut fps_steps = Vec::new();
        for i in 0..12u64 {
            match emergency_step(&mut st, &lever, t + std::time::Duration::from_secs(30 * i)) {
                Some(ladder::PlannedStep::Fps(d)) => fps_steps.push(d.to),
                Some(other) => panic!("a retired resolution rung must never step: {other:?}"),
                None => {}
            }
        }
        assert_eq!(
            fps_steps,
            vec![60],
            "120 → 60, and the rung is then exhausted"
        );
        assert_eq!(lever.fps(), 60);
        assert_eq!(lever.calls(), 0, "the SIZE lever is never touched");
        assert_eq!(lever.current(), (1920, 1080));
        assert_eq!(st.planner.res_rung(), 0, "the resolution rung never moved");
    }

    // A lever with no rate verb (an image without `videorate`) refuses once, and that must
    // retire the FPS rung ONLY — one shared flag took the working RESOLUTION rung down
    // with it, against `build_scale_stage`'s promise that it is unaffected.
    #[test]
    fn an_fps_refusal_retires_only_the_fps_rung() {
        let t = std::time::Instant::now();
        let lever = FakeLever::without_fps(res_rungs());
        let mut st = res_state_with(t, true, 120);
        // Hybrid at the 1080p pivot picks fps; the lever refuses.
        assert_eq!(emergency_step(&mut st, &lever, t), None);
        assert!(st.fps_disabled, "the fps rung is retired");
        assert!(!st.res_disabled, "the resolution rung must NOT be");

        // …and the resolution rung then steps normally, all the way to its floor.
        let mut seen = Vec::new();
        for i in 1..12u64 {
            if let Some(s) =
                emergency_step(&mut st, &lever, t + std::time::Duration::from_secs(30 * i))
            {
                seen.push(match s {
                    ladder::PlannedStep::Resolution(d) => d.to,
                    other => panic!("the retired fps rung must never be planned again: {other:?}"),
                });
            }
        }
        assert_eq!(seen, vec![1, 2], "the resolution rung keeps working");
        assert_eq!(lever.current(), (1280, 720));
        assert_eq!(
            lever.fps_calls(),
            1,
            "a refusing lever is tried exactly once"
        );
        assert_eq!(lever.calls(), 2, "one call per resolution step");
    }

    // The default trait method refuses rather than silently succeeding.
    #[test]
    fn the_default_fps_lever_refuses() {
        let lever = software_lever((1920, 1080));
        // EncodeResolutionLever overrides the default, but on a path with no
        // capsfilter/videorate it still refuses.
        let _ = lever.set_fps(30);
        struct Bare;
        impl ResolutionLever for Bare {
            fn available_rungs(&self) -> Vec<(i32, i32)> {
                vec![]
            }
            fn current(&self) -> (i32, i32) {
                (0, 0)
            }
            fn set_rung(&self, _w: i32, _h: i32) -> anyhow::Result<bool> {
                Ok(false)
            }
        }
        assert!(Bare.set_fps(60).is_err(), "the default must refuse");
    }

    // ---- encoder_saturation_fresh (#328 freshness derivation) ----
    //
    // Rolling state is `(was-saturated-last-window, EWMA baseline p95 ms)`.

    #[test]
    fn fresh_true_on_first_saturation_after_healthy() {
        let (fresh, new_state) = encoder_saturation_fresh(true, Some(20.0), (false, None));
        assert!(fresh, "transition into saturation is always fresh");
        // Baseline seeds from the first p95 sample.
        assert_eq!(new_state, (true, Some(20.0)));
    }

    // A transition is fresh even with no p95 sample (rising can't fire; transition does).
    #[test]
    fn fresh_true_on_transition_even_without_p95() {
        let (fresh, new_state) = encoder_saturation_fresh(true, None, (false, None));
        assert!(fresh, "transition alone (no p95) is still fresh");
        assert_eq!(
            new_state,
            (true, None),
            "None p95 leaves the baseline unchanged"
        );
    }

    // Steady-state saturation with a settled p95 is NOT fresh (#328): no transition, and
    // p95 == baseline so nothing is rising.
    #[test]
    fn fresh_false_on_steady_state_saturation() {
        let (fresh, new_state) = encoder_saturation_fresh(true, Some(20.0), (true, Some(20.0)));
        assert!(
            !fresh,
            "settled steady-state saturation is NOT fresh (#328)"
        );
        // EWMA: 0.3*20 + 0.7*20 = 20.0 (unchanged when p95 == baseline).
        assert_eq!(new_state, (true, Some(20.0)));
    }

    // A p95 within the 10% margin is jitter, not a climb: still not fresh.
    #[test]
    fn fresh_false_when_p95_rise_within_10pct_margin() {
        // 21.0 > 20.0*1.10 (= 22.0)? no.
        let (fresh, new_state) = encoder_saturation_fresh(true, Some(21.0), (true, Some(20.0)));
        assert!(!fresh, "a sub-10% p95 wobble is not a fresh degradation");
        // EWMA advances: 0.3*21 + 0.7*20 = 20.3.
        assert!((new_state.1.unwrap() - 20.3).abs() < 1e-9);
    }

    // `p95 > baseline*1.10` re-fires fresh mid-saturation, with no transition.
    #[test]
    fn fresh_true_when_p95_rises_past_10pct_margin() {
        let (fresh, new_state) = encoder_saturation_fresh(true, Some(25.0), (true, Some(20.0)));
        assert!(fresh, "a >10% p95 climb re-fires fresh mid-saturation");
        // 0.3*25 + 0.7*20 = 21.5.
        assert!((new_state.1.unwrap() - 21.5).abs() < 1e-9);
    }

    // The classifier label, not p95 alone, is the trigger: a non-saturated window is never
    // fresh however fast p95 rises.
    #[test]
    fn fresh_false_when_not_saturated_even_if_p95_rising() {
        let (fresh, new_state) = encoder_saturation_fresh(false, Some(99.0), (true, Some(20.0)));
        assert!(!fresh, "a non-saturated window is never fresh");
        // prev_saturated records false; the baseline still advances on the sample.
        assert!(!new_state.0);
        assert!((new_state.1.unwrap() - (0.3 * 99.0 + 0.7 * 20.0)).abs() < 1e-9);
    }

    // A Healthy window clears the saturated flag but CARRIES the EWMA baseline (advancing
    // it from its p95), so the next saturation still fires fresh via the transition branch.
    #[test]
    fn healthy_carries_baseline_but_clears_saturated_flag() {
        let (fresh1, state1) = encoder_saturation_fresh(false, Some(10.0), (true, Some(20.0)));
        assert!(!fresh1);
        assert!(
            !state1.0,
            "saturated flag cleared by a non-saturated window"
        );
        // Carried and advanced, not reset to None: 0.3*10 + 0.7*20 = 17.0.
        assert!((state1.1.unwrap() - 17.0).abs() < 1e-9);
        let (fresh2, _state2) = encoder_saturation_fresh(true, Some(17.0), state1);
        assert!(
            fresh2,
            "saturation after a healthy gap is a fresh transition"
        );
    }

    // None p95 samples never advance or clear a settled baseline.
    #[test]
    fn none_p95_leaves_baseline_intact() {
        let (_f, state) = encoder_saturation_fresh(true, None, (true, Some(18.5)));
        assert_eq!(state, (true, Some(18.5)), "None p95 → baseline unchanged");
    }

    // ---- write_encoder_bitrate: per-encoder unit mapping ----
    //
    // Only openh264enc exists in the dev image (no GPU at test time), so only the software
    // unit mapping runs against a real element; the VA/NVENC kbit/s mapping is pinned by
    // the writability-gate tests below.

    // openh264enc takes bit/s: a kbps retarget is written as kbps*1000.
    #[test]
    fn write_bitrate_openh264_maps_kbps_to_bits_per_second() {
        init();
        let enc = match gst::ElementFactory::make("openh264enc").build() {
            Ok(e) => e,
            Err(_) => return, // openh264 unavailable: skip
        };
        write_encoder_bitrate(&enc, EncoderChoice::Openh264, 6000, 60);
        let got: u32 = enc.property("bitrate");
        assert_eq!(
            got, 6_000_000,
            "openh264 bitrate is bit/s (6000 kbps → 6_000_000)"
        );
    }

    // A retarget never touches the rate-control mode: CBR stays CBR, and writing bitrate
    // must not disturb a property outside the write set.
    #[test]
    fn write_bitrate_openh264_only_writes_bitrate() {
        init();
        let enc = match gst::ElementFactory::make("openh264enc")
            .property_from_str("rate-control", "bitrate")
            .build()
        {
            Ok(e) => e,
            Err(_) => return,
        };
        write_encoder_bitrate(&enc, EncoderChoice::Openh264, 3000, 30);
        assert_eq!(enc.property::<u32>("bitrate"), 3_000_000);
    }

    // ---- encoder_abr_writable / write guards: missing property disarms ----

    // openh264enc HAS `bitrate` → the software writability gate passes.
    #[test]
    fn writable_openh264_true_when_bitrate_present() {
        init();
        let Ok(enc) = gst::ElementFactory::make("openh264enc").build() else {
            return;
        };
        assert!(
            encoder_abr_writable(&enc, EncoderChoice::Openh264),
            "openh264 has a writable `bitrate` → software ABR arms"
        );
    }

    // The VA gate requires `cpb-size`, which openh264enc lacks, so treating it as VA
    // disarms — AS-00: no CPB retune, no retarget.
    #[test]
    fn writable_va_gate_disarms_when_cpb_property_missing() {
        init();
        let Ok(enc) = gst::ElementFactory::make("openh264enc").build() else {
            return;
        };
        assert!(
            !encoder_abr_writable(&enc, EncoderChoice::Va),
            "no `cpb-size` on this element → VA writability gate disarms"
        );
    }

    // Likewise the NVENC gate requires `vbv-buffer-size`, absent on openh264 → disarm.
    #[test]
    fn writable_nvenc_gate_disarms_when_vbv_property_missing() {
        init();
        let Ok(enc) = gst::ElementFactory::make("openh264enc").build() else {
            return;
        };
        assert!(
            !encoder_abr_writable(&enc, EncoderChoice::Nvenc),
            "no `vbv-buffer-size` on this element → NVENC writability gate disarms"
        );
    }

    // Every write is writability-guarded, so writing VA/NVENC properties onto an element
    // that lacks them must be a silent no-op and never panic.
    #[test]
    fn write_bitrate_is_a_noop_for_missing_hardware_properties() {
        init();
        let Ok(enc) = gst::ElementFactory::make("openh264enc").build() else {
            return;
        };
        write_encoder_bitrate(&enc, EncoderChoice::Va, 5000, 60);
        write_encoder_bitrate(&enc, EncoderChoice::Nvenc, 5000, 60);
    }

    // ---- encoder_speed_bias_writable / write_encoder_speed_bias ----

    // openh264 has no live speed lever, so the rung is inert.
    #[test]
    fn speed_bias_writable_false_for_openh264() {
        init();
        let Ok(enc) = gst::ElementFactory::make("openh264enc").build() else {
            return;
        };
        assert!(!encoder_speed_bias_writable(&enc, EncoderChoice::Openh264));
        // And the VA/NVENC gates disarm too (openh264 lacks target-usage / preset).
        assert!(!encoder_speed_bias_writable(&enc, EncoderChoice::Va));
        assert!(!encoder_speed_bias_writable(&enc, EncoderChoice::Nvenc));
    }

    // write_encoder_speed_bias is a no-op for openh264 and panic-safe on missing props.
    #[test]
    fn write_speed_bias_is_panic_safe_on_missing_props() {
        init();
        let Ok(enc) = gst::ElementFactory::make("openh264enc").build() else {
            return;
        };
        write_encoder_speed_bias(&enc, EncoderChoice::Openh264, 2, 4); // inert
        write_encoder_speed_bias(&enc, EncoderChoice::Va, 2, 4); // target-usage absent → guarded
        write_encoder_speed_bias(&enc, EncoderChoice::Nvenc, 2, 4); // preset absent → guarded
    }
}
