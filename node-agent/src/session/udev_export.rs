//! Lifecycle of the per-session fake-udev export directory (#286).
//!
//! `{runtime_dir}/udev-{session_id}` is bind-mounted readonly into the untrusted
//! app container at `/run/udev/data`, so it must never carry the agent's
//! ownership proof. That proof lives in a SIBLING marker file,
//! `{runtime_dir}/udev-{session_id}.owner` — the audio sidecar directory's
//! marker-inside-directory pattern (`runtime/docker/helpers.rs`) does not apply
//! here for exactly that reason.
//!
//! The marker is written, synced, and fsynced to its parent BEFORE the export
//! directory exists: the durable obligation precedes the thing it obliges. On a
//! clean stop the caller retires both explicitly (see `session::host` /
//! `session::source`); on a killed agent neither `Drop` nor an explicit call
//! ever runs, so [`retire_all_owned`] reconciles at boot — only removing a
//! marker+directory pair this agent's persistent owner token wrote.

use std::fs::OpenOptions;
use std::io::{Read, Write};
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};

use anyhow::{anyhow, Context, Result};

const MAX_MARKER_BYTES: usize = 4096;

#[derive(serde::Serialize, serde::Deserialize)]
struct Marker {
    owner: String,
    session: String,
}

/// Reconciliation counts from [`retire_all_owned`].
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub struct Summary {
    /// Owned marker+directory pairs retired.
    pub removed: usize,
    /// Entries left alone: a foreign/malformed marker, or a directory with no
    /// marker at all (a pre-fix leftover).
    pub unattributable: usize,
    /// Retire attempts that failed (marker/dir left in place, logged by caller).
    pub errors: usize,
}

/// `{runtime_dir}/udev-{session_id}`, bind-mounted into the app container.
pub fn export_dir(runtime_dir: &str, session_id: &str) -> PathBuf {
    PathBuf::from(runtime_dir).join(format!("udev-{session_id}"))
}

/// `{runtime_dir}/udev-{session_id}.owner`, the sibling ownership marker.
pub fn marker_path(runtime_dir: &str, session_id: &str) -> PathBuf {
    PathBuf::from(runtime_dir).join(format!("udev-{session_id}.owner"))
}

fn fsync_parent(path: &Path) -> Result<()> {
    let parent = path
        .parent()
        .ok_or_else(|| anyhow!("{} has no parent", path.display()))?;
    std::fs::File::open(parent)
        .and_then(|f| f.sync_all())
        .with_context(|| format!("fsync parent of {}", path.display()))
}

/// Bounded, symlink-refusing marker read. `Ok(None)` = no marker. `Err` = present
/// but unreadable/malformed — the caller treats that as unattributable, not fatal.
fn read_marker(path: &Path) -> Result<Option<Marker>> {
    let file = match OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)
    {
        Ok(f) => f,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e).context("open udev export marker"),
    };
    let metadata = file.metadata().context("stat udev export marker")?;
    if !metadata.is_file() {
        return Err(anyhow!(
            "udev export marker at {} is not a regular file",
            path.display()
        ));
    }
    let mut bytes = Vec::new();
    file.take((MAX_MARKER_BYTES + 1) as u64)
        .read_to_end(&mut bytes)
        .context("read udev export marker")?;
    if bytes.len() > MAX_MARKER_BYTES {
        return Err(anyhow!(
            "udev export marker at {} exceeds bound",
            path.display()
        ));
    }
    let marker: Marker = serde_json::from_slice(&bytes).context("parse udev export marker")?;
    Ok(Some(marker))
}

fn write_marker(path: &Path, owner: &str, session_id: &str) -> Result<()> {
    let body = serde_json::to_vec(&Marker {
        owner: owner.to_string(),
        session: session_id.to_string(),
    })
    .context("serialize udev export marker")?;
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)
        .with_context(|| format!("create {}", path.display()))?;
    file.write_all(&body)
        .and_then(|_| file.sync_all())
        .with_context(|| format!("write {}", path.display()))?;
    fsync_parent(path)
}

/// Remove the export directory content-first: refuse a symlink or non-directory
/// outright (leave it untouched), otherwise remove its (plain-file) entries then
/// the directory itself. Missing is success.
fn remove_export_dir(dir: &Path) -> Result<()> {
    match std::fs::symlink_metadata(dir) {
        Ok(metadata) => {
            if metadata.file_type().is_symlink() || !metadata.is_dir() {
                return Err(anyhow!(
                    "refusing to remove non-directory at {}",
                    dir.display()
                ));
            }
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e).with_context(|| format!("stat {}", dir.display())),
    }
    for entry in std::fs::read_dir(dir).with_context(|| format!("read {}", dir.display()))? {
        let entry = entry.with_context(|| format!("read entry under {}", dir.display()))?;
        let file_type = entry
            .file_type()
            .with_context(|| format!("stat {}", entry.path().display()))?;
        if file_type.is_symlink() || file_type.is_dir() {
            return Err(anyhow!(
                "unexpected non-file entry {} in udev export dir",
                entry.path().display()
            ));
        }
        std::fs::remove_file(entry.path())
            .with_context(|| format!("remove {}", entry.path().display()))?;
    }
    std::fs::remove_dir(dir).with_context(|| format!("remove {}", dir.display()))
}

