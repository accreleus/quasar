//! The operator's `restore` (#352 decisions 14 and 20, R1): load a pre-update dump into a
//! stopped database and start the control plane it was taken under; or, on an
//! operator-supplied database the operator restored with their own tools, start the control
//! plane whose schema it matches; or (#380) load a pre-RH-06 stack's `pg_dump` into a fresh
//! install before its control plane's first boot, which then migrates it forward.
//!
//! The last is an **import** (`restore --dump -`, `crate::dump_dir`): accepted only on
//! Quasar's own database and only while this machine has never created a control plane
//! (the seed's `QUASAR_AWAIT_RESTORE=1` holds the first one back). Its dump may be at any
//! schema up to the installed control plane's, never above; the load also sets every host
//! the old install recorded offline, since their agents are gone and must re-enroll.
//!
//! Submitted only on the operator socket (`crate::operator`, `POST /v1/restore`), which lives
//! in the actor's own container (`docker exec quasar-recovery quasar-recovery restore …`),
//! and journalled and settled like a replacement:
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
use crate::dump_dir::{self as dump, DumpDir};
use crate::engine::EngineError;
use crate::install_control::CONTROL_PLANE_FILES;
use crate::journal::{
    tail_output, CallerTag, Failure, Journal, FORMAT, LOG_TAIL_LIMIT, RESTORE_FORMAT,
};
use crate::machine::Machine;
use crate::recipe::{self, names, Book, ImageRef, Role};
use crate::replace::{fail, Halt, ATTEMPT_LABEL};
use crate::socket::{
    Accepted, AttemptResult, MachineRole, Reason, Rejection, Request, RequestKind, State,
};
use crate::submit::{is_uuid, kept_name};

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
    /// The dump is an operator's file (#380): loaded into a fresh install, whose installed
    /// control plane then migrates it forward on its first boot.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub imported: bool,
    /// No control plane was ever created on this machine: the install itself creates it.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub fresh_install: bool,
    /// An import's target: the installed control plane's schema, which its first boot
    /// migrates the database to. Found at `checking`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub migrates_to: Option<i64>,
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

/// A refusal at `checking`, the only phase that has touched nothing. Never used later.
fn nothing_changed(why: impl std::fmt::Display) -> Halt {
    fail(
        Reason::Invalid,
        format!("{why}. Nothing was changed: the database and the control plane are as they were"),
    )
}

/// What `loading` leaves when it stops short, whatever stopped it: the helper may already
/// have dropped and re-created the database.
const LOAD_UNFINISHED: &str = "The database may now be empty; no control plane was started, and none starts until a restore finishes. Do not start the control plane by hand. Run the same command again: a restore can always be repeated";

/// `loading` stopped short: a helper that exited non-zero, or an engine that stopped
/// answering (a daemon restart) with the helper's work unknown.
fn load_halt(name: &str, e: DbError) -> Halt {
    match e {
        DbError::Crashed => Halt::Died,
        DbError::Failed(why) => fail(
            Reason::RecreateFailed,
            format!("loading dump {name} did not finish: {why}\n{LOAD_UNFINISHED}"),
        ),
    }
}

