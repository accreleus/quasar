//! Platform-service lifecycles (RH-06): the mutating typed calls a recovery actor makes
//! to create, start, stop, rename and remove Quasar's own long-running containers, pull
//! their images by digest, and manage the named volumes they mount.
//!
//! They are not the agent's application/helper lifecycles, which are bound to the agent's
//! own journals and ownership label. A platform service's owner is the
//! recovery actor, whose journal lives in its machine state; everything here is a
//! single, bounded engine call and nothing more. Every call runs on the
//! [`RuntimeClient`]'s executor under its admission and a deadline; a mutation is
//! detached, so a spent budget reads [`ErrorKind::UnknownOutcome`], never "it failed".
//!
//! Daemon text reaches a caller only as a [`Refused`] create or start, which the recovery
//! actor matches (a device request the engine cannot meet) and logs.

use crate::{docker, ErrorKind, Operation, RuntimeClient, RuntimeError};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::time::Duration;

/// How long a platform image pull may take before its outcome is unknown.
pub const PULL_BUDGET: Duration = Duration::from_secs(30 * 60);

/// The whole shape of one platform-service container, as the engine is asked to create
/// it. Ordered maps and vectors make its JSON canonical, so a hash of it identifies the
/// shape (the `io.quasar.spec` label).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ContainerSpec {
    pub name: String,
    /// `repository@sha256:<digest>`.
    pub image: String,
    /// `None` keeps the image's own entrypoint.
    pub entrypoint: Option<Vec<String>>,
    /// `None` keeps the image's own command.
    pub cmd: Option<Vec<String>>,
    pub env: BTreeMap<String, String>,
    pub labels: BTreeMap<String, String>,
    /// `None` is the engine's default network.
    pub network_mode: Option<String>,
    pub binds: Vec<Bind>,
    pub devices: Vec<Device>,
    pub device_cgroup_rules: Vec<String>,
    /// Engine device requests: `[{count: -1, capabilities: [["gpu"]]}]` is `--gpus all`.
    pub gpus: Vec<GpuRequest>,
    pub cap_add: Vec<String>,
    pub security_opt: Vec<String>,
    pub init: bool,
    pub restart: RestartPolicy,
}

/// One bind: `source` is an absolute daemon-host path, or the name of a named volume.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Bind {
    pub source: String,
    pub target: String,
    pub read_only: bool,
}

impl Bind {
    /// The Engine API `HostConfig.Binds` form (API 1.40: no mount sub-paths needed).
    pub fn to_engine(&self) -> String {
        if self.read_only {
            format!("{}:{}:ro", self.source, self.target)
        } else {
            format!("{}:{}", self.source, self.target)
        }
    }

