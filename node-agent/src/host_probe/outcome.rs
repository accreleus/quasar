//! From a probe's end to the readiness check the operator reads.

use std::time::{Duration, SystemTime};

use super::{ProbeKind, ProbeTarget};
use crate::readiness::report::ReadinessReport;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ProbeOutcome {
    Pass {
        summary: String,
    },
    Fail {
        summary: String,
        remediation: String,
    },
    /// The path is never used on this host, so there is nothing to prove.
    NotApplicable {
        summary: String,
    },
    /// Could not be concluded. Never a failure, never a skip.
    Indeterminate {
        reason: String,
    },
}

/// How a child-process probe ended. `stdout` is the child's one-line result.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ChildEnd {
    Exited { code: i32, stdout: String },
    Signaled(i32),
    Deadline(Duration),
    Preempted,
    SpawnFailed(String),
}

/// The wire status of an indeterminate result. `warn` until the #260 contract
/// amendment adds `unknown`; #261 changes this one function.
pub fn indeterminate_status() -> &'static str {
    crate::readiness::WARN
}

fn signal_name(signal: i32) -> String {
    match signal {
        libc::SIGSEGV => "SIGSEGV".into(),
        libc::SIGABRT => "SIGABRT".into(),
        libc::SIGBUS => "SIGBUS".into(),
        libc::SIGILL => "SIGILL".into(),
        libc::SIGFPE => "SIGFPE".into(),
        libc::SIGKILL => "SIGKILL".into(),
        libc::SIGTERM => "SIGTERM".into(),
        other => format!("signal {other}"),
    }
}

/// (what passed, what failed, what was being exercised) for operator sentences.
fn wording(target: ProbeTarget) -> (String, String, String) {
    let gpu = target
        .gpu
        .map_or("The GPU".to_string(), |i| format!("GPU {i}"));
    match target.kind {
        ProbeKind::Media => (
            format!("{gpu} composited and encoded frames"),
            format!("{gpu} could not composite and encode"),
            format!("compositing and encoding on {gpu}"),
        ),
        ProbeKind::ApplicationGpu => (
            format!("An application container opened {gpu}"),
            format!("An application container could not open {gpu}"),
            format!("application access to {gpu}"),
        ),
        ProbeKind::Input => (
            "Virtual input devices were created".into(),
            "Virtual input devices could not be created".into(),
            "virtual input".into(),
        ),
        ProbeKind::Audio => (
            "The audio sidecar started".into(),
            "The audio sidecar did not start".into(),
            "the audio sidecar".into(),
        ),
    }
}

/// The fix an operator reads for a failing probe of `kind`. Also used by the runner to
/// word a `Fail` it builds without going through [`child_outcome`] (a GPU that cannot be
/// bound for encoding never spawns a child).
pub fn remediation(kind: ProbeKind) -> String {
    match kind {
        ProbeKind::Media => "Check the render node is passed to the agent container, the \
            driver/driver volume, and the agent log for `token=\"host-probe-` lines."
            .into(),
        ProbeKind::Input => "Check /dev/uinput is passed to the container and \
            `device_cgroup_rules: ['c 13:* rmw']` is set."
            .into(),
        ProbeKind::Audio => "Check the runtime lets the agent image start the audio sidecar as \
            a sibling container: the runtime_endpoint check, and the agent logs."
            .into(),
        ProbeKind::ApplicationGpu => "Check /dev/dri permissions/groups (the \
            dri_node_app_access check) and, on NVIDIA, the driver volume \
            (driver_volume_version check)."
            .into(),
    }
}

pub fn child_outcome(target: ProbeTarget, end: ChildEnd) -> ProbeOutcome {
    let (passed, failed, exercising) = wording(target);
    match end {
        ChildEnd::Exited { code: 0, stdout } => ProbeOutcome::Pass {
            summary: format!("{passed}: {stdout}"),
        },
        ChildEnd::Exited { stdout, .. } => ProbeOutcome::Fail {
            summary: format!("{failed}: {stdout}"),
            remediation: remediation(target.kind),
        },
        ChildEnd::Signaled(signal) => ProbeOutcome::Fail {
            summary: format!(
                "The host probe of {exercising} crashed with {}",
                signal_name(signal)
            ),
            remediation: remediation(target.kind),
        },
        ChildEnd::Deadline(deadline) => ProbeOutcome::Indeterminate {
            reason: format!(
                "The host probe of {exercising} did not finish within {} s",
                deadline.as_secs()
            ),
        },
        ChildEnd::Preempted => ProbeOutcome::Indeterminate {
            reason: format!(
                "A session launch took priority over the host probe of {exercising}; it will run again"
            ),
        },
        ChildEnd::SpawnFailed(err) => ProbeOutcome::Indeterminate {
            reason: format!("The host probe of {exercising} could not start: {err}"),
        },
    }
}

/// Indeterminate never replaces a held pass or fail.
pub fn record(
    report: &mut ReadinessReport,
    target: ProbeTarget,
    outcome: ProbeOutcome,
    observed_at: SystemTime,
) {
    let id = target.check_id();
    match outcome {
        ProbeOutcome::Pass { summary } => report.retain(
            crate::messages::ReadinessCheck {
                id,
                status: crate::readiness::PASS.into(),
                summary,
                remediation: String::new(),
            },
            observed_at,
        ),
        ProbeOutcome::Fail {
            summary,
            remediation,
        } => report.retain(
            crate::messages::ReadinessCheck {
                id,
                status: crate::readiness::FAIL.into(),
                summary,
                remediation,
            },
            observed_at,
        ),
        ProbeOutcome::NotApplicable { summary } => report.retain(
            crate::messages::ReadinessCheck {
                id,
                status: crate::readiness::SKIP.into(),
                summary,
                remediation: String::new(),
            },
            observed_at,
        ),
        ProbeOutcome::Indeterminate { reason } => {
            let stands = matches!(
                report.retained(&id).map(|c| c.status.as_str()),
                Some(status) if status == crate::readiness::PASS
                    || status == crate::readiness::FAIL
            );
            if stands {
                return;
            }
            report.retain(
                crate::messages::ReadinessCheck {
                    id,
                    status: indeterminate_status().into(),
                    summary: reason,
                    remediation: "The agent will run the host probe again on the next input \
                        change, launch failure, or agent restart."
                        .into(),
                },
                observed_at,
            );
        }
    }
}

pub fn record_not_applicable(report: &mut ReadinessReport, kind: ProbeKind, at: SystemTime) {
    report.retain(
        crate::messages::ReadinessCheck {
            id: kind.check_id().to_string(),
            status: crate::readiness::SKIP.into(),
            summary: "This host has no GPU.".into(),
            remediation: String::new(),
        },
        at,
    );
}

pub fn forget(report: &mut ReadinessReport, target: ProbeTarget) {
    report.forget(&target.check_id());
}
