package session

import (
	"testing"
	"time"
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
