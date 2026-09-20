//! Storage readiness (#253): whether managed homes can really be written and whether the
//! storage they live on is running out. Facts are gathered once per probe in
//! [`StorageView::live`]; the verdicts are the `check_*` functions, pure over the view
//! except for [`write_probe`], which performs the one real write test.
//!
//! The write test mirrors the launch path: the agent (root) creates a home leaf under the
//! homes root, the leaf is handed to the app identity (`QUASAR_APP_PUID`/`PGID`), and the
//! app identity writes into it. It never opens an existing entry: the leaf name is unique
//! and created with `create_dir` (EEXIST is inconclusive, never a reuse), and the leaf is
//! removed on every exit path, including a failure part-way.

use std::path::{Path, PathBuf};

use crate::messages::{ReadinessBlocks, ReadinessCheck};

/// `QUASAR_HOMES_FREE_SPACE_FLOOR_GIB`: below this much free space the homes,
/// template and image storage checks warn. Homes storage fails only when exhausted.
pub const FREE_FLOOR_ENV: &str = "QUASAR_HOMES_FREE_SPACE_FLOOR_GIB";
pub const DEFAULT_FREE_FLOOR_GIB: u64 = 5;
pub const GIB: u64 = 1 << 30;

pub const HOMES_WRITABLE_ID: &str = "homes_root_writable";
pub const HOMES_FREE_SPACE_ID: &str = "homes_free_space";
pub const TEMPLATE_FREE_SPACE_ID: &str = "template_free_space";
pub const IMAGE_FREE_SPACE_ID: &str = "image_free_space";

/// What `statvfs` said about a root. `available_bytes` is what an unprivileged writer
/// gets (`f_bavail`), which is the number a home write runs out of.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SpaceFacts {
    pub available_bytes: u64,
    pub total_bytes: u64,
}

/// A configured storage root and, when it could be read, its free space.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StorageRoot {
    pub path: PathBuf,
    pub space: Option<SpaceFacts>,
}

/// The storage facts one probe sees. `None` roots are not configured on this host.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StorageView {
    /// `QUASAR_HOME_ROOT`.
    pub homes: Option<StorageRoot>,
    /// `QUASAR_TEMPLATE_ROOT` or its default sibling of the homes root.
    pub templates: Option<StorageRoot>,
    /// The container engine's data root, where it is visible to this agent.
    pub images: Option<StorageRoot>,
    /// The warn floor, in bytes, from [`FREE_FLOOR_ENV`].
    pub free_floor_bytes: u64,
}

impl Default for StorageView {
    fn default() -> Self {
        StorageView {
            homes: None,
            templates: None,
            images: None,
            free_floor_bytes: DEFAULT_FREE_FLOOR_GIB * GIB,
        }
    }
}

impl StorageView {
    /// Production facts: the configured roots, `statvfs` on each, the floor from the
    /// environment. `engine_answered` is false when this refresh already established that
    /// the container engine is unusable (#274): the image root is only discoverable by
    /// asking the engine, which would spend its own full deadline to return the same
    /// `None` this skips straight to.
    pub fn live(engine_answered: bool) -> Self {
        let homes = crate::session::home::configured_home_root().map(|path| StorageRoot {
            space: space_facts(&path),
            path,
        });
        let templates = homes.as_ref().map(|h| {
            let path = super::template_root_for(&h.path);
            StorageRoot {
                space: space_facts(&path),
                path,
            }
        });
        let images = engine_answered
            .then(crate::images::disk::engine_root_visible_to_agent)
            .flatten()
            .map(|path| StorageRoot {
                space: space_facts(&path),
                path,
            });
        StorageView {
            homes,
            templates,
            images,
            free_floor_bytes: parse_free_floor_gib(std::env::var(FREE_FLOOR_ENV).ok().as_deref()),
        }
    }
}

/// `statvfs` on `path`. `None` when the path is missing or the call fails — the caller
/// then warns rather than trusting a number that was never read.
fn space_facts(path: &Path) -> Option<SpaceFacts> {
    if !path.exists() {
        return None;
    }
    let c_path = std::ffi::CString::new(path.to_str()?.as_bytes()).ok()?;
    let mut vfs: libc::statvfs = unsafe { std::mem::zeroed() };
    // SAFETY: `c_path` outlives the call; `vfs` is a plain-data out-param.
    let rc = unsafe { libc::statvfs(c_path.as_ptr(), &mut vfs) };
    if rc != 0 {
        return None;
    }
    Some(SpaceFacts {
        available_bytes: vfs.f_bavail.saturating_mul(vfs.f_frsize),
        total_bytes: vfs.f_blocks.saturating_mul(vfs.f_frsize),
    })
}

