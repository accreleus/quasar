//! Console mode on an owned install (RH-07 #395): the node agent asks, on the agent socket
//! only, for its console additions ([`Inputs::console`]) on or off. The change is a
//! reconfigure of that one input that re-creates only the node agent, driven by the
//! reconfigure machinery ([`crate::reconfigure`]): `reconfigure.json` before the attempt,
//! the old agent kept until the new one verifies and put back if it does not, the outcome
//! settled from the journal, again on the next start. The operator's `reconfigure` never
//! sets it. Not a frozen interface; the shapes are pinned by `testdata/recovery/agent-socket`.
//!
//! `GET /v1/console` is a [`ConsoleStatus`]. `POST /v1/console` (a [`ConsoleRequest`])
//! answers `202` with the status once a replacement is admitted (`in_flight` names it
//! until it settles), `200` with it when nothing needed re-creating, `409` with a `busy`
//! `Rejection`, `400` with any other.
//!
//! The attempt is journalled as the operator's, like every reconfigure, so the agent's
//! release relay (`GET /v1/status`) never adopts it.

use std::sync::Arc;

use serde::{Deserialize, Serialize};
use tracing::info;

use crate::actor::Actor;
use crate::machine::Machine;
use crate::recipe::{self, Inputs, Role};
use crate::reconfigure::Settled;
use crate::socket::{MachineRole, Reason, Rejection};

/// The name a console change is recorded under in `reconfigure.json`'s `changed`; no
/// operator variable has it, which is how a console record is told apart.
pub const CHANGED: &str = "console";

/// `POST /v1/console`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConsoleRequest {
    pub enabled: bool,
}

/// `GET /v1/console`, and the answer to a `POST`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ConsoleStatus {
    /// Console mode as the verified agent runs it: a change is not in force until it settles.
    pub enabled: bool,
    /// The console change being applied, until it settles.
    pub in_flight: Option<String>,
    /// How the last console change settled; `null` when the last reconfigure was not one.
    pub last: Option<ConsoleLast>,
    /// Whether this machine can take console mode; `why` says why not.
    pub supported: bool,
    pub why: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ConsoleLast {
    /// The console mode that change asked for.
    pub target: bool,
    pub settled: Settled,
    /// The attempt's failure; `null` when it succeeded or was never journalled.
    pub reason: Option<Reason>,
    /// The attempt put the previous agent back.
    pub restored: bool,
    pub finished_at: String,
}

fn refuse(reason: Reason, message: impl Into<String>) -> Rejection {
    Rejection {
        request_id: String::new(),
        reason,
        message: message.into(),
    }
}

impl Actor {
    /// `GET /v1/console`.
    pub fn console_status(&self) -> ConsoleStatus {
        let machine = self.dir.load_machine().ok().flatten();
        let record = self.dir.reconfigure_file().load().ok().flatten();
        let mut enabled = machine.as_ref().is_some_and(|m| m.inputs.console);
        let (mut in_flight, mut last) = (None, None);
        if let Some(r) = record {
            let console = r.changed.iter().any(|c| c == CHANGED);
            match r.outcome {
                // Machine state already holds the new inputs; the old are what runs.
                None => {
                    enabled = r.before.console;
                    in_flight = console.then_some(r.request_id);
                }
                Some(o) if console => {
                    last = Some(ConsoleLast {
                        target: r.after.console,
                        settled: o.settled,
                        reason: o.reason,
                        restored: o.restored,
                        finished_at: o.settled_at,
                    })
                }
                Some(_) => {}
            }
        }
        let support = self.console_support(machine.as_ref());
        ConsoleStatus {
            enabled,
            in_flight,
            last,
            supported: support.is_ok(),
            why: support.err(),
        }
    }

    /// Whether the node agent can be rendered with console mode on this machine.
    fn console_support(&self, machine: Option<&Machine>) -> Result<(), String> {
        if let Some(why) = crate::uninstall::uninstalled(&self.dir) {
            return Err(why);
        }
        let Some(machine) = machine else {
            return Err("this machine is not installed yet".into());
        };
        if machine.role == MachineRole::ControlOnly {
            return Err("a control-only machine runs no node agent".into());
        }
        let mut want = machine.inputs.clone();
        want.console = true;
        match self.spec_with(Role::NodeAgent, &want) {
            Ok(Some(_)) => Ok(()),
            Ok(None) => Err("machine state has no record of the node agent".into()),
            Err(why) => Err(why),
        }
    }

    /// `POST /v1/console`: console mode on or off, through a verified replacement of the
    /// node agent alone. `Some` names the admitted attempt; `None`: nothing needed
    /// re-creating (enabling what is already in force, say).
    pub fn console(self: &Arc<Self>, req: ConsoleRequest) -> Result<Option<String>, Rejection> {
        let _gate = self.gate.lock().unwrap();
        let machine = self.admissible()?;
        if machine.role == MachineRole::ControlOnly {
            return Err(refuse(
                Reason::Invalid,
                "a control-only machine runs no node agent",
            ));
        }
        let after = Inputs {
            console: req.enabled,
            ..machine.inputs.clone()
        };
        recipe::validate(&after).map_err(|e| {
            refuse(
                Reason::Invalid,
                format!("{e}; console mode was not changed"),
            )
        })?;
        // A service left behind by an operator's partial reconfigure is the operator's
        // to catch up: this re-creates the agent and nothing else.
        let replaced: Vec<Role> = self
            .moved_by(&machine.inputs, &after)
            .map_err(|why| {
                refuse(
                    Reason::Invalid,
                    format!("{why}; console mode was not changed"),
                )
            })?
            .into_iter()
            .filter(|r| *r == Role::NodeAgent)
            .collect();
        if replaced.is_empty() && after == machine.inputs {
            info!(
                enabled = req.enabled,
                "console mode is already as asked; nothing re-created"
            );
            return Ok(None);
        }
        self.admit(&machine, after, &[CHANGED.to_string()], &replaced)
    }
}
