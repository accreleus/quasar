//! Durable RH05 next-session policy acceptance for every next-session group
//! (`policy_catalog::NEXT_SESSION_GROUPS`). The journal is committed before
//! mutating the session settings snapshot.

use serde::{Deserialize, Serialize};
use serde_json::{json, Map, Value};
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use time::{format_description::well_known::Rfc3339, OffsetDateTime};

use crate::messages::AgentMsg;
use crate::messages::{GpuCapacity, ReadinessCheck};
use crate::policy_catalog::{self, group_for_key};
use crate::session::settings::RuntimeSettings;

const IDLE: &str = "idle_timeout_secs";

/// Rejection codes that describe the offer itself, so the control plane never
/// retries them; every other code is transient. Twin of
/// `hostcfg.policyRejectionInvalid`.
pub const INVALID_REJECTIONS: &[&str] = &[
    "content_mismatch",
    "cross_key_invalid",
    "home_root_outside_mount",
    "invalid_content",
    "invalid_revision",
    "invalid_value",
    "missing_setting",
    "path_inaccessible",
    "resolved_mismatch",
    "revision_conflict",
    "unsupported_group",
    "unsupported_source",
];

#[derive(Debug, Clone, Deserialize)]
pub struct Offer {
    pub attempt_id: String,
    pub host_id: String,
    pub boot_incarnation: String,
    pub connection_incarnation: String,
    pub group: String,
    pub revision: String,
    pub content_sha256: String,
    pub scope: String,
    pub expires_at: String,
    pub prerequisites_sha256: String,
    pub prerequisites: Vec<Value>,
    pub settings: Value,
    pub resolved_settings: Value,
}

pub struct HardwareEvidence<'a> {
    pub gpus: &'a [GpuCapacity],
    pub readiness: &'a [ReadinessCheck],
}

fn idle_group() -> String {
    IDLE.into()
}

fn next_session_scope() -> String {
    "next_session".into()
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct Record {
    #[serde(default)]
    host_id: String,
    #[serde(default = "idle_group")]
    group: String,
    #[serde(default = "next_session_scope")]
    scope: String,
    revision: String,
    digest: String,
    phase: String,
    sequence: u64,
    /// Filled from `resolved_idle_timeout_secs` when loading a #335 journal.
    #[serde(default)]
    resolved_settings: Value,
    /// #335 shape, still written for idle records so an older binary can load
    /// an idle-only journal; one holding any other group fails its parse closed.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    resolved_idle_timeout_secs: Option<u64>,
    boot_incarnation: String,
    connection_incarnation: String,
    #[serde(default)]
    settings: Value,
    #[serde(default)]
    known_good: Option<ActiveGroup>,
    #[serde(default)]
    error: Option<String>,
    #[serde(default)]
    verified_at: Option<String>,
    #[serde(default)]
    verified_process_id: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct ActiveGroup {
    kind: String,
    digest: String,
    resolved_settings: Value,
}

#[derive(Debug, Default, Clone, Serialize, Deserialize)]
struct Journal {
    records: BTreeMap<String, Record>,
    #[serde(default)]
    high_water: BTreeMap<String, u64>,
    // The four #335 idle fields are read on load and mirrored on write
    // (`Journal::load` / `Journal::on_disk`); in memory only the generic maps count.
    #[serde(default)]
    high_water_idle_timeout: u64,
    #[serde(default)]
    active_idle_timeout_secs: Option<u64>,
    #[serde(default)]
    legacy_overlay: Option<Value>,
    #[serde(default)]
    legacy_delivery_id: Option<String>,
    #[serde(default)]
    ever_accepted_typed: BTreeSet<String>,
    #[serde(default)]
    active_groups: BTreeMap<String, ActiveGroup>,
    #[serde(default)]
    seeded_idle_timeout_secs: Option<u64>,
    #[serde(default)]
    verified_idle_digest: Option<String>,
}

impl Journal {
    fn load(bytes: &[u8]) -> serde_json::Result<Self> {
        let mut journal: Journal = serde_json::from_slice(bytes)?;
        for record in journal.records.values_mut() {
            if record.resolved_settings.is_null() {
                if let Some(timeout) = record.resolved_idle_timeout_secs {
                    record.resolved_settings = json!({ IDLE: timeout });
                }
            }
        }
        if journal.high_water_idle_timeout > 0 {
            let high = journal.high_water.entry(IDLE.into()).or_default();
            *high = (*high).max(journal.high_water_idle_timeout);
        }
        if !journal.active_groups.contains_key(IDLE) {
            let timeout = journal
                .active_idle_timeout_secs
                .or(journal.seeded_idle_timeout_secs);
            if let Some(timeout) = timeout {
                let resolved = json!({ IDLE: timeout });
                let (kind, digest) = match &journal.verified_idle_digest {
                    Some(digest) => ("verified", digest.clone()),
                    None => ("seeded", policy_catalog::snapshot_digest(IDLE, &resolved)),
                };
                journal.active_groups.insert(
                    IDLE.into(),
                    ActiveGroup {
                        kind: kind.into(),
                        digest,
                        resolved_settings: resolved,
                    },
                );
            }
        }
        Ok(journal)
    }

    fn on_disk(&self) -> Journal {
        let mut disk = self.clone();
        disk.high_water_idle_timeout = self.high_water.get(IDLE).copied().unwrap_or(0);
        let idle = self.active_groups.get(IDLE);
        disk.active_idle_timeout_secs = idle
            .and_then(|active| active.resolved_settings.get(IDLE))
            .and_then(Value::as_u64);
        disk.verified_idle_digest = idle
            .filter(|active| active.kind == "verified")
            .map(|active| active.digest.clone());
        if idle.is_some_and(|active| active.kind == "seeded") {
            disk.seeded_idle_timeout_secs = disk.active_idle_timeout_secs;
        }
        disk
    }

    /// Groups the agent must advertise: every sticky group and every durable seed.
    fn groups(&self) -> BTreeSet<String> {
        let mut groups = self.ever_accepted_typed.clone();
        groups.extend(self.active_groups.keys().cloned());
        if self.seeded_idle_timeout_secs.is_some() {
            groups.insert(IDLE.into());
        }
        groups
    }
}

struct InventorySnapshot {
    inventory_id: String,
    snapshot_id: String,
    entries: Vec<Value>,
    high_water: BTreeMap<String, String>,
    active_snapshots: BTreeMap<String, Value>,
}

/// The one durable restart transition consumed before any runtime startup.
/// `Recovery` requests one fresh process for the known-good snapshot.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum BootOutcome {
    None,
    Candidate(String),
    Recovery(String),
    RecoveryVerify(String),
    Uncertain(String),
}

/// Keeps an unsafe journal payload distinct from a storage failure while
/// startup admission is held. A write failure must never suggest replacing a
/// valid journal with an older backup.
#[derive(Debug)]
pub enum BootError {
    Read(std::io::Error),
    Invalid(String),
    Write(std::io::Error),
}

impl std::fmt::Display for BootError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Read(err) => write!(f, "journal read failed: {err}"),
            Self::Invalid(err) => write!(f, "journal content invalid: {err}"),
            Self::Write(err) => write!(f, "journal write failed: {err}"),
        }
    }
}

/// A consumed restart marker has selected one exact group snapshot. The caller
/// must prove this composed runtime against fresh host evidence before it can
/// become an active snapshot or be reported to the control plane.
pub enum BootPreparation {
    Complete(BootOutcome),
    Verify(Box<BootReadback>),
}

pub struct BootReadback {
    attempt_id: String,
    recovering: bool,
    settings: RuntimeSettings,
}

impl BootReadback {
    pub fn settings(&self) -> &RuntimeSettings {
        &self.settings
    }
}

pub struct PolicyAgent {
    path: PathBuf,
    host_id: String,
    boot_incarnation: String,
    connection_incarnation: String,
    journal: Journal,
    deployment_baseline: RuntimeSettings,
    inventory: Option<InventorySnapshot>,
    /// Crash injection: the Nth durable write lands and then reports failure,
    /// as if the process died right after its fsync.
    #[cfg(test)]
    crash_after_writes: std::sync::atomic::AtomicUsize,
}

type ApplyOutcome = (&'static str, u64, Option<&'static str>, Option<Value>);

impl PolicyAgent {
    pub fn has_open_restart(&self) -> bool {
        self.journal.records.values().any(|record| {
            record.scope == "restart"
                && matches!(
                    record.phase.as_str(),
                    "accepted"
                        | "activating"
                        | "awaiting_startup"
                        | "verifying"
                        | "failed"
                        | "recovery_verifying"
                        | "recovery_awaiting_startup"
                        | "uncertain"
                )
        })
    }

    pub fn has_uncertain_restart(&self) -> bool {
        self.journal
            .records
            .values()
            .any(|record| record.scope == "restart" && record.phase == "uncertain")
    }
    pub fn bootstrap(path: &Path) -> Result<BootOutcome, BootError> {
        let bytes = match fs::read(path) {
            Ok(bytes) => bytes,
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(BootOutcome::None),
            Err(err) => return Err(BootError::Read(err)),
        };
        let mut journal =
            Journal::load(&bytes).map_err(|err| BootError::Invalid(err.to_string()))?;
        let open: Vec<String> = journal
            .records
            .iter()
            .filter(|(_, record)| {
                record.scope == "restart"
                    && matches!(
                        record.phase.as_str(),
                        "accepted"
                            | "activating"
                            | "awaiting_startup"
                            | "verifying"
                            | "failed"
                            | "recovery_verifying"
                            | "recovery_awaiting_startup"
                            | "uncertain"
                    )
            })
            .map(|(id, _)| id.clone())
            .collect();
        if open.len() > 1 {
            return Err(BootError::Invalid("multiple open restart attempts".into()));
        }
        let Some(id) = open.into_iter().next() else {
            return Ok(BootOutcome::None);
        };
        let record = journal.records.get_mut(&id).unwrap();
        let outcome = match record.phase.as_str() {
            "awaiting_startup" => {
                record.phase = "verifying".into();
                record.sequence += 1;
                BootOutcome::Candidate(id)
            }
            "accepted" | "activating" | "verifying" | "failed" => {
                if record.known_good.is_none() {
                    record.phase = "uncertain".into();
                    record.sequence += 1;
                    BootOutcome::Uncertain(id)
                } else {
                    if record.phase != "failed" {
                        record.error = Some("candidate_startup_interrupted".into());
                        record.phase = "failed".into();
                        record.sequence += 1;
                    }
                    record.phase = "recovery_verifying".into();
                    record.sequence += 1;
                    persist_journal(path, &journal).map_err(BootError::Write)?;
                    record_restart_phase(path, &mut journal, &id, "recovery_awaiting_startup")
                        .map_err(BootError::Write)?;
                    return Ok(BootOutcome::Recovery(id));
                }
            }
            "recovery_awaiting_startup" => {
                record.phase = "recovery_verifying".into();
                record.sequence += 1;
                BootOutcome::RecoveryVerify(id)
            }
            "recovery_verifying" => {
                record.phase = "uncertain".into();
                record.sequence += 1;
                BootOutcome::Uncertain(id)
            }
            "uncertain" => BootOutcome::Uncertain(id),
            _ => BootOutcome::None,
        };
        persist_journal(path, &journal).map_err(BootError::Write)?;
        Ok(outcome)
    }
    pub fn advertised_groups(path: &PathBuf) -> Vec<String> {
        match fs::read(path).ok().and_then(|raw| Journal::load(&raw).ok()) {
            Some(journal) => journal.groups().into_iter().collect(),
            _ => Vec::new(),
        }
    }

    /// A durable seed or sticky group is missing from `advertised`: a fresh
    /// registration would advertise more than this connection did.
    pub fn has_unadvertised_groups(&self, advertised: &[String]) -> bool {
        self.journal
            .groups()
            .iter()
            .any(|group| !advertised.contains(group))
    }

    pub fn confirm_groups(
        &mut self,
        advertised: &[String],
        accepted: &[String],
    ) -> Result<(), String> {
        if accepted.windows(2).any(|pair| pair[0] >= pair[1])
            || accepted.iter().any(|group| !advertised.contains(group))
        {
            return Err("ownership_echo_invalid".into());
        }
        let prior = self.journal.clone();
        for group in accepted {
            self.journal.ever_accepted_typed.insert(group.clone());
        }
        if let Err(err) = self.persist() {
            self.journal = prior;
            return Err(format!("ownership_journal_write_failed: {err}"));
        }
        Ok(())
    }

    pub fn legacy_map_applied_id(&self) -> Option<String> {
        self.journal.legacy_delivery_id.clone()
    }

    pub fn has_sticky_ownership(&self) -> bool {
        !self.journal.ever_accepted_typed.is_empty()
    }
    pub fn has_seed(&self) -> bool {
        self.journal
            .active_groups
            .values()
            .any(|active| active.kind == "seeded")
            || self.journal.seeded_idle_timeout_secs.is_some()
    }
    pub fn has_idle_seed(&self) -> bool {
        self.has_seed()
    }
    pub fn sticky_groups_accepted(&self, accepted: &[String]) -> bool {
        self.journal
            .ever_accepted_typed
            .iter()
            .all(|group| accepted.contains(group))
    }
    pub fn connection_incarnation(&self) -> &str {
        &self.connection_incarnation
    }

