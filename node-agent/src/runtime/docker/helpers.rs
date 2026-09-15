//! Docker SDK details for the deliberately narrow owned helper lifecycle.
use crate::runtime::helpers::{HelperPhase, HelperProfile, MAX_JOURNAL_BYTES, MAX_LOG_BYTES};
use crate::{
    container_ownership,
    runtime::{
        AudioRun, DiagnosticHelper, DiagnosticRequirements, DiagnosticRun, ErrorKind, HelperIntent,
        HelperJournal, HelperResult, NvidiaDriverMount, NvidiaGpuRun, OwnedHelperId, RuntimeConfig,
        RuntimeError,
    },
};
use bollard::{
    container::LogOutput,
    errors::Error,
    models::{
        ContainerCreateBody, ContainerStateStatusEnum, DeviceRequest, HealthConfig, HostConfig,
        Mount, MountBindOptions, MountType,
    },
    query_parameters::{
        CreateContainerOptions, LogsOptions, RemoveContainerOptions, StartContainerOptions,
        StopContainerOptions,
    },
};
use futures_util::StreamExt;
use sha2::{Digest, Sha256};
use std::{collections::HashMap, os::unix::fs::PermissionsExt, time::Duration};

const OPERATION_LABEL: &str = "io.quasar.runtime-operation";
const AUDIO_MARKER: &str = ".quasar-runtime-audio-owner";

#[derive(serde::Serialize, serde::Deserialize)]
struct AudioMarker {
    owner: String,
    operation: String,
}

fn marker_path(run: &AudioRun) -> std::path::PathBuf {
    run.socket_dir.join(AUDIO_MARKER)
}
fn marker_matches(intent: &HelperIntent) -> Result<bool, RuntimeError> {
    let Some(audio) = &intent.audio else {
        return Ok(false);
    };
    use std::{io::Read, os::unix::fs::OpenOptionsExt};
    let mut bytes = Vec::new();
    let file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(marker_path(audio))
        .map_err(|_| ErrorKind::Unavailable)?;
    if !file
        .metadata()
        .map_err(|_| ErrorKind::Unavailable)?
        .is_file()
    {
        return Err(ErrorKind::Protocol.into());
    }
    file.take(4097)
        .read_to_end(&mut bytes)
        .map_err(|_| ErrorKind::Unavailable)?;
    if bytes.len() > 4096 {
        return Err(ErrorKind::Protocol.into());
    }
    let marker: AudioMarker = serde_json::from_slice(&bytes).map_err(|_| ErrorKind::Protocol)?;
    Ok(marker.owner == intent.owner && marker.operation == intent.operation)
}
fn create_audio_dir(intent: &HelperIntent) -> Result<(), RuntimeError> {
    let audio = intent.audio.as_ref().ok_or(ErrorKind::Protocol)?;
    std::fs::create_dir_all(audio.socket_dir.parent().ok_or(ErrorKind::Protocol)?)
        .map_err(|_| ErrorKind::Unavailable)?;
    match std::fs::symlink_metadata(&audio.socket_dir) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Ok(_) => return Err(ErrorKind::UnknownOutcome.into()),
        Err(_) => return Err(ErrorKind::Unavailable.into()),
    }
    std::fs::create_dir(&audio.socket_dir).map_err(|_| ErrorKind::Unavailable)?;
    std::fs::set_permissions(&audio.socket_dir, std::fs::Permissions::from_mode(0o755))
        .map_err(|_| ErrorKind::Unavailable)?;
    let marker = serde_json::to_vec(&AudioMarker {
        owner: intent.owner.clone(),
        operation: intent.operation.clone(),
    })
    .map_err(|_| ErrorKind::Protocol)?;
    use std::os::unix::fs::OpenOptionsExt;
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(marker_path(audio))
        .map_err(|_| ErrorKind::Unavailable)?;
    use std::io::Write;
    file.write_all(&marker)
        .and_then(|_| file.sync_all())
        .map_err(|_| ErrorKind::Unavailable)?;
    std::fs::File::open(&audio.socket_dir)
        .and_then(|f| f.sync_all())
        .map_err(|_| ErrorKind::Unavailable)?;
    fsync_parent(&audio.socket_dir)?;
    Ok(())
}
fn audio_metadata(path: &std::path::Path) -> Result<std::fs::Metadata, RuntimeError> {
    let metadata = std::fs::symlink_metadata(path).map_err(|_| ErrorKind::Unavailable)?;
    if metadata.file_type().is_symlink() || !metadata.is_dir() {
        return Err(ErrorKind::Protocol.into());
    }
    Ok(metadata)
}
fn rename_no_replace(
    source: &std::path::Path,
    target: &std::path::Path,
) -> Result<(), RuntimeError> {
    use std::os::unix::ffi::OsStrExt;
    let source =
        std::ffi::CString::new(source.as_os_str().as_bytes()).map_err(|_| ErrorKind::Protocol)?;
    let target =
        std::ffi::CString::new(target.as_os_str().as_bytes()).map_err(|_| ErrorKind::Protocol)?;
    // SAFETY: NUL-terminated paths remain live for this syscall.
    let status = unsafe {
        libc::renameat2(
            libc::AT_FDCWD,
            source.as_ptr(),
            libc::AT_FDCWD,
            target.as_ptr(),
            libc::RENAME_NOREPLACE,
        )
    };
    if status == 0 {
        Ok(())
    } else if std::io::Error::last_os_error().kind() == std::io::ErrorKind::AlreadyExists {
        Err(ErrorKind::UnknownOutcome.into())
    } else {
        Err(ErrorKind::Unavailable.into())
    }
}
fn same_audio_dir(intent: &HelperIntent, path: &std::path::Path) -> Result<bool, RuntimeError> {
    use std::os::unix::fs::MetadataExt;
    let metadata = audio_metadata(path)?;
    Ok(Some(metadata.dev()) == intent.audio_dir_device
        && Some(metadata.ino()) == intent.audio_dir_inode)
}
fn fsync_parent(path: &std::path::Path) -> Result<(), RuntimeError> {
    std::fs::File::open(path.parent().ok_or(ErrorKind::Protocol)?)
        .and_then(|f| f.sync_all())
        .map_err(|_| ErrorKind::Unavailable.into())
}