fn remove_marker(path: &Path) -> Result<()> {
    match std::fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e).with_context(|| format!("remove {}", path.display())),
    }
}

/// Idempotent: remove the export directory, THEN its marker — marker last, so an
/// interrupted retire keeps its ownership proof for the next attempt (boot sweep
/// or a retry of this same call). A missing directory still removes a lingering
/// marker.
pub fn retire(runtime_dir: &str, session_id: &str) -> Result<()> {
    if malformed_session_id(session_id) {
        return Err(anyhow!("refusing malformed session id {session_id:?}"));
    }
    remove_export_dir(&export_dir(runtime_dir, session_id))?;
    remove_marker(&marker_path(runtime_dir, session_id))
}

/// Publish this session's fake-udev records under `runtime_dir`, marking
/// ownership first. `records` are `(major, minor)` -> serialized udev-db body,
/// written world-readable, since app containers run as arbitrary non-root
/// UIDs.
///
/// Returns the export directory when published, or `None` when the export was
/// skipped (a foreign/unreadable marker already claims this session id — the
/// caller degrades to no in-container gamepad discovery, same as any other
/// best-effort export failure).
pub fn publish(
    runtime_dir: &str,
    session_id: &str,
    owner: &str,
    records: &[((u32, u32), String)],
) -> Result<Option<PathBuf>> {
    if malformed_session_id(session_id) {
        return Err(anyhow!("refusing malformed session id {session_id:?}"));
    }
    let marker = marker_path(runtime_dir, session_id);
    match read_marker(&marker) {
        Ok(Some(existing)) if existing.owner == owner => {
            // A stale marker from an earlier life of this exact agent (e.g. the
            // standalone harness's fixed sid "demo"). Reclaim it inline.
            retire(runtime_dir, session_id).context("reclaim stale udev export before publish")?;
        }
        Ok(Some(_)) => {
            tracing::warn!(
                token = "udev-export-unattributable",
                session = session_id,
                "udev export marker at {} is owned by another agent — skipping export",
                marker.display()
            );
            return Ok(None);
        }
        Ok(None) => {}
        Err(e) => {
            tracing::warn!(
                token = "udev-export-unattributable",
                session = session_id,
                "udev export marker at {} is unreadable ({e:#}) — skipping export",
                marker.display()
            );
            return Ok(None);
        }
    }
    if let Some(parent) = marker.parent() {
        std::fs::create_dir_all(parent).ok();
    }
    if let Err(e) = write_marker(&marker, owner, session_id) {
        tracing::warn!(
            token = "udev-export-failed",
            session = session_id,
            "write udev export marker {} failed: {e:#} — in-container gamepad discovery degraded",
            marker.display()
        );
        return Ok(None);
    }
    let dir = export_dir(runtime_dir, session_id);
    std::fs::create_dir(&dir).with_context(|| format!("create {}", dir.display()))?;
    std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o755))
        .with_context(|| format!("open perms on {}", dir.display()))?;
    for ((maj, min), body) in records {
        let path = dir.join(format!("c{maj}:{min}"));
        std::fs::write(&path, body).with_context(|| format!("write {}", path.display()))?;
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644))
            .with_context(|| format!("open perms on {}", path.display()))?;
    }
    Ok(Some(dir))
}

fn malformed_session_id(sid: &str) -> bool {
    sid.is_empty() || sid.contains('/') || sid.contains("..")
}

