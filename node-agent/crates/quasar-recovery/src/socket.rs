//! The control socket's wire shapes (architecture §5.2).
//!
//! Not a frozen interface. The Go twin is `control-plane/internal/actorsocket`; both sides
//! decode and re-encode every fixture in `testdata/recovery/socket/` to the same JSON, so a
//! field added on one side and not the other fails a test on both. Field spellings of
//! [`AttemptResult`] are the Go updater's result file's (`updater/result.go`), so the
//! agent's `release_state` relay stays a re-frame.
//!
//! What the actor reads ([`Request`] and its parts) refuses unknown fields; what it writes
//! does not, because the actor moves first and an older reader must accept a newer one.

use std::fmt;

use serde::{Deserialize, Deserializer, Serialize, Serializer};

/// One image to move, as a request names it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Component {
    pub name: String,
    /// A repository reference: no tag, no digest.
    pub image: String,
    pub digest: String,
}

/// Provenance only (ADR 0001): nothing here is resolved or trusted, except that the ADR
/// 0003 verifier fetches the manifest named by `version`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Release {
    pub id: String,
    pub version: Option<String>,
    pub source_commit: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RequestKind {
    Replace,
    Restore,
    Remove,
}

/// `POST` body of a submit. Components are replaced in request order.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Request {
    pub request_id: String,
    pub kind: RequestKind,
    #[serde(deserialize_with = "null_as_empty")]
    pub components: Vec<Component>,
    pub release: Release,
    #[serde(default)]
    pub migrates: bool,
    #[serde(default)]
    pub schema_version: Option<i64>,
    #[serde(default)]
    pub external_backup_confirmed: bool,
    /// The dump a `restore` restores.
    #[serde(default)]
    pub dump: Option<String>,
    /// `remove` only: also delete the data.
    #[serde(default)]
    pub purge: bool,
    /// Zero means the actor's default; omitted when zero, as the Go updater's request is.
    #[serde(default, skip_serializing_if = "is_zero")]
    pub wait_timeout_s: i64,
}

fn is_zero(n: &i64) -> bool {
    *n == 0
}

/// A Go nil slice encodes as `null`; read it as empty. The field is still required.
fn null_as_empty<'de, D: Deserializer<'de>, T: Deserialize<'de>>(d: D) -> Result<Vec<T>, D::Error> {
    Ok(Option::<Vec<T>>::deserialize(d)?.unwrap_or_default())
}

/// The digest a component was on before, `null` (never omitted) when it could not be told.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Previous {
    pub name: String,
    pub digest: Option<String>,
}

/// The answer to an admitted submit.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Accepted {
    pub request_id: String,
    #[serde(deserialize_with = "null_as_empty")]
    pub previous: Vec<Previous>,
}

/// The answer to a refused submit. Nothing was journalled.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Rejection {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub request_id: String,
    pub reason: Reason,
    pub message: String,
}

/// agent-api.md `release_state` states.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum State {
    Pending,
    Pulling,
    Recreating,
    Verifying,
    Succeeded,
    Failed,
}

impl State {
    pub fn is_terminal(self) -> bool {
        matches!(self, State::Succeeded | State::Failed)
    }
}

