//! #500 throwaway-home reaping: the agent-side sweep that deletes managed homes
//! belonging to *dev/harness* identities nobody will ever come back for.
//!
//! ## Why this exists next to `gc.rs`
//!
//! [`crate::session::gc`] (#175) only reaps homes the control plane still KNOWS
//! about. A qses/bench run mints an ephemeral identity
//! (`internal/auth.MintEphemeral` → username `agent-<8hex>-<8hex>`), and every
//! managed-home session materialises `<home_root>/<username>/<app-slug>/`. If
//! any link in that chain misses — a non-terminal session blocked the ephemeral
//! reaper, the GC job never scheduled, the home row was never written, or an
//! old reap left the `<username>` parent behind — the directory stays forever.
//! Reached 213 homes / ~10 GB and a 97% root filesystem on the devbox (#500).
//!
//! So this sweep is deliberately NOT database-driven: it keys only on facts the
//! agent can see on its own host — directory NAME, AGE, and whether it is
//! currently MOUNTED.
//!
//! ## What it will and will not delete
//!
//! Collectable ⟺ all of:
//!   1. a direct child of the configured home root whose name matches the
//!      ephemeral-username shape EXACTLY (`agent-` + 8 hex + `-` + 8 hex —
//!      [`is_throwaway_name`]); a real registered account's home is never even a
//!      candidate,
//!   2. nothing under it is mounted by a live container (docker mount sources +
//!      `/proc/mounts`), and
//!   3. its last-use age (the newest mtime in the top two levels) exceeds the
//!      retention window — OR it is EMPTY and older than [`EMPTY_HOME_MIN_AGE`],
//!      since retention protects data and an empty home has none (#92).
//!
//! Never: `templates`, a non-matching name, anything outside the root, or the
//! root itself. A sweep must never fail the agent — every error is logged and
//! the sweep continues.
//!
//! Deletion is rename-then-remove (`<root>/.gc-trash-…`) so a crash mid-remove
//! leaves an inert name the next sweep purges, never a half-deleted home that
//! would look "warm" to the provisioner.

use std::collections::HashSet;
use std::ffi::OsStr;
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

use tracing::{debug, info, warn};

use crate::session::container::ContainerRuntime;
use crate::session::home;

/// Prefix of the temporary name a home is renamed to before removal. Also the
/// marker the next sweep purges, so an interrupted removal self-heals.
const TRASH_PREFIX: &str = ".gc-trash-";

/// Default retention: three days — long enough a weekend soak can still be
/// inspected, short enough a nightly harness cannot fill a disk.
pub const DEFAULT_RETENTION_HOURS: u64 = 72;

/// How often the background sweep runs after the startup pass.
pub const SWEEP_INTERVAL: Duration = Duration::from_secs(24 * 60 * 60);

/// An EMPTY throwaway home is collectable this old whatever the retention window
/// says: retention protects data, and there is none (#92). The floor covers the
/// provisioner's gap between creating `<root>/<user>` and creating the app
/// directory under it — a sweep landing in that gap must not delete a home a
/// session is about to fill.
pub const EMPTY_HOME_MIN_AGE: Duration = Duration::from_secs(3600);

/// Directory names under the home root that must never be considered, whatever
/// else is true (`is_throwaway_name` already rejects them; belt and braces).
const NEVER_TOUCH: &[&str] = &["templates", "lost+found"];

/// Paths a home root may never be — a mis-set `QUASAR_HOME_ROOT` must disable
/// the sweep, not sweep `/var`.
const REFUSED_ROOTS: &[&str] = &[
    "/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/mnt", "/opt", "/proc", "/root",
    "/run", "/sbin", "/srv", "/sys", "/tmp", "/usr", "/var", "/var/lib",
];

/// The knobs, resolved once per sweep from the environment.
///
/// Env-only (not hostcfg-catalog) on purpose: catalog knobs arrive as a
/// per-connection `config_update` overlay, but this sweep is a PROCESS-level
/// timer that runs independently of any control-plane connection — a catalog
/// knob here would silently do nothing. Same shape as `QUASAR_TEMPLATE_*`
/// (`docs/configuration.md`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HomesGcSettings {
    pub root: PathBuf,
    pub retention: Duration,
    pub dry_run: bool,
}

