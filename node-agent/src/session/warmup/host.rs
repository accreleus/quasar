//! #488 §3.3 steps 4-6, 8 — the production [`WarmupHost`]: resolve the image's
//! container home, run one session-shaped warm-up through the existing
//! `SessionRunner` path, and tear it down.
//!
//! Nothing new is built here. The warm-up is an ordinary [`run_blocking`]
//! session with three deliberate differences from a user session:
//!
//! 1. **It is invisible to the control plane.** Its own event channel, not the
//!    agent loop's, so no `session_state` is emitted for an id the control
//!    plane never assigned — also what keeps it out of
//!    `SessionManager::running` (its serialization is the §3.4 gate).
//! 2. **Idle reaping is off** (`idle_timeout = ZERO`). A warm-up has no client
//!    by design; its bounds are `QUASAR_TEMPLATE_WARMUP_TIMEOUT_SECS` plus the
//!    runner's own boot watchdog.
//! 3. **`home_root` is empty**, so `session::home::provision_home_dirs` is a
//!    no-op — the scratch home is created by the job itself. This makes "a
//!    warm-up is never seeded from a template" structural: the seeding hook
//!    (WP3) hangs off a provisioning call this session never makes.
//!
//! Everything else — compositor, encoder, app container launch, the #484 `app
//! presented` gate — is the same code a user session runs: the completion
//! signal has already shipped and is already validated on hardware.

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicBool, AtomicU64};
use std::sync::Arc;

use tokio::sync::mpsc;
use tracing::{debug, info, warn};

use super::{WarmupHost, WarmupLaunch, WarmupSession, WarmupSessionState};
use crate::messages::AppExitPolicy;
use crate::session::capture::CaptureRequest;
use crate::session::container::{ContainerRuntime, ContainerSpec};
use crate::session::metrics::SessionMetrics;
use crate::session::runner::{
    run_blocking, DiagnosticEventTx, DisplayUpdateRequest, SessionEvent, SwapRequest,
};
use crate::session::settings::RuntimeSettings;
use crate::session::signaling::SignalMsg;
use crate::session::{SessionConfig, StreamParams};

/// Bound on the lifecycle-event channel. A warm-up produces a handful of events;
/// this is sized so the runner never blocks on a full channel between the job's
/// polls.
const EVENT_CAPACITY: usize = 64;

/// Docker network for a warm-up container.
///
/// **`bridge`, deliberately.** A template's value is the Steam client's 2.5 GiB
/// self-update (§2) — a network download. The hardened `none` default would
/// unpack only the baked bootstrap and snapshot a near-worthless home, which
/// §6.3's sanity floor would (correctly) refuse to publish. Real Steam tiles
/// get `bridge` via their app spec; a warm-up has none, so it is stated here.
const WARMUP_NETWORK: &str = "bridge";

/// The production [`WarmupHost`].
///
/// `settings` is snapshotted at construction so a `config_update`
/// mid-connection does not retune a warm-up already in flight — a job that
/// changed encoder halfway through would be harder to reason about than one
/// that runs to completion on its starting settings. The next `ready` picks up
/// the new settings.
pub struct AgentWarmupHost {
    runtime: ContainerRuntime,
    settings: RuntimeSettings,
}

impl AgentWarmupHost {
    pub fn new(runtime: ContainerRuntime, settings: RuntimeSettings) -> Self {
        AgentWarmupHost { runtime, settings }
    }

    /// `docker image inspect --format {{.Config.WorkingDir}}`.
    fn image_workdir(&self, image: &str) -> Option<String> {
        let out = self
            .runtime
            .run_raw(&[
                "image",
                "inspect",
                "--format",
                "{{.Config.WorkingDir}}",
                "--",
                image,
            ])
            .ok()?;
        let dir = out.trim().to_string();
        (!dir.is_empty() && dir != "/").then_some(dir)
    }
}

