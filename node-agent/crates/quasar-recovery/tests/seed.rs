//! The seed (ADR 0007) against the in-memory engine and a temporary machine-state
//! directory, observed through engine state, machine state, the seed's log and the
//! recovery actor it creates.

mod support;

use std::collections::BTreeMap;
use std::io::Write;
use std::sync::{Arc, Mutex};

use quasar_recovery::engine::{EngineError, ErrorKind, FakeEngine, FakeState, Fault, Image, When};
use quasar_recovery::recipe::names;
use quasar_recovery::seed::file::{ActorImage, SeedFile, SeedState};
use quasar_recovery::seed::{profile, Outcome};
use support::*;

fn new_machine(env: BTreeMap<String, String>) -> (Arc<FakeEngine>, tempfile::TempDir) {
    (
        Arc::new(FakeEngine::new(seeded_host(env))),
        tempfile::tempdir().unwrap(),
    )
}

/// Every recovery actor on the engine, by any name: containers carrying both labels.
fn actors(state: &FakeState) -> Vec<(String, String, String)> {
    state
        .containers
        .values()
        .filter(|c| {
            c.spec
                .labels
                .get("io.quasar.platform-service")
                .map(String::as_str)
                == Some("recovery-actor")
        })
        .map(|c| (c.spec.name.clone(), c.spec.image.clone(), c.status.clone()))
        .collect()
}

fn actor_id(state: &FakeState) -> String {
    state
        .container_named(names::RECOVERY_ACTOR)
        .unwrap()
        .id
        .clone()
}

/// First start of the seed, then the actor it created running its first start.
fn installed() -> (Arc<FakeEngine>, tempfile::TempDir) {
    let (engine, dir) = new_machine(seed_env());
    let created = seed(&engine, dir.path(), SEED_ID).step();
    assert!(matches!(created, Outcome::Created { .. }), "{created:?}");
    let id = actor_id(&engine.state());
    seeded_actor(&engine, dir.path(), &id)
        .resume()
        .expect("the seed-created actor installs the machine");
    (engine, dir)
}

fn seed_file(dir: &tempfile::TempDir) -> SeedFile {
    serde_json::from_slice(&std::fs::read(dir.path().join("seed.json")).unwrap()).unwrap()
}

#[test]
fn a_first_start_creates_exactly_one_recovery_actor_from_the_seeds_own_image_and_profile() {
    let (engine, dir) = new_machine(seed_env());
    let outcome = seed(&engine, dir.path(), SEED_ID).step();
    assert_eq!(
        outcome,
        Outcome::Created {
            id: actor_id(&engine.state()),
            image: ACTOR_IMAGE.into(),
            installation_id: INSTALLATION.into(),
        }
    );

    let state = engine.state();
    assert_eq!(
        actors(&state),
        [(
            names::RECOVERY_ACTOR.to_string(),
            ACTOR_IMAGE.to_string(),
            "running".to_string()
        )]
    );
    let actor = state.container_named(names::RECOVERY_ACTOR).unwrap();
    let image = ActorImage::parse(ACTOR_IMAGE).unwrap();
    assert_eq!(
        actor.spec,
        profile::actor(INSTALLATION, &image, SOCKET_HOST_PATH, SEED_ID),
        "the actor is exactly the compiled profile, with the engine socket at the seed's own host path"
    );
    // The seed never writes machine state, and creates nothing but the actor.
    assert!(std::fs::read_dir(dir.path()).unwrap().next().is_none());
    assert_eq!(state.containers.len(), 2);
}

#[test]
fn a_restarted_or_redeployed_seed_with_an_actor_present_does_nothing() {
    let (engine, dir) = installed();
    let before = engine.state();
    let files = contents(dir.path());

    // Restarted: the same container.
    let outcome = seed(&engine, dir.path(), SEED_ID).step();
    assert!(matches!(outcome, Outcome::Present { .. }), "{outcome:?}");
    assert_eq!(engine.state(), before);

    // Redeployed by its manager: a new container, possibly from a newer image.
    let redeployed = "5eed100000000000000000000000000000000000000000000000000000000000";
    engine.with_state(|s| {
        s.containers.remove(SEED_ID);
        s.images.insert(
            NEWER_IMAGE.into(),
            Image {
                id: "sha256:dd4a000000000000000000000000000000000000000000000000000000000000"
                    .into(),
                repo_digests: vec![NEWER_IMAGE.into()],
                labels: BTreeMap::new(),
            },
        );
        s.containers.insert(
            redeployed.into(),
            seed_container(redeployed, NEWER_IMAGE, seed_env()),
        );
    });
    let before = engine.state();
    let outcome = seed(&engine, dir.path(), redeployed).step();
    assert!(matches!(outcome, Outcome::Present { .. }), "{outcome:?}");
    assert_eq!(
        engine.state(),
        before,
        "a redeployed seed changed the engine"
    );
    assert_eq!(contents(dir.path()), files);
}

