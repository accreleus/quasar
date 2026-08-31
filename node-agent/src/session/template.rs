//! #488: golden-home template store — path resolution, `.meta.json` schema,
//! the reflink/copy clone ladder, and atomic publish/remove.
//!
//! Pure filesystem module: no session, Docker, or GPU dependency. Agent-owned
//! store described in `docs/design/plans/2026-08-12-488-golden-home-template.md`
//! §3.1/§4/§5 — consumed by the warm-up job (builds + sanitizes + verifies into a
//! [`StagingBuild`], calls [`TemplateStore::publish`]) and the `provision_home_dirs`
//! seeding hook in `session/home.rs` (calls [`TemplateStore::seed`] and
//! [`TemplateStore::clone_home_into`]).
//!
//! Layout (per §3.1, as published):
//! ```text
//! <template_root>/
//! ├── .staging/<image-id>-<version>-<pid>/         ← build-in-progress
//! ├── .versions/<image-id>-<version>-<ts>-<pid>-<n>/  ← published, versioned content
//! │                                                     ├── .meta.json
//! │                                                     └── home/
//! └── <image-id>                                   ← SYMLINK to the current .versions/ entry
//! ```
//!
//! `<template_root>/<image-id>` is a symlink, not a directory: publish is a
//! `rename(2)` of a symlink over a symlink, atomic, so the canonical path never
//! resolves to neither the old nor new content (see [`swap_symlink`]; an earlier
//! rename-aside-old/rename-new-in two-step DID have that window, caught by
//! `publish_is_atomic_under_concurrent_readers` under load).
//!
//! Operator constraint (#488): the template root must be a dedicated, agent-owned
//! location that is a SIBLING of the home root, never inside it — enforced here,
//! not just documented (§7.3 `QUASAR_TEMPLATE_ROOT`, acceptance step A10).

use std::fs;
use std::io;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};

use crate::session::home::is_under_root;

/// `.meta.json` schema version. Bump only with a deliberate migration; a
/// mismatched schema is treated as "no template" ([`TemplateStore::meta`]).
pub const TEMPLATE_META_SCHEMA: u32 = 1;

/// Name of the build-staging subdirectory under the template root.
const STAGING_DIR_NAME: &str = ".staging";

/// Name of the subdirectory under the template root holding versioned,
/// published template content — the rename targets that `<image-id>` symlinks
/// point at. See [`swap_symlink`].
const VERSIONS_DIR_NAME: &str = ".versions";

/// Name of the sanitized-content subdirectory inside a published template.
const HOME_DIR_NAME: &str = "home";

/// Name of the metadata file inside a published template.
const META_FILE_NAME: &str = ".meta.json";

/// `.meta.json`: the commit record for a published template (§3.1, §6.3, written
/// last). All fields required; a file that fails to parse is treated as absent.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct TemplateMeta {
    pub image_id: String,
    pub registry_ref: String,
    /// Image version string, compared against the installed image's current
    /// version on every `image_ensure` → `ready` to detect staleness (§3.2).
    pub version: String,
    pub digest: String,
    /// Unix seconds.
    pub built_at: u64,
    pub bytes: u64,
    pub files: u64,
    pub agent_version: String,
    pub schema: u32,
}

/// What [`TemplateStore::seed`] hands to the seeding hook: the resolved
/// `<template_root>/<image-id>/home` path and the image id it came from.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TemplateSeed {
    pub image_id: String,
    pub home_path: PathBuf,
}

/// The clone mechanism selected for this host, per §4.1/§4.2. Decided once at
/// startup by [`TemplateStore::resolve`] and cached — `reflink=auto` is never
/// used at runtime (it fails silently into a full copy).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CloneMode {
    /// Tier 1: `cp -a --reflink=always`. CoW, ~0.76s / 13MiB for a 2.5GiB home.
    Reflink,
    /// Tier 2: `cp -a --reflink=never`. Full copy, ~3.9s / 2.5GiB.
    Copy,
    /// Tier 3: cloning disabled — cold boot, as before the feature existed.
    Off,
}

/// `QUASAR_TEMPLATE_CLONE_MODE` (§7.3): the operator's configured preference,
/// resolved against actual filesystem support by [`TemplateStore::resolve`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TemplateCloneMode {
    /// Probe at startup: reflink if supported, else copy.
    Auto,
    /// Require reflink; fail open to [`CloneMode::Off`] (no seed) if unsupported.
    Reflink,
    /// Always full-copy, no probing.
    Copy,
    /// Cloning disabled entirely.
    Off,
}

impl TemplateCloneMode {
    /// Parse `QUASAR_TEMPLATE_CLONE_MODE`. Unrecognized/empty values default
    /// to `auto`, matching the documented default.
    pub fn parse(s: &str) -> Self {
        match s.trim().to_ascii_lowercase().as_str() {
            "reflink" => TemplateCloneMode::Reflink,
            "copy" => TemplateCloneMode::Copy,
            "off" => TemplateCloneMode::Off,
            _ => TemplateCloneMode::Auto,
        }
    }
}

/// A build in progress: a scratch directory under `.staging/` the caller fills
/// in at [`StagingBuild::home_dir`], then hands back to [`TemplateStore::publish`].
/// Dropping without publishing leaves the directory in place — the caller must
/// clean up on its own failure paths (§3.3 step 9).
#[derive(Debug)]
pub struct StagingBuild {
    dir: PathBuf,
    image_id: String,
}

