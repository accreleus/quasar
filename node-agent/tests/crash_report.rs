//! A fatal signal leaves a report on stderr and the process still dies as it would
//! have, promptly (#267).
//!
//! The dying half runs in a child: the test binary itself, asked for the one ignored
//! test below. A child says `ready` once it is set up, and from then has [`LIMIT`] to be
//! dead: a reporter that stalls, spins or waits on a lock fails its test instead of
//! hanging the suite.

use std::io::{BufRead, BufReader, Read, Write};
use std::os::unix::process::ExitStatusExt;
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering};
use std::sync::mpsc::{channel, Receiver};
use std::time::{Duration, Instant};

const CHILD_ENV: &str = "QUASAR_CRASH_TEST_CHILD";
const FIFO_ENV: &str = "QUASAR_CRASH_TEST_FIFO";
/// 15 bytes at most: the kernel keeps no more of a thread's name.
const THREAD: &str = "quasar-crash";
/// From `ready` to dead.
const LIMIT: Duration = Duration::from_secs(3);
/// From spawn to `ready`: the exec and libtest's start, which a loaded machine stretches
/// and which say nothing about the reporter.
const START_LIMIT: Duration = Duration::from_secs(60);

static GO: AtomicBool = AtomicBool::new(false);
static LOADER_TID: AtomicI32 = AtomicI32::new(0);

fn read_nowhere() {
    let nowhere = std::hint::black_box(0x10usize) as *const u8;
    // SAFETY: none. The read faults, which is the point.
    unsafe { std::ptr::read_volatile(nowhere) };
}

#[allow(unconditional_recursion)]
fn overflow(depth: u64) -> u64 {
    let pad = std::hint::black_box([depth; 64]);
    overflow(depth + 1) + pad[0]
}

/// A real SIGBUS: a page of a zero-length file.
fn bus() {
    // SAFETY: plain syscalls; the read past the end of the file is the point.
    unsafe {
        let fd = libc::memfd_create(c"quasar-crash-test".as_ptr(), 0);
        let page = libc::mmap(
            std::ptr::null_mut(),
            4096,
            libc::PROT_READ,
            libc::MAP_SHARED,
            fd,
            0,
        );
        std::ptr::read_volatile(page.cast::<u8>());
    }
}

/// A real breakpoint trap, which is how GLib's `g_error` dies. Execution resumes after
/// it when a handler returns.
fn breakpoint() {
    // SAFETY: one trap instruction.
    unsafe {
        #[cfg(target_arch = "x86_64")]
        std::arch::asm!("int3");
        #[cfg(target_arch = "aarch64")]
        std::arch::asm!("brk #0");
    }
}

/// Whether thread `tid` of this process is asleep in the kernel.
fn asleep(tid: i32) -> bool {
    std::fs::read_to_string(format!("/proc/self/task/{tid}/stat"))
        .ok()
        .and_then(|stat| {
            stat.rsplit_once(") ")
                .map(|(_, rest)| rest.starts_with('S'))
        })
        .unwrap_or(false)
}

/// What a child sets up before it says `ready`.
fn prepare(how: &str) {
    match how {
        // Another thread inside `dlopen`, holding the loader's lock for good: the loader
        // opens a FIFO nobody writes to.
        "locked" => {
            let fifo = std::ffi::CString::new(std::env::var(FIFO_ENV).unwrap()).unwrap();
            std::thread::spawn(move || {
                // SAFETY: `gettid` is a plain syscall; `fifo` outlives the call.
                unsafe {
                    LOADER_TID.store(libc::gettid(), Ordering::SeqCst);
                    libc::dlopen(fifo.as_ptr(), libc::RTLD_NOW);
                }
            });
            let started = Instant::now();
            while !asleep(LOADER_TID.load(Ordering::SeqCst)) {
                assert!(started.elapsed() < START_LIMIT, "the loader never blocked");
                std::thread::yield_now();
            }
        }
        // A second thread that faults the moment this one does.
        "pair" => {
            std::thread::Builder::new()
                .name("quasar-crash-2".into())
                .spawn(|| {
                    while !GO.load(Ordering::Acquire) {
                        std::hint::spin_loop();
                    }
                    read_nowhere();
                })
                .unwrap();
            // Let it reach its spin before `ready`.
            std::thread::sleep(Duration::from_millis(20));
        }
        _ => {}
    }
}

