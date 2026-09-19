//! The readiness report across refreshes (#255). Local checks come from `probe` over a
//! fake root; retained checks stand in for host-probe results and safety states.

use super::super::report::{ReadinessReport, REFRESH_WARNING_ID};
use super::super::*;
use super::{get, FakeRoot};
use std::time::{Duration, SystemTime};

fn at(secs: u64) -> SystemTime {
    SystemTime::UNIX_EPOCH + Duration::from_secs(secs)
}

fn check(id: &str, status: &str, summary: &str) -> ReadinessCheck {
    ReadinessCheck {
        id: id.into(),
        status: status.into(),
        summary: summary.into(),
        remediation: String::new(),
        observed_at: None,
        source: None,
        blocks: None,
    }
}

fn ids(checks: &[ReadinessCheck]) -> Vec<&str> {
    checks.iter().map(|c| c.id.as_str()).collect()
}

/// No `/dev/uinput`, so the local `uinput` check fails until the node appears.
fn root_without_uinput(name: &str) -> FakeRoot {
    let root = FakeRoot::new(name);
    root.file("dev/dri/renderD128", "")
        .file("proc/sys/user/max_user_namespaces", "15000\n")
        .file("etc/os-release", "ID=fedora\n");
    root
}

#[test]
fn a_retained_check_survives_a_refresh() {
    let root = root_without_uinput("report-survives");
    let mut report = ReadinessReport::default();
    report.refreshed(probe(&root.env(false, "")), at(0));
    report.retain(check("media_probe", FAIL, "GPU 0 cannot encode"), at(100));

    report.refreshed(probe(&root.env(false, "")), at(0));
    report.refreshed(probe(&root.env(false, "")), at(0));

    let merged = report.merged();
    assert_eq!(get(&merged, "media_probe").status, FAIL);
    assert_eq!(get(&merged, "media_probe").summary, "GPU 0 cannot encode");
    // The local set is still there, once.
    let local = probe(&root.env(false, ""));
    assert_eq!(merged.len(), local.len() + 1, "{:?}", ids(&merged));
}

#[test]
fn a_refresh_error_keeps_every_earlier_check_and_adds_the_warning() {
    let root = root_without_uinput("report-error");
    let mut report = ReadinessReport::default();
    report.refreshed(probe(&root.env(false, "")), at(0));
    report.retain(check("startup_cleanup", FAIL, "cleanup pending"), at(100));
    let before = report.merged();
    assert_eq!(get(&before, "uinput").status, FAIL);

    report.refresh_failed();

    let after = report.merged();
    assert_eq!(get(&after, "uinput"), get(&before, "uinput"));
    assert_eq!(get(&after, "startup_cleanup").status, FAIL);
    assert_eq!(get(&after, REFRESH_WARNING_ID).status, WARN);
    // Nothing else changed: the earlier report, then the warning.
    assert_eq!(after[..before.len()], before[..]);
    assert_eq!(after.len(), before.len() + 1);
}

#[test]
fn a_refresh_error_before_any_refresh_keeps_retained_checks() {
    let mut report = ReadinessReport::default();
    report.retain(check("runtime_endpoint", FAIL, "unreachable"), at(1));

    report.refresh_failed();

    assert_eq!(
        ids(&report.merged()),
        vec!["runtime_endpoint", REFRESH_WARNING_ID]
    );
}

#[test]
fn repeated_refresh_errors_add_one_warning() {
    let mut report = ReadinessReport::default();
    report.refreshed(vec![check("uinput", FAIL, "missing")], at(0));

    report.refresh_failed();
    report.refresh_failed();

    assert_eq!(ids(&report.merged()), vec!["uinput", REFRESH_WARNING_ID]);
}

#[test]
fn the_refresh_warning_clears_on_the_next_successful_refresh() {
    let mut report = ReadinessReport::default();
    report.refreshed(vec![check("uinput", FAIL, "missing")], at(0));
    report.retain(check("media_probe", FAIL, "cannot encode"), at(1));
    report.refresh_failed();

    report.refreshed(vec![check("uinput", PASS, "present")], at(2));

    let merged = report.merged();
    assert_eq!(ids(&merged), vec!["uinput", "media_probe"]);
    assert_eq!(get(&merged, "uinput").status, PASS);
    assert_eq!(get(&merged, "media_probe").status, FAIL);
}

