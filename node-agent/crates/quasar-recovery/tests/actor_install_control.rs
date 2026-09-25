//! Combined and control-only installs (#361) against the in-memory engine and a temporary
//! machine-state directory, from the seed's inputs to the running machine, observed
//! through engine state, machine state and `status`.

mod support;

use std::collections::BTreeMap;
use std::sync::Arc;

use quasar_recovery::actor::{Actor, ActorConfig, OperatorInputs, ResumeError};
use quasar_recovery::engine::{Behaviour, EngineError, FakeEngine, FakeState, Fault, Image, When};
use quasar_recovery::recipe::{control, names, secrets};
use quasar_recovery::seed::Outcome;
use quasar_recovery::socket::{DatabaseMode, MachineRole};
use support::*;

const CONTROL_IMAGE: &str = "registry.example.invalid/quasar/quasar-control-plane@sha256:aa11000000000000000000000000000000000000000000000000000000000000";
const POSTGRES_IMAGE: &str = "docker.io/library/postgres@sha256:dd55000000000000000000000000000000000000000000000000000000000000";
const PUBLIC_HOST: &str = "quasar.example.invalid";
const DB_PASSWORD: &str = "operators-own-db-password";

fn image(id: &str, reference: &str, recipe: Option<&str>) -> Image {
    Image {
        id: id.into(),
        repo_digests: vec![reference.into()],
        labels: recipe
            .map(|r| BTreeMap::from([("org.quasar.recipe".to_string(), r.to_string())]))
            .unwrap_or_default(),
    }
}

/// A clean AMD machine with only the seed on it, the three platform images in the
/// registry (the agent at recipe revision 2), and Postgres and the control plane healthy
/// once they run.
fn control_host(env: BTreeMap<String, String>) -> FakeState {
    let mut state = seeded_host(env);
    state
        .registry
        .insert(AGENT_IMAGE.into(), agent_image(Some("2")));
    state.registry.insert(
        CONTROL_IMAGE.into(),
        image(
            "sha256:c0c0000000000000000000000000000000000000000000000000000000000000",
            CONTROL_IMAGE,
            Some("1"),
        ),
    );
    state.registry.insert(
        POSTGRES_IMAGE.into(),
        image(
            "sha256:9090000000000000000000000000000000000000000000000000000000000000",
            POSTGRES_IMAGE,
            None,
        ),
    );
    for reference in [CONTROL_IMAGE, POSTGRES_IMAGE] {
        state.behaviour.insert(
            reference.into(),
            Behaviour {
                health: Some("healthy".into()),
                ..Default::default()
            },
        );
    }
    state
}

fn combined_env() -> BTreeMap<String, String> {
    BTreeMap::from([
        ("QUASAR_ROLE".to_string(), "combined".to_string()),
        ("QUASAR_HOME_ROOT".into(), HOME.into()),
        ("QUASAR_AGENT_IMAGE".into(), AGENT_IMAGE.into()),
        ("QUASAR_CONTROL_PLANE_IMAGE".into(), CONTROL_IMAGE.into()),
        ("QUASAR_POSTGRES_IMAGE".into(), POSTGRES_IMAGE.into()),
        ("QUASAR_PUBLIC_HOST".into(), PUBLIC_HOST.into()),
    ])
}

fn control_only_external_env() -> BTreeMap<String, String> {
    BTreeMap::from([
        ("QUASAR_ROLE".to_string(), "control-only".to_string()),
        ("QUASAR_CONTROL_PLANE_IMAGE".into(), CONTROL_IMAGE.into()),
        ("QUASAR_DATABASE_HOST".into(), "db.example.invalid".into()),
        ("QUASAR_DATABASE_PORT".into(), "5433".into()),
        ("QUASAR_DATABASE_USER".into(), "quasar_app".into()),
        ("QUASAR_DATABASE_PASSWORD".into(), DB_PASSWORD.into()),
    ])
}

fn fast(mut actor: ActorConfig) -> ActorConfig {
    actor.healthy_wait = std::time::Duration::from_millis(20);
    actor.timing.poll = std::time::Duration::ZERO;
    actor
}

/// The seed's first look, then the actor it created, as a new process on the machine.
fn seeded(env: BTreeMap<String, String>) -> (Arc<FakeEngine>, tempfile::TempDir, String) {
    let engine = Arc::new(FakeEngine::new(control_host(env)));
    let dir = tempfile::tempdir().unwrap();
    let created = seed(&engine, dir.path(), SEED_ID).step();
    assert!(matches!(created, Outcome::Created { .. }), "{created:?}");
    let id = engine
        .state()
        .container_named(names::RECOVERY_ACTOR)
        .unwrap()
        .id
        .clone();
    (engine, dir, id)
}

