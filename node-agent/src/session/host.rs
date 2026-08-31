//! Session host orchestration: the virtual input devices, the PulseAudio
//! sidecar, and the app container that sit *around* the media pipeline.
//!
//! Shared by both pipeline drivers ([`super::runner`], [`super::server`]) so the
//! container/input lifecycle lives in one place:
//!   1. [`SessionHost::prepare`] creates the uinput devices and starts the
//!      PulseAudio sidecar before the pipeline is built. Device node paths feed
//!      `waylanddisplaysrc` and the DataChannel input sink; the sidecar socket
//!      feeds `pulsesrc`.
//!   2. Once PLAYING, the compositor announces its Wayland socket on the bus
//!      (an `Application` message named `wayland.src`). [`SessionHost::on_bus_message`]
//!      launches the app container into it exactly once, injecting `PULSE_SERVER`.
//!   3. [`SessionHost::teardown`] (and `Drop`) removes both the container and the
//!      sidecar, so no terminal transition can leak a container.

use std::sync::Arc;

use gstreamer as gst;

use super::audio::PulseSidecar;
use super::container::{
    AppDisplayMode, ContainerRuntime, ContainerSpec, LaunchParams, RunningContainer,
};
use super::virtual_input::VirtualDevices;
use super::SessionConfig;

pub struct SessionHost {
    session_id: String,
    container_spec: Option<ContainerSpec>,
    runtime_dir: String,
    /// #375: resolved 32-bit NVIDIA driver-lib host dir; the launch's
    /// `/opt/quasar/nvidia-lib32` mount source.
    nvidia_lib32_path: String,
    /// #384: the session's streamed display mode, injected as env so a nested
    /// gamescope runs at the session profile, not its own default.
    display: AppDisplayMode,
    runtime: ContainerRuntime,
    /// `None` under `use_test_src` (videotestsrc takes no input). Shared (`Arc`)
    /// with the DataChannel input sink.
    pub devices: Option<Arc<VirtualDevices>>,
    container: Option<RunningContainer>,
    launched: bool,
    /// `None` under `use_test_audio` or when start failed (socket timeout). The
    /// sidecar's `Drop` removes its container.
    pulse: Option<PulseSidecar>,
    /// Why there is no sidecar despite wanting one (session streams silence), or
    /// `None`. Mirrors `SessionResources::audio_degraded` (see source.rs).
    audio_degraded: Option<String>,
}

