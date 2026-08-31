package devices

// LP-SEC-01 SEC-04 (list / rename / trust) + SEC-05 (token↔device binding + revocation).

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

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
