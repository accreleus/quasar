//! ADR 0003 release signatures: port of `control-plane/internal/updater/signature.go`.
//!
//! What is signed is the release manifest's exact bytes; a detached document
//! (`scripts/release/platform-release-signature.md`) carries ed25519 signatures over them.

use ring::signature::{UnparsedPublicKey, ED25519};

use super::golang::base64::decode_std;
use super::golang::json::{decode_signature_document, unmarshal_signed_manifest, DocumentError};
use super::golang::text::{quote, quote_bytes, to_lower_ascii_keyword, trim_space};
use super::{reject, Admitted, Rejection};
use crate::socket::{Reason, Request};

/// The only algorithm this build verifies. Entries in any other are skipped.
pub(crate) const SIGNATURE_ALGORITHM: &str = "ed25519";

/// The version of the `.sig` envelope, not of the manifest or the release.
pub(crate) const SIGNATURE_DOCUMENT_FORMAT_VERSION: i64 = 1;

const PUBLIC_KEY_LEN: usize = 32;
const SIGNATURE_LEN: usize = 64;

/// `QUASAR_UPDATER_SIGNATURE_MODE`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum SignatureMode {
    /// Fetches and grades nothing: ADR 0001 trust, digest and namespace.
    #[default]
    Off,
    /// Refuses a bad signature, applies a release that definitively has none (and warns).
    /// Not an enforcement boundary: the requester chooses the version looked up.
    Verify,
    /// Also refuses a release with no signature.
    Require,
}

impl SignatureMode {
    pub fn as_str(self) -> &'static str {
        match self {
            SignatureMode::Off => "off",
            SignatureMode::Verify => "verify",
            SignatureMode::Require => "require",
        }
    }
}

/// One public key this host trusts. `id` is a label: it orders the attempts and names
/// the key in messages, and is never what makes a signature good.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TrustedKey {
    pub id: String,
    pub key: [u8; PUBLIC_KEY_LEN],
}

/// The host's whole answer to "must a release be signed".
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SignaturePolicy {
    pub mode: SignatureMode,
    pub keys: Vec<TrustedKey>,
}

impl SignaturePolicy {
    /// Whether anything is fetched or graded at all.
    pub fn enabled(&self) -> bool {
        self.mode != SignatureMode::Off
    }
}

/// Key labels for messages, never key material.
fn key_ids(keys: &[TrustedKey]) -> Vec<String> {
    keys.iter().map(|k| key_label(&k.id).to_owned()).collect()
}

fn key_label(id: &str) -> &str {
    if id.is_empty() {
        "(unlabelled)"
    } else {
        id
    }
}

/// What the verifier found for one release. "Could not tell" is its own state and must
/// never fold into "absent": a proxy eating the request would read as unsigned.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SignatureEvidence {
    Signed {
        manifest: Vec<u8>,
        signature: Vec<u8>,
    },
    /// There is definitively no signature; `why` says so and names the version.
    Absent { why: String },
    /// The question could not be answered. Refused in both enabled modes.
    FetchError { error: String },
}

/// The signature gate; `evidence` is `None` when none was gathered.
pub(super) fn check(
    req: &Request,
    pol: &SignaturePolicy,
    evidence: Option<&SignatureEvidence>,
) -> Result<Admitted, Rejection> {
    if !pol.enabled() {
        return Ok(Admitted { unverified: None });
    }
    // Fail closed: verifying against no keys would look on while checking nothing.
    if pol.keys.is_empty() {
        return Err(reject(
            Reason::SignatureInvalid,
            format!(
                "signature mode is {} but this host trusts no release keys: set QUASAR_UPDATER_TRUSTED_KEYS (docs/upgrading.md)",
                quote(pol.mode.as_str())
            ),
        ));
    }
    let Some(evidence) = evidence else {
        return Err(reject(
            Reason::SignatureInvalid,
            "no signature evidence was gathered for this request".into(),
        ));
    };
    match evidence {
        SignatureEvidence::FetchError { error } => Err(reject(
            Reason::SignatureInvalid,
            format!(
                "the release signature could not be retrieved, so it could not be checked: {error}"
            ),
        )),
        SignatureEvidence::Absent { why } if pol.mode == SignatureMode::Require => Err(reject(
            Reason::SignatureMissing,
            format!("this host requires a signed release and this one carries no signature: {why}"),
        )),
        SignatureEvidence::Absent { why } => {
            // The verify bypass (ADR 0003): logged every time, naming the version via `why`.
            let msg = format!(
                "applying an UNVERIFIED release: {why} (mode=verify accepts this; mode=require would refuse it)"
            );
            tracing::warn!(token = "release-apply-unverified", "{msg}");
            Ok(Admitted {
                unverified: Some(msg),
            })
        }
        SignatureEvidence::Signed {
            manifest,
            signature,
        } => {
            let key_id =
                verify_manifest_signature(manifest, signature, &pol.keys).map_err(|e| {
                    reject(
                        Reason::SignatureInvalid,
                        format!("the release manifest signature did not verify: {e}"),
                    )
                })?;
            // Good signature over some manifest; it must also be over this request.
            bind_manifest(manifest, req).map_err(|e| {
                reject(
                    Reason::SignatureInvalid,
                    format!(
                        "the signed release manifest (key {}) does not describe this request: {e}",
                        key_label(&key_id)
                    ),
                )
            })?;
            Ok(Admitted { unverified: None })
        }
    }
}

