//! What `quasar-recovery status` says beside the inventory it prints: one line per service
//! that is not as this machine's role needs it, naming what to look at. Pure, so the
//! operator's wording is tested rather than read off a live machine.

use crate::machine::Machine;
use crate::recipe::{DatabaseInputs, Role};
use crate::socket::{MachineRole, Service, Status};

fn service<'a>(status: &'a Status, role: &str) -> Option<&'a Service> {
    status.services.iter().find(|s| s.role == role)
}

fn healthy(s: &Service) -> bool {
    s.state == "running" && s.health.as_deref().is_none_or(|h| h == "healthy")
}

fn state(s: &Service) -> String {
    match &s.health {
        Some(h) => format!("{}, {h}", s.state),
        None => s.state.clone(),
    }
}

/// The lines, in the order the services are created. Empty when everything this machine
/// runs is running and healthy.
pub fn explain(status: &Status, machine: Option<&Machine>) -> Vec<String> {
    let mut out = Vec::new();
    let Some(machine) = machine else {
        out.push("this machine is not installed yet: the recovery actor installs it from the seed's inputs when it starts (docker logs quasar-recovery says what it is waiting for)".into());
        return out;
    };
    if status.stale {
        out.push("the container engine did not answer in time: this is the last inventory the recovery actor read".into());
    }
    let external = machine
        .inputs
        .control
        .as_ref()
        .and_then(|c| match &c.database {
            DatabaseInputs::External { host, port, .. } => Some(format!("{host}:{port}")),
            DatabaseInputs::Owned => None,
        });
    let mut expected = Vec::new();
    if machine.role != MachineRole::Gpu {
        if external.is_none() {
            expected.push(Role::Postgres);
        }
        expected.push(Role::ControlPlane);
    }
    if machine.role != MachineRole::ControlOnly {
        expected.push(Role::NodeAgent);
    }
    for role in expected {
        let container = role.container_name();
        match service(status, role.as_str()) {
            None => out.push(format!(
                "{container} is not on this machine: `docker restart quasar-recovery` creates it"
            )),
            Some(s) if healthy(s) => {}
            Some(s) if role == Role::ControlPlane && external.is_some() => out.push(format!(
                "{container} is {}. Its database is your own, at {}: check that it is up and reachable from this machine; `docker logs {container}` says why",
                state(s),
                external.as_deref().unwrap_or_default()
            )),
            Some(s) => out.push(format!(
                "{container} is {}: `docker logs {container}` says why",
                state(s)
            )),
        }
    }
    for c in &status.conflicts {
        out.push(format!(
            "{} ({}) is not this installation's and is never acted on: {}",
            c.container, c.image, c.why
        ));
    }
    out
}
