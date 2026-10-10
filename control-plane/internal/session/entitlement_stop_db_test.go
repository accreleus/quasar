package session

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

func sessionState(t *testing.T, store *Store, id string) State {
	t.Helper()
	sess, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get session %s: %v", id, err)
	}
	return sess.State
}

// TestRevokingTheAllRowStopsOnlyTheUsersItUncovers — control-api.md amendment 23
// (#503): a user still covered by a personal row keeps their session, and a
// stopped session cannot be reconnected to.
func TestRevokingTheAllRowStopsOnlyTheUsersItUncovers(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	covered := seedExtraUser(t, pool, 2, 1)
	must(t, execEnt(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('user', $1::uuid, $2::uuid, 'admin')`, covered, s.appID))
	lost := insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "running")
	kept := insertSessionRow(t, pool, covered, s.appID, &s.hostID, "running")
	other := insertApp(t, pool, "other", 512, 1)
	elsewhere := insertSessionRow(t, pool, s.userID, other, &s.hostID, "running")

	// Nothing removed yet: the sweep stops nobody.
	if ids, err := coord.StopUnentitledSessions(ctx, s.appID); err != nil || len(ids) != 0 {
		t.Fatalf("sweep with every owner entitled: got %v, %v; want none", ids, err)
	}

	must(t, execEnt(ctx, pool, `DELETE FROM entitlements WHERE app_id = $1::uuid AND subject_type = 'all'`, s.appID))
	ids, err := coord.StopUnentitledSessions(ctx, s.appID)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !slices.Equal(ids, []string{lost}) {
		t.Fatalf("stopped %v, want only %s", ids, lost)
	}
	if got := sessionState(t, store, lost); got != StateStopping {
		t.Errorf("uncovered owner's session: %s, want stopping", got)
	}
	if got := disp.stopReason(lost); got != StopReasonEntitlementRevoked {
		t.Errorf("session_stop reason: %q, want %q", got, StopReasonEntitlementRevoked)
	}
	// The ack is never awaited: an admin request must not wait on N agents.
	if slices.Contains(disp.types(), "stop") {
		t.Errorf("the sweep awaited a session_stop ack: %v", disp.types())
	}
	if got := sessionState(t, store, kept); got != StateRunning {
		t.Errorf("session of a user still covered by a personal row: %s, want running", got)
	}
	if got := sessionState(t, store, elsewhere); got != StateRunning {
		t.Errorf("the same user's session on another app: %s, want running", got)
	}
	if _, err := store.MintSignalingToken(ctx, lost); !errors.Is(err, ErrSessionTerminal) {
		t.Errorf("signaling token for the stopped session: %v, want ErrSessionTerminal", err)
	}
}

// TestRestrictingTheParentStopsADerivedTileSession — amendment 22's parent rule
// applied to a live session: the tile's own row is untouched.
func TestRestrictingTheParentStopsADerivedTileSession(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	parent := seedSteamApp(t, pool, `{"gpu":true}`)
	tile := seedTile(t, pool, parent, "Redout", "517710")
	admin := seedExtraUser(t, pool, 2, 1)
	onTile := insertSessionRow(t, pool, s.userID, tile, &s.hostID, "running")
	adminOnParent := insertSessionRow(t, pool, admin, parent, &s.hostID, "starting")

	// Entitlement mode "user": the provider app is left to the acting admin alone.
	must(t, execEnt(ctx, pool, `DELETE FROM entitlements WHERE app_id = $1::uuid`, parent))
	must(t, execEnt(ctx, pool, `INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('user', $1::uuid, $2::uuid, 'admin')`, admin, parent))

	ids, err := coord.StopUnentitledSessions(ctx, parent)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !slices.Equal(ids, []string{onTile}) {
		t.Fatalf("stopped %v, want only the tile session %s", ids, onTile)
	}
	if got := sessionState(t, store, onTile); got != StateStopping {
		t.Errorf("derived-tile session: %s, want stopping", got)
	}
	if got := sessionState(t, store, adminOnParent); got != StateStarting {
		t.Errorf("the still-entitled admin's session: %s, want starting", got)
	}
}

// TestSwapCommittedIntoARevokedAppIsStopped — the revoke lands after Swap's
// entitlement check and before the commit, so the sweep listed the session under
// its old app.
func TestSwapCommittedIntoARevokedAppIsStopped(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	sess := runningSession(t, store, s)
	target := insertApp(t, pool, "target", 512, 1)
	if _, err := coord.Swap(ctx, sess.ID, target); err != nil {
		t.Fatalf("swap: %v", err)
	}
	waitFor(t, func() bool { return slices.Contains(disp.types(), "swap") })

	must(t, execEnt(ctx, pool, `DELETE FROM entitlements WHERE app_id = $1::uuid`, target))
	if ids, err := coord.StopUnentitledSessions(ctx, target); err != nil || len(ids) != 0 {
		t.Fatalf("sweep mid-swap: got %v, %v; the session still names its old app", ids, err)
	}

	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sess.ID, State: "running", Detail: "swap complete"})
	if got := sessionState(t, store, sess.ID); got != StateStopping {
		t.Fatalf("session swapped into a revoked app: %s, want stopping", got)
	}
	if got := disp.stopReason(sess.ID); got != StopReasonEntitlementRevoked {
		t.Errorf("session_stop reason: %q, want %q", got, StopReasonEntitlementRevoked)
	}
}
