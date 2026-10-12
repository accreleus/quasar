//! A report on stderr when a fatal signal kills the process, for hosts where no core is
//! written and the kernel log cannot be read (#267).
//!
//! Every line starts `quasar-node-agent <role>[<pid>]:`, the role being the subcommand,
//! because a probe child's stderr is the agent's log. Most robust evidence first:
//!
//! 1. the signal and its code, the thread's name and tid, the fault address, ip and sp;
//! 2. `stack`: 48 words from sp, read through `/proc/self/mem` so a bad sp cannot fault;
//! 3. `map`: from `/proc/self/maps`, each executable mapping and each file mapping at
//!    offset 0;
//! 4. `frame`: the unwinder's return addresses, as raw hex;
//! 5. `end of report`, or `end of report, incomplete` when the mappings could not be
//!    read to their end. A report with neither was cut short.
//!
//! # Reading a report
//!
//! Symbols are worked out offline, against the files of the same image. For an address
//! (ip, a `frame`, or a `stack` word that falls in an executable mapping, such as the
//! return address a wild call leaves at sp): find the `map` line whose range holds it,
//! take the lowest `map` line of the same file as that file's base, and ask
//! `llvm-symbolizer --obj=<file> 0x<address - base>` or
//! `addr2line -Cfie <file> 0x<address - base>`. The lowest, because a file can have
//! more than one line at offset 0: lld lets a small library's code share its first page.
//! The first `frame` lines are the reporter's own and the signal trampoline; the fault
//! is the frame equal to ip.
//!
//! # What the handler does and does not promise
//!
//! - Steps 1, 2, 3 and 5 make syscalls and do arithmetic on their own static buffers: no
//!   lock, no allocation, no timer. A syscall a signal interrupts is made again.
//! - Step 4 is glibc's `backtrace`, which is not async-signal-safe by contract. What it
//!   does with the libgcc and glibc the images ship: it finds unwind data without a lock
//!   (the `locked` test has another thread inside `dlopen`), and it locks and allocates
//!   only when the walk reaches code registered at run time, a JIT's, that libgcc has
//!   not sorted yet. A fault inside `malloc` below such a frame would deadlock it;
//!   nothing bounds that. With no unwind data for an address it reads the code there,
//!   and faults if that is unmapped, so it runs last, and not at all when ip is outside
//!   every readable executable mapping (a wild call or jump).
//! - Step 4 can still fault, on a smashed stack. Frames are printed once the walk is
//!   over, so that loses every frame, and it can change the signal the process dies of;
//!   see "re-entry" below.
//! - It can stall: a `write` to stderr blocks for as long as the log pipe is full.
//! - It can be skipped: on a thread with no alternate stack and a smashed or exhausted
//!   stack the kernel cannot run a handler at all. The process dies of SIGSEGV unreported.
//!
//! # How the process dies
//!
//! As it did before, with two exceptions named below. Once the report is out the
//! reporter retires: every signal it took goes back to the disposition [`install`]
//! found. A fault then runs again and meets Rust's stack-overflow check (SIGSEGV,
//! SIGBUS) or the default action. Retiring all of them, not only the reported one, lets
//! the abort that ends Rust's stack-overflow message die by default, with no second
//! handler frame on Rust's 8 KiB alternate stack, where a larger version of this handler
//! was seen not to fit.
//!
//! - **A raised signal, and SIGTRAP**, which does not run again: the reporter ends the
//!   process by the signal's default action. First exception: that includes a SIGSEGV
//!   or SIGBUS the process raises at itself, which Rust's handler alone swallowed once.
//! - **The init of a PID namespace** (a pod, a bare `docker run`): the kernel drops a
//!   raised default-fatal signal for it, so wherever the reporter ends the process that
//!   way and the process is still there, it exits with 128 + the signal, the status a
//!   shell gives such a death. Second exception: a fatal signal *sent* to such a
//!   process, which the kernel would have dropped, now ends it.
//! - **Sent by another process** (`kill`): not a fault, so one line and no report, and
//!   the reporter stays. What follows is what followed before this module, and is kept
//!   on purpose: the first SIGSEGV or SIGBUS is survived, because Rust's handler
//!   swallowed it, and the second kills; any other signal kills at once. A real fault
//!   after a survived `kill` is still reported.
//! - **Re-entry**, a second signal while a report is being written: no second report.
//!   On another thread, that thread waits for the report to finish, two seconds at
//!   most, and then dies of its own signal by default: dying at once cut the report
//!   off. On the reporting thread it is the report that faulted, and the process is
//!   made to die of the signal being reported instead. Two cases escape that: a fault
//!   with the reported signal's own number, which the kernel answers with the default
//!   action because the signal is blocked, and a fault on Rust's alternate stack with no
//!   room left for a second frame, which it answers with SIGSEGV. The first loses only
//!   Rust's stack-overflow message. The second changes the signal the process dies of,
//!   and only for a report of something other than SIGSEGV, on a Rust-spawned thread,
//!   whose unwinding then faults. The other reported signals are left unblocked in
//!   `sa_mask` for this: blocked, the kernel would always kill with the report's own
//!   fault.

