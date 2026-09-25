//! Recipes (ADR 0008): the container shape of each platform-service role, compiled into
//! the recovery actor and keyed by a **recipe revision**. Pure: no engine, no files.
//!
//! A service's versioned specification is recipe revision + machine inputs + image digest
//! (`CONTEXT.md` "Machine inputs"). An image can only select among the shapes this module
//! carries; it can never widen what host access its own container gets.
//!
//! The node-agent recipe carries what `deploy/docker-compose.yml` and
//! `deploy/docker-compose.nvidia.yml` describe today, with the owned-install differences
//! listed (and tested) in `tests/recipe_compose_parity.rs`. Golden rendered
//! specifications live in `testdata/recovery/recipes/`.

pub mod revision;

use std::collections::{BTreeMap, BTreeSet};
use std::ops::RangeInclusive;

use serde::{Deserialize, Serialize};

pub use quasar_runtime::platform::{Bind, ContainerSpec, Device, GpuRequest, RestartPolicy};

/// Deterministic names of what the recovery actor creates (architecture §5.4).
pub mod names {
    pub const NODE_AGENT: &str = "quasar-node-agent";
    pub const RECOVERY_ACTOR: &str = "quasar-recovery";
    pub const CONTROL_PLANE: &str = "quasar-control-plane";
    pub const POSTGRES: &str = "quasar-postgres";
    /// The actor's machine state, mounted only into actors.
    pub const MACHINE_VOLUME: &str = "quasar-machine";
    /// Holds the agent socket; the actor mounts it read-write, the agent read-only.
    pub const AGENT_SOCKET_VOLUME: &str = "quasar-recovery-agent";
    /// The agent's identity and state (`NODE_SECRET_PATH` lives here).
    pub const AGENT_DATA_VOLUME: &str = "quasar-agent-data";
    pub const NVIDIA_DRIVER_VOLUME: &str = "quasar-nvidia-driver";
    /// The node agent's per-service secrets volume, written by the actor, mounted
    /// read-only into that one container.
    pub const NODE_AGENT_SECRETS_VOLUME: &str = "quasar-node-agent-secrets";
    /// The disposable GPU probe; always removed after its run.
    pub const GPU_PROBE: &str = "quasar-gpu-probe";
    /// The never-started helper through which a secrets volume is written.
    pub const SECRETS_WRITER: &str = "quasar-secrets-writer";
}

/// Labels (architecture §5.4, ADR 0007/0008).
pub mod labels {
    pub const INSTALLATION: &str = "io.quasar.installation";
    pub const PLATFORM_SERVICE: &str = "io.quasar.platform-service";
    pub const RECIPE: &str = "io.quasar.recipe";
    pub const SPEC: &str = "io.quasar.spec";
    /// On a disposable helper (probe, secrets writer): what it is for.
    pub const HELPER: &str = "io.quasar.helper";
    /// The image label naming the recipe revision an image needs (ADR 0008).
    pub const IMAGE_RECIPE: &str = "org.quasar.recipe";
    /// The recovery image's release version (`deploy/Dockerfile.recovery`): how an actor
    /// tells which version a seed runs.
    pub const IMAGE_VERSION: &str = "org.quasar.version";
}

/// Fixed paths inside the containers the recipes describe.
pub mod paths {
    /// The machine-state volume inside an actor.
    pub const MACHINE_DIR: &str = "/var/lib/quasar-machine";
    pub use quasar_runtime::owned_install::{AGENT_SOCKET, AGENT_SOCKET_DIR, SECRETS_DIR};
    /// The engine socket inside every container that is given one.
    pub const ENGINE_SOCKET: &str = "/var/run/docker.sock";
    /// The fixed runtime directory the agent shares, same path, with its sessions.
    pub const AGENT_RUNTIME_DIR: &str = "/run/quasar-agent";
    pub const NVIDIA_DRIVER_DIR: &str = "/opt/quasar/nvidia-driver";
}

/// The secret files a recipe knows how to consume.
pub mod secrets {
    /// The operator's enrollment string (`qenr1.…`), for the agent to redeem.
    pub const ENROLLMENT: &str = "enrollment";
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum Role {
    ControlPlane,
    NodeAgent,
    Postgres,
    RecoveryActor,
}

impl Role {
    pub fn as_str(self) -> &'static str {
        match self {
            Role::ControlPlane => "control-plane",
            Role::NodeAgent => "node-agent",
            Role::Postgres => "postgres",
            Role::RecoveryActor => "recovery-actor",
        }
    }

