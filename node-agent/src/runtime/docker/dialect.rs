//! How each container engine reports what it created (RH-07 #397).
//!
//! The runtime re-inspects every container it creates and refuses anything it did not ask
//! for. Docker echoes a request back exactly, so its read-back is a plain comparison.
//! Podman's Docker-compatible inspect does not (measured on Podman 5.8.4, rootful and
//! rootless; the findings are on #397 and in `docs/rh07/2026-09-28-cdi-spike.md`):
//!
//! - capabilities are reported as a delta from Podman's own default set, so the exact set
//!   comes from Podman's native inspect (`EffectiveCaps`) instead;
//! - the runtime reads `oci` (the native inspect names it: `crun` or `runc`);
//! - namespaces Quasar left alone read `private` (and IPC `shareable`), not empty;
//! - `no-new-privileges` has no `:true`;
//! - requested devices and device requests are not reported (rootless never lists a plain
//!   device; CDI-expanded devices appear only once started, with empty permissions);
//! - named volumes sit in `Binds` with extra options, and `HostConfig.Mounts` is empty;
//! - `Config.Image` names the image even when it was created by ID.
//!
//! This module is the one place those differences live: each function answers "is what
//! the engine reported exactly what Quasar asked for?" per engine. The rule is the same on
//! every engine: anything reported that Quasar did not ask for is a refusal. What Podman
//! does not report at all is "not reported", never taken as granted; whether a requested
//! device really arrived is proven by the session's own function checks. Nothing here ever
//! relaxes rootful Docker.

use std::ops::Deref;
use std::path::{Path, PathBuf};
use std::time::Duration;

use bollard::models::{DeviceMapping, DeviceRequest};

use super::{RuntimeConfig, RuntimeError};
use crate::runtime::{EngineKind, ErrorKind, GpuInjection};

/// Which engine's reporting rules apply. An engine this agent cannot name gets Docker's,
/// the strictest.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Dialect {
    Docker,
    Podman,
}

impl Dialect {
    pub(crate) fn of(kind: EngineKind) -> Self {
        match kind {
            EngineKind::Podman => Dialect::Podman,
            EngineKind::Docker | EngineKind::Unknown => Dialect::Docker,
        }
    }
}

/// An engine connection plus what its read-back needs to know about it. Derefs to the
/// bollard client, so every existing call keeps working.
pub(crate) struct Engine {
    docker: bollard::Docker,
    pub(crate) dialect: Dialect,
    kind: EngineKind,
    socket: PathBuf,
    deadline: Duration,
}

impl Deref for Engine {
    type Target = bollard::Docker;
    fn deref(&self) -> &bollard::Docker {
        &self.docker
    }
}

pub(crate) async fn open(config: &RuntimeConfig) -> Result<Engine, RuntimeError> {
    let (docker, info) = quasar_runtime::docker::discover(config).await?;
    Ok(Engine {
        docker,
        dialect: Dialect::of(info.kind),
        kind: info.kind,
        socket: config.socket.clone(),
        deadline: config.deadline,
    })
}

/// What Podman's native inspect states exactly, where its compatible inspect does not.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub(crate) struct PodmanFacts {
    /// `None` when the inspect does not carry the field at all (it cannot be proven);
    /// Podman writes `null` for "no capabilities", which is the empty set.
    pub effective_caps: Option<Vec<String>>,
    /// The bounding set, under the same rule: a drop-all that was never applied could
    /// leave the effective set narrow and the bounding set wide.
    pub bounding_caps: Option<Vec<String>>,
    pub oci_runtime: Option<String>,
    /// Each realized mount's propagation, which the compatible inspect does not report.
    pub mount_propagations: Vec<String>,
    /// The user namespace's maps as `container:parent:length`; Podman reports a keep-id
    /// container's `UsernsMode` only as `private`, so these are the proof of the mapping.
    pub uid_map: Vec<String>,
    pub gid_map: Vec<String>,
}

