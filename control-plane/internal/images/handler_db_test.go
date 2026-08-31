// handler_db_test.go — the P1 admin surface exercised through the REAL
// RequireAuth→RequireAdmin chain, same posture as
// internal/settings/handler_db_test.go: hiding admin UI is never the access
// control (CLAUDE.md invariant #6), so the gate itself must be real here too.
package images

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// newImagesHarness wires the real images handler behind the real admin gate
// and hands back functions that perform authenticated GET/POST calls.
func newImagesHarness(t *testing.T, pool *pgxpool.Pool, fetch Fetcher) (
	get func(t *testing.T) (int, Envelope),
	sync func(t *testing.T) (int, Envelope),
) {
	t.Helper()
	ctx := context.Background()

	store := NewStoreWithFetcher(pool, fetch)

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	mustT(t, err)
	authHandler := auth.NewHandler(authSvc)
	_, err = authSvc.Register(ctx, "images-admin@t.local", "imagesadmin", "password12345")
	mustT(t, err)
	_, err = pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email='images-admin@t.local'`)
	mustT(t, err)
	tok, err := authSvc.Login(ctx, "images-admin@t.local", "password12345", "test")
	mustT(t, err)

	h := NewHandler(store)
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	do := func(t *testing.T, method, path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(""))
		mustT(t, err)
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		resp, err := http.DefaultClient.Do(req)
		mustT(t, err)
		return resp
	}

	get = func(t *testing.T) (int, Envelope) {
		t.Helper()
		resp := do(t, http.MethodGet, "/v1/admin/images")
		defer resp.Body.Close()
		var env Envelope
		mustT(t, json.NewDecoder(resp.Body).Decode(&env))
		return resp.StatusCode, env
	}
	sync = func(t *testing.T) (int, Envelope) {
		t.Helper()
		resp := do(t, http.MethodPost, "/v1/admin/images/sync")
		defer resp.Body.Close()
		var env Envelope
		mustT(t, json.NewDecoder(resp.Body).Decode(&env))
		return resp.StatusCode, env
	}
	return get, sync
}

func mustT(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestHandlerSyncPopulatesAndGetReadsBack — POST /v1/admin/images/sync
// populates the catalog through the real HTTP + admin-gate path, and GET
// /v1/admin/images reads the same picture back. Both must answer 200.
func TestHandlerSyncPopulatesAndGetReadsBack(t *testing.T) {
	pool := testDB(t)
	get, sync := newImagesHarness(t, pool, fixtureFetcher{data: readFixture(t)})

	code, env := sync(t)
	if code != http.StatusOK {
		t.Fatalf("POST /v1/admin/images/sync: status %d, want 200", code)
	}
	if env.SyncError != nil {
		t.Fatalf("sync_error: got %q want nil", *env.SyncError)
	}
	if len(env.Images) != 1 || env.Images[0].ID != "steam" {
		t.Fatalf("sync response images: got %+v", env.Images)
	}

	code2, env2 := get(t)
	if code2 != http.StatusOK {
		t.Fatalf("GET /v1/admin/images: status %d, want 200", code2)
	}
	if len(env2.Images) != 1 || env2.Images[0].ID != "steam" {
		t.Fatalf("GET response images: got %+v", env2.Images)
	}
}

// TestHandlerSyncFailureStays200 — acceptance: a sync whose fetch fails must
// still answer HTTP 200 (never 500), carrying sync_error in the body. This
// is the "launches unaffected" posture asserted at the wire level, on top of
// TestSyncWithFailingFetcherServesCachedCatalog's store-level assertion.
func TestHandlerSyncFailureStays200(t *testing.T) {
	pool := testDB(t)
	get, sync := newImagesHarness(t, pool, failingFetcher{err: errAlwaysFails})

	code, env := sync(t)
	if code != http.StatusOK {
		t.Fatalf("POST /v1/admin/images/sync with a failing fetch: status %d, want 200 (never 500 — launches must be unaffected)", code)
	}
	if env.SyncError == nil {
		t.Fatal("sync_error: got nil, want the fetch failure recorded")
	}
	if len(env.Images) != 0 {
		t.Fatalf("images on first-ever failed sync (nothing cached yet): got %+v, want empty", env.Images)
	}

	// GET must also stay 200 and reflect the same empty-but-not-erroring state.
	code2, env2 := get(t)
	if code2 != http.StatusOK {
		t.Fatalf("GET /v1/admin/images: status %d, want 200", code2)
	}
	if len(env2.Images) != 0 {
		t.Fatalf("GET images: got %+v, want empty", env2.Images)
	}
}

var errAlwaysFails = &fetchAlwaysFailsError{}

type fetchAlwaysFailsError struct{}

func (e *fetchAlwaysFailsError) Error() string { return "manifest fetch always fails (test double)" }
