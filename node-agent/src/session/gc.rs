//! #175 agent-side backing-store reaping.
//!
//! The control plane tombstones a `user_homes` row but has no host/docker access
//! (invariant #1) to remove its backing store. This module pulls homes past the
//! 24h GC grace period, reaps each locally (the docker-volume driver was
//! hard-removed, #473), and confirms reaped ids so the control plane
//! hard-deletes the row.
//!
//! Auth mirrors the agent reconnect trust model: `node_secret` bearer token +
//! `X-Quasar-Node` header. Additive HTTP surface (control-api.md §Agent storage
//! GC); the agent WebSocket contract is unchanged.
//!
//! A reap pass must never fail the agent: every per-home error is logged and the
//! pass continues; an id is confirmed only once its store is gone or provably
//! absent (idempotent). A live mount is skipped and retried next pass.

use crate::cp_http::CpClient;
use std::collections::HashSet;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use serde::Deserialize;
use tracing::{debug, info, warn};

use crate::session::home;

/// One reapable home as returned by GET /v1/agent/storage/gc-pending. Built from
/// the wire shape via [`PendingHome::from_wire`] (the wire field `ref` is a Rust
/// keyword), so this struct itself carries no derive.
#[derive(Debug, Clone)]
pub struct PendingHome {
    pub id: String,
    // "local" is the only supported provider (docker-volume was hard-removed,
    // #473). A legacy "volume" row from a pre-upgrade database is skipped
    // (logged gc-unknown-provider), not reaped — that code path is gone.
    pub provider: String,
    pub ref_: String,
}

// The wire field is `ref` (a Rust keyword), so map it explicitly.
impl PendingHome {
    fn from_wire(w: PendingHomeWire) -> Self {
        PendingHome {
            id: w.id,
            provider: w.provider,
            ref_: w.r#ref,
        }
    }
}

#[derive(Debug, Deserialize)]
struct PendingHomeWire {
    id: String,
    provider: String,
    r#ref: String,
}

#[derive(Debug, Deserialize)]
struct PendingResp {
    homes: Vec<PendingHomeWire>,
}

/// Host paths / volume names currently mounted by live sessions. The reaper
/// skips a ref a session is using — the only guard for the local-driver
/// directory case (docker's own "volume in use" is belt-and-braces here).
/// Shared with the agent loop, which updates it as sessions start/stop.
pub type LiveRefs = Arc<Mutex<HashSet<String>>>;

/// Extract the home ref (volume name or host path) from a docker mount string
/// (`source:container[:mode]`): the `source` component verbatim.
pub fn ref_of_mount(mount: &str) -> Option<String> {
    mount.split(':').next().map(|s| s.to_string())
}

/// What one reap pass did — recorded as `job_runs.summary` (WP6, design §8.4),
/// so a skipped-live pass and a nothing-to-do pass are distinguishable.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct GcPass {
    /// Homes the control plane offered this pass.
    pub pending: usize,
    /// Backing stores actually removed (or provably already absent).
    pub reaped: usize,
    /// Homes skipped because a live session has the ref mounted. NOT a failure —
    /// they are retried next pass.
    pub skipped_live: usize,
    /// Homes that were attempted and could not be reaped (volume in use, a
    /// refused path, a docker error). Logged per home above.
    pub unreaped: usize,
    /// Rows the control plane hard-deleted in response to the confirm.
    pub confirmed: i64,
    /// The pull itself failed: the pass did nothing at all.
    pub fetch_error: Option<String>,
    /// The reaping happened but the confirm did not land; the rows are re-pulled
    /// next pass (the reap is idempotent, so this is safe, not lost work).
    pub confirm_error: Option<String>,
}

/// Configuration the reaper needs, captured once after registration. Holds no
/// `ContainerRuntime` — the docker-volume driver is hard-removed (#473); the
/// local-driver reap path is plain `std::fs::remove_dir_all`.
pub struct GcClient {
    cp: CpClient,
    live: LiveRefs,
}

const GC_PENDING_PATH: &str = "/v1/agent/storage/gc-pending";
const GC_CONFIRM_PATH: &str = "/v1/agent/storage/gc-confirm";

impl GcClient {
    pub fn new(cp: CpClient, live: LiveRefs) -> Self {
        GcClient { cp, live }
    }

