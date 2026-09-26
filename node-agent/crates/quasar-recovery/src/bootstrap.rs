//! The bootstrap inputs: what an operator writes in the seed's `docker run` line or stack
//! (`docs/configuration.md` "Seed"). The seed checks them before it creates anything; the
//! actor it creates reads the same variables from the seed's container and installs from
//! them, once, on a clean machine. One parser and one check serve both, so a seed never
//! accepts inputs its actor would refuse.

use crate::actor::OperatorInputs;
use crate::recipe::{
    self, control, AppInputs, ControlInputs, DatabaseInputs, ImageRef, Inputs, TrustInputs,
};
use crate::socket::MachineRole;

pub const ROLE: &str = "QUASAR_ROLE";
pub const ENROLLMENT: &str = "QUASAR_ENROLLMENT";
pub const HOME_ROOT: &str = "QUASAR_HOME_ROOT";
pub const TEMPLATE_ROOT: &str = "QUASAR_TEMPLATE_ROOT";
pub const NODE_NAME: &str = "QUASAR_NODE_NAME";
pub const AGENT_IMAGE: &str = "QUASAR_AGENT_IMAGE";
pub const CONTROL_PLANE_IMAGE: &str = "QUASAR_CONTROL_PLANE_IMAGE";
pub const POSTGRES_IMAGE: &str = "QUASAR_POSTGRES_IMAGE";
pub const PUBLIC_HOST: &str = "QUASAR_PUBLIC_HOST";
pub const TLS_HOSTS: &str = "QUASAR_TLS_HOSTS";
pub const TRUSTED_PROXIES: &str = "QUASAR_TRUSTED_PROXIES";
pub const HTTP_PORT: &str = "QUASAR_HTTP_PORT";
pub const TLS_PORT: &str = "QUASAR_TLS_PORT";
/// Setting this makes the database the operator's own (#352 R1-Q1); the other
/// `QUASAR_DATABASE_*` inputs complete it, under the control plane's own names.
pub const DATABASE_HOST: &str = "QUASAR_DATABASE_HOST";
pub const DATABASE_PORT: &str = "QUASAR_DATABASE_PORT";
pub const DATABASE_USER: &str = "QUASAR_DATABASE_USER";
pub const DATABASE_NAME: &str = "QUASAR_DATABASE_NAME";
pub const DATABASE_SSLMODE: &str = "QUASAR_DATABASE_SSLMODE";
pub const DATABASE_PASSWORD: &str = "QUASAR_DATABASE_PASSWORD";

/// Release trust, read exactly as the updater reads them (`crate::trust`).
pub const ALLOWED_NAMESPACES: &str = "QUASAR_UPDATER_ALLOWED_NAMESPACES";
pub const SIGNATURE_MODE: &str = "QUASAR_UPDATER_SIGNATURE_MODE";
pub const TRUSTED_KEYS: &str = "QUASAR_UPDATER_TRUSTED_KEYS";
pub const MANIFEST_BASE_URL: &str = "QUASAR_UPDATER_MANIFEST_BASE_URL";
pub const MANIFEST_TIMEOUT_S: &str = "QUASAR_UPDATER_MANIFEST_TIMEOUT_S";
pub const INSECURE_REGISTRIES: &str = "QUASAR_PLATFORM_INSECURE_REGISTRIES";

/// Host defaults for the agent's app containers (the agent's own variables).
pub const APP_PUID: &str = "QUASAR_APP_PUID";
pub const APP_PGID: &str = "QUASAR_APP_PGID";
pub const CONTAINER_NETWORK: &str = "QUASAR_CONTAINER_NETWORK";

pub const DEFAULT_HTTP_PORT: u16 = 8080;
pub const DEFAULT_TLS_PORT: u16 = 8443;

#[derive(Debug, Clone)]
pub struct Bootstrap {
    pub role: MachineRole,
    pub operator: OperatorInputs,
}

pub fn parse_role(raw: &str) -> Result<MachineRole, String> {
    match raw {
        "gpu" => Ok(MachineRole::Gpu),
        "combined" => Ok(MachineRole::Combined),
        "control-only" | "control_only" => Ok(MachineRole::ControlOnly),
        other => Err(format!(
            "{ROLE}={other:?} is not a role: use gpu, combined or control-only"
        )),
    }
}

