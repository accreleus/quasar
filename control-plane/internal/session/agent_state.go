// agent_state.go — AgentState/AgentMetrics callbacks and terminal-metrics hygiene.
package session

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/accreleus/quasar/control-plane/internal/agentws"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// AgentSignalingFailure scopes a saturated agent-to-browser relay to its session.
// Stop the remote runner best-effort, then fail the authoritative row so its GPU
// reservation is released. A forged cross-host session id is ignored.
func (c *Coordinator) AgentSignalingFailure(ctx context.Context, hostID, sessionID, reason string) {
	hs, err := c.store.GetSessionHostState(ctx, sessionID)
	if err != nil || hs.HostID == nil || *hs.HostID != hostID || hs.State.IsTerminal() {
		return
	}
	cmd := agentws.SessionStopCmd{Type: "session_stop", ID: newCmdID(), SessionID: sessionID, Reason: "signaling_relay_failed"}
	if err := c.dispatcher.Send(hostID, cmd); err != nil {
		c.log.Warn("signaling-failure stop dispatch failed", "session_id", sessionID, "err", err)
	}
	c.failSession(sessionID, "signaling relay failed: "+reason)
}

// AgentState maps an agent session_state callback onto the state machine. The
// agent is authoritative for progress; an illegal transition is logged and
// dropped (it does not corrupt the row).
func (c *Coordinator) AgentState(ctx context.Context, hostID string, m agentws.SessionStateMsg) {
	to := State(m.State)
	switch to {
	case StateStarting, StateRunning, StateStopping, StateStopped, StateFailed:
		// ok
	default:
		c.log.Warn("ignoring unknown agent session state", "state", m.State, "session_id", m.SessionID)
		return
	}

	// A swap in flight rides within `running` via state_detail, so it must be
	// handled before the generic transition, which treats running→running as a
	// no-op and would drop the detail change and the app_id commit.
	if c.swapper.handleSwapCallback(ctx, m) {
		return
	}

	var detail, errMsg *string
	if m.Detail != "" {
		detail = &m.Detail
	}
	if m.Error != nil && *m.Error != "" {
		errMsg = m.Error
	}

	sess, err := c.store.Transition(ctx, m.SessionID, to, detail, errMsg)
	if errors.Is(err, ErrInvalidTransition) {
		c.log.Warn("agent reported illegal transition", "session_id", m.SessionID, "to", to, "err", err)
		return
	}
	if err != nil {
		c.log.Error("apply agent state failed", "session_id", m.SessionID, "to", to, "err", err)
		return
	}
	c.log.Info("session state", "session_id", sess.ID, "state", sess.State)

	// Failure classification and log tail, as a separate update outside the
	// lifecycle transaction: the transition must commit (releasing the GPU
	// reservation) whether or not this evidence lands, and threading a hundred
	// lines of app log through it would sit inside the row lock the scheduler
	// contends on.
	//
	// Guarded on the POST-transition state, never on what the agent reported:
	// Transition coerces a `failed` report on an already-`stopping` row to
	// `stopped` and clears error_message, so keying off the message would
	// re-attach failure_code and render an operator stop as an app crash.
	if sess.State == StateFailed && (m.ReasonCode != nil || m.AppLogTail != nil) {
		if err := c.store.SetFailureDetail(ctx, sess.ID, m.ReasonCode, m.AppLogTail); err != nil {
			c.log.Warn("failure detail write failed", "session_id", sess.ID, "err", err)
		}
	}
	// Keyed on the POST-transition state for the same reason the block above is:
	// a `failed` report on an already-`stopping` row is coerced to `stopped`, and
	// auditing what the agent said would call an operator stop an app crash.
	// m.ReasonCode, not sess.FailureCode — SetFailureDetail lands after the read.
	if sess.State == StateFailed {
		c.recordSessionFailed(ctx, sess, "agent", m.ReasonCode, "")
	}

	if sess.State.IsTerminal() {
		// reason_source is always "agent" here: every terminal transition reaching
		// this function was agent-reported (§3.4). The line exists so an operator
		// can grep for how and why a session ended, state_detail included.
		c.log.Info("session ended",
			"session_id", sess.ID,
			"state", sess.State,
			"state_detail", deref(sess.StateDetail),
			"reason_source", "agent",
		)
		// No telemetry prune: terminal FREEZES telemetry for the post-mortem
		// retention (internal/telemetry) and the janitor sweeps it.
		//
		// Drop every per-session in-memory map, or a normally-completed session
		// leaks an entry in each.
		c.health.forget(sess.ID)
		c.display.forget(sess.ID)
		c.swapper.forget(sess.ID) // #405: any orphaned pending swap
		// #402: a headless session never registers a browser, so the relay's
		// signaling buffer has no other eviction path.
		c.forgetTerminalSession(sess.ID)
		// Re-eval console auto-start now rather than at the next capacity report:
		// a static display never produces one on its own.
		c.fireConsoleReeval(sess.HostID, sess.ID)
		// Stamp the home's last_used_at. Best-effort, a no-op for a non-managed app.
		if c.homes != nil && sess.HostID != nil {
			uid, aid, hid := sess.UserID, sess.AppID, *sess.HostID
			go func() {
				if err := c.homes.TouchUsed(context.Background(), uid, aid, hid); err != nil {
					c.log.Warn("home touch failed", "session_id", sess.ID, "err", err)
				}
			}()
		}
	}
}

