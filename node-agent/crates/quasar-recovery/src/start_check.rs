//! What every start of the recovery actor checks once `resume` has finished (#432; the
//! console devices, RH-07 #407): that the node agent it recorded still fits this machine,
//! and runs.
//!
//! The device probe is read again from the agent's image. When the GPU it finds is not the
//! one the agent was created for (another vendor or render node, none at all, one where
//! there was none), or the engine no longer serves the NVIDIA GPU the way the agent asks for
//! it (its CDI specification gone), the agent is re-created for what the machine has now,
//! through the reconfigure machinery's verified replacement ([`crate::reconfigure`]),
//! recorded under [`GPU_CHANGED`]. Console devices that moved while console mode is on join
//! the same attempt under [`crate::console::DEVICES_CHANGED`]. Nothing is granted that the
//! recipe does not render for the probed shape.
//!
//! An agent left `exited` or `created` with its `unless-stopped` policy (an engine that could
//! not start it at boot) is started, and re-created under [`AGENT_UNSTARTABLE`] when the
//! engine refuses. Quasar sets the policy to `no` before any stop of its own (ADR 0007,
//! "an actor stopped from outside"), so an agent with policy `no` is left alone.
//!
//! One look per start: an attempt that fails puts the previous agent back and is tried again
//! only by the next start.

use std::sync::Arc;

use tracing::{info, warn};

use crate::actor::Actor;
use crate::console::{ConsoleDevices, DEVICES_CHANGED};
use crate::engine::{EngineError, RestartPolicy};
use crate::machine::{Machine, ServiceRecord};
use crate::probe::{self, ProbeReport};
use crate::recipe::{names, GpuFacts, Inputs, Role};
use crate::socket::MachineRole;

/// The name the actor's own re-render for a changed GPU is recorded under in
/// `reconfigure.json`'s `changed`.
pub const GPU_CHANGED: &str = "gpu";

/// The name a re-creation of an agent the engine would not start is recorded under.
pub const AGENT_UNSTARTABLE: &str = "agent-unstartable";

enum Agent {
    /// Running, missing (`resume` creates it), or stopped by Quasar: nothing to do.
    Fine,
    Started,
    Unstartable(EngineError),
}

