//! `reconfigure` on a combined or control-only machine (#386): the control plane's inputs
//! changed through a verified control-plane replacement on the same digest, then the node
//! agent's where it moves too. Against the in-memory engine (with a simulated database, so a
//! migration would show) and a temporary machine-state directory, observed through the
//! actor's reconfigure, status and resume, engine state, machine state and
//! `reconfigure.json`.

mod support;

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use quasar_recovery::actor::{Actor, ActorConfig, OperatorInputs, ReplaceTiming, TrustConfig};
use quasar_recovery::engine::{
    Behaviour, EngineError, FakeContainer, FakeDatabase, FakeEngine, FakeState, Fault, Image, When,
};
use quasar_recovery::journal::Phase;
use quasar_recovery::recipe::names;
use quasar_recovery::reconfigure::{ReconfigureRequest, Settled};
use quasar_recovery::seed::Outcome;
use quasar_recovery::socket::{MachineRole, Reason, State};
use quasar_recovery::trust::{Caller, SignaturePolicy};
use support::*;

const CONTROL_IMAGE: &str = "registry.example.invalid/quasar/quasar-control-plane@sha256:c0de000000000000000000000000000000000000000000000000000000000050";
const POSTGRES_IMAGE: &str = "docker.io/library/postgres@sha256:dd55000000000000000000000000000000000000000000000000000000000000";
const SCHEMA: i64 = 80;
const NEW_HTTP: u16 = 18080;
const NEW_TLS: u16 = 18443;
const PUBLIC: &str = "quasar.example.invalid";

fn image(reference: &str, id: u64, labels: &[(&str, &str)]) -> Image {
    Image {
        id: format!("sha256:{id:064x}"),
        repo_digests: vec![reference.into()],
        labels: labels
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect(),
    }
}

fn host(env: BTreeMap<String, String>) -> FakeState {
    let mut state = seeded_host(env);
    state
        .registry
        .insert(AGENT_IMAGE.into(), agent_image(Some("2")));
    let schema = SCHEMA.to_string();
    state.registry.insert(
        CONTROL_IMAGE.into(),
        image(
            CONTROL_IMAGE,
            0x1d50,
            &[
                ("org.quasar.recipe", "2"),
                ("org.quasar.schema.version", &schema),
            ],
        ),
    );
    state
        .registry
        .insert(POSTGRES_IMAGE.into(), image(POSTGRES_IMAGE, 0x9090, &[]));
    for reference in [CONTROL_IMAGE, POSTGRES_IMAGE] {
        state.behaviour.insert(
            reference.into(),
            Behaviour {
                health: Some("healthy".into()),
                logs: "listening\n".into(),
                ..Default::default()
            },
        );
    }
    state.database = Some(FakeDatabase {
        schema_version: 0,
        dirty: false,
        rows: "rows".into(),
        size_bytes: 1_000_000,
    });
    state
}

fn combined_env() -> BTreeMap<String, String> {
    BTreeMap::from([
        ("QUASAR_ROLE".to_string(), "combined".to_string()),
        ("QUASAR_HOME_ROOT".into(), HOME.into()),
        ("QUASAR_AGENT_IMAGE".into(), AGENT_IMAGE.into()),
        ("QUASAR_CONTROL_PLANE_IMAGE".into(), CONTROL_IMAGE.into()),
        ("QUASAR_POSTGRES_IMAGE".into(), POSTGRES_IMAGE.into()),
    ])
}

fn control_only_env() -> BTreeMap<String, String> {
    BTreeMap::from([
        ("QUASAR_ROLE".to_string(), "control-only".to_string()),
        ("QUASAR_CONTROL_PLANE_IMAGE".into(), CONTROL_IMAGE.into()),
        ("QUASAR_POSTGRES_IMAGE".into(), POSTGRES_IMAGE.into()),
    ])
}

struct Machine {
    engine: Arc<FakeEngine>,
    dir: tempfile::TempDir,
    _sockets: tempfile::TempDir,
    actor_id: String,
    clock: Arc<AtomicU64>,
}

impl Machine {
    fn install(env: BTreeMap<String, String>) -> Machine {
        let engine = Arc::new(FakeEngine::new(host(env)));
        let dir = tempfile::tempdir().unwrap();
        let created = seed(&engine, dir.path(), SEED_ID).step();
        assert!(matches!(created, Outcome::Created { .. }), "{created:?}");
        let actor_id = engine
            .state()
            .container_named(names::RECOVERY_ACTOR)
            .unwrap()
            .id
            .clone();
        let m = Machine {
            engine,
            dir,
            _sockets: tempfile::tempdir().unwrap(),
            actor_id,
            clock: Arc::new(AtomicU64::new(0)),
        };
        m.actor().resume().expect("the install");
        m
    }

