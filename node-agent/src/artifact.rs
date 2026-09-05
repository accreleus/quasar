//! Shared machinery for provisioning a downloaded artifact into a host volume: origin-pinned
//! download with sha256 and a completeness check, a cross-process lockfile, persisted
//! failure backoff, a free-space preflight. Each guard here is a defect that reached a live
//! host (#476/#477/#478), shared so a second provisioner cannot re-learn them.
//!
//! Fetching and guarding only — no layout, manifest or version scheme; those stay with each
//! provisioner ([`crate::nvidia_volume`], [`crate::cuda_runtime`]). `nvidia_volume` keeps
//! thin wrappers at the old names so its tests still assert the same surface.

use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, bail, Context, Result};
use sha2::{Digest, Sha256};

/// Fixed, not per-caller: `tracing`'s `target:` must be a constant expression. Lines carry
/// `artifact=<name>` ([`Download::what`]) instead, so one artifact's story stays greppable.
pub const T: &str = "quasar.artifact";

/// Redirect hops, each re-validated against the caller's pinned host so an off-host hop
/// still hard-fails.
pub const MAX_REDIRECTS: usize = 3;

/// Progress-logging cadence: every this many percent…
pub const PROGRESS_PERCENT_STEP: u64 = 10;
/// …or this often, whichever comes first, so a stalled download still says so.
pub const PROGRESS_TIME_STEP: Duration = Duration::from_secs(30);

/// Per-read socket timeout inside a download's total budget.
pub const READ_TIMEOUT: Duration = Duration::from_secs(60);

/// An older lockfile is taken over as belonging to an agent killed mid-provision. Long
/// enough that it can never race a live download that is merely slow.
///
/// Only for a lockfile with no `heartbeat=` marker — one written by an agent predating
/// #66. A heartbeating holder is reclaimed on [`stale_after_for`]'s much shorter window.
pub const LOCK_STALE_AFTER: Duration = Duration::from_secs(90 * 60);

/// How often a lock holder rewrites its lockfile to say it is still working.
pub const HEARTBEAT_INTERVAL: Duration = Duration::from_secs(30);

/// Floor on the heartbeat takeover window, so a short interval cannot make takeover
/// trigger-happy against a holder whose beat is merely delayed by a loaded box.
pub const MIN_HEARTBEAT_STALE: Duration = Duration::from_secs(5 * 60);

// ── url policy ───────────────────────────────────────────────────────────────

/// HTTPS + pinned host, nothing else. These payloads are executed or `dlopen`ed on the host
/// and no vendor publishes a verifiable signature, so origin is the only control there is.
pub fn validate_url(url: &str, host: &str) -> Result<()> {
    let rest = url
        .strip_prefix("https://")
        .ok_or_else(|| anyhow!("refusing a non-HTTPS download URL: {url}"))?;
    let authority = rest.split(['/', '?', '#']).next().unwrap_or("");
    // `evil.com@download.nvidia.com` must not smuggle an authority past a prefix check.
    if authority.contains('@') {
        bail!("refusing a download URL with embedded credentials: {url}");
    }
    let hostname = authority.split(':').next().unwrap_or("");
    if hostname != host {
        bail!("refusing a download from {hostname:?} — only {host} is allowed");
    }
    Ok(())
}

/// Resolve a `Location` against the URL it came from. Absolute URLs pass through (the
/// caller's [`validate_url`] pins them); `/path` joins onto the current origin. Anything
/// else is refused, never guessed — a wrong guess is a request to an unintended origin.
pub fn join_redirect(current: &str, location: &str) -> Result<String> {
    if location.starts_with("https://") || location.starts_with("http://") {
        return Ok(location.to_string());
    }
    if let Some(path) = location.strip_prefix('/') {
        if path.starts_with('/') {
            bail!("refusing a scheme-relative redirect Location: {location}");
        }
        let rest = current
            .strip_prefix("https://")
            .ok_or_else(|| anyhow!("cannot resolve a relative redirect from {current}"))?;
        let authority = rest.split(['/', '?', '#']).next().unwrap_or("");
        return Ok(format!("https://{authority}/{path}"));
    }
    bail!("refusing an unsupported redirect Location: {location}")
}

