//! Lifecycle of the media probe's private runtime dir (#291): the same
//! owner-marked sibling-marker pattern as `session::udev_export` (#286), reused
//! here through `crate::owned_entry` rather than re-implemented.
//!
//! Unlike the udev export (keyed by a caller-supplied session id), this dir's id
//! is a random suffix minted in [`acquire`] — the marker still precedes the
//! directory it obliges. `run()` (`host_probe::media`) removes both explicitly
//! when the probe child exits; a killed agent (a recreate, or the NVIDIA agent's
//! own self-restart mid-probe) leaves them for the boot-only [`retire_all_owned`]
//! pass to reclaim.

use std::path::{Path, PathBuf};

use anyhow::{anyhow, Context, Result};
use serde::{Deserialize, Serialize};

use crate::owned_entry::{self, OwnedMarker, Summary};

const PREFIX: &str = "quasar-media-probe-";
/// Bounded so a stuck id generator (or a hostile clock) cannot spin forever.
const MAX_ID_ATTEMPTS: u32 = 8;

#[derive(Serialize, Deserialize)]
struct Marker {
    owner: String,
    id: String,
}

impl OwnedMarker for Marker {
    fn owner(&self) -> &str {
        &self.owner
    }
}

fn malformed_id(id: &str) -> bool {
    id.is_empty() || id.contains('/') || id.contains("..")
}

fn random_id() -> String {
    use std::sync::atomic::{AtomicU64, Ordering};
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    let counter = COUNTER.fetch_add(1, Ordering::Relaxed);
    format!("{:x}-{nanos:x}-{counter:x}", std::process::id())
}

/// A live probe runtime dir: marker written first, directory second. The caller
/// removes it explicitly (dir then marker) on every return path; [`Drop`] is only
/// a backstop for a path that misses that call.
pub struct ProbeRuntimeDir {
    runtime_dir: String,
    id: String,
    dir: PathBuf,
    retired: bool,
}

impl ProbeRuntimeDir {
    pub fn path(&self) -> &Path {
        &self.dir
    }

    /// Idempotent explicit retire: directory then marker, mirroring
    /// `udev_export::retire`'s ordering (an interrupted retire keeps its
    /// ownership proof for the next attempt).
    pub fn retire(&mut self) -> Result<()> {
        if self.retired {
            return Ok(());
        }
        owned_entry::retire_entry(&self.runtime_dir, PREFIX, &self.id)?;
        self.retired = true;
        Ok(())
    }
}

impl Drop for ProbeRuntimeDir {
    fn drop(&mut self) {
        if !self.retired {
            let _ = owned_entry::retire_entry(&self.runtime_dir, PREFIX, &self.id);
        }
    }
}

/// Mint a fresh id, write its marker, then create the directory — in that order,
/// so the durable obligation always precedes the thing it obliges. Retries on an
/// (astronomically unlikely) id collision.
pub fn acquire(runtime_dir: &str, owner: &str) -> Result<ProbeRuntimeDir> {
    for _ in 0..MAX_ID_ATTEMPTS {
        let id = random_id();
        let marker = owned_entry::marker_path(runtime_dir, PREFIX, &id);
        if let Some(parent) = marker.parent() {
            std::fs::create_dir_all(parent)
                .with_context(|| format!("create {}", parent.display()))?;
        }
        let written = owned_entry::try_write_marker(
            &marker,
            &Marker {
                owner: owner.to_string(),
                id: id.clone(),
            },
        )?;
        if !written {
            continue; // id collision; try another.
        }
        let dir = owned_entry::entry_dir(runtime_dir, PREFIX, &id);
        if let Err(e) = std::fs::create_dir(&dir) {
            // Marker written but the directory failed: leave the marker in place
            // for boot reconcile rather than guessing at cleanup here.
            return Err(e).with_context(|| format!("create {}", dir.display()));
        }
        return Ok(ProbeRuntimeDir {
            runtime_dir: runtime_dir.to_string(),
            id,
            dir,
            retired: false,
        });
    }
    Err(anyhow!(
        "could not allocate a unique media-probe runtime dir id after {MAX_ID_ATTEMPTS} attempts"
    ))
}