    fn config(&self) -> ActorConfig {
        let mut config =
            ActorConfig::new(self.dir.path(), MachineRole::Gpu, OperatorInputs::default());
        config.self_container = Some(self.actor_id.clone());
        config.seed_container = Some(SEED_ID.into());
        let clock = self.clock.clone();
        config.now = Box::new(move || {
            let n = clock.fetch_add(1, Ordering::SeqCst);
            format!("2026-09-26T10:{:02}:{:02}Z", (n / 60) % 60, n % 60)
        });
        config.gpus_probe_backoff = Duration::ZERO;
        config.healthy_wait = Duration::from_millis(20);
        config.timing = ReplaceTiming {
            verify_timeout: Duration::from_millis(150),
            poll: Duration::from_millis(1),
            stop_grace: Duration::from_secs(1),
            retries: 1,
            retry_backoff: Duration::ZERO,
        };
        config.trust = TrustConfig {
            allowed_namespaces: vec!["registry.example.invalid/quasar".into()],
            signature: SignaturePolicy::default(),
        };
        config.machine_dir_host = Some(self.dir.path().display().to_string());
        config.free_space = Box::new(|_| Ok(212_000_000_000));
        config.socket_dir = self._sockets.path().join("run");
        config
    }

    fn actor(&self) -> Arc<Actor> {
        Arc::new(Actor::new(self.engine.clone(), self.config()))
    }

    fn actor_with(&self, adjust: impl FnOnce(&mut ActorConfig)) -> Arc<Actor> {
        let mut config = self.config();
        adjust(&mut config);
        Arc::new(Actor::new(self.engine.clone(), config))
    }

    fn named(&self, name: &str) -> Option<FakeContainer> {
        self.engine.state().container_named(name).cloned()
    }

    fn control_plane(&self) -> FakeContainer {
        self.named(names::CONTROL_PLANE).expect("a control plane")
    }

    fn agent(&self) -> FakeContainer {
        self.named(names::NODE_AGENT).expect("a node agent")
    }

    fn inputs(&self) -> serde_json::Value {
        let m: serde_json::Value =
            serde_json::from_slice(&std::fs::read(self.dir.path().join("machine.json")).unwrap())
                .unwrap();
        m["inputs"].clone()
    }

    fn record(&self) -> Option<serde_json::Value> {
        std::fs::read(self.dir.path().join("reconfigure.json"))
            .ok()
            .map(|b| serde_json::from_slice(&b).unwrap())
    }

    fn settled(&self) -> String {
        self.record()
            .and_then(|r| r["outcome"]["settled"].as_str().map(str::to_owned))
            .unwrap_or_else(|| "unsettled".into())
    }

    fn journal(&self, id: &str) -> serde_json::Value {
        let path = self.dir.path().join("journal").join(format!("{id}.json"));
        serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap()
    }

    /// The machine's control planes that run: there is always exactly one after a settle.
    fn running_control_planes(&self) -> Vec<String> {
        self.engine
            .state()
            .containers
            .values()
            .filter(|c| {
                c.spec
                    .labels
                    .get("io.quasar.platform-service")
                    .map(String::as_str)
                    == Some("control-plane")
                    && c.status == "running"
            })
            .map(|c| c.spec.name.clone())
            .collect()
    }

    /// Postgres and the recovery actor: never replaced by a reconfigure.
    fn untouchables(&self) -> (String, String) {
        (
            self.named(names::POSTGRES).expect("postgres").id,
            self.named(names::RECOVERY_ACTOR).expect("the actor").id,
        )
    }

    /// The database was not migrated, dumped or run under by an older control plane.
    fn never_a_migration(&self, at: &str) {
        let s = self.engine.state();
        assert_eq!(s.database.unwrap().schema_version, SCHEMA, "{at}");
        assert!(s.older_control_planes_started.is_empty(), "{at}");
        assert!(self.actor().status().dumps.is_empty(), "{at}: no dump");
    }
}

fn changes(pairs: &[(&str, &str)]) -> ReconfigureRequest {
    ReconfigureRequest {
        changes: pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect(),
        dry_run: false,
    }
}

fn dry(pairs: &[(&str, &str)]) -> ReconfigureRequest {
    ReconfigureRequest {
        dry_run: true,
        ..changes(pairs)
    }
}

