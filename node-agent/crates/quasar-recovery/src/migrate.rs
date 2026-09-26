//! A migrating control-plane replacement (#352 decision 14, ADR 0004 amendment): the part
//! of [`crate::replace`] that only a control plane moving its database's schema forward
//! takes.
//!
//! - At `checked` the step is decided migrating, by the request or by the images' own
//!   schema labels, whichever says so; an external database without the operator's
//!   confirmation is refused there, before anything stops (`backup_unconfirmed`).
//! - `dumping`, while the old control plane still runs: a Quasar-owned database is dumped
//!   into `dumps/`, refused `backup_failed` without room or on any error, and the last
//!   three dumps are kept; the restore point is recorded for either kind of database.
//! - Before the new control plane starts, the schema floor rises to its schema.
//! - A failure is never restored automatically: the attempt ends `failed`, its output
//!   ending with the one `restore` command, and the new container is left as it is (the
//!   build that matches the migrated schema, and the console if it serves one).

use tracing::{info, warn};

use crate::actor::Actor;
use crate::database::{self, DbError, DbOp, RestorePoint};
use crate::dump_dir::{self as dump, DumpDir, DumpRecord};
use crate::engine::{EngineError, Image};
use crate::journal::{tail_output, Failure, Journal, LOG_TAIL_LIMIT};
use crate::machine::Machine;
use crate::recipe::ImageRef;
use crate::replace::{fail, moved_before, Halt};
use crate::socket::{Reason, State};

/// An engine read the step cannot do without: a crash stops the process, anything else
/// fails the step before anything stopped.
fn died(e: EngineError) -> Halt {
    match e {
        EngineError::Crashed => Halt::Died,
        e => fail(Reason::BackupFailed, format!("the container engine: {e}")),
    }
}

fn db(e: DbError, what: &str) -> Halt {
    match e {
        DbError::Crashed => Halt::Died,
        DbError::Failed(why) => fail(Reason::BackupFailed, format!("{what}: {why}")),
    }
}

impl Actor {
    /// Whether step `i`, a control plane of `found`, migrates; refused here when it would
    /// run against a newer schema, or migrate an external database nobody backed up.
    pub(crate) fn control_plane_migrates(
        &self,
        j: &Journal,
        machine: &Machine,
        found: &Image,
        reference: &str,
    ) -> Result<bool, Halt> {
        self.schema_allows(found, reference).map_err(|why| {
            fail(
                Reason::Invalid,
                format!("{why}. The running control plane was not touched"),
            )
        })?;
        let new = database::image_schema(found);
        let read = |e: EngineError| match e {
            EngineError::Crashed => Halt::Died,
            e => fail(
                Reason::PullFailed,
                format!("read the running control plane's image: {e}"),
            ),
        };
        let old = match self.recorded_control_plane(machine).map_err(read)? {
            Some((image, _)) => self.local_image_schema(&image).map_err(read)?,
            None => None,
        };
        let migrating = j.request.migrates || matches!((new, old), (Some(n), Some(o)) if n > o);
        if migrating && !Self::is_owned_database(machine) && !j.request.external_backup_confirmed {
            return Err(fail(
                Reason::BackupUnconfirmed,
                "this control plane migrates the operator's own database, and no current backup of it was confirmed; Quasar never dumps an operator's database. Nothing was changed: take a backup, then apply again confirming it",
            ));
        }
        Ok(migrating)
    }

    /// The schema step `i` moves the database to: its image's label, else the request's.
    pub(crate) fn migration_target(
        &self,
        j: &Journal,
        i: usize,
    ) -> Result<Option<i64>, EngineError> {
        Ok(self
            .local_image_schema(&j.steps[i].image)?
            .or(j.request.schema_version))
    }

