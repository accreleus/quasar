//! The recovery actor (architecture §5.2): `resume`, `status` and `submit`. Submission,
//! the attempt journal, the settle table and the node agent's replacement live in
//! [`crate::submit`], [`crate::journal`], [`crate::settle`] and [`crate::replace`].
//!
//! `resume` runs once per start. It takes the machine's lease, creates machine state on a
//! clean machine, and makes sure every service this machine's role requires exists and
//! runs; a second run on a finished machine changes nothing. It never replaces a
//! container: one whose specification differs from what this actor would render is
//! reported and left alone. An install interrupted anywhere is completed by the next
//! `resume`, because every step is decided by observing the engine and machine state.
//!
//! An unreachable engine fails `resume`; the actor keeps serving `status` (marked stale)
//! and tries again only on its next start.

use std::collections::{BTreeMap, BTreeSet};
use std::io;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use quasar_runtime::{LeaseError, StateLease};
use tracing::{info, warn};

use crate::bootstrap::Bootstrap;
use crate::engine::{Container, ContainerSpec, EngineError, PlatformEngine, RestartPolicy};
use crate::journal::{JournalDir, Phase};
use crate::machine::{Machine, MachineDir, ServiceRecord, FORMAT};
use crate::probe;
use crate::recipe::{
    self, labels, names, paths, secrets, Bind, DatabaseInputs, ImageRef, Inputs, RenderError, Role,
    SecretMounts, TrustInputs,
};
use crate::seed;
use crate::socket::{
    ActorIdentity, AttemptResult, Conflict, DatabaseMode, MachineRole, Request, SeedIdentity,
    Service, Status,
};
use crate::trust::{SignatureEvidence, SignaturePolicy};

/// One socket the actor serves (see [`Actor::socket_plan`]).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SocketPlan {
    pub path: PathBuf,
    pub caller: crate::trust::Caller,
    /// `(uid, gid)` the socket is owned by, when its container does not run as root.
    pub owner: Option<(u32, u32)>,
}

/// What the operator gave this start. Read only on a clean machine: once machine state
/// exists it wins and these are ignored (`CONTEXT.md` "Machine inputs"). The variables are
/// `crate::bootstrap`'s. Its `Debug` never shows the enrollment string or the password.
#[derive(Clone, Default)]
pub struct OperatorInputs {
    pub enrollment: Option<String>,
    pub home_root: Option<String>,
    pub template_root: Option<String>,
    pub node_name: Option<String>,
    pub agent_image: Option<String>,
    pub control_plane_image: Option<String>,
    pub postgres_image: Option<String>,
    pub public_host: Option<String>,
    pub tls_hosts: Option<String>,
    pub trusted_proxies: Option<String>,
    pub http_port: Option<String>,
    pub tls_port: Option<String>,
    pub database_host: Option<String>,
    pub database_port: Option<String>,
    pub database_user: Option<String>,
    pub database_name: Option<String>,
    pub database_sslmode: Option<String>,
    pub database_password: Option<String>,
    pub app_puid: Option<String>,
    pub app_pgid: Option<String>,
    pub container_network: Option<String>,
    pub trust: TrustInputs,
}

impl std::fmt::Debug for OperatorInputs {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let set = |v: &Option<String>| v.as_ref().map(|_| "<set>");
        f.debug_struct("OperatorInputs")
            .field("enrollment", &set(&self.enrollment))
            .field("home_root", &self.home_root)
            .field("template_root", &self.template_root)
            .field("node_name", &self.node_name)
            .field("agent_image", &self.agent_image)
            .field("control_plane_image", &self.control_plane_image)
            .field("postgres_image", &self.postgres_image)
            .field("public_host", &self.public_host)
            .field("database_host", &self.database_host)
            .field("database_password", &set(&self.database_password))
            .field("trust", &self.trust)
            .finish_non_exhaustive()
    }
}

pub struct ActorConfig {
    pub machine_dir: PathBuf,
    pub role: MachineRole,
    pub operator: OperatorInputs,
    /// This process's own container, when it runs in one.
    pub self_container: Option<String>,
    /// The seed container that created this actor (`QUASAR_SEED_CONTAINER`): where a first
    /// install reads its inputs, and the first place the reported seed is looked for.
    pub seed_container: Option<String>,
    /// The engine socket's daemon-host path when self-inspection cannot tell.
    pub docker_socket_fallback: String,
    pub new_installation_id: Box<dyn Fn() -> String + Send + Sync>,
    /// RFC 3339 UTC, for the timestamps machine state records.
    pub now: Box<dyn Fn() -> String + Send + Sync>,
    /// Between attempts of the `--gpus` probe (multiplied by the attempt number).
    pub gpus_probe_backoff: std::time::Duration,
    /// The release trust configuration `submit` admits requests under.
    pub trust: TrustConfig,
    /// Gathers ADR 0003 signature evidence for a request; called only when signing is on.
    pub evidence: Box<dyn Fn(&Request) -> SignatureEvidence + Send + Sync>,
    pub timing: ReplaceTiming,
    /// How long an install waits for Postgres, then the control plane, to report healthy
    /// before it creates what depends on it (and creates it anyway, logged).
    pub healthy_wait: std::time::Duration,
    /// Between accepting a host removal and removing the agent that relayed it
    /// ([`crate::remove`]), so its ack reaches the control plane first.
    pub remove_grace: std::time::Duration,
    pub handover: HandoverTiming,
    /// The directory [`Actor::socket_plan`]'s sockets are served under. Fixed in the
    /// binary: the recipes name the same paths.
    pub socket_dir: PathBuf,
    /// Called when this process can no longer drive its attempt (a journal it cannot
    /// write, an injected crash): the binary exits, so the restart policy starts it again
    /// and `resume` settles the attempt (D8).
    pub on_died: Box<dyn Fn() + Send + Sync>,
    /// Fault injection: called after every committed phase with the component's name and
    /// the phase; `true` makes the process die right there.
    #[cfg(any(test, feature = "test-support"))]
    pub crash_after: Option<CrashAfter>,
}

/// See [`ActorConfig::crash_after`].
#[cfg(any(test, feature = "test-support"))]
pub type CrashAfter = Box<dyn Fn(&str, Phase) -> bool + Send + Sync>;

/// A hand-over's clocks (architecture §5.6). Fields, not constants, so a test can compress
/// them.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct HandoverTiming {
    /// How long a started successor has to self-check and write its ready marker.
    pub ready: std::time::Duration,
    /// How long the old actor waits, once it released the lease, for the successor to
    /// take it before it takes it back.
    pub takeover: std::time::Duration,
    /// How long a successor has to answer on its own sockets once it holds the lease.
    pub verify: std::time::Duration,
    /// On a GPU host, how long a successor then waits for the node agent to reach its
    /// agent socket (the relay polls status throughout an attempt).
    pub agent_contact: std::time::Duration,
    /// Between two looks at the lease, the journal or a ready marker.
    pub poll: std::time::Duration,
    /// How often a waiting successor checks that the old actor's container still exists.
    pub orphan_check: std::time::Duration,
}

