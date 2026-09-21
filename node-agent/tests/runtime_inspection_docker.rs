//! Explicit local-Docker acceptance; the CLI creates only test-owned foreign fixtures.
use quasar_node_agent::runtime::{MountKind, RuntimeClient, RuntimeConfig};
use quasar_node_agent::session::{
    container::ContainerRuntime,
    homes_gc::{self, HomesGcSettings},
};
use std::{path::PathBuf, process::Command, time::Duration};
#[path = "support/inspection_cp.rs"]
mod cp;
use cp::ControlPlane;

const LABEL: &str = "io.quasar.acceptance.inspection";

struct ForeignContainer {
    socket: String,
    name: String,
    scratch: Option<tempfile::TempDir>,
}
impl ForeignContainer {
    fn docker(&self, args: &[&str]) -> std::process::Output {
        Command::new("/usr/bin/timeout")
            .args(["20", "docker", "--host", &format!("unix://{}", self.socket)])
            .args(args)
            .output()
            .expect("run bounded fixture Docker CLI")
    }
    fn inspect(&self) -> Option<serde_json::Value> {
        let result = self.docker(&["inspect", &self.name]);
        if !result.status.success() {
            return None;
        }
        serde_json::from_slice::<Vec<serde_json::Value>>(&result.stdout)
            .ok()?
            .into_iter()
            .next()
    }
}
impl Drop for ForeignContainer {
    fn drop(&mut self) {
        // Only the unique fixture label grants cleanup authority, never the name alone.
        if let Some(info) = self.inspect() {
            if info["Config"]["Labels"][LABEL].as_str() == Some(&self.name) {
                if let Some(id) = info["Id"].as_str() {
                    let removed = self.docker(&["rm", "--force", id]);
                    if removed.status.success() {
                        return;
                    }
                }
            }
        }
        // An inaccessible engine is not proof of cleanup. Keep the fixture's
        // backing files for a later owned cleanup rather than deleting a live bind.
        if let Some(scratch) = self.scratch.take() {
            let _ = scratch.keep();
        }
        eprintln!("foreign fixture cleanup unverified; test backing files retained");
    }
}

