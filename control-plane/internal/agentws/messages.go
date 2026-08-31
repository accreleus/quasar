package agentws

import (
	"encoding/json"
	"log/slog"

	"github.com/accreleus/quasar/control-plane/internal/console"
)

// envelope is used to peek at the type field before full decode.
type envelope struct {
	Type string `json:"type"`
}

func peekType(raw []byte) (string, error) {
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return "", err
	}
	return e.Type, nil
}

// RegisterMsg is the first message the agent sends after every connect.
type RegisterMsg struct {
	Type         string          `json:"type"`
	NodeName     string          `json:"node_name"`
	AgentVersion string          `json:"agent_version"`
	Auth         json.RawMessage `json:"auth"`
	// Images (image-management P2) is a wholesale snapshot of the agent's managed
	// images. Keep-if-absent: nil ⇒ key absent, stored host_images rows untouched;
	// an explicit [] is a real "I have none" and flips ready rows to absent.
	Images []RegisterImage `json:"images"`
}

// AuthEnrollment is the auth field on first contact.
type AuthEnrollment struct {
	EnrollmentToken string `json:"enrollment_token"`
}

// AuthReconnect is the auth field on subsequent connects.
type AuthReconnect struct {
	NodeSecret string `json:"node_secret"`
}

// RegisteredMsg is the control-plane reply to register.
type RegisteredMsg struct {
	Type                string `json:"type"`
	HostID              string `json:"host_id"`
	NodeSecret          string `json:"node_secret,omitempty"`
	HeartbeatIntervalMs int    `json:"heartbeat_interval_ms"`
}

// CapacityMsg is a full capacity report from the agent.
type CapacityMsg struct {
	Type string        `json:"type"`
	Host HostCapacity  `json:"host"`
	GPUs []GPUCapacity `json:"gpus"`
	// GPUDetection is additive and fail-closed. Older agents omit it; a non-empty
	// GPU list is then treated as ok, while an empty list is unavailable.
	GPUDetection string `json:"gpu_detection,omitempty"`
	// GPUDetectionReason is a bounded, sanitized diagnostic for operators.
	GPUDetectionReason string `json:"gpu_detection_reason,omitempty"`
	// ConsoleCapabilities (CM-01, agent-api.md `capacity.console_capabilities`):
	// what the host can do in console mode. Absent ⇒ empty arrays reported and the
	// admin UI offers only "auto".
	ConsoleCapabilities *console.Capabilities `json:"console_capabilities,omitempty"`
	// EffectiveSettings (agent-api.md `capacity.effective_settings`): the agent's
	// resolved env←overrides overlay, stringified. Keep-if-absent: nil never
	// clobbers the last stored value (see upsertCapacity).
	EffectiveSettings map[string]string `json:"effective_settings"`
	// Codecs (agent-api.md §3.1.2): wire codecs ("h264"|"h265"|"av1") the host's
	// active encoder path can produce. Keep-if-absent (see upsertHostCodecs); a
	// host that never reports reads back as h264-only (Store.HostCodecs).
	Codecs []string `json:"codecs"`
	// CodecThroughput (#506, agent-api.md `capacity.codec_throughput`): per-codec
	// encode-throughput hints, keyed like Codecs. Keep-if-absent; an explicit `{}`
	// is a real overwrite to "no hints known". Held as raw JSON, never decoded —
	// re-encoding would drop keys a newer agent sends (the Readiness rule). The
	// launch path reads it via session.Store.HostCodecPixelRates.
	CodecThroughput json.RawMessage `json:"codec_throughput"`
	// Readiness (agent-api.md `capacity.readiness`): agent-owned host readiness
	// checks, held as raw JSON. Never decode-and-re-encode for storage — that
	// drops every key a newer agent sends, and pass-through is the contract.
	// Keep-if-absent; explicit [] is a real "no checks". Shape-checked by
	// ValidReadiness. Advisory only: admission and scheduling never read it.
	Readiness json.RawMessage `json:"readiness"`
}

// ReadinessCheck is a validation view of one readiness check, never the storage
// representation — the raw bytes are what gets persisted, so unknown per-check
// keys survive.
type ReadinessCheck struct {
	ID string `json:"id"`
	// "pass" | "fail" | "skip", not allow-listed: the vocabulary is agent-owned
	// and a future status must not be dropped by an older control plane.
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	Remediation string `json:"remediation"`
}

