//! The shared control-socket fixtures (#356). The Go twin runs the same files in
//! `control-plane/internal/actorsocket/actorsocket_test.go`; both decode each fixture
//! into their type and must re-encode it to the same JSON value.

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};

use quasar_recovery::socket::{
    Accepted, AttemptResult, Reason, Rejection, Request, RequestKind, State, Status,
};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    shape: String,
    #[allow(dead_code)]
    about: String,
    body: Value,
}

fn fixtures() -> Vec<(String, Fixture)> {
    let dir = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/recovery/socket");
    let mut paths: Vec<PathBuf> = std::fs::read_dir(&dir)
        .expect("read the fixture directory")
        .map(|e| e.expect("dir entry").path())
        .filter(|p| p.file_name().is_some_and(|n| n != "README.md"))
        .collect();
    paths.sort();
    assert!(
        !paths.is_empty(),
        "no fixtures: the test must never pass vacuously"
    );
    paths
        .into_iter()
        .map(|p| {
            assert_eq!(
                p.extension().and_then(|e| e.to_str()),
                Some("json"),
                "{p:?}"
            );
            let f: Fixture = serde_json::from_slice(&std::fs::read(&p).expect("read"))
                .expect("fixture envelope");
            (p.file_name().unwrap().to_string_lossy().into_owned(), f)
        })
        .collect()
}

/// Decodes into `T` and re-encodes; the value must come back unchanged, so a field the
/// type lacks (dropped) or adds, or a different omit/null choice, fails.
fn round_trip<T: DeserializeOwned + Serialize>(name: &str, body: &Value) -> T {
    let v: T =
        serde_json::from_value(body.clone()).unwrap_or_else(|e| panic!("{name}: decode: {e}"));
    let again = serde_json::to_value(&v).expect("encode");
    assert_eq!(&again, body, "{name}: re-encoded differently");
    v
}

#[test]
fn every_fixture_round_trips_through_the_rust_types_and_the_vocabulary_is_covered() {
    let mut shapes = BTreeSet::new();
    let mut kinds = BTreeSet::new();
    let mut rejections: BTreeSet<String> = BTreeSet::new();
    let mut failures: BTreeSet<String> = BTreeSet::new();
    let mut restored = BTreeSet::new();
    let mut results: Vec<AttemptResult> = Vec::new();
    for (name, f) in fixtures() {
        shapes.insert(f.shape.clone());
        match f.shape.as_str() {
            "request" => {
                let r: Request = round_trip(&name, &f.body);
                kinds.insert(format!("{:?}", r.kind));
            }
            "accepted" => {
                round_trip::<Accepted>(&name, &f.body);
            }
            "rejection" => {
                let r: Rejection = round_trip(&name, &f.body);
                assert!(
                    !matches!(r.reason, Reason::Other(_)),
                    "{name}: an unknown reason in a fixture"
                );
                rejections.insert(r.reason.as_str().to_owned());
            }
            "result" => results.push(round_trip(&name, &f.body)),
            "status" => {
                let s: Status = round_trip(&name, &f.body);
                results.extend(s.result);
            }
            other => panic!("{name}: unknown shape {other:?} (both sides must know every shape)"),
        }
    }
    let mut states = BTreeSet::new();
    for r in &results {
        states.insert(format!("{:?}", r.state));
        assert_eq!(
            r.reason.is_some(),
            r.state == State::Failed,
            "{}: reason exactly when failed",
            r.request_id
        );
        assert_eq!(
            r.finished_at.is_some(),
            r.state.is_terminal(),
            "{}: finished_at exactly when terminal",
            r.request_id
        );
        if let Some(reason) = &r.reason {
            failures.insert(reason.as_str().to_owned());
            restored.insert(r.restored);
        }
    }
    let set = |xs: &[&str]| xs.iter().map(|s| (*s).to_owned()).collect::<BTreeSet<_>>();
    assert_eq!(
        shapes,
        set(&["accepted", "rejection", "request", "result", "status"])
    );
    let all_kinds: BTreeSet<String> = [
        RequestKind::Replace,
        RequestKind::Restore,
        RequestKind::Remove,
    ]
    .iter()
    .map(|k| format!("{k:?}"))
    .collect();
    assert_eq!(kinds, all_kinds);
    let all_states: BTreeSet<String> = [
        State::Pending,
        State::Pulling,
        State::Recreating,
        State::Verifying,
        State::Succeeded,
        State::Failed,
    ]
    .iter()
    .map(|s| format!("{s:?}"))
    .collect();
    assert_eq!(states, all_states);
    let names = |rs: &[Reason]| {
        rs.iter()
            .map(|r| r.as_str().to_owned())
            .collect::<BTreeSet<_>>()
    };
    assert_eq!(
        rejections,
        names(Reason::REJECTIONS),
        "rejection fixtures cover exactly the rejection reasons"
    );
    assert_eq!(
        failures,
        names(Reason::FAILURES),
        "failed results cover exactly the failure reasons"
    );
    let mut both = names(Reason::REJECTIONS);
    both.extend(names(Reason::FAILURES));
    assert_eq!(
        both,
        names(Reason::KNOWN),
        "every known reason is a rejection or a failure"
    );
    assert_eq!(
        Reason::REJECTIONS.len() + Reason::FAILURES.len(),
        Reason::KNOWN.len(),
        "and not both"
    );
    assert_eq!(
        restored,
        [false, true].into_iter().collect(),
        "a failure both restored and not"
    );
}

#[test]
fn an_unknown_reason_survives_verbatim() {
    let r: Rejection =
        serde_json::from_str(r#"{"reason":"from_the_future","message":"m"}"#).unwrap();
    assert_eq!(r.reason, Reason::Other("from_the_future".into()));
    assert_eq!(
        serde_json::to_value(&r).unwrap()["reason"],
        "from_the_future"
    );
}

#[test]
fn a_go_nil_slice_reads_as_empty_but_the_field_stays_required() {
    let body = r#"{"request_id":"r","kind":"remove","components":null,
        "release":{"id":"","version":null,"source_commit":""},"purge":true}"#;
    let r: Request = serde_json::from_str(body).unwrap();
    assert!(r.components.is_empty());
    let missing = r#"{"request_id":"r","kind":"remove","release":{"id":"","version":null,"source_commit":""}}"#;
    assert!(serde_json::from_str::<Request>(missing).is_err());
}

#[test]
fn a_request_refuses_a_field_it_does_not_know() {
    let body = r#"{"request_id":"r","kind":"replace","components":[],
        "release":{"id":"","version":null,"source_commit":""},"force":true}"#;
    assert!(serde_json::from_str::<Request>(body).is_err());
}

#[test]
fn a_status_from_a_newer_actor_still_decodes() {
    let (_, f) = fixtures()
        .into_iter()
        .find(|(n, _)| n == "status-combined-idle.json")
        .expect("fixture");
    let mut body = f.body;
    body["a_field_from_a_later_release"] = Value::Bool(true);
    body["services"][0]["another"] = Value::from(1);
    serde_json::from_value::<Status>(body).expect("an older reader accepts a newer status");
}