use std::cell::UnsafeCell;
use std::ffi::c_void;
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering};
use std::sync::OnceLock;

const SIGNALS: [(libc::c_int, &str); 6] = [
    (libc::SIGSEGV, "SIGSEGV"),
    (libc::SIGBUS, "SIGBUS"),
    (libc::SIGILL, "SIGILL"),
    (libc::SIGFPE, "SIGFPE"),
    (libc::SIGABRT, "SIGABRT"),
    // GLib's `g_error` and failed assertions die of a breakpoint trap, not an abort.
    (libc::SIGTRAP, "SIGTRAP"),
];
const STACK_WORDS: usize = 48;
const MAX_FRAMES: usize = 48;
const WORD: usize = std::mem::size_of::<u64>();

struct Setup {
    /// The dispositions in force before [`install`]. Rust's runtime owns SIGSEGV and
    /// SIGBUS (its stack-overflow message); the rest are the default.
    previous: [libc::sigaction; SIGNALS.len()],
    role: [u8; 24],
    role_len: usize,
}

static SETUP: OnceLock<Setup> = OnceLock::new();
static REPORTING: AtomicBool = AtomicBool::new(false);
static REPORTER_TID: AtomicI32 = AtomicI32::new(0);
static REPORTED_SIGNAL: AtomicI32 = AtomicI32::new(0);
static REPORT_DONE: AtomicBool = AtomicBool::new(false);
/// How long a thread that faults during another's report waits for it to finish.
const WAIT_FOR_REPORT_MS: i64 = 2000;
/// Whether a SIGSEGV / SIGBUS sent by another process has been ignored once already.
static SENT_ONCE: [AtomicBool; SIGNALS.len()] = [const { AtomicBool::new(false) }; SIGNALS.len()];

/// The reporter's buffers. Static, not on the stack: the handler may be running on
/// Rust's 8 KiB alternate stack with the unwinder still to come.
struct Scratch {
    chunk: [u8; 4096],
    map_line: [u8; 512],
    out: [u8; 640],
    words: [u64; STACK_WORDS],
    frames: [*mut c_void; MAX_FRAMES],
}

struct ScratchCell(UnsafeCell<Scratch>);

// SAFETY: only the thread that won `REPORTING` touches it, and that happens once.
unsafe impl Sync for ScratchCell {}

static SCRATCH: ScratchCell = ScratchCell(UnsafeCell::new(Scratch {
    chunk: [0; 4096],
    map_line: [0; 512],
    out: [0; 640],
    words: [0; STACK_WORDS],
    frames: [std::ptr::null_mut(); MAX_FRAMES],
}));

