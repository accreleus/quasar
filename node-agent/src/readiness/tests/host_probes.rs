//! Host-probe outcomes as the operator sees them: recorded into the
//! report, merged with the local checks from `probe` over a fake root.

use super::super::report::ReadinessReport;
use super::super::*;
use super::FakeRoot;
use crate::host_probe::outcome::{
    child_outcome, forget, indeterminate_status, record, record_not_applicable, ChildEnd,
    ProbeOutcome,
};
use crate::host_probe::{ProbeKind, ProbeTarget};
use std::time::{Duration, SystemTime};

fn at(secs: u64) -> SystemTime {
    SystemTime::UNIX_EPOCH + Duration::from_secs(secs)
}

fn refreshed_report(name: &str) -> (FakeRoot, ReadinessReport) {
    let root = FakeRoot::new(name);
    root.file("dev/dri/renderD128", "")
        .file("dev/uinput", "")
        .file("proc/sys/user/max_user_namespaces", "15000\n")
        .file("etc/os-release", "ID=fedora\n");
    let mut report = ReadinessReport::default();
    report.refreshed(probe(&root.env(false, "")));
    (root, report)
}

fn find<'a>(checks: &'a [ReadinessCheck], id: &str) -> Option<&'a ReadinessCheck> {
    checks.iter().find(|c| c.id == id)
}

const MEDIA_GPU0: ProbeTarget = ProbeTarget {
    kind: ProbeKind::Media,
    gpu: Some(0),
};

fn exited(code: i32, stdout: &str) -> ChildEnd {
    ChildEnd::Exited {
        code,
        stdout: stdout.into(),
    }
}

#[test]
fn a_passing_probe_is_a_passing_check_that_survives_the_refresh() {
    let (root, mut report) = refreshed_report("hp-pass");
    record(
        &mut report,
        MEDIA_GPU0,
        child_outcome(
            MEDIA_GPU0,
            exited(0, "encoded 30 frames with vulkanh264enc"),
        ),
        at(100),
    );
    report.refreshed(probe(&root.env(false, "")));

    let merged = report.merged();
    let check = find(&merged, "media_probe_gpu0").expect("media check");
    assert_eq!(check.status, PASS);
    assert!(check.summary.contains("GPU 0"), "{}", check.summary);
    assert!(check.summary.contains("vulkanh264enc"), "{}", check.summary);
}

#[test]
fn a_failing_probe_is_a_failing_check_with_its_reason_and_a_fix() {
    let (_root, mut report) = refreshed_report("hp-fail");
    let target = ProbeTarget::host(ProbeKind::Input);
    record(
        &mut report,
        target,
        child_outcome(
            target,
            exited(1, "cannot open /dev/uinput: Permission denied"),
        ),
        at(100),
    );

    let merged = report.merged();
    let check = find(&merged, "input_probe").expect("input check");
    assert_eq!(check.status, FAIL);
    assert!(
        check.summary.contains("Permission denied"),
        "{}",
        check.summary
    );
    assert!(!check.remediation.is_empty());
}

#[test]
fn every_kind_has_a_fix_for_its_failure() {
    for kind in ProbeKind::ALL {
        let target = if kind.per_gpu() {
            ProbeTarget::gpu(kind, 0)
        } else {
            ProbeTarget::host(kind)
        };
        match child_outcome(target, exited(1, "broken")) {
            ProbeOutcome::Fail { remediation, .. } => {
                assert!(!remediation.is_empty(), "{kind:?}")
            }
            other => panic!("{kind:?}: {other:?}"),
        }
    }
}

#[test]
fn a_deadline_is_indeterminate_with_its_reason_not_a_failure_and_not_a_skip() {
    let (_root, mut report) = refreshed_report("hp-deadline");
    let outcome = child_outcome(MEDIA_GPU0, ChildEnd::Deadline(Duration::from_secs(30)));
    assert!(matches!(outcome, ProbeOutcome::Indeterminate { .. }));
    record(&mut report, MEDIA_GPU0, outcome, at(100));

    let merged = report.merged();
    let check = find(&merged, "media_probe_gpu0").expect("media check");
    assert_eq!(check.status, indeterminate_status());
    assert_ne!(check.status, FAIL);
    assert_ne!(check.status, SKIP);
    assert!(check.summary.contains("30"), "{}", check.summary);
}

/// No `unknown` on the wire before the #260 amendment is signed.
#[test]
fn indeterminate_is_reported_as_warn_until_the_contract_amendment() {
    assert_eq!(indeterminate_status(), WARN);
}

#[test]
fn a_preempted_probe_is_indeterminate_and_says_a_launch_took_priority() {
    match child_outcome(MEDIA_GPU0, ChildEnd::Preempted) {
        ProbeOutcome::Indeterminate { reason } => {
            assert!(reason.contains("launch"), "{reason}")
        }
        other => panic!("{other:?}"),
    }
}

#[test]
fn a_probe_that_could_not_start_is_indeterminate() {
    let outcome = child_outcome(MEDIA_GPU0, ChildEnd::SpawnFailed("ENOMEM".into()));
    assert!(matches!(outcome, ProbeOutcome::Indeterminate { .. }));
}

#[test]
fn a_crash_is_a_failure_that_names_the_signal() {
    let (_root, mut report) = refreshed_report("hp-crash");
    record(
        &mut report,
        MEDIA_GPU0,
        child_outcome(MEDIA_GPU0, ChildEnd::Signaled(libc::SIGSEGV)),
        at(100),
    );

    let merged = report.merged();
    let check = find(&merged, "media_probe_gpu0").expect("media check");
    assert_eq!(check.status, FAIL);
    assert!(check.summary.contains("SIGSEGV"), "{}", check.summary);
    assert!(!check.remediation.is_empty());
}

