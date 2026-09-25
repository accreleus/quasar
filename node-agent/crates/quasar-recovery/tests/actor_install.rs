//! The recovery actor's `resume` and `status` (seam 2 of #352's testing decisions): a GPU
//! host install against the in-memory engine and a temporary machine-state directory,
//! observed only through engine state, machine state and `status`.

mod support;

use std::os::unix::fs::PermissionsExt;
use std::sync::Arc;

use quasar_recovery::actor::ResumeError;
use quasar_recovery::engine::{EngineError, ErrorKind, FakeEngine, FakeState, Fault, When};
use quasar_recovery::recipe::names;
use quasar_recovery::socket::{DatabaseMode, MachineRole, Status};
use support::*;

fn installed(state: FakeState) -> (Arc<FakeEngine>, tempfile::TempDir) {
    let engine = Arc::new(FakeEngine::new(state));
    let dir = tempfile::tempdir().unwrap();
    actor(&engine, dir.path(), operator())
        .resume()
        .expect("a clean install");
    (engine, dir)
}

#[test]
fn a_clean_gpu_host_gets_exactly_its_node_agent_and_a_second_resume_changes_nothing() {
    let (engine, dir) = installed(amd_host());
    let state = engine.state();

    let names: Vec<_> = state.by_name().into_keys().collect();
    assert_eq!(names, [names::NODE_AGENT, names::RECOVERY_ACTOR]);
    let agent = state.container_named(names::NODE_AGENT).unwrap();
    assert_eq!(agent.status, "running");
    assert_eq!(agent.spec.image, AGENT_IMAGE);
    assert_eq!(agent.spec.labels["io.quasar.installation"], INSTALLATION);
    assert_eq!(
        agent.spec.labels["io.quasar.platform-service"],
        "node-agent"
    );
    assert_eq!(agent.spec.labels["io.quasar.recipe"], "1");
    assert!(agent.spec.labels["io.quasar.spec"].starts_with("sha256:"));
    // Same-path homes, the engine socket at its daemon-host path, the probed render node.
    assert!(agent
        .spec
        .binds
        .iter()
        .any(|b| b.source == HOME && b.target == HOME));
    assert!(agent
        .spec
        .binds
        .iter()
        .any(|b| b.source == SOCKET_HOST_PATH && b.target == "/var/run/docker.sock"));
    assert_eq!(agent.spec.env["QUASAR_RENDER_NODE"], "/dev/dri/renderD129");
    assert_eq!(agent.spec.env["NODE_NAME"], "gpu-host-01");
    for volume in [names::AGENT_DATA_VOLUME, names::NODE_AGENT_SECRETS_VOLUME] {
        assert_eq!(
            state.volumes[volume].labels["io.quasar.installation"], INSTALLATION,
            "{volume}"
        );
    }
    assert!(!state.volumes.contains_key(names::NVIDIA_DRIVER_VOLUME));

    let engine_before = engine.state();
    let files_before = tree(dir.path());
    actor(&engine, dir.path(), operator())
        .resume()
        .expect("a second resume");
    assert_eq!(engine.state(), engine_before, "the engine changed");
    assert_eq!(
        tree(dir.path()),
        files_before,
        "machine state was rewritten"
    );
}

