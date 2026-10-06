//! The direct-display console engine (ADR 0009): the console session's app container
//! drives the monitor itself. The agent takes the terminal, claims the card, launches the
//! container with the console plan's grants, watches that it is displaying, and stops it.
//! No compositor, pipeline or encoder exists for this session.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use super::super::console_plan::{self, ConsoleHost, DirectDisplay};
use super::super::container::{AppDisplayMode, ContainerRuntime, LaunchParams, RunningContainer};
use super::super::displaying::{self, Missing, ScanoutMode, Verdict};
use super::super::metrics::SessionMetrics;
use super::super::source::{spawn_observer, GenerationObservation};
use super::super::{teardown, SessionConfig};
use super::{
    app_container_name, app_exit_event, compute_bytes_used, console_leg, now_unix_ms,
    DiagnosticEventTx, SessionEvent, TraceEvent,
};
use crate::messages::VideoTopology;

/// A console session whose app declares it can run direct. Everything else on the
/// local-only topology still takes the nested path until it is retired (#461).
pub(super) fn wants_direct(cfg: &SessionConfig) -> bool {
    cfg.video_topology == VideoTopology::LocalOnly
        && cfg.container.as_ref().is_some_and(|c| c.direct_display)
}

/// How long the desktop gets to open the card before the agent looks: the agent's own
/// open would make it DRM master if it came first.
const FIRST_LOOK: Duration = Duration::from_secs(5);
/// How often the display is read once the desktop has had its first look.
const LOOK_EVERY: Duration = Duration::from_secs(2);
/// Without a session `app_boot_timeout`, how long the desktop gets to display.
const DEFAULT_DISPLAY_BUDGET: Duration = Duration::from_secs(120);
const POLL: Duration = Duration::from_millis(250);

/// What one display reading means for the session.
#[derive(Debug, Clone, PartialEq, Eq)]
enum WatchStep {
    /// Not displaying yet, within budget.
    Wait,
    /// Displaying for the first time: the session is running.
    Running(Option<ScanoutMode>),
    /// Still displaying; the mode moved.
    ModeChanged(Option<ScanoutMode>),
    /// Was displaying, is not now. The session goes on: a desktop can drop the display
    /// for a moment (a VT switch, a modeset), and its exit ends the session anyway.
    Lost(Vec<Missing>),
    /// Displaying again after a loss.
    Regained(Option<ScanoutMode>),
    /// Never displayed within the budget.
    NeverDisplayed(Vec<Missing>),
    /// Nothing changed.
    Same,
}

#[derive(Debug)]
struct DisplayWatch {
    budget: Duration,
    running: bool,
    displaying: bool,
    mode: Option<ScanoutMode>,
}

impl DisplayWatch {
    fn new(budget: Duration) -> Self {
        Self {
            budget,
            running: false,
            displaying: false,
            mode: None,
        }
    }

    fn observe(&mut self, verdict: Verdict, age: Duration) -> WatchStep {
        match verdict {
            Verdict::Displaying(mode) => {
                let was_displaying = std::mem::replace(&mut self.displaying, true);
                let moved = std::mem::replace(&mut self.mode, mode) != mode;
                if !std::mem::replace(&mut self.running, true) {
                    WatchStep::Running(mode)
                } else if !was_displaying {
                    WatchStep::Regained(mode)
                } else if moved {
                    WatchStep::ModeChanged(mode)
                } else {
                    WatchStep::Same
                }
            }
            Verdict::NotDisplaying(missing) => {
                if !self.running {
                    if age >= self.budget {
                        WatchStep::NeverDisplayed(missing)
                    } else {
                        WatchStep::Wait
                    }
                } else if std::mem::replace(&mut self.displaying, false) {
                    WatchStep::Lost(missing)
                } else {
                    WatchStep::Same
                }
            }
        }
    }
}

