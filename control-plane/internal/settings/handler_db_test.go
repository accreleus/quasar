// handler_db_test.go — the admin-libraries amendment's two new PATCH
// /v1/admin/settings fields (library_discovery_interval_minutes,
// library_discovery_appdetails_enabled), exercised through the REAL
// RequireAuth→RequireAdmin chain, the same way
// internal/library/forcescan_db_test.go's nudgeHarness drives the settings
// handler for the on-enable nudge — hiding admin UI is never the access
// control (CLAUDE.md invariant #6), so the gate itself must be real here too.
package settings

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// settingsEnvelope mirrors handler.go's {"settings": ...} response shape.
type settingsEnvelope struct {
	Settings Settings `json:"settings"`
}

// newSettingsHarness wires the real settings handler behind the real admin
// gate and hands back a function that performs an authenticated PATCH, plus
// one that performs an authenticated GET.
func newSettingsHarness(t *testing.T, pool *pgxpool.Pool) (
	patch func(t *testing.T, body string) (int, settingsEnvelope),
	get func(t *testing.T) settingsEnvelope,
) {
	t.Helper()
	ctx := context.Background()

	store := NewStore(pool)
	must(t, store.Seed(ctx, RegistrationClosed))

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	must(t, err)
	authHandler := auth.NewHandler(authSvc)
	_, err = authSvc.Register(ctx, "settings-admin@t.local", "settingsadmin", "password12345")
	must(t, err)
	must(t, execT(ctx, pool, `UPDATE users SET role='admin' WHERE email='settings-admin@t.local'`))
	tok, err := authSvc.Login(ctx, "settings-admin@t.local", "password12345", "test")
	must(t, err)

	h := NewHandler(store)
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	do := func(t *testing.T, method, body string) *http.Response {
		t.Helper()
		var reqBody *strings.Reader
		if body != "" {
			reqBody = strings.NewReader(body)
		} else {
			reqBody = strings.NewReader("")
		}
		req, err := http.NewRequest(method, srv.URL+"/v1/admin/settings", reqBody)
		must(t, err)
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		return resp
	}

	patch = func(t *testing.T, body string) (int, settingsEnvelope) {
		t.Helper()
		resp := do(t, http.MethodPatch, body)
		defer resp.Body.Close()
		var env settingsEnvelope
		must(t, json.NewDecoder(resp.Body).Decode(&env))
		return resp.StatusCode, env
	}
	get = func(t *testing.T) settingsEnvelope {
		t.Helper()
		resp := do(t, http.MethodGet, "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/admin/settings: status %d, want 200", resp.StatusCode)
		}
		var env settingsEnvelope
		must(t, json.NewDecoder(resp.Body).Decode(&env))
		return env
	}
	return patch, get
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func execT(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(ctx, sql, args...)
	return err
}

// TestPatchLibraryDiscoveryIntervalRoundTrip — the operator-UI path: a PATCH
// sets the database column, GET reads it back, and every OTHER field is left
// alone (pointer-decode semantics, same rule library_discovery_enabled
// already follows).
func TestPatchLibraryDiscoveryIntervalRoundTrip(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	code, env := patch(t, `{"library_discovery_interval_minutes":90}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH interval=90: status %d, want 200", code)
	}
	if env.Settings.LibraryDiscoveryIntervalMinutes != 90 {
		t.Errorf("PATCH response interval = %d, want 90", env.Settings.LibraryDiscoveryIntervalMinutes)
	}
	if env.Settings.RegistrationMode != RegistrationClosed {
		t.Errorf("an unrelated field (registration_mode) changed: got %q, want %q",
			env.Settings.RegistrationMode, RegistrationClosed)
	}

	got := get(t)
	if got.Settings.LibraryDiscoveryIntervalMinutes != 90 {
		t.Errorf("GET after PATCH: interval = %d, want 90 (persisted)", got.Settings.LibraryDiscoveryIntervalMinutes)
	}
}

// TestPatchLibraryDiscoveryAppDetailsRoundTrip — same round trip for the
// boolean field.
func TestPatchLibraryDiscoveryAppDetailsRoundTrip(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	code, env := patch(t, `{"library_discovery_appdetails_enabled":true}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH appdetails=true: status %d, want 200", code)
	}
	if !env.Settings.LibraryDiscoveryAppDetailsEnabled {
		t.Errorf("PATCH response appdetails_enabled = false, want true")
	}

	got := get(t)
	if !got.Settings.LibraryDiscoveryAppDetailsEnabled {
		t.Errorf("GET after PATCH: appdetails_enabled = false, want true (persisted)")
	}
}

// TestPatchLibraryDiscoveryIntervalBounds — 14 and 10081 are both out of
// bounds and must 400 validation_failed WITHOUT writing anything; 15 and
// 10080 are the inclusive edges and must succeed.
func TestPatchLibraryDiscoveryIntervalBounds(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	before := get(t).Settings.LibraryDiscoveryIntervalMinutes

	for _, bad := range []int{14, 10081, 0, -1} {
		code, _ := patch(t, `{"library_discovery_interval_minutes":`+strconv.Itoa(bad)+`}`)
		if code != http.StatusBadRequest {
			t.Errorf("PATCH interval=%d: status %d, want 400", bad, code)
		}
	}
	if after := get(t).Settings.LibraryDiscoveryIntervalMinutes; after != before {
		t.Errorf("an out-of-bounds PATCH changed the interval: before=%d after=%d, want no change", before, after)
	}

	for _, ok := range []int{15, 10080} {
		code, env := patch(t, `{"library_discovery_interval_minutes":`+strconv.Itoa(ok)+`}`)
		if code != http.StatusOK {
			t.Errorf("PATCH interval=%d (boundary): status %d, want 200", ok, code)
		}
		if env.Settings.LibraryDiscoveryIntervalMinutes != ok {
			t.Errorf("PATCH interval=%d (boundary): got %d", ok, env.Settings.LibraryDiscoveryIntervalMinutes)
		}
	}
}

