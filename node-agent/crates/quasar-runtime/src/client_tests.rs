//! Engine facade specifications at the crate's public boundary.
use super::*;
use std::io::{Read, Write};
use std::os::unix::net::UnixListener;
use std::path::PathBuf;
use std::time::Duration;
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
    let runtime = RuntimeClient::new(RuntimeConfig::unix(dir.path().join("missing.sock"))).unwrap();
    assert_eq!(
        runtime.inspect_engine().wait().unwrap_err().kind,
        ErrorKind::Unavailable
    );
}

const VERSION: &str = r#"{"Platform":{"Name":"Docker Engine - Community"},"Version":"28.0.0","ApiVersion":"1.48","MinAPIVersion":"1.24"}"#;

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
        .args([
            "--exact",
            "client_tests::environment_rejects_persisted_cli_context_without_explicit_endpoint",
        ])
        .env("QUASAR_RUNTIME_CONTEXT_CHILD", "1")
        .env("DOCKER_CONFIG", dir.path())
        .env_remove("DOCKER_HOST")
        .env_remove("DOCKER_CONTEXT")
        .env_remove("DOCKER_TLS")
        .env_remove("DOCKER_TLS_VERIFY")
        .env_remove("DOCKER_API_VERSION")
        .status()
        .unwrap();
    assert!(status.success());
}
