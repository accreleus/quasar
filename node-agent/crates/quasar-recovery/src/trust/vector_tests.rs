//! The shared release-trust golden vectors (#356), run against the Rust port. In the crate
//! rather than under `tests/` so the probe and the redirect rule stay crate-private.
//!
//! `control-plane/internal/updater/trustvectors_test.go` runs the same files against the
//! Go updater, and its case table generates them. Both runners know every kind, run
//! every vector, and fail on a file or kind they do not know.

use std::collections::{BTreeMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use base64::Engine as _;
use ring::signature::KeyPair as _;
use serde::Deserialize;
use serde_json::{json, Value};

use super::signature::verify_manifest_signature;
use super::source::{
    probe_assets, redirect_allowed, AssetResponse, ReleaseAssets, FORMAT_1_ASSETS, FORMAT_2_ASSETS,
};
use super::{
    admit, parse_allowed_namespaces, parse_manifest_base_url, parse_signature_mode,
    parse_trusted_keys, wants_signature_evidence, Caller, Config, SignatureEvidence,
    SignaturePolicy, TrustedKey,
};
use crate::socket::{Component, Release, Request, RequestKind};

fn vector_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/recovery/trust-vectors")
}

/// Must equal `vectorKeySeedPrefix` in the Go runner: keys are derived from labels, so
/// no key material is ever committed.
const KEY_SEED_PREFIX: &str =
    "quasar release-trust golden vector test key; public by construction, never trust it: ";

fn public_key_of(label: &str) -> [u8; 32] {
    let seed = ring::digest::digest(
        &ring::digest::SHA256,
        format!("{KEY_SEED_PREFIX}{label}").as_bytes(),
    );
    let pair = ring::signature::Ed25519KeyPair::from_seed_unchecked(seed.as_ref()).expect("seed");
    pair.public_key().as_ref().try_into().expect("32 bytes")
}

const VECTOR_ORIGIN: &str = "https://releases.example.invalid";

// ── file shapes ──────────────────────────────────────────────────────────────

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct VectorFile {
    kind: String,
    #[allow(dead_code)]
    about: String,
    vectors: Vec<Value>,
}

#[derive(Deserialize, Clone, Debug)]
#[serde(deny_unknown_fields)]
struct BytesSpec {
    text: Option<String>,
    base64: Option<String>,
    segments: Option<Vec<Segment>>,
}

#[derive(Deserialize, Clone, Debug)]
#[serde(deny_unknown_fields)]
struct Segment {
    text: String,
    count: usize,
}

impl BytesSpec {
    fn bytes(&self) -> Vec<u8> {
        match (&self.text, &self.base64, &self.segments) {
            (Some(t), None, None) => t.as_bytes().to_vec(),
            (None, Some(b), None) => base64::engine::general_purpose::STANDARD
                .decode(b)
                .expect("vector base64"),
            (None, None, Some(segs)) => segs
                .iter()
                .flat_map(|s| s.text.repeat(s.count).into_bytes())
                .collect(),
            _ => panic!("a bytes spec carries exactly one of text, base64, segments: {self:?}"),
        }
    }
}

#[derive(Deserialize, Clone, Debug)]
#[serde(deny_unknown_fields)]
struct VectorKey {
    id: String,
    public_key_of: String,
}

fn trusted_keys(keys: &[VectorKey]) -> Vec<TrustedKey> {
    keys.iter()
        .map(|k| TrustedKey {
            id: k.id.clone(),
            key: public_key_of(&k.public_key_of),
        })
        .collect()
}

#[derive(Deserialize, Clone, Debug)]
#[serde(deny_unknown_fields)]
struct EvidenceSpec {
    state: String,
    manifest: Option<BytesSpec>,
    signature: Option<BytesSpec>,
    why: Option<String>,
    error: Option<String>,
    error_prefix: Option<String>,
}

impl EvidenceSpec {
    fn evidence(&self) -> SignatureEvidence {
        match self.state.as_str() {
            "signed" => SignatureEvidence::Signed {
                manifest: self
                    .manifest
                    .as_ref()
                    .map(BytesSpec::bytes)
                    .unwrap_or_default(),
                signature: self
                    .signature
                    .as_ref()
                    .map(BytesSpec::bytes)
                    .unwrap_or_default(),
            },
            "absent" => SignatureEvidence::Absent {
                why: self.why.clone().unwrap_or_default(),
            },
            "fetch_error" => SignatureEvidence::FetchError {
                error: self.error.clone().unwrap_or_default(),
            },
            other => panic!("unknown evidence state {other}"),
        }
    }
}

