//! The agent's control loop: the control-plane WebSocket (register, capacity,
//! heartbeat, signaling relay) and [`SessionManager`], which owns every session's
//! handles. All session state lives in the single `connect_and_run` task — no locks;
//! runner threads only emit `SessionEvent`s into a channel this loop drains.
//!
//! This loop is outside the per-session tracing span (see `.claude/rules/agent-logging.md`),
//! so lines here carry an explicit `session_id` field.

use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use futures_util::{SinkExt, StreamExt};
use tokio::sync::mpsc;
use tokio::time::sleep;
use tokio_tungstenite::{connect_async_tls_with_config, tungstenite, tungstenite::Message};
use tracing::{debug, error, info, warn};

use crate::capacity;
use crate::config::Config;
use crate::health::HealthState;
use crate::host_probe;
use crate::images::ImageManager;
use crate::messages::{
    AgentMsg, AppSpec, Auth, CodecThroughput, ControlMsg, StreamSpec, VideoTopology,
};
use crate::release::ReleaseManager;
use crate::session::capture::{self, CaptureRequest, CaptureSlot};
use crate::session::console_hotplug::ConsoleHotplugWatcher;
use crate::session::container::{ContainerRuntime, ContainerSpec};
use crate::session::gc::{self, LiveRefs};
use crate::session::library_scan::LibraryScanClient;
use crate::session::metrics::SessionMetrics;
use crate::session::mount_policy::MountPolicy;
use crate::session::runner::{
    run_blocking, validate_display_update, DiagnosticEventTx, DisplayUpdateRequest, SessionEvent,
    SwapRequest,
};
use crate::session::signaling::SignalMsg;
use crate::session::vulkan_fault::{self, GpuGlobalFaultDetector};
use crate::session::{Codec, EncoderChoice, SessionConfig, StreamParams};
use crate::vram::{VramCache, VramTarget};

const CRITICAL_EVENT_CAPACITY: usize = 256;
const DIAGNOSTIC_EVENT_CAPACITY: usize = 128;

/// #409: how long a runner thread may stay `is_finished()` with its session still
/// `running` before the heartbeat sweep reaps it. The normal terminal path finishes
/// the thread microseconds before the loop drains its event; the grace is what keeps
/// that race from producing a spurious `failed`.
const RUNNER_REAP_GRACE: Duration = Duration::from_secs(10);

/// How long an assigned-but-never-started session may sit in `pending`. The control
/// plane's 10 s `assignAckTimeout` fails the session without dispatching a
/// `session_stop`, so nothing else ever releases the assignment.
const PENDING_ASSIGNMENT_TTL: Duration = Duration::from_secs(60);

/// The per-session runner. `Arc`-boxed so tests can inject a panicking or
/// immediately-returning runner with no pipeline, GPU or container runtime.
type RunnerFn = Arc<
    dyn Fn(
            String,
            SessionConfig,
            mpsc::Sender<(String, SessionEvent)>,
            DiagnosticEventTx,
            Arc<AtomicBool>,
            std::sync::mpsc::Receiver<SignalMsg>,
            std::sync::mpsc::Receiver<SwapRequest>,
            std::sync::mpsc::Receiver<DisplayUpdateRequest>,
            std::sync::mpsc::Receiver<CaptureRequest>,
            Arc<SessionMetrics>,
        ) + Send
        + Sync,
>;

/// The production runner: the real session pipeline.
fn default_runner() -> RunnerFn {
    Arc::new(
        |session_id,
         cfg,
         evt_tx,
         diagnostic_tx,
         stop,
         sig_rx,
         swap_rx,
         display_rx,
         capture_rx,
         metrics| {
            run_blocking(
                session_id,
                cfg,
                evt_tx,
                diagnostic_tx,
                stop,
                sig_rx,
                swap_rx,
                display_rx,
                capture_rx,
                metrics,
            );
        },
    )
}

/// Best-effort rendering of a `catch_unwind` payload. `panic!("literal")` yields
/// a `&str`; `panic!("{fmt}")` yields a `String`; anything else is opaque.
fn panic_payload_text(payload: &(dyn std::any::Any + Send)) -> String {
    if let Some(s) = payload.downcast_ref::<&'static str>() {
        (*s).to_string()
    } else if let Some(s) = payload.downcast_ref::<String>() {
        s.clone()
    } else {
        "non-string panic payload".to_string()
    }
}

/// Throttles the crash-loop a compose restart policy spins on the "can never
/// register" exit. Not the reconnect backoff — that ramps to 30 s on its own.
const ENROLLMENT_UNCONFIGURED_EXIT_DELAY: Duration = Duration::from_secs(5);

/// Run the agent forever, reconnecting with exponential backoff on failure.
pub async fn run(cfg: Config) {
    // #12: precedence decisions the transport resolver made before tracing existed.
    for w in &cfg.startup_warnings {
        warn!(token = "cp-transport-precedence", "{w}");
    }
    // #519: no persisted node_secret and no ENROLLMENT_TOKEN can never succeed, so
    // exit rather than retry forever. Checked before any other startup work.
    if let Err(msg) = enrollment_reachable(&cfg) {
        error!(token = "boot-enrollment-unconfigured", "{msg}");
        sleep(ENROLLMENT_UNCONFIGURED_EXIT_DELAY).await;
        std::process::exit(1);
    }

    if let Err(message) = crate::container_ownership::initialize(&cfg.node_secret_path) {
        error!(token = "boot-container-ownership-unavailable", "{message}");
        sleep(ENROLLMENT_UNCONFIGURED_EXIT_DELAY).await;
        std::process::exit(1);
    }

    crate::runtime::initialize_image_state(
        format!("{}.runtime-images", cfg.node_secret_path).into(),
    );

    // A prior process can leave an application holding a managed home after
    // SIGKILL. Retire it through the durable API before audio/home GC or any
    // control-plane registration. An unresolved pass enters diagnostic mode below.
    let (runtime, first_cleanup) = offload_probe(|| {
        let runtime = ContainerRuntime::from_env();
        let attempt = crate::diagnostic::startup_cleanup_configured();
        (runtime, attempt)
    })
    .await;

    let health = HealthState::new();
    // #152 — a health endpoint another process answers is worse than none. The
    // stack uses host networking, so agents on one machine share this port; the
    // loser of the bind used to carry on while its container HEALTHCHECK, and
    // any operator probing by hand, read the winner's status. Bind before
    // anything else starts, and treat failure like the other boot-fatal
    // conditions above — same throttled exit, so a restart loop is bounded.
    // Also before the homes GC below, so diagnostic mode has an endpoint to be
    // not-ready on.
    match crate::health::bind_if_enabled() {
        Ok(Some(listener)) => crate::health::spawn(listener, health.clone()),
        Ok(None) => {}
        Err((addr, e)) => {
            error!(
                token = "health-bind-failed",
                "health: failed to bind {addr}: {e} — refusing to start, because a \
                 health endpoint answered by another process is worse than none. Set \
                 QUASAR_HEALTH_ADDR to an address of this agent's own, or to an empty \
                 value to run without the endpoint."
            );
            sleep(ENROLLMENT_UNCONFIGURED_EXIT_DELAY).await;
            std::process::exit(1);
        }
    }

    // Returns only once the cleanup has succeeded; everything below is withheld until then.
    if let Some(station) = crate::diagnostic::Station::enter(&first_cleanup) {
        run_diagnostic_mode(&cfg, &health, &station).await;
    }

    // Install mode + updater presence, for the startup identity banner;
    // `connect_and_run` re-discovers before every register. Failure is
    // silent-by-design — the fields go absent and the host reads as
    // identity-unknown (agent-api.md §register).
    let runtime = {
        let (runtime, facts) = offload_probe(move || {
            let facts =
                crate::buildinfo::discover_install(&crate::buildinfo::DockerFacts::new(&runtime));
            (runtime, facts)
        })
        .await;
        crate::buildinfo::set_install_facts(facts.clone());
        crate::buildinfo::log_startup_identity(&facts);
        runtime
    };

    // #500 throwaway-home sweep. Only ever removes `agent-<8hex>-<8hex>` homes
    // (the ephemeral-username shape) that no live container mounts and that are
    // past the retention window — a real account's home is never a candidate.
    // Process-level: it must run whether or not this agent reaches the control plane.
    if crate::diagnostic::may_start(crate::diagnostic::Work::HomesGc) {
        crate::session::homes_gc::spawn_sweeper();
    }

    // Adopt an already-provisioned NVIDIA driver volume BEFORE anything can touch
    // EGL: the post-restart path (the provisioner exits so a fresh process lands
    // here) and the steady state on a provisioned host.
    let (runtime, nvidia_lib32_probed) = offload_probe(move || {
        if runtime.is_nvidia() {
            crate::nvidia_volume::adopt_current();
            crate::nvidia_volume::apply_process_env();
            crate::cuda_runtime::adopt_current();
        }

        // #375: the host's 32-bit NVIDIA driver-lib dir, resolved once per process
        // for read-only injection into NVIDIA app containers. Never fatal.
        let lib32 = resolve_nvidia_lib32(&runtime);
        (runtime, lib32)
    })
    .await;

    // QUASAR_CODEC overrides every control-plane codec assignment, so sessions.codec
    // silently diverges from the streamed codec. Banner it once at startup.
    if let Ok(v) = std::env::var("QUASAR_CODEC") {
        if !v.trim().is_empty() {
            warn!(
                token = "codec-force-override-active",
                "QUASAR_CODEC={v:?} is set — the agent-side codec force override is ACTIVE and will \
                 override EVERY control-plane codec assignment on this host (sessions.codec in the DB \
                 will NOT match the streamed codec). Unset QUASAR_CODEC on production agents."
            );
        }
    }

    // ONE ImageManager per process. Pull threads outlive a disconnect, so a
    // per-connection manager would give a reconnect a second in-flight map +
    // semaphore: duplicate pulls, concurrency cap exceeded across generations.
    // The `image_state` channel is attached/detached per connection instead.
    let image_mgr = {
        let state_path = cfg.image_state_path();
        match tokio::task::spawn_blocking(move || {
            ImageManager::new(ContainerRuntime::from_env(), state_path)
        })
        .await
        {
            Ok(mgr) => mgr,
            Err(e) => {
                error!(
                    token = "image-manager-init-panicked",
                    "image manager init panicked: {e}; agent cannot continue"
                );
                return;
            }
        }
    };

    // One ReleaseManager per process, for the same reason the ImageManager is:
    // a poller outlives a disconnect, and normally outlives the process itself.
    let release_mgr = ReleaseManager::from_env();

    // Materialise a missing NVIDIA graphics userspace into the driver volume. The
    // trigger is the readiness check set itself, so what provisions and what the
    // admin card shows can never disagree.
    if crate::diagnostic::may_start(crate::diagnostic::Work::DriverVolumeProvisioner) {
        spawn_nvidia_volume_provisioner(&runtime, &nvidia_lib32_probed);
    }

    // #545: the CUDA half. NOT chained onto the driver volume — that one returns
    // immediately on a CDI-injected host, and NVRTC is needed on those too. The two
    // share the volume and nothing else (separate lock, manifest, backoff), so
    // running them concurrently is safe.
    if crate::diagnostic::may_start(crate::diagnostic::Work::CudaRuntimeProvisioner) {
        spawn_cuda_runtime_provisioner(&runtime);
    }

    // The host-probe input "agent image". A new image is a new process, which re-runs
    // every probe anyway, so the build identity is enough and costs no engine call.
    let agent_image_identity = format!(
        "{}@{}",
        crate::buildinfo::source_commit().unwrap_or(crate::buildinfo::version()),
        crate::buildinfo::built_at().unwrap_or("unknown"),
    );

    // #128: built ONCE, outside the reconnect loop. Everything a running session
    // needs lives here, so a control-plane restart no longer takes the stream
    // down with it.
    let live_refs: LiveRefs = Arc::new(Mutex::new(HashSet::new()));
    let mut sessions = HostSessions::new(
        live_refs,
        health.clone(),
        nvidia_lib32_probed.to_string(),
        image_mgr.clone(),
        release_mgr.clone(),
    );
    match crate::home_cleanup::verified_ledger_path(&cfg.node_secret_path) {
        Some(path) => {
            let result = crate::home_cleanup::HomeCleanupLedger::open_after_startup_cleanup(path)
                .and_then(|mut ledger| {
                    ledger.recover_active(|id| {
                        crate::runtime::configured()
                            .map_err(std::io::Error::other)?
                            .retire_session_applications(id)
                            .wait()
                            .map_err(std::io::Error::other)
                    })?;
                    Ok(ledger)
                });
            match result {
                Ok(ledger) => sessions.mgr.home_cleanup = Some(ledger),
                Err(_) => {
                    error!(token = "home-cleanup-proof-unavailable",
                        "persistent home cleanup state is uncertain; refusing agent admission");
                    sleep(ENROLLMENT_UNCONFIGURED_EXIT_DELAY).await;
                    std::process::exit(1);
                }
            }
        }
        None if !matches!(crate::home_cleanup::ledger_truly_absent(&cfg.node_secret_path), Ok(true)) => {
            error!(token = "home-cleanup-existing-ledger-unverified",
                "existing home cleanup ledger lacks verified persistence; refusing agent admission");
            sleep(ENROLLMENT_UNCONFIGURED_EXIT_DELAY).await;
            std::process::exit(1);
        }
        None => warn!(
            token = "home-cleanup-ledger-unverified",
            "node identity directory is not a verified persistent mount; cleanup proof capability withheld"
        ),
    }
    let grace = session_grace();

    // Only records that already asked for terminal cleanup are eligible here.
    // This task never adopts or stops a running application; boot retirement above
    // remains the fail-closed policy for applications left by a previous agent.
    let _application_cleanup_guard = spawn_application_cleanup_recovery();

    let mut backoff = Duration::from_secs(1);
    // #199: see `EnrollmentFallback` — one token attempt per stale-secret reject.
    let mut enrollment_fallback = EnrollmentFallback::default();
    loop {
        match connect_and_run(
            &cfg,
            &health,
            &nvidia_lib32_probed,
            &agent_image_identity,
            &image_mgr,
            &release_mgr,
            &mut sessions,
            enrollment_fallback.take_for_attempt(),
        )
        .await
        {
            Ok(()) => {
                // A clean shutdown IS the end of the agent, so nothing is coming
                // back to reconcile against: stop the sessions rather than leave
                // their containers behind.
                if let Some(handle) = &sessions.mgr.probe_handle {
                    handle.disconnected();
                }
                sessions.mgr.stop_all();
                info!("agent exiting cleanly");
                return;
            }
            Err(e) => {
                if let Some(handle) = &sessions.mgr.probe_handle {
                    handle.disconnected();
                }
                health.set_connected(false);
                if e.downcast_ref::<PolicySeedReconnect>().is_some() {
                    sessions.registered_this_connection = false;
                    info!(token = "policy-seed-reconnect", "durable legacy seed applied; reconnecting once to negotiate typed ownership");
                    sleep(Duration::from_millis(250)).await;
                    continue;
                }
                // #128: hold the running sessions instead of stopping them. The
                // media path is agent-to-browser and needs nothing from the
                // control plane while it is away, and on reconnect the control
                // plane reconciles against the heartbeat rather than assuming
                // they all died. Give up only if it does not come back.
                let held = sessions.running_count();
                if held > 0 {
                    if grace.is_zero() {
                        info!(
                            token = "session-grace-disabled",
                            "connection lost with {held} running session(s); grace window is 0, stopping them now"
                        );
                        sessions.mgr.stop_all();
                    } else if sessions.grace_timer.is_none() {
                        info!(
                            token = "sessions-held-for-grace",
                            "connection lost with {held} running session(s); holding them for {grace:?} \
                             while the control plane comes back"
                        );
                        let flags = sessions.stop_flags();
                        sessions.grace_timer = Some(tokio::spawn(async move {
                            tokio::time::sleep(grace).await;
                            warn!(
                                token = "session-grace-expired",
                                "control plane did not return within the grace window; stopping {} held session(s)",
                                flags.len()
                            );
                            for f in flags {
                                f.store(true, Ordering::Relaxed);
                            }
                        }));
                    }
                }
                // #199 follow-up: a rate-limited upgrade is downstream of the
                // refused registers already logged, so it gets its own token and
                // WARN rather than reading as a fresh, unrelated ERROR.
                // ONE predicate drives both the token and the counting gate below:
                // split, the next status added to describe_upgrade_refusal would be
                // counted-suppressed while still logging as an unexplained ERROR.
                let explained_refusal = e
                    .downcast_ref::<UpgradeRefused>()
                    .is_some_and(|r| describe_upgrade_refusal(r.status).is_some());
                if explained_refusal {
                    warn!(
                        token = "cp-connect-rate-limited",
                        "agent connection failed: {e:#}"
                    );
                } else {
                    error!(
                        token = "agent-connection-failed",
                        "agent connection failed: {e:#}"
                    );
                }
                // #199: a stale-secret reject makes the next attempt present the
                // enrollment token instead. A refused upgrade carried no credential
                // at all, so it cannot (and must not) arm this.
                enrollment_fallback.observe(&e);
                // One line on the cycle that crosses the threshold: every retry
                // already logs above, so this fires only when transient becomes
                // sustained.
                if counts_as_registration_failure(explained_refusal, health.unhealthy()) {
                    let failures = health.record_registration_failure(&format!("{e:#}"));
                    if failures == crate::health::UNHEALTHY_AFTER_CONSECUTIVE_FAILURES {
                        error!(
                            token = "agent-registration-unhealthy",
                            "agent has failed to connect/register {failures} times in a row with no \
                             successful registration since; the health endpoint now reports \
                             unhealthy so `docker compose ps` surfaces this — check ENROLLMENT_TOKEN \
                             validity and control-plane reachability"
                        );
                    }
                }
                // A connection that actually registered restarts the ramp, so a
                // long-lived connection dropping does not inherit a 30 s delay
                // from some earlier outage.
                if sessions.registered_this_connection {
                    sessions.registered_this_connection = false;
                    backoff = Duration::from_secs(1);
                }
                // The tight cap exists to reach a returning control plane before the
                // grace expires. Once it HAS expired there is nothing left to save,
                // and `running` still holds the stopped sessions until the next
                // connection prunes them -- so without the is_finished() term the
                // agent would poll a dead control plane every 5 s forever.
                let grace_spent = sessions
                    .grace_timer
                    .as_ref()
                    .map(|t| t.is_finished())
                    .unwrap_or(false);
                let cap = if sessions.running_count() > 0 && !grace_spent {
                    HELD_SESSION_BACKOFF_CAP
                } else {
                    Duration::from_secs(30)
                };
                let wait = backoff.min(cap);
                info!("reconnecting in {wait:?}");
                sleep(wait).await;
                backoff = (wait * 2).min(Duration::from_secs(30));
            }
        }
    }
}

/// Grace between "provisioned" and the self-restart, so the final log lines and
/// one more capacity report reach the control plane before the process goes.
const NVIDIA_VOLUME_RESTART_GRACE: Duration = Duration::from_secs(10);

/// How long a self-restart waits for OTHER provisions to finish before giving up and
/// leaving the restart to the next agent start (#66).
///
/// Sized for the slow case this exists to protect: a 441 MB driver installer downloading
/// and extracting on a domestic link. Overshooting costs only a delayed restart of an
/// agent that is already serving; undershooting kills a live provision, which is the
/// defect itself.
pub const PROVISION_QUIESCENCE_WAIT: Duration = Duration::from_secs(30 * 60);

/// Kick off driver-volume auto-provisioning only when the readiness probe reports a
/// real NVIDIA graphics gap.
///
/// Fire-and-forget on a plain `std::thread`, never a tokio task: the whole body is
/// blocking I/O (hundreds of MB of download, a child process, thousands of file
/// copies) and parking a runtime worker on it for minutes starves the heartbeat. The
/// gap decision must stay inside the thread too — it forks the EGL self-test.
fn spawn_nvidia_volume_provisioner(runtime: &ContainerRuntime, nvidia_lib32_probed: &str) {
    if !runtime.is_nvidia() {
        return;
    }
    let nvidia_lib32_probed = nvidia_lib32_probed.to_string();
    std::thread::Builder::new()
        .name("quasar-nvvol".into())
        .spawn(move || loop {
            // ONE `ProbeEnv::live`: building it forks the EGL self-test, and the gap
            // and the card must answer about the same instant.
            let env = crate::readiness::ProbeEnv::live(true, &nvidia_lib32_probed);
            let gap = crate::readiness::nvidia_gap(&env);
            if !gap.any() {
                return;
            }
            match crate::nvidia_volume::provision_blocking(true, gap) {
                crate::nvidia_volume::Outcome::Provisioned {
                    restart_required: true,
                    ..
                } => crate::nvidia_volume::restart_for_egl(NVIDIA_VOLUME_RESTART_GRACE),
                crate::nvidia_volume::Outcome::Provisioned {
                    restart_required: false,
                    ..
                } => {
                    // 32-bit-only gap: the mount is computed per app-container
                    // launch, so the next session picks it up with no restart.
                    info!(
                        "nvidia driver volume provisioned (32-bit half only) — it takes effect on \
                         the next session launch; no agent restart needed"
                    );
                }
                crate::nvidia_volume::Outcome::Failed(_) => {
                    // The artifact layer owns download backoff and integrity refusal.
                    // Rechecking also recovers from another provisioner's stale lock.
                    std::thread::sleep(Duration::from_secs(60));
                    continue;
                }
                _ => {}
            }
            return;
        })
        .map(|_| ())
        .unwrap_or_else(|e| {
            warn!(
                token = "drvvol-provisioner-spawn-failed",
                "could not spawn the nvidia driver-volume provisioner thread: {e}"
            )
        });
}

/// #545: fetch the CUDA userspace (NVRTC) the `cuda*` GStreamer elements need, so
/// the universal agent image can serve the per-session NVENC fallback.
///
/// Fire-and-forget on a plain `std::thread` for the same reason as the driver volume:
/// blocking I/O must not park a runtime worker.
///
/// Every outcome is soft. A refusal (pre-r580 driver), a failure (no network) and an
/// opt-out all leave the host as the image finds it: no `cuda*` elements, Vulkan
/// encode untouched. Nothing here may block registration or a launch.
fn spawn_cuda_runtime_provisioner(runtime: &ContainerRuntime) {
    if !runtime.is_nvidia() {
        return;
    }
    std::thread::Builder::new()
        .name("quasar-cudart".into())
        .spawn(move || {
            let placed = matches!(
                crate::cuda_runtime::provision_blocking(true),
                crate::cuda_runtime::Outcome::Provisioned(_)
            );
            // A registry scanned before NVRTC existed will never grow `cudaconvert`;
            // an unscanned one finds it unaided, and restarting for that would delay
            // registration for nothing. See `restart_needed_after_placement`.
            let scanned = crate::session::gst_initialised();
            let present = scanned && gstreamer::ElementFactory::find("cudaconvert").is_some();
            if !crate::cuda_runtime::restart_needed_after_placement(placed, scanned, present) {
                return;
            }
            warn!(
                token = "cudart-agent-restart-scheduled",
                grace_s = NVIDIA_VOLUME_RESTART_GRACE.as_secs(),
                "CUDA userspace (NVRTC) provisioned, but this process had already scanned the \
                 GStreamer registry without it — RESTARTING the node agent so cudaconvert & co \
                 register. Plugin features are registered at scan time, so this cannot be applied \
                 in place. The container restart policy brings the agent straight back."
            );
            std::thread::sleep(NVIDIA_VOLUME_RESTART_GRACE);
            // #66: the driver-volume provisioner runs on its own thread and may still be
            // extracting a 441 MB installer. NVRTC is the smaller fetch and routinely wins
            // that race, so exiting on this thread's own schedule killed the extraction
            // mid-write and stranded its lockfile. Wait for the volume to go quiescent.
            if !crate::artifact::wait_for_quiescence(PROVISION_QUIESCENCE_WAIT) {
                warn!(
                    token = "cudart-agent-restart-deferred",
                    waited_s = PROVISION_QUIESCENCE_WAIT.as_secs(),
                    in_flight = crate::artifact::provisioning_in_flight(),
                    "another provision is still in flight — NOT restarting; cudaconvert & co \
                     will register on the next agent start instead. Killing a live provision \
                     is worse than deferring the elements."
                );
                return;
            }
            warn!(
                token = "cudart-agent-restart-now",
                "restarting node agent now"
            );
            std::process::exit(0);
        })
        .map(|_| ())
        .unwrap_or_else(|e| {
            warn!(
                token = "cudart-provisioner-spawn-failed",
                "could not spawn the CUDA-userspace provisioner thread: {e}"
            )
        });
}

// ── boot-time sanity gate (#98) ──────────────────────────────────────────────

/// Delay before a boot-fault exit. Short: the restart policy's own backoff is what paces the
/// retries, this only buys the log lines a moment to be shipped.
const BOOT_SANITY_EXIT_DELAY: Duration = Duration::from_secs(5);

/// Consecutive boot exits, in the CONTAINER's filesystem. A restart-policy restart reuses the
/// container, so the count survives exactly the retries it counts and resets on the recreate
/// that the other race needs anyway.
fn boot_exit_counter_path() -> std::path::PathBuf {
    std::env::temp_dir().join("quasar-boot-sanity-exits")
}

fn read_boot_exits(path: &std::path::Path) -> u32 {
    std::fs::read_to_string(path)
        .ok()
        .and_then(|s| s.trim().parse().ok())
        .unwrap_or(0)
}

/// Returns the new count. A write failure costs only the escalation, never the retry.
fn record_boot_exit(path: &std::path::Path) -> u32 {
    let next = read_boot_exits(path).saturating_add(1);
    let _ = std::fs::write(path, next.to_string());
    next
}

/// The gate's effects, injected so the decide→sleep→recheck→exit sequence is testable with no
/// device, no real sleep and no exit.
struct BootGateEffects<'a> {
    /// #66 barrier: provisions this process holds. Sampled before deciding AND after the
    /// delay — a provision that starts inside the delay must still cancel the exit.
    in_flight: &'a dyn Fn() -> usize,
    record_exit: &'a dyn Fn() -> u32,
    clear_exits: &'a dyn Fn(),
    sleep: &'a dyn Fn(Duration),
    exit: &'a dyn Fn(i32),
}

/// Act on the boot-time readiness verdict: exit for the fault a fresh container start fixes,
/// stay and name the fix for one it does not. The decision is
/// [`crate::readiness::boot_action`]; this is only its effects. Returns whether the exit
/// effect fired.
fn run_boot_gate(
    checks: &[crate::messages::ReadinessCheck],
    gpu_present: bool,
    container_has_render_node: bool,
    prior_exits: u32,
    fx: &BootGateEffects<'_>,
) -> bool {
    use crate::readiness::{BootAction, BootInputs, BOOT_EXIT_MAX_ATTEMPTS, SANITY_LOG_TOKEN};
    let in_flight = (fx.in_flight)();
    let action = crate::readiness::boot_action(BootInputs {
        checks,
        gpu_present,
        container_has_render_node,
        provision_in_flight: in_flight > 0,
        prior_exits,
    });
    let fault = match action {
        BootAction::Continue => {
            // Streak over: a later fault gets a full retry budget.
            (fx.clear_exits)();
            return false;
        }
        BootAction::Stay(f) => {
            log_boot_stay(&f, prior_exits, in_flight);
            return false;
        }
        BootAction::ExitForRetry(f) => f,
    };
    let attempt = (fx.record_exit)();
    error!(
        token = "boot-render-node-missing",
        check = %fault.check,
        attempt,
        max_attempts = BOOT_EXIT_MAX_ATTEMPTS,
        exit_in_s = BOOT_SANITY_EXIT_DELAY.as_secs(),
        "{SANITY_LOG_TOKEN} boot: {} — waiting for a /dev/dri/renderD* node to be visible \
         INSIDE this container. A device list is fixed at container creation, so exiting in \
         {}s and letting the restart policy start a fresh container is what picks the node up. \
         Attempt {attempt} of {}. RUN (only if this repeats): {}",
        fault.summary,
        BOOT_SANITY_EXIT_DELAY.as_secs(),
        BOOT_EXIT_MAX_ATTEMPTS,
        fault.remediation
    );
    (fx.sleep)(BOOT_SANITY_EXIT_DELAY);
    let in_flight = (fx.in_flight)();
    if in_flight > 0 {
        warn!(
            token = "boot-render-node-exit-deferred",
            in_flight,
            "a provision started while the boot exit was pending — staying up; the retry \
             happens on the next agent start"
        );
        return false;
    }
    (fx.exit)(1);
    true
}

/// One line per Stay, keyed on the fault's token. The tokens are repeated as literals because
/// the log-convention test requires a literal first field.
fn log_boot_stay(f: &crate::readiness::BootFault, prior_exits: u32, in_flight: usize) {
    use crate::readiness::{
        BOOT_DRI_MODES_TOKEN, BOOT_EXIT_MAX_ATTEMPTS, BOOT_HOST_RENDER_NODE_TOKEN,
        BOOT_RENDER_NODE_DEFERRED_TOKEN, BOOT_RENDER_NODE_UNOPENABLE_TOKEN, SANITY_LOG_TOKEN,
    };
    match f.token {
        BOOT_DRI_MODES_TOKEN => error!(
            token = "boot-dri-modes-stale-cdi",
            check = %f.check,
            "{SANITY_LOG_TOKEN} boot: {} — NOT restarting for this: CDI device edits are \
             applied when a container is CREATED, so every restart reproduces the same modes \
             from the same stale spec and nothing inside this container can re-read them. \
             Regenerate the spec on the HOST, then recreate the containers. RUN: {}",
            f.summary,
            f.remediation
        ),
        BOOT_HOST_RENDER_NODE_TOKEN => error!(
            token = "boot-host-render-node-missing",
            check = %f.check,
            "{SANITY_LOG_TOKEN} boot: {} — NOT restarting for this: the node has to be created \
             by the HOST kernel first, which no container restart can do. RUN: {}",
            f.summary,
            f.remediation
        ),
        BOOT_RENDER_NODE_UNOPENABLE_TOKEN => error!(
            token = "boot-render-node-unopenable",
            check = %f.check,
            "{SANITY_LOG_TOKEN} boot: {} — the node IS in this container and the agent cannot \
             open it, so this is a mode/group/device-cgroup fault rather than the boot race, \
             and a fresh container re-creates the same node with the same permissions. NOT \
             restarting. RUN: {}",
            f.summary,
            f.remediation
        ),
        BOOT_RENDER_NODE_DEFERRED_TOKEN => warn!(
            token = "boot-render-node-retry-deferred",
            check = %f.check,
            in_flight,
            "{SANITY_LOG_TOKEN} boot: {} — a restart would fix it, but {in_flight} provision(s) \
             are still writing a shared volume and killing one mid-write is worse than waiting: \
             the retry happens on the next agent start instead",
            f.summary
        ),
        // The only remaining Stay token: the retry budget is spent.
        _ => error!(
            token = "boot-render-node-retries-spent",
            check = %f.check,
            prior_exits,
            "{SANITY_LOG_TOKEN} boot: {} — {prior_exits} restarts have already failed to bring \
             the device in, so the pass-through is missing rather than late (budget: {}). NOT \
             exiting again; staying up so the readiness card shows this. RUN: {}",
            f.summary,
            BOOT_EXIT_MAX_ATTEMPTS,
            f.remediation
        ),
    }
}

/// Wire the gate to the real world. First connection of the process only: a later reconnect is
/// not a boot, and by then live sessions exist that an exit would kill.
async fn boot_sanity_gate(readiness: &[crate::messages::ReadinessCheck], gpu_present: bool) {
    static EVALUATED: AtomicBool = AtomicBool::new(false);
    if EVALUATED.swap(true, Ordering::SeqCst) {
        return;
    }
    let checks = readiness.to_vec();
    let prior_exits = read_boot_exits(&boot_exit_counter_path());
    // Blocking pool: the exit path sleeps, and parking a runtime worker for that would stall
    // the heartbeat of a process that may yet be told to stay.
    offload_probe(move || {
        let counter = boot_exit_counter_path();
        let has_node = crate::readiness::container_has_render_node(std::path::Path::new("/"));
        let fx = BootGateEffects {
            in_flight: &crate::artifact::provisioning_in_flight,
            record_exit: &|| record_boot_exit(&counter),
            clear_exits: &|| {
                let _ = std::fs::remove_file(&counter);
            },
            sleep: &std::thread::sleep,
            exit: &|code: i32| {
                std::process::exit(code);
            },
        };
        run_boot_gate(&checks, gpu_present, has_node, prior_exits, &fx);
    })
    .await;
}

/// #375: resolve the 32-bit NVIDIA driver-lib directory to inject into NVIDIA app
/// containers. Returns the PROBED value only — empty when nothing was found or when
/// `QUASAR_NV_LIB32_PATH` is set, since the override already rides the
/// `RuntimeSettings` env baseline. Non-NVIDIA hosts skip the probe.
fn resolve_nvidia_lib32(runtime: &ContainerRuntime) -> String {
    if !runtime.is_nvidia() {
        return String::new();
    }
    let override_path = std::env::var("QUASAR_NV_LIB32_PATH").unwrap_or_default();
    if !override_path.is_empty() {
        info!("nvidia lib32 path: {override_path} (override)");
        // Carried by the RuntimeSettings env baseline; nothing to seed.
        return String::new();
    }
    match runtime.probe_nvidia_lib32_path() {
        Some(p) => {
            info!("nvidia lib32 path: {p} (probed)");
            p
        }
        None => {
            // Before declaring 32-bit GL unavailable, fall back to the
            // Quasar-provisioned driver volume — a plain host path through the same
            // mount mechanism, so nothing downstream knows the difference.
            if let Some(p) =
                crate::nvidia_volume::lib32_host_path(crate::nvidia_volume::current().as_ref())
            {
                info!("nvidia lib32 path: {p} (Quasar-provisioned driver volume)");
                return p;
            }
            info!(
                "nvidia lib32 path: none detected — native 32-bit GL unavailable in app containers"
            );
            String::new()
        }
    }
}

/// Seed the startup-probed 32-bit NVIDIA lib dir into a freshly (re)derived
/// `RuntimeSettings` when the configured value is empty. An explicit override always
/// wins and is left untouched, so `effective_map` still reports it.
fn seed_nvidia_lib32(settings: &mut crate::session::settings::RuntimeSettings, probed: &str) {
    if settings.nvidia_lib32_path.is_empty() && !probed.is_empty() {
        settings.nvidia_lib32_path = probed.to_string();
    }
}

/// What the agent advertises about its encode capability: the wire codec set
/// (`capacity.codecs`) and the per-codec throughput hint (`capacity.codec_throughput`,
/// #506).
///
/// ONE value, not two fields: the throughput map is keyed on the elements the codec
/// probe resolved to, so a `config_update` that flips the effective encoder
/// invalidates both together. Two fields would let a re-probe refresh the codec set
/// and leave a stale hint — and a stale hint gates real launches.
#[derive(Debug, Clone, Default, PartialEq)]
pub(crate) struct HostCodecReport {
    /// `["h264", ...]` — the codecs the active encoder path can produce.
    codecs: Vec<String>,
    /// Wire codec → sustained throughput, for the SUBSET of `codecs` whose resolved
    /// element has a measured rate. A codec absent from here is unknown, and the
    /// control plane gates nothing on unknown.
    throughput: BTreeMap<String, CodecThroughput>,
}

/// The `capacity.codec_throughput` field for a (possibly failed) probe. A SUCCESSFUL
/// probe that measured nothing must report `{}`, not `None`: the empty map clears the
/// stored hints, which is what a host has to say once a `config_update` moved it off a
/// measured encoder path. `None` would leave the old path's hints gating launches.
pub(crate) fn advertised_codec_throughput(
    report: &Option<HostCodecReport>,
) -> Option<BTreeMap<String, CodecThroughput>> {
    report.as_ref().map(|r| r.throughput.clone())
}

/// How long register preparation may take before the agent says so. Register
/// preparation (the image reconcile and the install probe, both against the
/// container runtime) runs before the socket is dialled, so it can no longer eat the
/// control plane's handshake window — but the window is 15 s (agentws
/// `handshakeTimeout`), and a runtime slow enough to approach it deserves a log line
/// naming it rather than a slow, mysterious reconnect (#191).
const REGISTER_PREP_BUDGET: Duration = Duration::from_secs(10);

fn register_prep_over_budget(elapsed: Duration) -> bool {
    elapsed > REGISTER_PREP_BUDGET
}

/// #531: run one synchronous host probe on the blocking pool, never on the runtime
/// worker polling the agent's control future.
///
/// Everything this wraps forks subprocesses (`docker`, `nvidia-smi`, the EGL
/// self-test, `firewall-cmd`) and reads tens of sysfs files. Inline it produced a
/// single 1311 ms poll of the future that also owns heartbeats, the signalling relay
/// and `session_stop` — against a 20 s stale-host deadline. Ordering is unchanged
/// (the result is awaited immediately) and a probe panic still reaches the caller.
async fn offload_probe<T, F>(f: F) -> T
where
    F: FnOnce() -> T + Send + 'static,
    T: Send + 'static,
{
    match tokio::task::spawn_blocking(f).await {
        Ok(v) => v,
        Err(e) if e.is_panic() => std::panic::resume_unwind(e.into_panic()),
        // The blocking pool only cancels on runtime shutdown, at which point
        // there is no meaningful capacity/readiness answer to return.
        Err(e) => panic!("host probe task did not complete: {e}"),
    }
}

/// What the readiness refresh task reports to the control loop.
#[derive(Debug, PartialEq)]
enum ReadinessRefresh {
    /// The probe ended. `Err` is a panic.
    Done(Result<Vec<crate::messages::ReadinessCheck>, String>),
    /// The probe is past its deadline and still running. `Done` follows when it ends,
    /// and the control loop must not start another refresh before then: a blocking
    /// probe cannot be cancelled, so a second one would stack behind a hung first.
    Overdue,
}

const READINESS_REFRESH_DEADLINE: Duration = Duration::from_secs(60);

/// How often the connected agent re-probes host readiness. One capacity report leaves on
/// each refresh, so this is also the report cadence — and the reason no engine call in a
/// refresh may cost more than a small fraction of the control plane's staleness window
/// (#274): a hung engine's failing check has to catch the very next report.
pub(crate) const READINESS_REFRESH_INTERVAL: Duration = Duration::from_secs(15);

async fn run_readiness_refresh<F>(
    probe: F,
    deadline: Duration,
    sender: mpsc::Sender<ReadinessRefresh>,
) where
    F: FnOnce() -> Vec<crate::messages::ReadinessCheck> + Send + 'static,
{
    let mut probe = tokio::task::spawn_blocking(probe);
    let result = match tokio::time::timeout(deadline, &mut probe).await {
        Ok(result) => result,
        Err(_) => {
            let _ = sender.send(ReadinessRefresh::Overdue).await;
            probe.await
        }
    };
    let _ = sender
        .send(ReadinessRefresh::Done(result.map_err(|e| e.to_string())))
        .await;
}

/// The blocking half of every capacity re-detect: `capacity::detect()` plus an
/// unconditional warm of the memoized `nvidia-smi` row table.
///
/// The warm must happen here: `bind_assignment` maps a render node to a CUDA ordinal
/// from the synchronous `handle_control`, where the `OnceLock`ed `nvidia-smi` fork
/// cannot be awaited. Paying it before any assignment arrives leaves that a cache read.
fn detect_capacity_blocking() -> capacity::SystemCapacity {
    let cap = capacity::detect();
    if cap.gpus.iter().any(|g| g.vendor == "nvidia") {
        capacity::prewarm_nvidia_smi_rows();
    }
    cap
}

/// Is a degraded vulkan codec plan the expected first-boot shape (driver volume still
/// provisioning, so the Vulkan ICD is invisible to the registry scan) or an image
/// defect? Pure so it is testable without the module's process-global state;
/// `volume_current` is true once `nvidia_volume` has adopted a manifest.
fn vulkan_plan_degradation_is_pending_driver_volume(
    volume_current: bool,
    status: &crate::nvidia_volume::Status,
) -> bool {
    if volume_current {
        // Driver userspace is present, so a missing vulkan element is the image's
        // fault, not a timing artifact.
        return false;
    }
    // `Failed` means provisioning already gave up: no restart is coming to fix this,
    // so it earns the operator's attention rather than the quiet first-boot path.
    !matches!(status, crate::nvidia_volume::Status::Failed(_))
}

/// Probe the host's codec set and per-codec throughput hint. See [`HostCodecReport`]
/// for why the two travel together. `None` ⇒ gst init failed, and the host is then
/// advertised as `["h264"]`. The effective encoder is not restart-class —
/// a `config_update` can flip it live — so the connect loop re-probes on that flip,
/// keeping `hosts.codecs` equal to what sessions actually build.
fn probe_host_codecs(
    settings: &crate::session::settings::RuntimeSettings,
) -> Option<HostCodecReport> {
    let probe_cfg = SessionConfig::for_assignment_with(settings, StreamParams::default(), None);
    if let Err(e) = crate::session::ensure_gst_init(&probe_cfg) {
        warn!(
            token = "codec-probe-gst-init-failed",
            "codec probe: gstreamer init failed ({e:#}); host reported as h264-only"
        );
        return None;
    }
    // One of the `EncoderKnobs` ambient edges (see its doc): the knobs are read fresh
    // here and threaded as data from this point on.
    let knobs = crate::session::pipeline::EncoderKnobs::from_env();
    let support =
        crate::session::pipeline::probe_codec_support(settings.encoder, knobs, "software");
    let codecs = support.codec_strings();
    let throughput: BTreeMap<String, CodecThroughput> = support
        .pixel_rates_mpix_s()
        .into_iter()
        .map(|(codec, rate)| {
            (
                codec,
                CodecThroughput {
                    max_pixel_rate_mpix_s: rate,
                },
            )
        })
        .collect();
    if settings.encoder == crate::session::EncoderChoice::Vulkan {
        // WARN, not INFO, when a codec whose knob is ENABLED lost its vulkan element:
        // a mis-built image has to be loud at startup.
        let plan = crate::session::pipeline::describe_codec_plan(knobs);
        if plan.degraded {
            if vulkan_plan_degradation_is_pending_driver_volume(
                crate::nvidia_volume::current().is_some(),
                &crate::nvidia_volume::status(),
            ) {
                // Expected on a virgin NVIDIA deploy: the provisioner restarts the
                // agent when it finishes and the second boot re-probes against a
                // healthy ICD. A distinct token keeps dashboards keyed on
                // `vulkan-codec-plan-degraded` free of this.
                info!(
                    token = "vulkan-codec-plan-pending-driver-volume",
                    "vulkan codec plan: {} — expected on first boot: the NVIDIA driver volume is \
                     still provisioning (or has not started); the agent will self-restart once it \
                     completes and re-probe the codec plan",
                    plan.line
                );
            } else {
                warn!(
                    token = "vulkan-codec-plan-degraded",
                    "vulkan codec plan: {} — at least one ENABLED codec is not running on the vulkan \
                     encoder because its element is not registered on this image; check the image \
                     contract (deploy/image-contract.json)",
                    plan.line
                );
            }
        } else {
            info!("vulkan codec plan: {}", plan.line);
        }
    }
    info!(
        "codec support probed for {:?} encoder: {codecs:?}; throughput hints (Mpix/s): {:?}",
        settings.encoder,
        throughput
            .iter()
            .map(|(c, t)| (c.as_str(), t.max_pixel_rate_mpix_s))
            .collect::<Vec<_>>()
    );
    Some(HostCodecReport { codecs, throughput })
}

/// The render node whose registry plan (#301 layer 1) speaks for this GPU: the
/// in-container `renderD*` form when reported (what makes a VA candidate
/// device-prefixed), else the by-path identity, else `"software"`.
fn gpu_render_node_for_plan(gpu: &crate::messages::GpuCapacity) -> &str {
    gpu.device_path
        .as_deref()
        .or(gpu.render_node.as_deref())
        .unwrap_or("software")
}

/// #301 layer 1 in production: what the registry builds on this render node. Needs
/// `gst::init`, so it is only called once the host-level probe returned a report. A
/// non-default VA GPU's plan may name the generic `va<codec>enc`, because a session on
/// that GPU tries the same list; its codec probe, not the plan, is what admits a codec.
fn registry_codec_plan(encoder: EncoderChoice, render_node: &str) -> BTreeSet<Codec> {
    let knobs = crate::session::pipeline::EncoderKnobs::from_env();
    crate::session::pipeline::probe_codec_support(encoder, knobs, render_node)
        .codecs
        .into_iter()
        .collect()
}

/// #301 layer 2 in production: the driver-compatibility exclusion for this GPU.
fn gpu_excluded_codecs(gpu: &crate::messages::GpuCapacity) -> BTreeSet<Codec> {
    crate::encoder_compatibility::excluded_codecs(
        std::path::Path::new("/"),
        gpu_render_node_for_plan(gpu),
    )
}

/// The registry and sysfs reads behind layers 1 and 2, as data so tests inject them.
#[derive(Clone, Copy)]
struct CodecLayers {
    plan: fn(EncoderChoice, &str) -> BTreeSet<Codec>,
    excluded: fn(&crate::messages::GpuCapacity) -> BTreeSet<Codec>,
}

impl Default for CodecLayers {
    fn default() -> Self {
        CodecLayers {
            plan: registry_codec_plan,
            excluded: gpu_excluded_codecs,
        }
    }
}

/// The current stack, as the codec advertisement and the probe scheduler both read it:
/// one source for the probe inputs and for the stamps a codec pass is checked against.
struct CodecStack<'a> {
    agent_image: &'a str,
    gpus: &'a [crate::messages::GpuCapacity],
    settings: &'a crate::session::settings::RuntimeSettings,
    /// `gst::init` succeeded (the host-level probe returned a report): without it there
    /// is no registry to plan from, and every GPU is H.264-only.
    registry: bool,
    layers: CodecLayers,
}

impl CodecStack<'_> {
    fn plan(&self, gpu: &crate::messages::GpuCapacity) -> BTreeSet<Codec> {
        if !self.registry {
            return BTreeSet::new();
        }
        (self.layers.plan)(self.settings.encoder, gpu_render_node_for_plan(gpu))
    }

    /// What decides whether a host probe's earlier result still applies.
    fn probe_inputs(&self) -> crate::host_probe::decision::ProbeInputs {
        let mut inputs = self.identity();
        // Codec-probe targets: each GPU's own plan minus its exclusion, never gated on a
        // verdict (the verdict is what a codec probe produces).
        inputs.codecs = self
            .gpus
            .iter()
            .filter_map(|g| {
                let targets =
                    crate::gpu_codecs::probeable_codecs(&self.plan(g), &(self.layers.excluded)(g));
                (!targets.is_empty()).then_some((g.index, targets))
            })
            .collect();
        inputs
    }

    /// [`Self::probe_inputs`] without the codec plan: all an evidence stamp reads.
    fn identity(&self) -> crate::host_probe::decision::ProbeInputs {
        let mut driver_parts: Vec<String> = self
            .gpus
            .iter()
            .filter_map(|g| g.driver_identity.clone())
            .collect();
        if let Some(volume) = crate::nvidia_volume::current() {
            driver_parts.push(format!(
                "{}:{}",
                volume.name.as_deref().unwrap_or(""),
                volume.manifest.sha256
            ));
        }
        let gpus = self
            .gpus
            .iter()
            .map(|g| {
                let identity = g
                    .render_node
                    .clone()
                    .or_else(|| g.device_path.clone())
                    .unwrap_or_else(|| format!("{} {}", g.vendor, g.model));
                (g.index, identity)
            })
            .collect();
        crate::host_probe::decision::ProbeInputs {
            agent_image: self.agent_image.to_string(),
            driver: driver_parts.join(","),
            gpus,
            settings: probe_relevant_settings(&self.settings.effective_map()),
            codecs: BTreeMap::new(),
        }
    }
}

/// Per-GPU codec sets under the current stack, indexed like `stack.gpus` — the ONE
/// computation both `capacity.gpus[].codecs` (#302, `apply_gpu_codecs`) and the
/// host-level union `capacity.codecs` (#301, `host_codecs_from_sets`) derive from.
/// Never recompute the rule a second time from the same stack.
fn gpu_codec_sets(
    stack: &CodecStack<'_>,
    report: &crate::readiness::report::ReadinessReport,
    evidence: &crate::host_probe::outcome::CodecEvidence,
) -> Vec<(i32, BTreeSet<Codec>)> {
    let identity = stack.identity();
    stack
        .gpus
        .iter()
        .map(|gpu| {
            let current = identity.evidence_stamp(gpu.index);
            let set = crate::gpu_codecs::gpu_codec_set(
                &stack.plan(gpu),
                &(stack.layers.excluded)(gpu),
                |codec| {
                    crate::host_probe::ProbeCodec::above_floor(codec).is_some_and(|probe| {
                        evidence.proven(report, gpu.index, probe, current.as_ref())
                    })
                },
                gpu.encode_slots_total > 0,
            );
            (gpu.index, set)
        })
        .collect()
}

/// A `session_assign` the codec belt refuses: the log line and the ack error, decided
/// without I/O so tests assert on it rather than capture `tracing` events, whose
/// process-global interest cache races parallel tests (#313).
#[derive(Debug, Clone, PartialEq, Eq)]
struct AssignCodecRefusal {
    log: String,
    ack_error: String,
}

/// The #302 belt's decision: refuse `codec` on `gpu_index` unless that GPU's current
/// set (from [`gpu_codec_sets`]) carries it. H.264 is exempt — it is the floor of
/// every usable GPU's set by construction (`gpu_codecs::gpu_codec_set`) and never
/// fails to resolve. A GPU absent from `sets` has an empty set.
fn assign_codec_refusal(
    session_id: &str,
    gpu_index: i32,
    codec: Codec,
    sets: &[(i32, BTreeSet<Codec>)],
) -> Option<AssignCodecRefusal> {
    if codec == Codec::H264 {
        return None;
    }
    let empty = BTreeSet::new();
    let gpu_codecs = sets
        .iter()
        .find(|(index, _)| *index == gpu_index)
        .map_or(&empty, |(_, set)| set);
    if gpu_codecs.contains(&codec) {
        return None;
    }
    Some(AssignCodecRefusal {
        log: format!(
            "session {session_id} assignment rejected: gpu={gpu_index} codec={} not in \
             this GPU's current codec set {gpu_codecs:?}",
            codec.as_str()
        ),
        ack_error: format!(
            "gpu {gpu_index} cannot encode {}: not in its current codec set",
            codec.as_str()
        ),
    })
}

/// The host union (`capacity.codecs`, #301, agent-api.md amendment 12) as wire strings,
/// from already-computed per-GPU sets: H.264 plus every codec any usable GPU's set
/// carries. Always non-empty.
fn host_codecs_from_sets(sets: &[(i32, BTreeSet<Codec>)]) -> Vec<String> {
    crate::gpu_codecs::host_codec_set(sets.iter().map(|(_, s)| s))
        .into_iter()
        .map(|c| c.as_str().to_string())
        .collect()
}

/// `capacity.gpus[].codecs` (#302, agent-api.md amendment 12): stamps each GPU's own
/// wire codec set from the SAME per-GPU sets the host union is derived from — never a
/// second, possibly-diverging pass. A zero-slot (pinned-out) GPU's set is empty
/// (`gpu_codec_set`'s unusable case) and is sent as `[]`, not omitted: the control
/// plane stores an explicit `[]` as-is (never inherited), so this is the only way to
/// tell an operator "this GPU encodes nothing" apart from "this GPU never reported,
/// go by the host set". Omission is left to the case this stack never actually
/// produces — a GPU present in inventory but absent from `sets` — rather than
/// collapsed into the zero-slot case.
fn apply_gpu_codecs(gpus: &mut [crate::messages::GpuCapacity], sets: &[(i32, BTreeSet<Codec>)]) {
    for gpu in gpus.iter_mut() {
        gpu.codecs = sets
            .iter()
            .find(|(index, _)| *index == gpu.index)
            .map(|(_, set)| set.iter().map(|c| c.as_str().to_string()).collect());
    }
}

/// Poll a `select!` arm's receiver without ever resolving `Ready(None)` twice (#530).
/// An `mpsc::Receiver` whose senders have all dropped resolves `recv()` to
/// `Ready(None)` immediately and forever, so a bare arm wins every poll and spins the
/// loop at 100% of a core. Every caller MUST set its `Option` to `None` on a `None`,
/// which disables the arm for good.
async fn recv_or_disabled<T>(rx: &mut Option<mpsc::Receiver<T>>) -> Option<T> {
    match rx {
        Some(r) => r.recv().await,
        None => std::future::pending().await,
    }
}

/// Unbounded-channel counterpart of [`recv_or_disabled`] — `gpu_fault_rx` is
/// the one arm here backed by an `UnboundedReceiver`.
async fn recv_or_disabled_unbounded<T>(rx: &mut Option<mpsc::UnboundedReceiver<T>>) -> Option<T> {
    match rx {
        Some(r) => r.recv().await,
        None => std::future::pending().await,
    }
}

/// The two lines that say what this agent is about to connect to and how. Emitted
/// before the register preparation, so a slow prep is visible as a gap after them.
fn log_connect_intent(cfg: &Config) {
    info!(policy = ?cfg.transport, "connecting to {}", cfg.ws_url());
    if cfg.webpki_from_blob {
        // `qenr1..` (a mispaste that dropped the fingerprint) and a real CA deployment
        // produce the same policy; only this line tells them apart in a log.
        info!(
            token = "cp-tls-webpki-from-blob",
            "the enrollment string carried an empty fingerprint segment: verifying the control \
             plane against the WebPKI roots, not a pin"
        );
    }
}

type CpSink = futures_util::stream::SplitSink<
    tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>,
    Message,
>;
type CpStream = futures_util::stream::SplitStream<
    tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>,
>;

#[derive(Debug)]
struct PolicySeedReconnect;

impl std::fmt::Display for PolicySeedReconnect {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("RH05 seed reconnect")
    }
}

impl std::error::Error for PolicySeedReconnect {}

/// Open the control-plane socket and split it.
///
/// #12: the connector is chosen by policy, never by tokio-tungstenite's default — a
/// wss:// URL must not silently validate against the OS/bundled roots when a pin was
/// configured, and a ws:// URL is explicitly Plain.
/// A refused upgrade never reaches `register`, so its status is all the agent gets
/// to explain the failure with (#199 follow-up); `upgrade_error` is where that
/// explanation is attached.
async fn dial(cfg: &Config) -> anyhow::Result<(CpSink, CpStream)> {
    let (ws_stream, _) = connect_async_tls_with_config(
        &cfg.ws_url(),
        None,
        false,
        Some(crate::cp_tls::ws_connector(&cfg.transport)),
    )
    .await
    .map_err(upgrade_error)?;
    Ok(ws_stream.split())
}

/// The `register` this connection presents. `images` differs by caller: the normal path
/// reconciles against the daemon first, diagnostic mode reads the state file.
fn register_message(
    cfg: &Config,
    prefer_enrollment_token: bool,
    images: Vec<crate::messages::RegisterImageEntry>,
    install: &crate::buildinfo::InstallFacts,
    home_cleanup_capable: bool,
) -> anyhow::Result<AgentMsg> {
    Ok(AgentMsg::Register {
        source_policy_versions: Some(serde_json::json!({"steam_preparation": 1})),
        config_policy_versions: Some(
            serde_json::json!({"typed_settings":2,"execution_journal":1,"deployment_baseline":1}),
        ),
        config_policy_groups: Some(crate::policy::PolicyAgent::advertised_groups(
            &std::path::PathBuf::from(format!("{}.policy.json", cfg.node_secret_path)),
        )),
        terminal_home_cleanup_v1: home_cleanup_capable.then_some(true),
        node_name: cfg.node_name.clone(),
        agent_version: crate::buildinfo::version().to_string(),
        auth: choose_auth(cfg, prefer_enrollment_token)?,
        images,
        source_commit: crate::buildinfo::source_commit().map(str::to_string),
        built_at: crate::buildinfo::built_at().map(str::to_string),
        install_mode: install.install_mode.map(|m| m.as_str().to_string()),
        updater_present: install.updater_present,
    })
}

/// `register` + the `registered` reply, with the identity persistence a successful
/// handshake owes. Shared by the normal connect loop and the diagnostic one; everything
/// after it differs between them.
async fn register_and_await_registered<S, R>(
    cfg: &Config,
    tx: &mut S,
    rx: &mut R,
    register_msg: AgentMsg,
) -> anyhow::Result<(String, u64, Option<(String, String)>, Option<Vec<String>>)>
where
    S: SinkExt<Message, Error = tungstenite::Error> + Unpin,
    R: StreamExt<Item = Result<Message, tungstenite::Error>> + Unpin,
{
    // Which credential this attempt carries decides what a reject means (#199).
    let presented_saved_secret = matches!(
        &register_msg,
        AgentMsg::Register {
            auth: Auth::Reconnect { .. },
            ..
        }
    );
    send(tx, &register_msg).await?;
    info!("sent register (node_name={})", cfg.node_name);

    let raw = recv(rx).await?;
    let ctrl_msg: ControlMsg = serde_json::from_str(&raw)?;
    match ctrl_msg {
        ControlMsg::Registered {
            host_id,
            node_secret,
            heartbeat_interval_ms,
            boot_incarnation,
            connection_incarnation,
            config_policy_groups,
        } => {
            // A returned node_secret IS the enrollment signal: reconnect never mints one.
            let enrolled = node_secret.is_some();
            if let Some(secret) = node_secret {
                persist_node_secret(&cfg.node_secret_path, &secret)?;
                info!(
                    "enrolled as host {host_id}; node_secret saved to {}",
                    cfg.node_secret_path
                );
            } else {
                info!("reconnected as host {host_id}");
            }
            // #12: the pin that just verified this connection outlives the enrollment
            // string, so the operator can delete QUASAR_ENROLLMENT from the environment.
            persist_pin_if_new(cfg, enrolled);
            Ok((
                host_id,
                heartbeat_interval_ms,
                boot_incarnation.zip(connection_incarnation),
                config_policy_groups,
            ))
        }
        ControlMsg::Error { code, message } => Err(register_reject_error(
            cfg,
            &code,
            &message,
            presented_saved_secret,
        )),
        _ => anyhow::bail!("unexpected message type before registered"),
    }
}

/// How often a diagnostic connection re-detects capacity and re-probes readiness. Slower
/// than the normal path's event-driven re-sends: nothing here can change but the host
/// itself, and every pass forks the same probes a normal refresh does.
const DIAGNOSTIC_CAPACITY_REFRESH: Duration = Duration::from_secs(60);

/// Hold this process in diagnostic registration until its startup cleanup succeeds
/// (CONTEXT.md). Returns only on resume, after which normal startup continues in `run`.
async fn run_diagnostic_mode(
    cfg: &Config,
    health: &Arc<HealthState>,
    station: &Arc<crate::diagnostic::Station>,
) {
    crate::diagnostic::install_process_wide(station);
    let phase = station.phase();
    let fault = match &phase {
        crate::diagnostic::Phase::Diagnostic(fault) => fault.code(),
        crate::diagnostic::Phase::Normal => "none",
    };
    error!(
        token = "boot-diagnostic-mode",
        fault,
        "the startup cleanup did not resolve, so this host enters DIAGNOSTIC MODE: it \
         registers and reports, and refuses every launch. Withheld until it resumes: \
         managed-home GC, the NVIDIA driver-volume and CUDA-runtime provisioners, image \
         pulls and pruning, and host probes. {} The agent retries the cleanup on its own \
         and resumes without a restart.",
        phase.launch_refusal().unwrap_or_default()
    );
    health.set_not_ready(phase.launch_refusal());

    // Independent of the control plane by construction: this task is what resumes the
    // host, and it never reads a connection.
    let retry = tokio::spawn(crate::diagnostic::retry_until_resumed(
        station.clone(),
        crate::diagnostic::startup_cleanup_configured,
        crate::diagnostic::RetryPace::PRODUCTION,
    ));

    let mut backoff = Duration::from_secs(1);
    // #199: see `EnrollmentFallback` — one token attempt per stale-secret reject.
    let mut enrollment_fallback = EnrollmentFallback::default();
    loop {
        let attempt = tokio::select! {
            biased;
            () = station.resumed() => break,
            attempt = diagnostic_connection(
                cfg, health, station, enrollment_fallback.take_for_attempt(),
            ) => attempt,
        };
        let Err(error) = attempt else { break };
        health.set_connected(false);
        // Same predicate for the token and the counting gate, as in the normal loop.
        let explained_refusal = error
            .downcast_ref::<UpgradeRefused>()
            .is_some_and(|r| describe_upgrade_refusal(r.status).is_some());
        if explained_refusal {
            warn!(
                token = "cp-connect-rate-limited",
                "agent connection failed: {error:#}"
            );
        } else {
            error!(
                token = "diagnostic-connection-failed",
                "diagnostic connection failed: {error:#}"
            );
        }
        enrollment_fallback.observe(&error);
        if counts_as_registration_failure(explained_refusal, health.unhealthy()) {
            health.record_registration_failure(&format!("{error:#}"));
        }
        let wait = backoff.min(Duration::from_secs(30));
        info!("reconnecting in {wait:?}");
        // A dead control plane must never delay the resume.
        tokio::select! {
            biased;
            () = station.resumed() => break,
            () = sleep(wait) => {}
        }
        backoff = (wait * 2).min(Duration::from_secs(30));
    }
    retry.abort();
    info!(
        token = "boot-diagnostic-resumed",
        "the startup cleanup succeeded; leaving diagnostic mode and continuing normal startup"
    );
    health.set_ready();
    health.set_connected(false);
}

/// One diagnostic control-plane connection. Reads nothing from the container runtime it
/// could mutate: the images come from the persisted state file and the install facts from
/// read-only inspections that already degrade to `None` on a dead engine.
async fn diagnostic_connection(
    cfg: &Config,
    health: &Arc<HealthState>,
    station: &Arc<crate::diagnostic::Station>,
    prefer_enrollment_token: bool,
) -> anyhow::Result<crate::diagnostic::ConnectionEnd> {
    log_connect_intent(cfg);
    let images = crate::images::register_images_from_state(&cfg.image_state_path());
    let install = offload_probe(|| {
        let runtime = ContainerRuntime::from_env();
        crate::buildinfo::discover_install(&crate::buildinfo::DockerFacts::new(&runtime))
    })
    .await;
    crate::buildinfo::set_install_facts(install.clone());

    let (mut tx, mut rx) = dial(cfg).await?;
    let (_host_id, heartbeat_interval_ms, _policy_identity, _policy_groups) =
        register_and_await_registered(
            cfg,
            &mut tx,
            &mut rx,
            register_message(cfg, prefer_enrollment_token, images, &install, false)?,
        )
        .await?;
    health.set_connected(true);
    // Clear the failure streak before a stale count can flip /health unhealthy; the
    // diagnostic not-ready state is separate and stays.
    health.record_registered();

    crate::diagnostic::serve_registered(
        &mut tx,
        &mut rx,
        heartbeat_interval_ms,
        station,
        {
            let (health, station) = (health.clone(), station.clone());
            move || {
                // Keeps `/health`'s reason on the current fault; never re-arms after a resume.
                if let Some(reason) = station.phase().launch_refusal() {
                    health.set_not_ready(Some(reason));
                }
                diagnostic_observe()
            }
        },
        DIAGNOSTIC_CAPACITY_REFRESH,
    )
    .await
}

/// What a diagnostic capacity message reports: the host as detected, and the local
/// readiness checks. No codec probe (it would initialise GStreamer) and no effective
/// settings (no session may start). Blocking; the caller offloads it.
fn diagnostic_observe() -> (AgentMsg, Vec<crate::messages::ReadinessCheck>) {
    let cap = detect_capacity_blocking();
    let nvidia_host = cap.gpus.iter().any(|g| g.vendor == "nvidia");
    let gpu_present = !cap.gpus.is_empty();
    let checks = crate::readiness::probe(
        &crate::readiness::ProbeEnv::live(nvidia_host, "")
            .with_gpu_present(gpu_present)
            .with_codec_probe(None),
    )
    .into_iter()
    // Not observed in this mode, so not reported: the 32-bit GL path is resolved through
    // the engine, and the codec probe would initialise GStreamer before the driver
    // volume is adopted.
    .filter(|check| !matches!(check.id.as_str(), "nvidia_lib32_gl" | "encoder_codecs"))
    .collect();
    (
        AgentMsg::Capacity {
            source_preparation: None,
            deployment_settings: None,
            config_policy_accepted_groups: None,
            config_policy_legacy_map_applied_id: None,
            host: cap.host,
            gpus: cap.gpus,
            gpu_detection: cap.gpu_detection,
            gpu_detection_reason: cap.gpu_detection_reason,
            console_capabilities: Some(cap.console),
            effective_settings: None,
            codecs: None,
            codec_throughput: None,
            readiness: None,
        },
        checks,
    )
}

// Each argument is a distinct process-lifetime handle.
#[allow(clippy::too_many_arguments)]
async fn connect_and_run(
    cfg: &Config,
    health: &Arc<HealthState>,
    nvidia_lib32_probed: &str,
    agent_image_identity: &str,
    image_mgr: &Arc<ImageManager>,
    release_mgr: &Arc<ReleaseManager>,
    sessions: &mut HostSessions,
    // #199: set for exactly one attempt, by a previous attempt this control plane
    // refused with `host_not_found`. See `stale_identity`.
    prefer_enrollment_token: bool,
) -> anyhow::Result<()> {
    log_connect_intent(cfg);

    // Everything `register` needs from the container runtime is gathered BEFORE the
    // socket is opened (#191). The control plane gives a fresh connection its
    // handshake window (agentws `handshakeTimeout`, 15 s) to send `register`; these
    // two probes used to run after the dial, and on a host whose docker daemon
    // answers `inspect` slowly (each is bounded at 30 s) they pushed `register` past
    // that deadline. The control plane closed the socket without a close frame, the
    // agent wrote `register` into a dead connection, and every reconnect repeated
    // the same probes into the same wall.
    let prep_started = Instant::now();

    // agent-api.md: recorded images are verified against the docker daemon on startup
    // AND reconnect — an image `docker rmi`'d out from under a long-lived agent must
    // not keep reporting `ready`. Runs before the upstream attaches, so the
    // attach-time flush reports post-reconciliation states.
    let images = {
        let mgr = image_mgr.clone();
        tokio::task::spawn_blocking(move || mgr.refresh_register_images()).await?
    };

    // Re-discovered per connection, not once at boot: an updater that starts
    // after the agent must not leave the host reporting updater_present=false
    // forever. Offloaded because it shells out to docker.
    let install = offload_probe(|| {
        let runtime = ContainerRuntime::from_env();
        crate::buildinfo::discover_install(&crate::buildinfo::DockerFacts::new(&runtime))
    })
    .await;
    crate::buildinfo::set_install_facts(install.clone());

    let prep = prep_started.elapsed();
    if register_prep_over_budget(prep) {
        warn!(
            token = "register-prep-slow",
            elapsed_ms = prep.as_millis() as u64,
            "register preparation (image reconcile + install probe against the container \
             runtime) took {prep:?} — the container runtime is answering slowly; \
             registration still proceeds, but sessions on this host will feel it"
        );
    }

    let (mut tx, mut rx) = dial(cfg).await?;

    // Attach this connection's upstream channel to the process-wide ImageManager.
    // Attaching also flushes every op-free record's current state (terminal states
    // reached while disconnected, plus a reconnect resync). The guard detaches on
    // every exit path; a pull that outlives the connection is re-delivered on the
    // next attach.
    let (image_tx, image_rx) = mpsc::channel::<AgentMsg>(64);
    // `Option`-wrapped for `recv_or_disabled`.
    let mut image_rx = Some(image_rx);
    let _image_upstream_guard = image_mgr.attach_upstream(image_tx);

    // Same shape for `release_state`. Attaching re-emits the current state of every
    // updater result file still present, which is how an apply that destroyed the
    // previous agent still gets reported (agent-api.md `release_state`).
    let (release_tx, release_rx) = mpsc::channel::<AgentMsg>(16);
    let mut release_rx = Some(release_rx);
    let _release_upstream_guard = release_mgr.attach_upstream(release_tx);

    // --- Steps 1 and 2: send register, receive registered ---
    let (host_id, heartbeat_interval_ms, policy_identity, policy_groups) =
        register_and_await_registered(
            cfg,
            &mut tx,
            &mut rx,
            register_message(
                cfg,
                prefer_enrollment_token,
                images,
                &install,
                sessions.mgr.home_cleanup.is_some(),
            )?,
        )
        .await?;
    let path = std::path::PathBuf::from(format!("{}.policy.json", cfg.node_secret_path));
    let advertised = crate::policy::PolicyAgent::advertised_groups(&path);
    let mut baseline = crate::session::settings::RuntimeSettings::baseline();
    seed_nvidia_lib32(&mut baseline, nvidia_lib32_probed);
    sessions.mgr.deployment_baseline = baseline.clone();
    sessions.mgr.runtime_settings = baseline;
    let (boot, connection) = policy_identity.clone().unwrap_or_default();
    let mut policy = crate::policy::PolicyAgent::open(
        path,
        host_id.clone(),
        boot,
        connection.clone(),
        &mut sessions.mgr.runtime_settings,
    )?;
    if let Some(groups) = &policy_groups {
        if let Err(code) = policy.confirm_groups(&advertised, groups) {
            send(
                &mut tx,
                &AgentMsg::ConfigPolicyFeatureError {
                    code,
                    group: None,
                    connection_incarnation: connection,
                },
            )
            .await?;
            anyhow::bail!("RH05 ownership echo invalid");
        }
    }
    sessions.mgr.policy_session_ready = policy_identity.is_none() && !policy.has_sticky_ownership();
    sessions.mgr.policy_accepted_groups = policy_groups;
    sessions.mgr.policy_delivery_ack = None;
    sessions.mgr.policy_inventory_complete = false;
    sessions.mgr.policy_agent = Some(policy);
    health.set_connected(true);
    // #128: the control plane is back, so the sessions held across the outage are
    // safe. Disarmed HERE rather than at the top of the reconnect loop: doing it
    // there ran before each connection ATTEMPT, so every failed retry re-armed a
    // fresh 90 s and a control plane that never returned never stopped anything.
    sessions.registered_this_connection = true;
    if let Some(t) = sessions.grace_timer.take() {
        // is_finished() distinguishes "we beat the deadline" from "we did not".
        // Aborting a completed task is a no-op, so without this check a control
        // plane returning at 91 s logged `session-grace-expired` and then
        // `session-grace-cleared` while the sessions were being torn down --
        // exactly the pair a live gate reads to decide whether this works.
        if t.is_finished() {
            warn!(
                token = "session-grace-missed",
                "control plane returned AFTER the grace window; the held sessions were already stopped"
            );
        } else {
            t.abort();
            info!(
                token = "session-grace-cleared",
                "control plane returned within the grace window; held sessions continue"
            );
        }
    }
    // Clear the failure streak before a stale count can flip /health unhealthy.
    health.record_registered();

    // --- Step 3: send capacity ---
    let mut cap = offload_probe(detect_capacity_blocking).await;
    info!(
        "detected capacity: {} cores, {} MB RAM, {} GPU(s)",
        cap.host.cpu_cores,
        cap.host.mem_mb,
        cap.gpus.len()
    );
    for g in &cap.gpus {
        info!(
            "  GPU {}: {} {} — {} MB VRAM, {} encode slots",
            g.index, g.vendor, g.model, g.vram_mb_total, g.encode_slots_total
        );
    }
    if let Some(storage) = &cap.host.storage {
        for v in storage {
            info!(
                "  storage {}: {} — {} MB total, {} MB available",
                v.label, v.path, v.total_mb, v.available_mb
            );
        }
    }
    info!(
        "console capabilities: {} connector(s), {} audio sink(s), {} input device(s)",
        cap.console.connectors.len(),
        cap.console.audio_sinks.len(),
        cap.console.input_devices.len()
    );
    let gpu_inventory = cap.gpus.clone();
    let vram_targets: Vec<VramTarget> = cap.vram_targets;
    // The env baseline with the startup-probed lib32 path seeded in, so the very first
    // capacity report already carries the auto-detected value. Matches what
    // `SessionManager::new` seeds; the first config_update re-sends the overlay view.
    let first_settings = sessions.mgr.runtime_settings.clone();
    // Probed once (the gst registry is process-stable) and reused in every capacity
    // re-send below.
    let host_codec_report = {
        let settings = first_settings.clone();
        offload_probe(move || probe_host_codecs(&settings)).await
    };
    // The host readiness check set: advisory only, reported and logged, never gating.
    // Every input is already paid for (the vendor read, the #375 lib32 probe, the
    // codec probe above), so nothing here re-probes or launches a container.
    let nvidia_host = cap.gpus.iter().any(|g| g.vendor == "nvidia");
    let gpu_present = !cap.gpus.is_empty();
    let readiness = {
        let lib32 = first_settings.nvidia_lib32_path.clone();
        let probed_codecs = host_codec_report.as_ref().map(|r| r.codecs.clone());
        offload_probe(move || {
            crate::readiness::probe(
                &crate::readiness::ProbeEnv::live(nvidia_host, &lib32)
                    .with_gpu_present(gpu_present)
                    .with_codec_probe(probed_codecs.as_deref()),
            )
        })
        .await
    };
    crate::readiness::log_report(&readiness);
    sessions
        .mgr
        .readiness
        .refreshed(readiness.clone(), SystemTime::now());
    // A host-probe result produced while disconnected: pure in-memory work, so it is
    // safe inside the handshake window, unlike everything above it.
    while let Ok(update) = sessions.probe_updates.try_recv() {
        crate::host_probe::orchestrator::apply(
            &mut sessions.mgr.readiness,
            &mut sessions.mgr.codec_evidence,
            update,
        );
    }
    sessions.mgr.agent_image_identity = agent_image_identity.to_string();
    // Not yet `mgr`'s stack (`begin_connection` adopts it below), so built from the same
    // locals this message reports.
    let first_gpu_codec_sets = gpu_codec_sets(
        &CodecStack {
            agent_image: agent_image_identity,
            gpus: &gpu_inventory,
            settings: &first_settings,
            registry: host_codec_report.is_some(),
            layers: sessions.mgr.codec_layers,
        },
        &sessions.mgr.readiness,
        &sessions.mgr.codec_evidence,
    );
    let first_codecs = Some(host_codecs_from_sets(&first_gpu_codec_sets));
    apply_gpu_codecs(&mut cap.gpus, &first_gpu_codec_sets);
    let capacity_msg = AgentMsg::Capacity {
        source_preparation: None,
        deployment_settings: Some(sessions.mgr.deployment_baseline.deployment_map()),
        config_policy_accepted_groups: sessions.mgr.policy_accepted_groups.clone(),
        config_policy_legacy_map_applied_id: None,
        host: cap.host,
        gpus: cap.gpus,
        gpu_detection: cap.gpu_detection,
        gpu_detection_reason: cap.gpu_detection_reason,
        console_capabilities: Some(cap.console),
        effective_settings: Some(first_settings.effective_map()),
        codecs: first_codecs,
        codec_throughput: advertised_codec_throughput(&host_codec_report),
        readiness: Some(sessions.mgr.readiness.merged()),
    };
    send(&mut tx, &capacity_msg).await?;
    info!("capacity report sent");
    if let Some(ledger) = sessions.mgr.home_cleanup.as_mut() {
        for session_id in ledger.take_recovered() {
            send(
                &mut tx,
                &qualified_home_terminal(&session_id, crate::home_cleanup::TerminalKind::Failed),
            )
            .await?;
        }
    }

    // Sent first on purpose: the card carries the remediation, and the gate below may end
    // the process a few seconds later.
    boot_sanity_gate(&readiness, gpu_present).await;

    // CM-06/07: re-send capacity on a debounced console hotplug so the control plane's
    // connector-diff auto-start/stop sees it promptly. Spawned after `registered`; the
    // guard's Drop stops the thread on every disconnect path, so it never survives
    // into a reconnect.
    let (hotplug_tx, hotplug_rx) = mpsc::channel::<String>(1);
    // `Option`-wrapped for `recv_or_disabled`.
    let mut hotplug_rx = Some(hotplug_rx);
    let _hotplug_guard = ConsoleHotplugWatcher::spawn(hotplug_tx);

    // --- Step 4: lifecycle loop ---
    let interval = Duration::from_millis(heartbeat_interval_ms);
    let mut hb_timer = tokio::time::interval(interval);
    hb_timer.tick().await; // discard the immediate first tick

    // #128: the session map, its channels and the home refs OUTLIVE this
    // connection, so sessions survive a control-plane restart. Everything scoped
    // to one connection is reset here instead of by the struct being rebuilt.
    let HostSessions {
        mgr,
        evt_tx,
        evt_rx,
        diagnostic_tx,
        diagnostic_rx,
        diagnostic_dropped_interval,
        diagnostic_dropped_total,
        probe_updates,
        registered_this_connection: _,
        grace_timer: _,
    } = sessions;
    mgr.begin_connection(gpu_inventory, vram_targets);
    // #175: home refs mounted by live sessions. The GC reaper consults it so it
    // can never reap a backing store an active session is using. Hoisted with the
    // map: a fresh set would let the next connection's GC reap a home a surviving
    // session still has mounted.
    let live_refs: LiveRefs = mgr.live_refs.clone();
    // Cached with the encoder it was probed for, so capacity re-sends reuse it unless
    // a config_update flips the effective encoder and marks it stale.
    mgr.host_codec_report = host_codec_report.clone();
    mgr.probed_encoder = Some(first_settings.encoder);

    // #488: the golden-home warm-up. Scheduled by the control plane and claimed over
    // the additive `/v1/agent/jobs/*` HTTP pull, so `protocol/agent-api.md` is
    // untouched. Registered EVEN WHEN THE FEATURE IS OFF: the runner then reports
    // `skipped` with a reason, so "not configured" and "nothing to do" stay
    // distinguishable, and the gate collaborators stay wired either way.
    let warmup_store = crate::session::warmup::resolve_store(&mgr.runtime_settings.home_root);
    let warmup_activity = Arc::new(crate::session::warmup::HostActivity::new());
    let warmup_control = Arc::new(crate::session::warmup::WarmupControl::new());
    if let Some(handle) = mgr.probe_handle.clone() {
        // A probe's own release fires this too; the scheduler ignores it when
        // nothing waits on the gate.
        warmup_control.set_release_listener(move || handle.encode_gate_freed());
    }
    let source_policy = crate::source_policy::SourcePolicy::new(
        &mgr.runtime_settings.home_root,
        warmup_control.clone(),
        image_mgr.clone(),
    );
    let _source_policy_guard = crate::source_policy::ConnectionGuard(source_policy.clone());
    mgr.source_policy = Some(source_policy.clone());
    let warmup_runner = Arc::new(
        crate::session::warmup::WarmupJobRunner::new(
            crate::session::warmup::WarmupConfig::from_env(),
            warmup_store.clone(),
            Arc::new(crate::session::warmup::host::AgentWarmupHost::new(
                ContainerRuntime::from_env(),
                mgr.runtime_settings.clone(),
            )),
            warmup_control.clone(),
            warmup_activity.clone(),
            app_uid_gid(),
        )
        .with_policy(source_policy.clone()),
    );
    mgr.warmup_activity = Some(warmup_activity);
    mgr.warmup_control = Some(warmup_control.clone());
    mgr.note_session_count();
    // Outside the registration handshake window (it closed with the capacity
    // message sent above) and after `warmup_control` is set, so a `Start` the
    // orchestrator issues right away sees this connection's real gate.
    mgr.set_probe_context();
    if let Some(handle) = mgr.probe_handle.clone() {
        let inputs = probe_inputs(mgr);
        mgr.notified_probe_inputs = Some(inputs.clone());
        handle.registered(inputs);
    }
    // The one image-lifecycle duty that stayed agent-side: drop a template whose image
    // was uninstalled. Detached on disconnect (the ImageManager is process-wide); the
    // guard also aborts a warm-up that would otherwise outlive its connection (#489).
    image_mgr.set_lifecycle_observer(Some(source_policy));
    let _warmup_guard =
        crate::session::warmup::WarmupConnectionGuard::new(warmup_control, image_mgr.clone());
    // Tracks the reservation across heartbeats so a flip triggers exactly one
    // capacity re-send.
    let mut last_warmup_reserved = false;
    let mut last_source_report: Option<serde_json::Value> = None;
    // Device-lost failures across sessions on this connection: ≥2 within
    // GPU_GLOBAL_WINDOW escalate to a GPU-global drain+restart; one stays per-session.
    let mut gpu_fault = GpuGlobalFaultDetector::default();
    // `host.xid` / `host.gpu_fault`: the kernel ring-buffer tailer, on its own thread
    // off the media path. `spawn` returns `None` and drops `tx` when `/dev/kmsg` is
    // unreadable (the container default) — the busy-spin path `recv_or_disabled` fixes.
    let (gpu_fault_tx, gpu_fault_rx) = mpsc::unbounded_channel::<crate::gpu_kmsg::GpuFault>();
    let mut gpu_fault_rx = Some(gpu_fault_rx);
    let _gpu_kmsg_thread = crate::gpu_kmsg::spawn(gpu_fault_tx);

    // Steam library discovery: the ACF manifest scanner. Per-connection lifetime
    // (aborted by `_library_scan_guard`'s Drop), node_secret auth, never fatal to the
    // agent. The agent never learns a user — it walks a path the control plane gives
    // it and reports the manifests it finds.
    let _library_scan_guard = match current_node_secret(cfg).map(|s| cp_client(cfg, s)) {
        Some(Ok(cp)) => Some(spawn_library_scanner(cp)),
        Some(Err(e)) => {
            warn!(
                token = "library-scan-client-unavailable",
                "library-scan: {e} — skipping library scanner this connection"
            );
            None
        }
        None => {
            warn!(
                token = "library-scan-no-secret",
                "library-scan: no node_secret available — skipping library scanner this connection"
            );
            None
        }
    };

    // The generic job poller: claims control-plane-scheduled runs for this host and
    // dispatches each to a registered `JobRunner`. Same posture as the scanner above.
    // Two runners are registered (`template.warmup`, `home.gc`), both host-scoped with
    // their schedules in the control plane's `jobs` table; adding a third is a
    // `register` here plus a `Definition` there. Runners are built before the poller
    // because an empty registry spawns no task at all.
    let _job_poller_guard = match current_node_secret(cfg).map(|s| cp_client(cfg, s)) {
        Some(Err(e)) => {
            warn!(
                token = "job-poller-client-unavailable",
                "job: {e} — skipping the job poller this connection"
            );
            None
        }
        Some(Ok(cp)) => {
            let mut registry = crate::jobs::JobRegistry::new();
            registry.register(warmup_runner);
            registry.register(std::sync::Arc::new(
                crate::session::gc::HomeGcJobRunner::new(cp.clone(), live_refs.clone()),
            ));
            crate::jobs::spawn_job_poller(cp, std::sync::Arc::new(registry))
        }
        None => {
            warn!(
                token = "job-poller-no-secret",
                "job: no node_secret available — skipping the job poller this connection \
                 (template.warmup and home.gc will not run)"
            );
            None
        }
    };

    // Readiness observes filesystem and provisioning state, which can change while
    // connected. Keep probes off the WebSocket loop and allow only one in flight.
    let mut readiness_timer = tokio::time::interval(READINESS_REFRESH_INTERVAL);
    readiness_timer.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let (readiness_tx, mut readiness_rx) = tokio::sync::mpsc::channel(1);
    let mut readiness_busy = false;

    loop {
        if policy_identity.is_some()
            && !mgr.policy_seed_reconnect_used
            && mgr.policy_delivery_ack.is_some()
            && mgr
                .policy_agent
                .as_ref()
                .is_some_and(|agent| agent.has_seed() && agent.has_unadvertised_groups(&advertised))
            && mgr.pending.is_empty()
            && mgr.running.is_empty()
            && !mgr.warmup_reserved()
        {
            mgr.policy_seed_reconnect_used = true;
            return Err(PolicySeedReconnect.into());
        }
        tokio::select! {
            _ = readiness_timer.tick(), if !readiness_busy => {
                readiness_busy = true;
                let sender = readiness_tx.clone();
                let lib32 = mgr.runtime_settings.nvidia_lib32_path.clone();
                let codecs = mgr.host_codec_report.as_ref().map(|r| r.codecs.clone());
                tokio::spawn(run_readiness_refresh(
                    move || crate::readiness::probe(
                        &crate::readiness::ProbeEnv::live(nvidia_host, &lib32)
                            .with_gpu_present(gpu_present)
                            .with_codec_probe(codecs.as_deref()),
                    ),
                    READINESS_REFRESH_DEADLINE,
                    sender,
                ));
            }
            Some(refresh) = readiness_rx.recv() => {
                match refresh {
                    ReadinessRefresh::Done(Ok(checks)) => {
                        readiness_busy = false;
                        mgr.readiness.refreshed(checks, SystemTime::now());
                    }
                    ReadinessRefresh::Done(Err(error)) => {
                        readiness_busy = false;
                        warn!(token = "readiness-refresh-failed", "host readiness refresh failed: {error}");
                        mgr.readiness.refresh_failed();
                    }
                    // Still busy: `Done` follows when the probe ends.
                    ReadinessRefresh::Overdue => {
                        warn!(token = "readiness-refresh-overdue", "host readiness refresh is past its {} s deadline and still running", READINESS_REFRESH_DEADLINE.as_secs());
                        mgr.readiness.refresh_failed();
                    }
                }
                send_fresh_capacity(&mut tx, &mut *mgr).await?;
            }

            _ = hb_timer.tick() => {
                image_mgr.flush_terminal_states();
                let ts_unix_ms = SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .unwrap_or_default()
                    .as_millis() as i64;
                // #383: this tick's live-VRAM sample never blocks the heartbeat —
                // single-flighted so a hung sampler is not re-entered, and bounded by
                // killing the `nvidia-smi` fork. The heartbeat attaches whatever the
                // cache already holds; it never awaits a fresh sample.
                mgr.vram_cache.spawn_tick(mgr.vram_targets.clone());
                let gpu_vram = mgr.vram_cache.read();
                // Reconcile BEFORE running_ids/drain_metrics, so a session whose runner
                // died without a terminal event is neither reported live nor drained.
                for reaped in mgr.reconcile(Instant::now(), RUNNER_REAP_GRACE, PENDING_ASSIGNMENT_TTL) {
                    send(&mut tx, &reaped).await?;
                }
                let running = mgr.running_ids();
                let running_count = running.len();
                let hb = AgentMsg::Heartbeat {
                    running_sessions: running,
                    ts_unix_ms,
                    gpu_vram,
                };
                send(&mut tx, &hb).await?;
                debug!("heartbeat sent (host={host_id}, running={})", running_count);
                // P4-03: one session_metrics per running session, same cadence.
                for m in mgr.drain_metrics(ts_unix_ms) {
                    send(&mut tx, &m).await?;
                }
                // The reservation is taken and released by the warm-up thread, which
                // cannot send on this socket, so the heartbeat notices the flip. A
                // report one beat late costs nothing — the gate is what serializes.
                let source_report = mgr.source_policy.as_ref().and_then(|p| p.report());
                if source_report != last_source_report {
                    last_source_report = source_report;
                    send_fresh_capacity(&mut tx, &mut *mgr).await?;
                }
                if mgr.warmup_reserved() != last_warmup_reserved {
                    last_warmup_reserved = mgr.warmup_reserved();
                    send_fresh_capacity(&mut tx, &mut *mgr).await?;
                    info!(
                        "re-sent capacity: warm-up encode-slot reservation {}",
                        if last_warmup_reserved { "taken" } else { "released" }
                    );
                }
                let dropped = diagnostic_dropped_interval.swap(0, Ordering::Relaxed);
                if dropped > 0 {
                    let dropped_total = diagnostic_dropped_total.load(Ordering::Relaxed);
                    warn!(
                        token = "diagnostic-lane-dropped",dropped, dropped_total, "bounded diagnostic event lane dropped trace events since last heartbeat");
                }
            }
            inbound = rx.next() => {
                match inbound {
                    Some(Ok(Message::Text(raw))) => {
                        let ctrl: ControlMsg = match serde_json::from_str(&raw) {
                            Ok(c) => c,
                            Err(e) => { warn!(
                                token = "control-message-malformed","malformed control message: {e} (raw={raw})"); continue; }
                        };
                        // Handled here, not in handle_control, so the ack flushes before
                        // the process exits. The restart policy brings us back.
                        if let ControlMsg::Restart { id } = &ctrl {
                            info!("restart requested (cmd {id}); acking then exiting for config reload");
                            let reply = AgentMsg::Ack { id: id.clone(), ok: true, error: None };
                            let _ = send(&mut tx, &reply).await;
                            tokio::time::sleep(std::time::Duration::from_millis(250)).await;
                            std::process::exit(0);
                        }
                        // A config_update changes the reported effective settings; check
                        // before handle_control consumes ctrl.
                        let was_config_update = matches!(ctrl, ControlMsg::ConfigUpdate { .. });
                        if let Some(reply) = mgr.handle_control(ctrl, evt_tx, diagnostic_tx) {
                            send_control_reply(&mut tx, &mut *mgr, reply).await?;
                        }
                        for report in mgr.home_cleanup_reports.drain(..) {
                            send(&mut tx, &report).await?;
                        }
                        if was_config_update {
                            // The overlay may have flipped the effective encoder live, so
                            // re-probe before re-sending capacity: a stale hosts.codecs
                            // either hides a codec or routes (say) an h265 session to a
                            // host whose live encoder cannot produce it, which fails it.
                            if mgr.host_codecs_stale() {
                                let settings = mgr.runtime_settings.clone();
                                mgr.host_codec_report =
                                    offload_probe(move || probe_host_codecs(&settings)).await;
                                mgr.probed_encoder = Some(mgr.runtime_settings.encoder);
                                info!(
                                    "effective encoder changed by config_update; re-probed codecs: {:?}",
                                    mgr.host_codec_report
                                );
                            }
                            let cap = offload_probe(detect_capacity_blocking).await;
                            mgr.adopt_inventory(cap.gpus.clone(), cap.vram_targets);
                            // A `config_update` can move the encoder/render node/GPU set a
                            // probe result depended on.
                            mgr.notify_probe_inputs();
                            // Reported-copy only — see `send_fresh_capacity`.
                            let mut cap_gpus = cap.gpus;
                            crate::session::warmup::apply_encode_slot_reservation(
                                &mut cap_gpus,
                                mgr.warmup_reserved(),
                            );
                            let gpu_sets = mgr.gpu_codec_sets();
                            apply_gpu_codecs(&mut cap_gpus, &gpu_sets);
                            let capacity_msg = AgentMsg::Capacity {
            deployment_settings: Some(mgr.deployment_baseline.deployment_map()),
            config_policy_accepted_groups: mgr.policy_accepted_groups.clone(),
            config_policy_legacy_map_applied_id: mgr.policy_delivery_ack.clone(),

            source_preparation: mgr.source_policy.as_ref().and_then(|p| p.report()),
                                host: cap.host,
                                gpus: cap_gpus,
                                gpu_detection: cap.gpu_detection,
                                gpu_detection_reason: cap.gpu_detection_reason,
                                console_capabilities: Some(cap.console),
                                effective_settings: Some(mgr.runtime_settings.effective_map()),
                                codecs: Some(host_codecs_from_sets(&gpu_sets)),
                                codec_throughput: advertised_codec_throughput(&mgr.host_codec_report),
                                readiness: Some(mgr.readiness.merged()),
                            };
                            send(&mut tx, &capacity_msg).await?;
                            info!("re-sent capacity after config_update (fresh effective_settings)");
                        }
                    }
                    Some(Ok(Message::Close(_))) | None => {
                        anyhow::bail!("control-plane WebSocket closed");
                    }
                    Some(Ok(_)) => {} // ignore binary / ping / pong
                    Some(Err(e)) => return Err(e.into()),
                }
            }
            hp = recv_or_disabled(&mut hotplug_rx) => {
                // Also fires on the watcher's debounced storage-delta tick, reused
                // rather than adding a second poll thread. `None` means the watcher's
                // sender is gone: disable the arm (see `recv_or_disabled`).
                let Some(reason) = hp else {
                    error!(
                        token = "hotplug-channel-closed",
                        "hotplug sender dropped unexpectedly; disabling console hotplug \
                         capacity refresh for the rest of this connection"
                    );
                    hotplug_rx = None;
                    continue;
                };
                {
                    let cap = offload_probe(detect_capacity_blocking).await;
                    mgr.adopt_inventory(cap.gpus.clone(), cap.vram_targets);
                    // A hotplug can change the GPU set a probe result depended on.
                    mgr.notify_probe_inputs();
                    info!(
                        "console hotplug: {reason}; re-sending capacity ({} connector(s), {} audio sink(s), {} input device(s))",
                        cap.console.connectors.len(),
                        cap.console.audio_sinks.len(),
                        cap.console.input_devices.len()
                    );
                    // Reported-copy only — see `send_fresh_capacity`.
                    let mut cap_gpus = cap.gpus;
                    crate::session::warmup::apply_encode_slot_reservation(
                        &mut cap_gpus,
                        mgr.warmup_reserved(),
                    );
                    let gpu_sets = mgr.gpu_codec_sets();
                    apply_gpu_codecs(&mut cap_gpus, &gpu_sets);
                    let capacity_msg = AgentMsg::Capacity {
            deployment_settings: Some(mgr.deployment_baseline.deployment_map()),
            config_policy_accepted_groups: mgr.policy_accepted_groups.clone(),
            config_policy_legacy_map_applied_id: mgr.policy_delivery_ack.clone(),

            source_preparation: mgr.source_policy.as_ref().and_then(|p| p.report()),
                        host: cap.host,
                        gpus: cap_gpus,
                        gpu_detection: cap.gpu_detection,
                        gpu_detection_reason: cap.gpu_detection_reason,
                        console_capabilities: Some(cap.console),
                        effective_settings: Some(mgr.runtime_settings.effective_map()),
                        codecs: Some(host_codecs_from_sets(&gpu_sets)),
                        codec_throughput: advertised_codec_throughput(&mgr.host_codec_report),
                        readiness: Some(mgr.readiness.merged()),
                    };
                    send(&mut tx, &capacity_msg).await?;
                }
            }
            evt = recv_or_disabled(&mut *evt_rx) => {
                // `None` means every sender is gone — disable the arm.
                let Some((session_id, event)) = evt else {
                    error!(
                        token = "evt-channel-closed",
                        "session-event sender dropped unexpectedly; disabling session-event \
                         handling for the rest of this connection"
                    );
                    *evt_rx = None;
                    continue;
                };
                {
                    match event {
                        SessionEvent::Signaling(sig_msg) => {
                            let v = serde_json::to_value(&sig_msg)
                                .unwrap_or(serde_json::Value::Null);
                            let relay = AgentMsg::Signaling {
                                session_id,
                                msg: v,
                            };
                            send(&mut tx, &relay).await?;
                        }
                        // P5-03: pre-terminal bytes_used must arrive BEFORE
                        // session_state{stopped} so the control plane still sees
                        // the session as running when it processes the metric.
                        SessionEvent::Stopped { bytes_used, detail } => {
                            if let Some(bu) = bytes_used {
                                if let Some(h) = mgr.running.get(&session_id) {
                                    let ts = SystemTime::now()
                                        .duration_since(UNIX_EPOCH)
                                        .unwrap_or_default()
                                        .as_millis() as i64;
                                    let now = std::time::Instant::now();
                                    let w = h.metrics.drain_window(now);
                                    let pre_terminal = AgentMsg::session_metrics(
                                        session_id.clone(),
                                        ts,
                                        &w,
                                        Some(bu),
                                    );
                                    send(&mut tx, &pre_terminal).await?;
                                }
                            }
                            // #503: get pending trace events out before the terminal
                            // state — the control plane drops them afterwards.
                            flush_pending_diagnostics(&mut tx, &mut *diagnostic_rx).await?;
                            if let Some(msg) = mgr.prove_home_terminal(
                                &session_id,
                                SessionEvent::Stopped { bytes_used, detail },
                            ) {
                                send(&mut tx, &msg).await?;
                                // Console auto-start is level-triggered by capacity.
                                send_fresh_capacity(&mut tx, &mut *mgr).await?;
                                info!("re-sent capacity after session stopped for console reconciliation");
                            }
                        }
                        SessionEvent::EffectiveMedia(payload) => {
                            let ts_unix_ms = SystemTime::now()
                                .duration_since(UNIX_EPOCH)
                                .unwrap_or_default()
                                .as_millis() as i64;
                            let msg = AgentMsg::SessionTraceEvent {
                                session_id,
                                ts_unix_ms,
                                event: "session.effective_media".to_string(),
                                payload,
                            };
                            send(&mut tx, &msg).await?;
                        }
                        // The reliable ORDERED lifecycle lane, so these are already
                        // sequenced before this session's terminal `session_state` —
                        // the ordering `flush_pending_diagnostics` manufactures for the
                        // droppable lane comes free here.
                        SessionEvent::Capture { event, payload }
                        | SessionEvent::Trace { event, payload } => {
                            let ts_unix_ms = SystemTime::now()
                                .duration_since(UNIX_EPOCH)
                                .unwrap_or_default()
                                .as_millis() as i64;
                            let msg = AgentMsg::SessionTraceEvent {
                                session_id,
                                ts_unix_ms,
                                event: event.to_string(),
                                payload,
                            };
                            send(&mut tx, &msg).await?;
                        }
                        other => {
                            let terminal = matches!(
                                &other,
                                SessionEvent::Failed(_) | SessionEvent::AppFailed { .. }
                            );
                            // Classify for the GPU-global detector BEFORE `on_event`
                            // consumes the event. A pre-session device-open failure is
                            // GPU-global at once; a DEVICE_LOST only once ≥2 land in
                            // the window.
                            let gpu_global = match &other {
                                SessionEvent::Failed(reason)
                                    if vulkan_fault::reason_is_device_open_failed(reason) =>
                                {
                                    true
                                }
                                SessionEvent::Failed(reason)
                                    if vulkan_fault::reason_is_device_lost(reason) =>
                                {
                                    gpu_fault.record_device_lost(Instant::now())
                                }
                                _ => false,
                            };
                            // Which host probes could explain this, and the GPU it ran
                            // on — captured before `on_event` drops the `running` entry.
                            let launch_failure = match &other {
                                SessionEvent::Failed(reason) => Some((reason.clone(), false)),
                                SessionEvent::AppFailed { reason, .. } => {
                                    Some((reason.clone(), true))
                                }
                                _ => None,
                            };
                            let failed_on = mgr
                                .running
                                .get(&session_id)
                                .map(|h| (h.gpu_index, h.codec));
                            // #503: same pre-terminal flush as the `Stopped` arm —
                            // `webrtc.remote_description_failed` is emitted by the
                            // runner immediately before this very event.
                            if terminal {
                                flush_pending_diagnostics(&mut tx, &mut *diagnostic_rx).await?;
                            }
                            let msg = if terminal {
                                mgr.prove_home_terminal(&session_id, other)
                            } else {
                                Some(mgr.on_event(&session_id, other))
                            };
                            if let Some(msg) = msg {
                                send(&mut tx, &msg).await?;
                                if terminal {
                                    send_fresh_capacity(&mut tx, &mut *mgr).await?;
                                    info!("re-sent capacity after session failure for console reconciliation");
                                }
                            }
                            if let (Some((reason, app_failed)), Some((gpu, codec))) =
                                (launch_failure, failed_on)
                            {
                                let explains =
                                    host_probe::launch_failure::explains(&reason, app_failed);
                                if !explains.is_empty() {
                                    if let Some(handle) = &mgr.probe_handle {
                                        handle.launch_failed(gpu, explains, codec);
                                    }
                                }
                            }
                            if gpu_global && !mgr.draining {
                                error!(
                                    token = "gpu-fault-drain-started",
                                    "GPU-global Vulkan fault detected (session {session_id}); \
                                     draining sessions and restarting agent for a clean gst reset"
                                );
                                // The restart is the clean reset: `gst::init` is a
                                // process-wide `Once`.
                                mgr.begin_drain();
                                // Guarded restart: let in-flight sessions unwind for a
                                // bounded window, then exit(0) and let the container
                                // restart policy bring the agent back. Never an exit
                                // from a `Drop` — that remains banned.
                                tokio::spawn(async move {
                                    sleep(vulkan_fault::GPU_GLOBAL_DRAIN_TIMEOUT).await;
                                    error!(
                                        token = "gpu-fault-restart-now",
                                        "GPU-global drain window elapsed; restarting agent process now"
                                    );
                                    std::process::exit(0);
                                });
                            }
                        }
                    }
                }
            }
            diagnostic = recv_or_disabled(&mut *diagnostic_rx) => {
                // `None` means every sender is gone — disable the arm.
                let Some((session_id, te)) = diagnostic else {
                    error!(
                        token = "diagnostic-channel-closed",
                        "diagnostic-event sender dropped unexpectedly; disabling diagnostic \
                         trace forwarding for the rest of this connection"
                    );
                    *diagnostic_rx = None;
                    continue;
                };
                let msg = AgentMsg::SessionTraceEvent {
                    session_id,
                    ts_unix_ms: te.ts_unix_ms,
                    event: te.event.to_string(),
                    payload: te.payload,
                };
                send(&mut tx, &msg).await?;
            }
            // A kernel-reported GPU fault belongs to the host, not a session (the kernel
            // does not know whose work faulted), so it goes to every running session and
            // to none when there are none. Always on `tx`, never the bounded diagnostics
            // lane: an Xid must not be droppable.
            fault = recv_or_disabled_unbounded(&mut gpu_fault_rx) => {
                // `None` is EXPECTED whenever `/dev/kmsg` is unreadable (the container
                // default: `gpu_kmsg::spawn` drops `tx` immediately), hence info rather
                // than warn. Disable the arm either way.
                let Some(fault) = fault else {
                    info!(
                        token = "gpu-fault-channel-closed",
                        "gpu-fault sender gone (kmsg tailer not running); disabling the \
                         host.gpu_fault forwarding arm for the rest of this connection"
                    );
                    gpu_fault_rx = None;
                    continue;
                };
                {
                    for session_id in mgr.running_ids() {
                        let msg = AgentMsg::SessionTraceEvent {
                            session_id,
                            ts_unix_ms: fault.payload["ts_unix_ms"].as_i64().unwrap_or_default(),
                            event: fault.event.to_string(),
                            payload: fault.payload.clone(),
                        };
                        send(&mut tx, &msg).await?;
                    }
                }
            }
            // `image_state` emissions from the ImageManager's pull/remove threads.
            img = recv_or_disabled(&mut image_rx) => {
                // `None` means every sender is gone — disable the arm.
                let Some(msg) = img else {
                    error!(
                        token = "image-channel-closed",
                        "image-state sender dropped unexpectedly; disabling image-state \
                         forwarding for the rest of this connection"
                    );
                    image_rx = None;
                    continue;
                };
                send(&mut tx, &msg).await?;
            }
            // `release_state` emissions from the release poller thread.
            rel = recv_or_disabled(&mut release_rx) => {
                let Some(msg) = rel else {
                    error!(
                        token = "release-channel-closed",
                        "release-state sender dropped unexpectedly; disabling release-state \
                         forwarding for the rest of this connection"
                    );
                    release_rx = None;
                    continue;
                };
                send(&mut tx, &msg).await?;
            }
            // A host probe concluded, was deferred, or its check went not-applicable /
            // forgotten. Applying is pure in-memory work; the result reaches the
            // control plane on the next capacity message, per spec #252.
            Some(update) = probe_updates.recv() => {
                host_probe::orchestrator::apply(&mut mgr.readiness, &mut mgr.codec_evidence, update);
                send_fresh_capacity(&mut tx, &mut *mgr).await?;
            }
        }
    }
}

/// Drain every pending diagnostic trace event so it reaches the control plane BEFORE
/// a terminal `session_state` for the same session (#503).
///
/// The control plane drops an `agent_trace_event` whose session is no longer `running`
/// on this host (`agent-api.md`), so a trace emitted microseconds before the failure
/// that caused it loses the race and is never stored. Flushes the whole lane, not just
/// the caller's session — `audio.degraded` and `session.effective_media` share it.
async fn flush_pending_diagnostics<S>(
    tx: &mut S,
    // `None` when the `select!` arm already disabled itself: then there is nothing
    // pending and this is a no-op.
    diagnostic_rx: &mut Option<mpsc::Receiver<(String, crate::session::runner::TraceEvent)>>,
) -> anyhow::Result<()>
where
    S: SinkExt<Message, Error = tokio_tungstenite::tungstenite::Error> + Unpin,
{
    let Some(rx) = diagnostic_rx else {
        return Ok(());
    };
    while let Ok((session_id, te)) = rx.try_recv() {
        let msg = AgentMsg::SessionTraceEvent {
            session_id,
            ts_unix_ms: te.ts_unix_ms,
            event: te.event.to_string(),
            payload: te.payload,
        };
        send(tx, &msg).await?;
    }
    Ok(())
}

/// Tracks the agent's sessions: those prepared by `session_assign` (awaiting
/// start) and those started (with a stop flag + signaling channel). Owned by
/// the connection loop — no locking. On disconnect the manager drops; the
/// control plane reaps non-terminal sessions to failed (invariant #3).
/// Default grace window: how long running sessions are held after the control
/// plane goes away, before the agent gives up and stops them (#128).
///
/// 90 s covers the ~70 s a `docker compose up -d --force-recreate` of the
/// control plane takes. It must stay BELOW the control plane's own
/// `QUASAR_SESSION_GRACE_SECS` (120 s) minus this agent's maximum reconnect
/// backoff (30 s), or the control plane would terminalise sessions this agent is
/// still holding and about to re-report.
const DEFAULT_SESSION_GRACE_SECS: u64 = 90;

/// Reconnect backoff cap WHILE sessions are being held (#128).
///
/// The ordinary cap is 30 s, which ramps cumulative attempt times to
/// 1, 3, 7, 15, 31, 61, 91 s. A control plane back at ~70 s -- the measured
/// recreate -- would not be contacted until 91 s, one second after the grace
/// window stopped every session it was holding. Polling every 5 s while
/// sessions are at stake closes that gap; the cost is a handful of extra
/// connect attempts against a control plane that is coming back anyway.
const HELD_SESSION_BACKOFF_CAP: Duration = Duration::from_secs(5);

/// `QUASAR_SESSION_GRACE_SECS`, or the default. `0` disables the hold entirely,
/// restoring the pre-#128 behaviour of stopping every session the moment the
/// connection drops.
fn session_grace() -> Duration {
    session_grace_from(std::env::var("QUASAR_SESSION_GRACE_SECS").ok().as_deref())
}

/// Pure core of [`session_grace`]: `raw` is the `QUASAR_SESSION_GRACE_SECS` value as read
/// from env, `None` for unset.
fn session_grace_from(raw: Option<&str>) -> Duration {
    let secs = raw
        .and_then(|v| v.trim().parse::<u64>().ok())
        .unwrap_or(DEFAULT_SESSION_GRACE_SECS);
    Duration::from_secs(secs)
}

/// Session state that OUTLIVES a control-plane connection (#128).
///
/// Before this existed, `SessionManager` was built inside `connect_and_run` and
/// dropped with it, and its `Drop` stopped every session. A control-plane
/// restart therefore ended every stream on the host even though the media path
/// is agent-to-browser and needs nothing from the control plane while it is
/// away.
///
/// The channels move with the map, and that is not incidental: each runner
/// thread captures a CLONE of `evt_tx`/`diagnostic_tx` when it is spawned. Keep
/// the map but rebuild the channels and a survivor's terminal events go into a
/// dead channel, leaving a finished session wedged in `running` until the
/// reconcile sweep notices, reported with the wrong state. The dropped-event
/// counters move for the same reason: the survivors' senders hold clones of
/// those exact Arcs.
struct HostSessions {
    mgr: SessionManager,
    evt_tx: mpsc::Sender<(String, SessionEvent)>,
    /// `Option` for `recv_or_disabled`. The sender now lives here too, so the
    /// "sender gone, disable the arm" path can no longer fire in practice.
    evt_rx: Option<mpsc::Receiver<(String, SessionEvent)>>,
    diagnostic_tx: DiagnosticEventTx,
    diagnostic_rx: Option<mpsc::Receiver<(String, crate::session::runner::TraceEvent)>>,
    diagnostic_dropped_interval: Arc<AtomicU64>,
    diagnostic_dropped_total: Arc<AtomicU64>,
    /// True once a connection has registered. The reconnect ramp restarts from
    /// 1 s after a working connection drops, instead of resuming wherever the
    /// previous outage left it.
    registered_this_connection: bool,
    /// Armed at the FIRST disconnect and aborted only once a connection has
    /// registered. Deliberately not re-armed per reconnect attempt: doing that
    /// reset the window on every retry, so a control plane that never came back
    /// meant the sessions were held forever.
    grace_timer: Option<tokio::task::JoinHandle<()>>,
    /// Host-probe results. Process lifetime, like the orchestrator that feeds
    /// it — a result produced while disconnected is applied when the next
    /// connection's loop runs.
    probe_updates: mpsc::UnboundedReceiver<crate::host_probe::orchestrator::ReportUpdate>,
}

impl HostSessions {
    fn new(
        live_refs: LiveRefs,
        health: Arc<HealthState>,
        nvidia_lib32_probed: String,
        image_mgr: Arc<ImageManager>,
        release_mgr: Arc<ReleaseManager>,
    ) -> Self {
        let (evt_tx, evt_rx) = mpsc::channel::<(String, SessionEvent)>(CRITICAL_EVENT_CAPACITY);
        let (diagnostic_raw_tx, diagnostic_rx) = mpsc::channel(DIAGNOSTIC_EVENT_CAPACITY);
        let diagnostic_dropped_interval = Arc::new(AtomicU64::new(0));
        let diagnostic_dropped_total = Arc::new(AtomicU64::new(0));
        let diagnostic_tx = DiagnosticEventTx::new(
            diagnostic_raw_tx,
            diagnostic_dropped_interval.clone(),
            diagnostic_dropped_total.clone(),
        );
        let probe_runner = Arc::new(crate::host_probe::runner::HostProbeRunner::new());
        let (probe_handle, probe_updates) =
            crate::host_probe::orchestrator::spawn(probe_runner.clone());
        let mut mgr = SessionManager::new(
            live_refs,
            health,
            Vec::new(),
            Vec::new(),
            nvidia_lib32_probed,
            image_mgr,
            release_mgr,
        );
        mgr.probe_handle = Some(probe_handle);
        mgr.probe_runner = Some(probe_runner);
        Self {
            mgr,
            evt_tx,
            evt_rx: Some(evt_rx),
            diagnostic_tx,
            diagnostic_rx: Some(diagnostic_rx),
            diagnostic_dropped_interval,
            diagnostic_dropped_total,
            probe_updates,
            registered_this_connection: false,
            grace_timer: None,
        }
    }

    /// Every running session's stop flag, for the grace-window timer.
    fn stop_flags(&self) -> Vec<Arc<AtomicBool>> {
        self.mgr.running.values().map(|h| h.stop.clone()).collect()
    }

    fn running_count(&self) -> usize {
        self.mgr.running.len()
    }
}

type HomeSourceRetire = Arc<dyn Fn(&str) -> bool + Send + Sync>;

struct SessionManager {
    /// Assigned but not yet started. Aged out by the heartbeat sweep — see
    /// [`PENDING_ASSIGNMENT_TTL`].
    pending: HashMap<String, PendingAssignment>,
    running: HashMap<String, RunningHandle>,
    /// Agent-local runtime knobs, starting at the env baseline and overlaid by
    /// `config_update` pushes. Read when building each session's SessionConfig.
    runtime_settings: crate::session::settings::RuntimeSettings,
    deployment_baseline: crate::session::settings::RuntimeSettings,
    policy_agent: Option<crate::policy::PolicyAgent>,
    policy_accepted_groups: Option<Vec<String>>,
    policy_session_ready: bool,
    policy_delivery_ack: Option<String>,
    policy_inventory_complete: bool,
    policy_seed_reconnect_used: bool,
    /// Latest capacity inventory on this connection. An assignment's `gpu_index` is
    /// resolved against this exact inventory, never treated as an alias for a
    /// host-wide render/CUDA setting.
    gpu_inventory: Vec<crate::messages::GpuCapacity>,
    /// #383: sampling descriptors for `vram::sample`, same index/order as
    /// `gpu_inventory` and re-captured wherever capacity is re-detected, so a GPU-set
    /// change updates both together. Every reassignment site MUST also call
    /// `vram_cache.invalidate()`.
    vram_targets: Vec<VramTarget>,
    /// The live-VRAM sampler's cache + single-flight guard. On the manager so
    /// `send_fresh_capacity` can reach it. MUST be invalidated in lockstep with every
    /// `vram_targets` reassignment: `index` is a position over sorted cardN paths, so
    /// a stale cache misattributes a sample to the wrong physical GPU.
    vram_cache: Arc<VramCache>,
    /// CM-01: the host's console-mode config, latched from `config_update`
    /// (agent-api.md). `None` until one is pushed — the runner then falls back to
    /// `QUASAR_LOCAL_DISPLAY`.
    console_config: Option<crate::messages::ConsoleConfig>,
    /// #175: home refs mounted by live sessions, shared with the GC reaper so it never
    /// reaps a store an active session uses. Updated on start/stop/swap.
    live_refs: LiveRefs,
    /// Shared with the health endpoint: the running-session count.
    health: Arc<HealthState>,
    /// The codec set + throughput hint the host's active encoder path can produce.
    /// Re-probed whenever a `config_update` flips the effective encoder, since that
    /// overlay is live-class. `None` ⇒ `gst::init` failed: no registry to plan from,
    /// so `capacity.codecs` is `["h264"]`. Its flat `codecs` are never `capacity.codecs` (#301).
    host_codec_report: Option<HostCodecReport>,
    /// The process's agent image identity, part of every probe input.
    agent_image_identity: String,
    /// #301 layers 1 and 2, read live from the current inventory at every send.
    codec_layers: CodecLayers,
    /// Which stack each held codec-probe pass was proven under; lives beside `readiness`.
    codec_evidence: crate::host_probe::outcome::CodecEvidence,
    /// What the scheduler was last told, so a refresh re-notifies only on a change.
    notified_probe_inputs: Option<crate::host_probe::decision::ProbeInputs>,
    /// Every capacity message carries `merged()`. Outlives a connection, so retained
    /// checks survive a control-plane restart; registration and the 15 s refresh only
    /// replace the locally computed set.
    readiness: crate::readiness::report::ReadinessReport,
    /// The encoder `host_codec_report` was probed for; compared against
    /// `runtime_settings.encoder` to decide staleness.
    probed_encoder: Option<crate::session::EncoderChoice>,
    /// True once a GPU-global Vulkan fault is detected: new assigns are rejected and
    /// every running session is signalled to stop, then the guarded restart exits for a
    /// clean `gst::init`.
    draining: bool,
    /// The function spawned per session. Production is [`default_runner`]; tests swap
    /// in a panicking or immediately-returning stand-in.
    runner: RunnerFn,
    /// The live-session count the warm-up gate reads. `None` when the golden-home
    /// feature is off, in which case no warm-up path does anything.
    warmup_activity: Option<Arc<crate::session::warmup::HostActivity>>,
    /// The host-global warm-up gate: a `session_assign` raises its abort flag, the
    /// capacity path reads its reservation flag.
    warmup_control: Option<Arc<crate::session::warmup::WarmupControl>>,
    /// `image_ensure`/`image_remove` dispatch target. Process-wide, so its
    /// docker-reconciled state and idempotency bookkeeping outlive sessions.
    image_mgr: Arc<ImageManager>,
    /// `release_apply` dispatch target. Process-wide for the same reason as
    /// `image_mgr`: its poller outlives this connection.
    release_mgr: Arc<ReleaseManager>,
    /// Present only after startup proved prior API-owned source cleanup and
    /// opened the durable session-ID ledger. This is the advertised capability.
    home_cleanup: Option<crate::home_cleanup::HomeCleanupLedger>,
    /// Ack-less qualified terminals emitted after a repeated or never-recorded
    /// session_stop. The control loop drains these after the command ack.
    home_cleanup_reports: Vec<AgentMsg>,
    /// A deterministic runtime seam for cleanup-proof tests. Production uses
    /// the durable application journal adapter when this is absent.
    home_source_retire: Option<HomeSourceRetire>,
    /// Connection-scoped source policy shared with workers and session seeding.
    /// Its authorization is invalidated on disconnect even if sessions retain an Arc.
    source_policy: Option<Arc<crate::source_policy::SourcePolicy>>,
    /// Process-lifetime host-probe orchestrator handle. `None` only in tests
    /// that build a `SessionManager` directly — every send on it is a non-blocking
    /// unbounded-channel push, so nothing on the launch path can be delayed by it.
    probe_handle: Option<crate::host_probe::orchestrator::ProbeHandle>,
    /// The runner the orchestrator drives, so `set_context` can be refreshed before
    /// `registered`/`inputs_observed` without threading it through every call site.
    probe_runner: Option<Arc<crate::host_probe::runner::HostProbeRunner>>,
}

/// The uid/gid an app container's entrypoint drops to
/// (`QUASAR_APP_PUID`/`QUASAR_APP_PGID`), used to own a warm-up's scratch home before
/// the container starts. `None` leaves the home owned by the agent, which is correct
/// for an image that runs as root.
pub(crate) fn app_uid_gid() -> Option<(u32, u32)> {
    let uid = std::env::var("QUASAR_APP_PUID").ok()?.trim().parse().ok()?;
    let gid = std::env::var("QUASAR_APP_PGID")
        .ok()
        .and_then(|v| v.trim().parse().ok())
        .unwrap_or(uid);
    Some((uid, gid))
}

/// A `session_assign` awaiting its `session_start`, stamped so the heartbeat sweep
/// can age out an orphan (see [`PENDING_ASSIGNMENT_TTL`]).
struct PendingAssignment {
    cfg: SessionConfig,
    assigned_at: Instant,
    preparation: Option<crate::runtime::ImageOperation<crate::runtime::ImageInfo>>,
    /// Carried to `RunningHandle` at `session_start`: `LaunchArrived` needs the GPU a
    /// probe might be running on, and a `session_start` cannot re-derive it (the wire
    /// message carries no `gpu_index`).
    gpu_index: i32,
}

/// The per-running-session handles the agent loop holds.
struct RunningHandle {
    stop: Arc<AtomicBool>,
    sig: std::sync::mpsc::Sender<SignalMsg>,
    swap: std::sync::mpsc::Sender<SwapRequest>,
    display: std::sync::mpsc::Sender<DisplayUpdateRequest>,
    /// Admitted capture requests. Paired with [`RunningHandle::capture_slot`]: the
    /// agent loop reserves the slot to decide the ack, the runner releases it.
    capture: std::sync::mpsc::Sender<CaptureRequest>,
    /// The single-flight reservation, held here so `busy` is answered SYNCHRONOUSLY
    /// at ack time rather than discovered by the runner a tick later.
    capture_slot: CaptureSlot,
    /// Launch size, current external and render size, and whether this encode path
    /// can be resized live. Mirrored here so `session_display_update` is validated
    /// synchronously — which is what lets a bad request be acked `ok:false` instead of
    /// accepted and then silently dropped by the runner.
    display_state: crate::session::runner::SessionDisplayState,
    metrics: Arc<SessionMetrics>,
    /// Home refs this session contributed to `live_refs`; removed on teardown.
    home_refs: Vec<String>,
    /// Local-only console sessions are invisible to the session API (no signaling), so
    /// disabling console mode via `config_update` is the only remote lever that stops
    /// them — the ConfigUpdate handler keys off this.
    video_topology: crate::messages::VideoTopology,
    /// Kept solely so the reconciliation sweep can ask `is_finished()`. A runner that
    /// dies without a terminal event otherwise leaks this entry for the connection's
    /// life: dead session_metrics every heartbeat, `remove_live_refs` never runs so
    /// the #175 GC reaper stays blocked on that home, and `/health` over-reports.
    thread: Option<std::thread::JoinHandle<()>>,
    /// When the sweep first saw `thread.is_finished()`. Reaping waits
    /// [`RUNNER_REAP_GRACE`] past this so the ordinary terminal path is never mistaken
    /// for an abandoned slot.
    finished_seen_at: Option<Instant>,
    /// The GPU this session is bound to, for the host-probe scheduler's live-GPU set
    /// and a launch failure's `launch_failed(gpu, ..)`.
    gpu_index: i32,
    /// The assigned codec, before any `QUASAR_CODEC` diagnostic override (the runner
    /// applies that later): a launch failure on it selects that GPU's codec probe.
    codec: crate::session::Codec,
    /// Set by `SessionEvent::Running`. Until then the launch is in flight and no host
    /// probe starts.
    reached_running: bool,
    /// A runner-reported terminal waits here until its thread has exited and
    /// every source generation has been retired and verified absent.
    pending_home_terminal: Option<SessionEvent>,
}

impl SessionManager {
    fn new(
        live_refs: LiveRefs,
        health: Arc<HealthState>,
        gpu_inventory: Vec<crate::messages::GpuCapacity>,
        vram_targets: Vec<VramTarget>,
        nvidia_lib32_probed: String,
        image_mgr: Arc<ImageManager>,
        release_mgr: Arc<ReleaseManager>,
    ) -> Self {
        let mut runtime_settings = crate::session::settings::RuntimeSettings::baseline();
        seed_nvidia_lib32(&mut runtime_settings, &nvidia_lib32_probed);
        let deployment_baseline = runtime_settings.clone();
        SessionManager {
            pending: HashMap::new(),
            running: HashMap::new(),
            runtime_settings,
            deployment_baseline,
            policy_agent: None,
            policy_accepted_groups: None,
            policy_session_ready: true,
            policy_delivery_ack: None,
            policy_inventory_complete: false,
            policy_seed_reconnect_used: false,
            gpu_inventory,
            vram_targets,
            vram_cache: Arc::new(VramCache::new()),
            console_config: None,
            live_refs,
            health,
            host_codec_report: None,
            agent_image_identity: String::new(),
            codec_layers: CodecLayers::default(),
            codec_evidence: Default::default(),
            notified_probe_inputs: None,
            readiness: Default::default(),
            probed_encoder: None,
            draining: false,
            runner: default_runner(),
            warmup_activity: None,
            warmup_control: None,
            image_mgr,
            release_mgr,
            home_cleanup: None,
            home_cleanup_reports: Vec::new(),
            home_source_retire: None,
            source_policy: None,
            probe_handle: None,
            probe_runner: None,
        }
    }

    /// Publish the live-session count to the warm-up gate, and the live-GPU set (every
    /// pending-or-running assignment's GPU — the GPU is spoken for either way) to the
    /// host-probe scheduler. Called from every site that changes `pending` or
    /// `running`, so the two views can never disagree about host busyness.
    fn note_session_count(&self) {
        if let Some(a) = &self.warmup_activity {
            a.set_live(self.running.len(), Instant::now());
        }
        if let Some(handle) = &self.probe_handle {
            let live_gpus: std::collections::BTreeSet<i32> = self
                .pending
                .values()
                .map(|p| p.gpu_index)
                .chain(self.running.values().map(|h| h.gpu_index))
                .collect();
            let launching =
                !self.pending.is_empty() || self.running.values().any(|h| !h.reached_running);
            handle.sessions_changed(live_gpus, launching);
        }
    }

    /// Is a warm-up currently holding an encode slot?
    fn warmup_reserved(&self) -> bool {
        self.warmup_control
            .as_ref()
            .map(|c| c.reserved())
            .unwrap_or(false)
    }

    /// Refresh what a probe run reads. Called before every `registered`/
    /// `inputs_observed` send so a `Start` the orchestrator issues right after always
    /// sees the settings/inventory this exact message describes.
    fn set_probe_context(&self) {
        if let Some(runner) = &self.probe_runner {
            runner.set_context(crate::host_probe::runner::ProbeContext {
                settings: self.runtime_settings.clone(),
                inventory: self.gpu_inventory.clone(),
                warmup: self.warmup_control.clone(),
            });
        }
    }

    /// A probe input changed (capacity re-detection, a `config_update`): refresh the
    /// context and tell the scheduler. A no-op with no probe handle wired (tests).
    fn notify_probe_inputs(&mut self) {
        self.set_probe_context();
        if let Some(handle) = self.probe_handle.clone() {
            let inputs = probe_inputs(self);
            self.notified_probe_inputs = Some(inputs.clone());
            handle.inputs_observed(inputs);
        }
    }

    /// [`Self::notify_probe_inputs`] only when the inputs differ from the last ones sent.
    fn notify_probe_inputs_if_changed(&mut self) {
        if self.notified_probe_inputs.as_ref() != Some(&probe_inputs(self)) {
            self.notify_probe_inputs();
        }
    }

    /// Replace the true inventory after a capacity re-detection.
    fn adopt_inventory(
        &mut self,
        gpus: Vec<crate::messages::GpuCapacity>,
        vram_targets: Vec<VramTarget>,
    ) {
        self.gpu_inventory = gpus;
        self.vram_targets = vram_targets;
        // Must ride every `vram_targets` reassignment — see `vram_cache`'s doc.
        self.vram_cache.invalidate();
    }

    fn codec_stack(&self) -> CodecStack<'_> {
        CodecStack {
            agent_image: &self.agent_image_identity,
            gpus: &self.gpu_inventory,
            settings: &self.runtime_settings,
            registry: self.host_codec_report.is_some(),
            layers: self.codec_layers,
        }
    }

    /// Per-GPU codec sets for the current stack (#301/#302) — the one computation
    /// `advertised_codecs` (the host union) and `apply_gpu_codecs` (the per-GPU wire
    /// field) both derive from.
    fn gpu_codec_sets(&self) -> Vec<(i32, BTreeSet<Codec>)> {
        gpu_codec_sets(&self.codec_stack(), &self.readiness, &self.codec_evidence)
    }

    /// `capacity.codecs` for the current stack (#301). Test-only: a real capacity send
    /// needs `gpu_codec_sets()` anyway (to stamp `apply_gpu_codecs`), so production call
    /// sites derive the union from that same value with `host_codecs_from_sets` instead
    /// of computing it twice through this wrapper.
    #[cfg(test)]
    fn advertised_codecs(&self) -> Option<Vec<String>> {
        Some(host_codecs_from_sets(&self.gpu_codec_sets()))
    }

    /// Built per assign/swap rather than cached: `settings.home_root` is a live-class
    /// setting a `config_update` can move under a long-lived connection, and a stale
    /// root would refuse the managed home it just relocated to.
    fn mount_policy(&self) -> MountPolicy {
        MountPolicy::from_env_with_deployment_mount(
            &self.runtime_settings.home_root,
            &self.deployment_baseline.home_root,
        )
    }

    /// A user launch always wins. Raised on `session_assign`, the earliest point the
    /// agent knows a real session is coming and well before it needs the GPU.
    fn abort_any_warmup(&self) {
        if let Some(c) = &self.warmup_control {
            if c.active() {
                info!("template: a session was assigned; aborting the running warm-up");
            }
            c.abort_for_user_launch();
        }
    }

    /// Begin a GPU-global drain: latch `draining` (rejecting new assigns) and signal
    /// every running session to stop so in-flight work unwinds before the guarded
    /// restart. Idempotent.
    fn begin_drain(&mut self) {
        self.draining = true;
        for (id, h) in self.running.iter() {
            h.stop.store(true, Ordering::Relaxed);
            info!("gpu-global drain: signalling session {id} to stop");
        }
    }

    /// True when the cached report was probed for a different encoder than the current
    /// effective one, so it must be re-probed before the next capacity report.
    fn host_codecs_stale(&self) -> bool {
        self.probed_encoder != Some(self.runtime_settings.encoder)
    }

    fn bind_assignment(&self, gpu_index: i32, cfg: &mut SessionConfig) -> anyhow::Result<()> {
        bind_gpu(&self.gpu_inventory, gpu_index, cfg)
    }

    /// Home refs (volume names / host paths) for a session's container mounts.
    fn home_refs_of(cfg: &SessionConfig) -> Vec<String> {
        cfg.container
            .as_ref()
            .map(|c| {
                c.mounts
                    .iter()
                    .filter_map(|m| gc::ref_of_mount(m))
                    .collect()
            })
            .unwrap_or_default()
    }

    /// Add a started session's home refs to the shared live set (#175).
    fn add_live_refs(&self, refs: &[String]) {
        if refs.is_empty() {
            return;
        }
        if let Ok(mut g) = self.live_refs.lock() {
            for r in refs {
                g.insert(r.clone());
            }
        }
    }

    /// Remove a torn-down session's home refs from the shared live set (#175).
    fn remove_live_refs(&self, refs: &[String]) {
        if refs.is_empty() {
            return;
        }
        if let Ok(mut g) = self.live_refs.lock() {
            for r in refs {
                g.remove(r);
            }
        }
    }

    /// Session ids the agent currently considers live. Reported in heartbeats.
    fn running_ids(&self) -> Vec<String> {
        self.running.keys().cloned().collect()
    }

    /// Drain each running session's telemetry window into a `session_metrics` message
    /// on the heartbeat cadence, plus an `encoder.drop_detected` trace event for any
    /// session with non-zero `frames_dropped` in the window. Glass-to-glass latency is
    /// browser-side (abs-capture-time RTP extension), never a host overlay.
    fn drain_metrics(&self, ts_unix_ms: i64) -> Vec<AgentMsg> {
        let now = std::time::Instant::now();
        let mut out = Vec::with_capacity(self.running.len() * 2);
        for (sid, h) in &self.running {
            let w = h.metrics.drain_window(now);
            if w.frames_dropped > 0 {
                out.push(AgentMsg::SessionTraceEvent {
                    session_id: sid.clone(),
                    ts_unix_ms,
                    event: "encoder.drop_detected".to_string(),
                    payload: serde_json::json!({
                        "frames_dropped": w.frames_dropped,
                        "window_ms": w.window_ms,
                    }),
                });
            }
            out.push(AgentMsg::session_metrics(sid.clone(), ts_unix_ms, &w, None));
        }
        out
    }

    /// Reset the state that belongs to ONE control-plane connection (#128).
    ///
    /// The manager now outlives a connection, so anything scoped to the old one
    /// has to be dropped explicitly rather than by the struct going away.
    /// `pending` is the load-bearing one: an assignment that never got its
    /// `session_start` is owned by a control plane that has since restarted, and
    /// it will re-drive the assign. Everything the caller re-derives per
    /// connection (readiness, codec report, source policy, warm-up handles) is
    /// assigned immediately after this and is not cleared here.
    ///
    /// `running` is deliberately NOT touched. Surviving those is the whole point.
    fn begin_connection(
        &mut self,
        gpu_inventory: Vec<crate::messages::GpuCapacity>,
        vram_targets: Vec<VramTarget>,
    ) {
        let dropped = self.pending.len();
        self.pending.clear();
        self.gpu_inventory = gpu_inventory;
        self.vram_targets = vram_targets;
        // The cache indexes by position over the inventory, so it must be
        // invalidated in lockstep with vram_targets or a sample is attributed to
        // the wrong physical GPU.
        self.vram_cache.invalidate();
        self.draining = false;
        if dropped > 0 {
            info!(
                token = "pending-assignments-dropped",
                "dropped {dropped} pending assignment(s) from the previous connection; \
                 the control plane re-drives them"
            );
        }
        if !self.running.is_empty() {
            info!(
                token = "sessions-survived-reconnect",
                "carried {} running session(s) across the control-plane connection",
                self.running.len()
            );
        }
    }

    /// Signal every running session to stop. Called when the grace window expires
    /// with no control plane, and on a clean agent shutdown.
    ///
    /// Not called on an ordinary disconnect any more (#128): the control plane
    /// reconciles against `heartbeat.running_sessions` on reconnect, so a session
    /// the agent is still running is preserved rather than reaped.
    fn stop_all(&self) {
        for (id, h) in &self.running {
            h.stop.store(true, Ordering::Relaxed);
            info!("signalling session {id} to stop");
        }
    }

    /// Handle a downstream control message; returns an optional reply (an ack)
    /// to send back to the control plane.
    fn handle_control(
        &mut self,
        ctrl: ControlMsg,
        evt_tx: &mpsc::Sender<(String, SessionEvent)>,
        diagnostic_tx: &DiagnosticEventTx,
    ) -> Option<AgentMsg> {
        match ctrl {
            ControlMsg::SessionAssign {
                id,
                session_id,
                gpu_index,
                app,
                stream,
                resources,
                video_topology,
            } => {
                if !self.policy_session_ready {
                    return Some(ack(
                        id,
                        false,
                        Some("settings delivery not yet applied".to_string()),
                    ));
                }
                // A draining agent accepts no new sessions.
                if self.draining {
                    warn!(
                        token = "session-assign-refused-draining",
                        "session {session_id} assignment rejected: agent draining for GPU-global restart"
                    );
                    return Some(ack(
                        id,
                        false,
                        Some("agent draining for restart".to_string()),
                    ));
                }
                if self
                    .home_cleanup
                    .as_ref()
                    .is_some_and(|ledger| ledger.has_record(&session_id))
                {
                    return Some(ack(
                        id,
                        false,
                        Some("session id already recorded".to_string()),
                    ));
                }
                // Raised BEFORE anything else in the assign path: a warm-up's NVENC
                // teardown must never overlap the encoder this session is about to
                // create (#489). The assign→start gap is the abort's budget.
                self.abort_any_warmup();
                // A probe running on this GPU must yield the moment a launch could
                // need it, well before the config below can fail — never delayed by,
                // or delaying, anything else in this arm (an unbounded channel send).
                if let Some(handle) = &self.probe_handle {
                    handle.launch_arrived(gpu_index);
                }
                let container = match app_to_container(app, &self.mount_policy()) {
                    Ok(c) => c,
                    Err(error) => {
                        warn!(
                            token = "session-assign-rejected",
                            "session {session_id} assignment rejected: {error:#}"
                        );
                        // Nothing was inserted into `pending`: tell the probe scheduler
                        // this GPU was never really claimed.
                        self.note_session_count();
                        return Some(ack(id, false, Some(error.to_string())));
                    }
                };
                let params = match stream_to_params(stream) {
                    Ok(p) => p,
                    Err(error) => {
                        warn!(
                            token = "session-assign-rejected",
                            "session {session_id} assignment rejected: {error:#}"
                        );
                        self.note_session_count();
                        return Some(ack(id, false, Some(error.to_string())));
                    }
                };
                let mut cfg = SessionConfig::for_assignment_with(
                    &self.runtime_settings,
                    params,
                    container.clone(),
                );
                if let Err(error) = self.bind_assignment(gpu_index, &mut cfg) {
                    warn!(
                        token = "session-assign-rejected",
                        "session {session_id} assignment rejected: {error:#}"
                    );
                    self.note_session_count();
                    return Some(ack(id, false, Some(error.to_string())));
                }
                // #302 belt: the control plane guarantees `stream.codec` is in the
                // assigned GPU's codec set (agent-api.md amendment 12), but this is the
                // check behind that guarantee — a stale control-plane read of a GPU's set
                // (it can shrink faster than a report reaches the control plane, e.g.
                // right after an agent restart) must refuse here rather than let
                // `pipeline::resolve_effective_encoder` build a pipeline that fails later.
                // The decision (incl. the H.264 exemption) is `assign_codec_refusal`.
                if let Some(refusal) = assign_codec_refusal(
                    &session_id,
                    gpu_index,
                    cfg.stream.codec,
                    &self.gpu_codec_sets(),
                ) {
                    // guarded by assign_refusal_logs_the_codec_not_in_gpu_set_token
                    warn!(token = "assign-codec-not-in-gpu-set", "{}", refusal.log);
                    self.note_session_count();
                    return Some(ack(id, false, Some(refusal.ack_error)));
                }
                cfg.console_config = self.console_config.clone();
                cfg.video_topology = video_topology;
                // The wire carries no image_id on AppSpec, so resolve it from the
                // launch ref via the ImageManager's map. `None` on either side means
                // no seeding for this session, never an assignment failure.
                cfg.source_policy = self.source_policy.clone();
                cfg.image_id = container
                    .as_ref()
                    .and_then(|c| self.image_mgr.image_id_for_ref(&c.image));
                let image = container
                    .as_ref()
                    .map(|c| c.image.clone())
                    .unwrap_or_else(|| "<none>".to_string());
                let (vram, slots) = resources
                    .map(|r| (r.vram_mb, r.encode_slots))
                    .unwrap_or((0, 0));
                info!(
                    "session {session_id} assigned: {}x{}@{}, gpu_index={gpu_index}, \
                     image={image}, reserved vram={vram}MB slots={slots}",
                    cfg.stream.width, cfg.stream.height, cfg.stream.fps
                );
                if let Some(ledger) = self.home_cleanup.as_mut() {
                    match ledger.record_active(&session_id) {
                        Ok(true) => {}
                        Ok(false) => {
                            return Some(ack(
                                id,
                                false,
                                Some("session id already recorded".to_string()),
                            ))
                        }
                        Err(_) => {
                            return Some(ack(
                                id,
                                false,
                                Some("durable session admission unavailable".to_string()),
                            ))
                        }
                    }
                }
                // Preparation belongs to the runtime executor, not this connection.
                // Retain its observation so session_start cannot race or ignore it.
                let preparation = if let Some(spec) = container {
                    match crate::runtime::configured() {
                        Ok(runtime) => {
                            Some(runtime.ensure_image(spec.image, Duration::from_secs(600)))
                        }
                        Err(error) => {
                            self.note_session_count();
                            return Some(ack(id, false, Some(error.to_string())));
                        }
                    }
                } else {
                    None
                };
                self.pending.insert(
                    session_id,
                    PendingAssignment {
                        cfg,
                        assigned_at: Instant::now(),
                        preparation,
                        gpu_index,
                    },
                );
                self.note_session_count();
                Some(ack(id, true, None))
            }
            ControlMsg::SessionStart { id, session_id } => match self.pending.remove(&session_id) {
                Some(PendingAssignment {
                    mut cfg,
                    preparation,
                    gpu_index,
                    ..
                }) => {
                    // The assign already raised this, but a `session_start` for an
                    // assignment that landed on a previous connection would not have.
                    self.abort_any_warmup();
                    if let Some(handle) = &self.probe_handle {
                        handle.launch_arrived(gpu_index);
                    }
                    let stop = Arc::new(AtomicBool::new(false));
                    let (sig_in_tx, sig_in_rx) = std::sync::mpsc::channel::<SignalMsg>();
                    let (swap_tx, swap_rx) = std::sync::mpsc::channel::<SwapRequest>();
                    let (display_tx, display_rx) =
                        std::sync::mpsc::channel::<DisplayUpdateRequest>();
                    let (capture_tx, capture_rx) = std::sync::mpsc::channel::<CaptureRequest>();
                    let capture_slot = CaptureSlot::new();
                    // Shared telemetry: the runner's encode probes write it, the
                    // heartbeat drains it. Seeded with the ABR mode (so every window
                    // reports it rather than deriving it) and the target fps (so the
                    // adaptation classifier scales the per-frame encode budget).
                    let metrics = Arc::new(SessionMetrics::new(
                        cfg.abr_mode.as_str(),
                        cfg.stream.fps.max(1) as u32,
                    ));
                    let home_refs = Self::home_refs_of(&cfg);
                    self.add_live_refs(&home_refs);
                    let video_topology = cfg.video_topology;
                    let codec = cfg.stream.codec;
                    // Snapshotted before `cfg` moves into the runner thread, because the
                    // ack must be produced before the runner has built the encode
                    // pipeline that owns the real `ScaleStage`. Both sides go through
                    // `scale_stage::supports_external_resize`, so they cannot disagree.
                    // A local-only session has no encode pipeline, hence no lever.
                    let display_state = crate::session::runner::SessionDisplayState::new(
                        (cfg.stream.width, cfg.stream.height),
                        cfg.video_topology != crate::messages::VideoTopology::LocalOnly
                            && crate::session::pipeline::external_resize_supported(&cfg),
                    );
                    let stop_handle = stop.clone();
                    let metrics_handle = metrics.clone();
                    let evt_tx2 = evt_tx.clone();
                    let diagnostic_tx2 = diagnostic_tx.clone();
                    // #409: a panicking runner emits no terminal event, so
                    // `drop_running` never runs and the session slot leaks for the life
                    // of the WS connection. Contain it here.
                    let runner = self.runner.clone();
                    let panic_tx = evt_tx.clone();
                    let panic_sid = session_id.clone();
                    let thread = std::thread::spawn(move || {
                        let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(
                            || {
                                if let Some(preparation) = preparation {
                                    if let Err(error) = preparation
                                        .wait_with_cancel(|_| {}, || stop.load(Ordering::Relaxed))
                                    {
                                        let event = if error.kind
                                            == crate::runtime::ErrorKind::Cancelled
                                        {
                                            SessionEvent::Stopped {
                                                bytes_used: None,
                                                detail: None,
                                            }
                                        } else {
                                            SessionEvent::Failed(format!("image preparation failed: {error}; reconcile engine state before retrying"))
                                        };
                                        let _ = evt_tx2.try_send((panic_sid.clone(), event));
                                        return;
                                    }
                                    if let Some(container) = cfg.container.as_mut() {
                                        container.require_local_image = true;
                                    }
                                }
                                if stop.load(Ordering::Relaxed) {
                                    let _ = evt_tx2.try_send((
                                        panic_sid.clone(),
                                        SessionEvent::Stopped {
                                            bytes_used: None,
                                            detail: None,
                                        },
                                    ));
                                    return;
                                }
                                runner(
                                    panic_sid.clone(),
                                    cfg,
                                    evt_tx2,
                                    diagnostic_tx2,
                                    stop,
                                    sig_in_rx,
                                    swap_rx,
                                    display_rx,
                                    capture_rx,
                                    metrics,
                                );
                            },
                        ));
                        if let Err(payload) = outcome {
                            let text = panic_payload_text(payload.as_ref());
                            error!(
                                token = "session-runner-panicked",
                                "session {panic_sid} runner thread panicked: {text}"
                            );
                            // `try_send`, never `blocking_send`: a full lane (or a gone
                            // loop) would park this thread forever, and a thread that
                            // never finishes is what the sweep cannot reap. Dropping
                            // the event is safe — the sweep is the backstop.
                            let event =
                                SessionEvent::Failed(format!("runner thread panicked: {text}"));
                            if panic_tx.try_send((panic_sid.clone(), event)).is_err() {
                                error!(
                                    token = "session-panic-event-undeliverable",
                                    "session {panic_sid}: could not deliver the panic failure \
                                     event; the heartbeat reconciliation sweep will reap the slot"
                                );
                            }
                        }
                    });
                    self.running.insert(
                        session_id.clone(),
                        RunningHandle {
                            stop: stop_handle,
                            sig: sig_in_tx,
                            swap: swap_tx,
                            display: display_tx,
                            capture: capture_tx,
                            capture_slot,
                            display_state,
                            metrics: metrics_handle,
                            home_refs,
                            video_topology,
                            thread: Some(thread),
                            finished_seen_at: None,
                            gpu_index,
                            codec,
                            reached_running: false,
                            pending_home_terminal: None,
                        },
                    );
                    self.health.set_sessions(self.running.len());
                    self.note_session_count();
                    // #419: the per-cycle RSS series. Sampling at start as well as at
                    // teardown brackets the session's own transient allocation, so the
                    // delta between successive `start` samples is the retention.
                    crate::memstat::log_session_boundary("start", &session_id);
                    Some(ack(id, true, None))
                }
                None => {
                    warn!(
                        token = "session-start-unknown",
                        "session_start for unknown session {session_id}"
                    );
                    Some(ack(
                        id,
                        false,
                        Some(format!("no assignment for session {session_id}")),
                    ))
                }
            },
            ControlMsg::SessionStop {
                id,
                session_id,
                reason,
            } => {
                if let Some(ledger) = self.home_cleanup.as_mut() {
                    if let Some(terminal) = ledger.state(&session_id) {
                        self.home_cleanup_reports
                            .push(qualified_home_terminal(&session_id, terminal));
                        return Some(ack(id, true, None));
                    }
                    if !self.running.contains_key(&session_id) {
                        // No runner can still create a source. This includes a
                        // lost assign frame and a never-recorded ID. Persist
                        // retirement before reporting a terminal state.
                        let terminal = match ledger
                            .retire(&session_id, crate::home_cleanup::TerminalKind::Stopped)
                        {
                            Ok(value) => value,
                            Err(_) => {
                                return Some(ack(
                                    id,
                                    false,
                                    Some("durable session retirement unavailable".to_string()),
                                ))
                            }
                        };
                        if self.pending.remove(&session_id).is_some() {
                            self.note_session_count();
                        }
                        self.home_cleanup_reports
                            .push(qualified_home_terminal(&session_id, terminal));
                        return Some(ack(id, true, None));
                    }
                }
                if self.pending.remove(&session_id).is_some() {
                    self.note_session_count();
                }
                if let Some(h) = self.running.get(&session_id) {
                    h.stop.store(true, Ordering::Relaxed);
                    info!("session {session_id} stop requested (reason={reason})");
                }
                Some(ack(id, true, None))
            }
            ControlMsg::SessionSwapApp {
                id,
                session_id,
                app,
            } => {
                if self.home_cleanup.as_ref().is_some_and(|ledger| {
                    ledger.state(&session_id).is_some() || !ledger.has_record(&session_id)
                }) {
                    return Some(ack(id, false, Some("session id is not active".to_string())));
                }
                // A rejected swap is a no-op: ack{ok:false} and the session keeps its
                // previous app. Unlike assign/start, a rejected swap never fails the
                // session (agent-api.md).
                let container = match app_to_container(app, &self.mount_policy()) {
                    Ok(c) => c,
                    Err(error) => {
                        warn!(
                            token = "session-swap-rejected",
                            "session {session_id} swap rejected: {error:#}"
                        );
                        return Some(ack(id, false, Some(error.to_string())));
                    }
                };
                match self.running.get_mut(&session_id) {
                    Some(h) => {
                        // #175: refs accumulate and are only removed at teardown, so a
                        // swapped-away home just waits to become reapable — the reaper
                        // can never take a home this session swapped into.
                        let new_refs: Vec<String> = container
                            .as_ref()
                            .map(|c| {
                                c.mounts
                                    .iter()
                                    .filter_map(|m| gc::ref_of_mount(m))
                                    .collect()
                            })
                            .unwrap_or_default();
                        for r in &new_refs {
                            if !h.home_refs.contains(r) {
                                h.home_refs.push(r.clone());
                            }
                        }
                        match h.swap.send(SwapRequest { container }) {
                            Ok(()) => {
                                if let Ok(mut g) = self.live_refs.lock() {
                                    for r in &new_refs {
                                        g.insert(r.clone());
                                    }
                                }
                                info!("session {session_id} swap accepted");
                                Some(ack(id, true, None))
                            }
                            Err(_) => Some(ack(
                                id,
                                false,
                                Some(format!("session {session_id} runner is gone")),
                            )),
                        }
                    }
                    None => Some(ack(
                        id,
                        false,
                        Some(format!("session {session_id} is not running")),
                    )),
                }
            }
            ControlMsg::SessionDisplayUpdate {
                id,
                session_id,
                render_width,
                render_height,
                ui_scale,
                stream_width,
                stream_height,
            } => {
                // agent-api.md `session_display_update`. Same rejected-is-a-no-op
                // contract as the swap: a rejection acks
                // {ok:false, "display_update_rejected: …"} and never fails the session.
                //
                // Validation is SYNCHRONOUS against the handle's pinned encode size so
                // a bad number gets a real ok:false. LIMITATION: a compositor image
                // predating the render-size properties cannot be detected before the
                // ack — that acks ok:true while nothing changes and the runner warns.
                // The metrics echo is written only when the properties were taken, so
                // `session_metrics` still tells the truth.
                //
                // `stream_*` is both-or-neither, caught before the request is formed.
                let stream = match (stream_width, stream_height) {
                    (Some(w), Some(h)) => Some((w, h)),
                    (None, None) => None,
                    _ => {
                        return Some(ack(
                            id,
                            false,
                            Some(
                                "display_update_rejected: stream_width and stream_height \
                                 must be sent together"
                                    .to_string(),
                            ),
                        ));
                    }
                };
                let req = DisplayUpdateRequest {
                    render_width,
                    render_height,
                    ui_scale,
                    stream,
                };
                match self.running.get_mut(&session_id) {
                    Some(h) => {
                        // Render and external are independent axes, each bounded only by
                        // the launch size — see `validate_display_update`.
                        let eff = match validate_display_update(&req, &h.display_state) {
                            Ok(eff) => eff,
                            Err(why) => {
                                warn!(
                                    token = "display-update-rejected",
                                    "session {session_id} display update rejected: {why}"
                                );
                                return Some(ack(
                                    id,
                                    false,
                                    Some(format!("display_update_rejected: {why}")),
                                ));
                            }
                        };
                        match h.display.send(eff) {
                            Ok(()) => {
                                // Fold forward ONLY on a successful hand-off, so the
                                // next update is validated against the state the runner
                                // is actually about to be in.
                                h.display_state.apply(&eff);
                                info!("session {session_id} display update accepted: {eff:?}");
                                Some(ack(id, true, None))
                            }
                            Err(_) => Some(ack(
                                id,
                                false,
                                Some(format!(
                                    "display_update_rejected: session {session_id} runner is gone"
                                )),
                            )),
                        }
                    }
                    None => Some(ack(
                        id,
                        false,
                        Some(format!(
                            "display_update_rejected: session {session_id} is not running"
                        )),
                    )),
                }
            }
            ControlMsg::SessionCapture {
                id,
                session_id,
                capture_id,
                kind,
                budget,
                params,
            } => {
                // The ack means ARMED, not DONE: `ok:true` says a capture is running and
                // its `diag.<kind>` trace event follows on the reliable lane. So every
                // refusal must be answerable without the runner — `capture::admit`
                // reserves the single-flight slot here, making `busy` a fact about the
                // state the runner will observe rather than a guess.
                //
                // A rejected capture is a pure no-op: it never touches the pipeline and
                // can never fail the session (#270 — the old host-side deep probe could
                // crash a stream; nothing on this surface can).
                let Some(handle) = self.running.get(&session_id) else {
                    warn!(
                        token = "capture-session-not-running",
                        "session_capture for a session that is not running: {session_id}"
                    );
                    return Some(ack(id, false, Some("no_such_session".to_string())));
                };
                // A local-only session has no encode pipeline, so every kind is
                // `unsupported` — decided from the handle's topology, never by asking
                // the runner.
                let has_encode_pipeline = handle.video_topology != VideoTopology::LocalOnly;
                if let Err(why) = capture::admit(&kind, has_encode_pipeline, &handle.capture_slot) {
                    warn!(
                        token = "capture-refused",
                        "session {session_id} capture {capture_id} ({}) refused: {}",
                        kind.as_str(),
                        why.as_str()
                    );
                    return Some(ack(id, false, Some(why.as_str().to_string())));
                }
                let req = CaptureRequest {
                    capture_id: capture_id.clone(),
                    kind,
                    budget,
                    params,
                    slot: handle.capture_slot.clone(),
                };
                let kind_name = req.kind.as_str().to_string();
                match handle.capture.send(req) {
                    Ok(()) => {
                        info!(
                            "session {session_id} capture {capture_id} ({kind_name}) armed \
                             (budget {} bytes / {} ms)",
                            budget.max_bytes, budget.max_ms
                        );
                        Some(ack(id, true, None))
                    }
                    Err(_) => {
                        // The runner went away between the topology read and the send.
                        // Hand the slot back or this session can never be captured again.
                        handle.capture_slot.release();
                        warn!(
                            token = "capture-runner-gone",
                            "session {session_id} capture {capture_id}: runner is gone"
                        );
                        Some(ack(id, false, Some("no_such_session".to_string())))
                    }
                }
            }
            ControlMsg::Signaling { session_id, msg } => {
                if let Some(h) = self.running.get(&session_id) {
                    let raw = serde_json::to_string(&msg).unwrap_or_default();
                    match SignalMsg::from_json(&raw) {
                        Ok(sig) => {
                            if h.sig.send(sig).is_err() {
                                warn!(
                                    token = "signaling-channel-dropped",
                                    "session {session_id}: runner dropped sig channel"
                                );
                            }
                        }
                        Err(e) => warn!(
                            token = "signaling-relay-malformed",
                            "malformed relay signaling for {session_id}: {e}"
                        ),
                    }
                } else {
                    warn!(
                        token = "signaling-relay-unknown-session",
                        "relay signaling for unknown/not-running session {session_id}"
                    );
                }
                None // no ack for signaling relay messages
            }
            ControlMsg::Error { code, message } => {
                error!(
                    token = "control-plane-error",
                    "control plane error: {code}: {message}"
                );
                None
            }
            ControlMsg::ConfigUpdate {
                settings,
                settings_delivery_id,
                console_config,
                source_policies,
            } => {
                let mut feature_error = None;
                if let (Some(policy), Some(snapshot)) =
                    (&self.source_policy, source_policies.as_ref())
                {
                    policy.apply(snapshot);
                }
                // #194: re-derive from the env baseline, then overlay the host's sparse
                // overrides (agent-api.md `config_update` sends only those). An absent
                // key keeps the env value, so a cleared override reverts to env rather
                // than to the catalog default and a host's QUASAR_ENCODER survives.
                // A console-only PATCH sends settings as JSON null; that must NOT
                // rebaseline and silently undo a persisted encoder override.
                if !settings.is_null() {
                    if let Some(policy) = self.policy_agent.as_mut() {
                        let result = policy.apply_legacy_overlay(
                            &self.deployment_baseline,
                            &mut self.runtime_settings,
                            &settings,
                            settings_delivery_id.as_deref(),
                        );
                        match result {
                            Ok(conflict) => {
                                self.policy_delivery_ack = settings_delivery_id;
                                self.policy_session_ready = match &self.policy_accepted_groups {
                                    Some(groups) => {
                                        self.policy_inventory_complete
                                            && policy.sticky_groups_accepted(groups)
                                            && (!groups.is_empty() || !policy.has_seed())
                                    }
                                    None => !policy.has_sticky_ownership(),
                                };
                                if let Some(group) = conflict {
                                    feature_error = Some(AgentMsg::ConfigPolicyFeatureError {
                                        code: "attempt_conflict".into(),
                                        group: Some(group),
                                        connection_incarnation: policy
                                            .connection_incarnation()
                                            .into(),
                                    });
                                }
                            }
                            Err(code) => {
                                self.policy_session_ready = false;
                                feature_error = Some(AgentMsg::ConfigPolicyFeatureError {
                                    code,
                                    group: None,
                                    connection_incarnation: policy.connection_incarnation().into(),
                                });
                            }
                        }
                    } else {
                        let mut next = self.deployment_baseline.clone();
                        next.apply_json(&settings);
                        self.runtime_settings = next;
                    }
                    if let Some(policy) = &self.source_policy {
                        policy.update_root(&self.runtime_settings.home_root);
                    }
                    info!(
                        "runtime settings updated: encoder={:?} gop={} abr_mode={} \
                         target_usage={} home_root={:?}",
                        self.runtime_settings.encoder,
                        self.runtime_settings.gop,
                        self.runtime_settings.abr_mode.as_str(),
                        self.runtime_settings.target_usage,
                        self.runtime_settings.home_root
                    );
                }
                // Latch for the next session build. Absent ⇒ keep the current value.
                if let Some(cc) = console_config {
                    info!(
                        "console config updated: enabled={} connector={} compositor={} stream={} audio_output={:?}",
                        cc.enabled, cc.connector, cc.compositor, cc.stream, cc.audio_output
                    );
                    // #411: the DDC power probe forks `ddcutil` per connected connector
                    // on the 2 s hotplug poll, and nothing consumes a reading unless
                    // console mode is on — latch it so `ddc` can short-circuit.
                    crate::ddc::set_console_enabled(cc.enabled);
                    // The control plane's capacity-report diff is the primary stop path
                    // for a local-only session, but its tracker is in-memory and lost on
                    // a control-plane restart. Stopping them here too means such a
                    // session can never outlive the config that authorized it.
                    if !cc.enabled {
                        for (id, h) in &self.running {
                            if h.video_topology == crate::messages::VideoTopology::LocalOnly {
                                h.stop.store(true, Ordering::Relaxed);
                                info!("console mode disabled: stopping local-only console session {id}");
                            }
                        }
                    }
                    self.console_config = Some(cc);
                }
                feature_error // no ordinary config_update ack
            }
            ControlMsg::ConfigPolicyOffer {
                attempt_id,
                host_id,
                boot_incarnation,
                connection_incarnation,
                group,
                revision,
                content_sha256,
                scope,
                expires_at,
                prerequisites_sha256,
                prerequisites,
                settings,
                resolved_settings,
            } => {
                let offer = crate::policy::Offer {
                    attempt_id,
                    host_id,
                    boot_incarnation,
                    connection_incarnation,
                    group,
                    revision,
                    content_sha256,
                    scope,
                    expires_at,
                    prerequisites_sha256,
                    prerequisites,
                    settings,
                    resolved_settings,
                };
                let reply = self
                    .policy_agent
                    .as_mut()
                    .map(|policy| policy.accept(offer, &mut self.runtime_settings));
                if matches!(&reply, Some(AgentMsg::ConfigPolicyState { phase, .. }) if phase == "applied")
                {
                    // The same launch boundary as the legacy map path: new homes
                    // seed from the new root's templates, and a probe input
                    // (`zerocopy`) withdraws its evidence until re-probed.
                    if let Some(policy) = &self.source_policy {
                        policy.update_root(&self.runtime_settings.home_root);
                    }
                    self.notify_probe_inputs_if_changed();
                }
                reply
            }
            ControlMsg::ConfigPolicyJournalInventoryRequest {
                inventory_id,
                boot_incarnation,
                connection_incarnation,
                cursor,
            } => self.policy_agent.as_mut().map(|policy| {
                match policy.inventory_page(
                    &inventory_id,
                    &boot_incarnation,
                    &connection_incarnation,
                    cursor.as_deref(),
                ) {
                    Ok(page) => {
                        if matches!(
                            &page,
                            AgentMsg::ConfigPolicyJournalInventoryPage {
                                next_cursor: None,
                                ..
                            }
                        ) {
                            self.policy_inventory_complete = true;
                        }
                        page
                    }
                    Err(code) => AgentMsg::ConfigPolicyFeatureError {
                        code,
                        group: None,
                        connection_incarnation,
                    },
                }
            }),
            // `restart` is intercepted in the receive loop (so the ack flushes
            // before the process exits); it never reaches here in practice.
            ControlMsg::Restart { .. } => None,
            // The ImageManager acks immediately and pulls/removes on its own thread.
            ControlMsg::ImageEnsure {
                id,
                image_id,
                registry_ref,
                version,
            } => Some(
                self.image_mgr
                    .handle_ensure(id, image_id, registry_ref, version),
            ),
            ControlMsg::ImageRemove { id, image_id } => {
                Some(self.image_mgr.handle_remove(id, image_id))
            }
            // Same shape as ImageEnsure: acks immediately, then downloads the context
            // and runs `docker build` on its own thread.
            ControlMsg::ImageBuild {
                id,
                image_id,
                context_url,
                context_subdir,
                dockerfile,
                build_args,
                local_tag,
                version,
            } => Some(self.image_mgr.handle_build(
                id,
                image_id,
                context_url,
                context_subdir,
                dockerfile,
                build_args,
                local_tag,
                version,
            )),
            // The updater does the work; the agent validates, acks acceptance and
            // relays. It never recreates itself and runs no compose command.
            ControlMsg::ReleaseApply {
                id,
                request_id,
                release,
                components,
                force,
            } => Some(
                self.release_mgr
                    .handle_apply(id, request_id, release, components, force),
            ),
            ControlMsg::Registered { .. } => {
                warn!(
                    token = "duplicate-registered",
                    "unexpected duplicate 'registered' message"
                );
                None
            }
            ControlMsg::Unknown => None,
        }
    }

    /// #409: heartbeat-tick reconciliation sweep. Two independent leaks, one pass:
    ///
    /// 1. `running` entries whose runner thread is gone — reaped only after
    ///    `runner_grace` since the thread was FIRST seen finished, so the ordinary
    ///    terminal path is never mistaken for one. Each yields a synthetic
    ///    `session_state{failed}` so the control plane converges too.
    /// 2. `pending` assignments never started — see [`PENDING_ASSIGNMENT_TTL`].
    ///
    /// The durations are parameters, not the consts, so tests need not sleep.
    fn reconcile(
        &mut self,
        now: Instant,
        runner_grace: Duration,
        pending_ttl: Duration,
    ) -> Vec<AgentMsg> {
        let mut abandoned: Vec<String> = Vec::new();
        for (sid, h) in self.running.iter_mut() {
            let finished = h.thread.as_ref().map(|t| t.is_finished()).unwrap_or(false);
            if !finished {
                // A thread cannot un-finish, but clearing keeps the field honest.
                h.finished_seen_at = None;
                continue;
            }
            if self.home_cleanup.is_some() && h.pending_home_terminal.is_some() {
                abandoned.push(sid.clone());
                continue;
            }
            match h.finished_seen_at {
                None => h.finished_seen_at = Some(now),
                Some(seen) => {
                    if now.duration_since(seen) >= runner_grace {
                        abandoned.push(sid.clone());
                    }
                }
            }
        }
        let mut out = Vec::with_capacity(abandoned.len());
        for sid in abandoned {
            if self.home_cleanup.is_some() {
                let event = self
                    .running
                    .get_mut(&sid)
                    .and_then(|h| h.pending_home_terminal.take())
                    .unwrap_or_else(|| {
                        SessionEvent::Failed(
                            "runner thread ended without reporting a terminal state".to_string(),
                        )
                    });
                if let Some(message) = self.prove_home_terminal(&sid, event) {
                    out.push(message);
                }
                continue;
            }
            error!(
                token = "session-runner-no-terminal-event",
                "session {sid}: runner thread ended without a terminal event \
                 (panic or abandoned runner); reaping the session slot"
            );
            if let Some(mut h) = self.running.remove(&sid) {
                self.remove_live_refs(&h.home_refs);
                if let Some(t) = h.thread.take() {
                    let _ = t.join();
                }
            }
            out.push(AgentMsg::SessionState {
                session_id: sid,
                state: "failed".to_string(),
                detail: None,
                error: Some("runner thread ended without reporting a terminal state".to_string()),
                reason_code: None,
                app_log_tail: None,
            });
        }
        self.health.set_sessions(self.running.len());
        self.note_session_count();

        let stale: Vec<String> = self
            .pending
            .iter()
            .filter(|(_, p)| now.duration_since(p.assigned_at) >= pending_ttl)
            .map(|(sid, _)| sid.clone())
            .collect();
        if !stale.is_empty() {
            for sid in &stale {
                if let Some(ledger) = self.home_cleanup.as_mut() {
                    if ledger
                        .retire(sid, crate::home_cleanup::TerminalKind::Failed)
                        .is_err()
                    {
                        warn!(token = "home-pending-retirement-unavailable",
                            "stale assignment retirement could not be persisted; terminal proof withheld");
                        continue;
                    }
                    out.push(qualified_home_terminal(
                        sid,
                        crate::home_cleanup::TerminalKind::Failed,
                    ));
                }
                warn!(
                    token = "session-assign-never-started",
                    "session {sid}: assignment never started within {pending_ttl:?}; \
                     dropping the orphaned pending config"
                );
                self.pending.remove(sid);
            }
            self.note_session_count();
        }
        out
    }

    /// Remove a terminal session from `running` and free its home refs from the
    /// shared live set (#175), so the GC reaper may reap a now-tombstoned home.
    fn drop_running(&mut self, session_id: &str) {
        if let Some(h) = self.running.remove(session_id) {
            self.remove_live_refs(&h.home_refs);
            self.health.set_sessions(self.running.len());
            self.note_session_count();
            // The teardown-side RSS sample, plus the `QUASAR_MALLOC_TRIM` discriminator:
            // if trimming flattens the per-cycle slope the residual is reclaimable free
            // heap, otherwise the memory is genuinely still reachable.
            crate::memstat::on_session_teardown(session_id);
        }
    }

    /// A cleanup-capable terminal waits for its runner to finish, then uses
    /// the runtime's durable operation journals to stop/remove every source
    /// generation and verify absence. On any uncertainty the terminal stays
    /// pending and home refs remain live for a later retry.
    fn prove_home_terminal(&mut self, session_id: &str, event: SessionEvent) -> Option<AgentMsg> {
        if self.home_cleanup.is_none() {
            return Some(self.on_event(session_id, event));
        }
        let handle = self.running.get_mut(session_id)?;
        if !handle
            .thread
            .as_ref()
            .is_some_and(|thread| thread.is_finished())
        {
            handle.pending_home_terminal = Some(event);
            return None;
        }
        let cleaned = self.home_source_retire.as_ref().map_or_else(
            || {
                crate::runtime::configured()
                    .and_then(|runtime| runtime.retire_session_applications(session_id).wait())
                    .is_ok()
            },
            |retire| retire(session_id),
        );
        if !cleaned {
            handle.pending_home_terminal = Some(event);
            warn!(
                token = "home-terminal-cleanup-unverified",
                "session source cleanup could not be verified; terminal proof withheld"
            );
            return None;
        }
        let terminal = match &event {
            SessionEvent::Stopped { .. } => crate::home_cleanup::TerminalKind::Stopped,
            SessionEvent::Failed(_) | SessionEvent::AppFailed { .. } => {
                crate::home_cleanup::TerminalKind::Failed
            }
            _ => return None,
        };
        if self
            .home_cleanup
            .as_mut()
            .unwrap()
            .retire(session_id, terminal)
            .is_err()
        {
            handle.pending_home_terminal = Some(event);
            warn!(
                token = "home-terminal-retirement-unavailable",
                "session identity retirement could not be persisted; terminal proof withheld"
            );
            return None;
        }
        if let Some(thread) = handle.thread.take() {
            let _ = thread.join();
        }
        Some(self.on_event(session_id, event))
    }

    /// Map a runner lifecycle event onto a session_state message.
    fn on_event(&mut self, session_id: &str, event: SessionEvent) -> AgentMsg {
        // Handled ahead of the generic mapping so `reason_code`/`app_log_tail` need not
        // be threaded through every other arm as `None`.
        if let SessionEvent::AppFailed {
            reason,
            reason_code,
            app_log_tail,
        } = event
        {
            self.drop_running(session_id);
            return AgentMsg::SessionState {
                session_id: session_id.to_string(),
                state: "failed".to_string(),
                detail: None,
                error: Some(reason),
                reason_code: Some(reason_code.to_string()),
                // Omitted entirely when empty: an empty array renders as an empty
                // log panel that reads as a broken feature rather than a silent app.
                app_log_tail: (!app_log_tail.is_empty()).then(|| app_log_tail.join("\n")),
            };
        }
        let (state, detail, error) = match event {
            SessionEvent::Starting => ("starting", Some("building pipeline".to_string()), None),
            SessionEvent::Progress(detail) => ("starting", Some(detail.to_string()), None),
            SessionEvent::Running => {
                if let Some(handle) = self.running.get_mut(session_id) {
                    handle.reached_running = true;
                }
                self.note_session_count();
                (
                    "running",
                    Some("pipeline live; offer ready".to_string()),
                    None,
                )
            }
            SessionEvent::Stopping => ("stopping", Some("tearing down".to_string()), None),
            // A clean stop never carries an `error_message`. `detail` carries a reason
            // on a peer disconnect, recorded as `state_detail`, so operators see why it
            // ended without the row being classified `failed`.
            SessionEvent::Stopped { detail, .. } => {
                self.drop_running(session_id);
                ("stopped", detail.map(str::to_string), None)
            }
            SessionEvent::Failed(e) => {
                self.drop_running(session_id);
                ("failed", None, Some(e))
            }
            // Top-level state stays `running` throughout a swap; the detail carries
            // progress. The control plane maps these onto state_detail and commits the
            // new app_id on SwapDone (agent-api.md).
            SessionEvent::Swapping => ("running", Some("swapping".to_string()), None),
            SessionEvent::SwapDone => ("running", Some("swap complete".to_string()), None),
            SessionEvent::SwapRolledBack(reason) => (
                "running",
                Some(format!("swap failed; rolled back: {reason}")),
                None,
            ),
            // Same trick as the swap details: the transport IS live, so only `detail`
            // moves. These two strings are the client's loading-screen contract — hold
            // the loader through "app booting", reveal on "app presented".
            SessionEvent::AppBooting => ("running", Some("app booting".to_string()), None),
            SessionEvent::AppPresented => ("running", Some("app presented".to_string()), None),
            SessionEvent::Signaling(_) => unreachable!("Signaling handled in event loop"),
            SessionEvent::EffectiveMedia(_) => {
                unreachable!("EffectiveMedia handled in event loop")
            }
            SessionEvent::Capture { .. } | SessionEvent::Trace { .. } => {
                unreachable!("reliable-lane trace events are handled in the event loop")
            }
            SessionEvent::AppFailed { .. } => unreachable!("handled above"),
        };
        AgentMsg::SessionState {
            session_id: session_id.to_string(),
            state: state.to_string(),
            detail,
            error,
            reason_code: None,
            app_log_tail: None,
        }
    }
}

// No `impl Drop for SessionManager`. It used to stop every session, on the
// reasoning that the manager dropped exactly when `connect_and_run` returned so
// this covered every exit path. That is precisely why a control-plane restart
// ended every stream on the host (#128). The manager now outlives a connection,
// and `run()` decides when to give up: sessions are held for a bounded grace
// window and stopped only if the control plane does not come back within it.

/// `RuntimeSettings::effective_map()` keys that select the media path a host probe
/// exercises. Anything containing `vulkan` or `cuda` is included too (the Vulkan
/// per-codec knobs are process env, not in this map, so today that adds only
/// `cuda_device`; a future settings key needs no change here).
const PROBE_SETTINGS_KEYS: &[&str] = &["encoder", "render_node", "zerocopy"];

/// The media-relevant settings, as one comparable string (`host_probe::decision`'s
/// `ProbeInputs::settings`): an unrelated setting (e.g. `home_root`) must not cause a
/// probe re-run.
fn probe_relevant_settings(map: &std::collections::BTreeMap<String, String>) -> String {
    map.iter()
        .filter(|(k, _)| {
            PROBE_SETTINGS_KEYS.contains(&k.as_str()) || k.contains("vulkan") || k.contains("cuda")
        })
        .map(|(k, v)| format!("{k}={v}"))
        .collect::<Vec<_>>()
        .join(",")
}

/// What decides whether a host probe's earlier result still applies.
fn probe_inputs(mgr: &SessionManager) -> crate::host_probe::decision::ProbeInputs {
    mgr.codec_stack().probe_inputs()
}

/// Turn the assign's `AppSpec` into a launchable container spec, or `None` when
/// no image is set (a bare/compositor-only session).
///
/// The single point where wire-supplied mounts become agent data, so it is where
/// [`MountPolicy`] runs. Mounts the agent itself appends later (the Wayland socket,
/// the pulse runtime dir) are agent-authored and deliberately not re-checked.
/// A rejection fails the assign or the swap; nothing is spawned.
fn app_to_container(app: AppSpec, mounts: &MountPolicy) -> anyhow::Result<Option<ContainerSpec>> {
    if app.image.is_empty() {
        return Ok(None);
    }
    Ok(Some(ContainerSpec {
        image: app.image,
        args: app.args,
        env: app.env,
        mounts: mounts.check_all(&app.mounts)?,
        gpu: app.gpu,
        no_new_privileges: app.no_new_privileges,
        on_app_exit: app.on_app_exit,
        network: app.network,
        systempaths_unconfined: app.systempaths_unconfined,
        require_local_image: false,
    }))
}

/// Convert the wire `StreamSpec` into agent `StreamParams`, storing the assigned codec
/// verbatim — `QUASAR_CODEC` is applied later at session build so the assigned value
/// survives for the effective-media snapshot. An unrecognised wire codec is a hard
/// error: the assignment must be rejected rather than silently streaming H.264 while
/// `sessions.codec` claims otherwise.
fn stream_to_params(s: StreamSpec) -> anyhow::Result<StreamParams> {
    let codec = match s.codec.as_deref() {
        Some(c) => crate::session::Codec::parse(c)?,
        None => crate::session::Codec::H264,
    };
    Ok(StreamParams {
        width: s.width,
        height: s.height,
        fps: s.fps,
        bitrate_kbps: s.bitrate_kbps,
        h264_profile: s.h264_profile,
        codec,
        abr_floor_kbps: s.abr_floor_kbps,
        mic: s.mic,
    })
}

fn ack(id: String, ok: bool, error: Option<String>) -> AgentMsg {
    AgentMsg::Ack { id, ok, error }
}

fn qualified_home_terminal(
    session_id: &str,
    terminal: crate::home_cleanup::TerminalKind,
) -> AgentMsg {
    AgentMsg::SessionState {
        session_id: session_id.to_owned(),
        state: terminal.as_str().to_owned(),
        detail: None,
        error: None,
        reason_code: None,
        app_log_tail: None,
    }
}

/// Run startup work which is only safe after API-owned applications have retired.
/// A failed retirement blocks this process before it can sweep legacy containers or
/// register with the control plane, so a supervisor retries without releasing a home.
pub(crate) fn post_application_retirement<F, G>(
    applications_retired: bool,
    retire_audio: F,
    sweep_legacy: G,
) -> Option<usize>
where
    F: FnOnce(),
    G: FnOnce() -> usize,
{
    if !applications_retired {
        return None;
    }
    retire_audio();
    Some(sweep_legacy())
}

/// The boot-only legacy sweep: remove this agent's own pre-API `quasar-sess-*`
/// siblings through the runtime API, and report how many were removed. Foreign,
/// unlabelled and API-owned containers are preserved and only counted — they are
/// somebody else's to reap. A listing failure is logged and the boot continues,
/// exactly as the CLI sweep's `ps` failure did: a legacy container left behind is
/// retried next boot, while refusing to start would strand the host.
pub(crate) fn legacy_container_sweep(api: &crate::runtime::RuntimeClient) -> usize {
    match api
        .retire_legacy_containers(vec![
            crate::session::container::SESSION_NAME_PREFIX.to_owned()
        ])
        .wait()
    {
        Ok(outcome) => {
            if outcome.preserved > 0 || outcome.unresolved > 0 {
                info!(
                    token = "legacy-container-sweep-summary",
                    removed = outcome.removed,
                    preserved = outcome.preserved,
                    unresolved = outcome.unresolved,
                    "startup legacy sweep finished; preserved containers this agent cannot \
                     prove it owns were left for operator review"
                );
            }
            outcome.removed
        }
        Err(error) => {
            warn!(
                token = "legacy-container-list-failed",
                %error,
                "the startup legacy container listing failed; pre-API containers from an \
                 older agent may remain and will be retried on the next boot"
            );
            0
        }
    }
}

/// Aborts the process-lifetime application cleanup maintenance task on orderly shutdown.
struct ApplicationCleanupGuard(tokio::task::JoinHandle<()>);

impl Drop for ApplicationCleanupGuard {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// One maintenance pass over every durable journal an interrupted teardown can leave
/// behind: the caller's pending map, then the application, audio and diagnostic
/// journals. Caller obligations run first because runtime recovery correctly skips
/// `Running` records; a caller-map error never starves durable journal cleanup.
///
/// Each journal is recovered INDEPENDENTLY and its failure is aggregated, never
/// propagated early: a wedged application record must not leave an audio sidecar
/// running until the next agent restart, which is exactly the shape of the defect
/// this pass exists to close. `audio` is the ROUTINE variant, which never stops a
/// live sidecar — `runtime::helper_tests::ordinary_audio_recovery_preserves_live_work_but_boot_retirement_stops_it`.
fn application_cleanup_maintenance_tick<F, G, H, I>(
    pending: F,
    journal: G,
    audio: H,
    diagnostics: I,
) -> Result<(), crate::runtime::RuntimeError>
where
    F: FnOnce() -> anyhow::Result<()>,
    G: FnOnce() -> Result<(), crate::runtime::RuntimeError>,
    H: FnOnce() -> Result<(), crate::runtime::RuntimeError>,
    I: FnOnce() -> Result<(), crate::runtime::RuntimeError>,
{
    if let Err(error) = pending() {
        tracing::warn!(
            token = "application-pending-map-unavailable",
            "caller application pending map could not be scanned: {error}"
        );
    }
    let mut failure = journal().err();
    if let Err(error) = audio() {
        tracing::warn!(
            token = "runtime-audio-cleanup-maintenance-pending",
            "audio sidecar cleanup maintenance remains pending: {error}"
        );
        failure = failure.or(Some(error));
    }
    if let Err(error) = diagnostics() {
        tracing::warn!(
            token = "runtime-diagnostic-cleanup-maintenance-pending",
            "diagnostic helper cleanup maintenance remains pending: {error}"
        );
        failure = failure.or(Some(error));
    }
    failure.map_or(Ok(()), Err)
}

/// Retry every durable cleanup obligation a teardown can leave unproven: application,
/// audio-sidecar and diagnostic-helper journals. A live workload has no cleanup intent
/// and is therefore invisible to this pass; boot retirement, not this, ends prior work.
fn spawn_application_cleanup_recovery() -> ApplicationCleanupGuard {
    let handle = tokio::spawn(async move {
        // Startup already performed a bounded pass. Delay the first maintenance retry
        // so it cannot immediately duplicate that boot work, then keep the steady cadence.
        sleep(Duration::from_secs(30)).await;
        let mut ticker = tokio::time::interval(Duration::from_secs(30));
        ticker.tick().await; // discard interval's immediate tick
        loop {
            let outcome = tokio::task::spawn_blocking(|| {
                application_cleanup_maintenance_tick(
                    || {
                        crate::session::container::recover_pending_application_operations(
                            |operation| {
                                crate::runtime::configured()
                                    .and_then(|api| {
                                        api.abandon_application(operation.to_owned()).wait()
                                    })
                                    .map_err(|error| anyhow::anyhow!(error))
                            },
                        )
                    },
                    || {
                        crate::runtime::configured()
                            .and_then(|api| api.recover_application_cleanup().wait())
                    },
                    || {
                        crate::runtime::configured()
                            .and_then(|api| api.recover_audio_sidecars().wait())
                    },
                    || {
                        crate::runtime::configured()
                            .and_then(|api| api.recover_diagnostics().wait())
                    },
                )
            })
            .await;
            match outcome {
                Ok(Ok(())) => {}
                Ok(Err(error)) => warn!(
                    token = "runtime-application-cleanup-maintenance-pending",
                    "application cleanup maintenance remains pending: {error}"
                ),
                Err(error) => warn!(
                    token = "runtime-application-cleanup-maintenance-join",
                    "application cleanup maintenance task failed: {error}"
                ),
            }
            ticker.tick().await;
        }
    });
    ApplicationCleanupGuard(handle)
}

/// Aborts the library-scan task when this connection ends: a stale scanner must never
/// outlive its node_secret.
struct LibraryScanGuard(tokio::task::JoinHandle<()>);

impl Drop for LibraryScanGuard {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// Spawn the Steam library discovery scanner: one pass 30 s after registration (so it
/// does not contend with the post-reconnect burst), then every 60 s. Each pass runs on
/// a blocking thread and swallows its own errors — a scan failure must never be fatal.
/// The node-secret HTTP client for this connection's pull channels: same transport policy
/// as the websocket (#12), so a pinned host never has one client accept the control plane
/// while the other refuses it.
fn cp_client(cfg: &Config, node_secret: String) -> Result<crate::cp_http::CpClient, String> {
    crate::cp_http::CpClient::new(
        &cfg.transport,
        cfg.http_base_url(),
        cfg.node_name.clone(),
        node_secret,
    )
}

fn spawn_library_scanner(cp: crate::cp_http::CpClient) -> LibraryScanGuard {
    let handle = tokio::spawn(async move {
        sleep(Duration::from_secs(30)).await;
        // Poll cadence is NOT scan cadence. This ticker only asks whether a scan is
        // queued for this host; the control plane's janitor decides how often a home is
        // walked (QUASAR_LIBRARY_SCAN_INTERVAL). Matching the two meant a queued scan
        // could sit unclaimed for another full interval, so this is a cheap indexed
        // query at 60 s while the filesystem walk stays paced by the janitor.
        let mut ticker = tokio::time::interval(Duration::from_secs(60));
        ticker.tick().await; // discard the immediate first tick
        loop {
            let client = LibraryScanClient::new(cp.clone());
            if let Err(e) = tokio::task::spawn_blocking(move || client.run_pass()).await {
                warn!(
                    token = "library-scan-join-error",
                    "library-scan: scanner task join error: {e}"
                );
            }
            ticker.tick().await;
        }
    });
    LibraryScanGuard(handle)
}

/// The host's node_secret for the pull channels' HTTP auth, read from the file written
/// at enrollment. `None` when the file is missing or empty.
fn current_node_secret(cfg: &Config) -> Option<String> {
    std::fs::read_to_string(&cfg.node_secret_path)
        .ok()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
}

/// #519: is there any path left for this process to register — a persisted
/// `node_secret` or an `ENROLLMENT_TOKEN`? With neither, retrying is pointless:
/// nothing later can conjure a token that was never in the environment.
fn enrollment_reachable(cfg: &Config) -> Result<(), String> {
    if current_node_secret(cfg).is_some() {
        return Ok(());
    }
    match &cfg.enrollment_token {
        Some(t) if !t.trim().is_empty() => Ok(()),
        _ => Err(format!(
            "no persisted node_secret at {} and neither QUASAR_ENROLLMENT nor ENROLLMENT_TOKEN \
             is set: this agent can never register as-is. Paste the enrollment string from \
             Admin -> Fleet -> Enroll host into QUASAR_ENROLLMENT (or set ENROLLMENT_TOKEN; see \
             docs/configuration.md#enrollment_token), then restart the container.",
            cfg.node_secret_path
        )),
    }
}

/// The configured enrollment token, whitespace-only folded to `None` — the same
/// view [`enrollment_reachable`] takes, so "can register" and "what to present"
/// cannot disagree.
fn configured_enrollment_token(cfg: &Config) -> Option<&str> {
    cfg.enrollment_token
        .as_deref()
        .filter(|t| !t.trim().is_empty())
}

/// Pick the credential for one `register`.
///
/// A saved node secret normally wins: it is the steady-state credential, and
/// enrolling again rotates it (and is refused outright against a live agent, #96).
/// `prefer_enrollment_token` is the single #199 exception — see [`stale_identity`].
/// It is a *preference*, never a way to make a registerable agent unregisterable:
/// with no token configured the saved secret is still used.
fn choose_auth(cfg: &Config, prefer_enrollment_token: bool) -> anyhow::Result<Auth> {
    let token = configured_enrollment_token(cfg).map(str::to_string);
    if prefer_enrollment_token {
        if let Some(enrollment_token) = token.clone() {
            return Ok(Auth::Enrollment { enrollment_token });
        }
    }
    if let Ok(secret) = std::fs::read_to_string(&cfg.node_secret_path) {
        let secret = secret.trim().to_string();
        if !secret.is_empty() {
            return Ok(Auth::Reconnect {
                node_secret: secret,
            });
        }
    }
    match token {
        Some(enrollment_token) => Ok(Auth::Enrollment { enrollment_token }),
        None => anyhow::bail!(
            "no node_secret at {} and ENROLLMENT_TOKEN not set; cannot register",
            cfg.node_secret_path
        ),
    }
}

/// Log a refused `register` and produce the error `connect_and_run` returns.
///
/// A function rather than three `bail!`s inline so the #199 loop can be tested without
/// a socket: this is the same call the live path makes, and its result is what
/// [`EnrollmentFallback::observe`] reads.
fn register_reject_error(
    cfg: &Config,
    code: &str,
    message: &str,
    presented_saved_secret: bool,
) -> anyhow::Error {
    // #199: `host_not_found` for a register carrying a SAVED secret is the one reject
    // the agent can act on itself. The control plane's remedy ("enroll with an
    // enrollment token") is exactly what the operator already did — the token just
    // never gets presented while a secret exists on disk.
    match stale_identity(
        code,
        presented_saved_secret,
        configured_enrollment_token(cfg).is_some(),
    ) {
        Some(StaleIdentity::ReEnroll) => {
            let detail = stale_identity_message(&cfg.node_secret_path, StaleIdentity::ReEnroll);
            warn!(token = "cp-register-stale-identity", "{detail}");
            anyhow::Error::new(StaleNodeSecret(format!(
                "control plane rejected register: {code}: {message} — {detail}"
            )))
        }
        Some(StaleIdentity::Unresolvable) => {
            let detail = stale_identity_message(&cfg.node_secret_path, StaleIdentity::Unresolvable);
            error!(
                token = "cp-register-stale-identity-unresolvable",
                "{detail}"
            );
            anyhow::anyhow!("control plane rejected register: {code}: {message} — {detail}")
        }
        None => anyhow::anyhow!("control plane rejected register: {code}: {message}"),
    }
}

/// The one piece of credential state the reconnect loop carries between attempts (#199).
///
/// Armed by a stale-secret reject, consumed by the very next attempt. The consumption is
/// the whole point: a latch would keep presenting the enrollment token forever, and a
/// single-use token that has already been spent would then leave a host that a returning
/// control plane could still have re-admitted on its saved secret with nothing to offer
/// it. Alternating costs one extra attempt on the backoff ramp and gives both
/// credentials a turn.
#[derive(Default)]
struct EnrollmentFallback {
    armed: bool,
}

impl EnrollmentFallback {
    /// Hand the arming to the attempt about to be made, and disarm. The attempt AFTER
    /// this one goes back to the saved secret unless another reject arms it again.
    fn take_for_attempt(&mut self) -> bool {
        std::mem::take(&mut self.armed)
    }

    /// Arm iff this failure was the stale-secret reject. Every other failure — a dead
    /// socket, a bad token, a TLS pin mismatch — leaves the preference where it is.
    fn observe(&mut self, err: &anyhow::Error) {
        if err.downcast_ref::<StaleNodeSecret>().is_some() {
            self.armed = true;
        }
    }
}

/// The control plane's answer to a reconnect naming a host it has never heard of.
/// Not an enum value in `agent-api.md` — matched as the string the handler writes.
const HOST_NOT_FOUND: &str = "host_not_found";

/// What a `host_not_found` reject means for the credential this agent holds (#199).
///
/// The reject is *correct* whenever an agent data volume outlives the enrollment
/// that filled it: the secret inside was minted by a DIFFERENT control plane (or
/// this host row was deleted), so the new one has never seen the node. What the
/// control plane cannot know is that the operator already did what its message
/// asks — an enrollment token is sitting right there in the environment, losing
/// to the saved secret on every attempt.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum StaleIdentity {
    /// A token is configured: present it on the next attempt instead. Enrollment
    /// mints a fresh identity here and overwrites the saved secret, so the
    /// following reconnect is ordinary.
    ReEnroll,
    /// No token: nothing this process can do makes the saved secret valid here.
    Unresolvable,
}

/// Classify a rejected `register`. `None` for anything that is not "the saved
/// secret is unknown here" — notably a `host_not_found` answering a register that
/// already carried the enrollment token, where re-sending it would be a loop
/// rather than a recovery.
fn stale_identity(
    code: &str,
    presented_saved_secret: bool,
    has_enrollment_token: bool,
) -> Option<StaleIdentity> {
    if code != HOST_NOT_FOUND || !presented_saved_secret {
        return None;
    }
    Some(if has_enrollment_token {
        StaleIdentity::ReEnroll
    } else {
        StaleIdentity::Unresolvable
    })
}

/// The operator-facing line for each case. The control plane's own message cannot
/// carry this: only the agent knows where its secret is kept.
fn stale_identity_message(node_secret_path: &str, kind: StaleIdentity) -> String {
    let cause = format!(
        "the node secret saved at {node_secret_path} identifies no host on this control plane. \
         That is what a saved identity from a DIFFERENT control plane looks like — an agent data \
         volume that outlived an earlier enrollment — or a host row that was deleted here"
    );
    match kind {
        // What is about to happen, not how it will turn out. The enrollment token is
        // single-use and expiring: a spent one is refused too, and a line promising the
        // operator that nothing is needed would then repeat next to every failure.
        StaleIdentity::ReEnroll => format!(
            "{cause}. Presenting the configured enrollment token on the next attempt instead of \
             the saved secret. If the control plane accepts it this host gets a fresh identity \
             there and {node_secret_path} is replaced; if the token has already been used or has \
             expired, that attempt is refused too and a fresh enrollment string is needed."
        ),
        // The volume is NOT named: it is the compose project that decides its name, the
        // agent cannot see one, and an agent that is not in a container has none.
        StaleIdentity::Unresolvable => format!(
            "{cause}, and no enrollment token is configured — every reconnect will be refused the \
             same way. Clear the saved identity and enroll again: the command from \
             Admin -> Fleet -> Enroll host does the clearing with QUASAR_RESET_IDENTITY=1, or \
             stop this agent and delete {node_secret_path} yourself (in a container install that \
             file is inside the agent's data volume, so removing that volume is the same thing)."
        ),
    }
}

/// `connect_and_run`'s error when a register was refused as [`StaleIdentity::ReEnroll`].
/// Typed rather than a string so the run loop can act on it without matching prose:
/// it forces exactly the NEXT attempt to present the enrollment token.
#[derive(Debug)]
struct StaleNodeSecret(String);

impl std::fmt::Display for StaleNodeSecret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for StaleNodeSecret {}

/// What the agent has to say about an HTTP status the control plane answered the
/// WebSocket upgrade with, before any `register` was sent (#199 follow-up).
///
/// `429` is the one status the agent can explain better than the transport can: it
/// is the control plane's enrollment-failure limiter, and by construction the agent
/// has just watched its own registers be refused ten times in a minute. Logged bare
/// — `HTTP error: 429 Too Many Requests` — it reads as a second, unrelated fault,
/// which is exactly how it was reported. Everything else gets `None` and stays an
/// ordinary connection failure: inventing prose for a status the agent has no
/// insight into is how a log line starts lying.
///
/// The *accounting* deliberately did not change. Not counting the `host_not_found`
/// rejects that trip the limiter would make `/agent/ws` a rate-unbounded node-name
/// oracle (an unknown name answers `host_not_found`, a known one `auth_failed`, so
/// misses would be free), so the limiter keeps counting them and this is presentation.
fn describe_upgrade_refusal(status: u16) -> Option<&'static str> {
    match status {
        429 => Some(
            "the control plane is rate-limiting this address. Usually that is its \
             enrollment-failure limiter, tripped by the refused registers above: ten refused \
             registers with no minute's gap between them, lifting a minute after the LAST \
             refusal — so a run of refusals on a backoff that never idles a full minute trips it \
             however long it takes. It also answers 429 when more than ten handshakes from this \
             address are in flight at once, which is what a fleet of agents behind one NAT can do \
             on a simultaneous reconnect; in that case there will be no refusals above. Either \
             way this is a consequence of something else, not a separate fault: the agent keeps \
             retrying on its backoff and is admitted again once the window passes. Act on what \
             the refusals said — if a line above reports the saved identity is unresolvable here, \
             that is the fault to fix; the 429 needs nothing done about it on its own.",
        ),
        _ => None,
    }
}

/// A WebSocket upgrade the control plane refused with a status
/// [`describe_upgrade_refusal`] has an explanation for. Carried as a concrete type so
/// the reconnect loop can recognise it through `anyhow` — the same trick
/// [`StaleNodeSecret`] uses — and pick its log token and its counting by status.
#[derive(Debug)]
struct UpgradeRefused {
    status: u16,
    detail: &'static str,
}

impl std::fmt::Display for UpgradeRefused {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "control plane refused the connection with HTTP {}: {}",
            self.status, self.detail
        )
    }
}

impl std::error::Error for UpgradeRefused {}

/// Turn a failed WebSocket connect into the error `connect_and_run` returns.
///
/// Only an [`Error::Http`](tungstenite::Error::Http) — the upgrade reaching the control
/// plane and being answered with a plain HTTP response — can carry a status, and only a
/// status this agent can explain becomes an [`UpgradeRefused`]. Everything else (a dead
/// socket, a TLS pin mismatch, DNS) passes through unchanged.
fn upgrade_error(err: tungstenite::Error) -> anyhow::Error {
    if let tungstenite::Error::Http(resp) = &err {
        let status = resp.status().as_u16();
        if let Some(detail) = describe_upgrade_refusal(status) {
            return anyhow::Error::new(UpgradeRefused { status, detail });
        }
    }
    anyhow::Error::new(err)
}

/// Whether a failed connect/register cycle should bump
/// `consecutive_registration_failures` (#199 follow-up).
///
/// A refused upgrade is not a registration attempt — no `register` was sent — and the
/// refusals that caused it were each counted already. Letting it count again buries the
/// cause: `/health`'s `reason` is the LAST recorded failure, so the 429 would overwrite
/// the stale-identity line that says what to actually fix.
///
/// It is suppressed only once the verdict is already out. An agent that sees nothing
/// *but* 429s — its own counter reset by a restart, or another agent behind the same
/// address having spent the budget — has to be able to reach unhealthy, or a host that
/// never connects would answer `/health` with `ok` forever.
fn counts_as_registration_failure(refused_upgrade: bool, already_unhealthy: bool) -> bool {
    !(refused_upgrade && already_unhealthy)
}

/// Write the verified pin beside the node secret the first time a pinned connection
/// registers. Never overwrites what a reconnect merely re-learned; two cases do
/// overwrite, and both are a pin that differs from the file AND has just verified a real
/// handshake:
///   - a rotation the operator drove through CONTROL_PLANE_FINGERPRINT;
///   - `enrolled` — this register minted a NEW node identity (#199). The saved pin
///     belongs to the identity that was just replaced: on the re-enrollment path it is a
///     DIFFERENT control plane's certificate, and leaving it would strand the host the
///     moment `QUASAR_ENROLLMENT` is removed from the environment — which is exactly what
///     the docs tell operators to do once enrolled.
fn persist_pin_if_new(cfg: &Config, enrolled: bool) {
    let crate::enrollment::TransportPolicy::Pinned(fp) = &cfg.transport else {
        return;
    };
    let path = cfg.pin_path();
    // Compared as fingerprints, not as bytes: the file may have been hand-written
    // lowercase or with a `sha256:` prefix.
    let saved = std::fs::read_to_string(&path)
        .ok()
        .and_then(|s| crate::enrollment::Fingerprint::parse(&s).ok());
    if saved.as_ref() == Some(fp) {
        return;
    }
    let occupied = std::fs::symlink_metadata(&path).is_ok();
    let rotating =
        occupied && (enrolled || cfg.pin_source == Some(crate::enrollment::PinSource::Env));
    if occupied && !rotating {
        return;
    }
    let written = if rotating {
        replace_pin_file(&path, fp)
    } else {
        create_pin_file(&path, fp)
    };
    match written {
        // `Ok(false)` = the path was taken between the check and the create. Another
        // agent (or an attacker's symlink) owns it; leaving it alone is the safe answer.
        Ok(false) => {}
        Ok(true) => {
            info!(token = "cp-tls-pin-persisted", path = %path, rotated = rotating, "control-plane certificate pin saved")
        }
        Err(e) => {
            warn!(token = "cp-tls-pin-persist-failed", path = %path, "could not save the certificate pin: {e}")
        }
    }
}

/// `O_CREAT|O_EXCL` at 0600: no symlink is followed (EEXIST even for a dangling one), no
/// TOCTOU window behind the occupancy check above, and no umask widening. `Ok(false)`
/// means the path was already taken.
fn create_pin_file(path: &str, fp: &crate::enrollment::Fingerprint) -> std::io::Result<bool> {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;
    if let Some(parent) = std::path::Path::new(path).parent() {
        std::fs::create_dir_all(parent)?;
    }
    let mut f = match std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)
    {
        Ok(f) => f,
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => return Ok(false),
        Err(e) => return Err(e),
    };
    f.write_all(format!("{fp}\n").as_bytes())?;
    Ok(true)
}

/// The rotation path, the only one allowed to overwrite. Writes a pid-scoped temp with the
/// same `create_new` + 0600 rules and renames over the target: `rename(2)` replaces the
/// symlink itself rather than writing through it, and a reader never sees a half-file.
fn replace_pin_file(path: &str, fp: &crate::enrollment::Fingerprint) -> std::io::Result<bool> {
    let tmp = format!("{path}.{}.tmp", std::process::id());
    if !create_pin_file(&tmp, fp)? {
        return Ok(false);
    }
    match std::fs::rename(&tmp, path) {
        Ok(()) => Ok(true),
        Err(e) => {
            let _ = std::fs::remove_file(&tmp);
            Err(e)
        }
    }
}

fn persist_node_secret(path: &str, secret: &str) -> anyhow::Result<()> {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;
    if let Some(parent) = std::path::Path::new(path).parent() {
        std::fs::create_dir_all(parent)?;
    }
    // 0600: this is the host's credential and the default path is under
    // world-readable /tmp. `mode()` only applies on create, so a pre-existing file
    // must be tightened explicitly below.
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(path)?;
    f.write_all(secret.as_bytes())?;
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))?;
    Ok(())
}

async fn send_fresh_capacity<S>(sink: &mut S, mgr: &mut SessionManager) -> anyhow::Result<()>
where
    S: SinkExt<Message, Error = tokio_tungstenite::tungstenite::Error> + Unpin,
{
    let cap = offload_probe(detect_capacity_blocking).await;
    mgr.adopt_inventory(cap.gpus.clone(), cap.vram_targets);
    // Some inventory changes are only seen here (the 15 s refresh): without this their
    // stale checks are never forgotten and the new GPU is never probed (#301).
    mgr.notify_probe_inputs_if_changed();
    // A warm-up holds an encode slot for its duration. Applied to the REPORTED copy
    // only: `mgr.gpu_inventory` keeps the true inventory, so an assignment is still
    // validated against the hardware that exists.
    let mut cap_gpus = cap.gpus;
    crate::session::warmup::apply_encode_slot_reservation(&mut cap_gpus, mgr.warmup_reserved());
    let gpu_sets = mgr.gpu_codec_sets();
    apply_gpu_codecs(&mut cap_gpus, &gpu_sets);
    let msg = AgentMsg::Capacity {
        source_preparation: mgr.source_policy.as_ref().and_then(|p| p.report()),
        deployment_settings: Some(mgr.deployment_baseline.deployment_map()),
        config_policy_accepted_groups: mgr.policy_accepted_groups.clone(),
        config_policy_legacy_map_applied_id: mgr.policy_delivery_ack.clone(),
        host: cap.host,
        gpus: cap_gpus,
        gpu_detection: cap.gpu_detection,
        gpu_detection_reason: cap.gpu_detection_reason,
        console_capabilities: Some(cap.console),
        effective_settings: Some(mgr.runtime_settings.effective_map()),
        codecs: Some(host_codecs_from_sets(&gpu_sets)),
        codec_throughput: advertised_codec_throughput(&mgr.host_codec_report),
        readiness: Some(mgr.readiness.merged()),
    };
    send(sink, &msg).await
}

/// Send a `handle_control` reply with the capacity reports it needs.
async fn send_control_reply<S>(
    sink: &mut S,
    mgr: &mut SessionManager,
    reply: AgentMsg,
) -> anyhow::Result<()>
where
    S: SinkExt<Message, Error = tokio_tungstenite::tungstenite::Error> + Unpin,
{
    let typed_applied =
        matches!(&reply, AgentMsg::ConfigPolicyState { phase, .. } if phase == "applied");
    // agent-api.md §RH05: the fresh baseline precedes a
    // `deployment_baseline_changed` rejection on this ordered socket, so the
    // control plane reads the rejection against current evidence.
    if matches!(&reply, AgentMsg::ConfigPolicyState { error: Some(code), .. }
        if code == "deployment_baseline_changed")
    {
        send_fresh_capacity(sink, mgr).await?;
    }
    send(sink, &reply).await?;
    // A typed apply can withdraw probe-proven codecs (`zerocopy`); the control
    // plane must not keep routing on the old set.
    if typed_applied {
        send_fresh_capacity(sink, mgr).await?;
    }
    Ok(())
}

pub(crate) async fn send<S>(sink: &mut S, msg: &AgentMsg) -> anyhow::Result<()>
where
    S: SinkExt<Message, Error = tokio_tungstenite::tungstenite::Error> + Unpin,
{
    let json = serde_json::to_string(msg)?;
    sink.send(Message::Text(json.into())).await?;
    Ok(())
}

pub(crate) async fn recv<S>(stream: &mut S) -> anyhow::Result<String>
where
    S: StreamExt<Item = Result<Message, tokio_tungstenite::tungstenite::Error>> + Unpin,
{
    loop {
        match stream.next().await {
            None => anyhow::bail!("WebSocket closed by server"),
            Some(Err(e)) => return Err(e.into()),
            Some(Ok(Message::Text(t))) => return Ok(t.to_string()),
            Some(Ok(Message::Ping(_) | Message::Pong(_))) => continue, // handled by tungstenite
            Some(Ok(Message::Close(_))) => anyhow::bail!("WebSocket closed by server"),
            Some(Ok(other)) => {
                debug!(
                    token = "ws-non-text-message",
                    "ignoring non-text WS message: {other:?}"
                );
            }
        }
    }
}

/// Validate the scheduled GPU against `inventory` and pin the session to it: render node,
/// and the CUDA ordinal on the NVENC path. The media host probe binds through this same
/// function (`host_probe::media`), so a probe exercises the binding a session gets.
pub(crate) fn bind_gpu(
    inventory: &[crate::messages::GpuCapacity],
    gpu_index: i32,
    cfg: &mut SessionConfig,
) -> anyhow::Result<()> {
    let gpu = inventory
        .iter()
        .find(|gpu| gpu.index == gpu_index)
        .ok_or_else(|| {
            anyhow::anyhow!(
            "scheduled GPU index {gpu_index} is absent from the agent's latest capacity inventory"
        )
        })?;

    if cfg.encoder == EncoderChoice::Openh264 {
        return Ok(());
    }

    // `app.gpu=false` is not invalid: a benchmark app may feed a hardware
    // compositor without needing GPU access itself, and the app contract has no
    // separate "this workload requires a GPU" signal to validate against.

    let reported = gpu.render_node.as_deref().ok_or_else(|| anyhow::anyhow!(
        "scheduled GPU {gpu_index} ({} {}) has no reported render node; hardware encode cannot be pinned safely",
        gpu.vendor, gpu.model
    ))?;
    if cfg.render_node == "software" {
        anyhow::bail!(
            "hardware encoder {:?} cannot run with render_node=software; configure the reported node {reported}",
            cfg.encoder
        );
    }
    // Accept either exact identity capacity carries: the stable by-path
    // `render_node` or the in-container `device_path`. Never resolve the host's
    // by-path symlink here — it is not necessarily mounted even when the
    // corresponding renderD node is. An empty render_node (QUASAR_RENDER_NODE
    // unset) is unpinned: adopt the scheduled GPU's node below, matching the
    // scheduler's schedulableBindingSQL — the two resolvers must not diverge.
    let resolved_reported = gpu.device_path.as_deref().unwrap_or(reported);
    if !cfg.render_node.is_empty()
        && cfg.render_node != reported
        && cfg.render_node != resolved_reported
    {
        anyhow::bail!(
            "configured render node {} does not match scheduled GPU {gpu_index} node {reported} (resolved {resolved_reported})",
            cfg.render_node
        );
    }

    match cfg.encoder {
        EncoderChoice::Va if !matches!(gpu.vendor.as_str(), "amd" | "intel") => {
            anyhow::bail!(
                "VA encoder is incompatible with scheduled {} GPU {gpu_index}",
                gpu.vendor
            )
        }
        EncoderChoice::Nvenc if gpu.vendor != "nvidia" => {
            anyhow::bail!(
                "NVENC is incompatible with scheduled {} GPU {gpu_index}",
                gpu.vendor
            )
        }
        EncoderChoice::Nvenc => {
            cfg.cuda_device_id = capacity::nvidia_cuda_index_for_render_node(reported)
                .ok_or_else(|| anyhow::anyhow!(
                    "cannot map scheduled NVIDIA GPU {gpu_index} node {reported} to a CUDA device by PCI identity"
                ))?;
        }
        // Vulkan is pinned by the compositor-created GstVulkanDevice —
        // waylanddisplaysrc selects it from this render node and interpipe
        // forwards the context query — so it needs no ordinal.
        EncoderChoice::Vulkan => {}
        _ => {}
    }
    cfg.render_node = resolved_reported.to_string();
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::{AbrMode, EncoderChoice};

    #[test]
    fn failed_application_retirement_prevents_audio_and_legacy_sweep_before_admission() {
        let calls = std::cell::RefCell::new(Vec::new());
        assert_eq!(
            post_application_retirement(
                false,
                || calls.borrow_mut().push("audio"),
                || {
                    calls.borrow_mut().push("sweep");
                    1
                },
            ),
            None
        );
        assert!(calls.borrow().is_empty());
        assert_eq!(
            post_application_retirement(
                true,
                || calls.borrow_mut().push("audio"),
                || {
                    calls.borrow_mut().push("sweep");
                    1
                },
            ),
            Some(1)
        );
        assert_eq!(&*calls.borrow(), &["audio", "sweep"]);
    }

    #[test]
    fn application_maintenance_runs_journal_recovery_after_a_pending_map_error() {
        let calls = std::cell::RefCell::new(Vec::new());
        application_cleanup_maintenance_tick(
            || {
                calls.borrow_mut().push("pending");
                anyhow::bail!("map poisoned")
            },
            || {
                calls.borrow_mut().push("journal");
                Ok(())
            },
            || {
                calls.borrow_mut().push("audio");
                Ok(())
            },
            || {
                calls.borrow_mut().push("diagnostics");
                Ok(())
            },
        )
        .unwrap();
        assert_eq!(
            &*calls.borrow(),
            &["pending", "journal", "audio", "diagnostics"]
        );
    }

    /// Every journal is its own obligation. An application record this agent cannot
    /// finish must not leave a stopped-but-unremoved audio sidecar running until the
    /// next restart — the live defect this pass closes — so one failure never skips
    /// the journals after it, and the pass still reports that something is pending.
    #[test]
    fn a_failed_journal_never_starves_the_ones_after_it_and_the_tick_still_reports_it() {
        for failing in ["journal", "audio", "diagnostics"] {
            let calls = std::cell::RefCell::new(Vec::new());
            let run = |name: &'static str| {
                calls.borrow_mut().push(name);
                if name == failing {
                    Err(crate::runtime::RuntimeError::from(
                        crate::runtime::ErrorKind::Unavailable,
                    ))
                } else {
                    Ok(())
                }
            };
            let outcome = application_cleanup_maintenance_tick(
                || {
                    calls.borrow_mut().push("pending");
                    anyhow::bail!("map poisoned")
                },
                || run("journal"),
                || run("audio"),
                || run("diagnostics"),
            );
            assert_eq!(
                &*calls.borrow(),
                &["pending", "journal", "audio", "diagnostics"],
                "{failing} failing must not skip the journals after it"
            );
            assert_eq!(
                outcome.unwrap_err().kind,
                crate::runtime::ErrorKind::Unavailable,
                "a pass with an unfinished obligation is not a clean pass"
            );
        }
        // All clean is still clean.
        assert!(
            application_cleanup_maintenance_tick(|| Ok(()), || Ok(()), || Ok(()), || Ok(()))
                .is_ok()
        );
    }

    /// A `Config` on a scratch `node_secret_path`, so these tests never touch a real
    /// `/tmp/quasar-*-secret` left by another test or a live agent.
    fn test_cfg(node_secret_path: &str, enrollment_token: Option<&str>) -> Config {
        Config {
            control_plane_url: "ws://localhost:8080".to_string(),
            node_name: "test-node".to_string(),
            enrollment_token: enrollment_token.map(str::to_string),
            node_secret_path: node_secret_path.to_string(),
            transport: crate::enrollment::TransportPolicy::Plaintext,
            pin_source: None,
            webpki_from_blob: false,
            startup_warnings: Vec::new(),
        }
    }

    /// A pinned `Config` for the pin-file tests.
    fn pinned_cfg(
        node_secret_path: &str,
        fp: crate::enrollment::Fingerprint,
        pin_source: crate::enrollment::PinSource,
    ) -> Config {
        Config {
            transport: crate::enrollment::TransportPolicy::Pinned(fp),
            pin_source: Some(pin_source),
            ..test_cfg(node_secret_path, None)
        }
    }

    fn pin_fixture(byte: u8) -> crate::enrollment::Fingerprint {
        crate::enrollment::Fingerprint([byte; 32])
    }

    /// A readiness check by hand: `run_boot_gate` only reads id + status + wording, and a
    /// real probe would need devices these tests must not touch.
    fn gate_check(id: &str, status: &str) -> crate::messages::ReadinessCheck {
        crate::messages::ReadinessCheck {
            id: id.to_string(),
            status: status.to_string(),
            summary: format!("{id} is {status}"),
            remediation: format!("fix {id}"),
            observed_at: None,
            source: None,
            blocks: None,
        }
    }

    /// The #98 boot race as the gate sees it: host kernel has a node, this container has none.
    fn race_1_checks() -> Vec<crate::messages::ReadinessCheck> {
        vec![
            gate_check("render_node", crate::readiness::FAIL),
            gate_check("host_render_node", crate::readiness::PASS),
            gate_check("dri_node_app_access", crate::readiness::PASS),
        ]
    }

    /// Recorded effects, so a test asserts on what the gate DID rather than on log text.
    #[derive(Default)]
    struct GateSpy {
        in_flight_calls: AtomicU64,
        sleeps: AtomicU64,
        exits: AtomicU64,
        recorded: AtomicU64,
        cleared: AtomicU64,
    }

    #[test]
    fn a_provision_that_starts_during_the_delay_cancels_the_boot_exit() {
        let spy = GateSpy::default();
        // Quiescent when the decision is taken, busy by the time the delay ends: the exact
        // #66 race the post-sleep re-check exists for.
        let in_flight = || -> usize {
            if spy.in_flight_calls.fetch_add(1, Ordering::SeqCst) == 0 {
                0
            } else {
                1
            }
        };
        let fx = BootGateEffects {
            in_flight: &in_flight,
            record_exit: &|| spy.recorded.fetch_add(1, Ordering::SeqCst) as u32 + 1,
            clear_exits: &|| {
                spy.cleared.fetch_add(1, Ordering::SeqCst);
            },
            sleep: &|_| {
                spy.sleeps.fetch_add(1, Ordering::SeqCst);
            },
            exit: &|_| {
                spy.exits.fetch_add(1, Ordering::SeqCst);
            },
        };
        let exited = run_boot_gate(&race_1_checks(), true, false, 0, &fx);
        assert!(
            !exited,
            "a provision in flight must never be killed by the exit"
        );
        assert_eq!(spy.exits.load(Ordering::SeqCst), 0);
        assert_eq!(spy.sleeps.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn a_quiescent_boot_race_exits_once_and_records_the_attempt() {
        let spy = GateSpy::default();
        let fx = BootGateEffects {
            in_flight: &|| 0usize,
            record_exit: &|| spy.recorded.fetch_add(1, Ordering::SeqCst) as u32 + 1,
            clear_exits: &|| {
                spy.cleared.fetch_add(1, Ordering::SeqCst);
            },
            sleep: &|_| {
                spy.sleeps.fetch_add(1, Ordering::SeqCst);
            },
            exit: &|code| {
                assert_eq!(code, 1, "the restart policy keys on a non-zero exit");
                spy.exits.fetch_add(1, Ordering::SeqCst);
            },
        };
        assert!(run_boot_gate(&race_1_checks(), true, false, 0, &fx));
        assert_eq!(spy.exits.load(Ordering::SeqCst), 1);
        assert_eq!(spy.recorded.load(Ordering::SeqCst), 1);
        assert_eq!(spy.sleeps.load(Ordering::SeqCst), 1);
        assert_eq!(spy.cleared.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn a_stay_verdict_neither_sleeps_nor_exits() {
        let spy = GateSpy::default();
        let fx = BootGateEffects {
            in_flight: &|| 0usize,
            record_exit: &|| spy.recorded.fetch_add(1, Ordering::SeqCst) as u32 + 1,
            clear_exits: &|| {
                spy.cleared.fetch_add(1, Ordering::SeqCst);
            },
            sleep: &|_| {
                spy.sleeps.fetch_add(1, Ordering::SeqCst);
            },
            exit: &|_| {
                spy.exits.fetch_add(1, Ordering::SeqCst);
            },
        };
        // Stale CDI modes: a restart reproduces them, so the gate must not spend a boot on it.
        let checks = vec![
            gate_check("render_node", crate::readiness::PASS),
            gate_check("host_render_node", crate::readiness::PASS),
            gate_check("dri_node_app_access", crate::readiness::FAIL),
        ];
        assert!(!run_boot_gate(&checks, true, true, 0, &fx));
        assert_eq!(spy.sleeps.load(Ordering::SeqCst), 0);
        assert_eq!(spy.exits.load(Ordering::SeqCst), 0);
        assert_eq!(spy.recorded.load(Ordering::SeqCst), 0);
        assert_eq!(
            spy.cleared.load(Ordering::SeqCst),
            0,
            "a fault is not a clean boot"
        );
    }

    #[test]
    fn a_clean_boot_clears_the_retry_streak() {
        let spy = GateSpy::default();
        let fx = BootGateEffects {
            in_flight: &|| 0usize,
            record_exit: &|| spy.recorded.fetch_add(1, Ordering::SeqCst) as u32 + 1,
            clear_exits: &|| {
                spy.cleared.fetch_add(1, Ordering::SeqCst);
            },
            sleep: &|_| {
                spy.sleeps.fetch_add(1, Ordering::SeqCst);
            },
            exit: &|_| {
                spy.exits.fetch_add(1, Ordering::SeqCst);
            },
        };
        let checks = vec![
            gate_check("render_node", crate::readiness::PASS),
            gate_check("host_render_node", crate::readiness::PASS),
        ];
        assert!(!run_boot_gate(&checks, true, true, 3, &fx));
        assert_eq!(spy.cleared.load(Ordering::SeqCst), 1);
        assert_eq!(spy.exits.load(Ordering::SeqCst), 0);
    }

    /// The boot-exit streak has to survive a restart-policy restart (same container, same
    /// /tmp) and be clearable, or the retry bound is not a bound.
    #[test]
    fn boot_exits_count_up_and_clear() {
        let dir = tempfile::tempdir().unwrap();
        let counter = dir.path().join("boot-exits");
        assert_eq!(read_boot_exits(&counter), 0, "no file is a fresh streak");
        assert_eq!(record_boot_exit(&counter), 1);
        assert_eq!(record_boot_exit(&counter), 2);
        assert_eq!(read_boot_exits(&counter), 2);
        std::fs::remove_file(&counter).unwrap();
        assert_eq!(read_boot_exits(&counter), 0);
        // A truncated or hand-edited file must read as a fresh streak, never panic.
        std::fs::write(&counter, "not-a-number").unwrap();
        assert_eq!(read_boot_exits(&counter), 0);
    }

    #[test]
    fn a_fresh_pin_lands_at_0600() {
        use std::os::unix::fs::PermissionsExt;

        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        let cfg = pinned_cfg(
            secret_path.to_str().unwrap(),
            pin_fixture(0xAB),
            crate::enrollment::PinSource::Blob,
        );
        persist_pin_if_new(&cfg, false);

        let written = std::fs::read_to_string(cfg.pin_path()).unwrap();
        assert_eq!(written.trim(), pin_fixture(0xAB).to_colon_hex());
        let mode = std::fs::metadata(cfg.pin_path())
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o600, "mode was {:o}", mode & 0o777);
    }

    /// A reconnect must never re-learn a pin: a file already there is the operator's.
    #[test]
    fn an_existing_pin_file_is_not_overwritten_by_a_blob_or_persisted_pin() {
        for source in [
            crate::enrollment::PinSource::Blob,
            crate::enrollment::PinSource::Persisted,
        ] {
            let dir = tempfile::tempdir().unwrap();
            let secret_path = dir.path().join("node-secret");
            let cfg = pinned_cfg(secret_path.to_str().unwrap(), pin_fixture(0xAB), source);
            std::fs::write(cfg.pin_path(), "not-a-fingerprint\n").unwrap();

            persist_pin_if_new(&cfg, false);
            assert_eq!(
                std::fs::read_to_string(cfg.pin_path()).unwrap(),
                "not-a-fingerprint\n",
                "{source:?}"
            );
        }
    }

    /// CONTROL_PLANE_FINGERPRINT is the rotation vehicle, and the new pin has just
    /// verified a real handshake — the one case overwriting is right.
    #[test]
    fn an_operator_driven_rotation_refreshes_the_pin_file() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        let cfg = pinned_cfg(
            secret_path.to_str().unwrap(),
            pin_fixture(0xCD),
            crate::enrollment::PinSource::Env,
        );
        std::fs::write(cfg.pin_path(), format!("{}\n", pin_fixture(0xAB))).unwrap();

        persist_pin_if_new(&cfg, false);
        assert_eq!(
            std::fs::read_to_string(cfg.pin_path()).unwrap().trim(),
            pin_fixture(0xCD).to_colon_hex()
        );
        // No temp file survives the rename.
        let leftovers: Vec<_> = std::fs::read_dir(dir.path())
            .unwrap()
            .filter_map(|e| e.ok().map(|e| e.file_name()))
            .filter(|n| n.to_string_lossy().ends_with(".tmp"))
            .collect();
        assert!(leftovers.is_empty(), "{leftovers:?}");
    }

    /// A file whose content only differs in case/prefix is the same pin — an equality
    /// check on bytes would rewrite it on every connect.
    #[test]
    fn a_pin_file_in_another_spelling_is_left_alone() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        let cfg = pinned_cfg(
            secret_path.to_str().unwrap(),
            pin_fixture(0xAB),
            crate::enrollment::PinSource::Env,
        );
        let lowercase = format!(
            "sha256:{}\n",
            pin_fixture(0xAB).to_colon_hex().to_lowercase()
        );
        std::fs::write(cfg.pin_path(), &lowercase).unwrap();

        persist_pin_if_new(&cfg, false);
        assert_eq!(std::fs::read_to_string(cfg.pin_path()).unwrap(), lowercase);
    }

    /// A symlink planted at the pin path must not become a write primitive: neither the
    /// create nor the rotation path may create the symlink's target.
    #[test]
    fn a_dangling_symlink_at_the_pin_path_is_never_followed() {
        for source in [
            crate::enrollment::PinSource::Blob,
            crate::enrollment::PinSource::Env,
        ] {
            // Both write paths, including the #199 enrollment refresh.
            for enrolled in [false, true] {
                let dir = tempfile::tempdir().unwrap();
                let secret_path = dir.path().join("node-secret");
                let cfg = pinned_cfg(secret_path.to_str().unwrap(), pin_fixture(0xAB), source);
                let target = dir.path().join("victim");
                std::os::unix::fs::symlink(&target, cfg.pin_path()).unwrap();

                persist_pin_if_new(&cfg, enrolled);
                assert!(
                    !target.exists(),
                    "{source:?} enrolled={enrolled} wrote through the symlink to {target:?}"
                );
            }
        }
    }

    /// #199: the re-enrollment fallback mints a new identity on a control plane that is
    /// not the one whose certificate is pinned in the file beside the old node secret.
    /// The pin that just verified THIS handshake is the one that must survive.
    #[test]
    fn enrolling_again_refreshes_a_pin_left_by_a_previous_control_plane() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        let cfg = pinned_cfg(
            secret_path.to_str().unwrap(),
            pin_fixture(0xCD),
            crate::enrollment::PinSource::Blob,
        );
        std::fs::write(cfg.pin_path(), format!("{}\n", pin_fixture(0xAB))).unwrap();

        persist_pin_if_new(&cfg, true);
        assert_eq!(
            std::fs::read_to_string(cfg.pin_path()).unwrap().trim(),
            pin_fixture(0xCD).to_colon_hex()
        );
        let leftovers: Vec<_> = std::fs::read_dir(dir.path())
            .unwrap()
            .filter_map(|e| e.ok().map(|e| e.file_name()))
            .filter(|n| n.to_string_lossy().ends_with(".tmp"))
            .collect();
        assert!(leftovers.is_empty(), "{leftovers:?}");
    }

    /// Fixture manifest for `nvidia_volume::Status::Provisioned` — field values
    /// are irrelevant to the classification, only the variant is.
    fn nvvol_manifest() -> crate::nvidia_volume::Manifest {
        crate::nvidia_volume::Manifest {
            driver_version: "610.57.04".to_string(),
            sha256: "test".to_string(),
            url: crate::nvidia_volume::run_url("610.57.04"),
            provisioned_at_unix: 1,
            agent_version: "test".to_string(),
            lib64_count: 1,
            lib32_count: 1,
            layout_version: 1,
        }
    }

    /// First boot: no volume adopted, provisioning not started or mid-flight. INFO
    /// under `vulkan-codec-plan-pending-driver-volume`, not WARN.
    #[test]
    fn degraded_plan_is_pending_when_no_volume_adopted_and_not_yet_failed() {
        use crate::nvidia_volume::Status;

        assert!(vulkan_plan_degradation_is_pending_driver_volume(
            false,
            &Status::Idle,
        ));
        assert!(vulkan_plan_degradation_is_pending_driver_volume(
            false,
            &Status::Provisioning {
                phase: "downloading".to_string(),
                percent: Some(42),
            },
        ));
    }

    /// Provisioning gave up: no restart is coming, so it stays WARN under the original
    /// token even though no volume was ever adopted.
    #[test]
    fn degraded_plan_is_not_pending_once_provisioning_has_failed() {
        use crate::nvidia_volume::Status;

        assert!(!vulkan_plan_degradation_is_pending_driver_volume(
            false,
            &Status::Failed("network unreachable".to_string()),
        ));
    }

    /// Volume adopted but a codec's vulkan element still missing: an image defect, not
    /// first-boot timing. NOT pending, whatever the recorded status.
    #[test]
    fn degraded_plan_is_not_pending_once_a_volume_is_adopted() {
        use crate::nvidia_volume::Status;

        assert!(!vulkan_plan_degradation_is_pending_driver_volume(
            true,
            &Status::Provisioned(nvvol_manifest()),
        ));
        // Even a stale/odd status combination — volume presence wins.
        assert!(!vulkan_plan_degradation_is_pending_driver_volume(
            true,
            &Status::Idle,
        ));
    }

    /// #531: the defect was a poll that never yielded, so the assertion is on progress.
    /// Replace `offload_probe(f)` with `async { f() }` and `ticks` is 0; with the
    /// offload it is ~15.
    #[tokio::test]
    async fn offload_probe_lets_the_calling_future_keep_making_progress() {
        const PROBE: Duration = Duration::from_millis(300);
        const TICK: Duration = Duration::from_millis(20);

        let probe = offload_probe(|| {
            // A genuinely blocking, non-async wait, standing in for the real probes'
            // subprocess forks and sysfs reads.
            std::thread::sleep(PROBE);
            "capacity"
        });
        tokio::pin!(probe);

        let mut ticks = 0u32;
        let answer = loop {
            tokio::select! {
                v = &mut probe => break v,
                _ = tokio::time::sleep(TICK) => ticks += 1,
            }
        };

        assert_eq!(
            answer, "capacity",
            "the probe's result must still be awaited"
        );
        // 15 in the ideal case; assert clear of both 0 (the defect) and scheduler noise.
        assert!(
            ticks >= 5,
            "the sibling select! arm fired only {ticks} times while the probe ran — \
             the probe is still blocking the calling future's poll"
        );
    }

    /// Every probe entry point stays `Send + 'static`-callable: a non-`Send` capture
    /// would break the offload, and this fails at compile time.
    #[test]
    fn probe_entry_points_are_offloadable() {
        fn assert_offloadable<T: Send + 'static, F: FnOnce() -> T + Send + 'static>(_f: F) {}

        assert_offloadable(detect_capacity_blocking);
        assert_offloadable(capacity::detect);
        assert_offloadable(crate::capacity::prewarm_nvidia_smi_rows);

        let settings = crate::session::settings::RuntimeSettings::baseline_with(&|_| None);
        assert_offloadable(move || probe_host_codecs(&settings));

        let lib32 = String::new();
        assert_offloadable(move || {
            crate::readiness::probe(
                &crate::readiness::ProbeEnv::live(false, &lib32)
                    .with_gpu_present(false)
                    .with_codec_probe(None),
            )
        });
    }

    /// A panic inside an offloaded probe must reach the caller as a panic:
    /// `spawn_blocking` otherwise turns it into an easily-swallowed `JoinError`.
    #[tokio::test]
    #[should_panic(expected = "probe exploded")]
    async fn offload_probe_repropagates_a_panic() {
        let _: () = offload_probe(|| panic!("probe exploded")).await;
    }

    fn refresh_check(id: &str) -> crate::messages::ReadinessCheck {
        crate::messages::ReadinessCheck {
            id: id.into(),
            status: crate::readiness::PASS.into(),
            summary: String::new(),
            remediation: String::new(),
            observed_at: None,
            source: None,
            blocks: None,
        }
    }

    #[tokio::test]
    async fn a_readiness_refresh_inside_its_deadline_reports_its_checks() {
        let (tx, mut rx) = mpsc::channel(1);
        run_readiness_refresh(|| vec![refresh_check("uinput")], Duration::from_secs(5), tx).await;

        assert_eq!(
            rx.recv().await,
            Some(ReadinessRefresh::Done(Ok(vec![refresh_check("uinput")])))
        );
        assert_eq!(rx.recv().await, None);
    }

    #[tokio::test]
    async fn a_readiness_refresh_that_panics_is_a_refresh_error() {
        let (tx, mut rx) = mpsc::channel(1);
        run_readiness_refresh(|| panic!("probe exploded"), Duration::from_secs(5), tx).await;

        assert!(matches!(
            rx.recv().await,
            Some(ReadinessRefresh::Done(Err(_)))
        ));
        assert_eq!(rx.recv().await, None);
    }

    /// Overdue first, so the report gains its warning while the probe is still hung;
    /// then exactly one `Done` when it ends, which is what frees the next refresh.
    #[tokio::test]
    async fn a_readiness_refresh_past_its_deadline_is_overdue_then_done() {
        let (tx, mut rx) = mpsc::channel(1);
        let (release, held) = std::sync::mpsc::channel::<()>();
        let task = tokio::spawn(run_readiness_refresh(
            move || {
                let _ = held.recv();
                vec![refresh_check("uinput")]
            },
            Duration::from_millis(20),
            tx,
        ));

        let first = tokio::time::timeout(Duration::from_secs(2), rx.recv()).await;
        assert_eq!(first, Ok(Some(ReadinessRefresh::Overdue)));
        // Nothing more while the probe is still running.
        assert!(
            tokio::time::timeout(Duration::from_millis(100), rx.recv())
                .await
                .is_err(),
            "the refresh must stay in flight until its probe ends"
        );

        release.send(()).unwrap();
        assert_eq!(
            rx.recv().await,
            Some(ReadinessRefresh::Done(Ok(vec![refresh_check("uinput")])))
        );
        task.await.unwrap();
        assert_eq!(rx.recv().await, None);
    }

    #[test]
    fn enrollment_reachable_errs_with_no_secret_and_no_token() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        let cfg = test_cfg(secret_path.to_str().unwrap(), None);
        let err = enrollment_reachable(&cfg).expect_err("should be unreachable");
        assert!(err.contains("ENROLLMENT_TOKEN"), "err: {err}");
        assert!(err.contains(secret_path.to_str().unwrap()), "err: {err}");
    }

    #[test]
    fn enrollment_reachable_errs_on_whitespace_only_token() {
        // `Config::from_env` already folds this to None, but this function re-checks
        // rather than trusting that invariant.
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        let cfg = test_cfg(secret_path.to_str().unwrap(), Some("   "));
        assert!(enrollment_reachable(&cfg).is_err());
    }

    #[test]
    fn enrollment_reachable_ok_with_token_and_no_secret() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        let cfg = test_cfg(secret_path.to_str().unwrap(), Some("tok-123"));
        assert!(enrollment_reachable(&cfg).is_ok());
    }

    #[test]
    fn enrollment_reachable_ok_with_persisted_secret_and_no_token() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        std::fs::write(&secret_path, "persisted-secret\n").unwrap();
        let cfg = test_cfg(secret_path.to_str().unwrap(), None);
        assert!(enrollment_reachable(&cfg).is_ok());
    }

    #[test]
    fn enrollment_reachable_errs_on_empty_persisted_secret_file() {
        // A zero-byte secret file (a truncated write) must not count as reachable.
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        std::fs::write(&secret_path, "   \n").unwrap();
        let cfg = test_cfg(secret_path.to_str().unwrap(), None);
        assert!(enrollment_reachable(&cfg).is_err());
    }

    // ── #199: a saved node secret this control plane never minted ───────────
    //
    // The reported failure: a machine enrolled to control plane A is re-enrolled
    // against control plane B, the agent data volume survives, and `choose_auth`
    // keeps presenting A's node secret. B answers `host_not_found` forever and the
    // enrollment token the operator just pasted is never tried.

    #[test]
    fn stale_identity_re_enrolls_when_a_token_is_configured() {
        assert_eq!(
            stale_identity("host_not_found", true, true),
            Some(StaleIdentity::ReEnroll)
        );
    }

    #[test]
    fn stale_identity_is_unresolvable_without_a_token() {
        assert_eq!(
            stale_identity("host_not_found", true, false),
            Some(StaleIdentity::Unresolvable)
        );
    }

    #[test]
    fn stale_identity_ignores_a_reject_of_an_enrollment_register() {
        // The fallback already ran (or this was a first enrollment): re-sending the
        // same token is a loop, not a recovery.
        assert_eq!(stale_identity("host_not_found", false, true), None);
    }

    #[test]
    fn stale_identity_ignores_every_other_code() {
        for code in ["auth_failed", "protocol_error", "internal_error", ""] {
            assert_eq!(stale_identity(code, true, true), None, "code {code}");
        }
    }

    #[test]
    fn stale_identity_message_names_the_secret_and_the_remedy() {
        let re_enroll = stale_identity_message("/var/lib/x/node-secret", StaleIdentity::ReEnroll);
        assert!(re_enroll.contains("/var/lib/x/node-secret"), "{re_enroll}");
        assert!(
            re_enroll.contains("enrollment token"),
            "the re-enroll line must say what it is about to do: {re_enroll}"
        );
        let stuck = stale_identity_message("/var/lib/x/node-secret", StaleIdentity::Unresolvable);
        assert!(stuck.contains("/var/lib/x/node-secret"), "{stuck}");
        assert!(
            stuck.contains("QUASAR_RESET_IDENTITY"),
            "the unrecoverable line must name the way to clear the identity: {stuck}"
        );
    }

    /// Neither line may promise an outcome that has not happened yet. The enrollment
    /// token is single-use and expiring: when it has been spent the fallback is refused
    /// too, and a reassuring line would then print beside every failure forever.
    #[test]
    fn the_re_enroll_line_does_not_promise_success() {
        let re_enroll = stale_identity_message("/var/lib/x/node-secret", StaleIdentity::ReEnroll);
        let lower = re_enroll.to_lowercase();
        assert!(
            !lower.contains("no operator action") && !lower.contains("nothing is needed"),
            "the line promises an outcome it cannot know: {re_enroll}"
        );
        assert!(
            lower.contains("expired") || lower.contains("already been used"),
            "the line must admit the token can be refused too: {re_enroll}"
        );
    }

    /// The agent cannot know its data volume's name — the compose project decides it
    /// (`QUASAR_PROJECT`), and an agent outside a container has none. Naming one would
    /// be a remedy that silently points at the wrong volume.
    #[test]
    fn no_stale_identity_line_asserts_a_volume_name() {
        for kind in [StaleIdentity::ReEnroll, StaleIdentity::Unresolvable] {
            let msg = stale_identity_message("/var/lib/x/node-secret", kind);
            assert!(
                !msg.contains("quasar-agent-data") && !msg.contains("docker volume rm"),
                "{kind:?} asserts a volume name the agent cannot know: {msg}"
            );
        }
    }

    /// The run loop arms the one-shot token fallback by downcasting the error out of
    /// `connect_and_run`. That only works if `anyhow` keeps the concrete type across the
    /// bail, which is easy to break by "simplifying" the bail into a formatted string.
    #[test]
    fn a_stale_secret_reject_survives_the_anyhow_boundary() {
        fn rejected() -> anyhow::Result<()> {
            anyhow::bail!(StaleNodeSecret(
                "the saved secret is unknown here".to_string()
            ));
        }
        let err = rejected().expect_err("should be an error");
        assert!(
            err.downcast_ref::<StaleNodeSecret>().is_some(),
            "the run loop cannot see the stale-secret reject: {err:#}"
        );
        assert!(format!("{err:#}").contains("the saved secret is unknown here"));
    }

    // ── #199 follow-up: the 429 that follows the refused registers ──────────
    //
    // Ten refused registers with no minute's gap between them trip the control
    // plane's enrollment-failure limiter (its window slides off the LAST
    // refusal, so a backoff that never idles a full minute trips it however long
    // it takes), and the WebSocket upgrade is then refused with 429 before any
    // register is sent. The same 429 also answers an address with more than ten
    // handshakes in flight, where there are no refusals above it at all. The
    // operator's complaint was that this reads as a second, unrelated fault. It
    // is not accounting that is wrong — an uncounted `host_not_found` would turn
    // `/agent/ws` into a rate-unbounded node-name oracle — it is the presentation.

    #[test]
    fn a_rate_limited_upgrade_is_explained_as_a_consequence() {
        let line = describe_upgrade_refusal(429).expect("429 must be explained");
        let lower = line.to_lowercase();
        assert!(
            lower.contains("rate-limit"),
            "the line must name what the control plane is doing: {line}"
        );
        assert!(
            lower.contains("consequence"),
            "the line must say this is downstream of the refusals, not a new fault: {line}"
        );
        assert!(
            lower.contains("minute"),
            "the line must say when it lifts: {line}"
        );
        assert!(
            lower.contains("unresolvable"),
            "the line must point at the reject above as the thing to act on: {line}"
        );
        // The limiter's window slides off the LAST refusal, so "ten inside a
        // minute" is false — and false in the direction that sends an operator
        // hunting for another client, which is the misreading this line exists
        // to stop. An agent on a 30 s backoff trips it in about three minutes.
        assert!(
            lower.contains("no minute's gap"),
            "the line must state the limiter's real rule, not 'ten inside a minute': {line}"
        );
        // A 429 is not proof of refused registers: the in-flight cap answers the
        // same status for a fleet behind one NAT reconnecting together.
        assert!(
            lower.contains("in flight"),
            "the line must name the other thing that answers 429: {line}"
        );
    }

    #[test]
    fn an_unexplained_upgrade_status_gets_no_line() {
        for status in [503, 502, 500, 401, 404, 200] {
            assert_eq!(
                describe_upgrade_refusal(status),
                None,
                "status {status} has no explanation to offer"
            );
        }
    }

    /// The run loop reads the status back off the error to choose its token, so the
    /// refusal has to survive the `anyhow` boundary the same way `StaleNodeSecret` does.
    #[test]
    fn a_rate_limited_upgrade_survives_the_anyhow_boundary() {
        let err = upgrade_error(tungstenite::Error::Http(Box::new(
            http_response_with_status(429),
        )));
        let refusal = err
            .downcast_ref::<UpgradeRefused>()
            .expect("the run loop cannot see the refusal");
        assert_eq!(refusal.status, 429);
        assert!(
            format!("{err:#}").contains("429"),
            "the logged line must still name the status: {err:#}"
        );
    }

    /// An upgrade refused with a status the agent has nothing to say about stays an
    /// ordinary connection failure — generic token, ordinary counting.
    #[test]
    fn an_unexplained_upgrade_refusal_stays_an_ordinary_failure() {
        let err = upgrade_error(tungstenite::Error::Http(Box::new(
            http_response_with_status(503),
        )));
        assert!(
            err.downcast_ref::<UpgradeRefused>().is_none(),
            "503 must not be dressed up as an explained refusal: {err:#}"
        );
    }

    /// The 429 is not a register: it must not arm the #199 one-shot token fallback,
    /// or every rate-limited reconnect would spend a single-use enrollment string.
    #[test]
    fn a_rate_limited_upgrade_does_not_arm_the_enrollment_fallback() {
        let mut fallback = EnrollmentFallback::default();
        fallback.observe(&upgrade_error(tungstenite::Error::Http(Box::new(
            http_response_with_status(429),
        ))));
        assert!(
            !fallback.take_for_attempt(),
            "a refused upgrade carried no credential, so it cannot tell the agent to \
             switch credentials"
        );
    }

    /// Counting a rate-limited upgrade as one more registration failure would overwrite
    /// `/health`'s `reason` with the 429 and bury the reject that caused it. Suppressed
    /// only once the verdict is already out: a cold agent that sees nothing BUT 429s
    /// (its own counter reset by a restart, or another agent behind the same address
    /// spent the budget) must still reach unhealthy rather than report `ok` forever.
    #[test]
    fn a_rate_limited_upgrade_stops_counting_once_the_agent_is_already_unhealthy() {
        assert!(!counts_as_registration_failure(true, true));
        assert!(counts_as_registration_failure(true, false));
        assert!(counts_as_registration_failure(false, true));
        assert!(counts_as_registration_failure(false, false));
    }

    fn http_response_with_status(status: u16) -> tungstenite::http::Response<Option<Vec<u8>>> {
        tungstenite::http::Response::builder()
            .status(status)
            .body(None)
            .expect("build response")
    }

    // ── the reconnect loop's one-shot alternation (#199) ────────────────────
    //
    // `EnrollmentFallback` is the loop semantics the commit message, the CHANGELOG and
    // docs/configuration.md all describe: one token attempt per reject, never a latch.
    // These drive the same `register_reject_error` the live path calls.

    /// The reported scenario, end to end without a socket: the saved secret goes out,
    /// the control plane refuses it, the NEXT attempt carries the token.
    #[test]
    fn a_stale_secret_reject_makes_the_next_attempt_present_the_token() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        std::fs::write(&secret_path, "secret-from-the-other-control-plane\n").unwrap();
        let cfg = test_cfg(secret_path.to_str().unwrap(), Some("tok-123"));
        let mut fallback = EnrollmentFallback::default();

        let first = choose_auth(&cfg, fallback.take_for_attempt()).unwrap();
        assert!(
            matches!(first, Auth::Reconnect { .. }),
            "the saved secret goes first: {first:?}"
        );

        let err = register_reject_error(
            &cfg,
            "host_not_found",
            "node not enrolled",
            matches!(first, Auth::Reconnect { .. }),
        );
        fallback.observe(&err);

        let second = choose_auth(&cfg, fallback.take_for_attempt()).unwrap();
        assert!(
            matches!(second, Auth::Enrollment { .. }),
            "the reject did not arm the token fallback: {second:?}"
        );
    }

    /// The arming is CONSUMED, not latched. A spent single-use token is refused too, and
    /// a latch would leave a host that a returning control plane could still have
    /// re-admitted on its saved secret with nothing else to offer.
    #[test]
    fn the_token_fallback_is_consumed_by_one_attempt_and_does_not_latch() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        std::fs::write(&secret_path, "saved-secret\n").unwrap();
        let cfg = test_cfg(secret_path.to_str().unwrap(), Some("tok-123"));
        let mut fallback = EnrollmentFallback::default();

        fallback.observe(&register_reject_error(&cfg, "host_not_found", "", true));
        assert!(fallback.take_for_attempt(), "the reject should arm it");
        // The token attempt is refused in its turn — the control plane says nothing
        // about a saved secret, because none was presented.
        fallback.observe(&register_reject_error(
            &cfg,
            "auth_failed",
            "authentication failed",
            false,
        ));
        assert!(
            !fallback.take_for_attempt(),
            "the token preference latched; the saved secret would never be offered again"
        );
        assert!(
            matches!(choose_auth(&cfg, false).unwrap(), Auth::Reconnect { .. }),
            "the attempt after a spent token must go back to the saved secret"
        );
    }

    /// Two takes on one arming: the second is already false even with no failure in
    /// between. Pins `take_for_attempt` against being rewritten as a plain read.
    #[test]
    fn one_arming_survives_exactly_one_attempt() {
        let mut fallback = EnrollmentFallback::default();
        let dir = tempfile::tempdir().unwrap();
        let cfg = test_cfg(
            dir.path().join("node-secret").to_str().unwrap(),
            Some("tok-123"),
        );
        fallback.observe(&register_reject_error(&cfg, "host_not_found", "", true));
        assert!(fallback.take_for_attempt());
        assert!(
            !fallback.take_for_attempt(),
            "the arming outlived its attempt"
        );
    }

    /// Only the stale-secret reject arms it. Everything else the loop meets — a dropped
    /// socket, a bad token, a pin mismatch, and a `host_not_found` answering a register
    /// that already carried the token — must leave the preference alone.
    #[test]
    fn nothing_but_a_stale_secret_reject_arms_the_fallback() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        let cfg = test_cfg(secret_path.to_str().unwrap(), Some("tok-123"));

        let cases: Vec<anyhow::Error> = vec![
            anyhow::anyhow!("connection reset without closing handshake"),
            register_reject_error(&cfg, "auth_failed", "authentication failed", true),
            register_reject_error(&cfg, "protocol_error", "expected register", true),
            // The fallback already ran: re-sending the token is a loop, not a recovery.
            register_reject_error(&cfg, "host_not_found", "node not enrolled", false),
        ];
        for err in cases {
            let mut fallback = EnrollmentFallback::default();
            fallback.observe(&err);
            assert!(
                !fallback.take_for_attempt(),
                "armed the token fallback on: {err:#}"
            );
        }
    }

    /// With no token configured there is nothing to fall back to, so the reject must not
    /// arm anything — the agent reports the stale identity instead.
    #[test]
    fn a_stale_secret_reject_arms_nothing_without_a_token() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        std::fs::write(&secret_path, "saved-secret\n").unwrap();
        let cfg = test_cfg(secret_path.to_str().unwrap(), None);
        let mut fallback = EnrollmentFallback::default();

        let err = register_reject_error(&cfg, "host_not_found", "node not enrolled", true);
        fallback.observe(&err);
        assert!(!fallback.take_for_attempt());
        assert!(
            format!("{err:#}").contains(secret_path.to_str().unwrap()),
            "the unresolvable reject must name the saved secret: {err:#}"
        );
    }

    #[test]
    fn choose_auth_prefers_the_saved_secret() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        std::fs::write(&secret_path, "saved-secret\n").unwrap();
        let cfg = test_cfg(secret_path.to_str().unwrap(), Some("tok-123"));
        match choose_auth(&cfg, false).unwrap() {
            Auth::Reconnect { node_secret } => assert_eq!(node_secret, "saved-secret"),
            other => panic!("want Reconnect, got {other:?}"),
        }
    }

    #[test]
    fn choose_auth_forced_presents_the_token_over_the_saved_secret() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        std::fs::write(&secret_path, "stale-secret\n").unwrap();
        let cfg = test_cfg(secret_path.to_str().unwrap(), Some("tok-123"));
        match choose_auth(&cfg, true).unwrap() {
            Auth::Enrollment { enrollment_token } => assert_eq!(enrollment_token, "tok-123"),
            other => panic!("want Enrollment, got {other:?}"),
        }
    }

    #[test]
    fn choose_auth_forced_without_a_token_still_uses_the_saved_secret() {
        // The force flag is a preference, never a way to make a registerable agent
        // unregisterable: with no token there is nothing to prefer.
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        std::fs::write(&secret_path, "saved-secret\n").unwrap();
        let cfg = test_cfg(secret_path.to_str().unwrap(), None);
        match choose_auth(&cfg, true).unwrap() {
            Auth::Reconnect { node_secret } => assert_eq!(node_secret, "saved-secret"),
            other => panic!("want Reconnect, got {other:?}"),
        }
    }

    #[test]
    fn choose_auth_falls_back_to_the_token_with_no_saved_secret() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        let cfg = test_cfg(secret_path.to_str().unwrap(), Some("tok-123"));
        match choose_auth(&cfg, false).unwrap() {
            Auth::Enrollment { enrollment_token } => assert_eq!(enrollment_token, "tok-123"),
            other => panic!("want Enrollment, got {other:?}"),
        }
    }

    #[test]
    fn choose_auth_errs_with_neither_credential() {
        let dir = tempfile::tempdir().unwrap();
        let secret_path = dir.path().join("node-secret");
        let cfg = test_cfg(secret_path.to_str().unwrap(), None);
        assert!(choose_auth(&cfg, false).is_err());
    }

    fn gpu(index: i32, vendor: &str, render_node: Option<&str>) -> crate::messages::GpuCapacity {
        crate::messages::GpuCapacity {
            index,
            vendor: vendor.to_string(),
            model: "fixture".to_string(),
            vram_mb_total: 8192,
            encode_slots_total: 2,
            render_node: render_node.map(str::to_string),
            device_path: render_node.map(crate::session::settings::canonicalize_render_node),
            driver_identity: None,
            codecs: None,
        }
    }

    fn assignment_config(encoder: EncoderChoice, render_node: &str) -> SessionConfig {
        let mut settings = crate::session::settings::RuntimeSettings::baseline_with(&|_| None);
        settings.encoder = encoder;
        settings.render_node = render_node.to_string();
        SessionConfig::for_assignment_with(
            &settings,
            StreamParams {
                width: 1920,
                height: 1080,
                fps: 60,
                bitrate_kbps: 10_000,
                h264_profile: "constrained-baseline".to_string(),
                codec: crate::session::Codec::H264,
                abr_floor_kbps: 0,
                mic: false,
            },
            None,
        )
    }

    fn manager_with(gpus: Vec<crate::messages::GpuCapacity>) -> SessionManager {
        SessionManager::new(
            Arc::new(Mutex::new(HashSet::new())),
            HealthState::new(),
            gpus,
            Vec::new(),
            String::new(),
            test_image_mgr(),
            test_release_mgr(),
        )
    }

    fn diagnostic_sender() -> DiagnosticEventTx {
        let (tx, _rx) = mpsc::channel(1);
        DiagnosticEventTx::new(tx, Arc::new(AtomicU64::new(0)), Arc::new(AtomicU64::new(0)))
    }

    // ---- #191: register preparation runs before the dial ----

    #[test]
    fn register_prep_budget_sits_inside_the_control_plane_handshake_window() {
        // agentws handshakeTimeout is 15 s. The budget exists to NAME a slow runtime,
        // so it must trip before the peer would have given up on a same-window dial.
        assert!(REGISTER_PREP_BUDGET < Duration::from_secs(15));
        assert!(!register_prep_over_budget(Duration::from_millis(800)));
        assert!(register_prep_over_budget(Duration::from_secs(11)));
        // The field case: one 30 s inspect timeout.
        assert!(register_prep_over_budget(Duration::from_secs(30)));
    }

    /// A throwaway `ImageManager`: an empty state_path means `ImageManager::new`
    /// touches neither disk nor a docker daemon.
    /// A ReleaseManager pointed at paths that do not exist: `present()` is false,
    /// so nothing in these tests can reach a socket.
    fn test_release_mgr() -> Arc<ReleaseManager> {
        ReleaseManager::new("/nonexistent/updater.sock", "/nonexistent/results")
    }

    fn test_image_mgr() -> Arc<ImageManager> {
        ImageManager::new(ContainerRuntime::from_env(), String::new())
    }

    /// `app_to_container` is a field-by-field copy, and a missed field silently drops
    /// that knob at launch.
    #[test]
    fn app_to_container_carries_systempaths_unconfined() {
        let mut app = AppSpec {
            image: "quasar-desktop:latest".to_string(),
            ..Default::default()
        };
        app.systempaths_unconfined = true;
        let policy = MountPolicy::default();
        let container = app_to_container(app, &policy)
            .expect("no mounts ⇒ accepted")
            .expect("image set ⇒ Some");
        assert!(container.systempaths_unconfined);

        let app_default = AppSpec {
            image: "quasar-desktop:latest".to_string(),
            ..Default::default()
        };
        let container_default = app_to_container(app_default, &policy)
            .expect("no mounts ⇒ accepted")
            .expect("image set ⇒ Some");
        assert!(!container_default.systempaths_unconfined);
    }

    /// The assign is the boundary: a manifest mount this host has not allowed must
    /// fail before any container is spawned, and never reach `docker run -v`.
    #[test]
    fn app_to_container_refuses_a_disallowed_wire_mount() {
        let policy = MountPolicy::new("/var/lib/quasar/homes", "");
        let escape = AppSpec {
            image: "evil:latest".to_string(),
            mounts: vec!["/var/run:/hostrun".to_string()],
            ..Default::default()
        };
        assert!(app_to_container(escape, &policy).is_err());

        let ok = AppSpec {
            image: "quasar-steam:latest".to_string(),
            mounts: vec!["/var/lib/quasar/homes/alice/steam:/home/quasar".to_string()],
            ..Default::default()
        };
        let spec = app_to_container(ok, &policy).unwrap().unwrap();
        assert_eq!(
            spec.mounts,
            vec!["/var/lib/quasar/homes/alice/steam:/home/quasar".to_string()]
        );
    }

    #[test]
    fn binding_rejects_unknown_gpu_and_missing_render_node() {
        let mgr = manager_with(vec![gpu(0, "amd", None)]);
        let mut cfg = assignment_config(EncoderChoice::Va, "/dev/dri/renderD128");
        assert!(mgr
            .bind_assignment(9, &mut cfg)
            .unwrap_err()
            .to_string()
            .contains("absent"));
        assert!(mgr
            .bind_assignment(0, &mut cfg)
            .unwrap_err()
            .to_string()
            .contains("no reported render node"));
    }

    #[test]
    fn binding_rejects_hardware_software_and_mismatched_nodes() {
        let reported = "/dev/dri/by-path/pci-0000:04:00.0-render";
        let mgr = manager_with(vec![gpu(0, "amd", Some(reported))]);
        let mut software = assignment_config(EncoderChoice::Va, "software");
        assert!(mgr
            .bind_assignment(0, &mut software)
            .unwrap_err()
            .to_string()
            .contains("render_node=software"));
        let mut wrong = assignment_config(EncoderChoice::Va, "/dev/dri/renderD999");
        assert!(mgr
            .bind_assignment(0, &mut wrong)
            .unwrap_err()
            .to_string()
            .contains("does not match"));
    }

    #[test]
    fn binding_rejects_vendor_encoder_mismatch() {
        let node = "/dev/dri/by-path/pci-0000:04:00.0-render";
        let mgr = manager_with(vec![gpu(0, "nvidia", Some(node))]);
        let mut cfg = assignment_config(EncoderChoice::Va, node);
        assert!(mgr
            .bind_assignment(0, &mut cfg)
            .unwrap_err()
            .to_string()
            .contains("incompatible"));
    }

    #[test]
    fn binding_accepts_matching_va_and_software_diagnostic_mode() {
        let node = "/dev/dri/by-path/pci-0000:04:00.0-render";
        let mgr = manager_with(vec![gpu(0, "amd", Some(node))]);
        let mut va = assignment_config(EncoderChoice::Va, node);
        mgr.bind_assignment(0, &mut va).unwrap();
        assert_eq!(
            va.render_node,
            crate::session::settings::canonicalize_render_node(node)
        );
        let mut software = assignment_config(EncoderChoice::Openh264, "software");
        mgr.bind_assignment(0, &mut software).unwrap();
    }

    // QUASAR_RENDER_NODE unset (compose passes "") is unpinned: the assign
    // must adopt the scheduled GPU's node, matching schedulableBindingSQL's
    // empty-means-any-GPU rule on the control plane.
    #[test]
    fn binding_empty_render_node_adopts_scheduled_gpu() {
        let node = "/dev/dri/by-path/pci-0000:04:00.0-render";
        let mgr = manager_with(vec![gpu(0, "amd", Some(node))]);
        let mut cfg = assignment_config(EncoderChoice::Va, "");
        mgr.bind_assignment(0, &mut cfg).unwrap();
        assert_eq!(
            cfg.render_node,
            crate::session::settings::canonicalize_render_node(node)
        );
    }

    #[test]
    fn binding_accepts_nonzero_vulkan_via_render_node_context() {
        let node = "/dev/dri/by-path/pci-0000:05:00.0-render";
        let mgr = manager_with(vec![gpu(1, "amd", Some(node))]);
        let mut cfg = assignment_config(EncoderChoice::Vulkan, node);
        mgr.bind_assignment(1, &mut cfg).unwrap();
        assert_eq!(cfg.render_node, node);
    }

    #[test]
    fn config_update_applies_to_runtime_settings_no_ack() {
        let live_refs =
            std::sync::Arc::new(std::sync::Mutex::new(std::collections::HashSet::new()));
        let mut mgr = SessionManager::new(
            live_refs,
            HealthState::new(),
            Vec::new(),
            Vec::new(),
            String::new(),
            test_image_mgr(),
            test_release_mgr(),
        );
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let msg = ControlMsg::ConfigUpdate {
            source_policies: None,
            settings_delivery_id: None,
            settings: serde_json::json!({ "gop": 120, "abr_enabled": true, "encoder": "va" }),
            console_config: None,
        };
        let reply = mgr.handle_control(msg, &evt_tx, &diagnostic_sender());
        assert!(reply.is_none(), "config_update must not ack");
        assert_eq!(mgr.runtime_settings.gop, 120);
        // abr_enabled:true defers to abr_mode; the baseline Smooth is already non-Off,
        // so it stays rather than being forced to Protective.
        assert_eq!(mgr.runtime_settings.abr_mode, AbrMode::Smooth);
        assert_eq!(mgr.runtime_settings.encoder, EncoderChoice::Va);
    }

    #[test]
    fn config_update_rebaselines_cleared_keys_to_env() {
        // #194: a key omitted from a later push must revert to the ENV baseline, never
        // keeping a previously-pushed value or falling to the catalog default. That is
        // what preserves a host's QUASAR_ENCODER when no override is set.
        let live_refs =
            std::sync::Arc::new(std::sync::Mutex::new(std::collections::HashSet::new()));
        let mut mgr = SessionManager::new(
            live_refs,
            HealthState::new(),
            Vec::new(),
            Vec::new(),
            String::new(),
            test_image_mgr(),
            test_release_mgr(),
        );
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let env_encoder = crate::session::settings::RuntimeSettings::baseline().encoder;
        let env_gop = crate::session::settings::RuntimeSettings::baseline().gop;

        mgr.handle_control(
            ControlMsg::ConfigUpdate {
                source_policies: None,
                settings_delivery_id: None,
                settings: serde_json::json!({ "encoder": "va", "gop": 120 }),
                console_config: None,
            },
            &evt_tx,
            &diagnostic_sender(),
        );
        assert_eq!(mgr.runtime_settings.encoder, EncoderChoice::Va);
        assert_eq!(mgr.runtime_settings.gop, 120);

        // Encoder override cleared, only gop set.
        mgr.handle_control(
            ControlMsg::ConfigUpdate {
                source_policies: None,
                settings_delivery_id: None,
                settings: serde_json::json!({ "gop": 90 }),
                console_config: None,
            },
            &evt_tx,
            &diagnostic_sender(),
        );
        assert_eq!(
            mgr.runtime_settings.encoder, env_encoder,
            "cleared encoder must revert to env baseline, not stay va"
        );
        assert_eq!(mgr.runtime_settings.gop, 90);

        // Empty push → full env baseline.
        mgr.handle_control(
            ControlMsg::ConfigUpdate {
                source_policies: None,
                settings_delivery_id: None,
                settings: serde_json::json!({}),
                console_config: None,
            },
            &evt_tx,
            &diagnostic_sender(),
        );
        assert_eq!(mgr.runtime_settings.encoder, env_encoder);
        assert_eq!(mgr.runtime_settings.gop, env_gop);
    }

    #[test]
    fn console_only_config_update_preserves_runtime_settings() {
        let live_refs =
            std::sync::Arc::new(std::sync::Mutex::new(std::collections::HashSet::new()));
        let mut mgr = SessionManager::new(
            live_refs,
            HealthState::new(),
            Vec::new(),
            Vec::new(),
            String::new(),
            test_image_mgr(),
            test_release_mgr(),
        );
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        mgr.runtime_settings.encoder = EncoderChoice::Vulkan;
        mgr.runtime_settings.gop = 120;

        mgr.handle_control(
            ControlMsg::ConfigUpdate {
                source_policies: None,
                settings_delivery_id: None,
                settings: serde_json::Value::Null,
                console_config: None,
            },
            &evt_tx,
            &diagnostic_sender(),
        );

        assert_eq!(mgr.runtime_settings.encoder, EncoderChoice::Vulkan);
        assert_eq!(mgr.runtime_settings.gop, 120);
    }

    /// A probe that SUCCEEDED but measured nothing must reach the wire as `{}`, not as
    /// an omitted field: `{}` clears the stored hints, an absent key keeps them.
    /// Pinned because the tempting `.filter(|m| !m.is_empty())` or
    /// `skip_serializing_if` reads as tidying and silently breaks that case.
    #[test]
    fn a_probe_that_measured_nothing_clears_the_hint_rather_than_omitting_it() {
        let measured_nothing = Some(HostCodecReport {
            codecs: vec!["h264".to_string()],
            throughput: BTreeMap::new(),
        });
        assert_eq!(
            advertised_codec_throughput(&measured_nothing),
            Some(BTreeMap::new()),
            "a successful probe with no measurements must report Some({{}}) — an \
             explicit clear, not 'nothing to say'"
        );
        let json = serde_json::to_value(AgentMsg::Capacity {
            deployment_settings: None,
            config_policy_accepted_groups: None,
            config_policy_legacy_map_applied_id: None,

            source_preparation: None,
            host: crate::messages::HostCapacity {
                cpu_cores: 1,
                mem_mb: 1,
                storage: None,
                cpu_model: None,
            },
            gpus: vec![],
            gpu_detection: "ok".to_string(),
            gpu_detection_reason: None,
            console_capabilities: None,
            effective_settings: None,
            codecs: Some(vec!["h264".to_string()]),
            codec_throughput: advertised_codec_throughput(&measured_nothing),
            readiness: None,
        })
        .unwrap();
        assert_eq!(
            json["codec_throughput"],
            serde_json::json!({}),
            "an empty hint map must serialize as {{}}, not be omitted"
        );

        // A FAILED probe reports nothing at all, which is keep-if-absent.
        assert_eq!(advertised_codec_throughput(&None), None);
    }

    // ---- #301: only a codec proven on the current stack is advertised above H.264 ----

    fn plan_all(_: EncoderChoice, _: &str) -> BTreeSet<Codec> {
        BTreeSet::from([Codec::H264, Codec::H265, Codec::Av1])
    }

    fn plan_none(_: EncoderChoice, _: &str) -> BTreeSet<Codec> {
        BTreeSet::new()
    }

    fn exclude_none(_: &crate::messages::GpuCapacity) -> BTreeSet<Codec> {
        BTreeSet::new()
    }

    /// A GPU host whose flat host-level probe says all three codecs: the flat set must
    /// never reach the wire on its own.
    fn codec_mgr(
        gpus: Vec<crate::messages::GpuCapacity>,
        plan: fn(EncoderChoice, &str) -> BTreeSet<Codec>,
    ) -> SessionManager {
        let mut mgr = manager_with(gpus);
        mgr.agent_image_identity = "sha256:agent".into();
        mgr.codec_layers = CodecLayers {
            plan,
            excluded: exclude_none,
        };
        mgr.host_codec_report = Some(HostCodecReport {
            codecs: vec!["h264".into(), "h265".into(), "av1".into()],
            throughput: BTreeMap::new(),
        });
        mgr
    }

    /// A codec-probe pass as the orchestrator reports it, stamped with the stack `mgr`
    /// has right now.
    fn prove(mgr: &mut SessionManager, gpu: i32, codec: crate::host_probe::ProbeCodec) {
        let evidence = probe_inputs(mgr).evidence_stamp(gpu);
        crate::host_probe::orchestrator::apply(
            &mut mgr.readiness,
            &mut mgr.codec_evidence,
            crate::host_probe::orchestrator::ReportUpdate::Record {
                target: crate::host_probe::ProbeTarget::codec(gpu, codec),
                outcome: crate::host_probe::outcome::ProbeOutcome::Pass {
                    summary: "ok".into(),
                },
                observed_at: SystemTime::now(),
                evidence,
            },
        );
    }

    fn wire(codecs: &[&str]) -> Option<Vec<String>> {
        Some(codecs.iter().map(|c| c.to_string()).collect())
    }

    #[test]
    fn a_codec_pass_is_advertised_only_on_the_stack_it_was_proven_on() {
        use crate::host_probe::ProbeCodec;
        let gpus = vec![
            gpu(0, "nvidia", Some("/dev/dri/renderD128")),
            gpu(1, "nvidia", Some("/dev/dri/renderD129")),
        ];
        let mut mgr = codec_mgr(gpus, plan_all);
        assert_eq!(
            mgr.advertised_codecs(),
            wire(&["h264"]),
            "nothing proven yet"
        );
        prove(&mut mgr, 0, ProbeCodec::H265);
        assert_eq!(mgr.advertised_codecs(), wire(&["h264", "h265"]));

        // Settings: an encoder flip, before the scheduler's `Forget` has landed.
        let proven_on = mgr.runtime_settings.encoder;
        mgr.runtime_settings.encoder = if proven_on == EncoderChoice::Nvenc {
            EncoderChoice::Vulkan
        } else {
            EncoderChoice::Nvenc
        };
        assert_eq!(mgr.advertised_codecs(), wire(&["h264"]), "encoder");
        mgr.runtime_settings.encoder = proven_on;
        mgr.runtime_settings.zerocopy = !mgr.runtime_settings.zerocopy;
        assert_eq!(mgr.advertised_codecs(), wire(&["h264"]), "zerocopy");
        mgr.runtime_settings.zerocopy = !mgr.runtime_settings.zerocopy;
        assert_eq!(
            mgr.advertised_codecs(),
            wire(&["h264", "h265"]),
            "same stack"
        );

        // Driver identity.
        mgr.gpu_inventory[0].driver_identity = Some("nvidia:610.57.04".into());
        assert_eq!(mgr.advertised_codecs(), wire(&["h264"]), "driver");
        mgr.gpu_inventory[0].driver_identity = None;

        // GPU identity under the same index.
        mgr.gpu_inventory[0].render_node = Some("/dev/dri/renderD130".into());
        assert_eq!(mgr.advertised_codecs(), wire(&["h264"]), "GPU identity");
    }

    #[test]
    fn a_reused_gpu_index_does_not_inherit_the_vanished_gpus_pass() {
        use crate::host_probe::ProbeCodec;
        let mut mgr = codec_mgr(
            vec![
                gpu(0, "nvidia", Some("/dev/dri/renderD128")),
                gpu(1, "nvidia", Some("/dev/dri/renderD129")),
            ],
            plan_all,
        );
        prove(&mut mgr, 0, ProbeCodec::Av1);
        assert_eq!(mgr.advertised_codecs(), wire(&["h264", "av1"]));
        // GPU 0 vanishes; the old GPU 1 re-enumerates as index 0.
        mgr.gpu_inventory = vec![gpu(0, "nvidia", Some("/dev/dri/renderD129"))];
        assert_eq!(mgr.advertised_codecs(), wire(&["h264"]));
    }

    #[test]
    fn a_gpu_host_without_a_per_gpu_plan_never_falls_back_to_the_flat_set() {
        use crate::host_probe::ProbeCodec;
        let mut mgr = codec_mgr(vec![gpu(0, "amd", Some("/dev/dri/renderD128"))], plan_none);
        prove(&mut mgr, 0, ProbeCodec::H265);
        assert_eq!(
            mgr.advertised_codecs(),
            wire(&["h264"]),
            "a GPU with no plan still has the floor, and nothing above it"
        );
        // No GPU at all: H.264 only, whatever the flat probe says.
        mgr.gpu_inventory.clear();
        assert_eq!(mgr.advertised_codecs(), wire(&["h264"]));
    }

    #[test]
    fn gst_init_failure_on_a_gpu_host_sends_h264_not_an_absent_field() {
        use crate::host_probe::ProbeCodec;
        let mut mgr = codec_mgr(
            vec![gpu(0, "nvidia", Some("/dev/dri/renderD128"))],
            plan_all,
        );
        prove(&mut mgr, 0, ProbeCodec::H265);
        mgr.host_codec_report = None;
        assert_eq!(mgr.advertised_codecs(), wire(&["h264"]));
        // Per-GPU consistency (#302 review): gst-init failure is "no codec knowledge
        // beyond the floor", not "no knowledge at all" — a usable GPU still gets an
        // explicit ["h264"], never an omitted field.
        let mut gpus = vec![gpu(0, "nvidia", Some("/dev/dri/renderD128"))];
        apply_gpu_codecs(&mut gpus, &mgr.gpu_codec_sets());
        assert_eq!(gpus[0].codecs, Some(vec!["h264".to_string()]));
    }

    #[test]
    fn a_pinned_out_gpu_and_an_excluded_codec_drop_out_of_the_union() {
        use crate::host_probe::ProbeCodec;
        let mut pinned_out = gpu(1, "amd", Some("/dev/dri/renderD129"));
        pinned_out.encode_slots_total = 0;
        let mut mgr = codec_mgr(
            vec![gpu(0, "nvidia", Some("/dev/dri/renderD128")), pinned_out],
            plan_all,
        );
        mgr.codec_layers.excluded = |_| BTreeSet::from([Codec::Av1]);
        prove(&mut mgr, 0, ProbeCodec::Av1);
        prove(&mut mgr, 1, ProbeCodec::H265);
        assert_eq!(mgr.advertised_codecs(), wire(&["h264"]));
        let mut pinned_out_again = gpu(1, "amd", Some("/dev/dri/renderD129"));
        pinned_out_again.encode_slots_total = 0;
        let mut gpus = vec![
            gpu(0, "nvidia", Some("/dev/dri/renderD128")),
            pinned_out_again,
        ];
        apply_gpu_codecs(&mut gpus, &mgr.gpu_codec_sets());
        assert_eq!(
            gpus[1].codecs,
            Some(Vec::<String>::new()),
            "the pinned-out GPU reports [] on the wire, not an omitted field"
        );
    }

    // ---- #302: capacity.gpus[].codecs, and the assign-time belt ----

    /// `apply_gpu_codecs` and the host union it derives from must read the SAME
    /// per-GPU sets: a usable GPU's wire field always carries h264, a zero-slot GPU
    /// reports an explicit `[]` (never omits the field), and the union is exactly
    /// what the per-GPU fields say.
    #[test]
    fn apply_gpu_codecs_stamps_each_gpu_including_an_empty_set_for_a_zero_slot_gpu() {
        let mut gpus = vec![
            gpu(0, "nvidia", Some("/dev/dri/renderD128")),
            gpu(1, "amd", Some("/dev/dri/renderD129")),
        ];
        gpus[1].encode_slots_total = 0;
        let sets = vec![
            (0, BTreeSet::from([Codec::H264, Codec::H265])),
            (1, BTreeSet::new()),
        ];
        apply_gpu_codecs(&mut gpus, &sets);
        assert_eq!(
            gpus[0].codecs,
            Some(vec!["h264".to_string(), "h265".to_string()])
        );
        assert_eq!(
            gpus[1].codecs,
            Some(Vec::<String>::new()),
            "a zero-slot GPU reports an explicit empty codec set, never an omitted field"
        );
        assert_eq!(
            host_codecs_from_sets(&sets),
            vec!["h264".to_string(), "h265".to_string()],
            "the host union is derived from the same per-GPU sets, not recomputed"
        );
    }

    /// `SessionManager::gpu_codec_sets` is the single computation `advertised_codecs`
    /// (the host union) and a capacity send's `apply_gpu_codecs` (the per-GPU field)
    /// both read — proving a codec on one GPU must not appear on the other's set even
    /// though it appears in the union.
    #[test]
    fn gpu_codec_sets_are_per_gpu_and_the_union_matches_advertised_codecs() {
        use crate::host_probe::ProbeCodec;
        let mut mgr = codec_mgr(
            vec![
                gpu(0, "nvidia", Some("/dev/dri/renderD128")),
                gpu(1, "nvidia", Some("/dev/dri/renderD129")),
            ],
            plan_all,
        );
        prove(&mut mgr, 0, ProbeCodec::H265);
        prove(&mut mgr, 1, ProbeCodec::Av1);
        let sets = mgr.gpu_codec_sets();
        let gpu0 = sets.iter().find(|(i, _)| *i == 0).unwrap();
        let gpu1 = sets.iter().find(|(i, _)| *i == 1).unwrap();
        assert_eq!(gpu0.1, BTreeSet::from([Codec::H264, Codec::H265]));
        assert_eq!(gpu1.1, BTreeSet::from([Codec::H264, Codec::Av1]));
        assert_eq!(
            mgr.advertised_codecs(),
            wire(&["h264", "h265", "av1"]),
            "the host union is the per-GPU sets' union"
        );
    }

    fn session_assign_msg_codec(session_id: &str, gpu_index: i32, codec: &str) -> ControlMsg {
        serde_json::from_value(serde_json::json!({
            "type": "session_assign",
            "id": "c1",
            "session_id": session_id,
            "gpu_index": gpu_index,
            "stream": {"width": 1920, "height": 1080, "fps": 60,
                "bitrate_kbps": 15000, "h264_profile": "constrained-baseline",
                "codec": codec}
        }))
        .unwrap()
    }

    /// The belt: `session_assign` must refuse a codec the bound GPU has not (yet, or
    /// ever) proven, even though nothing upstream of it caught the mistake.
    #[test]
    fn assign_refuses_a_codec_outside_the_bound_gpus_set() {
        let mut mgr = codec_mgr(
            vec![gpu(0, "nvidia", Some("/dev/dri/renderD128"))],
            plan_all,
        );
        // Nothing proven yet: GPU 0's set is h264-only, even though the flat probe
        // and the plan both claim h265/av1.
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let reply = mgr.handle_control(
            session_assign_msg_codec("s1", 0, "h265"),
            &evt_tx,
            &diagnostic_sender(),
        );
        match reply {
            Some(AgentMsg::Ack {
                ok: false, error, ..
            }) => {
                assert!(
                    error.unwrap().contains("h265"),
                    "the ack error should name the refused codec"
                );
            }
            other => panic!("expected ack{{ok:false}}, got {other:?}"),
        }
    }

    /// The refusal decision: codec, GPU and session in the log line, the ack error
    /// naming the codec; a GPU absent from the sets proves nothing.
    #[test]
    fn assign_codec_refusal_refuses_a_codec_outside_the_gpus_set() {
        let h264_only = vec![(0, BTreeSet::from([Codec::H264]))];
        let refusal = assign_codec_refusal("s1", 0, Codec::H265, &h264_only)
            .expect("h265 is outside GPU 0's h264-only set");
        assert!(refusal.log.contains("session s1 "), "{refusal:?}");
        assert!(refusal.log.contains("gpu=0 codec=h265"), "{refusal:?}");
        assert_eq!(
            refusal.ack_error,
            "gpu 0 cannot encode h265: not in its current codec set"
        );
        assert!(assign_codec_refusal("s1", 7, Codec::Av1, &h264_only).is_some());
    }

    /// H.264 is the floor, and a codec in the GPU's set passes.
    #[test]
    fn assign_codec_refusal_passes_h264_and_codecs_in_the_set() {
        let sets = vec![(0, BTreeSet::from([Codec::H264, Codec::H265]))];
        assert_eq!(assign_codec_refusal("s1", 0, Codec::H264, &sets), None);
        assert_eq!(assign_codec_refusal("s1", 9, Codec::H264, &[]), None);
        assert_eq!(assign_codec_refusal("s1", 0, Codec::H265, &sets), None);
    }

    /// The handler takes the refusal path: its ack{ok:false} error is exactly the
    /// decision's, so the refusal's `warn!` (token checked below) is what it logged.
    #[test]
    fn assign_refusal_ack_is_the_refusal_decisions() {
        let mut mgr = codec_mgr(
            vec![gpu(0, "nvidia", Some("/dev/dri/renderD128"))],
            plan_all,
        );
        let expected = assign_codec_refusal("s1", 0, Codec::H265, &mgr.gpu_codec_sets())
            .expect("GPU 0 has proven nothing yet");
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        match mgr.handle_control(
            session_assign_msg_codec("s1", 0, "h265"),
            &evt_tx,
            &diagnostic_sender(),
        ) {
            Some(AgentMsg::Ack {
                ok: false, error, ..
            }) => assert_eq!(error, Some(expected.ack_error)),
            other => panic!("expected ack{{ok:false}}, got {other:?}"),
        }
    }

    /// An operator greps the refusal by token (`.claude/rules/agent-logging.md`). The
    /// token must be a literal (`tests/log_convention.rs`), so this checks the one
    /// `warn!` that logs the refusal in source rather than capturing `tracing` events,
    /// whose process-global interest cache made a capture flake under parallel tests.
    #[test]
    fn assign_refusal_logs_the_codec_not_in_gpu_set_token() {
        // Split literals so this test's own text never matches what it searches for.
        let logged = concat!("refusal.", "log);");
        let site = concat!(
            "warn!(token = \"assign-codec-not-in-gpu-set\", \"{}\", ",
            "refusal.",
            "log);"
        );
        let source = include_str!("agent.rs");
        assert_eq!(
            source.matches(logged).count(),
            1,
            "exactly one site logs the refusal"
        );
        assert!(
            source.contains(site),
            "the refusal's warn! must be `{site}`"
        );
    }

    /// A codec-probe pass on the bound GPU lifts the belt.
    #[test]
    fn assign_accepts_a_codec_once_the_bound_gpu_has_proven_it() {
        use crate::host_probe::ProbeCodec;
        let mut mgr = codec_mgr(
            vec![gpu(0, "nvidia", Some("/dev/dri/renderD128"))],
            plan_all,
        );
        prove(&mut mgr, 0, ProbeCodec::H265);
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let reply = mgr.handle_control(
            session_assign_msg_codec("s1", 0, "h265"),
            &evt_tx,
            &diagnostic_sender(),
        );
        assert!(
            matches!(reply, Some(AgentMsg::Ack { ok: true, .. })),
            "{reply:?}"
        );
    }

    /// H.264 is exempt from the belt: it is the floor of every usable GPU's set by
    /// construction, so a plan/probe gap must never refuse it.
    #[test]
    fn assign_never_refuses_h264_even_with_no_plan_and_nothing_proven() {
        let mut mgr = codec_mgr(
            vec![gpu(0, "nvidia", Some("/dev/dri/renderD128"))],
            plan_none,
        );
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let reply = mgr.handle_control(session_assign_msg("s1", 0), &evt_tx, &diagnostic_sender());
        assert!(
            matches!(reply, Some(AgentMsg::Ack { ok: true, .. })),
            "{reply:?}"
        );
    }

    #[tokio::test]
    async fn the_capacity_refresh_notifies_the_scheduler_of_an_inventory_change() {
        use crate::host_probe::decision::Event;
        use crate::host_probe::orchestrator::ProbeHandle;
        let mut mgr = codec_mgr(
            vec![gpu(0, "nvidia", Some("/dev/dri/renderD128"))],
            plan_all,
        );
        let (handle, mut rx) = ProbeHandle::detached();
        mgr.probe_handle = Some(handle);
        mgr.notify_probe_inputs();
        next_probe_event(&mut rx).await;

        mgr.adopt_inventory(
            vec![gpu(0, "nvidia", Some("/dev/dri/renderD128"))],
            Vec::new(),
        );
        mgr.notify_probe_inputs_if_changed();
        assert!(
            tokio::time::timeout(Duration::from_millis(150), rx.recv())
                .await
                .is_err(),
            "an unchanged inventory is not re-notified"
        );

        mgr.adopt_inventory(
            vec![gpu(0, "nvidia", Some("/dev/dri/renderD129"))],
            Vec::new(),
        );
        mgr.notify_probe_inputs_if_changed();
        match next_probe_event(&mut rx).await {
            Event::InputsObserved(inputs) => {
                assert_eq!(
                    inputs.gpus.get(&0).map(String::as_str),
                    Some("/dev/dri/renderD129")
                );
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn config_update_encoder_flip_marks_host_codecs_stale() {
        // The advertised hosts.codecs set must track the EFFECTIVE encoder, not the env
        // one — a config_update overlay flips it live. The connect loop re-probes when
        // this reports stale.
        let live_refs =
            std::sync::Arc::new(std::sync::Mutex::new(std::collections::HashSet::new()));
        let mut mgr = SessionManager::new(
            live_refs,
            HealthState::new(),
            Vec::new(),
            Vec::new(),
            String::new(),
            test_image_mgr(),
            test_release_mgr(),
        );
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let env_encoder = crate::session::settings::RuntimeSettings::baseline().encoder;

        assert!(mgr.host_codecs_stale());
        mgr.probed_encoder = Some(env_encoder); // simulate the startup probe cache

        // Console-only push (settings null) leaves runtime settings — still fresh.
        mgr.handle_control(
            ControlMsg::ConfigUpdate {
                source_policies: None,
                settings_delivery_id: None,
                settings: serde_json::Value::Null,
                console_config: None,
            },
            &evt_tx,
            &diagnostic_sender(),
        );
        assert!(!mgr.host_codecs_stale());

        // Empty overrides rebaseline to env — same encoder, still fresh.
        mgr.handle_control(
            ControlMsg::ConfigUpdate {
                source_policies: None,
                settings_delivery_id: None,
                settings: serde_json::json!({}),
                console_config: None,
            },
            &evt_tx,
            &diagnostic_sender(),
        );
        assert!(!mgr.host_codecs_stale());

        // Flip the encoder to something other than the env baseline — stale.
        let flip = if env_encoder == EncoderChoice::Va {
            "nvenc"
        } else {
            "va"
        };
        mgr.handle_control(
            ControlMsg::ConfigUpdate {
                source_policies: None,
                settings_delivery_id: None,
                settings: serde_json::json!({ "encoder": flip }),
                console_config: None,
            },
            &evt_tx,
            &diagnostic_sender(),
        );
        assert!(mgr.host_codecs_stale());

        // After the loop re-probes it records the new encoder — fresh again, and a
        // repeat push of the same override stays fresh (no redundant re-probe).
        mgr.probed_encoder = Some(mgr.runtime_settings.encoder);
        assert!(!mgr.host_codecs_stale());
        mgr.handle_control(
            ControlMsg::ConfigUpdate {
                source_policies: None,
                settings_delivery_id: None,
                settings: serde_json::json!({ "encoder": flip }),
                console_config: None,
            },
            &evt_tx,
            &diagnostic_sender(),
        );
        assert!(!mgr.host_codecs_stale());
    }

    fn running_handle(
        topology: crate::messages::VideoTopology,
    ) -> (RunningHandle, Arc<AtomicBool>) {
        let stop = Arc::new(AtomicBool::new(false));
        let (sig_tx, _sig_rx) = std::sync::mpsc::channel();
        let (swap_tx, _swap_rx) = std::sync::mpsc::channel();
        let (display_tx, _display_rx) = std::sync::mpsc::channel();
        let (capture_tx, _capture_rx) = std::sync::mpsc::channel();
        (
            RunningHandle {
                stop: stop.clone(),
                sig: sig_tx,
                swap: swap_tx,
                display: display_tx,
                capture: capture_tx,
                capture_slot: CaptureSlot::new(),
                display_state: crate::session::runner::SessionDisplayState::new((1920, 1080), true),
                metrics: Arc::new(SessionMetrics::new("off", 60)),
                home_refs: Vec::new(),
                video_topology: topology,
                thread: None,
                finished_seen_at: None,
                gpu_index: 0,
                codec: crate::session::Codec::H264,
                reached_running: true,
                pending_home_terminal: None,
            },
            stop,
        )
    }

    // ── session-display-update dispatch (agent-api.md) ───────────────────────

    /// A handle whose display receiver the caller keeps alive — the shared
    /// `running_handle` drops its receivers, which makes every send fail.
    fn display_handle(
        launch: (i32, i32),
    ) -> (
        RunningHandle,
        std::sync::mpsc::Receiver<DisplayUpdateRequest>,
    ) {
        display_handle_with(launch, true)
    }

    /// `external_resize_supported` explicit — the Vulkan / local-only arm.
    fn display_handle_with(
        launch: (i32, i32),
        external_resize_supported: bool,
    ) -> (
        RunningHandle,
        std::sync::mpsc::Receiver<DisplayUpdateRequest>,
    ) {
        let (sig_tx, _sig_rx) = std::sync::mpsc::channel();
        let (swap_tx, _swap_rx) = std::sync::mpsc::channel();
        let (display_tx, display_rx) = std::sync::mpsc::channel();
        let (capture_tx, _capture_rx) = std::sync::mpsc::channel();
        (
            RunningHandle {
                stop: Arc::new(AtomicBool::new(false)),
                sig: sig_tx,
                swap: swap_tx,
                display: display_tx,
                capture: capture_tx,
                capture_slot: CaptureSlot::new(),
                display_state: crate::session::runner::SessionDisplayState::new(
                    launch,
                    external_resize_supported,
                ),
                metrics: Arc::new(SessionMetrics::new("off", 60)),
                home_refs: Vec::new(),
                video_topology: crate::messages::VideoTopology::StreamOnly,
                thread: None,
                finished_seen_at: None,
                gpu_index: 0,
                codec: crate::session::Codec::H264,
                reached_running: true,
                pending_home_terminal: None,
            },
            display_rx,
        )
    }

    fn display_update(
        session_id: &str,
        w: Option<i32>,
        h: Option<i32>,
        s: Option<f64>,
    ) -> ControlMsg {
        ControlMsg::SessionDisplayUpdate {
            id: "c1".to_string(),
            session_id: session_id.to_string(),
            render_width: w,
            render_height: h,
            ui_scale: s,
            stream_width: None,
            stream_height: None,
        }
    }

    /// A `session_display_update` carrying only the external (stream) half.
    fn stream_update(session_id: &str, w: Option<i32>, h: Option<i32>) -> ControlMsg {
        ControlMsg::SessionDisplayUpdate {
            id: "c1".to_string(),
            session_id: session_id.to_string(),
            render_width: None,
            render_height: None,
            ui_scale: None,
            stream_width: w,
            stream_height: h,
        }
    }

    #[test]
    fn display_update_routes_to_the_runner_and_acks_true() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let (h, display_rx) = display_handle((1920, 1080));
        mgr.running.insert("s1".to_string(), h);

        let reply = mgr
            .handle_control(
                display_update("s1", Some(1280), Some(720), Some(1.5)),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("display update always acks");
        assert!(matches!(reply, AgentMsg::Ack { ok: true, .. }), "{reply:?}");
        assert_eq!(
            display_rx.try_recv().unwrap(),
            DisplayUpdateRequest {
                render_width: Some(1280),
                render_height: Some(720),
                ui_scale: Some(1.5),
                stream: None,
            }
        );
    }

    #[test]
    fn display_update_rejection_is_a_no_op_with_a_prefixed_error() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let (h, display_rx) = display_handle((1920, 1080));
        mgr.running.insert("s1".to_string(), h);

        // Above the pinned stream size: rejected, nothing routed, session untouched.
        let reply = mgr
            .handle_control(
                display_update("s1", Some(3840), Some(2160), None),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("rejection still acks");
        match reply {
            AgentMsg::Ack { ok, error, .. } => {
                assert!(!ok);
                let e = error.expect("rejection carries a reason");
                assert!(e.starts_with("display_update_rejected: "), "{e}");
            }
            other => panic!("wrong reply: {other:?}"),
        }
        assert!(
            display_rx.try_recv().is_err(),
            "nothing must reach the runner"
        );
        assert!(mgr.running.contains_key("s1"), "the session stays running");

        // Unknown session ⇒ same rejected-is-a-no-op shape.
        let reply = mgr
            .handle_control(
                display_update("nope", Some(1280), Some(720), None),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("unknown session still acks");
        assert!(
            matches!(reply, AgentMsg::Ack { ok: false, .. }),
            "{reply:?}"
        );
    }

    // ── adaptive external resolution: stream_* dispatch ──────────────────────

    fn ack_error(reply: &AgentMsg) -> String {
        match reply {
            AgentMsg::Ack {
                ok: false,
                error: Some(e),
                ..
            } => e.clone(),
            other => panic!("expected a rejection ack, got {other:?}"),
        }
    }

    #[test]
    fn stream_update_routes_a_rung_and_folds_the_handle_state_forward() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let (h, display_rx) = display_handle((1920, 1080));
        mgr.running.insert("s1".to_string(), h);

        let reply = mgr
            .handle_control(
                stream_update("s1", Some(1280), Some(720)),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("stream update always acks");
        assert!(matches!(reply, AgentMsg::Ack { ok: true, .. }), "{reply:?}");
        assert_eq!(display_rx.try_recv().unwrap().stream, Some((1280, 720)));
        // The render ceiling is unaffected: it is, and stays, the LAUNCH size.
        let st = mgr.running["s1"].display_state;
        assert_eq!(st.external, (1280, 720));
        assert_eq!(st.launch, (1920, 1080));

        // A render size above the new external size is accepted (encoder downsamples)…
        let reply = mgr
            .handle_control(
                display_update("s1", Some(1600), Some(900), None),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("acks");
        assert!(matches!(reply, AgentMsg::Ack { ok: true, .. }), "{reply:?}");
        assert_eq!(
            mgr.running["s1"].display_state.render,
            Some((1600, 900)),
            "render is bounded by launch, not by external"
        );
        // …and one above the LAUNCH size is still rejected.
        let reply = mgr
            .handle_control(
                display_update("s1", Some(2560), Some(1440), None),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("acks");
        assert!(ack_error(&reply).contains("above the session launch size"));
    }

    #[test]
    fn stream_update_is_acked_false_on_an_encoder_without_a_live_resize_lever() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        // No scale stage, so the answer must be a real rejection rather than an accepted
        // command that changes nothing.
        let (h, display_rx) = display_handle_with((1920, 1080), false);
        mgr.running.insert("s1".to_string(), h);

        let reply = mgr
            .handle_control(
                stream_update("s1", Some(1280), Some(720)),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("rejection still acks");
        assert_eq!(
            ack_error(&reply),
            "display_update_rejected: encoder does not support live resize"
        );
        assert!(
            display_rx.try_recv().is_err(),
            "nothing must reach the runner"
        );
        // The render/scale half of the SAME session is unaffected by the capability.
        let reply = mgr
            .handle_control(
                display_update("s1", Some(1280), Some(720), None),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("acks");
        assert!(matches!(reply, AgentMsg::Ack { ok: true, .. }), "{reply:?}");
    }

    #[test]
    fn stream_update_rejects_a_non_rung_and_a_half_sent_pair() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let (h, display_rx) = display_handle((1920, 1080));
        mgr.running.insert("s1".to_string(), h);

        let reply = mgr
            .handle_control(
                stream_update("s1", Some(1366), Some(768)),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("acks");
        assert!(ack_error(&reply).contains("not a rung"));

        // Both-or-neither.
        let reply = mgr
            .handle_control(
                stream_update("s1", Some(1280), None),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("acks");
        assert!(ack_error(&reply).contains("must be sent together"));

        assert!(
            display_rx.try_recv().is_err(),
            "nothing must reach the runner"
        );
        assert_eq!(
            mgr.running["s1"].display_state.external,
            (1920, 1080),
            "a rejected update must not move the handle's state"
        );
    }

    // Independent axes: a stream step carries ONLY the stream fields, in either
    // direction. The encode-side scale stage downsamples the compositor's launch-size
    // framebuffer to the external rung, so the app never sees a mode change.
    #[test]
    fn a_stream_step_never_rewrites_an_explicit_render_size() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let (h, display_rx) = display_handle((1920, 1080));
        mgr.running.insert("s1".to_string(), h);

        mgr.handle_control(
            display_update("s1", Some(1600), Some(900), None),
            &evt_tx,
            &diagnostic_sender(),
        );
        assert_eq!(
            display_rx.try_recv().unwrap().render_width,
            Some(1600),
            "the render request itself"
        );

        // Step the stream DOWN below the render size: legal, and render-silent.
        let reply = mgr
            .handle_control(
                stream_update("s1", Some(1280), Some(720)),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("acks");
        assert!(matches!(reply, AgentMsg::Ack { ok: true, .. }), "{reply:?}");
        let routed = display_rx.try_recv().unwrap();
        assert_eq!(routed.stream, Some((1280, 720)));
        assert_eq!(
            (routed.render_width, routed.render_height),
            (None, None),
            "an external step must carry no render fields at all"
        );
        let st = mgr.running["s1"].display_state;
        assert_eq!(st.external, (1280, 720));
        assert_eq!(st.render, Some((1600, 900)), "render is untouched");

        // …and back UP: still render-silent, still 1600x900.
        mgr.handle_control(
            stream_update("s1", Some(1920), Some(1080)),
            &evt_tx,
            &diagnostic_sender(),
        );
        let up = display_rx.try_recv().unwrap();
        assert_eq!(up.stream, Some((1920, 1080)));
        assert_eq!((up.render_width, up.render_height), (None, None));
        let st = mgr.running["s1"].display_state;
        assert_eq!(st.external, (1920, 1080));
        assert_eq!(st.render, Some((1600, 900)));
    }

    // The default (`render == None`) case: a stream round trip synthesises nothing, so
    // the compositor is never touched and the mirror stays `None`.
    #[test]
    fn a_stream_round_trip_leaves_the_default_render_alone() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let (h, display_rx) = display_handle((1920, 1080));
        mgr.running.insert("s1".to_string(), h);

        for (w, h_) in [(1280, 720), (1920, 1080)] {
            let reply = mgr
                .handle_control(
                    stream_update("s1", Some(w), Some(h_)),
                    &evt_tx,
                    &diagnostic_sender(),
                )
                .expect("acks");
            assert!(matches!(reply, AgentMsg::Ack { ok: true, .. }), "{reply:?}");
            let routed = display_rx.try_recv().unwrap();
            assert_eq!(routed.stream, Some((w, h_)));
            assert_eq!(
                (routed.render_width, routed.render_height),
                (None, None),
                "no render field may be synthesised at {w}x{h_}"
            );
        }
        let st = mgr.running["s1"].display_state;
        assert_eq!(st.external, (1920, 1080));
        assert_eq!(st.render, None);
    }

    #[test]
    fn console_disable_stops_local_only_sessions_only() {
        // `console_config.enabled=false` must stop every running local-only session —
        // they have no signaling and no session-API presence, so this is the only
        // remote lever. Stream-only sessions are untouched.
        let live_refs =
            std::sync::Arc::new(std::sync::Mutex::new(std::collections::HashSet::new()));
        let mut mgr = SessionManager::new(
            live_refs,
            HealthState::new(),
            Vec::new(),
            Vec::new(),
            String::new(),
            test_image_mgr(),
            test_release_mgr(),
        );
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);

        let (console_h, console_stop) = running_handle(crate::messages::VideoTopology::LocalOnly);
        let (stream_h, stream_stop) = running_handle(crate::messages::VideoTopology::StreamOnly);
        mgr.running.insert("console-sess".to_string(), console_h);
        mgr.running.insert("stream-sess".to_string(), stream_h);

        // Enabled push: nothing stops.
        mgr.handle_control(
            ControlMsg::ConfigUpdate {
                source_policies: None,
                settings_delivery_id: None,
                settings: serde_json::Value::Null,
                console_config: Some(
                    serde_json::from_value(serde_json::json!({ "enabled": true })).unwrap(),
                ),
            },
            &evt_tx,
            &diagnostic_sender(),
        );
        assert!(!console_stop.load(Ordering::Relaxed));
        assert!(!stream_stop.load(Ordering::Relaxed));

        // Disable push: the local-only session stops, the stream session doesn't.
        mgr.handle_control(
            ControlMsg::ConfigUpdate {
                source_policies: None,
                settings_delivery_id: None,
                settings: serde_json::Value::Null,
                console_config: Some(
                    serde_json::from_value(serde_json::json!({ "enabled": false })).unwrap(),
                ),
            },
            &evt_tx,
            &diagnostic_sender(),
        );
        assert!(console_stop.load(Ordering::Relaxed));
        assert!(!stream_stop.load(Ordering::Relaxed));
    }

    // ── #409: panic containment + heartbeat reconciliation ───────────────────

    fn manager_with_runner(runner: RunnerFn) -> (SessionManager, LiveRefs) {
        let live_refs: LiveRefs = Arc::new(Mutex::new(HashSet::new()));
        let mut mgr = SessionManager::new(
            live_refs.clone(),
            HealthState::new(),
            Vec::new(),
            Vec::new(),
            String::new(),
            test_image_mgr(),
            test_release_mgr(),
        );
        mgr.runner = runner;
        // SessionManager::new seeds these from the process env; pin them so tests stay hermetic.
        mgr.runtime_settings.encoder = crate::session::EncoderChoice::Openh264;
        mgr.runtime_settings.render_node = "software".to_string();
        (mgr, live_refs)
    }

    /// Assign + start a session through the real `handle_control` path.
    fn start_seam_session(
        mgr: &mut SessionManager,
        session_id: &str,
        evt_tx: &mpsc::Sender<(String, SessionEvent)>,
    ) {
        mgr.pending.insert(
            session_id.to_string(),
            PendingAssignment {
                cfg: assignment_config(EncoderChoice::Openh264, "software"),
                assigned_at: Instant::now(),
                preparation: None,
                gpu_index: 0,
            },
        );
        mgr.handle_control(
            ControlMsg::SessionStart {
                id: "cmd-1".to_string(),
                session_id: session_id.to_string(),
            },
            evt_tx,
            &diagnostic_sender(),
        );
    }

    /// Bounded `try_recv` poll, never `blocking_recv`: a regression that stops emitting
    /// the terminal event must fail the test, not hang the suite.
    fn recv_event_within(
        rx: &mut mpsc::Receiver<(String, SessionEvent)>,
        within: Duration,
    ) -> Option<(String, SessionEvent)> {
        let deadline = Instant::now() + within;
        loop {
            match rx.try_recv() {
                Ok(v) => return Some(v),
                Err(_) if Instant::now() < deadline => std::thread::sleep(Duration::from_millis(5)),
                Err(_) => return None,
            }
        }
    }

    fn wait_for_finished_thread(mgr: &SessionManager, session_id: &str) {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            let finished = mgr
                .running
                .get(session_id)
                .and_then(|h| h.thread.as_ref())
                .map(|t| t.is_finished())
                .unwrap_or(false);
            if finished {
                return;
            }
            assert!(
                Instant::now() < deadline,
                "runner thread for {session_id} never finished"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    #[test]
    fn capable_terminal_waits_for_source_absence_and_preserves_home_refs() {
        let directory = tempfile::tempdir().unwrap();
        let (mut mgr, live_refs) = manager_with_runner(Arc::new(|_, _, _, _, _, _, _, _, _, _| {}));
        let mut ledger = crate::home_cleanup::HomeCleanupLedger::open_after_startup_cleanup(
            directory.path().join("ledger"),
        )
        .unwrap();
        assert!(ledger.record_active("held").unwrap());
        mgr.home_cleanup = Some(ledger);
        let clean = Arc::new(AtomicBool::new(false));
        let observed = clean.clone();
        mgr.home_source_retire = Some(Arc::new(move |_| observed.load(Ordering::SeqCst)));
        let (tx, _rx) = mpsc::channel(8);
        start_seam_session(&mut mgr, "held", &tx);
        wait_for_finished_thread(&mgr, "held");
        mgr.running
            .get_mut("held")
            .unwrap()
            .home_refs
            .push("managed-home".into());
        mgr.add_live_refs(&["managed-home".into()]);
        assert!(mgr
            .prove_home_terminal("held", SessionEvent::Failed("runner failed".into()))
            .is_none());
        assert!(mgr.running.contains_key("held"));
        assert!(live_refs.lock().unwrap().contains("managed-home"));
        assert_eq!(mgr.home_cleanup.as_ref().unwrap().state("held"), None);
        clean.store(true, Ordering::SeqCst);
        let reports = mgr.reconcile(Instant::now(), Duration::ZERO, Duration::from_secs(60));
        assert!(
            matches!(reports.as_slice(), [AgentMsg::SessionState { state, .. }] if state == "failed")
        );
        assert!(!mgr.running.contains_key("held"));
        assert!(!live_refs.lock().unwrap().contains("managed-home"));
        assert_eq!(
            mgr.home_cleanup.as_ref().unwrap().state("held"),
            Some(crate::home_cleanup::TerminalKind::Failed)
        );
    }

    #[test]
    fn repeated_stop_retires_a_lost_assign_before_terminal_and_blocks_reuse() {
        let directory = tempfile::tempdir().unwrap();
        let mut mgr = manager_with(Vec::new());
        mgr.home_cleanup = Some(
            crate::home_cleanup::HomeCleanupLedger::open_after_startup_cleanup(
                directory.path().join("ledger"),
            )
            .unwrap(),
        );
        let (tx, _rx) = mpsc::channel(8);
        for _ in 0..2 {
            let ack = mgr.handle_control(
                ControlMsg::SessionStop {
                    id: "stop".into(),
                    session_id: "lost-assign".into(),
                    reason: "error".into(),
                },
                &tx,
                &diagnostic_sender(),
            );
            assert!(matches!(ack, Some(AgentMsg::Ack { ok: true, .. })));
            assert!(matches!(mgr.home_cleanup_reports.pop(),
                Some(AgentMsg::SessionState { state, .. }) if state == "stopped"));
            assert_eq!(
                mgr.home_cleanup.as_ref().unwrap().state("lost-assign"),
                Some(crate::home_cleanup::TerminalKind::Stopped)
            );
        }
        let refused = mgr.handle_control(
            session_assign_msg("lost-assign", 0),
            &tx,
            &diagnostic_sender(),
        );
        assert!(matches!(refused, Some(AgentMsg::Ack { ok: false, .. })));
    }

    #[test]
    fn verified_assignment_preparation_requires_a_local_image_at_launch() {
        use std::io::{Read, Write};
        let directory = tempfile::tempdir().unwrap();
        let socket_path = directory.path().join("engine.sock");
        let listener = std::os::unix::net::UnixListener::bind(&socket_path).unwrap();
        listener.set_nonblocking(true).unwrap();
        let engine = std::thread::spawn(move || {
            for (path, body) in [
                (
                    "/version",
                    r#"{"Version":"28.0.0","ApiVersion":"1.48","MinAPIVersion":"1.40"}"#,
                ),
                (
                    "/v1.48/images/test/json",
                    r#"{"Id":"sha256:fixture","Size":100}"#,
                ),
            ] {
                let until = Instant::now() + Duration::from_secs(3);
                let mut socket = loop {
                    match listener.accept() {
                        Ok((socket, _)) => break socket,
                        Err(e)
                            if e.kind() == std::io::ErrorKind::WouldBlock
                                && Instant::now() < until =>
                        {
                            std::thread::sleep(Duration::from_millis(5))
                        }
                        Err(e) => panic!("image preparation did not inspect: {e}"),
                    }
                };
                socket
                    .set_read_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                let mut request = Vec::new();
                while !request.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    socket.read_exact(&mut byte).unwrap();
                    request.push(byte[0]);
                }
                assert!(
                    String::from_utf8_lossy(&request).starts_with(&format!("GET {path} HTTP/1.1"))
                );
                write!(
                    socket,
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                )
                .unwrap();
            }
        });
        let mut runtime_config = crate::runtime::RuntimeConfig::unix(socket_path);
        runtime_config.image_state_path = Some(directory.path().join("operations"));
        let runtime = crate::runtime::RuntimeClient::new(runtime_config).unwrap();
        let observed = Arc::new(AtomicBool::new(false));
        let result = observed.clone();
        let (mut mgr, _refs) =
            manager_with_runner(Arc::new(move |_, cfg, _, _, _, _, _, _, _, _| {
                result.store(cfg.container.unwrap().require_local_image, Ordering::SeqCst);
            }));
        let mut cfg = assignment_config(EncoderChoice::Openh264, "software");
        cfg.container = Some(ContainerSpec {
            image: "test".into(),
            ..Default::default()
        });
        mgr.pending.insert(
            "prepared".into(),
            PendingAssignment {
                cfg,
                assigned_at: Instant::now(),
                preparation: Some(runtime.ensure_image("test", Duration::from_secs(2))),
                gpu_index: 0,
            },
        );
        let (tx, _rx) = mpsc::channel(8);
        mgr.handle_control(
            ControlMsg::SessionStart {
                id: "start".into(),
                session_id: "prepared".into(),
            },
            &tx,
            &diagnostic_sender(),
        );
        wait_for_finished_thread(&mgr, "prepared");
        engine.join().unwrap();
        assert!(
            observed.load(Ordering::SeqCst),
            "prepared assignment could implicitly pull through CLI"
        );
    }

    #[test]
    fn image_preparation_failure_prevents_the_session_runner_from_launching() {
        let called = Arc::new(AtomicBool::new(false));
        let observed = called.clone();
        let (mut mgr, _refs) =
            manager_with_runner(Arc::new(move |_, _, _, _, _, _, _, _, _, _| {
                observed.store(true, Ordering::SeqCst);
            }));
        let directory = tempfile::tempdir().unwrap();
        let runtime = crate::runtime::RuntimeClient::new(crate::runtime::RuntimeConfig::unix(
            directory.path().join("missing.sock"),
        ))
        .unwrap();
        mgr.pending.insert(
            "preparing".into(),
            PendingAssignment {
                cfg: assignment_config(EncoderChoice::Openh264, "software"),
                assigned_at: Instant::now(),
                preparation: Some(runtime.ensure_image("test", Duration::from_secs(2))),
                gpu_index: 0,
            },
        );
        let (tx, mut rx) = mpsc::channel(8);
        mgr.handle_control(
            ControlMsg::SessionStart {
                id: "start".into(),
                session_id: "preparing".into(),
            },
            &tx,
            &diagnostic_sender(),
        );
        let event = recv_event_within(&mut rx, Duration::from_secs(3));
        wait_for_finished_thread(&mgr, "preparing");
        assert!(!called.load(Ordering::SeqCst));
        assert!(
            matches!(event,Some((_,SessionEvent::Failed(message))) if message.contains("image preparation failed"))
        );
    }

    /// #128: a new connection must not disturb the sessions the agent carried
    /// across the outage, and must drop the assignments it did not.
    #[test]
    fn begin_connection_clears_pending_but_keeps_running_sessions() {
        // A runner that returns immediately. The handle stays in `running` until
        // the event loop reconciles it, which this test never runs, so the map is
        // still populated for the assertion. Deliberately NOT a sleeping runner:
        // that leaks a live thread for the rest of the suite and it takes the
        // agent's exclusive container-ownership lease with it, which fails an
        // unrelated test.
        let (mut mgr, _live_refs) = manager_with_runner(Arc::new(
            |_sid, _cfg, _evt, _diag, _stop, _sig, _swap, _display, _capture, _metrics| {},
        ));
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(8);
        start_seam_session(&mut mgr, "survivor", &evt_tx);
        assert_eq!(mgr.running.len(), 1, "setup: one running session");

        // An assignment that never received its session_start.
        mgr.pending.insert(
            "orphan".to_string(),
            PendingAssignment {
                cfg: assignment_config(EncoderChoice::Openh264, "software"),
                assigned_at: Instant::now(),
                preparation: None,
                gpu_index: 0,
            },
        );
        assert_eq!(mgr.pending.len(), 1, "setup: one pending assignment");

        mgr.begin_connection(Vec::new(), Vec::new());

        assert!(
            mgr.running.contains_key("survivor"),
            "a running session must survive the reconnect: that is the whole point of #128"
        );
        assert!(
            mgr.pending.is_empty(),
            "a pending assignment belongs to the old connection; the control plane re-drives it"
        );
    }

    /// #128: the grace window is a knob, and 0 restores the old stop-on-drop
    /// behaviour rather than meaning "no wait at all by accident".
    #[test]
    fn session_grace_reads_its_knob() {
        assert_eq!(
            session_grace_from(None),
            Duration::from_secs(DEFAULT_SESSION_GRACE_SECS)
        );

        assert_eq!(session_grace_from(Some("5")), Duration::from_secs(5));

        assert!(
            session_grace_from(Some("0")).is_zero(),
            "0 must disable the hold"
        );

        // Garbage falls back rather than disabling the hold silently.
        assert_eq!(
            session_grace_from(Some("not-a-number")),
            Duration::from_secs(DEFAULT_SESSION_GRACE_SECS)
        );
    }

    /// A panicking runner must produce a terminal `Failed` carrying the panic payload.
    /// Without `catch_unwind` the thread dies silently and the slot leaks for the life
    /// of the connection.
    #[test]
    fn panicking_runner_emits_failed_with_the_panic_payload() {
        let (mut mgr, live_refs) = manager_with_runner(Arc::new(
            |_sid, _cfg, _evt, _diag, _stop, _sig, _swap, _display, _capture, _metrics| {
                panic!("set_property on an absent element property")
            },
        ));
        let (evt_tx, mut evt_rx) = mpsc::channel::<(String, SessionEvent)>(8);
        start_seam_session(&mut mgr, "panic-sess", &evt_tx);
        mgr.running
            .get_mut("panic-sess")
            .unwrap()
            .home_refs
            .push("home-panic".to_string());
        mgr.add_live_refs(&["home-panic".to_string()]);

        let (sid, event) = recv_event_within(&mut evt_rx, Duration::from_secs(5))
            .expect("a panicking runner must still emit a terminal event");
        assert_eq!(sid, "panic-sess");
        let reason = match &event {
            SessionEvent::Failed(reason) => reason.clone(),
            other => panic!("expected Failed, got {other:?}"),
        };
        assert!(
            reason.contains("panicked") && reason.contains("absent element property"),
            "the panic payload must reach the control plane: {reason}"
        );

        let msg = mgr.on_event(&sid, event);
        match msg {
            AgentMsg::SessionState { state, .. } => assert_eq!(state, "failed"),
            other => panic!("expected session_state, got {other:?}"),
        }
        assert!(mgr.running.is_empty(), "the session slot must be released");
        assert!(
            live_refs.lock().unwrap().is_empty(),
            "live_refs must be released or the #175 GC reaper stays blocked on this home"
        );
    }

    /// The backstop: a runner that ends emitting nothing at all must be reaped by the
    /// sweep, but only after the grace window.
    #[test]
    fn reconcile_reaps_a_runner_that_ended_without_a_terminal_event() {
        let (mut mgr, live_refs) = manager_with_runner(Arc::new(
            |_sid, _cfg, _evt, _diag, _stop, _sig, _swap, _display, _capture, _metrics| {},
        ));
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(8);
        start_seam_session(&mut mgr, "ghost-sess", &evt_tx);
        mgr.running
            .get_mut("ghost-sess")
            .unwrap()
            .home_refs
            .push("home-ghost".to_string());
        mgr.add_live_refs(&["home-ghost".to_string()]);
        wait_for_finished_thread(&mgr, "ghost-sess");

        // Finished but within the grace window: the normal terminal path finishes its
        // thread just before the loop drains the event and must not be reaped from
        // under it.
        let first = mgr.reconcile(Instant::now(), RUNNER_REAP_GRACE, PENDING_ASSIGNMENT_TTL);
        assert!(first.is_empty(), "the grace window must protect the race");
        assert!(mgr.running.contains_key("ghost-sess"));

        // Past the grace: reaped, with a terminal state for the control plane.
        let second = mgr.reconcile(Instant::now(), Duration::ZERO, PENDING_ASSIGNMENT_TTL);
        assert_eq!(second.len(), 1);
        match &second[0] {
            AgentMsg::SessionState {
                session_id,
                state,
                error,
                ..
            } => {
                assert_eq!(session_id, "ghost-sess");
                assert_eq!(state, "failed");
                assert!(error.is_some());
            }
            other => panic!("expected session_state, got {other:?}"),
        }
        assert!(mgr.running.is_empty());
        assert!(live_refs.lock().unwrap().is_empty());
    }

    /// A live runner must never be reaped, however many sweeps run.
    #[test]
    fn reconcile_leaves_a_live_runner_alone() {
        let hold = Arc::new(AtomicBool::new(false));
        let hold2 = hold.clone();
        let (mut mgr, _live_refs) = manager_with_runner(Arc::new(
            move |_sid, _cfg, _evt, _diag, _stop, _sig, _swap, _display, _capture, _metrics| {
                while !hold2.load(Ordering::Relaxed) {
                    std::thread::sleep(Duration::from_millis(5));
                }
            },
        ));
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(8);
        start_seam_session(&mut mgr, "live-sess", &evt_tx);
        for _ in 0..3 {
            assert!(mgr
                .reconcile(Instant::now(), Duration::ZERO, PENDING_ASSIGNMENT_TTL)
                .is_empty());
        }
        assert!(mgr.running.contains_key("live-sess"));
        hold.store(true, Ordering::Relaxed);
    }

    /// An assign past the control plane's `assignAckTimeout` (which fails the session
    /// WITHOUT dispatching a stop) otherwise pins a full `SessionConfig` for the
    /// connection's lifetime.
    #[test]
    fn reconcile_ages_out_an_orphaned_pending_assignment() {
        let (mut mgr, _live_refs) = manager_with_runner(default_runner());
        mgr.pending.insert(
            "orphan-sess".to_string(),
            PendingAssignment {
                cfg: assignment_config(EncoderChoice::Openh264, "software"),
                assigned_at: Instant::now(),
                preparation: None,
                gpu_index: 0,
            },
        );
        mgr.reconcile(Instant::now(), RUNNER_REAP_GRACE, PENDING_ASSIGNMENT_TTL);
        assert_eq!(
            mgr.pending.len(),
            1,
            "a fresh assignment must survive the sweep"
        );
        mgr.reconcile(Instant::now(), RUNNER_REAP_GRACE, Duration::ZERO);
        assert!(mgr.pending.is_empty(), "an aged-out assignment is dropped");
    }

    // ── session_capture dispatch ─────────────────────────────────────────────

    /// A handle whose capture receiver the caller keeps alive — the shared
    /// `running_handle` drops its receivers, which makes every send fail.
    fn capture_handle(
        topology: crate::messages::VideoTopology,
    ) -> (RunningHandle, std::sync::mpsc::Receiver<CaptureRequest>) {
        let (sig_tx, _sig_rx) = std::sync::mpsc::channel();
        let (swap_tx, _swap_rx) = std::sync::mpsc::channel();
        let (display_tx, _display_rx) = std::sync::mpsc::channel();
        let (capture_tx, capture_rx) = std::sync::mpsc::channel();
        (
            RunningHandle {
                stop: Arc::new(AtomicBool::new(false)),
                sig: sig_tx,
                swap: swap_tx,
                display: display_tx,
                capture: capture_tx,
                capture_slot: CaptureSlot::new(),
                display_state: crate::session::runner::SessionDisplayState::new((1920, 1080), true),
                metrics: Arc::new(SessionMetrics::new("off", 60)),
                home_refs: Vec::new(),
                video_topology: topology,
                thread: None,
                finished_seen_at: None,
                gpu_index: 0,
                codec: crate::session::Codec::H264,
                reached_running: true,
                pending_home_terminal: None,
            },
            capture_rx,
        )
    }

    fn session_capture(session_id: &str, kind: &str) -> ControlMsg {
        serde_json::from_value(serde_json::json!({
            "type": "session_capture",
            "id": "cmd-1",
            "session_id": session_id,
            "capture_id": "cap-1",
            "kind": kind,
            "budget": { "max_bytes": 262144, "max_ms": 10000 }
        }))
        .expect("a well-formed session_capture")
    }

    fn nack_error(reply: &AgentMsg) -> String {
        match reply {
            AgentMsg::Ack {
                ok: false,
                error: Some(e),
                ..
            } => e.clone(),
            other => panic!("expected a nack, got {other:?}"),
        }
    }

    #[test]
    fn capture_routes_to_the_runner_and_acks_true() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let (h, capture_rx) = capture_handle(crate::messages::VideoTopology::StreamOnly);
        mgr.running.insert("s1".to_string(), h);

        let reply = mgr
            .handle_control(
                session_capture("s1", "pipeline_dot"),
                &evt_tx,
                &diagnostic_sender(),
            )
            .expect("session_capture always acks");
        assert!(matches!(reply, AgentMsg::Ack { ok: true, .. }), "{reply:?}");

        let routed = capture_rx
            .try_recv()
            .expect("the request reached the runner");
        assert_eq!(routed.capture_id, "cap-1");
        assert_eq!(routed.kind, crate::messages::CaptureKind::PipelineDot);
        assert_eq!(routed.budget.max_bytes, 262_144);
        assert_eq!(routed.budget.max_ms, 10_000);
        assert!(
            routed.slot.is_busy(),
            "the ack reserved the slot, so the runner receives it already held"
        );
    }

    /// `ok:true` means ARMED: nothing changes at ack time and no result rides the ack —
    /// the `diag.*` trace event carries it later.
    #[test]
    fn capture_is_single_flight_and_the_second_is_acked_busy() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let (h, capture_rx) = capture_handle(crate::messages::VideoTopology::StreamOnly);
        mgr.running.insert("s1".to_string(), h);

        let first = mgr
            .handle_control(
                session_capture("s1", "burst_stats"),
                &evt_tx,
                &diagnostic_sender(),
            )
            .unwrap();
        assert!(matches!(first, AgentMsg::Ack { ok: true, .. }));

        let second = mgr
            .handle_control(
                session_capture("s1", "pipeline_dot"),
                &evt_tx,
                &diagnostic_sender(),
            )
            .unwrap();
        assert_eq!(nack_error(&second), "busy");
        assert!(capture_rx.try_recv().is_ok(), "the first one was routed");
        assert!(
            capture_rx.try_recv().is_err(),
            "a refused capture must never be queued behind the running one"
        );
    }

    #[test]
    fn capture_for_an_unknown_kind_acks_unknown_kind_and_routes_nothing() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let (h, capture_rx) = capture_handle(crate::messages::VideoTopology::StreamOnly);
        let slot = h.capture_slot.clone();
        mgr.running.insert("s1".to_string(), h);

        let reply = mgr
            .handle_control(
                session_capture("s1", "bitstream_dump"),
                &evt_tx,
                &diagnostic_sender(),
            )
            .unwrap();
        assert_eq!(nack_error(&reply), "unknown_kind");
        assert!(capture_rx.try_recv().is_err());
        assert!(
            !slot.is_busy(),
            "a refused capture must leave the session capturable"
        );
    }

    /// A local-only session has no encode pipeline, so every kind is `unsupported`.
    #[test]
    fn capture_on_a_local_only_session_acks_unsupported() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let (h, capture_rx) = capture_handle(crate::messages::VideoTopology::LocalOnly);
        mgr.running.insert("s1".to_string(), h);

        let reply = mgr
            .handle_control(
                session_capture("s1", "encoder_props"),
                &evt_tx,
                &diagnostic_sender(),
            )
            .unwrap();
        assert_eq!(nack_error(&reply), "unsupported");
        assert!(capture_rx.try_recv().is_err());
    }

    #[test]
    fn capture_for_a_session_that_is_not_running_acks_no_such_session() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);

        let reply = mgr
            .handle_control(
                session_capture("nope", "pipeline_dot"),
                &evt_tx,
                &diagnostic_sender(),
            )
            .unwrap();
        assert_eq!(nack_error(&reply), "no_such_session");
    }

    /// A runner that died between the topology read and the send must hand the slot
    /// back, or the session is permanently `busy`.
    #[test]
    fn capture_with_a_dead_runner_acks_and_returns_the_slot() {
        let mut mgr = manager_with(Vec::new());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let (h, capture_rx) = capture_handle(crate::messages::VideoTopology::StreamOnly);
        let slot = h.capture_slot.clone();
        mgr.running.insert("s1".to_string(), h);
        drop(capture_rx);

        let reply = mgr
            .handle_control(
                session_capture("s1", "pipeline_dot"),
                &evt_tx,
                &diagnostic_sender(),
            )
            .unwrap();
        assert_eq!(nack_error(&reply), "no_such_session");
        assert!(
            !slot.is_busy(),
            "the slot must not leak on a failed hand-off"
        );
    }

    /// #530: locks in the tokio fact the bug rests on — `recv()` on a channel whose
    /// senders are all dropped resolves `Ready(None)` on every poll, never `Pending`.
    /// That made `gpu_fault_rx` a ~99%-of-one-core busy spin on every production host.
    /// If a tokio upgrade ever parks instead, this fails and flags that
    /// `recv_or_disabled` is no longer load-bearing.
    #[tokio::test]
    async fn closed_channel_recv_never_yields_pending() {
        let (tx, mut rx) = mpsc::channel::<()>(1);
        drop(tx);
        const BUDGET: usize = 1000;
        let start = std::time::Instant::now();
        for _ in 0..BUDGET {
            assert_eq!(rx.recv().await, None);
        }
        // 1000 immediately-ready polls take microseconds; the 50 ms bound only rules
        // out an accidental await point, not scheduling noise.
        assert!(
            start.elapsed() < Duration::from_millis(50),
            "closed-channel recv() no longer resolves instantly — the #530 spin \
             mechanism assumption changed; revisit recv_or_disabled's need"
        );
    }

    /// Once an arm built on `recv_or_disabled` sees `Ready(None)` and the caller nulls
    /// its `Option`, it must contribute exactly one resolution to the loop's whole
    /// lifetime. Under a paused clock (virtual time advances only when every polled
    /// future is `Pending`), a regression that re-spins the arm hangs this test.
    #[tokio::test(start_paused = true)]
    async fn select_arm_disables_and_never_refires_after_close() {
        let (tx, rx) = mpsc::channel::<u32>(1);
        drop(tx); // gpu_kmsg::spawn's default path: the only sender is gone
        let mut rx = Some(rx);
        let mut resolutions = 0u32;
        let mut ticks = 0u32;
        let mut ticker = tokio::time::interval(Duration::from_millis(1));
        while ticks < 200 {
            tokio::select! {
                v = recv_or_disabled(&mut rx) => {
                    resolutions += 1;
                    if v.is_none() {
                        rx = None;
                    }
                }
                _ = ticker.tick() => {
                    ticks += 1;
                }
            }
        }
        assert_eq!(
            resolutions, 1,
            "the closed-channel arm resolved more than once — it is being \
             re-polled instead of staying disabled, i.e. it is spinning (#530)"
        );
    }

    /// Same proof, unbounded-channel variant (`gpu_fault_rx`'s actual type).
    #[tokio::test(start_paused = true)]
    async fn select_arm_disables_and_never_refires_after_close_unbounded() {
        let (tx, rx) = mpsc::unbounded_channel::<u32>();
        drop(tx);
        let mut rx = Some(rx);
        let mut resolutions = 0u32;
        let mut ticks = 0u32;
        let mut ticker = tokio::time::interval(Duration::from_millis(1));
        while ticks < 200 {
            tokio::select! {
                v = recv_or_disabled_unbounded(&mut rx) => {
                    resolutions += 1;
                    if v.is_none() {
                        rx = None;
                    }
                }
                _ = ticker.tick() => {
                    ticks += 1;
                }
            }
        }
        assert_eq!(
            resolutions, 1,
            "the closed-channel arm resolved more than once — it is being \
             re-polled instead of staying disabled, i.e. it is spinning (#530)"
        );
    }

    fn session_assign_msg(session_id: &str, gpu_index: i32) -> ControlMsg {
        serde_json::from_value(serde_json::json!({
            "type": "session_assign",
            "id": "c1",
            "session_id": session_id,
            "gpu_index": gpu_index,
            "stream": {"width": 1920, "height": 1080, "fps": 60,
                "bitrate_kbps": 15000, "h264_profile": "constrained-baseline"}
        }))
        .unwrap()
    }

    async fn next_probe_event(
        rx: &mut mpsc::UnboundedReceiver<crate::host_probe::decision::Event>,
    ) -> crate::host_probe::decision::Event {
        tokio::time::timeout(Duration::from_secs(5), rx.recv())
            .await
            .expect("no probe event")
            .expect("probe handle's forwarding task ended")
    }

    #[tokio::test]
    async fn a_session_assign_sends_launch_arrived_and_a_rejection_corrects_the_live_set() {
        use crate::host_probe::decision::Event;
        use crate::host_probe::orchestrator::ProbeHandle;

        let (mut mgr, _live_refs) = manager_with_runner(default_runner());
        let (handle, mut rx) = ProbeHandle::detached();
        mgr.probe_handle = Some(handle);
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);

        // gpu_index 9 is absent from this manager's (empty) inventory, so
        // `bind_assignment` rejects it — after `abort_any_warmup`/`launch_arrived`,
        // which is the point: a probe must be told the GPU is wanted before the
        // config below can possibly fail.
        let reply = mgr.handle_control(
            session_assign_msg("rejected", 9),
            &evt_tx,
            &diagnostic_sender(),
        );
        assert!(matches!(reply, Some(AgentMsg::Ack { ok: false, .. })));

        assert_eq!(
            next_probe_event(&mut rx).await,
            Event::LaunchArrived { gpu: 9 }
        );
        match next_probe_event(&mut rx).await {
            Event::SessionsChanged { live_gpus, .. } => {
                assert!(
                    !live_gpus.contains(&9),
                    "a rejected assign must not leave the GPU marked live: {live_gpus:?}"
                );
            }
            other => panic!("{other:?}"),
        }
    }

    #[tokio::test]
    async fn an_accepted_assign_marks_its_gpu_live() {
        use crate::host_probe::decision::Event;
        use crate::host_probe::orchestrator::ProbeHandle;
        use crate::messages::GpuCapacity;

        let (mut mgr, _live_refs) = manager_with_runner(default_runner());
        mgr.gpu_inventory = vec![GpuCapacity {
            index: 0,
            vendor: "software".into(),
            model: "test".into(),
            vram_mb_total: 0,
            encode_slots_total: 0,
            render_node: None,
            device_path: None,
            driver_identity: None,
            codecs: None,
        }];
        let (handle, mut rx) = ProbeHandle::detached();
        mgr.probe_handle = Some(handle);
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);

        // openh264 (this manager's default) never needs a render node, so
        // `bind_assignment` accepts GPU 0 with no further configuration.
        let reply = mgr.handle_control(
            session_assign_msg("accepted", 0),
            &evt_tx,
            &diagnostic_sender(),
        );
        assert!(matches!(reply, Some(AgentMsg::Ack { ok: true, .. })));

        assert_eq!(
            next_probe_event(&mut rx).await,
            Event::LaunchArrived { gpu: 0 }
        );
        match next_probe_event(&mut rx).await {
            Event::SessionsChanged { live_gpus, .. } => {
                assert!(live_gpus.contains(&0), "{live_gpus:?}");
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn probe_inputs_lists_every_gpu_and_reacts_only_to_media_relevant_settings() {
        let (mut mgr, _live_refs) = manager_with_runner(default_runner());
        mgr.gpu_inventory = vec![
            crate::messages::GpuCapacity {
                index: 0,
                vendor: "amd".into(),
                model: "test".into(),
                vram_mb_total: 0,
                encode_slots_total: 0,
                render_node: Some("/dev/dri/renderD128".into()),
                device_path: None,
                driver_identity: Some("amd:1.2.3".into()),
                codecs: None,
            },
            crate::messages::GpuCapacity {
                index: 1,
                vendor: "amd".into(),
                model: "test2".into(),
                vram_mb_total: 0,
                encode_slots_total: 0,
                render_node: Some("/dev/dri/renderD129".into()),
                device_path: None,
                driver_identity: Some("amd:1.2.3".into()),
                codecs: None,
            },
        ];
        mgr.agent_image_identity = "sha256:agent".into();
        let inputs = probe_inputs(&mgr);
        assert_eq!(inputs.agent_image, "sha256:agent");
        assert_eq!(inputs.gpus.len(), 2);
        assert!(
            inputs.codecs.is_empty(),
            "no registry (gst::init never succeeded), no codec probes"
        );

        // #301: codec-probe targets come from each GPU's OWN registry plan, not a
        // host-wide set — GPU 0 and GPU 1 get different plans here to prove it.
        mgr.host_codec_report = Some(HostCodecReport::default());
        mgr.codec_layers = CodecLayers {
            plan: |_, node| match node {
                "/dev/dri/renderD128" => BTreeSet::from([Codec::H264, Codec::H265, Codec::Av1]),
                "/dev/dri/renderD129" => BTreeSet::from([Codec::H264, Codec::H265]),
                _ => BTreeSet::new(),
            },
            excluded: exclude_none,
        };
        let with_codecs = probe_inputs(&mgr);
        assert_eq!(
            with_codecs.codecs.get(&0),
            Some(&std::collections::BTreeSet::from([
                crate::host_probe::ProbeCodec::H265,
                crate::host_probe::ProbeCodec::Av1,
            ]))
        );
        assert_eq!(
            with_codecs.codecs.get(&1),
            Some(&std::collections::BTreeSet::from([
                crate::host_probe::ProbeCodec::H265
            ]))
        );

        // An h264-only plan has no above-floor target.
        mgr.codec_layers.plan = |_, _| BTreeSet::from([Codec::H264]);
        assert!(probe_inputs(&mgr).codecs.is_empty());
        mgr.host_codec_report = None;
        assert_eq!(
            inputs.gpus.get(&0).map(String::as_str),
            Some("/dev/dri/renderD128")
        );
        assert_eq!(
            inputs.gpus.get(&1).map(String::as_str),
            Some("/dev/dri/renderD129")
        );

        let baseline = probe_inputs(&mgr).settings;
        mgr.runtime_settings.encoder = EncoderChoice::Vulkan;
        let after_encoder_change = probe_inputs(&mgr).settings;
        assert_ne!(
            baseline, after_encoder_change,
            "an encoder change must be visible to the probe scheduler"
        );

        mgr.runtime_settings.home_root = "/mnt/unrelated".into();
        let after_unrelated_change = probe_inputs(&mgr).settings;
        assert_eq!(
            after_encoder_change, after_unrelated_change,
            "home_root does not select the media path and must not trigger a re-probe"
        );
    }

    #[test]
    fn a_probe_handle_of_none_leaves_session_assign_behaviour_unchanged() {
        // Every pre-existing test builds a `SessionManager` with no probe
        // wiring, so this is really a proof that `note_session_count` and the two
        // `launch_arrived` call sites are no-ops rather than panics with no handle.
        let (mut mgr, _live_refs) = manager_with_runner(default_runner());
        assert!(mgr.probe_handle.is_none());
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let reply = mgr.handle_control(
            session_assign_msg("no-probe", 9),
            &evt_tx,
            &diagnostic_sender(),
        );
        assert!(matches!(reply, Some(AgentMsg::Ack { ok: false, .. })));
    }

    // ---- RH05 #336: a typed next-session apply reaches the next launch ----

    fn typed_offer_msg(offer: crate::policy::Offer) -> ControlMsg {
        ControlMsg::ConfigPolicyOffer {
            attempt_id: offer.attempt_id,
            host_id: offer.host_id,
            boot_incarnation: offer.boot_incarnation,
            connection_incarnation: offer.connection_incarnation,
            group: offer.group,
            revision: offer.revision,
            content_sha256: offer.content_sha256,
            scope: offer.scope,
            expires_at: offer.expires_at,
            prerequisites_sha256: offer.prerequisites_sha256,
            prerequisites: offer.prerequisites,
            settings: offer.settings,
            resolved_settings: offer.resolved_settings,
        }
    }

    /// Owns `group` on a durable temp journal and applies `value` to it through
    /// `handle_control`, as the connection loop does.
    fn apply_typed(
        mgr: &mut SessionManager,
        dir: &std::path::Path,
        group: &str,
        value: serde_json::Value,
    ) -> Option<AgentMsg> {
        use crate::policy::test_support::{explicit, owned_agent};
        let agent = owned_agent(dir, &[group], &mut mgr.runtime_settings);
        let offer = explicit(&agent, "attempt", "1", group, value);
        mgr.policy_agent = Some(agent);
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        mgr.handle_control(typed_offer_msg(offer), &evt_tx, &diagnostic_sender())
    }

    /// agent-api.md §RH05: a `deployment_baseline_changed` rejection is
    /// preceded on the same ordered socket by a fresh capacity baseline, so
    /// the control plane never reads the rejection against stale evidence.
    #[tokio::test]
    async fn a_baseline_changed_rejection_follows_a_fresh_capacity_baseline() {
        use crate::policy::test_support::{offer, owned_agent};
        let (mut mgr, _live_refs) = manager_with_runner(default_runner());
        let dir = tempfile::tempdir().unwrap();
        let agent = owned_agent(dir.path(), &["gop"], &mut mgr.runtime_settings);
        let current = mgr.deployment_baseline.deployment_map()["gop"]
            .as_u64()
            .unwrap();
        let stale = offer(
            &agent,
            "stale",
            "1",
            "gop",
            serde_json::json!({"source":"deployment"}),
            serde_json::json!(current + 30),
        );
        mgr.policy_agent = Some(agent);
        let (evt_tx, _evt_rx) = mpsc::channel::<(String, SessionEvent)>(1);
        let reply = mgr
            .handle_control(typed_offer_msg(stale), &evt_tx, &diagnostic_sender())
            .unwrap();

        let mut wire: Vec<Message> = Vec::new();
        let mut sink = (&mut wire).sink_map_err(
            |never: std::convert::Infallible| -> tokio_tungstenite::tungstenite::Error {
                match never {}
            },
        );
        send_control_reply(&mut sink, &mut mgr, reply)
            .await
            .unwrap();
        let sent: Vec<serde_json::Value> = wire
            .iter()
            .map(|m| serde_json::from_str(m.to_text().unwrap()).unwrap())
            .collect();
        let types: Vec<&str> = sent.iter().map(|m| m["type"].as_str().unwrap()).collect();
        assert_eq!(types, ["capacity", "config_policy_state"], "{sent:?}");
        assert_eq!(
            sent[0]["deployment_settings"]["gop"],
            serde_json::json!(current),
            "the capacity carries the agent's current baseline"
        );
        assert_eq!(sent[1]["phase"], "failed");
        assert_eq!(sent[1]["error"], "deployment_baseline_changed");
    }

    fn applied(reply: &Option<AgentMsg>) -> bool {
        matches!(reply, Some(AgentMsg::ConfigPolicyState { phase, .. }) if phase == "applied")
    }

    /// `zerocopy` stays next-session, but it is a host-probe input: a typed
    /// change must withdraw codecs proven under the old value and ask the
    /// scheduler for a fresh probe, exactly as a legacy `config_update` does.
    #[tokio::test]
    async fn a_typed_zerocopy_apply_withdraws_probe_proven_codecs_and_requests_a_probe() {
        use crate::host_probe::decision::Event;
        use crate::host_probe::orchestrator::ProbeHandle;
        use crate::host_probe::ProbeCodec;
        let mut mgr = codec_mgr(
            vec![gpu(0, "nvidia", Some("/dev/dri/renderD128"))],
            plan_all,
        );
        let (handle, mut rx) = ProbeHandle::detached();
        mgr.probe_handle = Some(handle);
        mgr.notify_probe_inputs();
        next_probe_event(&mut rx).await;
        prove(&mut mgr, 0, ProbeCodec::H265);
        assert_eq!(mgr.advertised_codecs(), wire(&["h264", "h265"]));

        let dir = tempfile::tempdir().unwrap();
        let flipped = !mgr.runtime_settings.zerocopy;
        let reply = apply_typed(&mut mgr, dir.path(), "zerocopy", serde_json::json!(flipped));
        assert!(applied(&reply), "{reply:?}");
        assert_eq!(mgr.runtime_settings.zerocopy, flipped);
        assert_eq!(
            mgr.advertised_codecs(),
            wire(&["h264"]),
            "h265 was proven under the old zerocopy value"
        );
        match next_probe_event(&mut rx).await {
            Event::InputsObserved(inputs) => {
                assert!(
                    inputs.settings.contains(&format!("zerocopy={flipped}")),
                    "{}",
                    inputs.settings
                );
            }
            other => panic!("{other:?}"),
        }
    }

    /// The warm-up template store follows a typed `home_root` at the same
    /// boundary the next launch does; existing homes stay where they are.
    #[test]
    fn a_typed_home_root_apply_rebinds_template_seeding_for_the_next_launch() {
        let (fixture, source_policy) = crate::source_policy::tests::fixture();
        let mount = fixture.path().join("homes");
        let existing = mount.join("existing-user");
        std::fs::create_dir(&existing).unwrap();
        std::fs::write(existing.join("save.dat"), "kept").unwrap();
        let mut mgr = manager_with(Vec::new());
        mgr.runtime_settings.home_root = mount.to_str().unwrap().into();
        mgr.source_policy = Some(source_policy.clone());
        assert_eq!(source_policy.store().unwrap().home_root(), mount.as_path());
        let running = SessionConfig::for_assignment_with(
            &mgr.runtime_settings,
            StreamParams::default(),
            None,
        );

        let next_root = mount.join("v2");
        let dir = tempfile::tempdir().unwrap();
        let reply = apply_typed(
            &mut mgr,
            dir.path(),
            "home_root",
            serde_json::json!(next_root.to_str().unwrap()),
        );
        assert!(applied(&reply), "{reply:?}");
        let next = SessionConfig::for_assignment_with(
            &mgr.runtime_settings,
            StreamParams::default(),
            None,
        );
        assert_eq!(next.home_root, next_root.to_str().unwrap());
        // `session::source` seeds only when the store's root equals the launch's.
        assert_eq!(
            source_policy.store().unwrap().home_root(),
            std::path::Path::new(&next.home_root)
        );
        assert_eq!(
            running.home_root,
            mount.to_str().unwrap(),
            "running session keeps its root"
        );
        assert_eq!(
            std::fs::read_to_string(existing.join("save.dat")).unwrap(),
            "kept"
        );
    }

    #[test]
    fn a_typed_app_boot_timeout_apply_reaches_the_next_launch_only() {
        let mut mgr = manager_with(Vec::new());
        let running = SessionConfig::for_assignment_with(
            &mgr.runtime_settings,
            StreamParams::default(),
            None,
        );
        let dir = tempfile::tempdir().unwrap();
        let reply = apply_typed(
            &mut mgr,
            dir.path(),
            "app_boot_timeout_secs",
            serde_json::json!(42),
        );
        assert!(applied(&reply), "{reply:?}");
        let next = SessionConfig::for_assignment_with(
            &mgr.runtime_settings,
            StreamParams::default(),
            None,
        );
        assert_eq!(next.app_boot_timeout, Some(Duration::from_secs(42)));
        assert_ne!(running.app_boot_timeout, next.app_boot_timeout);
    }
}
