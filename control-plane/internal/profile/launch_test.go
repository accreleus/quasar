package profile

import "testing"

// launch_test.go — the UI-P4 launch-profile evaluation: the eligible-if-ANY
// inversion, its top-rung-ineligible counterweight, and the nominal block.
//
// NOTE ON WHAT MOVED. The catalog-shape assertions that used to live in
// profile_test.go (every profile has positive geometry, an ABR floor below
// nominal, MinDecodeHeight == Height, H.264 preferred, 720p30 not user-facing,
// …) tested a Go literal that no longer exists. They now test the SEEDED
// DATABASE ROWS instead — see TestSeededLadderShape in
// internal/session/launch_profiles_db_test.go, which is the real data those
// assertions were always about.

func rungAt(height, minDecode int32, codec Codec, id string) Profile {
	return Profile{
		ID: id, DisplayName: id, Codec: codec,
		Width: height * 16 / 9, Height: height, FPS: 60,
		H264Profile:                   "high",
		NominalBitrateKbps:            height * 10,
		MinOfferBandwidthKbps:         height * 12,
		RecommendedOfferBandwidthKbps: height * 15,
		HeadroomFactor:                1.5,
		ABRFloorKbps:                  height * 3,
		MinDecodeHeight:               minDecode,
		HighRefreshDisplay:            DisplayNone,
		BrowserClient:                 BrowserRecommended,
		Playout0Ms:                    50,
		Visibility:                    VisibilityInternal,
	}
}

// chain is "High": AV1 1440p, then H.264 1080p as the floor.
func chain() LaunchProfile {
	return LaunchProfile{
		ID: "high", DisplayName: "High", Visibility: VisibilityUser, SortOrder: 10,
		Rungs: []Profile{
			rungAt(1440, 1440, CodecAV1, "1440p60-av1"),
			rungAt(1080, 1080, CodecH264, "1080p60-h264"),
		},
	}
}

// TestEligibleIfAnyRungIs is the recorded behaviour INVERSION: a 1440p chain no
// longer vanishes for a client that cannot decode 1440p — it is offered and
// resolves to its H.264 1080p floor rung.
func TestEligibleIfAnyRungIs(t *testing.T) {
	in := EvalInput{Probe: &Probe{BandwidthKbps: 500000, RTTMs: 5, MaxDecodeHeight: 1080}}
	pe := EvaluateLaunchProfile(chain(), in)

	if pe.Rungs[0].Eligibility != EligibilityIneligible {
		t.Fatalf("top rung = %s, want ineligible (1440 > 1080 decode ceiling)", pe.Rungs[0].Eligibility)
	}
	if pe.Rungs[1].Eligibility != EligibilityEligible {
		t.Fatalf("floor rung = %s (%+v), want eligible", pe.Rungs[1].Eligibility, pe.Rungs[1].Reasons)
	}
	// Pre-UI-P4 this whole entry would have been ineligible and hidden.
	if pe.Eligibility == EligibilityIneligible {
		t.Errorf("launch profile = ineligible; the inversion requires it be offered via its floor rung")
	}
}

// TestTopRungIneligibleIsRiskyNotEligible is the counterweight to the inversion:
// without it, a "1440p" chain that always streams 1080p could become the
// recommendation.
func TestTopRungIneligibleIsRiskyNotEligible(t *testing.T) {
	in := EvalInput{Probe: &Probe{BandwidthKbps: 500000, RTTMs: 5, MaxDecodeHeight: 1080}}
	pe := EvaluateLaunchProfile(chain(), in)
	if pe.Eligibility != EligibilityRisky {
		t.Fatalf("launch profile = %s, want risky (top rung falls through)", pe.Eligibility)
	}
	if !hasReason(pe, ReasonDecodeHeightTooLow) {
		t.Errorf("expected the top rung's decode_height_too_low reason to be carried up, got %+v", pe.Reasons)
	}

	// A second, fully-eligible chain must win the recommendation. (With `high` as
	// the ONLY entry the answer would be `high` regardless — recommend()'s
	// documented best-effort fallback when nothing is fully eligible, which is
	// pre-existing behaviour and not what this test is about.)
	balanced := LaunchProfile{
		ID: "balanced", DisplayName: "Balanced", Visibility: VisibilityUser, SortOrder: 20,
		Rungs: []Profile{rungAt(1080, 1080, CodecH264, "1080p60-h264")},
	}
	ev := EvaluateLaunchProfiles([]LaunchProfile{chain(), balanced}, in)
	if ev.RecommendedID != "balanced" {
		t.Errorf("RecommendedID = %q, want balanced; recommend() must pick only fully-eligible entries", ev.RecommendedID)
	}
	if ev.Confidence != ConfidenceHigh {
		t.Errorf("Confidence = %q, want high (fresh probe, a fully-eligible entry exists)", ev.Confidence)
	}
}

