//! Session runner: drives the pipeline and relays signaling.
//!
//! Runs on a dedicated OS thread with a blocking GStreamer bus loop; progress goes
//! back over a Send tokio channel. Outbound offer/ICE become
//! `SessionEvent::Signaling`; inbound answer/ICE arrive on `sig_in_rx` and are
//! applied to `webrtcbin` on this thread.
//!
//! The first offer produced is the "running" signal (schema.md).

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use gstreamer as gst;
use gstreamer::prelude::*;
use tokio::sync::mpsc;

use super::capture::{Capture, CaptureCtx, CaptureRequest};
use super::container::{app_display_env, AppDisplayMode, AppExitStatus};
use super::encoder_stall;
use super::metrics::SessionMetrics;
use super::pipeline::{RemoteDescriptionApplied, RemoteDescriptionFailure};
use super::server;
use super::signaling::SignalMsg;
use super::source::{AppSource, SessionResources};
use super::vulkan_fault;
use super::{console, pipeline, SessionConfig};
use crate::messages::{AppExitPolicy, VideoTopology};

const SCTP_STOP_RACE_GRACE: Duration = Duration::from_secs(1);

const SCTP_STOP_RACE_POLL: Duration = Duration::from_millis(20);

/// Case-insensitive substring signatures of a WebRTC DataChannel / SCTP association
/// failure on the encode bus. gst reports it as a generic `RESOURCE/WRITE` GError
/// whose message names nothing ("Could not write to resource."); the signature lives
/// only in the debug half, so callers must match `err.error()` and `err.debug()`
/// CONCATENATED — matching the message alone silently never fires.
/// See `.claude/rules/webrtc-testing.md`.
const SCTP_ASSOCIATION_SIGNATURES: &[&str] = &["sctp association", "sctpenc", "sctpdec"];

/// True iff `text` (`error` + `debug` concatenated) carries an SCTP association
/// failure. These elements exist only in `webrtcbin`'s data-channel path, so a match
/// means the browser's peer connection is gone, not that the encoder or GPU broke.
fn is_sctp_association_error(message: &str) -> bool {
    let haystack = message.to_ascii_lowercase();
    SCTP_ASSOCIATION_SIGNATURES
        .iter()
        .any(|sig| haystack.contains(sig))
}

/// `state_detail` for a session ended by [`is_sctp_association_error`]: terminal
/// `stopped` with this detail and NO `error_message` (a peer disconnect, not an
/// agent fault).
const PEER_DISCONNECT_DETAIL: &str = "peer disconnected: WebRTC data channel closed";

/// The browser can close its peer connection before the Stop command finishes
/// traversing control-plane -> agent WS, so the SCTP error can arrive before the stop
/// flag. Wait out the grace period only for this signature; every other pipeline error
/// fails immediately, and an SCTP error with no stop still fails after the deadline.
fn stop_requested_for_transport_error(stop: &AtomicBool, message: &str) -> bool {
    if stop.load(Ordering::Relaxed) {
        return true;
    }
    if !is_sctp_association_error(message) {
        return false;
    }

    let deadline = Instant::now() + SCTP_STOP_RACE_GRACE;
    while Instant::now() < deadline {
        std::thread::sleep(SCTP_STOP_RACE_POLL);
        if stop.load(Ordering::Relaxed) {
            return true;
        }
    }
    false
}

/// #378: fail closed on a detected renderer degradation rather than stream a silently
/// software-rendered (black, for GL clients) picture. Defaults to require, globally —
/// safe because the only producer of the marker is the vendored gst-wayland-display
/// dmabuf-import failure path, which a `render-node=software` compositor never takes.
/// Knob: `QUASAR_REQUIRE_HW_RENDER`.
fn require_hw_render() -> bool {
    !matches!(
        std::env::var("QUASAR_REQUIRE_HW_RENDER").ok().as_deref(),
        Some("0") | Some("false") | Some("FALSE")
    )
}

/// Log every compositor bus Warning at `warn`; return the error text iff it carries
/// the gst-wayland-display renderer-degradation marker. Kept narrow: a generic
/// "software"+"fallback" match is pure false-positive surface. Matches the unprefixed
/// `renderer-degraded` stem so both the `quasar-` and `wolf-` marker spellings work,
/// making a `GST_WAYLAND_DISPLAY_REF` bump not a flag day.
fn renderer_degraded_marker(warn: &gst::message::Warning) -> Option<String> {
    let element = warn
        .src()
        .map(|s| s.name().to_string())
        .unwrap_or_else(|| "<unknown>".to_string());
    let error_text = warn.error().to_string();
    let debug_text = format!("{:?}", warn.debug());
    tracing::warn!(
        token = "compositor-bus-warning",
        element = %element,
        error = %error_text,
        debug = %debug_text,
        "compositor bus warning"
    );
    let haystack = format!("{error_text} {debug_text}").to_lowercase();
    if haystack.contains("renderer-degraded") {
        Some(error_text)
    } else {
        None
    }
}

/// Debounce window. The vendored patch rate-limits the marker to 1/5s, so a sustained
/// degradation produces a second marker inside 30s; a one-off transient does not.
const RENDERER_DEGRADE_WINDOW: Duration = Duration::from_secs(30);

/// One tracker per bus-pump loop (`run_blocking`, `run_local_only`): the first marker
/// warns and arms the window; only a SECOND marker inside
/// [`RENDERER_DEGRADE_WINDOW`] fails the session closed.
#[derive(Default)]
struct RendererDegradeTracker {
    first_seen: Option<Instant>,
}

impl RendererDegradeTracker {
    /// Returns `true` iff this occurrence is the fail-closed trigger (an earlier one
    /// is still inside the window). Otherwise arms/re-arms the window and returns
    /// `false` (first marker, or window already expired).
    fn record(&mut self) -> bool {
        let now = Instant::now();
        if self
            .first_seen
            .is_some_and(|first| now.duration_since(first) <= RENDERER_DEGRADE_WINDOW)
        {
            self.first_seen = None;
            true
        } else {
            self.first_seen = Some(now);
            false
        }
    }
}

/// Marker detection + the fail-closed knob + the debounce window. Returns a reason
/// only on the debounced trigger; a non-triggering marker logs and returns `None`, so
/// the caller keeps the session running.
fn renderer_degraded_failure(
    warn: &gst::message::Warning,
    tracker: &mut RendererDegradeTracker,
) -> Option<String> {
    let error_text = renderer_degraded_marker(warn)?;
    if !require_hw_render() {
        return None;
    }
    if tracker.record() {
        Some(format!("compositor renderer degraded: {error_text}"))
    } else {
        tracing::warn!(
            token = "renderer-degradation",
            "renderer degradation reported; failing closed if it repeats within 30s"
        );
        None
    }
}

const VULKAN_INSTANCE_CONTEXT: &str = "gst.vulkan.instance";
const VULKAN_DEVICE_CONTEXT: &str = "gst.vulkan.device";

fn context_object_identity(context: &gst::Context, field: &str) -> Option<usize> {
    context
        .structure()
        .get::<gst::glib::Object>(field)
        .ok()
        .map(|object| object.as_ptr() as usize)
}

/// Interpipe forwards context queries only after its src/sink are connected, so the
/// VulkanImage contract must not depend on that timing: take the producer-owned
/// instance/device, inject both before READY, and keep the expected device identity.
#[derive(Clone)]
struct VulkanContextBridge {
    instance: gst::Context,
    device: gst::Context,
    device_identity: usize,
}

impl VulkanContextBridge {
    fn capture(source: &AppSource) -> anyhow::Result<Self> {
        let instance = source
            .source_context(VULKAN_INSTANCE_CONTEXT)
            .ok_or_else(|| {
                anyhow::anyhow!("Vulkan producer did not provide {VULKAN_INSTANCE_CONTEXT}")
            })?;
        let device = source
            .source_context(VULKAN_DEVICE_CONTEXT)
            .ok_or_else(|| {
                anyhow::anyhow!("Vulkan producer did not provide {VULKAN_DEVICE_CONTEXT}")
            })?;
        let device_identity = context_object_identity(&device, VULKAN_DEVICE_CONTEXT)
            .ok_or_else(|| anyhow::anyhow!("producer Vulkan context has no GstVulkanDevice"))?;
        Ok(Self {
            instance,
            device,
            device_identity,
        })
    }

    fn install_on_source(&self, source: &AppSource) {
        source.apply_context(&self.instance);
        source.apply_context(&self.device);
    }

    fn install_on_pipeline(&self, pipeline: &gst::Pipeline) {
        pipeline.set_context(&self.instance);
        pipeline.set_context(&self.device);
    }

    fn assert_source(&self, source: &AppSource) -> anyhow::Result<()> {
        let context = source
            .source_context(VULKAN_DEVICE_CONTEXT)
            .ok_or_else(|| {
                anyhow::anyhow!(
                    "replacement Vulkan producer did not provide {VULKAN_DEVICE_CONTEXT}"
                )
            })?;
        self.assert_device_context(&context)
    }

    fn assert_device_context(&self, context: &gst::Context) -> anyhow::Result<()> {
        let actual = context_object_identity(context, VULKAN_DEVICE_CONTEXT)
            .ok_or_else(|| anyhow::anyhow!("replacement Vulkan context has no GstVulkanDevice"))?;
        if actual != self.device_identity {
            anyhow::bail!(
                "Vulkan device mismatch before source swap: persistent encoder GstVulkanDevice={:#x}, replacement producer={actual:#x}",
                self.device_identity
            );
        }
        Ok(())
    }
}

fn install_vulkan_context_bridge(
    bridge: &VulkanContextBridge,
    encode_pipe: &gst::Pipeline,
) -> usize {
    bridge.install_on_pipeline(encode_pipe);
    let expected = bridge.device_identity;
    tracing::info!(
        gst_vulkan_device = format_args!("{expected:#x}"),
        "installed producer-owned Vulkan instance/device on encode pipeline before READY"
    );
    expected
}

/// After READY the Vulkan encoder answers with the device it resolved. Pointer equality
/// beats matching GPU names: one GstVulkanDevice owns one VkDevice, so equality proves
/// producer images and encoder queues share a device.
fn assert_vulkan_encoder_device(
    encode_pipe: &gst::Pipeline,
    expected: usize,
) -> anyhow::Result<()> {
    let encoder = encode_pipe
        .by_name(pipeline::VULKAN_ENCODER_NAME)
        .ok_or_else(|| anyhow::anyhow!("Vulkan encoder element is missing"))?;
    let mut query = gst::query::Context::new(VULKAN_DEVICE_CONTEXT);
    if !encoder.query(&mut query) {
        anyhow::bail!("Vulkan encoder did not answer its resolved device context query");
    }
    let context = query
        .context_owned()
        .ok_or_else(|| anyhow::anyhow!("Vulkan encoder returned an empty device context"))?;
    let actual = context_object_identity(&context, VULKAN_DEVICE_CONTEXT)
        .ok_or_else(|| anyhow::anyhow!("Vulkan encoder context has no GstVulkanDevice"))?;
    if actual != expected {
        anyhow::bail!(
            "Vulkan device mismatch before first frame: producer GstVulkanDevice={expected:#x}, encoder={actual:#x}"
        );
    }
    tracing::info!(
        gst_vulkan_device = format_args!("{actual:#x}"),
        "verified producer and encoder share one GstVulkanDevice/VkDevice"
    );
    Ok(())
}

/// Fail the whole DualOutput session when a required console local-display leg cannot
/// be built or played: for a console session the local monitor IS the purpose, so emit
/// `Failed` and tear down source + encode + audio rather than stream to the browser
/// with a black monitor. Mirrors `run_local_only`'s hard-fail discipline.
fn fail_dualoutput_console<F: Fn(SessionEvent)>(
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
    super::nvenc_defer::finish_encode(encode_pipe, defer_encode_teardown);
    if let Some(ap) = audio_pipeline {
        let _ = ap.set_state(gst::State::Null);
    }
}

/// Lifecycle + signaling events emitted by a runner.
#[derive(Debug, Clone)]
pub enum SessionEvent {
    Starting,
    /// Fine-grained launch progress while the top-level state remains starting.
    Progress(&'static str),
    Running,
    Stopping,
    /// Session stopped cleanly. `bytes_used` is the post-session du measurement when
    /// `QUASAR_HOME_ROOT` is set, else `None`. `detail` is the terminal `stopped` row's
    /// `state_detail`: `None` for an ordinary stop, `Some(PEER_DISCONNECT_DETAIL)` when
    /// the peer's transport died (still a clean stop, never an `error_message`).
    Stopped {
        bytes_used: Option<u64>,
        detail: Option<&'static str>,
    },
    Failed(String),
    /// Terminal failure carrying a machine-readable classification plus the app
    /// container's log tail. Same `session_state{failed}` callback as
    /// [`SessionEvent::Failed`] (`reason` lands in `error_message`), with
    /// `reason_code`/`app_log_tail` additive.
    ///
    /// A separate variant, not extra fields on `Failed`: `Failed(String)` has 40+
    /// construction sites and the Vulkan GPU-global fault detector pattern-matches its
    /// reason text. An app container exiting is never a device-lost.
    AppFailed {
        reason: String,
        /// [`REASON_APP_EXITED_EARLY`] or [`REASON_APP_NEVER_PRESENTED`] (#484).
        reason_code: &'static str,
        /// Oldest first; may be empty (a genuinely silent app).
        app_log_tail: Vec<String>,
    },
    /// Snapshot read from the live pipeline's elements/caps. Reliable lifecycle lane,
    /// not the droppable diagnostics lane.
    EffectiveMedia(serde_json::Value),
    /// `session_capture` result ([`crate::session::capture`]). Reliable lane: an admin
    /// asked for it by `capture_id` and is polling, so a drop presents as a capture
    /// that never completes.
    Capture {
        /// `diag.pipeline_dot` | `diag.encoder_props` | `diag.burst_stats`.
        event: &'static str,
        payload: serde_json::Value,
    },
    /// A trace event that must NOT be dropped. [`DiagnosticEventTx`] stays the default for
    /// trace events; three are exceptions. `caps.negotiated` is the only statement of what
    /// the encode branch agreed; `sdp.answer_applied` records a rejected m-line, which
    /// explains a live-looking session with no media; `encoder.stall` is one open stall
    /// plus its recovery, and a dropped half is worse than neither.
    Trace {
        event: &'static str,
        payload: serde_json::Value,
    },
    /// A signaling message (offer/ICE) to relay to the control plane.
    Signaling(SignalMsg),
    /// Swap in progress; `session_state{running, detail:"swapping"}`.
    Swapping,
    /// Swap succeeded: the new app is the source AND has presented at least one frame
    /// through the live encoder, never the compositor's own empty frame.
    /// `session_state{running, detail:"swap complete"}`; the control plane commits
    /// `sessions.app_id` on it, and it is the client's quick-switch reveal signal.
    SwapDone,
    /// Swap failed, previous app restored and still running.
    /// `session_state{running, detail:"swap failed; rolled back: <reason>"}`; `app_id`
    /// unchanged.
    SwapRolledBack(String),
    /// #484: app container up but not drawn yet.
    /// `session_state{running, detail:"app booting"}` — top-level state stays `running`
    /// because the transport is live. Once per generation, always after `Running` (the
    /// control plane rejects a detail for a not-yet-running session), and only for a
    /// generation that launches an app container.
    AppBooting,
    /// The app committed its first mapped top-level surface AND a compositor frame
    /// followed it (`swap_source_ready`, the #487 mitigation).
    /// `session_state{running, detail:"app presented"}`; the launch-loading-screen reveal
    /// signal. Latched: at most once per generation, after `AppBooting`.
    AppPresented,
}

fn serialized_property(element: &gst::Element, name: &str) -> Option<String> {
    element.find_property(name)?;
    element
        .property_value(name)
        .serialize()
        .ok()
        .map(|value| value.to_string())
}

fn current_caps(element: &gst::Element, pad_name: &str) -> Option<String> {
    element
        .static_pad(pad_name)?
        .current_caps()
        .map(|caps| caps.to_string())
}

/// Map an encoder src-caps string (`video/x-h264, …`) to the session codec name.
/// Ordered so `x-h264` doesn't shadow `x-h265`.
fn codec_from_caps_str(caps: &str) -> Option<&'static str> {
    if caps.contains("x-h265") {
        Some("h265")
    } else if caps.contains("x-av1") {
        Some("av1")
    } else if caps.contains("x-h264") {
        Some("h264")
    } else {
        None
    }
}

fn effective_encoder_device_path(
    encoder_factory: Option<&str>,
    reported_device_path: Option<String>,
    resolved_render_node: &str,
) -> Option<String> {
    reported_device_path.or_else(|| {
        // vulkanh264enc has no device-path property, but its device is known:
        // build_video_source picks the GstVulkanDevice by this DRM render node and
        // interpipe forwards that context. Report it so the admin readback is unambiguous.
        (encoder_factory == Some("vulkanh264enc")).then(|| resolved_render_node.to_string())
    })
}

/// Wall-clock milliseconds for a trace event's `ts_unix_ms`.
fn now_unix_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// The `effective_media.audio.path` label. Pure so the precedence is unit tested.
///
/// The silent fallback also sets `use_test_audio` (that is how it swaps `pulsesrc` for
/// `audiotestsrc`), so `degraded` must be tested BEFORE `test_audio` or a muted session
/// reports itself as a deliberate test tone.
fn audio_path_label(disabled: bool, degraded: bool, test_audio: bool) -> &'static str {
    if disabled {
        "disabled"
    } else if degraded {
        "silent-fallback"
    } else if test_audio {
        "test-tone"
    } else {
        "sidecar"
    }
}

/// The `effective_media.mic` label (`"off" | "negotiated" | "active"`,
/// `protocol/agent-api.md` microphone amendment §3.1). Pure, so the precedence is unit
/// tested.
///
/// `"negotiated"` requires the control-plane grant (`stream.mic`), the host kill switch
/// `QUASAR_MIC_DISABLED` off, and an audio PeerConnection (the mic m-line rides it).
/// `"active"` is never produced here: the snapshot is emitted once at the first offer,
/// before the inbound track can exist.
fn mic_state_label(granted: bool, host_disabled: bool, audio_pc: bool) -> &'static str {
    if granted && !host_disabled && audio_pc {
        "negotiated"
    } else {
        "off"
    }
}

