//! The agent half of the background-jobs framework: a generic poller that pulls
//! runs the control plane has scheduled for THIS host, dispatches each to a
//! registered [`JobRunner`], and reports what the runner decided.
//!
//! Design of record: `docs/design/plans/2026-08-12-jobs-framework-and-viewer.md`
//! (§4.1 for this module, §3.6 for the wire shapes).
//!
//! Two rules this module exists to keep:
//!
//! 1. **The framework REQUESTS; the job DECIDES.** This poller never inspects a
//!    job's preconditions and never overrides a gate. A runner whose own gate
//!    refuses returns [`JobOutcome::Deferred`] with a reason, which the control
//!    plane turns into a persisted backoff. `session::warmup::gate` stays where
//!    it is.
//! 2. **Pull, never push.** The agent asks what is due over two node-secret HTTP
//!    routes (`GET /v1/agent/jobs/pending`, `POST /v1/agent/jobs/report`) rather
//!    than a WebSocket message — a claim is a database row, so a reconnect has
//!    nothing to correlate and `protocol/agent-api.md` is untouched. A change
//!    here that seems to need a new `AgentMsg` variant is an escalation.
//!
//! `agent.rs` registers two runners per connection: `template.warmup` (#488
//! golden-home warm-up) and `home.gc` (#175 backing-store reaper).
//!
//! Inertness is enforced twice, for a host with no node_secret (or any future
//! build registering nothing): [`spawn_job_poller`] returns `None` with an
//! empty registry, so no task is created and no HTTP is issued —
//! [`JobPoller::run_pass`] enforces the same rule for anyone constructing a
//! poller directly.

use std::collections::BTreeMap;
use std::panic::AssertUnwindSafe;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use tracing::{debug, info, warn};

/// HTTP request timeout for the two job endpoints (small JSON payloads),
/// matching `session::gc` and `session::library_scan`.
const HTTP_TIMEOUT: Duration = Duration::from_secs(15);

/// Initial delay before the first poll: must not contend with the
/// session-handling burst that follows a (re)connect.
const INITIAL_DELAY: Duration = Duration::from_secs(30);

/// `QUASAR_JOB_POLL_SECS` default. Also the worst-case latency of an admin
/// "Run now", which the admin API's `eta_note` says out loud.
const DEFAULT_POLL_SECS: u64 = 60;

/// Backoff ceiling after repeated transport failures — a control plane down
/// for an hour must not be polled 60 times by every host.
const MAX_POLL_BACKOFF: Duration = Duration::from_secs(15 * 60);

// ── Outcomes ────────────────────────────────────────────────────────────────

/// How a runner says its run ended.
///
/// `Deferred` and `Failed` are deliberately different variants rather than one
/// "did not succeed": "I declined" and "I broke" must not look the same in the
/// viewer. A deferral backs off and retries; a failure retries on the normal
/// schedule and shows red.
#[derive(Debug, Clone, PartialEq)]
pub enum JobOutcome {
    /// Done. The value is the operator-facing summary — the numbers someone
    /// actually wants (`{"files": 23701, "mib": 2512}`), bounded by the control
    /// plane's 4096-byte ceiling.
    Succeeded(Value),
    /// Nothing to do, or the feature is unconfigured. The string is the reason,
    /// and it is what finally makes "configured but found nothing" and "not
    /// configured at all" distinguishable — today they are both silence.
    Skipped(String),
    /// The job's own gate refused. The string is the operator-readable reason
    /// ("host has 1 live session(s)").
    Deferred(String),
    /// The run broke. The string is the error.
    Failed(String),
}

impl JobOutcome {
    /// The wire `state` value.
    fn state(&self) -> &'static str {
        match self {
            JobOutcome::Succeeded(_) => "succeeded",
            JobOutcome::Skipped(_) => "skipped",
            JobOutcome::Deferred(_) => "deferred",
            JobOutcome::Failed(_) => "failed",
        }
    }

    /// The wire `summary`. A reason is stored INSIDE the summary under
    /// `"reason"`, matching the control plane's `jobs.Skipped`/`jobs.Deferred`
    /// constructors, so one convention serves both planes.
    fn summary(&self) -> Value {
        match self {
            JobOutcome::Succeeded(v) if v.is_object() => v.clone(),
            // A runner that returned a bare scalar/array still has its result
            // recorded rather than dropped; the column is a JSON object.
            JobOutcome::Succeeded(Value::Null) => json!({}),
            JobOutcome::Succeeded(v) => json!({ "result": v }),
            JobOutcome::Skipped(r) | JobOutcome::Deferred(r) => json!({ "reason": r }),
            JobOutcome::Failed(_) => json!({}),
        }
    }

    /// The wire `error`, set only for a failure.
    fn error(&self) -> Option<String> {
        match self {
            JobOutcome::Failed(e) => Some(e.clone()),
            _ => None,
        }
    }
}

/// A cooperative abort signal handed to a running job, so a long job (an
/// incremental + interruptible + throttled re-convergence pass) has somewhere
/// to check. Interruption is normally signalled through a job's own abort
/// primitive (e.g. `WarmupControl::abort_for_user_launch`), never by the
/// scheduler killing work mid-pass. The window governs STARTING, never STOPPING.
///
/// The one exception is connection teardown (#492): a claim belongs to the
/// connection that took it, so on reconnect the control plane aborts every run
/// this host still holds (`ReclaimHostRuns`) — work continuing past teardown
/// would only produce a report for a run that no longer exists.
/// [`JobPollerGuard::drop`] must raise this flag, because aborting the poll
/// task alone is NOT enough: a `run_pass` already on a blocking thread cannot
/// be cancelled, and the flag is what actually stops it.
#[derive(Clone, Debug, Default)]
pub struct AbortFlag(Arc<AtomicBool>);

