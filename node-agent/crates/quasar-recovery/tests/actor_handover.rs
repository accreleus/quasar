//! The recovery actor replacing itself (#362): two actor processes and one lease on the
//! in-memory engine, with crash injection in both processes, a Docker daemon restart at
//! every phase, and a temporary machine-state directory. Observed only through engine
//! state, `seed.json`, the actors' `status`, their agent socket and the seed's decision.
//!
//! Each actor container runs a "process": an `Actor` driven the way the binary drives it
//! (wait for the lease, serve the socket, `resume`), started when the engine starts the
//! container, killed when the engine stops or removes it, and restarted when it dies while
//! its restart policy says so.

mod support;

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, Weak};
use std::time::{Duration, Instant};

use quasar_recovery::actor::{
    Actor, ActorConfig, HandoverTiming, OperatorInputs, ReplaceTiming, TrustConfig,
};
use quasar_recovery::engine::{
    Behaviour, Container, ContainerSpec, EngineError, EngineHost, FakeContainer, FakeEngine, Fault,
    Image, Lifecycle, Network, PlatformEngine, RestartPolicy, Volume, When,
};
use quasar_recovery::journal::Phase;
use quasar_recovery::recipe::names;
use quasar_recovery::seed::{self, file, Decision};
use quasar_recovery::socket::{
    AttemptResult, Component, MachineRole, Reason, Release, Request, RequestKind, State, Status,
};
use quasar_recovery::trust::Caller;
use support::*;

const ACTOR_REPO: &str = "registry.example.invalid/quasar/quasar-recovery";
const OLD_DIGEST: &str = "sha256:cc33000000000000000000000000000000000000000000000000000000000000";
const NEW_DIGEST: &str = "sha256:dd44000000000000000000000000000000000000000000000000000000000000";
const NEW_ACTOR: &str = NEWER_IMAGE;
const AGENT_REPO: &str = "registry.example.invalid/quasar/quasar-node-agent";
const NEW_AGENT: &str = "registry.example.invalid/quasar/quasar-node-agent@sha256:ee55000000000000000000000000000000000000000000000000000000000000";
const NEW_AGENT_DIGEST: &str =
    "sha256:ee55000000000000000000000000000000000000000000000000000000000000";
const ID: &str = "0b9f5d3a-6c1e-4f2a-8d7b-1e2f3a4b5c6d";
const COMMIT: &str = "cccccccccccccccccccccccccccccccccccccccc";
const KEPT: &str = "quasar-recovery.kept";
const NEXT: &str = "quasar-recovery.next";
/// The seed fixture set the current tree writes until a release ships it
/// (`testdata/recovery/seed/README.md`).
const UNRELEASED_SET: &str = "unreleased";

/// The phases each process commits in a hand-over, in order.
const OLD_PHASES: &[Phase] = &[
    Phase::Pulling,
    Phase::Checked,
    Phase::CreatingSuccessor,
    Phase::StartingSuccessor,
    Phase::AwaitingSuccessor,
    Phase::HandingOver,
];
const NEW_PHASES: &[Phase] = &[
    Phase::SuccessorActive,
    Phase::SuccessorRenaming,
    Phase::Verifying,
    Phase::Verified,
    Phase::OldDiscarded,
    Phase::Done,
];

fn fast() -> ReplaceTiming {
    ReplaceTiming {
        verify_timeout: Duration::from_millis(300),
        poll: Duration::from_millis(1),
        stop_grace: Duration::from_secs(1),
        retries: 2,
        retry_backoff: Duration::ZERO,
    }
}

fn handover_timing() -> HandoverTiming {
    HandoverTiming {
        ready: Duration::from_secs(3),
        takeover: Duration::from_secs(3),
        verify: Duration::from_secs(3),
        agent_contact: Duration::from_secs(3),
        poll: Duration::from_millis(2),
        orphan_check: Duration::from_secs(3600),
    }
}

/// Which process a fault or an action applies to: by the image it runs.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Who {
    Old,
    New,
}

type Action = Box<dyn FnMut(&Lab, Who, &str, Phase) -> bool + Send>;

/// One process's view of the engine: once the process is gone, nothing it still runs
/// reaches the engine, as nothing of a dead process does.
struct ProcEngine {
    engine: Arc<FakeEngine>,
    alive: Arc<AtomicBool>,
}

impl ProcEngine {
    fn gate(&self) -> Result<(), EngineError> {
        if self.alive.load(Ordering::SeqCst) {
            Ok(())
        } else {
            Err(EngineError::Crashed)
        }
    }
}

impl PlatformEngine for ProcEngine {
    fn host(&self) -> Result<EngineHost, EngineError> {
        self.gate()?;
        self.engine.host()
    }
    fn inspect_image(&self, reference: &str) -> Result<Option<Image>, EngineError> {
        self.gate()?;
        self.engine.inspect_image(reference)
    }
    fn pull(&self, reference: &str) -> Result<(), EngineError> {
        self.gate()?;
        self.engine.pull(reference)
    }
    fn inspect_container(&self, name_or_id: &str) -> Result<Option<Container>, EngineError> {
        self.gate()?;
        self.engine.inspect_container(name_or_id)
    }
    fn list_containers(&self) -> Result<Vec<Container>, EngineError> {
        self.gate()?;
        self.engine.list_containers()
    }
    fn create_container(&self, spec: &ContainerSpec) -> Result<String, EngineError> {
        self.gate()?;
        self.engine.create_container(spec)
    }
    fn start_container(&self, id: &str) -> Result<(), EngineError> {
        self.gate()?;
        self.engine.start_container(id)
    }
    fn stop_container(&self, id: &str, grace: Duration) -> Result<(), EngineError> {
        self.gate()?;
        self.engine.stop_container(id, grace)
    }
    fn set_restart_policy(&self, id: &str, policy: RestartPolicy) -> Result<(), EngineError> {
        self.gate()?;
        self.engine.set_restart_policy(id, policy)
    }
    fn rename_container(&self, id: &str, name: &str) -> Result<(), EngineError> {
        self.gate()?;
        self.engine.rename_container(id, name)
    }
    fn remove_container(&self, id: &str) -> Result<(), EngineError> {
        self.gate()?;
        self.engine.remove_container(id)
    }
    fn wait_container(&self, id: &str, timeout: Duration) -> Result<i64, EngineError> {
        self.gate()?;
        self.engine.wait_container(id, timeout)
    }
    fn logs_tail(&self, id: &str, lines: usize) -> Result<String, EngineError> {
        self.gate()?;
        self.engine.logs_tail(id, lines)
    }
    fn upload_archive(&self, id: &str, path: &str, tar: Vec<u8>) -> Result<(), EngineError> {
        self.gate()?;
        self.engine.upload_archive(id, path, tar)
    }
    fn inspect_volume(&self, name: &str) -> Result<Option<Volume>, EngineError> {
        self.gate()?;
        self.engine.inspect_volume(name)
    }
    fn create_volume(
        &self,
        name: &str,
        labels: &BTreeMap<String, String>,
    ) -> Result<Volume, EngineError> {
        self.gate()?;
        self.engine.create_volume(name, labels)
    }
    fn remove_volume(&self, name: &str) -> Result<(), EngineError> {
        self.gate()?;
        self.engine.remove_volume(name)
    }
    fn inspect_network(&self, name: &str) -> Result<Option<Network>, EngineError> {
        self.gate()?;
        self.engine.inspect_network(name)
    }
    fn create_network(
        &self,
        name: &str,
        labels: &BTreeMap<String, String>,
    ) -> Result<Network, EngineError> {
        self.gate()?;
        self.engine.create_network(name, labels)
    }
    fn remove_network(&self, name: &str) -> Result<(), EngineError> {
        self.gate()?;
        self.engine.remove_network(name)
    }
}

