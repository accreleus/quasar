//! Every engine call on the readiness refresh path is budgeted (#274).
//!
//! The refresh is what produces the capacity report, and the control plane stops
//! trusting a report once it is older than `QUASAR_READINESS_STALE_SECS` (default
//! 60 s). One engine call left on the client's full 30 s deadline therefore spends
//! half that window on its own, and a hung daemon — socket accepts, daemon never
//! answers — pays the deadline in full rather than failing fast.
//!
//! This was not hypothetical twice over. The original #274 report took ~115 s
//! because four such calls ran in series. The first fix gated three of them and
//! missed `detect_firewall_posture`'s `inspect_container`, which on hardware still
//! cost a flat 30 s (measured 44-50 s to the failing report). A reviewer cannot see
//! an ungated call by reading a diff of the file that added it, so the rule is
//! enforced here instead: on this path, name the budgeted form or don't call the
//! engine.
//!
//! Skipping the refresh's engine work when it has already proved unusable
//! (`RuntimeView::engine_answered`) is the other half, and it is NOT sufficient by
//! itself: a refresh that began before the freeze saw the engine answer, so it
//! takes the unskipped path through every collector. Only the budget bounds that
//! one.

use std::path::{Path, PathBuf};

/// The files whose engine calls a readiness refresh pays for, and why each is here:
/// `readiness.rs` and `readiness/*` are the probe itself; `images/disk.rs` is
/// `StorageView::live`'s image-root lookup, which is only ever reached from it.
const REFRESH_PATH: &[&str] = &[
    "readiness.rs",
    "readiness/runtime_facts.rs",
    "readiness/storage.rs",
    "readiness/platform_update.rs",
    "readiness/report.rs",
    "images/disk.rs",
];

/// The one function on the refresh path that lives outside those files: the sibling
/// EGL probe, whose `own_image` call runs BEFORE its own 60 s result cache, so the
/// cache does not protect it — and whose container lifecycle runs AFTER a cache MISS,
/// which the cache does not protect either (#283). Checked by line rather than by file,
/// because the rest of `nvidia_volume.rs` is provisioning, not the report path.
const EGL_PROBE_FILE: &str = "nvidia_volume.rs";

/// Owned-probe lifecycle operations that default to the client's full deadline, one
/// deadline EACH. `RuntimeClient::gpu_probe_within` runs the whole lifecycle under one
/// caller-chosen budget; in this function (the launch gate's driver check since #259,
/// and a refresh-path call before it), name that or don't drive a probe.
const UNBUDGETED_PROBE_LIFECYCLE: &[&str] = &[
    ".recover_diagnostics(",
    ".run_gpu_probe(",
    ".observe_gpu_probe(",
    ".stop_gpu_probe(",
    ".cleanup_gpu_probe(",
];

/// Engine reads that default to the client's full deadline. Each has a `_within`
/// twin taking an explicit budget; the refresh path must use that one.
const UNBUDGETED_CALLS: &[&str] = &[
    ".inspect_container(",
    ".engine_storage(",
    ".live_containers(",
];

fn src_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("src")
}

/// A file's production half: everything before its first `#[cfg(test)]`. A test
/// module may drive the engine however it likes.
fn production(rel: &str) -> String {
    let text = std::fs::read_to_string(src_root().join(rel))
        .unwrap_or_else(|e| panic!("read {rel}: {e} (has the refresh path been renamed?)"));
    text.split("#[cfg(test)]").next().unwrap_or("").to_owned()
}

#[test]
fn every_engine_call_on_the_readiness_refresh_path_is_budgeted() {
    let mut bad = Vec::new();
    for rel in REFRESH_PATH {
        let text = production(rel);
        for (i, line) in text.lines().enumerate() {
            for call in UNBUDGETED_CALLS {
                // `inspect_container_within(` contains `inspect_container(` only if the
                // `_within` is absent, because the open paren has to follow the name.
                if line.contains(call) {
                    bad.push(format!("{rel}:{}: {}", i + 1, line.trim()));
                }
            }
        }
    }
    assert!(
        bad.is_empty(),
        "a readiness refresh must not make an engine call bounded only by the client's \
         full deadline — use the `_within` form with \
         `crate::runtime::ENGINE_INSPECTION_BUDGET`:\n{}",
        bad.join("\n")
    );
}

