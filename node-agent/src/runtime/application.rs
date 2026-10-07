//! Quasar-owned description of an application container lifecycle.
//!
//! These are deliberately runtime types, rather than Bollard request types: the
//! session layer says what it needs and the engine adapter is responsible for
//! translating and verifying it.

/// The daemon identity of one application, bound to its durable operation.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApplicationId {
    pub(crate) id: String,
    pub(crate) operation: String,
}
impl ApplicationId {
    pub fn as_str(&self) -> &str {
        &self.id
    }
}

/// Security and resource posture selected by the application launch policy.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct ApplicationSecurity {
    pub cap_drop_all: bool,
    pub cap_add: Vec<String>,
    pub no_new_privileges: bool,
    /// Docker CLI's `systempaths=unconfined` shortcut is represented by the
    /// actual HostConfig path lists in the API adapter, never sent as an
    /// unsupported daemon SecurityOpt string.
    pub systempaths_unconfined: bool,
    pub security_opt: Vec<String>,
    pub read_only_rootfs: bool,
    pub pids_limit: i64,
    pub shm_size: i64,
}
impl Default for ApplicationSecurity {
    fn default() -> Self {
        Self {
            cap_drop_all: true,
            cap_add: Vec::new(),
            // The catalog chooses this explicitly.  A default here must not
            // silently undo its documented opt-out.
            no_new_privileges: false,
            systempaths_unconfined: false,
            security_opt: Vec::new(),
            read_only_rootfs: false,
            pids_limit: 8192,
            shm_size: 1024 * 1024 * 1024,
        }
    }
}

/// A `--mount` request retains Docker's typed semantics.  In particular bind
/// mounts use `CreateMountpoint=false`, while named volumes remain volumes.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub enum ApplicationMount {
    Bind {
        source: String,
        target: String,
        read_only: bool,
        consistency: Option<String>,
    },
    Volume {
        source: String,
        target: String,
        read_only: bool,
        no_copy: bool,
    },
}

/// The complete application request, expressed without engine SDK types.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct ApplicationRequest {
    pub operation: String,
    pub name: String,
    pub image: String,
    /// Only catalog `require_local_image` maps to `--pull=never`; ordinary
    /// launches use the owned image manager's pull-if-missing policy.
    pub pull_never: bool,
    pub entrypoint: Option<Vec<String>>,
    pub command: Vec<String>,
    pub environment: Vec<String>,
    /// Already policy-normalized Docker bind specifications. Keeping this wire
    /// form retains allowed `z`, `Z`, `nocopy`, and consistency options rather
    /// than silently weakening them during the API migration.
    pub mounts: Vec<String>,
    /// Typed `--mount` values. `mounts` above is reserved for legacy `-v`
    /// syntax whose SELinux and consistency suffixes must remain byte exact.
    pub typed_mounts: Vec<ApplicationMount>,
    pub devices: Vec<String>,
    /// `/dev/dri/cardN` nodes created with mknod-only cgroup permission (#464): libdrm
    /// still lists the GPU's primary node, and every open fails, so the app can never take
    /// the display. Realized per engine by `dialect::card_grant`.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub mknod_only_cards: Vec<String>,
    /// `c <major>:<minor|*> <rwm>` only; lets a node created after start be opened.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub device_cgroup_rules: Vec<String>,
    pub group_add: Vec<String>,
    pub network: String,
    pub gpu: bool,
    pub nvidia_gpu: bool,
    /// Narrow post-start repair for the NVIDIA `/proc` overmount. This remains
    /// an API exec under the same owned identity, never a CLI fallback.
    pub unmount_nvidia_params: bool,
    pub security: ApplicationSecurity,
}
impl Default for ApplicationRequest {
    fn default() -> Self {
        Self {
            operation: String::new(),
            name: String::new(),
            image: String::new(),
            pull_never: false,
            entrypoint: None,
            command: Vec::new(),
            environment: Vec::new(),
            mounts: Vec::new(),
            typed_mounts: Vec::new(),
            devices: Vec::new(),
            mknod_only_cards: Vec::new(),
            device_cgroup_rules: Vec::new(),
            group_add: Vec::new(),
            network: "none".into(),
            gpu: false,
            nvidia_gpu: false,
            unmount_nvidia_params: false,
            security: ApplicationSecurity::default(),
        }
    }
}
impl ApplicationRequest {
    pub fn is_valid(&self) -> bool {
        !self.operation.is_empty()
            && self.operation.len() <= 256
            && self.name.starts_with("quasar-sess-")
            && self.name.len() <= 255
            && !self.image.is_empty()
            && self.image.len() <= 2048
            && !self.network.is_empty()
            && self.security.pids_limit > 0
            && self.security.shm_size > 0
            && self
                .environment
                .iter()
                .all(|entry| entry.contains('=') && !entry.contains('\0'))
            && self
                .mounts
                .iter()
                .all(|mount| !mount.is_empty() && !mount.contains('\0'))
            && mknod_only_cards_ok(&self.mknod_only_cards, &self.devices)
            && self
                .device_cgroup_rules
                .iter()
                .all(|rule| valid_device_cgroup_rule(rule))
            && self.typed_mounts.iter().all(|mount| match mount {
                ApplicationMount::Bind { source, target, .. }
                | ApplicationMount::Volume { source, target, .. } => {
                    !source.is_empty()
                        && !target.is_empty()
                        && !source.contains('\0')
                        && !target.contains('\0')
                }
            })
    }
}

