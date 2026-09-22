//! Swap source management: the session-level resources shared across app swaps, and one
//! swappable per-app source (compositor pipeline + container).
//!
//! The launcher↔game swap splits a session into a persistent encode pipeline
//! (`pipeline::build_encode_pipeline`, owns `webrtcbin`, never restarts) and one or more
//! source pipelines behind a GStreamer interpipe boundary. This module owns the source
//! side:
//!   - [`SessionResources`] — virtual input devices + PulseAudio sidecar, created once
//!     per session and reused by every source generation.
//!   - [`AppSource`] — one generation: its source `gst::Pipeline`
//!     (`compositor → … → interpipesink`) plus the app container launched into that
//!     compositor.
//!
//! Two properties of the swap order are load-bearing (`runner::perform_swap`):
//!   - app containers are SERIALISED. The replacement must not launch until the outgoing
//!     one has exited: both bind-mount the same managed home, and a second instance of a
//!     home-locking app (Steam) hands off to the first and exits 0.
//!   - the encoder is re-pointed on the APP's first presented frame
//!     ([`AppSource::app_surface_commits`]), never the compositor's
//!     ([`AppSource::first_frame_ready`]), which fires with an empty scene.

use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

/// The read-only counter the vendored `gst-wayland-display` app-cadence patch exports on
/// `waylanddisplaysrc`: lifetime buffers committed by a mapped top-level application
/// surface tree (cursor, popup and configure-only commits excluded). The only signal that
/// distinguishes "the app drew something" from "the compositor produced an output frame":
/// the compositor renders its empty scene whether or not any client ever attached a
/// buffer.
const APP_SURFACE_COMMITS: &str = "app-surface-commits";
/// The compositor's app-facing `wl_output` logical mode (gst-wayland-display fork), as a
/// single `"WxH"` string (`"0x0"` ⇒ follow the encode size). Must stay ONE property, not
/// a width/height pair: two int sets let the intermediate `w_new × h_old` mode reach the
/// compositor (a live 1920x720 was observed between the two calls).
const RENDER_SIZE: &str = "render-size";
/// The compositor's `wp_fractional_scale_v1` preferred scale.
const UI_SCALE: &str = "ui-scale";
/// The `wl_output` modes the compositor advertises to the guest app, as a single
/// `"WxH,WxH,…"` string. One string for the same reason `render-size` is: a partially
/// applied ladder is still a mode set the app can pick from.
const MODE_LADDER: &str = "mode-ladder";

use gstreamer as gst;
use gstreamer::prelude::*;

use super::audio::PulseSidecar;
use super::container::{
    AppDisplayMode, AppExitStatus, AppLogRing, ContainerRuntime, ContainerSpec, LaunchParams,
    RunningContainer,
};
use super::host::wayland_display_from_message;
use super::input::InputState;
use super::metrics::SessionMetrics;
use super::pipeline;
use super::teardown::{self, StopAttempt, UdevAction};
use super::virtual_input::VirtualDevices;
use super::SessionConfig;
use crate::messages::AppExitPolicy;

/// Has the CURRENT app container drawn anything? `baseline` is `app-surface-commits` as
/// of that container's launch; `now` is the live reading. Pure so the relaunch case is
/// testable without a compositor.
fn app_presented_since_launch(baseline: Option<u64>, now: u64) -> bool {
    match baseline {
        // No launch recorded, or no counter at launch time: all that is left to ask is
        // whether anything has been drawn at all.
        None => now > 0,
        // The counter went BACKWARDS, so the compositor element was replaced and the
        // baseline describes something gone. Fall back to the absolute reading rather
        // than reporting "never drew" for a compositor that plainly has.
        Some(base) if now < base => now > 0,
        Some(base) => now > base,
    }
}

/// One compositor output observation is either backed by at least one new app
/// surface-tree buffer, or it is not. Do not turn multiple child-surface commits
/// from one rendered output into an inflated source cadence.
fn source_commit_advanced(current: u64, previous: u64) -> u64 {
    u64::from(current > previous)
}

/// Runtime observation errors are not terminal application evidence. The observer
/// keeps its own bounded retry loop for every error kind; only a verified
/// `ApplicationResult` may publish an application exit.
fn retry_application_observation(_kind: crate::runtime::ErrorKind) -> bool {
    true
}

/// App container, pulse sidecar and udev export, shared by every [`AppSource`]
/// generation so session end has one release path. The runner calls
/// [`AppSource::teardown`] on every terminal exit; that calls [`SharedSessionRuntime::apply`]
/// before the source pipeline is set to NULL.
struct SharedSessionRuntime {
    session_id: String,
    udev_blocked: AtomicBool,
    /// Set when an app container is launched; cleared only when its stop is confirmed.
    mount_live: AtomicBool,
    /// The last container-stop outcome. `None` until a generation records one.
    last_stop: Mutex<Option<StopAttempt>>,
    pulse: Mutex<Option<PulseSidecar>>,
    devices: Option<Arc<VirtualDevices>>,
}

impl SharedSessionRuntime {
    /// Record a container-stop outcome without touching the sidecar or the
    /// udev export. A swap drops the outgoing [`AppSource`] while the session,
    /// and the replacement generation, are still using both.
    fn note_container_stop(&self, report: StopAttempt) {
        let already_blocked = self.udev_blocked.load(Ordering::Relaxed);
        self.udev_blocked.store(
            teardown::blocked_after(report, already_blocked),
            Ordering::Relaxed,
        );
        if matches!(report, StopAttempt::Confirmed) {
            self.mount_live.store(false, Ordering::Relaxed);
        }
        *self
            .last_stop
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(report);
    }

    fn apply(&self, report: StopAttempt, final_chance: bool) {
        let already_blocked = self.udev_blocked.load(Ordering::Relaxed);
        let mount_live = self.mount_live.load(Ordering::Relaxed);
        self.note_container_stop(report);
        let action = teardown::action_this_chance(
            teardown::udev_action(report, already_blocked, mount_live),
            final_chance,
            mount_live,
        );
        if matches!(action, UdevAction::Abandon) {
            tracing::warn!(
                token = "udev-export-retire-skipped",
                session = %self.session_id,
                "an app container stop was not proven — leaving the udev export \
                 dir for the boot sweep"
            );
        }
        teardown::on_udev(
            action,
            || {
                if let Some(devices) = &self.devices {
                    devices.retire_udev_export();
                }
            },
            || {
                if let Some(devices) = &self.devices {
                    devices.abandon_udev_export();
                }
            },
        );
        // Leave the sidecar in place when a busy client could not start the stop:
        // the next release, and the sidecar's own Drop, try again. Taking it out
        // here would spend the only remaining attempt inside this call.
        let mut guard = self
            .pulse
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Some(pulse) = guard.as_mut() {
            pulse.stop();
        }
    }
}

/// Session-level resources shared across app swaps: the virtual input devices and the
/// PulseAudio sidecar. Each [`AppSource`] borrows the device node paths and the pulse
/// socket (mounted into its container; the encode pipeline's `pulsesrc` captures from the
/// same sidecar). Session end releases the sidecar and the udev export through
/// [`SharedSessionRuntime::apply`], not through a later `Drop` of this struct: pipeline
/// teardown can block, and a busy runtime client must not latch the release off.
pub struct SessionResources {
    /// Shared with the encode pipeline's DataChannel input sink.
    pub devices: Option<Arc<VirtualDevices>>,
    /// Per-session controller-first pointer-nudge state (BPM focus heal), shared with the
    /// encode pipeline's DataChannel input closure. Owned here, not in the closure, so
    /// the swap path can re-arm it per app process: the DataChannel persists across a
    /// swap, this state must not.
    pub input_state: Arc<InputState>,
    shared: Arc<SharedSessionRuntime>,
    runtime_dir: String,
    /// Set when the sidecar was WANTED but unusable, so the session is about to stream
    /// silence. `None` on the healthy path and under test audio — a caller must be able
    /// to tell "nobody asked for a sidecar" from "it broke". Surfaced on
    /// `effective_media.audio`, and under `QUASAR_AUDIO_REQUIRED` it fails the session. A
    /// sidecar image with no `pulseaudio` binary once silently muted every session on a
    /// host for days; the only evidence was one WARN line, and they all reported
    /// `running`.
    audio_degraded: Option<String>,
}

