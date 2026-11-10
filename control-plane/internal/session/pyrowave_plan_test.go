package session

// Ultra-low latency (PyroWave) in the stream plan (control-api.md §Ultra-low latency (PyroWave)
// ▸ The stream plan, amendment 58): pure table tests, no database.

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// pwProbe can decode PyroWave and carries a fresh LAN measurement.
func pwProbe() *DeviceProbe {
	p := wtProbe()
	p.WebGPU = true
	p.BandwidthKbps, p.RTTMsExact, p.MeasuredAt = 900_000, 0.6, fixedNow.Add(-time.Minute)
	return p
}

// pwInputs is a placed 1080p60 launch on a PyroWave-capable GPU holding a PyroWave slot.
func pwInputs(mode, transport string, explicit bool) StreamInputs {
	in := wtInputs(transport, "", true, pwProbe())
	in.Chain.PyroWaveMode = mode
	in.GPUCodecs = []string{"h264", "pyrowave"}
	in.Params.PyroWave = explicit
	in.HoldsPyroWaveSlot = true
	return in
}

func TestPlanStreamPyroWave(t *testing.T) {
	str := func(s string) *string { return &s }
	tests := []struct {
		name          string
		in            func() StreamInputs
		wantTransport string
		wantAllowed   bool
		wantWanted    bool
		wantReason    *string
		wantChoice    *string
		wantAuto      *string
		wantKeepSlot  bool
	}{
		{name: "chosen: an explicit pick on an allow profile",
			in:            func() StreamInputs { return pwInputs("allow", "webtransport_opt_in", true) },
			wantTransport: "webtransport", wantAllowed: true, wantWanted: true, wantChoice: str("explicit"), wantKeepSlot: true},
		{name: "refused: the device has no WebGPU",
			in: func() StreamInputs {
				in := pwInputs("allow", "webtransport_opt_in", true)
				in.Probe.WebGPU = false
				return in
			},
			wantTransport: "webrtc", wantReason: str("device_no_webgpu")},
		{name: "refused: the placed GPU has no PyroWave",
			in: func() StreamInputs {
				in := pwInputs("allow", "webtransport_opt_in", true)
				in.GPUCodecs = []string{"h264", "h265"}
				return in
			},
			wantTransport: "webrtc", wantReason: str("host_no_pyrowave")},
		{name: "refused: the profile is off",
			in:            func() StreamInputs { return pwInputs("off", "webtransport", true) },
			wantTransport: "webtransport", wantReason: str("profile_disallows")},
		{name: "refused: allow on a webrtc profile",
			in:            func() StreamInputs { return pwInputs("allow", "webrtc", true) },
			wantTransport: "webrtc", wantReason: str("profile_disallows")},
		{name: "refused: no slot left on the GPU",
			in: func() StreamInputs {
				in := pwInputs("allow", "webtransport_opt_in", true)
				in.HoldsPyroWaveSlot = false
				return in
			},
			wantTransport: "webrtc", wantReason: str("gpu_pyrowave_busy")},
		{name: "refused: wanted, but WebTransport could not be had",
			in: func() StreamInputs {
				in := pwInputs("allow", "webtransport_opt_in", true)
				in.Transport.Endpoint = nil
				return in
			},
			wantTransport: "webrtc", wantReason: str("not_webtransport")},
		{name: "allowed but not wanted: an Auto launch on allow",
			in:            func() StreamInputs { return pwInputs("allow", "webtransport", false) },
			wantTransport: "webtransport", wantAllowed: true},
		{name: "allowed but not wanted: an explicit standard codec on prefer",
			in: func() StreamInputs {
				in := pwInputs("prefer", "webtransport", false)
				in.Override.Codec = str("h264")
				return in
			},
			wantTransport: "webtransport", wantAllowed: true},
		{name: "Auto picks it on prefer",
			in:            func() StreamInputs { return pwInputs("prefer", "webtransport_opt_in", false) },
			wantTransport: "webtransport", wantAllowed: true, wantWanted: true, wantChoice: str("auto"), wantKeepSlot: true},
		{name: "Auto: a stale measurement",
			in: func() StreamInputs {
				in := pwInputs("prefer", "webtransport", false)
				in.Probe.MeasuredAt = fixedNow.Add(-11 * time.Minute)
				return in
			},
			wantTransport: "webtransport", wantAllowed: true, wantAuto: str("no_measurement")},
		{name: "Auto: a round trip over 5 ms",
			in: func() StreamInputs {
				in := pwInputs("prefer", "webtransport", false)
				in.Probe.RTTMsExact = 5.4
				return in
			},
			wantTransport: "webtransport", wantAllowed: true, wantAuto: str("rtt_too_high")},
		{name: "Auto: bandwidth under the target",
			in: func() StreamInputs {
				in := pwInputs("prefer", "webtransport", false)
				in.Probe.BandwidthKbps = 199_999
				return in
			},
			wantTransport: "webtransport", wantAllowed: true, wantAuto: str("link_too_slow")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planStream(tc.in())
			if err != nil {
				t.Fatalf("planStream: %v", err)
			}
			got := plan.PyroWave
			if plan.Update.Transport != tc.wantTransport || got.Allowed != tc.wantAllowed || got.Wanted != tc.wantWanted ||
				!eqStr(got.Reason, tc.wantReason) || !eqStr(got.Choice, tc.wantChoice) || !eqStr(got.AutoReason, tc.wantAuto) ||
				plan.Update.KeepPyroWaveSlot != tc.wantKeepSlot {
				t.Errorf("transport %q, record %s, keep slot %v", plan.Update.Transport, mustJSON(t, got), plan.Update.KeepPyroWaveSlot)
			}
			if plan.Update.Codec != "h264" {
				t.Errorf("PyroWave changed the standard codec: %s", plan.Update.Codec)
			}
			if (got.TargetKbps != nil) != tc.wantAllowed {
				t.Errorf("target_kbps %v with allowed %v", got.TargetKbps, got.Allowed)
			}
			if string(plan.Update.PyroWave) != mustJSON(t, got) {
				t.Errorf("the write carries %s, the plan %s", plan.Update.PyroWave, mustJSON(t, got))
			}
		})
	}
}

