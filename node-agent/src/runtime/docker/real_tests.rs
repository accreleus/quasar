//! Opt-in Docker acceptance. Only uniquely named assets created here are mutated.
use super::*;
use crate::runtime::ApplicationRequest;
use crate::runtime::RuntimeClient;
use std::io::{Read, Write};
use std::sync::{
    atomic::{AtomicBool, Ordering},
    Arc,
};
use std::time::Duration;

fn digest(bytes: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    format!(
        "sha256:{}",
        Sha256::digest(bytes)
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect::<String>()
    )
}

struct Registry {
    port: u16,
    manifest_digest: String,
    stopped: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
}
impl Registry {
    fn start(config: Vec<u8>) -> Self {
        let config_digest = digest(&config);
        let manifest = serde_json::to_vec(&serde_json::json!({
            "schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",
            "config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":config.len(),"digest":config_digest},"layers":[]
        })).unwrap();
        let manifest_digest = digest(&manifest);
        let listener = std::net::TcpListener::bind((std::net::Ipv4Addr::LOCALHOST, 0)).unwrap();
        let port = listener.local_addr().unwrap().port();
        listener.set_nonblocking(true).unwrap();
        let stopped = Arc::new(AtomicBool::new(false));
        let stop = stopped.clone();
        let expected_manifest = manifest_digest.clone();
        let thread = std::thread::spawn(move || {
            while !stop.load(Ordering::Relaxed) {
                let (mut socket, _) = match listener.accept() {
                    Ok(value) => value,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        std::thread::sleep(Duration::from_millis(10));
                        continue;
                    }
                    Err(e) => panic!("registry accept: {e}"),
                };
                socket
                    .set_read_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                let mut request = Vec::new();
                while !request.ends_with(b"\r\n\r\n") {
                    let mut b = [0];
                    if socket.read_exact(&mut b).is_err() {
                        break;
                    }
                    request.push(b[0]);
                    if request.len() > 16384 {
                        break;
                    }
                }
                let request = String::from_utf8_lossy(&request);
                let first = request.lines().next().unwrap_or("");
                let path = first.split_whitespace().nth(1).unwrap_or("");
                let (status, body, media, hash) = if path.contains("/manifests/") {
                    (
                        200,
                        manifest.as_slice(),
                        "application/vnd.oci.image.manifest.v1+json",
                        manifest_digest.as_str(),
                    )
                } else if path.ends_with(&format!("/blobs/{config_digest}")) {
                    (
                        200,
                        config.as_slice(),
                        "application/octet-stream",
                        config_digest.as_str(),
                    )
                } else if path == "/v2/" {
                    (200, b"{}".as_slice(), "application/json", "")
                } else {
                    (404, b"{}".as_slice(), "application/json", "")
                };
                let _ = write!(socket,"HTTP/1.1 {status} OK\r\nContent-Type: {media}\r\nContent-Length: {}\r\nDocker-Content-Digest: {hash}\r\nDocker-Distribution-Api-Version: registry/2.0\r\nConnection: close\r\n\r\n",body.len());
                if !first.starts_with("HEAD ") {
                    let _ = socket.write_all(body);
                }
            }
        });
        Self {
            port,
            manifest_digest: expected_manifest,
            stopped,
            thread: Some(thread),
        }
    }
}
impl Drop for Registry {
    fn drop(&mut self) {
        self.stopped.store(true, Ordering::Relaxed);
        self.thread.take().unwrap().join().unwrap();
    }
}

struct Assets {
    executor: tokio::runtime::Runtime,
    docker: Docker,
    container: Option<String>,
    references: Vec<String>,
}
impl Drop for Assets {
    fn drop(&mut self) {
        self.executor.block_on(async {
            if let Some(id) = &self.container {
                let _ = self.docker.remove_container(id, None).await;
            }
            for reference in &self.references {
                let _ = self
                    .docker
                    .remove_image(
                        reference,
                        Some(bollard::query_parameters::RemoveImageOptions {
                            noprune: true,
                            ..Default::default()
                        }),
                        None,
                    )
                    .await;
            }
        });
    }
}

