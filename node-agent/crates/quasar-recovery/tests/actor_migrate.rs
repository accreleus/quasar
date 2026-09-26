//! A migrating control-plane update and the operator's `restore` (#364), against the
//! in-memory engine (whose database helpers act on a simulated database and write real
//! dump files) and a temporary machine-state directory. Observed through the actor's
//! submit, status and resume, engine state, the simulated database and `dumps/`.

mod support;

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use quasar_recovery::actor::{Actor, ActorConfig, OperatorInputs, ReplaceTiming, TrustConfig};
use quasar_recovery::engine::{
    dump_bytes, Behaviour, EngineError, FakeDatabase, FakeEngine, FakeState, Fault, Image,
    RestartPolicy, When,
};
use quasar_recovery::journal::Phase;
use quasar_recovery::recipe::names;
use quasar_recovery::restore::{self, RestorePhase};
use quasar_recovery::seed::Outcome;
use quasar_recovery::socket::{
    AttemptResult, Component, DatabaseMode, MachineRole, Reason, Release, Request, RequestKind,
    State,
};
use quasar_recovery::trust::{Caller, SignaturePolicy};
use support::*;

const REPO: &str = "registry.example.invalid/quasar/quasar-control-plane";
const POSTGRES_IMAGE: &str = "docker.io/library/postgres@sha256:dd55000000000000000000000000000000000000000000000000000000000000";
const ID: &str = "7a1f6f1e-2c33-4a58-9a5e-0b6b0f7a1c22";
const COMMIT: &str = "cccccccccccccccccccccccccccccccccccccccc";
const KEPT: &str = "quasar-control-plane.kept";
/// The installed control plane's schema.
const OLD_SCHEMA: i64 = 80;

fn digest(schema: i64) -> String {
    format!("sha256:{:064x}", 0xc0de_0000_u64 + schema as u64)
}

/// The control-plane image that migrates the database to `schema`.
fn control_image(schema: i64) -> String {
    format!("{REPO}@{}", digest(schema))
}

fn nth_id(n: u64) -> String {
    format!("{:08x}-2c33-4a58-9a5e-0b6b0f7a1c22", n)
}

fn image(reference: &str, labels: &[(&str, &str)]) -> Image {
    Image {
        id: format!(
            "sha256:{:064x}",
            reference.len() as u64 * 7919 + labels.len() as u64
        ),
        repo_digests: vec![reference.into()],
        labels: labels
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect(),
    }
}

/// A clean machine with only a combined (or control-only) host's seed on it, control-plane
/// images of schema 80 to 85 in the registry, and a database the first control plane
/// migrates to schema 80.
fn control_host(env: BTreeMap<String, String>, healthy: bool) -> FakeState {
    let mut state = seeded_host(env);
    state
        .registry
        .insert(AGENT_IMAGE.into(), agent_image(Some("2")));
    for schema in OLD_SCHEMA..=85 {
        let reference = control_image(schema);
        let s = schema.to_string();
        let mut img = image(
            &reference,
            &[
                ("org.quasar.recipe", "1"),
                ("org.quasar.schema.version", &s),
                ("org.quasar.source.commit", COMMIT),
            ],
        );
        img.id = format!("sha256:{:064x}", 0x1d00 + schema as u64);
        state.registry.insert(reference.clone(), img);
        let health = if schema == OLD_SCHEMA || healthy {
            "healthy"
        } else {
            "starting"
        };
        state.behaviour.insert(
            reference,
            Behaviour {
                health: Some(health.into()),
                logs: format!("control plane at schema {schema}\n"),
                ..Default::default()
            },
        );
    }
    state
        .registry
        .insert(POSTGRES_IMAGE.into(), image(POSTGRES_IMAGE, &[]));
    // What an external database's schema is read with.
    let default_postgres = quasar_recovery::recipe::control::DEFAULT_POSTGRES_IMAGE;
    state
        .registry
        .insert(default_postgres.into(), image(default_postgres, &[]));
    state.behaviour.insert(
        POSTGRES_IMAGE.into(),
        Behaviour {
            health: Some("healthy".into()),
            ..Default::default()
        },
    );
    state.database = Some(FakeDatabase {
        schema_version: 0,
        dirty: false,
        rows: "empty".into(),
        size_bytes: 1_400_000_000,
    });
    state
}

fn combined_env() -> BTreeMap<String, String> {
    BTreeMap::from([
        ("QUASAR_ROLE".to_string(), "combined".to_string()),
        ("QUASAR_HOME_ROOT".into(), HOME.into()),
        ("QUASAR_AGENT_IMAGE".into(), AGENT_IMAGE.into()),
        (
            "QUASAR_CONTROL_PLANE_IMAGE".into(),
            control_image(OLD_SCHEMA),
        ),
        ("QUASAR_POSTGRES_IMAGE".into(), POSTGRES_IMAGE.into()),
    ])
}

fn external_env() -> BTreeMap<String, String> {
    BTreeMap::from([
        ("QUASAR_ROLE".to_string(), "control-only".to_string()),
        (
            "QUASAR_CONTROL_PLANE_IMAGE".into(),
            control_image(OLD_SCHEMA),
        ),
        ("QUASAR_DATABASE_HOST".into(), "db.example.invalid".into()),
        (
            "QUASAR_DATABASE_PASSWORD".into(),
            "operators-password".into(),
        ),
    ])
}

/// What a test tunes on the actor processes of one machine.
#[derive(Clone)]
struct Knobs {
    free: Arc<AtomicU64>,
    clock: Arc<AtomicU64>,
    sockets: std::path::PathBuf,
}

