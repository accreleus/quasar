//! The bootstrap inputs: what an operator writes in the seed's `docker run` line or stack
//! (`docs/configuration.md` "Seed"). The seed checks them before it creates anything; the
//! actor it creates reads the same variables from the seed's container and installs from
//! them, once, on a clean machine. One parser and one check serve both, so a seed never
//! accepts inputs its actor would refuse.

use crate::actor::OperatorInputs;
use crate::recipe::{self, ImageRef, Inputs};
use crate::socket::MachineRole;

pub const ROLE: &str = "QUASAR_ROLE";
pub const ENROLLMENT: &str = "QUASAR_ENROLLMENT";
pub const HOME_ROOT: &str = "QUASAR_HOME_ROOT";
pub const TEMPLATE_ROOT: &str = "QUASAR_TEMPLATE_ROOT";
pub const NODE_NAME: &str = "QUASAR_NODE_NAME";
pub const AGENT_IMAGE: &str = "QUASAR_AGENT_IMAGE";

#[derive(Debug, Clone)]
pub struct Bootstrap {
    pub role: MachineRole,
    pub operator: OperatorInputs,
}

/// Only `gpu` installs in this build; the combined and control-only roles arrive with #361.
pub fn parse_role(raw: &str) -> Result<MachineRole, String> {
    match raw {
        "gpu" => Ok(MachineRole::Gpu),
        "combined" | "control-only" => Err(format!(
            "{ROLE}={raw} is not installed by this build (combined and control-only machines arrive with RH06-09, #361); use gpu"
        )),
        other => Err(format!("{ROLE}={other:?} is not a role; this build installs gpu")),
    }
}

impl Bootstrap {
    /// From `KEY=value` pairs, as `Config.Env` holds them. Blank values are unset. Fails
    /// only on an unknown role; everything else is [`Bootstrap::check`]'s.
    pub fn from_env<S: AsRef<str>>(env: &[S]) -> Result<Bootstrap, String> {
        let get = |key: &str| {
            env.iter().rev().find_map(|kv| {
                let (k, v) = kv.as_ref().split_once('=')?;
                (k == key && !v.trim().is_empty()).then(|| v.to_string())
            })
        };
        Ok(Bootstrap {
            role: parse_role(get(ROLE).as_deref().unwrap_or("gpu"))?,
            operator: OperatorInputs {
                enrollment: get(ENROLLMENT),
                home_root: get(HOME_ROOT),
                template_root: get(TEMPLATE_ROOT),
                node_name: get(NODE_NAME),
                agent_image: get(AGENT_IMAGE),
            },
        })
    }

    pub fn from_process_env() -> Result<Bootstrap, String> {
        let env: Vec<String> = std::env::vars().map(|(k, v)| format!("{k}={v}")).collect();
        Bootstrap::from_env(&env)
    }

    /// Everything a first install needs, checked before anything is created or written.
    /// `host_name` is the engine host's, the node name's default. The messages name the
    /// variable to fix.
    pub fn check(&self, host_name: Option<&str>) -> Result<Checked, String> {
        if self.role != MachineRole::Gpu {
            return Err(format!(
                "machine role {:?}; only `gpu` installs in this build (combined and control-only arrive with #361)",
                self.role
            ));
        }
        let op = &self.operator;
        let enrollment = op
            .enrollment
            .as_deref()
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .ok_or_else(|| {
                format!(
                    "{ENROLLMENT} is required to install a GPU host (Admin → Fleet → Enroll host)"
                )
            })?;
        if !enrollment.starts_with("qenr1.") {
            return Err(format!(
                "{ENROLLMENT} is not an enrollment string (expected `qenr1.…`)"
            ));
        }
        let home_root = op
            .home_root
            .clone()
            .filter(|s| !s.is_empty())
            .ok_or_else(|| format!("{HOME_ROOT} is required"))?;
        let agent_image = ImageRef::parse(op.agent_image.as_deref().unwrap_or(""))
            .map_err(|e| format!("{AGENT_IMAGE}: {e}"))?;
        let node_name = op
            .node_name
            .clone()
            .filter(|s| !s.is_empty())
            .or_else(|| host_name.map(str::to_owned))
            .unwrap_or_else(|| "quasar-node".into());
        let template_root = op
            .template_root
            .clone()
            .filter(|s| !s.is_empty())
            .unwrap_or_else(recipe::default_template_root);
        let probe = Inputs {
            installation_id: "check".into(),
            node_name: node_name.clone(),
            home_root: home_root.clone(),
            template_root: template_root.clone(),
            docker_socket: recipe::default_docker_socket(),
            gpu: Default::default(),
            devices: Default::default(),
        };
        recipe::validate(&probe).map_err(|e| e.to_string())?;
        Ok(Checked {
            enrollment: enrollment.to_owned(),
            home_root,
            template_root,
            node_name,
            agent_image,
        })
    }
}

/// The checked first-install inputs of a GPU host.
#[derive(Debug, Clone)]
pub struct Checked {
    pub enrollment: String,
    pub home_root: String,
    pub template_root: String,
    pub node_name: String,
    pub agent_image: ImageRef,
}
