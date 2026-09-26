//! The real `PlatformEngine` adapter against a real engine (seam 4 of #352's testing
//! decisions), opt-in like RH-01's real-engine tests:
//!
//! ```text
//! QUASAR_TEST_RUNTIME_SOCKET=/var/run/docker.sock \
//! QUASAR_TEST_RECOVERY_IMAGE=<a local image with sh, stat and cat> \
//! [QUASAR_TEST_RECOVERY_PULL=<repository@sha256:… reachable from the engine>] \
//!   cargo test -p quasar-recovery --test docker_engine_real -- --ignored
//! ```
//!
//! Only uniquely named containers and volumes created here are touched, and each is
//! removed on every path.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use quasar_recovery::engine::{ContainerSpec, DockerEngine, PlatformEngine, RestartPolicy};
use quasar_recovery::probe;
use quasar_recovery::recipe::{Bind, ImageRef};

fn engine() -> Arc<DockerEngine> {
    let socket = std::env::var("QUASAR_TEST_RUNTIME_SOCKET").expect("QUASAR_TEST_RUNTIME_SOCKET");
    Arc::new(DockerEngine::new(quasar_runtime::RuntimeConfig::unix(socket)).unwrap())
}

/// Both probes use the one deterministic name `quasar-gpu-probe`, as the actor does under
/// its lease; the test harness runs tests in parallel, so they take turns.
static PROBE_NAME: std::sync::Mutex<()> = std::sync::Mutex::new(());

fn image() -> String {
    std::env::var("QUASAR_TEST_RECOVERY_IMAGE").expect("QUASAR_TEST_RECOVERY_IMAGE")
}

fn unique(what: &str) -> String {
    format!(
        "quasar-recovery-test-{what}-{}",
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    )
}

struct Cleanup {
    engine: Arc<DockerEngine>,
    containers: Vec<String>,
    volumes: Vec<String>,
}

impl Drop for Cleanup {
    fn drop(&mut self) {
        for c in &self.containers {
            let _ = self.engine.remove_container(c);
        }
        for v in &self.volumes {
            let _ = self.engine.remove_volume(v);
        }
    }
}

fn spec(name: &str, image: &str, script: &str, binds: Vec<Bind>) -> ContainerSpec {
    ContainerSpec {
        name: name.into(),
        image: image.into(),
        entrypoint: Some(vec!["/bin/sh".into(), "-c".into()]),
        cmd: Some(vec![script.into()]),
        env: BTreeMap::from([("QUASAR_TEST".to_string(), "1".to_string())]),
        labels: BTreeMap::from([("io.quasar.test".to_string(), name.to_string())]),
        network_mode: Some("none".into()),
        binds,
        devices: Vec::new(),
        device_cgroup_rules: Vec::new(),
        gpus: Vec::new(),
        cap_add: Vec::new(),
        security_opt: Vec::new(),
        init: true,
        restart: RestartPolicy::No,
        ports: Vec::new(),
        healthcheck: None,
    }
}

