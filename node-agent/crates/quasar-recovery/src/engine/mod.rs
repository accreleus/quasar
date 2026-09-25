//! The engine port (architecture §5.1): every container-engine call the recovery actor
//! makes, behind one trait with two adapters, [`DockerEngine`] over `quasar-runtime`'s
//! facade and `FakeEngine` in memory with fault and crash injection (feature
//! `test-support`, which only this crate's tests enable: the binary never carries it).
//!
//! Blocking by design: the actor is a single-purpose process, and each real call bridges
//! the runtime client's bounded executor.

mod docker;
#[cfg(any(test, feature = "test-support"))]
mod fake;

use std::collections::BTreeMap;
use std::time::Duration;

pub use docker::DockerEngine;
#[cfg(any(test, feature = "test-support"))]
pub use fake::{
    Behaviour, FakeContainer, FakeEngine, FakeState, FakeVolume, Fault, Lifecycle, When,
};
pub use quasar_runtime::platform::{
    ContainerSpec, EngineHost, PlatformContainer as Container, PlatformImage as Image,
    PlatformVolume as Volume, RestartPolicy,
};
pub use quasar_runtime::ErrorKind;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EngineError {
    /// The engine answered with (or failed to answer with) this classified error.
    Runtime(ErrorKind),
    /// The engine definitely refused this create or start, in its own words.
    Refused { status: u16, message: String },
    /// `FakeEngine` only: the actor process "died" at this call.
    Crashed,
}

/// Docker's answers to a GPU device request it cannot meet, lowercased: without CDI
/// `could not select device driver "" with capabilities: [[gpu]]`; with CDI enabled
/// (Docker 28+, measured on 29.8 as HTTP 500 at start) `failed to discover GPU vendor from
/// CDI: no known GPU vendor found`.
const DEVICE_REQUEST_REFUSALS: &[&str] = &[
    "could not select device driver",
    "failed to discover gpu vendor from cdi",
];

impl EngineError {
    /// The engine refused a GPU device request: the one definite "no GPUs" answer.
    pub fn is_device_request_refusal(&self) -> bool {
        match self {
            EngineError::Refused { message, .. } => {
                let message = message.to_lowercase();
                DEVICE_REQUEST_REFUSALS.iter().any(|r| message.contains(r))
            }
            _ => false,
        }
    }

    /// Worth asking again: the engine did not answer, or failed on its side for a reason
    /// other than refusing the request.
    pub fn is_transient(&self) -> bool {
        match self {
            EngineError::Runtime(kind) => matches!(
                kind,
                ErrorKind::Unavailable
                    | ErrorKind::Timeout
                    | ErrorKind::UnknownOutcome
                    | ErrorKind::Busy
            ),
            EngineError::Refused { status, .. } => {
                *status >= 500 && !self.is_device_request_refusal()
            }
            EngineError::Crashed => false,
        }
    }

    /// The engine itself could not be reached, as opposed to refusing one request.
    pub fn is_unreachable(&self) -> bool {
        matches!(
            self,
            EngineError::Runtime(
                ErrorKind::Unavailable
                    | ErrorKind::PermissionDenied
                    | ErrorKind::IncompatibleApi
                    | ErrorKind::InvalidConfiguration
                    | ErrorKind::Timeout
            )
        )
    }
}

impl std::fmt::Display for EngineError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            EngineError::Runtime(kind) => f.write_str(quasar_runtime::platform::describe(
                &quasar_runtime::RuntimeError::from(*kind),
            )),
            EngineError::Refused { status, message } => {
                write!(f, "the engine refused it (HTTP {status}): {message}")
            }
            EngineError::Crashed => f.write_str("crash injected"),
        }
    }
}

impl std::error::Error for EngineError {}

impl From<quasar_runtime::RuntimeError> for EngineError {
    fn from(e: quasar_runtime::RuntimeError) -> Self {
        EngineError::Runtime(e.kind)
    }
}

/// Every mutation is addressed by container id; names are only ever looked up.
pub trait PlatformEngine: Send + Sync {
    fn host(&self) -> Result<EngineHost, EngineError>;
    /// `Ok(None)`: no such image locally.
    fn inspect_image(&self, reference: &str) -> Result<Option<Image>, EngineError>;
    fn pull(&self, reference: &str) -> Result<(), EngineError>;
    /// `Ok(None)`: no such container.
    fn inspect_container(&self, name_or_id: &str) -> Result<Option<Container>, EngineError>;
    /// Every container, running or not.
    fn list_containers(&self) -> Result<Vec<Container>, EngineError>;
    /// Create, never start. The new container's id.
    fn create_container(&self, spec: &ContainerSpec) -> Result<String, EngineError>;
    fn start_container(&self, id: &str) -> Result<(), EngineError>;
    fn stop_container(&self, id: &str, grace: Duration) -> Result<(), EngineError>;
    fn set_restart_policy(&self, id: &str, policy: RestartPolicy) -> Result<(), EngineError>;
    fn rename_container(&self, id: &str, name: &str) -> Result<(), EngineError>;
    /// Forced; a missing container is not an error.
    fn remove_container(&self, id: &str) -> Result<(), EngineError>;
    /// The exit code once the container stops, or `Timeout` after `timeout`.
    fn wait_container(&self, id: &str, timeout: Duration) -> Result<i64, EngineError>;
    fn logs_tail(&self, id: &str, lines: usize) -> Result<String, EngineError>;
    /// Extract a tar archive at `path` in a (created) container.
    fn upload_archive(&self, id: &str, path: &str, tar: Vec<u8>) -> Result<(), EngineError>;
    fn inspect_volume(&self, name: &str) -> Result<Option<Volume>, EngineError>;
    fn create_volume(
        &self,
        name: &str,
        labels: &BTreeMap<String, String>,
    ) -> Result<Volume, EngineError>;
    /// A missing volume is not an error.
    fn remove_volume(&self, name: &str) -> Result<(), EngineError>;
}
