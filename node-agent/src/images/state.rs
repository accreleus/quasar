//! Persisted state for managed images: `image_id -> {registry_ref, version, state}`.
//! Best-effort JSON next to the node secret; the agent must remember refs it ensured
//! across restart (agent-api.md image-management P2 amendment).
//!
//! Saves are atomic and must stay so: a sibling `.tmp` created `create_new` + `0600`
//! (never lands on a planted path or follows a symlink), `sync_all`'d, then renamed
//! over the destination. Saved on every state transition including progress ticks, so
//! a crash mid-save must leave the previous good file, not a truncated one.

use std::collections::BTreeMap;
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

use serde::{Deserialize, Serialize};

/// Wire `image_state.state` / `register.images[].state` values (agent-api.md).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ImageState {
    Absent,
    Pulling,
    Building,
    Ready,
    Failed,
}

impl ImageState {
    pub fn as_wire_str(self) -> &'static str {
        match self {
            ImageState::Absent => "absent",
            ImageState::Pulling => "pulling",
            ImageState::Building => "building",
            ImageState::Ready => "ready",
            ImageState::Failed => "failed",
        }
    }
}

/// The `(registry_ref, version)` an in-flight `image_ensure` is pulling. Staged, not
/// written onto the record: a failed version bump must leave the record naming the ref
/// the daemon has, or a later `image_remove` rmi's a ref never pulled and orphans the
/// previous multi-GB image.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct StagedTarget {
    pub registry_ref: String,
    pub version: String,
}

/// One managed image's last-known state, keyed by `image_id`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ImageRecord {
    /// The ref the daemon actually holds (or last held); committed only on a
    /// successful pull, with [`Self::version`].
    pub registry_ref: String,
    pub version: String,
    pub state: ImageState,
    #[serde(default)]
    pub progress_pct: u8,
    #[serde(default)]
    pub bytes: u64,
    #[serde(default)]
    pub error: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub staged: Option<StagedTarget>,
}

impl ImageRecord {
    /// An image the agent knows of but has never pulled.
    pub fn empty() -> Self {
        ImageRecord {
            registry_ref: String::new(),
            version: String::new(),
            state: ImageState::Absent,
            progress_pct: 0,
            bytes: 0,
            error: String::new(),
            staged: None,
        }
    }

    /// Wire version: the in-flight target while pulling, the committed one otherwise.
    pub fn wire_version(&self) -> &str {
        match &self.staged {
            Some(s) => &s.version,
            None => &self.version,
        }
    }

    /// Commit a successful pull: the staged target becomes the record's ref.
    pub fn mark_ready(&mut self, bytes: u64) {
        if let Some(staged) = self.staged.take() {
            self.registry_ref = staged.registry_ref;
            self.version = staged.version;
        }
        self.state = ImageState::Ready;
        self.progress_pct = 100;
        self.bytes = bytes;
        self.error.clear();
    }

    /// A failed pull/remove: the staged target is dropped, so the record keeps
    /// naming the ref the daemon actually has.
    pub fn mark_failed(&mut self, error: &str) {
        if let Some(staged) = self.staged.take() {
            // Nothing committed yet, so no earlier ref to protect: adopt the attempted
            // one and report the failure against a real (version, ref), not blanks.
            if self.registry_ref.is_empty() {
                self.registry_ref = staged.registry_ref;
                self.version = staged.version;
            }
        }
        self.state = ImageState::Failed;
        self.error = error.to_string();
    }

    pub fn mark_absent(&mut self) {
        self.staged = None;
        self.state = ImageState::Absent;
        self.progress_pct = 0;
        self.bytes = 0;
        self.error.clear();
    }
}

/// On-disk shape.
#[derive(Debug, Default, Serialize, Deserialize)]
struct ImageStateFile {
    images: BTreeMap<String, ImageRecord>,
}

/// Best-effort load: a missing/corrupt file yields an empty map rather than failing
/// agent startup.
pub fn load(path: &str) -> BTreeMap<String, ImageRecord> {
    match std::fs::read_to_string(path) {
        Ok(raw) => match serde_json::from_str::<ImageStateFile>(&raw) {
            Ok(f) => f.images,
            Err(e) => {
                tracing::warn!(
                    token = "image-state-corrupt",
                    "image state file {path} is corrupt ({e}); starting empty"
                );
                BTreeMap::new()
            }
        },
        Err(_) => BTreeMap::new(),
    }
}

/// Best-effort save: a failure is logged, never fatal.
pub fn save(path: &str, images: &BTreeMap<String, ImageRecord>) {
    // An empty path means "no persistence" (the unit-test manager); without
    // this the atomic write below would create a bare `.tmp` in the cwd.
    if path.is_empty() {
        return;
    }
    if let Some(parent) = Path::new(path).parent() {
        if let Err(e) = std::fs::create_dir_all(parent) {
            tracing::warn!(
                token = "image-state-mkdir-failed",
                "image state: could not create {}: {e}",
                parent.display()
            );
            return;
        }
    }
    let file = ImageStateFile {
        images: images.clone(),
    };
    let bytes = match serde_json::to_vec_pretty(&file) {
        Ok(bytes) => bytes,
        Err(e) => {
            tracing::warn!(
                token = "image-state-serialize-failed",
                "image state: could not serialize: {e}"
            );
            return;
        }
    };
    if let Err(e) = write_atomically(path, &bytes) {
        tracing::warn!(
            token = "image-state-write-failed",
            "image state: could not write {path}: {e}"
        );
    }
}