struct Proc {
    actor: Arc<Actor>,
    crashed: Arc<AtomicBool>,
    booted: Arc<AtomicBool>,
    alive: Arc<AtomicBool>,
}

/// Every process that ever held the lease, by container id.
type Holders = Mutex<Vec<String>>;

impl Proc {
    fn kill(&self) {
        self.alive.store(false, Ordering::SeqCst);
        self.actor.kill();
    }
}

struct Lab {
    engine: Arc<FakeEngine>,
    dir: tempfile::TempDir,
    sockets: tempfile::TempDir,
    procs: Mutex<BTreeMap<String, Proc>>,
    /// After every committed phase: `true` makes the committing process die there.
    actions: Mutex<Vec<Action>>,
    /// Seed decisions that would have raced a hand-over.
    races: Mutex<Vec<String>>,
    /// Images whose containers start but whose process never runs (a broken binary).
    inert: Mutex<Vec<&'static str>>,
    /// Images whose process is given an agent socket it cannot bind.
    no_socket: Mutex<Vec<&'static str>>,
    /// The clocks a process is started with.
    handover: Mutex<HandoverTiming>,
    /// Whether the simulated node agent polls the agent socket, as its relay does.
    agent_polls: AtomicBool,
    /// Set by the first submit: the relay polls from then on, as the agent's does
    /// throughout an attempt. Not before, so every install makes the same engine calls.
    relaying: AtomicBool,
    /// Since when a node agent restarted by the daemon keeps asking a dark actor
    /// (`release::ReleaseManager::adopt_when_answered`), and how often it found it dark.
    reattach: Mutex<Option<Instant>>,
    attach_misses: std::sync::atomic::AtomicUsize,
    /// The test removed every actor container on purpose: until the seed has looked, a
    /// seed that would create one is right, not racing.
    operator_removed: AtomicBool,
    holders: Holders,
    me: Weak<Lab>,
    stop: AtomicBool,
}

fn view(c: &FakeContainer) -> Container {
    Container {
        id: c.id.clone(),
        name: c.spec.name.clone(),
        image: c.spec.image.clone(),
        image_id: String::new(),
        labels: c.spec.labels.clone(),
        status: c.status.clone(),
        running: c.status == "running",
        health: None,
        restart: Some(c.restart),
        mounts: Vec::new(),
        command: c
            .spec
            .entrypoint
            .iter()
            .flatten()
            .chain(c.spec.cmd.iter().flatten())
            .cloned()
            .collect(),
        env: c.spec.env.iter().map(|(k, v)| format!("{k}={v}")).collect(),
    }
}

fn is_actor(c: &FakeContainer) -> bool {
    c.spec.cmd.as_deref() == Some(&["actor".to_string()][..])
}

impl Lab {
    /// A GPU host installed the documented way: the seed started, it created the actor, and
    /// the actor installed the agent. The successor's image is in the registry.
    fn new() -> Arc<Lab> {
        let mut state = seeded_host(seed_env());
        let mut labels = BTreeMap::from([
            ("org.quasar.recipe".to_string(), "1".to_string()),
            ("org.quasar.version".to_string(), "0.7.0".to_string()),
        ]);
        state.registry.insert(
            NEW_ACTOR.into(),
            Image {
                id: "sha256:dd4a000000000000000000000000000000000000000000000000000000000000"
                    .into(),
                repo_digests: vec![NEW_ACTOR.into()],
                labels: labels.clone(),
            },
        );
        labels.remove("org.quasar.version");
        state.registry.insert(
            NEW_AGENT.into(),
            Image {
                id: "sha256:ee5a000000000000000000000000000000000000000000000000000000000000"
                    .into(),
                repo_digests: vec![NEW_AGENT.into()],
                labels,
            },
        );
        state.behaviour.insert(
            NEW_AGENT.into(),
            Behaviour {
                health: Some("healthy".into()),
                ..Default::default()
            },
        );
        let engine = Arc::new(FakeEngine::new(state));
        let lab = Arc::new_cyclic(|me| Lab {
            engine: engine.clone(),
            dir: tempfile::tempdir().unwrap(),
            sockets: tempfile::tempdir().unwrap(),
            procs: Mutex::new(BTreeMap::new()),
            actions: Mutex::new(Vec::new()),
            races: Mutex::new(Vec::new()),
            inert: Mutex::new(Vec::new()),
            no_socket: Mutex::new(Vec::new()),
            handover: Mutex::new(handover_timing()),
            agent_polls: AtomicBool::new(true),
            relaying: AtomicBool::new(false),
            reattach: Mutex::new(None),
            attach_misses: std::sync::atomic::AtomicUsize::new(0),
            operator_removed: AtomicBool::new(false),
            holders: Mutex::new(Vec::new()),
            me: me.clone(),
            stop: AtomicBool::new(false),
        });
        let weak = Arc::downgrade(&lab);
        engine.on_lifecycle(move |event| {
            if let Some(lab) = weak.upgrade() {
                lab.lifecycle(event);
            }
        });
        // The node agent's relay: it polls the attempt's status on the agent socket.
        let weak = Arc::downgrade(&lab);
        std::thread::spawn(move || loop {
            let Some(lab) = weak.upgrade() else { return };
            if lab.stop.load(Ordering::SeqCst) {
                return;
            }
            if lab.agent_polls.load(Ordering::SeqCst) {
                if lab.relaying.load(Ordering::SeqCst) {
                    let _ = quasar_recovery::server::fetch_status(&lab.socket());
                } else if lab.reattach.lock().unwrap().is_some() {
                    lab.agent_attach();
                }
            }
            drop(lab);
            std::thread::sleep(Duration::from_millis(5));
        });
        let weak = Arc::downgrade(&lab);
        std::thread::spawn(move || loop {
            let Some(lab) = weak.upgrade() else { return };
            if lab.stop.load(Ordering::SeqCst) {
                return;
            }
            lab.supervise();
            drop(lab);
            std::thread::sleep(Duration::from_millis(1));
        });
        assert!(matches!(lab.seed().step(), seed::Outcome::Created { .. }));
        lab.wait_until("the first install", |lab| {
            lab.engine
                .state()
                .container_named(names::NODE_AGENT)
                .is_some()
                && lab.serving().is_some()
        });
        lab
    }

    fn seed(&self) -> seed::Seed {
        seed(&self.engine, self.dir.path(), SEED_ID)
    }

    /// The operator removes every recovery-actor container.
    fn remove_every_actor(&self) {
        self.operator_removed.store(true, Ordering::SeqCst);
        for c in self.actors() {
            self.engine.remove_container(&c.id).unwrap();
        }
    }

