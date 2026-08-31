//! Console-mode hardware hotplug watcher (agent half).
//!
//! The control plane auto-starts/stops a console session by diffing connectors in
//! the agent's last-reported `capacity.console_capabilities` (agent-api.md), so the
//! agent must re-send its capacity report whenever console-relevant hardware
//! changes (display connect/disconnect, input device/controller hotplug).
//!
//! Polling, not a `udev` netlink monitor: `udev` isn't a crate dependency and a
//! hotplug event isn't latency-sensitive, so a dedicated thread re-reads the same
//! sysfs/procfs sources `capacity::detect_console_capabilities` uses every
//! [`POLL_INTERVAL`] and diffs the snapshot.
//!
//! Debounce: a change must repeat on two consecutive polls before it fires — since
//! [`POLL_INTERVAL`] (2s) already exceeds the desired 500ms quiet window, one
//! repeat is enough to coalesce a hub replug burst into one capacity re-send.
//!
//! This thread's tick also drives a coarser ~60s storage-availability recheck
//! (`STORAGE_POLL_TICKS`), since availability drifts continuously and needs its own
//! delta threshold ([`STORAGE_DELTA_MB`]/[`STORAGE_DELTA_FRACTION`]) rather than the
//! exact-match debounce above; not worth a second poll thread for a signal this cheap.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use tokio::sync::mpsc;
use tracing::info;

use crate::capacity;
use crate::messages::{ConsoleCapabilities, StorageVolume};

const POLL_INTERVAL: Duration = Duration::from_millis(2000);

/// Recheck storage every this-many ticks (~60s at 2s/tick).
const STORAGE_POLL_TICKS: u32 = 30;
/// Minimum delta (absolute MB) on any volume field worth a re-send.
const STORAGE_DELTA_MB: i64 = 1024;
/// Minimum delta (fraction of total) on any volume field worth a re-send.
const STORAGE_DELTA_FRACTION: f64 = 0.01;

/// RAII handle for the background poll thread: dropping it (on connection
/// teardown) sets the stop flag and joins the thread, so no watcher outlives
/// the WebSocket connection it reports changes for.
pub struct ConsoleHotplugWatcher {
    stop: Arc<AtomicBool>,
    // Held only for Drop's join(); never read directly (RAII holder, like
    // `agent::GcGuard`).
    #[allow(dead_code)]
    handle: std::thread::JoinHandle<()>,
}

impl ConsoleHotplugWatcher {
    /// Spawn the poll thread. On a debounced change, `reason` is sent on `tx` so
    /// the agent's connection loop re-sends a fresh capacity report. `tx` is
    /// `tokio::sync::mpsc` since a plain `std::thread` feeds an async `select!`.
    pub fn spawn(tx: mpsc::Sender<String>) -> Self {
        let stop = Arc::new(AtomicBool::new(false));
        let stop2 = stop.clone();
        let handle = std::thread::Builder::new()
            .name("quasar-console-hotplug".to_string())
            .spawn(move || run(stop2, tx))
            .expect("failed to spawn console-hotplug watcher thread");
        ConsoleHotplugWatcher { stop, handle }
    }
}

impl Drop for ConsoleHotplugWatcher {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        // Best-effort: the thread wakes from sleep within POLL_INTERVAL and exits
        // on its own; teardown does not block on a join.
    }
}

fn run(stop: Arc<AtomicBool>, tx: mpsc::Sender<String>) {
    // Baseline at watcher start; a mismatch against the agent's own initial
    // report costs at most one redundant early re-send.
    let mut last_reported = capacity::detect_console_capabilities();
    let mut prev_poll: Option<ConsoleCapabilities> = None;
    let mut last_reported_storage = capacity::detect_storage();
    let mut ticks: u32 = 0;

    while !stop.load(Ordering::Relaxed) {
        std::thread::sleep(POLL_INTERVAL);
        if stop.load(Ordering::Relaxed) {
            break;
        }

        let current = capacity::detect_console_capabilities();

        if let Some(prev) = &prev_poll {
            if *prev == current && current != last_reported {
                let reason = describe_change(&last_reported, &current);
                info!("console hotplug: {reason}, re-sending capacity");
                match tx.try_send(reason) {
                    Ok(()) | Err(mpsc::error::TrySendError::Full(_)) => {}
                    Err(mpsc::error::TrySendError::Closed(_)) => return,
                }
                last_reported = current.clone();
            }
        }
        prev_poll = Some(current);

        ticks += 1;
        if ticks >= STORAGE_POLL_TICKS {
            ticks = 0;
            let current_storage = capacity::detect_storage();
            if storage_delta_material(&last_reported_storage, &current_storage) {
                info!("host-observability: storage delta exceeds threshold, re-sending capacity");
                match tx.try_send("storage delta".to_string()) {
                    Ok(()) | Err(mpsc::error::TrySendError::Full(_)) => {}
                    Err(mpsc::error::TrySendError::Closed(_)) => return,
                }
                // Moves only on an actual re-send, else a slow steady writer
                // (each delta under threshold) would never trigger a report.
                last_reported_storage = current_storage;
            }
        }
    }
}

/// True if any volume present in both snapshots changed by at least
/// [`STORAGE_DELTA_MB`] or [`STORAGE_DELTA_FRACTION`] on `total_mb`/`available_mb`,
/// or if the volume set itself changed.
fn storage_delta_material(before: &[StorageVolume], after: &[StorageVolume]) -> bool {
    if before.len() != after.len() {
        return true;
    }
    for a in after {
        let Some(b) = before.iter().find(|b| b.label == a.label) else {
            return true; // a volume appeared that wasn't reported before
        };
        if field_delta_material(b.total_mb, a.total_mb, a.total_mb)
            || field_delta_material(b.available_mb, a.available_mb, a.total_mb)
        {
            return true;
        }
    }
    false
}

/// True if `before` -> `after` (both MB) differ by at least the absolute or
/// relative-to-`total_mb` threshold.
fn field_delta_material(before: i64, after: i64, total_mb: i64) -> bool {
    let delta = (after - before).abs();
    if delta >= STORAGE_DELTA_MB {
        return true;
    }
    if total_mb <= 0 {
        return false;
    }
    (delta as f64 / total_mb as f64) >= STORAGE_DELTA_FRACTION
}

/// Human-readable summary of what changed between two capability snapshots,
/// for the log line accompanying a hotplug-triggered capacity re-send.
fn describe_change(before: &ConsoleCapabilities, after: &ConsoleCapabilities) -> String {
    let mut parts = Vec::new();
    if before.connectors != after.connectors {
        parts.push(format!(
            "connectors changed {:?} -> {:?}",
            before.connectors, after.connectors
        ));
    }
    if before.input_devices.len() != after.input_devices.len() {
        parts.push(format!(
            "input devices changed {} -> {}",
            before.input_devices.len(),
            after.input_devices.len()
        ));
    }
    if before.audio_sinks.len() != after.audio_sinks.len() {
        parts.push(format!(
            "audio sinks changed {} -> {}",
            before.audio_sinks.len(),
            after.audio_sinks.len()
        ));
    }
    if parts.is_empty() {
        "console capabilities changed".to_string()
    } else {
        parts.join(", ")
    }
}
