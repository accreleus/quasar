// origins_db_test.go — the first-run wizard v2 §S6e allowed_origins field on
// PATCH /v1/admin/settings (migration 0064), exercised through the REAL
// RequireAuth→RequireAdmin chain like its neighbours in handler_db_test.go.
package settings

import (
	"context"
	"encoding/json"
	"github.com/accreleus/quasar/control-plane/internal/access"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/origins"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPatchAllowedOriginsRoundTrip — the operator path: a PATCH sets the
// column, GET reads it back NORMALIZED, and no other field moves.
func TestPatchAllowedOriginsRoundTrip(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	code, env := patch(t, `{"allowed_origins":["https://QUASAR.example.com:8443","http://192.0.2.10:8080"]}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH allowed_origins: status %d, want 200", code)
	}
	want := []string{"https://quasar.example.com:8443", "http://192.0.2.10:8080"}
	assertOrigins(t, "PATCH response", env.Settings.AllowedOrigins, want)
	if env.Settings.RegistrationMode != RegistrationClosed {
		t.Errorf("an unrelated field changed: registration_mode = %q", env.Settings.RegistrationMode)
	}

	// STORED NORMALIZED, not as typed. This is what keeps "what an admin saved"
	// and "what /v1/signal compares against" the same string.
	assertOrigins(t, "GET after PATCH", get(t).Settings.AllowedOrigins, want)
}

// TestPatchAllowedOriginsAbsentMeansUnchanged is the pointer-decode rule. A
// plain []string would decode an absent field to nil and silently wipe the
// allow-list every time an admin changed the registration mode.
func TestPatchAllowedOriginsAbsentMeansUnchanged(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	if code, _ := patch(t, `{"allowed_origins":["https://keep.example"]}`); code != http.StatusOK {
		t.Fatalf("seed PATCH: status %d", code)
	}
	code, env := patch(t, `{"registration_mode":"open"}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH registration_mode: status %d, want 200", code)
	}
	assertOrigins(t, "after an unrelated PATCH", env.Settings.AllowedOrigins, []string{"https://keep.example"})
	assertOrigins(t, "GET after an unrelated PATCH", get(t).Settings.AllowedOrigins, []string{"https://keep.example"})
}

// TestPatchAllowedOriginsEmptyArrayClears is the other half of the pointer
// rule: an EXPLICIT [] is a real request ("clear the list") and must be
// distinguishable from absence.
func TestPatchAllowedOriginsEmptyArrayClears(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	if code, _ := patch(t, `{"allowed_origins":["https://gone.example"]}`); code != http.StatusOK {
		t.Fatalf("seed PATCH: status %d", code)
	}
	code, env := patch(t, `{"allowed_origins":[]}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH []: status %d, want 200", code)
	}
	assertOrigins(t, "after clearing", env.Settings.AllowedOrigins, nil)
	assertOrigins(t, "GET after clearing", get(t).Settings.AllowedOrigins, nil)
}

// TestPatchAllowedOriginsRejectsWildcardAndWritesNothing — §S6e's hard rule,
// plus the "validate before any write" discipline: the rejected PATCH also
// carries a valid registration_mode change, and NEITHER may land.
func TestPatchAllowedOriginsRejectsWildcardAndWritesNothing(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	code, _ := patch(t, `{"registration_mode":"open","allowed_origins":["*"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf(`PATCH with "*": status %d, want 400 — a wildcard would discard the origin check entirely`, code)
	}
	got := get(t)
	if got.Settings.RegistrationMode != RegistrationClosed {
		t.Errorf("a rejected PATCH applied a partial change: registration_mode = %q, want %q",
			got.Settings.RegistrationMode, RegistrationClosed)
	}
	assertOrigins(t, "after a rejected PATCH", got.Settings.AllowedOrigins, nil)
}

func TestPatchAllowedOriginsRejectsMalformedEntry(t *testing.T) {
	pool := testDB(t)
	patch, _ := newSettingsHarness(t, pool)
	for _, bad := range []string{
		`{"allowed_origins":["https://ok.example","https://evil.example/@trusted"]}`,
		`{"allowed_origins":["ftp://quasar.example"]}`,
		`{"allowed_origins":["quasar.example"]}`,
	} {
		if code, _ := patch(t, bad); code != http.StatusBadRequest {
			t.Errorf("PATCH %s: status %d, want 400", bad, code)
		}
	}
}

// TestAllowedOriginsDefaultsToEmptyList — the store read the signal handler
// makes per handshake. An unconfigured instance must yield [] (and never null,
// which would render as a broken list in the admin UI).
func TestAllowedOriginsDefaultsToEmptyList(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	must(t, store.Seed(context.Background(), RegistrationClosed))

	list, err := store.AllowedOrigins(context.Background())
	must(t, err)
	if list == nil {
		t.Fatal("AllowedOrigins returned nil; want an empty slice")
	}
	if len(list) != 0 {
		t.Fatalf("AllowedOrigins = %v, want empty on a fresh instance", list)
	}
}

func assertOrigins(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: allowed_origins = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: allowed_origins = %v, want %v", what, got, want)
		}
	}
	if got == nil {
		t.Errorf("%s: allowed_origins is nil; the wire shape must be [] so a UI can render it", what)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: allowed_origins = %v, want %v", what, got, want)
	}
}

// A browser reads the active policy, PATCHes its origin, and immediately reads
// it again before leaving setup. No TTL sleep may be required in that sequence.
func TestPatchAllowedOriginsImmediatelyUpdatesAccessCheck(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)
	must(t, store.Seed(ctx, RegistrationClosed))
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	must(t, err)
	_, err = authSvc.Register(ctx, "origin-admin@t.local", "originadmin", "password12345")
	must(t, err)
	must(t, execT(ctx, pool, `UPDATE users SET role='admin' WHERE email='origin-admin@t.local'`))
	token, err := authSvc.Login(ctx, "origin-admin@t.local", "password12345", "test")
	must(t, err)
	authHandler := auth.NewHandler(authSvc)
	admin := func(next http.Handler) http.Handler { return authHandler.RequireAuth(authHandler.RequireAdmin(next)) }
	resolver := origins.NewResolver("", false, store, slog.Default())
	handler := NewHandler(store)
	handler.OnAllowedOriginsChanged = resolver.Invalidate
	mux := http.NewServeMux()
	handler.Register(mux, admin)
	access.NewService(nil, resolver, slog.Default()).Register(mux, admin)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	request := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		must(t, err)
		req.Header.Set("Authorization", "Bearer "+token.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		return resp
	}
	check := func(want []string) {
		t.Helper()
		resp := request(http.MethodGet, "/v1/admin/access-check", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("access check: %d", resp.StatusCode)
		}
		var body struct {
			Origins struct {
				Source  string   `json:"source"`
				Allowed []string `json:"allowed"`
			} `json:"origins"`
		}
		must(t, json.NewDecoder(resp.Body).Decode(&body))
		if body.Origins.Source != "database" {
			t.Fatalf("source = %q", body.Origins.Source)
		}
		assertOrigins(t, "immediate access check", body.Origins.Allowed, want)
	}
	check(nil) // primes the shared resolver cache with the previous policy
	for _, tc := range []struct {
		body   string
		status int
		want   []string
	}{
		{`{"allowed_origins":["https://play.example.test"]}`, http.StatusOK, []string{"https://play.example.test"}},
		{`{"allowed_origins":["*"]}`, http.StatusBadRequest, []string{"https://play.example.test"}},
		{`{"allowed_origins":[]}`, http.StatusOK, nil},
	} {
		resp := request(http.MethodPatch, "/v1/admin/settings", tc.body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("PATCH: got %d want %d", resp.StatusCode, tc.status)
		}
		check(tc.want)
	}
}