impl Knobs {
    fn new(sockets: &std::path::Path) -> Knobs {
        Knobs {
            free: Arc::new(AtomicU64::new(212_000_000_000)),
            clock: Arc::new(AtomicU64::new(0)),
            sockets: sockets.to_path_buf(),
        }
    }
}

struct Machine {
    engine: Arc<FakeEngine>,
    dir: tempfile::TempDir,
    _sockets: tempfile::TempDir,
    actor_id: String,
    knobs: Knobs,
}

impl Machine {
    /// Installed the documented way: the seed, then its actor's first `resume`.
    fn install(env: BTreeMap<String, String>, healthy: bool) -> Machine {
        let engine = Arc::new(FakeEngine::new(control_host(env, healthy)));
        let dir = tempfile::tempdir().unwrap();
        let sockets = tempfile::tempdir().unwrap();
        let created = seed(&engine, dir.path(), SEED_ID).step();
        assert!(matches!(created, Outcome::Created { .. }), "{created:?}");
        let actor_id = engine
            .state()
            .container_named(names::RECOVERY_ACTOR)
            .unwrap()
            .id
            .clone();
        let knobs = Knobs::new(&sockets.path().join("run"));
        let m = Machine {
            engine,
            dir,
            _sockets: sockets,
            actor_id,
            knobs,
        };
        m.actor().resume().expect("the install");
        m
    }

    fn config(&self) -> ActorConfig {
        let mut config =
            ActorConfig::new(self.dir.path(), MachineRole::Gpu, OperatorInputs::default());
        config.self_container = Some(self.actor_id.clone());
        config.seed_container = Some(SEED_ID.into());
        let clock = self.knobs.clock.clone();
        config.now = Box::new(move || {
            let n = clock.fetch_add(1, Ordering::SeqCst);
            format!(
                "2026-09-25T{:02}:{:02}:{:02}Z",
                10 + n / 3600,
                (n / 60) % 60,
                n % 60
            )
        });
        config.gpus_probe_backoff = Duration::ZERO;
        config.healthy_wait = Duration::from_millis(20);
        config.timing = ReplaceTiming {
            verify_timeout: Duration::from_millis(150),
            poll: Duration::from_millis(1),
            stop_grace: Duration::from_secs(1),
            retries: 2,
            retry_backoff: Duration::ZERO,
        };
        config.trust = TrustConfig {
            allowed_namespaces: vec!["registry.example.invalid/quasar".into()],
            signature: SignaturePolicy::default(),
        };
        config.machine_dir_host = Some(self.dir.path().display().to_string());
        let free = self.knobs.free.clone();
        config.free_space = Box::new(move |_| Ok(free.load(Ordering::SeqCst)));
        config.socket_dir = self.knobs.sockets.clone();
        config
    }

    /// A new actor process on this machine.
    fn actor(&self) -> Arc<Actor> {
        Arc::new(Actor::new(self.engine.clone(), self.config()))
    }

    fn actor_with(&self, adjust: impl FnOnce(&mut ActorConfig)) -> Arc<Actor> {
        let mut config = self.config();
        adjust(&mut config);
        Arc::new(Actor::new(self.engine.clone(), config))
    }

    fn db(&self) -> FakeDatabase {
        self.engine.state().database.expect("a simulated database")
    }

    fn control_plane(&self) -> quasar_recovery::engine::FakeContainer {
        self.engine
            .state()
            .container_named(names::CONTROL_PLANE)
            .cloned()
            .expect("a control plane")
    }

    fn kept(&self) -> Option<quasar_recovery::engine::FakeContainer> {
        self.engine.state().container_named(KEPT).cloned()
    }

    fn dump_files(&self) -> Vec<String> {
        let mut names: Vec<String> = std::fs::read_dir(self.dir.path().join("dumps"))
            .map(|entries| {
                entries
                    .flatten()
                    .map(|e| e.file_name().to_string_lossy().into_owned())
                    .collect()
            })
            .unwrap_or_default();
        names.sort();
        names
    }

    fn never_an_older_control_plane(&self, at: &str) {
        assert_eq!(
            self.engine.state().older_control_planes_started,
            Vec::<String>::new(),
            "{at}: an older control plane was started against a newer schema"
        );
    }
}

fn update(id: &str, schema: i64) -> Request {
    Request {
        request_id: id.into(),
        kind: RequestKind::Replace,
        components: vec![Component {
            name: "control-plane".into(),
            image: REPO.into(),
            digest: digest(schema),
        }],
        release: Release {
            id: format!("rel-0.{schema}.0"),
            version: Some(format!("0.{schema}.0")),
            source_commit: COMMIT.into(),
        },
        migrates: true,
        schema_version: Some(schema),
        external_backup_confirmed: false,
        dump: None,
        purge: false,
        wait_timeout_s: 0,
        from_version: Some(format!("0.{}.0", schema - 1)),
        force_again: false,
    }
}

fn apply(actor: &Arc<Actor>, req: Request) -> AttemptResult {
    let id = req.request_id.clone();
    actor
        .submit(Caller::ControlPlane, req)
        .unwrap_or_else(|r| panic!("refused: {r:?}"));
    actor.wait_attempt();
    actor.status_for(Some(&id)).result.expect("a result")
}

fn restore_request(id: &str, dump: Option<&str>, to: Option<&str>) -> Request {
    restore::request(id.into(), dump.map(Into::into), to.map(Into::into))
}