impl StagingBuild {
    /// The staging directory root (the future `<template_root>/<image-id>`).
    pub fn path(&self) -> &Path {
        &self.dir
    }

    /// Where the sanitized home content goes. Created empty; the caller
    /// populates it (e.g. via `rsync`, §6.1).
    pub fn home_dir(&self) -> PathBuf {
        self.dir.join(HOME_DIR_NAME)
    }

    pub fn image_id(&self) -> &str {
        &self.image_id
    }

    /// Delete the staging directory. Errors are returned for the caller to log.
    pub fn discard(self) -> io::Result<()> {
        if self.dir.exists() {
            fs::remove_dir_all(&self.dir)?;
        }
        Ok(())
    }
}

/// The agent-owned template store for one host. Constructed once at startup
/// via [`TemplateStore::resolve`]; `None` means the feature is disabled
/// (empty/invalid configured root, or a root that resolves inside the home
/// root).
#[derive(Debug, Clone)]
pub struct TemplateStore {
    root: PathBuf,
    home_root: PathBuf,
    clone_mode: CloneMode,
    /// Template root and home root are on different filesystems (`st_dev`
    /// differs). Recorded at resolve time because the WARM-UP needs it as a
    /// refusal input, not only as a clone-ladder input — see
    /// [`TemplateStore::cross_filesystem`].
    cross_fs: bool,
}

impl TemplateStore {
    /// Resolve the template root and probe the clone mode, per §3.1/§4.1.
    ///
    /// `configured_root`: `QUASAR_TEMPLATE_ROOT`. `None` or all-whitespace uses
    /// the documented default, `<home_root>/../templates`; this function does
    /// not distinguish "unset" from "empty" — a caller wanting an explicit
    /// opt-out disables the feature before calling this.
    ///
    /// Refuses (logs ERROR, returns `None`) a root that is inside, or equal to,
    /// `home_root` — the operator constraint, enforced not documented (A10).
    pub fn resolve(
        home_root: &Path,
        configured_root: Option<&str>,
        configured_mode: TemplateCloneMode,
    ) -> Option<Self> {
        let root = resolve_template_root(home_root, configured_root)?;

        if let Err(e) = fs::create_dir_all(&root) {
            tracing::error!(
                token = "template-root-uncreatable",
                "template root {} could not be created: {e:#} — feature disabled",
                root.display()
            );
            return None;
        }
        if let Err(e) = fs::create_dir_all(root.join(STAGING_DIR_NAME)) {
            tracing::error!(
                token = "template-staging-uncreatable",
                "template staging dir under {} could not be created: {e:#} — feature disabled",
                root.display()
            );
            return None;
        }

        let clone_mode = probe_clone_mode(&root, home_root, configured_mode);
        // A stat error reports NOT cross-fs: this flag gates a refusal, and a
        // stat failure is not evidence of a split.
        let cross_fs = matches!(same_filesystem(&root, home_root), Ok(false));
        if cross_fs {
            tracing::warn!(
                token = "template-root-crossfs",
                "template root {} is on a DIFFERENT filesystem from the home root {} — on a \
                 containerized agent this usually means the template root is not bind-mounted \
                 and builds would be orphaned in the container overlay; warm-up builds are \
                 refused unless QUASAR_TEMPLATE_ALLOW_CROSSFS=1",
                root.display(),
                home_root.display()
            );
        }

        Some(TemplateStore {
            root,
            home_root: home_root.to_path_buf(),
            clone_mode,
            cross_fs,
        })
    }

    /// [`Self::resolve`], reading `QUASAR_TEMPLATE_ROOT` / `QUASAR_TEMPLATE_CLONE_MODE`
    /// from the environment. The one place both are parsed for construction —
    /// reused by `warmup::resolve_store` and the seeding wiring in `agent.rs`
    /// (gated additionally by `QUASAR_HOME_TEMPLATES`), so both apply identical
    /// resolution rules from one call site.
    pub fn resolve_from_env(home_root: &Path) -> Option<Self> {
        let mode = TemplateCloneMode::parse(
            &std::env::var("QUASAR_TEMPLATE_CLONE_MODE").unwrap_or_default(),
        );
        let configured = std::env::var("QUASAR_TEMPLATE_ROOT").ok();
        Self::resolve(home_root, configured.as_deref(), mode)
    }

    pub fn template_root(&self) -> &Path {
        &self.root
    }

    pub fn clone_mode(&self) -> CloneMode {
        self.clone_mode
    }

    /// Whether the template root and the home root are on different
    /// filesystems. Seeding still works (§4.2 tier 2, a full copy); a warm-up
    /// BUILD refuses, because on this deployment a split means the template
    /// directory is not bind-mounted into the agent container.
    pub fn cross_filesystem(&self) -> bool {
        self.cross_fs
    }

    /// Read `<template_root>/<image_id>/.meta.json`. `None` if the template
    /// doesn't exist, has no home dir, or its meta is missing/corrupt/wrong
    /// schema — all "treat as absent", never an error (§3.2 row 4, §6.3).
    pub fn meta(&self, image_id: &str) -> Option<TemplateMeta> {
        read_meta(&self.template_dir(image_id))
    }

