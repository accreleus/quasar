//! Diagnostic registration (#256) against the scripted Engine double: a startup cleanup
//! that cannot finish registers, reports, refuses and retries; when the engine returns
//! the same obligation completes under the same identity and startup resumes once.

use super::*;
use crate::diagnostic::{self, Phase, Station, STARTUP_CLEANUP_ID};
use crate::messages::{AgentMsg, HostCapacity, ReadinessCheck};
use crate::readiness::runtime_facts::{self, RuntimeFault, RuntimeView};
use tokio::sync::mpsc;
use tokio_tungstenite::tungstenite::{Error as WsError, Message};

const BOUND: Duration = Duration::from_secs(20);
const OPERATION: &str = "session-fixture-diagnostic";

/// A prior agent's application, left non-terminal by SIGKILL.
fn leave_an_application_behind(engine: &Engine) {
    engine
        .client()
        .start_application(ApplicationRequest {
            operation: OPERATION.into(),
            name: "quasar-sess-fixture-diagnostic".into(),
            image: "quasar-app:test".into(),
            ..Default::default()
        })
        .wait()
        .unwrap();
    engine.state.lock().unwrap().keep_running = true;
}

fn runtime_checks(client: &RuntimeClient) -> Vec<ReadinessCheck> {
    let view = RuntimeView::Observed {
        endpoint: client.endpoint(),
        outcome: client.inspect_engine().wait().map_err(RuntimeFault::from),
    };
    vec![runtime_facts::check_runtime_endpoint(&view)]
}

fn capacity_template() -> AgentMsg {
    AgentMsg::Capacity {
        deployment_settings: None,
        config_policy_accepted_groups: None,
        config_policy_legacy_map_applied_id: None,

        source_preparation: None,
        host: HostCapacity {
            cpu_cores: 1,
            mem_mb: 1,
            storage: None,
            cpu_model: None,
        },
        gpus: Vec::new(),
        gpu_detection: "none".into(),
        gpu_detection_reason: None,
        console_capabilities: None,
        effective_settings: None,
        codecs: None,
        codec_throughput: None,
        readiness: None,
    }
}

fn register() -> AgentMsg {
    AgentMsg::Register {
        source_policy_versions: None,
        config_policy_versions: None,
        config_policy_groups: None,
        terminal_home_cleanup_v1: None,
        node_name: "fixture".into(),
        agent_version: "test".into(),
        auth: crate::messages::Auth::Reconnect {
            node_secret: "secret".into(),
        },
        images: Vec::new(),
        image_cleanup_v1: true,
        image_versions_complete: false,
        image_versions: Vec::new(),
        source_commit: None,
        built_at: None,
        install_mode: None,
        updater_present: None,
        recovery_actor_version: None,
        recovery_actor_source_commit: None,
        seed_version: None,
    }
}

/// The control plane's half of an in-memory connection.
struct ControlPlane {
    to_agent: mpsc::UnboundedSender<Message>,
    from_agent: mpsc::UnboundedReceiver<Message>,
}

impl ControlPlane {
    fn say(&self, value: Value) {
        self.to_agent
            .send(Message::Text(value.to_string().into()))
            .unwrap();
    }

    async fn hear(&mut self) -> Value {
        let message = tokio::time::timeout(BOUND, self.from_agent.recv())
            .await
            .expect("the agent said nothing within the bound")
            .expect("the agent hung up");
        serde_json::from_str(message.to_text().unwrap()).unwrap()
    }

    async fn hear_type(&mut self, kind: &str) -> Value {
        loop {
            let value = self.hear().await;
            if value["type"] == kind {
                return value;
            }
        }
    }
}

type AgentSink = std::pin::Pin<Box<dyn futures_util::Sink<Message, Error = WsError> + Send>>;
type AgentStream =
    std::pin::Pin<Box<dyn futures_util::Stream<Item = Result<Message, WsError>> + Send>>;

fn connection() -> (ControlPlane, AgentSink, AgentStream) {
    let (to_agent, agent_rx) = mpsc::unbounded_channel::<Message>();
    let (agent_tx, from_agent) = mpsc::unbounded_channel::<Message>();
    let sink = futures_util::sink::unfold(agent_tx, |tx, message: Message| async move {
        tx.send(message).map_err(|_| WsError::ConnectionClosed)?;
        Ok::<_, WsError>(tx)
    });
    let stream = futures_util::stream::unfold(agent_rx, |mut rx| async move {
        rx.recv().await.map(|message| (Ok(message), rx))
    });
    (
        ControlPlane {
            to_agent,
            from_agent,
        },
        Box::pin(sink),
        Box::pin(stream),
    )
}

fn readiness_of(capacity: &Value) -> Vec<(String, String)> {
    capacity["readiness"]
        .as_array()
        .expect("capacity carries readiness")
        .iter()
        .map(|c| {
            (
                c["id"].as_str().unwrap().to_owned(),
                c["status"].as_str().unwrap().to_owned(),
            )
        })
        .collect()
}