#[test]
#[ignore = "requires QUASAR_TEST_RUNTIME_SOCKET and QUASAR_TEST_RECOVERY_IMAGE; creates and removes only uniquely named assets"]
fn containers_volumes_and_archives_round_trip_through_a_real_engine() {
    let engine = engine();
    let image = image();
    let volume = unique("volume");
    let writer = unique("writer");
    let reader = unique("reader");
    let mut cleanup = Cleanup {
        engine: engine.clone(),
        containers: vec![writer.clone(), reader.clone()],
        volumes: vec![volume.clone()],
    };

    let host = engine.host().unwrap();
    assert!(!host.runtimes.is_empty());
    assert!(engine.inspect_image(&image).unwrap().is_some());
    assert!(engine.inspect_volume(&volume).unwrap().is_none());

    let labels = BTreeMap::from([("io.quasar.test".to_string(), volume.clone())]);
    let created = engine.create_volume(&volume, &labels).unwrap();
    assert_eq!(created.labels, labels);
    assert_eq!(
        engine.inspect_volume(&volume).unwrap().unwrap().labels,
        labels
    );

    // A created, never started container: an archive uploaded into it lands in the volume.
    let bind = |ro| Bind {
        source: volume.clone(),
        target: "/secrets".into(),
        read_only: ro,
    };
    let w = engine
        .create_container(&spec(&writer, &image, "true", vec![bind(false)]))
        .unwrap();
    let c = engine.inspect_container(&writer).unwrap().unwrap();
    assert_eq!(c.id, w);
    assert_eq!(c.status, "created");
    assert_eq!(c.restart, Some(RestartPolicy::No));
    assert_eq!(c.labels["io.quasar.test"], writer);
    // What the seed is recognised by, and where a seed-created actor reads its inputs.
    assert_eq!(c.command, ["/bin/sh", "-c", "true"]);
    assert!(c.env.iter().any(|kv| kv == "QUASAR_TEST=1"), "{:?}", c.env);
    let mut tar = tar::Builder::new(Vec::new());
    let mut header = tar::Header::new_gnu();
    header.set_size(6);
    header.set_mode(0o400);
    header.set_entry_type(tar::EntryType::Regular);
    tar.append_data(&mut header, "secret", &b"s3cret"[..])
        .unwrap();
    engine
        .upload_archive(&w, "/secrets", tar.into_inner().unwrap())
        .unwrap();
    engine.remove_container(&w).unwrap();
    assert!(engine.inspect_container(&writer).unwrap().is_none());

    let r = engine
        .create_container(&spec(
            &reader,
            &image,
            "stat -c %a /secrets/secret; cat /secrets/secret; touch /secrets/x 2>/dev/null && echo writable; exit 3",
            vec![bind(true)],
        ))
        .unwrap();
    engine
        .set_restart_policy(&r, RestartPolicy::UnlessStopped)
        .unwrap();
    engine.start_container(&r).unwrap();
    assert_eq!(
        engine.wait_container(&r, Duration::from_secs(60)).unwrap(),
        3
    );
    let logs = engine.logs_tail(&r, 50).unwrap();
    assert!(logs.contains("400"), "{logs}");
    assert!(logs.contains("s3cret"), "{logs}");
    assert!(
        !logs.contains("writable"),
        "a read-only mount was writable: {logs}"
    );
    let seen = engine.inspect_container(&r).unwrap().unwrap();
    assert!(seen
        .mounts
        .iter()
        .any(|(source, target, ro)| *source == volume && target == "/secrets" && *ro));
    assert!(engine
        .list_containers()
        .unwrap()
        .iter()
        .any(|c| c.name == reader));

    let renamed = unique("renamed");
    cleanup.containers.push(renamed.clone());
    engine.rename_container(&r, &renamed).unwrap();
    assert!(engine.inspect_container(&renamed).unwrap().is_some());
    engine.stop_container(&r, Duration::from_secs(1)).unwrap();
    engine.remove_container(&r).unwrap();
    engine
        .remove_container(&r)
        .expect("removing a missing container is not an error");
    engine.remove_volume(&volume).unwrap();
    assert!(engine.inspect_volume(&volume).unwrap().is_none());
}