fn ports() -> Vec<(&'static str, String)> {
    vec![
        ("QUASAR_PUBLIC_HOST", PUBLIC.to_string()),
        ("QUASAR_TLS_HOSTS", "play.example.invalid,192.0.2.10".into()),
        ("QUASAR_HTTP_PORT", NEW_HTTP.to_string()),
        ("QUASAR_TLS_PORT", NEW_TLS.to_string()),
    ]
}

fn ports_request() -> ReconfigureRequest {
    let owned = ports();
    let pairs: Vec<(&str, &str)> = owned.iter().map(|(k, v)| (*k, v.as_str())).collect();
    changes(&pairs)
}

fn env(c: &FakeContainer, key: &str) -> String {
    c.spec.env.get(key).cloned().unwrap_or_default()
}

fn host_ports(c: &FakeContainer) -> Vec<u16> {
    c.spec.ports.iter().map(|p| p.host_port).collect()
}

fn run(
    actor: &Arc<Actor>,
    req: ReconfigureRequest,
) -> (String, quasar_recovery::socket::AttemptResult) {
    let id = actor
        .reconfigure(req)
        .expect("admitted")
        .request_id
        .expect("a replacement");
    actor.wait_attempt();
    let result = actor.status_for(Some(&id)).result.expect("journalled");
    (id, result)
}

// ----- the replacement -----

#[test]
fn a_combined_hosts_public_host_and_ports_replace_the_control_plane_then_its_agent() {
    let m = Machine::install(combined_env());
    let actor = m.actor();
    let (old_cp, old_agent) = (m.control_plane(), m.agent());
    let untouchables = m.untouchables();
    assert_eq!(host_ports(&old_cp), vec![8080, 8443]);

    let owned = ports();
    let pairs: Vec<(&str, &str)> = owned.iter().map(|(k, v)| (*k, v.as_str())).collect();
    let plan = actor.reconfigure(dry(&pairs)).expect("planned");
    assert_eq!(
        plan.replaced,
        vec!["control-plane", "node-agent"],
        "the control plane first"
    );
    assert!(plan.request_id.is_none());
    let notes = plan.notes.join("\n");
    for said in [
        "ends this host's sessions",
        "enrollment string",
        "re-issued",
        "restarts the console",
    ] {
        assert!(notes.contains(said), "{said:?} in {notes}");
    }
    assert_eq!(m.control_plane().id, old_cp.id, "a dry run changes nothing");
    assert!(m.record().is_none());

    let (id, result) = run(&actor, ports_request());
    assert_eq!(result.state, State::Succeeded, "{}", result.output);
    let steps: Vec<String> = m.journal(&id)["steps"]
        .as_array()
        .unwrap()
        .iter()
        .map(|s| s["name"].as_str().unwrap().to_owned())
        .collect();
    assert_eq!(steps, vec!["control-plane", "node-agent"]);

    let (cp, agent) = (m.control_plane(), m.agent());
    assert_ne!(cp.id, old_cp.id);
    assert_eq!(cp.spec.image, old_cp.spec.image, "the same digest");
    assert_eq!(host_ports(&cp), vec![NEW_HTTP, NEW_TLS]);
    assert_eq!(env(&cp, "QUASAR_PUBLIC_HOST"), PUBLIC);
    assert_eq!(
        env(&cp, "QUASAR_TLS_HOSTS"),
        "play.example.invalid,192.0.2.10"
    );
    assert_eq!(env(&cp, "QUASAR_TLS_REDIRECT_PORT"), NEW_TLS.to_string());
    assert_ne!(agent.id, old_agent.id);
    assert_eq!(agent.spec.image, old_agent.spec.image);
    assert_eq!(
        env(&agent, "CONTROL_PLANE_URL"),
        format!("ws://127.0.0.1:{NEW_HTTP}"),
        "the agent dials the control plane's new port"
    );
    let inputs = m.inputs();
    assert_eq!(inputs["control"]["http_port"], NEW_HTTP);
    assert_eq!(inputs["control"]["tls_port"], NEW_TLS);
    assert_eq!(inputs["control"]["public_host"], PUBLIC);
    assert_eq!(m.settled(), "applied");
    assert_eq!(
        m.untouchables(),
        untouchables,
        "Postgres and the actor are never replaced"
    );
    assert!(m.named("quasar-control-plane.kept").is_none());
    assert!(m.named("quasar-node-agent.kept").is_none());
    assert_eq!(m.running_control_planes(), vec![names::CONTROL_PLANE]);
    m.never_a_migration("ports");
}