fn die(how: &str) {
    match how {
        "abort" => std::process::abort(),
        // SAFETY: raises a signal at this process.
        "raised-trap" => unsafe {
            libc::raise(libc::SIGTRAP);
        },
        "trap" => breakpoint(),
        "overflow" => {
            overflow(1);
        }
        "bus" => bus(),
        "wild" => {
            // SAFETY: none. A call to an unmapped address is the point.
            let nowhere: extern "C" fn() =
                unsafe { std::mem::transmute(std::hint::black_box(0x10usize)) };
            nowhere();
        }
        "pair" => {
            GO.store(true, Ordering::Release);
            read_nowhere();
        }
        _ => read_nowhere(),
    }
}

/// Only ever does anything as the child of the tests below; `--include-ignored` alone
/// leaves it a no-op. It sets up, says `ready`, and dies `<how>`; `wait:<how>` waits for
/// a byte on stdin in between, so the parent can send it a signal first.
#[test]
#[ignore = "the child half of the tests in this file"]
fn child_dies_of_a_signal_on_a_named_thread() {
    let Ok(how) = std::env::var(CHILD_ENV) else {
        return;
    };
    let no_core = libc::rlimit {
        rlim_cur: 0,
        rlim_max: 0,
    };
    // SAFETY: a valid `rlimit`. No core file, whatever the host's pattern says.
    unsafe { libc::setrlimit(libc::RLIMIT_CORE, &no_core) };
    quasar_node_agent::crash_report::install("crash-test");
    let thread = std::thread::Builder::new().name(THREAD.into());
    let _ = thread
        .spawn(move || {
            let (wait, how) = match how.strip_prefix("wait:") {
                Some(how) => (true, how),
                None => (false, how.as_str()),
            };
            prepare(how);
            // Raw, because libtest captures `println!`. `read` returns early when the
            // parent's signal interrupts it.
            let mut byte = [0u8; 1];
            // SAFETY: both buffers are live for the calls.
            unsafe {
                libc::write(1, b"ready\n".as_ptr().cast(), 6);
                while wait && libc::read(0, byte.as_mut_ptr().cast(), 1) < 0 {}
            }
            die(how);
        })
        .unwrap()
        .join();
}

struct Ended {
    signal: Option<i32>,
    code: Option<i32>,
    stderr: String,
}

impl Ended {
    fn lines(&self, kind: &str) -> usize {
        let marker = format!("]: {kind}");
        self.stderr.lines().filter(|l| l.contains(&marker)).count()
    }

    /// The report ran to its last line, and that line does not say it was cut short.
    fn complete(&self) -> bool {
        self.lines("end of report") == 1
            && self.stderr.lines().any(|l| l.ends_with("]: end of report"))
    }
}

struct Running {
    child: Child,
    how: String,
    stderr: Receiver<String>,
    /// When the child said `ready`.
    ready: Instant,
}

