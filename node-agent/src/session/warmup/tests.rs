//! #488 WP2 unit tests.
//!
//! Everything the job does apart from the launch itself is exercised here
//! against fakes: no GPU, no docker, no GStreamer, no sleeping. The launch
//! seam ([`WarmupHost`]) is what hardware verification covers (§9's devbox
//! acceptance protocol).

use super::*;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Mutex;

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

/// A clock that only moves when something sleeps on it.
struct FakeClock {
    now: Mutex<Instant>,
}

impl FakeClock {
    fn new() -> Self {
        FakeClock {
            now: Mutex::new(Instant::now()),
        }
    }
}

impl Clock for FakeClock {
    fn now(&self) -> Instant {
        *self.now.lock().unwrap()
    }
    fn sleep(&self, d: Duration) {
        let mut n = self.now.lock().unwrap();
        *n += d;
    }
}

#[derive(Default)]
struct StoreLog {
    published: Vec<TemplateMeta>,
    discarded: usize,
    removed: Vec<String>,
}

struct FakeStore {
    root: tempfile::TempDir,
    existing: Option<TemplateMeta>,
    can_build: bool,
    cross_fs: bool,
    log: Arc<Mutex<StoreLog>>,
}

impl FakeStore {
    fn new() -> Self {
        FakeStore {
            root: tempfile::tempdir().unwrap(),
            existing: None,
            can_build: true,
            cross_fs: false,
            log: Arc::new(Mutex::new(StoreLog::default())),
        }
    }
}

struct FakeStaging {
    root: PathBuf,
    log: Arc<Mutex<StoreLog>>,
}

impl TemplateStoreApi for FakeStore {
    fn meta(&self, _image_id: &str) -> Option<TemplateMeta> {
        self.existing.clone()
    }
    fn can_build(&self, _expected_bytes: u64, _min_free_bytes: u64) -> bool {
        self.can_build
    }
    fn begin_build(&self, image_id: &str, _version: &str) -> io::Result<Box<dyn StagingBuildApi>> {
        let root = self.root.path().join(format!(".staging/{image_id}"));
        std::fs::create_dir_all(&root)?;
        Ok(Box::new(FakeStaging {
            root,
            log: self.log.clone(),
        }))
    }
    fn remove(&self, image_id: &str) -> io::Result<()> {
        self.log.lock().unwrap().removed.push(image_id.to_string());
        Ok(())
    }
    fn cross_filesystem(&self) -> bool {
        self.cross_fs
    }
}

impl StagingBuildApi for FakeStaging {
    fn path(&self) -> &Path {
        &self.root
    }
    fn home_dir(&self) -> PathBuf {
        self.root.join("home")
    }
    fn publish(self: Box<Self>, meta: TemplateMeta) -> io::Result<PathBuf> {
        self.log.lock().unwrap().published.push(meta);
        Ok(self.root.clone())
    }
    fn discard(self: Box<Self>) -> io::Result<()> {
        self.log.lock().unwrap().discarded += 1;
        let _ = std::fs::remove_dir_all(&self.root);
        Ok(())
    }
}

/// What a fake session does to the scratch home when it "presents".
type Populate = Arc<dyn Fn(&Path) + Send + Sync>;

struct FakeHost {
    container_home: Option<String>,
    launch_err: Option<String>,
    /// How many `state()` polls the app takes to present.
    polls_to_present: usize,
    /// `Some(reason)` ⇒ the session ends instead of presenting.
    ends_with: Option<String>,
    populate: Option<Populate>,
    launched: Arc<AtomicUsize>,
    stopped: Arc<AtomicUsize>,
}

impl FakeHost {
    fn new() -> Self {
        FakeHost {
            container_home: Some("/home/quasar".into()),
            launch_err: None,
            polls_to_present: 1,
            ends_with: None,
            populate: None,
            launched: Arc::new(AtomicUsize::new(0)),
            stopped: Arc::new(AtomicUsize::new(0)),
        }
    }
}

struct FakeSession {
    polls: usize,
    polls_to_present: usize,
    ends_with: Option<String>,
    scratch: PathBuf,
    populate: Option<Populate>,
    stopped: Arc<AtomicUsize>,
}