impl Default for HandoverTiming {
    fn default() -> Self {
        HandoverTiming {
            ready: std::time::Duration::from_secs(60),
            takeover: std::time::Duration::from_secs(60),
            verify: std::time::Duration::from_secs(60),
            agent_contact: std::time::Duration::from_secs(60),
            poll: std::time::Duration::from_millis(500),
            orphan_check: std::time::Duration::from_secs(5),
        }
    }
}

/// This machine's trust knobs: `QUASAR_UPDATER_ALLOWED_NAMESPACES`,
/// `QUASAR_UPDATER_SIGNATURE_MODE` and `QUASAR_UPDATER_TRUSTED_KEYS`, as the updater reads
/// them (`docs/configuration.md` "Recovery actor").
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TrustConfig {
    pub allowed_namespaces: Vec<String>,
    pub signature: SignaturePolicy,
}

impl Default for TrustConfig {
    fn default() -> Self {
        TrustConfig {
            allowed_namespaces: crate::trust::parse_allowed_namespaces(""),
            signature: SignaturePolicy::default(),
        }
    }
}

/// A replacement's clocks. Fields, not constants, so a test can compress them.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ReplaceTiming {
    /// How long a new container has to run and pass its health check, unless the request
    /// names its own `wait_timeout_s` (the updater's default, 300 s).
    pub verify_timeout: std::time::Duration,
    /// Between two looks at a container being verified or restored.
    pub poll: std::time::Duration,
    /// A container stopped by a replacement gets this long before it is killed.
    pub stop_grace: std::time::Duration,
    /// An engine call that fails transiently is asked again this many times.
    pub retries: u32,
    pub retry_backoff: std::time::Duration,
}

impl Default for ReplaceTiming {
    fn default() -> Self {
        ReplaceTiming {
            verify_timeout: std::time::Duration::from_secs(300),
            poll: std::time::Duration::from_secs(2),
            stop_grace: std::time::Duration::from_secs(30),
            retries: 5,
            retry_backoff: std::time::Duration::from_secs(2),
        }
    }
}

impl ActorConfig {
    pub fn new(
        machine_dir: impl Into<PathBuf>,
        role: MachineRole,
        operator: OperatorInputs,
    ) -> Self {
        ActorConfig {
            machine_dir: machine_dir.into(),
            role,
            operator,
            self_container: None,
            seed_container: None,
            docker_socket_fallback: paths::ENGINE_SOCKET.into(),
            new_installation_id: Box::new(random_uuid),
            now: Box::new(rfc3339_now),
            gpus_probe_backoff: std::time::Duration::from_secs(2),
            trust: TrustConfig::default(),
            evidence: Box::new(|_| SignatureEvidence::FetchError {
                error: "this recovery actor has no release-asset fetcher configured".into(),
            }),
            timing: ReplaceTiming::default(),
            healthy_wait: std::time::Duration::from_secs(180),
            remove_grace: crate::remove::DEFAULT_GRACE,
            handover: HandoverTiming::default(),
            socket_dir: paths::AGENT_SOCKET_DIR.into(),
            on_died: Box::new(|| {}),
            #[cfg(any(test, feature = "test-support"))]
            crash_after: None,
        }
    }
}

#[derive(Debug)]
pub enum ResumeError {
    /// Another recovery actor holds this machine's lease; this one must not act.
    LeaseHeld,
    /// The machine-state volume is missing or unreadable.
    State(io::Error),
    Engine(EngineError),
    /// An operator input is missing or invalid on a clean machine.
    Inputs(String),
    /// A container or volume this installation would own already exists without its
    /// labels; nothing was done to it.
    OwnerConflict(String),
    /// The image declares a recipe revision this actor does not carry.
    RecipeUnsupported(String),
    /// Something this build does not do yet.
    Unsupported(String),
    /// This process is gone (a test's stand-in for the process dying).
    Stopped,
    /// This process is no party to a hand-over and the machine has another actor.
    Stray(String),
}

impl std::fmt::Display for ResumeError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ResumeError::LeaseHeld => f.write_str(
                "another recovery actor holds this machine's lease (actor.lease); this one will not act",
            ),
            ResumeError::State(e) => write!(f, "machine state: {e}"),
            ResumeError::Engine(e) => write!(f, "container engine: {e}"),
            ResumeError::Inputs(why) => write!(f, "install inputs: {why}"),
            ResumeError::OwnerConflict(why) => write!(f, "owner_conflict: {why}"),
            ResumeError::RecipeUnsupported(why) => write!(f, "recipe_unsupported: {why}"),
            ResumeError::Unsupported(why) => write!(f, "not supported by this build: {why}"),
            ResumeError::Stopped => f.write_str("this process was stopped"),
            ResumeError::Stray(why) => {
                write!(f, "this recovery actor is not this machine's ({why}); it will not act")
            }
        }
    }
}

impl std::error::Error for ResumeError {}

impl From<EngineError> for ResumeError {
    fn from(e: EngineError) -> Self {
        ResumeError::Engine(e)
    }
}

impl From<io::Error> for ResumeError {
    fn from(e: io::Error) -> Self {
        ResumeError::State(e)
    }
}

impl From<RenderError> for ResumeError {
    fn from(e: RenderError) -> Self {
        match e {
            RenderError::Unsupported { .. } => ResumeError::RecipeUnsupported(e.to_string()),
            RenderError::Invalid(why) => ResumeError::Inputs(why),
        }
    }
}

#[derive(Debug, Clone, Default)]
struct Inventory {
    services: Vec<Service>,
    conflicts: Vec<Conflict>,
    seed: Option<SeedIdentity>,
}

pub struct Actor {
    pub(crate) engine: Arc<dyn PlatformEngine>,
    status_engine: Arc<dyn PlatformEngine>,
    pub(crate) config: ActorConfig,
    pub(crate) dir: MachineDir,
    pub(crate) journals: JournalDir,
    lease: Mutex<Option<StateLease>>,
    last: Mutex<Option<Inventory>>,
    identity: Mutex<Option<ActorIdentity>>,
    /// Serialises admission, so two submits cannot both find no open attempt.
    pub(crate) gate: Mutex<()>,
    /// The thread driving the attempt `submit` admitted, until it has finished.
    pub(crate) worker: Mutex<Option<std::thread::JoinHandle<()>>>,
    /// Set by `acquire_lease` and `resume`, cleared when `resume` returns: a submit then
    /// is refused `busy`, so one queued on a socket served before `resume` cannot race it.
    pub(crate) resuming: std::sync::atomic::AtomicBool,
    /// A console removal is being driven in this process; a retry does not start another.
    pub(crate) removing: Arc<std::sync::atomic::AtomicBool>,
    /// Seed identities by image id: an image's labels never change.
    seed_images: Mutex<BTreeMap<String, SeedIdentity>>,
    /// The agent socket's serving loop, while this process serves it.
    server: Mutex<Vec<ServerHandle>>,
    /// Itself, for the serving loop, once `serve` was called on the `Arc`.
    me: std::sync::OnceLock<std::sync::Weak<Actor>>,
    /// This process handed the machine to another actor and must exit.
    retired: std::sync::atomic::AtomicBool,
    /// This process is gone: a test stands it in for the process dying.
    killed: std::sync::atomic::AtomicBool,
    /// Requests another process made on this actor's agent socket.
    external_requests: std::sync::atomic::AtomicU64,
}

