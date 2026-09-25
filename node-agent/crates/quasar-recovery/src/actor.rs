//! The recovery actor (architecture §5.2): `resume` and `status`. `submit` arrives with the
//! replacement slice (#360).
//!
//! `resume` runs once per start. It takes the machine's lease, creates machine state on a
//! clean machine, and makes sure every service this machine's role requires exists and
//! runs; a second run on a finished machine changes nothing. It never replaces a
//! container: one whose specification differs from what this actor would render is
//! reported and left alone. An install interrupted anywhere is completed by the next
//! `resume`, because every step is decided by observing the engine and machine state.
//!
//! An unreachable engine fails `resume`; the actor keeps serving `status` (marked stale)
//! and tries again only on its next start.

use std::collections::{BTreeMap, BTreeSet};
use std::io;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use quasar_runtime::{LeaseError, StateLease};
use tracing::{info, warn};

use crate::engine::{Container, ContainerSpec, EngineError, PlatformEngine, RestartPolicy};
use crate::machine::{Machine, MachineDir, ServiceRecord, FORMAT};
use crate::probe;
use crate::recipe::{
    self, labels, names, paths, secrets, Bind, ImageRef, Inputs, RenderError, Role, SecretMounts,
};
use crate::socket::{ActorIdentity, Conflict, DatabaseMode, MachineRole, Service, Status};

/// What the operator gave this start. Read only on a clean machine: once machine state
/// exists it wins and these are ignored (`CONTEXT.md` "Machine inputs").
#[derive(Debug, Clone, Default)]
pub struct OperatorInputs {
    pub enrollment: Option<String>,
    pub home_root: Option<String>,
    pub template_root: Option<String>,
    pub node_name: Option<String>,
    pub agent_image: Option<String>,
}

pub struct ActorConfig {
    pub machine_dir: PathBuf,
    pub role: MachineRole,
    pub operator: OperatorInputs,
    /// This process's own container, when it runs in one.
    pub self_container: Option<String>,
    /// The engine socket's daemon-host path when self-inspection cannot tell.
    pub docker_socket_fallback: String,
    pub new_installation_id: Box<dyn Fn() -> String + Send + Sync>,
    /// RFC 3339 UTC, for the timestamps machine state records.
    pub now: Box<dyn Fn() -> String + Send + Sync>,
    /// Between attempts of the `--gpus` probe (multiplied by the attempt number).
    pub gpus_probe_backoff: std::time::Duration,
}

impl ActorConfig {
    pub fn new(
        machine_dir: impl Into<PathBuf>,
        role: MachineRole,
        operator: OperatorInputs,
    ) -> Self {
        ActorConfig {
            machine_dir: machine_dir.into(),
            role,
            operator,
            self_container: None,
            docker_socket_fallback: paths::ENGINE_SOCKET.into(),
            new_installation_id: Box::new(random_uuid),
            now: Box::new(rfc3339_now),
            gpus_probe_backoff: std::time::Duration::from_secs(2),
        }
    }
}

#[derive(Debug)]
pub enum ResumeError {
    /// Another recovery actor holds this machine's lease; this one must not act.
    LeaseHeld,
    /// The machine-state volume is missing or unreadable.
    State(io::Error),
    Engine(EngineError),
    /// An operator input is missing or invalid on a clean machine.
    Inputs(String),
    /// A container or volume this installation would own already exists without its
    /// labels; nothing was done to it.
    OwnerConflict(String),
    /// The image declares a recipe revision this actor does not carry.
    RecipeUnsupported(String),
    /// Something this build does not do yet.
    Unsupported(String),
}

impl std::fmt::Display for ResumeError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ResumeError::LeaseHeld => f.write_str(
                "another recovery actor holds this machine's lease (actor.lease); this one will not act",
            ),
            ResumeError::State(e) => write!(f, "machine state: {e}"),
            ResumeError::Engine(e) => write!(f, "container engine: {e}"),
            ResumeError::Inputs(why) => write!(f, "install inputs: {why}"),
            ResumeError::OwnerConflict(why) => write!(f, "owner_conflict: {why}"),
            ResumeError::RecipeUnsupported(why) => write!(f, "recipe_unsupported: {why}"),
            ResumeError::Unsupported(why) => write!(f, "not supported by this build: {why}"),
        }
    }
}

