//! P5-03 agent-side home management: pre-launch directory provisioning and
//! post-session bounded usage measurement for local-driver homes.
//!
//! The home root (unset/empty ⇒ no-op) is the host root under which the control
//! plane's `localDriver` synthesises per-(user, app) paths. Any bind-mount the
//! agent provisions must be STRICTLY under that root (component-aware
//! `Path::starts_with` — `/foo/bar-baz` does NOT match `/foo/bar`); never
//! creates paths outside it.
//!
//! #377: session-scoped provision/measure take their root from the session's
//! **effective** `home_root` (env baseline overlaid by a `config_update`,
//! snapshotted onto `SessionConfig`) via [`resolve_root`]. The process-level
//! `configured_home_root` (capacity/GC) still reads env directly — not
//! session-scoped.

use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use super::template::CloneMode;
use super::template::{TemplateSeed, TemplateStore};

#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
pub struct HomeSeedOutcome {
    pub mode: &'static str,
    pub reason: &'static str,
}

/// How long the du walk is allowed to run before returning whatever it has.
const DU_TIMEOUT: Duration = Duration::from_secs(10);
/// Maximum directory depth the du walk will descend into.
const DU_DEPTH_CAP: u32 = 32;

/// #488 WP3 §3.5: what [`provision_home_dirs`] needs to seed a cold home from a
/// published golden-home template. Bundles the resolved [`TemplateSeed`] (what
/// to clone) with the [`TemplateStore`] it came from (its cached reflink-vs-copy
/// clone mode, probed once at startup) — the caller already holds both after
/// `store.seed(image_id)`.
pub struct TemplateSeeder<'a> {
    pub store: &'a TemplateStore,
    pub seed: &'a TemplateSeed,
    pub authorization: Option<&'a crate::source_policy::PolicyLease>,
}

/// Resolve an effective home-root string to a validated absolute path. `None`
/// when empty (local driver not in use; no-op) or — defensively — not absolute
/// (warn logged, never a panic mid-session; measurement disabled). Trims
/// whitespace before validating.
pub(crate) fn resolve_root(raw: &str) -> Option<PathBuf> {
    let v = raw.trim();
    if v.is_empty() {
        return None;
    }
    let p = PathBuf::from(v);
    if !p.is_absolute() {
        tracing::warn!(
            token = "knob-invalid-home-root",
            "home_root {v:?} is not absolute — ignoring (measurement disabled)"
        );
        return None;
    }
    Some(p)
}

/// Read `QUASAR_HOME_ROOT` from the environment. `None` when unset/empty
/// (local driver not in use). Used by process-level consumers (capacity/GC)
/// that are not session-scoped and have no `config_update` overlay.
fn home_root() -> Option<PathBuf> {
    resolve_root(&std::env::var("QUASAR_HOME_ROOT").unwrap_or_default())
}

/// Parse the host-path component of a Docker bind-mount string
/// (`host:container[:mode]`). Returns `None` for named volumes (no `/` prefix)
/// or for paths containing `..`.
pub(crate) fn host_path_of(mount: &str) -> Option<PathBuf> {
    let host = mount.split(':').next()?;
    // Named volumes have no leading `/`.
    if !host.starts_with('/') {
        return None;
    }
    let p = PathBuf::from(host);
    // Reject any traversal attempts.
    if p.components().any(|c| c.as_os_str() == "..") {
        return None;
    }
    Some(p)
}

/// True iff `path` is strictly under `root` (component-aware: `/foo/bar-baz` is
/// NOT under `/foo/bar`).
pub(crate) fn is_under_root(root: &Path, path: &Path) -> bool {
    path.starts_with(root) && path != root
}

/// `QUASAR_HOME_ROOT` as configured, or `None`. Re-exported for the GC reaper,
/// which must confine local-driver removals to the same root the provisioner
/// uses.
pub(crate) fn configured_home_root() -> Option<PathBuf> {
    home_root()
}

