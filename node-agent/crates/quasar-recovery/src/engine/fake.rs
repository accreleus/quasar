//! The in-memory adapter: an engine the actor's tests drive and observe. Faults are
//! injected by call index over every call (reads included), before or after the call
//! takes effect; [`EngineError::Crashed`] stands for the actor process dying there.

use std::collections::{BTreeMap, BTreeSet};
use std::io::Read;
use std::sync::Mutex;
use std::time::Duration;

use super::{
    Container, ContainerSpec, EngineError, EngineHost, ErrorKind, Image, PlatformEngine,
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

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct FakeVolume {
    pub labels: BTreeMap<String, String>,
    /// Path inside the volume → (content, mode).
    pub files: BTreeMap<String, (Vec<u8>, u32)>,
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
    pub host: EngineHost,
    /// Device nodes the host has; creating a container naming another fails.
    pub host_devices: BTreeSet<String>,
    /// What a started GPU probe prints.
    pub probe_output: String,
    /// Whether the engine can start a container that requests GPUs (`--gpus all`); when
    /// not, the start is refused with Docker's device-driver message.
    pub gpus_supported: bool,
    /// Failures the next creates of a GPU-requesting container return, in order.
    pub gpus_create_failures: Vec<EngineError>,
    pub unreachable: bool,
    pub next_id: u64,
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

#[derive(Default)]
struct Inner {
    state: FakeState,
    calls: usize,
    faults: Vec<Fault>,
}

#[derive(Default)]
pub struct FakeEngine {
    inner: Mutex<Inner>,
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
            }),
        }
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

    /// Runs one call: counts it, applies a `Before` fault, the operation, then an
    /// `After` fault.
    fn call<T>(
        &self,
        op: impl FnOnce(&mut FakeState) -> Result<T, EngineError>,
    ) -> Result<T, EngineError> {
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
            return Err(error);
        }
        let after = fault(When::After);
        if inner.state.unreachable {
            return Err(EngineError::Runtime(ErrorKind::Unavailable));
        }
        let result = op(&mut inner.state);
        match after {
            Some(error) => Err(error),
            None => result,
        }
    }
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
}

fn find_id(state: &FakeState, name_or_id: &str) -> Result<String, EngineError> {
    find(state, name_or_id)
        .map(|c| c.id.clone())
        .ok_or(engine(ErrorKind::Missing))
}

fn view(c: &FakeContainer) -> Container {
    Container {
        id: c.id.clone(),
        name: c.spec.name.clone(),
        image: c.spec.image.clone(),
        image_id: format!("sha256:image-of-{}", c.spec.image),
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
    }
}

impl PlatformEngine for FakeEngine {
    fn host(&self) -> Result<EngineHost, EngineError> {
        self.call(|s| Ok(s.host.clone()))
    }

    fn inspect_image(&self, reference: &str) -> Result<Option<Image>, EngineError> {
        self.call(|s| {
            Ok(s.images
                .get(reference)
                .or_else(|| s.images.values().find(|i| i.id == reference))
                .cloned())
        })
    }

    fn pull(&self, reference: &str) -> Result<(), EngineError> {
        self.call(|s| {
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
        self.call(|s| Ok(find(s, name_or_id).map(view)))
    }

    fn list_containers(&self) -> Result<Vec<Container>, EngineError> {
        self.call(|s| {
            let mut all: Vec<Container> = s.containers.values().map(view).collect();
            all.sort_by(|a, b| a.name.cmp(&b.name));
            Ok(all)
        })
    }

    fn create_container(&self, spec: &ContainerSpec) -> Result<String, EngineError> {
        self.call(|s| {
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
        self.call(|s| {
            let id = find_id(s, id)?;
            let output = s.probe_output.clone();
            let gpus_supported = s.gpus_supported;
            let c = s.containers.get_mut(&id).unwrap();
            if c.status == "running" {
                return Ok(());
            }
            if !c.spec.gpus.is_empty() && !gpus_supported {
                return Err(refused(
                    500,
                    "could not select device driver \"\" with capabilities: [[gpu]]",
                ));
            }
            c.starts += 1;
            // A helper runs to completion at once; only the GPU probe prints anything.
            if let Some(helper) = c.spec.labels.get(HELPER_LABEL) {
                c.status = "exited".into();
                c.exit_code = Some(0);
                if helper == PROBE_HELPER {
                    c.logs = output;
                }
            } else {
                c.status = "running".into();
            }
            Ok(())
        })
    }

    fn stop_container(&self, id: &str, _grace: Duration) -> Result<(), EngineError> {
        self.call(|s| {
            let id = find_id(s, id)?;
            let c = s.containers.get_mut(&id).unwrap();
            if c.status == "running" {
                c.status = "exited".into();
                c.exit_code = Some(0);
            }
            Ok(())
        })
    }

    fn set_restart_policy(&self, id: &str, policy: RestartPolicy) -> Result<(), EngineError> {
        self.call(|s| {
            let id = find_id(s, id)?;
            s.containers.get_mut(&id).unwrap().restart = policy;
            Ok(())
        })
    }

    fn rename_container(&self, id: &str, name: &str) -> Result<(), EngineError> {
        self.call(|s| {
            if s.container_named(name).is_some() {
                return Err(engine(ErrorKind::Engine));
            }
            let id = find_id(s, id)?;
            s.containers.get_mut(&id).unwrap().spec.name = name.into();
            Ok(())
        })
    }

    fn remove_container(&self, id: &str) -> Result<(), EngineError> {
        self.call(|s| {
            if let Ok(id) = find_id(s, id) {
                s.containers.remove(&id);
            }
            Ok(())
        })
    }

    fn wait_container(&self, id: &str, _timeout: Duration) -> Result<i64, EngineError> {
        self.call(|s| {
            let c = find(s, id).ok_or(engine(ErrorKind::Missing))?;
            match (c.status.as_str(), c.exit_code) {
                ("running", _) => Err(engine(ErrorKind::Timeout)),
                (_, Some(code)) => Ok(code),
                _ => Err(engine(ErrorKind::Timeout)),
            }
        })
    }

    fn logs_tail(&self, id: &str, _lines: usize) -> Result<String, EngineError> {
        self.call(|s| {
            find(s, id)
                .map(|c| c.logs.clone())
                .ok_or(engine(ErrorKind::Missing))
        })
    }

    fn upload_archive(&self, id: &str, path: &str, tar: Vec<u8>) -> Result<(), EngineError> {
        self.call(|s| {
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
        self.call(|s| {
            Ok(s.volumes.get(name).map(|v| Volume {
                name: name.into(),
                labels: v.labels.clone(),
            }))
        })
    }

    fn create_volume(
        &self,
        name: &str,
        labels: &BTreeMap<String, String>,
    ) -> Result<Volume, EngineError> {
        self.call(|s| {
            let v = s.volumes.entry(name.into()).or_insert_with(|| FakeVolume {
                labels: labels.clone(),
                files: BTreeMap::new(),
            });
            Ok(Volume {
                name: name.into(),
                labels: v.labels.clone(),
            })
        })
    }

    fn remove_volume(&self, name: &str) -> Result<(), EngineError> {
        self.call(|s| {
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