    pub fn parse(s: &str) -> Option<Role> {
        [
            Role::ControlPlane,
            Role::NodeAgent,
            Role::Postgres,
            Role::RecoveryActor,
        ]
        .into_iter()
        .find(|r| r.as_str() == s)
    }

    /// The container name the actor gives this role.
    pub fn container_name(self) -> &'static str {
        match self {
            Role::ControlPlane => names::CONTROL_PLANE,
            Role::NodeAgent => names::NODE_AGENT,
            Role::Postgres => names::POSTGRES,
            Role::RecoveryActor => names::RECOVERY_ACTOR,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GpuVendor {
    Nvidia,
    Amd,
    Intel,
}

/// What the disposable probe learned about the machine's GPU (architecture §5.3 "GPU facts
/// detected by a disposable probe").
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GpuFacts {
    /// The preferred GPU: NVIDIA when the machine has an NVIDIA render node. `None`: no
    /// render node of a known vendor; the agent is installed anyway and its readiness
    /// reports the gap.
    pub vendor: Option<GpuVendor>,
    /// The render node of `vendor`, from the node's own device evidence.
    pub render_node: Option<String>,
    /// The engine started a probe container that requested `--gpus all`. Recorded only
    /// when true: a "no" is decided again whenever the agent is created, so an engine that
    /// gains the NVIDIA toolkit later is not held to an old answer.
    #[serde(default, alias = "nvidia_runtime", skip_serializing_if = "is_false")]
    pub gpus_served: bool,
    /// On an NVIDIA machine, the lowest other recognised render node: what the agent uses
    /// when the engine does not serve `--gpus`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fallback: Option<GpuNode>,
}

fn is_false(b: &bool) -> bool {
    !*b
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GpuNode {
    pub vendor: GpuVendor,
    pub render_node: String,
}

impl GpuFacts {
    /// Whether the NVIDIA shape (the Compose NVIDIA overlay) applies.
    pub fn nvidia_shape(&self) -> bool {
        self.vendor == Some(GpuVendor::Nvidia) && self.gpus_served
    }

    /// The render node the agent is pointed at.
    pub fn effective_render_node(&self) -> Option<&str> {
        match &self.fallback {
            Some(other) if self.vendor == Some(GpuVendor::Nvidia) && !self.gpus_served => {
                Some(&other.render_node)
            }
            _ => self.render_node.as_deref(),
        }
    }
}

/// Optional host device nodes. The engine refuses to create a container that names a
/// device the host lacks, and system containers often lack some of these.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HostDevices {
    pub dri: bool,
    pub uinput: bool,
    pub kmsg: bool,
}

impl Default for HostDevices {
    fn default() -> Self {
        HostDevices {
            dri: true,
            uinput: true,
            kmsg: true,
        }
    }
}

/// The machine inputs a recipe is rendered with. Only ever grows, each with a default.
///
/// No control URL: on a GPU host the agent takes the control plane's URL and pin from the
/// enrollment string, so there is nothing to override. The combined install (#361) adds it
/// for the local agent.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Inputs {
    pub installation_id: String,
    pub node_name: String,
    /// Host path of the homes root, bound at the same path.
    pub home_root: String,
    /// Host path of the templates root, bound at the same path.
    #[serde(default = "default_template_root")]
    pub template_root: String,
    /// Daemon-host path of the engine socket.
    #[serde(default = "default_docker_socket")]
    pub docker_socket: String,
    #[serde(default)]
    pub gpu: GpuFacts,
    #[serde(default)]
    pub devices: HostDevices,
}

pub fn default_template_root() -> String {
    "/var/lib/quasar/templates".into()
}

pub fn default_docker_socket() -> String {
    paths::ENGINE_SOCKET.into()
}

/// A platform image, always pinned by digest (ADR 0001).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ImageRef {
    pub repository: String,
    pub digest: String,
}

