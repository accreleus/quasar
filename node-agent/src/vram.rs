//! #383 live free-VRAM telemetry sampler. Spec:
//! `docs/design/plans/2026-07-26-383-vram-admission-telemetry-spec.md` §3.1.
//!
//! One point-in-time used/free sample per GPU. Heartbeat cadence lives in agent.rs, driving
//! the [`VramCache`] this module hands it; the wire shape is `VramSample`'s own `Serialize`
//! (`messages.rs` embeds it in `Heartbeat`).
//!
//! Load-bearing invariants:
//! - **Never fabricate a `0`.** Admission reads `free_mb: 0` as "this GPU is full", so every
//!   read or parse failure must produce `None`. That includes a literal `used=0, free=0`,
//!   which is the faulted/off-the-bus signature, not an empty GPU.
//! - **ONE `nvidia-smi` fork covers every NVIDIA GPU**, matched back by PCI bus id. Never
//!   per-GPU forks, and never `capacity::nvidia_smi_rows()` — its `OnceLock` memoizes
//!   boot-time totals, so a live sampler would report the same number forever.

use std::collections::HashSet;
use std::io::Read;
use std::path::PathBuf;
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

/// What the sampler needs to read one GPU's live memory, captured by
/// `capacity::detect_gpus_at`. `pci_addr` matches an `nvidia-smi` row back to this GPU and
/// is used on the NVIDIA path only.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VramTarget {
    pub index: i32,
    pub vendor: String,
    pub sysfs_device: PathBuf,
    pub pci_addr: Option<String>,
    pub total_mb: i32,
}

/// One GPU's live memory state. `None` where unavailable, never a fabricated zero. Absent
/// fields are omitted, not `null`, per `messages.rs`'s `Heartbeat.gpu_vram` shape.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct VramSample {
    pub index: i32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub used_mb: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub free_mb: Option<i32>,
}

/// Hard timeout for one `nvidia-smi` fork, enforced by [`run_bounded`]'s spawn+poll+kill
/// loop. An outer `tokio::time::timeout` cannot cancel a blocking child wait, so the worst
/// case here is a `None` sample, never a leaked thread/process per tick.
pub const SAMPLE_TIMEOUT: Duration = Duration::from_secs(2);

/// Warn once per process per vendor on a sample-read failure, debug thereafter. Keyed per
/// vendor so a broken AMD iGPU cannot demote the first real NVIDIA failure to debug.
static WARNED_VENDORS: OnceLock<Mutex<HashSet<String>>> = OnceLock::new();

/// Split out from the `tracing` calls so tests can assert the bookkeeping with no subscriber.
fn is_first_failure(vendor: &str) -> bool {
    let set = WARNED_VENDORS.get_or_init(|| Mutex::new(HashSet::new()));
    let mut guard = set.lock().unwrap_or_else(|poisoned| poisoned.into_inner());
    guard.insert(vendor.to_string())
}

fn warn_or_debug_once(vendor: &str, detail: &str) {
    if is_first_failure(vendor) {
        tracing::warn!(
            token = "vram-sample-failed",
            vendor,
            detail,
            "vram sample read failed; further failures on this vendor logged at debug"
        );
    } else {
        tracing::debug!(vendor, detail, "vram sample read failed");
    }
}

/// Blocking. Async callers must run it via `spawn_blocking` ([`VramCache::spawn_tick`]),
/// never inline on the agent's control task.
pub fn sample(targets: &[VramTarget]) -> Vec<VramSample> {
    let nvidia_rows = if targets.iter().any(|t| t.vendor == "nvidia") {
        Some(run_nvidia_smi())
    } else {
        None
    };

    targets
        .iter()
        .map(|t| match t.vendor.as_str() {
            "amd" => sample_amd(t),
            "nvidia" => sample_nvidia(t, nvidia_rows.as_deref()),
            other => {
                tracing::debug!(
                    vendor = other,
                    index = t.index,
                    "no VRAM sampler for vendor"
                );
                VramSample {
                    index: t.index,
                    used_mb: None,
                    free_mb: None,
                }
            }
        })
        .collect()
}

