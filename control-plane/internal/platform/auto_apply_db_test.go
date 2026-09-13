package platform

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// The #122 policy against real rows. The unit table proves the decision; these
// prove the two halves that only exist in the database: an unattended run is
// created without `force` and marked `unattended`, and the failure suppression
// is answerable from the run history.

// An unattended run is force-free and marked, and the marker is served.
func TestCreateUnattendedRunIsMarkedAndNeverForced(t *testing.T) {
	h := newFleetHarness(t, commitA, parkedDrivers{})

	run, err := h.store.CreateUnattendedRun(context.Background(), h.release.ID)
	if err != nil {
		t.Fatalf("create unattended run: %v", err)
	}
	if !run.Unattended {
		t.Error("unattended must be true — the failure suppression is unanswerable without it")
	}
	if run.Force {
		t.Error("an unattended run must never be forced: force is an operator agreeing to end sessions")
	}
	if run.RequestedBy != nil {
		t.Errorf("requested_by = %v, want null: nobody requested it", *run.RequestedBy)
	}
}

// The suppression set is per release and counts only unattended failures — an
// admin's own failed run must not stop the schedule from trying again.
func TestUnattendedFailedReleaseIDsCountsOnlyUnattendedFailures(t *testing.T) {
	ctx := context.Background()
	h := newFleetHarness(t, commitA, parkedDrivers{})
	other := seedRelease(t, h.store, commitC, buildinfo.Get().SchemaVersion)

	// An unattended run that failed on h.release.
	auto, err := h.store.CreateUnattendedRun(ctx, h.release.ID)
	if err != nil {
		t.Fatalf("create unattended run: %v", err)
	}
	mustExec(t, h.pool, `UPDATE platform_apply_runs SET state='failed' WHERE id = $1::uuid`, auto.ID)

	// An ADMIN's run that failed on the other release. Not the schedule's doing,
	// so it must not suppress anything.
	manual, err := h.store.CreateRun(ctx, other.ID, false, nil, nil)
	if err != nil {
		t.Fatalf("create admin run: %v", err)
	}
	mustExec(t, h.pool, `UPDATE platform_apply_runs SET state='failed' WHERE id = $1::uuid`, manual.ID)

	got, err := h.store.UnattendedFailedReleaseIDs(ctx)
	if err != nil {
		t.Fatalf("read suppression set: %v", err)
	}
	if !got[h.release.ID] {
		t.Error("the release an unattended run failed on must be suppressed")
	}
	if got[other.ID] {
		t.Error("an ADMIN's failed run must not suppress the schedule — one flaky release is not a policy")
	}
}

// A SUCCEEDED unattended run does not suppress anything: the suppression is
// about a release that failed, not about having been automatic.
func TestASucceededUnattendedRunSuppressesNothing(t *testing.T) {
	ctx := context.Background()
	h := newFleetHarness(t, commitA, parkedDrivers{})

	run, err := h.store.CreateUnattendedRun(ctx, h.release.ID)
	if err != nil {
		t.Fatalf("create unattended run: %v", err)
	}
	mustExec(t, h.pool, `UPDATE platform_apply_runs SET state='succeeded' WHERE id = $1::uuid`, run.ID)

	got, err := h.store.UnattendedFailedReleaseIDs(ctx)
	if err != nil {
		t.Fatalf("read suppression set: %v", err)
	}
	if got[h.release.ID] {
		t.Error("a succeeded run must not suppress its own release")
	}
}

// `unattended` reaches the wire, so a client can tell an admin why a fleet run
// they did not start exists.
func TestUnattendedIsServedOnTheRunEnvelope(t *testing.T) {
	ctx := context.Background()
	h := newFleetHarness(t, commitA, parkedDrivers{})
	if _, err := h.store.CreateUnattendedRun(ctx, h.release.ID); err != nil {
		t.Fatalf("create unattended run: %v", err)
	}

	code, raw := h.do(t, http.MethodGet, "/v1/admin/platform/apply/runs", h.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("GET runs = %d %s, want 200", code, raw)
	}
	if !strings.Contains(string(raw), `"unattended":true`) {
		t.Fatalf("runs response does not carry unattended:true — %s", string(raw))
	}
}

