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
	manual, err := h.store.CreateRun(ctx, other.ID, false, nil)
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