/// Install the handler. Call once, while the process has one thread. `role` names the
/// process in every line; only `[a-z0-9-]` of it is kept.
pub fn install(role: &str) {
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
    let mut setup = Setup {
        previous,
        role: [0; 24],
        role_len: 0,
    };
    for byte in role
        .bytes()
        .filter(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || *b == b'-')
        .take(setup.role.len())
    {
        setup.role[setup.role_len] = byte;
        setup.role_len += 1;
    }
    if SETUP.set(setup).is_err() {
        return;
    }
    for (signal, _) in SIGNALS {
        // SAFETY: `handler` has the `SA_SIGINFO` signature and keeps to what the module
        // docs allow it.
        unsafe {
            let mut action: libc::sigaction = std::mem::zeroed();
            action.sa_sigaction = handler as *const () as usize;
            action.sa_flags = libc::SA_SIGINFO | libc::SA_ONSTACK;
            libc::sigemptyset(&mut action.sa_mask);
            libc::sigaction(signal, &action, std::ptr::null_mut());
        }
    }
}

/// One output line, built without `fmt` or the heap.
struct Line<'a> {
    bytes: &'a mut [u8],
    len: usize,
}

impl Line<'_> {
    fn text(&mut self, text: &[u8]) -> &mut Self {
        for &byte in text {
            if self.len < self.bytes.len() {
                self.bytes[self.len] = byte;
                self.len += 1;
            }
        }
        self
    }

    fn digits(&mut self, mut value: u64, radix: u64) -> &mut Self {
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
        self.text(&digits[at..])
    }

    fn dec(&mut self, value: i64) -> &mut Self {
        if value < 0 {
            self.text(b"-");
        }
        self.digits(value.unsigned_abs(), 10)
    }

    fn hex(&mut self, value: u64) -> &mut Self {
        self.text(b"0x").digits(value, 16)
    }

    /// Start a line: `quasar-node-agent <role>[<pid>]: `.
    fn begin(&mut self) -> &mut Self {
        self.len = 0;
        self.text(b"quasar-node-agent ");
        if let Some(setup) = SETUP.get() {
            self.text(&setup.role[..setup.role_len]);
        }
        // SAFETY: `getpid` is a plain syscall.
        let pid = unsafe { libc::getpid() };
        self.text(b"[").dec(pid.into()).text(b"]: ")
    }

    /// Write the line to stderr. Blocks while the log pipe is full; nothing else can
    /// be done about that from here.
    fn end(&mut self) {
        self.text(b"\n");
        let mut rest = &self.bytes[..self.len];
        while !rest.is_empty() {
            // SAFETY: `rest` is a live slice of this line; errno is this thread's.
            let (wrote, interrupted) = unsafe {
                let wrote = libc::write(libc::STDERR_FILENO, rest.as_ptr().cast(), rest.len());
                (wrote, *libc::__errno_location() == libc::EINTR)
            };
            if wrote > 0 {
                rest = &rest[wrote as usize..];
            } else if !interrupted {
                break;
            }
        }
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

fn name_of(signal: libc::c_int) -> &'static [u8] {
    SIGNALS
        .iter()
        .find(|(s, _)| *s == signal)
        .map_or(&b"?"[..], |(_, name)| name.as_bytes())
}

/// The calling thread's kernel name, and how much of the buffer it fills.
unsafe fn thread_name(name: &mut [u8; 16]) -> usize {
    libc::prctl(libc::PR_GET_NAME, name.as_mut_ptr());
    name.iter().position(|&b| b == 0).unwrap_or(name.len())
}

/// Whether returning from the handler raises `signal` again. A fault does; a raised
/// signal does not, and nor does SIGTRAP, a trap: the instruction after it runs.
fn runs_again(signal: libc::c_int, sent: bool) -> bool {
    !sent && signal != libc::SIGTRAP
}

/// Give every reported signal back to what [`install`] found. A signal that would not
/// run again ends the process here.
unsafe fn retire(signal: libc::c_int, sent: bool) {
    for (index, (reported, _)) in SIGNALS.iter().enumerate() {
        match SETUP.get() {
            Some(setup) => {
                libc::sigaction(*reported, &setup.previous[index], std::ptr::null_mut());
            }
            None => {
                libc::signal(*reported, libc::SIG_DFL);
            }
        }
    }
    if !runs_again(signal, sent) {
        die_of(signal);
    }
}