    pub fn apply_legacy_overlay(
        &mut self,
        baseline: &RuntimeSettings,
        settings: &mut RuntimeSettings,
        map: &Value,
        delivery_id: Option<&str>,
    ) -> Result<Option<String>, String> {
        let object = map.as_object().ok_or("invalid_legacy_map")?;
        let mut filtered = object.clone();
        let mut conflict = None;
        filtered.retain(|key, _| {
            let group = group_for_key(key);
            let owned = self.journal.ever_accepted_typed.contains(group);
            if owned && conflict.is_none() {
                conflict = Some(group.to_string());
            }
            !owned
        });
        let filtered = Value::Object(filtered);
        if self.journal.legacy_delivery_id.as_deref() == delivery_id
            && self.journal.legacy_overlay.as_ref() == Some(&filtered)
        {
            return Ok(conflict);
        }
        if delivery_id.is_some() && self.journal.legacy_delivery_id.as_deref() == delivery_id {
            return Err("attempt_conflict".into());
        }
        let mut composed = baseline.clone();
        composed.apply_json(&filtered);
        self.compose_typed_snapshots(&mut composed)?;
        let previous = self.journal.clone();
        self.journal.legacy_overlay = Some(filtered);
        self.journal.legacy_delivery_id = delivery_id.map(str::to_string);
        // A seed records what this process actually latched. It is an active
        // recovery target, never RH05 application proof; owned groups keep theirs.
        let latched = composed.deployment_map();
        for group in policy_catalog::NEXT_SESSION_GROUPS {
            let owned = self.journal.ever_accepted_typed.contains(*group);
            let verified = self
                .journal
                .active_groups
                .get(*group)
                .is_some_and(|active| active.kind != "seeded");
            let Some(value) = latched.get(*group).filter(|_| !owned && !verified) else {
                continue;
            };
            let resolved = json!({ *group: value });
            self.journal.active_groups.insert(
                group.to_string(),
                ActiveGroup {
                    kind: "seeded".into(),
                    digest: policy_catalog::snapshot_digest(group, &resolved),
                    resolved_settings: resolved,
                },
            );
        }
        if !self.journal.ever_accepted_typed.contains("hardware")
            && !self.journal.active_groups.contains_key("hardware")
        {
            let resolved = Value::Object(Map::from_iter(
                policy_catalog::HARDWARE_KEYS.iter().filter_map(|key| {
                    latched
                        .get(*key)
                        .cloned()
                        .map(|value| ((*key).into(), value))
                }),
            ));
            if resolved.as_object().is_some_and(|values| values.len() == 3) {
                self.journal.active_groups.insert(
                    "hardware".into(),
                    ActiveGroup {
                        kind: "seeded".into(),
                        digest: policy_catalog::snapshot_digest("hardware", &resolved),
                        resolved_settings: resolved,
                    },
                );
            }
        }
        if let Err(err) = self.persist() {
            self.journal = previous;
            return Err(format!("legacy_journal_write_failed: {err}"));
        }
        *settings = composed;
        Ok(conflict)
    }

    fn compose_typed_snapshots(&self, settings: &mut RuntimeSettings) -> Result<(), String> {
        let mut snapshots = Vec::new();
        for group in &self.journal.ever_accepted_typed {
            let active = self
                .journal
                .active_groups
                .get(group)
                .ok_or("typed_snapshot_missing")?;
            snapshots.push((group.as_str(), &active.resolved_settings));
        }
        policy_catalog::compose_groups(settings, snapshots).map_err(str::to_string)
    }

    pub fn inventory_page(
        &mut self,
        inventory_id: &str,
        boot: &str,
        connection: &str,
        cursor: Option<&str>,
    ) -> Result<AgentMsg, String> {
        if boot != self.boot_incarnation || connection != self.connection_incarnation {
            return Err("stale_inventory_request".into());
        }
        if cursor.is_none()
            && self
                .inventory
                .as_ref()
                .is_none_or(|snapshot| snapshot.inventory_id != inventory_id)
        {
            let snapshot_id = fs::read_to_string("/proc/sys/kernel/random/uuid")
                .map_err(|_| "snapshot_id_unavailable")?
                .trim()
                .to_string();
            let mut high_water: BTreeMap<String, String> = self
                .journal
                .high_water
                .iter()
                .map(|(group, revision)| (group.clone(), revision.to_string()))
                .collect();
            // #335 always reported the idle entry, even at "0".
            high_water.entry(IDLE.into()).or_insert_with(|| "0".into());
            let active_snapshots = self
                .journal
                .active_groups
                .iter()
                .map(|(group, active)| {
                    (
                        group.clone(),
                        json!({"kind":active.kind,"digest":active.digest}),
                    )
                })
                .collect();
            let entries = self.journal.records.iter().map(|(attempt_id, record)| {
                let applied = record.phase == "applied";
                json!({
                    "attempt_id":attempt_id,
                    "host_id":if record.host_id.is_empty() {&self.host_id} else {&record.host_id},
                    "group":record.group,
                    "revision":record.revision,
                    "content_sha256":record.digest,
                    "scope":record.scope,
                    "grant_boot_incarnation":record.boot_incarnation,
                    "grant_connection_incarnation":record.connection_incarnation,
                    "journal_sequence":record.sequence.to_string(),
                    "phase":record.phase,
                    "active_scope":if applied {Some(record.scope.as_str())} else {None},
                    "evidence":if applied {Some(json!({
                        "revision":record.revision,
                        "content_sha256":record.digest,
                        "resolved_settings":record.resolved_settings,
                        "agent_process_id":record.verified_process_id.clone().unwrap_or_else(||std::process::id().to_string()),
                        "observed_at":record.verified_at.clone().unwrap_or_else(||OffsetDateTime::now_utc().format(&Rfc3339).unwrap_or_default()),
                        "evidence_ids":[]
                    }))} else {None},
                    "error":record.error
                })
            }).collect();
            self.inventory = Some(InventorySnapshot {
                inventory_id: inventory_id.to_string(),
                snapshot_id,
                entries,
                high_water,
                active_snapshots,
            });
        }
        let snapshot = self
            .inventory
            .as_ref()
            .ok_or("inventory_snapshot_missing")?;
        if snapshot.inventory_id != inventory_id {
            return Err("inventory_id_mismatch".into());
        }
        let start = match cursor {
            Some(value) => value
                .parse::<usize>()
                .map_err(|_| "invalid_inventory_cursor")?,
            None => 0,
        };
        if start > snapshot.entries.len() || start % 256 != 0 {
            return Err("invalid_inventory_cursor".into());
        }
        let end = (start + 256).min(snapshot.entries.len());
        Ok(AgentMsg::ConfigPolicyJournalInventoryPage {
            inventory_id: snapshot.inventory_id.clone(),
            snapshot_id: snapshot.snapshot_id.clone(),
            cursor: cursor.map(str::to_string),
            next_cursor: (end < snapshot.entries.len()).then(|| end.to_string()),
            revision_high_water: snapshot.high_water.clone(),
            active_snapshots: snapshot.active_snapshots.clone(),
            entries: snapshot.entries[start..end].to_vec(),
        })
    }

    pub fn open(
        path: PathBuf,
        host_id: String,
        boot_incarnation: String,
        connection_incarnation: String,
        settings: &mut RuntimeSettings,
    ) -> std::io::Result<Self> {
        let deployment_baseline = settings.clone();
        let journal = match fs::read(&path) {
            Ok(bytes) => Journal::load(&bytes).map_err(std::io::Error::other)?,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Journal::default(),
            Err(e) => return Err(e),
        };
        if let Some(overlay) = &journal.legacy_overlay {
            settings.apply_json(overlay);
        }
        let mut agent = Self {
            path,
            host_id,
            boot_incarnation,
            connection_incarnation,
            journal,
            deployment_baseline,
            inventory: None,
            #[cfg(test)]
            crash_after_writes: Default::default(),
        };
        // A crash after durable acceptance but before activation cannot wait
        // for a new offer: admission is still gated on complete inventory.
        // Next-session activation is local and safe to finish idempotently.
        let mut accepted: BTreeMap<String, Vec<String>> = BTreeMap::new();
        for (id, record) in &agent.journal.records {
            if record.phase == "accepted" && record.scope == "next_session" {
                accepted
                    .entry(record.group.clone())
                    .or_default()
                    .push(id.clone());
            }
        }
        let mut recovered = false;
        for (group, ids) in accepted {
            if ids.len() != 1 || !agent.journal.ever_accepted_typed.contains(&group) {
                continue;
            }
            let record = agent.journal.records.get_mut(&ids[0]).unwrap();
            record.phase = "applied".into();
            record.sequence += 1;
            let active = ActiveGroup {
                kind: "verified".into(),
                digest: record.digest.clone(),
                resolved_settings: record.resolved_settings.clone(),
            };
            agent.journal.active_groups.insert(group, active);
            recovered = true;
        }
        if recovered {
            agent.persist()?;
        }
        agent
            .compose_typed_snapshots(settings)
            .map_err(std::io::Error::other)?;
        Ok(agent)
    }

    /// A fresh process may finish the marker consumed by `bootstrap` only
    /// after its startup baseline and durable overlay have been loaded. A
    /// reconnect in the same process sees the already terminal record.
    pub fn prepare_boot(
        &mut self,
        outcome: &BootOutcome,
        settings: &RuntimeSettings,
    ) -> std::io::Result<BootPreparation> {
        let (id, recovering) = match outcome {
            BootOutcome::Candidate(id) => (id, false),
            BootOutcome::RecoveryVerify(id) => (id, true),
            other => return Ok(BootPreparation::Complete(other.clone())),
        };
        let Some(record) = self.journal.records.get(id).cloned() else {
            return Err(std::io::Error::other("boot attempt disappeared"));
        };
        let expected_phase = if recovering {
            "recovery_verifying"
        } else {
            "verifying"
        };
        if record.phase != expected_phase {
            return Ok(BootPreparation::Complete(BootOutcome::None));
        }
        if !recovering {
            let baseline = self.deployment_baseline.deployment_map();
            let changed = record.settings.as_object().is_some_and(|choices| {
                choices.iter().any(|(key, choice)| {
                    choice["source"] == "deployment"
                        && !baseline.get(key).is_some_and(|value| {
                            record
                                .resolved_settings
                                .get(key)
                                .is_some_and(|approved| policy_catalog::json_equal(value, approved))
                        })
                })
            });
            if changed {
                return self
                    .fail_boot(id, false, "deployment_baseline_changed")
                    .map(BootPreparation::Complete);
            }
        }
        let target = if recovering {
            record
                .known_good
                .as_ref()
                .ok_or(std::io::Error::other("recovery target missing"))?
                .resolved_settings
                .clone()
        } else {
            record.resolved_settings.clone()
        };
        let mut next = settings.clone();
        let result = compose_candidate(&mut next, &record.group, &target);
        let readback_ok = result.is_ok()
            && target.as_object().is_some_and(|values| {
                let actual = next.deployment_map();
                values.iter().all(|(key, expected)| {
                    actual
                        .get(key)
                        .is_some_and(|got| policy_catalog::json_equal(got, expected))
                })
            });
        if !readback_ok {
            return self
                .fail_boot(id, recovering, "candidate_verification_failed")
                .map(BootPreparation::Complete);
        }
        Ok(BootPreparation::Verify(Box::new(BootReadback {
            attempt_id: id.clone(),
            recovering,
            settings: next,
        })))
    }

    pub fn complete_boot(
        &mut self,
        readback: BootReadback,
        proved: bool,
        settings: &mut RuntimeSettings,
    ) -> std::io::Result<BootOutcome> {
        let id = &readback.attempt_id;
        if !proved {
            return self.fail_boot(id, readback.recovering, "candidate_verification_failed");
        }
        let Some(record) = self.journal.records.get(id).cloned() else {
            return Err(std::io::Error::other("boot attempt disappeared"));
        };
        let expected_phase = if readback.recovering {
            "recovery_verifying"
        } else {
            "verifying"
        };
        if record.phase != expected_phase {
            return Err(std::io::Error::other("boot attempt phase changed"));
        }
        {
            let current = self.journal.records.get_mut(id).unwrap();
            current.phase = if readback.recovering {
                "recovered"
            } else {
                "applied"
            }
            .into();
            current.sequence += 1;
            current.verified_at = Some(
                OffsetDateTime::now_utc()
                    .format(&Rfc3339)
                    .unwrap_or_default(),
            );
            current.verified_process_id = Some(std::process::id().to_string());
            let active = if readback.recovering {
                current.known_good.clone().unwrap()
            } else {
                ActiveGroup {
                    kind: "verified".into(),
                    digest: current.digest.clone(),
                    resolved_settings: current.resolved_settings.clone(),
                }
            };
            self.journal.active_groups.insert(record.group, active);
            self.persist()?;
        }
        *settings = readback.settings;
        Ok(BootOutcome::None)
    }

