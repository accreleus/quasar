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
            _value: PhantomData,
        }
    }
    pub fn path(&self) -> &Path {
        &self.path
    }
    pub fn temp_path(&self) -> &Path {
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
        let mut f = OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(&self.temp)?;
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
        assert!(
            !lease_free_in_another_process(&path),
            "another process must be refused too"
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

    /// Asks a child process to take the lease; true when it could.
    fn lease_free_in_another_process(path: &Path) -> bool {
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
            .success()
    }

    /// Child half of the cross-process check: succeeds only if it acquires the lease.
    #[test]
    #[ignore = "child process of a_held_lease_refuses_every_other_holder_until_released"]
    fn child_tries_the_lease() {
        let path = std::env::var_os("QUASAR_LEASE_TEST_PATH").unwrap();
        StateLease::acquire(Path::new(&path)).unwrap();
    }
}
