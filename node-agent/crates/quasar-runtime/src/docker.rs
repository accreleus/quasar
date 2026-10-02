//! The typed Docker adapter and the seam engine adapters layered on the crate use.
//!
//! The crate carries engine discovery and its refusals, registry credentials, error
//! classification and read-only inspection. Its adapter seam is not Bollard-free:
//! [`discover`] returns a negotiated `bollard::Docker` and [`classify`] /
//! [`image_error`] take Bollard errors, so adapters layered on the crate share one
//! SDK version with it. The mutating typed lifecycles (image pull/ensure, exact removal, and
//! container create/start/stop/remove for applications, helpers and the legacy sweep) stay in
//! the agent because they are bound to the agent's journals; the recovery-actor slice extends
//! this crate with its own mutating adapter.
//!
//! Nothing here returns daemon text to a caller.
use crate::{ApiVersion, EngineInfo, ErrorKind, RuntimeConfig, RuntimeError};
use bollard::{errors::Error, Docker};
pub mod credentials;
mod inspection;
mod libpod;
pub(crate) mod platform;
pub use inspection::{all_container_image_ids, daemon_images, inspect_container_with};

/// Inspect one container, tolerating what Podman reports outside Docker's API schema, which
/// bollard cannot parse: the health status `stopped` (an exited container whose image has a
/// healthcheck), and the libpod states `stopped` and `configured` as the container's status.
/// Unparseable for any other reason is returned as it was.
pub async fn inspect_container_tolerant(
    docker: &bollard::Docker,
    name_or_id: &str,
) -> Result<bollard::models::ContainerInspectResponse, bollard::errors::Error> {
    // The one permitted raw call. Never log the error with `?`: with json_data_content its
    // Debug carries the whole inspect body, container environment included.
    #[allow(clippy::disallowed_methods)]
    let raw = docker.inspect_container(name_or_id, None).await;
    match raw {
        Err(bollard::errors::Error::JsonDataError {
            message,
            contents,
            column,
        }) => normalize_inspect(&contents).ok_or(bollard::errors::Error::JsonDataError {
            message,
            contents,
            column,
        }),
        other => other,
    }
}

/// Health statuses Docker's schema does not know read as `none`. Libpod's `stopped` (exited,
/// its cleanup pending) reads as `exited`, and `configured` (not yet in the OCI runtime) as
/// `created`; any other unknown status stays unparseable.
fn normalize_inspect(contents: &str) -> Option<bollard::models::ContainerInspectResponse> {
    let mut value: serde_json::Value = serde_json::from_str(contents).ok()?;
    if let Some(status) = value.pointer_mut("/State/Health/Status") {
        if !matches!(
            status.as_str(),
            Some("" | "none" | "starting" | "healthy" | "unhealthy")
        ) {
            *status = serde_json::Value::String("none".into());
        }
    }
    if let Some(status) = value.pointer_mut("/State/Status") {
        let docker = match status.as_str() {
            Some("stopped") => Some("exited"),
            Some("configured") => Some("created"),
            _ => None,
        };
        if let Some(docker) = docker {
            *status = serde_json::Value::String(docker.into());
        }
    }
    serde_json::from_value(value).ok()
}
pub(crate) use inspection::{
    engine_storage, inspect_container, inspect_image_metadata, live_containers,
};

