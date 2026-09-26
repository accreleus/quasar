//! `Actor::submit` (architecture §5.2): admit one request, journal it, drive it in the
//! background.
//!
//! The order is the security argument, so it is fixed here and tested:
//!
//! 1. A request id that is not a uuid never reaches the filesystem (it names a journal
//!    file); [`trust::admit`] refuses it.
//! 2. **A re-post of a known request id is answered from the journal, checked against
//!    the caller, before [`trust::admit`] runs** (the #356 security review). `admit`
//!    treats a re-post of the in-flight id as "not busy", as the Go updater's
//!    `AcceptedFor` did; without this step a node agent could reuse the control plane's
//!    in-flight id and have a second request admitted. The same caller gets the same
//!    `Accepted`, and nothing new happens; another caller is refused. An id whose
//!    journal was pruned is still spent (the used-id record), and so is one any
//!    container carries as its attempt label: never admitted again.
//! 3. From `acquire_lease` until `resume` returns, every submit is `busy`; so is every
//!    submit while any journal is unreadable (it may be the open attempt).
//! 4. **Rules per kind and per caller**, which `admit` does not look at. A `remove`
//!    (`host_remove`) is admitted by [`crate::remove`] before any of them, and on a machine
//!    being uninstalled nothing else is. The agent socket may send `replace` (agent-api.md
//!    `release_apply`) and `remove`; never `restore`, which loads a pre-update dump on the
//!    control plane's machine. It may name only `node-agent` and `recovery-actor`; the
//!    control socket only `control-plane` and `recovery-actor`. What a caller may name but
//!    this build cannot yet do is refused `invalid`, saying which ticket brings it.
//! 5. Every image a registry host plus well-formed path components, then
//!    [`trust::admit`]: single flight, the component table and the confused-deputy guard,
//!    image and digest shape, the namespace allowlist, then ADR 0003 signatures.
//! 6. The race guard at submission ([`crate::race_guard`]): any owner conflict on the
//!    machine, or a container holding a name this replacement needs without this
//!    installation's labels, is `owner_conflict`.
//!
//! Every refusal happens before the first journal record, so a refusal changed nothing.

use std::sync::atomic::Ordering;
use std::sync::Arc;

use tracing::{info, warn};

use crate::actor::Actor;
use crate::journal::{CallerTag, Journal, Phase, Step, FORMAT};
use crate::recipe::{labels, ImageRef, Role};
use crate::replace::ATTEMPT_LABEL;
use crate::socket::{
    Accepted, AttemptResult, Previous, Reason, Rejection, Request, RequestKind, State,
};
use crate::trust::{self, Caller};

/// The components each socket may name at all (architecture §5.2).
fn may_name(caller: Caller, name: &str) -> bool {
    match caller {
        Caller::Agent => matches!(name, "node-agent" | "recovery-actor"),
        Caller::ControlPlane => matches!(name, "control-plane" | "recovery-actor"),
    }
}

pub(crate) fn is_uuid(s: &str) -> bool {
    let groups = [8usize, 4, 4, 4, 12];
    let parts: Vec<&str> = s.split('-').collect();
    parts.len() == groups.len()
        && parts
            .iter()
            .zip(groups)
            .all(|(p, n)| p.len() == n && p.bytes().all(|b| b.is_ascii_hexdigit()))
}

fn tag(caller: Caller) -> CallerTag {
    match caller {
        Caller::ControlPlane => CallerTag::ControlPlane,
        Caller::Agent => CallerTag::Agent,
    }
}

fn refuse(req: &Request, reason: Reason, message: impl Into<String>) -> Rejection {
    Rejection {
        request_id: req.request_id.clone(),
        reason,
        message: message.into(),
    }
}