// ValidReadiness shape-checks a raw `capacity.readiness` payload. Permissive on
// purpose: it rejects only what could not render (not an array, or a check with
// no `id`); unknown statuses and extra keys are stored verbatim.
//
// Explicit JSON `null` is rejected: it unmarshals to a nil slice with no error,
// and would be written as SQL NULL while `readiness_reported_at` advances —
// destroying a good stored check set. `[]` remains a valid real report, hence
// the nil check rather than a length check.
func ValidReadiness(raw json.RawMessage) ([]ReadinessCheck, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var checks []ReadinessCheck
	if err := json.Unmarshal(raw, &checks); err != nil {
		return nil, false
	}
	if checks == nil {
		return nil, false // JSON `null` — see above
	}
	for _, c := range checks {
		if c.ID == "" {
			return nil, false
		}
	}
	return checks, true
}

// HostCapacity is the host-level capacity in a capacity report.
type HostCapacity struct {
	CPUCores int `json:"cpu_cores"`
	MemMB    int `json:"mem_mb"`
	// Storage (agent-api.md `capacity.host.storage`): the agent's storage roots.
	// Keep-if-absent (see upsertCapacity); explicit [] is a real "no roots".
	Storage []StorageVolume `json:"storage"`
	// CPUModel (agent-api.md `capacity.host.cpu_model`): the CPU marketing name.
	// Keep-if-absent like Storage.
	CPUModel *string `json:"cpu_model"`
}

// StorageVolume is one agent-reported storage root in a capacity report.
type StorageVolume struct {
	Label       string `json:"label"`
	Path        string `json:"path"`
	TotalMB     int    `json:"total_mb"`
	AvailableMB int    `json:"available_mb"`
}

// GPUCapacity is one GPU's capacity in a capacity report.
type GPUCapacity struct {
	Index            int    `json:"index"`
	Vendor           string `json:"vendor"`
	Model            string `json:"model"`
	VRAMMBTotal      int    `json:"vram_mb_total"`
	EncodeSlotsTotal int    `json:"encode_slots_total"`
	// RenderNode (agent-api.md `capacity.gpus[].render_node`): stable by-path
	// render-node device path. No keep-if-absent handling — the gpus set is
	// wholesale-replaced per report.
	RenderNode *string `json:"render_node"`
	DevicePath *string `json:"device_path"`
}

// HeartbeatMsg is sent periodically by the agent.
type HeartbeatMsg struct {
	Type            string   `json:"type"`
	RunningSessions []string `json:"running_sessions"`
	TsUnixMs        int64    `json:"ts_unix_ms"`
	// GPUVram (#383, agent-api.md `heartbeat.gpu_vram`): live per-GPU memory
	// sample. Keep-if-absent; the admission veto abstains once the stored sample
	// goes stale. Never a fabricated zero: unknown is nil, all the way down.
	GPUVram []GPUVramSample `json:"gpu_vram"`
}

// GPUVramSample is one GPU's live memory state in a heartbeat. Index matches
// gpus.index (the agent's detection-order enumeration). UsedMB/FreeMB are
// pointers so an unavailable read is transported as absent/null, never as 0 —
// admission would read a 0 as "this GPU is full" and veto a healthy host.
type GPUVramSample struct {
	Index  int    `json:"index"`
	UsedMB *int32 `json:"used_mb"`
	FreeMB *int32 `json:"free_mb"`
}

