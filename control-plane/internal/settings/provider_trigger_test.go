// provider_trigger_test.go — image-management P5: the settings PATCH handler
// fires EnsureLibraryProviders exactly on a library_discovery_enabled false→true
// transition, and never on a re-save or a disable (the disable-is-non-destructive
// guarantee at the handler seam). TEST_DATABASE_URL-gated.
package settings

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// newProviderTriggerHarness wires the real settings handler behind the real
// admin gate with an EnsureLibraryProviders spy, returning a PATCH func and the
// call counter.
func newProviderTriggerHarness(t *testing.T, pool *pgxpool.Pool) (func(t *testing.T, body string) int, *int32, *int32) {
	t.Helper()
	ctx := context.Background()

	store := NewStore(pool)
	must(t, store.Seed(ctx, RegistrationClosed))

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	must(t, err)
	authHandler := auth.NewHandler(authSvc)
	_, err = authSvc.Register(ctx, "p5-admin@t.local", "p5admin", "password12345")
	must(t, err)
	must(t, execT(ctx, pool, `UPDATE users SET role='admin' WHERE email='p5-admin@t.local'`))
	tok, err := authSvc.Login(ctx, "p5-admin@t.local", "password12345", "test")
	must(t, err)

	var calls, disables int32
	h := NewHandler(store)
	h.EnsureLibraryProviders = func() { atomic.AddInt32(&calls, 1) }
	h.DisableLibraryProviders = func() { atomic.AddInt32(&disables, 1) }

	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	patch := func(t *testing.T, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPatch, srv.URL+"/v1/admin/settings", strings.NewReader(body))
		must(t, err)
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	return patch, &calls, &disables
}

func TestEnsureProvidersFiresOnlyOnFalseToTrue(t *testing.T) {
	pool := testDB(t)
	patch, calls, _ := newProviderTriggerHarness(t, pool)

	// false→true: fires once.
	if code := patch(t, `{"library_discovery_enabled":true}`); code != http.StatusOK {
		t.Fatalf("enable: status %d, want 200", code)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("EnsureLibraryProviders calls after enable = %d, want 1", got)
	}

	// true→true (a re-save, e.g. changing another field): must NOT fire again.
	if code := patch(t, `{"library_discovery_enabled":true}`); code != http.StatusOK {
		t.Fatalf("re-save: status %d, want 200", code)
	}
	if code := patch(t, `{"registration_mode":"open"}`); code != http.StatusOK {
		t.Fatalf("unrelated field: status %d, want 200", code)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("EnsureLibraryProviders fired on a re-save/unrelated PATCH: calls = %d, want 1", got)
	}

	// true→false (disable): must NOT fire (disable is non-destructive; nothing
	// installs or uninstalls).
	if code := patch(t, `{"library_discovery_enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable: status %d, want 200", code)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("EnsureLibraryProviders fired on disable: calls = %d, want 1", got)
	}
}

// TestDisableProvidersFiresOnlyOnTrueToFalse — #456's disable half, the mirror of
// the test above: the provider apps are disabled on the true→false transition and
// on nothing else (not an enable, not a re-save, not a false→false).
func TestDisableProvidersFiresOnlyOnTrueToFalse(t *testing.T) {
	pool := testDB(t)
	patch, _, disables := newProviderTriggerHarness(t, pool)

	// false→false (the seeded state is false): nothing to turn off.
	if code := patch(t, `{"library_discovery_enabled":false}`); code != http.StatusOK {
		t.Fatalf("false→false: status %d, want 200", code)
	}
	if got := atomic.LoadInt32(disables); got != 0 {
		t.Fatalf("DisableLibraryProviders fired on false→false: %d, want 0", got)
	}

	// false→true: still nothing — the enable path is the other hook.
	if code := patch(t, `{"library_discovery_enabled":true}`); code != http.StatusOK {
		t.Fatalf("enable: status %d, want 200", code)
	}
	if got := atomic.LoadInt32(disables); got != 0 {
		t.Fatalf("DisableLibraryProviders fired on an enable: %d, want 0", got)
	}

	// true→false: fires exactly once.
	if code := patch(t, `{"library_discovery_enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable: status %d, want 200", code)
	}
	if got := atomic.LoadInt32(disables); got != 1 {
		t.Fatalf("DisableLibraryProviders calls after disable = %d, want 1", got)
	}

	// And a repeat disable (false→false) does not fire again.
	if code := patch(t, `{"library_discovery_enabled":false}`); code != http.StatusOK {
		t.Fatalf("re-disable: status %d, want 200", code)
	}
	if got := atomic.LoadInt32(disables); got != 1 {
		t.Errorf("DisableLibraryProviders fired on a repeat disable: %d, want 1", got)
	}
}