/// Legacy/non-Steam pre-launch provisioning under `QUASAR_HOME_ROOT`.
/// Errors remain best-effort here. Steam uses
/// [`provision_home_dirs_with_result`] so an uncertain managed home fails
/// safely instead of being silently called cold.
///
/// Docker creates a missing bind-mount source as `root:root 755`, unwritable by
/// a non-root container user; pre-creating it as the agent user lets the
/// container inherit the intended ownership/mode.
///
/// #488 WP3 §3.5: `template` is a resolved golden-home seed, or `None` (feature
/// off, no template, or upstream resolution failed) — `None` behaves exactly
/// like `create_dir_all` alone. When `Some`, only the unambiguous single
/// managed-home mount under `home_root` is eligible, and only if its leaf is
/// cold (absent or empty) — a warm or non-empty home is never touched. Every
/// seeding failure (clone, chown) is fail-open: logged, partial writes deleted,
/// leaf left empty, session pays today's cold-boot cost. This function's
/// best-effort contract applies to this compatibility wrapper only.
pub fn provision_home_dirs(
    mounts: &[String],
    home_root: &str,
    template: Option<TemplateSeeder<'_>>,
) {
    if let Some(root) = resolve_root(home_root) {
        let candidates: Vec<PathBuf> = mounts
            .iter()
            .filter_map(|m| host_path_of(m))
            .filter(|h| is_under_root(&root, h))
            .collect();
        if candidates.len() > 1 {
            for host in candidates {
                if let Err(error) = std::fs::create_dir_all(&host) {
                    tracing::warn!(
                        token = "home-provision-compat-failed",
                        "provision home dir {}: {error:#}",
                        host.display()
                    );
                }
            }
            return;
        }
    }
    let _ = provision_home_dirs_with_result(mounts, home_root, template, "template_unavailable");
}

/// Provision an initial managed home and report only what this launch proved.
/// An uncertain partial clone is an error so it cannot be mislabeled cold.
pub fn provision_home_dirs_with_result(
    mounts: &[String],
    home_root: &str,
    template: Option<TemplateSeeder<'_>>,
    cold_reason: &'static str,
) -> std::io::Result<Option<HomeSeedOutcome>> {
    let Some(root) = resolve_root(home_root) else {
        return Ok(None);
    };

    // §3.5: collect every host path strictly under the home root BEFORE
    // creating anything, so ambiguity is decided up front.
    let candidates: Vec<PathBuf> = mounts
        .iter()
        .filter_map(|m| host_path_of(m))
        .filter(|h| is_under_root(&root, h))
        .collect();

    if candidates.len() > 1 {
        return Err(std::io::Error::other("ambiguous managed-home mounts"));
    }

    let seed_target: Option<&Path> = match (&template, candidates.as_slice()) {
        (Some(_), [only]) => Some(only.as_path()),
        (Some(_), matches) if matches.len() > 1 => {
            tracing::warn!(
                token = "template-seed-ambiguous-mount",
                "template: seed skipped — {} managed-home mounts matched under {} \
                 (unambiguous match required)",
                matches.len(),
                root.display()
            );
            None
        }
        _ => None,
    };

    let mut outcome = None;
    for host in &candidates {
        // §3.5 step 1: check the leaf BEFORE create_dir_all, which is
        // idempotent and cannot itself tell cold from warm.
        let pre_existed = host.try_exists()?;
        let pre_empty = !pre_existed || std::fs::read_dir(host)?.next().is_none();

        match std::fs::create_dir_all(host) {
            Ok(()) => tracing::info!("provisioned home dir: {}", host.display()),
            Err(e) => {
                tracing::warn!(
                    token = "home-provision-failed",
                    "provision home dir {}: {e:#}",
                    host.display()
                );
                return Err(e);
            }
        }

        // §3.5 steps 3-5: seed only a cold leaf that is THE unambiguous match.
        if candidates.len() == 1 {
            outcome = Some(if !pre_empty {
                HomeSeedOutcome {
                    mode: "existing",
                    reason: "existing_home",
                }
            } else if seed_target == Some(host.as_path()) {
                match template.as_ref() {
                    Some(seeder) => seed_home(seeder, host)?,
                    None => cold_if_empty(host, cold_reason)?,
                }
            } else {
                cold_if_empty(host, cold_reason)?
            });
        }
    }
    Ok(outcome)
}

