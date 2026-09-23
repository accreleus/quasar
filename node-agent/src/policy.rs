//! Durable RH05 next-session policy acceptance for the idle timeout slice.
//! The journal is committed before mutating the session settings snapshot.

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::PathBuf;
use time::{format_description::well_known::Rfc3339, OffsetDateTime};

use crate::messages::AgentMsg;
use crate::session::settings::RuntimeSettings;

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

#[derive(Debug, Clone, Serialize, Deserialize)]
struct Record {
    #[serde(default)]
    host_id: String,
    revision: String,
    digest: String,
    phase: String,
    sequence: u64,
    resolved_idle_timeout_secs: u64,
    boot_incarnation: String,
    connection_incarnation: String,
    #[serde(default)]
    settings: Value,
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
    high_water_idle_timeout: u64,
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

struct InventorySnapshot {
    inventory_id: String,
    snapshot_id: String,
    entries: Vec<Value>,
    high_water: BTreeMap<String, String>,
    active_snapshots: BTreeMap<String, Value>,
}

pub struct PolicyAgent {
    path: PathBuf,
    host_id: String,
    boot_incarnation: String,
    connection_incarnation: String,
    journal: Journal,
    deployment_baseline: RuntimeSettings,
    inventory: Option<InventorySnapshot>,
}

type ApplyOutcome = (&'static str, u64, Option<&'static str>, Option<Value>);

impl PolicyAgent {
    pub fn advertised_groups(path: &PathBuf) -> Vec<String> {
        match fs::read(path)
            .ok()
            .and_then(|raw| serde_json::from_slice::<Journal>(&raw).ok())
        {
            Some(journal) => {
                let mut groups = journal.ever_accepted_typed;
                groups.extend(journal.active_groups.keys().cloned());
                if journal.seeded_idle_timeout_secs.is_some() {
                    groups.insert("idle_timeout_secs".into());
                }
                groups.into_iter().collect()
            }
            _ => Vec::new(),
        }
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
    pub fn has_idle_seed(&self) -> bool {
        self.journal
            .active_groups
            .get("idle_timeout_secs")
            .is_some_and(|active| active.kind == "seeded")
            || self.journal.seeded_idle_timeout_secs.is_some()
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
        if !self
            .journal
            .ever_accepted_typed
            .contains("idle_timeout_secs")
            && self.journal.verified_idle_digest.is_none()
        {
            // The seed records what this process actually latched. It is an
            // active recovery target, never RH05 application proof.
            self.journal.seeded_idle_timeout_secs = Some(composed.idle_timeout_secs);
            self.journal.active_idle_timeout_secs = Some(composed.idle_timeout_secs);
            self.journal.active_groups.insert(
                "idle_timeout_secs".into(),
                ActiveGroup {
                    kind: "seeded".into(),
                    digest: active_idle_digest(composed.idle_timeout_secs),
                    resolved_settings: json!({"idle_timeout_secs":composed.idle_timeout_secs}),
                },
            );
        }
        if let Err(err) = self.persist() {
            self.journal = previous;
            return Err(format!("legacy_journal_write_failed: {err}"));
        }
        *settings = composed;
        Ok(conflict)
    }

    fn compose_typed_snapshots(&self, settings: &mut RuntimeSettings) -> Result<(), String> {
        let known = settings.deployment_map();
        for group in &self.journal.ever_accepted_typed {
            let active = self
                .journal
                .active_groups
                .get(group)
                .ok_or("typed_snapshot_missing")?;
            let resolved = active
                .resolved_settings
                .as_object()
                .ok_or("typed_snapshot_invalid")?;
            if resolved.is_empty()
                || resolved
                    .iter()
                    .any(|(key, _)| group_for_key(key) != group || !known.contains_key(key))
            {
                return Err("typed_snapshot_unknown_key".into());
            }
            settings.apply_json(&active.resolved_settings);
            let applied = settings.deployment_map();
            if resolved
                .iter()
                .any(|(key, value)| applied.get(key) != Some(value))
            {
                return Err("typed_snapshot_invalid_value".into());
            }
        }
        Ok(())
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
            let mut high_water = BTreeMap::new();
            high_water.insert(
                "idle_timeout_secs".into(),
                self.journal.high_water_idle_timeout.to_string(),
            );
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
                    "group":"idle_timeout_secs",
                    "revision":record.revision,
                    "content_sha256":record.digest,
                    "scope":"next_session",
                    "grant_boot_incarnation":record.boot_incarnation,
                    "grant_connection_incarnation":record.connection_incarnation,
                    "journal_sequence":record.sequence.to_string(),
                    "phase":record.phase,
                    "active_scope":if applied {Some("next_session")} else {None},
                    "evidence":if applied {Some(json!({
                        "revision":record.revision,
                        "content_sha256":record.digest,
                        "resolved_settings":{"idle_timeout_secs":record.resolved_idle_timeout_secs},
                        "agent_process_id":std::process::id().to_string(),
                        "observed_at":OffsetDateTime::now_utc().format(&Rfc3339).unwrap_or_default(),
                        "evidence_ids":[]
                    }))} else {None},
                    "error":null
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
        let mut journal: Journal = match fs::read(&path) {
            Ok(bytes) => serde_json::from_slice(&bytes).map_err(std::io::Error::other)?,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Journal::default(),
            Err(e) => return Err(e),
        };
        if !journal.active_groups.contains_key("idle_timeout_secs") {
            if let Some(timeout) = journal.active_idle_timeout_secs {
                let (kind, digest) = if let Some(digest) = &journal.verified_idle_digest {
                    ("verified", digest.clone())
                } else {
                    ("seeded", active_idle_digest(timeout))
                };
                journal.active_groups.insert(
                    "idle_timeout_secs".into(),
                    ActiveGroup {
                        kind: kind.into(),
                        digest,
                        resolved_settings: json!({"idle_timeout_secs":timeout}),
                    },
                );
            }
        }
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
        };
        // A crash after durable acceptance but before activation cannot wait
        // for a new offer: admission is still gated on complete inventory.
        // Next-session activation is local and safe to finish idempotently.
        let accepted: Vec<_> = agent
            .journal
            .records
            .iter()
            .filter(|(_, record)| record.phase == "accepted")
            .map(|(id, _)| id.clone())
            .collect();
        if accepted.len() == 1 {
            let record = agent.journal.records.get_mut(&accepted[0]).unwrap();
            record.phase = "applied".into();
            record.sequence += 1;
            let timeout = record.resolved_idle_timeout_secs;
            agent.journal.active_idle_timeout_secs = Some(timeout);
            agent.journal.verified_idle_digest = Some(record.digest.clone());
            agent.journal.active_groups.insert(
                "idle_timeout_secs".into(),
                ActiveGroup {
                    kind: "verified".into(),
                    digest: record.digest.clone(),
                    resolved_settings: json!({"idle_timeout_secs":timeout}),
                },
            );
            agent.persist()?;
            settings.idle_timeout_secs = timeout;
        }
        agent
            .compose_typed_snapshots(settings)
            .map_err(std::io::Error::other)?;
        Ok(agent)
    }

