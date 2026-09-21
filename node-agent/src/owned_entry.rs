//! Generic owner-marked runtime-dir entry: `{runtime_dir}/{prefix}{id}` (a
//! directory) with a sibling ownership proof at `{runtime_dir}/{prefix}{id}.owner`
//! (a small JSON file). Shared by `session::udev_export` and
//! `host_probe::media_probe_dir` rather than each keeping its own copy.
//!
//! The marker is written, synced, and fsynced to its parent BEFORE the entry
//! directory exists: the durable obligation precedes the thing it obliges. A clean
//! caller retires both explicitly; a killed agent leaves them for a boot-only
//! [`retire_all_owned`] pass — never a periodic tick, since "ours" only implies
//! "dead" right after that boot-time application retirement.
//!
//! Every symlink-refusal / bounded-read / marker-last-on-write-first-on-remove rule
//! here is security-sensitive — extend call sites, don't duplicate the mechanics.

use std::fs::{File, OpenOptions};
use std::io::{Read, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};

use anyhow::{anyhow, Context, Result};
use serde::de::DeserializeOwned;
use serde::Serialize;

pub(crate) const MAX_MARKER_BYTES: usize = 4096;

/// A marker body that carries this entry's owner token. `owner()` is all the
/// generic reconcile logic needs; the rest of the shape (an id, a session, ...)
/// is caller-defined.
pub(crate) trait OwnedMarker: Serialize + DeserializeOwned {
    fn owner(&self) -> &str;
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

/// `{runtime_dir}/{prefix}{id}`.
pub(crate) fn entry_dir(runtime_dir: &str, prefix: &str, id: &str) -> PathBuf {
    PathBuf::from(runtime_dir).join(format!("{prefix}{id}"))
}

/// `{runtime_dir}/{prefix}{id}.owner`, the sibling ownership marker.
pub(crate) fn marker_path(runtime_dir: &str, prefix: &str, id: &str) -> PathBuf {
    PathBuf::from(runtime_dir).join(format!("{prefix}{id}.owner"))
}

fn fsync_parent(path: &Path) -> Result<()> {
    let parent = path
        .parent()
        .ok_or_else(|| anyhow!("{} has no parent", path.display()))?;
    std::fs::File::open(parent)
        .and_then(|f| f.sync_all())
        .with_context(|| format!("fsync parent of {}", path.display()))
}

fn open_marker_new(path: &Path) -> std::io::Result<File> {
    OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)
}

/// Bounded, symlink-refusing marker read. `Ok(None)` = no marker. `Err` = present
/// but unreadable/malformed — the caller treats that as unattributable, not fatal.
pub(crate) fn read_marker<M: OwnedMarker>(path: &Path) -> Result<Option<M>> {
    let file = match OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)
    {
        Ok(f) => f,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e).context("open owned-entry marker"),
    };
    let metadata = file.metadata().context("stat owned-entry marker")?;
    if !metadata.is_file() {
        return Err(anyhow!(
            "owned-entry marker at {} is not a regular file",
            path.display()
        ));
    }
    let mut bytes = Vec::new();
    file.take((MAX_MARKER_BYTES + 1) as u64)
        .read_to_end(&mut bytes)
        .context("read owned-entry marker")?;
    if bytes.len() > MAX_MARKER_BYTES {
        return Err(anyhow!(
            "owned-entry marker at {} exceeds bound",
            path.display()
        ));
    }
    let marker: M = serde_json::from_slice(&bytes).context("parse owned-entry marker")?;
    Ok(Some(marker))
}

/// Write a marker, failing if one already exists at `path`.
pub(crate) fn write_marker<M: OwnedMarker>(path: &Path, marker: &M) -> Result<()> {
    if try_write_marker(path, marker)? {
        Ok(())
    } else {
        Err(anyhow!(
            "owned-entry marker {} already exists",
            path.display()
        ))
    }
}

