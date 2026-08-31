package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestServer builds an httptest server with the auth routes wired, against
// the test database.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerV(t, "", "")
}

// newTestServerV builds the auth server with a P9-08 version policy applied
// (empty strings = permissive default).
func newTestServerV(t *testing.T, minClient, latestClient string) *httptest.Server {
	t.Helper()
	pool := testDB(t)
	svc := testService(t, pool)
	mux := http.NewServeMux()
	NewHandler(svc).WithVersionPolicy(minClient, latestClient).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url string, body any, bearer string) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return do(t, req)
}

func get(t *testing.T, url, bearer string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return do(t, req)
}

func do(t *testing.T, req *http.Request) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &parsed)
	}
	return resp, parsed
}

func TestHTTPAuthFlow(t *testing.T) {
	srv := newTestServer(t)

	reg := map[string]string{"email": "grace@example.com", "username": "grace", "password": "hopper-bug-1947"}

	// register → 201
	resp, body := post(t, srv.URL+"/v1/auth/register", reg, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status: want 201, got %d (%v)", resp.StatusCode, body)
	}
	if body["user"].(map[string]any)["email"] != "grace@example.com" {
		t.Fatalf("register body: %v", body)
	}

	// duplicate register → 409
	resp, _ = post(t, srv.URL+"/v1/auth/register", reg, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate register: want 409, got %d", resp.StatusCode)
	}

	// short password → 400
	resp, body = post(t, srv.URL+"/v1/auth/register",
		map[string]string{"email": "x@example.com", "username": "x", "password": "short"}, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("short password: want 400, got %d", resp.StatusCode)
	}
	if errCode(body) != "validation_failed" {
		t.Fatalf("short password code: %v", body)
	}

	// wrong password → 401 invalid_credentials
	resp, body = post(t, srv.URL+"/v1/auth/login",
		map[string]string{"email": "grace@example.com", "password": "wrong-password"}, "")
	if resp.StatusCode != http.StatusUnauthorized || errCode(body) != "invalid_credentials" {
		t.Fatalf("bad login: want 401 invalid_credentials, got %d %v", resp.StatusCode, body)
	}

	// login → 200 with access_token
	resp, body = post(t, srv.URL+"/v1/auth/login",
		map[string]string{"email": "grace@example.com", "password": "hopper-bug-1947"}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: want 200, got %d (%v)", resp.StatusCode, body)
	}
	token, _ := body["access_token"].(string)
	if token == "" {
		t.Fatalf("login: missing access_token in %v", body)
	}
	if body["token_type"] != "Bearer" {
		t.Fatalf("login: token_type %v", body["token_type"])
	}

	// /v1/me with no token → 401
	resp, _ = get(t, srv.URL+"/v1/me", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me no token: want 401, got %d", resp.StatusCode)
	}

	// /v1/me with token → 200
	resp, body = get(t, srv.URL+"/v1/me", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if body["user"].(map[string]any)["username"] != "grace" {
		t.Fatalf("me body: %v", body)
	}

	// logout → 204
	resp, _ = post(t, srv.URL+"/v1/auth/logout", nil, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: want 204, got %d", resp.StatusCode)
	}

	// token reuse after logout → 401 (the revoked token no longer authenticates)
	resp, _ = get(t, srv.URL+"/v1/me", token)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reuse after logout: want 401, got %d", resp.StatusCode)
	}

	// logout again with the same token → still 204 (idempotent)
	resp, _ = post(t, srv.URL+"/v1/auth/logout", nil, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("idempotent logout: want 204, got %d", resp.StatusCode)
	}
}

