//! Fault injection through the Quasar interface, with a disposable Unix HTTP engine.
use super::*;
use std::io::{Read, Write};
use std::os::unix::net::UnixListener;
use std::time::Instant;

const VERSION: &str = r#"{"Version":"28.0.0","ApiVersion":"1.48","MinAPIVersion":"1.40"}"#;
const PRESENT: &str = r#"{"Id":"sha256:fixture","Size":100}"#;
const ABSENT: &str = r#"{"message":"No such image"}"#;
const PULL: &str = "POST /v1.48/images/create?fromImage=test&tag=latest&platform=";
const INSPECT: &str = "GET /v1.48/images/test/json";

struct Reply {
    request: String,
    status: u16,
    body: String,
    incomplete: bool,
    hold: Duration,
    auth: Option<serde_json::Value>,
}
fn reply(request: &str, status: u16, body: impl Into<String>) -> Reply {
    Reply {
        request: request.into(),
        status,
        body: body.into(),
        incomplete: false,
        hold: Duration::ZERO,
        auth: None,
    }
}
fn discovery() -> Reply {
    reply("GET /version", 200, VERSION)
}
fn serve(
    replies: Vec<Reply>,
) -> (
    tempfile::TempDir,
    RuntimeConfig,
    std::thread::JoinHandle<()>,
) {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("engine.sock");
    let listener = UnixListener::bind(&path).unwrap();
    listener.set_nonblocking(true).unwrap();
    let server = std::thread::spawn(move || {
        for reply in replies {
            let until = Instant::now() + Duration::from_secs(5);
            let mut socket = loop {
                match listener.accept() {
                    Ok((socket, _)) => break socket,
                    Err(e)
                        if e.kind() == std::io::ErrorKind::WouldBlock && Instant::now() < until =>
                    {
                        std::thread::sleep(Duration::from_millis(5))
                    }
                    Err(e) => panic!("missing {}: {e}", reply.request),
                }
            };
            socket
                .set_read_timeout(Some(Duration::from_secs(2)))
                .unwrap();
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                socket.read_exact(&mut byte).unwrap();
                request.push(byte[0]);
                assert!(request.len() < 16384);
            }
            let request = String::from_utf8(request).unwrap();
            assert_eq!(
                request.lines().next().unwrap(),
                format!("{} HTTP/1.1", reply.request)
            );
            if let Some(expected) = reply.auth {
                use base64::Engine;
                let encoded = request
                    .lines()
                    .find_map(|line| {
                        line.split_once(':')
                            .filter(|(k, _)| k.eq_ignore_ascii_case("x-registry-auth"))
                            .map(|(_, v)| v.trim())
                    })
                    .unwrap();
                let bytes = base64::engine::general_purpose::URL_SAFE
                    .decode(encoded)
                    .unwrap();
                let actual: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
                for (key, value) in expected.as_object().unwrap() {
                    assert_eq!(&actual[key], value);
                }
            }
            let length = reply.body.len() + usize::from(reply.incomplete) * 100;
            let _ = write!(socket, "HTTP/1.1 {} OK\r\nContent-Type: application/json\r\nContent-Length: {length}\r\nConnection: close\r\n\r\n{}", reply.status, reply.body);
            std::thread::sleep(reply.hold);
        }
    });
    let mut config = RuntimeConfig::unix(path);
    config.image_state_path = Some(dir.path().join("operations"));
    (dir, config, server)
}

#[test]
fn absent_managed_local_tag_fails_without_asking_docker_to_pull() {
    let tag = "quasar-local/steam-template:one";
    let inspect = format!("GET /v1.48/images/{tag}/json");
    let (_dir, config, server) = serve(vec![discovery(), reply(&inspect, 404, ABSENT)]);
    let runtime = RuntimeClient::new(config).unwrap();
    assert_eq!(
        runtime
            .ensure_image(tag, Duration::from_secs(2))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::Missing
    );
    server.join().unwrap();
}

#[test]
fn present_managed_local_tag_is_reused_without_a_pull() {
    let tag = "quasar-local/steam-template:one";
    let inspect = format!("GET /v1.48/images/{tag}/json");
    let (_dir, config, server) = serve(vec![discovery(), reply(&inspect, 200, PRESENT)]);
    let runtime = RuntimeClient::new(config).unwrap();
    assert_eq!(
        runtime
            .ensure_image(tag, Duration::from_secs(2))
            .wait(|_| {})
            .unwrap()
            .id,
        "sha256:fixture"
    );
    server.join().unwrap();
}

