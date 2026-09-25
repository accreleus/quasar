//! Quasar-owned engine discovery and bounded image operations.
//!
//! The engine facade itself — discovery and its refusals, the bounded client,
//! error classification, credentials, read-only inspection — is the shared
//! `quasar-runtime` crate (#355). This module re-exports it under the paths the
//! agent has always used and adds the agent's own lifecycles on top: session
//! applications, owned helpers and probes, image pulls, removals and builds.

use std::{path::PathBuf, time::Duration};
use tokio::sync::watch;

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
pub use docker::ExactRemoval;
pub use images::{ImageInfo, ImageOperation, ImageProgress};
pub use quasar_runtime::{
    agent_path_for_daemon_path, daemon_path_for_agent_path, ApiVersion, CdiFacts,
    ContainerInspection, DaemonHostPath, DaemonImage, EngineFacts, EngineInfo, EngineStorage,
    ErrorKind, ImageMetadata, Mount, MountKind, Operation, RuntimeConfig, RuntimeError, API_FLOOR,
    ENGINE_INSPECTION_BUDGET,
};

/// How long ONE owned GPU probe may take end to end — recover, create and start, wait,
/// stop, remove — before [`RuntimeClient::gpu_probe_within`] stops waiting on the engine
/// and reports that it learned nothing (#283).
///
/// Sized from what a HEALTHY probe costs, not from what a wedged daemon may take. The
/// probe container self-limits (`timeout 20s` is its entrypoint), and every other step is
/// a single engine round-trip, so ~21 s is the theoretical ceiling of a good run and
/// hardware measures a few seconds. 30 s therefore cannot turn a healthy probe
/// indeterminate, and it replaces a worst case of five operations each on the client's
/// full deadline — 150 s at the default 30 s — with one bound.
///
/// Today the only production caller is the session launch gate (#259 moved the sibling
/// probe off the readiness refresh and onto `application_gpu_probe_gpu<N>`), so the bound
/// is what a launch pays on a wedged engine. It is also sized to fit a refresh should the
/// probe ever return there: 30 s on top of the five [`ENGINE_INSPECTION_BUDGET`] reads
/// #274 left on that path is 55 s, under the agent's 60 s `READINESS_REFRESH_DEADLINE`.
pub const GPU_PROBE_LIFECYCLE_BUDGET: Duration = Duration::from_secs(30);

