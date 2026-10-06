//! The console seam of the session runner: every local-display concern (the console
//! terminal hold, the local-only session, the dual-output fan-out of a streamed session)
//! is reached through this module, so the streaming path never touches the display.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use gstreamer as gst;
use gstreamer::prelude::*;

use super::{
    app_exit_event, apply_display_update, compute_bytes_used, ipsink_name, perform_swap,
    renderer_degraded_failure, start_first_source, DiagnosticEventTx, DisplayUpdateRequest,
    RendererDegradeTracker, SessionEvent, SwapRequest, TraceEvent, VulkanContextBridge, POLL,
};
use crate::messages::{AppExitPolicy, VideoTopology};
use crate::session::console_vt::ConsoleVt;
use crate::session::metrics::SessionMetrics;
use crate::session::source::{AppSource, SessionResources};
use crate::session::{console, pipeline, vulkan_fault, SessionConfig};

/// #407: a console session holds the console VT, keyboard off, from before its virtual
/// keyboard exists until after it is gone, so nothing typed in the session reaches a host
/// login prompt. The caller must declare it before `res`, weston and the physical input
/// forwarder so it drops after them. Fail-closed: `Err` is the `Failed` reason.
pub(super) fn take_terminal(
    cfg: &SessionConfig,
    stop: &Arc<AtomicBool>,
) -> Result<Option<ConsoleVt>, String> {
    let wants = cfg.console_config.as_ref().is_some_and(|c| c.enabled)
        && matches!(
            cfg.video_topology,
            VideoTopology::LocalOnly | VideoTopology::DualOutput
        );
    if !wants {
        return Ok(None);
    }
    ConsoleVt::take(stop.clone()).map(Some).map_err(|e| {
        tracing::error!(
            token = "runner-console-vt-failed",
            error = %format_args!("{e:#}"),
            "console session refused: the console terminal could not be taken"
        );
        format!("console terminal: {e:#}")
    })
}

/// Why the console terminal was lost mid-session, if it was.
pub(super) fn terminal_lost(vt: &mut Option<ConsoleVt>) -> Option<String> {
    vt.as_mut().and_then(|vt| vt.lost())
}

/// A console session's real output is the physical display, so it must never be reaped
/// for a missing or lost WebRTC transport, local-only or dual-output.
pub(super) fn holds_physical_display(cfg: &SessionConfig) -> bool {
    cfg.console_config.as_ref().is_some_and(|c| c.enabled)
}

/// Whether the session streams audio to the browser. Must stay the condition
/// `pipeline::build_encode_pipeline` builds the audio pipeline on.
pub(super) fn streams_audio(cfg: &SessionConfig) -> bool {
    cfg.video_topology != VideoTopology::DualOutput
        || cfg.console_config.as_ref().is_none_or(|c| c.stream_audio)
}

/// The console's `stream_audio` and `connector` settings as the effective-media snapshot
/// reports them.
pub(super) fn snapshot_settings(cfg: &SessionConfig) -> (bool, &str) {
    let console = cfg.console_config.as_ref();
    (
        console.is_none_or(|c| c.stream_audio),
        console.map(|c| c.connector.as_str()).unwrap_or("auto"),
    )
}

/// Fail the whole DualOutput session when a required console local-display leg cannot
/// be built or played: for a console session the local monitor IS the purpose, so emit
/// `Failed` and tear down source + encode + audio rather than stream to the browser
/// with a black monitor. Mirrors `run_local_only`'s hard-fail discipline.
pub(super) fn fail_dualoutput_console<F: Fn(SessionEvent)>(
    emit: &F,
    msg: String,
    current_source: &mut AppSource,
    encode_pipe: &gst::Pipeline,
    audio_pipeline: Option<&gst::Pipeline>,
    defer_encode_teardown: bool,
) {
    tracing::error!(
        token = "runner-console-dualoutput-failed",
        reason = %msg,
        "console dual-output session failed"
    );
    emit(SessionEvent::Failed(msg));
    current_source.teardown();
    crate::session::nvenc_defer::finish_encode(encode_pipe, defer_encode_teardown);
    if let Some(ap) = audio_pipeline {
        let _ = ap.set_state(gst::State::Null);
    }
}
/// The console fan-out of a streamed session: the local display (weston plus a third
/// interpipe listener into waylandsink, or kmssink), local audio and the physical-input
/// grab. Field order is drop order and is load-bearing: the grab is released first (a
/// leaked grab locks host input), and waylandsink's surface before weston is killed.
pub(super) struct DualOutputLeg {
    _physical_input: Option<crate::session::physical_input::PhysicalInput>,
    _local_audio: Option<pipeline::LocalAudio>,
    local_display: Option<pipeline::LocalDisplay>,
    _weston: Option<console::WestonConsole>,
    backend: console::LocalBackend,
}

