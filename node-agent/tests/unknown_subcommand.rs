//! A probe container runs the agent's own image with a subcommand. An image that does
//! not know the subcommand must refuse it; falling through to agent mode would boot a
//! second agent on the host.
use std::io::Read;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

fn run_bounded(args: &[&str]) -> (Option<i32>, String) {
    let mut child = Command::new(env!("CARGO_BIN_EXE_quasar-node-agent"))
        .args(args)
        // Agent mode would exit non-zero on a missing config too. A complete config
        // makes that path stay up, so only the refusal can end the process.
        .env("CONTROL_PLANE_URL", "ws://127.0.0.1:9/agent")
        .env("NODE_NAME", "unknown-subcommand-test")
        .env("ENROLLMENT_TOKEN", "test")
        .env("QUASAR_ALLOW_PLAINTEXT_AGENT", "1")
        .env("NODE_SECRET_PATH", "/nonexistent/unknown-subcommand-test")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .spawn()
        .expect("spawn agent binary");
    let deadline = Instant::now() + Duration::from_secs(10);
    let status = loop {
        if let Some(status) = child.try_wait().expect("try_wait") {
            break Some(status);
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let _ = child.wait();
            break None;
        }
        std::thread::sleep(Duration::from_millis(20));
    };
    let mut stderr = String::new();
    if let Some(mut pipe) = child.stderr.take() {
        let _ = pipe.read_to_string(&mut stderr);
    }
    (status.and_then(|s| s.code()), stderr)
}

#[test]
fn an_unknown_subcommand_is_refused_with_a_non_zero_exit() {
    for args in [
        &["media-probe-from-the-future"][..],
        &["--gpu", "0"][..],
        &[""][..],
    ] {
        let (code, stderr) = run_bounded(args);
        assert_eq!(code, Some(2), "{args:?}: stderr: {stderr}");
        assert!(stderr.contains("unknown subcommand"), "{args:?}: {stderr}");
        assert!(
            !stderr.contains("starting"),
            "{args:?}: the agent began to start: {stderr}"
        );
    }
}
