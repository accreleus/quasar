package storage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHomeClaimsDiagnosisIncludesClaimOnlyAndBindsCursor(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	u := seedUser(t, pool, "claim-admin@test.local")
	a1 := seedApp(t, pool, "Claim A")
	a2 := seedApp(t, pool, "Claim B")
	h := seedHost(t, pool)
	_, err := pool.Exec(ctx, `INSERT INTO managed_home_claims (user_id,canonical_app_id,host_id,state) VALUES ($1::uuid,$2::uuid,$4::uuid,'reserved'),($1::uuid,$3::uuid,NULL,'conflict')`, u, a1, a2, h)
	// The second row's reason is deliberately required by 0090's invariant.
	if err == nil {
		t.Fatal("conflict claim without reason accepted")
	}
	_, err = pool.Exec(ctx, `INSERT INTO managed_home_claims (user_id,canonical_app_id,host_id,state,conflict_reason) VALUES ($1::uuid,$2::uuid,$4::uuid,'reserved',NULL),($1::uuid,$3::uuid,NULL,'conflict','legacy_location_uncertain')`, u, a1, a2, h)
	must(t, err)
	mgr := NewLocal(pool, t.TempDir())
	first, cursor, err := mgr.ListHomeClaims(ctx, ListHomeClaimsOpts{UserID: u, Limit: 1})
	must(t, err)
	if len(first) != 1 || cursor == "" {
		t.Fatalf("first page = %+v, cursor=%q", first, cursor)
	}
	second, next, err := mgr.ListHomeClaims(ctx, ListHomeClaimsOpts{UserID: u, Limit: 1, Cursor: cursor})
	must(t, err)
	if len(second) != 1 || next != "" || first[0].CanonicalAppID == second[0].CanonicalAppID {
		t.Fatalf("second page = %+v, next=%q", second, next)
	}
	if _, _, err := mgr.ListHomeClaims(ctx, ListHomeClaimsOpts{UserID: u, HostID: h, Limit: 1, Cursor: cursor}); !errors.Is(err, ErrInvalidHomeClaimFilter) {
		t.Fatalf("cursor reused with different host filter: %v", err)
	}
	byHost, _, err := mgr.ListHomeClaims(ctx, ListHomeClaimsOpts{HostID: h})
	must(t, err)
	if len(byHost) != 1 || byHost[0].CanonicalAppID != a1 || len(byHost[0].RecordedHostIDs) != 0 {
		t.Fatalf("claim-only host filter = %+v", byHost)
	}
	for _, bad := range []ListHomeClaimsOpts{{UserID: "bad"}, {State: "unknown"}, {Cursor: "not-a-cursor"}, {Limit: 101}} {
		if _, _, err := mgr.ListHomeClaims(ctx, bad); !errors.Is(err, ErrInvalidHomeClaimFilter) {
			t.Fatalf("bad options %+v: %v", bad, err)
		}
	}
}

func TestHomeClaimsAdminEndpointKeepsLegacyRowsUnchanged(t *testing.T) {
	pool := testDB(t)
	u := seedUser(t, pool, "claim-http@test.local")
	a := seedApp(t, pool, "Claim App")
	host := seedHost(t, pool)
	_, err := pool.Exec(context.Background(), `INSERT INTO managed_home_claims (user_id,canonical_app_id,host_id,state) VALUES ($1::uuid,$2::uuid,$3::uuid,'reserved')`, u, a, host)
	must(t, err)
	mgr := NewLocal(pool, t.TempDir())
	handler := NewHandler(mgr)
	mux := http.NewServeMux()
	// Unit route wiring supplies a deterministic admin gate. Auth middleware
	// enforcement is separately covered by the storage route auth suite.
	handler.Register(mux, func(h http.Handler) http.Handler { return h }, func(h http.Handler) http.Handler { return h })
	for path, want := range map[string]int{
		"/v1/admin/storage/home-claims":           http.StatusOK,
		"/v1/admin/storage/home-claims?limit=0":   http.StatusBadRequest,
		"/v1/admin/storage/home-claims?limit=":    http.StatusBadRequest,
		"/v1/admin/storage/home-claims?state=bad": http.StatusBadRequest,
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != want {
			t.Fatalf("%s = %d: %s, want %d", path, rr.Code, rr.Body.String(), want)
		}
		if want == http.StatusOK {
			var body struct {
				Items      []HomeClaim `json:"items"`
				NextCursor *string     `json:"next_cursor"`
			}
			must(t, json.Unmarshal(rr.Body.Bytes(), &body))
			if len(body.Items) != 1 || body.Items[0].State != "reserved" || body.NextCursor != nil || strings.Contains(rr.Body.String(), "ref") {
				t.Fatalf("claim-only response = %s", rr.Body.String())
			}
		}
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/admin/storage/homes", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"items":[]`) {
		t.Fatalf("legacy homes changed: %d %s", rr.Code, rr.Body.String())
	}
}