impl HomesGcSettings {
    /// Read the knobs. `None` — the sweep does not run — when the local driver
    /// is not in use (`QUASAR_HOME_ROOT` unset/empty/relative) or when
    /// `QUASAR_HOMES_GC` is explicitly off.
    pub fn from_env() -> Option<Self> {
        if !env_flag("QUASAR_HOMES_GC", true) {
            info!("homes-gc: disabled by QUASAR_HOMES_GC");
            return None;
        }
        let root = home::configured_home_root()?;
        Some(HomesGcSettings {
            root,
            retention: Duration::from_secs(
                env_u64("QUASAR_HOMES_GC_RETENTION_HOURS", DEFAULT_RETENTION_HOURS)
                    .saturating_mul(3600),
            ),
            dry_run: env_flag("QUASAR_HOMES_GC_DRY_RUN", false),
        })
    }
}

/// A knob ON unless explicitly turned off (and vice versa) — `session::env_bool`
/// cannot express a default-ON knob.
fn env_flag(var: &str, default: bool) -> bool {
    match std::env::var(var) {
        Ok(v) => match v.trim().to_ascii_lowercase().as_str() {
            "" => default,
            "1" | "true" | "yes" | "on" => true,
            "0" | "false" | "no" | "off" => false,
            other => {
                warn!(
                    token = "knob-invalid-homes-gc-bool",
                    "homes-gc: {var}={other:?} is not a boolean — using {default}"
                );
                default
            }
        },
        Err(_) => default,
    }
}

fn env_u64(var: &str, default: u64) -> u64 {
    match std::env::var(var) {
        Ok(v) if !v.trim().is_empty() => v.trim().parse().unwrap_or_else(|_| {
            warn!(
                token = "knob-invalid-homes-gc-number",
                "homes-gc: {var}={v:?} is not a number — using {default}"
            );
            default
        }),
        _ => default,
    }
}

/// What one sweep did. Logged as a single summary line and returned for tests.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SweepReport {
    /// Direct children of the root that were looked at.
    pub scanned: usize,
    /// Entries whose name matched the throwaway shape.
    pub candidates: usize,
    /// Homes deleted (or, in dry-run, that WOULD be deleted).
    pub deleted: usize,
    /// Bytes those homes occupied (bounded walk; approximate by design).
    pub bytes: u64,
    /// Candidates skipped because a live container has them mounted.
    pub skipped_live: usize,
    /// Candidates skipped because they are younger than the retention window.
    pub skipped_young: usize,
    /// Of `deleted`, how many were collected early because they were empty.
    pub deleted_empty: usize,
    /// Interrupted removals from a previous sweep that were purged.
    pub trash_purged: usize,
    /// Per-entry failures (each logged at warn).
    pub errors: usize,
}

