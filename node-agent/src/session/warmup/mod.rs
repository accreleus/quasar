//! #488 WP2 — the golden-home **warm-up job**: build one per-image template by
//! booting the app once into a throwaway scratch home, letting it settle, and
//! snapshotting what it wrote.
//!
//! Design of record: `docs/design/plans/2026-08-12-488-golden-home-template.md`.
//! Measured payoff: ~35s → ~7s to `app presented` for a first-time user (0.5s
//! is the clone).
//!
//! ## Where this sits
//!
//! * **WP1** (`session::template`) owns the template store on disk: path
//!   resolution, `.meta.json`, the reflink/copy clone ladder, atomic
//!   staging→rename publish. Reached through [`TemplateStoreApi`] /
//!   [`StagingBuildApi`].
//! * **WP3** owns seeding (`session::home`): cloning a published template into
//!   a cold managed home. Nothing here reads or writes a user home; the
//!   warm-up's clone source is always a scratch dir under the staging root.
//!
//! ## Three invariants worth not breaking
//!
//! 1. **A warm-up must never overlap another session (#489).** See [`gate`] —
//!    the reason the job is one long abort-checked state machine, not spawned
//!    steps.
//! 2. **A warm-up never blocks or fails a user launch.** Every failure is a log
//!    line; the image just has no template and users pay today's cold boot.
//! 3. **A warm-up never seeds itself.** The scratch home is provisioned empty,
//!    so a defect washes out on the next rebuild instead of compounding across
//!    generations.

pub mod gate;
pub mod host;
pub mod job;
pub mod quiesce;
pub mod strip;
pub mod verify;

use std::io;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use tracing::{error, info, warn};

pub use gate::{
    AbortCause, GateRefusal, HostActivity, WarmupControl, WarmupGuard, POST_SESSION_QUIET,
};
pub use job::{TemplateReaper, WarmupConnectionGuard, WarmupJobRunner, JOB_ID};
pub use quiesce::{newest_mtime, QuiescenceTracker, DEFAULT_QUIESCE_WINDOW};
pub use strip::{copy_tree_sanitized, is_stripped, rsync_excludes, STRIP_RULES};
pub use verify::{verify_template, TemplateStats, VerifyPolicy};

/// The `skipped` reason a host that may not BUILD templates reports. It names
/// the knob AND says the default, so the jobs viewer answers "why did nothing
/// happen?" without anyone reading the code — the runner stays registered on
/// every host precisely so this line exists.
pub const WARMUP_DISABLED_REASON: &str =
    "QUASAR_TEMPLATE_WARMUP is not set on this host (warm-up builds are opt-in; \
     set QUASAR_TEMPLATE_WARMUP=1 to allow this host to build templates)";

/// The `failed` reason for the split-namespace refusal (see
/// [`TemplateStoreApi::cross_filesystem`]).
pub const WARMUP_CROSSFS_REASON: &str =
    "the template root and the home root are on different filesystems — on a containerized \
     agent this means the template directory is NOT bind-mounted, so the build would write \
     into the container overlay and be orphaned. Bind-mount the template root into the agent \
     (deploy/docker-compose.yml), or set QUASAR_TEMPLATE_ALLOW_CROSSFS=1 to build anyway";

/// `.meta.json` schema version. Mirrors WP1's `TEMPLATE_META_SCHEMA`.
pub const TEMPLATE_META_SCHEMA: u32 = 1;

/// §7.1 free-space guard: what we assume a template will cost before we have
/// ever built one for this image. The measured Steam template is 2.5 GiB; the
/// guard wants `3 ×` that to hold staging + old + new across the swap.
pub const DEFAULT_EXPECTED_TEMPLATE_BYTES: u64 = 3 << 30;

/// §7.3 `QUASAR_TEMPLATE_MIN_FREE_BYTES` default: 20 GiB.
pub const DEFAULT_MIN_FREE_BYTES: u64 = 21_474_836_480;

/// How often the wait loops wake to re-check the abort flag and the deadline.
/// Small enough that a user launch never waits on a warm-up for a perceptible
/// time, large enough not to spin.
const POLL: Duration = Duration::from_millis(250);

