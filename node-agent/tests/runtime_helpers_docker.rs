//! Opt-in acceptance against an explicitly selected local Docker daemon.
use quasar_node_agent::runtime::{
    DiagnosticHelper, DiagnosticRun, ErrorKind, ReadOnlyHostBind, RuntimeClient, RuntimeConfig,
};
use std::{
    io::{Read, Write},
    os::unix::net::UnixStream,
    path::PathBuf,
    time::Duration,
};

fn status(socket: &std::path::Path, api: &str, id: &str) -> u16 {
    let mut stream = UnixStream::connect(socket).unwrap();
    stream
        .set_read_timeout(Some(Duration::from_secs(5)))
        .unwrap();
    write!(
        stream,
        "GET /v{api}/containers/{id}/json HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n"
    )
    .unwrap();
    let mut response = String::new();
    stream.read_to_string(&mut response).unwrap();
    response.split_whitespace().nth(1).unwrap().parse().unwrap()
}

struct RecoverOnExit {
    config: RuntimeConfig,
    scratch: Option<tempfile::TempDir>,
}
impl Drop for RecoverOnExit {
    fn drop(&mut self) {
        if let Err(error) = RuntimeClient::new(self.config.clone())
            .and_then(|runtime| runtime.recover_diagnostics().wait())
        {
            if let Some(scratch) = self.scratch.take() {
                let _ = scratch.keep();
            }
            eprintln!(
                "owned diagnostic cleanup remains pending; acceptance state retained: {error}"
            );
        }
    }
}

#[test]
#[ignore = "requires explicit local Docker socket, installed image and daemon-host checkout path"]
fn docker_diagnostic_lifecycle() {
    let socket = PathBuf::from(
        std::env::var("QUASAR_TEST_RUNTIME_SOCKET").expect("set explicit test socket"),
    );
    let image = std::env::var("QUASAR_TEST_RUNTIME_IMAGE").expect("set existing image");
    let host_root = PathBuf::from(
        std::env::var("QUASAR_TEST_RUNTIME_HOST_ROOT").expect("set daemon-host checkout path"),
    );
    let local_root = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .unwrap()
        .to_owned();
    let scratch = tempfile::Builder::new()
        .prefix("helper-acceptance-")
        .tempdir_in(local_root.join(".diagnostics"))
        .unwrap();
    // This integration-test executable owns its own identity and runs one test.
    std::env::set_var("NODE_SECRET_PATH", scratch.path().join("node-secret"));
    let mut config = RuntimeConfig::unix(&socket);
    config.image_state_path = Some(scratch.path().join("operations"));
    let runtime = RuntimeClient::new(config.clone()).unwrap();
    let engine = runtime.discover().wait().unwrap();
    println!(
        "engine={} version={} api={}",
        engine.name, engine.version, engine.api_version
    );
    assert!(runtime.image_present(image.clone()).wait().unwrap());
    let shared = scratch.path().join("shared");
    std::fs::create_dir(&shared).unwrap();
    std::fs::write(shared.join("marker"), "fresh-diagnostic-marker").unwrap();
    let source = host_root.join(shared.strip_prefix(&local_root).unwrap());
    let _cleanup = RecoverOnExit {
        config: config.clone(),
        scratch: Some(scratch),
    };
    let request = |script: &str| {
        let operation = format!(
            "acceptance-{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        );
        (
            DiagnosticHelper {
                name: format!("quasar-diagnostic-{operation}"),
                operation,
                image: image.clone(),
            },
            DiagnosticRun {
                entrypoint: vec!["/bin/sh".into()],
                command: vec!["-c".into(), script.into()],
                bind: ReadOnlyHostBind {
                    source: source.clone(),
                    target: "/diagnostic".into(),
                },
            },
        )
    };
    let (helper, run) = request(
        "cat /diagnostic/marker; if touch /diagnostic/must-not-exist 2>/dev/null; then exit 9; fi",
    );
    let id = runtime.run_diagnostic(helper, run).wait().unwrap();
    let result = runtime.observe_diagnostic(id.clone()).wait().unwrap();
    assert_eq!(result.exit_code, Some(0));
    assert_eq!(result.stdout, "fresh-diagnostic-marker");
    assert!(!shared.join("must-not-exist").exists());
    assert_eq!(
        status(&socket, &engine.api_version.to_string(), id.as_str()),
        200,
        "retain container until explicit cleanup"
    );
    runtime.cleanup_diagnostic(id.clone()).wait().unwrap();
    assert_eq!(
        status(&socket, &engine.api_version.to_string(), id.as_str()),
        404
    );

    let (helper, run) = request("printf first; printf last; printf diagnostic-error >&2; exit 23");
    let id = runtime.run_diagnostic(helper, run).wait().unwrap();
    let result = runtime.observe_diagnostic(id.clone()).wait().unwrap();
    assert_eq!(result.exit_code, Some(23));
    assert_eq!(result.stdout, "firstlast");
    assert_eq!(result.stderr, "diagnostic-error");
    runtime.cleanup_diagnostic(id).wait().unwrap();

    let (helper, run) = request("sleep 2; printf survived-observer");
    let id = runtime.run_diagnostic(helper, run).wait().unwrap();
    let observation = runtime.observe_diagnostic(id.clone());
    observation.cancel();
    assert_eq!(observation.wait().unwrap_err().kind, ErrorKind::Cancelled);
    let result = runtime.observe_diagnostic(id.clone()).wait().unwrap();
    assert_eq!(result.exit_code, Some(0));
    assert_eq!(result.stdout, "survived-observer");
    runtime.cleanup_diagnostic(id).wait().unwrap();

    let (helper, run) = request("exec sleep 60");
    let id = runtime.run_diagnostic(helper, run).wait().unwrap();
    runtime.stop_diagnostic(id.clone()).wait().unwrap();
    let result = runtime.observe_diagnostic(id.clone()).wait().unwrap();
    assert!(result.exit_code.is_some_and(|code| code != 0));
    runtime.cleanup_diagnostic(id).wait().unwrap();

    let (helper, run) = request("sleep 1; printf recovered");
    let id = runtime.run_diagnostic(helper, run).wait().unwrap();
    drop(runtime);
    let recovered = RuntimeClient::new(config).unwrap();
    recovered.recover_diagnostics().wait().unwrap();
    assert_eq!(
        status(&socket, &engine.api_version.to_string(), id.as_str()),
        404
    );
    assert_eq!(
        std::fs::read_to_string(shared.join("marker")).unwrap(),
        "fresh-diagnostic-marker"
    );
    println!("readonly bind, final logs/nonzero exit, cancellation, explicit stop, restart recovery and data preservation passed");
}
