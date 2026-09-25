//! SDK details of the platform-service lifecycles (`crate::platform`).
use super::{classify, credentials, discover, image_error};
use crate::platform::{
    ContainerSpec, EngineHost, PlatformContainer, PlatformImage, PlatformNetwork, PlatformVolume,
    Refused, RestartPolicy,
};
use crate::{ErrorKind, RuntimeConfig, RuntimeError};
use bollard::errors::Error;
use bollard::models::{
    ContainerCreateBody, ContainerUpdateBody, DeviceMapping, DeviceRequest, HealthConfig,
    HostConfig, NetworkCreateRequest, PortBinding, RestartPolicyNameEnum, VolumeCreateRequest,
};
use bollard::query_parameters::{
    CreateContainerOptions, CreateImageOptions, ListContainersOptions, LogsOptions,
    RemoveContainerOptions, RenameContainerOptions, StopContainerOptions, UploadToContainerOptions,
    WaitContainerOptions,
};
use futures_util::StreamExt;
use std::collections::BTreeMap;
use std::time::Duration;

const MAX_LOG_BYTES: usize = 64 * 1024;
const MAX_REFUSAL_BYTES: usize = 1024;

/// The engine's definite answer to a create or start it refused, with its own words; any
/// other failure stays a classified [`RuntimeError`].
fn refusal(error: Error) -> Result<Refused, RuntimeError> {
    match error {
        Error::DockerResponseServerError {
            status_code,
            message,
        } if !matches!(status_code, 401 | 403) => {
            let mut message = message;
            if message.len() > MAX_REFUSAL_BYTES {
                let mut end = MAX_REFUSAL_BYTES;
                while !message.is_char_boundary(end) {
                    end -= 1;
                }
                message.truncate(end);
            }
            Ok(Refused {
                status: status_code,
                message,
            })
        }
        other => Err(mutation_error(other)),
    }
}

fn status_code(error: &Error) -> Option<u16> {
    match error {
        Error::DockerResponseServerError { status_code, .. } => Some(*status_code),
        _ => None,
    }
}

/// A mutation whose request never reached a definite answer is of unknown outcome; a
/// definite refusal (4xx) is classified.
fn mutation_error(error: Error) -> RuntimeError {
    match status_code(&error) {
        Some(400..=499) => classify(error),
        Some(_) => ErrorKind::Engine.into(),
        None => match classify(error) {
            e if e.kind == ErrorKind::Unavailable || e.kind == ErrorKind::PermissionDenied => e,
            _ => ErrorKind::UnknownOutcome.into(),
        },
    }
}

pub(crate) async fn engine_host(config: &RuntimeConfig) -> Result<EngineHost, RuntimeError> {
    let (docker, _) = discover(config).await?;
    let sys = docker.info().await.map_err(classify)?;
    let mut runtimes: Vec<String> = sys.runtimes.unwrap_or_default().into_keys().collect();
    runtimes.sort();
    let mut cdi_devices: Vec<String> = sys
        .discovered_devices
        .unwrap_or_default()
        .into_iter()
        .filter_map(|d| d.id.filter(|v| !v.is_empty()))
        .collect();
    cdi_devices.sort();
    Ok(EngineHost {
        name: sys.name.filter(|v| !v.is_empty()),
        runtimes,
        cdi_devices,
    })
}

pub(crate) async fn pull(config: &RuntimeConfig, reference: &str) -> Result<(), RuntimeError> {
    let (docker, _) = discover(config).await?;
    let credentials = credentials::load(config, reference).await?;
    let tag = if reference.contains('@')
        || reference
            .rsplit('/')
            .next()
            .unwrap_or(reference)
            .contains(':')
    {
        None
    } else {
        Some("latest".to_owned())
    };
    let options = CreateImageOptions {
        from_image: Some(reference.to_owned()),
        tag,
        ..Default::default()
    };
    let mut stream = docker.create_image(Some(options), None, credentials);
    while let Some(event) = stream.next().await {
        match event {
            Ok(event) if event.error_detail.is_some() => return Err(ErrorKind::Engine.into()),
            Ok(_) => {}
            Err(error) => return Err(image_error(error)),
        }
    }
    Ok(())
}

