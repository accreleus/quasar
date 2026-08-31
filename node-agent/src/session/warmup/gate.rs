//! #488 §3.4 — the #489 serialization gate.
//!
//! The known agent SIGSEGV is one session's NVENC teardown overlapping another
//! session's live encoder. A warm-up is a full session in every respect that
//! matters to that bug, so it is gated by three rules, all implemented here:
//!
//! 1. **One warm-up at a time per host** — [`WarmupControl::try_acquire`] is the
//!    host-global lock.
//! 2. **A warm-up only starts when the host has zero live and zero tearing-down
//!    sessions** — [`HostActivity::live`] is maintained by the agent loop at the
//!    same sites that maintain `/health`'s session count; a session counts
//!    until its runner emits its terminal event, i.e. *after* teardown. A short
//!    [`HostActivity::quiet_for`] margin on top means a warm-up never starts in
//!    the microseconds between "the encoder released" and "the manager noticed".
//! 3. **A user session launch always wins** — the agent loop calls
//!    [`WarmupControl::abort_for_user_launch`] the moment a `session_assign`
//!    lands; the job observes it at every phase boundary and wait loop. A
//!    warm-up must never make a real user wait, and must never share a
//!    teardown window with a live encoder.
//!
//! Nothing here starts, stops, or knows about GStreamer — pure bookkeeping so
//! all three rules are unit tested without a GPU.

use std::sync::atomic::{AtomicBool, AtomicU64, AtomicU8, AtomicUsize, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

/// Extra stillness required beyond "the manager reports zero sessions", so a
/// warm-up can never start inside the tail of another session's NVENC
/// teardown. The manager already drops a session from `running` only after
/// its runner's terminal event (post-teardown), so this margin is defence in
/// depth against #489, not a correctness requirement — small enough to be
/// invisible on the common path.
pub const POST_SESSION_QUIET: Duration = Duration::from_secs(5);

// The §3.4 retry ladder (30 s, doubling, capped at 15 min) lives in the jobs
// framework as `job_runs.attempt` + `scheduled_for` (design §3.4), persisted
// across a reconnect/restart/redeploy and visible in the admin viewer. What
// stays here is the part that cannot move out of process: the gate itself.

/// Live-session bookkeeping shared between the agent loop (writer) and the
/// warm-up scheduler thread (reader).
///
/// `Instant` has no atomic representation, so the last-transition timestamp is
/// a monotonic **millisecond offset from a fixed epoch** captured at
/// construction — keeps the struct lock-free on the read path, which matters
/// since the agent loop writes it on every session start/stop.
#[derive(Debug)]
pub struct HostActivity {
    live: AtomicUsize,
    epoch: Mutex<Instant>,
    last_change_ms: AtomicU64,
}

impl Default for HostActivity {
    fn default() -> Self {
        HostActivity {
            live: AtomicUsize::new(0),
            epoch: Mutex::new(Instant::now()),
            last_change_ms: AtomicU64::new(0),
        }
    }
}

impl HostActivity {
    pub fn new() -> Self {
        Self::default()
    }

    fn now_ms(&self, now: Instant) -> u64 {
        let epoch = *self.epoch.lock().unwrap_or_else(|e| e.into_inner());
        now.saturating_duration_since(epoch).as_millis() as u64
    }

    /// Record the host's current live-session count. Called by the agent loop
    /// wherever `/health`'s session count updates, so "live" here means exactly
    /// what the manager's `running` map means: assigned-and-started sessions,
    /// including one mid-teardown (it leaves the map only after its runner's
    /// terminal event).
    pub fn set_live(&self, n: usize, now: Instant) {
        let prev = self.live.swap(n, Ordering::SeqCst);
        if prev != n {
            self.last_change_ms
                .store(self.now_ms(now), Ordering::SeqCst);
        }
    }

    pub fn live(&self) -> usize {
        self.live.load(Ordering::SeqCst)
    }

    /// Zero live sessions, and no session-count transition for at least
    /// `margin`. The §3.4 precondition.
    pub fn quiet_for(&self, margin: Duration, now: Instant) -> bool {
        if self.live() != 0 {
            return false;
        }
        let since = self
            .now_ms(now)
            .saturating_sub(self.last_change_ms.load(Ordering::SeqCst));
        Duration::from_millis(since) >= margin
    }
}

/// Why a warm-up could not take the gate right now.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum GateRefusal {
    /// Another warm-up holds the host-global lock.
    Busy,
    /// The host has live (or just-finished) sessions — §3.4's zero-sessions
    /// precondition.
    HostBusy { live: usize },
}

