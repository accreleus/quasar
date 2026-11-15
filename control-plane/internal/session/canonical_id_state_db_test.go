package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// #1100: the routes below take a session id from the URL, and ::uuid casts accept
// any hex case. Everything sent to the agent, kept in memory or written to the
// audit log must carry the row's own spelling.

func upperOf(t *testing.T, sid string) string {
	t.Helper()
	upper := strings.ToUpper(sid)
	if upper == sid {
		t.Fatalf("session id %s has no hex letters to recase", sid)
	}
	return upper
}

type auditedFixture struct {
	url, adminTok, sid string
	pool               *pgxpool.Pool
	coord              *Coordinator
}

func newAuditedServer(t *testing.T, disp *fakeDispatcher) auditedFixture {
	t.Helper()
	pool := testDB(t)
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, disp, testLogger())
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(coord, store, audit.NewStore(pool)).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	sid := runningSessionFor(t, pool, store)
	return auditedFixture{srv.URL, captureAdminToken(t, pool, authSvc), sid, pool, coord}
}

func TestSwapKeysStateOnCanonicalID(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	s := seed(t, pool, 4)
	sess := runningSession(t, store, s)
	target := insertApp(t, pool, "target", 512, 1)

	if _, err := coord.Swap(context.Background(), upperOf(t, sess.ID), target); err != nil {
		t.Fatalf("swap: %v", err)
	}
	waitFor(t, func() bool { return slices.Contains(disp.types(), "swap") })

	disp.mu.Lock()
	got := disp.lastSwap.SessionID
	disp.mu.Unlock()
	if got != sess.ID {
		t.Errorf("session_swap_app carries %s, want the canonical %s", got, sess.ID)
	}
	sw := coord.swapper
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if len(sw.pendingSwaps) != 1 || sw.pendingSwaps[sess.ID] != target {
		t.Errorf("pendingSwaps = %v, want only %s -> %s", sw.pendingSwaps, sess.ID, target)
	}
	if _, ok := sw.swapSince[sess.ID]; !ok || len(sw.swapSince) != 1 {
		t.Errorf("swapSince keys = %d, want only the canonical id", len(sw.swapSince))
	}
}

func TestStopUsesCanonicalIDAndKeepsGrace(t *testing.T) {
	disp := newFakeDispatcher(true)
	f := newAuditedServer(t, disp)
	sid, coord := f.sid, f.coord
	ctx := context.Background()

	if _, err := coord.Stop(ctx, upperOf(t, sid), "user_requested"); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	disp.mu.Lock()
	got := disp.lastStop.SessionID
	disp.mu.Unlock()
	if got != sid {
		t.Errorf("session_stop carries %s, want the canonical %s", got, sid)
	}
	first := stopRecord(coord)
	if _, ok := first[sid]; !ok || len(first) != 1 {
		t.Fatalf("stop records = %v, want only %s", first, sid)
	}

	if _, err := coord.Stop(ctx, sid, "user_requested"); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	second := stopRecord(coord)
	if len(second) != 1 || !second[sid].Equal(first[sid]) {
		t.Errorf("a second stop under the other spelling moved the record: %v -> %v", first, second)
	}
}

func stopRecord(c *Coordinator) map[string]time.Time {
	c.stopMu.Lock()
	defer c.stopMu.Unlock()
	out := make(map[string]time.Time, len(c.stopRequested))
	for k, v := range c.stopRequested {
		out[k] = v
	}
	return out
}

func TestCaptureArmAndAuditUseCanonicalID(t *testing.T) {
	disp := newFakeDispatcher(true)
	f := newAuditedServer(t, disp)
	sid := f.sid

	resp := doJSON(t, http.MethodPost, f.url+"/v1/admin/sessions/"+upperOf(t, sid)+"/capture", f.adminTok,
		map[string]any{"kind": "pipeline_dot"})
	var accepted struct {
		SessionID string `json:"session_id"`
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("arm capture: got %d want 202 (code=%s)", resp.StatusCode, errCode(t, resp))
	}
	_ = json.NewDecoder(resp.Body).Decode(&accepted)
	resp.Body.Close()

	disp.mu.Lock()
	got := disp.lastCapture.SessionID
	disp.mu.Unlock()
	if got != sid || accepted.SessionID != sid {
		t.Errorf("session_capture carries %s and the response %s, want the canonical %s", got, accepted.SessionID, sid)
	}
	assertAuditTarget(t, f.pool, "session.capture", sid)
}

func TestAdminStopAuditTargetsCanonicalID(t *testing.T) {
	f := newAuditedServer(t, newFakeDispatcher(true))
	sid := f.sid

	resp := doJSON(t, http.MethodDelete, f.url+"/v1/sessions/"+upperOf(t, sid), f.adminTok, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("admin stop: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()
	assertAuditTarget(t, f.pool, "session.stop", sid)
}

func assertAuditTarget(t *testing.T, pool *pgxpool.Pool, action, want string) {
	t.Helper()
	var got string
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(target_id, '') FROM admin_activity WHERE action = $1 ORDER BY id DESC LIMIT 1`, action).Scan(&got)
	if err != nil {
		t.Fatalf("read %s audit row: %v", action, err)
	}
	if got != want {
		t.Errorf("%s audit target = %s, want the canonical %s", action, got, want)
	}
}

// The ingest counters are one set per session: rejections posted under either
// spelling are read back under either.
func TestIngestCountersAreSharedAcrossSpellings(t *testing.T) {
	sid, ownerTok, adminTok, _, srvURL := adminBundleSession(t)
	upper := upperOf(t, sid)
	bad := map[string]any{"samples": []map[string]any{
		{"ts_unix_ms": nowMs() / 1000, "metrics": map[string]any{"fps": 60}}, // seconds, not ms
	}}
	for _, id := range []string{sid, upper} {
		resp := doJSON(t, http.MethodPost, srvURL+"/v1/sessions/"+id+"/stats", ownerTok, bad)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("stats POST under %s: got %d want 202", id, resp.StatusCode)
		}
		resp.Body.Close()
	}
	for _, id := range []string{sid, upper} {
		resp := doJSON(t, http.MethodGet, srvURL+"/v1/admin/sessions/"+id+"/diagnostic-bundle", adminTok, nil)
		var out struct {
			Ingest *struct {
				RejectedTs int64 `json:"rejected_ts"`
			} `json:"ingest"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if out.Ingest == nil || out.Ingest.RejectedTs != 2 {
			t.Errorf("bundle read under %s: ingest = %+v, want rejected_ts 2", id, out.Ingest)
		}
	}
}
