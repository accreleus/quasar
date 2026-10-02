//! Durable, process-wide cleanup attempt fence. All socket epochs share this lock.
use std::collections::BTreeMap;
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::PathBuf;
use std::sync::Mutex;

use serde::{Deserialize, Serialize};

use crate::messages::{AgentMsg, ImageCleanupJournalEntry};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Attempt {
    pub attempt_id: String,
    pub image_id: String,
    pub version: String,
    pub image_ref: String,
    pub runtime_image_id: String,
    pub generation: String,
    pub state: String,
    pub reason: Option<String>,
}

impl Attempt {
    pub fn same_identity(&self, other: &Self) -> bool {
        self.attempt_id == other.attempt_id
            && self.image_id == other.image_id
            && self.version == other.version
            && self.image_ref == other.image_ref
            && self.runtime_image_id == other.runtime_image_id
            && self.generation == other.generation
    }

    pub fn report(&self) -> AgentMsg {
        AgentMsg::ImageCleanupState {
            attempt_id: self.attempt_id.clone(),
            image_id: self.image_id.clone(),
            version: self.version.clone(),
            image_ref: self.image_ref.clone(),
            runtime_image_id: self.runtime_image_id.clone(),
            generation: self.generation.clone(),
            state: self.state.clone(),
            reason: self.reason.clone(),
        }
    }

    fn journal_entry(&self) -> ImageCleanupJournalEntry {
        ImageCleanupJournalEntry {
            attempt_id: self.attempt_id.clone(),
            image_id: self.image_id.clone(),
            version: self.version.clone(),
            image_ref: self.image_ref.clone(),
            runtime_image_id: self.runtime_image_id.clone(),
            generation: self.generation.clone(),
            state: self.state.clone(),
        }
    }
}

#[derive(Default, Serialize, Deserialize, Clone)]
struct Disk {
    attempts: BTreeMap<String, Attempt>,
    retired: BTreeMap<String, String>,
}

pub enum Begin {
    Fresh,
    Duplicate(Attempt),
    Retired,
    Mismatch,
}

pub struct CleanupJournal {
    path: PathBuf,
    inner: Mutex<Disk>,
}

