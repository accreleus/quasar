//! Session subsystem: the `compositor → encode → webrtcbin` media path and WebRTC
//! negotiation, as a launchable parameterized session. Resolution/framerate/bitrate are
//! per-session [`StreamParams`], mirroring `session_assign.stream` (`protocol/agent-api.md`).
//!
//! [`pipeline::build_session_pipeline`] is transport-agnostic: outbound signaling
//! (offer/ICE) goes on an mpsc channel, inbound via [`server::handle_inbound`]. Both the
//! direct WebSocket server ([`server::serve_direct`]) and the control-plane signaling
//! relay drive the same pipeline core.

pub mod abr;
pub mod adaptation;
pub mod audio;
// Bounded, admin-only observation of a LIVE session (graph dot / encoder props /
// telemetry burst), driven from the runner's supervision tick. Never a pad probe,
// never on a streaming thread.
pub mod capture;
pub mod container;
// Headless weston process manager for the nvidia-drm local-display path.
pub(crate) mod console;
pub mod console_hotplug;
// The live display/external-size/ladder echo, and the one statement of the
// absent-when-default rule (`echo::Reported`).
pub mod echo;
/// `encoder.stall` detection: output silence with a reason.
pub mod encoder_stall;
// Loss-triggered ULPFEC ramp: mode derivation + hysteresis controller
// (glue in `pipeline::webrtc`).
pub mod fec;
pub mod gc;
pub mod home;
// #500: sweep of ephemeral (`agent-<8hex>-<8hex>`) managed homes that are unmounted
// and past retention. A floor under `gc` (#175), which only reaps tracked homes.
pub mod homes_gc;
// Steam ACF manifest scanner. Same HTTP-pull/report shape and node-secret auth as
// `gc`; never fatal to the agent.
pub mod library_scan;
pub mod settings;
// Shared GstCudaContext FFI (zero-copy NVENC). Only with `--features cuda`.
#[cfg(feature = "cuda")]
pub mod cuda_share;
pub mod host;
pub mod input;
pub mod ladder;
pub mod metrics;
// Host-path policy for wire-supplied bind mounts (QUASAR_APP_MOUNT_ALLOW).
pub mod mount_policy;
// #489: deferred NVENC encode-pipeline teardown (QUASAR_NVENC_DEFER_TEARDOWN).
pub mod nvenc_defer;
// Physical keyboard/mouse evdev grab, forwarded into virtual_input's uinput devices so
// the compositor's single input path sees both WebRTC and physical input.
pub mod physical_input;
pub mod pipeline;
/// `probe-encoder`: what the encode branch negotiates, through the production builders.
pub mod probe_encoder;
/// Adaptive external resolution rung ladder. Mirror of the control plane's
/// `internal/profile/rungs.go`; advertised to the guest compositor and checked
/// against `session_display_update.stream_*`.
pub mod rungs;
pub mod runner;
/// Answer-SDP m-line reading (`sdp.answer_applied`): which m-lines the peer refused.
pub mod sdp_answer;
pub mod server;
pub mod signaling;
pub mod source;
// #488: golden-home template store — path resolution, .meta.json, the reflink/copy
// clone ladder, atomic publish/remove. Pure filesystem module (see module doc).
pub mod template;
// #260: shared GstVaDisplay FFI. The VA analogue of cuda_share; dlopen's libgstva at
// runtime, so it adds no link dependency.
pub mod va_share;
pub mod virtual_input;
pub mod vulkan_fault;
/// The golden-home warm-up job (build one template per image).
pub mod warmup;

use std::sync::Once;

/// The universal H.264 floor: every WebRTC browser decodes constrained-baseline.
/// The [`StreamParams`] default; the effective profile per encoder is resolved in
/// [`pipeline::h264_caps_profile`] (contract profiles: constrained-baseline/main/high).
pub const PROFILE_CONSTRAINED_BASELINE: &str = "constrained-baseline";

/// The session video codec, orthogonal to [`EncoderChoice`] (vendor). One codec per
/// session, chosen server-side and carried in `session_assign.stream.codec`
/// (`protocol/agent-api.md`); offered as the single video codec in SDP. `H264` is the
/// floor and the default.
///
/// (vendor × codec) resolves to a concrete encoder element at build time via
/// [`pipeline::encoder_candidates`]; `EncoderChoice` is not widened. Browser HEVC
/// decode is per-device (probed client-side); AV1 decodes everywhere in Chrome.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Codec {
    H264,
    H265,
    Av1,
}

impl Codec {
    /// Parse a wire/env codec string (case-insensitive). An unrecognised value must
    /// fail loudly, never fall back to H.264: a silent fallback desyncs
    /// `sessions.codec` from what the agent produces.
    pub fn parse(s: &str) -> anyhow::Result<Codec> {
        match s.trim().to_ascii_lowercase().as_str() {
            "h264" | "avc" => Ok(Codec::H264),
            "h265" | "hevc" => Ok(Codec::H265),
            "av1" => Ok(Codec::Av1),
            other => Err(anyhow::anyhow!(
                "unknown codec '{other}' (legal: h264, h265, av1)"
            )),
        }
    }

    /// The canonical wire string (matches `sessions.codec` / agent-api).
    pub fn as_str(self) -> &'static str {
        match self {
            Codec::H264 => "h264",
            Codec::H265 => "h265",
            Codec::Av1 => "av1",
        }
    }

    /// The bitstream capsfilter media type between encoder and parser.
    pub fn caps_name(self) -> &'static str {
        match self {
            Codec::H264 => "video/x-h264",
            Codec::H265 => "video/x-h265",
            Codec::Av1 => "video/x-av1",
        }
    }

    /// The bitstream parser element.
    pub fn parser_element(self) -> &'static str {
        match self {
            Codec::H264 => "h264parse",
            Codec::H265 => "h265parse",
            Codec::Av1 => "av1parse",
        }
    }

    /// The RTP payloader element. `rtpav1pay` ships in gst-plugins-rs (`rsrtp`); its
    /// presence in the registry is what makes AV1 available (see `probe_codec_support`).
    pub fn rtp_payloader(self) -> &'static str {
        match self {
            Codec::H264 => "rtph264pay",
            Codec::H265 => "rtph265pay",
            Codec::Av1 => "rtpav1pay",
        }
    }

    /// The SDP `encoding-name` for the video RTP caps.
    pub fn rtp_encoding_name(self) -> &'static str {
        match self {
            Codec::H264 => "H264",
            Codec::H265 => "H265",
            Codec::Av1 => "AV1",
        }
    }

    /// Resolve the session codec. Precedence: `QUASAR_CODEC` (set + non-empty), else
    /// the wire value, else `H264`. An unrecognised string from either source errors.
    pub fn resolve(wire: Option<&str>) -> anyhow::Result<Codec> {
        if let Ok(v) = std::env::var("QUASAR_CODEC") {
            if !v.trim().is_empty() {
                return Codec::parse(&v);
            }
        }
        match wire {
            Some(s) => Codec::parse(s),
            None => Ok(Codec::H264),
        }
    }
}

/// Per-session stream parameters. Mirrors the `stream` object of `session_assign`
/// (`protocol/agent-api.md`); env/flags supply them on the standalone path.
#[derive(Debug, Clone)]
pub struct StreamParams {
    pub width: i32,
    pub height: i32,
    pub fps: i32,
    /// Target video bitrate in **kbit/s** (agent-api `bitrate_kbps`). Encoder builders
    /// convert to each element's native unit (openh264enc wants bit/s; VA wants kbit/s).
    pub bitrate_kbps: u32,
    /// Requested H.264 profile (`schema.md` / agent-api `session_assign`).
    /// [`pipeline::h264_caps_profile`] resolves what the chosen encoder actually emits.
    pub h264_profile: String,
    /// The session video codec, from `session_assign.stream.codec` with the
    /// `QUASAR_CODEC` override (see [`Codec::resolve`]). `h264_profile` above is
    /// ignored by the pipeline when this is not `H264`.
    pub codec: Codec,
    /// The stream profile's ABR floor (kbit/s) from
    /// `session_assign.stream.abr_floor_kbps`. `0` = unset, in which case the agent's
    /// env/ratio-derived floor applies; non-zero overrides it
    /// (see [`SessionConfig::abr_config`]).
    pub abr_floor_kbps: u32,
    /// Microphone capture requested (`session_assign.stream.mic`). `false` ⇒ the audio
    /// offer is a single sendonly m-line. Also gated at pipeline build by
    /// `QUASAR_MIC_DISABLED` — see [`SessionConfig::mic_enabled`].
    pub mic: bool,
}