impl PodmanFacts {
    pub(crate) fn from_inspect(body: &str) -> Result<Self, RuntimeError> {
        let value: serde_json::Value =
            serde_json::from_str(body).map_err(|_| RuntimeError::from(ErrorKind::Protocol))?;
        let caps = |key: &str| -> Result<Option<Vec<String>>, RuntimeError> {
            match value.get(key) {
                None => Ok(None),
                Some(serde_json::Value::Null) => Ok(Some(Vec::new())),
                Some(caps) => serde_json::from_value(caps.clone())
                    .map(Some)
                    .map_err(|_| RuntimeError::from(ErrorKind::Protocol)),
            }
        };
        let effective_caps = caps("EffectiveCaps")?;
        let bounding_caps = caps("BoundingCaps")?;
        let mount_propagations = value
            .get("Mounts")
            .and_then(|m| m.as_array())
            .map(|mounts| {
                mounts
                    .iter()
                    .map(|m| {
                        m.get("Propagation")
                            .and_then(|p| p.as_str())
                            .unwrap_or("")
                            .to_string()
                    })
                    .collect()
            })
            .unwrap_or_default();
        let oci_runtime = value
            .get("OCIRuntime")
            .and_then(|v| v.as_str())
            .map(str::to_string);
        let map = |key: &str| -> Vec<String> {
            value
                .pointer(&format!("/HostConfig/IDMappings/{key}"))
                .and_then(|m| m.as_array())
                .map(|m| {
                    m.iter()
                        .filter_map(|e| e.as_str().map(str::to_string))
                        .collect()
                })
                .unwrap_or_default()
        };
        Ok(Self {
            effective_caps,
            bounding_caps,
            oci_runtime,
            mount_propagations,
            uid_map: map("UidMap"),
            gid_map: map("GidMap"),
        })
    }
}

/// The keep-id mapping `(uid, gid)` proven by Podman's maps: the one range that maps onto
/// the engine's own user (parent id 0) is exactly `uid` (and `gid`), one id long.
pub(crate) fn keep_id_ok(podman: Option<&PodmanFacts>, uid: u32, gid: u32) -> bool {
    let onto_owner = |map: &[String]| -> Vec<String> {
        map.iter()
            .filter(|e| {
                let parts: Vec<&str> = e.split(':').collect();
                parts.len() == 3 && parts[1] == "0"
            })
            .cloned()
            .collect()
    };
    podman.is_some_and(|f| {
        onto_owner(&f.uid_map) == [format!("{uid}:0:1")]
            && onto_owner(&f.gid_map) == [format!("{gid}:0:1")]
    })
}

impl Engine {
    /// Whether this engine runs rootless, from its own `/info`.
    pub(crate) async fn rootless(&self) -> Result<bool, RuntimeError> {
        let sys = self.docker.info().await.map_err(super::classify)?;
        Ok(sys
            .security_options
            .iter()
            .flatten()
            .any(|o| o.split(',').any(|p| p == "name=rootless")))
    }

    /// How this engine injects an NVIDIA GPU now (decision D10). Asked per create, never
    /// cached: a CDI specification written after the agent started must be picked up.
    pub(crate) async fn gpu_injection(&self) -> Result<Option<GpuInjection>, RuntimeError> {
        let sys = self.docker.info().await.map_err(super::classify)?;
        Ok(quasar_runtime::docker::gpu_injection_from_info(
            self.kind, &sys,
        ))
    }

    /// Podman's native inspect of one container; `None` on Docker. A Podman that does not
    /// answer it is an unknown outcome: the read-back cannot be proven.
    pub(crate) async fn podman_facts(&self, id: &str) -> Result<Option<PodmanFacts>, RuntimeError> {
        if self.dialect != Dialect::Podman {
            return Ok(None);
        }
        let socket = self.socket.clone();
        let path = format!("/v4.0.0/libpod/containers/{id}/json");
        let deadline = self.deadline;
        let response = tokio::task::spawn_blocking(move || {
            crate::release::unix_http::request(&socket, "GET", &path, None, deadline)
        })
        .await
        .map_err(|_| RuntimeError::from(ErrorKind::UnknownOutcome))?
        .map_err(|_| RuntimeError::from(ErrorKind::UnknownOutcome))?;
        if response.status != 200 {
            return Err(ErrorKind::UnknownOutcome.into());
        }
        PodmanFacts::from_inspect(&response.body).map(Some)
    }
}

