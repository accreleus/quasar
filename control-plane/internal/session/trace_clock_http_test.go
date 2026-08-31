package session

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ST-05 HTTP-level tests for the browser clock-alignment ingest endpoint:
// POST /v1/sessions/{id}/trace/clock. Like the trace-events tests, these exercise
// the real auth middleware and run the full handler path. DB-gated (go-test-db).

func getTraceClock(t *testing.T, pool *pgxpool.Pool, sessionID string) (offset, uncertainty float64, present bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT client_offset_ms, uncertainty_ms FROM session_trace_clock WHERE session_id = $1::uuid`,
		sessionID).Scan(&offset, &uncertainty)
	if err != nil {
		return 0, 0, false
	}
	return offset, uncertainty, true
}

// TestPostTraceClockOwnerGate: owner posts → 202 + the clock row is upserted and
// GetClock returns it; a session with no post is unmeasured (GetClock → nil);
// non-owner → 403; unknown session → 404.
func TestPostTraceClockOwnerGate(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	if _, err := authSvc.Register(ctx, "other@test.local", "other", "quasar-fixture-pw-02"); err != nil {
		t.Fatalf("register other: %v", err)
	}
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	// A second session that never receives a clock post — its clock stays unmeasured.
	unmeasuredSid := sessionForUser(t, store, s, owner.ID)

	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")
	otherTok := loginTok(t, authSvc, "other@test.local", "quasar-fixture-pw-02")

	clockBody := map[string]any{"client_offset_ms": -3.2, "uncertainty_ms": 1.8}

	// Owner → 202 + the clock row upserted.
	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/clock", ownerTok, clockBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("owner POST: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()

	c, err := store.Telemetry().Clock(ctx, sid)
	if err != nil {
		t.Fatalf("get clock: %v", err)
	}
	if c == nil || c.ClientOffsetMs != -3.2 || c.UncertaintyMs != 1.8 {
		t.Fatalf("clock not persisted via POST: %+v", c)
	}

	// A second post refines in place (one row per session).
	resp = doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/clock", ownerTok,
		map[string]any{"client_offset_ms": 0.0, "uncertainty_ms": 0.5})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("refine POST: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()
	off, unc, present := getTraceClock(t, pool, sid)
	if !present || off != 0.0 || unc != 0.5 {
		t.Fatalf("clock not refined: off=%v unc=%v present=%v", off, unc, present)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_trace_clock WHERE session_id = $1::uuid`, sid).Scan(&rows); err != nil {
		t.Fatalf("count clock rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("clock rows after refine: got %d want 1", rows)
	}

	// The unmeasured session: never posted, so GetClock is nil (NOT offset 0).
	uc, err := store.Telemetry().Clock(ctx, unmeasuredSid)
	if err != nil {
		t.Fatalf("get unmeasured clock: %v", err)
	}
	if uc != nil {
		t.Fatalf("session with no clock post must be unmeasured (nil), got %+v", uc)
	}

	// Non-owner → 403 (and no row written for the non-owner's attempt).
	resp = doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/clock", otherTok, clockBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner POST: got %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown session → 404.
	resp = doJSON(t, http.MethodPost,
		srv.URL+"/v1/sessions/00000000-0000-0000-0000-000000000000/trace/clock", ownerTok, clockBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session POST: got %d want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestPostTraceClockAdminCanPost: an admin can post to any session (owner-or-admin).
func TestPostTraceClockAdminCanPost(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, _ := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email='admin@test.local'`); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	adminTok := loginTok(t, authSvc, "admin@test.local", "quasar-fixture-pw-01")

	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/clock", adminTok,
		map[string]any{"client_offset_ms": 12.5, "uncertainty_ms": 3.0})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("admin POST: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()

	c, err := store.Telemetry().Clock(ctx, sid)
	if err != nil {
		t.Fatalf("get clock: %v", err)
	}
	if c == nil || c.ClientOffsetMs != 12.5 || c.UncertaintyMs != 3.0 {
		t.Fatalf("admin POST did not persist clock: %+v", c)
	}
}

// TestPostTraceClockMalformed: missing fields, non-finite numbers, a negative
// uncertainty, and an oversize/garbage body all → 400; unauthenticated → 401.
func TestPostTraceClockMalformed(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()
	_ = seed(t, pool, 4)
	owner, _ := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")

	bad := []any{
		map[string]any{"client_offset_ms": -3.2},                        // missing uncertainty_ms
		map[string]any{"uncertainty_ms": 1.8},                           // missing client_offset_ms
		map[string]any{"client_offset_ms": 1.0, "uncertainty_ms": -1.0}, // negative uncertainty
		`{"client_offset_ms": -3.2, "uncertainty_ms":`,                  // truncated JSON
		`{"client_offset_ms": "not a number", "uncertainty_ms": 1.0}`,   // wrong type
		`not json at all`,
	}
	for i, b := range bad {
		resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/clock", ownerTok, b)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("malformed[%d]: got %d want 400", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Oversize body (> 4 KB) → 400.
	big := `{"client_offset_ms": -3.2, "uncertainty_ms": 1.8, "junk":"` +
		strings.Repeat("a", 5000) + `"}`
	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/clock", ownerTok, big)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize body: got %d want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// None of the malformed attempts wrote a row — the session stays unmeasured.
	c, err := store.Telemetry().Clock(ctx, sid)
	if err != nil {
		t.Fatalf("get clock: %v", err)
	}
	if c != nil {
		t.Fatalf("malformed posts must not persist a clock, got %+v", c)
	}

	// Unauthenticated → 401.
	resp = doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/clock", "",
		map[string]any{"client_offset_ms": 1.0, "uncertainty_ms": 1.0})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d want 401", resp.StatusCode)
	}
	resp.Body.Close()
}
