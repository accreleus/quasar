use serde::{Deserialize, Serialize};

use crate::vram::VramSample;

/// Host-stage latency probe stage aggregates, flattened onto `session_metrics`.
/// Present only when the probe is enabled and that stage sampled in the window:
/// absent means "not measured", never zero. Observability only — no
/// control-plane behaviour may key on these.
/// `docs/superpowers/specs/2026-08-18-latency-probe-design.md`.
#[derive(Serialize, Debug, Default, Clone, PartialEq)]
pub struct LatencyProbeStages {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe_capture_to_enc_in_p50_ms: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe_capture_to_enc_in_p95_ms: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe_enc_out_to_send_p50_ms: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe_enc_out_to_send_p95_ms: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe_pay_to_send_p50_ms: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe_pay_to_send_p95_ms: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe_pts_to_emit_p50_ms: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe_pts_to_emit_p95_ms: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe_compositor_frame_interval_p95_ms: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe_send_desyncs: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe_pts_unmatched: Option<f64>,
}

impl LatencyProbeStages {
    /// `None` when the probe contributed nothing this window, so a probe-off
    /// session's `session_metrics` is byte-identical to a pre-probe agent's.
    pub fn from_window(w: &crate::session::metrics::MetricsWindow) -> Option<Box<Self>> {
        let stages = LatencyProbeStages {
            probe_capture_to_enc_in_p50_ms: w.probe_capture_to_enc_in_p50_ms,
            probe_capture_to_enc_in_p95_ms: w.probe_capture_to_enc_in_p95_ms,
            probe_enc_out_to_send_p50_ms: w.probe_enc_out_to_send_p50_ms,
            probe_enc_out_to_send_p95_ms: w.probe_enc_out_to_send_p95_ms,
            probe_pay_to_send_p50_ms: w.probe_pay_to_send_p50_ms,
            probe_pay_to_send_p95_ms: w.probe_pay_to_send_p95_ms,
            probe_pts_to_emit_p50_ms: w.probe_pts_to_emit_p50_ms,
            probe_pts_to_emit_p95_ms: w.probe_pts_to_emit_p95_ms,
            probe_compositor_frame_interval_p95_ms: w.probe_compositor_frame_interval_p95_ms,
            probe_send_desyncs: w.probe_send_desyncs,
            probe_pts_unmatched: w.probe_pts_unmatched,
        };
        (stages != LatencyProbeStages::default()).then(|| Box::new(stages))
    }
}

/// Messages sent from the node agent to the control plane.
// AgentMsg derives Serialize only, so several fields (e.g. `error` on
// Ack/SessionState) are written for the wire but never read back by Rust,
// which rustc's dead_code lint flags even though serde serializes them.
#[allow(dead_code)]
#[derive(Serialize, Debug)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum AgentMsg {
    Register {
        node_name: String,
        agent_version: String,
        auth: Auth,
        /// The managed images this agent actually has. Always serialized as
        /// `[]` when empty — omission means pre-amendment agent, and the
        /// control plane only demotes stale `ready` rows when present. Losing
        /// the state file must still report `[]`, or the host reads falsely
        /// `ready` forever (agent-api.md `register`).
        images: Vec<RegisterImageEntry>,
    },
    Capacity {
        host: HostCapacity,
        gpus: Vec<GpuCapacity>,
        gpu_detection: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        gpu_detection_reason: Option<String>,
        /// CM-01: console-mode capabilities. Additive — absent means
        /// "no capabilities reported" (older agent).
        #[serde(skip_serializing_if = "Option::is_none")]
        console_capabilities: Option<ConsoleCapabilities>,
        /// The agent's resolved runtime settings (env <- config_update overlay),
        /// stringified per hostcfg key. Additive — absent keeps the control
        /// plane's `effective` view null (agent-api.md `capacity`).
        #[serde(skip_serializing_if = "Option::is_none")]
        effective_settings: Option<std::collections::BTreeMap<String, String>>,
        /// The codec set the active encoder path can produce (encoder element
        /// found AND payloader registered). Additive — absent ⇒ `["h264"]`
        /// assumed. agent-api §3.1.2.
        #[serde(skip_serializing_if = "Option::is_none")]
        codecs: Option<Vec<String>>,
        /// #506: per-codec sustained encode throughput hint, keyed like `codecs`.
        /// Additive — absent keeps the control plane's last-stored value; a
        /// codec missing from the map is UNKNOWN and gates nothing, so this is
        /// normally a SUBSET of `codecs` (only measured elements appear, see
        /// `pipeline::encoders::element_pixel_rate_mpix_s`). agent-api.md
        /// `capacity.codec_throughput`.
        #[serde(skip_serializing_if = "Option::is_none")]
        codec_throughput: Option<std::collections::BTreeMap<String, CodecThroughput>>,
        /// Host readiness check set (`readiness::probe`), repeated in every
        /// capacity report on the connection. Additive, keep-if-absent like
        /// `effective_settings`. ADVISORY ONLY: a failing check never blocks
        /// registration, scheduling or a session (agent-api.md `capacity`).
        #[serde(skip_serializing_if = "Option::is_none")]
        readiness: Option<Vec<ReadinessCheck>>,
    },
    Heartbeat {
        running_sessions: Vec<String>,
        ts_unix_ms: i64,
        /// #383: cached live-VRAM sample per detected GPU (fields omitted, not
        /// null, when unavailable). Additive — absent ⇒ unknown telemetry, the
        /// admission veto abstains (spec §3.2/§4.1). Filled by the sampler off
        /// the control task; the heartbeat tick never blocks on a sample.
        #[serde(skip_serializing_if = "Option::is_none")]
        gpu_vram: Option<Vec<VramSample>>,
    },
    Ack {
        id: String,
        ok: bool,
        #[serde(skip_serializing_if = "Option::is_none")]
        error: Option<String>,
    },
    /// P1-6 lifecycle callback: authoritative session progress reported to the
    /// control plane (starting → running → stopping → stopped, or failed).
    SessionState {
        session_id: String,
        state: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        detail: Option<String>,
        #[serde(skip_serializing_if = "Option::is_none")]
        error: Option<String>,
        /// Machine-readable terminal-failure classification, e.g.
        /// `"app_exited_early"` (#463: app container exited before presenting a
        /// frame). Additive, absent on every other failure. NEVER load-bearing
        /// for lifecycle: the state machine reads `state` alone.
        #[serde(skip_serializing_if = "Option::is_none")]
        reason_code: Option<String>,
        /// App container log tail (newest last, bounded — see
        /// `session::container::APP_LOG_TAIL_LINES`), present only alongside a
        /// `reason_code` that warrants it — otherwise the decisive lines die
        /// with the `--rm` container and are unrecoverable.
        #[serde(skip_serializing_if = "Option::is_none")]
        app_log_tail: Option<String>,
    },
    /// P1-7 signaling relay: wrap a Phase 0 inner message (offer/ice/…) for the
    /// control-plane relay, which unwraps and forwards it to the browser.
    Signaling {
        session_id: String,
        msg: serde_json::Value,
    },
    /// ST-03 per-session trace event: a discrete host-side marker emitted when the
    /// event happens (event-driven, not heartbeat-cadence). Fire-and-forget like
    /// `session_metrics`; carries no session-state authority. The control plane
    /// validates host ownership and writes a source='agent' row.
    SessionTraceEvent {
        session_id: String,
        ts_unix_ms: i64,
        /// trace-format.md §3.2 type: "abr.retarget" | "pipeline.source_swapped" |
        /// "encoder.drop_detected" | "webrtc.state_changed".
        event: String,
        /// Per-type payload; empty object when absent.
        payload: serde_json::Value,
    },
    /// P4-03 per-session telemetry: host-observable encode numbers, emitted once
    /// per running session on the heartbeat cadence (agent-api.md `session_metrics`).
    /// Fire-and-forget like `heartbeat`; carries no session-state authority.
    SessionMetrics {
        session_id: String,
        ts_unix_ms: i64,
        window_ms: u64,
        fps: f64,
        bitrate_kbps: f64,
        #[serde(skip_serializing_if = "Option::is_none")]
        encode_ms: Option<f64>,
        #[serde(skip_serializing_if = "Option::is_none")]
        encode_ms_p50: Option<f64>,
        #[serde(skip_serializing_if = "Option::is_none")]
        encode_ms_p95: Option<f64>,
        /// Worst single encode in the window (ms) — the only key that can show a
        /// one-frame stall: p95 over 5s at 60fps averages a 200ms hiccup away.
        #[serde(skip_serializing_if = "Option::is_none")]
        encode_ms_max: Option<f64>,
        source_fps: f64,
        compositor_fps: f64,
        #[serde(skip_serializing_if = "Option::is_none")]
        compositor_pts_delta_p50_ms: Option<f64>,
        #[serde(skip_serializing_if = "Option::is_none")]
        compositor_pts_delta_p95_ms: Option<f64>,
        interpipe_queue_level_max: u64,
        #[serde(skip_serializing_if = "Option::is_none")]
        interpipe_queue_dwell_p50_ms: Option<f64>,
        #[serde(skip_serializing_if = "Option::is_none")]
        interpipe_queue_dwell_p95_ms: Option<f64>,
        interpipe_queue_drops: u64,
        rtp_fps: f64,
        rtp_bitrate_kbps: f64,
        frames_encoded: u64,
        frames_dropped: u64,
        /// In-session ABR governor's current CBR setpoint (kbit/s). Present only
        /// when ABR is armed — absent ⇒ static CBR.
        #[serde(skip_serializing_if = "Option::is_none")]
        abr_setpoint_kbps: Option<f64>,
        /// Governor's CURRENT floor (kbit/s), present only once the ladder moves
        /// it off the launch floor. Absent ⇒ "the launch floor", never "unknown".
        #[serde(skip_serializing_if = "Option::is_none")]
        abr_floor_kbps: Option<f64>,
        /// Raw rtpgccbwe GCC estimate (kbit/s) BEFORE governor smoothing.
        /// Present only once ABR is armed and has received an estimate.
        #[serde(skip_serializing_if = "Option::is_none")]
        gcc_estimate_kbps: Option<f64>,
        /// Active ABR mode ("protective" | "off"). Always present — deriving it
        /// from setpoint presence was wrong for "off" (rtpgccbwe stays attached).
        abr_mode: &'static str,
        /// Live in-agent adaptation classifier label ("healthy" |
        /// "network_congested" | "encoder_saturated" | "unknown"). Always
        /// present, signal-only. `client_presentation_limited` is intentionally
        /// absent: it needs browser data the agent never sees (invariant #1).
        adaptation_state: &'static str,
        /// Host-stage latency probe (`QUASAR_LATENCY_PROBE`, default off).
        /// Flattened onto the top level per
        /// `docs/superpowers/specs/2026-08-18-latency-probe-design.md` §8. Boxed
        /// so a probe-off session costs one pointer, not nine `Option<f64>`.
        #[serde(flatten, skip_serializing_if = "Option::is_none")]
        latency_probe: Option<Box<LatencyProbeStages>>,

        /// Post-session home-dir bytes used, emitted once just before stopped;
        /// the control plane updates `user_homes.bytes_used` on receipt. Absent
        /// when the home root is empty/invalid (`session::home::measure_home_dirs`).
        #[serde(skip_serializing_if = "Option::is_none")]
        bytes_used: Option<u64>,
        /// Compositor's CURRENT app-facing `wl_output` mode and `preferred_scale`.
        /// Present only off default — see [`Reported`](crate::session::echo::Reported)
        /// for the rule every echo key on this message follows. The only
        /// authoritative readback of live render resolution / UI scale.
        #[serde(skip_serializing_if = "Option::is_none")]
        render_width: Option<i32>,
        #[serde(skip_serializing_if = "Option::is_none")]
        render_height: Option<i32>,
        #[serde(skip_serializing_if = "Option::is_none")]
        ui_scale: Option<f64>,
        /// Session's CURRENT external (encoded) size, read back from negotiated
        /// caps. Present only when off the launch size — the only authoritative
        /// readback of the live external size.
        #[serde(skip_serializing_if = "Option::is_none")]
        stream_width: Option<i32>,
        #[serde(skip_serializing_if = "Option::is_none")]
        stream_height: Option<i32>,
        /// Whether this session's encode path can change external resolution
        /// live (false on Vulkan — no in-graph scaler — and on local-only
        /// console sessions with no encode pipeline). Always `Some` from this
        /// agent; `Option` only keeps the field skippable across agent/CP
        /// version skew. Absent means unknown, never false.
        #[serde(skip_serializing_if = "Option::is_none")]
        external_resize_supported: Option<bool>,
        /// Ladder's current encoder speed-bias rung. Present only when non-zero.
        #[serde(skip_serializing_if = "Option::is_none")]
        ladder_speed_bias: Option<i32>,
        /// Ladder's current external-resolution rung index (0 = launch).
        /// Present only when non-zero.
        #[serde(skip_serializing_if = "Option::is_none")]
        ladder_res_rung: Option<i32>,
        /// Encoded frame rate the fps rung currently asks for. Present only
        /// below the launch rate.
        #[serde(skip_serializing_if = "Option::is_none")]
        ladder_fps: Option<i32>,
        /// Who owns the external size — `"auto"` (ladder) or `"pinned"` (user/
        /// admin PATCH). Present only alongside `stream_width`/`stream_height`.
        #[serde(skip_serializing_if = "Option::is_none")]
        external_owner: Option<&'static str>,
    },
    /// Image-management P2: pull/remove progress for a managed catalog image on
    /// this host (agent-api.md `image_state`). Fire-and-forget like
    /// `session_metrics` — no ack, image-presence authority only, never touches
    /// sessions. Sent on every state transition and throttled during a pull (at
    /// most every 2s or ≥5% progress delta, whichever is coarser).
    ImageState {
        image_id: String,
        version: String,
        /// `"absent" | "pulling" | "ready" | "failed"`.
        state: String,
        /// 0-100; best-effort, meaningful only while `state == "pulling"`.
        progress_pct: u8,
        /// Downloaded-so-far while pulling; the image's on-disk size (when
        /// cheaply known) on `ready`; 0 otherwise.
        bytes: u64,
        /// Non-empty only when `state == "failed"`; a short operator-readable
        /// cause, never a raw docker error blob (`images::errors`).
        error: String,
    },
}

