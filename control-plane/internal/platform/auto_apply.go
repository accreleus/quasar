package platform

import (
	"context"
	"log/slog"
)

// Unattended automatic apply of platform releases (#122).
//
// The whole feature is a TRIGGER on the existing fleet run, not a second
// sequencer: everything about ordering, skipping, cordons, the drain decision,
// adoption across the restart and ADR 0002 already lives in FleetRunner, is
// reviewed, and is DB-tested. A parallel path would be the same logic with
// different bugs.
//
// Four operator decisions shape it, and each one is a refusal rather than a
// knob:
//
//   - **A migrating release is never applied unattended.** Since #128 a
//     non-migrating control-plane recreate no longer ends a `running` session,
//     which is why #153 could stop draining for it — so an unattended
//     non-migrating apply costs a player nothing at the control-plane step. A
//     MIGRATING release is the opposite: it drains the whole instance by design
//     (a held row would otherwise be read back by a binary that has just
//     migrated the database under it). Ending every live session with nobody
//     watching is not something to do on a schedule.
//   - **No window of its own.** This runs from a successful
//     `platform.release_detect` pass, so the detection schedule IS the window —
//     already a cron in the Jobs tab, already editable there. One schedule
//     cannot disagree with itself.
//   - **Whole fleet**, exactly as the Fleet ▸ Apply button does.
//   - **A failure suppresses that release**, not the feature, and not for ever:
//     an admin applying it by hand, or a newer release, clears it.
//
// It NEVER sends `force`. Not a knob: `force` is an operator agreeing to end N
// live sessions, and there is no operator here. (Post-#153 `force` additionally
// stops sessions on a migrating release — which this path is never allowed to
// reach anyway.)

// Reasons an unattended pass did not start a run. Stable identifiers, written
// into the detection job's run summary, so an operator can read WHY nothing
// happened rather than inferring it from silence.
const (
	// AutoApplyDisabled — the instance has not opted in. The default.
	AutoApplyDisabled = "disabled"
	// AutoApplyNoRelease — nothing the channel offers is newer than this
	// control plane.
	AutoApplyNoRelease = "no_release"
	// AutoApplyCarriesMigration — the newest offered release would migrate the
	// database, which drains every session on the instance. Left for an admin.
	AutoApplyCarriesMigration = "carries_migration"
	// AutoApplyNotEligible — the control-plane target cannot take it (Detail
	// carries the EligibilityReason). Nothing moves before the control plane.
	AutoApplyNotEligible = "not_eligible"
	// AutoApplyInFlight — a run or a per-host attempt is already going.
	AutoApplyInFlight = "in_flight"
	// AutoApplyFailedBefore — an unattended run already failed on this release.
	// Not retried until an admin looks at it, or a newer release appears.
	AutoApplyFailedBefore = "failed_before"
	// AutoApplyStarted — a fleet run was started.
	AutoApplyStarted = "started"
)

// AutoApplyDecision is what a pass decided and why. Reason is always set;
// ReleaseID is set whenever a candidate was identified, INCLUDING when the
// candidate was refused, because "which release we declined and why" is the
// useful half of the record.
type AutoApplyDecision struct {
	Apply     bool
	ReleaseID string
	Reason    string
	// Detail qualifies Reason where the identifier alone is not enough — today
	// only the EligibilityReason behind AutoApplyNotEligible.
	Detail string
}

// AutoApplyInputs is everything the decision reads. Values, not accessors: the
// decision does no I/O, so it can be a table test rather than a fixture.
type AutoApplyInputs struct {
	// Enabled is instance_settings.platform_auto_apply.
	Enabled bool
	// View is the same release view the Releases page and the fleet apply
	// handler read, so "what is offered" cannot mean two things.
	View View
	// AnyAttemptOpen is len(OpenAttempts) > 0 — a per-host apply or revert can
	// be in flight without a fleet run existing.
	AnyAttemptOpen bool
	// SuppressedReleaseIDs are releases an unattended run has already failed on.
	SuppressedReleaseIDs map[string]bool
}

