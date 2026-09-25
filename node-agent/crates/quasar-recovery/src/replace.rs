//! One Replacement (architecture §5.5): the node agent's, driven from the journal.
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
//! verification is always restored). Nothing is retried as a new attempt: an engine call
//! that fails transiently is asked again inside its step, and that is all.

use std::time::{Duration, Instant};

use tracing::{error, info, warn};

use crate::actor::{Actor, ResumeError};
use crate::engine::{Container, EngineError, RestartPolicy};
use crate::journal::{tail_output, Failure, Journal, Phase, LOG_TAIL_LIMIT, OUTPUT_LIMIT};
use crate::recipe::{self, labels, Book, RenderError, Role};
use crate::settle::{settle, Settlement};
use crate::socket::{Reason, State};
use crate::submit::kept_name;

/// On a replacement's own containers: the request that created (`.next`/new) it.
pub const ATTEMPT_LABEL: &str = "io.quasar.attempt";

/// How a step stopped short.
enum Halt {
    /// The process "died" here (`EngineError::Crashed`, injected by tests), or the journal
    /// could not be written: stop driving and leave the journal as it is.
    Died,
    Fail(Failure),
}

fn fail(reason: Reason, detail: impl Into<String>) -> Halt {
    Halt::Fail(Failure {
        reason,
        detail: detail.into(),
    })
}