/// Why the abort flag was raised.
///
/// Both causes stop the warm-up identically; the split exists so the
/// `deferred` reason an operator reads in the jobs viewer is TRUE, rather than
/// a control-plane restart with no session nearby reading as "users keep
/// interrupting the warm-up".
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AbortCause {
    /// No abort is pending.
    None,
    /// A `session_assign` landed — §3.4's "a user launch always wins".
    UserLaunch,
    /// The control-plane connection ended (`WarmupConnectionGuard`'s Drop). No
    /// session is involved: the warm-up must not run on into the next
    /// connection's first user session (#489).
    ConnectionLost,
}

impl AbortCause {
    fn from_u8(v: u8) -> Self {
        match v {
            1 => AbortCause::UserLaunch,
            2 => AbortCause::ConnectionLost,
            _ => AbortCause::None,
        }
    }

    fn as_u8(self) -> u8 {
        match self {
            AbortCause::None => 0,
            AbortCause::UserLaunch => 1,
            AbortCause::ConnectionLost => 2,
        }
    }

    /// The operator-facing phrase for a `deferred` outcome.
    pub fn reason(self) -> &'static str {
        match self {
            AbortCause::UserLaunch => "aborted for a user session launch",
            AbortCause::ConnectionLost => "aborted: the control-plane connection was lost",
            // Aborted with no recorded cause: stay neutral rather than guess.
            AbortCause::None => "warm-up aborted; it will retry",
        }
    }
}

/// The host-global warm-up lock plus the abort flag, shared (behind an `Arc`)
/// between the agent loop and the warm-up scheduler.
#[derive(Debug, Default)]
pub struct WarmupControl {
    held: AtomicBool,
    abort: AtomicBool,
    /// The [`AbortCause`] behind the current `abort`, as its `u8` encoding.
    abort_cause: AtomicU8,
    /// Whether a warm-up currently holds an encode-slot reservation, so
    /// `capacity` can report one fewer free slot for the duration (§3.3 step 2).
    reserved: AtomicBool,
}

impl WarmupControl {
    pub fn new() -> Self {
        Self::default()
    }

    /// Take the host-global gate iff nothing else holds it and the host is
    /// quiet. Dropping the returned guard releases the gate and clears the
    /// abort flag for the next attempt.
    pub fn try_acquire<'a>(
        &'a self,
        activity: &HostActivity,
        quiet: Duration,
        now: Instant,
    ) -> Result<WarmupGuard<'a>, GateRefusal> {
        if !activity.quiet_for(quiet, now) {
            return Err(GateRefusal::HostBusy {
                live: activity.live(),
            });
        }
        if self
            .held
            .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
            .is_err()
        {
            return Err(GateRefusal::Busy);
        }
        // Clear any leftover abort only now that we own the gate — earlier
        // would swallow an abort against a still-unwinding warm-up.
        self.abort_cause
            .store(AbortCause::None.as_u8(), Ordering::SeqCst);
        self.abort.store(false, Ordering::SeqCst);
        Ok(WarmupGuard { control: self })
    }

    /// A user session launch arrived: whatever warm-up is running must abort.
    /// Idempotent, and safe to call when no warm-up is running (the flag is
    /// cleared when the next one takes the gate).
    pub fn abort_for_user_launch(&self) {
        self.abort_with(AbortCause::UserLaunch);
    }

    /// The control-plane connection ended. Same stop, different truth: no user
    /// session is involved, so the reason must not claim one.
    pub fn abort_for_connection_lost(&self) {
        self.abort_with(AbortCause::ConnectionLost);
    }

    fn abort_with(&self, cause: AbortCause) {
        // Cause written BEFORE the flag so a reader observing the abort never
        // sees a stale `None` behind it.
        self.abort_cause.store(cause.as_u8(), Ordering::SeqCst);
        self.abort.store(true, Ordering::SeqCst);
    }

    /// The cause behind the current abort, or [`AbortCause::None`] when no abort
    /// is pending.
    pub fn abort_cause(&self) -> AbortCause {
        if !self.aborted() {
            return AbortCause::None;
        }
        AbortCause::from_u8(self.abort_cause.load(Ordering::SeqCst))
    }

    pub fn aborted(&self) -> bool {
        self.abort.load(Ordering::SeqCst)
    }

    /// Whether a warm-up is currently holding the gate — read by the capacity
    /// path to decide whether to report the reserved encode slot.
    pub fn active(&self) -> bool {
        self.held.load(Ordering::SeqCst)
    }

    pub fn reserved(&self) -> bool {
        self.reserved.load(Ordering::SeqCst)
    }

    pub(crate) fn set_reserved(&self, on: bool) {
        self.reserved.store(on, Ordering::SeqCst);
    }
}

