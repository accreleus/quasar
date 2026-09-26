//! The in-memory adapter: an engine the actor's tests drive and observe. Faults are
//! injected by call index over every call (reads included), before or after the call
//! takes effect; [`EngineError::Crashed`] stands for the actor process dying there.

use std::collections::{BTreeMap, BTreeSet};
use std::io::Read;
use std::sync::Mutex;
use std::time::Duration;

use super::{
    Container, ContainerSpec, EngineError, EngineHost, ErrorKind, Image, Network, PlatformEngine,
    RestartPolicy, Volume,
};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum When {
    /// The call fails and nothing happens.
    Before,
    /// The call takes effect, then the caller sees the failure.
    After,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Fault {
    /// Zero-based index over every call this engine receives.
    pub call: usize,
    pub when: When,
    pub error: EngineError,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FakeContainer {
    pub id: String,
    pub spec: ContainerSpec,
    /// `created`, `running` or `exited`.
    pub status: String,
    pub starts: u32,
    pub restart: RestartPolicy,
    pub exit_code: Option<i64>,
    pub logs: String,
    pub health: Option<String>,
}

/// How a started container of one image behaves. An image with none runs, with no
/// healthcheck.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Behaviour {
    /// The health the engine reports once the container runs: `None` is no healthcheck.
    pub health: Option<String>,
    /// The engine refuses the start with this message; the container stays `created`.
    pub refuse_start: Option<String>,
    /// What the container has printed, for `logs_tail`.
    pub logs: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct FakeVolume {
    pub labels: BTreeMap<String, String>,
    /// Path inside the volume → (content, mode).
    pub files: BTreeMap<String, (Vec<u8>, u32)>,
    /// `None` is the `local` driver.
    pub driver: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct FakeState {
    /// What a pull can fetch, by reference.
    pub registry: BTreeMap<String, Image>,
    /// Local images, by the reference they were pulled or loaded as.
    pub images: BTreeMap<String, Image>,
    /// By id.
    pub containers: BTreeMap<String, FakeContainer>,
    pub volumes: BTreeMap<String, FakeVolume>,
    /// User-defined networks and their labels.
    pub networks: BTreeMap<String, BTreeMap<String, String>>,
    pub host: EngineHost,
    /// Device nodes the host has; creating a container naming another fails.
    pub host_devices: BTreeSet<String>,
    /// What a started GPU probe prints.
    pub probe_output: String,
    /// Whether the engine can start a container that requests GPUs (`--gpus all`); when
    /// not, the start is refused with Docker's device-driver message.
    pub gpus_supported: bool,
    /// The refusal a GPU-requesting start gets when `gpus_supported` is false; empty is
    /// Docker's device-driver message.
    pub gpus_refusal_message: String,
    /// Failures the next creates of a GPU-requesting container return, in order.
    pub gpus_create_failures: Vec<EngineError>,
    /// How a started container behaves, by its image reference.
    pub behaviour: BTreeMap<String, Behaviour>,
    pub unreachable: bool,
    pub next_id: u64,
    /// The control plane's database, as the database helpers (`crate::database`) and a
    /// starting control plane see it. `None`: nothing simulates one.
    pub database: Option<FakeDatabase>,
    /// A database helper's forced outcome, by its helper label (`db-dump`, ...): its exit
    /// code and output, with no other effect.
    pub db_failures: BTreeMap<String, (i64, String)>,
    /// Every control-plane image started against a database whose schema is above the one
    /// the image declares: what must never happen.
    pub older_control_planes_started: Vec<String>,
}

/// A database: its `schema_migrations` row and a token standing for its rows.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct FakeDatabase {
    pub schema_version: i64,
    pub dirty: bool,
    pub rows: String,
    pub size_bytes: u64,
}

/// What the in-memory engine's `pg_dump` writes: a custom-format archive's magic, then the
/// database it holds.
pub fn dump_bytes(db: &FakeDatabase) -> Vec<u8> {
    format!(
        "PGDMP fake archive\nschema={}\ndirty={}\nrows={}\nsize={}\n",
        db.schema_version,
        if db.dirty { "t" } else { "f" },
        db.rows,
        db.size_bytes
    )
    .into_bytes()
}

fn parse_dump(bytes: &[u8]) -> Option<FakeDatabase> {
    let text = std::str::from_utf8(bytes).ok()?;
    let mut lines = text.lines();
    if !lines.next()?.starts_with("PGDMP") {
        return None;
    }
    let mut db = FakeDatabase::default();
    for line in lines {
        let (k, v) = line.split_once('=')?;
        match k {
            "schema" => db.schema_version = v.parse().ok()?,
            "dirty" => db.dirty = v == "t",
            "rows" => db.rows = v.into(),
            "size" => db.size_bytes = v.parse().ok()?,
            _ => {}
        }
    }
    Some(db)
}

/// One database helper run: `(exit code, output)`. Files are real, under the host path the
/// helper's `/dumps` bind names (a test's temporary machine-state directory).
fn db_helper(s: &mut FakeState, spec: &ContainerSpec, op: &str) -> (i64, String) {
    if let Some(forced) = s.db_failures.get(op) {
        return forced.clone();
    }
    let file = spec.env.get("QUASAR_DUMP_FILE").and_then(|f| {
        let (dir, name) = f.rsplit_once('/')?;
        let bind = spec.binds.iter().find(|b| b.target == dir)?;
        Some((
            std::path::Path::new(&bind.source).join(name),
            bind.read_only,
        ))
    });
    let owned = spec.env.get("PGHOST").map(String::as_str) == Some("quasar-postgres");
    let reachable = !owned
        || s.containers
            .values()
            .any(|c| c.spec.name == "quasar-postgres" && c.status == "running");
    let unreachable = (2, "psql: error: connection to server failed\n".to_string());
    let schema_line = |db: &FakeDatabase| {
        format!(
            "schema={} dirty={}\n",
            db.schema_version,
            if db.dirty { "t" } else { "f" }
        )
    };
    match op {
        "db-size" => match (&s.database, reachable) {
            (Some(db), true) => (0, format!("{}\n", db.size_bytes)),
            _ => unreachable,
        },
        "db-schema" => match (&s.database, reachable) {
            (Some(db), true) => (0, schema_line(db)),
            _ => unreachable,
        },
        "db-dump" => {
            let (Some(db), true) = (&s.database, reachable) else {
                return unreachable;
            };
            match file {
                Some((path, false)) => match std::fs::write(&path, dump_bytes(db)) {
                    Ok(()) => (0, String::new()),
                    Err(e) => (
                        1,
                        format!("pg_dump: error: could not open output file: {e}\n"),
                    ),
                },
                _ => (1, "pg_dump: error: no writable output file\n".into()),
            }
        }
        "db-inspect" | "db-load" => {
            let Some((path, _)) = file else {
                return (1, "pg_restore: error: no input file\n".into());
            };
            let Some(archived) = std::fs::read(&path).ok().and_then(|b| parse_dump(&b)) else {
                return (
                    1,
                    "pg_restore: error: input file does not appear to be a valid archive\n".into(),
                );
            };
            if op == "db-inspect" {
                return (0, schema_line(&archived));
            }
            if !reachable {
                return unreachable;
            }
            s.database = Some(archived);
            (0, String::new())
        }
        _ => (0, String::new()),
    }
}

/// A control plane starting against the database: it migrates it up to the schema its
/// image declares, or, older than the database, is recorded as what must never happen.
fn control_plane_boot(s: &mut FakeState, spec: &ContainerSpec) {
    if spec
        .labels
        .get("io.quasar.platform-service")
        .map(String::as_str)
        != Some("control-plane")
    {
        return;
    }
    let declared = s
        .images
        .get(&spec.image)
        .and_then(|i| i.labels.get("org.quasar.schema.version"))
        .and_then(|v| v.parse::<i64>().ok());
    let (Some(db), Some(declared)) = (s.database.as_mut(), declared) else {
        return;
    };
    if declared < db.schema_version {
        s.older_control_planes_started.push(spec.image.clone());
    } else {
        db.schema_version = declared;
    }
}

impl FakeState {
    pub fn container_named(&self, name: &str) -> Option<&FakeContainer> {
        self.containers.values().find(|c| c.spec.name == name)
    }

    /// Everything but ids: two installs that reached the same end state compare equal
    /// however many helpers each created on the way.
    pub fn by_name(&self) -> BTreeMap<String, (ContainerSpec, String)> {
        self.containers
            .values()
            .map(|c| (c.spec.name.clone(), (c.spec.clone(), c.status.clone())))
            .collect()
    }
}

/// A container lifecycle change an engine call made, for a test that runs a process per
/// container (the recovery actor's hand-over has two).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Lifecycle {
    /// A container started running (not a helper, which runs to completion at once).
    Started(String),
    /// A running container was stopped.
    Stopped(String),
    /// A container was removed.
    Removed(String),
}

type Hook = std::sync::Arc<dyn Fn(&Lifecycle) + Send + Sync>;

#[derive(Default)]
struct Inner {
    state: FakeState,
    calls: usize,
    faults: Vec<Fault>,
    events: Vec<Lifecycle>,
}

#[derive(Default)]
pub struct FakeEngine {
    inner: Mutex<Inner>,
    hook: Mutex<Option<Hook>>,
}

const HELPER_LABEL: &str = "io.quasar.helper";
const PROBE_HELPER: &str = "gpu-probe";

impl FakeEngine {
    pub fn new(state: FakeState) -> Self {
        FakeEngine {
            inner: Mutex::new(Inner {
                state,
                calls: 0,
                faults: Vec::new(),
                events: Vec::new(),
            }),
            hook: Mutex::new(None),
        }
    }

    /// Called, outside the engine's lock, after every call that started, stopped or
    /// removed a container.
    pub fn on_lifecycle(&self, hook: impl Fn(&Lifecycle) + Send + Sync + 'static) {
        *self.hook.lock().unwrap() = Some(std::sync::Arc::new(hook));
    }

    pub fn state(&self) -> FakeState {
        self.inner.lock().unwrap().state.clone()
    }

    pub fn with_state<R>(&self, f: impl FnOnce(&mut FakeState) -> R) -> R {
        f(&mut self.inner.lock().unwrap().state)
    }

    /// How many calls this engine has received.
    pub fn calls(&self) -> usize {
        self.inner.lock().unwrap().calls
    }

    pub fn inject(&self, fault: Fault) {
        self.inner.lock().unwrap().faults.push(fault);
    }

    pub fn clear_faults(&self) {
        self.inner.lock().unwrap().faults.clear();
    }

    /// What a daemon restart does to containers: every running one stops, and the ones
    /// whose restart policy is `unless-stopped` come back. A stopped or created container,
    /// and one whose policy is `no`, stays as it is. Not an engine call.
    pub fn restart_daemon(&self) {
        let mut inner = self.inner.lock().unwrap();
        for c in inner.state.containers.values_mut() {
            if c.status != "running" {
                continue;
            }
            if c.restart == RestartPolicy::UnlessStopped {
                c.starts += 1;
            } else {
                c.status = "exited".into();
                c.exit_code = Some(0);
            }
        }
    }

    /// Runs one call: counts it, applies a `Before` fault, the operation, then an
    /// `After` fault.
    fn call<T>(
        &self,
        op: impl FnOnce(&mut FakeState, &mut Vec<Lifecycle>) -> Result<T, EngineError>,
    ) -> Result<T, EngineError> {
        let (result, events) = self.call_locked(op);
        if !events.is_empty() {
            let hook = self.hook.lock().unwrap().clone();
            if let Some(hook) = hook {
                for event in &events {
                    hook(event);
                }
            }
        }
        result
    }

    fn call_locked<T>(
        &self,
        op: impl FnOnce(&mut FakeState, &mut Vec<Lifecycle>) -> Result<T, EngineError>,
    ) -> (Result<T, EngineError>, Vec<Lifecycle>) {
        let mut inner = self.inner.lock().unwrap();
        let index = inner.calls;
        inner.calls += 1;
        let fault = |when| {
            inner
                .faults
                .iter()
                .find(|f| f.call == index && f.when == when)
                .map(|f| f.error.clone())
        };
        if let Some(error) = fault(When::Before) {
            return (Err(error), Vec::new());
        }
        let after = fault(When::After);
        if inner.state.unreachable {
            return (
                Err(EngineError::Runtime(ErrorKind::Unavailable)),
                Vec::new(),
            );
        }
        let inner = &mut *inner;
        let result = op(&mut inner.state, &mut inner.events);
        let events = std::mem::take(&mut inner.events);
        match after {
            Some(error) => (Err(error), events),
            None => (result, events),
        }
    }
}

/// Where the `local` driver keeps a volume's data on the engine host.
pub fn mountpoint(volume: &str) -> String {
    format!("/var/lib/docker/volumes/{volume}/_data")
}

fn engine(kind: ErrorKind) -> EngineError {
    EngineError::Runtime(kind)
}

fn refused(status: u16, message: &str) -> EngineError {
    EngineError::Refused {
        status,
        message: message.into(),
    }
}

fn find<'a>(state: &'a FakeState, name_or_id: &str) -> Option<&'a FakeContainer> {
    state
        .containers
        .get(name_or_id)
        .or_else(|| state.container_named(name_or_id))
        // The engine resolves a unique id prefix of at least 12 characters, as $HOSTNAME is.
        .or_else(|| {
            let mut matching = state
                .containers
                .values()
                .filter(|c| name_or_id.len() >= 12 && c.id.starts_with(name_or_id));
            match (matching.next(), matching.next()) {
                (Some(only), None) => Some(only),
                _ => None,
            }
        })
}

