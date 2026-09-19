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

/// The wire status of an indeterminate result (amendment 11, #261): neither a failure
/// nor a `skip` — a probe that could not be concluded.
pub fn indeterminate_status() -> &'static str {
    crate::readiness::UNKNOWN
}

/// Signals a process raises against itself by crashing. Anything else (SIGKILL from the
/// OOM killer, SIGTERM from an operator) says nothing about the path under test.
pub(super) fn fault_signal(signal: i32) -> Option<&'static str> {
    match signal {
        libc::SIGSEGV => Some("SIGSEGV"),
        libc::SIGABRT => Some("SIGABRT"),
        libc::SIGBUS => Some("SIGBUS"),
        libc::SIGILL => Some("SIGILL"),
        libc::SIGFPE => Some("SIGFPE"),
        libc::SIGSYS => Some("SIGSYS"),
        libc::SIGTRAP => Some("SIGTRAP"),
        _ => None,
    }
}

/// (what passed, what failed, what was being exercised) for operator sentences.
pub(super) fn wording(target: ProbeTarget) -> (String, String, String) {
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
        ChildEnd::Exited { code: 1, stdout } => ProbeOutcome::Fail {
            summary: format!("{failed}: {stdout}"),
            remediation: remediation(target.kind),
        },
        // The child's contract is 0 pass, 1 fail. Anything else (2 is bad argv) is
        // not a statement about the host.
        ChildEnd::Exited { code, stdout } => ProbeOutcome::Indeterminate {
            reason: format!("The host probe of {exercising} exited {code}: {stdout}"),
        },
        ChildEnd::Signaled(signal) => match fault_signal(signal) {
            Some(name) => ProbeOutcome::Fail {
                summary: format!("The host probe of {exercising} crashed with {name}"),
                remediation: remediation(target.kind),
            },
            None => ProbeOutcome::Indeterminate {
                reason: format!(
                    "The host probe of {exercising} was killed from outside (signal {signal})"
                ),
            },
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

/// Indeterminate never replaces a held pass, fail or skip.
pub fn record(
    report: &mut ReadinessReport,
    target: ProbeTarget,
    outcome: ProbeOutcome,
    observed_at: SystemTime,
) {
    let id = target.check_id();
    // `blocks` rides on the check whatever the outcome: it says what a fail would block.
    let blocks = target.blocks();
    match outcome {
        ProbeOutcome::Pass { summary } => report.retain(
            crate::messages::ReadinessCheck {
                id,
                status: crate::readiness::PASS.into(),
                summary,
                remediation: String::new(),
                observed_at: None,
                source: Some("host_probe".into()),
                blocks,
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
                observed_at: None,
                source: Some("host_probe".into()),
                blocks,
            },
            observed_at,
        ),
        ProbeOutcome::NotApplicable { summary } => report.retain(
            crate::messages::ReadinessCheck {
                id,
                status: crate::readiness::SKIP.into(),
                summary,
                remediation: String::new(),
                observed_at: None,
                source: Some("host_probe".into()),
                blocks,
            },
            observed_at,
        ),
        ProbeOutcome::Indeterminate { reason } => {
            let stands = matches!(
                report.retained(&id).map(|c| c.status.as_str()),
                Some(status) if status == crate::readiness::PASS
                    || status == crate::readiness::FAIL
                    || status == crate::readiness::SKIP
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
                    observed_at: None,
                    source: Some("host_probe".into()),
                    blocks,
                },
                observed_at,
            );
        }
    }
}

/// Kind-level skip for a host with no GPU: there is no index to scope a block to.
pub fn record_not_applicable(report: &mut ReadinessReport, kind: ProbeKind, at: SystemTime) {
    report.retain(
        crate::messages::ReadinessCheck {
            id: kind.check_id().to_string(),
            status: crate::readiness::SKIP.into(),
            summary: "This host has no GPU.".into(),
            remediation: String::new(),
            observed_at: None,
            source: Some("host_probe".into()),
            blocks: None,
        },
        at,
    );
}

pub fn forget(report: &mut ReadinessReport, target: ProbeTarget) {
    report.forget(&target.check_id());
}