/// End the process by `signal`'s default action, from inside a handler that may have
/// it blocked. Where that does not end it, exit with the status a shell gives such a
/// death: the kernel drops a raised default-fatal signal for a PID namespace's init.
unsafe fn die_of(signal: libc::c_int) -> ! {
    libc::signal(signal, libc::SIG_DFL);
    let mut only: libc::sigset_t = std::mem::zeroed();
    libc::sigemptyset(&mut only);
    libc::sigaddset(&mut only, signal);
    libc::pthread_sigmask(libc::SIG_UNBLOCK, &only, std::ptr::null_mut());
    libc::raise(signal);
    libc::_exit(128 + signal)
}

/// Let the thread that is reporting finish, for a bounded time.
unsafe fn wait_for_the_report() {
    let now_ms = || {
        let mut now: libc::timespec = std::mem::zeroed();
        libc::clock_gettime(libc::CLOCK_MONOTONIC, &mut now);
        now.tv_sec * 1000 + now.tv_nsec / 1_000_000
    };
    let started = now_ms();
    let nap = libc::timespec {
        tv_sec: 0,
        tv_nsec: 1_000_000,
    };
    while !REPORT_DONE.load(Ordering::SeqCst) && now_ms() - started < WAIT_FOR_REPORT_MS {
        libc::nanosleep(&nap, std::ptr::null_mut());
    }
}

/// A `read`-like call, made again for as long as a signal interrupts it: `sa_mask` is
/// empty, so an unrelated handler can run in the middle of a report.
unsafe fn uninterrupted(mut call: impl FnMut() -> isize) -> isize {
    loop {
        let got = call();
        if got >= 0 || *libc::__errno_location() != libc::EINTR {
            return got;
        }
    }
}

/// Step 2. `/proc/self/mem` instead of a read through sp: the kernel answers an
/// unreadable address with a short count, not a signal.
unsafe fn stack_words(scratch: &mut Scratch, sp: u64) {
    let mut line = Line {
        bytes: &mut scratch.out,
        len: 0,
    };
    let fd = libc::open(c"/proc/self/mem".as_ptr(), libc::O_RDONLY | libc::O_CLOEXEC);
    let got = if fd < 0 {
        -1
    } else {
        let got = uninterrupted(|| {
            libc::pread(
                fd,
                scratch.words.as_mut_ptr().cast(),
                STACK_WORDS * WORD,
                sp as libc::off_t,
            )
        });
        libc::close(fd);
        got
    };
    let words = if got > 0 { got as usize / WORD } else { 0 };
    if words == 0 {
        line.begin().text(b"stack: nothing readable at sp").end();
        return;
    }
    for (row, chunk) in scratch.words[..words].chunks(4).enumerate() {
        line.begin()
            .text(b"stack ")
            .hex(sp + (row * 4 * WORD) as u64)
            .text(b":");
        for &word in chunk {
            line.text(b" ").hex(word);
        }
        line.end();
    }
}

/// `start-end perms offset dev inode path` → (start, end, readable and executable,
/// printed). Printed: executable, or a file's offset-0 mapping.
fn parse_map(line: &[u8]) -> Option<(u64, u64, bool, bool)> {
    fn hex(bytes: &[u8]) -> Option<(u64, &[u8])> {
        let digits = bytes.iter().take_while(|b| b.is_ascii_hexdigit()).count();
        let text = std::str::from_utf8(&bytes[..digits]).ok()?;
        Some((u64::from_str_radix(text, 16).ok()?, &bytes[digits..]))
    }
    let (start, rest) = hex(line)?;
    let (end, rest) = hex(rest.strip_prefix(b"-")?)?;
    let rest = rest.strip_prefix(b" ")?;
    let (perms, rest) = (rest.get(..4)?, rest.get(4..)?);
    let (offset, rest) = hex(rest.strip_prefix(b" ")?)?;
    let executable = perms[2] == b'x';
    let file_base = offset == 0 && rest.contains(&b'/');
    Some((
        start,
        end,
        executable && perms[0] == b'r',
        executable || file_base,
    ))
}