/// Checks the detached document over the manifest bytes and returns the id of the
/// trusted key that verified it. Any entry under any trusted key is enough; `key_id` only
/// decides which key is tried first.
pub(crate) fn verify_manifest_signature(
    manifest: &[u8],
    document: &[u8],
    keys: &[TrustedKey],
) -> Result<String, String> {
    if keys.is_empty() {
        return Err("no trusted keys".into());
    }
    let doc = decode_signature_document(document).map_err(|e| match e {
        DocumentError::Json(e) => {
            format!("the signature document is not valid JSON in the documented shape: {e}")
        }
        DocumentError::TrailingContent => {
            "the signature document carries trailing content after the object".into()
        }
    })?;
    if doc.format_version != SIGNATURE_DOCUMENT_FORMAT_VERSION {
        return Err(format!(
            "signature document format_version {} is not understood by this build (want {SIGNATURE_DOCUMENT_FORMAT_VERSION})",
            doc.format_version
        ));
    }
    let entries = doc.signatures.as_slice();
    if entries.is_empty() {
        return Err("the signature document carries no signatures".into());
    }
    let mut usable = 0;
    for entry in entries {
        if entry.algorithm != SIGNATURE_ALGORITHM {
            continue;
        }
        let Ok(sig) = decode_std(trim_space(&entry.signature).as_bytes()) else {
            continue;
        };
        if sig.len() != SIGNATURE_LEN {
            continue;
        }
        usable += 1;
        for k in order_keys(keys, &entry.key_id) {
            if UnparsedPublicKey::new(&ED25519, &k.key)
                .verify(manifest, &sig)
                .is_ok()
            {
                return Ok(k.id.clone());
            }
        }
    }
    if usable == 0 {
        return Err(format!(
            "no {SIGNATURE_ALGORITHM} signature in the document is well-formed"
        ));
    }
    Err(format!(
        "no signature was made by a key this host trusts ({})",
        key_ids(keys).join(", ")
    ))
}

/// The label-matching keys first, then the rest; every key is tried.
fn order_keys<'k>(keys: &'k [TrustedKey], hint: &str) -> Vec<&'k TrustedKey> {
    if hint.is_empty() {
        return keys.iter().collect();
    }
    let (mut first, rest): (Vec<_>, Vec<_>) = keys.iter().partition(|k| k.id == hint);
    first.extend(rest);
    first
}

/// The signed manifest must name the version, images and digests this request asks
/// for, or one genuine release would launder any digest set.
fn bind_manifest(raw: &[u8], req: &Request) -> Result<(), String> {
    let m = unmarshal_signed_manifest(raw)
        .map_err(|e| format!("the signed manifest is not readable JSON: {e}"))?;
    let want = trim_space(req.release.version.as_deref().unwrap_or(""));
    if !want.is_empty() && m.version != want {
        return Err(format!(
            "it names version {}, the request names {}",
            quote(&m.version),
            quote(want)
        ));
    }
    let components = m.components.as_slice();
    if components.is_empty() {
        return Err("it names no components".into());
    }
    for c in &req.components {
        let mut found = false;
        for mc in components.iter().filter(|mc| mc.name == c.name) {
            found = true;
            if mc.image != c.image || mc.digest != c.digest {
                return Err(format!(
                    "component {} is {}@{} in the request and {}@{} in the manifest",
                    quote(&c.name),
                    c.image,
                    c.digest,
                    mc.image,
                    mc.digest
                ));
            }
        }
        if !found {
            return Err(format!(
                "component {} is not in the manifest",
                quote(&c.name)
            ));
        }
    }
    Ok(())
}

/// `QUASAR_UPDATER_SIGNATURE_MODE`. Blank is off; anything unrecognised is an error,
/// never a fallback to off.
pub fn parse_signature_mode(raw: &str) -> Result<SignatureMode, String> {
    match to_lower_ascii_keyword(trim_space(raw)).as_str() {
        "" | "off" => Ok(SignatureMode::Off),
        "verify" => Ok(SignatureMode::Verify),
        "require" => Ok(SignatureMode::Require),
        _ => Err(format!(
            "{} is not a signature mode (want off, verify or require)",
            quote(raw)
        )),
    }
}

/// `QUASAR_UPDATER_TRUSTED_KEYS`: comma-separated `key-id:base64key` or bare `base64key`,
/// each the raw 32 bytes of an ed25519 public key. The first colon separates. One bad
/// entry fails the whole list; the same key twice is kept once, with its first label.
pub fn parse_trusted_keys(raw: &str) -> Result<Vec<TrustedKey>, String> {
    let mut out: Vec<TrustedKey> = Vec::new();
    for part in raw.split(',') {
        let part = trim_space(part);
        if part.is_empty() {
            continue;
        }
        let (id, encoded) = match part.split_once(':') {
            Some((id, encoded)) => (trim_space(id), trim_space(encoded)),
            None => ("", part),
        };
        let key = decode_std(encoded.as_bytes()).map_err(|offset| {
            format!(
                "trusted key {} is not standard base64: illegal base64 data at input byte {offset}",
                label_of(id, encoded)
            )
        })?;
        let key: [u8; PUBLIC_KEY_LEN] = key.as_slice().try_into().map_err(|_| {
            format!(
                "trusted key {} decodes to {} bytes, not the {PUBLIC_KEY_LEN} of an ed25519 public key",
                label_of(id, encoded),
                key.len()
            )
        })?;
        if out.iter().any(|k| k.key == key) {
            continue;
        }
        out.push(TrustedKey {
            id: id.to_owned(),
            key,
        });
    }
    Ok(out)
}

/// The quoted label a refusal names a key by: its id, or its first twelve bytes.
fn label_of(id: &str, encoded: &str) -> String {
    if !id.is_empty() {
        return quote(id);
    }
    let b = encoded.as_bytes();
    if b.len() > 12 {
        let mut label = b[..12].to_vec();
        label.extend_from_slice("\u{2026}".as_bytes());
        return quote_bytes(&label);
    }
    quote(encoded)
}