/// The OOM killer or an operator ended it: no evidence about the GPU.
#[test]
fn a_kill_from_outside_is_indeterminate_not_a_crash() {
    for signal in [libc::SIGKILL, libc::SIGTERM, 63] {
        match child_outcome(MEDIA_GPU0, ChildEnd::Signaled(signal)) {
            ProbeOutcome::Indeterminate { reason } => {
                assert!(reason.contains(&signal.to_string()), "{reason}")
            }
            other => panic!("signal {signal}: {other:?}"),
        }
    }
}

#[test]
fn only_exit_1_is_a_failing_verdict() {
    for code in [2, 101, 127] {
        let outcome = child_outcome(MEDIA_GPU0, exited(code, "media-probe: bad --size"));
        assert!(
            matches!(outcome, ProbeOutcome::Indeterminate { .. }),
            "exit {code}: {outcome:?}"
        );
    }
}

#[test]
fn indeterminate_never_replaces_a_definitive_result() {
    for (code, status) in [(0, PASS), (1, FAIL)] {
        let (_root, mut report) = refreshed_report("hp-stands");
        record(
            &mut report,
            MEDIA_GPU0,
            child_outcome(MEDIA_GPU0, exited(code, "the definitive run")),
            at(100),
        );
        record(
            &mut report,
            MEDIA_GPU0,
            child_outcome(MEDIA_GPU0, ChildEnd::Preempted),
            at(200),
        );

        let merged = report.merged();
        let check = find(&merged, "media_probe_gpu0").expect("media check");
        assert_eq!(check.status, status);
        assert!(
            check.summary.contains("the definitive run"),
            "{}",
            check.summary
        );
    }
}

#[test]
fn a_definitive_result_replaces_an_indeterminate_one() {
    let (_root, mut report) = refreshed_report("hp-concludes");
    record(
        &mut report,
        MEDIA_GPU0,
        child_outcome(MEDIA_GPU0, ChildEnd::Preempted),
        at(100),
    );
    record(
        &mut report,
        MEDIA_GPU0,
        child_outcome(MEDIA_GPU0, exited(0, "encoded")),
        at(200),
    );
    assert_eq!(
        find(&report.merged(), "media_probe_gpu0").unwrap().status,
        PASS
    );
}

#[test]
fn a_host_with_no_gpu_skips_the_gpu_probes() {
    let (_root, mut report) = refreshed_report("hp-nogpu");
    record_not_applicable(&mut report, ProbeKind::Media, at(100));
    record_not_applicable(&mut report, ProbeKind::ApplicationGpu, at(100));

    let merged = report.merged();
    assert_eq!(find(&merged, "media_probe").unwrap().status, SKIP);
    assert_eq!(find(&merged, "application_gpu_probe").unwrap().status, SKIP);
}

/// A host pinned to one render node never schedules a session on its other GPUs.
#[test]
fn a_gpu_sessions_are_never_placed_on_is_skipped_not_failed() {
    let (_root, mut report) = refreshed_report("hp-unpinned");
    let gpu1 = ProbeTarget::gpu(ProbeKind::Media, 1);
    record(
        &mut report,
        gpu1,
        ProbeOutcome::NotApplicable {
            summary: "This host is pinned to /dev/dri/renderD128".into(),
        },
        at(100),
    );
    let merged = report.merged();
    let check = find(&merged, "media_probe_gpu1").expect("media check");
    assert_eq!(check.status, SKIP);
    assert!(check.summary.contains("renderD128"));
}

#[test]
fn indeterminate_never_replaces_not_applicable() {
    let (_root, mut report) = refreshed_report("hp-skip-stands");
    let gpu1 = ProbeTarget::gpu(ProbeKind::Media, 1);
    record(
        &mut report,
        gpu1,
        ProbeOutcome::NotApplicable {
            summary: "pinned elsewhere".into(),
        },
        at(100),
    );
    record(
        &mut report,
        gpu1,
        child_outcome(gpu1, ChildEnd::Preempted),
        at(200),
    );
    assert_eq!(
        find(&report.merged(), "media_probe_gpu1").unwrap().status,
        SKIP
    );
}

#[test]
fn a_vanished_gpu_takes_its_check_with_it() {
    let (root, mut report) = refreshed_report("hp-vanished");
    let gpu1 = ProbeTarget::gpu(ProbeKind::Media, 1);
    record(
        &mut report,
        MEDIA_GPU0,
        child_outcome(MEDIA_GPU0, exited(0, "ok")),
        at(100),
    );
    record(
        &mut report,
        gpu1,
        child_outcome(gpu1, exited(1, "no encoder")),
        at(100),
    );

    forget(&mut report, gpu1);
    report.refreshed(probe(&root.env(false, "")));

    let merged = report.merged();
    assert!(find(&merged, "media_probe_gpu1").is_none());
    assert_eq!(find(&merged, "media_probe_gpu0").unwrap().status, PASS);
    // A late result for the GPU that is gone may still arrive; forgetting is not a
    // tombstone, so the orchestrator must not record it. Forgetting twice is harmless.
    forget(&mut report, gpu1);
}

#[test]
fn a_forgotten_check_can_be_recorded_again_with_an_older_time() {
    let (_root, mut report) = refreshed_report("hp-reborn");
    record(
        &mut report,
        MEDIA_GPU0,
        child_outcome(MEDIA_GPU0, exited(1, "bad")),
        at(500),
    );
    forget(&mut report, MEDIA_GPU0);
    record(
        &mut report,
        MEDIA_GPU0,
        child_outcome(MEDIA_GPU0, exited(0, "good")),
        at(100),
    );
    assert_eq!(
        find(&report.merged(), "media_probe_gpu0").unwrap().status,
        PASS
    );
}
