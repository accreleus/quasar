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

pub mod control;
pub mod revision;

use std::collections::{BTreeMap, BTreeSet};
use std::ops::RangeInclusive;

use serde::{Deserialize, Serialize};

pub use quasar_runtime::platform::{
    Bind, ContainerSpec, Device, GpuRequest, Healthcheck, PublishedPort, RestartPolicy,
};
pub use quasar_runtime::{GpuInjection, NVIDIA_CDI_DEVICE};

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
    /// The never-started helper through which the engine makes the agent's runtime
    /// directory at an actor start (#439).
    pub const RUNTIME_DIR_HELPER: &str = "quasar-runtime-dir";
    /// The never-started helper through which a secrets volume is written.
    pub const SECRETS_WRITER: &str = "quasar-secrets-writer";
    /// The one-shot helper an `uninstall --purge` dumps a Quasar-owned database with.
    pub const FINAL_DUMP: &str = "quasar-final-dump";
    /// Where that dump goes unless the operator names a directory: a purge never deletes it.
    pub const FINAL_DUMP_VOLUME: &str = "quasar-final-dump";
    /// The disposable helper a migrating update's dump and a restore run in (`crate::database`).
    pub const DB_HELPER: &str = "quasar-db-helper";
    /// Every disposable helper the actor creates and removes: a crash may leave one.
    pub const HELPERS: &[&str] = &[GPU_PROBE, SECRETS_WRITER, FINAL_DUMP, DB_HELPER];
    /// The control plane's own state (its TLS pair, the artwork cache): a named volume, so
    /// it outlives every replacement of the container.
    pub const CONTROL_DATA_VOLUME: &str = "quasar-control-data";
    /// The control plane's per-service secrets volume.
    pub const CONTROL_PLANE_SECRETS_VOLUME: &str = "quasar-control-plane-secrets";
    /// A Quasar-owned database's data.
    pub const POSTGRES_DATA_VOLUME: &str = "quasar-postgres-data";
    /// Postgres's per-service secrets volume.
    pub const POSTGRES_SECRETS_VOLUME: &str = "quasar-postgres-secrets";
    /// The bridge network on which the control plane reaches a Quasar-owned Postgres by
    /// name. Nothing else joins it.
    pub const PLATFORM_NETWORK: &str = "quasar-platform";
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

    /// On a combined or control-only machine the socket volume holds one subdirectory per
    /// socket, and each container is given only its own, by a bind of that subdirectory's
    /// daemon-host path (Engine API 1.40 has no volume sub-paths).
    pub const AGENT_SOCKET_SUBDIR: &str = "agent";
    pub const CONTROL_SOCKET_SUBDIR: &str = "control";
    /// The agent socket inside the actor on a combined host.
    pub const SPLIT_AGENT_SOCKET: &str = "/run/quasar-recovery/agent/agent.sock";
    /// The control socket inside the actor.
    pub const CONTROL_SOCKET: &str = "/run/quasar-recovery/control/control.sock";
    /// Where the control plane sees its socket directory, and the socket in it.
    pub const CONTROL_PLANE_SOCKET_DIR: &str = "/run/quasar-recovery";
    pub const CONTROL_PLANE_SOCKET: &str = "/run/quasar-recovery/control.sock";
    /// The control plane's state directory (`QUASAR_TLS_DIR` defaults under it).
    pub const CONTROL_DATA_DIR: &str = "/var/lib/quasar-control";
    pub const POSTGRES_DATA_DIR: &str = "/var/lib/postgresql/data";
}

/// The uid and gid the control-plane image runs as (`deploy/Dockerfile.control.prod`,
/// the `quasar` user of the quasar-base family). Its secret files and its control socket
/// are owned by it.
pub const CONTROL_PLANE_UID: u32 = 1000;

