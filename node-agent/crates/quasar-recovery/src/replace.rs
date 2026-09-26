//! One Replacement (architecture §5.5): the node agent's or the control plane's, driven
//! from the journal. The recovery actor's own is a hand-over (`crate::handover`).
//!
//! ```text
//! admitted → pulling → checked → old_kept → created → started → verifying → verified
//!          → old_discarded → succeeded
//!                        on a failed verification: → restoring → failed (restored)
//! ```
//!
//! Every phase is journalled (fsync) **before** it is acted on, and every step is
//! idempotent, so the same code serves a fresh attempt and one `resume` continues after a
//! crash ([`crate::settle`]). The old container is **stopped and kept** — its restart
//! policy disabled so an engine or machine restart cannot bring it back, renamed
//! `<name>.kept` — until the new one is verified; restoring it is renaming it back and
//! starting it, with no pull (ADR 0004 and its RH06 amendment: a node agent that fails
//! verification is always restored, and so is a control plane that never passed a health
//! check, which submit admits only for a release that does not migrate). Nothing is
//! retried as a new attempt: an engine call that fails transiently is asked again inside
//! its step, and that is all.

use std::time::{Duration, Instant};

use tracing::{error, info, warn};

use crate::actor::{Actor, ResumeError};
use crate::engine::{Container, EngineError, RestartPolicy};
use crate::handover::Flow;
use crate::journal::{embedded_log_tail, tail_output, Failure, Journal, Phase, OUTPUT_LIMIT};
use crate::recipe::{self, labels, Book, RenderError, Role};
use crate::settle::{settle, Settlement, RECOVERY_ACTOR};
use crate::socket::{Reason, State};
use crate::submit::kept_name;

/// On a replacement's own containers: the request that created (`.next`/new) it.
pub const ATTEMPT_LABEL: &str = "io.quasar.attempt";

/// How a step stopped short.
pub(crate) enum Halt {
    /// The process "died" here (`EngineError::Crashed`, injected by tests), or the journal
    /// could not be written: stop driving and leave the journal as it is.
    Died,
    Fail(Failure),
}

pub(crate) fn fail(reason: Reason, detail: impl Into<String>) -> Halt {
    Halt::Fail(Failure {
        reason,
        detail: detail.into(),
    })
}

/// An engine error inside a step: a crash stops everything; anything else fails the step.
pub(crate) fn engine(e: EngineError, reason: Reason, what: &str) -> Halt {
    match e {
        EngineError::Crashed => Halt::Died,
        e => fail(reason, format!("{what}: {e}")),
    }
}

impl Actor {
    /// The background thread `submit` starts.
    pub(crate) fn drive(&self, request_id: &str) {
        let journal = match self.journals.load(request_id) {
            Ok(Some(j)) => j,
            Ok(None) => return,
            Err(e) => {
                error!(token = "actor-journal-reload-failed", request = %request_id, "{e}");
                return;
            }
        };
        if self.run(journal).is_err() {
            warn!(
                token = "actor-attempt-stopped",
                request = %request_id,
                "this attempt stopped being driven; the next start settles it"
            );
            self.died();
        }
    }