// PlanAutoApply is the whole decision, and it is pure.
//
// It deliberately mirrors handleFleetApply's refusal ladder rather than
// inventing a second one — same `Available[0]` candidate, same
// `fleetTargetReason(view, nil)` gate, same "nothing moves before the control
// plane" rule. Where the handler answers HTTP, this answers an identifier for
// the run summary. Two ladders over one set of rules is how a release becomes
// applicable to a scheduler and refused to an admin, or the reverse.
func PlanAutoApply(in AutoApplyInputs) AutoApplyDecision {
	if !in.Enabled {
		return AutoApplyDecision{Reason: AutoApplyDisabled}
	}
	if len(in.View.Available) == 0 {
		return AutoApplyDecision{Reason: AutoApplyNoRelease}
	}
	// Available is newest-first and already filtered to what this channel
	// offers at or above the installed schema (ADR 0002), so the head is the
	// candidate — the same one the console's Update button names.
	candidate := in.View.Available[0]

	// The migration refusal comes BEFORE the eligibility and in-flight checks
	// on purpose: it is the decision an operator most needs to see in the
	// summary, and reporting `in_flight` for a release that would never have
	// been applied unattended anyway would be misleading.
	if candidate.Migrates {
		return AutoApplyDecision{ReleaseID: candidate.ID, Reason: AutoApplyCarriesMigration}
	}
	if in.SuppressedReleaseIDs[candidate.ID] {
		return AutoApplyDecision{ReleaseID: candidate.ID, Reason: AutoApplyFailedBefore}
	}
	// `up_to_date` is not a refusal for the fleet handler — the run then goes
	// straight to the hosts — and it is not one here either. `run_active` is
	// already collapsed to "" by fleetTargetReason, so the in-flight check
	// below is what catches that.
	if reason := fleetTargetReason(in.View, nil); reason != "" && reason != ReasonUpToDate {
		return AutoApplyDecision{ReleaseID: candidate.ID, Reason: AutoApplyNotEligible, Detail: reason}
	}
	if in.AnyAttemptOpen || in.View.ActiveApply != nil && in.View.ActiveApply.Run != nil {
		return AutoApplyDecision{ReleaseID: candidate.ID, Reason: AutoApplyInFlight}
	}
	return AutoApplyDecision{Apply: true, ReleaseID: candidate.ID, Reason: AutoApplyStarted}
}

// autoApplyStore is the I/O the applier needs, named narrowly so a test can
// supply it without a database.
type autoApplyStore interface {
	OpenAttempts(ctx context.Context) ([]Attempt, error)
	// UnattendedFailedReleaseIDs is the suppression set: releases an unattended
	// run has already failed on.
	UnattendedFailedReleaseIDs(ctx context.Context) (map[string]bool, error)
	// CreateUnattendedRun is CreateRun with unattended=true, force=false and no
	// requesting admin. A separate method rather than a bool argument, so
	// `force` is not even expressible on this path.
	CreateUnattendedRun(ctx context.Context, releaseID string) (ApplyRun, error)
}

// AutoApplyDeps are the seams to the rest of the process. Function fields for
// the same reason ApplyDeps uses them: internal/settings and the job registry
// sit above this package.
type AutoApplyDeps struct {
	// Enabled reads instance_settings.platform_auto_apply.
	Enabled func(ctx context.Context) (bool, error)
	// View is the release view, the same read the Releases page performs.
	View func(ctx context.Context) (View, error)
	// Start hands the run to the existing fleet sequencer.
	Start func(run ApplyRun)
}

// AutoApplier runs one unattended pass. Constructed once; Consider is called by
// the detection job after a successful detect.
type AutoApplier struct {
	store autoApplyStore
	deps  AutoApplyDeps
	log   *slog.Logger
}

func NewAutoApplier(store autoApplyStore, deps AutoApplyDeps, log *slog.Logger) *AutoApplier {
	return &AutoApplier{store: store, deps: deps, log: log}
}