func TestPyroWaveGrantFollowsTheRecord(t *testing.T) {
	plan, err := planStream(pwInputs("allow", "webtransport_opt_in", true))
	if err != nil {
		t.Fatal(err)
	}
	g := plan.PyroWave.grant()
	if g == nil || !g.Wanted || g.TargetKbps != 200_000 || g.FloorKbps != 100_000 || g.MaxRTTMs != 5 {
		t.Fatalf("grant = %+v", g)
	}
	b, _ := json.Marshal(g)
	if string(b) != `{"wanted":true,"target_kbps":200000,"floor_kbps":100000,"max_rtt_ms":5}` {
		t.Errorf("session_assign.pyrowave = %s", b)
	}
	refused, _ := planStream(pwInputs("off", "webtransport", true))
	if refused.PyroWave.grant() != nil {
		t.Error("a refused session got a grant")
	}
	var none *PyroWaveRecord
	if none.grant() != nil {
		t.Error("a session with no record got a grant")
	}
}

func TestPyroWaveWantedSessionWantsWebTransportOnAnOptInProfile(t *testing.T) {
	in := pwInputs("allow", "webtransport_opt_in", true)
	in.Transport.Requested = "webrtc"
	plan, _ := planStream(in)
	if plan.Update.Transport != "webtransport" {
		t.Errorf("transport = %q: a session that wants PyroWave wants WebTransport, whatever the request says", plan.Update.Transport)
	}
	in.Params.parked = true
	plan, _ = planStream(in)
	if plan.Update.Transport != "webrtc" || plan.PyroWave.Allowed {
		t.Errorf("a parked session got %q / %+v", plan.Update.Transport, plan.PyroWave)
	}
}

// An inherited transport is the effective one: a default profile on a webtransport instance
// can offer PyroWave, on a webrtc instance it cannot.
func TestPyroWaveFollowsAnInheritedTransport(t *testing.T) {
	for _, tc := range []struct {
		setting     string
		wantAllowed bool
	}{{"webtransport", true}, {"webrtc", false}} {
		in := pwInputs("allow", tc.setting, true)
		in.Chain.TransportInherited = true
		if got := pyrowaveCandidate(in.Chain, in.Params); got != tc.wantAllowed {
			t.Errorf("%s: candidate = %v, want %v", tc.setting, got, tc.wantAllowed)
		}
		plan, _ := planStream(in)
		if plan.PyroWave.Allowed != tc.wantAllowed {
			t.Errorf("%s: allowed = %v, want %v", tc.setting, plan.PyroWave.Allowed, tc.wantAllowed)
		}
	}
}

