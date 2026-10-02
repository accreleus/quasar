//! Every start of the recovery actor checks the node agent it recorded against this
//! machine (#432): a GPU that changed, vanished or appeared, or an NVIDIA GPU the engine no
//! longer serves, re-creates the agent through a verified replacement; an agent the engine
//! left stopped is started, or re-created when it will not start. Against the in-memory
//! engine and a temporary machine-state directory, observed through the actor's start
//! check, its status, engine state and machine state.

mod support;

use std::sync::Arc;
use std::time::Duration;

use quasar_recovery::actor::{Actor, ActorConfig, ReplaceTiming};
use quasar_recovery::engine::{Behaviour, FakeContainer, FakeEngine, FakeState, RestartPolicy};
use quasar_recovery::recipe::{names, GpuInjection};
use quasar_recovery::socket::{MachineRole, Reason, State};
use support::*;

const CDI_REFUSAL: &str =
    "unable to start container: setting up CDI devices: unresolvable CDI devices nvidia.com/gpu=all";

/// A rootless Podman NVIDIA host whose engine serves the GPU by CDI, with an agent image
/// on recipe revision 3.
fn nvidia_cdi() -> FakeState {
    let mut state = nvidia_host(&[], true);
    state.host.rootless = true;
    state.host.kind = quasar_runtime::EngineKind::Podman;
    state.host.gpu_injection = Some(GpuInjection::Cdi);
    state
        .registry
        .insert(AGENT_IMAGE.into(), agent_image(Some("3")));
    state
}

fn amd_rootless() -> FakeState {
    let mut state = nvidia_cdi();
    state.probe_output = PROBE_AMD.into();
    state.host.gpu_injection = None;
    state.gpus_supported = false;
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

    fn actor(&self) -> Arc<Actor> {
        let mut config = ActorConfig::new(self.dir.path(), MachineRole::Gpu, operator());
        config.self_container = Some(ACTOR_ID.into());
        config.new_installation_id = Box::new(|| INSTALLATION.to_string());
        config.now = Box::new(|| NOW.to_string());
        config.gpus_probe_backoff = Duration::ZERO;
        config.timing = ReplaceTiming {
            verify_timeout: Duration::from_millis(150),
            poll: Duration::from_millis(1),
            stop_grace: Duration::from_secs(1),
            retries: 1,
            retry_backoff: Duration::ZERO,
        };
        Arc::new(Actor::new(self.engine.clone(), config))
    }

    /// The actor's next start: `resume`, then the start check.
    fn start(&self) -> (Arc<Actor>, Option<String>) {
        let actor = self.actor();
        actor.resume().expect("resume");
        let attempt = actor.recheck_on_start();
        actor.wait_attempt();
        (actor, attempt)
    }

    fn agent(&self) -> FakeContainer {
        self.engine
            .state()
            .container_named(names::NODE_AGENT)
            .cloned()
            .expect("a node agent")
    }

    fn agents(&self) -> usize {
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
            .count()
    }

    fn gpu_input(&self) -> serde_json::Value {
        let m: serde_json::Value =
            serde_json::from_slice(&std::fs::read(self.dir.path().join("machine.json")).unwrap())
                .unwrap();
        m["inputs"]["gpu"].clone()
    }

    /// What a reboot the engine could not start the agent after leaves.
    fn agent_left_exited(&self) {
        let id = self.agent().id;
        self.engine.with_state(|s| {
            let c = s.containers.get_mut(&id).unwrap();
            c.status = "exited".into();
            c.exit_code = Some(0);
        });
    }
}

fn render_node(agent: &FakeContainer) -> &str {
    agent
        .spec
        .env
        .get("QUASAR_RENDER_NODE")
        .map(String::as_str)
        .unwrap_or("")
}

fn cdi(agent: &FakeContainer) -> bool {
    agent
        .spec
        .gpus
        .iter()
        .any(|g| g.device_ids.iter().any(|d| d == "nvidia.com/gpu=all"))
}

