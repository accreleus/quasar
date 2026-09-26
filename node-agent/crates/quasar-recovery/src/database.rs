//! What the recovery actor does to a control plane's database (#352 decisions 14 and 20):
//! measure it, dump it, check a dump, load one, read a live schema. Each runs once, in a
//! disposable helper container from the machine's Postgres image (so `pg_dump` matches the
//! server), on the platform network, and is removed afterwards.
//!
//! Beside them, three machine-state files keep a control plane off a database it does not
//! match. None is a frozen interface.
//!
//! ```text
//! schema-floor.json   the database may be at this schema or above: no control plane
//!                     whose image declares less is created, started or put back
//! database-hold.json  no control plane is created or started: a fresh install awaits
//!                     its restore, or a restore stopped part-way
//! restore-point.json  what the last migrating control-plane replacement returns to
//! ```

use std::collections::BTreeMap;
use std::path::Path;
use std::time::Duration;

use quasar_runtime::DurableFile;
use serde::{Deserialize, Serialize};
use tracing::info;

use crate::actor::Actor;
use crate::engine::{ContainerSpec, EngineError, Image, RestartPolicy};
use crate::machine::Machine;
use crate::recipe::{
    self, control, labels, names, paths, secrets, Bind, DatabaseInputs, ImageRef, Role,
};

/// The helper's container name: one attempt at a time, so one helper at a time.
pub const HELPER: &str = "quasar-db-helper";

/// The image label naming the schema version a control-plane image migrates to
/// (`deploy/Dockerfile.control.prod`).
pub const IMAGE_SCHEMA: &str = "org.quasar.schema.version";

/// Where the helper sees the machine's `dumps/` directory.
const DUMPS_MOUNT: &str = "/dumps";
/// The helper's input: the dump file under [`DUMPS_MOUNT`].
pub const DUMP_FILE_ENV: &str = "QUASAR_DUMP_FILE";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DbOp {
    /// `pg_database_size`, in bytes.
    Size,
    /// `pg_dump --format=custom` into the dump file.
    Dump,
    /// The dump file is a readable archive; its `schema_migrations` row.
    Inspect,
    /// Drop and re-create the database, then `pg_restore` the dump file into it.
    Load,
    /// The live database's `schema_migrations` row.
    Schema,
}

impl DbOp {
    /// The helper label value, and how the in-memory engine tells what to simulate.
    pub fn label(self) -> &'static str {
        match self {
            DbOp::Size => "db-size",
            DbOp::Dump => "db-dump",
            DbOp::Inspect => "db-inspect",
            DbOp::Load => "db-load",
            DbOp::Schema => "db-schema",
        }
    }

    fn writes_dumps(self) -> bool {
        self == DbOp::Dump
    }

    fn reads_file(self) -> bool {
        matches!(self, DbOp::Dump | DbOp::Inspect | DbOp::Load)
    }

    /// The helper's `sh -c` script. Its inputs are the `PG*` variables, the password file
    /// in the secrets mount and `$QUASAR_DUMP_FILE`.
    pub fn script(self) -> String {
        let password = format!("{}/{}", paths::SECRETS_DIR, secrets::DATABASE_PASSWORD);
        let prelude = format!(
            "set -eu; set -o pipefail; PGPASSWORD=\"$(cat {password})\"; export PGPASSWORD; "
        );
        // Each prints `schema=<version> dirty=<t|f>` where it reads a schema: the one line
        // the actor parses (`parse_schema`). The file is always `$QUASAR_DUMP_FILE`.
        let body = match self {
            DbOp::Size => r#"psql -X -tA -c 'select pg_database_size(current_database())'"#,
            DbOp::Dump => {
                r#"pg_dump --format=custom --no-owner --no-privileges -f "$QUASAR_DUMP_FILE""#
            }
            DbOp::Inspect => {
                r#"pg_restore --list "$QUASAR_DUMP_FILE" >/dev/null; pg_restore --data-only --table=schema_migrations -f - "$QUASAR_DUMP_FILE" | awk '/^COPY /{c=1;next} /^\\\.$/{c=0} c && NF>=2 {print "schema="$1" dirty="$2}'"#
            }
            DbOp::Load => {
                r#"psql -X -v ON_ERROR_STOP=1 -d postgres -c "DROP DATABASE IF EXISTS \"$PGDATABASE\" WITH (FORCE)" -c "CREATE DATABASE \"$PGDATABASE\" OWNER \"$PGUSER\""; pg_restore --single-transaction --exit-on-error --no-owner --no-privileges -d "$PGDATABASE" "$QUASAR_DUMP_FILE""#
            }
            DbOp::Schema => {
                r#"psql -X -tA -F ' ' -c 'select version, dirty from schema_migrations' | awk 'NF>=2 {print "schema="$1" dirty="$2}'"#
            }
        };
        format!("{prelude}{body}")
    }
}

