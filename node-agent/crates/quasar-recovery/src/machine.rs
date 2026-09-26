//! Machine state (architecture §5.4, `CONTEXT.md` "Machine state"): what the recovery
//! actor keeps on its machine and nowhere else, in the `quasar-machine` volume.
//!
//! ```text
//! machine.json            installation id, role, machine inputs, the images installed
//! actor.lease             the single-writer flock; never renamed or unlinked
//! secrets/                0700
//!   <name>.json           0600, one secret each
//! services/<role>.json    the last specification the actor applied for that role
//! seed.json               ADR 0007 format 1, read by the seed (`crate::seed::file`)
//! ```
//!
//! The attempt journal is written by a later slice. Every file is committed through
//! [`DurableFile`], so a crash leaves each whole or absent.

use std::collections::BTreeMap;
use std::io;
use std::os::unix::fs::DirBuilderExt;
use std::path::{Path, PathBuf};

use quasar_runtime::{DurableFile, LeaseError, StateLease};
use serde::{Deserialize, Serialize};

use crate::recipe::{ContainerSpec, ImageRef, Inputs, Role, Unknown};
use crate::seed::{self, file::SeedFile};
use crate::socket::MachineRole;

pub const FORMAT: u32 = 1;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Machine {
    pub format: u32,
    pub installation_id: String,
    pub role: MachineRole,
    pub created_at: String,
    pub inputs: Inputs,
    /// The image each role was first installed from.
    pub install_images: BTreeMap<Role, ImageRef>,
    #[serde(flatten)]
    pub unknown: Unknown,
}

/// What the actor last applied for one role: the specification's three parts and the
/// rendered result.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ServiceRecord {
    pub role: Role,
    pub recipe_revision: u32,
    pub image: ImageRef,
    pub spec_digest: String,
    pub spec: ContainerSpec,
    pub applied_at: String,
}

pub struct MachineDir {
    root: PathBuf,
}

