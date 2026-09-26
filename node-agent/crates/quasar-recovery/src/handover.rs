//! The recovery actor replacing itself: the hand-over (architecture §5.6, ADR 0004
//! amendment, ADR 0008). Two processes and one lease, the machine's `actor.lease`: only
//! the process holding it acts on the engine or writes the journal.
//!
//! ```text
//! old actor (holds the lease)                 successor (quasar-recovery.next)
//! admitted → pulling → checked
//! creating_successor   create .next
//! starting_successor   start it  ────────────▶ boots, cannot take the lease, self-checks,
//! awaiting_successor   wait for the marker ◀── writes handover/<request-id>.ready
//! handing_over         stop serving, release ─▶ takes the lease
//!                      the lease, watch        successor_active   stop the old actor, disable
//!                                                                 it, rename it .kept
//!                                              successor_renaming take the actor's name
//!                                              verifying          answer on its own socket;
//!                                                                 on a GPU host, reached by the agent
//!                                              verified           record its specification
//!                                              old_discarded      remove .kept, then seed.json
//! ```
//!
//! Every crash point settles to a stated outcome (`crate::settle`, the hand-over table),
//! and at every point at least one container carries the recovery actor's two seed labels,
//! so the seed never re-creates an actor beside a hand-over in progress (ADR 0007).
//! `seed.json` names the successor only after it verified. A successor that never
//! verifies is removed and the previous actor keeps (or goes back to) running: failed,
//! restored. The one case needing a person, a successor that cannot start at all once the
//! old actor was stopped, is [`FIX`]; removing every actor container instead lets the seed
//! re-create the last verified one.

use std::path::{Path, PathBuf};
use std::time::Instant;

use quasar_runtime::DurableFile;
use serde::{Deserialize, Serialize};
use tracing::{info, warn};

use crate::actor::Actor;
use crate::engine::{Container, ContainerSpec, EngineError, RestartPolicy};
use crate::journal::{tail_output, Failure, Journal, Phase, LOG_TAIL_LIMIT};
use crate::recipe::{self, labels, names, ImageRef, Role};
use crate::replace::{engine, fail, moved_before, same_container, Halt, ATTEMPT_LABEL, CRASHED};
use crate::settle::{Party, RECOVERY_ACTOR};
use crate::socket::{Reason, State};

/// The one-line way back when no recovery actor runs after a hand-over: the previous actor
/// is kept, stopped, under this name until its successor verified, and started again it
/// takes the lease and restores itself.
pub const FIX: &str = "docker start quasar-recovery.kept";

/// The successor's name until it takes the actor's.
pub fn successor_name() -> String {
    format!("{}.next", names::RECOVERY_ACTOR)
}

/// The running actor's own configuration that its successor keeps: its release trust
/// knobs (`docs/configuration.md` "Recovery actor"), which a successor rendered from the
/// recipe alone would silently reset, the seed that created it, and its log level.
const CARRIED_ENV: &[&str] = &[
    "QUASAR_UPDATER_ALLOWED_NAMESPACES",
    "QUASAR_UPDATER_SIGNATURE_MODE",
    "QUASAR_UPDATER_TRUSTED_KEYS",
    "QUASAR_UPDATER_MANIFEST_BASE_URL",
    "QUASAR_UPDATER_MANIFEST_TIMEOUT_S",
    crate::seed::profile::SEED_CONTAINER_ENV,
    "RUST_LOG",
];

/// The successor's specification: the recipe's, plus [`CARRIED_ENV`] from the running
/// actor's container, and its `io.quasar.spec` recomputed over the result.
pub(crate) fn carry_forward(spec: &mut ContainerSpec, running: &Container) {
    for kv in &running.env {
        if let Some((k, v)) = kv.split_once('=') {
            if CARRIED_ENV.contains(&k) {
                spec.env.insert(k.to_owned(), v.to_owned());
            }
        }
    }
    let digest = recipe::spec_digest(spec);
    spec.labels.insert(labels::SPEC.into(), digest);
}

/// How driving the recovery actor's component ended for this process.
pub(crate) enum Flow {
    /// The component is done; the attempt goes on to the next one.
    StepDone,
    /// The attempt is terminal, or this process left it to another actor.
    Ended,
}

/// The successor's ready marker: written once it self-checked, while it waits for the
/// lease. The only file a process writes without holding the lease, and only for its own
/// attempt.
#[derive(Debug, Serialize, Deserialize)]
struct Ready {
    container: String,
    version: String,
    commit: String,
}

fn ready_path(root: &Path, request_id: &str) -> PathBuf {
    root.join("handover").join(format!("{request_id}.ready"))
}

fn ready_file(root: &Path, request_id: &str) -> DurableFile<Ready> {
    DurableFile::new(ready_path(root, request_id), "tmp")
}

/// Drops an attempt's ready marker once the attempt is terminal.
pub(crate) fn forget_ready(root: &Path, request_id: &str) {
    let _ = std::fs::remove_file(ready_path(root, request_id));
}

fn to_restore(failure: Failure) -> Result<Phase, Halt> {
    Err(Halt::Fail(failure))
}

