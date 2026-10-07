package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// Amendment 19 (#455): a console app that does not declare
// runtime_spec.direct_display is refused before anything is scheduled — a
// readiness failure, not a launch. The agentws auto-start gate refuses first;
// this is the backstop behind it.
func TestConsoleLaunchRefusesAppThatCannotRunDirect(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	coord := newTestCoordinator(t, store, newCapturingDispatcher(), testLogger())
	ctx := context.Background()

	_, err := coord.LaunchConsoleSession(ctx, s.hostID, s.userID, s.appID, "local_only", 1920, 1080, 60)
	if !errors.Is(err, ErrConsoleAppNotDirect) {
		t.Fatalf("console launch of an app without direct_display: err = %v, want ErrConsoleAppNotDirect", err)
	}
	var sessions int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = $1::uuid`, s.userID).Scan(&sessions))
	if sessions != 0 {
		t.Fatalf("refused console launch left %d session row(s), want none", sessions)
	}

	// Declared, the same app launches local_only.
	must(t, exec(t, pool, `UPDATE apps SET runtime_spec = runtime_spec || '{"direct_display":true}'::jsonb
		WHERE id::text = $1`, s.appID))
	if _, err := coord.LaunchConsoleSession(ctx, s.hostID, s.userID, s.appID, "local_only", 1920, 1080, 60); err != nil {
		t.Fatalf("console launch of a direct app: %v", err)
	}
	// The retired topology fails closed even for a direct app.
	if _, err := coord.LaunchConsoleSession(ctx, s.hostID, s.userID, s.appID, "dual_output", 1920, 1080, 60); err == nil {
		t.Fatal("dual_output console launch accepted after amendment 19 retired it")
	}
}

// ackSilentDispatcher never answers an ack, as the agent read loop cannot
// while it is the caller.
type ackSilentDispatcher struct {
	mu    sync.Mutex
	sent  []string
	waits int
}

func (d *ackSilentDispatcher) Send(_ string, v any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cmd, ok := v.(agentws.SessionStopCmd); ok {
		d.sent = append(d.sent, cmd.Type)
	}
	return nil
}

func (d *ackSilentDispatcher) SendWithAck(ctx context.Context, _, _ string, _ any) (agentws.AckResult, error) {
	d.mu.Lock()
	d.waits++
	d.mu.Unlock()
	<-ctx.Done()
	return agentws.AckResult{}, ctx.Err()
}

// #477: console teardown runs on the agent's read loop, so it must not wait for
// the session_stop ack that loop would have to read.
func TestConsoleStopDoesNotWaitForTheAck(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	ctx := context.Background()
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	must(t, err)
	_, err = store.Transition(ctx, sess.ID, StateRunning, nil, nil)
	must(t, err)
	d := &ackSilentDispatcher{}
	coord := newTestCoordinator(t, store, d, testLogger())

	start := time.Now()
	must(t, coord.StopConsoleSession(ctx, sess.ID, "console_disabled"))
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("console stop waited %v for an ack", waited)
	}
	d.mu.Lock()
	sent, waits := append([]string(nil), d.sent...), d.waits
	d.mu.Unlock()
	if waits != 0 || len(sent) != 1 || sent[0] != "session_stop" {
		t.Fatalf("dispatch = sent %v, ack waits %d; want one session_stop and no wait", sent, waits)
	}
	if got, _ := store.Get(ctx, sess.ID); got.State != StateStopping {
		t.Fatalf("console stop left the session %s, want stopping", got.State)
	}
}