// TestAllRungsIneligibleIsIneligible: when nothing in the chain is launchable,
// the chain is ineligible.
func TestAllRungsIneligibleIsIneligible(t *testing.T) {
	in := EvalInput{Probe: &Probe{BandwidthKbps: 500000, RTTMs: 5, MaxDecodeHeight: 480}}
	pe := EvaluateLaunchProfile(chain(), in)
	if pe.Eligibility != EligibilityIneligible {
		t.Fatalf("launch profile = %s, want ineligible (no rung survives a 480 decoder)", pe.Eligibility)
	}
}

// TestTopRungEligibleIsEligible: the happy path keeps the pre-UI-P4 verdict.
func TestTopRungEligibleIsEligible(t *testing.T) {
	in := EvalInput{Probe: &Probe{BandwidthKbps: 500000, RTTMs: 5, MaxDecodeHeight: 2160}}
	pe := EvaluateLaunchProfile(chain(), in)
	if pe.Eligibility != EligibilityEligible {
		t.Fatalf("launch profile = %s (%+v), want eligible", pe.Eligibility, pe.Reasons)
	}
}

// TestNominalEchoesTopRung: `nominal` is the TOP rung's numbers, advertised and
// not resolved.
func TestNominalEchoesTopRung(t *testing.T) {
	pe := EvaluateLaunchProfile(chain(), EvalInput{})
	top := chain().Rungs[0]
	want := Nominal{Width: top.Width, Height: top.Height, FPS: top.FPS, BitrateKbps: top.NominalBitrateKbps}
	if pe.Nominal != want {
		t.Errorf("nominal = %+v, want the top rung's %+v", pe.Nominal, want)
	}
}

// TestLaunchProfileLevelHistoricalFailureBlocksTheChain: a PRESENTATION-side
// failure is recorded at launch-profile grain and is codec- and
// resolution-independent, so it hard-fails the whole chain.
func TestLaunchProfileLevelHistoricalFailureBlocksTheChain(t *testing.T) {
	in := EvalInput{
		Probe:              &Probe{BandwidthKbps: 500000, RTTMs: 5, MaxDecodeHeight: 2160},
		HistoricalFailures: map[string]bool{"high": true},
	}
	pe := EvaluateLaunchProfile(chain(), in)
	if pe.Eligibility != EligibilityIneligible {
		t.Fatalf("launch profile = %s, want ineligible on a launch-profile-level failure", pe.Eligibility)
	}
	if !hasReason(pe, ReasonHistoricalClientPerfFailed) {
		t.Errorf("missing historical_client_performance_failed, got %+v", pe.Reasons)
	}
}

// TestNonUserFacingChainsAreNeverReturned: debug/internal launch profiles are
// never offered, exactly as before.
func TestNonUserFacingChainsAreNeverReturned(t *testing.T) {
	debug := chain()
	debug.ID, debug.Visibility = "debug-chain", VisibilityDebug
	ev := EvaluateLaunchProfiles([]LaunchProfile{chain(), debug}, EvalInput{})
	for _, pe := range ev.Profiles {
		if pe.LaunchProfile.ID == "debug-chain" {
			t.Error("a debug launch profile leaked into the evaluation")
		}
	}
}

// TestEmptyChainIsIneligible: a rung-less chain cannot dispatch anything, so it
// is never offered. Write-time validation and the 0036 fan-out both make this
// unreachable; the engine is defensive rather than optimistic.
func TestEmptyChainIsIneligible(t *testing.T) {
	empty := LaunchProfile{ID: "empty", DisplayName: "Empty", Visibility: VisibilityUser}
	pe := EvaluateLaunchProfile(empty, EvalInput{})
	if pe.Eligibility != EligibilityIneligible {
		t.Errorf("empty chain = %s, want ineligible", pe.Eligibility)
	}
}

// TestRungCodecGate: a rung whose codec the client cannot decode is ineligible,
// and the chain falls through to the next one.
func TestRungCodecGate(t *testing.T) {
	in := EvalInput{Probe: &Probe{
		BandwidthKbps: 500000, RTTMs: 5, MaxDecodeHeight: 2160,
		Codecs: map[Codec]bool{CodecH264: true, CodecAV1: false},
	}}
	pe := EvaluateLaunchProfile(chain(), in)
	if pe.Rungs[0].Eligibility != EligibilityIneligible || !hasReason(pe.Rungs[0], ReasonCodecNotSupported) {
		t.Fatalf("av1 rung = %s (%+v), want ineligible/codec_not_supported", pe.Rungs[0].Eligibility, pe.Rungs[0].Reasons)
	}
	if pe.Rungs[1].Eligibility != EligibilityEligible {
		t.Fatalf("h264 floor rung = %s, want eligible", pe.Rungs[1].Eligibility)
	}
	if pe.Eligibility != EligibilityRisky {
		t.Errorf("chain = %s, want risky (falls through to the floor)", pe.Eligibility)
	}
}