impl WarmupHost for AgentWarmupHost {
    /// §3.3 step 4: `Config.Env HOME`, then `Config.WorkingDir`.
    ///
    /// **No `/home/quasar` fallback.** A guessed home path is worse than no
    /// template: the scratch dir binds somewhere the app never writes,
    /// presents, snapshots an empty tree, and §6.3 refuses it — a wasted 35s
    /// job per `ready`, forever, on an unwarmable image. §3.3 states the skip
    /// explicitly ("skipped with a WARN" when the image exposes neither).
    fn container_home(&self, image: &str) -> Option<String> {
        if let Some(home) = self.runtime.image_env(image, "HOME") {
            let home = home.trim().to_string();
            if home.starts_with('/') && home != "/" {
                debug!("template: {image} container home from Config.Env HOME: {home}");
                return Some(home);
            }
            info!(
                token = "template-home-from-workingdir",
                "template: {image} declares an unusable HOME={home:?}; trying WorkingDir"
            );
        }
        match self.image_workdir(image) {
            Some(dir) if dir.starts_with('/') => {
                info!("template: {image} has no HOME; using Config.WorkingDir {dir}");
                Some(dir)
            }
            _ => {
                warn!(
                    token = "template-image-no-home",
                    "template: {image} exposes neither Config.Env HOME nor Config.WorkingDir; \
                     skipping the warm-up (this image gets no template)"
                );
                None
            }
        }
    }

    fn launch(&self, req: &WarmupLaunch) -> Result<Box<dyn WarmupSession>, String> {
        let mut env = BTreeMap::new();
        // The mount is at the image's own home path, so this is a no-op for a
        // well-formed image and a correction for one whose WorkingDir we fell
        // back to.
        env.insert("HOME".to_string(), req.container_home.clone());

        let container = ContainerSpec {
            image: req.image.clone(),
            // §1.4.4: the default `-bigpicture` CMD is what makes #484's `app
            // presented` a usable completion gate.
            args: Vec::new(),
            env,
            mounts: vec![format!(
                "{}:{}",
                req.scratch_home.display(),
                req.container_home
            )],
            gpu: true,
            no_new_privileges: true,
            // A warm-up whose app exits is a failed warm-up, not a session to
            // keep alive.
            on_app_exit: AppExitPolicy::Fail,
            network: Some(WARMUP_NETWORK.to_string()),
            // Steam-only today; no desktop-session image goes through this
            // path, so it never needs an unmasked /proc.
            systempaths_unconfined: false,
        };

        let mut cfg = SessionConfig::for_assignment_with(
            &self.settings,
            StreamParams::default(),
            Some(container),
        );
        // See the module docs for all three.
        cfg.idle_timeout = std::time::Duration::ZERO;
        cfg.home_root = String::new();

        let session_id = format!("warmup-{}", req.image_id);
        let stop = Arc::new(AtomicBool::new(false));
        let (evt_tx, evt_rx) = mpsc::channel::<(String, SessionEvent)>(EVENT_CAPACITY);
        // The diagnostics lane goes nowhere: a warm-up has no control-plane
        // session to attach a trace to. The receiver drops immediately, so
        // `try_emit` fails — the "bounded, droppable" contract that lane
        // advertises.
        let (diag_tx, _diag_rx) = mpsc::channel(1);
        let diagnostic_tx = DiagnosticEventTx::new(
            diag_tx,
            Arc::new(AtomicU64::new(0)),
            Arc::new(AtomicU64::new(0)),
        );
        let (sig_tx, sig_rx) = std::sync::mpsc::channel::<SignalMsg>();
        let (swap_tx, swap_rx) = std::sync::mpsc::channel::<SwapRequest>();
        // No control-plane session owns a warm-up, so it is never
        // display-updated; the sender drops immediately and the runner's
        // drain is a permanent no-op. Same for `session_capture`: no admin can
        // ever address a capture at it.
        let (_display_tx, display_rx) = std::sync::mpsc::channel::<DisplayUpdateRequest>();
        let (_capture_tx, capture_rx) = std::sync::mpsc::channel::<CaptureRequest>();
        let metrics = Arc::new(SessionMetrics::new(
            cfg.abr_mode.as_str(),
            cfg.stream.fps.max(1) as u32,
        ));

        let thread_stop = stop.clone();
        let thread_id = session_id.clone();
        let thread = std::thread::Builder::new()
            .name("quasar-warmup-session".to_string())
            .spawn(move || {
                // Contained the same way the agent contains a user session's
                // panic (#409): must end the warm-up, not the agent process.
                let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    run_blocking(
                        thread_id.clone(),
                        cfg,
                        evt_tx,
                        diagnostic_tx,
                        thread_stop,
                        sig_rx,
                        swap_rx,
                        display_rx,
                        capture_rx,
                        metrics,
                    );
                }));
                if outcome.is_err() {
                    warn!(
                        token = "template-warmup-runner-panicked",
                        "template: the warm-up session runner panicked ({thread_id})"
                    );
                }
            })
            .map_err(|e| format!("could not spawn the warm-up session thread: {e}"))?;

        info!(
            "template: warm-up session {session_id} launched ({} at {})",
            req.image, req.container_home
        );
        Ok(Box::new(RunnerWarmupSession {
            session_id,
            stop,
            evt_rx,
            thread: Some(thread),
            state: WarmupSessionState::Booting,
            _sig_tx: sig_tx,
            _swap_tx: swap_tx,
        }))
    }
}