#[derive(Deserialize, Clone, Debug)]
#[serde(deny_unknown_fields)]
struct FetchSpec {
    base_url: String,
    responses: BTreeMap<String, AssetSpec>,
    /// 2 for a vector about the format-2 pair the actor fetches; absent is the Go
    /// updater's format-1 pair.
    asset_format: Option<u8>,
}

impl FetchSpec {
    fn assets(&self) -> ReleaseAssets {
        match self.asset_format {
            None | Some(1) => FORMAT_1_ASSETS,
            Some(2) => FORMAT_2_ASSETS,
            Some(other) => panic!("unknown asset_format {other}"),
        }
    }
}

#[derive(Deserialize, Clone, Debug)]
#[serde(deny_unknown_fields)]
struct AssetSpec {
    status: Option<u16>,
    body: Option<BytesSpec>,
    transport_error: Option<bool>,
}

/// Serves a FetchSpec the way the Go runner's test server does; unlisted URLs are 404s.
fn fake_fetch(
    spec: &FetchSpec,
    fetched: &mut Vec<String>,
    url: &str,
) -> Result<AssetResponse, String> {
    fetched.push(url.to_owned());
    match spec.responses.get(url) {
        None => Ok(AssetResponse {
            status: 404,
            body: b"404 page not found\n".to_vec(),
        }),
        Some(AssetSpec {
            transport_error: Some(true),
            ..
        }) => Err("connection closed by the vector".into()),
        Some(AssetSpec { status, body, .. }) => Ok(AssetResponse {
            status: status.unwrap_or(200),
            body: body.as_ref().map(BytesSpec::bytes).unwrap_or_default(),
        }),
    }
}

fn gather(spec: &FetchSpec, version: Option<&str>) -> (SignatureEvidence, Vec<String>) {
    assert!(
        spec.base_url.starts_with(&format!("{VECTOR_ORIGIN}/")),
        "{}",
        spec.base_url
    );
    let base = parse_manifest_base_url(&spec.base_url).expect("vector base url");
    let mut fetched = Vec::new();
    let evidence =
        probe_assets(&base, version, spec.assets()).run(|url| fake_fetch(spec, &mut fetched, url));
    (evidence, fetched)
}

// ── kind: admit ──────────────────────────────────────────────────────────────

#[derive(Deserialize, Clone, Debug)]
#[serde(deny_unknown_fields)]
struct AdmitConfig {
    allowed_namespaces: Vec<String>,
    in_flight_request_id: String,
    signature_mode: String,
    trusted_keys: Vec<VectorKey>,
}

/// The Go updater's ApplyRequest, which is the trust-relevant part of a Request.
#[derive(Deserialize, Clone, Debug)]
#[serde(deny_unknown_fields)]
struct VectorRequest {
    request_id: String,
    components: Vec<Component>,
    release: Release,
}

#[derive(Deserialize, Clone, Debug)]
#[serde(deny_unknown_fields)]
struct AdmitVector {
    name: String,
    #[allow(dead_code)]
    source: String,
    caller: String,
    config: AdmitConfig,
    request: VectorRequest,
    evidence: Option<EvidenceSpec>,
    fetch: Option<FetchSpec>,
    expect: Value,
    expect_without_caller_guard: Option<Value>,
}

/// Captures WARN events, so every unverified apply is shown to be logged, not just returned.
#[derive(Clone, Default)]
struct WarnCapture(Arc<Mutex<Vec<String>>>);

impl<S: tracing::Subscriber> tracing_subscriber::Layer<S> for WarnCapture {
    fn on_event(&self, event: &tracing::Event<'_>, _: tracing_subscriber::layer::Context<'_, S>) {
        struct Message(String);
        impl tracing::field::Visit for Message {
            fn record_debug(&mut self, field: &tracing::field::Field, value: &dyn std::fmt::Debug) {
                if field.name() == "message" {
                    self.0 = format!("{value:?}");
                }
            }
        }
        if *event.metadata().level() == tracing::Level::WARN {
            let mut m = Message(String::new());
            event.record(&mut m);
            self.0.lock().unwrap().push(m.0);
        }
    }
}

