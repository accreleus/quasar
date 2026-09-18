//! A probe that runs as a bounded child process, so a driver crash ends the child and
//! never the agent. The child runs in its own process group; whatever ends it, the
//! whole group is killed and reaped before this returns.

use std::os::unix::process::ExitStatusExt;
use std::path::PathBuf;
use std::time::Duration;

use tokio::io::AsyncReadExt;
use tokio::sync::watch;

use super::outcome::ChildEnd;

#[derive(Debug, Clone)]
pub struct ChildSpec {
    pub program: PathBuf,
    pub args: Vec<String>,
    pub env: Vec<(String, String)>,
    pub deadline: Duration,
}

/// Longest result line kept; the rest of an over-long line is dropped. The child's
/// contract is one short line, so this only bounds a misbehaving one.
const MAX_LINE: usize = 4096;

/// How long a kill waits for the group to be reaped before giving up on the wait (the
/// SIGKILL itself cannot be refused, so this only bounds our own await).
const REAP_BOUND: Duration = Duration::from_secs(5);

/// Bound on draining the remaining stdout after the child exited: a grandchild may still
/// hold the write end, and the result line is already in hand by then.
const DRAIN_BOUND: Duration = Duration::from_secs(2);

/// SIGKILLs the child's whole process group on every exit path, including a drop of the
/// `run_child` future (tokio's `kill_on_drop` kills only the group leader).
struct KillGroupOnDrop {
    pgid: i32,
    armed: bool,
}

impl KillGroupOnDrop {
    fn kill(&self) {
        // Negative pid = the group. Safe on a reaped-but-not-yet-recycled pgid.
        unsafe { libc::kill(-self.pgid, libc::SIGKILL) };
    }

    /// The group is dead and the leader reaped; a later `kill` could only reach a
    /// recycled pgid.
    fn disarm(&mut self) {
        self.armed = false;
    }
}

impl Drop for KillGroupOnDrop {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        self.kill();
        // Non-blocking reap of the leader; anything left is tokio's `kill_on_drop`
        // orphan queue or init's job. Never blocks the dropping task.
        let mut status = 0;
        unsafe { libc::waitpid(self.pgid, &mut status, libc::WNOHANG) };
    }
}

/// Resolves once `preempt` is true. A dropped sender means nobody can pre-empt any more,
/// which is "never", not "now".
async fn preempted(preempt: &mut watch::Receiver<bool>) {
    if preempt.wait_for(|v| *v).await.is_err() {
        std::future::pending::<()>().await;
    }
}

/// Drain `stdout` to EOF, keeping only the last non-empty line. Reading only after exit
/// deadlocks a child that fills the 8 KiB pipe buffer (#194), so this runs concurrently
/// with the wait.
async fn last_nonempty_line(mut stdout: tokio::process::ChildStdout) -> String {
    fn keep(line: &[u8], last: &mut String) {
        let text = String::from_utf8_lossy(line);
        let text = text.trim();
        if !text.is_empty() {
            *last = text.to_string();
        }
    }

    let mut buf = [0u8; 8192];
    let mut line: Vec<u8> = Vec::new();
    let mut last = String::new();
    loop {
        let n = match stdout.read(&mut buf).await {
            Ok(0) | Err(_) => break,
            Ok(n) => n,
        };
        for &byte in &buf[..n] {
            if byte == b'\n' {
                keep(&line, &mut last);
                line.clear();
            } else if line.len() < MAX_LINE {
                line.push(byte);
            }
        }
    }
    // A child that exits without a trailing newline still said something.
    keep(&line, &mut last);
    last
}

