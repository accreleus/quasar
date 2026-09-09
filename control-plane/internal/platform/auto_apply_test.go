package platform

import (
	"errors"
	"testing"
)

// errRead stands in for "a read this pass needed did not work".
var errRead = errors.New("read failed")

// PlanAutoApply is the whole #122 policy, and it is pure, so this is a table
// over the operator's four decisions rather than over the function's branches.

// autoView builds a view whose control-plane target is eligible and which offers
// `rels` newest-first.
func autoView(rels ...Release) View {
	return View{
		Channel:   ChannelStable,
		Available: rels,
		Targets: []Target{
			{Kind: TargetControlPlane, Eligible: true},
			{Kind: TargetHost, HostID: str("h1"), Eligible: true},
		},
	}
}

// offeredRelease is a listed release; `migrates` is the field the drain decision
// and therefore this decision turns on.
func offeredRelease(id string, migrates bool) Release {
	r := rel(id, "0.4.0", commitC, 79, at(3))
	r.Migrates = migrates
	return r
}

func TestPlanAutoApply(t *testing.T) {
	tests := []struct {
		name       string
		in         AutoApplyInputs
		wantApply  bool
		wantReason string
		wantDetail string
	}{
		{
			name:       "off by default — the setting decides before anything is read",
			in:         AutoApplyInputs{Enabled: false, View: autoView(offeredRelease("r1", false))},
			wantReason: AutoApplyDisabled,
		},
		{
			name:       "nothing offered",
			in:         AutoApplyInputs{Enabled: true, View: autoView()},
			wantReason: AutoApplyNoRelease,
		},
		{
			name:      "a non-migrating release is applied",
			in:        AutoApplyInputs{Enabled: true, View: autoView(offeredRelease("r1", false))},
			wantApply: true, wantReason: AutoApplyStarted,
		},
		{
			// THE decision. Since #128 a non-migrating recreate no longer ends a
			// running session, so an unattended apply costs a player nothing; a
			// migrating one drains the whole instance by design, and ending every
			// live session with nobody watching is not a scheduled activity.
			name:       "a MIGRATING release is never applied unattended",
			in:         AutoApplyInputs{Enabled: true, View: autoView(offeredRelease("r1", true))},
			wantReason: AutoApplyCarriesMigration,
		},
		{
			// The candidate is the HEAD of Available, exactly as the console's
			// Update button names it — not "the newest non-migrating one". An
			// instance must not silently skip past a release an admin can see.
			name: "a migrating head is not skipped in favour of an older release",
			in: AutoApplyInputs{Enabled: true, View: autoView(
				offeredRelease("newer", true), offeredRelease("older", false))},
			wantReason: AutoApplyCarriesMigration,
		},
		{
			name: "an unattended run that already failed on this release is not retried",
			in: AutoApplyInputs{Enabled: true, View: autoView(offeredRelease("r1", false)),
				SuppressedReleaseIDs: map[string]bool{"r1": true}},
			wantReason: AutoApplyFailedBefore,
		},
		{
			// Per release, not global: one bad release must not end automatic
			// updates for the instance.
			name: "a NEWER release is still applied after an earlier one failed",
			in: AutoApplyInputs{Enabled: true, View: autoView(offeredRelease("r2", false)),
				SuppressedReleaseIDs: map[string]bool{"r1": true}},
			wantApply: true, wantReason: AutoApplyStarted,
		},
		{
			name: "an open attempt anywhere on the instance defers it",
			in: AutoApplyInputs{Enabled: true, View: autoView(offeredRelease("r1", false)),
				AnyAttemptOpen: true},
			wantReason: AutoApplyInFlight,
		},
		{
			name: "an active fleet run defers it",
			in: func() AutoApplyInputs {
				v := autoView(offeredRelease("r1", false))
				v.ActiveApply = &ActiveApply{Run: &ApplyRun{ID: "run-1", State: RunRunning}}
				return AutoApplyInputs{Enabled: true, View: v}
			}(),
			wantReason: AutoApplyInFlight,
		},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			got := PlanAutoApply(c.in)
			if got.Apply != c.wantApply || got.Reason != c.wantReason {
				t.Fatalf("PlanAutoApply = {apply:%v reason:%q}, want {apply:%v reason:%q}",
					got.Apply, got.Reason, c.wantApply, c.wantReason)
			}
			if c.wantDetail != "" && got.Detail != c.wantDetail {
				t.Fatalf("detail = %q, want %q", got.Detail, c.wantDetail)
			}
		})
	}
}

// Nothing moves before the control plane (ADR 0002), so an ineligible control
// plane is a refusal here exactly as it is in handleFleetApply — and the reason
// travels, because "not eligible" without saying why is not a usable record.
func TestPlanAutoApplyRefusesAnIneligibleControlPlaneAndNamesWhy(t *testing.T) {
	v := autoView(offeredRelease("r1", false))
	reason := ReasonUpdaterAbsent
	v.Targets[0] = Target{Kind: TargetControlPlane, Eligible: false, Reason: &reason}

	got := PlanAutoApply(AutoApplyInputs{Enabled: true, View: v})

	if got.Apply {
		t.Fatal("must not apply while the control plane cannot take it")
	}
	if got.Reason != AutoApplyNotEligible || got.Detail != ReasonUpdaterAbsent {
		t.Fatalf("got {%q,%q}, want {%q,%q}", got.Reason, got.Detail, AutoApplyNotEligible, ReasonUpdaterAbsent)
	}
	if got.ReleaseID != "r1" {
		t.Fatalf("release id = %q — a refusal must still name the release it declined", got.ReleaseID)
	}
}