    /// Resolve a [`TemplateSeed`] for `image_id`, or `None` if no usable
    /// template exists (see [`TemplateStore::meta`] for what "usable" means,
    /// plus the `home/` dir must actually be present).
    pub fn seed(&self, image_id: &str) -> Option<TemplateSeed> {
        let dir = self.template_dir(image_id);
        let meta = read_meta(&dir)?;
        let home = dir.join(HOME_DIR_NAME);
        if !home.is_dir() {
            tracing::warn!(
                token = "template-meta-without-home",
                "template {} has valid meta but no {} dir — treating as absent",
                image_id,
                home.display()
            );
            return None;
        }
        Some(TemplateSeed {
            image_id: meta.image_id,
            home_path: home,
        })
    }

    /// Begin a new build: create a fresh, empty staging directory under
    /// `.staging/`. The caller populates [`StagingBuild::home_dir`] and then
    /// calls [`TemplateStore::publish`].
    pub fn begin_build(&self, image_id: &str, version: &str) -> io::Result<StagingBuild> {
        let staging_root = self.root.join(STAGING_DIR_NAME);
        fs::create_dir_all(&staging_root)?;
        let pid = std::process::id();
        let dir = staging_root.join(format!("{image_id}-{version}-{pid}"));
        if dir.exists() {
            fs::remove_dir_all(&dir)?;
        }
        fs::create_dir_all(dir.join(HOME_DIR_NAME))?;
        Ok(StagingBuild {
            dir,
            image_id: image_id.to_string(),
        })
    }

    /// Publish a completed, verified build (§3.1/§6.3). Writes `meta` into the
    /// staging dir, moves it into `.versions/` as a uniquely-named entry, then
    /// atomically swaps the `<template_root>/<image_id>` symlink to point at it
    /// ([`swap_symlink`]) — a concurrent reader always sees a complete old or
    /// new template, never neither and never a partial mix. The previous
    /// version's directory is removed after the swap.
    ///
    /// The caller must sanitize and verify the staged content (§6.1/§6.3)
    /// *before* calling this; this function only writes the metadata commit
    /// record, never inspects the home content.
    pub fn publish(&self, staging: StagingBuild, meta: TemplateMeta) -> io::Result<PathBuf> {
        write_meta(&staging.dir, &meta)?;
        let link = self.template_dir(&staging.image_id);
        let versioned = self.publish_versioned(&staging.dir, &staging.image_id, &meta.version)?;
        swap_symlink(&link, &versioned)?;
        Ok(link)
    }

    /// Move a completed staging dir into `.versions/` under a fresh, unique
    /// name. Does not touch the `<image_id>` symlink — see [`swap_symlink`].
    fn publish_versioned(
        &self,
        staging_dir: &Path,
        image_id: &str,
        version: &str,
    ) -> io::Result<PathBuf> {
        let versions_root = self.root.join(VERSIONS_DIR_NAME);
        fs::create_dir_all(&versions_root)?;
        let dest = versions_root.join(versioned_dir_name(image_id, version));
        fs::rename(staging_dir, &dest)?;
        Ok(dest)
    }

    /// Delete a template (§3.2 "image uninstalled"). A no-op, not an error,
    /// if no template exists for `image_id`. Removes the `<image_id>`
    /// symlink and the versioned directory it points to.
    pub fn remove(&self, image_id: &str) -> io::Result<()> {
        let link = self.template_dir(image_id);
        let link_meta = match fs::symlink_metadata(&link) {
            Ok(m) => m,
            Err(_) => return Ok(()), // nothing published under this id
        };
        if link_meta.file_type().is_symlink() {
            let target = fs::read_link(&link).ok();
            fs::remove_file(&link)?;
            if let Some(target) = target {
                if target.exists() {
                    fs::remove_dir_all(&target)?;
                }
            }
        } else {
            // Defensive: not our symlink scheme (should not happen once this
            // module owns the template root).
            fs::remove_dir_all(&link)?;
        }
        tracing::info!("template: removed {image_id} (image uninstalled)");
        Ok(())
    }

    /// §7.1 build guard: refuse to build when free space on the template
    /// filesystem is below `max(3 × expected_template_bytes, min_free_bytes)`
    /// (staging + old + new must coexist across the swap).
    pub fn can_build(&self, expected_template_bytes: u64, min_free_bytes: u64) -> bool {
        let need = expected_template_bytes
            .saturating_mul(3)
            .max(min_free_bytes);
        has_free_space(&self.root, need)
    }

    /// §7.1 clone guard: refuse to clone when free space on the home-root
    /// filesystem is below `min_free_bytes`.
    pub fn can_clone(&self, min_free_bytes: u64) -> bool {
        has_free_space(&self.home_root, min_free_bytes)
    }

    /// Clone `source_home` (a [`TemplateSeed::home_path`]) into `dest` using
    /// the cached [`CloneMode`] for this store. `dest`'s contents end up
    /// identical to `source_home`'s; `dest` may pre-exist as an empty
    /// directory or not exist at all.
    pub fn clone_home_into(&self, source_home: &Path, dest: &Path) -> io::Result<()> {
        clone_tree(self.clone_mode, source_home, dest)
    }

    fn template_dir(&self, image_id: &str) -> PathBuf {
        self.root.join(image_id)
    }
}

