//! The pre-update dumps kept in machine state (`CONTEXT.md` "Pre-update dump"; #364). Not
//! `crate::dump`, which is `uninstall --purge`'s final dump. `dumps/` holds each dump as
//! `<name>.dump` beside `<name>.json`, its record. A dump exists once its record does;
//! `pg_dump` writes `<name>.dump.partial` first, which is never read.
//!
//! Not a frozen interface: the name is opaque everywhere else (control-api.md
//! `pre_update_dump`), and only this module and the `restore` command read the files.

use std::io::{self, Read};
use std::path::{Path, PathBuf};

use quasar_runtime::DurableFile;
use serde::{Deserialize, Serialize};

use crate::recipe::ImageRef;
use crate::socket::Dump;

pub const FORMAT: u32 = 1;

/// How many pre-update dumps a machine keeps (#352 decision 14).
pub const KEEP: usize = 3;

/// A custom-format archive begins with these bytes (`pg_dump --format=custom`).
const MAGIC: &[u8] = b"PGDMP";

/// What a dump is: enough to refuse a corrupt or mismatched one before the database is
/// touched, and to start the control plane it belongs to.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DumpRecord {
    pub format: u32,
    pub name: String,
    /// The `schema_migrations` version inside the dump.
    pub schema_version: i64,
    pub created_at: String,
    pub size_bytes: i64,
    pub sha256: String,
    /// The attempt that took it.
    #[serde(default)]
    pub request_id: Option<String>,
    /// The control plane the database was on when the dump was taken: the one a restore of
    /// it starts.
    #[serde(default)]
    pub control_plane: Option<ImageRef>,
    #[serde(default)]
    pub recipe_revision: Option<u32>,
    /// The version a restore of this dump returns to, as the printed command's `--to`.
    #[serde(default)]
    pub returns_to: Option<String>,
}

impl DumpRecord {
    pub fn wire(&self) -> Dump {
        Dump {
            name: self.name.clone(),
            schema_version: self.schema_version,
            created_at: self.created_at.clone(),
            size_bytes: self.size_bytes,
        }
    }
}

/// `20260925T100000Z-schema-88`, with `-<8 hex>` when two dumps would share a second.
/// Anything else is refused before it names a file.
pub fn valid_name(name: &str) -> bool {
    let stamp = |s: &str| {
        let b = s.as_bytes();
        b.len() == 16
            && b[..8].iter().all(u8::is_ascii_digit)
            && b[8] == b'T'
            && b[9..15].iter().all(u8::is_ascii_digit)
            && b[15] == b'Z'
    };
    let Some((ts, rest)) = name.split_once("-schema-") else {
        return false;
    };
    let (schema, suffix) = match rest.split_once('-') {
        Some((s, x)) => (s, Some(x)),
        None => (rest, None),
    };
    stamp(ts)
        && !schema.is_empty()
        && schema.len() <= 9
        && schema.bytes().all(|b| b.is_ascii_digit())
        && suffix.is_none_or(|x| {
            x.len() == 8
                && x.bytes()
                    .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
        })
}

/// RFC 3339 `2026-09-25T10:00:00Z` as the compact stamp a name carries.
pub fn stamp(rfc3339: &str) -> String {
    rfc3339
        .chars()
        .filter(|c| c.is_ascii_digit() || *c == 'T' || *c == 'Z')
        .collect()
}

pub struct DumpDir {
    dir: PathBuf,
}

impl DumpDir {
    pub fn new(machine_root: &Path) -> Self {
        DumpDir {
            dir: machine_root.join("dumps"),
        }
    }

    pub fn path(&self) -> &Path {
        &self.dir
    }

    pub fn ensure(&self) -> io::Result<()> {
        use std::os::unix::fs::DirBuilderExt;
        match std::fs::DirBuilder::new().mode(0o700).create(&self.dir) {
            Ok(()) => match self.dir.parent() {
                Some(parent) => std::fs::File::open(parent)?.sync_all(),
                None => Ok(()),
            },
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => Ok(()),
            Err(e) => Err(e),
        }
    }

    pub fn file(&self, name: &str) -> PathBuf {
        self.dir.join(format!("{name}.dump"))
    }

    pub fn partial(&self, name: &str) -> PathBuf {
        self.dir.join(format!("{name}.dump.partial"))
    }

    fn record_file(&self, name: &str) -> DurableFile<DumpRecord> {
        DurableFile::new(self.dir.join(format!("{name}.json")), "json.tmp").with_mode(0o600)
    }

