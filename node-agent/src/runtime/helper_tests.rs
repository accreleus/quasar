//! Behavioral engine fixtures at the public RuntimeClient boundary.
use super::*;
use crate::runtime::helpers::HelperPhase;
use serde_json::{json, Value};
use std::{
    io::{Read, Write},
    os::unix::net::{UnixListener, UnixStream},
    sync::{
        atomic::{AtomicBool, Ordering},
        Mutex,
    },
    thread,
};

const ID: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
#[derive(Default)]
struct LogGate {
    entered: AtomicBool,
    released: AtomicBool,
}
#[derive(Default)]
struct State {
    body: Option<Value>,
    name: String,
    running: bool,
    exited: bool,
    exit: Option<i64>,
    keep_running: bool,
    logs: Vec<(u8, Vec<u8>)>,
    lose_logs: bool,
    lose_create: bool,
    lose_start: bool,
    lose_stop: bool,
    lose_stop_before_effect: bool,
    lose_remove: bool,
    refuse_remove: bool,
    refuse_create: bool,
    replace_id: bool,
    inspect_code: Option<u16>,
    host_mount_override: Option<Value>,
    requests: Vec<String>,
    pause_first_log: Option<Arc<LogGate>>,
}
struct Engine {
    state: Arc<Mutex<State>>,
    config: RuntimeConfig,
    stop: Arc<AtomicBool>,
    thread: Option<thread::JoinHandle<()>>,
    _dir: tempfile::TempDir,
}
impl Engine {
    fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("docker.sock");
        let listener = UnixListener::bind(&path).unwrap();
        listener.set_nonblocking(true).unwrap();
        let state = Arc::new(Mutex::new(State {
            exit: Some(23),
            logs: vec![(1, b"final stdout".to_vec()), (2, b"final stderr".to_vec())],
            ..Default::default()
        }));
        let stop = Arc::new(AtomicBool::new(false));
        let server_state = state.clone();
        let stopped = stop.clone();
        let thread = thread::spawn(move || {
            let mut handlers = Vec::new();
            while !stopped.load(Ordering::SeqCst) {
                match listener.accept() {
                    Ok((socket, _)) => {
                        assert!(handlers.len() < 2048, "fixture request bound");
                        let state = server_state.clone();
                        handlers.push(thread::spawn(move || serve(socket, &state)));
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(2))
                    }
                    Err(error) => panic!("fixture accept: {error}"),
                }
            }
            for handler in handlers {
                handler.join().unwrap();
            }
        });
        let mut config = RuntimeConfig::unix(path);
        config.deadline = Duration::from_secs(2);
        config.image_state_path = Some(dir.path().join("operations"));
        config.diagnostic_owner = Some("fixture-owner".into());
        Self {
            state,
            config,
            stop,
            thread: Some(thread),
            _dir: dir,
        }
    }
    fn client(&self) -> RuntimeClient {
        RuntimeClient::new(self.config.clone()).unwrap()
    }
    fn requests(&self, prefix: &str) -> usize {
        self.state
            .lock()
            .unwrap()
            .requests
            .iter()
            .filter(|v| v.starts_with(prefix))
            .count()
    }
    fn finish(&self) {
        let mut s = self.state.lock().unwrap();
        s.running = false;
        s.exited = true;
    }
}
impl Drop for Engine {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        let joined = self.thread.take().unwrap().join();
        if !thread::panicking() {
            joined.unwrap();
        }
    }
}