impl ImageRef {
    /// Parses `repository@sha256:<64 hex>`. A tag, or a digest of the wrong shape, is
    /// refused: a mutable tag would let the image change under the same specification.
    pub fn parse(reference: &str) -> Result<ImageRef, RenderError> {
        let (repository, digest) = reference.trim().split_once('@').ok_or_else(|| {
            RenderError::Invalid(format!(
                "{reference:?} is not pinned by digest (repository@sha256:…)"
            ))
        })?;
        let hex = digest.strip_prefix("sha256:").unwrap_or("");
        let last = repository.rsplit('/').next().unwrap_or("");
        if repository.is_empty()
            || last.contains(':')
            || hex.len() != 64
            || !hex
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        {
            return Err(RenderError::Invalid(format!(
                "{reference:?} is not repository@sha256:<64 lowercase hex>"
            )));
        }
        Ok(ImageRef {
            repository: repository.into(),
            digest: digest.into(),
        })
    }

    pub fn reference(&self) -> String {
        format!("{}@{}", self.repository, self.digest)
    }
}

/// The per-service secrets volume a container gets, and which secret files are in it.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct SecretMounts {
    pub volume: Option<String>,
    pub files: BTreeSet<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RenderError {
    /// The book does not carry this role at this revision (`recipe_unsupported`).
    Unsupported { role: Role, revision: u32 },
    /// The inputs cannot produce a safe container.
    Invalid(String),
}

impl std::fmt::Display for RenderError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RenderError::Unsupported { role, revision } => write!(
                f,
                "recipe_unsupported: this recovery actor carries no {} recipe at revision {revision}",
                role.as_str()
            ),
            RenderError::Invalid(why) => write!(f, "invalid machine inputs: {why}"),
        }
    }
}

impl std::error::Error for RenderError {}

/// The recipe book: which revisions of which roles this actor can render.
pub struct Book;

impl Book {
    /// Every revision this actor renders for `role`, from the floor up to its own (Rule B).
    /// `None`: this actor carries no recipe for the role yet (the control plane and
    /// Postgres arrive with the combined install).
    pub fn window(role: Role) -> Option<RangeInclusive<u32>> {
        match role {
            Role::NodeAgent => Some(1..=1),
            Role::RecoveryActor => Some(1..=revision::RECIPE_REVISION),
            Role::ControlPlane | Role::Postgres => None,
        }
    }

    pub fn supports(role: Role, revision: u32) -> bool {
        Book::window(role).is_some_and(|w| w.contains(&revision))
    }
}

/// Render one role's container at one revision. Total and deterministic: the same inputs
/// give the same specification, and its `io.quasar.spec` label is the hash of the rest.
pub fn render(
    role: Role,
    revision: u32,
    inputs: &Inputs,
    image: &ImageRef,
    secrets: &SecretMounts,
) -> Result<ContainerSpec, RenderError> {
    if !Book::supports(role, revision) {
        return Err(RenderError::Unsupported { role, revision });
    }
    validate(inputs)?;
    let mut spec = match role {
        Role::NodeAgent => node_agent_r1(inputs, image, secrets),
        Role::RecoveryActor => recovery_actor_r1(inputs, image),
        Role::ControlPlane | Role::Postgres => unreachable!("not in the book"),
    };
    spec.labels.extend([
        (labels::INSTALLATION.into(), inputs.installation_id.clone()),
        (labels::PLATFORM_SERVICE.into(), role.as_str().into()),
        (labels::RECIPE.into(), revision.to_string()),
    ]);
    let digest = spec_digest(&spec);
    spec.labels.insert(labels::SPEC.into(), digest);
    Ok(spec)
}

/// `sha256:<hex>` of the specification's canonical JSON, without its own `io.quasar.spec`
/// label: what identifies a rendered shape.
pub fn spec_digest(spec: &ContainerSpec) -> String {
    let mut unlabelled = spec.clone();
    unlabelled.labels.remove(labels::SPEC);
    let bytes = serde_json::to_vec(&unlabelled).expect("a spec always serializes");
    let sum = ring::digest::digest(&ring::digest::SHA256, &bytes);
    let hex: String = sum.as_ref().iter().map(|b| format!("{b:02x}")).collect();
    format!("sha256:{hex}")
}

fn safe_host_path(what: &str, path: &str) -> Result<(), RenderError> {
    let p = std::path::Path::new(path);
    if !p.is_absolute()
        || path == "/"
        || path.contains([':', ',', '\0', '\n'])
        || p.components()
            .any(|c| matches!(c, std::path::Component::ParentDir))
    {
        return Err(RenderError::Invalid(format!(
            "{what} must be an absolute host path other than /, without `..`, `:` or `,` (got {path:?})"
        )));
    }
    Ok(())
}

