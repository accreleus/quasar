//! The agent's control loop: the control-plane WebSocket (register, capacity,
//! heartbeat, signaling relay) and [`SessionManager`], which owns every session's
//! handles. All session state lives in the single `connect_and_run` task — no locks;
//! runner threads only emit `SessionEvent`s into a channel this loop drains.
//!
//! This loop is outside the per-session tracing span (see `.claude/rules/agent-logging.md`),
//! so lines here carry an explicit `session_id` field.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use futures_util::{SinkExt, StreamExt};
use tokio::sync::mpsc;
use tokio::time::sleep;
use tokio_tungstenite::{connect_async_tls_with_config, tungstenite::Message};
use tracing::{debug, error, info, warn};

use crate::capacity;
use crate::config::Config;
use crate::health::HealthState;
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
use crate::session::{EncoderChoice, SessionConfig, StreamParams};
use crate::vram::{VramCache, VramTarget};

const AGENT_VERSION: &str = env!("CARGO_PKG_VERSION");
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

    // Startup orphan sweep (P2-06): `docker run --rm` survives a SIGKILL of the
    // agent, so a prior run can leave session/pulse sibling containers behind.
    // Best-effort — a sweep failure never blocks startup.
    let (runtime, swept) = offload_probe(|| {
        let runtime = ContainerRuntime::from_env();
        let swept = runtime.sweep_orphans(&[
            crate::session::container::SESSION_NAME_PREFIX,
            crate::session::audio::PULSE_NAME_PREFIX,
        ]);
        (runtime, swept)
    })
    .await;
    if swept > 0 {
        info!("startup sweep removed {swept} orphaned container(s) from a prior run");
    }

    // Install mode + updater presence, discovered once per process: it shells
    // out to docker, while `register` is re-sent on every reconnect. Failure is
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
    crate::session::homes_gc::spawn_sweeper();

    let health = HealthState::new();
    crate::health::spawn_if_enabled(health.clone());

    // Adopt an already-provisioned NVIDIA driver volume BEFORE anything can touch
    // EGL: the post-restart path (the provisioner exits so a fresh process lands
    // here) and the steady state on a provisioned host.
    let (runtime, nvidia_lib32_probed) = offload_probe(move || {
        if runtime.is_nvidia() {
            crate::nvidia_volume::adopt_current(runtime.bin());
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
    spawn_nvidia_volume_provisioner(&runtime, &nvidia_lib32_probed);

    // #545: the CUDA half. NOT chained onto the driver volume — that one returns
    // immediately on a CDI-injected host, and NVRTC is needed on those too. The two
    // share the volume and nothing else (separate lock, manifest, backoff), so
    // running them concurrently is safe.
    spawn_cuda_runtime_provisioner(&runtime);

    let mut backoff = Duration::from_secs(1);
    loop {
        match connect_and_run(
            &cfg,
            &health,
            &nvidia_lib32_probed,
            &image_mgr,
            &release_mgr,
        )
        .await
        {
            Ok(()) => {
                // clean shutdown (shouldn't happen in normal operation)
                info!("agent exiting cleanly");
                return;
            }
            Err(e) => {
                health.set_connected(false);
                error!(
                    token = "agent-connection-failed",
                    "agent connection failed: {e:#}; reconnecting in {backoff:?}"
                );
                // One line on the cycle that crosses the threshold: every retry
                // already logs above, so this fires only when transient becomes
                // sustained.
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
                sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(30));
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
    let docker = runtime.bin().to_string();
    let nvidia_lib32_probed = nvidia_lib32_probed.to_string();
    std::thread::Builder::new()
        .name("quasar-nvvol".into())
        .spawn(move || {
            // ONE `ProbeEnv::live`: building it forks the EGL self-test, and the gap
            // and the card must answer about the same instant.
            let env = crate::readiness::ProbeEnv::live(true, &nvidia_lib32_probed);
            let gap = crate::readiness::nvidia_gap(&env);
            if !gap.any() {
                return;
            }
            match crate::nvidia_volume::provision_blocking(true, gap, &docker) {
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
                _ => {}
            }
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

/// The `capacity.codecs` field for a (possibly failed) probe. `None` — the probe
/// never ran or `gst::init` failed — is a real wire distinction: the control plane
/// keeps whatever it last stored rather than clobbering it, and a host that has
/// never reported reads back as h264-only.
pub(crate) fn advertised_codecs(report: &Option<HostCodecReport>) -> Option<Vec<String>> {
    report.as_ref().map(|r| r.codecs.clone())
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
/// for why the two travel together. `None` ⇒ gst init failed, and the control plane
/// then defaults the host to `["h264"]`. The effective encoder is not restart-class —
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
    let support = crate::session::pipeline::probe_codec_support(settings.encoder, knobs);
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

async fn connect_and_run(
    cfg: &Config,
    health: &Arc<HealthState>,
    nvidia_lib32_probed: &str,
    image_mgr: &Arc<ImageManager>,
    release_mgr: &Arc<ReleaseManager>,
) -> anyhow::Result<()> {
    let url = cfg.ws_url();
    info!(policy = ?cfg.transport, "connecting to {url}");
    if cfg.webpki_from_blob {
        // `qenr1..` (a mispaste that dropped the fingerprint) and a real CA deployment
        // produce the same policy; only this line tells them apart in a log.
        info!(
            token = "cp-tls-webpki-from-blob",
            "the enrollment string carried an empty fingerprint segment: verifying the control \
             plane against the WebPKI roots, not a pin"
        );
    }

    // #12: the connector is chosen by policy, never by tokio-tungstenite's default — a
    // wss:// URL must not silently validate against the OS/bundled roots when a pin was
    // configured, and a ws:// URL is explicitly Plain.
    let (ws_stream, _) = connect_async_tls_with_config(
        &url,
        None,
        false,
        Some(crate::cp_tls::ws_connector(&cfg.transport)),
    )
    .await?;
    let (mut tx, mut rx) = ws_stream.split();

    // agent-api.md: recorded images are verified against the docker daemon on startup
    // AND reconnect — an image `docker rmi`'d out from under a long-lived agent must
    // not keep reporting `ready`. Runs before the upstream attaches, so the
    // attach-time flush reports post-reconciliation states.
    let images = {
        let mgr = image_mgr.clone();
        tokio::task::spawn_blocking(move || mgr.refresh_register_images()).await?
    };

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

    // --- Step 1: send register ---
    let auth = choose_auth(cfg)?;
    let install = crate::buildinfo::install_facts();
    let register_msg = AgentMsg::Register {
        node_name: cfg.node_name.clone(),
        agent_version: AGENT_VERSION.to_string(),
        auth,
        images,
        source_commit: crate::buildinfo::source_commit().map(str::to_string),
        built_at: crate::buildinfo::built_at().map(str::to_string),
        install_mode: install.install_mode.map(|m| m.as_str().to_string()),
        updater_present: install.updater_present,
    };
    send(&mut tx, &register_msg).await?;
    info!("sent register (node_name={})", cfg.node_name);

    // --- Step 2: receive registered ---
    let raw = recv(&mut rx).await?;
    let ctrl_msg: ControlMsg = serde_json::from_str(&raw)?;
    let (host_id, heartbeat_interval_ms) = match ctrl_msg {
        ControlMsg::Registered {
            host_id,
            node_secret,
            heartbeat_interval_ms,
        } => {
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
            persist_pin_if_new(cfg);
            (host_id, heartbeat_interval_ms)
        }
        ControlMsg::Error { code, message } => {
            anyhow::bail!("control plane rejected register: {code}: {message}");
        }
        _ => {
            anyhow::bail!("unexpected message type before registered");
        }
    };
    health.set_connected(true);
    // Clear the failure streak before a stale count can flip /health unhealthy.
    health.record_registered();

    // --- Step 3: send capacity ---
    let cap = offload_probe(detect_capacity_blocking).await;
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
    let mut first_settings = crate::session::settings::RuntimeSettings::baseline();
    seed_nvidia_lib32(&mut first_settings, nvidia_lib32_probed);
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
    let capacity_msg = AgentMsg::Capacity {
        host: cap.host,
        gpus: cap.gpus,
        gpu_detection: cap.gpu_detection,
        gpu_detection_reason: cap.gpu_detection_reason,
        console_capabilities: Some(cap.console),
        effective_settings: Some(first_settings.effective_map()),
        codecs: advertised_codecs(&host_codec_report),
        codec_throughput: advertised_codec_throughput(&host_codec_report),
        readiness: Some(readiness.clone()),
    };
    send(&mut tx, &capacity_msg).await?;
    info!("capacity report sent");

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

    // #175: home refs mounted by live sessions. The GC reaper consults it so it can
    // never reap a backing store an active session is using.
    let live_refs: LiveRefs = Arc::new(Mutex::new(HashSet::new()));
    let mut mgr = SessionManager::new(
        live_refs.clone(),
        health.clone(),
        gpu_inventory,
        vram_targets,
        nvidia_lib32_probed.to_string(),
        image_mgr.clone(),
        release_mgr.clone(),
    );
    // Cached with the encoder it was probed for, so capacity re-sends reuse it unless
    // a config_update flips the effective encoder and marks it stale.
    mgr.host_codec_report = host_codec_report.clone();
    mgr.readiness = readiness;
    mgr.probed_encoder = Some(first_settings.encoder);

    // #488: the golden-home warm-up. Scheduled by the control plane and claimed over
    // the additive `/v1/agent/jobs/*` HTTP pull, so `protocol/agent-api.md` is
    // untouched. Registered EVEN WHEN THE FEATURE IS OFF: the runner then reports
    // `skipped` with a reason, so "not configured" and "nothing to do" stay
    // distinguishable, and the gate collaborators stay wired either way.
    let warmup_store = crate::session::warmup::resolve_store(&mgr.runtime_settings.home_root);
    let warmup_activity = Arc::new(crate::session::warmup::HostActivity::new());
    let warmup_control = Arc::new(crate::session::warmup::WarmupControl::new());
    let warmup_runner = Arc::new(crate::session::warmup::WarmupJobRunner::new(
        crate::session::warmup::WarmupConfig::from_env(),
        warmup_store.clone(),
        Arc::new(crate::session::warmup::host::AgentWarmupHost::new(
            ContainerRuntime::from_env(),
            mgr.runtime_settings.clone(),
        )),
        warmup_control.clone(),
        warmup_activity.clone(),
        app_uid_gid(),
    ));
    mgr.warmup_activity = Some(warmup_activity);
    mgr.warmup_control = Some(warmup_control.clone());
    mgr.note_session_count();
    // The one image-lifecycle duty that stayed agent-side: drop a template whose image
    // was uninstalled. Detached on disconnect (the ImageManager is process-wide); the
    // guard also aborts a warm-up that would otherwise outlive its connection (#489).
    let _warmup_guard = warmup_store.as_ref().map(|store| {
        image_mgr.set_lifecycle_observer(Some(Arc::new(
            crate::session::warmup::TemplateReaper::new(store.clone()),
        )));
        info!("template: golden-home warm-up armed for this connection");
        crate::session::warmup::WarmupConnectionGuard::new(warmup_control, image_mgr.clone())
    });
    // The seeding half, gated separately from the warm-up-building wiring above
    // (`QUASAR_HOME_TEMPLATES` vs `QUASAR_TEMPLATE_WARMUP`): a host can consume
    // already-built templates without ever building one, and vice versa. Off by
    // default — `template_store` stays `None` and `provision_home_dirs` is unchanged.
    mgr.template_store = if crate::session::env_bool("QUASAR_HOME_TEMPLATES") {
        crate::session::template::TemplateStore::resolve_from_env(std::path::Path::new(
            &mgr.runtime_settings.home_root,
        ))
    } else {
        None
    };
    // Tracks the reservation across heartbeats so a flip triggers exactly one
    // capacity re-send.
    let mut last_warmup_reserved = false;
    let (evt_tx, evt_rx) = mpsc::channel::<(String, SessionEvent)>(CRITICAL_EVENT_CAPACITY);
    // `Option`-wrapped for `recv_or_disabled`, like the other four receiver arms.
    let mut evt_rx = Some(evt_rx);
    // Device-lost failures across sessions on this connection: ≥2 within
    // GPU_GLOBAL_WINDOW escalate to a GPU-global drain+restart; one stays per-session.
    let mut gpu_fault = GpuGlobalFaultDetector::default();
    // `host.xid` / `host.gpu_fault`: the kernel ring-buffer tailer, on its own thread
    // off the media path. `spawn` returns `None` and drops `tx` when `/dev/kmsg` is
    // unreadable (the container default) — the busy-spin path `recv_or_disabled` fixes.
    let (gpu_fault_tx, gpu_fault_rx) = mpsc::unbounded_channel::<crate::gpu_kmsg::GpuFault>();
    let mut gpu_fault_rx = Some(gpu_fault_rx);
    let _gpu_kmsg_thread = crate::gpu_kmsg::spawn(gpu_fault_tx);
    let (diagnostic_raw_tx, diagnostic_rx) = mpsc::channel(DIAGNOSTIC_EVENT_CAPACITY);
    let mut diagnostic_rx = Some(diagnostic_rx);
    let diagnostic_dropped_interval = Arc::new(AtomicU64::new(0));
    let diagnostic_dropped_total = Arc::new(AtomicU64::new(0));
    let diagnostic_tx = DiagnosticEventTx::new(
        diagnostic_raw_tx,
        diagnostic_dropped_interval.clone(),
        diagnostic_dropped_total.clone(),
    );

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

    loop {
        tokio::select! {
            _ = hb_timer.tick() => {
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
                if mgr.warmup_reserved() != last_warmup_reserved {
                    last_warmup_reserved = mgr.warmup_reserved();
                    send_fresh_capacity(&mut tx, &mut mgr).await?;
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
                        if let Some(reply) = mgr.handle_control(ctrl, &evt_tx, &diagnostic_tx) {
                            send(&mut tx, &reply).await?;
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
                            mgr.gpu_inventory.clone_from(&cap.gpus);
                            mgr.vram_targets = cap.vram_targets;
            // Must ride every `vram_targets` reassignment — see `vram_cache`'s doc.
            mgr.vram_cache.invalidate();
                            // Reported-copy only — see `send_fresh_capacity`.
                            let mut cap_gpus = cap.gpus;
                            crate::session::warmup::apply_encode_slot_reservation(
                                &mut cap_gpus,
                                mgr.warmup_reserved(),
                            );
                            let capacity_msg = AgentMsg::Capacity {
                                host: cap.host,
                                gpus: cap_gpus,
                                gpu_detection: cap.gpu_detection,
                                gpu_detection_reason: cap.gpu_detection_reason,
                                console_capabilities: Some(cap.console),
                                effective_settings: Some(mgr.runtime_settings.effective_map()),
                                codecs: advertised_codecs(&mgr.host_codec_report),
                                codec_throughput: advertised_codec_throughput(&mgr.host_codec_report),
                                readiness: Some(mgr.readiness.clone()),
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
                    mgr.gpu_inventory.clone_from(&cap.gpus);
                    mgr.vram_targets = cap.vram_targets;
            // Must ride every `vram_targets` reassignment — see `vram_cache`'s doc.
            mgr.vram_cache.invalidate();
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
                    let capacity_msg = AgentMsg::Capacity {
                        host: cap.host,
                        gpus: cap_gpus,
                        gpu_detection: cap.gpu_detection,
                        gpu_detection_reason: cap.gpu_detection_reason,
                        console_capabilities: Some(cap.console),
                        effective_settings: Some(mgr.runtime_settings.effective_map()),
                        codecs: advertised_codecs(&mgr.host_codec_report),
                        codec_throughput: advertised_codec_throughput(&mgr.host_codec_report),
                        readiness: Some(mgr.readiness.clone()),
                    };
                    send(&mut tx, &capacity_msg).await?;
                }
            }
            evt = recv_or_disabled(&mut evt_rx) => {
                // `None` means every sender is gone — disable the arm.
                let Some((session_id, event)) = evt else {
                    error!(
                        token = "evt-channel-closed",
                        "session-event sender dropped unexpectedly; disabling session-event \
                         handling for the rest of this connection"
                    );
                    evt_rx = None;
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
                            flush_pending_diagnostics(&mut tx, &mut diagnostic_rx).await?;
                            let msg =
                                mgr.on_event(&session_id, SessionEvent::Stopped { bytes_used, detail });
                            send(&mut tx, &msg).await?;
                            // Console auto-start is level-triggered by capacity, so
                            // re-send immediately after a terminal state rather than
                            // waiting on an unrelated connector/input/storage poll.
                            send_fresh_capacity(&mut tx, &mut mgr).await?;
                            info!("re-sent capacity after session stopped for console reconciliation");
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
                            // #503: same pre-terminal flush as the `Stopped` arm —
                            // `webrtc.remote_description_failed` is emitted by the
                            // runner immediately before this very event.
                            if terminal {
                                flush_pending_diagnostics(&mut tx, &mut diagnostic_rx).await?;
                            }
                            let msg = mgr.on_event(&session_id, other);
                            send(&mut tx, &msg).await?;
                            if terminal {
                                send_fresh_capacity(&mut tx, &mut mgr).await?;
                                info!("re-sent capacity after session failure for console reconciliation");
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
            diagnostic = recv_or_disabled(&mut diagnostic_rx) => {
                // `None` means every sender is gone — disable the arm.
                let Some((session_id, te)) = diagnostic else {
                    error!(
                        token = "diagnostic-channel-closed",
                        "diagnostic-event sender dropped unexpectedly; disabling diagnostic \
                         trace forwarding for the rest of this connection"
                    );
                    diagnostic_rx = None;
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
struct SessionManager {
    /// Assigned but not yet started. Aged out by the heartbeat sweep — see
    /// [`PENDING_ASSIGNMENT_TTL`].
    pending: HashMap<String, PendingAssignment>,
    running: HashMap<String, RunningHandle>,
    /// Agent-local runtime knobs, starting at the env baseline and overlaid by
    /// `config_update` pushes. Read when building each session's SessionConfig.
    runtime_settings: crate::session::settings::RuntimeSettings,
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
    /// #375: the 32-bit NVIDIA driver-lib dir auto-detected at startup, seeded into
    /// `runtime_settings.nvidia_lib32_path` whenever the configured value is empty.
    /// Empty on non-NVIDIA hosts and when nothing was detected.
    nvidia_lib32_probed: String,
    /// The codec set + throughput hint the host's active encoder path can produce.
    /// Re-probed whenever a `config_update` flips the effective encoder, since that
    /// overlay is live-class. `None` ⇒ probe skipped/failed, so the control plane
    /// defaults the host to `["h264"]`.
    host_codec_report: Option<HostCodecReport>,
    /// The host readiness check set, computed once per CONNECTION and repeated
    /// verbatim in every capacity re-send on it. Per connection because every input is
    /// fixed for the container's life: driver libraries and device nodes are injected
    /// at create time, and the 32-bit GL answer costs a throwaway container. Applying
    /// a host fix means recreating the container anyway.
    readiness: Vec<crate::messages::ReadinessCheck>,
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
    /// The resolved golden-home template store, or `None` when
    /// `QUASAR_HOME_TEMPLATES` is off (the default) or the template root is
    /// misconfigured. Independent of the warm-up store — a host can seed from
    /// already-built templates without building one. Snapshotted onto each session's
    /// `SessionConfig` at assignment.
    template_store: Option<crate::session::template::TemplateStore>,
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
        SessionManager {
            pending: HashMap::new(),
            running: HashMap::new(),
            runtime_settings,
            gpu_inventory,
            vram_targets,
            vram_cache: Arc::new(VramCache::new()),
            console_config: None,
            live_refs,
            health,
            nvidia_lib32_probed,
            host_codec_report: None,
            readiness: Vec::new(),
            probed_encoder: None,
            draining: false,
            runner: default_runner(),
            warmup_activity: None,
            warmup_control: None,
            image_mgr,
            release_mgr,
            template_store: None,
        }
    }

    /// Publish the live-session count to the warm-up gate. Called from every site that
    /// updates `/health`'s count, so the two can never disagree about host busyness.
    fn note_session_count(&self) {
        if let Some(a) = &self.warmup_activity {
            a.set_live(self.running.len(), Instant::now());
        }
    }

    /// Is a warm-up currently holding an encode slot?
    fn warmup_reserved(&self) -> bool {
        self.warmup_control
            .as_ref()
            .map(|c| c.reserved())
            .unwrap_or(false)
    }

    /// Built per assign/swap rather than cached: `settings.home_root` is a live-class
    /// setting a `config_update` can move under a long-lived connection, and a stale
    /// root would refuse the managed home it just relocated to.
    fn mount_policy(&self) -> MountPolicy {
        MountPolicy::from_env(&self.runtime_settings.home_root)
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
        let gpu = self
            .gpu_inventory
            .iter()
            .find(|gpu| gpu.index == gpu_index)
            .ok_or_else(|| anyhow::anyhow!(
                "scheduled GPU index {gpu_index} is absent from the agent's latest capacity inventory"
            ))?;

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

    /// Signal every running session to stop when the control-plane connection ends. On
    /// reconnect the agent presents as fresh and the control plane reconciles its
    /// sessions to failed, so the local pipelines must tear down too or their
    /// containers and sidecars orphan.
    fn stop_all(&self) {
        for (id, h) in &self.running {
            h.stop.store(true, Ordering::Relaxed);
            info!("connection ended: signalling session {id} to stop");
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
                // Raised BEFORE anything else in the assign path: a warm-up's NVENC
                // teardown must never overlap the encoder this session is about to
                // create (#489). The assign→start gap is the abort's budget.
                self.abort_any_warmup();
                let container = match app_to_container(app, &self.mount_policy()) {
                    Ok(c) => c,
                    Err(error) => {
                        warn!(
                            token = "session-assign-rejected",
                            "session {session_id} assignment rejected: {error:#}"
                        );
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
                    return Some(ack(id, false, Some(error.to_string())));
                }
                cfg.console_config = self.console_config.clone();
                cfg.video_topology = video_topology;
                // The wire carries no image_id on AppSpec, so resolve it from the
                // launch ref via the ImageManager's map. `None` on either side means
                // no seeding for this session, never an assignment failure.
                cfg.template_store = self.template_store.clone();
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
                // Prepare step of reserve→prepare→go-live: pull now, off the agent
                // loop, so session_start is fast.
                if let Some(spec) = container {
                    let runtime = ContainerRuntime::from_env();
                    std::thread::spawn(move || {
                        if let Err(e) = runtime.pull(&spec.image) {
                            warn!(
                                token = "assign-image-pull-failed",
                                "assign-time pull of {} failed: {e:#}", spec.image
                            );
                        }
                    });
                }
                self.pending.insert(
                    session_id,
                    PendingAssignment {
                        cfg,
                        assigned_at: Instant::now(),
                    },
                );
                Some(ack(id, true, None))
            }
            ControlMsg::SessionStart { id, session_id } => match self.pending.remove(&session_id) {
                Some(PendingAssignment { cfg, .. }) => {
                    // The assign already raised this, but a `session_start` for an
                    // assignment that landed on a previous connection would not have.
                    self.abort_any_warmup();
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
                        let outcome =
                            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
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
                            }));
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
                self.pending.remove(&session_id);
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
                console_config,
            } => {
                // #194: re-derive from the env baseline, then overlay the host's sparse
                // overrides (agent-api.md `config_update` sends only those). An absent
                // key keeps the env value, so a cleared override reverts to env rather
                // than to the catalog default and a host's QUASAR_ENCODER survives.
                // A console-only PATCH sends settings as JSON null; that must NOT
                // rebaseline and silently undo a persisted encoder override.
                if !settings.is_null() {
                    let mut next = crate::session::settings::RuntimeSettings::baseline();
                    next.apply_json(&settings);
                    seed_nvidia_lib32(&mut next, &self.nvidia_lib32_probed);
                    self.runtime_settings = next;
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
                None // fire-and-forget, no ack
            }
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
        for sid in stale {
            warn!(
                token = "session-assign-never-started",
                "session {sid}: assignment never started within {pending_ttl:?}; \
                 dropping the orphaned pending config"
            );
            self.pending.remove(&sid);
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
            SessionEvent::Running => (
                "running",
                Some("pipeline live; offer ready".to_string()),
                None,
            ),
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

impl Drop for SessionManager {
    /// The manager drops exactly when `connect_and_run` returns, so stopping every
    /// session here covers all exit paths (clean close, read error, write failure)
    /// without threading a guard through the message loop.
    fn drop(&mut self) {
        self.stop_all();
    }
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

fn choose_auth(cfg: &Config) -> anyhow::Result<Auth> {
    if let Ok(secret) = std::fs::read_to_string(&cfg.node_secret_path) {
        let secret = secret.trim().to_string();
        if !secret.is_empty() {
            return Ok(Auth::Reconnect {
                node_secret: secret,
            });
        }
    }
    match &cfg.enrollment_token {
        Some(token) => Ok(Auth::Enrollment {
            enrollment_token: token.clone(),
        }),
        None => anyhow::bail!(
            "no node_secret at {} and ENROLLMENT_TOKEN not set; cannot register",
            cfg.node_secret_path
        ),
    }
}

/// Write the verified pin beside the node secret the first time a pinned connection
/// registers. Never overwrites what a reconnect merely re-learned — the single exception
/// is a rotation the operator drove through CONTROL_PLANE_FINGERPRINT, which is the one
/// pin that both differs from the file and has just verified a real handshake.
fn persist_pin_if_new(cfg: &Config) {
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
    let rotating = occupied && cfg.pin_source == Some(crate::enrollment::PinSource::Env);
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
    mgr.gpu_inventory.clone_from(&cap.gpus);
    mgr.vram_targets = cap.vram_targets;
    // Must ride every `vram_targets` reassignment — see `vram_cache`'s doc.
    mgr.vram_cache.invalidate();
    // A warm-up holds an encode slot for its duration. Applied to the REPORTED copy
    // only: `mgr.gpu_inventory` keeps the true inventory, so an assignment is still
    // validated against the hardware that exists.
    let mut cap_gpus = cap.gpus;
    crate::session::warmup::apply_encode_slot_reservation(&mut cap_gpus, mgr.warmup_reserved());
    let msg = AgentMsg::Capacity {
        host: cap.host,
        gpus: cap_gpus,
        gpu_detection: cap.gpu_detection,
        gpu_detection_reason: cap.gpu_detection_reason,
        console_capabilities: Some(cap.console),
        effective_settings: Some(mgr.runtime_settings.effective_map()),
        codecs: advertised_codecs(&mgr.host_codec_report),
        codec_throughput: advertised_codec_throughput(&mgr.host_codec_report),
        readiness: Some(mgr.readiness.clone()),
    };
    send(sink, &msg).await
}

async fn send<S>(sink: &mut S, msg: &AgentMsg) -> anyhow::Result<()>
where
    S: SinkExt<Message, Error = tokio_tungstenite::tungstenite::Error> + Unpin,
{
    let json = serde_json::to_string(msg)?;
    sink.send(Message::Text(json.into())).await?;
    Ok(())
}

async fn recv<S>(stream: &mut S) -> anyhow::Result<String>
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::{AbrMode, EncoderChoice};

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
        persist_pin_if_new(&cfg);

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

            persist_pin_if_new(&cfg);
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

        persist_pin_if_new(&cfg);
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

        persist_pin_if_new(&cfg);
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
            let dir = tempfile::tempdir().unwrap();
            let secret_path = dir.path().join("node-secret");
            let cfg = pinned_cfg(secret_path.to_str().unwrap(), pin_fixture(0xAB), source);
            let target = dir.path().join("victim");
            std::os::unix::fs::symlink(&target, cfg.pin_path()).unwrap();

            persist_pin_if_new(&cfg);
            assert!(
                !target.exists(),
                "{source:?} wrote through the symlink to {target:?}"
            );
        }
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

        let settings = crate::session::settings::RuntimeSettings::baseline();
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

    fn gpu(index: i32, vendor: &str, render_node: Option<&str>) -> crate::messages::GpuCapacity {
        crate::messages::GpuCapacity {
            index,
            vendor: vendor.to_string(),
            model: "fixture".to_string(),
            vram_mb_total: 8192,
            encode_slots_total: 2,
            render_node: render_node.map(str::to_string),
            device_path: render_node.map(crate::session::settings::canonicalize_render_node),
        }
    }

    fn assignment_config(encoder: EncoderChoice, render_node: &str) -> SessionConfig {
        let mut settings = crate::session::settings::RuntimeSettings::baseline();
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
            codecs: advertised_codecs(&measured_nothing),
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
}