    /// One look by the seed; it ends an operator's removal.
    fn seed_step(&self) -> seed::Outcome {
        let outcome = self.seed().step();
        self.operator_removed.store(false, Ordering::SeqCst);
        outcome
    }

    fn socket(&self) -> std::path::PathBuf {
        self.sockets.path().join("agent.sock")
    }

    fn lifecycle(&self, event: &Lifecycle) {
        match event {
            Lifecycle::Started(id) => {
                let state = self.engine.state();
                if let Some(c) = state.containers.get(id) {
                    if is_actor(c) {
                        self.launch(id, c.spec.image.clone());
                    }
                }
            }
            Lifecycle::Stopped(id) => self.end(id),
            Lifecycle::Removed(id) => {
                self.end(id);
                // The moment a container goes is when a machine could be left without one.
                self.check_seed(&format!("{id} removed"));
            }
        }
    }

    /// Start the process of actor container `id`, unless one runs.
    fn launch(&self, id: &str, image: String) {
        if self.procs.lock().unwrap().contains_key(id) {
            return;
        }
        if self.inert.lock().unwrap().iter().any(|i| *i == image) {
            return;
        }
        let who = if image == NEW_ACTOR {
            Who::New
        } else {
            Who::Old
        };
        let mut config =
            ActorConfig::new(self.dir.path(), MachineRole::Gpu, OperatorInputs::default());
        config.self_container = Some(id.into());
        config.seed_container = Some(SEED_ID.into());
        config.new_installation_id = Box::new(|| "an-id-the-actor-must-not-use".to_string());
        config.now = Box::new(|| NOW.to_string());
        config.gpus_probe_backoff = Duration::ZERO;
        config.trust = TrustConfig {
            allowed_namespaces: vec!["registry.example.invalid/quasar".into()],
            ..Default::default()
        };
        config.timing = fast();
        config.handover = *self.handover.lock().unwrap();
        config.socket_dir = if self.no_socket.lock().unwrap().iter().any(|i| *i == image) {
            // A directory under a regular file: nothing can be bound there.
            let file = self.sockets.path().join("not-a-directory");
            std::fs::write(&file, b"").unwrap();
            file
        } else {
            self.sockets.path().to_owned()
        };
        let crashed = Arc::new(AtomicBool::new(false));
        let flag = crashed.clone();
        let crashed_flag = crashed.clone();
        config.on_died = Box::new(move || flag.store(true, Ordering::SeqCst));
        let lab = self.me.clone();
        config.crash_after = Some(Box::new(move |component, phase| {
            lab.upgrade()
                .is_some_and(|lab| lab.after_commit(who, component, phase))
        }));
        let alive = Arc::new(AtomicBool::new(true));
        let engine = Arc::new(ProcEngine {
            engine: self.engine.clone(),
            alive: alive.clone(),
        });
        let actor = Arc::new(Actor::new(engine, config));
        let booted = Arc::new(AtomicBool::new(false));
        self.procs.lock().unwrap().insert(
            id.to_owned(),
            Proc {
                actor: actor.clone(),
                crashed,
                booted: booted.clone(),
                alive,
            },
        );
        let (lab, id) = (self.me.clone(), id.to_owned());
        std::thread::spawn(move || {
            match actor.acquire_lease_waiting() {
                Ok(()) => {
                    if let Some(lab) = lab.upgrade() {
                        lab.holders.lock().unwrap().push(id);
                    }
                    let _ = actor.serve();
                    let _ = actor.resume();
                }
                // The binary exits (`actor-lease-unavailable`); its policy decides the rest.
                Err(_) => crashed_flag.store(true, Ordering::SeqCst),
            }
            booted.store(true, Ordering::SeqCst);
        });
    }

    /// The process of container `id` is gone (its container was stopped or removed).
    fn end(&self, id: &str) {
        let gone = self.procs.lock().unwrap().remove(id);
        if let Some(p) = gone {
            p.kill();
        }
    }

    /// Processes that died or exited: the engine restarts their container when its policy
    /// says so, as Docker does.
    fn supervise(&self) {
        let ended: Vec<(String, bool)> = self
            .procs
            .lock()
            .unwrap()
            .iter()
            .filter(|(_, p)| p.actor.retired() || p.crashed.load(Ordering::SeqCst))
            .map(|(id, p)| (id.clone(), p.actor.retired()))
            .collect();
        for (id, _retired) in ended {
            self.end(&id);
            let restart = self.engine.with_state(|s| match s.containers.get_mut(&id) {
                Some(c) if c.status == "running" && c.restart == RestartPolicy::UnlessStopped => {
                    c.starts += 1;
                    Some(c.spec.image.clone())
                }
                Some(c) => {
                    c.status = "exited".into();
                    None
                }
                None => None,
            });
            if let Some(image) = restart {
                self.launch(&id, image);
            }
        }
    }

    /// The restarted node agent's attach: one look at the actor's status. An attempt in
    /// flight is adopted and polled from then on; a dark actor is asked again until the
    /// agent's window ends.
    fn agent_attach(&self) {
        let mut reattach = self.reattach.lock().unwrap();
        let Some(since) = *reattach else { return };
        match quasar_recovery::server::fetch_status(&self.socket()) {
            Ok(body) => {
                *reattach = None;
                let status: Status = serde_json::from_str(&body).unwrap();
                if status.result.is_some_and(|r| !r.state.is_terminal()) {
                    self.relaying.store(true, Ordering::SeqCst);
                }
            }
            Err(_) => {
                self.attach_misses.fetch_add(1, Ordering::SeqCst);
                if since.elapsed() > Duration::from_secs(90) {
                    *reattach = None;
                }
            }
        }
    }

    /// What a Docker daemon restart does: every process ends, and the containers whose
    /// policy restarts them come back. The node agent restarts too: it stops relaying and
    /// attaches afresh, first before any actor serves again.
    fn restart_daemon(&self) {
        let all: Vec<String> = self.procs.lock().unwrap().keys().cloned().collect();
        for id in all {
            self.end(&id);
        }
        self.relaying.store(false, Ordering::SeqCst);
        *self.reattach.lock().unwrap() = Some(Instant::now());
        self.agent_attach();
        self.engine.restart_daemon();
        for c in self.engine.state().containers.values() {
            if is_actor(c) && c.status == "running" {
                self.launch(&c.id, c.spec.image.clone());
            }
        }
    }

    /// Kill a process and leave its container exited: the engine cannot start it again.
    fn break_container(&self, id: &str) {
        self.end(id);
        self.engine.with_state(|s| {
            if let Some(c) = s.containers.get_mut(id) {
                c.status = "exited".into();
            }
        });
    }

    fn after_commit(&self, who: Who, component: &str, phase: Phase) -> bool {
        self.check_seed(&format!("{who:?} committed {component} {phase:?}"));
        let mut actions = self.actions.lock().unwrap();
        let mut crash = false;
        for action in actions.iter_mut() {
            crash |= action(self, who, component, phase);
        }
        crash
    }

    /// The seed, looking at the machine right now, must never act beside a hand-over.
    fn check_seed(&self, at: &str) {
        if self.operator_removed.load(Ordering::SeqCst) {
            return;
        }
        let state = self.engine.state();
        let containers: Vec<Container> = state.containers.values().map(view).collect();
        let decided = seed::decide(&file::read(self.dir.path()), &containers, Some(SEED_ID));
        if matches!(decided, Decision::Create { .. } | Decision::StartOwn { .. }) {
            self.races
                .lock()
                .unwrap()
                .push(format!("{at}: {decided:?}"));
        }
    }

