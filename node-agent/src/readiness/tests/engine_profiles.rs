//! The published engine-profile table (`testdata/engine-profiles/profiles.json`, RH-07
//! #406) against what `runtime_engine` reports, row by row. The site's quick start, its
//! engine-profile page and the enrollment script read the same table, so a disagreement
//! here is a host told one thing by the docs and another by its readiness card.
//!
//! Every row is probed through the readiness boundary with each of its platform's sample
//! os-release files in a fake root, and again with only the engine's own report (no
//! os-release mount), which is the agent's fallback.

use std::path::Path;

use serde_json::Value;

use super::super::runtime_facts::*;
use super::super::*;
use super::{get, FakeRoot};
use crate::runtime::{
    ApiVersion, EngineFacts, EngineInfo, EngineKind, EngineMode, EngineMode::Rootful,
    EngineMode::Rootless,
};

fn table() -> Value {
    let path =
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../testdata/engine-profiles/profiles.json");
    let body =
        std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()));
    serde_json::from_str(&body).expect("profiles.json is JSON")
}

fn text<'a>(v: &'a Value, key: &str) -> &'a str {
    v[key]
        .as_str()
        .unwrap_or_else(|| panic!("{key} is a string in {v}"))
}

fn kind(engine: &str) -> EngineKind {
    match engine {
        "docker" => EngineKind::Docker,
        "podman" => EngineKind::Podman,
        other => panic!("engine {other} is not in the table's vocabulary"),
    }
}

fn mode(mode: &str) -> EngineMode {
    match mode {
        "rootful" => Rootful,
        "rootless" => Rootless,
        other => panic!("mode {other} is not in the table's vocabulary"),
    }
}

fn facts(kind: EngineKind, mode: EngineMode, report: Option<&Value>) -> EngineFacts {
    // A current release of each engine: a row's status is for an engine at least as new as
    // the table's minimum for it.
    let version = match kind {
        EngineKind::Podman => "5.8.4",
        _ => "29.0.0",
    };
    versioned(kind, mode, report, version)
}

fn versioned(
    kind: EngineKind,
    mode: EngineMode,
    report: Option<&Value>,
    version: &str,
) -> EngineFacts {
    let api = ApiVersion {
        major: 1,
        minor: 41,
    };
    let field = |key: &str| {
        report
            .and_then(|r| r[key].as_str())
            .filter(|v| !v.is_empty())
            .map(str::to_string)
    };
    EngineFacts {
        info: EngineInfo {
            kind,
            name: "engine".into(),
            version: version.into(),
            api_version: api,
            server_min_api: api,
            server_max_api: api,
        },
        mode,
        operating_system: field("operatingSystem"),
        os_version: field("osVersion"),
        architecture: Some("x86_64".into()),
        cgroup_version: Some("2".into()),
        cgroup_driver: Some("systemd".into()),
        security_options: Vec::new(),
        runtimes: vec!["runc".into()],
        default_runtime: Some("runc".into()),
        cdi: None,
    }
}

/// `runtime_engine`'s verdict as a status word: the external reading of `engine_profile`.
fn reported_status(os_release: Option<&str>, facts: EngineFacts) -> String {
    let root = FakeRoot::new("engine-profiles");
    if let Some(body) = os_release {
        root.file("etc/os-release", body);
    }
    let env = ProbeEnv {
        runtime: RuntimeView::Observed {
            endpoint: "unix:///var/run/docker.sock".into(),
            outcome: Ok(facts),
        },
        ..root.env(false, "")
    };
    let checks = probe(&env);
    let c = get(&checks, ENGINE_ID);
    assert!(c.blocks.is_none(), "runtime_engine never blocks: {c:?}");
    match c.status.as_str() {
        PASS if c.summary.contains("a supported engine profile") => "supported".into(),
        WARN if c.summary.contains("an experimental engine profile") => "experimental".into(),
        WARN if c.summary.contains("an unsupported engine profile") => "unsupported".into(),
        _ => panic!("runtime_engine said something the table has no word for: {c:?}"),
    }
}