/// A `schema_migrations` row.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct Schema {
    pub version: i64,
    pub dirty: bool,
}

/// The last `schema=<n> dirty=<t|f>` line of a helper's output.
pub fn parse_schema(logs: &str) -> Option<Schema> {
    logs.lines().rev().find_map(|l| {
        let l = l.trim();
        let rest = l.strip_prefix("schema=")?;
        let (v, dirty) = rest.split_once(" dirty=")?;
        Some(Schema {
            version: v.trim().parse().ok()?,
            dirty: matches!(dirty.trim(), "t" | "true"),
        })
    })
}

/// The schema a control-plane image declares, when it declares one.
pub fn image_schema(image: &Image) -> Option<i64> {
    image.labels.get(IMAGE_SCHEMA)?.trim().parse().ok()
}

/// How a helper run ended short of an exit code.
#[derive(Debug)]
pub enum DbError {
    /// The process "died" (a test's injected crash).
    Crashed,
    Failed(String),
}

impl From<EngineError> for DbError {
    fn from(e: EngineError) -> Self {
        match e {
            EngineError::Crashed => DbError::Crashed,
            e => DbError::Failed(e.to_string()),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SchemaFloor {
    pub format: u32,
    pub schema_version: i64,
    pub request_id: String,
    pub set_at: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum HoldReason {
    /// A fresh install whose seed asked for a restore before the first boot.
    AwaitRestore,
    /// A restore stopped the control plane and has not finished loading and starting it.
    RestoreIncomplete,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Hold {
    pub format: u32,
    pub reason: HoldReason,
    pub since: String,
    #[serde(default)]
    pub request_id: Option<String>,
}

/// The control plane a failed migrating replacement returns to, and how.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RestorePoint {
    pub format: u32,
    pub request_id: String,
    pub returns_to: String,
    pub control_plane: ImageRef,
    pub recipe_revision: u32,
    /// Its schema, when known: the dump's for a Quasar-owned database, the image's label
    /// for an external one.
    #[serde(default)]
    pub schema_version: Option<i64>,
    /// The pre-update dump; `None` for an external database, which Quasar never dumps.
    #[serde(default)]
    pub dump: Option<String>,
    pub created_at: String,
}

fn floor_file(root: &Path) -> DurableFile<SchemaFloor> {
    DurableFile::new(root.join("schema-floor.json"), "json.tmp")
}

fn hold_file(root: &Path) -> DurableFile<Hold> {
    DurableFile::new(root.join("database-hold.json"), "json.tmp")
}

fn point_file(root: &Path) -> DurableFile<RestorePoint> {
    DurableFile::new(root.join("restore-point.json"), "json.tmp")
}

pub fn load_floor(root: &Path) -> std::io::Result<Option<SchemaFloor>> {
    floor_file(root).load()
}

pub fn load_hold(root: &Path) -> std::io::Result<Option<Hold>> {
    hold_file(root).load()
}

pub fn store_hold(root: &Path, hold: &Hold) -> std::io::Result<()> {
    hold_file(root).store(hold)
}

pub fn clear_hold(root: &Path) -> std::io::Result<()> {
    match std::fs::remove_file(hold_file(root).path()) {
        Err(e) if e.kind() != std::io::ErrorKind::NotFound => Err(e),
        _ => std::fs::File::open(root)?.sync_all(),
    }
}

pub fn load_point(root: &Path) -> std::io::Result<Option<RestorePoint>> {
    point_file(root).load()
}

pub fn store_point(root: &Path, point: &RestorePoint) -> std::io::Result<()> {
    point_file(root).store(point)
}

/// The value a `restore --to` names: the control plane's version, else its image's commit
/// (12 hex), else its digest's first 12 hex. Only `[0-9A-Za-z._+-]`, so it is safe in a
/// printed shell command.
pub fn returns_to(version: Option<&str>, image: Option<&Image>, image_ref: &ImageRef) -> String {
    let safe = |s: &str| {
        !s.is_empty()
            && s.len() <= 64
            && s.bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'+' | b'-'))
    };
    if let Some(v) = version.map(str::trim).filter(|v| safe(v)) {
        return v.to_owned();
    }
    if let Some(c) = image
        .and_then(|i| i.labels.get("org.quasar.source.commit"))
        .map(|c| c.trim())
        .filter(|c| c.len() >= 7 && c.bytes().all(|b| b.is_ascii_hexdigit()))
    {
        return c[..c.len().min(12)].to_owned();
    }
    let hex = image_ref
        .digest
        .strip_prefix("sha256:")
        .unwrap_or(&image_ref.digest);
    hex[..hex.len().min(12)].to_owned()
}

/// The operator's command, as a failure's output ends with it (control-api.md: "output
/// ends with the one-line restore command"). Run on the machine, inside the actor.
pub fn restore_command(dump: Option<&str>, returns_to: &str) -> String {
    let exec = format!(
        "docker exec {} quasar-recovery restore",
        names::RECOVERY_ACTOR
    );
    match dump {
        Some(d) => format!("{exec} --dump {d} --to {returns_to}"),
        None => format!("{exec} --to {returns_to}"),
    }
}

impl Actor {
    /// The daemon-host path of this actor's machine-state directory, which the dump
    /// helper binds (`dumps/` only). Needs the engine's `local` volume driver, like the
    /// socket directories a control-plane machine binds.
    pub(crate) fn machine_host_dir(&self) -> Result<String, DbError> {
        if let Some(path) = &self.config.machine_dir_host {
            return Ok(path.clone());
        }
        let failed = |why: String| DbError::Failed(why);
        let engine = |what: &str, e: EngineError| match e {
            EngineError::Crashed => DbError::Crashed,
            e => DbError::Failed(format!("{what}: {e}")),
        };
        let me = self
            .config
            .self_container
            .as_deref()
            .ok_or_else(|| failed("this recovery actor cannot tell its own container, so it cannot tell where its machine state lives on the host".into()))?;
        let container = self
            .engine
            .inspect_container(me)
            .map_err(|e| engine("inspect this actor's container", e))?
            .ok_or_else(|| failed("this actor's own container is gone".into()))?;
        let (source, _, _) = container
            .mounts
            .iter()
            .find(|(_, target, _)| target == paths::MACHINE_DIR)
            .ok_or_else(|| failed(format!("nothing is mounted at {}", paths::MACHINE_DIR)))?;
        if source.starts_with('/') {
            return Ok(source.clone());
        }
        match self.engine.inspect_volume(source) {
            Ok(Some(v)) if v.driver == "local" => v
                .mountpoint
                .ok_or_else(|| failed(format!("the {source} volume reports no host path"))),
            Ok(Some(v)) => Err(failed(format!(
                "the {source} volume uses the {:?} driver; a pre-update dump needs the engine's local volume driver",
                v.driver
            ))),
            Ok(None) => Err(failed(format!("the {source} volume does not exist"))),
            Err(e) => Err(engine(&format!("inspect the {source} volume"), e)),
        }
    }

    /// The image a helper runs: the machine's own Postgres for a Quasar-owned database
    /// (its `pg_dump` matches the server), the default one for an external database (only
    /// ever asked for its schema).
    fn helper_image(&self, machine: &Machine) -> Result<ImageRef, String> {
        let owned = matches!(
            machine.inputs.control.as_ref().map(|c| &c.database),
            Some(DatabaseInputs::Owned)
        );
        if owned {
            if let Ok(Some(record)) = self.dir.load_service(Role::Postgres) {
                return Ok(record.image);
            }
            if let Some(image) = machine.install_images.get(&Role::Postgres) {
                return Ok(image.clone());
            }
        }
        ImageRef::parse(control::DEFAULT_POSTGRES_IMAGE).map_err(|e| e.to_string())
    }

    fn helper_spec(
        &self,
        machine: &Machine,
        op: DbOp,
        file: Option<&str>,
    ) -> Result<ContainerSpec, DbError> {
        let control = machine.inputs.control.as_ref().ok_or_else(|| {
            DbError::Failed("this machine runs no control plane, so it has no database".into())
        })?;
        let (host, port, user, name, sslmode, volume) = match &control.database {
            DatabaseInputs::Owned => (
                names::POSTGRES.to_string(),
                5432,
                control::OWNED_DATABASE_USER.to_string(),
                control::OWNED_DATABASE.to_string(),
                "disable".to_string(),
                names::POSTGRES_SECRETS_VOLUME,
            ),
            DatabaseInputs::External {
                host,
                port,
                user,
                name,
                sslmode,
                ..
            } => (
                host.clone(),
                *port,
                user.clone(),
                name.clone(),
                sslmode.clone(),
                names::CONTROL_PLANE_SECRETS_VOLUME,
            ),
        };
        let image = self.helper_image(machine).map_err(DbError::Failed)?;
        let mut env = BTreeMap::from([
            ("PGHOST".to_string(), host),
            ("PGPORT".into(), port.to_string()),
            ("PGUSER".into(), user),
            ("PGDATABASE".into(), name),
            ("PGSSLMODE".into(), sslmode),
        ]);
        let mut binds = vec![Bind {
            source: volume.into(),
            target: paths::SECRETS_DIR.into(),
            read_only: true,
        }];
        if op.reads_file() {
            let file = file.ok_or_else(|| DbError::Failed("no dump file named".into()))?;
            let host_dir = self.machine_host_dir()?;
            binds.push(Bind {
                source: format!("{}/dumps", host_dir.trim_end_matches('/')),
                target: DUMPS_MOUNT.into(),
                read_only: !op.writes_dumps(),
            });
            env.insert(DUMP_FILE_ENV.into(), format!("{DUMPS_MOUNT}/{file}"));
        }
        Ok(ContainerSpec {
            name: HELPER.into(),
            image: image.reference(),
            entrypoint: Some(vec!["sh".into(), "-c".into(), op.script()]),
            cmd: None,
            env,
            labels: BTreeMap::from([(labels::HELPER.to_string(), op.label().to_string())]),
            network_mode: Some(names::PLATFORM_NETWORK.into()),
            binds,
            devices: Vec::new(),
            device_cgroup_rules: Vec::new(),
            gpus: Vec::new(),
            cap_add: Vec::new(),
            security_opt: Vec::new(),
            init: false,
            restart: RestartPolicy::No,
            ports: Vec::new(),
            healthcheck: None,
        })
    }

    /// Runs `op` to completion: its exit code and the tail of its output. `file` is a file
    /// name in `dumps/`. A helper a crash left behind is removed first.
    pub(crate) fn run_db(
        &self,
        machine: &Machine,
        op: DbOp,
        file: Option<&str>,
    ) -> Result<(i64, String), DbError> {
        let spec = self.helper_spec(machine, op, file)?;
        if let Some(stale) = self.retrying(|| self.engine.inspect_container(HELPER))? {
            if !stale.labels.contains_key(labels::HELPER) {
                return Err(DbError::Failed(format!(
                    "a container this actor did not create holds the name {HELPER}; remove it"
                )));
            }
            self.retrying(|| self.engine.remove_container(&stale.id))?;
        }
        let image = ImageRef::parse(&spec.image).map_err(|e| DbError::Failed(e.to_string()))?;
        self.ensure_image(&image).map_err(|e| match e {
            crate::actor::ResumeError::Engine(EngineError::Crashed) => DbError::Crashed,
            e => DbError::Failed(format!("the database helper's image: {e}")),
        })?;
        let id = self.retrying(|| self.engine.create_container(&spec))?;
        let ran = (|| -> Result<(i64, String), DbError> {
            self.retrying(|| self.engine.start_container(&id))?;
            let code = self
                .engine
                .wait_container(&id, self.config.database_timeout)?;
            let logs = match self.engine.logs_tail(&id, 200) {
                Err(EngineError::Crashed) => return Err(DbError::Crashed),
                other => other.unwrap_or_default(),
            };
            Ok((code, logs))
        })();
        // A process that died does nothing more: the next start removes the helper.
        if matches!(ran, Err(DbError::Crashed)) {
            return Err(DbError::Crashed);
        }
        match self.retrying(|| self.engine.remove_container(&id)) {
            Err(EngineError::Crashed) => return Err(DbError::Crashed),
            Err(e) => tracing::warn!(
                token = "actor-db-helper-not-removed",
                "the database helper could not be removed ({e}); the next start removes it"
            ),
            Ok(()) => {}
        }
        let (code, logs) = ran?;
        info!(op = op.label(), code, "database helper finished");
        Ok((code, logs))
    }

    /// Whether this machine may run a control plane of `image`: its declared schema is not
    /// below the schema floor. An image that declares none is allowed only with no floor.
    pub(crate) fn schema_allows(&self, image: &Image, reference: &str) -> Result<(), String> {
        let floor = load_floor(self.dir.root())
            .map_err(|e| format!("schema-floor.json cannot be read ({e})"))?;
        let Some(floor) = floor else {
            return Ok(());
        };
        match image_schema(image) {
            Some(v) if v >= floor.schema_version => Ok(()),
            Some(v) => Err(format!(
                "{reference} is a control plane of schema {v}, and this machine's database may be at schema {} (since attempt {}); an older control plane never runs against a newer schema. Restore a dump taken before that update to go back",
                floor.schema_version, floor.request_id
            )),
            None => Err(format!(
                "{reference} declares no {IMAGE_SCHEMA} label, and this machine's database may be at schema {}; it is not started",
                floor.schema_version
            )),
        }
    }

    /// Raises the floor to `schema` before a control plane that may migrate to it starts.
    pub(crate) fn raise_floor(&self, schema: i64, request_id: &str) -> std::io::Result<()> {
        let root = self.dir.root();
        if load_floor(root)?.is_some_and(|f| f.schema_version >= schema) {
            return Ok(());
        }
        self.set_floor(schema, request_id)
    }

    /// The database is now exactly at `schema`: a restore loaded it.
    pub(crate) fn set_floor(&self, schema: i64, request_id: &str) -> std::io::Result<()> {
        floor_file(self.dir.root()).store(&SchemaFloor {
            format: 1,
            schema_version: schema,
            request_id: request_id.into(),
            set_at: self.now(),
        })
    }

    /// Reads an image's labels: pulled if needed.
    pub(crate) fn image_labels(&self, image: &ImageRef) -> Result<Image, DbError> {
        self.ensure_image(image).map_err(|e| match e {
            crate::actor::ResumeError::Engine(EngineError::Crashed) => DbError::Crashed,
            e => DbError::Failed(format!("{}: {e}", image.reference())),
        })
    }

    /// The schema an image present on this machine declares; `None` when it declares none
    /// or is not here.
    pub(crate) fn local_image_schema(&self, image: &ImageRef) -> Result<Option<i64>, EngineError> {
        Ok(self
            .engine
            .inspect_image(&image.reference())?
            .and_then(|i| image_schema(&i)))
    }

    pub(crate) fn is_owned_database(machine: &Machine) -> bool {
        matches!(
            machine.inputs.control.as_ref().map(|c| &c.database),
            Some(DatabaseInputs::Owned)
        )
    }

    /// The last verified control plane of this machine: image and recipe revision.
    pub(crate) fn recorded_control_plane(
        &self,
        machine: &Machine,
    ) -> Result<Option<(ImageRef, u32)>, EngineError> {
        if let Ok(Some(record)) = self.dir.load_service(Role::ControlPlane) {
            return Ok(Some((record.image, record.recipe_revision)));
        }
        let Some(image) = machine.install_images.get(&Role::ControlPlane).cloned() else {
            return Ok(None);
        };
        let rev = self
            .engine
            .inspect_image(&image.reference())?
            .and_then(|found| found.labels.get(labels::IMAGE_RECIPE)?.trim().parse().ok());
        Ok(rev.map(|rev| (image, rev)))
    }
}

/// The recipe revision a control-plane image declares, if this actor renders it.
pub(crate) fn control_plane_revision(image: &Image) -> Option<u32> {
    let rev: u32 = image
        .labels
        .get(labels::IMAGE_RECIPE)?
        .trim()
        .parse()
        .ok()?;
    recipe::Book::supports(Role::ControlPlane, rev).then_some(rev)
}

/// A bounded default for one database operation: a dump or a load of a large database.
pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(2 * 3600);

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_schema_line_is_read_from_the_end_of_the_output() {
        assert_eq!(
            parse_schema("notice\nschema=88 dirty=f\n"),
            Some(Schema {
                version: 88,
                dirty: false
            })
        );
        assert_eq!(
            parse_schema("schema=91 dirty=t"),
            Some(Schema {
                version: 91,
                dirty: true
            })
        );
        assert_eq!(parse_schema("pg_restore: error: no such table"), None);
    }

    #[test]
    fn the_restore_command_names_the_dump_and_the_version() {
        assert_eq!(
            restore_command(Some("20260925T140200Z-schema-88"), "0.5.2"),
            "docker exec quasar-recovery quasar-recovery restore --dump 20260925T140200Z-schema-88 --to 0.5.2"
        );
        assert_eq!(
            restore_command(None, "0.5.2"),
            "docker exec quasar-recovery quasar-recovery restore --to 0.5.2"
        );
        let img = ImageRef {
            repository: "r".into(),
            digest: format!("sha256:{}", "ab".repeat(32)),
        };
        assert_eq!(returns_to(Some("0.5.2"), None, &img), "0.5.2");
        assert_eq!(returns_to(Some("x; rm -rf /"), None, &img), "abababababab");
    }
}