fn effective_media_snapshot(
    cfg: &SessionConfig,
    encode_pipe: &gst::Pipeline,
    interpipesrc: &gst::Element,
    local_backend: Option<&str>,
) -> serde_json::Value {
    let encoder = encode_pipe
        .by_name(pipeline::VIDEO_ENCODER_NAME)
        .or_else(|| encode_pipe.by_name(pipeline::VULKAN_ENCODER_NAME));
    let encoder_factory = encoder
        .as_ref()
        .and_then(|e| e.factory())
        .map(|f| f.name().to_string());
    let encoder_device_path = effective_encoder_device_path(
        encoder_factory.as_deref(),
        encoder
            .as_ref()
            .and_then(|e| serialized_property(e, "device-path")),
        &cfg.render_node,
    );
    let encoder_sink_caps = encoder.as_ref().and_then(|e| current_caps(e, "sink"));
    // actual codec: the media type on the encoder's SRC (bitstream) caps.
    let actual_codec = encoder
        .as_ref()
        .and_then(|e| current_caps(e, "src"))
        .as_deref()
        .and_then(codec_from_caps_str);
    let source_caps = current_caps(interpipesrc, "src");
    let prop = |name: &str| encoder.as_ref().and_then(|e| serialized_property(e, name));
    let app_gpu_requested = cfg.container.as_ref().is_some_and(|c| c.gpu);
    let memory_path = encoder_sink_caps
        .as_deref()
        .and_then(|caps| {
            ["CUDAMemory", "VAMemory", "VulkanImage", "DMABuf"]
                .into_iter()
                .find(|feature| caps.contains(feature))
        })
        .unwrap_or("system");
    let console = cfg.console_config.as_ref();
    // #384: what the app container was told its display is, which the stream mode alone
    // cannot answer (rendered at 1440p, or rendered at 1080p and upscaled?). Computed by
    // the same function the launch uses over the same catalog env, so it cannot drift
    // from what was injected.
    let app_display_mode = AppDisplayMode {
        width: cfg.stream.width,
        height: cfg.stream.height,
        fps: cfg.stream.fps,
    };
    let app_display = app_display_env(
        app_display_mode,
        &cfg.container
            .as_ref()
            .map(|c| c.env.clone())
            .unwrap_or_default(),
    );
    let app_display_injected: serde_json::Map<String, serde_json::Value> = app_display
        .vars
        .iter()
        .map(|(k, v)| (k.clone(), serde_json::Value::String(v.clone())))
        .collect();
    let audio_path = audio_path_label(
        pipeline::audio_disabled(),
        cfg.audio_degraded_reason.is_some(),
        cfg.use_test_audio,
    );
    // Must stay the same condition `pipeline::build_encode_pipeline` builds the audio
    // pipeline on: it is what a mic m-line can exist under.
    let has_audio_pc = !pipeline::audio_disabled()
        && (cfg.video_topology != VideoTopology::DualOutput
            || cfg.console_config.as_ref().is_none_or(|c| c.stream_audio));
    let mic_state = mic_state_label(cfg.stream.mic, pipeline::mic_disabled(), has_audio_pc);

    serde_json::json!({
        "configured": {
            "render_node": cfg.render_node_configured,
            // The host-configured encoder, retained across a per-session codec fallback;
            // the actual factory below reveals the fallback.
            "encoder": format!("{:?}", cfg.configured_encoder).to_lowercase(),
            // The control-plane-assigned codec (sessions.codec). Under a QUASAR_CODEC
            // override this stays the assigned value; `actual.codec` reveals the real one.
            "codec": cfg.configured_codec.as_str(),
            "bitrate_kbps": cfg.stream.bitrate_kbps,
            "profile": cfg.stream.h264_profile,
            "gop": cfg.gop,
            "slices": cfg.num_slices,
            "game_gpu_access": app_gpu_requested
        },
        "resolved": {
            "render_node": cfg.render_node,
            "cuda_device_id": cfg.cuda_device_id,
            "stream": true,
            "stream_audio": console.is_none_or(|c| c.stream_audio),
            "local_output": local_backend.is_some(),
            "local_backend": local_backend,
            "connector": console.map(|c| c.connector.as_str()).unwrap_or("auto")
        },
        "actual": {
            "game_gpu_access": app_gpu_requested,
            "compositor_render_node": cfg.render_node,
            "source_caps": source_caps,
            "encoder_sink_caps": encoder_sink_caps,
            "memory_path": memory_path,
            "zero_copy": memory_path != "system",
            "codec": actual_codec,
            // True when QUASAR_CODEC forced a codec different from the assigned one.
            "codec_env_override": cfg.codec_env_override,
            "encoder_factory": encoder_factory,
            "encoder_device_path": encoder_device_path,
            "encoder_cuda_device_id": prop("cuda-device-id"),
            "rate_control": prop("rate-control"),
            "bitrate": prop("bitrate"),
            "profile": prop("profile"),
            "gop": prop("gop-size").or_else(|| prop("key-int-max")).or_else(|| prop("idr-period")),
            "slices": prop("num-slices"),
            "source_topology": "interpipe",
            "stream": true,
            "local_output": local_backend.is_some(),
            "local_backend": local_backend
        },
        // The app container's own display mode (what the app and any nested gamescope
        // see), distinct from the streamed mode above. `source`: `agent` |
        // `app-catalog` | `disabled` (knob `QUASAR_APP_DISPLAY_ENV`). Configured, not
        // measured: waylanddisplaysrc exposes no app-surface geometry.
        "app_display": {
            "width": app_display_mode.width,
            "height": app_display_mode.height,
            "refresh_hz": app_display_mode.fps,
            "source": app_display.source.as_str(),
            "gamescope_env": app_display.gamescope_env,
            "injected": app_display_injected
        },
        // `path`: sidecar (real app audio) | test-tone (QUASAR_USE_TEST_AUDIO) |
        // silent-fallback (sidecar wanted but unavailable, the session is MUTE) |
        // disabled (QUASAR_AUDIO_DISABLED, no audio m-line, #304). `degraded` is true
        // only for silent-fallback. Exists because a mute session was otherwise
        // indistinguishable from a healthy one above the agent: the fallback logged one
        // WARN and still reported `running`. `QUASAR_AUDIO_REQUIRED` makes it terminal.
        "audio": {
            "path": audio_path,
            "degraded": cfg.audio_degraded_reason.is_some(),
            "reason": cfg.audio_degraded_reason,
            "required": cfg.audio_required,
            "pulse_server": cfg.pulse_server.is_some()
        },
        // Mic capture state; see `mic_state_label`. Oversight sees state only, never
        // content: the agent decodes straight into the sidecar and records nothing.
        "mic": mic_state
    })
}

/// Does this bus message text say the graph could not agree a format? Used only to pick
/// `encoder.stall`'s reason discriminant; never fails a session. Matched over GError +
/// debug text, because GStreamer spells it several ways.
fn is_negotiation_message(text: &str) -> bool {
    let t = text.to_ascii_lowercase();
    t.contains("not-negotiated") || t.contains("not negotiated") || t.contains("could not link")
}

/// Read one string-valued field off a caps' first structure.
fn caps_field_str(caps: &gst::Caps, field: &str) -> Option<String> {
    caps.structure(0)?.get::<String>(field).ok()
}

/// The frame size + rate the encoder input is negotiated at, read off live caps.
/// `None` for a field the caps do not carry (an unnegotiated pad, an odd format).
fn caps_size(caps: &gst::Caps) -> serde_json::Value {
    let Some(st) = caps.structure(0) else {
        return serde_json::Value::Null;
    };
    let fps = st
        .get::<gst::Fraction>("framerate")
        .ok()
        .filter(|f| f.denom() != 0)
        .map(|f| f.numer() / f.denom());
    serde_json::json!({
        "w": st.get::<i32>("width").ok(),
        "h": st.get::<i32>("height").ok(),
        "fps": fps,
    })
}

/// The `caps.negotiated` payload: what the encode branch agreed, right now. Separate from
/// the one-shot `session.effective_media`, which goes stale after the first rung step.
/// Pure reads of live pad caps and element properties on the runner's tick: no probes, no
/// property writes, nothing that can renegotiate (#270).
fn caps_negotiated_payload(
    trigger: &str,
    encode_pipe: &gst::Pipeline,
    encode_size: (i32, i32),
    fps: i32,
) -> serde_json::Value {
    let encoder = encode_pipe
        .by_name(pipeline::VIDEO_ENCODER_NAME)
        .or_else(|| encode_pipe.by_name(pipeline::VULKAN_ENCODER_NAME));
    let encoder_factory = encoder
        .as_ref()
        .and_then(|e| e.factory())
        .map(|f| f.name().to_string());
    let sink_caps = encoder
        .as_ref()
        .and_then(|e| e.static_pad("sink"))
        .and_then(|p| p.current_caps());
    let src_caps = encoder
        .as_ref()
        .and_then(|e| e.static_pad("src"))
        .and_then(|p| p.current_caps());
    let pay_caps = encode_pipe
        .by_name(pipeline::VIDEO_PAYLOADER_NAME)
        .and_then(|e| e.static_pad("src"))
        .and_then(|p| p.current_caps());
    // VA/NVENC/Vulkan encoders expose no `profile` property (the downstream capsfilter
    // selects it), so the negotiated src caps are the authority; the property is only a
    // fallback for an encoder that has one (openh264).
    let profile = src_caps
        .as_ref()
        .and_then(|c| caps_field_str(c, "profile"))
        .or_else(|| {
            encoder
                .as_ref()
                .and_then(|e| serialized_property(e, "profile"))
        });
    let level = src_caps.as_ref().and_then(|c| caps_field_str(c, "level"));
    let codec = src_caps
        .as_ref()
        .map(|c| c.to_string())
        .as_deref()
        .and_then(codec_from_caps_str);
    // Prefer the size the encoder input really negotiated; fall back to the session's
    // pinned encode size when the pad has no caps yet (a pre-roll race on the tick).
    let size = match sink_caps.as_ref() {
        Some(c) => caps_size(c),
        None => serde_json::json!({ "w": encode_size.0, "h": encode_size.1, "fps": fps }),
    };
    serde_json::json!({
        "trigger": trigger,
        "encoder_factory": encoder_factory,
        "codec": codec,
        "profile": profile,
        "level": level,
        "encoder_sink_caps": sink_caps.map(|c| c.to_string()),
        "encoder_src_caps": src_caps.map(|c| c.to_string()),
        "payloader_src_caps": pay_caps.map(|c| c.to_string()),
        "size": size,
    })
}

/// The scale-stage / display context an `encoder_props` capture reports. Built here from
/// state the runner already holds so `session::capture` never reaches into pipeline
/// internals. Pure reads: no locks, no property sets, nothing that can renegotiate.
fn capture_stage_snapshot(
    lever: &pipeline::EncodeResolutionLever,
    encode_size: (i32, i32),
    current_render: Option<(i32, i32)>,
    current_ui_scale: Option<f64>,
) -> serde_json::Value {
    let stage = lever.stage();
    let (req_w, req_h) = stage.requested();
    let (cur_w, cur_h) = stage.current();
    serde_json::json!({
        "external_resize_supported": stage.supported,
        "fps_lever": stage.fps_lever,
        "launch": { "width": stage.launch.0, "height": stage.launch.1, "fps": stage.fps },
        // Asked-for vs negotiated: a persistent gap is a resize that never completed.
        "requested": { "width": req_w, "height": req_h },
        "current": { "width": cur_w, "height": cur_h, "fps": stage.current_fps() },
        // The pinned encode size and the compositor's app-facing mode, the same pair
        // `session_metrics` echoes.
        "encode_size": { "width": encode_size.0, "height": encode_size.1 },
        "render": current_render.map(|(w, h)| serde_json::json!({ "width": w, "height": h })),
        "ui_scale": current_ui_scale,
    })
}

/// A discrete host-side trace event, forwarded on the bounded diagnostic lane.
#[derive(Debug, Clone)]
pub struct TraceEvent {
    pub ts_unix_ms: i64,
    pub event: &'static str,
    pub payload: serde_json::Value,
}

/// Bounded, non-blocking diagnostics lane. Lifecycle, terminal, swap and
/// signaling events use a separate reliable channel, so a trace flood cannot
/// consume its memory or delay critical events.
#[derive(Clone)]
pub struct DiagnosticEventTx {
    tx: mpsc::Sender<(String, TraceEvent)>,
    dropped_interval: Arc<AtomicU64>,
    dropped_total: Arc<AtomicU64>,
}

impl DiagnosticEventTx {
    pub fn new(
        tx: mpsc::Sender<(String, TraceEvent)>,
        dropped_interval: Arc<AtomicU64>,
        dropped_total: Arc<AtomicU64>,
    ) -> Self {
        Self {
            tx,
            dropped_interval,
            dropped_total,
        }
    }

    pub fn try_emit(&self, session_id: String, event: TraceEvent) {
        if self.tx.try_send((session_id, event)).is_err() {
            self.dropped_interval.fetch_add(1, Ordering::Relaxed);
            self.dropped_total.fetch_add(1, Ordering::Relaxed);
        }
    }
}

/// A request to swap the session's source app, from `session_swap_app`.
#[derive(Debug, Clone)]
pub struct SwapRequest {
    /// The new app container to launch, or `None` for a bare compositor.
    pub container: Option<super::container::ContainerSpec>,
}

/// `protocol/agent-api.md` `session_display_update`. Partial: `None` means "leave as is".
/// Validated before it reaches the runner ([`validate_display_update`], called
/// synchronously in the agent loop so a rejection can be acked `ok:false`); the runner
/// only applies.
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct DisplayUpdateRequest {
    pub render_width: Option<i32>,
    pub render_height: Option<i32>,
    pub ui_scale: Option<f64>,
    /// The new external (encoded) size, i.e. the frame size on the wire. `None` leaves
    /// it as is. One `(w, h)` rather than two Options: meaningless by halves.
    pub stream: Option<(i32, i32)>,
}

/// Floor for a render dimension (agent-api.md `session_display_update`).
const RENDER_DIM_MIN: i32 = 16;
/// UI-scale bounds (agent-api.md `session_display_update`).
const UI_SCALE_MIN: f64 = 1.0;
const UI_SCALE_MAX: f64 = 3.0;

/// What the agent loop needs to validate a `session_display_update` synchronously, before
/// it must ack. Held per running session in `agent::RunningHandle`.
///
/// The render and external axes are INDEPENDENT: each is bounded only by `launch`, neither
/// bounds the other, and `render` may sit above `external` (4K internal on a wire degraded
/// to 1080p). The compositor always composites into its launch-size framebuffer and
/// `session::pipeline::scale_stage` resamples it to `external`, so an external step is
/// invisible to the app and can never move `render`.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct SessionDisplayState {
    /// The assigned (pinned) encode size: the ceiling and the top rung.
    pub launch: (i32, i32),
    /// The live external (encoded) size. Equals `launch` until a `stream_*` update.
    pub external: (i32, i32),
    /// The live render size; `None` is the launch size.
    pub render: Option<(i32, i32)>,
    /// `pipeline::external_resize_supported`. A `stream_*` request is rejected when
    /// false, never silently ignored.
    pub external_resize_supported: bool,
}

impl SessionDisplayState {
    /// A freshly started session: at its launch size, no render override.
    pub fn new(launch: (i32, i32), external_resize_supported: bool) -> Self {
        Self {
            launch,
            external: launch,
            render: None,
            external_resize_supported,
        }
    }

    /// Fold an accepted update (`eff` as returned by [`validate_display_update`]) forward.
    /// Independent axes: each half moves only its own field. `render` must move only
    /// through [`fold_render_size`], shared with the runner's fold, so the agent's mirror
    /// and the runner's live state cannot drift.
    pub fn apply(&mut self, eff: &DisplayUpdateRequest) {
        if let Some((w, h)) = eff.stream {
            self.external = (w, h);
        }
        if let (Some(w), Some(h)) = (eff.render_width, eff.render_height) {
            fold_render_size(&mut self.render, (w, h), self.launch);
        }
    }
}

/// The one render-size fold rule, shared by the runner's live state
/// ([`fold_display_update`]) and the agent's mirror ([`SessionDisplayState::apply`]) so
/// the two cannot drift.
///
/// A render size equal to the LAUNCH size folds to `None`, which stops the
/// `session_metrics` echo and makes the runner send the compositor its `"0x0"`
/// follow-the-framebuffer reset. Compared against launch, never the current external
/// size: the external size lives downstream of the compositor.
fn fold_render_size(render: &mut Option<(i32, i32)>, requested: (i32, i32), launch: (i32, i32)) {
    *render = if requested == launch {
        None
    } else {
        Some(requested)
    };
}

/// Validate a `session_display_update` against a session's live display state and return
/// the effective request for the runner. `Err(why)` makes the agent ack `ok:false` with
/// `display_update_rejected: <why>`, leaving the session untouched; a rejected update
/// never fails the session (`protocol/agent-api.md`).
///
/// Rules:
/// - `render_width`/`render_height` both-or-neither; `16 ≤ dim ≤` the LAUNCH size; even.
/// - `1.0 ≤ ui_scale ≤ 3.0`.
/// - `stream_*` must be a rung of this session's aspect family (`session::rungs`, the
///   mirror of the control plane's table) and needs a live-resize lever.
///
/// Never rewrites the request: the render and external axes are independent and there is
/// no `internal ≤ external` clamp, because the scale stage downsamples the launch-size
/// framebuffer to the external rung. The return is always `*req` modulo rejections.
pub fn validate_display_update(
    req: &DisplayUpdateRequest,
    st: &SessionDisplayState,
) -> Result<DisplayUpdateRequest, String> {
    let eff = *req;

    if let Some((w, h)) = req.stream {
        if !st.external_resize_supported {
            return Err("encoder does not support live resize".to_string());
        }
        if !crate::session::rungs::is_rung(w, h, st.launch.0, st.launch.1) {
            return Err(format!(
                "stream size {w}x{h} is not a rung of this session (launch {}x{})",
                st.launch.0, st.launch.1
            ));
        }
    }
    match (req.render_width, req.render_height) {
        (Some(w), Some(h)) => {
            // Bounded by the LAUNCH size, not the current external size: a render size
            // above external is legal and is resolved downstream by the scale stage.
            for (label, v, max) in [
                ("render_width", w, st.launch.0),
                ("render_height", h, st.launch.1),
            ] {
                if v < RENDER_DIM_MIN {
                    return Err(format!("{label} {v} below floor {RENDER_DIM_MIN}"));
                }
                if v > max {
                    return Err(format!("{label} {v} above the session launch size {max}"));
                }
                if v % 2 != 0 {
                    return Err(format!("{label} {v} is not even"));
                }
            }
        }
        // External-only: an external step never touches the render size.
        (None, None) => {}
        _ => {
            return Err("render_width and render_height must be sent together".to_string());
        }
    }

    if let Some(s) = req.ui_scale {
        if !s.is_finite() || !(UI_SCALE_MIN..=UI_SCALE_MAX).contains(&s) {
            return Err(format!(
                "ui_scale {s} outside {UI_SCALE_MIN}..={UI_SCALE_MAX}"
            ));
        }
    }
    if req.render_width.is_none() && req.ui_scale.is_none() && req.stream.is_none() {
        return Err("empty display update".to_string());
    }
    Ok(eff)
}

/// Fold a partial display update into the session's live values; `None` leaves as is.
/// Split out from [`apply_display_update`] so it is unit-testable without a pipeline.
fn fold_display_update(
    req: &DisplayUpdateRequest,
    current_render: &mut Option<(i32, i32)>,
    current_ui_scale: &mut f64,
    encode: (i32, i32),
) {
    if let (Some(w), Some(h)) = (req.render_width, req.render_height) {
        fold_render_size(current_render, (w, h), encode);
    }
    if let Some(s) = req.ui_scale {
        *current_ui_scale = s;
    }
}

/// Apply the external half of a display update: retarget the encode-side scale stage.
///
/// Returns nothing, because the resize completes asynchronously (the capsfilter
/// renegotiates on the next buffer) and `ScaleStage::current()` here is deterministically
/// one step stale. The `session_metrics` echo is written from the lever's completion
/// callback instead — see [`resolution_lever_with_echo`] and `resize::arm_on_next_caps`.
///
/// Fail-open: a refused retarget warns and changes nothing rather than failing the
/// session.
fn apply_stream_update(lever: &pipeline::EncodeResolutionLever, w: i32, h: i32) {
    // `set_rung_tagged`, not `set_rung`: a human resize, so its `caps.negotiated` must
    // carry `trigger:"scale_rebuild"` rather than the ladder's `rung_step`.
    match lever.set_rung_tagged(w, h, pipeline::trigger::SCALE_REBUILD) {
        Ok(_) => {
            // D4: a HUMAN set this size (this function is only ever reached from a
            // `session_display_update`, i.e. PATCH /v1/sessions/{id}/display). A
            // non-launch size PINS the session so the ABR ladder stops moving it; the
            // launch size RELEASES it back to `auto`. `Ok(false)` (a no-op resize) still
            // counts — re-picking the size you are already on is still a statement of
            // ownership. A refused (`Err`) retarget changes nothing: the size never moved,
            // so neither does its ownership.
            let pinned = (w, h) != lever.stage().launch;
            lever.set_pinned(pinned);
            tracing::info!(
                "external resolution: retarget to {w}x{h} issued by request — session is now {}",
                if pinned {
                    "PINNED (ladder will not move it)"
                } else {
                    "AUTO (ladder may move it)"
                }
            );
        }
        Err(e) => tracing::warn!(
            token = "external-resolution-retarget-refused",
            "external resolution: retarget to {w}x{h} refused: {e:#}"
        ),
    }
}

/// Apply an already-validated display update to a source generation's compositor element
/// and fold it into the echo state.
///
/// Must NOT re-inject `QUASAR_STREAM_*`: those describe the encode size, while apps read
/// render size from `wl_output.mode`.
///
/// Fail-open on a compositor image predating the `render-width`/`render-height`/`ui-scale`
/// properties: warn and change nothing, since `set_property` on an absent property panics
/// in gstreamer-rs and a panicking runner thread kills the session.
fn apply_display_update(
    source: &AppSource,
    req: &DisplayUpdateRequest,
    current_render: &mut Option<(i32, i32)>,
    current_ui_scale: &mut f64,
    encode: (i32, i32),
    metrics: &SessionMetrics,
    // The encode-side lever. `None` on a local-only console session, which has no encode
    // pipeline; the agent loop already rejects `stream_*` there.
    lever: Option<&pipeline::EncodeResolutionLever>,
) {
    if let Some((w, h)) = req.stream {
        match lever {
            // The SIZE echo is written from the lever's completion callback when the
            // renegotiation lands; only the OWNER half belongs here, because ownership is
            // decided by the request, not by the renegotiation.
            Some(lever) => {
                apply_stream_update(lever, w, h);
                metrics.set_external_owner_pinned(lever.pinned());
            }
            None => tracing::warn!(
                token = "display-update-no-encode-pipeline",
                "display update carried stream {w}x{h} but this session has no encode \
                 pipeline — ignored"
            ),
        }
    }
    fold_display_update(req, current_render, current_ui_scale, encode);
    push_display_to_source(source, *current_render, *current_ui_scale, metrics);
}

/// Push the live render size / UI scale onto a compositor generation and update the
/// metrics echo to match what the compositor took. `render == None` goes over the wire as
/// the compositor's `"0x0"` reset, not as the encode numbers. The echo resets to defaults
/// when the compositor did not take the properties, so `session_metrics` never reports a
/// value that is not on screen.
fn push_display_to_source(
    source: &AppSource,
    render: Option<(i32, i32)>,
    ui_scale: f64,
    metrics: &SessionMetrics,
) {
    if source.set_display_properties(render, ui_scale) {
        metrics.set_display(render, ui_scale);
    } else {
        metrics.set_display(None, 1.0);
    }
}

/// Bus poll cadence — also the responsiveness of stop, offer detection, and
/// inbound signaling dispatch.
const POLL: u64 = 100;

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