/// Step 4: what `admit` does not decide.
fn kind_and_caller_rules(caller: Caller, req: &Request) -> Result<(), Rejection> {
    match (caller, req.kind) {
        (_, RequestKind::Replace) => {}
        (Caller::Agent, RequestKind::Restore) => {
            return Err(refuse(
                req,
                Reason::Invalid,
                "the agent socket may not ask for a restore: a restore loads a pre-update dump on the control plane's machine and is the operator's command",
            ))
        }
        // Admitted by `submit_remove` before these rules.
        (Caller::Agent, RequestKind::Remove) => {}
        (Caller::ControlPlane, kind) => {
            return Err(refuse(
                req,
                Reason::Invalid,
                format!("a {kind:?} request on the control socket is not in this build; nothing was changed"),
            ))
        }
    }
    for c in &req.components {
        // A name no socket knows is `admit`'s to refuse ("unknown component").
        if !matches!(
            c.name.as_str(),
            "node-agent" | "recovery-actor" | "control-plane"
        ) {
            continue;
        }
        if !may_name(caller, &c.name) {
            let socket = match caller {
                Caller::Agent => "the agent socket: a node agent asking to replace it is a confused deputy",
                Caller::ControlPlane => "the control socket: a host's node agent is replaced over that host's own agent socket",
            };
            return Err(refuse(
                req,
                Reason::Invalid,
                format!("component \"{}\" may not be named on {socket}", c.name),
            ));
        }
        let not_yet = match c.name.as_str() {
            "recovery-actor" => Some("replacing the recovery actor (its hand-over to a successor) arrives with RH06-10 (#362)"),
            "control-plane" => Some("replacing the control plane arrives with RH06-11 (#363)"),
            _ => None,
        };
        if let Some(why) = not_yet {
            return Err(refuse(
                req,
                Reason::Invalid,
                format!("component \"{}\": {why}; nothing was changed", c.name),
            ));
        }
    }
    Ok(())
}