fn start(engine: &Arc<FakeEngine>, dir: &std::path::Path, actor_id: &str) -> Actor {
    let mut config = ActorConfig::new(dir, MachineRole::Gpu, OperatorInputs::default());
    config.self_container = Some(actor_id.into());
    config.seed_container = Some(SEED_ID.into());
    config.now = Box::new(|| NOW.to_string());
    config.gpus_probe_backoff = std::time::Duration::ZERO;
    Actor::new(engine.clone(), fast(config))
}

fn volume_file(state: &FakeState, volume: &str, file: &str) -> (Vec<u8>, u32) {
    state.volumes[volume]
        .files
        .get(file)
        .unwrap_or_else(|| panic!("{volume}/{file}"))
        .clone()
}

fn secret(dir: &std::path::Path, name: &str) -> String {
    let raw = std::fs::read(dir.join("secrets").join(format!("{name}.json"))).unwrap();
    serde_json::from_slice(&raw).unwrap()
}

fn every_env_value(state: &FakeState) -> Vec<String> {
    state
        .containers
        .values()
        .filter(|c| c.spec.name != "quasar-seed")
        .flat_map(|c| c.spec.env.values().cloned())
        .collect()
}

#[test]
fn a_combined_install_brings_up_postgres_then_the_control_plane_then_its_agent() {
    let (engine, dir, id) = seeded(combined_env());
    start(&engine, dir.path(), &id).resume().unwrap();
    let state = engine.state();

    let postgres = state.container_named(names::POSTGRES).unwrap();
    let cp = state.container_named(names::CONTROL_PLANE).unwrap();
    let agent = state.container_named(names::NODE_AGENT).unwrap();
    for c in [postgres, cp, agent] {
        assert_eq!(c.status, "running", "{}", c.spec.name);
        assert_eq!(c.spec.labels["io.quasar.installation"], INSTALLATION);
    }
    // Each is created only once what it depends on exists.
    assert!(postgres.id < cp.id && cp.id < agent.id);
    assert_eq!(postgres.spec.image, POSTGRES_IMAGE);
    assert_eq!(cp.spec.image, CONTROL_IMAGE);
    assert!(state.networks.contains_key(names::PLATFORM_NETWORK));
    assert_eq!(
        state.networks[names::PLATFORM_NETWORK]["io.quasar.installation"],
        INSTALLATION
    );
    assert_eq!(cp.spec.env["QUASAR_PUBLIC_HOST"], PUBLIC_HOST);
    assert_eq!(agent.spec.env["CONTROL_PLANE_URL"], "ws://127.0.0.1:8080");

    // The secrets exist once, in machine state, and reach each container as files.
    let password = secret(dir.path(), secrets::DATABASE_PASSWORD);
    let key = secret(dir.path(), secrets::SECRET_KEY);
    let token = secret(dir.path(), secrets::LOCAL_ENROLLMENT);
    assert_eq!(password.len(), 64);
    assert_eq!(
        volume_file(&state, names::POSTGRES_SECRETS_VOLUME, "database-password"),
        (password.clone().into_bytes(), 0o444)
    );
    for (file, value) in [
        ("database-password", &password),
        ("secret-key", &key),
        ("local-enrollment", &token),
    ] {
        assert_eq!(
            volume_file(&state, names::CONTROL_PLANE_SECRETS_VOLUME, file),
            (value.clone().into_bytes(), 0o400)
        );
    }
    assert_eq!(
        volume_file(&state, names::NODE_AGENT_SECRETS_VOLUME, "local-enrollment").0,
        token.as_bytes()
    );
    // ... and never as an environment value.
    for value in every_env_value(&state) {
        for s in [&password, &key, &token] {
            assert!(!value.contains(s.as_str()), "a secret in an environment");
        }
    }
    // No static enrollment token anywhere: the agent enrolls with the local one.
    assert!(state
        .containers
        .values()
        .all(|c| !c.spec.env.contains_key("ENROLLMENT_TOKEN")));

    let status = start(&engine, dir.path(), &id).status();
    assert_eq!(status.role, MachineRole::Combined);
    assert_eq!(status.database, DatabaseMode::Owned);
    assert_eq!(status.node_name.as_deref(), Some("gpu-host-01"));
    let roles: Vec<_> = status.services.iter().map(|s| s.role.as_str()).collect();
    for role in ["recovery-actor", "postgres", "control-plane", "node-agent"] {
        assert!(roles.contains(&role), "{roles:?}");
    }
}