    /// `dumping`: the pre-update dump of a Quasar-owned database, then the restore point.
    /// Idempotent on the request id: a dump this attempt already completed is kept.
    pub(crate) fn take_dump(&self, j: &mut Journal, i: usize) -> Result<(), Halt> {
        let machine = match self.dir.load_machine() {
            Ok(Some(m)) => m,
            _ => return Err(fail(Reason::BackupFailed, "machine state is unreadable")),
        };
        let request_id = j.request.request_id.clone();
        let (old_image, old_revision) = self
            .recorded_control_plane(&machine)
            .map_err(died)?
            .ok_or_else(|| {
                fail(
                    Reason::BackupFailed,
                    "this machine has no record of the control plane it runs, so a restore could not start it again; nothing was changed",
                )
            })?;
        let old_found = self
            .engine
            .inspect_image(&old_image.reference())
            .map_err(died)?;
        let returns_to = database::returns_to(
            j.request.from_version.as_deref(),
            old_found.as_ref(),
            &old_image,
        );
        let mut point = RestorePoint {
            format: 1,
            request_id: request_id.clone(),
            returns_to: returns_to.clone(),
            control_plane: old_image.clone(),
            recipe_revision: old_revision,
            schema_version: old_found.as_ref().and_then(database::image_schema),
            dump: None,
            created_at: self.now(),
        };
        if Self::is_owned_database(&machine) {
            let record = match DumpDir::new(self.dir.root()).taken_by(&request_id) {
                Some(done) => done,
                None => self.dump_now(&machine, j, &old_image, old_revision, &returns_to)?,
            };
            point.schema_version = Some(record.schema_version);
            point.dump = Some(record.name.clone());
            j.steps[i].dump = Some(record.name.clone());
            j.result.dump = Some(record.name);
        }
        database::store_point(self.dir.root(), &point).map_err(|e| {
            fail(
                Reason::BackupFailed,
                format!("record the restore point: {e}"),
            )
        })?;
        Ok(())
    }

    fn dump_now(
        &self,
        machine: &Machine,
        j: &Journal,
        old_image: &ImageRef,
        old_revision: u32,
        returns_to: &str,
    ) -> Result<DumpRecord, Halt> {
        let dir = DumpDir::new(self.dir.root());
        dir.ensure()
            .and_then(|()| dir.remove_partials())
            .map_err(|e| fail(Reason::BackupFailed, format!("the dumps directory: {e}")))?;
        let (code, out) = self
            .run_db(machine, DbOp::Size, None)
            .map_err(|e| db(e, "measure the database"))?;
        let size: u64 = out
            .lines()
            .rev()
            .find_map(|l| l.trim().parse().ok())
            .filter(|_| code == 0)
            .ok_or_else(|| {
                fail(
                    Reason::BackupFailed,
                    format!(
                        "the database could not be measured, so no dump was taken ({}); the control plane was not replaced and the database was not touched",
                        out.trim()
                    ),
                )
            })?;
        let need = dump::space_needed(size);
        let free = (self.config.free_space)(dir.path())
            .map_err(|e| fail(Reason::BackupFailed, format!("read the free space: {e}")))?;
        if free < need {
            return Err(fail(
                Reason::BackupFailed,
                format!(
                    "not enough free space for the pre-update dump: it needs about {} and {} is free. Free some space on this machine and apply again; the control plane was not replaced and the database was not touched",
                    dump::human(need),
                    dump::human(free)
                ),
            ));
        }
        // Named once its schema is known; `pending-<id>` meanwhile, never listed.
        let pending = format!("pending-{}", &j.request.request_id[..8]);
        let partial_file = format!("{pending}.dump.partial");
        let (code, out) = self
            .run_db(machine, DbOp::Dump, Some(&partial_file))
            .map_err(|e| db(e, "dump the database"))?;
        let partial = dir.partial(&pending);
        let cleanup = |why: String| {
            let _ = std::fs::remove_file(&partial);
            fail(
                Reason::BackupFailed,
                format!(
                    "{why}; the control plane was not replaced and the database was not touched"
                ),
            )
        };
        if code != 0 {
            return Err(cleanup(format!(
                "the pre-update dump failed (pg_dump exited {code}): {}",
                tail_output(out.trim(), LOG_TAIL_LIMIT)
            )));
        }
        let (bytes, sha, magic) = match dump::examine(&partial) {
            Ok(v) => v,
            Err(e) => return Err(cleanup(format!("the dump file cannot be read: {e}"))),
        };
        if !magic {
            return Err(cleanup(
                "the dump file is not a pg_dump custom-format archive".into(),
            ));
        }
        let (code, out) = self
            .run_db(machine, DbOp::Inspect, Some(&partial_file))
            .map_err(|e| db(e, "check the dump"))?;
        let schema = match database::parse_schema(&out) {
            Some(s) if code == 0 => s,
            _ => {
                return Err(cleanup(format!(
                    "the dump does not read back as a Quasar database: {}",
                    tail_output(out.trim(), LOG_TAIL_LIMIT)
                )))
            }
        };
        if schema.dirty {
            return Err(cleanup(format!(
                "the database is at schema {} and marked dirty (a migration did not finish), so a dump of it is no way back",
                schema.version
            )));
        }
        let now = self.now();
        let mut name = format!("{}-schema-{}", dump::stamp(&now), schema.version);
        if dir.load(&name).ok().flatten().is_some() {
            name = format!("{name}-{}", &j.request.request_id[..8].to_ascii_lowercase());
        }
        let renamed =
            std::fs::rename(&partial, dir.partial(&name)).and_then(|()| dir.complete(&name));
        if let Err(e) = renamed {
            return Err(cleanup(format!("the dump could not be kept: {e}")));
        }
        let record = DumpRecord {
            format: dump::FORMAT,
            name: name.clone(),
            schema_version: schema.version,
            created_at: now,
            size_bytes: bytes,
            sha256: sha,
            request_id: Some(j.request.request_id.clone()),
            control_plane: Some(old_image.clone()),
            recipe_revision: Some(old_revision),
            returns_to: Some(returns_to.to_owned()),
        };
        if let Err(e) = dir.store(&record) {
            let _ = dir.remove(&name);
            return Err(fail(
                Reason::BackupFailed,
                format!("the dump's record could not be written ({e}); the control plane was not replaced and the database was not touched"),
            ));
        }
        match dir.prune(&name) {
            Ok(gone) if !gone.is_empty() => {
                info!(removed = ?gone, "older pre-update dumps removed; the last three are kept")
            }
            Ok(_) => {}
            Err(e) => warn!(
                token = "actor-dump-prune-failed",
                "older pre-update dumps could not be removed: {e}"
            ),
        }
        info!(dump = %name, schema = schema.version, bytes, "pre-update dump taken");
        Ok(record)
    }