impl Default for StreamParams {
    fn default() -> Self {
        // 720p60 @ 8 Mbps.
        StreamParams {
            width: 1280,
            height: 720,
            fps: 60,
            bitrate_kbps: 8000,
            h264_profile: PROFILE_CONSTRAINED_BASELINE.to_string(),
            codec: Codec::H264,
            abr_floor_kbps: 0,
            mic: false,
        }
    }
}

impl StreamParams {
    /// Read stream params from the environment, falling back to [`Default`]. Knobs:
    /// `QUASAR_WIDTH` / `QUASAR_HEIGHT` / `QUASAR_FPS` / `QUASAR_BITRATE_KBPS` /
    /// `QUASAR_H264_PROFILE` / `QUASAR_CODEC` / `QUASAR_MIC`.
    pub fn from_env() -> Self {
        let d = StreamParams::default();
        StreamParams {
            width: env_i32("QUASAR_WIDTH", d.width),
            height: env_i32("QUASAR_HEIGHT", d.height),
            fps: env_i32("QUASAR_FPS", d.fps),
            bitrate_kbps: env_i32("QUASAR_BITRATE_KBPS", d.bitrate_kbps as i32) as u32,
            h264_profile: std::env::var("QUASAR_H264_PROFILE").unwrap_or(d.h264_profile),
            // A junk QUASAR_CODEC degrades to h264 with a warning here rather than
            // failing the launch; the wire path errors the assignment instead.
            codec: Codec::resolve(None).unwrap_or_else(|e| {
                tracing::warn!(token = "codec-resolve-fallback", "{e}; using h264");
                Codec::H264
            }),
            // No wire floor on this path; QUASAR_ABR_FLOOR_KBPS / the ratio applies.
            abr_floor_kbps: 0,
            // QUASAR_MIC is a dev-only force; the wire grant is the source of truth.
            mic: env_bool("QUASAR_MIC"),
        }
    }
}

/// The ABR operating mode. Knob: `QUASAR_ABR_MODE`, else the legacy
/// `QUASAR_ABR=0` / `QUASAR_ABR_DISABLED=1` (→ `Off`), else `Smooth`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AbrMode {
    /// AS-03 governor: one-step-down / EWMA-gated-up. rtpgccbwe drives the CBR setpoint.
    Protective,
    /// Fixed CBR at the configured bitrate. rtpgccbwe stays attached so
    /// `transport.gcc_estimate_kbps` telemetry flows, but its estimate never retargets
    /// the encoder; zero `abr.retarget` events.
    Off,
    /// Smoothness-biased, encoder-aware down path; up-path identical to `Protective`.
    /// Caps a normal downshift to −12.5% with a longer dwell, smooths the estimate when
    /// the encoder is freshly saturated, and refuses a GCC-only >50% cliff while the
    /// encoder is over budget. Still drops fast (protective one-step, bypassing
    /// cap+dwell) on confirmed congestion, preserving the #68 emergency descent.
    Smooth,
}

/// Outcomes of classifying a raw `QUASAR_ABR_MODE`. Separates "unset or empty" (silent
/// fall-through) from "unrecognised" (warns).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum AbrModeEnv {
    Recognised(AbrMode),
    UnsetOrEmpty,
    Unrecognised,
}

impl AbrMode {
    /// Parse from a string (case-insensitive); unknown ⇒ `None`. Not named `from_str`:
    /// it returns `Option`, and that trips clippy's `should_implement_trait`.
    pub fn parse_env(s: &str) -> Option<Self> {
        match s.to_ascii_lowercase().as_str() {
            "protective" => Some(AbrMode::Protective),
            "off" => Some(AbrMode::Off),
            "smooth" => Some(AbrMode::Smooth),
            _ => None,
        }
    }

    /// The canonical wire string (matches the diagnostic bundle values).
    pub fn as_str(self) -> &'static str {
        match self {
            AbrMode::Protective => "protective",
            AbrMode::Off => "off",
            AbrMode::Smooth => "smooth",
        }
    }

    /// Classify a raw `QUASAR_ABR_MODE` (`None` if unset). A trimmed-empty value must
    /// classify exactly as unset: docker-compose forwards an unset host var as `""`,
    /// and that has to fall through silently, never as unrecognised.
    fn classify_env(raw: Option<&str>) -> AbrModeEnv {
        match raw.map(str::trim) {
            None | Some("") => AbrModeEnv::UnsetOrEmpty,
            Some(s) => match Self::parse_env(s) {
                Some(m) => AbrModeEnv::Recognised(m),
                None => AbrModeEnv::Unrecognised,
            },
        }
    }

    /// Resolve the ABR mode from the environment (see the type doc for precedence).
    pub fn from_env() -> Self {
        let raw = std::env::var("QUASAR_ABR_MODE").ok();
        match Self::classify_env(raw.as_deref()) {
            AbrModeEnv::Recognised(m) => return m,
            // Empty-after-trim is treated as unset: silent fall-through, no WARN.
            AbrModeEnv::UnsetOrEmpty => {}
            AbrModeEnv::Unrecognised => {
                tracing::warn!(
                    token = "knob-invalid-abr-mode",
                    "QUASAR_ABR_MODE={:?} is not a recognised value (protective|off|smooth); \
                     falling back to legacy flags / default",
                    raw.as_deref().unwrap_or_default()
                );
            }
        }
        // Legacy disable flags.
        if matches!(
            std::env::var("QUASAR_ABR").ok().as_deref(),
            Some("0") | Some("false") | Some("FALSE")
        ) || env_bool("QUASAR_ABR_DISABLED")
        {
            return AbrMode::Off;
        }
        // SPT-10: `smooth` ships as the default. Identical to `protective` on a clean
        // path; under netem it cut present σ p95 ~69→19 ms, freezes 14→2, browser fps
        // 47→60, and it preserves the #68 emergency fast-drop.
        AbrMode::Smooth
    }
}

/// Which encoder element to build. Agent-local; knob: `QUASAR_ENCODER`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum EncoderChoice {
    /// `openh264enc` — software, I420 in, emits constrained-baseline. Default.
    Openh264,
    /// `vah264lpenc` / `vah264enc` — VAAPI hardware (AMD/Intel), NV12 in.
    Va,
    /// `nvcudah264enc` — NVENC, `memory:CUDAMemory` NV12 in (ZC-02). Blackwell killed
    /// the legacy `nvh264enc` preset GUIDs, so the modern CUDA-memory encoder is the
    /// only option; `cudaconvert` feeds it NV12 from the compositor's BGRA.
    Nvenc,
    /// `vulkanh264enc` — Vulkan Video encode, `memory:VulkanImage` NV12 in. The
    /// compositor emits `memory:VulkanImage` directly, selected by `waylanddisplaysrc`'s
    /// `vulkan` property (see `source_branch::build_video_source`).
    Vulkan,
}