/// A body truncated mid-connection still passes every downstream shape check and then fails
/// as somebody else's bug (#478). No `Content-Length` ⇒ nothing to check, accepted.
pub fn verify_complete(written: u64, total: Option<u64>) -> Result<()> {
    let Some(total) = total else {
        return Ok(());
    };
    if written == total {
        return Ok(());
    }
    bail!(
        "short download: got {written} of {total} bytes ({}%) — the connection closed mid-body. \
         Refusing to use a truncated payload.",
        written.saturating_mul(100) / total.max(1)
    )
}

pub fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// `PATH` lookup: a missing extraction tool must refuse before a hundreds-of-MiB fetch.
pub fn which(tool: &str) -> Option<PathBuf> {
    let path = std::env::var("PATH").ok()?;
    for dir in path.split(':').filter(|d| !d.is_empty()) {
        let p = Path::new(dir).join(tool);
        if p.is_file() {
            return Some(p);
        }
    }
    None
}

// ── download ─────────────────────────────────────────────────────────────────

/// One artifact fetch, parameterised so [`fetch`] has no opinion about what it downloads.
pub struct Download<'a> {
    pub url: &'a str,
    pub dest: &'a Path,
    /// The only host accepted, on the first request and every redirect hop.
    pub host: &'a str,
    /// Operator-readable noun; rides every line as `artifact=<what>`.
    pub what: &'a str,
    /// Total wall-clock budget, so a black-holed connection cannot hold the lock forever.
    pub timeout: Duration,
    /// Smallest plausible size; under it the payload is an error page or stub.
    pub min_bytes: u64,
    /// Required leading bytes (`#!`, xz magic). `None` ⇒ no shape check.
    pub magic: Option<&'a [u8]>,
    /// 404 message. A never-published build must not read as a network fault.
    pub not_found: &'a str,
    /// Per-progress-line hook for the provisioner's own readiness surface.
    pub on_progress: Option<&'a dyn Fn(Option<u64>)>,
}

