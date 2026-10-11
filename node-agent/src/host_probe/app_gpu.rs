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

/// `vulkan_requested`: the probe asked the self-test for its Vulkan step (an NVIDIA GPU).
pub fn outcome(
    target: ProbeTarget,
    vulkan_requested: bool,
    end: &ContainerProbeEnd,
) -> ProbeOutcome {
    let (passed, failed, exercising) = wording(target);
    match &end.observed {
        Observed::Exited(result) => match result.exit_code {
            Some(0) => match parse_egl_device_open(&result.stdout) {
                EglRuntime::Ok { loaded } => match parse_vulkan_probe(&result.stdout, vulkan_requested) {
                    VulkanProbe::NotRun => ProbeOutcome::Pass {
                        summary: format!("{passed}: {loaded}"),
                    },
                    VulkanProbe::Ok { driver } => ProbeOutcome::Pass {
                        summary: format!("{passed}: {loaded}; Vulkan: NVIDIA {driver}"),
                    },
                    VulkanProbe::Broken { detail } => ProbeOutcome::Fail {
                        summary: format!(
                            "{failed}: EGL opens it but Vulkan does not: {detail}, and the container was not given /dev/nvidia-modeset"
                        ),
                        remediation: vulkan_remediation(),
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
        match outcome(target(), false, &e) {
            ProbeOutcome::Pass { summary } => assert!(summary.contains("renderD128"), "{summary}"),
            other => panic!("{other:?}"),
        }
    }

    const EGL_OPENED: &str = "LOADED=/usr/lib64/libEGL.so.1\nEXTENSIONS=EGL_EXT_device_enumeration\nDEVICES=1\nRENDER_NODES=/dev/dri/renderD128\nOPENED=/dev/dri/renderD128\n";
    const NVIDIA_SUMMARY: &str = "Devices:\n========\nGPU0:\n\tvendorID = 0x10de\n\tdeviceID = 0x1f08\n\tdriverName = NVIDIA\n\tdriverInfo = 615.78.08\n";
    const LLVMPIPE_SUMMARY: &str = "Devices:\n========\nGPU0:\n\tvendorID = 0x10005\n\tdeviceID = 0x0\n\tdriverName = llvmpipe\n\tdriverInfo = Mesa 25.3.6\n";

    #[derive(Debug, Clone, Copy, PartialEq)]
    enum Verdict {
        Pass,
        Fail,
        Unknown,
    }
    use Verdict::{Fail, Pass, Unknown};

    fn node(absent: bool) -> &'static str {
        if absent {
            "absent"
        } else {
            "present"
        }
    }

    /// The probe's stdout for one `vulkaninfo` run, built by the self-test's own classifier.
    fn selftest_stdout(
        modeset_absent: bool,
        run: Option<(&str, Option<i32>, Option<i32>)>,
    ) -> String {
        let lines = crate::nvidia_volume::vulkan_selftest_lines(
            modeset_absent,
            run.map(|(out, code, signal)| (out.to_string(), code, signal)),
        );
        format!(
            "{EGL_OPENED}NVIDIA_MODESET_NODE={}\n{}\n",
            node(modeset_absent),
            lines.join("\n")
        )
    }

    fn verdict(stdout: &str, vulkan_requested: bool) -> Verdict {
        match outcome(
            target(),
            vulkan_requested,
            &end(exited(Some(0), stdout, ""), true),
        ) {
            ProbeOutcome::Pass { .. } => Pass,
            ProbeOutcome::Fail { .. } => Fail,
            ProbeOutcome::Indeterminate { .. } => Unknown,
            other => panic!("{other:?}"),
        }
    }

    type Run = Option<(&'static str, Option<i32>, Option<i32>)>;
    type Row = (&'static str, Run, Verdict, Verdict);

    /// The whole table: `(what vulkaninfo did, verdict with the node absent, with it present)`.
    #[test]
    fn the_vulkan_step_gives_each_verdict_in_both_node_states() {
        let (segv, kill) = (Some(libc::SIGSEGV), Some(libc::SIGKILL));
        let rows: [Row; 12] = [
            (
                "NVIDIA listed, exit 0",
                Some((NVIDIA_SUMMARY, Some(0), None)),
                Pass,
                Pass,
            ),
            (
                "NVIDIA listed, exit 1",
                Some((NVIDIA_SUMMARY, Some(1), None)),
                Pass,
                Pass,
            ),
            (
                "NVIDIA listed, then SIGSEGV",
                Some((NVIDIA_SUMMARY, None, segv)),
                Pass,
                Pass,
            ),
            (
                "exit 0, none listed",
                Some((LLVMPIPE_SUMMARY, Some(0), None)),
                Fail,
                Unknown,
            ),
            (
                "exit 1, none listed",
                Some(("", Some(1), None)),
                Fail,
                Unknown,
            ),
            (
                "SIGSEGV, none listed",
                Some(("", None, segv)),
                Fail,
                Unknown,
            ),
            ("exit 126", Some(("", Some(126), None)), Unknown, Unknown),
            ("exit 127", Some(("", Some(127), None)), Unknown, Unknown),
            ("SIGKILL", Some(("", None, kill)), Unknown, Unknown),
            (
                "SIGKILL after NVIDIA listed",
                Some((NVIDIA_SUMMARY, None, kill)),
                Pass,
                Pass,
            ),
            ("no exit code", Some(("", None, None)), Unknown, Unknown),
            ("did not run, or past its deadline", None, Unknown, Unknown),
        ];
        for (name, run, when_absent, when_present) in rows {
            for (absent, want) in [(true, when_absent), (false, when_present)] {
                let stdout = selftest_stdout(absent, run);
                assert_eq!(
                    verdict(&stdout, true),
                    want,
                    "{name}, node {}: {stdout}",
                    node(absent)
                );
            }
        }
        for absent in [true, false] {
            // The marker is there but the result line is not.
            let marker_only = format!("{EGL_OPENED}NVIDIA_MODESET_NODE={}\n", node(absent));
            assert_eq!(
                verdict(&marker_only, true),
                Unknown,
                "marker only, node {}",
                node(absent)
            );
        }
        // No Vulkan line at all: lost when asked for, an EGL-only pass when not.
        assert_eq!(verdict(EGL_OPENED, true), Unknown);
        assert_eq!(verdict(EGL_OPENED, false), Pass);
    }

    #[test]
    fn a_vulkan_fail_says_what_was_observed_and_the_fix() {
        let stdout = selftest_stdout(true, Some(("", None, Some(libc::SIGSEGV))));
        match outcome(target(), true, &end(exited(Some(0), &stdout, ""), true)) {
            ProbeOutcome::Fail {
                summary,
                remediation,
            } => {
                assert!(
                    summary.contains("Vulkan does not") && summary.contains("SIGSEGV"),
                    "{summary}"
                );
                assert!(summary.contains("/dev/nvidia-modeset"), "{summary}");
                assert!(
                    remediation.contains("were not given /dev/nvidia-modeset"),
                    "{remediation}"
                );
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
    fn an_indeterminate_vulkan_step_keeps_what_was_seen() {
        let stdout = selftest_stdout(false, Some(("", None, Some(libc::SIGSEGV))));
        match outcome(target(), true, &end(exited(Some(0), &stdout, ""), true)) {
            ProbeOutcome::Indeterminate { reason } => {
                assert!(
                    reason.contains("SIGSEGV") && reason.contains("/dev/nvidia-modeset is present"),
                    "{reason}"
                )
            }
            other => panic!("{other:?}"),
        }
        let stdout = selftest_stdout(false, Some((NVIDIA_SUMMARY, Some(1), None)));
        match outcome(target(), true, &end(exited(Some(0), &stdout, ""), true)) {
            ProbeOutcome::Pass { summary } => {
                assert!(
                    summary.contains("615.78.08") && summary.contains("then exited 1"),
                    "{summary}"
                )
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn a_failing_egl_open_wins_over_the_vulkan_step() {
        let e = end(
            exited(Some(0), "LOADED=/usr/lib64/libEGL.so.1\nEXTENSIONS=EGL_EXT_device_enumeration\nDEVICE_ERROR=eglInitialize failed (0x3003)\nVULKAN_NVIDIA=615.78.08\n", ""),
            true,
        );
        match outcome(target(), false, &e) {
            ProbeOutcome::Fail { summary, .. } => assert!(summary.contains("0x3003"), "{summary}"),
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
        match outcome(target(), false, &e) {
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
        assert!(matches!(
            outcome(target(), false, &e),
            ProbeOutcome::Fail { .. }
        ));
    }

    #[test]
    fn exit_124_is_indeterminate() {
        let e = end(exited(Some(124), "", ""), true);
        assert!(matches!(
            outcome(target(), false, &e),
            ProbeOutcome::Indeterminate { .. }
        ));
    }

    #[test]
    fn a_segv_fails_naming_the_signal() {
        let e = end(exited(Some(139), "", ""), true);
        match outcome(target(), false, &e) {
            ProbeOutcome::Fail { summary, .. } => assert!(summary.contains("SIGSEGV"), "{summary}"),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn exit_none_is_indeterminate_with_a_bounded_stderr_tail() {
        let e = end(exited(None, "", &"x".repeat(500)), true);
        match outcome(target(), false, &e) {
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
                matches!(
                    outcome(target(), false, &e),
                    ProbeOutcome::Indeterminate { .. }
                ),
                "{e:?}"
            );
        }
    }
}
