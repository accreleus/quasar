//! The live display / external-size / ladder **echo** — "what is the client
//! actually getting right now" — and the absent-when-default rule that governs
//! how it reaches the wire.
//!
//! Split out of `session::metrics` (observability review C10). Shares nothing with
//! the encode telemetry beside it (no counter, sample ring, classifier input, or
//! lock) — only a publication moment: the heartbeat drain names both in one
//! `session_metrics` message.
//!
//! Two types: [`LiveEcho`] is the mutable state, written by the runner and the ABR
//! ladder as each fact becomes true, read once per drain. [`Reported`] states the
//! absent-when-default rule once (see its doc).

use std::sync::Mutex;

/// A value that reaches the wire **only when it is off its default**, and is
/// omitted otherwise — the convention every optional echo key on `session_metrics`
/// obeys, stated here once:
///
/// > **Absent means "at the default", never "unknown" and never "zero".**
///
/// No `stream_width` ⇒ launch size; no `ladder_res_rung` ⇒ rung 0; no `ui_scale` ⇒
/// 1.0. The default is carried by the message's *shape*, not a value in it, which
/// keeps a steady-state window small and makes a present key mean something.
///
/// Two exceptions: a key whose absence would mean "not measured" is a plain
/// `Option`, not a `Reported` (the encode-latency/host-probe summaries at the
/// drain) — same wire shape, different sentence. `external_resize_supported` is a
/// `Reported` that in practice is always present (the runner sets it before the
/// first window drains); its absence means an agent too old to report the
/// capability, i.e. *unknown* — spelled out in its OpenAPI description.
///
/// Newtype over `Option<T>`: constructors decide presence, [`Reported::reported`]
/// hands back the `Option`, and `serde`'s `skip_serializing_if = "Option::is_none"`
/// on `AgentMsg::SessionMetrics` does the omitting.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Reported<T>(Option<T>);

impl<T> Reported<T> {
    /// Present when `opt` is `Some` — for a fact that is already `Option`-shaped, so
    /// `None` **is** its default (the render size, the external size, a capability
    /// nobody has reported yet).
    pub fn maybe(opt: Option<T>) -> Self {
        Self(opt)
    }

    /// The `Option` the wire struct carries. `None` ⇒ the key is omitted.
    pub fn reported(self) -> Option<T> {
        self.0
    }
}

impl<T: PartialEq> Reported<T> {
    /// Present unless `value` equals `default`.
    pub fn unless(value: T, default: T) -> Self {
        Self((value != default).then_some(value))
    }
}

impl Reported<f64> {
    /// Present unless `value` is `default` — the float spelling of [`Reported::unless`],
    /// so a scale that arrives as `1.0000000000000002` still reads as the default
    /// rather than echoing forever.
    pub fn unless_near(value: f64, default: f64) -> Self {
        ((value - default).abs() > f64::EPSILON)
            .then_some(value)
            .into()
    }
}

impl<T> From<Option<T>> for Reported<T> {
    fn from(opt: Option<T>) -> Self {
        Self(opt)
    }
}

/// One drain's read of the echo, already reduced to wire-shaped fields.
///
/// Every field is a [`Reported`], so the absent-when-default decision is made here,
/// once per key, and the drain does nothing but call [`Reported::reported`].
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct EchoSnapshot {
    pub render_width: Reported<i32>,
    pub render_height: Reported<i32>,
    pub ui_scale: Reported<f64>,
    pub stream_width: Reported<i32>,
    pub stream_height: Reported<i32>,
    pub external_resize_supported: Reported<bool>,
    pub ladder_speed_bias: Reported<i32>,
    pub ladder_res_rung: Reported<i32>,
    pub ladder_fps: Reported<i32>,
    pub external_owner: Reported<&'static str>,
}