#[test]
fn the_actor_a_seed_creates_installs_from_the_seeds_inputs_and_records_the_seed_file() {
    let (engine, dir) = installed();
    let state = engine.state();

    let agent = state.container_named(names::NODE_AGENT).unwrap();
    assert_eq!(agent.status, "running");
    assert_eq!(agent.spec.image, AGENT_IMAGE);
    // One installation: the id the seed chose is the actor's, the agent's and seed.json's.
    assert_eq!(agent.spec.labels["io.quasar.installation"], INSTALLATION);
    let machine: serde_json::Value =
        serde_json::from_slice(&std::fs::read(dir.path().join("machine.json")).unwrap()).unwrap();
    assert_eq!(machine["installation_id"], INSTALLATION);
    assert_eq!(
        state.volumes[names::NODE_AGENT_SECRETS_VOLUME].files["enrollment"].0,
        ENROLLMENT.as_bytes()
    );
    // The secret stayed in the seed's environment; the actor's has none.
    let actor = state.container_named(names::RECOVERY_ACTOR).unwrap();
    assert!(actor.spec.env.values().all(|v| !v.contains(ENROLLMENT)));

    assert_eq!(
        seed_file(&dir),
        SeedFile {
            format_version: 1,
            installation_id: INSTALLATION.into(),
            recovery_actor_image: ActorImage::parse(ACTOR_IMAGE).unwrap(),
            state: SeedState::Active,
        }
    );
}

#[test]
fn the_actor_reports_the_seeds_version_and_digest_and_no_seed_once_it_is_gone() {
    let (engine, dir) = installed();
    let id = actor_id(&engine.state());
    let actor = seeded_actor(&engine, dir.path(), &id);

    let status = actor.status();
    let seed = status.seed.expect("the seed is found");
    assert_eq!(seed.version, SEED_VERSION);
    assert_eq!(
        seed.digest.as_deref(),
        Some("sha256:cc33000000000000000000000000000000000000000000000000000000000000")
    );

    // Redeployed under a new id and name: still found, by what it runs.
    let redeployed = "5eed200000000000000000000000000000000000000000000000000000000000";
    engine.with_state(|s| {
        let mut moved = s.containers.remove(SEED_ID).unwrap();
        moved.id = redeployed.into();
        moved.spec.name = "dockge-quasar-seed-1".into();
        s.containers.insert(redeployed.into(), moved);
    });
    assert_eq!(
        actor.status().seed.map(|s| s.version).as_deref(),
        Some(SEED_VERSION)
    );

    engine.with_state(|s| {
        s.containers.remove(redeployed);
    });
    assert_eq!(
        actor.status().seed,
        None,
        "a removed seed is reported absent"
    );
}