#[test]
fn re_running_a_finished_combined_install_changes_nothing() {
    let (engine, dir, id) = seeded(combined_env());
    start(&engine, dir.path(), &id).resume().unwrap();
    let (before, files) = (engine.state(), contents(dir.path()));

    // A restarted seed and a restarted actor, twice.
    for _ in 0..2 {
        assert!(matches!(
            seed(&engine, dir.path(), SEED_ID).step(),
            Outcome::Present { .. }
        ));
        start(&engine, dir.path(), &id).resume().unwrap();
    }
    let after = engine.state();
    assert_eq!(after.by_name(), before.by_name());
    assert_eq!(after.volumes, before.volumes);
    assert_eq!(after.networks, before.networks);
    assert_eq!(contents(dir.path()), files);
}

#[test]
fn a_control_only_install_brings_up_no_agent_and_never_probes_for_a_gpu() {
    let mut env = combined_env();
    env.insert("QUASAR_ROLE".into(), "control-only".into());
    env.remove("QUASAR_AGENT_IMAGE");
    env.remove("QUASAR_HOME_ROOT");
    let (engine, dir, id) = seeded(env);
    start(&engine, dir.path(), &id).resume().unwrap();
    let state = engine.state();

    assert!(state.container_named(names::CONTROL_PLANE).is_some());
    assert!(state.container_named(names::POSTGRES).is_some());
    assert!(state.container_named(names::NODE_AGENT).is_none());
    assert!(
        !state.images.contains_key(AGENT_IMAGE),
        "the agent image was pulled"
    );
    assert!(!dir.path().join("secrets/local-enrollment.json").exists());
    let cp = state.container_named(names::CONTROL_PLANE).unwrap();
    assert!(!cp.spec.env.contains_key("QUASAR_LOCAL_ENROLLMENT_FILE"));
    assert!(!cp.spec.env.contains_key("QUASAR_HOME_ROOT"));

    let status = start(&engine, dir.path(), &id).status();
    assert_eq!(status.role, MachineRole::ControlOnly);
    assert!(status.services.iter().all(|s| s.role != "node-agent"));
    assert!(quasar_recovery::explain::explain(
        &status,
        quasar_recovery::machine::MachineDir::new(dir.path())
            .load_machine()
            .unwrap()
            .as_ref()
    )
    .is_empty());
}

#[test]
fn an_external_database_is_used_never_created_and_its_settings_are_copied_at_first_boot() {
    let (engine, dir, id) = seeded(control_only_external_env());
    start(&engine, dir.path(), &id).resume().unwrap();
    let state = engine.state();

    assert!(state.container_named(names::POSTGRES).is_none());
    assert!(!state.images.contains_key(POSTGRES_IMAGE));
    assert!(!state.images.keys().any(|i| i.contains("postgres")));
    let cp = state.container_named(names::CONTROL_PLANE).unwrap();
    assert_eq!(cp.spec.env["QUASAR_DATABASE_HOST"], "db.example.invalid");
    assert_eq!(cp.spec.env["QUASAR_DATABASE_PORT"], "5433");
    assert_eq!(cp.spec.env["QUASAR_DATABASE_USER"], "quasar_app");
    // The password is in machine state and the control plane's secrets volume, and in
    // nothing machine state keeps in the clear.
    assert_eq!(secret(dir.path(), secrets::DATABASE_PASSWORD), DB_PASSWORD);
    assert_eq!(
        volume_file(
            &state,
            names::CONTROL_PLANE_SECRETS_VOLUME,
            "database-password"
        )
        .0,
        DB_PASSWORD.as_bytes()
    );
    let machine = std::fs::read_to_string(dir.path().join("machine.json")).unwrap();
    assert!(machine.contains("db.example.invalid"));
    assert!(!machine.contains(DB_PASSWORD));
    assert!(every_env_value(&state)
        .iter()
        .all(|v| !v.contains(DB_PASSWORD)));

    let status = start(&engine, dir.path(), &id).status();
    assert_eq!(status.database, DatabaseMode::External);

    // The seed's copy of the settings is read once: machine state wins afterwards.
    engine.with_state(|s| {
        let seed = s.containers.get_mut(SEED_ID).unwrap();
        seed.spec
            .env
            .insert("QUASAR_DATABASE_HOST".into(), "elsewhere.invalid".into());
    });
    start(&engine, dir.path(), &id).resume().unwrap();
    let cp = engine
        .state()
        .container_named(names::CONTROL_PLANE)
        .unwrap()
        .clone();
    assert_eq!(cp.spec.env["QUASAR_DATABASE_HOST"], "db.example.invalid");
}

