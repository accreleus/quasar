package session

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// P4-05 HTTP-level tests for the telemetry endpoints: the owner-or-admin gate on
// the browser POST, the body limits, and the admin-only gate on the read. These
// exercise the real auth middleware (RequireAuth/RequireAdmin), so they run the
// full handler path. DB-gated like the rest (go-test-db).

func newMetricsServer(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, *auth.Service, *Store) {
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
	return srv, authSvc, store
}

func loginTok(t *testing.T, svc *auth.Service, email, pass string) string {
	t.Helper()
	tok, err := svc.Login(context.Background(), email, pass, "")
	if err != nil {
		t.Fatalf("login %s: %v", email, err)
	}
	return tok.Plaintext
}

func doJSON(t *testing.T, method, url, bearer string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
		rdr = nil
	case string:
		rdr = strings.NewReader(b)
	default:
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(b)
		rdr = &buf
	}
	req, _ := http.NewRequest(method, url, rdr)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// sessionForUser creates a running session owned by the given user id.
func sessionForUser(t *testing.T, store *Store, s seedIDs, userID string) string {
	t.Helper()
	ctx := context.Background()
	p := launchParams(s)
	p.UserID = userID
	sess, err := store.ScheduleAndCreate(ctx, p)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, _ = store.Transition(ctx, sess.ID, StateStarting, nil, nil)
	_, _ = store.Transition(ctx, sess.ID, StateRunning, nil, nil)
	return sess.ID
}

func TestPostStatsOwnerGate(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()

	// Two users; seed() created one user already (unused here — we register fresh).
	_ = seed(t, pool, 4) // host + gpu + app
	owner, err := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	if _, err := authSvc.Register(ctx, "other@test.local", "other", "quasar-fixture-pw-02"); err != nil {
		t.Fatalf("register other: %v", err)
	}
	// Re-fetch seed ids (the app/host/gpu) for ScheduleAndCreate.
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)

	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")
	otherTok := loginTok(t, authSvc, "other@test.local", "quasar-fixture-pw-02")

	statsBody := map[string]any{"samples": []map[string]any{
		{"ts_unix_ms": nowMs(), "metrics": map[string]any{
			"fps": 59.6, "rtt_ms": 12, "evil_key": "drop", "glass_to_glass_ms": 71,
			"rvfc_capture_time_available": 1, "abs_capture_time_negotiated": 0,
		}},
	}}

	// Owner → 202 + rows written; unknown key dropped.
	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/stats", ownerTok, statsBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("owner POST: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()
	if n := countMetrics(t, pool, sid); n != 1 {
		t.Fatalf("owner POST rows: got %d want 1", n)
	}
	samples, _, _ := store.Telemetry().Recent(ctx, sid, 10, nil, "")
	var m map[string]any
	_ = json.Unmarshal(samples[0].Metrics, &m)
	if _, ok := m["evil_key"]; ok {
		t.Fatalf("unknown key stored: %v", m)
	}
	if m["fps"] == nil || m["glass_to_glass_ms"] == nil ||
		m["rvfc_capture_time_available"] == nil || m["abs_capture_time_negotiated"] == nil {
		t.Fatalf("dictionary keys missing: %v", m)
	}

	// Non-owner → 403.
	resp = doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/stats", otherTok, statsBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner POST: got %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown session → 404.
	resp = doJSON(t, http.MethodPost,
		srv.URL+"/v1/sessions/00000000-0000-0000-0000-000000000000/stats", ownerTok, statsBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown POST: got %d want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestPostStatsNativeSource covers P9-07: the optional "client" discriminator
// maps to session_metrics.source ('native'), the existing default ('browser') is
// unaffected, and an unknown value is rejected outright (never coerced).
func TestPostStatsNativeSource(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()
	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")

	// client: "native" → source='native'.
	nativeBody := map[string]any{
		"client": "native",
		"samples": []map[string]any{
			{"ts_unix_ms": nowMs(), "metrics": map[string]any{"fps": 60, "glass_to_glass_ms": 45}},
		},
	}
	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/stats", ownerTok, nativeBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("native POST: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()

	// client omitted → source='browser' (unchanged default).
	browserBody := map[string]any{
		"samples": []map[string]any{
			{"ts_unix_ms": nowMs() + 1000, "metrics": map[string]any{"fps": 30}},
		},
	}
	resp = doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/stats", ownerTok, browserBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("default POST: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()

	samples, _, err := store.Telemetry().Recent(ctx, sid, 10, nil, "")
	if err != nil {
		t.Fatalf("recent metrics: %v", err)
	}
	var gotNative, gotBrowser bool
	for _, s := range samples {
		switch s.Source {
		case telemetry.SourceNative:
			gotNative = true
		case telemetry.SourceBrowser:
			gotBrowser = true
		}
	}
	if !gotNative || !gotBrowser {
		t.Fatalf("expected one native and one browser row, got %+v", samples)
	}

	// unknown client value → 400, not silently coerced, no row written.
	before := countMetrics(t, pool, sid)
	badBody := map[string]any{
		"client":  "totally-not-a-client",
		"samples": []map[string]any{{"ts_unix_ms": nowMs() + 2000, "metrics": map[string]any{"fps": 60}}},
	}
	resp = doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/stats", ownerTok, badBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown client POST: got %d want 400", resp.StatusCode)
	}
	resp.Body.Close()
	if after := countMetrics(t, pool, sid); after != before {
		t.Fatalf("unknown client should not write a row: before=%d after=%d", before, after)
	}

	// admin read: ?source=native returns only the native row.
	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email='admin@test.local'`); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	adminTok := loginTok(t, authSvc, "admin@test.local", "quasar-fixture-pw-01")
	resp = doJSON(t, http.MethodGet, srv.URL+"/v1/admin/sessions/"+sid+"/metrics?source=native", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin read source=native: got %d want 200", resp.StatusCode)
	}
	var out struct {
		Items []struct {
			Source string `json:"source"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Items) != 1 || out.Items[0].Source != telemetry.SourceNative {
		t.Fatalf("admin read source=native: got %+v", out.Items)
	}
}