struct ServerHandle {
    path: PathBuf,
    stop: Arc<std::sync::atomic::AtomicBool>,
    thread: std::thread::JoinHandle<io::Result<()>>,
}

const SECRETS_HELPER: &str = "secrets-writer";

impl Actor {
    pub fn new(engine: Arc<dyn PlatformEngine>, config: ActorConfig) -> Self {
        Actor {
            status_engine: engine.clone(),
            engine,
            dir: MachineDir::new(config.machine_dir.clone()),
            journals: JournalDir::new(&config.machine_dir),
            config,
            lease: Mutex::new(None),
            last: Mutex::new(None),
            identity: Mutex::new(None),
            gate: Mutex::new(()),
            worker: Mutex::new(None),
            resuming: std::sync::atomic::AtomicBool::new(false),
            removing: Arc::new(std::sync::atomic::AtomicBool::new(false)),
            seed_images: Mutex::new(BTreeMap::new()),
            server: Mutex::new(Vec::new()),
            me: std::sync::OnceLock::new(),
            retired: std::sync::atomic::AtomicBool::new(false),
            killed: std::sync::atomic::AtomicBool::new(false),
            external_requests: std::sync::atomic::AtomicU64::new(0),
        }
    }

    pub(crate) fn note_external_request(&self) {
        self.external_requests
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    }

    pub(crate) fn external_requests(&self) -> u64 {
        self.external_requests
            .load(std::sync::atomic::Ordering::SeqCst)
    }

    /// Serve `status` through a separate engine client, typically one with a short
    /// deadline, so a slow engine yields a stale answer rather than a hung one.
    pub fn with_status_engine(mut self, engine: Arc<dyn PlatformEngine>) -> Self {
        self.status_engine = engine;
        self
    }

    /// Whether this actor holds the machine's lease, which it needs to serve its sockets.
    pub fn holds_lease(&self) -> bool {
        self.lease.lock().unwrap().is_some()
    }

    /// Take the machine's lease without doing anything else, so the binary can serve
    /// `status` while `resume` settles an open attempt. `resume` takes it too.
    /// Submits are refused `busy` from here until `resume` returns.
    pub fn acquire_lease(&self) -> Result<(), ResumeError> {
        self.resuming
            .store(true, std::sync::atomic::Ordering::SeqCst);
        self.take_lease()
    }

    /// Take the machine's lease. Only a party to an open hand-over waits for it: during a
    /// hand-over two actors run and the lease decides which one acts (architecture §5.6).
    /// While it waits, a successor does its part of the hand-over (`crate::handover`):
    /// it self-checks and says it is ready, and it takes the lease only once the old actor
    /// has handed it over, or the old actor's container is gone. Any other process is
    /// refused at once when the lease is held or the machine has another actor.
    pub fn acquire_lease_waiting(&self) -> Result<(), ResumeError> {
        use std::sync::atomic::Ordering;
        self.resuming.store(true, Ordering::SeqCst);
        let mut logged = false;
        let mut last_orphan_check = None;
        let own_attempt = self.own_attempt_label();
        loop {
            if self.killed() {
                return Err(ResumeError::Stopped);
            }
            // Anyone else acts only on a free lease, as before a hand-over existed, and never
            // beside a party to one: taking the lease in the gap of a hand-over would be a
            // stranger's restore beside two running actors.
            if !self.is_handover_party(own_attempt.as_deref()) {
                if let Some(why) = self.stray() {
                    return Err(ResumeError::Stray(why));
                }
                return self.take_lease();
            }
            if self.may_take_lease(own_attempt.as_deref(), &mut last_orphan_check) {
                match self.take_lease() {
                    Ok(()) => return Ok(()),
                    Err(ResumeError::LeaseHeld) => {}
                    Err(e) => return Err(e),
                }
            }
            if !logged {
                info!(
                    token = "actor-lease-waiting",
                    "another recovery actor holds this machine's lease; waiting for it"
                );
                logged = true;
            }
            self.while_waiting(own_attempt.as_deref());
            std::thread::sleep(self.config.handover.poll);
        }
    }

    /// Serve every socket of [`Actor::socket_plan`], each on a thread of its own. Only the
    /// lease holder may: the lease is what makes a leftover socket file certainly stale.
    /// A socket already served is left alone, so the binary calls it again once a first
    /// install has learnt its role. Returns the sockets that could not be bound.
    pub fn serve(self: &Arc<Self>) -> Vec<(PathBuf, io::Error)> {
        let _ = self.me.set(Arc::downgrade(self));
        self.serve_again()
    }

    /// [`Actor::serve`] from `&self`, once `serve` has been called on the `Arc`.
    pub(crate) fn serve_again(&self) -> Vec<(PathBuf, io::Error)> {
        let mut servers = self.server.lock().unwrap();
        let plan = match self.socket_plan() {
            Ok(plan) => plan,
            Err(e) => {
                let why = format!("this machine's sockets are unknown: {e}");
                return vec![(self.config.socket_dir.clone(), io::Error::other(why))];
            }
        };
        let actor = match self.me.get().and_then(std::sync::Weak::upgrade) {
            Some(actor) if !self.killed() => actor,
            found => {
                let why = if found.is_some() {
                    "this process was stopped"
                } else {
                    "this actor never served its sockets"
                };
                return plan
                    .into_iter()
                    .map(|p| (p.path, io::Error::other(why)))
                    .collect();
            }
        };
        let mut failed = Vec::new();
        for p in plan {
            if servers.iter().any(|h| h.path == p.path) {
                continue;
            }
            let listener = match crate::server::bind_owned(&p.path, p.owner) {
                Ok(listener) => listener,
                Err(e) => {
                    failed.push((p.path, e));
                    continue;
                }
            };
            info!(socket = %p.path.display(), caller = ?p.caller, "serving");
            let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
            let (flag, actor, caller) = (stop.clone(), actor.clone(), p.caller);
            let thread =
                std::thread::spawn(move || crate::server::serve(listener, actor, caller, flag));
            servers.push(ServerHandle {
                path: p.path,
                stop,
                thread,
            });
        }
        failed
    }

    /// Whether any socket is being served.
    pub fn serving(&self) -> bool {
        !self.server.lock().unwrap().is_empty()
    }

    /// Stop serving, before the lease is released: the next holder binds the paths afresh.
    pub(crate) fn stop_serving(&self) {
        let handles = std::mem::take(&mut *self.server.lock().unwrap());
        for handle in &handles {
            handle.stop.store(true, std::sync::atomic::Ordering::SeqCst);
        }
        for handle in handles {
            let _ = handle.thread.join();
        }
    }

