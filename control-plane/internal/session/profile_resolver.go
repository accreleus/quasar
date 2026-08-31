// profile_resolver.go — launch-profile resolution and the eligibility gate.
package session

import (
	"context"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// resolveLaunchProfile looks up the selected launch profile with its rungs and
// applies the eligibility gate:
//   - unknown id → ErrProfileUnknown.
//   - admin, or an explicit stream override → all gating bypassed.
//   - non-user-facing (debug/internal) → ErrProfileIneligible.
//   - hard eligibility failure → ErrProfileIneligible. A probe-less caller never
//     hard-fails (unknown allows), so an unmeasured network never blocks a launch.
//
// Hard failure means EVERY rung is ineligible, or a chain-level presentation
// failure is on record. A chain whose top rung is ineligible but whose floor is
// fine is `risky` and allowed — the eligible-if-any inversion, so a 4K chain does
// not vanish for a client that cannot decode 4K.
func (c *Coordinator) resolveLaunchProfile(ctx context.Context, userID string, lp LaunchParams) (profile.LaunchProfile, error) {
	p, err := c.store.GetLaunchProfile(ctx, lp.ProfileID)
	if err != nil {
		return profile.LaunchProfile{}, ErrProfileUnknown
	}
	if lp.IsAdmin || lp.Override.any() {
		return p, nil // operator override — skip the eligibility gate
	}
	if p.Visibility != profile.VisibilityUser {
		return profile.LaunchProfile{}, ErrProfileIneligible
	}
	pe := profile.EvaluateLaunchProfile(p, c.probeEvalInput(ctx, userID))
	if pe.Eligibility == profile.EligibilityIneligible {
		c.log.Info("AS10-03: launch profile rejected as ineligible", "user_id", userID, "profile", p.ID)
		return profile.LaunchProfile{}, ErrProfileIneligible
	}
	return p, nil
}

// probeEvalInput builds the eligibility input from the caller's latest fresh
// probe plus their chain-level performance history. A read error or absent probe
// degrades to an empty input (unknown allows), never to a rejection.
func (c *Coordinator) probeEvalInput(ctx context.Context, userID string) profile.EvalInput {
	in := profile.EvalInput{}

	deviceKey, _ := c.store.LatestDeviceKey(ctx, userID)
	if hf, err := c.store.ProfileFailures(ctx, userID, deviceKey); err != nil {
		c.log.Warn("AS10-03: profile failure history load failed, gating without it", "user_id", userID, "err", err)
	} else {
		in.HistoricalFailures = hf
	}

	dp, err := c.store.LatestProbe(ctx, userID)
	if err != nil {
		c.log.Warn("AS10-03: probe load failed, gating profile without probe", "user_id", userID, "err", err)
		return in
	}
	if dp == nil {
		return in
	}
	in.Probe = &profile.Probe{
		BandwidthKbps:    dp.BandwidthKbps,
		RTTMs:            dp.RTTMs,
		MaxDecodeHeight:  dp.MaxDecodeHeight,
		DisplayRefreshHz: dp.DisplayRefreshHz,
	}
	return in
}
