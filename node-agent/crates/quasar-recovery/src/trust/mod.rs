//! Release trust (#356): a behaviour-for-behaviour port of the Go updater's request gates
//! (`control-plane/internal/updater/plan.go` `Plan`, lines 158-256) and ADR 0003 signature
//! verification (`signature.go`, `signature_source.go`, and `server.go` `signatureEvidence`).
//!
//! "The same" is defined by `testdata/recovery/trust-vectors/`, which the Go package also
//! runs. A divergence from a vector is a bug here; a Go behaviour that looks wrong is
//! reported, not changed. [`admit`] is pure apart from the WARN it emits for an unverified
//! apply; the network is [`HttpsFetcher`]'s, which gathers the [`SignatureEvidence`].

mod golang;
mod https;
mod signature;
mod source;
#[cfg(test)]
mod vector_tests;

use std::fmt;
use std::sync::LazyLock;

use regex::Regex;

pub use https::HttpsFetcher;
pub use signature::{
    parse_signature_mode, parse_trusted_keys, SignatureEvidence, SignatureMode, SignaturePolicy,
    TrustedKey,
};
pub use source::{parse_manifest_base_url, parse_manifest_timeout, ManifestBaseUrl};

use crate::socket::{Reason, Request};
use golang::text::quote;

/// The only namespace a platform release comes from unless an operator says otherwise.
pub(crate) const DEFAULT_ALLOWED_NAMESPACES: &[&str] = &["ghcr.io/accreleus/quasar"];

/// The closed component table. The actor never accepts a request naming itself or
/// anything else (`quasar-updater`, `postgres`, ...): those are `invalid`.
const COMPONENTS: &[&str] = &["control-plane", "node-agent"];

/// What a request on the agent socket may name. A node agent asking to replace the
/// control plane is a confused deputy (agent-api.md `release_apply`). `recovery-actor` joins
/// both tables only with the RH06-01 contract amendment.
const AGENT_SOCKET_COMPONENTS: &[&str] = &["node-agent"];

/// Which socket a request arrived on (architecture §5.2). Authority follows the mount.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Caller {
    ControlPlane,
    Agent,
}

/// The host facts the decision needs; none of it comes from the request.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Config {
    /// `QUASAR_UPDATER_ALLOWED_NAMESPACES`, as [`parse_allowed_namespaces`] returns it.
    pub allowed_namespaces: Vec<String>,
    /// The id of the open request, if any. Single flight: refuse, never queue.
    pub in_flight_request_id: Option<String>,
    pub signature: SignaturePolicy,
}

/// One identifier from the closed `reason` vocabulary plus an operator-readable message.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Rejection {
    pub reason: Reason,
    pub message: String,
}

impl fmt::Display for Rejection {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.reason, self.message)
    }
}

impl std::error::Error for Rejection {}

fn reject(reason: Reason, message: String) -> Rejection {
    Rejection { reason, message }
}

/// An admitted request.
#[must_use]
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Admitted {
    /// The WARN line of an unverified apply under `verify`, already logged by [`admit`].
    pub unverified: Option<String>,
}

static UUID_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
        .expect("uuid regex")
});
static DIGEST_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^sha256:[0-9a-f]{64}$").expect("digest regex"));

