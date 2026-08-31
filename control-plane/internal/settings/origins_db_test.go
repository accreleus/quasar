// origins_db_test.go — the first-run wizard v2 §S6e allowed_origins field on
// PATCH /v1/admin/settings (migration 0064), exercised through the REAL
// RequireAuth→RequireAdmin chain like its neighbours in handler_db_test.go.
package settings

import (
	"context"
	"net/http"
	"strings"
	"testing"
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