/// Fetch to [`Download::dest`], returning its sha256. HTTPS only, host pinned, redirects
/// followed manually and re-validated on every hop.
pub fn fetch(d: &Download<'_>) -> Result<String> {
    let what = d.what;
    let agent: ureq::Agent = ureq::Agent::config_builder()
        .max_redirects(0)
        .timeout_connect(Some(Duration::from_secs(20)))
        .build()
        .into();

    let mut current = d.url.to_string();
    let mut resp = None;
    for hop in 0..=MAX_REDIRECTS {
        validate_url(&current, d.host)?;
        tracing::info!(target: T, artifact = %what, url = %current, hop, "downloading {}", d.what);
        match agent
            .get(&current)
            .config()
            .timeout_recv_body(Some(d.timeout))
            .build()
            .call()
        {
            // A 3xx arrives in the Ok arm, never Err(StatusCode) (#478): ureq converts to
            // `Error::StatusCode` only at >= 400. Handling it on the Err arm is dead code, and
            // the empty 3xx body then lands on disk under a misleading error.
            Ok(r) if (300..400).contains(&r.status().as_u16()) => {
                let code = r.status().as_u16();
                let loc = r
                    .headers()
                    .get("Location")
                    .and_then(|v| v.to_str().ok())
                    .ok_or_else(|| anyhow!("HTTP {code} redirect with no Location header"))?
                    .to_string();
                let next = join_redirect(&current, &loc)?;
                tracing::info!(target: T, artifact = %what, code, location = %next, "following redirect");
                // Re-validated at the top of the next iteration: every hop is pinned.
                current = next;
            }
            Ok(r) => {
                resp = Some(r);
                break;
            }
            Err(ureq::Error::StatusCode(404)) => bail!("{} (HTTP 404)", d.not_found),
            Err(ureq::Error::StatusCode(code)) => bail!("HTTP {code} fetching {current}"),
            Err(e) => bail!("network error fetching {current}: {e}"),
        }
    }
    let mut resp =
        resp.ok_or_else(|| anyhow!("too many redirects (>{MAX_REDIRECTS}) fetching {}", d.url))?;

    let total: Option<u64> = resp
        .headers()
        .get("Content-Length")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.parse::<u64>().ok());
    tracing::info!(
        target: T, artifact = %what,
        bytes_total = total.unwrap_or(0),
        "download started ({} MiB expected)",
        total.map(|t| t / (1024 * 1024)).unwrap_or(0)
    );

    let mut reader = resp.body_mut().as_reader();
    let mut file =
        std::fs::File::create(d.dest).with_context(|| format!("create {}", d.dest.display()))?;
    let mut hasher = Sha256::new();
    let mut buf = vec![0u8; 256 * 1024];
    let mut written: u64 = 0;
    let started = Instant::now();
    let mut last_log = Instant::now();
    let mut last_pct = 0u64;
    let mut head = Vec::new();

    loop {
        if started.elapsed() > d.timeout {
            bail!(
                "download exceeded the {:?} budget after {written} bytes",
                d.timeout
            );
        }
        let n = reader.read(&mut buf).context("read download stream")?;
        if n == 0 {
            break;
        }
        if head.len() < 64 {
            head.extend_from_slice(&buf[..n.min(64)]);
        }
        hasher.update(&buf[..n]);
        file.write_all(&buf[..n])
            .context("write artifact to scratch")?;
        written += n as u64;

        let pct = total.map(|t| written.saturating_mul(100) / t.max(1));
        let pct_due = pct.is_some_and(|p| p >= last_pct + PROGRESS_PERCENT_STEP);
        if pct_due || last_log.elapsed() >= PROGRESS_TIME_STEP {
            if let Some(p) = pct {
                last_pct = p - (p % PROGRESS_PERCENT_STEP);
            }
            last_log = Instant::now();
            let mib = written / (1024 * 1024);
            let rate = mib as f64 / started.elapsed().as_secs_f64().max(0.001);
            tracing::info!(
                target: T, artifact = %what,
                percent = pct.unwrap_or(0),
                mib,
                "download progress: {} MiB{} at {rate:.1} MiB/s",
                mib,
                pct.map(|p| format!(" ({p}%)")).unwrap_or_default()
            );
            if let Some(f) = d.on_progress {
                f(pct);
            }
        }
    }
    file.flush().ok();
    drop(file);

    verify_complete(written, total)?;

    // A 200 that is really an error page would otherwise be handed to `sh` or `tar`.
    if let Some(magic) = d.magic {
        if !head.starts_with(magic) {
            bail!(
                "the downloaded payload does not have the expected {} header (first bytes: {:?}) \
                 — refusing to use it",
                d.what,
                String::from_utf8_lossy(&head[..head.len().min(16)])
            );
        }
    }
    if written < d.min_bytes {
        bail!(
            "downloaded {} is implausibly small ({written} bytes, expected at least {})",
            d.what,
            d.min_bytes
        );
    }

    let sha256 = hex(&hasher.finalize());
    tracing::info!(
        target: T, artifact = %what,
        bytes = written,
        sha256 = %sha256,
        elapsed_s = started.elapsed().as_secs(),
        "download complete"
    );
    Ok(sha256)
}

// ── restart barrier (#66) ────────────────────────────────────────────────────