#[test]
fn the_enrollment_string_reaches_the_agent_only_as_a_read_only_mounted_file() {
    let (engine, dir) = installed(amd_host());
    let state = engine.state();
    let agent = state.container_named(names::NODE_AGENT).unwrap();

    assert!(
        agent.spec.env.values().all(|v| !v.contains(ENROLLMENT)),
        "a secret reached the environment"
    );
    assert!(!agent.spec.env.contains_key("ENROLLMENT_TOKEN"));
    assert!(!agent.spec.env.contains_key("QUASAR_ENROLLMENT"));
    assert!(agent.spec.labels.values().all(|v| !v.contains(ENROLLMENT)));
    assert_eq!(
        agent.spec.env["QUASAR_ENROLLMENT_FILE"],
        "/run/quasar-secrets/enrollment"
    );
    let mount = agent
        .spec
        .binds
        .iter()
        .find(|b| b.target == "/run/quasar-secrets")
        .unwrap();
    assert_eq!(mount.source, names::NODE_AGENT_SECRETS_VOLUME);
    assert!(mount.read_only);
    let (content, mode) = &state.volumes[names::NODE_AGENT_SECRETS_VOLUME].files["enrollment"];
    assert_eq!(content, ENROLLMENT.as_bytes());
    assert_eq!(mode & 0o777, 0o400);
    // The agent socket volume is the actor's; the agent mounts it read-only.
    assert!(agent
        .spec
        .binds
        .iter()
        .any(|b| b.source == names::AGENT_SOCKET_VOLUME && b.read_only));

    // In machine state: a 0600 file in a 0700 directory, and nowhere else.
    let secret = dir.path().join("secrets/enrollment.json");
    let meta = std::fs::metadata(&secret).unwrap();
    assert_eq!(meta.permissions().mode() & 0o777, 0o600);
    let secrets_dir = std::fs::metadata(dir.path().join("secrets")).unwrap();
    assert_eq!(secrets_dir.permissions().mode() & 0o777, 0o700);
    for (path, (bytes, _, _)) in tree(dir.path()) {
        if path != "secrets/enrollment.json" {
            assert!(
                !String::from_utf8_lossy(&bytes).contains(ENROLLMENT),
                "{path} holds the enrollment string"
            );
        }
    }
}

#[test]
fn nothing_is_left_behind_but_the_services_and_their_volumes() {
    let (engine, _dir) = installed(amd_host());
    let state = engine.state();
    assert!(state.container_named(names::GPU_PROBE).is_none());
    assert!(state.container_named(names::SECRETS_WRITER).is_none());
    let volumes: Vec<_> = state.volumes.keys().cloned().collect();
    assert_eq!(
        volumes,
        [
            names::AGENT_DATA_VOLUME,
            names::MACHINE_VOLUME,
            names::NODE_AGENT_SECRETS_VOLUME,
            names::AGENT_SOCKET_VOLUME,
        ]
    );
}

/// Crash the actor at every engine call of a clean install, before and after the call
/// takes effect; the next start completes the install to exactly the uninterrupted state.
/// Swept on an AMD host and on an NVIDIA host, whose install adds the `--gpus` probe.
#[test]
fn an_install_interrupted_at_any_call_is_completed_by_the_next_resume() {
    for (host_name, host) in [
        ("amd", amd_host as fn() -> FakeState),
        ("nvidia", || nvidia_host(&[], true)),
    ] {
        let (reference_engine, reference_dir) = installed(host());
        let reference = reference_engine.state();
        let reference_files = contents(reference_dir.path());
        let calls = reference_engine.calls();
        assert!(
            calls > 10,
            "{host_name}: the sweep must not be vacuous ({calls} calls)"
        );

        for call in 0..calls {
            for when in [When::Before, When::After] {
                let at = format!("{host_name} call {call} {when:?}");
                let engine = Arc::new(FakeEngine::new(host()));
                let dir = tempfile::tempdir().unwrap();
                engine.inject(Fault {
                    call,
                    when,
                    error: EngineError::Crashed,
                });
                let crashed = actor(&engine, dir.path(), operator()).resume();
                assert!(crashed.is_err(), "{at}: the crash was swallowed");
                engine.clear_faults();

                actor(&engine, dir.path(), operator())
                    .resume()
                    .unwrap_or_else(|e| panic!("{at}: the next resume failed: {e}"));
                let state = engine.state();
                assert_eq!(state.by_name(), reference.by_name(), "{at}: containers");
                assert_eq!(state.volumes, reference.volumes, "{at}: volumes");
                assert_eq!(contents(dir.path()), reference_files, "{at}: machine state");
            }
        }
    }
}

#[test]
fn a_host_with_no_usable_gpu_still_gets_its_agent_without_gpu_devices() {
    let (engine, _dir) = installed(host(PROBE_NONE, &["runc"], false, &[]));
    let state = engine.state();
    let agent = state.container_named(names::NODE_AGENT).unwrap();
    assert_eq!(agent.status, "running");
    assert!(agent.spec.devices.is_empty());
    assert!(agent.spec.gpus.is_empty());
    assert_eq!(agent.spec.env["QUASAR_RENDER_NODE"], "");
}

