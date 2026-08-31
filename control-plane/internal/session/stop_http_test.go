package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// HTTP-level regression tests for DELETE /v1/sessions/{id}, pinned after the
// 2026-07-25 Tower triage: three "DELETE returned 404 while the session was
// running" reports turned out to be the qses harness deleting against the wrong
// stack (hermes) — the server path itself is deterministic. These tests lock
// that contract: the DELETE handler is DB-first (store.Get), so a 404 means the
// row does not exist, full stop. There is no post-launch window where a live
// session's DELETE can 404, because no in-memory registration is consulted
// before the row lookup. DB-gated like the rest (go-test-db).

func newStopServer(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, *auth.Service, *Store, *Coordinator) {
	t.Helper()
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())

	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	sh := NewHandler(coord, store)
	sh.Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, authSvc, store, coord
}

func decodeSessionResp(t *testing.T, resp *http.Response) sessionResp {
	t.Helper()
	defer resp.Body.Close()
	var body struct {
		Session sessionResp `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	return body.Session
}

// TestStopImmediatelyAfterRunning: launch through the real coordinator, drive
// the agent callbacks to `running`, and DELETE with zero delay. Must be 202 +
// stopping — never 404 — at any point after the row exists.
func TestStopImmediatelyAfterRunning(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, coord := newStopServer(t, pool)
	ctx := context.Background()

	s := seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "stopowner@test.local", "stopowner", "unrelated-pw-09")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	ownerTok := loginTok(t, authSvc, "stopowner@test.local", "unrelated-pw-09")

	res, err := coord.Launch(ctx, owner.ID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	sid := res.Session.ID

	// DELETE while still `assigned` — the earliest possible moment the client
	// knows the id (the launch response). Row exists ⇒ must not 404.
	resp := doJSON(t, http.MethodDelete, srv.URL+"/v1/sessions/"+sid, ownerTok, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE at assigned: got %d want 202", resp.StatusCode)
	}
	if got := decodeSessionResp(t, resp); got.State != string(StateStopping) {
		t.Fatalf("DELETE at assigned: state got %s want stopping", got.State)
	}

	// Second scenario: a fresh session driven to `running`, deleted immediately.
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sid, State: "stopped"})
	res, err = coord.Launch(ctx, owner.ID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	sid = res.Session.ID
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sid, State: "starting"})
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sid, State: "running"})

	resp = doJSON(t, http.MethodDelete, srv.URL+"/v1/sessions/"+sid, ownerTok, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE at running: got %d want 202", resp.StatusCode)
	}
	if got := decodeSessionResp(t, resp); got.State != string(StateStopping) {
		t.Fatalf("DELETE at running: state got %s want stopping", got.State)
	}

	// Eventual teardown: the agent confirms, the row goes terminal.
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sid, State: "stopped"})
	got, err := store.Get(ctx, sid)
	if err != nil {
		t.Fatalf("get after stopped: %v", err)
	}
	if got.State != StateStopped {
		t.Fatalf("after stopped callback: got %s want stopped", got.State)
	}
	if got.EndedAt == nil {
		t.Fatal("after stopped callback: ended_at not stamped")
	}

	// Idempotent repeat on the terminal session: 200, not 404 (control-api.md).
	resp = doJSON(t, http.MethodDelete, srv.URL+"/v1/sessions/"+sid, ownerTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE after terminal: got %d want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// 404 is reserved for a row that does not exist.
	resp = doJSON(t, http.MethodDelete, srv.URL+"/v1/sessions/00000000-0000-0000-0000-000000000000", ownerTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE absent id: got %d want 404", resp.StatusCode)
	}
	resp.Body.Close()
}
