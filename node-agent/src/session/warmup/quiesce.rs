//! #488 §3.3 step 7 — settle, then require **write quiescence**.
//!
//! Settle-alone is not enough: 60s after `app presented` a Steam client that
//! started a self-update is still writing, and snapshotting mid-update produces
//! a template that is neither the old nor the new client. The gate is "no
//! mtime change anywhere under the scratch home for `window` consecutive
//! seconds", capped by the job timeout.
//!
//! The tracker is pure: it takes an observation (newest mtime + instant) and
//! answers whether the tree has been still long enough. [`newest_mtime`]
//! produces the observation.

use std::fs;
use std::path::Path;
use std::time::{Duration, Instant, SystemTime};

/// #488 §3.3: how long the tree must be unchanged before snapshotting.
pub const DEFAULT_QUIESCE_WINDOW: Duration = Duration::from_secs(10);

/// Tracks consecutive-stillness of a tree across repeated observations.
#[derive(Debug)]
pub struct QuiescenceTracker {
    window: Duration,
    /// The newest mtime observed so far, and when we first saw the tree at that
    /// value. `None` until the first observation.
    latest: Option<(SystemTime, Instant)>,
}

impl QuiescenceTracker {
    pub fn new(window: Duration) -> Self {
        QuiescenceTracker {
            window,
            latest: None,
        }
    }

    /// Feed one observation. Returns `true` once the newest mtime has been
    /// unchanged for the whole window.
    ///
    /// A newest mtime that *moves backwards* (the newest file was deleted)
    /// counts as a change and restarts the window — the tree is still being
    /// written to.
    pub fn observe(&mut self, newest: SystemTime, now: Instant) -> bool {
        match self.latest {
            Some((seen, since)) if seen == newest => now.duration_since(since) >= self.window,
            _ => {
                self.latest = Some((newest, now));
                // A zero window means "one still observation is enough".
                self.window.is_zero()
            }
        }
    }

    /// How long the tree has been still, for logging.
    pub fn still_for(&self, now: Instant) -> Duration {
        self.latest
            .map(|(_, since)| now.duration_since(since))
            .unwrap_or_default()
    }
}

/// The newest mtime anywhere under `root` (root included). Unreadable entries
/// are skipped rather than failing the scan: the scratch home is written by
/// the app container as another uid, and one transiently-vanishing file must
/// not abort a 35s job. `None` means the tree could not be read at all.
pub fn newest_mtime(root: &Path) -> Option<SystemTime> {
    let mut newest = fs::symlink_metadata(root).ok()?.modified().ok();
    let mut stack = vec![root.to_path_buf()];
    while let Some(dir) = stack.pop() {
        let Ok(entries) = fs::read_dir(&dir) else {
            continue;
        };
        for entry in entries.flatten() {
            let Ok(meta) = fs::symlink_metadata(entry.path()) else {
                continue;
            };
            if let Ok(m) = meta.modified() {
                let advanced = match newest {
                    None => true,
                    Some(n) => m > n,
                };
                if advanced {
                    newest = Some(m);
                }
            }
            if meta.file_type().is_dir() {
                stack.push(entry.path());
            }
        }
    }
    newest
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn t(secs: u64) -> SystemTime {
        SystemTime::UNIX_EPOCH + Duration::from_secs(secs)
    }

    #[test]
    fn quiescence_needs_the_full_window_of_stillness() {
        let start = Instant::now();
        let mut q = QuiescenceTracker::new(Duration::from_secs(10));
        assert!(!q.observe(t(100), start));
        assert!(!q.observe(t(100), start + Duration::from_secs(9)));
        assert!(q.observe(t(100), start + Duration::from_secs(10)));
    }

    #[test]
    fn a_write_restarts_the_window() {
        let start = Instant::now();
        let mut q = QuiescenceTracker::new(Duration::from_secs(10));
        assert!(!q.observe(t(100), start));
        // 9s in, something wrote: the clock restarts from here.
        assert!(!q.observe(t(109), start + Duration::from_secs(9)));
        assert!(!q.observe(t(109), start + Duration::from_secs(18)));
        assert!(q.observe(t(109), start + Duration::from_secs(19)));
    }

    #[test]
    fn a_backwards_mtime_also_restarts_the_window() {
        let start = Instant::now();
        let mut q = QuiescenceTracker::new(Duration::from_secs(10));
        assert!(!q.observe(t(100), start));
        assert!(!q.observe(t(50), start + Duration::from_secs(10)));
        assert!(q.observe(t(50), start + Duration::from_secs(20)));
    }

    #[test]
    fn newest_mtime_walks_the_whole_tree() {
        let d = tempfile::tempdir().unwrap();
        fs::create_dir_all(d.path().join("a/b")).unwrap();
        let deep = d.path().join("a/b/c.txt");
        fs::File::create(&deep).unwrap().write_all(b"x").unwrap();
        let seen = newest_mtime(d.path()).expect("tree is readable");
        let direct = fs::metadata(&deep).unwrap().modified().unwrap();
        assert!(seen >= direct);
    }

    #[test]
    fn newest_mtime_of_a_missing_tree_is_none() {
        assert!(newest_mtime(Path::new("/definitely/not/here/488")).is_none());
    }
}
