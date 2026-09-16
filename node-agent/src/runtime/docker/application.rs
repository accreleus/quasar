//! Docker implementation of the application lifecycle. Bollard stays here.
use std::collections::HashMap;
use std::path::{Component, Path};

use bollard::{
    errors::Error,
    exec::{CreateExecOptions, StartExecOptions, StartExecResults},
    models::{
        ContainerCreateBody, ContainerStateStatusEnum, DeviceMapping, DeviceRequest, HostConfig,
        Mount, MountBindOptions, MountType, MountVolumeOptions,
    },
    query_parameters::{
        CreateContainerOptions, LogsOptions, RemoveContainerOptions, StartContainerOptions,
        StopContainerOptions,
    },
};
use futures_util::StreamExt;

use crate::runtime::application::{
    ApplicationIntent, ApplicationJournal, ApplicationMount, ApplicationPhase, ImageVolumeIdentity,
    NvidiaParamsRepair,
};
use crate::{
    container_ownership,
    runtime::{
        ApplicationId, ApplicationLogTail, ApplicationRequest, ApplicationResult, ErrorKind,
        RuntimeConfig, RuntimeError,
    },
};

const OPERATION_LABEL: &str = "io.quasar.application-operation";
// Keep a useful final tail (roughly a hundred normal application log lines),
// but never let JSON escaping of two streams evict the durable request.
const MAX_LOG_BYTES: usize = 16 * 1024;
/// Caller-facing final tail contract: enough normal records for diagnostics without
/// asking Docker to replay an unbounded application history on every observation.
const LOG_TAIL_LINES: usize = 100;
const MAX_JOURNAL_BYTES: usize = 64 * 1024;

async fn open(config: &RuntimeConfig) -> Result<bollard::Docker, RuntimeError> {
    Ok(super::discover(config).await?.0)
}

const REPAIR_MAX_STAGE: std::time::Duration = std::time::Duration::from_secs(3);

fn repair_budget(
    config: &RuntimeConfig,
    start_until: tokio::time::Instant,
) -> Option<std::time::Duration> {
    // Each stage is independently bounded while leaving most of the start
    // budget for create/start/verification. Docker may legitimately take more
    // than a few milliseconds while busy, so this is deliberately seconds.
    let remaining = start_until.saturating_duration_since(tokio::time::Instant::now());
    if remaining < std::time::Duration::from_millis(60) {
        return None;
    }
    Some(std::cmp::min(
        std::cmp::min(config.deadline / 6, REPAIR_MAX_STAGE),
        remaining / 6,
    ))
}

fn repair_outcome(
    intent: &mut ApplicationIntent,
    journal: &ApplicationJournal,
    outcome: &'static str,
) {
    if let Some(repair) = &mut intent.nvidia_params_repair {
        repair.outcome = Some(outcome.into());
    }
    let _ = journal.write(intent);
    tracing::warn!(token = "application-nvidia-proc-repair-incomplete", operation = %intent.request.operation, application = %intent.request.name, repair = outcome, "NVIDIA proc params repair incomplete");
}

async fn repair_nvidia_params(
    config: &RuntimeConfig,
    docker: &bollard::Docker,
    journal: &ApplicationJournal,
    intent: &mut ApplicationIntent,
    id: &ApplicationId,
    start_until: tokio::time::Instant,
) {
    if !intent.request.unmount_nvidia_params {
        return;
    }
    let Some(budget) = repair_budget(config, start_until) else {
        tracing::info!(operation = %intent.request.operation, application = %intent.request.name, repair = "deferred_budget", "NVIDIA proc params repair deferred");
        return;
    };
    let mut new_attempt = false;
    if intent.nvidia_params_repair.is_none() {
        intent.nvidia_params_repair = Some(NvidiaParamsRepair {
            attempted: true,
            exec_id: None,
            start_attempted: false,
            completed: false,
            outcome: None,
        });
        if journal.write(intent).is_err() {
            return;
        }
        new_attempt = true;
    }
    let repair = intent
        .nvidia_params_repair
        .as_ref()
        .expect("repair initialized");
    if repair.completed {
        return;
    }
    if repair.exec_id.is_none() {
        // A transport loss before Docker returns an exec ID cannot be safely
        // reconciled by creating another privileged exec.
        if !new_attempt {
            return;
        }
        match tokio::time::timeout(
            budget,
            docker.create_exec(
                id.as_str(),
                CreateExecOptions::<String> {
                    attach_stdout: Some(false),
                    attach_stderr: Some(false),
                    privileged: Some(true),
                    user: Some("root".into()),
                    cmd: Some(vec!["umount".into(), "/proc/driver/nvidia/params".into()]),
                    ..Default::default()
                },
            ),
        )
        .await
        {
            Ok(Ok(exec)) if !exec.id.is_empty() => {
                let repair = intent
                    .nvidia_params_repair
                    .as_mut()
                    .expect("repair initialized");
                repair.exec_id = Some(exec.id);
                if journal.write(intent).is_err() {
                    return;
                }
            }
            Ok(Ok(_)) => repair_outcome(intent, journal, "invalid_exec_id"),
            Ok(Err(_)) => repair_outcome(intent, journal, "create_failed"),
            Err(_) => repair_outcome(intent, journal, "create_timeout"),
        }
    }
    let Some(exec_id) = intent
        .nvidia_params_repair
        .as_ref()
        .and_then(|repair| repair.exec_id.clone())
    else {
        return;
    };
    // A recorded exec is not automatically ours: prove both the daemon exec
    // ID and its parent before starting or accepting its terminal result.
    let initial = match tokio::time::timeout(budget, docker.inspect_exec(&exec_id)).await {
        Ok(Ok(info))
            if info.id.as_deref() == Some(&exec_id)
                && info.container_id.as_deref() == Some(id.as_str()) =>
        {
            info
        }
        Ok(Ok(_)) => {
            repair_outcome(intent, journal, "foreign_exec");
            return;
        }
        Ok(Err(_)) => {
            repair_outcome(intent, journal, "inspect_failed");
            return;
        }
        Err(_) => {
            repair_outcome(intent, journal, "inspect_timeout");
            return;
        }
    };
    if initial.running == Some(false) && initial.exit_code.is_some() {
        let repair = intent
            .nvidia_params_repair
            .as_mut()
            .expect("repair initialized");
        repair.completed = true;
        repair.outcome = Some(format!("completed:{:?}", initial.exit_code));
        let _ = journal.write(intent);
        tracing::info!(operation = %intent.request.operation, application = %intent.request.name, exec = %exec_id, exit_code = ?initial.exit_code, "NVIDIA proc params repair completed");
        return;
    }
    if intent
        .nvidia_params_repair
        .as_ref()
        .is_some_and(|repair| repair.start_attempted)
        && initial.running != Some(true)
    {
        // A lost start reply can leave a created exec with no terminal exit
        // evidence. Do not start it again under a fresh assumption.
        repair_outcome(intent, journal, "start_terminal_unknown");
        return;
    }
    if !intent
        .nvidia_params_repair
        .as_ref()
        .is_some_and(|repair| repair.start_attempted)
        && initial.running != Some(true)
    {
        intent
            .nvidia_params_repair
            .as_mut()
            .expect("repair initialized")
            .start_attempted = true;
        if journal.write(intent).is_err() {
            return;
        }
        match tokio::time::timeout(
            budget,
            docker.start_exec(
                &exec_id,
                Some(StartExecOptions {
                    detach: false,
                    ..Default::default()
                }),
            ),
        )
        .await
        {
            Ok(Ok(StartExecResults::Attached { mut output, .. })) => {
                if tokio::time::timeout(budget, async { while output.next().await.is_some() {} })
                    .await
                    .is_err()
                {
                    repair_outcome(intent, journal, "drain_timeout");
                    return;
                }
            }
            Ok(Ok(StartExecResults::Detached)) => {}
            Ok(Err(_)) => repair_outcome(intent, journal, "start_failed"),
            Err(_) => repair_outcome(intent, journal, "start_timeout"),
        }
    }
    let until = tokio::time::Instant::now() + budget;
    loop {
        match tokio::time::timeout(budget, docker.inspect_exec(&exec_id)).await {
            Ok(Ok(info))
                if info.id.as_deref() == Some(&exec_id)
                    && info.container_id.as_deref() == Some(id.as_str())
                    && info.running == Some(false)
                    && info.exit_code.is_some() =>
            {
                let repair = intent
                    .nvidia_params_repair
                    .as_mut()
                    .expect("repair initialized");
                repair.completed = true;
                repair.outcome = Some(format!("completed:{:?}", info.exit_code));
                let _ = journal.write(intent);
                tracing::info!(operation = %intent.request.operation, application = %intent.request.name, exec = %exec_id, exit_code = ?info.exit_code, "NVIDIA proc params repair completed");
                return;
            }
            Ok(Ok(info))
                if info.id.as_deref() == Some(&exec_id)
                    && info.container_id.as_deref() == Some(id.as_str())
                    && info.running == Some(true)
                    && tokio::time::Instant::now() < until =>
            {
                tokio::time::sleep(std::time::Duration::from_millis(10)).await
            }
            Ok(Ok(info))
                if info.id.as_deref() != Some(&exec_id)
                    || info.container_id.as_deref() != Some(id.as_str()) =>
            {
                repair_outcome(intent, journal, "foreign_exec");
                return;
            }
            Ok(Ok(info)) if info.running == Some(false) && info.exit_code.is_none() => {
                repair_outcome(intent, journal, "terminal_exit_unknown");
                return;
            }
            Ok(Ok(_)) => {
                repair_outcome(intent, journal, "inspect_timeout");
                return;
            }
            Ok(Err(_)) => {
                repair_outcome(intent, journal, "inspect_failed");
                return;
            }
            Err(_) => {
                repair_outcome(intent, journal, "inspect_timeout");
                return;
            }
        }
    }
}
fn valid_id(id: &str) -> bool {
    id.len() == 64 && id.bytes().all(|v| v.is_ascii_hexdigit())
}
pub(super) fn owner(config: &RuntimeConfig) -> Result<String, RuntimeError> {
    #[cfg(test)]
    if let Some(owner) = &config.diagnostic_owner {
        return Ok(owner.clone());
    }
    #[cfg(not(test))]
    let _ = config;
    container_ownership::token().map_err(|_| ErrorKind::Unavailable.into())
}
fn uncertain(error: Error) -> RuntimeError {
    RuntimeError {
        kind: ErrorKind::UnknownOutcome,
        reconciliation: Some(super::classify(error).kind),
    }
}
fn identity(intent: &ApplicationIntent) -> Result<ApplicationId, RuntimeError> {
    let id = intent
        .id
        .clone()
        .filter(|id| valid_id(id))
        .ok_or(ErrorKind::UnknownOutcome)?;
    Ok(ApplicationId {
        id,
        operation: intent.request.operation.clone(),
    })
}
fn owns(intent: &ApplicationIntent, config: &RuntimeConfig, owner: &str) -> bool {
    intent.socket == config.socket && intent.owner == owner
}

