package crud

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Truncate all tables (order matters for FK dependencies). user_app_favourites
	// (UI-P1) references both users and apps, so it must go before either.
	if _, err := pool.Exec(ctx, `
		DELETE FROM sessions;
		DELETE FROM user_app_favourites;
		DELETE FROM gpus;
		DELETE FROM hosts;
		DELETE FROM apps;
		DELETE FROM runtime_presets;
		DELETE FROM auth_tokens;
		DELETE FROM users;
	`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}

	t.Cleanup(pool.Close)
	return pool
}

func newTestServer(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, *auth.Service) {
	t.Helper()

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}

	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	crudHandler := NewHandler(pool)
	crudHandler.Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, authSvc
}

func post(t *testing.T, url string, body any, bearer string) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(http.MethodPost, url, &buf)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(raw) > 0 {
		json.Unmarshal(raw, &parsed)
	}
	return resp, parsed
}

func patch(t *testing.T, url string, body any, bearer string) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(http.MethodPatch, url, &buf)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(raw) > 0 {
		json.Unmarshal(raw, &parsed)
	}
	return resp, parsed
}

func getReq(t *testing.T, url, bearer string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(raw) > 0 {
		json.Unmarshal(raw, &parsed)
	}
	return resp, parsed
}

func TestAppsCRUD(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)

	ctx := context.Background()

	// Register admin and a regular user.
	_, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	_, err = authSvc.Register(ctx, "user@test.local", "user", "quasar-fixture-pw-05")
	if err != nil {
		t.Fatalf("register user: %v", err)
	}

	tok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "user@test.local", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	// POST /v1/apps (create) — runtime_spec must be returned in admin response.
	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":                 "test-app",
		"description":          "a test app",
		"default_width":        1280,
		"default_height":       720,
		"default_fps":          30,
		"default_bitrate_kbps": 5000,
		"default_vram_mb":      2048,
		"default_encode_slots": 1,
		"runtime_spec":         map[string]any{"image": "quasar-agent-dev:latest", "args": []string{"weston-terminal"}},
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appData, ok := body["app"]
	if !ok {
		t.Fatalf("create response missing 'app': %v", body)
	}
	app := appData.(map[string]any)
	appID := app["id"].(string)
	if app["name"] != "test-app" {
		t.Fatalf("create response name: %v", body)
	}
	if app["runtime_spec"] == nil {
		t.Fatalf("create response missing runtime_spec: %v", body)
	}

	// GET /v1/apps/{id} (admin read — includes runtime_spec).
	resp, body = getReq(t, srv.URL+"/v1/apps/"+appID, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get app: want 200, got %d", resp.StatusCode)
	}
	if body["app"].(map[string]any)["runtime_spec"] == nil {
		t.Fatalf("admin get missing runtime_spec: %v", body)
	}

	// GET /v1/apps/{id} (regular user — must NOT expose runtime_spec).
	resp, body = getReq(t, srv.URL+"/v1/apps/"+appID, userTok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user get app: want 200, got %d", resp.StatusCode)
	}
	if _, hasSpec := body["app"].(map[string]any)["runtime_spec"]; hasSpec {
		t.Fatalf("user get should not include runtime_spec: %v", body)
	}

	// GET /v1/apps (UI-P1: now requires auth — the list was public until 2026-07-27,
	// see the breaking-change amendment in control-api.md; anonymous access is covered
	// by TestListAppsRequiresAuth in favourites_test.go).
	resp, body = getReq(t, srv.URL+"/v1/apps", userTok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list apps: want 200, got %d (%v)", resp.StatusCode, body)
	}
	itemsData := body["items"]
	if itemsData == nil {
		t.Fatalf("list response: items is nil (%v)", body)
	}
	items := itemsData.([]any)
	if len(items) != 1 {
		t.Fatalf("list: want 1 app, got %d (%v)", len(items), body)
	}

	// PATCH /v1/apps/{id} (update) — response includes runtime_spec.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"description": "updated description",
		"enabled":     false,
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update app: want 200, got %d (%v)", resp.StatusCode, body)
	}
	updatedData, ok := body["app"]
	if !ok {
		t.Fatalf("update response missing 'app': %v", body)
	}
	updated := updatedData.(map[string]any)
	if updated["description"] != "updated description" || updated["enabled"] != false {
		t.Fatalf("update response fields: %v", body)
	}
	if updated["runtime_spec"] == nil {
		t.Fatalf("update response missing runtime_spec: %v", body)
	}

	// GET /v1/apps after disabling (disabled apps hidden from the library).
	resp, body = getReq(t, srv.URL+"/v1/apps", userTok.Plaintext)
	items = body["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("list after disable: want 0, got %d", len(items))
	}

	// Admin GET /v1/apps/{id} on a disabled app — admin can see it (200).
	resp, body = getReq(t, srv.URL+"/v1/apps/"+appID, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin get disabled app: want 200, got %d", resp.StatusCode)
	}
	if body["app"].(map[string]any)["enabled"] != false {
		t.Fatalf("admin get disabled app: expected enabled=false: %v", body)
	}

	// Non-admin GET /v1/apps/{id} on a disabled app — must 404.
	resp, _ = getReq(t, srv.URL+"/v1/apps/"+appID, userTok.Plaintext)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("user get disabled app: want 404, got %d", resp.StatusCode)
	}
}