/// #408: RAII owner of the session's optional audio `gst::Pipeline`.
///
/// `gst::Pipeline` has no Drop-to-NULL (unlike `AppSource`): a READY audio pipeline that
/// is merely dropped LEAKS its `webrtcbin` + libnice agent + `GMainContext` thread + bound
/// UDP sockets forever. The guard is created at the destructure so every return path is
/// covered.
///
/// [`Self::finish`] is the explicit idempotent teardown, called by the ordered exit paths
/// at the exact point audio must go NULL relative to source/encode teardown; Drop is a
/// no-op afterwards and only fires for a path that forgot. `finished` is a `Cell` so
/// `finish()` takes `&self` (single-threaded, owned by the runner thread).
pub(crate) struct AudioPipelineGuard {
    pipeline: Option<gst::Pipeline>,
    finished: std::cell::Cell<bool>,
}

impl AudioPipelineGuard {
    pub(crate) fn new(pipeline: Option<gst::Pipeline>) -> Self {
        Self {
            pipeline,
            finished: std::cell::Cell::new(false),
        }
    }

    pub(crate) fn as_ref(&self) -> Option<&gst::Pipeline> {
        self.pipeline.as_ref()
    }

    /// Explicit NULL, idempotent: a second call (and the subsequent Drop) does nothing.
    pub(crate) fn finish(&self) {
        if self.finished.replace(true) {
            return;
        }
        if let Some(p) = &self.pipeline {
            let _ = p.set_state(gst::State::Null);
        }
    }
}

impl Drop for AudioPipelineGuard {
    fn drop(&mut self) {
        self.finish();
    }
}

/// Should an idle `running` session be reaped? `Some(reason)` to reap. Pure so the policy
/// is unit tested. `window` is the idle timeout; `Duration::ZERO` disables reaping.
///
/// A session whose transport NEVER established is reaped after a full window of `running`.
/// One whose transport DID establish is reaped only after the transport has been
/// continuously gone for the window, so a blip does not kill it (schema.md invariant #4:
/// transport state is not session state).
///
/// `booting` (#484) exempts a session whose app container is up but has not presented: it
/// is starting, not idle. The caller measures `running_for` from the app-presented instant
/// (falling back to `running_since` for an appless session) and pairs this with a bounded
/// boot watchdog, so deferring here cannot make a session unkillable.
fn idle_reap_reason(
    window: Duration,
    running_for: Duration,
    ever_connected: bool,
    unhealthy_for: Option<Duration>,
    booting: bool,
) -> Option<&'static str> {
    if window.is_zero() {
        return None;
    }
    if !ever_connected {
        if booting {
            return None;
        }
        return (running_for >= window).then_some("idle: WebRTC transport never established");
    }
    match unhealthy_for {
        Some(d) if d >= window => Some("idle: WebRTC transport lost"),
        _ => None,
    }
}

/// Whether an ICE connection state counts as "transport established".
fn ice_is_connected(state: gstreamer_webrtc::WebRTCICEConnectionState) -> bool {
    use gstreamer_webrtc::WebRTCICEConnectionState as S;
    matches!(state, S::Connected | S::Completed)
}

/// Whether an ICE connection state counts as "transport durably gone" (as opposed
/// to the transient New/Checking phase before a first connection).
fn ice_is_dead(state: gstreamer_webrtc::WebRTCICEConnectionState) -> bool {
    use gstreamer_webrtc::WebRTCICEConnectionState as S;
    matches!(state, S::Failed | S::Disconnected | S::Closed)
}

/// How long a swap waits for the new generation's compositor to announce its Wayland
/// socket before rolling back. Nothing has been taken from the outgoing generation yet, so
/// a timeout here is a free rollback.
const SWAP_COMPOSITOR_TIMEOUT: Duration = Duration::from_secs(20);

/// Budget for the second swap phase: the replacement app container must present a frame.
/// Much larger than the compositor budget because a Steam-derived image takes 4.5–10.6 s
/// to reach its ready gate (#384), plus any cold image pull. Knob:
/// `QUASAR_SWAP_APP_READY_TIMEOUT_MS`.
const SWAP_APP_READY_TIMEOUT_DEFAULT_MS: u64 = 45_000;

/// [`SWAP_APP_READY_TIMEOUT_DEFAULT_MS`] with its env override. A non-numeric or zero
/// value falls back to the default rather than making every swap fail instantly.
fn swap_app_ready_timeout() -> Duration {
    let ms = std::env::var("QUASAR_SWAP_APP_READY_TIMEOUT_MS")
        .ok()
        .and_then(|v| v.parse::<u64>().ok())
        .filter(|v| *v > 0)
        .unwrap_or(SWAP_APP_READY_TIMEOUT_DEFAULT_MS);
    Duration::from_millis(ms)
}

/// Poll interval for both swap wait loops.
const SWAP_POLL: Duration = Duration::from_millis(20);

/// #484 boot budget: how long a launched app may take to present its first frame before
/// the session fails `app_never_presented`. 300 s covers a cold managed home plus a cold
/// image pull; a false "your game is broken" would be a new defect. Knob:
/// `QUASAR_APP_BOOT_TIMEOUT_SECS`.
const APP_BOOT_TIMEOUT_DEFAULT_SECS: u64 = 300;

/// How often the boot watcher logs that it is still waiting.
const APP_BOOT_WAIT_LOG_INTERVAL: Duration = Duration::from_secs(15);

/// [`APP_BOOT_TIMEOUT_DEFAULT_SECS`] with its env override. `0` disables the watchdog
/// (`None`), leaving the idle reaper unchanged; a non-numeric value falls back to the
/// default rather than silently disabling it.
fn app_boot_timeout() -> Option<Duration> {
    let secs = match std::env::var("QUASAR_APP_BOOT_TIMEOUT_SECS") {
        Ok(v) => match v.trim().parse::<u64>() {
            Ok(n) => n,
            Err(_) => {
                tracing::warn!(
                    token = "knob-invalid-app-boot-timeout",
                    "QUASAR_APP_BOOT_TIMEOUT_SECS={v:?} is not a number; \
                     using the default {APP_BOOT_TIMEOUT_DEFAULT_SECS}s"
                );
                APP_BOOT_TIMEOUT_DEFAULT_SECS
            }
        },
        Err(_) => APP_BOOT_TIMEOUT_DEFAULT_SECS,
    };
    (secs > 0).then(|| Duration::from_secs(secs))
}

/// The gen-0 (session launch) app-presented gate, pure so the latch is unit tested (#484).
///
/// `booting` means "this generation launched an app container and has not presented yet";
/// false for a generation with no app container and for one that already latched, so
/// `AppPresented` is emitted at most once per generation.
///
/// The gate is `swap_source_ready` verbatim: the raw `app-surface-commits` counter also
/// counts commits from unmapped `pending_windows` surfaces (#487), so it can advance
/// before anything is visible. Requiring a compositor frame observed after the first
/// commit is the mitigation; fixing #487 upstream would let both gates simplify.
fn gen0_app_presented(
    booting: bool,
    app_commits: Option<u64>,
    sink_frames: u64,
    frames_at_first_commit: &mut Option<u64>,
) -> bool {
    booting && swap_source_ready(true, app_commits, sink_frames, frames_at_first_commit)
}

/// Has the app-boot watchdog expired? Pure so the policy is unit tested. `budget = None`
/// never fires, nor does a session that has already presented.
fn app_boot_watchdog_fires(budget: Option<Duration>, elapsed: Duration, presented: bool) -> bool {
    match budget {
        Some(b) if !presented => elapsed >= b,
        _ => false,
    }
}

/// Why a swap did not complete, and whether the session survived it.
///
/// `fatal = false` is the ordinary rollback: the previous app runs again and the session
/// keeps streaming it (`SwapRolledBack`). `fatal = true` means the outgoing app was
/// already stopped and could not be brought back, leaving a live compositor with nothing
/// in it, so the caller must fail the session rather than report a rollback.
#[derive(Debug, Clone, PartialEq, Eq)]
struct SwapFailure {
    reason: String,
    fatal: bool,
}

impl SwapFailure {
    fn rolled_back(reason: impl Into<String>) -> Self {
        SwapFailure {
            reason: reason.into(),
            fatal: false,
        }
    }
}

/// Is the new source generation showing the application, rather than the compositor's own
/// empty scene?
///
/// The caller MUST sample `app_commits` before `sink_frames`, so `sink_frames` is observed
/// no earlier than the commit it is compared against. The gate: at least one app-surface
/// commit, plus at least one compositor frame reaching the interpipe boundary after it.
///
/// `expects_app == false` and a compositor that does not export the counter both fall back
/// to "the compositor produced a frame", the only signal available there.
fn swap_source_ready(
    expects_app: bool,
    app_commits: Option<u64>,
    sink_frames: u64,
    frames_at_first_commit: &mut Option<u64>,
) -> bool {
    match app_commits {
        Some(commits) if expects_app => {
            if commits == 0 {
                return false;
            }
            let baseline = *frames_at_first_commit.get_or_insert(sink_frames);
            sink_frames > baseline
        }
        _ => sink_frames > 0,
    }
}