pub(crate) async fn inspect_image(
    config: &RuntimeConfig,
    reference: &str,
) -> Result<Option<PlatformImage>, RuntimeError> {
    let (docker, _) = discover(config).await?;
    let image = match docker.inspect_image(reference).await {
        Ok(image) => image,
        Err(e) if status_code(&e) == Some(404) => return Ok(None),
        Err(e) => return Err(classify(e)),
    };
    Ok(Some(PlatformImage {
        id: image
            .id
            .filter(|v| !v.is_empty())
            .ok_or(ErrorKind::Protocol)?,
        repo_digests: image.repo_digests.unwrap_or_default(),
        labels: image
            .config
            .and_then(|c| c.labels)
            .unwrap_or_default()
            .into_iter()
            .collect(),
    }))
}

fn word<T: serde::Serialize>(value: &T) -> Option<String> {
    serde_json::to_value(value)
        .ok()
        .and_then(|v| v.as_str().map(str::to_owned))
        .filter(|v| !v.is_empty())
}

pub(crate) async fn inspect(
    config: &RuntimeConfig,
    name_or_id: &str,
) -> Result<Option<PlatformContainer>, RuntimeError> {
    let (docker, _) = discover(config).await?;
    inspect_with(&docker, name_or_id).await
}

async fn inspect_with(
    docker: &bollard::Docker,
    name_or_id: &str,
) -> Result<Option<PlatformContainer>, RuntimeError> {
    let info = match docker.inspect_container(name_or_id, None).await {
        Ok(info) => info,
        Err(e) if status_code(&e) == Some(404) => return Ok(None),
        Err(e) => return Err(classify(e)),
    };
    let config = info.config.ok_or(ErrorKind::Protocol)?;
    let state = info.state.ok_or(ErrorKind::Protocol)?;
    let command = info
        .path
        .filter(|p| !p.is_empty())
        .into_iter()
        .chain(info.args.unwrap_or_default())
        .collect();
    let restart = info
        .host_config
        .and_then(|h| h.restart_policy)
        .and_then(|p| p.name)
        .and_then(|n| match n {
            RestartPolicyNameEnum::NO | RestartPolicyNameEnum::EMPTY => Some(RestartPolicy::No),
            RestartPolicyNameEnum::UNLESS_STOPPED => Some(RestartPolicy::UnlessStopped),
            _ => None,
        });
    let mounts = info
        .mounts
        .unwrap_or_default()
        .into_iter()
        .filter_map(|m| {
            let source = m.name.filter(|n| !n.is_empty()).or(m.source)?;
            Some((source, m.destination?, !m.rw.unwrap_or(true)))
        })
        .collect();
    Ok(Some(PlatformContainer {
        id: info
            .id
            .filter(|v| !v.is_empty())
            .ok_or(ErrorKind::Protocol)?,
        name: info
            .name
            .map(|n| n.trim_start_matches('/').to_owned())
            .ok_or(ErrorKind::Protocol)?,
        image: config.image.unwrap_or_default(),
        image_id: info.image.unwrap_or_default(),
        labels: config.labels.unwrap_or_default().into_iter().collect(),
        status: state.status.as_ref().and_then(word).unwrap_or_default(),
        running: state.running.unwrap_or(false),
        health: state
            .health
            .and_then(|h| h.status)
            .as_ref()
            .and_then(word)
            .filter(|w| w != "none"),
        restart,
        mounts,
        command,
        env: config.env.unwrap_or_default(),
    }))
}

