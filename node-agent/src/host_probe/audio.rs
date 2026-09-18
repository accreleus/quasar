//! The audio host probe: the existing sidecar profile started under a probe identity,
//! proven by its socket appearing, then stopped and removed.

use crate::runtime::{AudioRun, DiagnosticHelper, ErrorKind, RuntimeClient};

use super::container::{ContainerProbeEnd, Observed};
use super::outcome::{remediation, wording, ProbeOutcome};
use super::{ProbeKind, ProbeTarget};

/// Run one probe sidecar to a socket-ready/timeout verdict, always stopping and
/// cleaning up whatever it created. Recovery of a stale prior sidecar runs inside this
/// same single flight, before any new create.
pub fn run(
    api: &RuntimeClient,
    image: &str,
    runtime_dir: &str,
    nonce: &str,
    preempted: &(dyn Fn() -> bool + Sync),
) -> ContainerProbeEnd {
    if let Err(error) = api.recover_audio_sidecars().wait() {
        return ContainerProbeEnd {
            observed: Observed::RuntimeError(format!("recovering a stale audio sidecar: {error}")),
            reconciled: false,
        };
    }
    if preempted() {
        return ContainerProbeEnd {
            observed: Observed::Preempted,
            reconciled: true,
        };
    }

    let id = format!("probe-{nonce}");
    let socket_dir = crate::session::audio::pulse_socket_dir(runtime_dir, &id);
    let helper = DiagnosticHelper {
        operation: format!("audio-probe-{nonce}"),
        name: crate::session::audio::pulse_container_name(&id),
        image: image.to_string(),
    };
    let request = AudioRun {
        socket_dir: socket_dir.clone(),
        entrypoint: vec!["pulseaudio".into()],
        command: crate::session::audio::pulse_command(&socket_dir.to_string_lossy()),
    };
    let handle = match api.run_audio_sidecar(helper, request).wait() {
        Ok(handle) => handle,
        Err(error) if error.kind == ErrorKind::Busy => {
            return ContainerProbeEnd {
                observed: Observed::Busy,
                reconciled: false,
            };
        }
        Err(error) => {
            return ContainerProbeEnd {
                observed: Observed::RuntimeError(format!(
                    "starting the audio host probe sidecar: {error}"
                )),
                reconciled: false,
            };
        }
    };

    // The wait is not itself pre-emptible (≤2 s is an accepted delay); a launch that
    // arrived during it is honoured right after.
    let ready = crate::session::audio::wait_for_socket(&socket_dir.join("native"));
    let observed = if ready {
        Observed::SocketReady
    } else if preempted() {
        Observed::Preempted
    } else {
        Observed::SocketTimeout
    };

    let mut reconciled = true;
    if api.stop_audio_sidecar(handle.clone()).wait().is_err() {
        reconciled = false;
    }
    if api.cleanup_audio_sidecar(handle).wait().is_err() {
        reconciled = false;
    }
    ContainerProbeEnd {
        observed,
        reconciled,
    }
}

pub fn outcome(end: &ContainerProbeEnd) -> ProbeOutcome {
    let (passed, failed, exercising) = wording(ProbeTarget::host(ProbeKind::Audio));
    match &end.observed {
        Observed::SocketReady => ProbeOutcome::Pass {
            summary: format!("{passed}: its socket appeared"),
        },
        Observed::SocketTimeout => ProbeOutcome::Fail {
            summary: format!(
                "{failed}: its socket did not appear within {} s",
                crate::session::audio::PULSE_WAIT_TOTAL.as_secs()
            ),
            remediation: remediation(ProbeKind::Audio),
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
        Observed::Deadline | Observed::Exited(_) => ProbeOutcome::Indeterminate {
            reason: format!("The host probe of {exercising} reported a result that is not an audio result"),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn end(observed: Observed, reconciled: bool) -> ContainerProbeEnd {
        ContainerProbeEnd {
            observed,
            reconciled,
        }
    }

    #[test]
    fn socket_ready_passes() {
        assert!(matches!(
            outcome(&end(Observed::SocketReady, true)),
            ProbeOutcome::Pass { .. }
        ));
    }

    #[test]
    fn socket_timeout_fails_with_remediation() {
        match outcome(&end(Observed::SocketTimeout, true)) {
            ProbeOutcome::Fail {
                summary,
                remediation,
            } => {
                assert!(summary.contains("2 s"), "{summary}");
                assert!(!remediation.is_empty());
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn preempted_busy_and_runtime_error_are_indeterminate() {
        for observed in [
            Observed::Preempted,
            Observed::Busy,
            Observed::RuntimeError("engine unreachable".into()),
        ] {
            assert!(matches!(
                outcome(&end(observed, false)),
                ProbeOutcome::Indeterminate { .. }
            ));
        }
    }
}