impl SessionResources {
    /// Create the virtual input devices and start the PulseAudio sidecar. Returns
    /// `(resources, pulse_server_uri)`; the uri threads into the encode pipeline's
    /// `cfg.pulse_server` for `pulsesrc`, and `None` falls back to silent audio.
    pub fn prepare(
        session_id: &str,
        cfg: &SessionConfig,
    ) -> anyhow::Result<(Self, Option<String>)> {
        let devices = if cfg.use_test_src {
            None
        } else {
            let d = Arc::new(VirtualDevices::create(session_id)?);
            // Publish the fake-udev records for the app container's /run/udev/data mount:
            // SDL/Steam discover controllers via libudev, so without these the gamepad
            // node is invisible to games. Best-effort.
            if let Err(e) = d.export_udev_data(&cfg.runtime_dir, session_id) {
                tracing::warn!(
                    token = "udev-export-failed",
                    "udev export for session {session_id} failed: {e:#} — in-container gamepad \
                     discovery degraded"
                );
            }
            Some(d)
        };
        let runtime = ContainerRuntime::from_env();
        // Both no-sidecar outcomes carry a reason, because both mean the session streams
        // silence: `Err` (could not start at all) and `Ok(None)` (started, socket never
        // appeared). Audio failure still must not kill video by default, but it must not
        // be invisible either.
        let mut audio_degraded: Option<String> = None;
        let pulse = if cfg.use_test_audio {
            None
        } else {
            match PulseSidecar::start(session_id, &runtime, &cfg.runtime_dir) {
                Ok(Some(s)) => Some(s),
                Ok(None) => {
                    audio_degraded = Some(
                        "PulseAudio sidecar started but its socket never became ready".to_string(),
                    );
                    None
                }
                Err(e) => {
                    audio_degraded = Some(format!("PulseAudio sidecar start failed: {e:#}"));
                    None
                }
            }
        };
        if let Some(reason) = audio_degraded.as_deref() {
            tracing::warn!(
                token = "audio-unavailable-silent",
                "{reason} — session audio will be SILENT"
            );
        }
        let pulse_server = pulse.as_ref().map(|p| p.server_uri());
        let shared = Arc::new(SharedSessionRuntime {
            session_id: session_id.to_string(),
            udev_blocked: AtomicBool::new(false),
            mount_live: AtomicBool::new(false),
            last_stop: Mutex::new(None),
            pulse: Mutex::new(pulse),
            devices: devices.clone(),
        });
        // Pre-create any bind-mount host paths under QUASAR_HOME_ROOT so Docker does not
        // create them root:root 755. No-op when unset or no mount matches; never fails
        // the session.
        //
        // An authoritative source policy and matching adopted template are both
        // required. Unknown/custom images and disconnected sessions launch cold.
        if let Some(c) = cfg.container.as_ref() {
            let seed = cfg
                .source_policy
                .as_ref()
                .zip(cfg.image_id.as_deref())
                .and_then(|(policy, image_id)| policy.seed(image_id, &c.image))
                .filter(|(store, _, _)| store.home_root() == std::path::Path::new(&cfg.home_root));
            let template =
                seed.as_ref()
                    .map(|(store, seed, authorization)| super::home::TemplateSeeder {
                        store,
                        seed,
                        authorization: Some(authorization),
                    });
            super::home::provision_home_dirs(&c.mounts, &cfg.home_root, template);
        }
        Ok((
            SessionResources {
                devices,
                input_state: Arc::new(InputState::new()),
                shared,
                runtime_dir: cfg.runtime_dir.clone(),
                audio_degraded,
            },
            pulse_server,
        ))
    }

    /// Why this session has no PulseAudio sidecar despite wanting one. See the field doc.
    pub fn audio_degraded_reason(&self) -> Option<&str> {
        self.audio_degraded.as_deref()
    }

    fn device_nodes(&self) -> Vec<PathBuf> {
        self.devices
            .as_ref()
            .map(|d| {
                vec![
                    d.keyboard_path.clone(),
                    d.mouse_path.clone(),
                    d.gamepad_path.clone(),
                ]
            })
            .unwrap_or_default()
    }

    /// `(pulse_server_uri, socket_dir)` to inject into an app container, or `None`
    /// when no sidecar is running.
    fn pulse_mount(&self) -> Option<(String, String)> {
        self.shared
            .pulse
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .as_ref()
            .map(|p| {
                (
                    p.server_uri(),
                    p.socket_dir().to_string_lossy().into_owned(),
                )
            })
    }
}

impl Drop for SessionResources {
    /// Final chance. [`AppSource::teardown`] already applied a non-final release
    /// on the normal paths; this catches an early return that never built a
    /// source, and abandons an export whose container was never proved gone.
    /// The recorded stop is kept as-is: a busy client is not rewritten into an
    /// unconfirmed stop.
    fn drop(&mut self) {
        let report = self
            .shared
            .last_stop
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .unwrap_or(StopAttempt::Absent);
        self.shared.apply(report, true);
    }
}

/// One generation of the swappable source: the source `gst::Pipeline`
/// (`compositor → interpipesink`) plus the app container launched into its compositor.
/// Observation state for ONE launch attempt: the terminal-exit slot the runner polls and
/// the log ring the observer fills.
///
/// Transition rule: `AppSource::launch` installs a fresh record as the current one
/// immediately before it asks the engine for a container, and the observer thread it
/// spawns on success captures only that record's [`GenerationObserver`] handles. So an
/// observer from a previous generation can never publish a late exit or a dying log line
/// into the state the runner reads for the replacement; a launch attempt that fails
/// exposes an empty tail and no exit rather than the retired generation's evidence. That
/// matters on a rolled-back swap, where the previous app is relaunched into the SAME
/// `AppSource`: a shared slot would report the old process's exit as the relaunched
/// app's failure.
pub(crate) struct GenerationObservation {
    exit_result: Arc<Mutex<Option<AppExitStatus>>>,
    logs: AppLogRing,
    /// This generation's intentional-stop marker, once a container exists. Read again
    /// at take time: the observer checks it before publishing, but a stop that begins
    /// between that check and the write would otherwise reach the runner as an exit.
    removed: Option<Arc<AtomicBool>>,
}

impl GenerationObservation {
    pub(crate) fn new() -> Self {
        GenerationObservation {
            exit_result: Arc::new(Mutex::new(None)),
            logs: AppLogRing::new(),
            removed: None,
        }
    }

    /// The handles an observer thread for this generation owns. `removed` is the
    /// container's intentional-stop marker (`RunningContainer::removed_flag`).
    pub(crate) fn observer(&mut self, removed: Arc<AtomicBool>) -> GenerationObserver {
        self.removed = Some(removed.clone());
        GenerationObserver {
            exit_result: self.exit_result.clone(),
            logs: self.logs.clone(),
            removed,
        }
    }

