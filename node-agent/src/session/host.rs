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
    /// The sticky "a previous stop was unconfirmed" / "a mount may still be
    /// live" pair, carried between this host's release calls.
    release_state: super::teardown::ReleaseState,
    /// Last container-stop outcome. See `SessionHost::finish_container_stop`.
    ended: Option<super::teardown::StopAttempt>,
    /// This session end's whole retry allowance. Deliberately much smaller than
    /// the production session's: `Drop` runs on a Tokio worker (see
    /// [`super::teardown::DEMO_STOP_RETRY_BUDGET`]).
    budget: super::teardown::RetryBudget,
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

        // Both no-sidecar outcomes are recorded with a reason (rationale:
        // `SessionResources::prepare` in source.rs): `Err` means it could not
        // start at all, `Ok(None)` means its socket never became ready. Either way
        // the session streams silence, and that must not be invisible.
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
        // One allowance for this host's whole session end, shared with the sidecar.
        let budget = super::teardown::RetryBudget::new(super::teardown::DEMO_STOP_RETRY_BUDGET);
        let mut pulse = pulse;
        if let Some(sidecar) = pulse.as_mut() {
            sidecar.adopt_budget(budget.clone());
        }

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
                release_state: super::teardown::ReleaseState::IDLE,
                ended: None,
                budget,
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
                self.release_state.mount_live = true;
            }
            Err(e) => tracing::error!(
                token = "app-container-launch-failed",
                "failed to launch app container: {e:#}"
            ),
        }
    }

    /// Tear everything down (idempotent). `Drop` is the final chance.
    /// Order: app container, then udev export, then PulseAudio sidecar — and the
    /// sidecar is stopped even when the app stop is unconfirmed. A busy client
    /// is retried; it is not treated as a failed stop.
    pub fn teardown(&mut self) {
        self.release(false);
    }

    fn release(&mut self, final_chance: bool) {
        let report = self.finish_container_stop(final_chance);
        if final_chance {
            // The observed attempt above replaces the container's blind `Drop`
            // stop; see `RunningContainer::disarm_drop`. It also keeps this
            // release's blocking on the Tokio worker no worse than the single
            // stop+cleanup `SessionHost::teardown` already cost before #314.
            if let Some(container) = self.container.as_mut() {
                container.disarm_drop();
            }
        }
        self.release_state = super::teardown::settle_udev(
            &self.session_id,
            report,
            self.release_state,
            final_chance,
            self.devices
                .as_ref()
                .map(|d| &**d as &dyn super::teardown::UdevExport),
        );
        if let Some(pulse) = self.pulse.as_mut() {
            pulse.stop();
        }
    }

    /// Same rule as `AppSource::finish_app_stop`: every engine attempt is
    /// observed, a refused one may be asked again, and an unconfirmed one is
    /// asked exactly once more — on the last chance, in place of the container's
    /// blind `Drop`.
    fn finish_container_stop(&mut self, last_chance: bool) -> super::teardown::StopAttempt {
        use super::teardown::StopAttempt;
        if let Some(report) = self.ended {
            let ask_again = match report {
                StopAttempt::Retryable => true,
                StopAttempt::Unconfirmed => last_chance,
                StopAttempt::Confirmed | StopAttempt::Absent => false,
            };
            if !ask_again {
                return report;
            }
        }
        let budget = self.budget.clone();
        let report = super::teardown::retry_with_budget(|| self.stop_container_once(), &budget);
        self.ended = Some(report);
        report
    }

    fn stop_container_once(&mut self) -> super::teardown::StopAttempt {
        if self.container.is_none() {
            return super::teardown::StopAttempt::Absent;
        }
        let stopped = self.container.as_mut().unwrap().stop();
        match stopped {
            Ok(()) => {
                self.container.take();
                self.release_state.mount_live = false;
                super::teardown::StopAttempt::Confirmed
            }
            Err(error) => {
                let report = super::teardown::classify_stop(super::teardown::error_kind(&error));
                if !matches!(report, super::teardown::StopAttempt::Retryable) {
                    tracing::warn!(
                        token = "application-host-teardown-pending",
                        "application teardown remains durable: {error}"
                    );
                }
                report
            }
        }
    }
}

impl Drop for SessionHost {
    fn drop(&mut self) {
        self.release(true);
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
