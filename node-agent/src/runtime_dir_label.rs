//! The agent's runtime directory carries the container SELinux type.
//!
//! `/run/quasar-agent` holds the sockets the agent shares with its sessions and sidecars
//! (Wayland, PulseAudio). When the engine creates that bind source itself, as a rootful
//! Podman does on a fresh boot, it is labelled `container_var_run_t`, which a confined
//! container may not write: the audio sidecar then fails with "Failed to create secure
//! directory … Permission denied". `container_file_t` is the type a container may use,
//! the one an engine's own `:z` relabel gives a bind source.
//!
//! The agent runs unconfined (`label=disable`), so it sets that type on its own directory
//! at start, keeping the user, role and level. It changes nothing else and nothing on a
//! host without SELinux labels: there the directory has no `security.selinux` attribute.

use std::ffi::CString;
use std::os::unix::ffi::OsStrExt;
use std::path::Path;

/// The SELinux type a container may read and write.
pub const CONTAINER_FILE_TYPE: &str = "container_file_t";

const XATTR: &[u8] = b"security.selinux\0";

/// What `ensure` did.
#[derive(Debug, PartialEq, Eq)]
pub enum Outcome {
    /// The host has no SELinux label on the directory: nothing to do.
    Unlabelled,
    /// The directory already carries the container type.
    AlreadyLabelled,
    /// The type was changed from the first context to the second.
    Relabelled { from: String, to: String },
}

/// `context` with its type replaced by `new_type`, or `None` when it already has it or
/// is not a `user:role:type[:level]` context.
pub fn retype(context: &str, new_type: &str) -> Option<String> {
    let mut parts: Vec<&str> = context.splitn(4, ':').collect();
    if parts.len() < 3 || parts[2] == new_type {
        return None;
    }
    parts[2] = new_type;
    Some(parts.join(":"))
}

/// Give `dir` the container type if it carries an SELinux label of another type.
pub fn ensure(dir: &Path) -> std::io::Result<Outcome> {
    let path = CString::new(dir.as_os_str().as_bytes())
        .map_err(|_| std::io::Error::from(std::io::ErrorKind::InvalidInput))?;
    let mut buf = [0u8; 512];
    // SAFETY: path and the attribute name are NUL-terminated; buf is valid for its length.
    let n = unsafe {
        libc::getxattr(
            path.as_ptr(),
            XATTR.as_ptr().cast(),
            buf.as_mut_ptr().cast(),
            buf.len(),
        )
    };
    if n < 0 {
        let err = std::io::Error::last_os_error();
        return match err.raw_os_error() {
            Some(libc::ENODATA) | Some(libc::ENOTSUP) => Ok(Outcome::Unlabelled),
            _ => Err(err),
        };
    }
    let current = String::from_utf8_lossy(&buf[..n as usize])
        .trim_end_matches('\0')
        .to_string();
    let Some(next) = retype(&current, CONTAINER_FILE_TYPE) else {
        return Ok(Outcome::AlreadyLabelled);
    };
    let value = CString::new(next.clone())
        .map_err(|_| std::io::Error::from(std::io::ErrorKind::InvalidData))?;
    let bytes = value.as_bytes_with_nul();
    // SAFETY: as above; value is NUL-terminated and its length includes the NUL, as the
    // kernel expects for security.selinux.
    let rc = unsafe {
        libc::setxattr(
            path.as_ptr(),
            XATTR.as_ptr().cast(),
            bytes.as_ptr().cast(),
            bytes.len(),
            0,
        )
    };
    if rc != 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(Outcome::Relabelled {
        from: current,
        to: next,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_the_type_changes() {
        assert_eq!(
            retype("system_u:object_r:container_var_run_t:s0", CONTAINER_FILE_TYPE).as_deref(),
            Some("system_u:object_r:container_file_t:s0")
        );
        // An MLS level with categories survives whole.
        assert_eq!(
            retype("system_u:object_r:var_run_t:s0:c1,c2", CONTAINER_FILE_TYPE).as_deref(),
            Some("system_u:object_r:container_file_t:s0:c1,c2")
        );
    }

    #[test]
    fn an_already_container_typed_or_malformed_context_is_left_alone() {
        assert_eq!(
            retype("system_u:object_r:container_file_t:s0", CONTAINER_FILE_TYPE),
            None
        );
        assert_eq!(retype("unlabeled", CONTAINER_FILE_TYPE), None);
    }

    #[test]
    fn ensure_succeeds_on_a_plain_directory_and_is_idempotent() {
        // The test filesystem may or may not carry SELinux labels; whichever it is, a
        // second call has nothing left to change.
        let dir = tempfile::tempdir().unwrap();
        ensure(dir.path()).expect("first call");
        assert_ne!(
            std::mem::discriminant(&ensure(dir.path()).expect("second call")),
            std::mem::discriminant(&Outcome::Relabelled {
                from: String::new(),
                to: String::new()
            })
        );
    }
}
