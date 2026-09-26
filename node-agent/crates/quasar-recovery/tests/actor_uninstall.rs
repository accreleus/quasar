//! The race guard, the console's host removal, the operator's `uninstall [--purge]` and
//! `reconfigure` (#366), against the in-memory engine and a temporary machine-state
//! directory, observed only through engine state, machine state and `status`.

mod support;

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use quasar_recovery::actor::{Actor, ActorConfig, OperatorInputs, ReplaceTiming, TrustConfig};
use quasar_recovery::engine::{
    Behaviour, ContainerSpec, EngineError, ErrorKind, FakeContainer, FakeEngine, FakeState, Fault,
    Image, RestartPolicy, When,
};
use quasar_recovery::recipe::names;
use quasar_recovery::reconfigure::ReconfigureRequest;
use quasar_recovery::seed::Outcome;
use quasar_recovery::socket::{
    Component, MachineRole, Reason, Release, Request, RequestKind, State,
};
use quasar_recovery::trust::{Caller, SignaturePolicy};
use quasar_recovery::uninstall::{Options, Uninstall, UninstallError};
use support::*;

const ID: &str = "7a1f6f1e-2c33-4a58-9a5e-0b6b0f7a1c22";
const ID2: &str = "3c0a6f2e-8d1b-4f7e-9a55-2b8e1c0d9f41";
const REPO: &str = "registry.example.invalid/quasar/quasar-node-agent";
const NEW_AGENT: &str = "registry.example.invalid/quasar/quasar-node-agent@sha256:dd44000000000000000000000000000000000000000000000000000000000000";
const NEW_DIGEST: &str = "sha256:dd44000000000000000000000000000000000000000000000000000000000000";
const CONTROL_IMAGE: &str = "registry.example.invalid/quasar/quasar-control-plane@sha256:aa11000000000000000000000000000000000000000000000000000000000000";
const POSTGRES_IMAGE: &str = "docker.io/library/postgres@sha256:dd55000000000000000000000000000000000000000000000000000000000000";
const NEW_HOME: &str = "/mnt/quasar/homes";

fn fast() -> ReplaceTiming {
    ReplaceTiming {
        verify_timeout: Duration::from_millis(200),
        poll: Duration::from_millis(1),
        stop_grace: Duration::from_secs(1),
        retries: 1,
        retry_backoff: Duration::ZERO,
    }
}

fn configure(mut config: ActorConfig) -> ActorConfig {
    config.now = Box::new(|| NOW.to_string());
    config.gpus_probe_backoff = Duration::ZERO;
    config.healthy_wait = Duration::from_millis(20);
    config.remove_grace = Duration::ZERO;
    config.timing = fast();
    config.trust = TrustConfig {
        allowed_namespaces: vec!["registry.example.invalid/quasar".into()],
        signature: SignaturePolicy::default(),
    };
    config
}

/// A GPU host the seed installed: the seed, its actor, the agent, and `seed.json`.
fn seeded_gpu_host() -> (Arc<FakeEngine>, tempfile::TempDir, String) {
    let engine = Arc::new(FakeEngine::new(seeded_host(seed_env())));
    let dir = tempfile::tempdir().unwrap();
    let created = seed(&engine, dir.path(), SEED_ID).step();
    assert!(matches!(created, Outcome::Created { .. }), "{created:?}");
    let actor_id = actor_container(&engine).id;
    running_actor(&engine, dir.path(), &actor_id)
        .resume()
        .expect("a clean install");
    (engine, dir, actor_id)
}

fn running_actor(engine: &Arc<FakeEngine>, dir: &std::path::Path, actor_id: &str) -> Arc<Actor> {
    let mut config = ActorConfig::new(dir, MachineRole::Gpu, OperatorInputs::default());
    config.self_container = Some(actor_id.into());
    config.seed_container = Some(SEED_ID.into());
    config.new_installation_id = Box::new(|| "an-id-the-actor-must-not-use".to_string());
    Arc::new(Actor::new(engine.clone(), configure(config)))
}

fn actor_container(engine: &FakeEngine) -> FakeContainer {
    engine
        .state()
        .container_named(names::RECOVERY_ACTOR)
        .expect("a recovery actor")
        .clone()
}

fn installation(engine: &FakeEngine) -> String {
    actor_container(engine).spec.labels["io.quasar.installation"].clone()
}

