//! The relay on an owned install (#360), against the real recovery actor: its agent-socket
//! server, `submit`/`status`, and the in-memory engine. The socket is the interface, so
//! nothing in between is mocked.

use std::collections::{BTreeMap, BTreeSet};
use std::time::Duration;

use quasar_recovery::actor::{
    Actor as RecoveryActor, ActorConfig, OperatorInputs, ReplaceTiming, TrustConfig,
};
use quasar_recovery::engine::{
    Behaviour, ContainerSpec, EngineHost, FakeContainer, FakeEngine, FakeState, FakeVolume, Image,
    RestartPolicy,
};
use quasar_recovery::recipe::{names, paths, Bind};
use quasar_recovery::server;

use super::*;

const REQ: &str = "7a1f6f1e-2c33-4a58-9a5e-0b6b0f7a1c22";
const REQ2: &str = "3c0a6f2e-8d1b-4f7e-9a55-2b8e1c0d9f41";
const REPO: &str = "registry.example.invalid/quasar/quasar-node-agent";
const OLD: &str = "sha256:bb22000000000000000000000000000000000000000000000000000000000000";
const NEW: &str = "sha256:dd44000000000000000000000000000000000000000000000000000000000000";
const ACTOR_ID: &str = "ac00000000000000000000000000000000000000000000000000000000000000";

fn image(reference: &str) -> Image {
    Image {
        id: format!("sha256:{:0>64}", reference.len()),
        repo_digests: vec![reference.into()],
        labels: BTreeMap::from([("org.quasar.recipe".to_string(), "1".to_string())]),
    }
}

/// An AMD GPU host with the hand-started actor on it.
fn host(new: Behaviour) -> FakeState {
    let mut state = FakeState {
        host: EngineHost {
            name: Some("gpu-host-01".into()),
            runtimes: vec!["runc".into()],
            cdi_devices: Vec::new(),
        },
        host_devices: ["/dev/dri", "/dev/uinput", "/dev/kmsg"]
            .iter()
            .map(|d| d.to_string())
            .collect::<BTreeSet<_>>(),
        probe_output:
            "quasar-probe 1\ndev uinput\ndev kmsg\nnode /dev/dri/renderD129 226:129 0x1002\nend\n"
                .into(),
        ..Default::default()
    };
    for digest in [OLD, NEW] {
        let reference = format!("{REPO}@{digest}");
        state.registry.insert(reference.clone(), image(&reference));
    }
    state.behaviour.insert(format!("{REPO}@{NEW}"), new);
    let actor_image = format!("registry.example.invalid/quasar/quasar-recovery@{OLD}");
    state
        .images
        .insert(actor_image.clone(), image(&actor_image));
    for volume in [names::MACHINE_VOLUME, names::AGENT_SOCKET_VOLUME] {
        state.volumes.insert(volume.into(), FakeVolume::default());
    }
    state.containers.insert(
        ACTOR_ID.into(),
        FakeContainer {
            id: ACTOR_ID.into(),
            spec: ContainerSpec {
                name: names::RECOVERY_ACTOR.into(),
                image: actor_image,
                entrypoint: None,
                cmd: Some(vec!["actor".into()]),
                env: BTreeMap::new(),
                labels: BTreeMap::new(),
                network_mode: None,
                binds: vec![Bind {
                    source: "/var/run/docker.sock".into(),
                    target: paths::ENGINE_SOCKET.into(),
                    read_only: false,
                }],
                devices: Vec::new(),
                device_cgroup_rules: Vec::new(),
                gpus: Vec::new(),
                cap_add: Vec::new(),
                security_opt: Vec::new(),
                init: false,
                restart: RestartPolicy::UnlessStopped,
            },
            status: "running".into(),
            starts: 1,
            restart: RestartPolicy::UnlessStopped,
            exit_code: None,
            logs: String::new(),
            health: None,
        },
    );
    state
}

struct Machine {
    engine: Arc<FakeEngine>,
    _machine_dir: tempfile::TempDir,
    _socket_dir: tempfile::TempDir,
    socket: PathBuf,
}

