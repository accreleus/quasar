//! The application-GPU host probe: a disposable sibling container, given the same GPU
//! access as a session's application container, opens the GPU through EGL.

use std::time::{Duration, Instant};

use crate::container_ownership::PROBE_NAME_PREFIX;
use crate::nvidia_volume::{parse_egl_device_open, EglRuntime};
use crate::runtime::{DiagnosticHelper, ErrorKind, GpuProbeRun, RuntimeClient};

use super::container::{ContainerProbeEnd, Observed};
use super::outcome::{fault_signal, remediation, wording, ProbeOutcome};
use super::ProbeTarget;

/// Run one probe container to completion, or to `deadline`/pre-emption. Busy means an
/// earlier probe of this kind is unreconciled: never retried under a new identity here —
/// the caller reconciles and tries again later.
pub fn run(
    api: &RuntimeClient,
    image: &str,
    run: GpuProbeRun,
    nonce: &str,
    deadline: Duration,
    preempted: &(dyn Fn() -> bool + Sync),
) -> ContainerProbeEnd {
    if let Err(error) = api.recover_diagnostics().wait() {
        return ContainerProbeEnd {
            observed: Observed::RuntimeError(format!("recovering an earlier host probe: {error}")),
            reconciled: false,
        };
    }
    if preempted() {
        return ContainerProbeEnd {
            observed: Observed::Preempted,
            reconciled: true,
        };
    }

    let helper = DiagnosticHelper {
        operation: format!("app-gpu-probe-{nonce}"),
        name: format!("{PROBE_NAME_PREFIX}app-gpu-{nonce}"),
        image: image.to_string(),
    };
    let id = match api.run_gpu_probe(helper, run).wait() {
        Ok(id) => id,
        Err(error) if error.kind == ErrorKind::Busy => {
            return ContainerProbeEnd {
                observed: Observed::Busy,
                reconciled: false,
            };
        }
        Err(error) => {
            return ContainerProbeEnd {
                observed: Observed::RuntimeError(format!(
                    "starting the host probe container: {error}"
                )),
                reconciled: false,
            };
        }
    };

    let deadline_at = Instant::now() + deadline;
    let mut reconciled = true;
    let observed = match api
        .observe_gpu_probe(id.clone())
        .wait_with_cancel(|| preempted() || Instant::now() >= deadline_at)
    {
        Ok(result) => Observed::Exited(result),
        Err(error) => {
            let cause = if preempted() {
                Observed::Preempted
            } else if Instant::now() >= deadline_at {
                Observed::Deadline
            } else {
                Observed::RuntimeError(format!("observing the host probe container: {error}"))
            };
            // The container may still be running; an explicit stop always follows a lost
            // or cut-short observation.
            if api.stop_gpu_probe(id.clone()).wait().is_err() {
                reconciled = false;
            }
            cause
        }
    };
    if api.cleanup_gpu_probe(id).wait().is_err() {
        reconciled = false;
    }
    ContainerProbeEnd {
        observed,
        reconciled,
    }
}

/// Trim a stderr tail to a bounded operator-readable snippet.
fn tail(stderr: &str) -> String {
    let trimmed = stderr.trim();
    let count = trimmed.chars().count();
    if count > 200 {
        let kept: String = trimmed.chars().skip(count - 200).collect();
        format!("...{kept}")
    } else {
        trimmed.to_string()
    }
}

