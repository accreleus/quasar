//! Input injection — translate DataChannel input messages into compositor input.
//!
//! The browser sends input over the `"input"` DataChannel as JSON, one object per
//! message, discriminated by `t` (protocol/input.md, a frozen interface).
//!
//! ## Delivery: real evdev devices
//! The live path writes each event to the per-session [`VirtualDevices`] (uinput
//! keyboard/mouse/gamepad) via an [`InputSink`], not to the compositor as a
//! `CustomUpstream` event: one evdev pipeline feeds both the compositor (via
//! libinput) and a containerized game opening `/dev/input/event*` directly, and it
//! is the only way a gamepad reaches the app — Wayland has no joypad protocol and
//! the compositor drops pads.
//!
//! The `CustomUpstream` structure builders ([`InputMsg::to_structure`]) and the
//! compositor self-test ([`run_inject_selftest`]) remain for the browserless
//! compositor-path check (names/fields authoritative from the element's imp.rs)
//! but are no longer on the live input path.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use anyhow::Result;
use gstreamer as gst;
use gstreamer::prelude::*;
use serde::Deserialize;

use super::virtual_input::VirtualDevices;

/// One client→host input message, per protocol/input.md (`t`-discriminated).
/// Numeric fields are `f64` so integer and fractional JSON both parse.
///
/// `seq`/`tc` on `mm` are optional instrumentation fields a browser may attach to
/// correlate timing; ignored unless `QUASAR_INPUT_TRACE=1`. `#[serde(default)]`
/// keeps the wire shape backwards-compatible when absent.
#[derive(Debug, Clone, Deserialize, PartialEq)]
#[serde(tag = "t")]
pub enum InputMsg {
    /// Relative mouse motion, device pixels at the streamed resolution.
    #[serde(rename = "mm")]
    MouseMoveRel {
        dx: f64,
        dy: f64,
        #[serde(default)]
        seq: Option<u32>,
        #[serde(default)]
        tc: Option<f64>,
    },
    /// Absolute mouse motion, normalized 0.0..1.0 across the streamed surface.
    #[serde(rename = "ma")]
    MouseMoveAbs { x: f64, y: f64 },
    /// Mouse button (Linux input code: left 0x110, right 0x111, middle 0x112).
    #[serde(rename = "mb")]
    MouseButton { button: u32, pressed: bool },
    /// Scroll, high-resolution wheel units.
    #[serde(rename = "ms")]
    Scroll { dx: f64, dy: f64 },
    /// Key, Linux evdev keycode.
    #[serde(rename = "k")]
    Key { code: u32, pressed: bool },
    /// Gamepad state-on-change (W3C Standard Gamepad).
    #[serde(rename = "gp")]
    Gamepad {
        #[serde(default)]
        i: u32,
        #[serde(default)]
        buttons: Vec<f64>,
        #[serde(default)]
        axes: Vec<f64>,
    },
}

impl InputMsg {
    pub fn parse(json: &str) -> serde_json::Result<Self> {
        serde_json::from_str(json)
    }

    /// Short tag for logging.
    pub fn kind(&self) -> &'static str {
        match self {
            InputMsg::MouseMoveRel { .. } => "mouse-move-rel",
            InputMsg::MouseMoveAbs { .. } => "mouse-move-abs",
            InputMsg::MouseButton { .. } => "mouse-button",
            InputMsg::Scroll { .. } => "scroll",
            InputMsg::Key { .. } => "key",
            InputMsg::Gamepad { .. } => "gamepad",
        }
    }

    /// Build the `CustomUpstream` `GstStructure` for this message, given the
    /// streamed surface size (used to denormalize absolute motion). Returns
    /// `None` for messages with no compositor injection (gamepad).
    pub fn to_structure(&self, width: i32, height: i32) -> Option<gst::Structure> {
        let s = match self {
            InputMsg::MouseMoveRel { dx, dy, .. } => gst::Structure::builder("MouseMoveRelative")
                .field("pointer_x", *dx)
                .field("pointer_y", *dy)
                .build(),
            InputMsg::MouseMoveAbs { x, y } => gst::Structure::builder("MouseMoveAbsolute")
                .field("pointer_x", x * width as f64)
                .field("pointer_y", y * height as f64)
                .build(),
            InputMsg::MouseButton { button, pressed } => gst::Structure::builder("MouseButton")
                .field("button", *button)
                .field("pressed", *pressed)
                .build(),
            InputMsg::Scroll { dx, dy } => gst::Structure::builder("MouseAxis")
                .field("x", *dx)
                .field("y", *dy)
                .build(),
            InputMsg::Key { code, pressed } => gst::Structure::builder("KeyboardKey")
                .field("key", *code)
                .field("pressed", *pressed)
                .build(),
            InputMsg::Gamepad { .. } => return None,
        };
        Some(s)
    }
}