/// Atomic write via a sibling `.tmp` (see the module doc).
fn write_atomically(path: &str, bytes: &[u8]) -> std::io::Result<()> {
    let tmp = format!("{path}.tmp");
    // A stale .tmp from a crashed save would fail `create_new`. Only this agent
    // ever writes this path.
    match std::fs::remove_file(&tmp) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(e),
    }
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&tmp)?;
    f.write_all(bytes)?;
    f.sync_all()?;
    drop(f);
    std::fs::rename(&tmp, path)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn missing_file_loads_empty() {
        let map = load("/nonexistent/path/for/quasar-image-state-tests.json");
        assert!(map.is_empty());
    }

    #[test]
    fn save_then_load_round_trips() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("images.json");
        let path = path.to_str().unwrap();

        let mut images = BTreeMap::new();
        images.insert(
            "steam".to_string(),
            ImageRecord {
                registry_ref: "ghcr.io/x/steam:sha-abc".to_string(),
                version: "2026.08.07".to_string(),
                state: ImageState::Ready,
                progress_pct: 100,
                bytes: 12345,
                error: String::new(),
                staged: None,
            },
        );
        save(path, &images);
        let loaded = load(path);
        assert_eq!(loaded.len(), 1);
        let rec = &loaded["steam"];
        assert_eq!(rec.registry_ref, "ghcr.io/x/steam:sha-abc");
        assert_eq!(rec.state, ImageState::Ready);
        assert_eq!(rec.bytes, 12345);
        assert!(rec.staged.is_none());
    }

    #[test]
    fn a_failed_version_bump_keeps_the_committed_ref() {
        let mut rec = ImageRecord {
            registry_ref: "ghcr.io/x/steam:sha-old1234".to_string(),
            version: "v1".to_string(),
            state: ImageState::Ready,
            progress_pct: 100,
            bytes: 1,
            error: String::new(),
            staged: Some(StagedTarget {
                registry_ref: "ghcr.io/x/steam:sha-new5678".to_string(),
                version: "v2".to_string(),
            }),
        };
        assert_eq!(rec.wire_version(), "v2", "reports the in-flight target");
        rec.mark_failed("network error");
        assert_eq!(rec.registry_ref, "ghcr.io/x/steam:sha-old1234");
        assert_eq!(rec.version, "v1");
        assert_eq!(rec.state, ImageState::Failed);
    }

    #[test]
    fn a_first_ever_failure_adopts_the_attempted_ref() {
        let mut rec = ImageRecord::empty();
        rec.staged = Some(StagedTarget {
            registry_ref: "ghcr.io/x/steam:sha-new5678".to_string(),
            version: "v1".to_string(),
        });
        rec.mark_failed("registry auth denied");
        assert_eq!(rec.registry_ref, "ghcr.io/x/steam:sha-new5678");
        assert_eq!(rec.version, "v1");
    }

    #[test]
    fn a_successful_pull_commits_the_staged_target() {
        let mut rec = ImageRecord::empty();
        rec.staged = Some(StagedTarget {
            registry_ref: "ghcr.io/x/steam:sha-new5678".to_string(),
            version: "v2".to_string(),
        });
        rec.mark_ready(42);
        assert_eq!(rec.registry_ref, "ghcr.io/x/steam:sha-new5678");
        assert_eq!(rec.version, "v2");
        assert_eq!(rec.state, ImageState::Ready);
        assert_eq!(rec.bytes, 42);
        assert!(rec.staged.is_none());
    }

    #[test]
    fn save_writes_atomically_with_0600_and_leaves_no_tmp() {
        use std::os::unix::fs::PermissionsExt;

        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("images.json");
        let path = path.to_str().unwrap();

        let mut images = BTreeMap::new();
        images.insert("steam".to_string(), ImageRecord::empty());
        save(path, &images);

        let mode = std::fs::metadata(path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "state file must not be group/world readable");
        assert!(
            !Path::new(&format!("{path}.tmp")).exists(),
            "the sibling .tmp must be renamed away, not left behind"
        );

        // A second save replaces, not appends.
        images.insert("other".to_string(), ImageRecord::empty());
        save(path, &images);
        assert_eq!(load(path).len(), 2);
        let raw = std::fs::read_to_string(path).unwrap();
        assert_eq!(
            serde_json::from_str::<serde_json::Value>(&raw).unwrap()["images"]
                .as_object()
                .unwrap()
                .len(),
            2
        );
        let mode = std::fs::metadata(path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "the replacement keeps 0600");
    }

    #[test]
    fn save_recovers_from_a_stale_tmp_left_by_a_crash() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("images.json");
        let path = path.to_str().unwrap();
        std::fs::write(format!("{path}.tmp"), b"garbage from a crashed save").unwrap();

        let mut images = BTreeMap::new();
        images.insert("steam".to_string(), ImageRecord::empty());
        save(path, &images);
        assert_eq!(load(path).len(), 1);
    }

    #[test]
    fn saving_to_an_empty_path_is_a_no_op() {
        save("", &BTreeMap::new());
        assert!(!Path::new(".tmp").exists());
    }

    #[test]
    fn corrupt_file_loads_empty_not_panicking() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("images.json");
        std::fs::write(&path, b"not json").unwrap();
        let map = load(path.to_str().unwrap());
        assert!(map.is_empty());
    }
}