    /// An attempt that ends before its control plane moved leaves no dump of its own.
    pub(crate) fn discard_dump(&self, request_id: &str) {
        let dir = DumpDir::new(self.dir.root());
        let _ = dir.remove_partials();
        if let Some(record) = dir.taken_by(request_id) {
            if let Err(e) = dir.remove(&record.name) {
                warn!(token = "actor-dump-discard-failed", dump = %record.name, "{e}");
            }
        }
    }

    /// The end of a migrating control plane that did not verify: `failed`, not restored,
    /// the new container left as it is, and the output ending with the restore command.
    pub(crate) fn fail_migrating(
        &self,
        j: &mut Journal,
        i: usize,
        failure: Failure,
    ) -> Result<(), ()> {
        let mut output = failure.detail.clone();
        let new = j.steps[i].new_container.clone();
        let started = match &new {
            Some(id) => match self.engine.inspect_container(id) {
                Ok(Some(c)) => c.running || c.status != "created",
                Err(crate::engine::EngineError::Crashed) => return Err(()),
                _ => true,
            },
            None => false,
        };
        if let Some(id) = &new {
            match self.engine.logs_tail(id, 40) {
                Ok(tail) if !tail.trim_end().is_empty() => {
                    output.push_str("\n--- last lines of the new control plane ---\n");
                    output.push_str(&tail_output(tail.trim_end(), LOG_TAIL_LIMIT));
                }
                Err(crate::engine::EngineError::Crashed) => return Err(()),
                _ => {}
            }
        }
        output.push_str(&moved_before(j, i));
        let point = database::load_point(self.dir.root()).ok().flatten();
        let point = point.filter(|p| p.request_id == j.request.request_id);
        output.push_str(if started {
            "\nThis release migrates the database, and its migration may have run, so the previous control plane was not put back: an older control plane never runs against a newer schema. The new one is left as it is."
        } else {
            "\nThis release migrates the database, so the previous control plane was not put back automatically. The new control plane never started, so its migration did not run."
        });
        match &point {
            Some(p) => match &p.dump {
                Some(dump) => output.push_str(&format!(
                    "\nTo go back to {}, run this on this machine. It stops the control plane, loads the dump {dump} taken before the update into Quasar's database, and starts {} again; anything written since the dump is lost:\n{}",
                    p.returns_to,
                    p.returns_to,
                    database::restore_command(Some(dump), &p.returns_to)
                )),
                None => output.push_str(&format!(
                    "\nQuasar holds no dump of an operator's own database. To go back to {}, restore the backup you confirmed with your own tools, then run this on this machine; it starts {} only if the database's schema matches it:\n{}",
                    p.returns_to,
                    p.returns_to,
                    database::restore_command(None, &p.returns_to)
                )),
            },
            None => output.push_str(
                "\nNo restore point was recorded for this attempt, so no restore command can be printed.",
            ),
        }
        // The log carries the command too: `docker logs quasar-recovery` is where an
        // operator looks when the console is down.
        let command = point
            .as_ref()
            .map(|p| database::restore_command(p.dump.as_deref(), &p.returns_to))
            .unwrap_or_default();
        warn!(
            token = "actor-migrating-control-plane-failed",
            request = %j.request.request_id,
            reason = %failure.reason,
            restore = %command,
            "a migrating control plane did not verify; it is not restored automatically"
        );
        self.finish(j, State::Failed, Some(failure.reason), output, false)
    }
}