/// Persist an observed absence when its parent survived.  A volatile runtime
/// root may itself be gone after reboot; that is also conclusive absence.
/// Permission and every other I/O failure remain uncertain.
fn sync_parent_if_present(path: &std::path::Path) -> Result<(), RuntimeError> {
    match std::fs::File::open(path.parent().ok_or(ErrorKind::Protocol)?) {
        Ok(parent) => parent.sync_all().map_err(|_| ErrorKind::Unavailable.into()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(_) => Err(ErrorKind::Unavailable.into()),
    }
}
fn retired_dir(intent: &HelperIntent) -> Result<&std::path::Path, RuntimeError> {
    intent
        .audio_retired_dir
        .as_deref()
        .ok_or(ErrorKind::UnknownOutcome.into())
}
fn cleanup_audio_dir(intent: &HelperIntent) -> Result<(), RuntimeError> {
    if !intent.audio_dir_created {
        return Ok(());
    }
    let path = retired_dir(intent)?;
    match std::fs::symlink_metadata(path) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            sync_parent_if_present(path)?;
            return Ok(());
        }
        Err(_) => return Err(ErrorKind::Unavailable.into()),
        Ok(_) if !same_audio_dir(intent, path)? => return Err(ErrorKind::UnknownOutcome.into()),
        Ok(_) => {}
    }
    // The marker remains until every payload entry has gone, so an interrupted
    // cleanup retains its ownership proof for retry.  The directory is proven
    // ours before touching it; symlinks are still refused rather than followed.
    for entry in std::fs::read_dir(path).map_err(|_| ErrorKind::Unavailable)? {
        let entry = entry.map_err(|_| ErrorKind::Unavailable)?;
        if entry.file_name() == AUDIO_MARKER {
            continue;
        }
        let ty = entry.file_type().map_err(|_| ErrorKind::Unavailable)?;
        if ty.is_symlink() {
            return Err(ErrorKind::Protocol.into());
        }
        if ty.is_dir() {
            std::fs::remove_dir_all(entry.path()).map_err(|_| ErrorKind::Unavailable)?;
        } else {
            std::fs::remove_file(entry.path()).map_err(|_| ErrorKind::Unavailable)?;
        }
    }
    let marker = path.join(AUDIO_MARKER);
    match std::fs::remove_file(marker) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(_) => return Err(ErrorKind::Unavailable.into()),
    }
    match std::fs::remove_dir(path) {
        Ok(()) => fsync_parent(path),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => sync_parent_if_present(path),
        Err(_) => Err(ErrorKind::Unavailable.into()),
    }
}

fn retire_audio_dir(
    intent: &mut HelperIntent,
    journal: &HelperJournal,
) -> Result<(), RuntimeError> {
    if !intent.audio_dir_created {
        return Ok(());
    }
    let audio = intent.audio.as_ref().ok_or(ErrorKind::Protocol)?;
    if intent.audio_retired_dir.is_none() {
        use std::os::unix::fs::MetadataExt;
        let metadata = match std::fs::symlink_metadata(&audio.socket_dir) {
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                let target = audio
                    .socket_dir
                    .parent()
                    .ok_or(ErrorKind::Protocol)?
                    .join(format!(".quasar-audio-retired-{}", intent.operation));
                intent.audio_retired_dir = Some(target.clone());
                intent.audio_dir_renamed = true;
                sync_parent_if_present(&target)?;
                journal.write(intent)?;
                return Ok(());
            }
            Ok(_) => audio_metadata(&audio.socket_dir)?,
            Err(_) => return Err(ErrorKind::Unavailable.into()),
        };
        if !marker_matches(intent)? {
            return Err(ErrorKind::UnknownOutcome.into());
        }
        let target = audio
            .socket_dir
            .parent()
            .ok_or(ErrorKind::Protocol)?
            .join(format!(".quasar-audio-retired-{}", intent.operation));
        intent.audio_retired_dir = Some(target.clone());
        intent.audio_dir_device = Some(metadata.dev());
        intent.audio_dir_inode = Some(metadata.ino());
        // Durable retirement authority precedes the rename.
        journal.write(intent)?;
        rename_no_replace(&audio.socket_dir, &target)?;
        fsync_parent(&target)?;
        intent.audio_dir_renamed = true;
        journal.write(intent)?;
        return Ok(());
    }
    let target = retired_dir(intent)?.to_path_buf();
    match std::fs::symlink_metadata(&target) {
        Ok(_) => {
            if !same_audio_dir(intent, &target)? {
                return Err(ErrorKind::UnknownOutcome.into());
            }
            if !intent.audio_dir_renamed {
                fsync_parent(&target)?;
                intent.audio_dir_renamed = true;
                journal.write(intent)?;
            }
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound && intent.audio_dir_renamed => {
            sync_parent_if_present(&target)?;
            return Ok(());
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            match std::fs::symlink_metadata(&audio.socket_dir) {
                Ok(_) => {
                    if !same_audio_dir(intent, &audio.socket_dir)? || !marker_matches(intent)? {
                        return Err(ErrorKind::UnknownOutcome.into());
                    }
                    rename_no_replace(&audio.socket_dir, &target)?;
                    fsync_parent(&target)?;
                    intent.audio_dir_renamed = true;
                    journal.write(intent)?;
                }
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                    sync_parent_if_present(&target)?;
                }
                Err(_) => return Err(ErrorKind::Unavailable.into()),
            }
        }
        Err(_) => return Err(ErrorKind::Unavailable.into()),
    }
    Ok(())
}