#[test]
fn an_unreachable_external_database_is_explained_by_status() {
    let (engine, dir, id) = seeded(control_only_external_env());
    engine.with_state(|s| {
        s.behaviour.get_mut(CONTROL_IMAGE).unwrap().health = Some("unhealthy".into());
    });
    start(&engine, dir.path(), &id).resume().unwrap();
    let actor = start(&engine, dir.path(), &id);
    let machine = quasar_recovery::machine::MachineDir::new(dir.path())
        .load_machine()
        .unwrap();
    let lines = quasar_recovery::explain::explain(&actor.status(), machine.as_ref());
    assert_eq!(lines.len(), 1, "{lines:?}");
    assert!(lines[0].contains("db.example.invalid:5433"), "{}", lines[0]);
    assert!(
        lines[0].contains("docker logs quasar-control-plane"),
        "{}",
        lines[0]
    );
}

#[test]
fn postgres_not_yet_ready_delays_the_control_plane_rather_than_failing_the_install() {
    let (engine, dir, id) = seeded(combined_env());
    engine.with_state(|s| {
        s.behaviour.get_mut(POSTGRES_IMAGE).unwrap().health = Some("starting".into());
    });
    start(&engine, dir.path(), &id)
        .resume()
        .expect("a slow database never fails the install");
    let state = engine.state();
    assert!(state.container_named(names::CONTROL_PLANE).is_some());
    assert!(state.container_named(names::NODE_AGENT).is_some());
}

#[test]
fn a_combined_hosts_agent_image_must_read_the_local_token_or_nothing_is_installed() {
    let engine = Arc::new(FakeEngine::new({
        let mut s = control_host(combined_env());
        s.registry
            .insert(AGENT_IMAGE.into(), agent_image(Some("1")));
        s
    }));
    let dir = tempfile::tempdir().unwrap();
    let refused = seed(&engine, dir.path(), SEED_ID).step();
    assert!(
        matches!(&refused, Outcome::Idle { why, .. } if why.contains("revision 2")),
        "{refused:?}"
    );
    assert!(engine
        .state()
        .container_named(names::RECOVERY_ACTOR)
        .is_none());
    assert!(!dir.path().join("machine.json").exists());
}

#[test]
fn an_install_interrupted_anywhere_completes_on_the_next_start_with_one_set_of_secrets() {
    // The machine one uninterrupted install reaches.
    let (clean, clean_dir, clean_id) = seeded(combined_env());
    let before = clean.calls();
    start(&clean, clean_dir.path(), &clean_id).resume().unwrap();
    let calls = clean.calls() - before;
    let reference = clean.state();

    for call in 0..calls {
        for when in [When::Before, When::After] {
            let (engine, dir, id) = seeded(combined_env());
            engine.inject(Fault {
                call: engine.calls() + call,
                when,
                error: EngineError::Crashed,
            });
            let first = start(&engine, dir.path(), &id).resume();
            engine.clear_faults();
            // The crashed process is gone; the next start is a new one.
            start(&engine, dir.path(), &id)
                .resume()
                .unwrap_or_else(|e| panic!("call {call} {when:?}: {e}; first {first:?}"));
            let state = engine.state();
            let shape = |s: &FakeState| {
                s.by_name()
                    .into_iter()
                    .filter(|(name, _)| name != "quasar-seed")
                    .map(|(name, (spec, status))| (name, spec.labels.clone(), status))
                    .collect::<Vec<_>>()
            };
            assert_eq!(shape(&state), shape(&reference), "call {call} {when:?}");
            // One password, whichever start generated it.
            let password = secret(dir.path(), secrets::DATABASE_PASSWORD);
            assert_eq!(
                volume_file(&state, names::POSTGRES_SECRETS_VOLUME, "database-password").0,
                password.as_bytes(),
                "call {call} {when:?}"
            );
            assert_eq!(
                volume_file(
                    &state,
                    names::CONTROL_PLANE_SECRETS_VOLUME,
                    "database-password"
                )
                .0,
                password.as_bytes(),
                "call {call} {when:?}"
            );
        }
    }
}

