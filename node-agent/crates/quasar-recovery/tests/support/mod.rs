//! Fixtures shared by the actor tests: a GPU host as the in-memory engine sees it, and the
//! hand-started recovery actor already running on it.
#![allow(dead_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;
use std::sync::Arc;

use quasar_recovery::actor::{Actor, ActorConfig, OperatorInputs};
use quasar_recovery::engine::{
    ContainerSpec, EngineHost, FakeContainer, FakeEngine, FakeState, FakeVolume, Image,
    RestartPolicy,
};
use quasar_recovery::recipe::{names, paths, Bind};
use quasar_recovery::seed::{Seed, SeedConfig};
use quasar_recovery::socket::MachineRole;

pub const AGENT_IMAGE: &str = "registry.example.invalid/quasar/quasar-node-agent@sha256:bb22000000000000000000000000000000000000000000000000000000000000";
pub const ACTOR_IMAGE: &str = "registry.example.invalid/quasar/quasar-recovery@sha256:cc33000000000000000000000000000000000000000000000000000000000000";
pub const ACTOR_ID: &str = "ac00000000000000000000000000000000000000000000000000000000000000";
pub const INSTALLATION: &str = "5f0c1e0e-0c5a-4d1b-9a2f-3e4d5c6b7a89";
pub const NOW: &str = "2026-09-25T10:00:00Z";
pub const ENROLLMENT: &str = "qenr1..d3NzOi8vY3AuZXhhbXBsZS5pbnZhbGlkOjg0NDM.tok-4f2a9c";
pub const HOME: &str = "/srv/quasar/homes";
pub const SOCKET_HOST_PATH: &str = "/run/user-docker/docker.sock";

pub const PROBE_AMD: &str =
    "quasar-probe 1\ndev uinput\ndev kmsg\nnode /dev/dri/renderD129 226:129 0x1002\nend\n";
pub const PROBE_NVIDIA: &str = "quasar-probe 1\ndev uinput\ndev kmsg\ndev nvidiactl\nnode /dev/dri/renderD128 226:128 0x10de\nend\n";
pub const PROBE_NONE: &str = "quasar-probe 1\nend\n";

pub fn agent_image(recipe_label: Option<&str>) -> Image {
    Image {
        id: "sha256:a9e1000000000000000000000000000000000000000000000000000000000000".into(),
        repo_digests: vec![AGENT_IMAGE.into()],
        labels: recipe_label
            .map(|r| BTreeMap::from([("org.quasar.recipe".to_string(), r.to_string())]))
            .unwrap_or_default(),
    }
}

/// A GPU host with the hand-started actor on it: the actor's own container (engine socket
/// bound from a non-default host path), its image, and the agent image in the registry.
/// `gpus_served`: whether the engine starts a container that requests `--gpus all`.
pub fn host(
    probe_output: &str,
    runtimes: &[&str],
    gpus_served: bool,
    devices: &[&str],
) -> FakeState {
    let mut state = FakeState {
        host: EngineHost {
            name: Some("gpu-host-01".into()),
            runtimes: runtimes.iter().map(|r| r.to_string()).collect(),
            cdi_devices: Vec::new(),
        },
        host_devices: devices
            .iter()
            .map(|d| d.to_string())
            .collect::<BTreeSet<_>>(),
        probe_output: probe_output.into(),
        gpus_supported: gpus_served,
        ..Default::default()
    };
    state
        .registry
        .insert(AGENT_IMAGE.into(), agent_image(Some("1")));
    state.images.insert(
        ACTOR_IMAGE.into(),
        Image {
            id: "sha256:ac1a000000000000000000000000000000000000000000000000000000000000".into(),
            repo_digests: vec![ACTOR_IMAGE.into()],
            labels: BTreeMap::new(),
        },
    );
    let spec = ContainerSpec {
        name: names::RECOVERY_ACTOR.into(),
        image: ACTOR_IMAGE.into(),
        entrypoint: None,
        cmd: Some(vec!["actor".into()]),
        env: BTreeMap::new(),
        labels: BTreeMap::new(),
        network_mode: None,
        binds: vec![
            Bind {
                source: SOCKET_HOST_PATH.into(),
                target: paths::ENGINE_SOCKET.into(),
                read_only: false,
            },
            Bind {
                source: names::MACHINE_VOLUME.into(),
                target: paths::MACHINE_DIR.into(),
                read_only: false,
            },
            Bind {
                source: names::AGENT_SOCKET_VOLUME.into(),
                target: paths::AGENT_SOCKET_DIR.into(),
                read_only: false,
            },
        ],
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: Vec::new(),
        cap_add: Vec::new(),
        security_opt: Vec::new(),
        init: false,
        restart: RestartPolicy::UnlessStopped,
    };
    for volume in [names::MACHINE_VOLUME, names::AGENT_SOCKET_VOLUME] {
        state.volumes.insert(volume.into(), FakeVolume::default());
    }
    state.containers.insert(
        ACTOR_ID.into(),
        FakeContainer {
            id: ACTOR_ID.into(),
            spec,
            status: "running".into(),
            starts: 1,
            restart: RestartPolicy::UnlessStopped,
            exit_code: None,
            logs: String::new(),
            health: None,
        },
    );
    state
}