pub(crate) async fn list(config: &RuntimeConfig) -> Result<Vec<PlatformContainer>, RuntimeError> {
    let (docker, _) = discover(config).await?;
    let summaries = docker
        .list_containers(Some(ListContainersOptions {
            all: true,
            ..Default::default()
        }))
        .await
        .map_err(classify)?;
    let mut out = Vec::with_capacity(summaries.len());
    for summary in summaries {
        let Some(id) = summary.id else { continue };
        // A container removed between the listing and its inspection is simply gone.
        if let Some(container) = inspect_with(&docker, &id).await? {
            out.push(container);
        }
    }
    out.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(out)
}

fn engine_body(spec: &ContainerSpec) -> ContainerCreateBody {
    let restart = match spec.restart {
        RestartPolicy::No => RestartPolicyNameEnum::NO,
        RestartPolicy::UnlessStopped => RestartPolicyNameEnum::UNLESS_STOPPED,
    };
    const NANOS: i64 = 1_000_000_000;
    let secs = |s: u64| (s.min(i64::MAX as u64 / NANOS as u64) as i64) * NANOS;
    let port_key = |p: &crate::platform::PublishedPort| format!("{}/tcp", p.container_port);
    let exposed: Vec<String> = spec.ports.iter().map(port_key).collect();
    let mut bindings: std::collections::HashMap<String, Option<Vec<PortBinding>>> =
        Default::default();
    for p in &spec.ports {
        bindings
            .entry(port_key(p))
            .or_insert_with(|| Some(Vec::new()))
            .get_or_insert_with(Vec::new)
            .push(PortBinding {
                host_ip: p.host_ip.clone(),
                host_port: Some(p.host_port.to_string()),
            });
    }
    ContainerCreateBody {
        image: Some(spec.image.clone()),
        exposed_ports: (!exposed.is_empty()).then_some(exposed),
        healthcheck: spec.healthcheck.as_ref().map(|h| HealthConfig {
            test: Some(h.test.clone()),
            interval: Some(secs(h.interval_s)),
            timeout: Some(secs(h.timeout_s)),
            retries: Some(i64::from(h.retries)),
            start_period: Some(secs(h.start_period_s)),
            start_interval: None,
        }),
        entrypoint: spec.entrypoint.clone(),
        cmd: spec.cmd.clone(),
        env: Some(spec.env.iter().map(|(k, v)| format!("{k}={v}")).collect()),
        labels: Some(spec.labels.clone().into_iter().collect()),
        host_config: Some(HostConfig {
            network_mode: spec.network_mode.clone(),
            port_bindings: (!bindings.is_empty()).then_some(bindings),
            binds: Some(spec.binds.iter().map(|b| b.to_engine()).collect()),
            devices: Some(
                spec.devices
                    .iter()
                    .map(|d| DeviceMapping {
                        path_on_host: Some(d.host.clone()),
                        path_in_container: Some(d.container.clone()),
                        cgroup_permissions: Some(d.permissions.clone()),
                    })
                    .collect(),
            ),
            device_cgroup_rules: Some(spec.device_cgroup_rules.clone()),
            device_requests: (!spec.gpus.is_empty()).then(|| {
                spec.gpus
                    .iter()
                    .map(|g| DeviceRequest {
                        driver: g.driver.clone(),
                        count: Some(g.count),
                        device_ids: None,
                        capabilities: Some(g.capabilities.clone()),
                        options: None,
                    })
                    .collect()
            }),
            cap_add: Some(spec.cap_add.clone()),
            security_opt: Some(spec.security_opt.clone()),
            init: Some(spec.init),
            restart_policy: Some(bollard::models::RestartPolicy {
                name: Some(restart),
                maximum_retry_count: None,
            }),
            ..Default::default()
        }),
        ..Default::default()
    }
}

pub(crate) async fn create(
    config: &RuntimeConfig,
    spec: &ContainerSpec,
) -> Result<Result<String, Refused>, RuntimeError> {
    let (docker, _) = discover(config).await?;
    let created = match docker
        .create_container(
            Some(CreateContainerOptions {
                name: Some(spec.name.clone()),
                ..Default::default()
            }),
            engine_body(spec),
        )
        .await
    {
        Ok(created) => created,
        Err(e) => return refusal(e).map(Err),
    };
    if created.id.len() != 64 || !created.id.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Err(ErrorKind::Protocol.into());
    }
    Ok(Ok(created.id))
}