// The operator's rule is "an admin applying the failed release themselves clears
// the suppression". A `DISTINCT release_id WHERE unattended AND failed` query
// suppresses for ever, so this is the test that the rule is actually implemented
// rather than only documented.
func TestAnAdminsOwnRunClearsTheUnattendedSuppression(t *testing.T) {
	ctx := context.Background()
	h := newFleetHarness(t, commitA, parkedDrivers{})

	// An unattended run fails on the release.
	auto, err := h.store.CreateUnattendedRun(ctx, h.release.ID)
	if err != nil {
		t.Fatalf("create unattended run: %v", err)
	}
	mustExec(t, h.pool, `UPDATE platform_apply_runs SET state='failed' WHERE id = $1::uuid`, auto.ID)
	got, err := h.store.UnattendedFailedReleaseIDs(ctx)
	if err != nil {
		t.Fatalf("read suppression: %v", err)
	}
	if !got[h.release.ID] {
		t.Fatal("a failed unattended run must suppress its release")
	}

	// The admin then applies it themselves. created_at must be LATER, so the
	// admin's run is the most recent one on this release.
	manual, err := h.store.CreateRun(ctx, h.release.ID, false, nil, nil)
	if err != nil {
		t.Fatalf("create admin run: %v", err)
	}
	mustExec(t, h.pool,
		`UPDATE platform_apply_runs SET state='succeeded', created_at = now() + interval '1 second' WHERE id = $1::uuid`,
		manual.ID)

	got, err = h.store.UnattendedFailedReleaseIDs(ctx)
	if err != nil {
		t.Fatalf("read suppression: %v", err)
	}
	if got[h.release.ID] {
		t.Error("an admin's own run must clear the suppression — otherwise a host left behind by " +
			"one bad pass never updates again until a newer release appears")
	}
}

// Decision 1 must hold at the SEQUENCER, not only where the run was decided. The
// drain decision is re-made from the store row, and an unreadable row reads as
// migrating — so without this guard a transient read failure turns a run nobody
// is watching into a fleet-wide drain.
func TestAnUnattendedRunIsRefusedWhenItsReleaseWouldMigrate(t *testing.T) {
	ctx := context.Background()
	h := newFleetHarness(t, commitA, parkedDrivers{})
	migrating := seedRelease(t, h.store, commitC, buildinfo.Get().SchemaVersion+1)
	seedSession(t, h.pool, h.hostID)

	run, err := h.store.CreateUnattendedRun(ctx, migrating.ID)
	if err != nil {
		t.Fatalf("create unattended run: %v", err)
	}
	h.fleet.Start(run)

	waitFor(t, "the unattended run to be refused", func() bool {
		r, err := h.store.Run(ctx, run.ID)
		return err == nil && TerminalRunState(r.State)
	})
	final, err := h.store.Run(ctx, run.ID)
	if err != nil || final.State != RunFailed {
		t.Fatalf("run state = %q (%v), want failed: an unattended run must never drain", final.State, err)
	}
	// The live session is untouched and no attempt was ever created.
	if got := sessionStates(t, h.pool); len(got) != 1 || got[0] != "running" {
		t.Fatalf("session states = %v, want the running session left alone", got)
	}
	as, err := h.store.RunAttempts(ctx, run.ID)
	if err == nil && len(as) != 0 {
		t.Fatalf("attempts = %d, want none: the refusal precedes the control-plane attempt", len(as))
	}
}

// A succeeded_partial unattended run suppresses nothing (amendment 9): the
// host it passed over is picked up on the next pass, which is the whole point
// of the state not being a failure.
func TestAPartialUnattendedRunSuppressesNothing(t *testing.T) {
	ctx := context.Background()
	h := newFleetHarness(t, commitA, parkedDrivers{})
	auto, err := h.store.CreateUnattendedRun(ctx, h.release.ID)
	if err != nil {
		t.Fatalf("create unattended run: %v", err)
	}
	if err := h.store.RecordSkip(ctx, auto.ID, RunSkip{HostID: h.hostID, NodeName: "gpu-01", Reason: ReasonHostOffline}); err != nil {
		t.Fatalf("record skip: %v", err)
	}
	if err := h.store.FinishRun(ctx, auto.ID, RunSucceededPartial, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, err := h.store.UnattendedFailedReleaseIDs(ctx)
	if err != nil {
		t.Fatalf("read suppression set: %v", err)
	}
	if got[h.release.ID] {
		t.Error("a partial run is not a failure and must not suppress the release")
	}
}