/// Start a child, under `wrapper` if one is given, and wait for it to say `ready`.
fn spawn(wrapper: &[&str], how: &str, fifo: Option<&std::path::Path>) -> Running {
    let exe = std::env::current_exe().unwrap();
    let mut command = match wrapper.split_first() {
        Some((program, args)) => {
            let mut command = Command::new(program);
            command.args(args).arg(&exe);
            command
        }
        None => Command::new(&exe),
    };
    command
        .args([
            "--ignored",
            "--exact",
            "child_dies_of_a_signal_on_a_named_thread",
        ])
        .env(CHILD_ENV, how)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    if let Some(fifo) = fifo {
        command.env(FIFO_ENV, fifo);
    }
    let mut child = command.spawn().unwrap();

    let mut stderr = child.stderr.take().unwrap();
    let (stderr_tx, stderr_rx) = channel();
    std::thread::spawn(move || {
        let mut text = String::new();
        let _ = stderr.read_to_string(&mut text);
        let _ = stderr_tx.send(text);
    });
    let stdout = BufReader::new(child.stdout.take().unwrap());
    let (ready_tx, ready_rx) = channel();
    std::thread::spawn(move || {
        for line in stdout.lines().map_while(Result::ok) {
            if line.trim() == "ready" {
                let _ = ready_tx.send(());
                return;
            }
        }
    });
    if ready_rx.recv_timeout(START_LIMIT).is_err() {
        let _ = child.kill();
        let _ = child.wait();
        let seen = stderr_rx.recv_timeout(LIMIT).unwrap_or_default();
        panic!("`{how}` never said ready; its stderr:\n{seen}");
    }
    Running {
        child,
        how: how.to_string(),
        stderr: stderr_rx,
        ready: Instant::now(),
    }
}

/// Wait for the child, but not past [`LIMIT`] from when it was ready.
fn finish(running: Running) -> Ended {
    let Running {
        mut child,
        how,
        stderr,
        ready,
    } = running;
    let status = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break status;
        }
        if ready.elapsed() > LIMIT {
            let _ = child.kill();
            let _ = child.wait();
            let seen = stderr.recv_timeout(LIMIT).unwrap_or_default();
            panic!("`{how}` did not die within {LIMIT:?} of being ready; its stderr:\n{seen}");
        }
        std::thread::sleep(Duration::from_millis(1));
    };
    let stderr = stderr.recv_timeout(LIMIT).unwrap();
    eprintln!("`{how}` died {:?} after ready", ready.elapsed());
    Ended {
        signal: status.signal(),
        code: status.code(),
        stderr,
    }
}

fn child(how: &str) -> Ended {
    finish(spawn(&[], how, None))
}

/// The child takes `kills` SIGSEGVs from this process, then is told to die `how`.
fn child_sent_segvs(kills: usize, how: &str) -> Ended {
    let mut running = spawn(&[], &format!("wait:{how}"), None);
    for _ in 0..kills {
        // SAFETY: signals the child this test spawned.
        unsafe { libc::kill(running.child.id() as libc::pid_t, libc::SIGSEGV) };
        // Delivery is immediate; the pause is for the handler to finish before the
        // next signal or the byte. The child reads again when a signal interrupts it.
        std::thread::sleep(Duration::from_millis(100));
    }
    // A child the signals killed has no stdin left to write to.
    let _ = running.child.stdin.take().unwrap().write_all(b"x");
    finish(running)
}

fn banner(signal: &str) -> String {
    format!("]: fatal signal {signal} code ")
}

#[test]
fn a_fault_reports_its_thread_stack_maps_and_frames_and_dies_of_the_signal() {
    let end = child("segv");
    let stderr = &end.stderr;
    assert_eq!(end.signal, Some(libc::SIGSEGV), "{stderr}");
    assert!(stderr.contains("quasar-node-agent crash-test["), "{stderr}");
    assert!(stderr.contains(&banner("11 (SIGSEGV)")), "{stderr}");
    assert!(
        stderr.contains(&format!(" in thread {THREAD} tid ")),
        "{stderr}"
    );
    assert!(stderr.contains(": fault address 0x10 ip 0x"), "{stderr}");
    assert_eq!(
        end.lines("stack 0x"),
        12,
        "48 words, four to a line: {stderr}"
    );
    assert!(end.lines("map ") >= 2, "{stderr}");
    assert!(end.lines("frame ") >= 3, "{stderr}");
    assert!(end.complete(), "{stderr}");
}

