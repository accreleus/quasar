//! How a session end releases the app container, the PulseAudio sidecar and the
//! fake-udev export. One decision for every terminal path (intentional stop,
//! idle reap, peer disconnect, app exit, encode failure, and a live agent's
//! grace-window stop — the same path a `host_lost` row takes when this process
//! is still up).
//!
//! A killed agent never reaches this code. That case is the boot sweep
//! (`udev_export::retire_all_owned` and `retire_audio_sidecars`), which runs
//! only where "ours" implies "dead".
//!
//! # Two different `Busy`s
//!
//! The runtime client is single-flight-bounded (`max_in_flight`). Its
//! *admission* `Busy` (`RuntimeClient::submit_owned`, a refused
//! `try_acquire_owned`) is returned synchronously and never asked the engine,
//! so it costs nothing and writes nothing.
//!
//! `docker::application::cleanup` and `docker::helpers::cleanup` also return
//! `Busy`, and that one is NOT free: the journal already holds `Stopping`, a
//! `docker stop` has already been waited out (`QUASAR_APP_STOP_TIMEOUT_SECS`,
//! default 10 s) and the container was still `Running` on the inspect that
//! followed. Retrying it is still correct — stop is idempotent and this exact
//! durable identity is what must be re-driven — but a retry of *that* costs a
//! full stop timeout plus two inspects.
//!
//! Both must be retried and neither may be recorded as an unconfirmed stop:
//! that latch abandons the udev export and, on the sidecar, suppresses the only
//! retry (routine audio recovery will not stop a sidecar whose journal is still
//! `Running`). Because the second kind is expensive, the retry loop is bounded
//! by wall clock — [`RetryBudget`] — and not by an attempt count alone.
//!
//! # What one session end may spend
//!
//! [`STOP_RETRY_BUDGET`] is the whole allowance for *retrying*, shared by every
//! release call in one session end: `AppSource::teardown`, `AppSource::drop`,
//! `SessionResources::drop` and the sidecar's own `Drop`. Once it is spent every
//! later call makes exactly one attempt and returns. So the worst case for a
//! session whose runtime client is persistently busy is [`STOP_RETRY_BUDGET`] of
//! waiting in total, plus at most one in-flight engine call per release call
//! (each bounded by the runtime client's own `RuntimeConfig::deadline`) — not a
//! fresh budget per call.
//!
//! The budget is armed at the first pause, so the first attempt of the session
//! end is always made in full, and so a session that never has to retry never
//! starts its clock.
//!
//! A swap is not a session end: `AppSource::stop_app_container` takes its own
//! [`STOP_RETRY_BUDGET`] per swap, because a swap an hour before the session
//! ends must not spend the allowance the session end needs.
//!
//! # Audio ordering
//!
//! The sidecar is stopped from inside the release, which is before the runner's
//! `audio_pipeline.finish()`: `pulsesrc` is still PLAYING when its server dies.
//! That cannot stall the later `finish()`. `AudioPipelineGuard::finish` is a
//! synchronous downward `set_state(Null)`; `GstBaseSrc` calls the element's
//! `unlock` before joining its streaming task, and `pulsesrc`'s unlock signals
//! its own `pa_threaded_mainloop` rather than waiting on the server, while a
//! dead socket makes the context fail and wakes the same mainloop. Neither the
//! ring-buffer release nor `pa_context_disconnect` waits for a server reply.
//! The element posts an error and the transition completes.
//!
//! That is a reading of the element's contract, not a measurement: the one thing
//! to watch for on a host is a session end that hangs between the
//! `audio-pulse-cleanup-*` log and the encode pipeline reaching NULL. If that is
//! ever seen, the guard is to finish the audio pipeline before the release
//! rather than after it — `AudioPipelineGuard::finish` is idempotent, so the
//! runner can call it earlier on the terminal paths without changing anything
//! else.

use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use crate::runtime::ErrorKind;

/// Wall-clock allowance one session end may spend *retrying* a stop the runtime
/// refused, shared across every release call (see the module doc). Sized so a
/// single in-flight `docker stop` at the default `QUASAR_APP_STOP_TIMEOUT_SECS`
/// (10 s) can finish and free the admission slot, with a little slack.
pub const STOP_RETRY_BUDGET: Duration = Duration::from_secs(12);