fn find_id(state: &FakeState, name_or_id: &str) -> Result<String, EngineError> {
    find(state, name_or_id)
        .map(|c| c.id.clone())
        .ok_or(engine(ErrorKind::Missing))
}

fn view(s: &FakeState, c: &FakeContainer) -> Container {
    Container {
        id: c.id.clone(),
        name: c.spec.name.clone(),
        image: c.spec.image.clone(),
        image_id: s
            .images
            .get(&c.spec.image)
            .map(|i| i.id.clone())
            .unwrap_or_else(|| format!("sha256:image-of-{}", c.spec.image)),
        labels: c.spec.labels.clone(),
        status: c.status.clone(),
        running: c.status == "running",
        health: c.health.clone(),
        restart: Some(c.restart),
        mounts: c
            .spec
            .binds
            .iter()
            .map(|b| (b.source.clone(), b.target.clone(), b.read_only))
            .collect(),
        command: c
            .spec
            .entrypoint
            .iter()
            .flatten()
            .chain(c.spec.cmd.iter().flatten())
            .cloned()
            .collect(),
        env: c.spec.env.iter().map(|(k, v)| format!("{k}={v}")).collect(),
    }
}

impl PlatformEngine for FakeEngine {
    fn host(&self) -> Result<EngineHost, EngineError> {
        self.call(|s, _ev| Ok(s.host.clone()))
    }