/// A fault while another thread is inside `dlopen`, holding the loader's lock, as during
/// a plugin load. A reporter that asked the loader for symbol names waited on that lock
/// and then died of its own alarm instead of the fault.
#[test]
fn a_fault_while_another_thread_is_in_dlopen_is_reported_at_once() {
    let dir = tempfile::tempdir().unwrap();
    let fifo = dir.path().join("never-written.so");
    let path = std::ffi::CString::new(fifo.to_str().unwrap()).unwrap();
    // SAFETY: `path` is a valid C string.
    assert_eq!(unsafe { libc::mkfifo(path.as_ptr(), 0o600) }, 0);

    let end = finish(spawn(&[], "locked", Some(&fifo)));
    let stderr = &end.stderr;
    assert_eq!(end.signal, Some(libc::SIGSEGV), "{stderr}");
    assert!(stderr.contains(&banner("11 (SIGSEGV)")), "{stderr}");
    assert!(end.lines("frame ") >= 3, "{stderr}");
    assert!(end.complete(), "{stderr}");
}

/// Two threads that fault together: the one that loses the race to report must let the
/// winner finish. Dying at once, as it did, cut the report off or lost it whole.
#[test]
fn two_threads_faulting_together_leave_one_complete_report() {
    let end = child("pair");
    let stderr = &end.stderr;
    assert_eq!(end.signal, Some(libc::SIGSEGV), "{stderr}");
    assert_eq!(end.lines("fatal signal"), 1, "{stderr}");
    assert!(end.lines("frame ") >= 3, "{stderr}");
    assert!(end.complete(), "{stderr}");
}

/// The shape the unwinder cannot survive: libgcc reads the code at an ip it has no
/// unwind data for. Everything needed to place the caller is out before that matters.
#[test]
fn a_wild_call_is_reported_without_the_unwinder() {
    let end = child("wild");
    let stderr = &end.stderr;
    assert_eq!(end.signal, Some(libc::SIGSEGV), "{stderr}");
    assert!(stderr.contains(&banner("11 (SIGSEGV)")), "{stderr}");
    assert!(
        stderr.contains(": fault address 0x10 ip 0x10 sp 0x"),
        "{stderr}"
    );
    assert_eq!(end.lines("stack 0x"), 12, "{stderr}");
    let exe = std::env::current_exe().unwrap();
    let exe = exe.file_name().unwrap().to_str().unwrap();
    assert!(
        stderr
            .lines()
            .any(|l| l.contains("]: map ") && l.contains(" r-xp ") && l.contains(exe)),
        "the caller's mapping is in the report: {stderr}"
    );
    assert_eq!(end.lines("frame: none"), 1, "{stderr}");
    assert!(end.complete(), "{stderr}");
}

#[test]
fn an_abort_and_a_raised_trap_are_reported_and_die_as_themselves() {
    let end = child("abort");
    assert_eq!(end.signal, Some(libc::SIGABRT), "{}", end.stderr);
    assert!(
        end.stderr.contains(&banner("6 (SIGABRT)")),
        "{}",
        end.stderr
    );
    assert!(
        end.stderr.contains(": raised by this process, ip 0x"),
        "{}",
        end.stderr
    );
    assert!(end.complete(), "{}", end.stderr);

    let end = child("raised-trap");
    assert_eq!(end.signal, Some(libc::SIGTRAP), "{}", end.stderr);
    assert!(
        end.stderr.contains(&banner("5 (SIGTRAP)")),
        "{}",
        end.stderr
    );
}

/// A breakpoint instruction, not `raise`: execution would resume after it, so the
/// reporter has to end the process itself.
#[test]
fn a_real_trap_is_reported_and_dies_of_the_trap() {
    let end = child("trap");
    let stderr = &end.stderr;
    assert_eq!(end.signal, Some(libc::SIGTRAP), "{stderr}");
    assert!(stderr.contains(&banner("5 (SIGTRAP)")), "{stderr}");
    assert!(stderr.contains(": fault address 0x"), "{stderr}");
    assert!(end.complete(), "{stderr}");
}