    fn fail_boot(
        &mut self,
        id: &str,
        recovering: bool,
        reason: &str,
    ) -> std::io::Result<BootOutcome> {
        if recovering {
            let current = self.journal.records.get_mut(id).unwrap();
            current.phase = "uncertain".into();
            current.sequence += 1;
            current
                .error
                .get_or_insert_with(|| "recovery_verification_failed".into());
            self.persist()?;
            return Ok(BootOutcome::Uncertain(id.into()));
        }
        let current = self.journal.records.get_mut(id).unwrap();
        current.phase = "failed".into();
        current.sequence += 1;
        current.error = Some(reason.into());
        self.persist()?;
        record_restart_phase(&self.path, &mut self.journal, id, "recovery_verifying")?;
        record_restart_phase(
            &self.path,
            &mut self.journal,
            id,
            "recovery_awaiting_startup",
        )?;
        Ok(BootOutcome::Recovery(id.into()))
    }

    fn persist(&self) -> std::io::Result<()> {
        persist_journal(&self.path, &self.journal)?;
        #[cfg(test)]
        {
            use std::sync::atomic::Ordering;
            let left = self.crash_after_writes.load(Ordering::SeqCst);
            if left > 0 {
                self.crash_after_writes.store(left - 1, Ordering::SeqCst);
                if left == 1 {
                    return Err(std::io::Error::other("injected crash after fsync"));
                }
            }
        }
        Ok(())
    }

    pub fn accept(&mut self, offer: Offer, settings: &mut RuntimeSettings) -> AgentMsg {
        let result = self.accept_inner(&offer, settings);
        match result {
            Ok((phase, sequence, active, evidence)) => {
                state(&offer, phase, sequence, active, evidence, None)
            }
            Err(reason) => state(&offer, "failed", 0, None, None, Some(reason)),
        }
    }

    /// Accept a restart grant only after the caller has excluded local launches
    /// and checked assigned, starting, running, stopping and preparation work.
    /// The runtime is deliberately not changed in this process: an
    /// `awaiting_startup` result requests an orderly process restart.
    pub fn accept_restart(
        &mut self,
        offer: Offer,
        settings: &RuntimeSettings,
        is_idle: bool,
        hardware: Option<HardwareEvidence<'_>>,
    ) -> AgentMsg {
        let result = self.accept_restart_inner(&offer, settings, is_idle, hardware);
        match result {
            Ok((phase, sequence)) => state(&offer, phase, sequence, None, None, None),
            Err(reason) => state(&offer, "failed", 0, None, None, Some(reason)),
        }
    }

    fn accept_restart_inner(
        &mut self,
        offer: &Offer,
        settings: &RuntimeSettings,
        is_idle: bool,
        hardware: Option<HardwareEvidence<'_>>,
    ) -> Result<(&'static str, u64), String> {
        if offer.host_id != self.host_id
            || offer.boot_incarnation != self.boot_incarnation
            || offer.connection_incarnation != self.connection_incarnation
        {
            return Err("stale_grant".into());
        }
        if !policy_catalog::is_restart_group(&offer.group) || offer.scope != "restart" {
            return Err("unsupported_group".into());
        }
        let expires =
            OffsetDateTime::parse(&offer.expires_at, &Rfc3339).map_err(|_| "invalid_expiry")?;
        if expires <= OffsetDateTime::now_utc() {
            return Err("grant_expired".into());
        }
        let revision = parse_revision(&offer.revision)?;
        if let Some(record) = self.journal.records.get(&offer.attempt_id) {
            if record.group != offer.group
                || record.scope != offer.scope
                || record.digest != offer.content_sha256
                || record.revision != offer.revision
                || record.boot_incarnation != offer.boot_incarnation
                || record.connection_incarnation != offer.connection_incarnation
            {
                return Err("attempt_conflict".into());
            }
            return Ok((
                match record.phase.as_str() {
                    "accepted" => "accepted",
                    "awaiting_startup" => "awaiting_startup",
                    "verifying" => "verifying",
                    "applied" => "applied",
                    "failed" => "failed",
                    "recovery_verifying" => "recovery_verifying",
                    "recovery_awaiting_startup" => "recovery_awaiting_startup",
                    "recovered" => "recovered",
                    _ => "uncertain",
                },
                record.sequence,
            ));
        }
        if !is_idle {
            return Err("host_busy".into());
        }
        if self.journal.records.values().any(|record| {
            record.scope == "restart"
                && matches!(
                    record.phase.as_str(),
                    "accepted"
                        | "activating"
                        | "awaiting_startup"
                        | "verifying"
                        | "failed"
                        | "recovery_verifying"
                        | "recovery_awaiting_startup"
                        | "uncertain"
                )
        }) {
            return Err("attempt_conflict".into());
        }
        let high_water = self
            .journal
            .high_water
            .get(&offer.group)
            .copied()
            .unwrap_or(0);
        if revision < high_water {
            return Err("stale_revision".into());
        }
        if !self.journal.ever_accepted_typed.contains(&offer.group) {
            return Err("group_execution_unavailable".into());
        }
        let content = json!({"group":offer.group,"scope":offer.scope,"revision":offer.revision,
            "settings":offer.settings,"resolved_settings":offer.resolved_settings});
        if policy_catalog::digest(&content) != offer.content_sha256 {
            return Err("content_mismatch".into());
        }
        let automatic = offer.settings.as_object().is_some_and(|choices| {
            choices
                .values()
                .any(|choice| choice["source"] == "automatic")
        });
        let hardware = if automatic {
            Some(hardware_facts(
                offer,
                hardware.ok_or("hardware_evidence_missing")?,
            )?)
        } else {
            None
        };
        let mut resolved_choices = offer.settings.clone();
        if automatic {
            for (key, choice) in resolved_choices.as_object_mut().ok_or("missing_setting")? {
                if choice["source"] == "automatic" {
                    *choice = json!({"source":"explicit","value":offer.resolved_settings[key]});
                }
            }
        }
        let resolved = policy_catalog::resolve_group(
            &offer.group,
            &resolved_choices,
            &self.deployment_baseline,
        )?;
        if !same_resolution(&offer.resolved_settings, &resolved) {
            return Err("resolved_mismatch".into());
        }
        let mut candidate = settings.clone();
        compose_candidate(&mut candidate, &offer.group, &offer.resolved_settings)?;
        let known_good = self
            .journal
            .active_groups
            .get(&offer.group)
            .ok_or("active_snapshot_missing")?
            .clone();
        let snapshot_kind = match known_good.kind.as_str() {
            "verified" => "last_verified_group_digest",
            "seeded" => "seeded_group_digest",
            _ => return Err("active_snapshot_invalid".into()),
        };
        let mut facts = vec![
            json!({"kind":"accepted_attempts","id":self.accepted_attempts_digest(&offer.group)}),
            json!({"kind":snapshot_kind,"id":known_good.digest}),
        ];
        if let Some(baseline) = policy_catalog::deployment_fact(&offer.settings, &resolved) {
            facts.push(json!({"kind":"deployment_baseline","id":baseline}));
        }
        if let Some(hardware_facts) = hardware {
            facts.extend(hardware_facts);
        }
        facts.sort_by(|a, b| {
            (a["kind"].as_str(), a["id"].as_str()).cmp(&(b["kind"].as_str(), b["id"].as_str()))
        });
        if facts != offer.prerequisites || facts_digest(&facts)? != offer.prerequisites_sha256 {
            return Err("prerequisite_mismatch".into());
        }
        if revision == high_water
            && !self.journal.records.values().any(|record| {
                record.group == offer.group
                    && record.revision == offer.revision
                    && record.settings == offer.settings
            })
        {
            return Err("revision_conflict".into());
        }
        let record = Record {
            host_id: offer.host_id.clone(),
            group: offer.group.clone(),
            scope: offer.scope.clone(),
            revision: offer.revision.clone(),
            digest: offer.content_sha256.clone(),
            phase: "accepted".into(),
            sequence: 1,
            resolved_settings: offer.resolved_settings.clone(),
            resolved_idle_timeout_secs: None,
            boot_incarnation: offer.boot_incarnation.clone(),
            connection_incarnation: offer.connection_incarnation.clone(),
            settings: offer.settings.clone(),
            known_good: Some(known_good),
            error: None,
            verified_at: None,
            verified_process_id: None,
        };
        let prior = self.journal.clone();
        self.journal
            .records
            .insert(offer.attempt_id.clone(), record);
        self.journal
            .high_water
            .insert(offer.group.clone(), revision);
        if self.persist().is_err() {
            self.journal = prior;
            return Err("journal_write_failed".into());
        }
        self.journal
            .records
            .get_mut(&offer.attempt_id)
            .unwrap()
            .phase = "awaiting_startup".into();
        self.journal
            .records
            .get_mut(&offer.attempt_id)
            .unwrap()
            .sequence = 2;
        if self.persist().is_err() {
            // The marker may or may not have reached durable storage. Exit
            // instead of leaving an accepted attempt parked in this process:
            // bootstrap distinguishes the two states, and reconnect inventory
            // tells the control plane which one survived.
            return Err("journal_write_failed".into());
        }
        Ok(("awaiting_startup", 2))
    }

    fn accepted_attempts_digest(&self, group: &str) -> String {
        let mut bytes = Vec::new();
        for (id, record) in &self.journal.records {
            if record.group == group && record.scope == "restart" {
                bytes.extend_from_slice(id.as_bytes());
                bytes.push(0);
                bytes.extend_from_slice(record.phase.as_bytes());
                bytes.push(b'\n');
            }
        }
        hex_digest(&bytes)
    }

    fn accept_inner(
        &mut self,
        offer: &Offer,
        settings: &mut RuntimeSettings,
    ) -> Result<ApplyOutcome, String> {
        if offer.host_id != self.host_id
            || offer.boot_incarnation != self.boot_incarnation
            || offer.connection_incarnation != self.connection_incarnation
        {
            return Err("stale_grant".into());
        }
        let group = offer.group.as_str();
        if !policy_catalog::is_next_session_group(group) || offer.scope != "next_session" {
            return Err("unsupported_group".into());
        }
        let expires =
            OffsetDateTime::parse(&offer.expires_at, &Rfc3339).map_err(|_| "invalid_expiry")?;
        if expires <= OffsetDateTime::now_utc() {
            return Err("grant_expired".into());
        }
        let revision = parse_revision(&offer.revision)?;
        let high_water = self.journal.high_water.get(group).copied().unwrap_or(0);
        if revision < high_water {
            return Err("stale_revision".into());
        }
        if !self.journal.ever_accepted_typed.contains(group) {
            return Err("group_execution_unavailable".into());
        }
        let content = json!({"group":offer.group,"scope":offer.scope,"revision":offer.revision,"settings":offer.settings,"resolved_settings":offer.resolved_settings});
        let digest = policy_catalog::digest(&content);
        if digest != offer.content_sha256 {
            return Err("content_mismatch".into());
        }
        if let Some(existing) = self.journal.records.get(&offer.attempt_id).cloned() {
            if existing.group != offer.group
                || existing.digest != digest
                || existing.revision != offer.revision
                || existing.boot_incarnation != offer.boot_incarnation
                || existing.connection_incarnation != offer.connection_incarnation
            {
                return Err("attempt_conflict".into());
            }
            let mut next = settings.clone();
            compose_candidate(&mut next, group, &existing.resolved_settings)?;
            if existing.phase != "applied" {
                let durable = self.journal.clone();
                self.activate(&offer.attempt_id, &existing);
                if self.persist().is_err() {
                    self.journal = durable;
                    return Err("journal_write_failed".into());
                }
            }
            *settings = next;
            return Ok((
                "applied",
                self.journal.records[&offer.attempt_id].sequence,
                Some("next_session"),
                Some(evidence(offer, &existing.resolved_settings)),
            ));
        }
        // The baseline fact is checked against this process's current
        // pre-policy baseline before resolving: an unstarted grant resolved
        // from any other baseline is stale evidence, not the offer's fault.
        let offered_baseline = offer
            .prerequisites
            .iter()
            .find(|fact| fact["kind"] == "deployment_baseline")
            .and_then(|fact| fact["id"].as_str());
        if let Some(offered) = offered_baseline {
            let current = Map::from_iter(self.deployment_baseline.deployment_map());
            if policy_catalog::deployment_fact(&offer.settings, &current).as_deref()
                != Some(offered)
            {
                return Err("deployment_baseline_changed".into());
            }
        }
        let resolved =
            policy_catalog::resolve_group(group, &offer.settings, &self.deployment_baseline)?;
        if !same_resolution(&offer.resolved_settings, &resolved) {
            return Err("resolved_mismatch".into());
        }
        policy_catalog::check_host_effects(&resolved, &self.deployment_baseline, &|path| {
            fs::read_dir(path).is_ok()
        })?;
        if let Some(root) = resolved.get("home_root").and_then(Value::as_str) {
            if !root.is_empty()
                && !real_path_within_mount(root, &self.deployment_baseline.home_root)
            {
                return Err("home_root_outside_mount".into());
            }
        }
        let resolved = Value::Object(resolved);
        let mut next = settings.clone();
        compose_candidate(&mut next, group, &resolved)?;
        let active = self
            .journal
            .active_groups
            .get(group)
            .ok_or("active_snapshot_missing")?;
        let snapshot = match active.kind.as_str() {
            "verified" => ("last_verified_group_digest", active.digest.clone()),
            "seeded" => ("seeded_group_digest", active.digest.clone()),
            _ => return Err("active_snapshot_invalid".into()),
        };
        let mut facts = vec![json!({"kind":snapshot.0,"id":snapshot.1})];
        if let Some(baseline) = resolved
            .as_object()
            .and_then(|resolved| policy_catalog::deployment_fact(&offer.settings, resolved))
        {
            facts.push(json!({"kind":"deployment_baseline","id":baseline}));
        }
        facts.sort_by(|a, b| {
            (a["kind"].as_str(), a["id"].as_str()).cmp(&(b["kind"].as_str(), b["id"].as_str()))
        });
        if offer.prerequisites != facts || offer.prerequisites_sha256 != facts_digest(&facts)? {
            return Err("prerequisite_mismatch".into());
        }
        // Equal-revision deployment re-resolution may change only the
        // resolved value and its baseline prerequisite, never the choice.
        if revision == high_water {
            let same = self.journal.records.values().any(|r| {
                r.group == offer.group
                    && r.revision == offer.revision
                    && r.settings == offer.settings
            });
            if !same {
                return Err("revision_conflict".into());
            }
        }
        let record = Record {
            host_id: offer.host_id.clone(),
            group: offer.group.clone(),
            scope: offer.scope.clone(),
            revision: offer.revision.clone(),
            digest,
            phase: "accepted".into(),
            sequence: 1,
            resolved_idle_timeout_secs: (group == IDLE)
                .then(|| resolved.get(IDLE).and_then(Value::as_u64))
                .flatten(),
            resolved_settings: resolved.clone(),
            boot_incarnation: offer.boot_incarnation.clone(),
            connection_incarnation: offer.connection_incarnation.clone(),
            settings: offer.settings.clone(),
            known_good: None,
            error: None,
            verified_at: None,
            verified_process_id: None,
        };
        let durable = self.journal.clone();
        self.journal
            .records
            .insert(offer.attempt_id.clone(), record.clone());
        self.journal
            .high_water
            .insert(offer.group.clone(), revision);
        if self.persist().is_err() {
            self.journal = durable;
            return Err("journal_write_failed".into());
        }
        // SessionManager copies runtime_settings at launch. Existing sessions
        // retain their original copy while the next launch sees this value.
        let accepted = self.journal.clone();
        self.activate(&offer.attempt_id, &record);
        if self.persist().is_err() {
            self.journal = accepted;
            return Err("journal_write_failed".into());
        }
        *settings = next;
        Ok((
            "applied",
            self.journal.records[&offer.attempt_id].sequence,
            Some("next_session"),
            Some(evidence(offer, &resolved)),
        ))
    }