/// How often the settle/quiesce phase re-scans the scratch tree for its newest
/// mtime. A full `stat` walk of ~24 000 files is cheap but not free.
const QUIESCE_SCAN_INTERVAL: Duration = Duration::from_secs(1);

// ---------------------------------------------------------------------------
// WP1 seam
// ---------------------------------------------------------------------------

/// The `.meta.json` commit record, mirroring WP1's `template::TemplateMeta`
/// field for field. Duplicated (not imported) so this package builds and tests
/// independently of WP1's branch; reconciled only by the adapter on
/// [`TemplateStoreApi`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TemplateMeta {
    pub image_id: String,
    pub registry_ref: String,
    pub version: String,
    pub digest: String,
    pub built_at: u64,
    pub bytes: u64,
    pub files: u64,
    pub agent_version: String,
    pub schema: u32,
}

/// The half of WP1's `template::TemplateStore` this job needs. Adapted onto
/// this trait mechanically by [`StoreAdapter`] / [`StagingAdapter`] below.
pub trait TemplateStoreApi: Send + Sync {
    /// The published template's commit record, or `None` for "absent" — which
    /// covers no template, a missing/corrupt/truncated `.meta.json`, and a
    /// schema mismatch. Never an error: an unreadable template is a template
    /// that gets rebuilt, not a failure.
    fn meta(&self, image_id: &str) -> Option<TemplateMeta>;

    /// §7.1: is there room to build? `max(3 × expected_bytes, min_free_bytes)`
    /// free on the template filesystem.
    fn can_build(&self, expected_bytes: u64, min_free_bytes: u64) -> bool;

    /// Open a staging build. The returned handle owns the staging directory
    /// until it is published or discarded — it is never cleaned up implicitly.
    fn begin_build(&self, image_id: &str, version: &str) -> io::Result<Box<dyn StagingBuildApi>>;

    /// §3.2: drop a template whose image was uninstalled.
    fn remove(&self, image_id: &str) -> io::Result<()>;

    /// Whether the template root and the home root sit on DIFFERENT filesystems.
    ///
    /// §4.2 tier 2 (a cross-filesystem full copy) tolerably degrades SEEDING,
    /// but here it also means the container is not seeing the host's template
    /// directory through a bind mount: templates and staging trees are written
    /// into the container overlay, invisible to the host, orphaned on every
    /// redeploy, and unreflinkable into a home. A warm-up BUILD is therefore
    /// refused (`job.rs`) — why this lives on the trait, not `template.rs`.
    ///
    /// Defaults to `false` so a test fake states only what it cares about.
    fn cross_filesystem(&self) -> bool {
        false
    }
}

/// A staging build in progress (WP1's `template::StagingBuild`).
pub trait StagingBuildApi: Send {
    /// The staging root. The warm-up puts its scratch home here so the whole
    /// job's disk footprint lives under one directory that `discard` removes.
    fn path(&self) -> &Path;
    /// The (empty) directory the sanitized snapshot is assembled into.
    fn home_dir(&self) -> PathBuf;
    /// Write `.meta.json` last, then atomically swap into place.
    fn publish(self: Box<Self>, meta: TemplateMeta) -> io::Result<PathBuf>;
    /// Remove the staging tree, leaving any previously published template
    /// untouched.
    fn discard(self: Box<Self>) -> io::Result<()>;
}

// ---------------------------------------------------------------------------
// Host seam (session launch + image inspection)
// ---------------------------------------------------------------------------

/// What the warm-up needs from the agent's session/container machinery. A trait
/// so the whole job — gate, timeouts, settle, quiescence, teardown ordering,
/// sanitize, verify, publish — is unit tested with no GPU, no docker, and no
/// GStreamer.
pub trait WarmupHost: Send + Sync {
    /// §3.3 step 4: resolve the container-side home path **from the image**, not
    /// from a session. `image_ensure` carries no `home_container_path`, and the
    /// image is the more correct source anyway for a per-image artifact.
    fn container_home(&self, image: &str) -> Option<String>;

