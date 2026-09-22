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
    /// A codec probe whose encoder could not open on the device (#311): the GPU has no
    /// encoder for that codec. Definitive like `Fail` (the codec leaves the GPU codec
    /// set) but a hardware fact, so the wire status is `unsupported` and there is no
    /// remediation (protocol/agent-api.md `readiness`).
    Unsupported {
        summary: String,
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

/// [`remediation`] for the target's kind, except a codec probe. A codec probe's fail is
/// a fault past the encoder opening (a missing encoder is `Unsupported`), so it points at
/// the driver.
pub fn remediation_for(target: ProbeTarget) -> String {
    match target.codec {
        Some(codec) => {
            let codec = codec.as_str();
            format!(
                "Sessions on this GPU use another codec until {codec} passes. Check the \
                 driver and the agent log for `token=\"host-probe-` lines."
            )
        }
        None => remediation(target.kind),
    }
}

/// The retained verdict of the codec probe for (`gpu`, `codec`): `Some(true)` pass,
/// `Some(false)` fail or unsupported, `None` when no definitive run is held (absent,
/// indeterminate, skip).
/// An indeterminate run never replaces a held verdict ([`record`]), so this is the last
/// definitive one.
pub fn codec_probe_verdict(report: &ReadinessReport, gpu: i32, codec: ProbeCodec) -> Option<bool> {
    let check = report.retained(&ProbeTarget::codec(gpu, codec).check_id())?;
    match check.status.as_str() {
        crate::readiness::PASS => Some(true),
        crate::readiness::FAIL | crate::readiness::UNSUPPORTED => Some(false),
        _ => None,
    }
}

/// Which stack each held codec-probe pass was proven under (#301), kept beside the
/// report and fed by the same updates. Agent-side only: the check on the wire is unchanged.
#[derive(Debug, Default)]
pub struct CodecEvidence(BTreeMap<ProbeTarget, EvidenceStamp>);

impl CodecEvidence {
    /// Mirrors [`record`]: a pass takes the run's stamp, a fail, unsupported or skip drops
    /// it, and an indeterminate run leaves the held verdict — and its stamp — standing.
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
        // 4 is a codec probe whose encoder could not open (#311). Only a codec probe
        // answers it; from the H.264 media probe it would be a fault, so it reads as one.
        ChildEnd::Exited {
            code: crate::session::probe_media::UNSUPPORTED_EXIT,
            stdout,
            ..
        } => match target.codec {
            Some(_) => ProbeOutcome::Unsupported {
                summary: format!("{failed}: {stdout}"),
            },
            None => ProbeOutcome::Fail {
                summary: format!("{failed}: {stdout}"),
                remediation: remediation_for(target),
            },
        },
        // The child's contract is 0 pass, 1 fail, 4 unsupported. Anything else (2 is bad
        // argv) is not a statement about the host.
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

/// Indeterminate never replaces a held pass, fail, unsupported or skip.
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
        ProbeOutcome::Unsupported { summary } => report.retain(
            crate::messages::ReadinessCheck {
                id,
                status: crate::readiness::UNSUPPORTED.into(),
                summary,
                remediation: String::new(),
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
                    || status == crate::readiness::UNSUPPORTED
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

    /// #311: exit 4 is `Unsupported` for a codec probe, carrying the evidence; from the
    /// H.264 media probe it is a `Fail`.
    #[test]
    fn exit_four_is_unsupported_for_a_codec_probe_and_a_fail_for_the_media_probe() {
        let av1 = ProbeTarget::codec(1, ProbeCodec::Av1);
        assert_eq!(
            child_outcome(av1, exited(4, Some("ignored"))),
            ProbeOutcome::Unsupported {
                summary: "GPU 1 does not encode av1; sessions will not use av1 on this GPU: x"
                    .into()
            }
        );
        let media = ProbeTarget::gpu(ProbeKind::Media, 1);
        assert!(matches!(
            child_outcome(media, exited(4, None)),
            ProbeOutcome::Fail { .. }
        ));
    }

    /// #311: `unsupported` is definitive. It is retained, an indeterminate rerun does not
    /// replace it, the codec verdict is `Some(false)` and the evidence stamp is dropped.
    #[test]
    fn unsupported_is_retained_like_a_fail_and_drops_the_evidence_stamp() {
        use crate::host_probe::decision::ProbeInputs;
        let stamp = ProbeInputs {
            agent_image: "sha256:a".into(),
            driver: "610".into(),
            gpus: [(0, "pci-0".to_string())].into(),
            settings: "encoder=vulkan".into(),
            codecs: Default::default(),
        }
        .evidence_stamp(0);
        let target = ProbeTarget::codec(0, ProbeCodec::Av1);
        let mut report = ReadinessReport::default();
        let mut evidence = CodecEvidence::default();
        let at = SystemTime::UNIX_EPOCH;
        let pass = ProbeOutcome::Pass {
            summary: "ok".into(),
        };
        evidence.note(target, &pass, stamp.clone());
        record(&mut report, target, pass, at);
        assert!(evidence.proven(&report, 0, ProbeCodec::Av1, stamp.as_ref()));

        let unsupported = child_outcome(target, exited(4, None));
        evidence.note(target, &unsupported, stamp.clone());
        record(&mut report, target, unsupported, at);
        let check = report.retained("media_probe_gpu0_av1").unwrap();
        assert_eq!(check.status, crate::readiness::UNSUPPORTED);
        assert_eq!(check.blocks, None);
        assert_eq!(
            check.remediation, "",
            "nothing to fix: the contract wants it empty"
        );
        assert_eq!(
            codec_probe_verdict(&report, 0, ProbeCodec::Av1),
            Some(false)
        );
        assert!(!evidence.proven(&report, 0, ProbeCodec::Av1, stamp.as_ref()));

        let indeterminate = ProbeOutcome::Indeterminate {
            reason: "pre-empted".into(),
        };
        evidence.note(target, &indeterminate, stamp.clone());
        record(&mut report, target, indeterminate, at);
        let check = report.retained("media_probe_gpu0_av1").unwrap();
        assert_eq!(check.status, crate::readiness::UNSUPPORTED);
        assert_eq!(
            codec_probe_verdict(&report, 0, ProbeCodec::Av1),
            Some(false)
        );
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