impl AgentMsg {
    /// Build the `session_metrics` message for one drained telemetry window.
    /// One constructor for both callers (heartbeat drain and the pre-terminal
    /// drain) so a field can't reach the wire from one path and not the other;
    /// `bytes_used` is the only thing they legitimately differ on.
    pub fn session_metrics(
        session_id: String,
        ts_unix_ms: i64,
        w: &crate::session::metrics::MetricsWindow,
        bytes_used: Option<u64>,
    ) -> AgentMsg {
        AgentMsg::SessionMetrics {
            session_id,
            ts_unix_ms,
            window_ms: w.window_ms,
            fps: w.fps,
            bitrate_kbps: w.bitrate_kbps,
            encode_ms: w.encode_ms,
            encode_ms_p50: w.encode_ms_p50,
            encode_ms_p95: w.encode_ms_p95,
            encode_ms_max: w.encode_ms_max,
            source_fps: w.source_fps,
            compositor_fps: w.compositor_fps,
            compositor_pts_delta_p50_ms: w.compositor_pts_delta_p50_ms,
            compositor_pts_delta_p95_ms: w.compositor_pts_delta_p95_ms,
            interpipe_queue_level_max: w.interpipe_queue_level_max,
            interpipe_queue_dwell_p50_ms: w.interpipe_queue_dwell_p50_ms,
            interpipe_queue_dwell_p95_ms: w.interpipe_queue_dwell_p95_ms,
            interpipe_queue_drops: w.interpipe_queue_drops,
            rtp_fps: w.rtp_fps,
            rtp_bitrate_kbps: w.rtp_bitrate_kbps,
            frames_encoded: w.frames_encoded,
            frames_dropped: w.frames_dropped,
            latency_probe: LatencyProbeStages::from_window(w),
            abr_setpoint_kbps: w.abr_setpoint_kbps,
            abr_floor_kbps: w.abr_floor_kbps,
            gcc_estimate_kbps: w.gcc_estimate_kbps,
            abr_mode: w.abr_mode,
            adaptation_state: w.adaptation_state,
            bytes_used,
            render_width: w.render_width,
            render_height: w.render_height,
            ui_scale: w.ui_scale,
            stream_width: w.stream_width,
            stream_height: w.stream_height,
            external_resize_supported: w.external_resize_supported,
            ladder_speed_bias: w.ladder_speed_bias,
            ladder_res_rung: w.ladder_res_rung,
            ladder_fps: w.ladder_fps,
            external_owner: w.external_owner,
        }
    }
}

/// One entry of `register.images[]` — a managed image this agent has verified
/// against its docker daemon.
#[derive(Serialize, Debug, Clone)]
pub struct RegisterImageEntry {
    pub image_id: String,
    pub version: String,
    /// `"absent" | "pulling" | "ready" | "failed"`.
    pub state: String,
}

/// One codec's entry in `capacity.codec_throughput` (#506): sustained encode
/// throughput of the encoder element this host would actually build for it. An
/// OBJECT rather than a bare number so a future measured probe can add fields
/// (measured-at, confidence, GPU index) without a second contract amendment.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq)]
pub struct CodecThroughput {
    /// Megapixels/s the effective encoder element sustains at session settings.
    /// Always positive — a value the agent can't vouch for is omitted, not
    /// zero, since absent means unknown and present means gate on it.
    pub max_pixel_rate_mpix_s: f64,
}