fn port(name: &str, raw: Option<&str>, default: u16) -> Result<u16, String> {
    match raw {
        None => Ok(default),
        Some(v) => match v.trim().parse::<u16>() {
            Ok(p) if p > 0 => Ok(p),
            _ => Err(format!("{name}={v:?} is not a port")),
        },
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
                control_plane_image: get(CONTROL_PLANE_IMAGE),
                postgres_image: get(POSTGRES_IMAGE),
                public_host: get(PUBLIC_HOST),
                tls_hosts: get(TLS_HOSTS),
                trusted_proxies: get(TRUSTED_PROXIES),
                http_port: get(HTTP_PORT),
                tls_port: get(TLS_PORT),
                database_host: get(DATABASE_HOST),
                database_port: get(DATABASE_PORT),
                database_user: get(DATABASE_USER),
                database_name: get(DATABASE_NAME),
                database_sslmode: get(DATABASE_SSLMODE),
                database_password: get(DATABASE_PASSWORD),
                app_puid: get(APP_PUID),
                app_pgid: get(APP_PGID),
                container_network: get(CONTAINER_NETWORK),
                trust: TrustInputs {
                    allowed_namespaces: get(ALLOWED_NAMESPACES),
                    signature_mode: get(SIGNATURE_MODE),
                    trusted_keys: get(TRUSTED_KEYS),
                    manifest_base_url: get(MANIFEST_BASE_URL),
                    manifest_timeout_s: get(MANIFEST_TIMEOUT_S),
                    insecure_registries: get(INSECURE_REGISTRIES),
                },
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
        let op = &self.operator;
        let agent_here = self.role != MachineRole::ControlOnly;
        let control_here = self.role != MachineRole::Gpu;

        let enrollment = match self.role {
            MachineRole::Gpu => {
                let enrollment = op
                    .enrollment
                    .as_deref()
                    .map(str::trim)
                    .filter(|s| !s.is_empty())
                    .ok_or_else(|| {
                        format!(
                            "{ENROLLMENT} is required to install a GPU host (Admin → Fleet → Add host)"
                        )
                    })?;
                if !enrollment.starts_with("qenr1.") {
                    return Err(format!(
                        "{ENROLLMENT} is not an enrollment string (expected `qenr1.…`)"
                    ));
                }
                Some(enrollment.to_owned())
            }
            // The control plane on this machine enrolls its own agent with a local token.
            _ => None,
        };
        let home_root = op.home_root.clone().filter(|s| !s.is_empty());
        let home_root = match (agent_here, home_root) {
            (true, None) => return Err(format!("{HOME_ROOT} is required")),
            (_, home) => home.unwrap_or_default(),
        };
        // A control-only machine runs no agent, but names the one its Add host installs.
        let named_agent = match op.agent_image.as_deref() {
            Some(raw) => Some(ImageRef::parse(raw).map_err(|e| format!("{AGENT_IMAGE}: {e}"))?),
            None if agent_here => {
                return Err(format!(
                    "{AGENT_IMAGE}: {}",
                    ImageRef::parse("").unwrap_err()
                ))
            }
            None => None,
        };
        let agent_image = if agent_here {
            named_agent.clone()
        } else {
            None
        };
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
            .unwrap_or_else(|| {
                if home_root.is_empty() {
                    recipe::default_template_root()
                } else {
                    recipe::template_root_beside(&home_root)
                }
            });

        let control = if control_here {
            Some(self.check_control()?)
        } else {
            None
        };
        let probe = Inputs {
            installation_id: "check".into(),
            node_name: node_name.clone(),
            home_root: home_root.clone(),
            template_root: template_root.clone(),
            docker_socket: recipe::default_docker_socket(),
            gpu: Default::default(),
            devices: Default::default(),
            control: control.as_ref().map(|c| c.inputs.clone()),
            socket_dir: control_here.then(|| "/check".to_string()),
            trust: op.trust.clone(),
            enroll: Default::default(),
            app: self.check_app()?,
        };
        recipe::validate(&probe).map_err(|e| e.to_string())?;
        trust_config(&op.trust)?;
        Ok(Checked {
            role: self.role,
            enrollment,
            home_root,
            template_root,
            node_name,
            agent_image,
            control,
            trust: op.trust.clone(),
            enroll_agent_image: named_agent,
            app: self.check_app()?,
        })
    }

    fn check_app(&self) -> Result<AppInputs, String> {
        let op = &self.operator;
        let id = |name: &str, raw: &Option<String>| -> Result<Option<u32>, String> {
            raw.as_deref()
                .map(|v| {
                    v.trim()
                        .parse::<u32>()
                        .map_err(|_| format!("{name}={v:?} is not a numeric id"))
                })
                .transpose()
        };
        Ok(AppInputs {
            puid: id(APP_PUID, &op.app_puid)?,
            pgid: id(APP_PGID, &op.app_pgid)?,
            container_network: op.container_network.as_ref().map(|n| n.trim().to_owned()),
        })
    }

    fn check_control(&self) -> Result<CheckedControl, String> {
        let op = &self.operator;
        let image = ImageRef::parse(op.control_plane_image.as_deref().unwrap_or(""))
            .map_err(|e| format!("{CONTROL_PLANE_IMAGE}: {e}"))?;
        let http_port = port(HTTP_PORT, op.http_port.as_deref(), DEFAULT_HTTP_PORT)?;
        let tls_port = port(TLS_PORT, op.tls_port.as_deref(), DEFAULT_TLS_PORT)?;
        let (database, postgres_image, database_password) = match &op.database_host {
            None => {
                let named = op
                    .postgres_image
                    .as_deref()
                    .unwrap_or(control::DEFAULT_POSTGRES_IMAGE);
                let postgres =
                    ImageRef::parse(named).map_err(|e| format!("{POSTGRES_IMAGE}: {e}"))?;
                (DatabaseInputs::Owned, Some(postgres), None)
            }
            Some(host) => {
                if op.postgres_image.is_some() {
                    return Err(format!(
                        "{POSTGRES_IMAGE} and {DATABASE_HOST} are both set: an operator's own database gets no Quasar-owned Postgres"
                    ));
                }
                let password = op
                    .database_password
                    .clone()
                    .filter(|p| !p.is_empty())
                    .ok_or_else(|| {
                        format!("{DATABASE_PASSWORD} is required with {DATABASE_HOST}")
                    })?;
                let port = port(DATABASE_PORT, op.database_port.as_deref(), 5432)?;
                let database = DatabaseInputs::External {
                    host: host.trim().to_owned(),
                    port,
                    user: op
                        .database_user
                        .clone()
                        .unwrap_or_else(|| control::OWNED_DATABASE_USER.into()),
                    name: op
                        .database_name
                        .clone()
                        .unwrap_or_else(|| control::OWNED_DATABASE.into()),
                    sslmode: op
                        .database_sslmode
                        .clone()
                        .unwrap_or_else(|| "disable".into()),
                };
                (database, None, Some(password))
            }
        };
        if op.database_host.is_none() && op.database_password.is_some() {
            return Err(format!(
                "{DATABASE_PASSWORD} is set without {DATABASE_HOST}: a Quasar-owned database generates its own password"
            ));
        }
        Ok(CheckedControl {
            image,
            postgres_image,
            database_password,
            inputs: ControlInputs {
                machine_role: if self.role == MachineRole::Combined {
                    recipe::ControlRole::Combined
                } else {
                    recipe::ControlRole::ControlOnly
                },
                trusted_proxies: op.trusted_proxies.clone(),
                http_port,
                tls_port,
                public_host: op.public_host.clone().map(|h| h.trim().to_owned()),
                tls_hosts: op.tls_hosts.clone(),
                database,
            },
        })
    }
}

