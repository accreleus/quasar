//! Which host probes could explain a failed launch (spec #252 "When probes run").
//! A wrong answer only costs a missed or a needless re-run: results stay advisory.

use std::collections::BTreeSet;

use super::ProbeKind;

/// `reason` is the runner's failure text; its stage prefixes are the emit sites in
/// `session/runner.rs`. `app_failed` is an application that exited early or never
/// presented.
pub fn explains(reason: &str, app_failed: bool) -> BTreeSet<ProbeKind> {
    use ProbeKind::*;
    const STAGES: &[(&str, &[ProbeKind])] = &[
        ("codec/encoder resolution", &[Media]),
        ("create shared CUDA context", &[Media]),
        ("build source pipeline", &[Media]),
        ("start source pipeline", &[Media]),
        ("capture Vulkan producer contexts", &[Media]),
        ("build encode pipeline", &[Media]),
        ("encode set PLAYING", &[Media]),
        ("prepare session resources", &[Input, Audio]),
        ("audio required but unavailable", &[Audio]),
        ("container launch failed", &[ApplicationGpu]),
    ];
    if app_failed {
        return [ApplicationGpu].into();
    }
    if crate::session::vulkan_fault::reason_is_device_open_failed(reason) {
        return [Media].into();
    }
    STAGES
        .iter()
        .find(|(prefix, _)| reason.starts_with(prefix))
        .map(|(_, kinds)| kinds.iter().copied().collect())
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;
    use ProbeKind::*;

    #[test]
    fn a_failure_is_explained_by_the_probe_of_the_path_that_failed() {
        let cases: &[(&str, bool, &[ProbeKind])] = &[
            (
                "build source pipeline: waylanddisplaysrc not found",
                false,
                &[Media],
            ),
            ("codec/encoder resolution: no h264 encoder", false, &[Media]),
            ("encode set PLAYING: state change failed", false, &[Media]),
            (
                "prepare session resources: open /dev/uinput",
                false,
                &[Input, Audio],
            ),
            (
                "audio required but unavailable: socket timeout",
                false,
                &[Audio],
            ),
            (
                "container launch failed: no such device",
                false,
                &[ApplicationGpu],
            ),
            (
                "the application exited 1 before presenting",
                true,
                &[ApplicationGpu],
            ),
        ];
        for (reason, app_failed, kinds) in cases {
            assert_eq!(
                explains(reason, *app_failed),
                kinds.iter().copied().collect(),
                "{reason}"
            );
        }
    }

    #[test]
    fn failures_no_probe_could_explain_run_nothing() {
        for reason in [
            "image preparation failed: pull denied; reconcile engine state before retrying",
            "runner thread panicked: index out of bounds",
            "peer disconnected",
            "",
        ] {
            assert!(explains(reason, false).is_empty(), "{reason}");
        }
    }
}