/// A cold report requires a currently empty destination; a writer racing
/// provisioning must not be mislabeled cold.
fn cold_if_empty(dest: &Path, reason: &'static str) -> std::io::Result<HomeSeedOutcome> {
    if std::fs::read_dir(dest)?.next().is_some() {
        return Err(std::io::Error::other(
            "managed home changed during provisioning",
        ));
    }
    Ok(HomeSeedOutcome {
        mode: "cold",
        reason,
    })
}

/// Clone into staging, then install only while the real destination is empty.
/// Optional clone failure may proceed cold only after safe cleanup is proven.
fn seed_home(seeder: &TemplateSeeder<'_>, dest: &Path) -> std::io::Result<HomeSeedOutcome> {
    let started = Instant::now();
    // Clone outside the destination, then atomically install only while the
    // current policy lease is held. A live disable leaves a cold empty home.
    let staging = match tempfile::Builder::new()
        .prefix(".quasar-seed-")
        .tempdir_in(dest.parent().unwrap_or(dest))
    {
        Ok(dir) => dir,
        Err(error) => {
            tracing::warn!(token = "template-seed-staging-failed", "{error}");
            return cold_if_empty(dest, "storage_unavailable");
        }
    };
    let original_dest = dest;
    let dest = staging.path();
    if let Err(e) = seeder.store.clone_home_into(&seeder.seed.home_path, dest) {
        tracing::warn!(
            token = "template-seed-failed",
            "template: seed FAILED for {}: {e:#} — falling back to empty home",
            dest.display()
        );
        reset_to_empty_dir(dest)?;
        return cold_if_empty(original_dest, "clone_failed");
    }
    if let Some((uid, gid)) = crate::agent::app_uid_gid() {
        if let Err(e) = chown_recursive(dest, uid, gid) {
            tracing::warn!(
                token = "template-seed-chown-failed",
                "template: seed FAILED for {} (chown to {uid}:{gid}): {e:#} — \
                 falling back to empty home",
                dest.display()
            );
            reset_to_empty_dir(dest)?;
            return cold_if_empty(original_dest, "clone_failed");
        }
    }
    let install = || {
        if original_dest.read_dir()?.next().is_some() {
            return Err(std::io::Error::other("home is no longer empty"));
        }
        std::fs::rename(dest, original_dest)
    };
    if let Some(authorization) = seeder.authorization {
        if let Err(error) = authorization.commit(install) {
            if crate::source_policy::is_policy_revoked(&error) {
                tracing::warn!(
                    token = "template-seed-policy-changed",
                    "{error}; leaving home unseeded"
                );
                return cold_if_empty(original_dest, "policy_changed");
            }
            return Err(error);
        }
    } else {
        install()?;
    }
    tracing::info!(
        "template: seeded home {} from {} in {} ms ({:?})",
        dest.display(),
        seeder.seed.image_id,
        started.elapsed().as_millis(),
        seeder.store.clone_mode(),
    );
    Ok(HomeSeedOutcome {
        mode: match seeder.store.clone_mode() {
            CloneMode::Reflink => "reflink",
            CloneMode::Copy => "copy",
            CloneMode::Off => {
                return Err(std::io::Error::other(
                    "clone mode off after successful clone",
                ))
            }
        },
        reason: "seeded",
    })
}

/// Delete a failed staging clone and prove it is empty. Any uncertain cleanup
/// fails the Steam launch rather than reporting a cold home.
fn reset_to_empty_dir(dest: &Path) -> std::io::Result<()> {
    std::fs::remove_dir_all(dest)?;
    std::fs::create_dir_all(dest)?;
    if std::fs::read_dir(dest)?.next().is_some() {
        return Err(std::io::Error::other("partial seed cleanup uncertain"));
    }
    Ok(())
}

