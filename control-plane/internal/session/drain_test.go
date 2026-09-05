package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// nonexistentHostID is a well-formed UUID that no seeded host uses.
const nonexistentHostID = "00000000-0000-0000-0000-0000000000ff"

// P3-03 host drain/cordon lifecycle: these prove online→draining (graceful +
// force), the stable-draining semantics, draining→online (uncordon), idempotency,
// the offline-host conflicts, and that a force-drain stops the host's sessions and
// that draining stops new placement. Integration tests — need Postgres.

func hostStatus(t *testing.T, pool *pgxpool.Pool, hostID string) string {
	t.Helper()
	var st string
	must(t, pool.QueryRow(context.Background(),
		`SELECT status FROM hosts WHERE id::text = $1`, hostID).Scan(&st))
	return st
}

func setHostStatusRaw(t *testing.T, pool *pgxpool.Pool, hostID, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET status = $2 WHERE id::text = $1`, hostID, status); err != nil {
		t.Fatalf("set host status: %v", err)
	}
}

func newCoord(t *testing.T, pool *pgxpool.Pool) (*Store, *Coordinator, *fakeDispatcher) {
	t.Helper()
	store := NewStore(pool)
	disp := newFakeDispatcher(true)
	return store, newTestCoordinator(t, store, disp, testLogger()), disp
}

// fakeAgents is the AgentConnectivity seam with a fixed answer.
type fakeAgents bool

func (f fakeAgents) IsConnected(string) bool { return bool(f) }

func newCoordWithAgents(t *testing.T, pool *pgxpool.Pool, connected bool) (*Store, *Coordinator) {
	t.Helper()
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(),
		WithAgentConnectivity(fakeAgents(connected)))
	return store, coord
}

// TestDrainOnlineHostGraceful: online → draining, sessions untouched (graceful).
func TestDrainOnlineHostGraceful(t *testing.T) {
	pool := testDB(t)
	store, coord, disp := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	h, err := coord.DrainHost(ctx, s.hostID, false)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if h.Status != "draining" || hostStatus(t, pool, s.hostID) != "draining" {
		t.Fatalf("status: got %q want draining", hostStatus(t, pool, s.hostID))
	}
	// Graceful: the running session was NOT stopped.
	if got, _ := store.Get(ctx, sess.ID); got.State != StateAssigned {
		t.Fatalf("graceful drain stopped a session: state=%s", got.State)
	}
	if n := len(disp.types()); n != 0 {
		t.Fatalf("graceful drain dispatched %d commands, want 0", n)
	}
}

// TestDrainStable: a drained host stays draining (does not auto-flip to offline)
// even after its sessions end.
func TestDrainStable(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := coord.DrainHost(ctx, s.hostID, false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// End the session; the host must remain draining (not auto-offline).
	if _, err := store.Transition(ctx, sess.ID, StateStopped, nil, nil); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	if st := hostStatus(t, pool, s.hostID); st != "draining" {
		t.Fatalf("host status after last session ended: got %q want draining (stable)", st)
	}
}

// TestForceDrainStopsSessions: force-drain cordons the host AND stops every
// non-terminal session on it (session_stop dispatched, sessions → stopping).
func TestForceDrainStopsSessions(t *testing.T) {
	pool := testDB(t)
	store, coord, disp := newCoord(t, pool)
	s := seed(t, pool, 4)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	var ids []string
	for i := 0; i < 2; i++ {
		sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
		if err != nil {
			t.Fatalf("seed session %d: %v", i, err)
		}
		ids = append(ids, sess.ID)
	}

	h, err := coord.DrainHost(ctx, s.hostID, true)
	if err != nil {
		t.Fatalf("force drain: %v", err)
	}
	if h.Status != "draining" {
		t.Fatalf("status: got %q want draining", h.Status)
	}
	for _, id := range ids {
		got, _ := store.Get(ctx, id)
		if got.State != StateStopping {
			t.Fatalf("session %s: got %s want stopping (force-drain should stop it)", id, got.State)
		}
	}
	stops := 0
	for _, ty := range disp.types() {
		if ty == "stop" {
			stops++
		}
	}
	if stops != 2 {
		t.Fatalf("session_stop dispatched %d times, want 2", stops)
	}
}

// TestDrainStopsPlacement: a drained host is no longer a placement candidate.
func TestDrainStopsPlacement(t *testing.T) {
	pool := testDB(t)
	store, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	if _, err := coord.DrainHost(ctx, s.hostID, false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// The only host is draining ⇒ no online host ⇒ no_host_available.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("launch onto drained-only fleet: got %v want ErrNoHostAvailable", err)
	}
	if n := sessionsOnHost(t, pool, s.hostID); n != 0 {
		t.Fatalf("placed %d session(s) on the draining host", n)
	}
}

// TestDrainIdempotent: draining a draining host is a no-op success.
func TestDrainIdempotent(t *testing.T) {
	pool := testDB(t)
	_, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	if _, err := coord.DrainHost(ctx, s.hostID, false); err != nil {
		t.Fatalf("drain 1: %v", err)
	}
	h, err := coord.DrainHost(ctx, s.hostID, false)
	if err != nil {
		t.Fatalf("drain 2 (idempotent): %v", err)
	}
	if h.Status != "draining" {
		t.Fatalf("status: got %q want draining", h.Status)
	}
}

// TestDrainOfflineConflict: draining an offline host is a 409 conflict.
func TestDrainOfflineConflict(t *testing.T) {
	pool := testDB(t)
	_, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	setHostStatusRaw(t, pool, s.hostID, "offline")
	ctx := context.Background()

	if _, err := coord.DrainHost(ctx, s.hostID, false); !errors.Is(err, ErrHostNotDrainable) {
		t.Fatalf("drain offline host: got %v want ErrHostNotDrainable", err)
	}
}

// TestDrainNotFound: draining an unknown host is a 404.
func TestDrainNotFound(t *testing.T) {
	pool := testDB(t)
	_, coord, _ := newCoord(t, pool)
	ctx := context.Background()
	if _, err := coord.DrainHost(ctx, nonexistentHostID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drain unknown host: got %v want ErrNotFound", err)
	}
}

// TestUncordonDrainingHost: draining → online with no connectivity seam wired
// (the seam is optional; unwired trusts the status column).
func TestUncordonDrainingHost(t *testing.T) {
	pool := testDB(t)
	_, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	if _, err := coord.DrainHost(ctx, s.hostID, false); err != nil {
		t.Fatalf("drain: %v", err)
	}
	h, err := coord.UncordonHost(ctx, s.hostID)
	if err != nil {
		t.Fatalf("uncordon: %v", err)
	}
	if h.Status != "online" || hostStatus(t, pool, s.hostID) != "online" {
		t.Fatalf("status: got %q want online", hostStatus(t, pool, s.hostID))
	}
}

// TestUncordonOnlineIdempotent: uncordoning an online host is a no-op success.
func TestUncordonOnlineIdempotent(t *testing.T) {
	pool := testDB(t)
	_, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	h, err := coord.UncordonHost(ctx, s.hostID)
	if err != nil {
		t.Fatalf("uncordon online host: %v", err)
	}
	if h.Status != "online" {
		t.Fatalf("status: got %q want online", h.Status)
	}
}

// TestUncordonOfflineConflict: uncordoning an offline host is a 409 conflict (its
// agent is not connected; it returns online on its own when the agent reconnects).
func TestUncordonOfflineConflict(t *testing.T) {
	pool := testDB(t)
	_, coord, _ := newCoord(t, pool)
	s := seed(t, pool, 4)
	setHostStatusRaw(t, pool, s.hostID, "offline")
	ctx := context.Background()

	if _, err := coord.UncordonHost(ctx, s.hostID); !errors.Is(err, ErrHostNotResumable) {
		t.Fatalf("uncordon offline host: got %v want ErrHostNotResumable", err)
	}
	// The connectivity seam does not soften the offline conflict, even claiming
	// the agent is connected.
	_, wired := newCoordWithAgents(t, pool, true)
	if _, err := wired.UncordonHost(ctx, s.hostID); !errors.Is(err, ErrHostNotResumable) {
		t.Fatalf("uncordon offline host (agent connected): got %v want ErrHostNotResumable", err)
	}
	if st := hostStatus(t, pool, s.hostID); st != "offline" {
		t.Fatalf("status after refused uncordon: got %q want offline", st)
	}
}

// TestUncordonDrainingAgentConnected: the live check passes ⇒ draining → online.
func TestUncordonDrainingAgentConnected(t *testing.T) {
	pool := testDB(t)
	_, coord := newCoordWithAgents(t, pool, true)
	s := seed(t, pool, 4)
	ctx := context.Background()

	setHostStatusRaw(t, pool, s.hostID, "draining")
	h, err := coord.UncordonHost(ctx, s.hostID)
	if err != nil {
		t.Fatalf("uncordon: %v", err)
	}
	if h.Status != "online" || hostStatus(t, pool, s.hostID) != "online" {
		t.Fatalf("status: got %q/%q want online", h.Status, hostStatus(t, pool, s.hostID))
	}
}

// TestUncordonDrainingAgentDisconnected (#11): the cordon lifts but the host is
// not schedulable, so the row goes offline rather than online.
func TestUncordonDrainingAgentDisconnected(t *testing.T) {
	pool := testDB(t)
	store, coord := newCoordWithAgents(t, pool, false)
	s := seed(t, pool, 4)
	ctx := context.Background()

	setHostStatusRaw(t, pool, s.hostID, "draining")
	h, err := coord.UncordonHost(ctx, s.hostID)
	if err != nil {
		t.Fatalf("uncordon: %v", err)
	}
	if h.Status != "offline" || hostStatus(t, pool, s.hostID) != "offline" {
		t.Fatalf("status: got %q/%q want offline", h.Status, hostStatus(t, pool, s.hostID))
	}
	// The whole point: the scheduler must not pick it up.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("launch after uncordon with no agent: got %v want ErrNoHostAvailable", err)
	}
}

// TestUncordonHTTPAgentDisconnected: the 200 body carries the offline status, so
// an admin sees what the uncordon actually produced.
func TestUncordonHTTPAgentDisconnected(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	store, coord := newCoordWithAgents(t, pool, false)

	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(coord, store).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if _, err := authSvc.Register(ctx, "drainadmin@test.local", "drainadmin", "unrelated-pw-16"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email='drainadmin@test.local'`); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	tok := loginTok(t, authSvc, "drainadmin@test.local", "unrelated-pw-16")

	s := seed(t, pool, 4)
	setHostStatusRaw(t, pool, s.hostID, "draining")

	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/hosts/"+s.hostID+"/uncordon", tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST uncordon: got %d want 200 (code=%s)", resp.StatusCode, errCode(t, resp))
	}
	defer resp.Body.Close()
	var body struct {
		Host struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"host"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode host body: %v", err)
	}
	if body.Host.ID != s.hostID || body.Host.Status != "offline" {
		t.Fatalf("host body: got %+v want id=%s status=offline", body.Host, s.hostID)
	}
}
