package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// AS10-06 fail edge — DB integration test. evaluateHealth, on a sustained
// below-floor run, must drive the session to failed AND stamp
// state_detail = "unsustainable: <reason>" (mirroring the host_lost reap edge),
// keeping error_message too. The web streamHealth.ts failedUnsustainable banner
// branch keys on this state_detail prefix, so it would be dead code if the fail
// path left state_detail untouched. Needs Postgres (exercises the 0012 migration
// + the lifecycle transaction).
func TestEvaluateHealthFailStampsUnsustainableDetail(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	// Tie the session to a launch profile AND its resolved rung. UI-P4: health
	// evaluation reads the ABR floor of the RESOLVED RUNG from the database
	// (1080p60-h264 → 4000 kbps inherited from the seed), not of the launch
	// profile — a chain has no single floor, and a chain that fell through to a
	// lower rung has a lower one.
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET profile_id = '1080p60', stream_profile_id = '1080p60-h264' WHERE id::text = $1`,
		sess.ID); err != nil {
		t.Fatalf("set profile ids: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateStarting, nil, nil); err != nil {
		t.Fatalf("→ starting: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("→ running: %v", err)
	}

	// Seed a below-floor run that is already well past the unsustainable threshold,
	// then feed a sub-floor setpoint so Evaluate returns ShouldFail.
	coord.health.mu.Lock()
	coord.health.healthRuns[sess.ID] = time.Now().Add(-5 * time.Minute)
	coord.health.mu.Unlock()
	coord.health.evaluateHealth(ctx, sess.ID, 3000) // below the 4000 kbps floor

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.State != StateFailed {
		t.Fatalf("state: got %s want failed", got.State)
	}
	if got.HealthState != HealthUnsustainable {
		t.Fatalf("health_state: got %q want unsustainable", got.HealthState)
	}
	if got.StateDetail == nil || !strings.HasPrefix(*got.StateDetail, "unsustainable:") {
		d := "<nil>"
		if got.StateDetail != nil {
			d = *got.StateDetail
		}
		t.Fatalf("state_detail: got %q want prefix \"unsustainable:\"", d)
	}
	if got.ErrorMessage == nil || !strings.HasPrefix(*got.ErrorMessage, "unsustainable:") {
		e := "<nil>"
		if got.ErrorMessage != nil {
			e = *got.ErrorMessage
		}
		t.Fatalf("error_message: got %q want prefix \"unsustainable:\"", e)
	}
}

// AS10-11 two-writer no-clobber — DB integration test. The network evaluator
// (evaluateHealth) and the client evaluator (EvaluateClientHealth) both write
// health_state. The network path returns HealthHealthy for any above-floor sample;
// it must NOT overwrite a live client_decode_degrading/client_presentation_degrading
// banner set by the client evaluator (the client evaluator owns clearing it on its
// own recovery). But network degradation MUST still override a client_* state —
// network precedence wins. This test exercises both directions.
func TestEvaluateHealthYieldsToClientState(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	// The 1080p60-h264 RUNG carries the ABR floor (4000 kbps). Drive to running so
	// a fail is legal.
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET profile_id = '1080p60', stream_profile_id = '1080p60-h264' WHERE id::text = $1`,
		sess.ID); err != nil {
		t.Fatalf("set profile ids: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateStarting, nil, nil); err != nil {
		t.Fatalf("→ starting: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("→ running: %v", err)
	}

	// The client evaluator has set client_decode_degrading.
	if err := store.UpdateHealthState(ctx, sess.ID, HealthClientDecodeDegrading, nil); err != nil {
		t.Fatalf("set client_decode_degrading: %v", err)
	}

	// (1) A healthy (above-floor) agent sample must NOT clobber the client_* state.
	coord.health.evaluateHealth(ctx, sess.ID, 5000) // above the 4000 kbps floor
	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get after healthy sample: %v", err)
	}
	if got.HealthState != HealthClientDecodeDegrading {
		t.Fatalf("healthy agent sample clobbered client state: got %q want client_decode_degrading", got.HealthState)
	}

	// (2) A sustained below-floor agent sample MUST override the client_* state
	//     (network precedence). Seed a below-floor run past the unsustainable
	//     threshold so it escalates straight to unsustainable + fail.
	coord.health.mu.Lock()
	coord.health.healthRuns[sess.ID] = time.Now().Add(-5 * time.Minute)
	coord.health.mu.Unlock()
	coord.health.evaluateHealth(ctx, sess.ID, 3000) // below the 4000 kbps floor

	got, err = store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get after below-floor sample: %v", err)
	}
	if got.HealthState != HealthUnsustainable {
		t.Fatalf("below-floor agent sample did not override client state: got %q want unsustainable", got.HealthState)
	}
}