/// The secret files a recipe knows how to consume.
pub mod secrets {
    /// The operator's enrollment string (`qenr1.…`), for the agent to redeem.
    pub const ENROLLMENT: &str = "enrollment";
    /// The database password: generated for a Quasar-owned Postgres, the operator's for
    /// an external database (copied from the seed's inputs at first boot).
    pub const DATABASE_PASSWORD: &str = "database-password";
    /// `QUASAR_SECRET_KEY`: base64 of 32 random bytes, generated at install.
    pub const SECRET_KEY: &str = "secret-key";
    /// A combined host's single-use local enrollment token, which the control plane
    /// inserts as a `host_enrollments` row and its own agent redeems.
    pub const LOCAL_ENROLLMENT: &str = "local-enrollment";
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
    pub const ALL: [Role; 4] = [
        Role::ControlPlane,
        Role::NodeAgent,
        Role::Postgres,
        Role::RecoveryActor,
    ];

    pub fn as_str(self) -> &'static str {
        match self {
            Role::ControlPlane => "control-plane",
            Role::NodeAgent => "node-agent",
            Role::Postgres => "postgres",
            Role::RecoveryActor => "recovery-actor",
        }
    }

    pub fn parse(s: &str) -> Option<Role> {
        Role::ALL.into_iter().find(|r| r.as_str() == s)
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

/// The fields of one level of machine state that this build does not know: written by a
/// newer actor, kept, and written back on every rewrite, so an actor put back by a revert
/// does not erase them.
pub type Unknown = BTreeMap<String, serde_json::Value>;

/// What the disposable probe learned about the machine's GPU (architecture §5.3 "GPU facts
/// detected by a disposable probe").
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
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
    /// The engine served the GPU through CDI (`nvidia.com/gpu=all`) rather than
    /// `--gpus`. From recipe revision 3 the agent asks for it the same way. Written only
    /// when true.
    #[serde(default, skip_serializing_if = "is_false")]
    pub cdi: bool,
    /// On an NVIDIA machine, the lowest other recognised render node: what the agent uses
    /// when the engine does not serve `--gpus`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fallback: Option<GpuNode>,
    #[serde(flatten)]
    pub unknown: Unknown,
}

