//! `console_display`, `console_audio`, `console_ddc` (agent-api.md amendment 17, RH-07
//! #407). Reported on every host, `skip` while console mode is off, never `blocks` —
//! console mode is an operator convenience, not a scheduling gate.
//!
//! `console_display` reads the startup preflight's cached finding
//! (`session::console_preflight::last`) rather than probing the DRM card node again
//! here: a readiness probe runs far more often than a fresh master-acquire attempt is
//! safe to make, and re-opening the card for master outside of startup would race a live
//! `spawn_weston_console` for exactly the reason `session::console::drm_open_lock` exists.

use crate::messages::ReadinessCheck;

pub const CHECK_DISPLAY: &str = "console_display";
pub const CHECK_AUDIO: &str = "console_audio";
pub const CHECK_DDC: &str = "console_ddc";

const OFF_SUMMARY: &str = "Console mode is off for this host.";

/// What each check reads, gathered once per readiness probe.
#[derive(Debug, Clone, Default)]
pub struct ConsoleView {
    pub enabled: bool,
    /// This agent was created with the console additions (`QUASAR_CONSOLE_ACCESS=1`).
    /// Without it no preflight ever ran, and console mode cannot be turned on here.
    pub has_access: bool,
    /// The startup preflight's own finding; `None` before it has run (or when
    /// `has_access` is false, which never runs one).
    pub preflight: Option<crate::session::console_preflight::Preflight>,
    pub audio: AudioView,
    pub ddc: crate::ddc::DdcSummary,
}

/// For now, ALSA only (chunk 4 lands the PipeWire alternative, RH07-15 §3): `sinks` is
/// whatever `capacity::detect_audio_sinks` found and could actually open. That function
/// already carries `pipewire:*` ids alongside `hw:*` ones once chunk 4 lands, so this
/// check needs no change then — it only ever asks "is there at least one usable sink".
#[derive(Debug, Clone, Default)]
pub struct AudioView {
    pub sinks: Vec<crate::messages::AudioSink>,
}

pub fn check_display(v: &ConsoleView) -> ReadinessCheck {
    if !v.enabled {
        return super::skip(CHECK_DISPLAY, OFF_SUMMARY);
    }
    if !v.has_access {
        return super::fail(
            CHECK_DISPLAY,
            "this node agent was not created with console access".into(),
            "Turn console mode on for this host from Fleet; the recovery actor replaces \
             the node agent with one that has display, sound and monitor-control access."
                .into(),
        );
    }
    match &v.preflight {
        None => super::unknown(
            CHECK_DISPLAY,
            "no console preflight has run on this agent yet",
        ),
        Some(p) if p.ok => super::pass(
            CHECK_DISPLAY,
            "this agent's startup preflight found the console display free".into(),
        ),
        Some(p) => super::fail(
            CHECK_DISPLAY,
            p.detail
                .clone()
                .unwrap_or_else(|| "the console display is not available".into()),
            "Free the display, or run host preparation (deploy/prepare-host.sh --console) \
             as root, then turn console mode off and back on to try again."
                .into(),
        ),
    }
}

pub fn check_audio(v: &ConsoleView) -> ReadinessCheck {
    if !v.enabled {
        return super::skip(CHECK_AUDIO, OFF_SUMMARY);
    }
    match v.audio.sinks.first() {
        Some(sink) => super::pass(
            CHECK_AUDIO,
            format!("{} ({}) is usable for console audio", sink.label, sink.id),
        ),
        None => super::warn_check(
            CHECK_AUDIO,
            "no local audio sink was found for console mode".into(),
            "Check the host has a sound device passed to the agent (/dev/snd) and that \
             /proc/asound lists a card."
                .into(),
        ),
    }
}

