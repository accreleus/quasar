//! The commands this crate tells an operator to type, spelled for the engine the install
//! runs on: the CLI that matches it (`docker` or `podman`) and the engine socket the
//! install recorded, which on a rootless engine is not `/var/run/docker.sock`.

use quasar_runtime::EngineKind;

use crate::engine::PlatformEngine;
use crate::machine::Machine;
use crate::recipe::{names, paths};

/// Stands in for the recovery image when none can be told: after an uninstall no
/// `quasar-recovery` container is left to inspect.
pub const IMAGE_PLACEHOLDER: &str = "<registry>/quasar-recovery@sha256:<digest>";

pub struct Cli {
    name: &'static str,
    socket: String,
}

impl Cli {
    /// `socket` is the daemon-host path the install recorded (`Inputs::docker_socket`).
    /// An engine this crate cannot name is spelled `docker`, as before.
    pub fn new(kind: EngineKind, socket: &str) -> Self {
        Cli {
            name: if kind == EngineKind::Podman {
                "podman"
            } else {
                "docker"
            },
            socket: socket.to_owned(),
        }
    }

    /// The engine this machine's install runs on, as the engine reports itself, and the
    /// socket its machine state recorded. An engine that does not answer reads as Docker.
    pub fn for_machine(engine: &dyn PlatformEngine, machine: &Machine) -> Self {
        let kind = engine.host().map(|h| h.kind).unwrap_or_default();
        Cli::new(kind, &machine.inputs.docker_socket)
    }

    /// The same when no machine state is known: only the CLI's name is right, and the
    /// socket is the rootful default.
    pub fn for_engine(engine: &dyn PlatformEngine) -> Self {
        let kind = engine.host().map(|h| h.kind).unwrap_or_default();
        Cli::new(kind, paths::ENGINE_SOCKET)
    }

    /// The engine CLI's name.
    pub fn name(&self) -> &'static str {
        self.name
    }

    /// `command` run inside the recovery actor's container.
    pub fn exec(&self, command: &str) -> String {
        format!(
            "{} exec {} quasar-recovery {command}",
            self.name,
            names::RECOVERY_ACTOR
        )
    }

    /// The same with `-i`, to pass a file on standard input.
    pub fn exec_stdin(&self, command: &str) -> String {
        format!(
            "{} exec -i {} quasar-recovery {command}",
            self.name,
            names::RECOVERY_ACTOR
        )
    }

    /// `command` run in its own container on the recovery image, beside the actor: the
    /// way to run what stops the actor itself (`uninstall`). `image` is printed as it is.
    pub fn beside(&self, image: &str, command: &str) -> String {
        // On every engine, as the seed's own command: required on SELinux, harmless elsewhere.
        format!(
            "{} run --rm -it --security-opt label=disable -v {}:{} -v {}:{} {image} {command}",
            self.name,
            self.socket,
            paths::ENGINE_SOCKET,
            names::MACHINE_VOLUME,
            paths::MACHINE_DIR,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const IMAGE: &str = "r.example/quasar-recovery@sha256:abc";

    #[test]
    fn rootful_docker_names_the_default_socket() {
        let c = Cli::new(EngineKind::Docker, "/var/run/docker.sock");
        assert_eq!(
            c.beside(IMAGE, "uninstall --purge"),
            format!("docker run --rm -it --security-opt label=disable -v /var/run/docker.sock:/var/run/docker.sock -v quasar-machine:{} {IMAGE} uninstall --purge", paths::MACHINE_DIR)
        );
        assert_eq!(
            c.exec("status"),
            "docker exec quasar-recovery quasar-recovery status"
        );
    }

    #[test]
    fn rootless_docker_names_the_recorded_socket() {
        let line =
            Cli::new(EngineKind::Docker, "/run/user/1001/docker.sock").beside(IMAGE, "uninstall");
        assert!(
            line.starts_with(
                "docker run --rm -it --security-opt label=disable -v /run/user/1001/docker.sock:/var/run/docker.sock "
            ),
            "{line}"
        );
        assert!(
            !line.contains("/var/run/docker.sock:/var/run/docker.sock"),
            "{line}"
        );
    }

    #[test]
    fn podman_uses_podman_and_its_own_socket() {
        let c = Cli::new(EngineKind::Podman, "/run/user/1001/podman/podman.sock");
        let line = c.beside(IMAGE, "uninstall");
        assert!(line.starts_with("podman run --rm -it --security-opt label=disable -v /run/user/1001/podman/podman.sock:/var/run/docker.sock "), "{line}");
        assert!(!line.contains("docker run"), "{line}");
        assert!(c.exec("restore --list").starts_with("podman exec "));
    }

    #[test]
    fn rootful_podman_names_its_own_socket() {
        let line =
            Cli::new(EngineKind::Podman, "/run/podman/podman.sock").beside(IMAGE, "uninstall");
        assert!(line.starts_with("podman run --rm -it --security-opt label=disable -v /run/podman/podman.sock:/var/run/docker.sock "), "{line}");
    }
}