#[test]
fn an_nvidia_host_gets_the_gpu_request_and_the_driver_volume() {
    let (engine, _dir) = installed(nvidia_host(&["nvidia", "runc"], true));
    let state = engine.state();
    let agent = state.container_named(names::NODE_AGENT).unwrap();
    assert_eq!(agent.spec.gpus.len(), 1);
    assert_eq!(agent.spec.gpus[0].count, -1);
    assert_eq!(agent.spec.env["QUASAR_GPU_NVIDIA"], "1");
    assert_eq!(agent.spec.env["QUASAR_RENDER_NODE"], "/dev/dri/renderD128");
    assert!(agent.spec.binds.iter().any(
        |b| b.source == names::NVIDIA_DRIVER_VOLUME && b.target == "/opt/quasar/nvidia-driver"
    ));
    assert_eq!(
        state.volumes[names::NVIDIA_DRIVER_VOLUME].labels["io.quasar.installation"],
        INSTALLATION
    );
}

/// The container toolkit's hook serves `--gpus` with no `nvidia` runtime and no CDI
/// device in `/info`; the started probe is what tells.
#[test]
fn a_toolkit_hook_only_nvidia_host_still_gets_the_nvidia_shape() {
    let (engine, _dir) = installed(nvidia_host(&["runc"], true));
    let state = engine.state();
    let agent = state.container_named(names::NODE_AGENT).unwrap();
    assert_eq!(agent.spec.gpus.len(), 1);
    assert_eq!(agent.spec.env["QUASAR_GPU_NVIDIA"], "1");
    assert!(state.container_named(names::GPU_PROBE).is_none());
}

#[test]
fn an_nvidia_card_the_engine_cannot_serve_is_installed_without_the_nvidia_shape() {
    // An `nvidia` runtime entry is not evidence: only a started `--gpus all` probe is.
    let (engine, dir) = installed(nvidia_host(&["nvidia", "runc"], false));
    let state = engine.state();
    let agent = state.container_named(names::NODE_AGENT).unwrap();
    assert!(agent.spec.gpus.is_empty());
    assert!(!agent.spec.env.contains_key("QUASAR_GPU_NVIDIA"));
    assert_eq!(agent.status, "running");
    // A "no" is never recorded, so an engine that gains the toolkit is asked again.
    assert!(machine_json(&dir)["inputs"]["gpu"]
        .get("gpus_served")
        .is_none());
    // Once the agent exists nothing is probed again: the second start changes nothing.
    let before = engine.state();
    actor(&engine, dir.path(), operator()).resume().unwrap();
    assert_eq!(engine.state(), before);
}

fn machine_json(dir: &tempfile::TempDir) -> serde_json::Value {
    serde_json::from_slice(&std::fs::read(dir.path().join("machine.json")).unwrap()).unwrap()
}

fn transient_500() -> EngineError {
    EngineError::Refused {
        status: 500,
        message: "failed to create task for container: ttrpc: closed".into(),
    }
}

#[test]
fn a_transient_engine_failure_of_the_gpus_probe_is_asked_again_in_the_same_start() {
    let mut state = nvidia_host(&[], true);
    state.gpus_create_failures = vec![transient_500(), EngineError::Runtime(ErrorKind::Timeout)];
    let (engine, dir) = installed(state);
    let state = engine.state();
    let agent = state.container_named(names::NODE_AGENT).unwrap();
    assert_eq!(
        agent.spec.gpus.len(),
        1,
        "the NVIDIA shape after two transient failures"
    );
    assert_eq!(machine_json(&dir)["inputs"]["gpu"]["gpus_served"], true);
}

#[test]
fn a_gpus_probe_that_never_gets_an_answer_fails_the_start_and_the_next_one_asks_again() {
    let mut state = nvidia_host(&[], true);
    state.gpus_create_failures = vec![transient_500(), transient_500(), transient_500()];
    let engine = Arc::new(FakeEngine::new(state));
    let dir = tempfile::tempdir().unwrap();
    let err = actor(&engine, dir.path(), operator()).resume().unwrap_err();
    assert!(matches!(err, ResumeError::Engine(_)), "{err}");
    assert!(engine.state().container_named(names::NODE_AGENT).is_none());
    assert!(engine.state().container_named(names::GPU_PROBE).is_none());
    assert!(machine_json(&dir)["inputs"]["gpu"]
        .get("gpus_served")
        .is_none());

    actor(&engine, dir.path(), operator()).resume().unwrap();
    let state = engine.state();
    assert_eq!(
        state
            .container_named(names::NODE_AGENT)
            .unwrap()
            .spec
            .gpus
            .len(),
        1
    );
}