/// Recursively `chown` every entry under (and including) `root` to `uid:gid`.
/// Symlinks are `lchown`'d (never followed) so a cross-tree symlink cannot
/// chown something outside the cloned home. Stops at the first error — any
/// error means "seeding failed", so there is no value continuing.
fn chown_recursive(root: &Path, uid: u32, gid: u32) -> std::io::Result<()> {
    let meta = std::fs::symlink_metadata(root)?;
    if meta.file_type().is_symlink() {
        return std::os::unix::fs::lchown(root, Some(uid), Some(gid));
    }
    std::os::unix::fs::chown(root, Some(uid), Some(gid))?;
    if meta.is_dir() {
        for entry in std::fs::read_dir(root)? {
            chown_recursive(&entry?.path(), uid, gid)?;
        }
    }
    Ok(())
}

/// Post-session: sum bytes used by mount host-paths strictly under the
/// session's effective `home_root`. `None` when the root is empty/invalid
/// (volume driver — caller omits `bytes_used`); `Some(n)` when the local
/// driver is active.
///
/// Bounded: stops at `DU_TIMEOUT` wall time / `DU_DEPTH_CAP` depth, returning
/// whatever accumulated so far — the same #149 discipline every engine call
/// inherits from the runtime client's per-operation deadline.
pub fn measure_home_dirs(mounts: &[String], home_root: &str) -> Option<u64> {
    let root = resolve_root(home_root)?;
    let deadline = Instant::now() + DU_TIMEOUT;
    let mut total: u64 = 0;
    for mount in mounts {
        let Some(host) = host_path_of(mount) else {
            continue;
        };
        if !is_under_root(&root, &host) {
            continue;
        }
        if host.exists() {
            total = total.saturating_add(du(&host, deadline, DU_DEPTH_CAP));
        }
    }
    Some(total)
}

/// Bytes occupied by one directory tree, same bounds as [`measure_home_dirs`]
/// (10s/32 levels). Used by the #500 sweep to log how much a deletion
/// reclaimed; approximate by design.
pub(crate) fn dir_bytes(path: &Path) -> u64 {
    du(path, Instant::now() + DU_TIMEOUT, DU_DEPTH_CAP)
}