/// Everything needed to build and run one session pipeline. [`StreamParams`] is the
/// control-plane contract; the rest are agent-local realization knobs.
#[derive(Debug, Clone)]
pub struct SessionConfig {
    pub stream: StreamParams,
    /// The encoder the pipeline actually builds as. The runner resolves any per-session
    /// codec fallback ([`pipeline::effective_encoder`]) once, after `gst::init`, and
    /// rewrites this, so every downstream site keys on it with no per-site branching.
    pub encoder: EncoderChoice,
    /// The host-configured encoder (`QUASAR_ENCODER` / hostcfg), retained across a codec
    /// fallback so `effective_media_snapshot` can surface configured-vs-actual.
    pub configured_encoder: EncoderChoice,
    /// The assigned codec (`sessions.codec` / `session_assign.stream.codec`), kept apart
    /// from `stream.codec` so the effective-media snapshot can surface an
    /// assigned-vs-streamed divergence.
    pub configured_codec: Codec,
    /// Whether `QUASAR_CODEC` moved this session off
    /// [`configured_codec`](Self::configured_codec), so a stray prod-agent desync is
    /// visible in the snapshot rather than silent.
    pub codec_env_override: bool,
    /// `waylanddisplaysrc` render-node: "software" (llvmpipe) or a DRM node path.
    /// Ignored under `use_test_src`.
    pub render_node: String,
    /// The operator-configured identity before canonicalization, so diagnostics can
    /// distinguish configured from resolved device paths.
    pub render_node_configured: String,
    /// CUDA device id shared by the compositor's `cuda-device-id`, the encoder's pin, and
    /// the SO-07 read-back. Knob: `QUASAR_CUDA_DEVICE`; ignored unless `encoder == Nvenc`.
    pub cuda_device_id: i32,
    /// Keyframe interval in frames (`QUASAR_GOP`).
    pub gop: u32,
    /// VA-only loss-resilience knob: slices per frame (`QUASAR_SLICES`). Default 8
    /// (SO-02) — more parallel VCN slices keep the encoder ahead of bitrate peaks,
    /// avoiding the drop-burst 4 slices still hit.
    pub num_slices: u32,
    /// VA encoder `target-usage` (`QUASAR_TARGET_USAGE`, 1=quality … 7=speed).
    /// Default 6 (Wolf's low-latency value). VA only.
    pub target_usage: u32,
    /// Interpipe→encoder leaky-queue depth in buffers (`QUASAR_QUEUE_BUFFERS`).
    /// Default 3 (≈50 ms @60fps); lower trims queueing latency.
    pub queue_buffers: u32,
    /// Host-stage latency probe (`QUASAR_LATENCY_PROBE`). Gates the per-frame stage
    /// timings the always-on pad probes publish as `probe_*` keys on `session_metrics`.
    /// Design: `docs/superpowers/specs/2026-08-18-latency-probe-design.md`.
    pub latency_probe: bool,
    /// Use `videotestsrc` instead of the compositor: transport/encode without a Wayland
    /// app (input is parsed + logged, not injected). An explicit test override.
    pub use_test_src: bool,
    /// Use a silent `audiotestsrc` instead of `pulsesrc`. Set when PulseAudio is
    /// unreachable (`QUASAR_USE_TEST_AUDIO`), or when `use_test_src` is true.
    pub use_test_audio: bool,
    /// STUN server URL for WAN ICE. LAN/host-local needs none.
    pub stun: Option<String>,
    /// The app container to launch into the session compositor. `None` ⇒ no app.
    pub container: Option<container::ContainerSpec>,
    /// `XDG_RUNTIME_DIR` holding the compositor's Wayland socket, bind-mounted into the
    /// app container. Must be a path the container runtime can resolve.
    pub runtime_dir: String,
    /// PulseAudio socket URI (`unix:/path/to/native`) for `pulsesrc` and the app
    /// container's `PULSE_SERVER`. `None` ⇒ no sidecar, and `use_test_audio` should then
    /// be true. Populated by [`host::SessionHost::prepare`].
    pub pulse_server: Option<String>,
    /// Why this session fell back to silent audio when it wanted real audio; `None` on
    /// the healthy path and under test audio. Reported on `effective_media.audio` so a
    /// muted session is visible above the agent, not just a WARN in a container log.
    pub audio_degraded_reason: Option<String>,
    /// `QUASAR_AUDIO_REQUIRED` — treat an unavailable PulseAudio sidecar as a session
    /// failure rather than degrading to silence. Off by default so headless/CI hosts
    /// still run; release deployments should turn it on.
    pub audio_required: bool,
    /// Idle-session reap window. After `running`, the WebRTC transport must establish
    /// within it and must not stay gone this long afterwards, or the runner tears the
    /// session down and reports `failed` so its reservation is reclaimed.
    /// `Duration::ZERO` disables. Knob: `QUASAR_IDLE_TIMEOUT_SECS`.
    pub idle_timeout: std::time::Duration,
    /// ZC-03 full zero-copy VA: the compositor emits `memory:DMABuf` in a
    /// `vapostproc`-importable DRM modifier and `vapostproc` imports it, no system-memory
    /// hop. Knob: `QUASAR_ZEROCOPY`; gated by [`SessionConfig::dmabuf_zerocopy`].
    pub zerocopy: bool,
    /// ABR operating mode; see [`AbrMode`]. A control-plane host_settings push can
    /// overlay this via config_update (`abr_enabled` maps to Off/Protective).
    pub abr_mode: AbrMode,
    /// Explicit ABR floor (kbit/s) the governor never starves below. Precedence: the
    /// wire `session_assign.stream.abr_floor_kbps`, else `QUASAR_ABR_FLOOR_KBPS`, else
    /// (`None`) `ceiling × QUASAR_ABR_FLOOR_RATIO`.
    pub abr_floor_kbps: Option<u32>,
    /// The host/env ABR floor alone (`QUASAR_ABR_FLOOR_KBPS` or the `abr_floor_kbps` host
    /// setting), with the wire value NOT folded in. A profile floor travels with the
    /// rung; a host floor is absolute, so what the ladder may scale must be read here and
    /// never off merged [`abr_floor_kbps`](Self::abr_floor_kbps) — there a non-zero wire
    /// value wins, giving a host floor no effect whenever a profile also carries one.
    pub host_abr_floor_kbps: Option<u32>,
    /// Fraction of the ceiling used as the ABR floor when `abr_floor_kbps` is unset.
    /// Knob: `QUASAR_ABR_FLOOR_RATIO`.
    pub abr_floor_ratio: f64,
    /// SPT-08 ladder knobs snapshotted at assign time (live-class: a later
    /// `config_update` applies to the NEXT session, never this one).
    pub ladder: ladder::LadderSettings,
    /// ABR governor hysteresis knobs, snapshotted at assign time (live-class, as above).
    pub abr_governor: abr::AbrGovernorSettings,
    /// ULPFEC/RED redundancy percentage for the video transceiver (index 0). `0` leaves
    /// `fec-type`/`fec-percentage` untouched and puts no `red`/`ulpfec` lines in the
    /// offer. In `auto` mode this is reused as the *armed* level, not a static rate
    /// (default [`fec::DEFAULT_ARMED_PCT`]). Knob: `QUASAR_FEC_PERCENTAGE`.
    pub fec_percentage: u32,
    /// FEC operating mode. Knob: `QUASAR_FEC_MODE`, else derived from `fec_percentage`.
    /// `Auto` negotiates `ulp-red` at 0% up front and ramps `fec-percentage` on the
    /// agent-local loss signal ([`fec::FecController`]).
    pub fec_mode: fec::FecMode,
    /// The host's console-mode config, snapshotted from the agent's latched
    /// `config_update.console_config`. `None` ⇒ the runner falls back to
    /// `QUASAR_LOCAL_DISPLAY`. Drives the local-display leg.
    pub console_config: Option<crate::messages::ConsoleConfig>,
    /// Assignment-scoped output plan; defaults to stream-only for old control planes and
    /// standalone/dev sessions.
    pub video_topology: crate::messages::VideoTopology,
    /// #377: the session's effective `home_root`, driving pre-launch home provisioning
    /// and post-session home-usage measurement (`session::home`). Empty ⇒ both are
    /// no-ops. Held verbatim; absoluteness is validated at use.
    pub home_root: String,
    /// #375: host directory of 32-bit NVIDIA driver libs, bind-mounted read-only into the
    /// app container at `/opt/quasar/nvidia-lib32`, but only when the session has NVIDIA
    /// GPU access (gated in `container.rs`). Empty ⇒ no mount.
    pub nvidia_lib32_path: String,
    /// #227: rolling intra refresh on `vulkanh264enc`. Vulkan-only; needs the vendored
    /// `vkh264enc-intra-refresh.patch` (no-ops with a warning on an unpatched build, as
    /// `num_slices` does). Knob: `QUASAR_INTRA_REFRESH`.
    pub intra_refresh: bool,
    /// `vulkanh264enc` `intra-refresh-period` in frames (`QUASAR_INTRA_REFRESH_PERIOD`,
    /// 0 = continuous back-to-back cycles). Ignored unless `intra_refresh`.
    pub intra_refresh_period: u32,
    /// #488: the resolved golden-home template store, snapshotted from `agent.rs`
    /// (`QUASAR_HOME_TEMPLATES` gate + `template::TemplateStore::resolve_from_env`).
    /// `None` when off or misconfigured, and `provision_home_dirs` then does no seeding.
    pub template_store: Option<template::TemplateStore>,
    /// The `image_id` this session's container resolves to
    /// (`ImageManager::image_id_for_ref`; the wire's `AppSpec.image` carries none).
    /// `None` for test-src or an image never `image_ensure`'d, and seeding is then
    /// skipped rather than failing the launch.
    pub image_id: Option<String>,
    /// #425: the AUDIO webrtcbin's `latency` (rtpbin jitter-buffer target, ms); the video
    /// PC is untouched. Effectively the mic receive-leg jitter buffer, where webrtcbin's
    /// own 200 ms default dominates the ~250 ms round-trip on the mic loopback test.
    /// Knob: `QUASAR_MIC_JITTER_MS` (default 60, target 50-75 ms).
    pub mic_jitter_ms: u32,
}

/// Default `XDG_RUNTIME_DIR` for the Wayland socket. Matches the deploy run scripts.
fn default_runtime_dir() -> String {
    std::env::var("XDG_RUNTIME_DIR").unwrap_or_else(|_| "/tmp/runtime-quasar".to_string())
}

