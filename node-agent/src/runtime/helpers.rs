use std::path::PathBuf;

// Two 4KiB raw streams can expand to six bytes per byte in JSON. Leave room
// for escaped output plus the fixed request/identity record.
pub const MAX_JOURNAL_BYTES: usize = 64 * 1024;
pub const MAX_LOG_BYTES: usize = 4 * 1024;

/// Quasar-owned requirements for this diagnostic class. These narrow enums
/// describe the supported profile without exposing engine request types.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DiagnosticNetwork {
    None,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DiagnosticDevices {
    None,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DiagnosticSecurity {
    LockedDownRoot,
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DiagnosticRequirements {
    pub network: DiagnosticNetwork,
    pub devices: DiagnosticDevices,
    pub security: DiagnosticSecurity,
}
impl DiagnosticRequirements {
    /// Docker realization: network none, no devices, read-only root, cap-drop
    /// ALL, no-new-privileges, and UID:GID 0:0.
    pub const FIXED: Self = Self {
        network: DiagnosticNetwork::None,
        devices: DiagnosticDevices::None,
        security: DiagnosticSecurity::LockedDownRoot,
    };
}

/// Fixed security posture for a short-lived engine diagnostic.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct DiagnosticHelper {
    pub operation: String,
    pub name: String,
    pub image: String,
}

/// An opaque immutable container identity tied to its durable operation.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OwnedHelperId {
    pub(super) id: String,
    pub(super) operation: String,
}
impl OwnedHelperId {
    pub fn as_str(&self) -> &str {
        &self.id
    }
}

#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct ReadOnlyHostBind {
    pub source: PathBuf,
    pub target: String,
}

/// The only supported diagnostic execution profile. There are deliberately no
/// knobs for host networking, devices, writable mounts or privileges.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct DiagnosticRun {
    pub entrypoint: Vec<String>,
    pub command: Vec<String>,
    pub bind: ReadOnlyHostBind,
}

/// One read-only NVIDIA userspace mount accepted by the GPU diagnostic
/// profile.  Keeping the daemon-host bind and named-volume forms distinct is
/// important: a host path is meaningful only to the engine daemon, while a
/// volume name is the stable identity Docker resolves itself.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub enum NvidiaDriverMount {
    ReadOnlyBind(ReadOnlyHostBind),
    NamedVolume { name: String, target: String },
}

/// NVIDIA driver access for a GPU probe: Docker's all-NVIDIA-GPUs device
/// request, the read-only driver mount and the loader environment. This is
/// exactly what the NVIDIA GPU diagnostic realized before it was widened into
/// the GPU probe profile (#258); the realized create body is byte-for-byte the
/// same.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct NvidiaDriverAccess {
    pub driver_mount: NvidiaDriverMount,
    pub image_ld_library_path: String,
    pub has_gbm_backend: bool,
}

/// The one closed GPU probe profile, for every vendor (#258, spec #252 "Host
/// probes"). A probe container is given what a session's application
/// container is given for GPU access, and nothing else: the DRM nodes and the
/// groups owning them (AMD, Intel, and NVIDIA's render node), plus the NVIDIA
/// device request on an NVIDIA host and the Quasar driver volume when the host
/// has one — the two are independent, since a toolkit host has the first and not
/// the second. No network, a
/// read-only root, no capabilities, no other mounts, and no caller-controlled
/// environment. The container name carries
/// [`crate::container_ownership::PROBE_NAME_PREFIX`].
///
/// An unsupported requirement — a device outside `/dev/dri`, group 0, a probe
/// with no GPU access at all — is refused before any engine request. Nothing
/// is ever weakened to make a request fit.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct GpuProbeRun {
    pub entrypoint: Vec<String>,
    pub command: Vec<String>,
    /// `/dev/dri` itself or device nodes directly beneath it, realized as
    /// `rwm` device mappings at the same path.
    pub devices: Vec<String>,
    /// Numeric supplementary groups owning those nodes: never 0, sorted, no
    /// duplicates, so one request has one fingerprint.
    pub groups: Vec<u32>,
    /// The all-GPUs `nvidia` device request, as a session's application container
    /// gets it. True on every NVIDIA host, whether or not the host also has a
    /// Quasar driver volume: where the driver userspace comes from the container
    /// toolkit this request is the ONLY thing that carries it in (#280).
    #[serde(default)]
    pub nvidia_device_request: bool,
    /// The Quasar driver volume's mount and loader environment. `None` on a host
    /// that has no such volume — including an NVIDIA host running its own driver
    /// packages, which is served by `nvidia_device_request` alone.
    pub nvidia: Option<NvidiaDriverAccess>,
}