/// Resolve the configured or default template root against `home_root`,
/// refusing anything inside (or equal to) it. `None` on any invalid
/// configuration — always logged before returning.
fn resolve_template_root(home_root: &Path, configured: Option<&str>) -> Option<PathBuf> {
    let root = match configured.map(str::trim) {
        Some(s) if !s.is_empty() => PathBuf::from(s),
        _ => {
            let parent = home_root.parent()?;
            parent.join("templates")
        }
    };

    if !root.is_absolute() {
        tracing::error!(
            token = "knob-template-root-not-absolute",
            "QUASAR_TEMPLATE_ROOT {} is not absolute — feature disabled",
            root.display()
        );
        return None;
    }

    if root == home_root || is_under_root(home_root, &root) {
        tracing::error!(
            token = "knob-template-root-inside-home-root",
            "QUASAR_TEMPLATE_ROOT {} is inside QUASAR_HOME_ROOT {} — refusing (Michael's \
             operator constraint, #488): the template root must be a sibling of, never inside, \
             the home root. Feature disabled.",
            root.display(),
            home_root.display()
        );
        return None;
    }

    Some(root)
}

/// Compare device ids of two existing paths.
fn same_filesystem(a: &Path, b: &Path) -> io::Result<bool> {
    let da = fs::metadata(a)?.dev();
    let db = fs::metadata(b)?.dev();
    Ok(da == db)
}

/// Probe reflink support between `template_root` and `home_root`: same
/// filesystem, then an actual `cp --reflink=always` of a small probe file.
/// Returns `Err(reason)` describing why reflink is unavailable.
fn reflink_supported(template_root: &Path, home_root: &Path) -> Result<(), String> {
    match same_filesystem(template_root, home_root) {
        Ok(true) => {}
        Ok(false) => {
            return Err("template root on a different filesystem from home root".to_string())
        }
        Err(e) => return Err(format!("cannot stat template/home root: {e}")),
    }

    let probe_src = template_root.join(".reflink-probe-src");
    let probe_dst = home_root.join(".reflink-probe-dst");
    let _ = fs::remove_file(&probe_src);
    let _ = fs::remove_file(&probe_dst);

    let write_result =
        fs::write(&probe_src, b"quasar-reflink-probe").map_err(|e| format!("probe write: {e}"));

    let cp_result = write_result.and_then(|()| run_cp(&probe_src, &probe_dst, true, false));

    let _ = fs::remove_file(&probe_src);
    let _ = fs::remove_file(&probe_dst);

    cp_result
}

/// Pure decision: given the operator's configured mode and the outcome of a
/// reflink probe, pick the [`CloneMode`] and an optional log-worthy reason.
/// Separated from [`probe_clone_mode`] so the decision table is testable
/// without touching a real filesystem.
fn decide_clone_mode(
    configured: TemplateCloneMode,
    reflink: Result<(), String>,
) -> (CloneMode, Option<String>) {
    match configured {
        TemplateCloneMode::Off => (CloneMode::Off, None),
        TemplateCloneMode::Copy => (CloneMode::Copy, None),
        TemplateCloneMode::Reflink => match reflink {
            Ok(()) => (CloneMode::Reflink, None),
            Err(reason) => (
                CloneMode::Off,
                Some(format!(
                    "reflink requested but unsupported ({reason}) — seeding disabled"
                )),
            ),
        },
        TemplateCloneMode::Auto => match reflink {
            Ok(()) => (CloneMode::Reflink, None),
            Err(reason) => (CloneMode::Copy, Some(reason)),
        },
    }
}

/// Startup probe + the `clone mode:` log line (§4.1, §7.2). Called once by
/// [`TemplateStore::resolve`] and the result cached for the process
/// lifetime — `reflink=auto` is never re-probed at clone time.
fn probe_clone_mode(
    template_root: &Path,
    home_root: &Path,
    configured: TemplateCloneMode,
) -> CloneMode {
    let reflink = if matches!(configured, TemplateCloneMode::Copy | TemplateCloneMode::Off) {
        // No need to probe: the configured mode doesn't depend on the result.
        Err("not probed (mode fixed by configuration)".to_string())
    } else {
        reflink_supported(template_root, home_root)
    };

    let (mode, reason) = decide_clone_mode(configured, reflink);
    match (mode, &reason) {
        (CloneMode::Reflink, _) => tracing::info!("clone mode: reflink"),
        (CloneMode::Copy, Some(r)) => {
            tracing::info!(token = "template-clone-mode", "clone mode: copy ({r})")
        }
        (CloneMode::Copy, None) => tracing::info!("clone mode: copy"),
        (CloneMode::Off, Some(r)) => {
            tracing::info!(token = "template-clone-mode-off", "clone mode: off ({r})")
        }
        (CloneMode::Off, None) => tracing::info!("clone mode: off"),
    }
    mode
}

/// Run `cp -a --reflink=<always|never>` (never `auto`, per §4.1) and check
/// the exit status explicitly. `dir` appends a trailing `/.` to `source` so
/// that copying into a pre-existing empty `dest` places `source`'s CONTENTS
/// directly into `dest`, rather than nesting `source` one level deeper.
fn run_cp(source: &Path, dest: &Path, reflink: bool, dir: bool) -> Result<(), String> {
    let reflink_arg = if reflink {
        "--reflink=always"
    } else {
        "--reflink=never"
    };
    let mut src_arg = source.as_os_str().to_owned();
    if dir {
        src_arg.push("/.");
    }
    let status = Command::new("cp")
        .arg("-a")
        .arg(reflink_arg)
        .arg(&src_arg)
        .arg(dest)
        .status()
        .map_err(|e| format!("spawning cp: {e}"))?;
    if !status.success() {
        return Err(format!(
            "cp -a {reflink_arg} {} {} exited {status}",
            Path::new(&src_arg).display(),
            dest.display()
        ));
    }
    Ok(())
}