impl SessionConfig {
    /// Build from the environment plus the `use_test_src` / `stun` command-line flags.
    pub fn from_env(use_test_src: bool, stun: Option<String>) -> Self {
        let encoder = settings::resolve_encoder_choice();
        let stream = StreamParams::from_env();
        // On this path QUASAR_CODEC IS the codec source, not an override of a
        // control-plane value, so no override is recorded.
        let configured_codec = stream.codec;
        SessionConfig {
            stream,
            encoder,
            configured_encoder: encoder,
            configured_codec,
            codec_env_override: false,
            render_node: std::env::var("QUASAR_RENDER_NODE")
                .unwrap_or_else(|_| "software".to_string()),
            render_node_configured: std::env::var("QUASAR_RENDER_NODE")
                .unwrap_or_else(|_| "software".to_string()),
            // env_i32 rejects non-positive, so read directly to allow device 0.
            cuda_device_id: std::env::var("QUASAR_CUDA_DEVICE")
                .ok()
                .and_then(|s| s.parse::<i32>().ok())
                .filter(|&n| n >= 0)
                .unwrap_or(0),
            gop: env_i32("QUASAR_GOP", 60) as u32,
            num_slices: env_i32("QUASAR_SLICES", 8) as u32,
            target_usage: env_i32("QUASAR_TARGET_USAGE", 6) as u32,
            queue_buffers: env_i32("QUASAR_QUEUE_BUFFERS", 3) as u32,
            latency_probe: env_bool("QUASAR_LATENCY_PROBE"),
            use_test_src,
            use_test_audio: use_test_src || env_bool("QUASAR_USE_TEST_AUDIO"),
            stun,
            // QUASAR_APP_IMAGE on this path; the agent path fills it from the assign's
            // AppSpec (see for_assignment).
            container: container::ContainerSpec::from_env(),
            runtime_dir: default_runtime_dir(),
            pulse_server: None,
            audio_degraded_reason: None,
            audio_required: env_bool("QUASAR_AUDIO_REQUIRED"),
            idle_timeout: std::time::Duration::from_secs(env_u64("QUASAR_IDLE_TIMEOUT_SECS", 120)),
            zerocopy: env_bool("QUASAR_ZEROCOPY"),
            abr_mode: AbrMode::from_env(),
            abr_floor_kbps: std::env::var("QUASAR_ABR_FLOOR_KBPS")
                .ok()
                .and_then(|s| s.parse::<u32>().ok())
                .filter(|&n| n > 0),
            // No wire floor on this path, so this equals the merged one. Kept a separate
            // read rather than an alias, so the two never drift if a wire source arrives.
            host_abr_floor_kbps: std::env::var("QUASAR_ABR_FLOOR_KBPS")
                .ok()
                .and_then(|s| s.parse::<u32>().ok())
                .filter(|&n| n > 0),
            abr_floor_ratio: env_f64("QUASAR_ABR_FLOOR_RATIO", 0.3),
            ladder: ladder::LadderSettings::from_env(),
            abr_governor: abr::AbrGovernorSettings::from_env(),
            fec_percentage: env_fec_percentage("QUASAR_FEC_PERCENTAGE"),
            fec_mode: resolve_fec_mode(env_fec_percentage("QUASAR_FEC_PERCENTAGE")),
            console_config: None,
            video_topology: crate::messages::VideoTopology::StreamOnly,
            // env-direct: from_env has no settings overlay. The assign path snapshots
            // the effective values instead.
            home_root: std::env::var("QUASAR_HOME_ROOT").unwrap_or_default(),
            nvidia_lib32_path: std::env::var("QUASAR_NV_LIB32_PATH").unwrap_or_default(),
            intra_refresh: env_bool("QUASAR_INTRA_REFRESH"),
            intra_refresh_period: env_u64("QUASAR_INTRA_REFRESH_PERIOD", 0) as u32,
            // No connection-level agent state to snapshot here; callers that want
            // seeding set these explicitly after construction.
            template_store: None,
            image_id: None,
            mic_jitter_ms: env_mic_jitter_ms("QUASAR_MIC_JITTER_MS"),
        }
    }

    /// Whether this session negotiates a microphone (client → host) m-line. Both gates
    /// must pass: the wire grant `session_assign.stream.mic`, and the host kill switch
    /// `QUASAR_MIC_DISABLED` unset (`pipeline::mic_disabled`, mirror of
    /// `QUASAR_AUDIO_DISABLED`). Actually *building* the mic path additionally needs an
    /// audio webrtcbin and a live PulseAudio sidecar (`pipeline::build_encode_pipeline`).
    pub fn mic_enabled(&self) -> bool {
        self.stream.mic && !pipeline::mic_disabled()
    }

    /// The ABR governor configuration, or `None` in `Off` mode (static CBR). Ceiling is
    /// the configured profile/tier bitrate; floor is the resolved `abr_floor_kbps`,
    /// clamped to `[500, ceiling-1]`.
    pub fn abr_config(&self) -> Option<abr::AbrConfig> {
        if self.abr_mode == AbrMode::Off {
            return None;
        }
        let ceiling = self.stream.bitrate_kbps;
        let derived = (ceiling as f64 * self.abr_floor_ratio).round() as u32;
        let floor = self
            .abr_floor_kbps
            .unwrap_or(derived)
            .clamp(500, ceiling.saturating_sub(1).max(500));
        let fps = self.stream.fps.max(1) as u32;
        // `abr_governor` overlays the operator hysteresis knobs, resolved
        // `env ← config_update` like every other RuntimeSettings field; reading them off
        // the process env instead makes them unreachable from a Host Settings push.
        let base = match self.abr_mode {
            AbrMode::Smooth => abr::AbrConfig::new_smooth(floor, ceiling, fps),
            _ => abr::AbrConfig::new(floor, ceiling, fps),
        };
        Some(self.abr_governor.apply_to(base))
    }

    /// SPT-08 adaptation ladder config, or `None` when NO rung is armed. The ladder
    /// rides the `Smooth` mode's per-window classifier hook, so `Protective`/`Off` never
    /// engage any rung; that gate is announced by [`Self::ladder_gate_warning`].
    ///
    /// #502: each rung has its own switch. `abr_ladder` / `QUASAR_ABR_LADDER` is the
    /// encoder-speed-bias rung alone; off ⇒ `max_bias = 0`, which
    /// [`ladder::Ladder::observe`] treats as "rung disabled". It must never disarm
    /// `abr_ladder_resolution` / `abr_ladder_fps`: doing so silently ran two full netem
    /// experiments with no rung and no log line
    /// (`docs/reports/2026-08-22-vulkanscale-validation/REPORT.md`).
    ///
    /// Knobs come from the [`ladder`](Self::ladder) snapshot, not the process env, which
    /// is what makes them admin-editable per host.
    pub fn ladder_config(&self) -> Option<ladder::LadderConfig> {
        if self.abr_mode != AbrMode::Smooth {
            return None;
        }
        let s = &self.ladder;
        if !s.enabled && !s.resolution_enabled && !s.fps_enabled {
            return None;
        }
        let mut cfg = s.to_config();
        if !s.enabled {
            // Retire the speed-bias rung only; every other rung keeps its own switch.
            // `max_bias = 0` is the ladder module's own "rung disabled" encoding, so no
            // new code path enters the decision machine.
            cfg.max_bias = 0;
        }
        Some(cfg)
    }

    /// The one-line operator warning for a ladder rung armed on this host that cannot run
    /// in this session's ABR mode, or `None` when there is nothing to say.
    ///
    /// The resolution/fps rungs ride the `Smooth` classifier hook, so `protective`/`off`
    /// leave them inert whatever the per-rung knobs say. That interaction is invisible in
    /// `GET /v1/admin/hosts/{id}/settings`, so the agent says it out loud once per
    /// session. Pure (the caller logs it) so it is testable with no tracing subscriber.
    pub fn ladder_gate_warning(&self) -> Option<String> {
        let s = &self.ladder;
        if self.abr_mode == AbrMode::Smooth || !(s.resolution_enabled || s.fps_enabled) {
            return None;
        }
        let armed = match (s.resolution_enabled, s.fps_enabled) {
            (true, true) => "abr_ladder_resolution + abr_ladder_fps are",
            (true, false) => "abr_ladder_resolution is",
            _ => "abr_ladder_fps is",
        };
        Some(format!(
            "SPT-08 ladder: {armed} ON for this host, but abr_mode={} — the ladder rungs \
             only run in `smooth` mode, so they are INERT for this session. Set \
             abr_mode=smooth on the host to arm them.",
            self.abr_mode.as_str(),
        ))
    }

