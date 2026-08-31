//! Process-memory instrumentation for the per-session RSS-growth hunt (#419): ~1-4 MB RSS
//! per session cycle with no correlated growth in fds, threads, live `GstObject`s or VRAM.
//!
//! Measurement only — nothing here changes session behaviour. [`rss_kib`] samples
//! `/proc/self/statm` at each session boundary; [`maybe_trim`] is the discriminator between
//! reclaimable glibc-arena retention and a real retention path (trim flattens the slope in
//! the first case only). `MALLOC_ARENA_MAX` is the companion env-only knob, wired as a
//! compose passthrough; [`allocator_env_summary`] logs which arm a run was captured under.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::OnceLock;

use tracing::{debug, info};

/// Resident set size in KiB. `None` when `/proc` is unreadable; callers must treat that as
/// "no sample" and log nothing rather than fail. Page size comes from
/// `sysconf(_SC_PAGESIZE)` — a hardcoded 4096 mis-scales on a 16K-page kernel.
pub fn rss_kib() -> Option<u64> {
    let statm = std::fs::read_to_string("/proc/self/statm").ok()?;
    let resident_pages: u64 = statm.split_whitespace().nth(1)?.parse().ok()?;
    Some(resident_pages.saturating_mul(page_size_bytes()) / 1024)
}

fn page_size_bytes() -> u64 {
    static PAGE: OnceLock<u64> = OnceLock::new();
    *PAGE.get_or_init(|| {
        // SAFETY: `sysconf` is a pure query with no pointer arguments.
        let sz = unsafe { libc::sysconf(libc::_SC_PAGESIZE) };
        if sz > 0 {
            sz as u64
        } else {
            4096
        }
    })
}

// Declared locally: `libc`'s binding is target/version-gated, and the `cfg` keeps musl and
// non-Linux builds compiling. Plain `//` because rustdoc rejects a doc comment on an
// extern block and the crate builds under `clippy -D warnings`.
#[cfg(all(target_os = "linux", target_env = "gnu"))]
extern "C" {
    fn malloc_trim(pad: libc::size_t) -> libc::c_int;
}

/// `QUASAR_MALLOC_TRIM`, read once: the teardown path must not touch the environment per
/// session.
fn trim_enabled() -> bool {
    static ENABLED: OnceLock<bool> = OnceLock::new();
    *ENABLED.get_or_init(|| {
        matches!(
            std::env::var("QUASAR_MALLOC_TRIM").ok().as_deref(),
            Some("1") | Some("true") | Some("yes")
        )
    })
}

/// `malloc_trim(0)` if `QUASAR_MALLOC_TRIM` is on → `(freed_anything, rss_kib_after)`;
/// `None` when off (the default) so the caller skips logging. `malloc_trim` never
/// invalidates a live allocation but takes every arena lock, so call it once per session
/// teardown, never on a hot path.
pub fn maybe_trim() -> Option<(bool, Option<u64>)> {
    if !trim_enabled() {
        return None;
    }
    #[cfg(all(target_os = "linux", target_env = "gnu"))]
    {
        // SAFETY: no arguments to validate; malloc_trim is thread-safe.
        let released = unsafe { malloc_trim(0) } != 0;
        Some((released, rss_kib()))
    }
    #[cfg(not(all(target_os = "linux", target_env = "gnu")))]
    {
        Some((false, rss_kib()))
    }
}

/// Allocator-relevant environment, logged once at startup so a soak artifact records which
/// A/B arm it was captured under.
pub fn allocator_env_summary() -> String {
    let get = |k: &str| std::env::var(k).unwrap_or_else(|_| "<unset>".to_string());
    format!(
        "MALLOC_ARENA_MAX={} MALLOC_TRIM_THRESHOLD_={} MALLOC_MMAP_THRESHOLD_={} QUASAR_MALLOC_TRIM={}",
        get("MALLOC_ARENA_MAX"),
        get("MALLOC_TRIM_THRESHOLD_"),
        get("MALLOC_MMAP_THRESHOLD_"),
        get("QUASAR_MALLOC_TRIM"),
    )
}

/// Idempotent: safe to call from more than one entry point.
pub fn log_startup() {
    static LOGGED: AtomicBool = AtomicBool::new(false);
    if LOGGED.swap(true, Ordering::Relaxed) {
        return;
    }
    info!(
        target: "quasar.mem",
        "allocator env: {} | rss_kib={:?}",
        allocator_env_summary(),
        rss_kib()
    );
}

/// Sample RSS at a session boundary; `phase` is `"start"` or `"stop"`. INFO under the
/// `quasar.mem` target so a soak greps one token for the whole series.
pub fn log_session_boundary(phase: &str, session_id: &str) {
    let Some(rss) = rss_kib() else {
        return;
    };
    info!(
        target: "quasar.mem",
        "session {session_id}: {phase} rss_kib={rss}"
    );
}

/// Logs both sides of the trim so the reclaimable fraction of a cycle's growth is
/// attributable from the log alone.
pub fn on_session_teardown(session_id: &str) {
    let before = rss_kib();
    match maybe_trim() {
        Some((released, after)) => {
            info!(
                target: "quasar.mem",
                "session {session_id}: stop rss_kib={before:?} -> malloc_trim(released={released}) \
                 rss_kib={after:?}"
            );
        }
        None => {
            if let Some(before) = before {
                info!(target: "quasar.mem", "session {session_id}: stop rss_kib={before}");
            }
        }
    }
    debug!(target: "quasar.mem", "allocator env: {}", allocator_env_summary());
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rss_is_plausible_on_linux() {
        match rss_kib() {
            // Catches a unit mix-up (pages vs bytes vs KiB).
            Some(kib) => assert!(kib > 0 && kib < 1_000_000_000, "implausible rss {kib} KiB"),
            // Non-Linux / no /proc must return None, not panic.
            None => assert!(!cfg!(target_os = "linux") || !std::path::Path::new("/proc").exists()),
        }
    }

    #[test]
    fn trim_is_off_by_default() {
        // The shipping path must not call malloc_trim unless explicitly asked.
        if std::env::var("QUASAR_MALLOC_TRIM").is_err() {
            assert!(!trim_enabled());
            assert!(maybe_trim().is_none());
        }
    }

    #[test]
    fn env_summary_names_every_knob() {
        let s = allocator_env_summary();
        for k in [
            "MALLOC_ARENA_MAX",
            "MALLOC_TRIM_THRESHOLD_",
            "MALLOC_MMAP_THRESHOLD_",
            "QUASAR_MALLOC_TRIM",
        ] {
            assert!(s.contains(k), "summary missing {k}: {s}");
        }
    }
}
