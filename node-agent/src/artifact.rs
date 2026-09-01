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
pub const LOCK_STALE_AFTER: Duration = Duration::from_secs(90 * 60);

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
        .timeout_read(Some(READ_TIMEOUT))
        .build()
        .into();

    let mut current = d.url.to_string();
    let mut resp = None;
    for hop in 0..=MAX_REDIRECTS {
        validate_url(&current, d.host)?;
        tracing::info!(target: T, artifact = %what, url = %current, hop, "downloading {}", d.what);
        match agent.get(&current).call() {
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

// ── lock ─────────────────────────────────────────────────────────────────────

/// Cross-process guard: two agents sharing a volume must never both write it. `O_EXCL`
/// create; a lockfile older than [`LOCK_STALE_AFTER`] is taken over.
pub struct Lock {
    path: PathBuf,
    what: String,
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
                    let _ = writeln!(
                        f,
                        "pid={} agent={}",
                        std::process::id(),
                        env!("CARGO_PKG_VERSION")
                    );
                    tracing::debug!(target: T, artifact = %what, path = %path.display(), "provisioning lock acquired");
                    return Ok(Lock {
                        path: path.to_path_buf(),
                        what: what.to_string(),
                    });
                }
                Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists && attempt == 0 => {
                    let age = std::fs::metadata(path)
                        .and_then(|m| m.modified())
                        .ok()
                        .and_then(|t| SystemTime::now().duration_since(t).ok())
                        .unwrap_or(Duration::ZERO);
                    if age > LOCK_STALE_AFTER {
                        tracing::warn!(
                            target: T, token = "artifact-stale-lock-taken",
                            artifact = %what, age_s = age.as_secs(), path = %path.display(),
                            "taking over a stale provisioning lock (a previous agent died mid-provision)"
                        );
                        let _ = std::fs::remove_file(path);
                        continue;
                    }
                    bail!(
                        "another agent is already provisioning this artifact (lock held for {}s)",
                        age.as_secs()
                    );
                }
                Err(e) => return Err(anyhow!("acquire provisioning lock: {e}")),
            }
        }
        bail!("could not acquire the provisioning lock")
    }
}

impl Drop for Lock {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.path);
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
