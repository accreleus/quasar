package profile

// Eligibility evaluation (AS10-02): given a device capability probe (and,
// where available, host capability), classify every user-facing profile as
// eligible / risky / ineligible, attach stable reason codes explaining each
// decision, and pick a recommended profile.
//
// This is pure logic — no DB, no HTTP — so it is exhaustively unit-testable.
// The control-plane handler (GET /v1/me/profiles) is a thin adapter that loads
// the probe, calls Evaluate, and marshals the result.
//
// Until AS10-03 migrates the launch path, this evaluation is advisory only: it
// does not change what a launch actually does (that still flows through the
// tier ladder). It exists to drive the picker UX (AS10-09) and the native /
// performance gates (AS10-08/11).

// conservativeDefaultID is the profile recommended when no usable probe exists.
// It mirrors the legacy tier ladder default (1080p60) so a probe-less client
// gets exactly today's behaviour, with confidence marked low.
const conservativeDefaultID = "1080p60"

// ReasonCode is a stable, machine-readable explanation for an eligibility
// decision. The set is part of the API contract (control-api.md) — values are
// append-only; never repurpose an existing code.
type ReasonCode string

const (
	ReasonBandwidthTooLow            ReasonCode = "bandwidth_too_low"
	ReasonRTTTooHigh                 ReasonCode = "rtt_too_high"
	ReasonDecodeHeightTooLow         ReasonCode = "decode_height_too_low"
	ReasonCodecNotSupported          ReasonCode = "codec_not_supported"
	ReasonHostEncoderNotSupported    ReasonCode = "host_encoder_not_supported"
	ReasonDisplayRefreshUnknown      ReasonCode = "display_refresh_unknown"
	ReasonDisplayRefreshTooLow       ReasonCode = "display_refresh_too_low"
	ReasonBrowserPlayoutUnsupported  ReasonCode = "browser_playout_unsupported"
	ReasonHistoricalClientPerfFailed ReasonCode = "historical_client_performance_failed"
	ReasonProbeMissing               ReasonCode = "probe_missing"
	ReasonProbeStale                 ReasonCode = "probe_stale"
)

// Eligibility is the per-profile verdict.
type Eligibility string

const (
	// EligibilityEligible: passes every hard check and has bandwidth headroom —
	// safe to recommend.
	EligibilityEligible Eligibility = "eligible"
	// EligibilityRisky: passes the hard checks but carries a soft concern
	// (thin bandwidth headroom, browser-client risk, or an unconfirmable
	// high-refresh-display requirement). Launchable, but not recommended.
	EligibilityRisky Eligibility = "risky"
	// EligibilityIneligible: fails a hard requirement (bandwidth/RTT/decode/
	// codec/host-encoder/historical-failure). Must not be offered.
	EligibilityIneligible Eligibility = "ineligible"
)

// Confidence rates how much to trust the recommendation. A missing or stale
// probe yields low confidence (the network was not actually measured).
type Confidence string

const (
	ConfidenceLow  Confidence = "low"
	ConfidenceHigh Confidence = "high"
)

// Probe is the measured client capability used for eligibility. A zero field
// means "not measured" and never disqualifies a profile (unknown → allow).
type Probe struct {
	BandwidthKbps    int32   // measured available downstream bandwidth
	RTTMs            int32   // measured round-trip time (ms)
	MaxDecodeHeight  int32   // max decode resolution height the client supports
	DisplayRefreshHz float64 // measured presentation display refresh; zero means unknown
	// Codecs is the set of codecs the client accepts (decode capability). A nil
	// map means codec acceptance was not probed (unknown → allow). A non-nil map
	// with a codec absent/false means the client cannot decode it.
	Codecs map[Codec]bool
}

// HostCaps carries optional host-side capability. When Known is false the host
// is not considered (unknown → allow), so eligibility never hard-fails purely
// because host info was unavailable.
type HostCaps struct {
	Known           bool // whether host capability info is available at all
	HardwareEncoder bool // host has a hardware video encoder
}

// EvalInput bundles every input to Evaluate.
type EvalInput struct {
	// Probe is the latest fresh device probe, or nil when absent/stale.
	Probe *Probe
	// ProbeStale distinguishes "a probe existed but was too old" (true) from
	// "no probe row at all" (false) when Probe is nil. Drives the global note.
	ProbeStale bool
	// HostCaps is optional host capability (see HostCaps.Known).
	HostCaps HostCaps
	// HistoricalFailures marks profile IDs that previously failed client
	// performance certification for this user/device (AS10-11 feeds this; empty
	// in AS10-02). A listed profile is hard-ineligible.
	HistoricalFailures map[string]bool
}