func TestHomeTombstoneAndExactGCConfirmReleaseClaim(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	u := seedUser(t, pool, "claim-gc@test.local")
	a := seedApp(t, pool, "Claim GC")
	_, err := pool.Exec(ctx, `UPDATE apps SET managed_home=true,home_container_path='/home/quasar' WHERE id=$1::uuid`, a)
	must(t, err)
	h := seedHost(t, pool)
	homeID := insertHome(t, pool, u, a, h)
	_, err = pool.Exec(ctx, `INSERT INTO managed_home_claims (user_id,canonical_app_id,host_id,state,materialized_at) VALUES ($1::uuid,$2::uuid,$3::uuid,'materialized',now())`, u, a, h)
	must(t, err)
	mgr := NewLocal(pool, t.TempDir())
	_, err = mgr.TombstoneHome(ctx, homeID)
	must(t, err)
	var state, reason string
	var historical bool
	must(t, pool.QueryRow(ctx, `SELECT state,conflict_reason,materialized_at IS NOT NULL FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, u, a).Scan(&state, &reason, &historical))
	if state != "conflict" || reason != "gc_pending" || !historical {
		t.Fatalf("tombstone claim = (%s,%s,historical=%t)", state, reason, historical)
	}
	if _, err := mgr.EnsureHome(ctx, u, a, h, "/home/quasar"); !errors.Is(err, ErrHomeConflict) {
		t.Fatalf("EnsureHome revived tombstone: %v", err)
	}
	// The agent has no authority before grace, even when it names the exact ID.
	deleted, err := mgr.GCConfirm(ctx, h, []string{homeID})
	must(t, err)
	if deleted != 0 {
		t.Fatalf("early gc-confirm deleted %d", deleted)
	}
	setGCAfter(t, pool, homeID, "interval '25 hours'")
	deleted, err = mgr.GCConfirm(ctx, h, []string{homeID})
	must(t, err)
	if deleted != 1 {
		t.Fatalf("exact gc-confirm deleted %d, want 1", deleted)
	}
	var claims int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, u, a).Scan(&claims))
	if claims != 0 {
		t.Fatalf("reaped unique claim remains: %d", claims)
	}
}

func TestPresetOnlyManagedHomeTombstoneAndGC(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	u := seedUser(t, pool, "preset-claim-gc@test.local")
	a := seedApp(t, pool, "Preset Claim GC")
	var presetID string
	must(t, pool.QueryRow(ctx, `INSERT INTO runtime_presets (name,managed_home,home_container_path)
		VALUES ('preset-claim-gc',true,'/alternate/home') RETURNING id::text`).Scan(&presetID))
	_, err := pool.Exec(ctx, `UPDATE apps SET runtime_preset_id=$2::uuid WHERE id=$1::uuid`, a, presetID)
	must(t, err)
	h := seedHost(t, pool)
	homeID := insertHome(t, pool, u, a, h)
	mgr := NewLocal(pool, t.TempDir())
	_, err = mgr.TombstoneHome(ctx, homeID)
	must(t, err)
	var state, reason string
	must(t, pool.QueryRow(ctx, `SELECT state,conflict_reason FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, u, a).Scan(&state, &reason))
	if state != "conflict" || reason != "gc_pending" {
		t.Fatalf("preset-only tombstone claim = (%s,%s), want conflict/gc_pending", state, reason)
	}
	setGCAfter(t, pool, homeID, "interval '25 hours'")
	deleted, err := mgr.GCConfirm(ctx, h, []string{homeID})
	must(t, err)
	if deleted != 1 {
		t.Fatalf("preset-only GC deleted %d homes, want one", deleted)
	}
	var claims int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, u, a).Scan(&claims))
	if claims != 0 {
		t.Fatalf("preset-only claim retained after exact GC: %d", claims)
	}
}

func TestGCConfirmKeepsClaimWhenAnotherLocationIsKnown(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	u := seedUser(t, pool, "claim-other@test.local")
	a := seedApp(t, pool, "Claim Other")
	h1 := seedNamedHost(t, pool, "claim-host-1")
	h2 := seedNamedHost(t, pool, "claim-host-2")
	first := insertHome(t, pool, u, a, h1)
	_ = insertHome(t, pool, u, a, h2)
	setGCAfter(t, pool, first, "interval '25 hours'")
	_, err := pool.Exec(ctx, `INSERT INTO managed_home_claims (user_id,canonical_app_id,host_id,state,conflict_reason) VALUES ($1::uuid,$2::uuid,$3::uuid,'conflict','gc_pending')`, u, a, h1)
	must(t, err)
	mgr := NewLocal(pool, t.TempDir())
	deleted, err := mgr.GCConfirm(ctx, h1, []string{first})
	must(t, err)
	if deleted != 1 {
		t.Fatalf("gc-confirm deleted %d, want exact row", deleted)
	}
	var reason string
	must(t, pool.QueryRow(ctx, `SELECT conflict_reason FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, u, a).Scan(&reason))
	if reason == "" {
		t.Fatal("claim released despite another known location")
	}
}