/// Bounded directory-size walk: sum of regular-file sizes under `path`,
/// descending at most `depth` levels, stopping at `deadline`. Non-readable
/// entries and symlinks are skipped (a symlink target may lie outside the
/// root; skipping avoids double-counting).
fn du(path: &Path, deadline: Instant, depth: u32) -> u64 {
    if depth == 0 || Instant::now() >= deadline {
        return 0;
    }
    let rd = match std::fs::read_dir(path) {
        Ok(r) => r,
        Err(_) => return 0,
    };
    let mut sum: u64 = 0;
    for entry in rd {
        if Instant::now() >= deadline {
            break;
        }
        let entry = match entry {
            Ok(e) => e,
            Err(_) => continue,
        };
        let meta = match entry.metadata() {
            Ok(m) => m,
            Err(_) => continue,
        };
        if meta.is_dir() {
            sum = sum.saturating_add(du(&entry.path(), deadline, depth - 1));
        } else if meta.is_file() {
            sum = sum.saturating_add(meta.len());
        }
        // symlinks skipped intentionally
    }
    sum
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    #[test]
    fn host_path_of_absolute() {
        assert_eq!(
            host_path_of("/data/homes/u/a:/home/quasar:rw"),
            Some(PathBuf::from("/data/homes/u/a"))
        );
    }

    #[test]
    fn host_path_of_named_volume() {
        assert_eq!(host_path_of("quasar-home-u-a:/home/quasar:rw"), None);
    }

    #[test]
    fn host_path_of_traversal_rejected() {
        assert_eq!(host_path_of("/data/../etc:/home/quasar:rw"), None);
    }

    #[test]
    fn is_under_root_positive() {
        assert!(is_under_root(
            Path::new("/data/homes"),
            Path::new("/data/homes/u/a")
        ));
    }

    #[test]
    fn is_under_root_exact_root_rejected() {
        assert!(!is_under_root(
            Path::new("/data/homes"),
            Path::new("/data/homes")
        ));
    }

    #[test]
    fn is_under_root_sibling_prefix_rejected() {
        assert!(!is_under_root(
            Path::new("/data/homes"),
            Path::new("/data/homes-extra/u/a")
        ));
    }

    #[test]
    fn du_counts_files() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path();
        let mut f = std::fs::File::create(path.join("a.bin")).unwrap();
        f.write_all(&[0u8; 100]).unwrap();
        drop(f);
        std::fs::create_dir(path.join("sub")).unwrap();
        let mut f2 = std::fs::File::create(path.join("sub/b.bin")).unwrap();
        f2.write_all(&[0u8; 200]).unwrap();
        drop(f2);

        let deadline = Instant::now() + Duration::from_secs(10);
        assert_eq!(du(path, deadline, 32), 300);
    }

    #[test]
    fn resolve_root_empty_is_none() {
        assert_eq!(resolve_root(""), None);
        assert_eq!(resolve_root("   "), None);
    }

    #[test]
    fn resolve_root_non_absolute_is_none() {
        assert_eq!(resolve_root("relative/path"), None);
        assert_eq!(resolve_root("  also/relative "), None);
    }

    #[test]
    fn resolve_root_absolute_trimmed() {
        assert_eq!(
            resolve_root("  /data/homes  "),
            Some(PathBuf::from("/data/homes"))
        );
    }

    #[test]
    fn measure_home_dirs_empty_root_is_none() {
        assert_eq!(
            measure_home_dirs(&["/some/path:/home/quasar:rw".to_string()], ""),
            None
        );
    }

    #[test]
    fn measure_home_dirs_uses_effective_root() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        let under = root.join("u/a");
        std::fs::create_dir_all(&under).unwrap();
        let mut f = std::fs::File::create(under.join("save.bin")).unwrap();
        f.write_all(&[0u8; 128]).unwrap();
        drop(f);

        let root_str = root.to_str().unwrap();
        let mounts = vec![
            format!("{}:/home/quasar:rw", under.display()),
            "/outside/the/root:/mnt:rw".to_string(),
        ];
        assert_eq!(measure_home_dirs(&mounts, root_str), Some(128));
        assert_eq!(measure_home_dirs(&mounts, "relative"), None);
    }

    #[test]
    fn du_respects_depth_cap() {
        let dir = tempfile::tempdir().unwrap();
        let nested = dir.path().join("a/b/c");
        std::fs::create_dir_all(&nested).unwrap();
        let mut f = std::fs::File::create(nested.join("deep.bin")).unwrap();
        f.write_all(&[0u8; 50]).unwrap();
        drop(f);

        let deadline = Instant::now() + Duration::from_secs(10);
        // depth=1 only descends one level from dir — should not reach a/b/c.
        assert_eq!(du(dir.path(), deadline, 1), 0);
        // Full depth sees the file.
        assert_eq!(du(dir.path(), deadline, 32), 50);
    }

    // -----------------------------------------------------------------
    // #488 WP3 §3.5: provision_home_dirs seeding
    // -----------------------------------------------------------------

    use crate::session::template::{TemplateCloneMode, TemplateMeta, TEMPLATE_META_SCHEMA};

    /// A published one-file "steam" template. `clone_mode` lets a test force a
    /// deterministic clone failure (`TemplateCloneMode::Off`). Returns the
    /// tempdir (kept alive for the store's lifetime) and the resolved store.
    fn published_store(clone_mode: TemplateCloneMode) -> (tempfile::TempDir, TemplateStore) {
        let base = tempfile::tempdir().unwrap();
        let home_root = base.path().join("homes");
        std::fs::create_dir_all(&home_root).unwrap();
        let store = TemplateStore::resolve(&home_root, None, clone_mode).unwrap();
        let build = store.begin_build("steam", "1.0.0").unwrap();
        std::fs::write(build.home_dir().join("marker.txt"), b"golden").unwrap();
        let meta = TemplateMeta {
            image_id: "steam".to_string(),
            registry_ref: "ghcr.io/accreleus/quasar-steam:1".to_string(),
            version: "1.0.0".to_string(),
            digest: "sha256:deadbeef".to_string(),
            built_at: 1_723_000_000,
            bytes: 6,
            files: 1,
            agent_version: "test".to_string(),
            schema: TEMPLATE_META_SCHEMA,
        };
        store.publish(build, meta).unwrap();
        (base, store)
    }

    /// A `(home_root, leaf)` pair for a fresh scratch destination tree, plus
    /// the one-mount `mounts` vec `provision_home_dirs` expects.
    fn scratch_mount() -> (tempfile::TempDir, PathBuf, Vec<String>) {
        let dest = tempfile::tempdir().unwrap();
        let home_root = dest.path().join("homes");
        let leaf = home_root.join("u/a");
        let mounts = vec![format!("{}:/home/quasar:rw", leaf.display())];
        (dest, leaf, mounts)
    }

    #[test]
    fn seeds_a_cold_home_from_the_template() {
        let (_tpl, store) = published_store(TemplateCloneMode::Copy);
        let seed = store.seed("steam").unwrap();
        let (_dest, leaf, mounts) = scratch_mount();
        let home_root = leaf.parent().unwrap().parent().unwrap().to_path_buf();

        let seeder = TemplateSeeder {
            store: &store,
            seed: &seed,
            authorization: None,
        };
        let outcome = provision_home_dirs_with_result(
            &mounts,
            home_root.to_str().unwrap(),
            Some(seeder),
            "template_unavailable",
        )
        .unwrap();

        assert_eq!(
            outcome,
            Some(HomeSeedOutcome {
                mode: "copy",
                reason: "seeded"
            })
        );

        assert!(
            leaf.join("marker.txt").is_file(),
            "cold home should have been seeded from the template"
        );
    }

    #[test]
    fn warm_home_is_never_touched() {
        let (_tpl, store) = published_store(TemplateCloneMode::Copy);
        let seed = store.seed("steam").unwrap();
        let (_dest, leaf, mounts) = scratch_mount();
        let home_root = leaf.parent().unwrap().parent().unwrap().to_path_buf();
        std::fs::create_dir_all(&leaf).unwrap();
        std::fs::write(leaf.join("registry.vdf"), b"already warm").unwrap();

        let seeder = TemplateSeeder {
            store: &store,
            seed: &seed,
            authorization: None,
        };
        let outcome = provision_home_dirs_with_result(
            &mounts,
            home_root.to_str().unwrap(),
            Some(seeder),
            "template_unavailable",
        )
        .unwrap();

        assert_eq!(
            outcome,
            Some(HomeSeedOutcome {
                mode: "existing",
                reason: "existing_home"
            })
        );

        assert!(leaf.join("registry.vdf").is_file());
        assert_eq!(
            std::fs::read(leaf.join("registry.vdf")).unwrap(),
            b"already warm"
        );
        assert!(
            !leaf.join("marker.txt").exists(),
            "a warm home must never be seeded"
        );
    }

    /// Any existing content — not just a prior seed — disqualifies a leaf.
    #[test]
    fn non_empty_home_is_never_touched() {
        let (_tpl, store) = published_store(TemplateCloneMode::Copy);
        let seed = store.seed("steam").unwrap();
        let (_dest, leaf, mounts) = scratch_mount();
        let home_root = leaf.parent().unwrap().parent().unwrap().to_path_buf();
        std::fs::create_dir_all(&leaf).unwrap();
        std::fs::write(leaf.join("stray_user_file.txt"), b"user wrote this").unwrap();

        let seeder = TemplateSeeder {
            store: &store,
            seed: &seed,
            authorization: None,
        };
        provision_home_dirs(&mounts, home_root.to_str().unwrap(), Some(seeder));

        assert!(leaf.join("stray_user_file.txt").is_file());
        assert!(!leaf.join("marker.txt").exists());
    }

    /// `template: None` covers "feature off", "no template", and "resolution
    /// failed upstream" alike — one code path, byte-identical to
    /// `provision_home_dirs` before #488.
    #[test]
    fn template_absent_behaves_exactly_like_todays_code_path() {
        let (_dest, leaf, mounts) = scratch_mount();
        let home_root = leaf.parent().unwrap().parent().unwrap().to_path_buf();

        let outcome = provision_home_dirs_with_result(
            &mounts,
            home_root.to_str().unwrap(),
            None,
            "template_unavailable",
        )
        .unwrap();

        assert_eq!(
            outcome,
            Some(HomeSeedOutcome {
                mode: "cold",
                reason: "template_unavailable"
            })
        );

        assert!(leaf.is_dir());
        assert_eq!(std::fs::read_dir(&leaf).unwrap().count(), 0);
    }

    /// `TemplateCloneMode::Off` fails `clone_home_into` deterministically,
    /// proving fail-open: never a panic or propagated error, the leaf ends up
    /// like a template-less cold provision.
    #[test]
    fn clone_error_falls_open_to_an_empty_home_never_a_failure() {
        let (_tpl, store) = published_store(TemplateCloneMode::Off);
        let seed = store.seed("steam").unwrap();
        let (_dest, leaf, mounts) = scratch_mount();
        let home_root = leaf.parent().unwrap().parent().unwrap().to_path_buf();

        let seeder = TemplateSeeder {
            store: &store,
            seed: &seed,
            authorization: None,
        };
        let outcome = provision_home_dirs_with_result(
            &mounts,
            home_root.to_str().unwrap(),
            Some(seeder),
            "template_unavailable",
        )
        .unwrap();

        assert_eq!(
            outcome,
            Some(HomeSeedOutcome {
                mode: "cold",
                reason: "clone_failed"
            })
        );

        assert!(leaf.is_dir(), "the session must still get a home directory");
        assert_eq!(
            std::fs::read_dir(&leaf).unwrap().count(),
            0,
            "a failed clone must leave no partial content behind"
        );
    }

    #[test]
    fn two_matching_mounts_seed_nothing() {
        let (_tpl, store) = published_store(TemplateCloneMode::Copy);
        let seed = store.seed("steam").unwrap();
        let dest = tempfile::tempdir().unwrap();
        let home_root = dest.path().join("homes");
        let leaf_a = home_root.join("u/a");
        let leaf_b = home_root.join("u/b");
        let mounts = vec![
            format!("{}:/home/quasar:rw", leaf_a.display()),
            format!("{}:/home/quasar:rw", leaf_b.display()),
        ];

        let seeder = TemplateSeeder {
            store: &store,
            seed: &seed,
            authorization: None,
        };
        let outcome = provision_home_dirs_with_result(
            &mounts,
            home_root.to_str().unwrap(),
            Some(seeder),
            "template_unavailable",
        );

        assert!(
            outcome.is_err(),
            "ambiguous mounts must fail before provisioning"
        );
        assert!(!leaf_a.exists());
        assert!(!leaf_b.exists());
        assert!(
            !leaf_a.join("marker.txt").exists() && !leaf_b.join("marker.txt").exists(),
            "an ambiguous (multi-mount) match must seed nothing"
        );
    }

    #[test]
    fn cold_outcome_refuses_a_writer_after_initial_empty_check() {
        let dir = tempfile::tempdir().unwrap();
        let home = dir.path().join("home");
        std::fs::create_dir(&home).unwrap();
        assert!(std::fs::read_dir(&home).unwrap().next().is_none());
        std::fs::write(home.join("written-during-provision"), b"owned").unwrap();
        assert!(cold_if_empty(&home, "template_unavailable").is_err());
        assert_eq!(
            std::fs::read(home.join("written-during-provision")).unwrap(),
            b"owned"
        );
    }

    #[test]
    fn install_conflict_preserves_existing_content_and_is_not_policy_changed() {
        let (_tpl, store) = published_store(TemplateCloneMode::Copy);
        let seed = store.seed("steam").unwrap();
        let (_dest, leaf, _) = scratch_mount();
        std::fs::create_dir_all(&leaf).unwrap();
        std::fs::write(leaf.join("concurrent-write"), b"owned").unwrap();
        let seeder = TemplateSeeder {
            store: &store,
            seed: &seed,
            authorization: None,
        };
        assert!(seed_home(&seeder, &leaf).is_err());
        assert_eq!(
            std::fs::read(leaf.join("concurrent-write")).unwrap(),
            b"owned"
        );
    }
}