    fn persist(&self) -> std::io::Result<()> {
        let tmp = self.path.with_extension("policy.tmp");
        let bytes = serde_json::to_vec(&self.journal).map_err(std::io::Error::other)?;
        let mut f = OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(&tmp)?;
        f.write_all(&bytes)?;
        f.sync_all()?;
        fs::rename(&tmp, &self.path)?;
        if let Some(parent) = self.path.parent() {
            OpenOptions::new().read(true).open(parent)?.sync_all()?;
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
        if offer.group != "idle_timeout_secs" || offer.scope != "next_session" {
            return Err("unsupported_group".into());
        }
        let expires =
            OffsetDateTime::parse(&offer.expires_at, &Rfc3339).map_err(|_| "invalid_expiry")?;
        if expires <= OffsetDateTime::now_utc() {
            return Err("grant_expired".into());
        }
        let revision = parse_revision(&offer.revision)?;
        if revision < self.journal.high_water_idle_timeout {
            return Err("stale_revision".into());
        }
        if !self
            .journal
            .ever_accepted_typed
            .contains("idle_timeout_secs")
        {
            return Err("group_execution_unavailable".into());
        }
        let content = json!({"group":offer.group,"scope":offer.scope,"revision":offer.revision,"settings":offer.settings,"resolved_settings":offer.resolved_settings});
        let digest = hex_digest(&serde_json::to_vec(&content).map_err(|_| "invalid_content")?);
        if digest != offer.content_sha256 {
            return Err("content_mismatch".into());
        }
        if let Some(existing) = self.journal.records.get(&offer.attempt_id).cloned() {
            if existing.digest != digest
                || existing.revision != offer.revision
                || existing.boot_incarnation != offer.boot_incarnation
                || existing.connection_incarnation != offer.connection_incarnation
            {
                return Err("attempt_conflict".into());
            }
            if existing.phase != "applied" {
                let durable = self.journal.clone();
                self.journal.active_idle_timeout_secs = Some(existing.resolved_idle_timeout_secs);
                self.journal.verified_idle_digest = Some(existing.digest.clone());
                self.journal.active_groups.insert("idle_timeout_secs".into(), ActiveGroup {
                    kind: "verified".into(), digest: existing.digest.clone(),
                    resolved_settings: json!({"idle_timeout_secs":existing.resolved_idle_timeout_secs}),
                });
                if let Some(record) = self.journal.records.get_mut(&offer.attempt_id) {
                    record.phase = "applied".into();
                    record.sequence += 1;
                }
                if self.persist().is_err() {
                    self.journal = durable;
                    return Err("journal_write_failed".into());
                }
            }
            settings.idle_timeout_secs = existing.resolved_idle_timeout_secs;
            let evidence = self.evidence(offer, existing.resolved_idle_timeout_secs);
            return Ok((
                "applied",
                self.journal.records[&offer.attempt_id].sequence,
                Some("next_session"),
                Some(evidence),
            ));
        }
        let choice = offer
            .settings
            .get("idle_timeout_secs")
            .ok_or("missing_setting")?;
        let timeout = match choice.get("source").and_then(Value::as_str) {
            Some("explicit") => choice
                .get("value")
                .and_then(Value::as_u64)
                .ok_or("invalid_value")?,
            Some("deployment") => self.deployment_baseline.idle_timeout_secs,
            _ => return Err("unsupported_source".into()),
        };
        let active = self
            .journal
            .active_groups
            .get("idle_timeout_secs")
            .ok_or("active_snapshot_missing")?;
        let snapshot = match active.kind.as_str() {
            "verified" => ("last_verified_group_digest", active.digest.clone()),
            "seeded" => ("seeded_group_digest", active.digest.clone()),
            _ => return Err("active_snapshot_invalid".into()),
        };
        let mut facts = vec![json!({"kind":snapshot.0,"id":snapshot.1})];
        if choice.get("source").and_then(Value::as_str) == Some("deployment") {
            facts.push(json!({"kind":"deployment_baseline","id":hex_digest(&serde_json::to_vec(&json!({"idle_timeout_secs":timeout})).map_err(|_| "invalid_baseline")?)}));
        }
        facts.sort_by(|a, b| {
            (a["kind"].as_str(), a["id"].as_str()).cmp(&(b["kind"].as_str(), b["id"].as_str()))
        });
        if offer.prerequisites != facts || offer.prerequisites_sha256 != facts_digest(&facts)? {
            return Err("prerequisite_mismatch".into());
        }
        if offer
            .resolved_settings
            .get("idle_timeout_secs")
            .and_then(Value::as_u64)
            != Some(timeout)
        {
            return Err("resolved_mismatch".into());
        }
        // Equal-revision deployment re-resolution may change only the
        // resolved value and its baseline prerequisite, never the choice.
        if revision == self.journal.high_water_idle_timeout {
            let same = self
                .journal
                .records
                .values()
                .any(|r| r.revision == offer.revision && r.settings == offer.settings);
            if !same {
                return Err("revision_conflict".into());
            }
        }
        let durable = self.journal.clone();
        self.journal.records.insert(
            offer.attempt_id.clone(),
            Record {
                host_id: offer.host_id.clone(),
                revision: offer.revision.clone(),
                digest: digest.clone(),
                phase: "accepted".into(),
                sequence: 1,
                resolved_idle_timeout_secs: timeout,
                boot_incarnation: offer.boot_incarnation.clone(),
                connection_incarnation: offer.connection_incarnation.clone(),
                settings: offer.settings.clone(),
            },
        );
        self.journal.high_water_idle_timeout = revision;
        if self.persist().is_err() {
            self.journal = durable;
            return Err("journal_write_failed".into());
        }
        // SessionManager copies runtime_settings at launch. Existing sessions
        // retain their original copy while the next launch sees this value.
        let accepted = self.journal.clone();
        self.journal.active_idle_timeout_secs = Some(timeout);
        self.journal.verified_idle_digest = Some(digest.clone());
        self.journal.active_groups.insert(
            "idle_timeout_secs".into(),
            ActiveGroup {
                kind: "verified".into(),
                digest: digest.clone(),
                resolved_settings: json!({"idle_timeout_secs":timeout}),
            },
        );
        if let Some(record) = self.journal.records.get_mut(&offer.attempt_id) {
            record.phase = "applied".into();
            record.sequence = 2;
        }
        if self.persist().is_err() {
            self.journal = accepted;
            return Err("journal_write_failed".into());
        }
        settings.idle_timeout_secs = timeout;
        Ok((
            "applied",
            2,
            Some("next_session"),
            Some(self.evidence(offer, timeout)),
        ))
    }

    fn evidence(&self, offer: &Offer, timeout: u64) -> Value {
        json!({
            "revision":offer.revision,
            "content_sha256":offer.content_sha256,
            "resolved_settings":{"idle_timeout_secs":timeout},
            "agent_process_id":std::process::id().to_string(),
            "observed_at":OffsetDateTime::now_utc().format(&Rfc3339).unwrap_or_default(),
            "evidence_ids":[]
        })
    }
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

fn active_idle_digest(timeout: u64) -> String {
    hex_digest(
        &serde_json::to_vec(
            &json!({"group":"idle_timeout_secs","resolved_settings":{"idle_timeout_secs":timeout}}),
        )
        .expect("idle snapshot is serializable"),
    )
}

fn group_for_key(key: &str) -> &str {
    match key {
        "encoder" | "render_node" | "cuda_device" => "hardware",
        other => other,
    }
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

#[cfg(test)]
mod tests {
    use super::*;

    fn idle_offer() -> Offer {
        let baseline = RuntimeSettings::baseline_with(&|_| None);
        let prerequisites = vec![
            json!({"kind":"seeded_group_digest","id":active_idle_digest(baseline.idle_timeout_secs)}),
        ];
        let mut offer = Offer {
            attempt_id: "a".into(),
            host_id: "host".into(),
            boot_incarnation: "boot".into(),
            connection_incarnation: "conn".into(),
            group: "idle_timeout_secs".into(),
            revision: "1".into(),
            content_sha256: String::new(),
            scope: "next_session".into(),
            expires_at: "2099-01-01T00:00:00Z".into(),
            prerequisites_sha256: facts_digest(&prerequisites).unwrap(),
            prerequisites,
            settings: json!({"idle_timeout_secs":{"source":"explicit","value":900}}),
            resolved_settings: json!({"idle_timeout_secs":900}),
        };
        offer.content_sha256 = hex_digest(&serde_json::to_vec(&json!({"group":offer.group,"scope":offer.scope,"revision":offer.revision,"settings":offer.settings,"resolved_settings":offer.resolved_settings})).unwrap());
        offer
    }

    fn seed_idle(agent: &mut PolicyAgent, runtime: &mut RuntimeSettings) {
        let baseline = RuntimeSettings::baseline_with(&|_| None);
        agent
            .apply_legacy_overlay(&baseline, runtime, &json!({}), Some("delivery"))
            .unwrap();
        agent
            .confirm_groups(&["idle_timeout_secs".into()], &["idle_timeout_secs".into()])
            .unwrap();
    }

    #[test]
    fn journal_survives_reopen_and_rejects_stale_offer() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let mut agent = PolicyAgent::open(
            path.clone(),
            "host".into(),
            "boot".into(),
            "conn".into(),
            &mut runtime,
        )
        .unwrap();
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
        let mut agent = PolicyAgent::open(
            path,
            "host".into(),
            "boot".into(),
            "conn".into(),
            &mut runtime,
        )
        .unwrap();
        seed_idle(&mut agent, &mut runtime);
        fs::create_dir(&blocked_tmp).unwrap();
        let offer = idle_offer();
        assert!(
            matches!(agent.accept(offer.clone(), &mut runtime), AgentMsg::ConfigPolicyState { phase, .. } if phase == "failed")
        );
        assert_eq!(runtime.idle_timeout_secs, original);
        assert_eq!(agent.journal.high_water_idle_timeout, 0);
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
        let mut records = BTreeMap::new();
        records.insert(
            "a".into(),
            Record {
                host_id: "host".into(),
                revision: "1".into(),
                digest: offer.content_sha256.clone(),
                phase: "accepted".into(),
                sequence: 1,
                resolved_idle_timeout_secs: 900,
                boot_incarnation: "boot".into(),
                connection_incarnation: "conn".into(),
                settings: offer.settings.clone(),
            },
        );
        fs::write(
            &path,
            serde_json::to_vec(&Journal {
                records,
                high_water_idle_timeout: 1,
                active_idle_timeout_secs: None,
                ever_accepted_typed: BTreeSet::from(["idle_timeout_secs".into()]),
                ..Journal::default()
            })
            .unwrap(),
        )
        .unwrap();
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let mut agent = PolicyAgent::open(
            path,
            "host".into(),
            "boot".into(),
            "conn".into(),
            &mut runtime,
        )
        .unwrap();
        assert!(
            matches!(agent.accept(offer, &mut runtime), AgentMsg::ConfigPolicyState { phase, journal_sequence, .. } if phase == "applied" && journal_sequence == "2")
        );
        assert_eq!(runtime.idle_timeout_secs, 900);
    }

    #[test]
    fn seeded_group_accepts_only_current_snapshot_and_deployment_baseline() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("policy.json");
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let mut agent = PolicyAgent::open(
            path,
            "host".into(),
            "boot".into(),
            "conn".into(),
            &mut runtime,
        )
        .unwrap();
        agent
            .apply_legacy_overlay(
                &RuntimeSettings::baseline_with(&|_| None),
                &mut runtime,
                &json!({}),
                Some("delivery"),
            )
            .unwrap();
        agent
            .confirm_groups(&["idle_timeout_secs".into()], &["idle_timeout_secs".into()])
            .unwrap();
        let page = agent
            .inventory_page("inventory", "boot", "conn", None)
            .unwrap();
        let seed_digest = match page {
            AgentMsg::ConfigPolicyJournalInventoryPage {
                active_snapshots, ..
            } => active_snapshots["idle_timeout_secs"]["digest"]
                .as_str()
                .unwrap()
                .to_string(),
            _ => panic!("expected inventory page"),
        };
        let mut offer = idle_offer();
        offer.settings = json!({"idle_timeout_secs":{"source":"deployment"}});
        offer.resolved_settings = json!({"idle_timeout_secs":runtime.idle_timeout_secs});
        offer.content_sha256 = hex_digest(&serde_json::to_vec(&json!({"group":offer.group,"scope":offer.scope,"revision":offer.revision,"settings":offer.settings,"resolved_settings":offer.resolved_settings})).unwrap());
        let baseline_digest = hex_digest(
            &serde_json::to_vec(&json!({"idle_timeout_secs":runtime.idle_timeout_secs})).unwrap(),
        );
        offer.prerequisites = vec![
            json!({"kind":"deployment_baseline","id":baseline_digest}),
            json!({"kind":"seeded_group_digest","id":seed_digest}),
        ];
        let mut bytes = Vec::new();
        for fact in &offer.prerequisites {
            bytes.extend_from_slice(fact["kind"].as_str().unwrap().as_bytes());
            bytes.push(0);
            bytes.extend_from_slice(fact["id"].as_str().unwrap().as_bytes());
            bytes.push(b'\n');
        }
        offer.prerequisites_sha256 = hex_digest(&bytes);
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
        let mut agent = PolicyAgent::open(
            path.clone(),
            "host".into(),
            "boot".into(),
            "conn".into(),
            &mut runtime,
        )
        .unwrap();
        seed_idle(&mut agent, &mut runtime);
        let conflict = agent
            .apply_legacy_overlay(
                &baseline,
                &mut runtime,
                &json!({"idle_timeout_secs":60,"gop":90}),
                Some("second"),
            )
            .unwrap();
        assert_eq!(conflict.as_deref(), Some("idle_timeout_secs"));
        assert_eq!(runtime.idle_timeout_secs, original_idle);
        assert_eq!(runtime.gop, 90);
        assert_eq!(agent.legacy_map_applied_id().as_deref(), Some("second"));
        drop(agent);
        assert_eq!(
            PolicyAgent::advertised_groups(&path),
            vec!["idle_timeout_secs"]
        );
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
        let mut agent = PolicyAgent::open(
            path,
            "host".into(),
            "boot".into(),
            "conn".into(),
            &mut runtime,
        )
        .unwrap();
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
            journal.records.insert(
                format!("attempt-{number:03}"),
                Record {
                    host_id: "host".into(),
                    revision: number.to_string(),
                    digest: "a".repeat(64),
                    phase: "applied".into(),
                    sequence: 2,
                    resolved_idle_timeout_secs: 900,
                    boot_incarnation: "prior-boot".into(),
                    connection_incarnation: "prior-conn".into(),
                    settings: json!({"idle_timeout_secs":{"source":"explicit","value":900}}),
                },
            );
        }
        fs::write(&path, serde_json::to_vec(&journal).unwrap()).unwrap();
        let mut runtime = RuntimeSettings::baseline_with(&|_| None);
        let mut agent = PolicyAgent::open(
            path,
            "host".into(),
            "boot".into(),
            "conn".into(),
            &mut runtime,
        )
        .unwrap();
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
}
