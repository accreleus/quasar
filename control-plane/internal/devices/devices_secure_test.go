package devices

// LP-SEC-01 SEC-04 (list / rename / trust) + SEC-05 (token↔device binding + revocation).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// loginHTTP logs in over HTTP with an optional device_key and returns the access token.
func loginHTTP(t *testing.T, srvURL, email, pass, deviceKey string) string {
	t.Helper()
	body := map[string]any{"email": email, "password": pass}
	if deviceKey != "" {
		body["device_key"] = deviceKey
	}
	resp := doRequest(t, http.MethodPost, srvURL+"/v1/auth/login", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: got %d want 200", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return out.AccessToken
}

type listItem struct {
	ID              string  `json:"id"`
	DeviceKey       string  `json:"device_key"`
	Name            *string `json:"name"`
	Trusted         bool    `json:"trusted"`
	Current         bool    `json:"current"`
	ActiveSessionID *string `json:"active_session_id"`
}

func listDevices(t *testing.T, srvURL, tok string) []listItem {
	t.Helper()
	resp := doRequest(t, http.MethodGet, srvURL+"/v1/me/devices", tok, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: got %d want 200", resp.StatusCode)
	}
	var out struct {
		Devices []listItem `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return out.Devices
}

func mustRegister(t *testing.T, svc *auth.Service, email, user, pass string) {
	t.Helper()
	if _, err := svc.Register(context.Background(), email, user, pass); err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
}

// TestLoginBindsDeviceAndRevokeInvalidatesToken is the SEC-05 core: a login declaring a
// device_key mints a token bound to that device; revoking the device invalidates the
// token (real revocation), and the device row is gone.
func TestLoginBindsDeviceAndRevokeInvalidatesToken(t *testing.T) {
	pool := testDB(t)
	srv, svc := newServer(t, pool)
	mustRegister(t, svc, "bind@x.io", "binder", "quasar-fixture-pw-08")

	tok := loginHTTP(t, srv.URL, "bind@x.io", "quasar-fixture-pw-08", "dev-key-1")

	devs := listDevices(t, srv.URL, tok)
	if len(devs) != 1 || !devs[0].Current {
		t.Fatalf("expected 1 current device, got %+v", devs)
	}
	deviceID := devs[0].ID

	// The bound token authenticates fine right now.
	if u, _, err := svc.Authenticate(context.Background(), tok); err != nil || u.Email != "bind@x.io" {
		t.Fatalf("pre-revoke auth: u=%v err=%v", u, err)
	}

	// Revoke the device.
	resp := doRequest(t, http.MethodDelete, srv.URL+"/v1/me/devices/"+deviceID, tok, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: got %d want 204", resp.StatusCode)
	}

	// The token bound to that device is now invalid — this is the load-bearing assertion.
	if _, _, err := svc.Authenticate(context.Background(), tok); err != auth.ErrUserNotFound {
		t.Fatalf("post-revoke auth: got %v want ErrUserNotFound (token must be revoked)", err)
	}
	// The device row is gone (a re-login gets a fresh row, never reclaims the old token).
	if n := countDevices(t, pool, userID(t, pool, "bind@x.io"), "dev-key-1"); n != 0 {
		t.Fatalf("device row remained after revoke: %d", n)
	}
}

// TestReloginGetsFreshDeviceAfterRevoke: re-login with the same device_key mints a fresh
// bindable token and a fresh device row — it does not silently reclaim the revoked token.
func TestReloginGetsFreshDeviceAfterRevoke(t *testing.T) {
	pool := testDB(t)
	srv, svc := newServer(t, pool)
	mustRegister(t, svc, "re@x.io", "reuser", "quasar-fixture-pw-08")

	tok1 := loginHTTP(t, srv.URL, "re@x.io", "quasar-fixture-pw-08", "dev-key-x")
	id1 := listDevices(t, srv.URL, tok1)[0].ID
	resp := doRequest(t, http.MethodDelete, srv.URL+"/v1/me/devices/"+id1, tok1, nil)
	resp.Body.Close()

	tok2 := loginHTTP(t, srv.URL, "re@x.io", "quasar-fixture-pw-08", "dev-key-x")
	if tok2 == tok1 {
		t.Fatal("re-login returned the same token")
	}
	devs := listDevices(t, srv.URL, tok2)
	if len(devs) != 1 || devs[0].ID == id1 {
		t.Fatalf("re-login must mint a fresh device row, got %+v (old id %s)", devs, id1)
	}
	if _, _, err := svc.Authenticate(context.Background(), tok2); err != nil {
		t.Fatalf("fresh token invalid: %v", err)
	}
}

// TestPatchAndRevokeOwnerScoped: a device belonging to another user is 403 (never 404) on
// both PATCH and DELETE — no existence leak.
func TestPatchAndRevokeOwnerScoped(t *testing.T) {
	pool := testDB(t)
	srv, svc := newServer(t, pool)
	mustRegister(t, svc, "owner@x.io", "owner", "quasar-fixture-pw-08")
	mustRegister(t, svc, "attacker@x.io", "attacker", "quasar-fixture-pw-08")

	ownerTok := loginHTTP(t, srv.URL, "owner@x.io", "quasar-fixture-pw-08", "owner-dev")
	ownerDevID := listDevices(t, srv.URL, ownerTok)[0].ID
	attackerTok := loginHTTP(t, srv.URL, "attacker@x.io", "quasar-fixture-pw-08", "attacker-dev")

	// Attacker PATCHes the owner's device → 403.
	resp := doRequest(t, http.MethodPatch, srv.URL+"/v1/me/devices/"+ownerDevID, attackerTok,
		map[string]any{"name": "pwned"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-user PATCH: got %d want 403", resp.StatusCode)
	}
	// Attacker DELETEs the owner's device → 403.
	resp = doRequest(t, http.MethodDelete, srv.URL+"/v1/me/devices/"+ownerDevID, attackerTok, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-user DELETE: got %d want 403", resp.StatusCode)
	}
	// A completely unknown (well-formed) id is ALSO 403, not 404 (no oracle).
	resp = doRequest(t, http.MethodDelete, srv.URL+"/v1/me/devices/00000000-0000-0000-0000-000000000000", attackerTok, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown id DELETE: got %d want 403", resp.StatusCode)
	}

	// Owner renames + trusts their own device → 200, persisted.
	resp = doRequest(t, http.MethodPatch, srv.URL+"/v1/me/devices/"+ownerDevID, ownerTok,
		map[string]any{"name": "Living-room PC", "trusted": true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner PATCH: got %d want 200", resp.StatusCode)
	}
	var out struct {
		Device listItem `json:"device"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if out.Device.Name == nil || *out.Device.Name != "Living-room PC" || !out.Device.Trusted {
		t.Fatalf("rename/trust not applied: %+v", out.Device)
	}
}

// TestRevokeCollectsLiveSessions verifies the store-level policy: Revoke returns the ids
// of the device's live sessions so the handler can end them.
func TestRevokeCollectsLiveSessions(t *testing.T) {
	pool := testDB(t)
	srv, svc := newServer(t, pool)
	mustRegister(t, svc, "sess@x.io", "sessuser", "quasar-fixture-pw-08")
	tok := loginHTTP(t, srv.URL, "sess@x.io", "quasar-fixture-pw-08", "sess-dev")
	dev := listDevices(t, srv.URL, tok)
	deviceID := dev[0].ID
	uid := userID(t, pool, "sess@x.io")

	ctx := context.Background()
	var appID string
	if err := pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('t') RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	var sessID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, app_id, state, width, height, fps, bitrate_kbps, device_id)
		VALUES ($1::uuid, $2::uuid, 'running', 1280, 720, 60, 8000, $3::uuid)
		RETURNING id::text`, uid, appID, deviceID).Scan(&sessID); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	store := NewStore(pool)
	ids, err := store.Revoke(ctx, uid, deviceID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if len(ids) != 1 || ids[0] != sessID {
		t.Fatalf("revoke live sessions: got %v want [%s]", ids, sessID)
	}
}

// userID resolves a user's id from email (test helper).
func userID(t *testing.T, pool *pgxpool.Pool, email string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id::text FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("userID(%s): %v", email, err)
	}
	return id
}

// TestRevokeStopsSessionsOnDetachedContext: the stopper gets (sid, "device_revoked") on a
// context that survives the request being cancelled (#489).
func TestRevokeStopsSessionsOnDetachedContext(t *testing.T) {
	pool := testDB(t)
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	type call struct{ sid, reason string }
	var calls []call
	var stopCtxErr error
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	stopper := func(ctx context.Context, sid, reason string) error {
		cancelReq() // the client disconnects before the stop runs
		calls = append(calls, call{sid, reason})
		stopCtxErr = ctx.Err()
		return nil
	}
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(NewStore(pool), stopper).Register(mux, authHandler.RequireAuth)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mustRegister(t, authSvc, "stop@x.io", "stopuser", "quasar-fixture-pw-09")
	tok := loginHTTP(t, srv.URL, "stop@x.io", "quasar-fixture-pw-09", "stop-dev")
	deviceID := listDevices(t, srv.URL, tok)[0].ID
	uid := userID(t, pool, "stop@x.io")
	sessID := seedSession(t, pool, uid, deviceID)

	req := httptest.NewRequest(http.MethodDelete, "/v1/me/devices/"+deviceID, nil).WithContext(reqCtx)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: got %d want 204 (%s)", rec.Code, rec.Body.String())
	}
	if len(calls) != 1 || calls[0] != (call{sessID, "device_revoked"}) {
		t.Fatalf("stopper calls: got %v want [{%s device_revoked}]", calls, sessID)
	}
	if reqCtx.Err() == nil {
		t.Fatal("request ctx was not cancelled; test proves nothing")
	}
	if stopCtxErr != nil {
		t.Fatalf("stopper ctx cancelled with the request: %v", stopCtxErr)
	}
}

// TestRevokeBlocksConcurrentSessionInsert: while Revoke holds the device row, a session
// insert for that device waits, then fails on the FK once the row is deleted (#489).
// The test pins Revoke mid-transaction by holding the device's token row, which Revoke
// updates after taking its device lock.
func TestRevokeBlocksConcurrentSessionInsert(t *testing.T) {
	pool := testDB(t)
	srv, svc := newServer(t, pool)
	mustRegister(t, svc, "race@x.io", "raceuser", "quasar-fixture-pw-10")
	tok := loginHTTP(t, srv.URL, "race@x.io", "quasar-fixture-pw-10", "race-dev")
	deviceID := listDevices(t, srv.URL, tok)[0].ID
	uid := userID(t, pool, "race@x.io")
	ctx := context.Background()
	var appID string
	if err := pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('t') RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	pin, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin pin: %v", err)
	}
	defer pin.Rollback(ctx) //nolint:errcheck
	if _, err := pin.Exec(ctx, `SELECT 1 FROM auth_tokens WHERE device_id = $1::uuid FOR UPDATE`, deviceID); err != nil {
		t.Fatalf("pin token row: %v", err)
	}

	revokeDone := make(chan error, 1)
	go func() {
		_, err := NewStore(pool).Revoke(ctx, uid, deviceID)
		revokeDone <- err
	}()
	waitLockWait(t, pool, "%UPDATE auth_tokens%") // Revoke now holds the device row

	insertDone := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `
			INSERT INTO sessions (user_id, app_id, state, width, height, fps, bitrate_kbps, device_id)
			VALUES ($1::uuid, $2::uuid, 'running', 1280, 720, 60, 8000, $3::uuid)`, uid, appID, deviceID)
		insertDone <- err
	}()
	waitLockWait(t, pool, "%INSERT INTO sessions%")
	select {
	case err := <-insertDone:
		t.Fatalf("insert did not wait on the device row: %v", err)
	default:
	}

	if err := pin.Rollback(ctx); err != nil {
		t.Fatalf("release pin: %v", err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatalf("revoke: %v", err)
	}
	var pgErr *pgconn.PgError
	if err := <-insertDone; !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("insert after revoke: got %v want FK violation 23503", err)
	}
}

// waitLockWait blocks until a backend running a query LIKE pattern is waiting on a lock.
func waitLockWait(t *testing.T, pool *pgxpool.Pool, pattern string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE $1`, pattern).Scan(&n); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no backend waiting on a lock for %q", pattern)
}

// seedSession inserts a running session bound to deviceID and returns its id.
func seedSession(t *testing.T, pool *pgxpool.Pool, uid, deviceID string) string {
	t.Helper()
	ctx := context.Background()
	var appID, sessID string
	if err := pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('t') RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, app_id, state, width, height, fps, bitrate_kbps, device_id)
		VALUES ($1::uuid, $2::uuid, 'running', 1280, 720, 60, 8000, $3::uuid)
		RETURNING id::text`, uid, appID, deviceID).Scan(&sessID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return sessID
}