/// Run one direct-display console session to its end on the calling thread. The terminal
/// event goes out only once the container is stopped, the card claim released and the
/// console terminal restored, so a relaunch on it never meets this session's leftovers.
pub(super) fn run_direct<F: Fn(SessionEvent)>(
    session_id: &str,
    cfg: &SessionConfig,
    emit: &F,
    diagnostic_tx: DiagnosticEventTx,
    stop: Arc<AtomicBool>,
    session_metrics: Arc<SessionMetrics>,
) {
    emit(SessionEvent::Starting);
    let end = run_until_end(
        session_id,
        cfg,
        emit,
        &diagnostic_tx,
        &stop,
        &session_metrics,
    );
    emit(end);
}

/// Why a console session ended in failure; each is logged under its own token.
#[derive(Debug, Clone, Copy)]
enum Failure {
    ConsoleOff,
    NoApp,
    NoOutput,
    InputRefused,
    LaunchFailed,
    TerminalLost,
    NeverDisplayed,
}

fn failed(failure: Failure, reason: String) -> SessionEvent {
    let message = "console session failed";
    match failure {
        Failure::ConsoleOff => {
            tracing::error!(token = "direct-display-console-off", reason = %reason, "{message}")
        }
        Failure::NoApp => {
            tracing::error!(token = "direct-display-no-app", reason = %reason, "{message}")
        }
        Failure::NoOutput => {
            tracing::error!(token = "direct-display-no-output", reason = %reason, "{message}")
        }
        Failure::InputRefused => {
            tracing::error!(token = "direct-display-input-refused", reason = %reason, "{message}")
        }
        Failure::LaunchFailed => {
            tracing::error!(token = "direct-display-launch-failed", reason = %reason, "{message}")
        }
        Failure::TerminalLost => {
            tracing::error!(token = "direct-display-terminal-lost", reason = %reason, "{message}")
        }
        Failure::NeverDisplayed => {
            tracing::error!(token = "direct-display-never-displayed", reason = %reason, "{message}")
        }
    }
    SessionEvent::Failed(reason)
}