impl Actor {
    fn me(&self) -> Option<&str> {
        self.config.self_container.as_deref()
    }

    fn is_me(&self, id: &str) -> bool {
        self.me().is_some_and(|me| same_container(me, id))
    }

    /// Which party this process is to the attempt's recovery-actor component (a journal
    /// without one never asks for more than the node agent's table).
    pub(crate) fn party(&self, j: &Journal) -> Party {
        let stranger = Party::Stranger {
            on_new_image: false,
        };
        let Some(step) = j.steps.iter().find(|s| s.name == RECOVERY_ACTOR) else {
            return stranger;
        };
        let Some(me) = self.me() else {
            return stranger;
        };
        if step
            .old_container
            .as_deref()
            .is_some_and(|o| same_container(me, o))
        {
            return Party::Old;
        }
        if step
            .new_container
            .as_deref()
            .is_some_and(|n| same_container(me, n))
        {
            return Party::Successor;
        }
        match self.engine.inspect_container(me) {
            Ok(Some(c)) => {
                let this_attempt = c.labels.get(ATTEMPT_LABEL) == Some(&j.request.request_id)
                    && c.labels.get(labels::PLATFORM_SERVICE).map(String::as_str)
                        == Some(Role::RecoveryActor.as_str());
                if this_attempt {
                    return Party::Successor;
                }
                Party::Stranger {
                    on_new_image: c.image.split_once('@').map(|(_, d)| d)
                        == Some(step.image.digest.as_str()),
                }
            }
            _ => stranger,
        }
    }

    /// This process's own attempt label, read once while it waits for the lease: the
    /// successor of an attempt whose journal does not record it yet.
    pub(crate) fn own_attempt_label(&self) -> Option<String> {
        let me = self.me()?;
        let c = self.engine.inspect_container(me).ok()??;
        c.labels.get(ATTEMPT_LABEL).cloned()
    }

    /// The open attempt whose recovery-actor component is current and names this process
    /// its successor.
    fn waiting_successor(&self, own_attempt: Option<&str>) -> Option<(Journal, usize)> {
        let j = self.journals.scan().open()?.clone();
        let i = j.current()?;
        let step = &j.steps[i];
        if step.name != RECOVERY_ACTOR {
            return None;
        }
        let mine = match step.new_container.as_deref() {
            Some(new) => self.is_me(new),
            None => own_attempt == Some(j.request.request_id.as_str()),
        };
        mine.then_some((j, i))
    }

    /// Whether this waiting process may take the lease when it is free. A successor may
    /// not before the old actor handed it over, unless the old actor's container is gone:
    /// an old actor that crashed comes back and settles the attempt itself.
    pub(crate) fn may_take_lease(
        &self,
        own_attempt: Option<&str>,
        last_check: &mut Option<Instant>,
    ) -> bool {
        let Some((j, i)) = self.waiting_successor(own_attempt) else {
            return true;
        };
        let step = &j.steps[i];
        if step.phase.touched_old() {
            return true;
        }
        if last_check.is_some_and(|t| t.elapsed() < self.config.handover.orphan_check) {
            return false;
        }
        *last_check = Some(Instant::now());
        let Some(old) = &step.old_container else {
            return false;
        };
        matches!(self.engine.inspect_container(old), Ok(None))
    }

    /// A waiting successor's part: self-check, then say it is ready.
    pub(crate) fn while_waiting(&self, own_attempt: Option<&str>) {
        let Some((j, i)) = self.waiting_successor(own_attempt) else {
            return;
        };
        if j.steps[i].phase.touched_old() {
            return;
        }
        let id = &j.request.request_id;
        if ready_path(self.dir.root(), id).exists() {
            return;
        }
        match self.self_check() {
            Ok(()) => {}
            Err(why) => {
                warn!(token = "actor-successor-self-check-failed", request = %id, "{why}; not ready yet");
                return;
            }
        }
        let marker = Ready {
            container: self.me().unwrap_or_default().to_owned(),
            version: crate::identity::version().into(),
            commit: crate::identity::source_commit().into(),
        };
        let written = std::fs::create_dir_all(self.dir.root().join("handover"))
            .and_then(|()| ready_file(self.dir.root(), id).store(&marker));
        match written {
            Ok(()) => info!(
                token = "actor-successor-ready",
                request = %id,
                "this successor self-checked and is ready to take the machine over"
            ),
            Err(e) => {
                warn!(token = "actor-successor-ready-unwritten", request = %id, "the ready marker could not be written: {e}")
            }
        }
    }

    /// What a successor proves before it asks for the machine: it reaches the engine and
    /// its own container, and reads machine state and the journal.
    fn self_check(&self) -> Result<(), String> {
        let me = self
            .me()
            .ok_or("this process cannot tell its own container")?;
        match self.engine.inspect_container(me) {
            Ok(Some(c)) if c.running => {}
            Ok(_) => return Err("its own container is not running".into()),
            Err(e) => return Err(format!("the container engine did not answer: {e}")),
        }
        match self.dir.load_machine() {
            Ok(Some(_)) => {}
            Ok(None) => return Err("machine state is missing".into()),
            Err(e) => return Err(format!("machine state is unreadable: {e}")),
        }
        if !self.journals.scan().unreadable.is_empty() {
            return Err("an attempt journal is unreadable".into());
        }
        Ok(())
    }