/// Resolve the agent's engine endpoint: the shared crate's refusals and endpoint
/// resolution, with the agent's own retired-knob notice between them.
fn config_from_environment() -> Result<RuntimeConfig, RuntimeError> {
    RuntimeConfig::refuse_engine_selectors()?;
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
    RuntimeConfig::resolve_endpoint()
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
            let mut config = config_from_environment()?;
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

/// The shared engine client plus the agent's own lifecycles. Every read-only
/// operation of [`quasar_runtime::RuntimeClient`] is reached through `Deref`.
#[derive(Clone)]
pub struct RuntimeClient(quasar_runtime::RuntimeClient);
impl std::ops::Deref for RuntimeClient {
    type Target = quasar_runtime::RuntimeClient;
    fn deref(&self) -> &Self::Target {
        &self.0
    }
}
impl RuntimeClient {
    pub fn new(config: RuntimeConfig) -> Result<Self, RuntimeError> {
        quasar_runtime::RuntimeClient::new(config).map(Self)
    }

    pub fn image_present(&self, image: impl Into<String>) -> Operation<bool> {
        let config = self.config().clone();
        let image = image.into();
        self.submit(async move { docker::inspect_image(&config, &image).await })
    }

    pub fn image_inventory_snapshot(&self) -> Operation<(Vec<DaemonImage>, Vec<String>)> {
        let config = self.config().clone();
        self.submit(async move { docker::image_inventory_snapshot(&config).await })
    }

    /// Create and start one session application under a durable, owned
    /// operation. Repeated calls reconcile the same operation; they never
    /// create a second container after an uncertain response.
    pub fn start_application(&self, request: ApplicationRequest) -> Operation<ApplicationId> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::application::start(&config, request).await },
            self.config().deadline,
            true,
        )
    }

    /// Observe an application exit and collect its final retained logs. Dropping
    /// this observer only cancels observation; explicit stop owns termination.
    pub fn observe_application(&self, id: ApplicationId) -> Operation<ApplicationResult> {
        let config = self.config().clone();
        self.submit(async move { docker::application::observe(&config, id).await })
    }

    /// Read a bounded current application log tail for readiness diagnostics.
    /// This is observation only: cancellation and transport failure cannot
    /// request a stop or alter the durable lifecycle intent.
    pub fn application_log_tail(&self, id: ApplicationId) -> Operation<ApplicationLogTail> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::application::log_tail(&config, id).await },
            std::cmp::min(self.config().deadline, Duration::from_secs(2)),
            false,
        )
    }

    pub fn stop_application(&self, id: ApplicationId, timeout: Duration) -> Operation<()> {
        let config = self.config().clone();
        let seconds = timeout.as_secs().min(i32::MAX as u64) as i32;
        self.submit_owned(
            async move { docker::application::stop(&config, id, seconds).await },
            self.config().deadline,
            true,
        )
    }

    /// Persist final exit and log evidence before deleting the owned container.
    pub fn cleanup_application(&self, id: ApplicationId) -> Operation<()> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::application::cleanup(&config, id).await },
            self.config().deadline,
            true,
        )
    }

    /// Retry durable terminal application cleanup at startup or a safe
    /// maintenance tick. Running applications are never adopted or stopped.
    pub fn recover_application_cleanup(&self) -> Operation<()> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::application::recover_cleanup(&config).await },
            self.config().deadline.saturating_mul(4),
            true,
        )
    }

    /// Boot-only retirement of application records from a prior agent. This
    /// is an explicit teardown policy, never adoption of a running session.
    pub fn retire_applications(&self) -> Operation<()> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::application::retire(&config).await },
            self.config().deadline.saturating_mul(4),
            true,
        )
    }

    /// Prove that every API-owned source generation for one historical session
    /// is absent before a cleanup-qualified terminal report. Other sessions
    /// remain untouched. A malformed journal makes the proof uncertain.
    pub fn retire_session_applications(&self, session_id: impl Into<String>) -> Operation<()> {
        let config = self.config().clone();
        let session_id = session_id.into();
        self.submit_owned(
            async move { docker::application::retire_session(&config, &session_id).await },
            self.config().deadline.saturating_mul(4),
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
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::legacy::retire_legacy(&config, &prefixes).await },
            self.config().deadline.saturating_mul(4),
            true,
        )
    }

    /// Persist abandonment for a launch whose caller lost its returned handle.
    /// It reconciles only this stable operation and never discovers by prefix.
    pub fn abandon_application(&self, operation: impl Into<String>) -> Operation<()> {
        let config = self.config().clone();
        let operation = operation.into();
        self.submit_owned(
            async move { docker::application::abandon(&config, &operation).await },
            self.config().deadline,
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
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::run(&config, helper, run).await },
            self.config().deadline,
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
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::run_gpu_probe(&config, helper, run).await },
            self.config().deadline,
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

    /// One owned GPU probe end to end — recover, create and start, wait, then stop and
    /// remove — under a single wall-clock `budget` (#283).
    ///
    /// Each of those five steps used to be submitted on its own, so each carried the
    /// client's full per-operation deadline and a wedged daemon could be paid for five
    /// times over (measured: one full deadline for the create whose reply never comes,
    /// another for the wait on a container that never exits). Callers on a deadline —
    /// the readiness refresh, the launch gate — need the LIFECYCLE bounded, not each
    /// request, so the budget is spent once and apportioned here. (Since #259 the
    /// sibling probe runs from the launch gate, not the refresh; the bound serves both.)
    ///
    /// The four engine round-trips get [`ENGINE_INSPECTION_BUDGET`] or whatever is left,
    /// whichever is smaller; they are one request and one reply each and cost
    /// milliseconds on a healthy engine. The observation gets everything that remains,
    /// because it is the only step with real work behind it: it waits for the probe
    /// container to exit.
    ///
    /// A spent budget is [`ErrorKind::Timeout`] — a timeout of the observation, never a
    /// verdict about the probe. Teardown past the budget is left undone deliberately:
    /// the helper journal already records this probe, so the next probe's
    /// `recover_diagnostics` reconciles it, which is the same path a lost mutation reply
    /// takes. Nothing is force-removed and no unowned container is touched.
    pub fn gpu_probe_within(
        &self,
        helper: DiagnosticHelper,
        run: GpuProbeRun,
        budget: Duration,
    ) -> Result<HelperResult, RuntimeError> {
        let Some(deadline) = std::time::Instant::now().checked_add(budget) else {
            return Err(ErrorKind::InvalidConfiguration.into());
        };
        let left = move || deadline.saturating_duration_since(std::time::Instant::now());
        // One engine round-trip's share, or `None` once the budget is spent. Never more
        // than the client's own deadline: a caller may not buy time the client will not
        // wait for.
        let deadline_cap = self.config().deadline;
        let hop = move || {
            let left = left();
            (!left.is_zero())
                .then(|| std::cmp::min(left, ENGINE_INSPECTION_BUDGET))
                .map(|share| std::cmp::min(share, deadline_cap))
                .filter(|share| !share.is_zero())
        };

        let spent = || RuntimeError::from(ErrorKind::Timeout);
        self.recover_diagnostics_within(hop().ok_or_else(spent)?)
            .wait()?;
        let id = self
            .run_gpu_probe_within(helper, run, hop().ok_or_else(spent)?)
            .wait()?;
        let watch = std::cmp::min(left(), deadline_cap);
        let observed = if watch.is_zero() {
            Err(spent())
        } else {
            self.observe_gpu_probe_within(id.clone(), watch).wait()
        };
        if observed.is_err() {
            if let Some(share) = hop() {
                let _ = self.stop_gpu_probe_within(id.clone(), share).wait();
            }
        }
        let cleanup = match hop() {
            Some(share) => self.cleanup_gpu_probe_within(id, share).wait(),
            None => Err(spent()),
        };
        observed.and_then(|value| cleanup.map(|()| value))
    }

    /// [`Self::recover_diagnostics`] under a caller-chosen budget; see
    /// [`Self::gpu_probe_within`], the only caller, for why the lifecycle needs one.
    fn recover_diagnostics_within(&self, budget: Duration) -> Operation<()> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::recover(&config).await },
            std::cmp::min(self.config().deadline, budget),
            true,
        )
    }

    /// [`Self::run_gpu_probe`] under a caller-chosen budget.
    fn run_gpu_probe_within(
        &self,
        helper: DiagnosticHelper,
        run: GpuProbeRun,
        budget: Duration,
    ) -> Operation<OwnedHelperId> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::run_gpu_probe(&config, helper, run).await },
            std::cmp::min(self.config().deadline, budget),
            true,
        )
    }

    /// [`Self::observe_gpu_probe`] under a caller-chosen budget. A timeout here is a
    /// timeout of the observation, never the probe's outcome.
    fn observe_gpu_probe_within(
        &self,
        id: OwnedHelperId,
        budget: Duration,
    ) -> Operation<HelperResult> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::observe(&config, id).await },
            std::cmp::min(self.config().deadline, budget),
            false,
        )
    }

    /// [`Self::stop_gpu_probe`] under a caller-chosen budget.
    fn stop_gpu_probe_within(&self, id: OwnedHelperId, budget: Duration) -> Operation<()> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::stop(&config, id).await },
            std::cmp::min(self.config().deadline, budget),
            true,
        )
    }

    /// [`Self::cleanup_gpu_probe`] under a caller-chosen budget.
    fn cleanup_gpu_probe_within(&self, id: OwnedHelperId, budget: Duration) -> Operation<()> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::cleanup(&config, id).await },
            std::cmp::min(self.config().deadline, budget),
            true,
        )
    }

    /// Boot-only: finish every probe a previous agent process left behind,
    /// stopping one still running — its deadline died with that process — and
    /// completing tracked cleanup. Routine recovery (`recover_diagnostics`)
    /// covers probe journals too but never stops unrequested work.
    pub fn retire_gpu_probes(&self) -> Operation<()> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::retire_gpu_probes(&config).await },
            self.config().deadline,
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
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::run_audio(&config, helper, run).await },
            self.config().deadline,
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
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::recover_audio(&config).await },
            self.config().deadline,
            true,
        )
    }

    /// Boot-only retirement of journals left by a prior agent process.
    pub fn retire_audio_sidecars(&self) -> Operation<()> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::retire_audio(&config).await },
            self.config().deadline,
            true,
        )
    }

    /// Persist explicit abandonment of an audio launch whose start result was
    /// lost, then reconcile and remove only that recorded operation.
    pub fn abandon_audio_sidecar(&self, operation: impl Into<String>) -> Operation<()> {
        let config = self.config().clone();
        let operation = operation.into();
        self.submit_owned(
            async move { docker::helpers::abandon_audio(&config, &operation).await },
            self.config().deadline,
            true,
        )
    }

    /// Wait for a helper and collect its final bounded logs. Cancellation only
    /// stops this observation; `stop_diagnostic` owns termination.
    pub fn observe_diagnostic(&self, id: OwnedHelperId) -> Operation<HelperResult> {
        let config = self.config().clone();
        self.submit(async move { docker::helpers::observe(&config, id).await })
    }

    pub fn stop_diagnostic(&self, id: OwnedHelperId) -> Operation<()> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::stop(&config, id).await },
            self.config().deadline,
            true,
        )
    }

    /// Preserve final logs and exit evidence before removing the owned
    /// container. This operation never force-removes containers or volumes.
    pub fn cleanup_diagnostic(&self, id: OwnedHelperId) -> Operation<()> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::cleanup(&config, id).await },
            self.config().deadline,
            true,
        )
    }

    /// Recover durable helpers after agent restart. Unresolved records remain
    /// on disk and the returned error blocks a fresh probe.
    pub fn recover_diagnostics(&self) -> Operation<()> {
        let config = self.config().clone();
        self.submit_owned(
            async move { docker::helpers::recover(&config).await },
            self.config().deadline,
            true,
        )
    }
}