impl WarmupHost for FakeHost {
    fn container_home(&self, _image: &str) -> Option<String> {
        self.container_home.clone()
    }
    fn launch(&self, req: &WarmupLaunch) -> Result<Box<dyn WarmupSession>, String> {
        if let Some(e) = &self.launch_err {
            return Err(e.clone());
        }
        self.launched.fetch_add(1, Ordering::SeqCst);
        Ok(Box::new(FakeSession {
            polls: 0,
            polls_to_present: self.polls_to_present,
            ends_with: self.ends_with.clone(),
            scratch: req.scratch_home.clone(),
            populate: self.populate.clone(),
            stopped: self.stopped.clone(),
        }))
    }
}

impl WarmupSession for FakeSession {
    fn state(&mut self) -> WarmupSessionState {
        if let Some(r) = &self.ends_with {
            return WarmupSessionState::Ended(r.clone());
        }
        self.polls += 1;
        if self.polls >= self.polls_to_present {
            if self.polls == self.polls_to_present {
                if let Some(p) = &self.populate {
                    p(&self.scratch);
                }
            }
            WarmupSessionState::Presented
        } else {
            WarmupSessionState::Booting
        }
    }
    fn stop_and_wait(&mut self) {
        self.stopped.fetch_add(1, Ordering::SeqCst);
        // §3.3 step 8: the snapshot must happen strictly after teardown. This
        // marker written on the way down proves the ordering in the published
        // template.
        if self.scratch.is_dir() {
            let _ = std::fs::write(self.scratch.join(".stopped-marker"), b"flushed on exit");
        }
    }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

fn write(root: &Path, rel: &str, body: &str) {
    let p = root.join(rel);
    std::fs::create_dir_all(p.parent().unwrap()).unwrap();
    std::fs::write(p, body).unwrap();
}

/// A scratch home shaped like the §1.1 measurement, small enough to be a
/// fixture: the kept value, strip-list entries, and one symlink.
fn steam_like_home(scratch: &Path) {
    write(scratch, ".local/share/Steam/steam.sh", "#!/bin/sh\n");
    write(scratch, ".local/share/Steam/config/config.vdf", "\"CM\"\n");
    write(scratch, ".local/share/Steam/ubuntu12_32/steam", "elf");
    write(scratch, ".steam/registry.vdf", "reg");
    write(scratch, ".cache/nvidia/GLCache/blob", "gpu-specific");
    write(scratch, ".config/pulse/cookie", "secret");
    write(scratch, ".steam/steam.token", "0123456789abcdef");
    write(scratch, ".local/share/Steam/logs/bootstrap.txt", "log");
    let _ = std::os::unix::fs::symlink(
        "/home/quasar/.local/share/Steam",
        scratch.join(".steam/steam"),
    );
}

fn test_cfg() -> WarmupConfig {
    WarmupConfig {
        enabled: true,
        settle: Duration::from_secs(2),
        job_timeout: Some(Duration::from_secs(600)),
        min_free_bytes: 0,
        quiesce_window: Duration::from_secs(2),
        verify: VerifyPolicy {
            min_bytes: 1,
            max_bytes: 1 << 30,
            min_files: 1,
            marker: ".local/share/Steam/steam.sh",
            reject_root_owned: false,
        },
    }
}

fn req() -> WarmupRequest {
    WarmupRequest {
        image_id: "steam".into(),
        registry_ref: "ghcr.io/accreleus/quasar-steam@sha256:009ad46".into(),
        version: "v2".into(),
    }
}

fn meta_at(version: &str, registry_ref: &str) -> TemplateMeta {
    TemplateMeta {
        image_id: "steam".into(),
        registry_ref: registry_ref.into(),
        version: version.into(),
        digest: String::new(),
        built_at: 0,
        bytes: 2_684_354_560,
        files: 23_701,
        agent_version: "0.1.0".into(),
        schema: TEMPLATE_META_SCHEMA,
    }
}

// ---------------------------------------------------------------------------
// §3.2 — version comparison
// ---------------------------------------------------------------------------

#[test]
fn a_template_at_the_same_version_is_not_rebuilt() {
    let r = req();
    assert_eq!(
        decide_warmup(true, Some(&meta_at("v2", &r.registry_ref)), &r),
        WarmupDecision::SkipUpToDate
    );
}

#[test]
fn a_version_bump_or_a_ref_change_rebuilds() {
    let r = req();
    assert_eq!(
        decide_warmup(true, Some(&meta_at("v1", &r.registry_ref)), &r),
        WarmupDecision::Build
    );
    assert_eq!(
        decide_warmup(true, Some(&meta_at("v2", "ghcr.io/other@sha256:beef")), &r),
        WarmupDecision::Build
    );
    assert_eq!(decide_warmup(true, None, &r), WarmupDecision::Build);
}

#[test]
fn the_warmup_knob_disables_the_build_but_not_the_feature() {
    assert_eq!(
        decide_warmup(false, None, &req()),
        WarmupDecision::SkipDisabled
    );
}

/// The whole job short-circuits on the version compare — no staging dir, no
/// container, no gate time held.
#[test]
fn an_up_to_date_template_short_circuits_the_whole_job() {
    let r = req();
    let mut store = FakeStore::new();
    store.existing = Some(meta_at("v2", &r.registry_ref));
    let host = FakeHost::new();
    let launched = host.launched.clone();
    let log = store.log.clone();
    let clock = FakeClock::new();
    let control = WarmupControl::new();
    let activity = HostActivity::new();
    let guard = control
        .try_acquire(&activity, Duration::ZERO, clock.now())
        .unwrap();

    let job = WarmupJob {
        cfg: &test_cfg(),
        store: &store,
        host: &host,
        clock: &clock,
        app_uid_gid: None,
    };
    match job.run(&r, &guard) {
        Err(WarmupError::Skipped(reason)) => {
            assert!(reason.contains("already at version v2"), "{reason}")
        }
        other => panic!("expected a skip, got {other:?}"),
    }
    assert_eq!(launched.load(Ordering::SeqCst), 0);
    let log = log.lock().unwrap();
    assert!(log.published.is_empty());
    assert_eq!(
        log.discarded, 0,
        "nothing was staged, so nothing to discard"
    );
}

// ---------------------------------------------------------------------------
// The happy path
// ---------------------------------------------------------------------------

#[test]
fn a_warm_up_publishes_a_sanitized_verified_template() {
    let store = FakeStore::new();
    let log = store.log.clone();
    let mut host = FakeHost::new();
    host.polls_to_present = 3;
    host.populate = Some(Arc::new(steam_like_home));
    let stopped = host.stopped.clone();
    let clock = FakeClock::new();
    let control = WarmupControl::new();
    let activity = HostActivity::new();
    let guard = control
        .try_acquire(&activity, Duration::ZERO, clock.now())
        .unwrap();

    let job = WarmupJob {
        cfg: &test_cfg(),
        store: &store,
        host: &host,
        clock: &clock,
        app_uid_gid: None,
    };
    let outcome = job.run(&req(), &guard).expect("warm-up should succeed");

    // §3.3 step 8: session stopped, snapshot happened after.
    assert_eq!(stopped.load(Ordering::SeqCst), 1);
    // Strip list applied: 4 kept files (steam.sh, config.vdf, ubuntu12_32
    // blob, registry.vdf) plus the teardown marker proving snapshot-after-stop.
    assert_eq!(outcome.stats.files, 5, "{:?}", outcome.stats);
    assert_eq!(outcome.stats.symlinks, 1);

    let log = log.lock().unwrap();
    assert_eq!(log.discarded, 0);
    assert_eq!(log.published.len(), 1);
    let meta = &log.published[0];
    assert_eq!(meta.image_id, "steam");
    assert_eq!(meta.version, "v2");
    assert_eq!(meta.schema, TEMPLATE_META_SCHEMA);
    assert_eq!(meta.files, outcome.stats.files);
    assert!(meta.built_at > 0);
}

/// §3.3 step 7: the job does not snapshot until the tree has been still for the
/// quiescence window, even though the settle timer has already expired.
#[test]
fn the_job_waits_out_the_quiescence_window_after_settling() {
    let store = FakeStore::new();
    let mut host = FakeHost::new();
    host.populate = Some(Arc::new(steam_like_home));
    let clock = FakeClock::new();
    let control = WarmupControl::new();
    let activity = HostActivity::new();
    let guard = control
        .try_acquire(&activity, Duration::ZERO, clock.now())
        .unwrap();
    let started = clock.now();

    let mut cfg = test_cfg();
    cfg.settle = Duration::from_secs(5);
    cfg.quiesce_window = Duration::from_secs(10);
    let job = WarmupJob {
        cfg: &cfg,
        store: &store,
        host: &host,
        clock: &clock,
        app_uid_gid: None,
    };
    job.run(&req(), &guard).expect("warm-up should succeed");
    // 5s settle + 10s stillness on the fake clock — cannot reach the snapshot
    // any earlier.
    assert!(
        clock.now().duration_since(started) >= Duration::from_secs(15),
        "settle+quiesce were not both waited out"
    );
}

// ---------------------------------------------------------------------------
// §3.4 — a user launch always wins
// ---------------------------------------------------------------------------

#[test]
fn a_user_launch_aborts_the_warm_up_and_leaves_the_previous_template() {
    let r = req();
    let mut store = FakeStore::new();
    store.existing = Some(meta_at("v1", &r.registry_ref));
    let log = store.log.clone();
    let mut host = FakeHost::new();
    host.polls_to_present = 1_000; // long boot: abort lands mid-wait
    let stopped = host.stopped.clone();
    let clock = FakeClock::new();
    let control = WarmupControl::new();
    let activity = HostActivity::new();
    let guard = control
        .try_acquire(&activity, Duration::ZERO, clock.now())
        .unwrap();

    // The agent loop's `session_assign` handler does exactly this.
    control.abort_for_user_launch();

    let job = WarmupJob {
        cfg: &test_cfg(),
        store: &store,
        host: &host,
        clock: &clock,
        app_uid_gid: None,
    };
    assert_eq!(job.run(&r, &guard), Err(WarmupError::Aborted));
    // Stopped on the way out — a warm-up must never outlive its job and
    // overlap a user session's encoder (#489).
    assert_eq!(stopped.load(Ordering::SeqCst), 1);
    let log = log.lock().unwrap();
    assert!(
        log.published.is_empty(),
        "an aborted warm-up publishes nothing"
    );
    assert_eq!(log.discarded, 1, "the staging tree is cleaned up");
}

#[test]
fn the_whole_job_bound_expires_into_a_stopped_session() {
    let store = FakeStore::new();
    let mut host = FakeHost::new();
    host.polls_to_present = 1_000_000;
    let stopped = host.stopped.clone();
    let clock = FakeClock::new();
    let control = WarmupControl::new();
    let activity = HostActivity::new();
    let guard = control
        .try_acquire(&activity, Duration::ZERO, clock.now())
        .unwrap();

    let mut cfg = test_cfg();
    cfg.job_timeout = Some(Duration::from_secs(5));
    let job = WarmupJob {
        cfg: &cfg,
        store: &store,
        host: &host,
        clock: &clock,
        app_uid_gid: None,
    };
    assert_eq!(job.run(&req(), &guard), Err(WarmupError::TimedOut));
    assert_eq!(stopped.load(Ordering::SeqCst), 1);
}

// ---------------------------------------------------------------------------
// Failure modes — never a published template, never a user-visible failure
// ---------------------------------------------------------------------------

#[test]
fn a_verification_failure_refuses_to_publish() {
    let store = FakeStore::new();
    let log = store.log.clone();
    let mut host = FakeHost::new();
    // A home that "succeeded" with nothing in it — §6.3's sanity floor.
    host.populate = Some(Arc::new(|scratch: &Path| {
        write(scratch, "nothing/useful", "x");
    }));
    let clock = FakeClock::new();
    let control = WarmupControl::new();
    let activity = HostActivity::new();
    let guard = control
        .try_acquire(&activity, Duration::ZERO, clock.now())
        .unwrap();

    let job = WarmupJob {
        cfg: &test_cfg(),
        store: &store,
        host: &host,
        clock: &clock,
        app_uid_gid: None,
    };
    match job.run(&req(), &guard) {
        Err(WarmupError::Failed(r)) => assert!(r.contains("verification failed"), "{r}"),
        other => panic!("expected a verification failure, got {other:?}"),
    }
    let log = log.lock().unwrap();
    assert!(log.published.is_empty());
    assert_eq!(log.discarded, 1);
}

/// §6.3's tripwire: an account artifact aborts the build rather than shipping
/// into a template that every user would then be cloned from.
#[test]
fn account_state_in_the_scratch_home_aborts_the_build() {
    let store = FakeStore::new();
    let log = store.log.clone();
    let mut host = FakeHost::new();
    host.populate = Some(Arc::new(|scratch: &Path| {
        steam_like_home(scratch);
        write(scratch, ".local/share/Steam/config/loginusers.vdf", "oops");
    }));
    let clock = FakeClock::new();
    let control = WarmupControl::new();
    let activity = HostActivity::new();
    let guard = control
        .try_acquire(&activity, Duration::ZERO, clock.now())
        .unwrap();

    let job = WarmupJob {
        cfg: &test_cfg(),
        store: &store,
        host: &host,
        clock: &clock,
        app_uid_gid: None,
    };
    assert!(matches!(
        job.run(&req(), &guard),
        Err(WarmupError::Failed(_))
    ));
    assert!(log.lock().unwrap().published.is_empty());
}

#[test]
fn an_image_with_no_resolvable_container_home_is_skipped_before_staging() {
    let store = FakeStore::new();
    let log = store.log.clone();
    let mut host = FakeHost::new();
    host.container_home = None;
    let clock = FakeClock::new();
    let control = WarmupControl::new();
    let activity = HostActivity::new();
    let guard = control
        .try_acquire(&activity, Duration::ZERO, clock.now())
        .unwrap();

    let job = WarmupJob {
        cfg: &test_cfg(),
        store: &store,
        host: &host,
        clock: &clock,
        app_uid_gid: None,
    };
    match job.run(&req(), &guard) {
        Err(WarmupError::Skipped(r)) => assert!(r.contains("Config.Env HOME"), "{r}"),
        other => panic!("expected a skip, got {other:?}"),
    }
    assert_eq!(log.lock().unwrap().discarded, 0);
}

#[test]
fn a_full_disk_skips_the_build() {
    let mut store = FakeStore::new();
    store.can_build = false;
    let host = FakeHost::new();
    let launched = host.launched.clone();
    let clock = FakeClock::new();
    let control = WarmupControl::new();
    let activity = HostActivity::new();
    let guard = control
        .try_acquire(&activity, Duration::ZERO, clock.now())
        .unwrap();

    let job = WarmupJob {
        cfg: &test_cfg(),
        store: &store,
        host: &host,
        clock: &clock,
        app_uid_gid: None,
    };
    assert!(matches!(
        job.run(&req(), &guard),
        Err(WarmupError::Skipped(_))
    ));
    assert_eq!(launched.load(Ordering::SeqCst), 0);
}

#[test]
fn a_session_that_dies_before_presenting_fails_the_build_not_the_host() {
    let store = FakeStore::new();
    let log = store.log.clone();
    let mut host = FakeHost::new();
    host.ends_with = Some("app container exited with status 1".into());
    let stopped = host.stopped.clone();
    let clock = FakeClock::new();
    let control = WarmupControl::new();
    let activity = HostActivity::new();
    let guard = control
        .try_acquire(&activity, Duration::ZERO, clock.now())
        .unwrap();

    let job = WarmupJob {
        cfg: &test_cfg(),
        store: &store,
        host: &host,
        clock: &clock,
        app_uid_gid: None,
    };
    match job.run(&req(), &guard) {
        Err(WarmupError::Failed(r)) => assert!(r.contains("ended before presenting"), "{r}"),
        other => panic!("expected a failure, got {other:?}"),
    }
    assert_eq!(stopped.load(Ordering::SeqCst), 1);
    assert!(log.lock().unwrap().published.is_empty());
}

// ---------------------------------------------------------------------------
// WP5 — the job runner: the §3.4 gate, deferral mapping, and image removal.
// What they assert: the #489 preconditions, and that nothing launches beside a
// live session. A refusal reports `deferred` with a reason; retry is the
// control plane's persisted `job_runs.attempt` ladder.
// ---------------------------------------------------------------------------

use crate::jobs::{AbortFlag, JobOutcome, JobRunner};

fn runner(
    store: Option<Arc<dyn TemplateStoreApi>>,
    host: Arc<dyn WarmupHost>,
    control: Arc<WarmupControl>,
    activity: Arc<HostActivity>,
    clock: Arc<FakeClock>,
) -> WarmupJobRunner {
    WarmupJobRunner::new(test_cfg(), store, host, control, activity, None).with_clock(clock)
}

/// A host idle for longer than [`POST_SESSION_QUIET`] — the starting state for
/// every test that expects the gate to OPEN. A freshly constructed
/// `HostActivity` is NOT quiet: its last transition is "now" (the §3.4 margin
/// doing its job).
fn quiet_host() -> (Arc<HostActivity>, Arc<FakeClock>) {
    let activity = Arc::new(HostActivity::new());
    let clock = Arc::new(FakeClock::new());
    clock.sleep(POST_SESSION_QUIET * 2);
    (activity, clock)
}

/// The params blob the control plane materializes from an image reaching
/// `ready` (`Ensurer.AgentImageState` -> `Dispatcher.Enqueue`).
fn params() -> serde_json::Value {
    let r = req();
    serde_json::json!({
        "image_id": r.image_id,
        "registry_ref": r.registry_ref,
        "version": r.version,
    })
}

/// THE #489 TEST. A live session refuses the gate, and the refusal is VISIBLE:
/// a `deferred` outcome carrying the exact reason an operator reads in the
/// admin viewer. Nothing launches beside the live session.
#[test]
fn a_live_session_defers_the_warm_up_with_its_reason() {
    let store = Arc::new(FakeStore::new());
    let host = Arc::new(FakeHost::new());
    let launched = host.launched.clone();
    let control = Arc::new(WarmupControl::new());
    let activity = Arc::new(HostActivity::new());
    let clock = Arc::new(FakeClock::new());
    activity.set_live(1, clock.now());

    let r = runner(Some(store), host, control, activity, clock);
    let outcome = r.run(&params(), &AbortFlag::new());

    assert_eq!(
        outcome,
        JobOutcome::Deferred("host has 1 live session(s)".into())
    );
    assert_eq!(launched.load(Ordering::SeqCst), 0);
}

/// The post-teardown quiet margin is part of the #489 guard, not an
/// optimization: zero live sessions is not enough on its own.
#[test]
fn the_warm_up_runs_only_once_the_host_has_been_quiet() {
    let store = Arc::new(FakeStore::new());
    let mut h = FakeHost::new();
    h.populate = Some(Arc::new(steam_like_home));
    let host = Arc::new(h);
    let launched = host.launched.clone();
    let control = Arc::new(WarmupControl::new());
    let activity = Arc::new(HostActivity::new());
    let clock = Arc::new(FakeClock::new());
    activity.set_live(1, clock.now());

    let r = runner(Some(store), host, control, activity.clone(), clock.clone());
    assert!(matches!(
        r.run(&params(), &AbortFlag::new()),
        JobOutcome::Deferred(_)
    ));

    // Session ends, but the quiet margin has not elapsed yet.
    clock.sleep(Duration::from_secs(31));
    activity.set_live(0, clock.now());
    assert!(
        matches!(r.run(&params(), &AbortFlag::new()), JobOutcome::Deferred(_)),
        "the quiet margin after a session ends is part of the #489 guard"
    );
    assert_eq!(launched.load(Ordering::SeqCst), 0);

    clock.sleep(Duration::from_secs(60));
    match r.run(&params(), &AbortFlag::new()) {
        JobOutcome::Succeeded(v) => {
            assert_eq!(v["image_id"], "steam");
            assert_eq!(v["version"], "v2");
            assert!(v["files"].as_u64().unwrap() > 0);
        }
        other => panic!("expected success, got {other:?}"),
    }
    assert_eq!(launched.load(Ordering::SeqCst), 1);
}

/// A second warm-up cannot start while one holds the host-global lock — the
/// other half of the #489 gate, its own distinct deferral reason.
#[test]
fn a_warm_up_already_running_defers_the_next_one() {
    let store = Arc::new(FakeStore::new());
    let host = Arc::new(FakeHost::new());
    let control = Arc::new(WarmupControl::new());
    let (activity, clock) = quiet_host();

    let held = control
        .try_acquire(&activity, Duration::ZERO, clock.now())
        .unwrap();
    let r = runner(Some(store), host, control.clone(), activity, clock);
    assert_eq!(
        r.run(&params(), &AbortFlag::new()),
        JobOutcome::Deferred("another warm-up is already running on this host".into())
    );
    drop(held);
}

/// A user launch mid-warm-up is a DEFERRAL, not a failure: the warm-up did
/// what it is designed to do, and the persisted backoff brings it back.
#[test]
fn a_user_launch_mid_run_is_deferred_not_failed() {
    let store = Arc::new(FakeStore::new());
    let log = store.log.clone();
    let mut h = FakeHost::new();
    let control = Arc::new(WarmupControl::new());
    let aborter = control.clone();
    // Abort the moment the app presents (while the job is settling).
    h.populate = Some(Arc::new(move |_: &Path| aborter.abort_for_user_launch()));
    let host = Arc::new(h);
    let stopped = host.stopped.clone();
    let (activity, clock) = quiet_host();

    let r = runner(Some(store), host, control, activity, clock);
    assert_eq!(
        r.run(&params(), &AbortFlag::new()),
        JobOutcome::Deferred("aborted for a user session launch".into())
    );
    // The session was torn down, and no half-built template was published.
    assert_eq!(stopped.load(Ordering::SeqCst), 1);
    assert!(log.lock().unwrap().published.is_empty());
    assert_eq!(log.lock().unwrap().discarded, 1);
}

/// An up-to-date template is a `skipped` run with its reason — the whole point
/// of the framework: "nothing to do" stops looking like "never ran".
#[test]
fn an_up_to_date_template_is_skipped_with_a_reason() {
    let mut store = FakeStore::new();
    store.existing = Some(meta_at("v2", &req().registry_ref));
    let host = Arc::new(FakeHost::new());
    let launched = host.launched.clone();
    let (activity, clock) = quiet_host();
    let r = runner(
        Some(Arc::new(store)),
        host,
        Arc::new(WarmupControl::new()),
        activity,
        clock,
    );
    match r.run(&params(), &AbortFlag::new()) {
        JobOutcome::Skipped(reason) => {
            assert!(reason.contains("already at version v2"), "{reason}")
        }
        other => panic!("expected a skip, got {other:?}"),
    }
    assert_eq!(launched.load(Ordering::SeqCst), 0);
}

/// KNOB-vs-REGISTRATION PRECEDENCE: the runner is registered on EVERY host; a
/// disabled host answers `skipped` naming the knob rather than being silently
/// absent from the fleet's job list. It never takes the gate.
#[test]
fn a_host_with_the_warmup_knob_off_skips_without_taking_the_gate() {
    let host = Arc::new(FakeHost::new());
    let control = Arc::new(WarmupControl::new());
    let clock = Arc::new(FakeClock::new());
    let mut cfg = test_cfg();
    cfg.enabled = false;
    let r = WarmupJobRunner::new(
        cfg,
        Some(Arc::new(FakeStore::new())),
        host.clone(),
        control.clone(),
        Arc::new(HostActivity::new()),
        None,
    )
    .with_clock(clock);

    assert_eq!(
        r.run(&params(), &AbortFlag::new()),
        JobOutcome::Skipped(super::WARMUP_DISABLED_REASON.into())
    );
    assert!(!control.active(), "a disabled host must not take the gate");
    assert_eq!(host.launched.load(Ordering::SeqCst), 0);
}

/// THE DEFAULT IS OFF: a build takes the host's single encode slot for
/// minutes, serialized against every user launch by #489.
#[test]
fn the_warmup_build_gate_defaults_off_and_is_opt_in() {
    assert!(
        !WarmupConfig::default().enabled,
        "QUASAR_TEMPLATE_WARMUP must default OFF"
    );
}

/// A template root on a different filesystem from the home root FAILS the
/// build with a deployment-actionable reason and never takes the gate.
/// Seeding is untouched — only the build refuses.
#[test]
fn a_cross_filesystem_template_root_refuses_to_build() {
    let host = Arc::new(FakeHost::new());
    let control = Arc::new(WarmupControl::new());
    let clock = Arc::new(FakeClock::new());
    let mut store = FakeStore::new();
    store.cross_fs = true;
    let r = WarmupJobRunner::new(
        test_cfg(),
        Some(Arc::new(store)),
        host.clone(),
        control.clone(),
        Arc::new(HostActivity::new()),
        None,
    )
    .with_clock(clock);

    match r.run(&params(), &AbortFlag::new()) {
        JobOutcome::Failed(reason) => {
            assert!(reason.contains("different filesystems"), "{reason}");
            assert!(reason.contains("QUASAR_TEMPLATE_ALLOW_CROSSFS"), "{reason}");
        }
        other => panic!("expected a failure, got {other:?}"),
    }
    assert!(
        !control.active(),
        "a split namespace must not take the gate"
    );
    assert_eq!(host.launched.load(Ordering::SeqCst), 0);
}

/// A host with no resolvable template store is registered too, and says so.
#[test]
fn a_host_with_no_template_store_skips_with_that_reason() {
    let clock = Arc::new(FakeClock::new());
    let r = runner(
        None,
        Arc::new(FakeHost::new()),
        Arc::new(WarmupControl::new()),
        Arc::new(HostActivity::new()),
        clock,
    );
    assert_eq!(
        r.run(&params(), &AbortFlag::new()),
        JobOutcome::Skipped("golden-home templates are not configured on this host".into())
    );
}

/// Incomplete params are a FAILURE, not a silent no-op — the class of invisible
/// bug this framework exists to end.
#[test]
fn incomplete_params_fail_the_run() {
    let clock = Arc::new(FakeClock::new());
    let r = runner(
        Some(Arc::new(FakeStore::new())),
        Arc::new(FakeHost::new()),
        Arc::new(WarmupControl::new()),
        Arc::new(HostActivity::new()),
        clock,
    );
    match r.run(&serde_json::json!({"image_id": "steam"}), &AbortFlag::new()) {
        JobOutcome::Failed(e) => assert!(e.contains("params incomplete"), "{e}"),
        other => panic!("expected a failure, got {other:?}"),
    }
}

#[test]
fn the_runner_claims_the_frozen_job_id() {
    let clock = Arc::new(FakeClock::new());
    let r = runner(
        None,
        Arc::new(FakeHost::new()),
        Arc::new(WarmupControl::new()),
        Arc::new(HostActivity::new()),
        clock,
    );
    assert_eq!(r.job_id(), "template.warmup");
    assert_eq!(JOB_ID, "template.warmup");
}

/// §3.2 row 3 — the one image-lifecycle duty that stayed agent-side.
/// `image_ready` is deliberately inert: the control plane owns that trigger,
/// and firing here too would bypass the run window.
#[test]
fn uninstalling_an_image_removes_its_template_and_ready_is_inert() {
    use crate::images::ImageLifecycleObserver;
    let store = Arc::new(FakeStore::new());
    let log = store.log.clone();
    let reaper = TemplateReaper::new(store);

    reaper.image_ready("steam", "ghcr.io/x/steam@sha256:abc", "v7");
    assert!(
        log.lock().unwrap().removed.is_empty(),
        "image_ready must not touch the store"
    );

    reaper.image_removed("steam");
    assert_eq!(log.lock().unwrap().removed, vec!["steam".to_string()]);
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// §3.3 step 2 — capacity reservation on the EXISTING message
// ---------------------------------------------------------------------------

fn gpu(index: i32, slots: i32) -> crate::messages::GpuCapacity {
    crate::messages::GpuCapacity {
        index,
        vendor: "nvidia".into(),
        model: "RTX 5090".into(),
        vram_mb_total: 32_768,
        encode_slots_total: slots,
        render_node: None,
        device_path: None,
    }
}

#[test]
fn a_running_warm_up_reports_one_fewer_encode_slot() {
    let mut gpus = vec![gpu(0, 2), gpu(1, 3)];
    apply_encode_slot_reservation(&mut gpus, true);
    assert_eq!(gpus[0].encode_slots_total, 1);
    assert_eq!(gpus[1].encode_slots_total, 3, "only one slot is reserved");
}

#[test]
fn no_warm_up_reports_the_hardware_verbatim() {
    let mut gpus = vec![gpu(0, 2)];
    apply_encode_slot_reservation(&mut gpus, false);
    assert_eq!(gpus[0].encode_slots_total, 2);
}

#[test]
fn the_reservation_never_reports_a_negative_slot_count() {
    let mut gpus = vec![gpu(0, 0), gpu(1, 1)];
    apply_encode_slot_reservation(&mut gpus, true);
    assert_eq!(gpus[0].encode_slots_total, 0);
    assert_eq!(
        gpus[1].encode_slots_total, 0,
        "the slot comes off a GPU that has one"
    );
}

#[test]
fn the_config_defaults_match_the_design_doc() {
    let d = WarmupConfig::default();
    // Build gate: OFF by default. See `the_warmup_build_gate_defaults_off_and_is_opt_in`.
    assert!(!d.enabled);
    assert_eq!(d.settle, Duration::from_secs(60));
    assert_eq!(d.job_timeout, Some(Duration::from_secs(600)));
    assert_eq!(d.min_free_bytes, 21_474_836_480);
    assert_eq!(d.quiesce_window, Duration::from_secs(10));
}
