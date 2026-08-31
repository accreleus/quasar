package profile

import "testing"

// evalByID returns the launch profile's verdict from a LaunchEvaluation.
func evalByID(t *testing.T, ev LaunchEvaluation, id string) LaunchProfileEval {
	t.Helper()
	for _, pe := range ev.Profiles {
		if pe.LaunchProfile.ID == id {
			return pe
		}
	}
	t.Fatalf("launch profile %q not in evaluation", id)
	return LaunchProfileEval{}
}

// evaluate runs the engine over the seeded post-0036 launch-profile ladder.
func evaluate(in EvalInput) LaunchEvaluation { return EvaluateLaunchProfiles(testCatalog(), in) }

// reasoned is anything carrying reason codes (a launch-profile verdict, a rung
// verdict, or a single-profile verdict).
type reasoned interface{ reasons() []Reason }

func (e LaunchProfileEval) reasons() []Reason { return e.Reasons }
func (e RungEval) reasons() []Reason          { return e.Reasons }
func (e ProfileEval) reasons() []Reason       { return e.Reasons }

// hasReason reports whether a verdict carries the given reason code.
func hasReason(pe reasoned, code ReasonCode) bool {
	for _, r := range pe.reasons() {
		if r.Code == code {
			return true
		}
	}
	return false
}

// hasNote reports whether the evaluation carries the given global note code.
func hasNote(ev LaunchEvaluation, code ReasonCode) bool {
	for _, r := range ev.Notes {
		if r.Code == code {
			return true
		}
	}
	return false
}

// TestEvaluateProfileMatchesLaunchEvaluation asserts the single-rung evaluator
// (used per rung by the launch-profile engine) returns the same verdict as the
// full evaluation for a given rung and input.
func TestEvaluateProfileMatchesLaunchEvaluation(t *testing.T) {
	in := EvalInput{Probe: &Probe{BandwidthKbps: 20000, RTTMs: 20, MaxDecodeHeight: 1080}}
	ev := evaluate(in)
	for _, lpe := range ev.Profiles {
		for _, want := range lpe.Rungs {
			got := EvaluateProfile(want.Profile, in)
			if got.Eligibility != want.Eligibility {
				t.Errorf("%s: EvaluateProfile=%s, launch evaluation=%s",
					want.Profile.ID, got.Eligibility, want.Eligibility)
			}
			if len(got.Reasons) != len(want.Reasons) {
				t.Errorf("%s: reason count %d != %d", want.Profile.ID, len(got.Reasons), len(want.Reasons))
			}
		}
	}
}

// TestEvaluateProfileNoProbeNeverIneligible asserts a probe-less, host-unknown
// input never hard-fails a user-facing rung — the launch eligibility gate must
// not reject a launch just because the network was not measured.
func TestEvaluateProfileNoProbeNeverIneligible(t *testing.T) {
	for _, lp := range testUserFacing() {
		for _, r := range lp.Rungs {
			pe := EvaluateProfile(r, EvalInput{})
			if pe.Eligibility == EligibilityIneligible {
				t.Errorf("%s: ineligible with no probe; want eligible/risky", r.ID)
			}
		}
	}
}

// TestEvaluateProfileIneligibleOnLowBandwidth asserts a measured bandwidth below
// a profile's minimum hard-fails that profile (drives the AS10-03 rejection).
func TestEvaluateProfileIneligibleOnLowBandwidth(t *testing.T) {
	p, ok := testRungByID("4k120")
	if !ok {
		t.Fatal("4k120 missing from the test catalog")
	}
	in := EvalInput{Probe: &Probe{BandwidthKbps: p.MinOfferBandwidthKbps - 1}}
	pe := EvaluateProfile(p, in)
	if pe.Eligibility != EligibilityIneligible {
		t.Fatalf("4k120 with bw<min: got %s, want ineligible", pe.Eligibility)
	}
	if !hasReason(pe, ReasonBandwidthTooLow) {
		t.Error("expected bandwidth_too_low reason")
	}
}

