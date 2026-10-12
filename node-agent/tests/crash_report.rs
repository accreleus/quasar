//! A fatal signal leaves a report on stderr and the process still dies as it would
//! have, promptly (#267).
//!
//! The dying half runs in a child: the test binary itself, asked for the one ignored
//! test below. Every child is given [`LIMIT`] to exit; a reporter that stalls, spins or
//! waits on a lock fails its test instead of hanging the suite.

use std::io::{BufRead, BufReader, Read, Write};
use std::os::unix::process::ExitStatusExt;
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{channel, Receiver};
use std::time::{Duration, Instant};

const CHILD_ENV: &str = "QUASAR_CRASH_TEST_CHILD";
/// 15 bytes at most: the kernel keeps no more of a thread's name.
const THREAD: &str = "quasar-crash";
const LIMIT: Duration = Duration::from_secs(3);

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

static LOADER_LOCK_HELD: AtomicBool = AtomicBool::new(false);

/// Runs with the loader's lock held, as a `dlopen` on another thread would.
unsafe extern "C" fn hold_the_loader_lock(
    _info: *mut libc::dl_phdr_info,
    _size: libc::size_t,
    _data: *mut libc::c_void,
) -> libc::c_int {
    LOADER_LOCK_HELD.store(true, Ordering::SeqCst);
    std::thread::sleep(Duration::from_secs(60));
    0
}

fn die(how: &str) {
    match how {
        "locked" => {
            std::thread::spawn(|| {
                // SAFETY: the callback only sleeps.
                unsafe { libc::dl_iterate_phdr(Some(hold_the_loader_lock), std::ptr::null_mut()) }
            });
            while !LOADER_LOCK_HELD.load(Ordering::SeqCst) {
                std::thread::yield_now();
            }
            die("segv");
        }
        "abort" => std::process::abort(),
        // SAFETY: raises a signal at this process.
        "trap" => unsafe {
            libc::raise(libc::SIGTRAP);
        },
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
        _ => {
            let nowhere = std::hint::black_box(0x10usize) as *const u8;
            // SAFETY: none. The read faults, which is the point.
            unsafe { std::ptr::read_volatile(nowhere) };
        }
    }
}

/// Only ever does anything as the child of the tests below; `--include-ignored` alone
/// leaves it a no-op. `<how>` dies that way at once; `wait:<how>` first says `ready` and
/// waits for a byte on stdin, so the parent can send it a signal in between.
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
            let how = match how.strip_prefix("wait:") {
                None => how.as_str(),
                Some(how) => {
                    // Raw, because libtest captures `println!`. `read` returns early
                    // when the parent's signal interrupts it.
                    let mut byte = [0u8; 1];
                    // SAFETY: both buffers are live for the calls.
                    unsafe {
                        libc::write(1, b"ready\n".as_ptr().cast(), 6);
                        while libc::read(0, byte.as_mut_ptr().cast(), 1) < 0 {}
                    }
                    how
                }
            };
            die(how);
        })
        .unwrap()
        .join();
}

struct Ended {
    signal: Option<i32>,
    stderr: String,
}

impl Ended {
    fn lines(&self, kind: &str) -> usize {
        let marker = format!("]: {kind}");
        self.stderr.lines().filter(|l| l.contains(&marker)).count()
    }
}

fn spawn(how: &str) -> (Child, Receiver<String>) {
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args([
            "--ignored",
            "--exact",
            "child_dies_of_a_signal_on_a_named_thread",
        ])
        .env(CHILD_ENV, how)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let mut stderr = child.stderr.take().unwrap();
    let (tx, rx) = channel();
    std::thread::spawn(move || {
        let mut text = String::new();
        let _ = stderr.read_to_string(&mut text);
        let _ = tx.send(text);
    });
    (child, rx)
}

/// Wait for the child, but not past [`LIMIT`] from `started`.
fn finish(mut child: Child, stderr: Receiver<String>, how: &str, started: Instant) -> Ended {
    let status = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break status;
        }
        if started.elapsed() > LIMIT {
            let _ = child.kill();
            let _ = child.wait();
            let seen = stderr.recv_timeout(LIMIT).unwrap_or_default();
            panic!("`{how}` did not die within {LIMIT:?}; its stderr:\n{seen}");
        }
        std::thread::sleep(Duration::from_millis(2));
    };
    let stderr = stderr.recv_timeout(LIMIT).unwrap();
    eprintln!("`{how}` died in {:?}", started.elapsed());
    Ended {
        signal: status.signal(),
        stderr,
    }
}

fn child(how: &str) -> Ended {
    let started = Instant::now();
    let (child, stderr) = spawn(how);
    finish(child, stderr, how, started)
}

/// The child says `ready`, takes `kills` SIGSEGVs from this process, then is told to
/// die `how`.
fn child_sent_segvs(kills: usize, how: &str) -> Ended {
    let started = Instant::now();
    let how = format!("wait:{how}");
    let (mut child, stderr) = spawn(&how);
    let mut stdout = BufReader::new(child.stdout.take().unwrap());
    let mut line = String::new();
    while line.trim() != "ready" {
        line.clear();
        assert_ne!(
            stdout.read_line(&mut line).unwrap(),
            0,
            "the child never said ready"
        );
    }
    for _ in 0..kills {
        // SAFETY: signals the child this test spawned.
        unsafe { libc::kill(child.id() as libc::pid_t, libc::SIGSEGV) };
        // Delivery is immediate; the pause is for the handler to finish before the
        // next signal or the byte. The child reads again when a signal interrupts it.
        std::thread::sleep(Duration::from_millis(100));
    }
    // A child the signals killed has no stdin left to write to.
    let _ = child.stdin.take().unwrap().write_all(b"x");
    finish(child, stderr, &how, started)
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
    assert_eq!(end.lines("end of report"), 1, "{stderr}");
}

/// A fault while another thread holds the loader's lock, as during a plugin load. The
/// reporter once waited on that lock and died of its own alarm instead of the fault.
#[test]
fn a_fault_while_the_loader_lock_is_held_is_reported_at_once() {
    let end = child("locked");
    let stderr = &end.stderr;
    assert_eq!(end.signal, Some(libc::SIGSEGV), "{stderr}");
    assert!(stderr.contains(&banner("11 (SIGSEGV)")), "{stderr}");
    assert!(end.lines("frame ") >= 3, "{stderr}");
    assert_eq!(end.lines("end of report"), 1, "{stderr}");
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
    assert_eq!(end.lines("end of report"), 1, "{stderr}");
}

#[test]
fn an_abort_and_a_trap_are_reported_and_die_as_themselves() {
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
    assert_eq!(end.lines("end of report"), 1, "{}", end.stderr);

    let end = child("trap");
    assert_eq!(end.signal, Some(libc::SIGTRAP), "{}", end.stderr);
    assert!(
        end.stderr.contains(&banner("5 (SIGTRAP)")),
        "{}",
        end.stderr
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
    assert_eq!(end.lines("end of report"), 1, "{stderr}");
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
        assert_eq!(end.lines("end of report"), 1, "{stderr}");
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