fn run_admit(v: &AdmitVector, caller: Caller) -> Value {
    let cfg = Config {
        allowed_namespaces: v.config.allowed_namespaces.clone(),
        in_flight_request_id: Some(v.config.in_flight_request_id.clone()).filter(|s| !s.is_empty()),
        signature: SignaturePolicy {
            mode: parse_signature_mode(&v.config.signature_mode).expect("vector mode"),
            keys: trusted_keys(&v.config.trusted_keys),
        },
    };
    let req = Request {
        request_id: v.request.request_id.clone(),
        kind: RequestKind::Replace,
        components: v.request.components.clone(),
        release: v.request.release.clone(),
        migrates: false,
        schema_version: None,
        external_backup_confirmed: false,
        dump: None,
        purge: false,
        wait_timeout_s: 0,
    };
    let mut fetched = None;
    let evidence = match (&v.evidence, &v.fetch) {
        (Some(_), Some(_)) => panic!("{}: evidence or fetch, never both", v.name),
        (Some(e), None) => Some(e.evidence()),
        (None, Some(f)) => {
            let mut urls = Vec::new();
            let ev = wants_signature_evidence(&cfg, &req.request_id).then(|| {
                let (ev, u) = gather(f, req.release.version.as_deref());
                urls = u;
                ev
            });
            fetched = Some(urls);
            ev
        }
        (None, None) => None,
    };

    let capture = WarnCapture::default();
    let subscriber = tracing_subscriber::layer::SubscriberExt::with(
        tracing_subscriber::registry(),
        capture.clone(),
    );
    let decision = tracing::subscriber::with_default(subscriber, || {
        admit(caller, &req, &cfg, evidence.as_ref())
    });
    let logged = capture.0.lock().unwrap().clone();

    let mut out = match decision {
        Ok(a) => {
            let warnings: Vec<String> = a.unverified.into_iter().collect();
            assert_eq!(
                logged, warnings,
                "{}: every unverified apply is logged at WARN, exactly once",
                v.name
            );
            json!({"admitted": true, "warnings": warnings})
        }
        Err(r) => {
            assert!(
                logged.is_empty(),
                "{}: a refusal logs no unverified-apply WARN",
                v.name
            );
            json!({"admitted": false, "reason": r.reason.as_str(), "message": r.message, "warnings": []})
        }
    };
    if let Some(urls) = fetched {
        out["fetched"] = json!(urls);
    }
    out
}

/// Compares a decision against its expectation, honouring `message_prefix`.
fn match_decision(name: &str, want: &Value, mut got: Value, prefix_key: &str, message_key: &str) {
    let mut want = want.clone();
    if let Some(prefix) = want
        .get(prefix_key)
        .and_then(Value::as_str)
        .map(str::to_owned)
    {
        let msg = got
            .get(message_key)
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        assert!(
            msg.starts_with(&prefix) && msg != prefix,
            "{name}: {msg:?} does not extend the pinned prefix {prefix:?}"
        );
        want.as_object_mut().unwrap().remove(prefix_key);
        got.as_object_mut().unwrap().remove(message_key);
    }
    assert_eq!(want, got, "{name}");
}

// ── the other kinds ──────────────────────────────────────────────────────────

#[derive(Deserialize, Debug)]
#[serde(deny_unknown_fields)]
struct VerifyVector {
    name: String,
    #[allow(dead_code)]
    source: String,
    manifest: BytesSpec,
    document: BytesSpec,
    trusted_keys: Vec<VectorKey>,
    expect: Value,
}

#[derive(Deserialize, Debug)]
#[serde(deny_unknown_fields)]
struct EvidenceVector {
    name: String,
    #[allow(dead_code)]
    source: String,
    version: Option<String>,
    fetch: FetchSpec,
    expect: EvidenceExpect,
}

#[derive(Deserialize, Debug)]
#[serde(deny_unknown_fields)]
struct EvidenceExpect {
    evidence: EvidenceSpec,
    fetched: Vec<String>,
}

#[derive(Deserialize, Debug)]
#[serde(deny_unknown_fields)]
struct GateVector {
    name: String,
    #[allow(dead_code)]
    source: String,
    signature_mode: String,
    in_flight_request_id: String,
    request_id: String,
    expect: GateExpect,
}

#[derive(Deserialize, Debug)]
#[serde(deny_unknown_fields)]
struct GateExpect {
    fetches: bool,
}

#[derive(Deserialize, Debug)]
#[serde(deny_unknown_fields)]
struct ConfigVector {
    name: String,
    #[allow(dead_code)]
    source: String,
    parse: String,
    raw: String,
    expect: Value,
}

#[derive(Deserialize, Debug)]
#[serde(deny_unknown_fields)]
struct RedirectVector {
    name: String,
    #[allow(dead_code)]
    source: String,
    via: Vec<String>,
    next: String,
    expect: Value,
}