    /// Run one full reap pass: pull → reap each → confirm reaped ids. Blocking;
    /// call from a spawned thread. Logs and swallows all errors — never panics,
    /// never returns Err. The returned [`GcPass`] is the jobs framework's run
    /// summary (WP6); it is additive context, not a replacement for the logging.
    pub fn run_pass(&self) -> GcPass {
        let mut pass = GcPass::default();
        let homes = match self.fetch_pending() {
            Ok(h) => h,
            Err(e) => {
                warn!(token = "gc-fetch-failed", "gc: fetch pending failed: {e}");
                pass.fetch_error = Some(e);
                return pass;
            }
        };
        pass.pending = homes.len();
        if homes.is_empty() {
            debug!("gc: no pending homes to reap");
            return pass;
        }
        info!("gc: {} home(s) pending reaping", homes.len());

        let live = self.live_snapshot();
        let mut reaped: Vec<String> = Vec::new();
        for h in &homes {
            // A backing store an active session has mounted is never reaped; it
            // is retried next pass and counted in a SUCCEEDED summary, not a
            // deferral — a pass that reaped some and skipped others did its job
            // (design §8.4).
            if live.contains(&h.ref_) {
                debug!(
                    token = "gc-home-live-skipped",
                    "gc: home {} ref {} is live — skipping this pass", h.id, h.ref_
                );
                pass.skipped_live += 1;
                continue;
            }
            if self.reap_one(h) {
                reaped.push(h.id.clone());
            } else {
                pass.unreaped += 1;
            }
        }
        pass.reaped = reaped.len();

        if reaped.is_empty() {
            return pass;
        }
        match self.confirm(&reaped) {
            Ok(n) => {
                info!("gc: confirmed {n} reaped home(s)");
                pass.confirmed = n;
            }
            Err(e) => {
                warn!(
                    token = "gc-confirm-failed",
                    "gc: confirm failed (rows will be re-pulled next pass): {e}"
                );
                pass.confirm_error = Some(e);
            }
        }
        pass
    }

    fn live_snapshot(&self) -> HashSet<String> {
        self.live
            .lock()
            .map(|g| g.clone())
            .unwrap_or_else(|_| HashSet::new())
    }

    /// Reap one home's backing store. Returns true iff the store is now gone (so
    /// the id may be confirmed). Idempotent: an already-absent store is success.
    fn reap_one(&self, h: &PendingHome) -> bool {
        match h.provider.as_str() {
            "local" => self.reap_local(&h.ref_),
            // "volume" (docker-volume driver, hard-removed #473) is left
            // unreaped rather than mishandled; an operator can `docker volume
            // rm` it by hand.
            other => {
                warn!(
                    token = "gc-unknown-provider",
                    "gc: home {} has unknown provider {other:?} — skipping", h.id
                );
                false
            }
        }
    }

    /// Remove a local-driver home directory after confirming the ref is strictly
    /// under `QUASAR_HOME_ROOT` (reuse the provisioner's confinement helper). A
    /// ref outside the root, or an unset root, is an error → not reaped (and not
    /// confirmed). `NotFound` → success (idempotent).
    fn reap_local(&self, dir: &str) -> bool {
        let Some(root) = home::configured_home_root() else {
            warn!(
                token = "gc-local-home-root-unset",
                "gc: QUASAR_HOME_ROOT unset but a 'local' home was pulled — refusing to reap {dir}"
            );
            return false;
        };
        let path = PathBuf::from(dir);
        if path.components().any(|c| c.as_os_str() == "..") {
            warn!(
                token = "gc-local-ref-traversal",
                "gc: refusing to reap local ref with traversal: {dir}"
            );
            return false;
        }
        if !home::is_under_root(&root, &path) {
            warn!(
                token = "gc-local-ref-outside-root",
                "gc: local ref {dir} is not strictly under QUASAR_HOME_ROOT {} — refusing to reap",
                root.display()
            );
            return false;
        }
        match std::fs::remove_dir_all(&path) {
            Ok(()) => {
                info!("gc: removed local home dir {dir}");
                true
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                debug!("gc: local home dir {dir} already absent");
                true
            }
            Err(e) => {
                warn!(
                    token = "gc-local-dir-rm-failed",
                    "gc: removing local home dir {dir} failed: {e}"
                );
                false
            }
        }
    }

    // ── HTTP ────────────────────────────────────────────────────────────────

    fn fetch_pending(&self) -> Result<Vec<PendingHome>, String> {
        let parsed: PendingResp = self.cp.get_json(GC_PENDING_PATH)?;
        Ok(parsed
            .homes
            .into_iter()
            .map(PendingHome::from_wire)
            .collect())
    }

    fn confirm(&self, ids: &[String]) -> Result<i64, String> {
        let body = serde_json::json!({ "home_ids": ids });
        #[derive(Deserialize)]
        struct ConfirmResp {
            deleted: i64,
        }
        let parsed: ConfirmResp = self.cp.post_json(GC_CONFIRM_PATH, &body)?;
        Ok(parsed.deleted)
    }
}

// ---------------------------------------------------------------------------
// WP6 — the reaper as a background-jobs runner
// ---------------------------------------------------------------------------

/// The framework-wide id for the home GC. Never renamed (it is the `jobs` row
/// key, the admin API path segment and the log field).
pub const JOB_ID: &str = "home.gc";

/// Runs one [`GcClient::run_pass`] when the control plane asks for one. The
/// live-ref skip rule and the `QUASAR_HOME_ROOT` path-traversal refusal are
/// unchanged; the pass still cannot fail the agent.
pub struct HomeGcJobRunner {
    cp: CpClient,
    live: LiveRefs,
}