/// What a combined host needs of the engine (#361): a labelled bridge network on which
/// containers find each other by name, a published port, a healthcheck of the container's
/// own, and a volume's host path bound as a directory into another container.
#[test]
#[ignore = "requires QUASAR_TEST_RUNTIME_SOCKET and QUASAR_TEST_RECOVERY_IMAGE; creates and removes only uniquely named assets"]
fn networks_ports_healthchecks_and_volume_subdirectories_work_on_a_real_engine() {
    use quasar_recovery::recipe::{Healthcheck, PublishedPort};
    let engine = engine();
    let image = image();
    let network = unique("net");
    let volume = unique("sockets");
    let (server, client, writer) = (unique("server"), unique("client"), unique("writer"));
    let _cleanup = Cleanup {
        engine: engine.clone(),
        containers: vec![server.clone(), client.clone(), writer.clone()],
        volumes: vec![volume.clone()],
    };
    struct NetworkCleanup(Arc<DockerEngine>, String);
    impl Drop for NetworkCleanup {
        fn drop(&mut self) {
            let _ = self.0.remove_network(&self.1);
        }
    }
    let _net = NetworkCleanup(engine.clone(), network.clone());

    assert!(engine.inspect_network(&network).unwrap().is_none());
    let labels = BTreeMap::from([("io.quasar.test".to_string(), network.clone())]);
    engine.create_network(&network, &labels).unwrap();
    assert_eq!(
        engine.inspect_network(&network).unwrap().unwrap().labels,
        labels
    );

    let port = 20000 + (std::process::id() % 20000) as u16;
    let mut s = spec(&server, &image, "sleep 60", Vec::new());
    s.network_mode = Some(network.clone());
    s.ports = vec![PublishedPort {
        container_port: 8080,
        host_port: port,
        host_ip: Some("127.0.0.1".into()),
    }];
    s.healthcheck = Some(Healthcheck {
        test: vec!["CMD-SHELL".into(), "true".into()],
        interval_s: 1,
        timeout_s: 1,
        retries: 3,
        start_period_s: 0,
    });
    let id = engine.create_container(&s).unwrap();
    engine.start_container(&id).unwrap();
    let deadline = std::time::Instant::now() + Duration::from_secs(30);
    loop {
        let c = engine.inspect_container(&id).unwrap().unwrap();
        if c.health.as_deref() == Some("healthy") {
            break;
        }
        assert!(std::time::Instant::now() < deadline, "never healthy: {c:?}");
        std::thread::sleep(Duration::from_millis(250));
    }

    // Found by name on the network.
    let mut c = spec(
        &client,
        &image,
        &format!("getent hosts {server} || ping -c1 -W2 {server}"),
        Vec::new(),
    );
    c.network_mode = Some(network.clone());
    let cid = engine.create_container(&c).unwrap();
    engine.start_container(&cid).unwrap();
    assert_eq!(
        engine
            .wait_container(&cid, Duration::from_secs(60))
            .unwrap(),
        0
    );
    engine.remove_container(&cid).unwrap();

    // A volume's host path, one subdirectory of it bound elsewhere.
    engine.create_volume(&volume, &BTreeMap::new()).unwrap();
    let mountpoint = engine
        .inspect_volume(&volume)
        .unwrap()
        .unwrap()
        .mountpoint
        .expect("the local driver reports a host path");
    let w = spec(
        &writer,
        &image,
        "mkdir -p /v/agent /v/control && echo agent > /v/agent/x && echo control > /v/control/x",
        vec![Bind {
            source: volume.clone(),
            target: "/v".into(),
            read_only: false,
        }],
    );
    let wid = engine.create_container(&w).unwrap();
    engine.start_container(&wid).unwrap();
    assert_eq!(
        engine
            .wait_container(&wid, Duration::from_secs(60))
            .unwrap(),
        0
    );
    engine.remove_container(&wid).unwrap();
    let r = spec(
        &client,
        &image,
        "cat /only/x; ls /only",
        vec![Bind {
            source: format!("{mountpoint}/control"),
            target: "/only".into(),
            read_only: true,
        }],
    );
    let rid = engine.create_container(&r).unwrap();
    engine.start_container(&rid).unwrap();
    assert_eq!(
        engine
            .wait_container(&rid, Duration::from_secs(60))
            .unwrap(),
        0
    );
    let logs = engine.logs_tail(&rid, 20).unwrap();
    assert!(
        logs.contains("control") && !logs.contains("agent"),
        "{logs}"
    );
    engine.remove_container(&rid).unwrap();
    engine.remove_container(&id).unwrap();
    engine.remove_network(&network).unwrap();
    assert!(engine.inspect_network(&network).unwrap().is_none());
}

