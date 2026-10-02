//! Both modes of the binary stop on SIGTERM and SIGINT with exit 0, promptly, even in the
//! middle of an engine call that never answers. As PID 1 without a handler they ignored
//! both, so every `docker stop` waited out its timeout and killed them.

use std::os::unix::net::UnixListener;
use std::path::Path;
use std::process::{Command, Stdio};
use std::sync::mpsc;
use std::time::{Duration, Instant};

/// An engine socket that accepts connections and never answers.
fn hanging_engine(path: &Path) -> mpsc::Receiver<()> {
    let listener = UnixListener::bind(path).unwrap();
    let (seen, connected) = mpsc::channel();
    std::thread::spawn(move || {
        let mut held = Vec::new();
        for stream in listener.incoming().flatten() {
            held.push(stream);
            let _ = seen.send(());
        }
    });
    connected
}

fn stops(mode: &str, signal: &str) {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("engine.sock");
    let connected = hanging_engine(&socket);
    let machine = dir.path().join("machine");
    std::fs::create_dir(&machine).unwrap();
    std::fs::create_dir(dir.path().join("docker-config")).unwrap();

    let mut child = Command::new(env!("CARGO_BIN_EXE_quasar-recovery"))
        .arg(mode)
        .env("DOCKER_HOST", format!("unix://{}", socket.display()))
        .env("DOCKER_CONFIG", dir.path().join("docker-config"))
        .env("QUASAR_MACHINE_DIR", &machine)
        .env("RUST_LOG", "info")
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    connected
        .recv_timeout(Duration::from_secs(20))
        .unwrap_or_else(|_| panic!("{mode}: never reached the engine"));

    let sent = Instant::now();
    let kill = Command::new("kill")
        .arg(format!("-{signal}"))
        .arg(child.id().to_string())
        .status()
        .unwrap();
    assert!(kill.success());
    let status = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break status;
        }
        if sent.elapsed() > Duration::from_secs(5) {
            let _ = child.kill();
            panic!("{mode}: still running 5 s after {signal}");
        }
        std::thread::sleep(Duration::from_millis(20));
    };
    let mut log = String::new();
    std::io::Read::read_to_string(&mut child.stderr.take().unwrap(), &mut log).unwrap();
    assert_eq!(status.code(), Some(0), "{mode} after {signal}: {log}");
    assert!(log.contains("stopping"), "{mode} after {signal}: {log}");
}

#[test]
fn the_seed_stops_on_sigterm_and_sigint() {
    stops("seed", "TERM");
    stops("seed", "INT");
}

#[test]
fn the_actor_stops_on_sigterm_and_sigint_mid_install() {
    stops("actor", "TERM");
    stops("actor", "INT");
}