/// Docker 28+ with CDI enabled refuses `--gpus all` on an engine that has no NVIDIA
/// device with its own words (measured on Docker 29.8, HTTP 500 at start); that is the
/// same definite no as the older "could not select device driver".
#[test]
fn the_cdi_refusal_of_a_gpu_request_is_a_definite_no_too() {
    let mut state = nvidia_host(&[], false);
    state.gpus_refusal_message =
        "failed to discover GPU vendor from CDI: no known GPU vendor found".into();
    let (engine, dir) = installed(state);
    let state = engine.state();
    let agent = state.container_named(names::NODE_AGENT).unwrap();
    assert!(agent.spec.gpus.is_empty());
    assert_eq!(agent.status, "running");
    assert!(machine_json(&dir)["inputs"]["gpu"]
        .get("gpus_served")
        .is_none());
}

#[test]
fn a_gpus_probe_failure_that_is_not_about_gpus_fails_the_start_without_retrying() {
    let mut state = nvidia_host(&[], true);
    // One failure only: a retry would have succeeded and hidden the refusal.
    state.gpus_create_failures = vec![EngineError::Refused {
        status: 404,
        message: format!("No such image: {AGENT_IMAGE}"),
    }];
    let engine = Arc::new(FakeEngine::new(state));
    let dir = tempfile::tempdir().unwrap();
    let err = actor(&engine, dir.path(), operator()).resume().unwrap_err();
    assert!(err.to_string().contains("404"), "{err}");
    assert!(engine.state().container_named(names::NODE_AGENT).is_none());
    assert!(machine_json(&dir)["inputs"]["gpu"]
        .get("gpus_served")
        .is_none());
}

#[test]
fn a_container_holding_the_probe_name_without_our_label_stops_the_start_and_is_reported() {
    let mut state = nvidia_host(&[], true);
    let mut squatter = state.containers[ACTOR_ID].clone();
    squatter.id = "ab00000000000000000000000000000000000000000000000000000000000000".into();
    squatter.spec.name = names::GPU_PROBE.into();
    state
        .containers
        .insert(squatter.id.clone(), squatter.clone());
    let engine = Arc::new(FakeEngine::new(state));
    let dir = tempfile::tempdir().unwrap();
    let a = actor(&engine, dir.path(), operator());

    let err = a.resume().unwrap_err();
    assert!(matches!(err, ResumeError::OwnerConflict(_)), "{err}");
    assert_eq!(engine.state().containers[&squatter.id], squatter);
    assert!(engine.state().container_named(names::NODE_AGENT).is_none());
    let status = a.status();
    assert!(
        status
            .conflicts
            .iter()
            .any(|c| c.container == names::GPU_PROBE),
        "{:?}",
        status.conflicts
    );
}

/// An engine that does not answer (unreachable, a timeout, an unknown outcome) at any call
/// of an NVIDIA install is no answer about GPUs: nothing about them is recorded, and the
/// same start (the `--gpus` probe retries) or the next one installs exactly what an
/// undisturbed install would have.
#[test]
fn an_engine_failure_during_detection_is_never_recorded_as_an_answer() {
    let (reference_engine, reference_dir) = installed(nvidia_host(&[], true));
    let reference = reference_engine.state();
    let reference_files = contents(reference_dir.path());
    for call in 0..reference_engine.calls() {
        for kind in [
            ErrorKind::Unavailable,
            ErrorKind::Timeout,
            ErrorKind::UnknownOutcome,
        ] {
            let at = format!("call {call} {kind:?}");
            let engine = Arc::new(FakeEngine::new(nvidia_host(&[], true)));
            let dir = tempfile::tempdir().unwrap();
            engine.inject(Fault {
                call,
                when: When::Before,
                error: EngineError::Runtime(kind),
            });
            let _ = actor(&engine, dir.path(), operator()).resume();
            engine.clear_faults();
            actor(&engine, dir.path(), operator())
                .resume()
                .unwrap_or_else(|e| panic!("{at}: the next resume failed: {e}"));
            assert_eq!(
                engine.state().by_name(),
                reference.by_name(),
                "{at}: containers"
            );
            assert_eq!(contents(dir.path()), reference_files, "{at}: machine state");
        }
    }
}