/// Namespaces Quasar never sets for an app or helper container.
#[derive(Debug, Clone, Copy)]
pub(crate) enum Namespace {
    Pid,
    Ipc,
    Uts,
}

impl Dialect {
    /// Is `mode` what this engine reports for a namespace Quasar left at its default? The
    /// host namespace, or sharing another container's, is never the default.
    pub(crate) fn default_namespace(self, ns: Namespace, mode: Option<&str>) -> bool {
        let mode = mode.unwrap_or("");
        match (self, ns) {
            (Dialect::Docker, Namespace::Pid | Namespace::Ipc) => {
                mode.is_empty() || mode == "private"
            }
            (Dialect::Docker, Namespace::Uts) => mode.is_empty(),
            (Dialect::Podman, Namespace::Pid | Namespace::Uts) => {
                mode.is_empty() || mode == "private"
            }
            (Dialect::Podman, Namespace::Ipc) => {
                mode.is_empty() || mode == "private" || mode == "shareable"
            }
        }
    }

    /// Did the container get a runtime Quasar accepts? Docker: runc, or the engine default,
    /// or `nvidia` when a GPU was requested (today's rule). Podman: its native inspect must
    /// name crun or runc; the compatible field only ever says `oci`.
    pub(crate) fn runtime_ok(
        self,
        reported: Option<&str>,
        podman: Option<&PodmanFacts>,
        nvidia_requested: bool,
    ) -> bool {
        let reported = reported.unwrap_or("");
        match self {
            Dialect::Docker => {
                reported.is_empty()
                    || reported == "runc"
                    || (nvidia_requested && reported == "nvidia")
            }
            Dialect::Podman => {
                (reported.is_empty()
                    || reported == "oci"
                    || reported == "crun"
                    || reported == "runc")
                    && podman
                        .and_then(|f| f.oci_runtime.as_deref())
                        .is_some_and(|runtime| runtime == "crun" || runtime == "runc")
            }
        }
    }

    /// The security options as a comparable set. Podman writes `no-new-privileges` for
    /// Docker's `no-new-privileges:true`; the two mean the same.
    pub(crate) fn security_options(self, values: Option<&Vec<String>>) -> Vec<String> {
        let mut values: Vec<String> = values
            .cloned()
            .unwrap_or_default()
            .into_iter()
            .map(|value| match (self, value.as_str()) {
                (Dialect::Podman, "no-new-privileges") => "no-new-privileges:true".into(),
                _ => value,
            })
            .collect();
        values.sort();
        values
    }

    /// Are the container's capabilities exactly what Quasar asked for? Docker echoes the
    /// request (`CapDrop: [ALL]` plus the additions). Podman reports a delta from its own
    /// default set, so its native `EffectiveCaps` must equal the requested set exactly.
    pub(crate) fn capabilities_ok(
        self,
        reported_add: Option<&Vec<String>>,
        reported_drop: Option<&Vec<String>>,
        podman: Option<&PodmanFacts>,
        requested_add: &[String],
        drop_all: bool,
    ) -> bool {
        let canonical = |values: &[String]| {
            let mut values: Vec<String> = values
                .iter()
                .map(|value| value.trim_start_matches("CAP_").to_ascii_uppercase())
                .collect();
            values.sort();
            values.dedup();
            values
        };
        let requested = canonical(requested_add);
        let compat_echoes = || {
            canonical(reported_add.map(Vec::as_slice).unwrap_or_default()) == requested
                && canonical(reported_drop.map(Vec::as_slice).unwrap_or_default())
                    == if drop_all {
                        vec![String::from("ALL")]
                    } else {
                        Vec::new()
                    }
        };
        match self {
            Dialect::Docker => compat_echoes(),
            // Podman reports capabilities as a delta from its default set. With drop-all
            // that delta says nothing exact, so its native EffectiveCaps must equal the
            // request. Without drop-all, both engines grant their defaults plus the
            // additions, and the compatible delta means exactly what Docker's does.
            Dialect::Podman if drop_all => podman.is_some_and(|f| {
                [&f.effective_caps, &f.bounding_caps].iter().all(|set| {
                    set.as_deref()
                        .is_some_and(|caps| canonical(caps) == requested)
                })
            }),
            Dialect::Podman => compat_echoes(),
        }
    }

