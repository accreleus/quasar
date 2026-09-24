//! Multi-version managed-image history. A complete daemon scan is mandatory for
//! every authoritative snapshot; a current-only legacy record cannot establish it.
use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::PathBuf;
use std::sync::Mutex;

use serde::{Deserialize, Serialize};

use crate::messages::{ImageIdentity, ImageVersionEntry};
use crate::runtime::DaemonImage;

#[derive(Default, Clone, Serialize, Deserialize)]
struct Disk {
    records: BTreeMap<String, BTreeMap<String, Record>>,
}

#[derive(Clone, Serialize, Deserialize)]
struct Record {
    image_ref: String,
    runtime_image_id: String,
}

pub struct VersionInventory {
    path: PathBuf,
    disk: Mutex<Disk>,
    /// Never persisted as a validity claim; each process and each failed scan
    /// must re-prove completeness from the daemon and control-plane identities.
    complete: Mutex<bool>,
    authority_received: Mutex<bool>,
    last: Mutex<Vec<ImageVersionEntry>>,
}

impl VersionInventory {
    pub fn open(path: PathBuf) -> std::io::Result<Self> {
        let disk = match fs::read(&path) {
            Ok(bytes) => serde_json::from_slice(&bytes)
                .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))?,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Disk::default(),
            Err(e) => return Err(e),
        };
        let last = disk
            .records
            .iter()
            .flat_map(|(image_id, versions)| {
                versions
                    .iter()
                    .map(move |(version, record)| ImageVersionEntry {
                        image_id: image_id.clone(),
                        version: version.clone(),
                        image_ref: record.image_ref.clone(),
                        runtime_image_id: record.runtime_image_id.clone(),
                        state: "unknown".into(),
                    })
            })
            .collect();
        Ok(Self {
            path,
            disk: Mutex::new(disk),
            complete: Mutex::new(false),
            authority_received: Mutex::new(false),
            last: Mutex::new(last),
        })
    }

    fn save(&self, disk: &Disk) -> std::io::Result<()> {
        if let Some(parent) = self.path.parent() {
            fs::create_dir_all(parent)?;
        }
        let tmp = self.path.with_extension("versions.tmp");
        let _ = fs::remove_file(&tmp);
        let mut f = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&tmp)?;
        f.write_all(&serde_json::to_vec(disk)?)?;
        f.sync_all()?;
        fs::rename(&tmp, &self.path)?;
        if let Some(parent) = self.path.parent() {
            fs::File::open(parent)?.sync_all()?;
        }
        Ok(())
    }

    pub fn remember(&self, identity: &ImageIdentity) -> std::io::Result<()> {
        let mut guard = self.disk.lock().unwrap();
        if let Some(old) = guard
            .records
            .get(&identity.image_id)
            .and_then(|versions| versions.get(&identity.version))
        {
            if old.image_ref != identity.image_ref {
                self.revoke();
                return Err(std::io::Error::other("frozen managed identity changed"));
            }
            return Ok(());
        }
        let mut next = guard.clone();
        next.records
            .entry(identity.image_id.clone())
            .or_default()
            .insert(
                identity.version.clone(),
                Record {
                    image_ref: identity.image_ref.clone(),
                    runtime_image_id: String::new(),
                },
            );
        self.save(&next)?;
        *guard = next;
        self.last.lock().unwrap().push(ImageVersionEntry {
            image_id: identity.image_id.clone(),
            version: identity.version.clone(),
            image_ref: identity.image_ref.clone(),
            runtime_image_id: String::new(),
            state: "unknown".into(),
        });
        self.revoke();
        Ok(())
    }

    /// Reconcile frozen identities and the full daemon ref set. No unknown ref
    /// under a known managed repository may be silently classified as absent.
    pub fn reconcile(
        &self,
        identities: &[ImageIdentity],
        daemon: &[DaemonImage],
        container_image_ids: &[String],
    ) -> std::io::Result<(bool, Vec<ImageVersionEntry>)> {
        for identity in identities {
            self.remember(identity)?;
        }
        let mut guard = self.disk.lock().unwrap();
        let mut next = guard.clone();
        let mut by_ref = BTreeMap::new();
        for image in daemon {
            for reference in &image.refs {
                if by_ref.insert(reference.clone(), image.id.clone()).is_some() {
                    *self.complete.lock().unwrap() = false;
                    return Err(std::io::Error::other("duplicate daemon image ref"));
                }
            }
        }
        let known_refs: BTreeSet<_> = next
            .records
            .values()
            .flat_map(|versions| versions.values())
            .map(|r| r.image_ref.as_str())
            .collect();
        let managed_names: BTreeSet<_> = known_refs.iter().filter_map(|r| repository(r)).collect();
        let unclassified = by_ref.keys().any(|r| {
            repository(r).is_some_and(|name| managed_names.contains(name))
                && !known_refs.contains(r.as_str())
        });
        let binding_changed = next
            .records
            .values()
            .flat_map(|versions| versions.values())
            .any(|record| {
                by_ref.get(&record.image_ref).is_some_and(|id| {
                    !record.runtime_image_id.is_empty() && *id != record.runtime_image_id
                })
            });
        let orphaned_container = next
            .records
            .values()
            .flat_map(|versions| versions.values())
            .any(|record| {
                !record.runtime_image_id.is_empty()
                    && !by_ref.contains_key(&record.image_ref)
                    && container_image_ids.contains(&record.runtime_image_id)
            });
        let complete = !unclassified
            && !binding_changed
            && !orphaned_container
            && *self.authority_received.lock().unwrap();
        for versions in next.records.values_mut() {
            for record in versions.values_mut() {
                if let Some(id) = by_ref.get(&record.image_ref) {
                    if record.runtime_image_id.is_empty() {
                        record.runtime_image_id = id.clone();
                    }
                }
            }
        }
        self.save(&next)?;
        *guard = next;
        *self.complete.lock().unwrap() = complete;
        let mut found = entries(&guard, &by_ref, container_image_ids);
        if !complete {
            for entry in &mut found {
                entry.state = "unknown".to_string();
            }
        }
        *self.last.lock().unwrap() = found.clone();
        Ok((complete, found))
    }

    pub fn mark_authority_received(&self) {
        *self.authority_received.lock().unwrap() = true;
    }

    pub fn revoke(&self) {
        *self.complete.lock().unwrap() = false;
        for entry in self.last.lock().unwrap().iter_mut() {
            entry.state = "unknown".to_string();
        }
    }

    pub fn begin_connection(&self) {
        *self.authority_received.lock().unwrap() = false;
        self.revoke();
    }

    pub fn snapshot(&self) -> (bool, Vec<ImageVersionEntry>) {
        (self.is_complete(), self.last.lock().unwrap().clone())
    }

    pub fn is_complete(&self) -> bool {
        *self.complete.lock().unwrap()
    }

    pub fn matches(&self, identity: &ImageIdentity, runtime_id: &str) -> bool {
        self.is_complete()
            && self
                .disk
                .lock()
                .unwrap()
                .records
                .get(&identity.image_id)
                .and_then(|versions| versions.get(&identity.version))
                .is_some_and(|r| {
                    r.image_ref == identity.image_ref
                        && r.runtime_image_id == runtime_id
                        && !runtime_id.is_empty()
                })
    }
}

