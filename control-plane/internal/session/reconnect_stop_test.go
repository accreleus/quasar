package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/storage"
)

func TestReconnectStopBeforeFirstEmptyHeartbeatStopsHostLost(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	seeded := seed(t, pool, 1)
	app := seedManagedApp(t, pool, `{"image":"managed:1"}`)
	dispatcher := newFakeDispatcher(true)
	forgotten := &recordingForgetter{}
	coord := newTestCoordinator(t, store, dispatcher, testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)),
		WithSessionForgetter(forgotten), WithAuditor(audit.NewStore(pool)))
	ctx := context.Background()
	launched, err := coord.Launch(ctx, seeded.userID, app, StreamOverride{})
	must(t, err)
	waitFor(t, func() bool { return len(dispatcher.types()) >= 2 })
	sid := launched.Session.ID
	coord.AgentState(ctx, seeded.hostID, agentws.SessionStateMsg{SessionID: sid, State: "running"})
	coord.AgentReconnected(ctx, seeded.hostID)
	got, err := store.Get(ctx, sid)
	must(t, err)
	if got.State != StateRunning {
		t.Fatalf("reconnect state = %s, want running", got.State)
	}
	_, err = coord.Stop(ctx, sid, "user_requested")
	must(t, err)
	waitFor(t, func() bool { return len(dispatcher.types()) >= 3 })
	commands := len(dispatcher.types())

	for _, raw := range []string{`{"type":"heartbeat"}`, `{"type":"heartbeat","running_sessions":null}`} {
		var hb agentws.HeartbeatMsg
		must(t, json.Unmarshal([]byte(raw), &hb))
		coord.AgentHeartbeat(ctx, seeded.hostID, hb.RunningSessions)
	}
	for range 2 {
		coord.AgentHeartbeat(ctx, seeded.hostID, []string{sid})
	}
	got, err = store.Get(ctx, sid)
	must(t, err)
	if got.State != StateStopping || got.EndedAt != nil {
		t.Fatalf("missing/listed heartbeat ended slow teardown: %+v", got)
	}
	if len(dispatcher.types()) != commands {
		t.Fatal("heartbeat resent stop during legitimate teardown")
	}
	if _, err := coord.Launch(ctx, seeded.userID, app, StreamOverride{}); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("home during stopping: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(seeded)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("reservation during stopping: %v", err)
	}
	if len(forgotten.seen()) != 0 {
		t.Fatal("forgot a session still tearing down")
	}

	var empty agentws.HeartbeatMsg
	must(t, json.Unmarshal([]byte(`{"type":"heartbeat","running_sessions":[]}`), &empty))
	coord.AgentHeartbeat(ctx, seeded.hostID, empty.RunningSessions)
	got, err = store.Get(ctx, sid)
	must(t, err)
	if got.State != StateStopped || got.StateDetail == nil || *got.StateDetail != "host_lost" || got.EndedAt == nil {
		t.Fatalf("empty heartbeat terminal evidence: %+v", got)
	}
	if got.ErrorMessage != nil || got.FailureCode != nil {
		t.Fatalf("intentional stop reported failure: %+v", got)
	}
	ended := *got.EndedAt
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); coord.AgentHeartbeat(ctx, seeded.hostID, []string{}) }()
	}
	wg.Wait()
	got, err = store.Get(ctx, sid)
	must(t, err)
	if got.EndedAt == nil || !got.EndedAt.Equal(ended) || got.StateDetail == nil || *got.StateDetail != "host_lost" {
		t.Fatalf("duplicate heartbeat changed terminal evidence: %+v", got)
	}
	if ids := forgotten.seen(); len(ids) != 1 || ids[0] != sid {
		t.Fatalf("terminal notifications = %v, want one", ids)
	}
	for _, row := range sessionAuditRows(t, pool) {
		if row.TargetType == "session" && row.Target == sid {
			t.Fatalf("intentional stop audited as failure: %+v", row)
		}
	}
	if _, err := coord.Launch(ctx, seeded.userID, app, StreamOverride{}); err != nil {
		t.Fatalf("home and slot not reusable after terminal: %v", err)
	}
}