/// Everything the session holds is a local here, released on return.
fn run_until_end<F: Fn(SessionEvent)>(
    session_id: &str,
    cfg: &SessionConfig,
    emit: &F,
    diagnostic_tx: &DiagnosticEventTx,
    stop: &Arc<AtomicBool>,
    session_metrics: &SessionMetrics,
) -> SessionEvent {
    emit(SessionEvent::Progress("taking the console"));
    let Some(console) = cfg.console_config.as_ref().filter(|c| c.enabled) else {
        return failed(
            Failure::ConsoleOff,
            "local_only assignment requires enabled console_config".into(),
        );
    };
    let Some(spec) = cfg.container.clone() else {
        return failed(Failure::NoApp, "a console session needs an app".into());
    };
    // Declared first so it drops last: the terminal is restored after the container and
    // the card claim are gone.
    let mut console_vt = match console_leg::take_terminal(cfg, stop) {
        Ok(vt) => vt,
        Err(reason) => return SessionEvent::Failed(reason),
    };
    let outputs = crate::capacity::detect_drm_outputs();
    let output = match console_plan::console_output(console.output_id.as_deref(), &outputs) {
        Ok(output) => output.clone(),
        Err(reason) => return failed(Failure::NoOutput, reason),
    };
    let input = match console_plan::input_grant(&console.input_devices) {
        Ok(input) => input,
        Err(reason) => return failed(Failure::InputRefused, reason),
    };
    let card_node = format!("/dev/dri/{}", output.card);
    let host = ConsoleHost {
        card_node: card_node.clone(),
        render_node: output.render_node.clone(),
        sound: host_has_sound(),
    };
    let _claim = crate::capacity::claim_display(&output.card);

    emit(SessionEvent::Progress("starting the console desktop"));
    let container_name = app_container_name(session_id, 0);
    let params = LaunchParams {
        session_id,
        wayland_display: "",
        runtime_dir: &cfg.runtime_dir,
        device_nodes: Vec::new(),
        container_name: Some(container_name.clone()),
        nvidia_lib32_path: &cfg.nvidia_lib32_path,
        display: AppDisplayMode {
            width: cfg.stream.width,
            height: cfg.stream.height,
            fps: cfg.stream.fps,
        },
        direct_display: Some(DirectDisplay { host, input }),
    };
    let mut observation = GenerationObservation::new();
    let mut container = match ContainerRuntime::from_env().run(&spec, &params) {
        Ok(container) => container,
        Err(e) => {
            return failed(
                Failure::LaunchFailed,
                format!("console desktop launch failed: {e:#}"),
            )
        }
    };
    tracing::info!(
        output = %output.id,
        "console desktop {} launched to drive the display directly",
        container.name()
    );
    spawn_observer(
        container_name,
        container.application_id(),
        observation.observer(container.removed_flag()),
    );
    session_metrics.set_external_resize_supported(false);

    let trace = |event: &'static str, payload: serde_json::Value| {
        diagnostic_tx.try_emit(
            session_id.to_string(),
            TraceEvent {
                ts_unix_ms: now_unix_ms(),
                event,
                payload,
            },
        );
    };
    let displaying = |mode: Option<ScanoutMode>| {
        session_metrics.set_console_mode(mode.map(|m| (m.width, m.height, m.refresh_millihz)));
        tracing::info!(?mode, "console desktop is displaying");
        trace(
            "console.displaying",
            serde_json::json!({ "mode": mode_json(mode) }),
        );
        emit(SessionEvent::Displaying(mode));
    };
    let started = Instant::now();
    let mut watch = DisplayWatch::new(cfg.app_boot_timeout.unwrap_or(DEFAULT_DISPLAY_BUDGET));
    let mut looked_at: Option<Instant> = None;
    loop {
        if let Some(reason) = console_leg::terminal_lost(&mut console_vt) {
            stop_container(&mut container);
            return failed(
                Failure::TerminalLost,
                format!("console terminal lost: {reason}"),
            );
        }
        if stop.load(Ordering::Relaxed) {
            emit(SessionEvent::Stopping);
            stop_container(&mut container);
            return SessionEvent::Stopped {
                bytes_used: compute_bytes_used(cfg),
                detail: None,
            };
        }
        if let Some(status) = observation.take_exit() {
            tracing::error!(
                token = "direct-display-app-exited",
                status = ?status,
                "console desktop exited"
            );
            trace(
                "app.exited",
                serde_json::json!({
                    "status": format!("{status:?}"),
                    "video_topology": "local_only",
                }),
            );
            let end = app_exit_event(status, watch.running, observation.log_tail());
            stop_container(&mut container);
            return end;
        }
        let due = looked_at.map_or(started.elapsed() >= FIRST_LOOK, |at| {
            at.elapsed() >= LOOK_EVERY
        });
        if due {
            looked_at = Some(Instant::now());
            let facts =
                displaying::read_drm_facts(std::path::Path::new(&card_node), &output.connector);
            match watch.observe(displaying::verdict(&facts), started.elapsed()) {
                WatchStep::Running(mode)
                | WatchStep::ModeChanged(mode)
                | WatchStep::Regained(mode) => displaying(mode),
                WatchStep::Lost(missing) => {
                    let why = describe(&missing);
                    tracing::warn!(
                        token = "direct-display-lost",
                        "console desktop stopped displaying: {why}"
                    );
                    trace(
                        "console.not_displaying",
                        serde_json::json!({ "reason": &why }),
                    );
                    emit(SessionEvent::NotDisplaying(why));
                }
                WatchStep::NeverDisplayed(missing) => {
                    stop_container(&mut container);
                    return failed(
                        Failure::NeverDisplayed,
                        format!(
                            "the console desktop never displayed: {}",
                            describe(&missing)
                        ),
                    );
                }
                WatchStep::Wait | WatchStep::Same => {}
            }
        }
        std::thread::sleep(POLL);
    }
}

fn describe(missing: &[Missing]) -> String {
    missing
        .iter()
        .map(|m| m.describe())
        .collect::<Vec<_>>()
        .join("; ")
}