fn serve(mut socket: UnixStream, state: &Mutex<State>) {
    socket
        .set_read_timeout(Some(Duration::from_secs(1)))
        .unwrap();
    socket
        .set_write_timeout(Some(Duration::from_secs(1)))
        .unwrap();
    let mut header = Vec::new();
    while !header.ends_with(b"\r\n\r\n") {
        let mut byte = [0];
        if socket.read_exact(&mut byte).is_err() {
            return;
        }
        header.push(byte[0]);
        assert!(header.len() < 16 * 1024);
    }
    let header = String::from_utf8(header).unwrap();
    let first = header.lines().next().unwrap();
    let mut words = first.split_whitespace();
    let method = words.next().unwrap();
    let path = words.next().unwrap();
    let route = path.strip_prefix("/v1.48").unwrap_or(path);
    let length = header
        .lines()
        .filter_map(|line| line.split_once(':'))
        .find(|(key, _)| key.eq_ignore_ascii_case("content-length"))
        .map(|(_, value)| value.trim().parse::<usize>().unwrap())
        .unwrap_or(0);
    assert!(length < 128 * 1024);
    let mut body = vec![0; length];
    if socket.read_exact(&mut body).is_err() {
        return;
    }
    let mut s = state.lock().unwrap();
    s.requests.push(format!("{method} {route}"));
    let mut code = 200;
    let mut response = json!({});
    let mut raw = None;
    let mut gate = None;
    if route == "/version" {
        response = json!({"Platform":{"Name":"Docker"},"Version":"28.0.0","ApiVersion":"1.48","MinAPIVersion":"1.40"});
    } else if method == "POST" && route.starts_with("/containers/create?") {
        if s.refuse_create {
            s.refuse_create = false;
            code = 400;
            response = json!({"message":"invalid bind source"});
        } else {
            assert!(s.body.is_none(), "duplicate create");
            s.body = Some(serde_json::from_slice(&body).unwrap());
            s.name = route
                .split("name=")
                .nth(1)
                .unwrap()
                .split('&')
                .next()
                .unwrap()
                .to_owned();
            code = 201;
            response = json!({"Id":ID,"Warnings":[]});
            if std::mem::take(&mut s.lose_create) {
                return;
            }
        }
    } else if method == "GET" && route.ends_with("/json") {
        if let Some(body) = &s.body {
            let mounts: Vec<Value> = body["HostConfig"]["Mounts"].as_array().into_iter().flatten().map(|mount| json!({"Type":"bind","Source":mount["Source"],"Destination":mount["Target"],"RW":!mount["ReadOnly"].as_bool().unwrap_or(false)})).collect();
            let mut host_config = body["HostConfig"].clone();
            if let Some(mounts) = &s.host_mount_override {
                host_config["Mounts"] = mounts.clone();
            }
            response = json!({"Id":if s.replace_id { "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" } else {ID},"Name":format!("/{}",s.name),"Config":body,"HostConfig":host_config,"Mounts":mounts,"State":{"Running":s.running,"Status":if s.running {"running"} else if s.exited {"exited"} else {"created"},"ExitCode":s.exit}});
            if let Some(inspect_code) = s.inspect_code {
                code = inspect_code;
                response = json!({"message":"fixture inspect"});
            }
        } else {
            code = 404;
            response = json!({"message":"missing"});
        }
    } else if method == "POST" && route.ends_with("/start") {
        assert!(route.contains(ID), "mutation must use immutable id");
        s.running = s.keep_running;
        s.exited = !s.keep_running;
        code = 204;
        if std::mem::take(&mut s.lose_start) {
            return;
        }
    } else if method == "POST" && route.contains("/stop?") {
        assert!(route.contains(ID));
        if std::mem::take(&mut s.lose_stop_before_effect) {
            return;
        }
        s.running = false;
        s.exited = true;
        code = 204;
        if std::mem::take(&mut s.lose_stop) {
            return;
        }
    } else if method == "GET" && route.contains("/logs?") {
        if std::mem::take(&mut s.lose_logs) {
            return;
        }
        let mut bytes = Vec::new();
        for (kind, log) in &s.logs {
            bytes.extend_from_slice(&[*kind, 0, 0, 0]);
            bytes.extend_from_slice(&(log.len() as u32).to_be_bytes());
            bytes.extend_from_slice(log);
        }
        raw = Some(bytes);
        gate = s.pause_first_log.take();
    } else if method == "DELETE" && route.starts_with("/containers/") {
        assert!(route.contains(ID));
        assert!(
            route.contains("force=false") && route.contains("v=false"),
            "never force or delete volumes"
        );
        if std::mem::take(&mut s.refuse_remove) {
            code = 500;
            response = json!({"message":"temporary failure"});
        } else {
            s.body = None;
            code = 204;
            if std::mem::take(&mut s.lose_remove) {
                return;
            }
        }
    } else {
        panic!("unexpected fixture request: {method} {route}");
    }
    drop(s);
    let bytes = raw.unwrap_or_else(|| {
        if code == 204 {
            Vec::new()
        } else {
            serde_json::to_vec(&response).unwrap()
        }
    });
    let _ = write!(socket, "HTTP/1.1 {code} OK\r\nContent-Type: application/vnd.docker.raw-stream\r\nContent-Length: {}\r\nConnection: close\r\n\r\n", bytes.len());
    // Fragment both the Docker multiplex header and payload across writes.
    let chunks = bytes.chunks(3);
    let last = chunks.len().saturating_sub(1);
    for (index, chunk) in chunks.enumerate() {
        if index == last {
            if let Some(gate) = &gate {
                gate.entered.store(true, Ordering::SeqCst);
                let until = std::time::Instant::now() + Duration::from_secs(2);
                while !gate.released.load(Ordering::SeqCst) && std::time::Instant::now() < until {
                    thread::sleep(Duration::from_millis(2));
                }
            }
        }
        if socket.write_all(chunk).is_err() {
            break;
        }
    }
}
fn request() -> (DiagnosticHelper, DiagnosticRun) {
    (
        DiagnosticHelper {
            operation: "fixture-helper".into(),
            name: "quasar-diagnostic-fixture".into(),
            image: "quasar-agent:test".into(),
        },
        DiagnosticRun {
            entrypoint: vec!["/bin/sh".into()],
            command: vec!["-c".into(), "exit 23".into()],
            bind: ReadOnlyHostBind {
                source: "/daemon-only/driver".into(),
                target: "/diagnostic".into(),
            },
        },
    )
}

fn audio_request(engine: &Engine) -> (DiagnosticHelper, AudioRun) {
    let socket_dir = engine
        .config
        .image_state_path
        .as_ref()
        .unwrap()
        .parent()
        .unwrap()
        .join("pulse-fixture");
    (
        DiagnosticHelper {
            operation: "pulse-fixture".into(),
            name: "quasar-pulse-fixture".into(),
            image: "quasar-agent:test".into(),
        },
        AudioRun {
            socket_dir: socket_dir.clone(),
            entrypoint: vec!["pulseaudio".into()],
            command: vec![
                "--daemonize=no".into(),
                "--system=no".into(),
                "--disable-shm=true".into(),
                "--exit-idle-time=-1".into(),
                "--log-target=stderr".into(),
                "-n".into(),
                "--load=module-null-sink sink_name=quasar_output".into(),
                "--load=module-null-sink sink_name=quasar_mic".into(),
                "--load=module-remap-source master=quasar_mic.monitor source_name=quasar_mic_src"
                    .into(),
                format!(
                    "--load=module-native-protocol-unix socket={}/native auth-anonymous=1",
                    socket_dir.display()
                ),
            ],
        },
    )
}