#[test]
fn a_network_or_container_of_another_owner_is_never_acted_on() {
    let (engine, dir, id) = seeded(combined_env());
    engine.with_state(|s| {
        s.networks
            .insert(names::PLATFORM_NETWORK.into(), BTreeMap::new());
    });
    let err = start(&engine, dir.path(), &id).resume().unwrap_err();
    assert!(matches!(err, ResumeError::OwnerConflict(_)), "{err}");
    assert!(engine.state().container_named(names::POSTGRES).is_none());
}

#[test]
fn the_default_postgres_is_pinned_by_digest() {
    assert!(control::DEFAULT_POSTGRES_IMAGE.contains("@sha256:"));
    quasar_recovery::recipe::ImageRef::parse(control::DEFAULT_POSTGRES_IMAGE).unwrap();
}

#[test]
fn bootstrap_refuses_mixed_or_missing_database_inputs() {
    use quasar_recovery::bootstrap::Bootstrap;
    let check = |env: BTreeMap<String, String>| {
        let pairs: Vec<String> = env.iter().map(|(k, v)| format!("{k}={v}")).collect();
        Bootstrap::from_env(&pairs).unwrap().check(Some("host"))
    };
    let mut both = control_only_external_env();
    both.insert("QUASAR_POSTGRES_IMAGE".into(), POSTGRES_IMAGE.into());
    assert!(check(both).unwrap_err().contains("QUASAR_POSTGRES_IMAGE"));
    let mut no_password = control_only_external_env();
    no_password.remove("QUASAR_DATABASE_PASSWORD");
    assert!(check(no_password)
        .unwrap_err()
        .contains("QUASAR_DATABASE_PASSWORD"));
    let mut stray = combined_env();
    stray.insert("QUASAR_DATABASE_PASSWORD".into(), "x".into());
    assert!(check(stray).unwrap_err().contains("QUASAR_DATABASE_HOST"));
    let mut no_cp = combined_env();
    no_cp.remove("QUASAR_CONTROL_PLANE_IMAGE");
    assert!(check(no_cp)
        .unwrap_err()
        .contains("QUASAR_CONTROL_PLANE_IMAGE"));
    // The template root defaults beside the home root, as the agent's own default does.
    let checked = check(combined_env()).unwrap();
    assert_eq!(checked.template_root, "/srv/quasar/templates");
}

fn post(path: &std::path::Path, body: &str) -> String {
    use std::io::{Read, Write};
    let mut stream = std::os::unix::net::UnixStream::connect(path).unwrap();
    write!(
        stream,
        "POST /v1/submit HTTP/1.0\r\nContent-Length: {}\r\n\r\n{body}",
        body.len()
    )
    .unwrap();
    let mut out = String::new();
    stream.read_to_string(&mut out).unwrap();
    out
}

#[test]
fn the_control_socket_serves_the_machine_and_names_the_control_plane_as_its_caller() {
    use quasar_recovery::{server, trust::Caller};
    let (engine, dir, id) = seeded(combined_env());
    let actor = Arc::new(start(&engine, dir.path(), &id));
    actor.resume().unwrap();

    let plan = actor.socket_plan();
    let callers: Vec<_> = plan.iter().map(|p| p.caller).collect();
    assert_eq!(callers, vec![Caller::Agent, Caller::ControlPlane]);
    assert_eq!(
        plan[1].path,
        std::path::Path::new("/run/quasar-recovery/control/control.sock")
    );
    assert_eq!(plan[1].owner, Some((1000, 1000)));

    let sockets = tempfile::tempdir().unwrap();
    let path = sockets.path().join("control").join("control.sock");
    let listener = server::bind_owned(&path, None).unwrap();
    let serving = actor.clone();
    std::thread::spawn(move || server::serve(listener, serving, Caller::ControlPlane));

    let status: quasar_recovery::socket::Status =
        serde_json::from_str(&server::fetch_status(&path).unwrap()).unwrap();
    assert_eq!(status.role, MachineRole::Combined);
    assert_eq!(status.node_name.as_deref(), Some("gpu-host-01"));
    assert_eq!(status.database, DatabaseMode::Owned);

    // The control socket may not name a host's agent: that goes over its agent socket.
    let fixture: serde_json::Value = serde_json::from_str(
        &std::fs::read_to_string(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("../../../testdata/recovery/socket/request-replace-agent.json"),
        )
        .unwrap(),
    )
    .unwrap();
    let reply = post(&path, &fixture["body"].to_string());
    assert!(reply.starts_with("HTTP/1.1 400"), "{reply}");
    assert!(reply.contains("\"invalid\""), "{reply}");
    assert!(engine
        .state()
        .containers
        .values()
        .all(|c| !c.spec.name.ends_with(".kept")));
}