impl std::error::Error for ResumeError {}

impl From<EngineError> for ResumeError {
    fn from(e: EngineError) -> Self {
        ResumeError::Engine(e)
    }
}

impl From<io::Error> for ResumeError {
    fn from(e: io::Error) -> Self {
        ResumeError::State(e)
    }
}

impl From<RenderError> for ResumeError {
    fn from(e: RenderError) -> Self {
        match e {
            RenderError::Unsupported { .. } => ResumeError::RecipeUnsupported(e.to_string()),
            RenderError::Invalid(why) => ResumeError::Inputs(why),
        }
    }
}

#[derive(Debug, Clone, Default)]
struct Inventory {
    services: Vec<Service>,
    conflicts: Vec<Conflict>,
}

pub struct Actor {
    engine: Arc<dyn PlatformEngine>,
    status_engine: Arc<dyn PlatformEngine>,
    config: ActorConfig,
    dir: MachineDir,
    lease: Mutex<Option<StateLease>>,
    last: Mutex<Option<Inventory>>,
    identity: Mutex<Option<ActorIdentity>>,
}

const PLATFORM_NAMES: &[&str] = &[
    names::NODE_AGENT,
    names::RECOVERY_ACTOR,
    names::CONTROL_PLANE,
    names::POSTGRES,
];
const HELPER_NAMES: &[&str] = &[names::GPU_PROBE, names::SECRETS_WRITER];
const COMPOSE_SERVICE: &str = "com.docker.compose.service";
const SECRETS_HELPER: &str = "secrets-writer";

impl Actor {
    pub fn new(engine: Arc<dyn PlatformEngine>, config: ActorConfig) -> Self {
        Actor {
            status_engine: engine.clone(),
            engine,
            dir: MachineDir::new(config.machine_dir.clone()),
            config,
            lease: Mutex::new(None),
            last: Mutex::new(None),
            identity: Mutex::new(None),
        }
    }

    /// Serve `status` through a separate engine client, typically one with a short
    /// deadline, so a slow engine yields a stale answer rather than a hung one.
    pub fn with_status_engine(mut self, engine: Arc<dyn PlatformEngine>) -> Self {
        self.status_engine = engine;
        self
    }

    /// Whether this actor holds the machine's lease, which it needs to serve its sockets.
    pub fn holds_lease(&self) -> bool {
        self.lease.lock().unwrap().is_some()
    }

    pub fn resume(&self) -> Result<(), ResumeError> {
        self.take_lease()?;
        self.sweep_helpers()?;
        let machine = match self.dir.load_machine()? {
            Some(machine) => {
                self.note_ignored_inputs(&machine);
                machine
            }
            None => self.first_install()?,
        };
        match machine.role {
            MachineRole::Gpu => self.ensure_node_agent(&machine),
            other => Err(ResumeError::Unsupported(format!(
                "machine role {other:?}; only `gpu` installs in this build (combined and control-only arrive with #361)"
            ))),
        }
    }

    /// The machine inventory. Never fails: when the engine does not answer, the last
    /// inventory is returned with `stale: true`.
    pub fn status(&self) -> Status {
        let (inventory, stale) = match self.inventory() {
            Ok(inventory) => {
                *self.last.lock().unwrap() = Some(inventory.clone());
                (inventory, false)
            }
            Err(_) => (self.last.lock().unwrap().clone().unwrap_or_default(), true),
        };
        let role = self
            .dir
            .load_machine()
            .ok()
            .flatten()
            .map(|m| m.role)
            .unwrap_or(self.config.role);
        Status {
            actor: self.identity(),
            seed: None,
            role,
            database: match role {
                MachineRole::Gpu => DatabaseMode::None,
                _ => DatabaseMode::Owned,
            },
            services: inventory.services,
            conflicts: inventory.conflicts,
            in_flight: None,
            dumps: Vec::new(),
            result: None,
            stale,
        }
    }