/// The floor in bytes. Unset, empty, unparsable or zero ⇒ the default: a typo must never
/// silence the warning.
pub fn parse_free_floor_gib(raw: Option<&str>) -> u64 {
    let gib = raw
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .and_then(|s| s.parse::<u64>().ok())
        .filter(|&n| n != 0)
        .unwrap_or(DEFAULT_FREE_FLOOR_GIB);
    gib.saturating_mul(GIB)
}

/// Who the write test writes as.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum WriteIdentity {
    /// `QUASAR_APP_PUID`/`PGID`: the leaf is handed to this identity and written as it.
    App { uid: u32, gid: u32 },
    /// No app identity configured: written as the agent's own identity.
    Agent,
}

impl WriteIdentity {
    pub fn from_env_pair(app_uid: Option<u32>, app_gid: Option<u32>) -> Self {
        match app_uid {
            Some(uid) => WriteIdentity::App {
                uid,
                gid: app_gid.unwrap_or(uid),
            },
            None => WriteIdentity::Agent,
        }
    }
}

/// Where the write test was when it stopped.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum WriteStage {
    CreateLeaf,
    AssignOwner,
    AssumeIdentity,
    WriteFile,
    Cleanup,
}

/// The outcome of one write test.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WriteOutcome {
    /// Created, handed over, written and removed.
    Written { as_identity: WriteIdentity },
    /// Definitive: permission denied, read-only filesystem, or the root is missing.
    Unwritable { stage: WriteStage, reason: String },
    /// Definitive: no space or quota left.
    Exhausted { stage: WriteStage, reason: String },
    /// Could not be concluded (any other error, or the identity could not be assumed);
    /// the verdict warns with the reason.
    Inconclusive { stage: WriteStage, reason: String },
}

/// Classifies an I/O error by its OS errno. `reason` is `error.to_string()` — it carries
/// the OS text ("Permission denied (os error 13)") that verdicts and tests key on.
fn classify_io_error(error: &std::io::Error, stage: WriteStage) -> WriteOutcome {
    let reason = error.to_string();
    match error.raw_os_error() {
        Some(libc::EACCES) | Some(libc::EPERM) | Some(libc::EROFS) | Some(libc::ENOENT) => {
            WriteOutcome::Unwritable { stage, reason }
        }
        Some(libc::ENOSPC) | Some(libc::EDQUOT) => WriteOutcome::Exhausted { stage, reason },
        // Includes ENOTDIR/EEXIST/EIO and no-errno cases: none of them are a definitive
        // permission or space verdict.
        _ => WriteOutcome::Inconclusive { stage, reason },
    }
}

/// Restores the thread's fs identity and removes the leaf. Armed the moment the leaf
/// exists so every exit path — including a panic unwinding through `write_probe` — cleans
/// up. `finish` runs it once and reports a failed removal instead of swallowing it;
/// the `Drop` impl is the panic-safety net and discards the result (the thread is already
/// unwinding).
struct CleanupGuard {
    leaf: PathBuf,
    restore_fsid: Option<(u32, u32)>,
    done: bool,
}

impl CleanupGuard {
    fn armed(leaf: PathBuf) -> Self {
        CleanupGuard {
            leaf,
            restore_fsid: None,
            done: false,
        }
    }

    fn run(&mut self) -> Result<(), String> {
        if self.done {
            return Ok(());
        }
        self.done = true;
        if let Some((fsuid, fsgid)) = self.restore_fsid.take() {
            // SAFETY: setfsuid/setfsgid affect only the calling thread, which this guard
            // is scoped to and which dies at the end of `write_probe`'s spawned thread.
            unsafe {
                libc::setfsuid(fsuid);
                libc::setfsgid(fsgid);
            }
        }
        std::fs::remove_dir_all(&self.leaf)
            .map_err(|e| format!("could not remove {}: {e}", self.leaf.display()))
    }

    fn finish(mut self) -> Result<(), String> {
        self.run()
    }
}

