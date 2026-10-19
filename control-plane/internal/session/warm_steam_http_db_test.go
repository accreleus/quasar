package session

// warm_steam_http_db_test.go — the wire half of amendment 39: the heartbeat
// route, a parked session's 404s, the refused claim's Retry-After, and
// steam_dialogs on the profile preferences.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/storage"
)

type warmServer struct {
	*warmFixture
	srv   *httptest.Server
	token string // the fixture user's, bound to its device
	admin string
}

// newWarmServer serves the session routes over a warm fixture whose user signs
// in with a device-bound token, plus an admin.
func newWarmServer(t *testing.T) *warmServer {
	t.Helper()
	pool := testDB(t)
	ctx := context.Background()
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	s := seed(t, pool, 4)
	u, err := authSvc.Register(ctx, "warm@test.local", "warmuser", "password12345")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	tok, err := authSvc.LoginWithDevice(ctx, "warm@test.local", "password12345", "", "warm-device")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	adminTok, _ := registerUser(t, ctx, authSvc, "boss@test.local", "boss")
	must(t, exec(t, pool, `UPDATE users SET role = 'admin' WHERE username = 'boss'`))

	s.userID = u.ID
	steam := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	provisionHome(t, pool, u.ID, steam, s.hostID)
	f := &warmFixture{seedIDs: s, steam: steam, pool: pool, store: NewStore(pool), disp: newWarmDispatcher(true)}
	must(t, pool.QueryRow(ctx, `SELECT id::text FROM user_devices WHERE user_id = $1::uuid`, u.ID).Scan(&f.deviceID))
	clearHoldsAtEnd(t, pool)
	setWarm(t, pool, true, "library_open", 10)
	f.coord = newTestCoordinator(t, f.store, f.disp, testLogger(), WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))

	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(f.coord, f.store).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &warmServer{warmFixture: f, srv: srv, token: tok.Plaintext, admin: adminTok}
}

func (w *warmServer) do(t *testing.T, method, path, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, w.srv.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestSteamWarmRouteAlways204(t *testing.T) {
	w := newWarmServer(t)
	resp, body := w.do(t, http.MethodPut, "/v1/me/steam-warm", w.token, map[string]any{"ignored": true})
	if resp.StatusCode != http.StatusNoContent || body != nil {
		t.Fatalf("heartbeat = %d %v, want 204 with no body", resp.StatusCode, body)
	}
	if w.parkedID(t) == "" {
		t.Fatal("the heartbeat created no parked session")
	}
	// A heartbeat that can do nothing answers the same.
	setWarm(t, w.pool, false, "library_open", 10)
	if resp, _ := w.do(t, http.MethodPut, "/v1/me/steam-warm", w.admin, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("no-op heartbeat = %d, want 204", resp.StatusCode)
	}
	if resp, _ := w.do(t, http.MethodPut, "/v1/me/steam-warm", "not-a-token", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated heartbeat = %d, want 401", resp.StatusCode)
	}
}

func TestParkedSessionRoutesAre404ExceptAdminReadAndStop(t *testing.T) {
	w := newWarmServer(t)
	k := w.park(t)
	base := "/v1/sessions/" + k

	swap := map[string]any{"app_id": w.steam}
	display := map[string]any{"ui_scale": 1.5}
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, base, nil}, {http.MethodDelete, base, nil},
		{http.MethodPost, base + "/signaling-token", nil}, {http.MethodPost, base + "/swap", swap},
		{http.MethodPatch, base + "/display", display}, {http.MethodPost, base + "/stats", nil},
		{http.MethodPost, base + "/trace/events", nil}, {http.MethodPost, base + "/trace/clock", nil},
		{http.MethodGet, base + "/verdict", nil},
	} {
		resp, _ := w.do(t, r.method, r.path, w.token, r.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("owner %s %s = %d, want 404", r.method, r.path, resp.StatusCode)
		}
		if r.method == http.MethodGet && r.path == base || r.method == http.MethodDelete {
			continue
		}
		if resp, _ := w.do(t, r.method, r.path, w.admin, r.body); resp.StatusCode != http.StatusNotFound {
			t.Errorf("admin %s %s = %d, want 404", r.method, r.path, resp.StatusCode)
		}
	}
	resp, body := w.do(t, http.MethodGet, base, w.admin, nil)
	sess, _ := body["session"].(map[string]any)
	if resp.StatusCode != http.StatusOK || sess["kind"] != "parked" {
		t.Fatalf("admin GET = %d kind=%v, want 200 parked", resp.StatusCode, sess["kind"])
	}
	if resp, _ := w.do(t, http.MethodDelete, base, w.admin, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("admin DELETE = %d, want 202", resp.StatusCode)
	}
	resp, body = w.do(t, http.MethodGet, "/v1/sessions", w.token, nil)
	if items, _ := body["items"].([]any); resp.StatusCode != http.StatusOK || len(items) != 0 {
		t.Errorf("GET /v1/sessions = %d with %d items, want the parked session left out", resp.StatusCode, len(items))
	}
}

