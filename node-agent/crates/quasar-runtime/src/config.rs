//! Engine socket discovery and the refusals that keep it explicit.

use crate::{ErrorKind, RuntimeError};
use std::{path::PathBuf, time::Duration};

#[derive(Debug, Clone)]
pub struct RuntimeConfig {
    pub socket: PathBuf,
    pub deadline: Duration,
    pub max_in_flight: usize,
    /// Directory for the durable operation state of adapters layered on the client
    /// (image, application and helper journals). `None` refuses those operations.
    pub image_state_path: Option<PathBuf>,
    pub registry_config_path: Option<PathBuf>,
    /// Test-only injection keeps HTTP fixtures independent of process-global
    /// ownership state; production always resolves the persisted owner lease.
    #[cfg(feature = "test-support")]
    pub diagnostic_owner: Option<String>,
}
impl RuntimeConfig {
    pub fn unix(socket: impl Into<PathBuf>) -> Self {
        Self {
            socket: socket.into(),
            deadline: Duration::from_secs(30),
            max_in_flight: 4,
            image_state_path: None,
            registry_config_path: None,
            #[cfg(feature = "test-support")]
            diagnostic_owner: None,
        }
    }

    /// Resolve the one explicit Unix endpoint this process talks to. Context, TLS
    /// and API-version overrides are refused rather than silently ignored.
    ///
    /// Exactly [`Self::refuse_engine_selectors`] then [`Self::resolve_endpoint`]; a
    /// caller with its own startup notices to emit between the two composes them.
    pub fn from_environment() -> Result<Self, RuntimeError> {
        Self::refuse_engine_selectors()?;
        Self::resolve_endpoint()
    }

    /// The first half of [`Self::from_environment`]: refuse every Docker CLI selector
    /// this client cannot honour, so an operator's choice is never silently replaced.
    pub fn refuse_engine_selectors() -> Result<(), RuntimeError> {
        for key in [
            "DOCKER_CONTEXT",
            "DOCKER_TLS",
            "DOCKER_TLS_VERIFY",
            "DOCKER_API_VERSION",
        ] {
            if std::env::var_os(key).is_some_and(|v| !v.is_empty()) {
                return Err(ErrorKind::InvalidConfiguration.into());
            }
        }
        Ok(())
    }

    /// The second half of [`Self::from_environment`]: `DOCKER_HOST` when set, else the
    /// default socket — unless the Docker CLI config names a non-default context.
    pub fn resolve_endpoint() -> Result<Self, RuntimeError> {
        match std::env::var("DOCKER_HOST") {
            Ok(host) if !host.is_empty() => Self::from_endpoint(&host),
            Ok(_) | Err(std::env::VarError::NotPresent) => {
                // The Docker CLI remembers `docker context use` in its config.
                // An operator who switched context expects it to be honoured;
                // this agent cannot honour it, so refuse rather than silently
                // talking to a different engine than the one they selected.
                let directory = std::env::var_os("DOCKER_CONFIG")
                    .filter(|v| !v.is_empty())
                    .map(PathBuf::from)
                    .or_else(|| {
                        std::env::var_os("HOME").map(|home| PathBuf::from(home).join(".docker"))
                    })
                    .ok_or(ErrorKind::InvalidConfiguration)?;
                match std::fs::read(directory.join("config.json")) {
                    Ok(bytes) => {
                        #[derive(serde::Deserialize)]
                        struct Context {
                            #[serde(default, rename = "currentContext")]
                            current: String,
                        }
                        let context: Context = serde_json::from_slice(&bytes)
                            .map_err(|_| ErrorKind::InvalidConfiguration)?;
                        if !context.current.is_empty() && context.current != "default" {
                            return Err(ErrorKind::InvalidConfiguration.into());
                        }
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                    Err(error) if error.kind() == std::io::ErrorKind::PermissionDenied => {
                        return Err(ErrorKind::PermissionDenied.into())
                    }
                    Err(_) => return Err(ErrorKind::InvalidConfiguration.into()),
                }
                Ok(Self::unix("/var/run/docker.sock"))
            }
            Err(_) => Err(ErrorKind::InvalidConfiguration.into()),
        }
    }

    pub fn from_endpoint(endpoint: &str) -> Result<Self, RuntimeError> {
        let path = endpoint
            .strip_prefix("unix://")
            .ok_or(ErrorKind::InvalidConfiguration)?;
        if !std::path::Path::new(path).is_absolute() || path.contains(['?', '#', '\0']) {
            return Err(ErrorKind::InvalidConfiguration.into());
        }
        Ok(Self::unix(path))
    }
}