/// The echo's mutable state. One struct behind one mutex so a drain can never mix a
/// new width with an old height — separate `Relaxed` atomics would let a drain
/// straddle a [`LiveEcho::set_display`] and report half of each.
#[derive(Debug, Clone, Copy, PartialEq)]
struct EchoState {
    /// The compositor's app-facing `wl_output` mode; `None` ⇒ the session default.
    render: Option<(i32, i32)>,
    /// The compositor's `wp_fractional_scale_v1` preferred scale.
    ui_scale: f64,
    /// The negotiated EXTERNAL (encoded) size; `None` ⇒ the launch size.
    stream: Option<(i32, i32)>,
    /// Whether this session's encode path has a live-resize lever at all. `None` only
    /// before the runner has reported it.
    external_resize_supported: Option<bool>,
    /// SPT-08: the ladder's current encoder speed-bias rung (0 = baseline).
    ladder_bias: u8,
    /// SPT-08: the ladder's current resolution rung index (0 = launch).
    ladder_res_rung: usize,
    /// SPT-08 (D7): the fps rung's current rate, or 0 while at the launch rate.
    ladder_fps: i32,
    /// SPT-08 (D4): whether a human owns the external size (`false` ⇒ the ladder does).
    external_pinned: bool,
}

impl Default for EchoState {
    fn default() -> Self {
        Self {
            render: None,
            ui_scale: 1.0,
            stream: None,
            external_resize_supported: None,
            ladder_bias: 0,
            ladder_res_rung: 0,
            ladder_fps: 0,
            external_pinned: false,
        }
    }
}

/// The live "what is the client actually getting" echo, published on every
/// `session_metrics` window.
///
/// Owns its own lock rather than being wrapped in one by the caller — the coherence
/// rule above is a property of this data, so it travels with it. Setters are `&self`.
#[derive(Debug, Default)]
pub struct LiveEcho {
    state: Mutex<EchoState>,
}

impl LiveEcho {
    /// session-display-update: record the compositor's live app-facing render size /
    /// UI scale. `render = None` ⇒ the pinned stream size (default, omitted).
    /// Called by the runner only after the compositor took the values — restoring
    /// the defaults here is how a consumer sees "back to the pinned stream size".
    pub fn set_display(&self, render: Option<(i32, i32)>, ui_scale: f64) {
        let mut s = self.state.lock().unwrap();
        s.render = render;
        s.ui_scale = ui_scale;
    }

    /// adaptive-external-resolution: record the session's live EXTERNAL (encoded)
    /// size. `stream = None` ⇒ back at the launch size.
    ///
    /// Separate from [`Self::set_display`]: the two are written at different times
    /// by different parts of the runner (this half only after the encode graph
    /// negotiated the new caps), and neither should clobber the other by omission.
    pub fn set_external(&self, stream: Option<(i32, i32)>) {
        self.state.lock().unwrap().stream = stream;
    }

    /// adaptive-external-resolution: publish whether this session can be resized
    /// live. Called once by the runner at session start (both variants) — before the
    /// first window drains, which is what makes `external_resize_supported` always
    /// present.
    pub fn set_external_resize_supported(&self, supported: bool) {
        self.state.lock().unwrap().external_resize_supported = Some(supported);
    }

    /// SPT-08 (D6): publish the ladder's current encoder speed-bias rung. Written by
    /// the ladder's `on_window` closure on each actuated step; snapshot, not a counter.
    pub fn set_ladder_bias(&self, bias: u8) {
        self.state.lock().unwrap().ladder_bias = bias;
    }

    /// SPT-08 (D6): publish the ladder's current resolution rung index (0 = launch).
    pub fn set_ladder_res_rung(&self, rung: usize) {
        self.state.lock().unwrap().ladder_res_rung = rung;
    }

    /// SPT-08 (D7): publish the fps rung's current rate. `0` ⇒ back at the launch
    /// rate — the [`Reported`] default for this key, not a separate sentinel scheme.
    pub fn set_ladder_fps(&self, fps: i32) {
        self.state.lock().unwrap().ladder_fps = fps;
    }

    /// SPT-08 (D4/D6): publish who owns the external size. Written by the runner's
    /// manual `session_display_update` path — the ladder never sets it (auto is the
    /// default).
    pub fn set_external_owner_pinned(&self, pinned: bool) {
        self.state.lock().unwrap().external_pinned = pinned;
    }