pub fn amd_host() -> FakeState {
    host(
        PROBE_AMD,
        &["runc"],
        false,
        &["/dev/dri", "/dev/uinput", "/dev/kmsg"],
    )
}

pub fn nvidia_host(runtimes: &[&str], gpus_served: bool) -> FakeState {
    host(
        PROBE_NVIDIA,
        runtimes,
        gpus_served,
        &["/dev/dri", "/dev/uinput", "/dev/kmsg"],
    )
}

pub fn operator() -> OperatorInputs {
    OperatorInputs {
        enrollment: Some(ENROLLMENT.into()),
        home_root: Some(HOME.into()),
        template_root: None,
        node_name: None,
        agent_image: Some(AGENT_IMAGE.into()),
    }
}

pub fn actor(engine: &Arc<FakeEngine>, dir: &Path, operator: OperatorInputs) -> Actor {
    let mut config = ActorConfig::new(dir, MachineRole::Gpu, operator);
    config.self_container = Some(ACTOR_ID.into());
    config.new_installation_id = Box::new(|| INSTALLATION.to_string());
    config.now = Box::new(|| NOW.to_string());
    config.gpus_probe_backoff = std::time::Duration::ZERO;
    Actor::new(engine.clone(), config)
}

/// Every file under the machine-state directory: path → (content, mode, modified).
pub fn tree(dir: &Path) -> BTreeMap<String, (Vec<u8>, u32, std::time::SystemTime)> {
    use std::os::unix::fs::PermissionsExt;
    let mut out = BTreeMap::new();
    let mut todo = vec![dir.to_path_buf()];
    while let Some(d) = todo.pop() {
        for entry in std::fs::read_dir(&d).unwrap() {
            let path = entry.unwrap().path();
            let meta = std::fs::metadata(&path).unwrap();
            let key = path.strip_prefix(dir).unwrap().display().to_string();
            if meta.is_dir() {
                out.insert(
                    key,
                    (
                        Vec::new(),
                        meta.permissions().mode() & 0o777,
                        meta.modified().unwrap(),
                    ),
                );
                todo.push(path);
            } else {
                out.insert(
                    key,
                    (
                        std::fs::read(&path).unwrap(),
                        meta.permissions().mode() & 0o777,
                        meta.modified().unwrap(),
                    ),
                );
            }
        }
    }
    out
}

pub const SEED_ID: &str = "5eed000000000000000000000000000000000000000000000000000000000000";
pub const SEED_NAME: &str = "quasar-seed";
pub const SEED_VERSION: &str = "0.6.0";
/// A later recovery image a manager may update the seed to.
pub const NEWER_IMAGE: &str = "registry.example.invalid/quasar/quasar-recovery@sha256:dd44000000000000000000000000000000000000000000000000000000000000";