    /// §3.3 step 5: launch an ordinary session-shaped run — same compositor,
    /// same `SessionRunner` path, the image's default `CMD` (i.e. `-bigpicture`,
    /// per §1.4.4), the scratch home bind-mounted at the resolved container home
    /// path.
    fn launch(&self, req: &WarmupLaunch) -> Result<Box<dyn WarmupSession>, String>;
}

/// The launch request handed to [`WarmupHost::launch`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WarmupLaunch {
    pub image_id: String,
    pub image: String,
    /// Host-side scratch home (already created, already owned by the app uid).
    pub scratch_home: PathBuf,
    /// Container-side mount point for it.
    pub container_home: String,
}

/// A warm-up session in flight.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WarmupSessionState {
    /// The app container is up and has not presented yet.
    Booting,
    /// #484's `app presented` predicate has latched (`gen0_app_presented`).
    Presented,
    /// The session ended on its own — a container exit, a pipeline failure, the
    /// boot watchdog. Carries the reason for the log.
    Ended(String),
}

pub trait WarmupSession: Send {
    /// Non-blocking: the session's current state.
    fn state(&mut self) -> WarmupSessionState;
    /// §3.3 step 8: stop the container and wait for **full** teardown before the
    /// caller touches the tree — both for #489 and because Steam flushes on
    /// exit.
    fn stop_and_wait(&mut self);
}

/// Injected clock, so the job's timing is exercised in tests without sleeping.
pub trait Clock: Send + Sync {
    fn now(&self) -> Instant;
    fn sleep(&self, d: Duration);
}

/// The production clock.
#[derive(Debug, Default, Clone, Copy)]
pub struct SystemClock;

impl Clock for SystemClock {
    fn now(&self) -> Instant {
        Instant::now()
    }
    fn sleep(&self, d: Duration) {
        std::thread::sleep(d);
    }
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

/// §7.3 knobs, resolved once at agent startup.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WarmupConfig {
    /// `QUASAR_TEMPLATE_WARMUP` — **opt-in, default OFF**. `1`/`true`/`yes`/`on`
    /// ⇒ this host may BUILD templates; anything else (including unset) ⇒ it
    /// only ever consumes templates built elsewhere on the same box.
    ///
    /// It defaults off because a build is a full session that takes the host's
    /// single encode slot for minutes (#489 serializes it against every user
    /// launch), and an unconfigured host was rebuilding a template on every
    /// image `ready` without anyone asking for it.
    pub enabled: bool,
    /// `QUASAR_TEMPLATE_SETTLE_SECS`, default 60 (the §1.1 measured value).
    pub settle: Duration,
    /// `QUASAR_TEMPLATE_WARMUP_TIMEOUT_SECS`, default 600. `None` ⇒ no whole-job
    /// bound (the runner's own `QUASAR_APP_BOOT_TIMEOUT_SECS` watchdog still
    /// applies to the boot phase).
    pub job_timeout: Option<Duration>,
    /// `QUASAR_TEMPLATE_MIN_FREE_BYTES`, default 20 GiB.
    pub min_free_bytes: u64,
    /// The write-quiescence window (§3.3 step 7). Not an operator knob.
    pub quiesce_window: Duration,
    /// The §6.3 floor. Not an operator knob; relaxed only in tests.
    pub verify: VerifyPolicy,
}

impl Default for WarmupConfig {
    fn default() -> Self {
        WarmupConfig {
            enabled: false,
            settle: Duration::from_secs(60),
            job_timeout: Some(Duration::from_secs(600)),
            min_free_bytes: DEFAULT_MIN_FREE_BYTES,
            quiesce_window: DEFAULT_QUIESCE_WINDOW,
            verify: VerifyPolicy::default(),
        }
    }
}

