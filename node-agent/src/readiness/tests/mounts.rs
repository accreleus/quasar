//! `host_container_mounts` when the runtime client is busy or times out (#315).

use super::super::report::ReadinessReport;
use super::super::runtime_facts::{RuntimeFault, RuntimeView};
use super::super::{
    classify_mount_runtime, host_container_mounts_check, live_mount_observation, MountObservation,
    ENGINE_MOUNT_INSPECTION_FAILED, FAIL, PASS, UNKNOWN,
};
use crate::messages::ReadinessCheck;
use crate::runtime::{ErrorKind, RuntimeError};
use std::time::{Duration, SystemTime};

fn at(secs: u64) -> SystemTime {
    SystemTime::UNIX_EPOCH + Duration::from_secs(secs)
}

fn mount(checks: &[ReadinessCheck]) -> &ReadinessCheck {
    checks
        .iter()
        .find(|check| check.id == "host_container_mounts")
        .expect("host_container_mounts")
}

fn view(kind: ErrorKind) -> RuntimeView {
    RuntimeView::Observed {
        endpoint: "unix:///var/run/docker.sock".into(),
        outcome: Err(RuntimeFault::from(RuntimeError::from(kind))),
    }
}

#[test]
fn a_busy_or_timed_out_client_is_not_a_mount_failure() {
    for kind in [ErrorKind::Busy, ErrorKind::Cancelled, ErrorKind::Timeout] {
        assert_eq!(
            classify_mount_runtime(kind),
            MountObservation::Indeterminate,
            "{kind:?}"
        );
    }
    let check = host_container_mounts_check(&MountObservation::Indeterminate);
    assert_eq!(check.status, UNKNOWN);
    assert_ne!(check.status, FAIL);
    assert!(check.blocks.is_none());
}

#[test]
fn a_permission_or_missing_socket_still_fails_the_mount_check() {
    for kind in [ErrorKind::PermissionDenied, ErrorKind::Unavailable] {
        match classify_mount_runtime(kind) {
            MountObservation::Fail(summary) => {
                assert_eq!(summary, ENGINE_MOUNT_INSPECTION_FAILED);
                let check = host_container_mounts_check(&MountObservation::Fail(summary));
                assert_eq!(check.status, FAIL);
                assert_eq!(check.summary, ENGINE_MOUNT_INSPECTION_FAILED);
                assert!(check.remediation.contains("Docker socket"));
                assert!(check.blocks.is_none());
            }
            other => panic!("{kind:?} produced {other:?}"),
        }
    }
}

#[test]
fn a_timeout_view_is_not_treated_as_a_missing_socket() {
    let timed_out = view(ErrorKind::Timeout);
    assert!(timed_out.is_inspection_timeout());
    assert!(!timed_out.engine_answered());
    let missing = view(ErrorKind::Unavailable);
    assert!(!missing.is_inspection_timeout());
    // Not a container: both are Agree. In the dev container a timeout stays
    // indeterminate and a missing socket is the evidence failure.
    match live_mount_observation(&timed_out) {
        MountObservation::Indeterminate | MountObservation::Agree => {}
        MountObservation::Fail(summary) => panic!("timeout failed the mounts: {summary}"),
    }
}

#[test]
fn a_busy_refresh_keeps_the_last_pass_and_a_real_result_recovers() {
    let mut report = ReadinessReport::default();
    report.refreshed(
        vec![ReadinessCheck {
            id: "host_container_mounts".into(),
            status: PASS.into(),
            summary: "mounts agree".into(),
            remediation: String::new(),
            observed_at: None,
            source: Some("local".into()),
            blocks: None,
        }],
        at(0),
    );
    report.refreshed(
        vec![host_container_mounts_check(
            &MountObservation::Indeterminate,
        )],
        at(1),
    );
    let merged = report.merged();
    let kept = mount(&merged);
    assert_eq!(kept.status, PASS);
    assert_eq!(kept.summary, "mounts agree");
    assert!(kept.blocks.is_none());

    report.refreshed(
        vec![host_container_mounts_check(&MountObservation::Fail(
            ENGINE_MOUNT_INSPECTION_FAILED.into(),
        ))],
        at(2),
    );
    assert_eq!(mount(&report.merged()).status, FAIL);

    report.refreshed(
        vec![host_container_mounts_check(
            &MountObservation::Indeterminate,
        )],
        at(3),
    );
    assert_eq!(mount(&report.merged()).status, FAIL);
    assert_eq!(
        mount(&report.merged()).summary,
        ENGINE_MOUNT_INSPECTION_FAILED
    );

    report.refreshed(
        vec![host_container_mounts_check(&MountObservation::Agree)],
        at(4),
    );
    let merged = report.merged();
    let recovered = mount(&merged);
    assert_eq!(recovered.status, PASS);
    assert!(recovered.blocks.is_none());
}

#[test]
fn a_busy_refresh_with_no_prior_result_does_not_fail() {
    let mut report = ReadinessReport::default();
    report.refreshed(
        vec![host_container_mounts_check(
            &MountObservation::Indeterminate,
        )],
        at(0),
    );
    let merged = report.merged();
    let check = mount(&merged);
    assert_eq!(check.status, UNKNOWN);
    assert_ne!(check.status, FAIL);
    assert!(check.blocks.is_none());
}
