//! Installing a combined or control-only machine (#361): Postgres (or the operator's own
//! database), the control plane and, on a combined host, its own node agent, in that
//! order. Like the GPU-host install in [`crate::actor`], every step is decided by what the
//! engine and machine state already hold, so a second `resume` changes nothing and an
//! interrupted install completes on the next.

use std::time::{Duration, Instant};

use base64::Engine as _;
use tracing::{info, warn};

use crate::actor::{image_revision, Actor, FileOwner, ResumeError};
use crate::bootstrap::CheckedControl;
use crate::machine::Machine;
use crate::recipe::{
    self, control, names, secrets, DatabaseInputs, ImageRef, Role, SecretMounts, CONTROL_PLANE_UID,
};
use crate::socket::MachineRole;

/// Postgres reads its password file as its own user (uid 70 in the alpine image, 999 in
/// the Debian one) after the entrypoint drops root, so the file must be readable by any
/// uid. The volume is mounted into that one container only.
const POSTGRES_FILES: FileOwner = FileOwner {
    uid: 0,
    gid: 0,
    mode: 0o444,
};

const CONTROL_PLANE_FILES: FileOwner = FileOwner {
    uid: CONTROL_PLANE_UID,
    gid: CONTROL_PLANE_UID,
    mode: 0o400,
};

fn random_bytes() -> [u8; 32] {
    use ring::rand::{SecureRandom, SystemRandom};
    let mut b = [0u8; 32];
    SystemRandom::new()
        .fill(&mut b)
        .expect("the system random source");
    b
}

/// 64 hex characters: safe unquoted in a URL, a shell and `pg_hba`.
fn generate_password() -> String {
    random_bytes().iter().map(|b| format!("{b:02x}")).collect()
}

/// `QUASAR_SECRET_KEY`'s form: base64 of exactly 32 bytes.
fn generate_secret_key() -> String {
    base64::engine::general_purpose::STANDARD.encode(random_bytes())
}

/// The form of an admin-minted enrollment token (base64url of 32 bytes).
fn generate_token() -> String {
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(random_bytes())
}

impl Actor {
    /// The secrets a first install of a control-plane machine writes before machine state
    /// exists. A generated secret already present (from an install interrupted before its
    /// machine state was written) is kept: Postgres may already have initialised its data
    /// with it.
    pub(crate) fn store_control_secrets(
        &self,
        role: MachineRole,
        control: &CheckedControl,
    ) -> Result<(), ResumeError> {
        match &control.database_password {
            Some(operator) => self
                .dir
                .store_secret(secrets::DATABASE_PASSWORD, operator)?,
            None => self.ensure_generated(secrets::DATABASE_PASSWORD, generate_password)?,
        }
        self.ensure_generated(secrets::SECRET_KEY, generate_secret_key)?;
        if role == MachineRole::Combined {
            self.ensure_generated(secrets::LOCAL_ENROLLMENT, generate_token)?;
        }
        Ok(())
    }

    fn ensure_generated(&self, name: &str, generate: fn() -> String) -> Result<(), ResumeError> {
        if self.dir.load_secret(name)?.is_none() {
            self.dir.store_secret(name, &generate())?;
            info!(secret = name, "generated");
        }
        Ok(())
    }

    /// The daemon-host path of the socket volume this actor mounts, whose subdirectories are
    /// bound into the node agent and the control plane.
    pub(crate) fn socket_volume_host_path(&self) -> Result<String, ResumeError> {
        let volume = names::AGENT_SOCKET_VOLUME;
        match self.engine.inspect_volume(volume)? {
            Some(v) => v.mountpoint.ok_or_else(|| {
                ResumeError::Inputs(format!(
                    "the {volume} volume reports no host path, so its socket directories cannot be given to the control plane and the agent; a combined or control-only machine needs the engine's local volume driver"
                ))
            }),
            None => Err(ResumeError::Inputs(format!(
                "the {volume} volume does not exist: the recovery actor must mount it at {}",
                recipe::paths::AGENT_SOCKET_DIR
            ))),
        }
    }

    pub(crate) fn ensure_control_machine(&self, machine: &Machine) -> Result<(), ResumeError> {
        let control = machine.inputs.control.as_ref().ok_or_else(|| {
            ResumeError::Inputs("machine state names no control-plane inputs".into())
        })?;
        self.ensure_network(machine)?;
        if control.database == DatabaseInputs::Owned {
            self.ensure_volume(machine, names::POSTGRES_DATA_VOLUME, Role::Postgres)?;
            self.ensure_volume(machine, names::POSTGRES_SECRETS_VOLUME, Role::Postgres)?;
            let secrets = SecretMounts {
                volume: Some(names::POSTGRES_SECRETS_VOLUME.into()),
                files: [secrets::DATABASE_PASSWORD.to_string()].into(),
            };
            self.ensure_service(machine, Role::Postgres, &secrets, POSTGRES_FILES)?;
            // Not yet ready delays the control plane rather than failing it.
            self.await_healthy(names::POSTGRES);
        }
        self.ensure_volume(machine, names::CONTROL_DATA_VOLUME, Role::ControlPlane)?;
        self.ensure_volume(
            machine,
            names::CONTROL_PLANE_SECRETS_VOLUME,
            Role::ControlPlane,
        )?;
        self.ensure_service(
            machine,
            Role::ControlPlane,
            &self.control_plane_secrets()?,
            CONTROL_PLANE_FILES,
        )?;
        if machine.role == MachineRole::Combined {
            // Its agent enrolls with the local token, which the control plane inserts
            // when it boots.
            self.await_healthy(names::CONTROL_PLANE);
            self.ensure_node_agent(machine)?;
        }
        Ok(())
    }

