package session

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

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
	mintedBefore, err := store.MintSignalingToken(ctx, lost)
	if err != nil {
		t.Fatalf("mint before the revoke: %v", err)
	}

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
	if _, err := store.ConsumeSignalingToken(ctx, mintedBefore.Plaintext); !errors.Is(err, ErrSessionTerminal) {
		t.Errorf("consuming a token minted before the revoke: %v, want ErrSessionTerminal", err)
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

// TestSweepStopsASessionSwappedIntoARevokedApp — the revoke lands after Swap's
// entitlement check and before the commit, so the route's sweep listed the
// session under its old app. The periodic, unfiltered sweep is what ends it.
func TestSweepStopsASessionSwappedIntoARevokedApp(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	sess := runningSession(t, store, s)
	bystander := insertSessionRow(t, pool, seedExtraUser(t, pool, 2, 1), s.appID, &s.hostID, "running")
	target := insertApp(t, pool, "target", 512, 1)
	if _, err := coord.Swap(ctx, sess.ID, target); err != nil {
		t.Fatalf("swap: %v", err)
	}
	waitFor(t, func() bool { return slices.Contains(disp.types(), "swap") })

	must(t, execEnt(ctx, pool, `DELETE FROM entitlements WHERE app_id = $1::uuid`, target))
	if ids, err := coord.StopUnentitledSessions(ctx, target); err != nil || len(ids) != 0 {
		t.Fatalf("route sweep mid-swap: got %v, %v; the session still names its old app", ids, err)
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sess.ID, State: "running", Detail: "swap complete"})

	ids, err := coord.StopUnentitledSessions(ctx, "")
	if err != nil {
		t.Fatalf("periodic sweep: %v", err)
	}
	if !slices.Equal(ids, []string{sess.ID}) {
		t.Fatalf("periodic sweep stopped %v, want only %s", ids, sess.ID)
	}
	if got := disp.stopReason(sess.ID); got != StopReasonEntitlementRevoked {
		t.Errorf("session_stop reason: %q, want %q", got, StopReasonEntitlementRevoked)
	}
	if got := sessionState(t, store, bystander); got != StateRunning {
		t.Errorf("an entitled session under the unfiltered sweep: %s, want running", got)
	}
}

// TestStopIsConditionalOnTheEvaluatedApp — a session that swapped to another app
// between the sweep's entitlement check and its stop is left alone.
func TestStopIsConditionalOnTheEvaluatedApp(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	sid := insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "running")
	evaluated := insertApp(t, pool, "the app it ran when checked", 512, 1)

	if _, err := coord.stop(ctx, sid, evaluated, StopReasonEntitlementRevoked, false); !errors.Is(err, errAppChanged) {
		t.Fatalf("stop against an app the session no longer runs: %v, want errAppChanged", err)
	}
	if got := sessionState(t, store, sid); got != StateRunning {
		t.Errorf("session after the refused stop: %s, want running", got)
	}
	if got := disp.noAckTypes(); len(got) != 0 {
		t.Errorf("a refused stop still reached the agent: %v", got)
	}
}

// TestHeartbeatResendsAStopTheAgentNeverTook — a `stopping` row the agent still
// lists past stopAckTimeout means the session_stop was lost. The grace runs from
// this process's own record of the stop, so the session's owner cannot postpone
// it by keeping sessions.updated_at fresh, and a row with no record (a restart)
// is past it.
func TestHeartbeatResendsAStopTheAgentNeverTook(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	sid := insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "running")
	if _, err := coord.stop(ctx, sid, "", "user_requested", false); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stops := func() int {
		n := 0
		for _, ty := range disp.noAckTypes() {
			if ty == "stop:"+sid {
				n++
			}
		}
		return n
	}

	coord.AgentHeartbeat(ctx, s.hostID, []string{sid})
	if got := stops(); got != 1 {
		t.Fatalf("stops sent inside the grace: %d, want only the original", got)
	}

	// The grace has passed, and the owner's stats POST has just rewritten the row.
	coord.stopMu.Lock()
	coord.stopRequested[sid] = time.Now().Add(-2 * stopAckTimeout)
	coord.stopMu.Unlock()
	must(t, store.UpdateSessionNegotiatedCodec(ctx, sid, wireCodecAV1))
	var rowIsFresh bool
	must(t, pool.QueryRow(ctx, `SELECT updated_at > now() - interval '5 seconds' FROM sessions WHERE id = $1::uuid`, sid).Scan(&rowIsFresh))
	if !rowIsFresh {
		t.Fatal("fixture: the codec update did not move updated_at")
	}

	coord.AgentHeartbeat(ctx, s.hostID, []string{sid})
	if got := stops(); got != 2 {
		t.Fatalf("stops sent past the grace on a row kept fresh: %d, want the re-send", got)
	}
	if got := disp.stopReason(sid); got != "error" {
		t.Errorf("corrective session_stop reason: %q, want error", got)
	}
	// A repeated stop must not restart the grace.
	if _, err := coord.stop(ctx, sid, "", "user_requested", false); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if coord.stopInGrace(sid) {
		t.Error("a repeated stop restarted the re-send grace")
	}

	// A stopping row this process holds no record for: no grace to wait out.
	orphan := insertSessionRow(t, pool, seedExtraUser(t, pool, 2, 1), s.appID, &s.hostID, "stopping")
	coord.AgentHeartbeat(ctx, s.hostID, []string{orphan})
	if !slices.Contains(disp.noAckTypes(), "stop:"+orphan) {
		t.Errorf("a stopping row with no stop on record got no re-send: %v", disp.noAckTypes())
	}
}