/// Step 3. `Some`: whether `ip` lies in a readable executable mapping. `None`: the
/// mappings could not be read to their end, so nothing can be said about `ip`.
unsafe fn maps(scratch: &mut Scratch, ip: u64) -> Option<bool> {
    let mut line = Line {
        bytes: &mut scratch.out,
        len: 0,
    };
    let fd = libc::open(
        c"/proc/self/maps".as_ptr(),
        libc::O_RDONLY | libc::O_CLOEXEC,
    );
    if fd < 0 {
        line.begin()
            .text(b"map: /proc/self/maps did not open")
            .end();
        return None;
    }
    let (mut ip_mapped, mut held) = (false, 0);
    loop {
        let got = uninterrupted(|| {
            libc::read(fd, scratch.chunk.as_mut_ptr().cast(), scratch.chunk.len())
        });
        if got < 0 {
            libc::close(fd);
            return None;
        }
        if got == 0 {
            break;
        }
        for &byte in &scratch.chunk[..got as usize] {
            if byte != b'\n' {
                if held < scratch.map_line.len() {
                    scratch.map_line[held] = byte;
                    held += 1;
                }
                continue;
            }
            let map = &scratch.map_line[..held];
            held = 0;
            if let Some((start, end, code, printed)) = parse_map(map) {
                ip_mapped |= code && (start..end).contains(&ip);
                if printed {
                    line.begin().text(b"map ").text(map).end();
                }
            }
        }
    }
    libc::close(fd);
    Some(ip_mapped)
}

/// A signal another process sent. One line, then what happened before this module:
/// Rust's handler swallowed the first SIGSEGV or SIGBUS and left the signal at its
/// default, so the first is ignored and the second kills; anything else kills at once.
#[inline(never)]
unsafe fn sent_by_another(signal: libc::c_int, info: &libc::siginfo_t) {
    let index = SIGNALS.iter().position(|(s, _)| *s == signal);
    let previous = index
        .zip(SETUP.get())
        .map_or(libc::SIG_DFL, |(i, setup)| setup.previous[i].sa_sigaction);
    if previous == libc::SIG_IGN {
        return;
    }
    let ignore = previous != libc::SIG_DFL
        && index.is_some_and(|i| !SENT_ONCE[i].swap(true, Ordering::SeqCst));
    let mut name = [0u8; 16];
    let name_len = thread_name(&mut name);
    let mut bytes = [0u8; 200];
    let mut line = Line {
        bytes: &mut bytes,
        len: 0,
    };
    line.begin()
        .text(b"signal ")
        .dec(signal.into())
        .text(b" (")
        .text(name_of(signal))
        .text(b") sent by pid ")
        .dec(info.si_pid().into())
        .text(b", taken on thread ")
        .text(&name[..name_len])
        .text(if ignore {
            &b": not a fault, ignored once"[..]
        } else {
            &b": not a fault, dying of it"[..]
        })
        .end();
    if !ignore {
        die_of(signal);
    }
}

/// Steps 1 to 5 of the module docs. Its own frame, so the handler's stays small for the
/// paths that write nothing.
#[inline(never)]
unsafe fn report(signal: libc::c_int, info: &libc::siginfo_t, context: *mut c_void, tid: i32) {
    let scratch = &mut *SCRATCH.0.get();
    let (ip, sp) = ip_and_sp(context);
    let mut name = [0u8; 16];
    let name_len = thread_name(&mut name);
    let mut line = Line {
        bytes: &mut scratch.out,
        len: 0,
    };
    line.begin()
        .text(b"fatal signal ")
        .dec(signal.into())
        .text(b" (")
        .text(name_of(signal))
        .text(b") code ")
        .dec(info.si_code.into())
        .text(b" in thread ")
        .text(&name[..name_len])
        .text(b" tid ")
        .dec(tid.into());
    if info.si_code <= 0 {
        line.text(b": raised by this process,");
    } else {
        line.text(b": fault address ").hex(info.si_addr() as u64);
    }
    line.text(b" ip ").hex(ip).text(b" sp ").hex(sp).end();

    stack_words(scratch, sp);
    let mapped = maps(scratch, ip);

    let mut line = Line {
        bytes: &mut scratch.out,
        len: 0,
    };
    match mapped {
        Some(true) => {
            let count = libc::backtrace(scratch.frames.as_mut_ptr(), MAX_FRAMES as libc::c_int);
            for (number, frame) in scratch.frames[..count.max(0) as usize].iter().enumerate() {
                line.begin()
                    .text(b"frame ")
                    .dec(number as i64)
                    .text(b" ")
                    .hex(*frame as u64)
                    .end();
            }
        }
        Some(false) => line
            .begin()
            .text(b"frame: none, ip is in no readable executable mapping")
            .end(),
        None => line
            .begin()
            .text(b"frame: none, the mappings could not be read to their end")
            .end(),
    }
    line.begin()
        .text(if mapped.is_some() {
            &b"end of report"[..]
        } else {
            &b"end of report, incomplete"[..]
        })
        .end();
}