#[test]
fn an_unreachable_engine_with_no_journal_is_still_a_fault() {
    let engine = Engine::new();
    engine.state.lock().unwrap().unreachable = true;
    let attempt = diagnostic::startup_cleanup(&engine.client());
    assert!(attempt.engine.is_err(), "{attempt:?}");
    assert!(Station::enter(&attempt).is_some());
    assert_eq!(engine.requests("DELETE /containers/"), 0);
    assert_eq!(engine.requests("POST /containers/"), 0);
}

#[test]
fn a_clean_startup_cleanup_never_enters_diagnostic_mode() {
    let engine = Engine::new();
    let attempt = diagnostic::startup_cleanup(&engine.client());
    assert_eq!(attempt.fault(), None, "{attempt:?}");
    assert!(Station::enter(&attempt).is_none());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_failing_retirement_sweep_registers_refuses_and_resumes_under_the_same_identity() {
    let engine = Engine::new();
    leave_an_application_behind(&engine);
    let created = engine.requests("POST /containers/create");
    engine.state.lock().unwrap().unreachable = true;

    // Boot.
    let client = engine.client();
    let first = {
        let client = client.clone();
        tokio::task::spawn_blocking(move || diagnostic::startup_cleanup(&client))
            .await
            .unwrap()
    };
    assert!(first.retirement.is_err(), "{first:?}");
    let station = Station::enter(&first).expect("an unresolved retirement is diagnostic mode");
    assert!(matches!(station.phase(), Phase::Diagnostic(_)));
    assert!(station.host_probes_withheld());

    for work in diagnostic::WITHHELD {
        assert!(!station.phase().may_start(work), "{work:?}");
    }

    // The retry runs with no connection at all, and keeps failing while the engine is away.
    let retry = {
        let (station, client) = (station.clone(), client.clone());
        tokio::spawn(diagnostic::retry_until_resumed(
            station,
            move || diagnostic::startup_cleanup(&client),
            Duration::from_millis(50),
        ))
    };

    let (mut control_plane, mut sink, mut stream) = connection();
    let served = {
        let (station, client) = (station.clone(), client.clone());
        tokio::spawn(async move {
            diagnostic::serve_connection(
                &mut sink,
                &mut stream,
                register(),
                &station,
                move || (capacity_template(), runtime_checks(&client)),
                Duration::from_millis(100),
            )
            .await
        })
    };

    // Registration.
    assert_eq!(control_plane.hear().await["type"], "register");
    control_plane.say(json!({"type": "registered", "host_id": "h1", "heartbeat_interval_ms": 50}));

    // A failing runtime check and the retained blocking check.
    let capacity = control_plane.hear_type("capacity").await;
    let readiness = readiness_of(&capacity);
    assert!(
        readiness.contains(&(runtime_facts::ENDPOINT_ID.to_owned(), "fail".to_owned())),
        "{readiness:?}"
    );
    assert!(
        readiness.contains(&(STARTUP_CLEANUP_ID.to_owned(), "fail".to_owned())),
        "{readiness:?}"
    );
    // A later local refresh never drops it.
    let again = readiness_of(&control_plane.hear_type("capacity").await);
    assert!(again.contains(&(STARTUP_CLEANUP_ID.to_owned(), "fail".to_owned())));

    // A refused launch, with its reason.
    control_plane.say(json!({
        "type": "session_assign", "id": "m1", "session_id": "s1", "gpu_index": 0,
        "app": {"image": "quasar-app:test"},
        "stream": {"width": 1280, "height": 720, "fps": 60, "bitrate_kbps": 8000,
                   "h264_profile": "constrained-baseline"},
    }));
    let ack = control_plane.hear_type("ack").await;
    assert_eq!(
        (ack["id"].as_str(), ack["ok"].as_bool()),
        (Some("m1"), Some(false))
    );
    let reason = ack["error"].as_str().unwrap();
    assert!(reason.contains("diagnostic mode"), "{reason}");
    assert!(reason.contains("runtime_unusable"), "{reason}");

    // Heartbeats keep the host visible.
    control_plane.hear_type("heartbeat").await;

    // Nothing was mutated or issued while the engine was away.
    assert_eq!(engine.requests("POST /containers/create"), created);
    assert!(deleted_ids(&engine).is_empty());
    assert!(!retry.is_finished());

    // The engine returns: the same obligation finishes, under the identity it was
    // journalled with, and the connection ends so normal startup can take over.
    engine.state.lock().unwrap().unreachable = false;
    // Fail fast on the fixture's own cause rather than waiting out the whole
    // bound against a listener nobody is serving any more.
    assert!(engine.alive(), "fixture died: {:?}", engine.last_error());
    tokio::time::timeout(BOUND, retry)
        .await
        .unwrap_or_else(|_| {
            panic!(
                "the retry did not resume within the bound; fixture last error: {:?}",
                engine.last_error()
            )
        })
        .unwrap();
    let end = tokio::time::timeout(BOUND, served)
        .await
        .unwrap_or_else(|_| {
            panic!(
                "the diagnostic connection outlived the resume; fixture last error: {:?}",
                engine.last_error()
            )
        })
        .unwrap()
        .unwrap();
    assert_eq!(end, diagnostic::ConnectionEnd::Resumed);

    assert_eq!(station.phase(), Phase::Normal);
    assert!(!station.host_probes_withheld());
    assert_eq!(deleted_ids(&engine), vec![ID.to_owned()]);
    assert_eq!(
        engine.requests("POST /containers/create"),
        created,
        "recovery issued a new operation"
    );
    assert!(
        !station
            .readiness(Vec::new())
            .iter()
            .any(|c| c.id == STARTUP_CLEANUP_ID),
        "the blocking check outlived the resume"
    );
    for work in diagnostic::WITHHELD {
        assert!(station.phase().may_start(work), "{work:?}");
    }

    // Exactly once: a later pass, even a failing one, changes nothing.
    engine.state.lock().unwrap().unreachable = true;
    let late = {
        let client = client.clone();
        tokio::task::spawn_blocking(move || diagnostic::startup_cleanup(&client))
            .await
            .unwrap()
    };
    assert_eq!(
        station.observe(&late),
        diagnostic::Transition::AlreadyNormal
    );
    assert_eq!(station.resumes(), 1);
}

/// The resume lands while the connection is inside its blocking `observe`, which is
/// exactly where every `serve_registered` arm body sits: the select has returned, so the
/// `station.resumed()` future — and with it the only watch receiver — is dropped for the
/// whole body. A resume published into that window and not retained is lost for good,
/// because a process resumes exactly once and nothing re-publishes it; the connection
/// then serves a host that has already resumed until the control plane hangs up (#269).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_resume_published_inside_the_blocking_observe_still_ends_the_connection() {
    let station = Station::enter(&diagnostic::CleanupAttempt {
        engine: Err("refused".into()),
        retirement: Err("unavailable".into()),
    })
    .expect("an unreachable engine is diagnostic mode");

    // Held, not dropped: a hung-up control plane would end the connection for the
    // wrong reason.
    let (_control_plane, mut sink, mut stream) = connection();
    let served = {
        let (station, resuming) = (station.clone(), station.clone());
        tokio::spawn(async move {
            diagnostic::serve_registered(
                &mut sink,
                &mut stream,
                60_000,
                &station,
                move || {
                    resuming.observe(&diagnostic::CleanupAttempt {
                        engine: Ok(()),
                        retirement: Ok(()),
                    });
                    (capacity_template(), Vec::new())
                },
                Duration::from_millis(10),
                None,
            )
            .await
        })
    };

    let end = tokio::time::timeout(BOUND, served)
        .await
        .expect("the connection outlived a resume published while no waiter was alive")
        .unwrap()
        .unwrap();
    assert_eq!(end, diagnostic::ConnectionEnd::Resumed);
    assert_eq!(station.resumes(), 1);
    assert_eq!(station.phase(), Phase::Normal);
}