impl WarmupConfig {
    pub fn from_env() -> Self {
        let d = WarmupConfig::default();
        WarmupConfig {
            // OPT-IN, not opt-out: an unset (or unparseable) value leaves this
            // host a template CONSUMER. Flipping this to opt-out is what let an
            // unconfigured host rebuild templates unasked.
            enabled: matches!(
                std::env::var("QUASAR_TEMPLATE_WARMUP")
                    .map(|v| v.trim().to_ascii_lowercase())
                    .ok()
                    .as_deref(),
                Some("1") | Some("true") | Some("yes") | Some("on")
            ),
            settle: env_secs("QUASAR_TEMPLATE_SETTLE_SECS", d.settle).unwrap_or(Duration::ZERO),
            job_timeout: env_secs(
                "QUASAR_TEMPLATE_WARMUP_TIMEOUT_SECS",
                Duration::from_secs(600),
            ),
            min_free_bytes: std::env::var("QUASAR_TEMPLATE_MIN_FREE_BYTES")
                .ok()
                .and_then(|v| v.trim().parse::<u64>().ok())
                .unwrap_or(DEFAULT_MIN_FREE_BYTES),
            ..d
        }
    }
}

/// Parse a `_SECS` knob. `0` means "disabled" and yields `None`; an
/// unparseable value keeps the default rather than silently disabling a bound.
/// An empty-after-trim value (docker-compose's `${VAR:-}` idiom for "no
/// override") falls back to the default silently, like an unset var (mirrors
/// `env_mic_jitter_ms`, #496 F5); only a genuinely non-empty, unparseable
/// value warns.
fn env_secs(key: &str, default: Duration) -> Option<Duration> {
    let secs = match std::env::var(key) {
        Ok(v) if v.trim().is_empty() => default.as_secs(),
        Ok(v) => match v.trim().parse::<u64>() {
            Ok(n) => n,
            Err(_) => {
                warn!(
                    token = "knob-invalid-warmup-number",
                    "{key}={v:?} is not a number; using the default {default:?}"
                );
                default.as_secs()
            }
        },
        Err(_) => default.as_secs(),
    };
    (secs > 0).then(|| Duration::from_secs(secs))
}

// ---------------------------------------------------------------------------
// The job
// ---------------------------------------------------------------------------

/// What a host asked us to warm up. Built from the `image_ensure` → `ready`
/// terminal (§3.2), which carries everything the job needs.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WarmupRequest {
    pub image_id: String,
    pub registry_ref: String,
    pub version: String,
}

/// §3.2/§3.3: should this `ready` report trigger a warm-up?
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WarmupDecision {
    Build,
    /// A template for this image already exists at this exact version.
    SkipUpToDate,
    /// `QUASAR_TEMPLATE_WARMUP=0`.
    SkipDisabled,
}

/// The version comparison of §3.2. Staleness is self-healing precisely because
/// this runs on **every** `ready`, so an image whose ref/version changed
/// re-warms without any extra bookkeeping.
pub fn decide_warmup(
    enabled: bool,
    existing: Option<&TemplateMeta>,
    req: &WarmupRequest,
) -> WarmupDecision {
    if !enabled {
        return WarmupDecision::SkipDisabled;
    }
    match existing {
        // The registry ref is compared too: an image re-pointed at a different
        // ref under the same version string is a different image, and §3.2's
        // trigger is `Store.Update` re-adopting `installed_images.registry_ref`.
        Some(m) if m.version == req.version && m.registry_ref == req.registry_ref => {
            WarmupDecision::SkipUpToDate
        }
        _ => WarmupDecision::Build,
    }
}

/// Why a warm-up did not produce a template. None of these ever reach a user.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WarmupError {
    /// A user launch (or agent shutdown) took priority — §3.4. Rescheduled.
    Aborted,
    /// The whole-job bound expired.
    TimedOut,
    /// A precondition said "not now, and not because anything is wrong":
    /// disabled, up to date, no room, no resolvable container home.
    Skipped(String),
    /// The job ran and failed. The previous template (if any) is untouched.
    Failed(String),
}

impl std::fmt::Display for WarmupError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            WarmupError::Aborted => write!(f, "aborted for a user session launch"),
            WarmupError::TimedOut => write!(f, "job timeout expired"),
            WarmupError::Skipped(r) => write!(f, "skipped: {r}"),
            WarmupError::Failed(r) => write!(f, "{r}"),
        }
    }
}

/// A published template.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WarmupOutcome {
    pub image_id: String,
    pub version: String,
    pub stats: TemplateStats,
    pub elapsed: Duration,
    pub presented_after: Duration,
}

