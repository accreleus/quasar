//! From a probe's end to the readiness check the operator reads.

use std::collections::BTreeMap;
use std::time::{Duration, SystemTime};

use super::decision::EvidenceStamp;
use super::{ProbeCodec, ProbeKind, ProbeTarget};
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

/// How a child-process probe ended. `stdout` is the child's one-line result;
/// `remediation` is a child-supplied override of the per-kind default, from a
/// `quasar-probe-remediation:` line — see `host_probe::child::drain_stdout`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ChildEnd {
    Exited {
        code: i32,
        stdout: String,
        remediation: Option<String>,
    },
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
    if let Some(codec) = target.codec {
        let codec = codec.as_str();
        return (
            format!("{gpu} encoded {codec}"),
            format!("{gpu} does not encode {codec}; sessions will not use {codec} on this GPU"),
            format!("{codec} encoding on {gpu}"),
        );
    }
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

/// [`remediation`] for the target's kind, except a codec probe: its failure is often the
/// hardware (a video engine with no encoder for that codec), which nothing on the host fixes.
pub fn remediation_for(target: ProbeTarget) -> String {
    match target.codec {
        Some(codec) => {
            let codec = codec.as_str();
            format!(
                "Nothing needs fixing if this GPU's video engine has no {codec} encoder: \
                 sessions on it use another codec. If it should encode {codec}, check the \
                 driver and the agent log for `token=\"host-probe-` lines."
            )
        }
        None => remediation(target.kind),
    }
}

/// The retained verdict of the codec probe for (`gpu`, `codec`): `Some(true)` pass,
/// `Some(false)` fail, `None` when no definitive run is held (absent, indeterminate, skip).
/// An indeterminate run never replaces a held verdict ([`record`]), so this is the last
/// definitive one.
pub fn codec_probe_verdict(report: &ReadinessReport, gpu: i32, codec: ProbeCodec) -> Option<bool> {
    let check = report.retained(&ProbeTarget::codec(gpu, codec).check_id())?;
    match check.status.as_str() {
        crate::readiness::PASS => Some(true),
        crate::readiness::FAIL => Some(false),
        _ => None,
    }
}

/// Which stack each held codec-probe pass was proven under (#301), kept beside the
/// report and fed by the same updates. Agent-side only: the check on the wire is unchanged.
#[derive(Debug, Default)]
pub struct CodecEvidence(BTreeMap<ProbeTarget, EvidenceStamp>);

impl CodecEvidence {
    /// Mirrors [`record`]: a pass takes the run's stamp, a fail or skip drops it, and an
    /// indeterminate run leaves the held verdict — and its stamp — standing.
    pub fn note(
        &mut self,
        target: ProbeTarget,
        outcome: &ProbeOutcome,
        stamp: Option<EvidenceStamp>,
    ) {
        if target.codec.is_none() {
            return;
        }
        match (outcome, stamp) {
            (ProbeOutcome::Indeterminate { .. }, _) => {}
            (ProbeOutcome::Pass { .. }, Some(stamp)) => {
                self.0.insert(target, stamp);
            }
            _ => {
                self.0.remove(&target);
            }
        }
    }

    pub fn forget(&mut self, target: ProbeTarget) {
        self.0.remove(&target);
    }

    /// The held codec-probe verdict for (`gpu`, `codec`) is a pass proven under
    /// `current`, the stamp the current stack gives that GPU index.
    pub fn proven(
        &self,
        report: &ReadinessReport,
        gpu: i32,
        codec: ProbeCodec,
        current: Option<&EvidenceStamp>,
    ) -> bool {
        codec_probe_verdict(report, gpu, codec) == Some(true)
            && current.is_some()
            && self.0.get(&ProbeTarget::codec(gpu, codec)) == current
    }
}

