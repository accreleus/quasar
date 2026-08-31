package storage

// Integration tests for the #175 agent-pull backing-store reaping.
// TEST_DATABASE_URL-gated (see gc_test.go helpers); reuses testDB/seedUser/etc.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedHostWithSecret inserts a host with a known node_secret (stored as
// hex(sha256(secret)), matching agentws) and returns (hostID, nodeName, secret).
func seedHostWithSecret(t *testing.T, pool *pgxpool.Pool, nodeName, secret string) string {
	t.Helper()
	h := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(h[:])
	var id string
	must(t, pool.QueryRow(context.Background(),
		`INSERT INTO hosts (node_name, status, node_secret_hash) VALUES ($1, 'online', $2) RETURNING id::text`,
		nodeName, hash).Scan(&id))
	return id
}

// setGCAfter forces a home's gc_after relative to now (e.g. -25h = past grace).
func setGCAfter(t *testing.T, pool *pgxpool.Pool, homeID, interval string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE user_homes SET gc_after = now() - `+interval+` WHERE id::text = $1`, homeID)
	must(t, err)
}

func TestAuthAgentHost(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	hostID := seedHostWithSecret(t, pool, "node-a", "supersecret")

	got, err := mgr.AuthAgentHost(ctx, "node-a", "supersecret")
	must(t, err)
	if got != hostID {
		t.Errorf("AuthAgentHost host_id = %s, want %s", got, hostID)
	}

	if _, err := mgr.AuthAgentHost(ctx, "node-a", "wrong"); err != ErrAgentAuth {
		t.Errorf("bad secret: want ErrAgentAuth, got %v", err)
	}
	if _, err := mgr.AuthAgentHost(ctx, "unknown-node", "supersecret"); err != ErrAgentAuth {
		t.Errorf("unknown node: want ErrAgentAuth, got %v", err)
	}
	if _, err := mgr.AuthAgentHost(ctx, "", ""); err != ErrAgentAuth {
		t.Errorf("empty creds: want ErrAgentAuth, got %v", err)
	}
}

func TestGCPending_ScopedToHostAndGrace(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedUser(t, pool, "p@test")
	app1 := seedApp(t, pool, "p1")
	app2 := seedApp(t, pool, "p2")
	app3 := seedApp(t, pool, "p3")
	hostA := seedHostWithSecret(t, pool, "node-a", "s")
	hostB := seedHostWithSecret(t, pool, "node-b", "s")

	// Past-grace tombstoned on host A → returned.
	hExpired := insertHome(t, pool, u, app1, hostA)
	setGCAfter(t, pool, hExpired, "interval '25 hours'")
	// Within grace on host A → not returned.
	hFresh := insertHome(t, pool, u, app2, hostA)
	setGCAfter(t, pool, hFresh, "interval '1 hour'")
	// Past-grace on host B → not returned for host A.
	hOther := insertHome(t, pool, u, app3, hostB)
	setGCAfter(t, pool, hOther, "interval '25 hours'")

	pending, err := mgr.GCPending(ctx, hostA)
	must(t, err)
	if len(pending) != 1 || pending[0].ID != hExpired {
		t.Fatalf("GCPending(hostA): want [%s], got %v", hExpired, pending)
	}
	if pending[0].Provider != "volume" {
		t.Errorf("provider = %s, want volume", pending[0].Provider)
	}
}

func TestGCPending_ExcludesNullHost(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedUser(t, pool, "nh@test")
	app := seedApp(t, pool, "nha")
	hostA := seedHostWithSecret(t, pool, "node-a", "s")
	h := insertHome(t, pool, u, app, hostA)
	setGCAfter(t, pool, h, "interval '25 hours'")
	// Orphan host (host removed) → host_id NULL; not pullable by any agent.
	_, err := pool.Exec(ctx, `UPDATE user_homes SET host_id = NULL WHERE id::text = $1`, h)
	must(t, err)

	pending, err := mgr.GCPending(ctx, hostA)
	must(t, err)
	if len(pending) != 0 {
		t.Errorf("NULL-host home pulled: %v", pending)
	}
}

func TestGCConfirm_DeletesOnlyPastGraceOnHost(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedUser(t, pool, "c@test")
	app1 := seedApp(t, pool, "c1")
	app2 := seedApp(t, pool, "c2")
	hostA := seedHostWithSecret(t, pool, "node-a", "s")
	hostB := seedHostWithSecret(t, pool, "node-b", "s")

	expired := insertHome(t, pool, u, app1, hostA)
	setGCAfter(t, pool, expired, "interval '25 hours'")
	otherHost := insertHome(t, pool, u, app2, hostB)
	setGCAfter(t, pool, otherHost, "interval '25 hours'")

	// Confirm both ids on host A: only the host-A past-grace row deletes.
	deleted, err := mgr.GCConfirm(ctx, hostA, []string{expired, otherHost})
	must(t, err)
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	var n int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes WHERE id::text = $1`, expired).Scan(&n))
	if n != 0 {
		t.Errorf("expired host-A home not deleted")
	}
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes WHERE id::text = $1`, otherHost).Scan(&n))
	if n != 1 {
		t.Errorf("host-B home wrongly deleted by host-A confirm")
	}
}

func TestGCConfirm_NoopForRevivedHome(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedUser(t, pool, "r@test")
	app := seedApp(t, pool, "ra")
	hostA := seedHostWithSecret(t, pool, "node-a", "s")

	h := insertHome(t, pool, u, app, hostA)
	setGCAfter(t, pool, h, "interval '25 hours'")
	// Revive: a launch cleared gc_after between the agent's pull and confirm.
	_, err := pool.Exec(ctx, `UPDATE user_homes SET gc_after = NULL WHERE id::text = $1`, h)
	must(t, err)

	deleted, err := mgr.GCConfirm(ctx, hostA, []string{h})
	must(t, err)
	if deleted != 0 {
		t.Errorf("revived home deleted = %d, want 0", deleted)
	}
	var n int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes WHERE id::text = $1`, h).Scan(&n))
	if n != 1 {
		t.Errorf("revived home was wrongly deleted")
	}
}