pub fn classify(error: Error) -> RuntimeError {
    let mut cause: Option<&(dyn std::error::Error + 'static)> = Some(&error);
    while let Some(current) = cause {
        if current
            .downcast_ref::<std::io::Error>()
            .is_some_and(|e| e.kind() == std::io::ErrorKind::PermissionDenied)
        {
            return ErrorKind::PermissionDenied.into();
        }
        cause = current.source();
    }
    match error {
        Error::DockerResponseServerError {
            status_code: 401 | 403,
            ..
        } => ErrorKind::PermissionDenied,
        Error::DockerResponseServerError { .. } => ErrorKind::Engine,
        Error::RequestTimeoutError => ErrorKind::Timeout,
        Error::SocketNotFoundError(_)
        | Error::IOError { .. }
        | Error::HyperLegacyError { .. }
        | Error::HyperResponseError { .. } => ErrorKind::Unavailable,
        _ => ErrorKind::Protocol,
    }
    .into()
}

pub fn image_error(error: Error) -> RuntimeError {
    let message = match &error {
        Error::DockerStreamError { error } => error.as_str(),
        Error::DockerResponseServerError { message, .. } => message.as_str(),
        _ => return ErrorKind::UnknownOutcome.into(),
    }
    .to_lowercase();
    if message.contains("no space left") {
        ErrorKind::InsufficientDisk.into()
    } else if message.contains("unauthorized")
        || message.contains("denied")
        || message.contains("authentication required")
    {
        ErrorKind::RegistryDenied.into()
    } else if message.contains("manifest unknown") || message.contains("manifest not found") {
        ErrorKind::ManifestMissing.into()
    } else {
        classify(error)
    }
}

fn version(raw: Option<&str>) -> Result<ApiVersion, RuntimeError> {
    let (major, minor) = raw
        .and_then(|s| s.split_once('.'))
        .ok_or(ErrorKind::Protocol)?;
    Ok(ApiVersion {
        major: major.parse().map_err(|_| ErrorKind::Protocol)?,
        minor: minor.parse().map_err(|_| ErrorKind::Protocol)?,
    })
}

pub async fn discover(config: &RuntimeConfig) -> Result<(Docker, EngineInfo), RuntimeError> {
    // Bollard's exists() check can hide permission errors on a parent directory.
    tokio::fs::metadata(&config.socket)
        .await
        .map_err(|e| classify(e.into()))?;
    let docker = Docker::connect_with_unix(
        config.socket.to_str().unwrap(),
        config.deadline.as_secs().max(1),
        bollard::API_DEFAULT_VERSION,
    )
    .map_err(classify)?;
    let reported = docker.version().await.map_err(classify)?;
    let min = version(reported.min_api_version.as_deref())?;
    let max = version(reported.api_version.as_deref())?;
    // This slice uses the discovery/inspection contract at the module's API floor,
    // which is also what the readiness wording quotes (#266).
    let floor = crate::API_FLOOR;
    let ceiling = ApiVersion {
        major: bollard::API_DEFAULT_VERSION.major_version,
        minor: bollard::API_DEFAULT_VERSION.minor_version,
    };
    let selected = max.min(ceiling);
    if min > max {
        return Err(ErrorKind::Protocol.into());
    }
    if selected < floor || selected < min || selected.major != 1 {
        return Err(ErrorKind::IncompatibleApi.into());
    }
    let components: Vec<String> = reported
        .components
        .as_deref()
        .unwrap_or_default()
        .iter()
        .map(|c| c.name.clone())
        .collect();
    let kind = crate::EngineKind::from_version(
        reported.platform.as_ref().map(|p| p.name.as_str()),
        &components,
    );
    let info = EngineInfo {
        kind,
        name: reported
            .platform
            .map(|p| p.name)
            .filter(|n| !n.is_empty())
            .unwrap_or_else(|| "unknown".into()),
        version: reported
            .version
            .filter(|v| !v.is_empty())
            .ok_or(ErrorKind::Protocol)?,
        api_version: selected,
        server_min_api: min,
        server_max_api: max,
    };
    // Pin the actual wire path. Bollard 0.21.1's URI join discards its version
    // prefix for absolute endpoint paths; negotiation must affect requests too.
    let docker = Docker::connect_with_unix(
        config.socket.to_str().unwrap(),
        config.deadline.as_secs().max(1),
        &bollard::ClientVersion {
            major_version: selected.major,
            minor_version: selected.minor,
        },
    )
    .map_err(classify)?
    .with_request_modifier(move |mut request| {
        let mut parts = request.uri().clone().into_parts();
        let path = parts
            .path_and_query
            .as_ref()
            .map(|p| p.as_str())
            .unwrap_or("/");
        parts.path_and_query = Some(
            format!("/v{selected}{path}")
                .parse()
                .expect("valid versioned path"),
        );
        *request.uri_mut() = parts.try_into().expect("valid engine URI");
        request
    });
    Ok((docker, info))
}

/// How the engine that answered `sys` injects an NVIDIA GPU: the one reading of `/info` the
/// agent, its readiness and the recovery actor share.
pub fn gpu_injection_from_info(
    kind: crate::EngineKind,
    sys: &bollard::models::SystemInfo,
) -> Option<crate::GpuInjection> {
    let mode = crate::EngineMode::from_security_options(
        sys.security_options.as_deref().unwrap_or_default(),
    );
    let cdi = sys.cdi_spec_dirs.clone().map(|spec_dirs| crate::CdiFacts {
        spec_dirs,
        devices: sys
            .discovered_devices
            .iter()
            .flatten()
            .filter_map(|d| d.id.clone())
            .collect(),
    });
    crate::GpuInjection::for_engine(kind, mode, cdi.as_ref())
}

/// One read-only `/info`, folded into [`crate::EngineFacts`]. No CDI spec dir reported by
/// the engine (`None`) is distinct from CDI reported but disabled (empty `spec_dirs`).
pub async fn inspect_engine(config: &RuntimeConfig) -> Result<crate::EngineFacts, RuntimeError> {
    let (docker, info) = discover(config).await?;
    let sys = docker.info().await.map_err(classify)?;
    let cgroup_version = sys.cgroup_version.and_then(|v| {
        let v = v.to_string();
        (!v.is_empty()).then_some(v)
    });
    let mut runtimes: Vec<String> = sys.runtimes.unwrap_or_default().into_keys().collect();
    runtimes.sort();
    let cdi = sys.cdi_spec_dirs.map(|spec_dirs| {
        let devices = sys
            .discovered_devices
            .unwrap_or_default()
            .into_iter()
            .map(|device| {
                let id = device
                    .id
                    .filter(|v| !v.is_empty())
                    .unwrap_or_else(|| "unknown".into());
                let source = device
                    .source
                    .filter(|v| !v.is_empty())
                    .unwrap_or_else(|| "unknown".into());
                format!("{id} ({source})")
            })
            .collect();
        crate::CdiFacts { spec_dirs, devices }
    });
    let security_options = sys.security_options.unwrap_or_default();
    Ok(crate::EngineFacts {
        info,
        mode: crate::EngineMode::from_security_options(&security_options),
        operating_system: sys.operating_system,
        os_version: sys.os_version.filter(|v| !v.is_empty()),
        architecture: sys.architecture,
        cgroup_version,
        cgroup_driver: sys
            .cgroup_driver
            .map(|v| v.to_string())
            .filter(|v| !v.is_empty()),
        security_options,
        runtimes,
        default_runtime: sys.default_runtime,
        cdi,
    })
}

#[cfg(test)]
mod tolerant_tests {
    /// Captured live from rootless Podman 5.8.4: an exited container whose image has a
    /// healthcheck reports `stopped`.
    #[test]
    fn podmans_stopped_health_reads_as_none_and_nothing_else_changes() {
        let body = r#"{"Id":"abc","Name":"/x","State":{"Status":"exited","Running":false,"Health":{"Status":"stopped","FailingStreak":0,"Log":[]}},"Config":{"Image":"i"}}"#;
        assert!(serde_json::from_str::<bollard::models::ContainerInspectResponse>(body).is_err());
        let parsed = super::normalize_inspect(body).unwrap();
        let state = parsed.state.unwrap();
        assert_eq!(
            state.health.unwrap().status,
            Some(bollard::models::HealthStatusEnum::NONE)
        );
        assert_eq!(parsed.id.as_deref(), Some("abc"));
        assert!(super::normalize_inspect("not json").is_none());
    }

    /// Captured from Podman 4.9.3 on a hosted runner (engine suite, #408): a crash-looping
    /// `unless-stopped` container just after a stop, with no healthcheck of its own.
    #[test]
    fn podmans_stopped_state_reads_as_exited_without_a_healthcheck() {
        let body = r#"{"Id":"abc","Name":"/x","State":{"Dead":false,"Error":"","ExitCode":3,"Health":{"FailingStreak":0,"Log":null,"Status":""},"OOMKilled":false,"Paused":false,"Pid":0,"Restarting":false,"Running":false,"Status":"stopped"},"Config":{"Image":"i"}}"#;
        assert!(serde_json::from_str::<bollard::models::ContainerInspectResponse>(body).is_err());
        let state = super::normalize_inspect(body).unwrap().state.unwrap();
        assert_eq!(
            state.status,
            Some(bollard::models::ContainerStateStatusEnum::EXITED)
        );
        assert_eq!(state.exit_code, Some(3));
        assert_eq!(state.running, Some(false));

        let no_health = body.replace(
            r#""Health":{"FailingStreak":0,"Log":null,"Status":""},"#,
            "",
        );
        assert_eq!(
            super::normalize_inspect(&no_health)
                .unwrap()
                .state
                .unwrap()
                .status,
            Some(bollard::models::ContainerStateStatusEnum::EXITED)
        );
        let configured = no_health.replace(r#""Status":"stopped""#, r#""Status":"configured""#);
        assert_eq!(
            super::normalize_inspect(&configured)
                .unwrap()
                .state
                .unwrap()
                .status,
            Some(bollard::models::ContainerStateStatusEnum::CREATED)
        );
        let unknown = no_health.replace(r#""Status":"stopped""#, r#""Status":"weird""#);
        assert!(super::normalize_inspect(&unknown).is_none());
    }
}