/// Releases the host-global warm-up gate (and any capacity reservation) on every
/// exit path, including a panic in the job.
#[derive(Debug)]
pub struct WarmupGuard<'a> {
    control: &'a WarmupControl,
}

impl WarmupGuard<'_> {
    pub fn control(&self) -> &WarmupControl {
        self.control
    }

    /// Has a user launch (or a shutdown) asked this warm-up to abort?
    pub fn aborted(&self) -> bool {
        self.control.aborted()
    }
}

impl Drop for WarmupGuard<'_> {
    fn drop(&mut self) {
        self.control.set_reserved(false);
        self.control.held.store(false, Ordering::SeqCst);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_live_session_refuses_the_gate() {
        let now = Instant::now();
        let activity = HostActivity::new();
        let control = WarmupControl::new();
        activity.set_live(1, now);
        assert_eq!(
            control
                .try_acquire(&activity, Duration::ZERO, now)
                .unwrap_err(),
            GateRefusal::HostBusy { live: 1 }
        );
        assert!(!control.active());
    }

    #[test]
    fn the_gate_waits_out_the_post_session_quiet_margin() {
        let start = Instant::now();
        let activity = HostActivity::new();
        let control = WarmupControl::new();
        activity.set_live(1, start);
        activity.set_live(0, start + Duration::from_secs(1));
        // Zero sessions, but the transition was only a second ago.
        assert!(control
            .try_acquire(
                &activity,
                POST_SESSION_QUIET,
                start + Duration::from_secs(2)
            )
            .is_err());
        assert!(control
            .try_acquire(
                &activity,
                POST_SESSION_QUIET,
                start + Duration::from_secs(7)
            )
            .is_ok());
    }

    #[test]
    fn only_one_warmup_holds_the_gate_at_a_time() {
        let now = Instant::now();
        let activity = HostActivity::new();
        let control = WarmupControl::new();
        let first = control.try_acquire(&activity, Duration::ZERO, now).unwrap();
        assert_eq!(
            control
                .try_acquire(&activity, Duration::ZERO, now)
                .unwrap_err(),
            GateRefusal::Busy
        );
        assert!(control.active());
        drop(first);
        assert!(!control.active());
        assert!(control.try_acquire(&activity, Duration::ZERO, now).is_ok());
    }

    #[test]
    fn a_user_launch_aborts_the_running_warmup_and_the_flag_clears_for_the_next() {
        let now = Instant::now();
        let activity = HostActivity::new();
        let control = WarmupControl::new();
        let guard = control.try_acquire(&activity, Duration::ZERO, now).unwrap();
        assert!(!guard.aborted());
        control.abort_for_user_launch();
        assert!(guard.aborted());
        drop(guard);
        // The abort does not leak into the next attempt.
        let next = control.try_acquire(&activity, Duration::ZERO, now).unwrap();
        assert!(!next.aborted());
    }

    #[test]
    fn the_abort_cause_distinguishes_a_launch_from_a_lost_connection() {
        let now = Instant::now();
        let activity = HostActivity::new();
        let control = WarmupControl::new();
        let guard = control.try_acquire(&activity, Duration::ZERO, now).unwrap();
        assert_eq!(control.abort_cause(), AbortCause::None);

        control.abort_for_connection_lost();
        assert_eq!(control.abort_cause(), AbortCause::ConnectionLost);
        assert_eq!(
            control.abort_cause().reason(),
            "aborted: the control-plane connection was lost"
        );
        drop(guard);

        let next = control.try_acquire(&activity, Duration::ZERO, now).unwrap();
        assert_eq!(control.abort_cause(), AbortCause::None);
        control.abort_for_user_launch();
        assert_eq!(control.abort_cause(), AbortCause::UserLaunch);
        assert_eq!(
            control.abort_cause().reason(),
            "aborted for a user session launch"
        );
        drop(next);
    }

    #[test]
    fn dropping_the_guard_releases_the_capacity_reservation() {
        let now = Instant::now();
        let activity = HostActivity::new();
        let control = WarmupControl::new();
        let guard = control.try_acquire(&activity, Duration::ZERO, now).unwrap();
        control.set_reserved(true);
        assert!(control.reserved());
        drop(guard);
        assert!(!control.reserved());
    }
}
