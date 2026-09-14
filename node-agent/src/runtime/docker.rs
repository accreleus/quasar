//! Bollard types and protocol details stay private to this adapter.
use super::{ApiVersion, EngineInfo, ErrorKind, RuntimeConfig, RuntimeError};
use bollard::{errors::Error, Docker};

fn classify(error: Error) -> RuntimeError {
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

fn version(raw: Option<&str>) -> Result<ApiVersion, RuntimeError> {
    let (major, minor) = raw
        .and_then(|s| s.split_once('.'))
        .ok_or(ErrorKind::Protocol)?;
    Ok(ApiVersion {
        major: major.parse().map_err(|_| ErrorKind::Protocol)?,
        minor: minor.parse().map_err(|_| ErrorKind::Protocol)?,
    })
}

pub(super) async fn discover(config: &RuntimeConfig) -> Result<(Docker, EngineInfo), RuntimeError> {
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
    // This slice uses the v1.40 discovery/inspection contract. Higher capability
    // floors must be established by the caller migrations that need them.
    let floor = ApiVersion {
        major: 1,
        minor: 40,
    };
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

pub(super) async fn inspect_image(
    config: &RuntimeConfig,
    image: &str,
) -> Result<bool, RuntimeError> {
    if image.trim().is_empty() {
        return Err(ErrorKind::InvalidConfiguration.into());
    }
    let (docker, _) = discover(config).await?;
    match docker.inspect_image(image).await {
        Ok(info) if info.id.as_ref().is_some_and(|id| !id.is_empty()) => Ok(true),
        Ok(_) => Err(ErrorKind::Protocol.into()),
        Err(Error::DockerResponseServerError {
            status_code: 404, ..
        }) => Ok(false),
        Err(e) => Err(classify(e)),
    }
}