    /// Take semantics: the runner acts on a given exit exactly once. An exit is never
    /// handed out once our own teardown of this generation has begun, whichever side
    /// of the observer's marker check the stop landed on.
    pub(crate) fn take_exit(&self) -> Option<AppExitStatus> {
        let mut guard = match self.exit_result.lock() {
            Ok(g) => g,
            Err(poisoned) => poisoned.into_inner(),
        };
        let status = guard.take()?;
        if self
            .removed
            .as_ref()
            .is_some_and(|removed| removed.load(Ordering::SeqCst))
        {
            return None;
        }
        Some(status)
    }

    pub(crate) fn log_tail(&self) -> Vec<String> {
        self.logs.tail()
    }
}

/// The observer-side handles of one [`GenerationObservation`].
pub(crate) struct GenerationObserver {
    exit_result: Arc<Mutex<Option<AppExitStatus>>>,
    logs: AppLogRing,
    removed: Arc<AtomicBool>,
}

impl GenerationObserver {
    pub(crate) fn record_line(&self, line: String) {
        self.logs.push(line);
    }

    /// Whether our own teardown (swap, session stop) has begun for this container. Set
    /// BEFORE the engine sees a stop, so an exit observed afterwards is never an app
    /// failure.
    pub(crate) fn stopped_intentionally(&self) -> bool {
        self.removed.load(Ordering::SeqCst)
    }

    /// Publish a terminal status for this generation unless the stop was ours. Returns
    /// whether the status was published.
    pub(crate) fn publish(&self, status: AppExitStatus) -> bool {
        if self.stopped_intentionally() {
            return false;
        }
        let mut guard = match self.exit_result.lock() {
            Ok(g) => g,
            Err(poisoned) => poisoned.into_inner(),
        };
        *guard = Some(status);
        true
    }
}

/// Dropping it stops the pipeline and removes the container, so a rolled-back or
/// superseded source leaves no orphan.
pub struct AppSource {
    session_id: String,
    sink_name: String,
    container_name: String,
    pipeline: gst::Pipeline,
    container_spec: Option<ContainerSpec>,
    runtime: ContainerRuntime,
    runtime_dir: String,
    /// #375: resolved 32-bit NVIDIA driver-lib host dir, passed to each launch as the
    /// `/opt/quasar/nvidia-lib32` mount src.
    nvidia_lib32_path: String,
    /// #384: the session's streamed display mode, injected into every app container this
    /// source launches, so a swap's replacement gets the same mode.
    display: AppDisplayMode,
    device_nodes: Vec<PathBuf>,
    pulse: Option<(String, String)>,
    container: Option<RunningContainer>,
    launched: bool,
    first_frame: Arc<AtomicBool>,
    /// Compositor output buffers that have reached this generation's `interpipesink`,
    /// incremented by the same probe that sets [`Self::first_frame`]. The swap gate needs
    /// the count, not just "≥1", so it can require a frame produced strictly after the
    /// app's first surface commit (`runner::swap_source_ready`).
    sink_frames: Arc<AtomicU64>,
    /// Serialised-swap launch gate. When set, [`Self::on_bus_message`] records the
    /// compositor's Wayland socket in [`Self::pending_display`] instead of launching
    /// immediately, and the swap path launches explicitly
    /// ([`Self::launch_deferred_app`]) after the outgoing container has exited and
    /// released the managed home. `false` for the gen-0 source, which has nothing to
    /// serialise against.
    defer_launch: bool,
    /// The announced Wayland socket, held back by [`Self::defer_launch`].
    pending_display: Option<String>,
    /// The compositor socket this generation's app container was (or will be) launched
    /// into. Retained so a failed swap can relaunch into the still-live compositor
    /// ([`Self::relaunch_app`]).
    wl_display: Option<String>,
    metrics_probe: Option<(gst::Pad, gst::PadProbeId)>,
    /// #378: the formatted `self.runtime.run(...)` error from [`Self::launch`]. Take
    /// semantics via [`Self::take_launch_error`], so the caller fails the session exactly
    /// once per occurrence rather than on every poll.
    launch_error: Option<String>,
    /// The CURRENT container's exit slot and log ring, replaced on every launch. The
    /// exit status is written once by the RuntimeClient observer spawned in
    /// [`Self::launch`] and consumed by [`Self::take_container_exit`]; `None` while still
    /// running. A legitimate teardown (swap, session stop) is filtered out BEFORE it is
    /// written. The last ~100 log lines are read on the failure path
    /// ([`Self::app_log_tail`]): #463, an app that exits before producing a frame surfaces
    /// as "media path interrupted" unless its own final words travel with the failure.
    observation: GenerationObservation,
    /// `app-surface-commits` as it stood immediately BEFORE the current container
    /// launched. The counter is a lifetime total on a compositor element that OUTLIVES
    /// its app container, so after a rollback or retry the previous container's commits
    /// are still counted and a replacement that never drew reads as having presented —
    /// silently suppressing `app_exited_early` on exactly the retry paths that need it.
    /// This baseline scopes the question to the current container.
    ///
    /// `None` when nothing has launched yet, or the compositor build lacks the counter.
    app_commits_at_launch: Option<u64>,
    /// Shared with [`SessionResources`]. Session end releases the sidecar and the
    /// udev export from here, before this pipeline is set to NULL.
    shared: Arc<SharedSessionRuntime>,
    /// Set once this generation has asked the runtime to stop its container.
    /// A later drop must not spend another app-stop timeout on an unconfirmed
    /// result. A busy result is not cached: the client never accepted the call,
    /// so the next release may try again.
    ended: Option<StopAttempt>,
}

impl AppSource {
    /// Build the source pipeline (not yet PLAYING) with a first-frame probe on the
    /// interpipesink. `sink_name` is the interpipe node the encode pipeline listens to;
    /// `container_name` is the per-generation app-container name, unique so old and new
    /// containers can coexist during a swap. The container launches later, when the
    /// compositor announces its Wayland socket on the bus.
    pub fn new(
        cfg: &SessionConfig,
        session_id: &str,
        sink_name: &str,
        container_name: &str,
        res: &SessionResources,
        container_spec: Option<ContainerSpec>,
    ) -> anyhow::Result<Self> {
        let pipeline = pipeline::build_source_pipeline(cfg, res.devices.clone(), sink_name)?;

        // This is the COMPOSITOR's cadence, not the app's: gst-wayland-display renders
        // (and this probe fires) with no client surface mapped at all, which is why the
        // swap gate pairs it with the app-surface commit counter instead of treating the
        // first buffer here as "the new app is on screen".
        let first_frame = Arc::new(AtomicBool::new(false));
        let sink_frames = Arc::new(AtomicU64::new(0));
        if let Some(sink) = pipeline.by_name(sink_name) {
            if let Some(pad) = sink.static_pad("sink") {
                let ff = first_frame.clone();
                let frames = sink_frames.clone();
                pad.add_probe(gst::PadProbeType::BUFFER, move |_pad, _info| {
                    ff.store(true, Ordering::Relaxed);
                    frames.fetch_add(1, Ordering::Relaxed);
                    gst::PadProbeReturn::Ok
                });
            }
        }

        Ok(AppSource {
            session_id: session_id.to_string(),
            sink_name: sink_name.to_string(),
            container_name: container_name.to_string(),
            pipeline,
            container_spec,
            runtime: ContainerRuntime::from_env(),
            runtime_dir: res.runtime_dir.clone(),
            nvidia_lib32_path: cfg.nvidia_lib32_path.clone(),
            display: AppDisplayMode {
                width: cfg.stream.width,
                height: cfg.stream.height,
                fps: cfg.stream.fps,
            },
            device_nodes: res.device_nodes(),
            pulse: res.pulse_mount(),
            container: None,
            launched: false,
            first_frame,
            sink_frames,
            defer_launch: false,
            pending_display: None,
            wl_display: None,
            metrics_probe: None,
            launch_error: None,
            observation: GenerationObservation::new(),
            app_commits_at_launch: None,
            shared: res.shared.clone(),
            ended: None,
        })
    }

