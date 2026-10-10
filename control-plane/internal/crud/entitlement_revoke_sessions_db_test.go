// Control-api.md amendment 23 (#503) over HTTP: the two routes that remove
// access end the sessions they left unentitled, with a real session coordinator
// behind the handler. Requires Postgres (TEST_DATABASE_URL); -p 1.
package crud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/session"
)

type okDispatcher struct{}

func (okDispatcher) Send(string, any) error { return nil }
func (okDispatcher) SendWithAck(context.Context, string, string, any) (agentws.AckResult, error) {
	return agentws.AckResult{OK: true}, nil
}

// newRevokerTestServer is newAuditedTestServer with stop wired as the session
// revoker; nil wires the real coordinator.
func newRevokerTestServer(t *testing.T, pool *pgxpool.Pool, stop func(context.Context, string) ([]string, error)) (*httptest.Server, string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM admin_activity`); err != nil {
		t.Fatalf("truncate admin_activity: %v", err)
	}
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	if stop == nil {
		coord := session.NewCoordinator(session.NewStore(pool), okDispatcher{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		t.Cleanup(coord.Close)
		stop = coord.StopUnentitledSessions
	}
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	crudHandler := NewHandler(pool, audit.NewStore(pool))
	crudHandler.SetSessionRevoker(stop)
	crudHandler.Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, adminToken(t, pool, authSvc)
}

type revokeFixture struct{ hostID string }

func newRevokeFixture(t *testing.T, pool *pgxpool.Pool) revokeFixture {
	t.Helper()
	var f revokeFixture
	must43(t, pool.QueryRow(context.Background(), `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('host-1','online','ok') RETURNING id::text`).Scan(&f.hostID))
	return f
}

func (f revokeFixture) user(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	must43(t, pool.QueryRow(context.Background(), `INSERT INTO users (email, username, password_hash)
		VALUES ($1 || '@test.local', $1, 'x') RETURNING id::text`, name).Scan(&id))
	return id
}

func (f revokeFixture) running(t *testing.T, pool *pgxpool.Pool, userID, appID string) string {
	t.Helper()
	var id string
	must43(t, pool.QueryRow(context.Background(), `
		INSERT INTO sessions (user_id, app_id, host_id, state, width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'running', 1280, 720, 60, 6000)
		RETURNING id::text`, userID, appID, f.hostID).Scan(&id))
	return id
}

func stateOf(t *testing.T, pool *pgxpool.Pool, sessionID string) string {
	t.Helper()
	var state string
	must43(t, pool.QueryRow(context.Background(),
		`SELECT state FROM sessions WHERE id = $1::uuid`, sessionID).Scan(&state))
	return state
}

func grant(t *testing.T, pool *pgxpool.Pool, appID string, userID *string) string {
	t.Helper()
	subjectType := "all"
	if userID != nil {
		subjectType = "user"
	}
	var id string
	must43(t, pool.QueryRow(context.Background(), `
		INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ($1, $2::uuid, $3::uuid, 'admin') RETURNING id::text`, subjectType, userID, appID).Scan(&id))
	return id
}

func TestRevokeRouteStopsTheSessionsItLeftUnentitled(t *testing.T) {
	pool := testDB(t)
	srv, tok := newRevokerTestServer(t, pool, nil)
	ctx := context.Background()
	f := newRevokeFixture(t, pool)

	var appID string
	must43(t, pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('app') RETURNING id::text`).Scan(&appID))
	allRow := grant(t, pool, appID, nil)
	uncovered, covered := f.user(t, pool, "uncovered"), f.user(t, pool, "covered")
	personalRow := grant(t, pool, appID, &covered)
	lost := f.running(t, pool, uncovered, appID)
	kept := f.running(t, pool, covered, appID)

	url := srv.URL + "/v1/admin/apps/" + appID + "/entitlements/"
	if resp := deleteReq(t, url+allRow, tok); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke the all row: got %d, want 204", resp.StatusCode)
	}
	if got := stateOf(t, pool, lost); got != "stopping" {
		t.Errorf("uncovered owner's session after the response: %s, want stopping", got)
	}
	if got := stateOf(t, pool, kept); got != "running" {
		t.Errorf("session of a user who keeps a personal row: %s, want running", got)
	}
	details, _ := auditDetails(t, pool, "app.entitlement.revoke")
	if details["sessions_stopped"] != float64(1) {
		t.Errorf("audit sessions_stopped = %v, want 1", details["sessions_stopped"])
	}
	if ids, _ := details["stopped_session_ids"].([]any); len(ids) != 1 || ids[0] != lost {
		t.Errorf("audit stopped_session_ids = %v, want [%s]", details["stopped_session_ids"], lost)
	}

	// Their own row goes too: now that user's session ends, and only theirs.
	if resp := deleteReq(t, url+personalRow, tok); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke the personal row: got %d, want 204", resp.StatusCode)
	}
	if got := stateOf(t, pool, kept); got != "stopping" {
		t.Errorf("session after its owner's personal row was revoked: %s, want stopping", got)
	}
}