impl Actor {
    /// Once per start, after `resume`: the node agent re-created through a verified
    /// replacement when this machine's GPU or console devices no longer match it, or when
    /// it is not running and the engine will not start it. `Some` names the admitted
    /// attempt. Never fails the start: a probe that cannot run, or a machine that cannot
    /// take the attempt now, keeps the agent as it is (logged).
    pub fn recheck_on_start(self: &Arc<Self>) -> Option<String> {
        let machine = self.dir.load_machine().ok().flatten()?;
        if machine.role == MachineRole::ControlOnly
            || crate::uninstall::uninstalled(&self.dir).is_some()
        {
            return None;
        }
        let record = self.dir.load_service(Role::NodeAgent).ok().flatten()?;
        // Before the gate: the probes are short-lived containers, up to a minute each.
        let observed = match probe::run(self.engine.as_ref(), &record.image) {
            Ok(report) => match self.observe(&machine.inputs, &report, &record) {
                Ok(observed) => Some(observed),
                Err(e) => {
                    warn!(
                        token = "actor-gpu-recheck-failed",
                        "could not ask the engine about this machine's GPU ({e}); the node agent keeps the GPU it was created with"
                    );
                    None
                }
            },
            Err(e) => {
                warn!(
                    token = "actor-device-probe-failed",
                    "could not read this machine's devices ({e}); the node agent keeps the GPU and devices it was created with"
                );
                None
            }
        };
        let (mut after, mut changed) = observed.unwrap_or_else(|| (machine.inputs.clone(), vec![]));

        let _gate = self.gate.lock().unwrap();
        let now = match self.admissible() {
            Ok(m) => m,
            Err(r) => {
                warn!(token = "actor-start-check-busy", "{}", r.message);
                return None;
            }
        };
        if now.inputs != machine.inputs {
            warn!(
                token = "actor-start-check-inputs-moved",
                "machine inputs changed while this start looked at the node agent; the next start looks again"
            );
            return None;
        }
        if let Some(open) = self.journals.scan().open_id() {
            warn!(
                token = "actor-start-check-attempt-open",
                "attempt {open} is in flight; the next start looks at the node agent again"
            );
            return None;
        }
        let mut replaced = match self.agent_moved_by(&machine.inputs, &after) {
            Ok(replaced) => replaced,
            Err(why) => {
                warn!(
                    token = "actor-start-check-unrenderable",
                    "{why}; the node agent keeps the GPU and devices it was created with"
                );
                after = machine.inputs.clone();
                changed.clear();
                Vec::new()
            }
        };
        if replaced.is_empty() {
            match self.start_if_stopped(&machine) {
                Agent::Fine | Agent::Started => {}
                Agent::Unstartable(e) => {
                    // The engine's refusal only prompts asking it again; the probe decides.
                    if e.is_device_request_refusal() && after.gpu.nvidia_shape() {
                        after.gpu.gpus_served = false;
                        match self.decide_gpu_facts(
                            &mut after.gpu,
                            &record.image,
                            record.recipe_revision,
                        ) {
                            Ok(_) => {
                                if after.gpu != machine.inputs.gpu
                                    && !changed.iter().any(|c| c == GPU_CHANGED)
                                {
                                    changed.push(GPU_CHANGED.to_string());
                                }
                                replaced = self
                                    .agent_moved_by(&machine.inputs, &after)
                                    .unwrap_or_default();
                            }
                            Err(e) => {
                                warn!(
                                    token = "actor-gpu-reask-failed",
                                    "could not ask the engine about this machine's GPU ({e}); re-creating the node agent as it was"
                                );
                                after.gpu = machine.inputs.gpu.clone();
                            }
                        }
                    }
                    if replaced.is_empty() {
                        replaced = vec![Role::NodeAgent];
                        changed.push(AGENT_UNSTARTABLE.to_string());
                    }
                }
            }
        }
        if changed.is_empty() {
            return None;
        }
        let re_created = !replaced.is_empty();
        if changed.iter().any(|c| c == GPU_CHANGED) {
            let (was, now) = (&machine.inputs.gpu, &after.gpu);
            warn!(
                token = "actor-gpu-changed",
                was_vendor = ?was.vendor,
                was_render_node = was.effective_render_node().unwrap_or(""),
                was_nvidia_shape = was.nvidia_shape(),
                vendor = ?now.vendor,
                render_node = now.effective_render_node().unwrap_or(""),
                nvidia_shape = now.nvidia_shape(),
                re_created,
                "this machine's GPU is not the one the node agent was created for"
            );
        }
        if changed.iter().any(|c| c == DEVICES_CHANGED) {
            info!(
                token = "console-devices-changed",
                i2c = ?after.devices.i2c,
                sound = after.devices.sound,
                logind = after.devices.logind,
                console_audio = after.devices.console_audio,
                console_vt = after.devices.console_vt,
                re_created,
                "the host's console devices changed since the agent was created"
            );
        }
        match self.admit(&machine, after, &changed, &replaced) {
            Ok(id) => id,
            Err(r) => {
                warn!(token = "actor-start-recreate-refused", "{}", r.message);
                None
            }
        }
    }

    /// The inputs `report` gives this machine now, and what moved (the names recorded in
    /// `reconfigure.json`'s `changed`). Asks the engine about an NVIDIA GPU as a create does.
    fn observe(
        &self,
        before: &Inputs,
        report: &ProbeReport,
        record: &ServiceRecord,
    ) -> Result<(Inputs, Vec<String>), crate::actor::ResumeError> {
        let mut after = before.clone();
        let mut changed = Vec::new();
        let (fresh, devices) = probe::select(report);
        after.gpu = observed_gpu(&before.gpu, fresh);
        self.decide_gpu_facts(&mut after.gpu, &record.image, record.recipe_revision)?;
        after.devices.dri = devices.dri;
        // A machine recorded before the DRM nodes were is left on the `/dev/dri` directory
        // until its GPU changes: re-creating it for the list alone would be churn.
        if after.gpu != before.gpu || !before.devices.dri_nodes.is_empty() {
            after.devices.dri_nodes = devices.dri_nodes;
        }
        if after != *before {
            changed.push(GPU_CHANGED.to_string());
        }
        if before.console {
            let gpu_only = after.clone();
            ConsoleDevices::from_report(report).apply(&mut after.devices);
            after.keep_console_vt();
            if after != gpu_only {
                changed.push(DEVICES_CHANGED.to_string());
            }
        }
        Ok((after, changed))
    }