    /// Observe the compositor's own output pad, before caps normalization and the
    /// interpipe boundary. Probing `interpipesrc` instead conflates producer output with
    /// downstream interpipe delivery and cannot localize a boundary loss.
    ///
    /// `QUASAR_LATENCY_PROBE` adds the host-stage latency instrument to this same probe:
    /// the emit instant is banked against the buffer's PTS so the encoder-sink probe can
    /// close the compositor→encoder pair, the realized wall-clock frame interval is
    /// sampled, and the element's running time gives the compositor's PTS→emit hold.
    /// Metadata only — no pixel access on the render path (#270). Design:
    /// `docs/superpowers/specs/2026-08-18-latency-probe-design.md`.
    pub fn attach_compositor_metrics(&mut self, state: Arc<SessionMetrics>, latency_probe: bool) {
        let Some(source) = self.pipeline.by_name("video-source") else {
            tracing::warn!(
                token = "stage-metrics-no-video-source",
                "stage metrics: source pipeline has no video-source element"
            );
            return;
        };
        let Some(src) = source.static_pad("src") else {
            tracing::warn!(
                token = "stage-metrics-no-src-pad",
                "stage metrics: video-source has no src pad"
            );
            return;
        };
        let has_app_counter = source.find_property("app-surface-commits").is_some();
        if !has_app_counter {
            tracing::warn!(
                token = "stage-metrics-no-surface-commits",
                "stage metrics: waylanddisplaysrc lacks app-surface-commits; source.fps unavailable"
            );
        }
        let initial_app_commits = if has_app_counter {
            source.property::<u64>("app-surface-commits")
        } else {
            0
        };
        let last_app_commits = Arc::new(AtomicU64::new(initial_app_commits));
        // A swap re-attaches this probe against a NEW compositor, so clear the
        // compositor-side state: old ring entries can never match, and the first emit
        // after the swap would otherwise measure the whole swap gap as one frame interval
        // and carry it into the frame-interval p95.
        if latency_probe {
            state.probe_reset_source();
        }
        // Read the source via the pad's parent, never a captured strong clone: a strong
        // ref inside a probe on the element's own pad is a GObject ref cycle
        // (element → pad → closure → element) that never collects. It leaked the
        // compositor and, on Vulkan, its device + NV12 ring every session.
        // (`.claude/rules/gstreamer-gotchas.md`.)
        let Some(probe_id) = src.add_probe(gst::PadProbeType::BUFFER, move |pad, info| {
            if let Some(buffer) = info.buffer() {
                let pts = buffer.pts().map(|v| v.nseconds());
                state.record_compositor_frame(pts);
                // One `parent_element()` for both consumers: it takes GST_OBJECT_LOCK,
                // and this runs per frame on the render path.
                let source = if latency_probe || has_app_counter {
                    pad.parent_element()
                } else {
                    None
                };
                if latency_probe {
                    let now = std::time::Instant::now();
                    // Running time is on the same timebase as the buffer PTS: source and
                    // encode pipelines share a clock + base time (#68).
                    let running = source
                        .as_ref()
                        .and_then(|e| e.current_running_time())
                        .map(|t| t.nseconds());
                    state.probe_record_compositor_emit(pts, now, running);
                }
                if has_app_counter {
                    if let Some(source) = source.as_ref() {
                        let current = source.property::<u64>("app-surface-commits");
                        let previous = last_app_commits.swap(current, Ordering::Relaxed);
                        state.record_source_commits(source_commit_advanced(current, previous));
                    }
                }
            }
            gst::PadProbeReturn::Ok
        }) else {
            tracing::warn!(
                token = "stage-metrics-probe-attach-failed",
                "stage metrics: failed to attach video-source probe"
            );
            return;
        };
        self.metrics_probe = Some((src, probe_id));
    }

    pub fn detach_compositor_metrics(&mut self) {
        if let Some((pad, id)) = self.metrics_probe.take() {
            pad.remove_probe(id);
        }
    }

    /// Make this source pipeline share `clock` + `base` with the persistent encode
    /// pipeline (#68). interpipe spans two `GstPipeline`s; without a shared clock + base
    /// their running-times diverge, the encode side has to re-stamp at bursty arrival
    /// instants, and WebRTC congestion control collapses. `set_start_time(NONE)` stops
    /// the pipeline auto-resetting `base_time` when it goes PLAYING. Must be called
    /// before [`Self::start`].
    pub fn apply_shared_clock(&self, clock: &gst::Clock, base: gst::ClockTime) {
        self.pipeline.set_start_time(None::<gst::ClockTime>);
        self.pipeline.use_clock(Some(clock));
        self.pipeline.set_base_time(base);
    }

    /// Inject the shared `gst.cuda.context` so `waylanddisplaysrc` adopts the
    /// application-owned `GstCudaContext` instead of creating its own, making its
    /// `memory:CUDAMemory` surfaces valid across the interpipe in the encode pipeline.
    /// Must be called before `start()`, while the pipeline is still NULL/READY.
    pub fn apply_cuda_context(&self, ctx: &gst::Context) {
        self.pipeline.set_context(ctx);
    }

    /// Inject an application-owned context before the source leaves NULL. VulkanImage
    /// swaps use this to keep every compositor generation on the encoder's Vulkan device.
    pub fn apply_context(&self, ctx: &gst::Context) {
        self.pipeline.set_context(ctx);
    }

    /// #260: the VA analogue of [`Self::apply_cuda_context`] — inject the shared
    /// `gst.va.display.handle` so `waylanddisplaysrc` adopts the application-owned
    /// `GstVaDisplay`. Must be called before `start()`.
    pub fn apply_va_context(&self, ctx: &gst::Context) {
        self.pipeline.set_context(ctx);
        // `set_context` lands too late for device-pinned VA elements: they bind a display
        // on NULL→READY, before the stored context is read. A sync handler answers the
        // element's own NEED_CONTEXT in time so it adopts the shared display.
        super::va_share::install_need_context_handler(&self.pipeline, ctx);
    }

    /// Set the source pipeline PLAYING. The compositor starts and announces its Wayland
    /// socket on the bus, which [`Self::on_bus_message`] turns into the container launch.
    pub fn start(&self) -> anyhow::Result<()> {
        self.pipeline
            .set_state(gst::State::Playing)
            .map(|_| ())
            .map_err(|e| anyhow::anyhow!("source pipeline failed to reach PLAYING: {e}"))
    }

    pub fn bus(&self) -> Option<gst::Bus> {
        self.pipeline.bus()
    }

    /// Query the compositor source for a context it owns. Required by local-only Vulkan
    /// fan-out: interpipe does not forward context queries across its source/sink
    /// pipeline boundary, so the consumer pipeline must receive the producer's
    /// GstVulkanInstance/GstVulkanDevice explicitly.
    pub fn source_context(&self, context_type: &str) -> Option<gst::Context> {
        let source = self.pipeline.by_name("video-source")?;
        let mut query = gst::query::Context::new(context_type);
        if !source.query(&mut query) {
            return None;
        }
        query.context_owned()
    }