// A stop before running is still an in-flight launch. An empty heartbeat cannot
// prove it was a workload the replacement agent lost, so reconnect/disconnect
// reaping remains responsible for this case.
func TestEmptyHeartbeatDoesNotTerminalizePreRunningStop(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	seeded := seed(t, pool, 4)
	dispatcher := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, dispatcher, testLogger())
	ctx := context.Background()

	launched, err := coord.Launch(ctx, seeded.userID, seeded.appID, StreamOverride{})
	must(t, err)
	if _, err := coord.Stop(ctx, launched.Session.ID, "user_requested"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	coord.AgentHeartbeat(ctx, seeded.hostID, []string{})
	got, err := store.Get(ctx, launched.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateStopping {
		t.Fatalf("pre-running stop after empty heartbeat = %s, want stopping", got.State)
	}
}

func TestStoppingManagedHomeBlocksLaunchUntilTerminal(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	seeded := seed(t, pool, 4)
	app := seedManagedApp(t, pool, `{"image":"managed:1"}`)
	dispatcher := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, dispatcher, testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	first, err := coord.Launch(ctx, seeded.userID, app, StreamOverride{})
	must(t, err)
	dispatcher.waitFor(t, "assign")
	if _, err := coord.Stop(ctx, first.Session.ID, "user_requested"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := coord.Launch(ctx, seeded.userID, app, StreamOverride{}); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("launch while the managed-home writer is stopping: %v, want ErrHomeInUse", err)
	}
	coord.AgentState(ctx, seeded.hostID, agentws.SessionStateMsg{SessionID: first.Session.ID, State: "stopped"})
	if _, err := coord.Launch(ctx, seeded.userID, app, StreamOverride{}); err != nil {
		t.Fatalf("launch after terminal teardown: %v", err)
	}
}

func TestStoppingManagedHomeBlocksSwapUntilTerminal(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	seeded := seed(t, pool, 4)
	managed := seedManagedApp(t, pool, `{"image":"managed:1"}`)
	dispatcher := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, dispatcher, testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	old, err := store.ScheduleAndCreate(ctx, launchParams(seeded))
	must(t, err)
	old, err = store.Transition(ctx, old.ID, StateRunning, nil, nil)
	must(t, err)
	holder, err := coord.Launch(ctx, seeded.userID, managed, StreamOverride{})
	must(t, err)
	dispatcher.waitFor(t, "assign")
	if _, err := coord.Stop(ctx, holder.Session.ID, "user_requested"); err != nil {
		t.Fatalf("stop holder: %v", err)
	}
	if _, err := coord.Swap(ctx, old.ID, managed); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("swap while the managed-home writer is stopping: %v, want ErrHomeInUse", err)
	}
	coord.AgentState(ctx, seeded.hostID, agentws.SessionStateMsg{SessionID: holder.Session.ID, State: "stopped"})
	if _, err := coord.Swap(ctx, old.ID, managed); err != nil {
		t.Fatalf("swap after terminal teardown: %v", err)
	}
}

func TestHeartbeatMissingRespectsHostAndReportsFailureOnce(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	seeded := seed(t, pool, 4)
	forgotten := &recordingForgetter{}
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(), WithSessionForgetter(forgotten), WithAuditor(audit.NewStore(pool)))
	ctx := context.Background()
	sess := runningSession(t, store, seeded)
	coord.AgentHeartbeat(ctx, "00000000-0000-4000-8000-000000000001", []string{})
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	coord.AgentHeartbeat(cancelled, seeded.hostID, []string{})
	got, err := store.Get(ctx, sess.ID)
	must(t, err)
	if got.State != StateRunning || len(forgotten.seen()) != 0 {
		t.Fatal("wrong host or cancelled observation changed the session")
	}
	coord.AgentHeartbeat(ctx, seeded.hostID, []string{})
	coord.AgentHeartbeat(ctx, seeded.hostID, []string{})
	got, err = store.Get(ctx, sess.ID)
	must(t, err)
	if got.State != StateFailed || got.StateDetail == nil || *got.StateDetail != "host_lost" {
		t.Fatalf("lost running session: %+v", got)
	}
	var rows []sessionAuditRow
	for _, row := range sessionAuditRows(t, pool) {
		if row.TargetType == "session" && row.Target == sess.ID {
			rows = append(rows, row)
		}
	}
	if len(rows) != 1 || rows[0].Action != "session.failed" || rows[0].Target != sess.ID || rows[0].Details["reason_source"] != "control_plane" {
		t.Fatalf("failure audit = %+v", rows)
	}
	if ids := forgotten.seen(); len(ids) != 1 || ids[0] != sess.ID {
		t.Fatalf("terminal notifications = %v", ids)
	}
}

