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
    let (docker, info) = discover(config).await?;
    let sys = docker.info().await.map_err(classify)?;
    let gpu_injection = crate::docker::gpu_injection_from_info(info.kind, &sys);
    let mut runtimes: Vec<String> = sys.runtimes.unwrap_or_default().into_keys().collect();
    runtimes.sort();
    let mut cdi_devices: Vec<String> = sys
        .discovered_devices
        .unwrap_or_default()
        .into_iter()
        .filter_map(|d| d.id.filter(|v| !v.is_empty()))
        .collect();
    cdi_devices.sort();
    let mode = crate::EngineMode::from_security_options(
        sys.security_options.as_deref().unwrap_or_default(),
    );
    Ok(EngineHost {
        name: sys.name.filter(|v| !v.is_empty()),
        runtimes,
        cdi_devices,
        rootless: mode == crate::EngineMode::Rootless,
        gpu_injection,
        kind: info.kind,
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
    let info = match super::inspect_container_tolerant(docker, name_or_id).await {
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
                        device_ids: (!g.device_ids.is_empty()).then(|| g.device_ids.clone()),
                        // A CDI request names devices, not capabilities.
                        capabilities: (!g.capabilities.is_empty()).then(|| g.capabilities.clone()),
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

/// How long a stop keeps working, once the engine's own stop has answered, to make the
/// container stay stopped (#425).
pub(crate) const STOP_SETTLE: Duration = Duration::from_secs(15);
/// How long to wait before looking again at a container the engine is still moving.
const SETTLE_POLL: Duration = Duration::from_millis(50);

/// What a read-back after a stop says about the container.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum AfterStop {
    /// Not running, and nothing the engine does on its own starts it again.
    Down,
    /// Running or paused: it needs a stop.
    Up,
    /// Podman: it exited by itself, and its restart policy starts it again. A stop now is
    /// a no-op the engine does not record.
    RestartPending,
    /// Being stopped or removed by someone: look again shortly.
    Moving,
}

/// A Podman libpod inspect read as [`AfterStop`]. From libpod's `container_internal.go`
/// (`shouldRestart`, `fullCleanup`), alike in 4.9 and 5.x: a container whose program
/// exited is `stopped` until its cleanup runs, and that cleanup restarts it unless the stop
/// was recorded (`StoppedByUser`) or the policy is `no`. A cleanup that did not restart it
/// leaves it `exited`, and only an explicit start runs it again. A container whose program
/// never ran is `created` (never started; also 4.9's word for one created in the OCI
/// runtime) or `initialized` (5.x's), and nothing restarts it either.
pub(crate) fn podman_after_stop(inspect: &serde_json::Value) -> AfterStop {
    let state = &inspect["State"];
    let status = state["Status"].as_str().unwrap_or_default();
    if state["Running"].as_bool() == Some(true) || matches!(status, "running" | "paused") {
        return AfterStop::Up;
    }
    if matches!(status, "stopping" | "removing") {
        return AfterStop::Moving;
    }
    let recorded = state["StoppedByUser"].as_bool() == Some(true);
    let restarts = !matches!(
        inspect["HostConfig"]["RestartPolicy"]["Name"]
            .as_str()
            .unwrap_or_default(),
        "" | "no"
    );
    if recorded || !restarts {
        return AfterStop::Down;
    }
    match status {
        "exited" | "created" | "initialized" | "configured" => AfterStop::Down,
        "stopped" => AfterStop::RestartPending,
        _ => AfterStop::Moving,
    }
}

/// The engine's stop, its "already stopped" (304) being success.
async fn stop_once(docker: &bollard::Docker, id: &str, grace: Duration) -> Result<(), Error> {
    let options = StopContainerOptions {
        t: Some(grace.as_secs().min(i32::MAX as u64) as i32),
        signal: None,
    };
    match docker.stop_container(id, Some(options)).await {
        Err(e) if status_code(&e) == Some(304) => Ok(()),
        other => other,
    }
}

/// Stop the container and make sure it stays stopped, on every engine (#425).
///
/// Docker records an explicit stop whatever state the container is in: a container
/// between two runs of a restart loop is `restarting`, which Docker stops and never
/// restarts. Podman does not. Its API answers a stop of a container that is not running
/// with 304 before reaching libpod's own stop, which is what records the stop
/// (`StoppedByUser`), so a crash-looping `unless-stopped` container found between two runs
/// is restarted by its own cleanup a moment later (4.9 and 5.x; the engine-mode suite's
/// `stop-crash-loop` case). Stopping again does not help: measured on both versions, no
/// stop in over a thousand found a container that exits at once running.
///
/// So every stop is read back until it holds. On Docker the read-back is one inspect.
/// On Podman it is the native inspect, read by [`podman_after_stop`], and a container
/// whose restart is pending is first `init`ed: created in the OCI runtime without its
/// program, which is the half of the restart its cleanup was about to do anyway. That
/// takes it out of the pending state (`init` resets the restart match, so the cleanup
/// does not start it), and a stop of a `created` container is libpod's own stop, which
/// records it. Nothing runs, so that stop needs no grace. Measured on Podman 4.9.3 and
/// 5.8.4: one round, the stop recorded, no further run.
///
/// The alternatives were rejected. Disabling the restart policy around the stop needs a
/// policy update Podman before 5.1 does not have (CI's 4.9 among them), and a crash
/// between the two updates would leave `no`, which the recovery actor and the seed read as
/// "Quasar's own stop" (ADR 0007). A plain stop-and-retry loop never wins against a fast
/// crash loop.
///
/// A container that is not down by [`STOP_SETTLE`] fails the stop (`Engine`); a read-back
/// that cannot be made leaves the outcome unknown.
pub(crate) async fn stop(
    config: &RuntimeConfig,
    id: &str,
    grace: Duration,
) -> Result<(), RuntimeError> {
    let (docker, info) = discover(config).await?;
    stop_once(&docker, id, grace)
        .await
        .map_err(mutation_error)?;
    let unknown = |_| RuntimeError::from(ErrorKind::UnknownOutcome);
    // A container removed meanwhile is stopped for good.
    let again = |grace: Duration| {
        let docker = &docker;
        async move {
            match stop_once(docker, id, grace).await {
                Err(e) if status_code(&e) == Some(404) => Ok(()),
                other => other.map_err(mutation_error),
            }
        }
    };
    let deadline = tokio::time::Instant::now() + STOP_SETTLE;
    loop {
        let seen = if info.kind == crate::EngineKind::Podman {
            super::libpod::inspect(config, id)
                .await
                .map_err(unknown)?
                .map_or(AfterStop::Down, |v| podman_after_stop(&v))
        } else {
            match inspect_with(&docker, id).await.map_err(unknown)? {
                Some(c) if c.running => AfterStop::Up,
                _ => AfterStop::Down,
            }
        };
        if seen == AfterStop::Down {
            return Ok(());
        }
        if tokio::time::Instant::now() >= deadline {
            // The engine keeps running it: the stop did not hold.
            return Err(ErrorKind::Engine.into());
        }
        match seen {
            AfterStop::Down => unreachable!("returned above"),
            AfterStop::Up => again(grace).await?,
            AfterStop::RestartPending => {
                // Refused: it was started meanwhile, and the next look stops it.
                let created = super::libpod::init(config, id).await.map_err(unknown)?;
                again(if created { Duration::ZERO } else { grace }).await?;
            }
            AfterStop::Moving => tokio::time::sleep(SETTLE_POLL).await,
        }
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
        .map_err(mutation_error)?;
    // ADR 0007 (RH-07): read the policy back on every engine, and treat a mismatch as a
    // failed step. The seed's rules and every stop Quasar means to keep depend on the
    // policy really being what was set.
    let realized = super::inspect_container_tolerant(&docker, id)
        .await
        .map_err(|_| RuntimeError::from(ErrorKind::UnknownOutcome))?
        .host_config
        .and_then(|h| h.restart_policy)
        .and_then(|p| p.name);
    let matches = match (policy, realized) {
        (RestartPolicy::UnlessStopped, Some(RestartPolicyNameEnum::UNLESS_STOPPED)) => true,
        // An engine may report "no" as the empty policy.
        (
            RestartPolicy::No,
            Some(RestartPolicyNameEnum::NO | RestartPolicyNameEnum::EMPTY) | None,
        ) => true,
        _ => false,
    };
    if !matches {
        // The engine accepted the update but reports another policy: the step failed.
        return Err(ErrorKind::Engine.into());
    }
    Ok(())
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
        driver: v.driver,
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