    /// Read the echo for one drain, applying the [`Reported`] rule to every key.
    ///
    /// Non-destructive: the echo is a level, not a counter, so a drain reports it and
    /// leaves it exactly as it found it.
    pub fn snapshot(&self) -> EchoSnapshot {
        let s = *self.state.lock().unwrap();
        EchoSnapshot {
            render_width: Reported::maybe(s.render.map(|(w, _)| w)),
            render_height: Reported::maybe(s.render.map(|(_, h)| h)),
            ui_scale: Reported::unless_near(s.ui_scale, 1.0),
            stream_width: Reported::maybe(s.stream.map(|(w, _)| w)),
            stream_height: Reported::maybe(s.stream.map(|(_, h)| h)),
            external_resize_supported: Reported::maybe(s.external_resize_supported),
            ladder_speed_bias: Reported::unless(s.ladder_bias as i32, 0),
            ladder_res_rung: Reported::unless(s.ladder_res_rung as i32, 0),
            ladder_fps: Reported::unless(s.ladder_fps, 0),
            // Rides the SIZE echo: present only while the external size is off the
            // launch size, because at the launch size there is nothing to own.
            external_owner: Reported::maybe(s.stream.map(|_| {
                if s.external_pinned {
                    "pinned"
                } else {
                    "auto"
                }
            })),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_fresh_echo_reports_nothing_but_the_capability_gap() {
        let e = LiveEcho::default();
        let s = e.snapshot();
        assert_eq!(s.render_width.reported(), None);
        assert_eq!(s.render_height.reported(), None);
        assert_eq!(s.ui_scale.reported(), None);
        assert_eq!(s.stream_width.reported(), None);
        assert_eq!(s.stream_height.reported(), None);
        assert_eq!(s.external_resize_supported.reported(), None);
        assert_eq!(s.ladder_speed_bias.reported(), None);
        assert_eq!(s.ladder_res_rung.reported(), None);
        assert_eq!(s.ladder_fps.reported(), None);
        assert_eq!(s.external_owner.reported(), None);
    }

    #[test]
    fn every_key_appears_once_it_is_off_its_default() {
        let e = LiveEcho::default();
        e.set_display(Some((1280, 720)), 1.5);
        e.set_external(Some((960, 540)));
        e.set_external_resize_supported(true);
        e.set_ladder_bias(2);
        e.set_ladder_res_rung(1);
        e.set_ladder_fps(60);
        let s = e.snapshot();
        assert_eq!(s.render_width.reported(), Some(1280));
        assert_eq!(s.render_height.reported(), Some(720));
        assert_eq!(s.ui_scale.reported(), Some(1.5));
        assert_eq!(s.stream_width.reported(), Some(960));
        assert_eq!(s.stream_height.reported(), Some(540));
        assert_eq!(s.external_resize_supported.reported(), Some(true));
        assert_eq!(s.ladder_speed_bias.reported(), Some(2));
        assert_eq!(s.ladder_res_rung.reported(), Some(1));
        assert_eq!(s.ladder_fps.reported(), Some(60));
        assert_eq!(s.external_owner.reported(), Some("auto"));
    }

    #[test]
    fn the_owner_rides_the_size_echo_not_the_pin_flag() {
        let e = LiveEcho::default();
        // Pinned at the launch size: nothing to own, so nothing is reported.
        e.set_external_owner_pinned(true);
        assert_eq!(e.snapshot().external_owner.reported(), None);
        e.set_external(Some((1280, 720)));
        assert_eq!(e.snapshot().external_owner.reported(), Some("pinned"));
        e.set_external_owner_pinned(false);
        assert_eq!(e.snapshot().external_owner.reported(), Some("auto"));
        // Back at the launch size: the owner goes with the size.
        e.set_external(None);
        assert_eq!(e.snapshot().external_owner.reported(), None);
    }

    #[test]
    fn a_scale_of_one_is_the_default_however_it_arrives() {
        let e = LiveEcho::default();
        e.set_display(None, 1.0);
        assert_eq!(e.snapshot().ui_scale.reported(), None);
        e.set_display(None, 2.0);
        assert_eq!(e.snapshot().ui_scale.reported(), Some(2.0));
        e.set_display(None, 1.0);
        assert_eq!(e.snapshot().ui_scale.reported(), None);
    }

    #[test]
    fn a_snapshot_does_not_consume_the_echo() {
        let e = LiveEcho::default();
        e.set_ladder_res_rung(3);
        assert_eq!(e.snapshot().ladder_res_rung.reported(), Some(3));
        assert_eq!(e.snapshot().ladder_res_rung.reported(), Some(3));
    }
}
