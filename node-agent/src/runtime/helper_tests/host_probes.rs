//! Lifecycle tests for the application-GPU and audio host probes (#259) against the
//! scripted Engine double: `super::*` gives this module the fixture (`Engine`, `State`,
//! `dri_probe_request`, `audio_request`, `helper_intent`) the rest of `helper_tests`
//! already built for the GPU-probe and audio-sidecar profiles.

use super::*;
use crate::host_probe::container::Observed;
use crate::host_probe::{app_gpu, audio as audio_probe};
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Instant;

const IMAGE: &str = "quasar-agent:test";
const BOUND: Duration = Duration::from_secs(10);

fn never() -> impl Fn() -> bool {
    || false
}

#[test]
fn app_gpu_probe_happy_path_creates_under_the_prefix_and_removes_after() {
    let engine = Engine::new();
    let client = engine.client();
    let (_, run) = dri_probe_request("unused-operation-half");
    let end = app_gpu::run(&client, IMAGE, run, "happy", BOUND, &never());

    match &end.observed {
        Observed::Exited(result) => assert_eq!(result.exit_code, Some(23)),
        other => panic!("{other:?}"),
    }
    assert!(end.reconciled);

    let name = engine.state.lock().unwrap().name.clone();
    assert!(
        name.starts_with(&format!(
            "{}app-gpu-happy",
            crate::container_ownership::PROBE_NAME_PREFIX
        )),
        "{name}"
    );
    assert!(
        engine.state.lock().unwrap().body.is_none(),
        "the probe container is removed once observed"
    );
}

#[test]
fn a_deadline_stops_and_removes_exactly_once() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (_, run) = dri_probe_request("unused");
    let end = app_gpu::run(
        &client,
        IMAGE,
        run,
        "deadline",
        Duration::from_millis(250),
        &never(),
    );
    assert_eq!(end.observed, Observed::Deadline);
    assert!(end.reconciled);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
    assert!(engine.state.lock().unwrap().body.is_none());
}

#[test]
fn preemption_mid_observe_stops_and_removes() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (_, run) = dri_probe_request("unused");
    let flag = Arc::new(AtomicBool::new(false));
    let flipper = flag.clone();
    let flipper_thread = thread::spawn(move || {
        thread::sleep(Duration::from_millis(50));
        flipper.store(true, Ordering::SeqCst);
    });
    let preempted = || flag.load(Ordering::SeqCst);
    let end = app_gpu::run(&client, IMAGE, run, "preempt", BOUND, &preempted);
    flipper_thread.join().unwrap();

    assert_eq!(end.observed, Observed::Preempted);
    assert!(end.reconciled);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

/// `app_gpu::run`'s own `Busy` mapping (`Err(error) if error.kind == ErrorKind::Busy`) is
/// exercised directly in `app_gpu::tests` against a hand-built `ContainerProbeEnd` — this
/// runtime-level analogue is the closest reachable equivalent: an earlier unreconciled
/// GPU probe, genuinely still running, makes step 1 (`recover_diagnostics`) itself fail
/// (recovery cannot bring a live container to a terminal state), so the run never gets
/// far enough to attempt a second create. `run_gpu_probe`'s own pre-create `Busy` gate
/// (proven directly against the runtime by
/// `no_second_gpu_probe_starts_while_an_earlier_one_is_unreconciled`) is reached only when
/// recovery itself does not error on the earlier record first — which, for the
/// `ApplicationGpu` profile, recovery never does while that record is still genuinely
/// `Running` (unlike `Audio`, `GpuProbe` has no live-phase exemption in
/// `docker/helpers.rs::recover_entry`). Either way the safety property holds: no second
/// create while an earlier probe is unreconciled.
#[test]
fn an_earlier_unreconciled_probe_blocks_a_new_one_without_a_second_create() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let (helper, run) = dri_probe_request("first-app-gpu");
    let first = client.run_gpu_probe(helper, run).wait().unwrap();
    assert_eq!(engine.requests("POST /containers/create"), 1);

    let (_, second_run) = nvidia_probe_request();
    let end = app_gpu::run(&client, IMAGE, second_run, "second", BOUND, &never());
    assert!(
        matches!(end.observed, Observed::RuntimeError(_)),
        "{:?}",
        end.observed
    );
    assert!(!end.reconciled);
    assert_eq!(
        engine.requests("POST /containers/create"),
        1,
        "no second create while the first is unreconciled"
    );

    client.stop_gpu_probe(first.clone()).wait().unwrap();
    client.cleanup_gpu_probe(first).wait().unwrap();
}