func TestPostStatsLimits(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()
	_ = seed(t, pool, 4)
	owner, _ := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")

	// > 64 samples → 400.
	many := make([]map[string]any, 65)
	for i := range many {
		many[i] = map[string]any{"ts_unix_ms": nowMs() + int64(i), "metrics": map[string]any{"fps": 60}}
	}
	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/stats", ownerTok,
		map[string]any{"samples": many})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf(">64 samples: got %d want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Oversize body (> 32 KB) → 400. Build a big garbage-padded but valid-prefixed body.
	big := "{\"samples\":[{\"ts_unix_ms\":1,\"metrics\":{\"fps\":" +
		strings.Repeat("9", 40000) + "}}]}"
	resp = doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/stats", ownerTok, big)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize body: got %d want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminMetricsReadGate(t *testing.T) {
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
	_ = store.Telemetry().Append(ctx, sid, telemetry.SourceAgent, telemetry.SampleInput{TsUnixMs: 100, Metrics: json.RawMessage(`{"fps":60}`)})
	_ = store.Telemetry().Append(ctx, sid, telemetry.SourceBrowser, telemetry.SampleInput{TsUnixMs: 200, Metrics: json.RawMessage(`{"rtt_ms":12}`)})

	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")
	adminTok := loginTok(t, authSvc, "admin@test.local", "quasar-fixture-pw-01")

	// Non-admin (the owner!) → 403.
	resp := doJSON(t, http.MethodGet, srv.URL+"/v1/admin/sessions/"+sid+"/metrics", ownerTok, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin read: got %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Admin → 200 with both sources, newest first.
	resp = doJSON(t, http.MethodGet, srv.URL+"/v1/admin/sessions/"+sid+"/metrics", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin read: got %d want 200", resp.StatusCode)
	}
	var out struct {
		Items []struct {
			Source   string `json:"source"`
			TsUnixMs int64  `json:"ts_unix_ms"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Items) != 2 {
		t.Fatalf("admin read items: got %d want 2", len(out.Items))
	}
	if out.Items[0].TsUnixMs != 200 {
		t.Fatalf("admin read not newest-first: %d", out.Items[0].TsUnixMs)
	}

	// Admin, unknown session → 404.
	resp = doJSON(t, http.MethodGet,
		srv.URL+"/v1/admin/sessions/00000000-0000-0000-0000-000000000000/metrics", adminTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("admin read unknown: got %d want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// currentSeed re-reads the single seeded app/host/gpu ids from the DB so an HTTP
// test (which registers its own users) can still call ScheduleAndCreate.
func currentSeed(t *testing.T, pool *pgxpool.Pool) seedIDs {
	t.Helper()
	ctx := context.Background()
	var s seedIDs
	if err := pool.QueryRow(ctx, `SELECT id::text FROM apps LIMIT 1`).Scan(&s.appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id::text FROM hosts LIMIT 1`).Scan(&s.hostID); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id::text FROM gpus LIMIT 1`).Scan(&s.gpuID); err != nil {
		t.Fatalf("seed gpu: %v", err)
	}
	return s
}