impl SessionHost {
    /// Create the virtual input devices and start the PulseAudio sidecar. Call
    /// before building the pipeline. Returns `(host, pulse_server_uri)`;
    /// `pulse_server_uri` is `None` when falling back to silent audio.
    pub fn prepare(
        session_id: &str,
        cfg: &SessionConfig,
    ) -> anyhow::Result<(Self, Option<String>)> {
        let devices = if cfg.use_test_src {
            None
        } else {
            let d = Arc::new(VirtualDevices::create(session_id)?);
            // Publish fake-udev records where the app container's /run/udev/data
            // bind-mount can see them (SDL/Steam discover via libudev). Best-effort:
            // a failure only degrades gamepad discovery, not the session.
            let udev_dir = super::virtual_input::udev_export_dir(&cfg.runtime_dir, session_id);
            if let Err(e) = d.export_udev_data(&udev_dir) {
                tracing::warn!(
                    token = "udev-export-failed",
                    "udev export to {} failed: {e:#} — in-container gamepad discovery degraded",
                    udev_dir.display()
                );
            }
            Some(d)
        };

        let runtime = ContainerRuntime::from_env();

        // Both no-sidecar outcomes are recorded with a reason (rationale:
        // `SessionResources::prepare` in source.rs): `Err` means it could not
        // start at all, `Ok(None)` means its socket never appeared. Either way
        // the session streams silence, and that must not be invisible.
        let mut audio_degraded: Option<String> = None;
        let pulse = if cfg.use_test_audio {
            None
        } else {
            match PulseSidecar::start(session_id, &runtime, &cfg.runtime_dir) {
                Ok(Some(s)) => Some(s),
                Ok(None) => {
                    audio_degraded = Some(
                        "PulseAudio sidecar started but its socket never appeared".to_string(),
                    );
                    None
                }
                Err(e) => {
                    // Audio failure must not kill video.
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

        Ok((
            SessionHost {
                session_id: session_id.to_string(),
                container_spec: cfg.container.clone(),
                runtime_dir: cfg.runtime_dir.clone(),
                nvidia_lib32_path: cfg.nvidia_lib32_path.clone(),
                display: AppDisplayMode {
                    width: cfg.stream.width,
                    height: cfg.stream.height,
                    fps: cfg.stream.fps,
                },
                runtime,
                devices,
                container: None,
                launched: false,
                pulse,
                audio_degraded,
            },
            pulse_server,
        ))
    }

    /// Why this session has no PulseAudio sidecar despite wanting one, or `None`
    /// if it has one (or deliberately asked for test audio).
    pub fn audio_degraded_reason(&self) -> Option<&str> {
        self.audio_degraded.as_deref()
    }

    /// Feed every pipeline bus message here; the first `wayland.src` announcement
    /// triggers the (single) container launch.
    pub fn on_bus_message(&mut self, msg: &gst::Message) {
        if self.launched {
            return;
        }
        if let Some(display) = wayland_display_from_message(msg) {
            self.launched = true; // one shot even on launch failure, no spin
            self.launch(&display);
        }
    }

    fn launch(&mut self, wl_display: &str) {
        let Some(spec) = self.container_spec.clone() else {
            tracing::info!(
                "compositor Wayland socket '{}' ready; no app image configured \
                 (bare compositor)",
                wl_display
            );
            return;
        };

        // Inject PULSE_SERVER and mount the sidecar's socket dir at the same path
        // inside the container (docker-out-of-docker safe, like the Wayland dir).
        let mut effective_spec = spec;
        let mut extra_mounts: Vec<String> = Vec::new();

        if let Some(p) = &self.pulse {
            let server_uri = p.server_uri();
            tracing::info!("injecting PULSE_SERVER={server_uri} into app container");
            effective_spec
                .env
                .insert("PULSE_SERVER".to_string(), server_uri);

            let dir = p.socket_dir().to_string_lossy().into_owned();
            // No PULSE_COOKIE: the sidecar grants anonymous auth
            // (audio::pulse_run_args) — a shared cookie is unreliable, since
            // pressure-vessel remaps it and silently denies Proton clients.
            // Catalog-supplied PULSE_SINK/PULSE_SOURCE win; only default here.
            effective_spec
                .env
                .entry("PULSE_SINK".to_string())
                .or_insert_with(|| super::audio::QUASAR_SINK_NAME.to_string());
            // Mic source always exists (baked into the sidecar argv), silent
            // unless this session negotiated a mic m-line.
            effective_spec
                .env
                .entry("PULSE_SOURCE".to_string())
                .or_insert_with(|| super::audio::QUASAR_MIC_SOURCE_NAME.to_string());
            extra_mounts.push(format!("{dir}:{dir}"));
        }
        effective_spec.mounts.extend(extra_mounts);

        let device_nodes = self
            .devices
            .as_ref()
            .map(|d| {
                vec![
                    d.keyboard_path.clone(),
                    d.mouse_path.clone(),
                    d.gamepad_path.clone(),
                ]
            })
            .unwrap_or_default();
        let params = LaunchParams {
            session_id: &self.session_id,
            wayland_display: wl_display,
            runtime_dir: &self.runtime_dir,
            device_nodes,
            container_name: None,
            nvidia_lib32_path: &self.nvidia_lib32_path,
            display: self.display,
        };
        match self.runtime.run(&effective_spec, &params) {
            Ok(c) => {
                tracing::info!(
                    "app container '{}' launched into compositor socket '{}'",
                    c.name(),
                    wl_display
                );
                self.container = Some(c);
            }
            Err(e) => tracing::error!(
                token = "app-container-launch-failed",
                "failed to launch app container: {e:#}"
            ),
        }
    }

    /// Tear everything down (idempotent). `Drop` is the backstop.
    /// Order: app container first (stops producing audio), then PulseAudio sidecar.
    pub fn teardown(&mut self) {
        if let Some(mut c) = self.container.take() {
            c.stop();
        }
        if let Some(mut p) = self.pulse.take() {
            p.stop();
        }
    }
}

impl Drop for SessionHost {
    fn drop(&mut self) {
        self.teardown();
    }
}

/// Extract `WAYLAND_DISPLAY` from a `waylanddisplaysrc` environment announcement:
/// an `Application` message named `wayland.src` carrying the compositor's env
/// vars, since the element has no property/signal for the socket name.
pub fn wayland_display_from_message(msg: &gst::Message) -> Option<String> {
    if let gst::MessageView::Application(app) = msg.view() {
        let s = app.structure()?;
        if s.name() == "wayland.src" {
            return s.get::<String>("WAYLAND_DISPLAY").ok();
        }
    }
    None
}
