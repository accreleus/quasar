//! The compiled actor profile (ADR 0007, seed interface 1): the container the seed creates
//! when no recovery actor exists. Spelled out here rather than rendered from `recipe`, so a
//! new recipe revision can never move it; `testdata/recovery/seed/profile-1.json` pins it,
//! and every actor must start from it (`tests/seed.rs`) and then bring its own container to
//! its recipe's shape.

use std::collections::BTreeMap;

use quasar_runtime::platform::{Bind, ContainerSpec, RestartPolicy};

use super::{file::ActorImage, INSTALLATION_LABEL, PLATFORM_SERVICE_LABEL, RECOVERY_ACTOR};

pub const NAME: &str = "quasar-recovery";
/// Where the actor reads the container that created it (ADR 0007: "the seed's own container
/// identity"): its install inputs on a clean machine, and the seed it reports.
pub const SEED_CONTAINER_ENV: &str = "QUASAR_SEED_CONTAINER";
pub const MACHINE_VOLUME: &str = "quasar-machine";
pub const MACHINE_DIR: &str = "/var/lib/quasar-machine";
pub const AGENT_SOCKET_VOLUME: &str = "quasar-recovery-agent";
pub const AGENT_SOCKET_DIR: &str = "/run/quasar-recovery";
pub const ENGINE_SOCKET: &str = "/var/run/docker.sock";

/// `engine_socket` is the daemon-host path of the socket the seed itself was given.
pub fn actor(
    installation_id: &str,
    image: &ActorImage,
    engine_socket: &str,
    seed_container: &str,
) -> ContainerSpec {
    let bind = |source: &str, target: &str| Bind {
        source: source.into(),
        target: target.into(),
        read_only: false,
    };
    ContainerSpec {
        name: NAME.into(),
        image: image.reference(),
        entrypoint: None,
        cmd: Some(vec!["actor".into()]),
        env: BTreeMap::from([
            ("RUST_LOG".to_string(), "info".to_string()),
            (SEED_CONTAINER_ENV.to_string(), seed_container.to_string()),
        ]),
        labels: BTreeMap::from([
            (INSTALLATION_LABEL.to_string(), installation_id.to_string()),
            (
                PLATFORM_SERVICE_LABEL.to_string(),
                RECOVERY_ACTOR.to_string(),
            ),
        ]),
        network_mode: None,
        binds: vec![
            bind(AGENT_SOCKET_VOLUME, AGENT_SOCKET_DIR),
            bind(MACHINE_VOLUME, MACHINE_DIR),
            bind(engine_socket, ENGINE_SOCKET),
        ],
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: Vec::new(),
        cap_add: Vec::new(),
        // The engine socket on an SELinux host.
        security_opt: vec!["label=disable".into()],
        init: false,
        restart: RestartPolicy::UnlessStopped,
    }
}