/// The checked first-install inputs.
#[derive(Clone)]
pub struct Checked {
    pub role: MachineRole,
    /// A GPU host's enrollment string.
    pub enrollment: Option<String>,
    /// Empty on a control-only machine that names none.
    pub home_root: String,
    pub template_root: String,
    pub node_name: String,
    /// Every machine with a node agent.
    pub agent_image: Option<ImageRef>,
    /// Every machine with a control plane.
    pub control: Option<CheckedControl>,
    pub trust: TrustInputs,
    /// The agent image a control-plane machine's Add host installs on new GPU hosts: its
    /// own agent's on a combined host, `QUASAR_AGENT_IMAGE` if named on a control-only one.
    pub enroll_agent_image: Option<ImageRef>,
    pub app: AppInputs,
}

/// The release trust a machine's recorded settings give, parsed as the updater parses its
/// variables. Fails naming the variable.
pub fn trust_config(t: &TrustInputs) -> Result<crate::actor::TrustConfig, String> {
    use crate::trust;
    let raw = |v: &Option<String>| v.clone().unwrap_or_default();
    let mode = trust::parse_signature_mode(&raw(&t.signature_mode))
        .map_err(|e| format!("{SIGNATURE_MODE}: {e}"))?;
    let keys = trust::parse_trusted_keys(&raw(&t.trusted_keys))
        .map_err(|e| format!("{TRUSTED_KEYS}: {e}"))?;
    trust::parse_manifest_base_url(&raw(&t.manifest_base_url))
        .map_err(|e| format!("{MANIFEST_BASE_URL}: {e}"))?;
    Ok(crate::actor::TrustConfig {
        allowed_namespaces: trust::parse_allowed_namespaces(&raw(&t.allowed_namespaces)),
        signature: trust::SignaturePolicy { mode, keys },
    })
}

/// A combined or control-only machine's control-plane inputs.
#[derive(Clone)]
pub struct CheckedControl {
    pub image: ImageRef,
    /// A Quasar-owned database's image; `None` for the operator's own.
    pub postgres_image: Option<ImageRef>,
    /// The operator's own database's password, copied into machine state at first boot.
    pub database_password: Option<String>,
    pub inputs: ControlInputs,
}

impl std::fmt::Debug for Checked {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Checked")
            .field("role", &self.role)
            .field("home_root", &self.home_root)
            .field("node_name", &self.node_name)
            .finish_non_exhaustive()
    }
}
