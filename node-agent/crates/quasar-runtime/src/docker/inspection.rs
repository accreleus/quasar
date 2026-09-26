//! SDK details for read-only installation and storage facts.
use super::{classify, discover};
use crate::{ErrorKind, RuntimeConfig, RuntimeError};
use bollard::{errors::Error, Docker};

fn mount_kind(value: &str) -> crate::MountKind {
    match value {
        "bind" => crate::MountKind::Bind,
        "volume" => crate::MountKind::Volume,
        "tmpfs" => crate::MountKind::Tmpfs,
        other => crate::MountKind::Other(other.to_owned()),
    }
}

fn container_inspection(
    info: bollard::models::ContainerInspectResponse,
) -> Result<crate::ContainerInspection, RuntimeError> {
    let id = info
        .id
        .filter(|value| !value.is_empty())
        .ok_or(ErrorKind::Protocol)?;
    let image_id = info
        .image
        .filter(|value| !value.is_empty())
        .ok_or(ErrorKind::Protocol)?;
    let config = info.config.ok_or(ErrorKind::Protocol)?;
    let configured_image = config
        .image
        .filter(|value| !value.is_empty())
        .ok_or(ErrorKind::Protocol)?;
    let labels = config.labels.unwrap_or_default().into_iter().collect();
    let network_mode = info
        .host_config
        .and_then(|host| host.network_mode)
        .filter(|value| !value.is_empty());
    let mounts = info
        .mounts
        .ok_or(ErrorKind::Protocol)?
        .into_iter()
        .map(|mount| {
            let typ = mount
                .typ
                .filter(|value| !value.is_empty())
                .ok_or(ErrorKind::Protocol)?;
            let kind = mount_kind(&typ);
            let destination = mount
                .destination
                .filter(|value| !value.is_empty())
                .ok_or(ErrorKind::Protocol)?;
            if !std::path::Path::new(&destination).is_absolute()
                || std::path::Path::new(&destination)
                    .components()
                    .any(|component| matches!(component, std::path::Component::ParentDir))
            {
                return Err(ErrorKind::Protocol.into());
            }
            let source = mount
                .source
                .filter(|value| !value.is_empty())
                .map(|value| crate::DaemonHostPath(value.into()));
            let usable_source = source.as_ref().is_some_and(|source| {
                source.0.is_absolute()
                    && !source
                        .0
                        .components()
                        .any(|component| matches!(component, std::path::Component::ParentDir))
            });
            if matches!(
                kind,
                crate::MountKind::Bind | crate::MountKind::Volume | crate::MountKind::Other(_)
            ) && !usable_source
            {
                return Err(ErrorKind::Protocol.into());
            }
            let name = mount.name.filter(|value| !value.is_empty());
            if matches!(kind, crate::MountKind::Volume) && name.is_none() {
                return Err(ErrorKind::Protocol.into());
            }
            Ok(crate::Mount {
                kind,
                source,
                name,
                destination,
                read_only: mount.rw.map(|writable| !writable),
            })
        })
        .collect::<Result<Vec<_>, RuntimeError>>()?;
    Ok(crate::ContainerInspection {
        id,
        image_id,
        configured_image,
        labels,
        mounts,
        network_mode,
    })
}

pub(crate) async fn inspect_container(
    config: &RuntimeConfig,
    id: &str,
) -> Result<Option<crate::ContainerInspection>, RuntimeError> {
    if id.trim().is_empty() || id.contains(['/', '?', '#', '\0']) {
        return Err(ErrorKind::InvalidConfiguration.into());
    }
    let (docker, _) = discover(config).await?;
    inspect_container_with(&docker, id).await
}

async fn inspect_container_with(
    docker: &Docker,
    id: &str,
) -> Result<Option<crate::ContainerInspection>, RuntimeError> {
    match docker.inspect_container(id, None).await {
        Ok(info) => Ok(Some(container_inspection(info)?)),
        Err(Error::DockerResponseServerError {
            status_code: 404, ..
        }) => Ok(None),
        Err(error) => Err(classify(error)),
    }
}