fn run_verify(v: &VerifyVector) {
    let got = match verify_manifest_signature(
        &v.manifest.bytes(),
        &v.document.bytes(),
        &trusted_keys(&v.trusted_keys),
    ) {
        Ok(id) => json!({"verified": true, "key_id": id}),
        Err(e) => json!({"verified": false, "error": e}),
    };
    match_decision(&v.name, &v.expect, got, "error_prefix", "error");
}

fn run_evidence(v: &EvidenceVector) {
    let (ev, fetched) = gather(&v.fetch, v.version.as_deref());
    assert_eq!(fetched, v.expect.fetched, "{}: fetched", v.name);
    let want = &v.expect.evidence;
    match (&ev, want.state.as_str()) {
        (
            SignatureEvidence::Signed {
                manifest,
                signature,
            },
            "signed",
        ) => {
            assert_eq!(
                Some(manifest.clone()),
                want.manifest.as_ref().map(BytesSpec::bytes),
                "{}: manifest",
                v.name
            );
            assert_eq!(
                Some(signature.clone()),
                want.signature.as_ref().map(BytesSpec::bytes),
                "{}: signature",
                v.name
            );
        }
        (SignatureEvidence::Absent { why }, "absent") => {
            assert_eq!(Some(why), want.why.as_ref(), "{}", v.name)
        }
        (SignatureEvidence::FetchError { error }, "fetch_error") => {
            match (&want.error, &want.error_prefix) {
                (Some(exact), None) => assert_eq!(error, exact, "{}", v.name),
                (None, Some(prefix)) => {
                    assert!(
                        error.starts_with(prefix.as_str()) && error != prefix,
                        "{}: {error:?} vs {prefix:?}",
                        v.name
                    )
                }
                _ => panic!(
                    "{}: a fetch_error expectation pins error or error_prefix",
                    v.name
                ),
            }
        }
        (got, want) => panic!("{}: got {got:?}, want state {want}", v.name),
    }
}

fn run_gate(v: &GateVector) {
    let cfg = Config {
        allowed_namespaces: vec![],
        in_flight_request_id: Some(v.in_flight_request_id.clone()).filter(|s| !s.is_empty()),
        signature: SignaturePolicy {
            mode: parse_signature_mode(&v.signature_mode).expect("mode"),
            keys: vec![],
        },
    };
    assert_eq!(
        wants_signature_evidence(&cfg, &v.request_id),
        v.expect.fetches,
        "{}",
        v.name
    );
}

/// `{public_key_of:LABEL}` → that key's base64; returns the label of each key.
fn expand_keys(raw: &str) -> (String, BTreeMap<[u8; 32], String>) {
    let mut labels = BTreeMap::new();
    let mut out = String::new();
    let mut rest = raw;
    while let Some(i) = rest.find("{public_key_of:") {
        let j = rest[i..].find('}').expect("closing brace") + i;
        let label = &rest[i + "{public_key_of:".len()..j];
        let key = public_key_of(label);
        labels.insert(key, label.to_owned());
        out.push_str(&rest[..i]);
        out.push_str(&base64::engine::general_purpose::STANDARD.encode(key));
        rest = &rest[j + 1..];
    }
    out.push_str(rest);
    (out, labels)
}

fn run_config(v: &ConfigVector) {
    let got = match v.parse.as_str() {
        "signature_mode" => match parse_signature_mode(&v.raw) {
            Ok(m) => json!({"ok": true, "mode": m.as_str()}),
            Err(e) => json!({"ok": false, "error": e}),
        },
        "trusted_keys" => {
            let (raw, labels) = expand_keys(&v.raw);
            match parse_trusted_keys(&raw) {
                Ok(keys) => {
                    let keys: Vec<Value> = keys
                        .iter()
                        .map(|k| {
                            let label = labels
                                .get(&k.key)
                                .cloned()
                                .unwrap_or_else(|| "(not a vector key)".into());
                            json!({"id": k.id, "public_key_of": label})
                        })
                        .collect();
                    json!({"ok": true, "keys": keys})
                }
                Err(e) => json!({"ok": false, "error": e}),
            }
        }
        "allowed_namespaces" => json!({"ok": true, "namespaces": parse_allowed_namespaces(&v.raw)}),
        "manifest_base_url" => match parse_manifest_base_url(&v.raw) {
            Ok(u) => json!({"ok": true, "base_url": u.as_str()}),
            Err(e) => json!({"ok": false, "error": e}),
        },
        other => panic!("{}: unknown parse {other}", v.name),
    };
    match_decision(&v.name, &v.expect, got, "error_prefix", "error");
}

