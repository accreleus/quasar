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
pub use inspection::{all_container_image_ids, daemon_images};
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
    let info = EngineInfo {
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
    Ok(crate::EngineFacts {
        info,
        operating_system: sys.operating_system,
        architecture: sys.architecture,
        cgroup_version,
        security_options: sys.security_options.unwrap_or_default(),
        runtimes,
        default_runtime: sys.default_runtime,
        cdi,
    })
}