/// Every container this installation created, by name.
fn ours(state: &FakeState, installation: &str) -> Vec<String> {
    let mut out: Vec<String> = state
        .containers
        .values()
        .filter(|c| {
            c.spec
                .labels
                .get("io.quasar.installation")
                .map(String::as_str)
                == Some(installation)
        })
        .map(|c| c.spec.name.clone())
        .collect();
    out.sort();
    out
}

fn container(
    id: &str,
    name: &str,
    image: &str,
    labels: &[(&str, &str)],
    cmd: &[&str],
) -> FakeContainer {
    FakeContainer {
        id: id.into(),
        spec: ContainerSpec {
            name: name.into(),
            image: image.into(),
            entrypoint: None,
            cmd: (!cmd.is_empty()).then(|| cmd.iter().map(|s| s.to_string()).collect()),
            env: BTreeMap::new(),
            labels: labels
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_string()))
                .collect(),
            network_mode: None,
            binds: Vec::new(),
            devices: Vec::new(),
            device_cgroup_rules: Vec::new(),
            gpus: Vec::new(),
            cap_add: Vec::new(),
            security_opt: Vec::new(),
            init: false,
            restart: RestartPolicy::UnlessStopped,
            ports: Vec::new(),
            healthcheck: None,
        },
        status: "running".into(),
        starts: 1,
        restart: RestartPolicy::UnlessStopped,
        exit_code: None,
        logs: String::new(),
        health: None,
    }
}

fn replace_request(id: &str) -> Request {
    Request {
        request_id: id.into(),
        kind: RequestKind::Replace,
        components: vec![Component {
            name: "node-agent".into(),
            image: REPO.into(),
            digest: NEW_DIGEST.into(),
        }],
        release: Release {
            id: "rel-0.4.0".into(),
            version: Some("0.4.0".into()),
            source_commit: "cccccccccccccccccccccccccccccccccccccccc".into(),
        },
        migrates: false,
        schema_version: None,
        external_backup_confirmed: false,
        dump: None,
        purge: false,
        wait_timeout_s: 0,
    }
}

fn remove_request(id: &str) -> Request {
    Request {
        request_id: id.into(),
        kind: RequestKind::Remove,
        components: Vec::new(),
        release: Release {
            id: String::new(),
            version: None,
            source_commit: String::new(),
        },
        migrates: false,
        schema_version: None,
        external_backup_confirmed: false,
        dump: None,
        purge: false,
        wait_timeout_s: 0,
    }
}

fn uninstaller(engine: &Arc<FakeEngine>, dir: &std::path::Path) -> Uninstall {
    let mut run = Uninstall::new(engine.clone(), dir);
    run.now = Box::new(|| NOW.to_string());
    run.stop_grace = Duration::ZERO;
    run.lease_wait = Duration::from_millis(50);
    run
}

// ----- the race guard -----

