//! Behavioral inspection tests through the public RuntimeClient boundary.
use super::*;
use std::io::{Read, Write};
use std::os::unix::net::UnixListener;

const VERSION: &str = r#"{"Version":"28.0.0","ApiVersion":"1.48","MinAPIVersion":"1.40"}"#;

fn fixture(
    replies: Vec<(&str, u16, &str)>,
) -> (
    tempfile::TempDir,
    RuntimeClient,
    std::thread::JoinHandle<()>,
) {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("engine.sock");
    let listener = UnixListener::bind(&path).unwrap();
    listener.set_nonblocking(true).unwrap();
    let replies: Vec<_> = replies
        .into_iter()
        .map(|(a, b, c)| (a.to_owned(), b, c.to_owned()))
        .collect();
    let server = std::thread::spawn(move || {
        for (expected, status, body) in replies {
            let deadline = std::time::Instant::now() + Duration::from_secs(2);
            let (mut socket, _) = loop {
                match listener.accept() {
                    Ok(socket) => break socket,
                    Err(error)
                        if error.kind() == std::io::ErrorKind::WouldBlock
                            && std::time::Instant::now() < deadline =>
                    {
                        std::thread::sleep(Duration::from_millis(5))
                    }
                    Err(error) => panic!("missing expected request {expected}: {error}"),
                }
            };
            socket
                .set_read_timeout(Some(Duration::from_secs(2)))
                .unwrap();
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                let mut b = [0];
                socket.read_exact(&mut b).unwrap();
                request.push(b[0]);
                assert!(request.len() <= 16 * 1024, "oversized HTTP header");
            }
            assert_eq!(
                String::from_utf8_lossy(&request).lines().next().unwrap(),
                format!("{expected} HTTP/1.1")
            );
            write!(socket, "HTTP/1.1 {status} OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
        }
    });
    let client = RuntimeClient::new(RuntimeConfig::unix(path)).unwrap();
    (dir, client, server)
}
fn discovery() -> (&'static str, u16, &'static str) {
    ("GET /version", 200, VERSION)
}

#[test]
fn container_metadata_preserves_missing_compose_labels_and_structured_mounts() {
    let (_dir, runtime, server) = fixture(vec![
        discovery(),
        (
            "GET /v1.48/containers/app/json",
            200,
            r#"{"Id":"app","Image":"sha256:image","Config":{"Image":"example/app:1","Labels":{"other":"kept"}},"HostConfig":{"NetworkMode":"bridge"},"Mounts":[{"Type":"bind","Source":"/srv/home","Destination":"/home/app","RW":false},{"Type":"volume","Name":"state","Source":"/var/lib/docker/volumes/state/_data","Destination":"/state","RW":true}]}"#,
        ),
    ]);
    let info = runtime.inspect_container("app").wait().unwrap().unwrap();
    assert!(!info.labels.contains_key("com.docker.compose.project"));
    assert_eq!(
        info.mounts[0].source,
        Some(DaemonHostPath("/srv/home".into()))
    );
    assert_eq!(info.mounts[1].kind, MountKind::Volume);
    assert_eq!(info.mounts[1].name.as_deref(), Some("state"));
    assert_eq!(info.mounts[0].read_only, Some(true));
    assert_eq!(info.network_mode.as_deref(), Some("bridge"));
    server.join().unwrap();
}

#[test]
fn missing_container_is_distinct_from_denied_or_unavailable_engine() {
    let (_dir, runtime, server) = fixture(vec![
        discovery(),
        (
            "GET /v1.48/containers/gone/json",
            404,
            r#"{"message":"gone"}"#,
        ),
    ]);
    assert_eq!(runtime.inspect_container("gone").wait().unwrap(), None);
    server.join().unwrap();
}