func TestPyroWaveTargetKbps(t *testing.T) {
	cap := int32(250_000)
	for _, tc := range []struct {
		h, fps int32
		max    *int32
		want   int32
	}{
		{1080, 60, nil, 200_000},
		{720, 30, nil, 100_000},
		{1440, 60, nil, 300_000},
		{2160, 60, nil, 400_000},
		{2160, 144, nil, 960_000},
		{2160, 240, nil, 1_000_000},
		{1440, 60, &cap, 250_000},
	} {
		if got := pyrowaveTargetKbps(tc.h, tc.fps, tc.max); got != tc.want {
			t.Errorf("%dp%d max %v: %d, want %d", tc.h, tc.fps, tc.max, got, tc.want)
		}
	}
}

func TestPyroWaveCandidate(t *testing.T) {
	in := pwInputs("allow", "webtransport_opt_in", true)
	if !pyrowaveCandidate(in.Chain, in.Params) {
		t.Error("an explicit pick on allow is a candidate")
	}
	in.Params.PyroWave = false
	if pyrowaveCandidate(in.Chain, in.Params) {
		t.Error("an Auto launch on allow takes no slot")
	}
	in.Chain.PyroWaveMode = "prefer"
	if !pyrowaveCandidate(in.Chain, in.Params) {
		t.Error("an Auto launch on prefer is a candidate")
	}
	in.Chain.Transport = "webrtc"
	if pyrowaveCandidate(in.Chain, in.Params) {
		t.Error("a webrtc profile never takes a slot")
	}
}

func TestParseDeviceProbeWebGPUAndTheExactRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	p := parseDeviceProbe([]byte(`{"measured_at":"` + now.Format(time.RFC3339) + `","rtt_ms":0.4,"webgpu":{"available":true}}`))
	if p == nil || !p.WebGPU || p.RTTMsExact != 0.4 || p.RTTMs != 0 || !p.MeasuredAt.Equal(now) {
		t.Fatalf("probe = %+v", p)
	}
	for _, caps := range []string{`"webgpu":{"available":"true"}`, `"webgpu":true`, `"x":1`} {
		if p := parseDeviceProbe([]byte(`{"measured_at":"` + now.Format(time.RFC3339) + `",` + caps + `}`)); p == nil || p.WebGPU {
			t.Errorf("%s: probe = %+v", caps, p)
		}
	}
}

func eqStr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Seat A round 2: a claim whose re-plan without the slot fails is never granted PyroWave.
func TestAClaimWhoseReplanFailsIsBusyOnWebRTC(t *testing.T) {
	first, err := planStream(pwInputs("allow", "webtransport_opt_in", true))
	if err != nil || !first.Update.KeepPyroWaveSlot || first.Update.Transport != "webtransport" {
		t.Fatalf("first plan: %+v (%v)", first.Update, err)
	}
	firstStream := placedStream{plan: &first}
	again := placedStream{plan: &StreamPlan{}}
	if got := claimWithoutSlot(firstStream, again, nil); got.plan != again.plan {
		t.Error("a re-plan that worked is used as it is")
	}
	got := claimWithoutSlot(firstStream, placedStream{}, errors.New("db down")).plan
	if got.PyroWave.Allowed || got.PyroWave.grant() != nil || got.PyroWave.Reason == nil ||
		*got.PyroWave.Reason != "gpu_pyrowave_busy" || got.Update.KeepPyroWaveSlot ||
		got.Update.Transport != "webrtc" || got.Endpoint != nil {
		t.Errorf("failed re-plan: transport %q keep %v record %+v", got.Update.Transport, got.Update.KeepPyroWaveSlot, got.PyroWave)
	}
	if string(got.Update.PyroWave) != mustJSON(t, got.PyroWave) {
		t.Errorf("the write carries %s", got.Update.PyroWave)
	}
	if !first.Update.KeepPyroWaveSlot || first.Update.Transport != "webtransport" {
		t.Error("the first plan was changed in place")
	}
}