    /// Whether `source` names a volume rather than a host path.
    pub fn is_volume(&self) -> bool {
        !self.source.starts_with('/')
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Device {
    pub host: String,
    pub container: String,
    /// cgroup permissions, `rwm` unless narrower.
    pub permissions: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GpuRequest {
    /// `None` lets the engine pick the driver for the capability, as `--gpus` does.
    pub driver: Option<String>,
    pub count: i64,
    pub capabilities: Vec<Vec<String>>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum RestartPolicy {
    No,
    UnlessStopped,
}

/// The engine refused a create or start, with its HTTP status and its own message
/// (bounded to 1 KiB).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Refused {
    pub status: u16,
    pub message: String,
}

/// One container as the engine reports it now.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PlatformContainer {
    pub id: String,
    /// Without the engine's leading `/`.
    pub name: String,
    /// The configured image reference.
    pub image: String,
    pub image_id: String,
    pub labels: BTreeMap<String, String>,
    /// The engine's state word: `created`, `running`, `exited`, ...
    pub status: String,
    pub running: bool,
    /// `None` for a container with no healthcheck.
    pub health: Option<String>,
    pub restart: Option<RestartPolicy>,
    /// Every mount, as `(source or volume name, destination, read_only)`.
    pub mounts: Vec<(String, String, bool)>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PlatformImage {
    pub id: String,
    /// `repository@sha256:...` references this image is known by.
    pub repo_digests: Vec<String>,
    pub labels: BTreeMap<String, String>,
}

/// The engine's statements about its host that a recipe's inputs depend on.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct EngineHost {
    /// The engine host's name (`/info` `Name`, the machine's hostname).
    pub name: Option<String>,
    /// Configured OCI runtimes by name, sorted.
    pub runtimes: Vec<String>,
    /// CDI devices the engine discovered, by id (`nvidia.com/gpu=0`, ...).
    pub cdi_devices: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PlatformVolume {
    pub name: String,
    pub labels: BTreeMap<String, String>,
}

impl RuntimeClient {
    /// One bounded `/info`, reduced to [`EngineHost`].
    pub fn engine_host(&self) -> Operation<EngineHost> {
        let config = self.config().clone();
        self.submit(async move { docker::platform::engine_host(&config).await })
    }

    /// Pull `reference` (a digest reference, in practice). Registry credentials come from
    /// the configured Docker CLI config, as the agent's own pulls.
    pub fn pull_image(&self, reference: impl Into<String>) -> Operation<()> {
        let config = self.config().clone();
        let reference = reference.into();
        self.submit_owned(
            async move { docker::platform::pull(&config, &reference).await },
            PULL_BUDGET,
            true,
        )
    }

    /// `Ok(None)` is a conclusively missing image.
    pub fn inspect_platform_image(
        &self,
        reference: impl Into<String>,
    ) -> Operation<Option<PlatformImage>> {
        let config = self.config().clone();
        let reference = reference.into();
        self.submit(async move { docker::platform::inspect_image(&config, &reference).await })
    }

    /// `Ok(None)` is a conclusively missing container.
    pub fn inspect_platform_container(
        &self,
        name_or_id: impl Into<String>,
    ) -> Operation<Option<PlatformContainer>> {
        let config = self.config().clone();
        let name = name_or_id.into();
        self.submit(async move { docker::platform::inspect(&config, &name).await })
    }

    /// Every container on the engine, running or not, each re-inspected.
    pub fn platform_containers(&self) -> Operation<Vec<PlatformContainer>> {
        let config = self.config().clone();
        self.submit(async move { docker::platform::list(&config).await })
    }

    /// Create (never start) `spec`. The new container's id, or the engine's refusal.
    pub fn create_container(&self, spec: ContainerSpec) -> Operation<Result<String, Refused>> {
        let config = self.config().clone();
        let budget = self.deadline();
        self.submit_owned(
            async move { docker::platform::create(&config, &spec).await },
            budget,
            true,
        )
    }

    /// Start; an already running container is not an error. `Ok(Err(_))` is the
    /// engine's refusal.
    pub fn start_container(&self, id: impl Into<String>) -> Operation<Result<(), Refused>> {
        let config = self.config().clone();
        let id = id.into();
        let budget = self.deadline();
        self.submit_owned(
            async move { docker::platform::start(&config, &id).await },
            budget,
            true,
        )
    }

    /// Stop with a grace period; an already stopped container is not an error.
    pub fn stop_container(&self, id: impl Into<String>, grace: Duration) -> Operation<()> {
        let config = self.config().clone();
        let id = id.into();
        let budget = self.deadline() + grace;
        self.submit_owned(
            async move { docker::platform::stop(&config, &id, grace).await },
            budget,
            true,
        )
    }

    pub fn set_restart_policy(
        &self,
        id: impl Into<String>,
        policy: RestartPolicy,
    ) -> Operation<()> {
        let config = self.config().clone();
        let id = id.into();
        let budget = self.deadline();
        self.submit_owned(
            async move { docker::platform::update_restart(&config, &id, policy).await },
            budget,
            true,
        )
    }

    pub fn rename_container(
        &self,
        id: impl Into<String>,
        name: impl Into<String>,
    ) -> Operation<()> {
        let config = self.config().clone();
        let (id, name) = (id.into(), name.into());
        let budget = self.deadline();
        self.submit_owned(
            async move { docker::platform::rename(&config, &id, &name).await },
            budget,
            true,
        )
    }

    /// Remove (forced: a running container is stopped first). A missing container is not
    /// an error. Anonymous volumes go with it; named volumes never do.
    pub fn remove_container(&self, id: impl Into<String>) -> Operation<()> {
        let config = self.config().clone();
        let id = id.into();
        let budget = self.deadline();
        self.submit_owned(
            async move { docker::platform::remove(&config, &id).await },
            budget,
            true,
        )
    }

    /// Wait for the container to stop; its exit code. Budgeted by `timeout`, after which
    /// the wait is [`ErrorKind::Timeout`] (nothing was mutated by waiting).
    pub fn wait_container(&self, id: impl Into<String>, timeout: Duration) -> Operation<i64> {
        let config = self.config().clone();
        let id = id.into();
        self.submit_owned(
            async move { docker::platform::wait(&config, &id).await },
            timeout,
            false,
        )
    }

    /// The last `lines` of stdout and stderr, bounded to 64 KiB.
    pub fn container_logs_tail(&self, id: impl Into<String>, lines: usize) -> Operation<String> {
        let config = self.config().clone();
        let id = id.into();
        self.submit(async move { docker::platform::logs_tail(&config, &id, lines).await })
    }

    /// `PUT /containers/{id}/archive`: extract `tar` at `path` inside the container,
    /// which for a created, never started container writes into the volume mounted there.
    pub fn upload_archive(
        &self,
        id: impl Into<String>,
        path: impl Into<String>,
        tar: Vec<u8>,
    ) -> Operation<()> {
        let config = self.config().clone();
        let (id, path) = (id.into(), path.into());
        let budget = self.deadline();
        self.submit_owned(
            async move { docker::platform::upload(&config, &id, &path, tar).await },
            budget,
            true,
        )
    }

    /// `Ok(None)` is a conclusively missing volume.
    pub fn inspect_volume(&self, name: impl Into<String>) -> Operation<Option<PlatformVolume>> {
        let config = self.config().clone();
        let name = name.into();
        self.submit(async move { docker::platform::inspect_volume(&config, &name).await })
    }

    pub fn create_volume(
        &self,
        name: impl Into<String>,
        labels: BTreeMap<String, String>,
    ) -> Operation<PlatformVolume> {
        let config = self.config().clone();
        let name = name.into();
        let budget = self.deadline();
        self.submit_owned(
            async move { docker::platform::create_volume(&config, &name, labels).await },
            budget,
            true,
        )
    }

    /// A missing volume is not an error; a volume in use is [`ErrorKind::Busy`].
    pub fn remove_volume(&self, name: impl Into<String>) -> Operation<()> {
        let config = self.config().clone();
        let name = name.into();
        let budget = self.deadline();
        self.submit_owned(
            async move { docker::platform::remove_volume(&config, &name).await },
            budget,
            true,
        )
    }
}

/// Not a daemon message: a stable description a caller may log.
pub fn describe(error: &RuntimeError) -> &'static str {
    match error.kind {
        ErrorKind::InvalidConfiguration => "the engine endpoint is misconfigured",
        ErrorKind::PermissionDenied => "the engine socket refused this process",
        ErrorKind::Missing => "the object does not exist",
        ErrorKind::Unavailable => "the engine is unreachable",
        ErrorKind::IncompatibleApi => "the engine API is below the supported floor",
        ErrorKind::Protocol => "the engine answered something unreadable",
        ErrorKind::Engine => "the engine refused the request",
        ErrorKind::Timeout => "the engine did not answer in time",
        ErrorKind::Cancelled => "the operation was cancelled",
        ErrorKind::Busy => "the engine or the object is busy",
        ErrorKind::UnknownOutcome => "the outcome of the mutation is unknown",
        ErrorKind::ImageInUse => "the image is in use",
        ErrorKind::RegistryDenied => "the registry refused the pull",
        ErrorKind::ManifestMissing => "the registry has no such image",
        ErrorKind::InsufficientDisk => "the engine is out of disk space",
        ErrorKind::InvalidBuildContext => "the build context is invalid",
        ErrorKind::BuildFailed => "the build failed",
    }
}