/// A leftover Compose stack's agent and updater, a manager's definition under the control
/// plane's name, and another installation's actor: all four are conflicts. The actor's own
/// kept container, the seed, the agent's session container and an unrelated container are
/// not.
#[test]
fn look_alikes_are_reported_and_never_acted_on_while_the_actors_own_are_not_conflicts() {
    let (engine, dir, actor_id) = seeded_gpu_host();
    let id = installation(&engine);
    let foreign: Vec<FakeContainer> = vec![
        container(
            "f100000000000000000000000000000000000000000000000000000000000000",
            "quasar-quasar-node-agent-1",
            "ghcr.io/accreleus/quasar/quasar-node-agent:0.3.0",
            &[
                ("com.docker.compose.project", "quasar"),
                ("com.docker.compose.service", "quasar-node-agent"),
            ],
            &[],
        ),
        container(
            "f200000000000000000000000000000000000000000000000000000000000000",
            "quasar-quasar-updater-1",
            "ghcr.io/accreleus/quasar/quasar-updater:0.3.0",
            &[
                ("com.docker.compose.project", "quasar"),
                ("com.docker.compose.service", "quasar-updater"),
            ],
            &[],
        ),
        container(
            "f300000000000000000000000000000000000000000000000000000000000000",
            "quasar-control-plane",
            "ghcr.io/accreleus/quasar/quasar-control-plane:0.3.0",
            &[],
            &[],
        ),
        container(
            "f400000000000000000000000000000000000000000000000000000000000000",
            "other-actor",
            ACTOR_IMAGE,
            &[
                ("io.quasar.installation", "another-installation"),
                ("io.quasar.platform-service", "recovery-actor"),
            ],
            &["actor"],
        ),
    ];
    let benign: Vec<FakeContainer> = vec![
        // The agent's own session and probe containers run its image.
        container(
            "b100000000000000000000000000000000000000000000000000000000000000",
            "quasar-sess-1234",
            AGENT_IMAGE,
            &[("io.quasar.agent-owner", "owner-token")],
            &[],
        ),
        container(
            "b200000000000000000000000000000000000000000000000000000000000000",
            "quasar-probe-egl-9",
            AGENT_IMAGE,
            &[],
            &[],
        ),
        // A replacement's kept container is the actor's own.
        container(
            "b300000000000000000000000000000000000000000000000000000000000000",
            "quasar-node-agent.kept",
            AGENT_IMAGE,
            &[
                ("io.quasar.installation", id.as_str()),
                ("io.quasar.platform-service", "node-agent"),
                ("io.quasar.attempt", ID2),
            ],
            &[],
        ),
        container(
            "b400000000000000000000000000000000000000000000000000000000000000",
            "grafana",
            "docker.io/grafana/grafana:11",
            &[("com.docker.compose.service", "grafana")],
            &[],
        ),
    ];
    engine.with_state(|s| {
        for c in foreign.iter().chain(benign.iter()) {
            s.containers.insert(c.id.clone(), c.clone());
        }
        s.registry.insert(
            NEW_AGENT.into(),
            Image {
                id: "sha256:d0d0000000000000000000000000000000000000000000000000000000000000"
                    .into(),
                repo_digests: vec![NEW_AGENT.into()],
                labels: BTreeMap::from([("org.quasar.recipe".to_string(), "1".to_string())]),
            },
        );
    });
    let actor = running_actor(&engine, dir.path(), &actor_id);

    let status = actor.status();
    let mut reported: Vec<(String, String)> = status
        .conflicts
        .iter()
        .map(|c| (c.container.clone(), c.role.clone()))
        .collect();
    reported.sort();
    assert_eq!(
        reported,
        vec![
            ("other-actor".to_string(), "recovery-actor".to_string()),
            ("quasar-control-plane".into(), "control-plane".into()),
            ("quasar-quasar-node-agent-1".into(), "node-agent".into()),
            ("quasar-quasar-updater-1".into(), "updater".into()),
        ]
    );
    let agent = status
        .conflicts
        .iter()
        .find(|c| c.container == "quasar-quasar-node-agent-1")
        .unwrap();
    assert_eq!(agent.id, "f10000000000");
    assert_eq!(
        agent.image,
        "ghcr.io/accreleus/quasar/quasar-node-agent:0.3.0"
    );
    assert!(
        agent.why.contains("Compose project quasar"),
        "{}",
        agent.why
    );

    // A replacement is refused before anything is journalled or touched.
    let before = engine.state();
    let refused = actor
        .submit(Caller::Agent, replace_request(ID))
        .expect_err("owner_conflict");
    assert_eq!(refused.reason, Reason::OwnerConflict);
    assert!(
        refused.message.contains("quasar-quasar-node-agent-1"),
        "{}",
        refused.message
    );
    assert!(
        refused.message.contains("docker rm -f"),
        "{}",
        refused.message
    );
    let after = engine.state();
    assert_eq!(before.containers, after.containers, "nothing was touched");
    assert!(
        actor.status_for(Some(ID)).result.is_none(),
        "nothing was journalled"
    );

    // Resolved by the operator: the next submit is admitted.
    engine.with_state(|s| {
        for c in &foreign {
            s.containers.remove(&c.id);
        }
    });
    assert!(actor.status().conflicts.is_empty());
    actor
        .submit(
            Caller::Agent,
            replace_request("9d9a1b2c-3d4e-4f50-8a6b-7c8d9e0f1a2b"),
        )
        .expect("admitted");
    actor.wait_attempt();
}

// ----- the console's "remove host" -----

