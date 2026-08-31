package session

import (
	"testing"
	"time"
)

// AS10-11 — the pure client-health consumption state machine. The browser
// classifies; this maps the class into the health machine + cert outcomes, with
// network precedence, the hidden guard, and sustained-run gating.

func TestClientHealth_NetworkStateWins(t *testing.T) {
	now := time.Now()
	// Even a sustained decode-degrading run is suppressed while network-degraded.
	for _, st := range []HealthState{HealthNetworkDegrading, HealthABRAtFloor, HealthUnsustainable, HealthFailed} {
		run := &clientHealthRun{class: ClientHealthDecode, since: now.Add(-time.Minute)}
		d := evaluateClientHealthSample(st, run, ClientHealthDecode, now)
		if d.Run != nil || d.SetState != "" || d.RecordFail != "" {
			t.Fatalf("state %s: network must win (got %+v)", st, d)
		}
	}
}

func TestClientHealth_HiddenNeverFails(t *testing.T) {
	now := time.Now()
	// A long-standing degraded run, then a backgrounded sample → run cleared, no
	// state, no fail recorded. This is the #1 false-positive guard.
	run := &clientHealthRun{class: ClientHealthPresentation, since: now.Add(-time.Hour)}
	d := evaluateClientHealthSample(HealthHealthy, run, ClientHealthBackgrounded, now)
	if d.Run != nil {
		t.Error("hidden must clear the run")
	}
	if d.SetState != "" || d.RecordFail != "" {
		t.Fatalf("hidden must not flip state or record a fail: %+v", d)
	}
}

func TestClientHealth_SustainedDecodeFails(t *testing.T) {
	now := time.Now()
	// A fresh decode-degrading sample starts a run but does NOT act yet.
	d := evaluateClientHealthSample(HealthHealthy, nil, ClientHealthDecode, now)
	if d.SetState != "" || d.RecordFail != "" {
		t.Fatalf("first degraded sample must not act: %+v", d)
	}
	if d.Run == nil || d.Run.class != ClientHealthDecode {
		t.Fatalf("run not started: %+v", d.Run)
	}

	// After the sustain window, it flips state and records a fail.
	run := &clientHealthRun{class: ClientHealthDecode, since: now.Add(-clientDegradeSustainFor - time.Second)}
	d = evaluateClientHealthSample(HealthHealthy, run, ClientHealthDecode, now)
	if d.SetState != HealthClientDecodeDegrading {
		t.Fatalf("sustained decode: got state %q want client_decode_degrading", d.SetState)
	}
	if d.RecordFail != ClientHealthDecode {
		t.Fatalf("sustained decode: got record-fail %q want %q", d.RecordFail, ClientHealthDecode)
	}
}

func TestClientHealth_UnsupportedMapsToDecodeState(t *testing.T) {
	now := time.Now()
	run := &clientHealthRun{class: ClientHealthUnsupported, since: now.Add(-clientDegradeSustainFor - time.Second)}
	d := evaluateClientHealthSample(HealthHealthy, run, ClientHealthUnsupported, now)
	if d.SetState != HealthClientDecodeDegrading {
		t.Fatalf("unsupported should map to client_decode_degrading, got %q", d.SetState)
	}
	if d.RecordFail != ClientHealthUnsupported {
		t.Fatalf("unsupported record-fail %q", d.RecordFail)
	}
}

func TestClientHealth_SustainedPresentationFails(t *testing.T) {
	now := time.Now()
	run := &clientHealthRun{class: ClientHealthPresentation, since: now.Add(-clientDegradeSustainFor - time.Second)}
	d := evaluateClientHealthSample(HealthHealthy, run, ClientHealthPresentation, now)
	if d.SetState != HealthClientPresentationDegrading {
		t.Fatalf("got state %q want client_presentation_degrading", d.SetState)
	}
}

func TestClientHealth_SustainedSmoothRecovers(t *testing.T) {
	now := time.Now()
	// A sustained smooth run while in a client-degraded state → clear to healthy AND
	// certify a pass (latest-outcome-wins clears a prior fail).
	run := &clientHealthRun{class: ClientHealthSmooth, since: now.Add(-clientSmoothSustainFor - time.Second)}
	d := evaluateClientHealthSample(HealthClientDecodeDegrading, run, ClientHealthSmooth, now)
	if !d.ClearToHealthy {
		t.Error("sustained smooth from a client-degraded state must clear to healthy")
	}
	if !d.RecordPass {
		t.Error("sustained smooth must certify a pass")
	}
	if d.RecordFail != "" {
		t.Error("smooth must not record a fail")
	}
}

func TestClientHealth_TransientDoesNotFlip(t *testing.T) {
	now := time.Now()
	// Decode-degrading then smooth before the sustain window → run resets, no fail.
	run := &clientHealthRun{class: ClientHealthDecode, since: now.Add(-5 * time.Second)}
	d := evaluateClientHealthSample(HealthHealthy, run, ClientHealthSmooth, now)
	if d.RecordFail != "" || d.SetState != "" {
		t.Fatalf("a switch to smooth must not fail/flip: %+v", d)
	}
	// The new smooth run is too young to certify a pass yet.
	if d.RecordPass {
		t.Error("young smooth run must not certify a pass yet")
	}
}

// TestCertFailCodec — 0032 codec scoping: decode-side classes indict the
// session codec; presentation is profile-level; unknown classes are profile-level.
func TestCertFailCodec(t *testing.T) {
	cases := []struct {
		class, sessionCodec, want string
	}{
		{ClientHealthUnsupported, "h265", "h265"}, // the Tower 2026-07-24 case
		{ClientHealthUnsupported, "h264", "h264"}, // floor fail — profile-blocks via ProfileFailures
		{ClientHealthDecode, "av1", "av1"},        // slow software AV1 decode is codec-specific
		{ClientHealthPresentation, "h265", ""},    // display-side: genuinely profile-level
		{"future_class", "h265", ""},
	}
	for _, c := range cases {
		if got := certFailCodec(c.class, c.sessionCodec); got != c.want {
			t.Errorf("certFailCodec(%q, %q) = %q, want %q", c.class, c.sessionCodec, got, c.want)
		}
	}
}