fn succeeded(actor: &Actor, id: &str) {
    let result = actor.status_operator(Some(id)).result.expect("journalled");
    assert_eq!(result.state, State::Succeeded, "{}", result.output);
}

#[test]
fn an_nvidia_agent_is_re_created_for_the_amd_gpu_that_replaced_it() {
    let m = Machine::install(nvidia_cdi());
    let before = m.agent();
    assert!(cdi(&before) && render_node(&before) == "/dev/dri/renderD128");

    // The card is swapped between two boots: no NVIDIA CDI device any more, and the engine
    // could not start the agent.
    m.engine.with_state(|s| {
        s.probe_output = PROBE_AMD.into();
        s.host.gpu_injection = None;
        s.gpus_supported = false;
        s.gpus_refusal_message = CDI_REFUSAL.into();
    });
    m.agent_left_exited();

    let (actor, attempt) = m.start();
    let id = attempt.expect("a re-creation was admitted");
    succeeded(&actor, &id);
    let agent = m.agent();
    assert_ne!(agent.id, before.id);
    assert_eq!(agent.status, "running");
    assert!(agent.spec.gpus.is_empty(), "no NVIDIA request");
    assert_eq!(render_node(&agent), "/dev/dri/renderD129");
    assert!(agent
        .spec
        .devices
        .iter()
        .any(|d| d.host == "/dev/dri/renderD129"));
    assert!(!agent
        .spec
        .devices
        .iter()
        .any(|d| d.host == "/dev/dri/renderD128"));
    assert_eq!(m.agents(), 1, "the kept agent is discarded");
    let gpu = m.gpu_input();
    assert_eq!(gpu["vendor"], "amd");
    assert!(gpu.get("gpus_served").is_none() && gpu.get("cdi").is_none());
    assert!(actor.services_not_running().unwrap().is_empty());
    let record = actor.reconfigure_record().unwrap().expect("recorded");
    assert_eq!(record.changed, vec!["gpu".to_string()]);

    // The next start sees the machine it recorded: nothing is re-created.
    drop(actor);
    let (_, attempt) = m.start();
    assert_eq!(attempt, None);
    assert_eq!(m.agent().id, agent.id);
}

#[test]
fn an_amd_agent_is_re_created_with_the_nvidia_gpu_that_replaced_it() {
    let m = Machine::install(amd_rootless());
    let before = m.agent();
    assert!(before.spec.gpus.is_empty());
    m.engine.with_state(|s| {
        s.probe_output = PROBE_NVIDIA.into();
        s.host.gpu_injection = Some(GpuInjection::Cdi);
        s.gpus_supported = true;
    });
    m.agent_left_exited();

    let (actor, attempt) = m.start();
    succeeded(&actor, &attempt.expect("re-created"));
    let agent = m.agent();
    assert!(cdi(&agent), "the NVIDIA shape, by CDI");
    assert_eq!(render_node(&agent), "/dev/dri/renderD128");
    let gpu = m.gpu_input();
    assert_eq!(gpu["vendor"], "nvidia");
    assert_eq!(gpu["gpus_served"], true);
    assert_eq!(gpu["cdi"], true);
    drop(actor);
    assert_eq!(m.start().1, None, "no churn");
}

#[test]
fn a_gpu_gone_re_creates_the_agent_without_one_and_one_come_back_re_creates_it_again() {
    let m = Machine::install(nvidia_cdi());
    m.engine.with_state(|s| {
        s.probe_output = PROBE_NONE.into();
        s.host.gpu_injection = None;
        s.gpus_supported = false;
    });
    let (actor, attempt) = m.start();
    succeeded(&actor, &attempt.expect("re-created without a GPU"));
    let agent = m.agent();
    assert!(agent.spec.gpus.is_empty());
    assert_eq!(render_node(&agent), "");
    assert!(!agent
        .spec
        .devices
        .iter()
        .any(|d| d.host.starts_with("/dev/dri")));
    assert!(m.gpu_input().get("vendor").is_none_or(|v| v.is_null()));

    m.engine.with_state(|s| s.probe_output = PROBE_AMD.into());
    drop(actor);
    let (actor, attempt) = m.start();
    succeeded(&actor, &attempt.expect("re-created with the new GPU"));
    assert_eq!(render_node(&m.agent()), "/dev/dri/renderD129");
    assert_eq!(m.gpu_input()["vendor"], "amd");
}

