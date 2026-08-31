package profile

// launch.go — the LAUNCH PROFILE (UI-P4): an ordered chain of stream-profile
// rungs, best first, and the eligibility evaluation over it.
//
// ELIGIBILITY SEMANTICS INVERT HERE, AND THAT IS A DELIBERATE, RECORDED
// BEHAVIOUR CHANGE (re-spec §2.8, control-api.md UI-P4 amendment).
//
// Before UI-P4: a 4K profile was `ineligible` for a client that could not decode
// 4K, and vanished from the picker. After UI-P4: a launch profile is eligible if
// ANY of its rungs is, so that 4K launch profile is offered and resolves down to
// its H.264 1080p floor rung. The launch profile is a preference CHAIN, not a
// promise, and refusing to start is worse than starting lower.
//
// The counterweight, without which the inversion would silently recommend a "4K"
// profile that always streams 1080p: a launch profile whose TOP rung is
// ineligible while a lower one is not is classified `risky`, NOT `eligible`.
// recommend() only ever picks a fully eligible entry, so a launch profile that
// is *going* to fall through can never become the recommendation.

// LaunchProfile is an ordered chain of stream-profile rungs, best first. Rungs
// is in `position` order (position 1 = tried first) and is never empty for a
// well-formed row: write-time validation requires at least one h264 rung, and
// migration 0036's fan-out guarantees every migrated launch profile has one.
type LaunchProfile struct {
	ID          string
	DisplayName string
	Description string
	Visibility  Visibility
	SortOrder   int32
	// Rungs are the stream profiles this launch profile walks, best first.
	Rungs []Profile
}

// TopRung returns the highest-preference rung and ok=false for an empty chain.
func (lp LaunchProfile) TopRung() (Profile, bool) {
	if len(lp.Rungs) == 0 {
		return Profile{}, false
	}
	return lp.Rungs[0], true
}

// Nominal is the TOP rung's numbers, echoed so a picker and the admin
// app-editor preview have something to render. ADVERTISED, NOT RESOLVED: if a
// rung falls through the session streams something else, and the session's own
// `stream` block is the truth. Clients must not treat it as a promise.
type Nominal struct {
	Width       int32 `json:"width"`
	Height      int32 `json:"height"`
	FPS         int32 `json:"fps"`
	BitrateKbps int32 `json:"bitrate_kbps"`
}

// RungEval is one rung's verdict inside a LaunchProfileEval.
type RungEval struct {
	Profile     Profile     `json:"-"`
	Position    int32       `json:"position"`
	Eligibility Eligibility `json:"eligibility"`
	Reasons     []Reason    `json:"reasons"`
}

// LaunchProfileEval is a launch profile's verdict plus its per-rung verdicts.
type LaunchProfileEval struct {
	LaunchProfile LaunchProfile `json:"-"`
	Nominal       Nominal       `json:"nominal"`
	Eligibility   Eligibility   `json:"eligibility"`
	Reasons       []Reason      `json:"reasons"`
	Rungs         []RungEval    `json:"rungs"`
}

// LaunchEvaluation is the full result over the supplied launch profiles.
type LaunchEvaluation struct {
	// Profiles holds one verdict per user-facing launch profile, in the order
	// supplied (the store returns sort_order ASC, highest quality first).
	Profiles []LaunchProfileEval
	// RecommendedID is the recommended LAUNCH PROFILE id.
	RecommendedID string
	// Confidence rates trust in the recommendation.
	Confidence Confidence
	// Notes are global reasons (probe_missing / probe_stale) not tied to one entry.
	Notes []Reason
}

// EvaluateLaunchProfiles classifies every user-facing launch profile in lps
// against the input and selects a recommendation. It never returns an error:
// missing/partial inputs degrade to "unknown → allow" with low confidence.
//
// Non-user-facing launch profiles (debug/internal) are skipped, exactly as the
// pre-UI-P4 catalog evaluation skipped non-user profiles. A rung's OWN
// visibility is `internal` by construction and is deliberately not a filter
// here: rungs are evaluated as part of the launch profile that lists them, never
// standalone.
func EvaluateLaunchProfiles(lps []LaunchProfile, in EvalInput) LaunchEvaluation {
	var ev LaunchEvaluation

	probeMissing := in.Probe == nil
	if probeMissing {
		if in.ProbeStale {
			ev.Notes = append(ev.Notes, Reason{ReasonProbeStale, "device probe is stale; network not freshly measured"})
		} else {
			ev.Notes = append(ev.Notes, Reason{ReasonProbeMissing, "no device probe available; network not measured"})
		}
	}

	for _, lp := range lps {
		if lp.Visibility != VisibilityUser {
			continue
		}
		ev.Profiles = append(ev.Profiles, EvaluateLaunchProfile(lp, in))
	}

	ev.RecommendedID, ev.Confidence = recommendLaunch(ev.Profiles, probeMissing)
	return ev
}

