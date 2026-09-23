//! Quasar-owned engine discovery and bounded image operations.

use std::{
    path::PathBuf,
    sync::{mpsc, Arc},
    time::Duration,
};
use tokio::sync::{watch, Semaphore};

mod application;
mod builds;
mod docker;
mod helpers;
pub use application::{
    ApplicationId, ApplicationLogTail, ApplicationMount, ApplicationRequest, ApplicationResult,
    ApplicationSecurity,
};
pub use builds::BuildRequest;
pub use helpers::{
    AudioRun, DiagnosticDevices, DiagnosticHelper, DiagnosticNetwork, DiagnosticRequirements,
    DiagnosticRun, DiagnosticSecurity, GpuProbeRun, HelperResult, NvidiaDriverAccess,
    NvidiaDriverMount, OwnedHelperId, ReadOnlyHostBind,
};
pub(crate) use helpers::{HelperIntent, HelperJournal, NvidiaGpuRun};
mod images;
pub use images::{ImageInfo, ImageOperation, ImageProgress};
mod inspection;
pub use inspection::{
    agent_path_for_daemon_path, daemon_path_for_agent_path, ContainerInspection, DaemonHostPath,
    EngineStorage, ImageMetadata, Mount, MountKind,
};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ErrorKind {
    InvalidConfiguration,
    PermissionDenied,
    Missing,
    Unavailable,
    IncompatibleApi,
    Protocol,
    Engine,
    Timeout,
    Cancelled,
    Busy,
    UnknownOutcome,
    ImageInUse,
    RegistryDenied,
    ManifestMissing,
    InsufficientDisk,
    InvalidBuildContext,
    BuildFailed,
}

/// Safe to surface to callers. Raw daemon messages never become public errors.
#[derive(Debug, Clone)]
pub struct RuntimeError {
    pub kind: ErrorKind,
    /// A sanitized read-only observation that explains why a mutation outcome
    /// remains unknown. No daemon text or SDK type crosses this boundary.
    pub reconciliation: Option<ErrorKind>,
}
impl std::fmt::Display for RuntimeError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "container runtime: {:?}", self.kind)?;
        if let Some(reconciliation) = self.reconciliation {
            write!(f, " (reconciliation: {:?})", reconciliation)?;
        }
        Ok(())
    }
}
impl std::error::Error for RuntimeError {}
impl From<ErrorKind> for RuntimeError {
    fn from(kind: ErrorKind) -> Self {
        Self {
            kind,
            reconciliation: None,
        }
    }
}

/// How long one read-only engine inspection may take before the agent calls the engine
/// unreachable (#274). It is deliberately far below the client deadline: the readiness
/// refresh runs every [`crate::agent::READINESS_REFRESH_INTERVAL`] and the control plane
/// abstains from a readiness verdict once a report is older than
/// `QUASAR_READINESS_STALE_SECS` (default 60 s), so a hung daemon — one whose socket
/// accepts the connection and then never answers — has to be visible in a report inside
/// that window, not merely "eventually".
pub const ENGINE_INSPECTION_BUDGET: Duration = Duration::from_secs(5);

#[derive(Debug, Clone)]
pub struct RuntimeConfig {
    pub socket: PathBuf,
    pub deadline: Duration,
    pub max_in_flight: usize,
    pub image_state_path: Option<PathBuf>,
    pub registry_config_path: Option<PathBuf>,
    /// Test-only injection keeps HTTP fixtures independent of process-global
    /// ownership state; production always resolves the persisted owner lease.
    #[cfg(test)]
    pub diagnostic_owner: Option<String>,
}
impl RuntimeConfig {
    pub fn unix(socket: impl Into<PathBuf>) -> Self {
        Self {
            socket: socket.into(),
            deadline: Duration::from_secs(30),
            max_in_flight: 4,
            image_state_path: None,
            registry_config_path: None,
            #[cfg(test)]
            diagnostic_owner: None,
        }
    }