fn is_false(b: &bool) -> bool {
    !*b
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct GpuNode {
    pub vendor: GpuVendor,
    pub render_node: String,
    #[serde(flatten)]
    pub unknown: Unknown,
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
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct HostDevices {
    pub dri: bool,
    pub uinput: bool,
    pub kmsg: bool,
    /// RH-07 #402: the host lets unprivileged processes read the kernel log
    /// (`kernel.dmesg_restrict=0`, host preparation's `--allow-kernel-log`). From recipe
    /// revision 3 the agent is given `/dev/kmsg` only then, and never `SYSLOG`. Written
    /// only when true, so machine state an older actor wrote reads back unchanged.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub kernel_log: bool,
    /// RH-07 #402: the engine runs rootless. It refuses device-cgroup rules (access to
    /// device nodes is then the host's device permissions, which host preparation sets),
    /// so from recipe revision 3 the agent is created without one there. Written only
    /// when true.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub engine_rootless: bool,
    /// The host has `/dev/fuse`. From revision 3 the agent no longer sees the host's `/dev`,
    /// so the recipe tells it (`QUASAR_HOST_FUSE`) whether sessions may be given the node.
    /// Written only when true.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub fuse: bool,
    /// Rootless Docker mounts no sysfs for a host-network container (a fresh sysfs needs a
    /// network namespace of its own), so the agent would see neither its GPUs nor its
    /// input devices. From revision 3 it is given the host's `/sys`, read-only: what any
    /// other container sees. Written only when true.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub host_sysfs: bool,
    /// The host has `/dev/snd` (RH-07 #395). Console mode gives the agent the host's sound
    /// devices only then: a bind of a missing source is refused by Podman and silently
    /// created as an empty directory by Docker. Read again whenever console mode is turned
    /// on, since sound may appear after the install. Written only when true.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub sound: bool,
    /// The host's `/dev/i2c-<n>` bus numbers (RH-07 #407), for console mode's DDC on a
    /// rootless engine, where the agent cannot `mknod` them: each is passed as a device.
    /// Bus numbers can change across reboots and the engine refuses to create (or start) a
    /// container naming a node the host lacks, so the list is read again whenever console
    /// mode is turned on and at every recovery-actor start while it is on
    /// ([`crate::console`]). Sorted, without duplicates; written only when non-empty.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub i2c: Vec<u32>,
    /// The host runs logind: `/run/systemd/seats` and `/run/systemd/sessions` exist (RH-07
    /// #407). Console mode binds them read-only so the agent can name what holds the
    /// display (the login screen, someone's desktop session). Read with `i2c`; written
    /// only when true.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub logind: bool,
    /// The host has `/run/quasar-console-audio` (RH-07 #407, D13): host preparation's
    /// `--console-audio-user` made the desktop user's PipeWire listen there, in a directory
    /// only that user and the Quasar group can enter. Console mode binds it read-write so
    /// the agent plays console audio through that PipeWire instead of fighting it for the
    /// sound device. Read with `i2c`; written only when true.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub console_audio: bool,
    /// The host's DRM nodes (`/dev/dri/card*`, `/dev/dri/renderD*`), as the probe listed
    /// them (RH-07 #407). On a rootless engine each is passed as its own device instead of
    /// the `/dev/dri` directory, which rootless Podman refuses to expand in some hosts
    /// ("no devices found"); it is also the narrower grant. Read again whenever console
    /// mode is turned on. Sorted, without duplicates; written only when non-empty.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub dri_nodes: Vec<String>,
    /// The host has [`CONSOLE_VT_NODE`] (RH-07 #407): console mode's own virtual terminal,
    /// which the agent makes active with the kernel keyboard off for a console session's
    /// life. Absent on a host without VTs, which has no kernel keyboard handler to turn
    /// off. Read with `i2c`; written only when true.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub console_vt: bool,
    #[serde(flatten)]
    pub unknown: Unknown,
}

impl Default for HostDevices {
    fn default() -> Self {
        HostDevices {
            dri: true,
            uinput: true,
            kmsg: true,
            kernel_log: false,
            engine_rootless: false,
            fuse: false,
            host_sysfs: false,
            sound: false,
            i2c: Vec::new(),
            logind: false,
            console_audio: false,
            dri_nodes: Vec::new(),
            console_vt: false,
            unknown: Unknown::new(),
        }
    }
}

/// The machine inputs a recipe is rendered with. Only ever grows, each with a default.
///
/// Unknown fields, here and in every nested input, are kept ([`Unknown`]), so an actor put
/// back by a revert reads machine state a newer actor wrote and does not erase it on a
/// rewrite. It renders only the inputs its own recipes know, which is what it rendered
/// before; `ImageRef` and `ContainerSpec` stay strict.
///
/// On a GPU host the agent takes the control plane's URL and pin from the enrollment
/// string, so there is no control URL. A combined host's agent reaches the control plane
/// on its own machine, at the loopback address of `control.http_port`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
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
    /// A combined or control-only machine: what its control plane is run with. `None` on
    /// a GPU host.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub control: Option<ControlInputs>,
    /// A combined or control-only machine: the daemon-host path of the socket volume,
    /// whose `agent/` and `control/` subdirectories are bound into the node agent and the
    /// control plane (`paths::AGENT_SOCKET_SUBDIR`). `None` on a GPU host, whose agent
    /// mounts the whole volume.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub socket_dir: Option<String>,
    /// This machine's release-trust settings, from the seed at first install.
    #[serde(default, skip_serializing_if = "TrustInputs::is_empty")]
    pub trust: TrustInputs,
    /// A control-plane machine: the images its Add host gives a new GPU host.
    #[serde(default, skip_serializing_if = "EnrollImages::is_empty")]
    pub enroll: EnrollImages,
    /// Host defaults for the agent's app containers, from the seed at first install.
    #[serde(default, skip_serializing_if = "AppInputs::is_empty")]
    pub app: AppInputs,
    /// Console mode (RH-07 #395): the agent is given the console additions
    /// ([`console_access`]). Set only by the agent's console request on the agent socket,
    /// never by an operator reconfigure. Written only when true.
    #[serde(default, skip_serializing_if = "is_false")]
    pub console: bool,
    /// Console mode has been on here with the console VT (RH-07 #407), and stays set when it
    /// is turned off: every later agent is still given [`CONSOLE_VT_NODE`], so its startup
    /// can put back a VT a killed console agent left switched with the keyboard off. Set
    /// only by the actor's console paths ([`Inputs::keep_console_vt`]). Written only when
    /// true.
    #[serde(default, skip_serializing_if = "is_false")]
    pub console_vt_kept: bool,
    #[serde(flatten)]
    pub unknown: Unknown,
}

impl Inputs {
    /// With console mode on and the host's console VT seen, remember it for good.
    pub fn keep_console_vt(&mut self) {
        if self.console && self.devices.console_vt {
            self.console_vt_kept = true;
        }
    }
}

/// Host defaults the agent gives its app containers (`QUASAR_APP_PUID`, `QUASAR_APP_PGID`,
/// `QUASAR_CONTAINER_NETWORK`). Values of environment entries the node-agent recipe always
/// carried, so no revision moves: `None` renders what revision 1 always rendered.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct AppInputs {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub puid: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pgid: Option<u32>,
    /// `none`, `bridge` or `host`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub container_network: Option<String>,
    #[serde(flatten)]
    pub unknown: Unknown,
}