    /// `resume`'s D8 step: the open attempt, if any, reaches a terminal outcome.
    pub(crate) fn settle_open(&self) -> Result<(), ResumeError> {
        let scan = self.journals.scan();
        if !scan.unreadable.is_empty() {
            warn!(
                token = "actor-journal-unreadable",
                journals = ?scan.unreadable,
                "an attempt journal cannot be read (corrupt, or written by a newer actor); nothing is settled, installed or admitted until it is, so a kept container it may depend on is never touched"
            );
            return Err(ResumeError::State(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                format!("unreadable attempt journal(s) {:?}", scan.unreadable),
            )));
        }
        let Some(journal) = scan.open().cloned() else {
            return Ok(());
        };
        self.settle_journal(journal).map_err(|()| {
            self.died();
            stopped()
        })
    }

    /// Drive an open journal this process just took the lease for to its outcome, by the
    /// settle table for this process's party.
    pub(crate) fn settle_journal(&self, mut journal: Journal) -> Result<(), ()> {
        let id = journal.request.request_id.clone();
        let party = self.party(&journal);
        self.count_successor_start(&mut journal, party)?;
        match settle(&journal, party) {
            Settlement::Terminal => Ok(()),
            Settlement::Interrupted => {
                info!(request = %id, "settling an attempt interrupted before its current component was touched");
                self.interrupt(journal)
            }
            Settlement::Continue => {
                info!(request = %id, ?party, "continuing an attempt a restart interrupted");
                self.run(journal)
            }
            Settlement::Restore => {
                info!(request = %id, ?party, "the recovery actor's hand-over did not finish; putting the previous actor back");
                self.begin_actor_restore(&mut journal, party)?;
                self.run(journal)
            }
            Settlement::Yield => {
                info!(request = %id, ?party, "another recovery actor finishes this attempt; handing the machine to it");
                self.yield_attempt(journal, party)
            }
        }
    }

    pub(crate) fn now(&self) -> String {
        (self.config.now)()
    }

    /// Commit the journal. A journal that cannot be written stops the attempt: acting on
    /// a phase that is not on disk is the one thing D8 forbids.
    pub(crate) fn commit(&self, j: &mut Journal) -> Result<(), ()> {
        // A process that is gone writes nothing: another actor may own the attempt now.
        if self.killed() {
            return Err(());
        }
        j.project();
        j.result.updated_at = self.now();
        self.journals.store(j).map_err(|e| {
            error!(
                token = "actor-journal-write-failed",
                request = %j.request.request_id,
                "the attempt journal could not be written ({e}); the attempt stops here and the next start settles it"
            );
        })
    }

    pub(crate) fn advance(&self, j: &mut Journal, i: usize, phase: Phase) -> Result<(), ()> {
        j.steps[i].phase = phase;
        self.commit(j)?;
        #[cfg(any(test, feature = "test-support"))]
        if let Some(crash) = &self.config.crash_after {
            if crash(&j.steps[i].name, phase) {
                return Err(());
            }
        }
        Ok(())
    }

    pub(crate) fn finish(
        &self,
        j: &mut Journal,
        state: State,
        reason: Option<Reason>,
        output: String,
        restored: bool,
    ) -> Result<(), ()> {
        if self.killed() {
            return Err(());
        }
        for step in j.steps.iter_mut() {
            if state == State::Failed && step.phase != Phase::Done {
                step.phase = Phase::Done;
            }
        }
        let now = self.now();
        j.result.state = state;
        j.result.reason = reason;
        j.result.output = tail_output(&output, OUTPUT_LIMIT);
        j.result.restored = restored;
        j.result.updated_at = now.clone();
        j.result.finished_at = Some(now);
        self.journals.store(j).map_err(|e| {
            error!(
                token = "actor-journal-outcome-unwritten",
                request = %j.request.request_id,
                "the attempt's outcome could not be written ({e}); the next start settles it again"
            );
        })?;
        self.journals.prune();
        crate::handover::forget_ready(self.dir.root(), &j.request.request_id);
        match state {
            State::Succeeded => info!(request = %j.request.request_id, "attempt succeeded"),
            _ => warn!(
                token = "actor-attempt-failed",
                request = %j.request.request_id,
                reason = %j.result.reason.as_ref().map(|r| r.as_str()).unwrap_or(""),
                restored,
                "attempt failed"
            ),
        }
        Ok(())
    }

    pub(crate) fn retrying<T>(
        &self,
        mut op: impl FnMut() -> Result<T, EngineError>,
    ) -> Result<T, EngineError> {
        let timing = self.config.timing;
        let mut attempt = 0;
        loop {
            match op() {
                Err(e) if e.is_transient() && attempt < timing.retries => {
                    attempt += 1;
                    warn!(
                        token = "actor-engine-retry",
                        attempt, "the engine failed transiently ({e}); asking again"
                    );
                    std::thread::sleep(timing.retry_backoff * attempt);
                }
                other => return other,
            }
        }
    }

    /// Drive `j` to its terminal outcome. `Err` is [`Halt::Died`]: the journal is left for
    /// the next start.
    pub(crate) fn run(&self, mut j: Journal) -> Result<(), ()> {
        if j.restore.is_some() {
            return self.run_restore(j);
        }
        loop {
            if !j.is_open() {
                return Ok(());
            }
            let Some(i) = j.current() else {
                return self.finish(&mut j, State::Succeeded, None, String::new(), false);
            };
            if j.steps[i].name == RECOVERY_ACTOR {
                match self.drive_handover(&mut j, i)? {
                    Flow::StepDone => continue,
                    Flow::Ended => return Ok(()),
                }
            }
            let outcome = match j.steps[i].phase {
                Phase::Admitted => {
                    self.advance(&mut j, i, Phase::Pulling)?;
                    continue;
                }
                Phase::Pulling => self.pull_and_check(&mut j, i).map(|()| Phase::Checked),
                // A migrating control plane's way back is taken while the old one still runs.
                Phase::Checked if j.steps[i].migrating => Ok(Phase::Dumping),
                Phase::Checked => Ok(Phase::OldKept),
                Phase::Dumping => self.take_dump(&mut j, i).map(|()| Phase::OldKept),
                Phase::OldKept => self.keep_old(&j, i).map(|()| Phase::Created),
                Phase::Created => self.create_new(&mut j, i).map(|()| Phase::Started),
                Phase::Started => self.start_new(&j, i).map(|()| Phase::Verifying),
                Phase::Verifying => self.verify(&j, i).map(|()| Phase::Verified),
                Phase::Verified => self.record_new(&j, i).map(|()| Phase::OldDiscarded),
                Phase::OldDiscarded => self.discard_old(&j, i).map(|()| Phase::Done),
                Phase::Restoring => return self.restore(&mut j, i),
                Phase::Done => unreachable!("current() skips finished steps"),
                // Only the recovery actor's own component hands over (`drive_handover`).
                Phase::CreatingSuccessor
                | Phase::StartingSuccessor
                | Phase::AwaitingSuccessor
                | Phase::HandingOver
                | Phase::SuccessorActive
                | Phase::SuccessorRenaming
                | Phase::HandingBack => {
                    let detail = format!(
                        "component {} is journalled in a hand-over phase, which only the recovery actor's own component has",
                        j.steps[i].name
                    );
                    return self.finish(
                        &mut j,
                        State::Failed,
                        Some(Reason::Invalid),
                        detail,
                        false,
                    );
                }
            };
            match outcome {
                Ok(next) => self.advance(&mut j, i, next)?,
                Err(Halt::Died) => return Err(()),
                Err(Halt::Fail(failure)) if !j.steps[i].phase.touched_old() => {
                    // Before the old container was touched: this component changed nothing.
                    let output = format!("{}{}", failure.detail, moved_before(&j, i));
                    return self.finish(&mut j, State::Failed, Some(failure.reason), output, false);
                }
                Err(Halt::Fail(failure)) if j.steps[i].migrating => {
                    return self.fail_migrating(&mut j, i, failure);
                }
                Err(Halt::Fail(failure)) => {
                    warn!(
                        token = "actor-verification-failed",
                        request = %j.request.request_id,
                        reason = %failure.reason,
                        "{}; restoring the kept container", failure.detail
                    );
                    j.steps[i].failure = Some(failure);
                    self.advance(&mut j, i, Phase::Restoring)?;
                }
            }
        }
    }

    /// Settle an attempt interrupted before the old container was touched: remove anything
    /// it created, and end it `failed`/`interrupted`.
    fn interrupt(&self, mut j: Journal) -> Result<(), ()> {
        if j.restore.is_some() {
            return self.interrupt_restore(j);
        }
        let current = j.current().unwrap_or(0);
        // A dump the interrupted attempt started, or finished, is no way back for anything.
        if j.steps.get(current).is_some_and(|s| s.migrating) {
            self.discard_dump(&j.request.request_id);
        }
        let created = match self.attempt_containers(&j, current) {
            Ok(created) => created,
            Err(EngineError::Crashed) => return Err(()),
            Err(e) => {
                warn!(
                    token = "actor-interrupt-cleanup-unseen",
                    "could not list containers to clean up after the interrupted attempt: {e}"
                );
                Vec::new()
            }
        };
        for c in created {
            match self.retrying(|| self.engine.remove_container(&c.id)) {
                Ok(()) => {}
                Err(EngineError::Crashed) => return Err(()),
                Err(e) => {
                    warn!(token = "actor-interrupt-cleanup-failed", container = %c.name, "could not remove a container the interrupted attempt created: {e}")
                }
            }
        }
        let step = &j.steps[current];
        let phase = snake(step.phase);
        let output = format!(
            "the recovery actor, the container engine or the machine restarted while {} was at `{phase}`, before the running {} was touched; it was not changed.{} It is not retried: apply again to try again.",
            step.name,
            step.name,
            moved_before(&j, current)
        );
        self.finish(
            &mut j,
            State::Failed,
            Some(Reason::Interrupted),
            output,
            false,
        )
    }

    /// The containers this attempt created for component `i`: this installation's, of
    /// the component's role, labelled with the attempt, and never a container the attempt
    /// recorded as one it replaces, nor this process's own (architecture §5.4). A label
    /// alone proves nothing: it survives on the running service an earlier attempt of the
    /// same id created, and on a successor that became the actor.
    pub(crate) fn attempt_containers(
        &self,
        j: &Journal,
        i: usize,
    ) -> Result<Vec<Container>, EngineError> {
        let Ok(Some(machine)) = self.dir.load_machine() else {
            return Ok(Vec::new());
        };
        let id = j.request.request_id.as_str();
        let role = self.role(j, i).as_str();
        let old: Vec<&String> = j
            .steps
            .iter()
            .filter_map(|s| s.old_container.as_ref())
            .collect();
        let me = self.config.self_container.as_deref();
        Ok(self
            .retrying(|| self.engine.list_containers())?
            .into_iter()
            .filter(|c| {
                c.labels.get(ATTEMPT_LABEL).map(String::as_str) == Some(id)
                    && c.labels.get(labels::INSTALLATION) == Some(&machine.installation_id)
                    && c.labels.get(labels::PLATFORM_SERVICE).map(String::as_str) == Some(role)
                    && !old.contains(&&c.id)
                    && !me.is_some_and(|me| same_container(me, &c.id))
            })
            .collect())
    }

    pub(crate) fn role(&self, j: &Journal, i: usize) -> Role {
        Role::parse(&j.steps[i].name).unwrap_or(Role::NodeAgent)
    }

    /// `pulling`: the image by digest, its recipe revision (Rule C: refused before
    /// anything stops), the rendered specification, and the old container recorded.
    pub(crate) fn pull_and_check(&self, j: &mut Journal, i: usize) -> Result<(), Halt> {
        let role = self.role(j, i);
        let image = j.steps[i].image.clone();
        let reference = image.reference();
        let present = self
            .retrying(|| self.engine.inspect_image(&reference))
            .map_err(|e| engine(e, Reason::PullFailed, &format!("inspect {reference}")))?;
        if present.is_none() {
            info!(image = %reference, "pulling");
            self.retrying(|| self.engine.pull(&reference))
                .map_err(|e| engine(e, Reason::PullFailed, &format!("pull {reference}")))?;
        }
        let found = self
            .retrying(|| self.engine.inspect_image(&reference))
            .map_err(|e| engine(e, Reason::PullFailed, &format!("inspect {reference}")))?
            .ok_or_else(|| {
                fail(
                    Reason::PullFailed,
                    format!("{reference} is not present after the pull"),
                )
            })?;
        let label = found.labels.get(labels::IMAGE_RECIPE).ok_or_else(|| {
            fail(
                Reason::RecipeUnsupported,
                format!(
                    "{reference} carries no {} label, so this recovery actor cannot tell how to create it; the running service was not touched",
                    labels::IMAGE_RECIPE
                ),
            )
        })?;
        let revision: u32 = label.trim().parse().map_err(|_| {
            fail(
                Reason::RecipeUnsupported,
                format!(
                    "{reference} declares {}={label:?}, which is not a revision",
                    labels::IMAGE_RECIPE
                ),
            )
        })?;
        if !Book::supports(role, revision) {
            return Err(fail(
                Reason::RecipeUnsupported,
                format!(
                    "{reference} needs {} recipe revision {revision}, which this recovery actor does not carry (it renders {:?}); apply the recovery actor that does first. The running service was not touched",
                    role.as_str(),
                    Book::window(role)
                ),
            ));
        }
        let machine = match self.dir.load_machine() {
            Ok(Some(m)) => m,
            Ok(None) => return Err(fail(Reason::RecreateFailed, "machine state is missing")),
            Err(e) => return Err(fail(Reason::RecreateFailed, format!("machine state: {e}"))),
        };
        let secrets = match role {
            Role::NodeAgent => self.node_agent_secrets(),
            Role::ControlPlane => self.control_plane_secrets(),
            _ => Ok(Default::default()),
        }
        .map_err(|e| fail(Reason::RecreateFailed, format!("machine state: {e}")))?;
        let mut spec = recipe::render(role, revision, &machine.inputs, &image, &secrets).map_err(
            |e| match e {
                RenderError::Unsupported { .. } => fail(Reason::RecipeUnsupported, e.to_string()),
                RenderError::Invalid(_) => fail(Reason::RecreateFailed, e.to_string()),
            },
        )?;
        let old = self
            .retrying(|| self.engine.inspect_container(role.container_name()))
            .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the running container"))?;
        let itself = |c: &Container| {
            role == Role::RecoveryActor
                && self
                    .config
                    .self_container
                    .as_deref()
                    .is_some_and(|me| same_container(me, &c.id))
        };
        if let Some(old) = &old {
            if !itself(old) && !self.is_ours(&machine, old, role) {
                return Err(fail(
                    Reason::OwnerConflict,
                    format!(
                        "container {} ({}) is not this installation's; it is never acted on, and the running service was not touched",
                        old.name, old.image
                    ),
                ));
            }
        }
        if role == Role::RecoveryActor {
            let me = self.config.self_container.as_deref();
            let Some(old) = old
                .as_ref()
                .filter(|c| me.is_some_and(|me| same_container(me, &c.id)))
            else {
                return Err(fail(
                    Reason::RecreateFailed,
                    format!(
                        "the container named {} is not the recovery actor running this attempt, so it cannot hand over to a successor; the running actor was not touched",
                        role.container_name()
                    ),
                ));
            };
            let trust_recorded = matches!(
                self.dir.load_machine(),
                Ok(Some(m)) if !m.inputs.trust.is_empty()
            );
            crate::handover::carry_forward(&mut spec, old, trust_recorded);
        }
        let migrating = if role == Role::ControlPlane {
            self.control_plane_migrates(j, &machine, &found, &reference)?
        } else {
            false
        };
        let step = &mut j.steps[i];
        step.migrating = migrating;
        step.revision = Some(revision);
        step.spec = Some(spec);
        step.old_container = old.as_ref().map(|c| c.id.clone());
        step.old_restart = old.as_ref().and_then(|c| c.restart);
        Ok(())
    }

    /// `old_kept`: stop, disable the restart policy, rename `.kept`. Idempotent.
    pub(crate) fn keep_old(&self, j: &Journal, i: usize) -> Result<(), Halt> {
        let Some(old_id) = j.steps[i].old_container.clone() else {
            return Ok(()); // nothing ran before: nothing to keep
        };
        let kept = kept_name(self.role(j, i).container_name());
        let Some(old) = self
            .retrying(|| self.engine.inspect_container(&old_id))
            .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the old container"))?
        else {
            return Ok(());
        };
        if old.running {
            let grace = self.config.timing.stop_grace;
            self.retrying(|| self.engine.stop_container(&old_id, grace))
                .map_err(|e| engine(e, Reason::RecreateFailed, "stop the old container"))?;
        }
        if old.restart != Some(RestartPolicy::No) {
            self.retrying(|| self.engine.set_restart_policy(&old_id, RestartPolicy::No))
                .map_err(|e| {
                    engine(
                        e,
                        Reason::RecreateFailed,
                        "disable the old container's restart",
                    )
                })?;
        }
        if old.name != kept {
            if let Some(stale) = self
                .retrying(|| self.engine.inspect_container(&kept))
                .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the kept name"))?
            {
                // A kept container may be the only way back for an attempt whose
                // journal cannot be read: never remove one while any is unreadable.
                let unreadable = self.journals.scan().unreadable;
                if !unreadable.is_empty() {
                    return Err(fail(
                        Reason::RecreateFailed,
                        format!(
                            "container {} is kept by an earlier attempt and attempt journal(s) {unreadable:?} cannot be read; it is not removed",
                            stale.name
                        ),
                    ));
                }
                if stale.labels.get(labels::INSTALLATION) != old.labels.get(labels::INSTALLATION) {
                    return Err(fail(
                        Reason::OwnerConflict,
                        format!(
                            "container {} ({}) is not this installation's; it is never acted on",
                            stale.name, stale.image
                        ),
                    ));
                }
                // A container kept by an earlier attempt whose restore did not finish.
                self.retrying(|| self.engine.remove_container(&stale.id))
                    .map_err(|e| {
                        engine(e, Reason::RecreateFailed, "remove a stale kept container")
                    })?;
            }
            self.retrying(|| self.engine.rename_container(&old_id, &kept))
                .map_err(|e| engine(e, Reason::RecreateFailed, "rename the old container"))?;
        }
        Ok(())
    }

    /// `created`: the new container under the service's name, labelled with the attempt.
    /// Idempotent: a container this attempt already created is reused.
    fn create_new(&self, j: &mut Journal, i: usize) -> Result<(), Halt> {
        let id = j.request.request_id.clone();
        let mine = self
            .attempt_containers(j, i)
            .map_err(|e| engine(e, Reason::RecreateFailed, "list containers"))?;
        if let Some(c) = mine.into_iter().next() {
            j.steps[i].new_container = Some(c.id);
            return Ok(());
        }
        let name = self.role(j, i).container_name();
        if let Some(c) = self
            .retrying(|| self.engine.inspect_container(name))
            .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the service name"))?
        {
            if Some(&c.id) != j.steps[i].old_container.as_ref() {
                return Err(fail(
                    Reason::OwnerConflict,
                    format!("container {} ({}) holds the service's name and was not created by this attempt; it is never acted on", c.name, c.image),
                ));
            }
        }
        let mut spec = j.steps[i].spec.clone().ok_or_else(|| {
            fail(
                Reason::RecreateFailed,
                "no rendered specification was journalled",
            )
        })?;
        spec.labels.insert(ATTEMPT_LABEL.into(), id);
        let new = self
            .retrying(|| self.engine.create_container(&spec))
            .map_err(|e| engine(e, Reason::RecreateFailed, "create the new container"))?;
        info!(container = %name, image = %spec.image, "new container created");
        j.steps[i].new_container = Some(new);
        Ok(())
    }

    fn new_container(&self, j: &Journal, i: usize) -> Result<Option<Container>, Halt> {
        if let Some(new) = &j.steps[i].new_container {
            if let Some(c) = self
                .retrying(|| self.engine.inspect_container(new))
                .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the new container"))?
            {
                return Ok(Some(c));
            }
        }
        Ok(self
            .attempt_containers(j, i)
            .map_err(|e| engine(e, Reason::RecreateFailed, "list containers"))?
            .into_iter()
            .next())
    }

    /// `started`. A start the engine refuses leaves a container that never started.
    fn start_new(&self, j: &Journal, i: usize) -> Result<(), Halt> {
        let c = self
            .new_container(j, i)?
            .ok_or_else(|| fail(Reason::RecreateFailed, "the new container is gone"))?;
        if c.running {
            return Ok(());
        }
        // From this start on the database may be at the new schema: nothing may bring an
        // older control plane back against it, whatever happens to this attempt.
        if j.steps[i].migrating {
            let target = self.migration_target(j, i).map_err(|e| match e {
                EngineError::Crashed => Halt::Died,
                e => fail(Reason::RecreateFailed, format!("read the new image: {e}")),
            })?;
            if let Some(schema) = target {
                self.raise_floor(schema, &j.request.request_id)
                    .map_err(|e| {
                        error!(token = "actor-schema-floor-unwritten", "{e}");
                        Halt::Died
                    })?;
            }
        }
        match self.retrying(|| self.engine.start_container(&c.id)) {
            Ok(()) => Ok(()),
            Err(EngineError::Crashed) => Err(Halt::Died),
            Err(e) => Err(fail(
                Reason::NeverStarted,
                format!("the engine did not start the new container, so it never ran: {e}"),
            )),
        }
    }

    /// `verifying`: running and healthy (or running, twice, with no healthcheck) within
    /// the wait timeout. A container the engine reports `unhealthy` fails at once. A
    /// control plane must pass its own healthcheck (`/health`, which reaches the database):
    /// one with none never passed a health check (ADR 0004 amendment), so it is restored.
    fn verify(&self, j: &Journal, i: usize) -> Result<(), Halt> {
        let needs_health = self.role(j, i) == Role::ControlPlane;
        let timing = self.config.timing;
        let timeout = match j.request.wait_timeout_s {
            n if n > 0 => Duration::from_secs(n.min(3600) as u64),
            _ => timing.verify_timeout,
        };
        let deadline = Instant::now() + timeout;
        let mut ran = false;
        let mut steady = 0;
        let mut last: String;
        loop {
            let seen = match &j.steps[i].new_container {
                Some(id) => self.engine.inspect_container(id),
                None => Ok(None),
            };
            match seen {
                Ok(Some(c)) => {
                    ran |= c.running;
                    last = format!(
                        "state={} health={}",
                        c.status,
                        c.health.as_deref().unwrap_or("none")
                    );
                    match (c.running, c.health.as_deref()) {
                        (true, Some("healthy")) => return Ok(()),
                        (true, None) if !needs_health => {
                            steady += 1;
                            if steady >= 2 {
                                return Ok(());
                            }
                        }
                        // A migration may hold a control plane unhealthy until it is done, and a
                        // migrating one is never restored: it gets the whole wait.
                        (_, Some("unhealthy")) if !j.steps[i].migrating => {
                            return Err(fail(
                                Reason::Unhealthy,
                                format!("the new container reported unhealthy ({last})"),
                            ))
                        }
                        _ => steady = 0,
                    }
                    if !ran && c.status == "created" && Instant::now() >= deadline {
                        return Err(fail(
                            Reason::NeverStarted,
                            format!("the new container never started ({last})"),
                        ));
                    }
                }
                Ok(None) => {
                    return Err(fail(
                        Reason::RecreateFailed,
                        "the new container disappeared during verification",
                    ))
                }
                Err(EngineError::Crashed) => return Err(Halt::Died),
                // The engine may be restarting: keep looking until the deadline.
                Err(e) => last = format!("the engine did not answer: {e}"),
            }
            if Instant::now() >= deadline {
                return Err(fail(
                    Reason::Unhealthy,
                    format!(
                        "the new container was not running and healthy within {}s ({last})",
                        timeout.as_secs()
                    ),
                ));
            }
            std::thread::sleep(timing.poll);
        }
    }

    /// `verified`: the new specification becomes this machine's record of the service.
    pub(crate) fn record_new(&self, j: &Journal, i: usize) -> Result<(), Halt> {
        let step = &j.steps[i];
        let (Some(revision), Some(spec)) = (step.revision, step.spec.clone()) else {
            return Err(fail(
                Reason::RecreateFailed,
                "no rendered specification was journalled",
            ));
        };
        self.record(self.role(j, i), revision, &step.image, spec)
            .map_err(|e| {
                error!(token = "actor-service-record-failed", "{e}");
                Halt::Died
            })
    }

    /// `old_discarded`: the kept container goes.
    pub(crate) fn discard_old(&self, j: &Journal, i: usize) -> Result<(), Halt> {
        if let Some(old) = j.steps[i].old_container.clone() {
            match self.retrying(|| self.engine.remove_container(&old)) {
                Ok(()) => {}
                Err(EngineError::Crashed) => return Err(Halt::Died),
                // Verified is verified: a kept container that cannot be removed now is
                // swept by the next attempt's `old_kept`.
                Err(e) => warn!(
                    token = "actor-kept-not-removed",
                    "the kept old container could not be removed: {e}"
                ),
            }
        }
        Ok(())
    }

    /// `restoring`: remove the new container, bring the kept one back under the service's
    /// name with its restart policy, start it, see it running.
    fn restore(&self, j: &mut Journal, i: usize) -> Result<(), ()> {
        let failure = j.steps[i].failure.clone().unwrap_or(Failure {
            reason: Reason::Unhealthy,
            detail: String::new(),
        });
        if j.steps[i].migrating {
            return self.fail_migrating(j, i, failure);
        }
        let mut output = failure.detail.clone();
        let canonical = self.role(j, i).container_name();
        let mut restored = false;

        let new = match self.new_container(j, i) {
            Ok(c) => c,
            Err(Halt::Died) => return Err(()),
            Err(Halt::Fail(f)) => {
                output.push_str(&format!("\n{}", f.detail));
                None
            }
        };
        if let Some(new) = new {
            match self.engine.logs_tail(&new.id, 40) {
                Ok(tail) if !tail.trim_end().is_empty() => {
                    output.push_str("\n--- last lines of the failed container ---\n");
                    output.push_str(&embedded_log_tail(&tail));
                }
                Err(EngineError::Crashed) => return Err(()),
                _ => {}
            }
            match self.retrying(|| self.engine.remove_container(&new.id)) {
                Ok(()) => {}
                Err(EngineError::Crashed) => return Err(()),
                Err(e) => {
                    output.push_str(&format!("\nthe failed container could not be removed: {e}"))
                }
            }
        }

        let back = match j.steps[i].old_container.clone() {
            None => Err("there was no previous container to put back".to_string()),
            Some(old_id) => self.bring_back(&old_id, canonical, j.steps[i].old_restart),
        };
        match back {
            Ok(()) => {
                restored = true;
                output.push_str("\nthe new container did not verify; the previous container was put back and is running");
            }
            Err(why) if why == CRASHED => return Err(()),
            Err(why) => output.push_str(&format!(
                "\nthe new container did not verify and the automatic restore ALSO failed ({why}); apply the digests in `previous` by hand"
            )),
        }
        output.push_str(&moved_before(j, i));
        self.finish(j, State::Failed, Some(failure.reason), output, restored)
    }

    pub(crate) fn bring_back(
        &self,
        old_id: &str,
        canonical: &str,
        policy: Option<RestartPolicy>,
    ) -> Result<(), String> {
        let crashed = |e: EngineError| match e {
            EngineError::Crashed => CRASHED.to_string(),
            e => e.to_string(),
        };
        let old = self
            .retrying(|| self.engine.inspect_container(old_id))
            .map_err(crashed)?
            .ok_or_else(|| "the previous container is gone".to_string())?;
        if canonical == Role::ControlPlane.container_name() {
            let image = self
                .retrying(|| self.engine.inspect_image(&old.image_id))
                .map_err(crashed)?
                .ok_or_else(|| "the previous control plane's image is gone".to_string())?;
            self.schema_allows(&image, &old.image)?;
        }
        if old.name != canonical {
            if let Some(other) = self
                .retrying(|| self.engine.inspect_container(canonical))
                .map_err(crashed)?
            {
                return Err(format!("container {} holds the service's name", other.name));
            }
            self.retrying(|| self.engine.rename_container(old_id, canonical))
                .map_err(crashed)?;
        }
        let policy = policy.unwrap_or(RestartPolicy::UnlessStopped);
        if old.restart != Some(policy) {
            self.retrying(|| self.engine.set_restart_policy(old_id, policy))
                .map_err(crashed)?;
        }
        if !old.running {
            self.retrying(|| self.engine.start_container(old_id))
                .map_err(crashed)?;
        }
        let deadline = Instant::now() + self.config.timing.verify_timeout;
        loop {
            match self.engine.inspect_container(old_id) {
                Ok(Some(c)) if c.running => return Ok(()),
                Err(EngineError::Crashed) => return Err(CRASHED.into()),
                _ => {}
            }
            if Instant::now() >= deadline {
                return Err("the previous container did not come back up".into());
            }
            std::thread::sleep(self.config.timing.poll);
        }
    }
}