/// The §4.2 clone ladder: reflink, full copy, or refuse ([`CloneMode::Off`]).
/// `dest` may pre-exist as an empty directory.
fn clone_tree(mode: CloneMode, source: &Path, dest: &Path) -> io::Result<()> {
    match mode {
        CloneMode::Off => Err(io::Error::new(
            io::ErrorKind::Unsupported,
            "template cloning is disabled (clone mode: off)",
        )),
        CloneMode::Reflink => {
            fs::create_dir_all(dest)?;
            run_cp(source, dest, true, true)
                .map_err(|e| io::Error::other(format!("reflink clone: {e}")))
        }
        CloneMode::Copy => {
            fs::create_dir_all(dest)?;
            run_cp(source, dest, false, true)
                .map_err(|e| io::Error::other(format!("copy clone: {e}")))
        }
    }
}

/// Monotonic counter, combined with pid + timestamp, so [`versioned_dir_name`]
/// stays unique across multiple publishes of the same `(image_id, version)`
/// within one process inside the same second.
static VERSION_SEQ: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);

fn versioned_dir_name(image_id: &str, version: &str) -> String {
    let seq = VERSION_SEQ.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
    let ts = now_unix();
    let pid = std::process::id();
    format!("{image_id}-{version}-{ts}-{pid}-{seq}")
}

/// Atomically flip the `link` symlink to point at `target` (§3.1). Fixes a real
/// race in an earlier rename-aside-old/rename-new-in two-step: that had a window
/// where the canonical path resolved to nothing, letting a reader observe
/// "template absent" mid-publish (forbidden by §5: old or new, never neither).
///
/// Fix: build a new symlink under a temp name, then `rename(2)` it directly over
/// `link` — both sides are symlinks (or `link` doesn't exist on first publish),
/// so the syscall is one atomic directory-entry replace. The previous target is
/// recorded before the swap and removed after.
fn swap_symlink(link: &Path, target: &Path) -> io::Result<()> {
    let parent = link
        .parent()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "link has no parent"))?;

    let previous_target = fs::read_link(link).ok();

    let link_name = link
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or("template");
    let tmp_link = parent.join(format!(".tmp-symlink-{link_name}-{}", std::process::id()));
    // Best-effort: a crashed prior attempt may have left this behind.
    let _ = fs::remove_file(&tmp_link);

    std::os::unix::fs::symlink(target, &tmp_link)?;

    if let Err(e) = fs::rename(&tmp_link, link) {
        let _ = fs::remove_file(&tmp_link);
        return Err(e);
    }

    if let Some(prev) = previous_target {
        if prev != target {
            let _ = fs::remove_dir_all(&prev);
        }
    }
    Ok(())
}

/// Write `.meta.json`. Called by [`TemplateStore::publish`] last, before the
/// rename — the file's presence (with a valid schema) is the commit record.
fn write_meta(dir: &Path, meta: &TemplateMeta) -> io::Result<()> {
    let json = serde_json::to_vec_pretty(meta)
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
    fs::write(dir.join(META_FILE_NAME), json)
}

/// Read and validate `.meta.json`. `None` on any of: missing file, unreadable,
/// corrupt JSON, or schema mismatch — all "treat as absent" (§3.2, §6.3).
fn read_meta(dir: &Path) -> Option<TemplateMeta> {
    let path = dir.join(META_FILE_NAME);
    let raw = match fs::read(&path) {
        Ok(r) => r,
        Err(_) => return None,
    };
    let meta: TemplateMeta = match serde_json::from_slice(&raw) {
        Ok(m) => m,
        Err(e) => {
            tracing::warn!(
                token = "template-meta-corrupt",
                "template meta {} is corrupt: {e} — treating template as absent",
                path.display()
            );
            return None;
        }
    };
    if meta.schema != TEMPLATE_META_SCHEMA {
        tracing::warn!(
            token = "template-meta-schema-mismatch",
            "template meta {} has schema {} (expected {}) — treating as absent",
            path.display(),
            meta.schema,
            TEMPLATE_META_SCHEMA
        );
        return None;
    }
    Some(meta)
}

/// Free bytes available to a non-root writer on the filesystem containing
/// `path` (`statvfs.f_bavail * f_frsize`). `path` must exist.
fn free_bytes(path: &Path) -> io::Result<u64> {
    use std::ffi::CString;
    use std::os::unix::ffi::OsStrExt;

    let c_path =
        CString::new(path.as_os_str().as_bytes()).map_err(|e| io::Error::other(e.to_string()))?;
    let mut stat: libc::statvfs = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::statvfs(c_path.as_ptr(), &mut stat) };
    if rc != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(stat.f_bavail.saturating_mul(stat.f_frsize))
}

