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

    /// Wakes within tens of microseconds; otherwise a wake may come up to a tick late.
    pub(super) fn precise(&self) -> bool {
        self.0.is_some()
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
        // SAFETY: the fd is owned here, never duplicated or handed out, and closed only when the
        // `AsyncFd` drops; only this module's own syscalls touch it.
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

    /// Real clock: 200 waits of 300 µs on the timerfd, each beside one on tokio's timer, which
    /// wakes on the next millisecond tick (p50 ~0.75 ms late on an idle machine; the timerfd's
    /// ~3 µs). The two are interleaved and compared, not held to a fixed bound: on a loaded CI
    /// runner both are late by the wait for a CPU (timerfd p50 353 µs seen), but the timerfd
    /// still wakes before tokio's tick.
    #[tokio::test]
    async fn a_sub_millisecond_wait_wakes_close_to_its_deadline() {
        let mut timer = PacingTimer::new();
        assert!(timer.0.is_some(), "no timerfd");
        let lateness = |at: Instant| {
            let now = Instant::now();
            if now >= at {
                (now - at).as_micros() as i64
            } else {
                -((at - now).as_micros() as i64)
            }
        };
        let (mut fd, mut tick): (Vec<i64>, Vec<i64>) = (Vec::new(), Vec::new());
        for _ in 0..200 {
            let at = Instant::now() + Duration::from_micros(300);
            timer.sleep_until(at).await;
            fd.push(lateness(at));
            let at = Instant::now() + Duration::from_micros(300);
            tokio::time::sleep_until(at).await;
            tick.push(lateness(at));
        }
        fd.sort_unstable();
        tick.sort_unstable();
        eprintln!(
            "wake late by (us): timerfd p50 {} p95 {} · tokio p50 {} p95 {}",
            fd[100], fd[190], tick[100], tick[190]
        );
        assert!(fd[0] >= 0, "woke early: {fd:?}");
        assert!(
            fd[100] < tick[100],
            "timerfd p50 {} us is not under tokio's {} us",
            fd[100],
            tick[100]
        );
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

    /// The send loop's hot path: every turn drops the sleep and polls the same deadline again,
    /// which takes the timer as it is armed. 21 rounds; the median holds the < 1 ms bound (a
    /// single round can wait for a CPU under the parallel suite). A round whose first poll came
    /// after the deadline (the runtime stalled) exercised nothing and is skipped.
    #[tokio::test]
    async fn a_deadline_polled_again_after_a_drop_wakes_on_time() {
        let mut timer = PacingTimer::new();
        let mut late = Vec::new();
        for _ in 0..21 {
            let at = Instant::now() + Duration::from_millis(2);
            let first = tokio::time::timeout(Duration::from_micros(500), timer.sleep_until(at));
            if first.await.is_ok() {
                assert!(Instant::now() >= at, "woke early");
                continue;
            }
            timer.sleep_until(at).await;
            let woke = Instant::now();
            assert!(woke >= at, "woke early");
            late.push(woke - at);
        }
        late.sort_unstable();
        assert!(
            late.get(late.len() / 2)
                .is_none_or(|l| *l < Duration::from_millis(1)),
            "{late:?}"
        );
    }
}