#[test]
fn a_combined_hosts_home_root_moves_the_control_plane_and_the_agent() {
    let m = Machine::install(combined_env());
    let actor = m.actor();
    let (_, result) = run(
        &actor,
        changes(&[("QUASAR_HOME_ROOT", "/mnt/quasar/homes")]),
    );
    assert_eq!(result.state, State::Succeeded, "{}", result.output);
    assert_eq!(
        env(&m.control_plane(), "QUASAR_HOME_ROOT"),
        "/mnt/quasar/homes"
    );
    assert_eq!(env(&m.agent(), "QUASAR_HOME_ROOT"), "/mnt/quasar/homes");
    assert_eq!(m.inputs()["home_root"], "/mnt/quasar/homes");
    assert_eq!(m.settled(), "applied");
}

#[test]
fn release_trust_and_proxies_move_only_the_control_plane_and_a_signature_mode_is_only_recorded() {
    let m = Machine::install(combined_env());
    let actor = m.actor();
    let agent = m.agent().id;
    let plan = actor
        .reconfigure(dry(&[
            (
                "QUASAR_UPDATER_ALLOWED_NAMESPACES",
                "registry.example.invalid/quasar,registry.example.invalid/test",
            ),
            (
                "QUASAR_PLATFORM_INSECURE_REGISTRIES",
                "registry.example.invalid:5000",
            ),
            ("QUASAR_TRUSTED_PROXIES", "172.18.0.0/16"),
        ]))
        .unwrap();
    assert_eq!(plan.replaced, vec!["control-plane"]);
    assert!(
        !plan.notes.join(" ").contains("sessions."),
        "no agent is re-created: {:?}",
        plan.notes
    );
    let (_, result) = run(
        &actor,
        changes(&[
            (
                "QUASAR_UPDATER_ALLOWED_NAMESPACES",
                "registry.example.invalid/quasar,registry.example.invalid/test",
            ),
            (
                "QUASAR_PLATFORM_INSECURE_REGISTRIES",
                "registry.example.invalid:5000",
            ),
            ("QUASAR_TRUSTED_PROXIES", "172.18.0.0/16"),
        ]),
    );
    assert_eq!(result.state, State::Succeeded, "{}", result.output);
    let cp = m.control_plane();
    assert_eq!(env(&cp, "QUASAR_TRUSTED_PROXIES"), "172.18.0.0/16");
    assert_eq!(
        env(&cp, "QUASAR_PLATFORM_INSECURE_REGISTRIES"),
        "registry.example.invalid:5000"
    );
    assert_eq!(m.agent().id, agent, "the agent renders none of these");
    // The actor admits under machine state's trust from the next request on.
    let trusted = actor.trust().unwrap().allowed_namespaces;
    assert!(
        trusted
            .iter()
            .any(|n| n.contains("registry.example.invalid/test")),
        "{trusted:?}"
    );

    let before = m.control_plane().id;
    let plan = actor
        .reconfigure(changes(&[("QUASAR_UPDATER_SIGNATURE_MODE", "verify")]))
        .unwrap();
    assert!(plan.replaced.is_empty() && plan.request_id.is_none());
    assert_eq!(m.inputs()["trust"]["signature_mode"], "verify");
    assert_eq!(m.control_plane().id, before);
}

#[test]
fn a_control_only_machine_takes_every_control_plane_input_and_no_home_root() {
    let m = Machine::install(control_only_env());
    assert!(m.named(names::NODE_AGENT).is_none());
    let actor = m.actor();
    let (_, result) = run(
        &actor,
        changes(&[
            ("QUASAR_PUBLIC_HOST", PUBLIC),
            ("QUASAR_TLS_PORT", "18443"),
            ("QUASAR_HTTP_PORT", "18080"),
            ("QUASAR_TRUSTED_PROXIES", "10.0.0.1"),
            (
                "QUASAR_UPDATER_ALLOWED_NAMESPACES",
                "registry.example.invalid/quasar",
            ),
        ]),
    );
    assert_eq!(result.state, State::Succeeded, "{}", result.output);
    assert_eq!(host_ports(&m.control_plane()), vec![NEW_HTTP, NEW_TLS]);
    assert_eq!(m.settled(), "applied");
    assert!(m.named(names::NODE_AGENT).is_none(), "no agent appears");

    let refused = actor
        .reconfigure(changes(&[("QUASAR_HOME_ROOT", "/mnt/quasar/homes")]))
        .expect_err("no agent, no home root");
    assert_eq!(refused.reason, Reason::Invalid);
    assert!(
        refused.message.contains("control-only"),
        "{}",
        refused.message
    );
    m.never_a_migration("control-only");
}

// ----- what a reconfigure never does -----

