//! WP5 — the golden-home warm-up as a **background-jobs framework runner**.
//!
//! Design of record: `docs/design/plans/2026-08-12-jobs-framework-and-viewer.md`
//! §8.3 (the adoption), §4.1 (the `JobRunner` seam), §3.4 (deferral + persisted
//! backoff). The framework owns the pending set (`job_runs` row), the backoff
//! ladder (`attempt` + `scheduled_for`), the wait (the dispatcher's tick), and
//! the image-`ready` trigger (`internal/images/ensure.go`
//! `Ensurer.AgentImageState`).
//!
//! **The #489 gate must never change here.** [`WarmupControl::try_acquire`] is
//! called with the same arguments as before: the host-global lock, the
//! zero-live-session precondition with its [`POST_SESSION_QUIET`] margin, and
//! `abort_for_user_launch` observed at every phase boundary inside
//! [`WarmupJob::run`].
//!
//! The framework REQUESTS; the job DECIDES. A refusal is not overridden by a
//! schedule or an admin's "Run now" — it becomes [`JobOutcome::Deferred`] with
//! the reason, the first time an operator can see *why* a warm-up did not
//! happen.

use std::sync::Arc;

use serde::Deserialize;
use serde_json::{json, Value};
use tracing::{debug, info, warn};

use crate::jobs::{AbortFlag, JobOutcome, JobRunner};

use super::{
    log_outcome, AbortCause, GateRefusal, HostActivity, SystemClock, TemplateStoreApi,
    WarmupConfig, WarmupControl, WarmupError, WarmupHost, WarmupJob, WarmupRequest,
    POST_SESSION_QUIET, WARMUP_CROSSFS_REASON, WARMUP_DISABLED_REASON,
};

/// `QUASAR_TEMPLATE_ALLOW_CROSSFS=1` overrides the split-namespace refusal, for
/// the one legitimate case: an operator who deliberately put the template root
/// on a separate mount that IS bind-mounted, and accepts the §4.2 tier-2 copy.
fn allow_cross_filesystem() -> bool {
    matches!(
        std::env::var("QUASAR_TEMPLATE_ALLOW_CROSSFS")
            .map(|v| v.trim().to_ascii_lowercase())
            .ok()
            .as_deref(),
        Some("1") | Some("true") | Some("yes") | Some("on")
    )
}

/// The framework-wide id for this job. It is the `jobs` row key, the admin API
/// path segment and the log field, so it is never renamed — see the design's
/// job-id vocabulary.
pub const JOB_ID: &str = "template.warmup";

/// The `params` blob the control plane stored when it materialized the run,
/// built at `Ensurer.AgentImageState` from the row that just reached `ready` —
/// carries exactly the three fields [`WarmupRequest`] needs. A run with no
/// usable params is a WIRING failure, not a skip: silent no-ops are invisible.
#[derive(Debug, Deserialize)]
struct WarmupParams {
    #[serde(default)]
    image_id: String,
    #[serde(default)]
    registry_ref: String,
    #[serde(default)]
    version: String,
}

/// Runs one warm-up when the control plane asks for one. Owns no scheduling
/// state — it holds only the collaborators a run needs, all per-connection and
/// shared with the agent loop.
pub struct WarmupJobRunner {
    cfg: WarmupConfig,
    /// `None` when this host has no usable template store (feature off or
    /// `QUASAR_TEMPLATE_ROOT` misconfigured). Still registered, reporting
    /// `skipped` with the reason — "not configured" must not be
    /// indistinguishable from "configured, nothing to do".
    store: Option<Arc<dyn TemplateStoreApi>>,
    host: Arc<dyn WarmupHost>,
    control: Arc<WarmupControl>,
    activity: Arc<HostActivity>,
    app_uid_gid: Option<(u32, u32)>,
    /// Injected so the whole runner — gate, phases, timeouts — is unit tested
    /// without sleeping.
    clock: Arc<dyn super::Clock>,
}

impl WarmupJobRunner {
    pub fn new(
        cfg: WarmupConfig,
        store: Option<Arc<dyn TemplateStoreApi>>,
        host: Arc<dyn WarmupHost>,
        control: Arc<WarmupControl>,
        activity: Arc<HostActivity>,
        app_uid_gid: Option<(u32, u32)>,
    ) -> Self {
        WarmupJobRunner {
            cfg,
            store,
            host,
            control,
            activity,
            app_uid_gid,
            clock: Arc::new(SystemClock),
        }
    }

