//! How the agent restarts itself once it is serving (#388).
//!
//! Several paths end the process deliberately and let the container restart policy bring
//! it back: a freshly provisioned driver volume or CUDA userspace, the `restart` command,
//! a hardware-policy candidate, a GPU-global fault. Two things went wrong with that:
//!
//! - **The exit itself crashed.** `std::process::exit` runs libc's exit handlers, and those
//!   unload the C and C++ libraries the process has open: the Vulkan loader and ICD, Mesa,
//!   the NVIDIA stack. A session's compositor and encoder threads are still inside those
//!   libraries while that happens. On the lab this showed up as a segfault in
//!   `__cxa_finalize` on the exiting thread, or in `libgstvulkan.so` on the compositor's
//!   `waylanddisplays` thread. [`exit_now`] ends the process with `_exit`, which runs no
//!   handlers at all. Nothing is lost: logs go to unbuffered stderr, Rust runs no
//!   destructors on either path, and every durable write in the agent is synced before it
//!   returns.
//! - **A session was accepted just before the exit.** The provisioning restarts fire a few
//!   seconds to minutes after a fresh agent registers, which is exactly when the first
//!   session of a new install arrives. While a restart is [`pending`], the agent refuses
//!   new session assignments with a clear reason instead of starting one it is about to kill.

use std::io::Write;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::{Duration, Instant};

static PENDING: AtomicBool = AtomicBool::new(false);
static LIVE_SESSIONS: AtomicUsize = AtomicUsize::new(0);

/// The reason a session assignment is refused with while a restart is pending.
pub const REFUSAL_REASON: &str = "agent restarting to finish host setup";

/// True while the process has decided to restart and must not take new sessions.
pub fn pending() -> bool {
    PENDING.load(Ordering::SeqCst)
}

/// Refuse new sessions from now on: this process is about to restart.
pub fn mark_pending() {
    PENDING.store(true, Ordering::SeqCst);
}

/// The restart was called off (it deferred to the next agent start). Take sessions again.
pub fn clear_pending() {
    PENDING.store(false, Ordering::SeqCst);
}

/// Record how many sessions this process holds, pending assignments included. The session
/// manager calls this every time either set changes.
pub fn note_sessions(n: usize) {
    LIVE_SESSIONS.store(n, Ordering::SeqCst);
}

/// Sessions this process holds right now, pending assignments included.
pub fn live_sessions() -> usize {
    LIVE_SESSIONS.load(Ordering::SeqCst)
}

/// How long [`wait_until_idle_then_seal`] watches an idle agent after refusing new
/// sessions, to catch an assignment that was admitted just before the refusal took hold.
/// Admission and registration happen in one synchronous step of the session manager, so a
/// few seconds is ample.
pub const SEAL_SETTLE: Duration = Duration::from_secs(2);

/// Wait until the agent holds no sessions, then refuse new ones and return `true`.
///
/// For a restart that can wait for players, such as the CUDA userspace one: a session in
/// progress finishes normally, and the host keeps taking sessions until it is idle.
/// Returns `false` without refusing anything if `max` passes first.
pub fn wait_until_idle_then_seal(max: Duration, poll: Duration) -> bool {
    wait_until_idle_then_seal_with(
        max,
        poll,
        SEAL_SETTLE,
        &live_sessions,
        &std::thread::sleep,
        &|on| PENDING.store(on, Ordering::SeqCst),
    )
}

fn wait_until_idle_then_seal_with(
    max: Duration,
    poll: Duration,
    settle: Duration,
    live: &dyn Fn() -> usize,
    sleep: &dyn Fn(Duration),
    set_pending: &dyn Fn(bool),
) -> bool {
    let started = Instant::now();
    let mut waited = Duration::ZERO;
    loop {
        if live() == 0 {
            set_pending(true);
            sleep(settle);
            if live() == 0 {
                return true;
            }
            // An assignment slipped in before the refusal; let it play out.
            set_pending(false);
        }
        if waited >= max || started.elapsed() >= max {
            return false;
        }
        sleep(poll);
        waited += poll;
    }
}

/// End the process now, without running libc exit handlers (see the module docs).
pub fn exit_now(code: i32) -> ! {
    let _ = std::io::stderr().flush();
    let _ = std::io::stdout().flush();
    // SAFETY: `_exit` takes an int and never returns. It skips atexit handlers and
    // library destructors on purpose; see the module docs.
    unsafe { libc::_exit(code) }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::{Cell, RefCell};

    // The tests record the refusal flag instead of setting the process-wide one, which
    // the session manager's own tests read.

    #[test]
    fn an_idle_agent_seals_and_stays_sealed() {
        let flag = Cell::new(false);
        let sleeps = RefCell::new(Vec::new());
        let sealed = wait_until_idle_then_seal_with(
            Duration::from_secs(60),
            Duration::from_secs(5),
            Duration::from_secs(2),
            &|| 0,
            &|d| sleeps.borrow_mut().push(d),
            &|on| flag.set(on),
        );
        assert!(sealed);
        assert!(flag.get(), "a sealed agent refuses new sessions");
        assert_eq!(*sleeps.borrow(), vec![Duration::from_secs(2)]);
    }

    #[test]
    fn a_session_admitted_during_the_settle_unseals_and_waits_for_it() {
        // Idle, then a session appears during the settle, outlives three polls, and ends.
        let reads = [0usize, 1, 1, 1, 0, 0];
        let i = Cell::new(0);
        let flag = Cell::new(false);
        let flag_at_each_sleep = RefCell::new(Vec::new());
        let sealed = wait_until_idle_then_seal_with(
            Duration::from_secs(600),
            Duration::from_secs(5),
            Duration::from_secs(2),
            &|| {
                let v = reads[i.get().min(reads.len() - 1)];
                i.set(i.get() + 1);
                v
            },
            &|_| flag_at_each_sleep.borrow_mut().push(flag.get()),
            &|on| flag.set(on),
        );
        assert!(sealed);
        assert!(flag.get());
        // Refusing during the first settle, taking sessions again while that one ran,
        // refusing for good at the end.
        assert_eq!(
            *flag_at_each_sleep.borrow(),
            vec![true, false, false, false, true]
        );
    }

    #[test]
    fn a_busy_agent_gives_up_at_the_deadline_without_refusing_anything() {
        let flag = Cell::new(false);
        let polls = Cell::new(0);
        let sealed = wait_until_idle_then_seal_with(
            Duration::from_secs(20),
            Duration::from_secs(5),
            Duration::from_secs(2),
            &|| 1,
            &|_| polls.set(polls.get() + 1),
            &|on| flag.set(on),
        );
        assert!(!sealed);
        assert!(!flag.get(), "giving up must leave the host taking sessions");
        assert_eq!(polls.get(), 4);
    }
}