func TestEntitlementModeStopsDerivedTileSessions(t *testing.T) {
	pool := testDB(t)
	srv, tok := newRevokerTestServer(t, pool, nil)
	ctx := context.Background()
	f := newRevokeFixture(t, pool)

	var parent, tile string
	must43(t, pool.QueryRow(ctx, `INSERT INTO apps (name, kind, library_provider)
		VALUES ('Steam', 'launcher', 'steam') RETURNING id::text`).Scan(&parent))
	must43(t, pool.QueryRow(ctx, `INSERT INTO apps (name, kind, parent_app_id, external_source, external_id, origin)
		VALUES ('Redout', 'game', $1::uuid, 'steam', '517710', 'discovered') RETURNING id::text`, parent).Scan(&tile))
	grant(t, pool, parent, nil)
	player := f.user(t, pool, "player")
	grant(t, pool, tile, &player)
	onTile := f.running(t, pool, player, tile)

	url := srv.URL + "/v1/admin/library-providers/steam/entitlement-mode"
	if resp, body := post(t, url, map[string]any{"mode": "all"}, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("mode all: got %d (%v)", resp.StatusCode, body)
	}
	if got := stateOf(t, pool, onTile); got != "running" {
		t.Fatalf("tile session after mode all: %s, want running", got)
	}

	if resp, body := post(t, url, map[string]any{"mode": "user"}, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("mode user: got %d (%v)", resp.StatusCode, body)
	}
	if got := stateOf(t, pool, onTile); got != "stopping" {
		t.Errorf("tile session after its parent was restricted: %s, want stopping", got)
	}
	details, _ := auditDetails(t, pool, "app.entitlement.set_mode")
	if details["sessions_stopped"] != float64(1) {
		t.Errorf("audit sessions_stopped = %v, want 1", details["sessions_stopped"])
	}
}

type sendFailsDispatcher struct{ okDispatcher }

func (sendFailsDispatcher) Send(string, any) error { return errors.New("send queue full") }

// A session_stop that never reached the agent is still a stopped session (the
// row is `stopping`, the heartbeat re-sends), and the activity row says the stop
// was not clean.
func TestRevokeFlagsAStopThatDidNotReachTheAgent(t *testing.T) {
	pool := testDB(t)
	coord := session.NewCoordinator(session.NewStore(pool), sendFailsDispatcher{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(coord.Close)
	srv, tok := newRevokerTestServer(t, pool, coord.StopUnentitledSessions)
	f := newRevokeFixture(t, pool)

	var appID string
	must43(t, pool.QueryRow(context.Background(), `INSERT INTO apps (name) VALUES ('app') RETURNING id::text`).Scan(&appID))
	row := grant(t, pool, appID, nil)
	sid := f.running(t, pool, f.user(t, pool, "player"), appID)

	if resp := deleteReq(t, srv.URL+"/v1/admin/apps/"+appID+"/entitlements/"+row, tok); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: got %d, want 204", resp.StatusCode)
	}
	if got := stateOf(t, pool, sid); got != "stopping" {
		t.Errorf("session whose stop was not delivered: %s, want stopping", got)
	}
	details, _ := auditDetails(t, pool, "app.entitlement.revoke")
	if details["sessions_stopped"] != float64(1) || details["sessions_stop_failed"] != true {
		t.Errorf("audit details = %v, want sessions_stopped 1 and sessions_stop_failed", details)
	}
}

// A failed stop is recorded, not surfaced: a retried DELETE would be a 404 that
// sweeps nothing, and the periodic sweep finishes the job. A long stopped list
// must stay inside the audit row's 4096-byte CHECK.
func TestRevokeRecordsAFailedStopAndBoundsTheAuditList(t *testing.T) {
	pool := testDB(t)
	stopped := make([]string, 60)
	for i := range stopped {
		stopped[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	srv, tok := newRevokerTestServer(t, pool, func(context.Context, string) ([]string, error) {
		return stopped, errors.New("one session would not stop")
	})

	var appID string
	must43(t, pool.QueryRow(context.Background(), `INSERT INTO apps (name) VALUES ('app') RETURNING id::text`).Scan(&appID))
	row := grant(t, pool, appID, nil)

	resp := deleteReq(t, srv.URL+"/v1/admin/apps/"+appID+"/entitlements/"+row, tok)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke with a failed stop: got %d, want 204", resp.StatusCode)
	}
	details, _ := auditDetails(t, pool, "app.entitlement.revoke") // fails over 4096 bytes
	if details["sessions_stopped"] != float64(60) || details["sessions_stop_failed"] != true {
		t.Errorf("audit details = %v, want sessions_stopped 60 and sessions_stop_failed", details)
	}
	if ids, _ := details["stopped_session_ids"].([]any); len(ids) != maxAuditedStoppedSessions {
		t.Errorf("audit lists %d ids, want the %d-id bound", len(ids), maxAuditedStoppedSessions)
	}
}
