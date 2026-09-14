//! Fixed operator-facing failures; engine output and credentials never reach the wire.
use crate::runtime::{ErrorKind, RuntimeError};

pub const BUILD_FALLBACK: &str = "docker build failed; inspect node-agent logs";

pub fn map_runtime_error(error: RuntimeError) -> String {
    match error.kind {
        ErrorKind::UnknownOutcome => {
            "image operation outcome unknown; reconcile engine state before retrying"
        }
        ErrorKind::InvalidBuildContext => "invalid build context or dockerfile",
        ErrorKind::BuildFailed => BUILD_FALLBACK,
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
