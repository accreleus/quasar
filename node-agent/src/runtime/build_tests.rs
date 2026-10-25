//! Managed builds exercised at the Quasar runtime boundary.
use super::*;
use std::collections::BTreeMap;
use std::io::{Read, Write};
use std::os::unix::net::UnixListener;

fn read_line(socket: &mut impl Read) -> String {
    let mut line = Vec::new();
    while !line.ends_with(b"\r\n") {
        let mut byte = [0];
        socket.read_exact(&mut byte).unwrap();
        line.push(byte[0]);
        assert!(line.len() < 65536);
    }
    String::from_utf8(line).unwrap()
}
fn decode(value: &str) -> String {
    let mut out = Vec::new();
    let mut bytes = value.bytes();
    while let Some(byte) = bytes.next() {
        match byte {
            b'+' => out.push(b' '),
            b'%' => {
                let hex = [bytes.next().unwrap(), bytes.next().unwrap()];
                out.push(u8::from_str_radix(std::str::from_utf8(&hex).unwrap(), 16).unwrap());
            }
            v => out.push(v),
        }
    }
    String::from_utf8(out).unwrap()
}

#[test]
fn managed_build_uses_classic_builder_and_verifies_the_result() {
    let (_dir, config, mut request, server) = engine(vec![
        version(),
        absent(),
        post("{\"stream\":\"Step 1/1 : COPY payload /payload\\n\"}\n"),
        built(),
    ]);
    let context = request.context_dir.clone();
    std::fs::create_dir_all(context.join("nested")).unwrap();
    std::fs::write(
        context.join("nested/Recipe"),
        "FROM scratch\nARG MESSAGE\nCOPY payload /payload\n",
    )
    .unwrap();
    std::fs::write(context.join("payload"), "fixture").unwrap();
    std::fs::write(
        context.join(".dockerignore"),
        "secret.txt\nprivate/**\n!private/keep.txt\n",
    )
    .unwrap();
    std::fs::write(context.join("secret.txt"), "must not upload").unwrap();
    std::fs::create_dir(context.join("private")).unwrap();
    std::fs::write(context.join("private/keep.txt"), "keep").unwrap();
    std::fs::write(context.join("private/drop.txt"), "drop").unwrap();
    request.dockerfile = "nested/Recipe".into();
    request.build_args = BTreeMap::from([("MESSAGE".into(), "hello world".into())]);
    let result = RuntimeClient::new(config)
        .unwrap()
        .build_image(request, Duration::from_secs(3))
        .wait(|_| {})
        .unwrap();
    assert_eq!(result.id, "sha256:built");
    assert_eq!(result.bytes, 42);
    let requests = server.join().unwrap();
    let query = &requests[2].query;
    assert_eq!(query["version"], "1");
    assert_eq!(query["rm"], "true");
    assert_eq!(query["forcerm"], "false");
    assert_eq!(query["dockerfile"], "nested/Recipe");
    assert_eq!(query["t"], "quasar-local/test:one");
    assert_eq!(
        serde_json::from_str::<serde_json::Value>(&query["buildargs"]).unwrap(),
        serde_json::json!({"MESSAGE":"hello world"})
    );
    let mut archive = tar::Archive::new(&requests[2].body[..]);
    let paths: Vec<_> = archive
        .entries()
        .unwrap()
        .map(|e| e.unwrap().path().unwrap().to_string_lossy().into_owned())
        .collect();
    for keep in ["nested/Recipe", "payload", "private/keep.txt"] {
        assert!(paths.iter().any(|p| p == keep));
    }
    for drop in ["secret.txt", "private/drop.txt"] {
        assert!(!paths.iter().any(|p| p == drop));
    }
}