    fn agent_moved_by(&self, before: &Inputs, after: &Inputs) -> Result<Vec<Role>, String> {
        Ok(self
            .moved_by(before, after)?
            .into_iter()
            .filter(|r| *r == Role::NodeAgent)
            .collect())
    }

    fn start_if_stopped(&self, machine: &Machine) -> Agent {
        let agent = match self.engine.inspect_container(names::NODE_AGENT) {
            Ok(Some(c)) => c,
            Ok(None) => return Agent::Fine,
            Err(e) => {
                warn!(
                    token = "actor-agent-state-unknown",
                    "the engine did not say whether the node agent runs ({e}); the next start looks again"
                );
                return Agent::Fine;
            }
        };
        if agent.running {
            return Agent::Fine;
        }
        let stopped_from_outside = self.is_ours(machine, &agent, Role::NodeAgent)
            && agent.restart == Some(RestartPolicy::UnlessStopped)
            && matches!(agent.status.as_str(), "exited" | "created");
        if !stopped_from_outside {
            info!(
                token = "actor-agent-left-stopped",
                state = %agent.status,
                "the node agent is not running, and was stopped by Quasar or is not this installation's; it is left as it is"
            );
            return Agent::Fine;
        }
        match self.engine.start_container(&agent.id) {
            Ok(()) => {
                info!(
                    token = "actor-agent-started",
                    was = %agent.status,
                    "the node agent was not running; started it"
                );
                Agent::Started
            }
            Err(e) => {
                warn!(
                    token = "actor-agent-unstartable",
                    was = %agent.status,
                    "the node agent is not running and the engine will not start it ({e}); re-creating it through a verified replacement"
                );
                Agent::Unstartable(e)
            }
        }
    }

    /// This machine's services, by container name, whose container is not running. The
    /// recovery actor and containers kept by an attempt are not counted.
    pub fn services_not_running(&self) -> Result<Vec<String>, EngineError> {
        let machine = self.dir.load_machine().ok().flatten();
        let Some(machine) = machine else {
            return Ok(Vec::new());
        };
        let mut out = Vec::new();
        for role in [Role::Postgres, Role::ControlPlane, Role::NodeAgent] {
            if let Some(c) = self.engine.inspect_container(role.container_name())? {
                if self.is_ours(&machine, &c, role) && !c.running {
                    out.push(format!("{} ({})", c.name, c.status));
                }
            }
        }
        Ok(out)
    }
}

/// `recorded` with the GPU `fresh` found. The engine's answer about the NVIDIA GPU is kept
/// only while there still is one; fields a newer actor wrote are kept.
fn observed_gpu(recorded: &GpuFacts, fresh: GpuFacts) -> GpuFacts {
    let nvidia = Some(crate::recipe::GpuVendor::Nvidia);
    let still_nvidia = recorded.vendor == nvidia && fresh.vendor == nvidia;
    let mut gpu = recorded.clone();
    gpu.vendor = fresh.vendor;
    gpu.render_node = fresh.render_node;
    let same_fallback = match (&recorded.fallback, &fresh.fallback) {
        (Some(a), Some(b)) => a.vendor == b.vendor && a.render_node == b.render_node,
        (None, None) => true,
        _ => false,
    };
    if !same_fallback {
        gpu.fallback = fresh.fallback;
    }
    if !still_nvidia {
        gpu.gpus_served = false;
        gpu.cdi = false;
    }
    gpu
}