    fn take_lease(&self) -> Result<(), ResumeError> {
        let mut lease = self.lease.lock().unwrap();
        if lease.is_some() {
            return Ok(());
        }
        if !self.dir.root().is_dir() {
            return Err(ResumeError::State(io::Error::new(
                io::ErrorKind::NotFound,
                format!(
                    "{} does not exist: mount the {} volume there",
                    self.dir.root().display(),
                    names::MACHINE_VOLUME
                ),
            )));
        }
        match self.dir.lease() {
            Ok(held) => {
                *lease = Some(held);
                Ok(())
            }
            Err(LeaseError::Held(_)) => Err(ResumeError::LeaseHeld),
            Err(LeaseError::Open(e)) => Err(ResumeError::State(e)),
        }
    }

    /// A probe or secrets writer left by a crash is removed; nothing else is touched.
    fn sweep_helpers(&self) -> Result<(), ResumeError> {
        for name in [names::GPU_PROBE, names::SECRETS_WRITER] {
            if let Some(c) = self.engine.inspect_container(name)? {
                if c.labels.contains_key(labels::HELPER) {
                    info!(container = %c.name, "removing a helper left by an interrupted start");
                    self.engine.remove_container(&c.id)?;
                } else {
                    warn!(
                        token = "actor-helper-name-taken",
                        container = %c.name,
                        "a container this actor did not create holds the name of its {name} helper; it is left untouched and this start stops"
                    );
                    return Err(ResumeError::OwnerConflict(format!(
                        "container {name} ({}) is not this actor's helper; remove it to let the install continue",
                        c.image
                    )));
                }
            }
        }
        Ok(())
    }

    fn note_ignored_inputs(&self, machine: &Machine) {
        let op = &self.config.operator;
        if op.enrollment.is_some() {
            info!(
                installation = %machine.installation_id,
                "this machine is already installed; the enrollment string given at this start is ignored and can be removed"
            );
        }
        if let Some(home) = &op.home_root {
            if *home != machine.inputs.home_root {
                warn!(
                    token = "actor-input-ignored",
                    "QUASAR_HOME_ROOT={home} differs from the installed {}; machine inputs change only by reconfigure",
                    machine.inputs.home_root
                );
            }
        }
    }

