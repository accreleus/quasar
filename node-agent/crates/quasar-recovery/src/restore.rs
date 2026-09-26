//! The operator's `restore` (#352 decision 14, R1): load a pre-update dump into a stopped
//! database and start the control plane it was taken under; or, on an operator-supplied
//! database the operator restored with their own tools, start the control plane whose
//! schema it matches. Only dumps this machine's actor took are restored.
//!
//! Submitted only on the operator socket, which lives in the actor's own container
//! (`docker exec quasar-recovery quasar-recovery restore …`), and journalled and settled
//! like a replacement:
//!
//! ```text
//! admitted → checking → stopping → loading (Quasar's own database) → starting → verifying
//! ```
//!
//! Nothing before `stopping` touches the database or a container, so a corrupt or
//! mismatched dump is refused with nothing changed. From `stopping` on a hold keeps every
//! control plane off the database until the restore has finished, a restart continues it,
//! and every step can be repeated: running the same command again after any failure is
//! safe. The control plane is always created afresh, a new boot incarnation (ADR 0006).

use std::sync::Arc;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use tracing::{info, warn};

use crate::actor::Actor;
use crate::database::{self, DbError, DbOp, Hold, HoldReason};
use crate::dump::{self, DumpDir};
use crate::engine::EngineError;
use crate::install_control::CONTROL_PLANE_FILES;
use crate::journal::{tail_output, CallerTag, Failure, Journal, FORMAT, LOG_TAIL_LIMIT};
use crate::machine::Machine;
use crate::recipe::{self, names, Book, ImageRef, Role};
use crate::replace::{fail, Halt, ATTEMPT_LABEL};
use crate::socket::{
    Accepted, AttemptResult, MachineRole, Reason, Rejection, Request, RequestKind, State,
};
use crate::submit::{is_uuid, kept_name};

/// Inside the actor's container only: no volume holds it.
pub const OPERATOR_SOCKET: &str = "/run/quasar-recovery-operator/operator.sock";

/// The phase a restore is about to act on, or is acting on (as `journal::Phase`).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RestorePhase {
    Admitted,
    /// Reading the dump (or, on an external database, the live schema) and the control
    /// plane it needs. Nothing is touched.
    Checking,
    /// Holding every control plane off the database, then stopping this machine's.
    Stopping,
    /// Dropping and re-creating Quasar's database, then loading the dump into it.
    Loading,
    /// Creating and starting the control plane the database now matches.
    Starting,
    Verifying,
    Done,
}

impl RestorePhase {
    /// From here on an interruption continues the restore rather than ending it.
    pub fn touched(self) -> bool {
        !matches!(self, RestorePhase::Admitted | RestorePhase::Checking)
    }

    pub fn wire_state(self) -> State {
        match self {
            RestorePhase::Admitted => State::Pending,
            RestorePhase::Checking => State::Pulling,
            RestorePhase::Stopping | RestorePhase::Loading | RestorePhase::Starting => {
                State::Recreating
            }
            RestorePhase::Verifying | RestorePhase::Done => State::Verifying,
        }
    }
}

/// A restore's plan and progress, in its journal.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Restore {
    pub phase: RestorePhase,
    /// The dump loaded; `None` on an external database.
    pub dump: Option<String>,
    pub control_plane: ImageRef,
    #[serde(default)]
    pub recipe_revision: Option<u32>,
    /// The database's schema once loaded, found at `checking`.
    #[serde(default)]
    pub schema_version: Option<i64>,
    #[serde(default)]
    pub returns_to: Option<String>,
    #[serde(default)]
    pub created_at: Option<String>,
    #[serde(default)]
    pub new_container: Option<String>,
}

/// The component name crash injection sees for a restore's phases.
pub const COMPONENT: &str = "restore";

fn refuse(req: &Request, reason: Reason, message: impl Into<String>) -> Rejection {
    Rejection {
        request_id: req.request_id.clone(),
        reason,
        message: message.into(),
    }
}

fn nothing_changed(why: impl std::fmt::Display) -> Halt {
    fail(
        Reason::Invalid,
        format!("{why}. Nothing was changed: the database and the control plane are as they were"),
    )
}