fn body(intent: &ApplicationIntent) -> ContainerCreateBody {
    let r = &intent.request;
    let environment = canonical_env(&r.environment);
    let mut labels = HashMap::new();
    labels.insert(container_ownership::LABEL.into(), intent.owner.clone());
    labels.insert(OPERATION_LABEL.into(), r.operation.clone());
    ContainerCreateBody {
        // Resolve the mutable reference before journalling and create from the
        // immutable digest; the original reference remains caller evidence.
        image: Some(intent.image_id.clone().unwrap_or_else(|| r.image.clone())),
        entrypoint: r.entrypoint.clone(),
        // Omission preserves the image's Cmd; an explicit empty vector does
        // not. The same distinction applies to Entrypoint above.
        cmd: (!r.command.is_empty()).then_some(r.command.clone()),
        env: (!environment.is_empty()).then_some(environment),
        labels: Some(labels),
        host_config: Some(HostConfig {
            network_mode: Some(r.network.clone()),
            auto_remove: Some(false),
            cap_drop: r.security.cap_drop_all.then_some(vec!["ALL".into()]),
            cap_add: Some(r.security.cap_add.clone()),
            security_opt: Some({
                let mut options = r.security.security_opt.clone();
                if r.security.no_new_privileges {
                    options.push("no-new-privileges:true".into());
                }
                options
            }),
            readonly_rootfs: Some(r.security.read_only_rootfs),
            masked_paths: r.security.systempaths_unconfined.then_some(Vec::new()),
            readonly_paths: r.security.systempaths_unconfined.then_some(Vec::new()),
            pids_limit: Some(r.security.pids_limit),
            shm_size: Some(r.security.shm_size),
            group_add: Some(r.group_add.clone()),
            devices: Some(
                r.devices
                    .iter()
                    .map(|path| DeviceMapping {
                        path_on_host: Some(path.clone()),
                        path_in_container: Some(path.clone()),
                        cgroup_permissions: Some("rwm".into()),
                    })
                    .collect(),
            ),
            device_requests: r.nvidia_gpu.then_some(vec![DeviceRequest {
                driver: Some("nvidia".into()),
                count: Some(-1),
                capabilities: Some(vec![vec!["gpu".into()]]),
                ..Default::default()
            }]),
            binds: Some(r.mounts.clone()),
            mounts: Some(
                r.typed_mounts
                    .iter()
                    .map(|mount| match mount {
                        ApplicationMount::Bind {
                            source,
                            target,
                            read_only,
                            consistency,
                        } => Mount {
                            source: Some(source.clone()),
                            target: Some(target.clone()),
                            typ: Some(MountType::BIND),
                            read_only: Some(*read_only),
                            consistency: consistency.clone(),
                            // `--mount type=bind` must fail for a missing source;
                            // the legacy `-v` compatibility behavior is different.
                            bind_options: Some(MountBindOptions {
                                create_mountpoint: Some(false),
                                ..Default::default()
                            }),
                            ..Default::default()
                        },
                        ApplicationMount::Volume {
                            source,
                            target,
                            read_only,
                            no_copy,
                        } => Mount {
                            source: Some(source.clone()),
                            target: Some(target.clone()),
                            typ: Some(MountType::VOLUME),
                            read_only: Some(*read_only),
                            volume_options: Some(MountVolumeOptions {
                                no_copy: Some(*no_copy),
                                ..Default::default()
                            }),
                            ..Default::default()
                        },
                    })
                    .collect(),
            ),
            ..Default::default()
        }),
        ..Default::default()
    }
}

fn canonical_env(entries: &[String]) -> Vec<String> {
    let mut final_values = HashMap::new();
    for entry in entries {
        if let Some((key, value)) = entry.split_once('=') {
            final_values.insert(key, value);
        }
    }
    entries
        .iter()
        .filter(|entry| {
            entry
                .split_once('=')
                .is_some_and(|(key, value)| final_values.get(key) == Some(&value))
        })
        .fold(Vec::new(), |mut values, entry| {
            if !values.iter().any(|existing| {
                existing.split_once('=').map(|(key, _)| key)
                    == entry.split_once('=').map(|(key, _)| key)
            }) {
                values.push(entry.clone());
            }
            values
        })
}

#[derive(Debug, PartialEq, Eq)]
struct LegacyMount<'a> {
    source: &'a str,
    target: &'a str,
    read_only: bool,
}