/// An installed owned GPU host whose recovery actor serves its agent socket.
fn machine(new: Behaviour) -> Machine {
    let engine = Arc::new(FakeEngine::new(host(new)));
    let machine_dir = tempfile::tempdir().unwrap();
    let mut config = ActorConfig::new(
        machine_dir.path(),
        quasar_recovery::socket::MachineRole::Gpu,
        OperatorInputs {
            enrollment: Some("qenr1..d3NzOi8vY3AuZXhhbXBsZS5pbnZhbGlk.tok".into()),
            home_root: Some("/srv/quasar/homes".into()),
            template_root: None,
            node_name: None,
            agent_image: Some(format!("{REPO}@{OLD}")),
        },
    );
    config.self_container = Some(ACTOR_ID.into());
    config.gpus_probe_backoff = Duration::ZERO;
    config.trust = TrustConfig {
        allowed_namespaces: vec!["registry.example.invalid/quasar".into()],
        ..Default::default()
    };
    config.timing = ReplaceTiming {
        verify_timeout: Duration::from_millis(300),
        poll: Duration::from_millis(5),
        stop_grace: Duration::from_secs(1),
        retries: 1,
        retry_backoff: Duration::ZERO,
    };
    let actor = Arc::new(RecoveryActor::new(engine.clone(), config));
    actor.resume().expect("install");
    let socket_dir = tempfile::tempdir().unwrap();
    let socket = socket_dir.path().join("agent.sock");
    let listener = server::bind(&socket).unwrap();
    std::thread::spawn(move || server::serve(listener, actor));
    Machine {
        engine,
        _machine_dir: machine_dir,
        _socket_dir: socket_dir,
        socket,
    }
}

fn components(name: &str, digest: &str) -> Vec<ReleaseComponent> {
    vec![ReleaseComponent {
        name: name.into(),
        image: if name == "recovery-actor" {
            "registry.example.invalid/quasar/quasar-recovery".into()
        } else {
            REPO.into()
        },
        digest: digest.into(),
    }]
}

fn release() -> ReleaseInfo {
    ReleaseInfo {
        id: String::new(),
        version: None,
        source_commit: "c".repeat(40),
    }
}

fn ack_of(msg: &AgentMsg) -> (bool, Option<String>) {
    match msg {
        AgentMsg::Ack { ok, error, .. } => (*ok, error.clone()),
        other => panic!("expected an ack, got {other:?}"),
    }
}

/// Every `release_state` for `id` until a terminal one.
async fn states_until_terminal(rx: &mut mpsc::Receiver<AgentMsg>, id: &str) -> Vec<AgentMsg> {
    let mut out = Vec::new();
    loop {
        let msg = tokio::time::timeout(Duration::from_secs(20), rx.recv())
            .await
            .expect("a release_state within 20 s")
            .expect("the channel stays open");
        if let AgentMsg::ReleaseState {
            request_id, state, ..
        } = &msg
        {
            assert_eq!(request_id, id);
            let terminal = is_terminal(state);
            out.push(msg);
            if terminal {
                return out;
            }
        }
    }
}