fn repository(reference: &str) -> Option<&str> {
    reference
        .split_once('@')
        .map(|x| x.0)
        .or_else(|| reference.rsplit_once(':').map(|x| x.0))
}

fn entries(
    disk: &Disk,
    by_ref: &BTreeMap<String, String>,
    container_image_ids: &[String],
) -> Vec<ImageVersionEntry> {
    disk.records
        .iter()
        .flat_map(|(image_id, versions)| {
            versions.iter().map(move |(version, rec)| {
                let actual = by_ref.get(&rec.image_ref);
                let state = match actual {
                    Some(id) if *id == rec.runtime_image_id && !id.is_empty() => "present",
                    Some(_) => "unknown",
                    None if !rec.runtime_image_id.is_empty()
                        && !container_image_ids.contains(&rec.runtime_image_id) =>
                    {
                        "absent"
                    }
                    None => "unknown",
                };
                ImageVersionEntry {
                    image_id: image_id.clone(),
                    version: version.clone(),
                    image_ref: rec.image_ref.clone(),
                    runtime_image_id: rec.runtime_image_id.clone(),
                    state: state.to_string(),
                }
            })
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn identity(version: &str, tag: &str) -> ImageIdentity {
        ImageIdentity {
            image_id: "steam".into(),
            version: version.into(),
            image_ref: format!("ghcr.io/x/steam:sha-{tag}"),
        }
    }

    fn daemon(id: &str, reference: &str) -> DaemonImage {
        DaemonImage {
            id: id.into(),
            refs: vec![reference.into()],
        }
    }

    #[test]
    fn previous_version_survives_restart_and_absence_needs_all_container_proof() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("versions.json");
        let inventory = VersionInventory::open(path.clone()).unwrap();
        inventory.mark_authority_received();
        let old = identity("v1", "1111111");
        let new = identity("v2", "2222222");
        let both = vec![
            daemon("sha256:old", &old.image_ref),
            daemon("sha256:new", &new.image_ref),
        ];
        let (complete, entries) = inventory
            .reconcile(&[old.clone(), new.clone()], &both, &[])
            .unwrap();
        assert!(complete);
        assert_eq!(entries.len(), 2);
        let reopened = VersionInventory::open(path).unwrap();
        reopened.mark_authority_received();
        let (complete, entries) = reopened
            .reconcile(&[new], &[both[1].clone()], &["sha256:old".into()])
            .unwrap();
        assert!(!complete);
        assert_eq!(
            entries.iter().find(|e| e.version == "v1").unwrap().state,
            "unknown"
        );
        let (complete, entries) = reopened.reconcile(&[old], &[both[1].clone()], &[]).unwrap();
        assert!(complete);
        assert_eq!(
            entries.iter().find(|e| e.version == "v1").unwrap().state,
            "absent"
        );
    }

    #[test]
    fn changed_daemon_id_and_unclassified_managed_ref_revoke_completeness() {
        let dir = tempfile::tempdir().unwrap();
        let inventory = VersionInventory::open(dir.path().join("versions.json")).unwrap();
        inventory.mark_authority_received();
        let old = identity("v1", "1111111");
        inventory
            .reconcile(
                std::slice::from_ref(&old),
                &[daemon("sha256:old", &old.image_ref)],
                &[],
            )
            .unwrap();
        let (complete, entries) = inventory
            .reconcile(
                std::slice::from_ref(&old),
                &[daemon("sha256:other", &old.image_ref)],
                &[],
            )
            .unwrap();
        assert!(!complete);
        assert_eq!(entries[0].state, "unknown");
        let unknown = daemon("sha256:other", "ghcr.io/x/steam:sha-3333333");
        let (complete, entries) = inventory.reconcile(&[old], &[unknown], &[]).unwrap();
        assert!(!complete);
        assert_eq!(entries[0].state, "unknown");
    }
}