#[test]
fn audio_sidecar_uses_the_fixed_writable_pulse_profile_at_the_public_boundary() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let (helper, run) = audio_request(&engine);
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    let body = engine.state.lock().unwrap().body.clone().unwrap();
    assert_eq!(body["Entrypoint"], json!(["pulseaudio"]));
    assert_eq!(body["HostConfig"]["NetworkMode"], json!("none"));
    assert_eq!(body["HostConfig"]["ReadonlyRootfs"], json!(false));
    assert_eq!(body["HostConfig"]["CapDrop"], json!(["ALL"]));
    assert_eq!(
        body["HostConfig"]["SecurityOpt"],
        json!(["no-new-privileges"])
    );
    assert_eq!(body["HostConfig"]["PidsLimit"], json!(512));
    assert_eq!(body["Healthcheck"]["Test"], json!(["NONE"]));
    let socket = engine
        .config
        .image_state_path
        .as_ref()
        .unwrap()
        .parent()
        .unwrap()
        .join("pulse-fixture");
    assert_eq!(
        body["Env"],
        json!([
            format!("HOME={}", socket.display()),
            format!("PULSE_RUNTIME_PATH={}/.runtime", socket.display())
        ])
    );
    assert_eq!(body["HostConfig"]["Mounts"][0]["Source"], json!(socket));
    assert_eq!(body["HostConfig"]["Mounts"][0]["Target"], json!(socket));
    assert_eq!(body["HostConfig"]["Mounts"][0]["ReadOnly"], json!(false));
    assert_eq!(
        engine
            .client()
            .observe_audio_sidecar(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Timeout
    );
    engine.finish();
    engine.client().cleanup_audio_sidecar(id).wait().unwrap();
    assert!(
        !socket.exists(),
        "runtime removes only its marker-owned socket directory after confirmed container removal"
    );
}

#[test]
fn diagnostic_recovery_never_touches_a_running_audio_sidecar() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let (helper, run) = audio_request(&engine);
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    engine.client().recover_diagnostics().wait().unwrap();
    assert!(engine.state.lock().unwrap().running);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    engine.finish();
    engine.client().cleanup_audio_sidecar(id).wait().unwrap();
}

#[test]
fn audio_profile_accepts_dockers_omitted_false_read_only_field() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let (helper, run) = audio_request(&engine);
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    engine.state.lock().unwrap().body.as_mut().unwrap()["HostConfig"]["Mounts"][0]["ReadOnly"] =
        Value::Null;
    engine.finish();
    engine.client().cleanup_audio_sidecar(id).wait().unwrap();
}

#[test]
fn audio_cleanup_retires_populated_directory_and_never_touches_a_replacement_source() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let (helper, run) = audio_request(&engine);
    let socket = run.socket_dir.clone();
    let retired = socket
        .parent()
        .unwrap()
        .join(".quasar-audio-retired-pulse-fixture");
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    std::fs::create_dir(socket.join(".runtime")).unwrap();
    std::fs::create_dir(socket.join(".config")).unwrap();
    std::fs::write(socket.join("native"), b"socket payload").unwrap();
    // A symlink payload is rejected after the source is renamed, retaining the
    // persisted tombstone identity for an explicit retry.
    std::os::unix::fs::symlink("/tmp", socket.join("unsafe")).unwrap();
    engine.finish();
    assert!(engine
        .client()
        .cleanup_audio_sidecar(id.clone())
        .wait()
        .is_err());
    assert!(!socket.exists());
    assert!(retired.exists());
    std::fs::create_dir(&socket).unwrap();
    std::fs::write(socket.join("replacement"), b"do not remove").unwrap();
    std::fs::remove_file(retired.join("unsafe")).unwrap();
    engine.client().cleanup_audio_sidecar(id).wait().unwrap();
    assert_eq!(
        std::fs::read(socket.join("replacement")).unwrap(),
        b"do not remove"
    );
    assert!(!retired.exists());
}

#[test]
fn audio_lost_create_is_abandoned_through_its_recorded_operation_only() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let (helper, run) = audio_request(&engine);
    assert_eq!(
        engine
            .client()
            .run_audio_sidecar(helper.clone(), run)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine
        .client()
        .abandon_audio_sidecar(helper.operation)
        .wait()
        .unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert!(engine.state.lock().unwrap().body.is_none());
}