    fn control_plane_secrets(&self) -> Result<SecretMounts, ResumeError> {
        let mut files = std::collections::BTreeSet::new();
        for name in [
            secrets::DATABASE_PASSWORD,
            secrets::SECRET_KEY,
            secrets::LOCAL_ENROLLMENT,
        ] {
            if self.dir.load_secret(name)?.is_some() {
                files.insert(name.to_string());
            }
        }
        Ok(SecretMounts {
            volume: Some(names::CONTROL_PLANE_SECRETS_VOLUME.into()),
            files,
        })
    }

    fn ensure_network(&self, machine: &Machine) -> Result<(), ResumeError> {
        let name = names::PLATFORM_NETWORK;
        match self.engine.inspect_network(name)? {
            Some(n) => match n.labels.get(recipe::labels::INSTALLATION) {
                Some(id) if *id == machine.installation_id => Ok(()),
                _ => Err(ResumeError::OwnerConflict(format!(
                    "network {name} exists and is not this installation's; it is left untouched. Remove it (docker network rm {name}) to let the install continue"
                ))),
            },
            None => {
                self.engine
                    .create_network(name, &self.owned_labels(machine, Role::ControlPlane))?;
                info!(network = name, "network created");
                Ok(())
            }
        }
    }

    fn service_image(&self, machine: &Machine, role: Role) -> Result<ImageRef, ResumeError> {
        match self.dir.load_service(role)? {
            Some(record) => Ok(record.image),
            None => machine.install_images.get(&role).cloned().ok_or_else(|| {
                ResumeError::Inputs(format!("machine state names no {} image", role.as_str()))
            }),
        }
    }

    /// Create and start `role`'s container if it does not exist; start one an interrupted
    /// install created; leave any other alone. One whose specification differs from what
    /// this actor renders is reported and left as it is: replacing is not an install.
    fn ensure_service(
        &self,
        machine: &Machine,
        role: Role,
        secrets: &SecretMounts,
        owner: FileOwner,
    ) -> Result<(), ResumeError> {
        let image = self.service_image(machine, role)?;
        if let Some(existing) = self.engine.inspect_container(role.container_name())? {
            if !self.is_ours(machine, &existing, role) {
                return Err(ResumeError::OwnerConflict(format!(
                    "container {} ({}) is not this installation's; it is left untouched",
                    existing.name, existing.image
                )));
            }
            let revision = existing
                .labels
                .get(recipe::labels::RECIPE)
                .and_then(|r| r.parse().ok())
                .unwrap_or(0);
            let spec = recipe::render(role, revision, &machine.inputs, &image, secrets)?;
            if existing.labels.get(recipe::labels::SPEC) != spec.labels.get(recipe::labels::SPEC) {
                warn!(
                    token = "actor-spec-differs",
                    container = %existing.name,
                    "the running {} differs from what this actor renders; it is left as it is",
                    role.as_str()
                );
                return Ok(());
            }
            if existing.status == "created" {
                info!(container = %existing.name, "starting what an interrupted install created");
                self.engine.start_container(&existing.id)?;
            }
            return self.record(role, revision, &image, spec);
        }

        let found = self.ensure_image(&image)?;
        let revision = match role {
            Role::Postgres => control::POSTGRES_REVISION,
            _ => image_revision(&found, &image)?,
        };
        let spec = recipe::render(role, revision, &machine.inputs, &image, secrets)?;
        if let Some(volume) = &secrets.volume {
            self.deliver_secrets_as(&image, volume, &secrets.files, owner)?;
        }
        let id = self.engine.create_container(&spec)?;
        self.engine.start_container(&id)?;
        info!(container = role.container_name(), image = %image.reference(), revision, "created and started");
        self.record(role, revision, &image, spec)
    }

    /// Waits, bounded by `healthy_wait`, for `name` to run and report healthy (or run, with
    /// no healthcheck). Never fails: what depends on it is created either way, and retries
    /// or restarts on its own.
    fn await_healthy(&self, name: &str) {
        let deadline = Instant::now() + self.config.healthy_wait;
        let mut last;
        loop {
            match self.engine.inspect_container(name) {
                Ok(Some(c)) => {
                    match (c.running, c.health.as_deref()) {
                        (true, Some("healthy")) | (true, None) => return,
                        _ => {}
                    }
                    last = format!(
                        "state={} health={}",
                        c.status,
                        c.health.as_deref().unwrap_or("none")
                    );
                }
                Ok(None) => last = "missing".into(),
                Err(e) => last = e.to_string(),
            }
            if Instant::now() >= deadline {
                warn!(
                    token = "actor-dependency-not-healthy",
                    container = name,
                    "{name} did not report healthy within {}s ({last}); continuing, and what depends on it waits on its own",
                    self.config.healthy_wait.as_secs()
                );
                return;
            }
            std::thread::sleep(self.config.timing.poll.min(Duration::from_secs(5)));
        }
    }
}
