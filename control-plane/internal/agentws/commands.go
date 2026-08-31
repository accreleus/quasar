package agentws

import (
	"encoding/json"
)

// SignalingEnvelope is the agent-api.md relay wrapper: both directions use this
// shape on the agent WebSocket. The inner Msg is verbatim Phase 0
// offer/answer/ice/bye/error (signaling.md) — the relay is transparent.
type SignalingEnvelope struct {
	Type      string          `json:"type"` // "signaling"
	SessionID string          `json:"session_id"`
	Msg       json.RawMessage `json:"msg"`
}

// This file defines the P1-6 session-lifecycle messages on the agent WebSocket:
// the downstream commands the control plane sends, and the upstream callbacks the
// agent reports. Shapes are exactly agent-api.md (P1-A) — a frozen interface.

// --- downstream commands (control plane → agent) -----------------------------

// SessionAssignCmd tells the agent to prepare a placed+reserved session.
type SessionAssignCmd struct {
	Type      string          `json:"type"` // "session_assign"
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	GPUIndex  int32           `json:"gpu_index"`
	App       json.RawMessage `json:"app"`    // mirrors apps.runtime_spec verbatim
	Stream    StreamSpec      `json:"stream"` // mirrors the sessions launch-param columns
	Resources ResourceSpec    `json:"resources"`
	// VideoTopology is the explicit output plan for this session. Older agents
	// default an omitted value to stream_only.
	VideoTopology string `json:"video_topology,omitempty"`
}

// StreamSpec is the per-session pipeline parameterization (P1-5 StreamParams).
type StreamSpec struct {
	Width       int32  `json:"width"`
	Height      int32  `json:"height"`
	FPS         int32  `json:"fps"`
	BitrateKbps int32  `json:"bitrate_kbps"`
	H264Profile string `json:"h264_profile"`
	// Profile ABR floor (kbit/s) so the governor never starves below the
	// profile's minimum. Omitted (0) ⇒ the agent's env/ratio-derived floor
	// (QUASAR_ABR_FLOOR_KBPS, else ceiling × ratio).
	AbrFloorKbps int32 `json:"abr_floor_kbps,omitempty"`
	// The one codec to encode ("h264"|"h265"|"av1"), resolved server-side at
	// launch. Omitted ⇒ h264, keeping the wire byte-identical for old agents.
	// H264Profile applies only to h264.
	Codec string `json:"codec,omitempty"`
	// Granted mic-capture state: true only when the client requested it AND the
	// instance setting mic_capture_enabled is on; the agent then adds a recvonly
	// Opus transceiver before the first offer (agent-api.md). Omitted (false) ⇒
	// today's single-m-line audio offer, byte-identical.
	Mic bool `json:"mic,omitempty"`
}

// ResourceSpec is the reserved budget the agent must not exceed.
type ResourceSpec struct {
	VRAMMB      int32 `json:"vram_mb"`
	EncodeSlots int32 `json:"encode_slots"`
}

// SessionStartCmd tells the agent to bring the pipeline up.
type SessionStartCmd struct {
	Type      string `json:"type"` // "session_start"
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
}

// SessionStopCmd tells the agent to tear the session down.
type SessionStopCmd struct {
	Type      string `json:"type"` // "session_stop"
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"` // user_requested|idle_timeout|host_draining|admin|error
}

// SessionSwapAppCmd swaps a running session's source app behind its interpipe
// boundary. App mirrors SessionAssignCmd.App; validation happened upstream. A
// rejected swap (ack ok:false) never fails the session — it stays running the
// previous app (agent-api.md).
type SessionSwapAppCmd struct {
	Type      string          `json:"type"` // "session_swap_app"
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	App       json.RawMessage `json:"app"`
}

// SessionDisplayUpdateCmd changes a running session's app-facing compositor
// presentation (agent-api.md `session_display_update`): render resolution and/or
// UI scale. Ephemeral, agent-held state — never written to `sessions`; the only
// authoritative readback is `session_metrics`. An ack{ok:false} or timeout is a
// no-op that never fails the session.
//
// Pairs are both-or-neither; nil is omitted so the agent reads "unchanged", not
// "set to zero". stream_width/stream_height are the exception that DOES move the
// encoded size (live scale-stage retarget), pre-validated against the rung
// ladder (internal/profile.IsRung); `sessions` width/height remain the launch
// size.
type SessionDisplayUpdateCmd struct {
	Type         string   `json:"type"` // "session_display_update"
	ID           string   `json:"id"`
	SessionID    string   `json:"session_id"`
	RenderWidth  *int32   `json:"render_width,omitempty"`
	RenderHeight *int32   `json:"render_height,omitempty"`
	StreamWidth  *int32   `json:"stream_width,omitempty"`
	StreamHeight *int32   `json:"stream_height,omitempty"`
	UIScale      *float64 `json:"ui_scale,omitempty"`
}

