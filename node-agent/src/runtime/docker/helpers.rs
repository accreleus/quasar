//! Docker SDK details for the deliberately narrow owned helper lifecycle.
use crate::runtime::helpers::{HelperPhase, MAX_JOURNAL_BYTES, MAX_LOG_BYTES};
use crate::{
    container_ownership,
    runtime::{
        DiagnosticHelper, DiagnosticRequirements, DiagnosticRun, ErrorKind, HelperIntent,
        HelperJournal, HelperResult, OwnedHelperId, RuntimeConfig, RuntimeError,
    },
};
use bollard::{
    container::LogOutput,
    errors::Error,
    models::{
        ContainerCreateBody, ContainerStateStatusEnum, HostConfig, Mount, MountBindOptions,
        MountType,
    },
    query_parameters::{
        CreateContainerOptions, LogsOptions, RemoveContainerOptions, StartContainerOptions,
        StopContainerOptions,
    },
};
use futures_util::StreamExt;
use sha2::{Digest, Sha256};
use std::{collections::HashMap, time::Duration};

const OPERATION_LABEL: &str = "io.quasar.runtime-operation";

fn owner(config: &RuntimeConfig) -> Result<String, RuntimeError> {
    #[cfg(test)]
    {
        if let Some(owner) = &config.diagnostic_owner {
            return Ok(owner.clone());
        }
    }
    #[cfg(not(test))]
    let _ = config;
    container_ownership::token().map_err(|_| ErrorKind::Unavailable.into())
}
fn valid_id(id: &str) -> bool {
    id.len() == 64 && id.bytes().all(|b| b.is_ascii_hexdigit())
}
fn valid_helper(h: &DiagnosticHelper) -> bool {
    !h.operation.is_empty()
        && h.operation.len() <= 96
        && h.operation
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
        && !h.name.is_empty()
        && h.name.len() <= 128
        && !h.name.contains(['/', '\0'])
        && !h.image.trim().is_empty()
}
fn valid_run(run: &DiagnosticRun) -> bool {
    !run.entrypoint.is_empty() && run.entrypoint.iter().all(|v| !v.is_empty() && !v.contains('\0'))
        && run.command.iter().all(|v| !v.contains('\0')) && run.bind.source.is_absolute()
        // The daemon's host namespace owns this path. Only Docker may decide
        // whether it exists; checking agent-local paths is both wrong and racy.
        && run.bind.target.starts_with('/')
        && !run.bind.target.contains(['\0', ':'])
        && run.entrypoint.iter().chain(run.command.iter()).map(String::len).sum::<usize>() <= 8 * 1024
        && run.bind.source.as_os_str().len() + run.bind.target.len() <= 4 * 1024
}
fn fingerprint(
    helper: &DiagnosticHelper,
    run: Option<&DiagnosticRun>,
) -> Result<String, RuntimeError> {
    serde_json::to_vec(&(helper, run))
        .map(|v| {
            Sha256::digest(v)
                .iter()
                .map(|b| format!("{b:02x}"))
                .collect()
        })
        .map_err(|_| ErrorKind::Protocol.into())
}
/// A request is accepted only if its durable record can still hold two bounded
/// streams after JSON escaping. This is checked before any daemon mutation.
fn reserves_final_evidence(intent: &HelperIntent) -> Result<bool, RuntimeError> {
    let mut worst = intent.clone();
    worst.id = Some("f".repeat(64));
    worst.phase = HelperPhase::CleanupPending;
    let escaped = String::from_utf8(vec![0; MAX_LOG_BYTES]).expect("NUL is valid UTF-8");
    worst.result = Some(HelperResult {
        exit_code: Some(i64::MIN),
        stdout: escaped.clone(),
        stderr: escaped,
    });
    Ok(serde_json::to_vec(&worst)
        .map_err(|_| ErrorKind::Protocol)?
        .len()
        <= MAX_JOURNAL_BYTES)
}
fn matches(
    intent: &HelperIntent,
    helper: &DiagnosticHelper,
    run: Option<&DiagnosticRun>,
    owner: &str,
    config: &RuntimeConfig,
) -> Result<bool, RuntimeError> {
    Ok(intent.operation == helper.operation
        && intent.name == helper.name
        && intent.image == helper.image
        && intent.owner == owner
        && intent.socket == config.socket
        && intent.request_fingerprint == fingerprint(helper, run)?)
}
fn helper(intent: &HelperIntent) -> DiagnosticHelper {
    DiagnosticHelper {
        operation: intent.operation.clone(),
        name: intent.name.clone(),
        image: intent.image.clone(),
    }
}
fn owned(intent: &HelperIntent) -> Result<OwnedHelperId, RuntimeError> {
    Ok(OwnedHelperId {
        id: intent
            .id
            .clone()
            .filter(|v| valid_id(v))
            .ok_or(ErrorKind::UnknownOutcome)?,
        operation: intent.operation.clone(),
    })
}