    /// The resolved FEC setup: whether to negotiate `ulp-red`, the `fec-percentage` set
    /// at build, whether to run the auto controller, and the armed level. Pure
    /// derivation (see [`fec::derive_plan`]).
    pub fn fec_plan(&self) -> fec::FecPlan {
        fec::derive_plan(self.fec_mode, self.fec_percentage)
    }

    /// The auto FEC controller configuration, defaults overlaid by the operator
    /// `QUASAR_FEC_*` hysteresis knobs. Only meaningful when
    /// [`fec_plan().controller_enabled`](fec::FecPlan::controller_enabled).
    pub fn fec_controller_config(&self) -> fec::FecControllerConfig {
        fec::FecControllerConfig {
            arm_loss_pct: env_f64(
                "QUASAR_FEC_ARM_LOSS_PCT",
                fec::FecControllerConfig::DEFAULT_ARM_LOSS_PCT,
            ),
            arm_windows: env_i32(
                "QUASAR_FEC_ARM_WINDOWS",
                fec::FecControllerConfig::DEFAULT_ARM_WINDOWS as i32,
            ) as u32,
            disarm_windows: env_i32(
                "QUASAR_FEC_DISARM_WINDOWS",
                fec::FecControllerConfig::DEFAULT_DISARM_WINDOWS as i32,
            ) as u32,
            max_flaps: env_i32(
                "QUASAR_FEC_MAX_FLAPS",
                fec::FecControllerConfig::DEFAULT_MAX_FLAPS as i32,
            ) as u32,
            armed_pct: self.fec_plan().armed_pct,
            window_s: env_u64(
                "QUASAR_FEC_WINDOW_S",
                fec::FecControllerConfig::DEFAULT_WINDOW_S,
            )
            .max(1),
        }
    }

    /// ZC-03 active? Requires `QUASAR_ZEROCOPY` and the VA encoder. The DMABuf
    /// format/modifier is negotiated from `vapostproc`'s caps at pipeline build; with no
    /// compatible one the path falls back to ZC-01's read-back + upload.
    pub fn dmabuf_zerocopy(&self) -> bool {
        self.zerocopy && self.encoder == EncoderChoice::Va
    }

    /// Apply the `QUASAR_CODEC` force override loudly. The env beats the assigned wire
    /// codec, so a real change must WARN with both values and record
    /// [`codec_env_override`](Self::codec_env_override); otherwise a stray production
    /// `QUASAR_CODEC` silently desyncs `sessions.codec` from the streamed codec.
    /// [`configured_codec`](Self::configured_codec) stays at the assigned value. A junk
    /// value is warned and ignored: it must never reject a valid assignment. Idempotent
    /// when the env matches.
    pub fn apply_codec_env_override(&mut self) {
        let raw = match std::env::var("QUASAR_CODEC") {
            Ok(v) if !v.trim().is_empty() => v,
            _ => return,
        };
        let forced = match Codec::parse(&raw) {
            Ok(c) => c,
            Err(e) => {
                tracing::warn!(
                    token = "knob-invalid-codec-override",
                    "QUASAR_CODEC={raw:?} is invalid ({e}); ignoring the codec override"
                );
                return;
            }
        };
        if forced != self.stream.codec {
            tracing::warn!(
                token = "codec-force-override-applied",
                "QUASAR_CODEC={} OVERRIDES the control-plane-assigned codec {} for this session — \
                 the stream will be {} while sessions.codec (DB) says {}. This is the agent-side \
                 force escape hatch; unset QUASAR_CODEC on production agents to avoid a silent desync.",
                forced.as_str(),
                self.stream.codec.as_str(),
                forced.as_str(),
                self.stream.codec.as_str(),
            );
            self.stream.codec = forced;
            self.codec_env_override = true;
        }
    }

    /// Build a config for an assigned session from the agent's resolved runtime settings
    /// (latest `config_update`, or the env baseline). Stream params and `container` come
    /// from the `session_assign`; agent-local knobs come from `settings`.
    pub fn for_assignment_with(
        settings: &settings::RuntimeSettings,
        stream: StreamParams,
        container: Option<container::ContainerSpec>,
    ) -> Self {
        let use_test_src = env_bool("QUASAR_USE_TEST_SRC");
        // Capture the wire floor before `stream` moves into the config.
        let wire_abr_floor_kbps = stream.abr_floor_kbps;
        // Still the assigned codec here: the QUASAR_CODEC override applies later, at
        // session build.
        let configured_codec = stream.codec;
        SessionConfig {
            stream,
            encoder: settings.encoder,
            configured_encoder: settings.encoder,
            configured_codec,
            codec_env_override: false,
            render_node: settings.render_node.clone(),
            render_node_configured: settings.render_node_configured.clone(),
            cuda_device_id: settings.cuda_device_id,
            gop: settings.gop,
            num_slices: settings.num_slices,
            target_usage: settings.target_usage,
            queue_buffers: settings.queue_buffers,
            latency_probe: settings.latency_probe,
            use_test_src,
            use_test_audio: use_test_src || env_bool("QUASAR_USE_TEST_AUDIO"),
            stun: None,
            container,
            runtime_dir: default_runtime_dir(),
            pulse_server: None,
            audio_degraded_reason: None,
            audio_required: env_bool("QUASAR_AUDIO_REQUIRED"),
            idle_timeout: std::time::Duration::from_secs(settings.idle_timeout_secs),
            zerocopy: settings.zerocopy,
            abr_mode: settings.abr_mode,
            // A non-zero wire floor takes precedence; 0 means the env/ratio fallback
            // (`QUASAR_ABR_FLOOR_KBPS`) applies.
            abr_floor_kbps: if wire_abr_floor_kbps > 0 {
                Some(wire_abr_floor_kbps)
            } else {
                settings.abr_floor_kbps
            },
            // Unmerged and unconditional: the wire value must not mask a host floor,
            // which is absolute whether or not the profile also named one.
            host_abr_floor_kbps: settings.abr_floor_kbps,
            abr_floor_ratio: settings.abr_floor_ratio,
            ladder: settings.ladder,
            abr_governor: settings.abr_governor,
            // env-direct, not a RuntimeSettings/hostcfg knob.
            fec_percentage: env_fec_percentage("QUASAR_FEC_PERCENTAGE"),
            fec_mode: resolve_fec_mode(env_fec_percentage("QUASAR_FEC_PERCENTAGE")),
            console_config: None,
            video_topology: crate::messages::VideoTopology::StreamOnly,
            // Snapshot so provision + measurement use the value this session was
            // assigned under.
            home_root: settings.home_root.clone(),
            // Snapshot so a mid-session config change can't tear the running container.
            nvidia_lib32_path: settings.nvidia_lib32_path.clone(),
            intra_refresh: env_bool("QUASAR_INTRA_REFRESH"),
            intra_refresh_period: env_u64("QUASAR_INTRA_REFRESH_PERIOD", 0) as u32,
            // `agent.rs`'s SessionAssign handler sets these right after construction,
            // alongside `cfg.console_config` / `cfg.video_topology`.
            template_store: None,
            image_id: None,
            mic_jitter_ms: env_mic_jitter_ms("QUASAR_MIC_JITTER_MS"),
        }
    }

    /// [`Self::for_assignment_with`] against the baseline runtime settings. The source
    /// defaults to the real compositor + container launch; `QUASAR_USE_TEST_SRC` is the
    /// only way to `videotestsrc`.
    pub fn for_assignment(
        stream: StreamParams,
        container: Option<container::ContainerSpec>,
    ) -> Self {
        Self::for_assignment_with(&settings::RuntimeSettings::baseline(), stream, container)
    }
}

static GST_INIT: Once = Once::new();

/// Has this process already scanned the GStreamer registry? A plugin feature that
/// appears on disk after the scan can never be registered, so `cuda_runtime` uses this
/// to decide whether freshly-placed NVRTC needs a process restart
/// (`cuda_runtime::restart_needed_after_placement`).
pub fn gst_initialised() -> bool {
    GST_INIT.is_completed()
}

/// Initialise GStreamer exactly once per process (see [`init_gstreamer`]). Safe from any
/// thread; subsequent calls are no-ops.
pub fn ensure_gst_init(cfg: &SessionConfig) -> anyhow::Result<()> {
    let mut result = Ok(());
    GST_INIT.call_once(|| {
        result = init_gstreamer(cfg);
    });
    result
}

