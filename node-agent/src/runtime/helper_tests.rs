//! Behavioral engine fixtures at the public RuntimeClient boundary.
use super::*;
use crate::runtime::helpers::{HelperPhase, HelperProfile};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
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
    oom_killed: bool,
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
    inspect_after_start_code: Option<u16>,
    pause_mutation_reply: Option<(String, Arc<LogGate>)>,
    host_mount_override: Option<Value>,
    host_config_patch: Option<Value>,
    realized_mount_override: Option<Value>,
    host_device_requests_override: Option<Value>,
    host_security_opt_override: Option<Value>,
    host_devices_override: Option<Value>,
    host_group_add_override: Option<Value>,
    config_entrypoint_override: Option<Value>,
    config_cmd_override: Option<Value>,
    config_user_override: Option<Value>,
    image_missing: bool,
    image_volumes: Option<Vec<String>>,
    exec_created: bool,
    exec_started: bool,
    exec_running: bool,
    lose_exec_create: bool,
    lose_exec_start: bool,
    lose_exec_start_before_effect: bool,
    exec_create_delay: Option<Duration>,
    exec_foreign: bool,
    exec_exit: Option<Option<i64>>,
    inherited_env: Vec<String>,
    requests: Vec<String>,
    pause_first_log: Option<Arc<LogGate>>,
    /// Pre-API siblings this engine knows about, visible only to the legacy
    /// boot sweep: a label listing, an inspect by immutable ID, a forced remove.
    legacy: Vec<LegacyContainer>,
    /// The listing fails, so the sweep can prove no mutation follows.
    refuse_legacy_list: bool,
    /// This legacy ID's DELETE reply is dropped WITHOUT applying the removal, so
    /// a pass that treated a lost reply as success would be caught by the next
    /// listing still returning it.
    lose_legacy_remove: Option<String>,
    /// This legacy ID's DELETE succeeds but the container is still there.
    legacy_remove_has_no_effect: Option<String>,
    /// The engine is unreachable: every request is accepted and then reset,
    /// which is what a restarting or wedged daemon looks like to the client.
    unreachable: bool,
}

/// One pre-API container as the fixture engine reports it.
#[derive(Clone)]
struct LegacyContainer {
    id: String,
    name: String,
    labels: Value,
    /// Removed by this fixture's engine: it disappears from the listing and
    /// inspects 404, which is the only proof of removal the sweep accepts.
    gone: bool,
}
fn legacy(id: char, name: &str, labels: Value) -> LegacyContainer {
    LegacyContainer {
        id: std::iter::repeat_n(id, 64).collect(),
        name: name.into(),
        labels,
        gone: false,
    }
}
fn owner_label(owner: &str) -> Value {
    json!({ crate::container_ownership::LABEL: owner })
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
    if s.unreachable {
        return;
    }
    let mut code = 200;
    let mut response = json!({});
    let mut raw = None;
    let mut gate = None;
    if route == "/version" {
        response = json!({"Platform":{"Name":"Docker"},"Version":"28.0.0","ApiVersion":"1.48","MinAPIVersion":"1.40"});
    } else if route == "/info" {
        // `inspect_engine`, which the runtime readiness checks call. Every field it
        // folds is optional, so an empty object is a complete answer.
        response = json!({});
    } else if method == "GET" && route.starts_with("/images/") && route.contains("/json") {
        if s.image_missing {
            code = 404;
            response = json!({"message":"missing"});
        } else {
            let volumes = s.image_volumes.as_ref().map(|targets| {
                Value::Object(
                    targets
                        .iter()
                        .map(|target| (target.clone(), json!({})))
                        .collect(),
                )
            });
            response = json!({"Id":"sha256:fixture-image","Config":{"Env":[],"Entrypoint":["/image-entry"],"Cmd":["image-command"],"User":null,"Volumes":volumes}});
        }
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
    } else if method == "POST" && route.starts_with("/containers/") && route.ends_with("/exec") {
        assert!(
            route.contains(ID),
            "exec must use verified immutable application ID"
        );
        assert_eq!(
            serde_json::from_slice::<Value>(&body).unwrap(),
            json!({"AttachStderr":false,"AttachStdout":false,"Privileged":true,"User":"root","Cmd":["umount","/proc/driver/nvidia/params"]})
        );
        assert!(!s.exec_created, "duplicate repair exec");
        s.exec_created = true;
        response = json!({"Id":"repair-exec"});
        if let Some(delay) = s.exec_create_delay {
            std::thread::sleep(delay);
        }
        if std::mem::take(&mut s.lose_exec_create) {
            return;
        }
    } else if method == "POST" && route == "/exec/repair-exec/start" {
        assert!(s.exec_created, "exec start without create");
        assert!(!s.exec_started, "duplicate repair exec start");
        if std::mem::take(&mut s.lose_exec_start_before_effect) {
            return;
        }
        s.exec_started = true;
        raw = Some(Vec::new());
        if std::mem::take(&mut s.lose_exec_start) {
            return;
        }
    } else if method == "GET" && route == "/exec/repair-exec/json" {
        response = json!({"ID":"repair-exec","ContainerID":if s.exec_foreign { "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" } else { ID },"Running":s.exec_running,"ExitCode":if s.exec_running || !s.exec_started { Value::Null } else { s.exec_exit.unwrap_or(Some(0)).map_or(Value::Null, Value::from) }});
    } else if method == "GET" && route.starts_with("/containers/json") {
        assert!(
            route.contains("all=true"),
            "the legacy sweep must see stopped containers too: {route}"
        );
        assert!(
            route.contains("label") && route.contains("agent-owner"),
            "the legacy listing must be filtered by this agent's owner label: {route}"
        );
        if s.refuse_legacy_list {
            code = 500;
            response = json!({"message":"fixture list failure"});
        } else {
            // Deliberately ignore the requested filter: the independent inspect
            // check must preserve foreign, unlabelled and substring-only names
            // even when the daemon answers with more than was asked for.
            response = Value::Array(
                s.legacy
                    .iter()
                    .filter(|container| !container.gone)
                    .map(|container| json!({"Id":container.id,"Names":[container.name.clone()]}))
                    .chain(
                        s.body
                            .is_some()
                            .then(|| json!({"Id":ID,"Names":[format!("/{}", s.name)]})),
                    )
                    .collect(),
            );
        }
    } else if method == "GET"
        && route.ends_with("/json")
        && s.legacy
            .iter()
            .any(|container| route.contains(&container.id))
    {
        let container = s
            .legacy
            .iter()
            .find(|container| route.contains(&container.id))
            .unwrap();
        if container.gone {
            code = 404;
            response = json!({"message":"No such container"});
        } else {
            response = json!({"Id":container.id,"Name":container.name,
                "Image":"sha256:fixture-image","Config":{"Labels":container.labels},
                "State":{"Running":false,"Status":"exited","ExitCode":0,"OOMKilled":false}});
        }
    } else if method == "DELETE"
        && s.legacy
            .iter()
            .any(|container| route.contains(&container.id))
    {
        let id = s
            .legacy
            .iter()
            .find(|container| route.contains(&container.id))
            .unwrap()
            .id
            .clone();
        assert!(
            route.contains("force=true") && route.contains("v=false"),
            "a legacy removal may force, but never deletes volumes: {route}"
        );
        code = 204;
        if s.lose_legacy_remove.as_deref() == Some(id.as_str()) {
            s.lose_legacy_remove = None;
            return;
        }
        if s.legacy_remove_has_no_effect.as_deref() != Some(id.as_str()) {
            for container in s.legacy.iter_mut().filter(|c| c.id == id) {
                container.gone = true;
            }
        }
    } else if method == "GET" && route.ends_with("/json") {
        if let Some(body) = &s.body {
            let mut mounts: Vec<Value> = body["HostConfig"]["Mounts"].as_array().into_iter().flatten().map(|mount| {
                let volume = mount["Type"].as_str() == Some("volume");
                json!({"Type":mount["Type"],"Source":mount["Source"],"Name":if volume { mount["Source"].clone() } else { Value::Null },"Destination":mount["Target"],"RW":!mount["ReadOnly"].as_bool().unwrap_or(false)})
            }).collect();
            // Docker inspect exposes legacy `-v` mounts only through Mounts;
            // Binds retains the requested suffixes but not the realized RW bit.
            mounts.extend(body["HostConfig"]["Binds"].as_array().into_iter().flatten().filter_map(|bind| {
                let bind = bind.as_str()?;
                let mut parts = bind.splitn(3, ':');
                let source = parts.next()?;
                let destination = parts.next()?;
                let read_only = parts.next().is_some_and(|options| options.split(',').any(|option| option == "ro" || option == "readonly"));
                let volume = !source.starts_with('/');
                Some(json!({"Type":if volume { "volume" } else { "bind" }, "Source":if volume { Value::Null } else { json!(source) }, "Name":if volume { json!(source) } else { Value::Null }, "Destination":destination,"RW":!read_only}))
            }));
            let explicit_targets = mounts
                .iter()
                .filter_map(|mount| mount["Destination"].as_str().map(str::to_owned))
                .collect::<Vec<_>>();
            mounts.extend(
                s.image_volumes
                    .iter()
                    .flatten()
                    .filter(|target| !explicit_targets.contains(*target))
                    .map(|target| json!({"Type":"volume","Source":format!("/var/lib/docker/volumes/fixture-{target}/_data"),"Name":format!("fixture-{target}"),"Destination":target,"RW":true})),
            );
            let realized_mounts = s
                .realized_mount_override
                .clone()
                .unwrap_or_else(|| Value::Array(mounts.clone()));
            let mut host_config = body["HostConfig"].clone();
            if let Some(mounts) = &s.host_mount_override {
                host_config["Mounts"] = mounts.clone();
            }
            if let Some(Value::Object(patch)) = &s.host_config_patch {
                for (key, value) in patch {
                    host_config[key] = value.clone();
                }
            }
            if let Some(requests) = &s.host_device_requests_override {
                host_config["DeviceRequests"] = requests.clone();
            }
            if let Some(security) = &s.host_security_opt_override {
                host_config["SecurityOpt"] = security.clone();
            }
            if let Some(devices) = &s.host_devices_override {
                host_config["Devices"] = devices.clone();
            }
            if let Some(groups) = &s.host_group_add_override {
                host_config["GroupAdd"] = groups.clone();
            }
            let mut config = body.clone();
            if config["Entrypoint"].is_null() {
                config["Entrypoint"] = json!(["/image-entry"]);
            }
            if config["Cmd"].is_null() {
                config["Cmd"] = json!(["image-command"]);
            }
            if config["User"].is_null() {
                config["User"] = json!("");
            }
            if let Some(user) = &s.config_user_override {
                config["User"] = user.clone();
            }
            if let Some(entrypoint) = &s.config_entrypoint_override {
                config["Entrypoint"] = entrypoint.clone();
            }
            if let Some(cmd) = &s.config_cmd_override {
                config["Cmd"] = cmd.clone();
            }
            if !s.inherited_env.is_empty() {
                let env = config["Env"].as_array_mut().unwrap();
                env.extend(s.inherited_env.iter().cloned().map(Value::String));
            }
            response = json!({"Id":if s.replace_id { "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" } else {ID},"Image":"sha256:fixture-image","Name":format!("/{}",s.name),"Config":config,"HostConfig":host_config,"Mounts":realized_mounts,"State":{"Running":s.running,"Status":if s.running {"running"} else if s.exited {"exited"} else {"created"},"ExitCode":s.exit,"OOMKilled":s.oom_killed}});
            if let Some(inspect_code) = s.inspect_code.or_else(|| {
                (s.running || s.exited)
                    .then_some(s.inspect_after_start_code)
                    .flatten()
            }) {
                code = inspect_code;
                response = json!({"message":"fixture inspect"});
            }
        } else {
            code = 404;
            response = json!({"message":"missing"});
        }
    } else if method == "POST" && route.contains("/start") {
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
        let tail = route
            .split("tail=")
            .nth(1)
            .and_then(|value| value.split('&').next())
            .and_then(|value| value.parse::<usize>().ok())
            .unwrap_or(usize::MAX);
        let first = s.logs.len().saturating_sub(tail);
        let mut bytes = Vec::new();
        for (kind, log) in &s.logs[first..] {
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
    let mutation_gate = if s
        .pause_mutation_reply
        .as_ref()
        .is_some_and(|(prefix, _)| format!("{method} {route}").starts_with(prefix))
    {
        s.pause_mutation_reply.take().map(|(_, gate)| gate)
    } else {
        None
    };
    drop(s);
    if let Some(gate) = mutation_gate {
        gate.entered.store(true, Ordering::SeqCst);
        let until = std::time::Instant::now() + Duration::from_secs(2);
        while !gate.released.load(Ordering::SeqCst) && std::time::Instant::now() < until {
            thread::sleep(Duration::from_millis(2));
        }
    }
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

/// The NVIDIA arm of the GPU probe profile, which absorbed the NVIDIA GPU
/// diagnostic (#258); these tests pin the behaviour that arm inherited.
fn nvidia_request() -> (DiagnosticHelper, GpuProbeRun) {
    nvidia_probe_request()
}

#[test]
fn application_failed_realization_retires_its_verified_created_id() {
    for rejected in ["mount", "device", "security"] {
        let engine = Engine::new();
        {
            let mut state = engine.state.lock().unwrap();
            match rejected {
                "mount" => state.realized_mount_override = Some(json!([])),
                "device" => state.host_devices_override = Some(json!([])),
                "security" => state.host_security_opt_override = Some(json!([])),
                _ => unreachable!(),
            }
        }
        let request = realized_requirements_request(&format!("rejected-{rejected}"));
        assert!(
            engine
                .client()
                .start_application(request.clone())
                .wait()
                .is_err(),
            "{rejected}"
        );
        assert_eq!(engine.requests("POST /containers/create"), 1, "{rejected}");
        assert_eq!(
            engine.requests(&format!("POST /containers/{ID}/start")),
            0,
            "{rejected}"
        );
        // The full ID was durably returned by this create. Refusing to run a
        // weakened request must not also refuse its ownership-verified cleanup.
        engine
            .client()
            .abandon_application(request.operation)
            .wait()
            .unwrap();
        assert_eq!(engine.requests("DELETE /containers/"), 1, "{rejected}");
        assert!(engine.state.lock().unwrap().body.is_none(), "{rejected}");
    }
}

#[test]
fn application_abandon_before_intent_is_a_noop_without_opening_docker_and_allows_retry() {
    let engine = Engine::new();
    let operation = "session-fixture-preflight-no-intent";
    let request = ApplicationRequest {
        operation: operation.into(),
        name: "quasar-sess-fixture-preflight-no-intent".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let mut unavailable = engine.config.clone();
    unavailable.socket = "/tmp/quasar-preflight-engine-unavailable.sock".into();
    let unavailable_client = RuntimeClient::new(unavailable).unwrap();
    assert!(unavailable_client
        .start_application(request.clone())
        .wait()
        .is_err());
    // Opening Docker failed before start could fsync a submission intent. Abandonment
    // is therefore a proven no-op and must not attempt a second Docker connection.
    unavailable_client
        .abandon_application(operation)
        .wait()
        .unwrap();
    assert!(engine.state.lock().unwrap().requests.is_empty());
    engine.client().start_application(request).wait().unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 1);
}

#[test]
fn saturated_application_admission_leaves_no_intent_so_abandon_releases_the_retry() {
    let engine = Engine::new();
    let gate = Arc::new(LogGate::default());
    engine.state.lock().unwrap().pause_mutation_reply =
        Some(("POST /containers/create".into(), gate.clone()));
    let mut config = engine.config.clone();
    config.max_in_flight = 1;
    let client = RuntimeClient::new(config).unwrap();
    let first = client.start_application(ApplicationRequest {
        operation: "application-admission-slot-first".into(),
        name: "quasar-sess-admission-slot-first".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    });
    wait_for_mutation_reply(&gate);
    let retry = ApplicationRequest {
        operation: "application-admission-slot-retry".into(),
        name: "quasar-sess-admission-slot-retry".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    assert_eq!(
        client
            .start_application(retry.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Busy,
        "a saturated executor must fail before this retry writes an application intent"
    );
    gate.released.store(true, Ordering::SeqCst);
    first.wait().unwrap();
    // The caller may retain a same-home retry gate for this Busy response. Once the
    // slot is free, a successful no-intent abandon proves that gate can be cleared.
    client
        .abandon_application(retry.operation.clone())
        .wait()
        .unwrap();
    assert_eq!(
        engine.requests("POST /containers/create"),
        1,
        "the Busy retry left no durable intent or daemon mutation behind"
    );
}

#[test]
fn application_abandon_with_a_corrupt_journal_stays_uncertain_without_opening_docker() {
    let engine = Engine::new();
    let operation = "session-fixture-corrupt-abandon";
    let key = Sha256::digest(operation.as_bytes())
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    let journal = engine
        .config
        .image_state_path
        .as_ref()
        .unwrap()
        .join("applications")
        .join(key);
    std::fs::create_dir_all(journal.parent().unwrap()).unwrap();
    std::fs::write(journal, b"not-json").unwrap();
    assert!(engine
        .client()
        .abandon_application(operation)
        .wait()
        .is_err());
    assert!(engine.state.lock().unwrap().requests.is_empty());
}

#[test]
fn application_abandon_with_an_inaccessible_journal_stays_uncertain_without_opening_docker() {
    let engine = Engine::new();
    let blocked = engine._dir.path().join("application-journal-file");
    std::fs::write(&blocked, b"not a directory").unwrap();
    let mut config = engine.config.clone();
    config.image_state_path = Some(blocked);
    assert!(RuntimeClient::new(config)
        .unwrap()
        .abandon_application("session-fixture-inaccessible-abandon")
        .wait()
        .is_err());
    assert!(engine.state.lock().unwrap().requests.is_empty());
}

#[test]
fn application_lost_create_cannot_adopt_a_name_with_rejected_requirements() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let request = realized_requirements_request("lost-create-rejected-device");
    assert_eq!(
        engine
            .client()
            .start_application(request.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine.state.lock().unwrap().host_devices_override = Some(json!([]));
    // A failed retry must not persist an ID learned only from an unverified
    // name lookup, then let cleanup treat it as create-response authority.
    assert!(engine
        .client()
        .start_application(request.clone())
        .wait()
        .is_err());
    assert!(engine
        .client()
        .abandon_application(request.operation)
        .wait()
        .is_err());
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 0);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    assert_eq!(engine.requests("DELETE /containers/"), 0);
}

#[test]
fn application_foreign_name_collision_never_authorizes_a_mutation() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.name = "quasar-sess-foreign-name".into();
        state.body = Some(json!({"Labels": {"io.quasar.agent-owner":"foreign-owner"}}));
    }
    let result = engine
        .client()
        .start_application(ApplicationRequest {
            operation: "foreign-name-collision".into(),
            name: "quasar-sess-foreign-name".into(),
            image: "quasar-app:test".into(),
            ..Default::default()
        })
        .wait();
    assert_eq!(result.unwrap_err().kind, ErrorKind::UnknownOutcome);
    assert_eq!(engine.requests("POST /containers/"), 0);
    assert_eq!(engine.requests("DELETE /containers/"), 0);
    assert!(engine.state.lock().unwrap().body.is_some());
}

#[test]
fn application_changed_ownership_or_id_refuses_stop_and_cleanup() {
    for changed in ["owner", "operation", "id"] {
        let engine = Engine::new();
        engine.state.lock().unwrap().keep_running = true;
        let id = engine
            .client()
            .start_application(ApplicationRequest {
                operation: format!("ownership-{changed}"),
                name: format!("quasar-sess-ownership-{changed}"),
                image: "quasar-app:test".into(),
                ..Default::default()
            })
            .wait()
            .unwrap();
        {
            let mut state = engine.state.lock().unwrap();
            match changed {
                "owner" => {
                    state.body.as_mut().unwrap()["Labels"][crate::container_ownership::LABEL] =
                        json!("foreign-owner")
                }
                "operation" => {
                    state.body.as_mut().unwrap()["Labels"]["io.quasar.application-operation"] =
                        json!("foreign-operation")
                }
                "id" => state.replace_id = true,
                _ => unreachable!(),
            }
        }
        assert_eq!(
            engine
                .client()
                .stop_application(id.clone(), Duration::from_secs(1))
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::UnknownOutcome,
            "{changed}"
        );
        assert_eq!(
            engine
                .client()
                .cleanup_application(id)
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::UnknownOutcome,
            "{changed}"
        );
        assert_eq!(
            engine.requests(&format!("POST /containers/{ID}/stop")),
            0,
            "{changed}"
        );
        assert_eq!(engine.requests("DELETE /containers/"), 0, "{changed}");
        assert!(engine.state.lock().unwrap().running, "{changed}");
    }
}

fn wait_for_mutation_reply(gate: &LogGate) {
    let until = std::time::Instant::now() + Duration::from_secs(1);
    while !gate.entered.load(Ordering::SeqCst) && std::time::Instant::now() < until {
        thread::sleep(Duration::from_millis(2));
    }
    assert!(
        gate.entered.load(Ordering::SeqCst),
        "mutation never reached daemon"
    );
}

#[test]
fn application_cancelled_create_or_start_observer_does_not_roll_back_or_duplicate() {
    for mutation in [
        "POST /containers/create".to_owned(),
        format!("POST /containers/{ID}/start"),
    ] {
        let engine = Engine::new();
        let gate = Arc::new(LogGate::default());
        {
            let mut state = engine.state.lock().unwrap();
            state.keep_running = true;
            state.pause_mutation_reply = Some((mutation.clone(), gate.clone()));
        }
        let request = ApplicationRequest {
            operation: "cancelled-launch".into(),
            name: "quasar-sess-cancelled-launch".into(),
            image: "quasar-app:test".into(),
            ..Default::default()
        };
        let client = engine.client();
        let cancelled = client.start_application(request.clone());
        wait_for_mutation_reply(&gate);
        cancelled.cancel();
        drop(cancelled);
        gate.released.store(true, Ordering::SeqCst);
        let id = engine.client().start_application(request).wait().unwrap();
        assert_eq!(id.as_str(), ID);
        assert!(engine.state.lock().unwrap().running);
        assert_eq!(engine.requests("POST /containers/create"), 1);
        assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
        assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
        assert_eq!(engine.requests("DELETE /containers/"), 0);
    }
}

#[test]
fn application_cancelled_stop_and_remove_observers_preserve_cleanup_and_evidence() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.keep_running = true;
        state.exit = Some(0);
    }
    let client = engine.client();
    let id = client
        .start_application(ApplicationRequest {
            operation: "cancelled-cleanup".into(),
            name: "quasar-sess-cancelled-cleanup".into(),
            image: "quasar-app:test".into(),
            ..Default::default()
        })
        .wait()
        .unwrap();
    let stop_gate = Arc::new(LogGate::default());
    engine.state.lock().unwrap().pause_mutation_reply =
        Some((format!("POST /containers/{ID}/stop"), stop_gate.clone()));
    let stop = client.stop_application(id.clone(), Duration::from_secs(1));
    wait_for_mutation_reply(&stop_gate);
    stop.cancel();
    drop(stop);
    stop_gate.released.store(true, Ordering::SeqCst);
    engine
        .client()
        .stop_application(id.clone(), Duration::from_secs(1))
        .wait()
        .unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);

    let remove_gate = Arc::new(LogGate::default());
    engine.state.lock().unwrap().pause_mutation_reply =
        Some((format!("DELETE /containers/{ID}"), remove_gate.clone()));
    let cleanup = client.cleanup_application(id.clone());
    wait_for_mutation_reply(&remove_gate);
    cleanup.cancel();
    drop(cleanup);
    remove_gate.released.store(true, Ordering::SeqCst);
    let restarted = engine.client();
    restarted.cleanup_application(id.clone()).wait().unwrap();
    let result = restarted.observe_application(id).wait().unwrap();
    assert_eq!(result.exit_code, Some(0));
    assert_eq!(result.oom_killed, Some(false));
    assert_eq!(result.stdout, "final stdout");
    assert_eq!(result.stderr, "final stderr");
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn application_lifecycle_creates_an_owned_container_without_auto_remove() {
    let engine = Engine::new();
    let request = ApplicationRequest {
        operation: "session-fixture-generation-1".into(),
        name: "quasar-sess-fixture-g1".into(),
        image: "quasar-app:test".into(),
        environment: vec!["PULSE_SERVER=unix:/run/pulse/native".into()],
        mounts: vec!["/daemon/session.sock:/run/user/1000/wayland-0:Z,cached".into()],
        devices: vec!["/dev/dri".into()],
        group_add: vec!["44".into()],
        gpu: true,
        nvidia_gpu: true,
        security: ApplicationSecurity {
            cap_add: vec!["SYS_NICE".into()],
            security_opt: vec!["seccomp=unconfined".into()],
            ..Default::default()
        },
        ..Default::default()
    };
    let id = engine.client().start_application(request).wait().unwrap();
    let body = engine.state.lock().unwrap().body.clone().unwrap();
    assert_eq!(
        body["Labels"][crate::container_ownership::LABEL],
        "fixture-owner"
    );
    assert_eq!(body["HostConfig"]["AutoRemove"], false);
    assert_eq!(body["HostConfig"]["NetworkMode"], "none");
    assert_eq!(
        body["HostConfig"]["Binds"][0],
        "/daemon/session.sock:/run/user/1000/wayland-0:Z,cached"
    );
    assert_eq!(body["HostConfig"]["DeviceRequests"][0]["Driver"], "nvidia");
    assert_eq!(id.as_str(), ID);
}

#[test]
fn application_starts_a_created_container_even_when_docker_reports_exit_zero() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        // Docker reports ExitCode=0 for a freshly created container. Status,
        // not that incidental field, decides whether start is still required.
        state.exit = Some(0);
        state.keep_running = true;
    }
    let id = engine
        .client()
        .start_application(ApplicationRequest {
            operation: "session-fixture-created-zero".into(),
            name: "quasar-sess-fixture-created-zero".into(),
            image: "quasar-app:test".into(),
            ..Default::default()
        })
        .wait()
        .unwrap();
    assert_eq!(id.as_str(), ID);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
}