/// The NVIDIA GPU/EGL diagnostic as recorded by agents before #258. Journals of
/// this shape are still loaded and recovered; nothing writes one any more.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub(crate) struct NvidiaGpuRun {
    pub entrypoint: Vec<String>,
    pub command: Vec<String>,
    pub driver_mount: NvidiaDriverMount,
    pub image_ld_library_path: String,
    pub has_gbm_backend: bool,
}

/// The fixed, long-running PulseAudio sibling profile.  This is deliberately
/// not a general container request: the only writable mount is the
/// session-private socket directory and its environment is fixed by the
/// profile.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct AudioRun {
    pub socket_dir: PathBuf,
    pub entrypoint: Vec<String>,
    pub command: Vec<String>,
}

#[derive(Debug, Clone, Copy, serde::Serialize, serde::Deserialize, PartialEq, Eq, Default)]
pub(crate) enum HelperProfile {
    #[default]
    Diagnostic,
    /// Pre-#258 journals only; see [`NvidiaGpuRun`].
    NvidiaGpu,
    Audio,
    GpuProbe,
}

#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct HelperResult {
    /// `None` is deliberately unknown; it is never converted into success.
    pub exit_code: Option<i64>,
    pub stdout: String,
    pub stderr: String,
}

#[derive(Debug, Clone, Copy, serde::Serialize, serde::Deserialize, PartialEq, Eq)]
pub(crate) enum HelperPhase {
    /// Audio-only: durable directory preparation before any Docker create can
    /// have been submitted.  This makes a marker-creation interruption
    /// conclusively local rather than an unknown daemon mutation.
    Preparing,
    Creating,
    Created,
    Starting,
    Running,
    Stopping,
    Stopped,
    CleanupPending,
    Completed,
}

#[derive(Debug, Clone, serde::Serialize, serde::Deserialize, PartialEq, Eq)]
pub(crate) struct HelperIntent {
    pub operation: String,
    pub name: String,
    pub image: String,
    pub owner: String,
    pub socket: PathBuf,
    pub id: Option<String>,
    #[serde(default)]
    pub request_fingerprint: String,
    #[serde(default)]
    pub run: Option<DiagnosticRun>,
    #[serde(default)]
    pub nvidia_gpu: Option<NvidiaGpuRun>,
    /// Absent in every journal written before #258; part of the fingerprint.
    #[serde(default)]
    pub gpu_probe: Option<GpuProbeRun>,
    /// Kept separate from `run` so journals written by the diagnostic-only
    /// implementation continue to decode as the diagnostic profile.
    #[serde(default)]
    pub profile: HelperProfile,
    #[serde(default)]
    pub audio: Option<AudioRun>,
    #[serde(default)]
    pub audio_dir_created: bool,
    #[serde(default)]
    pub audio_retired_dir: Option<PathBuf>,
    #[serde(default)]
    pub audio_dir_device: Option<u64>,
    #[serde(default)]
    pub audio_dir_inode: Option<u64>,
    #[serde(default)]
    pub audio_dir_renamed: bool,
    #[serde(default = "default_phase")]
    pub phase: HelperPhase,
    #[serde(default)]
    pub result: Option<HelperResult>,
}
fn default_phase() -> HelperPhase {
    HelperPhase::Creating
}