pub fn check_ddc(v: &ConsoleView) -> ReadinessCheck {
    if !v.enabled {
        return super::skip(CHECK_DDC, OFF_SUMMARY);
    }
    let d = &v.ddc;
    if !d.available {
        return super::warn_check(
            CHECK_DDC,
            "ddcutil is not present in this agent image; monitor-power detection is off \
             (every connected display is treated as powered on)"
                .into(),
            "Rebuild the node agent image with ddcutil, or ignore it: display detection \
             and hotplug still work without it."
                .into(),
        );
    }
    if !d.bus_mapped {
        return super::warn_check(
            CHECK_DDC,
            "no I2C bus is mapped to a display connector yet".into(),
            "Check /dev/i2c-* is present (host preparation grants it on a rootless engine) \
             and that a monitor is connected."
                .into(),
        );
    }
    if !d.any_read {
        return super::unknown(
            CHECK_DDC,
            "an I2C bus is mapped to a connector, but no monitor-power read has completed yet",
        );
    }
    if d.all_off {
        return super::warn_check(
            CHECK_DDC,
            "every mapped monitor currently reads DDC power off/standby".into(),
            "Turn the monitor on.".into(),
        );
    }
    super::pass(
        CHECK_DDC,
        "read at least one connected monitor's DDC power state".into(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::console_preflight::Preflight;

    fn view(enabled: bool) -> ConsoleView {
        ConsoleView {
            enabled,
            ..Default::default()
        }
    }

    #[test]
    fn every_check_skips_while_console_mode_is_off() {
        let v = view(false);
        for check in [check_display(&v), check_audio(&v), check_ddc(&v)] {
            assert_eq!(check.status, super::super::SKIP, "{check:?}");
            assert!(check.blocks.is_none(), "{check:?}");
        }
    }

    #[test]
    fn display_fails_without_access_and_is_unknown_before_the_first_preflight() {
        let mut v = view(true);
        let no_access = check_display(&v);
        assert_eq!(no_access.status, super::super::FAIL);
        assert!(no_access.blocks.is_none());

        v.has_access = true;
        let no_run = check_display(&v);
        assert_eq!(no_run.status, super::super::UNKNOWN, "{no_run:?}");
    }

    #[test]
    fn display_passes_or_fails_named_from_the_cached_preflight() {
        let mut v = view(true);
        v.has_access = true;
        v.preflight = Some(Preflight {
            ok: true,
            detail: None,
        });
        assert_eq!(check_display(&v).status, super::super::PASS);

        v.preflight = Some(Preflight {
            ok: false,
            detail: Some("gdm, the login screen holds the display".into()),
        });
        let failed = check_display(&v);
        assert_eq!(failed.status, super::super::FAIL);
        assert!(
            failed.summary.contains("gdm, the login screen"),
            "{failed:?}"
        );
        assert!(failed.blocks.is_none());
    }

    #[test]
    fn audio_passes_with_a_sink_and_warns_without_one() {
        let mut v = view(true);
        assert_eq!(check_audio(&v).status, super::super::WARN);
        v.audio.sinks.push(crate::messages::AudioSink {
            id: "hw:0,3".into(),
            label: "HDMI / DisplayPort".into(),
        });
        let c = check_audio(&v);
        assert_eq!(c.status, super::super::PASS);
        assert!(c.summary.contains("hw:0,3"), "{c:?}");
    }

    #[test]
    fn ddc_walks_available_bus_mapped_any_read_all_off_in_order() {
        let mut v = view(true);
        assert_eq!(check_ddc(&v).status, super::super::WARN, "no ddcutil");

        v.ddc.available = true;
        assert_eq!(check_ddc(&v).status, super::super::WARN, "no bus mapped");

        v.ddc.bus_mapped = true;
        assert_eq!(check_ddc(&v).status, super::super::UNKNOWN, "no read yet");

        v.ddc.any_read = true;
        v.ddc.all_off = true;
        assert_eq!(check_ddc(&v).status, super::super::WARN, "all off");

        v.ddc.all_off = false;
        assert_eq!(check_ddc(&v).status, super::super::PASS);
    }
}