#[test]
fn a_removal_takes_the_agent_then_the_actor_and_nothing_brings_them_back() {
    let (engine, dir, actor_id) = seeded_gpu_host();
    let id = installation(&engine);
    let volumes_before: Vec<String> = engine.state().volumes.keys().cloned().collect();
    let actor = running_actor(&engine, dir.path(), &actor_id);

    let accepted = actor
        .submit(Caller::Agent, remove_request(ID))
        .expect("accepted");
    assert_eq!(accepted.request_id, ID);
    assert!(accepted.previous.is_empty());
    actor.wait_attempt();

    let state = engine.state();
    assert!(
        ours(&state, &id).is_empty(),
        "left: {:?}",
        ours(&state, &id)
    );
    assert!(
        state.containers.contains_key(SEED_ID),
        "the seed is the operator's"
    );
    let volumes_after: Vec<String> = state.volumes.keys().cloned().collect();
    assert_eq!(
        volumes_before, volumes_after,
        "a removal removes containers only"
    );
    let seed_file: serde_json::Value =
        serde_json::from_slice(&std::fs::read(dir.path().join("seed.json")).unwrap()).unwrap();
    assert_eq!(seed_file["state"], "uninstalled");

    // The seed idles, and an actor started anyway installs nothing.
    let looked = seed(&engine, dir.path(), SEED_ID).step();
    assert!(
        matches!(
            looked,
            Outcome::Idle {
                token: "seed-uninstalled",
                ..
            }
        ),
        "{looked:?}"
    );
    let revived = running_actor(&engine, dir.path(), &actor_id);
    revived.resume().expect("resume on an uninstalled machine");
    assert!(ours(&engine.state(), &id).is_empty());
    let refused = revived
        .submit(Caller::Agent, replace_request(ID2))
        .expect_err("nothing is replaced on a removed machine");
    assert_eq!(refused.reason, Reason::Invalid);

    // A re-sent command after a lost ack is the same removal.
    assert_eq!(
        revived
            .submit(Caller::Agent, remove_request(ID))
            .expect("idempotent")
            .request_id,
        ID
    );
}

#[test]
fn a_removal_is_refused_unchanged_off_the_agent_socket_or_carrying_anything() {
    let (engine, dir, actor_id) = seeded_gpu_host();
    let actor = running_actor(&engine, dir.path(), &actor_id);
    let before = engine.state();

    let from_control = actor
        .submit(Caller::ControlPlane, remove_request(ID))
        .expect_err("control socket");
    assert_eq!(from_control.reason, Reason::Invalid);
    let mut purge = remove_request(ID);
    purge.purge = true;
    assert_eq!(
        actor
            .submit(Caller::Agent, purge)
            .expect_err("purge")
            .reason,
        Reason::Invalid
    );
    let mut named = remove_request(ID);
    named.components = replace_request(ID).components;
    assert_eq!(
        actor
            .submit(Caller::Agent, named)
            .expect_err("components")
            .reason,
        Reason::Invalid
    );
    assert_eq!(before.containers, engine.state().containers);
    assert!(!dir.path().join("uninstalled.json").exists());
}

// ----- uninstall -----

#[test]
fn uninstall_removes_the_services_keeps_data_and_the_seed_idles() {
    let (engine, dir, actor_id) = seeded_gpu_host();
    let id = installation(&engine);
    let volumes_before: Vec<String> = engine.state().volumes.keys().cloned().collect();

    let report = uninstaller(&engine, dir.path())
        .run(&Options::default())
        .expect("uninstalled");
    assert!(
        report.lines.iter().any(|l| l.starts_with("Uninstalled.")),
        "{report:?}"
    );
    let state = engine.state();
    assert!(
        ours(&state, &id).is_empty(),
        "left: {:?}",
        ours(&state, &id)
    );
    assert!(state.containers.contains_key(SEED_ID));
    assert_eq!(
        volumes_before,
        state.volumes.keys().cloned().collect::<Vec<_>>()
    );
    assert!(
        dir.path().join("machine.json").exists(),
        "machine state is kept"
    );
    assert!(dir.path().join("secrets").exists());

    let looked = seed(&engine, dir.path(), SEED_ID).step();
    assert!(
        matches!(
            looked,
            Outcome::Idle {
                token: "seed-uninstalled",
                ..
            }
        ),
        "{looked:?}"
    );
    running_actor(&engine, dir.path(), &actor_id)
        .resume()
        .expect("resume");
    assert!(
        ours(&engine.state(), &id).is_empty(),
        "nothing re-installed"
    );

    // A second run is a stated outcome, not an error.
    let again = uninstaller(&engine, dir.path())
        .run(&Options::default())
        .expect("idempotent");
    assert!(again.lines.iter().any(|l| l.starts_with("Uninstalled.")));
}

#[test]
fn uninstall_refuses_to_run_inside_the_recovery_actor() {
    let (engine, dir, actor_id) = seeded_gpu_host();
    let mut run = uninstaller(&engine, dir.path());
    run.self_container = Some(actor_id);
    let refused = run.run(&Options::default()).expect_err("inside the actor");
    assert!(
        matches!(&refused, UninstallError::Refused(why) if why.contains("docker run --rm")),
        "{refused:?}"
    );
    assert!(!dir.path().join("uninstalled.json").exists());
}