    /// A successor that takes the lease once the old actor is stopped counts its start:
    /// one that keeps crashing gives the machine back (`MAX_SUCCESSOR_STARTS`).
    pub(crate) fn count_successor_start(&self, j: &mut Journal, party: Party) -> Result<(), ()> {
        if party != Party::Successor || !j.is_open() {
            return Ok(());
        }
        let Some(i) = j.current() else {
            return Ok(());
        };
        let step = &mut j.steps[i];
        if step.name != RECOVERY_ACTOR
            || !step.phase.touched_old()
            || matches!(step.phase, Phase::Restoring | Phase::HandingBack)
        {
            return Ok(());
        }
        step.successor_starts += 1;
        if step.new_container.is_none() {
            step.new_container = self.config.self_container.clone();
        }
        self.commit(j)
    }

    /// Settlement `Restore`: the recovery-actor component goes to `restoring`, with why.
    pub(crate) fn begin_actor_restore(&self, j: &mut Journal, party: Party) -> Result<(), ()> {
        let Some(i) = j.current() else {
            return Ok(());
        };
        let step = &mut j.steps[i];
        if step.name != RECOVERY_ACTOR
            || matches!(step.phase, Phase::Restoring | Phase::HandingBack)
        {
            return Ok(());
        }
        if step.failure.is_none() {
            let takeover = self.config.handover.takeover;
            step.failure = Some(match (party, step.phase) {
                (Party::Old, Phase::HandingOver) => Failure {
                    reason: Reason::Unhealthy,
                    detail: format!(
                        "the successor did not take the machine's lease within {takeover:?} of the hand-over"
                    ),
                },
                (Party::Old, _) => Failure {
                    reason: Reason::Unhealthy,
                    detail: "the successor stopped before it verified itself".into(),
                },
                (Party::Successor, _) => Failure {
                    reason: Reason::Unhealthy,
                    detail: format!(
                        "the successor was started {} times and never verified itself",
                        step.successor_starts
                    ),
                },
                (Party::Stranger { .. }, _) => Failure {
                    reason: Reason::RecreateFailed,
                    detail: "every recovery-actor container of the hand-over was removed before the successor verified; the recovery actor the seed re-created settled it".into(),
                },
            });
        }
        self.advance(j, i, Phase::Restoring)
    }

    /// Drive the recovery-actor component `i` as far as this process's part goes.
    pub(crate) fn drive_handover(&self, j: &mut Journal, i: usize) -> Result<Flow, ()> {
        loop {
            if !j.is_open() {
                return Ok(Flow::Ended);
            }
            let party = self.party(j);
            let phase = j.steps[i].phase;
            let on_new = matches!(
                party,
                Party::Successor | Party::Stranger { on_new_image: true }
            );
            let next: Result<Phase, Halt> = match phase {
                Phase::Done => return Ok(Flow::StepDone),
                // A successor holding the lease this early: the old actor's container is
                // gone (`may_take_lease`), so there is nothing to hand over from.
                p if party == Party::Successor && !p.touched_old() => {
                    warn!(
                        token = "actor-handover-old-actor-gone",
                        request = %j.request.request_id,
                        "the previous recovery actor's container is gone; this successor takes the machine over"
                    );
                    let step = &mut j.steps[i];
                    if step.new_container.is_none() {
                        step.new_container = self.config.self_container.clone();
                    }
                    step.successor_starts = step.successor_starts.max(1);
                    Ok(Phase::SuccessorActive)
                }
                Phase::Admitted => Ok(Phase::Pulling),
                Phase::Pulling => self.pull_and_check(j, i).map(|()| Phase::Checked),
                Phase::Checked => Ok(Phase::CreatingSuccessor),
                Phase::CreatingSuccessor => {
                    self.create_successor(j, i).map(|()| Phase::StartingSuccessor)
                }
                Phase::StartingSuccessor => {
                    self.start_successor(j, i).map(|()| Phase::AwaitingSuccessor)
                }
                Phase::AwaitingSuccessor => self.await_ready(j, i).map(|()| Phase::HandingOver),
                Phase::HandingOver => match party {
                    Party::Old => return self.hand_over(j, i),
                    Party::Successor => Ok(Phase::SuccessorActive),
                    Party::Stranger { .. } => to_restore(Failure {
                        reason: Reason::RecreateFailed,
                        detail: "the hand-over was in progress when every recovery-actor container was removed".into(),
                    }),
                },
                Phase::SuccessorActive | Phase::SuccessorRenaming | Phase::Verifying
                    if party != Party::Successor =>
                {
                    to_restore(Failure {
                        reason: Reason::Unhealthy,
                        detail: "the successor stopped before it verified itself".into(),
                    })
                }
                Phase::SuccessorActive => self.retire_old_actor(j, i).map(|()| Phase::SuccessorRenaming),
                Phase::SuccessorRenaming => self.take_actor_name(j, i).map(|()| Phase::Verifying),
                Phase::Verifying => self.verify_successor(j, i).map(|()| Phase::Verified),
                Phase::Verified | Phase::OldDiscarded if party == Party::Old => {
                    return self.yield_or_finish(j, i, party);
                }
                Phase::Verified | Phase::OldDiscarded if !on_new => to_restore(Failure {
                    reason: Reason::RecreateFailed,
                    detail: "the successor verified, then every recovery-actor container was removed; the seed re-created the last verified actor it knew".into(),
                }),
                Phase::Verified => self.record_new(j, i).map(|()| Phase::OldDiscarded),
                Phase::OldDiscarded => self.discard_old_actor(j, i).map(|()| Phase::Done),
                Phase::Restoring if party == Party::Successor => {
                    self.hand_back(j, i).map(|()| Phase::HandingBack)
                }
                Phase::HandingBack if party == Party::Successor => {
                    return self.yield_or_finish(j, i, party);
                }
                Phase::Restoring | Phase::HandingBack => {
                    return self.restore_actor(j, i, party).map(|()| Flow::Ended);
                }
                Phase::OldKept | Phase::Created | Phase::Started => {
                    let detail = format!(
                        "the recovery actor's component is journalled in the node agent's phase {phase:?}"
                    );
                    self.finish(j, State::Failed, Some(Reason::Invalid), detail, false)?;
                    return Ok(Flow::Ended);
                }
            };
            match next {
                Ok(next) => self.advance(j, i, next)?,
                Err(Halt::Died) => return Err(()),
                Err(Halt::Fail(failure)) if matches!(phase, Phase::Pulling | Phase::Checked) => {
                    // No successor exists yet: nothing changed for this component.
                    let output = format!("{}{}", failure.detail, moved_before(j, i));
                    self.finish(j, State::Failed, Some(failure.reason), output, false)?;
                    return Ok(Flow::Ended);
                }
                Err(Halt::Fail(failure)) => {
                    warn!(
                        token = "actor-handover-failed",
                        request = %j.request.request_id,
                        reason = %failure.reason,
                        "{}; putting the previous recovery actor back", failure.detail
                    );
                    if j.steps[i].failure.is_none() {
                        j.steps[i].failure = Some(failure);
                    }
                    self.advance(j, i, Phase::Restoring)?;
                }
            }
        }
    }