func TestAppsCRUDPermissions(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)

	// Create a non-admin user.
	_, err := authSvc.Register(context.Background(), "user@test.local", "user", "unrelated-pw-01")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	tok, err := authSvc.Login(context.Background(), "user@test.local", "unrelated-pw-01", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// POST /v1/apps by non-admin should 403
	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "test-app",
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create as non-admin: want 403, got %d (%v)", resp.StatusCode, body)
	}
}

// uuidZero is a syntactically-valid UUID used where a path id is required but
// the resource need not exist: the admin gate (middleware) must reject a
// non-admin before any not-found lookup runs.
const uuidZero = "00000000-0000-0000-0000-000000000000"

// TestAdminEndpointsEnforceRole is the P1-Auth-Enforce acceptance: every admin
// endpoint returns 403 for a valid non-admin token and succeeds for an admin
// token. Enforcement is the server-side RequireAdmin middleware, not the client.
func TestAdminEndpointsEnforceRole(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	// A valid, authenticated non-admin token.
	if _, err := authSvc.Register(ctx, "user@test.local", "user", "unrelated-pw-01"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "user@test.local", "unrelated-pw-01", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	// An admin token (register + promote, as P1-3 expects until bootstrap).
	admin, err := authSvc.Register(ctx, "admin@test.local", "admin", "unrelated-pw-02")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id::text = $1`, admin.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	adminTok, err := authSvc.Login(ctx, "admin@test.local", "unrelated-pw-02", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}

	// A host to read on the host endpoints.
	var hostID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO hosts (node_name, status) VALUES ('enforce-host', 'online') RETURNING id::text
	`).Scan(&hostID); err != nil {
		t.Fatalf("insert host: %v", err)
	}

	deny := func(name string, resp *http.Response) {
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s as non-admin: want 403, got %d", name, resp.StatusCode)
		}
	}

	// --- deny: valid non-admin token is rejected on every admin endpoint ----
	r, _ := post(t, srv.URL+"/v1/apps", map[string]any{"name": "nope"}, userTok.Plaintext)
	deny("POST /v1/apps", r)
	r, _ = patch(t, srv.URL+"/v1/apps/"+uuidZero, map[string]any{"name": "nope"}, userTok.Plaintext)
	deny("PATCH /v1/apps/{id}", r)
	r, _ = getReq(t, srv.URL+"/v1/hosts", userTok.Plaintext)
	deny("GET /v1/hosts", r)
	r, _ = getReq(t, srv.URL+"/v1/hosts/"+hostID, userTok.Plaintext)
	deny("GET /v1/hosts/{id}", r)

	// --- allow: the admin token succeeds -----------------------------------
	r, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "ok-app"}, adminTok.Plaintext)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/apps as admin: want 201, got %d (%v)", r.StatusCode, body)
	}
	r, _ = getReq(t, srv.URL+"/v1/hosts", adminTok.Plaintext)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/hosts as admin: want 200, got %d", r.StatusCode)
	}
	r, _ = getReq(t, srv.URL+"/v1/hosts/"+hostID, adminTok.Plaintext)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/hosts/{id} as admin: want 200, got %d", r.StatusCode)
	}
}