impl HomeGcJobRunner {
    pub fn new(cp: CpClient, live: LiveRefs) -> Self {
        HomeGcJobRunner { cp, live }
    }
}

impl crate::jobs::JobRunner for HomeGcJobRunner {
    fn job_id(&self) -> &'static str {
        JOB_ID
    }

    fn run(
        &self,
        _params: &serde_json::Value,
        _abort: &crate::jobs::AbortFlag,
    ) -> crate::jobs::JobOutcome {
        let client = GcClient::new(self.cp.clone(), self.live.clone());
        summarize(client.run_pass())
    }
}

/// Map a pass onto the framework's four-way outcome: a failed pull is `failed`;
/// nothing pending is `skipped` (so "ran, nothing to do" ≠ "never ran");
/// anything else — including all-live — is `succeeded` with the counts (design
/// §8.4).
fn summarize(pass: GcPass) -> crate::jobs::JobOutcome {
    use crate::jobs::JobOutcome;
    if let Some(e) = pass.fetch_error {
        return JobOutcome::Failed(format!("could not pull pending homes: {e}"));
    }
    if pass.pending == 0 {
        return JobOutcome::Skipped("no homes are past their GC grace period".into());
    }
    let mut summary = serde_json::json!({
        "pending": pass.pending,
        "reaped": pass.reaped,
        "skipped_live": pass.skipped_live,
        "unreaped": pass.unreaped,
        "confirmed": pass.confirmed,
    });
    if let Some(e) = pass.confirm_error {
        // Reaping happened; only the ack did not land — succeeded-with-a-note.
        summary["confirm_error"] = serde_json::Value::String(e);
    }
    JobOutcome::Succeeded(summary)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::jobs::JobOutcome;

    #[test]
    fn a_pass_with_nothing_pending_is_skipped_with_a_reason() {
        assert_eq!(
            summarize(GcPass::default()),
            JobOutcome::Skipped("no homes are past their GC grace period".into())
        );
    }

    #[test]
    fn a_failed_pull_is_a_failure_not_a_skip() {
        let pass = GcPass {
            fetch_error: Some("connection refused".into()),
            ..GcPass::default()
        };
        match summarize(pass) {
            JobOutcome::Failed(e) => assert!(e.contains("connection refused"), "{e}"),
            other => panic!("expected a failure, got {other:?}"),
        }
    }

    /// A mixed reap/skip pass SUCCEEDED — the live-ref skip is normal operation.
    #[test]
    fn a_mixed_pass_succeeds_and_reports_its_counts() {
        let pass = GcPass {
            pending: 3,
            reaped: 2,
            skipped_live: 1,
            unreaped: 0,
            confirmed: 2,
            ..GcPass::default()
        };
        match summarize(pass) {
            JobOutcome::Succeeded(v) => {
                assert_eq!(v["pending"], 3);
                assert_eq!(v["reaped"], 2);
                assert_eq!(v["skipped_live"], 1);
                assert_eq!(v["confirmed"], 2);
                assert!(v.get("confirm_error").is_none());
            }
            other => panic!("expected success, got {other:?}"),
        }
    }

    /// All-live: nothing reaped, still a successful pass — retried next run.
    #[test]
    fn an_all_live_pass_still_succeeds() {
        let pass = GcPass {
            pending: 2,
            skipped_live: 2,
            ..GcPass::default()
        };
        match summarize(pass) {
            JobOutcome::Succeeded(v) => {
                assert_eq!(v["reaped"], 0);
                assert_eq!(v["skipped_live"], 2);
            }
            other => panic!("expected success, got {other:?}"),
        }
    }

    /// The reap happened; only the ack did not — recorded as success carrying
    /// the error, since idempotent reaping means the rows are safely re-pulled.
    #[test]
    fn a_confirm_failure_keeps_the_reap_counts_and_notes_the_error() {
        let pass = GcPass {
            pending: 1,
            reaped: 1,
            confirm_error: Some("502".into()),
            ..GcPass::default()
        };
        match summarize(pass) {
            JobOutcome::Succeeded(v) => {
                assert_eq!(v["reaped"], 1);
                assert_eq!(v["confirm_error"], "502");
            }
            other => panic!("expected success, got {other:?}"),
        }
    }

    #[test]
    fn ref_of_volume_mount() {
        assert_eq!(
            ref_of_mount("quasar-home-u-a:/home/quasar:rw").as_deref(),
            Some("quasar-home-u-a")
        );
    }

    #[test]
    fn ref_of_local_mount() {
        assert_eq!(
            ref_of_mount("/data/homes/u/a:/home/quasar:rw").as_deref(),
            Some("/data/homes/u/a")
        );
    }

    #[test]
    fn from_wire_maps_ref() {
        let w = PendingHomeWire {
            id: "id1".into(),
            provider: "volume".into(),
            r#ref: "vol1".into(),
        };
        let h = PendingHome::from_wire(w);
        assert_eq!(h.id, "id1");
        assert_eq!(h.provider, "volume");
        assert_eq!(h.ref_, "vol1");
    }
}
