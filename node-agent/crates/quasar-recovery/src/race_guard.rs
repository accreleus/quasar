//! The race guard (#352 decision D3, architecture §5.4): which containers on this machine
//! look like a Quasar platform service but were not created by this installation.
//!
//! Such a container is an **owner conflict** (`CONTEXT.md`): a leftover Compose stack, a
//! definition an external manager still holds, another installation's service. The actor
//! never stops, renames or removes one. It reports each in `status`, the agent raises the
//! readiness check `owner_conflict` from that report, the control plane blocks the target,
//! and `submit` refuses `owner_conflict` while any exists. This module only decides; it
//! is pure over one container listing.
//!
//! What is *not* a conflict: every container carrying this installation's label, under any
//! name (the running service, a replacement's kept `.kept` and successor `.next`
//! containers); this actor's own container; a seed (the manager's, by design); the node
//! agent's own session and probe containers; the actor's disposable helpers.

use crate::engine::Container;
use crate::recipe::{labels, names, Role};
use crate::socket::Conflict;

/// Compose's service label: a leftover stack's containers carry it.
pub const COMPOSE_SERVICE: &str = "com.docker.compose.service";
/// Compose's project label, named in the conflict so the operator can find the stack.
pub const COMPOSE_PROJECT: &str = "com.docker.compose.project";
/// The node agent's ownership label on the containers it creates (sessions, probes).
const AGENT_OWNER: &str = quasar_runtime::ownership::LABEL;
/// The Go updater of a Compose install: it recreates containers on its own, so a leftover
/// one is in the way as much as a leftover service.
pub const UPDATER: &str = "updater";
const UPDATER_SERVICE: &str = "quasar-updater";
/// The node agent's probe containers, named before they are labelled.
const AGENT_PROBE_PREFIX: &str = "quasar-probe-";

const ROLES: [Role; 4] = [
    Role::NodeAgent,
    Role::ControlPlane,
    Role::Postgres,
    Role::RecoveryActor,
];

/// Every owner conflict in `containers`, in listing order. `installation` is this
/// machine's (`None` before the machine is installed: then every look-alike is one), `me`
/// this process's own container id.
pub fn conflicts(
    containers: &[Container],
    installation: Option<&str>,
    me: Option<&str>,
) -> Vec<Conflict> {
    containers
        .iter()
        .filter_map(|c| conflict(c, installation, me))
        .collect()
}

/// The conflict `c` is, if it is one.
pub fn conflict(c: &Container, installation: Option<&str>, me: Option<&str>) -> Option<Conflict> {
    if me.is_some_and(|me| same_id(me, &c.id)) || crate::seed::is_seed(c) {
        return None;
    }
    let label = c.labels.get(labels::INSTALLATION).map(String::as_str);
    if installation.is_some() && label == installation {
        return None;
    }
    if c.labels.contains_key(labels::HELPER) || c.labels.contains_key(AGENT_OWNER) {
        return None;
    }
    let (role, why) = looks_like(c)?;
    let why = match (label, c.labels.get(COMPOSE_PROJECT)) {
        (Some(other), _) => format!("{why}, of another installation ({other})"),
        (None, Some(project)) => format!(
            "part of the Compose project {project}, probably left from an older Compose install"
        ),
        (None, None) => why,
    };
    Some(Conflict {
        container: c.name.clone(),
        id: c.id.chars().take(12).collect(),
        image: c.image.clone(),
        role: role.into(),
        why,
    })
}

/// The role a container looks like, and what gave it away. The recovery image run as a
/// seed never gets here; run as an actor it does.
fn looks_like(c: &Container) -> Option<(&'static str, String)> {
    if let Some(role) = c
        .labels
        .get(labels::PLATFORM_SERVICE)
        .and_then(|r| Role::parse(r))
    {
        return Some((
            role.as_str(),
            format!("labelled as a Quasar {}", noun(role.as_str())),
        ));
    }
    if names::HELPERS.contains(&c.name.as_str()) {
        return Some((
            Role::RecoveryActor.as_str(),
            "holds the name of a recovery-actor helper without its label".into(),
        ));
    }
    let base = c
        .name
        .strip_suffix(".kept")
        .or_else(|| c.name.strip_suffix(".next"))
        .unwrap_or(&c.name);
    if let Some(role) = ROLES.into_iter().find(|r| r.container_name() == base) {
        return Some((
            role.as_str(),
            format!("holds the name of the Quasar {}", noun(role.as_str())),
        ));
    }
    if let Some(service) = c.labels.get(COMPOSE_SERVICE) {
        if let Some(role) = ROLES.into_iter().find(|r| r.container_name() == service) {
            return Some((
                role.as_str(),
                format!(
                    "a Compose service named like the Quasar {}",
                    noun(role.as_str())
                ),
            ));
        }
        if service == UPDATER_SERVICE {
            return Some((UPDATER, "the Compose updater of a Quasar install".into()));
        }
    }
    if c.name.starts_with(AGENT_PROBE_PREFIX) {
        return None;
    }
    let image = crate::actor::repository_of(&c.image);
    let last = image.rsplit('/').next().unwrap_or(&image);
    let by_image = match last {
        "quasar-control-plane" => Some(Role::ControlPlane),
        "quasar-node-agent" => Some(Role::NodeAgent),
        "quasar-recovery" if runs_actor(c) => Some(Role::RecoveryActor),
        _ => None,
    }?;
    Some((
        by_image.as_str(),
        format!("runs the Quasar {} image", noun(by_image.as_str())),
    ))
}

fn runs_actor(c: &Container) -> bool {
    let program = c.command.first().map(|p| p.rsplit('/').next().unwrap_or(p));
    program == Some("quasar-recovery") && c.command.get(1).map(String::as_str) == Some("actor")
}

/// "node agent", "control plane": the role as copy names it.
pub fn noun(role: &str) -> &'static str {
    match role {
        "node-agent" => "node agent",
        "control-plane" => "control plane",
        "postgres" => "database",
        "recovery-actor" => "recovery actor",
        UPDATER => "updater",
        _ => "platform service",
    }
}

/// The one-line fix for a set of conflicts: remove the containers. A manager that holds a
/// definition for one would recreate it, which the check then reports again.
pub fn remedy(conflicts: &[Conflict]) -> String {
    let names: Vec<&str> = conflicts.iter().map(|c| c.container.as_str()).collect();
    format!("docker rm -f {}", names.join(" "))
}

/// The refusal message `submit` and the operator commands give.
pub fn refusal(conflicts: &[Conflict]) -> String {
    let list: Vec<String> = conflicts
        .iter()
        .map(|c| format!("{} ({}: {})", c.container, c.image, c.why))
        .collect();
    format!(
        "{} on this machine look{} like a Quasar platform service but {} not created by this installation, and the recovery actor never acts on a container it did not create: {}. Remove {} (and the manager definition that recreates {}), e.g. `{}`; nothing was changed",
        if conflicts.len() == 1 { "A container" } else { "Containers" },
        if conflicts.len() == 1 { "s" } else { "" },
        if conflicts.len() == 1 { "was" } else { "were" },
        list.join("; "),
        if conflicts.len() == 1 { "it" } else { "them" },
        if conflicts.len() == 1 { "it" } else { "them" },
        remedy(conflicts),
    )
}

fn same_id(a: &str, b: &str) -> bool {
    !a.is_empty() && !b.is_empty() && (a.starts_with(b) || b.starts_with(a))
}