/// The checks `render` applies to machine inputs, for a caller that wants them before it
/// commits the inputs to machine state.
pub fn validate(inputs: &Inputs) -> Result<(), RenderError> {
    safe_host_path("the home root", &inputs.home_root)?;
    safe_host_path("the template root", &inputs.template_root)?;
    safe_host_path("the engine socket", &inputs.docker_socket)?;
    if inputs.home_root == inputs.template_root {
        return Err(RenderError::Invalid(
            "the home root and the template root must be different directories".into(),
        ));
    }
    let name_ok = !inputs.node_name.is_empty()
        && inputs.node_name.len() <= 253
        && inputs
            .node_name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.'));
    if !name_ok {
        return Err(RenderError::Invalid(format!(
            "node name {:?} must be 1-253 characters of letters, digits, `-`, `_` and `.`",
            inputs.node_name
        )));
    }
    if inputs.installation_id.is_empty() {
        return Err(RenderError::Invalid("no installation id".into()));
    }
    let fallback = inputs.gpu.fallback.as_ref().map(|f| &f.render_node);
    for node in inputs.gpu.render_node.iter().chain(fallback) {
        let ok = node
            .strip_prefix("/dev/dri/renderD")
            .is_some_and(|n| !n.is_empty() && n.bytes().all(|b| b.is_ascii_digit()));
        if !ok {
            return Err(RenderError::Invalid(format!(
                "render node {node:?} is not /dev/dri/renderD<n>"
            )));
        }
    }
    Ok(())
}

fn bind(source: &str, target: &str, read_only: bool) -> Bind {
    Bind {
        source: source.into(),
        target: target.into(),
        read_only,
    }
}