fn run_restore(actor: &Arc<Actor>, req: Request) -> AttemptResult {
    let id = req.request_id.clone();
    actor
        .submit_restore(req)
        .unwrap_or_else(|r| panic!("refused: {r:?}"));
    actor.wait_attempt();
    actor.status_operator(Some(&id)).result.expect("a result")
}

/// The `format` a journal is stored in, as an older actor reads it.
fn journal_format(m: &Machine, id: &str) -> u64 {
    let raw = std::fs::read(m.dir.path().join("journal").join(format!("{id}.json"))).unwrap();
    let v: serde_json::Value = serde_json::from_slice(&raw).unwrap();
    v["format"].as_u64().unwrap()
}

fn last_line(output: &str) -> &str {
    output
        .lines()
        .rev()
        .find(|l| !l.trim().is_empty())
        .unwrap_or("")
}

#[test]
fn a_migrating_update_dumps_the_database_first_and_records_the_dump() {
    let m = Machine::install(combined_env(), true);
    assert_eq!(m.db().schema_version, OLD_SCHEMA);
    let actor = m.actor();
    let result = apply(&actor, update(ID, 81));

    assert_eq!(result.state, State::Succeeded, "{result:?}");
    let dump = result.dump.clone().expect("the result names its dump");
    assert!(dump.ends_with("-schema-80"), "{dump}");
    // The dump holds the database as it was under the old control plane.
    let archived = std::fs::read(m.dir.path().join("dumps").join(format!("{dump}.dump"))).unwrap();
    assert_eq!(
        archived,
        dump_bytes(&FakeDatabase {
            schema_version: OLD_SCHEMA,
            dirty: false,
            rows: "empty".into(),
            size_bytes: 1_400_000_000,
        })
    );
    assert_eq!(m.db().schema_version, 81);
    assert_eq!(m.control_plane().spec.image, control_image(81));
    assert!(m.kept().is_none());
    let status = actor.status();
    assert_eq!(status.database, DatabaseMode::Owned);
    assert_eq!(status.dumps.len(), 1);
    assert_eq!(status.dumps[0].name, dump);
    assert_eq!(status.dumps[0].schema_version, OLD_SCHEMA);
    assert_eq!(status.dump_free_bytes, Some(212_000_000_000));
    // No helper is left behind.
    assert!(m
        .engine
        .state()
        .container_named("quasar-db-helper")
        .is_none());
    m.never_an_older_control_plane("after the update");
}

#[test]
fn a_dump_that_fails_or_does_not_fit_refuses_the_update_and_changes_nothing() {
    for case in [
        "dump fails",
        "no space",
        "unreadable dump",
        "dirty database",
    ] {
        let m = Machine::install(combined_env(), true);
        let before = m.control_plane();
        match case {
            "dump fails" => m.engine.with_state(|s| {
                s.db_failures.insert(
                    "db-dump".into(),
                    (
                        1,
                        "pg_dump: error: could not write: No space left on device\n".into(),
                    ),
                );
            }),
            "no space" => m.knobs.free.store(600_000_000, Ordering::SeqCst),
            "unreadable dump" => m.engine.with_state(|s| {
                s.db_failures.insert(
                    "db-inspect".into(),
                    (1, "pg_restore: error: could not read input file\n".into()),
                );
            }),
            _ => m
                .engine
                .with_state(|s| s.database.as_mut().unwrap().dirty = true),
        }
        let result = apply(&m.actor(), update(ID, 81));
        assert_eq!(result.state, State::Failed, "{case}: {result:?}");
        assert_eq!(result.reason, Some(Reason::BackupFailed), "{case}");
        assert!(!result.restored, "{case}");
        assert_eq!(result.dump, None, "{case}");
        assert!(
            result.output.contains("the control plane was not replaced"),
            "{case}: {}",
            result.output
        );
        if case == "no space" {
            assert!(
                result
                    .output
                    .contains("it needs about 1.5 GB and 600 MB is free"),
                "{}",
                result.output
            );
        }
        // The old control plane was never touched, and no dump or partial is left.
        let now = m.control_plane();
        assert_eq!(now.id, before.id, "{case}");
        assert_eq!(now.status, "running", "{case}");
        assert_eq!(now.restart, RestartPolicy::UnlessStopped, "{case}");
        assert!(m.kept().is_none(), "{case}");
        assert_eq!(m.dump_files(), Vec::<String>::new(), "{case}");
        assert_eq!(m.db().schema_version, OLD_SCHEMA, "{case}");
    }
}

#[test]
fn the_last_three_dumps_are_kept_with_their_schema_versions() {
    let m = Machine::install(combined_env(), true);
    let actor = m.actor();
    for (n, schema) in (81..=84).enumerate() {
        let result = apply(&actor, update(&nth_id(n as u64 + 1), schema));
        assert_eq!(result.state, State::Succeeded, "{schema}: {result:?}");
    }
    let dumps = actor.status().dumps;
    let schemas: Vec<i64> = dumps.iter().map(|d| d.schema_version).collect();
    assert_eq!(schemas, vec![83, 82, 81], "newest first, the oldest gone");
    assert_eq!(m.dump_files().len(), 6, "three dumps and their records");
}