/// Where a session's DataChannel input is delivered.
#[derive(Clone)]
pub enum InputSink {
    /// Real evdev virtual devices: keyboard/mouse also reach the compositor via
    /// libinput, the gamepad reaches the container directly.
    Virtual(Arc<VirtualDevices>),
    /// `--test-src`: videotestsrc accepts no input; messages are parsed and logged.
    None,
}

/// Per-session input state for the controller-first pointer nudge (BPM focus
/// heal). One instance per session's input DataChannel, reset on app swap (the
/// encode pipeline outlives a swap but the DataChannel/session does not). See
/// `docs/design/plans/2026-07-20-bpm-controller-focus-spec.md`.
///
/// Steam Big Picture under nested gamescope suppresses gamepad focus promotion
/// until the cursor enters its window via the real input path (uinput mouse ->
/// compositor -> gamescope). A ±1px net-zero pointer jiggle heals it. Fires
/// exactly once, on the first gamepad event of a session that has seen no real
/// mouse motion, and never when the mouse is already in use.
#[derive(Default)]
pub struct InputState {
    /// A real client mouse-motion event has been seen; when set, the nudge must
    /// not fire.
    mouse_seen: AtomicBool,
    /// The one-shot nudge has fired (or been claimed).
    nudge_sent: AtomicBool,
}

impl InputState {
    pub fn new() -> Self {
        Self::default()
    }

    /// Re-arm the one-shot on launcher<->game swap completion: the DataChannel and
    /// this `InputState` persist across a swap (only the app container is
    /// replaced), so without this reset a fresh BPM container would inherit a
    /// spent `nudge_sent` or a stale `mouse_seen` from launcher use and never heal.
    pub fn reset(&self) {
        self.mouse_seen.store(false, Ordering::Relaxed);
        self.nudge_sent.store(false, Ordering::Relaxed);
    }
}

/// Whether the controller-first pointer nudge is enabled (`QUASAR_INPUT_CONTROLLER_NUDGE`,
/// default ON). Probed once, then a cached `Relaxed` atomic load.
fn controller_nudge_enabled() -> bool {
    static FLAG: AtomicBool = AtomicBool::new(true);
    static PROBED: AtomicBool = AtomicBool::new(false);
    if !PROBED.swap(true, Ordering::Relaxed) {
        let disabled = std::env::var("QUASAR_INPUT_CONTROLLER_NUDGE")
            .ok()
            .is_some_and(|v| v == "0" || v.eq_ignore_ascii_case("false"));
        FLAG.store(!disabled, Ordering::Relaxed);
    }
    FLAG.load(Ordering::Relaxed)
}

/// Decide whether this message should trigger the one-shot controller nudge,
/// updating `state` as a side effect. Pure (no device I/O), so it's unit-testable
/// without real uinput devices. Returns `true` exactly once per session: the
/// first gamepad event with no prior mouse motion while enabled.
fn should_nudge(state: &InputState, enabled: bool, msg: &InputMsg) -> bool {
    match msg {
        InputMsg::MouseMoveRel { .. } | InputMsg::MouseMoveAbs { .. } => {
            state.mouse_seen.store(true, Ordering::Relaxed);
            false
        }
        InputMsg::Gamepad { .. } => {
            if !enabled
                || state.nudge_sent.load(Ordering::Relaxed)
                || state.mouse_seen.load(Ordering::Relaxed)
            {
                return false;
            }
            // Claim the one-shot; only the thread that flips false→true fires.
            !state.nudge_sent.swap(true, Ordering::Relaxed)
        }
        _ => false,
    }
}

/// One-time notice that input is parse-only on test-src (avoids log spam at frame rate).
static TESTSRC_NOTICE: AtomicBool = AtomicBool::new(false);