#[test]
fn a_lost_stop_reply_leaves_reconciled_false() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    engine.state.lock().unwrap().lose_stop = true;
    let client = engine.client();
    let (_, run) = dri_probe_request("unused");
    let end = app_gpu::run(
        &client,
        IMAGE,
        run,
        "loststop",
        Duration::from_millis(250),
        &never(),
    );
    assert_eq!(end.observed, Observed::Deadline);
    assert!(
        !end.reconciled,
        "a lost stop reply must not report reconciled"
    );
}

#[test]
fn restart_recovery_finishes_a_lost_stop_under_the_original_identity() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    engine.state.lock().unwrap().lose_stop = true;
    // The cleanup `app_gpu::run` itself attempts right after the failed stop also loses
    // its reply, so the journal is genuinely NOT Completed when `run` returns — leaving
    // real work for a later `recover_diagnostics` to finish, rather than a no-op.
    engine.state.lock().unwrap().lose_remove = true;
    let client = engine.client();
    let (_, run) = dri_probe_request("unused");
    let end = app_gpu::run(
        &client,
        IMAGE,
        run,
        "restart",
        Duration::from_millis(250),
        &never(),
    );
    assert!(!end.reconciled);
    assert_ne!(
        helper_intent(&engine, "app-gpu-probe-restart").phase,
        HelperPhase::Completed,
        "both replies were lost: nothing here confirmed completion yet"
    );
    let creates_before = engine.requests("POST /containers/create");

    // A fresh client over the same state dir, as `HostProbeRunner::reconcile` builds
    // when the orchestrator reconciles a container-probe kind before its next run.
    let fresh = RuntimeClient::new(engine.config.clone()).unwrap();
    fresh.recover_diagnostics().wait().unwrap();
    assert_eq!(
        engine.requests("POST /containers/create"),
        creates_before,
        "recovery never mints a new probe identity"
    );
    assert!(engine.state.lock().unwrap().body.is_none());
    assert_eq!(
        helper_intent(&engine, "app-gpu-probe-restart").phase,
        HelperPhase::Completed
    );
}

/// Binds a real Unix listener at `socket_dir`'s eventual `native` path, simulating the
/// sidecar's own socket creation. `wait_for_socket` does a real `connect(2)`, so a bare
/// file will not do.
fn bind_socket_once_ready(
    socket_dir: std::path::PathBuf,
) -> thread::JoinHandle<Option<std::os::unix::net::UnixListener>> {
    thread::spawn(move || {
        let until = Instant::now() + Duration::from_secs(3);
        let native = socket_dir.join("native");
        loop {
            match std::os::unix::net::UnixListener::bind(&native) {
                Ok(listener) => return Some(listener),
                Err(_) if Instant::now() < until => thread::sleep(Duration::from_millis(10)),
                Err(_) => return None,
            }
        }
    })
}

fn probe_runtime_dir(engine: &Engine) -> std::path::PathBuf {
    engine
        .config
        .image_state_path
        .as_ref()
        .unwrap()
        .parent()
        .unwrap()
        .to_owned()
}