// `-v` is intentionally retained as its Docker wire form so its SELinux,
// nocopy, and consistency suffixes survive migration. Inspect has no copy of
// those suffixes, but it must still prove the resulting source, destination,
// and read-only state.
fn parse_legacy_mount(value: &str) -> Result<LegacyMount<'_>, RuntimeError> {
    let mut parts = value.splitn(3, ':');
    let source = parts
        .next()
        .filter(|v| !v.is_empty())
        .ok_or(ErrorKind::Protocol)?;
    let target = parts
        .next()
        .filter(|v| !v.is_empty())
        .ok_or(ErrorKind::Protocol)?;
    let read_only = parts.next().is_some_and(|options| {
        options
            .split(',')
            .any(|option| option == "ro" || option == "readonly")
    });
    Ok(LegacyMount {
        source,
        target,
        read_only,
    })
}

/// Docker normalizes `.` and trailing separators in mount paths. Compare that
/// lexical identity only: resolving a symlink here would use the agent's view,
/// which may differ from dockerd's.
fn lexical_path(value: &str) -> Result<String, RuntimeError> {
    let path = Path::new(value);
    if !path.is_absolute() {
        return Err(ErrorKind::Protocol.into());
    }
    let mut normalized = String::from("/");
    for component in path.components() {
        match component {
            Component::RootDir | Component::CurDir => {}
            Component::Normal(component) => {
                if normalized.len() > 1 {
                    normalized.push('/');
                }
                normalized.push_str(component.to_str().ok_or(ErrorKind::Protocol)?);
            }
            Component::ParentDir | Component::Prefix(_) => return Err(ErrorKind::Protocol.into()),
        }
    }
    Ok(normalized)
}

fn same_path(left: &str, right: &str) -> bool {
    matches!(
        (lexical_path(left), lexical_path(right)),
        (Ok(left), Ok(right)) if left == right
    )
}

fn safe_default_bind_options(options: Option<&MountBindOptions>) -> bool {
    let Some(options) = options else {
        return true;
    };
    let safe_propagation = options.propagation.is_none()
        || matches!(
            options.propagation,
            Some(
                bollard::models::MountBindOptionsPropagationEnum::PRIVATE
                    | bollard::models::MountBindOptionsPropagationEnum::RPRIVATE
            )
        );
    options.create_mountpoint != Some(true)
        && safe_propagation
        && options.non_recursive != Some(true)
        && options.read_only_non_recursive != Some(true)
        && options.read_only_force_recursive != Some(true)
}

fn matches_typed_request(actual: &Mount, wanted: &ApplicationMount) -> bool {
    match wanted {
        ApplicationMount::Bind {
            source,
            target,
            read_only,
            consistency,
        } => {
            actual.typ == Some(MountType::BIND)
                && actual.source.as_deref().is_some_and(|actual| same_path(actual, source))
                && actual.target.as_deref().is_some_and(|actual| same_path(actual, target))
                && actual.read_only.unwrap_or(false) == *read_only
                && actual.consistency == *consistency
                // Docker may omit false/default fields in inspect. It may not
                // turn this typed bind into a source-creating or propagated one.
                && safe_default_bind_options(actual.bind_options.as_ref())
                && actual.volume_options.is_none()
        }
        ApplicationMount::Volume {
            source,
            target,
            read_only,
            no_copy,
        } => {
            actual.typ == Some(MountType::VOLUME)
                && actual.source.as_deref() == Some(source)
                && actual
                    .target
                    .as_deref()
                    .is_some_and(|actual| same_path(actual, target))
                && actual.read_only.unwrap_or(false) == *read_only
                && actual.bind_options.is_none()
                && match actual.volume_options.as_ref() {
                    Some(options) => options.no_copy.unwrap_or(false) == *no_copy,
                    // Omission is Docker's representation of NoCopy=false;
                    // a requested true must remain explicit in HostConfig.
                    None => !*no_copy,
                }
        }
    }
}

fn matches_realized_typed(actual: &bollard::models::MountPoint, wanted: &ApplicationMount) -> bool {
    match wanted {
        ApplicationMount::Bind {
            source,
            target,
            read_only,
            ..
        } => {
            actual.typ.as_deref() == Some("bind")
                && actual
                    .source
                    .as_deref()
                    .is_some_and(|actual| same_path(actual, source))
                && actual
                    .destination
                    .as_deref()
                    .is_some_and(|actual| same_path(actual, target))
                && actual.rw == Some(!read_only)
        }
        ApplicationMount::Volume {
            source,
            target,
            read_only,
            ..
        } => {
            actual.typ.as_deref() == Some("volume")
                && actual.name.as_deref() == Some(source)
                && actual
                    .destination
                    .as_deref()
                    .is_some_and(|actual| same_path(actual, target))
                && actual.rw == Some(!read_only)
        }
    }
}

fn matches_realized_legacy(actual: &bollard::models::MountPoint, wanted: &LegacyMount<'_>) -> bool {
    let volume = !wanted.source.starts_with('/');
    actual.typ.as_deref() == Some(if volume { "volume" } else { "bind" })
        && if volume {
            actual.name.as_deref() == Some(wanted.source)
        } else {
            actual
                .source
                .as_deref()
                .is_some_and(|actual| same_path(actual, wanted.source))
        }
        && actual
            .destination
            .as_deref()
            .is_some_and(|actual| same_path(actual, wanted.target))
        && actual.rw == Some(!wanted.read_only)
}

fn exact_nvidia_all_request(request: &DeviceRequest) -> bool {
    request.driver.as_deref() == Some("nvidia")
        && request.count == Some(-1)
        && request.device_ids.as_ref().is_none_or(Vec::is_empty)
        && request.capabilities.as_deref() == Some(&[vec!["gpu".to_owned()]])
        && request.options.as_ref().is_none_or(HashMap::is_empty)
}

fn canonical_capabilities(values: Option<&Vec<String>>) -> Vec<String> {
    let mut values = values
        .cloned()
        .unwrap_or_default()
        .into_iter()
        .map(|value| value.trim_start_matches("CAP_").to_ascii_uppercase())
        .collect::<Vec<_>>();
    values.sort();
    values
}

fn canonical_security_options(values: Option<&Vec<String>>) -> Vec<String> {
    let mut values = values.cloned().unwrap_or_default();
    values.sort();
    values
}

fn explicit_mount_targets(request: &ApplicationRequest) -> Result<Vec<String>, RuntimeError> {
    let mut targets = Vec::with_capacity(request.mounts.len() + request.typed_mounts.len());
    for mount in &request.mounts {
        let mount = parse_legacy_mount(mount)?;
        if mount.source.starts_with('/') {
            lexical_path(mount.source)?;
        }
        targets.push(lexical_path(mount.target)?);
    }
    for mount in &request.typed_mounts {
        let target = match mount {
            ApplicationMount::Bind { source, target, .. } => {
                lexical_path(source)?;
                target
            }
            ApplicationMount::Volume { target, .. } => target,
        };
        targets.push(lexical_path(target)?);
    }
    targets.sort();
    if targets.windows(2).any(|pair| pair[0] == pair[1]) {
        return Err(ErrorKind::InvalidConfiguration.into());
    }
    Ok(targets)
}