fn row<'a>(t: &'a Value, platform: &str, engine: &str, mode: &str) -> &'a Value {
    let rows: Vec<&Value> = t["profiles"]
        .as_array()
        .unwrap()
        .iter()
        .filter(|r| {
            text(r, "platform") == platform
                && text(r, "engine") == engine
                && text(r, "mode") == mode
        })
        .collect();
    assert_eq!(
        rows.len(),
        1,
        "exactly one row for {platform}/{engine}/{mode}"
    );
    rows[0]
}

fn samples<'a>(t: &'a Value, platform: &str) -> &'a Vec<Value> {
    let samples = t["platforms"][platform]["samples"]
        .as_array()
        .unwrap_or_else(|| panic!("platform {platform} has samples"));
    assert!(!samples.is_empty(), "platform {platform} has samples");
    samples
}

fn os_release(sample: &Value) -> String {
    sample["osRelease"]
        .as_array()
        .unwrap()
        .iter()
        .map(|l| format!("{}\n", l.as_str().unwrap()))
        .collect()
}

#[test]
fn every_platform_engine_and_mode_has_exactly_one_row() {
    let t = table();
    let platforms: Vec<&String> = t["platforms"].as_object().unwrap().keys().collect();
    let engines: Vec<&String> = t["engines"].as_object().unwrap().keys().collect();
    let modes: Vec<&str> = t["modes"]
        .as_array()
        .unwrap()
        .iter()
        .map(|m| m.as_str().unwrap())
        .collect();
    for p in &platforms {
        for e in &engines {
            for m in &modes {
                row(&t, p, e, m);
            }
        }
    }
    assert_eq!(
        t["profiles"].as_array().unwrap().len(),
        platforms.len() * engines.len() * modes.len(),
        "no row outside the platform x engine x mode grid"
    );
}

/// The agent reads each sample host as the table's platform, and reports the row's status.
#[test]
fn the_agent_agrees_with_every_row_given_the_hosts_os_release() {
    let t = table();
    for r in t["profiles"].as_array().unwrap() {
        let (platform, engine, m) = (text(r, "platform"), text(r, "engine"), text(r, "mode"));
        for sample in samples(&t, platform) {
            let body = os_release(sample);
            let host = HostOs::parse(&body).expect("sample names an ID");
            let report = sample["engineReports"].get(engine);
            let f = facts(kind(engine), mode(m), report);
            assert_eq!(
                ProfilePlatform::of(&f, Some(&host)).wire(),
                platform,
                "{} reads as {platform}",
                text(sample, "name")
            );
            assert_eq!(
                reported_status(Some(&body), f),
                text(r, "status"),
                "{platform}/{engine}/{m} on {}",
                text(sample, "name")
            );
        }
    }
}

/// With no os-release mount the agent falls back to the engine's own report, which cannot
/// see a derivative's family: the table records where that lands (`withoutOsRelease`).
#[test]
fn the_agent_agrees_with_the_table_from_the_engines_report_alone() {
    let t = table();
    for r in t["profiles"].as_array().unwrap() {
        let (platform, engine, m) = (text(r, "platform"), text(r, "engine"), text(r, "mode"));
        for sample in samples(&t, platform) {
            let Some(report) = sample["engineReports"].get(engine) else {
                continue;
            };
            let lands = report["withoutOsRelease"].as_str().unwrap_or(platform);
            let f = facts(kind(engine), mode(m), Some(report));
            assert_eq!(
                ProfilePlatform::of(&f, None).wire(),
                lands,
                "{engine} on {} reports {report}",
                text(sample, "name")
            );
            assert_eq!(
                reported_status(None, f),
                text(row(&t, lands, engine, m), "status"),
                "{platform}/{engine}/{m} on {} without os-release",
                text(sample, "name")
            );
        }
    }
}

