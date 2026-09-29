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
//!
//! **Console devices (RH-07 #407).** Whether the host has sound, logind's state, the
//! console-audio socket directory (D13) and which
//! `/dev/i2c-*` nodes are read by the device probe whenever console mode is turned on, and
//! again at every start of the recovery actor while it is on
//! ([`Actor::recheck_console_devices`]): i2c bus numbers can change across reboots, and an
//! engine refuses to create or start a container naming a node the host no longer has. A
//! changed set that moves the agent's rendered specification re-creates it through the same
//! verified replacement, recorded under [`DEVICES_CHANGED`] rather than [`CHANGED`]: it is
//! the actor's own, not a console change the agent asked for.
//!
//! **Preflight (RH-07 #407).** An agent created with the console additions checks, when it
//! starts, that it can take the display, and reports it with `POST /v1/console/preflight`
//! (a [`ConsolePreflight`]), before it reports healthy. Verifying an attempt that turns
//! console mode on fails at once, `unhealthy`, on a preflight that says it cannot; its text
//! is the settled change's [`ConsoleLast::detail`]. The report is kept in machine state
//! (`console-preflight.json`) so a verification a restart interrupted still reads it, and
//! is cleared when an attempt to turn console mode on is admitted.

use std::sync::Arc;

use serde::{Deserialize, Serialize};
use tracing::{info, warn};

use crate::actor::Actor;
use crate::machine::Machine;
use crate::recipe::{self, Inputs, Role};
use crate::reconfigure::Settled;
use crate::socket::{MachineRole, Reason, Rejection};

/// The name a console change is recorded under in `reconfigure.json`'s `changed`; no
/// operator variable has it, which is how a console record is told apart.
pub const CHANGED: &str = "console";

/// The name the actor's own re-render for changed console devices is recorded under.
pub const DEVICES_CHANGED: &str = "console-devices";

/// Where the last preflight report is kept, in machine state.
pub const PREFLIGHT_FILE: &str = "console-preflight.json";

/// How a failed preflight's text starts in the attempt's failure detail, which is how the
/// settled change finds it again ([`preflight_detail`]).
pub const PREFLIGHT_FAILED: &str = "the console agent cannot take the display: ";

/// The longest preflight text kept.
const PREFLIGHT_DETAIL_LIMIT: usize = 1024;

/// `POST /v1/console/preflight`: whether the console agent can take the display, and if
/// not, why, in words an operator reads (what holds it, or what host preparation lacks).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConsolePreflight {
    pub ok: bool,
    /// Required when `ok` is false; `null` otherwise, or a note.
    pub detail: Option<String>,
}