/// Sets `DOCKER_HOST` / `QUASAR_HOME_ROOT` / `NODE_SECRET_PATH` on the real process env;
/// this binary's sole test, so it is safe alone but must never run with
/// `--include-ignored` alongside another suite that shares these vars.
#[test]
#[ignore = "requires explicit local Docker endpoint, existing image and daemon-host checkout path"]
fn docker_inspection_and_foreign_mounts() {
    let socket = std::env::var("QUASAR_TEST_RUNTIME_SOCKET").expect("set test socket");
    let image = std::env::var("QUASAR_TEST_RUNTIME_IMAGE").expect("set existing image");
    let host_root = PathBuf::from(
        std::env::var("QUASAR_TEST_RUNTIME_HOST_ROOT").expect("set daemon-host checkout"),
    );
    let local_root = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .unwrap()
        .to_owned();
    let scratch = tempfile::Builder::new()
        .prefix("inspection-acceptance-")
        .tempdir_in(local_root.join(".diagnostics"))
        .unwrap();
    let shared = scratch.path().join("homes");
    let tracked = shared.join("agent-0bdc5920-fc5182ea/app");
    let throwaway = shared.join("agent-13d96fa5-8dff9894/app");
    for home in [&tracked, &throwaway] {
        std::fs::create_dir_all(home).unwrap();
        std::fs::write(home.join("state"), "preserve-user-data").unwrap();
    }
    // Only this fixture uses the alias. Removing it tests an unavailable engine
    // without interrupting Docker or any other client/workload.
    let engine_alias = scratch.path().join("engine.sock");
    std::os::unix::fs::symlink(&socket, &engine_alias).unwrap();
    // This isolated test executable owns these process environment changes.
    std::env::set_var("DOCKER_HOST", format!("unix://{}", engine_alias.display()));
    std::env::set_var("QUASAR_HOME_ROOT", &shared);
    std::env::set_var("NODE_SECRET_PATH", scratch.path().join("node-secret"));
    let host_source = host_root.join(shared.strip_prefix(&local_root).unwrap());
    let runtime = RuntimeClient::new(RuntimeConfig::unix(&engine_alias)).unwrap();
    let engine = runtime.discover().wait().unwrap();
    assert!(runtime.image_present(image.clone()).wait().unwrap());
    let metadata = runtime
        .inspect_image_metadata(image.clone())
        .wait()
        .unwrap()
        .unwrap();
    assert!(!metadata.id.is_empty());
    assert!(metadata.baked_env.iter().any(|v| v.starts_with("PATH=")));
    let legacy_runtime = ContainerRuntime::from_env();
    let installation = quasar_node_agent::buildinfo::discover_install(
        &quasar_node_agent::buildinfo::DockerFacts::new(&legacy_runtime),
    );
    assert_eq!(
        installation.install_mode,
        Some(quasar_node_agent::buildinfo::InstallMode::Source)
    );
    assert_eq!(
        installation.updater_present, None,
        "missing Compose labels remain unknown"
    );
    assert_eq!(legacy_runtime.own_image().unwrap(), metadata.id);
    assert_eq!(
        legacy_runtime.image_env(&image, "PATH"),
        metadata
            .baked_env
            .iter()
            .find_map(|value| value.strip_prefix("PATH=").map(str::to_owned))
    );

    assert!(runtime
        .engine_storage()
        .wait()
        .unwrap()
        .root
        .0
        .is_absolute());
    let fixture = ForeignContainer {
        socket,
        name: format!(
            "quasar-inspection-{}",
            scratch.path().file_name().unwrap().to_string_lossy()
        ),
        scratch: Some(scratch),
    };
    let bind = format!(
        "type=bind,src={},dst=/shared,readonly",
        host_source.display()
    );
    let label = format!("{LABEL}={}", fixture.name);
    let created = fixture.docker(&[
        "create",
        "--pull",
        "never",
        "--name",
        &fixture.name,
        "--label",
        &label,
        "--network",
        "none",
        "--mount",
        &bind,
        "--entrypoint",
        "/bin/sh",
        &image,
        "-c",
        "exec sleep 120",
    ]);
    assert!(
        created.status.success(),
        "create disposable foreign fixture"
    );
    let id = String::from_utf8(created.stdout).unwrap().trim().to_owned();
    let info = runtime
        .inspect_container(id.clone())
        .wait()
        .unwrap()
        .unwrap();
    assert_eq!(info.id, id);
    assert_eq!(info.image_id, metadata.id);
    assert_eq!(info.configured_image, image);
    assert_eq!(info.network_mode.as_deref(), Some("none"));
    assert!(!info.labels.contains_key("com.docker.compose.project"));
    assert!(!info.labels.contains_key("io.quasar.agent-owner"));
    let mount = info
        .mounts
        .iter()
        .find(|m| m.destination == "/shared")
        .unwrap();
    assert_eq!(mount.kind, MountKind::Bind);
    assert_eq!(mount.source.as_ref().unwrap().0, host_source);
    assert_eq!(mount.read_only, Some(true));
    assert!(!runtime
        .live_containers()
        .wait()
        .unwrap()
        .iter()
        .any(|c| c.id == id));
    assert!(fixture.docker(&["start", &id]).status.success());
    assert!(runtime
        .live_containers()
        .wait()
        .unwrap()
        .iter()
        .any(|c| c.id == id));
    let cp = ControlPlane::new(&tracked);
    let protected = cp.client().run_pass();
    assert_eq!(
        protected.reaped, 0,
        "foreign parent bind protects tracked home"
    );
    assert_eq!(protected.skipped_live, 1);
    assert_eq!(cp.confirmations(), 0);
    let settings = HomesGcSettings {
        root: shared.clone(),
        retention: Duration::ZERO,
        dry_run: false,
    };
    let protected = homes_gc::sweep(&settings, &ContainerRuntime::from_env());
    assert_eq!(
        protected.deleted, 0,
        "foreign parent bind protects throwaway homes"
    );
    assert_eq!(protected.skipped_live, 2);
    for home in [&tracked, &throwaway] {
        assert_eq!(
            std::fs::read_to_string(home.join("state")).unwrap(),
            "preserve-user-data"
        );
    }
    assert!(fixture
        .docker(&["stop", "--time", "1", &id])
        .status
        .success());
    assert!(!runtime
        .live_containers()
        .wait()
        .unwrap()
        .iter()
        .any(|c| c.id == id));
    std::fs::remove_file(&engine_alias).unwrap();
    let unavailable = cp.client().run_pass();
    assert_eq!(
        unavailable.reaped, 0,
        "unavailable engine cannot authorize deletion"
    );
    assert!(unavailable.liveness_error.is_some());
    assert_eq!(cp.confirmations(), 0);
    let unavailable = homes_gc::sweep(&settings, &ContainerRuntime::from_env());
    assert_eq!(unavailable.deleted, 0);
    assert_eq!(unavailable.errors, 1);
    assert!(tracked.join("state").exists());
    assert!(throwaway.join("state").exists());
    std::os::unix::fs::symlink(&fixture.socket, &engine_alias).unwrap();

    let poisoned: quasar_node_agent::session::gc::LiveRefs =
        std::sync::Arc::new(std::sync::Mutex::new(Default::default()));
    let poison = poisoned.clone();
    let _ = std::thread::spawn(move || {
        let _held = poison.lock().unwrap();
        panic!("intentional fixture liveness poison");
    })
    .join();
    let deferred = cp.client_with_live(poisoned).run_pass();
    assert_eq!(
        deferred.reaped, 0,
        "unknown in-process liveness must block deletion"
    );
    assert_eq!(cp.confirmations(), 0);
    assert!(tracked.join("state").exists());
    let collected = cp.client().run_pass();
    assert_eq!(collected.reaped, 1);
    assert_eq!(collected.confirmed, 1);
    assert_eq!(cp.confirmations(), 1);
    assert!(!tracked.exists());
    let collected = homes_gc::sweep(&settings, &ContainerRuntime::from_env());
    assert_eq!(collected.deleted, 1);
    assert!(!throwaway.exists());
    let cleanup_runtime = RuntimeClient::new(RuntimeConfig::unix(&fixture.socket)).unwrap();
    drop(fixture);
    assert!(cleanup_runtime
        .inspect_container(id)
        .wait()
        .unwrap()
        .is_none());
    println!("Docker {} API {}: metadata, missing Compose labels, daemon-host paths, foreign live/stopped mounts, unavailable engine, poisoned local liveness and both home reapers passed", engine.version, engine.api_version);
}