/// From `stopping` on, the hold is in place: a failure says so rather than that nothing
/// changed.
fn held(why: impl std::fmt::Display) -> Halt {
    fail(
        Reason::RecreateFailed,
        format!("{why}. The control plane is stopped and none starts until a restore finishes; run the same command again"),
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
            format: RESTORE_FORMAT,
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
            warn!(token = "actor-restore-id-unrecorded", request = %req.request_id, "{e}");
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
            imported: false,
            fresh_install: false,
            migrates_to: None,
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
                if dump::is_import(name) {
                    return self.plan_import(machine, plan, to.as_deref());
                }
                    let record = dumps
                        .load(name)
                        .map_err(|e| format!("the record of dump {name} cannot be read ({e})"))?
                        .ok_or_else(|| format!("dump {name} has no record, so it is not known to be complete"))?;
                    if let (Some(by), false) = (&record.restored_by, req.force_again) {
                        return Err(format!(
                            "dump {name} was already restored{} (restore {by}). Restoring it again discards everything written since then; add --force-again to do it anyway",
                            record
                                .restored_at
                                .as_deref()
                                .map(|t| format!(" at {t}"))
                                .unwrap_or_default()
                        ));
                    }
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
                    .ok_or("there is no restore point on this machine: no migrating update has run here, or the restore it printed has already been run")?;
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

    /// An import's plan: only before this machine's first control plane, which it then
    /// starts.
    fn plan_import(
        &self,
        machine: &Machine,
        mut plan: Restore,
        to: Option<&str>,
    ) -> Result<Restore, String> {
        if !self.control_plane_never_created()? {
            return Err(format!(
                "a pre-RH-06 dump is loaded only into a fresh install, before its control plane's first boot, and this machine's control plane has already been created. To load it, reinstall: `docker exec {} quasar-recovery uninstall --purge`, then install again with the seed and {}=1, then run the restore",
                names::RECOVERY_ACTOR,
                crate::bootstrap::AWAIT_RESTORE
            ));
        }
        if to.is_some() {
            return Err(
                "a pre-RH-06 dump starts this install's own control plane: run it without --to"
                    .into(),
            );
        }
        plan.imported = true;
        plan.fresh_install = true;
        plan.control_plane = machine
            .install_images
            .get(&Role::ControlPlane)
            .cloned()
            .ok_or("machine state names no control-plane image to start")?;
        Ok(plan)
    }

    /// No control plane was ever created here: no record of one, and no container.
    fn control_plane_never_created(&self) -> Result<bool, String> {
        let recorded = self
            .dir
            .load_service(Role::ControlPlane)
            .map_err(|e| format!("services/control-plane.json cannot be read ({e})"))?
            .is_some();
        let found = self
            .engine
            .inspect_container(names::CONTROL_PLANE)
            .map_err(|e| format!("the container engine did not answer ({e})"))?
            .is_some();
        Ok(!recorded && !found)
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
        let machine = self.restore_machine().map_err(nothing_changed)?;
        let plan = self.restore_mut(j).clone();
        let image = self
            .image_labels(&plan.control_plane)
            .map_err(|e| check_halt(e, "the control plane to start is not available"))?;
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
                // A running control plane migrates the database again on its next boot:
                // the operator's restored backup would not stay restored (#364 review B2).
                self.no_control_plane_running()?;
                let (code, out) = self
                    .run_db(&machine, DbOp::Schema, None)
                    .map_err(|e| check_halt(e, "read the database's schema"))?;
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
        let matches = if plan.imported {
            schema.version <= target
        } else {
            schema.version == target
        };
        if !matches {
            let to = plan
                .returns_to
                .as_deref()
                .unwrap_or("the control plane to start");
            return Err(nothing_changed(if plan.imported {
                format!(
                    "the dump is at schema {}, newer than this install's control plane (schema {target}): it was taken from a newer release. Install that release or a newer one with the seed, then restore",
                    schema.version
                )
            } else if plan.dump.is_some() {
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
        if r.imported {
            r.migrates_to = Some(target);
        }
        Ok(())
    }

    /// An external database is the operator's to restore, with this machine's control
    /// planes stopped: refused, with nothing changed, while one runs.
    fn no_control_plane_running(&self) -> Result<(), Halt> {
        for name in [
            names::CONTROL_PLANE.to_string(),
            kept_name(names::CONTROL_PLANE),
        ] {
            match self.engine.inspect_container(&name) {
                Ok(Some(c)) if c.running => {
                    return Err(nothing_changed(format!(
                        "control plane {name} is running, and on its next start it would migrate your database again. Stop it (docker stop {name}), restore the backup you took before the update, then run the command again"
                    )))
                }
                Ok(_) => {}
                Err(EngineError::Crashed) => return Err(Halt::Died),
                Err(e) => return Err(nothing_changed(format!("the container engine: {e}"))),
            }
        }
        Ok(())
    }

    fn check_dump(
        &self,
        machine: &Machine,
        name: &str,
        plan: &Restore,
    ) -> Result<database::Schema, Halt> {
        let dir = DumpDir::new(self.dir.root());
        // An import has no record: `pg_restore` reading the whole archive is its check.
        let record = if plan.imported {
            None
        } else {
            Some(
                dir.load(name)
                    .map_err(|e| {
                        nothing_changed(format!("the record of dump {name} cannot be read ({e})"))
                    })?
                    .ok_or_else(|| {
                        nothing_changed(format!(
                            "dump {name} has no record, so it is not known to be complete"
                        ))
                    })?,
            )
        };
        let (_, sha, magic) = dump::examine(&dir.file(name))
            .map_err(|e| nothing_changed(format!("dump {name} cannot be read: {e}")))?;
        if !magic {
            return Err(nothing_changed(format!(
                "{name} is not a pg_dump custom-format archive (make one with `pg_dump --format=custom`)"
            )));
        }
        if record.is_some_and(|r| r.sha256 != sha) {
            return Err(nothing_changed(format!(
                "dump {name} does not match the checksum recorded when it was taken: it is corrupt or was changed"
            )));
        }
        let (code, out) = self
            .run_db(machine, DbOp::Inspect, Some(&format!("{name}.dump")))
            .map_err(|e| check_halt(e, "read the dump"))?;
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

    fn restore_machine(&self) -> Result<Machine, &'static str> {
        match self.dir.load_machine() {
            Ok(Some(m)) => Ok(m),
            _ => Err("machine state is unreadable"),
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
        let plan = j.restore.as_ref().expect("a restore journal");
        let name = plan.dump.as_deref().expect("loading names a dump");
        let machine = self
            .restore_machine()
            .map_err(|why| load_halt(name, DbError::Failed(why.into())))?;
        let op = if plan.imported {
            DbOp::Import
        } else {
            DbOp::Load
        };
        let (code, out) = self
            .run_db(&machine, op, Some(&format!("{name}.dump")))
            .map_err(|e| load_halt(name, e))?;
        if code != 0 {
            return Err(load_halt(
                name,
                DbError::Failed(format!(
                    "exit {code}: {}",
                    tail_output(out.trim(), LOG_TAIL_LIMIT)
                )),
            ));
        }
        let schema = plan.schema_version.expect("checking found the schema");
        self.set_floor(schema, &j.request.request_id).map_err(|e| {
            warn!(token = "actor-restore-floor-unwritten", "{e}");
            Halt::Died
        })?;
        info!(dump = %name, schema, "dump loaded");
        Ok(())
    }

    /// `starting`: the failed and kept control planes go, and the one the database matches
    /// is created from its recipe and started.
    fn start_restored(&self, j: &mut Journal) -> Result<(), Halt> {
        let mut machine = self.restore_machine().map_err(held)?;
        let plan = self.restore_mut(j).clone();
        if plan.dump.is_none() {
            if let Some(schema) = plan.schema_version {
                self.set_floor(schema, &j.request.request_id)
                    .map_err(|_| Halt::Died)?;
            }
        }
        if plan.fresh_install {
            return self.start_fresh(&machine, &plan, &j.request.request_id);
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

    /// `starting` on a fresh install: the hold goes and the install itself continues,
    /// creating its control plane, which migrates the loaded database forward as it boots.
    fn start_fresh(&self, machine: &Machine, plan: &Restore, request_id: &str) -> Result<(), Halt> {
        if let Some(target) = plan.migrates_to {
            self.raise_floor(target, request_id).map_err(|e| {
                warn!(token = "actor-restore-floor-unwritten", "{e}");
                Halt::Died
            })?;
        }
        database::clear_hold(self.dir.root()).map_err(|_| Halt::Died)?;
        match self.ensure_control_machine(machine) {
            Ok(()) => Ok(()),
            Err(crate::actor::ResumeError::Engine(EngineError::Crashed)) => Err(Halt::Died),
            Err(e) => Err(fail(
                Reason::RecreateFailed,
                format!("the database is loaded, but the install could not create its control plane: {e}"),
            )),
        }
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
        self.mark_restored(&plan, &j.request.request_id);
        self.remove_import(&plan);
        let schema = plan.schema_version.unwrap_or_default();
        let output = match &plan.dump {
            Some(_) if plan.imported => format!(
                "Loaded the pre-RH-06 dump (schema {schema}) into this install's database and started its control plane, which migrated it to schema {}. Its accounts, library and settings are back. Every host the old install had is offline: add each GPU host again from Admin > Fleet > Add host, under its old node name, to keep its history and homes.",
                plan.migrates_to.unwrap_or_default()
            ),
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
        j.format = FORMAT;
        self.finish(j, State::Succeeded, None, output, false)
    }

    /// A restore that succeeded: its dump is recorded as restored (so the printed command
    /// is not run twice by accident) and the restore point it went back to is cleared.
    /// Best effort: the database is restored whatever these writes do.
    fn mark_restored(&self, plan: &Restore, request_id: &str) {
        let root = self.dir.root();
        if let Some(name) = &plan.dump {
            let dir = DumpDir::new(root);
            match dir.load(name) {
                Ok(Some(mut record)) if record.restored_by.as_deref() != Some(request_id) => {
                    record.restored_by = Some(request_id.to_owned());
                    record.restored_at = Some(self.now());
                    if let Err(e) = dir.store(&record) {
                        warn!(token = "actor-restore-mark-unwritten", dump = %name, "{e}");
                    }
                }
                Ok(_) => {}
                Err(e) => warn!(token = "actor-restore-record-unreadable", dump = %name, "{e}"),
            }
        }
        let point = database::load_point(root).ok().flatten();
        let went_back = point.is_some_and(|p| match &plan.dump {
            Some(name) => p.dump.as_deref() == Some(name.as_str()),
            None => p.dump.is_none() && Some(&p.returns_to) == plan.returns_to.as_ref(),
        });
        if went_back {
            if let Err(e) = database::clear_point(root) {
                warn!(token = "actor-restore-point-uncleared", "{e}");
            }
        }
    }

    fn fail_restore(&self, j: &mut Journal, at: RestorePhase, f: Failure) -> Result<(), ()> {
        // Once the database is restored the hold has done its work, whatever the control
        // plane then does; before that it keeps every control plane off a partial load.
        if matches!(at, RestorePhase::Starting | RestorePhase::Verifying) {
            let _ = database::clear_hold(self.dir.root());
        }
        self.remove_import(self.restore_mut(j));
        let mut output = f.detail;
        if self.restore_mut(j).imported {
            output.push('\n');
            output.push_str(&import_hint(at));
        }
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
        j.format = FORMAT;
        self.finish(j, State::Failed, Some(f.reason), output, false)
    }

    /// An import is a copy of the operator's file: gone once its restore has ended, since
    /// running the command again copies it afresh.
    fn remove_import(&self, plan: &Restore) {
        if let (true, Some(name)) = (plan.imported, &plan.dump) {
            if let Err(e) = DumpDir::new(self.dir.root()).remove(name) {
                warn!(token = "actor-import-not-removed", dump = %name, "{e}");
            }
        }
    }

    /// Settle: a restore interrupted before it stopped anything changed nothing.
    pub(crate) fn interrupt_restore(&self, mut j: Journal) -> Result<(), ()> {
        if let Some(plan) = j.restore.clone() {
            self.remove_import(&plan);
        }
        let output = "The recovery actor, the container engine or the machine restarted before the restore stopped anything: nothing was changed. Run the command again.".to_string();
        j.format = FORMAT;
        self.finish(
            &mut j,
            State::Failed,
            Some(Reason::Interrupted),
            output,
            false,
        )
    }
}

/// What an operator does after a failed import: before the control plane is created the
/// command can be run again, with the same file (its copy is not kept); after, the install
/// has booted on the loaded data and only a reinstall loads another dump.
fn import_hint(at: RestorePhase) -> String {
    let start_over = format!(
        "`docker exec {} quasar-recovery uninstall --purge`, then install again with the seed and {}=1",
        names::RECOVERY_ACTOR,
        crate::bootstrap::AWAIT_RESTORE
    );
    if matches!(at, RestorePhase::Starting | RestorePhase::Verifying) {
        format!(
            "The install's control plane was created on the loaded data, and its first boot migrates it forward; `docker logs {}` says how far it got. To load the dump again instead, start over: {start_over}",
            names::CONTROL_PLANE
        )
    } else {
        format!(
            "Your file was not kept: run the same command again with it. To start over instead: {start_over}"
        )
    }
}

fn check_halt(e: DbError, what: &str) -> Halt {
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
    request_again(request_id, dump, to, false)
}

/// [`request`], with `--force-again`: a dump already restored is restored again.
pub fn request_again(
    request_id: String,
    dump: Option<String>,
    to: Option<String>,
    force_again: bool,
) -> Request {
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
        force_again,
    }
}