pub fn outcome(target: ProbeTarget, end: &ContainerProbeEnd) -> ProbeOutcome {
    let (passed, failed, exercising) = wording(target);
    match &end.observed {
        Observed::Exited(result) => match result.exit_code {
            Some(0) => match parse_egl_device_open(&result.stdout) {
                EglRuntime::Ok { loaded } => ProbeOutcome::Pass {
                    summary: format!("{passed}: {loaded}"),
                },
                EglRuntime::Broken { detail, .. } => ProbeOutcome::Fail {
                    summary: format!("{failed}: {detail}"),
                    remediation: remediation(target.kind),
                },
                EglRuntime::Indeterminate { detail } => ProbeOutcome::Indeterminate { reason: detail },
                EglRuntime::Unknown => ProbeOutcome::Indeterminate {
                    reason: format!("The host probe of {exercising} produced no EGL verdict"),
                },
            },
            // timeout(1)'s own exit for "the command itself timed out".
            Some(124) => ProbeOutcome::Indeterminate {
                reason: format!("The host probe of {exercising} did not finish within its own in-container timeout"),
            },
            // Unknown subcommand / bad usage says nothing about the host.
            Some(2) => ProbeOutcome::Indeterminate {
                reason: format!("The host probe of {exercising} exited 2 (unexpected usage)"),
            },
            Some(code) if code >= 128 => match fault_signal((code - 128) as i32) {
                Some(name) => ProbeOutcome::Fail {
                    summary: format!("The host probe of {exercising} crashed with {name}"),
                    remediation: remediation(target.kind),
                },
                None => ProbeOutcome::Indeterminate {
                    reason: format!("The host probe of {exercising} exited {code}"),
                },
            },
            Some(code) => ProbeOutcome::Indeterminate {
                reason: format!(
                    "The host probe of {exercising} exited {code}: {}",
                    tail(&result.stderr)
                ),
            },
            None => ProbeOutcome::Indeterminate {
                reason: format!(
                    "The host probe of {exercising} produced no exit code: {}",
                    tail(&result.stderr)
                ),
            },
        },
        Observed::Deadline => ProbeOutcome::Indeterminate {
            reason: format!("The host probe of {exercising} did not finish before its deadline"),
        },
        Observed::Preempted => ProbeOutcome::Indeterminate {
            reason: format!(
                "A session launch took priority over the host probe of {exercising}; it will run again"
            ),
        },
        Observed::Busy => ProbeOutcome::Indeterminate {
            reason: "An earlier host probe container is still being cleaned up".into(),
        },
        Observed::RuntimeError(detail) => ProbeOutcome::Indeterminate {
            reason: format!("The host probe of {exercising} could not run: {detail}"),
        },
        Observed::SocketReady | Observed::SocketTimeout => ProbeOutcome::Indeterminate {
            reason: format!("The host probe of {exercising} reported an audio result"),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::super::ProbeKind;
    use super::*;
    use crate::runtime::HelperResult;
    use ProbeKind::ApplicationGpu;

    fn end(observed: Observed, reconciled: bool) -> ContainerProbeEnd {
        ContainerProbeEnd {
            observed,
            reconciled,
        }
    }

    fn exited(exit_code: Option<i64>, stdout: &str, stderr: &str) -> Observed {
        Observed::Exited(HelperResult {
            exit_code,
            stdout: stdout.into(),
            stderr: stderr.into(),
        })
    }

    fn target() -> ProbeTarget {
        ProbeTarget::gpu(ApplicationGpu, 0)
    }

    #[test]
    fn a_clean_open_passes_naming_the_node() {
        let e = end(
            exited(
                Some(0),
                "LOADED=/usr/lib64/libEGL.so.1\nEXTENSIONS=EGL_EXT_device_enumeration\nDEVICES=1\nRENDER_NODES=/dev/dri/renderD128\nOPENED=/dev/dri/renderD128\n",
                "",
            ),
            true,
        );
        match outcome(target(), &e) {
            ProbeOutcome::Pass { summary } => assert!(summary.contains("renderD128"), "{summary}"),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn a_broken_egl_stack_fails_with_remediation() {
        let e = end(
            exited(
                Some(0),
                "LOADED=/usr/lib64/libEGL.so.1\nEXTENSIONS=EGL_EXT_device_enumeration\nDEVICES=1\nRENDER_NODES=/dev/dri/renderD128\nDEVICE_ERROR=eglInitialize failed (0x3003)\n",
                "",
            ),
            true,
        );
        match outcome(target(), &e) {
            ProbeOutcome::Fail {
                summary,
                remediation,
            } => {
                assert!(summary.contains("0x3003"), "{summary}");
                assert!(!remediation.is_empty());
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn a_software_only_device_fails() {
        let e = end(
            exited(
                Some(0),
                "LOADED=/usr/lib64/libEGL.so.1\nEXTENSIONS=EGL_EXT_device_enumeration\nDEVICES=1\nRENDER_NODES=\nDEVICE_ERROR=no hardware EGL device (only software rendering is available)\n",
                "",
            ),
            true,
        );
        assert!(matches!(outcome(target(), &e), ProbeOutcome::Fail { .. }));
    }

    #[test]
    fn exit_124_is_indeterminate() {
        let e = end(exited(Some(124), "", ""), true);
        assert!(matches!(
            outcome(target(), &e),
            ProbeOutcome::Indeterminate { .. }
        ));
    }

    #[test]
    fn a_segv_fails_naming_the_signal() {
        let e = end(exited(Some(139), "", ""), true);
        match outcome(target(), &e) {
            ProbeOutcome::Fail { summary, .. } => assert!(summary.contains("SIGSEGV"), "{summary}"),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn exit_none_is_indeterminate_with_a_bounded_stderr_tail() {
        let e = end(exited(None, "", &"x".repeat(500)), true);
        match outcome(target(), &e) {
            ProbeOutcome::Indeterminate { reason } => assert!(reason.len() < 500),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn deadline_preempted_busy_and_runtime_error_are_all_indeterminate() {
        for observed in [
            Observed::Deadline,
            Observed::Preempted,
            Observed::Busy,
            Observed::RuntimeError("engine unreachable".into()),
        ] {
            let e = end(observed, false);
            assert!(
                matches!(outcome(target(), &e), ProbeOutcome::Indeterminate { .. }),
                "{e:?}"
            );
        }
    }
}