/// Per-operation fsynced journal. Its lock serializes recovery with a live
/// operation and the record is bounded on both read and write.
pub(crate) struct HelperJournal {
    path: PathBuf,
    _lease: std::fs::File,
}
impl HelperJournal {
    pub async fn acquire(
        config: &super::RuntimeConfig,
        operation: &str,
    ) -> Result<Self, super::RuntimeError> {
        use sha2::{Digest, Sha256};
        use std::{
            os::{
                fd::AsRawFd,
                unix::fs::{DirBuilderExt, OpenOptionsExt},
            },
            time::Duration,
        };
        let root = config
            .image_state_path
            .as_ref()
            .ok_or(super::ErrorKind::InvalidConfiguration)?
            .join("helpers");
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&root)
            .map_err(|_| super::ErrorKind::Unavailable)?;
        let key = Sha256::digest(operation.as_bytes())
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect::<String>();
        let lease = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW)
            .open(root.join(format!("{key}.lock")))
            .map_err(|_| super::ErrorKind::Unavailable)?;
        loop {
            // SAFETY: lease remains open until this operation releases the lock.
            if unsafe { libc::flock(lease.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } == 0 {
                break;
            }
            if std::io::Error::last_os_error().kind() != std::io::ErrorKind::WouldBlock {
                return Err(super::ErrorKind::Unavailable.into());
            }
            tokio::time::sleep(Duration::from_millis(25)).await;
        }
        Ok(Self {
            path: root.join(key),
            _lease: lease,
        })
    }
    pub fn read(&self) -> Result<Option<HelperIntent>, super::RuntimeError> {
        use std::{io::Read, os::unix::fs::OpenOptionsExt};
        match std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW)
            .open(&self.path)
        {
            Ok(file) => {
                let mut bytes = Vec::new();
                file.take((MAX_JOURNAL_BYTES + 1) as u64)
                    .read_to_end(&mut bytes)
                    .map_err(|_| super::ErrorKind::Unavailable)?;
                if bytes.len() > MAX_JOURNAL_BYTES {
                    return Err(super::ErrorKind::Protocol.into());
                }
                serde_json::from_slice(&bytes)
                    .map(Some)
                    .map_err(|_| super::ErrorKind::Protocol.into())
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(_) => Err(super::ErrorKind::Unavailable.into()),
        }
    }
    pub fn write(&self, intent: &HelperIntent) -> Result<(), super::RuntimeError> {
        use std::{io::Write, os::unix::fs::OpenOptionsExt};
        let bytes = serde_json::to_vec(intent).map_err(|_| super::ErrorKind::Protocol)?;
        if bytes.len() > MAX_JOURNAL_BYTES {
            return Err(super::ErrorKind::Protocol.into());
        }
        let temp = self.path.with_extension("new");
        match std::fs::remove_file(&temp) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(_) => return Err(super::ErrorKind::Unavailable.into()),
        }
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW)
            .open(&temp)
            .map_err(|_| super::ErrorKind::Unavailable)?;
        file.write_all(&bytes)
            .and_then(|_| file.sync_all())
            .map_err(|_| super::ErrorKind::Unavailable)?;
        std::fs::rename(&temp, &self.path).map_err(|_| super::ErrorKind::Unavailable)?;
        self.sync_parent()
    }
    /// Used only when Docker conclusively rejected a mutation before creating
    /// anything. Ambiguous mutations intentionally retain their journal.
    pub fn discard_definitive(&self) -> Result<(), super::RuntimeError> {
        match std::fs::remove_file(&self.path) {
            Ok(()) => self.sync_parent(),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(_) => Err(super::ErrorKind::Unavailable.into()),
        }
    }
    fn sync_parent(&self) -> Result<(), super::RuntimeError> {
        std::fs::File::open(self.path.parent().unwrap())
            .and_then(|f| f.sync_all())
            .map_err(|_| super::ErrorKind::Unavailable.into())
    }
}