impl CleanupJournal {
    pub fn open(path: PathBuf) -> std::io::Result<Self> {
        let inner = match fs::read(&path) {
            Ok(bytes) => serde_json::from_slice(&bytes)
                .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))?,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Disk::default(),
            Err(e) => return Err(e),
        };
        Ok(Self {
            path,
            inner: Mutex::new(inner),
        })
    }

    fn save(&self, next: &Disk) -> std::io::Result<()> {
        if let Some(parent) = self.path.parent() {
            fs::create_dir_all(parent)?;
        }
        let tmp = self.path.with_extension("cleanup.tmp");
        let _ = fs::remove_file(&tmp);
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&tmp)?;
        file.write_all(&serde_json::to_vec(next)?)?;
        file.sync_all()?;
        fs::rename(&tmp, &self.path)?;
        if let Some(parent) = self.path.parent() {
            fs::File::open(parent)?.sync_all()?;
        }
        Ok(())
    }

    fn update<T>(&self, f: impl FnOnce(&mut Disk) -> T) -> std::io::Result<T> {
        let mut guard = self.inner.lock().unwrap();
        let mut next = guard.clone();
        let result = f(&mut next);
        self.save(&next)?;
        *guard = next;
        Ok(result)
    }

    pub fn begin(&self, attempt: Attempt) -> std::io::Result<Begin> {
        let guard = self.inner.lock().unwrap();
        if guard.retired.contains_key(&attempt.attempt_id) {
            return Ok(Begin::Retired);
        }
        if let Some(existing) = guard.attempts.get(&attempt.attempt_id) {
            return Ok(if existing.same_identity(&attempt) {
                Begin::Duplicate(existing.clone())
            } else {
                Begin::Mismatch
            });
        }
        drop(guard);
        // The second check remains under the same mutex through durable append.
        self.update(|disk| {
            if disk.retired.contains_key(&attempt.attempt_id) {
                return Begin::Retired;
            }
            if let Some(existing) = disk.attempts.get(&attempt.attempt_id) {
                return if existing.same_identity(&attempt) {
                    Begin::Duplicate(existing.clone())
                } else {
                    Begin::Mismatch
                };
            }
            disk.attempts.insert(attempt.attempt_id.clone(), attempt);
            Begin::Fresh
        })
    }

    pub fn finish(
        &self,
        id: &str,
        state: &str,
        reason: Option<&str>,
    ) -> std::io::Result<Option<Attempt>> {
        self.update(|disk| {
            let a = disk.attempts.get_mut(id)?;
            if a.state == "removing" {
                a.state = state.to_string();
                a.reason = reason.map(str::to_string);
            }
            Some(a.clone())
        })
    }

    pub fn retire_terminal(&self, id: &str, generation: &str) -> std::io::Result<bool> {
        self.update(|disk| {
            let Some(a) = disk.attempts.get(id) else {
                return disk
                    .retired
                    .get(id)
                    .is_some_and(|stored| stored == generation);
            };
            if a.generation != generation || a.state == "removing" {
                return false;
            }
            disk.attempts.remove(id);
            disk.retired.insert(id.to_string(), generation.to_string());
            true
        })
    }

    pub fn snapshot(&self, request_id: String, ids: &[String]) -> std::io::Result<AgentMsg> {
        self.update(|disk| {
            let mut retired = Vec::new();
            for id in ids {
                if !disk.attempts.contains_key(id) {
                    disk.retired.entry(id.clone()).or_default();
                    retired.push(id.clone());
                }
            }
            AgentMsg::ImageCleanupJournal {
                request_id,
                retired_attempt_ids: retired,
                attempts: disk.attempts.values().map(Attempt::journal_entry).collect(),
            }
        })
    }

    pub fn unfinished(&self) -> Vec<Attempt> {
        self.inner
            .lock()
            .unwrap()
            .attempts
            .values()
            .filter(|a| a.state == "removing")
            .cloned()
            .collect()
    }

    pub fn terminal(&self) -> Vec<Attempt> {
        self.inner
            .lock()
            .unwrap()
            .attempts
            .values()
            .filter(|a| a.state != "removing")
            .cloned()
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn attempt(id: &str) -> Attempt {
        Attempt {
            attempt_id: id.into(),
            image_id: "steam".into(),
            version: "v1".into(),
            image_ref: "ghcr.io/x/steam:sha-1234567".into(),
            runtime_image_id: "sha256:one".into(),
            generation: "7".into(),
            state: "removing".into(),
            reason: None,
        }
    }

    #[test]
    fn lost_ack_does_not_restart_deletion_and_retirement_survives_restart() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("cleanup.json");
        let journal = CleanupJournal::open(path.clone()).unwrap();
        assert!(matches!(journal.begin(attempt("a")).unwrap(), Begin::Fresh));
        let reopened = CleanupJournal::open(path.clone()).unwrap();
        assert!(matches!(
            reopened.begin(attempt("a")).unwrap(),
            Begin::Duplicate(_)
        ));
        reopened.finish("a", "removed", None).unwrap();
        assert!(reopened.retire_terminal("a", "7").unwrap());
        let reopened = CleanupJournal::open(path).unwrap();
        assert!(matches!(
            reopened.begin(attempt("a")).unwrap(),
            Begin::Retired
        ));
    }

    #[test]
    fn journal_request_retires_absent_delayed_attempt_before_reply() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("cleanup.json");
        let journal = CleanupJournal::open(path.clone()).unwrap();
        let response = journal
            .snapshot("request".into(), &["delayed".into()])
            .unwrap();
        let serde_json::Value::Object(fields) = serde_json::to_value(response).unwrap() else {
            panic!("object")
        };
        assert_eq!(
            fields["retired_attempt_ids"],
            serde_json::json!(["delayed"])
        );
        assert!(matches!(
            journal.begin(attempt("delayed")).unwrap(),
            Begin::Retired
        ));
        let reopened = CleanupJournal::open(path).unwrap();
        assert!(matches!(
            reopened.begin(attempt("delayed")).unwrap(),
            Begin::Retired
        ));
    }

    #[test]
    fn journal_snapshot_contains_old_epoch_attempt_without_retiring_it() {
        let dir = tempfile::tempdir().unwrap();
        let journal = CleanupJournal::open(dir.path().join("cleanup.json")).unwrap();
        journal.begin(attempt("old")).unwrap();
        let response = journal
            .snapshot("new-epoch".into(), &["old".into(), "absent".into()])
            .unwrap();
        let json = serde_json::to_value(response).unwrap();
        assert_eq!(json["retired_attempt_ids"], serde_json::json!(["absent"]));
        assert_eq!(json["attempts"][0]["attempt_id"], "old");
    }

    #[test]
    fn delayed_handler_and_retirement_have_one_durable_winner() {
        for _ in 0..32 {
            let dir = tempfile::tempdir().unwrap();
            let journal =
                std::sync::Arc::new(CleanupJournal::open(dir.path().join("cleanup.json")).unwrap());
            let barrier = std::sync::Arc::new(std::sync::Barrier::new(3));
            let begin = {
                let journal = journal.clone();
                let barrier = barrier.clone();
                std::thread::spawn(move || {
                    barrier.wait();
                    journal.begin(attempt("race")).unwrap()
                })
            };
            let snapshot = {
                let journal = journal.clone();
                let barrier = barrier.clone();
                std::thread::spawn(move || {
                    barrier.wait();
                    journal
                        .snapshot("request".into(), &["race".into()])
                        .unwrap()
                })
            };
            barrier.wait();
            let began = begin.join().unwrap();
            let snapshot = serde_json::to_value(snapshot.join().unwrap()).unwrap();
            if matches!(began, Begin::Fresh) {
                assert_eq!(snapshot["attempts"][0]["attempt_id"], "race");
            } else {
                assert!(matches!(began, Begin::Retired));
                assert_eq!(snapshot["retired_attempt_ids"], serde_json::json!(["race"]));
            }
        }
    }
}
