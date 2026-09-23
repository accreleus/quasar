//! Durable session-ID retirement for cleanup-qualified managed-home reports.
//!
//! A capable agent records an ID before it accepts an assignment. Retirement
//! is fsynced before a terminal report can leave the process. The files stay
//! in place so a delayed command on any later connection cannot reuse the ID.

use std::collections::BTreeMap;
use std::fs::{self, DirBuilder, File, OpenOptions};
use std::io::{Read, Write};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use sha2::{Digest, Sha256};

static TEMP_SEQUENCE: AtomicU64 = AtomicU64::new(1);

#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum TerminalKind {
    Stopped,
    Failed,
}

impl TerminalKind {
    pub(crate) fn as_str(self) -> &'static str {
        match self {
            Self::Stopped => "stopped",
            Self::Failed => "failed",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "phase", rename_all = "snake_case")]
enum Record {
    Active {
        session_id: String,
    },
    Retired {
        session_id: String,
        terminal: TerminalKind,
    },
}

impl Record {
    fn session_id(&self) -> &str {
        match self {
            Self::Active { session_id } | Self::Retired { session_id, .. } => session_id,
        }
    }
}

pub(crate) struct HomeCleanupLedger {
    root: PathBuf,
    records: BTreeMap<String, Record>,
    recovered: Vec<String>,
}

impl HomeCleanupLedger {
    /// Load the durable inventory. Active IDs are retained until their exact
    /// source cleanup has been proved by `recover_active`.
    pub(crate) fn open_after_startup_cleanup(root: PathBuf) -> std::io::Result<Self> {
        Self::open_with_parent_sync(root, |parent| File::open(parent)?.sync_all())
    }

    fn open_with_parent_sync(
        root: PathBuf,
        sync_parent: impl FnOnce(&Path) -> std::io::Result<()>,
    ) -> std::io::Result<Self> {
        let parent = root
            .parent()
            .ok_or_else(|| std::io::Error::other("ledger has no parent"))?;
        match DirBuilder::new().mode(0o700).create(&root) {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {}
            Err(error) => return Err(error),
        }
        if fs::symlink_metadata(&root)?.file_type().is_symlink() {
            return Err(std::io::Error::other(
                "home cleanup ledger directory is a symlink",
            ));
        }
        // Persist the directory entry before any session ID is accepted. A
        // crash after the first record fsync must not lose its parent folder.
        sync_parent(parent)?;
        File::open(&root)?.sync_all()?;
        let mut ledger = Self {
            root,
            records: BTreeMap::new(),
            recovered: Vec::new(),
        };
        for entry in fs::read_dir(&ledger.root)? {
            let entry = entry?;
            let path = entry.path();
            if path.extension().is_some() {
                continue; // unfinished atomic replacement from a prior crash
            }
            if !entry.file_type()?.is_file() {
                return Err(std::io::Error::other(
                    "unexpected home cleanup ledger entry",
                ));
            }
            let mut bytes = Vec::new();
            OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_NOFOLLOW)
                .open(&path)?
                .take(4097)
                .read_to_end(&mut bytes)?;
            if bytes.len() > 4096 {
                return Err(std::io::Error::other("home cleanup ledger entry too large"));
            }
            let record: Record = serde_json::from_slice(&bytes).map_err(std::io::Error::other)?;
            if path != ledger.path_for(record.session_id()) {
                return Err(std::io::Error::other(
                    "home cleanup ledger identity mismatch",
                ));
            }
            if ledger
                .records
                .insert(record.session_id().to_owned(), record)
                .is_some()
            {
                return Err(std::io::Error::other(
                    "duplicate home cleanup ledger identity",
                ));
            }
        }
        Ok(ledger)
    }

    pub(crate) fn recover_active(
        &mut self,
        mut prove_absent: impl FnMut(&str) -> std::io::Result<()>,
    ) -> std::io::Result<()> {
        let active: Vec<String> = self
            .records
            .iter()
            .filter_map(|(id, record)| {
                matches!(record, Record::Active { .. }).then_some(id.clone())
            })
            .collect();
        for id in active {
            prove_absent(&id)?;
            self.retire(&id, TerminalKind::Failed)?;
            self.recovered.push(id);
        }
        Ok(())
    }

    pub(crate) fn record_active(&mut self, session_id: &str) -> std::io::Result<bool> {
        if self.records.contains_key(session_id) {
            return Ok(false);
        }
        let record = Record::Active {
            session_id: session_id.to_owned(),
        };
        self.write(&record)?;
        self.records.insert(session_id.to_owned(), record);
        Ok(true)
    }

    pub(crate) fn retire(
        &mut self,
        session_id: &str,
        terminal: TerminalKind,
    ) -> std::io::Result<TerminalKind> {
        if let Some(Record::Retired { terminal, .. }) = self.records.get(session_id) {
            return Ok(*terminal);
        }
        let record = Record::Retired {
            session_id: session_id.to_owned(),
            terminal,
        };
        self.write(&record)?;
        self.records.insert(session_id.to_owned(), record);
        Ok(terminal)
    }

    pub(crate) fn state(&self, session_id: &str) -> Option<TerminalKind> {
        match self.records.get(session_id) {
            Some(Record::Retired { terminal, .. }) => Some(*terminal),
            _ => None,
        }
    }

    pub(crate) fn has_record(&self, session_id: &str) -> bool {
        self.records.contains_key(session_id)
    }

    pub(crate) fn take_recovered(&mut self) -> Vec<String> {
        std::mem::take(&mut self.recovered)
    }

    fn path_for(&self, session_id: &str) -> PathBuf {
        let hash = Sha256::digest(session_id.as_bytes());
        let name = hash.iter().map(|b| format!("{b:02x}")).collect::<String>();
        self.root.join(name)
    }

    fn write(&self, record: &Record) -> std::io::Result<()> {
        let path = self.path_for(record.session_id());
        let temp = path.with_extension(format!(
            "tmp-{}-{}",
            std::process::id(),
            TEMP_SEQUENCE.fetch_add(1, Ordering::Relaxed)
        ));
        let bytes = serde_json::to_vec(record).map_err(std::io::Error::other)?;
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW)
            .open(&temp)?;
        file.write_all(&bytes)?;
        file.sync_all()?;
        fs::rename(&temp, &path)?;
        File::open(&self.root)?.sync_all()?;
        Ok(())
    }
}

