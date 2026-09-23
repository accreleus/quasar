//! Diagnostic registration (#256; CONTEXT.md "Diagnostic registration"): what the agent
//! withholds while its startup cleanup is unresolved, and when it resumes. The decision
//! is pure; `agent::run` owns the effects.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use futures_util::{SinkExt, StreamExt};
use tokio_tungstenite::tungstenite::{self, Message};
use tracing::{debug, error, info, warn};

use crate::images::ImageManager;
use crate::messages::{AgentMsg, ControlMsg, ReadinessCheck};
use crate::readiness::report::ReadinessReport;
use crate::runtime::RuntimeClient;

/// The agent's own safety state on the readiness card. Retained through
/// `ReadinessReport`, so no local refresh can drop it; no override lifts the refusal.
pub const STARTUP_CLEANUP_ID: &str = "startup_cleanup";
pub const POLICY_JOURNAL_ID: &str = "policy_journal";

/// Work that must not start while the startup cleanup is unresolved.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Work {
    HomesGc,
    DriverVolumeProvisioner,
    CudaRuntimeProvisioner,
    ImagePulls,
    ImagePruning,
    HostProbes,
}

pub const WITHHELD: [Work; 6] = [
    Work::HomesGc,
    Work::DriverVolumeProvisioner,
    Work::CudaRuntimeProvisioner,
    Work::ImagePulls,
    Work::ImagePruning,
    Work::HostProbes,
];

/// What one startup cleanup pass observed. Both halves come from the same pass: a
/// remembered "the engine answered earlier" must never stand in for `engine`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CleanupAttempt {
    /// The engine answered discovery on the configured endpoint.
    pub engine: Result<(), String>,
    /// Every application record left by a prior agent is proven terminal. An unreachable
    /// engine is an `Err` here whenever a record exists: absence of an answer is never
    /// absence of a container.
    pub retirement: Result<(), String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Fault {
    RuntimeUnusable(String),
    CleanupUnresolved(String),
    PolicyJournalCorrupt,
    PolicyJournalWriteFailed,
}

impl Fault {
    fn is_policy_journal(&self) -> bool {
        matches!(
            self,
            Self::PolicyJournalCorrupt | Self::PolicyJournalWriteFailed
        )
    }
}

impl Fault {
    /// Stable word for logs and the refusal reason.
    pub fn code(&self) -> &'static str {
        match self {
            Fault::RuntimeUnusable(_) => "runtime_unusable",
            Fault::CleanupUnresolved(_) => "startup_cleanup_unresolved",
            Fault::PolicyJournalCorrupt => "policy_journal_unavailable",
            Fault::PolicyJournalWriteFailed => "policy_journal_write_failed",
        }
    }

    fn sentence(&self) -> String {
        match self {
            Fault::RuntimeUnusable(detail) => {
                format!("the container runtime is unusable ({detail})")
            }
            Fault::CleanupUnresolved(detail) => format!(
                "cleanup of applications left by the previous agent has not finished ({detail})"
            ),
            Fault::PolicyJournalCorrupt => {
                "the host configuration journal cannot be read safely".into()
            }
            Fault::PolicyJournalWriteFailed => {
                "the host configuration journal cannot be updated safely".into()
            }
        }
    }
}