#[test]
#[ignore = "requires explicit test Docker socket and host networking; creates and removes only unique fixture assets"]
fn real_docker_pull_reuse_in_use_refusal_and_remove() {
    let socket =
        std::env::var("QUASAR_TEST_RUNTIME_SOCKET").expect("set explicit local test socket");
    let unique = format!(
        "quasar-runtime-{}",
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    );
    let arch = match std::env::consts::ARCH {
        "x86_64" => "amd64",
        "aarch64" => "arm64",
        v => v,
    };
    let config_bytes = serde_json::to_vec(&serde_json::json!({"architecture":arch,"os":"linux","config":{"Labels":{"quasar.test":unique},"Cmd":["/fixture"]},"rootfs":{"type":"layers","diff_ids":[]}})).unwrap();
    let expected_id = digest(&config_bytes);
    let registry = Registry::start(config_bytes);
    let reference = format!("127.0.0.1:{}/{}:test", registry.port, unique);
    let alias = format!("{unique}:alias");
    let dir = tempfile::tempdir().unwrap();
    let mut config = RuntimeConfig::unix(socket);
    config.image_state_path = Some(dir.path().join("operations"));
    let executor = tokio::runtime::Runtime::new().unwrap();
    let (docker, engine) = executor.block_on(discover(&config)).unwrap();
    let mut assets = Assets {
        executor,
        docker,
        container: None,
        references: vec![reference.clone(), alias.clone()],
    };
    let runtime = RuntimeClient::new(config).unwrap();
    let budget = Duration::from_secs(30);
    assert!(!runtime.image_present(&reference).wait().unwrap());
    let pulled_id = runtime
        .ensure_image(&reference, budget)
        .wait(|_| {})
        .unwrap()
        .id;
    // Classic Docker identifies by config; the containerd image store uses manifest IDs.
    assert!(pulled_id == expected_id || pulled_id == registry.manifest_digest);
    // Stop the registry: a second ensure must reuse the image without contacting it.
    drop(registry);
    assert_eq!(
        runtime
            .ensure_image(&reference, budget)
            .wait(|_| {})
            .unwrap()
            .id,
        pulled_id
    );
    assets
        .executor
        .block_on(assets.docker.tag_image(
            &reference,
            Some(bollard::query_parameters::TagImageOptions {
                repo: Some(unique.clone()),
                tag: Some("alias".into()),
            }),
        ))
        .unwrap();
    let container = assets
        .executor
        .block_on(assets.docker.create_container(
            Some(bollard::query_parameters::CreateContainerOptions {
                name: Some(unique),
                ..Default::default()
            }),
            bollard::models::ContainerCreateBody {
                image: Some(reference.clone()),
                ..Default::default()
            },
        ))
        .unwrap();
    assets.container = Some(container.id);
    assert_eq!(
        runtime
            .remove_image(&reference, budget)
            .wait()
            .unwrap_err()
            .kind,
        ErrorKind::ImageInUse
    );
    assert!(runtime.image_present(&reference).wait().unwrap());
    assets
        .executor
        .block_on(
            assets
                .docker
                .remove_container(assets.container.as_ref().unwrap(), None),
        )
        .unwrap();
    assets.container = None;
    runtime.remove_image(&reference, budget).wait().unwrap();
    assert!(!runtime.image_present(&reference).wait().unwrap());
    runtime.remove_image(&reference, budget).wait().unwrap();
    runtime.remove_image(&alias, budget).wait().unwrap();
    println!("Docker {} API {}: pull, offline reuse, multi-tag in-use refusal, removal and absent removal passed",engine.version,engine.api_version);
}

/// Keep the exact-operation journal if teardown cannot be proved, including
/// assertion unwinding after a lost create reply. The parent is explicitly
/// host-backed so an ephemeral test runner cannot erase cleanup obligations.
struct ApplicationAssets {
    runtime: RuntimeClient,
    operation: String,
    journal: Option<tempfile::TempDir>,
    cleaned: bool,
}

impl Drop for ApplicationAssets {
    fn drop(&mut self) {
        if self.cleaned {
            return;
        }
        if let Err(error) = self
            .runtime
            .abandon_application(self.operation.clone())
            .wait()
        {
            if let Some(journal) = self.journal.take() {
                let retained = journal.keep();
                eprintln!(
                    "application smoke cleanup pending: {error}; operation={}, journal={}",
                    self.operation,
                    retained.display()
                );
            }
        }
    }
}