/// The §3.3 job. Borrows its collaborators; owns no state between runs.
pub struct WarmupJob<'a> {
    pub cfg: &'a WarmupConfig,
    pub store: &'a dyn TemplateStoreApi,
    pub host: &'a dyn WarmupHost,
    pub clock: &'a dyn Clock,
    /// The app uid/gid the scratch home is owned by (`QUASAR_APP_PUID`/`PGID`).
    /// `None` leaves ownership as created — correct when the agent is not root.
    pub app_uid_gid: Option<(u32, u32)>,
}

impl WarmupJob<'_> {
    /// Run one warm-up to completion, holding `guard` (the §3.4 host-global
    /// gate) for its whole duration.
    ///
    /// Holding the guard already establishes the zero-live-sessions
    /// precondition; this function covers the other half of §3.4 — noticing an
    /// abort at every phase boundary and wait loop, and unwinding cleanly.
    pub fn run(
        &self,
        req: &WarmupRequest,
        guard: &WarmupGuard<'_>,
    ) -> Result<WarmupOutcome, WarmupError> {
        let started = self.clock.now();
        let deadline = self.cfg.job_timeout.map(|t| started + t);

        match decide_warmup(
            self.cfg.enabled,
            self.store.meta(&req.image_id).as_ref(),
            req,
        ) {
            WarmupDecision::Build => {}
            WarmupDecision::SkipDisabled => {
                return Err(WarmupError::Skipped(WARMUP_DISABLED_REASON.into()))
            }
            WarmupDecision::SkipUpToDate => {
                return Err(WarmupError::Skipped(format!(
                    "template for {} is already at version {}",
                    req.image_id, req.version
                )))
            }
        }

        // §7.1: refuse to build rather than fill the disk. The expected size
        // is the previous template's if we have one — no guessing for a re-warm.
        let expected = self
            .store
            .meta(&req.image_id)
            .map(|m| m.bytes)
            .filter(|b| *b > 0)
            .unwrap_or(DEFAULT_EXPECTED_TEMPLATE_BYTES);
        if !self.store.can_build(expected, self.cfg.min_free_bytes) {
            return Err(WarmupError::Skipped(format!(
                "not enough free space on the template filesystem for a ~{} MiB template",
                expected / (1 << 20)
            )));
        }

        // §3.3 step 4: resolved BEFORE staging, so an unwarmable image costs
        // nothing.
        let Some(container_home) = self.host.container_home(&req.registry_ref) else {
            return Err(WarmupError::Skipped(format!(
                "image {} exposes neither Config.Env HOME nor Config.WorkingDir; \
                 no container home to snapshot",
                req.registry_ref
            )));
        };

        let staging = self
            .store
            .begin_build(&req.image_id, &req.version)
            .map_err(|e| WarmupError::Failed(format!("staging: {e}")))?;

        // Everything from here on must clean the staging dir up on the way out.
        match self.build(
            req,
            &container_home,
            staging.as_ref(),
            guard,
            started,
            deadline,
        ) {
            Ok((stats, presented_after)) => {
                let meta = TemplateMeta {
                    image_id: req.image_id.clone(),
                    registry_ref: req.registry_ref.clone(),
                    version: req.version.clone(),
                    digest: String::new(),
                    built_at: SystemTime::now()
                        .duration_since(UNIX_EPOCH)
                        .unwrap_or_default()
                        .as_secs(),
                    bytes: stats.bytes,
                    files: stats.files,
                    agent_version: env!("CARGO_PKG_VERSION").to_string(),
                    schema: TEMPLATE_META_SCHEMA,
                };
                staging
                    .publish(meta)
                    .map_err(|e| WarmupError::Failed(format!("publish: {e}")))?;
                Ok(WarmupOutcome {
                    image_id: req.image_id.clone(),
                    version: req.version.clone(),
                    stats,
                    elapsed: self.clock.now().saturating_duration_since(started),
                    presented_after,
                })
            }
            Err(e) => {
                if let Err(err) = staging.discard() {
                    warn!(
                        token = "template-staging-cleanup-failed",
                        "template: staging cleanup for {} failed: {err}", req.image_id
                    );
                }
                Err(e)
            }
        }
    }

    /// Steps 3 and 5-9: scratch home, launch, present, settle, quiesce, stop,
    /// sanitize, verify. Split out so `run` owns "clean the staging dir on
    /// every error path" in one place.
    fn build(
        &self,
        req: &WarmupRequest,
        container_home: &str,
        staging: &dyn StagingBuildApi,
        guard: &WarmupGuard<'_>,
        started: Instant,
        deadline: Option<Instant>,
    ) -> Result<(TemplateStats, Duration), WarmupError> {
        // §3.3 step 3: the scratch home lives INSIDE the staging tree, so a
        // discarded build takes it with it — nothing is left under `homes/`.
        let scratch = staging.path().join("scratch-home");
        std::fs::create_dir_all(&scratch)
            .map_err(|e| WarmupError::Failed(format!("scratch home {}: {e}", scratch.display())))?;
        if let Some((uid, gid)) = self.app_uid_gid {
            if let Err(e) = std::os::unix::fs::chown(&scratch, Some(uid), Some(gid)) {
                warn!(
                    token = "template-scratch-chown-failed",
                    "template: could not chown the scratch home to {uid}:{gid} ({e}); \
                     continuing — the app container may fail to write it"
                );
            }
        }

        info!(
            "template: warm-up starting for {} v{} (scratch={})",
            req.image_id,
            req.version,
            scratch.display()
        );

        let launch = WarmupLaunch {
            image_id: req.image_id.clone(),
            image: req.registry_ref.clone(),
            scratch_home: scratch.clone(),
            container_home: container_home.to_string(),
        };
        let mut session = self
            .host
            .launch(&launch)
            .map_err(|e| WarmupError::Failed(format!("launch: {e}")))?;

        // A session is live from here on: every exit path must stop it before
        // returning, or a warm-up container outlives its job and overlaps the
        // next user launch (#489).
        let result = self.drive(req, session.as_mut(), &scratch, guard, started, deadline);
        session.stop_and_wait();

        let presented_after = result?;

        // §3.3 step 9: reached only after step 8's full teardown, before
        // anything reads the tree.
        let dest = staging.home_dir();
        std::fs::create_dir_all(&dest)
            .map_err(|e| WarmupError::Failed(format!("staging home {}: {e}", dest.display())))?;
        let copy = copy_tree_sanitized(&scratch, &dest)
            .map_err(|e| WarmupError::Failed(format!("snapshot: {e}")))?;
        if !copy.special_skipped.is_empty() {
            warn!(
                token = "template-special-files-skipped",
                "template: {} socket(s)/FIFO(s) outside the strip list were skipped: {:?} \
                 — the §6.1 strip list may need updating",
                copy.special_skipped.len(),
                copy.special_skipped
            );
        }

        let stats = verify_template(&dest, &self.cfg.verify).map_err(|violations| {
            error!(
                token = "template-verification-refused",
                "template: verification REFUSED to publish {} v{}: {}",
                req.image_id,
                req.version,
                violations.join("; ")
            );
            WarmupError::Failed(format!(
                "verification failed ({} violation(s))",
                violations.len()
            ))
        })?;

        // The scratch home has served its purpose; drop it before the publish so
        // the disk high-water mark is lower and a slow delete cannot delay the
        // atomic swap.
        if let Err(e) = std::fs::remove_dir_all(&scratch) {
            warn!(
                token = "template-scratch-rm-failed",
                "template: could not remove the scratch home {}: {e}",
                scratch.display()
            );
        }
        Ok((stats, presented_after))
    }

    /// Steps 6–8: wait for `app presented`, settle, require write quiescence.
    /// Returns how long the app took to present.
    fn drive(
        &self,
        req: &WarmupRequest,
        session: &mut dyn WarmupSession,
        scratch: &Path,
        guard: &WarmupGuard<'_>,
        started: Instant,
        deadline: Option<Instant>,
    ) -> Result<Duration, WarmupError> {
        // Step 6: `app presented`, bounded by the job timeout on top of the
        // runner's own boot watchdog.
        let presented_at = loop {
            self.checkpoint(guard, deadline)?;
            match session.state() {
                WarmupSessionState::Presented => break self.clock.now(),
                WarmupSessionState::Ended(reason) => {
                    return Err(WarmupError::Failed(format!(
                        "session ended before presenting: {reason}"
                    )))
                }
                WarmupSessionState::Booting => self.clock.sleep(POLL),
            }
        };
        let presented_after = presented_at.saturating_duration_since(started);
        info!(
            "template: warm-up app presented after {} ms; settling {}s",
            presented_after.as_millis(),
            self.cfg.settle.as_secs()
        );

        // Step 7a: settle.
        let settle_until = presented_at + self.cfg.settle;
        while self.clock.now() < settle_until {
            self.checkpoint(guard, deadline)?;
            if let WarmupSessionState::Ended(reason) = session.state() {
                return Err(WarmupError::Failed(format!(
                    "session ended while settling: {reason}"
                )));
            }
            self.clock.sleep(POLL);
        }

        // Step 7b: write quiescence. Settle-then-quiesce, not settle-alone, is
        // what keeps a mid-update Steam out of the snapshot.
        let quiesce_started = self.clock.now();
        let mut tracker = QuiescenceTracker::new(self.cfg.quiesce_window);
        loop {
            self.checkpoint(guard, deadline)?;
            let Some(newest) = newest_mtime(scratch) else {
                return Err(WarmupError::Failed(
                    "scratch home became unreadable while waiting for quiescence".into(),
                ));
            };
            if tracker.observe(newest, self.clock.now()) {
                break;
            }
            if let WarmupSessionState::Ended(reason) = session.state() {
                return Err(WarmupError::Failed(format!(
                    "session ended while waiting for quiescence: {reason}"
                )));
            }
            self.clock.sleep(QUIESCE_SCAN_INTERVAL);
        }
        info!(
            "template: warm-up quiesced after {}s; snapshotting {} v{}",
            self.clock
                .now()
                .saturating_duration_since(quiesce_started)
                .as_secs(),
            req.image_id,
            req.version
        );
        Ok(presented_after)
    }

    /// The two conditions checked at every phase boundary and inside every wait
    /// loop: has a user launch pre-empted us (§3.4), and has the whole-job bound
    /// expired (§7.3)?
    fn checkpoint(
        &self,
        guard: &WarmupGuard<'_>,
        deadline: Option<Instant>,
    ) -> Result<(), WarmupError> {
        if guard.aborted() {
            return Err(WarmupError::Aborted);
        }
        match deadline {
            Some(d) if self.clock.now() >= d => Err(WarmupError::TimedOut),
            _ => Ok(()),
        }
    }
}