    /// `creating_successor`: `quasar-recovery.next` from the journalled specification,
    /// labelled with the attempt. Idempotent: a successor this attempt created is reused.
    fn create_successor(&self, j: &mut Journal, i: usize) -> Result<(), Halt> {
        let id = j.request.request_id.clone();
        let mine = self
            .attempt_containers(j, i)
            .map_err(|e| engine(e, Reason::RecreateFailed, "list containers"))?;
        if let Some(c) = mine.into_iter().next() {
            j.steps[i].new_container = Some(c.id);
            return Ok(());
        }
        let name = successor_name();
        if let Some(c) = self
            .retrying(|| self.engine.inspect_container(&name))
            .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the successor's name"))?
        {
            self.remove_leftover_successor(&c)?;
        }
        let mut spec = j.steps[i].spec.clone().ok_or_else(|| {
            fail(
                Reason::RecreateFailed,
                "no rendered specification was journalled",
            )
        })?;
        spec.name = name.clone();
        spec.labels.insert(ATTEMPT_LABEL.into(), id);
        let new = self
            .retrying(|| self.engine.create_container(&spec))
            .map_err(|e| engine(e, Reason::RecreateFailed, "create the successor"))?;
        info!(container = %name, image = %spec.image, "successor created");
        j.steps[i].new_container = Some(new);
        Ok(())
    }

    /// A container holding the successor's name: an earlier attempt's successor that was
    /// never removed is this installation's and goes; anything else is never touched.
    fn remove_leftover_successor(&self, c: &Container) -> Result<(), Halt> {
        let installation = self
            .dir
            .load_machine()
            .ok()
            .flatten()
            .map(|m| m.installation_id);
        let ours = installation.is_some()
            && c.labels.get(labels::INSTALLATION) == installation.as_ref()
            && c.labels.get(labels::PLATFORM_SERVICE).map(String::as_str)
                == Some(Role::RecoveryActor.as_str())
            && !self.is_me(&c.id);
        if !ours {
            return Err(fail(
                Reason::OwnerConflict,
                format!(
                    "container {} ({}) holds the successor's name and is not this installation's; it is never acted on, and the running actor was not touched",
                    c.name, c.image
                ),
            ));
        }
        if !self.journals.scan().unreadable.is_empty() {
            return Err(fail(
                Reason::RecreateFailed,
                format!(
                    "container {} is left by an earlier attempt and an attempt journal cannot be read; it is not removed",
                    c.name
                ),
            ));
        }
        self.retrying(|| self.engine.remove_container(&c.id))
            .map_err(|e| engine(e, Reason::RecreateFailed, "remove an earlier successor"))
    }