#[test]
fn application_verifies_inherited_image_entrypoint_command_and_user() {
    let engine = Engine::new();
    let request = ApplicationRequest {
        operation: "session-fixture-image-defaults".into(),
        name: "quasar-sess-fixture-image-defaults".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    engine
        .client()
        .start_application(request.clone())
        .wait()
        .unwrap();
    let rejected_entrypoint = Engine::new();
    rejected_entrypoint
        .state
        .lock()
        .unwrap()
        .config_entrypoint_override = Some(json!(["/changed-entrypoint"]));
    assert_eq!(
        rejected_entrypoint
            .client()
            .start_application(ApplicationRequest {
                operation: "session-fixture-image-entrypoint-bad".into(),
                name: "quasar-sess-fixture-image-entrypoint-bad".into(),
                image: "quasar-app:test".into(),
                ..Default::default()
            })
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
    let rejected = Engine::new();
    rejected.state.lock().unwrap().config_cmd_override = Some(json!(["changed"]));
    assert_eq!(
        rejected
            .client()
            .start_application(ApplicationRequest {
                operation: "session-fixture-image-defaults-bad".into(),
                name: "quasar-sess-fixture-image-defaults-bad".into(),
                ..request
            })
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
    let rejected_user = Engine::new();
    rejected_user.state.lock().unwrap().config_user_override = Some(json!("1001"));
    assert_eq!(
        rejected_user
            .client()
            .start_application(ApplicationRequest {
                operation: "session-fixture-image-user-bad".into(),
                name: "quasar-sess-fixture-image-user-bad".into(),
                image: "quasar-app:test".into(),
                ..Default::default()
            })
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
}

fn realized_requirements_request(operation: &str) -> ApplicationRequest {
    ApplicationRequest {
        operation: operation.into(),
        name: format!("quasar-sess-{operation}"),
        image: "quasar-app:test".into(),
        entrypoint: Some(vec!["/init".into()]),
        command: vec!["game".into(), "--safe".into()],
        environment: vec!["PULSE_SERVER=unix:/run/pulse/native".into()],
        mounts: vec!["/host/home:/home/quasar:Z,nocopy".into()],
        typed_mounts: vec![ApplicationMount::Bind {
            source: "/host/wayland".into(),
            target: "/run/wayland-0".into(),
            read_only: true,
            consistency: None,
        }],
        devices: vec!["/dev/dri/renderD128".into()],
        group_add: vec!["44".into()],
        nvidia_gpu: true,
        security: ApplicationSecurity {
            cap_add: vec!["SYS_NICE".into()],
            no_new_privileges: false,
            security_opt: vec!["seccomp=unconfined".into()],
            ..Default::default()
        },
        ..Default::default()
    }
}

#[test]
fn application_rejects_mismatched_realized_mount_device_and_security_requirements() {
    type RealizationCase = (&'static str, Box<dyn Fn(&mut State)>);
    let cases: Vec<RealizationCase> = vec![
        (
            "mount",
            Box::new(|s| s.realized_mount_override = Some(json!([]))),
        ),
        (
            "device",
            Box::new(|s| s.host_devices_override = Some(json!([]))),
        ),
        (
            "security",
            Box::new(|s| s.host_security_opt_override = Some(json!([]))),
        ),
        (
            "group",
            Box::new(|s| s.host_group_add_override = Some(json!([]))),
        ),
        (
            "nvidia",
            Box::new(|s| s.host_device_requests_override = Some(json!([]))),
        ),
        (
            "entrypoint",
            Box::new(|s| s.config_entrypoint_override = Some(json!(["/wrong"]))),
        ),
        (
            "command",
            Box::new(|s| s.config_cmd_override = Some(json!(["wrong"]))),
        ),
    ];
    for (kind, change) in cases {
        let engine = Engine::new();
        change(&mut engine.state.lock().unwrap());
        assert_eq!(
            engine
                .client()
                .start_application(realized_requirements_request(&format!("fixture-{kind}")))
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::Protocol,
            "{kind} realization must be rejected"
        );
    }
}

#[test]
fn application_accepts_normalized_typed_mount_defaults() {
    let engine = Engine::new();
    engine.state.lock().unwrap().host_mount_override = Some(json!([{
        "Type":"bind", "Source":"/host/wayland", "Target":"/run/wayland-0",
        "ReadOnly":true,
        "BindOptions":{"CreateMountpoint":false,"NonRecursive":false,
            "ReadOnlyNonRecursive":false,"ReadOnlyForceRecursive":false,
            "Propagation":"rprivate"}
    }]));
    engine.state.lock().unwrap().host_config_patch = Some(json!({
        "CapAdd":["CAP_SYS_NICE"], "CapDrop":["CAP_ALL"]
    }));
    assert!(engine
        .client()
        .start_application(realized_requirements_request("fixture-mount-defaults"))
        .wait()
        .is_ok());
}

#[test]
fn application_rejects_weakened_typed_and_legacy_mount_realization() {
    let typed_cases: Vec<(&str, Value)> = vec![
        (
            "create",
            json!([{"Type":"bind","Source":"/host/wayland","Target":"/run/wayland-0","ReadOnly":true,"BindOptions":{"CreateMountpoint":true}}]),
        ),
        (
            "consistency",
            json!([{"Type":"bind","Source":"/host/wayland","Target":"/run/wayland-0","ReadOnly":true,"Consistency":"delegated","BindOptions":{"CreateMountpoint":false}}]),
        ),
        (
            "propagation",
            json!([{"Type":"bind","Source":"/host/wayland","Target":"/run/wayland-0","ReadOnly":true,"BindOptions":{"CreateMountpoint":false,"Propagation":"rshared"}}]),
        ),
    ];
    for (kind, mounts) in typed_cases {
        let engine = Engine::new();
        engine.state.lock().unwrap().host_mount_override = Some(mounts);
        assert_eq!(
            engine
                .client()
                .start_application(realized_requirements_request(&format!(
                    "fixture-typed-{kind}"
                )))
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::Protocol
        );
    }

    let engine = Engine::new();
    engine.state.lock().unwrap().realized_mount_override = Some(json!([
        {"Type":"bind","Source":"/host/wayland","Name":null,"Destination":"/run/wayland-0","RW":false},
        {"Type":"bind","Source":"/foreign/home","Name":null,"Destination":"/home/quasar","RW":true}
    ]));
    assert_eq!(
        engine
            .client()
            .start_application(realized_requirements_request("fixture-legacy-realized"))
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
}

#[test]
fn application_rejects_weakened_volume_nocopy_and_unexpected_host_grants() {
    let volume_request = ApplicationRequest {
        operation: "fixture-volume-nocopy".into(),
        name: "quasar-sess-fixture-volume-nocopy".into(),
        image: "quasar-app:test".into(),
        typed_mounts: vec![ApplicationMount::Volume {
            source: "quasar-driver".into(),
            target: "/opt/quasar-driver".into(),
            read_only: true,
            no_copy: true,
        }],
        ..Default::default()
    };
    let engine = Engine::new();
    engine.state.lock().unwrap().host_mount_override = Some(json!([{
        "Type":"volume", "Source":"quasar-driver", "Target":"/opt/quasar-driver",
        "ReadOnly":true, "VolumeOptions":{"NoCopy":false}
    }]));
    assert_eq!(
        engine
            .client()
            .start_application(volume_request)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );

    for (kind, patch) in [
        ("privileged", json!({"Privileged":true})),
        ("pid", json!({"PidMode":"host"})),
        ("ipc", json!({"IpcMode":"host"})),
        ("uts", json!({"UTSMode":"host"})),
        ("userns", json!({"UsernsMode":"host"})),
        ("cgroupns", json!({"CgroupnsMode":"host"})),
        ("network", json!({"NetworkMode":"host"})),
        ("runtime", json!({"Runtime":"foreign-runtime"})),
        (
            "extra-gpu",
            json!({"DeviceRequests":[
                {"Driver":"nvidia","Count":-1,"Capabilities":[["gpu"]]},
                {"Driver":"nvidia","Count":1,"Capabilities":[["gpu"]]}
            ]}),
        ),
    ] {
        let engine = Engine::new();
        engine.state.lock().unwrap().host_config_patch = Some(patch);
        assert_eq!(
            engine
                .client()
                .start_application(realized_requirements_request(&format!(
                    "fixture-host-{kind}"
                )))
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::Protocol,
            "{kind}"
        );
    }
}

#[test]
fn application_rejects_duplicate_lexical_mount_targets_before_create() {
    let engine = Engine::new();
    let request = ApplicationRequest {
        operation: "fixture-duplicate-target".into(),
        name: "quasar-sess-fixture-duplicate-target".into(),
        image: "quasar-app:test".into(),
        typed_mounts: vec![
            ApplicationMount::Bind {
                source: "/host/one".into(),
                target: "/same".into(),
                read_only: true,
                consistency: None,
            },
            ApplicationMount::Volume {
                source: "named".into(),
                target: "/same/.".into(),
                read_only: true,
                no_copy: true,
            },
        ],
        ..Default::default()
    };
    assert_eq!(
        engine
            .client()
            .start_application(request)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::InvalidConfiguration
    );
    assert!(engine.state.lock().unwrap().body.is_none());
}

#[test]
fn application_accepts_image_declared_volumes_and_binds_their_identity() {
    let engine = Engine::new();
    engine.state.lock().unwrap().image_volumes = Some(vec!["/image-storage".into()]);
    let request = ApplicationRequest {
        operation: "fixture-image-volume".into(),
        name: "quasar-sess-fixture-image-volume".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    engine
        .client()
        .start_application(request.clone())
        .wait()
        .unwrap();
    engine.state.lock().unwrap().realized_mount_override = Some(json!([{
        "Type":"volume", "Source":"/var/lib/docker/volumes/foreign/_data",
        "Name":"foreign", "Destination":"/image-storage", "RW":true
    }]));
    assert_eq!(
        engine
            .client()
            .start_application(request)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
}

#[test]
fn application_rejects_unwritable_or_unidentified_image_declared_volume() {
    for (kind, mount) in [
        (
            "readonly",
            json!({"Type":"volume","Source":"/var/lib/docker/volumes/fixture/_data","Name":"fixture","Destination":"/image-storage","RW":false}),
        ),
        (
            "unknown-identity",
            json!({"Type":"volume","Source":null,"Name":null,"Destination":"/image-storage","RW":true}),
        ),
    ] {
        let engine = Engine::new();
        let mut state = engine.state.lock().unwrap();
        state.image_volumes = Some(vec!["/image-storage".into()]);
        state.realized_mount_override = Some(json!([mount]));
        drop(state);
        let request = ApplicationRequest {
            operation: format!("fixture-image-volume-{kind}"),
            name: format!("quasar-sess-fixture-image-volume-{kind}"),
            image: "quasar-app:test".into(),
            ..Default::default()
        };
        assert_eq!(
            engine
                .client()
                .start_application(request)
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::Protocol,
            "{kind}"
        );
    }
}

#[test]
fn application_explicit_mount_overrides_image_declared_volume_and_rejects_foreign_extra() {
    let request = ApplicationRequest {
        operation: "fixture-image-volume-override".into(),
        name: "quasar-sess-fixture-image-volume-override".into(),
        image: "quasar-app:test".into(),
        typed_mounts: vec![ApplicationMount::Volume {
            source: "quasar-owned".into(),
            target: "/image-storage".into(),
            read_only: true,
            no_copy: true,
        }],
        ..Default::default()
    };
    let engine = Engine::new();
    engine.state.lock().unwrap().image_volumes = Some(vec!["/image-storage".into()]);
    assert!(engine.client().start_application(request).wait().is_ok());

    let foreign = Engine::new();
    foreign.state.lock().unwrap().image_volumes = Some(vec!["/image-storage".into()]);
    foreign.state.lock().unwrap().realized_mount_override = Some(json!([{
        "Type":"volume", "Source":"/var/lib/docker/volumes/foreign/_data",
        "Name":"foreign", "Destination":"/not-image-storage", "RW":true
    }]));
    let request = ApplicationRequest {
        operation: "fixture-image-volume-foreign".into(),
        name: "quasar-sess-fixture-image-volume-foreign".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    assert_eq!(
        foreign
            .client()
            .start_application(request)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
}

#[test]
fn application_normalizes_mount_paths_without_accepting_a_wrong_target() {
    let request = ApplicationRequest {
        operation: "fixture-mount-path".into(),
        name: "quasar-sess-fixture-mount-path".into(),
        image: "quasar-app:test".into(),
        typed_mounts: vec![ApplicationMount::Bind {
            source: "/host/wayland/.".into(),
            target: "/run/wayland-0/".into(),
            read_only: true,
            consistency: None,
        }],
        ..Default::default()
    };
    let engine = Engine::new();
    engine.state.lock().unwrap().realized_mount_override = Some(json!([{
        "Type":"bind", "Source":"/host/wayland", "Name":null,
        "Destination":"/run/wayland-0", "RW":false
    }]));
    assert!(engine
        .client()
        .start_application(request.clone())
        .wait()
        .is_ok());

    let wrong = Engine::new();
    wrong.state.lock().unwrap().realized_mount_override = Some(json!([{
        "Type":"bind", "Source":"/host/wayland", "Name":null,
        "Destination":"/run/other", "RW":false
    }]));
    let request = ApplicationRequest {
        operation: "fixture-mount-path-wrong".into(),
        name: "quasar-sess-fixture-mount-path-wrong".into(),
        ..request
    };
    assert_eq!(
        wrong
            .client()
            .start_application(request)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
}

#[test]
fn application_rejects_invalid_typed_bind_source_before_create() {
    let engine = Engine::new();
    let request = ApplicationRequest {
        operation: "fixture-invalid-bind-source".into(),
        name: "quasar-sess-fixture-invalid-bind-source".into(),
        image: "quasar-app:test".into(),
        typed_mounts: vec![ApplicationMount::Bind {
            source: "relative-source".into(),
            target: "/target".into(),
            read_only: true,
            consistency: None,
        }],
        ..Default::default()
    };
    assert_eq!(
        engine
            .client()
            .start_application(request)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::InvalidConfiguration
    );
    assert!(engine.state.lock().unwrap().body.is_none());
}

fn nvidia_params_repair_request(operation: &str) -> ApplicationRequest {
    ApplicationRequest {
        operation: operation.into(),
        name: format!("quasar-sess-{operation}"),
        image: "quasar-app:test".into(),
        nvidia_gpu: true,
        unmount_nvidia_params: true,
        ..Default::default()
    }
}

#[test]
fn application_nvidia_params_repair_uses_the_owned_api_exec_and_completes() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let id = engine
        .client()
        .start_application(nvidia_params_repair_request("fixture-nvidia-repair"))
        .wait()
        .unwrap();
    assert_eq!(id.as_str(), ID);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/exec")), 1);
    assert_eq!(engine.requests("POST /exec/repair-exec/start"), 1);
    assert_eq!(engine.requests("GET /exec/repair-exec/json"), 2);
}

#[test]
fn application_systempaths_unconfined_uses_realized_path_lists_not_securityopt() {
    let engine = Engine::new();
    let mut request = nvidia_params_repair_request("fixture-systempaths");
    request.unmount_nvidia_params = false;
    request.security.security_opt = vec!["seccomp=unconfined".into()];
    request.security.systempaths_unconfined = true;
    engine.client().start_application(request).wait().unwrap();
    let body = engine.state.lock().unwrap().body.clone().unwrap();
    assert_eq!(
        body["HostConfig"]["SecurityOpt"],
        json!(["seccomp=unconfined"])
    );
    assert_eq!(body["HostConfig"]["MaskedPaths"], json!([]));
    assert_eq!(body["HostConfig"]["ReadonlyPaths"], json!([]));

    let rejected = Engine::new();
    rejected.state.lock().unwrap().host_config_patch = Some(json!({
        "MaskedPaths":["/proc/acpi"], "ReadonlyPaths":["/proc/asound"]
    }));
    let mut request = nvidia_params_repair_request("fixture-systempaths-rejected");
    request.unmount_nvidia_params = false;
    request.security.systempaths_unconfined = true;
    assert_eq!(
        rejected
            .client()
            .start_application(request)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
}

#[test]
fn application_nvidia_params_repair_reconciles_lost_exec_replies_without_duplicates() {
    for (kind, create_lost, start_lost) in [("create", true, false), ("start", false, true)] {
        let engine = Engine::new();
        {
            let mut state = engine.state.lock().unwrap();
            state.keep_running = true;
            state.lose_exec_create = create_lost;
            state.lose_exec_start = start_lost;
        }
        let request = nvidia_params_repair_request(&format!("fixture-nvidia-repair-lost-{kind}"));
        assert!(engine
            .client()
            .start_application(request.clone())
            .wait()
            .is_ok());
        assert!(engine.client().start_application(request).wait().is_ok());
        assert_eq!(
            engine.requests(&format!("POST /containers/{ID}/exec")),
            1,
            "{kind}"
        );
        assert_eq!(
            engine.requests("POST /exec/repair-exec/start"),
            if create_lost { 0 } else { 1 },
            "{kind}"
        );
    }
}

#[test]
fn application_nvidia_params_repair_never_recreates_a_crash_gap_exec() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let request = nvidia_params_repair_request("fixture-nvidia-repair-crash-gap");
    engine
        .client()
        .start_application(request.clone())
        .wait()
        .unwrap();
    let key = Sha256::digest(request.operation.as_bytes())
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    let path = engine
        .config
        .image_state_path
        .as_ref()
        .unwrap()
        .join("applications")
        .join(key);
    let mut journal: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    journal["nvidia_params_repair"] = json!({"attempted":true,"exec_id":null,"start_attempted":false,"completed":false,"outcome":null});
    std::fs::write(path, serde_json::to_vec(&journal).unwrap()).unwrap();
    engine.client().start_application(request).wait().unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/exec")), 1);
}

#[test]
fn application_nvidia_params_repair_allows_a_busy_daemon_response() {
    let mut engine = Engine::new();
    engine.config.deadline = Duration::from_secs(8);
    {
        let mut state = engine.state.lock().unwrap();
        state.keep_running = true;
        state.exec_create_delay = Some(Duration::from_millis(250));
    }
    assert!(engine
        .client()
        .start_application(nvidia_params_repair_request(
            "fixture-nvidia-repair-delayed"
        ))
        .wait()
        .is_ok());
}

#[test]
fn application_nvidia_params_repair_does_not_complete_or_restart_unstarted_exec() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.keep_running = true;
        state.lose_exec_start_before_effect = true;
        state.exec_exit = Some(None);
    }
    let request = nvidia_params_repair_request("fixture-nvidia-repair-before-effect");
    assert!(engine
        .client()
        .start_application(request.clone())
        .wait()
        .is_ok());
    assert!(engine.client().start_application(request).wait().is_ok());
    assert_eq!(engine.requests("POST /exec/repair-exec/start"), 1);
}

#[test]
fn application_nvidia_params_repair_refuses_a_foreign_exec_parent() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.keep_running = true;
        state.exec_foreign = true;
    }
    assert!(engine
        .client()
        .start_application(nvidia_params_repair_request(
            "fixture-nvidia-repair-foreign"
        ))
        .wait()
        .is_ok());
    assert_eq!(engine.requests("POST /exec/repair-exec/start"), 0);
}

#[test]
fn application_nvidia_params_repair_timeout_keeps_the_running_handle() {
    let mut engine = Engine::new();
    engine.config.deadline = Duration::from_secs(8);
    {
        let mut state = engine.state.lock().unwrap();
        state.keep_running = true;
        state.exec_running = true;
    }
    let started = std::time::Instant::now();
    let id = engine
        .client()
        .start_application(nvidia_params_repair_request("fixture-nvidia-repair-hung"))
        .wait()
        .unwrap();
    assert_eq!(id.as_str(), ID);
    assert!(started.elapsed() < Duration::from_secs(6));
}

#[test]
fn application_running_recovery_attempts_an_unrecorded_nvidia_repair() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.running = true;
        state.keep_running = true;
    }
    assert!(engine
        .client()
        .start_application(nvidia_params_repair_request(
            "fixture-nvidia-repair-recovered"
        ))
        .wait()
        .is_ok());
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/exec")), 1);
}

#[test]
fn application_rejects_an_environment_value_shadowed_by_the_image() {
    let engine = Engine::new();
    engine
        .state
        .lock()
        .unwrap()
        .inherited_env
        .push("PULSE_SERVER=unix:/wrong".into());
    assert_eq!(
        engine
            .client()
            .start_application(realized_requirements_request("fixture-shadowed-env"))
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
}

#[test]
fn application_reconciles_a_lost_create_reply_without_a_second_create() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let request = ApplicationRequest {
        operation: "session-fixture-lost-create".into(),
        name: "quasar-sess-fixture-lost-create".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    assert_eq!(
        engine
            .client()
            .start_application(request.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine.state.lock().unwrap().image_missing = true;
    let id = engine.client().start_application(request).wait().unwrap();
    assert_eq!(id.as_str(), ID);
    assert_eq!(engine.requests("POST /containers/create"), 1);
}

#[test]
fn application_cleanup_recovers_a_lost_remove_reply_without_losing_final_logs() {
    let engine = Engine::new();
    engine.state.lock().unwrap().oom_killed = true;
    let request = ApplicationRequest {
        operation: "session-fixture-lost-remove".into(),
        name: "quasar-sess-fixture-lost-remove".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let id = engine.client().start_application(request).wait().unwrap();
    engine.state.lock().unwrap().lose_remove = true;
    assert_eq!(
        engine
            .client()
            .cleanup_application(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    // A new client has no in-memory result. The only evidence is the fsynced
    // operation journal: Docker already removed this exact container.
    let restarted = engine.client();
    let evidence = restarted.observe_application(id.clone()).wait().unwrap();
    assert_eq!(evidence.exit_code, Some(23));
    assert_eq!(evidence.oom_killed, Some(true));
    assert_eq!(evidence.stdout, "final stdout");
    assert_eq!(evidence.stderr, "final stderr");
    restarted.cleanup_application(id.clone()).wait().unwrap();
    assert_eq!(restarted.observe_application(id).wait().unwrap(), evidence);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn application_cleanup_retries_after_a_runtime_restart_without_touching_active_work() {
    let engine = Engine::new();
    let request = ApplicationRequest {
        operation: "session-fixture-restart".into(),
        name: "quasar-sess-fixture-restart".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let id = engine.client().start_application(request).wait().unwrap();
    engine.state.lock().unwrap().lose_remove = true;
    assert_eq!(
        engine
            .client()
            .cleanup_application(id)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    RuntimeClient::new(engine.config.clone())
        .unwrap()
        .recover_application_cleanup()
        .wait()
        .unwrap();
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn application_lost_start_reconciles_the_same_operation_without_a_second_start() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.lose_start = true;
        state.keep_running = true;
    }
    let request = ApplicationRequest {
        operation: "session-fixture-lost-start".into(),
        name: "quasar-sess-fixture-lost-start".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    // A dropped reply after Docker mutated the verified ID is successful once
    // reinspection proves the operation is running.
    let id = engine
        .client()
        .start_application(request.clone())
        .wait()
        .unwrap();
    assert_eq!(id.as_str(), ID);
    // A retry must reconcile the journaled pinned image and operation; the
    // mutable tag may have disappeared after Docker accepted the first start.
    engine.state.lock().unwrap().image_missing = true;
    let retry = engine.client().start_application(request).wait().unwrap();
    assert_eq!(retry.as_str(), ID);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
}

#[test]
fn application_recovers_post_start_inspection_failure_under_the_original_operation() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.inspect_after_start_code = Some(500);
        state.keep_running = true;
    }
    let request = ApplicationRequest {
        operation: "session-fixture-inspect-failure".into(),
        name: "quasar-sess-fixture-inspect-failure".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    assert_eq!(
        engine
            .client()
            .start_application(request.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
    engine.state.lock().unwrap().inspect_after_start_code = None;
    let restarted = engine.client();
    let id = restarted.start_application(request.clone()).wait().unwrap();
    assert_eq!(id.as_str(), ID);
    restarted
        .abandon_application(request.operation)
        .wait()
        .unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
    assert_eq!(
        restarted.observe_application(id).wait().unwrap().stdout,
        "final stdout"
    );
}

#[test]
fn application_observation_cancellation_never_stops_a_live_application() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let request = ApplicationRequest {
        operation: "session-fixture-observe-cancel".into(),
        name: "quasar-sess-fixture-observe-cancel".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let id = engine.client().start_application(request).wait().unwrap();
    let observation = engine.client().observe_application(id);
    observation.cancel();
    assert_eq!(observation.wait().unwrap_err().kind, ErrorKind::Cancelled);
    assert!(engine.state.lock().unwrap().running);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
}

#[test]
fn application_readiness_log_tail_observes_a_running_application_without_stopping_it() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.keep_running = true;
        state.logs = vec![(1, b"RH01-236-readiness-marker".to_vec())];
    }
    let request = ApplicationRequest {
        operation: "session-fixture-readiness-tail".into(),
        name: "quasar-sess-fixture-readiness-tail".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let client = engine.client();
    let id = client.start_application(request).wait().unwrap();
    assert!(client
        .application_log_tail(id)
        .wait()
        .unwrap()
        .stdout
        .contains("RH01-236-readiness-marker"));
    assert!(engine.state.lock().unwrap().running);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
}

#[test]
fn application_stop_proves_exit_then_preserves_final_oom_tail_before_cleanup() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let request = ApplicationRequest {
        operation: "session-fixture-stop-tail".into(),
        name: "quasar-sess-fixture-stop-tail".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let client = engine.client();
    let id = client.start_application(request).wait().unwrap();
    {
        let mut state = engine.state.lock().unwrap();
        state.oom_killed = true;
        state.logs = vec![(1, vec![b'x'; 20 * 1024]), (1, b"RH01-final-tail".to_vec())];
    }
    client
        .stop_application(id.clone(), Duration::from_secs(1))
        .wait()
        .unwrap();
    let result = client.observe_application(id.clone()).wait().unwrap();
    assert_eq!(result.exit_code, Some(23));
    assert_eq!(result.oom_killed, Some(true));
    assert!(result.stdout.contains("RH01-final-tail"));
    client.cleanup_application(id).wait().unwrap();
}

#[test]
fn application_logs_request_a_bounded_recent_tail_and_keep_final_exit_evidence() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.logs = (0..250)
            .map(|line| (1, format!("old-history-{line}\n").into_bytes()))
            .chain(std::iter::once((2, b"RH01-236-final-stderr".to_vec())))
            .collect();
    }
    let request = ApplicationRequest {
        operation: "session-fixture-bounded-final-tail".into(),
        name: "quasar-sess-fixture-bounded-final-tail".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let client = engine.client();
    let id = client.start_application(request).wait().unwrap();
    let result = client.observe_application(id.clone()).wait().unwrap();
    assert_eq!(result.exit_code, Some(23));
    assert!(result.stderr.contains("RH01-236-final-stderr"));
    assert!(!result.stdout.contains("old-history-0\n"));
    assert!(result.stdout.contains("old-history-249\n"));
    let requests = engine.state.lock().unwrap().requests.clone();
    assert!(requests.iter().any(|route| route.contains("/logs?")));
    assert!(!requests
        .iter()
        .any(|route| route.contains("/logs?") && route.contains("tail=all")));
    client.cleanup_application(id).wait().unwrap();
}

#[test]
fn application_escaped_final_logs_fit_the_durable_journal_without_losing_the_tail() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.logs = vec![
            (1, vec![1; 20 * 1024]),
            (2, vec![2; 20 * 1024]),
            (1, b"RH01-escaped-final-tail".to_vec()),
        ];
    }
    let request = ApplicationRequest {
        operation: "session-fixture-escaped-tail".into(),
        name: "quasar-sess-fixture-escaped-tail".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let id = engine.client().start_application(request).wait().unwrap();
    let result = engine.client().observe_application(id).wait().unwrap();
    assert!(result.stdout.contains("RH01-escaped-final-tail"));
    let journal = std::fs::read_dir(
        engine
            .config
            .image_state_path
            .as_ref()
            .unwrap()
            .join("applications"),
    )
    .unwrap()
    .filter_map(Result::ok)
    .map(|entry| std::fs::metadata(entry.path()).unwrap().len())
    .max()
    .unwrap();
    assert!(journal <= 64 * 1024);
}

#[test]
fn application_startup_retirement_reconciles_lost_create_without_adopting_active_records() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let request = ApplicationRequest {
        operation: "session-fixture-retire-partial".into(),
        name: "quasar-sess-fixture-retire-partial".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    assert_eq!(
        engine
            .client()
            .start_application(request)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    RuntimeClient::new(engine.config.clone())
        .unwrap()
        .retire_applications()
        .wait()
        .unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn application_verified_terminal_without_exit_code_is_persisted_replayed_and_cleaned() {
    let engine = Engine::new();
    engine.state.lock().unwrap().exit = None;
    let request = ApplicationRequest {
        operation: "session-fixture-unknown-exit".into(),
        name: "quasar-sess-fixture-unknown-exit".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let id = engine.client().start_application(request).wait().unwrap();
    let result = engine
        .client()
        .observe_application(id.clone())
        .wait()
        .unwrap();
    assert_eq!(result.exit_code, None);
    assert_eq!(result.oom_killed, Some(false));
    assert_eq!(
        engine
            .client()
            .observe_application(id.clone())
            .wait()
            .unwrap(),
        result
    );
    engine.client().cleanup_application(id).wait().unwrap();
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn application_lost_stop_reply_retries_the_same_durable_stopping_intent() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.keep_running = true;
        state.lose_stop = true;
    }
    let request = ApplicationRequest {
        operation: "session-fixture-lost-stop".into(),
        name: "quasar-sess-fixture-lost-stop".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let client = engine.client();
    let id = client.start_application(request).wait().unwrap();
    assert_eq!(
        client
            .stop_application(id.clone(), Duration::from_secs(1))
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    client
        .stop_application(id.clone(), Duration::from_secs(1))
        .wait()
        .unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    client.cleanup_application(id).wait().unwrap();
}

#[test]
fn application_stop_preserves_completed_and_delegates_cleanup_pending() {
    let engine = Engine::new();
    let request = ApplicationRequest {
        operation: "session-fixture-stop-terminal".into(),
        name: "quasar-sess-fixture-stop-terminal".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let client = engine.client();
    let id = client.start_application(request).wait().unwrap();
    engine.state.lock().unwrap().lose_remove = true;
    assert_eq!(
        client
            .cleanup_application(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    // A late stop must finish the already-recorded removal rather than writing
    // Stopping over retained terminal evidence.
    client
        .stop_application(id.clone(), Duration::from_secs(1))
        .wait()
        .unwrap();
    client
        .stop_application(id, Duration::from_secs(1))
        .wait()
        .unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn periodic_recovery_resumes_interrupted_no_id_abandonment_without_creating() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let request = ApplicationRequest {
        operation: "session-fixture-periodic-no-id".into(),
        name: "quasar-sess-fixture-periodic-no-id".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    assert_eq!(
        engine
            .client()
            .start_application(request)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine.state.lock().unwrap().inspect_code = Some(500);
    assert_eq!(
        engine
            .client()
            .abandon_application("session-fixture-periodic-no-id")
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine.state.lock().unwrap().inspect_code = None;
    RuntimeClient::new(engine.config.clone())
        .unwrap()
        .recover_application_cleanup()
        .wait()
        .unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn application_recovery_skips_a_locked_record_and_cleans_a_later_obligation() {
    use crate::runtime::application::{ApplicationIntent, ApplicationPhase};
    use std::os::{fd::AsRawFd, unix::fs::OpenOptionsExt};

    let engine = Engine::new();
    let second = ApplicationRequest {
        operation: "session-fixture-later-cleanup".into(),
        name: "quasar-sess-fixture-later-cleanup".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let second_id = engine.client().start_application(second).wait().unwrap();
    engine.state.lock().unwrap().lose_remove = true;
    assert_eq!(
        engine
            .client()
            .cleanup_application(second_id)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );

    let first_operation = "session-fixture-locked-recovery";
    let state_root = engine
        .config
        .image_state_path
        .as_ref()
        .unwrap()
        .join("applications");
    std::fs::create_dir_all(&state_root).unwrap();
    let key = Sha256::digest(first_operation.as_bytes())
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    let intent = ApplicationIntent {
        request: ApplicationRequest {
            operation: first_operation.into(),
            name: "quasar-sess-fixture-locked-recovery".into(),
            image: "quasar-app:test".into(),
            ..Default::default()
        },
        owner: "fixture-owner".into(),
        socket: engine.config.socket.clone(),
        id: None,
        image_id: None,
        image_entrypoint: None,
        image_cmd: None,
        image_user: None,
        image_volumes: None,
        image_volume_identities: None,
        nvidia_params_repair: None,
        phase: ApplicationPhase::Running,
        result: None,
    };
    std::fs::write(state_root.join(&key), serde_json::to_vec(&intent).unwrap()).unwrap();
    let lock = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(state_root.join(format!("{key}.lock")))
        .unwrap();
    assert_eq!(
        unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
        0
    );
    RuntimeClient::new(engine.config.clone())
        .unwrap()
        .recover_application_cleanup()
        .wait()
        .unwrap_err();
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn periodic_application_recovery_does_not_retire_a_running_unrequested_record() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let request = ApplicationRequest {
        operation: "session-fixture-periodic-active".into(),
        name: "quasar-sess-fixture-periodic-active".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    engine.client().start_application(request).wait().unwrap();
    RuntimeClient::new(engine.config.clone())
        .unwrap()
        .recover_application_cleanup()
        .wait()
        .unwrap();
    assert!(engine.state.lock().unwrap().running);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    assert_eq!(engine.requests("DELETE /containers/"), 0);
}

#[test]
fn late_application_observer_cannot_resurrect_completed_cleanup() {
    let engine = Engine::new();
    let request = ApplicationRequest {
        operation: "session-fixture-observe-race".into(),
        name: "quasar-sess-fixture-observe-race".into(),
        image: "quasar-app:test".into(),
        ..Default::default()
    };
    let client = engine.client();
    let id = client.start_application(request).wait().unwrap();
    let gate = Arc::new(LogGate::default());
    engine.state.lock().unwrap().pause_first_log = Some(gate.clone());
    let observation = client.observe_application(id.clone());
    let until = std::time::Instant::now() + Duration::from_secs(1);
    while !gate.entered.load(Ordering::SeqCst) {
        assert!(
            std::time::Instant::now() < until,
            "application observer must reach final logs"
        );
        thread::sleep(Duration::from_millis(2));
    }
    client.cleanup_application(id.clone()).wait().unwrap();
    gate.released.store(true, Ordering::SeqCst);
    assert_eq!(observation.wait().unwrap().exit_code, Some(23));
    client.recover_application_cleanup().wait().unwrap();
    client.cleanup_application(id).wait().unwrap();
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn nvidia_gpu_helper_requires_the_owned_all_gpu_driver_volume_profile() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = nvidia_request();
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    let body = engine.state.lock().unwrap().body.clone().unwrap();
    assert_eq!(body["HostConfig"]["NetworkMode"], json!("none"));
    assert_eq!(body["HostConfig"]["ReadonlyRootfs"], json!(true));
    assert_eq!(body["HostConfig"]["CapDrop"], json!(["ALL"]));
    assert_eq!(
        body["HostConfig"]["SecurityOpt"],
        json!(["no-new-privileges"])
    );
    assert_eq!(
        body["HostConfig"]["DeviceRequests"],
        json!([{"Driver":"nvidia","Count":-1,"Capabilities":[["gpu"]]}])
    );
    assert_eq!(body["HostConfig"]["Mounts"][0]["Type"], json!("volume"));
    assert_eq!(
        body["HostConfig"]["Mounts"][0]["Source"],
        json!("quasar-driver-fixture")
    );
    assert_eq!(body["HostConfig"]["Mounts"][0]["ReadOnly"], json!(true));
    assert_eq!(body["Env"], json!([
        "LD_LIBRARY_PATH=/opt/quasar/nvidia-driver/lib64:/image/lib",
        "__EGL_VENDOR_LIBRARY_DIRS=/opt/quasar/nvidia-driver/glvnd/egl_vendor.d:/etc/glvnd/egl_vendor.d:/usr/share/glvnd/egl_vendor.d",
        "__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS=/opt/quasar/nvidia-driver/egl_external_platform.d:/usr/share/egl/egl_external_platform.d",
        "VK_ADD_DRIVER_FILES=/opt/quasar/nvidia-driver/vulkan/icd.d/nvidia_icd.json",
        "GBM_BACKENDS_PATH=/opt/quasar/nvidia-driver/gbm",
    ]));
    assert_eq!(
        client
            .observe_diagnostic(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    client.cleanup_diagnostic(id).wait().unwrap();
}

#[test]
fn nvidia_gpu_helper_keeps_unrelated_inherited_image_environment() {
    let engine = Engine::new();
    engine.state.lock().unwrap().inherited_env = vec![
        "PATH=/usr/local/bin:/usr/bin".into(),
        "GST_PLUGIN_PATH=/image/plugins".into(),
    ];
    let (helper, run) = nvidia_request();
    let id = engine.client().run_gpu_probe(helper, run).wait().unwrap();
    engine
        .client()
        .observe_diagnostic(id.clone())
        .wait()
        .unwrap();
    engine.client().cleanup_diagnostic(id).wait().unwrap();
}

#[test]
fn nvidia_gpu_helper_rejects_an_inherited_driver_override() {
    let engine = Engine::new();
    engine.state.lock().unwrap().inherited_env = vec!["GBM_BACKENDS_PATH=/foreign/gbm".into()];
    let (helper, mut run) = nvidia_request();
    run.nvidia.as_mut().unwrap().has_gbm_backend = false;
    assert_eq!(
        engine
            .client()
            .run_gpu_probe(helper, run)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
}

#[test]
fn nvidia_gpu_helper_supports_a_readonly_driver_bind_without_gbm() {
    let engine = Engine::new();
    let (helper, mut run) = nvidia_request();
    run.nvidia.as_mut().unwrap().driver_mount = NvidiaDriverMount::ReadOnlyBind(ReadOnlyHostBind {
        source: "/daemon-only/nvidia-driver".into(),
        target: "/opt/quasar/nvidia-driver".into(),
    });
    run.nvidia.as_mut().unwrap().has_gbm_backend = false;
    engine.state.lock().unwrap().host_mount_override = Some(json!([{
        "Type":"bind", "Source":"/daemon-only/nvidia-driver", "Target":"/opt/quasar/nvidia-driver", "ReadOnly":true
    }]));
    let id = engine.client().run_gpu_probe(helper, run).wait().unwrap();
    let body = engine.state.lock().unwrap().body.clone().unwrap();
    assert_eq!(body["HostConfig"]["Mounts"][0]["Type"], json!("bind"));
    assert_eq!(body["HostConfig"]["Mounts"][0]["ReadOnly"], json!(true));
    assert!(body["Env"]
        .as_array()
        .unwrap()
        .iter()
        .all(|v| !v.as_str().unwrap().starts_with("GBM_BACKENDS_PATH=")));
    engine
        .client()
        .observe_diagnostic(id.clone())
        .wait()
        .unwrap();
    engine.client().cleanup_diagnostic(id).wait().unwrap();
}

#[test]
fn nvidia_gpu_helper_rejects_unsafe_driver_bind_options() {
    let engine = Engine::new();
    let (helper, mut run) = nvidia_request();
    run.nvidia.as_mut().unwrap().driver_mount = NvidiaDriverMount::ReadOnlyBind(ReadOnlyHostBind {
        source: "/daemon-only/nvidia-driver".into(),
        target: "/opt/quasar/nvidia-driver".into(),
    });
    engine.state.lock().unwrap().host_mount_override = Some(json!([{
        "Type":"bind", "Source":"/daemon-only/nvidia-driver", "Target":"/opt/quasar/nvidia-driver", "ReadOnly":true,
        "BindOptions":{"CreateMountpoint":true,"Propagation":"rshared","NonRecursive":true}
    }]));
    assert_eq!(
        engine
            .client()
            .run_gpu_probe(helper, run)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
}

#[test]
fn nvidia_gpu_helper_refuses_missing_gpu_or_weakened_driver_security_before_start() {
    for (requests, security) in [
        (Some(json!([])), None),
        (
            Some(json!([{"Driver":"nvidia","Count":1,"Capabilities":[["gpu"]]}])),
            None,
        ),
        (None, Some(json!([]))),
    ] {
        let engine = Engine::new();
        engine.state.lock().unwrap().host_device_requests_override = requests;
        engine.state.lock().unwrap().host_security_opt_override = security;
        let (helper, run) = nvidia_request();
        assert_eq!(
            engine
                .client()
                .run_gpu_probe(helper, run)
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::Protocol
        );
        assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 0);
    }
}

#[test]
fn nvidia_gpu_helper_refuses_a_wrong_realized_driver_volume_and_keeps_cleanup_tracked() {
    let engine = Engine::new();
    engine.state.lock().unwrap().host_mount_override = Some(json!([{
        "Type":"volume", "Source":"another-driver", "Target":"/opt/quasar/nvidia-driver", "ReadOnly":true
    }]));
    let (helper, run) = nvidia_request();
    assert_eq!(
        engine
            .client()
            .run_gpu_probe(helper, run)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
    assert!(
        engine.state.lock().unwrap().body.is_some(),
        "rejected realization remains journaled for explicit recovery"
    );
    engine.state.lock().unwrap().host_mount_override = None;
    engine.client().recover_diagnostics().wait().unwrap();
    assert!(engine.state.lock().unwrap().body.is_none());
}

#[test]
fn nvidia_gpu_helper_rejects_a_writable_realized_driver_mount() {
    let engine = Engine::new();
    engine.state.lock().unwrap().realized_mount_override = Some(json!([{
        "Type":"volume", "Source":"/var/lib/docker/volumes/quasar-driver-fixture/_data",
        "Name":"quasar-driver-fixture", "Destination":"/opt/quasar/nvidia-driver", "RW":true
    }]));
    let (helper, run) = nvidia_request();
    assert_eq!(
        engine
            .client()
            .run_gpu_probe(helper, run)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol
    );
}

#[test]
fn nvidia_gpu_lost_create_reconciles_the_same_owned_operation() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let (helper, run) = nvidia_request();
    let client = engine.client();
    assert_eq!(
        client
            .run_gpu_probe(helper.clone(), run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    let id = engine.client().run_gpu_probe(helper, run).wait().unwrap();
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
fn nvidia_gpu_lost_start_reconciles_the_original_without_a_second_start() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.lose_start = true;
        state.keep_running = true;
    }
    let (helper, run) = nvidia_request();
    let client = engine.client();
    assert_eq!(
        client
            .run_gpu_probe(helper.clone(), run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine.finish();
    let id = engine.client().run_gpu_probe(helper, run).wait().unwrap();
    assert_eq!(
        client
            .observe_diagnostic(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    client.cleanup_diagnostic(id).wait().unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
}

#[test]
fn failed_generic_recovery_still_recovers_the_nvidia_profile() {
    let engine = Engine::new();
    let client = engine.client();
    let (diagnostic_helper, diagnostic_run) = request();
    client
        .run_diagnostic(diagnostic_helper, diagnostic_run)
        .wait()
        .unwrap();
    // Make the generic journal unreconcilable while leaving its record in
    // place. A separate GPU operation then proves recovery tries both
    // profiles before returning the generic failure.
    engine.state.lock().unwrap().body = None;
    let (gpu_helper, gpu_run) = nvidia_request();
    let gpu = client.run_gpu_probe(gpu_helper, gpu_run).wait().unwrap();
    assert!(client.recover_diagnostics().wait().is_err());
    assert!(engine.state.lock().unwrap().body.is_none());
    assert_eq!(
        client.observe_diagnostic(gpu).wait().unwrap().exit_code,
        Some(23)
    );
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
fn audio_journal_with_the_pre_gpu_fingerprint_replays_without_a_second_create() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let (helper, run) = audio_request(&engine);
    let operation = helper.operation.clone();
    let id = engine
        .client()
        .run_audio_sidecar(helper.clone(), run.clone())
        .wait()
        .unwrap();
    let prior_fingerprint = Sha256::digest(
        serde_json::to_vec(&(&helper, Option::<&DiagnosticRun>::None, &run)).unwrap(),
    )
    .iter()
    .map(|byte| format!("{byte:02x}"))
    .collect();
    rewrite_helper_intent(&engine, &operation, |intent| {
        intent.request_fingerprint = prior_fingerprint;
    });
    assert_eq!(
        engine
            .client()
            .run_audio_sidecar(helper, run)
            .wait()
            .unwrap(),
        id
    );
    assert_eq!(engine.requests("POST /containers/create"), 1);
    engine.finish();
    engine.client().cleanup_audio_sidecar(id).wait().unwrap();
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
    let runtime = crate::session::container::ContainerRuntime::new(false);
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

// ---------------------------------------------------------------------------
// #239: the boot-only legacy sweep. It replaces the CLI `ps`/`inspect`/`rm -f`
// pass, so it must keep that pass's preservation rules exactly: an owner label
// alone never authorizes a removal, and neither does a name.

/// The six shapes an older agent can leave behind. Only the first is ours.
fn legacy_population() -> Vec<LegacyContainer> {
    vec![
        legacy('a', "/quasar-sess-one", owner_label("fixture-owner")),
        legacy('b', "/quasar-sess-two", owner_label("other-agent")),
        legacy('c', "/quasar-sess-unlabelled", json!({})),
        legacy('d', "/other-quasar-sess-one", owner_label("fixture-owner")),
        legacy('e', "/quasar-pulse-one", owner_label("fixture-owner")),
        legacy(
            'f',
            "/quasar-sess-api-owned",
            json!({crate::container_ownership::LABEL: "fixture-owner",
                   "io.quasar.application-operation": "api-operation"}),
        ),
    ]
}

fn prefixes() -> Vec<String> {
    vec![crate::session::container::SESSION_NAME_PREFIX.to_owned()]
}

fn deleted_ids(engine: &Engine) -> Vec<String> {
    engine
        .state
        .lock()
        .unwrap()
        .requests
        .iter()
        .filter_map(|request| request.strip_prefix("DELETE /containers/"))
        .map(|rest| rest.split('?').next().unwrap().to_owned())
        .collect()
}

#[test]
fn legacy_sweep_removes_only_the_owned_prefixed_container() {
    let engine = Engine::new();
    engine.state.lock().unwrap().legacy = legacy_population();
    let outcome = engine
        .client()
        .retire_legacy_containers(prefixes())
        .wait()
        .unwrap();
    assert_eq!(
        outcome,
        LegacyRetirement {
            removed: 1,
            preserved: 5,
            unresolved: 0
        }
    );
    assert_eq!(deleted_ids(&engine), vec!["a".repeat(64)]);
    // A foreign owner, an unlabelled legacy container, a substring-only name, an
    // audio sidecar and an API-owned application all survive the pass.
    let survivors = engine.state.lock().unwrap().legacy.clone();
    assert!(survivors
        .iter()
        .all(|container| container.gone == container.id.starts_with('a')));
}

#[test]
fn legacy_sweep_lost_remove_reply_is_unresolved_and_the_next_boot_retries_it() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.legacy = legacy_population();
        state.lose_legacy_remove = Some("a".repeat(64));
    }
    let outcome = engine
        .client()
        .retire_legacy_containers(prefixes())
        .wait()
        .unwrap();
    assert_eq!(
        outcome,
        LegacyRetirement {
            removed: 0,
            preserved: 5,
            unresolved: 1
        }
    );
    // Exactly one removal was attempted, and nothing else was touched.
    assert_eq!(deleted_ids(&engine), vec!["a".repeat(64)]);
    assert!(engine
        .state
        .lock()
        .unwrap()
        .legacy
        .iter()
        .all(|container| !container.gone));
    // The next boot reconciles the same container, with no duplicate work.
    let outcome = engine
        .client()
        .retire_legacy_containers(prefixes())
        .wait()
        .unwrap();
    assert_eq!(outcome.removed, 1);
    assert_eq!(outcome.unresolved, 0);
    assert_eq!(deleted_ids(&engine).len(), 2);
}

#[test]
fn legacy_sweep_without_proof_of_absence_does_not_remove_twice() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.legacy = vec![legacy(
            'a',
            "/quasar-sess-one",
            owner_label("fixture-owner"),
        )];
        state.legacy_remove_has_no_effect = Some("a".repeat(64));
    }
    let outcome = engine
        .client()
        .retire_legacy_containers(prefixes())
        .wait()
        .unwrap();
    assert_eq!(
        outcome,
        LegacyRetirement {
            removed: 0,
            preserved: 0,
            unresolved: 1
        }
    );
    assert_eq!(deleted_ids(&engine).len(), 1);
}

#[test]
fn legacy_sweep_listing_failure_is_an_error_without_any_mutation() {
    let engine = Engine::new();
    {
        let mut state = engine.state.lock().unwrap();
        state.legacy = legacy_population();
        state.refuse_legacy_list = true;
    }
    assert_eq!(
        engine
            .client()
            .retire_legacy_containers(prefixes())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Engine
    );
    assert!(deleted_ids(&engine).is_empty());
}

// ---------------------------------------------------------------------------
// #239 criterion 3: observation is inspect-polling by exact identity, so a gap
// in engine reachability must reconcile from inspection evidence rather than be
// mistaken for an application exit. There is deliberately no event stream to
// resubscribe to.

/// The ENGINE-side half of the observation gap: what the daemon is asked for, and
/// what it is never asked for, while an application exits during an outage. The
/// caller-side half — that the observer loop publishes no exit during the gap and
/// exactly one afterwards — is
/// `session::source::tests::the_observer_rides_out_an_engine_gap_and_publishes_the_true_exit_once`.
#[test]
fn application_observation_survives_an_engine_gap_and_reports_the_true_exit_once() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let id = client
        .start_application(ApplicationRequest {
            operation: "session-fixture-observation-gap".into(),
            name: "quasar-sess-fixture-observation-gap".into(),
            image: "quasar-app:test".into(),
            ..Default::default()
        })
        .wait()
        .unwrap();
    // The engine goes away, and the application exits while it is away — the
    // exit no observer was watching for.
    {
        let mut state = engine.state.lock().unwrap();
        state.unreachable = true;
        state.keep_running = false;
        state.running = false;
        state.exited = true;
        state.exit = Some(7);
    }
    for _ in 0..3 {
        assert_eq!(
            client
                .observe_application(id.clone())
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::Unavailable,
            "an unreachable engine is never terminal application evidence"
        );
    }
    engine.state.lock().unwrap().unreachable = false;
    let observed = client.observe_application(id.clone()).wait().unwrap();
    assert_eq!(observed.exit_code, Some(7));
    // The exit is derived from inspection once and then persisted: replaying the
    // observation asks the engine for nothing more and cannot report it twice.
    let inspections = engine.requests("GET /containers/");
    assert_eq!(client.observe_application(id).wait().unwrap(), observed);
    assert_eq!(engine.requests("GET /containers/"), inspections);
    // Observation across the whole gap started and stopped nothing.
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    assert!(deleted_ids(&engine).is_empty());
}

/// #239 criterion 4: the agent is killed with a cleanup obligation journalled
/// but unproven, and comes back to a foreign container sharing its name prefix
/// and a managed home full of user data. Boot must finish exactly its own
/// obligation, and must still be the same owner afterwards — the create label
/// and the label the legacy listing filters on are the same token on both sides
/// of the restart. Preservation of the on-disk owner LEASE is not provable here
/// (this fixture injects `diagnostic_owner` and never reads the file); it is
/// `container_ownership::tests::ownership_survives_restart_and_distinct_agents_are_isolated`
/// and the live interruption exercise. (The uncertain-mutation half — a lost stop
/// reply reconciled by the same operation — is
/// `application_lost_stop_reply_retries_the_same_durable_stopping_intent` and
/// `application_cleanup_retries_after_a_runtime_restart_without_touching_active_work`.)
#[test]
fn boot_finishes_pending_cleanup_without_touching_a_foreign_sibling_or_the_home() {
    let engine = Engine::new();
    let root = engine
        .config
        .image_state_path
        .as_ref()
        .unwrap()
        .parent()
        .unwrap()
        .to_owned();
    let home = root.join("homes/agent-1a2b3c4d-5e6f7a8b");
    std::fs::create_dir_all(&home).unwrap();
    std::fs::write(home.join("save.dat"), b"user data").unwrap();
    engine.state.lock().unwrap().legacy = vec![legacy(
        'b',
        "/quasar-sess-foreign",
        owner_label("another-agent"),
    )];
    let id = engine
        .client()
        .start_application(ApplicationRequest {
            operation: "session-fixture-boot-cleanup".into(),
            name: "quasar-sess-fixture-boot-cleanup".into(),
            image: "quasar-app:test".into(),
            typed_mounts: vec![ApplicationMount::Bind {
                source: home.to_string_lossy().into_owned(),
                target: "/home/quasar".into(),
                read_only: false,
                consistency: None,
            }],
            ..Default::default()
        })
        .wait()
        .unwrap();
    let created_owner = engine.state.lock().unwrap().body.as_ref().unwrap()["Labels"]
        [crate::container_ownership::LABEL]
        .as_str()
        .expect("the create carried an owner label")
        .to_owned();
    // SIGKILL between the remove and its reply: the obligation is durable, the
    // outcome is not.
    engine.state.lock().unwrap().lose_remove = true;
    assert_eq!(
        engine
            .client()
            .cleanup_application(id)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    // Boot, in the order agent.rs performs it.
    let booted = engine.client();
    booted.recover_application_cleanup().wait().unwrap();
    booted.retire_applications().wait().unwrap();
    assert_eq!(
        booted.retire_legacy_containers(prefixes()).wait().unwrap(),
        LegacyRetirement {
            removed: 0,
            preserved: 1,
            unresolved: 0
        }
    );
    // Exactly the owned container, exactly once.
    assert_eq!(deleted_ids(&engine), vec![ID.to_owned()]);
    assert!(engine
        .state
        .lock()
        .unwrap()
        .legacy
        .iter()
        .all(|container| !container.gone));
    assert_eq!(std::fs::read(home.join("save.dat")).unwrap(), b"user data");
    // Same owner on both sides of the restart: the token the pre-kill create
    // labelled the container with is the token the post-boot legacy listing
    // filtered on. A regenerated identity would orphan every prior container.
    let requests = engine.state.lock().unwrap().requests.clone();
    let listing = requests
        .iter()
        .find(|request| request.starts_with("GET /containers/json"))
        .expect("the legacy sweep listed containers");
    assert!(
        listing.contains(&urlencoding(&format!(
            "{}={created_owner}",
            crate::container_ownership::LABEL
        ))),
        "the post-boot listing filtered on a different owner than the create labelled: \
         {created_owner} vs {listing}"
    );
}

/// The subset of percent-encoding bollard applies to a filter's JSON value.
fn urlencoding(value: &str) -> String {
    value
        .chars()
        .map(|c| match c {
            '.' | '-' | '_' | '~' | '0'..='9' | 'a'..='z' | 'A'..='Z' => c.to_string(),
            other => format!("%{:02X}", other as u8),
        })
        .collect()
}

// ---------------------------------------------------------------------------
// #239: the endpoint can move under a journal. A proxy socket in front of the
// same daemon, or a `DOCKER_HOST` an operator edited, changes the recorded
// socket without changing what happened. A record whose outcome was already
// PROVEN needs no engine and must survive that; anything still unfinished must
// stay uncertain, because this agent cannot prove what the other endpoint did.

/// The same journals, reached through a different endpoint. Deliberately a dead
/// socket: any engine call at all fails loudly instead of quietly succeeding.
fn client_on_another_endpoint(engine: &Engine) -> RuntimeClient {
    let mut config = engine.config.clone();
    config.socket = engine.config.socket.with_file_name("proxy.sock");
    RuntimeClient::new(config).unwrap()
}

fn served(engine: &Engine) -> usize {
    engine.state.lock().unwrap().requests.len()
}

#[test]
fn boot_retirement_accepts_completed_applications_recorded_against_another_endpoint() {
    let engine = Engine::new();
    let client = engine.client();
    let id = client
        .start_application(ApplicationRequest {
            operation: "session-fixture-endpoint-completed".into(),
            name: "quasar-sess-fixture-endpoint-completed".into(),
            image: "quasar-app:test".into(),
            ..Default::default()
        })
        .wait()
        .unwrap();
    client.cleanup_application(id).wait().unwrap();
    let before = served(&engine);
    client_on_another_endpoint(&engine)
        .retire_applications()
        .wait()
        .expect("a proven-terminal record must not block startup from a new endpoint");
    assert_eq!(
        served(&engine),
        before,
        "a terminal record has nothing left to mutate; it must ask no engine anything"
    );
}

#[test]
fn boot_retirement_keeps_an_unfinished_application_uncertain_across_an_endpoint_change() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_remove = true;
    let client = engine.client();
    let id = client
        .start_application(ApplicationRequest {
            operation: "session-fixture-endpoint-pending".into(),
            name: "quasar-sess-fixture-endpoint-pending".into(),
            image: "quasar-app:test".into(),
            ..Default::default()
        })
        .wait()
        .unwrap();
    assert_eq!(
        client.cleanup_application(id).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    let before = served(&engine);
    assert_eq!(
        client_on_another_endpoint(&engine)
            .retire_applications()
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome,
        "an unfinished record from another endpoint stays fail-closed"
    );
    assert_eq!(served(&engine), before, "and mutates nothing on the way");
}

#[test]
fn boot_retirement_and_recovery_accept_completed_audio_recorded_against_another_endpoint() {
    let engine = Engine::new();
    let (helper, run) = audio_request(&engine);
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    engine.client().cleanup_audio_sidecar(id).wait().unwrap();
    let before = served(&engine);
    let moved = client_on_another_endpoint(&engine);
    moved
        .retire_audio_sidecars()
        .wait()
        .expect("a completed audio tombstone must not block startup from a new endpoint");
    moved
        .recover_audio_sidecars()
        .wait()
        .expect("nor routine audio recovery");
    moved
        .recover_diagnostics()
        .wait()
        .expect("nor diagnostic recovery, which scans the same journals");
    assert_eq!(served(&engine), before);
}

#[test]
fn boot_retirement_keeps_an_unfinished_audio_record_uncertain_across_an_endpoint_change() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let (helper, run) = audio_request(&engine);
    engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    let before = served(&engine);
    assert_eq!(
        client_on_another_endpoint(&engine)
            .retire_audio_sidecars()
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome,
        "boot retirement stops a sidecar; one recorded elsewhere is not ours to stop"
    );
    assert_eq!(served(&engine), before, "and nothing was mutated");
    assert!(engine.state.lock().unwrap().running);

    // Routine recovery, on a record that is neither live nor terminal: the
    // removal was refused, so its cleanup obligation is still open and only the
    // endpoint that recorded it may finish it.
    let pending = Engine::new();
    pending.state.lock().unwrap().refuse_remove = true;
    let (helper, run) = audio_request(&pending);
    let id = pending
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    assert!(pending.client().cleanup_audio_sidecar(id).wait().is_err());
    let before = served(&pending);
    assert_eq!(
        client_on_another_endpoint(&pending)
            .recover_audio_sidecars()
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(served(&pending), before);
}

/// #239/#228: an interrupted teardown stays tracked FOR RETRY, at runtime and not
/// only at the next boot. The engine goes away between a sidecar's stop request and
/// its reply, so the teardown is durable (`Stopping`) and the container untouched;
/// the agent's periodic maintenance pass must finish it as soon as the engine is
/// reachable again. Before that pass existed the sidecar simply ran on until a
/// restart. Routine recovery still leaves a LIVE sidecar alone —
/// `ordinary_audio_recovery_preserves_live_work_but_boot_retirement_stops_it`; what
/// makes this one ours to finish is the recorded stop, not the pass that finds it.
#[test]
fn routine_audio_recovery_finishes_a_stop_whose_reply_was_lost_and_retires_its_socket_dir() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let (helper, run) = audio_request(&engine);
    let socket_dir = run.socket_dir.clone();
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    assert!(socket_dir.is_dir());
    engine.state.lock().unwrap().lose_stop_before_effect = true;
    assert_eq!(
        engine
            .client()
            .stop_audio_sidecar(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert!(
        engine.state.lock().unwrap().running,
        "the stop never reached the daemon; the record is the only evidence it was asked for"
    );
    let maintenance = engine.client();
    maintenance.recover_audio_sidecars().wait().unwrap();
    assert!(!engine.state.lock().unwrap().running);
    assert_eq!(
        engine.requests(&format!("POST /containers/{ID}/stop")),
        2,
        "the same durable intent, retried once"
    );
    assert_eq!(
        engine.requests("POST /containers/create"),
        1,
        "recovery reconciles the recorded sidecar; it never creates a second one"
    );
    assert_eq!(
        maintenance
            .observe_audio_sidecar(id)
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    assert!(
        !socket_dir.exists(),
        "the socket directory is retired with the sidecar it belonged to"
    );
    // Idempotent: the next tick finds nothing left to finish.
    let settled = engine.state.lock().unwrap().requests.len();
    maintenance.recover_audio_sidecars().wait().unwrap();
    assert_eq!(engine.state.lock().unwrap().requests.len(), settled);
    assert_eq!(engine.requests("POST /containers/create"), 1);
}

/// One record this agent cannot reconcile must not hide every obligation behind
/// it. Recovery scans in a stable order, so a bad entry is met FIRST on every
/// 30 s pass — aborting there would leave a stopped-but-unremoved sidecar running
/// forever, which is the same failure mode as having no maintenance pass at all.
#[test]
fn routine_recovery_finishes_later_obligations_past_an_unreadable_journal() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let (helper, run) = audio_request(&engine);
    let socket_dir = run.socket_dir.clone();
    let id = engine
        .client()
        .run_audio_sidecar(helper, run)
        .wait()
        .unwrap();
    engine.state.lock().unwrap().lose_stop_before_effect = true;
    assert_eq!(
        engine
            .client()
            .stop_audio_sidecar(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    // A journal whose bytes are not a record at all, named so it sorts first.
    let helpers = helper_intent_path(&engine, "pulse-fixture")
        .parent()
        .unwrap()
        .to_owned();
    std::fs::write(helpers.join("0".repeat(64)), b"not a journal").unwrap();
    let maintenance = engine.client();
    assert_eq!(
        maintenance
            .recover_audio_sidecars()
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Protocol,
        "the unreadable record is still reported as an open obligation"
    );
    assert!(
        !engine.state.lock().unwrap().running,
        "the recoverable record behind the bad one was reconciled anyway"
    );
    assert_eq!(
        maintenance
            .observe_audio_sidecar(id)
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    assert!(!socket_dir.exists());
}

// ---------------------------------------------------------------------------
// #258: the closed GPU probe profile. One profile for every vendor — DRM nodes
// and their owning groups for AMD/Intel, the all-GPUs device request plus the
// driver volume for NVIDIA — journaled, owned and recovered like every helper.
// The NVIDIA arm must realize byte-for-byte what the NVIDIA GPU diagnostic
// realized before it was widened into this profile.

type RequestChange = Box<dyn Fn(&mut DiagnosticHelper, &mut GpuProbeRun)>;
type EngineChange = Box<dyn Fn(&mut State)>;
type ProbeChange = Box<dyn Fn(&mut GpuProbeRun)>;

fn nvidia_access() -> NvidiaDriverAccess {
    NvidiaDriverAccess {
        driver_mount: NvidiaDriverMount::NamedVolume {
            name: "quasar-driver-fixture".into(),
            target: "/opt/quasar/nvidia-driver".into(),
        },
        image_ld_library_path: "/image/lib".into(),
        has_gbm_backend: true,
    }
}

fn gpu_probe_helper(operation: &str) -> DiagnosticHelper {
    DiagnosticHelper {
        operation: operation.into(),
        name: format!(
            "{}{operation}",
            crate::container_ownership::PROBE_NAME_PREFIX
        ),
        image: "quasar-agent:test".into(),
    }
}

/// The NVIDIA arm, under the operation the pre-#258 NVIDIA fixture used so the
/// realized labels are comparable byte-for-byte.
fn nvidia_probe_request() -> (DiagnosticHelper, GpuProbeRun) {
    (
        gpu_probe_helper("nvidia-fixture"),
        GpuProbeRun {
            entrypoint: vec!["/usr/bin/timeout".into()],
            command: vec!["5s".into(), "/bin/sh".into(), "-c".into(), "exit 23".into()],
            devices: Vec::new(),
            groups: Vec::new(),
            nvidia: Some(nvidia_access()),
        },
    )
}

/// The AMD/Intel arm: the DRM directory and the groups owning its nodes, as a
/// session's application container is given them.
fn dri_probe_request(operation: &str) -> (DiagnosticHelper, GpuProbeRun) {
    (
        gpu_probe_helper(operation),
        GpuProbeRun {
            entrypoint: vec!["/usr/bin/timeout".into()],
            command: vec!["5s".into(), "/bin/sh".into(), "-c".into(), "exit 23".into()],
            devices: vec!["/dev/dri".into()],
            groups: vec![44, 991],
            nvidia: None,
        },
    )
}

#[test]
fn gpu_probe_nvidia_access_realizes_the_previous_nvidia_profile_byte_for_byte() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = nvidia_probe_request();
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    let body = engine.state.lock().unwrap().body.clone().unwrap();
    // Captured from `nvidia_gpu_helper_requires_the_owned_all_gpu_driver_volume_profile`
    // on the commit before this profile existed (b373c2f). Not derived from the
    // code under test: any drift here is a change to what NVIDIA hosts run.
    let before: Value = serde_json::from_str(r#"{"Cmd":["5s","/bin/sh","-c","exit 23"],"Entrypoint":["/usr/bin/timeout"],"Env":["LD_LIBRARY_PATH=/opt/quasar/nvidia-driver/lib64:/image/lib","__EGL_VENDOR_LIBRARY_DIRS=/opt/quasar/nvidia-driver/glvnd/egl_vendor.d:/etc/glvnd/egl_vendor.d:/usr/share/glvnd/egl_vendor.d","__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS=/opt/quasar/nvidia-driver/egl_external_platform.d:/usr/share/egl/egl_external_platform.d","VK_ADD_DRIVER_FILES=/opt/quasar/nvidia-driver/vulkan/icd.d/nvidia_icd.json","GBM_BACKENDS_PATH=/opt/quasar/nvidia-driver/gbm"],"HostConfig":{"AutoRemove":false,"CapDrop":["ALL"],"DeviceRequests":[{"Capabilities":[["gpu"]],"Count":-1,"Driver":"nvidia"}],"Devices":[],"Mounts":[{"ReadOnly":true,"Source":"quasar-driver-fixture","Target":"/opt/quasar/nvidia-driver","Type":"volume"}],"NetworkMode":"none","Privileged":false,"ReadonlyRootfs":true,"SecurityOpt":["no-new-privileges"]},"Image":"quasar-agent:test","Labels":{"io.quasar.agent-owner":"fixture-owner","io.quasar.runtime-operation":"nvidia-fixture"},"User":"0:0"}"#).unwrap();
    assert_eq!(body, before);
    assert_eq!(
        client
            .observe_gpu_probe(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    client.cleanup_gpu_probe(id).wait().unwrap();
    assert!(engine.state.lock().unwrap().body.is_none());
}

#[test]
fn gpu_probe_dri_access_realizes_devices_and_groups_without_network_or_privilege() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-fixture");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    let body = engine.state.lock().unwrap().body.clone().unwrap();
    assert_eq!(
        body["HostConfig"]["Devices"],
        json!([{"PathOnHost":"/dev/dri","PathInContainer":"/dev/dri","CgroupPermissions":"rwm"}])
    );
    assert_eq!(body["HostConfig"]["GroupAdd"], json!(["44", "991"]));
    assert!(body["HostConfig"]["DeviceRequests"]
        .as_array()
        .is_none_or(Vec::is_empty));
    assert!(body["HostConfig"]["Mounts"]
        .as_array()
        .is_none_or(Vec::is_empty));
    assert!(body["Env"].as_array().is_none_or(Vec::is_empty));
    assert_eq!(body["HostConfig"]["NetworkMode"], json!("none"));
    assert_eq!(body["HostConfig"]["ReadonlyRootfs"], json!(true));
    assert_eq!(body["HostConfig"]["Privileged"], json!(false));
    assert_eq!(body["HostConfig"]["AutoRemove"], json!(false));
    assert_eq!(body["HostConfig"]["CapDrop"], json!(["ALL"]));
    assert_eq!(
        body["HostConfig"]["SecurityOpt"],
        json!(["no-new-privileges"])
    );
    assert_eq!(body["User"], json!("0:0"));
    assert_eq!(body["Entrypoint"], json!(["/usr/bin/timeout"]));
    assert_eq!(body["Cmd"], json!(["5s", "/bin/sh", "-c", "exit 23"]));
    assert_eq!(
        body["Labels"],
        json!({"io.quasar.agent-owner":"fixture-owner","io.quasar.runtime-operation":"dri-fixture"})
    );
    let result = client.observe_gpu_probe(id.clone()).wait().unwrap();
    assert_eq!(result.exit_code, Some(23));
    assert_eq!(result.stdout, "final stdout");
    client.cleanup_gpu_probe(id).wait().unwrap();
    assert!(engine.state.lock().unwrap().body.is_none());
}

#[test]
fn gpu_probe_refuses_unsupported_requirements_before_any_engine_request() {
    let cases: Vec<(&str, RequestChange)> = vec![
        (
            "device outside /dev/dri",
            Box::new(|_, run| run.devices = vec!["/dev/kfd".into()]),
        ),
        (
            "device escaping /dev/dri",
            Box::new(|_, run| run.devices = vec!["/dev/dri/../sda".into()]),
        ),
        (
            "device with a NUL byte",
            Box::new(|_, run| run.devices = vec!["/dev/dri/card\0".into()]),
        ),
        ("root group", Box::new(|_, run| run.groups = vec![0])),
        (
            "unsorted groups",
            Box::new(|_, run| run.groups = vec![991, 44]),
        ),
        (
            "duplicate groups",
            Box::new(|_, run| run.groups = vec![44, 44]),
        ),
        (
            "no GPU access at all",
            Box::new(|_, run| {
                run.devices.clear();
                run.groups.clear();
                run.nvidia = None;
            }),
        ),
        (
            "empty entrypoint",
            Box::new(|_, run| run.entrypoint.clear()),
        ),
        (
            "name outside the probe prefix",
            Box::new(|helper, _| helper.name = "quasar-diagnostic-dri".into()),
        ),
        (
            "session name prefix",
            Box::new(|helper, _| helper.name = "quasar-sess-dri".into()),
        ),
        (
            "driver volume with a path separator",
            Box::new(|_, run| {
                run.nvidia = Some(NvidiaDriverAccess {
                    driver_mount: NvidiaDriverMount::NamedVolume {
                        name: "../etc".into(),
                        target: "/opt/quasar/nvidia-driver".into(),
                    },
                    ..nvidia_access()
                })
            }),
        ),
    ];
    for (label, change) in cases {
        let engine = Engine::new();
        let (mut helper, mut run) = dri_probe_request("dri-invalid");
        change(&mut helper, &mut run);
        assert_eq!(
            engine
                .client()
                .run_gpu_probe(helper, run)
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::InvalidConfiguration,
            "{label}"
        );
        assert_eq!(engine.requests(""), 0, "{label}: no engine request at all");
        assert!(
            std::fs::read_dir(
                engine
                    .config
                    .image_state_path
                    .as_ref()
                    .unwrap()
                    .join("helpers")
            )
            .map(|entries| entries.count() == 0)
            .unwrap_or(true),
            "{label}: no journal is written for a refused request"
        );
    }
}

#[test]
fn gpu_probe_refuses_weakened_device_group_or_driver_realization_before_start() {
    let cases: Vec<(&str, EngineChange)> = vec![
        (
            "devices dropped",
            Box::new(|s| s.host_devices_override = Some(json!([]))),
        ),
        (
            "device permissions narrowed",
            Box::new(|s| {
                s.host_devices_override = Some(
                    json!([{"PathOnHost":"/dev/dri","PathInContainer":"/dev/dri","CgroupPermissions":"r"}]),
                )
            }),
        ),
        (
            "device remapped",
            Box::new(|s| {
                s.host_devices_override = Some(
                    json!([{"PathOnHost":"/dev/dri","PathInContainer":"/dev/gpu","CgroupPermissions":"rwm"}]),
                )
            }),
        ),
        (
            "groups dropped",
            Box::new(|s| s.host_group_add_override = Some(json!([]))),
        ),
        (
            "one group dropped",
            Box::new(|s| s.host_group_add_override = Some(json!(["44"]))),
        ),
        (
            "a group added",
            Box::new(|s| s.host_group_add_override = Some(json!(["0", "44", "991"]))),
        ),
        (
            "an unrequested nvidia device request",
            Box::new(|s| {
                s.host_device_requests_override =
                    Some(json!([{"Driver":"nvidia","Count":-1,"Capabilities":[["gpu"]]}]))
            }),
        ),
        (
            "security weakened",
            Box::new(|s| s.host_security_opt_override = Some(json!([]))),
        ),
    ];
    for (label, change) in cases {
        let engine = Engine::new();
        change(&mut engine.state.lock().unwrap());
        let (helper, run) = dri_probe_request("dri-weakened");
        assert_eq!(
            engine
                .client()
                .run_gpu_probe(helper, run)
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::Protocol,
            "{label}"
        );
        assert_eq!(
            engine.requests(&format!("POST /containers/{ID}/start")),
            0,
            "{label}: never started"
        );
        assert!(
            engine.state.lock().unwrap().body.is_some(),
            "{label}: the rejected realization stays journaled for explicit recovery"
        );
        {
            let mut s = engine.state.lock().unwrap();
            s.host_devices_override = None;
            s.host_group_add_override = None;
            s.host_device_requests_override = None;
            s.host_security_opt_override = None;
        }
        engine.client().recover_diagnostics().wait().unwrap();
        assert!(
            engine.state.lock().unwrap().body.is_none(),
            "{label}: recovered"
        );
    }
    // The NVIDIA arm keeps its own realization guards.
    for (label, requests) in [
        ("nvidia device request dropped", json!([])),
        (
            "nvidia device request narrowed",
            json!([{"Driver":"nvidia","Count":1,"Capabilities":[["gpu"]]}]),
        ),
    ] {
        let engine = Engine::new();
        engine.state.lock().unwrap().host_device_requests_override = Some(requests);
        let (helper, run) = nvidia_probe_request();
        assert_eq!(
            engine
                .client()
                .run_gpu_probe(helper, run)
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::Protocol,
            "{label}"
        );
        assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 0);
    }
}

#[test]
fn gpu_probe_observation_timeout_is_not_an_outcome_and_explicit_stop_then_cleanup_follow() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-deadline");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    // The orchestrator's deadline (#259) is what expires here; the runtime
    // reports a timeout of the OBSERVATION and nothing else changes.
    assert_eq!(
        client
            .observe_gpu_probe(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Timeout
    );
    assert!(engine.state.lock().unwrap().running);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    assert_eq!(engine.requests("DELETE /containers/"), 0);
    let intent = helper_intent(&engine, "dri-deadline");
    assert!(
        intent.result.is_none(),
        "a timeout is never recorded as a result"
    );
    assert_eq!(intent.phase, HelperPhase::Running);
    // Past the deadline the orchestrator issues the explicit stop, then cleanup.
    client.stop_gpu_probe(id.clone()).wait().unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    assert!(!engine.state.lock().unwrap().running);
    assert_eq!(
        client
            .observe_gpu_probe(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    client.cleanup_gpu_probe(id).wait().unwrap();
    assert_eq!(engine.requests("DELETE /containers/"), 1);
    assert!(engine.state.lock().unwrap().body.is_none());
    assert_eq!(
        helper_intent(&engine, "dri-deadline").phase,
        HelperPhase::Completed
    );
}

#[test]
fn gpu_probe_dropped_observation_stops_and_removes_nothing() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-dropped");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    let observer = client.observe_gpu_probe(id.clone());
    thread::sleep(Duration::from_millis(100));
    observer.cancel();
    drop(observer);
    thread::sleep(Duration::from_millis(100));
    assert!(engine.state.lock().unwrap().running);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    assert_eq!(engine.requests("DELETE /containers/"), 0);
    assert_eq!(
        helper_intent(&engine, "dri-dropped").phase,
        HelperPhase::Running
    );
    // Only the explicit operations end it.
    client.stop_gpu_probe(id.clone()).wait().unwrap();
    client.cleanup_gpu_probe(id).wait().unwrap();
    assert!(engine.state.lock().unwrap().body.is_none());
}

/// A launch pre-empts a probe, or its deadline passes: the orchestrator stops waiting
/// and nothing else. Only the explicit stop and cleanup end the container.
#[test]
fn gpu_probe_cancelled_wait_stops_and_removes_nothing_and_explicit_teardown_still_works() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-cancelled-wait");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    // A timeout would report `Timeout`, so the kind alone proves it did not just
    // sit until the deadline.
    assert_eq!(
        client
            .observe_gpu_probe(id.clone())
            .wait_with_cancel(|| true)
            .unwrap_err()
            .kind,
        ErrorKind::Cancelled
    );
    assert!(engine.state.lock().unwrap().running);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    assert_eq!(engine.requests("DELETE /containers/"), 0);
    assert_eq!(
        helper_intent(&engine, "dri-cancelled-wait").phase,
        HelperPhase::Running
    );
    client.stop_gpu_probe(id.clone()).wait().unwrap();
    client.cleanup_gpu_probe(id).wait().unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
    assert!(engine.state.lock().unwrap().body.is_none());
}

#[test]
fn gpu_probe_that_finishes_before_any_cancel_returns_its_outcome() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-uncancelled-wait");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    let result = client
        .observe_gpu_probe(id.clone())
        .wait_with_cancel(|| false)
        .unwrap();
    assert_eq!(result.exit_code, Some(23));
    assert_eq!(result.stdout, "final stdout");
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    client.cleanup_gpu_probe(id).wait().unwrap();
}

#[test]
fn gpu_probe_lost_create_reply_is_reconciled_under_the_same_operation() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-lost-create");
    assert_eq!(
        client
            .run_gpu_probe(helper.clone(), run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    assert_eq!(
        client
            .observe_gpu_probe(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    client.cleanup_gpu_probe(id).wait().unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
}

#[test]
fn gpu_probe_lost_start_reply_is_reconciled_without_a_second_start() {
    let engine = Engine::new();
    {
        let mut s = engine.state.lock().unwrap();
        s.lose_start = true;
        s.keep_running = true;
    }
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-lost-start");
    assert_eq!(
        client
            .run_gpu_probe(helper.clone(), run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    engine.finish();
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    assert_eq!(
        client
            .observe_gpu_probe(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    client.cleanup_gpu_probe(id).wait().unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
}

#[test]
fn gpu_probe_lost_stop_reply_is_reconciled_without_a_second_stop() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-lost-stop");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    engine.state.lock().unwrap().lose_stop = true;
    assert_eq!(
        client.stop_gpu_probe(id.clone()).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(
        helper_intent(&engine, "dri-lost-stop").phase,
        HelperPhase::Stopping,
        "the termination request is durable"
    );
    client.stop_gpu_probe(id.clone()).wait().unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    assert_eq!(
        client
            .observe_gpu_probe(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    client.cleanup_gpu_probe(id).wait().unwrap();
    assert!(engine.state.lock().unwrap().body.is_none());
}

#[test]
fn gpu_probe_lost_remove_reply_is_reconciled_by_absence_of_the_same_id_and_keeps_evidence() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_remove = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-lost-remove");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    assert_eq!(
        client
            .observe_gpu_probe(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    assert_eq!(
        client
            .cleanup_gpu_probe(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    let intent = helper_intent(&engine, "dri-lost-remove");
    assert_eq!(intent.phase, HelperPhase::CleanupPending);
    assert_eq!(intent.result.as_ref().unwrap().exit_code, Some(23));
    client.cleanup_gpu_probe(id.clone()).wait().unwrap();
    assert_eq!(engine.requests("DELETE /containers/"), 1);
    assert_eq!(
        helper_intent(&engine, "dri-lost-remove").phase,
        HelperPhase::Completed
    );
    let replay = client.observe_gpu_probe(id).wait().unwrap();
    assert_eq!(replay.exit_code, Some(23));
    assert_eq!(replay.stderr, "final stderr");
}

#[test]
fn gpu_probe_journal_recovers_an_interrupted_stop_after_a_simulated_restart() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-restart");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    engine.state.lock().unwrap().lose_stop_before_effect = true;
    assert_eq!(
        client.stop_gpu_probe(id.clone()).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    assert!(engine.state.lock().unwrap().running);
    drop(client);
    let restarted = engine.client();
    restarted.recover_diagnostics().wait().unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 2);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
    assert!(engine.state.lock().unwrap().body.is_none());
    assert_eq!(
        helper_intent(&engine, "dri-restart").phase,
        HelperPhase::Completed
    );
    assert_eq!(
        restarted.observe_gpu_probe(id).wait().unwrap().exit_code,
        Some(23)
    );
}

#[test]
fn boot_retirement_finishes_a_running_probe_whose_orchestrator_died_but_routine_recovery_does_not()
{
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-orphan");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    drop(client);
    // Routine recovery never terminates work nobody asked to stop.
    let restarted = engine.client();
    assert!(restarted.recover_diagnostics().wait().is_err());
    assert!(engine.state.lock().unwrap().running);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    // Boot retirement does: the deadline's owner is gone with the process.
    restarted.retire_gpu_probes().wait().unwrap();
    assert!(!engine.state.lock().unwrap().running);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
    assert!(engine.state.lock().unwrap().body.is_none());
    assert_eq!(
        helper_intent(&engine, "dri-orphan").phase,
        HelperPhase::Completed
    );
    assert_eq!(
        restarted.observe_gpu_probe(id).wait().unwrap().exit_code,
        Some(23)
    );
    // Probe retirement leaves a live audio sidecar and a running generic
    // diagnostic alone: they are not its kind.
    let other = Engine::new();
    other.state.lock().unwrap().keep_running = true;
    let observer = other.client();
    let (helper, run) = request();
    let diagnostic = observer.run_diagnostic(helper, run).wait().unwrap();
    observer.retire_gpu_probes().wait().unwrap();
    assert!(other.state.lock().unwrap().running);
    assert_eq!(other.requests(&format!("POST /containers/{ID}/stop")), 0);
    other.finish();
    observer.cleanup_diagnostic(diagnostic).wait().unwrap();
}

#[test]
fn older_nvidia_gpu_journals_still_load_and_recover() {
    let engine = Engine::new();
    let client = engine.client();
    let (helper, run) = nvidia_probe_request();
    let id = client
        .run_gpu_probe(helper.clone(), run.clone())
        .wait()
        .unwrap();
    // Rewrite the record to the shape the pre-#258 agent wrote: profile
    // `NvidiaGpu`, the request under `nvidia_gpu`, no `gpu_probe` field at all,
    // and the three-tuple fingerprint of that implementation.
    let legacy_run = NvidiaGpuRun {
        entrypoint: run.entrypoint.clone(),
        command: run.command.clone(),
        driver_mount: nvidia_access().driver_mount,
        image_ld_library_path: nvidia_access().image_ld_library_path,
        has_gbm_backend: true,
    };
    let legacy_fingerprint = Sha256::digest(
        serde_json::to_vec(&(&helper, None::<&DiagnosticRun>, Some(&legacy_run))).unwrap(),
    )
    .iter()
    .map(|b| format!("{b:02x}"))
    .collect::<String>();
    let path = helper_intent_path(&engine, "nvidia-fixture");
    let mut record: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    let object = record.as_object_mut().unwrap();
    object.remove("gpu_probe");
    object.insert("profile".into(), json!("NvidiaGpu"));
    object.insert(
        "nvidia_gpu".into(),
        serde_json::to_value(&legacy_run).unwrap(),
    );
    object.insert("request_fingerprint".into(), json!(legacy_fingerprint));
    object.insert("phase".into(), json!("Stopped"));
    object.insert("result".into(), Value::Null);
    std::fs::write(&path, serde_json::to_vec(&record).unwrap()).unwrap();
    let loaded = helper_intent(&engine, "nvidia-fixture");
    assert_eq!(loaded.profile, HelperProfile::NvidiaGpu);
    assert!(
        loaded.gpu_probe.is_none(),
        "the new field defaults when absent"
    );
    assert_eq!(loaded.nvidia_gpu, Some(legacy_run));
    // The same request under the new profile is a DIFFERENT request: the old
    // record is never adopted by it, and no second container is created.
    assert_eq!(
        client.run_gpu_probe(helper, run).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(engine.requests("POST /containers/create"), 1);
    // Recovery finishes the old record under its own identity.
    client.recover_diagnostics().wait().unwrap();
    assert_eq!(engine.requests("DELETE /containers/"), 1);
    assert!(engine.state.lock().unwrap().body.is_none());
    assert_eq!(
        helper_intent(&engine, "nvidia-fixture").phase,
        HelperPhase::Completed
    );
    assert_eq!(
        client.observe_gpu_probe(id).wait().unwrap().exit_code,
        Some(23)
    );
}

#[test]
fn gpu_probe_fingerprint_covers_devices_groups_and_driver_access() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-fingerprint");
    let id = client
        .run_gpu_probe(helper.clone(), run.clone())
        .wait()
        .unwrap();
    let changes: Vec<ProbeChange> = vec![
        Box::new(|run| run.groups = vec![44]),
        Box::new(|run| run.devices = vec!["/dev/dri/renderD128".into()]),
        Box::new(|run| run.nvidia = Some(nvidia_access())),
        Box::new(|run| run.command.push("changed".into())),
    ];
    for change in changes {
        let mut changed = run.clone();
        change(&mut changed);
        assert_eq!(
            client
                .run_gpu_probe(helper.clone(), changed)
                .wait()
                .unwrap_err()
                .kind,
            ErrorKind::UnknownOutcome
        );
    }
    // The identical request replays the recorded operation.
    assert_eq!(client.run_gpu_probe(helper, run).wait().unwrap(), id);
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 1);
    engine.finish();
    client.cleanup_gpu_probe(id).wait().unwrap();
}

#[test]
fn gpu_probe_without_verified_ownership_is_refused() {
    // A foreign container already holds the name: read, never mutated, no create.
    let collision = Engine::new();
    {
        let mut s = collision.state.lock().unwrap();
        s.body = Some(json!({"HostConfig":{},"Env":[]}));
        s.name = format!(
            "{}dri-foreign",
            crate::container_ownership::PROBE_NAME_PREFIX
        );
    }
    let (helper, run) = dri_probe_request("dri-foreign");
    assert_eq!(
        collision
            .client()
            .run_gpu_probe(helper, run)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(collision.requests("POST /containers/create"), 0);
    assert_eq!(collision.requests("POST /containers/"), 0);
    assert_eq!(collision.requests("DELETE /containers/"), 0);

    // The recorded ID now resolves to another container: no stop, no remove.
    let replaced = Engine::new();
    replaced.state.lock().unwrap().keep_running = true;
    let client = replaced.client();
    let (helper, run) = dri_probe_request("dri-replaced");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    replaced.state.lock().unwrap().replace_id = true;
    assert_eq!(
        client.stop_gpu_probe(id.clone()).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(
        client
            .cleanup_gpu_probe(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(replaced.requests(&format!("POST /containers/{ID}/stop")), 0);
    assert_eq!(replaced.requests("DELETE /containers/"), 0);

    // Another owner reading the same journals cannot act on them either.
    let mut foreign = replaced.config.clone();
    foreign.diagnostic_owner = Some("another-agent".into());
    let other = RuntimeClient::new(foreign).unwrap();
    assert_eq!(
        other.stop_gpu_probe(id.clone()).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(
        other.cleanup_gpu_probe(id.clone()).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    assert!(other.retire_gpu_probes().wait().is_err());
    assert_eq!(replaced.requests(&format!("POST /containers/{ID}/stop")), 0);
    assert_eq!(replaced.requests("DELETE /containers/"), 0);
    assert!(replaced.state.lock().unwrap().running);
    replaced.state.lock().unwrap().replace_id = false;
    client.stop_gpu_probe(id.clone()).wait().unwrap();
    client.cleanup_gpu_probe(id).wait().unwrap();
}

#[test]
fn no_second_gpu_probe_starts_while_an_earlier_one_is_unreconciled() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (first_helper, first_run) = dri_probe_request("dri-first");
    let first = client
        .run_gpu_probe(first_helper, first_run)
        .wait()
        .unwrap();
    engine.state.lock().unwrap().lose_stop_before_effect = true;
    assert_eq!(
        client
            .stop_gpu_probe(first.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    // A different probe of the same kind is refused before any create.
    let (second_helper, second_run) = nvidia_probe_request();
    assert_eq!(
        client
            .run_gpu_probe(second_helper.clone(), second_run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Busy
    );
    assert_eq!(engine.requests("POST /containers/create"), 1);
    // The earlier one reconciles under its own identity; then the next may start.
    client.stop_gpu_probe(first.clone()).wait().unwrap();
    assert_eq!(
        client
            .run_gpu_probe(second_helper.clone(), second_run.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::Busy,
        "stopped but not yet removed is still unreconciled"
    );
    client.cleanup_gpu_probe(first).wait().unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 1);
    engine.state.lock().unwrap().keep_running = false;
    let second = client
        .run_gpu_probe(second_helper, second_run)
        .wait()
        .unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 2);
    assert_eq!(
        client
            .observe_gpu_probe(second.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    client.cleanup_gpu_probe(second).wait().unwrap();
}

#[test]
fn gpu_probe_missing_exit_status_reads_as_unknown_never_success() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-no-exit");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    engine.finish();
    engine.state.lock().unwrap().exit = None;
    assert_eq!(
        client
            .observe_gpu_probe(id.clone())
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(
        client.stop_gpu_probe(id.clone()).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    assert_eq!(engine.requests("DELETE /containers/"), 0);
    client.cleanup_gpu_probe(id.clone()).wait().unwrap();
    let replay = client.observe_gpu_probe(id).wait().unwrap();
    assert_eq!(replay.exit_code, None);
    assert_eq!(replay.stdout, "final stdout");
}

#[test]
fn legacy_sweep_preserves_probe_prefixed_containers_even_when_asked_for_the_prefix() {
    let engine = Engine::new();
    let mut population = legacy_population();
    population.push(legacy(
        '9',
        &format!("/{}old", crate::container_ownership::PROBE_NAME_PREFIX),
        owner_label("fixture-owner"),
    ));
    engine.state.lock().unwrap().legacy = population;
    let outcome = engine
        .client()
        .retire_legacy_containers(vec![
            crate::session::container::SESSION_NAME_PREFIX.to_owned(),
            crate::container_ownership::PROBE_NAME_PREFIX.to_owned(),
        ])
        .wait()
        .unwrap();
    assert_eq!(outcome.removed, 1);
    assert_eq!(outcome.preserved, 6);
    assert_eq!(outcome.unresolved, 0);
    assert_eq!(deleted_ids(&engine), vec!["a".repeat(64)]);
}

#[test]
fn boot_retirement_finishes_a_probe_whose_create_reply_was_lost_without_ever_starting_it() {
    let engine = Engine::new();
    engine.state.lock().unwrap().lose_create = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-lost-create-boot");
    assert_eq!(
        client.run_gpu_probe(helper, run).wait().unwrap_err().kind,
        ErrorKind::UnknownOutcome
    );
    drop(client);
    // The container exists in Docker's `created` state and nobody will ever
    // start it: boot adopts it under the recorded operation and removes it.
    let restarted = engine.client();
    restarted.retire_gpu_probes().wait().unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 1);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/start")), 0);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
    assert!(engine.state.lock().unwrap().body.is_none());
    let intent = helper_intent(&engine, "dri-lost-create-boot");
    assert_eq!(intent.phase, HelperPhase::Completed);
    assert_eq!(
        intent.result.as_ref().unwrap().exit_code,
        None,
        "a container that never ran has no exit status, and that is not success"
    );
    // Nothing is left to block the next probe of this kind.
    let (helper, run) = dri_probe_request("dri-after-boot");
    let id = restarted.run_gpu_probe(helper, run).wait().unwrap();
    assert_eq!(
        restarted
            .observe_gpu_probe(id.clone())
            .wait()
            .unwrap()
            .exit_code,
        Some(23)
    );
    restarted.cleanup_gpu_probe(id).wait().unwrap();
}

#[test]
fn gpu_probe_stop_is_durable_across_an_unreachable_engine_and_recovery_finishes_it() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("dri-durable-stop");
    let id = client.run_gpu_probe(helper, run).wait().unwrap();
    engine.state.lock().unwrap().unreachable = true;
    assert!(client.stop_gpu_probe(id.clone()).wait().is_err());
    assert_eq!(
        helper_intent(&engine, "dri-durable-stop").phase,
        HelperPhase::Stopping,
        "the orchestrator's termination request survives the engine outage"
    );
    assert!(engine.state.lock().unwrap().running);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 0);
    engine.state.lock().unwrap().unreachable = false;
    // The 30 s maintenance pass finishes the recorded stop, then cleanup.
    client.recover_diagnostics().wait().unwrap();
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
    assert!(engine.state.lock().unwrap().body.is_none());
    assert_eq!(
        helper_intent(&engine, "dri-durable-stop").phase,
        HelperPhase::Completed
    );
    assert_eq!(
        client.observe_gpu_probe(id).wait().unwrap().exit_code,
        Some(23)
    );
}

mod diagnostic_registration;
mod host_probes;