/// A live warm-up session: the runner thread, its stop flag, and the event lane
/// the #484 `app presented` signal arrives on.
struct RunnerWarmupSession {
    session_id: String,
    stop: Arc<AtomicBool>,
    evt_rx: mpsc::Receiver<(String, SessionEvent)>,
    thread: Option<std::thread::JoinHandle<()>>,
    state: WarmupSessionState,
    /// Held only so the runner's `Receiver`s never see a disconnect — nothing
    /// is ever sent: no peer to signal, no app to swap to.
    _sig_tx: std::sync::mpsc::Sender<SignalMsg>,
    _swap_tx: std::sync::mpsc::Sender<SwapRequest>,
}

impl WarmupSession for RunnerWarmupSession {
    /// Drain whatever the runner emitted and latch the strongest state seen.
    /// `Ended` wins over `Presented` (a session that presented and then died
    /// has still died); once latched, never downgraded.
    fn state(&mut self) -> WarmupSessionState {
        while let Ok((_, event)) = self.evt_rx.try_recv() {
            match event {
                SessionEvent::AppPresented => {
                    if self.state == WarmupSessionState::Booting {
                        self.state = WarmupSessionState::Presented;
                    }
                }
                SessionEvent::Stopped { .. } => {
                    self.state = WarmupSessionState::Ended("session stopped".into());
                }
                SessionEvent::Failed(reason) => {
                    self.state = WarmupSessionState::Ended(reason);
                }
                SessionEvent::AppFailed {
                    reason,
                    reason_code,
                    ..
                } => {
                    self.state = WarmupSessionState::Ended(format!("{reason_code}: {reason}"));
                }
                // Everything else is progress a warm-up does not act on;
                // Signaling is dropped since there is no control plane to
                // relay an offer to.
                _ => {}
            }
        }
        self.state.clone()
    }

    /// §3.3 step 8: stop and wait for **full** teardown before anything reads
    /// the tree — for #489 (encoder must be gone before the next session) and
    /// correctness (the app flushes on exit).
    ///
    /// The join is unbounded on purpose: a runner that will not unwind is a bug
    /// worth surfacing as a stuck thread rather than snapshotting a tree still
    /// being written, and it cannot stall the agent's control-plane loop (a
    /// different thread). The gate stays held for the duration.
    fn stop_and_wait(&mut self) {
        self.stop.store(true, std::sync::atomic::Ordering::Relaxed);
        if let Some(t) = self.thread.take() {
            if t.join().is_err() {
                warn!(
                    token = "template-warmup-session-panicked",
                    "template: warm-up session {} ended in a panic", self.session_id
                );
            }
        }
        debug!("template: warm-up session {} torn down", self.session_id);
    }
}

impl Drop for RunnerWarmupSession {
    /// Belt and braces: a warm-up container must never outlive its job.
    /// `stop_and_wait` runs on every path in `WarmupJob::build`, so this
    /// normally finds nothing to do.
    fn drop(&mut self) {
        if self.thread.is_some() {
            warn!(
                token = "template-warmup-session-not-stopped",
                "template: warm-up session {} was dropped without a stop; stopping now",
                self.session_id
            );
            self.stop_and_wait();
        }
    }
}

/// Host scratch dirs are created by the job and bind-mounted at the image's own
/// home path — this is only the shape assertion for that pairing.
#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    #[test]
    fn a_warm_up_mount_binds_the_scratch_home_at_the_image_home_path() {
        let req = WarmupLaunch {
            image_id: "steam".into(),
            image: "ghcr.io/x/steam:latest".into(),
            scratch_home: PathBuf::from("/var/lib/quasar/templates/.staging/steam/scratch-home"),
            container_home: "/home/quasar".into(),
        };
        let mount = format!("{}:{}", req.scratch_home.display(), req.container_home);
        assert_eq!(
            mount,
            "/var/lib/quasar/templates/.staging/steam/scratch-home:/home/quasar"
        );
    }
}