impl DualOutputLeg {
    /// Brings up whatever the console config asks of a streamed session. `Err` is the
    /// `Failed` reason of a real console session whose local display could not come up
    /// (fail-closed); the dev-only `QUASAR_LOCAL_DISPLAY` fallback stays best-effort.
    #[allow(clippy::too_many_arguments)]
    pub(super) fn start(
        cfg: &SessionConfig,
        session_id: &str,
        sink0: &str,
        res: &SessionResources,
        shared_clock: &gst::Clock,
        shared_base: gst::ClockTime,
        vulkan_contexts: Option<&VulkanContextBridge>,
        cuda_ctx: Option<&gst::Context>,
        va_ctx: Option<&gst::Context>,
    ) -> Result<Self, String> {
        let console_enabled = match cfg.console_config.as_ref() {
            Some(cc) => cc.enabled && cfg.video_topology == VideoTopology::DualOutput,
            None => std::env::var("QUASAR_LOCAL_DISPLAY")
                .map(|v| !v.trim().is_empty())
                .unwrap_or(false),
        };
        let console_required = cfg
            .console_config
            .as_ref()
            .map(|cc| cc.enabled)
            .unwrap_or(false)
            && cfg.video_topology == VideoTopology::DualOutput;
        if let Some(cc) = cfg.console_config.as_ref() {
            if cc.enabled && cc.compositor != "weston" {
                tracing::info!(
                    token = "console-compositor-not-wired",
                    "console_config.compositor={:?} not yet wired (CM-04); using weston",
                    cc.compositor
                );
            }
        }
        let local_backend = console::local_backend(cfg.console_config.as_ref());
        let weston: Option<console::WestonConsole> = 'weston: {
            if !console_enabled || local_backend == console::LocalBackend::DirectKms {
                break 'weston None;
            }
            match console::spawn_weston_console(session_id, cfg.console_config.as_ref()) {
                Ok(w) => Some(w),
                Err(e) => {
                    tracing::warn!(
                        token = "console-weston-spawn-failed",
                        "spawn weston console failed: {e:#} (local display disabled)"
                    );
                    None
                }
            }
        };
        let local_display: Option<pipeline::LocalDisplay> = 'local_display: {
            if !console_enabled {
                break 'local_display None;
            }
            // If Weston was selected but failed to come up, don't build the fan-out.
            if local_backend == console::LocalBackend::Weston && weston.is_none() {
                if console_required {
                    return Err(
                        "console: weston compositor failed to start for local display".into(),
                    );
                }
                break 'local_display None;
            }
            let socket = weston.as_ref().map(|weston| weston.socket.as_str());
            let ld = match pipeline::build_local_display_pipeline(cfg, sink0, socket) {
                Ok(ld) => ld,
                Err(e) => {
                    if console_required {
                        return Err(format!(
                            "console: build local-display pipeline failed: {e:#}"
                        ));
                    }
                    tracing::warn!(
                        token = "console-display-pipeline-build-failed",
                        "build local-display pipeline failed: {e:#}"
                    );
                    break 'local_display None;
                }
            };
            // Same clock+base as the source/encode pipelines (#68).
            ld.pipeline.set_start_time(None::<gst::ClockTime>);
            ld.pipeline.use_clock(Some(shared_clock));
            ld.pipeline.set_base_time(shared_base);
            // The leg consumes the shared VulkanImage interpipe via `vulkandownload`, which
            // needs the producer's Vulkan instance+device to map its images. Inject the
            // gen-0 contexts BEFORE this pipeline's NULL->READY transition or
            // `vulkandownload` binds a private device first (the va_share lesson) and the
            // browser stream runs fine while the monitor stays black.
            if let Some(vk) = vulkan_contexts {
                vk.install_on_pipeline(&ld.pipeline);
                tracing::info!(
                    gst_vulkan_device = format_args!("{:#x}", vk.device_identity),
                    "console: shared producer Vulkan instance/device installed on local-display pipeline"
                );
            }
            // Vulkan/VA need the shared GPU context adopted before PLAYING (mirror encode pipe).
            if let Some(ctx) = va_ctx {
                ld.pipeline.set_context(ctx);
                crate::session::va_share::install_need_context_handler(&ld.pipeline, ctx);
            }
            if let Some(ctx) = cuda_ctx {
                ld.pipeline.set_context(ctx);
            }
            if let Err(e) = ld.pipeline.set_state(gst::State::Playing) {
                if console_required {
                    // Consumer-before-source teardown: NULL the local-display pipeline
                    // before the caller tears down the producing source.
                    let _ = ld.pipeline.set_state(gst::State::Null);
                    return Err(format!(
                        "console: local-display pipeline failed to reach PLAYING: {e:#}"
                    ));
                }
                tracing::warn!(
                    token = "console-display-pipeline-play-failed",
                    "local-display pipeline PLAYING failed: {e:#} \
                     (console enabled but no local output)"
                );
                break 'local_display None;
            }
            tracing::info!(
                "console mode: local-display fan-out PLAYING (connector={}, backend={})",
                snapshot_settings(cfg).1,
                local_backend.name()
            );
            Some(ld)
        };

        // An independent `pulsesrc` client of the session's PulseAudio sidecar feeding a
        // host ALSA device. `audio_output: null` means console video only, so nothing is
        // built rather than built-then-muted. Needs no swap re-pointing: the sidecar is
        // session-scoped and outlives every app swap.
        let local_audio: Option<pipeline::LocalAudio> = 'local_audio: {
            let Some(cc) = cfg.console_config.as_ref() else {
                break 'local_audio None;
            };
            if !cc.enabled {
                break 'local_audio None;
            }
            let Some(audio_output) = cc.audio_output.as_deref() else {
                break 'local_audio None;
            };
            let la = match pipeline::build_local_audio_pipeline(cfg, audio_output) {
                Ok(la) => la,
                Err(e) => {
                    tracing::warn!(
                        token = "console-audio-pipeline-build-failed",
                        "build local-audio pipeline failed: {e:#}"
                    );
                    break 'local_audio None;
                }
            };
            // pulsesrc/alsasink do not participate in the interpipe running-time contract
            // (#68), but sharing the session clock+base keeps one coherent timebase.
            la.pipeline.set_start_time(None::<gst::ClockTime>);
            la.pipeline.use_clock(Some(shared_clock));
            la.pipeline.set_base_time(shared_base);
            if let Err(e) = la.pipeline.set_state(gst::State::Playing) {
                tracing::warn!(
                    token = "console-audio-pipeline-play-failed",
                    "local-audio pipeline PLAYING failed: {e:#} \
                     (console audio disabled, video unaffected)"
                );
                break 'local_audio None;
            }
            tracing::info!("console mode: local-audio fan-out PLAYING (device={audio_output})");
            Some(la)
        };

        // Physical keyboard/mouse grab, forwarded into the session's virtual devices
        // (`physical_input.rs` says why a forwarder, not a second compositor input path).
        // Gated on `enabled` AND `grab` AND a non-null `input_devices`, so console mode
        // without grab never touches physical devices.
        let physical_input: Option<crate::session::physical_input::PhysicalInput> = 'phys_input: {
            let Some(cc) = cfg.console_config.as_ref() else {
                break 'phys_input None;
            };
            if !cc.enabled || !cc.grab || cc.input_devices.is_null() {
                break 'phys_input None;
            }
            let Some(devices) = res.devices.as_ref() else {
                tracing::warn!(
                    token = "console-grab-no-virtual-devices",
                    "console-mode: grab requested but this session has no virtual input \
                     devices (use_test_src?) — physical input disabled"
                );
                break 'phys_input None;
            };
            Some(crate::session::physical_input::PhysicalInput::start(
                &cc.input_devices,
                cc.auto_connect_controller,
                devices,
            ))
        };

        Ok(Self {
            _physical_input: physical_input,
            _local_audio: local_audio,
            local_display,
            _weston: weston,
            backend: local_backend,
        })
    }

    /// The local backend's name while a local display is up, for the effective-media
    /// snapshot.
    pub(super) fn backend_name(&self) -> Option<&'static str> {
        self.local_display.as_ref().map(|_| self.backend.name())
    }

    /// NULLs the local display: consumers stop before the producing source is torn down.
    pub(super) fn halt_display(&self) {
        if let Some(ld) = &self.local_display {
            let _ = ld.pipeline.set_state(gst::State::Null);
        }
    }

    /// Re-points the local-display listener at a new source generation's interpipesink,
    /// alongside the encoder's.
    pub(super) fn follow_source(&self, sink: &str) {
        if let Some(ld) = &self.local_display {
            ld.interpipesrc.set_property("listen-to", sink);
        }
    }

    /// Logs the local display's delivered-frame cadence over `window`: delivered_fps ~0
    /// while the browser stream keeps running is the "monitor black" symptom.
    pub(super) fn log_cadence(&self, cfg: &SessionConfig, window: Duration) {
        let Some(ld) = &self.local_display else {
            return;
        };
        let frames = ld.drain_sink_frames();
        let seconds = window.as_secs_f64();
        let expected = (cfg.stream.fps as f64 * seconds).round() as u64;
        let missing = expected.saturating_sub(frames);
        tracing::info!(
            target_fps = cfg.stream.fps,
            frames,
            window_ms = window.as_millis() as u64,
            delivered_fps = format_args!("{:.2}", frames as f64 / seconds),
            missing,
            "local-display cadence"
        );
    }
}