    fn inspect_image(&self, reference: &str) -> Result<Option<Image>, EngineError> {
        self.call(|s, _ev| {
            Ok(s.images
                .get(reference)
                .or_else(|| s.images.values().find(|i| i.id == reference))
                .cloned())
        })
    }

    fn pull(&self, reference: &str) -> Result<(), EngineError> {
        self.call(|s, _ev| {
            let image = s
                .registry
                .get(reference)
                .cloned()
                .ok_or(engine(ErrorKind::ManifestMissing))?;
            s.images.insert(reference.into(), image);
            Ok(())
        })
    }

    fn inspect_container(&self, name_or_id: &str) -> Result<Option<Container>, EngineError> {
        self.call(|s, _ev| Ok(find(s, name_or_id).map(|c| view(s, c))))
    }

    fn list_containers(&self) -> Result<Vec<Container>, EngineError> {
        self.call(|s, _ev| {
            let mut all: Vec<Container> = s.containers.values().map(|c| view(s, c)).collect();
            all.sort_by(|a, b| a.name.cmp(&b.name));
            Ok(all)
        })
    }

    fn create_container(&self, spec: &ContainerSpec) -> Result<String, EngineError> {
        self.call(|s, _ev| {
            if !spec.gpus.is_empty() && !s.gpus_create_failures.is_empty() {
                return Err(s.gpus_create_failures.remove(0));
            }
            if s.container_named(&spec.name).is_some() {
                return Err(refused(
                    409,
                    &format!("Conflict. The container name \"/{}\" is already in use", spec.name),
                ));
            }
            if !s.images.contains_key(&spec.image) {
                return Err(refused(404, &format!("No such image: {}", spec.image)));
            }
            if let Some(d) = spec
                .devices
                .iter()
                .find(|d| !s.host_devices.contains(&d.host))
            {
                return Err(refused(
                    500,
                    &format!("error gathering device information while adding custom device \"{}\": no such file or directory", d.host),
                ));
            }
            if let Some(network) = spec.network_mode.as_deref() {
                let builtin = matches!(network, "host" | "none" | "bridge" | "default")
                    || network.starts_with("container:");
                if !builtin && !s.networks.contains_key(network) {
                    return Err(refused(404, &format!("network {network} not found")));
                }
            }
            // The engine creates a named volume a bind names but nobody created.
            for bind in spec.binds.iter().filter(|b| b.is_volume()) {
                s.volumes.entry(bind.source.clone()).or_default();
            }
            s.next_id += 1;
            let id = format!("{:064x}", s.next_id);
            s.containers.insert(
                id.clone(),
                FakeContainer {
                    id: id.clone(),
                    spec: spec.clone(),
                    status: "created".into(),
                    starts: 0,
                    restart: spec.restart,
                    exit_code: None,
                    logs: String::new(),
                    health: None,
                },
            );
            Ok(id)
        })
    }