func TestRefusedClaimAnswersCapacityWithRetryAfter2(t *testing.T) {
	w := newWarmServer(t)
	k := w.park(t)
	must(t, exec(t, w.pool, `UPDATE sessions SET fps = 30 WHERE id = $1::uuid`, k))

	resp, body := w.do(t, http.MethodPost, "/v1/sessions", w.token, map[string]any{"app_id": w.steam, "mic": true})
	errObj, _ := body["error"].(map[string]any)
	if resp.StatusCode != http.StatusServiceUnavailable || errObj["code"] != "capacity_exhausted" {
		t.Fatalf("launch = %d %v, want 503 capacity_exhausted", resp.StatusCode, errObj)
	}
	if got := resp.Header.Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2", got)
	}
	if _, named := errObj["session_id"]; named {
		t.Error("the refusal names a session")
	}
	if got := readRow(t, w.pool, k); got.State != "stopping" {
		t.Fatalf("parked row %+v, want stopping for its stop window", got)
	}

	// The client's retry while that Steam is still stopping: the same 503, never a 409.
	resp, body = w.do(t, http.MethodPost, "/v1/sessions", w.token, map[string]any{"app_id": w.steam, "mic": true})
	errObj, _ = body["error"].(map[string]any)
	if resp.StatusCode != http.StatusServiceUnavailable || errObj["code"] != "capacity_exhausted" ||
		resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("retry while stopping = %d %v Retry-After=%q, want 503 capacity_exhausted with 2",
			resp.StatusCode, errObj, resp.Header.Get("Retry-After"))
	}

	// Once the parked session has stopped with its cleanup proof, the launch runs cold.
	w.coord.AgentState(context.Background(), w.hostID, agentws.SessionStateMsg{SessionID: k, State: "stopped",
		HomeCleanupQualified: true})
	resp, body = w.do(t, http.MethodPost, "/v1/sessions", w.token, map[string]any{"app_id": w.steam, "mic": true})
	sess, _ := body["session"].(map[string]any)
	if resp.StatusCode != http.StatusCreated || sess == nil {
		t.Fatalf("launch after the stop = %d %v, want 201", resp.StatusCode, body)
	}
	if a := w.disp.assignFor(t, sess["id"].(string)); a.AdoptSessionID != "" {
		t.Errorf("assign adopts %q, want a cold launch", a.AdoptSessionID)
	}
}