impl Drop for CleanupGuard {
    fn drop(&mut self) {
        let _ = self.run();
    }
}

/// Step 4: assume `identity` on the calling thread for the write, recording in `guard`
/// what to restore. Only ever called with the leaf already owned by `guard`.
fn assume_identity(guard: &mut CleanupGuard, identity: WriteIdentity) -> Result<(), WriteOutcome> {
    let WriteIdentity::App { uid, gid } = identity else {
        return Ok(());
    };
    // SAFETY: geteuid has no preconditions.
    let euid = unsafe { libc::geteuid() };
    if euid == uid {
        return Ok(());
    }
    if euid != 0 {
        return Err(WriteOutcome::Inconclusive {
            stage: WriteStage::AssumeIdentity,
            reason: format!("the agent runs as uid {euid} and cannot write as uid {uid}"),
        });
    }
    // SAFETY: setfsgid/setfsuid affect only the calling thread.
    let saved_fsgid = unsafe { libc::setfsgid(gid) } as u32;
    let saved_fsuid = unsafe { libc::setfsuid(uid) } as u32;
    guard.restore_fsid = Some((saved_fsuid, saved_fsgid));
    // SAFETY: same as above; called again purely to read back the previous (now current) fsuid.
    let check = unsafe { libc::setfsuid(uid) } as u32;
    if check != uid {
        return Err(WriteOutcome::Inconclusive {
            stage: WriteStage::AssumeIdentity,
            reason: format!("could not assume uid {uid}"),
        });
    }
    Ok(())
}

/// Runs on the dedicated thread [`write_probe`] spawns: create the leaf, hand it to
/// `identity`, assume that identity, write, and always clean up before returning.
fn write_probe_on_thread(
    root: &Path,
    identity: WriteIdentity,
    payload: impl FnOnce(&mut std::fs::File) -> std::io::Result<()>,
) -> WriteOutcome {
    use std::os::unix::fs::{chown, DirBuilderExt, OpenOptionsExt};

    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or_default();
    let leaf = root.join(format!(
        ".quasar-readiness-write-{}-{nanos}",
        std::process::id()
    ));

    // Never create_dir_all, never reuse an existing path: EEXIST must stay inconclusive.
    if let Err(e) = std::fs::DirBuilder::new().mode(0o700).create(&leaf) {
        return classify_io_error(&e, WriteStage::CreateLeaf);
    }

    let mut guard = CleanupGuard::armed(leaf.clone());

    let result: Result<WriteIdentity, WriteOutcome> = (|| {
        if let WriteIdentity::App { uid, gid } = identity {
            chown(&leaf, Some(uid), Some(gid)).map_err(|e| WriteOutcome::Inconclusive {
                stage: WriteStage::AssignOwner,
                reason: e.to_string(),
            })?;
        }
        assume_identity(&mut guard, identity)?;
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(leaf.join("write-test"))
            .map_err(|e| classify_io_error(&e, WriteStage::WriteFile))?;
        payload(&mut file).map_err(|e| classify_io_error(&e, WriteStage::WriteFile))?;
        drop(file);
        Ok(identity)
    })();

    match guard.finish() {
        Ok(()) => match result {
            Ok(as_identity) => WriteOutcome::Written { as_identity },
            Err(outcome) => outcome,
        },
        // Residue must be visible, never silent: cleanup failure wins over any other outcome.
        Err(reason) => WriteOutcome::Inconclusive {
            stage: WriteStage::Cleanup,
            reason,
        },
    }
}

/// The real write test. `payload` writes the probe file's contents (production writes a
/// few bytes and syncs); it is a parameter so a failure after the leaf exists can be
/// exercised. Whatever happens, the leaf is gone when this returns.
///
/// Runs on a dedicated thread, joined before returning: step 4 changes the CALLING
/// thread's fs identity, and that change must die with the thread rather than leak onto
/// whichever thread happens to run the next probe.
pub fn write_probe(
    root: &Path,
    identity: WriteIdentity,
    payload: impl FnOnce(&mut std::fs::File) -> std::io::Result<()> + Send,
) -> WriteOutcome {
    let root = root.to_path_buf();
    std::thread::scope(|scope| {
        scope
            .spawn(move || write_probe_on_thread(&root, identity, payload))
            .join()
            .unwrap_or_else(|_| WriteOutcome::Inconclusive {
                stage: WriteStage::WriteFile,
                reason: "the write test thread panicked".to_string(),
            })
    })
}

