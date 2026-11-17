package session

import (
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// base is a fixed evaluation epoch for deterministic durations.
var healthEpoch = time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

func setpoint(v float64) (float64, bool) { return v, true }

func TestEvaluate_NoProfileFloorStaysHealthy(t *testing.T) {
	// A session with no profile (FloorKbps == 0) has no floor to enforce: even a
	// tiny setpoint with a stale below-floor run must resolve to healthy with the
	// run cleared.
	sp, ok := setpoint(50)
	res := Evaluate(HealthSample{
		Current:         HealthABRAtFloor,
		BelowFloorSince: healthEpoch.Add(-5 * time.Minute),
		SetpointKbps:    sp,
		HasSetpoint:     ok,
		FloorKbps:       0,
		Now:             healthEpoch,
	})
	if res.State != HealthHealthy {
		t.Fatalf("no-profile: state = %q, want healthy", res.State)
	}
	if !res.BelowFloorSince.IsZero() {
		t.Fatalf("no-profile: below-floor run not cleared: %v", res.BelowFloorSince)
	}
	if res.ShouldFail {
		t.Fatal("no-profile: ShouldFail = true, want false")
	}
}

func TestEvaluate_NoSetpointIsNonEvent(t *testing.T) {
	// A sample without a setpoint (ABR disarmed) must not disturb state or run.
	since := healthEpoch.Add(-45 * time.Second)
	res := Evaluate(HealthSample{
		Current:         HealthNetworkDegrading,
		BelowFloorSince: since,
		HasSetpoint:     false,
		FloorKbps:       4000,
		Now:             healthEpoch,
	})
	if res.State != HealthNetworkDegrading {
		t.Fatalf("no-setpoint: state = %q, want unchanged network_degrading", res.State)
	}
	if !res.BelowFloorSince.Equal(since) {
		t.Fatalf("no-setpoint: run = %v, want unchanged %v", res.BelowFloorSince, since)
	}
}

func TestEvaluate_AboveFloorHealthy(t *testing.T) {
	sp, ok := setpoint(5000)
	res := Evaluate(HealthSample{
		Current:      HealthHealthy,
		SetpointKbps: sp,
		HasSetpoint:  ok,
		FloorKbps:    4000,
		Now:          healthEpoch,
	})
	if res.State != HealthHealthy {
		t.Fatalf("above-floor: state = %q, want healthy", res.State)
	}
	if !res.BelowFloorSince.IsZero() {
		t.Fatalf("above-floor: run not cleared: %v", res.BelowFloorSince)
	}
}

func TestEvaluate_BelowFloorStartsRunButStaysHealthy(t *testing.T) {
	// First below-floor sample: the run starts at Now, but < 30s ⇒ still healthy.
	sp, ok := setpoint(4000) // at floor counts as below ("not above")
	res := Evaluate(HealthSample{
		Current:      HealthHealthy,
		SetpointKbps: sp,
		HasSetpoint:  ok,
		FloorKbps:    4000,
		Now:          healthEpoch,
	})
	if res.State != HealthHealthy {
		t.Fatalf("below<30s: state = %q, want healthy", res.State)
	}
	if !res.BelowFloorSince.Equal(healthEpoch) {
		t.Fatalf("below<30s: run = %v, want started at now %v", res.BelowFloorSince, healthEpoch)
	}
}

func TestEvaluate_Escalation(t *testing.T) {
	since := healthEpoch
	floor := int32(4000)
	cases := []struct {
		name      string
		elapsed   time.Duration
		wantState HealthState
		wantFail  bool
	}{
		{"29s healthy", 29 * time.Second, HealthHealthy, false},
		{"30s network_degrading", 30 * time.Second, HealthNetworkDegrading, false},
		{"89s network_degrading", 89 * time.Second, HealthNetworkDegrading, false},
		{"90s abr_at_floor", 90 * time.Second, HealthABRAtFloor, false},
		{"179s abr_at_floor", 179 * time.Second, HealthABRAtFloor, false},
		{"180s unsustainable", 180 * time.Second, HealthUnsustainable, true},
		{"300s unsustainable", 300 * time.Second, HealthUnsustainable, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp, ok := setpoint(3500) // below floor
			res := Evaluate(HealthSample{
				Current:         HealthHealthy,
				BelowFloorSince: since,
				SetpointKbps:    sp,
				HasSetpoint:     ok,
				FloorKbps:       floor,
				Now:             since.Add(tc.elapsed),
			})
			if res.State != tc.wantState {
				t.Fatalf("state = %q, want %q", res.State, tc.wantState)
			}
			if res.ShouldFail != tc.wantFail {
				t.Fatalf("ShouldFail = %v, want %v", res.ShouldFail, tc.wantFail)
			}
			// The run start is preserved across escalation samples.
			if !res.BelowFloorSince.Equal(since) {
				t.Fatalf("run = %v, want preserved %v", res.BelowFloorSince, since)
			}
			if tc.wantState != HealthHealthy && res.Reason == "" {
				t.Fatal("expected a non-empty reason on a degraded/unsustainable state")
			}
		})
	}
}

