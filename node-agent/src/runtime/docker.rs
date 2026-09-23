//! Bollard types and protocol details stay private to this adapter.
use super::{ApiVersion, EngineInfo, ErrorKind, RuntimeConfig, RuntimeError};
use bollard::{errors::Error, Docker};
use futures_util::StreamExt;
pub(super) mod application;
mod build;
mod credentials;
mod inspection;
pub(super) use inspection::{
    engine_storage, inspect_container, inspect_image_metadata, live_containers,
};
pub(super) mod helpers;
pub(super) mod legacy;
pub(super) use build::build as build_image;

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

fn image_error(error: Error) -> RuntimeError {
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

/// One reconciliation policy for every mutation of a reference.
async fn reconcile_image(
    config: &RuntimeConfig,
    image: &str,
) -> Result<
    (
        Docker,
        super::images::Journal,
        Option<super::ImageInfo>,
        Option<String>,
    ),
    RuntimeError,
> {
    use super::images::Journal;
    if image.is_empty() || image.len() > 2048 || image.contains(['?', '#', '\0']) {
        return Err(ErrorKind::InvalidConfiguration.into());
    }
    let (docker, _) = discover(config).await?;
    let journal = Journal::acquire(config, image).await?;
    let state = image_state(&docker, image).await?;
    let existing = state.as_ref().map(|s| s.info.clone());
    let mut recovered_build = None;
    if let Some(intent) = journal.pending()? {
        if intent.image != image || intent.socket != config.socket {
            return Err(ErrorKind::UnknownOutcome.into());
        }
        if let Some(id) = intent.build_id {
            if state.as_ref().and_then(|s| s.build_id.as_deref()) != Some(&id) {
                return Err(ErrorKind::UnknownOutcome.into());
            }
            recovered_build = Some(intent.build_fingerprint.ok_or(ErrorKind::UnknownOutcome)?);
            journal.clear()?;
        } else if intent.remove_id.is_some() && existing.is_none()
            || intent.remove_id.is_none() && existing.is_some()
        {
            journal.clear()?;
        } else {
            return Err(ErrorKind::UnknownOutcome.into());
        }
    }
    Ok((docker, journal, existing, recovered_build))
}

pub(super) async fn ensure_image(
    config: &RuntimeConfig,
    image: &str,
    progress: tokio::sync::watch::Sender<super::ImageProgress>,
) -> Result<super::ImageInfo, RuntimeError> {
    use super::images::Intent;
    let (docker, journal, existing, _) = reconcile_image(config, image).await?;
    if let Some(info) = existing {
        return Ok(info);
    }
    let credentials = credentials::load(config, image).await?;
    journal.begin(Intent {
        image: image.into(),
        socket: config.socket.clone(),
        remove_id: None,
        build_id: None,
        build_fingerprint: None,
    })?;
    let tag = if image.contains('@') || image.rsplit('/').next().unwrap_or(image).contains(':') {
        None
    } else {
        Some("latest".to_owned())
    };
    let options = bollard::query_parameters::CreateImageOptions {
        from_image: Some(image.to_owned()),
        tag,
        ..Default::default()
    };
    let mut stream = docker.create_image(Some(options), None, credentials);
    let mut layers = std::collections::BTreeMap::<String, (u64, u64)>::new();
    while let Some(event) = stream.next().await {
        let event = match event {
            Ok(event) => event,
            Err(error) => {
                let definite = matches!(error, Error::DockerStreamError { .. } | Error::DockerResponseServerError { status_code: 400..=407 | 409..=499, .. });
                if definite {
                    journal.clear()?;
                    return Err(image_error(error));
                }
                return Err(ErrorKind::UnknownOutcome.into());
            }
        };
        if event.error_detail.is_some() {
            journal.clear()?;
            return Err(ErrorKind::Engine.into());
        }
        if let (Some(id), Some(detail)) = (event.id, event.progress_detail) {
            if id.len() <= 128 && (layers.len() < 1024 || layers.contains_key(&id)) {
                layers.insert(
                    id,
                    (
                        detail.current.unwrap_or(0).max(0) as u64,
                        detail.total.unwrap_or(0).max(0) as u64,
                    ),
                );
                let (current, total) = layers.values().fold((0u64, 0u64), |(c, t), (nc, nt)| {
                    (c.saturating_add(*nc), t.saturating_add(*nt))
                });
                progress.send_replace(super::ImageProgress {
                    percent: if total == 0 {
                        0
                    } else {
                        ((current as f64 / total as f64) * 100.).clamp(0., 99.) as u8
                    },
                    bytes: current,
                });
            }
        }
    }
    let info = image_info(&docker, image)
        .await
        .map_err(|_| ErrorKind::UnknownOutcome)?
        .ok_or(ErrorKind::UnknownOutcome)?;
    journal.clear()?;
    Ok(info)
}

pub(super) async fn remove_image(config: &RuntimeConfig, image: &str) -> Result<(), RuntimeError> {
    use super::images::Intent;
    let (docker, journal, existing, _) = reconcile_image(config, image).await?;
    let Some(existing) = existing else {
        return Ok(());
    };
    // Docker can untag a multi-tagged image even when a container references it.
    // Check all containers as well as asking the daemon for non-forced removal.
    let containers = docker
        .list_containers(Some(bollard::query_parameters::ListContainersOptions {
            all: true,
            ..Default::default()
        }))
        .await
        .map_err(classify)?;
    if containers
        .iter()
        .any(|c| c.image_id.as_deref() == Some(&existing.id))
    {
        return Err(ErrorKind::ImageInUse.into());
    }
    journal.begin(Intent {
        image: image.into(),
        socket: config.socket.clone(),
        remove_id: Some(existing.id),
        build_id: None,
        build_fingerprint: None,
    })?;
    let result = docker
        .remove_image(
            image,
            Some(bollard::query_parameters::RemoveImageOptions {
                force: false,
                noprune: true,
                ..Default::default()
            }),
            None,
        )
        .await;
    match result {
        Ok(_)
        | Err(Error::DockerResponseServerError {
            status_code: 404, ..
        }) => {
            if image_info(&docker, image)
                .await
                .map_err(|_| ErrorKind::UnknownOutcome)?
                .is_some()
            {
                return Err(ErrorKind::UnknownOutcome.into());
            }
            journal.clear()?;
            Ok(())
        }
        Err(Error::DockerResponseServerError {
            status_code: 409, ..
        }) => {
            journal.clear()?;
            Err(ErrorKind::ImageInUse.into())
        }
        Err(error) => {
            if matches!(error, Error::DockerResponseServerError { status_code: 400..=407 | 409..=499, .. })
            {
                journal.clear()?;
                Err(classify(error))
            } else {
                Err(ErrorKind::UnknownOutcome.into())
            }
        }
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
    // This slice uses the discovery/inspection contract at the module's API floor,
    // which is also what the readiness wording quotes (#266).
    let floor = super::API_FLOOR;
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
    Ok(image_info(&docker, image).await?.is_some())
}

struct ImageState {
    info: super::ImageInfo,
    build_id: Option<String>,
}
async fn image_state(docker: &Docker, image: &str) -> Result<Option<ImageState>, RuntimeError> {
    match docker.inspect_image(image).await {
        Ok(info) if info.id.as_ref().is_some_and(|id| !id.is_empty()) => Ok(Some(ImageState {
            build_id: info
                .config
                .and_then(|c| c.labels)
                .and_then(|labels| labels.get(build::BUILD_LABEL).cloned()),
            info: super::ImageInfo {
                id: info.id.unwrap(),
                bytes: info.size.unwrap_or(0).max(0) as u64,
            },
        })),
        Ok(_) => Err(ErrorKind::Protocol.into()),
        Err(Error::DockerResponseServerError {
            status_code: 404, ..
        }) => Ok(None),
        Err(e) => Err(classify(e)),
    }
}
pub(super) async fn image_info(
    docker: &Docker,
    image: &str,
) -> Result<Option<super::ImageInfo>, RuntimeError> {
    Ok(image_state(docker, image).await?.map(|s| s.info))
}

/// One read-only `/info`, folded into [`super::EngineFacts`]. No CDI spec dir reported by
/// the engine (`None`) is distinct from CDI reported but disabled (empty `spec_dirs`).
pub(super) async fn inspect_engine(
    config: &RuntimeConfig,
) -> Result<super::EngineFacts, RuntimeError> {
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
        super::CdiFacts { spec_dirs, devices }
    });
    Ok(super::EngineFacts {
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

#[cfg(test)]
mod real_tests;