#[allow(clippy::too_many_arguments)]
pub(super) fn run_local_only<F: Fn(SessionEvent)>(
    session_id: &str,
    launch_cfg: &SessionConfig,
    emit: &F,
    diagnostic_tx: DiagnosticEventTx,
    stop: Arc<AtomicBool>,
    swap_rx: std::sync::mpsc::Receiver<SwapRequest>,
    display_rx: std::sync::mpsc::Receiver<DisplayUpdateRequest>,
    res: &SessionResources,
    session_metrics: Arc<SessionMetrics>,
    shared_clock: &gst::Clock,
    shared_base: gst::ClockTime,
    cuda_ctx: Option<&gst::Context>,
    va_ctx: Option<&gst::Context>,
    mut console_vt: Option<&mut ConsoleVt>,
) {
    // Local-only MUST establish the physical DRM mode before the Vulkan capture compositor
    // creates its device/context: on NVIDIA, modesetting Weston after waylanddisplaysrc was
    // already PLAYING reproducibly faulted the source GPU channel (Xid 13/32, then
    // VK_ERROR_DEVICE_LOST).
    let local_backend = console::local_backend(launch_cfg.console_config.as_ref());
    let prestarted_weston = if local_backend == console::LocalBackend::Weston {
        match console::spawn_weston_console(session_id, launch_cfg.console_config.as_ref()) {
            Ok(weston) => Some(weston),
            Err(e) => {
                tracing::error!(
                    token = "runner-weston-prelaunch-failed",
                    error = %format_args!("{e:#}"),
                    "spawn weston console failed"
                );
                emit(SessionEvent::Failed(format!("spawn weston console: {e:#}")));
                return;
            }
        }
    } else {
        None
    };

    let mut gen_owned: u64 = 0;
    let gen = &mut gen_owned;
    let sink0 = ipsink_name(session_id, *gen);
    // #445: the monitor's REAL modes, each with its own refresh, advertised before the
    // compositor starts so the app's display settings list them and a choice comes back as
    // a mode request. Empty when no display is connected (then no request can be honoured).
    let mut console_modes: Vec<crate::messages::ConsoleModeSelection> = Vec::new();
    let Some((mut source_owned, vulkan_contexts)) = start_first_source(
        launch_cfg,
        session_id,
        &sink0,
        res,
        &session_metrics,
        shared_clock,
        shared_base,
        cuda_ctx,
        va_ctx,
        emit,
        |source| {
            let outputs = crate::capacity::detect_drm_outputs();
            console_modes =
                console::console_output_modes(launch_cfg.console_config.as_ref(), &outputs);
            if !console_modes.is_empty() {
                let triples: Vec<(i32, i32, i32)> = console_modes
                    .iter()
                    .map(|m| (m.width as i32, m.height as i32, m.refresh_millihz as i32))
                    .collect();
                source.set_output_modes(&triples);
                // #447: a nested display server with no wlr-output-management support
                // still resizes its own fullscreen window on a resolution change; treat
                // that resize as an implicit mode request so the console monitor follows.
                source.set_follow_client_size(true);
            }
        },
    ) else {
        return;
    };
    let current_source = &mut source_owned;
    let vulkan_contexts = vulkan_contexts.as_ref();
    let sink0 = sink0.as_str();
    emit(SessionEvent::Progress(
        "container started; first frame ready",
    ));

    // #445: the session's mode can move (a mode the app picked), so the config the
    // display pipeline, a swap and the cadence log read is this live copy, not the
    // launch config.
    let mut live_cfg: SessionConfig = launch_cfg.clone();
    let Some(cc) = live_cfg.console_config.clone().filter(|c| c.enabled) else {
        tracing::error!(
            token = "runner-local-only-console-required",
            "local_only assignment requires enabled console_config"
        );
        emit(SessionEvent::Failed(
            "local_only assignment requires enabled console_config".into(),
        ));
        current_source.teardown();
        return;
    };

    let mut weston = match (local_backend, prestarted_weston) {
        (console::LocalBackend::Weston, Some(weston)) => Some(weston),
        (console::LocalBackend::Weston, None) => {
            match console::spawn_weston_console(session_id, live_cfg.console_config.as_ref()) {
                Ok(weston) => Some(weston),
                Err(e) => {
                    tracing::error!(
                        token = "runner-weston-launch-failed",
                        error = %format_args!("{e:#}"),
                        "spawn weston console failed"
                    );
                    emit(SessionEvent::Failed(format!("spawn weston console: {e:#}")));
                    current_source.teardown();
                    return;
                }
            }
        }
        (console::LocalBackend::DirectKms, _) => None,
    };
    let mut local_display = match bring_up_local_display(
        &live_cfg,
        sink0,
        weston.as_ref().map(|weston| weston.socket.as_str()),
        current_source,
        shared_clock,
        shared_base,
        cuda_ctx,
        va_ctx,
    ) {
        Ok(ld) => ld,
        Err(e) => {
            tracing::error!(
                token = "runner-local-display-build-failed",
                error = %e,
                "local-display pipeline failed"
            );
            emit(SessionEvent::Failed(e));
            current_source.teardown();
            return;
        }
    };

    let local_audio = cc.audio_output.as_deref().and_then(|output| {
        match pipeline::build_local_audio_pipeline(&live_cfg, output) {
            Ok(la) => {
                la.pipeline.set_start_time(None::<gst::ClockTime>);
                la.pipeline.use_clock(Some(shared_clock));
                la.pipeline.set_base_time(shared_base);
                match la.pipeline.set_state(gst::State::Playing) {
                    Ok(_) => Some(la),
                    Err(e) => {
                        tracing::warn!(
                            token = "local-audio-play-failed",
                            "local-only audio set PLAYING failed: {e:#}"
                        );
                        None
                    }
                }
            }
            Err(e) => {
                tracing::warn!(
                    token = "local-audio-build-failed",
                    "local-only audio build failed: {e:#}"
                );
                None
            }
        }
    });
    let _physical_input = if cc.grab && !cc.input_devices.is_null() {
        res.devices.as_ref().map(|devices| {
            crate::session::physical_input::PhysicalInput::start(
                &cc.input_devices,
                cc.auto_connect_controller,
                devices,
            )
        })
    } else {
        None
    };

    tracing::info!(
        "local-only topology PLAYING (connector={}, backend={}, encode_slots=0, signaling=none)",
        cc.connector,
        local_backend.name()
    );
    emit(SessionEvent::Progress("local display pipeline ready"));
    emit(SessionEvent::Running);

    // #445: the mode the local display runs at, as the monitor names it (the DRM entry
    // whose size is the session's and whose refresh is nearest the session fps), reported
    // on every metrics window. `None` when no display is connected.
    let mut console_current: Option<crate::messages::ConsoleModeSelection> = console_modes
        .iter()
        .filter(|m| {
            m.width as i32 == live_cfg.stream.width && m.height as i32 == live_cfg.stream.height
        })
        .min_by_key(|m| (m.refresh_millihz as i64 - live_cfg.stream.fps as i64 * 1000).abs())
        .cloned();
    session_metrics.set_console_mode(
        console_current
            .as_ref()
            .map(|m| (m.width, m.height, m.refresh_millihz)),
    );

    let mut display_bus = local_display.pipeline.bus();
    // session-display-update state — see the matching declarations in run_blocking.
    let encode_size = (live_cfg.stream.width, live_cfg.stream.height);
    let mut current_render: Option<(i32, i32)> = None;
    let mut current_ui_scale: f64 = 1.0;
    // A local-only console session has no encode pipeline (the source feeds
    // kmssink/waylandsink), so there is no external resolution and no lever. The agent
    // loop rejects a `stream_*` update for such a session before it gets here.
    session_metrics.set_external_resize_supported(false);
    let mut cadence_at = Instant::now();
    // See the matching declaration in run_blocking.
    let mut renderer_degrade_tracker = RendererDegradeTracker::default();
    loop {
        if let Some(reason) = console_vt.as_mut().and_then(|vt| vt.lost()) {
            tracing::error!(
                token = "runner-console-vt-lost",
                reason = %reason,
                "console terminal lost mid-session"
            );
            emit(SessionEvent::Failed(format!(
                "console terminal lost: {reason}"
            )));
            let _ = local_display.pipeline.set_state(gst::State::Null);
            current_source.teardown();
            return;
        }
        if let Some(weston) = weston.as_mut() {
            match weston.try_exit() {
                Ok(Some(status)) => {
                    tracing::error!(
                        token = "runner-weston-exited-unexpectedly",
                        %status,
                        "weston console exited unexpectedly"
                    );
                    emit(SessionEvent::Failed(format!(
                        "weston console exited unexpectedly ({status})"
                    )));
                    let _ = local_display.pipeline.set_state(gst::State::Null);
                    current_source.teardown();
                    return;
                }
                Ok(None) => {}
                Err(e) => {
                    tracing::error!(
                        token = "runner-weston-liveness-failed",
                        error = %format_args!("{e:#}"),
                        "weston console liveness check failed"
                    );
                    emit(SessionEvent::Failed(format!(
                        "weston console liveness check failed: {e:#}"
                    )));
                    let _ = local_display.pipeline.set_state(gst::State::Null);
                    current_source.teardown();
                    return;
                }
            }
        }
        if stop.load(Ordering::Relaxed) {
            emit(SessionEvent::Stopping);
            // Stop interpipe consumers before the source producer: tearing the source down
            // while a local listener is PLAYING can block the state transition
            // indefinitely and leak Weston/DRM master.
            let _ = local_display.pipeline.set_state(gst::State::Null);
            if let Some(audio) = &local_audio {
                let _ = audio.pipeline.set_state(gst::State::Null);
            }
            current_source.teardown();
            emit(SessionEvent::Stopped {
                bytes_used: compute_bytes_used(&live_cfg),
                detail: None,
            });
            return;
        }

        // Apply-only; see `apply_display_update`.
        while let Ok(req) = display_rx.try_recv() {
            apply_display_update(
                current_source,
                &req,
                &mut current_render,
                &mut current_ui_scale,
                encode_size,
                &session_metrics,
                // No encode pipeline on this topology ⇒ no resolution lever.
                None,
            );
        }

        // #445: a mode the app picked (through the compositor's wlr-output-management).
        if let Some(request) = current_source.take_mode_request() {
            let current =
                console_current
                    .clone()
                    .unwrap_or(crate::messages::ConsoleModeSelection {
                        width: live_cfg.stream.width as u16,
                        height: live_cfg.stream.height as u16,
                        refresh_millihz: live_cfg.stream.fps as u32 * 1000,
                    });
            match console::plan_mode_switch(request, &current, &console_modes, cc.stream) {
                Ok(target) => {
                    let outputs = crate::capacity::detect_drm_outputs();
                    match switch_console_mode(
                        session_id,
                        &mut live_cfg,
                        &cc,
                        &target,
                        &outputs,
                        &mut weston,
                        local_backend,
                        &mut local_display,
                        current_source,
                        &ipsink_name(session_id, *gen),
                        shared_clock,
                        shared_base,
                        cuda_ctx,
                        va_ctx,
                    ) {
                        Ok(()) => {
                            console_current = Some(target.clone());
                            session_metrics.set_console_mode(Some((
                                target.width,
                                target.height,
                                target.refresh_millihz,
                            )));
                            diagnostic_tx.try_emit(
                                session_id.to_string(),
                                TraceEvent {
                                    ts_unix_ms: std::time::SystemTime::now()
                                        .duration_since(std::time::UNIX_EPOCH)
                                        .map(|d| d.as_millis() as i64)
                                        .unwrap_or(0),
                                    event: "console.mode_switched",
                                    payload: serde_json::json!({
                                        "width": target.width,
                                        "height": target.height,
                                        "refresh_millihz": target.refresh_millihz,
                                        "source": "app_request",
                                    }),
                                },
                            );
                        }
                        Err(failure) if failure.fatal => {
                            tracing::error!(
                                token = "runner-console-mode-switch-failed",
                                reason = %failure.reason,
                                "console mode switch failed; ending session"
                            );
                            emit(SessionEvent::Failed(failure.reason));
                            let _ = local_display.pipeline.set_state(gst::State::Null);
                            if let Some(audio) = &local_audio {
                                let _ = audio.pipeline.set_state(gst::State::Null);
                            }
                            current_source.teardown();
                            return;
                        }
                        Err(failure) => {
                            tracing::warn!(
                                token = "runner-console-mode-switch-rolled-back",
                                reason = %failure.reason,
                                "console mode switch rolled back; session stays at its mode"
                            );
                        }
                    }
                    // Either way the local-display pipeline is a new object now.
                    display_bus = local_display.pipeline.bus();
                }
                Err(refusal) => {
                    tracing::info!(
                        token = "console-mode-request-refused",
                        ?request,
                        ?refusal,
                        "console mode request not applied"
                    );
                }
            }
        }

        while let Ok(req) = swap_rx.try_recv() {
            *gen += 1;
            emit(SessionEvent::Swapping);
            match perform_swap(
                &live_cfg,
                session_id,
                *gen,
                res,
                &local_display.interpipesrc,
                current_source,
                req,
                shared_clock,
                shared_base,
                cuda_ctx,
                va_ctx,
                vulkan_contexts,
                session_metrics.clone(),
                (current_render, current_ui_scale),
                emit,
            ) {
                Ok(()) => {
                    emit(SessionEvent::SwapDone);
                    diagnostic_tx.try_emit(
                        session_id.to_string(),
                        TraceEvent {
                            ts_unix_ms: std::time::SystemTime::now()
                                .duration_since(std::time::UNIX_EPOCH)
                                .map(|d| d.as_millis() as i64)
                                .unwrap_or(0),
                            event: "pipeline.source_swapped",
                            payload: serde_json::json!({"video_topology": "local_only"}),
                        },
                    );
                }
                // As on the browser path: a fatal swap failure left a compositor with no
                // app in it, so end the session rather than claim a rollback.
                Err(failure) if failure.fatal => {
                    tracing::error!(
                        token = "runner-swap-fatal-failed-local",
                        reason = %failure.reason,
                        "swap failed fatally (local-only); ending session"
                    );
                    emit(SessionEvent::Failed(failure.reason));
                    let _ = local_display.pipeline.set_state(gst::State::Null);
                    if let Some(audio) = &local_audio {
                        let _ = audio.pipeline.set_state(gst::State::Null);
                    }
                    current_source.teardown();
                    return;
                }
                Err(failure) => emit(SessionEvent::SwapRolledBack(failure.reason)),
            }
        }

        if let Some(bus) = current_source.bus() {
            while let Some(msg) = bus.pop() {
                current_source.on_bus_message(&msg);
                match msg.view() {
                    gst::MessageView::Error(err) => {
                        // A local-only session can also be a Vulkan session; classify
                        // DEVICE_LOST here too.
                        let debug_text = format!("{:?}", err.debug());
                        let reason = if vulkan_fault::is_device_lost(&format!(
                            "{} {debug_text}",
                            err.error()
                        )) {
                            vulkan_fault::device_lost_reason(&err.error().to_string(), &debug_text)
                        } else {
                            format!("source pipeline error: {}", err.error())
                        };
                        tracing::error!(
                            token = "runner-source-bus-error-local",
                            reason = %reason,
                            "source pipeline bus error (local-only)"
                        );
                        emit(SessionEvent::Failed(reason));
                        let _ = local_display.pipeline.set_state(gst::State::Null);
                        current_source.teardown();
                        return;
                    }
                    gst::MessageView::Warning(warn) => {
                        if let Some(reason) =
                            renderer_degraded_failure(warn, &mut renderer_degrade_tracker)
                        {
                            tracing::error!(
                                token = "runner-renderer-degraded-failed-local",
                                reason = %reason,
                                "renderer degraded (local-only); failing session"
                            );
                            emit(SessionEvent::Failed(reason));
                            let _ = local_display.pipeline.set_state(gst::State::Null);
                            current_source.teardown();
                            return;
                        }
                    }
                    _ => {}
                }
            }
        }
        // Same fail-closed check as the encoded-session main loop: a container-launch
        // failure never posts a gst bus Error.
        if let Some(err) = current_source.take_launch_error() {
            tracing::error!(
                token = "runner-container-launch-failed-local",
                error = %err,
                "container launch failed (local-only)"
            );
            emit(SessionEvent::Failed(format!(
                "container launch failed: {err}"
            )));
            let _ = local_display.pipeline.set_state(gst::State::Null);
            current_source.teardown();
            return;
        }
        if let Some(msg) = poll_display_bus(display_bus.as_ref()) {
            if let gst::MessageView::Error(err) = msg.view() {
                // DEVICE_LOST on the local-display bus.
                let debug_text = format!("{:?}", err.debug());
                let reason =
                    if vulkan_fault::is_device_lost(&format!("{} {debug_text}", err.error())) {
                        vulkan_fault::device_lost_reason(&err.error().to_string(), &debug_text)
                    } else {
                        format!(
                            "local-display pipeline error: {} ({:?})",
                            err.error(),
                            err.debug()
                        )
                    };
                tracing::error!(
                    token = "runner-local-display-bus-error",
                    reason = %reason,
                    "local-display pipeline bus error"
                );
                emit(SessionEvent::Failed(reason));
                let _ = local_display.pipeline.set_state(gst::State::Null);
                current_source.teardown();
                return;
            }
        }
        let cadence_elapsed = cadence_at.elapsed();
        if cadence_elapsed >= Duration::from_secs(5) {
            let frames = local_display.drain_sink_frames();
            let seconds = cadence_elapsed.as_secs_f64();
            let delivered_fps = frames as f64 / seconds;
            let expected = (live_cfg.stream.fps as f64 * seconds).round() as u64;
            let missing = expected.saturating_sub(frames);
            tracing::info!(
                target_fps = live_cfg.stream.fps,
                frames,
                window_ms = cadence_elapsed.as_millis() as u64,
                delivered_fps = format_args!("{delivered_fps:.2}"),
                missing,
                "local-display cadence"
            );

            // App-liveness on the same 5 s cadence; local_only defaults to `fail` unless
            // the console catalog row opted into `keep`.
            if let Some(status) = current_source.take_container_exit() {
                let policy = current_source.exit_policy();
                // Read BOTH before any teardown: the app-surface counter lives on the
                // compositor element and the log ring is filled by threads whose streams
                // close with the container, so tearing down first erases the evidence.
                let presented = current_source.app_has_presented();
                let app_log_tail = current_source.app_log_tail();
                let ts = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .map(|d| d.as_millis() as i64)
                    .unwrap_or(0);
                diagnostic_tx.try_emit(
                    session_id.to_string(),
                    TraceEvent {
                        ts_unix_ms: ts,
                        event: "app.exited",
                        payload: serde_json::json!({
                            "status": format!("{status:?}"),
                            "policy": format!("{policy:?}"),
                            "video_topology": "local_only",
                        }),
                    },
                );
                match policy {
                    AppExitPolicy::Keep => {
                        tracing::info!(
                            "app container exited ({status:?}); on_app_exit=keep — session continues"
                        );
                    }
                    AppExitPolicy::Unknown => {
                        // Degrade fail-closed; see the Unknown arm in run_blocking.
                        tracing::warn!(
                            token = "app-exit-disposition-unrecognized",
                            "unrecognized on_app_exit value, treating as fail"
                        );
                        tracing::error!(
                            token = "runner-app-exit-unrecognized-failed-local",
                            status = ?status,
                            presented,
                            "app container exited (local-only); unrecognized disposition treated as fail"
                        );
                        emit(app_exit_event(status, presented, app_log_tail));
                        let _ = local_display.pipeline.set_state(gst::State::Null);
                        current_source.teardown();
                        return;
                    }
                    AppExitPolicy::Fail => {
                        tracing::error!(
                            token = "runner-app-exit-failed-local",
                            status = ?status,
                            presented,
                            "app container exited (local-only); on_app_exit=fail"
                        );
                        emit(app_exit_event(status, presented, app_log_tail));
                        let _ = local_display.pipeline.set_state(gst::State::Null);
                        current_source.teardown();
                        return;
                    }
                }
            }

            cadence_at = Instant::now();
        }
    }
}