    /// `Ok(None)`: no complete dump of that name.
    pub fn load(&self, name: &str) -> io::Result<Option<DumpRecord>> {
        if !valid_name(name) || !self.dir.join(format!("{name}.json")).exists() {
            return Ok(None);
        }
        let record = self.record_file(name).load()?;
        match record {
            Some(r) if r.format != FORMAT => Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!(
                    "dump record {name} is format {}, this actor reads {FORMAT}",
                    r.format
                ),
            )),
            other => Ok(other),
        }
    }

    pub fn store(&self, record: &DumpRecord) -> io::Result<()> {
        self.ensure()?;
        self.record_file(&record.name).store(record)
    }

    /// Every complete dump, newest first. A record that does not read is skipped: it is
    /// no way back, and its file is not removed by anything but [`DumpDir::prune`].
    pub fn list(&self) -> Vec<DumpRecord> {
        let Ok(entries) = std::fs::read_dir(&self.dir) else {
            return Vec::new();
        };
        let mut out: Vec<DumpRecord> = entries
            .flatten()
            .filter_map(|e| e.file_name().into_string().ok())
            .filter_map(|n| n.strip_suffix(".json").map(str::to_owned))
            .filter_map(|n| self.load(&n).ok().flatten())
            .filter(|r| self.file(&r.name).exists())
            .collect();
        out.sort_by(|a, b| (&b.created_at, &b.name).cmp(&(&a.created_at, &a.name)));
        out
    }

    /// The complete pre-update dump the attempt `request_id` took, if any.
    pub fn taken_by(&self, request_id: &str) -> Option<DumpRecord> {
        self.list()
            .into_iter()
            .find(|r| r.request_id.as_deref() == Some(request_id))
    }

    /// Removes the dump and its record; missing files are not an error.
    pub fn remove(&self, name: &str) -> io::Result<()> {
        for path in [
            self.dir.join(format!("{name}.json")),
            self.file(name),
            self.partial(name),
        ] {
            match std::fs::remove_file(&path) {
                Ok(()) => {}
                Err(e) if e.kind() == io::ErrorKind::NotFound => {}
                Err(e) => return Err(e),
            }
        }
        Ok(())
    }

    /// Every `.partial`: a dump a crash stopped half-way.
    pub fn remove_partials(&self) -> io::Result<()> {
        let Ok(entries) = std::fs::read_dir(&self.dir) else {
            return Ok(());
        };
        for e in entries.flatten() {
            if e.file_name().to_string_lossy().ends_with(".dump.partial") {
                std::fs::remove_file(e.path())?;
            }
        }
        Ok(())
    }

    /// Keeps the newest [`KEEP`] pre-update dumps; `keep` is never removed.
    pub fn prune(&self, keep: &str) -> io::Result<Vec<String>> {
        let mut removed = Vec::new();
        for r in self.list().into_iter().skip(KEEP) {
            if r.name != keep {
                self.remove(&r.name)?;
                removed.push(r.name);
            }
        }
        Ok(removed)
    }

    /// `rename(partial, file)`, durable once it returns.
    pub fn complete(&self, name: &str) -> io::Result<()> {
        std::fs::File::open(self.partial(name))?.sync_all()?;
        std::fs::rename(self.partial(name), self.file(name))?;
        std::fs::File::open(&self.dir)?.sync_all()
    }
}

/// The file's size and sha256, and whether it starts like a custom-format archive.
pub fn examine(path: &Path) -> io::Result<(i64, String, bool)> {
    let mut f = std::fs::File::open(path)?;
    let mut ctx = ring::digest::Context::new(&ring::digest::SHA256);
    let mut buf = vec![0u8; 1 << 16];
    let mut size: i64 = 0;
    let mut head = Vec::new();
    loop {
        let n = f.read(&mut buf)?;
        if n == 0 {
            break;
        }
        if head.len() < MAGIC.len() {
            head.extend_from_slice(&buf[..n.min(MAGIC.len() - head.len())]);
        }
        ctx.update(&buf[..n]);
        size += n as i64;
    }
    let hex = ctx
        .finish()
        .as_ref()
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect();
    Ok((size, hex, head == MAGIC))
}

/// What the pre-update dump of a database this size may need: the database's own size (a
/// compressed custom-format dump is smaller, so this over-asks) plus a tenth, at least
/// 64 MiB. The Go twin is `platform.dumpSpaceNeeded`; keep the two equal.
pub fn space_needed(database_bytes: u64) -> u64 {
    database_bytes + (database_bytes / 10).max(64 << 20)
}

/// "1.4 GB", as operator prose reads a size.
pub fn human(bytes: u64) -> String {
    const GB: f64 = 1_000_000_000.0;
    const MB: f64 = 1_000_000.0;
    let b = bytes as f64;
    if b >= GB {
        format!("{:.1} GB", b / GB)
    } else {
        format!("{:.0} MB", (b / MB).max(1.0))
    }
}

/// Free bytes on the filesystem holding `path`, for an unprivileged writer.
pub fn free_bytes(path: &Path) -> io::Result<u64> {
    use std::os::unix::ffi::OsStrExt;
    let c = std::ffi::CString::new(path.as_os_str().as_bytes())
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "a path with a NUL byte"))?;
    let mut st: libc::statvfs = unsafe { std::mem::zeroed() };
    // SAFETY: `c` is a valid NUL-terminated path and `st` a writable statvfs.
    if unsafe { libc::statvfs(c.as_ptr(), &mut st) } != 0 {
        return Err(io::Error::last_os_error());
    }
    // The field types differ between libc targets.
    #[allow(clippy::unnecessary_cast)]
    Ok(st.f_bavail as u64 * st.f_frsize as u64)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_the_two_name_forms_are_accepted() {
        for ok in [
            "20260925T100000Z-schema-88",
            "20260925T100000Z-schema-88-7a1f6f1e",
        ] {
            assert!(valid_name(ok), "{ok}");
        }
        for bad in [
            "",
            "../x",
            "20260925T100000Z-schema-",
            "20260925T100000Z-schema-88.dump",
            "20260925T100000Z-schema-88-7A1F6F1E",
            "20260925T1000Z-schema-88",
            "import-20260925T100000Z",
            "20260925T100000Z-schema-88/../../secrets",
        ] {
            assert!(!valid_name(bad), "{bad}");
        }
        assert_eq!(stamp("2026-09-25T10:00:00Z"), "20260925T100000Z");
    }

    #[test]
    fn the_space_rule_asks_for_the_database_and_a_margin() {
        assert_eq!(space_needed(0), 64 << 20);
        assert_eq!(space_needed(10_000_000_000), 11_000_000_000);
        assert_eq!(human(1_400_000_000), "1.4 GB");
        assert_eq!(human(600_000), "1 MB");
    }
}