/// The demo host (`session::host::SessionHost`) drops inside `handle_peer`, on a
/// Tokio worker, so its release gets a much smaller allowance. It already blocks
/// that worker on a full `stop_application` + `cleanup_application`, so this adds
/// seconds, not an order of magnitude, and it is the whole session end's
/// allowance rather than a per-call one.
pub const DEMO_STOP_RETRY_BUDGET: Duration = Duration::from_secs(2);

/// Upper bound on iterations of one retry loop. Belt-and-braces next to
/// [`RetryBudget`]: an admission `Busy` returns synchronously, so without this
/// the loop would spin at [`STOP_PAUSE`] resolution for the whole budget.
/// 40 × [`STOP_PAUSE`] is 10 s, inside [`STOP_RETRY_BUDGET`].
pub const STOP_ATTEMPTS: u32 = 40;

pub const STOP_PAUSE: Duration = Duration::from_millis(250);

/// The shared wall-clock allowance described in the module doc. Cloning shares
/// it; `AppSource`, `SessionResources` and the session's [`Sidecar`] all hold
/// clones of one budget, so their retries draw down the same pool.
///
/// The clock is injectable so budget behaviour is testable without sleeping.
#[derive(Clone)]
pub struct RetryBudget {
    inner: Arc<Mutex<BudgetInner>>,
}

struct BudgetInner {
    total: Duration,
    tick: Tick,
    /// The clock reading when the budget was armed (its first [`RetryBudget::live`]
    /// call, i.e. after the session end's first attempt). `None` until then.
    armed: Option<Duration>,
}

enum Tick {
    Real(Instant),
    #[cfg(test)]
    Fake(Arc<Mutex<Duration>>),
}

impl BudgetInner {
    fn read(&self) -> Duration {
        match &self.tick {
            Tick::Real(origin) => origin.elapsed(),
            #[cfg(test)]
            Tick::Fake(now) => *now
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner),
        }
    }
}

impl RetryBudget {
    pub fn new(total: Duration) -> Self {
        Self::with_tick(total, Tick::Real(Instant::now()))
    }

    fn with_tick(total: Duration, tick: Tick) -> Self {
        Self {
            inner: Arc::new(Mutex::new(BudgetInner {
                total,
                tick,
                armed: None,
            })),
        }
    }

    /// A budget with nothing left: every loop makes exactly one attempt and
    /// returns. Used by call sites that must not wait at all.
    pub fn spent() -> Self {
        Self::new(Duration::ZERO)
    }

    /// Whether there is time left to pause before another attempt. Arms the
    /// budget on its first call, so the session end's first attempt is never
    /// charged to it.
    pub fn live(&self) -> bool {
        let mut inner = self
            .inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let now = inner.read();
        let armed = *inner.armed.get_or_insert(now);
        now.saturating_sub(armed) < inner.total
    }

    #[cfg(test)]
    pub fn fake(total: Duration, now: Arc<Mutex<Duration>>) -> Self {
        Self::with_tick(total, Tick::Fake(now))
    }
}

/// Outcome of one attempt to stop the app container.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StopAttempt {
    /// No container was running.
    Absent,
    /// Removal was proven.
    Confirmed,
    /// The engine was asked and removal was not proven. The bind mount may
    /// still be live.
    Unconfirmed,
    /// The call was refused, or the engine was asked and answered "still
    /// running". Either way removal was not attempted to completion and the
    /// same durable identity may be re-driven.
    Retryable,
}

/// What to do with `udev-<sid>` and `udev-<sid>.owner`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UdevAction {
    /// Remove the directory, then the marker.
    Retire,
    /// Disarm the in-process retire. The boot sweep owns the pair.
    Abandon,
    /// Leave the export armed for a later confirmed stop.
    Keep,
}

/// The session's fake-udev export, as the release path sees it. A trait so the
/// release is testable without uinput: the only implementation is
/// [`crate::session::virtual_input::VirtualDevices`].
pub trait UdevExport {
    fn retire(&self);
    fn abandon(&self);
}