/// Build the local-display pipeline (`interpipesrc(listen-to=sink) → … → waylandsink|kmssink`)
/// against the live config, share the session clock and the producer's Vulkan/VA/CUDA
/// contexts with it, and bring it to PLAYING. The one bring-up path for a console session's
/// local display: the launch uses it, and so does a mode switch (#445) after re-pinning
/// the source. Returns the `SessionEvent::Failed` reason on error.
#[allow(clippy::too_many_arguments)]
fn bring_up_local_display(
    cfg: &SessionConfig,
    sink_name: &str,
    wayland_socket: Option<&str>,
    current_source: &AppSource,
    shared_clock: &gst::Clock,
    shared_base: gst::ClockTime,
    cuda_ctx: Option<&gst::Context>,
    va_ctx: Option<&gst::Context>,
) -> Result<pipeline::LocalDisplay, String> {
    let local_display = pipeline::build_local_display_pipeline(cfg, sink_name, wayland_socket)
        .map_err(|e| format!("build local-display pipeline: {e:#}"))?;
    local_display
        .pipeline
        .set_start_time(None::<gst::ClockTime>);
    local_display.pipeline.use_clock(Some(shared_clock));
    local_display.pipeline.set_base_time(shared_base);
    if pipeline::vulkan_image_transport(cfg) {
        let mut shared = 0;
        for context_type in ["gst.vulkan.instance", "gst.vulkan.device"] {
            if let Some(context) = current_source.source_context(context_type) {
                local_display.pipeline.set_context(&context);
                shared += 1;
            } else {
                tracing::warn!(
                    token = "vulkan-context-query-unanswered",
                    "local-only Vulkan producer did not answer {context_type} context query"
                );
            }
        }
        tracing::info!("local-only Vulkan context bridge installed ({shared}/2 producer contexts)");
    }
    if let Some(ctx) = va_ctx {
        local_display.pipeline.set_context(ctx);
        crate::session::va_share::install_need_context_handler(&local_display.pipeline, ctx);
    }
    if let Some(ctx) = cuda_ctx {
        local_display.pipeline.set_context(ctx);
    }
    local_display
        .pipeline
        .set_state(gst::State::Playing)
        .map_err(|e| format!("local-display set PLAYING: {e:#}"))?;
    Ok(local_display)
}