/// Provisioning locks this process holds right now.
///
/// The two provisioners ([`crate::nvidia_volume`], [`crate::cuda_runtime`]) run on
/// independent threads and each schedules a self-restart when it finishes. `exit(0)` from
/// one kills the other mid-write: on a virgin host the small NVRTC fetch routinely wins
/// that race and tore down a 441 MB driver extraction, stranding the lockfile behind it
/// and leaving the host without Vulkan encode for the whole stale window (#66). A restart
/// path must therefore wait for quiescence instead of exiting on its own schedule.
static PROVISIONS_IN_FLIGHT: AtomicUsize = AtomicUsize::new(0);

/// Poll cadence for [`wait_for_quiescence`].
const QUIESCENCE_POLL: Duration = Duration::from_millis(200);

/// How many provisions this process is running right now.
pub fn provisioning_in_flight() -> usize {
    PROVISIONS_IN_FLIGHT.load(Ordering::SeqCst)
}

/// Block until this process holds no provisioning lock, or `timeout` elapses; `true` if it
/// went quiescent. Deliberately a poll rather than a condvar: the only caller is a restart
/// thread about to end the process, where a missed notify would hang the restart forever
/// and the extra precision buys nothing.
pub fn wait_for_quiescence(timeout: Duration) -> bool {
    let deadline = Instant::now() + timeout;
    loop {
        if provisioning_in_flight() == 0 {
            return true;
        }
        if Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(QUIESCENCE_POLL);
    }
}

// ── lock ─────────────────────────────────────────────────────────────────────

/// Cross-process guard: two agents sharing a volume must never both write it. `O_EXCL`
/// create; a lockfile whose holder has stopped touching it is taken over.
///
/// The holder rewrites its lockfile every [`HEARTBEAT_INTERVAL`] while it works, so a lock
/// abandoned by a killed agent is reclaimed in minutes rather than [`LOCK_STALE_AFTER`].
/// The heartbeat is advertised IN the lockfile (`heartbeat=<secs>`) and takeover only uses
/// the short window when it sees that marker — a lock written by an agent too old to
/// heartbeat keeps the long, conservative window, so a rolling upgrade can never let a new
/// agent evict an old one that is merely slow.
///
/// PID liveness is deliberately NOT the signal: the agent runs in a container under
/// `init: true`, so a restarted agent gets the same low PID its dead predecessor recorded
/// and `/proc/<pid>` would report the corpse as alive.
pub struct Lock {
    path: PathBuf,
    what: String,
    stop: Arc<AtomicBool>,
}

impl Lock {
    pub fn acquire(path: &Path, what: &str) -> Result<Lock> {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent).ok();
        }
        for attempt in 0..2 {
            match std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .open(path)
            {
                Ok(mut f) => {
                    let _ = f.write_all(lock_body().as_bytes());
                    tracing::debug!(target: T, artifact = %what, path = %path.display(), "provisioning lock acquired");
                    PROVISIONS_IN_FLIGHT.fetch_add(1, Ordering::SeqCst);
                    let stop = Arc::new(AtomicBool::new(false));
                    spawn_heartbeat(path.to_path_buf(), what.to_string(), Arc::clone(&stop));
                    return Ok(Lock {
                        path: path.to_path_buf(),
                        what: what.to_string(),
                        stop,
                    });
                }
                Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists && attempt == 0 => {
                    let age = std::fs::metadata(path)
                        .and_then(|m| m.modified())
                        .ok()
                        .and_then(|t| SystemTime::now().duration_since(t).ok())
                        .unwrap_or(Duration::ZERO);
                    let body = std::fs::read_to_string(path).unwrap_or_default();
                    let window = stale_after_for(&body);
                    if age > window {
                        tracing::warn!(
                            target: T, token = "artifact-stale-lock-taken",
                            artifact = %what, age_s = age.as_secs(),
                            window_s = window.as_secs(), path = %path.display(),
                            "taking over a stale provisioning lock (a previous agent died mid-provision)"
                        );
                        let _ = std::fs::remove_file(path);
                        continue;
                    }
                    bail!(
                        "another agent is already provisioning this artifact (lock held for {}s, \
                         taken over after {}s untouched)",
                        age.as_secs(),
                        window.as_secs()
                    );
                }
                Err(e) => return Err(anyhow!("acquire provisioning lock: {e}")),
            }
        }
        bail!("could not acquire the provisioning lock")
    }
}

