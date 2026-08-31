//! Vulkan device-fault classification + GPU-global fault detection (multi-session
//! spec §2b: the per-session Vulkan failure domain).
//!
//! Two pure, GPU-free pieces, unit-testable without a device:
//! - [`is_device_lost`] classifies a GStreamer `Error`'s message/debug text as a
//!   `VK_ERROR_DEVICE_LOST` signature, mapped to a **per-session**
//!   [`crate::session::runner::SessionEvent::Failed`], never a process abort. Per-element
//!   Vulkan devices (§2a) mean a lost device is retired by tearing down its session alone.
//! - [`GpuGlobalFaultDetector`] counts device-lost failures **across** sessions: ≥2 inside
//!   [`GPU_GLOBAL_WINDOW`] — or one pre-session device-open failure
//!   ([`DEVICE_OPEN_FAILED_REASON`]) — mean the GPU itself faulted (Xid 79 /
//!   fallen-off-the-bus), since independent per-element devices don't die simultaneously.
//!   The agent then drains and guarded-restarts the process rather than flapping each
//!   session. A lone per-session `DEVICE_LOST` stays per-session.

use std::time::{Duration, Instant};

/// Stable reason prefix a runner emits for a **per-session** Vulkan device loss.
/// The agent-side [`GpuGlobalFaultDetector`] matches [`reason_is_device_lost`] on
/// this to feed the ≥2-in-window counter without re-parsing raw driver text.
pub const DEVICE_LOST_REASON: &str = "vulkan device lost";

/// Reason prefix for a **pre-session** Vulkan device-open failure: the device is
/// unusable before any session owns it (e.g. `vkDeviceWaitIdle` fails on a fresh
/// device at session build). Treated as GPU-global immediately (no ≥2-in-window
/// needed) — a device that will not even open means the GPU is faulted.
pub const DEVICE_OPEN_FAILED_REASON: &str = "vulkan device open failed";

/// Case-insensitive substring signatures of `VK_ERROR_DEVICE_LOST`. Matched
/// **defensively** (lowercase + `contains`, not one pinned string) — wrapping varies by
/// element, gst version, and driver.
const DEVICE_LOST_SIGNATURES: &[&str] = &["device lost", "device_lost"];

/// True iff `text` (a gst `Error`'s `error()`+`debug()`, or an anyhow `{:#}` chain)
/// carries a `VK_ERROR_DEVICE_LOST` signature.
pub fn is_device_lost(text: &str) -> bool {
    let haystack = text.to_ascii_lowercase();
    DEVICE_LOST_SIGNATURES
        .iter()
        .any(|sig| haystack.contains(sig))
}

/// Format the canonical per-session device-lost failure reason from a gst error's
/// display + debug text. The result begins with [`DEVICE_LOST_REASON`] so the
/// agent-side detector recognizes it.
pub fn device_lost_reason(error: &str, debug: &str) -> String {
    format!("{DEVICE_LOST_REASON}: {error} ({debug})")
}

/// Classify a session-build failure: a device-lost `err_text` returns the GPU-global
/// [`DEVICE_OPEN_FAILED_REASON`]-prefixed reason, else the caller's generic
/// `context: err_text` (per-session, no restart). Gate the call site on a Vulkan
/// session so non-Vulkan build errors never reach here.
pub fn build_failure_reason(context: &str, err_text: &str) -> String {
    if is_device_lost(err_text) {
        format!("{DEVICE_OPEN_FAILED_REASON}: {context}: {err_text}")
    } else {
        format!("{context}: {err_text}")
    }
}

/// True iff a [`SessionEvent::Failed`](crate::session::runner::SessionEvent) reason
/// is a per-session Vulkan device-loss failure (from [`device_lost_reason`]). Excludes
/// the pre-session device-open case, which contains the same words but is an
/// immediate GPU-global (see [`reason_is_device_open_failed`]).
pub fn reason_is_device_lost(reason: &str) -> bool {
    let lower = reason.to_ascii_lowercase();
    lower.contains(DEVICE_LOST_REASON) && !lower.contains(DEVICE_OPEN_FAILED_REASON)
}

/// True iff a `Failed` reason is the pre-session device-open failure
/// ([`DEVICE_OPEN_FAILED_REASON`]) — an immediate GPU-global signal.
pub fn reason_is_device_open_failed(reason: &str) -> bool {
    reason
        .to_ascii_lowercase()
        .contains(DEVICE_OPEN_FAILED_REASON)
}

/// GPU-global fault window (spec §2b). ≥2 device losses within this span, on distinct
/// per-element devices, indicate the GPU reset, not coincidental independent deaths.
pub const GPU_GLOBAL_WINDOW: Duration = Duration::from_secs(10);