/// Last live-VRAM sample plus the single-flight guard bounding how many samplers can be
/// outstanding. Owned by `SessionManager` (agent.rs) so every site that re-detects
/// `vram_targets` can reach `invalidate()`.
#[derive(Default)]
pub struct VramCache {
    in_flight: AtomicBool,
    cache: Mutex<Option<Vec<VramSample>>>,
}

impl VramCache {
    pub fn new() -> Self {
        Self::default()
    }

    /// Claim the single in-flight slot. On `true` the caller MUST
    /// [`release`](Self::release) exactly once, on every exit path including a join error.
    /// On `false` a sample is already running and the caller must skip this tick.
    fn try_begin(&self) -> bool {
        self.in_flight
            .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
            .is_ok()
    }

    fn release(&self) {
        self.in_flight.store(false, Ordering::Release);
    }

    /// Called only from the one in-flight sampler `spawn_tick` spawned, so there is no
    /// concurrent writer and a slower older tick cannot clobber a newer one.
    fn store(&self, samples: Vec<VramSample>) {
        if let Ok(mut guard) = self.cache.lock() {
            *guard = Some(samples);
        }
    }

    /// Non-blocking read of the current cached sample for a heartbeat tick.
    pub fn read(&self) -> Option<Vec<VramSample>> {
        self.cache.try_lock().ok().and_then(|g| g.clone())
    }

    /// Must be called alongside every `vram_targets` reassignment (config_update, hotplug,
    /// post-session re-detect). `gpus.index` is a position over sorted cardN paths, so a
    /// GPU-set change shifts indices and a stale sample rides the next heartbeat attached
    /// to the wrong physical GPU.
    pub fn invalidate(&self) {
        if let Ok(mut guard) = self.cache.lock() {
            *guard = None;
        }
    }

    /// Single-flighted: a tick whose predecessor is still running (or hung) is a no-op and
    /// spawns nothing, so a hung sampler costs at most the one blocking-pool thread and
    /// child process the first tick claimed. The caller reads the cached sample instead.
    pub fn spawn_tick(self: &Arc<Self>, targets: Vec<VramTarget>) {
        if targets.is_empty() {
            return;
        }
        if !self.try_begin() {
            tracing::debug!(
                "vram sampler already in flight (previous tick still running); \
                 skipping this tick and reusing the cached sample"
            );
            return;
        }
        let cache = Arc::clone(self);
        tokio::spawn(async move {
            match tokio::task::spawn_blocking(move || sample(&targets)).await {
                Ok(samples) => cache.store(samples),
                Err(join_err) => {
                    tracing::warn!(
                        token = "vram-sampler-panicked",%join_err, "vram sampler task panicked/join failed");
                    // Leave the cache untouched: never write a fabricated value.
                }
            }
            cache.release();
        });
    }
}

/// AMD: `<sysfs_device>/mem_info_vram_used` (bytes), `free_mb = total - used`. Some AMD
/// firmware transiently reports used > total; a negative free must become `None`.
fn sample_amd(target: &VramTarget) -> VramSample {
    let used_mb = read_amd_used_mb(&target.sysfs_device);
    if used_mb.is_none() {
        warn_or_debug_once(
            "amd",
            &format!("{}/mem_info_vram_used", target.sysfs_device.display()),
        );
    }
    let free_mb = used_mb.and_then(|used| {
        let free = target.total_mb - used;
        (free >= 0).then_some(free)
    });
    VramSample {
        index: target.index,
        used_mb,
        free_mb,
    }
}

fn read_amd_used_mb(sysfs_device: &std::path::Path) -> Option<i32> {
    let text = std::fs::read_to_string(sysfs_device.join("mem_info_vram_used")).ok()?;
    parse_amd_vram_used_bytes(&text)
}

/// One integer of bytes into whole MB. Anything that does not parse cleanly is `None`,
/// never a fabricated `0`.
fn parse_amd_vram_used_bytes(text: &str) -> Option<i32> {
    let bytes: u64 = text.trim().parse().ok()?;
    Some((bytes / 1024 / 1024) as i32)
}

