//! `encoder.stall` — "the encoder has stopped producing output", as a fact rather than
//! an inference.
//!
//! Before this, no stall event/metric/counter existed anywhere in the agent
//! (`interpipe_queue_level_max` is a queue depth, not encoder state), so a headless-Chrome
//! peer rejecting the HEVC m-line (a negotiation fact, not a throughput one) was misdiagnosed
//! for hours as an encode-src ring stall.
//!
//! A stall is encoder OUTPUT silence for at least [`ENCODER_STALL_MS`] while INPUT keeps
//! arriving. The reason discriminant is the point: the same silence means three different
//! things depending on whether input is still flowing.
//!
//! Not the drop scan: `metrics::DROP_TIMEOUT` (500 ms) asks a per-frame question at drain
//! cadence; this asks a session-level question on the 100 ms supervision tick. The two are
//! deliberately independent.

use std::time::Duration;

/// Output-silence threshold. **The spec is `docs/session-trace/thresholds.json`
/// `agent.encoder_stall_ms`; this constant is the copy**, and
/// `tests::constant_matches_the_threshold_manifest` fails if the two drift.
pub const ENCODER_STALL_MS: u64 = 1000;

/// Why the encoder's output went quiet. Named on the wire (`encoder.stall.reason`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StallReason {
    /// Input is still flowing and nothing is coming out — the encoder itself.
    NoOutput,
    /// Input stopped too. The encoder is idle because it is being fed nothing;
    /// look upstream (compositor, interpipe, the app), not at the encoder.
    InputStarved,
    /// A not-negotiated / caps error was seen on the encode bus during the silence.
    /// This outranks the other two: a graph that cannot agree caps is not a
    /// throughput problem and must never be reported as one.
    Negotiation,
}

impl StallReason {
    pub fn as_str(self) -> &'static str {
        match self {
            StallReason::NoOutput => "no_output",
            StallReason::InputStarved => "input_starved",
            StallReason::Negotiation => "negotiation",
        }
    }
}

/// What [`StallDetector::poll`] wants reported.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StallEvent {
    Detected {
        reason: StallReason,
        since_ms: u64,
    },
    Recovered {
        reason: StallReason,
        stalled_ms: u64,
    },
}

/// The classification, with no state: is the encoder stalled *right now*?
///
/// `since_in` / `since_out` are how long each side has been silent; `None` ⇒ that side has
/// never fired. `running_for` is how long the session has been live, which is what stands
/// in for output silence when the encoder has produced nothing at all.
pub fn classify(
    threshold: Duration,
    since_in: Option<Duration>,
    since_out: Option<Duration>,
    running_for: Duration,
    negotiation_error: bool,
) -> Option<(StallReason, Duration)> {
    // Nothing has ever flowed either direction: pre-roll, not a stall. The app-boot and
    // idle-reap watchdogs already cover a session that never encodes anything.
    if since_in.is_none() && since_out.is_none() {
        return None;
    }
    let out_silence = since_out.unwrap_or(running_for);
    if out_silence < threshold {
        return None;
    }
    if negotiation_error {
        return Some((StallReason::Negotiation, out_silence));
    }
    let input_flowing = since_in.is_some_and(|d| d < threshold);
    let reason = if input_flowing {
        StallReason::NoOutput
    } else {
        StallReason::InputStarved
    };
    Some((reason, out_silence))
}

/// One open stall per session at a time. Edge-triggered: `poll` returns an event only on
/// the transition into or out of a stall, never once per tick.
#[derive(Debug, Default)]
pub struct StallDetector {
    open: Option<(StallReason, Duration)>,
}

impl StallDetector {
    /// Advance the state machine one supervision tick.
    pub fn poll(
        &mut self,
        threshold: Duration,
        since_in: Option<Duration>,
        since_out: Option<Duration>,
        running_for: Duration,
        negotiation_error: bool,
    ) -> Option<StallEvent> {
        let now = classify(
            threshold,
            since_in,
            since_out,
            running_for,
            negotiation_error,
        );
        match (self.open, now) {
            // Entering a stall.
            (None, Some((reason, since))) => {
                self.open = Some((reason, since));
                Some(StallEvent::Detected {
                    reason,
                    since_ms: since.as_millis() as u64,
                })
            }
            // Still stalled. Deliberately silent, and deliberately NOT re-opened under a
            // changed reason: one open stall means one Detected/Recovered pair, so a
            // reason that flickers cannot produce a storm.
            (Some(_), Some(_)) => None,
            // Recovered. The duration reported is the output silence at its longest,
            // i.e. what it had reached on the last stalled tick.
            (Some((reason, worst)), None) => {
                self.open = None;
                Some(StallEvent::Recovered {
                    reason,
                    stalled_ms: worst.as_millis() as u64,
                })
            }
            (None, None) => None,
        }
    }