fn image_volume_targets(intent: &ApplicationIntent) -> Result<Vec<String>, RuntimeError> {
    let mut targets = intent
        .image_volumes
        .as_deref()
        .unwrap_or_default()
        .iter()
        .map(|target| lexical_path(target))
        .collect::<Result<Vec<_>, _>>()?;
    targets.sort();
    if targets.windows(2).any(|pair| pair[0] == pair[1]) {
        return Err(ErrorKind::Protocol.into());
    }
    Ok(targets)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ApplicationState {
    Created,
    Running,
    Exited {
        exit_code: Option<i64>,
        oom_killed: Option<bool>,
    },
}

/// Prove only immutable operation ownership and lifecycle state. This deliberately
/// excludes launch requirements: a create response has already bound this exact ID, and
/// cleanup must be able to retire it even when later realization verification rejected
/// mounts, devices, or security posture.
async fn inspect_identity_state(
    docker: &bollard::Docker,
    intent: &ApplicationIntent,
) -> Result<ApplicationState, RuntimeError> {
    let id = identity(intent)?;
    let info = docker
        .inspect_container(id.as_str(), None)
        .await
        .map_err(uncertain)?;
    if info.id.as_deref() != Some(id.as_str())
        || info.name.as_deref() != Some(format!("/{}", intent.request.name).as_str())
        || intent
            .image_id
            .as_deref()
            .is_none_or(|image_id| info.image.as_deref() != Some(image_id))
        || info
            .config
            .as_ref()
            .and_then(|c| c.labels.as_ref())
            .and_then(|v| v.get(container_ownership::LABEL))
            != Some(&intent.owner)
        || info
            .config
            .as_ref()
            .and_then(|c| c.labels.as_ref())
            .and_then(|v| v.get(OPERATION_LABEL))
            != Some(&intent.request.operation)
    {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    let state = info.state.as_ref().ok_or(ErrorKind::UnknownOutcome)?;
    match state.status {
        Some(ContainerStateStatusEnum::CREATED) if state.running == Some(false) => {
            Ok(ApplicationState::Created)
        }
        Some(ContainerStateStatusEnum::RUNNING) if state.running == Some(true) => {
            Ok(ApplicationState::Running)
        }
        Some(ContainerStateStatusEnum::EXITED | ContainerStateStatusEnum::DEAD)
            if state.running == Some(false) =>
        {
            Ok(ApplicationState::Exited {
                exit_code: state.exit_code,
                oom_killed: state.oom_killed,
            })
        }
        _ => Err(ErrorKind::Protocol.into()),
    }
}

async fn inspect_owned(
    docker: &bollard::Docker,
    intent: &ApplicationIntent,
) -> Result<(ApplicationState, Vec<ImageVolumeIdentity>), RuntimeError> {
    let id = identity(intent)?;
    let info = docker
        .inspect_container(id.as_str(), None)
        .await
        .map_err(uncertain)?;
    if info.id.as_deref() != Some(id.as_str())
        || info.name.as_deref() != Some(format!("/{}", intent.request.name).as_str())
        || intent
            .image_id
            .as_deref()
            .is_none_or(|image_id| info.image.as_deref() != Some(image_id))
        || info
            .config
            .as_ref()
            .and_then(|c| c.labels.as_ref())
            .and_then(|v| v.get(container_ownership::LABEL))
            != Some(&intent.owner)
        || info
            .config
            .as_ref()
            .and_then(|c| c.labels.as_ref())
            .and_then(|v| v.get(OPERATION_LABEL))
            != Some(&intent.request.operation)
        || info.config.as_ref().and_then(|c| c.image.as_deref())
            != Some(
                intent
                    .image_id
                    .as_deref()
                    .unwrap_or(intent.request.image.as_str()),
            )
    {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    // Inherited image environment is allowed. Request values were canonicalized
    // to their final caller precedence before create; that final value must be
    // the last realized value too.
    let requested_env = canonical_env(&intent.request.environment);
    let realized_env = info.config.as_ref().and_then(|c| c.env.as_ref());
    for wanted in requested_env {
        let realized_env = realized_env.ok_or(ErrorKind::Protocol)?;
        let (key, value) = wanted.split_once('=').ok_or(ErrorKind::Protocol)?;
        if realized_env
            .iter()
            .rev()
            .find_map(|entry| entry.split_once('=').filter(|(actual, _)| actual == &key))
            != Some((key, value))
        {
            return Err(ErrorKind::Protocol.into());
        }
    }
    let config = info.config.as_ref().ok_or(ErrorKind::UnknownOutcome)?;
    fn normalized_argv(value: Option<&Vec<String>>) -> Option<&Vec<String>> {
        value.filter(|argv| !argv.is_empty())
    }
    fn normalized_user(value: Option<&String>) -> Option<&String> {
        value.filter(|user| !user.is_empty())
    }
    let entrypoint = normalized_argv(
        intent
            .request
            .entrypoint
            .as_ref()
            .or(intent.image_entrypoint.as_ref()),
    );
    let command = normalized_argv(
        (!intent.request.command.is_empty())
            .then_some(&intent.request.command)
            .or(intent.image_cmd.as_ref()),
    );
    if normalized_argv(config.entrypoint.as_ref()) != entrypoint
        || normalized_argv(config.cmd.as_ref()) != command
        || normalized_user(config.user.as_ref()) != normalized_user(intent.image_user.as_ref())
    {
        return Err(ErrorKind::Protocol.into());
    }
    let host = info.host_config.as_ref().ok_or(ErrorKind::UnknownOutcome)?;
    let normalized = |values: Option<&Vec<String>>| values.cloned().unwrap_or_default();
    if host.network_mode.as_deref() != Some(&intent.request.network)
        || host.auto_remove.unwrap_or(false)
        || host.privileged.unwrap_or(false)
        || host
            .pid_mode
            .as_deref()
            .is_some_and(|mode| !mode.is_empty() && mode != "private")
        || host
            .ipc_mode
            .as_deref()
            .is_some_and(|mode| !mode.is_empty() && mode != "private")
        || host
            .uts_mode
            .as_deref()
            .is_some_and(|mode| !mode.is_empty())
        || host
            .userns_mode
            .as_deref()
            .is_some_and(|mode| !mode.is_empty())
        || matches!(
            host.cgroupns_mode,
            Some(bollard::models::HostConfigCgroupnsModeEnum::HOST)
        )
        || host.runtime.as_deref().is_some_and(|runtime| {
            !runtime.is_empty()
                && runtime != "runc"
                && !(intent.request.nvidia_gpu && runtime == "nvidia")
        })
        || host.readonly_rootfs.unwrap_or(false) != intent.request.security.read_only_rootfs
        || host.pids_limit != Some(intent.request.security.pids_limit)
        || host.shm_size != Some(intent.request.security.shm_size)
        || normalized(host.group_add.as_ref()) != intent.request.group_add
        || normalized(host.binds.as_ref()) != intent.request.mounts
        || canonical_capabilities(host.cap_add.as_ref())
            != canonical_capabilities(Some(&intent.request.security.cap_add))
        || canonical_capabilities(host.cap_drop.as_ref())
            != if intent.request.security.cap_drop_all {
                vec![String::from("ALL")]
            } else {
                Vec::new()
            }
    {
        return Err(ErrorKind::Protocol.into());
    }
    let mut wanted_security = intent.request.security.security_opt.clone();
    if intent.request.security.no_new_privileges {
        wanted_security.push("no-new-privileges:true".into());
    }
    if canonical_security_options(host.security_opt.as_ref())
        != canonical_security_options(Some(&wanted_security))
    {
        return Err(ErrorKind::Protocol.into());
    }
    if intent.request.security.systempaths_unconfined
        && (host.masked_paths.as_deref() != Some(&[])
            || host.readonly_paths.as_deref() != Some(&[]))
    {
        return Err(ErrorKind::Protocol.into());
    }
    let devices = host.devices.as_deref().unwrap_or(&[]);
    if devices.len() != intent.request.devices.len()
        || devices
            .iter()
            .zip(&intent.request.devices)
            .any(|(actual, wanted)| {
                actual.path_on_host.as_deref() != Some(wanted)
                    || actual.path_in_container.as_deref() != Some(wanted)
                    || actual.cgroup_permissions.as_deref() != Some("rwm")
            })
    {
        return Err(ErrorKind::Protocol.into());
    }
    if intent.request.nvidia_gpu {
        if !matches!(host.device_requests.as_deref(), Some([request]) if exact_nvidia_all_request(request))
        {
            return Err(ErrorKind::Protocol.into());
        }
    } else if host
        .device_requests
        .as_ref()
        .is_some_and(|requests| !requests.is_empty())
    {
        return Err(ErrorKind::Protocol.into());
    }
    // HostConfig.Mounts carries the requested typed details that are absent from
    // MountPoint (CreateMountpoint, NoCopy, consistency). Check it separately
    // from MountPoint, whose source/type/RW values prove what Docker realized.
    let requested_typed = host.mounts.as_deref().unwrap_or(&[]);
    let mut unmatched_typed = requested_typed.iter().collect::<Vec<_>>();
    for wanted in &intent.request.typed_mounts {
        let Some(position) = unmatched_typed
            .iter()
            .position(|actual| matches_typed_request(actual, wanted))
        else {
            return Err(ErrorKind::Protocol.into());
        };
        unmatched_typed.remove(position);
    }
    if !unmatched_typed.is_empty() {
        return Err(ErrorKind::Protocol.into());
    }
    let legacy = intent
        .request
        .mounts
        .iter()
        .map(|value| parse_legacy_mount(value))
        .collect::<Result<Vec<_>, _>>()?;
    let explicit_targets = explicit_mount_targets(&intent.request)?;
    let image_targets = image_volume_targets(intent)?;
    let mut unmatched_realized = info
        .mounts
        .as_deref()
        .unwrap_or(&[])
        .iter()
        .collect::<Vec<_>>();
    for wanted in &intent.request.typed_mounts {
        let Some(position) = unmatched_realized
            .iter()
            .position(|actual| matches_realized_typed(actual, wanted))
        else {
            return Err(ErrorKind::Protocol.into());
        };
        unmatched_realized.remove(position);
    }
    for wanted in &legacy {
        let Some(position) = unmatched_realized
            .iter()
            .position(|actual| matches_realized_legacy(actual, wanted))
        else {
            return Err(ErrorKind::Protocol.into());
        };
        unmatched_realized.remove(position);
    }
    let mut learned_volumes = Vec::new();
    for target in image_targets
        .iter()
        .filter(|target| !explicit_targets.iter().any(|explicit| explicit == *target))
    {
        let known = intent
            .image_volume_identities
            .as_deref()
            .and_then(|identities| {
                identities
                    .iter()
                    .find(|identity| identity.target == *target)
            });
        let Some(position) = unmatched_realized.iter().position(|actual| {
            actual.typ.as_deref() == Some("volume")
                && actual
                    .destination
                    .as_deref()
                    .is_some_and(|destination| same_path(destination, target))
                && known.is_none_or(|identity| {
                    actual.name == identity.name && actual.source == identity.source
                })
                && actual.rw == Some(true)
                && actual.name.as_deref().is_some_and(|name| !name.is_empty())
                && actual
                    .source
                    .as_deref()
                    .is_some_and(|source| lexical_path(source).is_ok())
        }) else {
            return Err(ErrorKind::Protocol.into());
        };
        let actual = unmatched_realized.remove(position);
        learned_volumes.push(ImageVolumeIdentity {
            target: target.clone(),
            name: actual.name.clone(),
            source: actual.source.clone(),
        });
    }
    if !unmatched_realized.is_empty() {
        return Err(ErrorKind::Protocol.into());
    }
    let state = info.state.as_ref().ok_or(ErrorKind::UnknownOutcome)?;
    match state.status {
        Some(ContainerStateStatusEnum::CREATED) if state.running == Some(false) => {
            Ok((ApplicationState::Created, learned_volumes))
        }
        Some(ContainerStateStatusEnum::RUNNING) if state.running == Some(true) => {
            Ok((ApplicationState::Running, learned_volumes))
        }
        Some(ContainerStateStatusEnum::EXITED | ContainerStateStatusEnum::DEAD)
            if state.running == Some(false) =>
        {
            Ok((
                ApplicationState::Exited {
                    exit_code: state.exit_code,
                    oom_killed: state.oom_killed,
                },
                learned_volumes,
            ))
        }
        _ => Err(ErrorKind::Protocol.into()),
    }
}

/// Create, start and verify the one durable application operation. A lost
/// create/start reply is reconciled by the recorded immutable ID; it never
/// creates a second workload or falls back to the CLI.
pub(crate) async fn start(
    config: &RuntimeConfig,
    request: ApplicationRequest,
) -> Result<ApplicationId, RuntimeError> {
    let start_until = tokio::time::Instant::now() + config.deadline;
    if !request.is_valid() || explicit_mount_targets(&request).is_err() {
        return Err(ErrorKind::InvalidConfiguration.into());
    }
    let owner = owner(config)?;
    let docker = open(config).await?;
    let journal = ApplicationJournal::acquire(config, &request.operation).await?;
    let recovered = journal.read()?;
    let mut intent = match recovered.as_ref() {
        Some(intent) if owns(intent, config, &owner) && intent.request == request => intent.clone(),
        Some(_) => return Err(ErrorKind::UnknownOutcome.into()),
        None => {
            let image = match docker.inspect_image(&request.image).await {
                Ok(image) => image,
                Err(Error::DockerResponseServerError {
                    status_code: 404, ..
                }) => return Err(ErrorKind::Missing.into()),
                Err(error) => return Err(super::classify(error)),
            };
            let image_id = image
                .id
                .filter(|id| !id.is_empty())
                .ok_or(ErrorKind::Protocol)?;
            let image_config = image.config.ok_or(ErrorKind::Protocol)?;
            match docker.inspect_container(&request.name, None).await {
                Err(Error::DockerResponseServerError {
                    status_code: 404, ..
                }) => {}
                Ok(_) => return Err(ErrorKind::UnknownOutcome.into()),
                Err(e) => return Err(super::classify(e)),
            }
            let intent = ApplicationIntent {
                request,
                owner,
                socket: config.socket.clone(),
                id: None,
                image_id: Some(image_id),
                image_entrypoint: image_config.entrypoint,
                image_cmd: image_config.cmd,
                image_user: image_config.user,
                image_volumes: image_config.volumes,
                image_volume_identities: None,
                nvidia_params_repair: None,
                phase: ApplicationPhase::Creating,
                result: None,
            };
            journal.write(&intent)?;
            intent
        }
    };
    if intent.id.is_none() {
        // An uncertain create response is never permission to create another
        // workload. Reconcile the same name and adopt only when its immutable
        // labels prove it is this exact recorded operation.
        if recovered.is_some() {
            let info = docker
                .inspect_container(&intent.request.name, None)
                .await
                .map_err(uncertain)?;
            if info
                .config
                .as_ref()
                .and_then(|c| c.labels.as_ref())
                .and_then(|labels| labels.get(container_ownership::LABEL))
                != Some(&intent.owner)
                || info
                    .config
                    .as_ref()
                    .and_then(|c| c.labels.as_ref())
                    .and_then(|labels| labels.get(OPERATION_LABEL))
                    != Some(&intent.request.operation)
            {
                return Err(ErrorKind::UnknownOutcome.into());
            }
            intent.id = info.id.filter(|id| valid_id(id));
            if intent.id.is_none() {
                return Err(ErrorKind::UnknownOutcome.into());
            }
            // A name learned after a lost create reply is not create-response authority.
            // Prove the complete pinned request before persisting that learned ID; otherwise
            // a rejected realization could later be laundered into identity-only cleanup.
            let _ = inspect_owned(&docker, &intent).await?;
            intent.phase = ApplicationPhase::Created;
            journal.write(&intent)?;
        } else {
            let created = docker
                .create_container(
                    Some(CreateContainerOptions {
                        name: Some(intent.request.name.clone()),
                        ..Default::default()
                    }),
                    body(&intent),
                )
                .await;
            match created {
                Ok(response) if valid_id(&response.id) => {
                    intent.id = Some(response.id);
                    intent.phase = ApplicationPhase::Created;
                    journal.write(&intent)?
                }
                Ok(_) => return Err(ErrorKind::Protocol.into()),
                Err(Error::DockerResponseServerError {
                    status_code: 400..=499,
                    ..
                }) => {
                    // Keep the partial operation durable. Startup retirement
                    // can prove its named container absent; discarding here
                    // would make a lost/ambiguous create unrecoverable.
                    intent.phase = ApplicationPhase::CleanupPending;
                    journal.write(&intent)?;
                    return Err(ErrorKind::Engine.into());
                }
                Err(_) => return Err(ErrorKind::UnknownOutcome.into()),
            }
        }
    }
    let id = identity(&intent)?;
    let (state, learned_volumes) = inspect_owned(&docker, &intent).await?;
    if intent.image_volume_identities.is_none() && intent.image_volumes.is_some() {
        intent.image_volume_identities = Some(learned_volumes);
    }
    journal.write(&intent)?;
    if state == ApplicationState::Running {
        intent.phase = ApplicationPhase::Running;
        journal.write(&intent)?;
        repair_nvidia_params(config, &docker, &journal, &mut intent, &id, start_until).await;
        return Ok(id);
    }
    if matches!(state, ApplicationState::Exited { .. }) {
        intent.phase = ApplicationPhase::Stopped;
        journal.write(&intent)?;
        return Ok(id);
    }
    if !matches!(
        intent.phase,
        ApplicationPhase::Created | ApplicationPhase::Starting
    ) {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    if intent.phase == ApplicationPhase::Created {
        intent.phase = ApplicationPhase::Starting;
        journal.write(&intent)?;
        if let Err(error) = docker
            .start_container(id.as_str(), None::<StartContainerOptions>)
            .await
        {
            if matches!(
                error,
                Error::DockerResponseServerError {
                    status_code: 400..=499,
                    ..
                }
            ) {
                return Err(super::classify(error));
            }
            // A transport failure may follow a successful daemon mutation.
            // Reconcile the immutable ID before calling the result unknown.
            match inspect_owned(&docker, &intent).await?.0 {
                ApplicationState::Running | ApplicationState::Exited { .. } => {}
                ApplicationState::Created => return Err(ErrorKind::UnknownOutcome.into()),
            }
        }
    }
    let state = inspect_owned(&docker, &intent).await?.0;
    intent.phase = if state == ApplicationState::Running {
        ApplicationPhase::Running
    } else if matches!(state, ApplicationState::Exited { .. }) {
        ApplicationPhase::Stopped
    } else {
        return Err(ErrorKind::UnknownOutcome.into());
    };
    journal.write(&intent)?;
    if intent.phase == ApplicationPhase::Running {
        repair_nvidia_params(config, &docker, &journal, &mut intent, &id, start_until).await;
    }
    Ok(id)
}

async fn logs(
    docker: &bollard::Docker,
    id: &ApplicationId,
) -> Result<(String, String), RuntimeError> {
    let mut stdout = Vec::new();
    let mut stderr = Vec::new();
    let mut stream = docker.logs(
        id.as_str(),
        Some(LogsOptions {
            follow: false,
            stdout: true,
            stderr: true,
            timestamps: false,
            tail: LOG_TAIL_LINES.to_string(),
            ..Default::default()
        }),
    );
    while let Some(item) = stream.next().await {
        match item.map_err(super::classify)? {
            bollard::container::LogOutput::StdOut { message } => append_tail(&mut stdout, &message),
            bollard::container::LogOutput::StdErr { message } => append_tail(&mut stderr, &message),
            _ => {}
        }
    }
    Ok((
        String::from_utf8_lossy(&stdout).into_owned(),
        String::from_utf8_lossy(&stderr).into_owned(),
    ))
}

pub(crate) async fn log_tail(
    config: &RuntimeConfig,
    id: ApplicationId,
) -> Result<ApplicationLogTail, RuntimeError> {
    let journal = ApplicationJournal::acquire(config, &id.operation).await?;
    let intent = journal.read()?.ok_or(ErrorKind::UnknownOutcome)?;
    let owner_value = owner(config)?;
    if !owns(&intent, config, &owner_value) || identity(&intent)? != id {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    if let Some(result) = intent.result {
        return Ok(ApplicationLogTail {
            stdout: result.stdout,
            stderr: result.stderr,
        });
    }
    drop(journal);
    let docker = open(config).await?;
    let (stdout, stderr) = logs(&docker, &id).await?;
    Ok(ApplicationLogTail { stdout, stderr })
}
fn append_tail(dst: &mut Vec<u8>, bytes: &[u8]) {
    if bytes.len() >= MAX_LOG_BYTES {
        dst.clear();
        dst.extend_from_slice(&bytes[bytes.len() - MAX_LOG_BYTES..]);
        return;
    }
    let excess = dst
        .len()
        .saturating_add(bytes.len())
        .saturating_sub(MAX_LOG_BYTES);
    if excess > 0 {
        dst.drain(..excess);
    }
    dst.extend_from_slice(bytes);
}

/// A result is written only after the journal has accepted the exact serialized
/// record.  Request data is durable first; trim the oldest final-log bytes if
/// escaping would otherwise exceed the journal limit.
fn persist_result(
    journal: &ApplicationJournal,
    intent: &mut ApplicationIntent,
    mut result: ApplicationResult,
) -> Result<(), RuntimeError> {
    loop {
        intent.result = Some(result.clone());
        let serialized = serde_json::to_vec(intent).map_err(|_| ErrorKind::Protocol)?;
        if serialized.len() <= MAX_JOURNAL_BYTES {
            return journal.write(intent);
        }
        let overage = serialized.len() - MAX_JOURNAL_BYTES;
        // A JSON escaped UTF-8 byte consumes at most six serialized bytes.
        // Trim a chunk at a character boundary, then remeasure. This bounds
        // the synchronous work even for control-byte-heavy application logs.
        let remove = overage.div_ceil(6);
        if !result.stdout.is_empty() {
            trim_oldest(&mut result.stdout, remove);
        } else if !result.stderr.is_empty() {
            trim_oldest(&mut result.stderr, remove);
        } else {
            intent.result = None;
            return Err(ErrorKind::Protocol.into());
        }
    }
}

fn trim_oldest(value: &mut String, requested: usize) {
    if requested >= value.len() {
        value.clear();
        return;
    }
    let mut boundary = requested;
    while boundary < value.len() && !value.is_char_boundary(boundary) {
        boundary += 1;
    }
    value.drain(..boundary);
}
pub(crate) async fn observe(
    config: &RuntimeConfig,
    id: ApplicationId,
) -> Result<ApplicationResult, RuntimeError> {
    let journal = ApplicationJournal::acquire(config, &id.operation).await?;
    let intent = journal.read()?.ok_or(ErrorKind::UnknownOutcome)?;
    let owner = owner(config)?;
    if !owns(&intent, config, &owner) || identity(&intent)? != id {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    if let Some(result) = intent.result.clone() {
        return Ok(result);
    }
    drop(journal);
    let docker = open(config).await?;
    let until = tokio::time::Instant::now() + config.deadline;
    let (exit, oom) = loop {
        let state = inspect_owned(&docker, &intent).await?.0;
        if let ApplicationState::Exited {
            exit_code,
            oom_killed,
        } = state
        {
            break (exit_code, oom_killed);
        }
        if tokio::time::Instant::now() >= until {
            return Err(ErrorKind::Timeout.into());
        }
        // A full owned-requirements inspection is deliberately expensive; do
        // not turn long sessions into a 40Hz daemon load source.
        tokio::time::sleep(std::time::Duration::from_millis(250)).await;
    };
    let (stdout, stderr) = logs(&docker, &id).await?;
    let result = ApplicationResult {
        exit_code: exit,
        oom_killed: oom,
        stdout,
        stderr,
    };
    let journal = ApplicationJournal::acquire(config, &id.operation).await?;
    // The final log drain ran without the lease. Reload so an explicit stop
    // or cleanup cannot be regressed back to Running by a stale observer.
    let mut latest = journal.read()?.ok_or(ErrorKind::UnknownOutcome)?;
    if !owns(&latest, config, &owner) || identity(&latest)? != id {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    if let Some(recorded) = latest.result.clone() {
        return Ok(recorded);
    }
    if matches!(
        latest.phase,
        ApplicationPhase::CleanupPending | ApplicationPhase::Completed
    ) {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    latest.phase = ApplicationPhase::Stopped;
    persist_result(&journal, &mut latest, result.clone())?;
    Ok(result)
}
pub(crate) async fn stop(
    config: &RuntimeConfig,
    id: ApplicationId,
    timeout: i32,
) -> Result<(), RuntimeError> {
    let journal = ApplicationJournal::acquire(config, &id.operation).await?;
    let mut intent = journal.read()?.ok_or(ErrorKind::UnknownOutcome)?;
    let owner = owner(config)?;
    if !owns(&intent, config, &owner) || identity(&intent)? != id {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    if intent.phase == ApplicationPhase::Completed {
        return Ok(());
    }
    if intent.phase == ApplicationPhase::CleanupPending {
        drop(journal);
        return cleanup(config, id).await;
    }
    intent.phase = ApplicationPhase::Stopping;
    journal.write(&intent)?;
    let docker = open(config).await?;
    let state = inspect_identity_state(&docker, &intent).await?;
    if state == ApplicationState::Running {
        docker
            .stop_container(
                id.as_str(),
                Some(StopContainerOptions {
                    t: Some(timeout),
                    ..Default::default()
                }),
            )
            .await
            .map_err(|e| match e {
                Error::DockerResponseServerError {
                    status_code: 400..=499,
                    ..
                } => super::classify(e),
                _ => ErrorKind::UnknownOutcome.into(),
            })?;
    }
    let state = inspect_identity_state(&docker, &intent).await?;
    if state == ApplicationState::Created && intent.phase == ApplicationPhase::Stopping {
        // The container never ran.  A durable abandonment request makes this
        // safe to remove, while preserving the absence of terminal evidence.
        intent.phase = ApplicationPhase::Stopped;
        return journal.write(&intent);
    }
    if !matches!(state, ApplicationState::Exited { .. }) {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    intent.phase = ApplicationPhase::Stopped;
    journal.write(&intent)
}
pub(crate) async fn cleanup(config: &RuntimeConfig, id: ApplicationId) -> Result<(), RuntimeError> {
    let journal = ApplicationJournal::acquire(config, &id.operation).await?;
    let mut intent = journal.read()?.ok_or(ErrorKind::UnknownOutcome)?;
    let owner = owner(config)?;
    if !owns(&intent, config, &owner) || identity(&intent)? != id {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    let docker = open(config).await?;
    // A prior remove reply can be lost after Docker has accepted it. Terminal
    // evidence was fsynced before CleanupPending, so absence of the exact
    // immutable ID now completes this one operation without a second delete.
    if intent.phase == ApplicationPhase::Completed {
        return Ok(());
    }
    if intent.phase == ApplicationPhase::CleanupPending && intent.result.is_some() {
        match docker.inspect_container(id.as_str(), None).await {
            Err(Error::DockerResponseServerError {
                status_code: 404, ..
            }) => {
                intent.phase = ApplicationPhase::Completed;
                return journal.write(&intent);
            }
            Ok(_) => {}
            Err(error) => return Err(uncertain(error)),
        }
    }
    let state = inspect_identity_state(&docker, &intent).await?;
    if state == ApplicationState::Running {
        return Err(ErrorKind::Busy.into());
    }
    if intent.result.is_none() {
        let (stdout, stderr) = logs(&docker, &id).await?;
        let result = ApplicationResult {
            exit_code: match state {
                ApplicationState::Exited { exit_code, .. } => exit_code,
                _ => None,
            },
            oom_killed: match state {
                ApplicationState::Exited { oom_killed, .. } => oom_killed,
                _ => None,
            },
            stdout,
            stderr,
        };
        intent.phase = ApplicationPhase::CleanupPending;
        persist_result(&journal, &mut intent, result)?;
    }
    intent.phase = ApplicationPhase::CleanupPending;
    journal.write(&intent)?;
    docker
        .remove_container(
            id.as_str(),
            Some(RemoveContainerOptions {
                force: false,
                v: false,
                link: false,
            }),
        )
        .await
        .map_err(|e| match e {
            Error::DockerResponseServerError {
                status_code: 404, ..
            } => RuntimeError::from(ErrorKind::Missing),
            Error::DockerResponseServerError {
                status_code: 400..=499,
                ..
            } => super::classify(e),
            _ => ErrorKind::UnknownOutcome.into(),
        })?;
    match docker.inspect_container(id.as_str(), None).await {
        Err(Error::DockerResponseServerError {
            status_code: 404, ..
        }) => {
            intent.phase = ApplicationPhase::Completed;
            journal.write(&intent)
        }
        Ok(_) => Err(ErrorKind::UnknownOutcome.into()),
        Err(error) => Err(uncertain(error)),
    }
}

/// Record an irreversible caller decision before interacting with Docker.  It
/// also handles a lost create reply: only the exact durable name and labels may
/// supply the missing immutable ID.
pub(crate) async fn abandon(config: &RuntimeConfig, operation: &str) -> Result<(), RuntimeError> {
    let journal = ApplicationJournal::acquire(config, operation).await?;
    // No durable intent means this operation never reached the submission boundary.
    // In particular, image preflight/open failure happens before `start` writes its
    // intent. Treat only this successfully-read empty journal as a proven no-op; a
    // read/access/corruption error remains uncertain and must retain the caller gate.
    let Some(mut intent) = journal.read()? else {
        return Ok(());
    };
    // Terminal evidence outranks the endpoint. Completed is durable proof that no
    // container of this operation remains: normally the exact immutable ID was
    // inspected absent after removal, otherwise the durable unique name was absent
    // before an ID was ever learned. Either way there is nothing left to mutate
    // and nothing an ownership check could protect. Checking `owns` first
    // made a moved DOCKER_HOST (a proxy socket in front of the same daemon)
    // refuse every boot with UnknownOutcome, forever. Every NON-terminal phase
    // below still has to prove it owns the endpoint it would mutate.
    if intent.phase == ApplicationPhase::Completed {
        return Ok(());
    }
    let owner_value = owner(config)?;
    if !owns(&intent, config, &owner_value) || intent.request.operation != operation {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    if intent.phase == ApplicationPhase::CleanupPending {
        if let Some(id) = intent.id.as_ref().filter(|value| valid_id(value)) {
            drop(journal);
            return cleanup(
                config,
                ApplicationId {
                    id: id.clone(),
                    operation: operation.into(),
                },
            )
            .await;
        }
    }
    intent.phase = ApplicationPhase::Stopping;
    journal.write(&intent)?;
    let docker = open(config).await?;
    if intent.id.is_none() {
        match docker.inspect_container(&intent.request.name, None).await {
            Ok(info) => {
                if info
                    .config
                    .as_ref()
                    .and_then(|c| c.labels.as_ref())
                    .and_then(|labels| labels.get(container_ownership::LABEL))
                    != Some(&intent.owner)
                    || info
                        .config
                        .as_ref()
                        .and_then(|c| c.labels.as_ref())
                        .and_then(|labels| labels.get(OPERATION_LABEL))
                        != Some(&intent.request.operation)
                {
                    return Err(ErrorKind::UnknownOutcome.into());
                }
                intent.id = info.id.filter(|value| valid_id(value));
                if intent.id.is_none() {
                    return Err(ErrorKind::UnknownOutcome.into());
                }
                // A name/label match alone is insufficient authority for an
                // irreversible stop. Reinspect the immutable ID against the
                // complete pinned request before mutating it.
                let _ = inspect_owned(&docker, &intent).await?;
                journal.write(&intent)?;
            }
            Err(Error::DockerResponseServerError {
                status_code: 404, ..
            }) => {
                intent.phase = ApplicationPhase::Completed;
                return journal.write(&intent);
            }
            Err(error) => return Err(uncertain(error)),
        }
    }
    let id = identity(&intent)?;
    drop(journal);
    stop(config, id.clone(), 5).await?;
    cleanup(config, id).await
}

fn scanned_operations(
    config: &RuntimeConfig,
) -> Result<(Vec<String>, Option<ErrorKind>), RuntimeError> {
    use std::{io::Read, os::unix::fs::OpenOptionsExt};
    let root = config
        .image_state_path
        .as_ref()
        .ok_or(ErrorKind::InvalidConfiguration)?
        .join("applications");
    let entries = match std::fs::read_dir(root) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok((Vec::new(), None))
        }
        Err(_) => return Err(ErrorKind::Unavailable.into()),
    };
    let mut operations = Vec::new();
    let mut failure = None;
    for entry in entries {
        let entry = match entry {
            Ok(value) => value,
            Err(_) => {
                failure = Some(ErrorKind::Unavailable);
                continue;
            }
        };
        let path = entry.path();
        if path.extension().is_some() {
            continue;
        }
        let file_type = match entry.file_type() {
            Ok(value) => value,
            Err(_) => {
                failure = Some(ErrorKind::Unavailable);
                continue;
            }
        };
        if !file_type.is_file() {
            failure = Some(ErrorKind::Protocol);
            continue;
        }
        let mut bytes = Vec::new();
        let read = std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW)
            .open(&path)
            .and_then(|file| file.take((64 * 1024 + 1) as u64).read_to_end(&mut bytes));
        if read.is_err() {
            failure = Some(ErrorKind::Unavailable);
            continue;
        }
        if bytes.len() > 64 * 1024 {
            failure = Some(ErrorKind::Protocol);
            continue;
        }
        match serde_json::from_slice::<ApplicationIntent>(&bytes) {
            Ok(intent) => operations.push(intent.request.operation),
            Err(_) => failure = Some(ErrorKind::Protocol),
        }
    }
    Ok((operations, failure))
}

fn recovery_record_budget(config: &RuntimeConfig) -> std::time::Duration {
    std::cmp::min(config.deadline, std::time::Duration::from_secs(5))
}

/// Retry only recorded terminal cleanup. This is deliberately not session
/// adoption: a running application is left alone for the control-plane/session
/// owner, while stopped work retains its final evidence until removal succeeds.
pub(crate) async fn recover_cleanup(config: &RuntimeConfig) -> Result<(), RuntimeError> {
    let (operations, mut failure) = scanned_operations(config)?;
    // Routine recovery only retries an already-authorized teardown. Active
    // records remain running until their owning session explicitly stops them.
    for operation in operations {
        let budget = recovery_record_budget(config);
        let journal =
            match tokio::time::timeout(budget, ApplicationJournal::acquire(config, &operation))
                .await
            {
                Ok(Ok(value)) => value,
                Ok(Err(error)) => {
                    failure = Some(error.kind);
                    continue;
                }
                Err(_) => {
                    failure = Some(ErrorKind::Timeout);
                    continue;
                }
            };
        let intent = match journal.read() {
            Ok(Some(value)) => value,
            Ok(None) => continue,
            Err(error) => {
                failure = Some(error.kind);
                continue;
            }
        };
        let id = intent.id.clone().filter(|value| valid_id(value));
        let phase = intent.phase;
        drop(journal);
        let result = match (phase, id) {
            (ApplicationPhase::Stopping, Some(id)) => {
                tokio::time::timeout(budget, stop(config, ApplicationId { id, operation }, 5))
                    .await
                    .unwrap_or_else(|_| Err(ErrorKind::Timeout.into()))
            }
            (ApplicationPhase::Stopped | ApplicationPhase::CleanupPending, Some(id)) => {
                tokio::time::timeout(budget, cleanup(config, ApplicationId { id, operation }))
                    .await
                    .unwrap_or_else(|_| Err(ErrorKind::Timeout.into()))
            }
            (ApplicationPhase::Stopping, None) => {
                tokio::time::timeout(budget, abandon(config, &operation))
                    .await
                    .unwrap_or_else(|_| Err(ErrorKind::Timeout.into()))
            }
            (ApplicationPhase::Stopped | ApplicationPhase::CleanupPending, None) => {
                Err(ErrorKind::UnknownOutcome.into())
            }
            _ => Ok(()),
        };
        if let Err(error) = result {
            failure = Some(error.kind);
        }
    }
    failure.map_or(Ok(()), |kind| Err(kind.into()))
}

/// Startup is intentionally stronger than periodic recovery: old application
/// records are explicitly retired before a new agent starts sibling services.
pub(crate) async fn retire(config: &RuntimeConfig) -> Result<(), RuntimeError> {
    let (operations, mut failure) = scanned_operations(config)?;
    // Records this pass proved terminal under their lease: they need no
    // abandonment, and asking for one would re-read them through checks that
    // only a still-mutable record needs.
    let mut terminal = std::collections::HashSet::new();
    // First record every retirement request. A stuck first Docker call cannot
    // prevent later prior-agent workloads from becoming durable obligations.
    for operation in &operations {
        let budget = recovery_record_budget(config);
        let journal = match tokio::time::timeout(
            budget,
            ApplicationJournal::acquire(config, operation),
        )
        .await
        {
            Ok(Ok(value)) => value,
            Ok(Err(error)) => {
                failure = Some(error.kind);
                continue;
            }
            Err(_) => {
                failure = Some(ErrorKind::Timeout);
                continue;
            }
        };
        match journal.read() {
            Ok(Some(intent)) if intent.phase == ApplicationPhase::Completed => {
                terminal.insert(operation.clone());
            }
            Ok(Some(mut intent)) => {
                let owner_value = owner(config)?;
                if !owns(&intent, config, &owner_value) {
                    failure = Some(ErrorKind::UnknownOutcome);
                } else if intent.phase != ApplicationPhase::CleanupPending {
                    intent.phase = ApplicationPhase::Stopping;
                    if let Err(error) = journal.write(&intent) {
                        failure = Some(error.kind);
                    }
                }
            }
            Ok(_) => {}
            Err(error) => failure = Some(error.kind),
        }
    }
    for operation in operations {
        if terminal.contains(&operation) {
            continue;
        }
        let result =
            tokio::time::timeout(recovery_record_budget(config), abandon(config, &operation))
                .await
                .unwrap_or_else(|_| Err(ErrorKind::Timeout.into()));
        if let Err(error) = result {
            failure = Some(error.kind);
        }
    }
    failure.map_or(Ok(()), |kind| Err(kind.into()))
}