/// Losing the control plane changes nothing about the retry.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_retry_needs_no_control_plane_connection() {
    let engine = Engine::new();
    leave_an_application_behind(&engine);
    engine.state.lock().unwrap().unreachable = true;
    let client = engine.client();
    let first = {
        let client = client.clone();
        tokio::task::spawn_blocking(move || diagnostic::startup_cleanup(&client))
            .await
            .unwrap()
    };
    let station = Station::enter(&first).unwrap();

    let (control_plane, mut sink, mut stream) = connection();
    let served = {
        let (station, client) = (station.clone(), client.clone());
        tokio::spawn(async move {
            diagnostic::serve_connection(
                &mut sink,
                &mut stream,
                register(),
                &station,
                move || (capacity_template(), runtime_checks(&client)),
                Duration::from_millis(100),
            )
            .await
        })
    };
    drop(control_plane);
    let end = tokio::time::timeout(BOUND, served).await.unwrap().unwrap();
    assert!(
        end.is_err(),
        "a closed connection is an error to reconnect from"
    );
    assert!(matches!(station.phase(), Phase::Diagnostic(_)));

    engine.state.lock().unwrap().unreachable = false;
    let retry_client = client.clone();
    tokio::time::timeout(
        BOUND,
        diagnostic::retry_until_resumed(
            station.clone(),
            move || diagnostic::startup_cleanup(&retry_client),
            Duration::from_millis(50),
        ),
    )
    .await
    .expect("the retry did not resume without a connection");
    assert_eq!(station.phase(), Phase::Normal);
    assert_eq!(deleted_ids(&engine), vec![ID.to_owned()]);
}
