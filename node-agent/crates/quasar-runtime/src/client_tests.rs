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
            // A client that already has what it needs may close first (BrokenPipe on a
            // busy runner); the test's own assertions on the result judge the exchange.
            let _ = write!(socket, "HTTP/1.1 {status} OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len());
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

const INFO_WITH_CDI: &str = r#"{"OperatingSystem":"Ubuntu 24.04","OSVersion":"24.04","OSType":"linux","Architecture":"x86_64","CgroupVersion":"2","SecurityOptions":["name=seccomp,profile=builtin","name=cgroupns"],"Runtimes":{"runc":{"path":"runc"},"nvidia":{"path":"nvidia-container-runtime"}},"DefaultRuntime":"runc","CDISpecDirs":["/etc/cdi","/var/run/cdi"],"DiscoveredDevices":[{"Source":"cdi","ID":"nvidia.com/gpu=0"}]}"#;

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
    assert_eq!(facts.os_version.as_deref(), Some("24.04"));
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

/// RH-07 #396: each engine and mode is identified from what the real engine reports.
/// The fixtures are captured from Docker 29 and Podman 5.8, rootful and rootless.
#[test]
fn each_engine_and_mode_is_identified_from_its_own_version_and_info() {
    let cases: [(&str, &str, &str, EngineKind, &str, EngineMode); 4] = [
        (
            "docker-rootful",
            include_str!("../testdata/engines/docker-rootful-version.json"),
            include_str!("../testdata/engines/docker-rootful-info.json"),
            EngineKind::Docker,
            "29.7.2",
            EngineMode::Rootful,
        ),
        (
            "docker-rootless",
            include_str!("../testdata/engines/docker-rootless-version.json"),
            include_str!("../testdata/engines/docker-rootless-info.json"),
            EngineKind::Docker,
            "29.8.1",
            EngineMode::Rootless,
        ),
        (
            "podman-rootful",
            include_str!("../testdata/engines/podman-rootful-version.json"),
            include_str!("../testdata/engines/podman-rootful-info.json"),
            EngineKind::Podman,
            "5.8.4",
            EngineMode::Rootful,
        ),
        (
            "podman-rootless",
            include_str!("../testdata/engines/podman-rootless-version.json"),
            include_str!("../testdata/engines/podman-rootless-info.json"),
            EngineKind::Podman,
            "5.8.4",
            EngineMode::Rootless,
        ),
    ];
    for (label, version, info, kind, engine_version, mode) in cases {
        let api = if label.starts_with("podman") {
            "1.44"
        } else {
            "1.53"
        };
        let info_path: &'static str = Box::leak(format!("/v{api}/info").into_boxed_str());
        let version: &'static str = Box::leak(version.to_string().into_boxed_str());
        let info: &'static str = Box::leak(info.to_string().into_boxed_str());
        let (_dir, runtime, server) =
            fixture(vec![("/version", 200, version), (info_path, 200, info)]);
        let facts = runtime.inspect_engine().wait().unwrap();
        assert_eq!(facts.info.kind, kind, "{label}");
        assert_eq!(facts.info.version, engine_version, "{label}");
        assert_eq!(facts.mode, mode, "{label}");
        assert_eq!(facts.cgroup_driver.as_deref(), Some("systemd"), "{label}");
        server.join().unwrap();
    }
}

/// An engine this runtime does not know is `Unknown`, never guessed to be Docker.
#[test]
fn an_unrecognised_engine_is_named_by_its_component_or_unknown() {
    let (_dir, runtime, server) = fixture(vec![(
        "/version",
        200,
        r#"{"Components":[{"Name":"Moby Engine","Version":"1"}],"Version":"1.0","ApiVersion":"1.44","MinAPIVersion":"1.24"}"#,
    )]);
    assert_eq!(runtime.discover().wait().unwrap().kind, EngineKind::Unknown);
    server.join().unwrap();
    // Today's fixture (a Docker platform name and no components) is still Docker.
    let (_dir, runtime, server) = fixture(vec![("/version", 200, VERSION)]);
    assert_eq!(runtime.discover().wait().unwrap().kind, EngineKind::Docker);
    server.join().unwrap();
}

#[test]
fn the_wire_names_of_engine_kind_and_mode_are_the_contract_vocabulary() {
    assert_eq!(EngineKind::Docker.wire(), Some("docker"));
    assert_eq!(EngineKind::Podman.wire(), Some("podman"));
    assert_eq!(EngineKind::Unknown.wire(), None);
    assert_eq!(EngineMode::Rootful.wire(), "rootful");
    assert_eq!(EngineMode::Rootless.wire(), "rootless");
}

const RESTART_ID: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

fn restart_readback(reported: &'static str) -> Result<(), RuntimeError> {
    let inspect: &'static str = Box::leak(
        format!(
            r#"{{"Id":"{RESTART_ID}","Name":"/quasar-recovery","HostConfig":{{"RestartPolicy":{{"Name":"{reported}","MaximumRetryCount":0}}}},"State":{{"Status":"running","Running":true}}}}"#
        )
        .into_boxed_str(),
    );
    let update: &'static str =
        Box::leak(format!("POST /v1.48/containers/{RESTART_ID}/update").into_boxed_str());
    let get: &'static str =
        Box::leak(format!("/v1.48/containers/{RESTART_ID}/json").into_boxed_str());
    let (_dir, runtime, server) = fixture(vec![
        ("/version", 200, VERSION),
        (update, 200, r#"{"Warnings":[]}"#),
        (get, 200, inspect),
    ]);
    let result = runtime
        .set_restart_policy(RESTART_ID, crate::platform::RestartPolicy::No)
        .wait();
    server.join().unwrap();
    result
}

/// ADR 0007 (RH-07 clarification): after every restart-policy update the runtime reads
/// the policy back, on every engine, and a mismatch fails the step.
#[test]
fn a_restart_policy_update_is_read_back() {
    assert!(restart_readback("no").is_ok());
    assert_eq!(
        restart_readback("unless-stopped").unwrap_err().kind,
        ErrorKind::Engine
    );
}

/// D10: CDI everywhere it resolves; `--gpus` only on a rootful Docker without an
/// NVIDIA CDI device; nothing on a rootless Docker without one.
#[test]
fn gpu_injection_is_decided_from_engine_facts() {
    let with_nvidia = CdiFacts {
        spec_dirs: vec!["/etc/cdi".into()],
        devices: vec![
            "nvidia.com/gpu=0 (cdi)".into(),
            "nvidia.com/gpu=all (cdi)".into(),
        ],
    };
    let empty = CdiFacts {
        spec_dirs: vec!["/etc/cdi".into()],
        devices: vec![],
    };
    use EngineKind::*;
    use EngineMode::*;
    use GpuInjection::*;
    for (kind, mode, cdi, want) in [
        (Podman, Rootless, None, Some(Cdi)),
        (Podman, Rootful, None, Some(Cdi)),
        (Docker, Rootful, Some(&with_nvidia), Some(Cdi)),
        (Docker, Rootless, Some(&with_nvidia), Some(Cdi)),
        (Docker, Rootful, Some(&empty), Some(DeviceRequest)),
        (Docker, Rootful, None, Some(DeviceRequest)),
        (Docker, Rootless, Some(&empty), None),
        (Docker, Rootless, None, None),
        (Unknown, Rootful, None, Some(DeviceRequest)),
    ] {
        assert_eq!(
            GpuInjection::for_engine(kind, mode, cdi),
            want,
            "{kind:?} {mode:?} {cdi:?}"
        );
    }
}