/// One host-readiness check in `capacity.readiness[]`, produced by
/// [`crate::readiness::probe`].
///
/// Flat and stringly-typed on purpose: the control plane stores it opaquely
/// (`hosts.readiness` JSONB) and the admin UI renders it generically, so adding
/// a check is agent-only. `id` is the stable key the UI may special-case;
/// `summary`/`remediation` are operator-facing prose.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq)]
pub struct ReadinessCheck {
    /// Stable machine key, e.g. `"nvidia_egl_vendor_json"`.
    pub id: String,
    /// `"pass" | "fail" | "skip"`. `skip` means "not applicable to this host"
    /// (an NVIDIA check on an AMD box) — never "we could not tell".
    pub status: String,
    /// One sentence an operator can act on, in plain language.
    pub summary: String,
    /// Exact commands to fix it, distro-aware where cheaply knowable. Empty
    /// for `pass`/`skip`.
    pub remediation: String,
}

/// The auth credential in a `register` message.
#[derive(Serialize, Debug)]
#[serde(untagged)]
pub enum Auth {
    Enrollment { enrollment_token: String },
    Reconnect { node_secret: String },
}

#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct HostCapacity {
    pub cpu_cores: i32,
    pub mem_mb: i32,
    /// Filesystem capacity/availability (statvfs) of the storage roots the
    /// agent can see. Additive — absent keeps the control plane's last stored
    /// value (agent-api.md `capacity`).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub storage: Option<Vec<StorageVolume>>,
    /// CPU marketing name (`/proc/cpuinfo` `model name`). Additive — absent ⇒
    /// null on the API.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpu_model: Option<String>,
}

/// One statvfs'd storage root reported in `HostCapacity.storage`.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq)]
pub struct StorageVolume {
    pub label: String,
    pub path: String,
    pub total_mb: i64,
    pub available_mb: i64,
}

#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct GpuCapacity {
    pub index: i32,
    pub vendor: String,
    pub model: String,
    pub vram_mb_total: i32,
    pub encode_slots_total: i32,
    /// Stable render-node device path for this GPU, reboot-safe by-path form
    /// from its PCI address. Present only when a matching
    /// `/sys/class/drm/renderD*` entry exists. Additive — absent ⇒ null.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub render_node: Option<String>,
    /// Kernel device-node form for the same render node, e.g.
    /// `/dev/dri/renderD128`. Complements the by-path identity above.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub device_path: Option<String>,
}

/// Per-session stream parameters in a `session_assign` (mirrors the sessions
/// launch-param columns; drives the P1-5 pipeline).
#[derive(Deserialize, Debug, Clone)]
pub struct StreamSpec {
    pub width: i32,
    pub height: i32,
    pub fps: i32,
    pub bitrate_kbps: u32,
    pub h264_profile: String,
    /// Session video codec. Additive — omitted ⇒ H.264. `"h264" | "h265" |
    /// "av1"`; an unrecognised value must fail the assignment rather than
    /// silently produce a different codec than `sessions.codec` says.
    /// `h264_profile` is ignored for a non-h264 codec.
    #[serde(default)]
    pub codec: Option<String>,
    /// Selected stream profile's ABR floor (kbit/s). Additive — omitted/0 for
    /// legacy launches, falling back to `QUASAR_ABR_FLOOR_KBPS` else
    /// `ceiling × ratio`.
    #[serde(default)]
    pub abr_floor_kbps: u32,
    /// Microphone capture. When true, the agent adds a `recvonly` Opus
    /// transceiver to the audio webrtcbin before the first offer, decodes the
    /// client's mic track, and plays it into the sidecar's `quasar_mic` sink.
    /// Additive — absent/false ⇒ today's single-m-line audio offer.
    #[serde(default)]
    pub mic: bool,
}

/// The reserved budget the agent must not exceed.
#[derive(Deserialize, Debug, Clone)]
pub struct ResourceSpec {
    pub vram_mb: i32,
    pub encode_slots: i32,
}

/// The app to launch, mirrored from `apps.runtime_spec` (the `app` object of a
/// `session_assign` — agent-api.md). The agent pulls `image` on assign and runs
/// it as the per-session compositor's Wayland client on start.
#[derive(Deserialize, Debug, Clone, Default)]
pub struct AppSpec {
    #[serde(default)]
    pub image: String,
    #[serde(default)]
    pub args: Vec<String>,
    #[serde(default)]
    pub env: std::collections::BTreeMap<String, String>,
    #[serde(default)]
    pub mounts: Vec<String>,
    /// Whether the app needs GPU passthrough (`--device /dev/dri` / `--gpus`).
    #[serde(default)]
    pub gpu: bool,
    /// Additive `runtime_spec` knob: set `false` for images whose startup
    /// legitimately re-escalates — upstream GOW desktop images `sudo` in their
    /// startup scripts, which the default `--security-opt no-new-privileges`
    /// blocks, exiting 1 and streaming a black bare compositor. Absent ⇒ `true`.
    #[serde(default = "spec_true")]
    pub no_new_privileges: bool,
    /// Additive `runtime_spec` knob: policy for a steady-state app-container
    /// exit. Absent ⇒ `fail` — see [`AppExitPolicy`].
    #[serde(default)]
    pub on_app_exit: AppExitPolicy,
    /// Additive `runtime_spec` knob: docker network mode for the app container.
    /// A PER-APP requirement, not a host setting — containers default to
    /// `--network none`, but e.g. Steam's first boot must reach the internet
    /// or it clean-exits and the session dies (#463). Resolved by the control
    /// plane from `apps.runtime_spec.network` else the runtime preset's column.
    ///
    /// Absent/null/empty ⇒ the agent's host fallback chain
    /// (`QUASAR_CONTAINER_NETWORK`, else `none`).
    ///
    /// Wire values: `none` | `bridge` only — anything else FAILS the session
    /// rather than being handed to `docker run --network`, since this string
    /// becomes a CLI argument and e.g. `container:<id>` would join another
    /// container's namespace.
    ///
    /// `host` is NOT accepted here even though the agent's own
    /// `QUASAR_CONTAINER_NETWORK` knob permits it: this field is portable
    /// (can originate in a catalog manifest authored elsewhere), and `--network
    /// host` would dissolve the isolation boundary on every host that installs
    /// it. An operator can still choose host networking locally — see
    /// `session::container::HOST_CONTAINER_NETWORKS`.
    #[serde(default)]
    pub network: Option<String>,
    /// Additive `runtime_spec` knob: unmask `/proc` and `/sys` paths Docker's
    /// default `systempaths=masked` hides, via `--security-opt
    /// systempaths=unconfined`. Desktop-session images (KDE Plasma + user
    /// Flatpak) need this for `flatpak run` — Flatpak's `bwrap` sandbox mounts
    /// a fresh `/proc` that Docker's masked paths block even with
    /// `QUASAR_APP_SECCOMP=unconfined` already set (`flatpak install` works,
    /// `flatpak run` fails without this flag). Absent ⇒ `false`.
    #[serde(default)]
    pub systempaths_unconfined: bool,
}

fn spec_true() -> bool {
    true
}

/// Policy for a steady-state app-container exit — a dead app otherwise leaves
/// the compositor rendering a frozen last frame with no observable signal.
///   - `fail` (default): end the session.
///   - `keep`: log the exit and continue — e.g. a console/local_only desktop
///     process that legitimately exits 0 between launches (opt-in per catalog row).
///
/// No `return_to_launcher` variant yet (#39): an unhandled variant in wire data
/// must not silently deserialize into something no code path acts on.
///
/// A present-but-unrecognized wire value lands on `Unknown` via
/// `#[serde(other)]` rather than failing the whole `session_assign`
/// deserialize — a malformed value degrades fail-closed for this one knob
/// only. Every match site treats `Unknown` identically to `Fail`.
#[derive(Deserialize, Debug, Clone, Copy, Default, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum AppExitPolicy {
    #[default]
    Fail,
    Keep,
    /// Any other wire value — see the type doc. `serde(other)` discards the
    /// original string, so callers can only warn generically, not name it.
    #[serde(other)]
    Unknown,
}

/// CM-01 console-mode config, delivered in `config_update.console_config`. The
/// full resolved object; all fields `#[serde(default)]` so a partial/older
/// payload deserializes cleanly. `input_devices` stays opaque JSON (CM-03 consumes it).
#[derive(Deserialize, Debug, Clone)]
pub struct ConsoleConfig {
    #[serde(default)]
    pub enabled: bool,
    #[serde(default = "console_auto")]
    pub connector: String,
    #[serde(default)]
    pub output_id: Option<String>,
    #[serde(default)]
    pub mode: Option<ConsoleModeSelection>,
    #[serde(default = "console_weston")]
    pub compositor: String,
    #[serde(default)]
    pub audio_output: Option<String>,
    #[serde(default)]
    pub stream: bool,
    #[serde(default)]
    pub stream_audio: bool,
    #[serde(default)]
    pub input_devices: serde_json::Value,
    #[serde(default)]
    pub grab: bool,
    #[serde(default)]
    pub auto_start_on_display: bool,
    #[serde(default)]
    pub auto_connect_controller: bool,
    #[serde(default)]
    pub default_app: Option<String>,
    /// The control plane's designated console session owner. The agent never
    /// acts on this — carried only for lossless deserialization.
    #[serde(default)]
    pub default_user: Option<String>,
    #[serde(default)]
    pub fullscreen: bool,
}