/// Same card, but the engine lost its NVIDIA CDI specification (a driver update, a card
/// taken by a VM): the agent comes back without the NVIDIA shape instead of never starting.
#[test]
fn an_nvidia_cdi_specification_gone_re_creates_the_agent_without_the_nvidia_shape() {
    let m = Machine::install(nvidia_cdi());
    m.engine.with_state(|s| {
        s.host.gpu_injection = None;
        s.gpus_supported = false;
        s.gpus_refusal_message = CDI_REFUSAL.into();
    });
    m.agent_left_exited();
    let (actor, attempt) = m.start();
    succeeded(&actor, &attempt.expect("re-created"));
    let agent = m.agent();
    assert!(agent.spec.gpus.is_empty());
    assert_eq!(render_node(&agent), "/dev/dri/renderD128");
    let gpu = m.gpu_input();
    assert_eq!(gpu["vendor"], "nvidia");
    assert!(gpu.get("gpus_served").is_none());
}

/// The engine still lists the CDI device, but will not start the agent with it: its
/// refusal makes the actor ask again, and the probe's answer decides.
#[test]
fn an_agent_whose_cdi_request_the_engine_refuses_is_asked_again_and_re_created() {
    let m = Machine::install(nvidia_cdi());
    m.engine.with_state(|s| {
        s.gpus_supported = false;
        s.gpus_refusal_message = CDI_REFUSAL.into();
    });
    m.agent_left_exited();
    let (actor, attempt) = m.start();
    succeeded(&actor, &attempt.expect("re-created"));
    assert!(m.agent().spec.gpus.is_empty());
    assert!(m.gpu_input().get("gpus_served").is_none());
}

#[test]
fn an_unchanged_gpu_re_creates_nothing() {
    for state in [nvidia_cdi(), amd_rootless(), amd_host()] {
        let m = Machine::install(state);
        let before = m.engine.state().by_name();
        let (actor, attempt) = m.start();
        assert_eq!(attempt, None);
        assert_eq!(m.engine.state().by_name(), before, "nothing was touched");
        assert!(actor.reconfigure_record().unwrap().is_none());
        assert!(actor.services_not_running().unwrap().is_empty());
    }
}

/// The installed-and-running line cannot be wrong: an agent the engine left stopped at
/// boot, on a machine that did not change, is started.
#[test]
fn an_exited_agent_on_an_unchanged_machine_is_started() {
    let m = Machine::install(nvidia_cdi());
    let before = m.agent();
    m.agent_left_exited();
    let (actor, attempt) = m.start();
    assert_eq!(attempt, None, "started, not replaced");
    let agent = m.agent();
    assert_eq!(agent.id, before.id);
    assert_eq!(agent.status, "running");
    assert_eq!(agent.starts, before.starts + 1);
    assert!(actor.services_not_running().unwrap().is_empty());
}

/// Quasar sets the policy to `no` before any stop of its own: that agent is left alone.
#[test]
fn an_agent_stopped_by_quasar_is_left_stopped() {
    let m = Machine::install(amd_host());
    m.agent_left_exited();
    let id = m.agent().id;
    m.engine
        .with_state(|s| s.containers.get_mut(&id).unwrap().restart = RestartPolicy::No);
    let (actor, attempt) = m.start();
    assert_eq!(attempt, None);
    assert_eq!(m.agent().status, "exited");
    let stopped = actor.services_not_running().unwrap();
    assert_eq!(stopped, vec![format!("{} (exited)", names::NODE_AGENT)]);
}

