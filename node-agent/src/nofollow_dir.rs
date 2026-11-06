//! Directory steps that never follow a symlink. Each step is relative to an open
//! directory, so a path a user swaps for a symlink after a check cannot redirect a
//! root agent's `mkdir` or `chown` out of the tree. For trees a user can write: the
//! managed homes (`session::home`). Opens use `O_NOFOLLOW`, as every other open of a
//! user-reachable path in the agent does.

use std::ffi::{CString, OsStr};
use std::fs::{File, OpenOptions};
use std::io;
use std::os::fd::{AsRawFd, FromRawFd};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

/// Opens `path` as a directory. Its last component must not be a symlink; the
/// components above it are the caller's (agent-owned) path.
pub(crate) fn open_dir(path: &Path) -> io::Result<File> {
    OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW)
        .open(path)
}

/// Opens the one entry `name` of `dir` as a directory. A symlink there fails with
/// `ELOOP`, any other non-directory with `ENOTDIR`.
pub(crate) fn open_dir_at(dir: &File, name: &OsStr) -> io::Result<File> {
    let name = entry_name(name)?;
    // SAFETY: `dir` is an open descriptor and `name` a NUL-terminated string, both
    // alive for the call; the returned descriptor is owned by the new File.
    let fd = unsafe {
        libc::openat(
            dir.as_raw_fd(),
            name.as_ptr(),
            libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC,
        )
    };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: `fd` was just returned by openat and nothing else owns it.
    Ok(unsafe { File::from_raw_fd(fd) })
}

/// Makes the one entry `name` of `dir`.
pub(crate) fn mkdir_at(dir: &File, name: &OsStr, mode: libc::mode_t) -> io::Result<()> {
    let name = entry_name(name)?;
    // SAFETY: as in `open_dir_at`; mkdirat never follows a symlink at `name`.
    if unsafe { libc::mkdirat(dir.as_raw_fd(), name.as_ptr(), mode) } != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

/// Whether the open directory `dir` has no entries, read through its own descriptor.
pub(crate) fn is_empty(dir: &File) -> io::Result<bool> {
    Ok(
        std::fs::read_dir(format!("/proc/self/fd/{}", dir.as_raw_fd()))?
            .next()
            .is_none(),
    )
}

/// True for the errors an `O_NOFOLLOW | O_DIRECTORY` open gives on a symlink or a
/// non-directory.
pub(crate) fn is_symlink_or_not_dir(e: &io::Error) -> bool {
    matches!(e.raw_os_error(), Some(libc::ELOOP) | Some(libc::ENOTDIR))
}

/// One plain entry name: no separator, not `.` or `..`.
fn entry_name(name: &OsStr) -> io::Result<CString> {
    let bytes = name.as_bytes();
    if bytes.is_empty() || bytes == b"." || bytes == b".." || bytes.contains(&b'/') {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "not a single directory entry name",
        ));
    }
    CString::new(bytes).map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_symlinked_entry_is_never_opened_or_made_through() {
        let d = tempfile::tempdir().unwrap();
        let outside = d.path().join("outside");
        std::fs::create_dir(&outside).unwrap();
        std::fs::create_dir(d.path().join("real")).unwrap();
        std::os::unix::fs::symlink(&outside, d.path().join("link")).unwrap();
        let dir = open_dir(d.path()).unwrap();

        assert!(open_dir_at(&dir, OsStr::new("real")).is_ok());
        let e = open_dir_at(&dir, OsStr::new("link")).unwrap_err();
        assert!(is_symlink_or_not_dir(&e), "{e}");
        assert!(mkdir_at(&dir, OsStr::new("link"), 0o755).is_err());
        assert!(open_dir(&d.path().join("link")).is_err());
        assert!(open_dir_at(&dir, OsStr::new("../x")).is_err());
        assert!(std::fs::read_dir(&outside).unwrap().next().is_none());
    }
}
