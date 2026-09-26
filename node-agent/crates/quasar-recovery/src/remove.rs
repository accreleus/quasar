//! The console's "remove host" on the recovery actor's side (agent-api.md §`host_remove`,
//! amendment 14): a `kind: remove` request on the agent socket.
//!
//! Only on a GPU host, and only from the agent socket: a machine that runs a control plane
//! is taken apart with the operator's `uninstall` on that machine. The request carries no
//! components and never purges: it removes the node agent, then the recovery actor, and
//! nothing else, exactly as `uninstall` without `--purge` does ([`crate::uninstall`]).
//!
//! Admission writes the uninstall marker and sets `seed.json` to `uninstalled` **before**
//! answering, so from the moment the agent is told "accepted" nothing brings the services
//! back: not the seed, not this actor restarting (`resume` installs nothing on a marked
//! machine). Then, on a background thread and after a short grace so the agent's `ack`
//! reaches the control plane, it disables its own restart, removes the node agent (which
//! ends the agent's connection: that is the control plane's evidence), and removes its own
//! container, which ends this process. A removal that stops part-way leaves the host
//! visibly there, and `uninstall` on the machine finishes it.

use std::sync::Arc;
use std::time::Duration;

use tracing::{error, info, warn};

use crate::actor::Actor;
use crate::engine::RestartPolicy;
use crate::recipe::Role;
use crate::socket::{Accepted, MachineRole, Reason, Rejection, Request};
use crate::trust::Caller;
use crate::uninstall::{self, By, Marker, MARKER_FORMAT};

/// Between answering the agent and removing it, so its `ack` reaches the control plane.
pub const DEFAULT_GRACE: Duration = Duration::from_secs(3);

fn refuse(req: &Request, reason: Reason, message: impl Into<String>) -> Rejection {
    Rejection {
        request_id: req.request_id.clone(),
        reason,
        message: message.into(),
    }
}

impl Actor {
    /// `submit` for `kind: remove`. The checks change nothing; the marker is committed
    /// before `Accepted` is returned.
    pub(crate) fn submit_remove(
        self: &Arc<Self>,
        caller: Caller,
        req: Request,
    ) -> Result<Accepted, Rejection> {
        if caller != Caller::Agent {
            return Err(refuse(
                &req,
                Reason::Invalid,
                "a machine that runs the control plane is uninstalled with the uninstall command on that machine, never over the control socket; nothing was changed",
            ));
        }
        if !req.components.is_empty() || req.purge || req.dump.is_some() {
            return Err(refuse(
                &req,
                Reason::Invalid,
                "a removal names no components and never purges or restores: it removes this host's node agent and recovery actor; nothing was changed",
            ));
        }
        let machine = match self.dir.load_machine() {
            Ok(Some(m)) => m,
            Ok(None) => {
                return Err(refuse(
                    &req,
                    Reason::Invalid,
                    "this machine is not installed; there is nothing to remove",
                ))
            }
            Err(e) => {
                return Err(refuse(
                    &req,
                    Reason::Invalid,
                    format!("machine state is unreadable: {e}"),
                ))
            }
        };
        if machine.role != MachineRole::Gpu {
            return Err(refuse(
                &req,
                Reason::Invalid,
                "this machine also runs the control plane; take it apart with the uninstall command on the machine. Nothing was changed",
            ));
        }
        // Idempotent: a re-sent command after a lost ack is the same removal.
        if let Ok(Some(marker)) = self.dir.load_uninstall() {
            info!(request = %req.request_id, "a removal was already recorded; answering it again");
            if marker.by == By::Console && marker.request_id.as_deref() == Some(&req.request_id) {
                self.start_removal();
            }
            return Ok(Accepted {
                request_id: req.request_id,
                previous: Vec::new(),
            });
        }
        let scan = self.journals.scan();
        if let Some(open) = scan.open_id() {
            return Err(refuse(
                &req,
                Reason::Busy,
                format!("attempt {open} is in flight; a removal waits for it. Nothing was changed"),
            ));
        }
        let own_image = self
            .config
            .self_container
            .as_deref()
            .and_then(|me| self.engine.inspect_container(me).ok().flatten())
            .and_then(|me| crate::actor::own_image(self.engine.as_ref(), &me));
        let marker = Marker {
            format: MARKER_FORMAT,
            by: By::Console,
            request_id: Some(req.request_id.clone()),
            purge: false,
            dump: None,
            started_at: (self.config.now)(),
            finished_at: None,
        };
        if let Err(e) = uninstall::mark(&self.dir, &machine, &marker, own_image) {
            return Err(refuse(
                &req,
                Reason::Busy,
                format!("the removal could not be recorded ({e}); nothing was changed"),
            ));
        }
        info!(request = %req.request_id, "removal of this host's platform services accepted");
        self.start_removal();
        Ok(Accepted {
            request_id: req.request_id,
            previous: Vec::new(),
        })
    }