#[test]
fn an_unreachable_engine_is_reported_in_status_and_retried_only_on_the_next_start() {
    let mut state = amd_host();
    state.unreachable = true;
    let engine = Arc::new(FakeEngine::new(state));
    let dir = tempfile::tempdir().unwrap();
    let first = actor(&engine, dir.path(), operator());

    let err = first.resume().unwrap_err();
    assert!(
        matches!(&err, ResumeError::Engine(e) if e.is_unreachable()),
        "{err}"
    );
    assert!(
        first.holds_lease(),
        "the actor must keep its lease to serve status"
    );
    let status = first.status();
    assert!(status.stale);
    assert!(status.services.is_empty());

    engine.with_state(|s| s.unreachable = false);
    let status = first.status();
    assert!(!status.stale);
    assert!(
        status.services.iter().all(|s| s.role != "node-agent"),
        "status must not retry the install"
    );
    assert!(engine.state().container_named(names::NODE_AGENT).is_none());
    drop(first);

    actor(&engine, dir.path(), operator()).resume().unwrap();
    assert!(engine.state().container_named(names::NODE_AGENT).is_some());
}

#[test]
fn a_container_holding_the_agent_name_without_our_labels_is_never_touched() {
    let mut state = amd_host();
    let squatter = {
        let mut c = state.containers[ACTOR_ID].clone();
        c.id = "de00000000000000000000000000000000000000000000000000000000000000".into();
        c.spec.name = names::NODE_AGENT.into();
        c.spec.labels.insert(
            "com.docker.compose.service".into(),
            "quasar-node-agent".into(),
        );
        c
    };
    state
        .containers
        .insert(squatter.id.clone(), squatter.clone());
    let engine = Arc::new(FakeEngine::new(state));
    let dir = tempfile::tempdir().unwrap();
    let a = actor(&engine, dir.path(), operator());

    let err = a.resume().unwrap_err();
    assert!(matches!(err, ResumeError::OwnerConflict(_)), "{err}");
    assert_eq!(engine.state().containers[&squatter.id], squatter);
    let status = a.status();
    assert_eq!(status.conflicts.len(), 1);
    assert_eq!(status.conflicts[0].container, names::NODE_AGENT);
}

#[test]
fn an_agent_image_without_a_revision_this_actor_carries_is_refused_before_anything_is_created() {
    for label in [None, Some("3"), Some("one")] {
        let mut state = amd_host();
        state
            .registry
            .insert(AGENT_IMAGE.into(), agent_image(label));
        let engine = Arc::new(FakeEngine::new(state));
        let dir = tempfile::tempdir().unwrap();
        let err = actor(&engine, dir.path(), operator()).resume().unwrap_err();
        assert!(
            matches!(err, ResumeError::RecipeUnsupported(_)),
            "{label:?}: {err}"
        );
        assert!(engine.state().container_named(names::NODE_AGENT).is_none());
        // Nothing durable either: machine state would win over corrected inputs.
        let files: Vec<_> = tree(dir.path())
            .into_keys()
            .filter(|f| f != "actor.lease")
            .collect();
        assert!(files.is_empty(), "{label:?}: {files:?}");

        // The same machine, started again with an image the actor carries, installs.
        engine.with_state(|s| {
            s.registry
                .insert(AGENT_IMAGE.into(), agent_image(Some("1")));
            s.images.remove(AGENT_IMAGE);
        });
        actor(&engine, dir.path(), operator()).resume().unwrap();
        assert!(engine.state().container_named(names::NODE_AGENT).is_some());
    }
}

#[test]
fn a_first_install_without_its_required_inputs_says_which_and_creates_nothing() {
    for (missing, op) in [
        ("QUASAR_ENROLLMENT", {
            let mut op = operator();
            op.enrollment = None;
            op
        }),
        ("QUASAR_HOME_ROOT", {
            let mut op = operator();
            op.home_root = None;
            op
        }),
        ("QUASAR_AGENT_IMAGE", {
            let mut op = operator();
            op.agent_image =
                Some("registry.example.invalid/quasar/quasar-node-agent:latest".into());
            op
        }),
    ] {
        let engine = Arc::new(FakeEngine::new(amd_host()));
        let dir = tempfile::tempdir().unwrap();
        let err = actor(&engine, dir.path(), op).resume().unwrap_err();
        assert!(
            matches!(&err, ResumeError::Inputs(why) if why.contains(missing)),
            "{missing}: {err}"
        );
        assert_eq!(
            engine.state().by_name().len(),
            1,
            "{missing}: something was created"
        );
        assert!(!dir.path().join("machine.json").exists());
    }
}