#[test]
fn the_database_and_the_node_name_are_refused_naming_the_reinstall() {
    for env in [combined_env(), control_only_env()] {
        let m = Machine::install(env);
        let actor = m.actor();
        let before = m.inputs();
        for (key, value, says) in [
            (
                "QUASAR_DATABASE_HOST",
                "db.example.invalid",
                "database mode",
            ),
            ("QUASAR_DATABASE_PASSWORD", "x", "database mode"),
            ("QUASAR_NODE_NAME", "another", "identity"),
            ("QUASAR_POSTGRES_IMAGE", POSTGRES_IMAGE, "update"),
            ("QUASAR_CONTROL_PLANE_IMAGE", CONTROL_IMAGE, "update"),
        ] {
            let refused = actor.reconfigure(changes(&[(key, value)])).expect_err(key);
            assert_eq!(refused.reason, Reason::Invalid, "{key}");
            assert!(refused.message.contains(says), "{key}: {}", refused.message);
            if says != "update" {
                assert!(
                    refused.message.contains("reinstall"),
                    "{key}: {}",
                    refused.message
                );
            }
            assert!(refused.message.contains("nothing was changed"), "{key}");
        }
        assert_eq!(m.inputs(), before);
        assert!(m.record().is_none());
    }
}

#[test]
fn a_restore_holding_the_database_refuses_a_control_plane_reconfigure() {
    let m = Machine::install(combined_env());
    quasar_recovery::database::store_hold(
        m.dir.path(),
        &quasar_recovery::database::Hold {
            format: 1,
            reason: quasar_recovery::database::HoldReason::RestoreIncomplete,
            since: NOW.into(),
            request_id: None,
        },
    )
    .unwrap();
    let refused = m
        .actor()
        .reconfigure(changes(&[("QUASAR_TLS_PORT", "18443")]))
        .expect_err("held");
    assert!(
        refused.message.contains("restore holds"),
        "{}",
        refused.message
    );
    assert_eq!(m.inputs()["control"]["tls_port"], 8443);
}

#[test]
fn a_control_plane_on_another_image_than_recorded_is_not_reconfigured() {
    let m = Machine::install(combined_env());
    let other = "registry.example.invalid/quasar/quasar-control-plane@sha256:c0de000000000000000000000000000000000000000000000000000000000051";
    m.engine.with_state(|s| {
        let cp = s
            .containers
            .values_mut()
            .find(|c| c.spec.name == names::CONTROL_PLANE)
            .unwrap();
        cp.spec.image = other.into();
    });
    let refused = m
        .actor()
        .reconfigure(changes(&[("QUASAR_TLS_PORT", "18443")]))
        .expect_err("not the recorded image");
    assert!(
        refused.message.contains("keeps a service's image"),
        "{}",
        refused.message
    );
    assert!(m.record().is_none());
}

// ----- a control plane that does not verify -----

#[test]
fn a_control_plane_that_never_starts_on_a_taken_port_is_restored_with_the_old_inputs() {
    let m = Machine::install(combined_env());
    let actor = m.actor();
    let (old_cp, old_agent) = (m.control_plane(), m.agent());
    m.engine.with_state(|s| {
        s.ports_in_use.insert(NEW_HTTP);
    });
    let (_, result) = run(&actor, ports_request());
    assert_eq!(result.state, State::Failed);
    assert_eq!(
        result.reason,
        Some(Reason::NeverStarted),
        "{}",
        result.output
    );
    assert!(result.restored, "{}", result.output);
    assert!(
        result.output.contains("port is already allocated"),
        "{}",
        result.output
    );

    let cp = m.control_plane();
    assert_eq!(cp.id, old_cp.id, "the kept control plane is back");
    assert_eq!(cp.status, "running");
    assert_eq!(host_ports(&cp), vec![8080, 8443]);
    assert_eq!(m.agent().id, old_agent.id, "the agent was never touched");
    assert_eq!(
        m.inputs()["control"]["http_port"],
        8080,
        "the old inputs are back"
    );
    assert!(m.inputs()["control"]["public_host"].is_null());
    let record = m.record().unwrap();
    assert_eq!(record["outcome"]["settled"], "put_back");
    assert_eq!(record["outcome"]["reason"], "never_started");
    assert_eq!(record["outcome"]["restored"], true);
    assert_eq!(m.running_control_planes(), vec![names::CONTROL_PLANE]);

    // The port freed, the same reconfigure goes through.
    m.engine.with_state(|s| s.ports_in_use.clear());
    let (_, result) = run(&actor, ports_request());
    assert_eq!(result.state, State::Succeeded, "{}", result.output);
    assert_eq!(m.settled(), "applied");
}

