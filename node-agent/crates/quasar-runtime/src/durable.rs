//! Durable-state primitives: an atomically replaced file and a process lease.

use serde::{de::DeserializeOwned, Serialize};
use std::{
    fs::{self, File, OpenOptions},
    io::{self, Write},
    marker::PhantomData,
    os::{fd::AsRawFd, unix::fs::OpenOptionsExt},
    path::{Path, PathBuf},
};

/// A JSON value committed to one file so a reader only ever sees a whole value.
pub struct DurableFile<T> {
    path: PathBuf,
    temp: PathBuf,
    mode: Option<u32>,
    _value: PhantomData<fn(&T) -> T>,
}
impl<T> DurableFile<T> {
    /// The committed file at `path`, staged through `path.with_extension(temp_extension)`.
    pub fn new(path: impl Into<PathBuf>, temp_extension: &str) -> Self {
        let path = path.into();
        let temp = path.with_extension(temp_extension);
        Self {
            path,
            temp,
            mode: None,
            _value: PhantomData,
        }
    }
    /// Commit with these permission bits (e.g. `0o600` for a secret), whatever the
    /// process umask. Without it the temp is created under the umask, as before.
    pub fn with_mode(mut self, mode: u32) -> Self {
        self.mode = Some(mode);
        self
    }
    /// The committed file's path.
    pub fn path(&self) -> &Path {
        &self.path
    }
    #[cfg(test)]
    fn temp_path(&self) -> &Path {
        &self.temp
    }
}
impl<T: Serialize> DurableFile<T> {
    /// Commit `value` as compact JSON: write the temp (created or truncated), fsync
    /// it, rename it over the committed file, then fsync the parent directory so the
    /// rename itself is durable. A crash at any point leaves the committed file
    /// holding either the previous value or this one, never a mix; a leftover temp
    /// is never read and the next store overwrites it.
    pub fn store(&self, value: &T) -> io::Result<()> {
        let bytes = serde_json::to_vec(value).map_err(io::Error::other)?;
        let mut options = OpenOptions::new();
        options.create(true).write(true).truncate(true);
        if let Some(mode) = self.mode {
            options.mode(mode);
        }
        let mut f = options.open(&self.temp)?;
        if let Some(mode) = self.mode {
            // A leftover temp keeps its old bits through `truncate`; set them explicitly.
            use std::os::unix::fs::PermissionsExt;
            f.set_permissions(fs::Permissions::from_mode(mode))?;
        }
        f.write_all(&bytes)?;
        f.sync_all()?;
        fs::rename(&self.temp, &self.path)?;
        if let Some(parent) = self.path.parent() {
            OpenOptions::new().read(true).open(parent)?.sync_all()?;
        }
        Ok(())
    }
}
impl<T: DeserializeOwned> DurableFile<T> {
    /// The committed value; `Ok(None)` when nothing was ever committed. A committed
    /// file that does not parse is [`io::ErrorKind::InvalidData`], never `None`.
    pub fn load(&self) -> io::Result<Option<T>> {
        let bytes = match fs::read(&self.path) {
            Ok(bytes) => bytes,
            Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(None),
            Err(err) => return Err(err),
        };
        serde_json::from_slice(&bytes)
            .map(Some)
            .map_err(|err| io::Error::new(io::ErrorKind::InvalidData, err))
    }
}

/// An exclusive, non-blocking `flock` on one state file, held until drop.
pub struct StateLease {
    file: File,
    created: bool,
}

/// Why a [`StateLease`] was not acquired.
#[derive(Debug)]
pub enum LeaseError {
    /// The lease file could not be created or opened (a symlink is refused here).
    Open(io::Error),
    /// The lock was refused: another holder has it. Carries the `flock` error.
    Held(io::Error),
}