func TestEvaluate_RecoveryFromDegrading(t *testing.T) {
	// From any non-terminal degraded state, a single above-floor sample returns
	// to healthy and clears the run.
	for _, from := range []HealthState{HealthNetworkDegrading, HealthABRAtFloor} {
		sp, ok := setpoint(4500)
		res := Evaluate(HealthSample{
			Current:         from,
			BelowFloorSince: healthEpoch.Add(-2 * time.Minute),
			SetpointKbps:    sp,
			HasSetpoint:     ok,
			FloorKbps:       4000,
			Now:             healthEpoch,
		})
		if res.State != HealthHealthy {
			t.Fatalf("recovery from %q: state = %q, want healthy", from, res.State)
		}
		if !res.BelowFloorSince.IsZero() {
			t.Fatalf("recovery from %q: run not cleared: %v", from, res.BelowFloorSince)
		}
		if res.ShouldFail {
			t.Fatalf("recovery from %q: ShouldFail = true", from)
		}
	}
}

func TestEvaluate_SustainedFromFirstSample(t *testing.T) {
	// Time below floor is measured from the FIRST below-floor sample, not from
	// entry into the current state. A sample arriving with the run already 90s
	// old jumps straight to abr_at_floor even though Current is only healthy.
	since := healthEpoch.Add(-95 * time.Second)
	sp, ok := setpoint(3000)
	res := Evaluate(HealthSample{
		Current:         HealthHealthy,
		BelowFloorSince: since,
		SetpointKbps:    sp,
		HasSetpoint:     ok,
		FloorKbps:       4000,
		Now:             healthEpoch,
	})
	if res.State != HealthABRAtFloor {
		t.Fatalf("state = %q, want abr_at_floor (sustained from first sample)", res.State)
	}
}

func TestValidHealthState(t *testing.T) {
	all := []HealthState{
		HealthHealthy, HealthNetworkDegrading, HealthABRAtFloor,
		HealthClientDecodeDegrading, HealthClientPresentationDegrading,
		HealthUnsustainable, HealthFailed,
	}
	if len(all) != 7 {
		t.Fatalf("expected 7 states, got %d", len(all))
	}
	for _, s := range all {
		if !ValidHealthState(s) {
			t.Fatalf("ValidHealthState(%q) = false, want true", s)
		}
	}
	if ValidHealthState("bogus") {
		t.Fatal("ValidHealthState(bogus) = true, want false")
	}
}

// Amendment 79 (#1434): a sample is judged on the governor's unclamped link rate when it
// carries one, and on the setpoint only when it does not — the setpoint cannot fall under
// its floor, so a floor-pinned session is always "at or below" on it however much the link
// carries. A zero is not a measurement (the agent omits the key instead), so it falls back.
func TestStreamHealthRate(t *testing.T) {
	v := func(f float64) *float64 { return &f }
	table := []struct {
		what   string
		sample agentws.SessionMetricsMsg
		want   healthSignal
		wantOk bool
	}{
		{
			what:   "the per-report loop's rate wins over the setpoint",
			sample: agentws.SessionMetricsMsg{AbrSetpointKbps: v(577), AbrLinkRateKbps: v(2400)},
			want:   healthSignal{kbps: 2400, linkRate: true},
			wantOk: true,
		},
		{
			what:   "no link rate: the setpoint, as before the amendment",
			sample: agentws.SessionMetricsMsg{AbrSetpointKbps: v(577)},
			want:   healthSignal{kbps: 577},
			wantOk: true,
		},
		{
			what:   "a zero link rate is not a measurement",
			sample: agentws.SessionMetricsMsg{AbrSetpointKbps: v(4000), AbrLinkRateKbps: v(0)},
			want:   healthSignal{kbps: 4000},
			wantOk: true,
		},
		{
			what:   "neither: not an observation of the floor",
			sample: agentws.SessionMetricsMsg{},
			wantOk: false,
		},
	}
	for _, tc := range table {
		got, ok := streamHealthRate(tc.sample)
		if ok != tc.wantOk || got != tc.want {
			t.Errorf("%s: got %+v ok=%v, want %+v ok=%v", tc.what, got, ok, tc.want, tc.wantOk)
		}
	}
}
