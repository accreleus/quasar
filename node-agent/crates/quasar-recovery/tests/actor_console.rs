//! Console mode on a rootful owned install (RH-07 #395): the node agent's `POST
//! /v1/console` re-creates the agent alone with the console additions on or off, verified
//! and put back if it does not verify. Against the in-memory engine with crash injection
//! and a temporary machine-state directory, observed through the actor's console request
//! and status, its sockets, `resume`, engine state and machine state.

mod support;

use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::sync::atomic::AtomicBool;
use std::sync::Arc;
use std::time::Duration;

use quasar_recovery::actor::{Actor, ActorConfig, ReplaceTiming, TrustConfig};
use quasar_recovery::console::{ConsoleRequest, ConsoleStatus};
use quasar_recovery::engine::{Behaviour, FakeContainer, FakeEngine, FakeState, Lifecycle};
use quasar_recovery::journal::Phase;
use quasar_recovery::recipe::names;
use quasar_recovery::reconfigure::{ReconfigureRequest, Settled};
use quasar_recovery::socket::{
    Component, MachineRole, Reason, Release, Request, RequestKind, State,
};
use quasar_recovery::trust::{Caller, SignaturePolicy};
use support::*;

const MARKER: &str = "QUASAR_CONSOLE_ACCESS";
const NEW_AGENT: &str = "registry.example.invalid/quasar/quasar-node-agent@sha256:dd44000000000000000000000000000000000000000000000000000000000000";
const RELEASE_ID: &str = "7a1f6f1e-2c33-4a58-9a5e-0b6b0f7a1c22";

/// A rootful AMD GPU host whose agent image carries recipe revision 3.
fn rootful() -> FakeState {
    let mut state = amd_host();
    state
        .registry
        .insert(AGENT_IMAGE.into(), agent_image(Some("3")));
    state
}

struct Machine {
    engine: Arc<FakeEngine>,
    dir: tempfile::TempDir,
}

impl Machine {
    fn install(state: FakeState) -> Machine {
        let m = Machine {
            engine: Arc::new(FakeEngine::new(state)),
            dir: tempfile::tempdir().unwrap(),
        };
        m.actor().resume().expect("a clean install");
        m
    }