impl StateLease {
    /// Create the lease file (mode 0600) or open the existing one, never following a
    /// symlink, then take `LOCK_EX | LOCK_NB`. The file is never renamed or unlinked:
    /// its inode is the lock, so every holder must agree on it.
    pub fn acquire(path: &Path) -> Result<Self, LeaseError> {
        let (file, created) = match OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW)
            .open(path)
        {
            Ok(file) => (file, true),
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => (
                OpenOptions::new()
                    .read(true)
                    .write(true)
                    .custom_flags(libc::O_NOFOLLOW)
                    .open(path)
                    .map_err(LeaseError::Open)?,
                false,
            ),
            Err(e) => return Err(LeaseError::Open(e)),
        };
        // SAFETY: `file` owns this live fd throughout the call and until the lease drops.
        if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
            return Err(LeaseError::Held(io::Error::last_os_error()));
        }
        Ok(Self { file, created })
    }
    /// Whether this acquisition created the lease file.
    pub fn created(&self) -> bool {
        self.created
    }
    /// The lease file, for reading and writing the state it guards.
    pub fn file(&self) -> &File {
        &self.file
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn journal(dir: &tempfile::TempDir) -> DurableFile<Vec<String>> {
        DurableFile::new(dir.path().join("journal.json"), "journal.tmp")
    }

    /// A store that died after writing its temp and before the rename leaves the temp
    /// behind, whole or torn. Neither shadows the committed value, and the next store
    /// still commits.
    #[test]
    fn a_leftover_temp_from_an_interrupted_store_never_shadows_the_committed_value() {
        let dir = tempfile::tempdir().unwrap();
        let file = journal(&dir);
        file.store(&vec!["old".to_string()]).unwrap();

        for leftover in [&br#"["new"]"#[..], &br#"["ne"#[..]] {
            std::fs::write(file.temp_path(), leftover).unwrap();
            assert_eq!(file.load().unwrap(), Some(vec!["old".to_string()]));
        }

        file.store(&vec!["new".to_string()]).unwrap();
        assert_eq!(file.load().unwrap(), Some(vec!["new".to_string()]));
        assert!(!file.temp_path().exists(), "the commit consumes the temp");
    }

    /// A store that fails before its rename (here the temp cannot even be opened)
    /// reports the error and leaves the committed value in place.
    #[test]
    fn a_store_that_cannot_commit_reports_it_and_keeps_the_committed_value() {
        let dir = tempfile::tempdir().unwrap();
        let file = journal(&dir);
        file.store(&vec!["old".to_string()]).unwrap();
        std::fs::create_dir(file.temp_path()).unwrap();

        assert!(file.store(&vec!["new".to_string()]).is_err());
        assert_eq!(file.load().unwrap(), Some(vec!["old".to_string()]));

        std::fs::remove_dir(file.temp_path()).unwrap();
        file.store(&vec!["new".to_string()]).unwrap();
        assert_eq!(file.load().unwrap(), Some(vec!["new".to_string()]));
    }

    /// A secret committed with a mode carries exactly those bits, even when a leftover
    /// temp from an earlier store had wider ones.
    #[test]
    fn a_mode_is_applied_to_the_committed_file_whatever_the_leftover_temp_had() {
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().unwrap();
        let file: DurableFile<String> =
            DurableFile::new(dir.path().join("secret.json"), "secret.tmp").with_mode(0o600);
        std::fs::write(file.temp_path(), b"old").unwrap();
        std::fs::set_permissions(file.temp_path(), std::fs::Permissions::from_mode(0o644)).unwrap();
        file.store(&"s3cret".to_string()).unwrap();
        let mode = std::fs::metadata(file.path()).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600);
        assert_eq!(file.load().unwrap().as_deref(), Some("s3cret"));
    }

    /// A second holder of a held lease is refused, whether it is this process opening
    /// the file again or another process; the lease is free again once released.
    #[test]
    fn a_held_lease_refuses_every_other_holder_until_released() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("owner");

        let first = StateLease::acquire(&path).unwrap();
        assert!(first.created());
        assert!(matches!(
            StateLease::acquire(&path),
            Err(LeaseError::Held(_))
        ));
        assert_eq!(
            lease_attempt_in_another_process(&path),
            Some(CHILD_HELD),
            "another process must be refused with Held"
        );

        drop(first);
        let again = reacquire(&path);
        assert!(!again.created(), "the lease file persists across holders");
    }

    /// flock belongs to the open file description, and a sibling test's fork copies it
    /// until that child's exec closes it, so a released lease can stay held briefly.
    fn reacquire(path: &Path) -> StateLease {
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        loop {
            match StateLease::acquire(path) {
                Err(LeaseError::Held(_)) if std::time::Instant::now() < deadline => {
                    std::thread::sleep(std::time::Duration::from_millis(10))
                }
                result => return result.expect("the released lease is free again"),
            }
        }
    }

    /// Exit codes of [`child_tries_the_lease`], one per outcome, so a child that fails
    /// for any other reason (panic, bad setup) can never read as a refusal.
    const CHILD_ACQUIRED: i32 = 10;
    const CHILD_HELD: i32 = 11;
    const CHILD_OPEN_FAILED: i32 = 12;

    /// Asks a child process to take the lease; its exit code.
    fn lease_attempt_in_another_process(path: &Path) -> Option<i32> {
        std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--ignored",
                "--exact",
                "durable::tests::child_tries_the_lease",
            ])
            .env("QUASAR_LEASE_TEST_PATH", path)
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()
            .unwrap()
            .code()
    }

    /// Child half of the cross-process check: exits with the code naming its outcome.
    #[test]
    #[ignore = "child process of a_held_lease_refuses_every_other_holder_until_released"]
    fn child_tries_the_lease() {
        let path = std::env::var_os("QUASAR_LEASE_TEST_PATH").unwrap();
        std::process::exit(match StateLease::acquire(Path::new(&path)) {
            Ok(_) => CHILD_ACQUIRED,
            Err(LeaseError::Held(_)) => CHILD_HELD,
            Err(LeaseError::Open(_)) => CHILD_OPEN_FAILED,
        });
    }
}