#[test]
fn machine_state_wins_over_inputs_given_to_a_later_start() {
    let (engine, dir) = installed(amd_host());
    let before = contents(dir.path());
    let mut other = operator();
    other.enrollment = Some("qenr1..b3RoZXI.another-token".into());
    other.home_root = Some("/srv/elsewhere".into());
    actor(&engine, dir.path(), other).resume().unwrap();
    assert_eq!(contents(dir.path()), before);
    let state = engine.state();
    let agent = state.container_named(names::NODE_AGENT).unwrap();
    assert!(agent.spec.binds.iter().any(|b| b.target == HOME));
}

#[test]
fn a_second_actor_on_the_same_machine_state_is_refused_while_the_first_holds_the_lease() {
    let engine = Arc::new(FakeEngine::new(amd_host()));
    let dir = tempfile::tempdir().unwrap();
    let first = actor(&engine, dir.path(), operator());
    first.resume().unwrap();
    let second = actor(&engine, dir.path(), operator());
    assert!(matches!(second.resume(), Err(ResumeError::LeaseHeld)));
    assert!(!second.holds_lease());
}

#[test]
fn status_reports_the_machine_inventory_in_the_socket_shape() {
    let engine = Arc::new(FakeEngine::new(amd_host()));
    let dir = tempfile::tempdir().unwrap();
    let a = actor(&engine, dir.path(), operator());
    a.resume().unwrap();
    engine.with_state(|s| {
        let id = s.container_named(names::NODE_AGENT).unwrap().id.clone();
        s.containers.get_mut(&id).unwrap().health = Some("healthy".into());
    });

    let status = a.status();
    let json = serde_json::to_value(&status).unwrap();
    let again: Status = serde_json::from_value(json.clone()).unwrap();
    assert_eq!(again, status);
    assert_eq!(
        json,
        serde_json::json!({
            "actor": {
                "version": quasar_recovery::identity::version(),
                "commit": quasar_recovery::identity::source_commit(),
                "image": "registry.example.invalid/quasar/quasar-recovery",
                "digest": "sha256:cc33000000000000000000000000000000000000000000000000000000000000"
            },
            "seed": null,
            "role": "gpu",
            "node_name": "gpu-host-01",
            "database": "none",
            "services": [
                {
                    "role": "recovery-actor",
                    "container": "quasar-recovery",
                    "image": "registry.example.invalid/quasar/quasar-recovery",
                    "digest": "sha256:cc33000000000000000000000000000000000000000000000000000000000000",
                    "state": "running",
                    "health": null
                },
                {
                    "role": "node-agent",
                    "container": "quasar-node-agent",
                    "image": "registry.example.invalid/quasar/quasar-node-agent",
                    "digest": "sha256:bb22000000000000000000000000000000000000000000000000000000000000",
                    "state": "running",
                    "health": "healthy"
                }
            ],
            "conflicts": [],
            "in_flight": null,
            "dumps": [],
            "result": null,
            "stale": false
        })
    );
    assert_eq!(status.role, MachineRole::Gpu);
    assert_eq!(status.database, DatabaseMode::None);
}

#[test]
fn an_engine_that_refuses_a_call_fails_resume_without_a_partial_container_left_running() {
    let engine = Arc::new(FakeEngine::new(amd_host()));
    let dir = tempfile::tempdir().unwrap();
    // The engine refuses everything during one start; the next start still converges.
    for call in 0..200 {
        engine.inject(Fault {
            call,
            when: When::Before,
            error: EngineError::Runtime(ErrorKind::Engine),
        });
    }
    assert!(actor(&engine, dir.path(), operator()).resume().is_err());
    engine.clear_faults();
    actor(&engine, dir.path(), operator()).resume().unwrap();
    assert_eq!(
        engine
            .state()
            .container_named(names::NODE_AGENT)
            .unwrap()
            .status,
        "running"
    );
}