    fn on(&self, action: impl FnMut(&Lab, Who, &str, Phase) -> bool + Send + 'static) {
        self.actions.lock().unwrap().push(Box::new(action));
    }

    /// The process that holds the lease and has finished starting, if exactly one does.
    fn serving(&self) -> Option<Arc<Actor>> {
        let procs = self.procs.lock().unwrap();
        let serving: Vec<_> = procs
            .values()
            .filter(|p| p.booted.load(Ordering::SeqCst) && p.actor.holds_lease())
            .collect();
        match serving.as_slice() {
            [only] => Some(only.actor.clone()),
            _ => None,
        }
    }

    fn actors(&self) -> Vec<FakeContainer> {
        let mut out: Vec<_> = self
            .engine
            .state()
            .containers
            .values()
            .filter(|c| {
                c.spec
                    .labels
                    .get("io.quasar.platform-service")
                    .map(String::as_str)
                    == Some("recovery-actor")
            })
            .cloned()
            .collect();
        out.sort_by(|a, b| a.spec.name.cmp(&b.spec.name));
        out
    }

    fn old_actor(&self) -> FakeContainer {
        self.engine
            .state()
            .container_named(names::RECOVERY_ACTOR)
            .unwrap()
            .clone()
    }

    fn wait_until(&self, what: &str, done: impl Fn(&Lab) -> bool) {
        let deadline = Instant::now() + Duration::from_secs(30);
        while !done(self) {
            self.check_seed(what);
            if Instant::now() >= deadline {
                let containers: Vec<String> = self
                    .engine
                    .state()
                    .containers
                    .values()
                    .map(|c| {
                        format!(
                            "{} {} {} {:?} {}",
                            c.id, c.spec.name, c.status, c.restart, c.spec.image
                        )
                    })
                    .collect();
                let procs: Vec<String> = self
                    .procs
                    .lock()
                    .unwrap()
                    .iter()
                    .map(|(id, p)| {
                        format!(
                            "{id} booted={} lease={} retired={}",
                            p.booted.load(Ordering::SeqCst),
                            p.actor.holds_lease(),
                            p.actor.retired()
                        )
                    })
                    .collect();
                panic!(
                    "{what}: never settled\ncontainers {containers:#?}\nprocesses {procs:#?}\njournal {:?}",
                    std::fs::read_to_string(self.dir.path().join("journal").join(format!("{ID}.json")))
                );
            }
            std::thread::sleep(Duration::from_millis(2));
        }
    }

    /// Submit on the running actor's agent socket path, as the agent's relay does.
    fn submit(&self, req: Request) -> Result<(), quasar_recovery::socket::Rejection> {
        let actor = self.serving().expect("an actor serves");
        self.relaying.store(true, Ordering::SeqCst);
        actor.submit(Caller::Agent, req).map(|_| ())
    }

    /// Wait until the attempt is terminal and exactly one actor runs, holds the lease and
    /// serves; return its outcome.
    fn outcome(&self, at: &str) -> AttemptResult {
        self.wait_until(at, |lab| {
            let Some(actor) = lab.serving() else {
                return false;
            };
            let terminal = actor
                .status_for(Some(ID))
                .result
                .is_some_and(|r| r.state.is_terminal());
            let actors = lab.actors();
            terminal
                && actors.len() == 1
                && actors[0].status == "running"
                && lab.procs.lock().unwrap().len() == 1
        });
        let races = self.races.lock().unwrap().clone();
        assert!(
            races.is_empty(),
            "{at}: the seed would have acted: {races:#?}"
        );
        self.serving().unwrap().status_for(Some(ID)).result.unwrap()
    }

    fn seed_file(&self) -> file::SeedFile {
        match file::read(self.dir.path()) {
            file::SeedRead::Found(f) => f,
            other => panic!("seed.json: {other:?}"),
        }
    }

    /// The machine after an attempt: one actor, under its name, restartable, running the
    /// image `seed.json` names, answering on the agent socket as that image.
    fn assert_one_actor(&self, image: &str, at: &str) {
        let actors = self.actors();
        assert_eq!(actors.len(), 1, "{at}: actor containers {actors:#?}");
        let actor = &actors[0];
        assert_eq!(actor.spec.name, names::RECOVERY_ACTOR, "{at}");
        assert_eq!(actor.spec.image, image, "{at}");
        assert_eq!(actor.status, "running", "{at}");
        assert_eq!(actor.restart, RestartPolicy::UnlessStopped, "{at}");
        assert_eq!(
            self.seed_file().recovery_actor_image.reference(),
            image,
            "{at}: seed.json"
        );
        let served: Status = serde_json::from_str(
            &quasar_recovery::server::fetch_status(&self.socket())
                .unwrap_or_else(|e| panic!("{at}: the agent socket: {e}")),
        )
        .unwrap();
        assert_eq!(
            served.actor.digest.as_deref(),
            image.split_once('@').map(|(_, d)| d),
            "{at}: the socket is served by another actor"
        );
        assert!(
            self.engine
                .state()
                .container_named(names::NODE_AGENT)
                .is_some(),
            "{at}: the agent is gone"
        );
    }
}

impl Drop for Lab {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        for (_, p) in std::mem::take(&mut *self.procs.lock().unwrap()) {
            p.kill();
        }
    }
}

fn request(components: Vec<Component>) -> Request {
    Request {
        request_id: ID.into(),
        kind: RequestKind::Replace,
        components,
        release: Release {
            id: "rel-0.7.0".into(),
            version: Some("0.7.0".into()),
            source_commit: COMMIT.into(),
        },
        migrates: false,
        schema_version: None,
        external_backup_confirmed: false,
        dump: None,
        purge: false,
        wait_timeout_s: 0,
    }
}

fn actor_component() -> Component {
    Component {
        name: "recovery-actor".into(),
        image: ACTOR_REPO.into(),
        digest: NEW_DIGEST.into(),
    }
}

fn agent_component() -> Component {
    Component {
        name: "node-agent".into(),
        image: AGENT_REPO.into(),
        digest: NEW_AGENT_DIGEST.into(),
    }
}

fn hand_over(lab: &Lab) {
    lab.submit(request(vec![actor_component()]))
        .expect("admitted");
}

fn assert_succeeded(lab: &Lab, result: &AttemptResult, at: &str) {
    assert_eq!(result.state, State::Succeeded, "{at}: {result:?}");
    assert!(!result.restored, "{at}");
    lab.assert_one_actor(NEW_ACTOR, at);
}

fn assert_restored(lab: &Lab, result: &AttemptResult, old: &FakeContainer, at: &str) {
    assert_eq!(result.state, State::Failed, "{at}: {result:?}");
    assert!(result.restored, "{at}: {result:?}");
    lab.assert_one_actor(ACTOR_IMAGE, at);
    assert_eq!(
        lab.actors()[0].id,
        old.id,
        "{at}: not the previous actor's container"
    );
}