    /// Is every reported device one Quasar asked for? Docker reports each requested device
    /// in order with `rwm`, and nothing else. Podman may leave a requested device out and
    /// reports permissions empty; what it does list must each be requested, or be a node
    /// a requested GPU expands to (`/dev/nvidia*`, `/dev/dri/*`).
    pub(crate) fn devices_ok(
        self,
        reported: &[DeviceMapping],
        requested: &[String],
        gpu_requested: bool,
    ) -> bool {
        match self {
            Dialect::Docker => {
                reported.len() == requested.len()
                    && reported.iter().zip(requested).all(|(actual, wanted)| {
                        actual.path_on_host.as_deref() == Some(wanted.as_str())
                            && actual.path_in_container.as_deref() == Some(wanted.as_str())
                            && actual.cgroup_permissions.as_deref() == Some("rwm")
                    })
            }
            Dialect::Podman => reported.iter().all(|actual| {
                let (Some(host), Some(inside)) = (
                    actual.path_on_host.as_deref(),
                    actual.path_in_container.as_deref(),
                ) else {
                    return false;
                };
                if !normal_absolute(host) {
                    return false;
                }
                let permissions = actual.cgroup_permissions.as_deref().unwrap_or("");
                // Podman expands a requested directory (`/dev/dri`) into its nodes, and a
                // CDI GPU into the nodes its specification lists.
                let under = |dir: &str| {
                    Path::new(host).parent() == Some(Path::new(dir)) && gpu_expansion(host)
                };
                host == inside
                    && (permissions.is_empty() || permissions == "rwm")
                    && (requested
                        .iter()
                        .any(|wanted| wanted == host || under(wanted))
                        || (gpu_requested && gpu_expansion(host)))
            }),
        }
    }

    /// Does the reported device-request list match? `wanted` says whether one request was
    /// made and how to recognise it. Docker echoes it exactly; Podman reports none at all,
    /// and must never report one Quasar did not make.
    pub(crate) fn device_requests_ok(
        self,
        reported: Option<&Vec<DeviceRequest>>,
        wanted: Option<&dyn Fn(&DeviceRequest) -> bool>,
    ) -> bool {
        let reported = reported.map(Vec::as_slice).unwrap_or_default();
        match (self, wanted) {
            (_, None) => reported.is_empty(),
            (Dialect::Docker, Some(matches)) => matches!(reported, [request] if matches(request)),
            (Dialect::Podman, Some(matches)) => match reported {
                [] => true,
                [request] => matches(request),
                _ => false,
            },
        }
    }

    /// Is every realized mount's propagation private? Docker's is checked through the
    /// echoed request (`HostConfig.Mounts`), so this adds nothing there. Podman does not
    /// echo it; its native inspect states it per mount, and a shared or slave mount is a
    /// grant Quasar never asks for.
    pub(crate) fn mount_propagation_ok(self, podman: Option<&PodmanFacts>) -> bool {
        match self {
            Dialect::Docker => true,
            Dialect::Podman => podman.is_some_and(|f| {
                f.mount_propagations
                    .iter()
                    .all(|p| matches!(p.as_str(), "" | "private" | "rprivate"))
            }),
        }
    }

    /// Whether `HostConfig.Binds` / `HostConfig.Mounts` echo the request. When they do
    /// not (Podman), the realized mount points, which every engine reports, are what is
    /// compared.
    pub(crate) fn echoes_mount_requests(self) -> bool {
        self == Dialect::Docker
    }

    /// Whether `Config.Image` echoes the reference the container was created from. Podman
    /// names the image instead; the image ID is still compared on every engine.
    pub(crate) fn echoes_config_image(self) -> bool {
        self == Dialect::Docker
    }

    /// Whether an empty `MaskedPaths`/`ReadonlyPaths` is reported for an unmasked
    /// container. Podman reports neither; that is not a grant Quasar did not ask for.
    pub(crate) fn reports_masked_paths(self) -> bool {
        self == Dialect::Docker
    }
}

