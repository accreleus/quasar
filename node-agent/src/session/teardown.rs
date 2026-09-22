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
//! The runtime client is single-flight-bounded (`max_in_flight`). A busy or
//! cancelled call never asked the engine, so it must not be recorded as an
//! unconfirmed stop: that latch abandons the udev export and, on the sidecar,
//! suppresses the only retry. Routine audio recovery will not stop a sidecar
//! whose journal is still `Running`.

use std::time::Duration;

use crate::runtime::ErrorKind;

/// How many times a session end retries a stop the runtime client refused
/// because it was busy. Each pause is [`STOP_PAUSE`], so the budget is 10 s:
/// long enough for an in-flight engine call to return its slot, short of the
/// app-container stop timeout itself.
pub const STOP_ATTEMPTS: u32 = 40;

pub const STOP_PAUSE: Duration = Duration::from_millis(250);

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
    /// The runtime client did not accept the call. The engine was not asked.
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

/// Classify a runtime error from an app-container or sidecar stop.
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
/// so the bind mount is gone. A busy client changes nothing.
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

/// The blocked flag after `report`. A confirmed stop clears it. A busy client
/// leaves it alone. Only an unconfirmed stop sets it.
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
/// A busy client (`Keep`) is not an unconfirmed stop. On the last chance, if
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

/// Run the udev half of [`action_this_chance`]. Both session-end owners call
/// this so the match lives in one place.
pub fn on_udev(action: UdevAction, retire: impl FnOnce(), abandon: impl FnOnce()) {
    match action {
        UdevAction::Retire => retire(),
        UdevAction::Abandon => abandon(),
        UdevAction::Keep => {}
    }
}

/// Retry while the attempt is [`StopAttempt::Retryable`]. A confirmed, absent
/// or unconfirmed result returns immediately: an unconfirmed stop already spent
/// an engine call and must not be spun.
pub fn retry_retryable(
    mut attempt: impl FnMut() -> StopAttempt,
    mut pause: impl FnMut(),
    limit: u32,
) -> StopAttempt {
    let limit = limit.max(1);
    let mut last = StopAttempt::Absent;
    for i in 0..limit {
        last = attempt();
        match last {
            StopAttempt::Confirmed | StopAttempt::Absent | StopAttempt::Unconfirmed => {
                return last;
            }
            StopAttempt::Retryable => {
                if i + 1 == limit {
                    return last;
                }
                pause();
            }
        }
    }
    last
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::{ErrorKind, RuntimeError};

    #[test]
    fn a_busy_stop_keeps_udev_armed_and_does_not_latch_pulse() {
        let error = anyhow::Error::from(RuntimeError::from(ErrorKind::Busy));
        let report = classify_stop(error_kind(&error));
        assert_eq!(report, StopAttempt::Retryable);
        assert_eq!(udev_action(report, false, true), UdevAction::Keep);
        assert!(!blocked_after(report, false));
        assert!(!pulse_stop_latches(ErrorKind::Busy));
        assert!(!pulse_stop_latches(ErrorKind::Cancelled));
    }

    #[test]
    fn an_unknown_stop_outcome_latches_pulse_and_disarms_udev() {
        let error = anyhow::Error::from(RuntimeError::from(ErrorKind::UnknownOutcome));
        let report = classify_stop(error_kind(&error));
        assert_eq!(report, StopAttempt::Unconfirmed);
        assert_eq!(udev_action(report, false, true), UdevAction::Abandon);
        assert!(blocked_after(report, false));
        assert!(pulse_stop_latches(ErrorKind::UnknownOutcome));
    }

    #[test]
    fn a_later_confirmed_stop_retires_udev_and_clears_an_earlier_block() {
        // The bug: the first busy (or unconfirmed) attempt set the sticky flag,
        // a later stop proved the container gone, and the export was abandoned
        // anyway. A confirmed stop retires even when the flag and the mount
        // bit are still set from that earlier attempt.
        assert_eq!(
            udev_action(StopAttempt::Confirmed, true, true),
            UdevAction::Retire
        );
        assert!(!blocked_after(StopAttempt::Confirmed, true));
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
        );
        assert_eq!(got, StopAttempt::Unconfirmed);
        assert_eq!(calls, 1);
        assert_eq!(pauses, 0);
    }

    #[test]
    fn a_nonfinal_unconfirmed_stop_keeps_the_export_for_a_later_confirm() {
        let action = udev_action(StopAttempt::Unconfirmed, false, true);
        assert_eq!(action_this_chance(action, false, true), UdevAction::Keep);
        assert_eq!(action_this_chance(action, true, true), UdevAction::Abandon);
    }

    #[test]
    fn a_final_busy_stop_abandons_a_live_mount_without_recording_unconfirmed() {
        // The engine was never asked, so the blocked flag stays clear. Retiring
        // is still unsafe: the container may hold the bind. The last chance
        // disarms Drop and leaves the pair for the boot sweep.
        let action = udev_action(StopAttempt::Retryable, false, true);
        assert_eq!(action, UdevAction::Keep);
        assert_eq!(action_this_chance(action, false, true), UdevAction::Keep);
        assert_eq!(action_this_chance(action, true, true), UdevAction::Abandon);
        assert!(!blocked_after(StopAttempt::Retryable, false));
    }

    #[test]
    fn no_container_and_no_mount_retires_the_export() {
        assert_eq!(
            udev_action(StopAttempt::Absent, false, false),
            UdevAction::Retire
        );
    }

    #[test]
    fn idle_reap_and_session_stop_share_the_confirmed_stop_disposition() {
        // Who ended the session is not an input. The same confirmed stop
        // retires the export for an idle reap and for an intentional stop.
        let disposition = udev_action(StopAttempt::Confirmed, true, true);
        assert_eq!(disposition, UdevAction::Retire);
    }
}