#[test]
fn audio_lost_remove_retries_tombstone_cleanup_without_touching_a_replacement() {
    let engine = Engine::new();
    let (helper, run) = audio_request(&engine);
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    engine.state.lock().unwrap().lose_remove = true;
    assert_eq!(
        engine
            .client()
            .cleanup_audio_sidecar(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine.client().cleanup_audio_sidecar(id).wait().unwrap();
}

#[test]
fn ordinary_audio_recovery_preserves_live_work_but_boot_retirement_stops_it() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let (helper, run) = audio_request(&engine);
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    engine.client().recover_audio_sidecars().wait().unwrap();
    assert!(engine.state.lock().unwrap().running);
    engine.client().retire_audio_sidecars().wait().unwrap();
    assert!(!engine.state.lock().unwrap().running);
    assert_eq!(
        engine
            .client()
            .observe_audio_sidecar(id)
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
}

#[test]
fn audio_socket_directory_collision_is_preserved_without_a_container_mutation() {
    let engine = Engine::new();
    let (helper, run) = audio_request(&engine);
    std::fs::create_dir(&run.socket_dir).unwrap();
    std::fs::write(run.socket_dir.join("foreign"), b"keep").unwrap();
    assert_eq!(
        engine
            .client()
            .run_audio_sidecar(helper, run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(
        std::fs::read(run.socket_dir.join("foreign")).unwrap(),
        b"keep"
    );
    assert_eq!(engine.requests("POST /containers/create"), 0);
}

#[test]
fn audio_profile_initializes_a_missing_parent_without_removing_it_on_cleanup() {
    let engine = Engine::new();
    let (helper, mut run) = audio_request(&engine);
    let parent = run.socket_dir.parent().unwrap().join("missing-parent");
    run.socket_dir = parent.join("pulse-fixture");
    *run.command.last_mut().unwrap() = format!(
        "--load=module-native-protocol-unix socket={}/native auth-anonymous=1",
        run.socket_dir.display()
    );
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    assert!(parent.exists());
    engine.client().cleanup_audio_sidecar(id).wait().unwrap();
    assert!(parent.exists());
}

fn rewrite_helper_intent(engine: &Engine, operation: &str, change: impl FnOnce(&mut HelperIntent)) {
    let path = helper_intent_path(engine, operation);
    let mut intent: HelperIntent = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    change(&mut intent);
    std::fs::write(path, serde_json::to_vec(&intent).unwrap()).unwrap();
}

fn helper_intent_path(engine: &Engine, operation: &str) -> std::path::PathBuf {
    let helpers = engine
        .config
        .image_state_path
        .as_ref()
        .unwrap()
        .join("helpers");
    std::fs::read_dir(helpers)
        .unwrap()
        .filter_map(Result::ok)
        .map(|entry| entry.path())
        .find(|path| {
            std::fs::read(path)
                .ok()
                .and_then(|bytes| serde_json::from_slice::<HelperIntent>(&bytes).ok())
                .is_some_and(|intent| intent.operation == operation)
        })
        .unwrap()
}

fn helper_intent(engine: &Engine, operation: &str) -> HelperIntent {
    serde_json::from_slice(&std::fs::read(helper_intent_path(engine, operation)).unwrap()).unwrap()
}

#[test]
fn preparing_before_rename_reconciles_original_and_never_removes_a_replacement() {
    use std::os::unix::fs::MetadataExt;
    let engine = Engine::new();
    let (helper, run) = audio_request(&engine);
    let operation = helper.operation.clone();
    let socket = run.socket_dir.clone();
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    let metadata = std::fs::metadata(&socket).unwrap();
    let retired = socket
        .parent()
        .unwrap()
        .join(format!(".quasar-audio-retired-{operation}"));
    rewrite_helper_intent(&engine, &operation, |intent| {
        intent.phase = HelperPhase::Preparing;
        intent.audio_dir_created = true;
        intent.audio_retired_dir = Some(retired);
        intent.audio_dir_device = Some(metadata.dev());
        intent.audio_dir_inode = Some(metadata.ino());
        intent.audio_dir_renamed = false;
    });
    engine.finish();
    engine
        .client()
        .abandon_audio_sidecar(operation)
        .wait()
        .unwrap();
    assert!(!socket.exists());
    drop(id);
}

#[test]
fn preparing_after_rename_retries_tombstone_without_touching_new_original() {
    use std::os::unix::fs::MetadataExt;
    let engine = Engine::new();
    let (helper, run) = audio_request(&engine);
    let operation = helper.operation.clone();
    let socket = run.socket_dir.clone();
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    let metadata = std::fs::metadata(&socket).unwrap();
    let retired = socket
        .parent()
        .unwrap()
        .join(format!(".quasar-audio-retired-{operation}"));
    std::fs::rename(&socket, &retired).unwrap();
    std::os::unix::fs::symlink("/tmp", retired.join("interrupted")).unwrap();
    rewrite_helper_intent(&engine, &operation, |intent| {
        intent.phase = HelperPhase::Preparing;
        intent.audio_dir_created = true;
        intent.audio_retired_dir = Some(retired.clone());
        intent.audio_dir_device = Some(metadata.dev());
        intent.audio_dir_inode = Some(metadata.ino());
        intent.audio_dir_renamed = true;
    });
    engine.finish();
    assert!(engine
        .client()
        .abandon_audio_sidecar(operation.clone())
        .wait()
        .is_err());
    std::fs::create_dir(&socket).unwrap();
    std::fs::write(socket.join("replacement"), b"keep").unwrap();
    std::fs::remove_file(retired.join("interrupted")).unwrap();
    engine
        .client()
        .abandon_audio_sidecar(operation)
        .wait()
        .unwrap();
    assert_eq!(std::fs::read(socket.join("replacement")).unwrap(), b"keep");
    assert!(!retired.exists());
    drop(id);
}

#[test]
fn cleanup_finishes_when_reboot_has_removed_the_runtime_socket_directory() {
    let engine = Engine::new();
    let (helper, run) = audio_request(&engine);
    let socket = run.socket_dir.clone();
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    engine.finish();
    std::fs::remove_dir_all(&socket).unwrap();
    engine.client().cleanup_audio_sidecar(id).wait().unwrap();
}

#[test]
fn cleanup_finishes_when_reboot_has_removed_the_whole_runtime_parent() {
    let engine = Engine::new();
    let (helper, mut run) = audio_request(&engine);
    let runtime_parent = engine
        .config
        .image_state_path
        .as_ref()
        .unwrap()
        .parent()
        .unwrap()
        .join("volatile-runtime");
    run.socket_dir = runtime_parent.join("pulse-fixture");
    *run.command.last_mut().unwrap() = format!(
        "--load=module-native-protocol-unix socket={}/native auth-anonymous=1",
        run.socket_dir.display()
    );
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    engine.finish();
    std::fs::remove_dir_all(runtime_parent).unwrap();

    engine
        .client()
        .cleanup_audio_sidecar(id.clone())
        .wait()
        .unwrap();
    assert_eq!(
        engine
            .client()
            .observe_audio_sidecar(id)
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
}

#[test]
fn repeated_audio_stop_preserves_completed_cleanup_evidence() {
    let engine = Engine::new();
    let (helper, run) = audio_request(&engine);
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    engine
        .client()
        .cleanup_audio_sidecar(id.clone())
        .wait()
        .unwrap();
    engine
        .client()
        .stop_audio_sidecar(id.clone())
        .wait()
        .unwrap();
    assert_eq!(
        engine
            .client()
            .observe_audio_sidecar(id)
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
}

#[test]
fn audio_shared_bind_propagation_is_rejected_before_any_delete() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let (helper, run) = audio_request(&engine);
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    engine.state.lock().unwrap().body.as_mut().unwrap()["HostConfig"]["Mounts"][0]["BindOptions"] =
        json!({"Propagation":"shared"});
    assert!(engine.client().cleanup_audio_sidecar(id).wait().is_err());
    assert_eq!(engine.requests("DELETE /containers/"), 0);
}

#[test]
fn audio_fifo_marker_is_rejected_without_blocking_cleanup() {
    use std::os::unix::ffi::OsStrExt;

    let engine = Engine::new();
    let (helper, run) = audio_request(&engine);
    let socket = run.socket_dir.clone();
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    let marker = socket.join(".quasar-runtime-audio-owner");
    std::fs::remove_file(&marker).unwrap();
    let marker = std::ffi::CString::new(marker.as_os_str().as_bytes()).unwrap();
    // SAFETY: the C string is a NUL-terminated local path and the fixture
    // owns this session directory.
    assert_eq!(unsafe { libc::mkfifo(marker.as_ptr(), 0o600) }, 0);
    engine.finish();

    assert_eq!(
        engine
            .client()
            .cleanup_audio_sidecar(id)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
    assert!(socket.exists());
}

#[test]
fn audio_definitive_create_rejection_removes_its_directory_and_allows_retry() {
    let engine = Engine::new();
    engine.state.lock().unwrap().refuse_create = true;
    let (helper, run) = audio_request(&engine);
    let socket = run.socket_dir.clone();
    assert_eq!(
        engine
            .client()
            .run_audio_sidecar(helper.clone(), run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Engine
    );
    assert!(!socket.exists());
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    engine.client().cleanup_audio_sidecar(id).wait().unwrap();
}

#[test]
fn boot_retirement_continues_past_a_nonregular_journal_and_retires_valid_audio() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let (helper, run) = audio_request(&engine);
    let socket = run.socket_dir.clone();
    engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    std::fs::create_dir(
        engine
            .config
            .image_state_path
            .as_ref()
            .unwrap()
            .join("helpers")
            .join("corrupt-entry"),
    )
    .unwrap();

    assert_eq!(
        engine
            .client()
            .retire_audio_sidecars()
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
    assert!(engine.state.lock().unwrap().body.is_none());
    assert!(!socket.exists());
}

#[test]
fn boot_retirement_marks_lost_create_stopping_before_an_unavailable_daemon() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let (helper, run) = audio_request(&engine);
    let operation = helper.operation.clone();
    assert_eq!(
        engine
            .client()
            .run_audio_sidecar(helper, run)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine.state.lock().unwrap().inspect_code = Some(500);

    assert!(engine.client().retire_audio_sidecars().wait().is_err());
    assert_eq!(
        helper_intent(&engine, &operation).phase,
        HelperPhase::Stopping
    );
}

#[test]
fn definitive_rejection_directory_cleanup_retries_without_container_inspection() {
    let engine = Engine::new();
    let (helper, run) = audio_request(&engine);
    let operation = helper.operation.clone();
    let socket = run.socket_dir.clone();
    engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    rewrite_helper_intent(&engine, &operation, |intent| {
        intent.id = None;
        intent.phase = HelperPhase::CleanupPending;
        intent.result = None;
    });
    std::os::unix::fs::symlink("/tmp", socket.join("interrupted")).unwrap();

    assert_eq!(
        engine
            .client()
            .recover_audio_sidecars()
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
    let before = engine.requests("GET /containers/");
    std::fs::remove_file(
        socket
            .parent()
            .unwrap()
            .join(format!(".quasar-audio-retired-{operation}/interrupted")),
    )
    .unwrap();
    engine.client().recover_audio_sidecars().wait().unwrap();
    assert_eq!(engine.requests("GET /containers/"), before);
    assert!(!socket.exists());
}

/// Exercises the caller's real socket-readiness fallback against the same
/// public Unix-engine fixture. Run explicitly: it mutates process globals and
/// waits for the two-second readiness budget twice.
#[test]
#[ignore]
fn pulse_sidecar_socket_readiness_fallback_keeps_final_evidence_and_cleans() {
    let engine = Engine::new();
    let root = engine
        .config
        .image_state_path
        .as_ref()
        .unwrap()
        .parent()
        .unwrap();
    std::env::set_var(
        "DOCKER_HOST",
        format!("unix://{}", engine.config.socket.display()),
    );
    std::env::set_var("NODE_SECRET_PATH", root.join("audio-owner"));
    std::env::set_var("QUASAR_PULSE_IMAGE", "quasar-agent:test");
    crate::runtime::initialize_image_state(root.join("runtime-state"));
    let runtime = crate::session::container::ContainerRuntime::test_runtime("unused");
    for keep_running in [false, true] {
        engine.state.lock().unwrap().keep_running = keep_running;
        let session = if keep_running {
            "runtime-timeout"
        } else {
            "runtime-exit"
        };
        assert!(crate::session::audio::PulseSidecar::start(
            session,
            &runtime,
            root.to_str().unwrap()
        )
        .unwrap()
        .is_none());
        assert!(engine.state.lock().unwrap().body.is_none());
        assert!(!root.join(format!("pulse-{session}")).exists());
        if !keep_running {
            let helpers = root.join("runtime-state/helpers");
            let evidence = std::fs::read_dir(helpers)
                .unwrap()
                .filter_map(Result::ok)
                .filter_map(|entry| std::fs::read(entry.path()).ok())
                .any(|bytes| {
                    serde_json::from_slice::<HelperIntent>(&bytes)
                        .ok()
                        .and_then(|intent| intent.result)
                        .is_some_and(|result| {
                            result.exit_code == Some(23) && result.stdout == "final stdout"
                        })
                });
            assert!(
                evidence,
                "early exit evidence must survive readiness fallback"
            );
        }
    }
}

#[test]
fn reconciliation_keeps_unknown_outcome_and_reports_sanitized_inspect_cause() {
    for (status, detail) in [
        (404, ErrorKind::Missing),
        (403, ErrorKind::PermissionDenied),
        (500, ErrorKind::Engine),
    ] {
        let engine = Engine::new();
        let client = engine.client();
        let (helper, run) = request();
        client
            .run_diagnostic(helper.clone(), run.clone())
            .wait()
            .unwrap();
        engine.state.lock().unwrap().inspect_code = Some(status);
        let error = client.run_diagnostic(helper, run).wait().unwrap_err();
        assert_eq!(error.kind, ErrorKind::UnknownOutcome);
        assert_eq!(error.reconciliation, Some(detail));
    }
}

#[test]
fn host_config_mount_options_cannot_weaken_the_fixed_read_only_bind() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = request();
    engine.state.lock().unwrap().host_mount_override = Some(json!([{
        "Type":"bind", "Source":"/daemon-only/driver", "Target":"/diagnostic",
        "ReadOnly":false, "BindOptions":{"CreateMountpoint":true}
    }]));
    assert_eq!(
        client.run_diagnostic(helper, run).wait().unwrap_err().kind,
        ErrorKind::Protocol
    );
}

#[test]
fn host_config_mount_false_defaults_are_equivalent_to_omission() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = request();
    engine.state.lock().unwrap().host_mount_override = Some(json!([{
        "Type":"bind", "Source":"/daemon-only/driver", "Target":"/diagnostic",
        "ReadOnly":true,
        "BindOptions":{"CreateMountpoint":false,"NonRecursive":false,
            "ReadOnlyNonRecursive":false,"ReadOnlyForceRecursive":false}
    }]));
    assert!(client.run_diagnostic(helper, run).wait().is_ok());
}

#[test]
fn cleanup_reconciliation_retains_sanitized_inspect_detail() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = request();
    let id = client.run_diagnostic(helper, run).wait().unwrap();
    client.observe_diagnostic(id.clone()).wait().unwrap();
    engine.state.lock().unwrap().refuse_remove = true;
    assert_eq!(
        client
            .cleanup_diagnostic(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine.state.lock().unwrap().inspect_code = Some(500);
    let error = client.cleanup_diagnostic(id).wait().unwrap_err();
    assert_eq!(error.kind, ErrorKind::UnknownOutcome);
    assert_eq!(error.reconciliation, Some(ErrorKind::Engine));
}

#[test]
fn helper_records_final_nonzero_exit_and_logs_before_explicit_cleanup() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = request();
    let id = client.run_diagnostic(helper, run).wait().unwrap();
    let result = client.observe_diagnostic(id.clone()).wait().unwrap();
    assert_eq!(result.exit_code, Some(23));
    assert_eq!(result.stdout, "final stdout");
    assert_eq!(result.stderr, "final stderr");
    assert!(engine.state.lock().unwrap().body.is_some());
    client.cleanup_diagnostic(id.clone()).wait().unwrap();
    assert!(engine.state.lock().unwrap().body.is_none());
    assert_eq!(client.observe_diagnostic(id).wait().unwrap(), result);
}

#[test]
fn recovery_adopts_lost_create_without_starting_or_duplicating_it() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let client = engine.client();
    let (helper, run) = request();
    assert_eq!(
        client.run_diagnostic(helper, run).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    drop(client);
    engine.client().recover_diagnostics().wait().unwrap();
    assert!(engine.state.lock().unwrap().body.is_none());
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 0);
}

#[test]
fn detached_run_replays_its_completed_operation_without_restarting() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = request();
    drop(client.run_diagnostic(helper.clone(), run.clone()));
    drop(client);
    let until = std::time::Instant::now() + Duration::from_secs(2);
    while engine.requests(&format!("POST /containers/{ID}/start")) == 0 {
        assert!(
            std::time::Instant::now() < until,
            "owned run must survive observer and client drop"
        );
        thread::sleep(Duration::from_millis(5));
    }
    let recovered = engine.client();
    let id = recovered
        .run_diagnostic(helper.clone(), run.clone())
        .wait()
        .unwrap();
    assert_eq!(
        recovered
            .observe_diagnostic(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    recovered.cleanup_diagnostic(id.clone()).wait().unwrap();
    let replay = recovered.run_diagnostic(helper, run).wait().unwrap();
    assert_eq!(replay, id);
    assert_eq!(
        recovered
            .observe_diagnostic(replay)
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
}

#[test]
fn definitive_create_rejection_allows_recovery_and_a_corrected_retry() {
    let engine = Engine::new();
    engine.state.lock().unwrap().refuse_create = true;
    let client = engine.client();
    let (helper, run) = request();
    assert_eq!(
        client
            .run_diagnostic(helper.clone(), run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Engine
    );
    assert!(engine.state.lock().unwrap().body.is_none());
    client.recover_diagnostics().wait().unwrap();
    let id = client.run_diagnostic(helper, run).wait().unwrap();
    client.observe_diagnostic(id.clone()).wait().unwrap();
    client.cleanup_diagnostic(id).wait().unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 2);
}

#[test]
fn lost_stop_response_is_unknown_and_retry_reconciles_without_restart() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = request();
    let id = client.run_diagnostic(helper, run).wait().unwrap();
    engine.state.lock().unwrap().lose_stop = true;
    assert_eq!(
        client.stop_diagnostic(id.clone()).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    let retry = engine.client();
    retry.stop_diagnostic(id.clone()).wait().unwrap();
    assert_eq!(
        retry
            .observe_diagnostic(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    retry.cleanup_diagnostic(id).wait().unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
}

#[test]
fn lost_remove_response_recovers_absence_and_retains_final_evidence() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = request();
    let id = client.run_diagnostic(helper, run).wait().unwrap();
    let result = client.observe_diagnostic(id.clone()).wait().unwrap();
    engine.state.lock().unwrap().lose_remove = true;
    assert_eq!(
        client
            .cleanup_diagnostic(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert!(engine.state.lock().unwrap().body.is_none());
    let retry = engine.client();
    retry.recover_diagnostics().wait().unwrap();
    retry.cleanup_diagnostic(id.clone()).wait().unwrap();
    assert_eq!(retry.observe_diagnostic(id).wait().unwrap(), result);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn late_observer_cannot_resurrect_a_completed_cleanup_obligation() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = request();
    let id = client.run_diagnostic(helper, run).wait().unwrap();
    let gate = Arc::new(LogGate::default());
    engine.state.lock().unwrap().pause_first_log = Some(gate.clone());
    let observing = client.observe_diagnostic(id.clone());
    let until = std::time::Instant::now() + Duration::from_secs(1);
    while !gate.entered.load(Ordering::SeqCst) {
        assert!(
            std::time::Instant::now() < until,
            "observer must reach final log frame"
        );
        thread::sleep(Duration::from_millis(2));
    }
    client.cleanup_diagnostic(id.clone()).wait().unwrap();
    gate.released.store(true, Ordering::SeqCst);
    assert_eq!(observing.wait().unwrap().exit_code, Some(23));
    client.recover_diagnostics().wait().unwrap();
    client.cleanup_diagnostic(id).wait().unwrap();
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn lost_start_reconciles_original_exit_without_a_second_start() {
    let engine = Engine::new();
    {
        let mut s = engine.state.lock().unwrap();
        s.lose_start = true;
        s.keep_running = true;
    }
    let client = engine.client();
    let (helper, run) = request();
    assert_eq!(
        client
            .run_diagnostic(helper.clone(), run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine.finish();
    let retry = engine.client();
    let id = retry.run_diagnostic(helper, run).wait().unwrap();
    assert_eq!(
        retry
            .observe_diagnostic(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    retry.cleanup_diagnostic(id).wait().unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
}

#[test]
fn foreign_owner_replaced_id_and_unsafe_realized_security_block_mutations() {
    for fault in ["owner", "id", "privilege", "pid", "mount"] {
        let engine = Engine::new();
        let client = engine.client();
        let (helper, run) = request();
        let id = client.run_diagnostic(helper, run).wait().unwrap();
        let original = engine.state.lock().unwrap().body.clone();
        {
            let mut s = engine.state.lock().unwrap();
            match fault {
                "owner" => {
                    s.body.as_mut().unwrap()["Labels"]["io.quasar.agent-owner"] =
                        json!("foreign-node")
                }
                "id" => s.replace_id = true,
                "privilege" => s.body.as_mut().unwrap()["HostConfig"]["Privileged"] = json!(true),
                "pid" => {
                    s.body.as_mut().unwrap()["HostConfig"]["PidMode"] = json!("container:foreign")
                }
                "mount" => {
                    s.body.as_mut().unwrap()["HostConfig"]["Mounts"][0]["Source"] =
                        json!("/foreign/data")
                }
                _ => unreachable!(),
            }
        }
        assert!(
            client.stop_diagnostic(id.clone()).wait().is_err(),
            "{fault}"
        );
        assert!(
            client.cleanup_diagnostic(id.clone()).wait().is_err(),
            "{fault}"
        );
        assert_eq!(engine.requests("DELETE /containers/"), 0, "{fault}");
        assert_eq!(
            engine.requests(&format!("POST /containers/{ID}/stop")),
            0,
            "{fault}"
        );
        {
            let mut s = engine.state.lock().unwrap();
            s.body = original;
            s.replace_id = false;
        }
        client.cleanup_diagnostic(id).wait().unwrap();
    }
}

#[test]
fn foreign_name_collision_is_preserved_without_any_mutation() {
    let engine = Engine::new();
    {
        let mut s = engine.state.lock().unwrap();
        s.body = Some(json!({"Labels":{"io.quasar.agent-owner":"foreign"}}));
        s.name = "quasar-diagnostic-fixture".into();
    }
    let (helper, run) = request();
    assert_eq!(
        engine
            .client()
            .run_diagnostic(helper, run)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(engine.requests("POST /containers/"), 0);
    assert_eq!(engine.requests("DELETE /containers/"), 0);
}

#[test]
fn changed_request_node_or_endpoint_cannot_mutate_a_recorded_operation() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = request();
    let id = client
        .run_diagnostic(helper.clone(), run.clone())
        .wait()
        .unwrap();
    let mut changed = run;
    changed.command = vec!["different".into()];
    assert_eq!(
        client
            .run_diagnostic(helper, changed)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    for different_node in [true, false] {
        let mut config = engine.config.clone();
        if different_node {
            config.diagnostic_owner = Some("another-node".into());
        } else {
            config.socket = "/tmp/not-the-original-engine.sock".into();
        }
        let other = RuntimeClient::new(config).unwrap();
        assert_eq!(
            other
                .cleanup_diagnostic(id.clone())
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::UnknownOutcome
        );
        assert_eq!(
            other.stop_diagnostic(id.clone()).wait().unwrap_err().kind,
            ErrorKind::UnknownOutcome
        );
    }
    assert_eq!(engine.requests("DELETE /containers/"), 0);
    client.cleanup_diagnostic(id).wait().unwrap();
}

#[test]
fn missing_exit_stays_unknown_through_explicit_cleanup_and_replay() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = request();
    let id = client.run_diagnostic(helper, run).wait().unwrap();
    engine.finish();
    engine.state.lock().unwrap().exit = None;
    assert_eq!(
        client
            .observe_diagnostic(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(engine.requests("DELETE /containers/"), 0);
    client.cleanup_diagnostic(id.clone()).wait().unwrap();
    assert_eq!(
        client.observe_diagnostic(id).wait().unwrap().exit_code,
        None
    );
}

#[test]
fn fragmented_utf8_and_large_escaped_logs_remain_bounded_and_replayable() {
    let engine = Engine::new();
    engine.state.lock().unwrap().logs =
        vec![(1, vec![0xe2]), (1, vec![0x82, 0xac]), (2, vec![1; 5000])];
    let client = engine.client();
    let (helper, run) = request();
    let id = client.run_diagnostic(helper, run).wait().unwrap();
    let result = client.observe_diagnostic(id.clone()).wait().unwrap();
    assert_eq!(result.stdout, "€");
    assert_eq!(result.stderr.len(), 4096);
    client.cleanup_diagnostic(id.clone()).wait().unwrap();
    assert_eq!(client.observe_diagnostic(id).wait().unwrap(), result);
}

#[test]
fn cleanup_failures_and_log_failures_keep_the_container_until_retry() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = request();
    let id = client.run_diagnostic(helper, run).wait().unwrap();
    engine.state.lock().unwrap().lose_logs = true;
    assert!(client.cleanup_diagnostic(id.clone()).wait().is_err());
    assert_eq!(engine.requests("DELETE /containers/"), 0);
    assert!(engine.state.lock().unwrap().body.is_some());
    engine.state.lock().unwrap().refuse_remove = true;
    assert_eq!(
        client
            .cleanup_diagnostic(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert!(engine.state.lock().unwrap().body.is_some());
    let retry = engine.client();
    retry.recover_diagnostics().wait().unwrap();
    assert!(engine.state.lock().unwrap().body.is_none());
    assert_eq!(
        retry.observe_diagnostic(id).wait().unwrap().stdout,
        "final stdout"
    );
}

#[test]
fn observation_cancellation_does_not_stop_and_explicit_stop_can_interrupt_an_observer() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = request();
    let id = client.run_diagnostic(helper, run).wait().unwrap();
    let cancelled = client.observe_diagnostic(id.clone());
    cancelled.cancel();
    assert_eq!(cancelled.wait().unwrap_err().kind, ErrorKind::Cancelled);
    assert!(engine.state.lock().unwrap().running);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    let observing = client.observe_diagnostic(id.clone());
    client.stop_diagnostic(id.clone()).wait().unwrap();
    assert_eq!(observing.wait().unwrap().exit_code, Some(23));
    client.cleanup_diagnostic(id).wait().unwrap();
}

#[test]
fn apparently_missing_uncertain_create_never_authorizes_another_create() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let client = engine.client();
    let (helper, run) = request();
    assert_eq!(
        client
            .run_diagnostic(helper.clone(), run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine.state.lock().unwrap().body = None;
    assert_eq!(
        engine
            .client()
            .run_diagnostic(helper, run)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(
        client.recover_diagnostics().wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(engine.requests("POST /containers/create"), 1);
}

#[test]
fn accepted_requests_leave_room_for_durable_final_log_evidence() {
    let engine = Engine::new();
    engine.state.lock().unwrap().logs = vec![(1, vec![1; 4096]), (2, vec![2; 4096])];
    let client = engine.client();
    let (helper, mut run) = request();
    run.command = vec!["\u{1}".repeat(7000)];
    match client.run_diagnostic(helper, run).wait() {
        Err(error) => {
            assert_eq!(error.kind, ErrorKind::InvalidConfiguration);
            assert_eq!(engine.requests("POST /containers/create"), 0);
        }
        Ok(id) => {
            let result = client.observe_diagnostic(id.clone()).wait().unwrap();
            assert_eq!(result.exit_code, Some(23));
            client.cleanup_diagnostic(id.clone()).wait().unwrap();
            assert_eq!(client.observe_diagnostic(id).wait().unwrap(), result);
        }
    }
}

#[test]
fn lost_create_retry_can_start_the_verified_original_created_helper() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let client = engine.client();
    let (helper, run) = request();
    assert_eq!(
        client
            .run_diagnostic(helper.clone(), run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    let id = engine.client().run_diagnostic(helper, run).wait().unwrap();
    assert_eq!(
        client
            .observe_diagnostic(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    client.cleanup_diagnostic(id).wait().unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
}

#[test]
fn restart_retries_an_explicit_stop_intent_but_does_not_stop_unrequested_work() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = request();
    let id = client.run_diagnostic(helper, run).wait().unwrap();
    engine.state.lock().unwrap().lose_stop_before_effect = true;
    assert_eq!(
        client.stop_diagnostic(id.clone()).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    assert!(engine.state.lock().unwrap().running);
    drop(client);
    let restarted = engine.client();
    restarted.recover_diagnostics().wait().unwrap();
    assert!(engine.state.lock().unwrap().body.is_none());
    assert_eq!(
        restarted.observe_diagnostic(id).wait().unwrap().exit_code,
        Some(23)
    );
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 2);
    let untouched = Engine::new();
    untouched.state.lock().unwrap().keep_running = true;
    let observer = untouched.client();
    let (helper, run) = request();
    let id = observer.run_diagnostic(helper, run).wait().unwrap();
    assert!(observer.recover_diagnostics().wait().is_err());
    assert!(untouched.state.lock().unwrap().running);
    assert_eq!(
        untouched.requests(&format!("POST /containers/{ID}/stop")),
        0
    );
    untouched.finish();
    observer.cleanup_diagnostic(id).wait().unwrap();
}