#[test]
fn local_checks_are_recomputed_by_every_refresh() {
    let root = root_without_uinput("report-recompute");
    let mut report = ReadinessReport::default();
    report.retain(check("media_probe", PASS, "encodes"), at(1));
    report.refreshed(probe(&root.env(false, "")), at(0));
    assert_eq!(get(&report.merged(), "uinput").status, FAIL);

    root.file("dev/uinput", "");
    report.refreshed(probe(&root.env(false, "")), at(0));

    assert_eq!(get(&report.merged(), "uinput").status, PASS);
}

#[test]
fn a_local_check_the_refresh_no_longer_reports_is_gone() {
    let mut report = ReadinessReport::default();
    report.refreshed(vec![check("a", PASS, ""), check("b", PASS, "")], at(0));

    report.refreshed(vec![check("a", PASS, "")], at(1));

    assert_eq!(ids(&report.merged()), vec!["a"]);
}

#[test]
fn a_retained_check_is_replaced_only_by_a_newer_result_for_the_same_id() {
    let mut report = ReadinessReport::default();
    report.retain(check("media_probe", FAIL, "second"), at(200));

    // Older: ignored.
    report.retain(check("media_probe", PASS, "first"), at(100));
    assert_eq!(get(&report.merged(), "media_probe").summary, "second");

    // Another id: added, never a replacement.
    report.retain(check("input_probe", PASS, "input ok"), at(300));
    assert_eq!(get(&report.merged(), "media_probe").summary, "second");

    // Newer: replaces, in place.
    report.retain(check("media_probe", PASS, "third"), at(300));
    let merged = report.merged();
    assert_eq!(get(&merged, "media_probe").summary, "third");
    assert_eq!(ids(&merged), vec!["media_probe", "input_probe"]);

    // Same instant: the later call wins.
    report.retain(check("media_probe", FAIL, "fourth"), at(300));
    assert_eq!(get(&report.merged(), "media_probe").summary, "fourth");
}

#[test]
fn merged_order_is_local_then_retained_then_the_warning() {
    let mut report = ReadinessReport::default();
    report.retain(check("z_probe", PASS, ""), at(1));
    report.retain(check("a_probe", PASS, ""), at(2));
    report.refreshed(
        vec![check("render_node", PASS, ""), check("uinput", PASS, "")],
        at(3),
    );
    report.refresh_failed();

    assert_eq!(
        ids(&report.merged()),
        vec![
            "render_node",
            "uinput",
            "z_probe",
            "a_probe",
            REFRESH_WARNING_ID
        ]
    );
}

/// Ids are meant to have one owner. If a refresh and a retained check ever share one, the
/// id appears once, the retained result stands, and a failure is never the one hidden.
#[test]
fn a_shared_id_appears_once_and_never_hides_a_failure() {
    let mut report = ReadinessReport::default();
    report.retain(check("shared", FAIL, "retained fail"), at(1));
    report.refreshed(
        vec![
            check("first", PASS, ""),
            check("shared", PASS, "local pass"),
        ],
        at(0),
    );
    let merged = report.merged();
    assert_eq!(ids(&merged), vec!["first", "shared"]);
    assert_eq!(get(&merged, "shared").summary, "retained fail");

    let mut report = ReadinessReport::default();
    report.retain(check("shared", PASS, "retained pass"), at(1));
    report.refreshed(vec![check("shared", FAIL, "local fail")], at(2));
    assert_eq!(get(&report.merged(), "shared").summary, "local fail");

    let mut report = ReadinessReport::default();
    report.retain(check("shared", FAIL, "retained fail"), at(1));
    report.refreshed(vec![check("shared", FAIL, "local fail")], at(2));
    assert_eq!(get(&report.merged(), "shared").summary, "retained fail");
}