// TestPatchStorageProviderRejectsVolume — #473 hard removal. An admin PATCH
// carrying the removed docker-volume driver must be REJECTED (400) with the
// specific ErrVolumeDriverRemovedMsg wording (not the generic "must be auto
// or local" message another bad value gets), and must not touch the stored
// setting. "auto" and "local" keep working, exactly as before.
func TestPatchStorageProviderRejectsVolume(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	before := get(t).Settings.StorageProvider

	code, body := patchRaw(t, pool, `{"storage_provider":"volume"}`)
	if code != http.StatusBadRequest {
		t.Fatalf(`PATCH storage_provider="volume": status %d, want 400`, code)
	}
	if !strings.Contains(body, ErrVolumeDriverRemovedMsg) {
		t.Errorf("PATCH storage_provider=%q: body %q does not contain %q", "volume", body, ErrVolumeDriverRemovedMsg)
	}
	if after := get(t).Settings.StorageProvider; after != before {
		t.Errorf("rejected volume PATCH changed storage_provider: before=%q after=%q", before, after)
	}

	// Another invalid value gets the generic message, not the volume-specific
	// one — the two must stay distinguishable.
	code, body = patchRaw(t, pool, `{"storage_provider":"nfs"}`)
	if code != http.StatusBadRequest {
		t.Fatalf(`PATCH storage_provider="nfs": status %d, want 400`, code)
	}
	if strings.Contains(body, ErrVolumeDriverRemovedMsg) {
		t.Errorf(`PATCH storage_provider="nfs" got the volume-specific message: %q`, body)
	}

	for _, ok := range []string{"auto", "local"} {
		code, env := patch(t, `{"storage_provider":"`+ok+`"}`)
		if code != http.StatusOK {
			t.Errorf("PATCH storage_provider=%q: status %d, want 200", ok, code)
		}
		if env.Settings.StorageProvider != ok {
			t.Errorf("PATCH storage_provider=%q: got %q", ok, env.Settings.StorageProvider)
		}
	}
}

// patchRaw is a second, minimal harness (its own server) that hands back the
// raw response body text — newSettingsHarness's `patch` decodes straight into
// settingsEnvelope, which silently ignores an error response's "error" field.
func patchRaw(t *testing.T, pool *pgxpool.Pool, body string) (int, string) {
	t.Helper()
	ctx := context.Background()
	store := NewStore(pool)
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	must(t, err)
	authHandler := auth.NewHandler(authSvc)
	email := "storage-admin@t.local"
	if _, err := authSvc.Register(ctx, email, "storageadmin", "password12345"); err != nil {
		// Already registered by an earlier call in this test's subtests — fine.
	}
	must(t, execT(ctx, pool, `UPDATE users SET role='admin' WHERE email=$1`, email))
	tok, err := authSvc.Login(ctx, email, "password12345", "test")
	must(t, err)

	h := NewHandler(store)
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/v1/admin/settings", strings.NewReader(body))
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	must(t, err)
	return resp.StatusCode, string(raw)
}

// TestPatchSettingsWithNoKnownFieldIsANoOp — every field including the two
// new ones is optional; a PATCH naming none of them returns current state
// rather than erroring or clearing anything (mirrors handler.go's existing
// three-field check, now five).
func TestPatchSettingsWithNoKnownFieldIsANoOp(t *testing.T) {
	pool := testDB(t)
	patch, _ := newSettingsHarness(t, pool)

	code, env := patch(t, `{}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH {}: status %d, want 200", code)
	}
	if env.Settings.LibraryDiscoveryIntervalMinutes != 360 {
		t.Errorf("PATCH {}: interval = %d, want the untouched default 360", env.Settings.LibraryDiscoveryIntervalMinutes)
	}
}

// TestPatchMicCaptureEnabledRoundTrip — microphone capture amendment (spec
// §3.5): a PATCH sets the instance gate, GET reads it back, and every other
// field is left alone (same pointer-decode rule as its neighbours).
func TestPatchMicCaptureEnabledRoundTrip(t *testing.T) {
	pool := testDB(t)
	patch, get := newSettingsHarness(t, pool)

	before := get(t)
	if before.Settings.MicCaptureEnabled {
		t.Fatalf("mic_capture_enabled should default false")
	}

	code, env := patch(t, `{"mic_capture_enabled":true}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH mic_capture_enabled=true: status %d, want 200", code)
	}
	if !env.Settings.MicCaptureEnabled {
		t.Errorf("PATCH response mic_capture_enabled = false, want true")
	}
	if env.Settings.RegistrationMode != RegistrationClosed {
		t.Errorf("an unrelated field (registration_mode) changed: got %q, want %q",
			env.Settings.RegistrationMode, RegistrationClosed)
	}

	got := get(t)
	if !got.Settings.MicCaptureEnabled {
		t.Errorf("GET after PATCH: mic_capture_enabled = false, want true (persisted)")
	}

	// Absence on a subsequent PATCH must leave it alone — the pointer-decode
	// rule, exercised the same way TestPatchLibraryDiscoveryIntervalRoundTrip
	// exercises registration_mode staying untouched.
	code, env = patch(t, `{"registration_mode":"open"}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH registration_mode=open: status %d, want 200", code)
	}
	if !env.Settings.MicCaptureEnabled {
		t.Errorf("an unrelated PATCH cleared mic_capture_enabled: got false, want true (unchanged)")
	}
}