    fn start_container(&self, id: &str) -> Result<(), EngineError> {
        self.call(|s, ev| {
            let id = find_id(s, id)?;
            let output = s.probe_output.clone();
            let gpus_supported = s.gpus_supported;
            let gpus_refusal = if s.gpus_refusal_message.is_empty() {
                "could not select device driver \"\" with capabilities: [[gpu]]".to_string()
            } else {
                s.gpus_refusal_message.clone()
            };
            let behaviour = s
                .containers
                .get(&id)
                .and_then(|c| s.behaviour.get(&c.spec.image))
                .cloned();
            let c = s.containers.get_mut(&id).unwrap();
            if c.status == "running" {
                return Ok(());
            }
            if !c.spec.gpus.is_empty() && !gpus_supported {
                return Err(refused(500, &gpus_refusal));
            }
            if let Some(message) = behaviour.as_ref().and_then(|b| b.refuse_start.clone()) {
                return Err(refused(500, &message));
            }
            c.starts += 1;
            // A helper runs to completion at once; the GPU probe prints its report and a
            // database helper acts on the simulated database.
            if let Some(helper) = c.spec.labels.get(HELPER_LABEL).cloned() {
                let spec = c.spec.clone();
                let (code, logs) = if helper.starts_with("db-") {
                    db_helper(s, &spec, &helper)
                } else if helper == PROBE_HELPER {
                    (0, output)
                } else {
                    (0, String::new())
                };
                let c = s.containers.get_mut(&id).unwrap();
                c.status = "exited".into();
                c.exit_code = Some(code);
                c.logs = logs;
            } else {
                c.status = "running".into();
                if let Some(b) = behaviour {
                    c.health = b.health;
                    c.logs = b.logs;
                }
                let spec = c.spec.clone();
                control_plane_boot(s, &spec);
                ev.push(Lifecycle::Started(id.clone()));
            }
            Ok(())
        })
    }