/// The agent's environment as `deploy/docker-compose.yml` renders it from an empty `.env`,
/// minus the entries a machine input or the owned install supplies (see `node_agent_r1`).
/// Operator overrides of these knobs are host policy (RH-05), not machine inputs.
const NODE_AGENT_ENV: &[(&str, &str)] = &[
    ("NODE_SECRET_PATH", "/var/lib/quasar-agent/node-secret"),
    ("RUST_LOG", "info"),
    ("XDG_RUNTIME_DIR", "/run/quasar-agent"),
    ("QUASAR_ENCODER", ""),
    ("QUASAR_HEALTH_ADDR", "127.0.0.1:9091"),
    ("QUASAR_HOMES_GC", ""),
    ("QUASAR_HOMES_GC_RETENTION_HOURS", ""),
    ("QUASAR_HOMES_GC_DRY_RUN", ""),
    ("QUASAR_HOMES_FREE_SPACE_FLOOR_GIB", ""),
    ("QUASAR_TEMPLATE_CLONE_MODE", ""),
    ("QUASAR_TEMPLATE_ALLOW_CROSSFS", ""),
    ("QUASAR_TEMPLATE_SETTLE_SECS", ""),
    ("QUASAR_TEMPLATE_WARMUP_TIMEOUT_SECS", ""),
    ("QUASAR_TEMPLATE_MIN_FREE_BYTES", ""),
    ("QUASAR_ZEROCOPY", "0"),
    ("QUASAR_LATENCY_PROBE", "0"),
    ("QUASAR_CAPTURE_H264", ""),
    ("LIBVA_TRACE", ""),
    ("GST_DEBUG", ""),
    ("QUASAR_TARGET_USAGE", "6"),
    ("QUASAR_QUEUE_BUFFERS", "3"),
    ("QUASAR_SLICES", "8"),
    ("QUASAR_FEC_MODE", ""),
    ("QUASAR_FEC_PERCENTAGE", "0"),
    ("QUASAR_FEC_ARM_LOSS_PCT", ""),
    ("QUASAR_FEC_WINDOW_S", ""),
    ("QUASAR_FEC_ARM_WINDOWS", ""),
    ("QUASAR_FEC_DISARM_WINDOWS", ""),
    ("QUASAR_FEC_MAX_FLAPS", ""),
    ("QUASAR_INTRA_REFRESH", "0"),
    ("QUASAR_INTRA_REFRESH_PERIOD", "0"),
    ("QUASAR_VULKAN_H264", ""),
    ("QUASAR_VULKAN_HEVC", ""),
    ("QUASAR_VULKAN_AV1", ""),
    ("WOLF_VULKAN_RING", ""),
    ("QUASAR_TRACE_RTP_TS", ""),
    ("QUASAR_TRACE_RTP_MARKER", ""),
    ("QUASAR_TRACE_ENC_PTS", ""),
    ("QUASAR_ABR", "1"),
    ("QUASAR_ABR_MODE", "smooth"),
    ("QUASAR_ABR_FLOOR_KBPS", ""),
    ("QUASAR_ABR_FLOOR_RATIO", "0.3"),
    ("QUASAR_ABR_EWMA_ALPHA", ""),
    ("QUASAR_ABR_DEADBAND", ""),
    ("QUASAR_ABR_MAX_UP_STEP", ""),
    ("QUASAR_ABR_MIN_INTERVAL_MS", ""),
    ("QUASAR_ABR_MAX_DOWN_STEP", ""),
    ("QUASAR_ABR_DOWN_DWELL_MS", ""),
    ("QUASAR_ABR_CLIFF_GUARD_FRAC", ""),
    ("MALLOC_ARENA_MAX", ""),
    ("MALLOC_TRIM_THRESHOLD_", ""),
    ("MALLOC_MMAP_THRESHOLD_", ""),
    ("QUASAR_MALLOC_TRIM", ""),
    ("QUASAR_AUDIO_DISABLED", ""),
    ("QUASAR_AUDIO_NO_CLOCK", ""),
    ("QUASAR_AUDIO_REQUIRED", ""),
    ("QUASAR_INPUT_TRACE", ""),
    ("QUASAR_INPUT_CHANNEL_MODE", ""),
    ("QUASAR_INPUT_BATCH_MS", ""),
    ("QUASAR_INPUT_CONTROLLER_NUDGE", ""),
    ("LIBGL_ALWAYS_SOFTWARE", ""),
    ("MESA_LOADER_DRIVER_OVERRIDE", ""),
    ("QUASAR_NVIDIA_DRIVER_HOST_PATH", ""),
    ("QUASAR_APP_SHM_SIZE", "1g"),
    ("QUASAR_APP_STOP_TIMEOUT_SECS", "10"),
    ("QUASAR_CONTAINER_NETWORK", "none"),
    ("QUASAR_APP_PUID", ""),
    ("QUASAR_APP_PGID", ""),
];

/// `deploy/docker-compose.nvidia.yml`'s environment, beyond the render node.
const NVIDIA_ENV: &[(&str, &str)] = &[
    ("NVIDIA_DRIVER_CAPABILITIES", "all"),
    ("QUASAR_GPU_NVIDIA", "1"),
    ("QUASAR_CUDA_DEVICE", "0"),
    ("QUASAR_NVIDIA_DRIVER_VOLUME", "1"),
    ("QUASAR_CUDA_RUNTIME", "1"),
    (
        "LD_LIBRARY_PATH",
        "/opt/quasar/nvidia-driver/lib64:/opt/quasar/nvidia-driver/cuda/lib64",
    ),
    ("LIBVA_MESSAGING_LEVEL", "0"),
];

pub use quasar_runtime::owned_install::{AGENT_SOCKET_ENV, ENROLLMENT_FILE_ENV};