impl AppInputs {
    pub fn is_empty(&self) -> bool {
        *self == AppInputs::default()
    }
}

/// What `QUASAR_CONTAINER_NETWORK` accepts (docs/configuration.md).
pub const APP_NETWORKS: &[&str] = &["none", "bridge", "host"];

/// The images a control plane's Add host (#359) installs on a new GPU host, by digest
/// (`repository@sha256:…`): the seed (this machine's recovery image) and the node agent.
/// Recorded at install; `None` serves none. `seed`/`agent` are the install-time images, the
/// control plane's last resort; `*_override` are the operator's `QUASAR_ENROLL_*` seed
/// inputs, which win over the installed release's images (#365).
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct EnrollImages {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub seed: Option<ImageRef>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub agent: Option<ImageRef>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub seed_override: Option<ImageRef>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub agent_override: Option<ImageRef>,
    #[serde(flatten)]
    pub unknown: Unknown,
}

impl EnrollImages {
    pub fn is_empty(&self) -> bool {
        *self == EnrollImages::default()
    }
}

/// The release-trust settings of a machine (`docs/configuration.md` "Recovery actor"): what
/// its recovery actor admits platform images under and, on a control-plane machine, what
/// its control plane checks a developer apply against and reads a test registry over. Kept
/// as the variables' raw values, which the install checked; an unset one keeps its default.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct TrustInputs {
    /// `QUASAR_UPDATER_ALLOWED_NAMESPACES`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub allowed_namespaces: Option<String>,
    /// `QUASAR_UPDATER_SIGNATURE_MODE`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub signature_mode: Option<String>,
    /// `QUASAR_UPDATER_TRUSTED_KEYS` (public keys).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub trusted_keys: Option<String>,
    /// `QUASAR_UPDATER_MANIFEST_BASE_URL`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub manifest_base_url: Option<String>,
    /// `QUASAR_UPDATER_MANIFEST_TIMEOUT_S`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub manifest_timeout_s: Option<String>,
    /// `QUASAR_PLATFORM_INSECURE_REGISTRIES`: the control plane's plain-HTTP registries.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub insecure_registries: Option<String>,
    #[serde(flatten)]
    pub unknown: Unknown,
}

impl TrustInputs {
    pub fn is_empty(&self) -> bool {
        *self == TrustInputs::default()
    }

    fn values(&self) -> [&Option<String>; 6] {
        [
            &self.allowed_namespaces,
            &self.signature_mode,
            &self.trusted_keys,
            &self.manifest_base_url,
            &self.manifest_timeout_s,
            &self.insecure_registries,
        ]
    }
}