    /// A serving loop that ended on its own, with its socket and why; `None` while every
    /// socket serves, and after a deliberate stop.
    pub fn serving_failed(&self) -> Option<(PathBuf, io::Error)> {
        let mut servers = self.server.lock().unwrap();
        let at = servers.iter().position(|h| h.thread.is_finished())?;
        let handle = servers.remove(at);
        let why = match handle.thread.join() {
            Ok(Err(e)) => e,
            Ok(Ok(())) => io::Error::other("the socket stopped"),
            Err(_) => io::Error::other("the socket thread panicked"),
        };
        Some((handle.path, why))
    }

    pub(crate) fn release_lease(&self) {
        *self.lease.lock().unwrap() = None;
    }

    /// This process handed the machine to another recovery actor: the binary exits, and
    /// its disabled restart policy keeps it down.
    pub fn retired(&self) -> bool {
        self.retired.load(std::sync::atomic::Ordering::SeqCst)
    }

    pub(crate) fn retire(&self) {
        self.stop_serving();
        self.release_lease();
        self.retired
            .store(true, std::sync::atomic::Ordering::SeqCst);
    }

    /// A test's stand-in for this process dying: it stops serving, drops the lease and
    /// acts on nothing more.
    pub fn kill(&self) {
        self.killed.store(true, std::sync::atomic::Ordering::SeqCst);
        self.stop_serving();
        self.release_lease();
    }

    pub(crate) fn killed(&self) -> bool {
        self.killed.load(std::sync::atomic::Ordering::SeqCst)
    }

    pub(crate) fn died(&self) {
        if !self.killed() {
            (self.config.on_died)();
        }
    }

    pub fn resume(&self) -> Result<(), ResumeError> {
        use std::sync::atomic::Ordering;
        if self.killed() {
            return Err(ResumeError::Stopped);
        }
        self.resuming.store(true, Ordering::SeqCst);
        let result = self.resume_inner();
        self.resuming.store(false, Ordering::SeqCst);
        result
    }

    fn resume_inner(&self) -> Result<(), ResumeError> {
        self.take_lease()?;
        // An uninstall or a console removal has started here: nothing is installed,
        // settled or re-created, whoever started this actor (ADR 0007: the seed idles too).
        if let Some(why) = crate::uninstall::uninstalled(&self.dir) {
            // A console removal a previous process did not finish is finished here; every
            // step is remove-if-present. An operator's uninstall is left to the operator.
            if let Ok(Some(marker)) = self.dir.load_uninstall() {
                if marker.by == crate::uninstall::By::Console && marker.finished_at.is_none() {
                    info!(
                        token = "actor-removal-resumed",
                        "a console removal did not finish; finishing it"
                    );
                    self.remove_services();
                    return Ok(());
                }
            }
            warn!(
                token = "actor-machine-uninstalled",
                "{why}; this recovery actor installs and replaces nothing. `quasar-recovery uninstall` finishes the removal"
            );
            return Ok(());
        }
        // D8: an attempt a restart left open reaches its outcome before anything else
        // looks at the machine's services, and no new attempt is started here. First, so
        // nothing else a start does can leave a hand-over waiting on this process.
        self.settle_open()?;
        if self.retired() {
            return Ok(());
        }
        // A reconfigure keeps its new inputs only if its attempt succeeded.
        self.settle_reconfigure_on_start();
        self.sweep_helpers()?;
        let machine = match self.dir.load_machine()? {
            Some(machine) => {
                self.note_ignored_inputs(&machine);
                self.note_label_mismatch(&machine);
                machine
            }
            None => self.first_install()?,
        };
        self.ensure_seed_file(&machine)?;
        match machine.role {
            MachineRole::Gpu => self.ensure_node_agent(&machine),
            MachineRole::Combined | MachineRole::ControlOnly => {
                self.ensure_control_machine(&machine)
            }
        }
    }

    /// The machine inventory. Never fails: when the engine does not answer, the last
    /// inventory is returned with `stale: true`. `result` is the most recent attempt's.
    pub fn status(&self) -> Status {
        self.status_for(None)
    }

    /// [`Actor::status`], with `result` the named attempt's (`null` when this machine has
    /// no journal for it), or the most recent attempt's when `request_id` is `None`.
    pub fn status_for(&self, request_id: Option<&str>) -> Status {
        let mut status = self.inventory_status();
        // An unreadable journal is reported as in flight: nothing is admitted past it.
        status.in_flight = self.journals.scan().open_id();
        status.result = self.attempt_result(request_id);
        status
    }

    fn attempt_result(&self, request_id: Option<&str>) -> Option<AttemptResult> {
        match request_id {
            Some(id) if crate::submit::is_uuid(id) => {
                self.journals.load(id).ok().flatten().map(|j| j.result)
            }
            Some(_) => None,
            None => self.journals.latest().map(|j| j.result),
        }
    }

    fn inventory_status(&self) -> Status {
        let (inventory, stale) = match self.inventory() {
            Ok(inventory) => {
                *self.last.lock().unwrap() = Some(inventory.clone());
                (inventory, false)
            }
            Err(_) => (self.last.lock().unwrap().clone().unwrap_or_default(), true),
        };
        let machine = self.dir.load_machine().ok().flatten();
        let role = machine.as_ref().map_or(self.config.role, |m| m.role);
        let database = match machine.as_ref().and_then(|m| m.inputs.control.as_ref()) {
            Some(c) if matches!(c.database, DatabaseInputs::External { .. }) => {
                DatabaseMode::External
            }
            Some(_) => DatabaseMode::Owned,
            None if role == MachineRole::Gpu => DatabaseMode::None,
            None => DatabaseMode::Owned,
        };
        Status {
            actor: self.identity(),
            seed: inventory.seed,
            role,
            node_name: machine.map(|m| m.inputs.node_name),
            database,
            services: inventory.services,
            conflicts: inventory.conflicts,
            in_flight: None,
            dumps: Vec::new(),
            result: None,
            stale,
        }
    }

    pub(crate) fn take_lease(&self) -> Result<(), ResumeError> {
        let mut lease = self.lease.lock().unwrap();
        if lease.is_some() {
            return Ok(());
        }
        if !self.dir.root().is_dir() {
            return Err(ResumeError::State(io::Error::new(
                io::ErrorKind::NotFound,
                format!(
                    "{} does not exist: mount the {} volume there",
                    self.dir.root().display(),
                    names::MACHINE_VOLUME
                ),
            )));
        }
        match self.dir.lease() {
            // A process that is gone never holds the lease, even one it had just taken.
            Ok(_) if self.killed() => Err(ResumeError::Stopped),
            Ok(held) => {
                *lease = Some(held);
                Ok(())
            }
            Err(LeaseError::Held(_)) => Err(ResumeError::LeaseHeld),
            Err(LeaseError::Open(e)) => Err(ResumeError::State(e)),
        }
    }