/// The lockfile's contents. `pid`/`agent` are for the operator reading it; `heartbeat` is
/// the load-bearing field — see [`stale_after_for`].
fn lock_body() -> String {
    format!(
        "pid={} agent={} heartbeat={}\n",
        std::process::id(),
        env!("CARGO_PKG_VERSION"),
        HEARTBEAT_INTERVAL.as_secs()
    )
}

/// Rewrite the lockfile on a cadence so its mtime keeps saying "still working". Rewriting
/// the body (rather than a `utimensat`) keeps this dependency-free and self-describing.
fn spawn_heartbeat(path: PathBuf, what: String, stop: Arc<AtomicBool>) {
    let _ = std::thread::Builder::new()
        .name("quasar-provision-hb".into())
        .spawn(move || {
            while !stop.load(Ordering::SeqCst) {
                std::thread::sleep(HEARTBEAT_INTERVAL);
                if stop.load(Ordering::SeqCst) {
                    return;
                }
                if std::fs::write(&path, lock_body()).is_err() {
                    tracing::debug!(target: T, artifact = %what, "provisioning lock heartbeat could not write");
                    return;
                }
            }
        });
}

/// How long a lockfile may go untouched before another agent may take it over.
///
/// A holder that advertises a heartbeat is reclaimed at five missed beats (floored at
/// [`MIN_HEARTBEAT_STALE`] so a tiny interval cannot make takeover trigger-happy). A
/// lockfile with no marker predates the heartbeat and keeps [`LOCK_STALE_AFTER`].
pub fn stale_after_for(body: &str) -> Duration {
    match heartbeat_interval(body) {
        Some(interval) => (interval * 5).max(MIN_HEARTBEAT_STALE),
        None => LOCK_STALE_AFTER,
    }
}

fn heartbeat_interval(body: &str) -> Option<Duration> {
    body.split_whitespace()
        .find_map(|field| field.strip_prefix("heartbeat="))
        .and_then(|v| v.parse::<u64>().ok())
        .filter(|secs| *secs > 0)
        .map(Duration::from_secs)
}

impl Drop for Lock {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        let _ = std::fs::remove_file(&self.path);
        PROVISIONS_IN_FLIGHT.fetch_sub(1, Ordering::SeqCst);
        tracing::debug!(target: T, artifact = %self.what, "provisioning lock released");
    }
}

// ── failure backoff (#477) ───────────────────────────────────────────────────

/// Persisted attempt bookkeeping. Must be written BEFORE the download, or a crash
/// loop that never reaches the failure path is not rate-limited at all. A change of
/// `version` resets the counter.
#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct Attempts {
    pub version: String,
    pub attempts: u32,
    pub last_attempt_unix: u64,
    #[serde(default)]
    pub last_error: String,
}

pub fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

pub fn read_attempts(path: &Path) -> Attempts {
    std::fs::read_to_string(path)
        .ok()
        .and_then(|b| serde_json::from_str(&b).ok())
        .unwrap_or_default()
}

/// Remaining wait before a re-attempt, `None` when it may go now:
/// `base * 2^(attempts-1)`, capped at `max`.
pub fn backoff_remaining(
    a: &Attempts,
    version: &str,
    now_unix: u64,
    base: Duration,
    max: Duration,
) -> Option<Duration> {
    if a.version != version || a.attempts == 0 {
        return None;
    }
    let wait = base
        .saturating_mul(1u32.checked_shl(a.attempts.min(20) - 1).unwrap_or(u32::MAX))
        .min(max);
    let elapsed = Duration::from_secs(now_unix.saturating_sub(a.last_attempt_unix));
    (elapsed < wait).then(|| wait - elapsed)
}