    /// Swap the clock. Test-only in practice; production always uses
    /// [`SystemClock`].
    pub fn with_clock(mut self, clock: Arc<dyn super::Clock>) -> Self {
        self.clock = clock;
        self
    }
}

impl JobRunner for WarmupJobRunner {
    fn job_id(&self) -> &'static str {
        JOB_ID
    }

    fn run(&self, params: &Value, abort: &AbortFlag) -> JobOutcome {
        // The connection-scoped flag the poller hands every runner, raised only
        // when the poller's connection ends (#492, `JobPollerGuard::drop`). A
        // warm-up also has its own older primitive,
        // `WarmupControl::abort_for_user_launch`; honour both — they agree on
        // teardown (`WarmupConnectionGuard` raises the second one too).
        if abort.is_aborted() {
            return JobOutcome::Deferred("aborted before the warm-up started".into());
        }

        // Cheap refusals BEFORE the gate, so a host that can never build a
        // template does not take the host-global lock or flap the encode-slot
        // reservation (which re-sends `capacity`).
        let Some(store) = self.store.clone() else {
            return JobOutcome::Skipped(
                "golden-home templates are not configured on this host".into(),
            );
        };
        if !self.cfg.enabled {
            return JobOutcome::Skipped(WARMUP_DISABLED_REASON.into());
        }
        // A template root on a different filesystem from the home root: seeding
        // degrades honestly to §4.2's tier-2 copy and is left alone, but a BUILD
        // writes gigabytes into the container overlay that the host cannot see
        // and the next redeploy discards — `failed`, not `skipped`, since it is
        // a deployment defect an operator has to fix.
        if store.cross_filesystem() && !allow_cross_filesystem() {
            warn!(
                token = "template-build-refused-crossfs",
                "template: refusing to build — {WARMUP_CROSSFS_REASON}"
            );
            return JobOutcome::Failed(WARMUP_CROSSFS_REASON.into());
        }

        let req = match parse_request(params) {
            Ok(r) => r,
            Err(e) => return JobOutcome::Failed(e),
        };

        // ── THE #489 GATE, byte-for-byte the scheduler's call ────────────────
        let now = self.clock.now();
        let guard = match self
            .control
            .try_acquire(&self.activity, POST_SESSION_QUIET, now)
        {
            Ok(g) => g,
            Err(refusal) => {
                let reason = refusal_reason(&refusal);
                // The framework persists the retry as `job_runs.attempt` +
                // `scheduled_for` (survives a reconnect), so this line need not
                // state the next delay.
                info!("template: warm-up deferred — {reason}");
                return JobOutcome::Deferred(reason);
            }
        };
        // §3.3 step 2: hold an encode-slot reservation for the duration.
        // Released by the guard's Drop on every path, including a panic (which
        // the poller records as `failed`).
        guard.control().set_reserved(true);

        let job = WarmupJob {
            cfg: &self.cfg,
            store: store.as_ref(),
            host: self.host.as_ref(),
            clock: self.clock.as_ref(),
            app_uid_gid: self.app_uid_gid,
        };
        let result = job.run(&req, &guard);
        // Read the abort cause while the gate is still held: dropping the
        // guard frees it, and the next acquirer clears the cause.
        let cause = self.control.abort_cause();
        // §7.2 domain log lines are never replaced by the framework's `job:`
        // lines — additive context only (design §3.8).
        log_outcome(&req, &result);
        drop(guard);

        outcome_of(&req, result, cause)
    }
}

/// Map the request-shaped params, refusing anything the job could not act on.
fn parse_request(params: &Value) -> Result<WarmupRequest, String> {
    let p: WarmupParams = serde_json::from_value(params.clone())
        .map_err(|e| format!("template.warmup params are not an object: {e}"))?;
    if p.image_id.is_empty() || p.registry_ref.is_empty() || p.version.is_empty() {
        return Err(format!(
            "template.warmup params incomplete (image_id={:?} registry_ref={:?} version={:?})",
            p.image_id, p.registry_ref, p.version
        ));
    }
    Ok(WarmupRequest {
        image_id: p.image_id,
        registry_ref: p.registry_ref,
        version: p.version,
    })
}

/// The operator-facing phrasing of a gate refusal. `"host has N live
/// session(s)"` is the exact string the design's acceptance step A10 asserts.
fn refusal_reason(r: &GateRefusal) -> String {
    match r {
        GateRefusal::HostBusy { live } => format!("host has {live} live session(s)"),
        GateRefusal::Busy => "another warm-up is already running on this host".into(),
    }
}

