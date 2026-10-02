//! SIGTERM and SIGINT. The recovery binary runs as PID 1 with no init, so without a handler
//! the kernel ignores both and every `docker stop` waits out its grace period and kills.
//!
//! On a signal the process exits 0 at the next safe point: at once, unless a [`Critical`]
//! section is open, in which case when it closes, or after [`GRACE`] if it does not. Leaving
//! anywhere else is safe because everything the actor and the seed persist is crash-safe
//! (atomic files, installs resumed on the next start); a critical section only keeps a
//! short engine sequence (create then start) from being split.

use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::{Duration, Instant};

use tracing::{info, warn};

/// Below Docker's default 10 s stop timeout.
pub const GRACE: Duration = Duration::from_secs(8);

static OPEN: AtomicUsize = AtomicUsize::new(0);
static REQUESTED: AtomicBool = AtomicBool::new(false);

/// Holds off a signal's exit while it lives.
#[must_use = "a critical section ends when the guard is dropped"]
pub struct Critical(());

pub fn critical() -> Critical {
    OPEN.fetch_add(1, Ordering::SeqCst);
    Critical(())
}

impl Drop for Critical {
    fn drop(&mut self) {
        OPEN.fetch_sub(1, Ordering::SeqCst);
    }
}

/// Whether a stop was asked for; a loop about to start new work should not.
pub fn requested() -> bool {
    REQUESTED.load(Ordering::SeqCst)
}

/// Waits until no critical section is open, or `grace` passes. True when none was.
pub fn settle(grace: Duration) -> bool {
    let deadline = Instant::now() + grace;
    while OPEN.load(Ordering::SeqCst) > 0 {
        if Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    true
}

/// Installs the handlers on a thread of their own. `what` names the process in the log.
pub fn install(what: &'static str) -> std::io::Result<()> {
    use tokio::signal::unix::{signal, SignalKind};
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()?;
    // Registered before returning, so a signal that arrives right after start is not lost.
    let (mut term, mut int) = runtime.block_on(async {
        Ok::<_, std::io::Error>((
            signal(SignalKind::terminate())?,
            signal(SignalKind::interrupt())?,
        ))
    })?;
    std::thread::Builder::new()
        .name("quasar-signals".into())
        .spawn(move || {
            let name = runtime.block_on(async {
                tokio::select! {
                    _ = term.recv() => "SIGTERM",
                    _ = int.recv() => "SIGINT",
                }
            });
            REQUESTED.store(true, Ordering::SeqCst);
            info!(signal = name, "{what} stopping");
            if !settle(GRACE) {
                warn!(
                    token = "shutdown-critical-abandoned",
                    "{what}: a critical step did not finish within {GRACE:?}; stopping anyway (the next start completes it)"
                );
            }
            std::process::exit(0);
        })?;
    Ok(())
}