const TEST_NAMESPACE: &str = "registry.test.invalid:5000/rh06";

fn agent_replace(id: &str, image: &str) -> quasar_recovery::socket::Request {
    use quasar_recovery::socket::{Component, Release, Request, RequestKind};
    Request {
        request_id: id.into(),
        kind: RequestKind::Replace,
        components: vec![Component {
            name: "node-agent".into(),
            image: image.into(),
            digest: format!("sha256:{}", "e".repeat(64)),
        }],
        release: Release {
            id: String::new(),
            version: None,
            source_commit: "c".repeat(40),
        },
        migrates: false,
        schema_version: None,
        external_backup_confirmed: false,
        dump: None,
        purge: false,
        wait_timeout_s: 0,
    }
}

/// The seed's trust inputs reach an actor the seed created, whose own environment holds
/// none (the frozen profile): recorded at first install, used from then on.
#[test]
fn release_trust_comes_from_the_seed_at_first_install_and_stays_in_machine_state() {
    use quasar_recovery::socket::Reason;
    use quasar_recovery::trust::Caller;
    let mut env = seed_env();
    env.insert(
        "QUASAR_UPDATER_ALLOWED_NAMESPACES".into(),
        TEST_NAMESPACE.into(),
    );
    let engine = Arc::new(FakeEngine::new(seeded_host(env)));
    let dir = tempfile::tempdir().unwrap();
    assert!(matches!(
        seed(&engine, dir.path(), SEED_ID).step(),
        Outcome::Created { .. }
    ));
    let id = engine
        .state()
        .container_named(names::RECOVERY_ACTOR)
        .unwrap()
        .id
        .clone();
    start(&engine, dir.path(), &id).resume().unwrap();
    let machine = std::fs::read_to_string(dir.path().join("machine.json")).unwrap();
    assert!(machine.contains(TEST_NAMESPACE), "{machine}");

    // A later start (its own environment still empty) admits the test registry's image...
    let actor = Arc::new(start(&engine, dir.path(), &id));
    actor.resume().unwrap();
    assert_eq!(
        actor.trust().unwrap().allowed_namespaces,
        vec![TEST_NAMESPACE]
    );
    let accepted = actor.submit(
        Caller::Agent,
        agent_replace(
            "0b7e1d2c-3a4f-4b5c-8d6e-7f8091a2b3c4",
            &format!("{TEST_NAMESPACE}/quasar-node-agent"),
        ),
    );
    assert!(accepted.is_ok(), "{accepted:?}");
    actor.wait_attempt();
    // ... and still refuses anything outside it.
    let refused = actor
        .submit(
            Caller::Agent,
            agent_replace(
                "1c8f2e3d-4b5a-4c6d-9e7f-8091a2b3c4d5",
                "registry.elsewhere.invalid/quasar/quasar-node-agent",
            ),
        )
        .unwrap_err();
    assert_eq!(refused.reason, Reason::NamespaceRejected);
}

#[test]
fn a_machine_whose_seed_named_no_trust_keeps_the_default_allowlist() {
    use quasar_recovery::socket::Reason;
    use quasar_recovery::trust::Caller;
    let engine = Arc::new(FakeEngine::new(seeded_host(seed_env())));
    let dir = tempfile::tempdir().unwrap();
    seed(&engine, dir.path(), SEED_ID).step();
    let id = engine
        .state()
        .container_named(names::RECOVERY_ACTOR)
        .unwrap()
        .id
        .clone();
    let actor = Arc::new(start(&engine, dir.path(), &id));
    actor.resume().unwrap();
    let refused = actor
        .submit(
            Caller::Agent,
            agent_replace(
                "0b7e1d2c-3a4f-4b5c-8d6e-7f8091a2b3c4",
                &format!("{TEST_NAMESPACE}/quasar-node-agent"),
            ),
        )
        .unwrap_err();
    assert_eq!(refused.reason, Reason::NamespaceRejected);
}

