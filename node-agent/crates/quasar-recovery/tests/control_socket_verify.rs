//! `POST /v1/verify-manifest` (#824): release detection's signature check, served on the
//! control socket only, graded by the policy the actor admits by. Keys and the genuine
//! manifest/signature pair come from `testdata/recovery/trust-vectors/verify_signature.json`.

mod support;

use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::atomic::AtomicBool;
use std::sync::Arc;

use base64::Engine as _;
use quasar_recovery::actor::{Actor, ActorConfig, TrustConfig};
use quasar_recovery::engine::FakeEngine;
use quasar_recovery::server;
use quasar_recovery::socket::{MachineRole, ManifestVerdict, Verdict};
use quasar_recovery::trust::{Caller, SignatureMode, SignaturePolicy, TrustedKey};
use serde_json::{json, Value};
use support::*;

/// `vector_tests.rs` `KEY_SEED_PREFIX`: the vectors' keys are public by construction.
const KEY_SEED_PREFIX: &str =
    "quasar release-trust golden vector test key; public by construction, never trust it: ";

fn vector_key(label: &str) -> TrustedKey {
    let seed = ring::digest::digest(
        &ring::digest::SHA256,
        format!("{KEY_SEED_PREFIX}{label}").as_bytes(),
    );
    let pair = ring::signature::Ed25519KeyPair::from_seed_unchecked(seed.as_ref()).unwrap();
    use ring::signature::KeyPair;
    TrustedKey {
        id: label.into(),
        key: pair.public_key().as_ref().try_into().unwrap(),
    }
}

/// The genuine pair: (manifest, signature document).
fn genuine() -> (Vec<u8>, Vec<u8>) {
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/recovery/trust-vectors/verify_signature.json");
    let file: Value = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
    let v = &file["vectors"][0];
    assert_eq!(v["name"], "a genuine signature verifies");
    let text = |k: &str| v[k]["text"].as_str().unwrap().as_bytes().to_vec();
    (text("manifest"), text("document"))
}

fn serve(
    mode: SignatureMode,
    keys: Vec<TrustedKey>,
    caller: Caller,
) -> (PathBuf, tempfile::TempDir) {
    let engine = Arc::new(FakeEngine::new(amd_host()));
    let dir = tempfile::tempdir().unwrap();
    let mut config = ActorConfig::new(dir.path().join("machine"), MachineRole::Gpu, operator());
    config.trust = TrustConfig {
        allowed_namespaces: vec![],
        signature: SignaturePolicy { mode, keys },
    };
    let actor = Arc::new(Actor::new(engine, config));
    let path = dir.path().join("control.sock");
    let listener = server::bind(&path).unwrap();
    let stop = Arc::new(AtomicBool::new(false));
    std::thread::spawn(move || server::serve(listener, actor, caller, stop));
    (path, dir)
}

fn post(path: &Path, method: &str, body: &str) -> (u16, String) {
    let mut stream = UnixStream::connect(path).unwrap();
    write!(
        stream,
        "{method} /v1/verify-manifest HTTP/1.1\r\nHost: recovery\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",
        body.len()
    )
    .unwrap();
    // A route that answers without reading the body closes with it unread, so the last
    // read is ECONNRESET after the whole answer (as an older actor's 404 does).
    let mut raw = Vec::new();
    let _ = stream.read_to_end(&mut raw);
    let raw = String::from_utf8(raw).unwrap();
    let (head, body) = raw.split_once("\r\n\r\n").unwrap();
    (head[9..12].parse().unwrap(), body.to_owned())
}

fn check(path: &Path, manifest: &[u8], signature: Option<&[u8]>) -> ManifestVerdict {
    let b64 = |b: &[u8]| base64::engine::general_purpose::STANDARD.encode(b);
    let body = json!({"manifest": b64(manifest), "signature": signature.map(b64)}).to_string();
    let (status, body) = post(path, "POST", &body);
    assert_eq!(status, 200, "{body}");
    serde_json::from_str(&body).unwrap()
}