#[test]
#[ignore = "requires QUASAR_TEST_RUNTIME_SOCKET and QUASAR_TEST_RECOVERY_IMAGE; creates and removes one uniquely named probe"]
fn the_gpu_probe_runs_on_a_real_engine_and_is_removed() {
    let _turn = PROBE_NAME.lock().unwrap_or_else(|e| e.into_inner());
    let engine = engine();
    let image = image();
    // A tag resolves to the digest reference the engine already knows it by.
    let pinned = match ImageRef::parse(&image) {
        Ok(r) => r.reference(),
        Err(_) => engine.inspect_image(&image).unwrap().unwrap().repo_digests[0].clone(),
    };
    let reference = ImageRef::parse(&pinned).unwrap();
    let report = probe::run(engine.as_ref(), &reference).expect("the probe");
    assert!(engine
        .inspect_container(quasar_recovery::recipe::names::GPU_PROBE)
        .unwrap()
        .is_none());
    eprintln!("probe report on this engine: {report:?}");
}

/// A real engine gives the `--gpus all` probe a definite answer: served on an engine with
/// the NVIDIA toolkit, the device-request refusal on one without. Never a start failure.
#[test]
#[ignore = "requires QUASAR_TEST_RUNTIME_SOCKET and QUASAR_TEST_RECOVERY_IMAGE; creates and removes one uniquely named probe"]
fn the_gpus_probe_gets_a_definite_answer_from_a_real_engine() {
    let _turn = PROBE_NAME.lock().unwrap_or_else(|e| e.into_inner());
    let engine = engine();
    let image = image();
    let pinned = match ImageRef::parse(&image) {
        Ok(r) => r.reference(),
        Err(_) => engine.inspect_image(&image).unwrap().unwrap().repo_digests[0].clone(),
    };
    let answer = probe::serves_gpus(
        engine.as_ref(),
        &ImageRef::parse(&pinned).unwrap(),
        Duration::from_millis(200),
    )
    .expect("a definite answer");
    eprintln!("--gpus all on this engine: {answer:?}");
    assert!(engine
        .inspect_container(quasar_recovery::recipe::names::GPU_PROBE)
        .unwrap()
        .is_none());
}