pub(crate) async fn start(
    config: &RuntimeConfig,
    id: &str,
) -> Result<Result<(), Refused>, RuntimeError> {
    let (docker, _) = discover(config).await?;
    match docker.start_container(id, None).await {
        Ok(()) => Ok(Ok(())),
        Err(e) if status_code(&e) == Some(304) => Ok(Ok(())),
        Err(e) => refusal(e).map(Err),
    }
}

pub(crate) async fn stop(
    config: &RuntimeConfig,
    id: &str,
    grace: Duration,
) -> Result<(), RuntimeError> {
    let (docker, _) = discover(config).await?;
    let options = StopContainerOptions {
        t: Some(grace.as_secs().min(i32::MAX as u64) as i32),
        signal: None,
    };
    match docker.stop_container(id, Some(options)).await {
        Ok(()) => Ok(()),
        Err(e) if status_code(&e) == Some(304) => Ok(()),
        Err(e) => Err(mutation_error(e)),
    }
}

pub(crate) async fn update_restart(
    config: &RuntimeConfig,
    id: &str,
    policy: RestartPolicy,
) -> Result<(), RuntimeError> {
    let (docker, _) = discover(config).await?;
    let name = match policy {
        RestartPolicy::No => RestartPolicyNameEnum::NO,
        RestartPolicy::UnlessStopped => RestartPolicyNameEnum::UNLESS_STOPPED,
    };
    docker
        .update_container(
            id,
            ContainerUpdateBody {
                restart_policy: Some(bollard::models::RestartPolicy {
                    name: Some(name),
                    maximum_retry_count: None,
                }),
                ..Default::default()
            },
        )
        .await
        .map_err(mutation_error)
}

pub(crate) async fn rename(
    config: &RuntimeConfig,
    id: &str,
    name: &str,
) -> Result<(), RuntimeError> {
    let (docker, _) = discover(config).await?;
    docker
        .rename_container(
            id,
            RenameContainerOptions {
                name: name.to_owned(),
            },
        )
        .await
        .map_err(mutation_error)
}

pub(crate) async fn remove(config: &RuntimeConfig, id: &str) -> Result<(), RuntimeError> {
    let (docker, _) = discover(config).await?;
    match docker
        .remove_container(
            id,
            Some(RemoveContainerOptions {
                force: true,
                v: true,
                ..Default::default()
            }),
        )
        .await
    {
        Ok(()) => Ok(()),
        Err(e) if status_code(&e) == Some(404) => Ok(()),
        Err(e) => Err(mutation_error(e)),
    }
}

pub(crate) async fn wait(config: &RuntimeConfig, id: &str) -> Result<i64, RuntimeError> {
    let (docker, _) = discover(config).await?;
    let mut stream = docker.wait_container(
        id,
        Some(WaitContainerOptions {
            condition: "not-running".into(),
        }),
    );
    match stream.next().await {
        Some(Ok(response)) => Ok(response.status_code),
        Some(Err(Error::DockerContainerWaitError { code, .. })) => Ok(code),
        Some(Err(e)) => Err(classify(e)),
        None => Err(ErrorKind::Protocol.into()),
    }
}

pub(crate) async fn logs_tail(
    config: &RuntimeConfig,
    id: &str,
    lines: usize,
) -> Result<String, RuntimeError> {
    let (docker, _) = discover(config).await?;
    let mut stream = docker.logs(
        id,
        Some(LogsOptions {
            stdout: true,
            stderr: true,
            tail: lines.to_string(),
            ..Default::default()
        }),
    );
    let mut out = Vec::new();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(classify)?;
        out.extend_from_slice(&chunk.into_bytes());
        if out.len() > MAX_LOG_BYTES {
            out.truncate(MAX_LOG_BYTES);
            break;
        }
    }
    Ok(String::from_utf8_lossy(&out).into_owned())
}