fn cleanup_preparing_audio(
    intent: &mut HelperIntent,
    journal: &HelperJournal,
) -> Result<(), RuntimeError> {
    // Reconcile the durable target before deciding a missing tombstone means
    // completion: a target was recorded before the no-replace rename.
    if intent.audio_retired_dir.is_some() {
        retire_audio_dir(intent, journal)?;
        return cleanup_audio_dir(intent);
    }
    let audio = intent.audio.as_ref().ok_or(ErrorKind::Protocol)?;
    match std::fs::symlink_metadata(&audio.socket_dir) {
        Ok(_) => {
            if !marker_matches(intent)? {
                return Err(ErrorKind::UnknownOutcome.into());
            }
            intent.audio_dir_created = true;
            retire_audio_dir(intent, journal)?;
            cleanup_audio_dir(intent)?;
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(_) => return Err(ErrorKind::Unavailable.into()),
    }
    Ok(())
}

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
fn valid_nvidia_gpu(run: &NvidiaGpuRun) -> bool {
    let mount_valid = match &run.driver_mount {
        NvidiaDriverMount::ReadOnlyBind(bind) => {
            bind.source.is_absolute()
                && bind.target.starts_with('/')
                && !bind.target.contains(['\0', ':'])
                && bind.source.as_os_str().len() + bind.target.len() <= 4 * 1024
        }
        NvidiaDriverMount::NamedVolume { name, target } => {
            !name.is_empty()
                && name.len() <= 255
                && !name.contains(['/', '\0', ':'])
                && target.starts_with('/')
                && !target.contains(['\0', ':'])
        }
    };
    mount_valid
        && !run.entrypoint.is_empty()
        && run
            .entrypoint
            .iter()
            .chain(run.command.iter())
            .all(|v| !v.is_empty() && !v.contains('\0'))
        && run
            .entrypoint
            .iter()
            .chain(run.command.iter())
            .map(String::len)
            .sum::<usize>()
            <= 8 * 1024
        && !run.image_ld_library_path.contains('\0')
}
fn valid_audio(run: &AudioRun, name: &str) -> bool {
    let socket = run.socket_dir.to_str();
    let suffix = name.strip_prefix("quasar-pulse-");
    socket.is_some()
        && run.socket_dir.is_absolute()
        && run
            .socket_dir
            .components()
            .all(|c| !matches!(c, std::path::Component::ParentDir))
        && suffix.is_some_and(|id| {
            run.socket_dir.file_name().and_then(|v| v.to_str()) == Some(&format!("pulse-{id}"))
        })
        && run.entrypoint == ["pulseaudio"]
        && [
            "--daemonize=no",
            "--system=no",
            "--disable-shm=true",
            "--exit-idle-time=-1",
            "--log-target=stderr",
            "-n",
        ]
        .iter()
        .all(|required| run.command.iter().any(|v| v == required))
        && run.command.iter().any(|v| {
            v == &format!(
                "--load=module-native-protocol-unix socket={}/native auth-anonymous=1",
                run.socket_dir.display()
            )
        })
        && run
            .command
            .iter()
            .any(|v| v.contains("module-null-sink sink_name=quasar_output"))
        && run
            .command
            .iter()
            .any(|v| v.contains("module-null-sink sink_name=quasar_mic"))
        && run.command.iter().any(|v| {
            v.contains("module-remap-source master=quasar_mic.monitor source_name=quasar_mic_src")
        })
        && run
            .command
            .iter()
            .all(|v| !v.is_empty() && !v.contains('\0'))
        && run.command.iter().map(String::len).sum::<usize>() <= 8 * 1024
}
fn fingerprint(
    helper: &DiagnosticHelper,
    run: Option<&DiagnosticRun>,
    nvidia_gpu: Option<&NvidiaGpuRun>,
    audio: Option<&AudioRun>,
) -> Result<String, RuntimeError> {
    // Existing diagnostic journals fingerprint exactly this two-tuple.  Keep
    // it stable so records written before the audio profile remain replayable.
    let bytes = if let Some(audio) = audio {
        serde_json::to_vec(&(helper, run, audio))
    } else if nvidia_gpu.is_some() {
        serde_json::to_vec(&(helper, run, nvidia_gpu))
    } else {
        serde_json::to_vec(&(helper, run))
    }
    .map(|v| {
        Sha256::digest(v)
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect()
    });
    bytes.map_err(|_| ErrorKind::Protocol.into())
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
    if let Some(audio) = &worst.audio {
        worst.audio_dir_created = true;
        worst.audio_retired_dir = Some(
            audio
                .socket_dir
                .parent()
                .unwrap_or_else(|| std::path::Path::new("/"))
                .join(format!(".quasar-audio-retired-{}", worst.operation)),
        );
        worst.audio_dir_device = Some(u64::MAX);
        worst.audio_dir_inode = Some(u64::MAX);
        worst.audio_dir_renamed = true;
    }
    Ok(serde_json::to_vec(&worst)
        .map_err(|_| ErrorKind::Protocol)?
        .len()
        <= MAX_JOURNAL_BYTES)
}
fn matches(
    intent: &HelperIntent,
    helper: &DiagnosticHelper,
    run: Option<&DiagnosticRun>,
    nvidia_gpu: Option<&NvidiaGpuRun>,
    audio: Option<&AudioRun>,
    owner: &str,
    config: &RuntimeConfig,
) -> Result<bool, RuntimeError> {
    Ok(intent.operation == helper.operation
        && intent.name == helper.name
        && intent.image == helper.image
        && intent.owner == owner
        && intent.socket == config.socket
        && intent.request_fingerprint == fingerprint(helper, run, nvidia_gpu, audio)?)
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

fn nvidia_mount(run: &NvidiaGpuRun) -> Mount {
    match &run.driver_mount {
        NvidiaDriverMount::ReadOnlyBind(bind) => Mount {
            typ: Some(MountType::BIND),
            source: bind.source.to_str().map(str::to_owned),
            target: Some(bind.target.clone()),
            read_only: Some(true),
            bind_options: Some(MountBindOptions {
                create_mountpoint: Some(false),
                ..Default::default()
            }),
            ..Default::default()
        },
        NvidiaDriverMount::NamedVolume { name, target } => Mount {
            typ: Some(MountType::VOLUME),
            source: Some(name.clone()),
            target: Some(target.clone()),
            read_only: Some(true),
            ..Default::default()
        },
    }
}
fn nvidia_env(run: &NvidiaGpuRun) -> Vec<String> {
    let target = match &run.driver_mount {
        NvidiaDriverMount::ReadOnlyBind(bind) => &bind.target,
        NvidiaDriverMount::NamedVolume { target, .. } => target,
    };
    let mut ld = vec![format!("{target}/lib64")];
    ld.extend(
        run.image_ld_library_path
            .split(':')
            .filter(|v| !v.is_empty())
            .map(str::to_owned),
    );
    let mut env = vec![
        format!("LD_LIBRARY_PATH={}", ld.join(":")),
        format!("__EGL_VENDOR_LIBRARY_DIRS={target}/glvnd/egl_vendor.d:/etc/glvnd/egl_vendor.d:/usr/share/glvnd/egl_vendor.d"),
        format!("__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS={target}/egl_external_platform.d:/usr/share/egl/egl_external_platform.d"),
        format!("VK_ADD_DRIVER_FILES={target}/vulkan/icd.d/nvidia_icd.json"),
    ];
    if run.has_gbm_backend {
        env.push(format!("GBM_BACKENDS_PATH={target}/gbm"));
    }
    env
}
/// Docker inspect includes the image's inherited environment as well as the
/// values supplied at create.  Require each Quasar-controlled loader key once
/// and byte-for-byte, while leaving unrelated image defaults intact.
fn has_nvidia_env(env: &[String], run: &NvidiaGpuRun) -> bool {
    let expected = nvidia_env(run);
    let controlled = [
        "LD_LIBRARY_PATH",
        "__EGL_VENDOR_LIBRARY_DIRS",
        "__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS",
        "VK_ADD_DRIVER_FILES",
        "GBM_BACKENDS_PATH",
    ];
    expected.iter().all(|required| {
        let (key, _) = required
            .split_once('=')
            .expect("fixed environment assignment");
        env.iter()
            .filter(|value| value.starts_with(&format!("{key}=")))
            .collect::<Vec<_>>()
            == vec![required]
    }) && controlled.iter().all(|key| {
        let expected_value = expected
            .iter()
            .find(|value| value.starts_with(&format!("{key}=")));
        let actual = env
            .iter()
            .filter(|value| value.starts_with(&format!("{key}=")))
            .collect::<Vec<_>>();
        match expected_value {
            Some(value) => actual == vec![value],
            None => actual.is_empty(),
        }
    })
}
fn is_nvidia_all_request(request: &DeviceRequest) -> bool {
    request.driver.as_deref() == Some("nvidia")
        && request.count == Some(-1)
        && request.device_ids.as_ref().is_none_or(Vec::is_empty)
        && request.capabilities.as_deref() == Some(&[vec!["gpu".to_owned()]])
        && request
            .options
            .as_ref()
            .is_none_or(std::collections::HashMap::is_empty)
}
fn matches_nvidia_mount(mount: &bollard::models::MountPoint, run: &NvidiaGpuRun) -> bool {
    let (typ, source, name, target) = match &run.driver_mount {
        NvidiaDriverMount::ReadOnlyBind(bind) => ("bind", bind.source.to_str(), None, &bind.target),
        NvidiaDriverMount::NamedVolume { name, target } => {
            // Docker realizes a named volume source as a daemon storage path;
            // the stable volume identity is `Name`, not that host-private path.
            ("volume", None, Some(name.as_str()), target)
        }
    };
    mount.typ.as_deref() == Some(typ)
        && (source.is_none() || mount.source.as_deref() == source)
        && mount.name.as_deref() == name
        && mount.destination.as_deref() == Some(target)
        && mount.rw == Some(false)
}
fn requested_nvidia_mount(mount: &Mount, run: &NvidiaGpuRun) -> bool {
    let expected = nvidia_mount(run);
    mount.typ == expected.typ
        && mount.source == expected.source
        && mount.target == expected.target
        && mount.read_only == Some(true)
        && match &run.driver_mount {
            NvidiaDriverMount::ReadOnlyBind(_) => {
                safe_readonly_bind_options(mount.bind_options.as_ref())
            }
            NvidiaDriverMount::NamedVolume { .. } => mount.bind_options.is_none(),
        }
}
fn safe_readonly_bind_options(options: Option<&MountBindOptions>) -> bool {
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
        || host.readonly_rootfs != Some(intent.profile != HelperProfile::Audio)
        || host.privileged != Some(false)
        || host.auto_remove != Some(false)
        || host.cap_add.as_ref().is_some_and(|v| !v.is_empty())
        || host.devices.as_ref().is_some_and(|v| !v.is_empty())
        || (intent.profile != HelperProfile::NvidiaGpu
            && host.device_requests.as_ref().is_some_and(|v| !v.is_empty()))
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
        || (intent.profile == HelperProfile::Audio && host.pids_limit != Some(512))
    {
        return Err(ErrorKind::Protocol.into());
    }
    if intent.profile == HelperProfile::NvidiaGpu {
        let run = intent.nvidia_gpu.as_ref().ok_or(ErrorKind::Protocol)?;
        if !matches!(host.device_requests.as_deref(), Some([request]) if is_nvidia_all_request(request))
            || c.entrypoint.as_ref() != Some(&run.entrypoint)
            || c.cmd.as_ref() != Some(&run.command)
            || !c.env.as_deref().is_some_and(|env| has_nvidia_env(env, run))
        {
            return Err(ErrorKind::Protocol.into());
        }
        let mounts = info.mounts.as_deref().unwrap_or(&[]);
        let requested = host.mounts.as_deref().unwrap_or(&[]);
        if mounts.len() != 1
            || !matches_nvidia_mount(&mounts[0], run)
            || requested.len() != 1
            || !requested_nvidia_mount(&requested[0], run)
        {
            return Err(ErrorKind::Protocol.into());
        }
    }
    if intent.profile != HelperProfile::NvidiaGpu {
        if let Some(run) = &intent.run {
            if c.entrypoint.as_ref() != Some(&run.entrypoint)
                || c.cmd.as_ref() != Some(&run.command)
            {
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
        } else if let Some(audio) = &intent.audio {
            let env = c.env.as_deref().unwrap_or(&[]);
            let expected_home = format!("HOME={}", audio.socket_dir.display());
            let expected_runtime =
                format!("PULSE_RUNTIME_PATH={}/.runtime", audio.socket_dir.display());
            if c.entrypoint.as_ref() != Some(&audio.entrypoint)
                || c.cmd.as_ref() != Some(&audio.command)
                || env.iter().filter(|v| v.starts_with("HOME=")).count() != 1
                || !env.iter().any(|v| v == &expected_home)
                || env
                    .iter()
                    .filter(|v| v.starts_with("PULSE_RUNTIME_PATH="))
                    .count()
                    != 1
                || !env.iter().any(|v| v == &expected_runtime)
                || c.healthcheck
                    .as_ref()
                    .and_then(|h| h.test.as_ref())
                    .map(Vec::as_slice)
                    != Some(&["NONE".to_owned()])
            {
                return Err(ErrorKind::Protocol.into());
            }
            let mounts = info.mounts.unwrap_or_default();
            if mounts.len() != 1
                || mounts[0].typ.as_deref() != Some("bind")
                || mounts[0].source.as_deref() != audio.socket_dir.to_str()
                || mounts[0].destination.as_deref() != audio.socket_dir.to_str()
                || mounts[0].rw != Some(true)
            {
                return Err(ErrorKind::Protocol.into());
            }
            let requested = host.mounts.as_deref().unwrap_or(&[]);
            if requested.len() != 1 || requested[0].typ != Some(MountType::BIND)
            || requested[0].source.as_deref() != audio.socket_dir.to_str()
            || requested[0].target.as_deref() != audio.socket_dir.to_str()
            // Docker omits `ReadOnly` when false in inspect output; the
            // realized mount's RW=true remains mandatory for this profile.
            || requested[0].read_only == Some(true)
            || requested[0].bind_options.as_ref().is_some_and(|o| o.create_mountpoint == Some(true))
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
                if !safe_propagation
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
    nvidia_gpu: Option<NvidiaGpuRun>,
    audio: Option<AudioRun>,
) -> Result<(bollard::Docker, HelperJournal, HelperIntent), RuntimeError> {
    if !valid_helper(&helper)
        || run.as_ref().is_some_and(|r| !valid_run(r))
        || nvidia_gpu.as_ref().is_some_and(|r| !valid_nvidia_gpu(r))
        || audio
            .as_ref()
            .is_some_and(|r| !valid_audio(r, &helper.name))
        || (run.is_some() as u8 + nvidia_gpu.is_some() as u8 + audio.is_some() as u8) != 1
    {
        return Err(ErrorKind::InvalidConfiguration.into());
    }
    let owner = owner(config)?;
    let docker = open(config).await?;
    let journal = HelperJournal::acquire(config, &helper.operation).await?;
    if let Some(intent) = journal.read()? {
        if !matches(
            &intent,
            &helper,
            run.as_ref(),
            nvidia_gpu.as_ref(),
            audio.as_ref(),
            &owner,
            config,
        )? {
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
    let is_audio = audio.is_some();
    let is_nvidia_gpu = nvidia_gpu.is_some();
    let mut intent = HelperIntent {
        operation: helper.operation.clone(),
        name: helper.name.clone(),
        image: helper.image.clone(),
        owner,
        socket: config.socket.clone(),
        id: None,
        request_fingerprint: fingerprint(
            &helper,
            run.as_ref(),
            nvidia_gpu.as_ref(),
            audio.as_ref(),
        )?,
        run,
        nvidia_gpu,
        profile: if is_audio {
            HelperProfile::Audio
        } else if is_nvidia_gpu {
            HelperProfile::NvidiaGpu
        } else {
            HelperProfile::Diagnostic
        },
        audio,
        audio_dir_created: false,
        audio_retired_dir: None,
        audio_dir_device: None,
        audio_dir_inode: None,
        audio_dir_renamed: false,
        phase: if is_audio {
            HelperPhase::Preparing
        } else {
            HelperPhase::Creating
        },
        result: None,
    };
    if !reserves_final_evidence(&intent)? {
        return Err(ErrorKind::InvalidConfiguration.into());
    }
    journal.write(&intent)?;
    if intent.profile == HelperProfile::Audio {
        create_audio_dir(&intent)?;
        intent.audio_dir_created = true;
        intent.phase = HelperPhase::Creating;
        journal.write(&intent)?;
    }
    let mut labels = HashMap::new();
    labels.insert(container_ownership::LABEL.to_owned(), intent.owner.clone());
    labels.insert(OPERATION_LABEL.to_owned(), helper.operation.clone());
    let mounts = intent
        .nvidia_gpu
        .as_ref()
        .map(|r| vec![nvidia_mount(r)])
        .or_else(|| {
            intent
                .run
                .as_ref()
                .map(|r| {
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
                })
                .or_else(|| {
                    intent.audio.as_ref().map(|r| {
                        vec![Mount {
                            typ: Some(MountType::BIND),
                            source: r.socket_dir.to_str().map(str::to_owned),
                            target: r.socket_dir.to_str().map(str::to_owned),
                            read_only: Some(false),
                            bind_options: Some(MountBindOptions {
                                create_mountpoint: Some(false),
                                ..Default::default()
                            }),
                            ..Default::default()
                        }]
                    })
                })
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
        entrypoint: intent
            .nvidia_gpu
            .as_ref()
            .map(|r| r.entrypoint.clone())
            .or_else(|| {
                intent
                    .run
                    .as_ref()
                    .map(|r| r.entrypoint.clone())
                    .or_else(|| intent.audio.as_ref().map(|r| r.entrypoint.clone()))
            }),
        cmd: intent
            .nvidia_gpu
            .as_ref()
            .map(|r| r.command.clone())
            .or_else(|| {
                intent
                    .run
                    .as_ref()
                    .map(|r| r.command.clone())
                    .or_else(|| intent.audio.as_ref().map(|r| r.command.clone()))
            }),
        env: intent.nvidia_gpu.as_ref().map(nvidia_env).or_else(|| {
            intent.audio.as_ref().map(|r| {
                vec![
                    format!("HOME={}", r.socket_dir.display()),
                    format!("PULSE_RUNTIME_PATH={}/.runtime", r.socket_dir.display()),
                ]
            })
        }),
        healthcheck: intent.audio.as_ref().map(|_| HealthConfig {
            test: Some(vec!["NONE".into()]),
            ..Default::default()
        }),
        host_config: Some(HostConfig {
            network_mode: Some(network.into()),
            readonly_rootfs: Some(if intent.profile == HelperProfile::Audio {
                false
            } else {
                readonly_rootfs
            }),
            privileged: Some(false),
            auto_remove: Some(false),
            cap_drop: Some(cap_drop),
            security_opt: Some(security_opt),
            devices: Some(devices),
            device_requests: intent.nvidia_gpu.as_ref().map(|_| {
                vec![DeviceRequest {
                    driver: Some("nvidia".into()),
                    count: Some(-1),
                    device_ids: None,
                    capabilities: Some(vec![vec!["gpu".into()]]),
                    options: None,
                }]
            }),
            mounts,
            pids_limit: intent.audio.as_ref().map(|_| 512),
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
            if intent.profile == HelperProfile::Audio {
                // Docker conclusively rejected create.  Persist a directory-only
                // cleanup obligation before attempting retirement so an I/O
                // failure cannot strand a Creating/no-ID journal forever.
                intent.phase = HelperPhase::CleanupPending;
                journal.write(&intent)?;
                retire_audio_dir(&mut intent, &journal)?;
                cleanup_audio_dir(&intent)?;
            }
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
    run_inner(config, helper, Some(run), None, None).await
}

pub(crate) async fn run_nvidia_gpu(
    config: &RuntimeConfig,
    helper: DiagnosticHelper,
    run: NvidiaGpuRun,
) -> Result<OwnedHelperId, RuntimeError> {
    run_inner(config, helper, None, Some(run), None).await
}

async fn run_inner(
    config: &RuntimeConfig,
    helper: DiagnosticHelper,
    run: Option<DiagnosticRun>,
    nvidia_gpu: Option<NvidiaGpuRun>,
    audio: Option<AudioRun>,
) -> Result<OwnedHelperId, RuntimeError> {
    let (docker, journal, mut intent) =
        create_or_adopt_inner(config, helper, run, nvidia_gpu, audio).await?;
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

pub(crate) async fn run_audio(
    config: &RuntimeConfig,
    helper: DiagnosticHelper,
    run: AudioRun,
) -> Result<OwnedHelperId, RuntimeError> {
    run_inner(config, helper, None, None, Some(run)).await
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
        || latest.nvidia_gpu != intent.nvidia_gpu
        || latest.profile != intent.profile
        || latest.audio != intent.audio
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
    if intent.profile == HelperProfile::Audio && intent.phase == HelperPhase::Completed {
        return Ok(());
    }
    if intent.profile == HelperProfile::Audio && intent.phase == HelperPhase::CleanupPending {
        drop(journal);
        return cleanup(config, id).await;
    }
    // Audio stop is a durable teardown request.  Record it before touching an
    // unavailable daemon so the next startup can reconcile the same immutable
    // sidecar; diagnostic behavior remains byte-for-byte compatible.
    if intent.profile == HelperProfile::Audio && intent.phase != HelperPhase::Stopped {
        intent.phase = HelperPhase::Stopping;
        journal.write(&intent)?;
    }
    let docker = open(config).await?;
    let (_, running, mut exit) = inspect(&docker, &intent).await?;
    if intent.profile == HelperProfile::Audio
        && !running
        && exit.is_none()
        && intent.phase == HelperPhase::Stopping
    {
        // Docker's created state has no exit evidence because it never ran.
        // It is nevertheless safe to remove after the explicit, durable
        // abandonment request; cleanup retains `None` rather than calling it
        // a successful workload execution.
        intent.phase = HelperPhase::Stopped;
        journal.write(&intent)?;
        return Ok(());
    }
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

/// Mark an audio launch whose caller never received an ID as abandoned.  This
/// only operates on an existing durable operation; it never searches names.
pub(crate) async fn abandon_audio(
    config: &RuntimeConfig,
    operation: &str,
) -> Result<(), RuntimeError> {
    let journal = HelperJournal::acquire(config, operation).await?;
    let mut intent = journal.read()?.ok_or(ErrorKind::UnknownOutcome)?;
    current_intent(config, &intent)?;
    if intent.profile != HelperProfile::Audio {
        return Err(ErrorKind::UnknownOutcome.into());
    }
    if intent.phase == HelperPhase::Completed {
        return Ok(());
    }
    if intent.phase == HelperPhase::Preparing {
        cleanup_preparing_audio(&mut intent, &journal)?;
        journal.discard_definitive()?;
        return Ok(());
    }
    if intent.phase == HelperPhase::CleanupPending {
        let id = owned(&intent)?;
        drop(journal);
        return cleanup(config, id).await;
    }
    intent.phase = HelperPhase::Stopping;
    journal.write(&intent)?;
    let docker = open(config).await?;
    if intent.id.is_none() {
        let info = docker
            .inspect_container(&intent.name, None)
            .await
            .map_err(uncertain_inspection)?;
        let id = info
            .id
            .clone()
            .filter(|v| valid_id(v))
            .ok_or(ErrorKind::UnknownOutcome)?;
        intent.id = Some(id);
        let _ = inspect_owned(info, &intent)?;
        journal.write(&intent)?;
    }
    let id = owned(&intent)?;
    drop(journal);
    stop(config, id.clone()).await?;
    cleanup(config, id).await
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
                retire_audio_dir(&mut intent, &journal)?;
                cleanup_audio_dir(&intent)?;
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
            retire_audio_dir(&mut intent, &journal)?;
            cleanup_audio_dir(&intent)?;
            intent.phase = HelperPhase::Completed;
            journal.write(&intent)
        }
        Ok(_) => Err(ErrorKind::UnknownOutcome.into()),
        Err(error) => Err(uncertain_inspection(error)),
    }
}

pub(crate) async fn recover(config: &RuntimeConfig) -> Result<(), RuntimeError> {
    // One stalled legacy diagnostic must not strand a separately journaled GPU
    // helper. Both reconciliations are bounded; return the first failure only
    // after attempting each independent profile.
    let diagnostic = recover_profile(config, HelperProfile::Diagnostic).await;
    let nvidia_gpu = recover_profile(config, HelperProfile::NvidiaGpu).await;
    diagnostic.and(nvidia_gpu)
}

pub(crate) async fn recover_audio(config: &RuntimeConfig) -> Result<(), RuntimeError> {
    recover_profile(config, HelperProfile::Audio).await
}

/// Boot-only retirement of the previous agent's recorded audio workloads.
/// This deliberately enumerates journals, never Docker names, then delegates
/// each operation to the durable abandonment path.
pub(crate) async fn retire_audio(config: &RuntimeConfig) -> Result<(), RuntimeError> {
    let root = config
        .image_state_path
        .as_ref()
        .ok_or(ErrorKind::InvalidConfiguration)?
        .join("helpers");
    let entries = match std::fs::read_dir(root) {
        Ok(v) => v,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(_) => return Err(ErrorKind::Unavailable.into()),
    };
    let mut operations = Vec::new();
    let mut failure = None;
    for entry in entries {
        let entry = match entry {
            Ok(v) => v,
            Err(_) => {
                failure = Some(ErrorKind::Unavailable);
                continue;
            }
        };
        let path = entry.path();
        if path.extension().and_then(|v| v.to_str()) == Some("lock")
            || path.extension().and_then(|v| v.to_str()) == Some("new")
        {
            continue;
        }
        if !entry
            .file_type()
            .map_err(|_| ErrorKind::Unavailable)?
            .is_file()
        {
            failure = Some(ErrorKind::Protocol);
            continue;
        }
        use std::{io::Read, os::unix::fs::OpenOptionsExt};
        let mut bytes = Vec::new();
        if std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW)
            .open(path)
            .map_err(|_| ErrorKind::Unavailable)
            .and_then(|file| {
                file.take((MAX_JOURNAL_BYTES + 1) as u64)
                    .read_to_end(&mut bytes)
                    .map_err(|_| ErrorKind::Unavailable)
            })
            .is_err()
        {
            failure = Some(ErrorKind::Unavailable);
            continue;
        }
        if bytes.len() > MAX_JOURNAL_BYTES {
            failure = Some(ErrorKind::Protocol);
            continue;
        }
        let intent: HelperIntent = match serde_json::from_slice(&bytes) {
            Ok(v) => v,
            Err(_) => {
                failure = Some(ErrorKind::Protocol);
                continue;
            }
        };
        if current_intent(config, &intent).is_err() {
            failure = Some(ErrorKind::UnknownOutcome);
            continue;
        }
        if intent.profile == HelperProfile::Audio && intent.phase != HelperPhase::Completed {
            operations.push(intent.operation);
        }
    }
    // Persist every termination request before a daemon call can block later
    // records behind an unavailable engine.
    for operation in &operations {
        match HelperJournal::acquire(config, operation).await {
            Ok(journal) => match journal.read() {
                Ok(Some(mut intent)) => {
                    // The scan raced with another lifecycle action.  Recheck
                    // the durable record while holding its lease before
                    // recording an irreversible boot-retirement request.
                    if let Err(error) = current_intent(config, &intent) {
                        failure = Some(error.kind);
                    } else if matches!(
                        intent.phase,
                        HelperPhase::Running
                            | HelperPhase::Starting
                            | HelperPhase::Creating
                            | HelperPhase::Created
                    ) {
                        intent.phase = HelperPhase::Stopping;
                        if journal.write(&intent).is_err() {
                            failure = Some(ErrorKind::Unavailable);
                        }
                    }
                }
                Ok(None) => {}
                Err(error) => failure = Some(error.kind),
            },
            Err(error) => failure = Some(error.kind),
        }
    }
    for operation in operations {
        if let Err(error) = abandon_audio(config, &operation).await {
            failure = Some(error.kind);
        }
    }
    failure.map_or(Ok(()), |kind| Err(kind.into()))
}

async fn recover_profile(
    config: &RuntimeConfig,
    profile: HelperProfile,
) -> Result<(), RuntimeError> {
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
        // NVIDIA readiness calls diagnostic recovery.  An active audio sibling
        // is a long-running workload and must neither make that path Busy nor
        // be stopped by it.
        if scanned.profile != profile {
            continue;
        }
        if profile == HelperProfile::Audio
            && matches!(
                scanned.phase,
                HelperPhase::Running | HelperPhase::Starting | HelperPhase::Creating
            )
        {
            // Pending recovery is never a hidden stop request for live audio.
            continue;
        }
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
        if profile == HelperProfile::Audio
            && matches!(
                intent.phase,
                HelperPhase::Running | HelperPhase::Starting | HelperPhase::Creating
            )
        {
            // Re-check under the operation lease: a stop/start transition can
            // race the initial scan, and routine recovery never terminates it.
            continue;
        }
        if profile == HelperProfile::Audio && intent.phase == HelperPhase::Preparing {
            // No Docker create was submitted in this phase. A marker proves a
            // partial local mkdir belongs to this operation; absence means the
            // preparation never reached the filesystem.
            cleanup_preparing_audio(&mut intent, &journal)?;
            journal.discard_definitive()?;
            continue;
        }
        if profile == HelperProfile::Audio
            && intent.phase == HelperPhase::CleanupPending
            && intent.id.is_none()
        {
            // A definitive create rejection has no container to inspect.  Its
            // marker-backed directory obligation is completed directly.
            retire_audio_dir(&mut intent, &journal)?;
            cleanup_audio_dir(&intent)?;
            intent.phase = HelperPhase::Completed;
            journal.write(&intent)?;
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