#[test]
fn an_interrupted_uninstall_leaves_the_machine_down_and_a_rerun_finishes_it() {
    let (engine, dir, actor_id) = seeded_gpu_host();
    let id = installation(&engine);
    // list, then the actor's restart and stop, then the agent's stop: that one fails.
    let base = engine.calls();
    engine.inject(Fault {
        call: base + 3,
        when: When::Before,
        error: EngineError::Runtime(ErrorKind::Unavailable),
    });
    let stopped = uninstaller(&engine, dir.path())
        .run(&Options::default())
        .expect_err("stopped part-way");
    assert!(matches!(stopped, UninstallError::Stopped(_)), "{stopped:?}");
    let actor = actor_container(&engine);
    assert_eq!(actor.status, "exited");
    assert_eq!(
        actor.restart,
        RestartPolicy::No,
        "nothing restarts the actor"
    );
    // An actor started anyway, and the seed, bring nothing back.
    running_actor(&engine, dir.path(), &actor_id)
        .resume()
        .expect("resume");
    assert!(matches!(
        seed(&engine, dir.path(), SEED_ID).step(),
        Outcome::Idle {
            token: "seed-uninstalled",
            ..
        }
    ));

    engine.clear_faults();
    uninstaller(&engine, dir.path())
        .run(&Options::default())
        .expect("finished on the re-run");
    assert!(ours(&engine.state(), &id).is_empty());
}

#[test]
fn purge_needs_a_typed_confirmation_and_no_seed_and_never_runs_without_them() {
    let (engine, dir, _) = seeded_gpu_host();
    let id = installation(&engine);
    let untouched = |why: &str| {
        let state = engine.state();
        assert!(!ours(&state, &id).is_empty(), "{why}: services removed");
        assert!(
            state.volumes.contains_key(names::AGENT_DATA_VOLUME),
            "{why}: data deleted"
        );
        assert!(
            !dir.path().join("uninstalled.json").exists(),
            "{why}: marked"
        );
    };
    let purge = |confirm: Option<&str>| Options {
        purge: true,
        confirm: confirm.map(str::to_owned),
        dump_to: None,
    };

    let err = uninstaller(&engine, dir.path())
        .run(&purge(None))
        .expect_err("no confirmation");
    assert!(
        matches!(&err, UninstallError::Refused(why) if why.contains("typed confirmation")),
        "{err:?}"
    );
    untouched("no confirmation");

    let mut typed_wrong = uninstaller(&engine, dir.path());
    typed_wrong.prompt = Some(Box::new(|_| Some("not-the-name".into())));
    let err = typed_wrong.run(&purge(None)).expect_err("wrong name typed");
    assert!(
        matches!(&err, UninstallError::Refused(why) if why.contains("did not match")),
        "{err:?}"
    );
    untouched("wrong name");

    let err = uninstaller(&engine, dir.path())
        .run(&purge(Some("gpu-host-01")))
        .expect_err("a seed is running");
    assert!(
        matches!(&err, UninstallError::Refused(why) if why.contains("Remove the seed first")),
        "{err:?}"
    );
    untouched("seed present");

    engine.with_state(|s| {
        s.containers.remove(SEED_ID);
    });
    let mut typed = uninstaller(&engine, dir.path());
    typed.prompt = Some(Box::new(|_| Some("gpu-host-01".into())));
    let report = typed.run(&purge(None)).expect("purged");
    assert!(
        report.lines.iter().any(|l| l.starts_with("Purged.")),
        "{report:?}"
    );
    let state = engine.state();
    assert!(ours(&state, &id).is_empty());
    for v in [
        names::AGENT_DATA_VOLUME,
        names::NODE_AGENT_SECRETS_VOLUME,
        names::AGENT_SOCKET_VOLUME,
    ] {
        assert!(!state.volumes.contains_key(v), "{v} survived a purge");
    }
    let left: Vec<String> = std::fs::read_dir(dir.path())
        .unwrap()
        .map(|e| e.unwrap().file_name().into_string().unwrap())
        .collect();
    assert_eq!(
        left,
        vec!["actor.lease".to_string()],
        "machine state is emptied"
    );
}