impl Actor {
    /// Admits one request and, if admitted, journals it and drives it on a background
    /// thread. Idempotent on `request_id` for the caller that submitted it; single flight
    /// otherwise. See the module documentation for the order of the checks.
    pub fn submit(self: &Arc<Self>, caller: Caller, req: Request) -> Result<Accepted, Rejection> {
        let _gate = self.gate.lock().unwrap();

        // 1-2. The journal answers a re-post, before anything grades the request.
        if is_uuid(&req.request_id) {
            match self.journals.load(&req.request_id) {
                Ok(Some(known)) if known.caller == tag(caller) => {
                    info!(request = %req.request_id, "a re-post of a known request; answered from the journal, nothing new is started");
                    return Ok(Accepted {
                        request_id: req.request_id.clone(),
                        previous: known.result.previous,
                    });
                }
                Ok(Some(known)) => {
                    warn!(
                        token = "actor-submit-foreign-request-id",
                        request = %req.request_id,
                        "a request id another caller submitted was posted on this socket; refused"
                    );
                    return Err(if known.is_open() {
                        refuse(
                            &req,
                            Reason::Busy,
                            format!("request {} is still in flight", req.request_id),
                        )
                    } else {
                        refuse(
                            &req,
                            Reason::Invalid,
                            format!(
                                "request id {} was already used by another caller",
                                req.request_id
                            ),
                        )
                    });
                }
                Ok(None) => {}
                Err(e) => {
                    let why = format!("the journal for {} cannot be read ({e})", req.request_id);
                    return Err(refuse(&req, Reason::Busy, why));
                }
            }
            // A pruned journal's id is still spent: admitting it again would adopt the
            // containers the earlier attempt labelled.
            match self.journals.was_used(&req.request_id) {
                Ok(false) => {}
                Ok(true) => {
                    return Err(refuse(
                        &req,
                        Reason::Invalid,
                        format!(
                            "request id {} was already used on this machine",
                            req.request_id
                        ),
                    ))
                }
                Err(e) => {
                    let why = format!("the record of used request ids cannot be read ({e})");
                    return Err(refuse(&req, Reason::Busy, why));
                }
            }
        }

        // 3.
        if self.resuming.load(Ordering::SeqCst) {
            return Err(refuse(
                &req,
                Reason::Busy,
                "the recovery actor is still settling this machine after a start",
            ));
        }

        if req.kind == RequestKind::Remove && is_uuid(&req.request_id) {
            return self.submit_remove(caller, req);
        }
        if let Some(why) = crate::uninstall::uninstalled(&self.dir) {
            return Err(refuse(
                &req,
                Reason::Invalid,
                format!("{why}; nothing is replaced on it"),
            ));
        }

        // 4.
        if is_uuid(&req.request_id) {
            kind_and_caller_rules(caller, &req)?;
        }

        // 5. An unreadable journal fails closed: it may be the open attempt.
        let scan = self.journals.scan();
        if let Some(id) = scan.unreadable.first() {
            return Err(refuse(
                &req,
                Reason::Busy,
                format!("attempt journal {id} cannot be read (corrupt, or written by a newer actor); nothing is admitted until it can"),
            ));
        }
        for c in &req.components {
            if !repository_well_formed(&c.image) {
                return Err(refuse(
                    &req,
                    Reason::Invalid,
                    format!(
                        "component \"{}\": image {:?} is not a registry host followed by lowercase path components",
                        c.name, c.image
                    ),
                ));
            }
        }
        let settings = self
            .trust()
            .map_err(|why| refuse(&req, Reason::Invalid, format!("release trust: {why}")))?;
        let cfg = trust::Config {
            allowed_namespaces: settings.allowed_namespaces,
            in_flight_request_id: scan.open().map(|j| j.request.request_id.clone()),
            signature: settings.signature,
        };
        let evidence = trust::wants_signature_evidence(&cfg, &req.request_id)
            .then(|| (self.config.evidence)(&req));
        // An unverified apply under `verify` is logged by `admit` itself.
        let _admitted = trust::admit(caller, &req, &cfg, evidence.as_ref())
            .map_err(|r| refuse(&req, r.reason, r.message))?;

        // 6. and the machine facts the attempt needs.
        let machine = match self.dir.load_machine() {
            Ok(Some(m)) => m,
            Ok(None) => {
                return Err(refuse(
                    &req,
                    Reason::Invalid,
                    "this machine is not installed yet; nothing can be replaced",
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
        match self.engine.list_containers() {
            Ok(all) => {
                let conflicts = crate::race_guard::conflicts(
                    &all,
                    Some(&machine.installation_id),
                    self.config.self_container.as_deref(),
                );
                if !conflicts.is_empty() {
                    return Err(refuse(
                        &req,
                        Reason::OwnerConflict,
                        crate::race_guard::refusal(&conflicts),
                    ));
                }
                if let Some(c) = all.iter().find(|c| {
                    c.labels.get(ATTEMPT_LABEL).map(String::as_str) == Some(req.request_id.as_str())
                }) {
                    return Err(refuse(
                        &req,
                        Reason::Invalid,
                        format!(
                            "request id {} was already used on this machine (container {} carries it)",
                            req.request_id, c.name
                        ),
                    ));
                }
            }
            Err(e) => {
                return Err(refuse(
                    &req,
                    Reason::Busy,
                    format!("the container engine did not answer ({e}); nothing was admitted"),
                ))
            }
        }
        let mut steps = Vec::with_capacity(req.components.len());
        let mut previous = Vec::with_capacity(req.components.len());
        for c in &req.components {
            let role = Role::parse(&c.name).ok_or_else(|| {
                refuse(
                    &req,
                    Reason::Invalid,
                    format!("unknown component {:?}", c.name),
                )
            })?;
            let canonical = role.container_name();
            let mut old_digest = None;
            for name in [canonical.to_string(), kept_name(canonical)] {
                match self.engine.inspect_container(&name) {
                    Ok(Some(existing)) => {
                        if existing.labels.get(labels::INSTALLATION)
                            != Some(&machine.installation_id)
                        {
                            return Err(refuse(
                                &req,
                                Reason::OwnerConflict,
                                format!(
                                    "container {} ({}) is not this installation's; it is never acted on. Remove it, or the manager definition that recreates it",
                                    existing.name, existing.image
                                ),
                            ));
                        }
                        if name == canonical {
                            old_digest = existing.image.split_once('@').map(|(_, d)| d.to_owned());
                        }
                    }
                    Ok(None) => {}
                    Err(e) => {
                        return Err(refuse(
                            &req,
                            Reason::Busy,
                            format!(
                                "the container engine did not answer ({e}); nothing was admitted"
                            ),
                        ))
                    }
                }
            }
            previous.push(Previous {
                name: c.name.clone(),
                digest: old_digest.clone(),
            });
            steps.push(Step {
                name: c.name.clone(),
                image: ImageRef {
                    repository: c.image.clone(),
                    digest: c.digest.clone(),
                },
                phase: Phase::Admitted,
                old_container: None,
                old_restart: None,
                old_digest,
                revision: None,
                spec: None,
                new_container: None,
                failure: None,
            });
        }

        let now = (self.config.now)();
        let journal = Journal {
            format: FORMAT,
            seq: self.journals.next_seq(),
            caller: tag(caller),
            request: req.clone(),
            steps,
            result: AttemptResult {
                request_id: req.request_id.clone(),
                state: State::Pending,
                reason: None,
                components: req.components.clone(),
                previous: previous.clone(),
                output: String::new(),
                started_at: now.clone(),
                updated_at: now,
                finished_at: None,
                restored: false,
                release: req.release.clone(),
            },
        };
        // Committed before `Accepted` is returned: an attempt the caller was told about
        // exists on disk whatever happens next.
        if let Err(e) = self.journals.store(&journal) {
            return Err(refuse(
                &req,
                Reason::Busy,
                format!("the journal could not be written ({e}); nothing was admitted"),
            ));
        }
        // After the journal: until pruned, the journal itself answers a re-post, and
        // `prune` records the id again before it deletes one.
        if let Err(e) = self.journals.mark_used(&req.request_id) {
            warn!(token = "actor-used-id-unrecorded", request = %req.request_id, "the used request id could not be recorded yet ({e}); pruning records it");
        }
        info!(
            request = %req.request_id,
            caller = ?caller,
            components = ?req.components.iter().map(|c| c.name.as_str()).collect::<Vec<_>>(),
            "attempt admitted"
        );

        let actor = self.clone();
        let id = req.request_id.clone();
        let mut worker = self.worker.lock().unwrap();
        if let Some(done) = worker.take() {
            let _ = done.join();
        }
        *worker = Some(std::thread::spawn(move || actor.drive(&id)));
        Ok(Accepted {
            request_id: req.request_id,
            previous,
        })
    }

    /// Waits for the attempt `submit` started to stop being driven: terminal, or (in a
    /// test) the injected crash that stands for the process dying.
    pub fn wait_attempt(&self) {
        let handle = self.worker.lock().unwrap().take();
        if let Some(handle) = handle {
            let _ = handle.join();
        }
    }
}

/// A registry host, then lowercase path components in the distribution reference
/// grammar: no empty, `.` or `..` segment, so a prefix match on the namespace
/// allowlist cannot be walked out of.
fn repository_well_formed(image: &str) -> bool {
    let mut parts = image.split('/');
    let host = parts.next().unwrap_or("");
    let host_ok = !host.is_empty()
        && host
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-' || b == b':')
        && !host.starts_with(['.', '-', ':']);
    let path: Vec<&str> = parts.collect();
    host_ok && !path.is_empty() && path.iter().all(|p| path_component(p))
}

fn path_component(p: &str) -> bool {
    let b = p.as_bytes();
    let alnum = |c: u8| c.is_ascii_lowercase() || c.is_ascii_digit();
    if b.is_empty() || !alnum(b[0]) || !alnum(b[b.len() - 1]) {
        return false;
    }
    // Separators are `.`, `_`, `__` or a run of `-`, always between alphanumerics.
    let mut i = 0;
    while i < b.len() {
        if alnum(b[i]) {
            i += 1;
            continue;
        }
        let start = i;
        while i < b.len() && !alnum(b[i]) {
            i += 1;
        }
        let sep = &p[start..i];
        let ok = sep == "." || sep == "_" || sep == "__" || sep.bytes().all(|c| c == b'-');
        if !ok {
            return false;
        }
    }
    true
}

pub(crate) fn kept_name(canonical: &str) -> String {
    format!("{canonical}.kept")
}