    fn start_removal(self: &Arc<Self>) {
        let actor = self.clone();
        let mut worker = self.worker.lock().unwrap();
        if let Some(done) = worker.take() {
            if !done.is_finished() {
                // The removal (or an attempt) is still being driven: nothing to start.
                *worker = Some(done);
                return;
            }
            let _ = done.join();
        }
        *worker = Some(std::thread::spawn(move || actor.remove_services()));
    }

    /// The node agent, then this actor. Every step is "remove if present".
    fn remove_services(&self) {
        std::thread::sleep(self.config.remove_grace);
        let Ok(Some(machine)) = self.dir.load_machine() else {
            return;
        };
        let me = self.config.self_container.clone();
        if let Some(me) = &me {
            if let Err(e) = self.engine.set_restart_policy(me, RestartPolicy::No) {
                warn!(
                    token = "remove-restart-not-disabled",
                    "could not disable this actor's restart: {e}"
                );
            }
        }
        let containers = match self.engine.list_containers() {
            Ok(c) => c,
            Err(e) => {
                error!(token = "remove-stopped", "the container engine did not answer ({e}); this host stays removed-in-part, and `uninstall` on the machine finishes it");
                return;
            }
        };
        let grace = self.config.timing.stop_grace;
        for c in uninstall::ours(&containers, &machine.installation_id, Role::NodeAgent) {
            match uninstall::stop_and_remove(self.engine.as_ref(), c, grace) {
                Ok(()) => info!(container = %c.name, "node agent removed"),
                Err(e) => {
                    error!(token = "remove-agent-failed", container = %c.name, "the node agent could not be removed ({e}); this host stays removed-in-part, and `uninstall` on the machine finishes it");
                    return;
                }
            }
        }
        for h in uninstall::helpers(&containers) {
            let _ = self.engine.remove_container(&h.id);
        }
        let is_me = |id: &str| {
            me.as_deref()
                .is_some_and(|m| id.starts_with(m) || m.starts_with(id))
        };
        // Any other actor container of this installation (a kept or successor one), then
        // this one, which ends this process.
        for c in uninstall::ours(&containers, &machine.installation_id, Role::RecoveryActor) {
            if !is_me(&c.id) {
                let _ = uninstall::stop_and_remove(self.engine.as_ref(), c, grace);
            }
        }
        if let Ok(Some(mut marker)) = self.dir.load_uninstall() {
            marker.finished_at = Some((self.config.now)());
            let _ = self.dir.store_uninstall(&marker);
        }
        match &me {
            Some(me) => {
                info!("removing this recovery actor's own container; the removal is done");
                if let Err(e) = self.engine.remove_container(me) {
                    error!(token = "remove-self-failed", "this recovery actor could not remove its own container ({e}); it stays stopped-in-place, installs nothing, and `docker rm -f {}` finishes the removal", crate::recipe::names::RECOVERY_ACTOR);
                }
            }
            None => warn!(
                token = "remove-self-unknown",
                "this recovery actor cannot tell its own container, so it is left; `docker rm -f {}` finishes the removal",
                crate::recipe::names::RECOVERY_ACTOR
            ),
        }
    }
}