#[test]
fn a_genuine_signature_verifies_and_names_its_key() {
    let (manifest, sig) = genuine();
    let (path, _dir) = serve(
        SignatureMode::Verify,
        vec![vector_key("release-2026")],
        Caller::ControlPlane,
    );
    let v = check(&path, &manifest, Some(&sig));
    assert_eq!(v.verdict, Verdict::Verified, "{}", v.message);
    assert_eq!(v.key_id.as_deref(), Some("release-2026"));
}

#[test]
fn a_tampered_manifest_is_refused() {
    let (mut manifest, sig) = genuine();
    let at = manifest.len() - 3;
    manifest[at] = b'9'; // "0.3.0" -> "0.3.9"
    let (path, _dir) = serve(
        SignatureMode::Verify,
        vec![vector_key("release-2026")],
        Caller::ControlPlane,
    );
    let v = check(&path, &manifest, Some(&sig));
    assert_eq!(v.verdict, Verdict::Refused);
    assert!(v.message.contains("did not verify"), "{}", v.message);
    assert_eq!(v.key_id, None);
}

#[test]
fn an_untrusted_key_is_refused() {
    let (manifest, sig) = genuine();
    let (path, _dir) = serve(
        SignatureMode::Require,
        vec![vector_key("someone-else")],
        Caller::ControlPlane,
    );
    assert_eq!(
        check(&path, &manifest, Some(&sig)).verdict,
        Verdict::Refused
    );
}

#[test]
fn a_missing_signature_is_refused_under_verify_and_require() {
    let (manifest, _) = genuine();
    for mode in [SignatureMode::Verify, SignatureMode::Require] {
        let (path, _dir) = serve(mode, vec![vector_key("release-2026")], Caller::ControlPlane);
        let v = check(&path, &manifest, None);
        assert_eq!(v.verdict, Verdict::Refused, "{mode:?}");
        assert!(
            v.message.contains("publishes no signature"),
            "{}",
            v.message
        );
    }
}

#[test]
fn mode_off_checks_nothing() {
    let (manifest, _) = genuine();
    let (path, _dir) = serve(SignatureMode::Off, vec![], Caller::ControlPlane);
    let v = check(&path, &manifest, None);
    assert_eq!(v.verdict, Verdict::Unchecked);
    assert!(v.message.contains("\"off\""), "{}", v.message);
    let v = check(&path, b"not even json", Some(b"garbage"));
    assert_eq!(v.verdict, Verdict::Unchecked);
}

#[test]
fn verify_with_no_keys_fails_closed() {
    let (manifest, sig) = genuine();
    let (path, _dir) = serve(SignatureMode::Verify, vec![], Caller::ControlPlane);
    let v = check(&path, &manifest, Some(&sig));
    assert_eq!(v.verdict, Verdict::Refused);
    assert!(
        v.message.contains("trusts no release keys"),
        "{}",
        v.message
    );
}

#[test]
fn a_body_that_is_not_a_check_is_400_and_a_get_is_405() {
    let (path, _dir) = serve(
        SignatureMode::Verify,
        vec![vector_key("release-2026")],
        Caller::ControlPlane,
    );
    let (status, body) = post(
        &path,
        "POST",
        r#"{"manifest":"","signature":null,"extra":1}"#,
    );
    assert_eq!(status, 400, "{body}");
    assert!(body.contains("\"invalid\""), "{body}");
    let (status, body) = post(&path, "POST", r#"{"manifest":"!!","signature":"!!"}"#);
    assert_eq!(status, 200);
    let v: ManifestVerdict = serde_json::from_str(&body).unwrap();
    assert_eq!(v.verdict, Verdict::Refused);
    assert!(v.message.contains("not standard base64"), "{}", v.message);
    assert_eq!(post(&path, "GET", "").0, 405);
}

#[test]
fn the_agent_socket_does_not_serve_it() {
    let (manifest, sig) = genuine();
    let (path, _dir) = serve(
        SignatureMode::Verify,
        vec![vector_key("release-2026")],
        Caller::Agent,
    );
    let b64 = |b: &[u8]| base64::engine::general_purpose::STANDARD.encode(b);
    let body = json!({"manifest": b64(&manifest), "signature": b64(&sig)}).to_string();
    assert_eq!(post(&path, "POST", &body).0, 404);
}