// AutoApplyOutcome is one pass, for the job summary.
type AutoApplyOutcome struct {
	Decision AutoApplyDecision
	RunID    string
	// Err is a read that failed. A pass that cannot read its inputs decides
	// nothing rather than guessing — and it must not fail the detection job,
	// which had already succeeded by the time we were called.
	Err error
}

// Summary folds into the detection run's summary map, alongside the detector's
// and the notifier's keys. Same shape as NotifyOutcome.Summary.
func (o AutoApplyOutcome) Summary() map[string]any {
	// A disabled instance is the overwhelming majority and says nothing: an
	// operator reading a run record should not have to scroll past a line
	// telling them a feature they never turned on did not run.
	if o.Decision.Reason == AutoApplyDisabled && o.Err == nil {
		return nil
	}
	out := map[string]any{"auto_apply": o.Decision.Reason}
	if o.Decision.ReleaseID != "" {
		out["auto_apply_release_id"] = o.Decision.ReleaseID
	}
	if o.Decision.Detail != "" {
		out["auto_apply_detail"] = o.Decision.Detail
	}
	if o.RunID != "" {
		out["auto_apply_run_id"] = o.RunID
	}
	if o.Err != nil {
		out["auto_apply_error"] = o.Err.Error()
	}
	return out
}

// Consider runs one pass. It is called from the detection job body AFTER a
// successful detect, so `Available` already reflects whatever that pass found.
//
// It never returns an error to its caller: detection succeeded, and a release
// this instance chose not to install is not a detection failure. Everything that
// went wrong is in the outcome, and therefore in the run record.
func (a *AutoApplier) Consider(ctx context.Context) AutoApplyOutcome {
	enabled, err := a.deps.Enabled(ctx)
	if err != nil {
		return AutoApplyOutcome{Decision: AutoApplyDecision{Reason: AutoApplyDisabled}, Err: err}
	}
	if !enabled {
		// Read nothing else. The default path costs one boolean.
		return AutoApplyOutcome{Decision: AutoApplyDecision{Reason: AutoApplyDisabled}}
	}

	view, err := a.deps.View(ctx)
	if err != nil {
		return AutoApplyOutcome{Decision: AutoApplyDecision{Reason: AutoApplyNoRelease}, Err: err}
	}
	open, err := a.store.OpenAttempts(ctx)
	if err != nil {
		return AutoApplyOutcome{Decision: AutoApplyDecision{Reason: AutoApplyInFlight}, Err: err}
	}
	suppressed, err := a.store.UnattendedFailedReleaseIDs(ctx)
	if err != nil {
		// Fail CLOSED: unable to tell whether an unattended run already failed
		// on this release, do not start another one. The cost of waiting a week
		// is a week; the cost of guessing is re-marching a known-bad release
		// across the fleet.
		return AutoApplyOutcome{Decision: AutoApplyDecision{Reason: AutoApplyFailedBefore}, Err: err}
	}

	decision := PlanAutoApply(AutoApplyInputs{
		Enabled:              true,
		View:                 view,
		AnyAttemptOpen:       len(open) > 0,
		SuppressedReleaseIDs: suppressed,
	})
	if !decision.Apply {
		a.log.Info("platform auto-apply: nothing started",
			"reason", decision.Reason, "detail", decision.Detail, "release_id", decision.ReleaseID)
		return AutoApplyOutcome{Decision: decision}
	}

	run, err := a.store.CreateUnattendedRun(ctx, decision.ReleaseID)
	if err != nil {
		// ErrRunActive included: the database's active-run index is the real
		// single-flight, and losing that race is not a failure worth escalating.
		a.log.Warn("platform auto-apply: could not create the run",
			"release_id", decision.ReleaseID, "err", err)
		return AutoApplyOutcome{Decision: AutoApplyDecision{
			ReleaseID: decision.ReleaseID, Reason: AutoApplyInFlight}, Err: err}
	}
	a.log.Info("platform auto-apply: starting a fleet run",
		"release_id", decision.ReleaseID, "run_id", run.ID, "token", "auto-apply-started")
	a.deps.Start(run)
	return AutoApplyOutcome{Decision: decision, RunID: run.ID}
}