/// An agent the engine will not start, for a reason no re-render changes, still goes
/// through the verified replacement once, and its failure is reported as it is.
#[test]
fn an_agent_that_will_not_start_is_re_created_once_and_reported_truthfully() {
    let m = Machine::install(amd_host());
    m.agent_left_exited();
    m.engine.with_state(|s| {
        s.behaviour.insert(
            AGENT_IMAGE.into(),
            Behaviour {
                refuse_start: Some("OCI runtime create failed".into()),
                ..Default::default()
            },
        );
    });
    let (actor, attempt) = m.start();
    let id = attempt.expect("a re-creation was admitted");
    let result = actor.status_operator(Some(&id)).result.unwrap();
    assert_eq!(result.state, State::Failed);
    assert_eq!(result.reason, Some(Reason::NeverStarted));
    assert!(!result.restored, "{}", result.output);
    assert_eq!(m.agents(), 1, "the new one is removed, the old one is back");
    assert_eq!(m.agent().spec.name, names::NODE_AGENT);
    assert!(!actor.services_not_running().unwrap().is_empty());
    let record = actor.reconfigure_record().unwrap().unwrap();
    assert_eq!(record.changed, vec!["agent-unstartable".to_string()]);
    assert_eq!(record.before, record.after);

    // The failure is not retried within this start.
    assert!(actor.status().in_flight.is_none());
    let journals = std::fs::read_dir(m.dir.path().join("journal"))
        .unwrap()
        .filter(|e| {
            let name = e.as_ref().unwrap().file_name();
            name.to_string_lossy().ends_with(".json")
        })
        .count();
    assert!(journals <= 1, "{journals} attempts");
}

// ----- the start's closing line (#432) -----

#[derive(Clone, Default)]
struct Captured(Arc<std::sync::Mutex<Vec<u8>>>);