fn run_redirect(v: &RedirectVector) {
    let scheme = |u: &str| super::golang::url::parse(u).expect("vector URL").scheme;
    let via: Vec<String> = v.via.iter().map(|u| scheme(u)).collect();
    let via: Vec<&str> = via.iter().map(String::as_str).collect();
    let got = match redirect_allowed(&via, &scheme(&v.next)) {
        Ok(()) => json!({"allowed": true}),
        Err(e) => json!({"allowed": false, "error": e}),
    };
    assert_eq!(v.expect, got, "{}", v.name);
}

// ── the runner ───────────────────────────────────────────────────────────────

fn decode<T: serde::de::DeserializeOwned>(raw: &Value) -> T {
    serde_json::from_value(raw.clone()).unwrap_or_else(|e| panic!("vector {raw}: {e}"))
}

#[test]
fn every_trust_vector_passes_against_the_rust_port() {
    let mut files: Vec<PathBuf> = std::fs::read_dir(vector_dir())
        .expect("read the vector directory")
        .map(|e| e.expect("dir entry").path())
        .filter(|p| p.file_name().is_some_and(|n| n != "README.md"))
        .collect();
    files.sort();
    assert!(
        !files.is_empty(),
        "no vector files: the guard must never pass vacuously"
    );

    const KINDS: [&str; 6] = [
        "admit",
        "config",
        "evidence",
        "evidence_gate",
        "redirect",
        "verify_signature",
    ];
    let (mut total, mut ran) = (0, 0);
    let mut seen = HashSet::new();
    let mut per_kind: BTreeMap<String, usize> = BTreeMap::new();
    for path in &files {
        assert_eq!(
            path.extension().and_then(|e| e.to_str()),
            Some("json"),
            "{path:?}: only .json vector files belong here"
        );
        let file: VectorFile =
            serde_json::from_slice(&std::fs::read(path).expect("read")).expect("vector file");
        assert!(!file.vectors.is_empty(), "{path:?}: an empty vector file");
        total += file.vectors.len();
        let mut ran_in_file = 0;
        for raw in &file.vectors {
            let name = raw["name"].as_str().unwrap_or_default().to_owned();
            assert!(
                !name.is_empty() && seen.insert(format!("{}/{name}", file.kind)),
                "{path:?}: duplicate or empty name {name:?}"
            );
            match file.kind.as_str() {
                "admit" => {
                    let v: AdmitVector = decode(raw);
                    match v.caller.as_str() {
                        // `expect_without_caller_guard` here is Go's answer where the port
                        // admits what Go refuses; the port is held to `expect`.
                        "control_plane" => {
                            match_decision(
                                &v.name,
                                &v.expect,
                                run_admit(&v, Caller::ControlPlane),
                                "message_prefix",
                                "message",
                            );
                        }
                        "agent" => {
                            let without = v
                                .expect_without_caller_guard
                                .clone()
                                .expect("agent vectors say what Go does");
                            match_decision(
                                &v.name,
                                &v.expect,
                                run_admit(&v, Caller::Agent),
                                "message_prefix",
                                "message",
                            );
                            match_decision(
                                &v.name,
                                &without,
                                run_admit(&v, Caller::ControlPlane),
                                "message_prefix",
                                "message",
                            );
                        }
                        other => panic!("{}: unknown caller {other}", v.name),
                    }
                }
                "verify_signature" => run_verify(&decode(raw)),
                "evidence" => run_evidence(&decode(raw)),
                "evidence_gate" => run_gate(&decode(raw)),
                "config" => run_config(&decode(raw)),
                "redirect" => run_redirect(&decode(raw)),
                other => panic!(
                    "{path:?}: unknown vector kind {other:?} (both runners must know every kind)"
                ),
            }
            ran_in_file += 1;
        }
        assert_eq!(
            ran_in_file,
            file.vectors.len(),
            "{path:?}: every vector in the file ran"
        );
        *per_kind.entry(file.kind.clone()).or_default() += ran_in_file;
        ran += ran_in_file;
    }
    assert!(ran > 0, "no vectors ran");
    assert_eq!(ran, total);
    let kinds: Vec<&str> = per_kind.keys().map(String::as_str).collect();
    assert_eq!(kinds, KINDS, "every vector kind is present on disk and ran");
    eprintln!("ran {ran} trust vectors: {per_kind:?}");
}
