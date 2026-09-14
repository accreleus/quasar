//! Quasar-owned engine discovery and bounded image operations.

use std::{
    path::PathBuf,
    sync::{mpsc, Arc},
    time::Duration,
};
use tokio::sync::{watch, Semaphore};

mod builds;
mod docker;
mod helpers;
pub use builds::BuildRequest;
pub use helpers::{
    DiagnosticDevices, DiagnosticHelper, DiagnosticNetwork, DiagnosticRequirements, DiagnosticRun,
    DiagnosticSecurity, HelperResult, OwnedHelperId, ReadOnlyHostBind,
};
pub(crate) use helpers::{HelperIntent, HelperJournal};
mod images;
pub use images::{ImageInfo, ImageOperation, ImageProgress};

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

    /// Use the same explicit Unix endpoint as the still-unmigrated Docker CLI.
    /// Context/TLS/version overrides are not silently ignored.
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
        let cli = std::env::var("QUASAR_CONTAINER_RUNTIME").unwrap_or_else(|_| "docker".into());
        if std::path::Path::new(&cli)
            .file_name()
            .is_none_or(|v| v != "docker")
        {
            return Err(ErrorKind::InvalidConfiguration.into());
        }
        match std::env::var("DOCKER_HOST") {
            Ok(host) if !host.is_empty() => Self::from_endpoint(&host),
            Ok(_) | Err(std::env::VarError::NotPresent) => {
                // Docker CLI also remembers `docker context use` in its config.
                // Without an explicit endpoint, that could split API reads from
                // still-unmigrated CLI writes onto two different engines.
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
            config.image_state_path = IMAGE_STATE_PATH.get().cloned();
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

/// Discovery facts are not a claim that GPU/rootless capabilities were tested.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EngineInfo {
    pub name: String,
    pub version: String,
    pub api_version: ApiVersion,
    pub server_min_api: ApiVersion,
    pub server_max_api: ApiVersion,
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
            .env("QUASAR_CONTAINER_RUNTIME", "docker")
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