struct Reply {
    method: &'static str,
    path: &'static str,
    status: u16,
    body: String,
    hold: Duration,
    incomplete: bool,
}
fn response(
    method: &'static str,
    path: &'static str,
    status: u16,
    body: impl Into<String>,
) -> Reply {
    Reply {
        method,
        path,
        status,
        body: body.into(),
        hold: Duration::ZERO,
        incomplete: false,
    }
}
fn version() -> Reply {
    response(
        "GET",
        "/version",
        200,
        r#"{"Version":"28.0.0","ApiVersion":"1.48","MinAPIVersion":"1.40"}"#,
    )
}
const IMAGE_PATH: &str = "/v1.48/images/quasar-local/test:one/json";
fn absent() -> Reply {
    response("GET", IMAGE_PATH, 404, r#"{"message":"No such image"}"#)
}
fn built() -> Reply {
    response(
        "GET",
        IMAGE_PATH,
        200,
        r#"{"Id":"sha256:built","Size":42,"Config":{"Labels":{"io.quasar.build-operation":"$BUILD_ID"}}}"#,
    )
}
fn post(body: impl Into<String>) -> Reply {
    response("POST", "/v1.48/build", 200, body)
}
struct Observed {
    query: BTreeMap<String, String>,
    headers: BTreeMap<String, String>,
    body: Vec<u8>,
}
fn engine(
    replies: Vec<Reply>,
) -> (
    tempfile::TempDir,
    RuntimeConfig,
    BuildRequest,
    std::thread::JoinHandle<Vec<Observed>>,
) {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("engine.sock");
    let listener = UnixListener::bind(&path).unwrap();
    listener.set_nonblocking(true).unwrap();
    let thread = std::thread::spawn(move || {
        let mut observed = Vec::new();
        let mut build_id = String::new();
        for reply in replies {
            let until = std::time::Instant::now() + Duration::from_secs(5);
            let mut socket = loop {
                match listener.accept() {
                    Ok((s, _)) => break s,
                    Err(e)
                        if e.kind() == std::io::ErrorKind::WouldBlock
                            && std::time::Instant::now() < until =>
                    {
                        std::thread::sleep(Duration::from_millis(5))
                    }
                    Err(e) => panic!("missing {} {}: {e}", reply.method, reply.path),
                }
            };
            socket
                .set_read_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            let first = read_line(&mut socket);
            let mut words = first.split_whitespace();
            assert_eq!(words.next().unwrap(), reply.method);
            let url = words.next().unwrap();
            let (path, query) = url.split_once('?').unwrap_or((url, ""));
            assert_eq!(path, reply.path);
            let query: BTreeMap<_, _> = query
                .split('&')
                .filter(|v| !v.is_empty())
                .map(|p| {
                    let (k, v) = p.split_once('=').unwrap();
                    (decode(k), decode(v))
                })
                .collect();
            let mut headers = BTreeMap::new();
            loop {
                let line = read_line(&mut socket);
                if line == "\r\n" {
                    break;
                }
                let (k, v) = line.split_once(':').unwrap();
                headers.insert(k.to_ascii_lowercase(), v.trim().into());
            }
            let mut body = Vec::new();
            if headers
                .get("transfer-encoding")
                .is_some_and(|v: &String| v == "chunked")
            {
                loop {
                    let size = usize::from_str_radix(read_line(&mut socket).trim(), 16).unwrap();
                    if size == 0 {
                        read_line(&mut socket);
                        break;
                    }
                    let start = body.len();
                    body.resize(start + size, 0);
                    socket.read_exact(&mut body[start..]).unwrap();
                    read_line(&mut socket);
                }
            } else {
                body.resize(
                    headers
                        .get("content-length")
                        .and_then(|v| v.parse().ok())
                        .unwrap_or(0),
                    0,
                );
                socket.read_exact(&mut body).unwrap();
            }
            if let Some(labels) = query.get("labels") {
                let labels: serde_json::Value = serde_json::from_str(labels).unwrap();
                build_id = labels["io.quasar.build-operation"].as_str().unwrap().into();
            }
            observed.push(Observed {
                query,
                headers,
                body,
            });
            let body = reply.body.replace("$BUILD_ID", &build_id);
            let size = body.len() + usize::from(reply.incomplete) * 100;
            write!(socket,"HTTP/1.1 {} OK\r\nContent-Type: application/json\r\nContent-Length: {size}\r\nConnection: close\r\n\r\n",reply.status).unwrap();
            std::thread::sleep(reply.hold);
            let _ = socket.write_all(body.as_bytes());
        }
        observed
    });
    let context = dir.path().join("context");
    std::fs::create_dir(&context).unwrap();
    std::fs::write(
        context.join("Dockerfile"),
        "FROM scratch\nLABEL fixture=yes\n",
    )
    .unwrap();
    let mut config = RuntimeConfig::unix(path);
    config.image_state_path = Some(dir.path().join("intents"));
    let request = BuildRequest {
        tag: "quasar-local/test:one".into(),
        context_dir: context,
        dockerfile: "Dockerfile".into(),
        build_args: BTreeMap::new(),
    };
    (dir, config, request, thread)
}

#[test]
fn build_stream_errors_are_definite_failures_not_successful_http_responses() {
    let (_dir, config, request, server) = engine(vec![
        version(),
        absent(),
        post("{\"errorDetail\":{\"message\":\"failed to build fixture-secret\"}}\n"),
    ]);
    let error = RuntimeClient::new(config)
        .unwrap()
        .build_image(request, Duration::from_secs(2))
        .wait(|_| {})
        .unwrap_err();
    assert_eq!(error.kind, ErrorKind::BuildFailed);
    assert!(!error.to_string().contains("fixture-secret"));
    server.join().unwrap();
}

#[test]
fn interrupted_build_reconciles_the_same_operation_after_restart() {
    let mut interrupted = post("");
    interrupted.incomplete = true;
    let (_dir, config, request, server) =
        engine(vec![version(), absent(), interrupted, version(), built()]);
    let runtime = RuntimeClient::new(config.clone()).unwrap();
    assert_eq!(
        runtime
            .build_image(request.clone(), Duration::from_secs(2))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    drop(runtime);
    let result = RuntimeClient::new(config)
        .unwrap()
        .build_image(request, Duration::from_secs(2))
        .wait(|_| {})
        .unwrap();
    assert_eq!(result.id, "sha256:built");
    assert_eq!(
        server
            .join()
            .unwrap()
            .iter()
            .filter(|r| r.query.contains_key("version"))
            .count(),
        1,
        "repeated the build mutation"
    );
}

fn cut(mut reply: Reply) -> Reply {
    reply.incomplete = true;
    reply
}
fn build_requests(requests: &[Observed]) -> usize {
    requests
        .iter()
        .filter(|r| r.query.contains_key("version"))
        .count()
}
fn records(config: &RuntimeConfig) -> usize {
    std::fs::read_dir(config.image_state_path.as_ref().unwrap())
        .map(|entries| {
            entries
                .filter(|e| e.as_ref().unwrap().path().extension().is_none())
                .count()
        })
        .unwrap_or(0)
}

/// An agent that dies at an engine call leaves what a reply cut short there leaves:
/// the record as written so far, and the lease free (#505).
#[test]
fn a_build_cut_off_at_any_engine_call_is_settled_by_the_next_start() {
    // (engine call cut short, whether the engine finished the build anyway, builds sent)
    for (at, landed, builds) in [
        (0, false, 1),
        (1, false, 1),
        (2, false, 2),
        (2, true, 1),
        (3, false, 2),
        (3, true, 1),
    ] {
        let after = || if landed { built() } else { absent() };
        let mut replies = vec![version(), absent(), post(""), after()];
        replies.truncate(at + 1);
        let last = replies.pop().unwrap();
        replies.push(cut(last));
        replies.extend([version(), after()]);
        if !landed {
            replies.extend([post(""), built()]);
        }
        let (_dir, config, request, server) = engine(replies);
        let first = RuntimeClient::new(config.clone())
            .unwrap()
            .build_image(request.clone(), Duration::from_secs(3))
            .wait(|_| {})
            .unwrap_err();
        assert_eq!(
            (first.kind == ErrorKind::UnknownOutcome, records(&config)),
            (at >= 2, usize::from(at >= 2)),
            "cut at {at}: {first}"
        );
        let result = RuntimeClient::new(config.clone())
            .unwrap()
            .build_image(request, Duration::from_secs(3))
            .wait(|_| {});
        assert_eq!(
            result.map(|image| image.id).map_err(|e| e.kind),
            Ok("sha256:built".into()),
            "cut at {at}, landed {landed}"
        );
        assert_eq!(records(&config), 0, "cut at {at}, landed {landed}");
        assert_eq!(
            build_requests(&server.join().unwrap()),
            builds,
            "cut at {at}, landed {landed}"
        );
    }
}

#[test]
fn a_build_that_fails_after_an_interrupted_one_reports_its_own_failure() {
    let refused = "pull access denied for fixture-base, repository does not exist";
    let (_dir, config, request, server) = engine(vec![
        version(),
        absent(),
        cut(post("")),
        version(),
        absent(),
        post(format!(
            "{}\n",
            serde_json::json!({"errorDetail":{"message":refused},"error":refused})
        )),
    ]);
    let build = || {
        RuntimeClient::new(config.clone())
            .unwrap()
            .build_image(request.clone(), Duration::from_secs(3))
            .wait(|_| {})
            .unwrap_err()
            .kind
    };
    assert_eq!(build(), ErrorKind::UnknownOutcome);
    assert_eq!(build(), ErrorKind::RegistryDenied);
    assert_eq!(records(&config), 0);
    server.join().unwrap();
}

/// The record a replaced agent leaves when it dies mid-build (#505).
fn strand_build_record(
    config: &RuntimeConfig,
    image: &str,
    socket: &std::path::Path,
    fingerprint: Option<&str>,
) -> std::path::PathBuf {
    use sha2::{Digest, Sha256};
    let root = config.image_state_path.clone().unwrap();
    std::fs::create_dir_all(&root).unwrap();
    let key: String = Sha256::digest(image.as_bytes())
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect();
    let path = root.join(key);
    let record = serde_json::json!({
        "image": image,
        "socket": socket,
        "remove_id": null,
        "build_id": "previous-agent",
        "build_fingerprint": fingerprint,
    });
    std::fs::write(&path, record.to_string()).unwrap();
    path
}

#[test]
fn a_build_record_from_another_engine_socket_is_retired_and_built_again() {
    let (_dir, config, request, server) = engine(vec![version(), absent(), post(""), built()]);
    let record = strand_build_record(
        &config,
        &request.tag,
        std::path::Path::new("/another-engine.sock"),
        Some("previous-context"),
    );
    assert_eq!(
        RuntimeClient::new(config)
            .unwrap()
            .build_image(request, Duration::from_secs(3))
            .wait(|_| {})
            .unwrap()
            .id,
        "sha256:built"
    );
    assert!(!record.exists());
    assert_eq!(build_requests(&server.join().unwrap()), 1);
}

#[test]
fn an_unparsable_record_is_retired_and_built_again() {
    let (_dir, config, request, server) = engine(vec![version(), absent(), post(""), built()]);
    let record = strand_build_record(&config, &request.tag, &config.socket, None);
    std::fs::write(&record, "").unwrap();
    assert_eq!(
        RuntimeClient::new(config)
            .unwrap()
            .build_image(request, Duration::from_secs(3))
            .wait(|_| {})
            .unwrap()
            .id,
        "sha256:built"
    );
    assert!(!record.exists());
    assert_eq!(build_requests(&server.join().unwrap()), 1);
}

#[test]
fn an_unparsable_record_is_left_alone_while_its_lease_is_held() {
    use std::os::fd::AsRawFd;
    let (_dir, config, request, server) = engine(vec![version()]);
    let record = strand_build_record(&config, &request.tag, &config.socket, None);
    std::fs::write(&record, "").unwrap();
    let lease = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .write(true)
        .open(record.with_extension("lock"))
        .unwrap();
    assert_eq!(
        unsafe { libc::flock(lease.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
        0
    );
    assert_eq!(
        RuntimeClient::new(config)
            .unwrap()
            .build_image(request, Duration::from_secs(1))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert!(record.exists());
    assert_eq!(server.join().unwrap().len(), 1);
}

#[test]
fn a_finished_build_whose_record_has_no_fingerprint_is_built_again() {
    let finished = response(
        "GET",
        IMAGE_PATH,
        200,
        r#"{"Id":"sha256:earlier","Size":1,"Config":{"Labels":{"io.quasar.build-operation":"previous-agent"}}}"#,
    );
    let (_dir, config, request, server) = engine(vec![version(), finished, post(""), built()]);
    let record = strand_build_record(&config, &request.tag, &config.socket, None);
    assert_eq!(
        RuntimeClient::new(config)
            .unwrap()
            .build_image(request, Duration::from_secs(3))
            .wait(|_| {})
            .unwrap()
            .id,
        "sha256:built"
    );
    assert!(!record.exists());
    assert_eq!(build_requests(&server.join().unwrap()), 1);
}

#[test]
fn a_build_record_is_left_alone_while_its_lease_is_held() {
    use std::os::fd::AsRawFd;
    let (_dir, config, request, server) =
        engine(vec![version(), version(), absent(), post(""), built()]);
    let record = strand_build_record(
        &config,
        &request.tag,
        &config.socket,
        Some("previous-context"),
    );
    // A held lease is a build still running, in this process or a replaced agent.
    let lease = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .write(true)
        .open(record.with_extension("lock"))
        .unwrap();
    assert_eq!(
        unsafe { libc::flock(lease.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
        0
    );
    let runtime = RuntimeClient::new(config).unwrap();
    // Waits on the lease for its whole budget: an inspection or a build sent here
    // would meet the fixture's second discovery reply and fail it.
    assert_eq!(
        runtime
            .build_image(request.clone(), Duration::from_secs(1))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert!(record.exists());
    drop(lease);
    assert_eq!(
        runtime
            .build_image(request, Duration::from_secs(3))
            .wait(|_| {})
            .unwrap()
            .id,
        "sha256:built"
    );
    assert!(!record.exists());
    assert_eq!(build_requests(&server.join().unwrap()), 1);
}

#[test]
fn build_supplies_existing_registry_logins_without_exposing_them_to_callers() {
    use base64::Engine;
    let (dir, mut config, request, server) = engine(vec![version(), absent(), post(""), built()]);
    let path = dir.path().join("config.json");
    std::fs::write(
        &path,
        r#"{"auths":{"registry.example":{"auth":"Zml4dHVyZTpwcml2YXRl"}}}"#,
    )
    .unwrap();
    config.registry_config_path = Some(path);
    RuntimeClient::new(config)
        .unwrap()
        .build_image(request, Duration::from_secs(2))
        .wait(|_| {})
        .unwrap();
    let requests = server.join().unwrap();
    let encoded = &requests[2].headers["x-registry-config"];
    let decoded = base64::engine::general_purpose::URL_SAFE
        .decode(encoded)
        .unwrap();
    let credentials: serde_json::Value = serde_json::from_slice(&decoded).unwrap();
    assert_eq!(credentials["registry.example"]["username"], "fixture");
    assert_eq!(credentials["registry.example"]["password"], "private");
}

#[test]
fn fragmented_and_verbose_build_output_still_reports_progress() {
    let output = [
        serde_json::json!({"stream":"Step 1/"}),
        serde_json::json!({"stream":"2 : FROM scratch\n"}),
        serde_json::json!({"stream":"x".repeat(100_000)}),
        serde_json::json!({"stream":"\nStep 2/"}),
        serde_json::json!({"stream":"2 : LABEL done=yes\n"}),
    ]
    .iter()
    .map(|v| format!("{v}\n"))
    .collect::<String>();
    let (_dir, config, request, server) = engine(vec![version(), absent(), post(output), built()]);
    let mut samples = Vec::new();
    RuntimeClient::new(config)
        .unwrap()
        .build_image(request, Duration::from_secs(2))
        .wait(|p| samples.push(p.percent))
        .unwrap();
    assert!(
        samples.contains(&100),
        "lost final build progress: {samples:?}"
    );
    server.join().unwrap();
}

#[test]
fn build_deadline_does_not_depend_on_output_or_claim_rollback() {
    let mut silent = post("{\"stream\":\"eventually\"}\n");
    silent.hold = Duration::from_secs(2);
    let (_dir, config, request, server) = engine(vec![
        version(),
        absent(),
        silent,
        version(),
        absent(),
        post(""),
        built(),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    let start = std::time::Instant::now();
    assert_eq!(
        runtime
            .build_image(request.clone(), Duration::from_secs(1))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert!(start.elapsed() < Duration::from_millis(1500));
    // The deadline dropped the build and its lease with it, so the next one builds.
    assert_eq!(
        runtime
            .build_image(request, Duration::from_secs(4))
            .wait(|_| {})
            .unwrap()
            .id,
        "sha256:built"
    );
    assert_eq!(server.join().unwrap().len(), 7);
}

#[test]
fn old_image_is_not_proof_of_a_successful_build_and_the_next_build_replaces_it() {
    let old = || response("GET", IMAGE_PATH, 200, r#"{"Id":"sha256:old","Size":1}"#);
    let (_dir, config, request, server) = engine(vec![
        version(),
        old(),
        post(""),
        old(),
        version(),
        old(),
        post(""),
        built(),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    assert_eq!(
        runtime
            .build_image(request.clone(), Duration::from_secs(2))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(
        runtime
            .build_image(request, Duration::from_secs(2))
            .wait(|_| {})
            .unwrap()
            .id,
        "sha256:built"
    );
    assert_eq!(build_requests(&server.join().unwrap()), 2);
}

#[test]
fn definite_build_failure_allows_a_corrected_retry() {
    let (_dir, config, request, server) = engine(vec![
        version(),
        absent(),
        post("{\"errorDetail\":{\"message\":\"bad instruction\"}}\n"),
        version(),
        absent(),
        post(""),
        built(),
    ]);
    let runtime = RuntimeClient::new(config).unwrap();
    assert_eq!(
        runtime
            .build_image(request.clone(), Duration::from_secs(2))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::BuildFailed
    );
    std::fs::write(
        request.context_dir.join("Dockerfile"),
        "FROM scratch\nLABEL fixed=yes\n",
    )
    .unwrap();
    assert!(runtime
        .build_image(request, Duration::from_secs(2))
        .wait(|_| {})
        .is_ok());
    server.join().unwrap();
}

#[test]
fn invalid_context_is_rejected_before_contacting_the_engine() {
    for bad in ["missing", "../Dockerfile", "/Dockerfile"] {
        let (_dir, config, mut request, server) = engine(vec![]);
        request.dockerfile = bad.into();
        assert_eq!(
            RuntimeClient::new(config)
                .unwrap()
                .build_image(request, Duration::from_secs(2))
                .wait(|_| {})
                .unwrap_err()
                .kind,
            ErrorKind::InvalidBuildContext
        );
        server.join().unwrap();
    }
    let (_dir, config, request, server) = engine(vec![]);
    std::os::unix::fs::symlink("/etc/passwd", request.context_dir.join("link")).unwrap();
    assert_eq!(
        RuntimeClient::new(config)
            .unwrap()
            .build_image(request, Duration::from_secs(2))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::InvalidBuildContext
    );
    server.join().unwrap();
}

#[test]
fn dockerignore_context_policy_preserves_negations_patterns_and_required_files() {
    let (_dir, config, request, server) = engine(vec![version(), absent(), post(""), built()]);
    std::fs::write(request.context_dir.join(".dockerignore"),"\u{feff}# comment\nDockerfile\n.dockerignore\n**/*.log\ntmp?\n[a-c].txt\n/cache/\n!cache/keep\n").unwrap();
    for file in [
        "a.log",
        "dir/nested.log",
        "tmp1",
        "a.txt",
        "d.txt",
        "cache/drop",
        "cache/keep",
        "dir/a.txt",
    ] {
        let path = request.context_dir.join(file);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(path, "test").unwrap();
    }
    RuntimeClient::new(config)
        .unwrap()
        .build_image(request, Duration::from_secs(2))
        .wait(|_| {})
        .unwrap();
    let requests = server.join().unwrap();
    let mut archive = tar::Archive::new(&requests[2].body[..]);
    let paths: Vec<_> = archive
        .entries()
        .unwrap()
        .map(|e| e.unwrap().path().unwrap().to_string_lossy().into_owned())
        .collect();
    for keep in [
        "Dockerfile",
        ".dockerignore",
        "d.txt",
        "cache/keep",
        "dir/a.txt",
    ] {
        assert!(paths.iter().any(|p| p == keep), "missing {keep}: {paths:?}");
    }
    for drop in ["a.log", "dir/nested.log", "tmp1", "a.txt", "cache/drop"] {
        assert!(!paths.iter().any(|p| p == drop), "included {drop}");
    }
}

#[test]
fn dropping_build_observation_and_client_does_not_stop_the_owned_build() {
    let mut delayed = post("{\"stream\":\"Step 1/1 : FROM scratch\\n\"}\n");
    delayed.hold = Duration::from_millis(200);
    let (_dir, config, request, server) = engine(vec![version(), absent(), delayed, built()]);
    let runtime = RuntimeClient::new(config).unwrap();
    let operation = runtime.build_image(request, Duration::from_secs(3));
    assert_eq!(
        operation
            .wait_with_cancel(|_| {}, || true)
            .unwrap_err()
            .kind,
        ErrorKind::Cancelled
    );
    drop(runtime);
    assert_eq!(
        server.join().unwrap().len(),
        4,
        "observation cancellation stopped the build"
    );
}

#[test]
fn build_loads_all_logins_from_the_existing_credential_store() {
    use base64::Engine;
    use std::os::unix::fs::PermissionsExt;
    if std::env::var_os("QUASAR_BUILD_CREDENTIAL_FIXTURE_CHILD").is_none() {
        let directory = tempfile::tempdir().unwrap();
        let helper = directory.path().join("docker-credential-buildfixture");
        std::fs::write(&helper,b"#!/bin/sh\ncase \"$1\" in\nlist) printf '%s' '{\"registry.example\":\"fixture\"}' ;;\nget) read -r registry; [ \"$registry\" = registry.example ] || exit 1; printf '%s' '{\"Username\":\"fixture\",\"Secret\":\"private\"}' ;;\n*) exit 1 ;;\nesac\n").unwrap();
        std::fs::set_permissions(helper, std::fs::Permissions::from_mode(0o700)).unwrap();
        assert!(std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "runtime::build_tests::build_loads_all_logins_from_the_existing_credential_store"
            ])
            .env("QUASAR_BUILD_CREDENTIAL_FIXTURE_CHILD", "1")
            .env("PATH", directory.path())
            .status()
            .unwrap()
            .success());
        return;
    }
    let (dir, mut config, request, server) = engine(vec![version(), absent(), post(""), built()]);
    let path = dir.path().join("config.json");
    std::fs::write(&path, r#"{"credsStore":"buildfixture"}"#).unwrap();
    config.registry_config_path = Some(path);
    RuntimeClient::new(config)
        .unwrap()
        .build_image(request, Duration::from_secs(3))
        .wait(|_| {})
        .unwrap();
    let requests = server.join().unwrap();
    let decoded = base64::engine::general_purpose::URL_SAFE
        .decode(&requests[2].headers["x-registry-config"])
        .unwrap();
    let value: serde_json::Value = serde_json::from_slice(&decoded).unwrap();
    assert_eq!(value["registry.example"]["password"], "private");
}

#[test]
fn docker_character_classes_do_not_use_rust_set_operators() {
    for (pattern, files) in [
        ("[a&&b].txt", vec!["a.txt", "b.txt", "&.txt"]),
        ("[[]", vec!["["]),
        ("[x~~y].txt", vec!["x.txt", "~.txt", "y.txt"]),
    ] {
        let (_dir, config, request, server) = engine(vec![version(), absent(), post(""), built()]);
        std::fs::write(
            request.context_dir.join(".dockerignore"),
            format!("{pattern}\n"),
        )
        .unwrap();
        for file in &files {
            std::fs::write(request.context_dir.join(file), "private").unwrap();
        }
        RuntimeClient::new(config)
            .unwrap()
            .build_image(request, Duration::from_secs(3))
            .wait(|_| {})
            .unwrap();
        let requests = server.join().unwrap();
        let mut archive = tar::Archive::new(&requests[2].body[..]);
        let paths: Vec<_> = archive
            .entries()
            .unwrap()
            .map(|e| e.unwrap().path().unwrap().to_string_lossy().into_owned())
            .collect();
        for file in files {
            assert!(
                !paths.iter().any(|p| p == file),
                "Docker pattern {pattern} uploaded excluded {file}"
            );
        }
    }
}
