package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

type heldTemplatePreparation struct {
	called  chan struct{}
	release chan struct{}
}

func (p *heldTemplatePreparation) PrepareLazyTemplate(ctx context.Context, hostID, imageRef string) error {
	select {
	case p.called <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type lazyTemplateLaunch struct {
	prep      *heldTemplatePreparation
	disp      *fakeDispatcher
	coord     *Coordinator
	store     *Store
	hostID    string
	sessionID string
}

// startLazyTemplateLaunch performs an operator launch whose preparation is
// held, and returns once the coordinator is waiting on it.
func startLazyTemplateLaunch(t *testing.T) lazyTemplateLaunch {
	t.Helper()
	pool := testDB(t)
	s := seed(t, pool, 2)
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authSvc.Register(context.Background(), "lazy-template@test.local", "lazy-template", "unrelated-pw-09"); err != nil {
		t.Fatal(err)
	}
	token := loginTok(t, authSvc, "lazy-template@test.local", "unrelated-pw-09")
	var appID string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO apps(name, runtime_spec, default_encode_slots)
		VALUES('lazy-template', '{"image":"quasar-local/xfce-desktop:2026.08.08"}', 1) RETURNING id::text`).Scan(&appID))
	entitleAll(t, pool, appID)
	prep := &heldTemplatePreparation{called: make(chan struct{}, 1), release: make(chan struct{})}
	disp := newFakeDispatcher(true)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, disp, testLogger(), WithLazyTemplatePreparer(prep))
	mux := http.NewServeMux()
	ah := auth.NewHandler(authSvc)
	NewHandler(coord, store).Register(mux, ah.RequireAuth, ah.RequireAdmin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions", token, map[string]any{"app_id": appID})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/sessions = %d, want 201", resp.StatusCode)
	}
	var body struct {
		Session struct{ ID, State string } `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Session.State != "assigned" {
		t.Fatalf("state = %q, want assigned", body.Session.State)
	}
	select {
	case <-prep.called:
	case <-time.After(5 * time.Second):
		t.Fatal("preparation not requested")
	}
	if got := disp.types(); len(got) != 0 {
		t.Fatalf("assigned before image ready: %v", got)
	}
	return lazyTemplateLaunch{prep: prep, disp: disp, coord: coord, store: store, hostID: s.hostID, sessionID: body.Session.ID}
}

// assertNeverAssigned releases the held preparation and proves the ended
// session is not assigned to the agent afterwards.
func (l lazyTemplateLaunch) assertNeverAssigned(t *testing.T) {
	t.Helper()
	close(l.prep.release)
	// sawAssign fires for every acked command, stop included, so inspect the
	// recorded command types once the waiter has had time to act.
	time.Sleep(1500 * time.Millisecond)
	for _, typ := range l.disp.types() {
		if typ == "assign" || typ == "start" {
			t.Fatalf("ended session dispatched %q: %v", typ, l.disp.types())
		}
	}
}

// An operator receives the existing 201/assigned response while the selected
// template builds. Assignment cannot outrun verified image preparation.
func TestOperatorLazyTemplateLaunchWaitsBeforeAssign(t *testing.T) {
	l := startLazyTemplateLaunch(t)
	close(l.prep.release)
	select {
	case <-l.disp.sawAssign:
	case <-time.After(5 * time.Second):
		t.Fatal("no assignment after preparation")
	}
	if got := l.disp.types(); len(got) == 0 || got[0] != "assign" {
		t.Fatalf("dispatches = %v", got)
	}
}

// A control-plane restart or agent reconnect during the build reaps the
// undispatched assigned row. The waiter must not resurrect it.
func TestLazyTemplateWaitEndsOnAgentReconnect(t *testing.T) {
	l := startLazyTemplateLaunch(t)
	l.coord.AgentReconnected(context.Background(), l.hostID)
	sess, err := l.store.Get(context.Background(), l.sessionID)
	must(t, err)
	if sess.State != StateFailed {
		t.Fatalf("state after reconnect = %s, want failed", sess.State)
	}
	l.assertNeverAssigned(t)
}

// A stop during the build cancels the wait and sends no assignment.
func TestLazyTemplateWaitEndsOnStop(t *testing.T) {
	l := startLazyTemplateLaunch(t)
	if _, err := l.coord.Stop(context.Background(), l.sessionID, "user"); err != nil {
		t.Fatal(err)
	}
	l.assertNeverAssigned(t)
	sess, err := l.store.Get(context.Background(), l.sessionID)
	must(t, err)
	if sess.State == StateFailed {
		t.Fatalf("user stop during preparation was recorded as failed: %+v", sess.ErrorMessage)
	}
}