/// Turn a [`WarmupError`] into the framework's four-way outcome.
///
/// * `Aborted` is **deferred, not failed**: a warm-up that yielded to a user
///   launch did what it was designed to do, and the persisted backoff brings
///   it back.
/// * `Skipped` stays `Skipped`, so "up to date"/"disabled"/"no disk space"
///   never read as red in the viewer.
/// * `TimedOut` is a failure an operator should see.
/// * `Aborted` names its CAUSE — `abort_for_user_launch` and the connection
///   guard's Drop raise the same flag, so a plain restart must not report
///   "aborted for a user session launch" with no session in sight.
fn outcome_of(
    req: &WarmupRequest,
    result: Result<super::WarmupOutcome, WarmupError>,
    cause: AbortCause,
) -> JobOutcome {
    match result {
        Ok(o) => JobOutcome::Succeeded(json!({
            "image_id": o.image_id,
            "version": o.version,
            "files": o.stats.files,
            "bytes": o.stats.bytes,
            "mib": o.stats.bytes / (1 << 20),
            "elapsed_secs": o.elapsed.as_secs(),
            "presented_after_ms": o.presented_after.as_millis() as u64,
        })),
        Err(WarmupError::Skipped(reason)) => JobOutcome::Skipped(reason),
        Err(WarmupError::Aborted) => JobOutcome::Deferred(cause.reason().into()),
        Err(WarmupError::TimedOut) => JobOutcome::Failed(format!(
            "warm-up for {} v{} exceeded QUASAR_TEMPLATE_WARMUP_TIMEOUT_SECS",
            req.image_id, req.version
        )),
        Err(WarmupError::Failed(reason)) => JobOutcome::Failed(reason),
    }
}

/// The one image-lifecycle duty that stays agent-side: dropping a template
/// whose image was uninstalled (#488 §3.2 row 3). `removed` has no
/// control-plane analogue — the template is a purely host-local artifact under
/// `<template_root>/<image-id>/`, and nothing else would delete it, stranding
/// gigabytes and letting a re-install seed from a stale-version template.
pub struct TemplateReaper {
    store: Arc<dyn TemplateStoreApi>,
}

impl TemplateReaper {
    pub fn new(store: Arc<dyn TemplateStoreApi>) -> Self {
        TemplateReaper { store }
    }
}

impl crate::images::ImageLifecycleObserver for TemplateReaper {
    fn image_ready(&self, image_id: &str, _registry_ref: &str, version: &str) {
        // Deliberately inert: the control plane enqueues `template.warmup`
        // from `Ensurer.AgentImageState`, where the run window and schedule
        // live. Triggering here too would double-fire (harmless — the
        // open-run unique index dedupes) but reintroduce a window-bypassing
        // path.
        debug!(
            "template: {image_id} v{version} is ready; the control plane owns the warm-up trigger"
        );
    }

    fn image_removed(&self, image_id: &str) {
        match self.store.remove(image_id) {
            Ok(()) => info!("template: removed {image_id} (image uninstalled)"),
            Err(e) => warn!(
                token = "template-remove-failed",
                "template: could not remove the template for {image_id}: {e}"
            ),
        }
    }
}

/// Detaches [`TemplateReaper`] from the process-wide `ImageManager` when a
/// control-plane connection ends, and aborts any warm-up still in flight.
///
/// Both halves matter: `ImageManager` OUTLIVES a connection (pull threads are
/// not duplicated across reconnects), so a left-attached observer stacks a
/// second one on the next connect; and a warm-up still running when the
/// socket drops must not run on into the next connection's first user session
/// (#489).
pub struct WarmupConnectionGuard {
    control: Arc<WarmupControl>,
    image_mgr: Arc<crate::images::ImageManager>,
}

impl WarmupConnectionGuard {
    pub fn new(control: Arc<WarmupControl>, image_mgr: Arc<crate::images::ImageManager>) -> Self {
        WarmupConnectionGuard { control, image_mgr }
    }
}

impl Drop for WarmupConnectionGuard {
    fn drop(&mut self) {
        self.image_mgr.set_lifecycle_observer(None);
        // NOT `abort_for_user_launch`: no session is involved here, and reporting
        // one made every control-plane restart look like a user interruption.
        self.control.abort_for_connection_lost();
    }
}