#[test]
fn denied_or_unavailable_inspection_is_not_a_missing_container() {
    let (_dir, runtime, server) = fixture(vec![
        discovery(),
        ("GET /v1.48/containers/app/json", 403, r#"{}"#),
    ]);
    assert_eq!(
        runtime.inspect_container("app").wait().unwrap_err().kind,
        ErrorKind::PermissionDenied
    );
    server.join().unwrap();
}

#[test]
fn live_snapshot_includes_foreign_running_mounts_and_filters_stopped() {
    let (_dir, runtime, server) = fixture(vec![
        discovery(),
        (
            "GET /v1.48/containers/json?all=true&size=false",
            200,
            r#"[{"Id":"foreign"},{"Id":"stopped"}]"#,
        ),
        (
            "GET /v1.48/containers/foreign/json",
            200,
            r#"{"Id":"foreign","Image":"sha256:f","Config":{"Image":"busybox","Labels":{"owner":"someone-else"}},"HostConfig":{"NetworkMode":"host"},"Mounts":[{"Type":"bind","Source":"/homes/alice","Destination":"/data","RW":true}],"State":{"Running":true,"Paused":false,"Restarting":false}}"#,
        ),
        (
            "GET /v1.48/containers/stopped/json",
            200,
            r#"{"Id":"stopped","Image":"sha256:s","Config":{"Image":"busybox"},"HostConfig":{},"Mounts":[],"State":{"Running":false,"Paused":false,"Restarting":false}}"#,
        ),
    ]);
    let containers = runtime.live_containers().wait().unwrap();
    assert_eq!(containers.len(), 1);
    assert_eq!(containers[0].labels["owner"], "someone-else");
    server.join().unwrap();
}

#[test]
fn malformed_liveness_or_list_inspect_race_fails_closed() {
    let (_dir, runtime, server) = fixture(vec![
        discovery(),
        (
            "GET /v1.48/containers/json?all=true&size=false",
            200,
            r#"[{"Id":"racing"}]"#,
        ),
        ("GET /v1.48/containers/racing/json", 404, r#"{}"#),
    ]);
    assert_eq!(
        runtime.live_containers().wait().unwrap_err().kind,
        ErrorKind::Missing
    );
    server.join().unwrap();
}

#[test]
fn listed_container_without_liveness_facts_is_not_silently_dropped() {
    let (_dir, runtime, server) = fixture(vec![
        discovery(),
        (
            "GET /v1.48/containers/json?all=true&size=false",
            200,
            r#"[{"Id":"bad"}]"#,
        ),
        (
            "GET /v1.48/containers/bad/json",
            200,
            r#"{"Id":"bad","Image":"sha256:bad","Config":{"Image":"busybox"},"HostConfig":{"NetworkMode":"bridge"},"Mounts":[],"State":{}}"#,
        ),
    ]);
    assert_eq!(
        runtime.live_containers().wait().unwrap_err().kind,
        ErrorKind::Protocol
    );
    server.join().unwrap();
}

#[test]
fn malformed_mounts_fail_instead_of_becoming_an_empty_safe_fact() {
    let (_dir, runtime, server) = fixture(vec![
        discovery(),
        (
            "GET /v1.48/containers/bad/json",
            200,
            r#"{"Id":"bad","Image":"sha256:bad","Config":{"Image":"busybox"},"HostConfig":{},"Mounts":[{"Type":"bind","Destination":"/home"}]}"#,
        ),
    ]);
    assert_eq!(
        runtime.inspect_container("bad").wait().unwrap_err().kind,
        ErrorKind::Protocol
    );
    server.join().unwrap();
}

#[test]
fn omitted_or_unusable_mount_facts_never_become_an_empty_safe_mount_list() {
    for body in [
        r#"{"Id":"bad","Image":"sha256:bad","Config":{"Image":"busybox"},"HostConfig":{}}"#,
        r#"{"Id":"bad","Image":"sha256:bad","Config":{"Image":"busybox"},"HostConfig":{},"Mounts":[{"Type":"bind","Source":"relative","Destination":"/home","RW":true}]}"#,
        r#"{"Id":"bad","Image":"sha256:bad","Config":{"Image":"busybox"},"HostConfig":{},"Mounts":[{"Type":"volume","Name":"home","Destination":"/home","RW":true}]}"#,
        r#"{"Id":"bad","Image":"sha256:bad","Config":{"Image":"busybox"},"HostConfig":{},"Mounts":[{"Type":"mystery","Destination":"/home","RW":true}]}"#,
    ] {
        let (_dir, runtime, server) = fixture(vec![
            discovery(),
            ("GET /v1.48/containers/bad/json", 200, body),
        ]);
        assert_eq!(
            runtime.inspect_container("bad").wait().unwrap_err().kind,
            ErrorKind::Protocol
        );
        server.join().unwrap();
    }
}

#[test]
fn omitted_mount_access_mode_is_preserved_as_unknown() {
    let (_dir, runtime, server) = fixture(vec![
        discovery(),
        (
            "GET /v1.48/containers/app/json",
            200,
            r#"{"Id":"app","Image":"sha256:app","Config":{"Image":"busybox"},"HostConfig":{},"Mounts":[{"Type":"bind","Source":"/home","Destination":"/home"}]}"#,
        ),
    ]);
    assert_eq!(
        runtime
            .inspect_container("app")
            .wait()
            .unwrap()
            .unwrap()
            .mounts[0]
            .read_only,
        None
    );
    server.join().unwrap();
}

#[test]
fn snapshot_rejects_replaced_container_identity() {
    let (_dir, runtime, server) = fixture(vec![
        discovery(),
        (
            "GET /v1.48/containers/json?all=true&size=false",
            200,
            r#"[{"Id":"old"}]"#,
        ),
        (
            "GET /v1.48/containers/old/json",
            200,
            r#"{"Id":"new","Image":"sha256:new","Config":{"Image":"busybox"},"HostConfig":{},"Mounts":[],"State":{"Running":true,"Paused":false,"Restarting":false}}"#,
        ),
    ]);
    assert_eq!(
        runtime.live_containers().wait().unwrap_err().kind,
        ErrorKind::Protocol
    );
    server.join().unwrap();
}

#[test]
fn image_without_config_is_not_reported_as_having_an_empty_environment() {
    let (_dir, runtime, server) = fixture(vec![
        discovery(),
        ("GET /v1.48/images/app/json", 200, r#"{"Id":"sha256:app"}"#),
    ]);
    assert_eq!(
        runtime
            .inspect_image_metadata("app")
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
    server.join().unwrap();
}

fn bind(source: &str, destination: &str) -> Mount {
    Mount {
        kind: MountKind::Bind,
        source: Some(DaemonHostPath(source.into())),
        name: None,
        destination: destination.into(),
        read_only: Some(false),
    }
}

#[test]
fn namespace_mapping_uses_component_prefixes_and_rejects_hidden_nested_mounts() {
    let mounts = vec![
        bind("/host/data", "/data"),
        Mount {
            kind: MountKind::Tmpfs,
            source: None,
            name: None,
            destination: "/data/cache".into(),
            read_only: Some(false),
        },
    ];
    assert_eq!(
        daemon_path_for_agent_path(&mounts, std::path::Path::new("/data/file")),
        Some(DaemonHostPath("/host/data/file".into()))
    );
    assert_eq!(
        agent_path_for_daemon_path(&mounts, std::path::Path::new("/host/data/cache")),
        None
    );
    assert_eq!(
        daemon_path_for_agent_path(&mounts, std::path::Path::new("/database")),
        None
    );
}

#[test]
fn namespace_mapping_rejects_named_volume_shadow_and_ambiguous_destinations() {
    let mounts = vec![
        bind("/host/data", "/data"),
        Mount {
            kind: MountKind::Volume,
            source: Some(DaemonHostPath("/var/lib/docker/volumes/cache/_data".into())),
            name: Some("cache".into()),
            destination: "/data/cache".into(),
            read_only: Some(false),
        },
    ];
    assert_eq!(
        agent_path_for_daemon_path(&mounts, std::path::Path::new("/host/data/cache")),
        None
    );
    assert_eq!(
        daemon_path_for_agent_path(&mounts, std::path::Path::new("/data/cache/key")),
        None
    );
    let ambiguous = vec![bind("/one", "/data"), bind("/two", "/data")];
    assert_eq!(
        daemon_path_for_agent_path(&ambiguous, std::path::Path::new("/data/key")),
        None
    );
}

#[test]
fn unavailable_socket_is_not_a_missing_container() {
    let dir = tempfile::tempdir().unwrap();
    let runtime = RuntimeClient::new(RuntimeConfig::unix(dir.path().join("missing.sock"))).unwrap();
    assert_eq!(
        runtime.inspect_container("app").wait().unwrap_err().kind,
        ErrorKind::Unavailable
    );
}

#[test]
fn image_environment_and_daemon_storage_root_are_read_as_daemon_facts() {
    let (_dir, runtime, server) = fixture(vec![
        discovery(),
        (
            "GET /v1.48/images/app/json",
            200,
            r#"{"Id":"sha256:app","Config":{"Env":["A=1","B=two"]}}"#,
        ),
        discovery(),
        (
            "GET /v1.48/info",
            200,
            r#"{"DockerRootDir":"/var/lib/docker"}"#,
        ),
    ]);
    assert_eq!(
        runtime
            .inspect_image_metadata("app")
            .wait()
            .unwrap()
            .unwrap()
            .baked_env,
        ["A=1", "B=two"]
    );
    assert_eq!(
        runtime.engine_storage().wait().unwrap().root,
        DaemonHostPath("/var/lib/docker".into())
    );
    server.join().unwrap();
}