/// Each mknod-only card is `/dev/dri/card<N>` exactly, and the same request never also
/// grants it openable, by its own path or through the whole `/dev/dri`.
pub(crate) fn mknod_only_cards_ok(cards: &[String], devices: &[String]) -> bool {
    let card_node = |path: &str| {
        path.strip_prefix("/dev/dri/card")
            .is_some_and(|n| !n.is_empty() && n.len() <= 4 && n.bytes().all(|b| b.is_ascii_digit()))
    };
    cards.is_empty()
        || (cards.iter().all(|card| card_node(card))
            && !devices
                .iter()
                .any(|d| d == "/dev/dri" || d == "/dev/dri/" || cards.contains(d)))
}

/// A character-device rule with numeric major, numeric-or-`*` minor and a non-empty
/// subset of `rwm`. Block devices and the `a` wildcard are refused.
fn valid_device_cgroup_rule(rule: &str) -> bool {
    let mut parts = rule.split(' ');
    let (Some("c"), Some(numbers), Some(access), None) =
        (parts.next(), parts.next(), parts.next(), parts.next())
    else {
        return false;
    };
    let Some((major, minor)) = numbers.split_once(':') else {
        return false;
    };
    let number = |s: &str| !s.is_empty() && s.len() <= 10 && s.bytes().all(|b| b.is_ascii_digit());
    number(major)
        && (minor == "*" || number(minor))
        && !access.is_empty()
        && access.len() <= 3
        && access.bytes().all(|b| matches!(b, b'r' | b'w' | b'm'))
}

/// Terminal evidence retained before a container is removed.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct ApplicationResult {
    pub exit_code: Option<i64>,
    pub oom_killed: Option<bool>,
    pub stdout: String,
    pub stderr: String,
}