extern "C" fn handler(signal: libc::c_int, info: *mut libc::siginfo_t, context: *mut c_void) {
    // SAFETY: the kernel hands a valid `siginfo_t` and `ucontext_t` to an `SA_SIGINFO`
    // handler. Everything reached from here is a syscall, or `backtrace` under the
    // conditions the module docs give, and `SCRATCH` belongs to the thread that wins
    // `REPORTING`.
    unsafe {
        let errno = *libc::__errno_location();
        let info = &*info;
        let sent = info.si_code <= 0;
        if sent && info.si_pid() != libc::getpid() {
            sent_by_another(signal, info);
            *libc::__errno_location() = errno;
            return;
        }
        let tid = libc::syscall(libc::SYS_gettid) as i32;
        if REPORTING.swap(true, Ordering::SeqCst) {
            if tid == REPORTER_TID.load(Ordering::SeqCst) {
                die_of(REPORTED_SIGNAL.load(Ordering::SeqCst));
            }
            // Another thread is mid-report, and dying now would cut it off.
            wait_for_the_report();
            if !runs_again(signal, sent) {
                die_of(signal);
            }
            libc::signal(signal, libc::SIG_DFL);
            return;
        }
        REPORTED_SIGNAL.store(signal, Ordering::SeqCst);
        REPORTER_TID.store(tid, Ordering::SeqCst);
        report(signal, info, context, tid);
        // From here a later signal on this thread is its own, not the report's.
        REPORTER_TID.store(0, Ordering::SeqCst);
        REPORT_DONE.store(true, Ordering::SeqCst);
        retire(signal, sent);
    }
}

#[cfg(test)]
mod tests {
    use super::parse_map;

    #[test]
    fn a_maps_line_gives_its_range_and_whether_it_is_code_or_a_load_base() {
        let code = b"7f2a1c400000-7f2a1c5a1000 r-xp 00028000 fd:01 1234  /usr/lib64/libc.so.6";
        assert_eq!(
            parse_map(code),
            Some((0x7f2a1c400000, 0x7f2a1c5a1000, true, true))
        );
        let base = b"7f2a1c3d8000-7f2a1c400000 r--p 00000000 fd:01 1234  /usr/lib64/libc.so.6";
        assert_eq!(
            parse_map(base),
            Some((0x7f2a1c3d8000, 0x7f2a1c400000, false, true))
        );
        let data = b"7f2a1c5f8000-7f2a1c5fc000 rw-p 001f8000 fd:01 1234  /usr/lib64/libc.so.6";
        assert_eq!(parse_map(data).map(|m| m.3), Some(false));
        let heap = b"55d0a0e00000-55d0a0e21000 rw-p 00000000 00:00 0  [heap]";
        assert_eq!(parse_map(heap).map(|m| m.3), Some(false));
        // Execute-only: printed, but not safe for the unwinder to read.
        let xom = b"7f0000000000-7f0000001000 --xp 00000000 00:00 0";
        assert_eq!(parse_map(xom).map(|m| (m.2, m.3)), Some((false, true)));
        assert_eq!(parse_map(b"not a maps line"), None);
    }
}