pub fn note_attempt(path: &Path, version: &str) {
    let prev = read_attempts(path);
    let attempts = if prev.version == version {
        prev.attempts.saturating_add(1)
    } else {
        1
    };
    let rec = Attempts {
        version: version.to_string(),
        attempts,
        last_attempt_unix: now_unix(),
        last_error: String::new(),
    };
    if let Ok(body) = serde_json::to_vec_pretty(&rec) {
        let _ = std::fs::write(path, body);
    }
}

pub fn note_failure(path: &Path, error: &str) {
    let mut rec = read_attempts(path);
    if rec.attempts == 0 {
        return;
    }
    rec.last_error = error.to_string();
    if let Ok(body) = serde_json::to_vec_pretty(&rec) {
        let _ = std::fs::write(path, body);
    }
}

pub fn clear_attempts(path: &Path) {
    let _ = std::fs::remove_file(path);
}

// ── disk preflight ───────────────────────────────────────────────────────────

/// Bytes available under `path` via `statvfs`. `None` on syscall failure: the caller
/// fails open and proceeds, since an unanswerable question must not become a hard no.
// `statvfs` block counts are 64-bit on x86_64 but 32-bit on other targets, so the cast is
// portable and clippy calls it redundant only when building for aarch64.
#[allow(clippy::unnecessary_cast)]
pub fn free_bytes(path: &Path) -> Option<u64> {
    let c = std::ffi::CString::new(path.as_os_str().as_encoded_bytes()).ok()?;
    // SAFETY: both pointers are valid for the duration of the call.
    unsafe {
        let mut st: libc::statvfs = std::mem::zeroed();
        if libc::statvfs(c.as_ptr(), &mut st) != 0 {
            return None;
        }
        Some(st.f_bavail.saturating_mul(st.f_frsize))
    }
}