fn node_agent_r1(inputs: &Inputs, image: &ImageRef, secrets: &SecretMounts) -> ContainerSpec {
    let reference = image.reference();
    let mut env: BTreeMap<String, String> = NODE_AGENT_ENV
        .iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect();
    env.extend([
        ("NODE_NAME".to_string(), inputs.node_name.clone()),
        ("QUASAR_HOME_ROOT".into(), inputs.home_root.clone()),
        ("QUASAR_TEMPLATE_ROOT".into(), inputs.template_root.clone()),
        // The sidecar follows the agent image (compose: `${QUASAR_PULSE_IMAGE:-$AGENT}`).
        ("QUASAR_PULSE_IMAGE".into(), reference.clone()),
        (
            "QUASAR_RENDER_NODE".into(),
            inputs
                .gpu
                .effective_render_node()
                .unwrap_or_default()
                .to_owned(),
        ),
        (AGENT_SOCKET_ENV.into(), paths::AGENT_SOCKET.into()),
    ]);
    let secrets_dir = paths::SECRETS_DIR;
    if secrets.files.contains(secrets::ENROLLMENT) {
        env.insert(
            ENROLLMENT_FILE_ENV.into(),
            format!("{secrets_dir}/{}", secrets::ENROLLMENT),
        );
    }

    let mut binds = vec![
        bind(&inputs.docker_socket, paths::ENGINE_SOCKET, false),
        bind(paths::AGENT_RUNTIME_DIR, paths::AGENT_RUNTIME_DIR, false),
        bind("/dev/input", "/dev/input", false),
        bind(&inputs.home_root, &inputs.home_root, false),
        bind(&inputs.template_root, &inputs.template_root, false),
        bind("/etc/os-release", "/host/etc/os-release", true),
        bind("/dev", "/host/dev", true),
        bind("/sys/kernel", "/host/sys/kernel", true),
        bind(names::AGENT_DATA_VOLUME, "/var/lib/quasar-agent", false),
        bind(names::AGENT_SOCKET_VOLUME, paths::AGENT_SOCKET_DIR, true),
    ];
    if let Some(volume) = &secrets.volume {
        binds.push(bind(volume, secrets_dir, true));
    }

    let mut devices = Vec::new();
    if inputs.devices.dri {
        devices.push(Device {
            host: "/dev/dri".into(),
            container: "/dev/dri".into(),
            permissions: "rwm".into(),
        });
    }
    if inputs.devices.uinput {
        devices.push(Device {
            host: "/dev/uinput".into(),
            container: "/dev/uinput".into(),
            permissions: "rwm".into(),
        });
    }
    if inputs.devices.kmsg {
        devices.push(Device {
            host: "/dev/kmsg".into(),
            container: "/dev/kmsg".into(),
            permissions: "r".into(),
        });
    }

    let mut gpus = Vec::new();
    if inputs.gpu.nvidia_shape() {
        env.extend(
            NVIDIA_ENV
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_string())),
        );
        binds.push(bind(
            names::NVIDIA_DRIVER_VOLUME,
            paths::NVIDIA_DRIVER_DIR,
            false,
        ));
        gpus.push(GpuRequest {
            driver: None,
            count: -1,
            capabilities: vec![vec!["gpu".into()]],
        });
    }
    binds.sort_by(|a, b| a.target.cmp(&b.target));

    ContainerSpec {
        name: names::NODE_AGENT.into(),
        image: reference,
        entrypoint: Some(vec!["/usr/local/bin/quasar-node-agent-entrypoint".into()]),
        cmd: None,
        env,
        labels: BTreeMap::new(),
        network_mode: Some("host".into()),
        binds,
        devices,
        device_cgroup_rules: vec!["c 13:* rmw".into()],
        gpus,
        cap_add: vec!["NET_ADMIN".into(), "SYSLOG".into()],
        security_opt: Vec::new(),
        init: true,
        restart: RestartPolicy::UnlessStopped,
    }
}

/// The recovery actor's own container: the engine socket, its machine state and the agent
/// socket volume. The hand-started actor of this slice is run with exactly this shape
/// (`docs/configuration.md` "Recovery actor"); the seed's frozen profile (ADR 0007) and the
/// hand-over render it.
fn recovery_actor_r1(inputs: &Inputs, image: &ImageRef) -> ContainerSpec {
    ContainerSpec {
        name: names::RECOVERY_ACTOR.into(),
        image: image.reference(),
        entrypoint: None,
        cmd: Some(vec!["actor".into()]),
        env: BTreeMap::from([("RUST_LOG".to_string(), "info".to_string())]),
        labels: BTreeMap::new(),
        network_mode: None,
        binds: vec![
            bind(names::AGENT_SOCKET_VOLUME, paths::AGENT_SOCKET_DIR, false),
            bind(names::MACHINE_VOLUME, paths::MACHINE_DIR, false),
            bind(&inputs.docker_socket, paths::ENGINE_SOCKET, false),
        ],
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: Vec::new(),
        cap_add: Vec::new(),
        // The engine socket on an SELinux host, as the updater's Compose service.
        security_opt: vec!["label=disable".into()],
        init: false,
        restart: RestartPolicy::UnlessStopped,
    }
}