    fn first_install(&self) -> Result<Machine, ResumeError> {
        if self.config.role != MachineRole::Gpu {
            return Err(ResumeError::Unsupported(format!(
                "machine role {:?}; only `gpu` installs in this build (combined and control-only arrive with #361)",
                self.config.role
            )));
        }
        let op = &self.config.operator;
        let enrollment = op
            .enrollment
            .as_deref()
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .ok_or_else(|| {
                ResumeError::Inputs(
                    "QUASAR_ENROLLMENT is required to install a GPU host (Admin → Fleet → Enroll host)".into(),
                )
            })?;
        if !enrollment.starts_with("qenr1.") {
            return Err(ResumeError::Inputs(
                "QUASAR_ENROLLMENT is not an enrollment string (expected `qenr1.…`)".into(),
            ));
        }
        let home_root = op
            .home_root
            .clone()
            .filter(|s| !s.is_empty())
            .ok_or_else(|| ResumeError::Inputs("QUASAR_HOME_ROOT is required".into()))?;
        let image = ImageRef::parse(op.agent_image.as_deref().unwrap_or(""))
            .map_err(|e| ResumeError::Inputs(format!("QUASAR_AGENT_IMAGE: {e}")))?;
        let host = self.engine.host()?;
        let node_name = op
            .node_name
            .clone()
            .filter(|s| !s.is_empty())
            .or_else(|| host.name.clone())
            .unwrap_or_else(|| "quasar-node".into());
        let mut inputs = Inputs {
            installation_id: (self.config.new_installation_id)(),
            node_name,
            home_root,
            template_root: op
                .template_root
                .clone()
                .filter(|s| !s.is_empty())
                .unwrap_or_else(recipe::default_template_root),
            docker_socket: self.docker_socket_host_path()?,
            gpu: Default::default(),
            devices: Default::default(),
        };
        recipe::validate(&inputs)?;

        self.dir.store_secret(secrets::ENROLLMENT, enrollment)?;
        self.ensure_image(&image)?;
        let report = match probe::run(self.engine.as_ref(), &image) {
            Ok(report) => report,
            Err(probe::ProbeError::Engine(e)) => return Err(e.into()),
            Err(e @ probe::ProbeError::Unreadable(_)) => {
                warn!(
                    token = "actor-gpu-probe-unreadable",
                    "GPU detection failed ({e}); installing the agent without GPU devices, and its readiness will report the gap"
                );
                probe::ProbeReport::default()
            }
        };
        let (gpu, devices) = probe::select(&report);
        match &gpu.vendor {
            Some(vendor) => info!(
                vendor = ?vendor,
                render_node = gpu.render_node.as_deref().unwrap_or(""),
                "GPU detected"
            ),
            None => warn!(
                token = "actor-no-gpu",
                "no usable GPU render node on this machine; installing the agent anyway, and its readiness will report the gap"
            ),
        }
        inputs.gpu = gpu;
        inputs.devices = devices;

        let machine = Machine {
            format: FORMAT,
            installation_id: inputs.installation_id.clone(),
            role: MachineRole::Gpu,
            created_at: (self.config.now)(),
            inputs,
            install_images: BTreeMap::from([(Role::NodeAgent, image)]),
        };
        self.dir.machine().store(&machine)?;
        info!(installation = %machine.installation_id, node = %machine.inputs.node_name, "machine state created");
        Ok(machine)
    }

    /// The daemon-host path of the engine socket this process was given, learned from its
    /// own container's mounts.
    fn docker_socket_host_path(&self) -> Result<String, ResumeError> {
        if let Some(id) = &self.config.self_container {
            if let Some(me) = self.engine.inspect_container(id)? {
                if let Some((source, _, _)) = me
                    .mounts
                    .iter()
                    .find(|(_, target, _)| target == paths::ENGINE_SOCKET)
                {
                    return Ok(source.clone());
                }
            }
        }
        Ok(self.config.docker_socket_fallback.clone())
    }

    fn ensure_image(&self, image: &ImageRef) -> Result<crate::engine::Image, ResumeError> {
        let reference = image.reference();
        if let Some(found) = self.engine.inspect_image(&reference)? {
            return Ok(found);
        }
        info!(image = %reference, "pulling");
        self.engine.pull(&reference)?;
        self.engine
            .inspect_image(&reference)?
            .ok_or(ResumeError::Engine(EngineError::Runtime(
                crate::engine::ErrorKind::Missing,
            )))
    }

    fn owned_labels(&self, machine: &Machine, role: Role) -> BTreeMap<String, String> {
        BTreeMap::from([
            (
                labels::INSTALLATION.to_string(),
                machine.installation_id.clone(),
            ),
            (
                labels::PLATFORM_SERVICE.to_string(),
                role.as_str().to_string(),
            ),
        ])
    }

    fn ensure_volume(&self, machine: &Machine, name: &str, role: Role) -> Result<(), ResumeError> {
        match self.engine.inspect_volume(name)? {
            Some(v) => match v.labels.get(labels::INSTALLATION) {
                Some(id) if *id != machine.installation_id => Err(ResumeError::OwnerConflict(
                    format!("volume {name} belongs to installation {id}"),
                )),
                _ => Ok(()),
            },
            None => {
                self.engine
                    .create_volume(name, &self.owned_labels(machine, role))?;
                Ok(())
            }
        }
    }

    fn is_ours(&self, machine: &Machine, c: &Container, role: Role) -> bool {
        c.labels.get(labels::INSTALLATION) == Some(&machine.installation_id)
            && c.labels.get(labels::PLATFORM_SERVICE).map(String::as_str) == Some(role.as_str())
    }