// EvaluateLaunchProfile classifies a single launch profile — the launch path
// (AS10-03) uses it to gate a profile_id selection without iterating everything.
func EvaluateLaunchProfile(lp LaunchProfile, in EvalInput) LaunchProfileEval {
	out := LaunchProfileEval{LaunchProfile: lp, Rungs: make([]RungEval, 0, len(lp.Rungs))}

	for i, r := range lp.Rungs {
		pe := evaluateProfile(r, in)
		out.Rungs = append(out.Rungs, RungEval{
			Profile:     r,
			Position:    int32(i + 1),
			Eligibility: pe.Eligibility,
			Reasons:     pe.Reasons,
		})
	}

	if top, ok := lp.TopRung(); ok {
		out.Nominal = Nominal{Width: top.Width, Height: top.Height, FPS: top.FPS, BitrateKbps: top.NominalBitrateKbps}
	}

	// A presentation-side historical failure is recorded at LAUNCH-PROFILE grain
	// (re-spec §4.4) because it is genuinely codec- and resolution-independent, so
	// it hard-fails the whole chain rather than one rung. (A DECODE-side failure
	// is recorded at rung grain and is applied per rung by evaluateProfile above.)
	if in.HistoricalFailures[lp.ID] {
		out.Eligibility = EligibilityIneligible
		out.Reasons = []Reason{{ReasonHistoricalClientPerfFailed,
			"this client previously failed performance certification at this profile"}}
		return out
	}

	// A chain with no rungs cannot dispatch anything. Write-time validation and
	// the 0036 fan-out both make this unreachable; classify defensively rather
	// than offering something that would fail at launch.
	if len(out.Rungs) == 0 {
		out.Eligibility = EligibilityIneligible
		out.Reasons = []Reason{}
		return out
	}

	top := out.Rungs[0]
	switch top.Eligibility {
	case EligibilityEligible:
		out.Eligibility = EligibilityEligible
		out.Reasons = top.Reasons
	case EligibilityRisky:
		// The top rung is launchable but carries a soft concern: the chain does
		// too, and it must not become the recommendation.
		out.Eligibility = EligibilityRisky
		out.Reasons = top.Reasons
	default: // top rung ineligible
		if anyLaunchable(out.Rungs) {
			// Eligible-if-ANY (the inversion), downgraded to risky because the top
			// rung WILL fall through. The top rung's reasons are carried so a client
			// can say why, and the per-rung verdicts say what will actually be used.
			out.Eligibility = EligibilityRisky
		} else {
			out.Eligibility = EligibilityIneligible
		}
		out.Reasons = top.Reasons
	}
	if out.Reasons == nil {
		out.Reasons = []Reason{}
	}
	return out
}

// anyLaunchable reports whether any rung below the top is eligible or risky.
func anyLaunchable(rungs []RungEval) bool {
	for _, r := range rungs[1:] {
		if r.Eligibility != EligibilityIneligible {
			return true
		}
	}
	return false
}

// recommendLaunch picks the recommended LAUNCH PROFILE id and the confidence.
// Identical rules to the pre-UI-P4 recommend(), one grain up:
//
//   - No usable probe → the conservative default (1080p60), confidence low.
//   - Otherwise → the highest fully-ELIGIBLE launch profile, confidence high. A
//     `risky` entry is never recommended, which is what stops a chain that is
//     going to fall through from being presented as the right choice.
//   - If nothing is fully eligible → the lowest-demand entry, confidence low.
func recommendLaunch(evals []LaunchProfileEval, probeMissing bool) (string, Confidence) {
	if probeMissing {
		for _, pe := range evals {
			if pe.LaunchProfile.ID == conservativeDefaultID {
				return conservativeDefaultID, ConfidenceLow
			}
		}
		// conservativeDefaultID absent (shouldn't happen) — fall through.
	}

	if !probeMissing {
		for _, pe := range evals {
			if pe.Eligibility == EligibilityEligible {
				return pe.LaunchProfile.ID, ConfidenceHigh
			}
		}
	}

	if n := len(evals); n > 0 {
		return evals[n-1].LaunchProfile.ID, ConfidenceLow
	}
	return "", ConfidenceLow
}
