//! The agent-owned tools directory, its install record and the `compat_tools.converge` runner.
//! semantics: control-api.md §Compatibility tools, "Install job" and "Trust rules".
//!
//! Layout under the root (`QUASAR_COMPAT_TOOLS_ROOT`, self-mapped like the template root):
//! `tools/<name>` is the tools directory sessions mount from, `staging/` holds a run's download
//! and tree on the same filesystem, and `record.json` is the install record, outside every tool.

use std::collections::HashMap;
use std::ffi::OsStr;
use std::fs::File;
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use quasar_runtime::DurableFile;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use tracing::{info, warn};

use super::extract::{self, Extracted};
use super::fetch::{self, Source};
use super::{valid_name, Fail};
use crate::jobs::{AbortFlag, JobOutcome, JobRunner};
use crate::nofollow_dir::open_dir_at;

pub const JOB_ID: &str = "compat_tools.converge";
const GIB: u64 = 1 << 30;
const MAX_ASSET: u64 = 2 * GIB;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EntryState {
    Installing,
    Installed,
    Removing,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RecordEntry {
    pub name: String,
    pub tag: String,
    pub sha512: String,
    pub bytes: u64,
    pub state: EntryState,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
struct Record {
    tools: Vec<RecordEntry>,
}

/// The tools root, its record and the tools live sessions hold mounted.
pub struct ToolStore {
    root: PathBuf,
    record: Mutex<Record>,
    /// session id -> (mounted names, parked). A parked session never holds a removal back.
    mounted: Mutex<HashMap<String, (Vec<String>, bool)>>,
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

fn io_fail(what: &str, e: io::Error) -> Fail {
    Fail::new("io_error", format!("{what}: {e}"))
}

/// Whether `path` or a proper ancestor other than `/` is a mount point in this process's
/// namespace: the root must be a bind from the host, or a session's bind of a tool would name a
/// directory the host does not have.
fn is_mounted(path: &Path) -> bool {
    let Ok(body) = std::fs::read_to_string("/proc/self/mountinfo") else {
        return false;
    };
    let points: Vec<&str> = body.lines().filter_map(|l| l.split(' ').nth(4)).collect();
    path.ancestors()
        .filter(|a| *a != Path::new("/"))
        .any(|a| points.iter().any(|p| Path::new(p) == a))
}

fn mkdir_if_missing(p: &Path) -> io::Result<()> {
    match std::fs::create_dir(p) {
        Err(e) if e.kind() != io::ErrorKind::AlreadyExists => Err(e),
        _ => {
            let m = std::fs::symlink_metadata(p)?;
            if m.is_dir() {
                Ok(())
            } else {
                Err(io::Error::other(format!(
                    "{} is not a directory",
                    p.display()
                )))
            }
        }
    }
}

/// Walks `dir` without following links, setting directories to `dir_mode` and files to
/// `file_mode(executable)`. Directories are opened `O_NOFOLLOW` one entry at a time.
fn set_modes(dir: &File, dir_mode: u32, file_mode: &dyn Fn(bool) -> u32) -> io::Result<()> {
    let names: Vec<Vec<u8>> = std::fs::read_dir(format!("/proc/self/fd/{}", dir.as_raw_fd()))?
        .map(|e| e.map(|e| e.file_name().as_bytes().to_vec()))
        .collect::<io::Result<_>>()?;
    for n in names {
        let name = OsStr::from_bytes(&n);
        match open_dir_at(dir, name) {
            Ok(sub) => set_modes(&sub, dir_mode, file_mode)?,
            Err(e) if crate::nofollow_dir::is_symlink_or_not_dir(&e) => {
                let c = std::ffi::CString::new(n.clone()).map_err(io::Error::other)?;
                // SAFETY: `dir` is open and `c` NUL-terminated; the fd is closed below.
                let fd = unsafe {
                    libc::openat(
                        dir.as_raw_fd(),
                        c.as_ptr(),
                        libc::O_RDONLY | libc::O_NOFOLLOW | libc::O_CLOEXEC | libc::O_NONBLOCK,
                    )
                };
                if fd < 0 {
                    let err = io::Error::last_os_error();
                    if err.raw_os_error() == Some(libc::ELOOP) {
                        continue; // a symlink: its mode is never set
                    }
                    return Err(err);
                }
                // SAFETY: openat returned an fd we own.
                let f = unsafe { <File as std::os::fd::FromRawFd>::from_raw_fd(fd) };
                let meta = f.metadata()?;
                use std::os::unix::fs::PermissionsExt;
                let exec = meta.permissions().mode() & 0o111 != 0;
                // SAFETY: `f` is open.
                if unsafe { libc::fchmod(f.as_raw_fd(), file_mode(exec)) } != 0 {
                    return Err(io::Error::last_os_error());
                }
            }
            Err(e) => return Err(e),
        }
    }
    // SAFETY: `dir` is open.
    if unsafe { libc::fchmod(dir.as_raw_fd(), dir_mode) } != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

fn make_read_only(dir: &File) -> io::Result<()> {
    set_modes(dir, 0o555, &|x| if x { 0o555 } else { 0o444 })
}

/// Restores owner write on a read-only tree, then deletes it (neither follows a link).
fn delete_tree(path: &Path) -> io::Result<()> {
    match crate::nofollow_dir::open_dir(path) {
        Ok(d) => set_modes(&d, 0o755, &|x| if x { 0o755 } else { 0o644 })?,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e),
    }
    std::fs::remove_dir_all(path)
}

/// `renameat2(RENAME_NOREPLACE)`.
fn rename_noreplace(from_dir: &File, from: &[u8], to_dir: &File, to: &[u8]) -> io::Result<()> {
    let (f, t) = (
        std::ffi::CString::new(from).map_err(io::Error::other)?,
        std::ffi::CString::new(to).map_err(io::Error::other)?,
    );
    // SAFETY: both directories are open and both names NUL-terminated for the call.
    let rc = unsafe {
        libc::renameat2(
            from_dir.as_raw_fd(),
            f.as_ptr(),
            to_dir.as_raw_fd(),
            t.as_ptr(),
            libc::RENAME_NOREPLACE,
        )
    };
    if rc != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

impl ToolStore {
    /// Opens the root if it is usable (agent-api.md §register): an existing directory, outside the
    /// managed-home and template roots, writable, and (when `require_mount`) a bind from the host.
    /// Then finishes or rolls back what a killed run left (§Install job, "The install record").
    pub fn open(root: &Path, others: &[&Path], require_mount: bool) -> Result<Self, String> {
        if !root.is_absolute() {
            return Err(format!("{} is not an absolute path", root.display()));
        }
        let meta =
            std::fs::symlink_metadata(root).map_err(|e| format!("{}: {e}", root.display()))?;
        if !meta.is_dir() {
            return Err(format!("{} is not a directory", root.display()));
        }
        for other in others.iter().filter(|o| !o.as_os_str().is_empty()) {
            if root.starts_with(other) || other.starts_with(root) {
                return Err(format!(
                    "{} overlaps {}, which sessions may mount",
                    root.display(),
                    other.display()
                ));
            }
        }
        if require_mount && !is_mounted(root) {
            return Err(format!(
                "{} is not bind-mounted into the agent container at the same path",
                root.display()
            ));
        }
        for sub in ["tools", "staging"] {
            mkdir_if_missing(&root.join(sub))
                .map_err(|e| format!("{}: {e}", root.join(sub).display()))?;
        }
        tempfile::tempdir_in(root.join("staging"))
            .map_err(|e| format!("{} is not writable: {e}", root.display()))?;
        let store = ToolStore {
            root: root.to_path_buf(),
            record: Mutex::new(Record::default()),
            mounted: Mutex::new(HashMap::new()),
        };
        store
            .recover()
            .map_err(|e| format!("recovering the install record: {e}"))?;
        Ok(store)
    }

    pub fn tools_dir(&self) -> PathBuf {
        self.root.join("tools")
    }

    fn record_file(&self) -> DurableFile<Record> {
        DurableFile::new(self.root.join("record.json"), "json.tmp")
    }

    fn save(&self, rec: &Record) -> io::Result<()> {
        self.record_file().store(rec)
    }

    fn recover(&self) -> io::Result<()> {
        for leftover in std::fs::read_dir(self.root.join("staging"))? {
            let path = leftover?.path();
            delete_tree(&path)?;
            info!(
                token = "compat-tools-staging-cleaned",
                "compat tools: removed the leftover {}",
                path.display()
            );
        }
        let mut rec = self.record_file().load()?.unwrap_or_default();
        let tools = self.tools_dir();
        let mut kept = Vec::new();
        for mut e in std::mem::take(&mut rec.tools) {
            let dir = tools.join(&e.name);
            let present = std::fs::symlink_metadata(&dir).is_ok_and(|m| m.is_dir());
            match (e.state, present) {
                (EntryState::Installed, true) => kept.push(e),
                // The rename happened: the tree was verified, so finish it.
                (EntryState::Installing, true) => {
                    make_read_only(&crate::nofollow_dir::open_dir(&dir)?)?;
                    e.state = EntryState::Installed;
                    info!(
                        token = "compat-tools-install-finished",
                        "compat tools: finished installing {}", e.name
                    );
                    kept.push(e);
                }
                (EntryState::Removing, true) => {
                    self.move_out_and_delete(&e.name)?;
                    info!(
                        token = "compat-tools-remove-finished",
                        "compat tools: finished removing {}", e.name
                    );
                }
                (_, false) => info!(
                    token = "compat-tools-record-dropped",
                    "compat tools: dropped {} from the install record (its directory is missing)",
                    e.name
                ),
            }
        }
        rec.tools = kept;
        self.save(&rec)?;
        *lock(&self.record) = rec;
        Ok(())
    }

    fn move_out_and_delete(&self, name: &str) -> io::Result<()> {
        let tools = crate::nofollow_dir::open_dir(&self.tools_dir())?;
        let staging_path = self.root.join("staging");
        let staging = crate::nofollow_dir::open_dir(&staging_path)?;
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos();
        let away = format!("removing-{name}-{nanos}");
        rename_noreplace(&tools, name.as_bytes(), &staging, away.as_bytes())?;
        delete_tree(&staging_path.join(away))
    }

    /// The installed tools, for `capacity.compat_tools.tools`.
    pub fn installed(&self) -> Vec<RecordEntry> {
        lock(&self.record)
            .tools
            .iter()
            .filter(|e| e.state == EntryState::Installed)
            .cloned()
            .collect()
    }

    pub fn free_bytes(&self) -> Option<u64> {
        crate::artifact::free_bytes(&self.tools_dir())
    }

    /// The names in `wanted` this store can mount: installed and present (agent-api.md
    /// §session_assign, "The mounts"). Others are skipped and logged.
    pub fn mountable(&self, wanted: &[String]) -> Vec<String> {
        let installed = self.installed();
        let tools = self.tools_dir();
        let mut out = Vec::new();
        for n in wanted {
            let ok = valid_name(n)
                && installed.iter().any(|e| &e.name == n)
                && std::fs::symlink_metadata(tools.join(n)).is_ok_and(|m| m.is_dir())
                && !out.contains(n);
            if ok {
                out.push(n.clone());
            } else {
                warn!(
                    token = "compat-tools-mount-skipped",
                    "compat tools: not mounting {n:?}: not installed here"
                );
            }
        }
        out
    }

    pub fn hold(&self, session_id: &str, names: Vec<String>, parked: bool) {
        if !names.is_empty() {
            lock(&self.mounted).insert(session_id.to_owned(), (names, parked));
        }
    }

    /// A play session that adopts a parked runtime keeps its mounts, no longer parked.
    pub fn adopt(&self, from: &str, to: &str, parked: bool) {
        let mut m = lock(&self.mounted);
        if let Some((names, _)) = m.remove(from) {
            m.insert(to.to_owned(), (names, parked));
        }
    }

    pub fn release(&self, session_id: &str) {
        lock(&self.mounted).remove(session_id);
    }

    fn in_use(&self, name: &str) -> bool {
        lock(&self.mounted)
            .values()
            .any(|(names, parked)| !parked && names.iter().any(|n| n == name))
    }

    fn set_entry(&self, entry: Option<RecordEntry>, drop_name: &str) -> io::Result<()> {
        let mut rec = lock(&self.record).clone();
        rec.tools.retain(|e| e.name != drop_name);
        rec.tools.extend(entry);
        self.save(&rec)?;
        *lock(&self.record) = rec;
        Ok(())
    }
}

#[derive(Debug)]
struct InstallParams {
    tool_id: Value,
    tag: String,
    asset: String,
    size: u64,
    sha512: String,
    expected_name: Option<String>,
    reserve: u64,
}

fn hex128(s: &str) -> bool {
    s.len() == 128 && s.bytes().all(|c| matches!(c, b'0'..=b'9' | b'a'..=b'f'))
}

fn invalid(detail: impl Into<String>) -> Fail {
    Fail::new("invalid_params", detail)
}

fn parse_install(p: &Value) -> Result<InstallParams, Fail> {
    let s = |k: &str| p.get(k).and_then(Value::as_str);
    let tag = s("tag")
        .filter(|t| fetch::valid_tag(t))
        .ok_or_else(|| invalid("tag"))?;
    let asset = s("asset")
        .filter(|a| fetch::asset_name(tag, a).is_some())
        .ok_or_else(|| invalid("asset"))?;
    let size = p
        .get("size_bytes")
        .and_then(Value::as_u64)
        .filter(|n| (1..=MAX_ASSET).contains(n))
        .ok_or_else(|| invalid("size_bytes"))?;
    let sha512 = s("sha512")
        .filter(|h| hex128(h))
        .ok_or_else(|| invalid("sha512"))?;
    let expected_name = match p.get("expected_name") {
        None | Some(Value::Null) => None,
        Some(Value::String(n)) if valid_name(n) => Some(n.clone()),
        Some(_) => return Err(invalid("expected_name")),
    };
    let reserve = p
        .get("reserve_bytes")
        .and_then(Value::as_u64)
        .ok_or_else(|| invalid("reserve_bytes"))?;
    Ok(InstallParams {
        tool_id: p.get("tool_id").cloned().unwrap_or(Value::Null),
        tag: tag.to_owned(),
        asset: asset.to_owned(),
        size,
        sha512: sha512.to_owned(),
        expected_name,
        // T7: whatever the params say, at least 1 GiB.
        reserve: reserve.max(GIB),
    })
}

/// T6: the name checks after the grammar.
fn check_name(name: &str, expected: Option<&str>) -> Result<(), Fail> {
    if !valid_name(name) || name == "." || name == ".." {
        return Err(Fail::new(
            "vdf_invalid",
            format!("tool name {name:?} is outside the grammar"),
        ));
    }
    let lower = name.to_ascii_lowercase();
    if !name.starts_with("GE-Proton")
        || ["proton_", "steamlinuxruntime", "legacysteamruntime"]
            .iter()
            .any(|p| lower.starts_with(p))
    {
        return Err(Fail::new(
            "name_reserved",
            format!("tool name {name:?} is reserved"),
        ));
    }
    if expected.is_some_and(|x| x != name) {
        return Err(Fail::new(
            "name_mismatch",
            format!(
                "the archive registers {name:?}, expected {:?}",
                expected.unwrap_or_default()
            ),
        ));
    }
    Ok(())
}

pub struct ConvergeRunner {
    store: Arc<ToolStore>,
    source: Arc<dyn Source>,
    on_change: Arc<dyn Fn() + Send + Sync>,
    limits: extract::Limits,
}

impl ConvergeRunner {
    pub fn new(
        store: Arc<ToolStore>,
        source: Arc<dyn Source>,
        on_change: Arc<dyn Fn() + Send + Sync>,
    ) -> Self {
        Self {
            store,
            source,
            on_change,
            limits: extract::Limits::CONTRACT,
        }
    }

    fn summary(
        action: &str,
        tool_id: &Value,
        name: &str,
        sha: &str,
        bytes: u64,
        already: bool,
    ) -> Value {
        json!({ "action": action, "tool_id": tool_id, "name": name, "sha512": sha,
                "bytes": bytes, "already_installed": already })
    }

    fn install(&self, p: &Value, abort: &AbortFlag) -> Result<JobOutcome, Fail> {
        let p = parse_install(p)?;
        let store = &self.store;
        if let Some(e) = store.installed().into_iter().find(|e| e.sha512 == p.sha512) {
            check_name(&e.name, p.expected_name.as_deref())?;
            if std::fs::symlink_metadata(store.tools_dir().join(&e.name)).is_ok_and(|m| m.is_dir())
            {
                return Ok(JobOutcome::Succeeded(Self::summary(
                    "install", &p.tool_id, &e.name, &e.sha512, e.bytes, true,
                )));
            }
        }
        let need = p.size + extract::MAX_UNPACKED + p.reserve;
        let free = store.free_bytes().unwrap_or(0);
        if free < need {
            info!(
                token = "compat-tools-disk-short",
                "compat tools: {free} bytes free, {need} needed"
            );
            return Ok(JobOutcome::Deferred("insufficient_disk".into()));
        }
        let run = tempfile::Builder::new()
            .prefix("run-")
            .tempdir_in(store.root.join("staging"))
            .map_err(|e| io_fail("staging", e))?;
        let asset = fetch::asset_name(&p.tag, &p.asset).expect("checked in parse_install");
        let archive = run.path().join("asset");
        let mut out = File::create(&archive).map_err(|e| io_fail("create download", e))?;
        let sha = fetch::download(&*self.source, &p.tag, &asset, p.size, &mut out, &|| {
            abort.is_aborted()
        })?;
        drop(out);
        if sha != p.sha512 {
            let mut f = Fail::new(
                "checksum_mismatch",
                "the download's SHA-512 is not the pinned one",
            );
            f.extra = Some(json!({ "expected_sha512": p.sha512, "actual_sha512": sha }));
            return Err(f);
        }
        let tree_path = run.path().join("tree");
        std::fs::create_dir(&tree_path).map_err(|e| io_fail("staging tree", e))?;
        let tree =
            crate::nofollow_dir::open_dir(&tree_path).map_err(|e| io_fail("staging tree", e))?;
        let reserve = p.reserve;
        let tools_dir = store.tools_dir();
        let mut space_ok = || crate::artifact::free_bytes(&tools_dir).is_some_and(|f| f >= reserve);
        let Extracted { top, bytes } = extract::extract(
            File::open(&archive).map_err(|e| io_fail("open download", e))?,
            &tree,
            &mut space_ok,
            self.limits,
        )?;
        let _ = std::fs::remove_file(&archive);
        if abort.is_aborted() {
            return Err(Fail::new("io_error", "the run was aborted"));
        }
        let topfd =
            open_dir_at(&tree, OsStr::from_bytes(&top)).map_err(|e| io_fail("open tree", e))?;
        let name = extract::read_vdf_name(&topfd)?;
        check_name(&name, p.expected_name.as_deref())?;
        let conflict = || {
            Fail::new(
                "name_conflict",
                format!("{name} is already in the tools directory with other bytes"),
            )
        };
        if std::fs::symlink_metadata(tools_dir.join(&name)).is_ok() {
            return Err(conflict());
        }
        let entry = RecordEntry {
            name: name.clone(),
            tag: p.tag.clone(),
            sha512: sha.clone(),
            bytes,
            state: EntryState::Installing,
        };
        store
            .set_entry(Some(entry.clone()), &name)
            .map_err(|e| io_fail("record", e))?;
        let tools =
            crate::nofollow_dir::open_dir(&tools_dir).map_err(|e| io_fail("tools dir", e))?;
        if let Err(e) = rename_noreplace(&tree, &top, &tools, name.as_bytes()) {
            let _ = store.set_entry(None, &name);
            return Err(if e.kind() == io::ErrorKind::AlreadyExists {
                conflict()
            } else {
                io_fail("rename", e)
            });
        }
        let placed = open_dir_at(&tools, OsStr::new(&name)).map_err(|e| io_fail("open tool", e))?;
        make_read_only(&placed).map_err(|e| io_fail("read-only", e))?;
        store
            .set_entry(
                Some(RecordEntry {
                    state: EntryState::Installed,
                    ..entry
                }),
                &name,
            )
            .map_err(|e| io_fail("record", e))?;
        info!(
            token = "compat-tools-installed",
            "compat tools: installed {name} ({} bytes)", bytes
        );
        (self.on_change)();
        Ok(JobOutcome::Succeeded(Self::summary(
            "install", &p.tool_id, &name, &sha, bytes, false,
        )))
    }

    fn remove(&self, p: &Value) -> Result<JobOutcome, Fail> {
        let tool_id = p.get("tool_id").cloned().unwrap_or(Value::Null);
        let name = p
            .get("name")
            .and_then(Value::as_str)
            .filter(|n| valid_name(n) && *n != "." && *n != "..")
            .ok_or_else(|| invalid("name"))?;
        let sha = p
            .get("sha512")
            .and_then(Value::as_str)
            .filter(|h| hex128(h))
            .ok_or_else(|| invalid("sha512"))?;
        let store = &self.store;
        let Some(entry) = store
            .installed()
            .into_iter()
            .find(|e| e.name == name && e.sha512 == sha)
        else {
            return Ok(JobOutcome::Succeeded(Self::summary(
                "remove", &tool_id, name, sha, 0, false,
            )));
        };
        if store.in_use(name) {
            return Ok(JobOutcome::Deferred("in_use".into()));
        }
        store
            .set_entry(
                Some(RecordEntry {
                    state: EntryState::Removing,
                    ..entry.clone()
                }),
                name,
            )
            .map_err(|e| io_fail("record", e))?;
        store
            .move_out_and_delete(name)
            .map_err(|e| io_fail("remove", e))?;
        store
            .set_entry(None, name)
            .map_err(|e| io_fail("record", e))?;
        info!(
            token = "compat-tools-removed",
            "compat tools: removed {name}"
        );
        (self.on_change)();
        Ok(JobOutcome::Succeeded(Self::summary(
            "remove",
            &tool_id,
            name,
            sha,
            entry.bytes,
            false,
        )))
    }
}

impl JobRunner for ConvergeRunner {
    fn job_id(&self) -> &'static str {
        JOB_ID
    }

    fn run(&self, params: &Value, abort: &AbortFlag) -> JobOutcome {
        let result = match params.get("action").and_then(Value::as_str) {
            Some("install") => self.install(params, abort),
            Some("remove") => self.remove(params),
            _ => Err(invalid("action")),
        };
        match result {
            Ok(o) => o,
            Err(f) => {
                warn!(
                    token = "compat-tools-run-failed",
                    "compat tools: {} ({})", f.reason, f.detail
                );
                f.into_outcome()
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::compat_tools::fetch::{Hop, HopError};
    use sha2::{Digest, Sha512};
    use std::io::Write as _;
    use std::os::unix::fs::PermissionsExt;
    use std::sync::atomic::{AtomicUsize, Ordering};

    const TAG: &str = "GE-Proton11-12";
    const NAME: &str = "GE-Proton11-12-x86_64";
    const ASSET_URL: &str = "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/GE-Proton11-12-x86_64.tar.gz";
    /// A fake source's answer for a URL: `None` serves the default route.
    type Route = Box<dyn Fn(&str) -> Option<Result<(u16, Option<String>), String>> + Send + Sync>;

    const CDN: &str =
        "https://release-assets.githubusercontent.com/github-production-release-asset/1?sig=x";

    fn vdf(name: &str, extra: &str) -> Vec<u8> {
        format!(
            "\"compatibilitytools\"\n{{\n  \"compat_tools\"\n  {{\n    \"{name}\" // tool\n    {{\n      \"install_path\" \".\"\n      \"display_name\" \"{name}\"\n      \"from_oslist\" \"windows\"\n      \"to_oslist\" \"linux\"\n{extra}    }}\n  }}\n}}\n"
        )
        .into_bytes()
    }

    enum E<'a> {
        Dir(&'a [u8]),
        File(&'a [u8], &'a [u8], u32),
        Link(&'a [u8], &'a [u8]),
        Hard(&'a [u8], &'a [u8]),
        Fifo(&'a [u8]),
        /// A header that claims `size` bytes but carries none.
        Claims(&'a [u8], u64),
        Global,
    }

    /// Writes headers byte for byte, so paths `tar::Builder` would refuse can be built.
    fn tgz(entries: &[E]) -> Vec<u8> {
        let mut b = tar::Builder::new(flate2::write::GzEncoder::new(
            Vec::new(),
            flate2::Compression::fast(),
        ));
        for e in entries {
            let mut h = tar::Header::new_gnu();
            let (path, kind, link, data, mode): (&[u8], tar::EntryType, &[u8], &[u8], u32) = match e
            {
                E::Dir(p) => (p, tar::EntryType::Directory, b"", b"", 0o755),
                E::File(p, d, m) => (p, tar::EntryType::Regular, b"", d, *m),
                E::Link(p, t) => (p, tar::EntryType::Symlink, t, b"", 0o777),
                E::Hard(p, t) => (p, tar::EntryType::Link, t, b"", 0o644),
                E::Fifo(p) => (p, tar::EntryType::Fifo, b"", b"", 0o644),
                E::Claims(p, n) => {
                    let g = h.as_gnu_mut().unwrap();
                    g.name[..p.len()].copy_from_slice(p);
                    h.set_entry_type(tar::EntryType::Regular);
                    h.set_size(*n);
                    h.set_mode(0o644);
                    h.set_cksum();
                    b.append(&h, &[][..]).unwrap();
                    continue;
                }
                E::Global => (
                    b"pax_global_header",
                    tar::EntryType::XGlobalHeader,
                    b"",
                    b"",
                    0o644,
                ),
            };
            let g = h.as_gnu_mut().unwrap();
            g.name[..path.len()].copy_from_slice(path);
            g.linkname[..link.len()].copy_from_slice(link);
            h.set_entry_type(kind);
            h.set_size(data.len() as u64);
            h.set_mode(mode);
            h.set_cksum();
            b.append(&h, data).unwrap();
        }
        b.into_inner().unwrap().finish().unwrap()
    }

    fn good_tool() -> Vec<u8> {
        let v = vdf(NAME, "");
        tgz(&[
            E::Dir(b"GE-Proton11-12/"),
            E::File(b"GE-Proton11-12/compatibilitytool.vdf", &v, 0o644),
            E::File(b"GE-Proton11-12/proton", b"#!/bin/sh\n", 0o4755),
            E::Dir(b"GE-Proton11-12/files/lib/"),
            E::File(b"GE-Proton11-12/files/x", b"x", 0o644),
            E::Link(b"GE-Proton11-12/files/lib/l", b"../x"),
        ])
    }

    fn sha(b: &[u8]) -> String {
        crate::artifact::hex(&Sha512::digest(b))
    }

    /// Serves `ASSET_URL` -> redirect -> `CDN` -> 200 `body`, unless `route` says otherwise.
    struct Fake {
        body: Vec<u8>,
        route: Route,
        calls: AtomicUsize,
    }

    impl Source for Fake {
        fn get(&self, url: &str) -> Result<Hop, HopError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            let (status, location) = match (self.route)(url) {
                Some(Ok(r)) => r,
                Some(Err(why)) => return Err(HopError::AddressRefused(why)),
                None if url == ASSET_URL => (302, Some(CDN.to_string())),
                None if url == CDN => (200, None),
                None => (404, None),
            };
            Ok(Hop {
                status,
                location,
                content_encoding: None,
                body: Box::new(std::io::Cursor::new(if status == 200 {
                    self.body.clone()
                } else {
                    vec![]
                })),
            })
        }
    }

    struct Rig {
        dir: tempfile::TempDir,
        store: Arc<ToolStore>,
        changes: Arc<AtomicUsize>,
    }

    impl Rig {
        fn new() -> Self {
            let dir = tempfile::tempdir().unwrap();
            let store = Arc::new(ToolStore::open(dir.path(), &[], false).unwrap());
            Rig {
                dir,
                store,
                changes: Arc::new(AtomicUsize::new(0)),
            }
        }

        fn reopen(&self) -> ToolStore {
            ToolStore::open(self.dir.path(), &[], false).unwrap()
        }

        fn runner(&self, fake: Fake) -> (ConvergeRunner, Arc<Fake>) {
            let fake = Arc::new(fake);
            let changes = self.changes.clone();
            let mut r = ConvergeRunner::new(
                self.store.clone(),
                fake.clone(),
                Arc::new(move || {
                    changes.fetch_add(1, Ordering::SeqCst);
                }),
            );
            r.limits = extract::Limits {
                entries: 50,
                bytes: 1 << 20,
            };
            (r, fake)
        }

        fn run(&self, body: Vec<u8>, params: Value) -> (JobOutcome, Arc<Fake>) {
            self.run_routed(body, params, Box::new(|_| None))
        }

        fn run_routed(
            &self,
            body: Vec<u8>,
            params: Value,
            route: Route,
        ) -> (JobOutcome, Arc<Fake>) {
            let (r, fake) = self.runner(Fake {
                body,
                route,
                calls: AtomicUsize::new(0),
            });
            (r.run(&params, &AbortFlag::new()), fake)
        }

        fn leftovers(&self) -> usize {
            std::fs::read_dir(self.dir.path().join("staging"))
                .unwrap()
                .count()
        }

        fn tools(&self) -> Vec<String> {
            let mut v: Vec<String> = std::fs::read_dir(self.store.tools_dir())
                .unwrap()
                .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
                .collect();
            v.sort();
            v
        }
    }

    impl Drop for Rig {
        fn drop(&mut self) {
            // Installed trees are read-only; a non-root test run could not clean them up.
            let _ = delete_tree(&self.store.tools_dir());
        }
    }

    fn install(body: &[u8]) -> Value {
        json!({ "action": "install", "tool_id": "6f1c", "tag": TAG, "asset": "x86_64",
                "size_bytes": body.len(), "sha512": sha(body), "expected_name": NAME,
                "reserve_bytes": 1 })
    }

    fn reason(o: &JobOutcome) -> String {
        match o {
            JobOutcome::FailedWith(s, _) => s["reason"].as_str().unwrap().to_owned(),
            JobOutcome::Deferred(r) => r.clone(),
            other => panic!("not refused: {other:?}"),
        }
    }

    #[test]
    fn a_matching_download_installs_read_only_and_is_reported() {
        let rig = Rig::new();
        let body = good_tool();
        let (out, fake) = rig.run(body.clone(), install(&body));
        let JobOutcome::Succeeded(s) = &out else {
            panic!("{out:?}")
        };
        assert_eq!(s["name"], NAME);
        assert_eq!(s["already_installed"], false);
        assert_eq!(s["sha512"], sha(&body));
        assert_eq!(
            fake.calls.load(Ordering::SeqCst),
            2,
            "one redirect, then the asset host"
        );
        assert_eq!(rig.tools(), vec![NAME]);
        assert_eq!(rig.leftovers(), 0, "the temporary directory is removed");
        assert_eq!(rig.changes.load(Ordering::SeqCst), 1, "capacity is re-sent");
        let tool = rig.store.tools_dir().join(NAME);
        let mode = |p: &str| {
            std::fs::symlink_metadata(tool.join(p))
                .unwrap()
                .permissions()
                .mode()
                & 0o7777
        };
        assert_eq!(mode(""), 0o555);
        assert_eq!(mode("proton"), 0o555, "setuid cleared, still executable");
        assert_eq!(mode("files/x"), 0o444);
        assert_eq!(
            std::fs::read_link(tool.join("files/lib/l")).unwrap(),
            Path::new("../x")
        );
        let installed = rig.store.installed();
        assert_eq!(installed.len(), 1);
        assert_eq!(installed[0].bytes, (vdf(NAME, "").len() + 11) as u64);
    }

    #[test]
    fn a_re_run_is_idempotent_and_fetches_nothing() {
        let rig = Rig::new();
        let body = good_tool();
        rig.run(body.clone(), install(&body));
        let (out, fake) = rig.run(body.clone(), install(&body));
        let JobOutcome::Succeeded(s) = &out else {
            panic!("{out:?}")
        };
        assert_eq!(s["already_installed"], true);
        assert_eq!(fake.calls.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn the_install_survives_a_restart_and_is_reported_after_reconnect() {
        let rig = Rig::new();
        let body = good_tool();
        rig.run(body.clone(), install(&body));
        let again = rig.reopen();
        assert_eq!(again.installed(), rig.store.installed());
        assert_eq!(
            again.mountable(&[NAME.to_string(), "GE-Proton9-1".into()]),
            vec![NAME.to_string()]
        );
    }

    #[test]
    fn a_checksum_mismatch_keeps_nothing_and_shows_both_digests() {
        let rig = Rig::new();
        let body = good_tool();
        let mut params = install(&body);
        params["sha512"] = json!("a".repeat(128));
        let (out, _) = rig.run(body.clone(), params);
        let JobOutcome::FailedWith(s, _) = &out else {
            panic!("{out:?}")
        };
        assert_eq!(s["reason"], "checksum_mismatch");
        assert_eq!(s["expected_sha512"], "a".repeat(128));
        assert_eq!(s["actual_sha512"], sha(&body));
        assert!(rig.tools().is_empty());
        assert_eq!(rig.leftovers(), 0);
        assert!(rig.store.installed().is_empty());
    }

    #[test]
    fn redirects_outside_the_allowlist_are_refused() {
        let body = good_tool();
        let cases: Vec<(&str, Route)> = vec![
            (
                "another host",
                Box::new(|u: &str| {
                    (u == ASSET_URL).then(|| Ok((302, Some("https://evil.example/x".into()))))
                }),
            ),
            (
                "plain http",
                Box::new(|u: &str| {
                    (u == ASSET_URL)
                        .then(|| Ok((302, Some("http://objects.githubusercontent.com/x".into()))))
                }),
            ),
            (
                "another release",
                Box::new(|u: &str| {
                    (u == ASSET_URL).then(|| Ok((302, Some("https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/../GE-Proton1-1/x".into()))))
                }),
            ),
            (
                "no Location",
                Box::new(|u: &str| (u == ASSET_URL).then_some(Ok((302, None)))),
            ),
            (
                "a loop",
                Box::new(|_: &str| Some(Ok((302, Some(ASSET_URL.into()))))),
            ),
            (
                "a refused address",
                Box::new(|_: &str| Some(Err("github.com resolves only to 10.0.0.1".into()))),
            ),
        ];
        for (what, route) in cases {
            let rig = Rig::new();
            let (out, fake) = rig.run_routed(body.clone(), install(&body), route);
            assert_eq!(reason(&out), "redirect_refused", "{what}");
            assert!(
                fake.calls.load(Ordering::SeqCst) <= 6,
                "{what}: at most 5 redirects followed"
            );
            assert!(rig.tools().is_empty() && rig.leftovers() == 0, "{what}");
        }
    }

    #[test]
    fn five_redirects_are_followed_and_a_sixth_is_refused() {
        let hops = |n: usize| -> Route {
            Box::new(move |u: &str| {
                let i: usize = u.rsplit_once("hop=").map_or(0, |(_, i)| i.parse().unwrap());
                (u == ASSET_URL || u.contains("hop=")).then(|| {
                    Ok(if i + 1 < n {
                        (
                            302,
                            Some(format!(
                                "https://objects.githubusercontent.com/a?hop={}",
                                i + 1
                            )),
                        )
                    } else {
                        (302, Some(CDN.into()))
                    })
                })
            })
        };
        let body = good_tool();
        let rig = Rig::new();
        assert!(matches!(
            rig.run_routed(body.clone(), install(&body), hops(5)).0,
            JobOutcome::Succeeded(_)
        ));
        let rig = Rig::new();
        assert_eq!(
            reason(&rig.run_routed(body.clone(), install(&body), hops(6)).0),
            "redirect_refused"
        );
    }

    #[test]
    fn a_body_of_the_wrong_size_is_refused() {
        let rig = Rig::new();
        let body = good_tool();
        let mut params = install(&body);
        params["size_bytes"] = json!(body.len() - 1);
        assert_eq!(reason(&rig.run(body.clone(), params).0), "size_mismatch");
        let mut params = install(&body);
        params["size_bytes"] = json!(body.len() + 1);
        assert_eq!(reason(&rig.run(body, params).0), "size_mismatch");
        assert_eq!(rig.leftovers(), 0);
    }

    #[test]
    fn params_outside_the_rules_fetch_nothing() {
        let body = good_tool();
        let bad = [
            ("tag", json!("GE-Proton11-12/../x")),
            ("tag", json!("Proton-11")),
            ("asset", json!("../x")),
            ("size_bytes", json!(3u64 << 30)),
            ("sha512", json!("ABC")),
            ("expected_name", json!("../x")),
            ("action", json!("exec")),
        ];
        for (k, v) in bad {
            let rig = Rig::new();
            let mut p = install(&body);
            p[k] = v;
            let (out, fake) = rig.run(body.clone(), p);
            assert_eq!(reason(&out), "invalid_params", "{k}");
            assert_eq!(fake.calls.load(Ordering::SeqCst), 0, "{k}");
        }
    }

    #[test]
    fn short_disk_defers_before_any_download() {
        let rig = Rig::new();
        let body = good_tool();
        let mut p = install(&body);
        p["reserve_bytes"] = json!(1u64 << 60);
        let (out, fake) = rig.run(body, p);
        assert_eq!(out, JobOutcome::Deferred("insufficient_disk".into()));
        assert_eq!(fake.calls.load(Ordering::SeqCst), 0);
    }

    fn vdf_entry_2(v: &[u8]) -> E<'_> {
        E::File(b"t/compatibilitytool.vdf", v, 0o644)
    }

    fn refused_archive(entries: &[E]) -> String {
        let rig = Rig::new();
        let body = tgz(entries);
        let mut p = install(&body);
        p["expected_name"] = Value::Null;
        let out = rig.run(body, p).0;
        assert!(rig.tools().is_empty() && rig.leftovers() == 0);
        reason(&out)
    }

    #[test]
    fn unsafe_archives_are_refused() {
        let v = vdf(NAME, "");
        let f = |p: &'static [u8]| E::File(p, b"x", 0o644);
        let vdf_entry = E::File(b"t/compatibilitytool.vdf", &v, 0o644);
        let cases: Vec<(&str, Vec<E>)> = vec![
            ("absolute path", vec![E::Dir(b"t/"), f(b"/etc/x")]),
            ("dot-dot", vec![E::Dir(b"t/"), f(b"t/../x")]),
            ("two top-level dirs", vec![E::Dir(b"t/"), E::Dir(b"u/")]),
            ("a top-level file", vec![f(b"x")]),
            ("duplicate", vec![E::Dir(b"t/"), f(b"t/a"), f(b"t/a")]),
            (
                "duplicate dir",
                vec![E::Dir(b"t/"), E::Dir(b"t/d/"), E::Dir(b"t/d")],
            ),
            (
                "hardlink",
                vec![E::Dir(b"t/"), f(b"t/a"), E::Hard(b"t/b", b"t/a")],
            ),
            ("fifo", vec![E::Dir(b"t/"), E::Fifo(b"t/p")]),
            (
                "absolute symlink",
                vec![E::Dir(b"t/"), E::Link(b"t/l", b"/etc/passwd")],
            ),
            (
                "escaping symlink",
                vec![E::Dir(b"t/"), vdf_entry, E::Link(b"t/l", b"../../x")],
            ),
            (
                "dir symlink escape",
                vec![E::Dir(b"t/"), vdf_entry_2(&v), E::Link(b"t/up", b"..")],
            ),
            (
                "looping symlink",
                vec![E::Dir(b"t/"), E::Link(b"t/a", b"b"), E::Link(b"t/b", b"a")],
            ),
            (
                "dangling symlink",
                vec![E::Dir(b"t/"), E::Link(b"t/l", b"nothing")],
            ),
            (
                "write through a link",
                vec![
                    E::Dir(b"t/"),
                    E::Dir(b"t/d/"),
                    E::Link(b"t/l", b"d"),
                    f(b"t/l/x"),
                ],
            ),
            (
                "file over a link",
                vec![E::Dir(b"t/"), f(b"t/x"), E::Link(b"t/l", b"x"), f(b"t/l")],
            ),
        ];
        for (what, entries) in cases {
            assert_eq!(refused_archive(&entries), "unsafe_archive", "{what}");
        }
    }

    #[test]
    fn a_link_that_climbs_but_stays_inside_is_accepted() {
        let v = vdf(NAME, "");
        let rig = Rig::new();
        let body = tgz(&[
            E::Dir(b"t/"),
            E::File(b"t/compatibilitytool.vdf", &v, 0o644),
            E::Dir(b"t/a/b/"),
            E::File(b"t/x", b"x", 0o644),
            E::Link(b"t/a/b/l", b"../../x"),
            E::Link(b"t/self", b"."),
            E::Global,
        ]);
        assert!(matches!(
            rig.run(body.clone(), install(&body)).0,
            JobOutcome::Succeeded(_)
        ));
    }

    #[test]
    fn caps_are_counted_while_extracting() {
        let mut many = vec![E::Dir(b"t/")];
        many.extend((0..60).map(|_| E::Global));
        assert_eq!(refused_archive(&many), "archive_too_large", "entry count");
        assert_eq!(
            refused_archive(&[E::Dir(b"t/"), E::Claims(b"t/big", 5 << 30)]),
            "archive_too_large",
            "declared size"
        );
        let big = vec![7u8; (1 << 20) + 1];
        assert_eq!(
            refused_archive(&[E::Dir(b"t/"), E::File(b"t/big", &big, 0o644)]),
            "archive_too_large",
            "bytes"
        );
    }

    #[test]
    fn the_vdf_decides_the_name() {
        let run = |vdf_body: Vec<u8>, expected: Value| {
            let rig = Rig::new();
            let body = tgz(&[
                E::Dir(b"t/"),
                E::File(b"t/compatibilitytool.vdf", &vdf_body, 0o644),
            ]);
            let mut p = install(&body);
            p["expected_name"] = expected;
            reason(&rig.run(body, p).0)
        };
        assert_eq!(
            run(vdf(NAME, "      \"commandline\" \"x\"\n"), Value::Null),
            "vdf_invalid"
        );
        assert_eq!(run(vdf("../evil", ""), Value::Null), "vdf_invalid");
        assert_eq!(run(vdf("proton_9", ""), Value::Null), "name_reserved");
        assert_eq!(run(vdf("Proton-GE", ""), Value::Null), "name_reserved");
        assert_eq!(
            run(vdf("GE-Proton11-13-x86_64", ""), json!(NAME)),
            "name_mismatch"
        );
        let rig = Rig::new();
        let body = tgz(&[E::Dir(b"t/"), E::File(b"t/other.txt", b"x", 0o644)]);
        assert_eq!(
            reason(&rig.run(body.clone(), install(&body)).0),
            "vdf_invalid",
            "missing vdf"
        );
        let rig = Rig::new();
        let body = tgz(&[
            E::Dir(b"t/"),
            E::File(b"t/x", &vdf(NAME, ""), 0o644),
            E::Link(b"t/compatibilitytool.vdf", b"x"),
        ]);
        assert_eq!(
            reason(&rig.run(body.clone(), install(&body)).0),
            "vdf_invalid",
            "a linked vdf"
        );
    }

    #[test]
    fn a_name_already_in_the_tools_directory_is_a_conflict() {
        let rig = Rig::new();
        std::fs::create_dir(rig.store.tools_dir().join(NAME)).unwrap();
        let body = good_tool();
        assert_eq!(
            reason(&rig.run(body.clone(), install(&body)).0),
            "name_conflict"
        );
        assert!(rig.store.installed().is_empty());
        assert_eq!(rig.leftovers(), 0);
    }

    fn remove() -> Value {
        json!({ "action": "remove", "tool_id": "6f1c", "name": NAME, "sha512": sha(&good_tool()) })
    }

    #[test]
    fn a_removal_waits_for_live_sessions_but_not_parked_ones() {
        let rig = Rig::new();
        let body = good_tool();
        rig.run(body.clone(), install(&body));
        rig.store.hold("parked", vec![NAME.into()], true);
        rig.store.hold("play", vec![NAME.into()], false);
        assert_eq!(
            rig.run(vec![], remove()).0,
            JobOutcome::Deferred("in_use".into())
        );
        rig.store.release("play");
        rig.store.adopt("parked", "adopted", false);
        assert_eq!(
            rig.run(vec![], remove()).0,
            JobOutcome::Deferred("in_use".into()),
            "adoption keeps the hold"
        );
        rig.store.release("adopted");
        let (out, _) = rig.run(vec![], remove());
        assert!(matches!(out, JobOutcome::Succeeded(_)), "{out:?}");
        assert!(rig.tools().is_empty() && rig.leftovers() == 0);
        assert!(rig.reopen().installed().is_empty());
    }

    #[test]
    fn a_removal_touches_only_what_the_record_holds() {
        let rig = Rig::new();
        std::fs::create_dir(rig.store.tools_dir().join(NAME)).unwrap();
        let (out, _) = rig.run(vec![], remove());
        let JobOutcome::Succeeded(s) = &out else {
            panic!("{out:?}")
        };
        assert_eq!(s["bytes"], 0);
        assert_eq!(
            rig.tools(),
            vec![NAME],
            "a directory the record does not hold stays"
        );
        let mut p = remove();
        p["name"] = json!("../tools");
        assert_eq!(reason(&rig.run(vec![], p).0), "invalid_params");
    }

    #[test]
    fn a_killed_run_is_finished_or_rolled_back_on_restart() {
        let rig = Rig::new();
        let body = good_tool();
        rig.run(body.clone(), install(&body));
        // A run killed mid-extract leaves its temporary tree.
        let left = rig.dir.path().join("staging/run-killed/tree/t");
        std::fs::create_dir_all(&left).unwrap();
        std::fs::File::create(left.join("half"))
            .unwrap()
            .write_all(b"x")
            .unwrap();
        // One killed after the record said installing and before the rename, one after it.
        let mut rec = rig.store.record_file().load().unwrap().unwrap();
        let entry = |name: &str, state| RecordEntry {
            name: name.into(),
            tag: TAG.into(),
            sha512: "b".repeat(128),
            bytes: 1,
            state,
        };
        rec.tools
            .push(entry("GE-Proton9-1", EntryState::Installing));
        rec.tools
            .push(entry("GE-Proton9-2", EntryState::Installing));
        std::fs::create_dir(rig.store.tools_dir().join("GE-Proton9-2")).unwrap();
        // And a removal killed before its move.
        rec.tools.retain(|e| e.name != NAME);
        rec.tools.push(RecordEntry {
            state: EntryState::Removing,
            ..rig.store.installed()[0].clone()
        });
        rig.store.record_file().store(&rec).unwrap();

        let again = rig.reopen();
        assert_eq!(rig.leftovers(), 0, "the leftover tree is removed");
        let names: Vec<String> = again.installed().into_iter().map(|e| e.name).collect();
        assert_eq!(names, vec!["GE-Proton9-2".to_string()]);
        assert_eq!(rig.tools(), vec!["GE-Proton9-2".to_string()]);
        let mode = std::fs::metadata(again.tools_dir().join("GE-Proton9-2"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777;
        assert_eq!(mode, 0o555, "the finished install is read-only");
    }

    /// The offline check a release passes before it goes on the recommended list (#1071):
    /// `QUASAR_COMPAT_TARBALL=<path> QUASAR_COMPAT_TAG=<tag> QUASAR_COMPAT_SHA512=<pin>
    /// QUASAR_COMPAT_NAME=<steam name> cargo test -- --ignored a_release_passes`.
    #[test]
    #[ignore]
    fn a_release_passes_the_install_checks() {
        struct FileSource(String, String);
        impl Source for FileSource {
            fn get(&self, url: &str) -> Result<Hop, HopError> {
                if url == self.1 {
                    return Ok(Hop {
                        status: 302,
                        location: Some(CDN.into()),
                        content_encoding: None,
                        body: Box::new(std::io::empty()),
                    });
                }
                let f = std::fs::File::open(&self.0).map_err(|e| HopError::Other(e.to_string()))?;
                Ok(Hop {
                    status: 200,
                    location: None,
                    content_encoding: None,
                    body: Box::new(f),
                })
            }
        }
        let var = |k: &str| std::env::var(k).unwrap_or_else(|_| panic!("set {k}"));
        let (path, tag, sha, name) = (
            var("QUASAR_COMPAT_TARBALL"),
            var("QUASAR_COMPAT_TAG"),
            var("QUASAR_COMPAT_SHA512"),
            var("QUASAR_COMPAT_NAME"),
        );
        let size = std::fs::metadata(&path).unwrap().len();
        let asset = if path.ends_with("-x86_64.tar.gz") {
            "x86_64"
        } else {
            "plain"
        };
        let url = fetch::asset_url(&tag, &fetch::asset_name(&tag, asset).unwrap());
        let rig = Rig::new();
        let r = ConvergeRunner::new(
            rig.store.clone(),
            Arc::new(FileSource(path, url)),
            Arc::new(|| {}),
        );
        let p = json!({ "action": "install", "tool_id": null, "tag": tag, "asset": asset, "size_bytes": size,
                        "sha512": sha, "expected_name": name, "reserve_bytes": 1 });
        let out = r.run(&p, &AbortFlag::new());
        let JobOutcome::Succeeded(s) = &out else {
            panic!("{out:?}")
        };
        eprintln!(
            "PASS {tag}: name {} sha512 {} size_bytes {size} unpacked {}",
            s["name"], s["sha512"], s["bytes"]
        );
    }

    #[test]
    fn a_root_that_overlaps_the_homes_is_not_usable() {
        let dir = tempfile::tempdir().unwrap();
        let inside = dir.path().join("homes/compat");
        std::fs::create_dir_all(&inside).unwrap();
        assert!(ToolStore::open(&inside, &[&dir.path().join("homes")], false).is_err());
        assert!(ToolStore::open(&dir.path().join("missing"), &[], false).is_err());
        assert!(ToolStore::open(Path::new("relative"), &[], false).is_err());
        assert!(
            ToolStore::open(dir.path(), &[], true).is_err(),
            "a temp dir is not a bind mount"
        );
    }
}