#[test]
fn an_external_database_needs_the_confirmed_backup_and_is_never_dumped() {
    let m = Machine::install(external_env(), true);
    let actor = m.actor();
    let refused = actor
        .submit(Caller::ControlPlane, update(ID, 81))
        .unwrap_err();
    assert_eq!(refused.reason, Reason::BackupUnconfirmed, "{refused:?}");
    assert_eq!(
        actor.status_for(Some(ID)).result,
        None,
        "nothing journalled"
    );

    // A request that says it does not migrate, for an image whose schema label says it
    // does, is caught before anything stops.
    let mut unflagged = update(&nth_id(2), 81);
    unflagged.migrates = false;
    let result = apply(&actor, unflagged);
    assert_eq!(result.reason, Some(Reason::BackupUnconfirmed), "{result:?}");
    assert_eq!(m.control_plane().spec.image, control_image(OLD_SCHEMA));
    assert_eq!(m.control_plane().status, "running");

    let mut confirmed = update(&nth_id(3), 81);
    confirmed.external_backup_confirmed = true;
    let result = apply(&actor, confirmed);
    assert_eq!(result.state, State::Succeeded, "{result:?}");
    assert_eq!(result.dump, None);
    assert_eq!(m.dump_files(), Vec::<String>::new());
    assert_eq!(actor.status().database, DatabaseMode::External);
    assert_eq!(actor.status().dump_free_bytes, None);
}

#[test]
fn a_failed_migrating_update_is_not_restored_and_prints_the_restore_command() {
    let m = Machine::install(combined_env(), false);
    let old = m.control_plane();
    let result = apply(&m.actor(), update(ID, 81));

    assert_eq!(result.state, State::Failed, "{result:?}");
    assert_eq!(result.reason, Some(Reason::Unhealthy));
    assert!(!result.restored);
    let dump = result.dump.clone().expect("the dump is named");
    assert_eq!(
        last_line(&result.output),
        format!("docker exec quasar-recovery quasar-recovery restore --dump {dump} --to 0.80.0")
    );
    // The old control plane is kept, stopped and disabled; the new one is left as it is.
    let kept = m.kept().expect("the old control plane is kept");
    assert_eq!(kept.id, old.id);
    assert_eq!(kept.status, "exited");
    assert_eq!(kept.restart, RestartPolicy::No);
    assert_eq!(m.control_plane().spec.image, control_image(81));
    assert_eq!(m.control_plane().status, "running");
    assert_eq!(m.db().schema_version, 81, "its migration ran");

    // A restart, a daemon restart, or the new container vanishing never brings the older
    // control plane back against the newer schema.
    m.engine.restart_daemon();
    m.actor().resume().unwrap();
    m.never_an_older_control_plane("after a restart");
    m.engine.with_state(|s| {
        let id = s.container_named(names::CONTROL_PLANE).unwrap().id.clone();
        s.containers.remove(&id);
    });
    let refused = m.actor().resume().unwrap_err();
    assert!(
        refused
            .to_string()
            .contains("an older control plane never runs against a newer schema"),
        "{refused}"
    );
    assert!(m
        .engine
        .state()
        .container_named(names::CONTROL_PLANE)
        .is_none());
    m.never_an_older_control_plane("after the new container was removed");
}

#[test]
fn restore_loads_the_dump_and_starts_the_control_plane_it_was_taken_under() {
    let m = Machine::install(combined_env(), false);
    let failed = apply(&m.actor(), update(ID, 81));
    let dump = failed.dump.clone().unwrap();

    let actor = m.actor();
    let result = run_restore(
        &actor,
        restore_request(&nth_id(9), Some(&dump), Some("0.80.0")),
    );
    assert_eq!(result.state, State::Succeeded, "{result:?}");
    assert_eq!(m.db().schema_version, OLD_SCHEMA);
    assert_eq!(m.db().rows, "empty");
    let cp = m.control_plane();
    assert_eq!(cp.spec.image, control_image(OLD_SCHEMA));
    assert_eq!(cp.status, "running");
    assert_eq!(cp.restart, RestartPolicy::UnlessStopped);
    assert!(
        m.kept().is_none(),
        "the kept and the failed control planes are gone"
    );
    let cps = m
        .engine
        .state()
        .containers
        .values()
        .filter(|c| {
            c.spec
                .labels
                .get("io.quasar.platform-service")
                .map(String::as_str)
                == Some("control-plane")
        })
        .count();
    assert_eq!(cps, 1);
    m.never_an_older_control_plane("after the restore");
    assert!(!m.dir.path().join("database-hold.json").exists());

    // The machine is whole again: a further start changes nothing, and the next
    // migrating update from the restored control plane works.
    let settled = m.engine.state().by_name();
    m.actor().resume().unwrap();
    assert_eq!(m.engine.state().by_name(), settled);
}

#[test]
fn a_corrupt_or_mismatched_dump_is_refused_before_the_database_is_touched() {
    let m = Machine::install(combined_env(), false);
    let failed = apply(&m.actor(), update(ID, 81));
    let dump = failed.dump.clone().unwrap();
    let actor = m.actor();

    let refused = actor
        .submit_restore(restore_request(&nth_id(2), Some(&dump), Some("0.79.0")))
        .unwrap_err();
    assert_eq!(refused.reason, Reason::Invalid);
    assert!(
        refused.message.contains("returns to 0.80.0"),
        "{}",
        refused.message
    );
    let refused = actor
        .submit_restore(restore_request(&nth_id(3), Some("../../secrets/x"), None))
        .unwrap_err();
    assert_eq!(refused.reason, Reason::Invalid, "{refused:?}");

    let file = m.dir.path().join("dumps").join(format!("{dump}.dump"));
    let mut bytes = std::fs::read(&file).unwrap();
    bytes.extend_from_slice(b"garbage");
    std::fs::write(&file, &bytes).unwrap();
    let before = m.engine.state();
    let result = run_restore(&actor, restore_request(&nth_id(4), Some(&dump), None));
    assert_eq!(result.state, State::Failed);
    assert!(result.output.contains("corrupt"), "{}", result.output);
    assert!(
        result.output.contains("Nothing was changed"),
        "{}",
        result.output
    );
    assert_eq!(m.engine.state().by_name(), before.by_name());
    assert_eq!(m.db(), before.database.unwrap());
}