#[cfg(test)]
mod application_tests;

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
        let mut config = client.config().clone();
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
        let mut config = client.config().clone();
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
        let mut config = client.config().clone();
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

    /// The agent's own resolution (`configured()` calls it, not the crate's
    /// `from_environment`) keeps both refusals: an explicit engine selector, and a
    /// persisted CLI context with no explicit endpoint.
    #[test]
    fn agent_environment_rejects_selectors_and_persisted_cli_context() {
        if std::env::var_os("QUASAR_RUNTIME_CONTEXT_CHILD").is_some() {
            assert_eq!(
                config_from_environment().unwrap_err().kind,
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
        let no_context = tempfile::tempdir().unwrap();
        let child = |config: &std::path::Path, selector: Option<(&str, &str)>| {
            let mut command = std::process::Command::new(std::env::current_exe().unwrap());
            command
                .args([
                    "--exact",
                    "runtime::tests::agent_environment_rejects_selectors_and_persisted_cli_context",
                ])
                .env("QUASAR_RUNTIME_CONTEXT_CHILD", "1")
                .env("DOCKER_CONFIG", config)
                .env_remove("DOCKER_HOST")
                .env_remove("DOCKER_CONTEXT")
                .env_remove("DOCKER_TLS")
                .env_remove("DOCKER_TLS_VERIFY")
                .env_remove("DOCKER_API_VERSION");
            if let Some((key, value)) = selector {
                command.env(key, value);
            }
            command.status().unwrap().success()
        };
        assert!(
            child(dir.path(), None),
            "a persisted CLI context must be refused"
        );
        // No persisted context here, so only the selector refusal can fail resolution.
        assert!(
            child(
                no_context.path(),
                Some(("DOCKER_CONTEXT", "another-engine"))
            ),
            "an explicit engine selector must be refused"
        );
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