impl Actor {
    /// Admits the operator's `restore`. Every refusal happens before the first journal
    /// record; a re-post of the same id is the same restore.
    pub fn submit_restore(self: &Arc<Self>, req: Request) -> Result<Accepted, Rejection> {
        let _gate = self.gate.lock().unwrap();
        if !is_uuid(&req.request_id) {
            return Err(refuse(&req, Reason::Invalid, "request_id must be a uuid"));
        }
        match self.journals.load(&req.request_id) {
            Ok(Some(known)) if known.caller == CallerTag::Operator => {
                return Ok(Accepted {
                    request_id: req.request_id.clone(),
                    previous: Vec::new(),
                })
            }
            Ok(Some(_)) => {
                return Err(refuse(
                    &req,
                    Reason::Invalid,
                    "that request id was used by another caller",
                ))
            }
            Ok(None) => {}
            Err(e) => return Err(refuse(&req, Reason::Busy, format!("the journal: {e}"))),
        }
        if self.journals.was_used(&req.request_id).unwrap_or(true) {
            return Err(refuse(
                &req,
                Reason::Invalid,
                "that request id was already used on this machine",
            ));
        }
        if self.resuming.load(std::sync::atomic::Ordering::SeqCst) {
            return Err(refuse(
                &req,
                Reason::Busy,
                "the recovery actor is still settling this machine after a start",
            ));
        }
        if req.kind != RequestKind::Restore || !req.components.is_empty() {
            return Err(refuse(
                &req,
                Reason::Invalid,
                "the operator socket takes a restore only, naming no components",
            ));
        }
        let scan = self.journals.scan();
        if let Some(id) = scan.open_id() {
            return Err(refuse(
                &req,
                Reason::Busy,
                format!("attempt {id} is in flight on this machine; a restore waits for it to end"),
            ));
        }
        let machine = match self.dir.load_machine() {
            Ok(Some(m)) if m.role != MachineRole::Gpu && m.inputs.control.is_some() => m,
            Ok(Some(_)) => {
                return Err(refuse(
                    &req,
                    Reason::Invalid,
                    "this is a GPU host: it has no database and no control plane to restore",
                ))
            }
            Ok(None) => {
                return Err(refuse(
                    &req,
                    Reason::Invalid,
                    "this machine is not installed yet: start the seed first",
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
        let plan = self
            .plan_restore(&machine, &req)
            .map_err(|why| refuse(&req, Reason::Invalid, why))?;

        let now = self.now();
        let journal = Journal {
            format: FORMAT,
            seq: self.journals.next_seq(),
            caller: CallerTag::Operator,
            request: req.clone(),
            steps: Vec::new(),
            result: AttemptResult {
                request_id: req.request_id.clone(),
                state: State::Pending,
                reason: None,
                components: Vec::new(),
                previous: Vec::new(),
                output: String::new(),
                started_at: now.clone(),
                updated_at: now,
                finished_at: None,
                restored: false,
                release: req.release.clone(),
                dump: plan.dump.clone(),
            },
            restore: Some(plan),
        };
        if let Err(e) = self.journals.store(&journal) {
            return Err(refuse(
                &req,
                Reason::Busy,
                format!("the journal could not be written ({e}); nothing was admitted"),
            ));
        }
        if let Err(e) = self.journals.mark_used(&req.request_id) {
            warn!(token = "actor-used-id-unrecorded", request = %req.request_id, "{e}");
        }
        info!(request = %req.request_id, dump = ?req.dump, "restore admitted");
        let actor = self.clone();
        let id = req.request_id.clone();
        let mut worker = self.worker.lock().unwrap();
        if let Some(done) = worker.take() {
            let _ = done.join();
        }
        *worker = Some(std::thread::spawn(move || actor.drive(&id)));
        Ok(Accepted {
            request_id: req.request_id,
            previous: Vec::new(),
        })
    }

    /// What the request restores, from what machine state already holds. The dump itself
    /// is read at `checking`.
    fn plan_restore(&self, machine: &Machine, req: &Request) -> Result<Restore, String> {
        let owned = Self::is_owned_database(machine);
        let to = req.release.version.clone().filter(|v| !v.is_empty());
        let dumps = DumpDir::new(self.dir.root());
        let mut plan = Restore {
            phase: RestorePhase::Admitted,
            dump: None,
            control_plane: ImageRef {
                repository: String::new(),
                digest: String::new(),
            },
            recipe_revision: None,
            schema_version: None,
            returns_to: None,
            created_at: None,
            new_container: None,
        };
        match (req.dump.as_deref(), owned) {
            (Some(_), false) => return Err(
                "Quasar holds no dump of an operator's own database. Restore your backup with your own tools, then run `restore --to <version>`".into(),
            ),
            (None, true) => return Err(
                "name the dump to restore: `restore --dump <name> --to <version>` (`restore --list` lists them)".into(),
            ),
            (Some(name), true) => {
                if !dump::valid_name(name) {
                    return Err(format!("{name:?} is not the name of a dump"));
                }
                if !dumps.file(name).exists() {
                    return Err(format!("there is no dump named {name} on this machine (`restore --list` lists them)"));
                }
                plan.dump = Some(name.to_owned());
                    let record = dumps
                        .load(name)
                        .map_err(|e| format!("the record of dump {name} cannot be read ({e})"))?
                        .ok_or_else(|| format!("dump {name} has no record, so it is not known to be complete"))?;
                    if let (Some(to), Some(r)) = (&to, &record.returns_to) {
                        if to != r {
                            return Err(format!(
                                "dump {name} returns to {r}, not {to}: check the command, or `restore --list`"
                            ));
                        }
                    }
                    plan.control_plane = record
                        .control_plane
                        .clone()
                        .ok_or_else(|| format!("dump {name} does not record its control plane"))?;
                    plan.recipe_revision = record.recipe_revision;
                    plan.schema_version = Some(record.schema_version);
                    plan.returns_to = record.returns_to.clone();
                    plan.created_at = Some(record.created_at.clone());
            }
            (None, false) => {
                let to = to.ok_or(
                    "name the version to return to: `restore --to <version>`, as the failed update printed it",
                )?;
                let point = database::load_point(self.dir.root())
                    .map_err(|e| format!("restore-point.json cannot be read ({e})"))?
                    .ok_or("no migrating update has run on this machine, so there is nothing to return to")?;
                if point.returns_to != to {
                    return Err(format!(
                        "the last migrating update returns to {}, not {to}",
                        point.returns_to
                    ));
                }
                plan.control_plane = point.control_plane;
                plan.recipe_revision = Some(point.recipe_revision);
                plan.returns_to = Some(point.returns_to);
            }
        }
        Ok(plan)
    }

    fn restore_mut<'a>(&self, j: &'a mut Journal) -> &'a mut Restore {
        j.restore.as_mut().expect("a restore journal")
    }

    fn advance_restore(&self, j: &mut Journal, phase: RestorePhase) -> Result<(), ()> {
        self.restore_mut(j).phase = phase;
        self.commit(j)?;
        #[cfg(any(test, feature = "test-support"))]
        if let Some(crash) = &self.config.crash_after {
            if crash(COMPONENT, crash_point(phase)) {
                return Err(());
            }
        }
        Ok(())
    }

    /// Drives a restore journal to its outcome; `Err` leaves it for the next start.
    pub(crate) fn run_restore(&self, mut j: Journal) -> Result<(), ()> {
        loop {
            if !j.is_open() {
                return Ok(());
            }
            let (phase, loads) = {
                let r = self.restore_mut(&mut j);
                (r.phase, r.dump.is_some())
            };
            let step = match phase {
                RestorePhase::Admitted => Ok(RestorePhase::Checking),
                RestorePhase::Checking => {
                    self.check_restore(&mut j).map(|()| RestorePhase::Stopping)
                }
                RestorePhase::Stopping => self.stop_for_restore(&j).map(|()| {
                    if loads {
                        RestorePhase::Loading
                    } else {
                        RestorePhase::Starting
                    }
                }),
                RestorePhase::Loading => self.load_dump(&j).map(|()| RestorePhase::Starting),
                RestorePhase::Starting => self
                    .start_restored(&mut j)
                    .map(|()| RestorePhase::Verifying),
                RestorePhase::Verifying => self.verify_restored().map(|()| RestorePhase::Done),
                RestorePhase::Done => return self.finish_restore(&mut j),
            };
            match step {
                Ok(next) => self.advance_restore(&mut j, next)?,
                Err(Halt::Died) => return Err(()),
                Err(Halt::Fail(f)) => return self.fail_restore(&mut j, phase, f),
            }
        }
    }

    /// `checking`: the dump is whole and readable, its schema is what its record says,
    /// and the control plane to start matches it. Nothing is touched.
    fn check_restore(&self, j: &mut Journal) -> Result<(), Halt> {
        let machine = self.restore_machine()?;
        let plan = self.restore_mut(j).clone();
        let image = self
            .image_labels(&plan.control_plane)
            .map_err(|e| db_halt(e, "the control plane to start is not available"))?;
        let revision = match plan.recipe_revision {
            Some(r) => r,
            None => database::control_plane_revision(&image).ok_or_else(|| {
                nothing_changed(format!(
                    "{} declares no control-plane recipe revision this actor renders",
                    plan.control_plane.reference()
                ))
            })?,
        };
        if !Book::supports(Role::ControlPlane, revision) {
            return Err(fail(
                Reason::RecipeUnsupported,
                format!(
                    "the control plane to start needs recipe revision {revision}, which this recovery actor does not render ({:?}). Nothing was changed",
                    Book::window(Role::ControlPlane)
                ),
            ));
        }
        let target = database::image_schema(&image).ok_or_else(|| {
            nothing_changed(format!(
                "{} declares no {} label, so it cannot be shown to match the database",
                plan.control_plane.reference(),
                database::IMAGE_SCHEMA
            ))
        })?;
        let schema = match &plan.dump {
            Some(name) => self.check_dump(&machine, name, &plan)?,
            None => {
                let (code, out) = self
                    .run_db(&machine, DbOp::Schema, None)
                    .map_err(|e| db_halt(e, "read the database's schema"))?;
                match database::parse_schema(&out) {
                    Some(s) if code == 0 => s,
                    _ => {
                        return Err(nothing_changed(format!(
                            "the database's schema could not be read: {}",
                            tail_output(out.trim(), LOG_TAIL_LIMIT)
                        )))
                    }
                }
            }
        };
        if schema.dirty {
            return Err(nothing_changed(format!(
                "the {} is at schema {} and marked dirty: a migration did not finish in it",
                if plan.dump.is_some() {
                    "dump"
                } else {
                    "database"
                },
                schema.version
            )));
        }
        if schema.version != target {
            let to = plan
                .returns_to
                .as_deref()
                .unwrap_or("the control plane to start");
            return Err(nothing_changed(if plan.dump.is_some() {
                format!(
                    "the dump is at schema {} but {to} is a control plane of schema {target}: they do not belong together",
                    schema.version
                )
            } else {
                format!(
                    "your database is at schema {} and {to} is a control plane of schema {target}: restore the backup you took before the update first",
                    schema.version
                )
            }));
        }
        if Self::is_owned_database(&machine) {
            match self.engine.inspect_container(names::POSTGRES) {
                Ok(Some(c)) if c.running => {}
                Ok(_) => {
                    return Err(nothing_changed(format!(
                        "Quasar's Postgres ({}) is not running; start it (docker start {}) and run the command again",
                        names::POSTGRES,
                        names::POSTGRES
                    )))
                }
                Err(EngineError::Crashed) => return Err(Halt::Died),
                Err(e) => return Err(nothing_changed(format!("the container engine: {e}"))),
            }
        }
        for name in [
            names::CONTROL_PLANE.to_string(),
            kept_name(names::CONTROL_PLANE),
        ] {
            match self.engine.inspect_container(&name) {
                Ok(Some(c)) if !self.is_ours(&machine, &c, Role::ControlPlane) => {
                    return Err(fail(
                        Reason::OwnerConflict,
                        format!(
                            "container {} ({}) is not this installation's; it is never acted on. Remove it, then run the command again. Nothing was changed",
                            c.name, c.image
                        ),
                    ))
                }
                Ok(_) => {}
                Err(EngineError::Crashed) => return Err(Halt::Died),
                Err(e) => return Err(nothing_changed(format!("the container engine: {e}"))),
            }
        }
        let r = self.restore_mut(j);
        r.recipe_revision = Some(revision);
        r.schema_version = Some(schema.version);
        Ok(())
    }

    fn check_dump(
        &self,
        machine: &Machine,
        name: &str,
        plan: &Restore,
    ) -> Result<database::Schema, Halt> {
        let dir = DumpDir::new(self.dir.root());
        let record = dir.load(name).ok().flatten();
        let (_, sha, magic) = dump::examine(&dir.file(name))
            .map_err(|e| nothing_changed(format!("dump {name} cannot be read: {e}")))?;
        if !magic {
            return Err(nothing_changed(format!(
                "{name} is not a pg_dump custom-format archive (make one with `pg_dump --format=custom`)"
            )));
        }
        if let Some(r) = &record {
            if r.sha256 != sha {
                return Err(nothing_changed(format!(
                    "dump {name} does not match the checksum recorded when it was taken: it is corrupt or was changed"
                )));
            }
        }
        let (code, out) = self
            .run_db(machine, DbOp::Inspect, Some(&format!("{name}.dump")))
            .map_err(|e| db_halt(e, "read the dump"))?;
        let schema = match database::parse_schema(&out) {
            Some(s) if code == 0 => s,
            _ if code != 0 => {
                return Err(nothing_changed(format!(
                    "pg_restore cannot read dump {name}, so it is corrupt or incomplete: {}",
                    tail_output(out.trim(), LOG_TAIL_LIMIT)
                )))
            }
            _ => {
                return Err(nothing_changed(format!(
                    "dump {name} holds no schema_migrations table: it is not a dump of a Quasar database"
                )))
            }
        };
        if let Some(expected) = plan.schema_version {
            if expected != schema.version {
                return Err(nothing_changed(format!(
                    "dump {name} is recorded at schema {expected} but holds schema {}",
                    schema.version
                )));
            }
        }
        Ok(schema)
    }

    fn restore_machine(&self) -> Result<Machine, Halt> {
        match self.dir.load_machine() {
            Ok(Some(m)) => Ok(m),
            _ => Err(nothing_changed("machine state is unreadable")),
        }
    }

    /// `stopping`: the hold first, so nothing starts a control plane from here until the
    /// restore finishes; then this machine's control plane and any kept one, stopped with
    /// their restart disabled.
    fn stop_for_restore(&self, j: &Journal) -> Result<(), Halt> {
        let root = self.dir.root();
        let held = database::load_hold(root).ok().flatten();
        if held.is_none_or(|h| h.reason != HoldReason::RestoreIncomplete) {
            database::store_hold(
                root,
                &Hold {
                    format: 1,
                    reason: HoldReason::RestoreIncomplete,
                    since: self.now(),
                    request_id: Some(j.request.request_id.clone()),
                },
            )
            .map_err(|e| {
                warn!(token = "actor-restore-hold-unwritten", "{e}");
                Halt::Died
            })?;
        }
        for name in [
            names::CONTROL_PLANE.to_string(),
            kept_name(names::CONTROL_PLANE),
        ] {
            let found = self
                .retrying(|| self.engine.inspect_container(&name))
                .map_err(|e| {
                    crate::replace::engine(e, Reason::RecreateFailed, "inspect the control plane")
                })?;
            let Some(c) = found else { continue };
            if c.running {
                let grace = self.config.timing.stop_grace;
                self.retrying(|| self.engine.stop_container(&c.id, grace))
                    .map_err(|e| {
                        crate::replace::engine(e, Reason::RecreateFailed, "stop the control plane")
                    })?;
            }
            if c.restart != Some(crate::engine::RestartPolicy::No) {
                self.retrying(|| {
                    self.engine
                        .set_restart_policy(&c.id, crate::engine::RestartPolicy::No)
                })
                .map_err(|e| {
                    crate::replace::engine(
                        e,
                        Reason::RecreateFailed,
                        "disable the control plane's restart",
                    )
                })?;
            }
        }
        Ok(())
    }

    /// `loading`: the whole database replaced by the dump, in one transaction.
    fn load_dump(&self, j: &Journal) -> Result<(), Halt> {
        let machine = self.restore_machine()?;
        let plan = j.restore.as_ref().expect("a restore journal");
        let name = plan.dump.as_deref().expect("loading names a dump");
        let (code, out) = self
            .run_db(&machine, DbOp::Load, Some(&format!("{name}.dump")))
            .map_err(|e| db_halt(e, "load the dump"))?;
        if code != 0 {
            return Err(fail(
                Reason::RecreateFailed,
                format!(
                    "loading dump {name} failed (exit {code}): {}\nThe database may now be empty; no control plane was started, and none starts until a restore finishes. Run the same command again: a restore can always be repeated",
                    tail_output(out.trim(), LOG_TAIL_LIMIT)
                ),
            ));
        }
        let schema = plan.schema_version.expect("checking found the schema");
        self.set_floor(schema, &j.request.request_id).map_err(|e| {
            warn!(token = "actor-schema-floor-unwritten", "{e}");
            Halt::Died
        })?;
        info!(dump = %name, schema, "dump loaded");
        Ok(())
    }

    /// `starting`: the failed and kept control planes go, and the one the database matches
    /// is created from its recipe and started.
    fn start_restored(&self, j: &mut Journal) -> Result<(), Halt> {
        let mut machine = self.restore_machine()?;
        let plan = self.restore_mut(j).clone();
        if plan.dump.is_none() {
            if let Some(schema) = plan.schema_version {
                self.set_floor(schema, &j.request.request_id)
                    .map_err(|_| Halt::Died)?;
            }
        }
        let id = j.request.request_id.clone();
        for name in [
            names::CONTROL_PLANE.to_string(),
            kept_name(names::CONTROL_PLANE),
        ] {
            let found = self
                .retrying(|| self.engine.inspect_container(&name))
                .map_err(|e| crate::replace::engine(e, Reason::RecreateFailed, "inspect"))?;
            if let Some(c) = found {
                if c.labels.get(ATTEMPT_LABEL) == Some(&id) {
                    continue;
                }
                self.retrying(|| self.engine.remove_container(&c.id))
                    .map_err(|e| {
                        crate::replace::engine(
                            e,
                            Reason::RecreateFailed,
                            "remove the old control plane",
                        )
                    })?;
            }
        }
        let mine = self
            .retrying(|| self.engine.inspect_container(names::CONTROL_PLANE))
            .map_err(|e| crate::replace::engine(e, Reason::RecreateFailed, "inspect"))?
            .filter(|c| c.labels.get(ATTEMPT_LABEL) == Some(&id));
        let container = match mine {
            Some(c) => c.id,
            None => self.create_restored(&mut machine, &plan, &id)?,
        };
        self.retrying(|| self.engine.start_container(&container))
            .map_err(|e| {
                crate::replace::engine(e, Reason::NeverStarted, "start the control plane")
            })?;
        self.restore_mut(j).new_container = Some(container);
        Ok(())
    }

    fn create_restored(
        &self,
        machine: &mut Machine,
        plan: &Restore,
        request_id: &str,
    ) -> Result<String, Halt> {
        let found = self
            .image_labels(&plan.control_plane)
            .map_err(|e| match e {
                DbError::Crashed => Halt::Died,
                DbError::Failed(why) => fail(Reason::PullFailed, why),
            })?;
        self.schema_allows(&found, &plan.control_plane.reference())
            .map_err(|why| fail(Reason::Invalid, why))?;
        let revision = plan.recipe_revision.expect("checking found the revision");
        let secrets = self
            .control_plane_secrets()
            .map_err(|e| fail(Reason::RecreateFailed, e.to_string()))?;
        let mut spec = recipe::render(
            Role::ControlPlane,
            revision,
            &machine.inputs,
            &plan.control_plane,
            &secrets,
        )
        .map_err(|e| fail(Reason::RecreateFailed, e.to_string()))?;
        if let Some(volume) = &secrets.volume {
            self.deliver_secrets_as(
                &plan.control_plane,
                volume,
                &secrets.files,
                CONTROL_PLANE_FILES,
            )
            .map_err(|e| match e {
                crate::actor::ResumeError::Engine(EngineError::Crashed) => Halt::Died,
                e => fail(Reason::RecreateFailed, e.to_string()),
            })?;
        }
        // Recorded first: from here the machine's control plane is this one.
        self.record(
            Role::ControlPlane,
            revision,
            &plan.control_plane,
            spec.clone(),
        )
        .map_err(|_| Halt::Died)?;
        spec.labels.insert(ATTEMPT_LABEL.into(), request_id.into());
        let id = self
            .retrying(|| self.engine.create_container(&spec))
            .map_err(|e| {
                crate::replace::engine(e, Reason::RecreateFailed, "create the control plane")
            })?;
        info!(image = %plan.control_plane.reference(), "restored control plane created");
        Ok(id)
    }

    /// `verifying`: the control plane runs and its healthcheck reports healthy, which
    /// also shows it reached the database.
    fn verify_restored(&self) -> Result<(), Halt> {
        let deadline = Instant::now() + self.config.timing.verify_timeout;
        let mut last;
        loop {
            match self.engine.inspect_container(names::CONTROL_PLANE) {
                Ok(Some(c)) if c.running && c.health.as_deref() == Some("healthy") => return Ok(()),
                Ok(Some(c)) => {
                    last = format!(
                        "state={} health={}",
                        c.status,
                        c.health.as_deref().unwrap_or("none")
                    )
                }
                Ok(None) => last = "the control plane is gone".into(),
                Err(EngineError::Crashed) => return Err(Halt::Died),
                Err(e) => last = format!("the engine did not answer: {e}"),
            }
            if Instant::now() >= deadline {
                return Err(fail(
                    Reason::Unhealthy,
                    format!(
                        "the database is restored, but the control plane did not report healthy within {}s ({last})",
                        self.config.timing.verify_timeout.as_secs()
                    ),
                ));
            }
            std::thread::sleep(self.config.timing.poll.max(Duration::from_millis(1)));
        }
    }

    fn finish_restore(&self, j: &mut Journal) -> Result<(), ()> {
        let plan = self.restore_mut(j).clone();
        database::clear_hold(self.dir.root()).map_err(|_| ())?;
        let schema = plan.schema_version.unwrap_or_default();
        let output = match &plan.dump {
            Some(name) => format!(
                "Restored dump {name} (schema {schema}, taken {}) into Quasar's database and started control plane {} again. Anything written after the dump was taken is gone.",
                plan.created_at.as_deref().unwrap_or("before the update"),
                plan.returns_to.as_deref().unwrap_or("")
            ),
            None => format!(
                "Your database is at schema {schema}; started control plane {} against it.",
                plan.returns_to.as_deref().unwrap_or("")
            ),
        };
        self.finish(j, State::Succeeded, None, output, false)
    }

    fn fail_restore(&self, j: &mut Journal, at: RestorePhase, f: Failure) -> Result<(), ()> {
        // Once the database is restored the hold has done its work, whatever the control
        // plane then does; before that it keeps every control plane off a partial load.
        if matches!(at, RestorePhase::Starting | RestorePhase::Verifying) {
            let _ = database::clear_hold(self.dir.root());
        }
        let mut output = f.detail;
        if at.touched() {
            if let Some(c) = self.restore_mut(j).new_container.clone() {
                if let Ok(tail) = self.engine.logs_tail(&c, 40) {
                    if !tail.trim().is_empty() {
                        output.push_str("\n--- last lines of the control plane ---\n");
                        output.push_str(&tail_output(tail.trim_end(), LOG_TAIL_LIMIT));
                    }
                }
            }
        }
        self.finish(j, State::Failed, Some(f.reason), output, false)
    }

    /// Settle: a restore interrupted before it stopped anything changed nothing.
    pub(crate) fn interrupt_restore(&self, mut j: Journal) -> Result<(), ()> {
        let output = "The recovery actor, the container engine or the machine restarted before the restore stopped anything: nothing was changed. Run the command again.".to_string();
        self.finish(
            &mut j,
            State::Failed,
            Some(Reason::Interrupted),
            output,
            false,
        )
    }
}

fn db_halt(e: DbError, what: &str) -> Halt {
    match e {
        DbError::Crashed => Halt::Died,
        DbError::Failed(why) => nothing_changed(format!("{what}: {why}")),
    }
}

/// What `ActorConfig::crash_after` is called with once a restore has committed `p`: the
/// component [`COMPONENT`] and this stand-in journal phase, one per restore phase.
#[cfg(any(test, feature = "test-support"))]
pub fn crash_point(p: RestorePhase) -> crate::journal::Phase {
    use crate::journal::Phase;
    match p {
        RestorePhase::Admitted => Phase::Admitted,
        RestorePhase::Checking => Phase::Checked,
        RestorePhase::Stopping => Phase::OldKept,
        RestorePhase::Loading => Phase::Dumping,
        RestorePhase::Starting => Phase::Started,
        RestorePhase::Verifying => Phase::Verifying,
        RestorePhase::Done => Phase::Done,
    }
}

/// The request the CLI sends.
pub fn request(request_id: String, dump: Option<String>, to: Option<String>) -> Request {
    Request {
        request_id,
        kind: RequestKind::Restore,
        components: Vec::new(),
        release: crate::socket::Release {
            id: String::new(),
            version: to,
            source_commit: String::new(),
        },
        migrates: false,
        schema_version: None,
        external_backup_confirmed: false,
        dump,
        purge: false,
        wait_timeout_s: 0,
        from_version: None,
    }
}
