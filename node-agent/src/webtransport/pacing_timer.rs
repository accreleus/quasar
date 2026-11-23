//! The PyroWave pacing arm's wake (#1633). tokio's timer rounds a deadline up to a whole
//! millisecond and wakes on a tick, so a 0.35 ms gap between slices took 1 to 2 ms and a 4K120
//! frame's slices spread over its whole interval. A `timerfd` on CLOCK_MONOTONIC, polled through
//! tokio's I/O driver, wakes tens of microseconds after its deadline.

use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::time::Duration;

use tokio::io::unix::AsyncFd;
use tokio::io::Interest;
use tokio::time::Instant;

struct Fd {
    fd: AsyncFd<OwnedFd>,
    /// An `Instant` and CLOCK_MONOTONIC read together: `Instant` is that clock on Linux, so a
    /// deadline maps to an absolute expiry with no clock read per arm.
    epoch: (Instant, Duration),
    /// The deadline the timer is armed for: the send loop polls this arm every turn.
    armed: Option<Instant>,
}

/// A sleep for the pacing arm only: a `timerfd`, or tokio's timer where none is available.
pub(super) struct PacingTimer(Option<Fd>);

impl PacingTimer {
    pub(super) fn new() -> Self {
        match open() {
            Ok(fd) => Self(Some(fd)),
            Err(error) => {
                warn_coarse(&error);
                Self::coarse()
            }
        }
    }

    /// tokio's timer, which a paused test clock drives.
    pub(super) fn coarse() -> Self {
        Self(None)
    }

    pub(super) async fn sleep_until(&mut self, at: Instant) {
        let Some(timer) = self.0.as_mut() else {
            return tokio::time::sleep_until(at).await;
        };
        if at <= Instant::now() {
            return;
        }
        if let Err(error) = timer.wait(at).await {
            self.fall_back(error, at).await;
        }
    }

    /// A timerfd that fails once is dropped for good: a hot loop on a broken fd would spin.
    async fn fall_back(&mut self, error: io::Error, at: Instant) {
        warn_coarse(&error);
        self.0 = None;
        tokio::time::sleep_until(at).await;
    }
}

impl Fd {
    async fn wait(&mut self, at: Instant) -> io::Result<()> {
        if self.armed != Some(at) {
            self.arm(at)?;
        }
        loop {
            let mut ready = self.fd.readable().await?;
            if let Ok(fired) = ready.try_io(|fd| expirations(fd.get_ref())) {
                self.armed = None;
                return fired;
            }
        }
    }

    fn arm(&mut self, at: Instant) -> io::Result<()> {
        let expiry = self.epoch.1 + at.saturating_duration_since(self.epoch.0);
        let spec = libc::itimerspec {
            it_interval: libc::timespec {
                tv_sec: 0,
                tv_nsec: 0,
            },
            it_value: libc::timespec {
                tv_sec: expiry.as_secs() as libc::time_t,
                tv_nsec: expiry.subsec_nanos() as libc::c_long,
            },
        };
        // SAFETY: a valid fd and itimerspec; the old value is not asked for.
        let rc = unsafe {
            libc::timerfd_settime(
                self.fd.get_ref().as_raw_fd(),
                libc::TFD_TIMER_ABSTIME,
                &spec,
                std::ptr::null_mut(),
            )
        };
        if rc != 0 {
            return Err(io::Error::last_os_error());
        }
        // Re-arming also clears an expiry left unread by a sleep the send loop dropped.
        self.armed = Some(at);
        Ok(())
    }
}

fn warn_coarse(error: &io::Error) {
    tracing::warn!(
        token = "pyrowave-pacing-timer-unavailable",
        %error,
        "no timerfd for PyroWave slice pacing: slices are spaced by tokio's 1 ms timer"
    );
}

fn open() -> io::Result<Fd> {
    // SAFETY: plain syscalls; the fd is owned from here on.
    let raw = unsafe {
        libc::timerfd_create(
            libc::CLOCK_MONOTONIC,
            libc::TFD_NONBLOCK | libc::TFD_CLOEXEC,
        )
    };
    if raw < 0 {
        return Err(io::Error::last_os_error());
    }
    let fd = unsafe { OwnedFd::from_raw_fd(raw) };
    let at = Instant::now();
    let mut now = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    // SAFETY: a valid out pointer.
    if unsafe { libc::clock_gettime(libc::CLOCK_MONOTONIC, &mut now) } != 0 {
        return Err(io::Error::last_os_error());
    }
    // Read after `at`, so a mapped expiry is never early.
    let mono = Duration::new(now.tv_sec as u64, now.tv_nsec as u32);
    Ok(Fd {
        // SAFETY: `OwnedFd` keeps the fd open and returns the same one until the `AsyncFd` drops.
        fd: unsafe { AsyncFd::register_with_interest(fd, Interest::READABLE) }?,
        epoch: (at, mono),
        armed: None,
    })
}

/// Reads the timer's expiry count: `WouldBlock` until it has fired.
fn expirations(fd: &OwnedFd) -> io::Result<()> {
    let mut count = 0u64;
    // SAFETY: reads 8 bytes into a u64.
    let n = unsafe { libc::read(fd.as_raw_fd(), (&mut count as *mut u64).cast(), 8) };
    if n == 8 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Real clock: 200 waits of 300 µs, the lateness of each wake. tokio's timer wakes on the
    /// next millisecond tick (p50 ~0.7 ms late on this wait); the timerfd must do far better.
    #[tokio::test]
    async fn a_sub_millisecond_wait_wakes_close_to_its_deadline() {
        let mut timer = PacingTimer::new();
        assert!(timer.0.is_some(), "no timerfd");
        let mut late: Vec<i64> = Vec::new();
        for _ in 0..200 {
            let at = Instant::now() + Duration::from_micros(300);
            timer.sleep_until(at).await;
            let now = Instant::now();
            late.push(if now >= at {
                (now - at).as_micros() as i64
            } else {
                -((at - now).as_micros() as i64)
            });
        }
        late.sort_unstable();
        let (p50, p95) = (late[100], late[190]);
        eprintln!(
            "timerfd wake late by: min {} p50 {p50} p95 {p95} max {} us",
            late[0], late[199]
        );
        assert!(late[0] >= 0, "woke early: {late:?}");
        assert!(p50 < 200 && p95 < 500, "p50 {p50} us, p95 {p95} us");
    }

    /// A sleep the send loop drops mid-wait leaves the timer armed; the next one still waits
    /// out its own deadline.
    #[tokio::test]
    async fn a_dropped_sleep_does_not_wake_the_next_one_early() {
        let mut timer = PacingTimer::new();
        let first = Instant::now() + Duration::from_micros(200);
        let _ = tokio::time::timeout(Duration::ZERO, timer.sleep_until(first)).await;
        std::thread::sleep(Duration::from_millis(1));
        let next = Instant::now() + Duration::from_millis(2);
        timer.sleep_until(next).await;
        assert!(Instant::now() >= next);
    }
}
