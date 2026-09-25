//! The one error vocabulary every engine operation reports in.

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ErrorKind {
    InvalidConfiguration,
    PermissionDenied,
    Missing,
    Unavailable,
    IncompatibleApi,
    Protocol,
    Engine,
    Timeout,
    Cancelled,
    Busy,
    UnknownOutcome,
    ImageInUse,
    RegistryDenied,
    ManifestMissing,
    InsufficientDisk,
    InvalidBuildContext,
    BuildFailed,
}

/// Safe to surface to callers. Raw daemon messages never become public errors.
#[derive(Debug, Clone)]
pub struct RuntimeError {
    pub kind: ErrorKind,
    /// A sanitized read-only observation that explains why a mutation outcome
    /// remains unknown. No daemon text or SDK type crosses this boundary.
    pub reconciliation: Option<ErrorKind>,
}
impl std::fmt::Display for RuntimeError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "container runtime: {:?}", self.kind)?;
        if let Some(reconciliation) = self.reconciliation {
            write!(f, " (reconciliation: {:?})", reconciliation)?;
        }
        Ok(())
    }
}
impl std::error::Error for RuntimeError {}
impl From<ErrorKind> for RuntimeError {
    fn from(kind: ErrorKind) -> Self {
        Self {
            kind,
            reconciliation: None,
        }
    }
}