    /// Set this generation's compositor app-facing presentation: the `wl_output` logical
    /// mode (`render`, `None` ⇒ follow the encode size) and the `wp_fractional_scale_v1`
    /// `preferred_scale`. Returns whether the compositor took them.
    ///
    /// The mode goes over as ONE atomic `render-size` string. There is no int-pair
    /// fallback: setting width and height as two properties lets the intermediate
    /// `w_new × h_old` mode reach the compositor (a live 1920x720 was observed between
    /// the two calls).
    ///
    /// Guarded on `find_property`, never set blind: `set_property` on an absent property
    /// PANICS in gstreamer-rs, and these exist only on a gst-wayland-display build
    /// carrying the render-size patch. An older image warns and no-ops.
    pub fn set_display_properties(&self, render: Option<(i32, i32)>, ui_scale: f64) -> bool {
        let Some(source) = self.pipeline.by_name("video-source") else {
            tracing::warn!(
                token = "display-update-no-video-source",
                "display update: source pipeline has no video-source element"
            );
            return false;
        };
        if source.find_property(RENDER_SIZE).is_none() || source.find_property(UI_SCALE).is_none() {
            tracing::warn!(
                token = "compositor-no-render-size",
                "compositor lacks render-size properties (old gst-wayland-display); \
                 display update ignored"
            );
            return false;
        }
        let size = match render {
            Some((w, h)) => format!("{w}x{h}"),
            // "0x0" is the compositor's "follow the encode size" reset.
            None => "0x0".to_string(),
        };
        source.set_property(RENDER_SIZE, size.as_str());
        source.set_property(UI_SCALE, ui_scale);
        tracing::info!(
            "session {}: compositor display set to render-size {size} @ ui_scale {ui_scale}",
            self.session_id,
        );
        true
    }

    /// Advertise the session's rung ladder (`session::rungs::available_rungs` of the
    /// launch size) as this compositor's `wl_output` modes. The internal half of adaptive
    /// external resolution, independent of the encoded size. Must be set once per source
    /// generation BEFORE the compositor starts: an app that reads its output's mode list
    /// once at startup has to see the full ladder. Guarded on `find_property` for the
    /// same panic reason as [`Self::set_display_properties`]; an older image debug-logs
    /// and no-ops. Returns whether it was taken.
    pub fn set_mode_ladder(&self, rungs: &[(i32, i32)]) -> bool {
        let Some(source) = self.pipeline.by_name("video-source") else {
            tracing::warn!(
                token = "mode-ladder-no-video-source",
                "mode ladder: source pipeline has no video-source element"
            );
            return false;
        };
        if source.find_property(MODE_LADDER).is_none() {
            tracing::debug!(
                "compositor lacks the {MODE_LADDER} property (older gst-wayland-display); \
                 the guest sees a single output mode"
            );
            return false;
        }
        let ladder = super::rungs::format_ladder(rungs);
        source.set_property(MODE_LADDER, ladder.as_str());
        tracing::info!(
            "session {}: compositor mode ladder set to {ladder}",
            self.session_id,
        );
        true
    }

    /// Whether the compositor has produced its first frame. This is NOT "the app is on
    /// screen": `gst-wayland-display` renders its own empty scene with no client surface
    /// mapped, so it flips within milliseconds of PLAYING, long before any application
    /// has drawn. Swapping the encoder on this alone cut a swap to black; the swap gate
    /// uses [`Self::app_surface_commits`] + [`Self::sink_frames`] instead. Retained for
    /// no-app-container generations (bare compositor, `use_test_src`), the only place it
    /// is the sole available signal.
    pub fn first_frame_ready(&self) -> bool {
        self.first_frame.load(Ordering::Relaxed)
    }

    /// Compositor output buffers delivered to this generation's `interpipesink` so far;
    /// the field doc says why the count matters.
    pub fn sink_frames(&self) -> u64 {
        self.sink_frames.load(Ordering::Relaxed)
    }

    /// Lifetime buffers committed by a mapped top-level APPLICATION surface in this
    /// generation's compositor. `None` when the element does not export the counter (a
    /// `waylanddisplaysrc` without the vendored app-cadence patch, or `videotestsrc`);
    /// `Some(0)` means the compositor is up but no app has drawn yet.
    pub fn app_surface_commits(&self) -> Option<u64> {
        let source = self.pipeline.by_name("video-source")?;
        source.find_property(APP_SURFACE_COMMITS)?;
        Some(source.property::<u64>(APP_SURFACE_COMMITS))
    }

    /// Whether this generation launches an app container at all. `false` for a bare
    /// compositor and for `use_test_src`: neither can ever report an app-surface commit,
    /// so the swap gate must not wait for one.
    pub fn expects_app_container(&self) -> bool {
        self.container_spec.is_some()
    }

    /// Consume the container-launch failure, if any. Take semantics: callers poll once
    /// per loop iteration and must see it exactly once, so the fail-closed path fires
    /// a single time per failed launch rather than on every subsequent poll.
    pub fn take_launch_error(&mut self) -> Option<String> {
        self.launch_error.take()
    }

    /// Take the container's terminal exit status, if the waiter thread spawned in
    /// [`Self::launch`] has observed one since the last call. Take semantics mirror
    /// [`Self::take_launch_error`]: the runner must act on a given exit exactly once.
    pub fn take_container_exit(&self) -> Option<AppExitStatus> {
        self.observation.take_exit()
    }

    /// Whether the CURRENT app container has presented a frame. Built on
    /// [`Self::app_surface_commits`], never [`Self::first_frame_ready`] or
    /// [`Self::sink_frames`]: those count COMPOSITOR output, which flows within
    /// milliseconds of PLAYING with no client attached, so they are `true` even for an
    /// app that never started. Compared against [`Self::app_commits_at_launch`], not
    /// zero, for the reason that field's doc gives.
    ///
    /// A missing counter (`None`) returns `true`, declining to classify: it must never
    /// manufacture an `app_exited_early` verdict on a session that may have been
    /// streaming fine.
    pub fn app_has_presented(&self) -> bool {
        match self.app_surface_commits() {
            Some(now) => app_presented_since_launch(self.app_commits_at_launch, now),
            None => true,
        }
    }

    /// The app container's retained log lines, oldest first. Empty when nothing was
    /// captured (no container, follower failed to start, or a silent app).
    ///
    /// RuntimeClient supplies this bounded ring from its owned API log reads. Terminal
    /// observation replaces the running snapshot before it publishes an exit, preserving
    /// the application's final lines even when cleanup follows immediately.
    pub fn app_log_tail(&mut self) -> Vec<String> {
        self.observation.log_tail()
    }

    /// The app's configured exit policy. `fail` when no app is configured at all, where
    /// liveness is a no-op anyway since no waiter is ever spawned.
    pub fn exit_policy(&self) -> AppExitPolicy {
        self.container_spec
            .as_ref()
            .map(|s| s.on_app_exit)
            .unwrap_or_default()
    }

    /// Hold the app-container launch back until [`Self::launch_deferred_app`] is called
    /// explicitly, instead of firing it from the compositor's Wayland announcement. The
    /// serialised swap path needs this: two app containers bind-mounting the same managed
    /// home cannot coexist (Steam's second instance finds the first's lock, hands off,
    /// and exits 0). Must be called before [`Self::start`].
    pub fn defer_app_launch(&mut self) {
        self.defer_launch = true;
    }

    /// Whether the compositor has announced its Wayland socket, so a deferred
    /// launch is now possible. Only meaningful under [`Self::defer_app_launch`].
    pub fn compositor_socket_ready(&self) -> bool {
        self.pending_display.is_some()
    }