/// A failed preflight's text, from an attempt's failure detail.
pub(crate) fn preflight_detail(failure_detail: &str) -> Option<String> {
    failure_detail
        .strip_prefix(PREFLIGHT_FAILED)
        .map(|d| d.lines().next().unwrap_or("").to_owned())
}

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
    /// The console mode the change in flight asks for; `null` when none is.
    pub in_flight_target: Option<bool>,
    /// When the change in flight started (RFC 3339); `null` when none is.
    pub in_flight_started_at: Option<String>,
    /// How the last console change settled; `null` when the last reconfigure was not one.
    pub last: Option<ConsoleLast>,
    /// Whether this machine can take console mode; `why` says why not.
    pub supported: bool,
    pub why: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ConsoleLast {
    /// The attempt: the id `in_flight` named while it ran.
    pub request_id: String,
    /// The console mode that change asked for.
    pub target: bool,
    pub settled: Settled,
    /// The attempt's failure; `null` when it succeeded or was never journalled.
    pub reason: Option<Reason>,
    /// The attempt put the previous agent back.
    pub restored: bool,
    pub started_at: String,
    pub finished_at: String,
    /// The console agent's own preflight text when that is why the attempt failed (what
    /// holds the display, or what host preparation lacks); `null` otherwise.
    pub detail: Option<String>,
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
        let (mut in_flight, mut in_flight_target, mut in_flight_started_at, mut last) =
            (None, None, None, None);
        if let Some(r) = record {
            let console = r.changed.iter().any(|c| c == CHANGED);
            match r.outcome {
                // Machine state already holds the new inputs; the old are what runs.
                None => {
                    enabled = r.before.console;
                    if console {
                        in_flight_target = Some(r.after.console);
                        in_flight_started_at = Some(r.started_at);
                        in_flight = Some(r.request_id);
                    }
                }
                Some(o) if console => {
                    last = Some(ConsoleLast {
                        request_id: r.request_id,
                        target: r.after.console,
                        settled: o.settled,
                        reason: o.reason,
                        restored: o.restored,
                        started_at: r.started_at,
                        finished_at: o.settled_at,
                        detail: o.detail,
                    })
                }
                Some(_) => {}
            }
        }
        let support = self.console_support(machine.as_ref());
        ConsoleStatus {
            enabled,
            in_flight,
            in_flight_target,
            in_flight_started_at,
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

    /// The console devices the host has now (sound, logind's state, i2c nodes), read by
    /// the device probe from the node agent's current image: they can change after the
    /// install (a card added, a host prepared later, i2c buses renumbered by a reboot).
    /// `None` when they cannot be read; the last reading stands.
    fn host_console_devices(&self) -> Option<ConsoleDevices> {
        let record = self.dir.load_service(Role::NodeAgent).ok().flatten()?;
        match crate::probe::run(self.engine.as_ref(), &record.image) {
            Ok(report) => Some(ConsoleDevices {
                sound: report.sound,
                logind: report.logind(),
                console_audio: report.console_audio,
                i2c: report.i2c.clone(),
                dri_nodes: report.dri_nodes(),
            }),
            Err(e) => {
                warn!(
                    token = "console-devices-probe-failed",
                    "could not read the host's console devices ({e}); keeping the last reading"
                );
                None
            }
        }
    }

    /// `POST /v1/console/preflight`: the console agent's report of whether it can take the
    /// display, read by the verification of an attempt that turns console mode on.
    pub fn console_preflight(
        &self,
        report: ConsolePreflight,
    ) -> Result<ConsolePreflight, Rejection> {
        let detail = report
            .detail
            .as_deref()
            .map(str::trim)
            .filter(|d| !d.is_empty());
        if !report.ok && detail.is_none() {
            return Err(refuse(
                Reason::Invalid,
                "a preflight that fails must say why (`detail`)",
            ));
        }
        if detail
            .is_some_and(|d| d.len() > PREFLIGHT_DETAIL_LIMIT || d.contains(['\n', '\r', '\0']))
        {
            return Err(refuse(
                Reason::Invalid,
                format!("`detail` must be one line of at most {PREFLIGHT_DETAIL_LIMIT} bytes"),
            ));
        }
        let kept = ConsolePreflight {
            ok: report.ok,
            detail: detail.map(str::to_owned),
        };
        if kept.ok {
            info!(
                token = "console-preflight-ok",
                "the console agent can take the display"
            );
        } else {
            warn!(
                token = "console-preflight-failed",
                "the console agent cannot take the display: {}",
                kept.detail.as_deref().unwrap_or("")
            );
        }
        self.preflight_file().store(&kept).map_err(|e| {
            refuse(
                Reason::Busy,
                format!("the preflight could not be recorded ({e})"),
            )
        })?;
        Ok(kept)
    }

    fn preflight_file(&self) -> quasar_runtime::DurableFile<ConsolePreflight> {
        quasar_runtime::DurableFile::new(self.dir.root().join(PREFLIGHT_FILE), "json.tmp")
    }

    fn clear_preflight(&self) -> std::io::Result<()> {
        match std::fs::remove_file(self.preflight_file().path()) {
            Ok(()) => std::fs::File::open(self.dir.root())?.sync_all(),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(e),
        }
    }

    /// During the verification of attempt `request_id`: the failed preflight's text when the
    /// attempt turns console mode on and the new agent reported it cannot take the display.
    pub(crate) fn failed_preflight(&self, request_id: &str) -> Option<String> {
        let record = self.dir.reconfigure_file().load().ok().flatten()?;
        let enabling = record.request_id == request_id
            && record.outcome.is_none()
            && record.changed.iter().any(|c| c == CHANGED)
            && record.after.console
            && !record.before.console;
        if !enabling {
            return None;
        }
        match self.preflight_file().load() {
            Ok(Some(p)) if !p.ok => Some(p.detail.unwrap_or_default()),
            _ => None,
        }
    }

    /// At every start, while console mode is on: the console devices read again, and the
    /// agent re-created through a verified replacement when they move its specification (a
    /// renumbered or vanished i2c node, sound or logind gone or come). `Some` names the
    /// admitted attempt. Never fails the start: a probe that cannot run, or a machine that
    /// cannot take the attempt now, keeps the agent as it is (logged).
    pub fn recheck_console_devices(self: &Arc<Self>) -> Option<String> {
        let machine = self.dir.load_machine().ok().flatten()?;
        if !machine.inputs.console || machine.role == MachineRole::ControlOnly {
            return None;
        }
        let devices = self.host_console_devices()?;
        let _gate = self.gate.lock().unwrap();
        let machine = match self.admissible() {
            Ok(m) => m,
            Err(r) => {
                warn!(token = "console-devices-recheck-deferred", "{}", r.message);
                return None;
            }
        };
        if !machine.inputs.console {
            return None;
        }
        let mut after = machine.inputs.clone();
        devices.apply(&mut after.devices);
        if after == machine.inputs {
            return None;
        }
        let replaced: Vec<Role> = match self.moved_by(&machine.inputs, &after) {
            Ok(moved) => moved
                .into_iter()
                .filter(|r| *r == Role::NodeAgent)
                .collect(),
            Err(why) => {
                warn!(token = "console-devices-unrenderable", "{why}");
                return None;
            }
        };
        info!(
            token = "console-devices-changed",
            i2c = ?after.devices.i2c,
            sound = after.devices.sound,
            logind = after.devices.logind,
            console_audio = after.devices.console_audio,
            re_created = !replaced.is_empty(),
            "the host's console devices changed since the agent was created"
        );
        match self.admit(&machine, after, &[DEVICES_CHANGED.to_string()], &replaced) {
            Ok(id) => id,
            Err(r) => {
                warn!(token = "console-devices-recreate-refused", "{}", r.message);
                None
            }
        }
    }

    /// `POST /v1/console`: console mode on or off, through a verified replacement of the
    /// node agent alone. `Some` names the admitted attempt; `None`: nothing needed
    /// re-creating (enabling what is already in force, say).
    pub fn console(self: &Arc<Self>, req: ConsoleRequest) -> Result<Option<String>, Rejection> {
        // Before the gate: the probe is a short-lived container, up to a minute.
        let devices = if req.enabled {
            self.host_console_devices()
        } else {
            None
        };
        let _gate = self.gate.lock().unwrap();
        let machine = self.admissible()?;
        if machine.role == MachineRole::ControlOnly {
            return Err(refuse(
                Reason::Invalid,
                "a control-only machine runs no node agent",
            ));
        }
        let mut after = Inputs {
            console: req.enabled,
            ..machine.inputs.clone()
        };
        if let Some(devices) = devices {
            devices.apply(&mut after.devices);
        }
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
        // A preflight an earlier console agent reported is not this attempt's.
        if req.enabled && !machine.inputs.console {
            self.clear_preflight().map_err(|e| {
                refuse(
                    Reason::Busy,
                    format!("an earlier preflight could not be cleared ({e}); console mode was not changed"),
                )
            })?;
        }
        self.admit(&machine, after, &[CHANGED.to_string()], &replaced)
    }
}

/// What the device probe read of the host's console devices.
#[derive(Debug, Clone, PartialEq, Eq)]
struct ConsoleDevices {
    sound: bool,
    logind: bool,
    console_audio: bool,
    i2c: Vec<u32>,
    dri_nodes: Vec<String>,
}

impl ConsoleDevices {
    fn apply(self, devices: &mut crate::recipe::HostDevices) {
        devices.sound = self.sound;
        devices.logind = self.logind;
        devices.console_audio = self.console_audio;
        devices.i2c = self.i2c;
        if !self.dri_nodes.is_empty() {
            devices.dri_nodes = self.dri_nodes;
        }
    }
}