/// A bounded read-only snapshot for readiness diagnostics while an application
/// is still running. It deliberately carries no inferred exit status.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApplicationLogTail {
    pub stdout: String,
    pub stderr: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub(crate) enum ApplicationPhase {
    Creating,
    Created,
    Starting,
    Running,
    Stopping,
    Stopped,
    CleanupPending,
    Completed,
}
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub(crate) struct ImageVolumeIdentity {
    pub target: String,
    pub name: Option<String>,
    pub source: Option<String>,
}
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub(crate) struct NvidiaParamsRepair {
    pub attempted: bool,
    pub exec_id: Option<String>,
    pub start_attempted: bool,
    pub completed: bool,
    pub outcome: Option<String>,
}
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub(crate) struct ApplicationIntent {
    pub request: ApplicationRequest,
    pub owner: String,
    pub socket: std::path::PathBuf,
    pub id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image_entrypoint: Option<Vec<String>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image_cmd: Option<Vec<String>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image_user: Option<String>,
    /// Image-declared anonymous-volume destinations. Docker realizes these as
    /// additional mounts unless an explicit request overrides the target.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image_volumes: Option<Vec<String>>,
    /// Anonymous volume identities learned from inspect. The first inspection
    /// may accept an image-declared target without a pre-known source; every
    /// later inspection binds that target to this recorded realization.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image_volume_identities: Option<Vec<ImageVolumeIdentity>>,
    /// Durable best-effort state for the narrowly scoped NVIDIA proc repair.
    /// It prevents a lost exec reply from authorizing a fresh exec identity.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub nvidia_params_repair: Option<NvidiaParamsRepair>,
    /// How the NVIDIA GPU was requested at create; the read-back judges against this.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub gpu_injection: Option<crate::runtime::GpuInjection>,
    /// #413: the `NVIDIA_DRIVER_CAPABILITIES` a `--gpus` container was created with, so
    /// the read-back judges against it. `None` for CDI and for a journal from before #413.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub nvidia_driver_capabilities: Option<String>,
    /// RH-07 D14 (#404): on rootless Podman the app's `(uid, gid)` is mapped onto the Quasar
    /// user (`keep-id`), so home files on the host belong to it.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub keep_id: Option<(u32, u32)>,
    /// #428: gids the image adds to the app user itself, as `QUASAR_APP_ENGINE_GROUPS`
    /// (`docs/configuration.md`). A `--group-add` cannot: the image's drop re-derives groups.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub engine_groups: Vec<u32>,
    /// The container's supplementary groups when the engine cannot map some the request
    /// named; `None` is the request's own list.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub group_add: Option<Vec<String>>,
    /// #460: the container's device-cgroup rules when the engine cannot apply the
    /// request's (a rootless engine: none); `None` is the request's own list.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub device_cgroup_rules: Option<Vec<String>>,
    /// #464: the engine cannot hold the request's `mknod_only_cards` to mknod
    /// (`dialect::card_grant`), so they were created openable.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub cards_openable: bool,
    /// On an engine that confines with SELinux the app runs as the nested-sandbox type
    /// (`dialect::NESTED_SANDBOX_LABEL`).
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub nested_sandbox_label: bool,
    pub phase: ApplicationPhase,
    pub result: Option<ApplicationResult>,
}

/// A separate journal namespace prevents an application recovery from being
/// mistaken for a short-lived helper and vice versa.
pub(crate) struct ApplicationJournal {
    path: std::path::PathBuf,
    _lease: std::fs::File,
}
impl ApplicationJournal {
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
            .join("applications");
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
            // SAFETY: this file remains open for the journal lifetime.
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
    pub fn read(&self) -> Result<Option<ApplicationIntent>, super::RuntimeError> {
        use std::{io::Read, os::unix::fs::OpenOptionsExt};
        match std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW)
            .open(&self.path)
        {
            Ok(file) => {
                let mut bytes = Vec::new();
                file.take((64 * 1024 + 1) as u64)
                    .read_to_end(&mut bytes)
                    .map_err(|_| super::ErrorKind::Unavailable)?;
                if bytes.len() > 64 * 1024 {
                    return Err(super::ErrorKind::Protocol.into());
                }
                serde_json::from_slice(&bytes)
                    .map(Some)
                    .map_err(|_| super::ErrorKind::Protocol.into())
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(_) => Err(super::ErrorKind::Unavailable.into()),
        }
    }
    pub fn write(&self, intent: &ApplicationIntent) -> Result<(), super::RuntimeError> {
        use std::{io::Write, os::unix::fs::OpenOptionsExt};
        let bytes = serde_json::to_vec(intent).map_err(|_| super::ErrorKind::Protocol)?;
        if bytes.len() > 64 * 1024 {
            return Err(super::ErrorKind::Protocol.into());
        }
        let temp = self.path.with_extension("new");
        let _ = std::fs::remove_file(&temp);
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
        std::fs::rename(temp, &self.path).map_err(|_| super::ErrorKind::Unavailable)?;
        std::fs::File::open(self.path.parent().ok_or(super::ErrorKind::Protocol)?)
            .and_then(|f| f.sync_all())
            .map_err(|_| super::ErrorKind::Unavailable.into())
    }
}

#[cfg(test)]
mod device_cgroup_rule_tests {
    use super::valid_device_cgroup_rule;

    #[test]
    fn only_character_device_rules_are_valid() {
        for ok in ["c 13:* rwm", "c 116:* rwm", "c 116:3 rw", "c 226:0 r"] {
            assert!(valid_device_cgroup_rule(ok), "{ok}");
        }
        for bad in [
            "a *:* rwm",
            "b 8:* rwm",
            "c *:* rwm",
            "c 13:* rwx",
            "c 13 rwm",
            "c 13:* ",
            "c 13:*  rwm",
            "c 13:* rwm extra",
            "",
        ] {
            assert!(!valid_device_cgroup_rule(bad), "{bad:?}");
        }
    }
}