    /// Perform the launch held back by [`Self::defer_app_launch`]. No-op if the
    /// socket has not been announced yet, or if the app was already launched.
    pub fn launch_deferred_app(&mut self) {
        if self.launched {
            return;
        }
        let Some(display) = self.pending_display.take() else {
            return;
        };
        self.launched = true;
        self.launch(&display);
    }

    /// Feed every source-pipeline bus message here. The first `wayland.src` announcement
    /// launches the app container into the compositor once, or under
    /// [`Self::defer_app_launch`] records the socket for a later explicit launch.
    pub fn on_bus_message(&mut self, msg: &gst::Message) {
        if self.launched {
            return;
        }
        if let Some(display) = wayland_display_from_message(msg) {
            self.wl_display = Some(display.clone());
            if self.defer_launch {
                self.pending_display = Some(display);
                return;
            }
            self.launched = true;
            self.launch(&display);
        }
    }

    /// Stop just this generation's app container, blocking until it has actually exited,
    /// and leave the compositor source pipeline PLAYING. The serialised-swap primitive;
    /// the split matters twice:
    ///  - the container must be GONE, not merely signalled, before the replacement
    ///    launches, because the exit is what releases the lock in the shared managed
    ///    home. `RunningContainer::stop` uses the owned RuntimeClient stop and cleanup
    ///    operations; it returns only after that exact durable identity proves removal.
    ///  - the compositor keeps running, so the encode pipeline keeps being fed real (now
    ///    app-less) frames for the whole gap: the encoder never starves, PTS stay
    ///    continuous, and GCC never sees a dead media path. The user sees the empty
    ///    scene, which the client's transition screen covers.
    ///
    /// Returns `true` if a container was running and has now been reaped. The waiter
    /// thread's observation of this exit is discarded (the shared `removed` flag is set
    /// first), so it is never misreported as an app-liveness failure.
    pub fn stop_app_container(&mut self) -> Result<bool, String> {
        if self.container.is_none() {
            return Ok(false);
        }
        let stopped = self.container.as_mut().unwrap().stop();
        match stopped {
            Err(error) => {
                // A busy client never asked the engine, so it must not disarm
                // the export. An unconfirmed stop might still have a live bind.
                // This is a swap's outgoing generation: record the outcome and
                // leave the sidecar alone.
                let report = teardown::classify_stop(teardown::error_kind(&error));
                self.shared.note_container_stop(report);
                Err(error.to_string())
            }
            Ok(()) => {
                self.container.take();
                self.shared.note_container_stop(StopAttempt::Confirmed);
                Ok(true)
            }
        }
    }

    /// Relaunch this generation's app container into its still-live compositor: the
    /// rollback for a serialised swap that failed AFTER the outgoing app was stopped. The
    /// app restarts from scratch (its process state is gone, the cost of stopping it
    /// first), but the session, compositor, encoder and transport all survive.
    ///
    /// `Ok(())` when there is nothing to relaunch or the relaunch succeeded. `Err` means
    /// the session is now a live compositor with no app in it — the caller must treat
    /// that as fatal rather than report a rollback that did not happen.
    pub fn relaunch_app(&mut self) -> Result<(), String> {
        if self.container.is_some() {
            return Ok(());
        }
        if self.container_spec.is_none() {
            return Ok(());
        }
        let Some(display) = self.wl_display.clone() else {
            return Err("compositor socket for the previous app is unknown".to_string());
        };
        // `launch` records its own failure in `launch_error` for the runner's fail-closed
        // poll; consume it here so the rollback reports it once and the runner does not
        // also trip on it a moment later.
        let _ = self.launch_error.take();
        self.launch(&display);
        match self.launch_error.take() {
            Some(e) => Err(e),
            None if self.container.is_some() => Ok(()),
            None => Err("relaunch produced no container".to_string()),
        }
    }

    fn launch(&mut self, wl_display: &str) {
        let Some(spec) = self.container_spec.clone() else {
            tracing::info!(
                "source '{}': no app image configured (bare compositor)",
                self.sink_name
            );
            return;
        };
        let mut effective_spec = spec;
        if let Some((server_uri, dir)) = &self.pulse {
            effective_spec
                .env
                .insert("PULSE_SERVER".to_string(), server_uri.clone());
            // No PULSE_COOKIE: the sidecar socket grants anonymous auth, so a shared
            // cookie is neither needed nor reliable across the pressure-vessel sandbox
            // (host.rs / audio::pulse_run_args). PULSE_SINK routes name-picking clients
            // to the session's baked-in null-sink; an operator PULSE_SINK from the app
            // catalog wins, so only default when the app config did not set one.
            effective_spec
                .env
                .entry("PULSE_SINK".to_string())
                .or_insert_with(|| super::audio::QUASAR_SINK_NAME.to_string());
            // Point pulse-aware apps at the session's remapped mic source by name so
            // voice chat finds it without a device picker. The device always exists
            // (baked into the sidecar argv) and is simply silent unless this session
            // negotiated a mic m-line. Catalog env wins, as with PULSE_SINK.
            effective_spec
                .env
                .entry("PULSE_SOURCE".to_string())
                .or_insert_with(|| super::audio::QUASAR_MIC_SOURCE_NAME.to_string());
            effective_spec.mounts.push(format!("{dir}:{dir}"));
        }
        let params = LaunchParams {
            session_id: &self.session_id,
            wayland_display: wl_display,
            runtime_dir: &self.runtime_dir,
            device_nodes: self.device_nodes.clone(),
            container_name: Some(self.container_name.clone()),
            nvidia_lib32_path: &self.nvidia_lib32_path,
            display: self.display,
        };
        // Snapshot the commit counter BEFORE the container exists, so anything counted
        // from here on can only have been drawn by the container about to start. Read
        // before `run`, never after: the app can commit its first surface while `docker
        // run` is still returning.
        self.app_commits_at_launch = self.app_surface_commits();
        // A FRESH observation record per launch attempt, installed BEFORE the engine is
        // asked for a container. `AppSource` outlives its container across a rollback
        // relaunch, so a shared exit slot or log ring would report the previous
        // container's exit or dying words as the replacement app's failure — and a
        // launch that fails must not expose the retired generation's evidence either.
        // The previous observer keeps only its own retired handles and publishes into
        // them harmlessly.
        self.observation = GenerationObservation::new();
        match self.runtime.run(&effective_spec, &params) {
            Ok(c) => {
                tracing::info!(
                    "source '{}': app container '{}' launched into compositor socket '{}'",
                    self.sink_name,
                    c.name(),
                    wl_display
                );
                // The RuntimeClient observer fills final API log evidence before it
                // publishes terminal status, including an application that dies immediately.
                // It receives only THIS launch's observation handles.
                let observer = self.observation.observer(c.removed_flag());
                self.spawn_exit_waiter(c.application_id(), observer);
                self.container = Some(c);
                self.shared.mount_live.store(true, Ordering::Relaxed);
            }
            Err(e) => {
                let msg = format!("{e:#}");
                tracing::error!(
                    token = "app-container-launch-failed-source",
                    "source '{}': container launch failed: {msg}",
                    self.sink_name
                );
                // #378: stash it so the runner's bus pump can fail the session closed.
                // Discarding it leaves the session `running` forever with a dead
                // compositor and no app inside it.
                self.launch_error = Some(msg);
            }
        }
    }