/// The bootstrap inputs of a GPU host's seed, as its `docker run -e …` gives them.
pub fn seed_env() -> BTreeMap<String, String> {
    BTreeMap::from([
        ("QUASAR_ROLE".to_string(), "gpu".to_string()),
        ("QUASAR_ENROLLMENT".to_string(), ENROLLMENT.to_string()),
        ("QUASAR_HOME_ROOT".to_string(), HOME.to_string()),
        ("QUASAR_AGENT_IMAGE".to_string(), AGENT_IMAGE.to_string()),
    ])
}

/// A running seed container as the documented `docker run` makes it.
pub fn seed_container(id: &str, image: &str, env: BTreeMap<String, String>) -> FakeContainer {
    FakeContainer {
        id: id.into(),
        spec: ContainerSpec {
            name: SEED_NAME.into(),
            image: image.into(),
            entrypoint: Some(vec!["/usr/local/bin/quasar-recovery".into()]),
            cmd: Some(vec!["seed".into()]),
            env,
            labels: BTreeMap::new(),
            network_mode: None,
            binds: vec![
                Bind {
                    source: SOCKET_HOST_PATH.into(),
                    target: paths::ENGINE_SOCKET.into(),
                    read_only: false,
                },
                Bind {
                    source: names::MACHINE_VOLUME.into(),
                    target: paths::MACHINE_DIR.into(),
                    read_only: true,
                },
            ],
            devices: Vec::new(),
            device_cgroup_rules: Vec::new(),
            gpus: Vec::new(),
            cap_add: Vec::new(),
            security_opt: Vec::new(),
            init: false,
            restart: RestartPolicy::UnlessStopped,
        },
        status: "running".into(),
        starts: 1,
        restart: RestartPolicy::UnlessStopped,
        exit_code: None,
        logs: String::new(),
        health: None,
    }
}

/// A clean AMD GPU host on which only the seed has been started: no recovery actor yet.
/// The recovery image carries its release version label.
pub fn seeded_host(env: BTreeMap<String, String>) -> FakeState {
    let mut state = amd_host();
    state.containers.remove(ACTOR_ID);
    state
        .images
        .get_mut(ACTOR_IMAGE)
        .unwrap()
        .labels
        .insert("org.quasar.version".into(), SEED_VERSION.into());
    state
        .containers
        .insert(SEED_ID.into(), seed_container(SEED_ID, ACTOR_IMAGE, env));
    state
}

pub fn seed(engine: &Arc<FakeEngine>, dir: &Path, self_id: &str) -> Seed {
    let mut config = SeedConfig::new(dir);
    config.self_container = Some(self_id.into());
    config.new_installation_id = Box::new(|| INSTALLATION.to_string());
    Seed::new(engine.clone(), config)
}

/// The recovery actor a seed created, running as that container: no inputs of its own.
pub fn seeded_actor(engine: &Arc<FakeEngine>, dir: &Path, actor_id: &str) -> Actor {
    let mut config = ActorConfig::new(dir, MachineRole::Gpu, OperatorInputs::default());
    config.self_container = Some(actor_id.into());
    config.seed_container = Some(SEED_ID.into());
    config.new_installation_id = Box::new(|| "an-id-the-actor-must-not-use".to_string());
    config.now = Box::new(|| NOW.to_string());
    config.gpus_probe_backoff = std::time::Duration::ZERO;
    Actor::new(engine.clone(), config)
}

/// The tree without modification times, for comparing two machines that reached the same
/// state along different paths.
pub fn contents(dir: &Path) -> BTreeMap<String, (Vec<u8>, u32)> {
    tree(dir)
        .into_iter()
        .map(|(k, (bytes, mode, _))| (k, (bytes, mode)))
        .collect()
}