    fn own_container(&self) -> Result<Option<Container>, EngineError> {
        match &self.config.self_container {
            Some(id) => self.engine.inspect_container(id),
            None => Ok(None),
        }
    }

    /// Records this actor's image in `seed.json` when no actor has yet, so a seed can
    /// re-create it. An existing file is left alone: only a verified hand-over rewrites the
    /// image, and only an uninstall the state (ADR 0007).
    fn ensure_seed_file(&self, machine: &Machine) -> Result<(), ResumeError> {
        match self.dir.load_seed_file() {
            Ok(Some(_)) => return Ok(()),
            Ok(None) => {}
            Err(e) => {
                warn!(
                    token = "actor-seed-file-unreadable",
                    "seed.json is unreadable ({e}); it is left as it is, and a seed stays idle until it is fixed"
                );
                return Ok(());
            }
        }
        let Some(image) = self
            .own_container()?
            .and_then(|me| own_image(self.engine.as_ref(), &me))
        else {
            warn!(
                token = "actor-seed-file-unwritten",
                "this actor cannot tell its own image digest, so seed.json is not written and a seed cannot re-create this actor if it is deleted; run the actor by digest"
            );
            return Ok(());
        };
        let file = seed::file::SeedFile {
            format_version: seed::file::FORMAT_VERSION,
            installation_id: machine.installation_id.clone(),
            recovery_actor_image: image,
            state: seed::file::SeedState::Active,
        };
        self.dir.store_seed_file(&file)?;
        info!(image = %file.recovery_actor_image.reference(), "seed.json records this recovery actor");
        Ok(())
    }

    /// A probe or secrets writer left by a crash is removed; nothing else is touched.
    fn sweep_helpers(&self) -> Result<(), ResumeError> {
        for name in [names::GPU_PROBE, names::SECRETS_WRITER] {
            if let Some(c) = self.engine.inspect_container(name)? {
                if c.labels.contains_key(labels::HELPER) {
                    info!(container = %c.name, "removing a helper left by an interrupted start");
                    self.engine.remove_container(&c.id)?;
                } else {
                    warn!(
                        token = "actor-helper-name-taken",
                        container = %c.name,
                        "a container this actor did not create holds the name of its {name} helper; it is left untouched and this start stops"
                    );
                    return Err(ResumeError::OwnerConflict(format!(
                        "container {name} ({}) is not this actor's helper; remove it to let the install continue",
                        c.image
                    )));
                }
            }
        }
        Ok(())
    }

    /// A seed that found no `seed.json` on an installed machine labelled this actor with a
    /// new installation. Machine state wins; the seed then sees no actor of the installation
    /// `seed.json` names and says `seed-name-taken`, until this container is re-created.
    /// Only a diagnosis: an engine that does not answer here is left to the steps after it.
    fn note_label_mismatch(&self, machine: &Machine) {
        let label = self
            .own_container()
            .ok()
            .flatten()
            .and_then(|me| me.labels.get(labels::INSTALLATION).cloned());
        if let Some(label) = label.filter(|l| *l != machine.installation_id) {
            warn!(
                token = "actor-installation-label-differs",
                "this container is labelled installation {label}, machine state is installation {}; machine state wins. Remove this container (docker rm -f {}) and the seed re-creates it with the right label",
                machine.installation_id,
                names::RECOVERY_ACTOR
            );
        }
    }

    fn note_ignored_inputs(&self, machine: &Machine) {
        let op = &self.config.operator;
        if op.enrollment.is_some() {
            info!(
                installation = %machine.installation_id,
                "this machine is already installed; the enrollment string given at this start is ignored and can be removed"
            );
        }
        if let Some(home) = &op.home_root {
            if *home != machine.inputs.home_root {
                warn!(
                    token = "actor-input-ignored",
                    "QUASAR_HOME_ROOT={home} differs from the installed {}; machine inputs change only by reconfigure",
                    machine.inputs.home_root
                );
            }
        }
    }

    /// The release trust `submit` admits under: the settings machine state recorded at
    /// install (the seed's), else this start's own (`ActorConfig::trust`), for a machine
    /// installed before they were recorded.
    pub fn trust(&self) -> Result<TrustConfig, String> {
        match self.dir.load_machine() {
            Ok(Some(m)) if !m.inputs.trust.is_empty() => {
                crate::bootstrap::trust_config(&m.inputs.trust)
            }
            Ok(_) => Ok(self.config.trust.clone()),
            Err(e) => Err(format!("machine state is unreadable: {e}")),
        }
    }

    /// The sockets this machine's role serves, as `(path in this container, caller, owner)`.
    /// The role is machine state's, else a first install's inputs, else this start's own.
    /// Unreadable machine state is an error: guessing a role could serve the wrong caller.
    pub fn socket_plan(&self) -> Result<Vec<SocketPlan>, ResumeError> {
        let role = match self.dir.load_machine()? {
            Some(m) => m.role,
            None => self
                .install_inputs()
                .map(|b| b.role)
                .unwrap_or(self.config.role),
        };
        // The constants are the binary's paths; a test serves the same layout elsewhere.
        let at = |path: &str| {
            let rel = std::path::Path::new(path)
                .strip_prefix(paths::AGENT_SOCKET_DIR)
                .expect("every socket lives in the socket directory");
            self.config.socket_dir.join(rel)
        };
        let agent = |path: &str| SocketPlan {
            path: at(path),
            caller: crate::trust::Caller::Agent,
            owner: None,
        };
        let control = SocketPlan {
            path: at(paths::CONTROL_SOCKET),
            caller: crate::trust::Caller::ControlPlane,
            owner: Some((recipe::CONTROL_PLANE_UID, recipe::CONTROL_PLANE_UID)),
        };
        Ok(match role {
            MachineRole::Gpu => vec![agent(paths::AGENT_SOCKET)],
            MachineRole::Combined => vec![agent(paths::SPLIT_AGENT_SOCKET), control],
            MachineRole::ControlOnly => vec![control],
        })
    }

    /// The inputs of a first install: the seed's, read from the container that created this
    /// actor, or, for an actor started by hand, this process's own.
    fn install_inputs(&self) -> Result<Bootstrap, ResumeError> {
        let Some(seed) = &self.config.seed_container else {
            return Ok(Bootstrap {
                role: self.config.role,
                operator: self.config.operator.clone(),
            });
        };
        // A seed redeployed since it created this actor has a new id: any seed will do.
        let containers = self.engine.list_containers()?;
        let container = seed::find(&containers, Some(seed)).ok_or_else(|| {
            ResumeError::Inputs(format!(
                "no seed container is on this machine (the one that created this actor, {seed}, is gone), so a first install has no inputs; start the seed with its inputs, then restart this actor (docker restart {})",
                names::RECOVERY_ACTOR
            ))
        })?;
        Bootstrap::from_env(&container.env).map_err(ResumeError::Inputs)
    }