/// One decimal, trimmed: `5 GiB`, `1.2 GiB`, `50 GiB`.
fn gib(bytes: u64) -> String {
    let value = bytes as f64 / GIB as f64;
    let mut s = format!("{value:.1}");
    if s.ends_with(".0") {
        s.truncate(s.len() - 2);
    }
    format!("{s} GiB")
}

const HOMES_NOT_CONFIGURED: &str =
    "No homes root is configured (QUASAR_HOME_ROOT), so managed homes are not used on this host";

fn stage_words(stage: WriteStage) -> &'static str {
    match stage {
        WriteStage::CreateLeaf => "creating the test home",
        WriteStage::AssignOwner => "handing it to the app identity",
        WriteStage::AssumeIdentity => "assuming the app identity",
        WriteStage::WriteFile => "writing the test file",
        WriteStage::Cleanup => "removing the test home",
    }
}

pub fn check_homes_root_writable(view: &StorageView, identity: WriteIdentity) -> ReadinessCheck {
    check_homes_root_writable_inner(view, identity)
        .with_blocks(ReadinessBlocks::homes("control_plane"))
}

fn check_homes_root_writable_inner(view: &StorageView, identity: WriteIdentity) -> ReadinessCheck {
    let Some(root) = &view.homes else {
        return super::skip(HOMES_WRITABLE_ID, HOMES_NOT_CONFIGURED);
    };
    let path = root.path.display().to_string();
    let outcome = write_probe(&root.path, identity, |file| {
        use std::io::Write as _;
        file.write_all(b"quasar readiness write test\n")?;
        file.sync_data()
    });
    match outcome {
        WriteOutcome::Written { as_identity: WriteIdentity::App { uid, gid } } => super::pass(
            HOMES_WRITABLE_ID,
            format!(
                "the app identity (uid {uid}, gid {gid}) created, wrote and removed a test home under {path}"
            ),
        ),
        WriteOutcome::Written { as_identity: WriteIdentity::Agent } => super::pass(
            HOMES_WRITABLE_ID,
            format!("the agent (QUASAR_APP_PUID unset) created, wrote and removed a test home under {path}"),
        ),
        WriteOutcome::Unwritable { stage: WriteStage::CreateLeaf, reason } => {
            let remediation = if reason.contains("No such file or directory") {
                format!(
                    "Create {path} on the host and bind-mount it into the agent container at the same path (deploy/docker-compose.yml), then recreate the agent."
                )
            } else {
                format!(
                    "Make {path} writable by the agent: remount a read-only filesystem read-write, disable root_squash on an NFS export, or add allow_other on a FUSE mount, then check again."
                )
            };
            super::fail(HOMES_WRITABLE_ID, format!("the agent cannot create a home under {path}: {reason}"), remediation)
        }
        WriteOutcome::Unwritable { stage: WriteStage::WriteFile, reason } => {
            let who = match identity {
                WriteIdentity::App { uid, gid } => format!("the app identity (uid {uid}, gid {gid})"),
                WriteIdentity::Agent => "the agent".to_string(),
            };
            let remediation = match identity {
                WriteIdentity::App { uid, .. } => format!(
                    "The filesystem under {path} does not honour ownership for uid {uid}: add allow_other on a FUSE mount, disable root_squash/all_squash on an NFS export, or move QUASAR_HOME_ROOT to a local filesystem."
                ),
                WriteIdentity::Agent => format!(
                    "The filesystem under {path} does not honour ownership for the agent: add allow_other on a FUSE mount, disable root_squash/all_squash on an NFS export, or move QUASAR_HOME_ROOT to a local filesystem."
                ),
            };
            super::fail(
                HOMES_WRITABLE_ID,
                format!("{who} cannot write into a home it owns under {path}: {reason}"),
                remediation,
            )
        }
        // AssignOwner/AssumeIdentity/Cleanup never classify as Unwritable — only CreateLeaf
        // and WriteFile go through `classify_io_error`. Kept for exhaustiveness, worded the
        // same as the WriteFile case rather than panicking on a readiness check.
        WriteOutcome::Unwritable { stage: _, reason } => super::fail(
            HOMES_WRITABLE_ID,
            format!("the agent cannot write under {path}: {reason}"),
            format!("Check the filesystem holding {path} and the agent's logs, then check again."),
        ),
        WriteOutcome::Exhausted { reason, .. } => super::fail(
            HOMES_WRITABLE_ID,
            format!("homes storage under {path} is exhausted: {reason}"),
            format!(
                "Free space on the filesystem holding {path} (remove unused homes with the homes GC or move QUASAR_HOME_ROOT to a larger filesystem)."
            ),
        ),
        WriteOutcome::Inconclusive { stage, reason } => super::warn_check(
            HOMES_WRITABLE_ID,
            format!("the write test under {path} could not be concluded ({}): {reason}", stage_words(stage)),
            format!("Check the filesystem holding {path} and the agent's logs; the test runs again on the next refresh."),
        ),
    }
}

