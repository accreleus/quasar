package session

import (
	"encoding/json"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// TestBuildAgentMetricsPassesThroughLadderKeys covers the
// abr-resolution-fps-ladder amendment: the ladder's live per-session state
// (speed bias, engaged resolution rung, external-size ownership) is stored
// verbatim into the metrics JSONB when the agent reports it, and omitted
// entirely (no key, not a zero value) when it doesn't.
func TestBuildAgentMetricsPassesThroughLadderKeys(t *testing.T) {
	bias, rung, fps := int32(2), int32(1), int32(60)
	raw := buildAgentMetrics(agentws.SessionMetricsMsg{
		LadderSpeedBias: &bias, LadderResRung: &rung, LadderFps: &fps,
		ExternalOwner: "pinned",
	})
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["ladder_speed_bias"] != float64(2) || got["ladder_res_rung"] != float64(1) {
		t.Fatalf("ladder keys not passed through: %#v", got)
	}
	if got["ladder_fps"] != float64(60) {
		t.Fatalf("ladder_fps not passed through: %#v", got)
	}
	if got["external_owner"] != "pinned" {
		t.Fatalf("external_owner not passed through: %#v", got)
	}

	raw = buildAgentMetrics(agentws.SessionMetricsMsg{})
	got = map[string]any{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ladder_speed_bias", "ladder_res_rung", "ladder_fps", "external_owner"} {
		if _, ok := got[k]; ok {
			t.Fatalf("%s must be omitted when the agent does not report it", k)
		}
	}
}

// TestBuildAgentMetricsPassesThroughAbrFloor covers amendment 5: the governor's
// ladder-followed ABR floor is stored verbatim when reported, and omitted entirely
// when the session is still at its launch floor (the agent sends no key then).
func TestBuildAgentMetricsPassesThroughAbrFloor(t *testing.T) {
	floor := 1130.0
	raw := buildAgentMetrics(agentws.SessionMetricsMsg{AbrFloorKbps: &floor})
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["abr_floor_kbps"] != 1130.0 {
		t.Fatalf("abr_floor_kbps not passed through: %#v", got)
	}

	raw = buildAgentMetrics(agentws.SessionMetricsMsg{})
	got = map[string]any{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["abr_floor_kbps"]; ok {
		t.Fatal("abr_floor_kbps must be omitted at the launch floor")
	}
}

// TestBuildAgentMetricsPassesThroughLatencyProbe covers the host-stage latency
// probe keys (agent `QUASAR_LATENCY_PROBE`, default off): stored verbatim when the
// agent reports them, and — the part that matters — entirely absent otherwise, so a
// probe-off session's metrics object is unchanged from before the probe existed. A
// zero would be indistinguishable from a real 0 ms stage in any downstream average.
func TestBuildAgentMetricsPassesThroughLatencyProbe(t *testing.T) {
	capIn, encPay, paySend, ptsEmit, interval := 3.5, 0.4, 1.2, 0.1, 17.8
	raw := buildAgentMetrics(agentws.SessionMetricsMsg{
		ProbeCaptureToEncInP50Ms:          &capIn,
		ProbeEncOutToSendP50Ms:            &encPay,
		ProbePayToSendP95Ms:               &paySend,
		ProbePTSToEmitP50Ms:               &ptsEmit,
		ProbeCompositorFrameIntervalP95Ms: &interval,
	})
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]float64{
		"probe_capture_to_enc_in_p50_ms":         3.5,
		"probe_enc_out_to_send_p50_ms":           0.4,
		"probe_pay_to_send_p95_ms":               1.2,
		"probe_pts_to_emit_p50_ms":               0.1,
		"probe_compositor_frame_interval_p95_ms": 17.8,
	} {
		if got[k] != want {
			t.Fatalf("%s not passed through: %#v", k, got[k])
		}
	}
	// Reported stages only — a stage the window never sampled stays absent.
	if _, ok := got["probe_capture_to_enc_in_p95_ms"]; ok {
		t.Fatal("an unreported probe stage must be omitted, not zero-filled")
	}

	raw = buildAgentMetrics(agentws.SessionMetricsMsg{})
	got = map[string]any{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"probe_capture_to_enc_in_p50_ms", "probe_capture_to_enc_in_p95_ms",
		"probe_enc_out_to_send_p50_ms", "probe_enc_out_to_send_p95_ms",
		"probe_pay_to_send_p50_ms", "probe_pay_to_send_p95_ms",
		"probe_pts_to_emit_p50_ms", "probe_pts_to_emit_p95_ms",
		"probe_compositor_frame_interval_p95_ms",
		"probe_send_desyncs", "probe_pts_unmatched",
	} {
		if _, ok := got[k]; ok {
			t.Fatalf("%s must be omitted when the latency probe is off", k)
		}
	}
}