/// Why a console mode switch did not complete. `fatal` means the session cannot go on
/// (the display could not be brought back at any mode); otherwise it was rolled back to
/// the mode it had.
struct ModeSwitchFailure {
    fatal: bool,
    reason: String,
}

/// #445: move a local console session to `target`, the mode the app picked, without
/// restarting the app: the local display pipeline is stopped, the source is HELD (PAUSED,
/// so the compositor keeps serving its clients and nothing renders on the GPU during the
/// modeset -- a modeset under a PLAYING Vulkan source has faulted NVIDIA, Xid 13/32),
/// weston is restarted at the target mode (the DirectKms path has no weston: kmssink
/// modesets from the new caps), the source tail is re-pinned to the new WxH@fps, the local
/// display is rebuilt at the new caps, and the source resumes. The compositor sees the new
/// caps, moves its `wl_output` to the target mode and configures every toplevel; a rootful
/// Xwayland follows the configure, so the desktop's screen resizes in place.
///
/// If weston cannot start at the target mode, the previous mode is restored the same way;
/// only a failure to come back at all is fatal.
#[allow(clippy::too_many_arguments)]
fn switch_console_mode(
    session_id: &str,
    live_cfg: &mut SessionConfig,
    console_config: &crate::messages::ConsoleConfig,
    target: &crate::messages::ConsoleModeSelection,
    outputs: &[crate::messages::DrmOutputCapability],
    weston: &mut Option<console::WestonConsole>,
    local_backend: console::LocalBackend,
    local_display: &mut pipeline::LocalDisplay,
    current_source: &mut AppSource,
    sink_name: &str,
    shared_clock: &gst::Clock,
    shared_base: gst::ClockTime,
    cuda_ctx: Option<&gst::Context>,
    va_ctx: Option<&gst::Context>,
) -> Result<(), ModeSwitchFailure> {
    let previous = (
        live_cfg.stream.width,
        live_cfg.stream.height,
        live_cfg.stream.fps,
    );
    let fps = console::mode_fps(target);
    tracing::info!(
        token = "console-mode-switch",
        from = %format_args!("{}x{}@{}", previous.0, previous.1, previous.2),
        to = %format_args!("{}x{}@{} ({} mHz)", target.width, target.height, fps, target.refresh_millihz),
        "console mode switch requested by the app"
    );

    // 1. Consumers before the producer (the same order as session stop).
    let _ = local_display.pipeline.set_state(gst::State::Null);
    current_source.pause().map_err(|e| ModeSwitchFailure {
        fatal: true,
        reason: format!("console mode switch: hold source: {e:#}"),
    })?;

    // 2. The physical display, at the target mode.
    let mut at_mode = |mode: &crate::messages::ConsoleModeSelection| -> anyhow::Result<()> {
        if local_backend == console::LocalBackend::Weston {
            // Drop first: only one console weston may exist, and Drop drains the old one.
            *weston = None;
            let cfg_at = console::console_config_at_mode(console_config, mode.clone(), outputs);
            *weston = Some(console::spawn_weston_console(session_id, Some(&cfg_at))?);
        }
        Ok(())
    };
    let mut restore = |live_cfg: &mut SessionConfig,
                       weston: &Option<console::WestonConsole>,
                       current_source: &mut AppSource,
                       reason: String|
     -> ModeSwitchFailure {
        live_cfg.stream.width = previous.0;
        live_cfg.stream.height = previous.1;
        live_cfg.stream.fps = previous.2;
        let caps = pipeline::raw_video_caps(live_cfg);
        current_source.set_stream_mode(previous.0, previous.1, previous.2, &caps);
        match bring_up_local_display(
            live_cfg,
            sink_name,
            weston.as_ref().map(|w| w.socket.as_str()),
            current_source,
            shared_clock,
            shared_base,
            cuda_ctx,
            va_ctx,
        ) {
            Ok(ld) => {
                *local_display = ld;
                match current_source.start() {
                    Ok(()) => ModeSwitchFailure {
                        fatal: false,
                        reason,
                    },
                    Err(e) => ModeSwitchFailure {
                        fatal: true,
                        reason: format!("{reason}; and the source did not resume: {e:#}"),
                    },
                }
            }
            Err(e) => ModeSwitchFailure {
                fatal: true,
                reason: format!("{reason}; and the previous mode did not come back: {e}"),
            },
        }
    };

    if let Err(e) = at_mode(target) {
        let reason = format!(
            "console mode switch: weston did not start at {}x{}@{}: {e:#}",
            target.width, target.height, fps
        );
        tracing::warn!(token = "console-mode-switch-weston-failed", "{reason}");
        let previous_mode = crate::messages::ConsoleModeSelection {
            width: previous.0 as u16,
            height: previous.1 as u16,
            refresh_millihz: previous.2 as u32 * 1000,
        };
        if let Err(e) = at_mode(&previous_mode) {
            return Err(ModeSwitchFailure {
                fatal: true,
                reason: format!("{reason}; and it did not come back at the previous mode: {e:#}"),
            });
        }
        return Err(restore(live_cfg, weston, current_source, reason));
    }

    // 3. The source, re-pinned to the new mode; the compositor applies it on the new caps.
    live_cfg.stream.width = target.width as i32;
    live_cfg.stream.height = target.height as i32;
    live_cfg.stream.fps = fps;
    let caps = pipeline::raw_video_caps(live_cfg);
    if !current_source.set_stream_mode(target.width as i32, target.height as i32, fps, &caps) {
        return Err(restore(
            live_cfg,
            weston,
            current_source,
            "console mode switch: the source has no re-pinnable caps".to_string(),
        ));
    }

    // 4. The local display at the new caps, then the source resumes.
    match bring_up_local_display(
        live_cfg,
        sink_name,
        weston.as_ref().map(|w| w.socket.as_str()),
        current_source,
        shared_clock,
        shared_base,
        cuda_ctx,
        va_ctx,
    ) {
        Ok(ld) => *local_display = ld,
        Err(e) => {
            return Err(ModeSwitchFailure {
                fatal: true,
                reason: format!("console mode switch: {e}"),
            })
        }
    }
    current_source.start().map_err(|e| ModeSwitchFailure {
        fatal: true,
        reason: format!("console mode switch: resume source: {e:#}"),
    })?;

    tracing::info!(
        "app display mode: {}x{}@{} (source=app_request, refresh_millihz={})",
        target.width,
        target.height,
        fps,
        target.refresh_millihz
    );
    Ok(())
}