#[test]
fn an_engine_the_agent_cannot_name_is_unsupported_everywhere() {
    let t = table();
    assert_eq!(text(&t["unknownEngine"], "status"), "unsupported");
    for platform in t["platforms"].as_object().unwrap().keys() {
        for sample in samples(&t, platform) {
            for m in [Rootful, Rootless] {
                let f = facts(EngineKind::Unknown, m, None);
                assert_eq!(
                    reported_status(Some(&os_release(sample)), f),
                    "unsupported",
                    "{}",
                    text(sample, "name")
                );
            }
        }
    }
}

/// An unsupported row names somewhere to go, and never another unsupported profile.
#[test]
fn every_unsupported_row_names_alternatives_that_are_not_unsupported() {
    let t = table();
    let mut rows: Vec<(&str, &Value)> = t["profiles"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| (text(r, "platform"), r))
        .collect();
    rows.extend(
        t["platforms"]
            .as_object()
            .unwrap()
            .keys()
            .map(|p| (p.as_str(), &t["unknownEngine"])),
    );
    for (platform, r) in rows {
        assert!(!text(r, "reason").is_empty(), "{r}");
        let alternatives = r["alternatives"].as_array().unwrap();
        if text(r, "status") != "unsupported" {
            continue;
        }
        assert!(!alternatives.is_empty(), "{r}");
        for alt in alternatives {
            let to = alt["platform"].as_str().unwrap_or(platform);
            let target = row(&t, to, text(alt, "engine"), text(alt, "mode"));
            assert_ne!(text(target, "status"), "unsupported", "{r} -> {alt}");
        }
    }
}

/// #424: Podman older than the table's minimum cannot change a restart policy, so it is
/// unsupported on every platform and mode, whatever the row says; the minimum itself is not.
#[test]
fn podman_older_than_the_tables_minimum_is_unsupported_everywhere() {
    let t = table();
    let minimum = text(&t["engines"]["podman"], "minimumVersion");
    assert_eq!(
        minimum, PODMAN_MINIMUM_VERSION,
        "the agent holds the table's minimum"
    );
    assert!(t["engines"]["podman"]["minimumVersionReason"].is_string());
    assert!(t["engines"]["docker"].get("minimumVersion").is_none());
    for platform in t["platforms"].as_object().unwrap().keys() {
        for sample in samples(&t, platform) {
            let body = os_release(sample);
            for m in [Rootful, Rootless] {
                let row_status = text(row(&t, platform, "podman", mode_word(m)), "status");
                for old in ["4.9.3", "5.0.3"] {
                    let f = versioned(EngineKind::Podman, m, None, old);
                    assert_eq!(
                        reported_status(Some(&body), f),
                        "unsupported",
                        "Podman {old} on {}",
                        text(sample, "name")
                    );
                }
                for current in ["5.1.0", "5.8.4", "6.0"] {
                    let f = versioned(EngineKind::Podman, m, None, current);
                    assert_eq!(
                        reported_status(Some(&body), f),
                        row_status,
                        "Podman {current} on {}",
                        text(sample, "name")
                    );
                }
            }
        }
    }
}

#[test]
fn an_old_podman_is_told_why_and_what_to_do() {
    let root = FakeRoot::new("engine-profiles-old-podman");
    let env = ProbeEnv {
        runtime: RuntimeView::Observed {
            endpoint: "unix:///run/podman/podman.sock".into(),
            outcome: Ok(versioned(EngineKind::Podman, Rootful, None, "4.9.3")),
        },
        ..root.env(false, "")
    };
    let checks = probe(&env);
    let c = get(&checks, ENGINE_ID);
    assert_eq!(c.status, WARN, "{c:?}");
    assert!(c.blocks.is_none(), "{c:?}");
    assert!(
        c.summary.contains("Podman 4.9.3") && c.summary.contains("older than Podman 5.1"),
        "{c:?}"
    );
    assert!(c.remediation.contains("5.1"), "{c:?}");
}

fn mode_word(m: EngineMode) -> &'static str {
    match m {
        Rootful => "rootful",
        Rootless => "rootless",
    }
}