/// Write a marker, returning `Ok(false)` instead of erroring on an id collision
/// (`AlreadyExists`) so a caller minting random ids can retry with a new one.
pub(crate) fn try_write_marker<M: OwnedMarker>(path: &Path, marker: &M) -> Result<bool> {
    let body = serde_json::to_vec(marker).context("serialize owned-entry marker")?;
    let mut file = match open_marker_new(path) {
        Ok(file) => file,
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => return Ok(false),
        Err(e) => return Err(e).with_context(|| format!("create {}", path.display())),
    };
    file.write_all(&body)
        .and_then(|_| file.sync_all())
        .with_context(|| format!("write {}", path.display()))?;
    fsync_parent(path)?;
    Ok(true)
}

/// Remove the entry directory content-first: refuse a symlink or non-directory
/// outright (leave it untouched), otherwise remove its (plain-file) entries then
/// the directory itself. Missing is success.
pub(crate) fn remove_entry_dir(dir: &Path) -> Result<()> {
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
                "unexpected non-file entry {} in owned entry dir",
                entry.path().display()
            ));
        }
        std::fs::remove_file(entry.path())
            .with_context(|| format!("remove {}", entry.path().display()))?;
    }
    std::fs::remove_dir(dir).with_context(|| format!("remove {}", dir.display()))
}

pub(crate) fn remove_marker(path: &Path) -> Result<()> {
    match std::fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e).with_context(|| format!("remove {}", path.display())),
    }
}

/// Idempotent: remove the entry directory, THEN its marker — marker last, so an
/// interrupted retire keeps its ownership proof for the next attempt (boot sweep
/// or a retry of this same call). A missing directory still removes a lingering
/// marker.
pub(crate) fn retire_entry(runtime_dir: &str, prefix: &str, id: &str) -> Result<()> {
    remove_entry_dir(&entry_dir(runtime_dir, prefix, id))?;
    remove_marker(&marker_path(runtime_dir, prefix, id))
}

/// Boot-only reconciliation: retire every `{prefix}*` marker+directory this
/// `owner` token wrote, leaving anything it cannot attribute to itself. Never
/// follows a symlink. Must run only where "ours" implies "dead" (boot, after
/// application retirement) — never the periodic maintenance tick.
pub(crate) fn retire_all_owned<M: OwnedMarker>(
    runtime_dir: &str,
    prefix: &str,
    owner: &str,
    malformed_id: impl Fn(&str) -> bool,
) -> Summary {
    let mut summary = Summary::default();
    let entries = match std::fs::read_dir(runtime_dir) {
        Ok(entries) => entries,
        Err(_) => return summary,
    };
    let mut marker_ids: Vec<String> = Vec::new();
    let mut dir_ids: std::collections::HashSet<String> = std::collections::HashSet::new();
    for entry in entries.flatten() {
        let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
            continue;
        };
        let Some(rest) = name.strip_prefix(prefix) else {
            continue;
        };
        if let Some(id) = rest.strip_suffix(".owner") {
            marker_ids.push(id.to_string());
        } else if entry.file_type().is_ok_and(|t| t.is_dir()) {
            dir_ids.insert(rest.to_string());
        }
    }
    for id in marker_ids {
        dir_ids.remove(&id);
        if malformed_id(&id) {
            summary.unattributable += 1;
            continue;
        }
        match read_marker::<M>(&marker_path(runtime_dir, prefix, &id)) {
            Ok(Some(marker)) if marker.owner() == owner => {
                match retire_entry(runtime_dir, prefix, &id) {
                    Ok(()) => summary.removed += 1,
                    Err(_) => summary.errors += 1,
                }
            }
            Ok(Some(_)) => summary.unattributable += 1,
            Ok(None) => {} // Raced away between listing and reading; nothing to do.
            Err(_) => summary.unattributable += 1,
        }
    }
    // `{prefix}*` directories with no marker at all: pre-fix leftovers. Leave them.
    summary.unattributable += dir_ids.len();
    summary
}
