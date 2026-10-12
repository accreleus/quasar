//! A fatal signal leaves a report on stderr and still kills the process (#267).
//!
//! The faulting half runs in a child: the test binary itself, asked for the one ignored
//! test below.

use std::os::unix::process::ExitStatusExt;
use std::process::{Command, Stdio};

const CHILD_ENV: &str = "QUASAR_CRASH_TEST_CHILD";
/// 15 bytes at most: the kernel keeps no more of a thread's name.
const THREAD: &str = "quasar-crash";

/// Only ever does anything as the child of the tests below; `--include-ignored` alone
/// leaves it a no-op.
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
    quasar_node_agent::crash_report::install();
    let thread = std::thread::Builder::new().name(THREAD.into());
    let _ = thread
        .spawn(move || {
            if how == "abort" {
                std::process::abort();
            }
            if how == "overflow" {
                overflow(1);
            }
            let nowhere = std::hint::black_box(0x10usize) as *const u8;
            // SAFETY: none. The read faults, which is the point.
            unsafe { std::ptr::read_volatile(nowhere) };
        })
        .unwrap()
        .join();
}

#[allow(unconditional_recursion)]
fn overflow(depth: u64) -> u64 {
    let pad = std::hint::black_box([depth; 64]);
    overflow(depth + 1) + pad[0]
}

fn child(how: &str) -> (Option<i32>, String) {
    let out = Command::new(std::env::current_exe().unwrap())
        .args([
            "--ignored",
            "--exact",
            "child_dies_of_a_signal_on_a_named_thread",
        ])
        .env(CHILD_ENV, how)
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .output()
        .unwrap();
    (
        out.status.signal(),
        String::from_utf8_lossy(&out.stderr).into_owned(),
    )
}

#[test]
fn a_fault_names_its_thread_and_address_and_still_kills_the_process() {
    let (signal, stderr) = child("segv");
    assert_eq!(signal, Some(libc::SIGSEGV), "{stderr}");
    assert!(
        stderr.contains(&format!(
            "fatal signal 11 (SIGSEGV) in thread {THREAD} tid "
        )),
        "{stderr}"
    );
    assert!(stderr.contains(": fault address 0x10 ip 0x"), "{stderr}");
    let frames = stderr.lines().filter(|line| line.contains("[0x")).count();
    assert!(frames >= 3, "a backtrace follows the first line: {stderr}");
}

#[test]
fn an_abort_is_reported_and_still_an_abort() {
    let (signal, stderr) = child("abort");
    assert_eq!(signal, Some(libc::SIGABRT), "{stderr}");
    assert!(
        stderr.contains(&format!("fatal signal 6 (SIGABRT) in thread {THREAD} tid ")),
        "{stderr}"
    );
}

/// The handler runs on Rust's 8 KiB alternate stack here, unwinder included, and must
/// leave Rust's own stack-overflow report and its abort as they were.
#[test]
fn a_stack_overflow_keeps_rusts_own_report_and_abort() {
    let (signal, stderr) = child("overflow");
    assert_eq!(signal, Some(libc::SIGABRT), "{stderr}");
    assert!(
        stderr.contains(&format!(
            "fatal signal 11 (SIGSEGV) in thread {THREAD} tid "
        )),
        "{stderr}"
    );
    assert!(stderr.contains("has overflowed its stack"), "{stderr}");
}