/// Run one sweep. Blocking; never panics, never returns an error, never
/// touches anything outside `cfg.root`.
pub fn sweep(cfg: &HomesGcSettings, runtime: &ContainerRuntime) -> SweepReport {
    let mut rep = SweepReport::default();
    if let Err(reason) = check_root(&cfg.root) {
        warn!(
            token = "homes-gc-root-refused",
            "homes-gc: refusing to sweep {}: {reason}",
            cfg.root.display()
        );
        return rep;
    }
    let live = live_mount_sources(runtime);
    let now = SystemTime::now();

    let entries = match std::fs::read_dir(&cfg.root) {
        Ok(e) => e,
        Err(e) => {
            warn!(
                token = "homes-gc-root-unreadable",
                "homes-gc: cannot read {}: {e}",
                cfg.root.display()
            );
            return rep;
        }
    };

    for entry in entries.flatten() {
        let name = entry.file_name();
        let name = name.to_string_lossy().to_string();
        let path = entry.path();

        if name.starts_with(TRASH_PREFIX) {
            match std::fs::remove_dir_all(&path) {
                Ok(()) => {
                    rep.trash_purged += 1;
                    info!("homes-gc: purged interrupted removal {}", path.display());
                }
                Err(e) => {
                    rep.errors += 1;
                    warn!(
                        token = "homes-gc-purge-failed",
                        "homes-gc: purging {} failed: {e}",
                        path.display()
                    );
                }
            }
            continue;
        }

        rep.scanned += 1;
        if NEVER_TOUCH.contains(&name.as_str()) || !is_throwaway_name(&name) {
            continue;
        }
        // A symlink is never followed or removed: its target may lie outside root.
        match entry.file_type() {
            Ok(t) if t.is_dir() => {}
            Ok(_) => continue,
            Err(e) => {
                rep.errors += 1;
                warn!(
                    token = "homes-gc-stat-failed",
                    "homes-gc: stat {} failed: {e}",
                    path.display()
                );
                continue;
            }
        }
        rep.candidates += 1;

        if is_live(&path, &live) {
            debug!(
                "homes-gc: {} is mounted by a live container — keeping",
                path.display()
            );
            rep.skipped_live += 1;
            continue;
        }

        let age = last_use_age(&path, now);
        let empty = is_empty_dir(&path);
        if !reclaimable(age, cfg.retention, empty) {
            rep.skipped_young += 1;
            continue;
        }
        if empty && age < cfg.retention {
            rep.deleted_empty += 1;
        }

        let bytes = home::dir_bytes(&path);
        if cfg.dry_run {
            info!(
                "homes-gc: WOULD delete {} ({:.1} MiB, idle {} h)",
                path.display(),
                bytes as f64 / (1024.0 * 1024.0),
                age.as_secs() / 3600
            );
            rep.deleted += 1;
            rep.bytes = rep.bytes.saturating_add(bytes);
            continue;
        }

        match delete_home(&cfg.root, &path) {
            Ok(()) => {
                info!(
                    "homes-gc: deleted {} ({:.1} MiB, idle {} h)",
                    path.display(),
                    bytes as f64 / (1024.0 * 1024.0),
                    age.as_secs() / 3600
                );
                rep.deleted += 1;
                rep.bytes = rep.bytes.saturating_add(bytes);
            }
            Err(e) => {
                rep.errors += 1;
                warn!(
                    token = "homes-gc-delete-failed",
                    "homes-gc: deleting {} failed: {e}",
                    path.display()
                );
            }
        }
    }

    info!(
        "homes-gc: sweep{} of {} — scanned {}, candidates {}, deleted {} ({:.1} MiB, \
         {} empty), live {}, too-young {}, purged {}, errors {}",
        if cfg.dry_run { " (DRY RUN)" } else { "" },
        cfg.root.display(),
        rep.scanned,
        rep.candidates,
        rep.deleted,
        rep.bytes as f64 / (1024.0 * 1024.0),
        rep.deleted_empty,
        rep.skipped_live,
        rep.skipped_young,
        rep.trash_purged,
        rep.errors,
    );
    rep
}

/// Spawn the process-level sweeper: one pass now, then one every 24 h. A no-op
/// when the knobs say so (no local driver, or `QUASAR_HOMES_GC=0`).
///
/// A blocking thread, not a tokio task: parking a runtime worker on this
/// filesystem walk + docker subprocess would starve the heartbeat.
pub fn spawn_sweeper() {
    let Some(cfg) = HomesGcSettings::from_env() else {
        return;
    };
    info!(
        "homes-gc: armed for {} (retention {} h, dry_run {}, every {} h)",
        cfg.root.display(),
        cfg.retention.as_secs() / 3600,
        cfg.dry_run,
        SWEEP_INTERVAL.as_secs() / 3600,
    );
    if let Err(e) = std::thread::Builder::new()
        .name("quasar-homes-gc".into())
        .spawn(move || loop {
            sweep(&cfg, &ContainerRuntime::from_env());
            std::thread::sleep(SWEEP_INTERVAL);
        })
    {
        warn!(
            token = "homes-gc-spawn-failed",
            "homes-gc: could not spawn the sweeper thread: {e}"
        );
    }
}