#[test]
fn a_control_plane_that_reports_unhealthy_is_restored_with_the_old_inputs() {
    let m = Machine::install(control_only_env());
    let actor = m.actor();
    let old = m.control_plane();
    m.engine.with_state(|s| {
        s.behaviour.get_mut(CONTROL_IMAGE).unwrap().health = Some("unhealthy".into());
    });
    let (_, result) = run(&actor, changes(&[("QUASAR_TLS_PORT", "18443")]));
    assert_eq!(result.state, State::Failed);
    assert_eq!(result.reason, Some(Reason::Unhealthy));
    assert!(result.restored);
    assert_eq!(m.control_plane().id, old.id);
    assert_eq!(m.inputs()["control"]["tls_port"], 8443);
    assert_eq!(m.settled(), "put_back");
    assert_eq!(m.running_control_planes(), vec![names::CONTROL_PLANE]);
}

#[test]
fn an_agent_that_does_not_verify_after_the_control_plane_did_is_partial_and_a_rerun_catches_it_up()
{
    let m = Machine::install(combined_env());
    let actor = m.actor();
    let old_agent = m.agent();
    m.engine.with_state(|s| {
        s.behaviour.insert(
            AGENT_IMAGE.into(),
            Behaviour {
                health: Some("unhealthy".into()),
                ..Default::default()
            },
        );
    });
    let (_, result) = run(&actor, changes(&[("QUASAR_HTTP_PORT", "18080")]));
    assert_eq!(result.state, State::Failed);
    assert!(result.restored);
    assert!(
        result.output.contains("control-plane") && result.output.contains("stays"),
        "the output says the verified control plane stays: {}",
        result.output
    );
    assert_eq!(
        host_ports(&m.control_plane())[0],
        NEW_HTTP,
        "the control plane verified"
    );
    assert_eq!(m.agent().id, old_agent.id, "the agent was put back");
    assert_eq!(
        m.inputs()["control"]["http_port"],
        NEW_HTTP,
        "what the control plane runs"
    );
    let record = m.record().unwrap();
    assert_eq!(record["outcome"]["settled"], "partial");
    assert_eq!(
        record["outcome"]["behind"],
        serde_json::json!(["node-agent"])
    );

    m.engine.with_state(|s| {
        s.behaviour.remove(AGENT_IMAGE);
    });
    let plan = actor
        .reconfigure(dry(&[("QUASAR_HTTP_PORT", "18080")]))
        .unwrap();
    assert!(plan.changed.is_empty(), "the value is already in force");
    assert_eq!(plan.replaced, vec!["node-agent"], "the agent left behind");
    let (_, result) = run(&actor, changes(&[("QUASAR_HTTP_PORT", "18080")]));
    assert_eq!(result.state, State::Succeeded, "{}", result.output);
    assert_eq!(
        env(&m.agent(), "CONTROL_PLANE_URL"),
        format!("ws://127.0.0.1:{NEW_HTTP}")
    );
    assert_eq!(m.settled(), "applied");
    let plan = actor
        .reconfigure(dry(&[("QUASAR_HTTP_PORT", "18080")]))
        .unwrap();
    assert!(plan.replaced.is_empty(), "nothing left behind: {plan:?}");
}

// ----- a kill or an engine restart at every phase -----

/// Every phase `advance` commits; `admitted` is committed at admission, before any.
const PHASES: [Phase; 9] = [
    Phase::Pulling,
    Phase::Checked,
    Phase::OldKept,
    Phase::Created,
    Phase::Started,
    Phase::Verifying,
    Phase::Verified,
    Phase::OldDiscarded,
    Phase::Done,
];