#[test]
fn the_control_plane_takes_the_allowlist_and_the_test_registry_from_the_machine() {
    let mut env = combined_env();
    env.insert(
        "QUASAR_UPDATER_ALLOWED_NAMESPACES".into(),
        TEST_NAMESPACE.into(),
    );
    env.insert(
        "QUASAR_PLATFORM_INSECURE_REGISTRIES".into(),
        "registry.test.invalid:5000".into(),
    );
    let (engine, dir, id) = seeded(env);
    start(&engine, dir.path(), &id).resume().unwrap();
    let state = engine.state();
    let cp = state.container_named(names::CONTROL_PLANE).unwrap();
    assert_eq!(
        cp.spec.env["QUASAR_UPDATER_ALLOWED_NAMESPACES"],
        TEST_NAMESPACE
    );
    assert_eq!(
        cp.spec.env["QUASAR_PLATFORM_INSECURE_REGISTRIES"],
        "registry.test.invalid:5000"
    );
}

#[test]
fn an_unreadable_trust_input_is_refused_before_anything_is_installed() {
    let mut env = seed_env();
    env.insert("QUASAR_UPDATER_SIGNATURE_MODE".into(), "sometimes".into());
    let engine = Arc::new(FakeEngine::new(seeded_host(env)));
    let dir = tempfile::tempdir().unwrap();
    let refused = seed(&engine, dir.path(), SEED_ID).step();
    assert!(
        matches!(&refused, Outcome::Idle { why, .. } if why.contains("QUASAR_UPDATER_SIGNATURE_MODE")),
        "{refused:?}"
    );
    assert!(engine
        .state()
        .container_named(names::RECOVERY_ACTOR)
        .is_none());
}

/// Add host (#359) installs on a new GPU host the seed this machine runs and its agent.
#[test]
fn the_control_plane_serves_add_host_this_machines_seed_and_agent_images() {
    let (engine, dir, id) = seeded(combined_env());
    start(&engine, dir.path(), &id).resume().unwrap();
    let state = engine.state();
    let cp = state.container_named(names::CONTROL_PLANE).unwrap();
    assert_eq!(cp.spec.env["QUASAR_ENROLL_SEED_IMAGE"], ACTOR_IMAGE);
    assert_eq!(cp.spec.env["QUASAR_ENROLL_AGENT_IMAGE"], AGENT_IMAGE);

    // A control-only machine runs no agent but names the one new hosts install.
    let mut env = combined_env();
    env.insert("QUASAR_ROLE".into(), "control-only".into());
    let (engine, dir, id) = seeded(env);
    start(&engine, dir.path(), &id).resume().unwrap();
    let state = engine.state();
    assert!(state.container_named(names::NODE_AGENT).is_none());
    assert!(!state.images.contains_key(AGENT_IMAGE));
    let cp = state.container_named(names::CONTROL_PLANE).unwrap();
    assert_eq!(cp.spec.env["QUASAR_ENROLL_AGENT_IMAGE"], AGENT_IMAGE);
}

/// The control plane knows its machine's shape from its own configuration, and takes
/// trusted proxies from the seed under its own rules.
#[test]
fn the_control_plane_is_told_its_machine_shape_and_trusted_proxies() {
    let mut env = combined_env();
    env.insert(
        "QUASAR_TRUSTED_PROXIES".into(),
        "172.18.0.0/16,10.1.2.3".into(),
    );
    let (engine, dir, id) = seeded(env);
    start(&engine, dir.path(), &id).resume().unwrap();
    let state = engine.state();
    let cp = state.container_named(names::CONTROL_PLANE).unwrap();
    assert_eq!(cp.spec.env["QUASAR_MACHINE_ROLE"], "combined");
    assert_eq!(cp.spec.env["QUASAR_MACHINE_NODE_NAME"], "gpu-host-01");
    assert_eq!(
        cp.spec.env["QUASAR_TRUSTED_PROXIES"],
        "172.18.0.0/16,10.1.2.3"
    );
    assert_eq!(cp.spec.env["QUASAR_ENV"], "production");

    let mut env = combined_env();
    env.insert("QUASAR_ROLE".into(), "control-only".into());
    let (engine, dir, id) = seeded(env);
    start(&engine, dir.path(), &id).resume().unwrap();
    let state = engine.state();
    let cp = state.container_named(names::CONTROL_PLANE).unwrap();
    assert_eq!(cp.spec.env["QUASAR_MACHINE_ROLE"], "control_only");
    assert_eq!(cp.spec.env["QUASAR_TRUSTED_PROXIES"], "");

    for bad in ["0.0.0.0/0", "::/0", "not-an-address", "10.0.0.0/33"] {
        let mut env = combined_env();
        env.insert("QUASAR_TRUSTED_PROXIES".into(), bad.into());
        let engine = Arc::new(FakeEngine::new(control_host(env)));
        let dir = tempfile::tempdir().unwrap();
        let refused = seed(&engine, dir.path(), SEED_ID).step();
        assert!(
            matches!(&refused, Outcome::Idle { why, .. } if why.contains("QUASAR_TRUSTED_PROXIES")),
            "{bad}: {refused:?}"
        );
    }
}