    /// Spawn the dedicated RuntimeClient observer for a just-launched container. One thread
    /// per generation owns only observation: each request is bounded, the intentional-stop
    /// marker ends the loop, and cancellation never asks the engine to stop the workload.
    /// A verified terminal result supplies final logs before this thread publishes status.
    fn spawn_exit_waiter(
        &self,
        application: crate::runtime::ApplicationId,
        observer: GenerationObserver,
    ) {
        let sink_name = self.sink_name.clone();
        let thread_sink_name = sink_name.clone();
        let builder = std::thread::Builder::new().name("quasar-app-wait".to_string());
        let log_span = tracing::Span::current();
        if let Err(e) = builder.spawn(move || {
            // Re-enter the session span so this thread's lines carry session=<id>.
            let _log_span = log_span.enter();
            let sink_name = thread_sink_name;
            let observed = application.clone();
            let tailed = application;
            observe_until_exit(
                &observer,
                &sink_name,
                || {
                    crate::runtime::configured()
                        .and_then(|api| api.observe_application(observed.clone()).wait())
                },
                || {
                    crate::runtime::configured()
                        .and_then(|api| api.application_log_tail(tailed.clone()).wait())
                        .ok()
                },
                || std::thread::sleep(OBSERVATION_RETRY_DELAY),
            );
        }) {
            tracing::warn!(
                token = "app-liveness-waiter-spawn-failed",
                "source '{sink_name}': failed to spawn app-liveness waiter thread: {e} — \
                 an app exit for this generation will go undetected"
            );
        }
    }

    /// Tear down (idempotent): stop the app container, release the pulse sidecar
    /// and the udev export, then NULL the pipeline. The release happens before
    /// the state change so a pipeline NULL that blocks — the idle-reap case,
    /// where the encode consumer is still PLAYING — cannot skip it.
    ///
    /// This is the session-end path. [`Drop`] stops this generation's container
    /// only: a swap drops the outgoing source while the replacement still needs
    /// the sidecar and the udev export.
    pub fn teardown(&mut self) {
        let report = self.finish_app_stop();
        self.shared.apply(report, false);
        let _ = self.pipeline.set_state(gst::State::Null);
    }

    /// One engine attempt per unconfirmed outcome. Busy is retried here and
    /// again from [`Drop`] if this call exhausted its budget, because that
    /// failure never reached the engine.
    fn finish_app_stop(&mut self) -> StopAttempt {
        if let Some(report) = self.ended {
            if !matches!(report, StopAttempt::Retryable) {
                return report;
            }
        }
        let report = self.stop_app_for_session_end();
        self.ended = Some(report);
        report
    }

    fn stop_app_for_session_end(&mut self) -> StopAttempt {
        teardown::retry_retryable(
            || self.stop_app_once(),
            || std::thread::sleep(teardown::STOP_PAUSE),
            teardown::STOP_ATTEMPTS,
        )
    }

    fn stop_app_once(&mut self) -> StopAttempt {
        if self.container.is_none() {
            return StopAttempt::Absent;
        }
        let stopped = self.container.as_mut().unwrap().stop();
        match stopped {
            Ok(()) => {
                self.container.take();
                self.shared.mount_live.store(false, Ordering::Relaxed);
                StopAttempt::Confirmed
            }
            Err(error) => {
                let report = teardown::classify_stop(teardown::error_kind(&error));
                if !matches!(report, StopAttempt::Retryable) {
                    tracing::warn!(
                        token = "application-teardown-pending",
                        "runtime application teardown remains durable: {error}"
                    );
                }
                report
            }
        }
    }
}

/// How long the observer waits before re-inspecting after a non-terminal answer.
/// The engine is polled by exact identity — there is no event stream to resubscribe
/// to — so this is also the reconciliation interval after an engine gap.
const OBSERVATION_RETRY_DELAY: std::time::Duration = std::time::Duration::from_millis(250);

/// The observer loop for one generation, with its engine calls injected.
///
/// A bounded runtime observation is not terminal evidence: keep observing a silent,
/// healthy game past its per-request deadline, and treat every error — including a
/// daemon that has gone away entirely — as "ask again", never as an exit. Only a
/// verified `ApplicationResult` ends the loop, and it publishes exactly once into this
/// generation's slot, after its final logs are recorded.
///
/// `observe` and `log_tail` are the RuntimeClient calls in production. `stopped` is
/// checked before every observation so our own teardown ends the loop without
/// publishing anything. What this function cannot show — that no observation ever
/// asks the engine to start or stop the workload — is proven engine-side by
/// `runtime::helper_tests::application_observation_survives_an_engine_gap_and_reports_the_true_exit_once`.
fn observe_until_exit(
    observer: &GenerationObserver,
    sink_name: &str,
    mut observe: impl FnMut() -> Result<crate::runtime::ApplicationResult, crate::runtime::RuntimeError>,
    mut log_tail: impl FnMut() -> Option<crate::runtime::ApplicationLogTail>,
    mut backoff: impl FnMut(),
) {
    let status = loop {
        if observer.stopped_intentionally() {
            return;
        }
        match observe() {
            Ok(result) => {
                for line in result.stdout.lines().chain(result.stderr.lines()) {
                    observer.record_line(line.to_owned());
                }
                if result.oom_killed == Some(true) {
                    break AppExitStatus::OomKilled;
                }
                break result
                    .exit_code
                    .and_then(|code| i32::try_from(code).ok())
                    .map(AppExitStatus::Code)
                    .unwrap_or(AppExitStatus::Unknown);
            }
            Err(error) if retry_application_observation(error.kind) => {
                // Readiness needs evidence while a game is still alive. This bounded
                // read is observational and cannot alter its lifecycle; final logs
                // replace it on terminal observe.
                if let Some(tail) = log_tail() {
                    for line in tail.stdout.lines().chain(tail.stderr.lines()) {
                        observer.record_line(line.to_owned());
                    }
                }
                backoff();
            }
            Err(error) => {
                tracing::warn!(
                    token = "application-wait-failed",
                    "runtime application wait failed: {error}"
                );
                backoff();
            }
        }
    };
    // A deliberate stop (swap teardown, session stop) sets the shared `removed` flag
    // BEFORE the engine sees the stop (`RunningContainer::stop`), so if it is set here
    // the exit just observed is our own teardown, not an app failure. `publish`
    // discards it; and it can only ever reach THIS generation's slot.
    if observer.publish(status) {
        tracing::info!("source '{sink_name}': app container exited: {status:?}");
    } else {
        tracing::debug!(
            "source '{sink_name}': app container exit observed after our own teardown \
             (status={status:?}) — ignoring, not a liveness failure"
        );
    }
}

impl Drop for AppSource {
    fn drop(&mut self) {
        let report = self.finish_app_stop();
        // SessionResources plus this source is 2. A swap holds the outgoing and
        // incoming generations at once; releasing the sidecar or the udev
        // export from that drop would strand the replacement. `strong_count`
        // is exact here: every clone is taken on this thread.
        if Arc::strong_count(&self.shared) > 2 {
            self.shared.note_container_stop(report);
        } else {
            // Before NULL. A source NULL can block while an interpipe listener
            // is still PLAYING, and that must not skip the release. Covers
            // terminal returns that never reached `teardown`.
            self.shared.apply(report, false);
        }
        let _ = self.pipeline.set_state(gst::State::Null);
    }
}

#[cfg(test)]
mod tests {
    use super::{
        app_presented_since_launch, observe_until_exit, retry_application_observation,
        source_commit_advanced, GenerationObservation,
    };
    use crate::runtime::{ApplicationResult, ErrorKind, RuntimeError};
    use crate::session::container::AppExitStatus;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::Arc;

    // One observation record per launched generation. The observer thread of a
    // generation holds only that generation's handles, so nothing it publishes late can
    // land in the state the runner polls for the replacement (a rolled-back swap
    // relaunches into the same `AppSource`).