/// The session's audio sidecar, as the release path sees it. A trait for the
/// same reason as [`UdevExport`]: the only implementation is
/// [`crate::session::audio::PulseSidecar`], which needs a live runtime client.
pub trait Sidecar: Send {
    /// The `unix:…` URI `PULSE_SERVER` clients and `pulsesrc` connect to.
    fn server_uri(&self) -> String;
    /// The socket directory bind-mounted into the app container.
    fn socket_dir(&self) -> std::path::PathBuf;
    /// Join the session end's shared allowance. Called once, when the session
    /// takes ownership of the sidecar.
    fn adopt_budget(&mut self, budget: RetryBudget);
    /// Release it (idempotent). Draws on the adopted [`RetryBudget`].
    fn stop(&mut self);
}

/// The two sticky bits a session end carries between release calls.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ReleaseState {
    /// A previous stop was unconfirmed, so the export is not safe to retire.
    pub blocked: bool,
    /// An app container was launched and has not been proven gone.
    pub mount_live: bool,
}

impl ReleaseState {
    pub const IDLE: Self = Self {
        blocked: false,
        mount_live: false,
    };
}

/// Classify a runtime error from an app-container or sidecar stop.
///
/// [`ErrorKind::Cancelled`] is unreachable on these paths — every stop rides a
/// `detached` operation, which ignores the cancel watch, and none of these call
/// sites calls `Operation::cancel`. It is matched defensively: a cancelled call
/// did not settle anything either, and classifying it as unconfirmed is exactly
/// the #314 bug.
pub fn classify_stop(kind: Option<ErrorKind>) -> StopAttempt {
    match kind {
        Some(ErrorKind::Busy | ErrorKind::Cancelled) => StopAttempt::Retryable,
        _ => StopAttempt::Unconfirmed,
    }
}

/// The runtime kind has to survive `anyhow::Error::from`. A display-only wrap
/// looks like an unconfirmed stop, and that is what latches the sidecar and
/// abandons the udev export.
pub fn error_kind(error: &anyhow::Error) -> Option<ErrorKind> {
    error.chain().find_map(|cause| {
        cause
            .downcast_ref::<crate::runtime::RuntimeError>()
            .map(|rt| rt.kind)
    })
}

/// Whether this pulse-stop error recorded durable intent. A busy or cancelled
/// call did not, so `Drop` and the retry loop must be allowed to try again.
pub fn pulse_stop_latches(kind: ErrorKind) -> bool {
    !matches!(kind, ErrorKind::Busy | ErrorKind::Cancelled)
}

/// Udev disposition for one settled stop attempt.
///
/// `already_blocked` is the sticky "a previous stop was unconfirmed" flag.
/// `mount_live` means an app container was launched and has not been proven
/// gone. A confirmed stop retires even if both are set: removal was proven,
/// so the bind mount is gone. A refused stop changes nothing.
pub fn udev_action(report: StopAttempt, already_blocked: bool, mount_live: bool) -> UdevAction {
    match report {
        StopAttempt::Confirmed => UdevAction::Retire,
        StopAttempt::Absent => {
            if already_blocked || mount_live {
                UdevAction::Abandon
            } else {
                UdevAction::Retire
            }
        }
        StopAttempt::Unconfirmed => UdevAction::Abandon,
        StopAttempt::Retryable => {
            if already_blocked {
                UdevAction::Abandon
            } else {
                UdevAction::Keep
            }
        }
    }
}

/// The blocked flag after `report`. A confirmed stop clears it. A refused stop
/// leaves it alone. Only an unconfirmed stop sets it.
///
/// Clearing a flag an EARLIER generation set is safe only because a swap cannot
/// outlive an unproven stop: `runner::perform_swap` step 2 treats an unconfirmed
/// stop of the outgoing generation as fatal, so a live later generation implies
/// every earlier generation's container was proven gone. Without that rule this
/// would clear a block belonging to a container this process never reaped.
pub fn blocked_after(report: StopAttempt, already_blocked: bool) -> bool {
    match report {
        StopAttempt::Confirmed => false,
        StopAttempt::Unconfirmed => true,
        StopAttempt::Absent | StopAttempt::Retryable => already_blocked,
    }
}