impl std::io::Write for Captured {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(buf);
        Ok(buf.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

impl<'a> tracing_subscriber::fmt::MakeWriter<'a> for Captured {
    type Writer = Captured;
    fn make_writer(&'a self) -> Self::Writer {
        self.clone()
    }
}

/// The lines one start logs, `resume` then `finish_start`, as the binary runs them.
fn logged_start(m: &Machine) -> Vec<String> {
    let captured = Captured::default();
    let subscriber = tracing_subscriber::fmt()
        .with_writer(captured.clone())
        .with_ansi(false)
        .finish();
    tracing::subscriber::with_default(subscriber, || {
        let actor = m.actor();
        let resumed = actor.resume();
        actor.finish_start(resumed.is_ok());
        actor.wait_attempt();
    });
    let bytes = captured.0.lock().unwrap().clone();
    String::from_utf8(bytes)
        .unwrap()
        .lines()
        .map(str::to_owned)
        .collect()
}

const RUNNING_LINE: &str = "this machine's services are installed and running";

fn position(lines: &[String], needle: &str) -> Vec<usize> {
    lines
        .iter()
        .enumerate()
        .filter(|(_, l)| l.contains(needle))
        .map(|(i, _)| i)
        .collect()
}

/// An agent exited at the actor's start: nothing claims the services run until the start
/// check has started it, and then it is said once.
#[test]
fn an_exited_agent_is_started_before_the_start_says_the_services_run() {
    let m = Machine::install(amd_host());
    m.agent_left_exited();
    let lines = logged_start(&m);
    let started = position(&lines, "actor-agent-started");
    let running = position(&lines, RUNNING_LINE);
    assert_eq!(started.len(), 1, "{lines:#?}");
    assert_eq!(running.len(), 1, "said once: {lines:#?}");
    assert!(running[0] > started[0], "said after the start: {lines:#?}");
    assert_eq!(m.agent().status, "running");
}

/// An agent the start check leaves stopped is named, and nothing says the services run.
#[test]
fn a_start_that_leaves_the_agent_stopped_never_says_the_services_run() {
    let m = Machine::install(amd_host());
    m.agent_left_exited();
    let id = m.agent().id;
    m.engine
        .with_state(|s| s.containers.get_mut(&id).unwrap().restart = RestartPolicy::No);
    let lines = logged_start(&m);
    assert!(position(&lines, RUNNING_LINE).is_empty(), "{lines:#?}");
    let stopped = position(&lines, "actor-services-not-running");
    assert_eq!(stopped.len(), 1, "{lines:#?}");
    assert!(lines[stopped[0]].contains(names::NODE_AGENT));
}

/// A GPU change: the closing line says the agent is being re-created, not that it runs.
#[test]
fn a_start_that_re_creates_the_agent_says_so_instead() {
    let m = Machine::install(nvidia_cdi());
    m.engine.with_state(|s| {
        s.probe_output = PROBE_AMD.into();
        s.host.gpu_injection = None;
        s.gpus_supported = false;
    });
    m.agent_left_exited();
    let lines = logged_start(&m);
    assert!(position(&lines, RUNNING_LINE).is_empty(), "{lines:#?}");
    let changed = position(&lines, "actor-gpu-changed");
    let recreating = position(&lines, "actor-services-recreating");
    assert_eq!(recreating.len(), 1, "{lines:#?}");
    assert!(
        changed.len() == 1 && changed[0] < recreating[0],
        "{lines:#?}"
    );
}

impl Machine {
    /// A Podman reboot (#439): `/run` emptied, so the agent's runtime directory is gone, and
    /// the engine could not start the agent. Every other host directory a container binds is
    /// still there.
    fn rebooted_without_runtime_dir(&self) {
        self.engine.with_state(|s| {
            let dirs = s
                .containers
                .values()
                .flat_map(|c| c.spec.binds.iter())
                .filter(|b| !b.is_volume() && b.source != "/run/quasar-agent")
                .map(|b| b.source.clone())
                .chain(["/dev".to_string(), "/run".to_string()])
                .collect();
            s.host_dirs = Some(dirs);
        });
        self.agent_left_exited();
    }

    fn runtime_dir_exists(&self) -> bool {
        self.engine
            .state()
            .host_dirs
            .is_some_and(|d| d.contains("/run/quasar-agent"))
    }
}

/// #439: after a reboot Podman cannot start the agent, its runtime directory being gone
/// with the rest of `/run`. The actor has the engine make it before anything starts the
/// agent, so the agent is simply started: the same container, nothing re-created, and
/// the helper that made it is gone.
#[test]
fn the_runtime_directory_is_made_before_the_agent_is_started_after_a_reboot() {
    let m = Machine::install(nvidia_cdi());
    let before = m.agent();
    m.rebooted_without_runtime_dir();
    assert!(!m.runtime_dir_exists());

    let actor = m.actor();
    actor.make_agent_runtime_dir();
    assert!(m.runtime_dir_exists(), "the engine made it");
    assert!(
        m.engine
            .state()
            .container_named(names::RUNTIME_DIR_HELPER)
            .is_none(),
        "the helper is removed"
    );
    drop(actor);
    let (actor, attempt) = m.start();
    assert_eq!(attempt, None, "nothing re-created");
    let agent = m.agent();
    assert_eq!(agent.id, before.id, "the same container");
    assert_eq!(agent.status, "running");
    assert!(actor.services_not_running().unwrap().is_empty());
}

/// Without it, the start check can only re-create the agent through a verified
/// replacement, as #432 does for any agent the engine will not start.
#[test]
fn without_the_runtime_directory_the_agent_is_re_created_instead() {
    let m = Machine::install(nvidia_cdi());
    let before = m.agent();
    m.rebooted_without_runtime_dir();
    let (actor, attempt) = m.start();
    succeeded(&actor, &attempt.expect("re-created"));
    assert_ne!(m.agent().id, before.id);
}

/// A machine with no node agent has no runtime directory to make.
#[test]
fn a_machine_without_an_agent_makes_no_runtime_directory() {
    let m = Machine::install(nvidia_cdi());
    m.engine
        .with_state(|s| s.host_dirs = Some(Default::default()));
    m.engine
        .with_state(|s| s.containers.retain(|_, c| c.spec.name != names::NODE_AGENT));
    std::fs::remove_file(m.dir.path().join("services").join("node-agent.json")).ok();
    m.actor().make_agent_runtime_dir();
    assert!(!m.runtime_dir_exists(), "no helper was created");
}