/// What a combined or control-only machine's control plane is run with.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ControlInputs {
    /// `combined` or `control_only`: the control plane serves it as `machine_role`. Absent
    /// in state an earlier build wrote; `load_machine` sets it from the machine's role.
    #[serde(default)]
    pub machine_role: ControlRole,
    /// The host port of the control plane's HTTP listener (agents and `/health`).
    pub http_port: u16,
    /// The host port of its HTTPS listener (the console).
    pub tls_port: u16,
    /// The name or address the console is reached by: a SAN on the generated certificate.
    #[serde(default)]
    pub public_host: Option<String>,
    /// More SANs for the generated certificate (`QUASAR_TLS_HOSTS`), comma-separated.
    #[serde(default)]
    pub tls_hosts: Option<String>,
    /// The reverse proxies whose X-Forwarded-For the control plane honours
    /// (`QUASAR_TRUSTED_PROXIES`), checked with the control plane's rules.
    #[serde(default)]
    pub trusted_proxies: Option<String>,
    pub database: DatabaseInputs,
    #[serde(flatten)]
    pub unknown: Unknown,
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ControlRole {
    #[default]
    Combined,
    ControlOnly,
}

impl ControlRole {
    pub fn as_str(self) -> &'static str {
        match self {
            ControlRole::Combined => "combined",
            ControlRole::ControlOnly => "control_only",
        }
    }
}

/// Whose database the control plane uses (`CONTEXT.md` "Machine inputs": database mode).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "mode")]
pub enum DatabaseInputs {
    /// A Postgres this machine's recovery actor created, on the platform network.
    Owned,
    /// The operator's own database: Quasar only uses it. Its password is a secret in
    /// machine state, never an input.
    External {
        host: String,
        port: u16,
        user: String,
        name: String,
        sslmode: String,
        #[serde(flatten)]
        unknown: Unknown,
    },
}

/// The template root beside a home root, as the agent defaults it
/// (`{QUASAR_HOME_ROOT}/../templates`): `/srv/quasar/homes` → `/srv/quasar/templates`.
pub fn template_root_beside(home_root: &str) -> String {
    match std::path::Path::new(home_root).parent() {
        Some(parent) if parent != std::path::Path::new("") => {
            parent.join("templates").to_string_lossy().into_owned()
        }
        _ => default_template_root(),
    }
}