// TestHeartbeatStopsAPreRunningSessionTheAgentNeverLists — a session stopped
// before it started, whose session_stop was lost, is in no agent list, so only
// this path can release its slot and home. The row is not made terminal here:
// the agent's `stopped` does that.
func TestHeartbeatStopsAPreRunningSessionTheAgentNeverLists(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	sid := insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "assigned")
	if _, err := coord.stop(ctx, sid, "", StopReasonEntitlementRevoked, false); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stops := func() int {
		n := 0
		for _, ty := range disp.noAckTypes() {
			if ty == "stop:"+sid {
				n++
			}
		}
		return n
	}

	coord.AgentHeartbeat(ctx, s.hostID, []string{})
	if got := stops(); got != 1 {
		t.Fatalf("stops sent inside the grace: %d, want only the original", got)
	}

	coord.stopMu.Lock()
	coord.stopRequested[sid] = time.Now().Add(-2 * stopAckTimeout)
	coord.stopMu.Unlock()
	coord.AgentHeartbeat(ctx, s.hostID, []string{})
	if got := stops(); got != 2 {
		t.Fatalf("stops sent past the grace for an unlisted pre-running row: %d, want the re-send", got)
	}
	if got := sessionState(t, store, sid); got != StateStopping {
		t.Errorf("row after the re-send: %s, want stopping until the agent reports", got)
	}
}

// TestSweepStopsASessionWhoseSwapNeverResolved — a row left in the durable
// `swapping` guard names an app the agent may no longer run. A fresh coordinator
// (a restart) has no swap on record for it; a swap on record is given
// swapUnresolvedAfter.
func TestSweepStopsASessionWhoseSwapNeverResolved(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	survivor := insertSessionRow(t, pool, seedExtraUser(t, pool, 2, 1), s.appID, &s.hostID, "running")
	must(t, execEnt(ctx, pool, `UPDATE sessions SET state_detail = $2 WHERE id = $1::uuid`, survivor, swapDetailInProgress))
	inFlight := runningSession(t, store, s)
	if _, err := coord.Swap(ctx, inFlight.ID, insertApp(t, pool, "target", 512, 1)); err != nil {
		t.Fatalf("swap: %v", err)
	}

	ids, err := coord.StopUnentitledSessions(ctx, "")
	if err != nil || len(ids) != 0 {
		t.Fatalf("sweep: got %v, %v; nobody here lost an entitlement", ids, err)
	}
	if got := sessionState(t, store, survivor); got != StateStopping {
		t.Errorf("swapping row with no swap on record: %s, want stopping", got)
	}
	if got := disp.stopReason(survivor); got != "error" {
		t.Errorf("session_stop reason for the unresolved swap: %q, want error", got)
	}
	if got := sessionState(t, store, inFlight.ID); got != StateRunning {
		t.Fatalf("session with a swap in flight: %s, want running", got)
	}

	coord.swapper.mu.Lock()
	coord.swapper.swapSince[inFlight.ID] = time.Now().Add(-2 * swapUnresolvedAfter)
	coord.swapper.mu.Unlock()
	if _, err := coord.StopUnentitledSessions(ctx, ""); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := sessionState(t, store, inFlight.ID); got != StateStopping {
		t.Errorf("session whose swap outlived swapUnresolvedAfter: %s, want stopping", got)
	}
}

// TestTokenConsumeWaitsForAStopInFlight — the consume locks the session row, so
// it cannot read `running`, lose the race to a stop, and still attach.
func TestTokenConsumeWaitsForAStopInFlight(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sid := insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "running")
	tok, err := store.MintSignalingToken(ctx, sid)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// A stop that has written `stopping` and not yet committed.
	stop, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer stop.Rollback(ctx) //nolint:errcheck
	if _, err := stop.Exec(ctx, `UPDATE sessions SET state = 'stopping' WHERE id = $1::uuid`, sid); err != nil {
		t.Fatalf("stop update: %v", err)
	}

	consumed := make(chan error, 1)
	go func() {
		_, err := store.ConsumeSignalingToken(ctx, tok.Plaintext)
		consumed <- err
	}()
	select {
	case err := <-consumed:
		t.Fatalf("consume did not wait for the session row (returned %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	must(t, stop.Commit(ctx))
	if err := <-consumed; !errors.Is(err, ErrSessionTerminal) {
		t.Fatalf("consume after the stop committed: %v, want ErrSessionTerminal", err)
	}
}