    fn node_agent_secrets(&self) -> Result<SecretMounts, ResumeError> {
        let mut files = BTreeSet::new();
        if self.dir.load_secret(secrets::ENROLLMENT)?.is_some() {
            files.insert(secrets::ENROLLMENT.to_string());
        }
        Ok(SecretMounts {
            volume: Some(names::NODE_AGENT_SECRETS_VOLUME.into()),
            files,
        })
    }

    fn ensure_node_agent(&self, machine: &Machine) -> Result<(), ResumeError> {
        let mut machine = machine.clone();
        let machine = &mut machine;
        let role = Role::NodeAgent;
        let image = match self.dir.load_service(role)? {
            Some(record) => record.image,
            None => machine.install_images.get(&role).cloned().ok_or_else(|| {
                ResumeError::Inputs("machine state names no node-agent image".into())
            })?,
        };
        self.ensure_volume(machine, names::AGENT_DATA_VOLUME, role)?;
        self.ensure_volume(machine, names::NODE_AGENT_SECRETS_VOLUME, role)?;
        self.ensure_volume(machine, names::AGENT_SOCKET_VOLUME, Role::RecoveryActor)?;
        let secrets = self.node_agent_secrets()?;

        if let Some(existing) = self.engine.inspect_container(role.container_name())? {
            if machine.inputs.gpu.nvidia_shape() {
                self.ensure_volume(machine, names::NVIDIA_DRIVER_VOLUME, role)?;
            }
            if !self.is_ours(machine, &existing, role) {
                return Err(ResumeError::OwnerConflict(format!(
                    "container {} ({}) is not this installation's; it is left untouched",
                    existing.name, existing.image
                )));
            }
            let revision = existing
                .labels
                .get(labels::RECIPE)
                .and_then(|r| r.parse().ok())
                .unwrap_or(0);
            let spec = recipe::render(role, revision, &machine.inputs, &image, &secrets)?;
            if existing.labels.get(labels::SPEC) != spec.labels.get(labels::SPEC) {
                warn!(
                    token = "actor-spec-differs",
                    container = %existing.name,
                    "the running node agent differs from what this actor renders; it is left as it is (replacing a service is not in this build)"
                );
                return Ok(());
            }
            if existing.status == "created" {
                info!(container = %existing.name, "starting the node agent an interrupted install created");
                self.engine.start_container(&existing.id)?;
            }
            self.record(role, revision, &image, spec)?;
            return Ok(());
        }

        let found = self.ensure_image(&image)?;
        let revision = image_revision(&found, &image)?;
        self.decide_gpus(machine, &image)?;
        if machine.inputs.gpu.nvidia_shape() {
            self.ensure_volume(machine, names::NVIDIA_DRIVER_VOLUME, role)?;
        }
        let spec = recipe::render(role, revision, &machine.inputs, &image, &secrets)?;
        self.deliver_secrets(&image, names::NODE_AGENT_SECRETS_VOLUME, &secrets.files)?;
        let id = self.engine.create_container(&spec)?;
        self.engine.start_container(&id)?;
        info!(container = names::NODE_AGENT, image = %image.reference(), revision, "node agent created and started");
        self.record(role, revision, &image, spec)
    }

    /// On an NVIDIA machine not yet known to serve `--gpus`, ask the engine before the agent
    /// is created. Only a yes is recorded; a definite no installs the agent without the
    /// NVIDIA shape (readiness reports the gap) and is asked again the next time the agent
    /// is created; no answer stops this start.
    fn decide_gpus(&self, machine: &mut Machine, image: &ImageRef) -> Result<(), ResumeError> {
        let gpu = &machine.inputs.gpu;
        if gpu.vendor != Some(recipe::GpuVendor::Nvidia) || gpu.gpus_served {
            return Ok(());
        }
        match probe::serves_gpus(self.engine.as_ref(), image, self.config.gpus_probe_backoff)? {
            probe::GpusAnswer::Served => {
                info!(
                    token = "actor-gpus-served",
                    "NVIDIA: the engine started a --gpus all probe; installing the NVIDIA shape"
                );
                machine.inputs.gpu.gpus_served = true;
                self.dir.machine().store(machine)?;
            }
            probe::GpusAnswer::Refused(why) => warn!(
                token = "actor-gpus-refused",
                "NVIDIA device found, but the engine does not serve --gpus: {why}; installing without the NVIDIA shape (is the NVIDIA Container Toolkit installed for this engine?)"
            ),
        }
        Ok(())
    }