    fn first_install(&self) -> Result<Machine, ResumeError> {
        let boot = self.install_inputs()?;
        let host = self.engine.host()?;
        let checked = boot
            .check(host.name.as_deref())
            .map_err(ResumeError::Inputs)?;
        // A seed-created actor carries the installation id the seed chose; adopting it keeps
        // the seed's labels, machine state and seed.json naming one installation.
        let installation_id = self
            .own_container()?
            .and_then(|me| me.labels.get(labels::INSTALLATION).cloned())
            .filter(|id| !id.trim().is_empty())
            .unwrap_or_else(|| (self.config.new_installation_id)());
        let socket_dir = match &checked.control {
            Some(_) => Some(self.socket_volume_host_path()?),
            None => None,
        };
        let mut inputs = Inputs {
            unknown: Default::default(),
            installation_id,
            node_name: checked.node_name.clone(),
            home_root: checked.home_root.clone(),
            template_root: checked.template_root.clone(),
            docker_socket: self.docker_socket_host_path()?,
            gpu: Default::default(),
            devices: Default::default(),
            control: checked.control.as_ref().map(|c| c.inputs.clone()),
            socket_dir,
            trust: checked.trust.clone(),
            enroll: Default::default(),
            app: checked.app.clone(),
        };
        if checked.control.is_some() {
            // The seed a new GPU host runs is this machine's recovery image.
            inputs.enroll = recipe::EnrollImages {
                unknown: Default::default(),
                seed: self
                    .own_container()?
                    .and_then(|me| own_image(self.engine.as_ref(), &me))
                    .and_then(|i| ImageRef::parse(&i.reference()).ok()),
                agent: checked.enroll_agent_image.clone(),
            };
        }
        recipe::validate(&inputs)?;

        // Refused here, before anything durable: once machine state exists it wins over
        // corrected inputs.
        let mut install_images = BTreeMap::new();
        if let Some(image) = &checked.agent_image {
            let found = self.ensure_image(image)?;
            agent_revision_for(&found, image, checked.role)?;
            install_images.insert(Role::NodeAgent, image.clone());
        }
        if let Some(control) = &checked.control {
            let found = self.ensure_image(&control.image)?;
            supported_revision(Role::ControlPlane, &found, &control.image)?;
            install_images.insert(Role::ControlPlane, control.image.clone());
            if let Some(postgres) = &control.postgres_image {
                self.ensure_image(postgres)?;
                install_images.insert(Role::Postgres, postgres.clone());
            }
        }
        if let Some(enrollment) = &checked.enrollment {
            self.dir.store_secret(secrets::ENROLLMENT, enrollment)?;
        }
        if let Some(control) = &checked.control {
            self.store_control_secrets(checked.role, control)?;
        }
        if let Some(image) = &checked.agent_image {
            let report = match probe::run(self.engine.as_ref(), image) {
                Ok(report) => report,
                Err(probe::ProbeError::Engine(e)) => return Err(e.into()),
                Err(e @ probe::ProbeError::Unreadable(_)) => {
                    warn!(
                        token = "actor-gpu-probe-unreadable",
                        "GPU detection failed ({e}); installing the agent without GPU devices, and its readiness will report the gap"
                    );
                    probe::ProbeReport::default()
                }
            };
            let (gpu, devices) = probe::select(&report);
            match &gpu.vendor {
                Some(vendor) => info!(
                    vendor = ?vendor,
                    render_node = gpu.render_node.as_deref().unwrap_or(""),
                    "GPU detected"
                ),
                None => warn!(
                    token = "actor-no-gpu",
                    "no usable GPU render node on this machine; installing the agent anyway, and its readiness will report the gap"
                ),
            }
            inputs.gpu = gpu;
            inputs.devices = devices;
        }

        let machine = Machine {
            unknown: Default::default(),
            format: FORMAT,
            installation_id: inputs.installation_id.clone(),
            role: checked.role,
            created_at: (self.config.now)(),
            inputs,
            install_images,
        };
        self.dir.machine().store(&machine)?;
        info!(installation = %machine.installation_id, node = %machine.inputs.node_name, role = ?machine.role, "machine state created");
        Ok(machine)
    }

    /// The daemon-host path of the engine socket this process was given, learned from its
    /// own container's mounts.
    fn docker_socket_host_path(&self) -> Result<String, ResumeError> {
        if let Some(id) = &self.config.self_container {
            if let Some(me) = self.engine.inspect_container(id)? {
                if let Some((source, _, _)) = me
                    .mounts
                    .iter()
                    .find(|(_, target, _)| target == paths::ENGINE_SOCKET)
                {
                    return Ok(source.clone());
                }
            }
        }
        Ok(self.config.docker_socket_fallback.clone())
    }

    pub(crate) fn ensure_image(
        &self,
        image: &ImageRef,
    ) -> Result<crate::engine::Image, ResumeError> {
        let reference = image.reference();
        if let Some(found) = self.engine.inspect_image(&reference)? {
            return Ok(found);
        }
        info!(image = %reference, "pulling");
        self.engine.pull(&reference)?;
        self.engine
            .inspect_image(&reference)?
            .ok_or(ResumeError::Engine(EngineError::Runtime(
                crate::engine::ErrorKind::Missing,
            )))
    }

    pub(crate) fn owned_labels(&self, machine: &Machine, role: Role) -> BTreeMap<String, String> {
        BTreeMap::from([
            (
                labels::INSTALLATION.to_string(),
                machine.installation_id.clone(),
            ),
            (
                labels::PLATFORM_SERVICE.to_string(),
                role.as_str().to_string(),
            ),
        ])
    }

    pub(crate) fn ensure_volume(
        &self,
        machine: &Machine,
        name: &str,
        role: Role,
    ) -> Result<(), ResumeError> {
        match self.engine.inspect_volume(name)? {
            Some(v) => match v.labels.get(labels::INSTALLATION) {
                Some(id) if *id != machine.installation_id => Err(ResumeError::OwnerConflict(
                    format!("volume {name} belongs to installation {id}"),
                )),
                _ => Ok(()),
            },
            None => {
                self.engine
                    .create_volume(name, &self.owned_labels(machine, role))?;
                Ok(())
            }
        }
    }

    pub(crate) fn is_ours(&self, machine: &Machine, c: &Container, role: Role) -> bool {
        c.labels.get(labels::INSTALLATION) == Some(&machine.installation_id)
            && c.labels.get(labels::PLATFORM_SERVICE).map(String::as_str) == Some(role.as_str())
    }

    pub(crate) fn node_agent_secrets(&self) -> Result<SecretMounts, ResumeError> {
        let mut files = BTreeSet::new();
        // A GPU host stores only the first, a combined host only the second.
        for name in [secrets::ENROLLMENT, secrets::LOCAL_ENROLLMENT] {
            if self.dir.load_secret(name)?.is_some() {
                files.insert(name.to_string());
            }
        }
        Ok(SecretMounts {
            volume: Some(names::NODE_AGENT_SECRETS_VOLUME.into()),
            files,
        })
    }