/// As the init of a PID namespace (a pod, a bare `docker run`) the kernel drops a raised
/// default-fatal signal, so after a trap the process ran on past the breakpoint. It must
/// exit instead. Needs a user and PID namespace, which a container's seccomp profile
/// usually refuses: skipped there.
#[test]
fn as_a_namespace_init_a_trap_still_ends_the_process() {
    let unshare = [
        "unshare",
        "--user",
        "--map-root-user",
        "--pid",
        "--fork",
        "--kill-child",
    ];
    let allowed = Command::new(unshare[0])
        .args(&unshare[1..])
        .arg("true")
        .output()
        .is_ok_and(|probe| probe.status.success());
    if !allowed {
        eprintln!("SKIP: no user and PID namespace here, so no process can be made PID 1");
        return;
    }
    let end = finish(spawn(&unshare, "trap", None));
    let stderr = &end.stderr;
    assert!(
        stderr.contains("quasar-node-agent crash-test[1]: fatal signal 5 (SIGTRAP)"),
        "the child is PID 1 and reports the trap: {stderr}"
    );
    assert!(end.complete(), "{stderr}");
    assert_eq!(
        end.code,
        Some(128 + libc::SIGTRAP),
        "it exits as a shell reports a death by SIGTRAP: {stderr}"
    );
}

/// The handler runs on Rust's 8 KiB alternate stack here, unwinder included, and must
/// leave Rust's own stack-overflow report and its abort as they were.
#[test]
fn a_stack_overflow_keeps_rusts_own_report_and_abort() {
    let end = child("overflow");
    let stderr = &end.stderr;
    assert_eq!(end.signal, Some(libc::SIGABRT), "{stderr}");
    assert!(stderr.contains(&banner("11 (SIGSEGV)")), "{stderr}");
    assert!(end.complete(), "{stderr}");
    assert!(stderr.contains("has overflowed its stack"), "{stderr}");
    assert_eq!(
        end.lines("fatal signal"),
        1,
        "the abort is not reported twice: {stderr}"
    );
}

/// A SIGSEGV sent with `kill` is survived once and kills the second time, as before the
/// reporter (Rust's handler swallowed the first). After a survived one the process must
/// still die of a real fault, reported, and never spin: the reporter once stayed armed
/// with its "reporting" flag set and returned into the fault forever.
#[test]
fn a_real_fault_after_a_survived_kill_is_still_reported_and_kills() {
    for (how, signal, number) in [
        ("bus", libc::SIGBUS, "7 (SIGBUS)"),
        ("segv", libc::SIGSEGV, "11 (SIGSEGV)"),
    ] {
        let end = child_sent_segvs(1, how);
        let stderr = &end.stderr;
        assert_eq!(end.signal, Some(signal), "{stderr}");
        assert_eq!(
            end.lines("signal 11 (SIGSEGV) sent by pid "),
            1,
            "the sent signal is noted, not reported: {stderr}"
        );
        assert!(stderr.contains(": not a fault, ignored once"), "{stderr}");
        assert!(stderr.contains(&banner(number)), "{stderr}");
        assert!(end.complete(), "{stderr}");
    }
}

#[test]
fn a_second_sent_segv_kills_without_a_report() {
    let end = child_sent_segvs(2, "segv");
    let stderr = &end.stderr;
    assert_eq!(end.signal, Some(libc::SIGSEGV), "{stderr}");
    assert_eq!(end.lines("signal 11 (SIGSEGV) sent by pid "), 2, "{stderr}");
    assert!(stderr.contains(": not a fault, dying of it"), "{stderr}");
    assert_eq!(end.lines("fatal signal"), 0, "{stderr}");
}