    /// Mark an accepted attempt applied and make its snapshot the group's
    /// verified active one. In memory only; the caller persists.
    fn activate(&mut self, attempt_id: &str, record: &Record) {
        self.journal.active_groups.insert(
            record.group.clone(),
            ActiveGroup {
                kind: "verified".into(),
                digest: record.digest.clone(),
                resolved_settings: record.resolved_settings.clone(),
            },
        );
        if let Some(record) = self.journal.records.get_mut(attempt_id) {
            if record.phase != "applied" {
                record.phase = "applied".into();
                record.sequence += 1;
            }
        }
    }
}

fn persist_journal(path: &Path, journal: &Journal) -> std::io::Result<()> {
    let tmp = path.with_extension("policy.tmp");
    let bytes = serde_json::to_vec(&journal.on_disk()).map_err(std::io::Error::other)?;
    let mut f = OpenOptions::new()
        .create(true)
        .write(true)
        .truncate(true)
        .open(&tmp)?;
    f.write_all(&bytes)?;
    f.sync_all()?;
    fs::rename(&tmp, path)?;
    if let Some(parent) = path.parent() {
        OpenOptions::new().read(true).open(parent)?.sync_all()?;
    }
    Ok(())
}

fn record_restart_phase(
    path: &Path,
    journal: &mut Journal,
    id: &str,
    phase: &str,
) -> std::io::Result<()> {
    let record = journal
        .records
        .get_mut(id)
        .ok_or(std::io::Error::other("restart attempt missing"))?;
    record.phase = phase.into();
    record.sequence += 1;
    persist_journal(path, journal)
}

// The catalog checks lexical containment. Before accepting a new home root,
// resolve its deepest existing component so a symlink cannot send future
// homes outside the deployment mount. A not-yet-created child is valid only
// when that existing ancestor is inside the real mount.
fn real_path_within_mount(candidate: &str, mount: &str) -> bool {
    let Ok(real_mount) = fs::canonicalize(mount) else {
        return false;
    };
    let mut ancestor = Path::new(candidate);
    loop {
        match fs::symlink_metadata(ancestor) {
            Ok(_) => {
                return fs::canonicalize(ancestor)
                    .is_ok_and(|real_candidate| real_candidate.starts_with(&real_mount))
            }
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
                let Some(parent) = ancestor.parent() else {
                    return false;
                };
                ancestor = parent;
            }
            Err(_) => return false,
        }
    }
}

/// Compose one group's candidate onto `next`. A value the agent would not
/// actually run with is the offer's own fault, so it reports `invalid_value`.
fn compose_candidate(
    next: &mut RuntimeSettings,
    group: &str,
    resolved: &Value,
) -> Result<(), String> {
    policy_catalog::compose_groups(next, [(group, resolved)]).map_err(|code| match code {
        "typed_snapshot_invalid_value" => "invalid_value".to_string(),
        other => other.to_string(),
    })
}

fn same_resolution(offered: &Value, resolved: &Map<String, Value>) -> bool {
    offered.as_object().is_some_and(|offered| {
        offered.len() == resolved.len()
            && resolved.iter().all(|(key, value)| {
                offered
                    .get(key)
                    .is_some_and(|got| policy_catalog::json_equal(got, value))
            })
    })
}

fn evidence(offer: &Offer, resolved: &Value) -> Value {
    json!({
        "revision":offer.revision,
        "content_sha256":offer.content_sha256,
        "resolved_settings":resolved,
        "agent_process_id":std::process::id().to_string(),
        "observed_at":OffsetDateTime::now_utc().format(&Rfc3339).unwrap_or_default(),
        "evidence_ids":[]
    })
}

fn parse_revision(raw: &str) -> Result<u64, String> {
    let parsed = raw.parse::<u64>().map_err(|_| "invalid_revision")?;
    if parsed.to_string() != raw {
        return Err("invalid_revision".into());
    }
    Ok(parsed)
}