fn assert_interrupted(lab: &Lab, result: &AttemptResult, old: &FakeContainer, at: &str) {
    assert_eq!(result.state, State::Failed, "{at}: {result:?}");
    assert_eq!(result.reason, Some(Reason::Interrupted), "{at}: {result:?}");
    assert!(!result.restored, "{at}");
    lab.assert_one_actor(ACTOR_IMAGE, at);
    assert_eq!(lab.actors()[0].id, old.id, "{at}");
}

/// A crash, or a daemon restart, right after one process committed `phase`.
fn at_phase(lab: &Lab, who: Who, phase: Phase, then: fn(&Lab) -> bool) {
    let mut fired = false;
    lab.on(move |lab, w, component, p| {
        if fired || w != who || component != "recovery-actor" || p != phase {
            return false;
        }
        fired = true;
        then(lab)
    });
}

#[test]
fn a_successor_takes_over_and_seed_json_names_it_only_once_it_verified() {
    let lab = Lab::new();
    let old = lab.old_actor();
    assert_eq!(
        lab.seed_file().recovery_actor_image.reference(),
        ACTOR_IMAGE
    );
    let seen = Arc::new(Mutex::new(Vec::new()));
    let log = seen.clone();
    let dir = lab.dir.path().to_path_buf();
    lab.on(move |_, who, component, phase| {
        if component == "recovery-actor" {
            let named = match file::read(&dir) {
                file::SeedRead::Found(f) => f.recovery_actor_image.reference(),
                other => format!("{other:?}"),
            };
            log.lock().unwrap().push((who, phase, named));
        }
        false
    });

    hand_over(&lab);
    let result = lab.outcome("hand-over");
    assert_succeeded(&lab, &result, "hand-over");
    assert_eq!(result.previous[0].digest.as_deref(), Some(OLD_DIGEST));
    assert!(lab.engine.state().container_named(KEPT).is_none());
    assert!(lab.engine.state().container_named(NEXT).is_none());
    assert!(!lab.engine.state().containers.contains_key(&old.id));

    // The two processes' halves, in order, and seed.json on the old image until the
    // successor had verified and the kept actor was removed.
    let seen = seen.lock().unwrap().clone();
    let order: Vec<(Who, Phase)> = seen.iter().map(|(w, p, _)| (*w, *p)).collect();
    let want: Vec<(Who, Phase)> = OLD_PHASES
        .iter()
        .map(|p| (Who::Old, *p))
        .chain(NEW_PHASES.iter().map(|p| (Who::New, *p)))
        .collect();
    assert_eq!(order, want);
    for (who, phase, named) in &seen {
        let want = if *phase == Phase::Done {
            NEW_ACTOR
        } else {
            ACTOR_IMAGE
        };
        assert_eq!(named, want, "{who:?} {phase:?}");
    }

    // The successor keeps the seed that created the actor it replaced.
    let now = lab.actors()[0].clone();
    assert_eq!(
        now.spec
            .env
            .get("QUASAR_SEED_CONTAINER")
            .map(String::as_str),
        Some(SEED_ID)
    );
    assert!(now.spec.labels.contains_key("io.quasar.recipe"));

    // A later start of the new actor changes nothing.
    let before = lab.engine.state().by_name();
    lab.restart_daemon();
    lab.wait_until("restart", |lab| lab.serving().is_some());
    assert_eq!(
        lab.engine.state().by_name().keys().collect::<Vec<_>>(),
        before.keys().collect::<Vec<_>>()
    );
    lab.assert_one_actor(NEW_ACTOR, "after a restart");
}

/// Recorded trust wins over the variables (`Actor::trust`), so a successor keeps the
/// running actor's `QUASAR_UPDATER_*` only on a machine whose state records none.
#[test]
fn a_successor_keeps_the_trust_variables_only_where_machine_state_records_none() {
    const VAR: &str = "QUASAR_UPDATER_ALLOWED_NAMESPACES";
    for recorded in [false, true] {
        let at = format!("trust recorded: {recorded}");
        let lab = Lab::new();
        let old = lab.old_actor();
        lab.engine.with_state(|s| {
            let c = s.containers.get_mut(&old.id).unwrap();
            c.spec
                .env
                .insert(VAR.into(), "registry.example.invalid/quasar".into());
        });
        if recorded {
            let path = lab.dir.path().join("machine.json");
            let mut machine: serde_json::Value =
                serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
            machine["inputs"]["trust"] =
                serde_json::json!({ "allowed_namespaces": "registry.example.invalid/quasar" });
            std::fs::write(&path, serde_json::to_vec(&machine).unwrap()).unwrap();
        }
        hand_over(&lab);
        let result = lab.outcome(&at);
        assert_succeeded(&lab, &result, &at);
        let env = &lab.actors()[0].spec.env;
        assert_eq!(env.contains_key(VAR), !recorded, "{at}: {env:?}");
        assert_eq!(
            env.get("QUASAR_SEED_CONTAINER").map(String::as_str),
            Some(SEED_ID),
            "{at}"
        );
    }
}

#[test]
fn a_crash_of_either_process_after_any_phase_settles_to_one_actor() {
    for (who, phases) in [(Who::Old, OLD_PHASES), (Who::New, NEW_PHASES)] {
        for &phase in phases {
            let at = format!("{who:?} dies after {phase:?}");
            let lab = Lab::new();
            let old = lab.old_actor();
            at_phase(&lab, who, phase, |_| true);
            hand_over(&lab);
            let result = lab.outcome(&at);
            match (who, phase) {
                // Before the old actor let go of the lease: it comes back and settles.
                (Who::Old, p) if p != Phase::HandingOver => {
                    assert_interrupted(&lab, &result, &old, &at)
                }
                // Both run and either may take the lease: two stated outcomes.
                (Who::Old, _) | (Who::New, Phase::SuccessorActive) => {
                    if result.state == State::Succeeded {
                        assert_succeeded(&lab, &result, &at)
                    } else {
                        assert_restored(&lab, &result, &old, &at)
                    }
                }
                // The old actor is stopped: the successor comes back and finishes.
                (Who::New, _) => assert_succeeded(&lab, &result, &at),
            }
        }
    }
}

#[test]
fn a_daemon_restart_after_any_phase_settles_to_one_actor() {
    for (who, phases) in [(Who::Old, OLD_PHASES), (Who::New, NEW_PHASES)] {
        for &phase in phases {
            let at = format!("daemon restart after {who:?} {phase:?}");
            let lab = Lab::new();
            let old = lab.old_actor();
            at_phase(&lab, who, phase, |lab| {
                lab.restart_daemon();
                false
            });
            hand_over(&lab);
            let result = lab.outcome(&at);
            match result.state {
                State::Succeeded => assert_succeeded(&lab, &result, &at),
                _ if result.reason == Some(Reason::Interrupted) => {
                    assert_interrupted(&lab, &result, &old, &at)
                }
                _ => assert_restored(&lab, &result, &old, &at),
            }
            if who == Who::New && phase != Phase::SuccessorActive {
                assert_eq!(
                    result.state,
                    State::Succeeded,
                    "{at}: the old actor was already stopped"
                );
            }
        }
    }
}