/// One-shot sweep for the `homes-gc` CLI subcommand (the `make homes-gc` DX
/// verb). Returns the report so the caller can set an exit code.
pub fn run_once(dry_run_override: bool) -> Option<SweepReport> {
    let mut cfg = HomesGcSettings::from_env()?;
    if dry_run_override {
        cfg.dry_run = true;
    }
    Some(sweep(&cfg, &ContainerRuntime::from_env()))
}

// ── the three predicates ────────────────────────────────────────────────────

/// Does this directory name have the EXACT shape of an ephemeral (dev/harness)
/// username — `agent-<8 hex>-<8 hex>`, as minted by the control plane's
/// `MintEphemeral` (`internal/auth/ephemeral.go`)?
///
/// Deliberately exact, not an `agent-*` glob: a real account's home must never
/// match. (A user who names themselves `agent-0bdc5920-fc5182ea` would collide
/// — documented in `docs/configuration.md`.)
pub fn is_throwaway_name(name: &str) -> bool {
    let Some(rest) = name.strip_prefix("agent-") else {
        return false;
    };
    let mut parts = rest.split('-');
    let (Some(a), Some(b), None) = (parts.next(), parts.next(), parts.next()) else {
        return false;
    };
    a.len() == 8
        && b.len() == 8
        && a.bytes().all(|c| c.is_ascii_hexdigit())
        && b.bytes().all(|c| c.is_ascii_hexdigit())
}

/// May a candidate of this age be collected? Retention protects DATA; an empty
/// home has none, so it waits out only [`EMPTY_HOME_MIN_AGE`] (#92). Pure, so
/// the rule is testable without backdating a directory's mtime.
pub fn reclaimable(age: Duration, retention: Duration, empty: bool) -> bool {
    age >= retention || (empty && age >= EMPTY_HOME_MIN_AGE)
}

/// Has this home nothing left in it? An unreadable directory is NOT empty — the
/// sweep must not delete what it cannot inspect.
pub fn is_empty_dir(dir: &Path) -> bool {
    std::fs::read_dir(dir)
        .map(|mut rd| rd.next().is_none())
        .unwrap_or(false)
}

/// Is `dir` — or anything under it — one of the mount sources currently in use?
pub fn is_live(dir: &Path, live: &HashSet<PathBuf>) -> bool {
    live.iter().any(|src| src == dir || src.starts_with(dir))
}

/// Age since the home was last touched: the newest mtime in the top two levels
/// (the home dir, its per-app children, and their entries).
///
/// Two levels, not one: the home dir's own mtime only moves when an app
/// subdirectory is created/removed, so a month-long session writing inside
/// `<home>/<app>/` would otherwise look untouched. Two levels, not a full walk:
/// this runs over every candidate on the host, and a deeper write always bumps
/// the parents' mtimes we do read.
pub fn last_use_age(dir: &Path, now: SystemTime) -> Duration {
    let mut newest = mtime(dir);
    if let Ok(rd) = std::fs::read_dir(dir) {
        for child in rd.flatten() {
            let cp = child.path();
            newest = newest.max(mtime(&cp));
            if child.file_type().map(|t| t.is_dir()).unwrap_or(false) {
                if let Ok(inner) = std::fs::read_dir(&cp) {
                    for g in inner.flatten() {
                        newest = newest.max(mtime(&g.path()));
                    }
                }
            }
        }
    }
    now.duration_since(newest).unwrap_or_default()
}

fn mtime(p: &Path) -> SystemTime {
    std::fs::symlink_metadata(p)
        .and_then(|m| m.modified())
        .unwrap_or(SystemTime::UNIX_EPOCH)
}

// ── guards ──────────────────────────────────────────────────────────────────