impl MachineDir {
    pub fn new(root: impl Into<PathBuf>) -> Self {
        MachineDir { root: root.into() }
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    pub fn lease(&self) -> Result<StateLease, LeaseError> {
        StateLease::acquire(&self.root.join("actor.lease"))
    }

    pub fn machine(&self) -> DurableFile<Machine> {
        DurableFile::new(self.root.join("machine.json"), "json.tmp")
    }

    pub fn load_machine(&self) -> io::Result<Option<Machine>> {
        let mut machine = self.machine().load()?;
        if let Some(m) = &mut machine {
            // The machine's role is authoritative; state an earlier build wrote has no
            // `machine_role` in its control inputs.
            if let Some(control) = m.inputs.control.as_mut() {
                if m.role == MachineRole::ControlOnly {
                    control.machine_role = crate::recipe::ControlRole::ControlOnly;
                } else if m.role == MachineRole::Combined {
                    control.machine_role = crate::recipe::ControlRole::Combined;
                }
            }
            if m.format != FORMAT {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!(
                        "machine.json is format {}, this actor reads {FORMAT}",
                        m.format
                    ),
                ));
            }
        }
        Ok(machine)
    }

    fn seed_file(&self) -> DurableFile<SeedFile> {
        DurableFile::new(self.root.join(seed::file::FILE_NAME), "json.tmp")
    }

    /// `Ok(None)`: no actor has recorded one yet.
    pub fn load_seed_file(&self) -> io::Result<Option<SeedFile>> {
        self.seed_file().load()
    }

    pub fn store_seed_file(&self, file: &SeedFile) -> io::Result<()> {
        self.seed_file().store(file)
    }

    fn ensure_dir(&self, name: &str, mode: u32) -> io::Result<PathBuf> {
        let dir = self.root.join(name);
        match std::fs::DirBuilder::new().mode(mode).create(&dir) {
            Ok(()) => {}
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => {}
            Err(e) => return Err(e),
        }
        Ok(dir)
    }

    fn secret_file(&self, name: &str) -> io::Result<DurableFile<String>> {
        let dir = self.ensure_dir("secrets", 0o700)?;
        Ok(DurableFile::new(dir.join(format!("{name}.json")), "json.tmp").with_mode(0o600))
    }

    pub fn load_secret(&self, name: &str) -> io::Result<Option<String>> {
        let path = self.root.join("secrets").join(format!("{name}.json"));
        if !path.exists() {
            return Ok(None);
        }
        self.secret_file(name)?.load()
    }

    /// Writes only when the value differs, so an unchanged secret is never rewritten.
    pub fn store_secret(&self, name: &str, value: &str) -> io::Result<()> {
        if self.load_secret(name)?.as_deref() == Some(value) {
            return Ok(());
        }
        self.secret_file(name)?.store(&value.to_owned())
    }

    fn service_file(&self, role: Role) -> io::Result<DurableFile<ServiceRecord>> {
        let dir = self.ensure_dir("services", 0o700)?;
        Ok(DurableFile::new(
            dir.join(format!("{}.json", role.as_str())),
            "json.tmp",
        ))
    }

    pub fn load_service(&self, role: Role) -> io::Result<Option<ServiceRecord>> {
        let path = self
            .root
            .join("services")
            .join(format!("{}.json", role.as_str()));
        if !path.exists() {
            return Ok(None);
        }
        self.service_file(role)?.load()
    }

    /// Writes only when the specification differs from the recorded one.
    pub fn store_service(&self, record: &ServiceRecord) -> io::Result<()> {
        if let Some(existing) = self.load_service(record.role)? {
            if existing.spec_digest == record.spec_digest
                && existing.image == record.image
                && existing.recipe_revision == record.recipe_revision
            {
                return Ok(());
            }
        }
        self.service_file(record.role)?.store(record)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A combined machine's state as a newer actor might write it: a field this build does
    /// not know at every level.
    const NEWER: &str = r#"{
      "format": 1, "installation_id": "5f0c1e0e-0c5a-4d1b-9a2f-3e4d5c6b7a89", "role": "combined",
      "created_at": "2026-09-26T00:00:00Z", "install_images": {}, "later": {"a": [1, 2]},
      "inputs": {
        "installation_id": "5f0c1e0e-0c5a-4d1b-9a2f-3e4d5c6b7a89", "node_name": "gpu-host-01",
        "home_root": "/srv/quasar/homes", "template_root": "/srv/quasar/templates",
        "docker_socket": "/var/run/docker.sock", "later": 1,
        "gpu": {"vendor": "nvidia", "render_node": "/dev/dri/renderD128", "later": "x",
                "fallback": {"vendor": "amd", "render_node": "/dev/dri/renderD129", "later": true}},
        "devices": {"dri": true, "uinput": true, "kmsg": false, "later": null},
        "control": {"machine_role": "combined", "http_port": 8080, "tls_port": 8443,
                    "public_host": null, "tls_hosts": null, "trusted_proxies": null, "later": 2,
                    "database": {"mode": "external", "host": "db", "port": 5432, "user": "q",
                                 "name": "q", "sslmode": "require", "later": "y"}},
        "trust": {"allowed_namespaces": "ghcr.io/accreleus", "later": "z"},
        "enroll": {"later": {}},
        "app": {"puid": 1000, "later": 3}
      }
    }"#;

    /// `decide_gpus` is such a rewrite: it records one answer into loaded state.
    #[test]
    fn a_rewrite_keeps_every_field_a_newer_actor_wrote() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("machine.json"), NEWER).unwrap();
        let machines = MachineDir::new(dir.path());
        let mut m = machines.load_machine().unwrap().unwrap();
        m.inputs.gpu.gpus_served = true;
        machines.machine().store(&m).unwrap();

        let read = |raw: &[u8]| serde_json::from_slice::<serde_json::Value>(raw).unwrap();
        let mut want = read(NEWER.as_bytes());
        want["inputs"]["gpu"]["gpus_served"] = true.into();
        let got = read(&std::fs::read(dir.path().join("machine.json")).unwrap());
        assert_eq!(got, want);
    }
}