#[test]
fn absent_registry_digest_still_requests_a_pull() {
    let reference = "registry.example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111";
    let inspect = format!("GET /v1.48/images/{reference}/json");
    let pull = format!(
        "POST /v1.48/images/create?fromImage=registry.example%2Fapp%40sha256%3A{}&platform=",
        "1".repeat(64)
    );
    let (_dir, config, server) = serve(vec![
        discovery(),
        reply(&inspect, 404, ABSENT),
        reply(&pull, 200, ""),
        reply(&inspect, 200, PRESENT),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    assert_eq!(
        runtime
            .ensure_image(reference, Duration::from_secs(2))
            .wait(|_| {})
            .unwrap()
            .id,
        "sha256:fixture"
    );
    server.join().unwrap();
}

#[test]
fn server_failure_after_pull_submission_is_an_unknown_outcome() {
    let (_dir, config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 404, ABSENT),
        reply(PULL, 500, r#"{"message":"engine lost contact"}"#),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    assert_eq!(
        runtime
            .ensure_image("test", Duration::from_secs(2))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    server.join().unwrap();
}

#[test]
fn interrupted_pull_is_reconciled_after_client_restart_without_blind_retry() {
    let mut interrupted = reply(PULL, 200, "{\"status\":\"Downloading\"}\n");
    interrupted.incomplete = true;
    let (_dir, config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 404, ABSENT),
        interrupted,
        discovery(),
        reply(INSPECT, 404, ABSENT),
        discovery(),
        reply(INSPECT, 200, PRESENT),
    ]);
    let runtime = RuntimeClient::new(config.clone()).unwrap();
    assert_eq!(
        runtime
            .ensure_image("test", Duration::from_secs(2))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    drop(runtime);
    let runtime = RuntimeClient::new(config).unwrap();
    assert_eq!(
        runtime
            .ensure_image("test", Duration::from_secs(2))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(
        runtime
            .ensure_image("test", Duration::from_secs(2))
            .wait(|_| {})
            .unwrap()
            .bytes,
        100
    );
    server.join().unwrap();
}

#[test]
fn silent_pull_deadline_releases_capacity_without_claiming_cancellation() {
    let mut silent = reply(PULL, 200, "");
    silent.incomplete = true;
    silent.hold = Duration::from_millis(350);
    let (_dir, mut config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 404, ABSENT),
        silent,
        discovery(),
        reply(INSPECT, 404, ABSENT),
    ]);
    config.max_in_flight = 1;
    let runtime = RuntimeClient::new(config).unwrap();
    let start = Instant::now();
    assert_eq!(
        runtime
            .ensure_image("test", Duration::from_millis(150))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert!(start.elapsed() < Duration::from_secs(2));
    // A second operation gets capacity, but absence cannot prove the old pull stopped.
    assert_eq!(
        runtime
            .ensure_image("test", Duration::from_secs(2))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    server.join().unwrap();
}

#[test]
fn stream_error_after_http_success_is_terminal_and_does_not_report_ready() {
    for (message, kind) in [
        ("denied: fixture-secret", ErrorKind::RegistryDenied),
        ("manifest unknown", ErrorKind::ManifestMissing),
        ("no space left on device", ErrorKind::InsufficientDisk),
    ] {
        let body = format!(
            "{{\"errorDetail\":{{\"message\":{}}},\"error\":{}}}\n",
            serde_json::to_string(message).unwrap(),
            serde_json::to_string(message).unwrap()
        );
        let (_dir, config, server) = serve(vec![
            discovery(),
            reply(INSPECT, 404, ABSENT),
            reply(PULL, 200, body),
            discovery(),
            reply(INSPECT, 404, ABSENT),
            reply(PULL, 200, ""),
            reply(INSPECT, 200, PRESENT),
        ]);
        let runtime = RuntimeClient::new(config).unwrap();
        let error = runtime
            .ensure_image("test", Duration::from_secs(2))
            .wait(|_| {})
            .unwrap_err();
        assert_eq!(error.kind, kind);
        assert!(!error.to_string().contains("fixture-secret"));
        // A conclusive daemon error permits retry, unlike a broken transport.
        assert!(runtime
            .ensure_image("test", Duration::from_secs(2))
            .wait(|_| {})
            .is_ok());
        server.join().unwrap();
    }
}

#[test]
fn successful_stream_without_a_verified_image_is_not_ready() {
    let (_dir, config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 404, ABSENT),
        reply(PULL, 200, "{\"status\":\"done\"}\n"),
        reply(INSPECT, 404, ABSENT),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    assert_eq!(
        runtime
            .ensure_image("test", Duration::from_secs(2))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    server.join().unwrap();
}

#[test]
fn noisy_pull_coalesces_progress_when_the_observer_is_slow() {
    let mut body = String::new();
    for n in 0..10000 {
        body.push_str(&format!(
            "{{\"id\":\"layer\",\"progressDetail\":{{\"current\":{n},\"total\":10000}}}}\n"
        ));
    }
    let (_dir, config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 404, ABSENT),
        reply(PULL, 200, body),
        reply(INSPECT, 200, PRESENT),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    let operation = runtime.ensure_image("test", Duration::from_secs(4));
    // No observer drains progress while the daemon sends 10,000 records.
    server.join().unwrap();
    let mut samples = Vec::new();
    assert_eq!(
        operation.wait(|sample| samples.push(sample)).unwrap().bytes,
        100
    );
    assert!(samples.len() <= 4, "progress backlog was replayed");
    assert_eq!(samples.last().unwrap().bytes, 9999);
    assert!(samples.iter().all(|p| p.percent < 100));
}

#[test]
fn dropping_the_observer_and_client_does_not_abandon_a_pull() {
    let mut pull = reply(PULL, 200, "");
    pull.hold = Duration::from_millis(100);
    let (_dir, config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 404, ABSENT),
        pull,
        reply(INSPECT, 200, PRESENT),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    let operation = runtime.ensure_image("test", Duration::from_secs(2));
    drop(operation);
    drop(runtime);
    // Final inspection still occurs with no control-plane or observer attached.
    server.join().unwrap();
}

#[test]
fn unavailable_initial_inspection_never_triggers_pull_or_delete() {
    for remove in [false, true] {
        let (_dir, config, server) = serve(vec![
            discovery(),
            reply(INSPECT, 503, r#"{"message":"unavailable"}"#),
        ]);
        let runtime = RuntimeClient::new(config).unwrap();
        let result = if remove {
            runtime.remove_image("test", Duration::from_secs(2)).wait()
        } else {
            runtime
                .ensure_image("test", Duration::from_secs(2))
                .wait(|_| {})
                .map(|_| ())
        };
        assert_eq!(result.unwrap_err().kind, ErrorKind::Engine);
        server.join().unwrap();
    }
}

#[test]
fn removal_verifies_absence_and_never_forces_or_prunes_parents() {
    let (_dir, config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 200, PRESENT),
        reply("GET /v1.48/containers/json?all=true&size=false", 200, "[]"),
        reply(
            "DELETE /v1.48/images/test?force=false&noprune=true",
            200,
            "[]",
        ),
        reply(INSPECT, 404, ABSENT),
        discovery(),
        reply(INSPECT, 404, ABSENT),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    runtime
        .remove_image("test", Duration::from_secs(2))
        .wait()
        .unwrap();
    runtime
        .remove_image("test", Duration::from_secs(2))
        .wait()
        .unwrap();
    server.join().unwrap();
}

#[test]
fn uncertain_removal_never_retries_against_a_retagged_reference() {
    let mut interrupted = reply(
        "DELETE /v1.48/images/test?force=false&noprune=true",
        200,
        "",
    );
    interrupted.incomplete = true;
    let (_dir, config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 200, PRESENT),
        reply("GET /v1.48/containers/json?all=true&size=false", 200, "[]"),
        interrupted,
        discovery(),
        reply(INSPECT, 200, r#"{"Id":"sha256:different"}"#),
        discovery(),
        reply(INSPECT, 404, ABSENT),
    ]);
    let runtime = RuntimeClient::new(config.clone()).unwrap();
    assert_eq!(
        runtime
            .remove_image("test", Duration::from_secs(2))
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    drop(runtime);
    let runtime = RuntimeClient::new(config).unwrap();
    assert_eq!(
        runtime
            .remove_image("test", Duration::from_secs(2))
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    runtime
        .remove_image("test", Duration::from_secs(2))
        .wait()
        .unwrap();
    server.join().unwrap();
}

#[test]
fn registry_login_is_forwarded_privately_and_local_reuse_does_not_require_login() {
    let mut pull = reply(PULL, 200, "");
    pull.auth = Some(
        serde_json::json!({"username":"fixture","password":"private","serveraddress":"https://index.docker.io/v1/"}),
    );
    let (dir, mut config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 404, ABSENT),
        pull,
        reply(INSPECT, 200, PRESENT),
        discovery(),
        reply(INSPECT, 200, PRESENT),
    ]);
    let auth = dir.path().join("config.json");
    std::fs::write(
        &auth,
        r#"{"auths":{"https://index.docker.io/v1/":{"auth":"Zml4dHVyZTpwcml2YXRl"}}}"#,
    )
    .unwrap();
    config.registry_config_path = Some(auth.clone());
    let runtime = RuntimeClient::new(config).unwrap();
    runtime
        .ensure_image("test", Duration::from_secs(2))
        .wait(|_| {})
        .unwrap();
    std::fs::write(auth, "malformed").unwrap();
    runtime
        .ensure_image("test", Duration::from_secs(2))
        .wait(|_| {})
        .unwrap();
    server.join().unwrap();
}

#[test]
fn installed_credential_helper_supplies_registry_auth() {
    use std::os::unix::fs::PermissionsExt;
    if std::env::var_os("QUASAR_CREDENTIAL_FIXTURE_CHILD").is_none() {
        let directory = tempfile::tempdir().unwrap();
        let helper = directory.path().join("docker-credential-fixture");
        std::fs::write(&helper,b"#!/bin/sh\nread -r registry\n[ \"$1\" = get ] && [ \"$registry\" = https://index.docker.io/v1/ ] || exit 1\nprintf '%s' '{\"Username\":\"fixture\",\"Secret\":\"helper-private\"}'\n").unwrap();
        std::fs::set_permissions(&helper, std::fs::Permissions::from_mode(0o700)).unwrap();
        let status = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "runtime::image_tests::installed_credential_helper_supplies_registry_auth",
            ])
            .env("QUASAR_CREDENTIAL_FIXTURE_CHILD", "1")
            .env("PATH", directory.path())
            .status()
            .unwrap();
        assert!(status.success());
        return;
    }
    let mut pull = reply(PULL, 200, "");
    pull.auth = Some(serde_json::json!({"username":"fixture","password":"helper-private"}));
    let (dir, mut config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 404, ABSENT),
        pull,
        reply(INSPECT, 200, PRESENT),
    ]);
    let auth = dir.path().join("config.json");
    std::fs::write(&auth, r#"{"credsStore":"fixture"}"#).unwrap();
    config.registry_config_path = Some(auth);
    RuntimeClient::new(config)
        .unwrap()
        .ensure_image("test", Duration::from_secs(2))
        .wait(|_| {})
        .unwrap();
    server.join().unwrap();
}

#[test]
fn a_blocked_progress_observer_does_not_block_engine_completion() {
    let (_dir, config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 404, ABSENT),
        reply(
            PULL,
            200,
            "{\"id\":\"layer\",\"progressDetail\":{\"current\":50,\"total\":100}}\n",
        ),
        reply(INSPECT, 200, PRESENT),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    let operation = runtime.ensure_image("test", Duration::from_secs(3));
    let (release, resume) = std::sync::mpsc::channel();
    let observer = std::thread::spawn(move || {
        let mut first = true;
        operation.wait(|_| {
            if first {
                first = false;
                let _ = resume.recv_timeout(Duration::from_secs(2));
            }
        })
    });
    let (finished, done) = std::sync::mpsc::channel();
    let checker = std::thread::spawn(move || {
        server.join().unwrap();
        let _ = finished.send(());
    });
    let completed = done.recv_timeout(Duration::from_secs(1)).is_ok();
    let _ = release.send(());
    assert!(observer.join().unwrap().is_ok());
    checker.join().unwrap();
    assert!(completed, "observer held the runtime progress lock");
}

#[test]
fn cancelling_session_observation_does_not_cancel_the_image_mutation() {
    let (_dir, config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 404, ABSENT),
        reply(PULL, 200, ""),
        reply(INSPECT, 200, PRESENT),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    assert_eq!(
        runtime
            .ensure_image("test", Duration::from_secs(2))
            .wait_with_cancel(|_| {}, || true)
            .unwrap_err()
            .kind,
        ErrorKind::Cancelled
    );
    server.join().unwrap();
}

#[test]
fn cancellation_during_preparation_completion_wins_over_a_ready_result() {
    use std::sync::atomic::{AtomicBool, Ordering};
    let (_dir, config, server) = serve(vec![
        discovery(),
        reply(INSPECT, 404, ABSENT),
        reply(PULL, 200, ""),
        reply(INSPECT, 200, PRESENT),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    let stop = AtomicBool::new(false);
    let operation = runtime.ensure_image("test", Duration::from_secs(2));
    let result = operation.wait_with_cancel(
        |_| stop.store(true, Ordering::SeqCst),
        || stop.load(Ordering::SeqCst),
    );
    assert_eq!(result.unwrap_err().kind, ErrorKind::Cancelled);
    server.join().unwrap();
}