/// Does this look like OUR home root? Absolute, a real directory (not a
/// symlink), at least two path components deep, and not a system directory.
pub fn check_root(root: &Path) -> Result<(), String> {
    if !root.is_absolute() {
        return Err("home root is not absolute".into());
    }
    if root.components().any(|c| c.as_os_str() == OsStr::new("..")) {
        return Err("home root contains ..".into());
    }
    let depth = root
        .components()
        .filter(|c| matches!(c, std::path::Component::Normal(_)))
        .count();
    if depth < 2 {
        return Err(format!(
            "home root {} is too shallow to be a managed-home root",
            root.display()
        ));
    }
    if REFUSED_ROOTS.iter().any(|r| Path::new(r) == root) {
        return Err(format!("{} is a system directory", root.display()));
    }
    match std::fs::symlink_metadata(root) {
        Ok(m) if m.is_dir() => Ok(()),
        Ok(_) => Err("home root is not a directory".into()),
        Err(e) => Err(format!("home root is unreadable: {e}")),
    }
}

/// Rename the home to an inert `.gc-trash-*` sibling INSIDE the root, then
/// remove the tree. A crash between the two leaves a name the next sweep
/// purges — never a half-emptied home the provisioner would treat as warm.
fn delete_home(root: &Path, path: &Path) -> std::io::Result<()> {
    let stamp = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or_default();
    let leaf = path
        .file_name()
        .map(|s| s.to_string_lossy().to_string())
        .unwrap_or_else(|| "home".to_string());
    let trash = root.join(format!("{TRASH_PREFIX}{leaf}-{stamp}"));
    match std::fs::rename(path, &trash) {
        Ok(()) => std::fs::remove_dir_all(&trash),
        // Cross-device or a racing rename: fall back to removing in place. The
        // path confinement above still holds.
        Err(e) => {
            warn!(
                token = "homes-gc-rename-failed",
                "homes-gc: rename {} -> {} failed ({e}) — removing in place",
                path.display(),
                trash.display()
            );
            std::fs::remove_dir_all(path)
        }
    }
}

// ── liveness ────────────────────────────────────────────────────────────────

/// Every host path currently mounted into a running container, plus this
/// process's `/proc/mounts`. Best-effort: a failure yields an EMPTY set only
/// for that source; the age gate still protects a home in use.
fn live_mount_sources(runtime: &ContainerRuntime) -> HashSet<PathBuf> {
    let mut set = HashSet::new();
    match runtime.run_raw(&["ps", "-q"]) {
        Ok(ids) => {
            let ids: Vec<String> = ids
                .lines()
                .map(str::trim)
                .filter(|l| !l.is_empty())
                .map(String::from)
                .collect();
            if !ids.is_empty() {
                let mut args: Vec<&str> = vec![
                    "inspect",
                    "--format",
                    "{{range .Mounts}}{{println .Source}}{{end}}",
                ];
                args.extend(ids.iter().map(String::as_str));
                match runtime.run_raw(&args) {
                    Ok(out) => set.extend(parse_paths(&out)),
                    Err(e) => warn!(
                        token = "homes-gc-inspect-failed",
                        "homes-gc: docker inspect for live mounts failed: {e}"
                    ),
                }
            }
        }
        Err(e) => warn!(
            token = "homes-gc-ps-failed",
            "homes-gc: docker ps for live mounts failed: {e}"
        ),
    }
    if let Ok(mounts) = std::fs::read_to_string("/proc/mounts") {
        set.extend(parse_proc_mounts(&mounts));
    }
    set
}

/// Absolute paths, one per line, ignoring blanks.
fn parse_paths(out: &str) -> Vec<PathBuf> {
    out.lines()
        .map(str::trim)
        .filter(|l| l.starts_with('/'))
        .map(PathBuf::from)
        .collect()
}