/// One place that turns a job result into the §7.2 log line, so the scheduler
/// and any future caller phrase the same outcome the same way.
pub fn log_outcome(req: &WarmupRequest, result: &Result<WarmupOutcome, WarmupError>) {
    match result {
        Ok(o) => info!(
            "template: built {} v{} — {} files, {} MiB, {}s (presented after {} ms)",
            o.image_id,
            o.version,
            o.stats.files,
            o.stats.bytes / (1 << 20),
            o.elapsed.as_secs(),
            o.presented_after.as_millis(),
        ),
        Err(WarmupError::Skipped(reason)) => info!(
            "template: no warm-up for {} v{} — {reason}",
            req.image_id, req.version
        ),
        Err(e) => error!(
            token = "template-warmup-failed",
            "template: warm-up FAILED for {} v{}: {e} (previous template retained)",
            req.image_id,
            req.version
        ),
    }
}

// ---------------------------------------------------------------------------
// Scheduling — owned by the jobs framework, not by this module
// ---------------------------------------------------------------------------
//
// The pending set, backoff ladder, and image-`ready` trigger live in the
// background-jobs framework (design `2026-08-12-jobs-framework-and-viewer.md`
// §8.3): `job_runs` rows + the dispatcher's tick, `job_runs.attempt` +
// `scheduled_for` (survives a reconnect), and the control plane's
// `Ensurer.AgentImageState` -> `Dispatcher.Enqueue("template.warmup", ...)`. A
// gate refusal surfaces as `JobOutcome::Deferred(reason)`, visible to an
// operator rather than only a log line.
//
// What stays here is [`gate`]: the #489 safety mechanism reads live agent-loop
// state and is structurally in-process. The framework only *requests* a run —
// [`job::WarmupJobRunner`] connects the two. The one image-lifecycle duty that
// stays agent-side is dropping a template whose image was uninstalled (§3.2
// row 3) — [`job::TemplateReaper`].