pub(crate) async fn upload(
    config: &RuntimeConfig,
    id: &str,
    path: &str,
    tar: Vec<u8>,
) -> Result<(), RuntimeError> {
    let (docker, _) = discover(config).await?;
    docker
        .upload_to_container(
            id,
            Some(UploadToContainerOptions {
                path: path.to_owned(),
                ..Default::default()
            }),
            bollard::body_full(bytes::Bytes::from(tar)),
        )
        .await
        .map_err(mutation_error)
}

fn volume(v: bollard::models::Volume) -> PlatformVolume {
    PlatformVolume {
        name: v.name,
        labels: v.labels.into_iter().collect(),
        mountpoint: Some(v.mountpoint).filter(|m| m.starts_with('/')),
    }
}

pub(crate) async fn inspect_network(
    config: &RuntimeConfig,
    name: &str,
) -> Result<Option<PlatformNetwork>, RuntimeError> {
    let (docker, _) = discover(config).await?;
    match docker
        .inspect_network(
            name,
            None::<bollard::query_parameters::InspectNetworkOptions>,
        )
        .await
    {
        Ok(n) => Ok(Some(PlatformNetwork {
            name: n.name.unwrap_or_else(|| name.to_owned()),
            labels: n.labels.unwrap_or_default().into_iter().collect(),
        })),
        Err(e) if status_code(&e) == Some(404) => Ok(None),
        Err(e) => Err(classify(e)),
    }
}

pub(crate) async fn create_network(
    config: &RuntimeConfig,
    name: &str,
    labels: BTreeMap<String, String>,
) -> Result<PlatformNetwork, RuntimeError> {
    let (docker, _) = discover(config).await?;
    docker
        .create_network(NetworkCreateRequest {
            name: name.to_owned(),
            driver: Some("bridge".into()),
            labels: Some(labels.clone().into_iter().collect()),
            ..Default::default()
        })
        .await
        .map_err(mutation_error)?;
    Ok(PlatformNetwork {
        name: name.to_owned(),
        labels,
    })
}

pub(crate) async fn inspect_volume(
    config: &RuntimeConfig,
    name: &str,
) -> Result<Option<PlatformVolume>, RuntimeError> {
    let (docker, _) = discover(config).await?;
    match docker.inspect_volume(name).await {
        Ok(v) => Ok(Some(volume(v))),
        Err(e) if status_code(&e) == Some(404) => Ok(None),
        Err(e) => Err(classify(e)),
    }
}

pub(crate) async fn create_volume(
    config: &RuntimeConfig,
    name: &str,
    labels: BTreeMap<String, String>,
) -> Result<PlatformVolume, RuntimeError> {
    let (docker, _) = discover(config).await?;
    docker
        .create_volume(VolumeCreateRequest {
            name: Some(name.to_owned()),
            labels: Some(labels.into_iter().collect()),
            ..Default::default()
        })
        .await
        .map(volume)
        .map_err(mutation_error)
}

pub(crate) async fn remove_network(config: &RuntimeConfig, name: &str) -> Result<(), RuntimeError> {
    let (docker, _) = discover(config).await?;
    match docker.remove_network(name).await {
        Ok(()) => Ok(()),
        Err(e) if status_code(&e) == Some(404) => Ok(()),
        Err(e) if status_code(&e) == Some(403) || status_code(&e) == Some(409) => {
            Err(ErrorKind::Busy.into())
        }
        Err(e) => Err(mutation_error(e)),
    }
}

pub(crate) async fn remove_volume(config: &RuntimeConfig, name: &str) -> Result<(), RuntimeError> {
    let (docker, _) = discover(config).await?;
    match docker
        .remove_volume(name, None::<bollard::query_parameters::RemoveVolumeOptions>)
        .await
    {
        Ok(()) => Ok(()),
        Err(e) if status_code(&e) == Some(404) => Ok(()),
        Err(e) if status_code(&e) == Some(409) => Err(ErrorKind::Busy.into()),
        Err(e) => Err(mutation_error(e)),
    }
}