    fn config(&self) -> ActorConfig {
        let mut config = ActorConfig::new(self.dir.path(), MachineRole::Gpu, operator());
        config.self_container = Some(ACTOR_ID.into());
        config.new_installation_id = Box::new(|| INSTALLATION.to_string());
        config.now = Box::new(|| NOW.to_string());
        config.gpus_probe_backoff = Duration::ZERO;
        config.trust = TrustConfig {
            allowed_namespaces: vec!["registry.example.invalid/quasar".into()],
            signature: SignaturePolicy::default(),
        };
        config.timing = ReplaceTiming {
            verify_timeout: Duration::from_millis(150),
            poll: Duration::from_millis(1),
            stop_grace: Duration::from_secs(1),
            retries: 1,
            retry_backoff: Duration::ZERO,
        };
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

    fn agent(&self) -> FakeContainer {
        self.engine
            .state()
            .container_named(names::NODE_AGENT)
            .cloned()
            .expect("a node agent")
    }

    fn agents(&self) -> Vec<FakeContainer> {
        self.engine
            .state()
            .containers
            .values()
            .filter(|c| {
                c.spec
                    .labels
                    .get("io.quasar.platform-service")
                    .map(String::as_str)
                    == Some("node-agent")
            })
            .cloned()
            .collect()
    }

    fn inputs(&self) -> serde_json::Value {
        let m: serde_json::Value =
            serde_json::from_slice(&std::fs::read(self.dir.path().join("machine.json")).unwrap())
                .unwrap();
        m["inputs"].clone()
    }

    fn console_input(&self) -> bool {
        self.inputs()["console"].as_bool().unwrap_or(false)
    }

    /// One agent, running, under its name: nothing kept or left behind.
    fn one_running_agent(&self, at: &str) -> FakeContainer {
        let agents = self.agents();
        assert_eq!(agents.len(), 1, "{at}: {agents:#?}");
        let agent = &agents[0];
        assert_eq!(agent.spec.name, names::NODE_AGENT, "{at}");
        assert_eq!(agent.status, "running", "{at}");
        agent.clone()
    }
}

fn console_on(agent: &FakeContainer) -> bool {
    agent.spec.env.get(MARKER).map(String::as_str) == Some("1")
}

fn enable() -> ConsoleRequest {
    ConsoleRequest { enabled: true }
}

fn disable() -> ConsoleRequest {
    ConsoleRequest { enabled: false }
}

/// A console request driven to its end.
fn run(actor: &Arc<Actor>, req: ConsoleRequest) -> String {
    let id = actor
        .console(req)
        .expect("admitted")
        .expect("a replacement");
    actor.wait_attempt();
    id
}

fn release_request() -> Request {
    Request {
        request_id: RELEASE_ID.into(),
        kind: RequestKind::Replace,
        components: vec![Component {
            name: "node-agent".into(),
            image: "registry.example.invalid/quasar/quasar-node-agent".into(),
            digest: NEW_AGENT.split_once('@').unwrap().1.into(),
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
        from_version: None,
        force_again: false,
    }
}

fn with_release(mut state: FakeState) -> FakeState {
    let mut image = agent_image(Some("3"));
    image.id = "sha256:d0d0000000000000000000000000000000000000000000000000000000000000".into();
    image.repo_digests = vec![NEW_AGENT.into()];
    state.registry.insert(NEW_AGENT.into(), image);
    state
}

// ----- on and off -----

#[test]
fn enabling_replaces_only_the_agent_with_the_console_additions_and_disabling_takes_them_away() {
    let m = Machine::install(rootful());
    let actor = m.actor();
    let old = m.agent();
    assert!(!console_on(&old));
    let before = actor.console_status();
    assert_eq!(
        before,
        ConsoleStatus {
            enabled: false,
            in_flight: None,
            in_flight_target: None,
            in_flight_started_at: None,
            last: None,
            supported: true,
            why: None,
        }
    );
    let untouched: Vec<String> = m
        .engine
        .state()
        .containers
        .values()
        .filter(|c| c.spec.name != names::NODE_AGENT)
        .map(|c| c.id.clone())
        .collect();

    let id = run(&actor, enable());
    let result = actor.status_operator(Some(&id)).result.expect("journalled");
    assert_eq!(result.state, State::Succeeded, "{}", result.output);
    let agent = m.one_running_agent("enabled");
    assert_ne!(agent.id, old.id);
    assert_eq!(agent.spec.image, old.spec.image, "the same digest");
    assert!(console_on(&agent));
    assert_eq!(agent.spec.cap_add, vec!["SYS_ADMIN".to_string()]);
    assert!(agent
        .spec
        .binds
        .iter()
        .any(|b| b.source == "/dev/snd" && b.target == "/dev/snd"));
    assert!(m.console_input());
    let status = actor.console_status();
    assert!(status.enabled && status.in_flight.is_none(), "{status:?}");
    let last = status.last.expect("the change settled");
    assert!(last.target && !last.restored && last.reason.is_none());
    assert_eq!(last.settled, Settled::Applied);
    for id in &untouched {
        assert!(
            m.engine.state().containers.contains_key(id),
            "{id} was re-created"
        );
    }

    // Asked again, nothing is re-created.
    assert_eq!(actor.console(enable()).expect("answered"), None);
    assert_eq!(m.agent().id, agent.id);

    run(&actor, disable());
    let off = m.one_running_agent("disabled");
    assert!(!console_on(&off));
    assert!(off.spec.cap_add.is_empty());
    assert_eq!(off.spec.device_cgroup_rules, old.spec.device_cgroup_rules);
    assert_eq!(off.spec.binds, old.spec.binds);
    assert!(m.inputs().get("console").is_none(), "{}", m.inputs());
    let status = actor.console_status();
    assert!(!status.enabled);
    assert!(!status.last.unwrap().target);
    assert_eq!(actor.console(disable()).expect("answered"), None);
}

/// An operator reconfigure keeps console mode as it is, and cannot set it.
#[test]
fn the_operator_reconfigure_keeps_console_mode_and_cannot_set_it() {
    let m = Machine::install(rootful());
    let actor = m.actor();
    run(&actor, enable());
    for key in ["console", "QUASAR_CONSOLE", "QUASAR_CONSOLE_ACCESS"] {
        let refused = actor
            .reconfigure(ReconfigureRequest {
                changes: [(key.to_string(), "0".to_string())].into(),
                dry_run: false,
            })
            .expect_err(key);
        assert_eq!(refused.reason, Reason::Invalid, "{key}");
    }
    assert!(
        serde_json::from_str::<ReconfigureRequest>(r#"{"changes":{},"console":false}"#).is_err()
    );

    let admitted = actor
        .reconfigure(ReconfigureRequest {
            changes: [("QUASAR_APP_PUID".to_string(), "1001".to_string())].into(),
            dry_run: false,
        })
        .expect("admitted");
    assert!(admitted.request_id.is_some());
    actor.wait_attempt();
    let agent = m.one_running_agent("after the operator's reconfigure");
    assert_eq!(agent.spec.env["QUASAR_APP_PUID"], "1001");
    assert!(console_on(&agent), "console mode stays on");
    assert!(m.console_input());
    let status = actor.console_status();
    assert!(status.enabled && status.in_flight.is_none());
    assert!(
        status.last.is_none(),
        "the last reconfigure was not a console change"
    );
}

// ----- an agent that does not verify -----

fn failed_enable(m: &Machine, reason: Reason) {
    let actor = m.actor();
    let old = m.agent();
    let id = run(&actor, enable());
    let result = actor.status_operator(Some(&id)).result.expect("journalled");
    assert_eq!(result.state, State::Failed, "{reason:?}");
    assert_eq!(result.reason, Some(reason.clone()), "{}", result.output);
    assert!(result.restored, "{}", result.output);
    let agent = m.one_running_agent(&format!("{reason:?}"));
    assert_eq!(agent.id, old.id, "the previous agent is back");
    assert!(!console_on(&agent));
    assert!(!m.console_input(), "the old inputs are back");
    let status = actor.console_status();
    assert!(!status.enabled && status.in_flight.is_none());
    let last = status.last.expect("settled");
    assert_eq!(last.settled, Settled::PutBack);
    assert!(last.target && last.restored);
    assert_eq!(last.reason, Some(reason));
}

#[test]
fn an_agent_that_reports_unhealthy_is_put_back_without_console_mode() {
    let m = Machine::install(rootful());
    m.engine.with_state(|s| {
        s.behaviour.insert(
            AGENT_IMAGE.into(),
            Behaviour {
                health: Some("unhealthy".into()),
                ..Default::default()
            },
        );
    });
    failed_enable(&m, Reason::Unhealthy);
}

/// The engine refuses to start the new agent: from when the old one stops until the refused
/// one is removed, so the old one starts again.
#[test]
fn an_agent_that_never_starts_is_put_back_without_console_mode() {
    let m = Machine::install(rootful());
    let old = m.agent().id;
    let engine = Arc::downgrade(&m.engine);
    m.engine.on_lifecycle(move |event| {
        let refuse = match event {
            Lifecycle::Stopped(id) if *id == old => true,
            Lifecycle::Removed(_) => false,
            _ => return,
        };
        if let Some(engine) = engine.upgrade() {
            engine.with_state(|s| {
                s.behaviour.insert(
                    AGENT_IMAGE.into(),
                    Behaviour {
                        refuse_start: refuse.then(|| "error gathering device information".into()),
                        ..Default::default()
                    },
                );
            });
        }
    });
    failed_enable(&m, Reason::NeverStarted);
}

#[test]
fn an_agent_the_engine_will_not_create_is_put_back_without_console_mode() {
    let m = Machine::install(rootful());
    m.engine.with_state(|s| {
        s.host_devices.remove("/dev/dri");
    });
    failed_enable(&m, Reason::RecreateFailed);
}

// ----- a kill or an engine restart at every phase -----

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

/// The actor dies right after the agent's step commits `phase` (optionally the engine
/// restarts too) and the next start settles: `applied` with the console agent running, or
/// `put_back` with the previous one, and machine state never holds console mode no running
/// agent was verified with.
fn crash_then_settle(phase: Phase, daemon_restart: bool) -> Settled {
    let at = format!("{phase:?} daemon_restart={daemon_restart}");
    let m = Machine::install(rootful());
    let actor = m.actor_with(|c| {
        c.crash_after = Some(Box::new(move |name, p| name == "node-agent" && p == phase));
    });
    let id = actor
        .console(enable())
        .expect("admitted")
        .expect("an attempt");
    actor.wait_attempt();
    drop(actor);
    let open = m.actor().console_status();
    if open.in_flight.is_some() {
        assert_eq!(open.in_flight.as_deref(), Some(id.as_str()), "{at}");
        assert!(!open.enabled, "{at}: not in force before it settles");
    }
    if daemon_restart {
        m.engine.restart_daemon();
    }
    m.actor().resume().unwrap_or_else(|e| panic!("{at}: {e}"));

    let status = m.actor().console_status();
    assert!(status.in_flight.is_none(), "{at}");
    let last = status.last.clone().expect("settled");
    let agent = m.one_running_agent(&at);
    assert_eq!(console_on(&agent), m.console_input(), "{at}");
    assert_eq!(status.enabled, m.console_input(), "{at}");
    match last.settled {
        Settled::Applied => assert!(console_on(&agent), "{at}"),
        Settled::PutBack => assert!(!console_on(&agent), "{at}"),
        Settled::Partial => panic!("{at}: one service cannot be partly applied"),
    }
    if matches!(phase, Phase::Pulling | Phase::Checked) {
        assert_eq!(last.settled, Settled::PutBack, "{at}");
        assert_eq!(last.reason, Some(Reason::Interrupted), "{at}");
    }

    // Settling again changes nothing, and the machine takes the next request.
    m.actor().resume().unwrap();
    assert_eq!(m.actor().console_status(), status, "{at}");
    let next = if console_on(&agent) {
        disable()
    } else {
        enable()
    };
    let actor = m.actor();
    run(&actor, next.clone());
    assert_eq!(
        console_on(&m.one_running_agent(&at)),
        next.enabled,
        "{at}: the next request"
    );
    last.settled
}

#[test]
fn a_kill_at_every_phase_settles_applied_or_put_back() {
    let seen: std::collections::BTreeSet<String> = PHASES
        .iter()
        .map(|p| format!("{:?}", crash_then_settle(*p, false)))
        .collect();
    assert_eq!(
        seen,
        ["Applied", "PutBack"].map(String::from).into(),
        "both outcomes are reached"
    );
}

#[test]
fn a_kill_and_an_engine_restart_at_every_phase_settle_applied_or_put_back() {
    for phase in PHASES {
        crash_then_settle(phase, true);
    }
}

// ----- one attempt at a time -----

#[test]
fn a_console_change_is_busy_while_a_release_is_in_flight() {
    let m = Machine::install(with_release(rootful()));
    let actor = m.actor_with(|c| {
        c.crash_after = Some(Box::new(|name, p| {
            name == "node-agent" && p == Phase::OldKept
        }));
    });
    actor
        .submit(Caller::Agent, release_request())
        .expect("admitted");
    actor.wait_attempt();
    drop(actor);
    let refused = m.actor().console(enable()).expect_err("busy");
    assert_eq!(refused.reason, Reason::Busy, "{}", refused.message);
    assert!(!m.console_input());
    assert!(m.actor().console_status().in_flight.is_none());
}

#[test]
fn a_release_is_busy_while_a_console_change_is_in_flight_and_never_adopts_it() {
    let m = Machine::install(with_release(rootful()));
    let actor = m.actor_with(|c| {
        c.crash_after = Some(Box::new(|name, p| {
            name == "node-agent" && p == Phase::OldKept
        }));
    });
    let id = actor
        .console(enable())
        .expect("admitted")
        .expect("an attempt");
    actor.wait_attempt();
    drop(actor);
    let actor = m.actor();
    let refused = actor
        .submit(Caller::Agent, release_request())
        .expect_err("busy");
    assert_eq!(refused.reason, Reason::Busy, "{}", refused.message);
    assert_eq!(
        actor.console(disable()).expect_err("busy").reason,
        Reason::Busy
    );
    // The agent's release relay reads its own attempts only.
    let seen = actor.status_as(Caller::Agent, None);
    assert_eq!(seen.in_flight.as_deref(), Some(id.as_str()));
    assert!(seen.result.is_none());
    assert!(actor.status_as(Caller::Agent, Some(&id)).result.is_none());
}

// ----- where console mode cannot be given -----

#[test]
fn a_rootless_engine_is_refused_naming_rootless_console_mode() {
    let mut state = rootful();
    state.host.rootless = true;
    let m = Machine::install(state);
    let actor = m.actor();
    let old = m.agent().id;
    let status = actor.console_status();
    assert!(!status.supported);
    assert!(
        status.why.as_deref().unwrap().contains("#407"),
        "{status:?}"
    );
    let refused = actor.console(enable()).expect_err("rootless");
    assert_eq!(refused.reason, Reason::Invalid);
    assert!(refused.message.contains("#407"), "{}", refused.message);
    assert_eq!(m.agent().id, old);
    assert!(!m.console_input());
    assert_eq!(actor.console(disable()).expect("off is off"), None);
}

#[test]
fn a_host_without_dri_is_refused() {
    let mut state = host(PROBE_NONE, &["runc"], false, &["/dev/uinput", "/dev/kmsg"]);
    state
        .registry
        .insert(AGENT_IMAGE.into(), agent_image(Some("3")));
    let m = Machine::install(state);
    let actor = m.actor();
    assert!(!actor.console_status().supported);
    let refused = actor.console(enable()).expect_err("no /dev/dri");
    assert_eq!(refused.reason, Reason::Invalid);
    assert!(refused.message.contains("/dev/dri"), "{}", refused.message);
    assert!(!m.console_input());
}

#[test]
fn an_agent_on_an_older_recipe_revision_is_refused() {
    let mut state = amd_host();
    state
        .registry
        .insert(AGENT_IMAGE.into(), agent_image(Some("2")));
    let m = Machine::install(state);
    let actor = m.actor();
    let status = actor.console_status();
    assert!(!status.supported);
    assert!(status.why.unwrap().contains("revision 3"));
    let refused = actor.console(enable()).expect_err("revision 2");
    assert_eq!(refused.reason, Reason::Invalid);
    assert!(!m.console_input());
}

// ----- the sockets -----

fn raw(path: &std::path::Path, request: &str) -> (String, String) {
    let mut stream = UnixStream::connect(path).unwrap();
    stream.write_all(request.as_bytes()).unwrap();
    let mut out = String::new();
    stream.read_to_string(&mut out).unwrap();
    let (head, body) = out.split_once("\r\n\r\n").unwrap();
    (head.lines().next().unwrap().to_owned(), body.to_owned())
}

fn post(body: &str) -> String {
    format!(
        "POST /v1/console HTTP/1.0\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",
        body.len()
    )
}

fn serve(actor: &Arc<Actor>, dir: &std::path::Path, caller: Caller) -> std::path::PathBuf {
    let path = dir.join(format!("{caller:?}.sock"));
    let listener = quasar_recovery::server::bind(&path).unwrap();
    let serving = actor.clone();
    std::thread::spawn(move || {
        quasar_recovery::server::serve(listener, serving, caller, Arc::new(AtomicBool::new(false)))
    });
    path
}

#[test]
fn only_the_agent_socket_serves_console_mode() {
    let m = Machine::install(rootful());
    let actor = m.actor();
    let sockets = tempfile::tempdir().unwrap();
    let agent = serve(&actor, sockets.path(), Caller::Agent);
    let control = serve(&actor, sockets.path(), Caller::ControlPlane);

    // No body: the control socket answers without reading one.
    for request in [
        "GET /v1/console HTTP/1.0\r\n\r\n",
        "POST /v1/console HTTP/1.0\r\nContent-Length: 0\r\n\r\n",
    ] {
        let (status, _) = raw(&control, request);
        assert!(status.starts_with("HTTP/1.1 404"), "{status}");
    }
    assert!(!m.console_input(), "the control socket changed nothing");

    let (status, body) = raw(&agent, "GET /v1/console HTTP/1.0\r\n\r\n");
    assert!(status.starts_with("HTTP/1.1 200"), "{status}");
    assert_eq!(
        serde_json::from_str::<ConsoleStatus>(&body).unwrap(),
        actor.console_status()
    );
    let (status, _) = raw(&agent, "PUT /v1/console HTTP/1.0\r\n\r\n");
    assert!(status.starts_with("HTTP/1.1 405"), "{status}");
    let (status, body) = raw(&agent, &post(r#"{"enabled":true,"force":true}"#));
    assert!(status.starts_with("HTTP/1.1 400"), "{status}");
    assert!(body.contains("invalid"), "{body}");

    let (status, body) = raw(&agent, &post(r#"{"enabled":true}"#));
    assert!(status.starts_with("HTTP/1.1 202"), "{status} {body}");
    serde_json::from_str::<ConsoleStatus>(&body).expect("a console status");
    actor.wait_attempt();
    let (_, body) = raw(&agent, "GET /v1/console HTTP/1.0\r\n\r\n");
    let served: ConsoleStatus = serde_json::from_str(&body).unwrap();
    assert!(served.enabled);
    assert_eq!(served.last.unwrap().settled, Settled::Applied);
    let (status, _) = raw(&agent, &post(r#"{"enabled":true}"#));
    assert!(status.starts_with("HTTP/1.1 200"), "{status}");
}