pub(crate) async fn live_containers(
    config: &RuntimeConfig,
) -> Result<Vec<crate::ContainerInspection>, RuntimeError> {
    let (docker, _) = discover(config).await?;
    let listed = docker
        .list_containers(Some(bollard::query_parameters::ListContainersOptions {
            all: true,
            ..Default::default()
        }))
        .await
        .map_err(classify)?;
    let mut inspected = Vec::with_capacity(listed.len());
    for summary in listed {
        let id = summary
            .id
            .filter(|value| !value.is_empty())
            .ok_or(ErrorKind::Protocol)?;
        let detail = match docker.inspect_container(&id, None).await {
            Ok(detail) => detail,
            // A race means the snapshot cannot prove a home is safe to remove.
            Err(Error::DockerResponseServerError {
                status_code: 404, ..
            }) => return Err(ErrorKind::Missing.into()),
            Err(error) => return Err(classify(error)),
        };
        let inspection = container_inspection(detail.clone())?;
        if inspection.id != id {
            return Err(ErrorKind::Protocol.into());
        }
        let state = detail.state.as_ref().ok_or(ErrorKind::Protocol)?;
        let running = state.running.ok_or(ErrorKind::Protocol)?;
        let paused = state.paused.ok_or(ErrorKind::Protocol)?;
        let restarting = state.restarting.ok_or(ErrorKind::Protocol)?;
        if !running && !paused && !restarting {
            continue;
        }
        inspected.push(inspection);
    }
    Ok(inspected)
}

/// A complete daemon inventory. Ref enumeration is required before the agent can
/// claim that old managed versions are classified; the legacy current record is
/// insufficient evidence.
pub async fn daemon_images(
    config: &RuntimeConfig,
) -> Result<Vec<crate::DaemonImage>, RuntimeError> {
    let (docker, _) = discover(config).await?;
    let images = docker
        .list_images(Some(bollard::query_parameters::ListImagesOptions {
            all: true,
            ..Default::default()
        }))
        .await
        .map_err(classify)?;
    images
        .into_iter()
        .map(|image| {
            if image.id.is_empty() {
                return Err(ErrorKind::Protocol.into());
            }
            let refs = image
                .repo_tags
                .into_iter()
                .chain(image.repo_digests)
                .filter(|r| r != "<none>:<none>")
                .collect();
            Ok(crate::DaemonImage { id: image.id, refs })
        })
        .collect()
}

/// Includes running and stopped containers, regardless of Quasar ownership.
pub async fn all_container_image_ids(config: &RuntimeConfig) -> Result<Vec<String>, RuntimeError> {
    let (docker, _) = discover(config).await?;
    let containers = docker
        .list_containers(Some(bollard::query_parameters::ListContainersOptions {
            all: true,
            ..Default::default()
        }))
        .await
        .map_err(classify)?;
    containers
        .into_iter()
        .map(|container| {
            container
                .image_id
                .filter(|id| !id.is_empty())
                .ok_or(ErrorKind::Protocol.into())
        })
        .collect()
}

pub(crate) async fn engine_storage(
    config: &RuntimeConfig,
) -> Result<crate::EngineStorage, RuntimeError> {
    let (docker, _) = discover(config).await?;
    let root = docker
        .info()
        .await
        .map_err(classify)?
        .docker_root_dir
        .filter(|value| !value.is_empty())
        .filter(|value| std::path::Path::new(value).is_absolute())
        .ok_or(ErrorKind::Protocol)?;
    Ok(crate::EngineStorage {
        root: crate::DaemonHostPath(root.into()),
    })
}

pub(crate) async fn inspect_image_metadata(
    config: &RuntimeConfig,
    image: &str,
) -> Result<Option<crate::ImageMetadata>, RuntimeError> {
    if image.trim().is_empty() {
        return Err(ErrorKind::InvalidConfiguration.into());
    }
    let (docker, _) = discover(config).await?;
    match docker.inspect_image(image).await {
        Ok(info) => {
            let config = info.config.ok_or(ErrorKind::Protocol)?;
            Ok(Some(crate::ImageMetadata {
                id: info
                    .id
                    .filter(|value| !value.is_empty())
                    .ok_or(ErrorKind::Protocol)?,
                baked_env: config.env.unwrap_or_default(),
                working_dir: config.working_dir.filter(|value| !value.is_empty()),
            }))
        }
        Err(Error::DockerResponseServerError {
            status_code: 404, ..
        }) => Ok(None),
        Err(error) => Err(classify(error)),
    }
}
