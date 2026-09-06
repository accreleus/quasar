//! A real process death must release the provisioning lock immediately. A marker
//! written by a killed container is not evidence that another provision is alive.
use quasar_node_agent::artifact::Lock;
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

struct ChildGuard(Child);
impl Drop for ChildGuard {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

#[test]
fn killed_provisioner_releases_kernel_lock_without_waiting_for_marker_age() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join(".provision.lock");
    let ready = dir.path().join("ready");
    let mut child = ChildGuard(
        Command::new(std::env::current_exe().unwrap())
            .args(["--ignored", "--exact", "child_holds_provisioning_lock"])
            .env("QUASAR_LOCK_TEST_DIR", dir.path())
            .stdout(Stdio::null())
            .spawn()
            .unwrap(),
    );
    let deadline = Instant::now() + Duration::from_secs(10);
    while !ready.exists() {
        assert!(child.0.try_wait().unwrap().is_none(), "lock holder exited");
        assert!(Instant::now() < deadline, "lock holder never became ready");
        std::thread::sleep(Duration::from_millis(10));
    }
    let marker = std::fs::read_to_string(&path).unwrap();
    assert!(marker.contains("kernel_lock=1"));
    assert!(Lock::acquire(&path, "test driver").is_err());
    assert_eq!(std::fs::read_to_string(&path).unwrap(), marker);

    // Child::kill sends SIGKILL on Unix: Drop cannot remove the marker.
    child.0.kill().unwrap();
    child.0.wait().unwrap();
    assert!(path.exists(), "SIGKILL unexpectedly cleaned up the marker");
    let started = Instant::now();
    let recovered = Lock::acquire(&path, "test driver").unwrap();
    assert!(started.elapsed() < Duration::from_secs(2));
    drop(recovered);
    assert!(!path.exists());
}

#[test]
#[ignore = "subprocess fixture, invoked by the process-death regression"]
fn child_holds_provisioning_lock() {
    let dir = PathBuf::from(std::env::var_os("QUASAR_LOCK_TEST_DIR").unwrap());
    let _guard = Lock::acquire(&dir.join(".provision.lock"), "test driver").unwrap();
    std::fs::write(dir.join("ready"), "ready").unwrap();
    loop {
        std::thread::park();
    }
}