// AgentMetrics ingests a session_metrics sample under the per-host trust
// boundary: an agent on host A must never write host B's metrics. A mismatch, a
// session not running here, an unknown session or a malformed sample is dropped
// silently — telemetry never resurrects or alters a session, and a bad sample
// must not fatal the WS (agent-api.md). Fire-and-forget. Implements agentws.Events.
func (c *Coordinator) AgentMetrics(ctx context.Context, hostID string, m agentws.SessionMetricsMsg) {
	if m.SessionID == "" {
		return
	}
	hs, err := c.store.GetSessionHostState(ctx, m.SessionID)
	if err != nil {
		return // unknown session or read error: never stored (agent-api.md)
	}
	// Trust boundary: the session must be placed on THIS host.
	if hs.HostID == nil || *hs.HostID != hostID {
		c.log.Warn("dropping cross-host session_metrics",
			"session_id", m.SessionID, "reporting_host", hostID, "session_host", deref(hs.HostID))
		return
	}
	// A sample after the session is terminal is dropped (agent-api.md), but
	// `stopping` is accepted: an operator DELETE moves the row there BEFORE the
	// agent tears down, and the pre-terminal bytes_used sample lands in exactly
	// that window.
	if hs.State != StateRunning && hs.State != StateStopping {
		return
	}

	// Fold the external-resolution readback into the cache BEFORE the fallible
	// insert: the cache validates the next display request and is what the Session
	// resource serializes, so it must not be hostage to a metrics write.
	c.display.observe(m.SessionID, m.StreamWidth, m.StreamHeight, m.ExternalResizeSupported, m.ExternalOwner)

	metrics := buildAgentMetrics(m)
	// Append only; retention is the telemetry janitor's job. An ingest that also
	// issued a DELETE ran ~0.4 near-always-empty deletes a second per session on
	// the hot path.
	if err := c.store.Telemetry().Append(ctx, m.SessionID, telemetry.SourceAgent,
		telemetry.SampleInput{TsUnixMs: m.TsUnixMs, Metrics: metrics}); err != nil {
		c.log.Warn("insert agent metric failed", "session_id", m.SessionID, "err", err)
		return
	}
	// Stream health from the ABR setpoint against the session's profile floor. Own
	// goroutine, failures logged not propagated: it must add no latency to the
	// metrics hot path.
	if hs.State == StateRunning && m.AbrSetpointKbps != nil {
		setpoint := *m.AbrSetpointKbps
		sid := m.SessionID
		go c.health.evaluateHealth(context.Background(), sid, setpoint)
	}

	// Home usage, when the pre-terminal sample carries bytes_used. Best-effort
	// goroutine: a miss must not block or fail the metrics path.
	if m.BytesUsed != nil && c.homes != nil {
		bu := *m.BytesUsed
		sid := m.SessionID
		go func() {
			if err := c.homes.ReportBytesUsed(context.Background(), sid, bu); err != nil {
				c.log.Warn("report home bytes_used failed", "session_id", sid, "err", err)
			}
		}()
	}
}

// AgentTraceEvent ingests an agent session_trace_event under the same per-host
// trust boundary as AgentMetrics (AgentTraceEventAllowed). A failed insert is
// logged and must never fatal the WS; a cross-host or not-running session is
// dropped silently. Implements agentws.Events.
func (c *Coordinator) AgentTraceEvent(ctx context.Context, hostID string, m agentws.SessionTraceEventMsg) {
	if m.SessionID == "" || m.Event == "" {
		return
	}
	allowed, err := c.store.AgentTraceEventAllowed(ctx, hostID, m.SessionID)
	if err != nil {
		c.log.Warn("agent_trace_event ownership check failed",
			"session_id", m.SessionID, "host_id", hostID, "err", err)
		return
	}
	if !allowed {
		c.log.Debug("dropping agent_trace_event: not owned/running on this host",
			"session_id", m.SessionID, "host_id", hostID, "event", m.Event)
		return
	}
	if err := c.store.Telemetry().AppendEvent(ctx, m.SessionID, telemetry.SourceAgent,
		telemetry.EventInput{TsUnixMs: m.TsUnixMs, Type: m.Event, Payload: m.Payload}); err != nil {
		c.log.Warn("insert agent trace event failed",
			"session_id", m.SessionID, "event", m.Event, "err", err)
		return
	}
}