    /// Resolve the one explicit Unix endpoint this agent talks to. Context, TLS
    /// and API-version overrides are refused rather than silently ignored.
    pub fn from_environment() -> Result<Self, RuntimeError> {
        for key in [
            "DOCKER_CONTEXT",
            "DOCKER_TLS",
            "DOCKER_TLS_VERIFY",
            "DOCKER_API_VERSION",
        ] {
            if std::env::var_os(key).is_some_and(|v| !v.is_empty()) {
                return Err(ErrorKind::InvalidConfiguration.into());
            }
        }
        // Retired by #239: the agent no longer runs an engine CLI, so there is
        // nothing left for this knob to select. Say so once rather than failing
        // an upgraded host whose .env still carries it.
        if std::env::var_os("QUASAR_CONTAINER_RUNTIME").is_some() {
            tracing::warn!(
                token = "runtime-cli-knob-retired",
                "QUASAR_CONTAINER_RUNTIME is ignored: the agent no longer runs an engine CLI; \
                 select the engine with DOCKER_HOST (unix://)"
            );
        }
        match std::env::var("DOCKER_HOST") {
            Ok(host) if !host.is_empty() => Self::from_endpoint(&host),
            Ok(_) | Err(std::env::VarError::NotPresent) => {
                // The Docker CLI remembers `docker context use` in its config.
                // An operator who switched context expects it to be honoured;
                // this agent cannot honour it, so refuse rather than silently
                // talking to a different engine than the one they selected.
                let directory = std::env::var_os("DOCKER_CONFIG")
                    .filter(|v| !v.is_empty())
                    .map(PathBuf::from)
                    .or_else(|| {
                        std::env::var_os("HOME").map(|home| PathBuf::from(home).join(".docker"))
                    })
                    .ok_or(ErrorKind::InvalidConfiguration)?;
                match std::fs::read(directory.join("config.json")) {
                    Ok(bytes) => {
                        #[derive(serde::Deserialize)]
                        struct Context {
                            #[serde(default, rename = "currentContext")]
                            current: String,
                        }
                        let context: Context = serde_json::from_slice(&bytes)
                            .map_err(|_| ErrorKind::InvalidConfiguration)?;
                        if !context.current.is_empty() && context.current != "default" {
                            return Err(ErrorKind::InvalidConfiguration.into());
                        }
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                    Err(error) if error.kind() == std::io::ErrorKind::PermissionDenied => {
                        return Err(ErrorKind::PermissionDenied.into())
                    }
                    Err(_) => return Err(ErrorKind::InvalidConfiguration.into()),
                }
                Ok(Self::unix("/var/run/docker.sock"))
            }
            Err(_) => Err(ErrorKind::InvalidConfiguration.into()),
        }
    }

    pub fn from_endpoint(endpoint: &str) -> Result<Self, RuntimeError> {
        let path = endpoint
            .strip_prefix("unix://")
            .ok_or(ErrorKind::InvalidConfiguration)?;
        if !std::path::Path::new(path).is_absolute() || path.contains(['?', '#', '\0']) {
            return Err(ErrorKind::InvalidConfiguration.into());
        }
        Ok(Self::unix(path))
    }
}

/// One executor per agent process, shared across the existing runtime facades.
static IMAGE_STATE_PATH: std::sync::OnceLock<PathBuf> = std::sync::OnceLock::new();
pub(crate) fn initialize_image_state(path: PathBuf) {
    let _ = IMAGE_STATE_PATH.set(path);
}

pub(crate) fn configured() -> Result<&'static RuntimeClient, RuntimeError> {
    static CLIENT: std::sync::OnceLock<Result<RuntimeClient, RuntimeError>> =
        std::sync::OnceLock::new();
    CLIENT
        .get_or_init(|| {
            let mut config = RuntimeConfig::from_environment()?;
            config.image_state_path = IMAGE_STATE_PATH.get().cloned().or_else(|| {
                Some(std::path::PathBuf::from(format!(
                    "{}.runtime-images",
                    crate::container_ownership::standalone_secret_path()
                )))
            });
            config.registry_config_path = std::env::var_os("DOCKER_CONFIG")
                .filter(|v| !v.is_empty())
                .map(PathBuf::from)
                .or_else(|| std::env::var_os("HOME").map(|v| PathBuf::from(v).join(".docker")))
                .map(|directory| directory.join("config.json"));
            RuntimeClient::new(config)
        })
        .as_ref()
        .map_err(Clone::clone)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub struct ApiVersion {
    pub major: usize,
    pub minor: usize,
}
impl std::fmt::Display for ApiVersion {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}.{}", self.major, self.minor)
    }
}

/// The lowest engine API this agent speaks, and the single place it is written down
/// (#266): discovery refuses anything below it and the `runtime_api_version` readiness
/// wording renders from it, so the two can never quote different numbers. Higher
/// capability floors must be established by the caller migrations that need them.
pub const API_FLOOR: ApiVersion = ApiVersion {
    major: 1,
    minor: 40,
};

/// What one boot-only legacy sweep did (see
/// [`RuntimeClient::retire_legacy_containers`]). `preserved` counts containers
/// this agent could not prove it owns and therefore left alone — a foreign
/// owner, an unrelated name, an API-owned application, an audio sidecar.
/// `unresolved` counts owned containers whose removal this pass could not
/// prove; the next boot retries them.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct LegacyRetirement {
    pub removed: usize,
    pub preserved: usize,
    pub unresolved: usize,
}

/// Discovery facts are not a claim that GPU/rootless capabilities were tested.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EngineInfo {
    pub name: String,
    pub version: String,
    pub api_version: ApiVersion,
    pub server_min_api: ApiVersion,
    pub server_max_api: ApiVersion,
}

/// What one engine inspection reports beyond [`EngineInfo`]: the engine's own statements
/// about itself, observed and passed through. None of it is a claim that a capability was
/// exercised (#254).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EngineFacts {
    pub info: EngineInfo,
    pub operating_system: Option<String>,
    pub architecture: Option<String>,
    pub cgroup_version: Option<String>,
    pub security_options: Vec<String>,
    /// Configured OCI runtimes by name, sorted.
    pub runtimes: Vec<String>,
    pub default_runtime: Option<String>,
    /// `None` when the engine does not report CDI at all (pre-CDI API).
    pub cdi: Option<CdiFacts>,
}

/// The engine's CDI statement. An empty `spec_dirs` means CDI injection is disabled.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CdiFacts {
    pub spec_dirs: Vec<String>,
    /// `"<id> (<source>)"` per discovered device.
    pub devices: Vec<String>,
}

// Own the executor independently of any control-plane connection. Background
// shutdown is safe even when the last caller lives on another Tokio executor.
struct Executor {
    runtime: Option<tokio::runtime::Runtime>,
    slots: Arc<Semaphore>,
}
impl Drop for Executor {
    fn drop(&mut self) {
        if let Some(runtime) = self.runtime.take() {
            runtime.shutdown_background();
        }
    }
}

/// A bounded operation. Dropping or cancelling reads stops them; image mutations
/// continue under their own deadline and retain uncertainty when interrupted.
/// `wait` bridges existing blocking callers; it must not be used on a media path.
pub struct Operation<T> {
    result: mpsc::Receiver<Result<T, RuntimeError>>,
    cancel: watch::Sender<bool>,
    _executor: Arc<Executor>,
}
impl<T> Operation<T> {
    pub fn cancel(&self) {
        let _ = self.cancel.send(true);
    }
    pub fn wait(self) -> Result<T, RuntimeError> {
        self.result
            .recv()
            .unwrap_or_else(|_| Err(ErrorKind::Unavailable.into()))
    }