pub fn check_homes_free_space(view: &StorageView) -> ReadinessCheck {
    check_homes_free_space_inner(view).with_blocks(ReadinessBlocks::homes("control_plane"))
}

fn check_homes_free_space_inner(view: &StorageView) -> ReadinessCheck {
    let Some(root) = &view.homes else {
        return super::skip(HOMES_FREE_SPACE_ID, HOMES_NOT_CONFIGURED);
    };
    let path = root.path.display().to_string();
    let Some(space) = root.space else {
        return super::warn_check(
            HOMES_FREE_SPACE_ID,
            format!("could not read free space under {path}"),
            format!(
                "Check that {path} exists inside the agent container and is a mounted filesystem."
            ),
        );
    };
    if space.available_bytes == 0 {
        return super::fail(
            HOMES_FREE_SPACE_ID,
            format!("homes storage under {path} is exhausted: 0 bytes available of {}", gib(space.total_bytes)),
            format!(
                "Free space on the filesystem holding {path} (remove unused homes with the homes GC or move QUASAR_HOME_ROOT to a larger filesystem)."
            ),
        );
    }
    if space.available_bytes < view.free_floor_bytes {
        return super::warn_check(
            HOMES_FREE_SPACE_ID,
            format!("{} free under {path}, below the {} floor", gib(space.available_bytes), gib(view.free_floor_bytes)),
            format!(
                "Free space on the filesystem holding {path}, or change the floor with {FREE_FLOOR_ENV} (default {DEFAULT_FREE_FLOOR_GIB})."
            ),
        );
    }
    super::pass(
        HOMES_FREE_SPACE_ID,
        format!(
            "{} free of {} under {path} (floor {})",
            gib(space.available_bytes),
            gib(space.total_bytes),
            gib(view.free_floor_bytes)
        ),
    )
}

/// Shared by [`check_template_free_space`] and [`check_image_free_space`]: never a failure,
/// only skip/warn/pass — these are auxiliary storage, not the homes write path.
fn warn_only_free_space_check(
    id: &str,
    root: Option<&StorageRoot>,
    floor: u64,
    unconfigured: &str,
) -> ReadinessCheck {
    let Some(root) = root else {
        return super::skip(id, unconfigured);
    };
    let path = root.path.display().to_string();
    let Some(space) = root.space else {
        return super::warn_check(
            id,
            format!("could not read free space under {path}"),
            format!(
                "Check that {path} exists inside the agent container and is a mounted filesystem."
            ),
        );
    };
    if space.available_bytes < floor {
        return super::warn_check(
            id,
            format!("{} free under {path}, below the {} floor", gib(space.available_bytes), gib(floor)),
            format!(
                "Free space on the filesystem holding {path} (prune unused images / templates), or change the floor with {FREE_FLOOR_ENV}."
            ),
        );
    }
    super::pass(
        id,
        format!(
            "{} free of {} under {path} (floor {})",
            gib(space.available_bytes),
            gib(space.total_bytes),
            gib(floor)
        ),
    )
}

pub fn check_template_free_space(view: &StorageView) -> ReadinessCheck {
    warn_only_free_space_check(
        TEMPLATE_FREE_SPACE_ID,
        view.templates.as_ref(),
        view.free_floor_bytes,
        "No template root is configured on this host",
    )
}

pub fn check_image_free_space(view: &StorageView) -> ReadinessCheck {
    warn_only_free_space_check(
        IMAGE_FREE_SPACE_ID,
        view.images.as_ref(),
        view.free_floor_bytes,
        "The container engine's image storage is not visible to this agent",
    )
}