    pub(crate) fn ensure_node_agent(&self, machine: &Machine) -> Result<(), ResumeError> {
        let mut machine = machine.clone();
        let machine = &mut machine;
        let role = Role::NodeAgent;
        self.ensure_volume(machine, names::AGENT_DATA_VOLUME, role)?;
        self.ensure_volume(machine, names::NODE_AGENT_SECRETS_VOLUME, role)?;
        self.ensure_volume(machine, names::AGENT_SOCKET_VOLUME, Role::RecoveryActor)?;
        if machine.inputs.gpu.nvidia_shape() {
            self.ensure_volume(machine, names::NVIDIA_DRIVER_VOLUME, role)?;
        }
        let secrets = self.node_agent_secrets()?;
        self.ensure_service(
            machine,
            role,
            &secrets,
            FileOwner::ROOT,
            |machine, image| {
                // A combined host's agent enrolls with the local token, which the control
                // plane inserts when it boots.
                if machine.role == MachineRole::Combined {
                    self.await_healthy(names::CONTROL_PLANE);
                }
                self.decide_gpus(machine, image)?;
                if machine.inputs.gpu.nvidia_shape() {
                    self.ensure_volume(machine, names::NVIDIA_DRIVER_VOLUME, role)?;
                }
                Ok(())
            },
        )
    }

    /// On an NVIDIA machine not yet known to serve `--gpus`, ask the engine before the agent
    /// is created. Only a yes is recorded; a definite no installs the agent without the
    /// NVIDIA shape (readiness reports the gap) and is asked again the next time the agent
    /// is created; no answer stops this start.
    pub(crate) fn decide_gpus(
        &self,
        machine: &mut Machine,
        image: &ImageRef,
    ) -> Result<(), ResumeError> {
        let gpu = &machine.inputs.gpu;
        if gpu.vendor != Some(recipe::GpuVendor::Nvidia) || gpu.gpus_served {
            return Ok(());
        }
        match probe::serves_gpus(self.engine.as_ref(), image, self.config.gpus_probe_backoff)? {
            probe::GpusAnswer::Served => {
                info!(
                    token = "actor-gpus-served",
                    "NVIDIA: the engine started a --gpus all probe; installing the NVIDIA shape"
                );
                machine.inputs.gpu.gpus_served = true;
                self.dir.machine().store(machine)?;
            }
            probe::GpusAnswer::Refused(why) => warn!(
                token = "actor-gpus-refused",
                "NVIDIA device found, but the engine does not serve --gpus: {why}; installing without the NVIDIA shape (is the NVIDIA Container Toolkit installed for this engine?)"
            ),
        }
        Ok(())
    }

    pub(crate) fn record(
        &self,
        role: Role,
        revision: u32,
        image: &ImageRef,
        spec: ContainerSpec,
    ) -> Result<(), ResumeError> {
        let record = ServiceRecord {
            role,
            recipe_revision: revision,
            image: image.clone(),
            spec_digest: spec.labels.get(labels::SPEC).cloned().unwrap_or_default(),
            spec,
            applied_at: (self.config.now)(),
        };
        Ok(self.dir.store_service(&record)?)
    }

    /// Writes the secret files into a service's secrets volume through a helper container
    /// that is created, never started, and removed, owned as the consuming container needs.
    pub(crate) fn deliver_secrets_as(
        &self,
        image: &ImageRef,
        volume: &str,
        files: &BTreeSet<String>,
        owner: FileOwner,
    ) -> Result<(), ResumeError> {
        let mut entries = Vec::new();
        for name in files {
            if let Some(value) = self.dir.load_secret(name)? {
                entries.push((name.clone(), value));
            }
        }
        let archive = tar_of(&entries, owner)?;
        let writer = ContainerSpec {
            name: names::SECRETS_WRITER.into(),
            image: image.reference(),
            entrypoint: Some(vec!["/bin/true".into()]),
            cmd: None,
            env: BTreeMap::new(),
            labels: BTreeMap::from([(labels::HELPER.to_string(), SECRETS_HELPER.to_string())]),
            network_mode: Some("none".into()),
            binds: vec![Bind {
                source: volume.into(),
                target: "/secrets".into(),
                read_only: false,
            }],
            devices: Vec::new(),
            device_cgroup_rules: Vec::new(),
            gpus: Vec::new(),
            cap_add: Vec::new(),
            security_opt: Vec::new(),
            init: false,
            restart: RestartPolicy::No,
            ports: Vec::new(),
            healthcheck: None,
        };
        let id = self.engine.create_container(&writer)?;
        let uploaded = self.engine.upload_archive(&id, "/secrets", archive);
        let removed = self.engine.remove_container(&id);
        uploaded?;
        removed?;
        Ok(())
    }

    fn identity(&self) -> ActorIdentity {
        if let Some(known) = self.identity.lock().unwrap().clone() {
            return known;
        }
        let mut identity = ActorIdentity {
            version: crate::identity::version().into(),
            commit: crate::identity::source_commit().into(),
            image: String::new(),
            digest: None,
        };
        let Some(id) = &self.config.self_container else {
            return identity;
        };
        let Ok(Some(me)) = self.status_engine.inspect_container(id) else {
            return identity;
        };
        identity.image = repository_of(&me.image);
        identity.digest = own_image(self.status_engine.as_ref(), &me).map(|i| i.digest);
        *self.identity.lock().unwrap() = Some(identity.clone());
        identity
    }

    fn inventory(&self) -> Result<Inventory, EngineError> {
        let containers = self.status_engine.list_containers()?;
        let installation = self
            .dir
            .load_machine()
            .ok()
            .flatten()
            .map(|m| m.installation_id);
        let me = self.config.self_container.as_deref();
        let mut services = Vec::new();
        for c in &containers {
            if c.labels.contains_key(labels::HELPER) {
                continue;
            }
            if Some(c.id.as_str()) == me {
                services.push(service(c, Role::RecoveryActor.as_str()));
                continue;
            }
            let ours = installation.is_some()
                && c.labels.get(labels::INSTALLATION) == installation.as_ref();
            if ours {
                if let Some(role) = c.labels.get(labels::PLATFORM_SERVICE) {
                    services.push(service(c, role));
                }
            }
        }
        let conflicts = crate::race_guard::conflicts(&containers, installation.as_deref(), me);
        services.sort_by_key(|s| (s.role != Role::RecoveryActor.as_str(), s.role.clone()));
        let seed = seed::find_running(&containers, self.config.seed_container.as_deref())
            .map(|c| self.seed_identity(c));
        Ok(Inventory {
            services,
            conflicts,
            seed,
        })
    }