    fn record(
        &self,
        role: Role,
        revision: u32,
        image: &ImageRef,
        spec: ContainerSpec,
    ) -> Result<(), ResumeError> {
        let record = ServiceRecord {
            role,
            recipe_revision: revision,
            image: image.clone(),
            spec_digest: spec.labels.get(labels::SPEC).cloned().unwrap_or_default(),
            spec,
            applied_at: (self.config.now)(),
        };
        Ok(self.dir.store_service(&record)?)
    }

    /// Writes the secret files into a service's secrets volume through a helper container
    /// that is created, never started, and removed.
    fn deliver_secrets(
        &self,
        image: &ImageRef,
        volume: &str,
        files: &BTreeSet<String>,
    ) -> Result<(), ResumeError> {
        let mut entries = Vec::new();
        for name in files {
            if let Some(value) = self.dir.load_secret(name)? {
                entries.push((name.clone(), value));
            }
        }
        let archive = tar_of(&entries)?;
        let writer = ContainerSpec {
            name: names::SECRETS_WRITER.into(),
            image: image.reference(),
            entrypoint: Some(vec!["/bin/true".into()]),
            cmd: None,
            env: BTreeMap::new(),
            labels: BTreeMap::from([(labels::HELPER.to_string(), SECRETS_HELPER.to_string())]),
            network_mode: Some("none".into()),
            binds: vec![Bind {
                source: volume.into(),
                target: "/secrets".into(),
                read_only: false,
            }],
            devices: Vec::new(),
            device_cgroup_rules: Vec::new(),
            gpus: Vec::new(),
            cap_add: Vec::new(),
            security_opt: Vec::new(),
            init: false,
            restart: RestartPolicy::No,
        };
        let id = self.engine.create_container(&writer)?;
        let uploaded = self.engine.upload_archive(&id, "/secrets", archive);
        let removed = self.engine.remove_container(&id);
        uploaded?;
        removed?;
        Ok(())
    }

    fn identity(&self) -> ActorIdentity {
        if let Some(known) = self.identity.lock().unwrap().clone() {
            return known;
        }
        let mut identity = ActorIdentity {
            version: crate::identity::version().into(),
            commit: crate::identity::source_commit().into(),
            image: String::new(),
            digest: None,
        };
        let Some(id) = &self.config.self_container else {
            return identity;
        };
        let Ok(Some(me)) = self.status_engine.inspect_container(id) else {
            return identity;
        };
        let repository = repository_of(&me.image);
        identity.image = repository.clone();
        identity.digest = match me.image.split_once('@') {
            Some((_, digest)) => Some(digest.to_owned()),
            None => self
                .status_engine
                .inspect_image(&me.image_id)
                .ok()
                .flatten()
                .and_then(|i| {
                    i.repo_digests.into_iter().find_map(|d| {
                        let (repo, digest) = d.split_once('@')?;
                        (repo == repository).then(|| digest.to_owned())
                    })
                }),
        };
        *self.identity.lock().unwrap() = Some(identity.clone());
        identity
    }

