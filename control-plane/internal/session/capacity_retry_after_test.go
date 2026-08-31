package session

// #494 — Session slot releases ~15s after DELETE, so an immediate replacement
// launch on a full host bounces once with `503 capacity_exhausted`. Option 2
// (server) adds a `Retry-After` header to that response so a client knows the
// refusal is transient and roughly how long the wait is, without changing the
// error envelope's body shape (see the reasoning at the ErrCapacityExhausted
// case in handler.go).
//
// This is an HTTP-level test, not a store-level one, because the header is set
// on the ResponseWriter in the handler — the store-level tests in
// governor_test.go / lifecycle_test.go / vram_admission_test.go already prove
// ErrCapacityExhausted itself; what only an HTTP test can prove is the wire
// header.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// newCapacityTestServer wires the real handler over a real store/coordinator
// with no home provider — an ordinary (non-Steam-derived) app never needs one,
// and #494 is not about the home path.
func newCapacityTestServer(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, *auth.Service) {
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
	NewHandler(coord, store).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, authSvc
}

// TestLaunchCapacityExhausted503CarriesRetryAfter is #494's server-side option:
// the second launch onto a one-slot GPU whose only slot is already reserved
// gets 503 capacity_exhausted with a Retry-After header a client can honour,
// instead of a bare error the client has no timing information for.
func TestLaunchCapacityExhausted503CarriesRetryAfter(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newCapacityTestServer(t, pool)
	ctx := context.Background()

	s := seed(t, pool, 1) // one encode slot total
	// Default max_concurrent_sessions (3, migration 0002) comfortably covers the
	// two launches this test drives, so quota is not the constraint under test.
	token, _ := registerUser(t, ctx, authSvc, "cap494@test.local", "cap494")

	// First launch fills the only slot.
	resp, body := launchHTTP(t, srv.URL, token, s.appID)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first launch: want 201, got %d (%+v)", resp.StatusCode, body)
	}

	// The second, immediate launch bounces — the slot is held, not free.
	resp2, body2 := launchHTTP(t, srv.URL, token, s.appID)
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second launch: want 503, got %d (%+v)", resp2.StatusCode, body2)
	}
	if body2.Error.Code != "capacity_exhausted" {
		t.Errorf("error.code = %q, want capacity_exhausted", body2.Error.Code)
	}
	retryAfter := resp2.Header.Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("503 capacity_exhausted carries no Retry-After header — a polling client has no idea " +
			"whether or how long to wait (#494)")
	}
	if retryAfter != capacityExhaustedRetryAfterSeconds {
		t.Errorf("Retry-After = %q, want %q", retryAfter, capacityExhaustedRetryAfterSeconds)
	}
}

// TestLaunchNoHostAvailable503OmitsRetryAfter is the negative control: the
// OTHER 503 launch code, no_host_available (nothing online at all), gets no
// Retry-After — retrying against an offline fleet on a fixed clock is not what
// #494 is about, and a client should not be told a specific wait for a
// condition that is not on any teardown timer.
func TestLaunchNoHostAvailable503OmitsRetryAfter(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newCapacityTestServer(t, pool)
	ctx := context.Background()

	// Seed an app with NO online host/GPU at all. The launch caller is the
	// registered login below — this fixture needs no user row of its own.
	var appID string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('app-nohost', 1024, 1, 1280, 720, 60, 6000) RETURNING id::text`).Scan(&appID))
	entitleAll(t, pool, appID)

	token, _ := registerUser(t, ctx, authSvc, "nohost494login@test.local", "nohost494login")

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]any{"app_id": appID})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sessions", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/sessions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("launch with no online host: want 503, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Errorf("no_host_available carries Retry-After = %q, want none — it is not the #494 "+
			"teardown-timer case", got)
	}
}