/// The actor dies right after `component` commits `phase`, optionally the engine restarts
/// too, and the next start settles the reconfigure: one running control plane, a terminal
/// attempt, a settled record, and machine inputs matching what runs.
fn crash_then_settle(
    component: &'static str,
    phase: Phase,
    daemon_restart: bool,
    fail_cp: bool,
) -> String {
    let at = format!("{component} {phase:?} daemon_restart={daemon_restart} fail_cp={fail_cp}");
    let m = Machine::install(combined_env());
    let untouchables = m.untouchables();
    if fail_cp {
        m.engine.with_state(|s| {
            s.behaviour.get_mut(CONTROL_IMAGE).unwrap().health = Some("unhealthy".into());
        });
    }
    let actor = m.actor_with(|c| {
        c.crash_after = Some(Box::new(move |name, p| name == component && p == phase));
    });
    let id = actor
        .reconfigure(ports_request())
        .expect("admitted")
        .request_id
        .unwrap();
    actor.wait_attempt();
    drop(actor);
    if daemon_restart {
        m.engine.restart_daemon();
    }
    m.actor().resume().unwrap_or_else(|e| panic!("{at}: {e}"));

    let result = m.actor().status_for(Some(&id)).result.expect("journalled");
    assert!(
        matches!(result.state, State::Succeeded | State::Failed),
        "{at}: {result:?}"
    );
    assert_eq!(
        m.running_control_planes(),
        vec![names::CONTROL_PLANE],
        "{at}"
    );
    assert_eq!(m.untouchables(), untouchables, "{at}");
    assert!(m.named("quasar-control-plane.kept").is_none(), "{at}");
    assert!(m.named("quasar-node-agent.kept").is_none(), "{at}");
    assert_eq!(m.agent().status, "running", "{at}");
    let cp_port = host_ports(&m.control_plane())[0];
    let agent_url = env(&m.agent(), "CONTROL_PLANE_URL");
    let inputs_port = m.inputs()["control"]["http_port"].as_u64().unwrap() as u16;
    match m.settled().as_str() {
        "applied" => {
            assert_eq!(result.state, State::Succeeded, "{at}");
            assert_eq!(cp_port, NEW_HTTP, "{at}");
            assert_eq!(agent_url, format!("ws://127.0.0.1:{NEW_HTTP}"), "{at}");
            assert_eq!(inputs_port, NEW_HTTP, "{at}");
        }
        "put_back" => {
            assert_eq!(cp_port, 8080, "{at}");
            assert_eq!(agent_url, "ws://127.0.0.1:8080", "{at}");
            assert_eq!(inputs_port, 8080, "{at}");
        }
        "partial" => {
            assert_eq!(cp_port, NEW_HTTP, "{at}");
            assert_eq!(agent_url, "ws://127.0.0.1:8080", "{at}");
            assert_eq!(inputs_port, NEW_HTTP, "{at}");
        }
        other => panic!("{at}: settled {other}"),
    }
    if fail_cp {
        assert_eq!(m.settled(), "put_back", "{at}");
    }
    // Before the control plane was touched, nothing changed; after it verified, it stays.
    let cp_untouched =
        component == "control-plane" && matches!(phase, Phase::Pulling | Phase::Checked);
    if cp_untouched {
        assert_eq!(result.reason, Some(Reason::Interrupted), "{at}");
        assert_eq!(m.settled(), "put_back", "{at}");
    }
    if component == "node-agent" && !fail_cp {
        assert_ne!(
            m.settled(),
            "put_back",
            "{at}: the verified control plane stays"
        );
    }
    m.never_a_migration(&at);

    // Settling again changes nothing, and the machine takes the next reconfigure.
    let before = m.record();
    m.actor().resume().unwrap();
    assert_eq!(m.record(), before, "{at}");
    m.engine.with_state(|s| {
        s.behaviour.get_mut(CONTROL_IMAGE).unwrap().health = Some("healthy".into());
    });
    let settled = m.settled();
    let (_, again) = run(&m.actor(), changes(&[("QUASAR_TLS_PORT", "28443")]));
    assert_eq!(again.state, State::Succeeded, "{at}: {}", again.output);
    settled
}

#[test]
fn a_kill_at_every_control_plane_phase_settles_to_a_stated_outcome() {
    let seen: std::collections::BTreeSet<String> = PHASES
        .iter()
        .map(|p| crash_then_settle("control-plane", *p, false, false))
        .collect();
    assert_eq!(
        seen,
        ["applied", "partial", "put_back"].map(String::from).into()
    );
}

#[test]
fn a_kill_at_every_node_agent_phase_settles_to_a_stated_outcome() {
    let seen: std::collections::BTreeSet<String> = PHASES
        .iter()
        .map(|p| crash_then_settle("node-agent", *p, false, false))
        .collect();
    assert_eq!(seen, ["applied", "partial"].map(String::from).into());
}

#[test]
fn a_kill_and_an_engine_restart_at_every_phase_settle_to_a_stated_outcome() {
    for component in ["control-plane", "node-agent"] {
        for phase in PHASES {
            crash_then_settle(component, phase, true, false);
        }
    }
}

#[test]
fn a_kill_while_a_control_plane_that_did_not_verify_is_being_restored_settles_put_back() {
    for phase in [Phase::Verifying, Phase::Restoring] {
        for daemon_restart in [false, true] {
            crash_then_settle("control-plane", phase, daemon_restart, true);
        }
    }
}