    /// The seed's version is its image's `org.quasar.version` label; an image without one
    /// is this actor's own when the ids match. A seed whose version cannot be read is still
    /// a seed: it is reported as [`SEED_VERSION_UNKNOWN`], never as absent, since absent
    /// reads "not found" on the console.
    fn seed_identity(&self, seed: &Container) -> SeedIdentity {
        if let Some(known) = self.seed_images.lock().unwrap().get(&seed.image_id) {
            return known.clone();
        }
        let digest_of_ref = seed.image.split_once('@').map(|(_, d)| d.to_owned());
        let Ok(image) = self.status_engine.inspect_image(&seed.image_id) else {
            return SeedIdentity {
                version: SEED_VERSION_UNKNOWN.into(),
                digest: digest_of_ref,
            };
        };
        let version = image
            .as_ref()
            .and_then(|i| i.labels.get(labels::IMAGE_VERSION))
            .map(|v| crate::identity::normalized_version(v).to_owned())
            .or_else(|| {
                let own = self.config.self_container.as_deref()?;
                let me = self.status_engine.inspect_container(own).ok()??;
                (me.image_id == seed.image_id).then(|| crate::identity::version().to_owned())
            })
            .unwrap_or_else(|| SEED_VERSION_UNKNOWN.into());
        let repository = repository_of(&seed.image);
        let digest =
            digest_of_ref.or_else(|| image.as_ref().and_then(|i| digest_for(i, &repository)));
        let identity = SeedIdentity { version, digest };
        if image.is_some() {
            self.seed_images
                .lock()
                .unwrap()
                .insert(seed.image_id.clone(), identity.clone());
        }
        identity
    }
}

/// A container's image as `repository@sha256:…`: its configured reference when that is
/// digest-pinned, else the registry digest the engine knows for its repository.
pub(crate) fn own_image(
    engine: &dyn PlatformEngine,
    me: &Container,
) -> Option<seed::file::ActorImage> {
    if let Some(pinned) = seed::file::ActorImage::parse(&me.image) {
        return Some(pinned);
    }
    let repository = repository_of(&me.image);
    let image = engine.inspect_image(&me.image_id).ok()??;
    let digest = digest_for(&image, &repository)?;
    seed::file::ActorImage::parse(&format!("{repository}@{digest}"))
}

fn digest_for(image: &crate::engine::Image, repository: &str) -> Option<String> {
    image.repo_digests.iter().find_map(|d| {
        let (repo, digest) = d.split_once('@')?;
        (repo == repository).then(|| digest.to_owned())
    })
}

fn service(c: &Container, role: &str) -> Service {
    Service {
        role: role.into(),
        container: c.name.clone(),
        image: repository_of(&c.image),
        digest: c.image.split_once('@').map(|(_, d)| d.to_owned()),
        state: c.status.clone(),
        health: c.health.clone(),
    }
}

/// The reported version of a seed that exists but whose version could not be read.
/// `seed_version` is opaque (agent-api.md amendment 14): no consumer may parse or
/// special-case this value (ADR 0007).
pub const SEED_VERSION_UNKNOWN: &str = "unknown";

/// `registry/repo:tag@sha256:…` → `registry/repo`.
pub(crate) fn repository_of(reference: &str) -> String {
    let without_digest = reference.split('@').next().unwrap_or(reference);
    match without_digest.rsplit_once(':') {
        Some((repo, tag)) if !tag.contains('/') => repo.to_owned(),
        _ => without_digest.to_owned(),
    }
}

/// The node-agent recipe revision `image` declares, refused unless this actor carries it.
pub(crate) fn node_agent_revision(
    image: &crate::engine::Image,
    reference: &ImageRef,
) -> Result<u32, ResumeError> {
    supported_revision(Role::NodeAgent, image, reference)
}

/// [`node_agent_revision`], and on a combined host a revision whose agent reads the local
/// enrollment token's file (2 or later).
pub(crate) fn agent_revision_for(
    image: &crate::engine::Image,
    reference: &ImageRef,
    role: MachineRole,
) -> Result<u32, ResumeError> {
    let revision = node_agent_revision(image, reference)?;
    if role == MachineRole::Combined && revision < 2 {
        return Err(ResumeError::RecipeUnsupported(format!(
            "{} declares node-agent recipe revision {revision}; a combined host's agent needs revision 2 or later, which reads the local enrollment token",
            reference.reference()
        )));
    }
    Ok(revision)
}

/// The recipe revision `image` declares for `role`, refused unless this actor carries it.
pub(crate) fn supported_revision(
    role: Role,
    image: &crate::engine::Image,
    reference: &ImageRef,
) -> Result<u32, ResumeError> {
    let revision = image_revision(image, reference)?;
    if !recipe::Book::supports(role, revision) {
        return Err(RenderError::Unsupported { role, revision }.into());
    }
    Ok(revision)
}

pub(crate) fn image_revision(
    image: &crate::engine::Image,
    reference: &ImageRef,
) -> Result<u32, ResumeError> {
    let label = image.labels.get(labels::IMAGE_RECIPE).ok_or_else(|| {
        ResumeError::RecipeUnsupported(format!(
            "{} carries no {} label, so it cannot be installed by a recovery actor",
            reference.reference(),
            labels::IMAGE_RECIPE
        ))
    })?;
    label.trim().parse().map_err(|_| {
        ResumeError::RecipeUnsupported(format!(
            "{} declares {}={label:?}, which is not a revision",
            reference.reference(),
            labels::IMAGE_RECIPE
        ))
    })
}

/// Who a delivered secret file belongs to, and its mode.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct FileOwner {
    pub uid: u32,
    pub gid: u32,
    pub mode: u32,
}

impl FileOwner {
    pub const ROOT: FileOwner = FileOwner {
        uid: 0,
        gid: 0,
        mode: 0o400,
    };
}

fn tar_of(entries: &[(String, String)], owner: FileOwner) -> Result<Vec<u8>, ResumeError> {
    let mut builder = tar::Builder::new(Vec::new());
    for (name, value) in entries {
        let mut header = tar::Header::new_gnu();
        header.set_size(value.len() as u64);
        header.set_mode(owner.mode);
        header.set_uid(u64::from(owner.uid));
        header.set_gid(u64::from(owner.gid));
        header.set_mtime(0);
        header.set_entry_type(tar::EntryType::Regular);
        builder.append_data(&mut header, name, value.as_bytes())?;
    }
    Ok(builder.into_inner()?)
}

pub(crate) fn random_uuid() -> String {
    use ring::rand::{SecureRandom, SystemRandom};
    let mut b = [0u8; 16];
    SystemRandom::new()
        .fill(&mut b)
        .expect("the system random source");
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    let h: String = b.iter().map(|x| format!("{x:02x}")).collect();
    format!(
        "{}-{}-{}-{}-{}",
        &h[0..8],
        &h[8..12],
        &h[12..16],
        &h[16..20],
        &h[20..32]
    )
}

pub fn rfc3339_now() -> String {
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    rfc3339(secs)
}

fn rfc3339(secs: i64) -> String {
    let days = secs.div_euclid(86_400);
    let rem = secs.rem_euclid(86_400);
    // Howard Hinnant's civil_from_days.
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + i64::from(month <= 2);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}Z",
        rem / 3600,
        (rem % 3600) / 60,
        rem % 60
    )
}