#[derive(Deserialize, Debug, Clone)]
pub struct ConsoleModeSelection {
    pub width: u16,
    pub height: u16,
    pub refresh_millihz: u32,
}

/// Explicit per-session video output plan. This is assignment-scoped so a host
/// with console capability does not accidentally mirror every browser session.
#[derive(Deserialize, Debug, Clone, Copy, Default, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum VideoTopology {
    #[default]
    StreamOnly,
    LocalOnly,
    DualOutput,
}

// ── session_capture ──────────────────────────────────────────────────────────

/// Default compressed-payload cap for a capture: 256 KiB. The control plane
/// always sends `budget`, so this only covers a hand-rolled/older sender.
pub const CAPTURE_DEFAULT_MAX_BYTES: usize = 262_144;
/// Default wall-clock cap for a capture: 10 s.
pub const CAPTURE_DEFAULT_MAX_MS: u64 = 10_000;

/// What a `session_capture` asks the agent to observe.
///
/// Deserialized by hand rather than with `#[serde(other)]` because the string
/// matters: an unknown kind must be acked `unknown_kind` (→ 422), which needs
/// the name for the log line, and `serde(other)` discards it (same trap as
/// [`AppExitPolicy::Unknown`]).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum CaptureKind {
    /// The encode pipeline's graphviz dump (`CAPS_DETAILS | STATES` only).
    PipelineDot,
    /// Allow-listed encoder properties + negotiated caps, read live.
    EncoderProps,
    /// N short sub-windows of the telemetry the metrics drain already computes.
    BurstStats,
    /// A kind this build does not implement — acked `unknown_kind`, never run.
    Other(String),
}

impl CaptureKind {
    pub fn as_str(&self) -> &str {
        match self {
            CaptureKind::PipelineDot => "pipeline_dot",
            CaptureKind::EncoderProps => "encoder_props",
            CaptureKind::BurstStats => "burst_stats",
            CaptureKind::Other(s) => s.as_str(),
        }
    }

    /// `true` for a kind this build knows how to run.
    pub fn is_known(&self) -> bool {
        !matches!(self, CaptureKind::Other(_))
    }
}

impl<'de> Deserialize<'de> for CaptureKind {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        let raw = String::deserialize(deserializer)?;
        Ok(match raw.as_str() {
            "pipeline_dot" => CaptureKind::PipelineDot,
            "encoder_props" => CaptureKind::EncoderProps,
            "burst_stats" => CaptureKind::BurstStats,
            _ => CaptureKind::Other(raw),
        })
    }
}

impl Serialize for CaptureKind {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        serializer.serialize_str(self.as_str())
    }
}

/// The caps a capture must respect: compressed payload bytes and wall clock.
/// Both are hard — over either one the capture reports what it has with
/// `truncated` / `error: "deadline"` rather than growing.
#[derive(Deserialize, Debug, Clone, Copy, PartialEq, Eq)]
pub struct CaptureBudget {
    #[serde(default = "capture_default_max_bytes")]
    pub max_bytes: usize,
    #[serde(default = "capture_default_max_ms")]
    pub max_ms: u64,
}

fn capture_default_max_bytes() -> usize {
    CAPTURE_DEFAULT_MAX_BYTES
}
fn capture_default_max_ms() -> u64 {
    CAPTURE_DEFAULT_MAX_MS
}

impl Default for CaptureBudget {
    fn default() -> Self {
        CaptureBudget {
            max_bytes: CAPTURE_DEFAULT_MAX_BYTES,
            max_ms: CAPTURE_DEFAULT_MAX_MS,
        }
    }
}

/// `burst_stats` shaping. Absent fields take the agent's defaults; both are
/// clamped agent-side (`session::capture::BurstPlan`) — a control plane that
/// asks for 10 000 × 5 ms gets a legal plan, not a refusal.
#[derive(Deserialize, Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct CaptureParams {
    #[serde(default)]
    pub windows: Option<u32>,
    #[serde(default)]
    pub window_ms: Option<u64>,
}

fn console_auto() -> String {
    "auto".to_string()
}
fn console_weston() -> String {
    "weston".to_string()
}

/// CM-01 host console capabilities, reported in `capacity.console_capabilities`
/// (agent-api.md) so the admin console-config UI can populate selectors.
/// `PartialEq` (CM-06/07): the console-hotplug watcher (`session::console_hotplug`)
/// diffs successive snapshots to detect a display/input hardware change.
#[derive(Serialize, Debug, Clone, Default, PartialEq, Eq)]
pub struct ConsoleCapabilities {
    pub connectors: Vec<String>,
    /// Wave 3.2 typed per-card connector/mode inventory. `connectors` remains
    /// for compatibility with older control planes and hotplug logic.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub outputs: Vec<DrmOutputCapability>,
    pub audio_sinks: Vec<AudioSink>,
    pub input_devices: Vec<InputDeviceInfo>,
}

#[derive(Serialize, Debug, Clone, PartialEq, Eq)]
pub struct DrmOutputCapability {
    pub id: String,
    pub card: String,
    pub render_node: Option<String>,
    pub connector: String,
    pub connected: bool,
    pub active_mode: Option<DrmModeCapability>,
    pub modes: Vec<DrmModeCapability>,
}

#[derive(Serialize, Debug, Clone, PartialEq, Eq)]
pub struct DrmModeCapability {
    pub name: String,
    pub width: u16,
    pub height: u16,
    pub refresh_millihz: u32,
    pub preferred: bool,
    pub interlaced: bool,
    pub clock_khz: u32,
    pub htotal: u16,
    pub vtotal: u16,
}

#[derive(Serialize, Debug, Clone, PartialEq, Eq)]
pub struct AudioSink {
    pub id: String,
    pub label: String,
}

#[derive(Serialize, Debug, Clone, PartialEq, Eq)]
pub struct InputDeviceInfo {
    pub path: String,
    pub label: String,
}