/// Boot-only reconciliation: retire every `udev-*` marker+directory this
/// `owner` token wrote, leaving anything it cannot attribute to itself. Never
/// follows a symlink. Must run only where "ours" implies "dead" (boot, after
/// application retirement) — never the periodic maintenance tick.
pub fn retire_all_owned(runtime_dir: &str, owner: &str) -> Summary {
    let mut summary = Summary::default();
    let entries = match std::fs::read_dir(runtime_dir) {
        Ok(entries) => entries,
        Err(_) => return summary,
    };
    let mut marker_sids: Vec<String> = Vec::new();
    let mut dir_sids: std::collections::HashSet<String> = std::collections::HashSet::new();
    for entry in entries.flatten() {
        let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
            continue;
        };
        let Some(rest) = name.strip_prefix("udev-") else {
            continue;
        };
        if let Some(sid) = rest.strip_suffix(".owner") {
            marker_sids.push(sid.to_string());
        } else if entry.file_type().is_ok_and(|t| t.is_dir()) {
            dir_sids.insert(rest.to_string());
        }
    }
    for sid in marker_sids {
        dir_sids.remove(&sid);
        if malformed_session_id(&sid) {
            summary.unattributable += 1;
            continue;
        }
        match read_marker(&marker_path(runtime_dir, &sid)) {
            Ok(Some(marker)) if marker.owner == owner => match retire(runtime_dir, &sid) {
                Ok(()) => summary.removed += 1,
                Err(_) => summary.errors += 1,
            },
            Ok(Some(_)) => summary.unattributable += 1,
            Ok(None) => {} // Raced away between listing and reading; nothing to do.
            Err(_) => summary.unattributable += 1,
        }
    }
    // `udev-*` directories with no marker at all: pre-fix leftovers. Leave them.
    summary.unattributable += dir_sids.len();
    summary
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;

    fn records() -> Vec<((u32, u32), String)> {
        vec![((13, 63), "I:1\nE:ID_INPUT=1\nG:seat\n".to_string())]
    }

    #[test]
    fn publish_writes_marker_before_directory() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        let dir = publish(runtime_dir, "sid1", "owner-a", &records())
            .unwrap()
            .expect("published");
        assert_eq!(dir, export_dir(runtime_dir, "sid1"));
        assert!(dir.is_dir());
        let marker = marker_path(runtime_dir, "sid1");
        let meta = std::fs::symlink_metadata(&marker).unwrap();
        assert!(meta.is_file());
        assert_eq!(meta.permissions().mode() & 0o777, 0o600);
        let marker_body: Marker = serde_json::from_slice(&std::fs::read(&marker).unwrap()).unwrap();
        assert_eq!(marker_body.owner, "owner-a");
        assert_eq!(marker_body.session, "sid1");
    }

    #[test]
    fn exported_directory_carries_only_records_never_the_marker() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        let dir = publish(runtime_dir, "sid1", "owner-a", &records())
            .unwrap()
            .expect("published");
        let names: Vec<String> = std::fs::read_dir(&dir)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        assert_eq!(names, vec!["c13:63".to_string()]);
    }

    #[test]
    fn retire_removes_directory_then_marker_and_is_idempotent() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        publish(runtime_dir, "sid1", "owner-a", &records()).unwrap();
        retire(runtime_dir, "sid1").unwrap();
        assert!(!export_dir(runtime_dir, "sid1").exists());
        assert!(!marker_path(runtime_dir, "sid1").exists());
        // Idempotent: nothing left to remove.
        retire(runtime_dir, "sid1").unwrap();
    }

    #[test]
    fn retire_all_owned_removes_only_matching_owner() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        publish(runtime_dir, "mine", "owner-a", &records()).unwrap();
        publish(runtime_dir, "theirs", "owner-b", &records()).unwrap();

        let summary = retire_all_owned(runtime_dir, "owner-a");
        assert_eq!(
            summary,
            Summary {
                removed: 1,
                unattributable: 1,
                errors: 0
            }
        );
        assert!(!export_dir(runtime_dir, "mine").exists());
        assert!(!marker_path(runtime_dir, "mine").exists());
        assert!(export_dir(runtime_dir, "theirs").is_dir());
        assert!(marker_path(runtime_dir, "theirs").is_file());
    }

    #[test]
    fn retire_all_owned_leaves_a_markerless_directory() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        std::fs::create_dir(export_dir(runtime_dir, "legacy")).unwrap();

        let summary = retire_all_owned(runtime_dir, "owner-a");
        assert_eq!(
            summary,
            Summary {
                removed: 0,
                unattributable: 1,
                errors: 0
            }
        );
        assert!(export_dir(runtime_dir, "legacy").is_dir());
    }

    #[test]
    fn retire_all_owned_leaves_a_malformed_marker() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        let marker = marker_path(runtime_dir, "broken");
        std::fs::write(&marker, b"not json").unwrap();

        let summary = retire_all_owned(runtime_dir, "owner-a");
        assert_eq!(
            summary,
            Summary {
                removed: 0,
                unattributable: 1,
                errors: 0
            }
        );
        assert!(marker.is_file());
    }

    #[test]
    fn retire_all_owned_leaves_a_session_id_with_dotdot() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        // Not created via `publish` (which never accepts such an id) — simulates
        // a hostile/corrupt filename landing directly in the runtime dir.
        let marker = tmp.path().join("udev-..evil.owner");
        std::fs::write(
            &marker,
            serde_json::to_vec(&Marker {
                owner: "owner-a".to_string(),
                session: "..evil".to_string(),
            })
            .unwrap(),
        )
        .unwrap();

        let summary = retire_all_owned(runtime_dir, "owner-a");
        assert_eq!(
            summary,
            Summary {
                removed: 0,
                unattributable: 1,
                errors: 0
            }
        );
        assert!(marker.is_file());
    }

    #[test]
    fn retire_refuses_a_symlink_where_the_directory_should_be() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        let target = tmp.path().join("elsewhere");
        std::fs::create_dir(&target).unwrap();
        std::fs::write(target.join("untouched"), b"keep").unwrap();
        symlink(&target, export_dir(runtime_dir, "sid1")).unwrap();

        let result = retire(runtime_dir, "sid1");
        assert!(result.is_err());
        // The symlink is left in place, and its target is untouched.
        assert!(export_dir(runtime_dir, "sid1")
            .symlink_metadata()
            .unwrap()
            .file_type()
            .is_symlink());
        assert!(target.join("untouched").exists());
    }

    #[test]
    fn retire_removes_a_marker_with_no_directory() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        write_marker(&marker_path(runtime_dir, "sid1"), "owner-a", "sid1").unwrap();

        retire(runtime_dir, "sid1").unwrap();
        assert!(!marker_path(runtime_dir, "sid1").exists());
    }

    #[test]
    fn publish_reclaims_its_own_stale_marker() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        // A prior life of this exact agent (e.g. the standalone harness's fixed
        // sid "demo") left a marker+dir behind without retiring it.
        publish(runtime_dir, "demo", "owner-a", &records()).unwrap();

        let dir = publish(runtime_dir, "demo", "owner-a", &records())
            .unwrap()
            .expect("republished");
        assert!(dir.is_dir());
    }

    #[test]
    fn publish_skips_export_under_a_foreign_marker() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        publish(runtime_dir, "sid1", "owner-a", &records()).unwrap();

        let result = publish(runtime_dir, "sid1", "owner-b", &records()).unwrap();
        assert!(result.is_none());
        // The original owner's export is untouched.
        let marker_body: Marker =
            serde_json::from_slice(&std::fs::read(marker_path(runtime_dir, "sid1")).unwrap())
                .unwrap();
        assert_eq!(marker_body.owner, "owner-a");
    }

    #[test]
    fn publish_refuses_a_malformed_session_id() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        for sid in ["", "../escape", "a/b", ".."] {
            let result = publish(runtime_dir, sid, "owner-a", &records());
            assert!(result.is_err(), "sid {sid:?} should be refused");
        }
        // No path traversal outside runtime_dir, and nothing created under it.
        assert!(std::fs::read_dir(runtime_dir).unwrap().next().is_none());
    }

    #[test]
    fn retire_refuses_a_malformed_session_id() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        for sid in ["", "../escape", "a/b", ".."] {
            let result = retire(runtime_dir, sid);
            assert!(result.is_err(), "sid {sid:?} should be refused");
        }
        assert!(std::fs::read_dir(runtime_dir).unwrap().next().is_none());
    }

    #[test]
    fn retire_all_owned_leaves_a_symlinked_marker() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        std::fs::create_dir(export_dir(runtime_dir, "sid1")).unwrap();
        let target = tmp.path().join("elsewhere.owner");
        std::fs::write(
            &target,
            serde_json::to_vec(&Marker {
                owner: "owner-a".to_string(),
                session: "sid1".to_string(),
            })
            .unwrap(),
        )
        .unwrap();
        symlink(&target, marker_path(runtime_dir, "sid1")).unwrap();

        let summary = retire_all_owned(runtime_dir, "owner-a");
        assert_eq!(
            summary,
            Summary {
                removed: 0,
                unattributable: 1,
                errors: 0
            }
        );
        assert!(export_dir(runtime_dir, "sid1").is_dir());
        assert!(marker_path(runtime_dir, "sid1")
            .symlink_metadata()
            .unwrap()
            .file_type()
            .is_symlink());
    }

    #[test]
    fn retire_all_owned_leaves_an_oversized_marker() {
        let tmp = tempfile::tempdir().unwrap();
        let runtime_dir = tmp.path().to_str().unwrap();
        std::fs::create_dir(export_dir(runtime_dir, "sid1")).unwrap();
        let oversized = vec![b'a'; MAX_MARKER_BYTES + 1];
        std::fs::write(marker_path(runtime_dir, "sid1"), &oversized).unwrap();

        let summary = retire_all_owned(runtime_dir, "owner-a");
        assert_eq!(
            summary,
            Summary {
                removed: 0,
                unattributable: 1,
                errors: 0
            }
        );
        assert!(export_dir(runtime_dir, "sid1").is_dir());
        assert!(marker_path(runtime_dir, "sid1").is_file());
    }
}
