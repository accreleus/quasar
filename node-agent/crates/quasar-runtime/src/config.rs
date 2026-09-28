//! Engine socket discovery and the refusals that keep it explicit.

use crate::{ErrorKind, RuntimeError};
use std::{path::PathBuf, time::Duration};

/// Docker's default engine socket, and the endpoint when nothing else is found.
pub const DOCKER_DEFAULT_SOCKET: &str = "/var/run/docker.sock";

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

    /// The second half of [`Self::from_environment`], on the process environment and the
    /// real filesystem. See [`Self::resolve_endpoint_with`] for the rules.
    pub fn resolve_endpoint() -> Result<Self, RuntimeError> {
        Self::resolve_endpoint_with(&|key| std::env::var_os(key), &|path| path.exists())
    }

    /// Which one Unix endpoint this process talks to (RH-07 #396, amendment 17):
    ///
    /// 1. `DOCKER_HOST` and `CONTAINER_HOST` (Podman's name for the same setting) both set
    ///    to different endpoints is **ambiguous** and refused by name; never resolved by
    ///    picking one.
    /// 2. Otherwise the one that is set, which must be an absolute `unix://` path.
    /// 3. Otherwise the default: the first socket that exists of Docker's
    ///    `/var/run/docker.sock`, Podman's rootful `/run/podman/podman.sock`, then the
    ///    user's own `$XDG_RUNTIME_DIR/podman/podman.sock` and `$XDG_RUNTIME_DIR/docker.sock`
    ///    (rootless engines). Docker comes first, so a Docker host is unchanged when Podman
    ///    is installed beside it. With none present it stays `/var/run/docker.sock`, which
    ///    then reports unreachable. A Docker CLI context other than `default` is refused.
    ///
    /// Finding a socket says nothing about what the engine can do; each capability is
    /// reported by its own readiness check.
    pub fn resolve_endpoint_with(
        env: &dyn Fn(&str) -> Option<std::ffi::OsString>,
        exists: &dyn Fn(&std::path::Path) -> bool,
    ) -> Result<Self, RuntimeError> {
        let set = |key: &str| env(key).filter(|value| !value.is_empty());
        // An endpoint is a unix:// URL, so it must be text; paths keep their bytes.
        let endpoint = |key: &str| -> Result<Option<String>, RuntimeError> {
            set(key)
                .map(|value| {
                    value
                        .into_string()
                        .map_err(|_| RuntimeError::from(ErrorKind::InvalidConfiguration))
                })
                .transpose()
        };
        match (endpoint("DOCKER_HOST")?, endpoint("CONTAINER_HOST")?) {
            (Some(docker), Some(container)) if docker != container => {
                // The same socket spelled two ways is one endpoint, not two.
                let (docker, container) = (
                    Self::from_endpoint(&docker)?,
                    Self::from_endpoint(&container)?,
                );
                if docker.socket.components().ne(container.socket.components()) {
                    return Err(ErrorKind::AmbiguousEndpoint.into());
                }
                return Ok(docker);
            }
            (Some(host), _) | (None, Some(host)) => return Self::from_endpoint(&host),
            (None, None) => {}
        }
        // The Docker CLI remembers `docker context use` in its config. An operator who
        // switched context expects it to be honoured; this agent cannot honour it, so
        // refuse rather than silently talking to a different engine than the one they
        // selected.
        let directory = set("DOCKER_CONFIG")
            .map(PathBuf::from)
            .or_else(|| set("HOME").map(|home| PathBuf::from(home).join(".docker")))
            .ok_or(ErrorKind::InvalidConfiguration)?;
        match std::fs::read(directory.join("config.json")) {
            Ok(bytes) => {
                #[derive(serde::Deserialize)]
                struct Context {
                    #[serde(default, rename = "currentContext")]
                    current: String,
                }
                let context: Context =
                    serde_json::from_slice(&bytes).map_err(|_| ErrorKind::InvalidConfiguration)?;
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
        let mut candidates = vec![
            PathBuf::from(DOCKER_DEFAULT_SOCKET),
            PathBuf::from("/run/podman/podman.sock"),
        ];
        if let Some(runtime_dir) = set("XDG_RUNTIME_DIR").map(PathBuf::from) {
            if runtime_dir.is_absolute() {
                candidates.push(runtime_dir.join("podman/podman.sock"));
                candidates.push(runtime_dir.join("docker.sock"));
            }
        }
        let socket = candidates
            .into_iter()
            .find(|path| exists(path))
            .unwrap_or_else(|| PathBuf::from(DOCKER_DEFAULT_SOCKET));
        Ok(Self::unix(socket))
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::path::Path;

    fn resolve(env: &[(&str, &str)], existing: &[&str]) -> Result<PathBuf, ErrorKind> {
        let env: HashMap<String, std::ffi::OsString> = env
            .iter()
            .map(|(k, v)| (k.to_string(), std::ffi::OsString::from(v)))
            .collect();
        let existing: Vec<PathBuf> = existing.iter().map(PathBuf::from).collect();
        RuntimeConfig::resolve_endpoint_with(&|key| env.get(key).cloned(), &|path: &Path| {
            existing.iter().any(|p| p == path)
        })
        .map(|config| config.socket)
        .map_err(|error| error.kind)
    }

    #[test]
    fn docker_host_is_honoured_exactly_as_before() {
        assert_eq!(
            resolve(&[("DOCKER_HOST", "unix:///srv/docker.sock")], &[]),
            Ok(PathBuf::from("/srv/docker.sock"))
        );
        assert_eq!(
            resolve(&[("DOCKER_HOST", "tcp://10.0.0.1:2375")], &[]),
            Err(ErrorKind::InvalidConfiguration)
        );
    }

    #[test]
    fn container_host_names_the_endpoint_when_docker_host_is_unset() {
        assert_eq!(
            resolve(
                &[("CONTAINER_HOST", "unix:///run/user/1000/podman/podman.sock")],
                &[]
            ),
            Ok(PathBuf::from("/run/user/1000/podman/podman.sock"))
        );
    }

    #[test]
    fn the_same_socket_spelled_twice_is_not_ambiguous() {
        assert_eq!(
            resolve(
                &[
                    ("DOCKER_HOST", "unix:///run/podman/podman.sock"),
                    ("CONTAINER_HOST", "unix:///run/podman//podman.sock")
                ],
                &[]
            ),
            Ok(PathBuf::from("/run/podman/podman.sock"))
        );
    }

    #[test]
    fn the_same_endpoint_in_both_variables_is_not_ambiguous() {
        let same = "unix:///run/podman/podman.sock";
        assert_eq!(
            resolve(&[("DOCKER_HOST", same), ("CONTAINER_HOST", same)], &[]),
            Ok(PathBuf::from("/run/podman/podman.sock"))
        );
    }

    /// Amendment 17: ambiguous means exactly this, and it is refused by name, never
    /// resolved by picking one.
    #[test]
    fn two_different_endpoints_are_ambiguous() {
        assert_eq!(
            resolve(
                &[
                    ("DOCKER_HOST", "unix:///var/run/docker.sock"),
                    ("CONTAINER_HOST", "unix:///run/podman/podman.sock")
                ],
                &[]
            ),
            Err(ErrorKind::AmbiguousEndpoint)
        );
    }

    /// With nothing configured, Docker's default comes first, so a Docker host is
    /// unchanged even when Podman is installed beside it.
    #[test]
    fn the_default_prefers_docker_then_podman_rootful_then_the_users_sockets() {
        let all = [
            "/var/run/docker.sock",
            "/run/podman/podman.sock",
            "/run/user/1000/podman/podman.sock",
            "/run/user/1000/docker.sock",
        ];
        let env = [
            ("XDG_RUNTIME_DIR", "/run/user/1000"),
            ("HOME", "/nonexistent"),
        ];
        assert_eq!(
            resolve(&env, &all),
            Ok(PathBuf::from("/var/run/docker.sock"))
        );
        assert_eq!(
            resolve(&env, &all[1..]),
            Ok(PathBuf::from("/run/podman/podman.sock"))
        );
        assert_eq!(
            resolve(&env, &all[2..]),
            Ok(PathBuf::from("/run/user/1000/podman/podman.sock"))
        );
        assert_eq!(
            resolve(&env, &all[3..]),
            Ok(PathBuf::from("/run/user/1000/docker.sock"))
        );
        // Nothing there at all: the traditional default, which then reports unreachable.
        assert_eq!(
            resolve(&env, &[]),
            Ok(PathBuf::from("/var/run/docker.sock"))
        );
    }

    #[test]
    fn a_relative_runtime_dir_is_never_used() {
        let env = [
            ("XDG_RUNTIME_DIR", "run/user/1000"),
            ("HOME", "/nonexistent"),
        ];
        assert_eq!(
            resolve(&env, &["run/user/1000/podman/podman.sock"]),
            Ok(PathBuf::from("/var/run/docker.sock"))
        );
    }
}