/// Parse one DataChannel message and inject it into `sink`. `state` carries the
/// per-session controller-first-nudge bookkeeping.
pub fn handle(json: &str, sink: &InputSink, state: &InputState, width: i32, height: i32) {
    let msg = match InputMsg::parse(json) {
        Ok(m) => m,
        Err(e) => {
            tracing::debug!("ignoring non-input message: {e} (raw={json})");
            return;
        }
    };

    // Opt-in per-`mm` timing trace: client `seq`/`tc` alongside host receive+inject
    // instants, for correlating cadence and sequence gaps. Off by default (would
    // flood logs at input rate).
    let trace = input_trace_enabled();
    let recv_ts = if trace {
        Some(host_monotonic_ms())
    } else {
        None
    };

    match sink {
        InputSink::Virtual(devices) => {
            if !trace {
                tracing::debug!("input rx: {} (json={json})", msg.kind());
            }
            // BPM focus heal (see InputState docs).
            if should_nudge(state, controller_nudge_enabled(), &msg) {
                match devices.pointer_nudge() {
                    Ok(()) => tracing::info!(
                        "controller-first session: injected pointer nudge (BPM focus heal)"
                    ),
                    Err(e) => tracing::warn!(
                        token = "input-pointer-nudge-failed",
                        "controller-first pointer nudge failed: {e:#}"
                    ),
                }
            }
            inject_virtual(devices, &msg, width, height);
            if trace {
                emit_mm_trace(&msg, recv_ts, host_monotonic_ms());
            }
        }
        InputSink::None => {
            if !TESTSRC_NOTICE.swap(true, Ordering::Relaxed) {
                tracing::info!("input received but not injected (test-src has no input target)");
            }
        }
    }
}

/// Whether per-`mm` input timing tracing is enabled (`QUASAR_INPUT_TRACE=1`).
/// Probed lazily on first call, then a cached `Relaxed` load.
fn input_trace_enabled() -> bool {
    static FLAG: AtomicBool = AtomicBool::new(false);
    static PROBED: AtomicBool = AtomicBool::new(false);
    if !PROBED.swap(true, Ordering::Relaxed) {
        let on = matches!(
            std::env::var("QUASAR_INPUT_TRACE").ok().as_deref(),
            Some("1") | Some("true") | Some("TRUE")
        );
        FLAG.store(on, Ordering::Relaxed);
    }
    FLAG.load(Ordering::Relaxed)
}

/// Host monotonic time in ms, for correlation with the client `tc`.
fn host_monotonic_ms() -> f64 {
    use std::sync::OnceLock;
    use std::time::Instant;
    static EPOCH: OnceLock<Instant> = OnceLock::new();
    let epoch = *EPOCH.get_or_init(Instant::now);
    epoch.elapsed().as_secs_f64() * 1000.0
}

/// Emit one `tracing::debug!` line for an `mm` message with send/receive/inject
/// timing. No-op (and returns without logging) for non-`mm` messages.
fn emit_mm_trace(msg: &InputMsg, recv_ts: Option<f64>, inject_ts: f64) {
    if let InputMsg::MouseMoveRel { dx, dy, seq, tc } = msg {
        let recv = recv_ts.unwrap_or(inject_ts);
        tracing::debug!(
            target: "quasar.input.trace",
            seq = seq.unwrap_or(0),
            tc = tc.unwrap_or(0.0),
            recv_ms = recv,
            inject_ms = inject_ts,
            dwell_ms = inject_ts - recv,
            dx = *dx,
            dy = *dy,
            "mm"
        );
    }
}

/// Translate one parsed message into evdev events on the virtual devices.
/// Absolute motion is denormalized from 0.0..1.0 to output pixels here (the
/// device works in pixels).
fn inject_virtual(devices: &VirtualDevices, msg: &InputMsg, width: i32, height: i32) {
    // Drain pending relative motion first so a click lands where the user saw
    // the cursor. No-op when batching is disabled or nothing is pending.
    if !matches!(msg, InputMsg::MouseMoveRel { .. }) {
        devices.flush_pending_rel();
    }
    let result = match msg {
        InputMsg::MouseMoveRel { dx, dy, .. } => devices.mouse_move_rel(*dx, *dy),
        InputMsg::MouseMoveAbs { x, y } => {
            devices.mouse_move_abs(x * width as f64, y * height as f64)
        }
        InputMsg::MouseButton { button, pressed } => devices.mouse_button(*button, *pressed),
        InputMsg::Scroll { dx, dy } => devices.scroll(*dx, *dy),
        InputMsg::Key { code, pressed } => devices.key(*code, *pressed),
        InputMsg::Gamepad { buttons, axes, .. } => devices.gamepad(buttons, axes),
    };
    if let Err(e) = result {
        tracing::warn!(
            token = "input-inject-failed",
            "failed to inject {} into virtual device: {e:#}",
            msg.kind()
        );
    }
}