// TestEvaluateHistoricalFailureIsIneligible asserts AS10-11 wiring: a profile
// listed in HistoricalFailures hard-fails with ReasonHistoricalClientPerfFailed,
// even with an otherwise-fine probe (the client previously failed to decode/present
// it — e.g. constrained-baseline 4k120, bug #228).
func TestEvaluateHistoricalFailureIsIneligible(t *testing.T) {
	p, ok := testRungByID("4k120")
	if !ok {
		t.Fatal("4k120 missing from the test catalog")
	}
	// A generous probe that would otherwise make 4k120 eligible.
	in := EvalInput{
		Probe:              &Probe{BandwidthKbps: 60000, RTTMs: 10, MaxDecodeHeight: 2160},
		HistoricalFailures: map[string]bool{"4k120-h264": true},
	}
	pe := EvaluateProfile(p, in)
	if pe.Eligibility != EligibilityIneligible {
		t.Fatalf("4k120 with historical failure: got %s, want ineligible", pe.Eligibility)
	}
	if !hasReason(pe, ReasonHistoricalClientPerfFailed) {
		t.Error("expected historical_client_performance_failed reason")
	}

	// A profile NOT in the failure set is unaffected.
	other := EvaluateProfile(p, EvalInput{
		Probe:              &Probe{BandwidthKbps: 60000, RTTMs: 10, MaxDecodeHeight: 2160},
		HistoricalFailures: map[string]bool{"1080p60-h264": true},
	})
	if other.Eligibility == EligibilityIneligible && hasReason(other, ReasonHistoricalClientPerfFailed) {
		t.Error("4k120 marked historical-failed when only 1080p60 was in the set")
	}
}

// TestEvaluateCoversAllUserFacing asserts every user-facing profile gets exactly
// one verdict and no debug profile (720p30) leaks in.
func TestEvaluateCoversAllUserFacing(t *testing.T) {
	ev := evaluate(EvalInput{})
	if len(ev.Profiles) != len(testUserFacing()) {
		t.Fatalf("evaluated %d profiles, want %d", len(ev.Profiles), len(testUserFacing()))
	}
	for _, pe := range ev.Profiles {
		if pe.LaunchProfile.Visibility != VisibilityUser {
			t.Errorf("evaluation includes non-user-facing %q (%s)", pe.LaunchProfile.ID, pe.LaunchProfile.Visibility)
		}
		if pe.LaunchProfile.ID == "720p30" {
			t.Errorf("debug profile 720p30 leaked into evaluation")
		}
	}
}

// TestNoProbeConservativeDefault: with no probe at all, the recommendation is the
// conservative default (1080p60) with low confidence, and a probe_missing note.
func TestNoProbeConservativeDefault(t *testing.T) {
	ev := evaluate(EvalInput{}) // Probe nil, ProbeStale false

	if ev.RecommendedID != "1080p60" {
		t.Errorf("RecommendedID = %q, want 1080p60 (conservative default)", ev.RecommendedID)
	}
	if ev.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %q, want low (no probe)", ev.Confidence)
	}
	if !hasNote(ev, ReasonProbeMissing) {
		t.Errorf("expected probe_missing note, got %+v", ev.Notes)
	}
	// Without a probe no profile is hard-ineligible on network grounds.
	for _, pe := range ev.Profiles {
		if hasReason(pe, ReasonBandwidthTooLow) && pe.Eligibility == EligibilityIneligible {
			t.Errorf("profile %q hard-failed on bandwidth without a probe", pe.LaunchProfile.ID)
		}
	}
}

// TestStaleProbeNote: a stale probe (Probe nil, ProbeStale true) yields the
// probe_stale note and low-confidence conservative default.
func TestStaleProbeNote(t *testing.T) {
	ev := evaluate(EvalInput{ProbeStale: true})
	if !hasNote(ev, ReasonProbeStale) {
		t.Errorf("expected probe_stale note, got %+v", ev.Notes)
	}
	if hasNote(ev, ReasonProbeMissing) {
		t.Errorf("did not expect probe_missing note alongside probe_stale")
	}
	if ev.RecommendedID != "1080p60" || ev.Confidence != ConfidenceLow {
		t.Errorf("stale probe: got (%q,%s), want (1080p60,low)", ev.RecommendedID, ev.Confidence)
	}
}

// TestPoorNetworkAllIneligible: a very poor network falls below every user-facing
// profile's minimum, so all are ineligible (each with bandwidth_too_low), the
// recommendation is the lowest-demand profile (720p60) as best-effort, low confidence.
func TestPoorNetworkAllIneligible(t *testing.T) {
	ev := evaluate(EvalInput{Probe: &Probe{BandwidthKbps: 3000, RTTMs: 80, MaxDecodeHeight: 1080}})

	for _, pe := range ev.Profiles {
		if pe.Eligibility != EligibilityIneligible {
			t.Errorf("profile %q = %s, want ineligible on a 3000 kbps link", pe.LaunchProfile.ID, pe.Eligibility)
		}
		if !hasReason(pe, ReasonBandwidthTooLow) {
			t.Errorf("profile %q missing bandwidth_too_low", pe.LaunchProfile.ID)
		}
	}
	if ev.RecommendedID != "720p60" {
		t.Errorf("RecommendedID = %q, want 720p60 (best-effort floor)", ev.RecommendedID)
	}
	if ev.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %q, want low (nothing fully eligible)", ev.Confidence)
	}
}

