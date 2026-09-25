//! Bollard types and protocol details stay private to this adapter. The engine
//! seam it builds on (discovery, classification, credentials, read-only
//! inspection) is `quasar_runtime::docker`.
use super::{ErrorKind, RuntimeConfig, RuntimeError};
use bollard::{errors::Error, Docker};
use futures_util::StreamExt;
use quasar_runtime::docker::{
    all_container_image_ids, classify, credentials, daemon_images, discover, image_error,
};
use std::sync::OnceLock;
pub(super) mod application;
mod build;
pub(super) mod helpers;
pub(super) mod legacy;
pub(super) use build::build as build_image;

pub(super) fn image_launch_lock(config: &RuntimeConfig) -> std::sync::Arc<tokio::sync::Mutex<()>> {
    static LOCKS: OnceLock<
        std::sync::Mutex<
            std::collections::HashMap<std::path::PathBuf, std::sync::Arc<tokio::sync::Mutex<()>>>,
        >,
    > = OnceLock::new();
    LOCKS
        .get_or_init(|| std::sync::Mutex::new(std::collections::HashMap::new()))
        .lock()
        .unwrap()
        .entry(config.socket.clone())
        .or_insert_with(|| std::sync::Arc::new(tokio::sync::Mutex::new(())))
        .clone()
}

pub(super) async fn image_inventory_snapshot(
    config: &RuntimeConfig,
) -> Result<(Vec<super::DaemonImage>, Vec<String>), RuntimeError> {
    let lock = image_launch_lock(config);
    let _guard = lock.lock().await;
    let images = daemon_images(config).await?;
    let containers = all_container_image_ids(config).await?;
    Ok((images, containers))
}

/// Holds the same launch exclusion as container creation through the final
/// daemon checks and non-forced removal. A changed tag binding is a refusal.
pub(super) async fn remove_exact_image(
    config: &RuntimeConfig,
    image_ref: &str,
    expected_id: &str,
) -> Result<ExactRemoval, RuntimeError> {
    let lock = image_launch_lock(config);
    let _guard = lock.lock().await;
    let images = daemon_images(config).await?;
    let containers = all_container_image_ids(config).await?;
    if containers.iter().any(|id| id == expected_id) {
        return Ok(ExactRemoval::Referenced);
    }
    let current = images
        .iter()
        .find(|image| image.refs.iter().any(|reference| reference == image_ref));
    let Some(current) = current else {
        return Ok(ExactRemoval::Absent);
    };
    if current.id != expected_id {
        return Ok(ExactRemoval::IdentityMismatch);
    }
    // The ref can be rebound by an external Docker client after our check.
    // Address the daemon object we proved safe, never the mutable ref.
    let (docker, _) = discover(config).await?;
    let result = docker
        .remove_image(
            expected_id,
            Some(bollard::query_parameters::RemoveImageOptions {
                force: false,
                noprune: true,
                ..Default::default()
            }),
            None,
        )
        .await
        .map_err(classify);
    let after = daemon_images(config).await?;
    let refs = all_container_image_ids(config).await?;
    if after
        .iter()
        .any(|image| image.refs.iter().any(|reference| reference == image_ref))
    {
        Ok(ExactRemoval::StillPresent)
    } else if !refs.iter().any(|id| id == expected_id) {
        Ok(ExactRemoval::Removed)
    } else {
        Err(result.err().unwrap_or(ErrorKind::UnknownOutcome.into()))
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ExactRemoval {
    Removed,
    Absent,
    Referenced,
    IdentityMismatch,
    StillPresent,
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
    // Managed build tags are host-local outputs, not registry references. A
    // missing tag must fail here rather than letting Docker resolve it as a
    // remote repository.
    if image.starts_with("quasar-local/") {
        return Err(ErrorKind::Missing.into());
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

#[cfg(test)]
mod real_tests;