#[test]
#[ignore = "requires explicit test socket, application image, and host-backed QUASAR_TEST_APPLICATION_STATE_DIR; creates one unique owned application"]
fn real_docker_application_runtime_lifecycle() {
    let socket =
        std::env::var("QUASAR_TEST_RUNTIME_SOCKET").expect("set explicit local test socket");
    let image =
        std::env::var("QUASAR_TEST_APPLICATION_IMAGE").expect("set explicit local test image");
    let state_dir = std::env::var("QUASAR_TEST_APPLICATION_STATE_DIR")
        .expect("set an existing host-backed directory for durable test cleanup");
    let unique = format!(
        "quasar-sess-runtime-smoke-{}",
        crate::runtime::builds::build_id()
    );
    let dir = tempfile::Builder::new()
        .prefix("application-smoke-")
        .tempdir_in(state_dir)
        .unwrap();
    let mut config = RuntimeConfig::unix(socket);
    config.image_state_path = Some(dir.path().join("operations"));
    config.diagnostic_owner = Some(format!("runtime-smoke-{unique}"));
    config.deadline = Duration::from_secs(20);
    let runtime = RuntimeClient::new(config).unwrap();
    let request = ApplicationRequest {
        operation: format!("application-{unique}"),
        name: unique,
        image,
        pull_never: true,
        command: vec![
            "sh".into(),
            "-c".into(),
            "echo runtime-smoke-final; exit 0".into(),
        ],
        ..Default::default()
    };
    let mut assets = ApplicationAssets {
        runtime: runtime.clone(),
        operation: request.operation.clone(),
        journal: Some(dir),
        cleaned: false,
    };
    let id = runtime.start_application(request).wait().unwrap();
    let result = runtime.observe_application(id.clone()).wait().unwrap();
    assert_eq!(result.exit_code, Some(0));
    assert!(result.stdout.contains("runtime-smoke-final"));
    runtime.cleanup_application(id).wait().unwrap();
    assets.cleaned = true;
}

#[test]
#[ignore = "requires explicit local test Docker socket; classic builder, unique disposable image only"]
fn real_docker_classic_build_context_args_failure_and_verification() {
    use crate::runtime::BuildRequest;
    let socket =
        std::env::var("QUASAR_TEST_RUNTIME_SOCKET").expect("set explicit local test socket");
    let unique = format!("quasar-build-test-{}", crate::runtime::builds::build_id());
    let reference = format!("{unique}:test");
    let dir = tempfile::tempdir().unwrap();
    let context = dir.path().join("context");
    std::fs::create_dir(&context).unwrap();
    std::fs::write(
        context.join("Dockerfile"),
        "FROM scratch\nARG MESSAGE\nLABEL fixture.message=$MESSAGE\nCOPY payload /payload\n",
    )
    .unwrap();
    std::fs::write(context.join("payload"), &unique).unwrap();
    std::fs::write(context.join("secret"), "excluded").unwrap();
    std::fs::write(context.join(".dockerignore"), "secret\n").unwrap();
    let mut config = RuntimeConfig::unix(socket);
    config.image_state_path = Some(dir.path().join("operations"));
    let executor = tokio::runtime::Runtime::new().unwrap();
    let (docker, engine) = executor.block_on(discover(&config)).unwrap();
    let assets = Assets {
        executor,
        docker,
        container: None,
        references: vec![reference.clone()],
    };
    let runtime = RuntimeClient::new(config).unwrap();
    let request = BuildRequest {
        tag: reference.clone(),
        context_dir: context.clone(),
        dockerfile: "Dockerfile".into(),
        build_args: std::collections::BTreeMap::from([("MESSAGE".into(), "hello API".into())]),
    };
    let result = runtime
        .build_image(request.clone(), Duration::from_secs(60))
        .wait(|_| {})
        .unwrap();
    let image = assets
        .executor
        .block_on(assets.docker.inspect_image(&reference))
        .unwrap();
    assert_eq!(image.id.as_deref(), Some(result.id.as_str()));
    assert!(result.bytes > 0);
    let labels = image.config.unwrap().labels.unwrap();
    assert_eq!(labels["fixture.message"], "hello API");
    assert!(labels.contains_key("io.quasar.build-operation"));
    // Missing COPY source must fail; an old tag cannot turn failure into readiness.
    std::fs::write(
        context.join("Dockerfile"),
        "FROM scratch\nCOPY secret /secret\n",
    )
    .unwrap();
    assert_eq!(
        runtime
            .build_image(request.clone(), Duration::from_secs(60))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::BuildFailed
    );
    std::fs::write(context.join("Dockerfile"), "THIS_IS_INVALID\n").unwrap();
    assert_eq!(
        runtime
            .build_image(request, Duration::from_secs(60))
            .wait(|_| {})
            .unwrap_err()
            .kind,
        ErrorKind::BuildFailed
    );
    runtime
        .remove_image(&reference, Duration::from_secs(30))
        .wait()
        .unwrap();
    assert!(!runtime.image_present(&reference).wait().unwrap());
    eprintln!("classic build acceptance passed: engine={engine:?}");
}