    fn stop_container(&self, id: &str, _grace: Duration) -> Result<(), EngineError> {
        self.call(|s, ev| {
            let id = find_id(s, id)?;
            let c = s.containers.get_mut(&id).unwrap();
            if c.status == "running" {
                c.status = "exited".into();
                c.exit_code = Some(0);
                ev.push(Lifecycle::Stopped(id.clone()));
            }
            Ok(())
        })
    }

    fn set_restart_policy(&self, id: &str, policy: RestartPolicy) -> Result<(), EngineError> {
        self.call(|s, _ev| {
            let id = find_id(s, id)?;
            s.containers.get_mut(&id).unwrap().restart = policy;
            Ok(())
        })
    }

    fn rename_container(&self, id: &str, name: &str) -> Result<(), EngineError> {
        self.call(|s, _ev| {
            if s.container_named(name).is_some() {
                return Err(engine(ErrorKind::Engine));
            }
            let id = find_id(s, id)?;
            s.containers.get_mut(&id).unwrap().spec.name = name.into();
            Ok(())
        })
    }

    fn remove_container(&self, id: &str) -> Result<(), EngineError> {
        self.call(|s, ev| {
            if let Ok(id) = find_id(s, id) {
                s.containers.remove(&id);
                ev.push(Lifecycle::Removed(id));
            }
            Ok(())
        })
    }

    fn wait_container(&self, id: &str, _timeout: Duration) -> Result<i64, EngineError> {
        self.call(|s, _ev| {
            let c = find(s, id).ok_or(engine(ErrorKind::Missing))?;
            match (c.status.as_str(), c.exit_code) {
                ("running", _) => Err(engine(ErrorKind::Timeout)),
                (_, Some(code)) => Ok(code),
                _ => Err(engine(ErrorKind::Timeout)),
            }
        })
    }