#[test]
fn an_interrupted_restore_finishes_on_the_next_start_and_can_be_run_again() {
    let reference = Machine::install(combined_env(), false);
    let dump = apply(&reference.actor(), update(ID, 81)).dump.unwrap();
    let start = reference.engine.calls();
    run_restore(
        &reference.actor(),
        restore_request(&nth_id(9), Some(&dump), Some("0.80.0")),
    );
    let total = reference.engine.calls() - start;
    assert!(total > 10, "the sweep must not be vacuous ({total} calls)");

    let mut interrupted = 0;
    let mut continued = 0;
    // The failed update before it polls its verification on a clock, so each crash point
    // is counted from the start of the restore.
    for offset in 0..total {
        let m = Machine::install(combined_env(), false);
        let dump = apply(&m.actor(), update(ID, 81)).dump.unwrap();
        let at = format!("call {offset}");
        m.engine.inject(Fault {
            call: m.engine.calls() + offset,
            when: When::After,
            error: EngineError::Crashed,
        });
        let actor = m.actor();
        let id = nth_id(9);
        let submitted = actor.submit_restore(restore_request(&id, Some(&dump), Some("0.80.0")));
        actor.wait_attempt();
        m.engine.clear_faults();
        if submitted.is_err() {
            assert_eq!(m.db().schema_version, 81, "{at}");
            continue;
        }
        let before = actor.status_operator(Some(&id)).result.unwrap();
        drop(actor);
        m.actor()
            .resume()
            .unwrap_or_else(|e| panic!("{at}: the next start failed: {e}"));
        let result = m.actor().status_operator(Some(&id)).result.unwrap();
        assert!(result.state.is_terminal(), "{at}: {result:?}");
        m.never_an_older_control_plane(&at);
        if before.state == State::Pulling || before.state == State::Pending {
            interrupted += 1;
            assert_eq!(result.reason, Some(Reason::Interrupted), "{at}: {result:?}");
            assert_eq!(m.db().schema_version, 81, "{at}: nothing changed");
            // Running the command again restores.
            let again = run_restore(
                &m.actor(),
                restore_request(&nth_id(10), Some(&dump), Some("0.80.0")),
            );
            assert_eq!(again.state, State::Succeeded, "{at}: {again:?}");
        } else {
            continued += 1;
            assert_eq!(result.state, State::Succeeded, "{at}: {result:?}");
        }
        assert_eq!(m.db().schema_version, OLD_SCHEMA, "{at}");
        assert_eq!(
            m.control_plane().spec.image,
            control_image(OLD_SCHEMA),
            "{at}"
        );
        assert_eq!(m.control_plane().status, "running", "{at}");
        assert!(m.kept().is_none(), "{at}");
    }
    assert!(
        interrupted > 0 && continued > 0,
        "interrupted {interrupted}, continued {continued}"
    );
}

#[test]
fn a_restore_whose_load_fails_keeps_every_control_plane_off_the_database_until_it_is_run_again() {
    let m = Machine::install(combined_env(), false);
    let dump = apply(&m.actor(), update(ID, 81)).dump.unwrap();
    m.engine.with_state(|s| {
        s.db_failures.insert(
            "db-load".into(),
            (
                1,
                "pg_restore: error: could not execute query: disk full\n".into(),
            ),
        );
    });
    let result = run_restore(&m.actor(), restore_request(&nth_id(2), Some(&dump), None));
    assert_eq!(result.state, State::Failed);
    assert!(
        result.output.contains("Run the same command again"),
        "{}",
        result.output
    );
    assert!(m.dir.path().join("database-hold.json").exists());
    // Nothing starts a control plane meanwhile, a daemon restart included.
    m.engine.restart_daemon();
    m.actor().resume().unwrap();
    assert!(
        m.engine
            .state()
            .containers
            .values()
            .filter(|c| c.spec.name.starts_with(names::CONTROL_PLANE))
            .all(|c| c.status != "running"),
        "a control plane runs against a partly loaded database"
    );
    let refused = m
        .actor()
        .submit(Caller::ControlPlane, update(&nth_id(3), 82))
        .unwrap_err();
    assert!(
        refused.message.contains("a restore holds"),
        "{}",
        refused.message
    );
    // The hold is the control plane's only: a combined machine's node agent that went is
    // still re-created.
    m.engine.with_state(|s| {
        let id = s.container_named(names::NODE_AGENT).unwrap().id.clone();
        s.containers.remove(&id);
    });
    m.actor().resume().unwrap();
    assert_eq!(
        m.engine
            .state()
            .container_named(names::NODE_AGENT)
            .map(|c| c.status.clone()),
        Some("running".to_string()),
        "the node agent is re-created while the control plane is held"
    );
    assert!(m
        .engine
        .state()
        .containers
        .values()
        .filter(|c| c.spec.name.starts_with(names::CONTROL_PLANE))
        .all(|c| c.status != "running"));

    m.engine.with_state(|s| s.db_failures.clear());
    let again = run_restore(&m.actor(), restore_request(&nth_id(4), Some(&dump), None));
    assert_eq!(again.state, State::Succeeded, "{again:?}");
    assert_eq!(m.db().schema_version, OLD_SCHEMA);
    assert_eq!(m.control_plane().status, "running");
    m.never_an_older_control_plane("after the second restore");
}