    /// Stop waiting when a host probe's deadline passes or a launch pre-empts it, then
    /// keep waiting for the executor's own answer: dropping an observation must never
    /// stop, remove or roll back anything, and the race the cancellation lost still
    /// carries the real result.
    pub fn wait_with_cancel(self, mut cancelled: impl FnMut() -> bool) -> Result<T, RuntimeError> {
        let mut sent = false;
        loop {
            if !sent && cancelled() {
                self.cancel();
                sent = true;
            }
            match self.result.recv_timeout(Duration::from_millis(50)) {
                Ok(result) => return result,
                Err(mpsc::RecvTimeoutError::Timeout) => {}
                Err(mpsc::RecvTimeoutError::Disconnected) => {
                    return Err(ErrorKind::Unavailable.into())
                }
            }
        }
    }
}
impl<T> Drop for Operation<T> {
    fn drop(&mut self) {
        self.cancel();
    }
}

#[derive(Clone)]
pub struct RuntimeClient {
    config: RuntimeConfig,
    executor: Arc<Executor>,
}
impl RuntimeClient {
    pub fn new(config: RuntimeConfig) -> Result<Self, RuntimeError> {
        if !config.socket.is_absolute()
            || config.socket.to_str().is_none()
            || config.deadline.is_zero()
            || config.max_in_flight == 0
            || config.max_in_flight > Semaphore::MAX_PERMITS
            || std::time::Instant::now()
                .checked_add(config.deadline)
                .is_none()
        {
            return Err(ErrorKind::InvalidConfiguration.into());
        }
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(1)
            .thread_name("quasar-runtime")
            .enable_all()
            .build()
            .map_err(|_| RuntimeError::from(ErrorKind::Unavailable))?;
        let slots = Arc::new(Semaphore::new(config.max_in_flight));
        Ok(Self {
            config,
            executor: Arc::new(Executor {
                runtime: Some(runtime),
                slots,
            }),
        })
    }

