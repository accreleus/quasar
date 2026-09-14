//! Fixed operator-facing failures; engine output and credentials never reach the wire.
use crate::runtime::{ErrorKind, RuntimeError};

pub const BUILD_FALLBACK: &str = "docker build failed; inspect node-agent logs";

pub fn map_runtime_error(error: RuntimeError) -> String {
    match error.kind {
        ErrorKind::UnknownOutcome => {
            "image operation outcome unknown; reconcile engine state before retrying"
        }
        ErrorKind::ImageInUse => "image in use",
        ErrorKind::RegistryDenied => "registry auth denied",
        ErrorKind::ManifestMissing => "manifest not found",
        ErrorKind::InsufficientDisk => "insufficient disk",
        ErrorKind::PermissionDenied => "engine access denied",
        ErrorKind::Unavailable => "engine unavailable",
        ErrorKind::Timeout => "engine request timed out",
        ErrorKind::Busy => "runtime busy; retry later",
        ErrorKind::InvalidConfiguration => "invalid runtime configuration",
        ErrorKind::IncompatibleApi => "incompatible engine API",
        _ => "image operation failed; inspect node-agent logs",
    }
    .to_string()
}

/// Map a failed `docker build`'s output into a short cause. Unrecognized failures
/// must never echo the raw build log (agent-api.md `image_build`) — it can carry the
/// Dockerfile, registry hostnames and secrets echoed by a `RUN`.
pub fn map_build_error(raw: &str) -> String {
    let lower = raw.to_lowercase();
    if lower.contains("no space left on device") {
        "insufficient disk".to_string()
    } else {
        BUILD_FALLBACK.to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn maps_build_disk_full() {
        assert_eq!(
            map_build_error("failed to solve: write /var/lib/docker/x: no space left on device"),
            "insufficient disk"
        );
    }

    #[test]
    fn unknown_build_errors_never_leak_the_raw_build_log() {
        assert_eq!(
            map_build_error(
                "Step 3/5 : RUN false\nThe command returned a non-zero code: 1\nsecret=hunter2"
            ),
            BUILD_FALLBACK
        );
        assert!(!map_build_error("secret=hunter2 in the log").contains("hunter2"));
        assert_eq!(map_build_error(""), BUILD_FALLBACK);
    }
}