#[test]
fn every_crash_point_of_a_migrating_update_settles_with_nothing_half_done() {
    let reference = Machine::install(combined_env(), true);
    let start = reference.engine.calls();
    apply(&reference.actor(), update(ID, 81));
    let total = reference.engine.calls() - start;

    let mut interrupted = 0;
    let mut continued = 0;
    for call in start..start + total {
        let m = Machine::install(combined_env(), true);
        let at = format!("call {}", call - start);
        m.engine.inject(Fault {
            call,
            when: When::Before,
            error: EngineError::Crashed,
        });
        let actor = m.actor();
        if actor.submit(Caller::ControlPlane, update(ID, 81)).is_err() {
            continue;
        }
        actor.wait_attempt();
        m.engine.clear_faults();
        let before = actor.status_for(Some(ID)).result.unwrap();
        drop(actor);
        m.actor()
            .resume()
            .unwrap_or_else(|e| panic!("{at}: the next start failed: {e}"));
        let result = m.actor().status_for(Some(ID)).result.unwrap();
        assert!(result.state.is_terminal(), "{at}: {result:?}");
        m.never_an_older_control_plane(&at);
        match before.state {
            State::Pending | State::Pulling => {
                interrupted += 1;
                assert_eq!(result.reason, Some(Reason::Interrupted), "{at}: {result:?}");
                assert_eq!(
                    m.control_plane().spec.image,
                    control_image(OLD_SCHEMA),
                    "{at}"
                );
                assert_eq!(m.control_plane().status, "running", "{at}");
                assert_eq!(m.db().schema_version, OLD_SCHEMA, "{at}");
                assert_eq!(
                    m.dump_files(),
                    Vec::<String>::new(),
                    "{at}: a dump was left"
                );
            }
            _ => {
                continued += 1;
                assert_eq!(result.state, State::Succeeded, "{at}: {result:?}");
                assert!(result.dump.is_some(), "{at}");
                assert_eq!(m.control_plane().spec.image, control_image(81), "{at}");
                assert_eq!(m.db().schema_version, 81, "{at}");
            }
        }
    }
    assert!(
        interrupted > 0 && continued > 0,
        "interrupted {interrupted}, continued {continued}"
    );
}

#[test]
fn a_crash_at_every_restore_phase_is_settled_by_the_next_start() {
    for phase in [
        RestorePhase::Checking,
        RestorePhase::Stopping,
        RestorePhase::Loading,
        RestorePhase::Starting,
        RestorePhase::Verifying,
    ] {
        let m = Machine::install(combined_env(), false);
        let dump = apply(&m.actor(), update(ID, 81)).dump.unwrap();
        let point = restore::crash_point(phase);
        let actor = m.actor_with(|c| {
            c.crash_after = Some(Box::new(move |component, p| {
                component == restore::COMPONENT && p == point
            }))
        });
        let id = nth_id(5);
        actor
            .submit_restore(restore_request(&id, Some(&dump), None))
            .unwrap();
        actor.wait_attempt();
        drop(actor);
        m.actor().resume().unwrap();
        let result = m.actor().status_operator(Some(&id)).result.unwrap();
        let at = format!("{phase:?}");
        if phase == RestorePhase::Checking {
            assert_eq!(result.reason, Some(Reason::Interrupted), "{at}: {result:?}");
            assert_eq!(m.db().schema_version, 81, "{at}");
        } else {
            assert_eq!(result.state, State::Succeeded, "{at}: {result:?}");
            assert_eq!(m.db().schema_version, OLD_SCHEMA, "{at}");
            assert_eq!(
                m.control_plane().spec.image,
                control_image(OLD_SCHEMA),
                "{at}"
            );
        }
        m.never_an_older_control_plane(&at);
    }
    let _ = Phase::Dumping;
}

#[test]
fn an_external_database_is_restored_by_the_operator_then_started_only_on_a_matching_schema() {
    let m = Machine::install(external_env(), false);
    let mut req = update(ID, 81);
    req.external_backup_confirmed = true;
    let failed = apply(&m.actor(), req);
    assert_eq!(failed.state, State::Failed);
    assert_eq!(failed.dump, None);
    assert_eq!(
        last_line(&failed.output),
        "docker exec quasar-recovery quasar-recovery restore --to 0.80.0"
    );
    // The failed control plane is stopped, its restart disabled: left running it would
    // migrate the operator's restored backup again on its next boot.
    assert!(
        failed.output.contains("stopped with its restart disabled"),
        "{}",
        failed.output
    );
    assert!(
        failed.output.contains("docker stop quasar-control-plane"),
        "{}",
        failed.output
    );
    assert_eq!(m.control_plane().spec.image, control_image(81));
    assert_eq!(m.control_plane().status, "exited");
    assert_eq!(m.control_plane().restart, RestartPolicy::No);
    m.engine.restart_daemon();
    m.actor().resume().unwrap();
    assert_eq!(
        m.control_plane().status,
        "exited",
        "nothing starts it again"
    );
    let actor = m.actor();
    let refused = actor
        .submit_restore(restore_request(
            &nth_id(2),
            Some("20260925T100000Z-schema-80"),
            None,
        ))
        .unwrap_err();
    assert!(
        refused.message.contains("holds no dump"),
        "{}",
        refused.message
    );

    // The operator has not restored their backup yet: the schema does not match.
    let result = run_restore(&actor, restore_request(&nth_id(3), None, Some("0.80.0")));
    assert_eq!(result.state, State::Failed);
    assert!(result.output.contains("schema 81"), "{}", result.output);
    assert_eq!(m.control_plane().spec.image, control_image(81));

    // Restored with their own tools, the old control plane starts again.
    m.engine
        .with_state(|s| s.database.as_mut().unwrap().schema_version = OLD_SCHEMA);
    let result = run_restore(&actor, restore_request(&nth_id(4), None, Some("0.80.0")));
    assert_eq!(result.state, State::Succeeded, "{result:?}");
    assert_eq!(m.control_plane().spec.image, control_image(OLD_SCHEMA));
    assert_eq!(m.control_plane().status, "running");
    m.never_an_older_control_plane("after the external restore");
    assert!(!m.dir.path().join("restore-point.json").exists());
    let refused = actor
        .submit_restore(restore_request(&nth_id(5), None, Some("0.80.0")))
        .unwrap_err();
    assert!(
        refused.message.contains("already been run"),
        "{}",
        refused.message
    );
}