func TestGCConfirm_Empty(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	deleted, err := mgr.GCConfirm(context.Background(), "00000000-0000-0000-0000-000000000000", nil)
	must(t, err)
	if deleted != 0 {
		t.Errorf("empty confirm deleted = %d, want 0", deleted)
	}
}

func TestJanitor_LeavesPinnedReapsNullHost(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	u := seedUser(t, pool, "j@test")
	app1 := seedApp(t, pool, "j1")
	app2 := seedApp(t, pool, "j2")
	hostA := seedHostWithSecret(t, pool, "node-a", "s")

	pinned := insertHome(t, pool, u, app1, hostA)
	setGCAfter(t, pool, pinned, "interval '25 hours'")
	orphan := insertHome(t, pool, u, app2, hostA)
	setGCAfter(t, pool, orphan, "interval '25 hours'")
	_, err := pool.Exec(ctx, `UPDATE user_homes SET host_id = NULL WHERE id::text = $1`, orphan)
	must(t, err)

	// The exact janitor sweep SQL.
	_, err = pool.Exec(ctx, `DELETE FROM user_homes
		WHERE gc_after IS NOT NULL AND gc_after + interval '24 hours' < now()
		  AND host_id IS NULL`)
	must(t, err)

	var n int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes WHERE id::text = $1`, pinned).Scan(&n))
	if n != 1 {
		t.Errorf("janitor reaped a host-pinned home (should be left for agent)")
	}
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes WHERE id::text = $1`, orphan).Scan(&n))
	if n != 0 {
		t.Errorf("janitor left an unreapable NULL-host home")
	}
}

// ── HTTP handler auth (#175) ───────────────────────────────────────────────────

func newAgentMux(mgr *Manager) http.Handler {
	h := NewHandler(mgr)
	mux := http.NewServeMux()
	// Pass no-op middleware; the agent routes don't use it.
	noop := func(next http.Handler) http.Handler { return next }
	h.Register(mux, noop, noop)
	return mux
}

func TestAgentGCEndpoints_Auth(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	seedHostWithSecret(t, pool, "node-a", "supersecret")
	mux := newAgentMux(mgr)

	// No auth header → 401.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/agent/storage/gc-pending", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("no auth: want 401, got %d", rr.Code)
	}

	// Bad secret → 401.
	req := httptest.NewRequest("GET", "/v1/agent/storage/gc-pending", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	req.Header.Set("X-Quasar-Node", "node-a")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("bad secret: want 401, got %d", rr.Code)
	}

	// Unknown node → 401.
	req = httptest.NewRequest("GET", "/v1/agent/storage/gc-pending", nil)
	req.Header.Set("Authorization", "Bearer supersecret")
	req.Header.Set("X-Quasar-Node", "ghost")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unknown node: want 401, got %d", rr.Code)
	}
}

func TestAgentGCEndpoints_PendingAndConfirm(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedUser(t, pool, "e2e@test")
	app := seedApp(t, pool, "e2eapp")
	hostA := seedHostWithSecret(t, pool, "node-a", "supersecret")
	h := insertHome(t, pool, u, app, hostA)
	setGCAfter(t, pool, h, "interval '25 hours'")
	mux := newAgentMux(mgr)

	auth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer supersecret")
		r.Header.Set("X-Quasar-Node", "node-a")
	}

	// Pending lists the expired home.
	req := httptest.NewRequest("GET", "/v1/agent/storage/gc-pending", nil)
	auth(req)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("pending: want 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	var pendResp struct {
		Homes []PendingHome `json:"homes"`
	}
	must(t, json.Unmarshal(rr.Body.Bytes(), &pendResp))
	if len(pendResp.Homes) != 1 || pendResp.Homes[0].ID != h {
		t.Fatalf("pending homes = %v, want [%s]", pendResp.Homes, h)
	}

	// Confirm deletes it.
	body := strings.NewReader(`{"home_ids":["` + h + `"]}`)
	req = httptest.NewRequest("POST", "/v1/agent/storage/gc-confirm", body)
	auth(req)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm: want 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	var confResp struct {
		Deleted int `json:"deleted"`
	}
	must(t, json.Unmarshal(rr.Body.Bytes(), &confResp))
	if confResp.Deleted != 1 {
		t.Errorf("confirm deleted = %d, want 1", confResp.Deleted)
	}
	var n int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes WHERE id::text = $1`, h).Scan(&n))
	if n != 0 {
		t.Errorf("home not deleted after confirm")
	}
}
