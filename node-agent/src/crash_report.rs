//! A report on stderr when a fatal signal kills the process, for hosts where no core is
//! written and the kernel log cannot be read (#267).
//!
//! The kernel's own `segfault at` line names the faulting thread and, when it can take
//! the address-space lock, the library. It never gives a call chain, and a containerised
//! agent cannot read it. This prints the thread, the fault address and a backtrace into
//! the agent's log, then hands the signal back so the process dies exactly as before.
//!
//! The handler allocates nothing and takes no lock of its own. Its first line is built
//! from syscalls alone. The backtrace is glibc's: `backtrace` is lock-free once libgcc is
//! loaded, which [`install`] sees to, and `backtrace_symbols_fd` does not allocate but
//! does take the loader's lock, so an alarm bounds the wait. A fault inside the handler
//! is a second synchronous signal while the first is blocked, which the kernel answers
//! with the default action: the process still dies of the signal.

use std::ffi::c_void;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::OnceLock;

const SIGNALS: [(libc::c_int, &str); 5] = [
    (libc::SIGSEGV, "SIGSEGV"),
    (libc::SIGBUS, "SIGBUS"),
    (libc::SIGILL, "SIGILL"),
    (libc::SIGFPE, "SIGFPE"),
    (libc::SIGABRT, "SIGABRT"),
];
const MAX_FRAMES: usize = 48;
/// How long the symbol lookup may wait on the loader's lock before SIGALRM ends the
/// process: a report must never leave a half-dead agent running.
const REPORT_DEADLINE_S: libc::c_uint = 10;

/// The dispositions in force before [`install`], restored once the report is out. Rust's
/// runtime owns SIGSEGV and SIGBUS (its stack-overflow message), the rest are default.
static PREVIOUS: OnceLock<[libc::sigaction; SIGNALS.len()]> = OnceLock::new();
static REPORTING: AtomicBool = AtomicBool::new(false);

/// Install the handler. Call once, before any thread that could fault exists.
pub fn install() {
    // The first `backtrace` loads libgcc, which allocates and locks: not in the handler.
    let mut warm = [std::ptr::null_mut::<c_void>(); 2];
    // SAFETY: `warm` holds the two frames asked for.
    unsafe { libc::backtrace(warm.as_mut_ptr(), 2) };

    // SAFETY: a zeroed `sigaction` is a valid value, and each call reads one signal's
    // current disposition into it.
    let previous = SIGNALS.map(|(signal, _)| unsafe {
        let mut old: libc::sigaction = std::mem::zeroed();
        libc::sigaction(signal, std::ptr::null(), &mut old);
        old
    });
    if PREVIOUS.set(previous).is_err() {
        return;
    }
    for (signal, _) in SIGNALS {
        // SAFETY: `handler` has the `SA_SIGINFO` signature and keeps to
        // async-signal-safe work (module docs).
        unsafe {
            let mut action: libc::sigaction = std::mem::zeroed();
            action.sa_sigaction = handler as *const () as usize;
            action.sa_flags = libc::SA_SIGINFO | libc::SA_ONSTACK;
            libc::sigemptyset(&mut action.sa_mask);
            libc::sigaction(signal, &action, std::ptr::null_mut());
        }
    }
}

/// A fixed line: the handler may not allocate or reach for `fmt`.
struct Line {
    bytes: [u8; 256],
    len: usize,
}

impl Line {
    fn text(&mut self, text: &[u8]) {
        for &byte in text {
            if self.len < self.bytes.len() {
                self.bytes[self.len] = byte;
                self.len += 1;
            }
        }
    }

    fn number(&mut self, mut value: u64, radix: u64) {
        let mut digits = [0u8; 20];
        let mut at = digits.len();
        loop {
            at -= 1;
            digits[at] = b"0123456789abcdef"[(value % radix) as usize];
            value /= radix;
            if value == 0 {
                break;
            }
        }
        self.text(&digits[at..]);
    }
}

#[cfg(target_arch = "x86_64")]
unsafe fn ip_and_sp(context: *mut c_void) -> (u64, u64) {
    let registers = &(*context.cast::<libc::ucontext_t>()).uc_mcontext.gregs;
    (
        registers[libc::REG_RIP as usize] as u64,
        registers[libc::REG_RSP as usize] as u64,
    )
}

#[cfg(target_arch = "aarch64")]
unsafe fn ip_and_sp(context: *mut c_void) -> (u64, u64) {
    let machine = &(*context.cast::<libc::ucontext_t>()).uc_mcontext;
    (machine.pc, machine.sp)
}

#[cfg(not(any(target_arch = "x86_64", target_arch = "aarch64")))]
unsafe fn ip_and_sp(_context: *mut c_void) -> (u64, u64) {
    (0, 0)
}

extern "C" fn handler(signal: libc::c_int, info: *mut libc::siginfo_t, context: *mut c_void) {
    // A second thread faulting while the first reports returns straight into its
    // fault, and so spins until the first has restored the old disposition.
    if REPORTING.swap(true, Ordering::SeqCst) {
        return;
    }
    let index = SIGNALS.iter().position(|(s, _)| *s == signal);
    // SAFETY: the kernel hands a valid `siginfo_t` and `ucontext_t` to an `SA_SIGINFO`
    // handler; everything called is a syscall or one of the two glibc calls the module
    // docs account for, and every buffer is on this stack.
    unsafe {
        let mut thread = [0u8; 16];
        libc::prctl(libc::PR_GET_NAME, thread.as_mut_ptr());
        let thread_len = thread.iter().position(|&b| b == 0).unwrap_or(thread.len());
        let (ip, sp) = ip_and_sp(context);

        let mut line = Line {
            bytes: [0; 256],
            len: 0,
        };
        line.text(b"quasar-node-agent: fatal signal ");
        line.number(signal as u64, 10);
        line.text(b" (");
        line.text(index.map_or("?", |i| SIGNALS[i].1).as_bytes());
        line.text(b") in thread ");
        line.text(&thread[..thread_len]);
        line.text(b" tid ");
        line.number(libc::syscall(libc::SYS_gettid) as u64, 10);
        line.text(b": fault address 0x");
        line.number((*info).si_addr() as u64, 16);
        line.text(b" ip 0x");
        line.number(ip, 16);
        line.text(b" sp 0x");
        line.number(sp, 16);
        line.text(b"; backtrace follows, the faulting frame is the one at ip\n");
        libc::write(libc::STDERR_FILENO, line.bytes.as_ptr().cast(), line.len);

        libc::alarm(REPORT_DEADLINE_S);
        let mut frames = [std::ptr::null_mut::<c_void>(); MAX_FRAMES];
        let count = libc::backtrace(frames.as_mut_ptr(), MAX_FRAMES as libc::c_int);
        libc::backtrace_symbols_fd(frames.as_ptr(), count, libc::STDERR_FILENO);
        libc::alarm(0);

        // Hand the signal back: a fault runs again and meets the old disposition (Rust's
        // stack-overflow check, then the default), a raised signal has to be raised again.
        if let (Some(index), Some(previous)) = (index, PREVIOUS.get()) {
            libc::sigaction(signal, &previous[index], std::ptr::null_mut());
        } else {
            libc::signal(signal, libc::SIG_DFL);
        }
        if (*info).si_code <= 0 {
            libc::raise(signal);
        }
    }
}