#[test]
fn an_external_restore_is_refused_while_a_control_plane_runs() {
    let m = Machine::install(external_env(), false);
    let mut req = update(ID, 81);
    req.external_backup_confirmed = true;
    assert_eq!(apply(&m.actor(), req).state, State::Failed);
    // The operator starts the failed control plane by hand, then restores their backup.
    let id = m.control_plane().id;
    m.engine
        .with_state(|s| s.containers.get_mut(&id).unwrap().status = "running".into());
    m.engine
        .with_state(|s| s.database.as_mut().unwrap().schema_version = OLD_SCHEMA);
    let before = m.engine.state().by_name();
    let result = run_restore(
        &m.actor(),
        restore_request(&nth_id(2), None, Some("0.80.0")),
    );
    assert_eq!(result.state, State::Failed, "{result:?}");
    assert!(
        result.output.contains("docker stop quasar-control-plane"),
        "{}",
        result.output
    );
    assert!(
        result.output.contains("Nothing was changed"),
        "{}",
        result.output
    );
    assert_eq!(m.engine.state().by_name(), before);

    // Stopped, the same command restores.
    m.engine
        .with_state(|s| s.containers.get_mut(&id).unwrap().status = "exited".into());
    let result = run_restore(
        &m.actor(),
        restore_request(&nth_id(3), None, Some("0.80.0")),
    );
    assert_eq!(result.state, State::Succeeded, "{result:?}");
    assert_eq!(m.control_plane().spec.image, control_image(OLD_SCHEMA));
    m.never_an_older_control_plane("after the external restore");
}

#[test]
fn a_restore_whose_engine_goes_away_mid_load_says_the_database_may_be_empty_and_holds() {
    let m = Machine::install(combined_env(), false);
    let dump = apply(&m.actor(), update(ID, 81)).dump.unwrap();
    m.engine.with_state(|s| {
        s.db_interrupted.insert("db-load".into());
    });
    let result = run_restore(&m.actor(), restore_request(&nth_id(2), Some(&dump), None));
    assert_eq!(result.state, State::Failed, "{result:?}");
    assert!(
        result.output.contains("The database may now be empty"),
        "{}",
        result.output
    );
    assert!(
        result.output.contains("Run the same command again"),
        "{}",
        result.output
    );
    assert!(
        !result.output.contains("Nothing was changed"),
        "{}",
        result.output
    );
    assert_eq!(m.db().rows, "", "the load dropped the database");
    assert!(m.dir.path().join("database-hold.json").exists());
    m.engine.restart_daemon();
    m.actor().resume().unwrap();
    assert!(
        m.engine
            .state()
            .containers
            .values()
            .filter(|c| c.spec.name.starts_with(names::CONTROL_PLANE))
            .all(|c| c.status != "running"),
        "a control plane runs against an emptied database"
    );

    m.engine.with_state(|s| s.db_interrupted.clear());
    let again = run_restore(&m.actor(), restore_request(&nth_id(3), Some(&dump), None));
    assert_eq!(again.state, State::Succeeded, "{again:?}");
    assert_eq!(m.db().schema_version, OLD_SCHEMA);
    assert!(!m.dir.path().join("database-hold.json").exists());
}

#[test]
fn a_restored_dump_is_not_restored_again_without_force_again() {
    let m = Machine::install(combined_env(), false);
    let dump = apply(&m.actor(), update(ID, 81)).dump.unwrap();
    let done = run_restore(
        &m.actor(),
        restore_request(&nth_id(2), Some(&dump), Some("0.80.0")),
    );
    assert_eq!(done.state, State::Succeeded, "{done:?}");
    assert!(
        !m.dir.path().join("restore-point.json").exists(),
        "the point is cleared once its restore succeeded"
    );
    // A pre-#364 actor reads the finished restore as a closed attempt.
    assert_eq!(journal_format(&m, &nth_id(2)), 1);

    let refused = m
        .actor()
        .submit_restore(restore_request(&nth_id(3), Some(&dump), Some("0.80.0")))
        .unwrap_err();
    assert_eq!(refused.reason, Reason::Invalid);
    assert!(
        refused.message.contains("already restored") && refused.message.contains("--force-again"),
        "{}",
        refused.message
    );
    let again = run_restore(
        &m.actor(),
        restore::request_again(nth_id(4), Some(dump.clone()), Some("0.80.0".into()), true),
    );
    assert_eq!(again.state, State::Succeeded, "{again:?}");
    assert_eq!(m.db().schema_version, OLD_SCHEMA);
}

