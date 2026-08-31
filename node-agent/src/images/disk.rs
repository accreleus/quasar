//! Pre-pull/-build disk-space guard: fail fast with a readable `image_state` error
//! rather than letting a full docker filesystem kill the pull obscurely
//! (agent-api.md image-management P2 amendment).
//!
//! `statvfs` the daemon's own data root (`docker info --format '{{.DockerRootDir}}'`),
//! which stays correct when the agent is a sibling container whose filesystem is
//! unrelated. Not `docker system df`: that reports image SIZE usage, not free headroom.
//! Fail-open: if the path is not visible from this mount namespace the guard logs and
//! lets the pull proceed — "cannot tell" must never block an operator's dev box.

use crate::session::container::ContainerRuntime;

/// Conservative floor: never attempt a pull with less than this much free,
/// regardless of the (often unknown) image size.
pub const MIN_FREE_MB: u64 = 2048;

/// Floor for a template build: heavier than a pull (context + every intermediate
/// layer + the final image), so higher than [`MIN_FREE_MB`].
pub const MIN_BUILD_FREE_MB: u64 = 4096;

fn docker_root_dir(runtime: &ContainerRuntime) -> Option<String> {
    let out = runtime
        .run_raw(&["info", "--format", "{{.DockerRootDir}}"])
        .ok()?;
    let p = out.trim().to_string();
    if p.is_empty() {
        None
    } else {
        Some(p)
    }
}

/// Available space at `path` in MiB; `None` on any failure.
fn free_space_mb(path: &str) -> Option<u64> {
    let c = std::ffi::CString::new(path).ok()?;
    // SAFETY: `buf` is zero-initialized and only written by `statvfs`; `c` is a
    // valid NUL-terminated C string.
    unsafe {
        let mut buf: libc::statvfs = std::mem::zeroed();
        let rc = libc::statvfs(c.as_ptr(), &mut buf);
        if rc != 0 {
            return None;
        }
        // `f_bavail`/`f_frsize` are already `u64` on this target (a cast trips clippy
        // `unnecessary_cast`); a narrower-field target would need `TryFrom`.
        let free_bytes = buf.f_bavail.saturating_mul(buf.f_frsize);
        Some(free_bytes / (1024 * 1024))
    }
}

/// Pure verdict over resolved numbers; the unit-testable half.
pub fn verdict(free_mb: u64, known_image_mb: Option<u64>) -> Result<(), String> {
    let floor = MIN_FREE_MB + known_image_mb.unwrap_or(0);
    if free_mb < floor {
        Err(format!(
            "insufficient disk: {free_mb} MiB free (need at least {floor} MiB)"
        ))
    } else {
        Ok(())
    }
}

/// Free space at the daemon's data root in MiB; `None` drives the shared
/// "cannot tell, don't block" degradation in [`check`] and [`check_build`].
fn resolve_free_mb(runtime: &ContainerRuntime) -> Option<u64> {
    let root = docker_root_dir(runtime)?;
    match free_space_mb(&root) {
        Some(mb) => Some(mb),
        None => {
            tracing::debug!("disk guard: statvfs({root}) failed; skipping check");
            None
        }
    }
}

/// The pre-pull guard. Any resolution failure fails open (logged, never fatal).
pub fn check(runtime: &ContainerRuntime, known_image_mb: Option<u64>) -> Result<(), String> {
    let Some(free_mb) = resolve_free_mb(runtime) else {
        tracing::debug!("disk guard: could not resolve free space; skipping check");
        return Ok(());
    };
    verdict(free_mb, known_image_mb)
}

/// The pre-build guard: same fail-open as [`check`], against [`MIN_BUILD_FREE_MB`]
/// expressed as a delta in [`verdict`]'s `known_image_mb` slot so both share one rule.
pub fn check_build(runtime: &ContainerRuntime) -> Result<(), String> {
    let Some(free_mb) = resolve_free_mb(runtime) else {
        tracing::debug!("disk guard: could not resolve free space; skipping build check");
        return Ok(());
    };
    verdict(free_mb, Some(MIN_BUILD_FREE_MB - MIN_FREE_MB))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn passes_with_ample_headroom() {
        assert!(verdict(100_000, None).is_ok());
    }

    #[test]
    fn fails_below_floor() {
        let err = verdict(1_000, None).unwrap_err();
        assert!(err.contains("insufficient disk"));
        assert!(err.contains("1000 MiB free"));
    }

    #[test]
    fn passes_exactly_at_the_floor() {
        assert!(verdict(MIN_FREE_MB, None).is_ok());
        assert!(verdict(MIN_FREE_MB - 1, None).is_err());
    }

    #[test]
    fn accounts_for_known_image_size() {
        // 3000 MiB free, floor 2048 + 2000 image = 4048 -> fails.
        assert!(verdict(3000, Some(2000)).is_err());
        // 5000 MiB free, same floor -> passes.
        assert!(verdict(5000, Some(2000)).is_ok());
    }

    #[test]
    fn build_floor_is_higher_than_the_pull_floor() {
        let build_delta = Some(MIN_BUILD_FREE_MB - MIN_FREE_MB);
        assert!(verdict(MIN_BUILD_FREE_MB, build_delta).is_ok());
        assert!(verdict(MIN_BUILD_FREE_MB - 1, build_delta).is_err());
        // A pull would have been allowed at this level; a build is not.
        assert!(verdict(MIN_FREE_MB, None).is_ok());
        assert!(verdict(MIN_FREE_MB, build_delta).is_err());
    }

    #[test]
    fn statvfs_on_a_real_path_resolves_something() {
        // Value is host-dependent; this only proves the syscall path works.
        assert!(free_space_mb("/").is_some());
    }

    #[test]
    fn statvfs_on_a_nonexistent_path_is_none() {
        assert!(free_space_mb("/this/path/does/not/exist/quasar-test").is_none());
    }
}