    fn successor(&self, j: &Journal, i: usize) -> Result<Option<Container>, Halt> {
        if let Some(new) = &j.steps[i].new_container {
            if let Some(c) = self
                .retrying(|| self.engine.inspect_container(new))
                .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the successor"))?
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

    /// `starting_successor`.
    fn start_successor(&self, j: &Journal, i: usize) -> Result<(), Halt> {
        let c = self
            .successor(j, i)?
            .ok_or_else(|| fail(Reason::RecreateFailed, "the successor is gone"))?;
        if c.running {
            return Ok(());
        }
        match self.retrying(|| self.engine.start_container(&c.id)) {
            Ok(()) => Ok(()),
            Err(EngineError::Crashed) => Err(Halt::Died),
            Err(e) => Err(fail(
                Reason::NeverStarted,
                format!("the engine did not start the successor, so it never ran: {e}"),
            )),
        }
    }

    /// `awaiting_successor`: the successor's ready marker, within the ready timeout. No
    /// engine call while it waits: the successor is the one using the engine.
    fn await_ready(&self, j: &Journal, i: usize) -> Result<(), Halt> {
        let timing = self.config.handover;
        let new = j.steps[i].new_container.clone().unwrap_or_default();
        let deadline = Instant::now() + timing.ready;
        let root = self.dir.root().to_path_buf();
        loop {
            if self.killed() {
                return Err(Halt::Died);
            }
            let path = ready_path(&root, &j.request.request_id);
            if path.exists() {
                if let Ok(Some(ready)) = ready_file(&root, &j.request.request_id).load() {
                    if same_container(&ready.container, &new) {
                        info!(
                            request = %j.request.request_id,
                            successor_version = %ready.version,
                            successor_commit = %ready.commit,
                            "the successor is ready"
                        );
                        return Ok(());
                    }
                }
            }
            if Instant::now() >= deadline {
                break;
            }
            std::thread::sleep(timing.poll);
        }
        let seen = self.successor(j, i)?.map(|c| (c.status.clone(), c.running));
        Err(match seen {
            Some((status, _)) if status == "created" => fail(
                Reason::NeverStarted,
                "the successor never started".to_string(),
            ),
            Some((status, running)) => fail(
                Reason::Unhealthy,
                format!(
                    "the successor did not report ready within {}s (state={status}, running={running})",
                    timing.ready.as_secs()
                ),
            ),
            None => fail(Reason::RecreateFailed, "the successor is gone"),
        })
    }

    /// `handing_over`, the old actor's side: stop serving, release the lease, and watch.
    fn hand_over(&self, j: &Journal, i: usize) -> Result<Flow, ()> {
        info!(
            token = "actor-handover-release",
            request = %j.request.request_id,
            successor = %j.steps[i].new_container.as_deref().unwrap_or(""),
            "handing this machine over to the successor. If no recovery actor is running afterwards, `{FIX}` brings this one back"
        );
        self.stop_serving();
        self.release_lease();
        self.watch_takeover(&j.request.request_id)
    }

    /// The old actor, lease released. The successor normally takes the lease at once and
    /// stops this process. Once the successor moved on, or the takeover wait is over,
    /// this process tries the lease: holding it means the successor never came or is gone,
    /// and the journal says what to do.
    fn watch_takeover(&self, request_id: &str) -> Result<Flow, ()> {
        let timing = self.config.handover;
        let released = Instant::now();
        loop {
            if self.killed() {
                return Err(());
            }
            std::thread::sleep(timing.poll);
            let Ok(Some(j)) = self.journals.load(request_id) else {
                continue;
            };
            let handing_over = j.is_open()
                && j.current()
                    .is_some_and(|i| j.steps[i].phase == Phase::HandingOver);
            if handing_over && released.elapsed() < timing.takeover {
                continue;
            }
            if self.take_lease_now().is_err() {
                continue;
            }
            if let Err(e) = self.serve_again() {
                warn!(token = "actor-reclaim-socket-rebind-failed", "{e}");
            }
            let Ok(Some(j)) = self.journals.load(request_id) else {
                return Ok(Flow::Ended);
            };
            warn!(
                token = "actor-handover-reclaimed",
                request = %request_id,
                "the successor did not keep the machine's lease; this recovery actor took it back"
            );
            self.settle_journal(j)?;
            return Ok(Flow::Ended);
        }
    }

    /// `successor_active`: the old actor out of service, kept.
    fn retire_old_actor(&self, j: &Journal, i: usize) -> Result<(), Halt> {
        info!(
            token = "actor-handover-taking-over",
            request = %j.request.request_id,
            "this successor holds the machine's lease; stopping the previous recovery actor. If no recovery actor is running after this, `{FIX}` brings the previous one back"
        );
        self.keep_old(j, i)
    }

    /// `successor_renaming`: the successor takes the actor's name.
    fn take_actor_name(&self, _j: &Journal, _i: usize) -> Result<(), Halt> {
        let me = self
            .me()
            .ok_or_else(|| {
                fail(
                    Reason::RecreateFailed,
                    "this successor cannot tell its own container",
                )
            })?
            .to_owned();
        let canonical = names::RECOVERY_ACTOR;
        let mine = self
            .retrying(|| self.engine.inspect_container(&me))
            .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the successor"))?
            .ok_or_else(|| {
                fail(
                    Reason::RecreateFailed,
                    "the successor's own container is gone",
                )
            })?;
        if mine.name == canonical {
            return Ok(());
        }
        if let Some(other) = self
            .retrying(|| self.engine.inspect_container(canonical))
            .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the actor's name"))?
        {
            return Err(fail(
                Reason::OwnerConflict,
                format!(
                    "container {} ({}) holds the recovery actor's name; it is never acted on",
                    other.name, other.image
                ),
            ));
        }
        self.retrying(|| self.engine.rename_container(&me, canonical))
            .map_err(|e| engine(e, Reason::RecreateFailed, "rename the successor"))
    }

    /// `verifying`: the successor answers on its own agent socket, and its container runs.
    fn verify_successor(&self, _j: &Journal, _i: usize) -> Result<(), Halt> {
        let timing = self.config.handover;
        let deadline = Instant::now() + timing.verify;
        let socket = self.config.agent_socket.clone();
        let me = self.me().unwrap_or_default().to_owned();
        loop {
            if self.killed() {
                return Err(Halt::Died);
            }
            let answered = crate::server::probe_self(&socket);
            let running = match self.engine.inspect_container(&me) {
                Ok(Some(c)) => c.running,
                Err(EngineError::Crashed) => return Err(Halt::Died),
                _ => false,
            };
            let why = match (&answered, running) {
                (Ok(_), true) => return self.await_agent_contact(),
                (Err(e), _) => {
                    format!("its agent socket {} did not answer ({e})", socket.display())
                }
                (Ok(_), false) => "its container is not running".to_string(),
            };
            if Instant::now() >= deadline {
                return Err(fail(
                    Reason::Unhealthy,
                    format!(
                        "the successor did not verify within {}s: {why}",
                        timing.verify.as_secs()
                    ),
                ));
            }
            std::thread::sleep(timing.poll);
        }
    }

    /// The second half of verifying on a GPU host (architecture §5.6): the node agent
    /// reconnects, which its relay does by polling status on the agent socket throughout
    /// an attempt. Any request another process makes there counts; none within
    /// `agent_contact` is not verified.
    fn await_agent_contact(&self) -> Result<(), Halt> {
        let gpu = matches!(
            self.dir.load_machine(),
            Ok(Some(m)) if m.role == crate::socket::MachineRole::Gpu
        );
        if !gpu {
            return Ok(());
        }
        let timing = self.config.handover;
        let deadline = Instant::now() + timing.agent_contact;
        loop {
            if self.killed() {
                return Err(Halt::Died);
            }
            if self.external_requests() > 0 {
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err(fail(
                    Reason::Unhealthy,
                    format!(
                        "the successor did not verify: no node agent reached its agent socket within {}s",
                        timing.agent_contact.as_secs()
                    ),
                ));
            }
            std::thread::sleep(timing.poll);
        }
    }

    /// `old_discarded`: the kept actor goes, and only then does `seed.json` name the
    /// verified successor (ADR 0007).
    fn discard_old_actor(&self, j: &Journal, i: usize) -> Result<(), Halt> {
        self.discard_old(j, i)?;
        self.record_verified_actor(&j.steps[i].image);
        Ok(())
    }

    fn record_verified_actor(&self, image: &ImageRef) {
        let existing = match self.dir.load_seed_file() {
            Ok(existing) => existing,
            Err(e) => {
                warn!(
                    token = "actor-handover-seed-file-unreadable",
                    "seed.json is unreadable ({e}); it is not updated to name the verified successor, and a seed stays idle until it is fixed"
                );
                return;
            }
        };
        let Ok(Some(machine)) = self.dir.load_machine() else {
            return;
        };
        let state = existing
            .as_ref()
            .map(|f| f.state)
            .unwrap_or(crate::seed::file::SeedState::Active);
        if state != crate::seed::file::SeedState::Active {
            return;
        }
        let file = crate::seed::file::SeedFile {
            format_version: crate::seed::file::FORMAT_VERSION,
            installation_id: machine.installation_id,
            recovery_actor_image: crate::seed::file::ActorImage {
                repository: image.repository.clone(),
                digest: image.digest.clone(),
            },
            state,
        };
        match self.dir.store_seed_file(&file) {
            Ok(()) => info!(
                token = "actor-seed-file-updated",
                image = %image.reference(),
                "seed.json now names the verified recovery actor"
            ),
            Err(e) => warn!(
                token = "actor-seed-file-not-updated",
                "seed.json could not be updated ({e}); a seed would re-create the previous verified actor"
            ),
        }
    }

    /// `restoring` for the successor: put the old actor back under the actor's name,
    /// restartable, before `handing_back` starts it. This process keeps the lease until the
    /// old actor runs.
    fn hand_back(&self, j: &Journal, i: usize) -> Result<(), Halt> {
        let old_id = j.steps[i].old_container.clone().unwrap_or_default();
        let Some(old) = self
            .retrying(|| self.engine.inspect_container(&old_id))
            .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the previous actor"))?
        else {
            // Nothing to hand back to: this successor stays (`yield_or_finish`).
            return Ok(());
        };
        let canonical = names::RECOVERY_ACTOR;
        let me = self.me().unwrap_or_default().to_owned();
        if let Some(mine) = self
            .retrying(|| self.engine.inspect_container(&me))
            .map_err(|e| engine(e, Reason::RecreateFailed, "inspect the successor"))?
        {
            if mine.name == canonical {
                let away = successor_name();
                self.retrying(|| self.engine.rename_container(&me, &away))
                    .map_err(|e| engine(e, Reason::RecreateFailed, "rename the successor away"))?;
            }
        }
        if old.name != canonical {
            self.retrying(|| self.engine.rename_container(&old.id, canonical))
                .map_err(|e| engine(e, Reason::RecreateFailed, "rename the previous actor back"))?;
        }
        let policy = j.steps[i]
            .old_restart
            .unwrap_or(RestartPolicy::UnlessStopped);
        if old.restart != Some(policy) {
            self.retrying(|| self.engine.set_restart_policy(&old.id, policy))
                .map_err(|e| {
                    engine(
                        e,
                        Reason::RecreateFailed,
                        "re-enable the previous actor's restart",
                    )
                })?;
        }
        Ok(())
    }

    /// Settlement `Yield`: another actor finishes this attempt. Falls back to finishing it
    /// here when that actor is gone.
    pub(crate) fn yield_attempt(&self, mut j: Journal, party: Party) -> Result<(), ()> {
        let Some(i) = j.steps.iter().position(|s| s.name == RECOVERY_ACTOR) else {
            return self.run(j);
        };
        match self.yield_or_finish(&mut j, i, party)? {
            Flow::Ended => Ok(()),
            Flow::StepDone => self.run(j),
        }
    }

    /// Hand the machine to the other party of the hand-over (the successor hands back to
    /// the old actor; an old actor started again hands on to its verified successor), or,
    /// when that actor is gone, finish the component here as a stranger would.
    fn yield_or_finish(&self, j: &mut Journal, i: usize, party: Party) -> Result<Flow, ()> {
        let target = match party {
            Party::Successor => j.steps[i].old_container.clone(),
            Party::Old => j.steps[i].new_container.clone(),
            Party::Stranger { .. } => None,
        };
        let found = match target {
            Some(id) => match self.retrying(|| self.engine.inspect_container(&id)) {
                Ok(found) => found,
                Err(EngineError::Crashed) => return Err(()),
                Err(_) => None,
            },
            None => None,
        };
        if let Some(target) = found {
            if self.hand_to(&target)? {
                return Ok(Flow::Ended);
            }
        }
        if j.steps[i].phase == Phase::Done {
            return Ok(Flow::StepDone);
        }
        // Nobody to hand to: the process running now keeps the machine.
        let fallback = Party::Stranger {
            on_new_image: party == Party::Successor,
        };
        self.restore_actor(j, i, fallback)?;
        Ok(Flow::Ended)
    }

    /// Start `target`, make this process's own container stay down once it exits, and
    /// leave the machine to it. `Ok(false)`: the engine did not start it; this process
    /// keeps the machine.
    fn hand_to(&self, target: &Container) -> Result<bool, ()> {
        let crashed = |e: &EngineError| matches!(e, EngineError::Crashed);
        if target.restart != Some(RestartPolicy::UnlessStopped) {
            if let Err(e) = self.retrying(|| {
                self.engine
                    .set_restart_policy(&target.id, RestartPolicy::UnlessStopped)
            }) {
                if crashed(&e) {
                    return Err(());
                }
                warn!(token = "actor-handover-yield-policy-failed", container = %target.name, "re-enable its restart: {e}");
                return Ok(false);
            }
        }
        if !target.running {
            if let Err(e) = self.retrying(|| self.engine.start_container(&target.id)) {
                if crashed(&e) {
                    return Err(());
                }
                warn!(token = "actor-handover-yield-start-failed", container = %target.name, "start it: {e}");
                return Ok(false);
            }
        }
        if let Some(me) = self.me().map(str::to_owned) {
            if let Err(e) = self.retrying(|| self.engine.set_restart_policy(&me, RestartPolicy::No))
            {
                if crashed(&e) {
                    return Err(());
                }
                warn!(
                    token = "actor-handover-self-disable-failed",
                    "disable this actor's restart: {e}"
                );
            }
        }
        info!(
            token = "actor-handover-yielded",
            container = %target.name,
            "handed this machine to {}; this recovery actor exits", target.name
        );
        self.retire();
        Ok(true)
    }

    /// `restoring` (or `handing_back`) for the old actor or a stranger: remove every
    /// successor this attempt created, with its last log lines, and keep this process
    /// the machine's actor under the actor's name.
    fn restore_actor(&self, j: &mut Journal, i: usize, party: Party) -> Result<(), ()> {
        let failure = j.steps[i].failure.clone().unwrap_or(Failure {
            reason: Reason::Unhealthy,
            detail: "the successor did not verify itself".into(),
        });
        let mut output = failure.detail.clone();
        let crash = |e: &EngineError| matches!(e, EngineError::Crashed);

        let mut successors = match self.attempt_containers(j, i) {
            Ok(found) => found,
            Err(e) if crash(&e) => return Err(()),
            Err(e) => {
                output.push_str(&format!("\ncould not list the successor: {e}"));
                Vec::new()
            }
        };
        if let Some(new) = j.steps[i].new_container.clone() {
            if !self.is_me(&new) && !successors.iter().any(|c| same_container(&c.id, &new)) {
                match self.engine.inspect_container(&new) {
                    Ok(Some(c)) => successors.push(c),
                    Err(e) if crash(&e) => return Err(()),
                    _ => {}
                }
            }
        }
        for c in successors {
            if c.running && matches!(party, Party::Stranger { .. }) {
                output.push_str(&format!(
                    "\nthe successor {} is running and was left alone",
                    c.name
                ));
                continue;
            }
            match self.engine.logs_tail(&c.id, 40) {
                Ok(tail) if !tail.trim_end().is_empty() => {
                    output.push_str(&format!("\n--- last lines of {} ---\n", c.name));
                    output.push_str(&tail_output(tail.trim_end(), LOG_TAIL_LIMIT));
                }
                Err(e) if crash(&e) => return Err(()),
                _ => {}
            }
            match self.retrying(|| self.engine.remove_container(&c.id)) {
                Ok(()) => {}
                Err(e) if crash(&e) => return Err(()),
                Err(e) => output.push_str(&format!(
                    "\nthe successor {} could not be removed: {e}",
                    c.name
                )),
            }
        }
        // A stranger removes the previous actor only when it is really left over: stopped.
        // A running one is another actor process, never removed by elimination.
        let leftover_old = match j.steps[i].old_container.clone().filter(|o| !self.is_me(o)) {
            Some(old) if matches!(party, Party::Stranger { .. }) => {
                match self.engine.inspect_container(&old) {
                    Ok(Some(c)) if !c.running => Some(old),
                    Ok(Some(c)) => {
                        output.push_str(&format!(
                            "\nthe previous actor {} is running and was left alone",
                            c.name
                        ));
                        None
                    }
                    Err(e) if crash(&e) => return Err(()),
                    _ => None,
                }
            }
            _ => None,
        };
        if let Some(old) = leftover_old {
            {
                match self.retrying(|| self.engine.remove_container(&old)) {
                    Ok(()) => {}
                    Err(e) if crash(&e) => return Err(()),
                    Err(e) => {
                        output.push_str(&format!("\nthe previous actor could not be removed: {e}"))
                    }
                }
            }
        }

        let policy = match party {
            Party::Old => j.steps[i]
                .old_restart
                .unwrap_or(RestartPolicy::UnlessStopped),
            _ => RestartPolicy::UnlessStopped,
        };
        let restored = match party {
            Party::Old => true,
            Party::Successor => false,
            Party::Stranger { .. } => self.runs_digest(j.steps[i].old_digest.as_deref()),
        };
        match self.keep_this_actor(policy) {
            Ok(()) if restored => output.push_str(
                "\nthe successor did not verify; the previous recovery actor was put back and is running",
            ),
            Ok(()) => output.push_str("\nthe recovery actor running now keeps this machine"),
            Err(why) if why == CRASHED => return Err(()),
            Err(why) => output.push_str(&format!(
                "\nthe recovery actor running now could not take the actor's name back ({why})"
            )),
        }
        if let Err(e) = self.serve_again() {
            warn!(token = "actor-restore-socket-rebind-failed", "{e}");
        }
        output.push_str(&moved_before(j, i));
        self.finish(j, State::Failed, Some(failure.reason), output, restored)
    }

    fn runs_digest(&self, digest: Option<&str>) -> bool {
        let (Some(me), Some(digest)) = (self.me(), digest) else {
            return false;
        };
        matches!(
            self.engine.inspect_container(me),
            Ok(Some(c)) if c.image.split_once('@').map(|(_, d)| d) == Some(digest)
        )
    }

    /// This process's container under the recovery actor's name, with `policy`.
    fn keep_this_actor(&self, policy: RestartPolicy) -> Result<(), String> {
        let crashed = |e: EngineError| match e {
            EngineError::Crashed => CRASHED.to_string(),
            e => e.to_string(),
        };
        let Some(me) = self.me().map(str::to_owned) else {
            return Ok(());
        };
        let canonical = names::RECOVERY_ACTOR;
        let mine = self
            .retrying(|| self.engine.inspect_container(&me))
            .map_err(crashed)?
            .ok_or_else(|| "this actor's own container is gone".to_string())?;
        if mine.name != canonical {
            if let Some(other) = self
                .retrying(|| self.engine.inspect_container(canonical))
                .map_err(crashed)?
            {
                return Err(format!("container {} holds the actor's name", other.name));
            }
            self.retrying(|| self.engine.rename_container(&me, canonical))
                .map_err(crashed)?;
        }
        if mine.restart != Some(policy) {
            self.retrying(|| self.engine.set_restart_policy(&me, policy))
                .map_err(crashed)?;
        }
        Ok(())
    }

    /// Take the lease if nobody holds it; `Err` when another process does.
    fn take_lease_now(&self) -> Result<(), ()> {
        if self.killed() {
            return Err(());
        }
        self.take_lease().map_err(|_| ())
    }
}