/// One attempt's observable state. `reason` is non-null exactly when `state` is `failed`;
/// an interrupted attempt is `failed` with reason `interrupted` and `restored: false`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct AttemptResult {
    pub request_id: String,
    pub state: State,
    pub reason: Option<Reason>,
    #[serde(deserialize_with = "null_as_empty")]
    pub components: Vec<Component>,
    #[serde(deserialize_with = "null_as_empty")]
    pub previous: Vec<Previous>,
    pub output: String,
    pub started_at: String,
    pub updated_at: String,
    pub finished_at: Option<String>,
    pub restored: bool,
    pub release: Release,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MachineRole {
    Combined,
    Gpu,
    ControlOnly,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DatabaseMode {
    Owned,
    External,
    None,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ActorIdentity {
    pub version: String,
    pub commit: String,
    pub image: String,
    pub digest: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SeedIdentity {
    pub version: String,
    pub digest: Option<String>,
}

/// One platform service's container as the actor last inspected it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Service {
    /// `control-plane`, `node-agent`, `postgres` or `recovery-actor`.
    pub role: String,
    pub container: String,
    pub image: String,
    pub digest: Option<String>,
    /// The engine's container state (`running`, `exited`, ...).
    pub state: String,
    /// The engine's health status, `null` for a container with no healthcheck.
    pub health: Option<String>,
}

/// A container that looks like a platform service but lacks this installation's labels
/// (the race guard, [`crate::race_guard`]): reported, never acted on.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Conflict {
    /// Its name, as `docker ps` shows it.
    pub container: String,
    /// Its id, the engine's first 12 characters. Empty from an actor that predates it.
    #[serde(default)]
    pub id: String,
    /// Its configured image reference, tag or digest included.
    pub image: String,
    /// The role it looks like: `control-plane`, `node-agent`, `postgres`,
    /// `recovery-actor`, or `updater` (the Go updater of a Compose install). Empty from an
    /// actor that predates it.
    #[serde(default)]
    pub role: String,
    /// What gave it away, for the operator.
    pub why: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Dump {
    pub name: String,
    pub schema_version: i64,
    pub created_at: String,
    pub size_bytes: i64,
}

/// The machine inventory, plus one attempt's result when a request id was asked for.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Status {
    pub actor: ActorIdentity,
    pub seed: Option<SeedIdentity>,
    pub role: MachineRole,
    /// This machine's node name (machine inputs): on a combined host, the node name its
    /// own agent enrolls under, which is how the control plane knows which registered
    /// host shares its machine. `null` before the machine is installed.
    pub node_name: Option<String>,
    pub database: DatabaseMode,
    #[serde(deserialize_with = "null_as_empty")]
    pub services: Vec<Service>,
    #[serde(deserialize_with = "null_as_empty")]
    pub conflicts: Vec<Conflict>,
    pub in_flight: Option<String>,
    #[serde(deserialize_with = "null_as_empty")]
    pub dumps: Vec<Dump>,
    pub result: Option<AttemptResult>,
    /// True when the inventory is the last one because the engine was slow to answer.
    pub stale: bool,
}

/// The closed `reason` vocabulary of agent-api.md `release_state`, as far as the actor
/// emits it, plus the RH-06 identifiers of the RH06-01 draft amendment (#353). An
/// identifier this build does not know decodes to [`Reason::Other`] and re-encodes
/// verbatim, the contract's rule for an unknown identifier.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum Reason {
    Invalid,
    NamespaceRejected,
    DigestMalformed,
    Busy,
    PullFailed,
    RecreateFailed,
    NeverStarted,
    Unhealthy,
    SignatureMissing,
    SignatureInvalid,
    RecipeUnsupported,
    OwnerConflict,
    BackupFailed,
    BackupUnconfirmed,
    Interrupted,
    Other(String),
}

impl Reason {
    /// Refuse a submit before anything is journalled.
    pub const REJECTIONS: &'static [Reason] = &[
        Reason::Invalid,
        Reason::Busy,
        Reason::NamespaceRejected,
        Reason::DigestMalformed,
        Reason::SignatureMissing,
        Reason::SignatureInvalid,
        Reason::BackupUnconfirmed,
        Reason::OwnerConflict,
    ];

    /// End an admitted attempt in state `failed`.
    pub const FAILURES: &'static [Reason] = &[
        Reason::PullFailed,
        Reason::RecreateFailed,
        Reason::NeverStarted,
        Reason::Unhealthy,
        Reason::RecipeUnsupported,
        Reason::BackupFailed,
        Reason::Interrupted,
    ];

    /// Every identifier this build knows, in vocabulary order.
    pub const KNOWN: &'static [Reason] = &[
        Reason::Invalid,
        Reason::NamespaceRejected,
        Reason::DigestMalformed,
        Reason::Busy,
        Reason::PullFailed,
        Reason::RecreateFailed,
        Reason::NeverStarted,
        Reason::Unhealthy,
        Reason::SignatureMissing,
        Reason::SignatureInvalid,
        Reason::RecipeUnsupported,
        Reason::OwnerConflict,
        Reason::BackupFailed,
        Reason::BackupUnconfirmed,
        Reason::Interrupted,
    ];

    pub fn as_str(&self) -> &str {
        match self {
            Reason::Invalid => "invalid",
            Reason::NamespaceRejected => "namespace_rejected",
            Reason::DigestMalformed => "digest_malformed",
            Reason::Busy => "busy",
            Reason::PullFailed => "pull_failed",
            Reason::RecreateFailed => "recreate_failed",
            Reason::NeverStarted => "never_started",
            Reason::Unhealthy => "unhealthy",
            Reason::SignatureMissing => "signature_missing",
            Reason::SignatureInvalid => "signature_invalid",
            Reason::RecipeUnsupported => "recipe_unsupported",
            Reason::OwnerConflict => "owner_conflict",
            Reason::BackupFailed => "backup_failed",
            Reason::BackupUnconfirmed => "backup_unconfirmed",
            Reason::Interrupted => "interrupted",
            Reason::Other(s) => s,
        }
    }

    pub fn parse(s: &str) -> Reason {
        Reason::KNOWN
            .iter()
            .find(|r| r.as_str() == s)
            .cloned()
            .unwrap_or_else(|| Reason::Other(s.to_owned()))
    }
}

impl fmt::Display for Reason {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

impl Serialize for Reason {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for Reason {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(Reason::parse(&String::deserialize(d)?))
    }
}