/// Bounded drain before the guarded restart: after a GPU-global fault the agent stops
/// accepting sessions, force-stops running ones, then restarts the process once
/// in-flight sessions have had this long to unwind (or times them out).
pub const GPU_GLOBAL_DRAIN_TIMEOUT: Duration = Duration::from_secs(15);

/// Counts recent per-session device-lost failures to distinguish a per-session loss
/// (retire only that session) from a GPU-global reset (drain + guarded restart). Pure
/// logic, no GPU or clock ownership (caller passes `now`).
#[derive(Default)]
pub struct GpuGlobalFaultDetector {
    recent: Vec<Instant>,
}

impl GpuGlobalFaultDetector {
    /// Record one device-lost failure at `now`, pruning entries older than
    /// [`GPU_GLOBAL_WINDOW`], and return `true` iff ≥2 now fall inside the window.
    pub fn record_device_lost(&mut self, now: Instant) -> bool {
        self.recent
            .retain(|t| now.duration_since(*t) <= GPU_GLOBAL_WINDOW);
        self.recent.push(now);
        self.recent.len() >= 2
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn classifies_representative_device_lost_strings() {
        assert!(is_device_lost("VK_ERROR_DEVICE_LOST"));
        assert!(is_device_lost(
            "vkQueueSubmit returned VK_ERROR_DEVICE_LOST"
        ));
        assert!(is_device_lost("Vulkan error: device lost"));
        assert!(is_device_lost("ErrorDeviceLost (device_lost)"));
        // Case-insensitive.
        assert!(is_device_lost("Device Lost while flushing queue"));
        assert!(is_device_lost("vk_error_device_lost"));
    }

    #[test]
    fn does_not_classify_unrelated_errors() {
        for s in [
            "encode pipeline error: Internal data stream error",
            "container launch failed: no such image",
            "not-negotiated: caps mismatch",
            "sctp association shutdown",
            "out of device memory", // NOT device lost
            "",
        ] {
            assert!(!is_device_lost(s), "false positive on {s:?}");
        }
    }

    #[test]
    fn device_lost_reason_is_recognized_as_per_session() {
        let reason = device_lost_reason("VK_ERROR_DEVICE_LOST", "gstvulkanh264enc:enc");
        assert!(reason.starts_with(DEVICE_LOST_REASON));
        assert!(reason_is_device_lost(&reason));
        assert!(!reason_is_device_open_failed(&reason));
    }

    #[test]
    fn build_failure_classifies_device_open_vs_generic() {
        let open = build_failure_reason(
            "encode set READY for Vulkan device verification",
            "VK_ERROR_DEVICE_LOST",
        );
        assert!(reason_is_device_open_failed(&open));
        // Excluded so it isn't double-counted in the per-session ≥2-window path.
        assert!(!reason_is_device_lost(&open));

        let generic = build_failure_reason("build encode pipeline", "no element \"vulkanh264enc\"");
        assert!(!reason_is_device_open_failed(&generic));
        assert!(!reason_is_device_lost(&generic));
    }

    #[test]
    fn detector_single_loss_is_per_session() {
        let mut d = GpuGlobalFaultDetector::default();
        assert!(!d.record_device_lost(Instant::now()));
    }

    #[test]
    fn detector_two_within_window_is_gpu_global() {
        let mut d = GpuGlobalFaultDetector::default();
        let t0 = Instant::now();
        assert!(!d.record_device_lost(t0));
        assert!(d.record_device_lost(t0 + Duration::from_secs(1)));
    }

    #[test]
    fn detector_two_outside_window_stays_per_session() {
        let mut d = GpuGlobalFaultDetector::default();
        let t0 = Instant::now();
        assert!(!d.record_device_lost(t0));
        // Past the window: the first is pruned, so this is again a lone loss.
        assert!(!d.record_device_lost(t0 + GPU_GLOBAL_WINDOW + Duration::from_secs(1)));
    }

    #[test]
    fn detector_three_rapid_stays_tripped() {
        let mut d = GpuGlobalFaultDetector::default();
        let t0 = Instant::now();
        assert!(!d.record_device_lost(t0));
        assert!(d.record_device_lost(t0 + Duration::from_secs(1)));
        assert!(d.record_device_lost(t0 + Duration::from_secs(2)));
    }

    /// Spec §2b item 3 + §7: no `process::exit` / `exit(75)` may survive in the
    /// session teardown paths. A cheap source-level audit baked into the test
    /// suite (the branch's `source.rs` `exit(75)`-in-`Drop` is banned).
    #[test]
    fn no_process_exit_in_session_teardown_sources() {
        for (name, src) in [
            ("runner.rs", include_str!("runner.rs")),
            ("source.rs", include_str!("source.rs")),
        ] {
            assert!(
                !src.contains("exit(75)"),
                "{name} must not contain exit(75) (ring-gate process-suicide is banned)"
            );
            assert!(
                !src.contains("process::exit"),
                "{name} must not call process::exit in a session/teardown path"
            );
        }
    }
}
