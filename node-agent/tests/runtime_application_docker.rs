//! Opt-in RuntimeClient acceptance against an explicitly selected Docker daemon.
use quasar_node_agent::runtime::{
    ApplicationMount, ApplicationRequest, ErrorKind, RuntimeClient, RuntimeConfig,
};
use std::{
    io::{Read, Write},
    os::unix::net::UnixStream,
    path::{Path, PathBuf},
    time::Duration,
};

fn status(socket: &Path, api: &str, id: &str) -> u16 {
    let mut stream = UnixStream::connect(socket).unwrap();
    stream
        .set_read_timeout(Some(Duration::from_secs(5)))
        .unwrap();
    stream
        .set_write_timeout(Some(Duration::from_secs(5)))
        .unwrap();
    write!(
        stream,
        "GET /v{api}/containers/{id}/json HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n"
    )
    .unwrap();
    let mut reply = String::new();
    stream.read_to_string(&mut reply).unwrap();
    reply.split_whitespace().nth(1).unwrap().parse().unwrap()
}
fn unique() -> String {
    format!(
        "{}-{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    )
}

struct CleanupGuard {
    runtime: RuntimeClient,
    operations: Vec<String>,
    state: Option<tempfile::TempDir>,
    complete: bool,
}
impl CleanupGuard {
    fn track(&mut self, operation: String) {
        self.operations.push(operation);
    }
    fn state(&self) -> &Path {
        self.state.as_ref().unwrap().path()
    }
}
impl Drop for CleanupGuard {
    fn drop(&mut self) {
        if self.complete {
            return;
        }
        let mut unproven = false;
        for operation in &self.operations {
            if let Err(error) = self.runtime.abandon_application(operation).wait() {
                unproven = true;
                eprintln!(
                    "application acceptance cleanup pending: operation={operation}, error={error}"
                );
            }
        }
        if unproven {
            if let Some(state) = self.state.take() {
                let retained = state.keep();
                eprintln!(
                    "application acceptance state retained at {}",
                    retained.display()
                );
            }
        }
    }
}

/// Sets `NODE_SECRET_PATH` on the real process env; this binary's sole test, so it is
/// safe alone but must never run with `--include-ignored` alongside another suite that
/// shares this env var.
#[test]
#[ignore = "requires QUASAR_TEST_RUNTIME_SOCKET, QUASAR_TEST_RUNTIME_IMAGE, and QUASAR_TEST_RUNTIME_HOST_ROOT"]
fn docker_application_lifecycle() {
    let socket = PathBuf::from(
        std::env::var("QUASAR_TEST_RUNTIME_SOCKET").expect("set explicit test socket"),
    );
    let image = std::env::var("QUASAR_TEST_RUNTIME_IMAGE").expect("set existing test image");
    let daemon_root = PathBuf::from(
        std::env::var("QUASAR_TEST_RUNTIME_HOST_ROOT").expect("set daemon-host checkout path"),
    );
    let local_root = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .unwrap()
        .to_owned();
    let diagnostics = local_root.join(".diagnostics");
    std::fs::create_dir_all(&diagnostics).unwrap();
    let state = tempfile::Builder::new()
        .prefix("rh01-240-")
        .tempdir_in(&diagnostics)
        .unwrap();
    std::env::set_var("NODE_SECRET_PATH", state.path().join("node-secret"));
    let mut config = RuntimeConfig::unix(&socket);
    config.image_state_path = Some(state.path().join("operations"));
    config.deadline = Duration::from_secs(20);
    let runtime = RuntimeClient::new(config).unwrap();
    let mut cleanup = CleanupGuard {
        runtime: runtime.clone(),
        operations: Vec::new(),
        state: Some(state),
        complete: false,
    };
    let engine = runtime.discover().wait().unwrap();
    assert!(runtime.image_present(image.clone()).wait().unwrap());
    let token = unique();
    let home = cleanup.state().join("home");
    std::fs::create_dir(&home).unwrap();
    let source = daemon_root.join(home.strip_prefix(&local_root).unwrap());
    let request = |suffix: &str, command: &str| ApplicationRequest {
        operation: format!("rh01-240-{token}-{suffix}"),
        name: format!("quasar-sess-rh01-240-{token}-{suffix}"),
        image: image.clone(),
        pull_never: true,
        entrypoint: Some(vec!["/bin/sh".into()]),
        command: vec!["-c".into(), command.into()],
        typed_mounts: vec![ApplicationMount::Bind {
            source: source.to_string_lossy().into_owned(),
            target: "/home/quasar".into(),
            read_only: false,
            consistency: None,
        }],
        ..Default::default()
    };
    let success = request(
        "success",
        "printf retained >/home/quasar/save; printf final-out",
    );
    cleanup.track(success.operation.clone());
    let id = runtime.start_application(success).wait().unwrap();
    let result = runtime.observe_application(id.clone()).wait().unwrap();
    assert_eq!(result.exit_code, Some(0));
    assert!(result.stdout.contains("final-out"));
    assert_eq!(
        std::fs::read_to_string(home.join("save")).unwrap(),
        "retained"
    );
    runtime.cleanup_application(id.clone()).wait().unwrap();
    assert_eq!(
        status(&socket, &engine.api_version.to_string(), id.as_str()),
        404
    );
    let nonzero = request("nonzero", "printf final-err >&2; exit 23");
    cleanup.track(nonzero.operation.clone());
    let id = runtime.start_application(nonzero).wait().unwrap();
    let result = runtime.observe_application(id.clone()).wait().unwrap();
    assert_eq!(result.exit_code, Some(23));
    assert!(result.stderr.contains("final-err"));
    runtime.cleanup_application(id).wait().unwrap();
    let cancel = request("cancel", "sleep 2; printf observer-survived");
    cleanup.track(cancel.operation.clone());
    let id = runtime.start_application(cancel).wait().unwrap();
    let observation = runtime.observe_application(id.clone());
    observation.cancel();
    assert_eq!(observation.wait().unwrap_err().kind, ErrorKind::Cancelled);
    let result = runtime.observe_application(id.clone()).wait().unwrap();
    assert_eq!(result.exit_code, Some(0));
    assert!(result.stdout.contains("observer-survived"));
    runtime.cleanup_application(id).wait().unwrap();
    let stop = request("stop", "exec sleep 60");
    cleanup.track(stop.operation.clone());
    let id = runtime.start_application(stop).wait().unwrap();
    runtime
        .stop_application(id.clone(), Duration::from_secs(2))
        .wait()
        .unwrap();
    assert!(runtime
        .observe_application(id.clone())
        .wait()
        .unwrap()
        .exit_code
        .is_some_and(|code| code != 0));
    runtime.cleanup_application(id).wait().unwrap();
    let missing_local = local_root.join(format!(".diagnostics/rh01-240-never-create-{token}"));
    let missing = daemon_root.join(missing_local.strip_prefix(&local_root).unwrap());
    assert!(!missing_local.exists());
    let rejected = ApplicationRequest {
        operation: format!("rh01-240-{token}-missing"),
        name: format!("quasar-sess-rh01-240-{token}-missing"),
        image,
        pull_never: true,
        typed_mounts: vec![ApplicationMount::Bind {
            source: missing.to_string_lossy().into_owned(),
            target: "/missing".into(),
            read_only: true,
            consistency: None,
        }],
        ..Default::default()
    };
    cleanup.track(rejected.operation.clone());
    assert_eq!(
        runtime
            .start_application(rejected.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Engine
    );
    assert!(!missing_local.exists());
    runtime
        .abandon_application(rejected.operation)
        .wait()
        .unwrap();
    assert_eq!(
        std::fs::read_to_string(home.join("save")).unwrap(),
        "retained"
    );
    cleanup.complete = true;
    println!("Docker {} API {}: final logs, nonzero exit, observation cancellation, explicit stop, cleanup, writable-home preservation, and missing typed-bind rejection passed", engine.version, engine.api_version);
}