/// Messages received by the node agent from the control plane.
#[derive(Deserialize, Debug)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ControlMsg {
    Registered {
        host_id: String,
        node_secret: Option<String>,
        heartbeat_interval_ms: u64,
    },
    /// Reserve + prepare a placed session (P1-6).
    SessionAssign {
        id: String,
        session_id: String,
        #[serde(default)]
        gpu_index: i32,
        /// The app image + runtime spec to launch (P1-5 container launch).
        #[serde(default)]
        app: AppSpec,
        stream: StreamSpec,
        #[serde(default)]
        resources: Option<ResourceSpec>,
        #[serde(default)]
        video_topology: VideoTopology,
    },
    /// Bring the session pipeline up (P1-6).
    SessionStart {
        id: String,
        session_id: String,
    },
    /// Tear the session down (P1-6).
    SessionStop {
        id: String,
        session_id: String,
        #[serde(default)]
        reason: String,
    },
    /// Swap the source app of a running session behind the interpipe boundary
    /// while encode + webrtcbin stay live. `app` is the same `AppSpec` shape as
    /// `session_assign.app`. The control plane has already validated ownership,
    /// swappable state, and reservation-fit.
    SessionSwapApp {
        id: String,
        session_id: String,
        #[serde(default)]
        app: AppSpec,
    },
    /// Change a running session's compositor-side `wl_output` mode and/or
    /// `preferred_scale` without touching the encode/stream path. Partial: an
    /// omitted field means "leave as is". Same rejected-is-a-no-op ack contract
    /// as `session_swap_app`.
    SessionDisplayUpdate {
        id: String,
        session_id: String,
        #[serde(default)]
        render_width: Option<i32>,
        #[serde(default)]
        render_height: Option<i32>,
        #[serde(default)]
        ui_scale: Option<f64>,
        /// New external (encoded) size actually sent to the client. Both-or-
        /// neither, must be a rung of the session's aspect family ≤ launch size
        /// (control plane validates, agent re-checks). Orthogonal to `render_*`
        /// (compositor mode vs wire frame size); omitted means "leave as is".
        #[serde(default)]
        stream_width: Option<i32>,
        #[serde(default)]
        stream_height: Option<i32>,
    },
    /// On-demand observation capture for a running session — admin-only,
    /// observability-only.
    ///
    /// Routed to the runner over a per-session mpsc and **acked on ARM**, not
    /// completion. `ok:true` means capture is running; the result arrives later
    /// as a `session_trace_event` (`diag.<kind>`) on the reliable lane. Refusals:
    /// `busy` (single-flight, never queued), `unsupported` (known kind,
    /// impossible here — e.g. a local-only console session with no encode
    /// pipeline), `unknown_kind`, `no_such_session`.
    ///
    /// Never touches the media path: driven from the runner's 100ms supervision
    /// tick, never a pad probe or streaming thread, and reads only element
    /// properties/caps and already-collected telemetry (#270: the host-side
    /// overlay/probe that could crash a stream is gone for good).
    SessionCapture {
        id: String,
        session_id: String,
        /// Minted by the control plane; echoed verbatim so `GET
        /// .../captures/{capture_id}` can find it.
        capture_id: String,
        /// An unrecognized string deserializes to [`CaptureKind::Other`] rather
        /// than failing the message, so the agent can ack `unknown_kind` (→ 422)
        /// instead of nacking the frame.
        kind: CaptureKind,
        #[serde(default)]
        budget: CaptureBudget,
        /// `burst_stats` only; ignored (and clamped) for the other kinds.
        #[serde(default)]
        params: CaptureParams,
    },
    Error {
        code: String,
        message: String,
    },
    /// Relay browser→agent signaling (answer/ice/bye/error). `msg` is a
    /// verbatim Phase 0 JSON object.
    Signaling {
        session_id: String,
        msg: serde_json::Value,
    },
    /// Per-host runtime settings push. Overlaid onto RuntimeSettings; live
    /// knobs apply to the next session. No ack. Restart-class knobs need a
    /// separate `restart`.
    ConfigUpdate {
        #[serde(default)]
        settings: serde_json::Value,
        /// Host's resolved console-mode config. Absent ⇒ console mode disabled
        /// (falls back to `QUASAR_LOCAL_DISPLAY` for dev).
        #[serde(default)]
        console_config: Option<ConsoleConfig>,
    },
    /// Restart request: ack, then exit so the container restart policy
    /// restarts us with fresh config.
    Restart {
        id: String,
    },
    /// Pull a prebuilt catalog image into this host's docker daemon. Acked
    /// immediately, then pulls asynchronously with progress via `image_state`.
    /// `registry_ref` is always a concrete immutable ref, never a floating tag.
    ImageEnsure {
        id: String,
        image_id: String,
        registry_ref: String,
        version: String,
    },
    /// Best-effort removal of a managed image. Never force-removes an image
    /// backing a live container.
    ImageRemove {
        id: String,
        image_id: String,
    },
    /// Build a `kind:"template"` catalog image locally on this host — the
    /// template analogue of `image_ensure`: ack immediately, fetch the build
    /// context tarball (`context_url`, commit-sha-pinned GitHub codeload),
    /// extract `context_subdir`, `docker build`, report progress via
    /// `image_state` (reusing `building`). `local_tag` is CP-assigned and never
    /// pushed anywhere; recorded in the same state map as `image_id →
    /// registry_ref` so `image_remove`/`register.images[]` work unchanged.
    ImageBuild {
        id: String,
        image_id: String,
        context_url: String,
        context_subdir: String,
        dockerfile: String,
        /// `--build-arg` name→value pairs (JSON object). Empty/absent ⇒ none.
        #[serde(default)]
        build_args: std::collections::BTreeMap<String, String>,
        local_tag: String,
        version: String,
    },
    /// Future additions land here until handled.
    #[serde(other)]
    Unknown,
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::metrics::MetricsWindow;

    /// Render size only ⇒ ui_scale absent means "leave as is".
    #[test]
    fn session_display_update_parses_partial_and_tolerates_extra_fields() {
        let msg: ControlMsg = serde_json::from_value(serde_json::json!({
            "type": "session_display_update", "id": "c1", "session_id": "s",
            "render_width": 1280, "render_height": 720
        }))
        .unwrap();
        match msg {
            ControlMsg::SessionDisplayUpdate {
                id,
                session_id,
                render_width,
                render_height,
                ui_scale,
                stream_width,
                stream_height,
            } => {
                assert_eq!(id, "c1");
                assert_eq!(session_id, "s");
                assert_eq!(render_width, Some(1280));
                assert_eq!(render_height, Some(720));
                assert_eq!(ui_scale, None);
                assert_eq!((stream_width, stream_height), (None, None));
            }
            other => panic!("wrong variant: {other:?}"),
        }

        // ui_scale only, plus an unknown field (forward compat).
        let msg: ControlMsg = serde_json::from_value(serde_json::json!({
            "type": "session_display_update", "id": "c2", "session_id": "s",
            "ui_scale": 1.5, "something_new": true
        }))
        .unwrap();
        assert!(matches!(
            msg,
            ControlMsg::SessionDisplayUpdate {
                render_width: None,
                render_height: None,
                ui_scale: Some(v),
                ..
            } if (v - 1.5).abs() < f64::EPSILON
        ));
    }

    /// External resolution only — the render/scale half is untouched.
    #[test]
    fn session_display_update_parses_the_stream_pair() {
        let msg: ControlMsg = serde_json::from_value(serde_json::json!({
            "type": "session_display_update", "id": "c3", "session_id": "s",
            "stream_width": 1280, "stream_height": 720
        }))
        .unwrap();
        assert!(matches!(
            msg,
            ControlMsg::SessionDisplayUpdate {
                render_width: None,
                render_height: None,
                ui_scale: None,
                stream_width: Some(1280),
                stream_height: Some(720),
                ..
            }
        ));

        // Both halves in one message — independent axes, each bounded by launch size.
        let msg: ControlMsg = serde_json::from_value(serde_json::json!({
            "type": "session_display_update", "id": "c4", "session_id": "s",
            "render_width": 1280, "render_height": 720,
            "stream_width": 1280, "stream_height": 720
        }))
        .unwrap();
        assert!(matches!(
            msg,
            ControlMsg::SessionDisplayUpdate {
                render_width: Some(1280),
                render_height: Some(720),
                stream_width: Some(1280),
                stream_height: Some(720),
                ..
            }
        ));
    }

    fn metrics_msg(render: Option<(i32, i32)>, ui_scale: Option<f64>) -> AgentMsg {
        metrics_msg_full(render, ui_scale, None, Some(true))
    }

    fn metrics_msg_full(
        render: Option<(i32, i32)>,
        ui_scale: Option<f64>,
        stream: Option<(i32, i32)>,
        external_resize_supported: Option<bool>,
    ) -> AgentMsg {
        AgentMsg::SessionMetrics {
            session_id: "s".to_string(),
            ts_unix_ms: 0,
            window_ms: 5000,
            fps: 60.0,
            bitrate_kbps: 15000.0,
            encode_ms: None,
            encode_ms_p50: None,
            encode_ms_p95: None,
            encode_ms_max: None,
            source_fps: 60.0,
            compositor_fps: 60.0,
            compositor_pts_delta_p50_ms: None,
            compositor_pts_delta_p95_ms: None,
            interpipe_queue_level_max: 0,
            interpipe_queue_dwell_p50_ms: None,
            interpipe_queue_dwell_p95_ms: None,
            interpipe_queue_drops: 0,
            rtp_fps: 60.0,
            rtp_bitrate_kbps: 15000.0,
            frames_encoded: 300,
            frames_dropped: 0,
            abr_setpoint_kbps: None,
            abr_floor_kbps: None,
            gcc_estimate_kbps: None,
            abr_mode: "off",
            adaptation_state: "healthy",
            latency_probe: None,
            bytes_used: None,
            render_width: render.map(|r| r.0),
            render_height: render.map(|r| r.1),
            ui_scale,
            stream_width: stream.map(|s| s.0),
            stream_height: stream.map(|s| s.1),
            external_resize_supported,
            ladder_speed_bias: None,
            ladder_res_rung: None,
            ladder_fps: None,
            external_owner: None,
        }
    }

    #[test]
    fn session_metrics_echoes_display_fields_only_when_non_default() {
        let json = serde_json::to_value(metrics_msg(None, None)).unwrap();
        let obj = json.as_object().unwrap();
        assert!(!obj.contains_key("render_width"));
        assert!(!obj.contains_key("render_height"));
        assert!(!obj.contains_key("ui_scale"));

        let json = serde_json::to_value(metrics_msg(Some((1280, 720)), Some(1.5))).unwrap();
        assert_eq!(json["render_width"], 1280);
        assert_eq!(json["render_height"], 720);
        assert_eq!(json["ui_scale"], 1.5);
    }

    // `stream_*` is omitted at launch size, present once stepped off it;
    // `external_resize_supported` rides EVERY window.
    #[test]
    fn session_metrics_echoes_stream_fields_only_when_non_default() {
        let json = serde_json::to_value(metrics_msg_full(None, None, None, Some(true))).unwrap();
        let obj = json.as_object().unwrap();
        assert!(!obj.contains_key("stream_width"));
        assert!(!obj.contains_key("stream_height"));
        assert_eq!(json["external_resize_supported"], true);

        let json =
            serde_json::to_value(metrics_msg_full(None, None, Some((1280, 720)), Some(false)))
                .unwrap();
        assert_eq!(json["stream_width"], 1280);
        assert_eq!(json["stream_height"], 720);
        assert_eq!(json["external_resize_supported"], false);

        // The two halves are independent.
        let json = serde_json::to_value(metrics_msg_full(
            Some((960, 540)),
            Some(1.0),
            Some((1280, 720)),
            Some(true),
        ))
        .unwrap();
        assert_eq!(json["render_width"], 960);
        assert_eq!(json["stream_width"], 1280);
    }

    // The three ladder keys ride the same convention as stream_*: absent means
    // "at the default", never "unknown".
    #[test]
    fn session_metrics_omits_ladder_keys_at_the_default() {
        let json = serde_json::to_value(metrics_msg_full(None, None, None, Some(true))).unwrap();
        let obj = json.as_object().unwrap();
        assert!(!obj.contains_key("ladder_speed_bias"));
        assert!(!obj.contains_key("ladder_res_rung"));
        assert!(!obj.contains_key("ladder_fps"));
        assert!(!obj.contains_key("external_owner"));
        // The ladder-followed ABR floor rides the same convention.
        assert!(!obj.contains_key("abr_floor_kbps"));
    }

    #[test]
    fn session_metrics_carries_the_abr_floor_once_the_ladder_moves_it() {
        let mut msg = metrics_msg_full(None, None, None, Some(true));
        if let AgentMsg::SessionMetrics {
            ref mut abr_floor_kbps,
            ..
        } = msg
        {
            *abr_floor_kbps = Some(1130.0);
        }
        let json = serde_json::to_value(msg).unwrap();
        assert_eq!(json["abr_floor_kbps"], 1130.0);
    }

    #[test]
    fn session_assign_topology_defaults_stream_only_and_accepts_local_only() {
        let base = serde_json::json!({
            "type": "session_assign", "id": "c1", "session_id": "s1",
            "stream": {"width": 1920, "height": 1080, "fps": 60,
                "bitrate_kbps": 15000, "h264_profile": "constrained-baseline"}
        });
        let old: ControlMsg = serde_json::from_value(base.clone()).unwrap();
        assert!(matches!(
            old,
            ControlMsg::SessionAssign {
                video_topology: VideoTopology::StreamOnly,
                ..
            }
        ));

        let mut local = base;
        local["video_topology"] = serde_json::json!("local_only");
        let parsed: ControlMsg = serde_json::from_value(local).unwrap();
        assert!(matches!(
            parsed,
            ControlMsg::SessionAssign {
                video_topology: VideoTopology::LocalOnly,
                ..
            }
        ));
    }

    #[test]
    fn storage_volume_serde_roundtrip() {
        let v = StorageVolume {
            label: "agent-data".to_string(),
            path: "/var/lib/quasar-agent".to_string(),
            total_mb: 819200,
            available_mb: 512000,
        };
        let json = serde_json::to_string(&v).unwrap();
        let back: StorageVolume = serde_json::from_str(&json).unwrap();
        assert_eq!(v, back);
    }

    #[test]
    fn capacity_json_omits_absent_additive_fields() {
        let msg = AgentMsg::Capacity {
            host: HostCapacity {
                cpu_cores: 16,
                mem_mb: 64000,
                storage: None,
                cpu_model: None,
            },
            gpus: vec![GpuCapacity {
                index: 0,
                vendor: "amd".to_string(),
                model: "Radeon Pro V520".to_string(),
                vram_mb_total: 16384,
                encode_slots_total: 2,
                render_node: None,
                device_path: None,
            }],
            gpu_detection: "ok".to_string(),
            gpu_detection_reason: None,
            console_capabilities: None,
            effective_settings: None,
            codecs: None,
            codec_throughput: None,
            readiness: None,
        };
        let json = serde_json::to_value(&msg).unwrap();
        assert!(!json["host"].as_object().unwrap().contains_key("storage"));
        assert!(!json["host"].as_object().unwrap().contains_key("cpu_model"));
        assert!(!json["gpus"][0]
            .as_object()
            .unwrap()
            .contains_key("render_node"));
        assert!(!json
            .as_object()
            .unwrap()
            .contains_key("console_capabilities"));
        assert!(!json.as_object().unwrap().contains_key("effective_settings"));
        assert!(!json.as_object().unwrap().contains_key("codecs"));
        // #506: absent, not null — distinguishes "never reported" from "{}".
        assert!(!json.as_object().unwrap().contains_key("codec_throughput"));
    }

    #[test]
    fn capacity_json_includes_present_additive_fields() {
        let msg = AgentMsg::Capacity {
            host: HostCapacity {
                cpu_cores: 16,
                mem_mb: 64000,
                storage: Some(vec![StorageVolume {
                    label: "agent-data".to_string(),
                    path: "/var/lib/quasar-agent".to_string(),
                    total_mb: 819200,
                    available_mb: 512000,
                }]),
                cpu_model: Some("AMD Ryzen 9 7950X 16-Core Processor".to_string()),
            },
            gpus: vec![GpuCapacity {
                index: 0,
                vendor: "amd".to_string(),
                model: "Radeon Pro V520".to_string(),
                vram_mb_total: 16384,
                encode_slots_total: 2,
                render_node: Some("/dev/dri/by-path/pci-0000:04:00.0-render".to_string()),
                device_path: Some("/dev/dri/renderD128".to_string()),
            }],
            gpu_detection: "ok".to_string(),
            gpu_detection_reason: None,
            console_capabilities: None,
            effective_settings: Some(
                [("encoder".to_string(), "nvenc".to_string())]
                    .into_iter()
                    .collect(),
            ),
            codecs: Some(vec!["h264".to_string(), "h265".to_string()]),
            codec_throughput: Some(
                [
                    (
                        "h264".to_string(),
                        CodecThroughput {
                            max_pixel_rate_mpix_s: 1400.0,
                        },
                    ),
                    (
                        "h265".to_string(),
                        CodecThroughput {
                            max_pixel_rate_mpix_s: 395.0,
                        },
                    ),
                ]
                .into_iter()
                .collect(),
            ),
            readiness: Some(vec![ReadinessCheck {
                id: "nvidia_lib32_gl".to_string(),
                status: "fail".to_string(),
                summary: "no 32-bit NVIDIA GL libraries on the host".to_string(),
                remediation: "sudo dnf install -y nvidia-driver-libs.i686".to_string(),
            }]),
        };
        let json = serde_json::to_value(&msg).unwrap();
        assert_eq!(json["host"]["storage"][0]["label"], "agent-data");
        assert_eq!(
            json["host"]["cpu_model"],
            "AMD Ryzen 9 7950X 16-Core Processor"
        );
        assert_eq!(
            json["gpus"][0]["render_node"],
            "/dev/dri/by-path/pci-0000:04:00.0-render"
        );
        assert_eq!(json["effective_settings"]["encoder"], "nvenc");
        assert_eq!(json["codecs"][1], "h265");
        // #506: the hint is an OBJECT per codec, extensible without a second amendment.
        assert_eq!(
            json["codec_throughput"]["h265"]["max_pixel_rate_mpix_s"],
            395.0
        );
        assert_eq!(
            json["codec_throughput"]["h264"]["max_pixel_rate_mpix_s"],
            1400.0
        );
        // Readiness rides the same report; dropping remediation would leave a
        // red row with no fix.
        assert_eq!(json["readiness"][0]["id"], "nvidia_lib32_gl");
        assert_eq!(json["readiness"][0]["status"], "fail");
        assert_eq!(
            json["readiness"][0]["remediation"],
            "sudo dnf install -y nvidia-driver-libs.i686"
        );
    }

    /// `reason_code`/`app_log_tail` must be ABSENT (not null) on every ordinary
    /// session_state.
    #[test]
    fn session_state_json_omits_the_absent_failure_detail_fields() {
        let msg = AgentMsg::SessionState {
            session_id: "s1".to_string(),
            state: "running".to_string(),
            detail: Some("pipeline live; offer ready".to_string()),
            error: None,
            reason_code: None,
            app_log_tail: None,
        };
        let json = serde_json::to_value(&msg).unwrap();
        let obj = json.as_object().unwrap();
        assert!(!obj.contains_key("reason_code"));
        assert!(!obj.contains_key("app_log_tail"));
        assert!(!obj.contains_key("error"));
    }

    #[test]
    fn session_state_json_carries_the_app_exit_failure_detail() {
        let msg = AgentMsg::SessionState {
            session_id: "s1".to_string(),
            state: "failed".to_string(),
            detail: None,
            error: Some("the app exited with code 0 before producing any video.".to_string()),
            reason_code: Some("app_exited_early".to_string()),
            app_log_tail: Some("Steam needs to be online to update\nexiting".to_string()),
        };
        let json = serde_json::to_value(&msg).unwrap();
        assert_eq!(json["reason_code"], "app_exited_early");
        assert!(json["app_log_tail"]
            .as_str()
            .unwrap()
            .contains("Steam needs to be online to update"));
    }

    /// #383: `gpu_vram: None` must be entirely absent on the wire, not `null`.
    #[test]
    fn heartbeat_json_omits_absent_gpu_vram() {
        let msg = AgentMsg::Heartbeat {
            running_sessions: vec!["s1".to_string()],
            ts_unix_ms: 1_700_000_000_000,
            gpu_vram: None,
        };
        let json = serde_json::to_value(&msg).unwrap();
        assert!(!json.as_object().unwrap().contains_key("gpu_vram"));
    }

    /// #383: `used_mb`/`free_mb` are omitted (not `null`) per-GPU on a failed
    /// read, never fabricated as `0`.
    #[test]
    fn heartbeat_json_includes_gpu_vram_and_omits_unknown_fields_per_gpu() {
        let msg = AgentMsg::Heartbeat {
            running_sessions: vec![],
            ts_unix_ms: 1_700_000_000_000,
            gpu_vram: Some(vec![
                VramSample {
                    index: 0,
                    used_mb: Some(512),
                    free_mb: Some(7680),
                },
                VramSample {
                    index: 1,
                    used_mb: None,
                    free_mb: None,
                },
            ]),
        };
        let json = serde_json::to_value(&msg).unwrap();
        assert_eq!(json["gpu_vram"][0]["index"], 0);
        assert_eq!(json["gpu_vram"][0]["used_mb"], 512);
        assert_eq!(json["gpu_vram"][0]["free_mb"], 7680);
        let gpu1 = json["gpu_vram"][1].as_object().unwrap();
        assert_eq!(gpu1["index"], 1);
        assert!(
            !gpu1.contains_key("used_mb"),
            "unknown used_mb must be omitted, never serialized as 0 or null"
        );
        assert!(
            !gpu1.contains_key("free_mb"),
            "unknown free_mb must be omitted, never serialized as 0 or null"
        );
    }

    // Wire round-trips for the image-management message shapes, field-name-exact.

    #[test]
    fn image_ensure_deserializes_with_exact_field_names() {
        let raw = serde_json::json!({
            "type": "image_ensure",
            "id": "cmd-1",
            "image_id": "steam",
            "registry_ref": "ghcr.io/accreleus/quasar-steam:sha-969cc14ea168",
            "version": "2026.08.07"
        });
        let msg: ControlMsg = serde_json::from_value(raw).unwrap();
        match msg {
            ControlMsg::ImageEnsure {
                id,
                image_id,
                registry_ref,
                version,
            } => {
                assert_eq!(id, "cmd-1");
                assert_eq!(image_id, "steam");
                assert_eq!(
                    registry_ref,
                    "ghcr.io/accreleus/quasar-steam:sha-969cc14ea168"
                );
                assert_eq!(version, "2026.08.07");
            }
            other => panic!("expected ImageEnsure, got {other:?}"),
        }
    }

    #[test]
    fn image_remove_deserializes_with_exact_field_names() {
        let raw = serde_json::json!({
            "type": "image_remove",
            "id": "cmd-2",
            "image_id": "steam"
        });
        let msg: ControlMsg = serde_json::from_value(raw).unwrap();
        match msg {
            ControlMsg::ImageRemove { id, image_id } => {
                assert_eq!(id, "cmd-2");
                assert_eq!(image_id, "steam");
            }
            other => panic!("expected ImageRemove, got {other:?}"),
        }
    }

    // A future control plane sending an unknown type must degrade to Unknown,
    // not fail the whole connection.
    #[test]
    fn unrecognized_control_type_is_unknown_not_a_parse_error() {
        let raw = serde_json::json!({"type": "image_frobnicate", "id": "x"});
        let msg: ControlMsg = serde_json::from_value(raw).unwrap();
        assert!(matches!(msg, ControlMsg::Unknown));
    }

    // Field-name-exact wire round-trip for image_build.
    #[test]
    fn image_build_deserializes_with_exact_field_names() {
        let raw = serde_json::json!({
            "type": "image_build",
            "id": "cmd-3",
            "image_id": "xfce-desktop",
            "context_url": "https://codeload.github.com/accreleus/quasar-images/tar.gz/deadbeef",
            "context_subdir": "xfce-desktop",
            "dockerfile": "Dockerfile",
            "build_args": {"BASE": "alpine:3", "VERSION": "1"},
            "local_tag": "quasar-local/xfce-desktop:2026.08.08",
            "version": "2026.08.08"
        });
        let msg: ControlMsg = serde_json::from_value(raw).unwrap();
        match msg {
            ControlMsg::ImageBuild {
                id,
                image_id,
                context_url,
                context_subdir,
                dockerfile,
                build_args,
                local_tag,
                version,
            } => {
                assert_eq!(id, "cmd-3");
                assert_eq!(image_id, "xfce-desktop");
                assert_eq!(
                    context_url,
                    "https://codeload.github.com/accreleus/quasar-images/tar.gz/deadbeef"
                );
                assert_eq!(context_subdir, "xfce-desktop");
                assert_eq!(dockerfile, "Dockerfile");
                assert_eq!(build_args.get("BASE").map(String::as_str), Some("alpine:3"));
                assert_eq!(build_args.get("VERSION").map(String::as_str), Some("1"));
                assert_eq!(local_tag, "quasar-local/xfce-desktop:2026.08.08");
                assert_eq!(version, "2026.08.08");
            }
            other => panic!("expected ImageBuild, got {other:?}"),
        }
    }

    // build_args is optional/additive — an object-less command still parses.
    #[test]
    fn image_build_build_args_defaults_to_empty() {
        let raw = serde_json::json!({
            "type": "image_build",
            "id": "cmd-4",
            "image_id": "minimal",
            "context_url": "https://codeload.github.com/x/y/tar.gz/abc",
            "context_subdir": "minimal",
            "dockerfile": "Dockerfile",
            "local_tag": "quasar-local/minimal:1",
            "version": "1"
        });
        let msg: ControlMsg = serde_json::from_value(raw).unwrap();
        match msg {
            ControlMsg::ImageBuild { build_args, .. } => assert!(build_args.is_empty()),
            other => panic!("expected ImageBuild, got {other:?}"),
        }
    }

    #[test]
    fn image_state_serializes_with_exact_field_names() {
        let msg = AgentMsg::ImageState {
            image_id: "steam".to_string(),
            version: "2026.08.07".to_string(),
            state: "pulling".to_string(),
            progress_pct: 42,
            bytes: 1_234_567,
            error: String::new(),
        };
        let json = serde_json::to_value(&msg).unwrap();
        assert_eq!(json["type"], "image_state");
        assert_eq!(json["image_id"], "steam");
        assert_eq!(json["version"], "2026.08.07");
        assert_eq!(json["state"], "pulling");
        assert_eq!(json["progress_pct"], 42);
        assert_eq!(json["bytes"], 1_234_567);
        assert_eq!(json["error"], "");
    }

    #[test]
    fn register_always_sends_images_even_when_empty() {
        // With nothing recorded, must send `[]`, not omit — omission means
        // pre-amendment agent, so the control plane never demotes stale rows.
        let msg = AgentMsg::Register {
            node_name: "gpu-host-01".to_string(),
            agent_version: "0.1.0".to_string(),
            auth: Auth::Enrollment {
                enrollment_token: "tok".to_string(),
            },
            images: Vec::new(),
        };
        let json = serde_json::to_value(&msg).unwrap();
        assert_eq!(json["images"], serde_json::json!([]));
    }

    #[test]
    fn register_includes_images_when_present() {
        let msg = AgentMsg::Register {
            node_name: "gpu-host-01".to_string(),
            agent_version: "0.1.0".to_string(),
            auth: Auth::Reconnect {
                node_secret: "secret".to_string(),
            },
            images: vec![RegisterImageEntry {
                image_id: "steam".to_string(),
                version: "2026.08.07".to_string(),
                state: "ready".to_string(),
            }],
        };
        let json = serde_json::to_value(&msg).unwrap();
        assert_eq!(json["images"][0]["image_id"], "steam");
        assert_eq!(json["images"][0]["version"], "2026.08.07");
        assert_eq!(json["images"][0]["state"], "ready");
    }

    // ── session_capture ──────────────────────────────────────────────────────

    #[test]
    fn session_capture_parses_a_known_kind_with_its_budget_and_params() {
        let msg: ControlMsg = serde_json::from_value(serde_json::json!({
            "type": "session_capture", "id": "c1", "session_id": "s",
            "capture_id": "cap-9", "kind": "burst_stats",
            "budget": { "max_bytes": 262144, "max_ms": 10000 },
            "params": { "windows": 20, "window_ms": 250 }
        }))
        .unwrap();
        match msg {
            ControlMsg::SessionCapture {
                id,
                session_id,
                capture_id,
                kind,
                budget,
                params,
            } => {
                assert_eq!(id, "c1");
                assert_eq!(session_id, "s");
                assert_eq!(capture_id, "cap-9");
                assert_eq!(kind, CaptureKind::BurstStats);
                assert_eq!(budget.max_bytes, 262_144);
                assert_eq!(budget.max_ms, 10_000);
                assert_eq!(params.windows, Some(20));
                assert_eq!(params.window_ms, Some(250));
            }
            other => panic!("wrong variant: {other:?}"),
        }
    }

    /// An unknown kind must land on `Other(<the string>)`, NOT fail the frame —
    /// a failed deserialize would nack the message type (→ 501 "too old"),
    /// while `Other` lets the agent ack `unknown_kind` (→ 422). `serde(other)`
    /// can't distinguish those since it discards the string.
    #[test]
    fn session_capture_maps_an_unknown_kind_to_other_rather_than_failing() {
        let msg: ControlMsg = serde_json::from_value(serde_json::json!({
            "type": "session_capture", "id": "c2", "session_id": "s",
            "capture_id": "cap-1", "kind": "bitstream_dump"
        }))
        .unwrap();
        match msg {
            ControlMsg::SessionCapture { kind, .. } => {
                assert_eq!(kind, CaptureKind::Other("bitstream_dump".to_string()));
                assert!(!kind.is_known());
                assert_eq!(kind.as_str(), "bitstream_dump");
            }
            other => panic!("wrong variant: {other:?}"),
        }
    }

    #[test]
    fn session_capture_defaults_budget_and_params_and_tolerates_extra_fields() {
        let msg: ControlMsg = serde_json::from_value(serde_json::json!({
            "type": "session_capture", "id": "c3", "session_id": "s",
            "capture_id": "cap-2", "kind": "pipeline_dot",
            "requested_by": "admin@example.test", "future_field": 7
        }))
        .unwrap();
        match msg {
            ControlMsg::SessionCapture {
                kind,
                budget,
                params,
                ..
            } => {
                assert_eq!(kind, CaptureKind::PipelineDot);
                assert_eq!(budget.max_bytes, CAPTURE_DEFAULT_MAX_BYTES);
                assert_eq!(budget.max_ms, CAPTURE_DEFAULT_MAX_MS);
                assert_eq!(params, CaptureParams::default());
            }
            other => panic!("wrong variant: {other:?}"),
        }
    }

    #[test]
    fn session_capture_accepts_a_partial_budget() {
        let msg: ControlMsg = serde_json::from_value(serde_json::json!({
            "type": "session_capture", "id": "c4", "session_id": "s",
            "capture_id": "cap-3", "kind": "encoder_props",
            "budget": { "max_ms": 2000 }
        }))
        .unwrap();
        match msg {
            ControlMsg::SessionCapture { budget, .. } => {
                assert_eq!(budget.max_ms, 2_000);
                assert_eq!(budget.max_bytes, CAPTURE_DEFAULT_MAX_BYTES);
            }
            other => panic!("wrong variant: {other:?}"),
        }
    }

    /// Every kind round-trips through its wire string — the result payload
    /// echoes `kind`, so a rename here would silently break the read side.
    #[test]
    fn capture_kind_round_trips_through_its_wire_string() {
        for wire in ["pipeline_dot", "encoder_props", "burst_stats"] {
            let kind: CaptureKind = serde_json::from_value(serde_json::json!(wire)).unwrap();
            assert!(kind.is_known());
            assert_eq!(kind.as_str(), wire);
            assert_eq!(
                serde_json::to_value(&kind).unwrap(),
                serde_json::json!(wire)
            );
        }
    }

    /// `ok:true` carries no `error` key at all (not `null`) — the same
    /// omit-when-absent convention every ack on this wire uses.
    #[test]
    fn capture_ack_shapes_match_the_wire_spec() {
        let ok = AgentMsg::Ack {
            id: "c1".to_string(),
            ok: true,
            error: None,
        };
        let json = serde_json::to_value(&ok).unwrap();
        assert_eq!(json["type"], "ack");
        assert_eq!(json["ok"], true);
        assert!(!json.as_object().unwrap().contains_key("error"));

        for reason in ["busy", "unsupported", "unknown_kind", "no_such_session"] {
            let nack = AgentMsg::Ack {
                id: "c1".to_string(),
                ok: false,
                error: Some(reason.to_string()),
            };
            let json = serde_json::to_value(&nack).unwrap();
            assert_eq!(json["ok"], false);
            assert_eq!(json["error"], reason);
        }
    }

    /// CHARACTERISATION — the `session_metrics` wire key set, pinned. Display,
    /// external size and ladder keys ride the wire ONLY when off their default;
    /// absence must always mean "at the default", never "unknown". If a
    /// refactor needs this list edited, the refactor changed the contract.
    #[test]
    fn session_metrics_wire_key_set_is_pinned() {
        let keys = |w: &MetricsWindow| -> Vec<String> {
            let v =
                serde_json::to_value(AgentMsg::session_metrics("s".into(), 0, w, None)).unwrap();
            let mut k: Vec<String> = v.as_object().unwrap().keys().cloned().collect();
            k.sort();
            k
        };

        let base = MetricsWindow {
            abr_mode: "smooth",
            adaptation_state: "healthy",
            ..Default::default()
        };
        assert_eq!(
            keys(&base),
            [
                "abr_mode",
                "adaptation_state",
                "bitrate_kbps",
                "compositor_fps",
                "fps",
                "frames_dropped",
                "frames_encoded",
                "interpipe_queue_drops",
                "interpipe_queue_level_max",
                "rtp_bitrate_kbps",
                "rtp_fps",
                "session_id",
                "source_fps",
                "ts_unix_ms",
                "type",
                "window_ms",
            ]
        );

        let full = MetricsWindow {
            encode_ms: Some(4.0),
            encode_ms_p50: Some(3.5),
            encode_ms_p95: Some(9.0),
            encode_ms_max: Some(212.0),
            compositor_pts_delta_p50_ms: Some(16.6),
            compositor_pts_delta_p95_ms: Some(16.7),
            interpipe_queue_dwell_p50_ms: Some(0.4),
            interpipe_queue_dwell_p95_ms: Some(1.2),
            abr_setpoint_kbps: Some(12000.0),
            abr_floor_kbps: Some(3000.0),
            gcc_estimate_kbps: Some(11800.0),
            render_width: Some(1280),
            render_height: Some(720),
            ui_scale: Some(1.5),
            stream_width: Some(960),
            stream_height: Some(540),
            external_resize_supported: Some(true),
            ladder_speed_bias: Some(2),
            ladder_res_rung: Some(1),
            ladder_fps: Some(60),
            external_owner: Some("auto"),
            probe_capture_to_enc_in_p50_ms: Some(1.0),
            probe_capture_to_enc_in_p95_ms: Some(2.0),
            probe_enc_out_to_send_p50_ms: Some(3.0),
            probe_enc_out_to_send_p95_ms: Some(4.0),
            probe_pay_to_send_p50_ms: Some(5.0),
            probe_pay_to_send_p95_ms: Some(6.0),
            probe_pts_to_emit_p50_ms: Some(7.0),
            probe_pts_to_emit_p95_ms: Some(8.0),
            probe_compositor_frame_interval_p95_ms: Some(9.0),
            probe_send_desyncs: Some(0.0),
            probe_pts_unmatched: Some(0.0),
            ..base.clone()
        };
        assert_eq!(
            keys(&full),
            [
                "abr_floor_kbps",
                "abr_mode",
                "abr_setpoint_kbps",
                "adaptation_state",
                "bitrate_kbps",
                "compositor_fps",
                "compositor_pts_delta_p50_ms",
                "compositor_pts_delta_p95_ms",
                "encode_ms",
                "encode_ms_max",
                "encode_ms_p50",
                "encode_ms_p95",
                "external_owner",
                "external_resize_supported",
                "fps",
                "frames_dropped",
                "frames_encoded",
                "gcc_estimate_kbps",
                "interpipe_queue_drops",
                "interpipe_queue_dwell_p50_ms",
                "interpipe_queue_dwell_p95_ms",
                "interpipe_queue_level_max",
                "ladder_fps",
                "ladder_res_rung",
                "ladder_speed_bias",
                "probe_capture_to_enc_in_p50_ms",
                "probe_capture_to_enc_in_p95_ms",
                "probe_compositor_frame_interval_p95_ms",
                "probe_enc_out_to_send_p50_ms",
                "probe_enc_out_to_send_p95_ms",
                "probe_pay_to_send_p50_ms",
                "probe_pay_to_send_p95_ms",
                "probe_pts_to_emit_p50_ms",
                "probe_pts_to_emit_p95_ms",
                "probe_pts_unmatched",
                "probe_send_desyncs",
                "render_height",
                "render_width",
                "rtp_bitrate_kbps",
                "rtp_fps",
                "session_id",
                "source_fps",
                "stream_height",
                "stream_width",
                "ts_unix_ms",
                "type",
                "ui_scale",
                "window_ms",
            ]
        );

        // Not just the key set: the VALUES too, so a reroute can't swap width for height.
        let v =
            serde_json::to_value(AgentMsg::session_metrics("s".into(), 0, &full, None)).unwrap();
        assert_eq!(v["render_width"], 1280);
        assert_eq!(v["render_height"], 720);
        assert_eq!(v["ui_scale"], 1.5);
        assert_eq!(v["stream_width"], 960);
        assert_eq!(v["stream_height"], 540);
        assert_eq!(v["external_resize_supported"], true);
        assert_eq!(v["ladder_speed_bias"], 2);
        assert_eq!(v["ladder_res_rung"], 1);
        assert_eq!(v["ladder_fps"], 60);
        assert_eq!(v["external_owner"], "auto");
    }
}