#[test]
fn a_crash_at_every_engine_call_of_a_hand_over_settles_to_one_actor() {
    let reference = Lab::new();
    let start = reference.engine.calls();
    hand_over(&reference);
    reference.outcome("reference");
    let total = reference.engine.calls() - start;
    assert!(total > 12, "the sweep must not be vacuous ({total} calls)");
    drop(reference);

    let mut outcomes = BTreeMap::new();
    for call in start..start + total {
        for when in [When::Before, When::After] {
            let at = format!("engine call {} {when:?}", call - start);
            let lab = Lab::new();
            assert_eq!(
                lab.engine.calls(),
                start,
                "{at}: installs must be identical"
            );
            let old = lab.old_actor();
            lab.engine.inject(Fault {
                call,
                when,
                error: EngineError::Crashed,
            });
            if lab.submit(request(vec![actor_component()])).is_err() {
                // The crash hit admission: nothing was journalled.
                lab.assert_one_actor(ACTOR_IMAGE, &at);
                continue;
            }
            let result = lab.outcome(&at);
            match result.state {
                State::Succeeded => assert_succeeded(&lab, &result, &at),
                _ if result.reason == Some(Reason::Interrupted) => {
                    assert_interrupted(&lab, &result, &old, &at)
                }
                _ => assert_restored(&lab, &result, &old, &at),
            }
            *outcomes
                .entry(format!("{:?} {:?}", result.state, result.reason))
                .or_insert(0) += 1;
        }
    }
    assert!(outcomes.len() >= 2, "{outcomes:?}");
}

#[test]
fn a_successor_that_never_reports_ready_is_removed_and_the_old_actor_keeps_running() {
    let lab = Lab::new();
    let old = lab.old_actor();
    lab.inert.lock().unwrap().push(NEW_ACTOR);
    hand_over(&lab);
    let result = lab.outcome("never ready");
    assert_restored(&lab, &result, &old, "never ready");
    assert_eq!(result.reason, Some(Reason::Unhealthy), "{result:?}");
    assert!(
        result.output.contains("did not report ready"),
        "{}",
        result.output
    );
    assert!(lab.engine.state().container_named(NEXT).is_none());
}

#[test]
fn a_successor_the_engine_refuses_to_start_never_started_and_is_restored() {
    let lab = Lab::new();
    let old = lab.old_actor();
    lab.engine.with_state(|s| {
        s.behaviour.insert(
            NEW_ACTOR.into(),
            Behaviour {
                refuse_start: Some("OCI runtime create failed".into()),
                ..Default::default()
            },
        );
    });
    hand_over(&lab);
    let result = lab.outcome("refused start");
    assert_restored(&lab, &result, &old, "refused start");
    assert_eq!(result.reason, Some(Reason::NeverStarted), "{result:?}");
    assert!(
        result.output.contains("OCI runtime create failed"),
        "{}",
        result.output
    );
}

#[test]
fn a_successor_that_cannot_verify_hands_the_machine_back() {
    let lab = Lab::new();
    let old = lab.old_actor();
    lab.no_socket.lock().unwrap().push(NEW_ACTOR);
    hand_over(&lab);
    let result = lab.outcome("cannot verify");
    assert_restored(&lab, &result, &old, "cannot verify");
    assert_eq!(result.reason, Some(Reason::Unhealthy), "{result:?}");
    assert!(
        result.output.contains("did not verify"),
        "{}",
        result.output
    );
}

#[test]
fn a_successor_that_keeps_crashing_after_the_old_actor_stopped_gives_the_machine_back() {
    let lab = Lab::new();
    let old = lab.old_actor();
    // Every start of the successor dies at its next step, until it gives up.
    lab.on(|_, who, component, phase| {
        who == Who::New
            && component == "recovery-actor"
            && matches!(
                phase,
                Phase::SuccessorRenaming | Phase::Verifying | Phase::Verified | Phase::OldDiscarded
            )
    });
    hand_over(&lab);
    let result = lab.outcome("crash loop");
    assert_restored(&lab, &result, &old, "crash loop");
    assert!(
        result.output.contains("never verified"),
        "{}",
        result.output
    );
}

#[test]
fn a_successor_that_cannot_start_after_the_old_actor_stopped_needs_one_command() {
    for fix in ["start the kept actor", "remove every actor container"] {
        let lab = Lab::new();
        let old = lab.old_actor();
        at_phase(&lab, Who::New, Phase::SuccessorRenaming, |lab| {
            // The daemon restarts and the successor's container no longer starts.
            let next = lab.engine.state().container_named(NEXT).unwrap().id.clone();
            lab.break_container(&next);
            true
        });
        hand_over(&lab);
        lab.wait_until(fix, |lab| {
            lab.procs.lock().unwrap().is_empty()
                && lab.actors().iter().all(|c| c.status != "running")
        });
        // Nobody runs, and the seed rightly does nothing while actor containers exist.
        assert!(
            matches!(lab.seed().step(), seed::Outcome::Present { .. }),
            "{fix}"
        );
        match fix {
            "start the kept actor" => {
                // `docker start quasar-recovery.kept`
                lab.engine.start_container(KEPT).unwrap();
                let result = lab.outcome(fix);
                assert_restored(&lab, &result, &old, fix);
            }
            _ => {
                lab.remove_every_actor();
                assert!(
                    matches!(lab.seed_step(), seed::Outcome::Created { .. }),
                    "{fix}"
                );
                let result = lab.outcome(fix);
                assert_eq!(result.state, State::Failed, "{fix}: {result:?}");
                // The seed re-created the last verified actor, the previous one.
                assert!(result.restored, "{fix}: {result:?}");
                lab.assert_one_actor(ACTOR_IMAGE, fix);
            }
        }
    }
}

#[test]
fn with_every_actor_container_removed_the_seed_recreates_the_verified_actor() {
    for (who, phase) in [
        (Who::Old, Phase::AwaitingSuccessor),
        (Who::New, Phase::SuccessorActive),
        (Who::New, Phase::Verifying),
        (Who::New, Phase::Verified),
        (Who::New, Phase::Done),
    ] {
        let at = format!("every actor removed after {who:?} {phase:?}");
        let lab = Lab::new();
        at_phase(&lab, who, phase, |lab| {
            lab.remove_every_actor();
            true
        });
        hand_over(&lab);
        lab.wait_until(&at, |lab| {
            lab.actors().is_empty() && lab.procs.lock().unwrap().is_empty()
        });
        let verified = lab.seed_file().recovery_actor_image.reference();
        assert!(
            matches!(lab.seed_step(), seed::Outcome::Created { .. }),
            "{at}"
        );
        let result = lab.outcome(&at);
        lab.assert_one_actor(&verified, &at);
        if verified == NEW_ACTOR {
            assert_eq!(result.state, State::Succeeded, "{at}: {result:?}");
        } else {
            assert_eq!(result.state, State::Failed, "{at}: {result:?}");
        }
    }
}

#[test]
fn a_successor_whose_old_actor_vanished_takes_the_machine_over() {
    let lab = Lab::new();
    lab.handover.lock().unwrap().orphan_check = Duration::from_millis(5);
    at_phase(&lab, Who::Old, Phase::AwaitingSuccessor, |lab| {
        let old = lab
            .engine
            .state()
            .container_named(names::RECOVERY_ACTOR)
            .unwrap()
            .id
            .clone();
        lab.engine.remove_container(&old).unwrap();
        true
    });
    hand_over(&lab);
    let result = lab.outcome("orphan");
    assert_succeeded(&lab, &result, "orphan");
}