// Reason is a reason code plus a human-readable message.
type Reason struct {
	Code    ReasonCode `json:"code"`
	Message string     `json:"message"`
}

// ProfileEval is the verdict for one profile.
type ProfileEval struct {
	Profile     Profile     `json:"-"`
	Eligibility Eligibility `json:"eligibility"`
	Reasons     []Reason    `json:"reasons"`
}

// EvaluateProfile classifies a single stream profile (rung) against the input.
// Same hard/soft rules the whole engine uses; an empty input (no probe, unknown
// host) never hard-fails (unknown → allow), so a probe-less launch is never
// rejected for eligibility.
//
// UI-P4: the catalog-level entry points (Evaluate / EvaluateCatalog) are gone.
// A user picks a LAUNCH profile, so the top-level evaluation is
// EvaluateLaunchProfiles (launch.go), which calls this per rung.
func EvaluateProfile(p Profile, in EvalInput) ProfileEval {
	return evaluateProfile(p, in)
}

// evaluateProfile applies the hard and soft checks to a single profile.
func evaluateProfile(p Profile, in EvalInput) ProfileEval {
	var reasons []Reason
	hardFail := false
	softFail := false

	hard := func(code ReasonCode, msg string) {
		reasons = append(reasons, Reason{code, msg})
		hardFail = true
	}
	soft := func(code ReasonCode, msg string) {
		reasons = append(reasons, Reason{code, msg})
		softFail = true
	}

	// --- Hard checks (only enforced when the input was actually measured) ---
	if pr := in.Probe; pr != nil {
		if pr.BandwidthKbps > 0 && pr.BandwidthKbps < p.MinOfferBandwidthKbps {
			hard(ReasonBandwidthTooLow, "measured bandwidth is below the profile minimum")
		}
		if pr.RTTMs > 0 && p.MaxStartupRTTMs > 0 && pr.RTTMs > p.MaxStartupRTTMs {
			hard(ReasonRTTTooHigh, "measured RTT exceeds the profile maximum")
		}
		if pr.MaxDecodeHeight > 0 && pr.MaxDecodeHeight < p.MinDecodeHeight {
			hard(ReasonDecodeHeightTooLow, "client decode height is below the profile resolution")
		}
		// UI-P4: a rung IS one codec, so this is a direct membership test rather
		// than "the first launchable entry in a preference list". A nil Codecs map
		// means codec acceptance was never probed (unknown → allow).
		if pr.Codecs != nil && p.Codec != "" && !pr.Codecs[p.Codec] {
			hard(ReasonCodecNotSupported, "client does not accept this rung's codec")
		}
	}
	if in.HostCaps.Known && p.HardwareEncoderRequired && !in.HostCaps.HardwareEncoder {
		hard(ReasonHostEncoderNotSupported, "no host with a hardware encoder is available for this profile")
	}
	if in.HistoricalFailures[p.ID] {
		hard(ReasonHistoricalClientPerfFailed, "this client previously failed performance certification at this profile")
	}

	// --- Soft checks (downgrade to risky, never block) ---
	// Thin bandwidth headroom: above the minimum but below the recommended
	// congestion-control headroom. Only meaningful when not already hard-failed.
	if pr := in.Probe; pr != nil && !hardFail {
		if pr.BandwidthKbps > 0 &&
			pr.BandwidthKbps >= p.MinOfferBandwidthKbps &&
			pr.BandwidthKbps < p.RecommendedOfferBandwidthKbps {
			soft(ReasonBandwidthTooLow, "bandwidth is below the recommended headroom; quality may be unstable")
		}
	}
	// Browser-client risk: the profile is marked risky for the WebRTC client.
	if p.BrowserClient == BrowserRisky {
		soft(ReasonBrowserPlayoutUnsupported, "browser playback of this profile is risky; a native client is recommended")
	}
	// High-refresh profiles remain launchable, but are not recommended when the
	// measured display cannot present their cadence. A 2% tolerance covers common
	// fractional timings such as 119.88 Hz for a nominal 120 fps profile.
	if p.HighRefreshDisplay == DisplayRequired {
		if in.Probe == nil || in.Probe.DisplayRefreshHz <= 0 {
			soft(ReasonDisplayRefreshUnknown, "profile needs a high-refresh display; the client display rate is unknown")
		} else if in.Probe.DisplayRefreshHz < float64(p.FPS)*0.98 {
			soft(ReasonDisplayRefreshTooLow, "measured display refresh is below the profile frame rate")
		}
	}

	elig := EligibilityEligible
	switch {
	case hardFail:
		elig = EligibilityIneligible
	case softFail:
		elig = EligibilityRisky
	}
	return ProfileEval{Profile: p, Eligibility: elig, Reasons: reasons}
}
