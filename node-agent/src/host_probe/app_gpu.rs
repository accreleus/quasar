//! The application-GPU host probe: a disposable sibling container, given the same GPU
//! access as a session's application container, opens the GPU through EGL.

use std::time::{Duration, Instant};

use crate::container_ownership::PROBE_NAME_PREFIX;
use crate::nvidia_volume::{parse_egl_device_open, parse_vulkan_probe, EglRuntime, VulkanProbe};
use crate::runtime::{DiagnosticHelper, ErrorKind, GpuProbeRun, RuntimeClient};

use super::container::{ContainerProbeEnd, Observed};
use super::outcome::{fault_signal, remediation, vulkan_remediation, wording, ProbeOutcome};
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
                EglRuntime::Ok { loaded } => match parse_vulkan_probe(&result.stdout) {
                    VulkanProbe::NotRun => ProbeOutcome::Pass {
                        summary: format!("{passed}: {loaded}"),
                    },
                    VulkanProbe::Ok { driver } => ProbeOutcome::Pass {
                        summary: format!("{passed}: {loaded}; Vulkan: NVIDIA {driver}"),
                    },
                    VulkanProbe::Broken {
                        detail,
                        modeset_absent,
                    } => ProbeOutcome::Fail {
                        summary: format!(
                            "{failed}: EGL opens it but Vulkan does not: {detail}{}",
                            if modeset_absent {
                                " (the container was not given /dev/nvidia-modeset)"
                            } else {
                                ""
                            }
                        ),
                        remediation: vulkan_remediation(modeset_absent),
                    },
                    VulkanProbe::Indeterminate { detail } => ProbeOutcome::Indeterminate {
                        reason: format!("The Vulkan step of the host probe of {exercising}: {detail}"),
                    },
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

    const EGL_OPENED: &str = "LOADED=/usr/lib64/libEGL.so.1\nEXTENSIONS=EGL_EXT_device_enumeration\nDEVICES=1\nRENDER_NODES=/dev/dri/renderD128\nOPENED=/dev/dri/renderD128\n";

    fn with_vulkan(lines: &str) -> ContainerProbeEnd {
        end(exited(Some(0), &format!("{EGL_OPENED}{lines}"), ""), true)
    }

    #[test]
    fn a_working_vulkan_device_passes_naming_the_driver() {
        match outcome(
            target(),
            &with_vulkan("NVIDIA_MODESET_NODE=present\nVULKAN_NVIDIA=615.78.08\n"),
        ) {
            ProbeOutcome::Pass { summary } => {
                assert!(summary.contains("Vulkan: NVIDIA 615.78.08"), "{summary}")
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn egl_open_with_a_crashing_vulkan_loader_and_no_modeset_node_fails_with_the_cdi_remediation() {
        let e = with_vulkan(
            "NVIDIA_MODESET_NODE=absent\nVULKAN_ERROR=vulkaninfo crashed with SIGSEGV\n",
        );
        match outcome(target(), &e) {
            ProbeOutcome::Fail {
                summary,
                remediation,
            } => {
                assert!(
                    summary.contains("Vulkan does not") && summary.contains("SIGSEGV"),
                    "{summary}"
                );
                assert!(summary.contains("/dev/nvidia-modeset"), "{summary}");
                assert!(remediation.contains("nvidia-modprobe -m"), "{remediation}");
                assert!(
                    remediation.contains("nvidia-ctk cdi generate"),
                    "{remediation}"
                );
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn a_vulkan_failure_with_the_modeset_node_present_gets_the_generic_remediation() {
        let e = with_vulkan(
            "NVIDIA_MODESET_NODE=present\nVULKAN_ERROR=the Vulkan loader lists no NVIDIA device\n",
        );
        match outcome(target(), &e) {
            ProbeOutcome::Fail {
                summary,
                remediation,
            } => {
                assert!(!summary.contains("/dev/nvidia-modeset"), "{summary}");
                assert!(!remediation.contains("nvidia-modprobe"), "{remediation}");
                assert!(
                    remediation.contains("vulkaninfo --summary"),
                    "{remediation}"
                );
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn an_unavailable_vulkan_step_is_indeterminate_never_a_fail() {
        for lines in [
            "NVIDIA_MODESET_NODE=absent\nVULKAN_UNAVAILABLE=vulkaninfo did not run (absent, or past its deadline)\n",
            "NVIDIA_MODESET_NODE=absent\n",
        ] {
            assert!(
                matches!(outcome(target(), &with_vulkan(lines)), ProbeOutcome::Indeterminate { .. }),
                "{lines}"
            );
        }
    }

    #[test]
    fn a_vulkaninfo_that_was_killed_not_crashed_is_indeterminate_through_to_the_outcome() {
        use crate::nvidia_volume::VulkanStep;
        // The self-test's own output for each signal, as the probe reads it.
        let stdout = |signal: i32| {
            let lines = crate::nvidia_volume::vulkan_selftest_lines(
                VulkanStep::Own,
                true,
                true,
                Some((String::new(), None, Some(signal))),
            );
            format!(
                "{EGL_OPENED}NVIDIA_MODESET_NODE=absent\n{}\n",
                lines.join("\n")
            )
        };
        for signal in [libc::SIGKILL, libc::SIGTERM] {
            let e = end(exited(Some(0), &stdout(signal), ""), true);
            assert!(
                matches!(outcome(target(), &e), ProbeOutcome::Indeterminate { .. }),
                "{signal}"
            );
        }
        let e = end(exited(Some(0), &stdout(libc::SIGSEGV), ""), true);
        assert!(matches!(outcome(target(), &e), ProbeOutcome::Fail { .. }));
    }

    #[test]
    fn a_failing_egl_open_wins_over_the_vulkan_step() {
        let e = end(
            exited(Some(0), "LOADED=/usr/lib64/libEGL.so.1\nEXTENSIONS=EGL_EXT_device_enumeration\nDEVICE_ERROR=eglInitialize failed (0x3003)\nVULKAN_NVIDIA=615.78.08\n", ""),
            true,
        );
        match outcome(target(), &e) {
            ProbeOutcome::Fail { summary, .. } => assert!(summary.contains("0x3003"), "{summary}"),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn a_probe_without_the_vulkan_step_passes_on_egl_alone() {
        assert!(matches!(
            outcome(target(), &with_vulkan("")),
            ProbeOutcome::Pass { .. }
        ));
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