fn hex_digest(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

fn facts_digest(facts: &[Value]) -> Result<String, String> {
    let mut bytes = Vec::new();
    for fact in facts {
        let kind = fact
            .get("kind")
            .and_then(Value::as_str)
            .ok_or("invalid_prerequisite")?;
        let id = fact
            .get("id")
            .and_then(Value::as_str)
            .ok_or("invalid_prerequisite")?;
        if kind.is_empty()
            || id.is_empty()
            || kind.contains(['\0', '\n'])
            || id.contains(['\0', '\n'])
        {
            return Err("invalid_prerequisite".into());
        }
        bytes.extend_from_slice(kind.as_bytes());
        bytes.push(0);
        bytes.extend_from_slice(id.as_bytes());
        bytes.push(b'\n');
    }
    Ok(hex_digest(&bytes))
}

fn hardware_facts(offer: &Offer, evidence: HardwareEvidence<'_>) -> Result<Vec<Value>, String> {
    let node = offer.resolved_settings["render_node"]
        .as_str()
        .ok_or("hardware_evidence_missing")?;
    let mut matched = evidence.gpus.iter().filter(|gpu| {
        gpu.render_node.as_deref() == Some(node)
            && gpu.encode_slots_total > 0
            && gpu
                .driver_identity
                .as_ref()
                .is_some_and(|id| !id.is_empty())
    });
    let gpu = matched.next().ok_or("hardware_evidence_missing")?;
    if matched.next().is_some() {
        return Err("hardware_evidence_ambiguous".into());
    }
    if offer.settings["encoder"]["source"] == "automatic" {
        let encoder = match gpu.vendor.trim().to_ascii_lowercase().as_str() {
            "amd" | "nvidia" => "vulkan",
            "intel" => "va",
            _ => return Err("hardware_vendor_unavailable".into()),
        };
        if offer.resolved_settings["encoder"] != encoder {
            return Err("resolved_mismatch".into());
        }
    }
    // Capacity reports can contain sysfs paths visible across container
    // boundaries. Opening the actual node is the admission evidence.
    let path = gpu.device_path.as_deref().unwrap_or(node);
    std::fs::File::open(path).map_err(|_| "hardware_device_inaccessible")?;
    let driver = gpu
        .driver_identity
        .as_deref()
        .ok_or("hardware_evidence_missing")?;
    let probe_id = format!("media_probe_gpu{}", gpu.index);
    let check = evidence
        .readiness
        .iter()
        .find(|check| check.id == probe_id)
        .ok_or("hardware_probe_unavailable")?;
    if check.status != "pass"
        || check.source.as_deref() != Some("host_probe")
        || check
            .observed_at
            .as_deref()
            .is_none_or(|at| OffsetDateTime::parse(at, &Rfc3339).is_err())
        || check
            .blocks
            .as_ref()
            .is_some_and(|blocks| blocks.gpu_index != Some(gpu.index))
    {
        return Err("hardware_probe_unavailable".into());
    }
    let device = hex_digest(format!("gpu\0{}\0{}\0{}\n", gpu.index, node, driver).as_bytes());
    let probe = hex_digest(
        format!(
            "{probe_id}\0{device}\0{}\0host_probe\0pass\n",
            offer.connection_incarnation
        )
        .as_bytes(),
    );
    Ok(vec![
        json!({"kind":"accessible_device","id":device}),
        json!({"kind":"driver_identity","id":driver}),
        json!({"kind":"host_probe_result","id":probe}),
    ])
}

fn state(
    offer: &Offer,
    phase: &str,
    sequence: u64,
    active: Option<&str>,
    evidence: Option<Value>,
    error: Option<String>,
) -> AgentMsg {
    AgentMsg::ConfigPolicyState {
        attempt_id: offer.attempt_id.clone(),
        host_id: offer.host_id.clone(),
        group: offer.group.clone(),
        revision: offer.revision.clone(),
        content_sha256: offer.content_sha256.clone(),
        scope: offer.scope.clone(),
        grant_boot_incarnation: offer.boot_incarnation.clone(),
        grant_connection_incarnation: offer.connection_incarnation.clone(),
        journal_sequence: sequence.to_string(),
        phase: phase.to_string(),
        active_scope: active.map(str::to_string),
        evidence,
        error,
    }
}

/// Builders for a journal-backed agent and a well-formed offer, shared with the
/// `agent.rs` tests that drive typed offers through `handle_control`.
#[cfg(test)]
pub(crate) mod test_support {
    use super::*;

    /// An agent on `dir` with a durable seed for every next-session group and
    /// `owned` typed-owned, composed onto `runtime` as a connection would.
    pub(crate) fn owned_agent(
        dir: &std::path::Path,
        owned: &[&str],
        runtime: &mut RuntimeSettings,
    ) -> PolicyAgent {
        let baseline = runtime.clone();
        let mut agent = PolicyAgent::open(
            dir.join("policy.json"),
            "host".into(),
            "boot".into(),
            "conn".into(),
            runtime,
        )
        .unwrap();
        agent
            .apply_legacy_overlay(&baseline, runtime, &json!({}), Some("delivery"))
            .unwrap();
        let advertised: Vec<String> = agent.journal.groups().into_iter().collect();
        let mut owned: Vec<String> = owned.iter().map(|g| g.to_string()).collect();
        owned.sort();
        agent.confirm_groups(&advertised, &owned).unwrap();
        agent
    }

    /// An offer for `group` with `choice` (`{"source":..}`), resolved by the
    /// caller as `resolved`, bound to the agent's current active snapshot.
    pub(crate) fn offer(
        agent: &PolicyAgent,
        attempt: &str,
        revision: &str,
        group: &str,
        choice: Value,
        resolved: Value,
    ) -> Offer {
        let active = &agent.journal.active_groups[group];
        let kind = if active.kind == "verified" {
            "last_verified_group_digest"
        } else {
            "seeded_group_digest"
        };
        let settings = json!({ group: choice });
        let resolved_settings = json!({ group: resolved });
        let mut prerequisites = vec![json!({"kind":kind,"id":active.digest})];
        if let Some(fact) =
            policy_catalog::deployment_fact(&settings, resolved_settings.as_object().unwrap())
        {
            prerequisites.push(json!({"kind":"deployment_baseline","id":fact}));
        }
        prerequisites.sort_by(|a, b| {
            (a["kind"].as_str(), a["id"].as_str()).cmp(&(b["kind"].as_str(), b["id"].as_str()))
        });
        let content = json!({"group":group,"scope":"next_session","revision":revision,
            "settings":settings,"resolved_settings":resolved_settings});
        Offer {
            attempt_id: attempt.into(),
            host_id: "host".into(),
            boot_incarnation: agent.boot_incarnation.clone(),
            connection_incarnation: agent.connection_incarnation.clone(),
            group: group.into(),
            revision: revision.into(),
            content_sha256: policy_catalog::digest(&content),
            scope: "next_session".into(),
            expires_at: "2099-01-01T00:00:00Z".into(),
            prerequisites_sha256: facts_digest(&prerequisites).unwrap(),
            prerequisites,
            settings,
            resolved_settings,
        }
    }

    pub(crate) fn explicit(
        agent: &PolicyAgent,
        attempt: &str,
        revision: &str,
        group: &str,
        value: Value,
    ) -> Offer {
        offer(
            agent,
            attempt,
            revision,
            group,
            json!({"source":"explicit","value":value.clone()}),
            value,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::test_support::{explicit, offer, owned_agent};
    use super::*;

    fn idle_offer() -> Offer {
        let baseline = RuntimeSettings::baseline_with(&|_| None);
        let seed = json!({ IDLE: baseline.idle_timeout_secs });
        let prerequisites = vec![
            json!({"kind":"seeded_group_digest","id":policy_catalog::snapshot_digest(IDLE, &seed)}),
        ];
        let mut offer = Offer {
            attempt_id: "a".into(),
            host_id: "host".into(),
            boot_incarnation: "boot".into(),
            connection_incarnation: "conn".into(),
            group: IDLE.into(),
            revision: "1".into(),
            content_sha256: String::new(),
            scope: "next_session".into(),
            expires_at: "2099-01-01T00:00:00Z".into(),
            prerequisites_sha256: facts_digest(&prerequisites).unwrap(),
            prerequisites,
            settings: json!({"idle_timeout_secs":{"source":"explicit","value":900}}),
            resolved_settings: json!({"idle_timeout_secs":900}),
        };
        offer.content_sha256 = content_digest(&offer);
        offer
    }

    fn content_digest(offer: &Offer) -> String {
        policy_catalog::digest(
            &json!({"group":offer.group,"scope":offer.scope,"revision":offer.revision,"settings":offer.settings,"resolved_settings":offer.resolved_settings}),
        )
    }

    fn seed_idle(agent: &mut PolicyAgent, runtime: &mut RuntimeSettings) {
        let baseline = RuntimeSettings::baseline_with(&|_| None);
        agent
            .apply_legacy_overlay(&baseline, runtime, &json!({}), Some("delivery"))
            .unwrap();
        agent
            .confirm_groups(&[IDLE.into()], &[IDLE.into()])
            .unwrap();
    }

    fn record(group: &str, revision: &str, phase: &str, sequence: u64, resolved: Value) -> Record {
        Record {
            host_id: "host".into(),
            group: group.into(),
            scope: "next_session".into(),
            revision: revision.into(),
            digest: "a".repeat(64),
            phase: phase.into(),
            sequence,
            resolved_idle_timeout_secs: None,
            resolved_settings: resolved,
            boot_incarnation: "boot".into(),
            connection_incarnation: "conn".into(),
            settings: Value::Null,
            known_good: None,
            error: None,
            verified_at: None,
            verified_process_id: None,
        }
    }

    fn phase_of(msg: &AgentMsg) -> (&str, Option<&str>) {
        match msg {
            AgentMsg::ConfigPolicyState { phase, error, .. } => (phase, error.as_deref()),
            other => panic!("expected config_policy_state, got {other:?}"),
        }
    }

    fn open_at(path: &std::path::Path, runtime: &mut RuntimeSettings) -> PolicyAgent {
        PolicyAgent::open(
            path.to_path_buf(),
            "host".into(),
            "boot".into(),
            "conn".into(),
            runtime,
        )
        .unwrap()
    }

    fn finish_test_boot(
        agent: &mut PolicyAgent,
        outcome: &BootOutcome,
        runtime: &mut RuntimeSettings,
        proved: bool,
    ) -> BootOutcome {
        match agent.prepare_boot(outcome, runtime).unwrap() {
            BootPreparation::Complete(result) => result,
            BootPreparation::Verify(readback) => {
                agent.complete_boot(*readback, proved, runtime).unwrap()
            }
        }
    }

    #[test]
    fn hardware_offer_is_durable_before_restart_and_never_claims_prestart_application() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let baseline = runtime.clone();
        let mut agent = open_at(&path, &mut runtime);
        agent
            .apply_legacy_overlay(&baseline, &mut runtime, &json!({}), Some("delivery"))
            .unwrap();
        assert!(agent.journal.active_groups.contains_key("hardware"));
        let advertised: Vec<String> = agent.journal.groups().into_iter().collect();
        agent
            .confirm_groups(&advertised, &["hardware".into()])
            .unwrap();
        let active = agent.journal.active_groups["hardware"].clone();
        let live = runtime.deployment_map();
        let resolved = json!({"encoder":live["encoder"],"render_node":live["render_node"],"cuda_device":live["cuda_device"]});
        let settings = json!({
            "encoder":{"source":"explicit","value":resolved["encoder"]},
            "render_node":{"source":"explicit","value":resolved["render_node"]},
            "cuda_device":{"source":"explicit","value":resolved["cuda_device"]}
        });
        let prerequisites = vec![
            json!({"kind":"accepted_attempts","id":hex_digest(&[])}),
            json!({"kind":"seeded_group_digest","id":active.digest}),
        ];
        let offer = Offer {
            attempt_id: "00000000-0000-4000-8000-000000000101".into(),
            host_id: "host".into(),
            boot_incarnation: "boot".into(),
            connection_incarnation: "conn".into(),
            group: "hardware".into(),
            revision: "1".into(),
            content_sha256: policy_catalog::digest(&json!({
                "group":"hardware","scope":"restart","revision":"1",
                "settings":settings,"resolved_settings":resolved
            })),
            scope: "restart".into(),
            expires_at: "2099-01-01T00:00:00Z".into(),
            prerequisites_sha256: facts_digest(&prerequisites).unwrap(),
            prerequisites,
            settings,
            resolved_settings: resolved,
        };
        let before = runtime.deployment_map();
        let state = agent.accept_restart(offer.clone(), &runtime, true, None);
        assert_eq!(phase_of(&state).0, "awaiting_startup");
        assert_eq!(runtime.deployment_map(), before);
        let disk = fs::read(&path).unwrap();
        let journal = Journal::load(&disk).unwrap();
        assert_eq!(journal.records[&offer.attempt_id].phase, "awaiting_startup");
        assert_eq!(
            journal.records[&offer.attempt_id].resolved_settings,
            offer.resolved_settings
        );
        assert_eq!(
            phase_of(&agent.accept_restart(offer, &runtime, true, None)).0,
            "awaiting_startup"
        );
    }

    #[test]
    fn ambiguous_restart_marker_write_forces_boot_reconciliation() {
        use std::sync::atomic::Ordering;
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let baseline = runtime.clone();
        let mut agent = open_at(&path, &mut runtime);
        agent
            .apply_legacy_overlay(&baseline, &mut runtime, &json!({}), Some("delivery"))
            .unwrap();
        let advertised: Vec<String> = agent.journal.groups().into_iter().collect();
        agent
            .confirm_groups(&advertised, &["hardware".into()])
            .unwrap();
        let live = runtime.deployment_map();
        let resolved = json!({"encoder":live["encoder"],"render_node":live["render_node"],"cuda_device":live["cuda_device"]});
        let settings = json!({
            "encoder":{"source":"explicit","value":resolved["encoder"]},
            "render_node":{"source":"explicit","value":resolved["render_node"]},
            "cuda_device":{"source":"explicit","value":resolved["cuda_device"]}
        });
        let active = &agent.journal.active_groups["hardware"];
        let prerequisites = vec![
            json!({"kind":"accepted_attempts","id":hex_digest(&[])}),
            json!({"kind":"seeded_group_digest","id":active.digest}),
        ];
        let offer = Offer {
            attempt_id: "00000000-0000-4000-8000-000000000104".into(),
            host_id: "host".into(),
            boot_incarnation: "boot".into(),
            connection_incarnation: "conn".into(),
            group: "hardware".into(),
            revision: "1".into(),
            content_sha256: policy_catalog::digest(&json!({"group":"hardware","scope":"restart",
                "revision":"1","settings":settings,"resolved_settings":resolved})),
            scope: "restart".into(),
            expires_at: "2099-01-01T00:00:00Z".into(),
            prerequisites_sha256: facts_digest(&prerequisites).unwrap(),
            prerequisites,
            settings,
            resolved_settings: resolved,
        };
        agent.crash_after_writes.store(2, Ordering::SeqCst);
        let reply = agent.accept_restart(offer.clone(), &runtime, true, None);
        assert_eq!(phase_of(&reply), ("failed", Some("journal_write_failed")));
        assert_eq!(runtime.deployment_map(), baseline.deployment_map());
        drop(agent);
        let disk = Journal::load(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(disk.records[&offer.attempt_id].phase, "awaiting_startup");
        assert_eq!(
            PolicyAgent::bootstrap(&path).unwrap(),
            BootOutcome::Candidate(offer.attempt_id)
        );
    }

    #[test]
    fn accepted_restart_without_marker_recovers_known_good_once() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let baseline = RuntimeSettings::baseline_with(&|_| None);
        let good = json!({"encoder":baseline.deployment_map()["encoder"],
            "render_node":baseline.deployment_map()["render_node"],
            "cuda_device":baseline.deployment_map()["cuda_device"]});
        let active = ActiveGroup {
            kind: "verified".into(),
            digest: policy_catalog::snapshot_digest("hardware", &good),
            resolved_settings: good.clone(),
        };
        let id = "00000000-0000-4000-8000-000000000107";
        let mut journal = Journal::default();
        journal
            .active_groups
            .insert("hardware".into(), active.clone());
        let mut attempt = record("hardware", "2", "accepted", 1, good);
        attempt.scope = "restart".into();
        attempt.known_good = Some(active);
        journal.records.insert(id.into(), attempt);
        fs::write(&path, serde_json::to_vec(&journal).unwrap()).unwrap();

        assert_eq!(
            PolicyAgent::bootstrap(&path).unwrap(),
            BootOutcome::Recovery(id.into())
        );
        let after = Journal::load(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(after.records[id].phase, "recovery_awaiting_startup");
        assert_eq!(
            after.records[id].error.as_deref(),
            Some("candidate_startup_interrupted")
        );
        assert_eq!(
            PolicyAgent::bootstrap(&path).unwrap(),
            BootOutcome::RecoveryVerify(id.into())
        );
    }

    #[test]
    fn automatic_hardware_checks_exact_current_device_probe_and_deployment_facts() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("device");
        fs::write(&path, b"device-access").unwrap();
        let node = path.to_str().unwrap();
        let mut runtime = RuntimeSettings::baseline();
        let mut agent = test_support::owned_agent(dir.path(), &["hardware"], &mut runtime);
        let resolved = json!({"encoder":"vulkan","render_node":node,
            "cuda_device":runtime.deployment_map()["cuda_device"]});
        let settings = json!({
            "encoder":{"source":"automatic"},
            "render_node":{"source":"automatic"},
            "cuda_device":{"source":"deployment"}
        });
        let gpu = GpuCapacity {
            index: 0,
            vendor: "amd".into(),
            model: "test".into(),
            vram_mb_total: 1,
            encode_slots_total: 1,
            render_node: Some(node.into()),
            device_path: Some(node.into()),
            driver_identity: Some("driver:test".into()),
            codecs: None,
        };
        let check = ReadinessCheck {
            id: "media_probe_gpu0".into(),
            status: "pass".into(),
            summary: String::new(),
            remediation: String::new(),
            observed_at: Some("2026-09-23T00:00:00Z".into()),
            source: Some("host_probe".into()),
            blocks: None,
        };
        let gpus = [gpu];
        let readiness = [check];
        let mut offer = Offer {
            attempt_id: "00000000-0000-4000-8000-000000000102".into(),
            host_id: "host".into(),
            boot_incarnation: "boot".into(),
            connection_incarnation: "conn".into(),
            group: "hardware".into(),
            revision: "1".into(),
            content_sha256: String::new(),
            scope: "restart".into(),
            expires_at: "2099-01-01T00:00:00Z".into(),
            prerequisites_sha256: String::new(),
            prerequisites: Vec::new(),
            settings,
            resolved_settings: resolved,
        };
        offer.content_sha256 =
            policy_catalog::digest(&json!({"group":"hardware","scope":"restart",
            "revision":"1","settings":offer.settings,"resolved_settings":offer.resolved_settings}));
        let active = &agent.journal.active_groups["hardware"];
        let mut facts = hardware_facts(
            &offer,
            HardwareEvidence {
                gpus: &gpus,
                readiness: &readiness,
            },
        )
        .unwrap();
        facts.push(json!({"kind":"accepted_attempts","id":hex_digest(&[])}));
        facts.push(json!({"kind":"seeded_group_digest","id":active.digest}));
        let baseline = policy_catalog::deployment_fact(
            &offer.settings,
            offer.resolved_settings.as_object().unwrap(),
        )
        .unwrap();
        facts.push(json!({"kind":"deployment_baseline","id":baseline}));
        facts.sort_by(|a, b| {
            (a["kind"].as_str(), a["id"].as_str()).cmp(&(b["kind"].as_str(), b["id"].as_str()))
        });
        assert_eq!(facts.len(), 6); // no fabricated agent-image digest
        assert!(!facts
            .iter()
            .any(|fact| fact["kind"] == "agent_image_digest"));
        offer.prerequisites_sha256 = facts_digest(&facts).unwrap();
        offer.prerequisites = facts;
        assert_eq!(
            phase_of(&agent.accept_restart(
                offer,
                &runtime,
                true,
                Some(HardwareEvidence {
                    gpus: &gpus,
                    readiness: &readiness
                })
            ))
            .0,
            "awaiting_startup"
        );
    }

    #[test]
    fn boot_consumes_candidate_marker_once_and_second_boot_never_retries_candidate() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let mut journal = Journal::default();
        let mut attempt = record(
            "hardware",
            "3",
            "awaiting_startup",
            2,
            json!({"encoder":"openh264","render_node":"software","cuda_device":0}),
        );
        attempt.scope = "restart".into();
        attempt.known_good = Some(ActiveGroup {
            kind: "seeded".into(),
            digest: "b".repeat(64),
            resolved_settings: json!({"encoder":"va","render_node":"/dev/dri/renderD128","cuda_device":0}),
        });
        journal
            .records
            .insert("00000000-0000-4000-8000-000000000102".into(), attempt);
        fs::write(&path, serde_json::to_vec(&journal).unwrap()).unwrap();

        let first = PolicyAgent::bootstrap(&path).unwrap();
        assert_eq!(
            first,
            BootOutcome::Candidate("00000000-0000-4000-8000-000000000102".into())
        );
        assert_eq!(
            Journal::load(&fs::read(&path).unwrap())
                .unwrap()
                .records
                .values()
                .next()
                .unwrap()
                .phase,
            "verifying"
        );

        let second = PolicyAgent::bootstrap(&path).unwrap();
        assert_eq!(
            second,
            BootOutcome::Recovery("00000000-0000-4000-8000-000000000102".into())
        );
        let after = Journal::load(&fs::read(&path).unwrap()).unwrap();
        let record = after.records.values().next().unwrap();
        assert_eq!(record.phase, "recovery_awaiting_startup");
        assert!(
            record.error.is_some(),
            "original candidate failure remains visible"
        );

        let third = PolicyAgent::bootstrap(&path).unwrap();
        assert_eq!(
            third,
            BootOutcome::RecoveryVerify("00000000-0000-4000-8000-000000000102".into())
        );
        let fourth = PolicyAgent::bootstrap(&path).unwrap();
        assert_eq!(
            fourth,
            BootOutcome::Uncertain("00000000-0000-4000-8000-000000000102".into())
        );
        let after = Journal::load(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(after.records.values().next().unwrap().phase, "uncertain");
    }

    #[test]
    fn bootstrap_distinguishes_write_failure_from_invalid_journal() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let mut journal = Journal::default();
        let mut attempt = record(
            "hardware",
            "3",
            "awaiting_startup",
            2,
            json!({"encoder":"openh264","render_node":"software","cuda_device":0}),
        );
        attempt.scope = "restart".into();
        journal.records.insert("attempt".into(), attempt);
        fs::write(&path, serde_json::to_vec(&journal).unwrap()).unwrap();
        fs::create_dir(path.with_extension("policy.tmp")).unwrap();

        assert!(matches!(
            PolicyAgent::bootstrap(&path),
            Err(BootError::Write(_))
        ));
        assert_eq!(
            Journal::load(&fs::read(&path).unwrap()).unwrap().records["attempt"].phase,
            "awaiting_startup",
            "a failed write must preserve the durable marker"
        );

        fs::write(&path, b"not json").unwrap();
        assert!(matches!(
            PolicyAgent::bootstrap(&path),
            Err(BootError::Invalid(_))
        ));
    }

    #[test]
    fn candidate_startup_failure_restores_only_the_known_good_and_keeps_original_error() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let baseline = RuntimeSettings::baseline();
        let good =
            Value::Object(Map::from_iter(policy_catalog::HARDWARE_KEYS.iter().map(
                |key| ((*key).to_string(), baseline.deployment_map()[*key].clone()),
            )));
        let active = ActiveGroup {
            kind: "verified".into(),
            digest: policy_catalog::snapshot_digest("hardware", &good),
            resolved_settings: good.clone(),
        };
        let id = "00000000-0000-4000-8000-000000000103";
        let mut journal = Journal::default();
        journal.ever_accepted_typed.insert("hardware".into());
        journal
            .active_groups
            .insert("hardware".into(), active.clone());
        let mut attempt = record(
            "hardware",
            "3",
            "awaiting_startup",
            2,
            json!({"encoder":"not-an-encoder","render_node":"software","cuda_device":0}),
        );
        attempt.scope = "restart".into();
        attempt.known_good = Some(active.clone());
        journal.records.insert(id.into(), attempt);
        fs::write(&path, serde_json::to_vec(&journal).unwrap()).unwrap();
        let candidate = PolicyAgent::bootstrap(&path).unwrap();
        let mut runtime = RuntimeSettings::baseline();
        let mut agent = open_at(&path, &mut runtime);
        assert_eq!(
            finish_test_boot(&mut agent, &candidate, &mut runtime, true),
            BootOutcome::Recovery(id.into())
        );
        assert_eq!(
            agent.journal.records[id].error.as_deref(),
            Some("candidate_verification_failed")
        );
        assert_eq!(
            agent.journal.active_groups["hardware"].digest,
            active.digest
        );
        drop(agent);
        let recovery = PolicyAgent::bootstrap(&path).unwrap();
        assert_eq!(recovery, BootOutcome::RecoveryVerify(id.into()));
        let mut runtime = RuntimeSettings::baseline();
        let mut agent = open_at(&path, &mut runtime);
        assert_eq!(
            finish_test_boot(&mut agent, &recovery, &mut runtime, true),
            BootOutcome::None
        );
        assert_eq!(agent.journal.records[id].phase, "recovered");
        assert_eq!(
            agent.journal.records[id].error.as_deref(),
            Some("candidate_verification_failed")
        );
        assert_eq!(
            agent.journal.active_groups["hardware"].digest,
            active.digest
        );
    }

    #[test]
    fn accepted_hardware_candidate_losing_its_device_after_restart_recovers() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let device = dir.path().join("render-node");
        fs::write(&device, b"accessible at acceptance").unwrap();
        let baseline = RuntimeSettings::baseline_with(&|_| None);
        let good = json!({"encoder":"openh264","render_node":"software","cuda_device":0});
        let active = ActiveGroup {
            kind: "verified".into(),
            digest: policy_catalog::snapshot_digest("hardware", &good),
            resolved_settings: good,
        };
        let id = "00000000-0000-4000-8000-000000000105";
        let candidate =
            json!({"encoder":"va","render_node":device.to_str().unwrap(),"cuda_device":0});
        let mut journal = Journal::default();
        journal.ever_accepted_typed.insert("hardware".into());
        journal
            .active_groups
            .insert("hardware".into(), active.clone());
        let mut attempt = record("hardware", "2", "awaiting_startup", 2, candidate);
        attempt.scope = "restart".into();
        attempt.known_good = Some(active);
        journal.records.insert(id.into(), attempt);
        fs::write(&path, serde_json::to_vec(&journal).unwrap()).unwrap();
        fs::remove_file(&device).unwrap();

        let boot = PolicyAgent::bootstrap(&path).unwrap();
        let mut runtime = baseline;
        let mut agent = open_at(&path, &mut runtime);
        assert_eq!(
            finish_test_boot(&mut agent, &boot, &mut runtime, false),
            BootOutcome::Recovery(id.into())
        );
        let disk = Journal::load(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(
            disk.records[id].error.as_deref(),
            Some("candidate_verification_failed")
        );
        assert_eq!(disk.active_groups["hardware"].kind, "verified");
    }

    #[test]
    fn successful_startup_probe_is_durable_before_connection_opens() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let baseline = RuntimeSettings::baseline_with(&|_| None);
        let hardware = json!({"encoder":"openh264","render_node":"software","cuda_device":0});
        let active = ActiveGroup {
            kind: "seeded".into(),
            digest: policy_catalog::snapshot_digest("hardware", &hardware),
            resolved_settings: hardware.clone(),
        };
        let id = "00000000-0000-4000-8000-000000000108";
        let mut journal = Journal::default();
        journal
            .active_groups
            .insert("hardware".into(), active.clone());
        let mut attempt = record("hardware", "2", "awaiting_startup", 2, hardware);
        attempt.scope = "restart".into();
        attempt.known_good = Some(active);
        journal.records.insert(id.into(), attempt);
        fs::write(&path, serde_json::to_vec(&journal).unwrap()).unwrap();

        let boot = PolicyAgent::bootstrap(&path).unwrap();
        let mut runtime = baseline;
        let mut agent = open_at(&path, &mut runtime);
        let BootPreparation::Verify(readback) = agent.prepare_boot(&boot, &runtime).unwrap() else {
            panic!("valid candidate must require a fresh startup probe");
        };
        assert_eq!(
            agent.complete_boot(*readback, true, &mut runtime).unwrap(),
            BootOutcome::None
        );
        let disk = Journal::load(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(disk.records[id].phase, "applied");
        assert_eq!(
            disk.active_groups["hardware"].digest,
            disk.records[id].digest
        );
        assert!(disk.records[id].verified_at.is_some());
    }

    #[test]
    fn failed_recovery_probe_stays_uncertain_and_keeps_original_failure() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let baseline = RuntimeSettings::baseline_with(&|_| None);
        let hardware = json!({"encoder":"openh264","render_node":"software","cuda_device":0});
        let active = ActiveGroup {
            kind: "verified".into(),
            digest: policy_catalog::snapshot_digest("hardware", &hardware),
            resolved_settings: hardware.clone(),
        };
        let id = "00000000-0000-4000-8000-000000000109";
        let mut journal = Journal::default();
        journal
            .active_groups
            .insert("hardware".into(), active.clone());
        let mut attempt = record("hardware", "2", "recovery_awaiting_startup", 5, hardware);
        attempt.scope = "restart".into();
        attempt.known_good = Some(active);
        attempt.error = Some("candidate_verification_failed".into());
        journal.records.insert(id.into(), attempt);
        fs::write(&path, serde_json::to_vec(&journal).unwrap()).unwrap();

        let boot = PolicyAgent::bootstrap(&path).unwrap();
        let mut runtime = baseline;
        let mut agent = open_at(&path, &mut runtime);
        assert_eq!(
            finish_test_boot(&mut agent, &boot, &mut runtime, false),
            BootOutcome::Uncertain(id.into())
        );
        let disk = Journal::load(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(disk.records[id].phase, "uncertain");
        assert_eq!(
            disk.records[id].error.as_deref(),
            Some("candidate_verification_failed")
        );
    }

    #[test]
    fn startup_baseline_change_fails_deployment_sourced_candidate_before_loading() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let old_baseline = RuntimeSettings::baseline_with(&|_| None);
        let old = old_baseline.deployment_map();
        let good = json!({"encoder":old["encoder"],"render_node":old["render_node"],"cuda_device":old["cuda_device"]});
        let active = ActiveGroup {
            kind: "verified".into(),
            digest: policy_catalog::snapshot_digest("hardware", &good),
            resolved_settings: good.clone(),
        };
        let id = "00000000-0000-4000-8000-000000000106";
        let mut journal = Journal::default();
        journal.ever_accepted_typed.insert("hardware".into());
        journal
            .active_groups
            .insert("hardware".into(), active.clone());
        let mut attempt = record("hardware", "2", "awaiting_startup", 2, good);
        attempt.scope = "restart".into();
        attempt.settings = json!({
            "encoder":{"source":"explicit","value":old["encoder"]},
            "render_node":{"source":"explicit","value":old["render_node"]},
            "cuda_device":{"source":"deployment"}
        });
        attempt.known_good = Some(active);
        journal.records.insert(id.into(), attempt);
        fs::write(&path, serde_json::to_vec(&journal).unwrap()).unwrap();

        let boot = PolicyAgent::bootstrap(&path).unwrap();
        let mut runtime = RuntimeSettings::baseline_with(&|key| {
            (key == "QUASAR_CUDA_DEVICE").then(|| "1".into())
        });
        let mut agent = open_at(&path, &mut runtime);
        assert!(matches!(agent.prepare_boot(&boot, &runtime).unwrap(),
            BootPreparation::Complete(BootOutcome::Recovery(recovery)) if recovery == id));
        let disk = Journal::load(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(
            disk.records[id].error.as_deref(),
            Some("deployment_baseline_changed")
        );
        assert_ne!(disk.records[id].phase, "applied");
    }

    #[test]
    fn journal_survives_reopen_and_rejects_stale_offer() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let mut agent = open_at(&path, &mut runtime);
        seed_idle(&mut agent, &mut runtime);
        let mut offer = idle_offer();
        let result = agent.accept(offer.clone(), &mut runtime);
        assert!(matches!(result,AgentMsg::ConfigPolicyState{phase,..} if phase=="applied"));
        assert_eq!(runtime.idle_timeout_secs, 900);
        let mut restarted = RuntimeSettings::baseline_with(&|_| None);
        let mut reopened = PolicyAgent::open(
            path,
            "host".into(),
            "boot2".into(),
            "conn2".into(),
            &mut restarted,
        )
        .unwrap();
        assert_eq!(restarted.idle_timeout_secs, 900);
        offer.revision = "0".into();
        offer.boot_incarnation = "boot2".into();
        offer.connection_incarnation = "conn2".into();
        assert!(
            matches!(reopened.accept(offer,&mut restarted),AgentMsg::ConfigPolicyState{phase,..} if phase=="failed")
        );
    }

    #[test]
    fn failed_durable_acceptance_does_not_change_next_session_or_poison_retry() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let blocked_tmp = path.with_extension("policy.tmp");
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let original = runtime.idle_timeout_secs;
        let mut agent = open_at(&path, &mut runtime);
        seed_idle(&mut agent, &mut runtime);
        fs::create_dir(&blocked_tmp).unwrap();
        let offer = idle_offer();
        assert!(
            matches!(agent.accept(offer.clone(), &mut runtime), AgentMsg::ConfigPolicyState { phase, .. } if phase == "failed")
        );
        assert_eq!(runtime.idle_timeout_secs, original);
        assert_eq!(agent.journal.high_water.get(IDLE), None);
        fs::remove_dir(&blocked_tmp).unwrap();
        assert!(
            matches!(agent.accept(offer, &mut runtime), AgentMsg::ConfigPolicyState { phase, .. } if phase == "applied")
        );
        assert_eq!(runtime.idle_timeout_secs, 900);
    }

    #[test]
    fn accepted_journal_record_recovers_to_applied_before_runtime_mutation() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let offer = idle_offer();
        let mut accepted = record(IDLE, "1", "accepted", 1, json!({ IDLE: 900 }));
        accepted.digest = offer.content_sha256.clone();
        accepted.settings = offer.settings.clone();
        fs::write(
            &path,
            serde_json::to_vec(&Journal {
                records: BTreeMap::from([("a".into(), accepted)]),
                high_water: BTreeMap::from([(IDLE.into(), 1)]),
                ever_accepted_typed: BTreeSet::from([IDLE.into()]),
                ..Journal::default()
            })
            .unwrap(),
        )
        .unwrap();
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let mut agent = open_at(&path, &mut runtime);
        assert_eq!(runtime.idle_timeout_secs, 900);
        assert!(
            matches!(agent.accept(offer, &mut runtime), AgentMsg::ConfigPolicyState { phase, journal_sequence, .. } if phase == "applied" && journal_sequence == "2")
        );
        assert_eq!(runtime.idle_timeout_secs, 900);
    }

    /// A journal written by the #335 idle-only binary, byte shape and all:
    /// records carry `resolved_idle_timeout_secs` and no `group`, the high-water
    /// mark and active value live in the idle-only fields.
    #[test]
    fn a_335_journal_on_disk_still_loads_recovers_and_fences() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let old = r#"{
            "records": {
                "a": {"host_id":"host","revision":"1","digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                      "phase":"applied","sequence":2,"resolved_idle_timeout_secs":600,
                      "boot_incarnation":"old-boot","connection_incarnation":"old-conn",
                      "settings":{"idle_timeout_secs":{"source":"explicit","value":600}}},
                "b": {"host_id":"host","revision":"2","digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
                      "phase":"accepted","sequence":1,"resolved_idle_timeout_secs":900,
                      "boot_incarnation":"old-boot","connection_incarnation":"old-conn",
                      "settings":{"idle_timeout_secs":{"source":"explicit","value":900}}}
            },
            "high_water_idle_timeout": 2,
            "active_idle_timeout_secs": 600,
            "legacy_overlay": {"gop": 70},
            "legacy_delivery_id": "d",
            "ever_accepted_typed": ["idle_timeout_secs"],
            "active_groups": {},
            "seeded_idle_timeout_secs": 300,
            "verified_idle_digest": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        }"#;
        fs::write(&path, old).unwrap();
        assert_eq!(PolicyAgent::advertised_groups(&path), vec![IDLE]);

        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let mut agent = open_at(&path, &mut runtime);
        assert_eq!(runtime.idle_timeout_secs, 900, "accepted record recovered");
        assert_eq!(runtime.gop, 70, "durable legacy overlay kept");
        let page = agent.inventory_page("inv", "boot", "conn", None).unwrap();
        let AgentMsg::ConfigPolicyJournalInventoryPage {
            entries,
            revision_high_water,
            active_snapshots,
            ..
        } = page
        else {
            panic!("expected inventory page");
        };
        assert_eq!(revision_high_water[IDLE], "2");
        assert_eq!(active_snapshots[IDLE]["kind"], "verified");
        assert_eq!(active_snapshots[IDLE]["digest"], "b".repeat(64));
        for entry in &entries {
            assert_eq!(entry["group"], IDLE);
            assert_eq!(entry["phase"], "applied");
        }
        assert_eq!(
            entries[0]["evidence"]["resolved_settings"],
            json!({ IDLE: 600 })
        );

        let mut stale = idle_offer();
        stale.revision = "1".into();
        stale.content_sha256 = content_digest(&stale);
        let reply = agent.accept(stale, &mut runtime);
        assert_eq!(phase_of(&reply), ("failed", Some("stale_revision")));

        // The rewritten file keeps the #335 fields an older binary requires.
        let disk: Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        assert_eq!(disk["high_water_idle_timeout"], 2);
        assert_eq!(disk["active_idle_timeout_secs"], 900);
        assert_eq!(disk["records"]["b"]["resolved_idle_timeout_secs"], 900);
        assert_eq!(disk["records"]["b"]["group"], IDLE);
    }

    #[test]
    fn seeded_group_accepts_only_current_snapshot_and_deployment_baseline() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let mut agent = open_at(&path, &mut runtime);
        seed_idle(&mut agent, &mut runtime);
        let page = agent
            .inventory_page("inventory", "boot", "conn", None)
            .unwrap();
        let seed_digest = match page {
            AgentMsg::ConfigPolicyJournalInventoryPage {
                active_snapshots, ..
            } => active_snapshots[IDLE]["digest"]
                .as_str()
                .unwrap()
                .to_string(),
            _ => panic!("expected inventory page"),
        };
        let mut offer = idle_offer();
        offer.settings = json!({"idle_timeout_secs":{"source":"deployment"}});
        offer.resolved_settings = json!({"idle_timeout_secs":runtime.idle_timeout_secs});
        offer.content_sha256 = content_digest(&offer);
        let baseline_digest = hex_digest(
            &serde_json::to_vec(&json!({"idle_timeout_secs":runtime.idle_timeout_secs})).unwrap(),
        );
        offer.prerequisites = vec![
            json!({"kind":"deployment_baseline","id":baseline_digest}),
            json!({"kind":"seeded_group_digest","id":seed_digest}),
        ];
        offer.prerequisites_sha256 = facts_digest(&offer.prerequisites).unwrap();
        assert!(
            matches!(agent.accept(offer.clone(), &mut runtime), AgentMsg::ConfigPolicyState { phase, .. } if phase == "applied")
        );

        let mut stale = offer;
        stale.attempt_id = "stale".into();
        stale.revision = "2".into();
        assert!(
            matches!(agent.accept(stale, &mut runtime), AgentMsg::ConfigPolicyState { phase, .. } if phase == "failed")
        );
    }

    #[test]
    fn sticky_typed_snapshot_survives_legacy_map_and_process_reopen() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let baseline = RuntimeSettings::baseline_with(&|_| None);
        let mut runtime = baseline.clone();
        let original_idle = runtime.idle_timeout_secs;
        let mut agent = open_at(&path, &mut runtime);
        seed_idle(&mut agent, &mut runtime);
        let conflict = agent
            .apply_legacy_overlay(
                &baseline,
                &mut runtime,
                &json!({"idle_timeout_secs":60,"gop":90}),
                Some("second"),
            )
            .unwrap();
        assert_eq!(conflict.as_deref(), Some(IDLE));
        assert_eq!(runtime.idle_timeout_secs, original_idle);
        assert_eq!(runtime.gop, 90);
        assert_eq!(agent.legacy_map_applied_id().as_deref(), Some("second"));
        drop(agent);
        assert_eq!(PolicyAgent::advertised_groups(&path), {
            let mut groups = policy_catalog::NEXT_SESSION_GROUPS.to_vec();
            groups.push("hardware");
            groups.sort();
            groups
        });
        let mut restarted = baseline;
        let reopened = PolicyAgent::open(
            path,
            "host".into(),
            "boot2".into(),
            "conn2".into(),
            &mut restarted,
        )
        .unwrap();
        assert!(reopened.has_sticky_ownership());
        assert_eq!(restarted.idle_timeout_secs, original_idle);
        assert_eq!(restarted.gop, 90);
    }

    #[test]
    fn older_v2_binary_composes_known_future_group_and_filters_legacy_key() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let journal = Journal {
            legacy_overlay: Some(json!({"gop":70,"idle_timeout_secs":120})),
            ever_accepted_typed: BTreeSet::from(["gop".into()]),
            active_groups: BTreeMap::from([(
                "gop".into(),
                ActiveGroup {
                    kind: "verified".into(),
                    digest: "a".repeat(64),
                    resolved_settings: json!({"gop":90}),
                },
            )]),
            ..Journal::default()
        };
        fs::write(&path, serde_json::to_vec(&journal).unwrap()).unwrap();
        assert_eq!(PolicyAgent::advertised_groups(&path), vec!["gop"]);
        let baseline = RuntimeSettings::baseline_with(&|_| None);
        let mut runtime = baseline.clone();
        let mut agent = open_at(&path, &mut runtime);
        assert_eq!(runtime.gop, 90);
        assert_eq!(runtime.idle_timeout_secs, 120);
        let conflict = agent
            .apply_legacy_overlay(
                &baseline,
                &mut runtime,
                &json!({"gop":80,"idle_timeout_secs":150}),
                Some("delivery"),
            )
            .unwrap();
        assert_eq!(conflict.as_deref(), Some("gop"));
        assert_eq!(runtime.gop, 90);
        assert_eq!(runtime.idle_timeout_secs, 150);
    }

    #[test]
    fn journal_inventory_pages_are_stable_and_complete() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let mut journal = Journal::default();
        for number in 0..257 {
            let mut entry = record(
                IDLE,
                &number.to_string(),
                "applied",
                2,
                json!({ IDLE: 900 }),
            );
            entry.boot_incarnation = "prior-boot".into();
            entry.connection_incarnation = "prior-conn".into();
            journal
                .records
                .insert(format!("attempt-{number:03}"), entry);
        }
        fs::write(&path, serde_json::to_vec(&journal).unwrap()).unwrap();
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let mut agent = open_at(&path, &mut runtime);
        let first = agent
            .inventory_page("inventory", "boot", "conn", None)
            .unwrap();
        let repeated = agent
            .inventory_page("inventory", "boot", "conn", None)
            .unwrap();
        let (snapshot, next) = match (&first, &repeated) {
            (
                AgentMsg::ConfigPolicyJournalInventoryPage {
                    snapshot_id: a,
                    entries,
                    next_cursor: Some(next),
                    ..
                },
                AgentMsg::ConfigPolicyJournalInventoryPage {
                    snapshot_id: b,
                    entries: replay,
                    ..
                },
            ) => {
                assert_eq!(a, b);
                assert_eq!(entries.len(), 256);
                assert_eq!(entries, replay);
                (a.clone(), next.clone())
            }
            _ => panic!("expected first inventory page and stable replay"),
        };
        let last = agent
            .inventory_page("inventory", "boot", "conn", Some(&next))
            .unwrap();
        assert!(
            matches!(last, AgentMsg::ConfigPolicyJournalInventoryPage { snapshot_id, entries, next_cursor: None, .. } if snapshot_id == snapshot && entries.len() == 1)
        );
    }

    /// Every next-session group is seeded from the latched overlay, advertised,
    /// and executes a typed deployment-source offer against its seed.
    #[test]
    fn every_next_session_group_is_seeded_advertised_and_applies() {
        let dir = tempfile::tempdir().unwrap();
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let groups = policy_catalog::NEXT_SESSION_GROUPS;
        let mut agent = owned_agent(dir.path(), groups, &mut runtime);
        assert!(agent.has_seed());
        assert_eq!(
            PolicyAgent::advertised_groups(&dir.path().join("policy.json")),
            {
                let mut seeded = groups.to_vec();
                seeded.push("hardware");
                seeded.sort();
                seeded
            }
        );
        assert!(agent
            .has_unadvertised_groups(&groups.iter().map(|g| g.to_string()).collect::<Vec<_>>()));
        assert!(agent.has_unadvertised_groups(&[IDLE.into()]));
        let deployed = runtime.deployment_map();
        for group in groups {
            let o = offer(
                &agent,
                group,
                "1",
                group,
                json!({"source":"deployment"}),
                deployed[*group].clone(),
            );
            let reply = agent.accept(o, &mut runtime);
            assert_eq!(phase_of(&reply), ("applied", None), "{group}");
        }
        let page = agent.inventory_page("inv", "boot", "conn", None).unwrap();
        let AgentMsg::ConfigPolicyJournalInventoryPage {
            revision_high_water,
            active_snapshots,
            entries,
            ..
        } = page
        else {
            panic!("expected inventory page");
        };
        assert_eq!(entries.len(), groups.len());
        for group in groups {
            assert_eq!(revision_high_water[*group], "1", "{group}");
            assert_eq!(active_snapshots[*group]["kind"], "verified", "{group}");
        }
    }

    /// A typed snapshot of any group reaches the next session's settings, is
    /// rebuilt onto a fresh baseline on reopen, and wins over a legacy map.
    #[test]
    fn a_typed_group_snapshot_composes_for_next_launch_and_after_reopen() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let baseline = RuntimeSettings::baseline_with(&|_| None);
        let mut runtime = baseline.clone();
        let mut agent = owned_agent(dir.path(), &["abr_mode", "gop"], &mut runtime);
        let reply = agent.accept(explicit(&agent, "g", "3", "gop", json!(45)), &mut runtime);
        assert_eq!(phase_of(&reply), ("applied", None));
        let reply = agent.accept(
            explicit(&agent, "m", "1", "abr_mode", json!("protective")),
            &mut runtime,
        );
        assert_eq!(phase_of(&reply), ("applied", None));
        assert_eq!(runtime.gop, 45);
        assert_eq!(runtime.abr_mode.as_str(), "protective");
        drop(agent);

        let mut restarted = baseline.clone();
        let mut reopened = open_at(&path, &mut restarted);
        assert_eq!(restarted.gop, 45);
        assert_eq!(restarted.abr_mode.as_str(), "protective");
        let conflict = reopened
            .apply_legacy_overlay(
                &baseline,
                &mut restarted,
                &json!({"gop":70,"slices":2}),
                Some("x"),
            )
            .unwrap();
        assert_eq!(conflict.as_deref(), Some("gop"));
        assert_eq!((restarted.gop, restarted.num_slices), (45, 2));
        // The per-group high-water fence: gop is at 3, abr_mode at 1.
        let stale = explicit(&reopened, "g2", "2", "gop", json!(50));
        assert_eq!(
            phase_of(&reopened.accept(stale, &mut restarted)),
            ("failed", Some("stale_revision"))
        );
        let other = explicit(&reopened, "m2", "2", "abr_mode", json!("off"));
        assert_eq!(
            phase_of(&reopened.accept(other, &mut restarted)),
            ("applied", None)
        );
    }

    /// A rejection that describes the offer is never-retry, has no effect on
    /// the journal or the next session, and is distinguishable from a
    /// transient failure.
    #[test]
    fn a_home_root_symlink_cannot_escape_the_deployment_mount() {
        let dir = tempfile::tempdir().unwrap();
        let mount = dir.path().join("homes");
        let outside = dir.path().join("outside");
        fs::create_dir(&mount).unwrap();
        fs::create_dir(&outside).unwrap();
        std::os::unix::fs::symlink(&outside, mount.join("link")).unwrap();
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        runtime.home_root = mount.to_str().unwrap().into();
        let mut agent = owned_agent(dir.path(), &["home_root"], &mut runtime);
        let before = serde_json::to_value(&agent.journal).unwrap();
        let escaped = mount.join("link").join("new");
        let reply = agent.accept(
            explicit(
                &agent,
                "escape",
                "1",
                "home_root",
                json!(escaped.to_str().unwrap()),
            ),
            &mut runtime,
        );
        assert_eq!(
            phase_of(&reply),
            ("failed", Some("home_root_outside_mount"))
        );
        assert_eq!(runtime.home_root, mount.to_str().unwrap());
        assert_eq!(serde_json::to_value(&agent.journal).unwrap(), before);

        let valid = mount.join("new");
        let reply = agent.accept(
            explicit(
                &agent,
                "inside",
                "1",
                "home_root",
                json!(valid.to_str().unwrap()),
            ),
            &mut runtime,
        );
        assert_eq!(phase_of(&reply), ("applied", None));
    }

    /// A rejection that describes the offer is never-retry, has no effect on
    /// the journal or the next session, and is distinguishable from a
    /// transient failure.
    #[test]
    fn invalid_offers_fail_with_never_retry_codes_and_no_effect() {
        let dir = tempfile::tempdir().unwrap();
        let mounts = tempfile::tempdir().unwrap();
        let mount = mounts.path().join("homes");
        fs::create_dir(&mount).unwrap();
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        runtime.home_root = mount.to_str().unwrap().into();
        let groups = [
            "abr_floor_ratio",
            "abr_ladder_res_engage_frac",
            "gop",
            "home_root",
            "nvidia_lib32_path",
        ];
        let mut agent = owned_agent(dir.path(), &groups, &mut runtime);
        let before = runtime.deployment_map();
        let journal_before = serde_json::to_value(&agent.journal).unwrap();
        let cases = [
            (explicit(&agent, "1", "1", "gop", json!(0)), "invalid_value"),
            (
                explicit(&agent, "2", "1", "abr_floor_ratio", json!(0)),
                "invalid_value",
            ),
            (
                explicit(&agent, "3", "1", "abr_ladder_res_engage_frac", json!(0.9)),
                "cross_key_invalid",
            ),
            (
                explicit(&agent, "4", "1", "home_root", json!("/elsewhere")),
                "home_root_outside_mount",
            ),
            (
                explicit(
                    &agent,
                    "5",
                    "1",
                    "nvidia_lib32_path",
                    json!(mounts.path().join("absent").to_str().unwrap()),
                ),
                "path_inaccessible",
            ),
            (
                offer(
                    &agent,
                    "6",
                    "1",
                    "gop",
                    json!({"source":"automatic"}),
                    json!(60),
                ),
                "unsupported_source",
            ),
            (
                {
                    let mut o = explicit(&agent, "7", "1", "gop", json!(60));
                    o.resolved_settings = json!({"gop": 61});
                    o.content_sha256 = content_digest(&o);
                    o
                },
                "resolved_mismatch",
            ),
            (
                {
                    let mut o = explicit(&agent, "8", "1", "gop", json!(60));
                    o.group = "hardware".into();
                    o
                },
                "unsupported_group",
            ),
        ];
        for (o, want) in cases {
            let reply = agent.accept(o, &mut runtime);
            assert_eq!(phase_of(&reply), ("failed", Some(want)));
            assert!(INVALID_REJECTIONS.contains(&want), "{want}");
        }
        assert_eq!(runtime.deployment_map(), before, "no next-session effect");
        assert_eq!(
            serde_json::to_value(&agent.journal).unwrap(),
            journal_before,
            "no durable record"
        );

        // Transient: a grant whose snapshot fact is stale, and a failed fsync.
        let mut o = explicit(&agent, "9", "1", "gop", json!(60));
        o.prerequisites = vec![json!({"kind":"seeded_group_digest","id":"c".repeat(64)})];
        o.prerequisites_sha256 = facts_digest(&o.prerequisites).unwrap();
        let reply = agent.accept(o, &mut runtime);
        assert_eq!(phase_of(&reply), ("failed", Some("prerequisite_mismatch")));
        let blocked = dir.path().join("policy.policy.tmp");
        fs::create_dir(&blocked).unwrap();
        let reply = agent.accept(explicit(&agent, "10", "1", "gop", json!(60)), &mut runtime);
        assert_eq!(phase_of(&reply), ("failed", Some("journal_write_failed")));
        for transient in [
            "prerequisite_mismatch",
            "journal_write_failed",
            "stale_grant",
        ] {
            assert!(!INVALID_REJECTIONS.contains(&transient));
        }
        fs::remove_dir(&blocked).unwrap();

        // Valid storage effects: a root inside the mount, an accessible dir.
        let inside = mount.join("v2");
        let reply = agent.accept(
            explicit(
                &agent,
                "11",
                "1",
                "home_root",
                json!(inside.to_str().unwrap()),
            ),
            &mut runtime,
        );
        assert_eq!(phase_of(&reply), ("applied", None));
        assert_eq!(runtime.home_root, inside.to_str().unwrap());
        let lib = mounts.path().to_str().unwrap();
        let reply = agent.accept(
            explicit(&agent, "12", "1", "nvidia_lib32_path", json!(lib)),
            &mut runtime,
        );
        assert_eq!(phase_of(&reply), ("applied", None));
        assert_eq!(runtime.nvidia_lib32_path, lib);
    }

    /// Crash injection on a durable temp journal for the generic record shape:
    /// the process dies right after the `accepted` fsync, or right after the
    /// `applied` fsync but before the in-memory settings mutation.
    #[test]
    fn a_crash_after_either_fsync_recovers_the_group_exactly_once() {
        use std::sync::atomic::Ordering;
        for crash_at in [1, 2] {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join("policy.json");
            let baseline = RuntimeSettings::baseline_with(&|_| None);
            let mut runtime = baseline.clone();
            let mut agent = owned_agent(dir.path(), &["gop"], &mut runtime);
            let o = explicit(&agent, "g", "4", "gop", json!(33));
            agent.crash_after_writes.store(crash_at, Ordering::SeqCst);
            let reply = agent.accept(o.clone(), &mut runtime);
            assert_eq!(phase_of(&reply), ("failed", Some("journal_write_failed")));
            assert_ne!(
                runtime.gop, 33,
                "a failed report never mutates this process"
            );
            drop(agent);

            let disk: Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
            let want = if crash_at == 1 { "accepted" } else { "applied" };
            assert_eq!(disk["records"]["g"]["phase"], want);
            assert_eq!(disk["records"]["g"]["group"], "gop");
            assert_eq!(
                disk["records"]["g"]["resolved_settings"],
                json!({"gop": 33})
            );
            assert!(disk["records"]["g"]
                .get("resolved_idle_timeout_secs")
                .is_none());
            assert_eq!(disk["high_water"]["gop"], 4);

            let mut restarted = baseline.clone();
            let mut reopened = open_at(&path, &mut restarted);
            assert_eq!(restarted.gop, 33, "crash_at={crash_at}");
            let reply = reopened.accept(o.clone(), &mut restarted);
            assert!(
                matches!(&reply, AgentMsg::ConfigPolicyState { phase, journal_sequence, .. }
                    if phase == "applied" && journal_sequence == "2"),
                "crash_at={crash_at}: {reply:?}"
            );
            let page = reopened
                .inventory_page("inv", "boot", "conn", None)
                .unwrap();
            let AgentMsg::ConfigPolicyJournalInventoryPage {
                active_snapshots, ..
            } = page
            else {
                panic!("expected inventory page");
            };
            assert_eq!(active_snapshots["gop"]["kind"], "verified");
            assert_eq!(active_snapshots["gop"]["digest"], o.content_sha256.as_str());
        }
    }

    /// agent-api.md §RH05: an unstarted grant resolved from a baseline this
    /// process no longer has is `deployment_baseline_changed` — transient, so
    /// the control plane re-resolves — checked before resolution, and leaves
    /// the journal, high-water and last verified snapshot untouched.
    #[test]
    fn a_changed_deployment_baseline_rejects_before_acceptance() {
        let dir = tempfile::tempdir().unwrap();
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let mut agent = owned_agent(dir.path(), &["gop", "slices"], &mut runtime);
        let reply = agent.accept(explicit(&agent, "v", "1", "slices", json!(3)), &mut runtime);
        assert_eq!(phase_of(&reply).0, "applied");
        let before = runtime.deployment_map();
        let journal_before = serde_json::to_value(&agent.journal).unwrap();

        let current = runtime.deployment_map()["gop"].as_u64().unwrap();
        let stale = offer(
            &agent,
            "s",
            "2",
            "gop",
            json!({"source":"deployment"}),
            json!(current + 30),
        );
        let reply = agent.accept(stale, &mut runtime);
        assert_eq!(
            phase_of(&reply),
            ("failed", Some("deployment_baseline_changed"))
        );
        assert!(!INVALID_REJECTIONS.contains(&"deployment_baseline_changed"));
        assert_eq!(runtime.deployment_map(), before, "no next-session effect");
        assert_eq!(
            serde_json::to_value(&agent.journal).unwrap(),
            journal_before,
            "no durable acceptance, high-water or snapshot change"
        );

        // A current fact with a wrong resolution is still the offer's fault.
        let mut wrong = offer(
            &agent,
            "w",
            "2",
            "gop",
            json!({"source":"deployment"}),
            json!(current),
        );
        wrong.resolved_settings = json!({"gop": current + 30});
        wrong.content_sha256 = content_digest(&wrong);
        let reply = agent.accept(wrong, &mut runtime);
        assert_eq!(phase_of(&reply), ("failed", Some("resolved_mismatch")));

        // Re-resolved from the current baseline, the group applies.
        let fresh = offer(
            &agent,
            "f",
            "2",
            "gop",
            json!({"source":"deployment"}),
            json!(current),
        );
        let reply = agent.accept(fresh, &mut runtime);
        assert_eq!(phase_of(&reply).0, "applied");
    }
}