/// The whole trust decision, in Go's order: request id, single flight, components (each
/// in request order: closed table, caller, duplicate, image, digest, namespace), then the
/// signature gate. Kind-specific rules for `restore`/`remove` are the actor's.
pub fn admit(
    caller: Caller,
    req: &Request,
    cfg: &Config,
    evidence: Option<&SignatureEvidence>,
) -> Result<Admitted, Rejection> {
    if !UUID_RE.is_match(&req.request_id) {
        return Err(reject(
            Reason::Invalid,
            format!("request_id {} is not a uuid", quote(&req.request_id)),
        ));
    }
    // Before the component rules, so a busy host answers `busy` rather than grading a
    // request it will not run. Re-posting the in-flight id is idempotent, not busy.
    if let Some(current) = in_flight_other_than(cfg, &req.request_id) {
        return Err(reject(
            Reason::Busy,
            format!("request {current} is still in flight"),
        ));
    }
    if req.components.is_empty() {
        return Err(reject(Reason::Invalid, "components is empty".into()));
    }
    let mut seen: Vec<&str> = Vec::with_capacity(req.components.len());
    for c in &req.components {
        let name = quote(&c.name);
        if !COMPONENTS.contains(&c.name.as_str()) {
            return Err(reject(Reason::Invalid, format!("unknown component {name}")));
        }
        if caller == Caller::Agent && !AGENT_SOCKET_COMPONENTS.contains(&c.name.as_str()) {
            return Err(reject(
                Reason::Invalid,
                format!("component {name} may not be named on the agent socket: a node agent asking to replace it is a confused deputy"),
            ));
        }
        if seen.contains(&c.name.as_str()) {
            return Err(reject(
                Reason::Invalid,
                format!("component {name} named twice"),
            ));
        }
        seen.push(&c.name);
        let image = quote(&c.image);
        // Only space, tab and newline, as Go's `strings.ContainsAny(image, " \t\n")`.
        if c.image.is_empty() || c.image.contains([' ', '\t', '\n']) {
            return Err(reject(
                Reason::Invalid,
                format!("component {name}: image {image} is not a repository reference"),
            ));
        }
        if image_has_tag_or_digest(&c.image) {
            return Err(reject(
                Reason::Invalid,
                format!("component {name}: image {image} carries a tag or a digest; it must be a bare repository reference"),
            ));
        }
        if !DIGEST_RE.is_match(&c.digest) {
            return Err(reject(
                Reason::DigestMalformed,
                format!(
                    "component {name}: digest {} is not sha256: + 64 lowercase hex",
                    quote(&c.digest)
                ),
            ));
        }
        if !namespace_allowed(&c.image, &cfg.allowed_namespaces) {
            return Err(reject(
                Reason::NamespaceRejected,
                format!(
                    "component {name}: image {image} is outside this host's platform-image namespaces ({})",
                    cfg.allowed_namespaces.join(",")
                ),
            ));
        }
    }
    // Last, because it grades a document fetched over the network.
    signature::check(req, &cfg.signature, evidence)
}

/// Whether the release assets are fetched for this request at all: only when signing is
/// on, and never while another request is in flight (a busy answer must not wait on the
/// network).
pub fn wants_signature_evidence(cfg: &Config, request_id: &str) -> bool {
    cfg.signature.enabled() && in_flight_other_than(cfg, request_id).is_none()
}

fn in_flight_other_than<'c>(cfg: &'c Config, request_id: &str) -> Option<&'c str> {
    cfg.in_flight_request_id
        .as_deref()
        .filter(|current| !current.is_empty() && *current != request_id)
}

/// `QUASAR_UPDATER_ALLOWED_NAMESPACES`: comma-separated, trimmed, trailing slashes dropped.
/// Blank is the default, never an empty allowlist.
pub fn parse_allowed_namespaces(raw: &str) -> Vec<String> {
    let out: Vec<String> = raw
        .split(',')
        .map(|part| golang::text::trim_space(part).trim_end_matches('/'))
        .filter(|ns| !ns.is_empty())
        .map(str::to_owned)
        .collect();
    if out.is_empty() {
        return DEFAULT_ALLOWED_NAMESPACES
            .iter()
            .map(|ns| (*ns).to_owned())
            .collect();
    }
    out
}

/// A byte-exact prefix on a path-segment boundary: `ghcr.io/accreleus/quasar` never
/// admits `ghcr.io/accreleus/quasar-evil/thing`, nor the namespace itself.
fn namespace_allowed(image: &str, allowed: &[String]) -> bool {
    allowed.iter().any(|ns| {
        image.len() > ns.len() + 1
            && image.starts_with(ns.as_str())
            && image.as_bytes()[ns.len()] == b'/'
    })
}

/// A tag is a `:` after the last `/` (`registry:5000/repo` is a port); a digest is any `@`.
fn image_has_tag_or_digest(image: &str) -> bool {
    if image.contains('@') {
        return true;
    }
    match image.rfind('/') {
        Some(i) => image[i + 1..].contains(':'),
        None => image.contains(':'),
    }
}