/// The mount POINTS (field 2) of `/proc/mounts`. A bind mount of a home into
/// this process's own namespace shows up here even when docker cannot be asked.
fn parse_proc_mounts(text: &str) -> Vec<PathBuf> {
    text.lines()
        .filter_map(|l| l.split_whitespace().nth(1))
        .filter(|p| p.starts_with('/'))
        // /proc/mounts octal-escapes spaces; only that one matters in practice.
        .map(|p| PathBuf::from(p.replace("\\040", " ")))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    #[test]
    fn throwaway_names_are_matched_exactly() {
        assert!(is_throwaway_name("agent-0bdc5920-fc5182ea"));
        assert!(is_throwaway_name("agent-13d96fa5-8dff9894"));
        // Real users and near-misses that must NOT be collected.
        for name in [
            "admin",
            "salty2011",
            "agent",
            "agent-",
            "agent-smith",
            "agent-0bdc5920",                // one group
            "agent-0bdc5920-fc5182ea-extra", // three groups
            "agent-0bdc592-fc5182ea",        // 7 hex
            "agent-0bdc59200-fc5182ea",      // 9 hex
            "agent-0bdc5920-zzzzzzzz",       // not hex
            "templates",
            "Agent-0bdc5920-fc5182ea", // case-sensitive prefix
        ] {
            assert!(!is_throwaway_name(name), "{name} must not be collectable");
        }
    }

    #[test]
    fn a_mounted_home_is_live_even_when_the_mount_is_the_app_leaf() {
        let dir = PathBuf::from("/var/lib/quasar/homes/agent-0bdc5920-fc5182ea");
        let live: HashSet<PathBuf> = ["/var/lib/quasar/homes/agent-0bdc5920-fc5182ea/kde-desktop"]
            .iter()
            .map(PathBuf::from)
            .collect();
        assert!(is_live(&dir, &live));
        // A sibling home, and a prefix-of-name path, must not match.
        assert!(!is_live(
            &PathBuf::from("/var/lib/quasar/homes/agent-13d96fa5-8dff9894"),
            &live
        ));
        assert!(!is_live(
            &PathBuf::from("/var/lib/quasar/homes/agent-0bdc5920-fc5182eb"),
            &live
        ));
    }

    #[test]
    fn last_use_age_sees_a_write_two_levels_down() {
        let tmp = tempfile::tempdir().unwrap();
        let home = tmp.path().join("agent-0bdc5920-fc5182ea");
        fs::create_dir_all(home.join("kde-desktop")).unwrap();
        fs::write(home.join("kde-desktop/state"), b"x").unwrap();
        assert!(last_use_age(&home, SystemTime::now()) < Duration::from_secs(60));
        // A clock far in the future exercises the same arithmetic an aged home
        // would, since the gate is a comparison against `now`.
        let future = SystemTime::now() + Duration::from_secs(100 * 3600);
        assert!(last_use_age(&home, future) > Duration::from_secs(99 * 3600));
    }

    #[test]
    fn check_root_refuses_shallow_and_system_paths() {
        for bad in ["/", "/var", "/usr", "/tmp"] {
            assert!(check_root(Path::new(bad)).is_err(), "{bad} must be refused");
        }
        assert!(check_root(Path::new("relative/homes")).is_err());
        assert!(check_root(Path::new("/var/lib/../lib/quasar/homes")).is_err());
        // A real, deep enough directory passes.
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("var/lib/quasar/homes");
        fs::create_dir_all(&root).unwrap();
        assert!(check_root(&root).is_ok());
        // A file is not a root.
        let f = tmp.path().join("var/lib/quasar/file");
        fs::write(&f, b"x").unwrap();
        assert!(check_root(&f).is_err());
    }

    /// The whole selection rule end to end: only aged throwaway homes go.
    #[test]
    fn sweep_deletes_only_aged_unmounted_throwaways() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("var/lib/quasar/homes");
        fs::create_dir_all(&root).unwrap();

        let mk = |name: &str| {
            let p = root.join(name);
            fs::create_dir_all(p.join("kde-desktop")).unwrap();
            fs::write(p.join("kde-desktop/state"), b"payload").unwrap();
            p
        };
        let old_a = mk("agent-0bdc5920-fc5182ea");
        let old_b = mk("agent-13d96fa5-8dff9894");
        let real_user = mk("salty2011");
        let admin = mk("admin");
        let templates = mk("templates");
        let trash = root.join(format!("{TRASH_PREFIX}agent-2b4851f0-1aea19c5-123"));
        fs::create_dir_all(&trash).unwrap();

        // Zero retention makes the just-created entries "aged" — the age
        // arithmetic itself is `last_use_age_sees_a_write_two_levels_down`.
        let cfg = HomesGcSettings {
            root: root.clone(),
            retention: Duration::ZERO,
            dry_run: false,
        };
        let rep = sweep(&cfg, &ContainerRuntime::from_env());

        assert!(!old_a.exists(), "an aged throwaway home must be deleted");
        assert!(!old_b.exists());
        assert!(real_user.exists(), "a real user's home must survive");
        assert!(admin.exists());
        assert!(templates.exists(), "templates must never be touched");
        assert!(!trash.exists(), "an interrupted removal must be purged");
        assert_eq!(rep.deleted, 2);
        assert_eq!(rep.candidates, 2);
        assert_eq!(rep.trash_purged, 1);
        assert!(rep.bytes > 0);
        assert_eq!(rep.errors, 0);
    }

    #[test]
    fn a_young_home_survives_and_dry_run_deletes_nothing() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("var/lib/quasar/homes");
        let home = root.join("agent-0bdc5920-fc5182ea");
        fs::create_dir_all(home.join("kde-desktop")).unwrap();
        fs::write(home.join("kde-desktop/state"), b"payload").unwrap();

        let cfg = HomesGcSettings {
            root: root.clone(),
            retention: Duration::from_secs(72 * 3600),
            dry_run: false,
        };
        let rep = sweep(&cfg, &ContainerRuntime::from_env());
        assert!(home.exists());
        assert_eq!(rep.skipped_young, 1);
        assert_eq!(rep.deleted, 0);

        // Aged (retention 0) but dry-run: reported, not removed.
        let cfg = HomesGcSettings {
            retention: Duration::ZERO,
            dry_run: true,
            ..cfg
        };
        let rep = sweep(&cfg, &ContainerRuntime::from_env());
        assert!(home.exists(), "a dry run must never delete");
        assert_eq!(rep.deleted, 1);
    }

    /// A live-mounted home is kept past retention — this is what makes a sweep
    /// safe to run mid-session.
    #[test]
    fn a_live_home_is_kept_past_retention() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("var/lib/quasar/homes");
        let home = root.join("agent-0bdc5920-fc5182ea");
        fs::create_dir_all(home.join("kde-desktop")).unwrap();

        let live: HashSet<PathBuf> = [home.join("kde-desktop")].into_iter().collect();
        assert!(is_live(&home, &live));
        // The deletion path is reached only for a non-live candidate: with no
        // live set, retention 0 collects the same tree.
        let cfg = HomesGcSettings {
            root,
            retention: Duration::ZERO,
            dry_run: true,
        };
        assert_eq!(sweep(&cfg, &ContainerRuntime::from_env()).deleted, 1);
    }

    #[test]
    fn sweep_refuses_a_system_root() {
        let cfg = HomesGcSettings {
            root: PathBuf::from("/var"),
            retention: Duration::ZERO,
            dry_run: false,
        };
        assert_eq!(
            sweep(&cfg, &ContainerRuntime::from_env()),
            SweepReport::default()
        );
    }

    #[test]
    fn proc_mounts_points_are_parsed() {
        let text = "/dev/sda1 / ext4 rw 0 0\n\
                    tmpfs /run/user/1000 tmpfs rw 0 0\n\
                    /dev/sdb /var/lib/quasar/homes/agent-0bdc5920-fc5182ea ext4 rw 0 0\n";
        let got = parse_proc_mounts(text);
        assert!(got.contains(&PathBuf::from(
            "/var/lib/quasar/homes/agent-0bdc5920-fc5182ea"
        )));
        assert!(got.contains(&PathBuf::from("/")));
    }

    #[test]
    fn docker_mount_sources_are_parsed() {
        let out = "/var/lib/quasar/homes/agent-0bdc5920-fc5182ea/kde-desktop\n\
                   \n/run/quasar-agent\nnamed-volume\n";
        assert_eq!(
            parse_paths(out),
            vec![
                PathBuf::from("/var/lib/quasar/homes/agent-0bdc5920-fc5182ea/kde-desktop"),
                PathBuf::from("/run/quasar-agent"),
            ]
        );
    }

    /// #92: retention protects data. An empty throwaway home has none, so it
    /// waits out only the anti-race floor.
    #[test]
    fn an_empty_home_is_collectable_before_the_retention_window() {
        let retention = Duration::from_secs(72 * 3600);
        // Not empty: only retention lets it go.
        assert!(!reclaimable(Duration::from_secs(2 * 3600), retention, false));
        assert!(reclaimable(retention, retention, false));
        // Empty: past the floor is enough, below it is not.
        assert!(reclaimable(EMPTY_HOME_MIN_AGE, retention, true));
        assert!(reclaimable(
            EMPTY_HOME_MIN_AGE + Duration::from_secs(1),
            retention,
            true
        ));
        assert!(
            !reclaimable(EMPTY_HOME_MIN_AGE - Duration::from_secs(1), retention, true),
            "a just-created home must survive the provisioner race"
        );
    }

    #[test]
    fn emptiness_is_read_from_the_directory_and_unreadable_is_not_empty() {
        let tmp = tempfile::tempdir().unwrap();
        let empty = tmp.path().join("agent-0bdc5920-fc5182ea");
        fs::create_dir_all(&empty).unwrap();
        assert!(is_empty_dir(&empty));

        let full = tmp.path().join("agent-13d96fa5-8dff9894");
        fs::create_dir_all(full.join("kde-desktop")).unwrap();
        assert!(!is_empty_dir(&full));

        // Absent (hence unreadable) is NOT empty: never delete what cannot be
        // inspected.
        assert!(!is_empty_dir(&tmp.path().join("agent-2b4851f0-1aea19c5")));
    }

    /// The whole rule through `sweep`: an emptied throwaway home backdated past
    /// the floor goes even though retention is 72 h, while a populated one of
    /// the same age stays.
    #[test]
    fn sweep_collects_an_aged_empty_home_inside_the_retention_window() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("var/lib/quasar/homes");
        fs::create_dir_all(&root).unwrap();

        let emptied = root.join("agent-0bdc5920-fc5182ea");
        fs::create_dir_all(&emptied).unwrap();
        let populated = root.join("agent-13d96fa5-8dff9894");
        fs::create_dir_all(populated.join("kde-desktop")).unwrap();
        fs::write(populated.join("kde-desktop/state"), b"payload").unwrap();

        let old = SystemTime::now() - Duration::from_secs(4 * 3600);
        let times = std::fs::FileTimes::new().set_accessed(old).set_modified(old);
        for p in [&emptied, &populated, &populated.join("kde-desktop")] {
            std::fs::File::open(p).unwrap().set_times(times).unwrap();
        }

        let cfg = HomesGcSettings {
            root,
            retention: Duration::from_secs(72 * 3600),
            dry_run: false,
        };
        let rep = sweep(&cfg, &ContainerRuntime::from_env());
        assert!(!emptied.exists(), "an aged empty home must be collected");
        assert!(populated.exists(), "a home holding data must wait out retention");
        assert_eq!(rep.deleted, 1);
        assert_eq!(rep.deleted_empty, 1);
        assert_eq!(rep.skipped_young, 1);
        assert_eq!(rep.errors, 0);
    }

    #[test]
    fn env_flag_defaults_and_overrides() {
        // Serial within this test: env is process-global.
        std::env::remove_var("QUASAR_TEST_FLAG");
        assert!(env_flag("QUASAR_TEST_FLAG", true));
        assert!(!env_flag("QUASAR_TEST_FLAG", false));
        std::env::set_var("QUASAR_TEST_FLAG", "0");
        assert!(!env_flag("QUASAR_TEST_FLAG", true));
        std::env::set_var("QUASAR_TEST_FLAG", "yes");
        assert!(env_flag("QUASAR_TEST_FLAG", false));
        std::env::set_var("QUASAR_TEST_FLAG", "banana");
        assert!(env_flag("QUASAR_TEST_FLAG", true));
        std::env::remove_var("QUASAR_TEST_FLAG");
    }
}