/// §3.3 step 2: while a warm-up holds the gate it uses a GPU and an encoder, so
/// the host reports one fewer encode slot so admission control cannot
/// overcommit it.
///
/// **Rides the existing `capacity` message's `encode_slots_total` field — no
/// wire change** (§5). Taken from the first GPU that has one: a warm-up is not
/// scheduled by the control plane, so it is not pinned to an index, only to
/// this host's configured render node.
pub fn apply_encode_slot_reservation(gpus: &mut [crate::messages::GpuCapacity], reserved: bool) {
    if !reserved {
        return;
    }
    if let Some(gpu) = gpus.iter_mut().find(|g| g.encode_slots_total > 0) {
        gpu.encode_slots_total -= 1;
    }
}

/// Construct the template store for this host, or `None` when the feature is
/// off/misconfigured (empty or inside-the-home-root template root — §7.3's
/// ERROR-and-disable rule — or an uncreatable root). Emits the `clone mode:`
/// startup log line itself.
pub fn resolve_store(home_root: &str) -> Option<Arc<dyn TemplateStoreApi>> {
    crate::session::template::TemplateStore::resolve_from_env(Path::new(home_root))
        .map(|s| Arc::new(StoreAdapter(s)) as Arc<dyn TemplateStoreApi>)
}