// buildAgentMetrics flattens an agent session_metrics message into the schema.md
// `metrics` JSONB object. Unset fields are omitted (a reporter sends what it has).
func buildAgentMetrics(m agentws.SessionMetricsMsg) json.RawMessage {
	obj := make(map[string]any, 6)
	if m.FPS != nil {
		obj["fps"] = *m.FPS
	}
	if m.BitrateKbps != nil {
		obj["bitrate_kbps"] = *m.BitrateKbps
	}
	if m.EncodeMs != nil {
		obj["encode_ms"] = *m.EncodeMs
	}
	for key, value := range map[string]*float64{
		"encode_ms_p50":                m.EncodeMsP50,
		"encode_ms_p95":                m.EncodeMsP95,
		"encode_ms_max":                m.EncodeMsMax,
		"source_fps":                   m.SourceFPS,
		"compositor_fps":               m.CompositorFPS,
		"compositor_pts_delta_p50_ms":  m.CompositorPTSDeltaP50Ms,
		"compositor_pts_delta_p95_ms":  m.CompositorPTSDeltaP95Ms,
		"interpipe_queue_dwell_p50_ms": m.InterpipeQueueDwellP50Ms,
		"interpipe_queue_dwell_p95_ms": m.InterpipeQueueDwellP95Ms,
		"rtp_fps":                      m.RTPFPS,
		"rtp_bitrate_kbps":             m.RTPBitrateKbps,
		// Host-stage latency probe (QUASAR_LATENCY_PROBE). A nil pointer is omitted
		// by the loop, so a probe-off agent writes the same metrics object.
		"probe_capture_to_enc_in_p50_ms":         m.ProbeCaptureToEncInP50Ms,
		"probe_capture_to_enc_in_p95_ms":         m.ProbeCaptureToEncInP95Ms,
		"probe_enc_out_to_send_p50_ms":           m.ProbeEncOutToSendP50Ms,
		"probe_enc_out_to_send_p95_ms":           m.ProbeEncOutToSendP95Ms,
		"probe_pay_to_send_p50_ms":               m.ProbePayToSendP50Ms,
		"probe_pay_to_send_p95_ms":               m.ProbePayToSendP95Ms,
		"probe_pts_to_emit_p50_ms":               m.ProbePTSToEmitP50Ms,
		"probe_pts_to_emit_p95_ms":               m.ProbePTSToEmitP95Ms,
		"probe_compositor_frame_interval_p95_ms": m.ProbeCompositorFrameIntervalP95Ms,
		"probe_send_desyncs":                     m.ProbeSendDesyncs,
		"probe_pts_unmatched":                    m.ProbePTSUnmatched,
	} {
		if value != nil {
			obj[key] = *value
		}
	}
	for key, value := range map[string]*int64{
		"interpipe_queue_level_max": m.InterpipeQueueLevelMax,
		"interpipe_queue_drops":     m.InterpipeQueueDrops,
	} {
		if value != nil {
			obj[key] = *value
		}
	}
	if m.FramesEncoded != nil {
		obj["frames_encoded"] = *m.FramesEncoded
	}
	if m.FramesDropped != nil {
		obj["frames_dropped"] = *m.FramesDropped
	}
	if m.AbrSetpointKbps != nil {
		obj["abr_setpoint_kbps"] = *m.AbrSetpointKbps
	}
	if m.GccEstimateKbps != nil {
		obj["gcc_estimate_kbps"] = *m.GccEstimateKbps
	}
	// The ladder-followed ABR floor, present only off the launch floor.
	if m.AbrFloorKbps != nil {
		obj["abr_floor_kbps"] = *m.AbrFloorKbps
	}
	// The agent's reported ABR mode and adaptation-classifier label, stored
	// verbatim so the diagnostic bundle needs no heuristic derivation. An older
	// agent sends "" (omitted by its skip_if_empty convention), which stays absent
	// rather than being stored as an empty string.
	if m.AbrMode != "" {
		obj["abr_mode"] = m.AbrMode
	}
	if m.AdaptationState != "" {
		obj["adaptation_state"] = m.AdaptationState
	}
	// The live render size / UI scale readback, present only when the agent
	// reports a non-default value (agent-api.md §session_metrics).
	if m.RenderWidth != nil {
		obj["render_width"] = *m.RenderWidth
	}
	if m.RenderHeight != nil {
		obj["render_height"] = *m.RenderHeight
	}
	if m.UIScale != nil {
		obj["ui_scale"] = *m.UIScale
	}
	// The live EXTERNAL (encoded) size, present only when it differs from the
	// launch size, plus the encoder's live-resize capability. The bundle needs
	// what was actually encoded in a window; sessions.width/height only ever says
	// what the session launched at.
	if m.StreamWidth != nil {
		obj["stream_width"] = *m.StreamWidth
	}
	if m.StreamHeight != nil {
		obj["stream_height"] = *m.StreamHeight
	}
	if m.ExternalResizeSupported != nil {
		obj["external_resize_supported"] = *m.ExternalResizeSupported
	}
	// The ladder's live per-session state, verbatim, for the same reason.
	if m.LadderSpeedBias != nil {
		obj["ladder_speed_bias"] = *m.LadderSpeedBias
	}
	if m.LadderResRung != nil {
		obj["ladder_res_rung"] = *m.LadderResRung
	}
	if m.LadderFps != nil {
		obj["ladder_fps"] = *m.LadderFps
	}
	if m.ExternalOwner != "" {
		obj["external_owner"] = m.ExternalOwner
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}