/// An unconfirmed stop's abandon waits for the last chance, so a later
/// confirmed stop can still retire the export.
///
/// A refused stop (`Keep`) is not an unconfirmed stop. On the last chance, if
/// the mount may still be live, `Keep` becomes an abandon: the virtual-devices
/// `Drop` would otherwise retire the export under a container this process
/// never proved gone. That abandon is the boot sweep's, not a claim the engine
/// was asked.
pub fn action_this_chance(action: UdevAction, final_chance: bool, mount_live: bool) -> UdevAction {
    match (action, final_chance, mount_live) {
        (UdevAction::Abandon, false, _) => UdevAction::Keep,
        (UdevAction::Keep, true, true) => UdevAction::Abandon,
        _ => action,
    }
}

/// Apply one settled stop outcome to the session's udev export and return the
/// state to store back. The whole udev half of a release lives here, so the
/// production session (`source::SharedSessionRuntime::apply`) and the demo host
/// (`host::SessionHost::release`) cannot drift: they differ only in how they
/// hold `state` (atomics behind an `Arc` vs. plain fields), which is why this
/// takes and returns it by value instead of owning it.
pub fn settle_udev(
    session: &str,
    report: StopAttempt,
    state: ReleaseState,
    final_chance: bool,
    udev: Option<&dyn UdevExport>,
) -> ReleaseState {
    let action = action_this_chance(
        udev_action(report, state.blocked, state.mount_live),
        final_chance,
        state.mount_live,
    );
    match action {
        UdevAction::Retire => {
            if let Some(export) = udev {
                export.retire();
            }
        }
        UdevAction::Abandon => {
            tracing::warn!(
                token = "udev-export-retire-skipped",
                session = %session,
                "an app container stop was not proven — leaving the udev export \
                 dir for the boot sweep"
            );
            if let Some(export) = udev {
                export.abandon();
            }
        }
        UdevAction::Keep => {}
    }
    ReleaseState {
        blocked: blocked_after(report, state.blocked),
        mount_live: state.mount_live && !matches!(report, StopAttempt::Confirmed),
    }
}

/// Retry while the attempt is [`StopAttempt::Retryable`]. A confirmed, absent
/// or unconfirmed result returns immediately: an unconfirmed stop already spent
/// an engine call and must not be spun.
///
/// Two bounds, both needed (see the module doc): `limit` caps iterations when
/// attempts are free (admission `Busy`), and `budget_live` caps wall clock when
/// they are not (a post-journal `Busy` that already waited out a stop timeout).
/// `budget_live` is consulted only between attempts, so one attempt is always
/// made.
pub fn retry_retryable(
    mut attempt: impl FnMut() -> StopAttempt,
    mut pause: impl FnMut(),
    limit: u32,
    mut budget_live: impl FnMut() -> bool,
) -> StopAttempt {
    let limit = limit.max(1);
    let mut last = StopAttempt::Absent;
    for i in 0..limit {
        last = attempt();
        if !matches!(last, StopAttempt::Retryable) {
            return last;
        }
        if i + 1 == limit || !budget_live() {
            return last;
        }
        pause();
    }
    last
}