/// The groups a session's application container is given for the daemon host's
/// DRM nodes, by the same rule the launcher applies (`dri_group_granted`).
fn local_dri_groups() -> Vec<u32> {
    use std::os::unix::fs::MetadataExt;
    let mut groups: Vec<u32> = std::fs::read_dir("/dev/dri")
        .into_iter()
        .flatten()
        .flatten()
        .filter(|entry| {
            let name = entry.file_name();
            let name = name.to_string_lossy();
            name.starts_with("renderD") || name.starts_with("card")
        })
        .filter_map(|entry| std::fs::metadata(entry.path()).ok())
        .filter(|md| crate::session::container::dri_group_granted(md.mode(), md.gid()))
        .map(|md| md.gid())
        .collect();
    groups.sort_unstable();
    groups.dedup();
    groups
}

#[test]
#[ignore = "requires explicit local test Docker socket, QUASAR_TEST_APPLICATION_IMAGE and /dev/dri on the daemon host; creates and removes one unique owned probe container"]
fn real_docker_gpu_probe_profile_runs_with_dri_access_and_cleans_up() {
    use crate::runtime::{DiagnosticHelper, GpuProbeRun};
    let socket =
        std::env::var("QUASAR_TEST_RUNTIME_SOCKET").expect("set explicit local test socket");
    let image =
        std::env::var("QUASAR_TEST_APPLICATION_IMAGE").expect("set explicit local test image");
    assert!(
        std::path::Path::new("/dev/dri").is_dir(),
        "the daemon host must expose /dev/dri for the DRI arm of the profile"
    );
    let groups = local_dri_groups();
    let unique = format!("gpu-probe-smoke-{}", crate::runtime::builds::build_id());
    let dir = tempfile::tempdir().unwrap();
    let mut config = RuntimeConfig::unix(socket);
    config.image_state_path = Some(dir.path().join("operations"));
    config.diagnostic_owner = Some(format!("runtime-smoke-{unique}"));
    config.deadline = Duration::from_secs(20);
    let executor = tokio::runtime::Runtime::new().unwrap();
    let (docker, _) = executor.block_on(discover(&config)).unwrap();
    let runtime = RuntimeClient::new(config).unwrap();
    let helper = DiagnosticHelper {
        operation: unique.clone(),
        name: format!("{}{unique}", crate::container_ownership::PROBE_NAME_PREFIX),
        image,
    };
    let run = GpuProbeRun {
        entrypoint: vec!["timeout".into()],
        command: vec![
            "20s".into(),
            "sh".into(),
            "-c".into(),
            "ls /dev/dri >/dev/null && id -G && exit 23".into(),
        ],
        devices: vec!["/dev/dri".into()],
        groups: groups.clone(),
        nvidia_device_request: false,
        nvidia: None,
    };
    let id = runtime.run_gpu_probe(helper, run).wait().unwrap();
    let container = id.as_str().to_owned();
    let result = runtime.observe_gpu_probe(id.clone()).wait();
    let cleanup = runtime.cleanup_gpu_probe(id).wait();
    // Whatever happened above, never leave the fixture behind.
    let gone = executor.block_on(async {
        let _ = docker
            .remove_container(
                &container,
                Some(bollard::query_parameters::RemoveContainerOptions {
                    force: true,
                    ..Default::default()
                }),
            )
            .await;
        docker.inspect_container(&container, None).await.is_err()
    });
    let result = result.unwrap();
    assert_eq!(result.exit_code, Some(23), "stderr: {}", result.stderr);
    for gid in groups {
        assert!(
            result
                .stdout
                .split_whitespace()
                .any(|v| v == gid.to_string()),
            "the probe runs with group {gid}: {}",
            result.stdout
        );
    }
    cleanup.unwrap();
    assert!(gone, "the owned probe container was removed");
}