// SessionCaptureCmd arms one bounded, on-demand observation of a running
// session (agent-api.md `session_capture`). Observability only — no media-path
// insertion, no session-state change. The ack means armed, not done: the result
// arrives as a `session_trace_event` with `event: "diag.<kind>"`, joined by
// CaptureID, which the control plane mints before dispatch. Single-flight per
// session: a concurrent capture is nacked `busy`, never queued.
//
// Budget and Params are values, not pointers: every capture must be bounded,
// and an absent budget would read as "unbounded". The control plane clamps;
// the agent clamps again to its own ceilings.
type SessionCaptureCmd struct {
	Type      string        `json:"type"` // "session_capture"
	ID        string        `json:"id"`
	SessionID string        `json:"session_id"`
	CaptureID string        `json:"capture_id"`
	Kind      string        `json:"kind"` // pipeline_dot|encoder_props|burst_stats
	Budget    CaptureBudget `json:"budget"`
	// Params is read only by burst_stats; omitted entirely for the other kinds
	// so an agent never has to decide whether a zero means "none" or "default".
	Params *CaptureParams `json:"params,omitempty"`
}

// CaptureBudget bounds a capture in BOTH dimensions — bytes and wall clock.
// MaxBytes is the COMPRESSED payload cap: over it the agent truncates the
// uncompressed text at a line boundary, recompresses, and reports
// `truncated: true` with `original_bytes`.
type CaptureBudget struct {
	MaxBytes int32 `json:"max_bytes"`
	MaxMs    int32 `json:"max_ms"`
}

// CaptureParams is the burst_stats window plan: Windows samples of WindowMs
// each. Both are clamped by the control plane and again by the agent, and their
// product is additionally clamped to the budget's MaxMs.
type CaptureParams struct {
	Windows  int32 `json:"windows"`
	WindowMs int32 `json:"window_ms"`
}

// ConfigUpdateCmd pushes the host's full resolved runtime-knob block to the agent
// (agent-api.md `config_update`). Fire-and-forget; no ack. Sent once after
// `registered` and again on every admin change. Settings is the resolved knob map
// (hostcfg.Resolve); the agent overlays it and applies live knobs on the next
// session. Older agents ignore unknown message types.
type ConfigUpdateCmd struct {
	Type     string         `json:"type"` // "config_update"
	Settings map[string]any `json:"settings"`
	// Resolved console-mode config (CM-01, agent-api.md
	// `config_update.console_config`). Typed `any` rather than importing
	// internal/console — only the JSON shape is load-bearing.
	ConsoleConfig any `json:"console_config,omitempty"`
}

// RestartCmd asks the agent to exit so its container restart policy restarts it
// with fresh config (agent-api.md `restart`). Used when a restart-class knob
// (encoder/render-node/CUDA device) changes.
type RestartCmd struct {
	Type string `json:"type"` // "restart"
	ID   string `json:"id"`
}

// --- upstream callbacks (agent → control plane) ------------------------------

// AckMsg confirms receipt/acceptance of a command (by id). ok:false means the
// agent rejected/failed the command and carries an error.
type AckMsg struct {
	Type  string  `json:"type"` // "ack"
	ID    string  `json:"id"`
	OK    bool    `json:"ok"`
	Error *string `json:"error"`
}

// SessionStateMsg is the agent's authoritative lifecycle-progress callback.
type SessionStateMsg struct {
	Type      string  `json:"type"` // "session_state"
	SessionID string  `json:"session_id"`
	State     string  `json:"state"` // starting|running|stopping|stopped|failed
	Detail    string  `json:"detail"`
	Error     *string `json:"error"`
	// ReasonCode (agent-api.md `session_state.reason_code`) classifies a
	// terminal failure for the UI. Never load-bearing for the state machine —
	// the transition is driven by State alone.
	ReasonCode *string `json:"reason_code"`
	// App container log tail, oldest first; nil unless the failure warrants it.
	// The only copy: app containers run `--rm`, so the daemon has already
	// discarded the logs by the time anyone looks (#463).
	AppLogTail *string `json:"app_log_tail"`
}