pub(crate) fn ledger_path(secret_path: &str) -> PathBuf {
    Path::new(&format!("{secret_path}.home-ledger")).to_path_buf()
}

pub(crate) fn ledger_truly_absent(secret_path: &str) -> std::io::Result<bool> {
    match fs::symlink_metadata(ledger_path(secret_path)) {
        Ok(_) => Ok(false),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(true),
        Err(error) => Err(error),
    }
}

/// A sibling of the node secret is durable only when its parent is an
/// explicitly mounted persistent filesystem. The default /tmp secret and a
/// path inside the container overlay do not qualify. This is intentionally
/// conservative: unsupported agents still register with legacy behavior.
pub(crate) fn verified_ledger_path(secret_path: &str) -> Option<PathBuf> {
    let parent = Path::new(secret_path).parent()?;
    let mountinfo = fs::read_to_string("/proc/self/mountinfo").ok()?;
    persistent_mount_at(&mountinfo, parent).then(|| ledger_path(secret_path))
}

fn persistent_mount_at(mountinfo: &str, parent: &Path) -> bool {
    let target = parent.to_str();
    mountinfo.lines().any(|line| {
        let Some((pre, post)) = line.split_once(" - ") else {
            return false;
        };
        let mountpoint = pre.split_whitespace().nth(4);
        let filesystem = post.split_whitespace().next();
        mountpoint == target
            && matches!(
                filesystem,
                Some("ext4" | "xfs" | "btrfs" | "zfs" | "nfs" | "nfs4" | "virtiofs")
            )
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn active_id_is_durably_retired_on_restart_and_cannot_be_reused() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("ledger");
        let mut first = HomeCleanupLedger::open_after_startup_cleanup(path.clone()).unwrap();
        assert!(first.record_active("session-one").unwrap());
        drop(first);
        let mut restarted = HomeCleanupLedger::open_after_startup_cleanup(path).unwrap();
        assert!(restarted
            .recover_active(|_| Err(std::io::Error::other("source remains")))
            .is_err());
        assert_eq!(restarted.state("session-one"), None);
        restarted.recover_active(|_| Ok(())).unwrap();
        assert_eq!(restarted.take_recovered(), vec!["session-one"]);
        assert_eq!(restarted.state("session-one"), Some(TerminalKind::Failed));
        assert!(!restarted.record_active("session-one").unwrap());
        assert_eq!(
            restarted
                .retire("session-one", TerminalKind::Stopped)
                .unwrap(),
            TerminalKind::Failed
        );
    }

    #[test]
    fn stop_of_never_recorded_id_retires_before_repeated_report() {
        let dir = tempfile::tempdir().unwrap();
        let mut ledger =
            HomeCleanupLedger::open_after_startup_cleanup(dir.path().join("ledger")).unwrap();
        assert_eq!(
            ledger.retire("lost-assign", TerminalKind::Stopped).unwrap(),
            TerminalKind::Stopped
        );
        assert_eq!(
            ledger.retire("lost-assign", TerminalKind::Failed).unwrap(),
            TerminalKind::Stopped
        );
        assert!(!ledger.record_active("lost-assign").unwrap());
    }

    #[test]
    fn corrupt_record_blocks_capability_instead_of_forgetting_it() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("ledger");
        fs::create_dir(&path).unwrap();
        fs::write(path.join("unknown"), b"not json").unwrap();
        assert!(HomeCleanupLedger::open_after_startup_cleanup(path).is_err());
    }

    #[test]
    fn corrupt_restart_cannot_forget_a_retired_id() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("ledger");
        let mut first = HomeCleanupLedger::open_after_startup_cleanup(path.clone()).unwrap();
        first
            .retire("retired-session", TerminalKind::Stopped)
            .unwrap();
        fs::write(path.join("corrupt"), b"not json").unwrap();
        assert!(HomeCleanupLedger::open_after_startup_cleanup(path.clone()).is_err());
        fs::remove_file(path.join("corrupt")).unwrap();
        let mut recovered = HomeCleanupLedger::open_after_startup_cleanup(path).unwrap();
        assert_eq!(
            recovered.state("retired-session"),
            Some(TerminalKind::Stopped)
        );
        assert!(!recovered.record_active("retired-session").unwrap());
    }

    #[test]
    fn failed_parent_sync_prevents_ledger_admission() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("ledger");
        assert!(HomeCleanupLedger::open_with_parent_sync(path.clone(), |_| {
            Err(std::io::Error::other("injected fsync failure"))
        })
        .is_err());
        let ledger = HomeCleanupLedger::open_after_startup_cleanup(path).unwrap();
        assert!(!ledger.has_record("never-accepted"));
    }

    #[test]
    fn legacy_mode_requires_a_truly_absent_ledger() {
        let dir = tempfile::tempdir().unwrap();
        let secret = dir.path().join("node-secret");
        let secret = secret.to_str().unwrap();
        assert!(ledger_truly_absent(secret).unwrap());
        let _ledger = HomeCleanupLedger::open_after_startup_cleanup(ledger_path(secret)).unwrap();
        assert!(!ledger_truly_absent(secret).unwrap());
    }

    #[test]
    fn capability_requires_a_dedicated_persistent_mount() {
        let mounts = "1 0 0:1 / / rw - overlay overlay rw\n\
            2 1 0:2 / /tmp rw - tmpfs tmpfs rw\n\
            3 1 8:1 /vol /var/lib/quasar-agent rw - xfs /dev/test rw\n";
        assert!(persistent_mount_at(
            mounts,
            Path::new("/var/lib/quasar-agent")
        ));
        assert!(!persistent_mount_at(mounts, Path::new("/tmp")));
        assert!(!persistent_mount_at(mounts, Path::new("/var/lib")));
    }
}