fn terminal(msgs: &[AgentMsg]) -> (String, Option<String>, bool, Vec<ReleasePrevious>) {
    match msgs.last().unwrap() {
        AgentMsg::ReleaseState {
            state,
            reason,
            restored,
            previous,
            ..
        } => (state.clone(), reason.clone(), *restored, previous.clone()),
        _ => unreachable!(),
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn an_owned_apply_is_submitted_to_the_recovery_actor_and_its_outcome_relayed() {
    let m = machine(Behaviour {
        health: Some("healthy".into()),
        ..Default::default()
    });
    let mgr = ReleaseManager::owned(&m.socket);
    let (tx, mut rx) = mpsc::channel(32);
    let _guard = mgr.attach_upstream(tx);
    assert!(mgr.present());

    let (ok, err) = ack_of(&mgr.handle_apply(
        "c1".into(),
        REQ.into(),
        release(),
        components("node-agent", NEW),
        false,
    ));
    assert!(ok, "{err:?}");
    let msgs = states_until_terminal(&mut rx, REQ).await;
    let (state, reason, restored, previous) = terminal(&msgs);
    assert_eq!(
        (state.as_str(), reason, restored),
        ("succeeded", None, false)
    );
    assert_eq!(previous[0].digest.as_deref(), Some(OLD));
    let agent = m
        .engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone();
    assert_eq!(agent.spec.image, format!("{REPO}@{NEW}"));
    assert_eq!(agent.status, "running");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_restored_agent_reports_the_failure_that_restored_it_on_its_next_connect() {
    let m = machine(Behaviour {
        health: Some("unhealthy".into()),
        ..Default::default()
    });
    let mgr = ReleaseManager::owned(&m.socket);
    let (tx, mut rx) = mpsc::channel(32);
    let guard = mgr.attach_upstream(tx);
    let (ok, _) = ack_of(&mgr.handle_apply(
        "c1".into(),
        REQ.into(),
        release(),
        components("node-agent", NEW),
        false,
    ));
    assert!(ok);
    let (state, reason, restored, _) = terminal(&states_until_terminal(&mut rx, REQ).await);
    assert_eq!(state, "failed");
    assert_eq!(reason.as_deref(), Some("unhealthy"));
    assert!(restored);
    drop(guard);

    // The old agent, restarted by the restore, connects and replays the result from the
    // actor's journal (ADR 0004: the failure is not hidden).
    let restarted = ReleaseManager::owned(&m.socket);
    let (tx, mut rx) = mpsc::channel(32);
    let _guard = restarted.attach_upstream(tx);
    let (state, reason, restored, _) = terminal(&states_until_terminal(&mut rx, REQ).await);
    assert_eq!(
        (state.as_str(), reason.as_deref(), restored),
        ("failed", Some("unhealthy"), true)
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn the_relay_forwards_the_agent_and_actor_components_and_relays_the_actors_refusals() {
    let m = machine(Behaviour::default());
    let mgr = ReleaseManager::owned(&m.socket);
    let before = m.engine.state();

    // Never forwarded: the confused deputy, and a name this agent does not know.
    for name in ["control-plane", "postgres"] {
        let (ok, err) = ack_of(&mgr.handle_apply(
            "c".into(),
            REQ.into(),
            release(),
            components(name, NEW),
            false,
        ));
        assert_eq!((ok, err.as_deref()), (false, Some("invalid")), "{name}");
    }
    // Forwarded; this build's actor refuses the actor's own replacement.
    let (ok, err) = ack_of(&mgr.handle_apply(
        "c".into(),
        REQ.into(),
        release(),
        components("recovery-actor", NEW),
        false,
    ));
    assert_eq!((ok, err.as_deref()), (false, Some("invalid")));
    // The actor's allowlist is the enforcement.
    let outside = vec![ReleaseComponent {
        name: "node-agent".into(),
        image: "elsewhere.example.invalid/x/quasar-node-agent".into(),
        digest: NEW.into(),
    }];
    let (ok, err) = ack_of(&mgr.handle_apply("c".into(), REQ.into(), release(), outside, false));
    assert_eq!((ok, err.as_deref()), (false, Some("namespace_rejected")));
    assert_eq!(m.engine.state(), before);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_second_apply_while_one_is_in_flight_is_busy_and_the_same_one_is_idempotent() {
    let m = machine(Behaviour {
        health: Some("starting".into()),
        ..Default::default()
    });
    let mgr = ReleaseManager::owned(&m.socket);
    let (tx, mut rx) = mpsc::channel(64);
    let _guard = mgr.attach_upstream(tx);
    let apply = |id: &str| {
        ack_of(&mgr.handle_apply(
            "c".into(),
            id.into(),
            release(),
            components("node-agent", NEW),
            false,
        ))
    };
    assert_eq!(apply(REQ), (true, None));
    assert_eq!(apply(REQ), (true, None));
    assert_eq!(apply(REQ2).1.as_deref(), Some("busy"));
    let (state, reason, restored, _) = terminal(&states_until_terminal(&mut rx, REQ).await);
    assert_eq!(
        (state.as_str(), reason.as_deref(), restored),
        ("failed", Some("unhealthy"), true)
    );
}

#[test]
fn an_owned_manager_with_no_actor_socket_is_absent() {
    let dir = tempfile::tempdir().unwrap();
    let mgr = ReleaseManager::owned(dir.path().join("agent.sock"));
    assert!(!mgr.present());
    let (ok, err) = ack_of(&mgr.handle_apply(
        "c".into(),
        REQ.into(),
        release(),
        components("node-agent", NEW),
        false,
    ));
    assert_eq!((ok, err.as_deref()), (false, Some("updater_absent")));
}