/// The actor dies on the attempt's first engine call, with only `admitted` journalled.
#[test]
fn a_kill_right_after_admission_settles_put_back() {
    for daemon_restart in [false, true] {
        let m = Machine::install(combined_env());
        let actor = m.actor();
        let old = m.control_plane().id;
        // The admission's list and two inspects, then the attempt's first look at the image.
        m.engine.inject(Fault {
            call: m.engine.calls() + 3,
            when: When::Before,
            error: EngineError::Crashed,
        });
        let id = actor
            .reconfigure(ports_request())
            .expect("admitted")
            .request_id
            .unwrap();
        actor.wait_attempt();
        drop(actor);
        m.engine.clear_faults();
        assert_eq!(m.inputs()["control"]["http_port"], NEW_HTTP, "mid-attempt");
        if daemon_restart {
            m.engine.restart_daemon();
        }
        m.actor().resume().unwrap();
        let result = m.actor().status_for(Some(&id)).result.unwrap();
        assert_eq!(result.reason, Some(Reason::Interrupted), "{result:?}");
        assert_eq!(m.settled(), "put_back");
        assert_eq!(m.inputs()["control"]["http_port"], 8080);
        assert_eq!(m.control_plane().id, old);
        assert_eq!(m.running_control_planes(), vec![names::CONTROL_PLANE]);
    }
}

/// A crash after the record and the new inputs were written but before the attempt was
/// journalled: the next start puts the old inputs back.
#[test]
fn a_reconfigure_never_journalled_is_put_back_on_the_next_start() {
    let m = Machine::install(combined_env());
    let actor = m.actor();
    let (id, _) = run(&actor, ports_request());
    drop(actor);
    // Rewind to the moment between machine state and the journal.
    let mut record = m.record().unwrap();
    record["outcome"] = serde_json::Value::Null;
    record["request_id"] = "0a1b2c3d-0000-4000-8000-000000000000".into();
    record["before"] = record["after"].clone();
    record["after"]["control"]["tls_port"] = 28443.into();
    std::fs::write(
        m.dir.path().join("reconfigure.json"),
        serde_json::to_vec(&record).unwrap(),
    )
    .unwrap();
    let mut machine: serde_json::Value =
        serde_json::from_slice(&std::fs::read(m.dir.path().join("machine.json")).unwrap()).unwrap();
    machine["inputs"] = record["after"].clone();
    std::fs::write(
        m.dir.path().join("machine.json"),
        serde_json::to_vec(&machine).unwrap(),
    )
    .unwrap();

    m.actor().resume().unwrap();
    assert_eq!(m.settled(), "put_back");
    assert_eq!(m.inputs()["control"]["tls_port"], NEW_TLS);
    assert!(m.record().unwrap()["outcome"]["state"].is_null());
    assert!(m.actor().status_for(Some(&id)).result.is_some());
}

// ----- what the control plane sees of it -----

/// The control socket answers only for attempts submitted on it, so a control plane booted
/// by an operator's reconfigure never reads that attempt as one of its own: it sees only
/// that the machine is busy.
#[test]
fn the_control_socket_never_shows_an_operators_reconfigure() {
    let m = Machine::install(combined_env());
    let actor = m.actor_with(|c| {
        c.crash_after = Some(Box::new(|name, p| {
            name == "node-agent" && p == Phase::OldKept
        }));
    });
    let id = actor
        .reconfigure(ports_request())
        .unwrap()
        .request_id
        .unwrap();
    actor.wait_attempt();
    let open = actor.status_as(Caller::ControlPlane, Some(&id));
    assert_eq!(
        open.in_flight.as_deref(),
        Some(id.as_str()),
        "the machine is busy"
    );
    assert!(
        open.result.is_none(),
        "but the attempt is not the control plane's"
    );
    assert!(actor.status_as(Caller::ControlPlane, None).result.is_none());
    drop(actor);

    let settled = m.actor();
    settled.resume().unwrap();
    let st = settled.status_as(Caller::ControlPlane, Some(&id));
    assert!(st.in_flight.is_none() && st.result.is_none());
    assert_eq!(
        settled
            .status_operator(Some(&id))
            .result
            .unwrap()
            .request_id,
        id,
        "the operator's door shows it"
    );
}

#[test]
fn the_operator_socket_serves_the_settled_record() {
    let m = Machine::install(combined_env());
    let actor = m.actor();
    let (id, _) = run(&actor, changes(&[("QUASAR_TLS_PORT", "18443")]));
    let socket = m.dir.path().join("operator-test.sock");
    let listener = std::os::unix::net::UnixListener::bind(&socket).expect("bind");
    let served = actor.clone();
    std::thread::spawn(move || quasar_recovery::operator::serve(listener, served));
    let record = quasar_recovery::operator::reconfigure_record(&socket)
        .expect("answered")
        .expect("a record");
    assert_eq!(record.request_id, id);
    assert_eq!(record.outcome.unwrap().settled, Settled::Applied);
}