/// The pure half of the preflight, so the threshold is testable without a filesystem of a
/// given size. `consequence` says why filling this filesystem is worse than failing.
pub fn free_space_verdict(free: u64, required: u64, what: &str, consequence: &str) -> Result<()> {
    let mib = |b: u64| b / (1024 * 1024);
    if free >= required {
        tracing::info!(
            target: T, artifact = %what,
            free_mib = mib(free), required_mib = mib(required),
            "free-space preflight passed"
        );
        return Ok(());
    }
    bail!(
        "not enough free space to provision {what}: {} MiB available where {} MiB are needed. \
         {consequence}",
        mib(free),
        mib(required)
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    /// `PROVISIONS_IN_FLIGHT` is process-global, so the tests that assert on it must not
    /// run beside each other — cargo runs tests on parallel threads and a neighbour holding
    /// its own lock makes the count read one too high.
    static LOCK_TEST_SERIAL: std::sync::Mutex<()> = std::sync::Mutex::new(());

    fn serial_lock_test() -> std::sync::MutexGuard<'static, ()> {
        LOCK_TEST_SERIAL
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    #[test]
    fn only_the_pinned_https_host_is_accepted() {
        assert!(validate_url("https://example.com/a", "example.com").is_ok());
        assert!(validate_url("https://example.com:443/a", "example.com").is_ok());
        for bad in [
            "http://example.com/a",
            "https://evil.example/a",
            "https://example.com.evil.example/a",
            "https://evil.example@example.com/a",
            "ftp://example.com/a",
        ] {
            assert!(
                validate_url(bad, "example.com").is_err(),
                "must refuse {bad}"
            );
        }
        // The pin is per-caller.
        assert!(validate_url("https://a.example/x", "b.example").is_err());
    }

    #[test]
    fn lock_is_exclusive_and_released_on_drop() {
        let _serial = serial_lock_test();
        let dir = std::env::temp_dir().join(format!("quasar-artifact-lock-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let p = dir.join(".provision.lock");
        let _ = std::fs::remove_file(&p);

        let l = Lock::acquire(&p, "test").unwrap();
        assert!(p.is_file());
        assert!(
            Lock::acquire(&p, "test").is_err(),
            "a second concurrent provision must be refused"
        );
        drop(l);
        assert!(!p.exists());
        let l2 = Lock::acquire(&p, "test").unwrap();
        drop(l2);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_held_lock_is_counted_in_flight_and_blocks_a_restart_barrier() {
        let _serial = serial_lock_test();
        // #66: the restart path must see the driver-volume provision that the cudart
        // thread's `exit(0)` would otherwise kill mid-extraction.
        let dir =
            std::env::temp_dir().join(format!("quasar-artifact-barrier-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let p = dir.join(".provision.lock");
        let _ = std::fs::remove_file(&p);

        let before = provisioning_in_flight();
        let l = Lock::acquire(&p, "test").unwrap();
        assert_eq!(provisioning_in_flight(), before + 1);
        assert!(
            !wait_for_quiescence(Duration::from_millis(300)),
            "a restart must not be cleared to exit while a provision holds the lock"
        );
        drop(l);
        assert_eq!(provisioning_in_flight(), before);
        assert!(
            wait_for_quiescence(Duration::from_secs(5)),
            "the barrier must clear once the provision releases its lock"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_heartbeating_lock_is_reclaimed_far_sooner_than_a_legacy_one() {
        // A holder that advertises a heartbeat is reclaimed at five missed beats...
        let heartbeat = format!(
            "pid=7 agent=0.1.0 heartbeat={}\n",
            HEARTBEAT_INTERVAL.as_secs()
        );
        assert_eq!(stale_after_for(&heartbeat), MIN_HEARTBEAT_STALE);
        let slow = "pid=7 agent=0.1.0 heartbeat=120\n";
        assert_eq!(stale_after_for(slow), Duration::from_secs(600));

        // ...while a lockfile written by an agent predating #66 keeps the long window, so
        // a rolling upgrade cannot evict an old agent that is merely slow.
        for legacy in [
            "pid=7 agent=0.1.0\n",
            "",
            "pid=7 heartbeat=notanumber\n",
            "heartbeat=0\n",
        ] {
            assert_eq!(
                stale_after_for(legacy),
                LOCK_STALE_AFTER,
                "a lockfile with no usable heartbeat marker must keep the conservative window: {legacy:?}"
            );
        }
    }

    #[test]
    fn acquire_advertises_the_heartbeat_in_the_lockfile() {
        let _serial = serial_lock_test();
        let dir = std::env::temp_dir().join(format!("quasar-artifact-hb-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let p = dir.join(".provision.lock");
        let _ = std::fs::remove_file(&p);

        let l = Lock::acquire(&p, "test").unwrap();
        let body = std::fs::read_to_string(&p).unwrap();
        assert!(
            body.contains(&format!("heartbeat={}", HEARTBEAT_INTERVAL.as_secs())),
            "body was {body:?}"
        );
        assert!(
            body.contains("pid="),
            "the operator-facing pid must survive: {body:?}"
        );
        assert_eq!(stale_after_for(&body), MIN_HEARTBEAT_STALE);
        drop(l);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn backoff_grows_and_is_capped_and_resets_on_a_new_version() {
        let base = Duration::from_secs(300);
        let max = Duration::from_secs(6 * 3600);
        let mut a = Attempts {
            version: "1".into(),
            attempts: 1,
            last_attempt_unix: 1_000,
            last_error: String::new(),
        };
        assert_eq!(backoff_remaining(&a, "1", 1_000, base, max), Some(base));
        a.attempts = 3;
        assert_eq!(backoff_remaining(&a, "1", 1_000, base, max), Some(base * 4));
        a.attempts = 99;
        assert_eq!(backoff_remaining(&a, "1", 1_000, base, max), Some(max));
        assert!(backoff_remaining(&a, "2", 1_000, base, max).is_none());
    }
}
