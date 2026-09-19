//! Provenance and freshness on a readiness check (#261, protocol amendment 11):
//! `observed_at`, `source`, and `blocks`, as the control plane receives them.
//!
//! The rule these tests exist for: `blocks` rides only on a check that rests on
//! evidence. A proxy check carrying it could refuse sessions on a false negative.

use super::super::report::ReadinessReport;
use super::super::*;
use super::FakeRoot;
use crate::host_probe::outcome::{child_outcome, indeterminate_status, record, ChildEnd};
use crate::host_probe::{ProbeKind, ProbeTarget};
use crate::messages::ReadinessBlocks;
use std::time::{Duration, SystemTime};

fn at(secs: u64) -> SystemTime {
    SystemTime::UNIX_EPOCH + Duration::from_secs(secs)
}

fn find<'a>(checks: &'a [ReadinessCheck], id: &str) -> &'a ReadinessCheck {
    checks
        .iter()
        .find(|c| c.id == id)
        .unwrap_or_else(|| panic!("no check {id}"))
}

fn exited(code: i32, stdout: &str) -> ChildEnd {
    ChildEnd::Exited {
        code,
        stdout: stdout.into(),
    }
}

fn gpu(kind: ProbeKind, index: i32) -> ProbeTarget {
    ProbeTarget {
        kind,
        gpu: Some(index),
    }
}

fn blocks(scope: &str, gpu_index: Option<i32>, enforced_by: &str) -> Option<ReadinessBlocks> {
    Some(ReadinessBlocks {
        scope: scope.into(),
        gpu_index,
        enforced_by: enforced_by.into(),
    })
}

/// The local checks over every fixture shape the suite already uses: NVIDIA and not,
/// a filtering firewall and an open one. Proxy checks take different arms in each.
fn local_check_sets(name: &str) -> Vec<Vec<ReadinessCheck>> {
    let root = FakeRoot::new(name);
    root.file("dev/dri/renderD128", "")
        .file("dev/uinput", "")
        .file("proc/sys/user/max_user_namespaces", "15000\n")
        .file("etc/os-release", "ID=fedora\n");
    let bare = FakeRoot::new(&format!("{name}-bare"));
    vec![
        probe(&root.env(false, "")),
        probe(&root.env(true, "/usr/lib")),
        probe(&bare.env(false, "")),
        probe(&bare.env(true, "/usr/lib")),
    ]
}

/// Every local id that rests on evidence, with what it blocks. Anything `probe` emits
/// that is not listed here is a proxy.
fn local_evidence(id: &str) -> Option<Option<ReadinessBlocks>> {
    match id {
        "homes_root_writable" | "homes_free_space" => Some(blocks("homes", None, "control_plane")),
        "runtime_endpoint" => Some(blocks("host", None, "agent")),
        _ => None,
    }
}

#[test]
fn no_proxy_check_ever_carries_blocks() {
    for checks in local_check_sets("prov-proxy") {
        assert!(checks.len() > 20, "the fixture lost most of the check set");
        for c in &checks {
            if local_evidence(&c.id).is_none() {
                assert_eq!(
                    c.blocks, None,
                    "proxy check {} carries blocks: a false negative would refuse launches",
                    c.id
                );
            }
        }
    }
}

#[test]
fn local_evidence_checks_carry_their_scope_whatever_their_status() {
    for checks in local_check_sets("prov-evidence") {
        for id in [
            "homes_root_writable",
            "homes_free_space",
            "runtime_endpoint",
        ] {
            let c = find(&checks, id);
            assert_eq!(c.blocks, local_evidence(id).unwrap(), "{id} ({})", c.status);
        }
    }
}

#[test]
fn every_local_check_names_a_known_source() {
    for checks in local_check_sets("prov-source") {
        for c in &checks {
            let want = if c.id.starts_with("runtime_") {
                "runtime"
            } else {
                "local"
            };
            assert_eq!(c.source.as_deref(), Some(want), "{}", c.id);
        }
    }
}

#[test]
fn host_probe_results_carry_their_source_and_scope() {
    let mut report = ReadinessReport::default();
    let cases = [
        (
            gpu(ProbeKind::Media, 1),
            blocks("gpu", Some(1), "control_plane"),
        ),
        (
            gpu(ProbeKind::ApplicationGpu, 0),
            blocks("gpu", Some(0), "control_plane"),
        ),
        (
            ProbeTarget::host(ProbeKind::Input),
            blocks("host", None, "control_plane"),
        ),
        (
            ProbeTarget::host(ProbeKind::Audio),
            blocks("host", None, "control_plane"),
        ),
    ];
    // Pass, fail and indeterminate alike: `blocks` says what the check WOULD block.
    for (code, secs) in [(0, 10), (1, 20)] {
        for (target, want) in &cases {
            record(
                &mut report,
                *target,
                child_outcome(*target, exited(code, "x")),
                at(secs),
            );
            let merged = report.merged();
            let c = find(&merged, &target.check_id());
            assert_eq!(&c.blocks, want, "{} exit {code}", c.id);
            assert_eq!(c.source.as_deref(), Some("host_probe"), "{}", c.id);
        }
    }
    let fresh = gpu(ProbeKind::Media, 3);
    record(
        &mut report,
        fresh,
        child_outcome(fresh, ChildEnd::Deadline(Duration::from_secs(15))),
        at(30),
    );
    let merged = report.merged();
    let c = find(&merged, "media_probe_gpu3");
    assert_eq!(c.blocks, blocks("gpu", Some(3), "control_plane"));
    assert_eq!(c.source.as_deref(), Some("host_probe"));
}

#[test]
fn indeterminate_is_reported_as_unknown() {
    assert_eq!(indeterminate_status(), UNKNOWN);
    assert_eq!(UNKNOWN, "unknown");
}