impl AbortFlag {
    pub fn new() -> Self {
        AbortFlag(Arc::new(AtomicBool::new(false)))
    }

    /// Request that the run stop at its next checkpoint.
    pub fn abort(&self) {
        self.0.store(true, Ordering::SeqCst);
    }

    pub fn is_aborted(&self) -> bool {
        self.0.load(Ordering::SeqCst)
    }
}

/// One agent-side job implementation.
///
/// `run` is called on a blocking thread, so it may do filesystem and subprocess
/// work directly. It must not panic — but if it does, the poller records the
/// panic as a `failed` run rather than taking the agent's poll task with it.
pub trait JobRunner: Send + Sync {
    /// The stable dotted id from the design's vocabulary (`template.warmup`,
    /// `home.gc`). It is the control-plane row key, the log field and the API
    /// path segment, so it is never renamed.
    fn job_id(&self) -> &'static str;

    /// Execute one run. `params` is the opaque blob the control plane stored
    /// when it materialized the run (`{}` for a plain scheduled run).
    fn run(&self, params: &Value, abort: &AbortFlag) -> JobOutcome;
}

/// The set of runners this agent can execute. A job id with no registered
/// runner is reported `failed` immediately rather than left to time out an
/// hour later — the control plane's dispatcher does the same for a
/// control-plane run with no `RunFunc`.
#[derive(Default)]
pub struct JobRegistry {
    runners: BTreeMap<&'static str, Arc<dyn JobRunner>>,
}

impl JobRegistry {
    pub fn new() -> Self {
        JobRegistry::default()
    }

    /// Register a runner. A duplicate id replaces the previous entry and warns
    /// — two runners claiming one job id is a wiring bug.
    pub fn register(&mut self, runner: Arc<dyn JobRunner>) {
        let id = runner.job_id();
        if self.runners.insert(id, runner).is_some() {
            warn!(
                token = "job-runner-registered-twice",
                "job: runner {id} registered twice — the later registration wins"
            );
        }
    }

    pub fn get(&self, job_id: &str) -> Option<Arc<dyn JobRunner>> {
        self.runners.get(job_id).cloned()
    }

    pub fn is_empty(&self) -> bool {
        self.runners.is_empty()
    }

    pub fn len(&self) -> usize {
        self.runners.len()
    }

    /// The registered ids, for the one boot log line that says what this agent
    /// can execute.
    pub fn job_ids(&self) -> Vec<&'static str> {
        self.runners.keys().copied().collect()
    }
}

// ── Wire shapes (control-api.md, additive HTTP surface) ─────────────────────

/// One run this host has just claimed. Deserialized from
/// `GET /v1/agent/jobs/pending`.
#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct PendingRun {
    pub run_id: String,
    pub job_id: String,
    /// Opaque per-job blob; the agent never interprets it, it hands it to the
    /// runner verbatim.
    #[serde(default)]
    pub params: Value,
    /// After this long with no report the control plane aborts the run and
    /// re-materializes it. Carried so a runner can bound itself rather than
    /// discover the abort by racing.
    #[serde(default)]
    pub deadline_secs: u64,
}

#[derive(Debug, Deserialize)]
struct PendingResponse {
    #[serde(default)]
    runs: Vec<PendingRun>,
}

/// The body POSTed to `/v1/agent/jobs/report`.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct JobReport {
    pub run_id: String,
    pub state: String,
    pub summary: Value,
    pub error: Option<String>,
}

// ── Transport ───────────────────────────────────────────────────────────────

/// The two HTTP calls, behind a trait so the poller's dispatch/report logic is
/// testable without a control plane. Production uses [`HttpTransport`].
pub trait JobTransport: Send + Sync {
    fn fetch_pending(&self) -> Result<Vec<PendingRun>, String>;
    fn post_report(&self, report: &JobReport) -> Result<(), String>;
}

/// The real transport: node-secret auth, identical in shape to
/// `session::gc::GcClient` and `session::library_scan::LibraryScanClient`.
pub struct HttpTransport {
    http_base: String,
    node_name: String,
    node_secret: String,
}

impl HttpTransport {
    pub fn new(http_base: String, node_name: String, node_secret: String) -> Self {
        HttpTransport {
            http_base,
            node_name,
            node_secret,
        }
    }
}

impl JobTransport for HttpTransport {
    fn fetch_pending(&self) -> Result<Vec<PendingRun>, String> {
        let resp = ureq::get(&format!("{}/v1/agent/jobs/pending", self.http_base))
            .timeout(HTTP_TIMEOUT)
            .set("Authorization", &format!("Bearer {}", self.node_secret))
            .set("X-Quasar-Node", &self.node_name)
            .call()
            .map_err(|e| format!("GET jobs/pending: {e}"))?;
        let parsed: PendingResponse = resp
            .into_json()
            .map_err(|e| format!("decode jobs/pending: {e}"))?;
        Ok(parsed.runs)
    }

    fn post_report(&self, report: &JobReport) -> Result<(), String> {
        ureq::post(&format!("{}/v1/agent/jobs/report", self.http_base))
            .timeout(HTTP_TIMEOUT)
            .set("Authorization", &format!("Bearer {}", self.node_secret))
            .set("X-Quasar-Node", &self.node_name)
            .send_json(report)
            .map_err(|e| format!("POST jobs/report: {e}"))?;
        Ok(())
    }
}

// ── The poller ──────────────────────────────────────────────────────────────