/// Spawn `cmd`, poll `try_wait`, and `kill()` + reap at the deadline, returning `None`.
/// Never `Command::output()`: it blocks until the child exits, which leaked a blocking-pool
/// thread and a zombie per heartbeat tick when `nvidia-smi` wedged on an Xid-faulted GPU.
/// `cmd` is a parameter so the mechanism can be tested against `sh`.
///
/// A GPU wedged in an uninterruptible driver wait can still resist `kill()`; the
/// single-flight guard in [`VramCache`] is what bounds that to ONE thread.
///
/// The crate's one bounded-exec primitive (#407) — the capacity path's `nvidia-smi` fork
/// runs on the agent's select-loop thread and reuses it rather than re-implementing it.
pub(crate) fn run_bounded(
    cmd: &str,
    args: &[&str],
    timeout: Duration,
) -> Option<(String, std::process::ExitStatus)> {
    let deadline = Instant::now() + timeout;
    let mut child = Command::new(cmd)
        .args(args)
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .ok()?;

    loop {
        match child.try_wait() {
            Ok(Some(status)) => {
                let mut stdout = String::new();
                if let Some(mut out) = child.stdout.take() {
                    let _ = out.read_to_string(&mut stdout);
                }
                return Some((stdout, status));
            }
            Ok(None) => {
                if Instant::now() >= deadline {
                    let _ = child.kill();
                    let _ = child.wait();
                    return None;
                }
                std::thread::sleep(Duration::from_millis(20));
            }
            Err(_) => return None,
        }
    }
}

/// ONE fork for every NVIDIA target, bounded by [`run_bounded`].
fn run_nvidia_smi() -> Vec<(String, i32, i32)> {
    match run_bounded(
        "nvidia-smi",
        &[
            "--query-gpu=pci.bus_id,memory.used,memory.free",
            "--format=csv,noheader,nounits",
        ],
        SAMPLE_TIMEOUT,
    ) {
        Some((stdout, status)) if status.success() => parse_nvidia_smi_used_free_csv(&stdout),
        Some((_, status)) => {
            warn_or_debug_once("nvidia", &format!("nvidia-smi exited with {status}"));
            Vec::new()
        }
        None => {
            warn_or_debug_once(
                "nvidia",
                &format!(
                    "nvidia-smi failed to spawn, or exceeded the {SAMPLE_TIMEOUT:?} sample timeout and was killed"
                ),
            );
            Vec::new()
        }
    }
}

/// Rows of `<bus_id>, <used_mb>, <free_mb>`. A malformed or short row is skipped, never
/// panics and never yields a fabricated value; targets no surviving row covers get `None`.
/// Does NOT filter a literal `0, 0` row — judging that needs `total_mb`, which only
/// [`sample_nvidia`] has.
fn parse_nvidia_smi_used_free_csv(text: &str) -> Vec<(String, i32, i32)> {
    let mut out = Vec::new();
    for line in text.lines() {
        let mut parts = line.splitn(3, ',');
        let addr = parts.next().map(str::trim).unwrap_or_default();
        let used = parts.next().and_then(|s| s.trim().parse::<i32>().ok());
        let free = parts.next().and_then(|s| s.trim().parse::<i32>().ok());
        if addr.is_empty() {
            continue;
        }
        if let (Some(used), Some(free)) = (used, free) {
            if used >= 0 && free >= 0 {
                out.push((addr.to_string(), used, free));
            }
        }
    }
    out
}