    /// Keep the worst-so-far silence for the recovery event. Called on every tick while a
    /// stall is open; separate from `poll`'s edge logic so the growing duration cannot
    /// re-trigger anything.
    pub fn observe(&mut self, since_out: Option<Duration>, running_for: Duration) {
        if let Some((_, worst)) = self.open.as_mut() {
            let now = since_out.unwrap_or(running_for);
            if now > *worst {
                *worst = now;
            }
        }
    }

    pub fn is_open(&self) -> bool {
        self.open.is_some()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const T: Duration = Duration::from_millis(ENCODER_STALL_MS);
    fn ms(n: u64) -> Duration {
        Duration::from_millis(n)
    }

    /// `docs/session-trace/thresholds.json` is the SPEC and this constant is the copy —
    /// the same drift discipline the Go and web consumers are held to.
    #[test]
    fn constant_matches_the_threshold_manifest() {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../docs/session-trace/thresholds.json");
        let body = std::fs::read_to_string(&path)
            .unwrap_or_else(|e| panic!("cannot read {}: {e}", path.display()));
        let doc: serde_json::Value = serde_json::from_str(&body).expect("thresholds.json parses");
        let want = doc["thresholds"]["agent.encoder_stall_ms"]["value"]
            .as_u64()
            .expect("thresholds.json has agent.encoder_stall_ms.value");
        assert_eq!(
            ENCODER_STALL_MS, want,
            "ENCODER_STALL_MS drifted from docs/session-trace/thresholds.json"
        );
    }

    #[test]
    fn pre_roll_is_not_a_stall() {
        assert_eq!(classify(T, None, None, ms(30_000), false), None);
    }

    #[test]
    fn input_flowing_with_no_output_is_the_encoder() {
        let got = classify(T, Some(ms(20)), Some(ms(1500)), ms(60_000), false);
        assert_eq!(got, Some((StallReason::NoOutput, ms(1500))));
    }

    #[test]
    fn silence_on_both_sides_points_upstream() {
        let got = classify(T, Some(ms(1400)), Some(ms(1400)), ms(60_000), false);
        assert_eq!(got, Some((StallReason::InputStarved, ms(1400))));
    }

    #[test]
    fn a_caps_error_outranks_the_throughput_reading() {
        let got = classify(T, Some(ms(20)), Some(ms(1500)), ms(60_000), true);
        assert_eq!(got, Some((StallReason::Negotiation, ms(1500))));
    }

    #[test]
    fn an_encoder_that_never_produced_anything_is_measured_from_session_start() {
        // Frames are arriving, nothing has ever come out, and the session has been up
        // longer than the threshold.
        let got = classify(T, Some(ms(20)), None, ms(4_000), false);
        assert_eq!(got, Some((StallReason::NoOutput, ms(4_000))));
        // …but not before the threshold has elapsed.
        assert_eq!(classify(T, Some(ms(20)), None, ms(400), false), None);
    }

    #[test]
    fn healthy_flow_reports_nothing() {
        assert_eq!(
            classify(T, Some(ms(16)), Some(ms(16)), ms(60_000), false),
            None
        );
    }

    #[test]
    fn one_open_stall_then_one_recovery_and_re_arm() {
        let mut d = StallDetector::default();
        assert_eq!(
            d.poll(T, Some(ms(16)), Some(ms(16)), ms(9_000), false),
            None
        );
        assert_eq!(
            d.poll(T, Some(ms(16)), Some(ms(1100)), ms(9_000), false),
            Some(StallEvent::Detected {
                reason: StallReason::NoOutput,
                since_ms: 1100
            })
        );
        assert!(d.is_open());
        // Deeper into the same stall: silent, and the worst-so-far grows.
        assert_eq!(
            d.poll(T, Some(ms(16)), Some(ms(2600)), ms(9_000), false),
            None
        );
        d.observe(Some(ms(2600)), ms(9_000));
        assert_eq!(
            d.poll(T, Some(ms(16)), Some(ms(16)), ms(9_000), false),
            Some(StallEvent::Recovered {
                reason: StallReason::NoOutput,
                stalled_ms: 2600
            })
        );
        assert!(!d.is_open());
        // Re-armed.
        assert_eq!(
            d.poll(T, Some(ms(16)), Some(ms(1100)), ms(9_000), false),
            Some(StallEvent::Detected {
                reason: StallReason::NoOutput,
                since_ms: 1100
            })
        );
    }

    #[test]
    fn a_reason_that_flickers_mid_stall_does_not_reopen_it() {
        let mut d = StallDetector::default();
        d.poll(T, Some(ms(16)), Some(ms(1100)), ms(9_000), false);
        assert_eq!(
            d.poll(T, Some(ms(1200)), Some(ms(1200)), ms(9_000), false),
            None,
            "the reason changed but the stall never closed — no second Detected"
        );
    }
}