// SessionMetricsMsg is the agent's per-session telemetry sample (agent-api.md
// § session_metrics). Fire-and-forget: no ack, no session-state authority; the
// coordinator validates host ownership and writes a source='agent' row. Fields
// are pointers so an absent key stores as absent, never zero. Trusted host-side
// reporter, so no browser-path key-filtering applies.
type SessionMetricsMsg struct {
	Type                     string   `json:"type"` // "session_metrics"
	SessionID                string   `json:"session_id"`
	TsUnixMs                 int64    `json:"ts_unix_ms"`
	WindowMs                 *int64   `json:"window_ms"`
	FPS                      *float64 `json:"fps"`
	BitrateKbps              *float64 `json:"bitrate_kbps"`
	EncodeMs                 *float64 `json:"encode_ms"`
	EncodeMsP50              *float64 `json:"encode_ms_p50"`
	EncodeMsP95              *float64 `json:"encode_ms_p95"`
	EncodeMsMax              *float64 `json:"encode_ms_max"`
	SourceFPS                *float64 `json:"source_fps"`
	CompositorFPS            *float64 `json:"compositor_fps"`
	CompositorPTSDeltaP50Ms  *float64 `json:"compositor_pts_delta_p50_ms"`
	CompositorPTSDeltaP95Ms  *float64 `json:"compositor_pts_delta_p95_ms"`
	InterpipeQueueLevelMax   *int64   `json:"interpipe_queue_level_max"`
	InterpipeQueueDwellP50Ms *float64 `json:"interpipe_queue_dwell_p50_ms"`
	InterpipeQueueDwellP95Ms *float64 `json:"interpipe_queue_dwell_p95_ms"`
	InterpipeQueueDrops      *int64   `json:"interpipe_queue_drops"`
	RTPFPS                   *float64 `json:"rtp_fps"`
	RTPBitrateKbps           *float64 `json:"rtp_bitrate_kbps"`
	FramesEncoded            *int64   `json:"frames_encoded"`
	FramesDropped            *int64   `json:"frames_dropped"`
	// Host-stage latency probe (QUASAR_LATENCY_PROBE / hostcfg `latency_probe`).
	// Present only while the probe is on and the window sampled that stage —
	// absent means "not measured", never zero. Observability only: nothing may
	// key scheduling, classification or ABR on these.
	ProbeCaptureToEncInP50Ms          *float64 `json:"probe_capture_to_enc_in_p50_ms"`
	ProbeCaptureToEncInP95Ms          *float64 `json:"probe_capture_to_enc_in_p95_ms"`
	ProbeEncOutToSendP50Ms            *float64 `json:"probe_enc_out_to_send_p50_ms"`
	ProbeEncOutToSendP95Ms            *float64 `json:"probe_enc_out_to_send_p95_ms"`
	ProbePayToSendP50Ms               *float64 `json:"probe_pay_to_send_p50_ms"`
	ProbePayToSendP95Ms               *float64 `json:"probe_pay_to_send_p95_ms"`
	ProbePTSToEmitP50Ms               *float64 `json:"probe_pts_to_emit_p50_ms"`
	ProbePTSToEmitP95Ms               *float64 `json:"probe_pts_to_emit_p95_ms"`
	ProbeCompositorFrameIntervalP95Ms *float64 `json:"probe_compositor_frame_interval_p95_ms"`
	ProbeSendDesyncs                  *float64 `json:"probe_send_desyncs"`
	ProbePTSUnmatched                 *float64 `json:"probe_pts_unmatched"`
	// ABR governor CBR setpoint (kbit/s); absent ⇒ static CBR.
	AbrSetpointKbps *float64 `json:"abr_setpoint_kbps"`
	// Governor's current floor (kbit/s), sent only when the ladder has moved it
	// off the launch floor. Absent ⇒ the launch floor, never "unknown".
	AbrFloorKbps *float64 `json:"abr_floor_kbps"`
	// Raw rtpgccbwe GCC estimate (kbit/s) before EWMA/deadband/step — distinct
	// from AbrSetpointKbps (governor output).
	GccEstimateKbps *float64 `json:"gcc_estimate_kbps"`
	// "protective" | "off"; empty from old agents, for which the diagnostic
	// bundle falls back to the setpoint-presence heuristic.
	AbrMode string `json:"abr_mode"`
	// Host-side bottleneck label ("healthy" | "network_congested" |
	// "encoder_saturated" | "unknown"); empty from old agents. Signal-only.
	// "client_presentation_limited" is never one of these — that is classified
	// control-plane-side (classifier.go).
	AdaptationState string `json:"adaptation_state"`
	// Post-session home-dir bytes used (local driver only), emitted once just
	// before session_state{stopped}; updates user_homes.bytes_used. Absent for
	// volume-driver sessions, not zero.
	BytesUsed *int64 `json:"bytes_used,omitempty"`
	// Compositor's app-facing wl_output logical mode + preferred_scale, present
	// only when non-default (agent-api.md § session_display_update). The only
	// authoritative readback of live render resolution / UI scale — neither
	// lives in session_state, the Session resource, or the sessions table.
	RenderWidth  *int32   `json:"render_width,omitempty"`
	RenderHeight *int32   `json:"render_height,omitempty"`
	UIScale      *float64 `json:"ui_scale,omitempty"`
	// Current external (encoded) size, present only when it differs from the
	// launch size — absent means "back at launch size", not "unknown". The
	// authoritative readback; the control plane's cache is only optimistic
	// between command ack and the next sample.
	StreamWidth  *int32 `json:"stream_width,omitempty"`
	StreamHeight *int32 `json:"stream_height,omitempty"`
	// Whether the active encoder can be retargeted to a new encoded size live.
	// nil (older agent) is treated as unknown-and-therefore-permitted — the
	// agent nacks the command instead.
	ExternalResizeSupported *bool `json:"external_resize_supported,omitempty"`
	// Live ladder state: encoder-facing speed-bias step and engaged resolution
	// rung (0 = launch/native). Absent from pre-amendment agents.
	LadderSpeedBias *int32 `json:"ladder_speed_bias,omitempty"`
	LadderResRung   *int32 `json:"ladder_res_rung,omitempty"`
	// Encoded frame rate the fps rung asks for, present only below the launch
	// rate — absent means "at the launch rate", never "unknown".
	LadderFps     *int32 `json:"ladder_fps,omitempty"`
	ExternalOwner string `json:"external_owner,omitempty"` // "auto" | "pinned"
}