// `up_to_date` on the control plane is NOT a refusal, matching handleFleetApply:
// the run then goes straight to the hosts, which is exactly the case an
// unattended pass exists to handle (a control plane already updated by hand,
// hosts left behind).
func TestPlanAutoApplyAppliesWhenOnlyTheHostsAreBehind(t *testing.T) {
	v := autoView(offeredRelease("r1", false))
	reason := ReasonUpToDate
	v.Targets[0] = Target{Kind: TargetControlPlane, Eligible: false, Reason: &reason}

	got := PlanAutoApply(AutoApplyInputs{Enabled: true, View: v})

	if !got.Apply || got.Reason != AutoApplyStarted {
		t.Fatalf("got {apply:%v reason:%q}, want an apply: up_to_date sends the run to the hosts",
			got.Apply, got.Reason)
	}
}

// A disabled instance is the overwhelming majority, and it must not write a line
// into every detection run record telling an operator that a feature they never
// enabled did not run.
func TestAutoApplySummaryIsSilentWhenDisabled(t *testing.T) {
	if got := (AutoApplyOutcome{Decision: AutoApplyDecision{Reason: AutoApplyDisabled}}).Summary(); got != nil {
		t.Fatalf("summary = %v, want nothing for a disabled instance", got)
	}
	// ...but a FAILED read while disabled is still reported: that is a fault,
	// not a quiet default.
	out := AutoApplyOutcome{Decision: AutoApplyDecision{Reason: AutoApplyDisabled}, Err: errRead}
	if got := out.Summary(); got == nil || got["auto_apply_error"] == nil {
		t.Fatalf("summary = %v, want the read error reported", got)
	}
}

// Every refusal reaches the run record with its release and detail, because the
// summary is the only place an operator can read why nothing happened.
func TestAutoApplySummaryCarriesTheRefusal(t *testing.T) {
	out := AutoApplyOutcome{Decision: AutoApplyDecision{
		ReleaseID: "r1", Reason: AutoApplyNotEligible, Detail: ReasonUpdaterAbsent}}
	got := out.Summary()
	if got["auto_apply"] != AutoApplyNotEligible ||
		got["auto_apply_release_id"] != "r1" ||
		got["auto_apply_detail"] != ReasonUpdaterAbsent {
		t.Fatalf("summary = %v", got)
	}
}

// upToDateView is a fully-updated instance: the channel offers a release and it
// is the one already installed, so every target reads `up_to_date`. This is the
// steady state of every healthy instance, and it is what `Available[0]` looks
// like there — `belowInstalledVersion` keeps the equal version listed precisely
// so `up_to_date` can be evaluated against it.
func upToDateView(rels ...Release) View {
	up := ReasonUpToDate
	return View{
		Channel:   ChannelStable,
		Available: rels,
		Targets: []Target{
			{Kind: TargetControlPlane, Eligible: false, Reason: &up},
			{Kind: TargetHost, HostID: str("h1"), Eligible: false, Reason: &up},
		},
	}
}

// THE ONE THAT MATTERS FOR AN IDLE INSTANCE. Without this gate the scheduler
// starts a fleet run on every pass of an up-to-date instance: a `succeeded` run
// with zero attempts, weekly, for ever, with the Update button vanishing and the
// active-run panel flashing each time. The admin's button is hidden in this
// state by `hasUpdate`; this is the scheduler's equivalent.
// (The sibling guard, `offered(view, candidate)`, mirrors the handler's own
// refusal for an edge row older than what is installed. It is a one-line reuse
// of the handler's helper and is covered by that helper's own tests; building the
// edge-older-than-installed fixture here bought a brittle test rather than
// confidence.)
func TestPlanAutoApplyStartsNothingWhenEverythingIsUpToDate(t *testing.T) {
	got := PlanAutoApply(AutoApplyInputs{
		Enabled: true, View: upToDateView(offeredRelease("r1", false)),
	})

	if got.Apply {
		t.Fatal("must not start a run when no target is eligible — nothing to do is not the same as do it")
	}
	if got.Reason != AutoApplyUpToDate {
		t.Fatalf("reason = %q, want %q", got.Reason, AutoApplyUpToDate)
	}
}

// `attempt_in_flight` must report as in-flight, not as a generic ineligibility —
// exact parity with the handler, which excludes it from the durable gate and
// catches it with its own more specific check.
func TestPlanAutoApplyReportsAnInFlightAttemptAsSuch(t *testing.T) {
	v := autoView(offeredRelease("r1", false))
	reason := ReasonAttemptInFlight
	v.Targets[0] = Target{Kind: TargetControlPlane, Eligible: false, Reason: &reason}

	got := PlanAutoApply(AutoApplyInputs{Enabled: true, View: v, AnyAttemptOpen: true})

	if got.Reason != AutoApplyInFlight {
		t.Fatalf("reason = %q, want %q — attempt_in_flight is not a durable refusal",
			got.Reason, AutoApplyInFlight)
	}
}