/// Run one session to completion on the calling (dedicated) thread, over the split
/// pipeline so the source app can be swapped while encode + `webrtcbin` stay live.
///
/// `sig_in_rx` carries inbound answer/ICE from the control-plane relay; outbound offer/ICE
/// goes out via `evt_tx`. `swap_rx` carries `session_swap_app` requests.
#[allow(clippy::too_many_arguments)]
pub fn run_blocking(
    session_id: String,
    mut cfg: SessionConfig,
    evt_tx: mpsc::Sender<(String, SessionEvent)>,
    diagnostic_tx: DiagnosticEventTx,
    stop: Arc<AtomicBool>,
    sig_in_rx: std::sync::mpsc::Receiver<SignalMsg>,
    swap_rx: std::sync::mpsc::Receiver<SwapRequest>,
    display_rx: std::sync::mpsc::Receiver<DisplayUpdateRequest>,
    // `session_capture` requests; the agent loop has already admitted each one and
    // reserved its single-flight slot.
    capture_rx: std::sync::mpsc::Receiver<CaptureRequest>,
    session_metrics: Arc<SessionMetrics>,
) {
    // Everything this thread logs belongs to one session, so name it once here rather
    // than threading `session_id` through every call site. Spawned threads re-enter a
    // clone (`Span::current()`); GStreamer's streaming threads cannot, which is why
    // probe/callback sites keep an explicit `session_id` field
    // (`.claude/rules/agent-logging.md`).
    let session_span = crate::logging::session_span(&session_id);
    let _session_span_guard = session_span.enter();

    let emit = |e: SessionEvent| {
        // Dedicated runner thread: blocking gives bounded backpressure without blocking
        // the async control loop or dropping a critical event.
        let _ = evt_tx.blocking_send((session_id.clone(), e));
    };

    // Break the ABR hook ref cycle on EVERY exit path. `abr_glue`'s `on_drain`/`on_window`
    // hooks hold the encoder + rtpgccbwe strong while the encoder's pad probes hold this
    // `SessionMetrics` Arc, so encoder -> probe -> metrics -> hook -> encoder never
    // finalizes and LEAKS the encoder's VkDevice/DPB (~0.5 GiB VRAM per Vulkan session)
    // past teardown. See `.claude/rules/gstreamer-gotchas.md`.
    struct MetricsHookGuard(Arc<SessionMetrics>);
    impl Drop for MetricsHookGuard {
        fn drop(&mut self) {
            self.0.clear_hooks();
        }
    }
    let _metrics_hook_guard = MetricsHookGuard(session_metrics.clone());

    if let Err(e) = super::ensure_gst_init(&cfg) {
        tracing::error!(
            token = "runner-gst-init-failed",
            error = %format_args!("{e:#}"),
            "gstreamer init failed"
        );
        emit(SessionEvent::Failed(format!("gstreamer init: {e:#}")));
        return;
    }
    // Apply the QUASAR_CODEC agent-side force override (harness/diagnostic) before
    // resolving the encoder — it rewrites `cfg.stream.codec` (loudly, WARN) while
    // leaving `cfg.configured_codec` at the control-plane-assigned value, so the
    // effective-media snapshot can surface the assigned-vs-streamed divergence.
    cfg.apply_codec_env_override();
    // Resolve the per-session effective encoder now the GStreamer registry is populated.
    // Rewrites `cfg.encoder` in place so every downstream setup keys on the effective
    // encoder with no per-site branching; `configured_encoder` keeps the host-configured
    // value for the effective-media snapshot. `resolved_encoder` is held alive to
    // `build_encode_pipeline` so its `.factory` is threaded through, not re-scanned.
    let resolved_encoder = match pipeline::resolve_effective_encoder(&mut cfg) {
        Ok(r) => r,
        Err(e) => {
            tracing::error!(
                token = "runner-encoder-resolution-failed",
                error = %format_args!("{e:#}"),
                "codec/encoder resolution failed"
            );
            emit(SessionEvent::Failed(format!(
                "codec/encoder resolution: {e:#}"
            )));
            return;
        }
    };
    emit(SessionEvent::Starting);
    emit(SessionEvent::Progress("preparing resources and image"));

    // Session-level resources (input devices + PulseAudio sidecar) shared across
    // swaps. Dropping `res` releases the sidecar; each AppSource borrows its nodes.
    let (res, pulse_server) = match SessionResources::prepare(&session_id, &cfg) {
        Ok(pair) => pair,
        Err(e) => {
            tracing::error!(
                token = "runner-session-resources-failed",
                error = %format_args!("{e:#}"),
                "prepare session resources failed"
            );
            emit(SessionEvent::Failed(format!(
                "prepare session resources: {e:#}"
            )));
            return;
        }
    };
    cfg.pulse_server = pulse_server;
    if cfg.pulse_server.is_none() && !cfg.use_test_audio {
        // The sidecar was wanted and is not there: this session will stream SILENCE. A
        // WARN alone left a mute stack invisible above the agent, so record a reason on
        // effective_media, emit a trace event, and let QUASAR_AUDIO_REQUIRED make it
        // terminal.
        let reason = res
            .audio_degraded_reason()
            .unwrap_or("PulseAudio sidecar unavailable")
            .to_string();
        if cfg.audio_required {
            tracing::error!(
                token = "audio-required-but-unavailable",
                "{reason} — QUASAR_AUDIO_REQUIRED is set, failing the session"
            );
            emit(SessionEvent::Failed(format!(
                "audio required but unavailable: {reason}"
            )));
            return;
        }
        tracing::warn!(
            token = "audio-fallback-silent",
            "{reason} — switching to silent audio fallback"
        );
        cfg.audio_degraded_reason = Some(reason);
        cfg.use_test_audio = true;
    }

    // One clock + base time shared by the source pipeline(s), the encode pipeline and
    // audio (#68). interpipe spans two GstPipelines; without a shared timebase and
    // `do-timestamp=false`, arrival re-stamping collapses GCC to ~245 kbps and ~0.5 fps.
    // Every swap's source gets the same clock+base so PTS stay continuous.
    // See `.claude/rules/gstreamer-gotchas.md`.
    let shared_clock: gst::Clock = gst::SystemClock::obtain();
    // gstreamer-rs 0.25 made `ClockExt::time()` infallible (it returned
    // `Option<ClockTime>` through 0.23, hence the old `.unwrap_or(ZERO)`). The system
    // clock's `gst_clock_get_time` never yields GST_CLOCK_TIME_NONE, so the fallback
    // was unreachable and the base time is unchanged.
    let shared_base = shared_clock.time();

    // The process-global application-owned GstCudaContext, injected into every pipeline
    // before start so the compositor adopts ours and its memory:CUDAMemory surfaces are
    // valid across the interpipe. Global, not per-session: gst-wayland-display LEAKS a ref
    // on the injected context (`cuda_share::shared_cuda_context`), and a per-session
    // context leaked ~500 MiB VRAM per session. `None` on VA/software/non-cuda builds,
    // where the NVENC path is system RGBx + cudaupload.
    #[cfg(feature = "cuda")]
    let cuda_ctx: Option<gst::Context> = if cfg.encoder == super::EncoderChoice::Nvenc {
        match super::cuda_share::shared_cuda_context(cfg.cuda_device_id) {
            Ok(ctx) => Some(ctx),
            Err(e) => {
                tracing::error!(
                    token = "runner-cuda-context-failed",
                    error = %format_args!("{e:#}"),
                    "create shared CUDA context failed"
                );
                emit(SessionEvent::Failed(format!(
                    "create shared CUDA context: {e:#}"
                )));
                return;
            }
        }
    } else {
        None
    };
    #[cfg(not(feature = "cuda"))]
    let cuda_ctx: Option<gst::Context> = None;

    // `va_share` provided a shared GstVaDisplay for the retired in-compositor-NV12 path
    // (#367). No current path constructs one, so `va_ctx` is always `None`; the plumbing
    // is kept in case a future zero-copy path needs cross-pipeline VA display sharing.
    let va_ctx: Option<gst::Context> = None;

    // gen 0: the initial source (compositor + first app container) behind the interpipe
    // boundary; `sink_name` is the node the encoder listens to. Local-only MUST establish
    // the physical DRM mode before the Vulkan capture compositor creates its
    // device/context: on NVIDIA, modesetting Weston after waylanddisplaysrc was already
    // PLAYING reproducibly faulted the source GPU channel (Xid 13/32, then
    // VK_ERROR_DEVICE_LOST).
    let local_backend = console::local_backend(cfg.console_config.as_ref());
    let prestarted_weston = if cfg.video_topology == VideoTopology::LocalOnly
        && local_backend == console::LocalBackend::Weston
    {
        match console::spawn_weston_console(&session_id, cfg.console_config.as_ref()) {
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

    let mut gen: u64 = 0;
    let sink0 = ipsink_name(&session_id, gen);
    let cname0 = app_container_name(&session_id, gen);
    let mut current_source = match AppSource::new(
        &cfg,
        &session_id,
        &sink0,
        &cname0,
        &res,
        cfg.container.clone(),
    ) {
        Ok(s) => s,
        Err(e) => {
            tracing::error!(
                token = "runner-source-build-failed",
                error = %format_args!("{e:#}"),
                "build source pipeline failed"
            );
            emit(SessionEvent::Failed(format!(
                "build source pipeline: {e:#}"
            )));
            return;
        }
    };
    if std::env::var("QUASAR_DIAG_NO_OBS")
        .map(|v| v == "1")
        .unwrap_or(false)
    {
        tracing::warn!(
            token = "diag-observability-disabled",
            "QUASAR_DIAG_NO_OBS=1: compositor/encode/stage probes disabled (leak bisection)"
        );
    } else {
        current_source.attach_compositor_metrics(session_metrics.clone(), cfg.latency_probe);
    }
    current_source.apply_shared_clock(&shared_clock, shared_base);
    // Both contexts must be injected before the source leaves NULL (no-op when None).
    if let Some(ctx) = cuda_ctx.as_ref() {
        current_source.apply_cuda_context(ctx);
    }
    if let Some(ctx) = va_ctx.as_ref() {
        current_source.apply_va_context(ctx);
    }
    // Advertise the session's rung ladder as `wl_output` modes BEFORE the compositor
    // starts, so a guest that reads its mode list once at startup sees all of them. No-op
    // on a compositor image without the property. This is the internal half: what the app
    // may pick from, independent of the external size.
    current_source.set_mode_ladder(&super::rungs::available_rungs(
        cfg.stream.width,
        cfg.stream.height,
    ));
    if let Err(e) = current_source.start() {
        tracing::error!(
            token = "runner-source-start-failed",
            error = %format_args!("{e:#}"),
            "start source pipeline failed"
        );
        emit(SessionEvent::Failed(format!(
            "start source pipeline: {e:#}"
        )));
        return;
    }
    // Retained for the full runner lifetime: every replacement compositor MUST receive
    // these before start or it may create a second logical VkDevice while the persistent
    // encoder stays bound to generation 0.
    let vulkan_contexts = if pipeline::vulkan_image_transport(&cfg) {
        match VulkanContextBridge::capture(&current_source) {
            Ok(contexts) => Some(contexts),
            Err(e) => {
                tracing::error!(
                    token = "runner-vulkan-capture-failed",
                    error = %format_args!("{e:#}"),
                    "capture Vulkan producer contexts failed"
                );
                emit(SessionEvent::Failed(format!(
                    "capture Vulkan producer contexts: {e:#}"
                )));
                current_source.teardown();
                return;
            }
        }
    } else {
        None
    };
    emit(SessionEvent::Progress(
        "container started; first frame ready",
    ));

    // Local-only is an encoder-free topology: no encode pipeline, webrtcbin, RTP/audio
    // transport, signaling or transport idle-reaper. The source feeds only the local
    // display interpipe listener.
    if cfg.video_topology == VideoTopology::LocalOnly {
        // Nothing any capture kind can observe here. `capture::admit` already refuses
        // `session_capture` for this topology; dropping the receiver makes that
        // structural rather than a convention.
        drop(capture_rx);
        run_local_only(
            &session_id,
            &cfg,
            &emit,
            diagnostic_tx,
            stop,
            swap_rx,
            display_rx,
            &res,
            &mut current_source,
            &mut gen,
            session_metrics.clone(),
            &sink0,
            &shared_clock,
            shared_base,
            cuda_ctx.as_ref(),
            va_ctx.as_ref(),
            vulkan_contexts.as_ref(),
            local_backend,
            prestarted_weston,
        );
        return;
    }

    // session-display-update state: the compositor's app-facing render size (`None` is the
    // pinned encode size) and UI scale. Re-applied to each new source generation after a
    // swap; echoed on `session_metrics` only once the compositor has taken it.
    let encode_size = (cfg.stream.width, cfg.stream.height);
    let mut current_render: Option<(i32, i32)> = None;
    let mut current_ui_scale: f64 = 1.0;

    // The persistent encode pipeline: interpipesrc(listen-to=sink0) → encoder →
    // webrtcbin. Stays PLAYING across every swap — the transport never restarts.
    let (sig_tx, mut sig_rx) = tokio::sync::mpsc::unbounded_channel::<SignalMsg>();
    // GStreamer callbacks (ABR retarget, ICE/connection state) use the bounded diagnostic
    // lane; lifecycle/signaling stays on evt_tx.
    let built = match pipeline::build_encode_pipeline(
        &cfg,
        sig_tx.clone(),
        res.devices.clone(),
        res.input_state.clone(),
        &sink0,
        session_metrics.clone(),
        va_ctx.as_ref(),
        diagnostic_tx.clone(),
        session_id.clone(),
        &resolved_encoder.factory,
    ) {
        Ok(t) => t,
        Err(e) => {
            // On a Vulkan session a device-lost-flavored build failure is a pre-session
            // device-open failure (GPU-global); otherwise a per-session build error.
            let err_text = format!("{e:#}");
            let reason = if pipeline::vulkan_image_transport(&cfg) {
                vulkan_fault::build_failure_reason("build encode pipeline", &err_text)
            } else {
                format!("build encode pipeline: {err_text}")
            };
            tracing::error!(
                token = "runner-encode-build-failed",
                reason = %reason,
                "build encode pipeline failed"
            );
            emit(SessionEvent::Failed(reason));
            return;
        }
    };
    let pipeline::EncodePipeline {
        pipeline: encode_pipe,
        webrtc,
        data_channel: _dc,
        interpipesrc,
        audio_webrtc,
        audio_pipeline: audio_pipeline_raw,
        resolution_lever,
    } = built;
    // The scale stage and the IDR that must follow a change are paired inside the lever.
    // Publish the capability BEFORE the first window can drain, so
    // `external_resize_supported` is present from the session's first sample.
    session_metrics.set_external_resize_supported(resolution_lever.stage().supported);
    // #489 (NVENC teardown UAF): under `QUASAR_NVENC_DEFER_TEARDOWN` the encode pipeline
    // is PARKED at teardown and destroyed only once the host holds no live encode lease.
    // The lease MUST be taken here, before the encode pipeline reaches READY/PLAYING and
    // NVENC is opened; it is released on return (Drop covers every exit path) and the last
    // lease out drains the parked pipelines. Keys on the EFFECTIVE encoder.
    let defer_encode_teardown = super::nvenc_defer::deferral_enabled(cfg.encoder);
    let _encode_lease = super::nvenc_defer::EncodeLease::acquire(defer_encode_teardown);
    if defer_encode_teardown {
        tracing::info!(
            "#489: deferred NVENC teardown active — this session's encode pipeline is \
             kept alive (~190 MiB VRAM) after the session ends, until the host has no \
             live encoders. Prevents the driver use-after-free that otherwise takes \
             down every session on the host. Opt out with QUASAR_NVENC_DEFER_TEARDOWN=0"
        );
    }
    // #408: take ownership in a Drop-to-NULL guard the moment the pipeline exists, so
    // every early encode-setup failure return below is covered.
    let audio_pipeline = AudioPipelineGuard::new(audio_pipeline_raw);
    // Same clock + base as the source pipeline(s) so interpipesrc's pass-through PTS land
    // in this pipeline's running-time (#68).
    encode_pipe.set_start_time(None::<gst::ClockTime>);
    encode_pipe.use_clock(Some(&shared_clock));
    encode_pipe.set_base_time(shared_base);
    // The VulkanImage crosses an interpipe boundary between two GstPipelines: install the
    // producer-owned contexts explicitly before READY rather than relying on the timing of
    // interpipe's forwarded context query, then verify what the encoder resolved.
    let expected_vulkan_device = vulkan_contexts
        .as_ref()
        .map(|contexts| install_vulkan_context_bridge(contexts, &encode_pipe));
    // Inject the SAME shared CUDA context before PLAYING so cudaconvert + the NVENC encoder
    // adopt it rather than making a new one, keeping the interpipe's CUDAMemory surfaces
    // valid. set_context at READY is in time: elements bind it at READY->PAUSED.
    if let Some(ctx) = cuda_ctx.as_ref() {
        encode_pipe.set_context(ctx);
    }
    // Same for the shared VA display, so the encoder reuses the compositor's NV12 surfaces
    // instead of importing.
    if let Some(ctx) = va_ctx.as_ref() {
        encode_pipe.set_context(ctx);
        // set_context lands too late for device-pinned vapostproc/vah264enc: they bind a
        // display on NULL->READY before the stored context is read ("Can't replace VA
        // display while operating"). A sync handler answers their NEED_CONTEXT in time.
        super::va_share::install_need_context_handler(&encode_pipe, ctx);
    }
    if let Some(expected) = expected_vulkan_device {
        // A pre-session device-open/verify failure carrying a device-lost signature means
        // the fresh Vulkan device is unusable before any session owns it: a GPU-global
        // `device_open_failed`, which the agent escalates to a drain+restart. A plain
        // build error stays per-session; `build_failure_reason` picks.
        if let Err(e) = encode_pipe.set_state(gst::State::Ready) {
            let reason = vulkan_fault::build_failure_reason(
                "encode set READY for Vulkan device verification",
                &e.to_string(),
            );
            tracing::error!(
                token = "runner-vulkan-ready-failed",
                reason = %reason,
                "encode pipeline READY transition (Vulkan device verification) failed"
            );
            emit(SessionEvent::Failed(reason));
            super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
            current_source.teardown();
            return;
        }
        if let Err(e) = assert_vulkan_encoder_device(&encode_pipe, expected) {
            let reason = vulkan_fault::build_failure_reason(
                "verify Vulkan context identity",
                &format!("{e:#}"),
            );
            tracing::error!(
                token = "runner-vulkan-identity-failed",
                reason = %reason,
                "verify Vulkan context identity failed"
            );
            emit(SessionEvent::Failed(reason));
            super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
            current_source.teardown();
            return;
        }
    }
    if let Err(e) = encode_pipe.set_state(gst::State::Playing) {
        tracing::error!(
            token = "runner-encode-playing-failed",
            error = %e,
            "encode pipeline PLAYING transition failed"
        );
        emit(SessionEvent::Failed(format!("encode set PLAYING: {e}")));
        super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
        return;
    }
    emit(SessionEvent::Progress(
        "media pipeline ready; negotiating transport",
    ));
    // VK-05: the encode pipeline just reached PLAYING — rearm vulkanh264enc's
    // rate-control/bitrate now (gst 1.28.4 drops a pre-PLAYING rate-control set,
    // silently falling back to CQP; see pipeline::rearm_vulkan_rc).
    if pipeline::vulkan_image_transport(&cfg) {
        if let Some(enc) = encode_pipe.by_name(pipeline::VULKAN_ENCODER_NAME) {
            pipeline::rearm_vulkan_rc(&enc, cfg.stream.bitrate_kbps);
        } else {
            tracing::warn!(
                token = "vulkan-encoder-rearm-not-found",
                "VK-05: could not find the Vulkan encoder ({}) in the encode pipeline to \
                 rearm rate-control post-PLAYING — encoder may be stuck at CQP",
                pipeline::VULKAN_ENCODER_NAME
            );
        }
    }
    // #304: the audio pipeline is a separate gst::Pipeline, set PLAYING independently so
    // neither blocks the other's state transition. Teardown is the idempotent
    // `audio_pipeline.finish()`, the one mechanism no exit path can skip.
    if let Some(audio_pipe) = audio_pipeline.as_ref() {
        // Same clock as the encode pipeline, so RTP timestamps stay coherent.
        audio_pipe.set_start_time(None::<gst::ClockTime>);
        audio_pipe.use_clock(Some(&shared_clock));
        audio_pipe.set_base_time(shared_base);
        if let Err(e) = audio_pipe.set_state(gst::State::Playing) {
            tracing::warn!(
                token = "audio-pipeline-play-failed",
                "audio pipeline set PLAYING failed: {e:#} (audio disabled)"
            );
        } else {
            tracing::info!("audio pipeline PLAYING (separate from encode)");
        }
    }
    // Optional local-display fan-out (third interpipe listener into waylandsink+headless
    // weston, or kmssink). The nvidia-drm path needs weston, since kmssink cannot drive
    // nvidia's atomic-only KMS. Gated by console_config.enabled, falling back to the
    // dev-only QUASAR_LOCAL_DISPLAY.
    //
    // Teardown order is load-bearing: `_weston` MUST be declared before `_local_display`
    // so reverse-declaration drop releases waylandsink's surface before weston is killed.
    let console_enabled = match cfg.console_config.as_ref() {
        Some(cc) => cc.enabled && cfg.video_topology == VideoTopology::DualOutput,
        None => std::env::var("QUASAR_LOCAL_DISPLAY")
            .map(|v| !v.trim().is_empty())
            .unwrap_or(false),
    };
    // A real console session must drive the local display: its failure fails the session
    // (fail-closed). The QUASAR_LOCAL_DISPLAY dev fallback stays best-effort, so this is
    // `false` for the env path even when `console_enabled` is true.
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
    let _weston: Option<console::WestonConsole> = 'weston: {
        if !console_enabled || local_backend == console::LocalBackend::DirectKms {
            break 'weston None;
        }
        match console::spawn_weston_console(&session_id, cfg.console_config.as_ref()) {
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
    let _local_display: Option<pipeline::LocalDisplay> = 'local_display: {
        if !console_enabled {
            break 'local_display None;
        }
        // If Weston was selected but failed to come up, don't build the fan-out.
        if local_backend == console::LocalBackend::Weston && _weston.is_none() {
            if console_required {
                fail_dualoutput_console(
                    &emit,
                    "console: weston compositor failed to start for local display".into(),
                    &mut current_source,
                    &encode_pipe,
                    audio_pipeline.as_ref(),
                    defer_encode_teardown,
                );
                return;
            }
            break 'local_display None;
        }
        let socket = _weston.as_ref().map(|weston| weston.socket.as_str());
        let ld = match pipeline::build_local_display_pipeline(&cfg, &sink0, socket) {
            Ok(ld) => ld,
            Err(e) => {
                if console_required {
                    fail_dualoutput_console(
                        &emit,
                        format!("console: build local-display pipeline failed: {e:#}"),
                        &mut current_source,
                        &encode_pipe,
                        audio_pipeline.as_ref(),
                        defer_encode_teardown,
                    );
                    return;
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
        ld.pipeline.use_clock(Some(&shared_clock));
        ld.pipeline.set_base_time(shared_base);
        // The DualOutput local-display leg consumes the shared VulkanImage interpipe via
        // `vulkandownload`, which needs the producer's Vulkan instance+device to map its
        // images. Inject the gen-0 contexts BEFORE this pipeline's NULL->READY transition
        // or `vulkandownload` binds a private device first (the va_share lesson) and the
        // browser stream runs fine while the monitor stays black. Non-Vulkan legs never
        // carry VulkanImage, so `vulkan_contexts` is `None` for them.
        if let Some(vk) = vulkan_contexts.as_ref() {
            vk.install_on_pipeline(&ld.pipeline);
            tracing::info!(
                gst_vulkan_device = format_args!("{:#x}", vk.device_identity),
                "console: shared producer Vulkan instance/device installed on local-display pipeline"
            );
        }
        // Vulkan/VA need the shared GPU context adopted before PLAYING (mirror encode pipe).
        if let Some(ctx) = va_ctx.as_ref() {
            ld.pipeline.set_context(ctx);
            super::va_share::install_need_context_handler(&ld.pipeline, ctx);
        }
        if let Some(ctx) = cuda_ctx.as_ref() {
            ld.pipeline.set_context(ctx);
        }
        if let Err(e) = ld.pipeline.set_state(gst::State::Playing) {
            if console_required {
                // Consumer-before-source teardown: NULL the local-display pipeline before
                // fail_dualoutput_console tears down the producing source.
                let _ = ld.pipeline.set_state(gst::State::Null);
                fail_dualoutput_console(
                    &emit,
                    format!("console: local-display pipeline failed to reach PLAYING: {e:#}"),
                    &mut current_source,
                    &encode_pipe,
                    audio_pipeline.as_ref(),
                    defer_encode_teardown,
                );
                return;
            }
            tracing::warn!(
                token = "console-display-pipeline-play-failed",
                "local-display pipeline PLAYING failed: {e:#} \
                 (console enabled but no local output)"
            );
            break 'local_display None;
        }
        let connector = cfg
            .console_config
            .as_ref()
            .map(|c| c.connector.as_str())
            .unwrap_or("auto");
        tracing::info!(
            "console mode: local-display fan-out PLAYING (connector={connector}, backend={})",
            local_backend.name()
        );
        Some(ld)
    };

    // Optional local-audio fan-out: an independent `pulsesrc` client of the session's
    // PulseAudio sidecar feeding a host ALSA device. Gated on `console_config.enabled` and
    // a `Some` `audio_output`; `audio_output: null` means console video only, so nothing is
    // built rather than built-then-muted (avoids an idle ALSA open and a spurious pulsesrc
    // client). Needs no swap re-pointing: the sidecar is session-scoped and outlives every
    // app swap.
    let _local_audio: Option<pipeline::LocalAudio> = 'local_audio: {
        let Some(cc) = cfg.console_config.as_ref() else {
            break 'local_audio None;
        };
        if !cc.enabled {
            break 'local_audio None;
        }
        let Some(audio_output) = cc.audio_output.as_deref() else {
            break 'local_audio None;
        };
        let la = match pipeline::build_local_audio_pipeline(&cfg, audio_output) {
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
        la.pipeline.use_clock(Some(&shared_clock));
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

    // Optional physical keyboard/mouse grab, forwarded into the session's virtual devices
    // (`physical_input.rs` says why a forwarder, not a second compositor input path).
    // Gated on `console_config.enabled` AND `grab` AND a non-null `input_devices`, so
    // console mode without grab never touches physical devices. Dropping it ungrabs every
    // device, and a leaked grab locks host input.
    let _physical_input: Option<super::physical_input::PhysicalInput> = 'phys_input: {
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
        Some(super::physical_input::PhysicalInput::start(
            &cc.input_devices,
            cc.auto_connect_controller,
            devices,
        ))
    };

    let Some(encode_bus) = encode_pipe.bus() else {
        tracing::error!(
            token = "runner-encode-bus-missing",
            "encode pipeline has no bus"
        );
        emit(SessionEvent::Failed("encode pipeline has no bus".into()));
        super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
        audio_pipeline.finish();
        return;
    };

    let mut running = false;
    let mut effective_media_sent = false;
    // On-demand observation, idle until a `session_capture` lands. The encoder handle is
    // resolved once: the encode pipeline is never structurally touched after build (the
    // no-renegotiation constraint), so it stays valid for the session.
    let mut capture = Capture::new();
    let capture_encoder = encode_pipe
        .by_name(pipeline::VIDEO_ENCODER_NAME)
        .or_else(|| encode_pipe.by_name(pipeline::VULKAN_ENCODER_NAME));
    let mut running_since: Option<Instant> = None;
    let mut ever_connected = false;
    let mut unhealthy_since: Option<Instant> = None;
    // One debounce tracker for the session's lifetime, so a repeat marker across separate
    // bus-pump iterations is what trips the fail-closed path.
    let mut renderer_degrade_tracker = RendererDegradeTracker::default();
    // App-liveness is cadence-gated, never on the 100 ms POLL tick, to keep it off the hot
    // path.
    let mut liveness_cadence_at = Instant::now();
    // #484 gen-0 app-boot gate. `AppBooting` follows `Running` for any generation that
    // launches an app container; `AppPresented` latches the first genuine draw. Both are
    // single-shot.
    let app_boot_budget = app_boot_timeout();
    let mut app_boot_started_at: Option<Instant> = None;
    let mut app_presented_at: Option<Instant> = None;
    let mut app_boot_counter_warned = false;
    let mut app_boot_frames_at_first_commit: Option<u64> = None;
    let mut app_boot_last_wait_log = Instant::now();
    // #503: a `set-remote-description` webrtcbin refuses (e.g. the peer rejected the video
    // m-line because it cannot decode the negotiated codec) is only a webrtcbin WARN,
    // leaving the session `running` until the 300 s app-never-presented timeout. The
    // promise's change-func reports it here instead, drained on the `sig_in_rx` tick.
    let (srd_fail_tx, mut srd_fail_rx) =
        tokio::sync::mpsc::unbounded_channel::<RemoteDescriptionFailure>();
    // The other half of #503: an answer that APPLIED. Accepting an answer says nothing
    // about whether the peer accepted our media — a port-0 / `inactive` m-line is a refusal
    // inside a successful answer.
    let (srd_ok_tx, mut srd_ok_rx) =
        tokio::sync::mpsc::unbounded_channel::<RemoteDescriptionApplied>();
    // `encoder.stall`: output silence with a reason, evaluated on this loop's tick from the
    // two relaxed atomics the existing encode probes already stamp. No new probe, nothing
    // on a streaming thread (#270).
    let mut stall = encoder_stall::StallDetector::default();
    let stall_threshold = Duration::from_millis(encoder_stall::ENCODER_STALL_MS);
    // Sticky for one threshold window, so a stall beginning just after a not-negotiated
    // message still classifies as `negotiation` rather than as a throughput problem.
    let mut negotiation_seen_at: Option<Instant> = None;
    // Per-PC ICE ufrag of the last applied answer, so a re-answer can be told apart from
    // an ICE restart without keeping the SDP itself around.
    let mut last_ice_ufrag: std::collections::HashMap<String, String> =
        std::collections::HashMap::new();
    loop {
        if stop.load(Ordering::Relaxed) {
            emit(SessionEvent::Stopping);
            current_source.teardown(); // remove the app container before media
            super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
            audio_pipeline.finish();
            emit(SessionEvent::Stopped {
                bytes_used: compute_bytes_used(&cfg),
                detail: None,
            });
            return;
        }

        // Outbound signaling (offer/ICE) to the control-plane relay; the first offer means
        // live. The running-transition is single-shot via the `!running` guard, and
        // `AppSource::first_frame` is an idempotent `AtomicBool` store rather than a
        // one-shot promise, so there is no double-satisfy crash path.
        while let Ok(msg) = sig_rx.try_recv() {
            if matches!(msg, SignalMsg::Offer { .. }) && !running {
                running = true;
                running_since = Some(Instant::now());
                emit(SessionEvent::Running);
                // Ordering is load-bearing: `AppBooting` is a detail on a `running`
                // session, so it must follow `Running` on the same ordered lane. Only a
                // generation that launches an app container can report a surface commit.
                if current_source.expects_app_container() {
                    app_boot_started_at = Some(Instant::now());
                    app_boot_last_wait_log = Instant::now();
                    emit(SessionEvent::AppBooting);
                    tracing::info!(
                        session_id = %session_id,
                        "app boot: container {} started; waiting for its first presented frame (gen {gen})",
                        app_container_name(&session_id, gen)
                    );
                }
                if !effective_media_sent {
                    emit(SessionEvent::EffectiveMedia(effective_media_snapshot(
                        &cfg,
                        &encode_pipe,
                        &interpipesrc,
                        _local_display.as_ref().map(|_| local_backend.name()),
                    )));
                    effective_media_sent = true;
                    // Emit the audio-degradation trace event here, not at detection: the
                    // control plane drops an `agent_trace_event` for a session not yet
                    // owned+running (`AgentTraceEventAllowed`), and the sidecar failure
                    // happens during resource prepare, long before the first offer.
                    if let Some(reason) = cfg.audio_degraded_reason.as_deref() {
                        diagnostic_tx.try_emit(
                            session_id.clone(),
                            TraceEvent {
                                ts_unix_ms: now_unix_ms(),
                                event: "audio.degraded",
                                payload: serde_json::json!({
                                    "reason": reason,
                                    "required": cfg.audio_required,
                                }),
                            },
                        );
                    }
                }
                // Outside the `effective_media_sent` latch: `session.effective_media` is a
                // one-shot by contract, this event is re-emitted on every later
                // renegotiation (rung step, scale rebuild, source swap).
                emit(SessionEvent::Trace {
                    event: "caps.negotiated",
                    payload: caps_negotiated_payload(
                        pipeline::trigger::LAUNCH,
                        &encode_pipe,
                        encode_size,
                        cfg.stream.fps,
                    ),
                });
            }
            emit(SessionEvent::Signaling(msg));
        }

        // Apply-only: the agent loop already validated the numbers. See
        // `apply_display_update`.
        while let Ok(req) = display_rx.try_recv() {
            apply_display_update(
                &current_source,
                &req,
                &mut current_render,
                &mut current_ui_scale,
                encode_size,
                &session_metrics,
                Some(&resolution_lever),
            );
        }

        // `encoder.stall` — see `session::encoder_stall`. Only while running: before the
        // first offer a "stall" is pre-roll, not a fault.
        if let Some(since_running) = running_since {
            let now = Instant::now();
            let (since_in, since_out) = session_metrics.encode_flow(now);
            let negotiation = negotiation_seen_at.is_some_and(|t| t.elapsed() < stall_threshold);
            stall.observe(since_out, since_running.elapsed());
            if let Some(ev) = stall.poll(
                stall_threshold,
                since_in,
                since_out,
                since_running.elapsed(),
                negotiation,
            ) {
                let payload = match ev {
                    encoder_stall::StallEvent::Detected { reason, since_ms } => {
                        tracing::warn!(
                            token = "encoder-stall",
                            session_id = %session_id,
                            "encoder-stall: no encoder output for {since_ms} ms (reason={}) —                              {}",
                            reason.as_str(),
                            match reason {
                                encoder_stall::StallReason::NoOutput =>
                                    "frames are still reaching the encoder; the encoder itself                                      has stopped producing",
                                encoder_stall::StallReason::InputStarved =>
                                    "the encoder is being fed nothing — look upstream                                      (compositor / interpipe / app), not at the encoder",
                                encoder_stall::StallReason::Negotiation =>
                                    "a caps/not-negotiated message was on the encode bus —                                      the graph cannot agree a format",
                            }
                        );
                        serde_json::json!({
                            "phase": "detected",
                            "reason": reason.as_str(),
                            "since_ms": since_ms,
                        })
                    }
                    encoder_stall::StallEvent::Recovered { reason, stalled_ms } => {
                        tracing::info!(
                            session_id = %session_id,
                            "encoder-stall: recovered after {stalled_ms} ms (reason={})",
                            reason.as_str()
                        );
                        serde_json::json!({
                            "phase": "recovered",
                            "reason": reason.as_str(),
                            "stalled_ms": stalled_ms,
                        })
                    }
                };
                emit(SessionEvent::Trace {
                    event: "encoder.stall",
                    payload,
                });
            }
        }

        // `caps.negotiated` after a live renegotiation. The completion probe (a streaming
        // thread) parked only a `&'static str`; all graph reading must happen here on the
        // 100 ms tick, never behind a pad probe (#270).
        if let Some(trigger) = resolution_lever.renegotiation().take() {
            emit(SessionEvent::Trace {
                event: "caps.negotiated",
                payload: caps_negotiated_payload(
                    trigger,
                    &encode_pipe,
                    encode_size,
                    cfg.stream.fps,
                ),
            });
        }

        // Arm anything the agent loop admitted, then advance the in-flight capture. Both
        // must run on THIS thread, never on a streaming thread or behind a pad probe
        // (#270).
        while let Ok(req) = capture_rx.try_recv() {
            if let Err(why) = capture.arm(req, &session_metrics) {
                // Only reachable if the slot reservation and the runner disagree. Fail
                // open: warn and carry on, never fail the session over observability.
                tracing::warn!(
                    token = "capture-arm-failed",
                    session_id = %session_id,
                    "session_capture could not be armed: {}",
                    why.as_str()
                );
            }
        }
        if capture.is_active() {
            let ctx = CaptureCtx {
                encode_pipe: &encode_pipe,
                encoder: capture_encoder.as_ref(),
                metrics: &session_metrics,
                stage: capture_stage_snapshot(
                    &resolution_lever,
                    encode_size,
                    current_render,
                    Some(current_ui_scale),
                ),
            };
            if let Some(report) = capture.poll(&ctx) {
                match report.error {
                    None => tracing::info!(
                        session_id = %session_id,
                        capture_id = %report.capture_id,
                        kind = report.kind,
                        bytes = report.compressed_bytes,
                        duration_ms = report.duration_ms,
                        "session_capture complete"
                    ),
                    Some(err) => tracing::warn!(
                        token = "capture-finished-with-error",
                        session_id = %session_id,
                        capture_id = %report.capture_id,
                        kind = report.kind,
                        bytes = report.compressed_bytes,
                        duration_ms = report.duration_ms,
                        "session_capture finished with error: {err}"
                    ),
                }
                emit(SessionEvent::Capture {
                    event: report.event,
                    payload: report.payload,
                });
            }
        }

        // Inbound relay signaling (answer/ICE from the browser).
        while let Ok(msg) = sig_in_rx.try_recv() {
            if let Err(e) = server::apply_inbound_as_offerer(
                msg,
                &webrtc,
                audio_webrtc.as_ref(),
                &sig_tx,
                Some(&srd_fail_tx),
                Some(&srd_ok_tx),
            ) {
                tracing::warn!(
                    token = "signaling-apply-failed",
                    "apply inbound signaling: {e:#}"
                );
            }
        }

        // `sdp.answer_applied`: what the peer agreed to, per applied answer. Parsing runs
        // on the supervision tick; the promise's change-func only handed over the text.
        while let Ok(applied) = srd_ok_rx.try_recv() {
            let pc = applied.pc.to_string();
            let answer = crate::session::sdp_answer::parse_answer(&applied.sdp);
            let rejected = answer.rejected_count();
            let ufrag_changed = answer.ice_ufrag.as_ref().map(|u| {
                let changed = last_ice_ufrag.get(&pc).is_some_and(|prev| prev != u);
                last_ice_ufrag.insert(pc.clone(), u.clone());
                changed
            });
            let m_lines: Vec<serde_json::Value> = answer
                .m_lines
                .iter()
                .map(|m| {
                    serde_json::json!({
                        "mid": m.mid,
                        "kind": m.kind,
                        "codec": m.codec,
                        "port": m.port,
                        "direction": m.direction,
                        "rejected": m.rejected,
                    })
                })
                .collect();
            let mut payload = serde_json::json!({
                "pc": pc,
                "m_lines": m_lines,
                "rejected_count": rejected,
            });
            if let Some(changed) = ufrag_changed {
                payload["ice_ufrag_changed"] = serde_json::Value::Bool(changed);
            }
            if rejected > 0 {
                // Names the codec: "an m-line was rejected" is the symptom, "the peer will
                // not take h265" is the cause.
                tracing::warn!(
                    token = "sdp-mline-rejected",
                    session_id = %session_id,
                    pc = %pc,
                    "sdp-mline-rejected: the peer ACCEPTED the answer but refused {rejected}                      m-line(s) [{}] — that media will never flow on this PeerConnection.                      A headless/Linux Chrome peer always refuses h265 (hardware-decode                      gated, see docs/testing-bench-mode.md); this is not an encoder stall.",
                    answer.rejected_codecs().join(", ")
                );
            }
            emit(SessionEvent::Trace {
                event: "sdp.answer_applied",
                payload,
            });
        }

        // #503: fail fast on a REJECTED remote description on either PC; that
        // PeerConnection will never carry media and waiting out the timeout hides the
        // cause. A DUPLICATE answer is not terminal: it is normal on the ICE-restart path
        // (the control plane replays the buffered offer to a reconnecting client), and
        // failing on it killed every `qses matrix` reconnect. Drained in a loop, not once
        // per tick, so a duplicate cannot queue the second PC's event behind it.
        while let Ok(failure) = srd_fail_rx.try_recv() {
            // Emit the trace event BEFORE any terminal state: the control plane drops an
            // `agent_trace_event` for a session no longer running on this host. The lanes
            // differ, so this ordering is only real via the agent loop's pre-terminal
            // flush (`agent::flush_pending_diagnostics`); do not reorder without reading
            // it.
            diagnostic_tx.try_emit(
                session_id.clone(),
                TraceEvent {
                    ts_unix_ms: now_unix_ms(),
                    event: "webrtc.remote_description_failed",
                    payload: serde_json::json!({
                        "pc": failure.pc.to_string(),
                        "reason": failure.reason,
                        "kind": failure.kind.as_str(),
                        "benign": failure.kind.is_benign(),
                    }),
                },
            );
            if failure.kind.is_benign() {
                tracing::warn!(
                    token = "webrtc-duplicate-answer",
                    session_id = %session_id,
                    pc = %failure.pc,
                    kind = failure.kind.as_str(),
                    "webrtc: ignoring a duplicate remote answer: {}",
                    failure.reason
                );
                continue;
            }
            let reason = pipeline::remote_description_failure_reason(failure.pc, &failure.reason);
            tracing::error!(
                token = "webrtc-set-remote-failed",
                session_id = %session_id,
                pc = %failure.pc,
                "webrtc: set-remote-description failed — failing session: {reason}"
            );
            emit(SessionEvent::Failed(reason));
            current_source.teardown();
            super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
            audio_pipeline.finish();
            return;
        }

        // Swap: bring up the new compositor, stop the outgoing app and wait for it to
        // exit, launch the replacement, wait for it to present, then re-point the encoder
        // and drop the old source. The encode pipeline and the browser's transport stay
        // untouched on the old compositor until the single `listen-to` switch, so they are
        // never starved and never renegotiate. `SwapDone` is not emitted until the new app
        // is on screen (`perform_swap`), which is the client's reveal signal.
        while let Ok(req) = swap_rx.try_recv() {
            gen += 1;
            emit(SessionEvent::Swapping);
            let from_image = cfg.container.as_ref().map(|c| c.image.clone());
            let to_image = req.container.as_ref().map(|c| c.image.clone());
            match perform_swap(
                &cfg,
                &session_id,
                gen,
                &res,
                &interpipesrc,
                &mut current_source,
                req,
                &shared_clock,
                shared_base,
                cuda_ctx.as_ref(),
                va_ctx.as_ref(),
                vulkan_contexts.as_ref(),
                session_metrics.clone(),
                (current_render, current_ui_scale),
                &emit,
            ) {
                Ok(()) => {
                    emit(SessionEvent::SwapDone);
                    // Re-point the local-display listener at the new source's
                    // interpipesink alongside the encoder's (the same `gen`).
                    if let Some(ld) = &_local_display {
                        let sink = ipsink_name(&session_id, gen);
                        ld.interpipesrc.set_property("listen-to", sink.as_str());
                    }
                    let ts = std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .map(|d| d.as_millis() as i64)
                        .unwrap_or(0);
                    let mut payload = serde_json::json!({});
                    if let Some(from) = from_image {
                        payload["from_app"] = serde_json::Value::String(from);
                    }
                    if let Some(to) = to_image {
                        payload["to_app"] = serde_json::Value::String(to);
                    }
                    diagnostic_tx.try_emit(
                        session_id.clone(),
                        TraceEvent {
                            ts_unix_ms: ts,
                            event: "pipeline.source_swapped",
                            payload,
                        },
                    );
                    // The encode pipeline is not rebuilt, but upstream caps can change with
                    // the generation, so re-state what the branch agreed.
                    emit(SessionEvent::Trace {
                        event: "caps.negotiated",
                        payload: caps_negotiated_payload(
                            pipeline::trigger::SOURCE_SWAP,
                            &encode_pipe,
                            encode_size,
                            cfg.stream.fps,
                        ),
                    });
                }
                // A `fatal` failure means the previous app could not be brought back, so
                // the session is a live compositor with nothing in it: end it with the
                // real reason rather than report a rollback that did not happen.
                Err(failure) if failure.fatal => {
                    tracing::error!(
                        token = "runner-swap-fatal-failed",
                        reason = %failure.reason,
                        "swap failed fatally; ending session"
                    );
                    emit(SessionEvent::Failed(failure.reason));
                    current_source.teardown();
                    super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
                    audio_pipeline.finish();
                    return;
                }
                Err(failure) => emit(SessionEvent::SwapRolledBack(failure.reason)),
            }
        }

        // Idle reap: watch the live transport on the encode webrtcbin.
        if let Some(since) = running_since {
            let ice = webrtc
                .property::<gstreamer_webrtc::WebRTCICEConnectionState>("ice-connection-state");
            if ice_is_connected(ice) {
                ever_connected = true;
                unhealthy_since = None;
            } else if ice_is_dead(ice) {
                unhealthy_since.get_or_insert_with(Instant::now);
            } else {
                unhealthy_since = None;
            }
            // A console session's real output is the physical display, so it must NEVER be
            // reaped for a missing/lost WebRTC transport. Disables the reap for any
            // console-enabled session, local-only or dual-output.
            let reap_window = if cfg.console_config.as_ref().is_some_and(|c| c.enabled) {
                Duration::ZERO
            } else {
                cfg.idle_timeout
            };
            // #484: charge the never-connected window from the app-presented instant, not
            // the first offer — a cold app boot is 30–50 s. Appless sessions keep the old
            // clock.
            let running_for = app_presented_at.unwrap_or(since).elapsed();
            let booting = app_boot_started_at.is_some() && app_presented_at.is_none();
            if let Some(reason) = idle_reap_reason(
                reap_window,
                running_for,
                ever_connected,
                unhealthy_since.map(|u| u.elapsed()),
                booting,
            ) {
                tracing::warn!(
                    token = "session-teardown-summary",
                    session_id = %session_id,
                    "session teardown: reason={reason} ever_connected={ever_connected} \
                     running_for={}s app_presented={}",
                    running_for.as_secs(),
                    app_presented_at.is_some()
                );
                emit(SessionEvent::Stopping);
                current_source.teardown();
                super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
                audio_pipeline.finish();
                tracing::error!(
                    token = "runner-idle-reap-failed",
                    reason = %reason,
                    "session failed via idle reap"
                );
                emit(SessionEvent::Failed(reason.to_string()));
                return;
            }
        }

        // Pump the current source's bus (non-blocking): launch its container on the
        // Wayland-socket announcement. A source error fails the session, and so does a
        // renderer-degraded Warning when hardware rendering is required.
        if let Some(sbus) = current_source.bus() {
            while let Some(m) = sbus.pop() {
                current_source.on_bus_message(&m);
                match m.view() {
                    gst::MessageView::Error(err) => {
                        // DEVICE_LOST can also surface on the source (compositor) bus;
                        // classify it the same way so it fails per-session with the
                        // recognizable reason.
                        let debug_text = format!("{:?}", err.debug());
                        let reason = if vulkan_fault::is_device_lost(&format!(
                            "{} {debug_text}",
                            err.error()
                        )) {
                            vulkan_fault::device_lost_reason(&err.error().to_string(), &debug_text)
                        } else {
                            format!("source pipeline error: {} ({:?})", err.error(), err.debug())
                        };
                        tracing::error!(
                            token = "runner-source-bus-error",
                            reason = %reason,
                            "source pipeline bus error"
                        );
                        emit(SessionEvent::Failed(reason));
                        current_source.teardown();
                        super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
                        audio_pipeline.finish();
                        return;
                    }
                    gst::MessageView::Warning(warn) => {
                        if let Some(reason) =
                            renderer_degraded_failure(warn, &mut renderer_degrade_tracker)
                        {
                            tracing::error!(
                                token = "runner-renderer-degraded-failed",
                                reason = %reason,
                                "renderer degraded; failing session"
                            );
                            emit(SessionEvent::Failed(reason));
                            current_source.teardown();
                            super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
                            audio_pipeline.finish();
                            return;
                        }
                    }
                    _ => {}
                }
            }
        }
        // #378: a container-launch failure (e.g. bad image tag) never posts a gst bus
        // Error — the compositor keeps running empty and the session would sit `running`
        // forever. Fail closed like the source-error arm above.
        if let Some(err) = current_source.take_launch_error() {
            tracing::error!(
                token = "runner-container-launch-failed",
                error = %err,
                "container launch failed"
            );
            emit(SessionEvent::Failed(format!(
                "container launch failed: {err}"
            )));
            current_source.teardown();
            super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
            audio_pipeline.finish();
            return;
        }

        // #484 gen-0 app-presented watcher, reusing the swap path's gate verbatim
        // (`gen0_app_presented`). `app_has_presented()` alone will NOT do: #487.
        if let Some(boot_started) = app_boot_started_at {
            if app_presented_at.is_none() {
                // Order matters: commits first, then frames — see `swap_source_ready`.
                let commits = current_source.app_surface_commits();
                let frames = current_source.sink_frames();
                if commits.is_none() && !app_boot_counter_warned {
                    app_boot_counter_warned = true;
                    tracing::warn!(
                        token = "appboot-no-surface-commits",
                        "app boot: this compositor build does not export 'app-surface-commits' — \
                         revealing on the compositor's own first frame instead, which can happen \
                         before the app has drawn (rebuild the image from the Quasar \
                         gst-wayland-display fork)"
                    );
                }
                if gen0_app_presented(true, commits, frames, &mut app_boot_frames_at_first_commit) {
                    let elapsed = boot_started.elapsed();
                    app_presented_at = Some(Instant::now());
                    emit(SessionEvent::AppPresented);
                    tracing::info!(
                        session_id = %session_id,
                        "app boot: app presented its first frame after {} ms \
                         (app_surface_commits={}, sink_frames={frames}, gen={gen})",
                        elapsed.as_millis(),
                        commits.unwrap_or(0)
                    );
                } else if app_boot_watchdog_fires(app_boot_budget, boot_started.elapsed(), false) {
                    // Bounded, so the reaper exemption above can never become an unkillable
                    // session, and the verdict names the app instead of a mute idle reap.
                    let budget_secs = app_boot_budget.map(|b| b.as_secs()).unwrap_or(0);
                    tracing::error!(
                        token = "appboot-never-presented",
                        session_id = %session_id,
                        "app boot: FAILED — no frame within {budget_secs}s; \
                         failing session as {REASON_APP_NEVER_PRESENTED}"
                    );
                    emit(SessionEvent::AppFailed {
                        reason: format!(
                            "app produced no frame within {budget_secs}s of container start"
                        ),
                        reason_code: REASON_APP_NEVER_PRESENTED,
                        app_log_tail: current_source.app_log_tail(),
                    });
                    current_source.teardown();
                    super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
                    audio_pipeline.finish();
                    return;
                } else if app_boot_last_wait_log.elapsed() >= APP_BOOT_WAIT_LOG_INTERVAL {
                    app_boot_last_wait_log = Instant::now();
                    tracing::info!(
                        session_id = %session_id,
                        "app boot: app has not presented after {} ms; still waiting (budget {}s)",
                        boot_started.elapsed().as_millis(),
                        app_boot_budget.map(|b| b.as_secs()).unwrap_or(0)
                    );
                }
            }
        }

        // App-liveness on a 5 s cadence, aligned with the metrics heartbeat.
        if liveness_cadence_at.elapsed() >= Duration::from_secs(5) {
            let cadence_elapsed = liveness_cadence_at.elapsed();
            liveness_cadence_at = Instant::now();
            // The DualOutput console leg's delivered-frame cadence, so the local display
            // is observable in-process: delivered_fps ~0 while the browser stream keeps
            // running is the "monitor black" symptom.
            if let Some(ld) = &_local_display {
                let frames = ld.drain_sink_frames();
                let seconds = cadence_elapsed.as_secs_f64();
                let expected = (cfg.stream.fps as f64 * seconds).round() as u64;
                let missing = expected.saturating_sub(frames);
                tracing::info!(
                    target_fps = cfg.stream.fps,
                    frames,
                    window_ms = cadence_elapsed.as_millis() as u64,
                    delivered_fps = format_args!("{:.2}", frames as f64 / seconds),
                    missing,
                    "local-display cadence"
                );
            }
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
                    session_id.clone(),
                    TraceEvent {
                        ts_unix_ms: ts,
                        event: "app.exited",
                        payload: serde_json::json!({
                            "status": format!("{status:?}"),
                            "policy": format!("{policy:?}"),
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
                        // An unrecognized wire value must degrade fail-closed, never panic
                        // or silently keep (`messages::AppExitPolicy`). `serde(other)`
                        // discards the original string, so it cannot be named here.
                        tracing::warn!(
                            token = "app-exit-disposition-unrecognized",
                            "unrecognized on_app_exit value, treating as fail"
                        );
                        tracing::error!(
                            token = "runner-app-exit-unrecognized-failed",
                            status = ?status,
                            presented,
                            "app container exited; unrecognized disposition treated as fail"
                        );
                        emit(app_exit_event(status, presented, app_log_tail));
                        current_source.teardown();
                        super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
                        audio_pipeline.finish();
                        return;
                    }
                    AppExitPolicy::Fail => {
                        tracing::error!(
                            token = "runner-app-exit-failed",
                            status = ?status,
                            presented,
                            "app container exited; on_app_exit=fail"
                        );
                        emit(app_exit_event(status, presented, app_log_tail));
                        current_source.teardown();
                        super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
                        audio_pipeline.finish();
                        return;
                    }
                }
            }
        }

        // Pump the encode bus (the transport); this paces the loop at ~POLL ms.
        if let Some(m) = encode_bus.timed_pop(gst::ClockTime::from_mseconds(POLL)) {
            match m.view() {
                gst::MessageView::Error(err) => {
                    // The SCTP signature lives in the DEBUG text, not the GError message
                    // (SCTP_ASSOCIATION_SIGNATURES): classify on both, concatenated.
                    let debug_text = format!("{:?}", err.debug());
                    let error_text = err.error().to_string();
                    let classify_text = format!("{error_text} {debug_text}");
                    // Teardown-race guard: after a stop is requested the transport is torn
                    // down and the SCTP association errors, racing the stop-flag check at
                    // the top of the loop. That is a CLEAN teardown, so complete via the
                    // normal Stopping/Stopped path and do not emit Failed. An error on a
                    // session nobody asked to stop is a real failure (below).
                    if stop_requested_for_transport_error(&stop, &classify_text) {
                        tracing::info!(
                            "ignoring encode pipeline error during teardown: {} ({:?})",
                            err.error(),
                            err.debug()
                        );
                        emit(SessionEvent::Stopping);
                        current_source.teardown();
                        super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
                        audio_pipeline.finish();
                        emit(SessionEvent::Stopped {
                            bytes_used: compute_bytes_used(&cfg),
                            detail: None,
                        });
                        return;
                    }
                    // Peer disconnect: the DataChannel's SCTP association died with no stop
                    // requested. The session is over (no input channel, and webrtcbin
                    // cannot re-establish the association without renegotiation) but the
                    // peer went away, not the agent — so end it on the SAME clean teardown
                    // path as an operator stop: `stopped` + PEER_DISCONNECT_DETAIL, NO
                    // error_message. Must stay ordered BEFORE the device-lost check: an
                    // SCTP error is never a GPU fault, and mis-tagging it would feed the
                    // GPU-global fault detector.
                    if is_sctp_association_error(&classify_text) {
                        tracing::warn!(
                            token = "peer-transport-gone",
                            "peer transport gone (SCTP association error); ending session cleanly: \
                             {} ({:?})",
                            err.error(),
                            err.debug()
                        );
                        diagnostic_tx.try_emit(
                            session_id.clone(),
                            TraceEvent {
                                ts_unix_ms: std::time::SystemTime::now()
                                    .duration_since(std::time::UNIX_EPOCH)
                                    .map(|d| d.as_millis() as i64)
                                    .unwrap_or(0),
                                event: "transport.peer_disconnected",
                                payload: serde_json::json!({
                                    "error": error_text,
                                    "debug": debug_text,
                                }),
                            },
                        );
                        emit(SessionEvent::Stopping);
                        current_source.teardown();
                        super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
                        audio_pipeline.finish();
                        emit(SessionEvent::Stopped {
                            bytes_used: compute_bytes_used(&cfg),
                            detail: Some(PEER_DISCONNECT_DETAIL),
                        });
                        return;
                    }
                    // A VK_ERROR_DEVICE_LOST on the encode bus fails only THIS session
                    // closed; dropping its element retires that per-element device and the
                    // other sessions keep theirs. The agent-side detector escalates to a
                    // GPU-global drain+restart only if 2 or more sessions report it inside
                    // the window.
                    let reason = if vulkan_fault::is_device_lost(&classify_text) {
                        vulkan_fault::device_lost_reason(&error_text, &debug_text)
                    } else {
                        format!("encode pipeline error: {} ({:?})", err.error(), err.debug())
                    };
                    tracing::error!(
                        token = "runner-encode-bus-error",
                        reason = %reason,
                        "encode pipeline bus error"
                    );
                    emit(SessionEvent::Failed(reason));
                    current_source.teardown();
                    super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
                    audio_pipeline.finish();
                    return;
                }
                // `encoder.stall` reason discriminant only; this arm changes no behaviour.
                // A caps/not-negotiated WARNING is not fatal (the graph may recover on the
                // next buffer) but separates "the encoder is slow" from "the graph cannot
                // agree a format".
                gst::MessageView::Warning(warn) => {
                    if is_negotiation_message(&format!("{} {:?}", warn.error(), warn.debug())) {
                        negotiation_seen_at = Some(Instant::now());
                    }
                }
                gst::MessageView::Eos(_) => {
                    current_source.teardown();
                    super::nvenc_defer::finish_encode(&encode_pipe, defer_encode_teardown);
                    audio_pipeline.finish();
                    emit(SessionEvent::Stopped {
                        bytes_used: compute_bytes_used(&cfg),
                        detail: None,
                    });
                    return;
                }
                _ => {}
            }
        }
    }
}

#[allow(clippy::too_many_arguments)]
fn run_local_only<F: Fn(SessionEvent)>(
    session_id: &str,
    cfg: &SessionConfig,
    emit: &F,
    diagnostic_tx: DiagnosticEventTx,
    stop: Arc<AtomicBool>,
    swap_rx: std::sync::mpsc::Receiver<SwapRequest>,
    display_rx: std::sync::mpsc::Receiver<DisplayUpdateRequest>,
    res: &SessionResources,
    current_source: &mut AppSource,
    gen: &mut u64,
    session_metrics: Arc<SessionMetrics>,
    sink0: &str,
    shared_clock: &gst::Clock,
    shared_base: gst::ClockTime,
    cuda_ctx: Option<&gst::Context>,
    va_ctx: Option<&gst::Context>,
    vulkan_contexts: Option<&VulkanContextBridge>,
    local_backend: console::LocalBackend,
    prestarted_weston: Option<console::WestonConsole>,
) {
    let Some(cc) = cfg.console_config.as_ref().filter(|c| c.enabled) else {
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
            match console::spawn_weston_console(session_id, cfg.console_config.as_ref()) {
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
    let socket = weston.as_ref().map(|weston| weston.socket.as_str());
    let local_display = match pipeline::build_local_display_pipeline(cfg, sink0, socket) {
        Ok(ld) => ld,
        Err(e) => {
            tracing::error!(
                token = "runner-local-display-build-failed",
                error = %format_args!("{e:#}"),
                "build local-display pipeline failed"
            );
            emit(SessionEvent::Failed(format!(
                "build local-display pipeline: {e:#}"
            )));
            current_source.teardown();
            return;
        }
    };
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
        super::va_share::install_need_context_handler(&local_display.pipeline, ctx);
    }
    if let Some(ctx) = cuda_ctx {
        local_display.pipeline.set_context(ctx);
    }
    if let Err(e) = local_display.pipeline.set_state(gst::State::Playing) {
        tracing::error!(
            token = "runner-local-display-playing-failed",
            error = %format_args!("{e:#}"),
            "local-display pipeline PLAYING transition failed"
        );
        emit(SessionEvent::Failed(format!(
            "local-display set PLAYING: {e:#}"
        )));
        current_source.teardown();
        return;
    }

    let local_audio = cc.audio_output.as_deref().and_then(|output| {
        match pipeline::build_local_audio_pipeline(cfg, output) {
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
            super::physical_input::PhysicalInput::start(
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

    let display_bus = local_display.pipeline.bus();
    // session-display-update state — see the matching declarations in run_blocking.
    let encode_size = (cfg.stream.width, cfg.stream.height);
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
                bytes_used: compute_bytes_used(cfg),
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

        while let Ok(req) = swap_rx.try_recv() {
            *gen += 1;
            emit(SessionEvent::Swapping);
            match perform_swap(
                cfg,
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
            let expected = (cfg.stream.fps as f64 * seconds).round() as u64;
            let missing = expected.saturating_sub(frames);
            tracing::info!(
                target_fps = cfg.stream.fps,
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

/// Bytes used by the session's local-driver home dirs after the container is torn down,
/// against the session's effective `home_root` (#377). `None` when that root is
/// empty/invalid, and the caller then omits `bytes_used`.
fn compute_bytes_used(cfg: &SessionConfig) -> Option<u64> {
    let mounts = cfg
        .container
        .as_ref()
        .map(|c| c.mounts.as_slice())
        .unwrap_or(&[]);
    super::home::measure_home_dirs(mounts, &cfg.home_root)
}

/// The `SessionEvent::Failed` message for a `fail`-policy app-container exit. Pure so the
/// classification is unit tested.
fn app_exit_failure_reason(status: AppExitStatus) -> String {
    match status {
        AppExitStatus::OomKilled => "app container OOM-killed".to_string(),
        AppExitStatus::Code(0) => "app exited (0)".to_string(),
        AppExitStatus::Code(n) => format!("app exited with code {n}"),
        AppExitStatus::Unknown => "app exited (status unavailable)".to_string(),
    }
}

/// Reason code: the app container exited before it ever presented a frame. Stable wire
/// value; the admin UI and the client key off it.
pub const REASON_APP_EXITED_EARLY: &str = "app_exited_early";

/// #484 reason code: the app container is alive but never presented a frame within
/// `QUASAR_APP_BOOT_TIMEOUT_SECS`. Same `AppFailed` shape as
/// [`REASON_APP_EXITED_EARLY`]; this case used to surface as a mute
/// `idle: WebRTC transport never established` reap.
pub const REASON_APP_NEVER_PRESENTED: &str = "app_never_presented";

/// The terminal event for a `fail`-policy app-container exit.
///
/// `presented` is [`AppSource::app_has_presented`]: whether the APP, not the compositor,
/// ever drew. When it did not, the session never had a picture and the real cause lives in
/// a container the daemon has already reaped (#463), so that case gets a reason code,
/// prose saying what happened, and the log tail. An app that exits after streaming keeps
/// its existing wording.
///
/// Pure, so the classification is unit tested.
fn app_exit_event(status: AppExitStatus, presented: bool, log_tail: Vec<String>) -> SessionEvent {
    if presented {
        return SessionEvent::Failed(app_exit_failure_reason(status));
    }
    let what = match status {
        AppExitStatus::OomKilled => "was OOM-killed".to_string(),
        AppExitStatus::Code(n) => format!("exited with code {n}"),
        AppExitStatus::Unknown => "exited (status unavailable)".to_string(),
    };
    let hint = if log_tail.is_empty() {
        " The app container produced no log output.".to_string()
    } else {
        String::new()
    };
    SessionEvent::AppFailed {
        reason: format!(
            "the app {what} before producing any video. Check the app container log.{hint}"
        ),
        reason_code: REASON_APP_EXITED_EARLY,
        app_log_tail: log_tail,
    }
}

/// The interpipe node name a source generation publishes (and the encoder listens
/// to). Unique per session + generation across the whole agent process.
fn ipsink_name(session_id: &str, gen: u64) -> String {
    format!("quasar-ipsink-{session_id}-{gen}")
}

/// The app-container name for a source generation. Per-generation so old and new can run
/// at once during a swap; the `quasar-sess-` prefix keeps it within the orphan-sweep's
/// reach.
fn app_container_name(session_id: &str, gen: u64) -> String {
    format!("quasar-sess-{session_id}-g{gen}")
}

/// Perform one serialised source swap. Step order is load-bearing:
///  1. Build + start the new generation's compositor and wait for its Wayland socket. The
///     outgoing app is untouched, so any failure up to here is a free rollback.
///  2. Stop the outgoing app container and WAIT for it to exit. Mandatory, not an
///     optimisation: both generations bind-mount the same managed home, and a second
///     instance of a home-locking app (Steam) finds the first's lock, hands off and exits
///     0 ~1.2 s later, which an overlapping design surfaced as `app exited (0)` right
///     after `swap complete`. The outgoing COMPOSITOR keeps running, so the encoder is fed
///     real frames for the whole gap: PTS stay continuous, the encoder never starves and
///     the transport never sees a dead media path.
///  3. Launch the replacement app container.
///  4. Wait until it has actually PRESENTED: an app-surface commit followed by a
///     compositor frame (`swap_source_ready`), never the compositor's own first frame,
///     which fires milliseconds after PLAYING with an empty scene and caused the black
///     screen. `SwapDone` must not be emitted before this succeeds.
///  5. Re-point the encoder and adopt the new generation.
///
/// `Err(SwapFailure)` is a rollback; `fatal` separates "still running the previous app"
/// from "the previous app could not be brought back". The encode pipeline, `webrtcbin`,
/// the transport and the DataChannel are never touched in any outcome.
#[allow(clippy::too_many_arguments)]
fn perform_swap(
    cfg: &SessionConfig,
    session_id: &str,
    gen: u64,
    res: &SessionResources,
    interpipesrc: &gst::Element,
    current_source: &mut AppSource,
    req: SwapRequest,
    shared_clock: &gst::Clock,
    shared_base: gst::ClockTime,
    cuda_ctx: Option<&gst::Context>,
    va_ctx: Option<&gst::Context>,
    vulkan_contexts: Option<&VulkanContextBridge>,
    session_metrics: Arc<SessionMetrics>,
    // The live render size (`None` is the pinned encode size) and UI scale, pushed onto
    // the new compositor BEFORE it boots; see the apply site below.
    display: (Option<(i32, i32)>, f64),
    emit: &dyn Fn(SessionEvent),
) -> Result<(), SwapFailure> {
    let sink = ipsink_name(session_id, gen);
    let cname = app_container_name(session_id, gen);
    let mut new_source = AppSource::new(cfg, session_id, &sink, &cname, res, req.container)
        .map_err(|e| SwapFailure::rolled_back(format!("build new source: {e:#}")))?;
    // Hold the replacement container back until the outgoing one has exited (step 2).
    // Without this the launch fires straight off the Wayland announcement, ~90 ms into the
    // swap, with the old app still holding the shared managed home.
    new_source.defer_app_launch();
    // Same shared clock+base as every other source so PTS stay continuous across the swap
    // (#68) and the step-5 `listen-to` re-point introduces no PTS jump.
    new_source.apply_shared_clock(shared_clock, shared_base);
    // The new compositor MUST adopt the SAME shared CUDA/VA contexts before it starts, or
    // its surfaces are not valid in the live encoder. No-op when None.
    if let Some(ctx) = cuda_ctx {
        new_source.apply_cuda_context(ctx);
    }
    if let Some(ctx) = va_ctx {
        new_source.apply_va_context(ctx);
    }
    if let Some(contexts) = vulkan_contexts {
        contexts.install_on_source(&new_source);
    }
    // Push the live render size / UI scale onto the new generation BEFORE it starts.
    // Applying it after the swap would boot the replacement at the encode-size
    // `wl_output` mode and only then resize, and an app that reads `wl_output.mode` once
    // at startup would keep the wrong size forever. Skipped at the defaults.
    let (swap_render, swap_ui_scale) = display;
    if swap_render.is_some() || swap_ui_scale != 1.0 {
        push_display_to_source(&new_source, swap_render, swap_ui_scale, &session_metrics);
    }
    // The mode ladder is per-generation state on the compositor element, so the
    // replacement needs it too, with the same pre-boot reasoning as the render size.
    // Derived from the LAUNCH size, so it is identical for every generation and unaffected
    // by the current external size.
    new_source.set_mode_ladder(&super::rungs::available_rungs(
        cfg.stream.width,
        cfg.stream.height,
    ));
    new_source
        .start()
        .map_err(|e| SwapFailure::rolled_back(format!("start new source: {e:#}")))?;

    // ── Step 1: the new compositor comes up ──────────────────────────────────
    // The outgoing app is still on screen, so every early return below is a free rollback.
    //
    // Benign transient, do not chase as a bug: the two compositors overlap here, so the
    // new one may log `OtherEGLDisplayAlreadyBound` and one CUDA `Failed to acquire buffer
    // from pool: -2` while its buffer pool warms up. It self-recovers.
    let expects_app = new_source.expects_app_container();
    let deadline = Instant::now() + SWAP_COMPOSITOR_TIMEOUT;
    let new_bus = new_source.bus();
    loop {
        pump_new_source_bus(&new_bus, &mut new_source).map_err(SwapFailure::rolled_back)?;
        // A generation with no app container never announces a launch target; its
        // readiness is the compositor's own frame, checked in step 4.
        if !expects_app || new_source.compositor_socket_ready() {
            break;
        }
        if Instant::now() >= deadline {
            return Err(SwapFailure::rolled_back(
                "new compositor did not announce a Wayland socket within timeout",
            ));
        }
        std::thread::sleep(SWAP_POLL);
    }

    if let Some(contexts) = vulkan_contexts {
        contexts.assert_source(&new_source).map_err(|e| {
            SwapFailure::rolled_back(format!("verify replacement Vulkan context identity: {e:#}"))
        })?;
    }

    // ── Step 2: stop the outgoing app and WAIT for it to exit ────────────────
    // Past this line a failure costs the previous app's process state, so everything
    // cheaply validatable must already have been validated above.
    let stopped_previous_app = current_source.stop_app_container();
    if stopped_previous_app {
        tracing::info!(
            "swap: previous app container stopped and reaped (gen {} -> {gen}); its compositor \
             keeps feeding the encoder while the replacement starts",
            gen.saturating_sub(1)
        );
    }

    // ── Step 3: launch the replacement app ───────────────────────────────────
    new_source.launch_deferred_app();

    // ── Step 4: wait until the replacement app has actually PRESENTED ────────
    if expects_app && new_source.app_surface_commits().is_none() {
        tracing::warn!(
            token = "swap-no-surface-commits",
            "swap: this compositor build does not export 'app-surface-commits' — falling back \
             to the compositor's own first frame, which can re-point the encoder before the \
             app has drawn (black-screen risk; rebuild the image from the Quasar \
             gst-wayland-display fork)"
        );
    }
    // #484: the swap's app-boot window speaks the same vocabulary as a launch, so a client
    // implements one reveal rule for both. `SwapDone`'s `"swap complete"` string must stay
    // unchanged; these are additive details around it.
    if expects_app {
        emit(SessionEvent::AppBooting);
    }
    let budget = swap_app_ready_timeout();
    let deadline = Instant::now() + budget;
    let mut frames_at_first_commit: Option<u64> = None;
    let ready = loop {
        if let Err(e) = pump_new_source_bus(&new_bus, &mut new_source) {
            break Err(e);
        }
        // A container-launch failure never posts a gst bus Error, so without this check
        // the swap sits out the whole budget below.
        if let Some(err) = new_source.take_launch_error() {
            break Err(format!("new source container launch failed: {err}"));
        }
        // Order matters: commits first, then frames — see `swap_source_ready`.
        let commits = new_source.app_surface_commits();
        let frames = new_source.sink_frames();
        if swap_source_ready(expects_app, commits, frames, &mut frames_at_first_commit) {
            tracing::info!(
                app_surface_commits = commits.unwrap_or(0),
                sink_frames = frames,
                "swap: replacement app has presented its first frame"
            );
            if expects_app {
                emit(SessionEvent::AppPresented);
            }
            break Ok(());
        }
        if Instant::now() >= deadline {
            break Err(format!(
                "replacement app produced no frame within {budget:?} \
                 (app_surface_commits={commits:?})"
            ));
        }
        std::thread::sleep(SWAP_POLL);
    };
    if let Err(reason) = ready {
        // Discard the half-built generation first (Drop removes the replacement container
        // and NULLs its compositor), so the rollback relaunch below cannot race it for the
        // shared managed home.
        drop(new_source);
        return Err(roll_back_stopped_app(
            current_source,
            res,
            stopped_previous_app,
            reason,
        ));
    }

    // ── Step 5: re-point the encoder ─────────────────────────────────────────
    // Release held inputs so a key held in the launcher does not bleed into the incoming
    // game. Must stay HERE rather than beside the container stop in step 2: the new
    // compositor is live and receiving input by now and needs to observe the release
    // events itself.
    if let Some(d) = res.devices.as_ref() {
        if let Err(e) = d.release_all() {
            tracing::warn!(
                token = "swap-release-all-failed",
                "perform_swap: release_all failed: {e:#}"
            );
        }
    }

    // Re-arm the controller-first pointer nudge for the incoming app process. The "input"
    // DataChannel and its InputState persist across the swap, so without this reset a swap
    // into a fresh Steam Big Picture container inherits `nudge_sent=true` (heal spent) and
    // a stale `mouse_seen=true`, both of which block the heal the new BPM needs.
    res.input_state.reset();

    // The single operation that makes the swap visible: re-point the live encoder
    // at the new source. No element restarts; the transport is undisturbed.
    current_source.detach_compositor_metrics();
    interpipesrc.set_property("listen-to", sink.as_str());
    new_source.attach_compositor_metrics(session_metrics, cfg.latency_probe);
    tracing::info!("swap: encoder now listening to '{sink}' (gen {gen})");

    // The previous AppSource drops here, tearing down its container + source pipeline.
    // That drop runs `AppSource::teardown` -> `RunningContainer::stop`, which sets the old
    // generation's `removed` flag BEFORE the `docker stop`, so its waiter thread discards
    // whatever exit it observes and it is never misclassified as an app failure. Its
    // `exit_result` slot drops with it, so the caller's `take_container_exit` poll only
    // ever sees the new generation. The old app container is already gone (step 2), so
    // this drop only NULLs its compositor and runs the idempotent `force_remove` backstop.
    *current_source = new_source;
    Ok(())
}

/// Drain the new generation's bus into its [`AppSource`], turning a pipeline
/// error into the swap's rollback reason.
fn pump_new_source_bus(bus: &Option<gst::Bus>, source: &mut AppSource) -> Result<(), String> {
    let Some(b) = bus else {
        return Ok(());
    };
    while let Some(m) = b.pop() {
        source.on_bus_message(&m);
        if let gst::MessageView::Error(err) = m.view() {
            return Err(format!("new source error: {}", err.error()));
        }
    }
    Ok(())
}

/// Undo step 2 of a failed serialised swap: put the previous app back into its still-live
/// compositor.
///
/// The previous app RESTARTS: its process state is gone, because stopping it is what
/// releases the shared managed home. The session itself survives — compositor, encode
/// pipeline, `webrtcbin`, transport and DataChannel are untouched.
///
/// A failed relaunch leaves a live compositor with no app in it, reported as `fatal` so
/// the caller ends the session accurately instead of claiming a rollback.
fn roll_back_stopped_app(
    current_source: &mut AppSource,
    res: &SessionResources,
    stopped_previous_app: bool,
    reason: String,
) -> SwapFailure {
    if !stopped_previous_app {
        return SwapFailure::rolled_back(reason);
    }
    // The previous app becomes a fresh process too, so it gets the same input hygiene the
    // forward path gives the incoming app.
    if let Some(d) = res.devices.as_ref() {
        if let Err(e) = d.release_all() {
            tracing::warn!(
                token = "swap-rollback-release-all-failed",
                "swap rollback: release_all failed: {e:#}"
            );
        }
    }
    res.input_state.reset();
    match current_source.relaunch_app() {
        Ok(()) => {
            tracing::warn!(
                token = "swap-failed-rolled-back",
                "swap failed ({reason}); previous app relaunched"
            );
            SwapFailure::rolled_back(format!("{reason}; previous app was restarted"))
        }
        Err(e) => {
            tracing::error!(
                token = "swap-failed-unrecoverable",
                "swap failed ({reason}) and the previous app could not be relaunched: {e}"
            );
            SwapFailure {
                reason: format!("{reason}; previous app could not be relaunched: {e}"),
                fatal: true,
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{
        app_boot_watchdog_fires, app_exit_event, audio_path_label, context_object_identity,
        effective_encoder_device_path, fold_display_update, gen0_app_presented, idle_reap_reason,
        is_sctp_association_error, mic_state_label, poll_display_bus,
        stop_requested_for_transport_error, swap_source_ready, validate_display_update,
        AppExitStatus, AudioPipelineGuard, DiagnosticEventTx, DisplayUpdateRequest, SessionEvent,
        TraceEvent, VulkanContextBridge, POLL, REASON_APP_EXITED_EARLY, VULKAN_DEVICE_CONTEXT,
    };
    use gstreamer::prelude::*;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::Arc;
    use std::time::Duration;

    const WINDOW: Duration = Duration::from_secs(120);

    // ── session-display-update: validation (agent-api.md) ────────────────────
    // Validation runs synchronously in the agent loop so a bad number is acked
    // {ok:false, "display_update_rejected: …"} and the session is untouched.

    use super::{apply_stream_update, SessionDisplayState};
    use crate::session::metrics::SessionMetrics;
    use crate::session::pipeline;
    use gstreamer as gst;

    fn du(w: Option<i32>, h: Option<i32>, s: Option<f64>) -> DisplayUpdateRequest {
        DisplayUpdateRequest {
            render_width: w,
            render_height: h,
            ui_scale: s,
            stream: None,
        }
    }

    /// A display update carrying only the external (stream) half.
    fn du_stream(w: i32, h: i32) -> DisplayUpdateRequest {
        DisplayUpdateRequest {
            stream: Some((w, h)),
            ..du(None, None, None)
        }
    }

    /// A 1080p session on an encode path that CAN resize live, at its launch size.
    fn state_1080p() -> SessionDisplayState {
        SessionDisplayState::new((1920, 1080), true)
    }

    /// Validate + fold as the agent loop does, returning the effective request handed to
    /// the runner.
    fn accept(st: &mut SessionDisplayState, req: DisplayUpdateRequest) -> DisplayUpdateRequest {
        let eff = validate_display_update(&req, st).expect("expected an accepted update");
        st.apply(&eff);
        eff
    }

    #[test]
    fn display_update_accepts_a_lowered_even_render_size() {
        assert!(validate_display_update(&du(Some(1280), Some(720), None), &state_1080p()).is_ok());
        // Equal to the pinned stream size is allowed (the default), too.
        assert!(validate_display_update(&du(Some(1920), Some(1080), None), &state_1080p()).is_ok());
    }

    #[test]
    fn display_update_rejects_out_of_range_odd_and_half_pairs() {
        // Above the LAUNCH size; the external size has no say here.
        let err = validate_display_update(&du(Some(1921), Some(1080), None), &state_1080p())
            .expect_err("above launch width");
        assert!(err.contains("render_width"), "{err}");
        // Below the 16px floor.
        assert!(validate_display_update(&du(Some(15), Some(15), None), &state_1080p()).is_err());
        // Odd (and hence not encodable-aligned).
        let err = validate_display_update(&du(Some(1281), Some(720), None), &state_1080p())
            .expect_err("odd width");
        assert!(err.contains("even"), "{err}");
        // Width without height.
        assert!(validate_display_update(&du(Some(1280), None, None), &state_1080p()).is_err());
        // Height without width.
        assert!(validate_display_update(&du(None, Some(720), None), &state_1080p()).is_err());
        // Nothing at all to do.
        assert!(validate_display_update(&du(None, None, None), &state_1080p()).is_err());
    }

    #[test]
    fn display_update_folds_partially_and_collapses_an_encode_sized_render_to_default() {
        let encode = (1920, 1080);
        let mut render = None;
        let mut scale = 1.0;

        // Size only: scale untouched.
        fold_display_update(
            &du(Some(1280), Some(720), None),
            &mut render,
            &mut scale,
            encode,
        );
        assert_eq!(render, Some((1280, 720)));
        assert_eq!(scale, 1.0);

        // Scale only: size untouched.
        fold_display_update(&du(None, None, Some(1.5)), &mut render, &mut scale, encode);
        assert_eq!(render, Some((1280, 720)));
        assert_eq!(scale, 1.5);

        // Back to the pinned encode size folds to `None`, stopping the metrics echo.
        fold_display_update(
            &du(Some(1920), Some(1080), None),
            &mut render,
            &mut scale,
            encode,
        );
        assert_eq!(render, None);
        assert_eq!(scale, 1.5);
    }

    #[test]
    fn display_update_bounds_ui_scale() {
        assert!(validate_display_update(&du(None, None, Some(3.0)), &state_1080p()).is_ok());
        assert!(validate_display_update(&du(None, None, Some(1.0)), &state_1080p()).is_ok());
        let err = validate_display_update(&du(None, None, Some(0.5)), &state_1080p())
            .expect_err("below 1.0");
        assert!(err.contains("ui_scale"), "{err}");
        assert!(validate_display_update(&du(None, None, Some(3.5)), &state_1080p()).is_err());
        assert!(validate_display_update(&du(None, None, Some(f64::NAN)), &state_1080p()).is_err());
    }

    // ── adaptive external resolution: the stream half of the same validation ───

    #[test]
    fn stream_update_accepts_a_rung_and_rejects_anything_else() {
        let st = state_1080p();
        // Every rung of a 1080p session's 16:9 family, including the launch size.
        for (w, h) in [(1920, 1080), (1600, 900), (1280, 720)] {
            let eff = validate_display_update(&du_stream(w, h), &st)
                .unwrap_or_else(|e| panic!("{w}x{h} should be a rung: {e}"));
            assert_eq!(eff.stream, Some((w, h)));
            // The stream half never touches the render axis, at any rung.
            assert_eq!(
                (eff.render_width, eff.render_height),
                (None, None),
                "{w}x{h}"
            );
        }
        // Above the launch size, off the ladder, and another family's rung.
        for (w, h) in [(2560, 1440), (1366, 768), (1440, 900)] {
            let err =
                validate_display_update(&du_stream(w, h), &st).expect_err("{w}x{h} is not a rung");
            assert!(err.contains("not a rung"), "{err}");
        }
    }

    // A session with no lever must be told NO, never silently left at its launch size.
    #[test]
    fn stream_update_is_rejected_when_the_encode_path_cannot_resize() {
        let st = SessionDisplayState::new((1920, 1080), false);
        let err = validate_display_update(&du_stream(1280, 720), &st).expect_err("unsupported");
        assert_eq!(err, "encoder does not support live resize");
        // …but the render/scale half of the same session still works.
        assert!(validate_display_update(&du(Some(1280), Some(720), None), &st).is_ok());
    }

    // Render is validated against the LAUNCH size, not the external size: a render size
    // above external is legal and resolved by the encode-side downsample.
    #[test]
    fn render_is_checked_against_the_launch_size_not_the_external_size() {
        let st = state_1080p();
        let both = DisplayUpdateRequest {
            render_width: Some(1600),
            render_height: Some(900),
            ui_scale: None,
            stream: Some((1280, 720)),
        };
        let eff = validate_display_update(&both, &st).expect("render above external is legal");
        assert_eq!(
            (eff.render_width, eff.render_height),
            (Some(1600), Some(900))
        );
        assert_eq!(eff.stream, Some((1280, 720)));

        // Above the LAUNCH size is still a rejection, and the message says so.
        let err = validate_display_update(&du(Some(1922), Some(1080), None), &st)
            .expect_err("above launch");
        assert!(err.contains("above the session launch size 1920"), "{err}");
    }

    // A stream step DOWN below an explicit render override leaves the render size where it
    // is and carries no render fields: the compositor is never told anything.
    #[test]
    fn a_stream_step_down_leaves_an_explicit_render_size_untouched() {
        let mut st = state_1080p();
        let eff = accept(&mut st, du(Some(1600), Some(900), None));
        assert_eq!(eff.render_width, Some(1600));
        assert_eq!(st.render, Some((1600, 900)));

        let eff = accept(&mut st, du_stream(1280, 720));
        assert_eq!(eff.stream, Some((1280, 720)));
        assert_eq!(
            (eff.render_width, eff.render_height),
            (None, None),
            "an external step must synthesise no render fields"
        );
        assert_eq!(st.external, (1280, 720));
        assert_eq!(
            st.render,
            Some((1600, 900)),
            "the render size is independent of the external size"
        );
        assert!(
            st.render.unwrap() > st.external,
            "render ABOVE external is the whole point — the encoder downsamples"
        );
    }

    // `render == None` stays `None` across a step down: nothing is synthesised, so the
    // compositor keeps rendering at its launch-size framebuffer.
    #[test]
    fn a_stream_step_down_leaves_a_default_render_as_none() {
        let mut st = state_1080p();
        assert_eq!(st.render, None, "no explicit override");

        let eff = accept(&mut st, du_stream(1280, 720));
        assert_eq!((eff.render_width, eff.render_height), (None, None));
        assert_eq!(st.render, None, "still the session default");
        assert_eq!(st.external, (1280, 720));
    }

    // Stepping the external size back UP changes nothing on the render axis: no clamp, so
    // nothing to release.
    #[test]
    fn a_stream_step_up_changes_nothing_on_the_render_axis() {
        let mut st = state_1080p();
        accept(&mut st, du(Some(1280), Some(720), None));
        accept(&mut st, du_stream(1280, 720));
        assert_eq!(st.render, Some((1280, 720)));

        for (w, h) in [(1600, 900), (1920, 1080)] {
            let eff = accept(&mut st, du_stream(w, h));
            assert_eq!(
                (eff.render_width, eff.render_height),
                (None, None),
                "step up to {w}x{h} must not rewrite the render size"
            );
            assert_eq!(st.external, (w, h));
            assert_eq!(st.render, Some((1280, 720)), "unchanged at {w}x{h}");
        }
    }

    // The render axis's bounds are the launch size: above it is rejected, equal to it folds
    // to `None` (the compositor's "0x0" reset, stopping the metrics echo), anything between
    // is accepted, including a size above the current external size.
    #[test]
    fn render_bounds_are_the_launch_size_while_the_external_size_is_lower() {
        let mut st = state_1080p();
        accept(&mut st, du_stream(1280, 720));
        assert_eq!(st.external, (1280, 720));

        // Above launch ⇒ rejected.
        let err = validate_display_update(&du(Some(1920), Some(1082), None), &st)
            .expect_err("above launch height");
        assert!(err.contains("above the session launch size 1080"), "{err}");

        // Between the external size and the launch size ⇒ accepted, and kept.
        let eff = accept(&mut st, du(Some(1600), Some(900), None));
        assert_eq!(
            (eff.render_width, eff.render_height),
            (Some(1600), Some(900))
        );
        assert_eq!(st.render, Some((1600, 900)));
        assert_eq!(st.external, (1280, 720), "the external size did not move");

        // Exactly the launch size ⇒ folds to the default.
        accept(&mut st, du(Some(1920), Some(1080), None));
        assert_eq!(st.render, None);
        assert_eq!(st.external, (1280, 720));
    }

    // The agent's copy of the state must track what the runner will do, or the NEXT update
    // is validated against a stale ceiling.
    #[test]
    fn display_state_folds_stream_and_render_forward() {
        let mut st = state_1080p();
        assert_eq!(st.external, (1920, 1080));
        assert_eq!(st.render, None);

        st.apply(&du_stream(1280, 720));
        assert_eq!(st.external, (1280, 720));

        // A render size below launch is a real override even when it equals the current
        // external size: `None` means the LAUNCH size, not the external one.
        st.apply(&du(Some(1280), Some(720), None));
        assert_eq!(st.render, Some((1280, 720)));

        st.apply(&du(Some(640), Some(360), None));
        assert_eq!(st.render, Some((640, 360)));

        // Back up the ladder: the render size is untouched (it is still legal).
        st.apply(&du_stream(1920, 1080));
        assert_eq!(st.external, (1920, 1080));
        assert_eq!(st.render, Some((640, 360)));

        // Back at the launch size folds to the default, stopping the metrics echo.
        st.apply(&du(Some(1920), Some(1080), None));
        assert_eq!(st.render, None);
    }

    // Regression test for the one-step-stale echo: reading `ScaleStage::current()`
    // immediately after `set_size` returns the PREVIOUS size (the pad still carries the old
    // caps), so a 1080->720 step echoed 1080 and a return-to-launch was never reported. The
    // echo comes off the CAPS event.
    #[test]
    fn the_metrics_echo_follows_the_negotiated_size_after_a_live_step() {
        use crate::session::pipeline::ScaleStage;
        gst::init().unwrap();

        // A 1280x720 "launch" software session (openh264 arm: videoscale + capsfilter).
        let mut settings = crate::session::settings::RuntimeSettings::baseline();
        settings.encoder = crate::session::EncoderChoice::Openh264;
        let stream = crate::session::StreamParams {
            width: 1280,
            height: 720,
            fps: 60,
            bitrate_kbps: 8000,
            h264_profile: "constrained-baseline".to_string(),
            codec: crate::session::Codec::H264,
            abr_floor_kbps: 0,
            mic: false,
        };
        let cfg = crate::session::SessionConfig::for_assignment_with(&settings, stream, None);
        let stage: ScaleStage = pipeline::build_scale_stage_for_test(&cfg).unwrap();

        // videotestsrc -> videoconvert -> [stage] -> identity (stands in for the encoder;
        // a real element with a sink pad is all the completion probe needs) -> fakesink.
        let gpipeline = gst::Pipeline::new();
        let src = gst::ElementFactory::make("videotestsrc")
            .property("is-live", true)
            .build()
            .unwrap();
        let convert = gst::ElementFactory::make("videoconvert").build().unwrap();
        let encoder = gst::ElementFactory::make("identity").build().unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        gpipeline
            .add_many([&src, &convert, &encoder, &sink])
            .unwrap();
        gpipeline.add_many(stage.elements.iter()).unwrap();
        let mut chain: Vec<&gst::Element> = vec![&src, &convert];
        chain.extend(stage.elements.iter());
        chain.extend_from_slice(&[&encoder, &sink]);
        gst::Element::link_many(chain).unwrap();

        let metrics = Arc::new(SessionMetrics::new("off", 60));
        let lever = pipeline::resolution_lever_with_echo(stage, encoder, &metrics);

        gpipeline.set_state(gst::State::Playing).unwrap();
        let _ = gpipeline.state(gst::ClockTime::from_seconds(5));

        let echo = |m: &Arc<SessionMetrics>| {
            let w = m.drain_window(std::time::Instant::now());
            (w.stream_width, w.stream_height)
        };
        let wait_for_echo = |want: (Option<i32>, Option<i32>)| {
            for _ in 0..40 {
                if echo(&metrics) == want {
                    return true;
                }
                std::thread::sleep(Duration::from_millis(25));
            }
            false
        };

        // At the launch size, nothing is echoed.
        assert_eq!(echo(&metrics), (None, None));

        // Step DOWN: the echo must become the new size within ~1 s. The stale read returned
        // the launch size, which folds to "default", so this assertion catches the bug.
        apply_stream_update(&lever, 640, 360);
        assert!(
            wait_for_echo((Some(640), Some(360))),
            "the echo never followed the step down: {:?}",
            echo(&metrics)
        );

        // Step back UP to the launch size: the echo must STOP, since that absence is how a
        // consumer sees "back at the launch size".
        apply_stream_update(&lever, 1280, 720);
        assert!(
            wait_for_echo((None, None)),
            "the echo never returned to the launch default: {:?}",
            echo(&metrics)
        );

        gpipeline.set_state(gst::State::Null).unwrap();
    }

    // ── auto/pinned ownership of the external size ──────────────────────────────

    fn test_lever(
        metrics: &Arc<SessionMetrics>,
        launch: (i32, i32),
    ) -> Arc<pipeline::EncodeResolutionLever> {
        gst::init().unwrap();
        let mut settings = crate::session::settings::RuntimeSettings::baseline();
        settings.encoder = crate::session::EncoderChoice::Openh264;
        let stream = crate::session::StreamParams {
            width: launch.0,
            height: launch.1,
            fps: 60,
            bitrate_kbps: 8000,
            h264_profile: "constrained-baseline".to_string(),
            codec: crate::session::Codec::H264,
            abr_floor_kbps: 0,
            mic: false,
        };
        let cfg = crate::session::SessionConfig::for_assignment_with(&settings, stream, None);
        let stage = pipeline::build_scale_stage_for_test(&cfg).unwrap();
        let encoder = gst::ElementFactory::make("identity").build().unwrap();
        pipeline::resolution_lever_with_echo(stage, encoder, metrics)
    }

    // A manual PATCH to a non-launch size pins the session; a PATCH back to the launch size
    // releases it. There is no wire field: "back to launch" IS the release.
    #[test]
    fn a_manual_stream_update_pins_and_the_launch_size_releases() {
        let metrics = Arc::new(SessionMetrics::new("smooth", 60));
        let lever = test_lever(&metrics, (1920, 1080));
        apply_stream_update(&lever, 1280, 720);
        assert!(lever.pinned(), "a non-launch manual size pins");
        assert_eq!(
            metrics
                .drain_window(std::time::Instant::now())
                .external_owner,
            None,
            "the owner key only appears once the size echo does"
        );
        apply_stream_update(&lever, 1920, 1080);
        assert!(!lever.pinned(), "back to launch releases the pin");
    }

    // A REFUSED manual update must not change ownership (the size never moved).
    #[test]
    fn a_refused_manual_stream_update_leaves_ownership_alone() {
        let metrics = Arc::new(SessionMetrics::new("smooth", 60));
        let lever = test_lever(&metrics, (1920, 1080));
        apply_stream_update(&lever, 3840, 2160); // above launch ⇒ refused
        assert!(!lever.pinned());
    }

    // A launch size on no ladder can still be stepped to itself, but nowhere else.
    #[test]
    fn an_unknown_ratio_session_has_only_its_own_rung() {
        let st = SessionDisplayState::new((1280, 1024), true);
        assert!(validate_display_update(&du_stream(1280, 1024), &st).is_ok());
        assert!(validate_display_update(&du_stream(1280, 720), &st).is_err());
    }

    // ── app-exit-before-frames classification (#463) ──────────────────────────
    // An app container exiting before producing a frame surfaced only as "media path
    // interrupted", and the decisive log lines died with the --rm container.

    #[test]
    fn an_app_that_never_presented_is_classified_early_with_its_log_tail() {
        let logs = vec![
            "Steam needs to be online to update".to_string(),
            "Please confirm your network connection and try again.".to_string(),
        ];
        match app_exit_event(AppExitStatus::Code(0), false, logs.clone()) {
            SessionEvent::AppFailed {
                reason,
                reason_code,
                app_log_tail,
            } => {
                assert_eq!(reason_code, REASON_APP_EXITED_EARLY);
                assert_eq!(app_log_tail, logs, "the captured log must travel with it");
                assert!(
                    reason.contains("before producing any video"),
                    "the reason must say what actually happened: {reason}"
                );
                assert!(
                    reason.contains("code 0"),
                    "the exit code must be named: {reason}"
                );
            }
            other => panic!("expected AppFailed, got {other:?}"),
        }
    }

    /// A clean exit code 0 is the #463 shape and must NOT be softened into
    /// "app exited (0)" when nothing was ever on screen.
    #[test]
    fn early_exit_classification_is_independent_of_the_exit_code() {
        for status in [
            AppExitStatus::Code(0),
            AppExitStatus::Code(137),
            AppExitStatus::OomKilled,
            AppExitStatus::Unknown,
        ] {
            match app_exit_event(status, false, vec![]) {
                SessionEvent::AppFailed {
                    reason_code,
                    reason,
                    ..
                } => {
                    assert_eq!(reason_code, REASON_APP_EXITED_EARLY, "{status:?}");
                    assert!(
                        reason.contains("produced no log output"),
                        "an empty capture must say so rather than looking like a missing feature: {reason}"
                    );
                }
                other => panic!("{status:?}: expected AppFailed, got {other:?}"),
            }
        }
    }

    /// An app that streamed and then exited is a different event: its wording must stay
    /// unchanged, and it must not acquire a reason code the UI renders as an early exit.
    #[test]
    fn an_app_that_presented_keeps_the_existing_failure_wording() {
        match app_exit_event(AppExitStatus::Code(1), true, vec!["noise".to_string()]) {
            SessionEvent::Failed(reason) => assert_eq!(reason, "app exited with code 1"),
            other => panic!("expected the unchanged Failed variant, got {other:?}"),
        }
        match app_exit_event(AppExitStatus::OomKilled, true, vec![]) {
            SessionEvent::Failed(reason) => assert_eq!(reason, "app container OOM-killed"),
            other => panic!("expected the unchanged Failed variant, got {other:?}"),
        }
    }

    // ── swap readiness gate (black-screen fix) ────────────────────────────────
    // A compositor frame is NOT evidence the app has drawn: gst-wayland-display renders its
    // empty scene whether or not a client surface is mapped, which is how a live swap
    // re-pointed the encoder 3 ms after the container launched and cut to black.

    #[test]
    fn compositor_frames_alone_never_satisfy_an_app_swap() {
        let mut baseline = None;
        // Hundreds of compositor frames, zero app-surface commits: still not ready.
        assert!(!swap_source_ready(true, Some(0), 1, &mut baseline));
        assert!(!swap_source_ready(true, Some(0), 600, &mut baseline));
        assert_eq!(baseline, None, "no commit observed ⇒ no frame baseline yet");
    }

    #[test]
    fn app_swap_needs_a_compositor_frame_after_the_first_app_commit() {
        let mut baseline = None;
        // First commit seen at frame 42: that frame may predate the commit, so it does not
        // count.
        assert!(!swap_source_ready(true, Some(1), 42, &mut baseline));
        assert_eq!(baseline, Some(42));
        assert!(!swap_source_ready(true, Some(3), 42, &mut baseline));
        // The next frame is necessarily post-commit, so it carries the app.
        assert!(swap_source_ready(true, Some(3), 43, &mut baseline));
    }

    #[test]
    fn generations_with_no_app_container_fall_back_to_the_compositor_frame() {
        // No app surface will ever commit, so waiting for one times out every swap.
        let mut baseline = None;
        assert!(!swap_source_ready(false, None, 0, &mut baseline));
        assert!(swap_source_ready(false, None, 1, &mut baseline));
    }

    #[test]
    fn a_compositor_without_the_commit_counter_degrades_to_the_old_gate() {
        // An image without the app-cadence patch exports no counter: degrade (with a WARN
        // at the call site) rather than making every swap fail.
        let mut baseline = None;
        assert!(!swap_source_ready(true, None, 0, &mut baseline));
        assert!(swap_source_ready(true, None, 1, &mut baseline));
    }

    #[test]
    fn swap_app_ready_timeout_rejects_junk_and_zero() {
        // A typo in the env override must not make every swap fail instantly.
        assert_eq!(
            super::swap_app_ready_timeout(),
            Duration::from_millis(super::SWAP_APP_READY_TIMEOUT_DEFAULT_MS)
        );
    }

    // ── effective_media.audio.path ────────────────────────────────────────────
    // A session that fell back to silence must never be reportable as healthy or as a
    // deliberate test tone. The fallback sets `use_test_audio`, so precedence is the test.

    // ── effective_media.mic ───────────────────────────────────────────────────
    // Every gate must veto on its own: the control-plane grant, the host kill switch, and
    // the presence of an audio PeerConnection (the mic m-line rides it).

    #[test]
    fn mic_reports_negotiated_only_when_every_gate_passes() {
        assert_eq!(mic_state_label(true, false, true), "negotiated");
    }

    #[test]
    fn mic_reports_off_when_any_single_gate_vetoes() {
        // Not granted by the control plane.
        assert_eq!(mic_state_label(false, false, true), "off");
        // QUASAR_MIC_DISABLED=1 host kill switch.
        assert_eq!(mic_state_label(true, true, true), "off");
        // No audio PeerConnection to hang the m-line on.
        assert_eq!(mic_state_label(true, false, false), "off");
        assert_eq!(mic_state_label(false, true, false), "off");
    }

    #[test]
    fn audio_path_reports_sidecar_on_the_healthy_path() {
        assert_eq!(audio_path_label(false, false, false), "sidecar");
    }

    #[test]
    fn audio_path_reports_test_tone_when_deliberately_requested() {
        assert_eq!(audio_path_label(false, false, true), "test-tone");
    }

    #[test]
    fn audio_path_reports_silent_fallback_even_though_test_audio_is_set() {
        // The degraded path flips use_test_audio to swap pulsesrc for audiotestsrc. With
        // the precedence reversed, a MUTE session would advertise an intentional test tone.
        assert_eq!(audio_path_label(false, true, true), "silent-fallback");
    }

    #[test]
    fn audio_path_reports_disabled_regardless_of_other_flags() {
        // QUASAR_AUDIO_DISABLED strips the audio m-line (#304), so nothing can degrade.
        assert_eq!(audio_path_label(true, true, true), "disabled");
        assert_eq!(audio_path_label(true, false, false), "disabled");
    }

    #[test]
    fn vulkan_context_identity_is_the_embedded_object_pointer() {
        gstreamer::init().unwrap();
        let object = gstreamer::Pipeline::new();
        let expected = object.as_ptr() as usize;
        let mut context = gstreamer::Context::new(VULKAN_DEVICE_CONTEXT, true);
        context
            .get_mut()
            .unwrap()
            .structure_mut()
            .set(VULKAN_DEVICE_CONTEXT, &object);
        assert_eq!(
            context_object_identity(&context, VULKAN_DEVICE_CONTEXT),
            Some(expected)
        );
    }

    fn device_context(object: &gstreamer::Pipeline) -> gstreamer::Context {
        let mut context = gstreamer::Context::new(VULKAN_DEVICE_CONTEXT, true);
        context
            .get_mut()
            .unwrap()
            .structure_mut()
            .set(VULKAN_DEVICE_CONTEXT, object);
        context
    }

    #[test]
    fn retained_vulkan_context_rejects_replacement_device_drift() {
        gstreamer::init().unwrap();
        let expected = gstreamer::Pipeline::new();
        let replacement = gstreamer::Pipeline::new();
        let expected_context = device_context(&expected);
        let bridge = VulkanContextBridge {
            instance: gstreamer::Context::new("gst.vulkan.instance", true),
            device: expected_context.clone(),
            device_identity: expected.as_ptr() as usize,
        };
        assert!(bridge.assert_device_context(&expected_context).is_ok());
        assert!(bridge
            .assert_device_context(&device_context(&replacement))
            .is_err());
    }

    #[test]
    fn sctp_stop_race_waits_for_late_intentional_stop() {
        let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let signal = stop.clone();
        let setter = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(40));
            signal.store(true, Ordering::Relaxed);
        });

        assert!(stop_requested_for_transport_error(
            &stop,
            "SCTP association went into error state"
        ));
        setter.join().unwrap();
    }

    #[test]
    fn non_sctp_transport_error_does_not_wait_for_future_stop() {
        let stop = std::sync::atomic::AtomicBool::new(false);
        assert!(!stop_requested_for_transport_error(
            &stop,
            "encoder device was lost"
        ));
        assert!(is_sctp_association_error(
            "encode pipeline error: SCTP association went into error state"
        ));
        assert!(!is_sctp_association_error("ICE connection failed"));
    }

    /// The exact text three live sessions failed with. It must classify as a peer
    /// disconnect, NOT as a generic encode failure.
    const TOWER_20260725_SCTP_ERROR: &str = "encode pipeline error: Could not write to resource. \
         (gstsctpenc.c(898): on_sctp_association_state_changed: SCTP association went into error state)";

    #[test]
    fn classifies_the_live_tower_sctp_association_error() {
        assert!(is_sctp_association_error(TOWER_20260725_SCTP_ERROR));
        // The GError message alone names nothing: only the debug half carries the
        // signature, so classification MUST run over `error` + `debug` concatenated.
        assert!(!is_sctp_association_error("Could not write to resource."));
        assert!(is_sctp_association_error(&format!(
            "{} {:?}",
            "Could not write to resource.",
            Some(
                "gstsctpenc.c(898): on_sctp_association_state_changed (): \
                 /GstPipeline:enc/GstWebRTCBin:webrtc/GstSctpEnc:sctpenc0: \
                 SCTP association went into error state"
            )
        )));
    }

    #[test]
    fn sctp_classifier_matches_defensively_but_not_unrelated_errors() {
        // Case-insensitive; element-name and cause-text shapes both hit.
        assert!(is_sctp_association_error("sctp association shutdown"));
        assert!(is_sctp_association_error(
            "SCTP ASSOCIATION WENT INTO ERROR STATE"
        ));
        assert!(is_sctp_association_error(
            "GstSctpDec:sctpdec0: Failed to parse SCTP packet"
        ));
        for s in [
            "encode pipeline error: Internal data stream error",
            "container launch failed: no such image",
            "not-negotiated: caps mismatch",
            "ICE connection failed",
            "VK_ERROR_DEVICE_LOST",
            "",
        ] {
            assert!(!is_sctp_association_error(s), "false positive on {s:?}");
        }
    }

    /// A peer disconnect must never be mistaken for a GPU fault: it would feed the
    /// GPU-global device-lost detector and trip a drain + agent restart.
    #[test]
    fn sctp_error_is_not_a_vulkan_device_loss() {
        assert!(!super::vulkan_fault::is_device_lost(
            TOWER_20260725_SCTP_ERROR
        ));
    }

    /// The peer-disconnect terminal event is a clean `stopped` with a detail and NO
    /// error_message.
    #[test]
    fn peer_disconnect_stop_event_carries_detail_and_no_error() {
        let event = SessionEvent::Stopped {
            bytes_used: None,
            detail: Some(super::PEER_DISCONNECT_DETAIL),
        };
        match event {
            SessionEvent::Stopped { detail, .. } => {
                assert_eq!(
                    detail,
                    Some("peer disconnected: WebRTC data channel closed")
                )
            }
            other => panic!("expected Stopped, got {other:?}"),
        }
    }

    #[test]
    fn vulkan_effective_media_reports_shared_render_device() {
        assert_eq!(
            effective_encoder_device_path(Some("vulkanh264enc"), None, "/dev/dri/renderD128")
                .as_deref(),
            Some("/dev/dri/renderD128")
        );
        assert_eq!(
            effective_encoder_device_path(
                Some("vah264enc"),
                Some("/dev/dri/renderD129".into()),
                "/dev/dri/renderD128"
            )
            .as_deref(),
            Some("/dev/dri/renderD129")
        );
        assert_eq!(
            effective_encoder_device_path(Some("openh264enc"), None, "software"),
            None
        );
    }

    #[test]
    fn diagnostic_flood_is_bounded_and_exactly_counted() {
        let (tx, mut rx) = tokio::sync::mpsc::channel(2);
        let interval = Arc::new(AtomicU64::new(0));
        let total = Arc::new(AtomicU64::new(0));
        let diagnostics = DiagnosticEventTx::new(tx, interval.clone(), total.clone());
        for i in 0..10 {
            diagnostics.try_emit(
                "session".to_string(),
                TraceEvent {
                    ts_unix_ms: i,
                    event: "flood",
                    payload: serde_json::json!({ "i": i }),
                },
            );
        }
        assert_eq!(interval.load(Ordering::Relaxed), 8);
        assert_eq!(total.load(Ordering::Relaxed), 8);
        assert_eq!(rx.try_recv().unwrap().1.ts_unix_ms, 0);
        assert_eq!(rx.try_recv().unwrap().1.ts_unix_ms, 1);
        assert!(rx.try_recv().is_err());

        assert_eq!(interval.swap(0, Ordering::Relaxed), 8);
        assert_eq!(interval.load(Ordering::Relaxed), 0);
        assert_eq!(total.load(Ordering::Relaxed), 8);
    }

    #[test]
    fn bounded_critical_lane_backpressures_without_drop_or_reorder() {
        let (critical_tx, mut critical_rx) = tokio::sync::mpsc::channel(1);
        let producer = std::thread::spawn(move || {
            for i in 0..100_u64 {
                critical_tx
                    .blocking_send((
                        "session".to_string(),
                        if i == 99 {
                            SessionEvent::Stopped {
                                bytes_used: Some(i),
                                detail: None,
                            }
                        } else {
                            SessionEvent::SwapRolledBack(i.to_string())
                        },
                    ))
                    .unwrap();
            }
        });

        for i in 0..100_u64 {
            let (_, event) = critical_rx.blocking_recv().unwrap();
            if i == 99 {
                assert!(matches!(
                    event,
                    SessionEvent::Stopped {
                        bytes_used: Some(99),
                        detail: None,
                    }
                ));
            } else {
                assert!(
                    matches!(event, SessionEvent::SwapRolledBack(ref n) if n == &i.to_string())
                );
            }
        }
        producer.join().unwrap();
        assert!(critical_rx.try_recv().is_err());
    }

    // Idle-reap policy.
    #[test]
    fn zero_window_never_reaps() {
        assert!(idle_reap_reason(
            Duration::ZERO,
            Duration::from_secs(9999),
            false,
            None,
            false
        )
        .is_none());
    }

    #[test]
    fn never_connected_reaps_after_window() {
        // Before the window, ICE may still be negotiating.
        assert!(idle_reap_reason(WINDOW, Duration::from_secs(60), false, None, false).is_none());
        assert!(idle_reap_reason(WINDOW, WINDOW, false, None, false).is_some());
        assert!(idle_reap_reason(WINDOW, Duration::from_secs(200), false, None, false).is_some());
    }

    #[test]
    fn connected_then_healthy_is_kept() {
        assert!(idle_reap_reason(WINDOW, Duration::from_secs(9999), true, None, false).is_none());
    }

    #[test]
    fn connected_then_briefly_gone_is_kept() {
        // A blip under the window must not reap an established session (schema.md
        // invariant #4: transport state is not session state).
        assert!(idle_reap_reason(
            WINDOW,
            Duration::from_secs(9999),
            true,
            Some(Duration::from_secs(5)),
            false
        )
        .is_none());
    }

    #[test]
    fn connected_then_durably_gone_reaps() {
        assert!(
            idle_reap_reason(WINDOW, Duration::from_secs(9999), true, Some(WINDOW), false)
                .is_some()
        );
    }

    // ── #484 app-boot visibility ──────────────────────────────────────────────
    // A cold app boot is 30–50 s, and charging it against the never-connected idle budget
    // killed real sessions at 120 s with `idle: WebRTC transport never established`.

    #[test]
    fn booting_defers_the_never_connected_reap_at_every_age() {
        // The boot window is not idleness: no age of it may reap.
        for secs in [0u64, 60, 119, 120, 300, 9999] {
            assert!(
                idle_reap_reason(WINDOW, Duration::from_secs(secs), false, None, true).is_none(),
                "booting session reaped after {secs}s"
            );
        }
    }

    #[test]
    fn booting_does_not_defer_the_lost_transport_reap() {
        // A durably dead transport is reclaimed on the same rule, boot flag or not.
        assert!(
            idle_reap_reason(WINDOW, Duration::from_secs(9999), true, Some(WINDOW), true).is_some()
        );
    }

    #[test]
    fn a_generation_with_no_app_container_never_reports_boot_events() {
        // `booting` is false, so the watcher can never latch.
        let mut baseline = None;
        assert!(!gen0_app_presented(false, None, 0, &mut baseline));
        assert!(!gen0_app_presented(false, None, 600, &mut baseline));
        assert!(!gen0_app_presented(false, Some(5), 600, &mut baseline));
    }

    #[test]
    fn a_compositor_without_the_commit_counter_reveals_immediately() {
        // Fail-open: without the vendored patch there is no counter, so the first
        // compositor frame reveals. A stuck loader must be impossible.
        let mut baseline = None;
        assert!(gen0_app_presented(true, None, 1, &mut baseline));
    }

    #[test]
    fn gen0_reveals_only_on_a_frame_produced_after_the_first_app_commit() {
        // #487: the counter also counts unmapped `pending_windows` commits, so a bare
        // `app_has_presented()` test can fire before anything is visible.
        let mut baseline = None;
        assert!(!gen0_app_presented(true, Some(0), 600, &mut baseline));
        assert!(!gen0_app_presented(true, Some(1), 600, &mut baseline));
        assert_eq!(baseline, Some(600));
        assert!(gen0_app_presented(true, Some(1), 601, &mut baseline));
    }

    #[test]
    fn gen0_app_presented_latches_and_never_re_emits() {
        let mut baseline = None;
        assert!(!gen0_app_presented(true, Some(1), 10, &mut baseline));
        assert!(gen0_app_presented(true, Some(1), 11, &mut baseline));
        // Once latched the caller passes `booting = false`, so exactly one AppBooting and
        // one AppPresented are emitted.
        assert!(!gen0_app_presented(false, Some(2), 12, &mut baseline));
        assert!(!gen0_app_presented(false, Some(99), 9999, &mut baseline));
    }

    #[test]
    fn app_boot_watchdog_fires_only_past_the_budget_and_only_while_unpresented() {
        let budget = Some(Duration::from_secs(300));
        assert!(!app_boot_watchdog_fires(
            budget,
            Duration::from_secs(0),
            false
        ));
        assert!(!app_boot_watchdog_fires(
            budget,
            Duration::from_secs(299),
            false
        ));
        assert!(app_boot_watchdog_fires(
            budget,
            Duration::from_secs(300),
            false
        ));
        assert!(app_boot_watchdog_fires(
            budget,
            Duration::from_secs(9999),
            false
        ));
        // A presented app is never a watchdog candidate, however long it ran.
        assert!(!app_boot_watchdog_fires(
            budget,
            Duration::from_secs(9999),
            true
        ));
        // QUASAR_APP_BOOT_TIMEOUT_SECS=0 turns the watchdog off.
        assert!(!app_boot_watchdog_fires(
            None,
            Duration::from_secs(9999),
            false
        ));
    }

    #[test]
    fn app_boot_timeout_defaults_when_unset() {
        // A typo'd value must not silently disable the watchdog.
        assert_eq!(
            super::app_boot_timeout(),
            Some(Duration::from_secs(super::APP_BOOT_TIMEOUT_DEFAULT_SECS))
        );
    }

    // ---- #408: audio pipeline guard ----

    /// A stand-in for the #304 audio pipeline, brought to READY as
    /// `build_encode_pipeline` leaves it.
    fn ready_audio_pipeline() -> gstreamer::Pipeline {
        gstreamer::init().unwrap();
        let pipe = gstreamer::Pipeline::new();
        let src = gstreamer::ElementFactory::make("fakesrc")
            .build()
            .expect("fakesrc");
        let sink = gstreamer::ElementFactory::make("fakesink")
            .build()
            .expect("fakesink");
        pipe.add_many([&src, &sink]).unwrap();
        gstreamer::Element::link_many([&src, &sink]).unwrap();
        pipe.set_state(gstreamer::State::Ready).expect("READY");
        pipe
    }

    /// The hazard the guard exists for: `gst::Pipeline` has no Drop-to-NULL, so dropping
    /// the `Option<gst::Pipeline>` LEAKS it in READY with its webrtcbin/libnice/
    /// GMainContext resources alive.
    #[test]
    fn dropping_a_bare_pipeline_does_not_reach_null() {
        let pipe = ready_audio_pipeline();
        let observer = pipe.clone();
        let bare: Option<gstreamer::Pipeline> = Some(pipe);
        drop(bare);
        assert_eq!(
            observer.current_state(),
            gstreamer::State::Ready,
            "a dropped gst::Pipeline is expected to stay READY — that is the leak"
        );
        let _ = observer.set_state(gstreamer::State::Null);
    }

    /// #408: the guard NULLs on drop, so an early return can no longer leak.
    #[test]
    fn audio_pipeline_guard_nulls_on_drop() {
        let pipe = ready_audio_pipeline();
        let observer = pipe.clone();
        let guard = AudioPipelineGuard::new(Some(pipe));
        assert_eq!(observer.current_state(), gstreamer::State::Ready);
        drop(guard);
        assert_eq!(
            observer.current_state(),
            gstreamer::State::Null,
            "AudioPipelineGuard::drop must set the audio pipeline to NULL"
        );
    }

    /// The ordered teardown paths call `finish()` at a specific point relative to
    /// source/encode teardown, after which it must be a no-op so Drop never re-enters a
    /// NULL transition.
    #[test]
    fn audio_pipeline_guard_finish_is_idempotent_and_drop_is_a_noop_after_it() {
        let pipe = ready_audio_pipeline();
        let observer = pipe.clone();
        let guard = AudioPipelineGuard::new(Some(pipe));
        guard.finish();
        assert_eq!(observer.current_state(), gstreamer::State::Null);
        guard.finish();
        assert!(guard.finished.get());
        drop(guard);
        assert_eq!(observer.current_state(), gstreamer::State::Null);
    }

    /// A `QUASAR_AUDIO_DISABLED` session carries `None`; the guard must be inert, never
    /// panic.
    #[test]
    fn audio_pipeline_guard_tolerates_absent_audio() {
        let guard = AudioPipelineGuard::new(None);
        assert!(guard.as_ref().is_none());
        guard.finish();
        drop(guard);
    }

    // ---- #413: run_local_only idle-loop cadence ----

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