/// A combined host the seed installed: Postgres, the control plane and its own agent.
fn seeded_combined_host() -> (Arc<FakeEngine>, tempfile::TempDir, String) {
    let env = BTreeMap::from([
        ("QUASAR_ROLE".to_string(), "combined".to_string()),
        ("QUASAR_HOME_ROOT".into(), HOME.into()),
        ("QUASAR_AGENT_IMAGE".into(), AGENT_IMAGE.into()),
        ("QUASAR_CONTROL_PLANE_IMAGE".into(), CONTROL_IMAGE.into()),
        ("QUASAR_POSTGRES_IMAGE".into(), POSTGRES_IMAGE.into()),
    ]);
    let mut state = seeded_host(env);
    state
        .registry
        .insert(AGENT_IMAGE.into(), agent_image(Some("2")));
    for (reference, id, recipe) in [
        (
            CONTROL_IMAGE,
            "sha256:c0c0000000000000000000000000000000000000000000000000000000000000",
            Some("1"),
        ),
        (
            POSTGRES_IMAGE,
            "sha256:9090000000000000000000000000000000000000000000000000000000000000",
            None,
        ),
    ] {
        state.registry.insert(
            reference.into(),
            Image {
                id: id.into(),
                repo_digests: vec![reference.into()],
                labels: recipe
                    .map(|r| BTreeMap::from([("org.quasar.recipe".to_string(), r.to_string())]))
                    .unwrap_or_default(),
            },
        );
        state.behaviour.insert(
            reference.into(),
            Behaviour {
                health: Some("healthy".into()),
                ..Default::default()
            },
        );
    }
    let engine = Arc::new(FakeEngine::new(state));
    let dir = tempfile::tempdir().unwrap();
    assert!(matches!(
        seed(&engine, dir.path(), SEED_ID).step(),
        Outcome::Created { .. }
    ));
    let actor_id = actor_container(&engine).id;
    running_actor(&engine, dir.path(), &actor_id)
        .resume()
        .expect("a combined install");
    (engine, dir, actor_id)
}

#[test]
fn purging_a_combined_host_takes_a_final_dump_first_and_keeps_it() {
    let (engine, dir, _) = seeded_combined_host();
    let id = installation(&engine);
    assert!(engine.state().container_named(names::POSTGRES).is_some());
    engine.with_state(|s| {
        s.containers.remove(SEED_ID);
    });
    let node = "gpu-host-01";

    // Without the Postgres image the dump helper cannot run: nothing is deleted.
    let image = engine
        .with_state(|s| s.images.remove(POSTGRES_IMAGE))
        .unwrap();
    let err = uninstaller(&engine, dir.path())
        .run(&Options {
            purge: true,
            confirm: Some(node.into()),
            dump_to: None,
        })
        .expect_err("the dump failed");
    assert!(
        matches!(&err, UninstallError::Stopped(why) if why.contains("no data was deleted")),
        "{err:?}"
    );
    assert!(engine
        .state()
        .volumes
        .contains_key(names::POSTGRES_DATA_VOLUME));
    assert!(dir.path().join("machine.json").exists());

    engine.with_state(|s| {
        s.images.insert(POSTGRES_IMAGE.into(), image);
    });
    let report = uninstaller(&engine, dir.path())
        .run(&Options {
            purge: true,
            confirm: Some(node.into()),
            dump_to: None,
        })
        .expect("purged");
    assert!(
        report
            .lines
            .iter()
            .any(|l| l.contains("quasar-final-") && l.contains(names::FINAL_DUMP_VOLUME)),
        "{report:?}"
    );
    let state = engine.state();
    assert!(ours(&state, &id).is_empty());
    assert!(!state.volumes.contains_key(names::POSTGRES_DATA_VOLUME));
    assert!(
        state.volumes.contains_key(names::FINAL_DUMP_VOLUME),
        "the dump survives the purge"
    );
    assert!(
        state.container_named(names::FINAL_DUMP).is_none(),
        "the helper is removed"
    );
    assert!(!state.networks.contains_key(names::PLATFORM_NETWORK));
}

#[test]
fn a_combined_host_is_never_removed_over_the_agent_socket() {
    let (engine, dir, actor_id) = seeded_combined_host();
    let actor = running_actor(&engine, dir.path(), &actor_id);
    let refused = actor
        .submit(Caller::Agent, remove_request(ID))
        .expect_err("combined");
    assert_eq!(refused.reason, Reason::Invalid);
    assert!(refused.message.contains("uninstall"), "{}", refused.message);
    assert!(engine
        .state()
        .container_named(names::CONTROL_PLANE)
        .is_some());
}