/// An engine error inside a step: a crash stops everything; anything else fails the step.
fn engine(e: EngineError, reason: Reason, what: &str) -> Halt {
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
                error!(token = "actor-journal-unreadable", request = %request_id, "{e}");
                return;
            }
        };
        if self.run(journal).is_err() {
            warn!(
                token = "actor-attempt-stopped",
                request = %request_id,
                "this attempt stopped being driven; the next start settles it"
            );
        }
    }

    /// `resume`'s D8 step: the open attempt, if any, reaches a terminal outcome.
    pub(crate) fn settle_open(&self) -> Result<(), ResumeError> {
        let Some(journal) = self.journals.open() else {
            return Ok(());
        };
        let id = journal.request.request_id.clone();
        match settle(&journal) {
            Settlement::Terminal => Ok(()),
            Settlement::Interrupted => {
                info!(request = %id, "settling an attempt interrupted before anything changed");
                self.interrupt(journal).map_err(|()| stopped())
            }
            Settlement::Continue => {
                info!(request = %id, "continuing an attempt a restart interrupted after the old container was taken out of service");
                self.run(journal).map_err(|()| stopped())
            }
        }
    }

    fn now(&self) -> String {
        (self.config.now)()
    }

    /// Commit the journal. A journal that cannot be written stops the attempt: acting on
    /// a phase that is not on disk is the one thing D8 forbids.
    fn commit(&self, j: &mut Journal) -> Result<(), ()> {
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

    fn advance(&self, j: &mut Journal, i: usize, phase: Phase) -> Result<(), ()> {
        j.steps[i].phase = phase;
        self.commit(j)
    }

    fn finish(
        &self,
        j: &mut Journal,
        state: State,
        reason: Option<Reason>,
        output: String,
        restored: bool,
    ) -> Result<(), ()> {
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

    fn retrying<T>(
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
        loop {
            if !j.is_open() {
                return Ok(());
            }
            let Some(i) = j.current() else {
                return self.finish(&mut j, State::Succeeded, None, String::new(), false);
            };
            let outcome = match j.steps[i].phase {
                Phase::Admitted => {
                    self.advance(&mut j, i, Phase::Pulling)?;
                    continue;
                }
                Phase::Pulling => self.pull_and_check(&mut j, i).map(|()| Phase::Checked),
                Phase::Checked => Ok(Phase::OldKept),
                Phase::OldKept => self.keep_old(&j, i).map(|()| Phase::Created),
                Phase::Created => self.create_new(&mut j, i).map(|()| Phase::Started),
                Phase::Started => self.start_new(&j, i).map(|()| Phase::Verifying),
                Phase::Verifying => self.verify(&j, i).map(|()| Phase::Verified),
                Phase::Verified => self.record_new(&j, i).map(|()| Phase::OldDiscarded),
                Phase::OldDiscarded => self.discard_old(&j, i).map(|()| Phase::Done),
                Phase::Restoring => return self.restore(&mut j, i),
                Phase::Done => unreachable!("current() skips finished steps"),
            };
            match outcome {
                Ok(next) => self.advance(&mut j, i, next)?,
                Err(Halt::Died) => return Err(()),
                Err(Halt::Fail(failure)) if !j.steps[i].phase.touched_old() => {
                    // Before the old container was touched: nothing changed.
                    return self.finish(
                        &mut j,
                        State::Failed,
                        Some(failure.reason),
                        failure.detail,
                        false,
                    );
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
        let id = j.request.request_id.clone();
        let created = match self.attempt_containers(&id) {
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
        let phase = j
            .current()
            .map(|i| format!("{:?}", j.steps[i].phase).to_lowercase())
            .unwrap_or_default();
        self.finish(
            &mut j,
            State::Failed,
            Some(Reason::Interrupted),
            format!(
                "the recovery actor, the container engine or the machine restarted while this attempt was at `{phase}`, before the running service was touched; nothing was changed. It is not retried: apply again to try again."
            ),
            false,
        )
    }

    fn attempt_containers(&self, request_id: &str) -> Result<Vec<Container>, EngineError> {
        Ok(self
            .retrying(|| self.engine.list_containers())?
            .into_iter()
            .filter(|c| c.labels.get(ATTEMPT_LABEL).map(String::as_str) == Some(request_id))
            .collect())
    }

    fn role(&self, j: &Journal, i: usize) -> Role {
        Role::parse(&j.steps[i].name).unwrap_or(Role::NodeAgent)
    }

    /// `pulling`: the image by digest, its recipe revision (Rule C: refused before
    /// anything stops), the rendered specification, and the old container recorded.
    fn pull_and_check(&self, j: &mut Journal, i: usize) -> Result<(), Halt> {
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
                    "{reference} carries no {} label, so this recovery actor cannot tell how to create it; nothing was changed",
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
                    "{reference} needs {} recipe revision {revision}, which this recovery actor does not carry (it renders {:?}); apply the recovery actor that does first. Nothing was changed",
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
        let secrets = self
            .node_agent_secrets()
            .map_err(|e| fail(Reason::RecreateFailed, format!("machine state: {e}")))?;
        let spec = recipe::render(role, revision, &machine.inputs, &image, &secrets).map_err(
            |e| match e {
                RenderError::Unsupported { .. } => fail(Reason::RecipeUnsupported, e.to_string()),
                RenderError::Invalid(_) => fail(Reason::RecreateFailed, e.to_string()),
            },
        )?;
        let old = self
            .retrying(|| self.engine.inspect_container(role.container_name()))
            .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the running container"))?;
        if let Some(old) = &old {
            if !self.is_ours(&machine, old, role) {
                return Err(fail(
                    Reason::OwnerConflict,
                    format!(
                        "container {} ({}) is not this installation's; it is never acted on. Nothing was changed",
                        old.name, old.image
                    ),
                ));
            }
        }
        let step = &mut j.steps[i];
        step.revision = Some(revision);
        step.spec = Some(spec);
        step.old_container = old.as_ref().map(|c| c.id.clone());
        step.old_restart = old.as_ref().and_then(|c| c.restart);
        Ok(())
    }

    /// `old_kept`: stop, disable the restart policy, rename `.kept`. Idempotent.
    fn keep_old(&self, j: &Journal, i: usize) -> Result<(), Halt> {
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
            .attempt_containers(&id)
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
        let id = j.request.request_id.clone();
        if let Some(new) = &j.steps[i].new_container {
            if let Some(c) = self
                .retrying(|| self.engine.inspect_container(new))
                .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the new container"))?
            {
                return Ok(Some(c));
            }
        }
        Ok(self
            .attempt_containers(&id)
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
    /// the wait timeout. A container the engine reports `unhealthy` fails at once.
    fn verify(&self, j: &Journal, i: usize) -> Result<(), Halt> {
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
                        (true, None) => {
                            steady += 1;
                            if steady >= 2 {
                                return Ok(());
                            }
                        }
                        (_, Some("unhealthy")) => {
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
    fn record_new(&self, j: &Journal, i: usize) -> Result<(), Halt> {
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
    fn discard_old(&self, j: &Journal, i: usize) -> Result<(), Halt> {
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
                    output.push_str(&tail_output(tail.trim_end(), LOG_TAIL_LIMIT));
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
        self.finish(j, State::Failed, Some(failure.reason), output, restored)
    }

    fn bring_back(
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

const CRASHED: &str = "\u{0}crashed";

fn stopped() -> ResumeError {
    ResumeError::State(std::io::Error::other(
        "the open attempt stopped before it reached an outcome; the next start settles it",
    ))
}