#[test]
fn an_attempt_moves_the_actor_first_then_the_agent() {
    let lab = Lab::new();
    lab.submit(request(vec![actor_component(), agent_component()]))
        .unwrap();
    let result = lab.outcome("actor then agent");
    assert_succeeded(&lab, &result, "actor then agent");
    let agent = lab
        .engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone();
    assert_eq!(agent.spec.image, NEW_AGENT);
    assert_eq!(result.previous.len(), 2);
}

#[test]
fn a_failed_agent_after_the_actor_moved_restores_only_the_agent() {
    let lab = Lab::new();
    let agent = lab
        .engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone();
    lab.engine.with_state(|s| {
        s.behaviour.insert(
            NEW_AGENT.into(),
            Behaviour {
                health: Some("unhealthy".into()),
                ..Default::default()
            },
        );
    });
    lab.submit(request(vec![actor_component(), agent_component()]))
        .unwrap();
    let result = lab.outcome("agent restored");
    assert_eq!(result.state, State::Failed, "{result:?}");
    assert_eq!(result.reason, Some(Reason::Unhealthy));
    assert!(result.restored);
    // The actor moved first and stays on the new release; the agent was put back.
    lab.assert_one_actor(NEW_ACTOR, "agent restored");
    let now = lab
        .engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone();
    assert_eq!(now.id, agent.id);
    assert!(
        result.output.contains("recovery-actor")
            && result.output.contains("stays on the new image"),
        "{}",
        result.output
    );
}

#[test]
fn an_attempt_interrupted_at_the_agent_says_the_actor_already_moved() {
    let lab = Lab::new();
    let agent = lab
        .engine
        .state()
        .container_named(names::NODE_AGENT)
        .unwrap()
        .clone();
    let mut fired = false;
    lab.on(move |_, who, component, phase| {
        let hit = !fired && who == Who::New && component == "node-agent" && phase == Phase::Pulling;
        fired |= hit;
        hit
    });
    lab.submit(request(vec![actor_component(), agent_component()]))
        .unwrap();
    let result = lab.outcome("agent interrupted");
    assert_eq!(result.state, State::Failed, "{result:?}");
    assert_eq!(result.reason, Some(Reason::Interrupted));
    assert!(!result.restored);
    lab.assert_one_actor(NEW_ACTOR, "agent interrupted");
    assert_eq!(
        lab.engine
            .state()
            .container_named(names::NODE_AGENT)
            .unwrap()
            .id,
        agent.id
    );
    assert!(
        !result.output.contains("nothing was changed")
            && result.output.contains("recovery-actor")
            && result.output.contains("stays on the new image"),
        "{}",
        result.output
    );
}

#[test]
fn only_a_gpu_hosts_agent_socket_may_move_the_actor() {
    let lab = Lab::new();
    let path = lab.dir.path().join("machine.json");
    let mut machine: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    machine["role"] = "combined".into();
    std::fs::write(&path, serde_json::to_vec(&machine).unwrap()).unwrap();
    let refused = lab.submit(request(vec![actor_component()])).unwrap_err();
    assert_eq!(refused.reason, Reason::Invalid, "{}", refused.message);
    assert!(
        refused.message.contains("control-plane step"),
        "{}",
        refused.message
    );
    assert_eq!(lab.actors().len(), 1);
}

/// The machine states a successful hand-over leaves, one per committed phase, as seed
/// contract fixtures in the unreleased set (`testdata/recovery/seed/actors/`
/// [`UNRELEASED_SET`], read by `tests/seed_contract.rs` with every released actor's): a seed
/// of any age must see a recovery actor in each and do nothing. Regenerate with
/// `QUASAR_WRITE_SEED_FIXTURES=1`; a release copies the set to its version and freezes it.
#[test]
fn the_hand_over_states_this_actor_writes_are_seed_fixtures() {
    type Snapshot = (String, Vec<serde_json::Value>, Vec<u8>);
    let lab = Lab::new();
    let snapshots = Arc::new(Mutex::new(Vec::<Snapshot>::new()));
    let taken = snapshots.clone();
    let dir = lab.dir.path().to_path_buf();
    lab.on(move |lab, who, component, phase| {
        if component != "recovery-actor" {
            return false;
        }
        let who = match who {
            Who::Old => "old",
            Who::New => "successor",
        };
        let phase = serde_json::to_value(phase).unwrap();
        let case = format!("hand-over-{who}-committed-{}", phase.as_str().unwrap());
        let mut containers: Vec<FakeContainer> =
            lab.engine.state().containers.values().cloned().collect();
        containers.sort_by(|a, b| a.spec.name.cmp(&b.spec.name));
        let recorded = containers
            .iter()
            .map(|c| {
                let v = view(c);
                let labels: BTreeMap<_, _> = v
                    .labels
                    .iter()
                    .filter(|(k, _)| k.starts_with("io.quasar."))
                    .collect();
                let mut r = serde_json::json!({
                    "name": v.name, "labels": labels, "status": v.status, "command": v.command,
                });
                if seed::is_seed(&v) {
                    r["id"] = v.id.clone().into();
                }
                if is_actor(c) {
                    let mut env = v.env.clone();
                    env.sort();
                    r["env"] = env.into();
                }
                r
            })
            .collect();
        let seed_json = std::fs::read(dir.join(file::FILE_NAME)).unwrap();
        taken.lock().unwrap().push((case, recorded, seed_json));
        false
    });
    hand_over(&lab);
    assert_eq!(lab.outcome("fixtures").state, State::Succeeded);

    let set = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/recovery/seed/actors")
        .join(UNRELEASED_SET);
    let write = std::env::var_os("QUASAR_WRITE_SEED_FIXTURES").is_some();
    let snapshots = snapshots.lock().unwrap().clone();
    assert_eq!(snapshots.len(), OLD_PHASES.len() + NEW_PHASES.len());
    for (case, recorded, seed_json) in snapshots {
        let at = set.join(&case);
        let containers = serde_json::to_string_pretty(&recorded).unwrap() + "\n";
        let expected = "{\n  \"decision\": \"actor_present\"\n}\n";
        if write {
            std::fs::create_dir_all(&at).unwrap();
            std::fs::write(at.join("containers.json"), &containers).unwrap();
            std::fs::write(at.join("seed.json"), &seed_json).unwrap();
            std::fs::write(at.join("expected.json"), expected).unwrap();
            continue;
        }
        let read = |name: &str| {
            std::fs::read(at.join(name)).unwrap_or_else(|e| {
                panic!("{case}/{name}: {e} (QUASAR_WRITE_SEED_FIXTURES=1 writes a new set)")
            })
        };
        assert_eq!(
            String::from_utf8(read("containers.json")).unwrap(),
            containers,
            "{case}"
        );
        assert_eq!(read("seed.json"), seed_json, "{case}");
        assert_eq!(
            String::from_utf8(read("expected.json")).unwrap(),
            expected,
            "{case}"
        );
    }
}