/// [`retry_retryable`] wired to the session's shared allowance and the real
/// clock. Every production retry goes through here.
pub fn retry_with_budget(
    attempt: impl FnMut() -> StopAttempt,
    budget: &RetryBudget,
) -> StopAttempt {
    retry_retryable(
        attempt,
        || std::thread::sleep(STOP_PAUSE),
        STOP_ATTEMPTS,
        || budget.live(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::{ErrorKind, RuntimeError};
    use std::cell::RefCell;

    /// Records what the release did to the export, so a decision test asserts on
    /// the effect rather than restating the match arm it is testing.
    #[derive(Default)]
    struct RecordingExport {
        log: RefCell<Vec<&'static str>>,
    }
    impl UdevExport for RecordingExport {
        fn retire(&self) {
            self.log.borrow_mut().push("retire");
        }
        fn abandon(&self) {
            self.log.borrow_mut().push("abandon");
        }
    }
    impl RecordingExport {
        fn log(&self) -> Vec<&'static str> {
            self.log.borrow().clone()
        }
    }

    fn settle(
        report: StopAttempt,
        state: ReleaseState,
        final_chance: bool,
    ) -> (ReleaseState, Vec<&'static str>) {
        let export = RecordingExport::default();
        let next = settle_udev("s1", report, state, final_chance, Some(&export));
        (next, export.log())
    }

    const LAUNCHED: ReleaseState = ReleaseState {
        blocked: false,
        mount_live: true,
    };

    #[test]
    fn a_busy_stop_keeps_udev_armed_and_does_not_latch_pulse() {
        let error = anyhow::Error::from(RuntimeError::from(ErrorKind::Busy));
        let report = classify_stop(error_kind(&error));
        assert_eq!(report, StopAttempt::Retryable);
        let (next, log) = settle(report, LAUNCHED, false);
        assert_eq!(log, Vec::<&str>::new(), "a refused stop touches nothing");
        assert_eq!(next, LAUNCHED, "and leaves both sticky bits alone");
        assert!(!pulse_stop_latches(ErrorKind::Busy));
        assert!(!pulse_stop_latches(ErrorKind::Cancelled));
    }

    #[test]
    fn an_unknown_stop_outcome_latches_pulse_and_disarms_udev() {
        let error = anyhow::Error::from(RuntimeError::from(ErrorKind::UnknownOutcome));
        let report = classify_stop(error_kind(&error));
        assert_eq!(report, StopAttempt::Unconfirmed);
        let (next, log) = settle(report, LAUNCHED, true);
        assert_eq!(log, vec!["abandon"]);
        assert!(next.blocked);
        assert!(pulse_stop_latches(ErrorKind::UnknownOutcome));
    }

    #[test]
    fn a_later_confirmed_stop_retires_udev_and_clears_an_earlier_block() {
        // The bug: the first busy (or unconfirmed) attempt set the sticky flag,
        // a later stop proved the container gone, and the export was abandoned
        // anyway. A confirmed stop retires even when the flag and the mount
        // bit are still set from that earlier attempt.
        let blocked = ReleaseState {
            blocked: true,
            mount_live: true,
        };
        let (next, log) = settle(StopAttempt::Confirmed, blocked, false);
        assert_eq!(log, vec!["retire"]);
        assert_eq!(
            next,
            ReleaseState::IDLE,
            "the block and the mount both clear"
        );
    }

    #[test]
    fn an_absent_container_with_no_live_mount_retires_the_export() {
        // The session never launched an app (an early failure, or an appless
        // generation): nothing can hold the bind, so the export is ours to remove
        // on the spot rather than leaving litter for the boot sweep.
        let (next, log) = settle(StopAttempt::Absent, ReleaseState::IDLE, false);
        assert_eq!(log, vec!["retire"]);
        assert_eq!(next, ReleaseState::IDLE);
        // But an absent handle over a mount whose container was never proven gone
        // is not evidence of anything, and must not retire.
        let (_, log) = settle(StopAttempt::Absent, LAUNCHED, true);
        assert_eq!(log, vec!["abandon"]);
    }

    #[test]
    fn a_nonfinal_unconfirmed_stop_keeps_the_export_for_a_later_confirm() {
        let (next, log) = settle(StopAttempt::Unconfirmed, LAUNCHED, false);
        assert_eq!(log, Vec::<&str>::new(), "abandon waits for the last chance");
        assert!(next.blocked);
        let (_, log) = settle(StopAttempt::Confirmed, next, false);
        assert_eq!(log, vec!["retire"], "and a later confirm still retires");
    }

    #[test]
    fn a_final_busy_stop_abandons_a_live_mount_without_recording_unconfirmed() {
        // The engine was never asked, so the blocked flag stays clear. Retiring
        // is still unsafe: the container may hold the bind. The last chance
        // disarms Drop and leaves the pair for the boot sweep.
        let (next, log) = settle(StopAttempt::Retryable, LAUNCHED, true);
        assert_eq!(log, vec!["abandon"]);
        assert!(
            !next.blocked,
            "abandoning is not a claim the engine answered"
        );
    }

    #[test]
    fn a_session_with_no_devices_still_settles_its_state() {
        // `use_test_src` sessions have no uinput devices and therefore no export.
        let next = settle_udev("s1", StopAttempt::Confirmed, LAUNCHED, false, None);
        assert_eq!(next, ReleaseState::IDLE);
    }

    #[test]
    fn retry_keeps_going_through_a_busy_client_until_the_stop_is_confirmed() {
        let script = [
            StopAttempt::Retryable,
            StopAttempt::Retryable,
            StopAttempt::Confirmed,
        ];
        let mut pauses = 0;
        let mut n = 0;
        let got = retry_retryable(
            || {
                let report = script[n];
                n += 1;
                report
            },
            || pauses += 1,
            5,
            || true,
        );
        assert_eq!(got, StopAttempt::Confirmed);
        assert_eq!(pauses, 2);
        assert_eq!(n, 3);
    }

    #[test]
    fn an_unconfirmed_stop_is_not_retried_as_if_the_client_were_busy() {
        let mut pauses = 0;
        let mut calls = 0;
        let got = retry_retryable(
            || {
                calls += 1;
                StopAttempt::Unconfirmed
            },
            || pauses += 1,
            40,
            || true,
        );
        assert_eq!(got, StopAttempt::Unconfirmed);
        assert_eq!(calls, 1);
        assert_eq!(pauses, 0);
    }

    #[test]
    fn a_spent_budget_still_makes_one_attempt_and_never_pauses() {
        let budget = RetryBudget::spent();
        let mut calls = 0;
        let mut pauses = 0;
        let got = retry_retryable(
            || {
                calls += 1;
                StopAttempt::Retryable
            },
            || pauses += 1,
            STOP_ATTEMPTS,
            || budget.live(),
        );
        assert_eq!(got, StopAttempt::Retryable);
        assert_eq!(calls, 1, "the engine is still asked once");
        assert_eq!(pauses, 0, "but nothing waits on a spent budget");
    }

    #[test]
    fn an_expensive_busy_is_bounded_by_wall_clock_not_by_the_attempt_count() {
        // `application::cleanup` answers Busy only AFTER waiting out a whole
        // `docker stop`, so 40 attempts would be minutes, not the 10 s the
        // attempt count suggests. The clock is what has to stop it.
        let now = Arc::new(Mutex::new(Duration::ZERO));
        let budget = RetryBudget::fake(STOP_RETRY_BUDGET, now.clone());
        let stop_timeout = Duration::from_secs(10);
        let mut calls = 0;
        let got = retry_retryable(
            || {
                calls += 1;
                *now.lock().unwrap() += stop_timeout;
                StopAttempt::Retryable
            },
            || *now.lock().unwrap() += STOP_PAUSE,
            STOP_ATTEMPTS,
            || budget.live(),
        );
        assert_eq!(got, StopAttempt::Retryable);
        assert!(
            calls < STOP_ATTEMPTS,
            "the attempt cap must not be what ends this: {calls}"
        );
        let spent = *now.lock().unwrap();
        assert!(
            spent <= stop_timeout + STOP_RETRY_BUDGET + stop_timeout,
            "one session end spent {spent:?}"
        );
    }

    #[test]
    fn one_budget_is_drawn_down_by_every_release_call_that_shares_it() {
        // teardown, then AppSource::drop, then SessionResources::drop. Each gets
        // its own loop but they must not each get their own 12 s.
        let now = Arc::new(Mutex::new(Duration::ZERO));
        let budget = RetryBudget::fake(STOP_RETRY_BUDGET, now.clone());
        let mut total = 0;
        for _ in 0..3 {
            let handle = budget.clone();
            retry_retryable(
                || {
                    total += 1;
                    StopAttempt::Retryable
                },
                || *now.lock().unwrap() += STOP_PAUSE,
                STOP_ATTEMPTS,
                || handle.live(),
            );
        }
        let spent = *now.lock().unwrap();
        assert!(
            spent < STOP_RETRY_BUDGET + STOP_PAUSE,
            "three release calls waited {spent:?}"
        );
        assert!(
            total > STOP_ATTEMPTS,
            "an admission Busy is free, so the attempt cap should bind first: {total}"
        );
        assert!(
            total < 3 * STOP_ATTEMPTS,
            "but the third call must find the budget spent: {total}"
        );
    }
}