/// For a machine with no home root (a control-only machine), and for machine state
/// written before the template root was recorded.
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
            // Revision 2: the agent reads `ENROLLMENT_TOKEN_FILE`, which a combined host's
            // agent is given. A GPU host's agent has the same shape at both.
            // Revision 3 (RH-07 #402): least privilege in every engine mode.
            Role::NodeAgent => Some(1..=3),
            Role::RecoveryActor => Some(1..=revision::RECIPE_REVISION),
            // Revision 2: Add host's images arrive as operator overrides plus install-time
            // fallbacks, so the installed release's images can come between (#365).
            Role::ControlPlane => Some(1..=2),
            // The Postgres image carries no recipe label: its revision is this actor's own
            // (`control::POSTGRES_REVISION`).
            Role::Postgres => Some(1..=1),
        }
    }

    pub fn supports(role: Role, revision: u32) -> bool {
        Book::window(role).is_some_and(|w| w.contains(&revision))
    }

    /// Every role's window as one JSON line, a role with none omitted. The shape
    /// `scripts/release/check-release-compatibility.sh` reads from `quasar-recovery recipes`.
    pub fn windows_json() -> String {
        let windows: serde_json::Map<String, serde_json::Value> = Role::ALL
            .into_iter()
            .filter_map(|role| {
                Book::window(role).map(|w| {
                    (
                        role.as_str().to_string(),
                        serde_json::json!({ "from": w.start(), "to": w.end() }),
                    )
                })
            })
            .collect();
        serde_json::json!({ "format_version": 1, "windows": windows }).to_string()
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
        Role::NodeAgent => {
            // A revision-1 agent reads no `ENROLLMENT_TOKEN_FILE`, so it cannot be a
            // combined host's agent.
            if inputs.control.is_some() && revision < 2 {
                return Err(RenderError::Unsupported { role, revision });
            }
            if inputs.home_root.is_empty() {
                return Err(RenderError::Invalid(
                    "a node agent needs a home root".into(),
                ));
            }
            let mut spec = node_agent_r1(inputs, image, secrets);
            if revision >= 3 {
                least_privilege(&mut spec, inputs);
                if inputs.console {
                    console_access(&mut spec, inputs);
                }
                console_vt(&mut spec, inputs);
            } else if inputs.console {
                // Fail closed: an older revision would render without the console additions
                // and a console request would settle `applied` with no console access.
                return Err(RenderError::Invalid(
                    "console mode needs node-agent recipe revision 3 or later".into(),
                ));
            }
            if inputs.control.is_some() {
                control::local_agent(&mut spec, inputs, secrets)?;
            }
            spec
        }
        Role::RecoveryActor => recovery_actor_r1(inputs, image),
        Role::ControlPlane => control::control_plane(revision, inputs, image, secrets)?,
        Role::Postgres => control::postgres_r1(inputs, image, secrets)?,
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

pub(crate) fn safe_host_path(what: &str, path: &str) -> Result<(), RenderError> {
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
    // Only a control-only machine has no home root; a node agent refuses to render
    // without one.
    if !inputs.home_root.is_empty() || inputs.control.is_none() {
        safe_host_path("the home root", &inputs.home_root)?;
    }
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
    if let Some(dir) = &inputs.socket_dir {
        safe_host_path("the socket volume's host path", dir)?;
    }
    for value in inputs.trust.values().into_iter().flatten() {
        if value.contains(['\n', '\r', '\0']) || value.len() > 4096 {
            return Err(RenderError::Invalid(
                "a release-trust setting holds a line break or is longer than 4096 bytes".into(),
            ));
        }
    }
    if let Some(n) = &inputs.app.container_network {
        if !APP_NETWORKS.contains(&n.as_str()) {
            return Err(RenderError::Invalid(format!(
                "the app container network {n:?} is not one of {}",
                APP_NETWORKS.join(", ")
            )));
        }
    }
    if let Some(control) = &inputs.control {
        control::validate(control)?;
        if inputs.socket_dir.is_none() {
            return Err(RenderError::Invalid(
                "a combined or control-only machine needs its socket volume's host path".into(),
            ));
        }
    }
    if inputs.console && !inputs.devices.dri {
        return Err(RenderError::Invalid(
            "console mode needs the host's /dev/dri, which this host lacks".into(),
        ));
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

pub(crate) fn bind(source: &str, target: &str, read_only: bool) -> Bind {
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

pub use quasar_runtime::owned_install::{
    AGENT_SOCKET_ENV, ENROLLMENT_FILE_ENV, ENROLLMENT_TOKEN_FILE_ENV,
};

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
    if let Some(puid) = inputs.app.puid {
        env.insert("QUASAR_APP_PUID".into(), puid.to_string());
    }
    if let Some(pgid) = inputs.app.pgid {
        env.insert("QUASAR_APP_PGID".into(), pgid.to_string());
    }
    if let Some(network) = &inputs.app.container_network {
        env.insert("QUASAR_CONTAINER_NETWORK".into(), network.clone());
    }
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
        if inputs.devices.engine_rootless && !inputs.devices.dri_nodes.is_empty() {
            for node in &inputs.devices.dri_nodes {
                devices.push(Device {
                    host: node.clone(),
                    container: node.clone(),
                    permissions: "rwm".into(),
                });
            }
        } else {
            devices.push(Device {
                host: "/dev/dri".into(),
                container: "/dev/dri".into(),
                permissions: "rwm".into(),
            });
        }
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
        gpus.push(GpuRequest::nvidia_all(GpuInjection::DeviceRequest));
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
        ports: Vec::new(),
        healthcheck: None,
    }
}

/// Recipe revision 3 (RH-07 #402, decisions D1 and D11, P1 least privilege): the agent
/// gives up the host's `/dev` mount, `NET_ADMIN`, `SYSLOG` and `/dev/kmsg`, in every
/// engine mode, rootful included. What used them becomes optional and reports why it is
/// skipped: GPU fault messages need `/dev/kmsg`, given only when the host allows
/// unprivileged kernel-log reads; the media reachability check reads real traffic
/// instead of firewall rules. It mounts the engine socket, so it carries
/// `label=disable` like every other container that does.
fn least_privilege(spec: &mut ContainerSpec, inputs: &Inputs) {
    spec.binds.retain(|b| b.target != "/host/dev");
    spec.cap_add.clear();
    if !inputs.devices.kernel_log {
        spec.devices.retain(|d| d.host != "/dev/kmsg");
    }
    // A rootless engine refuses device-cgroup rules; the Quasar user's device access is
    // the host's (the udev rule host preparation writes). Rootful keeps read/write on the
    // input nodes the agent creates after it starts, but no `m`: nothing is `mknod`ed (#401).
    if inputs.devices.host_sysfs {
        spec.binds.push(bind("/sys", "/sys", true));
        spec.binds.sort_by(|a, b| a.target.cmp(&b.target));
    }
    if inputs.devices.engine_rootless {
        spec.device_cgroup_rules.clear();
    } else {
        spec.device_cgroup_rules = vec!["c 13:* rw".into()];
    }
    spec.security_opt = vec!["label=disable".into()];
    // CDI where the engine served it (D10); `--gpus` stays only where it did not.
    if inputs.gpu.nvidia_shape() && inputs.gpu.cdi {
        spec.gpus = vec![GpuRequest::nvidia_all(GpuInjection::Cdi)];
    }
    // The host's answer the agent can no longer read from /host/dev.
    spec.env.insert(
        "QUASAR_HOST_FUSE".into(),
        if inputs.devices.fuse { "1" } else { "0" }.into(),
    );
}

/// Console mode (RH-07 #395, #407): what `deploy/overlays/docker-compose.console.yml`
/// grants, shaped for the engine mode. Each rootful difference from the overlay is listed in
/// `tests/recipe_compose_parity.rs`.
///
/// Taking a free display needs no capability: the first opener of a card node with no
/// DRM master becomes master, and may set master again on that fd (Linux >= 5.8,
/// `drm_auth.c`). So a rootless engine, where `SYS_ADMIN` never reaches the initial user
/// namespace the kernel checks, gets no capability and no device-cgroup rules (it refuses
/// them): device access is the host's, which host preparation grants the Quasar account.
fn console_access(spec: &mut ContainerSpec, inputs: &Inputs) {
    let rootless = inputs.devices.engine_rootless;
    if !rootless {
        // seatd's drmSetMaster on weston's behalf (session/console.rs).
        // TODO(#407, owner decision 1): drop SYS_ADMIN here too once the agent's display
        // preflight has landed and hardware has proven the rootful recipe path without it
        // (a free display needs none, the spike showed); the rootful goldens change then.
        spec.cap_add.push("SYS_ADMIN".into());
    }
    // Sound only on a host that has it: console mode runs quiet without, and the agent's
    // console_audio check says why.
    if inputs.devices.sound {
        spec.binds.push(bind("/dev/snd", "/dev/snd", false));
        // Docker forbids a bind into the container's own /proc; sink discovery reads this.
        spec.binds
            .push(bind("/proc/asound", "/host-proc/asound", true));
        if !rootless {
            // ALSA nodes arrive with the bind, so no `m`.
            spec.device_cgroup_rules.push("c 116:* rw".to_string());
        }
    }
    // logind's seat and session files, read-only: how the agent names what holds the
    // display when it cannot take it.
    if inputs.devices.logind {
        for dir in LOGIND_DIRS {
            spec.binds
                .push(bind(dir, &format!("{CONSOLE_HOST_PREFIX}{dir}"), true));
        }
    }
    // The desktop user's PipeWire, through the Quasar-only socket host preparation made
    // (D13): read-write, since connecting to a socket is a write. The same path inside, as
    // the agent names the server by it.
    if inputs.devices.console_audio {
        spec.binds
            .push(bind(CONSOLE_AUDIO_DIR, CONSOLE_AUDIO_DIR, false));
    }
    spec.binds.sort_by(|a, b| a.target.cmp(&b.target));
    if rootless {
        // No mknod inside a user namespace: each i2c node the host has is passed in.
        for bus in &inputs.devices.i2c {
            let node = format!("/dev/i2c-{bus}");
            spec.devices.push(Device {
                host: node.clone(),
                container: node,
                permissions: "rw".into(),
            });
        }
    } else {
        // The agent mknods /dev/i2c-N for DDC (ddc.rs). DRM is already a mapped device
        // (`/dev/dri`), so no major-226 rule.
        spec.device_cgroup_rules.push("c 89:* rmw".to_string());
    }
    spec.env.insert(CONSOLE_ACCESS_ENV.into(), "1".into());
}

/// The console VT, by device on either engine, while console mode is on and, once it has
/// been on, after it is turned off ([`Inputs::console_vt_kept`]): a console agent killed
/// holding it leaves the host's console switched with the keyboard off, and only an agent
/// with the node can put it back. Read/write only; opening it needs no capability
/// (session/console_vt.rs), and host preparation's ACL is what grants it.
fn console_vt(spec: &mut ContainerSpec, inputs: &Inputs) {
    if inputs.devices.console_vt && (inputs.console || inputs.console_vt_kept) {
        spec.devices.push(Device {
            host: CONSOLE_VT_NODE.into(),
            container: CONSOLE_VT_NODE.into(),
            permissions: "rw".into(),
        });
    }
}

/// Console mode's own virtual terminal. The other halves: the agent's
/// `session::console_vt::CONSOLE_VT` and `deploy/prepare-host.sh --console`.
pub const CONSOLE_VT_NODE: &str = "/dev/tty8";

/// logind's state directories console mode binds read-only (`HostDevices::logind`).
pub const LOGIND_DIRS: [&str; 2] = ["/run/systemd/seats", "/run/systemd/sessions"];

/// The directory holding the desktop user's Quasar-only PipeWire (pulse protocol) socket,
/// on the host and inside the agent (`HostDevices::console_audio`).
pub const CONSOLE_AUDIO_DIR: &str = "/run/quasar-console-audio";

/// Where console mode's host views live inside the agent: `/host/run/systemd/seats`, ...
pub const CONSOLE_HOST_PREFIX: &str = "/host";

/// `1` on an agent container created with the console additions, and absent otherwise.
pub const CONSOLE_ACCESS_ENV: &str = "QUASAR_CONSOLE_ACCESS";

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
        ports: Vec::new(),
        healthcheck: None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn windows_json_is_one_line_with_every_window() {
        let line = Book::windows_json();
        assert!(!line.contains('\n'), "{line}");
        assert!(
            line.starts_with(r#"{"format_version":1,"windows":{"control-plane":{"from":"#),
            "{line}"
        );
        let doc: serde_json::Value = serde_json::from_str(&line).unwrap();
        assert_eq!(doc.as_object().unwrap().len(), 2, "{line}");
        assert_eq!(doc["format_version"], 1);
        let windows = doc["windows"].as_object().unwrap();
        let mut carried = 0;
        for role in Role::ALL {
            match Book::window(role) {
                Some(w) => {
                    carried += 1;
                    assert_eq!(
                        windows[role.as_str()],
                        serde_json::json!({ "from": w.start(), "to": w.end() }),
                        "{line}"
                    );
                }
                None => assert!(!windows.contains_key(role.as_str()), "{line}"),
            }
        }
        assert_eq!(windows.len(), carried, "{line}");
    }
}