// TestHTTPChangePassword (CP-01) drives POST /v1/me/password end-to-end:
// 401 without a token, 400 on a weak new password, 401 on a wrong current
// password, 204 on a correct rotation (new password logs in / old does not),
// and ALL pre-change tokens are revoked (force re-login on every device).
func TestHTTPChangePassword(t *testing.T) {
	srv := newTestServer(t)

	reg := map[string]string{"email": "katherine@example.com", "username": "katherine", "password": "johnson-orbit-1"}
	if resp, body := post(t, srv.URL+"/v1/auth/register", reg, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: want 201, got %d (%v)", resp.StatusCode, body)
	}
	// Mint two tokens — one to drive the change, one to prove log-out-everywhere.
	loginAs := func() string {
		resp, body := post(t, srv.URL+"/v1/auth/login",
			map[string]string{"email": "katherine@example.com", "password": "johnson-orbit-1"}, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("login: want 200, got %d (%v)", resp.StatusCode, body)
		}
		tok, _ := body["access_token"].(string)
		if tok == "" {
			t.Fatalf("login: missing access_token in %v", body)
		}
		return tok
	}
	token1 := loginAs()
	token2 := loginAs()

	change := func(bearer, current, next string) (*http.Response, map[string]any) {
		return post(t, srv.URL+"/v1/me/password",
			map[string]string{"current_password": current, "new_password": next}, bearer)
	}

	// no token → 401
	if resp, _ := change("", "johnson-orbit-1", "orbit-mechanics-2"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", resp.StatusCode)
	}
	// weak new password → 400 validation_failed
	resp, body := change(token1, "johnson-orbit-1", "short")
	if resp.StatusCode != http.StatusBadRequest || errCode(body) != "validation_failed" {
		t.Fatalf("weak new: want 400 validation_failed, got %d %v", resp.StatusCode, body)
	}
	// wrong current password → 401 invalid_credentials
	resp, body = change(token1, "wrong-current", "orbit-mechanics-2")
	if resp.StatusCode != http.StatusUnauthorized || errCode(body) != "invalid_credentials" {
		t.Fatalf("wrong current: want 401 invalid_credentials, got %d %v", resp.StatusCode, body)
	}
	// correct rotation → 204
	if resp, body := change(token1, "johnson-orbit-1", "orbit-mechanics-2"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rotate: want 204, got %d (%v)", resp.StatusCode, body)
	}

	// new password logs in; old does not
	if resp, _ := post(t, srv.URL+"/v1/auth/login",
		map[string]string{"email": "katherine@example.com", "password": "orbit-mechanics-2"}, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("login with new password: want 200, got %d", resp.StatusCode)
	}
	if resp, _ := post(t, srv.URL+"/v1/auth/login",
		map[string]string{"email": "katherine@example.com", "password": "johnson-orbit-1"}, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login with old password: want 401, got %d", resp.StatusCode)
	}
	// Both pre-change tokens are revoked — every device must re-authenticate.
	if resp, _ := get(t, srv.URL+"/v1/me", token1); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token1 after change: want 401 (revoked), got %d", resp.StatusCode)
	}
	if resp, _ := get(t, srv.URL+"/v1/me", token2); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token2 after change: want 401 (revoked), got %d", resp.StatusCode)
	}
}