#[test]
fn a_deleted_actor_is_re_created_from_the_verified_digest_and_nothing_else_moves() {
    let (engine, dir) = installed();
    let agent_before = engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone();
    let files = contents(dir.path());

    // The manager has since updated the seed to a newer image, and the verified actor
    // image is no longer on the engine: the seed still uses seed.json's digest, pulled.
    engine.with_state(|s| {
        let id = s.container_named(names::RECOVERY_ACTOR).unwrap().id.clone();
        s.containers.remove(&id);
        let verified = s.images.remove(ACTOR_IMAGE).unwrap();
        s.registry.insert(ACTOR_IMAGE.into(), verified);
        s.images.insert(
            NEWER_IMAGE.into(),
            Image {
                id: "sha256:dd4a000000000000000000000000000000000000000000000000000000000000"
                    .into(),
                repo_digests: vec![NEWER_IMAGE.into()],
                labels: BTreeMap::new(),
            },
        );
        s.containers.get_mut(SEED_ID).unwrap().spec.image = NEWER_IMAGE.into();
    });

    let outcome = seed(&engine, dir.path(), SEED_ID).step();
    assert!(
        matches!(&outcome, Outcome::Created { image, installation_id, .. }
            if image == ACTOR_IMAGE && installation_id == INSTALLATION),
        "{outcome:?}"
    );
    let state = engine.state();
    assert_eq!(
        actors(&state),
        [(
            names::RECOVERY_ACTOR.to_string(),
            ACTOR_IMAGE.to_string(),
            "running".to_string()
        )]
    );
    assert_eq!(
        state.container_named(names::NODE_AGENT).unwrap(),
        &agent_before
    );

    // The re-created actor starts from the profile on the installed machine: nothing to do.
    let id = actor_id(&state);
    let before = engine.state();
    seeded_actor(&engine, dir.path(), &id).resume().unwrap();
    assert_eq!(engine.state().by_name(), before.by_name());
    assert_eq!(contents(dir.path()), files, "machine state was rewritten");
}

#[test]
fn the_seed_never_replaces_or_starts_an_existing_actor() {
    let (engine, dir) = installed();
    let id = actor_id(&engine.state());

    // Stopped by the operator.
    engine.with_state(|s| s.containers.get_mut(&id).unwrap().status = "exited".into());
    let before = engine.state();
    assert!(matches!(
        seed(&engine, dir.path(), SEED_ID).step(),
        Outcome::Present { .. }
    ));
    assert_eq!(engine.state(), before);

    // Mid hand-over: only a kept actor and a stopped successor, under other names.
    engine.with_state(|s| {
        let mut kept = s.containers.remove(&id).unwrap();
        kept.spec.name = "quasar-recovery.kept".into();
        let mut next = kept.clone();
        next.id = "ac10000000000000000000000000000000000000000000000000000000000000".into();
        next.spec.name = "quasar-recovery.next".into();
        next.status = "created".into();
        s.containers.insert(kept.id.clone(), kept);
        s.containers.insert(next.id.clone(), next);
    });
    let before = engine.state();
    assert!(matches!(
        seed(&engine, dir.path(), SEED_ID).step(),
        Outcome::Present { .. }
    ));
    assert_eq!(
        engine.state(),
        before,
        "a hand-over's containers were touched"
    );
}

#[test]
fn a_container_holding_the_actor_name_without_the_labels_is_left_alone() {
    let (engine, dir) = new_machine(seed_env());
    engine.with_state(|s| {
        let mut stranger = seed_container(ACTOR_ID, ACTOR_IMAGE, BTreeMap::new());
        stranger.spec.name = names::RECOVERY_ACTOR.into();
        stranger.spec.cmd = Some(vec!["actor".into()]);
        s.containers.insert(ACTOR_ID.into(), stranger);
    });
    let before = engine.state();
    let outcome = seed(&engine, dir.path(), SEED_ID).step();
    assert!(
        matches!(
            &outcome,
            Outcome::Idle {
                token: "seed-name-taken",
                ..
            }
        ),
        "{outcome:?}"
    );
    assert_eq!(engine.state(), before);
}

/// A tracing writer into a buffer the test reads back.
#[derive(Clone, Default)]
struct Captured(Arc<Mutex<Vec<u8>>>);