func TestHostsCRUD(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)

	// Create and promote an admin.
	user, _ := authSvc.Register(context.Background(), "admin@test.local", "admin", "unrelated-pw-02")
	pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id::text = $1`, user.ID)
	tok, _ := authSvc.Login(context.Background(), "admin@test.local", "unrelated-pw-02", "")

	// Manually insert a host (P1-4 is the agent-API registration endpoint).
	var hostID string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO hosts (node_name, status)
		VALUES ($1, $2)
		RETURNING id::text
	`, "test-host", "online").Scan(&hostID)
	if err != nil {
		t.Fatalf("insert host: %v", err)
	}

	// GET /v1/hosts
	resp, body := getReq(t, srv.URL+"/v1/hosts", tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list hosts: want 200, got %d", resp.StatusCode)
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list: want 1 host, got %d", len(items))
	}

	// GET /v1/hosts/{id}
	resp, body = getReq(t, srv.URL+"/v1/hosts/"+hostID, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get host: want 200, got %d", resp.StatusCode)
	}
	h := body["host"].(map[string]any)
	if h["node_name"] != "test-host" || h["status"] != "online" {
		t.Fatalf("get response: %v", body)
	}
	// host-observability: storage is always serialized — null before any agent
	// reports (openapi.yaml Host.storage is required, JSON null until reported).
	if raw, ok := h["storage"]; !ok || raw != nil {
		t.Fatalf("get response storage: want present+null before any report, got %v (present=%v)", raw, ok)
	}

	// A capacity report populates storage; GET must reflect it verbatim.
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET storage = $2 WHERE id::text = $1`,
		hostID, `[{"label":"agent-data","path":"/var/lib/quasar-agent","total_mb":819200,"available_mb":512000}]`); err != nil {
		t.Fatalf("seed storage: %v", err)
	}
	resp, body = getReq(t, srv.URL+"/v1/hosts/"+hostID, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get host after storage seed: want 200, got %d", resp.StatusCode)
	}
	h = body["host"].(map[string]any)
	storage, ok := h["storage"].([]any)
	if !ok || len(storage) != 1 {
		t.Fatalf("get response storage after seed: want 1-item array, got %v", h["storage"])
	}
	vol := storage[0].(map[string]any)
	if vol["label"] != "agent-data" || vol["total_mb"] != float64(819200) {
		t.Fatalf("storage volume shape: %v", vol)
	}

	// host-observability-2: cpu_model is always serialized — null before any
	// agent reports (openapi.yaml Host.cpu_model is required).
	if raw, ok := h["cpu_model"]; !ok || raw != nil {
		t.Fatalf("get response cpu_model: want present+null before any report, got %v (present=%v)", raw, ok)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET cpu_model = $2 WHERE id::text = $1`,
		hostID, "AMD Ryzen 9 7950X 16-Core Processor"); err != nil {
		t.Fatalf("seed cpu_model: %v", err)
	}
	resp, body = getReq(t, srv.URL+"/v1/hosts/"+hostID, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get host after cpu_model seed: want 200, got %d", resp.StatusCode)
	}
	h = body["host"].(map[string]any)
	if h["cpu_model"] != "AMD Ryzen 9 7950X 16-Core Processor" {
		t.Fatalf("get response cpu_model after seed: got %v", h["cpu_model"])
	}

	// #429 follow-on (restart visibility): agent_restart_count is always
	// serialized (0 default), agent_connected_since/agent_last_restart_at are
	// null before the host's first connect ever populates them (this host was
	// inserted directly, bypassing agentws.enrollHost/reconnectHost).
	if h["agent_restart_count"] != float64(0) {
		t.Fatalf("get response agent_restart_count: want 0, got %v", h["agent_restart_count"])
	}
	if raw, ok := h["agent_connected_since"]; !ok || raw != nil {
		t.Fatalf("get response agent_connected_since: want present+null, got %v (present=%v)", raw, ok)
	}
	if raw, ok := h["agent_last_restart_at"]; !ok || raw != nil {
		t.Fatalf("get response agent_last_restart_at: want present+null, got %v (present=%v)", raw, ok)
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE hosts SET agent_process_started_at = now() - interval '2 hours',
		agent_restart_count = 3, agent_last_restart_at = now() - interval '10 minutes'
		WHERE id::text = $1
	`, hostID); err != nil {
		t.Fatalf("seed restart fields: %v", err)
	}
	resp, body = getReq(t, srv.URL+"/v1/hosts/"+hostID, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get host after restart fields seed: want 200, got %d", resp.StatusCode)
	}
	h = body["host"].(map[string]any)
	if h["agent_restart_count"] != float64(3) {
		t.Fatalf("get response agent_restart_count after seed: got %v", h["agent_restart_count"])
	}
	if h["agent_connected_since"] == nil {
		t.Fatal("get response agent_connected_since after seed: want non-null")
	}
	if h["agent_last_restart_at"] == nil {
		t.Fatal("get response agent_last_restart_at after seed: want non-null")
	}
	// listHosts must expose the same fields (separate query path from getHost).
	resp, body = getReq(t, srv.URL+"/v1/hosts", tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list hosts after restart fields seed: want 200, got %d", resp.StatusCode)
	}
	listed := body["items"].([]any)[0].(map[string]any)
	if listed["agent_restart_count"] != float64(3) {
		t.Fatalf("list response agent_restart_count: got %v", listed["agent_restart_count"])
	}

	// Non-admin cannot list hosts
	authSvc.Register(context.Background(), "user@test.local", "user", "unrelated-pw-01")
	tok2, _ := authSvc.Login(context.Background(), "user@test.local", "unrelated-pw-01", "")
	resp, _ = getReq(t, srv.URL+"/v1/hosts", tok2.Plaintext)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("list as non-admin: want 403, got %d", resp.StatusCode)
	}
}

// An app created without resource fields must inherit the SCHEMA defaults, not Go zero
// values. Regression for the live Tower data bug (2026-07-26): the create request struct
// took plain int32s, so an omitted `default_encode_slots` decoded to 0 and the app was
// admitted onto a GPU with no free encode slots — admission control silently bypassed.
func TestCreateAppResourceDefaults(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}

	// Only `name` — every resource default omitted (openapi AppWrite requires just name).
	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "defaults-app"}, tok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	app := body["app"].(map[string]any)
	for _, want := range []struct {
		field string
		value float64
	}{
		{"default_vram_mb", 1024},
		{"default_encode_slots", 1},
		{"default_width", 1920},
		{"default_height", 1080},
		{"default_fps", 60},
		{"default_bitrate_kbps", 15000},
	} {
		if got := app[want.field]; got != want.value {
			t.Fatalf("%s: want schema default %v, got %v", want.field, want.value, got)
		}
	}

	// An explicit zero is a client error, not a silently-admitted app.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "zero-slots-app", "default_encode_slots": 0,
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with zero encode slots: want 400, got %d (%v)", resp.StatusCode, body)
	}

	// Same rule on PATCH — an existing app cannot be zeroed either.
	appID := app["id"].(string)
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"default_encode_slots": 0,
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch to zero encode slots: want 400, got %d (%v)", resp.StatusCode, body)
	}

	// An explicit non-default value still wins.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{
		"name": "explicit-app", "default_encode_slots": 2, "default_vram_mb": 4096,
	}, tok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with explicit resources: want 201, got %d (%v)", resp.StatusCode, body)
	}
	app = body["app"].(map[string]any)
	if app["default_encode_slots"] != float64(2) || app["default_vram_mb"] != float64(4096) {
		t.Fatalf("explicit resources not honoured: %v", app)
	}
}

// TestPublicAppReadOmitsResourceDefaults (#397) — default_vram_mb and
// default_encode_slots are scheduler-internal. protocol/openapi.yaml declares
// them on AdminApp only (App / AppListItem do not), and control-api.md is
// explicit that resource defaults are not exposed to clients. This pins both
// directions: the public GET /v1/apps and GET /v1/apps/{id} responses must
// NOT carry either key, while the admin surfaces must keep carrying them —
// so a future "fix" cannot resolve the drift by deleting the fields
// everywhere and quietly breaking the admin app-catalogue UI.
func TestPublicAppReadOmitsResourceDefaults(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)

	ctx := context.Background()
	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, err := authSvc.Register(ctx, "user@test.local", "user", "quasar-fixture-pw-05"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	adminTok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "user@test.local", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":                 "resource-defaults-app",
		"default_vram_mb":      2048,
		"default_encode_slots": 2,
	}, adminTok.Plaintext)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	forbiddenKeys := []string{"default_vram_mb", "default_encode_slots"}

	// GET /v1/apps/{id} — non-admin arm must omit both fields.
	resp, body = getReq(t, srv.URL+"/v1/apps/"+appID, userTok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user get app: want 200, got %d (%v)", resp.StatusCode, body)
	}
	userApp := body["app"].(map[string]any)
	for _, k := range forbiddenKeys {
		if _, present := userApp[k]; present {
			t.Fatalf("user GET /v1/apps/{id} leaked %q: %v", k, userApp)
		}
	}

	// GET /v1/apps (list) — same requirement, every item.
	resp, body = getReq(t, srv.URL+"/v1/apps", userTok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list apps: want 200, got %d (%v)", resp.StatusCode, body)
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list: want 1 app, got %d (%v)", len(items), body)
	}
	listedApp := items[0].(map[string]any)
	for _, k := range forbiddenKeys {
		if _, present := listedApp[k]; present {
			t.Fatalf("user GET /v1/apps leaked %q: %v", k, listedApp)
		}
	}

	// Admin arm of GET /v1/apps/{id} must still carry both fields, unchanged.
	resp, body = getReq(t, srv.URL+"/v1/apps/"+appID, adminTok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin get app: want 200, got %d (%v)", resp.StatusCode, body)
	}
	adminApp := body["app"].(map[string]any)
	if adminApp["default_vram_mb"] != float64(2048) {
		t.Fatalf("admin get app: default_vram_mb missing/wrong: %v", adminApp)
	}
	if adminApp["default_encode_slots"] != float64(2) {
		t.Fatalf("admin get app: default_encode_slots missing/wrong: %v", adminApp)
	}

	// GET /v1/admin/apps — the admin app-catalogue list arm must also still
	// carry both fields; this is the endpoint the admin catalogue UI reads.
	resp, body = getReq(t, srv.URL+"/v1/admin/apps", adminTok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin list apps: want 200, got %d (%v)", resp.StatusCode, body)
	}
	adminItems := body["items"].([]any)
	if len(adminItems) != 1 {
		t.Fatalf("admin list: want 1 app, got %d (%v)", len(adminItems), body)
	}
	adminListedApp := adminItems[0].(map[string]any)
	if adminListedApp["default_vram_mb"] != float64(2048) {
		t.Fatalf("admin list apps: default_vram_mb missing/wrong: %v", adminListedApp)
	}
	if adminListedApp["default_encode_slots"] != float64(2) {
		t.Fatalf("admin list apps: default_encode_slots missing/wrong: %v", adminListedApp)
	}
}