impl CleanupAttempt {
    /// The engine fault is reported first: it is the cause whenever both halves fail.
    pub fn fault(&self) -> Option<Fault> {
        match (&self.engine, &self.retirement) {
            (Err(detail), _) => Some(Fault::RuntimeUnusable(detail.clone())),
            (Ok(()), Err(detail)) => Some(Fault::CleanupUnresolved(detail.clone())),
            (Ok(()), Ok(())) => None,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Phase {
    Diagnostic(Fault),
    Normal,
}

impl Phase {
    pub fn may_start(&self, work: Work) -> bool {
        match self {
            Phase::Normal => true,
            Phase::Diagnostic(Fault::PolicyJournalCorrupt | Fault::PolicyJournalWriteFailed) => {
                matches!(work, Work::ImagePulls | Work::ImagePruning)
            }
            Phase::Diagnostic(_) => false,
        }
    }

    pub fn health_ready(&self) -> bool {
        matches!(self, Phase::Normal)
    }

    /// A journal fault can still register for independent image work, but it
    /// must not advertise a configuration writer until a later safe boot.
    pub fn policy_available(&self) -> bool {
        !matches!(self, Phase::Diagnostic(fault) if fault.is_policy_journal())
    }

    /// `Some` refuses every launch. Carried on the existing `session_assign` nack.
    pub fn launch_refusal(&self) -> Option<String> {
        match self {
            Phase::Normal => None,
            Phase::Diagnostic(Fault::PolicyJournalCorrupt) => Some(
                "host in diagnostic mode (policy_journal_unavailable): the host configuration journal cannot be read safely. Every launch remains refused until an operator repairs the journal and restarts the agent; no readiness override lifts this.".into(),
            ),
            Phase::Diagnostic(Fault::PolicyJournalWriteFailed) => Some(
                "host in diagnostic mode (policy_journal_write_failed): the host configuration journal cannot be updated safely. Every launch remains refused until an operator repairs journal storage and restarts the agent; no readiness override lifts this.".into(),
            ),
            Phase::Diagnostic(fault) => Some(format!(
                "host in diagnostic mode ({}): {}. This host refuses every launch until its \
                 startup cleanup succeeds; the agent retries on its own and no override lifts \
                 this.",
                fault.code(),
                fault.sentence()
            )),
        }
    }

    pub fn safety_check(&self) -> Option<ReadinessCheck> {
        let Phase::Diagnostic(fault) = self else {
            return None;
        };
        let (id, remediation, source) = if matches!(fault, Fault::PolicyJournalCorrupt) {
            (POLICY_JOURNAL_ID,
             "Inspect the agent's host configuration journal and its persistent mount. If its contents are invalid, repair it from a verified backup, then restart the agent. Keep operation journals and managed homes in place; do not clear admission protection to bypass this failure.",
             "agent")
        } else if matches!(fault, Fault::PolicyJournalWriteFailed) {
            (POLICY_JOURNAL_ID,
             "Restore write access and free space on the agent's persistent journal mount, then restart the agent. Preserve the existing journal and managed homes; do not restore an older journal or clear admission protection to bypass this failure.",
             "agent")
        } else {
            (STARTUP_CLEANUP_ID,
             "Start or repair the container runtime on the endpoint this agent is configured for (see the Container runtime checks), and leave the agent's operation journals and managed homes in place. The agent retries the cleanup on its own and resumes without a restart.",
             "runtime")
        };
        Some(ReadinessCheck {
            id: id.into(),
            status: crate::readiness::FAIL.into(),
            summary: format!(
                "Diagnostic mode: {}. Every launch on this host is refused.",
                fault.sentence()
            ),
            remediation: remediation.into(),
            observed_at: None,
            source: Some(source.into()),
            // Agent-enforced: it refuses these launches itself, and no readiness
            // override lifts them (protocol/agent-api.md `readiness`).
            blocks: Some(crate::messages::ReadinessBlocks::host("agent")),
        })
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Transition {
    StillDiagnostic,
    /// Returned by exactly one `observe` per process.
    Resumed,
    AlreadyNormal,
}

/// The startup phase over the life of the process. Normal is absorbing: an engine lost
/// after startup is the runtime checks' business, never a second diagnostic mode.
#[derive(Debug)]
pub struct Startup {
    phase: Phase,
}

impl Startup {
    pub fn begin(first: &CleanupAttempt) -> Self {
        Self {
            phase: first.fault().map_or(Phase::Normal, Phase::Diagnostic),
        }
    }

    pub fn phase(&self) -> &Phase {
        &self.phase
    }

    pub fn observe(&mut self, retry: &CleanupAttempt) -> Transition {
        if self.phase == Phase::Normal {
            return Transition::AlreadyNormal;
        }
        if matches!(&self.phase, Phase::Diagnostic(fault) if fault.is_policy_journal()) {
            return Transition::StillDiagnostic;
        }
        match retry.fault() {
            Some(fault) => {
                self.phase = Phase::Diagnostic(fault);
                Transition::StillDiagnostic
            }
            None => {
                self.phase = Phase::Normal;
                Transition::Resumed
            }
        }
    }
}

// Startup cleanup may resume before an independently corrupt policy journal is
// discovered. Retain both stations: a later safety refusal must still gate work.
static PROCESS_STATIONS: std::sync::OnceLock<Mutex<Vec<Arc<Station>>>> = std::sync::OnceLock::new();

/// Whether `work` may start in this process: always, unless it is in diagnostic mode.
pub fn may_start(work: Work) -> bool {
    PROCESS_STATIONS.get().is_none_or(|stations| {
        stations
            .lock()
            .unwrap()
            .iter()
            .all(|station| station.phase().may_start(work))
    })
}

/// The one flag the host-probe orchestrator reads before starting a probe. False when
/// this process never entered diagnostic mode.
pub fn host_probes_withheld() -> bool {
    PROCESS_STATIONS.get().is_some_and(|stations| {
        stations
            .lock()
            .unwrap()
            .iter()
            .any(|station| station.host_probes_withheld())
    })
}

/// One boot cleanup pass: discovery, then every journalled obligation a previous agent
/// can have left, replayed through the runtime API under the identity it was journalled
/// with. Nothing here issues a new operation identity, creates a container, or falls back
/// to a CLI (docs/runtime-api-recovery.md "Uncertainty and failure handling").
///
/// Blocking — callers offload it.
pub fn startup_cleanup(client: &RuntimeClient) -> CleanupAttempt {
    let engine = match client.discover().wait() {
        Ok(engine) => {
            info!(token = "runtime-engine-discovered", engine = %engine.name,
                version = %engine.version, api = %engine.api_version,
                "container engine discovered; capability support requires separate validation");
            Ok(())
        }
        Err(error) => {
            warn!(token = "runtime-engine-unavailable", %error,
                "engine discovery failed; check Docker Unix socket configuration and access");
            Err(error.to_string())
        }
    };
    if let Err(error) = client.recover_diagnostics().wait() {
        warn!(token = "runtime-diagnostic-recovery-pending", %error,
            "diagnostic recovery remains pending; host-path validation will retry before launching another helper");
    }
    if let Err(error) = client.recover_application_cleanup().wait() {
        warn!(token = "runtime-application-cleanup-pending", %error,
            "stopped application cleanup remains journalled; active applications were preserved");
    }
    let retirement = match client.retire_applications().wait() {
        Ok(()) => Ok(()),
        Err(error) => {
            error!(token = "runtime-application-retirement-pending", %error,
                "previous application retirement is unresolved; managed homes stay protected until it completes");
            Err(error.to_string())
        }
    };
    // Never touch audio or the legacy sweep while an API-owned application may still
    // own a managed home.
    if let Some(swept) = crate::agent::post_application_retirement(
        retirement.is_ok(),
        || {
            // Boot-only retirement, after acquiring the persistent owner lease.
            // Routine recovery never stops an active audio sibling.
            if let Err(error) = client.retire_audio_sidecars().wait() {
                warn!(token = "runtime-audio-retirement-pending", %error,
                    "previous audio cleanup remains journalled; retry runtime recovery when Docker is available");
            }
            // A probe's deadline died with the previous process; boot is the one
            // pass allowed to stop one still running.
            if let Err(error) = client.retire_gpu_probes().wait() {
                warn!(token = "runtime-probe-retirement-pending", %error,
                    "previous host-probe cleanup remains journalled; the maintenance pass and the next probe retry it");
            }
            // A killed agent never runs a session's udev-export Drop, nor a
            // media probe's runtime-dir Drop (the same failure mode, a
            // recreate or the NVIDIA agent's own self-restart mid-probe). "Ours"
            // implies "dead" only here, right after application retirement —
            // never the periodic maintenance tick.
            match crate::container_ownership::token() {
                Ok(owner) => {
                    let summary = crate::session::udev_export::retire_all_owned(
                        &crate::session::default_runtime_dir(),
                        &owner,
                    );
                    if summary.errors > 0 {
                        warn!(
                            token = "udev-export-reconcile-errors",
                            removed = summary.removed,
                            unattributable = summary.unattributable,
                            errors = summary.errors,
                            "boot udev-export reconciliation: {summary:?}"
                        );
                    } else if summary.removed > 0 || summary.unattributable > 0 {
                        info!(
                            token = "udev-export-retired",
                            removed = summary.removed,
                            unattributable = summary.unattributable,
                            errors = summary.errors,
                            "boot udev-export reconciliation: {summary:?}"
                        );
                    }
                    let media_summary = crate::host_probe::media_probe_dir::retire_all_owned(
                        &crate::host_probe::media_probe_dir::probe_parent_dir(),
                        &owner,
                    );
                    if media_summary.errors > 0 {
                        warn!(
                            token = "media-probe-dir-reconcile-errors",
                            removed = media_summary.removed,
                            unattributable = media_summary.unattributable,
                            errors = media_summary.errors,
                            "boot media-probe-dir reconciliation: {media_summary:?}"
                        );
                    } else if media_summary.removed > 0 || media_summary.unattributable > 0 {
                        info!(
                            token = "media-probe-dir-retired",
                            removed = media_summary.removed,
                            unattributable = media_summary.unattributable,
                            errors = media_summary.errors,
                            "boot media-probe-dir reconciliation: {media_summary:?}"
                        );
                    }
                }
                Err(error) => warn!(token = "owned-entry-retire-no-owner", %error,
                    "no owner token at boot; skipping udev-export and media-probe-dir reconciliation"),
            }
        },
        || crate::agent::legacy_container_sweep(client),
    ) {
        if swept > 0 {
            info!("startup sweep removed {swept} legacy container(s) from a prior run");
        }
    }
    CleanupAttempt { engine, retirement }
}

/// [`startup_cleanup`] against the configured endpoint. An endpoint configuration this
/// agent cannot use is both halves' fault, reported without an engine call.
pub fn startup_cleanup_configured() -> CleanupAttempt {
    match crate::runtime::configured() {
        Ok(client) => startup_cleanup(client),
        Err(error) => {
            let detail = error.to_string();
            error!(token = "runtime-endpoint-unconfigured", %error,
                "the container runtime endpoint configuration was refused; no engine was contacted \
                 (docs/configuration.md, DOCKER_HOST)");
            CleanupAttempt {
                engine: Err(detail.clone()),
                retirement: Err(detail),
            }
        }
    }
}

/// The process's diagnostic mode: the startup phase, the readiness card entry that
/// outlives every local refresh, and the one-shot resume signal every waiter selects on.
///
/// Created only by a first pass that failed, so its existence is "this process is in
/// diagnostic mode".
#[derive(Debug)]
pub struct Station {
    startup: Mutex<Startup>,
    /// Holds the safety check as retained, so no local refresh can drop it.
    readiness: Mutex<ReadinessReport>,
    resumes: AtomicUsize,
    resumed: tokio::sync::watch::Sender<bool>,
}

impl Station {
    /// `None` when the first pass was clean: that process never enters diagnostic mode.
    pub fn enter(first: &CleanupAttempt) -> Option<Arc<Station>> {
        let startup = Startup::begin(first);
        Self::from_startup(startup)
    }

    /// A corrupt policy journal is a fixed diagnostic state. Cleanup retries
    /// cannot repair it or authorize resumed admission in this process.
    pub fn policy_journal_corrupt() -> Arc<Station> {
        Self::from_startup(Startup {
            phase: Phase::Diagnostic(Fault::PolicyJournalCorrupt),
        })
        .expect("policy journal fault is diagnostic")
    }

    pub fn policy_journal_write_failed() -> Arc<Station> {
        Self::from_startup(Startup {
            phase: Phase::Diagnostic(Fault::PolicyJournalWriteFailed),
        })
        .expect("policy journal write fault is diagnostic")
    }

    fn from_startup(startup: Startup) -> Option<Arc<Station>> {
        let check = startup.phase().safety_check()?;
        let mut readiness = ReadinessReport::default();
        readiness.retain(check, SystemTime::now());
        Some(Arc::new(Station {
            startup: Mutex::new(startup),
            readiness: Mutex::new(readiness),
            resumes: AtomicUsize::new(0),
            resumed: tokio::sync::watch::channel(false).0,
        }))
    }

    pub fn phase(&self) -> Phase {
        self.startup.lock().unwrap().phase().clone()
    }

    /// Fold one retry pass in. Resumes at most once per process ([`Startup::observe`]).
    pub fn observe(&self, retry: &CleanupAttempt) -> Transition {
        let mut startup = self.startup.lock().unwrap();
        let transition = startup.observe(retry);
        match transition {
            Transition::StillDiagnostic => {
                if let Some(check) = startup.phase().safety_check() {
                    self.readiness
                        .lock()
                        .unwrap()
                        .retain(check, SystemTime::now());
                }
            }
            Transition::Resumed => {
                self.readiness.lock().unwrap().forget(STARTUP_CLEANUP_ID);
                self.resumes.fetch_add(1, Ordering::SeqCst);
                // `send_replace`, never `send`: a resume is a fact about this process,
                // not a notification. `Sender::send` discards the value outright when no
                // receiver happens to exist, and every waiter here is a `resumed()`
                // future that lives only while its `select!` is polling — dropped for the
                // whole of each arm body. A `send` landing in that window would be lost
                // for good, because a process resumes exactly once and nothing
                // re-publishes it (#269).
                self.resumed.send_replace(true);
            }
            Transition::AlreadyNormal => {}
        }
        transition
    }

    pub fn resumes(&self) -> usize {
        self.resumes.load(Ordering::SeqCst)
    }

    /// The readiness card a capacity message carries: `local` refreshed in, then merged
    /// with what is retained.
    pub fn readiness(&self, local: Vec<ReadinessCheck>) -> Vec<ReadinessCheck> {
        let mut report = self.readiness.lock().unwrap();
        report.refreshed(local, SystemTime::now());
        report.merged()
    }

    pub fn host_probes_withheld(&self) -> bool {
        matches!(self.phase(), Phase::Diagnostic(_))
    }

    /// Resolves once this process has resumed, immediately if it already has — including
    /// when the resume was published before this waiter existed ([`Station::observe`]).
    pub async fn resumed(&self) {
        let mut rx = self.resumed.subscribe();
        loop {
            if *rx.borrow_and_update() {
                return;
            }
            if rx.changed().await.is_err() {
                // The sender lives in this Station, so only a dropped Station gets
                // here; a resume can no longer arrive.
                std::future::pending::<()>().await;
            }
        }
    }
}

/// Retains every process safety station so a later independent fault remains
/// enforced after an earlier startup cleanup resumed.
pub(crate) fn install_process_wide(station: &Arc<Station>) {
    let mut stations = PROCESS_STATIONS
        .get_or_init(|| Mutex::new(Vec::new()))
        .lock()
        .unwrap();
    if !stations
        .iter()
        .any(|existing| Arc::ptr_eq(existing, station))
    {
        stations.push(station.clone());
    }
}

/// How a retry loop paces itself. A plain [`Duration`] is a fixed interval; production
/// starts at [`RetryPace::PRODUCTION`] and doubles to its cap.
#[derive(Debug, Clone, Copy)]
pub struct RetryPace {
    first: Duration,
    cap: Duration,
}

impl RetryPace {
    pub const PRODUCTION: RetryPace = RetryPace {
        first: Duration::from_secs(5),
        cap: Duration::from_secs(60),
    };

    fn next(self, current: Duration) -> Duration {
        (current * 2).min(self.cap)
    }
}

impl From<Duration> for RetryPace {
    fn from(fixed: Duration) -> Self {
        RetryPace {
            first: fixed,
            cap: fixed,
        }
    }
}

/// Retry the startup cleanup until this process resumes. Independent of any
/// control-plane connection by construction: nothing here reads or waits on one.
pub async fn retry_until_resumed<F>(station: Arc<Station>, cleanup: F, pace: impl Into<RetryPace>)
where
    F: Fn() -> CleanupAttempt + Send + Sync + 'static,
{
    let pace = pace.into();
    let cleanup = Arc::new(cleanup);
    let mut interval = pace.first;
    loop {
        tokio::time::sleep(interval).await;
        let pass = cleanup.clone();
        let attempt = match tokio::task::spawn_blocking(move || pass()).await {
            Ok(attempt) => attempt,
            Err(error) => {
                error!(
                    token = "boot-diagnostic-retry-lost",
                    "the diagnostic startup-cleanup retry did not complete ({error}); this host \
                     stays in diagnostic mode until it is restarted"
                );
                return;
            }
        };
        match station.observe(&attempt) {
            Transition::StillDiagnostic => {
                interval = pace.next(interval);
                // Already paced by the sleep above, so one line per pass is bounded.
                info!(
                    token = "boot-diagnostic-retry-pending",
                    fault = attempt.fault().map_or("", |fault| fault.code()),
                    "startup cleanup is still unresolved; retrying in {interval:?}"
                );
            }
            Transition::Resumed | Transition::AlreadyNormal => return,
        }
    }
}

/// How a diagnostic connection ended. A `restart` exits the process instead.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConnectionEnd {
    /// The station resumed; normal startup takes this process over.
    Resumed,
}

/// One diagnostic control-plane connection, handshake included.
///
/// A caller that must persist what the handshake returned (an enrollment mints a
/// `node_secret` exactly once) does its own `register`/`registered` exchange and calls
/// [`serve_registered`]; production does.
#[cfg(test)]
pub async fn serve_connection<S, R, F>(
    sink: &mut S,
    stream: &mut R,
    register: AgentMsg,
    station: &Arc<Station>,
    observe: F,
    refresh: Duration,
) -> anyhow::Result<ConnectionEnd>
where
    S: SinkExt<Message, Error = tungstenite::Error> + Unpin,
    R: StreamExt<Item = Result<Message, tungstenite::Error>> + Unpin,
    F: FnMut() -> (AgentMsg, Vec<ReadinessCheck>) + Send,
{
    crate::agent::send(sink, &register).await?;
    let raw = crate::agent::recv(stream).await?;
    let heartbeat_interval_ms = match serde_json::from_str::<ControlMsg>(&raw)? {
        ControlMsg::Registered {
            host_id,
            heartbeat_interval_ms,
            ..
        } => {
            info!("registered as host {host_id} in diagnostic mode");
            heartbeat_interval_ms
        }
        ControlMsg::Error { code, message } => {
            anyhow::bail!("control plane rejected register: {code}: {message}")
        }
        _ => anyhow::bail!("unexpected message type before registered"),
    };
    serve_registered(
        sink,
        stream,
        heartbeat_interval_ms,
        station,
        observe,
        refresh,
        None,
    )
    .await
}

/// The diagnostic connection after `registered`: report, heartbeat, refuse, and end the
/// moment this process resumes. Every wait on the peer is in the select with the resume
/// arm, so a dead control plane can never delay a resume. The arm bodies themselves run
/// with no waiter subscribed; what keeps them from swallowing a resume that lands there
/// is that [`Station::observe`] retains it for the next waiter, not this select (#269).
pub async fn serve_registered<S, R, F>(
    sink: &mut S,
    stream: &mut R,
    heartbeat_interval_ms: u64,
    station: &Arc<Station>,
    mut observe: F,
    refresh: Duration,
    image_mgr: Option<&Arc<ImageManager>>,
) -> anyhow::Result<ConnectionEnd>
where
    S: SinkExt<Message, Error = tungstenite::Error> + Unpin,
    R: StreamExt<Item = Result<Message, tungstenite::Error>> + Unpin,
    F: FnMut() -> (AgentMsg, Vec<ReadinessCheck>) + Send,
{
    let (image_tx, image_rx) = tokio::sync::mpsc::channel(64);
    let _image_guard = image_mgr.map(|manager| manager.attach_upstream(image_tx));
    let mut image_rx = image_mgr.map(|_| image_rx);
    report_capacity(sink, station, &mut observe).await?;

    let mut heartbeat = tokio::time::interval(Duration::from_millis(heartbeat_interval_ms.max(1)));
    heartbeat.tick().await; // discard the immediate first tick
    let mut refresher = tokio::time::interval(refresh);
    refresher.tick().await;

    loop {
        tokio::select! {
            // Biased: nothing that could read as ready may follow a resume.
            biased;
            () = station.resumed() => return Ok(ConnectionEnd::Resumed),
            _ = heartbeat.tick() => {
                let ts_unix_ms = SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .unwrap_or_default()
                    .as_millis() as i64;
                crate::agent::send(sink, &AgentMsg::Heartbeat {
                    running_sessions: Vec::new(),
                    ts_unix_ms,
                    gpu_vram: None,
                }).await?;
            }
            _ = refresher.tick() => report_capacity(sink, station, &mut observe).await?,
            image = async {
                match &mut image_rx {
                    Some(rx) => rx.recv().await,
                    None => std::future::pending().await,
                }
            } => {
                if let Some(image) = image {
                    crate::agent::send(sink, &image).await?;
                } else {
                    image_rx = None;
                }
            },
            inbound = stream.next() => {
                let raw = match inbound {
                    None | Some(Ok(Message::Close(_))) => {
                        anyhow::bail!("WebSocket closed by server")
                    }
                    Some(Err(error)) => return Err(error.into()),
                    Some(Ok(Message::Text(text))) => text.to_string(),
                    Some(Ok(_)) => continue,
                };
                answer(sink, station, image_mgr, &raw).await?;
            }
        }
    }
}

/// `observe()` is blocking in production (capacity detection + the readiness probe), so
/// it runs off the runtime worker polling this connection.
async fn report_capacity<S, F>(
    sink: &mut S,
    station: &Arc<Station>,
    observe: &mut F,
) -> anyhow::Result<()>
where
    S: SinkExt<Message, Error = tungstenite::Error> + Unpin,
    F: FnMut() -> (AgentMsg, Vec<ReadinessCheck>) + Send,
{
    let (mut message, local) = tokio::task::block_in_place(observe);
    if let AgentMsg::Capacity { readiness, .. } = &mut message {
        *readiness = Some(station.readiness(local));
    }
    crate::agent::send(sink, &message).await
}

/// The `id` a control message expects its ack under, read off the envelope. `None` for
/// the no-ack messages (`registered`, `config_update`, `signaling`, `error`), none of
/// which carries one — agent-api.md.
fn ack_id(raw: &str) -> Option<String> {
    serde_json::from_str::<serde_json::Value>(raw)
        .ok()?
        .get("id")?
        .as_str()
        .map(str::to_owned)
}

/// Answer one control message. Image work remains independent of a corrupt policy
/// journal; commands tied to host configuration or sessions are refused.
async fn answer<S>(
    sink: &mut S,
    station: &Arc<Station>,
    image_mgr: Option<&Arc<ImageManager>>,
    raw: &str,
) -> anyhow::Result<()>
where
    S: SinkExt<Message, Error = tungstenite::Error> + Unpin,
{
    let phase = station.phase();
    let refusal = phase.launch_refusal();
    let control: ControlMsg = match serde_json::from_str(raw) {
        Ok(control) => control,
        Err(error) => {
            // A body this host will never act on must not decide whether the control
            // plane gets its answer: anything still carrying an ack id is refused, so a
            // launch cannot hang on an unparseable field. Only commands carry `id`.
            warn!(
                token = "diagnostic-control-message-malformed",
                "malformed control message: {error} (raw={raw})"
            );
            if let Some(id) = ack_id(raw) {
                crate::agent::send(
                    sink,
                    &AgentMsg::Ack {
                        id,
                        ok: false,
                        error: refusal,
                    },
                )
                .await?;
            }
            return Ok(());
        }
    };
    let nack = |id: String| AgentMsg::Ack {
        id,
        ok: false,
        error: refusal.clone(),
    };
    let image_commands_allowed =
        matches!(&phase, Phase::Diagnostic(fault) if fault.is_policy_journal());
    match control {
        ControlMsg::SessionAssign { id, session_id, .. } => {
            warn!(
                token = "session-assign-refused-diagnostic",
                session_id = %session_id,
                "refusing the assignment: {}",
                refusal.clone().unwrap_or_default()
            );
            crate::agent::send(sink, &nack(id)).await?;
        }
        ControlMsg::SessionStart { id, .. }
        | ControlMsg::SessionStop { id, .. }
        | ControlMsg::SessionSwapApp { id, .. }
        | ControlMsg::SessionDisplayUpdate { id, .. }
        | ControlMsg::SessionCapture { id, .. }
        | ControlMsg::ReleaseApply { id, .. } => {
            warn!(
                token = "control-command-refused-diagnostic",
                "refusing command {id}: {}",
                refusal.clone().unwrap_or_default()
            );
            crate::agent::send(sink, &nack(id)).await?;
        }
        ControlMsg::ImageEnsure {
            id,
            image_id,
            registry_ref,
            version,
        } => {
            let reply = image_mgr
                .filter(|_| image_commands_allowed)
                .map(|manager| manager.handle_ensure(id.clone(), image_id, registry_ref, version))
                .unwrap_or_else(|| nack(id));
            crate::agent::send(sink, &reply).await?;
        }
        ControlMsg::ImageRemove { id, image_id } => {
            let reply = image_mgr
                .filter(|_| image_commands_allowed)
                .map(|manager| manager.handle_remove(id.clone(), image_id))
                .unwrap_or_else(|| nack(id));
            crate::agent::send(sink, &reply).await?;
        }
        ControlMsg::ImageBuild {
            id,
            image_id,
            context_url,
            context_subdir,
            dockerfile,
            build_args,
            local_tag,
            version,
        } => {
            let reply = image_mgr
                .filter(|_| image_commands_allowed)
                .map(|manager| {
                    manager.handle_build(
                        id.clone(),
                        image_id,
                        context_url,
                        context_subdir,
                        dockerfile,
                        build_args,
                        local_tag,
                        version,
                    )
                })
                .unwrap_or_else(|| nack(id));
            crate::agent::send(sink, &reply).await?;
        }
        ControlMsg::Restart { id } => {
            if matches!(&phase, Phase::Diagnostic(fault) if fault.is_policy_journal()) {
                crate::agent::send(sink, &nack(id)).await?;
                return Ok(());
            }
            // Cleanup diagnostic mode still lets an operator restart the agent.
            info!("restart requested (cmd {id}); acking then exiting for config reload");
            let _ = crate::agent::send(
                sink,
                &AgentMsg::Ack {
                    id,
                    ok: true,
                    error: None,
                },
            )
            .await;
            tokio::time::sleep(Duration::from_millis(250)).await;
            std::process::exit(0);
        }
        ControlMsg::ConfigPolicyOffer {
            attempt_id,
            host_id,
            boot_incarnation,
            connection_incarnation,
            group,
            revision,
            content_sha256,
            scope,
            ..
        } => {
            crate::agent::send(
                sink,
                &AgentMsg::ConfigPolicyState {
                    attempt_id,
                    host_id,
                    group,
                    revision,
                    content_sha256,
                    scope,
                    grant_boot_incarnation: boot_incarnation,
                    grant_connection_incarnation: connection_incarnation,
                    journal_sequence: "0".into(),
                    phase: "failed".into(),
                    active_scope: None,
                    evidence: None,
                    error: Some("diagnostic_mode".into()),
                },
            )
            .await?;
        }
        // No ack, and nothing to apply while every launch is refused.
        ControlMsg::Registered { .. }
        | ControlMsg::ConfigUpdate { .. }
        | ControlMsg::ConfigPolicyJournalInventoryRequest { .. }
        | ControlMsg::Signaling { .. }
        | ControlMsg::Error { .. }
        | ControlMsg::Unknown => {
            debug!(
                token = "diagnostic-message-ignored",
                "ignoring a control message with nothing to do in diagnostic mode"
            );
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::pin::Pin;
    use std::task::{Context, Poll};

    #[derive(Default)]
    struct Outbound(Vec<Message>);

    impl futures_util::Sink<Message> for Outbound {
        type Error = tungstenite::Error;

        fn poll_ready(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
            Poll::Ready(Ok(()))
        }
        fn start_send(self: Pin<&mut Self>, item: Message) -> Result<(), Self::Error> {
            self.get_mut().0.push(item);
            Ok(())
        }
        fn poll_flush(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
            Poll::Ready(Ok(()))
        }
        fn poll_close(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
            Poll::Ready(Ok(()))
        }
    }

    fn attempt(engine: Result<(), &str>, retirement: Result<(), &str>) -> CleanupAttempt {
        CleanupAttempt {
            engine: engine.map_err(str::to_owned),
            retirement: retirement.map_err(str::to_owned),
        }
    }

    #[test]
    fn the_withheld_set_is_everything_the_boot_exit_prevented() {
        assert_eq!(
            WITHHELD.to_vec(),
            vec![
                Work::HomesGc,
                Work::DriverVolumeProvisioner,
                Work::CudaRuntimeProvisioner,
                Work::ImagePulls,
                Work::ImagePruning,
                Work::HostProbes,
            ]
        );
        let diagnostic = Phase::Diagnostic(Fault::RuntimeUnusable("refused".into()));
        for work in WITHHELD {
            assert!(
                !diagnostic.may_start(work),
                "{work:?} started in diagnostic mode"
            );
            assert!(
                Phase::Normal.may_start(work),
                "{work:?} withheld after resume"
            );
        }
    }

    #[test]
    fn diagnostic_mode_is_not_ready_and_refuses_every_launch_with_a_readable_reason() {
        for fault in [
            Fault::RuntimeUnusable("no engine answered".into()),
            Fault::CleanupUnresolved("unknown outcome".into()),
        ] {
            let code = fault.code();
            let phase = Phase::Diagnostic(fault);
            assert!(!phase.health_ready());
            let reason = phase.launch_refusal().expect("diagnostic mode refuses");
            assert!(reason.contains("diagnostic mode"), "{reason}");
            assert!(reason.contains(code), "{reason}");
            assert!(reason.contains("no override"), "{reason}");
            let check = phase
                .safety_check()
                .expect("diagnostic mode reports its check");
            assert_eq!(check.id, STARTUP_CLEANUP_ID);
            assert_eq!(check.status, crate::readiness::FAIL);
            assert!(!check.remediation.is_empty());
        }
        assert!(Phase::Normal.health_ready());
        assert_eq!(Phase::Normal.launch_refusal(), None);
        assert_eq!(Phase::Normal.safety_check(), None);
    }

    #[test]
    fn corrupt_policy_journal_stays_in_operator_repair_diagnostic_mode() {
        let station = Station::policy_journal_corrupt();
        let phase = station.phase();
        assert!(!phase.health_ready());
        let refusal = phase.launch_refusal().expect("launch must be refused");
        assert!(refusal.contains("policy_journal_unavailable"));
        assert!(refusal.contains("repairs the journal"));
        let checks = station.readiness(Vec::new());
        let check = checks
            .iter()
            .find(|check| check.id == POLICY_JOURNAL_ID)
            .expect("agent must report a durable-journal readiness failure");
        assert_eq!(check.status, crate::readiness::FAIL);
        assert_eq!(
            check.blocks,
            Some(crate::messages::ReadinessBlocks::host("agent"))
        );
        assert!(check.remediation.contains("verified backup"));
        for work in WITHHELD {
            assert_eq!(
                phase.may_start(work),
                matches!(work, Work::ImagePulls | Work::ImagePruning),
                "{work:?} has the wrong policy-journal dependency"
            );
        }
        assert_eq!(
            station.observe(&attempt(Ok(()), Ok(()))),
            Transition::StillDiagnostic
        );
        assert_eq!(station.resumes(), 0);
        assert!(matches!(
            station.phase(),
            Phase::Diagnostic(Fault::PolicyJournalCorrupt)
        ));
    }

    #[test]
    fn unwritable_policy_journal_preserves_it_and_names_storage_remedy() {
        let station = Station::policy_journal_write_failed();
        let phase = station.phase();
        assert!(!phase.health_ready());
        assert!(phase
            .launch_refusal()
            .unwrap()
            .contains("policy_journal_write_failed"));
        let check = station
            .readiness(Vec::new())
            .into_iter()
            .find(|check| check.id == POLICY_JOURNAL_ID)
            .unwrap();
        assert!(check.remediation.contains("free space"));
        assert!(check.remediation.contains("Preserve the existing journal"));
        assert!(!check.remediation.contains("verified backup"));
        for work in WITHHELD {
            assert_eq!(
                phase.may_start(work),
                matches!(work, Work::ImagePulls | Work::ImagePruning)
            );
        }
        assert_eq!(
            station.observe(&attempt(Ok(()), Ok(()))),
            Transition::StillDiagnostic
        );
    }

    #[tokio::test]
    async fn policy_journal_faults_refuse_restart_and_policy_offers_on_the_wire() {
        for station in [
            Station::policy_journal_corrupt(),
            Station::policy_journal_write_failed(),
        ] {
            let mut outbound = Outbound::default();
            answer(
                &mut outbound,
                &station,
                None,
                r#"{"type":"restart","id":"restart-1"}"#,
            )
            .await
            .unwrap();
            answer(&mut outbound, &station, None, r#"{"type":"config_policy_offer","attempt_id":"a","host_id":"h","boot_incarnation":"b","connection_incarnation":"c","group":"hardware","revision":"r","content_sha256":"s","scope":"host","expires_at":"now","prerequisites_sha256":"p","prerequisites":[],"settings":{},"resolved_settings":{}}"#)
                .await.unwrap();
            let replies: Vec<serde_json::Value> = outbound
                .0
                .iter()
                .map(|message| serde_json::from_str(message.to_text().unwrap()).unwrap())
                .collect();
            assert_eq!(replies.len(), 2);
            assert_eq!(replies[0]["type"], "ack");
            assert_eq!(replies[0]["ok"], false);
            assert_eq!(replies[1]["type"], "config_policy_state");
            assert_eq!(replies[1]["phase"], "failed");
            assert_eq!(replies[1]["error"], "diagnostic_mode");
        }
    }

    #[tokio::test]
    async fn corrupt_policy_journal_keeps_independent_image_removal_available() {
        let station = Station::policy_journal_corrupt();
        let manager = ImageManager::new(
            crate::session::container::ContainerRuntime::from_env(),
            String::new(),
        );
        let mut outbound = Outbound::default();
        answer(
            &mut outbound,
            &station,
            Some(&manager),
            r#"{"type":"image_remove","id":"remove-1","image_id":"absent-image"}"#,
        )
        .await
        .unwrap();
        answer(
            &mut outbound,
            &station,
            Some(&manager),
            r#"{"type":"restart","id":"restart-1"}"#,
        )
        .await
        .unwrap();
        let replies: Vec<serde_json::Value> = outbound
            .0
            .iter()
            .map(|message| serde_json::from_str(message.to_text().unwrap()).unwrap())
            .collect();
        assert_eq!(replies.len(), 2);
        assert_eq!(replies[0]["type"], "ack");
        assert_eq!(
            replies[0]["ok"], true,
            "independent image work remains available"
        );
        assert_eq!(replies[1]["ok"], false, "policy restart remains refused");
    }

    #[test]
    fn resume_needs_the_engine_and_the_retirement_in_the_same_pass() {
        assert_eq!(attempt(Ok(()), Ok(())).fault(), None);
        assert_eq!(
            attempt(Err("refused"), Ok(())).fault(),
            Some(Fault::RuntimeUnusable("refused".into())),
            "an empty journal must not read an unreachable engine as usable"
        );
        assert_eq!(
            attempt(Ok(()), Err("unknown outcome")).fault(),
            Some(Fault::CleanupUnresolved("unknown outcome".into()))
        );
        assert_eq!(
            attempt(Err("refused"), Err("unavailable")).fault(),
            Some(Fault::RuntimeUnusable("refused".into()))
        );
    }

    #[test]
    fn a_clean_first_pass_never_enters_diagnostic_mode() {
        let mut startup = Startup::begin(&attempt(Ok(()), Ok(())));
        assert_eq!(startup.phase(), &Phase::Normal);
        assert_eq!(
            startup.observe(&attempt(Err("refused"), Err("unavailable"))),
            Transition::AlreadyNormal
        );
        assert_eq!(startup.phase(), &Phase::Normal);
    }

    /// A resume is a fact about the process, not a notification: it has to survive
    /// being published while nothing is subscribed. Every `serve_registered` arm body
    /// runs with the select's `resumed()` future — and so the only watch receiver —
    /// dropped, so this is the window a real resume lands in (#269).
    #[tokio::test]
    async fn a_resume_published_with_no_waiter_is_still_seen_by_the_next_one() {
        let station = Station::enter(&attempt(Err("refused"), Err("unavailable")))
            .expect("an unresolved first pass is diagnostic mode");
        assert_eq!(
            station.observe(&attempt(Ok(()), Ok(()))),
            Transition::Resumed
        );
        tokio::time::timeout(Duration::from_secs(5), station.resumed())
            .await
            .expect("a resume published with no waiter alive was lost");
        assert_eq!(station.resumes(), 1);
    }

    #[test]
    fn startup_resumes_exactly_once_and_never_re_enters() {
        let mut startup = Startup::begin(&attempt(Err("refused"), Err("unavailable")));
        assert!(matches!(
            startup.phase(),
            Phase::Diagnostic(Fault::RuntimeUnusable(_))
        ));

        // The engine is back but the prior agent's application is still unresolved.
        assert_eq!(
            startup.observe(&attempt(Ok(()), Err("unknown outcome"))),
            Transition::StillDiagnostic
        );
        assert!(matches!(
            startup.phase(),
            Phase::Diagnostic(Fault::CleanupUnresolved(_))
        ));

        assert_eq!(
            startup.observe(&attempt(Ok(()), Ok(()))),
            Transition::Resumed
        );
        assert_eq!(startup.phase(), &Phase::Normal);

        assert_eq!(
            startup.observe(&attempt(Ok(()), Ok(()))),
            Transition::AlreadyNormal
        );
        assert_eq!(
            startup.observe(&attempt(Err("refused"), Err("unavailable"))),
            Transition::AlreadyNormal
        );
        assert_eq!(startup.phase(), &Phase::Normal);
    }
}