/// An absolute path in normal form: every component after the root is a plain name, so
/// `..` or `.` can never walk out of the directory a pattern names.
fn normal_absolute(path: &str) -> bool {
    let mut components = Path::new(path).components();
    // `components()` quietly drops `.` and repeated slashes, so the path must also be
    // exactly its own reassembly: one spelling per node, nothing to argue about.
    matches!(components.next(), Some(std::path::Component::RootDir))
        && components.all(|c| matches!(c, std::path::Component::Normal(_)))
        && Path::new(path)
            .components()
            .collect::<PathBuf>()
            .as_os_str()
            == path
}

/// The device nodes a requested NVIDIA CDI device resolves to, exactly: the NVIDIA
/// control and GPU nodes, and DRM card and render nodes. Nothing else under `/dev`.
fn gpu_expansion(path: &str) -> bool {
    if !normal_absolute(path) {
        return false;
    }
    let numbered = |name: &str, prefix: &str| {
        name.strip_prefix(prefix)
            .is_some_and(|n| !n.is_empty() && n.bytes().all(|b| b.is_ascii_digit()))
    };
    let path = Path::new(path);
    let Some(name) = path.file_name().and_then(|n| n.to_str()) else {
        return false;
    };
    match path.parent().and_then(|p| p.to_str()) {
        Some("/dev/dri") => numbered(name, "card") || numbered(name, "renderD"),
        Some("/dev") => {
            numbered(name, "nvidia")
                || matches!(
                    name,
                    "nvidiactl" | "nvidia-uvm" | "nvidia-uvm-tools" | "nvidia-modeset"
                )
        }
        Some("/dev/nvidia-caps") => numbered(name, "nvidia-cap"),
        _ => false,
    }
}

/// The device request that asks for every NVIDIA GPU the way `injection` says. The
/// `--gpus` shape is (`driver: nvidia`, count -1, capability `gpu`).
pub(crate) fn nvidia_device_request(injection: GpuInjection) -> DeviceRequest {
    match injection {
        GpuInjection::Cdi => DeviceRequest {
            driver: Some("cdi".into()),
            device_ids: Some(vec![crate::runtime::NVIDIA_CDI_DEVICE.into()]),
            ..Default::default()
        },
        GpuInjection::DeviceRequest => DeviceRequest {
            driver: Some("nvidia".into()),
            count: Some(-1),
            capabilities: Some(vec![vec!["gpu".into()]]),
            ..Default::default()
        },
    }
}

/// The injection a container was created with. An NVIDIA intent journalled before it was
/// recorded was created with `--gpus`.
pub(crate) fn recorded_injection(
    nvidia: bool,
    recorded: Option<GpuInjection>,
) -> Option<GpuInjection> {
    nvidia.then(|| recorded.unwrap_or(GpuInjection::DeviceRequest))
}

/// Is `request` exactly the NVIDIA request for `injection`, as the engine echoes it back?
/// Docker echoes a CDI request with `Count: 0` and no capabilities; anything else, or any
/// other device, is not what Quasar asked for.
pub(crate) fn is_nvidia_request(injection: GpuInjection, request: &DeviceRequest) -> bool {
    let no_options = request.options.as_ref().is_none_or(|o| o.is_empty());
    match injection {
        GpuInjection::Cdi => {
            request.driver.as_deref() == Some("cdi")
                && request.device_ids.as_deref()
                    == Some(&[crate::runtime::NVIDIA_CDI_DEVICE.to_string()][..])
                && request.count.is_none_or(|c| c == 0)
                && request.capabilities.as_ref().is_none_or(|c| c.is_empty())
                && no_options
        }
        GpuInjection::DeviceRequest => {
            request.driver.as_deref() == Some("nvidia")
                && request.count == Some(-1)
                && request.device_ids.as_ref().is_none_or(Vec::is_empty)
                && request.capabilities.as_deref() == Some(&[vec!["gpu".to_owned()]])
                && no_options
        }
    }
}

#[cfg(test)]
#[path = "dialect_tests.rs"]
mod tests;