    #[test]
    fn a_previous_generations_late_exit_is_never_reported_for_the_replacement() {
        let mut previous = GenerationObservation::new();
        let previous_observer = previous.observer(Arc::new(AtomicBool::new(false)));
        // The rollback relaunch replaces the observation record before it spawns a new
        // observer, exactly as `AppSource::launch` does.
        let replacement = GenerationObservation::new();
        assert!(previous_observer.publish(AppExitStatus::Code(0)));
        assert_eq!(replacement.take_exit(), None);
        assert_eq!(previous.take_exit(), Some(AppExitStatus::Code(0)));
        assert_eq!(previous.take_exit(), None, "take semantics: reported once");
    }

    #[test]
    fn an_intentional_stop_suppresses_the_exit_publication() {
        let mut generation = GenerationObservation::new();
        let removed = Arc::new(AtomicBool::new(false));
        let observer = generation.observer(removed.clone());
        // `RunningContainer::stop_with` sets the marker BEFORE the engine sees a stop.
        removed.store(true, Ordering::SeqCst);
        assert!(observer.stopped_intentionally());
        assert!(!observer.publish(AppExitStatus::OomKilled));
        assert_eq!(generation.take_exit(), None);
    }

    #[test]
    fn an_exit_published_just_before_the_stop_marker_is_discarded_at_read_time() {
        // The observer checked the marker, found it clear, and published; the stop set the
        // marker a moment later, before the runner polled. The runner must not see it.
        let mut generation = GenerationObservation::new();
        let removed = Arc::new(AtomicBool::new(false));
        let observer = generation.observer(removed.clone());
        assert!(observer.publish(AppExitStatus::Code(0)));
        removed.store(true, Ordering::SeqCst);
        assert_eq!(generation.take_exit(), None);
    }

    #[test]
    fn a_previous_generations_log_lines_never_reach_the_replacement_tail() {
        let mut previous = GenerationObservation::new();
        let previous_observer = previous.observer(Arc::new(AtomicBool::new(false)));
        let mut replacement = GenerationObservation::new();
        let replacement_observer = replacement.observer(Arc::new(AtomicBool::new(false)));
        previous_observer.record_line("Steam needs to be online to update".into());
        replacement_observer.record_line("replacement booting".into());
        assert_eq!(
            previous.log_tail(),
            vec!["Steam needs to be online to update"]
        );
        assert_eq!(replacement.log_tail(), vec!["replacement booting"]);
    }

    // `app-surface-commits` is a LIFETIME total on a compositor that outlives its app
    // container. Scoping "did it ever draw?" to the current container is what keeps a
    // relaunch from inheriting the previous container's frames.

    #[test]
    fn a_replacement_container_that_never_drew_is_not_reported_as_presented() {
        // Generation 1 drew 400 frames, then died. The relaunch snapshots 400 as its
        // baseline and the replacement draws nothing.
        assert!(
            !app_presented_since_launch(Some(400), 400),
            "a replacement that never drew inherited the previous container's commits — \
             the early-exit classification would be silently suppressed on every retry"
        );
        // One frame past the baseline IS the replacement drawing.
        assert!(app_presented_since_launch(Some(400), 401));
    }

    #[test]
    fn no_observation_error_is_published_as_an_application_exit() {
        for kind in [
            ErrorKind::Timeout,
            ErrorKind::Unavailable,
            ErrorKind::Busy,
            ErrorKind::UnknownOutcome,
            ErrorKind::Cancelled,
            ErrorKind::Protocol,
            ErrorKind::Engine,
            ErrorKind::InvalidConfiguration,
        ] {
            assert!(
                retry_application_observation(kind),
                "{kind:?} is missing terminal evidence and must leave a live app alone"
            );
        }
    }

    /// The engine goes away mid-session and the application exits during the gap.
    /// There is no event stream to resubscribe to, so the observer must keep asking
    /// and reconcile the true exit from the first answer it gets back — publishing
    /// nothing meanwhile, and exactly once afterwards. Nothing in this loop asks the
    /// engine to start or stop anything; that half is proven engine-side by
    /// `runtime::helper_tests::application_observation_survives_an_engine_gap_and_reports_the_true_exit_once`.
    #[test]
    fn the_observer_rides_out_an_engine_gap_and_publishes_the_true_exit_once() {
        let mut generation = GenerationObservation::new();
        let observer = generation.observer(Arc::new(AtomicBool::new(false)));
        let observations = std::cell::Cell::new(0usize);
        let gap = std::cell::Cell::new(0usize);
        observe_until_exit(
            &observer,
            "gap-source",
            || {
                observations.set(observations.get() + 1);
                if observations.get() <= 3 {
                    // Each failed answer is also the runner's chance to see there is
                    // still no exit: a published error would end the session here.
                    assert_eq!(generation.take_exit(), None, "an engine gap is not an exit");
                    return Err(RuntimeError::from(ErrorKind::Unavailable));
                }
                assert_eq!(observations.get(), 4, "observed after the terminal result");
                Ok(ApplicationResult {
                    exit_code: Some(7),
                    oom_killed: Some(false),
                    stdout: "last words".into(),
                    stderr: String::new(),
                })
            },
            || {
                gap.set(gap.get() + 1);
                None
            },
            || {},
        );
        assert_eq!(
            observations.get(),
            4,
            "the terminal answer must end the loop"
        );
        assert_eq!(
            gap.get(),
            3,
            "each gap observation still feeds readiness a tail"
        );
        assert_eq!(generation.take_exit(), Some(AppExitStatus::Code(7)));
        assert_eq!(
            generation.take_exit(),
            None,
            "take semantics: reported once"
        );
        assert_eq!(generation.log_tail(), vec!["last words"]);
    }

    /// The same loop against our OWN teardown: the marker is set before the engine
    /// sees the stop, so the observer must leave without asking or publishing.
    #[test]
    fn the_observer_publishes_nothing_once_our_own_teardown_has_begun() {
        let mut generation = GenerationObservation::new();
        let removed = Arc::new(AtomicBool::new(true));
        let observer = generation.observer(removed);
        observe_until_exit(
            &observer,
            "stopped-source",
            || panic!("an intentional stop must not observe again"),
            || None,
            || panic!("no backoff after an intentional stop"),
        );
        assert_eq!(generation.take_exit(), None);
    }

    #[test]
    fn the_first_generation_compares_against_a_zero_baseline() {
        assert!(!app_presented_since_launch(Some(0), 0));
        assert!(app_presented_since_launch(Some(0), 1));
    }

    /// No baseline recorded (nothing was ever launched through this source, or
    /// the counter was unavailable at launch): fall back to the absolute read.
    #[test]
    fn an_absent_baseline_falls_back_to_the_absolute_reading() {
        assert!(!app_presented_since_launch(None, 0));
        assert!(app_presented_since_launch(None, 7));
    }

    /// A counter that goes backwards means the compositor element was replaced.
    /// The stale baseline must not make a drawing app read as never having drawn.
    #[test]
    fn a_compositor_reset_falls_back_to_the_absolute_reading() {
        assert!(app_presented_since_launch(Some(400), 3));
        assert!(!app_presented_since_launch(Some(400), 0));
    }

    #[test]
    fn source_commit_delta_is_deduplicated_per_output_observation() {
        assert_eq!(source_commit_advanced(4, 4), 0);
        assert_eq!(source_commit_advanced(5, 4), 1);
        assert_eq!(source_commit_advanced(9, 4), 1);
        // A compositor restart/reset must not create a synthetic source frame.
        assert_eq!(source_commit_advanced(1, 9), 0);
    }
}