// TestHTTPVersionHandshake (P9-08 / #236) drives the login version gate against
// a real database with an operator floor of 1.2.0 and a latest of 1.5.0:
//   - a below-floor native client is hard-gated (426 client_too_old);
//   - an at-floor client proceeds and receives the advisory fields;
//   - a below-latest-but-above-floor client proceeds;
//   - a client sending no version (legacy/web) still logs in (additive baseline).
func TestHTTPVersionHandshake(t *testing.T) {
	srv := newTestServerV(t, "1.2.0", "1.5.0")

	reg := map[string]string{"email": "ada@example.com", "username": "ada", "password": "lovelace-note-g"}
	if resp, body := post(t, srv.URL+"/v1/auth/register", reg, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: want 201, got %d (%v)", resp.StatusCode, body)
	}

	login := func(extra map[string]string) (*http.Response, map[string]any) {
		body := map[string]string{"email": "ada@example.com", "password": "lovelace-note-g"}
		for k, v := range extra {
			body[k] = v
		}
		return post(t, srv.URL+"/v1/auth/login", body, "")
	}

	// below floor → 426 client_too_old, no token minted, advisory fields present
	// (a gated client never reaches 200, so the 426 must tell it what to update to)
	resp, body := login(map[string]string{"client_version": "1.1.9", "contract_version": "p9-01"})
	if resp.StatusCode != http.StatusUpgradeRequired || errCode(body) != "client_too_old" {
		t.Fatalf("below floor: want 426 client_too_old, got %d %v", resp.StatusCode, body)
	}
	if _, ok := body["access_token"]; ok {
		t.Fatalf("below floor: no token should be minted, got %v", body)
	}
	if body["min_client_version"] != "1.2.0" || body["latest_client_version"] != "1.5.0" {
		t.Fatalf("below floor: 426 must carry advisory fields, got %v", body)
	}

	// malformed non-empty version with a floor set → gated (cannot be proven current)
	resp, body = login(map[string]string{"client_version": "not-a-version"})
	if resp.StatusCode != http.StatusUpgradeRequired || errCode(body) != "client_too_old" {
		t.Fatalf("malformed version: want 426 client_too_old, got %d %v", resp.StatusCode, body)
	}

	// at floor → 200, advisory fields echoed
	resp, body = login(map[string]string{"client_version": "1.2.0", "contract_version": "p9-01"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("at floor: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if body["access_token"] == "" || body["access_token"] == nil {
		t.Fatalf("at floor: missing access_token in %v", body)
	}
	if body["min_client_version"] != "1.2.0" || body["latest_client_version"] != "1.5.0" {
		t.Fatalf("at floor: advisory fields wrong: %v", body)
	}

	// below latest but above floor → 200 (soft-warn is client-side; server does not block)
	resp, body = login(map[string]string{"client_version": "1.3.0"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("below latest: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if body["latest_client_version"] != "1.5.0" {
		t.Fatalf("below latest: advisory latest missing: %v", body)
	}

	// no version (legacy/web) → 200, additive baseline preserved
	resp, body = login(nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy no-version: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if body["access_token"] == "" || body["access_token"] == nil {
		t.Fatalf("legacy no-version: missing access_token in %v", body)
	}
}

// TestHTTPVersionHandshakePermissive confirms the permissive default: with no
// floor configured, a client at ANY version — including a would-be-below-floor
// one — logs in, and the advisory fields are omitted entirely.
func TestHTTPVersionHandshakePermissive(t *testing.T) {
	srv := newTestServer(t) // no version policy

	reg := map[string]string{"email": "edith@example.com", "username": "edith", "password": "clarke-antenna-1"}
	if resp, body := post(t, srv.URL+"/v1/auth/register", reg, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: want 201, got %d (%v)", resp.StatusCode, body)
	}

	resp, body := post(t, srv.URL+"/v1/auth/login",
		map[string]string{"email": "edith@example.com", "password": "clarke-antenna-1", "client_version": "0.0.1"}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("permissive: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if _, ok := body["min_client_version"]; ok {
		t.Fatalf("permissive: min_client_version should be omitted, got %v", body)
	}
	if _, ok := body["latest_client_version"]; ok {
		t.Fatalf("permissive: latest_client_version should be omitted, got %v", body)
	}
}

// TestHTTPVersionGateOnBearerEndpoint is #380: the floor is enforced on a
// bearer-authenticated endpoint (GET /v1/me, wired through the shared
// RequireAuth middleware), not just at login. A cached token no longer escapes a
// raised floor.
func TestHTTPVersionGateOnBearerEndpoint(t *testing.T) {
	srv := newTestServerV(t, "1.2.0", "1.5.0")

	reg := map[string]string{"email": "hedy@example.com", "username": "hedy", "password": "lamarr-hopping-41"}
	if resp, body := post(t, srv.URL+"/v1/auth/register", reg, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: want 201, got %d (%v)", resp.StatusCode, body)
	}
	// Log in WITHOUT a client_version — the token is minted exactly as a cached
	// pre-floor token would have been, which is the situation #380 exists for.
	resp, body := post(t, srv.URL+"/v1/auth/login",
		map[string]string{"email": "hedy@example.com", "password": "lamarr-hopping-41"}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: want 200, got %d (%v)", resp.StatusCode, body)
	}
	token, _ := body["access_token"].(string)
	if token == "" {
		t.Fatalf("login: missing access_token in %v", body)
	}

	me := func(version string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if version != "" {
			req.Header.Set(clientVersionHeader, version)
		}
		return do(t, req)
	}

	// no header (web / legacy client) → no gate
	if resp, body := me(""); resp.StatusCode != http.StatusOK {
		t.Fatalf("absent header: want 200, got %d (%v)", resp.StatusCode, body)
	}

	// at or above floor → proceeds
	for _, v := range []string{"1.2.0", "1.9.0", "v2.0.0"} {
		if resp, body := me(v); resp.StatusCode != http.StatusOK {
			t.Fatalf("header %q: want 200, got %d (%v)", v, resp.StatusCode, body)
		}
	}

	// below floor → 426 with the SAME body login's 426 carries
	resp, body = me("1.1.9")
	if resp.StatusCode != http.StatusUpgradeRequired || errCode(body) != "client_too_old" {
		t.Fatalf("below floor: want 426 client_too_old, got %d (%v)", resp.StatusCode, body)
	}
	if body["min_client_version"] != "1.2.0" || body["latest_client_version"] != "1.5.0" {
		t.Fatalf("below floor: 426 must carry the advisory fields, got %v", body)
	}
	if _, ok := body["user"]; ok {
		t.Fatalf("below floor: handler must not have run, got %v", body)
	}

	// malformed header → treated as ABSENT (a typo must not brick a signed-in
	// client on every call); deliberately unlike the login body gate
	if resp, body := me("not-a-version"); resp.StatusCode != http.StatusOK {
		t.Fatalf("malformed header: want 200 (treated as absent), got %d (%v)", resp.StatusCode, body)
	}

	// the gate is after authentication: a bad token is still 401, never a 426
	// that would let an unauthenticated caller probe the configured floor
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/me", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	req.Header.Set(clientVersionHeader, "0.0.1")
	if resp, body := do(t, req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token + old version: want 401, got %d (%v)", resp.StatusCode, body)
	}
}

// TestHTTPVersionGateOffByDefault: with no floor configured, the header is inert
// on bearer endpoints — the permissive default of the login handshake, extended.
func TestHTTPVersionGateOffByDefault(t *testing.T) {
	srv := newTestServer(t) // no version policy

	reg := map[string]string{"email": "katherine@example.com", "username": "katherine", "password": "johnson-orbit-62"}
	if resp, body := post(t, srv.URL+"/v1/auth/register", reg, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: want 201, got %d (%v)", resp.StatusCode, body)
	}
	resp, body := post(t, srv.URL+"/v1/auth/login",
		map[string]string{"email": "katherine@example.com", "password": "johnson-orbit-62"}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: want 200, got %d (%v)", resp.StatusCode, body)
	}
	token, _ := body["access_token"].(string)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(clientVersionHeader, "0.0.1")
	if resp, body := do(t, req); resp.StatusCode != http.StatusOK {
		t.Fatalf("no floor configured: want 200, got %d (%v)", resp.StatusCode, body)
	}
}

func errCode(body map[string]any) string {
	e, ok := body["error"].(map[string]any)
	if !ok {
		return ""
	}
	c, _ := e["code"].(string)
	return c
}