/// Initialise GStreamer for a session. Must run before any other GStreamer API.
///
/// Stale-registry trap: VA/NVENC/Vulkan elements are device-probing and register only
/// for a GPU present at registry-scan time, and the image bakes its registry with no
/// GPU. So point `GST_REGISTRY` at a fresh per-process path to force a re-scan against
/// the runtime GPU (registers `vah264enc` / `nvcudah264enc` + `cudaconvert` /
/// `vulkanh264enc`). Unconditional, and harmless for software: gst init runs once per
/// process at the startup codec probe, whose cfg carries the env-baseline encoder, but a
/// `config_update` can flip a software-env host to a hardware encoder with no restart —
/// gating the fresh scan on `cfg.encoder` would strand such a host on the baked registry
/// with no hardware elements.
pub fn init_gstreamer(_cfg: &SessionConfig) -> anyhow::Result<()> {
    use anyhow::Context;
    let path = format!("/tmp/quasar-gst-registry-{}.bin", std::process::id());
    let _ = std::fs::remove_file(&path);
    std::env::set_var("GST_REGISTRY", &path);
    gstreamer::init().context("failed to initialise GStreamer")?;
    Ok(())
}

/// Parse an `i32` env var, keeping only strictly-positive values (empty/junk ⇒ default).
fn env_i32(var: &str, default: i32) -> i32 {
    std::env::var(var)
        .ok()
        .and_then(|s| s.parse::<i32>().ok())
        .filter(|&n| n > 0)
        .unwrap_or(default)
}

/// Parse a `u64` env var. `0` is accepted and meaningful (it disables the idle
/// timeout); junk/empty ⇒ default.
fn env_u64(var: &str, default: u64) -> u64 {
    std::env::var(var)
        .ok()
        .and_then(|s| s.parse::<u64>().ok())
        .unwrap_or(default)
}

/// Parse `QUASAR_FEC_PERCENTAGE`: unset/empty ⇒ `0` (off, silently). Out of `0..=100`
/// or unparseable ⇒ warn and `0`, never a nonsensical redundancy on the transceiver.
fn env_fec_percentage(var: &str) -> u32 {
    match std::env::var(var) {
        Err(_) => 0,
        Ok(s) if s.is_empty() => 0,
        Ok(s) => match s.parse::<u32>() {
            Ok(n) if n <= 100 => n,
            _ => {
                tracing::warn!(
                    token = "knob-invalid-fec-percentage",
                    "{var}={s:?} is not a valid percentage (0-100) — FEC disabled (0)"
                );
                0
            }
        },
    }
}

/// Parse `QUASAR_MIC_JITTER_MS`: unset/empty ⇒ 60 ms. Unparseable or `0` ⇒ warn and use
/// the default; a 0 ms jitter buffer turns any reorder or late arrival into an audible
/// gap.
fn env_mic_jitter_ms(var: &str) -> u32 {
    const DEFAULT_MIC_JITTER_MS: u32 = 60;
    match std::env::var(var) {
        Err(_) => DEFAULT_MIC_JITTER_MS,
        Ok(s) if s.is_empty() => DEFAULT_MIC_JITTER_MS,
        Ok(s) => match s.parse::<u32>() {
            Ok(n) if n > 0 => n,
            _ => {
                tracing::warn!(
                    token = "knob-invalid-mic-jitter-ms",
                    "{var}={s:?} is not a valid positive millisecond value — \
                     falling back to the default ({DEFAULT_MIC_JITTER_MS} ms)"
                );
                DEFAULT_MIC_JITTER_MS
            }
        },
    }
}

/// Resolve the FEC mode: `QUASAR_FEC_MODE`, else derived from
/// `QUASAR_FEC_PERCENTAGE` (`0` ⇒ `Off`, `>0` ⇒ `Fixed`). An empty-after-trim value
/// falls through silently, exactly like absent (docker-compose forwards an unset host
/// var as `=""`); an unrecognised non-empty value warns, then derives.
fn resolve_fec_mode(percentage: u32) -> fec::FecMode {
    let derived = if percentage > 0 {
        fec::FecMode::Fixed
    } else {
        fec::FecMode::Off
    };
    let raw = std::env::var("QUASAR_FEC_MODE").ok();
    match fec::FecMode::classify_env(raw.as_deref()) {
        fec::FecModeEnv::Recognised(m) => m,
        fec::FecModeEnv::UnsetOrEmpty => derived,
        fec::FecModeEnv::Unrecognised => {
            tracing::warn!(
                token = "knob-invalid-fec-mode",
                "QUASAR_FEC_MODE={:?} is not a recognised value (off|fixed|auto); \
                 deriving from QUASAR_FEC_PERCENTAGE ({})",
                raw.as_deref().unwrap_or_default(),
                derived.as_str(),
            );
            derived
        }
    }
}

/// Parse an `f64` env var, keeping only finite, strictly-positive values.
fn env_f64(var: &str, default: f64) -> f64 {
    std::env::var(var)
        .ok()
        .and_then(|s| s.parse::<f64>().ok())
        .filter(|n| n.is_finite() && *n > 0.0)
        .unwrap_or(default)
}