    fn submit<T: Send + 'static>(
        &self,
        work: impl std::future::Future<Output = Result<T, RuntimeError>> + Send + 'static,
    ) -> Operation<T> {
        self.submit_owned(work, self.config.deadline, false)
    }

    fn submit_owned<T: Send + 'static>(
        &self,
        work: impl std::future::Future<Output = Result<T, RuntimeError>> + Send + 'static,
        budget: Duration,
        detached: bool,
    ) -> Operation<T> {
        let (send, result) = mpsc::sync_channel(1);
        let (cancel, mut cancelled) = watch::channel(false);
        let operation = Operation {
            result,
            cancel,
            _executor: self.executor.clone(),
        };
        if budget.is_zero() || std::time::Instant::now().checked_add(budget).is_none() {
            let _ = send.send(Err(ErrorKind::InvalidConfiguration.into()));
            return operation;
        }
        match self.executor.slots.clone().try_acquire_owned() {
            Err(_) => {
                let _ = send.send(Err(ErrorKind::Busy.into()));
            }
            Ok(permit) => {
                let deadline = tokio::time::Instant::now() + budget;
                let owner = detached.then(|| self.executor.clone());
                self.executor
                    .runtime
                    .as_ref()
                    .expect("live executor")
                    .spawn(async move {
                        let _permit = permit;
                        let _owner = owner;
                        let result = tokio::select! {
                            biased;
                            _ = cancelled.changed(), if !detached => Err(ErrorKind::Cancelled.into()),
                            result = tokio::time::timeout_at(deadline, work) =>
                                result.unwrap_or_else(|_| Err(if detached { ErrorKind::UnknownOutcome } else { ErrorKind::Timeout }.into())),
                        };
                        let _ = send.send(result);
                    });
            }
        }
        operation
    }

    pub fn image_present(&self, image: impl Into<String>) -> Operation<bool> {
        let config = self.config.clone();
        let image = image.into();
        self.submit(async move { docker::inspect_image(&config, &image).await })
    }

    pub fn discover(&self) -> Operation<EngineInfo> {
        let config = self.config.clone();
        self.submit(async move { docker::discover(&config).await.map(|(_, info)| info) })
    }

    /// Read one container's daemon-authoritative configuration. `Ok(None)` is
    /// only a conclusively missing container; inaccessible engines are errors.
    pub fn inspect_container(
        &self,
        id: impl Into<String>,
    ) -> Operation<Option<ContainerInspection>> {
        let config = self.config.clone();
        let id = id.into();
        self.submit(async move { docker::inspect_container(&config, &id).await })
    }

    /// [`Self::inspect_container`] under a caller-chosen budget. The readiness refresh uses
    /// it with [`ENGINE_INSPECTION_BUDGET`]: a refresh that straddles a daemon freeze has
    /// already proved the engine answers, so it does not skip its remaining collectors —
    /// and one full client deadline inside a refresh is a whole staleness window (#274).
    pub fn inspect_container_within(
        &self,
        id: impl Into<String>,
        budget: Duration,
    ) -> Operation<Option<ContainerInspection>> {
        let config = self.config.clone();
        let id = id.into();
        self.submit_owned(
            async move { docker::inspect_container(&config, &id).await },
            std::cmp::min(self.config.deadline, budget),
            false,
        )
    }

    /// Snapshot every live container, including containers Quasar does not own.
    /// Every listed ID is re-inspected so an incomplete liveness fact fails closed.
    pub fn live_containers(&self) -> Operation<Vec<ContainerInspection>> {
        let config = self.config.clone();
        self.submit(async move { docker::live_containers(&config).await })
    }

    /// Filesystem location in the container engine daemon's host namespace.
    pub fn engine_storage(&self) -> Operation<EngineStorage> {
        let config = self.config.clone();
        self.submit(async move { docker::engine_storage(&config).await })
    }

    /// [`Self::engine_storage`] under a caller-chosen budget; see
    /// [`Self::inspect_container_within`] for why the readiness path needs one.
    pub fn engine_storage_within(&self, budget: Duration) -> Operation<EngineStorage> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::engine_storage(&config).await },
            std::cmp::min(self.config.deadline, budget),
            false,
        )
    }

    /// One bounded, read-only inspection of the engine: discovery plus `/info`, folded
    /// into [`EngineFacts`]. Budgeted at [`ENGINE_INSPECTION_BUDGET`] so a readiness
    /// refresh on a wedged daemon reports it inside the control plane's staleness window
    /// instead of spending the whole refresh window here (#274).
    pub fn inspect_engine(&self) -> Operation<EngineFacts> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::inspect_engine(&config).await },
            std::cmp::min(self.config.deadline, ENGINE_INSPECTION_BUDGET),
            false,
        )
    }

    /// This client's default per-operation deadline, for callers choosing between it and
    /// an explicit budget.
    pub fn deadline(&self) -> Duration {
        self.config.deadline
    }

    /// The endpoint this client speaks to, for readiness wording.
    pub fn endpoint(&self) -> String {
        format!("unix://{}", self.config.socket.display())
    }

    /// Image identity and its baked environment. `Ok(None)` is a missing image.
    pub fn inspect_image_metadata(
        &self,
        image: impl Into<String>,
    ) -> Operation<Option<ImageMetadata>> {
        let config = self.config.clone();
        let image = image.into();
        self.submit(async move { docker::inspect_image_metadata(&config, &image).await })
    }

    /// Create and start one session application under a durable, owned
    /// operation. Repeated calls reconcile the same operation; they never
    /// create a second container after an uncertain response.
    pub fn start_application(&self, request: ApplicationRequest) -> Operation<ApplicationId> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::application::start(&config, request).await },
            self.config.deadline,
            true,
        )
    }

    /// Observe an application exit and collect its final retained logs. Dropping
    /// this observer only cancels observation; explicit stop owns termination.
    pub fn observe_application(&self, id: ApplicationId) -> Operation<ApplicationResult> {
        let config = self.config.clone();
        self.submit(async move { docker::application::observe(&config, id).await })
    }

    /// Read a bounded current application log tail for readiness diagnostics.
    /// This is observation only: cancellation and transport failure cannot
    /// request a stop or alter the durable lifecycle intent.
    pub fn application_log_tail(&self, id: ApplicationId) -> Operation<ApplicationLogTail> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::application::log_tail(&config, id).await },
            std::cmp::min(self.config.deadline, Duration::from_secs(2)),
            false,
        )
    }

    pub fn stop_application(&self, id: ApplicationId, timeout: Duration) -> Operation<()> {
        let config = self.config.clone();
        let seconds = timeout.as_secs().min(i32::MAX as u64) as i32;
        self.submit_owned(
            async move { docker::application::stop(&config, id, seconds).await },
            self.config.deadline,
            true,
        )
    }

    /// Persist final exit and log evidence before deleting the owned container.
    pub fn cleanup_application(&self, id: ApplicationId) -> Operation<()> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::application::cleanup(&config, id).await },
            self.config.deadline,
            true,
        )
    }

    /// Retry durable terminal application cleanup at startup or a safe
    /// maintenance tick. Running applications are never adopted or stopped.
    pub fn recover_application_cleanup(&self) -> Operation<()> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::application::recover_cleanup(&config).await },
            self.config.deadline.saturating_mul(4),
            true,
        )
    }

    /// Boot-only retirement of application records from a prior agent. This
    /// is an explicit teardown policy, never adoption of a running session.
    pub fn retire_applications(&self) -> Operation<()> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::application::retire(&config).await },
            self.config.deadline.saturating_mul(4),
            true,
        )
    }

    /// Boot-only retirement of LEGACY containers — pre-API siblings an older
    /// agent shell-launched, which carry this agent's owner label and an
    /// allowed name prefix but no operation journal. Each candidate is
    /// re-inspected by its immutable ID before any removal; anything not proven
    /// owned is preserved and counted. This is a teardown policy, never
    /// adoption: nothing here observes or resumes a prior session.
    pub fn retire_legacy_containers(&self, prefixes: Vec<String>) -> Operation<LegacyRetirement> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::legacy::retire_legacy(&config, &prefixes).await },
            self.config.deadline.saturating_mul(4),
            true,
        )
    }

    /// Persist abandonment for a launch whose caller lost its returned handle.
    /// It reconciles only this stable operation and never discovers by prefix.
    pub fn abandon_application(&self, operation: impl Into<String>) -> Operation<()> {
        let config = self.config.clone();
        let operation = operation.into();
        self.submit_owned(
            async move { docker::application::abandon(&config, &operation).await },
            self.config.deadline,
            true,
        )
    }

    /// Create and explicitly start one owned diagnostic helper. Dropping the
    /// returned operation detaches its observer; it never stops the helper.
    pub fn run_diagnostic(
        &self,
        helper: DiagnosticHelper,
        run: DiagnosticRun,
    ) -> Operation<OwnedHelperId> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::helpers::run(&config, helper, run).await },
            self.config.deadline,
            true,
        )
    }

    /// Create and explicitly start one owned GPU probe container (#258): the
    /// closed profile that gives a probe what a session's application container
    /// gets for GPU access, for every vendor. Refused before any engine request
    /// when a requirement is unsupported, when the name is outside the probe
    /// prefix, or while an earlier probe of this kind is unreconciled (`Busy`).
    /// Dropping the returned operation detaches its observer; it never stops
    /// the probe. The orchestrator owns the deadline: past it, `stop_gpu_probe`
    /// then `cleanup_gpu_probe`.
    pub fn run_gpu_probe(
        &self,
        helper: DiagnosticHelper,
        run: GpuProbeRun,
    ) -> Operation<OwnedHelperId> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::helpers::run_gpu_probe(&config, helper, run).await },
            self.config.deadline,
            true,
        )
    }

    /// Wait for a probe and collect its final bounded logs. A timeout here is a
    /// timeout of the observation, never the probe's outcome; cancellation only
    /// stops observing.
    pub fn observe_gpu_probe(&self, id: OwnedHelperId) -> Operation<HelperResult> {
        self.observe_diagnostic(id)
    }

    /// Explicit, durable termination of one owned probe.
    pub fn stop_gpu_probe(&self, id: OwnedHelperId) -> Operation<()> {
        self.stop_diagnostic(id)
    }

    /// Preserve exit and log evidence, then remove the owned probe container.
    pub fn cleanup_gpu_probe(&self, id: OwnedHelperId) -> Operation<()> {
        self.cleanup_diagnostic(id)
    }

    /// Boot-only: finish every probe a previous agent process left behind,
    /// stopping one still running — its deadline died with that process — and
    /// completing tracked cleanup. Routine recovery (`recover_diagnostics`)
    /// covers probe journals too but never stops unrequested work.
    pub fn retire_gpu_probes(&self) -> Operation<()> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::helpers::retire_gpu_probes(&config).await },
            self.config.deadline,
            true,
        )
    }

    /// Start a fixed-profile PulseAudio sibling.  The returned identity is
    /// available while the daemon runs; observing it never terminates it.
    pub fn run_audio_sidecar(
        &self,
        helper: DiagnosticHelper,
        run: AudioRun,
    ) -> Operation<OwnedHelperId> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::helpers::run_audio(&config, helper, run).await },
            self.config.deadline,
            true,
        )
    }

    pub fn observe_audio_sidecar(&self, id: OwnedHelperId) -> Operation<HelperResult> {
        self.observe_diagnostic(id)
    }

    pub fn stop_audio_sidecar(&self, id: OwnedHelperId) -> Operation<()> {
        self.stop_diagnostic(id)
    }

    pub fn cleanup_audio_sidecar(&self, id: OwnedHelperId) -> Operation<()> {
        self.cleanup_diagnostic(id)
    }

    /// Audio is intentionally excluded from routine diagnostic recovery:
    /// startup migration owns explicit audio recovery and termination policy.
    pub fn recover_audio_sidecars(&self) -> Operation<()> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::helpers::recover_audio(&config).await },
            self.config.deadline,
            true,
        )
    }

    /// Boot-only retirement of journals left by a prior agent process.
    pub fn retire_audio_sidecars(&self) -> Operation<()> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::helpers::retire_audio(&config).await },
            self.config.deadline,
            true,
        )
    }

    /// Persist explicit abandonment of an audio launch whose start result was
    /// lost, then reconcile and remove only that recorded operation.
    pub fn abandon_audio_sidecar(&self, operation: impl Into<String>) -> Operation<()> {
        let config = self.config.clone();
        let operation = operation.into();
        self.submit_owned(
            async move { docker::helpers::abandon_audio(&config, &operation).await },
            self.config.deadline,
            true,
        )
    }

    /// Wait for a helper and collect its final bounded logs. Cancellation only
    /// stops this observation; `stop_diagnostic` owns termination.
    pub fn observe_diagnostic(&self, id: OwnedHelperId) -> Operation<HelperResult> {
        let config = self.config.clone();
        self.submit(async move { docker::helpers::observe(&config, id).await })
    }

    pub fn stop_diagnostic(&self, id: OwnedHelperId) -> Operation<()> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::helpers::stop(&config, id).await },
            self.config.deadline,
            true,
        )
    }

    /// Preserve final logs and exit evidence before removing the owned
    /// container. This operation never force-removes containers or volumes.
    pub fn cleanup_diagnostic(&self, id: OwnedHelperId) -> Operation<()> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::helpers::cleanup(&config, id).await },
            self.config.deadline,
            true,
        )
    }

    /// Recover durable helpers after agent restart. Unresolved records remain
    /// on disk and the returned error blocks a fresh probe.
    pub fn recover_diagnostics(&self) -> Operation<()> {
        let config = self.config.clone();
        self.submit_owned(
            async move { docker::helpers::recover(&config).await },
            self.config.deadline,
            true,
        )
    }
}

