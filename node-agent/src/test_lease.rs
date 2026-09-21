//! Re-acquiring an flock lease a test just dropped. flock belongs to the open file
//! description, and a sibling test that spawns a child copies every open fd until the
//! child's exec closes it, so the lock can outlive `drop` by that window (#285). A real
//! restart is a new process and never sees this.

use std::path::Path;
use std::time::{Duration, Instant};

/// Retries `acquire` while `held` says the lease is still locked. Any other error, or a
/// lock still held after 5 s, panics with the /proc/locks lines for `locked_file`.
pub(crate) fn reacquire<T, E: std::fmt::Display>(
    locked_file: &Path,
    mut acquire: impl FnMut() -> Result<T, E>,
    held: impl Fn(&E) -> bool,
) -> T {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        match acquire() {
            Ok(lease) => return lease,
            Err(e) if held(&e) && Instant::now() < deadline => {
                std::thread::sleep(Duration::from_millis(10));
            }
            Err(e) if held(&e) => {
                use std::os::unix::fs::MetadataExt;
                let ino = std::fs::metadata(locked_file).map(|m| m.ino()).unwrap_or(0);
                let holders: Vec<String> = std::fs::read_to_string("/proc/locks")
                    .unwrap_or_default()
                    .lines()
                    .filter(|l| l.contains(&format!(":{ino} ")))
                    .map(str::to_owned)
                    .collect();
                panic!("lease still held 5 s after drop: {e}; /proc/locks: {holders:?}");
            }
            Err(e) => panic!("re-acquire failed for a reason other than the lease: {e}"),
        }
    }
}