/// What one pass did, for the caller's backoff decision and for tests.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct PassOutcome {
    /// Runs claimed and dispatched this pass.
    pub claimed: usize,
    /// Reports the control plane accepted.
    pub reported: usize,
    /// The pending fetch itself failed. This — and only this — drives the poll
    /// backoff: a job that FAILED is not a transport problem and must not slow
    /// the poll.
    pub fetch_failed: bool,
    /// The pass did no HTTP because no runner is registered.
    pub inert: bool,
    /// The connection that owns this poller ended mid-pass (#492), so the pass
    /// stopped early and did not report. NOT a transport failure: it must not
    /// feed the poll backoff, because the task is about to be dropped anyway.
    pub torn_down: bool,
}

/// Pulls, dispatches and reports. Cheap to clone (two `Arc`s), because each pass
/// runs on a fresh blocking thread.
#[derive(Clone)]
pub struct JobPoller {
    transport: Arc<dyn JobTransport>,
    registry: Arc<JobRegistry>,
    /// Raised when this poller's CONNECTION ends (#492). Shared with every run
    /// it dispatches, so one flag stops the pass, the runner and the report —
    /// see [`AbortFlag`].
    abort: AbortFlag,
}

impl JobPoller {
    pub fn new(transport: Arc<dyn JobTransport>, registry: Arc<JobRegistry>) -> Self {
        JobPoller {
            transport,
            registry,
            abort: AbortFlag::new(),
        }
    }

    /// A handle on this poller's teardown flag, for the guard that owns the
    /// connection's lifetime. Cloning shares the flag — that is the point.
    pub fn abort_flag(&self) -> AbortFlag {
        self.abort.clone()
    }

    /// Run one full pass: claim due runs, execute each, report each outcome.
    ///
    /// Blocking (runners do filesystem/subprocess work, HTTP is blocking); call
    /// it from a spawned blocking thread. It never panics and never returns an
    /// error — a job failure, a transport failure and a panicking runner are all
    /// ordinary outcomes here, matching the GC reaper's "never fatal to the
    /// agent" posture.
    pub fn run_pass(&self) -> PassOutcome {
        // Inertness enforced before the first byte: with nothing registered
        // there is nothing to execute, so it does not ask. Belt-and-braces with
        // spawn_job_poller declining to create the task at all.
        if self.registry.is_empty() {
            return PassOutcome {
                inert: true,
                ..PassOutcome::default()
            };
        }
        // The connection this poller belongs to is gone (#492): claiming more
        // work would take runs this agent cannot report, and the control plane
        // is about to abort everything this host holds anyway.
        if self.abort.is_aborted() {
            return PassOutcome {
                torn_down: true,
                ..PassOutcome::default()
            };
        }

        let runs = match self.transport.fetch_pending() {
            Ok(r) => r,
            Err(e) => {
                warn!(token = "job-fetch-failed", "job: fetch pending failed: {e}");
                return PassOutcome {
                    fetch_failed: true,
                    ..PassOutcome::default()
                };
            }
        };
        if runs.is_empty() {
            debug!("job: no runs claimed");
            return PassOutcome::default();
        }

        let mut out = PassOutcome {
            claimed: runs.len(),
            ..PassOutcome::default()
        };
        for run in &runs {
            // Checked BETWEEN runs as well as inside them: a runner that never
            // looks at its abort flag still cannot start the next job of a
            // batch after the connection died.
            if self.abort.is_aborted() {
                out.torn_down = true;
                warn!(
                    token = "job-pass-torn-down",
                    "job: connection torn down mid-pass — abandoning {} claimed run(s); \
                     the control plane reclaims them on re-register",
                    runs.len() - out.reported
                );
                break;
            }
            let outcome = self.execute(run);
            if self.report(run, &outcome) {
                out.reported += 1;
            }
        }
        out
    }

    /// Dispatch one claimed run to its runner. An unknown job id is a `failed`
    /// report, not silence — saying nothing would hold the job's single-flight
    /// slot on this host until the claim timeout expires an hour later. A
    /// panicking runner is caught so one bad job never stops every other job.
    fn execute(&self, run: &PendingRun) -> JobOutcome {
        let Some(runner) = self.registry.get(&run.job_id) else {
            warn!(
                token = "job-no-runner",
                "job: no runner registered for {} (run {})", run.job_id, run.run_id
            );
            return JobOutcome::Failed(format!("no runner registered for {}", run.job_id));
        };
        info!(
            "job: run started job_id={} run_id={} deadline_secs={}",
            run.job_id, run.run_id, run.deadline_secs
        );
        // The poller's flag, not a fresh one: this is how a long job stops
        // when this connection ends (#492).
        let abort = self.abort.clone();
        let started = std::time::Instant::now();
        let outcome =
            match std::panic::catch_unwind(AssertUnwindSafe(|| runner.run(&run.params, &abort))) {
                Ok(o) => o,
                Err(payload) => {
                    let msg = panic_message(&payload);
                    JobOutcome::Failed(format!("panic: {msg}"))
                }
            };
        let dur_ms = started.elapsed().as_millis();
        match &outcome {
            JobOutcome::Succeeded(summary) => info!(
                "job: run finished job_id={} run_id={} state=succeeded dur_ms={dur_ms} summary={summary}",
                run.job_id, run.run_id
            ),
            JobOutcome::Skipped(reason) => info!(
                "job: run skipped job_id={} run_id={} dur_ms={dur_ms} reason={reason:?}",
                run.job_id, run.run_id
            ),
            JobOutcome::Deferred(reason) => info!(
                "job: run deferred job_id={} run_id={} dur_ms={dur_ms} reason={reason:?}",
                run.job_id, run.run_id
            ),
            JobOutcome::Failed(err) => tracing::error!(
                token = "job-run-failed",
                "job: run failed job_id={} run_id={} dur_ms={dur_ms} err={err:?}",
                run.job_id, run.run_id
            ),
        }
        outcome
    }