// SessionTraceEventMsg is an agent-emitted trace event (agent-api.md
// § session_trace_event). Fire-and-forget; host ownership is validated via
// AgentTraceEventAllowed. A malformed or unowned event is dropped, never fatal
// to the WS.
type SessionTraceEventMsg struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	TsUnixMs  int64           `json:"ts_unix_ms"` // agent wall-clock, ms
	Event     string          `json:"event"`      // trace-format.md §3.2 type
	Payload   json.RawMessage `json:"payload"`    // per-type object; {} when absent
}

// ErrorMsg is sent to the agent on auth failure before closing.
type ErrorMsg struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	// diagEventPrefix marks an on-demand capture result (agent-api.md
	// §session_trace_event, diag.*). The prefix is the whole classification —
	// it selects both the ingest path here and the prune exemption in the trace
	// store, so the two cannot drift apart.
	diagEventPrefix = "diag."

	// Wire cap on a capture result — a diag.* row is prune-exempt, so it must
	// be bounded. Set well above the 256 KiB capture budget: the budget bounds
	// compressed bytes, which travel base64-encoded (~350 KiB) in JSON, so a
	// cap at the budget would reject a legal maximum-size capture.
	maxDiagPayloadBytes = 1 << 20
)

// diagPayloadHead is the only part of a capture payload the control plane
// reads; the rest is stored verbatim for the reader, so new capture kinds need
// no field here.
type diagPayloadHead struct {
	CaptureID string `json:"capture_id"`
}

// validDiagEvent gates a diag.* event: it must carry a capture_id (a row
// nothing can address is never pruned and never read) and fit the wire cap
// (prune-exempt must not mean unbounded). Rejects drop and log, never fatal to
// the connection.
func validDiagEvent(m SessionTraceEventMsg, log *slog.Logger, hostID string) bool {
	if len(m.Payload) > maxDiagPayloadBytes {
		log.Warn("dropping diag capture event: payload over the wire cap",
			"host_id", hostID, "session_id", m.SessionID, "event", m.Event,
			"bytes", len(m.Payload), "cap", maxDiagPayloadBytes)
		return false
	}
	var head diagPayloadHead
	if err := json.Unmarshal(m.Payload, &head); err != nil || head.CaptureID == "" {
		log.Warn("dropping diag capture event: no capture_id in payload",
			"host_id", hostID, "session_id", m.SessionID, "event", m.Event, "err", err)
		return false
	}
	return true
}