#[test]
fn purge_by_installation_id_removes_what_an_install_left_before_it_wrote_machine_state() {
    let engine = Arc::new(FakeEngine::new(seeded_host(seed_env())));
    let dir = tempfile::tempdir().unwrap();
    assert!(matches!(
        seed(&engine, dir.path(), SEED_ID).step(),
        Outcome::Created { .. }
    ));
    let id = installation(&engine);
    engine.with_state(|s| {
        s.containers.remove(SEED_ID);
    });
    assert!(!dir.path().join("machine.json").exists());
    let purge = |confirm: Option<String>| Options {
        purge: true,
        confirm,
        dump_to: None,
    };

    let report = uninstaller(&engine, dir.path())
        .run(&purge(None))
        .expect("nothing is known without the id");
    assert!(report.lines[0].contains("nothing to remove"), "{report:?}");
    assert!(!ours(&engine.state(), &id).is_empty());

    uninstaller(&engine, dir.path())
        .run(&purge(Some(id.clone())))
        .expect("purged by id");
    assert!(ours(&engine.state(), &id).is_empty());
}

#[test]
fn a_purge_may_be_confirmed_with_the_installation_id() {
    let (engine, dir, _) = seeded_gpu_host();
    let id = installation(&engine);
    engine.with_state(|s| {
        s.containers.remove(SEED_ID);
    });
    uninstaller(&engine, dir.path())
        .run(&Options {
            purge: true,
            confirm: Some(id.clone()),
            dump_to: None,
        })
        .expect("purged");
    assert!(ours(&engine.state(), &id).is_empty());
}

// ----- reconfigure -----

fn changes(pairs: &[(&str, &str)]) -> ReconfigureRequest {
    ReconfigureRequest {
        changes: pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect(),
        dry_run: false,
    }
}

fn home_of(engine: &FakeEngine) -> Option<String> {
    engine
        .state()
        .container_named(names::NODE_AGENT)?
        .spec
        .env
        .get("QUASAR_HOME_ROOT")
        .cloned()
}

fn machine_home(dir: &std::path::Path) -> String {
    let m: serde_json::Value =
        serde_json::from_slice(&std::fs::read(dir.join("machine.json")).unwrap()).unwrap();
    m["inputs"]["home_root"].as_str().unwrap().to_owned()
}

#[test]
fn reconfiguring_the_home_root_replaces_the_agent_on_the_same_image() {
    let (engine, dir, actor_id) = seeded_gpu_host();
    let actor = running_actor(&engine, dir.path(), &actor_id);
    let old = engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone();

    let mut dry = changes(&[("QUASAR_HOME_ROOT", NEW_HOME)]);
    dry.dry_run = true;
    let plan = actor.reconfigure(dry).expect("planned");
    assert_eq!(plan.changed, vec!["QUASAR_HOME_ROOT".to_string()]);
    assert_eq!(plan.replaced, vec!["node-agent".to_string()]);
    assert!(plan.request_id.is_none());
    assert_eq!(
        home_of(&engine).as_deref(),
        Some(HOME),
        "a dry run changes nothing"
    );

    let done = actor
        .reconfigure(changes(&[("QUASAR_HOME_ROOT", NEW_HOME)]))
        .expect("admitted");
    let request = done.request_id.expect("a replacement");
    actor.wait_attempt();

    let result = actor.status_for(Some(&request)).result.expect("journalled");
    assert_eq!(result.state, State::Succeeded, "{}", result.output);
    let new = engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone();
    assert_ne!(new.id, old.id);
    assert_eq!(new.spec.image, old.spec.image, "the same digest");
    assert_eq!(home_of(&engine).as_deref(), Some(NEW_HOME));
    assert!(new.spec.binds.iter().any(|b| b.source == NEW_HOME));
    assert_eq!(machine_home(dir.path()), NEW_HOME);
    assert!(!dir.path().join("reconfigure.json").exists());
    assert!(engine
        .state()
        .container_named("quasar-node-agent.kept")
        .is_none());
}