fn mode_json(mode: Option<ScanoutMode>) -> serde_json::Value {
    mode.map_or(serde_json::Value::Null, |m| {
        serde_json::json!({
            "width": m.width,
            "height": m.height,
            "refresh_millihz": m.refresh_millihz,
        })
    })
}

/// Stops the console container within the session's teardown budget; an unconfirmed stop
/// stays with the runtime's durable cleanup.
fn stop_container(container: &mut RunningContainer) {
    let budget = teardown::RetryBudget::new(teardown::STOP_RETRY_BUDGET);
    let mut last_error = None;
    let report = teardown::retry_with_budget(
        || match container.stop() {
            Ok(()) => teardown::StopAttempt::Confirmed,
            Err(error) => {
                let attempt = teardown::classify_stop(teardown::error_kind(&error));
                last_error = Some(format!("{error:#}"));
                attempt
            }
        },
        &budget,
    );
    if !matches!(report, teardown::StopAttempt::Confirmed) {
        tracing::warn!(
            token = "direct-display-stop-unconfirmed",
            "console desktop stop not confirmed ({report:?}): {}; runtime cleanup holds it",
            last_error.as_deref().unwrap_or("no error reported")
        );
    }
}

/// Does the host have a sound device to hand the desktop? The agent sees the host's `/dev`
/// at `/host/dev` when the recipe mounts it, else its own `/dev`, which carries `/dev/snd`
/// only while the agent's own console grants do (until #461 moves them to the host probe).
fn host_has_sound() -> bool {
    let host_dev = std::path::Path::new("/host/dev");
    let dev = if host_dev.is_dir() {
        host_dev
    } else {
        std::path::Path::new("/dev")
    };
    dev.join("snd").is_dir()
}

#[cfg(test)]
mod tests {
    use super::*;

    const MODE: ScanoutMode = ScanoutMode {
        width: 3840,
        height: 2160,
        refresh_millihz: 239_990,
    };
    const BUDGET: Duration = Duration::from_secs(60);

    fn shown(mode: ScanoutMode) -> Verdict {
        Verdict::Displaying(Some(mode))
    }

    fn dark() -> Verdict {
        Verdict::NotDisplaying(vec![Missing::Master])
    }

    fn secs(n: u64) -> Duration {
        Duration::from_secs(n)
    }

    #[test]
    fn the_first_displaying_reading_makes_the_session_running() {
        let mut watch = DisplayWatch::new(BUDGET);
        assert_eq!(watch.observe(dark(), secs(5)), WatchStep::Wait);
        assert_eq!(
            watch.observe(shown(MODE), secs(7)),
            WatchStep::Running(Some(MODE))
        );
        assert_eq!(watch.observe(shown(MODE), secs(9)), WatchStep::Same);
    }

    #[test]
    fn a_desktop_that_never_displays_fails_at_the_budget_naming_what_was_missing() {
        let mut watch = DisplayWatch::new(BUDGET);
        assert_eq!(watch.observe(dark(), secs(59)), WatchStep::Wait);
        assert_eq!(
            watch.observe(dark(), secs(60)),
            WatchStep::NeverDisplayed(vec![Missing::Master])
        );
    }

    #[test]
    fn losing_the_display_is_reported_once_and_never_fails_the_session() {
        let mut watch = DisplayWatch::new(BUDGET);
        watch.observe(shown(MODE), secs(5));
        assert_eq!(
            watch.observe(dark(), secs(300)),
            WatchStep::Lost(vec![Missing::Master])
        );
        assert_eq!(watch.observe(dark(), secs(302)), WatchStep::Same);
        assert_eq!(
            watch.observe(shown(MODE), secs(304)),
            WatchStep::Regained(Some(MODE))
        );
    }

    #[test]
    fn a_new_mode_is_reported() {
        let mut watch = DisplayWatch::new(BUDGET);
        watch.observe(shown(MODE), secs(5));
        let sixty = ScanoutMode {
            refresh_millihz: 60_000,
            ..MODE
        };
        assert_eq!(
            watch.observe(shown(sixty), secs(7)),
            WatchStep::ModeChanged(Some(sixty))
        );
    }
}