/// `free_bytes(path) >= min_bytes`, `false` (not an error) if `path` can't be
/// statted — a guard should fail closed.
fn has_free_space(path: &Path, min_bytes: u64) -> bool {
    free_bytes(path).map(|b| b >= min_bytes).unwrap_or(false)
}

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write as _;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::Arc;

    fn sample_meta(image_id: &str, version: &str) -> TemplateMeta {
        TemplateMeta {
            image_id: image_id.to_string(),
            registry_ref: format!("ghcr.io/accreleus/{image_id}:{version}"),
            version: version.to_string(),
            digest: "sha256:deadbeef".to_string(),
            built_at: 1_723_000_000,
            bytes: 2_684_354_560,
            files: 23_701,
            agent_version: "test".to_string(),
            schema: TEMPLATE_META_SCHEMA,
        }
    }

    #[test]
    fn resolve_template_root_default_sibling() {
        let home_root = PathBuf::from("/var/lib/quasar/homes");
        let root = resolve_template_root(&home_root, None).unwrap();
        assert_eq!(root, PathBuf::from("/var/lib/quasar/templates"));
    }

    #[test]
    fn resolve_template_root_configured_override() {
        let home_root = PathBuf::from("/var/lib/quasar/homes");
        let root = resolve_template_root(&home_root, Some("/mnt/fast/templates")).unwrap();
        assert_eq!(root, PathBuf::from("/mnt/fast/templates"));
    }

    #[test]
    fn resolve_template_root_inside_home_root_refused() {
        let home_root = PathBuf::from("/var/lib/quasar/homes");
        assert_eq!(
            resolve_template_root(&home_root, Some("/var/lib/quasar/homes/templates")),
            None
        );
    }

    #[test]
    fn resolve_template_root_equal_home_root_refused() {
        let home_root = PathBuf::from("/var/lib/quasar/homes");
        assert_eq!(
            resolve_template_root(&home_root, Some("/var/lib/quasar/homes")),
            None
        );
    }

    #[test]
    fn resolve_template_root_sibling_prefix_not_confused_with_inside() {
        // A sibling with a shared prefix (homes-extra) must not be refused.
        let home_root = PathBuf::from("/var/lib/quasar/homes");
        let root = resolve_template_root(&home_root, Some("/var/lib/quasar/homes-extra")).unwrap();
        assert_eq!(root, PathBuf::from("/var/lib/quasar/homes-extra"));
    }

    #[test]
    fn resolve_template_root_relative_refused() {
        let home_root = PathBuf::from("/var/lib/quasar/homes");
        assert_eq!(
            resolve_template_root(&home_root, Some("relative/templates")),
            None
        );
    }

    // --- TemplateStore::resolve end-to-end (tempfile root) ----------------

    #[test]
    fn template_store_resolve_inside_home_root_disables_feature() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let bad = home_root.join("templates").to_str().unwrap().to_string();
        assert!(TemplateStore::resolve(&home_root, Some(&bad), TemplateCloneMode::Copy).is_none());
    }

    #[test]
    fn template_store_resolve_default_sibling_creates_root() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();
        assert_eq!(store.template_root(), base.path().join("templates"));
        assert!(store.template_root().is_dir());
        assert_eq!(store.clone_mode(), CloneMode::Copy);
    }

    #[test]
    fn template_root_excluded_from_gc_reaper() {
        // The GC reaper confines reaping to home::is_under_root(home_root, path);
        // a resolved template root must never satisfy that check.
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();

        assert!(!is_under_root(&home_root, store.template_root()));
        assert!(!is_under_root(
            &home_root,
            &store.template_root().join("steam")
        ));
        assert!(!is_under_root(
            &home_root,
            &store.template_root().join("steam").join("home")
        ));
    }

    #[test]
    fn decide_clone_mode_auto_falls_back_to_copy_when_unsupported() {
        let (mode, reason) =
            decide_clone_mode(TemplateCloneMode::Auto, Err("simulated".to_string()));
        assert_eq!(mode, CloneMode::Copy);
        assert_eq!(reason.as_deref(), Some("simulated"));
    }

    #[test]
    fn decide_clone_mode_auto_uses_reflink_when_supported() {
        let (mode, reason) = decide_clone_mode(TemplateCloneMode::Auto, Ok(()));
        assert_eq!(mode, CloneMode::Reflink);
        assert_eq!(reason, None);
    }

    #[test]
    fn decide_clone_mode_explicit_reflink_fails_open_to_off() {
        let (mode, reason) =
            decide_clone_mode(TemplateCloneMode::Reflink, Err("no xfs".to_string()));
        assert_eq!(mode, CloneMode::Off);
        assert!(reason.unwrap().contains("no xfs"));
    }

    #[test]
    fn decide_clone_mode_copy_is_never_probed_dependent() {
        let (mode, _) = decide_clone_mode(TemplateCloneMode::Copy, Err("irrelevant".to_string()));
        assert_eq!(mode, CloneMode::Copy);
        let (mode, _) = decide_clone_mode(TemplateCloneMode::Copy, Ok(()));
        assert_eq!(mode, CloneMode::Copy);
    }

    #[test]
    fn decide_clone_mode_off_is_always_off() {
        let (mode, _) = decide_clone_mode(TemplateCloneMode::Off, Ok(()));
        assert_eq!(mode, CloneMode::Off);
    }

    #[test]
    fn same_filesystem_true_within_one_tempdir() {
        let base = tempfile::tempdir().unwrap();
        let a = base.path().join("a");
        let b = base.path().join("b");
        fs::create_dir_all(&a).unwrap();
        fs::create_dir_all(&b).unwrap();
        assert!(same_filesystem(&a, &b).unwrap());
    }

    #[test]
    fn same_filesystem_false_across_real_pseudo_filesystems() {
        // /proc reliably has a different st_dev than a tempdir, exercising the
        // "different filesystem" branch without root or a real second mount.
        let base = tempfile::tempdir().unwrap();
        let proc_root = Path::new("/proc");
        if !proc_root.is_dir() {
            eprintln!("skipping: /proc not present on this host");
            return;
        }
        match same_filesystem(base.path(), proc_root) {
            Ok(same) => assert!(!same, "/proc unexpectedly shares a device with a tempdir"),
            Err(e) => panic!("stat failed: {e}"),
        }
    }

    #[test]
    fn meta_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        let meta = sample_meta("steam", "1.2.3");
        write_meta(dir.path(), &meta).unwrap();
        assert_eq!(read_meta(dir.path()), Some(meta));
    }

    #[test]
    fn meta_missing_is_absent() {
        let dir = tempfile::tempdir().unwrap();
        assert_eq!(read_meta(dir.path()), None);
    }

    #[test]
    fn meta_corrupt_json_is_absent() {
        let dir = tempfile::tempdir().unwrap();
        let mut f = fs::File::create(dir.path().join(META_FILE_NAME)).unwrap();
        f.write_all(b"{ not valid json").unwrap();
        assert_eq!(read_meta(dir.path()), None);
    }

    #[test]
    fn meta_truncated_is_absent() {
        // Acceptance step A6.
        let dir = tempfile::tempdir().unwrap();
        fs::write(dir.path().join(META_FILE_NAME), b"").unwrap();
        assert_eq!(read_meta(dir.path()), None);
    }

    #[test]
    fn meta_wrong_schema_is_absent() {
        let dir = tempfile::tempdir().unwrap();
        let mut meta = sample_meta("steam", "1.2.3");
        meta.schema = TEMPLATE_META_SCHEMA + 1;
        write_meta(dir.path(), &meta).unwrap();
        assert_eq!(read_meta(dir.path()), None);
    }

    #[test]
    fn store_meta_and_seed_absent_when_no_template() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();
        assert_eq!(store.meta("steam"), None);
        assert_eq!(store.seed("steam"), None);
    }

    #[test]
    fn store_seed_absent_when_meta_present_but_home_dir_missing() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();

        let template_dir = store.template_root().join("steam");
        fs::create_dir_all(&template_dir).unwrap();
        write_meta(&template_dir, &sample_meta("steam", "1.2.3")).unwrap();
        assert!(store.meta("steam").is_some());
        assert_eq!(store.seed("steam"), None);
    }

    #[test]
    fn begin_build_creates_empty_staging_home_dir() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();

        let build = store.begin_build("steam", "1.2.3").unwrap();
        assert!(build.home_dir().is_dir());
        assert!(build
            .path()
            .starts_with(store.template_root().join(STAGING_DIR_NAME)));
    }

    #[test]
    fn publish_first_time_creates_template_with_meta_and_home() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();

        let build = store.begin_build("steam", "1.0.0").unwrap();
        fs::write(build.home_dir().join("steam.sh"), b"#!/bin/sh\n").unwrap();
        let meta = sample_meta("steam", "1.0.0");
        let dest = store.publish(build, meta.clone()).unwrap();

        assert_eq!(dest, store.template_root().join("steam"));
        assert!(dest.join(HOME_DIR_NAME).join("steam.sh").is_file());
        assert_eq!(store.meta("steam"), Some(meta));
        assert!(store.seed("steam").is_some());
        assert!(!build_dir_survives(store.template_root()));
    }

    fn build_dir_survives(template_root: &Path) -> bool {
        let staging = template_root.join(STAGING_DIR_NAME);
        fs::read_dir(&staging)
            .map(|mut it| it.any(|e| e.is_ok()))
            .unwrap_or(false)
    }

    #[test]
    fn publish_replaces_existing_template_atomically_in_sequence() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();

        let b1 = store.begin_build("steam", "1.0.0").unwrap();
        fs::write(b1.home_dir().join("marker"), b"v1").unwrap();
        store.publish(b1, sample_meta("steam", "1.0.0")).unwrap();
        assert_eq!(store.meta("steam").unwrap().version, "1.0.0");

        let b2 = store.begin_build("steam", "2.0.0").unwrap();
        fs::write(b2.home_dir().join("marker"), b"v2").unwrap();
        store.publish(b2, sample_meta("steam", "2.0.0")).unwrap();

        assert_eq!(store.meta("steam").unwrap().version, "2.0.0");
        let marker = fs::read(
            store
                .template_root()
                .join("steam")
                .join(HOME_DIR_NAME)
                .join("marker"),
        )
        .unwrap();
        assert_eq!(marker, b"v2");
        // Exactly one live version; v1's versioned directory was removed
        // after the swap, no leak in .versions/.
        assert!(fs::symlink_metadata(store.template_root().join("steam"))
            .unwrap()
            .file_type()
            .is_symlink());
        let versions = store.template_root().join(VERSIONS_DIR_NAME);
        let live: Vec<_> = fs::read_dir(&versions)
            .unwrap()
            .filter_map(|e| e.ok())
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .collect();
        assert_eq!(live.len(), 1, "expected exactly one live version: {live:?}");
        assert!(live[0].starts_with("steam-2.0.0-"), "{live:?}");
    }

    #[test]
    fn publish_is_atomic_under_concurrent_readers() {
        // A reader polling meta() throughout a publish must only ever see the
        // fully-old or fully-new version, never a torn/partial read (§3.1).
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();

        let b1 = store.begin_build("steam", "1.0.0").unwrap();
        fs::write(b1.home_dir().join("marker"), vec![b'1'; 4096]).unwrap();
        store.publish(b1, sample_meta("steam", "1.0.0")).unwrap();

        let stop = Arc::new(AtomicBool::new(false));
        let reader_store = store.clone();
        let reader_stop = Arc::clone(&stop);
        let reader = std::thread::spawn(move || {
            let mut saw_v1 = false;
            let mut saw_v2 = false;
            while !reader_stop.load(Ordering::Relaxed) {
                match reader_store.meta("steam") {
                    Some(m) if m.version == "1.0.0" => saw_v1 = true,
                    Some(m) if m.version == "2.0.0" => saw_v2 = true,
                    Some(m) => panic!("torn/unexpected read: {m:?}"),
                    None => panic!("reader observed template as absent mid-swap"),
                }
            }
            (saw_v1, saw_v2)
        });

        let b2 = store.begin_build("steam", "2.0.0").unwrap();
        fs::write(b2.home_dir().join("marker"), vec![b'2'; 4096]).unwrap();
        store.publish(b2, sample_meta("steam", "2.0.0")).unwrap();

        stop.store(true, Ordering::Relaxed);
        let (saw_v1, saw_v2) = reader.join().unwrap();
        // Not asserting both were observed (scheduling-dependent); the reader
        // thread already enforced "never None, never a third value" by panicking.
        assert!(saw_v1 || saw_v2);
    }

    #[test]
    fn remove_deletes_published_template() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();

        let build = store.begin_build("steam", "1.0.0").unwrap();
        store.publish(build, sample_meta("steam", "1.0.0")).unwrap();
        assert!(store.meta("steam").is_some());

        store.remove("steam").unwrap();
        assert_eq!(store.meta("steam"), None);
        assert!(!store.template_root().join("steam").exists());
    }

    #[test]
    fn remove_nonexistent_template_is_not_an_error() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();
        assert!(store.remove("nonexistent").is_ok());
    }

    #[test]
    fn clone_tree_copy_mode_duplicates_contents() {
        let base = tempfile::tempdir().unwrap();
        let source = base.path().join("source");
        fs::create_dir_all(source.join("sub")).unwrap();
        fs::write(source.join("top.txt"), b"top").unwrap();
        fs::write(source.join("sub").join("nested.txt"), b"nested").unwrap();

        let dest = base.path().join("dest");
        clone_tree(CloneMode::Copy, &source, &dest).unwrap();

        assert_eq!(fs::read(dest.join("top.txt")).unwrap(), b"top");
        assert_eq!(
            fs::read(dest.join("sub").join("nested.txt")).unwrap(),
            b"nested"
        );
    }

    #[test]
    fn clone_tree_off_mode_refuses() {
        let base = tempfile::tempdir().unwrap();
        let source = base.path().join("source");
        fs::create_dir_all(&source).unwrap();
        let dest = base.path().join("dest");
        let err = clone_tree(CloneMode::Off, &source, &dest).unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::Unsupported);
    }

    #[test]
    fn clone_tree_into_preexisting_empty_dest() {
        // The seeding hook creates the leaf dir via create_dir_all before
        // deciding whether to seed it (§3.5 step 2): clone_tree must handle a
        // dest that already exists (empty).
        let base = tempfile::tempdir().unwrap();
        let source = base.path().join("source");
        fs::create_dir_all(&source).unwrap();
        fs::write(source.join("f"), b"x").unwrap();
        let dest = base.path().join("dest");
        fs::create_dir_all(&dest).unwrap();

        clone_tree(CloneMode::Copy, &source, &dest).unwrap();
        assert_eq!(fs::read(dest.join("f")).unwrap(), b"x");
    }

    #[test]
    fn store_clone_home_into_seeds_a_new_leaf() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();

        let build = store.begin_build("steam", "1.0.0").unwrap();
        fs::write(build.home_dir().join("steam.sh"), b"#!/bin/sh\n").unwrap();
        store.publish(build, sample_meta("steam", "1.0.0")).unwrap();

        let seed = store.seed("steam").unwrap();
        let target = home_root.join("alice").join("steam");
        store.clone_home_into(&seed.home_path, &target).unwrap();

        assert_eq!(fs::read(target.join("steam.sh")).unwrap(), b"#!/bin/sh\n");
    }

    #[test]
    fn can_build_and_can_clone_pass_with_low_threshold() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();

        assert!(store.can_build(0, 1));
        assert!(store.can_clone(1));
    }

    #[test]
    fn can_build_and_can_clone_fail_with_absurd_threshold() {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, TemplateCloneMode::Copy).unwrap();

        let absurd = u64::MAX / 2;
        assert!(!store.can_build(absurd, absurd));
        assert!(!store.can_clone(absurd));
    }

    #[test]
    fn free_bytes_reports_something_plausible() {
        let dir = tempfile::tempdir().unwrap();
        let bytes = free_bytes(dir.path()).unwrap();
        assert!(bytes > 0);
    }
}