/// Architecture §5.6: on a GPU host the successor verifies only once the node agent
/// reaches it. An agent that never does leaves it unverified, and it hands back.
#[test]
fn a_successor_the_agent_never_reaches_hands_the_machine_back() {
    let lab = Lab::new();
    let old = lab.old_actor();
    lab.agent_polls.store(false, Ordering::SeqCst);
    hand_over(&lab);
    let result = lab.outcome("no agent contact");
    assert_restored(&lab, &result, &old, "no agent contact");
    assert_eq!(result.reason, Some(Reason::Unhealthy), "{result:?}");
    assert!(
        result
            .output
            .contains("no node agent reached its agent socket"),
        "{}",
        result.output
    );
}

/// Crash-table row 3: the old actor released the lease, the successor died before taking
/// it and does not come back; the old actor's watchdog takes the machine back.
#[test]
fn a_successor_that_dies_before_taking_the_lease_leaves_the_old_actor_restored() {
    let lab = Lab::new();
    lab.handover.lock().unwrap().takeover = Duration::from_millis(300);
    let old = lab.old_actor();
    at_phase(&lab, Who::Old, Phase::HandingOver, |lab| {
        let next = lab.engine.state().container_named(NEXT).unwrap().id.clone();
        lab.break_container(&next);
        false
    });
    hand_over(&lab);
    let result = lab.outcome("row 3");
    assert_restored(&lab, &result, &old, "row 3");
    assert!(
        result.output.contains("did not take the machine's lease"),
        "{}",
        result.output
    );
    assert!(lab.engine.state().container_named(NEXT).is_none());
}

/// A daemon restart while the successor verifies on a GPU host: the node agent comes back
/// before the successor serves and finds its socket dark, keeps asking, and its contact
/// verifies the successor.
#[test]
fn a_daemon_restart_while_the_successor_verifies_still_verifies_it() {
    let lab = Lab::new();
    at_phase(&lab, Who::New, Phase::Verifying, |lab| {
        lab.restart_daemon();
        false
    });
    hand_over(&lab);
    let result = lab.outcome("daemon restart at verifying");
    assert_succeeded(&lab, &result, "daemon restart at verifying");
    assert!(
        lab.attach_misses.load(Ordering::SeqCst) > 0,
        "the agent attached before the successor served"
    );
}

/// An actor started by hand without the installation's labels, started again by the
/// printed fix after its successor died under the actor's name: it takes the name back
/// before it removes the successor, so the seed never sees a machine without an actor.
#[test]
fn a_hand_started_actor_takes_its_name_back_before_its_successor_goes() {
    let lab = Lab::new();
    let old = lab.old_actor();
    lab.engine.with_state(|s| {
        let c = s.containers.get_mut(&old.id).unwrap();
        c.spec.labels.remove("io.quasar.installation");
        c.spec.labels.remove("io.quasar.platform-service");
    });
    let old_id = old.id.clone();
    let mut fired = false;
    lab.on(move |lab, who, component, phase| {
        if fired || who != Who::New || component != "recovery-actor" || phase != Phase::Verifying {
            return false;
        }
        fired = true;
        let successor = lab
            .engine
            .state()
            .container_named(names::RECOVERY_ACTOR)
            .unwrap()
            .id
            .clone();
        lab.break_container(&successor);
        let (lab, old_id) = (lab.me.clone(), old_id.clone());
        std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(50));
            if let Some(lab) = lab.upgrade() {
                lab.engine.start_container(&old_id).unwrap();
            }
        });
        false
    });
    hand_over(&lab);
    lab.wait_until("the hand-started actor is back", |lab| {
        let state = lab.engine.state();
        state.container_named(NEXT).is_none()
            && state
                .container_named(names::RECOVERY_ACTOR)
                .is_some_and(|c| c.id == old.id && c.status == "running")
            && lab.serving().is_some_and(|a| {
                a.status_for(Some(ID))
                    .result
                    .is_some_and(|r| r.state.is_terminal())
            })
    });
    let result = lab.serving().unwrap().status_for(Some(ID)).result.unwrap();
    assert_eq!(
        (result.state, result.restored),
        (State::Failed, true),
        "{result:?}"
    );
    let races = lab.races.lock().unwrap().clone();
    assert!(races.is_empty(), "the seed would have acted: {races:#?}");
}

/// A duplicate actor container on the machine, started before the hand-over or while the
/// old actor lets go of the lease, never takes the lease: it exits at once, and the
/// hand-over ends with one actor as if it had never run.
#[test]
fn a_stray_actor_never_takes_the_machine() {
    const STRAY: &str = "quasar-recovery-stray";
    for at_release in [false, true] {
        let at = format!("stray started at the release: {at_release}");
        let lab = Lab::new();
        let mut spec = lab.old_actor().spec;
        spec.name = STRAY.into();
        let start_stray = move |lab: &Lab| {
            let id = lab.engine.create_container(&spec).unwrap();
            lab.engine
                .set_restart_policy(&id, RestartPolicy::No)
                .unwrap();
            lab.engine.start_container(&id).unwrap();
        };
        if at_release {
            let once = Arc::new(AtomicBool::new(false));
            let lab_ref = lab.me.clone();
            lab.on(move |_, who, component, phase| {
                if who == Who::Old
                    && component == "recovery-actor"
                    && phase == Phase::HandingOver
                    && !once.swap(true, Ordering::SeqCst)
                {
                    let (lab, start) = (lab_ref.clone(), start_stray.clone());
                    std::thread::spawn(move || {
                        let Some(lab) = lab.upgrade() else { return };
                        // Start it the moment nobody holds the lease.
                        let deadline = Instant::now() + Duration::from_secs(5);
                        while Instant::now() < deadline
                            && lab
                                .procs
                                .lock()
                                .unwrap()
                                .values()
                                .any(|p| p.actor.holds_lease())
                        {
                            std::hint::spin_loop();
                        }
                        start(&lab);
                    });
                }
                false
            });
        } else {
            start_stray(&lab);
        }
        hand_over(&lab);
        lab.wait_until(&at, |lab| {
            lab.engine
                .state()
                .container_named(STRAY)
                .is_some_and(|c| c.status == "exited")
        });
        let stray = lab
            .engine
            .state()
            .container_named(STRAY)
            .unwrap()
            .id
            .clone();
        assert!(
            !lab.holders.lock().unwrap().contains(&stray),
            "{at}: the stray actor took the lease"
        );
        lab.engine.remove_container(&stray).unwrap();
        let result = lab.outcome(&at);
        assert_succeeded(&lab, &result, &at);
    }
}

/// ADR 0007: an actor a manager declares is refused a hand-over, changing nothing; a
/// hand-started actor without labels may hand over (the self-exception).
#[test]
fn a_manager_declared_actor_is_refused_a_hand_over() {
    let lab = Lab::new();
    let old = lab.old_actor();
    lab.engine.with_state(|s| {
        let c = s.containers.get_mut(&old.id).unwrap();
        c.spec
            .labels
            .insert("com.docker.compose.project".into(), "quasar".into());
    });
    let refused = lab.submit(request(vec![actor_component()])).unwrap_err();
    assert_eq!(refused.reason, Reason::OwnerConflict, "{}", refused.message);
    assert!(
        refused.message.contains("external manager"),
        "{}",
        refused.message
    );
    assert_eq!(lab.actors().len(), 1);
    assert!(lab.engine.state().container_named(NEXT).is_none());
}