func TestProfilePreferencesSteamDialogs(t *testing.T) {
	w := newWarmServer(t)
	_, body := w.do(t, http.MethodGet, "/v1/me/profile-preferences", w.token, nil)
	if body["steam_dialogs"] != "show" {
		t.Fatalf("default steam_dialogs = %v, want show", body["steam_dialogs"])
	}
	if resp, body := w.do(t, http.MethodPatch, "/v1/me/profile-preferences", w.token,
		map[string]any{"default_profile_id": "720p60"}); resp.StatusCode != http.StatusOK || body["steam_dialogs"] != "show" {
		t.Fatalf("profile PATCH = %d %v", resp.StatusCode, body)
	}
	// Naming only steam_dialogs leaves the profile.
	resp, body := w.do(t, http.MethodPatch, "/v1/me/profile-preferences", w.token,
		map[string]any{"steam_dialogs": "dismiss_safe"})
	if resp.StatusCode != http.StatusOK || body["steam_dialogs"] != "dismiss_safe" || body["default_profile_id"] != "720p60" {
		t.Fatalf("dialogs PATCH = %d %v, want dismiss_safe with the profile kept", resp.StatusCode, body)
	}
	for _, bad := range []any{"always", nil, 3} {
		if resp, _ := w.do(t, http.MethodPatch, "/v1/me/profile-preferences", w.token,
			map[string]any{"steam_dialogs": bad}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("steam_dialogs %v = %d, want 400", bad, resp.StatusCode)
		}
	}
	_, body = w.do(t, http.MethodGet, "/v1/me/profile-preferences", w.token, nil)
	if body["default_profile_id"] != "720p60" || body["steam_dialogs"] != "dismiss_safe" {
		t.Fatalf("refused PATCHes changed the preferences: %v", body)
	}
	// Without steam_dialogs, today's rule: absent default_profile_id clears it; dialogs stay.
	_, body = w.do(t, http.MethodPatch, "/v1/me/profile-preferences", w.token, map[string]any{})
	if body["default_profile_id"] != nil || body["steam_dialogs"] != "dismiss_safe" {
		t.Errorf("empty PATCH = %v, want the profile cleared and dismiss_safe kept", body)
	}
}

// Both warm-Steam waits answer the web client's retryable 503 with Retry-After 2:
// a parked session still starting (left running), and a released held runtime
// whose cleanup proof is owed (then admitted once it arrives).
func TestWarmSteamWaitsAnswer503WithRetryAfter2(t *testing.T) {
	launch := func(w *warmServer) (*http.Response, map[string]any) {
		return w.do(t, http.MethodPost, "/v1/sessions", w.token, map[string]any{"app_id": w.steam, "mic": true})
	}
	want503 := func(t *testing.T, resp *http.Response, body map[string]any) {
		t.Helper()
		errObj, _ := body["error"].(map[string]any)
		if resp.StatusCode != http.StatusServiceUnavailable || errObj["code"] != "capacity_exhausted" ||
			resp.Header.Get("Retry-After") != "2" {
			t.Fatalf("launch = %d %v Retry-After=%q, want 503 capacity_exhausted with 2",
				resp.StatusCode, errObj, resp.Header.Get("Retry-After"))
		}
	}

	t.Run("warming parked session", func(t *testing.T) {
		w := newWarmServer(t)
		w.coord.SteamWarmHeartbeat(context.Background(), w.userID, false, w.deviceID)
		k := w.parkedID(t)
		w.disp.waitFor(t, "start")
		resp, body := launch(w)
		want503(t, resp, body)
		if got := readRow(t, w.pool, k); got.State != "assigned" || w.disp.stopReason(k) != "" {
			t.Errorf("parked row %+v stop=%q, want left to finish warming", got, w.disp.stopReason(k))
		}
	})

	t.Run("released held runtime", func(t *testing.T) {
		w := newWarmServer(t)
		play := w.runningTile(t)
		setWarm(t, w.pool, false, "library_open", 10)
		w.coord.AgentState(context.Background(), w.hostID, heldStop(play))
		resp, body := launch(w)
		want503(t, resp, body)
		w.coord.AgentState(context.Background(), w.hostID, agentws.SessionStateMsg{SessionID: play, State: "stopped",
			HomeCleanupQualified: true})
		if resp, body := launch(w); resp.StatusCode != http.StatusCreated {
			t.Fatalf("launch after the proof = %d %v, want 201", resp.StatusCode, body)
		}
	})
}