#[test]
fn an_inconclusive_probe_keeps_the_last_definitive_result_and_its_observation_time() {
    let target = gpu(ProbeKind::Media, 0);
    let mut report = ReadinessReport::default();
    record(
        &mut report,
        target,
        child_outcome(target, exited(1, "no frames")),
        at(1_000),
    );
    record(
        &mut report,
        target,
        child_outcome(target, ChildEnd::Preempted),
        at(2_000),
    );
    let merged = report.merged();
    let c = find(&merged, "media_probe_gpu0");
    assert_eq!(
        c.status, FAIL,
        "an indeterminate probe must not clear a block"
    );
    assert_eq!(c.observed_at.as_deref(), Some("1970-01-01T00:16:40Z"));

    // And the mirror: it must not set one either.
    let mut report = ReadinessReport::default();
    record(
        &mut report,
        target,
        child_outcome(target, exited(0, "ok")),
        at(1_000),
    );
    record(
        &mut report,
        target,
        child_outcome(target, ChildEnd::Deadline(Duration::from_secs(15))),
        at(2_000),
    );
    let merged = report.merged();
    let c = find(&merged, "media_probe_gpu0");
    assert_eq!(c.status, PASS);
    assert_eq!(c.observed_at.as_deref(), Some("1970-01-01T00:16:40Z"));
}

#[test]
fn unknown_is_reported_only_while_no_definitive_result_exists() {
    let target = ProbeTarget::host(ProbeKind::Audio);
    let mut report = ReadinessReport::default();
    record(
        &mut report,
        target,
        child_outcome(target, ChildEnd::SpawnFailed("ENOENT".into())),
        at(50),
    );
    let merged = report.merged();
    let c = find(&merged, "audio_probe");
    assert_eq!(c.status, UNKNOWN);
    assert!(c.summary.contains("ENOENT"), "{}", c.summary);
    assert_eq!(c.observed_at.as_deref(), Some("1970-01-01T00:00:50Z"));

    record(
        &mut report,
        target,
        child_outcome(target, exited(0, "socket appeared")),
        at(60),
    );
    let merged = report.merged();
    let c = find(&merged, "audio_probe");
    assert_eq!(c.status, PASS);
    assert_eq!(c.observed_at.as_deref(), Some("1970-01-01T00:01:00Z"));
}

#[test]
fn a_refreshed_check_is_observed_at_the_refresh_and_a_retained_one_at_its_own_time() {
    let root = FakeRoot::new("prov-times");
    root.file("etc/os-release", "ID=fedora\n");
    let mut report = ReadinessReport::default();
    let target = ProbeTarget::host(ProbeKind::Input);
    record(
        &mut report,
        target,
        child_outcome(target, exited(0, "ok")),
        at(100),
    );
    report.refreshed(probe(&root.env(false, "")), at(3_600));

    let merged = report.merged();
    assert_eq!(
        find(&merged, "render_node").observed_at.as_deref(),
        Some("1970-01-01T01:00:00Z")
    );
    assert_eq!(
        find(&merged, "input_probe").observed_at.as_deref(),
        Some("1970-01-01T00:01:40Z")
    );

    // A failed refresh keeps the earlier local checks, and they keep the time they
    // were really observed at; the warning it adds carries no observation of its own.
    report.refresh_failed();
    let merged = report.merged();
    assert_eq!(
        find(&merged, "render_node").observed_at.as_deref(),
        Some("1970-01-01T01:00:00Z")
    );
    let warning = find(&merged, super::super::report::REFRESH_WARNING_ID);
    assert_eq!(warning.blocks, None);
}

#[test]
fn the_startup_cleanup_safety_check_is_agent_enforced() {
    use crate::diagnostic::{Fault, Phase};
    let check = Phase::Diagnostic(Fault::RuntimeUnusable("no engine".into()))
        .safety_check()
        .expect("diagnostic mode reports its safety check");
    assert_eq!(check.id, "startup_cleanup");
    assert_eq!(check.blocks, blocks("host", None, "agent"));
    assert_eq!(check.source.as_deref(), Some("runtime"));
    assert_eq!(Phase::Normal.safety_check(), None);
}

#[test]
fn the_wire_shape_adds_fields_and_changes_none() {
    // A check with no provenance is byte-for-byte what a pre-amendment agent sent.
    let bare = ReadinessCheck {
        id: "render_node".into(),
        status: PASS.into(),
        summary: "s".into(),
        remediation: String::new(),
        observed_at: None,
        source: None,
        blocks: None,
    };
    assert_eq!(
        serde_json::to_string(&bare).unwrap(),
        r#"{"id":"render_node","status":"pass","summary":"s","remediation":""}"#
    );

    let full = ReadinessCheck {
        observed_at: Some("2026-09-19T10:00:00Z".into()),
        source: Some("host_probe".into()),
        blocks: blocks("gpu", Some(1), "control_plane"),
        ..bare.clone()
    };
    let v: serde_json::Value = serde_json::to_value(&full).unwrap();
    assert_eq!(v["observed_at"], "2026-09-19T10:00:00Z");
    assert_eq!(v["source"], "host_probe");
    assert_eq!(
        v["blocks"],
        serde_json::json!({"scope": "gpu", "gpu_index": 1, "enforced_by": "control_plane"})
    );

    // gpu_index is present only for the gpu scope.
    let host = ReadinessCheck {
        blocks: blocks("host", None, "agent"),
        ..bare
    };
    let v: serde_json::Value = serde_json::to_value(&host).unwrap();
    assert_eq!(
        v["blocks"],
        serde_json::json!({"scope": "host", "enforced_by": "agent"})
    );
}