#[test]
fn a_reconfigure_whose_agent_does_not_verify_puts_back_the_agent_and_the_inputs() {
    let (engine, dir, actor_id) = seeded_gpu_host();
    let actor = running_actor(&engine, dir.path(), &actor_id);
    let old = engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone();
    engine.with_state(|s| {
        s.behaviour.insert(
            AGENT_IMAGE.into(),
            Behaviour {
                health: Some("unhealthy".into()),
                ..Default::default()
            },
        );
    });

    let request = actor
        .reconfigure(changes(&[("QUASAR_HOME_ROOT", NEW_HOME)]))
        .expect("admitted")
        .request_id
        .unwrap();
    actor.wait_attempt();

    let result = actor.status_for(Some(&request)).result.unwrap();
    assert_eq!(result.state, State::Failed);
    assert!(result.restored);
    let back = engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone();
    assert_eq!(back.id, old.id, "the kept container is back");
    assert_eq!(home_of(&engine).as_deref(), Some(HOME));
    assert_eq!(
        machine_home(dir.path()),
        HOME,
        "the old inputs are back in force"
    );
    assert!(!dir.path().join("reconfigure.json").exists());
}

#[test]
fn a_reconfigure_interrupted_before_the_agent_was_touched_settles_as_nothing_changed() {
    let (engine, dir, actor_id) = seeded_gpu_host();
    let actor = running_actor(&engine, dir.path(), &actor_id);
    let old = engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone();
    // The reconfigure's list and inspect, then the attempt's first look at the image.
    let base = engine.calls();
    engine.inject(Fault {
        call: base + 2,
        when: When::Before,
        error: EngineError::Crashed,
    });
    let request = actor
        .reconfigure(changes(&[("QUASAR_HOME_ROOT", NEW_HOME)]))
        .expect("admitted")
        .request_id
        .unwrap();
    actor.wait_attempt();
    assert_eq!(machine_home(dir.path()), NEW_HOME, "mid-attempt");
    engine.clear_faults();

    let restarted = running_actor(&engine, dir.path(), &actor_id);
    restarted.resume().expect("settled on the next start");
    let result = restarted.status_for(Some(&request)).result.unwrap();
    assert_eq!(result.reason, Some(Reason::Interrupted));
    assert_eq!(machine_home(dir.path()), HOME, "the old inputs are back");
    assert_eq!(
        engine
            .state()
            .container_named(names::NODE_AGENT)
            .unwrap()
            .id,
        old.id
    );
    assert!(!dir.path().join("reconfigure.json").exists());
}

#[test]
fn a_reconfigure_that_moves_no_container_is_recorded_and_one_that_cannot_be_applied_changes_nothing(
) {
    let (engine, dir, actor_id) = seeded_gpu_host();
    let actor = running_actor(&engine, dir.path(), &actor_id);
    let old = engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .id
        .clone();

    let plan = actor
        .reconfigure(changes(&[("QUASAR_UPDATER_SIGNATURE_MODE", "verify")]))
        .expect("recorded");
    assert!(plan.replaced.is_empty() && plan.request_id.is_none());
    let m: serde_json::Value =
        serde_json::from_slice(&std::fs::read(dir.path().join("machine.json")).unwrap()).unwrap();
    assert_eq!(m["inputs"]["trust"]["signature_mode"], "verify");
    assert_eq!(
        engine
            .state()
            .container_named(names::NODE_AGENT)
            .unwrap()
            .id,
        old
    );

    for (key, value, why) in [
        ("QUASAR_NODE_NAME", "another", "identity"),
        ("QUASAR_AGENT_IMAGE", NEW_AGENT, "update"),
        ("QUASAR_PUBLIC_HOST", "quasar.example.invalid", "runs none"),
        ("QUASAR_HOME_ROOT", "relative/path", "absolute"),
    ] {
        let refused = actor.reconfigure(changes(&[(key, value)])).expect_err(key);
        assert_eq!(refused.reason, Reason::Invalid, "{key}");
        assert!(refused.message.contains(why), "{key}: {}", refused.message);
    }
    assert_eq!(machine_home(dir.path()), HOME);
}

#[test]
fn a_combined_host_home_root_reconfigure_is_refused_until_control_plane_replacement_exists() {
    let (engine, dir, actor_id) = seeded_combined_host();
    let actor = running_actor(&engine, dir.path(), &actor_id);
    let refused = actor
        .reconfigure(changes(&[("QUASAR_HOME_ROOT", NEW_HOME)]))
        .expect_err("moves the control plane");
    assert_eq!(refused.reason, Reason::Invalid);
    assert!(refused.message.contains("#363"), "{}", refused.message);
    assert_eq!(machine_home(dir.path()), HOME);
    assert!(!dir.path().join("reconfigure.json").exists());
}