/// An actor put back by a revert reads machine state a newer actor wrote.
#[test]
fn machine_state_with_inputs_this_actor_does_not_know_still_loads() {
    let (engine, dir, id) = seeded(combined_env());
    start(&engine, dir.path(), &id).resume().unwrap();
    let path = dir.path().join("machine.json");
    let mut machine: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    machine["inputs"]["an_input_from_a_later_release"] = serde_json::json!("x");
    machine["inputs"]["control"]["another_later_input"] = serde_json::json!(1);
    std::fs::write(&path, serde_json::to_vec(&machine).unwrap()).unwrap();
    let before = engine.state().by_name();
    start(&engine, dir.path(), &id)
        .resume()
        .expect("unknown machine inputs are ignored");
    assert_eq!(engine.state().by_name(), before);
}

#[test]
fn a_socket_volume_on_another_driver_is_refused_with_the_reason() {
    let (engine, dir, id) = seeded(combined_env());
    engine.with_state(|s| {
        s.volumes
            .get_mut(names::AGENT_SOCKET_VOLUME)
            .unwrap()
            .driver = Some("nfs-plugin".into());
    });
    let err = start(&engine, dir.path(), &id).resume().unwrap_err();
    assert!(
        matches!(&err, ResumeError::Inputs(why) if why.contains("local volume driver") && why.contains("nfs-plugin")),
        "{err}"
    );
    assert!(!dir.path().join("machine.json").exists());
}

/// Host defaults for app containers (an Unraid host's 99/100) are first-install inputs; a
/// machine that names none renders exactly what revision 1 always rendered.
#[test]
fn app_container_defaults_come_from_the_seed_and_reach_the_agent() {
    let agent_env = |env: BTreeMap<String, String>| {
        let engine = Arc::new(FakeEngine::new(seeded_host(env)));
        let dir = tempfile::tempdir().unwrap();
        seed(&engine, dir.path(), SEED_ID).step();
        let id = engine
            .state()
            .container_named(names::RECOVERY_ACTOR)
            .unwrap()
            .id
            .clone();
        start(&engine, dir.path(), &id).resume().unwrap();
        let agent = engine
            .state()
            .container_named(names::NODE_AGENT)
            .unwrap()
            .clone();
        agent.spec.env
    };
    let mut env = seed_env();
    env.insert("QUASAR_APP_PUID".into(), "99".into());
    env.insert("QUASAR_APP_PGID".into(), "100".into());
    env.insert("QUASAR_CONTAINER_NETWORK".into(), "bridge".into());
    let set = agent_env(env);
    assert_eq!(set["QUASAR_APP_PUID"], "99");
    assert_eq!(set["QUASAR_APP_PGID"], "100");
    assert_eq!(set["QUASAR_CONTAINER_NETWORK"], "bridge");

    let unset = agent_env(seed_env());
    assert_eq!(unset["QUASAR_APP_PUID"], "");
    assert_eq!(unset["QUASAR_CONTAINER_NETWORK"], "none");

    for (key, bad) in [
        ("QUASAR_APP_PUID", "nobody"),
        ("QUASAR_CONTAINER_NETWORK", "macvlan"),
    ] {
        let mut env = seed_env();
        env.insert(key.into(), bad.into());
        let engine = Arc::new(FakeEngine::new(seeded_host(env)));
        let dir = tempfile::tempdir().unwrap();
        let refused = seed(&engine, dir.path(), SEED_ID).step();
        assert!(
            matches!(
                &refused,
                Outcome::Idle {
                    token: "seed-inputs-invalid",
                    ..
                }
            ),
            "{key}: {refused:?}"
        );
    }
}