pub(crate) const CRASHED: &str = "\u{0}crashed";

/// What components before `i` already changed, for an outcome that would otherwise read
/// as "nothing changed": a multi-component attempt keeps what it verified (ADR 0004
/// amendment, "one service per failure"). Empty when nothing did.
pub(crate) fn moved_before(j: &Journal, i: usize) -> String {
    let moved: Vec<String> = j.steps[..i]
        .iter()
        .filter(|s| s.phase == Phase::Done)
        .map(|s| format!("{} {}", s.name, s.image.reference()))
        .collect();
    if moved.is_empty() {
        return String::new();
    }
    format!(
        " Earlier in this attempt {} {} replaced and verified, and {} on the new image.",
        moved.join(" and "),
        if moved.len() == 1 { "was" } else { "were" },
        if moved.len() == 1 { "stays" } else { "stay" },
    )
}

/// A phase as the journal spells it.
pub(crate) fn snake(phase: Phase) -> String {
    serde_json::to_value(phase)
        .ok()
        .and_then(|v| v.as_str().map(str::to_owned))
        .unwrap_or_default()
}

/// Two references to one container: equal, or one a unique prefix of the other (a
/// container knows itself by `$HOSTNAME`, the id's first 12 characters, when its mounts
/// do not tell).
pub(crate) fn same_container(a: &str, b: &str) -> bool {
    a == b || (a.len().min(b.len()) >= 12 && (a.starts_with(b) || b.starts_with(a)))
}

fn stopped() -> ResumeError {
    ResumeError::State(std::io::Error::other(
        "the open attempt stopped before it reached an outcome; the next start settles it",
    ))
}