/// #413: exactly one `POLL`-bounded wait per `run_local_only` iteration.
/// `Bus::timed_pop(POLL)` already blocks up to `POLL`, so adding a sleep on the
/// no-message branch made stop latency ~200 ms instead of 100 ms. The sleep is gated on
/// `display_bus.is_none()` (nothing to block on would spin hot), never on the pop result.
fn poll_display_bus(display_bus: Option<&gst::Bus>) -> Option<gst::Message> {
    match display_bus {
        Some(bus) => bus.timed_pop(gst::ClockTime::from_mseconds(POLL)),
        None => {
            std::thread::sleep(Duration::from_millis(POLL));
            None
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// An idle iteration must cost ONE `POLL` wait, not two; the old
    /// `timed_pop(POLL)` + `else { sleep(POLL) }` shape took ~2x `iterations * POLL`.
    #[test]
    fn idle_local_display_loop_waits_one_poll_per_iteration() {
        gstreamer::init().unwrap();
        let pipe = gstreamer::Pipeline::new();
        let bus = pipe.bus().expect("pipeline bus");
        let iterations = 10u32;
        let started = std::time::Instant::now();
        for _ in 0..iterations {
            assert!(poll_display_bus(Some(&bus)).is_none());
        }
        let elapsed = started.elapsed();
        let budget = Duration::from_millis(POLL * u64::from(iterations) * 3 / 2);
        assert!(
            elapsed < budget,
            "idle loop took {elapsed:?}, over the {budget:?} one-wait-per-iteration budget \
             (a second sleep per iteration would land near {:?})",
            Duration::from_millis(POLL * u64::from(iterations) * 2)
        );
    }

    /// The sleep is still required with no local-display pipeline, or the loop spins hot.
    #[test]
    fn absent_display_bus_still_paces_the_loop() {
        let started = std::time::Instant::now();
        assert!(poll_display_bus(None).is_none());
        assert!(started.elapsed() >= Duration::from_millis(POLL));
    }
}