/// The sibling EGL probe asks the engine for the agent's own image before it can even
/// consult its cache, so that call is on the report path and must carry a budget.
#[test]
fn the_sibling_egl_probes_own_image_lookup_is_budgeted() {
    let text = production(EGL_PROBE_FILE);
    let bad: Vec<_> = text
        .lines()
        .enumerate()
        .filter(|(_, line)| line.contains(".own_image()"))
        .map(|(i, line)| format!("{EGL_PROBE_FILE}:{}: {}", i + 1, line.trim()))
        .collect();
    assert!(
        bad.is_empty(),
        "the EGL probe runs inside a readiness refresh; use \
         `own_image_within(ENGINE_INSPECTION_BUDGET)`:\n{}",
        bad.join("\n")
    );
}

/// #283: after the cache MISSES, the probe creates, starts, waits on and removes a real
/// container. Each of those was submitted on its own, so each carried the client's full
/// deadline and a daemon that wedged mid-refresh could be paid for five of them — one
/// measured full deadline for a create whose reply never comes, another for a wait on a
/// container that never exits. `own_image` being budgeted (above) does not help here: it
/// has already returned by the time the cache misses.
#[test]
fn the_sibling_egl_probes_container_lifecycle_is_budgeted() {
    let text = production(EGL_PROBE_FILE);
    let mut bad = Vec::new();
    for (i, line) in text.lines().enumerate() {
        for call in UNBUDGETED_PROBE_LIFECYCLE {
            if line.contains(call) {
                bad.push(format!("{EGL_PROBE_FILE}:{}: {}", i + 1, line.trim()));
            }
        }
    }
    assert!(
        bad.is_empty(),
        "the EGL probe's container lifecycle must run under ONE budget — use \
         `gpu_probe_within(.., crate::runtime::GPU_PROBE_LIFECYCLE_BUDGET)`:\n{}",
        bad.join("\n")
    );
    assert!(
        text.contains("gpu_probe_within("),
        "the EGL probe no longer drives an owned probe at all; if that is deliberate, \
         retire this test with the call rather than leaving it passing vacuously"
    );
}

/// The lifecycle budget is the number that bound is worth, and it is sized so a HEALTHY
/// probe cannot trip it: the probe container self-limits with `timeout 20s`, so anything
/// at or below that turns slow-but-working hosts indeterminate. `nvidia_volume.rs` holds
/// the arithmetic tests; this one guards the constant against a silent edit.
#[test]
fn the_gpu_probe_lifecycle_budget_stays_where_the_arithmetic_put_it() {
    let text = production("runtime.rs");
    assert!(
        text.contains("pub const GPU_PROBE_LIFECYCLE_BUDGET: Duration = Duration::from_secs(30)"),
        "GPU_PROBE_LIFECYCLE_BUDGET must clear the probe container's own 20 s self-limit \
         and still leave the refresh path inside its 60 s deadline; changing it needs the \
         arithmetic in nvidia_volume.rs revisited in the same commit"
    );
}

/// The budget is the number the bound above is worth. Keep it small enough that a
/// whole refresh's worth of them still lands inside the staleness window.
#[test]
fn the_engine_inspection_budget_stays_small() {
    // Defined by the shared runtime crate (#355); the agent re-exports it.
    let text = production("../crates/quasar-runtime/src/client.rs");
    assert!(
        text.contains("pub const ENGINE_INSPECTION_BUDGET: Duration = Duration::from_secs(5)"),
        "ENGINE_INSPECTION_BUDGET is the one knob the refresh-path bound rests on; \
         changing it needs the staleness arithmetic in readiness/tests/runtime_checks.rs \
         revisited in the same commit"
    );
}