impl Write for Captured {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(buf);
        Ok(buf.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// Runs `looks` seed looks under a subscriber and returns what they logged.
fn logged(engine: &Arc<FakeEngine>, dir: &std::path::Path, looks: usize) -> (Vec<Outcome>, String) {
    let captured = Captured::default();
    let writer = captured.clone();
    let subscriber = tracing_subscriber::fmt()
        .with_writer(move || writer.clone())
        .with_ansi(false)
        .finish();
    let outcomes = tracing::subscriber::with_default(subscriber, || {
        let mut seed = seed(engine, dir, SEED_ID);
        (0..looks).map(|_| seed.step()).collect()
    });
    let text = String::from_utf8(captured.0.lock().unwrap().clone()).unwrap();
    (outcomes, text)
}

#[test]
fn invalid_bootstrap_inputs_log_one_clear_line_and_leave_an_idle_seed_and_nothing_installed() {
    let cases: [(&str, &str, Option<&str>); 6] = [
        ("QUASAR_ENROLLMENT", "QUASAR_ENROLLMENT", None),
        (
            "QUASAR_ENROLLMENT",
            "QUASAR_ENROLLMENT",
            Some("not-a-token"),
        ),
        ("QUASAR_HOME_ROOT", "QUASAR_HOME_ROOT", None),
        ("home root", "QUASAR_HOME_ROOT", Some("relative/homes")),
        (
            "QUASAR_AGENT_IMAGE",
            "QUASAR_AGENT_IMAGE",
            Some("registry.example.invalid/quasar/quasar-node-agent:latest"),
        ),
        ("QUASAR_ROLE", "QUASAR_ROLE", Some("sideways")),
    ];
    for (named, key, value) in cases {
        let mut env = seed_env();
        match value {
            Some(v) => env.insert(key.into(), v.into()),
            None => env.remove(key),
        };
        let (engine, dir) = new_machine(env);
        let before = engine.state();

        let (outcomes, log) = logged(&engine, dir.path(), 3);
        for outcome in &outcomes {
            assert!(
                matches!(outcome, Outcome::Idle { token: "seed-inputs-invalid", why } if why.contains(named)),
                "{key}={value:?}: {outcome:?}"
            );
        }
        let lines: Vec<_> = log
            .lines()
            .filter(|l| l.contains("seed-inputs-invalid"))
            .collect();
        assert_eq!(lines.len(), 1, "{key}={value:?}: {log}");
        assert!(lines[0].contains("ERROR"), "{}", lines[0]);
        assert_eq!(
            engine.state(),
            before,
            "{key}={value:?}: something was created"
        );
        assert!(std::fs::read_dir(dir.path()).unwrap().next().is_none());
    }
}

#[test]
fn inputs_are_not_needed_to_re_create_an_installed_machines_actor() {
    let (engine, dir) = installed();
    engine.with_state(|s| {
        let id = s.container_named(names::RECOVERY_ACTOR).unwrap().id.clone();
        s.containers.remove(&id);
        // The single-use enrollment string was long since removed from the stack.
        s.containers.get_mut(SEED_ID).unwrap().spec.env.clear();
    });
    let outcome = seed(&engine, dir.path(), SEED_ID).step();
    assert!(matches!(outcome, Outcome::Created { .. }), "{outcome:?}");
}

#[test]
fn an_uninstalled_marker_keeps_the_seed_idle() {
    let (engine, dir) = installed();
    let mut file = seed_file(&dir);
    file.state = SeedState::Uninstalled;
    std::fs::write(
        dir.path().join("seed.json"),
        serde_json::to_vec(&file).unwrap(),
    )
    .unwrap();
    engine.with_state(|s| {
        s.containers.retain(|_, c| c.spec.name == SEED_NAME);
    });
    let before = engine.state();

    let (outcomes, log) = logged(&engine, dir.path(), 3);
    for outcome in outcomes {
        assert!(
            matches!(
                outcome,
                Outcome::Idle {
                    token: "seed-uninstalled",
                    ..
                }
            ),
            "{outcome:?}"
        );
    }
    assert_eq!(log.matches("seed-uninstalled").count(), 1, "{log}");
    assert_eq!(engine.state(), before);
}

#[test]
fn a_seed_file_it_cannot_read_keeps_the_seed_idle_and_is_never_guessed_at() {
    for (bytes, token) in [
        (&br#"{"format_version":2,"anything":"new"}"#[..], "seed-file-unknown-format"),
        (&br#"{"installation_id":"x"}"#[..], "seed-file-unknown-format"),
        (&b"{not json"[..], "seed-file-unreadable"),
        (
            &br#"{"format_version":1,"installation_id":"5f0c1e0e-0c5a-4d1b-9a2f-3e4d5c6b7a89","recovery_actor_image":{"repository":"r/quasar-recovery","digest":"sha256:cc33000000000000000000000000000000000000000000000000000000000000"},"state":"paused"}"#[..],
            "seed-file-unreadable",
        ),
        (
            &br#"{"format_version":1,"installation_id":"5f0c1e0e-0c5a-4d1b-9a2f-3e4d5c6b7a89","recovery_actor_image":{"repository":"r/quasar-recovery","digest":"latest"},"state":"active"}"#[..],
            "seed-file-unreadable",
        ),
    ] {
        let (engine, dir) = new_machine(seed_env());
        std::fs::write(dir.path().join("seed.json"), bytes).unwrap();
        let before = engine.state();
        let outcome = seed(&engine, dir.path(), SEED_ID).step();
        assert!(
            matches!(&outcome, Outcome::Idle { token: t, .. } if *t == token),
            "{}: {outcome:?}",
            String::from_utf8_lossy(bytes)
        );
        assert_eq!(engine.state(), before);
    }
}

#[test]
fn a_seed_that_cannot_create_a_correct_actor_says_why_and_creates_nothing() {
    let without_machine = |s: &mut FakeState| {
        s.containers
            .get_mut(SEED_ID)
            .unwrap()
            .spec
            .binds
            .retain(|b| b.source != names::MACHINE_VOLUME);
    };
    let without_socket = |s: &mut FakeState| {
        s.containers
            .get_mut(SEED_ID)
            .unwrap()
            .spec
            .binds
            .retain(|b| b.target != "/var/run/docker.sock");
    };
    let local_tag = |s: &mut FakeState| {
        s.images.insert(
            "quasar-recovery:local".into(),
            Image {
                id: "sha256:10ca000000000000000000000000000000000000000000000000000000000000"
                    .into(),
                repo_digests: Vec::new(),
                labels: BTreeMap::new(),
            },
        );
        s.containers.get_mut(SEED_ID).unwrap().spec.image = "quasar-recovery:local".into();
    };
    for (what, change) in [
        (
            "quasar-machine",
            &without_machine as &dyn Fn(&mut FakeState),
        ),
        ("engine socket", &without_socket),
        ("by digest", &local_tag),
    ] {
        let (engine, dir) = new_machine(seed_env());
        engine.with_state(|s| change(s));
        let before = engine.state();
        let outcome = seed(&engine, dir.path(), SEED_ID).step();
        assert!(
            matches!(&outcome, Outcome::Idle { token: "seed-self-invalid", why } if why.contains(what)),
            "{what}: {outcome:?}"
        );
        assert_eq!(engine.state(), before, "{what}");
    }
}

#[test]
fn a_seed_started_by_tag_pins_the_actor_to_its_registry_digest() {
    let (engine, dir) = new_machine(seed_env());
    engine.with_state(|s| {
        let image = s.images[ACTOR_IMAGE].clone();
        s.images.insert(
            "registry.example.invalid/quasar/quasar-recovery:dev-abc".into(),
            image,
        );
        s.containers.get_mut(SEED_ID).unwrap().spec.image =
            "registry.example.invalid/quasar/quasar-recovery:dev-abc".into();
    });
    let outcome = seed(&engine, dir.path(), SEED_ID).step();
    assert!(
        matches!(&outcome, Outcome::Created { image, .. } if image == ACTOR_IMAGE),
        "{outcome:?}"
    );
}

/// An engine that fails any one call of the seed's first look, before or after the call
/// takes effect: the looks that follow always end with exactly one running actor.
#[test]
fn a_failed_engine_call_during_a_first_look_converges_to_exactly_one_running_actor() {
    let (reference, _dir) = new_machine(seed_env());
    seed(&reference, _dir.path(), SEED_ID).step();
    let calls = reference.calls();
    assert!(calls >= 5, "the sweep must not be vacuous ({calls} calls)");

    for call in 0..calls {
        for when in [When::Before, When::After] {
            for error in [
                EngineError::Runtime(ErrorKind::UnknownOutcome),
                EngineError::Crashed,
            ] {
                let at = format!("call {call} {when:?} {error:?}");
                let (engine, dir) = new_machine(seed_env());
                engine.inject(Fault {
                    call,
                    when,
                    error: error.clone(),
                });
                seed(&engine, dir.path(), SEED_ID).step();
                engine.clear_faults();
                // A crash is a new seed process in the same container; a failure the same one.
                let mut next = seed(&engine, dir.path(), SEED_ID);
                next.step();
                next.step();
                assert_eq!(
                    actors(&engine.state()),
                    [(
                        names::RECOVERY_ACTOR.to_string(),
                        ACTOR_IMAGE.to_string(),
                        "running".to_string()
                    )],
                    "{at}"
                );
            }
        }
    }
}