/// Boot-only reconciliation: retire every `quasar-media-probe-*` marker+directory
/// this `owner` token wrote, leaving anything it cannot attribute to itself.
/// Mirrors `udev_export::retire_all_owned` — must run only where "ours" implies
/// "dead" (boot, after application retirement), never the periodic maintenance
/// tick.
pub fn retire_all_owned(runtime_dir: &str, owner: &str) -> Summary {
    owned_entry::retire_all_owned::<Marker>(runtime_dir, PREFIX, owner, malformed_id)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;

    #[test]
    fn acquire_writes_the_marker_before_the_directory() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        let dir = acquire(runtime_dir, "owner-a").unwrap();
        assert!(dir.path().is_dir());
        assert!(dir.path().starts_with(tmp.path()));
        let file_name = dir.path().file_name().unwrap().to_string_lossy();
        assert!(file_name.starts_with(PREFIX));
        let marker = owned_entry::marker_path(runtime_dir, PREFIX, &dir.id);
        let meta = std::fs::symlink_metadata(&marker).unwrap();
        assert!(meta.is_file());
        let marker_body: Marker = serde_json::from_slice(&std::fs::read(&marker).unwrap()).unwrap();
        assert_eq!(marker_body.owner, "owner-a");
        assert_eq!(marker_body.id, dir.id);
    }

    #[test]
    fn normal_retire_leaves_nothing_behind() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        let mut dir = acquire(runtime_dir, "owner-a").unwrap();
        let path = dir.path().to_path_buf();
        let marker = owned_entry::marker_path(runtime_dir, PREFIX, &dir.id);
        dir.retire().unwrap();
        assert!(!path.exists());
        assert!(!marker.exists());
        // Idempotent.
        dir.retire().unwrap();
        // Drop after an explicit retire must not error or resurrect anything.
        drop(dir);
        assert!(!path.exists());
    }

    #[test]
    fn a_simulated_crash_is_reclaimed_by_boot_reconcile_for_its_owner() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        let dir = acquire(runtime_dir, "owner-a").unwrap();
        let path = dir.path().to_path_buf();
        let marker = owned_entry::marker_path(runtime_dir, PREFIX, &dir.id);
        // The agent died mid-probe: neither Drop nor an explicit retire runs.
        std::mem::forget(dir);
        assert!(path.is_dir());
        assert!(marker.is_file());

        let summary = retire_all_owned(runtime_dir, "owner-a");
        assert_eq!(
            summary,
            Summary {
                removed: 1,
                unattributable: 0,
                errors: 0
            }
        );
        assert!(!path.exists());
        assert!(!marker.exists());
    }

    #[test]
    fn a_foreign_owner_marker_and_a_markerless_dir_are_left_and_counted() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        let foreign = acquire(runtime_dir, "owner-b").unwrap();
        let foreign_path = foreign.path().to_path_buf();
        std::mem::forget(foreign);

        let legacy = owned_entry::entry_dir(runtime_dir, PREFIX, "legacy");
        std::fs::create_dir(&legacy).unwrap();

        let summary = retire_all_owned(runtime_dir, "owner-a");
        assert_eq!(
            summary,
            Summary {
                removed: 0,
                unattributable: 2,
                errors: 0
            }
        );
        assert!(foreign_path.is_dir());
        assert!(legacy.is_dir());
    }

    #[test]
    fn a_symlink_in_place_of_the_directory_is_refused() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        let target = tmp.path().join("elsewhere");
        std::fs::create_dir(&target).unwrap();
        std::fs::write(target.join("untouched"), b"keep").unwrap();
        let dir_path = owned_entry::entry_dir(runtime_dir, PREFIX, "sid1");
        symlink(&target, &dir_path).unwrap();

        let result = owned_entry::retire_entry(runtime_dir, PREFIX, "sid1");
        assert!(result.is_err());
        assert!(dir_path
            .symlink_metadata()
            .unwrap()
            .file_type()
            .is_symlink());
        assert!(target.join("untouched").exists());
    }
}