// TestRepresentativeProbesRecommendation walks the capability ladder. For each
// representative probe it asserts (a) the named profile is launchable at that
// capability (not hard-ineligible), and (b) the *recommendation* is the highest
// fully-eligible profile — which, on the browser client, conservatively caps at
// 1440p60 because 4K is browser-risky and every 120 fps profile needs an
// unconfirmable high-refresh display. (AS10-12 native client lifts those.)
func TestRepresentativeProbesRecommendation(t *testing.T) {
	cases := []struct {
		name       string
		probe      Probe
		launchable string // this profile must not be hard-ineligible at this capability
		wantRecom  string // the conservative recommendation (highest fully eligible)
	}{
		{
			// Just enough for 720p60 (recommended 12000), below 1080p60's min (14400).
			name:       "720p-capable",
			probe:      Probe{BandwidthKbps: 12000, RTTMs: 40, MaxDecodeHeight: 720},
			launchable: "720p60", wantRecom: "720p60",
		},
		{
			name:       "1080p60-capable",
			probe:      Probe{BandwidthKbps: 18000, RTTMs: 35, MaxDecodeHeight: 1080},
			launchable: "1080p60", wantRecom: "1080p60",
		},
		{
			// 1080p120 is launchable (risky: high-refresh-unknown); rec stays 1080p60.
			name:       "1080p120-capable",
			probe:      Probe{BandwidthKbps: 30000, RTTMs: 30, MaxDecodeHeight: 1080},
			launchable: "1080p120", wantRecom: "1080p60",
		},
		{
			name:       "1440p60-capable",
			probe:      Probe{BandwidthKbps: 30000, RTTMs: 35, MaxDecodeHeight: 1440},
			launchable: "1440p60", wantRecom: "1440p60",
		},
		{
			// 1440p120 launchable (risky: high-refresh); rec caps at 1440p60.
			name:       "1440p120-capable",
			probe:      Probe{BandwidthKbps: 52500, RTTMs: 20, MaxDecodeHeight: 1440},
			launchable: "1440p120", wantRecom: "1440p60",
		},
		{
			// 4k60 launchable (risky: browser); rec caps at 1440p60.
			name:       "4k60-capable",
			probe:      Probe{BandwidthKbps: 60000, RTTMs: 35, MaxDecodeHeight: 2160},
			launchable: "4k60", wantRecom: "1440p60",
		},
		{
			// 4k120 launchable (risky: browser + high-refresh); rec caps at 1440p60.
			name:       "4k120-capable",
			probe:      Probe{BandwidthKbps: 120000, RTTMs: 15, MaxDecodeHeight: 2160},
			launchable: "4k120", wantRecom: "1440p60",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := evaluate(EvalInput{Probe: &tc.probe})
			if pe := evalByID(t, ev, tc.launchable); pe.Eligibility == EligibilityIneligible {
				t.Errorf("%s should be launchable at this capability, got ineligible (%+v)", tc.launchable, pe.Reasons)
			}
			if ev.RecommendedID != tc.wantRecom {
				t.Errorf("RecommendedID = %q, want %q", ev.RecommendedID, tc.wantRecom)
			}
			if ev.Confidence != ConfidenceHigh {
				t.Errorf("Confidence = %q, want high (fresh probe)", ev.Confidence)
			}
			// The recommendation is always a fully-eligible profile.
			if rec := evalByID(t, ev, ev.RecommendedID); rec.Eligibility != EligibilityEligible {
				t.Errorf("recommended %q is %s, want eligible", ev.RecommendedID, rec.Eligibility)
			}
		})
	}
}