#[test]
fn audio_probe_identity_and_a_ready_socket_pass_then_removes() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let runtime_dir = probe_runtime_dir(&engine);
    let socket_dir =
        crate::session::audio::pulse_socket_dir(&runtime_dir.to_string_lossy(), "probe-ready");
    let socket_thread = bind_socket_once_ready(socket_dir);

    let end = audio_probe::run(
        &client,
        IMAGE,
        &runtime_dir.to_string_lossy(),
        "ready",
        &never(),
    );
    let _listener = socket_thread.join().unwrap();

    assert_eq!(end.observed, Observed::SocketReady);
    assert!(end.reconciled);
    let name = engine.state.lock().unwrap().name.clone();
    assert_eq!(name, "quasar-pulse-probe-ready");
    let body = engine.state.lock().unwrap().body.clone();
    assert!(body.is_none(), "the sidecar is stopped and removed");
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn audio_probe_socket_timeout_is_still_stopped_and_removed() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let runtime_dir = probe_runtime_dir(&engine);
    let end = audio_probe::run(
        &client,
        IMAGE,
        &runtime_dir.to_string_lossy(),
        "timeout",
        &never(),
    );
    assert_eq!(end.observed, Observed::SocketTimeout);
    assert!(end.reconciled);
    assert_eq!(engine.requests(&format!("POST /containers/{ID}/stop")), 1);
    assert_eq!(engine.requests("DELETE /containers/"), 1);
}

#[test]
fn audio_probe_recovers_a_stale_sidecar_before_its_own_create() {
    let engine = Engine::new();
    engine.state.lock().unwrap().keep_running = true;
    let client = engine.client();
    let runtime_dir = probe_runtime_dir(&engine);
    let runtime_dir_str = runtime_dir.to_string_lossy().into_owned();

    // A stale probe-shaped sidecar journal, stuck mid-stop as the durable-recovery
    // precedent (`routine_audio_recovery_finishes_a_stop_whose_reply_was_lost...`) sets
    // one up: the first stop attempt fails before it takes effect.
    let stale_id = "probe-stale";
    let stale_socket = crate::session::audio::pulse_socket_dir(&runtime_dir_str, stale_id);
    let stale_helper = DiagnosticHelper {
        operation: "audio-probe-stale".into(),
        name: crate::session::audio::pulse_container_name(stale_id),
        image: IMAGE.into(),
    };
    let stale_run = AudioRun {
        socket_dir: stale_socket.clone(),
        entrypoint: vec!["pulseaudio".into()],
        command: crate::session::audio::pulse_command(&stale_socket.to_string_lossy()),
    };
    let stale = client
        .run_audio_sidecar(stale_helper, stale_run)
        .wait()
        .unwrap();
    engine.state.lock().unwrap().lose_stop_before_effect = true;
    assert!(client.stop_audio_sidecar(stale.clone()).wait().is_err());
    assert!(engine.state.lock().unwrap().running);

    let creates_before_probe = engine.requests("POST /containers/create");
    let end = audio_probe::run(&client, IMAGE, &runtime_dir_str, "after-stale", &never());

    // The recovery step ran to completion (its own stop+cleanup HTTP calls) strictly
    // before the new probe's create, because `audio::run` calls it as its first line —
    // a request-log inspection double-checks that ordering held in practice.
    let requests = engine.state.lock().unwrap().requests.clone();
    let recovered_stop = requests
        .iter()
        .position(|r| r.starts_with(&format!("POST /containers/{ID}/stop")))
        .expect("recovery must have re-issued the lost stop");
    let new_probe_create = requests
        .iter()
        .rposition(|r| r.starts_with("POST /containers/create"))
        .expect("the new probe must have created its own container");
    assert!(
        recovered_stop < new_probe_create,
        "recovery's stop must precede the new probe's create: {requests:?}"
    );
    assert_eq!(
        helper_intent(&engine, "audio-probe-stale").phase,
        HelperPhase::Completed
    );
    assert_eq!(
        engine.requests("POST /containers/create"),
        creates_before_probe + 1,
        "recovery reconciles the stale sidecar; it never creates a second one for it"
    );
    // The new probe's own outcome is incidental to this test (no socket was bound for
    // it), but it must still have run cleanly end to end.
    assert!(matches!(
        end.observed,
        Observed::SocketReady | Observed::SocketTimeout
    ));
}