/// Server-side self-test: build a real `waylanddisplaysrc` pipeline, send one of
/// each mouse/keyboard event, and confirm the compositor accepts them
/// (`send_event` true). Exercises the full input path without a browser/GPU.
pub fn run_inject_selftest() -> Result<()> {
    use anyhow::Context;

    let width = 320;
    let height = 240;

    let pipeline = gst::Pipeline::new();
    let src = gst::ElementFactory::make("waylanddisplaysrc")
        .property("render-node", "software")
        .build()
        .context("waylanddisplaysrc not found — check GST_PLUGIN_PATH")?;
    // System-memory RGBx avoids a DMABuf negotiation; fakesink just drops frames.
    let caps = gst::Caps::builder("video/x-raw")
        .field("format", "RGBx")
        .field("width", width)
        .field("height", height)
        .build();
    let capsfilter = gst::ElementFactory::make("capsfilter")
        .property("caps", &caps)
        .build()
        .context("capsfilter not found")?;
    let sink = gst::ElementFactory::make("fakesink")
        .property("sync", false)
        .build()
        .context("fakesink not found")?;

    pipeline.add_many([&src, &capsfilter, &sink])?;
    gst::Element::link_many([&src, &capsfilter, &sink])
        .context("failed to link self-test pipeline")?;

    pipeline
        .set_state(gst::State::Playing)
        .context("self-test pipeline failed to go to PLAYING")?;
    // Settle so the element's command channel exists; NoPreroll counts as success.
    let (res, _cur, _pending) = pipeline.state(gst::ClockTime::from_seconds(10));
    res.context("self-test pipeline did not reach PLAYING")?;

    let samples = [
        InputMsg::MouseMoveRel {
            dx: 5.0,
            dy: -3.0,
            seq: None,
            tc: None,
        },
        InputMsg::MouseMoveAbs { x: 0.5, y: 0.5 },
        InputMsg::MouseButton {
            button: 272,
            pressed: true,
        },
        InputMsg::MouseButton {
            button: 272,
            pressed: false,
        },
        InputMsg::Scroll { dx: 0.0, dy: 120.0 },
        InputMsg::Key {
            code: 30,
            pressed: true,
        },
        InputMsg::Key {
            code: 30,
            pressed: false,
        },
    ];

    let mut all_ok = true;
    for m in &samples {
        let Some(structure) = m.to_structure(width, height) else {
            continue;
        };
        let name = structure.name().to_string();
        let event = gst::event::CustomUpstream::new(structure);
        let ok = src.send_event(event);
        tracing::info!("inject {name:<18} -> send_event={ok}");
        all_ok &= ok;
    }

    pipeline.set_state(gst::State::Null).ok();

    if all_ok {
        tracing::info!(
            "✅ input inject self-test PASS: compositor accepted all mouse/keyboard events"
        );
        Ok(())
    } else {
        anyhow::bail!("input inject self-test FAIL: compositor rejected at least one input event")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn init() {
        let _ = gst::init();
    }

    #[test]
    fn parses_protocol_messages() {
        assert_eq!(
            InputMsg::parse(r#"{"t":"mm","dx":4,"dy":-2}"#).unwrap(),
            InputMsg::MouseMoveRel {
                dx: 4.0,
                dy: -2.0,
                seq: None,
                tc: None
            }
        );
        assert_eq!(
            InputMsg::parse(r#"{"t":"mm","dx":4,"dy":-2,"seq":17,"tc":123.4}"#).unwrap(),
            InputMsg::MouseMoveRel {
                dx: 4.0,
                dy: -2.0,
                seq: Some(17),
                tc: Some(123.4)
            }
        );
        assert_eq!(
            InputMsg::parse(r#"{"t":"ma","x":0.51,"y":0.42}"#).unwrap(),
            InputMsg::MouseMoveAbs { x: 0.51, y: 0.42 }
        );
        assert_eq!(
            InputMsg::parse(r#"{"t":"mb","button":272,"pressed":true}"#).unwrap(),
            InputMsg::MouseButton {
                button: 272,
                pressed: true
            }
        );
        assert_eq!(
            InputMsg::parse(r#"{"t":"ms","dx":0,"dy":120}"#).unwrap(),
            InputMsg::Scroll { dx: 0.0, dy: 120.0 }
        );
        assert_eq!(
            InputMsg::parse(r#"{"t":"k","code":30,"pressed":true}"#).unwrap(),
            InputMsg::Key {
                code: 30,
                pressed: true
            }
        );
        assert!(matches!(
            InputMsg::parse(r#"{"t":"gp","i":0,"buttons":[0,1],"axes":[0.0,-0.3]}"#).unwrap(),
            InputMsg::Gamepad { .. }
        ));
    }

    #[test]
    fn relative_motion_maps_to_mousemoverelative() {
        init();
        let s = InputMsg::MouseMoveRel {
            dx: 7.0,
            dy: -4.0,
            seq: None,
            tc: None,
        }
        .to_structure(320, 240)
        .unwrap();
        assert_eq!(s.name(), "MouseMoveRelative");
        assert_eq!(s.get::<f64>("pointer_x").unwrap(), 7.0);
        assert_eq!(s.get::<f64>("pointer_y").unwrap(), -4.0);
    }

    #[test]
    fn absolute_motion_denormalizes_to_pixels() {
        init();
        let s = InputMsg::MouseMoveAbs { x: 0.5, y: 0.25 }
            .to_structure(320, 240)
            .unwrap();
        assert_eq!(s.name(), "MouseMoveAbsolute");
        assert_eq!(s.get::<f64>("pointer_x").unwrap(), 160.0);
        assert_eq!(s.get::<f64>("pointer_y").unwrap(), 60.0);
    }

    #[test]
    fn button_and_key_carry_codes_and_state() {
        init();
        let b = InputMsg::MouseButton {
            button: 272,
            pressed: true,
        }
        .to_structure(320, 240)
        .unwrap();
        assert_eq!(b.name(), "MouseButton");
        assert_eq!(b.get::<u32>("button").unwrap(), 272);
        assert!(b.get::<bool>("pressed").unwrap());

        let k = InputMsg::Key {
            code: 30,
            pressed: false,
        }
        .to_structure(320, 240)
        .unwrap();
        assert_eq!(k.name(), "KeyboardKey");
        assert_eq!(k.get::<u32>("key").unwrap(), 30);
        assert!(!k.get::<bool>("pressed").unwrap());
    }

    #[test]
    fn scroll_maps_to_mouseaxis() {
        init();
        let s = InputMsg::Scroll { dx: 0.0, dy: 120.0 }
            .to_structure(320, 240)
            .unwrap();
        assert_eq!(s.name(), "MouseAxis");
        assert_eq!(s.get::<f64>("x").unwrap(), 0.0);
        assert_eq!(s.get::<f64>("y").unwrap(), 120.0);
    }

    fn gp() -> InputMsg {
        InputMsg::Gamepad {
            i: 0,
            buttons: vec![0.0],
            axes: vec![0.0, 0.0],
        }
    }

    fn mm() -> InputMsg {
        InputMsg::MouseMoveRel {
            dx: 1.0,
            dy: 0.0,
            seq: None,
            tc: None,
        }
    }

    #[test]
    fn nudge_fires_once_on_first_gamepad_when_no_mouse() {
        let state = InputState::new();
        assert!(should_nudge(&state, true, &gp()));
        assert!(!should_nudge(&state, true, &gp()));
        assert!(!should_nudge(&state, true, &gp()));
    }

    #[test]
    fn nudge_never_fires_when_mouse_seen_first() {
        let state = InputState::new();
        assert!(!should_nudge(&state, true, &mm()));
        assert!(!should_nudge(&state, true, &gp()));
    }

    #[test]
    fn nudge_never_fires_when_disabled() {
        let state = InputState::new();
        assert!(!should_nudge(&state, false, &gp()));
        // The disabled call left state untouched, so enabling now still fires.
        assert!(should_nudge(&state, true, &gp()));
    }

    #[test]
    fn reset_rearms_the_nudge_after_a_swap() {
        let state = InputState::new();
        assert!(!should_nudge(&state, true, &mm()));
        assert!(!should_nudge(&state, true, &gp()));
        state.reset();
        assert!(should_nudge(&state, true, &gp()));
        assert!(!should_nudge(&state, true, &gp()));
    }

    #[test]
    fn absolute_motion_also_counts_as_mouse_seen() {
        let state = InputState::new();
        assert!(!should_nudge(
            &state,
            true,
            &InputMsg::MouseMoveAbs { x: 0.5, y: 0.5 }
        ));
        assert!(!should_nudge(&state, true, &gp()));
    }

    #[test]
    fn non_motion_events_do_not_arm_or_fire() {
        let state = InputState::new();
        assert!(!should_nudge(
            &state,
            true,
            &InputMsg::Key {
                code: 30,
                pressed: true
            }
        ));
        assert!(should_nudge(&state, true, &gp()));
    }

    #[test]
    fn gamepad_produces_no_event() {
        init();
        let none = InputMsg::Gamepad {
            i: 0,
            buttons: vec![0.0, 1.0],
            axes: vec![0.0, -0.3],
        }
        .to_structure(320, 240);
        assert!(none.is_none());
    }
}