#[cfg(test)]
mod application_tests;
#[cfg(test)]
mod inspection_tests;

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::os::unix::net::UnixListener;

    fn fixture(
        responses: Vec<(&'static str, u16, &'static str)>,
    ) -> (
        tempfile::TempDir,
        RuntimeClient,
        std::thread::JoinHandle<()>,
    ) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("engine.sock");
        let listener = UnixListener::bind(&path).unwrap();
        let thread = std::thread::spawn(move || {
            for (expected_path, status, body) in responses {
                let (mut socket, _) = listener.accept().unwrap();
                socket
                    .set_read_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                let mut request = Vec::new();
                while !request.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    socket.read_exact(&mut byte).unwrap();
                    request.push(byte[0]);
                }
                let body_length = String::from_utf8_lossy(&request)
                    .lines()
                    .find_map(|line| {
                        line.split_once(':')
                            .filter(|(name, _)| name.eq_ignore_ascii_case("content-length"))
                            .and_then(|(_, value)| value.trim().parse::<usize>().ok())
                    })
                    .unwrap_or(0);
                if body_length > 0 {
                    let mut body = vec![0; body_length];
                    socket.read_exact(&mut body).unwrap();
                }
                let expected =
                    if expected_path.starts_with("POST ") || expected_path.starts_with("DELETE ") {
                        expected_path.to_owned()
                    } else {
                        format!("GET {expected_path}")
                    };
                assert!(
                    String::from_utf8_lossy(&request).starts_with(&format!("{expected} HTTP/1.1")),
                    "{}",
                    String::from_utf8_lossy(&request)
                );
                write!(socket, "HTTP/1.1 {status} OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
            }
        });
        let mut config = RuntimeConfig::unix(path);
        config.image_state_path = Some(dir.path().join("operations"));
        let client = RuntimeClient::new(config).unwrap();
        (dir, client, thread)
    }

    #[test]
    fn discovery_reports_negotiated_engine_identity() {
        let (_dir, runtime, server) = fixture(vec![(
            "/version",
            200,
            r#"{"Platform":{"Name":"Docker Engine - Community"},"Version":"28.0.0","ApiVersion":"1.48","MinAPIVersion":"1.24"}"#,
        )]);
        let info = runtime.discover().wait().unwrap();
        assert_eq!(info.name, "Docker Engine - Community");
        assert_eq!(info.version, "28.0.0");
        assert_eq!(info.api_version.to_string(), "1.48");
        server.join().unwrap();
    }

    const INFO_WITH_CDI: &str = r#"{"OperatingSystem":"Ubuntu 24.04","OSType":"linux","Architecture":"x86_64","CgroupVersion":"2","SecurityOptions":["name=seccomp,profile=builtin","name=cgroupns"],"Runtimes":{"runc":{"path":"runc"},"nvidia":{"path":"nvidia-container-runtime"}},"DefaultRuntime":"runc","CDISpecDirs":["/etc/cdi","/var/run/cdi"],"DiscoveredDevices":[{"Source":"cdi","ID":"nvidia.com/gpu=0"}]}"#;

    /// #254: one inspection carries the negotiated identity, the engine's stated
    /// capabilities and its CDI facts. Nothing is mutated: two GETs, no more.
    #[test]
    fn engine_inspection_reports_identity_capabilities_and_cdi() {
        let (_dir, runtime, server) = fixture(vec![
            ("/version", 200, VERSION),
            ("/v1.48/info", 200, INFO_WITH_CDI),
        ]);
        let facts = runtime.inspect_engine().wait().unwrap();
        assert_eq!(facts.info.version, "28.0.0");
        assert_eq!(facts.info.api_version.to_string(), "1.48");
        assert_eq!(facts.operating_system.as_deref(), Some("Ubuntu 24.04"));
        assert_eq!(facts.architecture.as_deref(), Some("x86_64"));
        assert_eq!(facts.cgroup_version.as_deref(), Some("2"));
        assert_eq!(
            facts.security_options,
            vec!["name=seccomp,profile=builtin", "name=cgroupns"]
        );
        // Sorted, so the wording is stable across engines.
        assert_eq!(facts.runtimes, vec!["nvidia", "runc"]);
        assert_eq!(facts.default_runtime.as_deref(), Some("runc"));
        let cdi = facts.cdi.expect("engine reported CDI");
        assert_eq!(cdi.spec_dirs, vec!["/etc/cdi", "/var/run/cdi"]);
        assert_eq!(cdi.devices, vec!["nvidia.com/gpu=0 (cdi)"]);
        server.join().unwrap();
    }

    #[test]
    fn engine_inspection_distinguishes_cdi_disabled_from_cdi_unreported() {
        let (_dir, runtime, server) = fixture(vec![
            ("/version", 200, VERSION),
            (
                "/v1.48/info",
                200,
                r#"{"OperatingSystem":"Ubuntu 24.04","CDISpecDirs":[]}"#,
            ),
        ]);
        let facts = runtime.inspect_engine().wait().unwrap();
        assert_eq!(
            facts.cdi,
            Some(CdiFacts {
                spec_dirs: vec![],
                devices: vec![]
            })
        );
        assert!(facts.runtimes.is_empty());
        server.join().unwrap();

        let (_dir, runtime, server) = fixture(vec![
            ("/version", 200, VERSION),
            ("/v1.48/info", 200, r#"{"OperatingSystem":"Ubuntu 20.04"}"#),
        ]);
        let facts = runtime.inspect_engine().wait().unwrap();
        assert_eq!(facts.cdi, None);
        server.join().unwrap();
    }

    /// A silent engine is a timeout, bounded by the client's deadline; the readiness
    /// layer reads that as unreachable.
    #[test]
    fn engine_inspection_of_a_silent_engine_times_out_within_the_budget() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("silent.sock");
        let _listener = UnixListener::bind(&path).unwrap();
        let mut config = RuntimeConfig::unix(path);
        config.deadline = Duration::from_millis(200);
        let runtime = RuntimeClient::new(config).unwrap();
        let started = std::time::Instant::now();
        let error = runtime.inspect_engine().wait().unwrap_err();
        assert_eq!(error.kind, ErrorKind::Timeout);
        assert!(started.elapsed() < Duration::from_secs(5));
    }

    #[test]
    fn engine_inspection_of_a_missing_socket_is_unavailable() {
        let dir = tempfile::tempdir().unwrap();
        let runtime =
            RuntimeClient::new(RuntimeConfig::unix(dir.path().join("missing.sock"))).unwrap();
        assert_eq!(
            runtime.inspect_engine().wait().unwrap_err().kind,
            ErrorKind::Unavailable
        );
    }

    const VERSION: &str = r#"{"Platform":{"Name":"Docker Engine - Community"},"Version":"28.0.0","ApiVersion":"1.48","MinAPIVersion":"1.24"}"#;

    #[test]
    fn ensure_image_reuses_a_local_image_without_a_registry_request() {
        let (dir, client, server) = fixture(vec![
            ("/version", 200, VERSION),
            (
                "/v1.48/images/test/json",
                200,
                r#"{"Id":"sha256:abc","Size":123}"#,
            ),
        ]);
        let mut config = client.config.clone();
        config.image_state_path = Some(dir.path().join("operations"));
        let runtime = RuntimeClient::new(config).unwrap();
        let image = runtime
            .ensure_image("test", Duration::from_secs(2))
            .wait(|_| {})
            .unwrap();
        assert_eq!(image.id, "sha256:abc");
        assert_eq!(image.bytes, 123);
        server.join().unwrap();
    }

    #[test]
    fn a_pull_verifies_the_image_after_consuming_progress() {
        let (dir, client, server) = fixture(vec![
            ("/version", 200, VERSION),
            ("/v1.48/images/test/json", 404, r#"{"message":"No such image"}"#),
            ("POST /v1.48/images/create?fromImage=test&tag=latest&platform=", 200,
             "{\"id\":\"layer\",\"status\":\"Downloading\",\"progressDetail\":{\"current\":50,\"total\":100}}\n"),
            ("/v1.48/images/test/json", 200, r#"{"Id":"sha256:new","Size":100}"#),
        ]);
        let mut config = client.config.clone();
        config.image_state_path = Some(dir.path().join("operations"));
        let runtime = RuntimeClient::new(config).unwrap();
        assert_eq!(
            runtime
                .ensure_image("test", Duration::from_secs(2))
                .wait(|_| {})
                .unwrap()
                .id,
            "sha256:new"
        );
        server.join().unwrap();
    }

    #[test]
    fn removal_refuses_an_image_referenced_by_a_container() {
        let (dir, client, server) = fixture(vec![
            ("/version", 200, VERSION),
            (
                "/v1.48/images/test/json",
                200,
                r#"{"Id":"sha256:abc","Size":123}"#,
            ),
            (
                "/v1.48/containers/json?all=true&size=false",
                200,
                r#"[{"ImageID":"sha256:abc"}]"#,
            ),
        ]);
        let mut config = client.config.clone();
        config.image_state_path = Some(dir.path().join("operations"));
        let runtime = RuntimeClient::new(config).unwrap();
        assert_eq!(
            runtime
                .remove_image("test", Duration::from_secs(2))
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::ImageInUse
        );
        server.join().unwrap();
    }

    #[test]
    fn image_inspection_uses_negotiated_version_and_rejects_invalid_metadata() {
        let (_dir, runtime, server) = fixture(vec![
            ("/version", 200, VERSION),
            ("/v1.48/images/test/json", 200, "{}"),
        ]);
        let error = runtime.image_present("test").wait().unwrap_err();
        assert_eq!(error.kind, ErrorKind::Protocol);
        server.join().unwrap();
    }

    #[test]
    fn unavailable_engine_is_not_a_missing_image() {
        let dir = tempfile::tempdir().unwrap();
        let runtime =
            RuntimeClient::new(RuntimeConfig::unix(dir.path().join("missing.sock"))).unwrap();
        let error = runtime
            .image_present("quasar-test:missing")
            .wait()
            .unwrap_err();
        assert_eq!(error.kind, ErrorKind::Unavailable);
    }

    #[test]
    fn image_presence_distinguishes_absence_from_engine_failures() {
        for (status, body, expected) in [
            (200, r#"{"Id":"sha256:abc"}"#, Ok(true)),
            (404, r#"{"message":"No such image"}"#, Ok(false)),
            (
                403,
                r#"{"message":"private daemon detail"}"#,
                Err(ErrorKind::PermissionDenied),
            ),
            (
                500,
                r#"{"message":"private daemon detail"}"#,
                Err(ErrorKind::Engine),
            ),
        ] {
            let (_dir, runtime, server) = fixture(vec![
                ("/version", 200, VERSION),
                ("/v1.48/images/test/json", status, body),
            ]);
            let result = runtime.image_present("test").wait();
            if let Err(error) = &result {
                assert!(!error.to_string().contains("private daemon detail"));
            }
            assert_eq!(result.map_err(|e| e.kind), expected);
            server.join().unwrap();
        }
    }

    #[test]
    fn incompatible_or_malformed_discovery_never_becomes_image_absence() {
        for (body, kind) in [
            (
                r#"{"Version":"old","ApiVersion":"1.39","MinAPIVersion":"1.24"}"#,
                ErrorKind::IncompatibleApi,
            ),
            (
                r#"{"Version":"future","ApiVersion":"1.60","MinAPIVersion":"1.54"}"#,
                ErrorKind::IncompatibleApi,
            ),
            (
                r#"{"Version":"bad","ApiVersion":"1.48","MinAPIVersion":"1.50"}"#,
                ErrorKind::Protocol,
            ),
            (
                r#"{"Version":"bad","ApiVersion":"garbage","MinAPIVersion":"1.24"}"#,
                ErrorKind::Protocol,
            ),
            (r#"{"Version":"unknown"}"#, ErrorKind::Protocol),
        ] {
            let (_dir, runtime, server) = fixture(vec![("/version", 200, body)]);
            assert_eq!(runtime.image_present("test").wait().unwrap_err().kind, kind);
            server.join().unwrap();
        }
        let (_dir, runtime, server) =
            fixture(vec![("/version", 404, r#"{"message":"not an engine"}"#)]);
        assert_eq!(
            runtime.image_present("test").wait().unwrap_err().kind,
            ErrorKind::Engine
        );
        server.join().unwrap();
    }

    #[test]
    fn silent_engine_operations_are_bounded_and_cancellable() {
        for cancel in [false, true] {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join("silent.sock");
            let listener = UnixListener::bind(&path).unwrap();
            let mut config = RuntimeConfig::unix(path);
            config.deadline = Duration::from_millis(100);
            config.max_in_flight = 1;
            let runtime = RuntimeClient::new(config).unwrap();
            let first = runtime.discover();
            assert_eq!(runtime.discover().wait().unwrap_err().kind, ErrorKind::Busy);
            if cancel {
                first.cancel();
            }
            let error = first.wait().unwrap_err();
            assert_eq!(
                error.kind,
                if cancel {
                    ErrorKind::Cancelled
                } else {
                    ErrorKind::Timeout
                }
            );
            drop(listener);
            // Cancellation must release admission as well as the caller's wait.
            let until = std::time::Instant::now() + Duration::from_secs(2);
            loop {
                let kind = runtime.discover().wait().unwrap_err().kind;
                if kind != ErrorKind::Busy {
                    assert_eq!(kind, ErrorKind::Unavailable);
                    break;
                }
                assert!(std::time::Instant::now() < until, "operation slot leaked");
                std::thread::yield_now();
            }
        }
    }

    #[test]
    fn endpoint_configuration_does_not_select_other_transports() {
        for endpoint in [
            "tcp://localhost:2375",
            "ssh://example",
            "unix://relative",
            "unix:///tmp/socket?other",
        ] {
            assert_eq!(
                RuntimeConfig::from_endpoint(endpoint).unwrap_err().kind,
                ErrorKind::InvalidConfiguration
            );
        }
        assert_eq!(
            RuntimeConfig::from_endpoint("unix:///tmp/explicit.sock")
                .unwrap()
                .socket,
            PathBuf::from("/tmp/explicit.sock")
        );
    }

    #[test]
    fn environment_rejects_persisted_cli_context_without_explicit_endpoint() {
        if std::env::var_os("QUASAR_RUNTIME_CONTEXT_CHILD").is_some() {
            assert_eq!(
                RuntimeConfig::from_environment().unwrap_err().kind,
                ErrorKind::InvalidConfiguration
            );
            return;
        }
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(
            dir.path().join("config.json"),
            r#"{"currentContext":"another-engine"}"#,
        )
        .unwrap();
        let status = std::process::Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "runtime::tests::environment_rejects_persisted_cli_context_without_explicit_endpoint"])
            .env("QUASAR_RUNTIME_CONTEXT_CHILD", "1").env("DOCKER_CONFIG", dir.path())
            .env_remove("DOCKER_HOST").env_remove("DOCKER_CONTEXT")
            .env_remove("DOCKER_TLS")
            .env_remove("DOCKER_TLS_VERIFY").env_remove("DOCKER_API_VERSION")
            .status().unwrap();
        assert!(status.success());
    }

    #[test]
    fn socket_permission_denial_is_distinct_from_an_unavailable_engine() {
        use std::os::unix::{fs::PermissionsExt, process::CommandExt};
        if let Some(socket) = std::env::var_os("QUASAR_RUNTIME_PERMISSION_SOCKET") {
            let runtime = RuntimeClient::new(RuntimeConfig::unix(PathBuf::from(socket))).unwrap();
            assert_eq!(
                runtime.image_present("test").wait().unwrap_err().kind,
                ErrorKind::PermissionDenied
            );
            return;
        }
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("denied.sock");
        let _listener = UnixListener::bind(&socket).unwrap();
        std::fs::set_permissions(dir.path(), std::fs::Permissions::from_mode(0o755)).unwrap();
        std::fs::set_permissions(&socket, std::fs::Permissions::from_mode(0o000)).unwrap();
        let mut command = std::process::Command::new(std::env::current_exe().unwrap());
        command
            .args([
                "--exact",
                "runtime::tests::socket_permission_denial_is_distinct_from_an_unavailable_engine",
            ])
            .env("QUASAR_RUNTIME_PERMISSION_SOCKET", &socket);
        // The dev image runs as root, which bypasses ordinary socket modes.
        if unsafe { libc::geteuid() } == 0 {
            command.uid(65534).gid(65534);
        }
        assert!(command.status().unwrap().success());
    }

    #[test]
    #[ignore = "requires an explicitly selected Docker socket and existing image; read-only"]
    fn real_docker_discovery_and_image_inspection() {
        let socket = std::env::var("QUASAR_TEST_RUNTIME_SOCKET").expect("set explicit test socket");
        let image =
            std::env::var("QUASAR_TEST_RUNTIME_IMAGE").expect("set an existing image reference");
        let runtime = RuntimeClient::new(RuntimeConfig::unix(socket)).unwrap();
        let info = runtime.discover().wait().unwrap();
        println!(
            "engine={} version={} api={} server_min={} server_max={}",
            info.name, info.version, info.api_version, info.server_min_api, info.server_max_api
        );
        assert!(runtime.image_present(image).wait().unwrap());
        let absent = format!(
            "quasar-runtime-absent-{}:test",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        );
        assert!(!runtime.image_present(absent).wait().unwrap());
    }
}

#[cfg(test)]
mod image_tests;

#[cfg(test)]
mod build_tests;

#[cfg(test)]
mod helper_tests;