    fn logs_tail(&self, id: &str, _lines: usize) -> Result<String, EngineError> {
        self.call(|s, _ev| {
            find(s, id)
                .map(|c| c.logs.clone())
                .ok_or(engine(ErrorKind::Missing))
        })
    }

    fn upload_archive(&self, id: &str, path: &str, tar: Vec<u8>) -> Result<(), EngineError> {
        self.call(|s, _ev| {
            let c = find(s, id).ok_or(engine(ErrorKind::Missing))?;
            let volume = c
                .spec
                .binds
                .iter()
                .find(|b| b.is_volume() && b.target == path && !b.read_only)
                .map(|b| b.source.clone())
                .ok_or(engine(ErrorKind::Engine))?;
            let mut archive = tar::Archive::new(tar.as_slice());
            let mut files = Vec::new();
            for entry in archive.entries().map_err(|_| engine(ErrorKind::Protocol))? {
                let mut entry = entry.map_err(|_| engine(ErrorKind::Protocol))?;
                let name = entry
                    .path()
                    .map_err(|_| engine(ErrorKind::Protocol))?
                    .to_string_lossy()
                    .into_owned();
                let mode = entry
                    .header()
                    .mode()
                    .map_err(|_| engine(ErrorKind::Protocol))?;
                let mut data = Vec::new();
                entry
                    .read_to_end(&mut data)
                    .map_err(|_| engine(ErrorKind::Protocol))?;
                files.push((name, data, mode));
            }
            let target = s.volumes.entry(volume).or_default();
            for (name, data, mode) in files {
                target.files.insert(name, (data, mode));
            }
            Ok(())
        })
    }

    fn inspect_volume(&self, name: &str) -> Result<Option<Volume>, EngineError> {
        self.call(|s, _ev| {
            Ok(s.volumes.get(name).map(|v| Volume {
                name: name.into(),
                labels: v.labels.clone(),
                mountpoint: Some(mountpoint(name)),
                driver: v.driver.clone().unwrap_or_else(|| "local".into()),
            }))
        })
    }

    fn create_volume(
        &self,
        name: &str,
        labels: &BTreeMap<String, String>,
    ) -> Result<Volume, EngineError> {
        self.call(|s, _ev| {
            let v = s.volumes.entry(name.into()).or_insert_with(|| FakeVolume {
                labels: labels.clone(),
                files: BTreeMap::new(),
                driver: None,
            });
            Ok(Volume {
                name: name.into(),
                labels: v.labels.clone(),
                mountpoint: Some(mountpoint(name)),
                driver: v.driver.clone().unwrap_or_else(|| "local".into()),
            })
        })
    }

    fn inspect_network(&self, name: &str) -> Result<Option<Network>, EngineError> {
        self.call(|s, _ev| {
            Ok(s.networks.get(name).map(|labels| Network {
                name: name.into(),
                labels: labels.clone(),
            }))
        })
    }

    fn create_network(
        &self,
        name: &str,
        labels: &BTreeMap<String, String>,
    ) -> Result<Network, EngineError> {
        self.call(|s, _ev| {
            if s.networks.contains_key(name) {
                return Err(refused(
                    409,
                    &format!("network with name {name} already exists"),
                ));
            }
            s.networks.insert(name.into(), labels.clone());
            Ok(Network {
                name: name.into(),
                labels: labels.clone(),
            })
        })
    }

    fn remove_network(&self, name: &str) -> Result<(), EngineError> {
        self.call(|s, _ev| {
            let in_use = s
                .containers
                .values()
                .any(|c| c.spec.network_mode.as_deref() == Some(name));
            if in_use {
                return Err(engine(ErrorKind::Busy));
            }
            s.networks.remove(name);
            Ok(())
        })
    }

    fn remove_volume(&self, name: &str) -> Result<(), EngineError> {
        self.call(|s, _ev| {
            let in_use = s
                .containers
                .values()
                .any(|c| c.spec.binds.iter().any(|b| b.source == name));
            if in_use {
                return Err(engine(ErrorKind::Busy));
            }
            s.volumes.remove(name);
            Ok(())
        })
    }
}