/// `preempt` turning true stops the child at once. Dropping the returned future also
/// kills the group.
pub async fn run_child(spec: ChildSpec, mut preempt: watch::Receiver<bool>) -> ChildEnd {
    if *preempt.borrow_and_update() {
        return ChildEnd::Preempted;
    }

    let mut cmd = tokio::process::Command::new(&spec.program);
    cmd.args(&spec.args)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::piped())
        // Inherited so the child's own tracing lands in the agent log.
        .stderr(std::process::Stdio::inherit())
        .kill_on_drop(true)
        // Its own process group, so a deadline kills whatever the child started too.
        .process_group(0);
    for (k, v) in &spec.env {
        cmd.env(k, v);
    }

    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            return ChildEnd::SpawnFailed(format!("{}: {e}", spec.program.display()));
        }
    };
    let Some(pid) = child.id() else {
        return ChildEnd::SpawnFailed("the child exited before it could be tracked".into());
    };
    let mut group = KillGroupOnDrop {
        pgid: pid as i32,
        armed: true,
    };
    let drain = child
        .stdout
        .take()
        .map(|out| tokio::spawn(last_nonempty_line(out)));

    enum Stop {
        Exited(std::io::Result<std::process::ExitStatus>),
        Deadline,
        Preempted,
    }
    let stop = tokio::select! {
        status = child.wait() => Stop::Exited(status),
        _ = tokio::time::sleep(spec.deadline) => Stop::Deadline,
        _ = preempted(&mut preempt) => Stop::Preempted,
    };

    let status = match stop {
        Stop::Exited(status) => status,
        Stop::Deadline | Stop::Preempted => {
            group.kill();
            // The leader must be reaped here, not left to the orphan queue.
            let _ = tokio::time::timeout(REAP_BOUND, child.wait()).await;
            group.disarm();
            return match stop {
                Stop::Deadline => ChildEnd::Deadline(spec.deadline),
                _ => ChildEnd::Preempted,
            };
        }
    };
    // Reaped, so the pgid may be recycled: no group kill from here on.
    group.disarm();

    let stdout = match drain {
        Some(handle) => tokio::time::timeout(DRAIN_BOUND, handle)
            .await
            .ok()
            .and_then(|r| r.ok())
            .unwrap_or_default(),
        None => String::new(),
    };

    match status {
        Ok(status) => match (status.code(), status.signal()) {
            (Some(code), _) => ChildEnd::Exited { code, stdout },
            (None, Some(signal)) => ChildEnd::Signaled(signal),
            (None, None) => ChildEnd::SpawnFailed("the child ended with no status".into()),
        },
        Err(e) => ChildEnd::SpawnFailed(format!("could not wait for the child: {e}")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Instant;

    const TEST_BOUND: Duration = Duration::from_secs(20);

    fn sh(script: &str, deadline: Duration) -> ChildSpec {
        ChildSpec {
            program: "/bin/sh".into(),
            args: vec!["-c".into(), script.into()],
            env: Vec::new(),
            deadline,
        }
    }

    fn never() -> (watch::Sender<bool>, watch::Receiver<bool>) {
        watch::channel(false)
    }

    async fn bounded(spec: ChildSpec, preempt: watch::Receiver<bool>) -> ChildEnd {
        tokio::time::timeout(TEST_BOUND, run_child(spec, preempt))
            .await
            .expect("run_child must return within its own deadline")
    }

    fn pid_alive(pid: i32) -> bool {
        // A zombie still answers kill(0); /proc says whether it is really gone.
        match std::fs::read_to_string(format!("/proc/{pid}/stat")) {
            Ok(stat) => !stat.contains(") Z "),
            Err(_) => false,
        }
    }

    async fn wait_written(path: &std::path::Path) {
        let until = Instant::now() + Duration::from_secs(5);
        while std::fs::read_to_string(path).map_or(true, |s| !s.ends_with('\n')) {
            assert!(Instant::now() < until, "the child never wrote {path:?}");
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }

    async fn wait_gone(pid: i32) {
        let until = Instant::now() + Duration::from_secs(5);
        while pid_alive(pid) {
            assert!(Instant::now() < until, "process {pid} outlived the probe");
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    }

    #[tokio::test]
    async fn a_clean_exit_carries_the_code_and_the_last_stdout_line() {
        let (_tx, rx) = never();
        let end = bounded(
            sh(
                "echo working; echo 'encoded 30 frames'",
                Duration::from_secs(10),
            ),
            rx,
        )
        .await;
        assert_eq!(
            end,
            ChildEnd::Exited {
                code: 0,
                stdout: "encoded 30 frames".into()
            }
        );
    }

    #[tokio::test]
    async fn a_failing_exit_carries_its_code_and_reason() {
        let (_tx, rx) = never();
        let end = bounded(sh("echo 'no encoder'; exit 3", Duration::from_secs(10)), rx).await;
        assert_eq!(
            end,
            ChildEnd::Exited {
                code: 3,
                stdout: "no encoder".into()
            }
        );
    }

    #[tokio::test]
    async fn the_environment_reaches_the_child() {
        let (_tx, rx) = never();
        let mut spec = sh("echo \"$QUASAR_RENDER_NODE\"", Duration::from_secs(10));
        spec.env = vec![("QUASAR_RENDER_NODE".into(), "/dev/dri/renderD129".into())];
        assert_eq!(
            bounded(spec, rx).await,
            ChildEnd::Exited {
                code: 0,
                stdout: "/dev/dri/renderD129".into()
            }
        );
    }

    #[tokio::test]
    async fn a_crash_reports_the_signal_and_the_caller_survives() {
        let (_tx, rx) = never();
        let end = bounded(sh("kill -SEGV $$", Duration::from_secs(10)), rx).await;
        assert_eq!(end, ChildEnd::Signaled(libc::SIGSEGV));
    }

    #[tokio::test]
    async fn a_child_that_floods_stdout_cannot_block_or_grow_without_bound() {
        let (_tx, rx) = never();
        let end = bounded(
            sh(
                "i=0; while [ $i -lt 20000 ]; do echo 0123456789012345678901234567890123456789; i=$((i+1)); done; echo done",
                Duration::from_secs(15),
            ),
            rx,
        )
        .await;
        assert_eq!(
            end,
            ChildEnd::Exited {
                code: 0,
                stdout: "done".into()
            }
        );
    }

    #[tokio::test]
    async fn the_deadline_kills_the_child_and_everything_it_started() {
        let dir = tempfile::tempdir().unwrap();
        let pids = dir.path().join("pids");
        let script = format!(
            "sleep 300 & echo $! > {p}; echo $$ >> {p}; wait",
            p = pids.display()
        );
        let (_tx, rx) = never();
        let started = Instant::now();
        let end = bounded(sh(&script, Duration::from_millis(400)), rx).await;
        assert_eq!(end, ChildEnd::Deadline(Duration::from_millis(400)));
        assert!(started.elapsed() < Duration::from_secs(5));
        for pid in std::fs::read_to_string(&pids).unwrap().lines() {
            wait_gone(pid.trim().parse().unwrap()).await;
        }
    }

    #[tokio::test]
    async fn a_child_that_ignores_sigterm_is_still_killed_at_the_deadline() {
        let (_tx, rx) = never();
        let started = Instant::now();
        let end = bounded(
            sh("trap '' TERM; sleep 300", Duration::from_millis(300)),
            rx,
        )
        .await;
        assert_eq!(end, ChildEnd::Deadline(Duration::from_millis(300)));
        assert!(started.elapsed() < Duration::from_secs(5));
    }

    #[tokio::test]
    async fn preemption_stops_the_child_promptly() {
        let dir = tempfile::tempdir().unwrap();
        let pidfile = dir.path().join("pid");
        let script = format!("echo $$ > {}; sleep 300", pidfile.display());
        let (tx, rx) = never();
        let run = tokio::spawn(bounded(sh(&script, Duration::from_secs(60)), rx));
        wait_written(&pidfile).await;
        let asked = Instant::now();
        tx.send(true).unwrap();
        let end = tokio::time::timeout(TEST_BOUND, run)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(end, ChildEnd::Preempted);
        assert!(
            asked.elapsed() < Duration::from_secs(2),
            "{:?}",
            asked.elapsed()
        );
        let pid: i32 = std::fs::read_to_string(&pidfile)
            .unwrap()
            .trim()
            .parse()
            .unwrap();
        wait_gone(pid).await;
    }

    #[tokio::test]
    async fn preemption_raised_before_the_start_never_spawns() {
        let dir = tempfile::tempdir().unwrap();
        let marker = dir.path().join("ran");
        let (tx, rx) = never();
        tx.send(true).unwrap();
        let end = bounded(
            sh(
                &format!("touch {}", marker.display()),
                Duration::from_secs(10),
            ),
            rx,
        )
        .await;
        assert_eq!(end, ChildEnd::Preempted);
        assert!(!marker.exists());
    }

    #[tokio::test]
    async fn a_missing_program_is_a_spawn_failure_not_a_verdict() {
        let (_tx, rx) = never();
        let spec = ChildSpec {
            program: "/nonexistent/quasar-probe".into(),
            args: Vec::new(),
            env: Vec::new(),
            deadline: Duration::from_secs(1),
        };
        assert!(matches!(bounded(spec, rx).await, ChildEnd::SpawnFailed(_)));
    }

    #[tokio::test]
    async fn dropping_the_run_kills_the_child() {
        let dir = tempfile::tempdir().unwrap();
        let pidfile = dir.path().join("pid");
        let script = format!("echo $$ > {}; sleep 300", pidfile.display());
        let (_tx, rx) = never();
        let run = tokio::spawn(run_child(sh(&script, Duration::from_secs(60)), rx));
        wait_written(&pidfile).await;
        run.abort();
        let pid: i32 = std::fs::read_to_string(&pidfile)
            .unwrap()
            .trim()
            .parse()
            .unwrap();
        wait_gone(pid).await;
    }
}