func TestHeartbeatMissingPreservesConcurrentTerminalEvidence(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	seeded := seed(t, pool, 4)
	forgotten := &recordingForgetter{}
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(), WithSessionForgetter(forgotten))
	ctx := context.Background()
	sess := runningSession(t, store, seeded)
	_, err := store.Transition(ctx, sess.ID, StateStopping, nil, nil)
	must(t, err)
	tx, err := pool.Begin(ctx)
	must(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	// Hold a real competing terminal transaction until reconciliation is waiting on it.
	_, err = tx.Exec(ctx, `UPDATE sessions SET state='stopped', state_detail='teardown complete', ended_at=now() WHERE id::text=$1`, sess.ID)
	must(t, err)
	done := make(chan struct{})
	go func() { defer close(done); coord.AgentHeartbeat(ctx, seeded.hostID, []string{}) }()
	waitFor(t, func() bool {
		var blocked bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::integer = ANY(pg_blocking_pids(pid)))`, int(tx.Conn().PgConn().PID())).Scan(&blocked)
		return err == nil && blocked
	})
	must(t, tx.Commit(ctx))
	<-done
	got, err := store.Get(ctx, sess.ID)
	must(t, err)
	if got.State != StateStopped || got.StateDetail == nil || *got.StateDetail != "teardown complete" {
		t.Fatalf("concurrent terminal evidence overwritten: %+v", got)
	}
	if len(forgotten.seen()) != 0 {
		t.Fatal("heartbeat notified a terminal transition it did not perform")
	}
}

func TestStoppingManagedHomeProtectsDerivedFamily(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	seeded := seed(t, pool, 8)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	tile := seedTile(t, pool, parent, "First game", "100")
	sibling := seedTile(t, pool, parent, "Second game", "200")
	provisionHome(t, pool, seeded.userID, parent, seeded.hostID)
	dispatcher := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, dispatcher, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()
	holder, err := coord.Launch(ctx, seeded.userID, tile, StreamOverride{})
	must(t, err)
	dispatcher.waitFor(t, "assign")
	_, err = coord.Stop(ctx, holder.Session.ID, "user_requested")
	must(t, err)
	params := launchParams(seeded)
	params.NeedEncodeSlots = 2
	other, err := store.ScheduleAndCreate(ctx, params)
	must(t, err)
	_, err = store.Transition(ctx, other.ID, StateRunning, nil, nil)
	must(t, err)
	for _, app := range []string{parent, sibling} {
		if _, err := coord.Launch(ctx, seeded.userID, app, StreamOverride{}); !errors.Is(err, ErrHomeInUse) {
			t.Fatalf("family launch during stop: %v", err)
		}
		if _, err := coord.Swap(ctx, other.ID, app); !errors.Is(err, ErrHomeInUse) {
			t.Fatalf("family swap during stop: %v", err)
		}
	}
	coord.AgentState(ctx, seeded.hostID, agentws.SessionStateMsg{SessionID: holder.Session.ID, State: "stopped"})
	if _, err := coord.Swap(ctx, other.ID, sibling); err != nil {
		t.Fatalf("family home not reusable after terminal: %v", err)
	}
}

func TestHeartbeatReapRollsBackWhenTerminalEvidenceCannotBeDecoded(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	seeded := seed(t, pool, 4)
	ctx := context.Background()
	sess := runningSession(t, store, seeded)
	// PostgreSQL accepts infinity, but Session's time.Time cannot represent it.
	must(t, exec(t, pool, `UPDATE sessions SET created_at = 'infinity'::timestamptz WHERE id::text = $1`, sess.ID))
	reaped, err := store.ReapHeartbeatMissing(ctx, seeded.hostID, []string{})
	if err == nil || len(reaped) != 0 {
		t.Fatalf("unreadable terminal evidence: rows=%v err=%v", reaped, err)
	}
	var state string
	must(t, pool.QueryRow(ctx, `SELECT state FROM sessions WHERE id::text = $1`, sess.ID).Scan(&state))
	if state != string(StateRunning) {
		t.Fatalf("decode failure committed state %s, want running so reconciliation can retry", state)
	}
}