/// Adapts WP1's concrete `template::TemplateStore` onto [`TemplateStoreApi`] —
/// the mechanical mapping described on that trait's doc comment.
struct StoreAdapter(crate::session::template::TemplateStore);

impl TemplateStoreApi for StoreAdapter {
    fn meta(&self, image_id: &str) -> Option<TemplateMeta> {
        self.0.meta(image_id).map(|m| TemplateMeta {
            image_id: m.image_id,
            registry_ref: m.registry_ref,
            version: m.version,
            digest: m.digest,
            built_at: m.built_at,
            bytes: m.bytes,
            files: m.files,
            agent_version: m.agent_version,
            schema: m.schema,
        })
    }

    fn can_build(&self, expected_bytes: u64, min_free_bytes: u64) -> bool {
        self.0.can_build(expected_bytes, min_free_bytes)
    }

    fn begin_build(&self, image_id: &str, version: &str) -> io::Result<Box<dyn StagingBuildApi>> {
        let inner = self.0.begin_build(image_id, version)?;
        Ok(Box::new(StagingAdapter {
            store: self.0.clone(),
            inner,
        }))
    }

    fn remove(&self, image_id: &str) -> io::Result<()> {
        self.0.remove(image_id)
    }

    fn cross_filesystem(&self) -> bool {
        self.0.cross_filesystem()
    }
}

/// Adapts WP1's `template::StagingBuild` onto [`StagingBuildApi`], and
/// forwards `publish`/`discard` to the owning `TemplateStore`.
struct StagingAdapter {
    store: crate::session::template::TemplateStore,
    inner: crate::session::template::StagingBuild,
}

impl StagingBuildApi for StagingAdapter {
    fn path(&self) -> &Path {
        self.inner.path()
    }

    fn home_dir(&self) -> PathBuf {
        self.inner.home_dir()
    }

    fn publish(self: Box<Self>, meta: TemplateMeta) -> io::Result<PathBuf> {
        let full = crate::session::template::TemplateMeta {
            image_id: meta.image_id,
            registry_ref: meta.registry_ref,
            version: meta.version,
            digest: meta.digest,
            built_at: meta.built_at,
            bytes: meta.bytes,
            files: meta.files,
            agent_version: meta.agent_version,
            schema: meta.schema,
        };
        self.store.publish(self.inner, full)
    }

    fn discard(self: Box<Self>) -> io::Result<()> {
        self.inner.discard()
    }
}

#[cfg(test)]
mod tests;