    fn inventory(&self) -> Result<Inventory, EngineError> {
        let containers = self.status_engine.list_containers()?;
        let installation = self
            .dir
            .load_machine()
            .ok()
            .flatten()
            .map(|m| m.installation_id);
        let me = self.config.self_container.as_deref();
        let mut services = Vec::new();
        let mut conflicts = Vec::new();
        for c in &containers {
            if c.labels.contains_key(labels::HELPER) {
                continue;
            }
            if Some(c.id.as_str()) == me {
                services.push(service(c, Role::RecoveryActor.as_str()));
                continue;
            }
            let ours = installation.is_some()
                && c.labels.get(labels::INSTALLATION) == installation.as_ref();
            if ours {
                if let Some(role) = c.labels.get(labels::PLATFORM_SERVICE) {
                    services.push(service(c, role));
                }
                continue;
            }
            let compose = c.labels.get(COMPOSE_SERVICE).map(String::as_str);
            if HELPER_NAMES.contains(&c.name.as_str()) {
                conflicts.push(Conflict {
                    container: c.name.clone(),
                    image: repository_of(&c.image),
                    why: "holds the name of a recovery-actor helper without its label".into(),
                });
            } else if PLATFORM_NAMES.contains(&c.name.as_str())
                || compose.is_some_and(|s| PLATFORM_NAMES.contains(&s))
            {
                conflicts.push(Conflict {
                    container: c.name.clone(),
                    image: repository_of(&c.image),
                    why: if compose.is_some() {
                        "a Compose service named like a Quasar platform service, without this installation's labels".into()
                    } else {
                        "a Quasar platform service name without this installation's labels".into()
                    },
                });
            }
        }
        services.sort_by_key(|s| (s.role != Role::RecoveryActor.as_str(), s.role.clone()));
        Ok(Inventory {
            services,
            conflicts,
        })
    }
}

fn service(c: &Container, role: &str) -> Service {
    Service {
        role: role.into(),
        container: c.name.clone(),
        image: repository_of(&c.image),
        digest: c.image.split_once('@').map(|(_, d)| d.to_owned()),
        state: c.status.clone(),
        health: c.health.clone(),
    }
}

/// `registry/repo:tag@sha256:…` → `registry/repo`.
fn repository_of(reference: &str) -> String {
    let without_digest = reference.split('@').next().unwrap_or(reference);
    match without_digest.rsplit_once(':') {
        Some((repo, tag)) if !tag.contains('/') => repo.to_owned(),
        _ => without_digest.to_owned(),
    }
}

fn image_revision(image: &crate::engine::Image, reference: &ImageRef) -> Result<u32, ResumeError> {
    let label = image.labels.get(labels::IMAGE_RECIPE).ok_or_else(|| {
        ResumeError::RecipeUnsupported(format!(
            "{} carries no {} label, so it cannot be installed by a recovery actor",
            reference.reference(),
            labels::IMAGE_RECIPE
        ))
    })?;
    label.trim().parse().map_err(|_| {
        ResumeError::RecipeUnsupported(format!(
            "{} declares {}={label:?}, which is not a revision",
            reference.reference(),
            labels::IMAGE_RECIPE
        ))
    })
}

fn tar_of(entries: &[(String, String)]) -> Result<Vec<u8>, ResumeError> {
    let mut builder = tar::Builder::new(Vec::new());
    for (name, value) in entries {
        let mut header = tar::Header::new_gnu();
        header.set_size(value.len() as u64);
        header.set_mode(0o400);
        header.set_uid(0);
        header.set_gid(0);
        header.set_mtime(0);
        header.set_entry_type(tar::EntryType::Regular);
        builder.append_data(&mut header, name, value.as_bytes())?;
    }
    Ok(builder.into_inner()?)
}

fn random_uuid() -> String {
    use ring::rand::{SecureRandom, SystemRandom};
    let mut b = [0u8; 16];
    SystemRandom::new()
        .fill(&mut b)
        .expect("the system random source");
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    let h: String = b.iter().map(|x| format!("{x:02x}")).collect();
    format!(
        "{}-{}-{}-{}-{}",
        &h[0..8],
        &h[8..12],
        &h[12..16],
        &h[16..20],
        &h[20..32]
    )
}

pub fn rfc3339_now() -> String {
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    rfc3339(secs)
}

fn rfc3339(secs: i64) -> String {
    let days = secs.div_euclid(86_400);
    let rem = secs.rem_euclid(86_400);
    // Howard Hinnant's civil_from_days.
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + i64::from(month <= 2);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}Z",
        rem / 3600,
        (rem % 3600) / 60,
        rem % 60
    )
}