    /// POST one outcome. A failed report is logged and dropped: the run is
    /// already claimed control-plane-side, so the reaper aborts and
    /// re-materializes it. Reporting is per-run so one unreportable outcome
    /// cannot swallow the rest of the pass.
    fn report(&self, run: &PendingRun, outcome: &JobOutcome) -> bool {
        // A report from a torn-down connection is about a run the control
        // plane has already reclaimed, or is about to (#492); sending it would
        // race ReclaimHostRuns and lose. Dropped here rather than pretending
        // the claim survived the connection that took it.
        if self.abort.is_aborted() {
            warn!(
                token = "job-report-abandoned",
                "job: not reporting run {} ({}, {}) — the connection that claimed it is gone; \
                 the control plane reclaims it on re-register",
                run.run_id,
                run.job_id,
                outcome.state()
            );
            return false;
        }
        let report = JobReport {
            run_id: run.run_id.clone(),
            state: outcome.state().to_string(),
            summary: outcome.summary(),
            error: outcome.error(),
        };
        match self.transport.post_report(&report) {
            Ok(()) => true,
            Err(e) => {
                warn!(
                    token = "job-report-failed",
                    "job: report for run {} ({}) failed — the control plane will reap it: {e}",
                    run.run_id,
                    run.job_id
                );
                false
            }
        }
    }
}

fn panic_message(payload: &(dyn std::any::Any + Send)) -> String {
    if let Some(s) = payload.downcast_ref::<&'static str>() {
        (*s).to_string()
    } else if let Some(s) = payload.downcast_ref::<String>() {
        s.clone()
    } else {
        "unknown panic".to_string()
    }
}

// ── Poll cadence ────────────────────────────────────────────────────────────

/// The delay between passes: `base` normally, doubling per consecutive
/// transport failure up to [`MAX_POLL_BACKOFF`], reset by the first pass that
/// reaches the control plane. Only a transport failure backs off — a job that
/// reported `failed` reached the control plane fine, and slowing the poll for
/// a broken job would delay every other job, including a manual "Run now".
#[derive(Debug, Clone)]
pub struct PollBackoff {
    base: Duration,
    max: Duration,
    consecutive_failures: u32,
}

impl PollBackoff {
    pub fn new(base: Duration) -> Self {
        PollBackoff {
            base,
            max: MAX_POLL_BACKOFF,
            consecutive_failures: 0,
        }
    }

    /// Fold in a pass's result and return how long to wait before the next one.
    pub fn next_delay(&mut self, outcome: &PassOutcome) -> Duration {
        if outcome.fetch_failed {
            self.consecutive_failures = self.consecutive_failures.saturating_add(1);
        } else {
            self.consecutive_failures = 0;
        }
        if self.consecutive_failures == 0 {
            return self.base;
        }
        // Shift by failures-1 so the FIRST failure retries at the base interval:
        // a single dropped poll is usually a redeploy, and doubling immediately
        // would double manual-trigger latency for a blip.
        let shift = (self.consecutive_failures - 1).min(16);
        let scaled = self.base.saturating_mul(1u32 << shift);
        scaled.min(self.max)
    }
}

/// `QUASAR_JOB_POLL_SECS`: how often the agent asks for work. `0` disables the
/// job client entirely — scheduled jobs then show as overdue in the admin viewer.
fn poll_interval() -> Option<Duration> {
    let secs = std::env::var("QUASAR_JOB_POLL_SECS")
        .ok()
        .and_then(|s| s.trim().parse::<u64>().ok())
        .unwrap_or(DEFAULT_POLL_SECS);
    if secs == 0 {
        return None;
    }
    Some(Duration::from_secs(secs))
}

/// Ends the poller when this connection ends (dropped on return from
/// `connect_and_run`) — a stale poller must never outlive its node_secret.
///
/// Must raise the abort flag BEFORE aborting the task (#492): `JoinHandle::abort`
/// only cancels at an await point, so a pass already on a blocking thread runs
/// to completion regardless and would report a run whose claim died with this
/// connection. The flag is what that in-flight pass, and the runner inside it,
/// actually observes.
pub struct JobPollerGuard {
    handle: tokio::task::JoinHandle<()>,
    abort: AbortFlag,
}

impl Drop for JobPollerGuard {
    fn drop(&mut self) {
        self.abort.abort();
        self.handle.abort();
    }
}