fn sample_nvidia(target: &VramTarget, rows: Option<&[(String, i32, i32)]>) -> VramSample {
    let none = VramSample {
        index: target.index,
        used_mb: None,
        free_mb: None,
    };
    let Some(rows) = rows else {
        return none;
    };
    let Some(pci_addr) = &target.pci_addr else {
        warn_or_debug_once("nvidia", "target has no pci_addr to match against");
        return none;
    };
    let normalized = crate::capacity::normalize_pci_addr(pci_addr);
    match rows
        .iter()
        .find(|(addr, _, _)| crate::capacity::normalize_pci_addr(addr) == normalized)
    {
        Some((_, used, free)) => {
            // A faulted GPU prints a literal `0, 0` forever, so accepting it stores
            // `vram_mb_free = 0`, admission reads "full", and the sample never goes stale:
            // the GPU is refused for every launch indefinitely. Scoped to a positive
            // detected total, per spec.
            if *used == 0 && *free == 0 && target.total_mb > 0 {
                warn_or_debug_once(
                    "nvidia",
                    &format!(
                        "nvidia-smi reported used=0 free=0 for pci_addr {pci_addr} \
                         (total_mb={}); treating as unknown, not a real zero reading",
                        target.total_mb
                    ),
                );
                none
            } else {
                VramSample {
                    index: target.index,
                    used_mb: Some(*used),
                    free_mb: Some(*free),
                }
            }
        }
        None => {
            warn_or_debug_once(
                "nvidia",
                &format!("no nvidia-smi row matched pci_addr {pci_addr}"),
            );
            none
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn target(
        index: i32,
        vendor: &str,
        sysfs_device: PathBuf,
        pci_addr: Option<&str>,
        total_mb: i32,
    ) -> VramTarget {
        VramTarget {
            index,
            vendor: vendor.to_string(),
            sysfs_device,
            pci_addr: pci_addr.map(str::to_string),
            total_mb,
        }
    }

    // ---- AMD sysfs path ----

    #[test]
    fn amd_sysfs_fixture_parses() {
        let dir = tempfile::tempdir().unwrap();
        let device_dir = dir.path().join("device");
        std::fs::create_dir_all(&device_dir).unwrap();
        // 2 GiB used, out of an 8 GiB total target.
        std::fs::write(device_dir.join("mem_info_vram_used"), "2147483648\n").unwrap();

        let t = target(0, "amd", device_dir, None, 8192);
        let samples = sample(std::slice::from_ref(&t));
        assert_eq!(samples.len(), 1);
        assert_eq!(samples[0].index, 0);
        assert_eq!(samples[0].used_mb, Some(2048));
        assert_eq!(samples[0].free_mb, Some(8192 - 2048));
    }

    #[test]
    fn amd_missing_file_yields_none_never_zero() {
        let dir = tempfile::tempdir().unwrap();
        let device_dir = dir.path().join("device");
        std::fs::create_dir_all(&device_dir).unwrap();
        let t = target(0, "amd", device_dir, None, 8192);
        let samples = sample(std::slice::from_ref(&t));
        assert_eq!(samples[0].used_mb, None);
        assert_eq!(samples[0].free_mb, None);
    }

    #[test]
    fn amd_malformed_content_yields_none() {
        let dir = tempfile::tempdir().unwrap();
        let device_dir = dir.path().join("device");
        std::fs::create_dir_all(&device_dir).unwrap();
        std::fs::write(device_dir.join("mem_info_vram_used"), "not-a-number\n").unwrap();

        let t = target(0, "amd", device_dir, None, 8192);
        let samples = sample(std::slice::from_ref(&t));
        assert_eq!(samples[0].used_mb, None);
        assert_eq!(samples[0].free_mb, None);
    }

    #[test]
    fn amd_used_above_total_yields_none_free_not_negative() {
        let dir = tempfile::tempdir().unwrap();
        let device_dir = dir.path().join("device");
        std::fs::create_dir_all(&device_dir).unwrap();
        std::fs::write(device_dir.join("mem_info_vram_used"), "9663676416\n").unwrap(); // 9 GiB

        let t = target(0, "amd", device_dir, None, 8192); // 8 GiB total
        let samples = sample(std::slice::from_ref(&t));
        assert_eq!(samples[0].used_mb, Some(9216));
        assert_eq!(
            samples[0].free_mb, None,
            "used > total must not yield a negative free_mb"
        );
    }

    #[test]
    fn parse_amd_vram_used_bytes_rejects_garbage() {
        assert_eq!(parse_amd_vram_used_bytes(""), None);
        assert_eq!(parse_amd_vram_used_bytes("abc\n"), None);
        assert_eq!(parse_amd_vram_used_bytes("-5\n"), None);
        assert_eq!(parse_amd_vram_used_bytes("1048576\n"), Some(1));
    }

    // ---- NVIDIA CSV path ----

    #[test]
    fn nvidia_csv_parses_multi_gpu_and_matches_by_bus_id() {
        let csv = "00000000:01:00.0, 2048, 30720\n00000000:41:00.0, 1024, 15360\n";
        let rows = parse_nvidia_smi_used_free_csv(csv);
        assert_eq!(rows.len(), 2);

        let targets = [
            target(0, "nvidia", PathBuf::new(), Some("0000:01:00.0"), 32768),
            target(1, "nvidia", PathBuf::new(), Some("0000:41:00.0"), 16384),
        ];
        let s0 = sample_nvidia(&targets[0], Some(&rows));
        let s1 = sample_nvidia(&targets[1], Some(&rows));
        assert_eq!(s0.used_mb, Some(2048));
        assert_eq!(s0.free_mb, Some(30720));
        assert_eq!(s1.used_mb, Some(1024));
        assert_eq!(s1.free_mb, Some(15360));
    }

    #[test]
    fn nvidia_csv_malformed_or_empty_yields_none_never_panics() {
        for csv in [
            "",
            "garbage line with no commas\n",
            "0000:01:00.0, notanumber, 100\n",
            "0000:01:00.0, -1, 100\n",
        ] {
            let rows = parse_nvidia_smi_used_free_csv(csv);
            let t = target(0, "nvidia", PathBuf::new(), Some("0000:01:00.0"), 8192);
            let s = sample_nvidia(&t, Some(&rows));
            assert_eq!(s.used_mb, None, "csv={csv:?}");
            assert_eq!(s.free_mb, None, "csv={csv:?}");
        }
    }

    #[test]
    fn nvidia_no_pci_addr_on_target_yields_none() {
        let rows = parse_nvidia_smi_used_free_csv("0000:01:00.0, 2048, 30720\n");
        let t = target(0, "nvidia", PathBuf::new(), None, 32768);
        let s = sample_nvidia(&t, Some(&rows));
        assert_eq!(s.used_mb, None);
        assert_eq!(s.free_mb, None);
    }

    #[test]
    fn nvidia_no_rows_available_yields_none() {
        let t = target(0, "nvidia", PathBuf::new(), Some("0000:01:00.0"), 32768);
        let s = sample_nvidia(&t, None);
        assert_eq!(s.used_mb, None);
        assert_eq!(s.free_mb, None);
    }

    #[test]
    fn unrecognized_vendor_yields_none_not_zero() {
        let t = target(0, "intel", PathBuf::new(), None, 8192);
        let samples = sample(std::slice::from_ref(&t));
        assert_eq!(samples[0].used_mb, None);
        assert_eq!(samples[0].free_mb, None);
    }

    // ---- both-zero is not a real reading ----

    #[test]
    fn nvidia_both_zero_on_a_gpu_with_positive_total_yields_none() {
        let rows = parse_nvidia_smi_used_free_csv("00000000:01:00.0, 0, 0\n");
        let t = target(0, "nvidia", PathBuf::new(), Some("0000:01:00.0"), 32768);
        let s = sample_nvidia(&t, Some(&rows));
        assert_eq!(s.used_mb, None);
        assert_eq!(s.free_mb, None);
    }

    #[test]
    fn nvidia_both_zero_on_a_gpu_with_nonpositive_total_is_reported_as_is() {
        // The override is scoped to a positive total; total_mb <= 0 is unaffected.
        let rows = parse_nvidia_smi_used_free_csv("00000000:01:00.0, 0, 0\n");
        let t = target(0, "nvidia", PathBuf::new(), Some("0000:01:00.0"), 0);
        let s = sample_nvidia(&t, Some(&rows));
        assert_eq!(s.used_mb, Some(0));
        assert_eq!(s.free_mb, Some(0));
    }

    #[test]
    fn nvidia_nonzero_reading_on_a_positive_total_gpu_is_unaffected() {
        let rows = parse_nvidia_smi_used_free_csv("00000000:01:00.0, 0, 32768\n");
        let t = target(0, "nvidia", PathBuf::new(), Some("0000:01:00.0"), 32768);
        let s = sample_nvidia(&t, Some(&rows));
        assert_eq!(s.used_mb, Some(0));
        assert_eq!(s.free_mb, Some(32768));
    }

    // ---- wire shape ----

    #[test]
    fn vram_sample_omits_none_fields_on_the_wire() {
        let s = VramSample {
            index: 2,
            used_mb: None,
            free_mb: None,
        };
        let json = serde_json::to_value(&s).unwrap();
        let obj = json.as_object().unwrap();
        assert_eq!(obj["index"], 2);
        assert!(!obj.contains_key("used_mb"));
        assert!(!obj.contains_key("free_mb"));
    }

    #[test]
    fn vram_sample_includes_present_fields_on_the_wire() {
        let s = VramSample {
            index: 0,
            used_mb: Some(512),
            free_mb: Some(7680),
        };
        let json = serde_json::to_value(&s).unwrap();
        assert_eq!(json["used_mb"], 512);
        assert_eq!(json["free_mb"], 7680);
    }

    // ---- warn-once is per vendor ----

    #[test]
    fn warn_once_is_keyed_per_vendor() {
        // Test-only vendor strings: WARNED_VENDORS is process-wide and tests share it.
        assert!(is_first_failure("test-vendor-a-383"));
        assert!(
            !is_first_failure("test-vendor-a-383"),
            "second failure for the same vendor must not be 'first' again"
        );
        assert!(
            is_first_failure("test-vendor-b-383"),
            "a different vendor's first failure must still be treated as first \
             (i.e. warn-level), not demoted by an unrelated vendor's prior failure"
        );
    }

    // ---- the fork is killed, not just awaited ----

    #[test]
    fn run_bounded_kills_and_reaps_a_process_exceeding_the_deadline() {
        // Returning well under the child's own 5s sleep proves it was killed. A generous
        // upper bound, not a timing-equality assertion.
        let start = Instant::now();
        let result = run_bounded("sh", &["-c", "sleep 5"], Duration::from_millis(150));
        let elapsed = start.elapsed();
        assert!(
            result.is_none(),
            "a killed process must yield None, not a stale/partial reading"
        );
        assert!(
            elapsed < Duration::from_secs(2),
            "run_bounded must not wait out the full child sleep; took {elapsed:?}"
        );
    }

    #[test]
    fn run_bounded_returns_output_for_a_fast_process() {
        let result = run_bounded("sh", &["-c", "echo hello"], Duration::from_secs(2));
        let (stdout, status) = result.expect("a fast process should complete normally");
        assert!(status.success());
        assert_eq!(stdout.trim(), "hello");
    }

    // ---- VramCache: single-flight + cache invalidation ----

    #[test]
    fn vram_cache_try_begin_is_single_flight() {
        let cache = VramCache::new();
        assert!(cache.try_begin(), "first claim must succeed");
        assert!(
            !cache.try_begin(),
            "a second claim while one is in flight must be refused"
        );
        cache.release();
        assert!(cache.try_begin(), "after release, a new claim must succeed");
    }

    #[tokio::test]
    async fn spawn_tick_skips_and_spawns_nothing_when_already_in_flight() {
        let cache = Arc::new(VramCache::new());
        assert!(cache.try_begin(), "simulate a sample already running");

        let dir = tempfile::tempdir().unwrap();
        let device_dir = dir.path().join("device");
        std::fs::create_dir_all(&device_dir).unwrap();
        std::fs::write(device_dir.join("mem_info_vram_used"), "1048576\n").unwrap();
        let targets = vec![target(0, "amd", device_dir, None, 8192)];

        cache.spawn_tick(targets);
        // A wrongly-spawned second sampler would write the cache within this window.
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert_eq!(
            cache.read(),
            None,
            "a tick observed while already in flight must never spawn a second \
             sampler or write the cache"
        );
        cache.release();
    }

    #[tokio::test]
    async fn spawn_tick_samples_and_populates_the_cache_when_idle() {
        let cache = Arc::new(VramCache::new());
        let dir = tempfile::tempdir().unwrap();
        let device_dir = dir.path().join("device");
        std::fs::create_dir_all(&device_dir).unwrap();
        std::fs::write(device_dir.join("mem_info_vram_used"), "1048576\n").unwrap();
        let targets = vec![target(0, "amd", device_dir, None, 8192)];

        cache.spawn_tick(targets);
        // Bounded wait, not a timing-equality assertion.
        for _ in 0..50 {
            if cache.read().is_some() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        let samples = cache.read().expect("sample should have completed");
        assert_eq!(samples[0].used_mb, Some(1));
    }

    #[test]
    fn vram_cache_invalidate_clears_a_cached_sample() {
        let cache = VramCache::new();
        cache.store(vec![VramSample {
            index: 0,
            used_mb: Some(1),
            free_mb: Some(2),
        }]);
        assert!(cache.read().is_some());
        cache.invalidate();
        assert_eq!(cache.read(), None);
    }
}
