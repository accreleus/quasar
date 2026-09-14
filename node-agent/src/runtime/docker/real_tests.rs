//! Opt-in Docker acceptance. Only uniquely named assets created here are mutated.
use super::*;
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