/// Quasar's own Postgres, as its recipe renders it, becomes healthy with its password file
/// root:root 0400, as the actor delivers it: the image's entrypoint reads the file as root.
/// The image defaults to `recipe::control::DEFAULT_POSTGRES_IMAGE`, pulled if absent.
#[test]
#[ignore = "requires QUASAR_TEST_RUNTIME_SOCKET; pulls the default Postgres image unless QUASAR_TEST_POSTGRES_IMAGE names a local one; creates and removes only uniquely named assets"]
fn postgres_starts_healthy_with_a_root_only_password_file() {
    use quasar_recovery::recipe::{
        control, names, paths, render, secrets, ControlInputs, ControlRole, DatabaseInputs,
        GpuFacts, HostDevices, Inputs, Role, SecretMounts,
    };
    let engine = engine();
    let image = std::env::var("QUASAR_TEST_POSTGRES_IMAGE")
        .unwrap_or_else(|_| control::DEFAULT_POSTGRES_IMAGE.to_string());
    if engine.inspect_image(&image).unwrap().is_none() {
        engine.pull(&image).unwrap();
    }
    let pinned = match ImageRef::parse(&image) {
        Ok(r) => r,
        Err(_) => ImageRef::parse(&engine.inspect_image(&image).unwrap().unwrap().repo_digests[0])
            .unwrap(),
    };
    let (network, secrets_vol, data_vol) = (unique("pgnet"), unique("pgsecrets"), unique("pgdata"));
    let (writer, pg) = (unique("pgwriter"), unique("postgres"));
    let _cleanup = Cleanup {
        engine: engine.clone(),
        containers: vec![writer.clone(), pg.clone()],
        volumes: vec![secrets_vol.clone(), data_vol.clone()],
    };
    struct NetworkCleanup(Arc<DockerEngine>, String);
    impl Drop for NetworkCleanup {
        fn drop(&mut self) {
            let _ = self.0.remove_network(&self.1);
        }
    }
    engine.create_network(&network, &BTreeMap::new()).unwrap();
    let _net = NetworkCleanup(engine.clone(), network.clone());
    engine
        .create_volume(&secrets_vol, &BTreeMap::new())
        .unwrap();
    engine.create_volume(&data_vol, &BTreeMap::new()).unwrap();

    // Delivered as the actor does: a never-started container, an archive, root:root 0400.
    let w = engine
        .create_container(&spec(
            &writer,
            &pinned.reference(),
            "true",
            vec![Bind {
                source: secrets_vol.clone(),
                target: paths::SECRETS_DIR.into(),
                read_only: false,
            }],
        ))
        .unwrap();
    let password = "a-root-only-password";
    let mut tar = tar::Builder::new(Vec::new());
    let mut header = tar::Header::new_gnu();
    header.set_size(password.len() as u64);
    header.set_mode(0o400);
    header.set_uid(0);
    header.set_gid(0);
    header.set_entry_type(tar::EntryType::Regular);
    tar.append_data(&mut header, secrets::DATABASE_PASSWORD, password.as_bytes())
        .unwrap();
    engine
        .upload_archive(&w, paths::SECRETS_DIR, tar.into_inner().unwrap())
        .unwrap();
    engine.remove_container(&w).unwrap();

    let inputs = Inputs {
        installation_id: "5f0c1e0e-0c5a-4d1b-9a2f-3e4d5c6b7a89".into(),
        node_name: "control-host".into(),
        home_root: String::new(),
        template_root: "/var/lib/quasar/templates".into(),
        docker_socket: "/var/run/docker.sock".into(),
        gpu: GpuFacts {
            vendor: None,
            render_node: None,
            gpus_served: false,
            fallback: None,
        },
        devices: HostDevices {
            dri: false,
            uinput: false,
            kmsg: false,
        },
        control: Some(ControlInputs {
            machine_role: ControlRole::ControlOnly,
            trusted_proxies: None,
            http_port: 8080,
            tls_port: 8443,
            public_host: None,
            tls_hosts: None,
            database: DatabaseInputs::Owned,
        }),
        socket_dir: Some("/var/lib/docker/volumes/quasar-recovery-agent/_data".into()),
        trust: Default::default(),
        enroll: Default::default(),
        app: Default::default(),
    };
    let mounts = SecretMounts {
        volume: Some(secrets_vol.clone()),
        files: std::collections::BTreeSet::from([secrets::DATABASE_PASSWORD.to_string()]),
    };
    let mut s = render(Role::Postgres, 1, &inputs, &pinned, &mounts).unwrap();
    s.name = pg.clone();
    s.network_mode = Some(network.clone());
    s.restart = RestartPolicy::No;
    for b in &mut s.binds {
        if b.source == names::POSTGRES_DATA_VOLUME {
            b.source = data_vol.clone();
        }
    }
    assert!(s
        .binds
        .iter()
        .any(|b| b.source == secrets_vol && b.read_only));
    let id = engine.create_container(&s).unwrap();
    engine.start_container(&id).unwrap();
    let deadline = std::time::Instant::now() + Duration::from_secs(120);
    loop {
        let c = engine.inspect_container(&id).unwrap().unwrap();
        if c.health.as_deref() == Some("healthy") {
            break;
        }
        assert!(
            std::time::Instant::now() < deadline && c.status == "running",
            "Postgres never became healthy ({c:?}):\n{}",
            engine.logs_tail(&id, 50).unwrap_or_default()
        );
        std::thread::sleep(Duration::from_millis(500));
    }
    let logs = engine.logs_tail(&id, 200).unwrap();
    assert!(!logs.contains("Permission denied"), "{logs}");
}

#[test]
#[ignore = "requires QUASAR_TEST_RUNTIME_SOCKET and QUASAR_TEST_RECOVERY_PULL (a digest reference the engine can pull)"]
fn a_pull_by_digest_makes_the_image_inspectable() {
    let engine = engine();
    let Ok(reference) = std::env::var("QUASAR_TEST_RECOVERY_PULL") else {
        eprintln!("QUASAR_TEST_RECOVERY_PULL unset; nothing pulled");
        return;
    };
    engine.pull(&reference).unwrap();
    let image = engine.inspect_image(&reference).unwrap().unwrap();
    assert!(image.repo_digests.contains(&reference));
}