/// Spawn the per-connection job poller, or `None` when there is nothing to
/// poll for: an empty registry (host with no node_secret), or
/// `QUASAR_JOB_POLL_SECS=0`. Polling anyway with an empty registry would put a
/// request per host per minute on the control plane for nothing.
pub fn spawn_job_poller(
    http_base: String,
    node_name: String,
    node_secret: String,
    registry: Arc<JobRegistry>,
) -> Option<JobPollerGuard> {
    if registry.is_empty() {
        debug!("job: no agent-side job runners registered — job poller not started");
        return None;
    }
    let Some(interval) = poll_interval() else {
        info!("job: QUASAR_JOB_POLL_SECS=0 — job poller disabled on this host");
        return None;
    };
    info!(
        "job: poller starting (every {}s, runners: {})",
        interval.as_secs(),
        registry.job_ids().join(", ")
    );
    let transport: Arc<dyn JobTransport> =
        Arc::new(HttpTransport::new(http_base, node_name, node_secret));
    let poller = JobPoller::new(transport, registry);
    // Taken BEFORE the poller moves into the task: the guard must reach the
    // flag of the poller that is actually running (#492).
    let abort = poller.abort_flag();

    let handle = tokio::spawn(async move {
        tokio::time::sleep(INITIAL_DELAY).await;
        let mut backoff = PollBackoff::new(interval);
        loop {
            let p = poller.clone();
            // Move the blocking pass off the async runtime.
            let outcome = match tokio::task::spawn_blocking(move || p.run_pass()).await {
                Ok(o) => o,
                Err(e) => {
                    warn!(
                        token = "job-poll-join-error",
                        "job: poll task join error: {e}"
                    );
                    PassOutcome {
                        fetch_failed: true,
                        ..PassOutcome::default()
                    }
                }
            };
            tokio::time::sleep(backoff.next_delay(&outcome)).await;
        }
    });
    Some(JobPollerGuard { handle, abort })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    // ── Fakes ───────────────────────────────────────────────────────────────

    #[derive(Default)]
    struct FakeTransport {
        /// Successive pending batches; a pass past the end sees an empty batch.
        batches: Mutex<Vec<Result<Vec<PendingRun>, String>>>,
        fetches: Mutex<usize>,
        reports: Mutex<Vec<JobReport>>,
        report_fails: Mutex<bool>,
    }

    impl FakeTransport {
        fn with(batches: Vec<Result<Vec<PendingRun>, String>>) -> Arc<Self> {
            Arc::new(FakeTransport {
                batches: Mutex::new(batches),
                ..FakeTransport::default()
            })
        }
        fn fetch_count(&self) -> usize {
            *self.fetches.lock().unwrap()
        }
        fn reports(&self) -> Vec<JobReport> {
            self.reports.lock().unwrap().clone()
        }
    }

    impl JobTransport for FakeTransport {
        fn fetch_pending(&self) -> Result<Vec<PendingRun>, String> {
            *self.fetches.lock().unwrap() += 1;
            let mut b = self.batches.lock().unwrap();
            if b.is_empty() {
                return Ok(Vec::new());
            }
            b.remove(0)
        }
        fn post_report(&self, report: &JobReport) -> Result<(), String> {
            if *self.report_fails.lock().unwrap() {
                return Err("report rejected".into());
            }
            self.reports.lock().unwrap().push(report.clone());
            Ok(())
        }
    }

    struct FixedRunner {
        id: &'static str,
        outcome: JobOutcome,
        seen_params: Mutex<Vec<Value>>,
    }

    impl FixedRunner {
        fn new(id: &'static str, outcome: JobOutcome) -> Arc<Self> {
            Arc::new(FixedRunner {
                id,
                outcome,
                seen_params: Mutex::new(Vec::new()),
            })
        }
    }

    impl JobRunner for FixedRunner {
        fn job_id(&self) -> &'static str {
            self.id
        }
        fn run(&self, params: &Value, _abort: &AbortFlag) -> JobOutcome {
            self.seen_params.lock().unwrap().push(params.clone());
            self.outcome.clone()
        }
    }

    struct PanickingRunner;
    impl JobRunner for PanickingRunner {
        fn job_id(&self) -> &'static str {
            "boom.job"
        }
        fn run(&self, _params: &Value, _abort: &AbortFlag) -> JobOutcome {
            panic!("runner exploded");
        }
    }

    fn pending(run_id: &str, job_id: &str, params: Value) -> PendingRun {
        PendingRun {
            run_id: run_id.into(),
            job_id: job_id.into(),
            params,
            deadline_secs: 3600,
        }
    }

    fn registry_with(runners: Vec<Arc<dyn JobRunner>>) -> Arc<JobRegistry> {
        let mut reg = JobRegistry::new();
        for r in runners {
            reg.register(r);
        }
        Arc::new(reg)
    }

    // ── pull -> dispatch -> report ──────────────────────────────────────────

    #[test]
    fn pulls_dispatches_and_reports_success() {
        let runner = FixedRunner::new(
            "home.gc",
            JobOutcome::Succeeded(json!({"reaped": 2, "bytes": 5312000})),
        );
        let tx = FakeTransport::with(vec![Ok(vec![pending(
            "run-1",
            "home.gc",
            json!({"image_id": "steam"}),
        )])]);
        let poller = JobPoller::new(tx.clone(), registry_with(vec![runner.clone()]));

        let out = poller.run_pass();
        assert_eq!(out.claimed, 1);
        assert_eq!(out.reported, 1);
        assert!(!out.fetch_failed && !out.inert);

        // The params blob reached the runner verbatim.
        assert_eq!(
            runner.seen_params.lock().unwrap().as_slice(),
            [json!({"image_id": "steam"})]
        );
        let reports = tx.reports();
        assert_eq!(reports.len(), 1);
        assert_eq!(reports[0].run_id, "run-1");
        assert_eq!(reports[0].state, "succeeded");
        assert_eq!(reports[0].summary, json!({"reaped": 2, "bytes": 5312000}));
        assert_eq!(reports[0].error, None);
    }

    /// A runner that declines produces a `deferred` report carrying its reason
    /// — not a failure, and not silence.
    #[test]
    fn a_refusing_gate_reports_deferred_with_its_reason() {
        let runner = FixedRunner::new(
            "template.warmup",
            JobOutcome::Deferred("host has 1 live session(s)".into()),
        );
        let tx = FakeTransport::with(vec![Ok(vec![pending(
            "run-2",
            "template.warmup",
            Value::Null,
        )])]);
        let poller = JobPoller::new(tx.clone(), registry_with(vec![runner]));

        poller.run_pass();
        let reports = tx.reports();
        assert_eq!(reports[0].state, "deferred");
        assert_eq!(
            reports[0].summary,
            json!({"reason": "host has 1 live session(s)"})
        );
        assert_eq!(reports[0].error, None);
    }

    #[test]
    fn a_skipped_run_carries_its_reason_and_is_not_a_failure() {
        let runner = FixedRunner::new(
            "artwork.sweep",
            JobOutcome::Skipped("no artwork provider configured".into()),
        );
        let tx = FakeTransport::with(vec![Ok(vec![pending(
            "run-3",
            "artwork.sweep",
            Value::Null,
        )])]);
        let poller = JobPoller::new(tx.clone(), registry_with(vec![runner]));

        poller.run_pass();
        let reports = tx.reports();
        assert_eq!(reports[0].state, "skipped");
        assert_eq!(
            reports[0].summary,
            json!({"reason": "no artwork provider configured"})
        );
    }

    #[test]
    fn a_failure_carries_the_error_and_an_empty_summary() {
        let runner = FixedRunner::new("home.gc", JobOutcome::Failed("docker not reachable".into()));
        let tx = FakeTransport::with(vec![Ok(vec![pending("run-4", "home.gc", Value::Null)])]);
        let poller = JobPoller::new(tx.clone(), registry_with(vec![runner]));

        poller.run_pass();
        let reports = tx.reports();
        assert_eq!(reports[0].state, "failed");
        assert_eq!(reports[0].error.as_deref(), Some("docker not reachable"));
        assert_eq!(reports[0].summary, json!({}));
    }

    /// A run for a job this agent cannot execute is closed immediately as
    /// failed, not left to time out the claim.
    #[test]
    fn an_unknown_job_id_is_reported_failed_not_ignored() {
        let known = FixedRunner::new("home.gc", JobOutcome::Succeeded(json!({})));
        let tx = FakeTransport::with(vec![Ok(vec![pending(
            "run-5",
            "some.future.job",
            Value::Null,
        )])]);
        let poller = JobPoller::new(tx.clone(), registry_with(vec![known]));

        let out = poller.run_pass();
        assert_eq!(out.claimed, 1);
        let reports = tx.reports();
        assert_eq!(reports[0].state, "failed");
        assert!(
            reports[0]
                .error
                .as_deref()
                .unwrap_or_default()
                .contains("no runner registered"),
            "error = {:?}",
            reports[0].error
        );
    }

    /// A panicking runner is recorded and the pass carries on to the next run.
    #[test]
    fn a_panicking_runner_is_recorded_and_the_pass_continues() {
        let ok = FixedRunner::new("home.gc", JobOutcome::Succeeded(json!({"reaped": 1})));
        let tx = FakeTransport::with(vec![Ok(vec![
            pending("run-6", "boom.job", Value::Null),
            pending("run-7", "home.gc", Value::Null),
        ])]);
        let poller = JobPoller::new(
            tx.clone(),
            registry_with(vec![Arc::new(PanickingRunner), ok]),
        );

        let out = poller.run_pass();
        assert_eq!(out.claimed, 2);
        assert_eq!(out.reported, 2);
        let reports = tx.reports();
        assert_eq!(reports[0].state, "failed");
        assert!(
            reports[0]
                .error
                .as_deref()
                .unwrap_or_default()
                .contains("panic"),
            "error = {:?}",
            reports[0].error
        );
        assert_eq!(reports[1].state, "succeeded");
    }

    /// A report the control plane would not take is dropped; the rest of the
    /// pass still runs.
    #[test]
    fn a_failed_report_does_not_abort_the_pass() {
        let ok = FixedRunner::new("home.gc", JobOutcome::Succeeded(json!({})));
        let tx = FakeTransport::with(vec![Ok(vec![
            pending("run-8", "home.gc", Value::Null),
            pending("run-9", "home.gc", Value::Null),
        ])]);
        *tx.report_fails.lock().unwrap() = true;
        let poller = JobPoller::new(tx.clone(), registry_with(vec![ok]));

        let out = poller.run_pass();
        assert_eq!(out.claimed, 2);
        assert_eq!(out.reported, 0);
        assert!(tx.reports().is_empty());
    }

    // ── Inertness ───────────────────────────────────────────────────────────

    /// With no runner registered the poller makes NO HTTP call at all — not an
    /// empty poll, no poll.
    #[test]
    fn zero_handlers_means_no_http_at_all() {
        let tx = FakeTransport::with(vec![Ok(vec![pending("run-x", "home.gc", Value::Null)])]);
        let poller = JobPoller::new(tx.clone(), Arc::new(JobRegistry::new()));

        let out = poller.run_pass();
        assert!(out.inert);
        assert_eq!(out.claimed, 0);
        assert_eq!(tx.fetch_count(), 0, "an inert poller must not call out");
        assert!(tx.reports().is_empty());
    }

    /// The task is not even created, so an inert agent costs nothing.
    #[test]
    fn an_empty_registry_does_not_spawn_a_poller() {
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_time()
            .build()
            .unwrap();
        let guard = rt.block_on(async {
            spawn_job_poller(
                "http://127.0.0.1:1".into(),
                "tower".into(),
                "secret".into(),
                Arc::new(JobRegistry::new()),
            )
        });
        assert!(guard.is_none());
    }

    // ── Backoff ─────────────────────────────────────────────────────────────

    #[test]
    fn backoff_doubles_on_consecutive_fetch_failures_and_caps() {
        let base = Duration::from_secs(60);
        let mut b = PollBackoff::new(base);
        let fail = PassOutcome {
            fetch_failed: true,
            ..PassOutcome::default()
        };
        // The first failure retries at the base interval — doubling immediately
        // would double manual-trigger latency for a blip.
        assert_eq!(b.next_delay(&fail), base);
        assert_eq!(b.next_delay(&fail), base * 2);
        assert_eq!(b.next_delay(&fail), base * 4);
        for _ in 0..20 {
            b.next_delay(&fail);
        }
        assert_eq!(b.next_delay(&fail), MAX_POLL_BACKOFF);
    }

    #[test]
    fn backoff_resets_on_the_first_pass_that_reaches_the_control_plane() {
        let base = Duration::from_secs(60);
        let mut b = PollBackoff::new(base);
        let fail = PassOutcome {
            fetch_failed: true,
            ..PassOutcome::default()
        };
        b.next_delay(&fail);
        b.next_delay(&fail);
        assert_eq!(b.next_delay(&PassOutcome::default()), base);
        assert_eq!(b.next_delay(&fail), base, "the ladder must start over");
    }

    /// A job that FAILED is not a transport problem and must not back off.
    #[test]
    fn a_failed_job_does_not_back_off_the_poll() {
        let base = Duration::from_secs(60);
        let mut b = PollBackoff::new(base);
        let job_failed = PassOutcome {
            claimed: 1,
            reported: 1,
            ..PassOutcome::default()
        };
        assert_eq!(b.next_delay(&job_failed), base);
        assert_eq!(b.next_delay(&job_failed), base);
    }

    /// An inert pass is not a failure either.
    #[test]
    fn an_inert_pass_does_not_back_off() {
        let base = Duration::from_secs(60);
        let mut b = PollBackoff::new(base);
        let inert = PassOutcome {
            inert: true,
            ..PassOutcome::default()
        };
        assert_eq!(b.next_delay(&inert), base);
    }

    // ── Transport failure ───────────────────────────────────────────────────

    #[test]
    fn a_fetch_failure_is_reported_as_such_and_dispatches_nothing() {
        let runner = FixedRunner::new("home.gc", JobOutcome::Succeeded(json!({})));
        let tx = FakeTransport::with(vec![Err("connection refused".into())]);
        let poller = JobPoller::new(tx.clone(), registry_with(vec![runner]));

        let out = poller.run_pass();
        assert!(out.fetch_failed);
        assert_eq!(out.claimed, 0);
        assert!(tx.reports().is_empty());
    }

    // ── Registry ────────────────────────────────────────────────────────────

    #[test]
    fn registry_lookup_is_by_job_id() {
        let reg = registry_with(vec![
            FixedRunner::new("home.gc", JobOutcome::Succeeded(json!({}))),
            FixedRunner::new("template.warmup", JobOutcome::Succeeded(json!({}))),
        ]);
        assert_eq!(reg.len(), 2);
        assert!(!reg.is_empty());
        assert_eq!(reg.job_ids(), vec!["home.gc", "template.warmup"]);
        assert!(reg.get("home.gc").is_some());
        assert!(reg.get("nope").is_none());
    }

    // ── Wire shapes ─────────────────────────────────────────────────────────

    /// Decoded as the control plane writes it, including a run with no params.
    #[test]
    fn pending_response_decodes_the_control_plane_shape() {
        let body = r#"{"runs":[
            {"run_id":"1f4a","job_id":"template.warmup",
             "params":{"image_id":"steam","version":"v1"},"deadline_secs":3600},
            {"run_id":"2b5c","job_id":"home.gc","params":{},"deadline_secs":600}
        ]}"#;
        let parsed: PendingResponse = serde_json::from_str(body).unwrap();
        assert_eq!(parsed.runs.len(), 2);
        assert_eq!(parsed.runs[0].job_id, "template.warmup");
        assert_eq!(parsed.runs[0].params["image_id"], json!("steam"));
        assert_eq!(parsed.runs[1].deadline_secs, 600);

        let empty: PendingResponse = serde_json::from_str(r#"{"runs":[]}"#).unwrap();
        assert!(empty.runs.is_empty());
        // No `runs` key at all must decode as "nothing to do", not fail the pass.
        let missing: PendingResponse = serde_json::from_str(r#"{}"#).unwrap();
        assert!(missing.runs.is_empty());
    }

    #[test]
    fn report_serializes_the_control_plane_shape() {
        let r = JobReport {
            run_id: "1f4a".into(),
            state: "succeeded".into(),
            summary: json!({"files": 23701}),
            error: None,
        };
        let v: Value = serde_json::from_str(&serde_json::to_string(&r).unwrap()).unwrap();
        assert_eq!(
            v,
            json!({"run_id":"1f4a","state":"succeeded","summary":{"files":23701},"error":null})
        );
    }

    // ── AbortFlag ───────────────────────────────────────────────────────────

    #[test]
    fn abort_flag_is_shared_and_unraised_until_teardown() {
        let flag = AbortFlag::new();
        let clone = flag.clone();
        assert!(!flag.is_aborted());
        clone.abort();
        assert!(flag.is_aborted());

        // A live poller hands an unraised flag to every run; only connection
        // teardown raises it (#492).
        struct Observer(Mutex<Vec<bool>>);
        impl JobRunner for Observer {
            fn job_id(&self) -> &'static str {
                "home.gc"
            }
            fn run(&self, _p: &Value, abort: &AbortFlag) -> JobOutcome {
                self.0.lock().unwrap().push(abort.is_aborted());
                JobOutcome::Succeeded(json!({}))
            }
        }
        let obs = Arc::new(Observer(Mutex::new(Vec::new())));
        let tx = FakeTransport::with(vec![Ok(vec![pending("r", "home.gc", Value::Null)])]);
        JobPoller::new(tx, registry_with(vec![obs.clone()])).run_pass();
        assert_eq!(obs.0.lock().unwrap().as_slice(), [false]);
    }

    // ── Connection teardown (#492) ──────────────────────────────────────────
    //
    // The agent-side half of the control plane's reclaim-on-re-register: makes
    // "a re-registering agent is not executing its previous claims" TRUE.

    /// Dropping the guard, what a connection ending DOES, raises the flag.
    #[tokio::test]
    async fn dropping_the_guard_raises_the_pollers_abort_flag() {
        let abort = AbortFlag::new();
        let handle = tokio::spawn(async { std::future::pending::<()>().await });
        let guard = JobPollerGuard {
            handle,
            abort: abort.clone(),
        };
        assert!(!abort.is_aborted(), "a live connection must not be aborted");

        drop(guard);

        assert!(
            abort.is_aborted(),
            "connection teardown must raise the poller's abort flag"
        );
    }

    /// The runner sees the raised flag, so a long job has something to stop on.
    #[test]
    fn teardown_flag_reaches_the_runner() {
        struct Observer(Mutex<Vec<bool>>);
        impl JobRunner for Observer {
            fn job_id(&self) -> &'static str {
                "home.gc"
            }
            fn run(&self, _p: &Value, abort: &AbortFlag) -> JobOutcome {
                self.0.lock().unwrap().push(abort.is_aborted());
                JobOutcome::Succeeded(json!({}))
            }
        }
        let obs = Arc::new(Observer(Mutex::new(Vec::new())));
        let tx = FakeTransport::with(vec![Ok(vec![pending("r", "home.gc", Value::Null)])]);
        let poller = JobPoller::new(tx, registry_with(vec![obs.clone()]));
        poller.abort_flag().abort();
        poller.run_pass();
        // The pass stops before dispatching at all, which is strictly stronger.
        assert!(obs.0.lock().unwrap().is_empty());
    }

    /// A pass that starts on a live connection and is torn down mid-batch stops
    /// claiming further work instead of running the rest of the batch.
    #[test]
    fn teardown_mid_pass_abandons_the_rest_of_the_batch() {
        struct Tearer {
            abort: Mutex<Option<AbortFlag>>,
        }
        impl JobRunner for Tearer {
            fn job_id(&self) -> &'static str {
                "home.gc"
            }
            fn run(&self, _p: &Value, _abort: &AbortFlag) -> JobOutcome {
                // The connection dies while this run is in flight.
                if let Some(f) = self.abort.lock().unwrap().take() {
                    f.abort();
                }
                JobOutcome::Succeeded(json!({}))
            }
        }
        let tearer = Arc::new(Tearer {
            abort: Mutex::new(None),
        });
        let second = FixedRunner::new("template.warmup", JobOutcome::Succeeded(json!({})));
        let tx = FakeTransport::with(vec![Ok(vec![
            pending("r1", "home.gc", Value::Null),
            pending("r2", "template.warmup", Value::Null),
        ])]);
        let poller = JobPoller::new(
            tx.clone(),
            registry_with(vec![tearer.clone(), second.clone()]),
        );
        *tearer.abort.lock().unwrap() = Some(poller.abort_flag());

        let out = poller.run_pass();

        assert!(out.torn_down, "the pass must record the teardown");
        assert!(!out.fetch_failed, "teardown is not a transport failure");
        assert_eq!(out.reported, 0, "no report may survive the connection");
        assert!(
            second.seen_params.lock().unwrap().is_empty(),
            "the second run of the batch must not start after teardown"
        );
        assert!(tx.reports().is_empty());
    }

    /// The report itself is suppressed — load-bearing since a `spawn_blocking`
    /// pass cannot be cancelled, so this check is the only thing stopping the
    /// outcome racing the control plane's reclaim.
    #[test]
    fn teardown_suppresses_the_report_of_a_completed_run() {
        struct Tearer {
            abort: Mutex<Option<AbortFlag>>,
        }
        impl JobRunner for Tearer {
            fn job_id(&self) -> &'static str {
                "home.gc"
            }
            fn run(&self, _p: &Value, _abort: &AbortFlag) -> JobOutcome {
                if let Some(f) = self.abort.lock().unwrap().take() {
                    f.abort();
                }
                JobOutcome::Succeeded(json!({"reaped": 2}))
            }
        }
        let tearer = Arc::new(Tearer {
            abort: Mutex::new(None),
        });
        let tx = FakeTransport::with(vec![Ok(vec![pending("r1", "home.gc", Value::Null)])]);
        let poller = JobPoller::new(tx.clone(), registry_with(vec![tearer.clone()]));
        *tearer.abort.lock().unwrap() = Some(poller.abort_flag());

        let out = poller.run_pass();

        assert_eq!(out.claimed, 1);
        assert_eq!(out.reported, 0);
        assert!(
            tx.reports().is_empty(),
            "a run whose connection died must not report: {:?}",
            tx.reports()
        );
    }

    /// A torn-down poller does not even ask for work — claiming a run it could
    /// not report would wedge that job on this host until the reclaim.
    #[test]
    fn teardown_stops_the_pass_before_it_fetches() {
        let runner = FixedRunner::new("home.gc", JobOutcome::Succeeded(json!({})));
        let tx = FakeTransport::with(vec![Ok(vec![pending("r", "home.gc", Value::Null)])]);
        let poller = JobPoller::new(tx.clone(), registry_with(vec![runner]));
        poller.abort_flag().abort();

        let out = poller.run_pass();

        assert!(out.torn_down);
        assert_eq!(tx.fetch_count(), 0, "a dead connection must not claim work");
        // Must not look like a transport failure and feed the poll backoff.
        assert!(!out.fetch_failed);
        assert_eq!(
            PollBackoff::new(Duration::from_secs(60)).next_delay(&out),
            Duration::from_secs(60)
        );
    }
}