// TestRTTGateBlocksHighRefresh: a high-refresh profile (120 fps, MaxStartupRTTMs=40)
// is ineligible when RTT exceeds the gate, even with ample bandwidth/decode.
func TestRTTGateBlocksHighRefresh(t *testing.T) {
	ev := evaluate(EvalInput{Probe: &Probe{BandwidthKbps: 200000, RTTMs: 90, MaxDecodeHeight: 2160}})

	hi := evalByID(t, ev, "4k120")
	if hi.Eligibility != EligibilityIneligible {
		t.Errorf("4k120 = %s on 90 ms RTT, want ineligible", hi.Eligibility)
	}
	if !hasReason(hi, ReasonRTTTooHigh) {
		t.Errorf("4k120 missing rtt_too_high, reasons=%+v", hi.Reasons)
	}
	// 4k60 has no RTT gate → not blocked by the 90 ms RTT (it is risky on the
	// browser, but crucially NOT ineligible, and carries no rtt_too_high reason).
	lo := evalByID(t, ev, "4k60")
	if lo.Eligibility == EligibilityIneligible {
		t.Errorf("4k60 = ineligible at 90 ms RTT, want launchable (no RTT gate): %+v", lo.Reasons)
	}
	if hasReason(lo, ReasonRTTTooHigh) {
		t.Errorf("4k60 has no RTT gate but carries rtt_too_high")
	}
}

// TestDecodeHeightGate: a 1080-capped decoder blocks 1440p+ on decode_height_too_low.
func TestDecodeHeightGate(t *testing.T) {
	ev := evaluate(EvalInput{Probe: &Probe{BandwidthKbps: 200000, RTTMs: 10, MaxDecodeHeight: 1080}})
	for _, id := range []string{"1440p60", "1440p120", "4k60", "4k120"} {
		pe := evalByID(t, ev, id)
		if pe.Eligibility != EligibilityIneligible {
			t.Errorf("%s = %s with 1080 decoder, want ineligible", id, pe.Eligibility)
		}
		if !hasReason(pe, ReasonDecodeHeightTooLow) {
			t.Errorf("%s missing decode_height_too_low", id)
		}
	}
	// 1080p60 fits a 1080 decoder.
	if pe := evalByID(t, ev, "1080p60"); pe.Eligibility == EligibilityIneligible {
		t.Errorf("1080p60 should fit a 1080 decoder, got %s (%+v)", pe.Eligibility, pe.Reasons)
	}
}

// TestCodecGate: a client that does not accept H.264 is ineligible everywhere
// with codec_not_supported (H.264 is the only launchable codec today).
func TestCodecGate(t *testing.T) {
	ev := evaluate(EvalInput{Probe: &Probe{
		BandwidthKbps: 200000, RTTMs: 5, MaxDecodeHeight: 2160,
		Codecs: map[Codec]bool{CodecH264: false, CodecAV1: true},
	}})
	for _, pe := range ev.Profiles {
		if !hasReason(pe, ReasonCodecNotSupported) {
			t.Errorf("%s missing codec_not_supported", pe.LaunchProfile.ID)
		}
		if pe.Eligibility != EligibilityIneligible {
			t.Errorf("%s = %s without H.264, want ineligible", pe.LaunchProfile.ID, pe.Eligibility)
		}
	}
}

// TestHostEncoderGate: when the host has no hardware encoder, profiles that
// require one are ineligible; profiles that don't are unaffected.
func TestHostEncoderGate(t *testing.T) {
	in := EvalInput{
		Probe:    &Probe{BandwidthKbps: 200000, RTTMs: 5, MaxDecodeHeight: 2160},
		HostCaps: HostCaps{Known: true, HardwareEncoder: false},
	}
	ev := evaluate(in)
	for _, pe := range ev.Profiles {
		if pe.Rungs[0].Profile.HardwareEncoderRequired {
			if pe.Eligibility != EligibilityIneligible || !hasReason(pe, ReasonHostEncoderNotSupported) {
				t.Errorf("%s requires hw encoder but is %s (%+v)", pe.LaunchProfile.ID, pe.Eligibility, pe.Reasons)
			}
		} else if hasReason(pe, ReasonHostEncoderNotSupported) {
			t.Errorf("%s does not require hw encoder but flagged host_encoder_not_supported", pe.LaunchProfile.ID)
		}
	}
}

// TestHostEncoderUnknownDoesNotBlock: when host caps are unknown, the hw-encoder
// requirement is not enforced (unknown → allow).
func TestHostEncoderUnknownDoesNotBlock(t *testing.T) {
	ev := evaluate(EvalInput{
		Probe: &Probe{BandwidthKbps: 200000, RTTMs: 5, MaxDecodeHeight: 2160},
		// HostCaps zero value: Known=false.
	})
	for _, pe := range ev.Profiles {
		if hasReason(pe, ReasonHostEncoderNotSupported) {
			t.Errorf("%s flagged host_encoder_not_supported with unknown host caps", pe.LaunchProfile.ID)
		}
	}
}

