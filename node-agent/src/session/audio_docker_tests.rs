//! Opt-in Docker acceptance at the real PulseSidecar caller boundary.
use crate::session::{audio::PulseSidecar, container::ContainerRuntime};
use std::{path::PathBuf, process::Command, time::Instant};

struct Fixture {
    socket: String,
    alias: PathBuf,
    name: String,
    id: Option<String>,
    scratch: Option<tempfile::TempDir>,
}
impl Fixture {
    fn docker(&self, args: &[&str]) -> std::process::Output {
        Command::new("/usr/bin/timeout")
            .args(["20", "docker", "--host", &format!("unix://{}", self.socket)])
            .args(args)
            .output()
            .expect("bounded fixture inspection")
    }
    fn inspect(&self) -> Option<serde_json::Value> {
        let out = self.docker(&["inspect", &self.name]);
        if !out.status.success() {
            return None;
        }
        serde_json::from_slice::<Vec<serde_json::Value>>(&out.stdout)
            .ok()?
            .into_iter()
            .next()
    }
    fn restore(&self) {
        if !self.alias.exists() {
            std::os::unix::fs::symlink(&self.socket, &self.alias).unwrap();
        }
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        self.restore();
        if let Some(info) = self.inspect() {
            // Only the immutable ID captured after this test's successful launch
            // grants emergency cleanup authority. Never remove a name collision.
            if self.id.as_deref() == info["Id"].as_str() {
                if let Some(id) = &self.id {
                    if self.docker(&["rm", "--force", id]).status.success() {
                        return;
                    }
                }
            }
            if let Some(scratch) = self.scratch.take() {
                let _ = scratch.keep();
            }
            eprintln!("audio fixture cleanup unverified; backing directory retained");
        }
    }
}

#[test]
#[ignore = "requires explicit local Docker socket, existing Pulse image and same-path host bind"]
fn pulse_caller_preserves_socket_until_daemon_cleanup_is_known() {
    let socket = std::env::var("QUASAR_TEST_RUNTIME_SOCKET").expect("test socket");
    let root =
        PathBuf::from(std::env::var("QUASAR_TEST_AUDIO_ROOT").expect("same-path fixture root"));
    let image = std::env::var("QUASAR_TEST_RUNTIME_IMAGE").expect("existing Pulse image");
    let scratch = tempfile::Builder::new()
        .prefix("audio-")
        .tempdir_in(root)
        .unwrap();
    let alias = scratch.path().join("engine.sock");
    std::os::unix::fs::symlink(&socket, &alias).unwrap();
    std::env::set_var("DOCKER_HOST", format!("unix://{}", alias.display()));
    std::env::set_var("NODE_SECRET_PATH", scratch.path().join("node-secret"));
    std::env::set_var("QUASAR_PULSE_IMAGE", image);
    crate::runtime::initialize_image_state(scratch.path().join("runtime-state"));
    let nonce = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos();
    let sid = format!("00000234-0000-4000-8000-{:012x}", nonce & 0xffffffffffff);
    let mut fixture = Fixture {
        socket,
        alias,
        name: format!("quasar-pulse-{sid}"),
        id: None,
        scratch: Some(scratch),
    };
    let root = fixture.scratch.as_ref().unwrap().path().to_str().unwrap();
    let mut pulse = PulseSidecar::start(&sid, &ContainerRuntime::from_env(), root)
        .expect("audio launch")
        .expect("real Pulse socket readiness");
    let info = fixture.inspect().expect("launched audio container");
    fixture.id = info["Id"].as_str().map(str::to_owned);
    let dir = pulse.socket_dir().to_owned();
    assert!(dir.join("native").exists());
    assert_eq!(info["HostConfig"]["NetworkMode"], "none");
    assert_eq!(info["HostConfig"]["PidsLimit"], 512);
    assert_eq!(info["HostConfig"]["ReadonlyRootfs"], false);
    assert_eq!(info["HostConfig"]["CapDrop"], serde_json::json!(["ALL"]));
    assert_eq!(
        info["Config"]["Healthcheck"]["Test"],
        serde_json::json!(["NONE"])
    );
    let id = fixture.id.as_deref().unwrap();
    let server = pulse.server_uri();
    let topology = fixture.docker(&[
        "exec",
        "-u",
        "99:100",
        "-e",
        "HOME=/tmp",
        id,
        "pactl",
        "--server",
        &server,
        "info",
    ]);
    assert!(topology.status.success(), "non-root anonymous Pulse access");
    let topology = String::from_utf8_lossy(&topology.stdout);
    assert!(topology.contains("quasar_output"));
    assert!(topology.contains("quasar_mic_src"));
    let recorder = Command::new("/usr/bin/timeout")
        .args([
            "10",
            "docker",
            "--host",
            &format!("unix://{}", fixture.socket),
            "exec",
            "-u",
            "99:100",
            "-e",
            "HOME=/tmp",
            id,
            "timeout",
            "4",
            "parec",
            "--server",
            &server,
            "--device=quasar_output.monitor",
            "--format=s16le",
            "--rate=48000",
            "--channels=2",
            "--raw",
        ])
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .expect("bounded monitor capture");
    let capture = std::thread::spawn(move || recorder.wait_with_output().unwrap());
    std::thread::sleep(std::time::Duration::from_millis(200));
    let tone = fixture.docker(&[
        "exec",
        id,
        "gst-launch-1.0",
        "-q",
        "audiotestsrc",
        "num-buffers=100",
        "wave=sine",
        "!",
        "audioconvert",
        "!",
        "pulsesink",
        &format!("server={server}"),
        "device=quasar_output",
    ]);
    assert!(tone.status.success(), "known tone reached the Pulse sink");
    let captured = capture.join().unwrap();
    assert!(
        captured.stdout.len() >= 48000 * 4,
        "at least one second of stereo PCM"
    );
    let peak = captured
        .stdout
        .chunks_exact(2)
        .map(|sample| i16::from_le_bytes([sample[0], sample[1]]).unsigned_abs())
        .max()
        .unwrap_or(0);
    assert!(peak > 1000, "monitor received nonzero tone, peak={peak}");
    // Removing this fixture's alias affects no other Docker client or daemon.
    std::fs::remove_file(&fixture.alias).unwrap();
    let began = Instant::now();
    pulse.stop();
    assert!(began.elapsed().as_secs() < 35, "teardown must be bounded");
    assert!(
        dir.exists(),
        "unknown cleanup must preserve the audio socket directory"
    );
    assert_eq!(fixture.inspect().unwrap()["State"]["Running"], true);
    fixture.restore();
    pulse.stop();
    assert!(
        fixture.inspect().is_none(),
        "retry removes the exact audio container"
    );
    assert!(
        !dir.exists(),
        "known cleanup removes the owned socket directory"
    );
}