fn inspect_owned(
    info: bollard::models::ContainerInspectResponse,
    intent: &HelperIntent,
) -> Result<(OwnedHelperId, bool, Option<i64>), RuntimeError> {
    let h = helper(intent);
    let id = info.id.filter(|v| valid_id(v)).ok_or(ErrorKind::Protocol)?;
    if Some(&id) != intent.id.as_ref() || info.name.as_deref() != Some(&format!("/{}", h.name)) {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    let c = info.config.ok_or(ErrorKind::Protocol)?;
    let labels = c.labels.unwrap_or_default();
    if labels.get(container_ownership::LABEL).map(String::as_str) != Some(intent.owner.as_str())
        || labels.get(OPERATION_LABEL).map(String::as_str) != Some(h.operation.as_str())
        || c.image.as_deref() != Some(&h.image)
        || c.user.as_deref() != Some("0:0")
    {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    let host = info.host_config.ok_or(ErrorKind::Protocol)?;
    if host.network_mode.as_deref() != Some("none")
        || host.readonly_rootfs != Some(true)
        || host.privileged != Some(false)
        || host.auto_remove != Some(false)
        || host.cap_add.as_ref().is_some_and(|v| !v.is_empty())
        || host.devices.as_ref().is_some_and(|v| !v.is_empty())
        || host.device_requests.as_ref().is_some_and(|v| !v.is_empty())
        || host.volumes_from.as_ref().is_some_and(|v| !v.is_empty())
        || host.binds.as_ref().is_some_and(|v| !v.is_empty())
        || host.pid_mode.as_deref().is_some_and(|v| !v.is_empty())
        || host
            .ipc_mode
            .as_deref()
            .is_some_and(|v| !v.is_empty() && v != "private")
        || host.uts_mode.as_deref().is_some_and(|v| !v.is_empty())
        || host
            .cgroupns_mode
            .as_ref()
            .is_some_and(|v| format!("{v:?}").eq_ignore_ascii_case("host"))
        || host.cap_drop.as_deref() != Some(&["ALL".to_owned()])
        || host.security_opt.as_deref() != Some(&["no-new-privileges".to_owned()])
    {
        return Err(ErrorKind::Protocol.into());
    }
    if let Some(run) = &intent.run {
        if c.entrypoint.as_ref() != Some(&run.entrypoint) || c.cmd.as_ref() != Some(&run.command) {
            return Err(ErrorKind::Protocol.into());
        }
        let mounts = info.mounts.unwrap_or_default();
        if mounts.len() != 1
            || mounts[0].typ.as_deref() != Some("bind")
            || mounts[0].source.as_deref() != run.bind.source.to_str()
            || mounts[0].destination.as_deref() != Some(&run.bind.target)
            || mounts[0].rw != Some(false)
        {
            return Err(ErrorKind::Protocol.into());
        }
        // `Mounts` in HostConfig is the requested realization. Inspecting only
        // the resulting mountpoint would miss an option such as host-path
        // creation that weakens this fixed profile.
        let requested = host.mounts.as_deref().unwrap_or(&[]);
        if requested.len() != 1
            || requested[0].typ != Some(MountType::BIND)
            || requested[0].source.as_deref() != run.bind.source.to_str()
            || requested[0].target.as_deref() != Some(&run.bind.target)
            || requested[0].read_only != Some(true)
        {
            return Err(ErrorKind::Protocol.into());
        }
        if let Some(options) = &requested[0].bind_options {
            let safe_propagation = options.propagation.is_none()
                || matches!(
                    options.propagation,
                    Some(
                        bollard::models::MountBindOptionsPropagationEnum::PRIVATE
                            | bollard::models::MountBindOptionsPropagationEnum::RPRIVATE
                    )
                );
            if options.create_mountpoint == Some(true)
                || !safe_propagation
                || options.non_recursive == Some(true)
                || options.read_only_non_recursive == Some(true)
                || options.read_only_force_recursive == Some(true)
            {
                return Err(ErrorKind::Protocol.into());
            }
        }
    } else if info.mounts.as_ref().is_some_and(|m| !m.is_empty()) {
        return Err(ErrorKind::Protocol.into());
    }
    let state = info.state.ok_or(ErrorKind::Protocol)?;
    let running = state.running.ok_or(ErrorKind::Protocol)?;
    let exit = if !running && state.status == Some(ContainerStateStatusEnum::EXITED) {
        state.exit_code
    } else {
        None
    };
    Ok((
        OwnedHelperId {
            id,
            operation: h.operation,
        },
        running,
        exit,
    ))
}

async fn open(config: &RuntimeConfig) -> Result<bollard::Docker, RuntimeError> {
    Ok(super::discover(config).await?.0)
}
fn uncertain_inspection(error: Error) -> RuntimeError {
    let reconciliation = match error {
        Error::DockerResponseServerError {
            status_code: 404, ..
        } => ErrorKind::Missing,
        error => super::classify(error).kind,
    };
    RuntimeError {
        kind: ErrorKind::UnknownOutcome,
        reconciliation: Some(reconciliation),
    }
}
async fn inspect(
    docker: &bollard::Docker,
    intent: &HelperIntent,
) -> Result<(OwnedHelperId, bool, Option<i64>), RuntimeError> {
    inspect_owned(
        docker
            .inspect_container(owned(intent)?.as_str(), None)
            .await
            .map_err(uncertain_inspection)?,
        intent,
    )
}
fn current_intent(config: &RuntimeConfig, intent: &HelperIntent) -> Result<(), RuntimeError> {
    if intent.socket != config.socket || intent.owner != owner(config)? {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    Ok(())
}

async fn create_or_adopt_inner(
    config: &RuntimeConfig,
    helper: DiagnosticHelper,
    run: Option<DiagnosticRun>,
) -> Result<(bollard::Docker, HelperJournal, HelperIntent), RuntimeError> {
    if !valid_helper(&helper) || run.as_ref().is_some_and(|r| !valid_run(r)) {
        return Err(ErrorKind::InvalidConfiguration.into());
    }
    let owner = owner(config)?;
    let docker = open(config).await?;
    let journal = HelperJournal::acquire(config, &helper.operation).await?;
    if let Some(intent) = journal.read()? {
        if !matches(&intent, &helper, run.as_ref(), &owner, config)? {
            return Err(ErrorKind::UnknownOutcome.into());
        }
        // A completed tombstone is the durable result of this exact operation.
        // Its handle is replayable, but it can never authorize a new create.
        if intent.phase == HelperPhase::Completed {
            return Ok((docker, journal, intent));
        }
        if intent.id.is_none() {
            // A response loss is reconciled by read-only name inspection. 404
            // is still uncertain and never authorizes a second create.
            let info = docker
                .inspect_container(&helper.name, None)
                .await
                .map_err(uncertain_inspection)?;
            let id = info
                .id
                .clone()
                .filter(|v| valid_id(v))
                .ok_or(ErrorKind::UnknownOutcome)?;
            let mut adopted = intent;
            adopted.id = Some(id);
            let _ = inspect_owned(info, &adopted)?;
            journal.write(&adopted)?;
            return Ok((docker, journal, adopted));
        }
        let _ = inspect(&docker, &intent).await?;
        return Ok((docker, journal, intent));
    }
    // A foreign (or unjournalled old) name is only read, never changed by name.
    match docker.inspect_container(&helper.name, None).await {
        Ok(_) => return Err(ErrorKind::UnknownOutcome.into()),
        Err(Error::DockerResponseServerError {
            status_code: 404, ..
        }) => {}
        Err(e) => return Err(super::classify(e)),
    }
    let mut intent = HelperIntent {
        operation: helper.operation.clone(),
        name: helper.name.clone(),
        image: helper.image.clone(),
        owner,
        socket: config.socket.clone(),
        id: None,
        request_fingerprint: fingerprint(&helper, run.as_ref())?,
        run,
        phase: HelperPhase::Creating,
        result: None,
    };
    if !reserves_final_evidence(&intent)? {
        return Err(ErrorKind::InvalidConfiguration.into());
    }
    journal.write(&intent)?;
    let mut labels = HashMap::new();
    labels.insert(container_ownership::LABEL.to_owned(), intent.owner.clone());
    labels.insert(OPERATION_LABEL.to_owned(), helper.operation.clone());
    let mounts = intent.run.as_ref().map(|r| {
        vec![Mount {
            typ: Some(MountType::BIND),
            source: r.bind.source.to_str().map(str::to_owned),
            target: Some(r.bind.target.clone()),
            read_only: Some(true),
            bind_options: Some(MountBindOptions {
                create_mountpoint: Some(false),
                ..Default::default()
            }),
            ..Default::default()
        }]
    });
    let requirements = DiagnosticRequirements::FIXED;
    let devices = match requirements.devices {
        crate::runtime::DiagnosticDevices::None => Vec::new(),
    };
    let (user, readonly_rootfs, cap_drop, security_opt) = match requirements.security {
        crate::runtime::DiagnosticSecurity::LockedDownRoot => (
            "0:0",
            true,
            vec!["ALL".into()],
            vec!["no-new-privileges".into()],
        ),
    };
    let network = match requirements.network {
        crate::runtime::DiagnosticNetwork::None => "none",
    };
    let body = ContainerCreateBody {
        image: Some(helper.image),
        labels: Some(labels),
        user: Some(user.into()),
        entrypoint: intent.run.as_ref().map(|r| r.entrypoint.clone()),
        cmd: intent.run.as_ref().map(|r| r.command.clone()),
        host_config: Some(HostConfig {
            network_mode: Some(network.into()),
            readonly_rootfs: Some(readonly_rootfs),
            privileged: Some(false),
            auto_remove: Some(false),
            cap_drop: Some(cap_drop),
            security_opt: Some(security_opt),
            devices: Some(devices),
            mounts,
            ..Default::default()
        }),
        ..Default::default()
    };
    let created = match docker
        .create_container(
            Some(CreateContainerOptions {
                name: Some(helper.name),
                ..Default::default()
            }),
            body,
        )
        .await
    {
        Ok(created) => created,
        Err(
            error @ Error::DockerResponseServerError {
                status_code: 400..=499,
                ..
            },
        ) => {
            // This response proves Docker did not create this request, so a
            // caller may correct the request and retry the same operation.
            journal.discard_definitive()?;
            return Err(super::classify(error));
        }
        Err(_) => return Err(ErrorKind::UnknownOutcome.into()),
    };
    if !valid_id(&created.id) {
        return Err(ErrorKind::Protocol.into());
    }
    intent.id = Some(created.id);
    intent.phase = HelperPhase::Created;
    journal.write(&intent)?;
    let _ = inspect(&docker, &intent).await?;
    Ok((docker, journal, intent))
}

pub(crate) async fn run(
    config: &RuntimeConfig,
    helper: DiagnosticHelper,
    run: DiagnosticRun,
) -> Result<OwnedHelperId, RuntimeError> {
    let (docker, journal, mut intent) = create_or_adopt_inner(config, helper, Some(run)).await?;
    if intent.phase == HelperPhase::Completed {
        return owned(&intent);
    }
    if intent.phase == HelperPhase::Creating {
        // A lost create response can be retried only after the exact recorded
        // container proves it was created and has never started.
        let info = docker
            .inspect_container(owned(&intent)?.as_str(), None)
            .await
            .map_err(uncertain_inspection)?;
        let is_created = info.state.as_ref().and_then(|state| state.status)
            == Some(ContainerStateStatusEnum::CREATED)
            && info.state.as_ref().and_then(|state| state.running) == Some(false);
        let _ = inspect_owned(info, &intent)?;
        if !is_created {
            return Err(ErrorKind::UnknownOutcome.into());
        }
        intent.phase = HelperPhase::Created;
        journal.write(&intent)?;
    }
    let (id, running, exit) = inspect(&docker, &intent).await?;
    if !running && exit.is_some() {
        // A detached observer may leave a completed container before the next
        // caller arrives. Preserve and replay that operation, never restart it.
        intent.phase = HelperPhase::Stopped;
        journal.write(&intent)?;
        return Ok(id);
    }
    let starting_now = !running && intent.phase == HelperPhase::Created;
    if starting_now {
        // Immutable id was verified immediately before this mutation.
        intent.phase = HelperPhase::Starting;
        journal.write(&intent)?;
        docker
            .start_container(id.as_str(), None::<StartContainerOptions>)
            .await
            .map_err(|e| match e {
                Error::DockerResponseServerError {
                    status_code: 304, ..
                } => ErrorKind::UnknownOutcome.into(),
                Error::DockerResponseServerError {
                    status_code: 400..=499,
                    ..
                } => super::classify(e),
                _ => ErrorKind::UnknownOutcome.into(),
            })?;
    }
    if !starting_now && !running && intent.phase == HelperPhase::Starting {
        // The prior start may have reached Docker. Reconcile its immutable ID;
        // an exited instance is a completed run, never permission to restart.
        if exit.is_some() {
            intent.phase = HelperPhase::Stopped;
            journal.write(&intent)?;
            return Ok(id);
        }
        return Err(ErrorKind::UnknownOutcome.into());
    }
    if !starting_now && !running && intent.phase != HelperPhase::Created {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    // Do not infer the post-start phase from the stale pre-start inspection.
    // A fast helper may already have exited by the time Docker answers start.
    let (verified, now_running, now_exit) = inspect(&docker, &intent).await?;
    intent.phase = if now_running {
        HelperPhase::Running
    } else if now_exit.is_some() {
        HelperPhase::Stopped
    } else {
        return Err(ErrorKind::UnknownOutcome.into());
    };
    journal.write(&intent)?;
    Ok(verified)
}

fn append_raw_bounded(dst: &mut Vec<u8>, bytes: &[u8]) {
    let room = MAX_LOG_BYTES.saturating_sub(dst.len());
    dst.extend_from_slice(&bytes[..bytes.len().min(room)]);
}
async fn final_logs(
    docker: &bollard::Docker,
    id: &OwnedHelperId,
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
            tail: "all".into(),
            ..Default::default()
        }),
    );
    while let Some(item) = stream.next().await {
        match item.map_err(super::classify)? {
            LogOutput::StdOut { message } => append_raw_bounded(&mut stdout, &message),
            LogOutput::StdErr { message } => append_raw_bounded(&mut stderr, &message),
            LogOutput::Console { message } => append_raw_bounded(&mut stdout, &message),
            LogOutput::StdIn { .. } => {}
        }
    }
    Ok((
        String::from_utf8_lossy(&stdout).into_owned(),
        String::from_utf8_lossy(&stderr).into_owned(),
    ))
}

pub(crate) async fn observe(
    config: &RuntimeConfig,
    id: OwnedHelperId,
) -> Result<HelperResult, RuntimeError> {
    // Do not retain the operation lock while waiting. An explicit stop must be
    // able to acquire it and terminate a running diagnostic.
    let mut intent = {
        let journal = HelperJournal::acquire(config, &id.operation).await?;
        journal.read()?.ok_or(ErrorKind::UnknownOutcome)?
    };
    current_intent(config, &intent)?;
    if owned(&intent)? != id {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    if let Some(result) = &intent.result {
        return Ok(result.clone());
    }
    let docker = open(config).await?;
    let until = tokio::time::Instant::now() + config.deadline;
    let exit = loop {
        let (_, running, exit) = inspect(&docker, &intent).await?;
        if !running {
            break exit;
        }
        if tokio::time::Instant::now() >= until {
            return Err(ErrorKind::Timeout.into());
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    };
    let (stdout, stderr) = final_logs(&docker, &id).await?;
    if exit.is_none() {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    let result = HelperResult {
        exit_code: exit,
        stdout,
        stderr,
    };
    intent.phase = HelperPhase::Stopped;
    intent.result = Some(result.clone());
    let journal = HelperJournal::acquire(config, &id.operation).await?;
    // The log drain ran without the lease so an explicit cleanup could proceed.
    // Never overwrite that newer cleanup decision with this stale observation.
    let latest = journal.read()?.ok_or(ErrorKind::UnknownOutcome)?;
    current_intent(config, &latest)?;
    if owned(&latest)? != id
        || latest.request_fingerprint != intent.request_fingerprint
        || latest.name != intent.name
        || latest.image != intent.image
        || latest.run != intent.run
    {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    if let Some(recorded) = latest.result {
        return Ok(recorded);
    }
    if matches!(
        latest.phase,
        HelperPhase::CleanupPending | HelperPhase::Completed
    ) {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    journal.write(&intent)?;
    Ok(result)
}

pub(crate) async fn stop(config: &RuntimeConfig, id: OwnedHelperId) -> Result<(), RuntimeError> {
    let journal = HelperJournal::acquire(config, &id.operation).await?;
    let mut intent = journal.read()?.ok_or(ErrorKind::UnknownOutcome)?;
    current_intent(config, &intent)?;
    if owned(&intent)? != id {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    let docker = open(config).await?;
    let (_, running, mut exit) = inspect(&docker, &intent).await?;
    if running {
        // Explicit termination is durable intent, separate from cancellation
        // of an observer. Recovery may retry only this recorded operation.
        intent.phase = HelperPhase::Stopping;
        journal.write(&intent)?;
        docker
            .stop_container(
                id.as_str(),
                Some(StopContainerOptions {
                    t: Some(5),
                    ..Default::default()
                }),
            )
            .await
            .map_err(|e| match e {
                // A transport failure or server failure can have stopped the
                // helper after Docker accepted the request. Preserve uncertainty.
                Error::DockerResponseServerError {
                    status_code: 400..=499,
                    ..
                } => super::classify(e),
                _ => ErrorKind::UnknownOutcome.into(),
            })?;
        let (_, still_running, observed_exit) = inspect(&docker, &intent).await?;
        if still_running {
            return Err(ErrorKind::UnknownOutcome.into());
        }
        exit = observed_exit;
    }
    if exit.is_none() {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    intent.phase = HelperPhase::Stopped;
    journal.write(&intent)?;
    Ok(())
}

pub(crate) async fn cleanup(config: &RuntimeConfig, id: OwnedHelperId) -> Result<(), RuntimeError> {
    let journal = HelperJournal::acquire(config, &id.operation).await?;
    let mut intent = journal.read()?.ok_or(ErrorKind::UnknownOutcome)?;
    current_intent(config, &intent)?;
    if owned(&intent)? != id {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    if intent.phase == HelperPhase::Completed {
        return Ok(());
    }
    let docker = open(config).await?;
    if intent.phase == HelperPhase::CleanupPending && intent.result.is_some() {
        // A prior delete response was lost. Absence of this exact immutable ID
        // is conclusive only because terminal evidence was synced first.
        match docker.inspect_container(id.as_str(), None).await {
            Err(Error::DockerResponseServerError {
                status_code: 404, ..
            }) => {
                intent.phase = HelperPhase::Completed;
                return journal.write(&intent);
            }
            Ok(info) => {
                let _ = inspect_owned(info, &intent)?;
            }
            Err(error) => return Err(uncertain_inspection(error)),
        }
    }
    let (_, running, _) = inspect(&docker, &intent).await?;
    if running {
        return Err(ErrorKind::Busy.into());
    }
    if intent.result.is_none() {
        let (stdout, stderr) = final_logs(&docker, &id).await?;
        let (_, _, exit) = inspect(&docker, &intent).await?;
        intent.result = Some(HelperResult {
            exit_code: exit,
            stdout,
            stderr,
        });
    }
    intent.phase = HelperPhase::CleanupPending;
    journal.write(&intent)?;
    match docker
        .remove_container(
            id.as_str(),
            Some(RemoveContainerOptions {
                force: false,
                v: false,
                link: false,
            }),
        )
        .await
    {
        Ok(()) => {}
        // A 404 is conclusive only after the retained terminal result was
        // persisted and the same immutable id is absent on a retry.
        Err(Error::DockerResponseServerError {
            status_code: 404, ..
        }) if intent.result.is_some() => {}
        // The daemon may have accepted a delete after the transport failed.
        // Keep the CleanupPending evidence for a later immutable-ID probe.
        Err(
            error @ Error::DockerResponseServerError {
                status_code: 400..=499,
                ..
            },
        ) => return Err(super::classify(error)),
        Err(_) => return Err(ErrorKind::UnknownOutcome.into()),
    }
    match docker.inspect_container(id.as_str(), None).await {
        Err(Error::DockerResponseServerError {
            status_code: 404, ..
        }) => {
            intent.phase = HelperPhase::Completed;
            journal.write(&intent)
        }
        Ok(_) => Err(ErrorKind::UnknownOutcome.into()),
        Err(error) => Err(uncertain_inspection(error)),
    }
}

pub(crate) async fn recover(config: &RuntimeConfig) -> Result<(), RuntimeError> {
    let root = config
        .image_state_path
        .as_ref()
        .ok_or(ErrorKind::InvalidConfiguration)?
        .join("helpers");
    let entries = match std::fs::read_dir(&root) {
        Ok(v) => v,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(_) => return Err(ErrorKind::Unavailable.into()),
    };
    for entry in entries {
        let entry = entry.map_err(|_| ErrorKind::Unavailable)?;
        let path = entry.path();
        if path.extension().and_then(|s| s.to_str()) == Some("lock")
            || path.extension().and_then(|s| s.to_str()) == Some("new")
        {
            continue;
        }
        if !entry
            .file_type()
            .map_err(|_| ErrorKind::Unavailable)?
            .is_file()
        {
            return Err(ErrorKind::Protocol.into());
        }
        use std::{io::Read, os::unix::fs::OpenOptionsExt};
        let mut bytes = Vec::new();
        std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW)
            .open(&path)
            .map_err(|_| ErrorKind::Unavailable)?
            .take((crate::runtime::helpers::MAX_JOURNAL_BYTES + 1) as u64)
            .read_to_end(&mut bytes)
            .map_err(|_| ErrorKind::Unavailable)?;
        if bytes.len() > crate::runtime::helpers::MAX_JOURNAL_BYTES {
            return Err(ErrorKind::Protocol.into());
        }
        let scanned: HelperIntent =
            serde_json::from_slice(&bytes).map_err(|_| ErrorKind::Protocol)?;
        current_intent(config, &scanned)?;
        if scanned.phase == HelperPhase::Completed {
            continue;
        }
        // Re-read while holding the per-operation lease: recovery must not
        // race a caller that is reconciling the same lost create.
        let journal = HelperJournal::acquire(config, &scanned.operation).await?;
        let mut intent = journal.read()?.ok_or(ErrorKind::UnknownOutcome)?;
        current_intent(config, &intent)?;
        if intent.phase == HelperPhase::Completed {
            continue;
        }
        if intent.id.is_none() {
            // POST /create may have succeeded although its response was lost.
            // This is read-only name inspection, followed by full immutable
            // configuration/ownership validation before recording the ID.
            let docker = open(config).await?;
            let info = docker
                .inspect_container(&intent.name, None)
                .await
                .map_err(uncertain_inspection)?;
            let immutable_id = info
                .id
                .clone()
                .filter(|v| valid_id(v))
                .ok_or(ErrorKind::UnknownOutcome)?;
            intent.id = Some(immutable_id);
            let _ = inspect_owned(info, &intent)?;
            journal.write(&intent)?;
        }
        let id = owned(&intent)?;
        let requested_stop = intent.phase == HelperPhase::Stopping;
        drop(journal);
        if requested_stop {
            // `stop` takes this same per-operation lease before it verifies
            // and retries the explicitly authorized termination.
            stop(config, id.clone()).await?;
        }
        // A recovery pass never launches/recreates. Its bounded observation either
        // proves a stopped helper can be collected or leaves uncertainty durable.
        let _ = tokio::time::timeout(Duration::from_secs(5), async {
            let _ = observe(config, id.clone()).await;
        })
        .await;
        cleanup(config, id).await?;
    }
    Ok(())
}