#[test]
fn a_failed_restore_is_stored_as_a_journal_an_older_actor_reads() {
    let m = Machine::install(combined_env(), false);
    let dump = apply(&m.actor(), update(ID, 81)).dump.unwrap();
    let file = m.dir.path().join("dumps").join(format!("{dump}.dump"));
    let mut bytes = std::fs::read(&file).unwrap();
    bytes.extend_from_slice(b"garbage");
    std::fs::write(&file, &bytes).unwrap();
    let result = run_restore(&m.actor(), restore_request(&nth_id(2), Some(&dump), None));
    assert_eq!(result.state, State::Failed);
    assert_eq!(journal_format(&m, &nth_id(2)), 1);
}

#[test]
fn an_update_whose_running_schema_cannot_be_read_is_refused() {
    let m = Machine::install(combined_env(), true);
    // The running control plane's image is gone from the engine: its schema is unknown.
    m.engine.with_state(|s| {
        s.images
            .retain(|reference, _| *reference != control_image(OLD_SCHEMA))
    });
    let mut req = update(ID, 81);
    req.migrates = false;
    let result = apply(&m.actor(), req);
    assert_eq!(result.state, State::Failed, "{result:?}");
    assert_eq!(result.reason, Some(Reason::Invalid));
    assert!(
        result.output.contains("cannot be read"),
        "{}",
        result.output
    );
    assert_eq!(m.control_plane().spec.image, control_image(OLD_SCHEMA));
    assert_eq!(m.control_plane().status, "running");
    assert_eq!(m.db().schema_version, OLD_SCHEMA);
}

#[test]
fn a_migrating_update_after_an_unrestored_failure_takes_no_dump_it_could_not_restore() {
    let m = Machine::install(combined_env(), false);
    let first = apply(&m.actor(), update(ID, 81));
    assert_eq!(first.state, State::Failed);
    let dumps = m.dump_files();
    let result = apply(&m.actor(), update(&nth_id(2), 82));
    assert_eq!(result.state, State::Failed, "{result:?}");
    assert_eq!(result.reason, Some(Reason::BackupFailed));
    assert!(
        result.output.contains("restore the dump"),
        "{}",
        result.output
    );
    assert_eq!(
        m.dump_files(),
        dumps,
        "the first update's dump is kept, no other taken"
    );
    assert_eq!(
        last_line(&first.output),
        format!(
            "docker exec quasar-recovery quasar-recovery restore --dump {} --to 0.80.0",
            first.dump.clone().unwrap()
        )
    );
    // The first update's way back still works.
    let restored = run_restore(
        &m.actor(),
        restore_request(&nth_id(3), first.dump.as_deref(), Some("0.80.0")),
    );
    assert_eq!(restored.state, State::Succeeded, "{restored:?}");
}

#[test]
fn only_the_operator_socket_takes_a_restore() {
    let m = Machine::install(combined_env(), false);
    let dump = apply(&m.actor(), update(ID, 81)).dump.unwrap();
    let actor = m.actor();
    assert!(actor.serve().is_empty());
    let body = serde_json::to_string(&restore_request(&nth_id(2), Some(&dump), None)).unwrap();
    let control = m.knobs.sockets.join("control/control.sock");
    let (code, answer) =
        quasar_recovery::operator::call(&control, "POST", "/v1/submit", Some(&body)).unwrap();
    assert_eq!(code, 400, "{answer}");
    assert!(answer.contains("operator's command"), "{answer}");

    // The operator socket as main.rs serves it (its real path is inside the actor's own
    // container, outside the socket volume), on a path of this test's own.
    let dir = std::env::temp_dir().join(format!("qr-op-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let operator = dir.join("operator.sock");
    let _ = std::fs::remove_file(&operator);
    let listener = std::os::unix::net::UnixListener::bind(&operator).unwrap();
    {
        let actor = actor.clone();
        std::thread::spawn(move || quasar_recovery::operator::serve(listener, actor));
    }
    let (code, answer) =
        quasar_recovery::operator::call(&operator, "POST", "/v1/restore", Some(&body)).unwrap();
    assert_eq!(code, 202, "{answer}");
    actor.wait_attempt();
    let (code, answer) = quasar_recovery::operator::call(
        &operator,
        "GET",
        &format!("/v1/status?request_id={}", nth_id(2)),
        None,
    )
    .unwrap();
    assert_eq!(code, 200);
    let status: quasar_recovery::socket::Status = serde_json::from_str(&answer).unwrap();
    assert_eq!(status.result.unwrap().state, State::Succeeded);
    assert_eq!(
        actor
            .status_operator(Some(&nth_id(2)))
            .result
            .unwrap()
            .state,
        State::Succeeded,
        "the restore is the operator's"
    );
    // The control socket never sees the operator's attempt.
    let (_, answer) = quasar_recovery::operator::call(
        &control,
        "GET",
        &format!("/v1/status?request_id={}", nth_id(2)),
        None,
    )
    .unwrap();
    let status: quasar_recovery::socket::Status = serde_json::from_str(&answer).unwrap();
    assert!(status.result.is_none());
    // Nor does a replacement request on the operator socket become a restore.
    let replace = serde_json::to_string(&update(&nth_id(3), 81)).unwrap();
    let (code, answer) =
        quasar_recovery::operator::call(&operator, "POST", "/v1/restore", Some(&replace)).unwrap();
    assert_eq!(code, 400, "{answer}");
    let _ = std::fs::remove_dir_all(&dir);
}