// TestBandwidthHeadroomRisky: bandwidth above the minimum but below the
// recommended headroom marks the profile risky (not ineligible) with bandwidth_too_low.
func TestBandwidthHeadroomRisky(t *testing.T) {
	// 1080p60: min 14400, recommended 18000. 15000 sits in the risky band.
	ev := evaluate(EvalInput{Probe: &Probe{BandwidthKbps: 15000, RTTMs: 30, MaxDecodeHeight: 1080}})
	pe := evalByID(t, ev, "1080p60")
	if pe.Eligibility != EligibilityRisky {
		t.Errorf("1080p60 = %s at thin headroom, want risky", pe.Eligibility)
	}
	if !hasReason(pe, ReasonBandwidthTooLow) {
		t.Errorf("1080p60 missing bandwidth_too_low (headroom), reasons=%+v", pe.Reasons)
	}
	// A risky-only profile must not be the recommendation; 720p60 (fully eligible
	// at 15000) should be recommended instead.
	if ev.RecommendedID == "1080p60" {
		t.Errorf("recommended a risky profile (1080p60); want a fully-eligible one")
	}
}

// TestBrowserRiskyProfilesAreRisky: profiles marked BrowserRisky are at best
// risky even on a perfect link, via browser_playout_unsupported.
func TestBrowserRiskyProfilesAreRisky(t *testing.T) {
	ev := evaluate(EvalInput{Probe: &Probe{BandwidthKbps: 500000, RTTMs: 3, MaxDecodeHeight: 2160}})
	for _, pe := range ev.Profiles {
		if pe.Rungs[0].Profile.BrowserClient == BrowserRisky {
			if pe.Eligibility == EligibilityEligible {
				t.Errorf("%s is BrowserRisky but evaluated eligible", pe.LaunchProfile.ID)
			}
			if !hasReason(pe, ReasonBrowserPlayoutUnsupported) {
				t.Errorf("%s missing browser_playout_unsupported", pe.LaunchProfile.ID)
			}
		}
	}
}

// TestHighRefreshUnknownRisky: 120 fps profiles (high-refresh required) carry the
// display_refresh_unknown soft reason because the probe can't confirm the display.
func TestHighRefreshUnknownRisky(t *testing.T) {
	ev := evaluate(EvalInput{Probe: &Probe{BandwidthKbps: 500000, RTTMs: 3, MaxDecodeHeight: 2160}})
	for _, pe := range ev.Profiles {
		if pe.Rungs[0].Profile.HighRefreshDisplay == DisplayRequired && !hasReason(pe, ReasonDisplayRefreshUnknown) {
			t.Errorf("%s requires high-refresh but missing display_refresh_unknown", pe.LaunchProfile.ID)
		}
	}
}

func TestHighRefreshUsesMeasuredDisplay(t *testing.T) {
	base := Probe{BandwidthKbps: 500000, RTTMs: 3, MaxDecodeHeight: 2160}

	low := base
	low.DisplayRefreshHz = 60
	pe := evalByID(t, evaluate(EvalInput{Probe: &low}), "1080p120")
	if !hasReason(pe, ReasonDisplayRefreshTooLow) || hasReason(pe, ReasonDisplayRefreshUnknown) {
		t.Fatalf("60 Hz verdict reasons = %+v, want measured-too-low only", pe.Reasons)
	}

	fast := base
	fast.DisplayRefreshHz = 119.88
	pe = evalByID(t, evaluate(EvalInput{Probe: &fast}), "1080p120")
	if hasReason(pe, ReasonDisplayRefreshTooLow) || hasReason(pe, ReasonDisplayRefreshUnknown) {
		t.Fatalf("119.88 Hz should satisfy nominal 120 fps tolerance, reasons=%+v", pe.Reasons)
	}
}

// TestHistoricalFailureBlocks: a profile in HistoricalFailures is hard-ineligible.
func TestHistoricalFailureBlocks(t *testing.T) {
	ev := evaluate(EvalInput{
		Probe:              &Probe{BandwidthKbps: 500000, RTTMs: 3, MaxDecodeHeight: 2160},
		HistoricalFailures: map[string]bool{"1080p60": true},
	})
	pe := evalByID(t, ev, "1080p60")
	if pe.Eligibility != EligibilityIneligible {
		t.Errorf("1080p60 = %s with a historical failure, want ineligible", pe.Eligibility)
	}
	if !hasReason(pe, ReasonHistoricalClientPerfFailed) {
		t.Errorf("1080p60 missing historical_client_performance_failed")
	}
}