pub fn child_outcome(target: ProbeTarget, end: ChildEnd) -> ProbeOutcome {
    let (passed, failed, exercising) = wording(target);
    match end {
        ChildEnd::Exited {
            code: 0, stdout, ..
        } => ProbeOutcome::Pass {
            summary: format!("{passed}: {stdout}"),
        },
        ChildEnd::Exited {
            code: 1,
            stdout,
            remediation: child_remediation,
        } => ProbeOutcome::Fail {
            summary: format!("{failed}: {stdout}"),
            remediation: child_remediation.unwrap_or_else(|| remediation_for(target)),
        },
        // The child's contract is 0 pass, 1 fail. Anything else (2 is bad argv) is
        // not a statement about the host.
        ChildEnd::Exited {
            code, stdout, ..
        } => ProbeOutcome::Indeterminate {
            reason: format!("The host probe of {exercising} exited {code}: {stdout}"),
        },
        ChildEnd::Signaled(signal) => match fault_signal(signal) {
            Some(name) => ProbeOutcome::Fail {
                summary: format!("The host probe of {exercising} crashed with {name}"),
                remediation: remediation_for(target),
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

#[cfg(test)]
mod tests {
    use super::*;

    const KINDS: [ProbeKind; 4] = [
        ProbeKind::Media,
        ProbeKind::Input,
        ProbeKind::Audio,
        ProbeKind::ApplicationGpu,
    ];

    fn exited(code: i32, remediation: Option<&str>) -> ChildEnd {
        ChildEnd::Exited {
            code,
            stdout: "x".into(),
            remediation: remediation.map(str::to_string),
        }
    }

    // ── `remediation(kind)` strings, pinned verbatim so they cannot drift ────────────

    #[test]
    fn remediation_text_is_verbatim_per_kind() {
        assert_eq!(
            remediation(ProbeKind::Media),
            "Check the render node is passed to the agent container, the driver/driver \
             volume, and the agent log for `token=\"host-probe-` lines."
        );
        assert_eq!(
            remediation(ProbeKind::Input),
            "Check /dev/uinput is passed to the container and \
             `device_cgroup_rules: ['c 13:* rmw']` is set."
        );
        assert_eq!(
            remediation(ProbeKind::Audio),
            "Check the runtime lets the agent image start the audio sidecar as a sibling \
             container: the runtime_endpoint check, and the agent logs."
        );
        assert_eq!(
            remediation(ProbeKind::ApplicationGpu),
            "Check /dev/dri permissions/groups (the dri_node_app_access check) and, on \
             NVIDIA, the driver volume (driver_volume_version check)."
        );
    }

    #[test]
    fn a_fail_with_a_child_supplied_remediation_uses_it_over_the_per_kind_default() {
        for kind in KINDS {
            let outcome =
                child_outcome(ProbeTarget::host(kind), exited(1, Some("do this instead")));
            match outcome {
                ProbeOutcome::Fail { remediation, .. } => {
                    assert_eq!(remediation, "do this instead")
                }
                other => panic!("{other:?}"),
            }
        }
    }

    #[test]
    fn a_fail_with_no_child_remediation_falls_back_to_the_per_kind_default_for_every_kind() {
        for kind in KINDS {
            let outcome = child_outcome(ProbeTarget::host(kind), exited(1, None));
            match outcome {
                ProbeOutcome::Fail {
                    remediation: got, ..
                } => assert_eq!(got, remediation(kind)),
                other => panic!("{other:?}"),
            }
        }
    }

    #[test]
    fn a_pass_ignores_a_supplied_remediation() {
        let outcome = child_outcome(
            ProbeTarget::host(ProbeKind::Media),
            exited(0, Some("ignored")),
        );
        assert!(matches!(outcome, ProbeOutcome::Pass { .. }));
    }

    #[test]
    fn an_indeterminate_exit_ignores_a_supplied_remediation() {
        let outcome = child_outcome(
            ProbeTarget::host(ProbeKind::Media),
            exited(2, Some("ignored")),
        );
        assert!(matches!(outcome, ProbeOutcome::Indeterminate { .. }));
    }

    #[test]
    fn a_signal_uses_the_per_kind_default_a_crash_is_not_the_child_speaking() {
        let outcome = child_outcome(
            ProbeTarget::host(ProbeKind::Media),
            ChildEnd::Signaled(libc::SIGSEGV),
        );
        match outcome {
            ProbeOutcome::Fail {
                remediation: got, ..
            } => {
                assert_eq!(got, remediation(ProbeKind::Media))
            }
            other => panic!("{other:?}"),
        }
    }

    /// #301: a pass counts only under the stack it was proven on; an indeterminate rerun
    /// keeps it, a fail or a `Forget` drops it.
    #[test]
    fn codec_evidence_counts_a_pass_only_under_its_own_stamp() {
        use crate::host_probe::decision::ProbeInputs;
        let inputs = |driver: &str| ProbeInputs {
            agent_image: "sha256:a".into(),
            driver: driver.into(),
            gpus: [(0, "pci-0".to_string())].into(),
            settings: "encoder=vulkan".into(),
            codecs: Default::default(),
        };
        let old = inputs("595").evidence_stamp(0);
        let new = inputs("610").evidence_stamp(0);
        let target = ProbeTarget::codec(0, ProbeCodec::H265);
        let pass = ProbeOutcome::Pass {
            summary: "ok".into(),
        };
        let mut report = ReadinessReport::default();
        let mut evidence = CodecEvidence::default();
        let at = SystemTime::UNIX_EPOCH;

        evidence.note(target, &pass, old.clone());
        record(&mut report, target, pass.clone(), at);
        assert!(evidence.proven(&report, 0, ProbeCodec::H265, old.as_ref()));
        assert!(!evidence.proven(&report, 0, ProbeCodec::H265, new.as_ref()));
        assert!(!evidence.proven(&report, 0, ProbeCodec::H265, None));
        assert!(!evidence.proven(&report, 0, ProbeCodec::Av1, old.as_ref()));

        let indeterminate = ProbeOutcome::Indeterminate {
            reason: "pre-empted".into(),
        };
        evidence.note(target, &indeterminate, new.clone());
        record(&mut report, target, indeterminate, at);
        assert!(evidence.proven(&report, 0, ProbeCodec::H265, old.as_ref()));

        // A pass with no stamp is never evidence.
        evidence.note(target, &pass, None);
        assert!(!evidence.proven(&report, 0, ProbeCodec::H265, old.as_ref()));

        evidence.note(target, &pass, new.clone());
        assert!(evidence.proven(&report, 0, ProbeCodec::H265, new.as_ref()));
        evidence.forget(target);
        assert!(!evidence.proven(&report, 0, ProbeCodec::H265, new.as_ref()));
    }
}