// healthMapsContain reports whether the healthEvaluator's in-memory maps still
// carry an entry for sessionID. White-box (same package) so the test can reach
// into the collaborator's state directly, matching the existing health_db_test
// style of touching coord.health.mu/healthRuns.
func healthMapsContain(coord *Coordinator, sessionID string) (inHealthRuns, inClientRuns bool) {
	coord.health.mu.Lock()
	defer coord.health.mu.Unlock()
	_, inHealthRuns = coord.health.healthRuns[sessionID]
	_, inClientRuns = coord.health.clientRuns[sessionID]
	return
}

// TestHealthMapLeak_FailSessionWithDetailForgets pins the health-evaluator map
// leak fix: failing a session via Coordinator.failSessionWithDetail (used by the
// unsustainable-health fail path and commandOK) must drop its entries from both
// healthRuns and clientRuns, not just leave them for evaluateHealth's own
// below-floor delete to (never) reach.
func TestHealthMapLeak_FailSessionWithDetailForgets(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateStarting, nil, nil); err != nil {
		t.Fatalf("→ starting: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("→ running: %v", err)
	}

	// Seed both maps as if evaluate paths had populated them.
	coord.health.mu.Lock()
	coord.health.healthRuns[sess.ID] = time.Now().Add(-time.Minute)
	coord.health.clientRuns[sess.ID] = &clientHealthRun{class: ClientHealthDecode, since: time.Now()}
	coord.health.mu.Unlock()

	if inHR, inCR := healthMapsContain(coord, sess.ID); !inHR || !inCR {
		t.Fatalf("precondition: expected both maps to carry %s, got healthRuns=%v clientRuns=%v", sess.ID, inHR, inCR)
	}

	coord.failSessionWithDetail(sess.ID, "test fail", nil)

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.State != StateFailed {
		t.Fatalf("state: got %s want failed", got.State)
	}
	if inHR, inCR := healthMapsContain(coord, sess.ID); inHR || inCR {
		t.Fatalf("leak: session %s still tracked after fail: healthRuns=%v clientRuns=%v", sess.ID, inHR, inCR)
	}
}

// TestHealthMapLeak_HostDisconnectedForgets pins the reap-path forget: a session
// that goes terminal via HostDisconnected's ReapHost (not via evaluateHealth's own
// delete) must also have its health-run tracking dropped.
func TestHealthMapLeak_HostDisconnectedForgets(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateStarting, nil, nil); err != nil {
		t.Fatalf("→ starting: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("→ running: %v", err)
	}

	coord.health.mu.Lock()
	coord.health.healthRuns[sess.ID] = time.Now().Add(-time.Minute)
	coord.health.mu.Unlock()

	coord.HostDisconnected(ctx, s.hostID)

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.State != StateFailed {
		t.Fatalf("state: got %s want failed", got.State)
	}
	if inHR, inCR := healthMapsContain(coord, sess.ID); inHR || inCR {
		t.Fatalf("leak: session %s still tracked after host-disconnect reap: healthRuns=%v clientRuns=%v", sess.ID, inHR, inCR)
	}
}

// TestHealthMapLeak_AgentStateTerminalForgets pins the normal agent-reported
// terminal path (AgentState → StateStopped/StateFailed): it must forget the
// session's health-run tracking too, not just the fail/reap paths.
func TestHealthMapLeak_AgentStateTerminalForgets(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateStarting, nil, nil); err != nil {
		t.Fatalf("→ starting: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("→ running: %v", err)
	}

	coord.health.mu.Lock()
	coord.health.healthRuns[sess.ID] = time.Now().Add(-time.Minute)
	coord.health.clientRuns[sess.ID] = &clientHealthRun{class: ClientHealthDecode, since: time.Now()}
	coord.health.mu.Unlock()

	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: sess.ID,
		State:     string(StateStopped),
	})

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.State != StateStopped {
		t.Fatalf("state: got %s want stopped", got.State)
	}
	if inHR, inCR := healthMapsContain(coord, sess.ID); inHR || inCR {
		t.Fatalf("leak: session %s still tracked after agent-reported stop: healthRuns=%v clientRuns=%v", sess.ID, inHR, inCR)
	}
}