/// Parse a boolean-ish env var: `"1"`/`"true"`/`"TRUE"` ⇒ true, anything else
/// (including unset) ⇒ false.
pub(crate) fn env_bool(var: &str) -> bool {
    matches!(
        std::env::var(var).ok().as_deref(),
        Some("1") | Some("true") | Some("TRUE")
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    // ---- Codec parse / resolve ----

    #[test]
    fn codec_parse_accepts_legal_values_case_insensitive() {
        assert_eq!(Codec::parse("h264").unwrap(), Codec::H264);
        assert_eq!(Codec::parse("H264").unwrap(), Codec::H264);
        assert_eq!(Codec::parse("avc").unwrap(), Codec::H264);
        assert_eq!(Codec::parse("h265").unwrap(), Codec::H265);
        assert_eq!(Codec::parse("HEVC").unwrap(), Codec::H265);
        assert_eq!(Codec::parse(" av1 ").unwrap(), Codec::Av1);
    }

    // An unknown string must error, never fall back to h264 (that desyncs
    // sessions.codec from what the agent produces).
    #[test]
    fn codec_parse_rejects_unknown() {
        assert!(Codec::parse("vp9").is_err());
        assert!(Codec::parse("").is_err());
        assert!(Codec::parse("h266").is_err());
    }

    #[test]
    fn codec_as_str_roundtrips() {
        for c in [Codec::H264, Codec::H265, Codec::Av1] {
            assert_eq!(Codec::parse(c.as_str()).unwrap(), c);
        }
    }

    // Env is process-global; serialize + save/restore.
    #[test]
    fn codec_resolve_env_override_and_wire() {
        const VAR: &str = "QUASAR_CODEC";
        let prior = std::env::var(VAR).ok();

        std::env::remove_var(VAR);
        assert_eq!(Codec::resolve(None).unwrap(), Codec::H264, "absent ⇒ h264");
        assert_eq!(
            Codec::resolve(Some("av1")).unwrap(),
            Codec::Av1,
            "wire value used when no override"
        );
        assert!(
            Codec::resolve(Some("bogus")).is_err(),
            "unknown wire value errors"
        );

        std::env::set_var(VAR, "h265");
        assert_eq!(
            Codec::resolve(Some("av1")).unwrap(),
            Codec::H265,
            "QUASAR_CODEC force override beats the wire value"
        );
        std::env::set_var(VAR, "");
        assert_eq!(
            Codec::resolve(Some("av1")).unwrap(),
            Codec::Av1,
            "empty QUASAR_CODEC is treated as unset (wire wins)"
        );
        std::env::set_var(VAR, "junk");
        assert!(
            Codec::resolve(None).is_err(),
            "a junk QUASAR_CODEC force errors"
        );

        // apply_codec_env_override: same precedence as resolve, but it preserves the
        // assigned codec in `configured_codec`, records `codec_env_override` on a real
        // divergence, and ignores junk instead of erroring.
        let assigned_h264 = || {
            let mut stream = stream_with_floor(0);
            stream.codec = Codec::H264;
            SessionConfig::for_assignment_with(&settings::RuntimeSettings::baseline(), stream, None)
        };

        std::env::remove_var(VAR);
        let mut cfg = assigned_h264();
        cfg.apply_codec_env_override();
        assert_eq!(cfg.stream.codec, Codec::H264);
        assert!(!cfg.codec_env_override, "unset ⇒ no override");

        std::env::set_var(VAR, "h264");
        let mut cfg = assigned_h264();
        cfg.apply_codec_env_override();
        assert!(
            !cfg.codec_env_override,
            "env == assigned ⇒ no divergence recorded"
        );

        std::env::set_var(VAR, "av1");
        let mut cfg = assigned_h264();
        cfg.apply_codec_env_override();
        assert_eq!(cfg.stream.codec, Codec::Av1, "force override streams av1");
        assert_eq!(
            cfg.configured_codec,
            Codec::H264,
            "assigned codec preserved for configured.codec in the snapshot"
        );
        assert!(
            cfg.codec_env_override,
            "divergence recorded for the snapshot"
        );

        std::env::set_var(VAR, "vp9");
        let mut cfg = assigned_h264();
        cfg.apply_codec_env_override();
        assert_eq!(
            cfg.stream.codec,
            Codec::H264,
            "junk QUASAR_CODEC is ignored, not fatal, and does not disturb the assignment"
        );
        assert!(!cfg.codec_env_override);

        match prior {
            Some(v) => std::env::set_var(VAR, v),
            None => std::env::remove_var(VAR),
        }
    }

    // Serialized in one test: std::env::set_var is not thread-safe against concurrent
    // test readers.
    #[test]
    fn fec_percentage_env_parsing() {
        const VAR: &str = "QUASAR_FEC_PERCENTAGE";
        let prior = std::env::var(VAR).ok();

        std::env::remove_var(VAR);
        assert_eq!(env_fec_percentage(VAR), 0, "unset ⇒ 0 (disabled)");

        std::env::set_var(VAR, "20");
        assert_eq!(env_fec_percentage(VAR), 20, "\"20\" ⇒ 20");

        std::env::set_var(VAR, "junk");
        assert_eq!(env_fec_percentage(VAR), 0, "unparseable ⇒ 0 with warn");

        std::env::set_var(VAR, "150");
        assert_eq!(
            env_fec_percentage(VAR),
            0,
            "out-of-range (>100) ⇒ 0 with warn"
        );

        std::env::set_var(VAR, "-5");
        assert_eq!(
            env_fec_percentage(VAR),
            0,
            "negative ⇒ 0 with warn (u32 parse fails)"
        );

        std::env::set_var(VAR, "0");
        assert_eq!(env_fec_percentage(VAR), 0, "explicit 0 ⇒ 0 (disabled)");

        std::env::set_var(VAR, "100");
        assert_eq!(env_fec_percentage(VAR), 100, "100 is the max valid value");

        match prior {
            Some(v) => std::env::set_var(VAR, v),
            None => std::env::remove_var(VAR),
        }
    }

    // Serialized in one test, same reasoning as fec_percentage_env_parsing above.
    #[test]
    fn mic_jitter_ms_env_parsing() {
        const VAR: &str = "QUASAR_MIC_JITTER_MS";
        let prior = std::env::var(VAR).ok();

        std::env::remove_var(VAR);
        assert_eq!(env_mic_jitter_ms(VAR), 60, "unset ⇒ 60 ms default");

        std::env::set_var(VAR, "");
        assert_eq!(env_mic_jitter_ms(VAR), 60, "empty ⇒ 60 ms default");

        std::env::set_var(VAR, "75");
        assert_eq!(env_mic_jitter_ms(VAR), 75, "\"75\" ⇒ 75");

        std::env::set_var(VAR, "50");
        assert_eq!(env_mic_jitter_ms(VAR), 50, "\"50\" ⇒ 50");

        std::env::set_var(VAR, "junk");
        assert_eq!(
            env_mic_jitter_ms(VAR),
            60,
            "unparseable ⇒ default with warn"
        );

        std::env::set_var(VAR, "-5");
        assert_eq!(
            env_mic_jitter_ms(VAR),
            60,
            "negative ⇒ default with warn (u32 parse fails)"
        );

        std::env::set_var(VAR, "0");
        assert_eq!(
            env_mic_jitter_ms(VAR),
            60,
            "explicit 0 ⇒ default with warn (a 0 ms jitter buffer defeats its purpose)"
        );

        // Not clamped to the 50-75 ms target: a value outside it is an operator's
        // experiment, not junk.
        std::env::set_var(VAR, "200");
        assert_eq!(
            env_mic_jitter_ms(VAR),
            200,
            "any positive value is accepted, including outside the suggested range"
        );

        match prior {
            Some(v) => std::env::set_var(VAR, v),
            None => std::env::remove_var(VAR),
        }
    }

    /// A `StreamParams` at a 10 Mbps ceiling with an explicit wire ABR floor (`0` ⇒ unset).
    fn stream_with_floor(wire_floor_kbps: u32) -> StreamParams {
        StreamParams {
            width: 1920,
            height: 1080,
            fps: 60,
            bitrate_kbps: 10000,
            h264_profile: PROFILE_CONSTRAINED_BASELINE.to_string(),
            codec: Codec::H264,
            abr_floor_kbps: wire_floor_kbps,
            mic: false,
        }
    }

    /// A baseline `RuntimeSettings` with ABR armed and a known ratio, plus an optional
    /// `QUASAR_ABR_FLOOR_KBPS`-equivalent floor.
    fn abr_settings(env_floor: Option<u32>, ratio: f64) -> settings::RuntimeSettings {
        let mut s = settings::RuntimeSettings::baseline();
        s.abr_mode = AbrMode::Protective;
        s.abr_floor_kbps = env_floor;
        s.abr_floor_ratio = ratio;
        s
    }

    // A trimmed-empty QUASAR_ABR_MODE must classify exactly as unset: silent
    // fall-through, no WARN.
    #[test]
    fn abr_mode_empty_env_classifies_as_unset() {
        assert_eq!(AbrMode::classify_env(None), AbrModeEnv::UnsetOrEmpty);
        assert_eq!(AbrMode::classify_env(Some("")), AbrModeEnv::UnsetOrEmpty);
        assert_eq!(AbrMode::classify_env(Some("   ")), AbrModeEnv::UnsetOrEmpty);
    }

    // A recognised value (case-insensitive, whitespace-trimmed) resolves to its mode.
    #[test]
    fn abr_mode_recognised_env_classifies_as_mode() {
        assert_eq!(
            AbrMode::classify_env(Some("smooth")),
            AbrModeEnv::Recognised(AbrMode::Smooth)
        );
        assert_eq!(
            AbrMode::classify_env(Some(" Protective ")),
            AbrModeEnv::Recognised(AbrMode::Protective)
        );
        assert_eq!(
            AbrMode::classify_env(Some("OFF")),
            AbrModeEnv::Recognised(AbrMode::Off)
        );
    }

    // A genuinely bad value still takes the WARN branch (distinct from empty).
    #[test]
    fn abr_mode_bogus_env_classifies_as_unrecognised() {
        assert_eq!(
            AbrMode::classify_env(Some("bogus")),
            AbrModeEnv::Unrecognised
        );
    }

    // The wire profile floor takes precedence over both the env floor and the ratio.
    #[test]
    fn abr_config_uses_explicit_profile_floor_from_wire() {
        let cfg = SessionConfig::for_assignment_with(
            &abr_settings(Some(2500), 0.3),
            stream_with_floor(4000),
            None,
        );
        let abr = cfg.abr_config().expect("ABR armed");
        assert_eq!(abr.floor_kbps, 4000, "wire profile floor must win");
        assert_eq!(abr.ceiling_kbps, 10000, "ceiling stays the nominal bitrate");
    }

    // No wire floor (0) ⇒ the agent-local env floor.
    #[test]
    fn abr_config_falls_back_to_env_floor_when_wire_absent() {
        let cfg = SessionConfig::for_assignment_with(
            &abr_settings(Some(2500), 0.3),
            stream_with_floor(0),
            None,
        );
        let abr = cfg.abr_config().expect("ABR armed");
        assert_eq!(abr.floor_kbps, 2500, "env floor applies when wire is unset");
    }

    // Neither wire nor env floor ⇒ ceiling × ratio.
    #[test]
    fn abr_config_falls_back_to_ratio_when_wire_and_env_absent() {
        let cfg = SessionConfig::for_assignment_with(
            &abr_settings(None, 0.3),
            stream_with_floor(0),
            None,
        );
        let abr = cfg.abr_config().expect("ABR armed");
        assert_eq!(abr.floor_kbps, 3000, "ratio floor = 10000 × 0.3");
    }

    // `ladder_config()` reads the process-global `QUASAR_ABR_LADDER*` vars, so every
    // case runs inside ONE serialized test that saves/restores each var it touches
    // (there is no `serial_test` dep in this crate).

    /// A config whose `abr_mode` is set explicitly (no env), so only the
    /// `QUASAR_ABR_LADDER*` vars gate `ladder_config()`.
    fn cfg_with_mode(mode: AbrMode) -> SessionConfig {
        let mut s = settings::RuntimeSettings::baseline();
        s.abr_mode = mode;
        SessionConfig::for_assignment_with(&s, stream_with_floor(0), None)
    }

    /// Restore an env var to its prior value (unset if it was absent).
    fn restore(key: &str, prior: Option<String>) {
        match prior {
            Some(v) => std::env::set_var(key, v),
            None => std::env::remove_var(key),
        }
    }

    #[test]
    fn ladder_config_env_gating() {
        let keys = [
            "QUASAR_ABR_LADDER",
            "QUASAR_ABR_LADDER_FPS",
            "QUASAR_ABR_LADDER_RESOLUTION",
        ];
        let saved: Vec<(&str, Option<String>)> =
            keys.iter().map(|k| (*k, std::env::var(k).ok())).collect();
        // Clean slate.
        for k in &keys {
            std::env::remove_var(k);
        }

        // Smooth + all ladder vars unset → Some(default ladder config).
        let lc = cfg_with_mode(AbrMode::Smooth)
            .ladder_config()
            .expect("Smooth + unset ⇒ Some(default)");
        assert_eq!(lc.max_bias, ladder::LadderConfig::DEFAULT_MAX_BIAS);
        assert!(!lc.fps_enabled, "fps rung defaults OFF");
        assert!(!lc.resolution_enabled, "resolution rung defaults OFF");

        // QUASAR_ABR_LADDER=0 with no other rung armed ⇒ nothing to build → None.
        std::env::set_var("QUASAR_ABR_LADDER", "0");
        assert!(
            cfg_with_mode(AbrMode::Smooth).ladder_config().is_none(),
            "QUASAR_ABR_LADDER=0 + no rung ⇒ None"
        );
        std::env::set_var("QUASAR_ABR_LADDER", "false");
        assert!(cfg_with_mode(AbrMode::Smooth).ladder_config().is_none());

        // #502: with a rung armed, the master flag does not disarm it; the config is
        // built and only the speed-bias rung is retired (max_bias == 0).
        std::env::set_var("QUASAR_ABR_LADDER_RESOLUTION", "1");
        let lc = cfg_with_mode(AbrMode::Smooth)
            .ladder_config()
            .expect("QUASAR_ABR_LADDER=0 + resolution rung on ⇒ Some");
        assert_eq!(lc.max_bias, 0, "master flag off ⇒ speed-bias rung inert");
        assert!(
            lc.resolution_enabled,
            "resolution rung survives the master flag"
        );
        std::env::remove_var("QUASAR_ABR_LADDER_RESOLUTION");
        std::env::remove_var("QUASAR_ABR_LADDER");

        // Protective never engages the ladder regardless of env → None.
        assert!(
            cfg_with_mode(AbrMode::Protective).ladder_config().is_none(),
            "Protective ⇒ None"
        );
        // Off never engages the ladder → None.
        assert!(
            cfg_with_mode(AbrMode::Off).ladder_config().is_none(),
            "Off ⇒ None"
        );

        // fps / resolution env flags map onto the config fields (Smooth, ladder on).
        std::env::set_var("QUASAR_ABR_LADDER_FPS", "1");
        std::env::set_var("QUASAR_ABR_LADDER_RESOLUTION", "true");
        let lc = cfg_with_mode(AbrMode::Smooth)
            .ladder_config()
            .expect("Smooth + ladder on ⇒ Some");
        assert!(lc.fps_enabled, "QUASAR_ABR_LADDER_FPS=1 ⇒ fps_enabled");
        assert!(
            lc.resolution_enabled,
            "QUASAR_ABR_LADDER_RESOLUTION=true ⇒ resolution_enabled"
        );
        std::env::remove_var("QUASAR_ABR_LADDER_FPS");
        std::env::remove_var("QUASAR_ABR_LADDER_RESOLUTION");

        // The Protective/Off short-circuit wins even with fps/resolution flags set.
        std::env::set_var("QUASAR_ABR_LADDER_FPS", "1");
        assert!(
            cfg_with_mode(AbrMode::Protective).ladder_config().is_none(),
            "mode gate precedes the fps/resolution flags"
        );

        // Restore every var to its pre-test value.
        for (k, prior) in saved {
            restore(k, prior);
        }
    }

    // Ladder settings are snapshotted at assign time: a later config_update cannot
    // retune a running session's ladder.
    #[test]
    fn ladder_config_comes_from_the_settings_snapshot_not_env() {
        let mut settings = settings::RuntimeSettings::baseline();
        settings.abr_mode = AbrMode::Smooth;
        settings.ladder.resolution_enabled = true;
        settings.ladder.res.min_height = 1080;
        let cfg = SessionConfig::for_assignment_with(&settings, StreamParams::default(), None);
        let lc = cfg.ladder_config().expect("smooth + ladder on ⇒ Some");
        assert!(lc.resolution_enabled);
        assert_eq!(lc.res.min_height, 1080);
        assert_eq!(cfg.ladder.res.min_height, 1080);
    }

    /// The master flag off with no other rung armed is the only "no ladder at all" case.
    #[test]
    fn ladder_config_is_none_when_no_rung_is_armed() {
        let mut settings = settings::RuntimeSettings::baseline();
        settings.abr_mode = AbrMode::Smooth;
        settings.ladder.enabled = false;
        settings.ladder.resolution_enabled = false;
        settings.ladder.fps_enabled = false;
        let cfg = SessionConfig::for_assignment_with(&settings, StreamParams::default(), None);
        assert!(cfg.ladder_config().is_none());
    }

    /// #502: `abr_ladder=false` retires the speed-bias rung only. A host that armed the
    /// resolution (or fps) rung keeps it, whatever the master flag says.
    #[test]
    fn a_rung_survives_the_master_flag_being_off() {
        for (res, fps) in [(true, false), (false, true), (true, true)] {
            let mut settings = settings::RuntimeSettings::baseline();
            settings.abr_mode = AbrMode::Smooth;
            settings.ladder.enabled = false;
            settings.ladder.resolution_enabled = res;
            settings.ladder.fps_enabled = fps;
            let cfg = SessionConfig::for_assignment_with(&settings, StreamParams::default(), None);
            let lc = cfg
                .ladder_config()
                .unwrap_or_else(|| panic!("res={res} fps={fps} with abr_ladder=false ⇒ Some"));
            assert_eq!(lc.max_bias, 0, "the speed-bias rung stays retired");
            assert_eq!(lc.resolution_enabled, res);
            assert_eq!(lc.fps_enabled, fps);
        }
    }

    /// The master flag ON passes `max_bias` through untouched. Every field is set
    /// explicitly rather than left to `baseline()`'s env read, so the serialized env
    /// test above cannot perturb it.
    #[test]
    fn the_master_flag_on_is_unchanged() {
        let mut settings = settings::RuntimeSettings::baseline();
        settings.abr_mode = AbrMode::Smooth;
        settings.ladder.enabled = true;
        settings.ladder.max_bias = 3;
        settings.ladder.resolution_enabled = false;
        settings.ladder.fps_enabled = false;
        let cfg = SessionConfig::for_assignment_with(&settings, StreamParams::default(), None);
        let lc = cfg.ladder_config().expect("smooth + master on ⇒ Some");
        assert_eq!(lc.max_bias, 3, "the operator's max_bias is passed through");
        assert!(!lc.resolution_enabled);
        assert!(!lc.fps_enabled);
    }

    /// The mode gate must speak, and only when a rung is actually armed.
    #[test]
    fn ladder_gate_warning_covers_the_mode_gate_only() {
        let armed = |mode: AbrMode, res: bool, fps: bool| {
            let mut s = settings::RuntimeSettings::baseline();
            s.abr_mode = mode;
            s.ladder.resolution_enabled = res;
            s.ladder.fps_enabled = fps;
            SessionConfig::for_assignment_with(&s, StreamParams::default(), None)
        };
        // Nothing armed, or Smooth: silence.
        assert!(armed(AbrMode::Protective, false, false)
            .ladder_gate_warning()
            .is_none());
        assert!(armed(AbrMode::Smooth, true, true)
            .ladder_gate_warning()
            .is_none());
        // Armed in a mode that cannot run it: one warning naming rung and mode.
        for (mode, res, fps, needle) in [
            (AbrMode::Protective, true, false, "abr_ladder_resolution is"),
            (AbrMode::Off, false, true, "abr_ladder_fps is"),
            (
                AbrMode::Protective,
                true,
                true,
                "abr_ladder_resolution + abr_ladder_fps are",
            ),
        ] {
            let w = armed(mode, res, fps)
                .ladder_gate_warning()
                .expect("armed rung + non-smooth mode ⇒ a warning");
            assert!(w.contains(needle), "{w}");
            assert!(w.contains(mode.as_str()), "{w}");
        }
    }
}
